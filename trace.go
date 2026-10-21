package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Op is one file operation in the trace. The trace is saved as JSON Lines:
// one Op per line, in the order our filesystem handled them.
type Op struct {
	Seq      uint64 `json:"seq"`
	Op       string `json:"op"` // INIT_DIR, INIT_FILE, INIT_DATA, OPEN, CREATE, MKDIR, READ, WRITE, ACK, TRUNCATE, FALLOCATE, FSYNC, FDATASYNC, FSYNCDIR, FLUSH, RELEASE, RENAME, UNLINK, RMDIR
	Path     string `json:"path,omitempty"`
	Off      int64  `json:"off,omitempty"`
	Size     int64  `json:"size,omitempty"`    // READ: bytes requested; TRUNCATE: new size; FALLOCATE: length; INIT_FILE: file size
	Data     []byte `json:"-"`                 // WRITE, INIT_DATA: the bytes, kept in data.bin (see harness.store)
	DataOff  int64  `json:"dataOff,omitempty"` // where Data starts in data.bin
	DataLen  int64  `json:"dataLen,omitempty"` // how long Data is
	Zeros    bool   `json:"zeros,omitempty"`   // Data is DataLen zero bytes, so it isn't in data.bin
	Ack      string `json:"ack,omitempty"`     // ACK: what the program promised
	Flags    string `json:"flags,omitempty"`
	Err      string `json:"err,omitempty"`
	Inode    uint64 `json:"inode,omitempty"`
	ChildIno uint64 `json:"childIno,omitempty"`
	NewPath  string `json:"newPath,omitempty"`
}

// writeTrace saves ops to path, one JSON object per line.
func writeTrace(path string, ops []Op) error {
	f, err := os.Create(path) // truncates any older trace
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f) // Encode writes one object followed by "\n"
	for _, o := range ops {
		if err := enc.Encode(o); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// readTrace loads a trace saved by writeTrace, with each op's Data read back
// from the data.bin next to it.
func readTrace(path string) ([]Op, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ops []Op
	dec := json.NewDecoder(f)
	for {
		var o Op
		if err := dec.Decode(&o); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		ops = append(ops, o)
	}

	data, err := os.Open(dataPath(path))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if data != nil {
		defer data.Close()
	}
	for i := range ops {
		o := &ops[i]
		if o.DataLen == 0 {
			continue
		}
		o.Data = make([]byte, o.DataLen)
		if o.Zeros {
			continue
		}
		if data == nil {
			return nil, fmt.Errorf("op %d: its data is in %s, which is missing", o.Seq, dataPath(path))
		}
		if _, err := data.ReadAt(o.Data, o.DataOff); err != nil {
			return nil, fmt.Errorf("op %d: reading its data: %w", o.Seq, err)
		}
	}
	return ops, nil
}

// dataPath is the data.bin that goes with the trace at tracePath.
func dataPath(tracePath string) string {
	return filepath.Join(filepath.Dir(tracePath), "data.bin")
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// snapshot describes everything under dir as INIT_DIR and INIT_FILE ops, so a
// trace also holds the files that existed before the program ran. Paths are
// relative to dir, and parents come before their children.
func snapshot(dir string) ([]Op, error) {
	var ops []Op
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil // the root: the model always has it
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		o := Op{Path: rel, Inode: info.Sys().(*syscall.Stat_t).Ino}
		switch {
		case info.IsDir():
			o.Op = "INIT_DIR"
		case info.Mode().IsRegular():
			o.Op, o.Size = "INIT_FILE", info.Size()
			chunks, err := initData(path, rel, o.Inode)
			if err != nil {
				return err
			}
			ops = append(ops, o)
			ops = append(ops, chunks...)
			return nil
		default:
			return fmt.Errorf("%s: unsupported file type %v", path, info.Mode().Type())
		}
		ops = append(ops, o)
		return nil
	})
	return ops, err
}

// initChunk is how much of a file one INIT_DATA op holds.
const initChunk = 64 << 10

// initData returns INIT_DATA ops for the parts of the file at path that aren't
// all zeros. INIT_FILE gives the file its size, as zeros, so zero chunks (like
// space a database preallocated) take no room in the trace.
func initData(path string, rel string, ino uint64) ([]Op, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ops []Op
	for off := int64(0); ; {
		buf := make([]byte, initChunk)
		n, err := io.ReadFull(f, buf)
		if n > 0 && !isZero(buf[:n]) {
			ops = append(ops, Op{Op: "INIT_DATA", Path: rel, Inode: ino, Off: off, Data: buf[:n]})
		}
		off += int64(n)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return ops, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// errName is errno as text for Op.Err, or "" for success.
func errName(errno syscall.Errno) string {
	if errno == 0 {
		return ""
	}
	return errno.Error()
}

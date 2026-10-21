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
	Op       string `json:"op"` // INIT_DIR, INIT_FILE, OPEN, CREATE, MKDIR, READ, WRITE, ACK, TRUNCATE, FALLOCATE, FSYNC, FDATASYNC, FSYNCDIR, FLUSH, RELEASE, RENAME, UNLINK, RMDIR
	Path     string `json:"path,omitempty"`
	Off      int64  `json:"off,omitempty"`
	Size     int64  `json:"size,omitempty"` // READ: bytes requested; TRUNCATE: new size; FALLOCATE: length
	Data     []byte `json:"data,omitempty"` // full bytes written; JSON stores them as base64
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

// readTrace loads a trace saved by writeTrace.
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
			return ops, nil
		} else if err != nil {
			return nil, err
		}
		ops = append(ops, o)
	}
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
			o.Op = "INIT_FILE"
			if o.Data, err = os.ReadFile(path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported file type %v", path, info.Mode().Type())
		}
		ops = append(ops, o)
		return nil
	})
	return ops, err
}

// errName is errno as text for Op.Err, or "" for success.
func errName(errno syscall.Errno) string {
	if errno == 0 {
		return ""
	}
	return errno.Error()
}

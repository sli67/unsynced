package main

import (
	"encoding/json"
	"os"
	"syscall"
)

// Op is one file operation in the trace. The trace is saved as JSON Lines:
// one Op per line, in the order our filesystem handled them.
type Op struct {
	Seq      uint64 `json:"seq"`
	Op       string `json:"op"` // OPEN, CREATE, READ, WRITE, ACK, FSYNC, FDATASYNC, FLUSH, RELEASE, RENAME
	Path     string `json:"path,omitempty"`
	Off      int64  `json:"off,omitempty"`
	Size     int64  `json:"size,omitempty"` // READ: bytes requested
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

// errName is errno as text for Op.Err, or "" for success.
func errName(errno syscall.Errno) string {
	if errno == 0 {
		return ""
	}
	return errno.Error()
}

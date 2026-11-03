package main

import (
	"os"
	"sync"
	"syscall"
)

// harness is shared by every node in the filesystem. It numbers operations
// and keeps the trace.
//
// mu makes each operation one indivisible step (see apply). FUSE runs
// requests on many goroutines at once; without mu, an operation could be
// recorded in a different order than it took effect.
type harness struct {
	mu       sync.Mutex
	seq      uint64
	ops      []Op     // the trace, saved as JSON
	data     *os.File // data.bin: the bytes of WRITE and INIT_DATA ops, one after another
	dataSize int64    // how much is in data
}

// apply performs one operation as a single step: run effect (the operation's
// real work), then record the op in the trace.
func (h *harness) apply(o *Op, effect func(o *Op) syscall.Errno) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	errno := effect(o)
	h.seq++
	o.Seq, o.Err = h.seq, errName(errno)
	h.store(o)
	h.ops = append(h.ops, *o)
	return errno
}

// store moves o.Data into data.bin and records where it went, so the trace
// only holds its position. All-zero data isn't written, only its length.
func (h *harness) store(o *Op) {
	if o.Data == nil {
		return
	}
	o.DataLen = int64(len(o.Data))
	if isZero(o.Data) {
		o.Zeros = true
	} else {
		h.data.Write(o.Data)
		o.DataOff = h.dataSize
		h.dataSize += o.DataLen
	}
	o.Data = nil
}

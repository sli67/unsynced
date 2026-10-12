package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// recNode is one file or directory in our filesystem. It embeds go-fuse's
// LoopbackNode, which already knows how to forward every operation to
// backingDir. We override the operations we care about: writes go into the
// harness's page cache instead of to disk, and each operation is recorded
// in the trace through harness.apply.
type recNode struct {
	*fs.LoopbackNode
	h *harness
}

// The compiler checks that recNode has the methods go-fuse looks for.
var (
	_ fs.NodeWrapChilder = (*recNode)(nil)
	_ fs.NodeLookuper    = (*recNode)(nil)
	_ fs.NodeGetattrer   = (*recNode)(nil)
	_ fs.NodeOpener      = (*recNode)(nil)
	_ fs.NodeCreater     = (*recNode)(nil)
	_ fs.NodeReader      = (*recNode)(nil)
	_ fs.NodeWriter      = (*recNode)(nil)
	_ fs.NodeFlusher     = (*recNode)(nil)
	_ fs.NodeReleaser    = (*recNode)(nil)
	_ fs.NodeFsyncer     = (*recNode)(nil)
)

// WrapChild is called for every new file or directory the kernel asks about.
// Wrapping it in a recNode means operations on it get handled by us too, not
// just operations on the root directory.
func (n *recNode) WrapChild(ctx context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	return &recNode{LoopbackNode: ops.(*fs.LoopbackNode), h: n.h}
}

// name is the node's path relative to mountDir, e.g. "db".
func (n *recNode) name() string {
	return n.Path(nil)
}

// ino identifies the file in the page cache.
func (n *recNode) ino() uint64 {
	return n.StableAttr().Ino
}

// Lookup and Getattr report file sizes to the kernel. They must include
// unsynced writes, otherwise the program sees its file shrink, and an
// O_APPEND write lands at the wrong offset. They hold h.mu so they never see
// an fsync halfway through moving data from the page cache to disk.
func (n *recNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.h.mu.Lock()
	defer n.h.mu.Unlock()
	child, errno := n.LoopbackNode.Lookup(ctx, name, out)
	if errno == 0 {
		out.Size = max(out.Size, uint64(n.h.cache.end(child.StableAttr().Ino)))
	}
	return child, errno
}

func (n *recNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.h.mu.Lock()
	defer n.h.mu.Unlock()
	errno := n.LoopbackNode.Getattr(ctx, f, out)
	if errno == 0 {
		out.Size = max(out.Size, uint64(n.h.cache.end(n.ino())))
	}
	return errno
}

// Open: the program called open() on an existing file.
func (n *recNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	errno = n.h.apply(Op{Op: "OPEN", Path: n.name(), Flags: openFlags(flags)}, func() syscall.Errno {
		var errno syscall.Errno
		fh, fuseFlags, errno = n.LoopbackNode.Open(ctx, flags)
		return errno
	})
	return fh, fuseFlags, errno
}

// Create: the program called open() with O_CREAT on a file that didn't exist.
func (n *recNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	errno = n.h.apply(Op{Op: "CREATE", Path: filepath.Join(n.name(), name), Flags: openFlags(flags)}, func() syscall.Errno {
		var errno syscall.Errno
		inode, fh, fuseFlags, errno = n.LoopbackNode.Create(ctx, name, flags, mode, out)
		return errno
	})
	return inode, fh, fuseFlags, errno
}

// Read: the program called read(). It must see its own unsynced writes, so
// we read what's on disk and lay the page cache over it.
func (n *recNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n.h.mu.Lock()
	defer n.h.mu.Unlock()
	res, errno := f.(fs.FileReader).Read(ctx, dest, off)
	if errno != 0 {
		return nil, errno
	}
	onDisk, status := res.Bytes(dest)
	if !status.Ok() {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(n.h.cache.overlay(n.ino(), onDisk, off, len(dest))), 0
}

// Write: the program called write(). Like the real kernel, we only put the
// data in the page cache. It reaches backingDir at the next fsync. Writes to
// .ack are the program's promises: we record them but never store them.
func (n *recNode) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	o := Op{Op: "WRITE", Path: n.name(), Off: off, Data: bytes.Clone(data)}
	if o.Path == ".ack" {
		o.Op = "ACK"
	}
	errno := n.h.apply(o, func() syscall.Errno {
		if o.Op == "WRITE" {
			n.h.cache.add(n.ino(), off, data)
		}
		return 0
	})
	if errno != 0 {
		return 0, errno
	}
	return uint32(len(data)), 0
}

// Flush: the program called close(). This does NOT make data durable, so the
// page cache is left alone.
func (n *recNode) Flush(ctx context.Context, f fs.FileHandle) syscall.Errno {
	return n.h.apply(Op{Op: "FLUSH", Path: n.name()}, func() syscall.Errno {
		return f.(fs.FileFlusher).Flush(ctx)
	})
}

// Release: the last reference to the open file is gone (it is fully closed).
// The real descriptor is closed even after the power loss so it doesn't leak.
func (n *recNode) Release(ctx context.Context, f fs.FileHandle) syscall.Errno {
	errno := f.(fs.FileReleaser).Release(ctx)
	n.h.apply(Op{Op: "RELEASE", Path: n.name()}, func() syscall.Errno { return errno })
	return errno
}

// Fsync: the program called fsync() or fdatasync() (bit 0 of flags). Now the
// file's unsynced writes go to backingDir, so they survive a power loss.
func (n *recNode) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	op := "FSYNC"
	if flags&1 != 0 {
		op = "FDATASYNC"
	}
	return n.h.apply(Op{Op: op, Path: n.name()}, func() syscall.Errno {
		for _, w := range n.h.cache.take(n.ino()) {
			if _, errno := f.(fs.FileWriter).Write(ctx, w.data, w.off); errno != 0 {
				return errno
			}
		}
		if fsyncer, ok := f.(fs.FileFsyncer); ok {
			return fsyncer.Fsync(ctx, flags)
		}
		return syscall.ENOTSUP
	})
}

// openFlags turns open() flags into readable text, e.g. "O_WRONLY|O_APPEND".
func openFlags(flags uint32) string {
	var parts []string
	switch flags & syscall.O_ACCMODE {
	case syscall.O_RDONLY:
		parts = append(parts, "O_RDONLY")
	case syscall.O_WRONLY:
		parts = append(parts, "O_WRONLY")
	case syscall.O_RDWR:
		parts = append(parts, "O_RDWR")
	}
	for _, f := range []struct {
		bit  uint32
		name string
	}{
		{syscall.O_APPEND, "O_APPEND"},
		{syscall.O_CREAT, "O_CREAT"},
		{syscall.O_EXCL, "O_EXCL"},
		{syscall.O_TRUNC, "O_TRUNC"},
		{syscall.O_SYNC, "O_SYNC"},
		{syscall.O_DSYNC, "O_DSYNC"},
	} {
		if flags&f.bit == f.bit {
			parts = append(parts, f.name)
		}
	}
	return strings.Join(parts, "|")
}

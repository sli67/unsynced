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
// backingDir. We override the operations we care about so each one is
// recorded in the trace through harness.apply.
type recNode struct {
	*fs.LoopbackNode
	h *harness
}

// The compiler checks that recNode has the methods go-fuse looks for.
var (
	_ fs.NodeWrapChilder = (*recNode)(nil)
	_ fs.NodeOpener      = (*recNode)(nil)
	_ fs.NodeCreater     = (*recNode)(nil)
	_ fs.NodeReader      = (*recNode)(nil)
	_ fs.NodeWriter      = (*recNode)(nil)
	_ fs.NodeFlusher     = (*recNode)(nil)
	_ fs.NodeReleaser    = (*recNode)(nil)
	_ fs.NodeFsyncer     = (*recNode)(nil)
	_ fs.NodeRenamer     = (*recNode)(nil)
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

// ino is the file's inode number: ops with the same ino touch the same file.
func (n *recNode) ino() uint64 {
	return n.StableAttr().Ino
}

// Open: the program called open() on an existing file.
func (n *recNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	errno = n.h.apply(&Op{Op: "OPEN", Path: n.name(), Flags: openFlags(flags), Inode: n.ino()}, func(*Op) syscall.Errno {
		var errno syscall.Errno
		fh, fuseFlags, errno = n.LoopbackNode.Open(ctx, flags)
		return errno
	})
	return fh, fuseFlags, errno
}

// Create: the program called open() with O_CREAT on a file that didn't exist.
func (n *recNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {

	errno = n.h.apply(
		&Op{Op: "CREATE", Path: filepath.Join(n.name(), name), Flags: openFlags(flags), Inode: n.ino()}, func(o *Op) syscall.Errno {
			var errno syscall.Errno
			inode, fh, fuseFlags, errno = n.LoopbackNode.Create(ctx, name, flags, mode, out)
			o.ChildIno = inode.StableAttr().Ino
			return errno
		})
	return inode, fh, fuseFlags, errno
}

// Read: the program called read().
func (n *recNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (res fuse.ReadResult, errno syscall.Errno) {
	o := Op{Op: "READ", Path: n.name(), Off: off, Size: int64(len(dest)), Inode: n.ino()}
	errno = n.h.apply(&o, func(*Op) syscall.Errno {
		var errno syscall.Errno
		res, errno = f.(fs.FileReader).Read(ctx, dest, off)
		return errno
	})
	return res, errno
}

// Write: the program called write(). Writes to .ack are the program's
// promises: we record them but never store them.
func (n *recNode) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (written uint32, errno syscall.Errno) {
	o := Op{Op: "WRITE", Path: n.name(), Off: off, Data: bytes.Clone(data), Inode: n.ino()}
	if o.Path == ".ack" {
		o.Op = "ACK"
	}
	errno = n.h.apply(&o, func(*Op) syscall.Errno {
		if o.Op == "ACK" {
			written = uint32(len(data))
			return 0
		}
		var errno syscall.Errno
		written, errno = f.(fs.FileWriter).Write(ctx, data, off)
		return errno
	})
	return written, errno
}

// Flush: the program called close(). This does NOT make data durable.
func (n *recNode) Flush(ctx context.Context, f fs.FileHandle) syscall.Errno {
	return n.h.apply(&Op{Op: "FLUSH", Path: n.name(), Inode: n.ino()}, func(*Op) syscall.Errno {
		return f.(fs.FileFlusher).Flush(ctx)
	})
}

// Release: the last reference to the open file is gone (it is fully closed).
func (n *recNode) Release(ctx context.Context, f fs.FileHandle) syscall.Errno {
	return n.h.apply(&Op{Op: "RELEASE", Path: n.name(), Inode: n.ino()}, func(*Op) syscall.Errno {
		return f.(fs.FileReleaser).Release(ctx)
	})
}

// Fsync: the program called fsync() or fdatasync() (bit 0 of flags). Its
// writes so far would now survive a power loss.
func (n *recNode) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	op := "FSYNC"
	if flags&1 != 0 {
		op = "FDATASYNC"
	}
	return n.h.apply(&Op{Op: op, Path: n.name(), Inode: n.ino()}, func(*Op) syscall.Errno {
		if fsyncer, ok := f.(fs.FileFsyncer); ok {
			return fsyncer.Fsync(ctx, flags)
		}
		return syscall.ENOTSUP
	})
}

// Rename: the program called rename(). n is the directory holding the old
// name. Both paths and the moved file's inode are read before the real
// rename runs, because it removes the old name.
func (n *recNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	o := Op{
		Op:      "RENAME",
		Path:    filepath.Join(n.name(), name),
		NewPath: filepath.Join(newParent.EmbeddedInode().Path(nil), newName),
		Flags:   renameFlags(flags),
		Inode:   n.ino(),
	}
	if child := n.GetChild(name); child != nil {
		o.ChildIno = child.StableAttr().Ino
	}
	return n.h.apply(&o, func(*Op) syscall.Errno {
		return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
	})
}

// renameFlags turns renameat2() flags into readable text, e.g.
// "RENAME_NOREPLACE". Plain rename() has none.
func renameFlags(flags uint32) string {
	var parts []string
	for _, f := range []struct {
		bit  uint32
		name string
	}{
		{1, "RENAME_NOREPLACE"},
		{fs.RENAME_EXCHANGE, "RENAME_EXCHANGE"},
		{4, "RENAME_WHITEOUT"},
	} {
		if flags&f.bit != 0 {
			parts = append(parts, f.name)
		}
	}
	return strings.Join(parts, "|")
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

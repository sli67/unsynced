package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	FILE      = 0
	DIRECTORY = 1
)

type inode struct {
	kind      int
	vData     []byte
	dData     []byte
	vChildren map[string]uint64
	dChildren map[string]uint64
}

type fileSys struct {
	inode2Index map[uint64]int
	inodes      []*inode
}

// initFs returns an empty file system: just the root directory, which FUSE
// reports as inode 0.
func initFs() *fileSys {
	fs := fileSys{}
	fs.inode2Index = make(map[uint64]int)

	fs.inodes = append(fs.inodes, newDir())
	fs.inode2Index[0] = 0

	return &fs
}

// importDir loads everything under dir into the model as already durable:
// these files were on disk before the program started. Call it on backingDir
// after the -init copy and before mounting, so the inode numbers match the
// ones FUSE records. dir itself is the root (index 0).
func (fs *fileSys) importDir(dir string) error {
	return fs.importChildren(fs.inodes[0], dir)
}

func (fs *fileSys) importChildren(parent *inode, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			return err
		}
		ino := info.Sys().(*syscall.Stat_t).Ino
		parent.vChildren[e.Name()] = ino
		parent.dChildren[e.Name()] = ino
		if _, ok := fs.inode2Index[ino]; ok {
			continue // a hard link to a file we already loaded
		}

		var child *inode
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			child = &inode{kind: FILE, vData: data, dData: bytes.Clone(data)}
		case info.IsDir():
			child = newDir()
		default:
			return fmt.Errorf("%s: unsupported file type %v", path, info.Mode().Type())
		}
		fs.inode2Index[ino] = len(fs.inodes)
		fs.inodes = append(fs.inodes, child)

		if child.kind == DIRECTORY {
			if err := fs.importChildren(child, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (fs *fileSys) exportTo(ino uint64, currentDir string, childName string) error {
	fmt.Fprintf(os.Stdout, "%s cname\n", childName)
	if _, ok := fs.inode2Index[ino]; !ok {
		return fmt.Errorf("inode number %d does not exist", ino)
	}
	file := fs.inodes[fs.inode2Index[ino]]
	switch file.kind {
	case FILE:
		fd, err := os.Create(filepath.Join(currentDir, childName))
		if err != nil {
			return err
		}
		fd.Write(file.dData)
		fd.Close()
		return nil
	case DIRECTORY:
		if ino != 0 {
			if err := os.Mkdir(filepath.Join(currentDir, childName), 0755); err != nil {
				return err
			}
		}
		for cname, cino := range file.dChildren {
			if err := fs.exportTo(cino, filepath.Join(currentDir, childName), cname); err != nil {
				return err
			}
		}
	}
	return nil
}

func (fs *fileSys) simOperations(ops []Op) error {
	for _, op := range ops {
		var err error
		switch op.Op {
		case "CREATE":
			err = fs.create(op.Path, op.ChildIno)
		case "READ":
		case "WRITE":
			err = fs.write(op.Inode, op.Off, op.Data)
		case "RENAME":
			// RENAME_NOREPLACE only succeeds if NewPath didn't exist, which
			// makes it a plain rename. RENAME_EXCHANGE swaps two names.
			if op.Flags != "" && op.Flags != "RENAME_NOREPLACE" {
				err = fmt.Errorf("rename flags %s are not supported", op.Flags)
			} else {
				err = fs.rename(op.Path, op.NewPath)
			}
		case "FSYNC":
			fallthrough
		case "FDATASYNC":
			err = fs.fsync(op.Inode)
		}

		if err != nil {
			return err
		}
	}
	return nil
}

func newDir() *inode {
	nino := inode{}
	nino.kind = DIRECTORY
	nino.vChildren = make(map[string]uint64)
	nino.dChildren = make(map[string]uint64)
	return &nino
}

func (fs *fileSys) pathInode(path string) (*inode, error) {
	cur := fs.inodes[0]
	for _, name := range strings.Split(path, "/") {
		if name == "" || name == "." {
			continue
		}
		if cur.kind == FILE {
			return nil, fmt.Errorf("%s: invalid path", path)
		}
		ino, ok := cur.vChildren[name]
		if !ok {
			return nil, fmt.Errorf("%s: no such file or directory", path)
		}
		cur = fs.inodes[fs.inode2Index[ino]]
	}
	return cur, nil
}

func (fs *fileSys) write(ino uint64, off int64, data []byte) error {
	index, ok := fs.inode2Index[ino]
	if !ok {
		return fmt.Errorf("inode number %d does not exist", ino)
	}
	file := fs.inodes[index]
	oldlen := int64(len(file.vData))
	writeEnd := off + int64(len(data))
	if oldlen < writeEnd {
		file.vData = append(file.vData, make([]byte, writeEnd-oldlen)...)
	}
	copy(file.vData[off:], data)
	return nil
}

func (fs *fileSys) create(path string, ino uint64) error {
	_, ok := fs.inode2Index[ino]
	if ok {
		return fmt.Errorf("inode number %d already exists", ino)
	}
	parent, err := fs.pathInode(filepath.Dir(path))
	if err != nil {
		return err
	}
	if parent.kind != DIRECTORY {
		return fmt.Errorf("%s: parent is not a directory", path)
	}
	name := filepath.Base(path)
	if _, ok := parent.vChildren[name]; ok {
		return fmt.Errorf("%s: already exists", path)
	}
	fs.inode2Index[ino] = len(fs.inodes)
	fs.inodes = append(fs.inodes, &inode{kind: FILE})
	parent.vChildren[name] = ino
	return nil
}

// rename moves the name oldPath to newPath, replacing whatever newPath named.
// Like create, it only changes vChildren: each directory's change becomes
// durable when that directory is fsynced.
func (fs *fileSys) rename(oldPath string, newPath string) error {
	oldParent, err := fs.pathInode(filepath.Dir(oldPath))
	if err != nil {
		return err
	}
	oldName := filepath.Base(oldPath)
	ino, ok := oldParent.vChildren[oldName]
	if !ok {
		return fmt.Errorf("%s: no such file or directory", oldPath)
	}
	newParent, err := fs.pathInode(filepath.Dir(newPath))
	if err != nil {
		return err
	}
	if newParent.kind != DIRECTORY {
		return fmt.Errorf("%s: parent is not a directory", newPath)
	}
	newName := filepath.Base(newPath)
	file := fs.inodes[fs.inode2Index[ino]]
	if file.kind == DIRECTORY && strings.HasPrefix(newPath+"/", oldPath+"/") && newPath != oldPath {
		return fmt.Errorf("%s: cannot move a directory into itself", newPath)
	}
	if target, ok := newParent.vChildren[newName]; ok {
		if target == ino {
			return nil // both names already point to the same file: nothing to do
		}
		old := fs.inodes[fs.inode2Index[target]]
		if file.kind == DIRECTORY && old.kind != DIRECTORY {
			return fmt.Errorf("%s: not a directory", newPath)
		}
		if file.kind != DIRECTORY && old.kind == DIRECTORY {
			return fmt.Errorf("%s: is a directory", newPath)
		}
		if old.kind == DIRECTORY && len(old.vChildren) != 0 {
			return fmt.Errorf("%s: directory not empty", newPath)
		}
	}
	delete(oldParent.vChildren, oldName)
	newParent.vChildren[newName] = ino
	return nil
}

func (fs *fileSys) fsync(ino uint64) error {
	_, ok := fs.inode2Index[ino]
	if !ok {
		return fmt.Errorf("inode number %d does not exist", ino)
	}
	file := fs.inodes[fs.inode2Index[ino]]
	if !ok {
		return fmt.Errorf("%d: inode number does not exist", ino)
	}
	if file.kind == FILE {
		file.dData = append(file.dData[:0], file.vData...)
	} else if file.kind == DIRECTORY {
		file.dChildren = make(map[string]uint64, len(file.vChildren))
		for k, v := range file.vChildren {
			file.dChildren[k] = v
		}
	}
	return nil
}

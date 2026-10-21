package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	FILE      = 0
	DIRECTORY = 1
)

type CrashSpec struct {
	CrashAfter uint64   `json:"crashAfter"`
	PersistSeq []uint64 `json:"persistSeq"`
}

// readCrashSpec loads a CrashSpec saved as JSON, e.g.
// {"crashAfter": 12, "persistSeq": []}. Unknown fields are an error, so a
// typo like "crash_after" isn't silently read as crashAfter 0.
func readCrashSpec(path string) (CrashSpec, error) {
	var spec CrashSpec
	data, err := os.ReadFile(path)
	if err != nil {
		return spec, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

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
	nextRetired uint64 // next number for retire; far above real inode numbers
}

// initFs returns an empty file system: just the root directory, which FUSE
// reports as inode 0.
func initFs() *fileSys {
	fs := fileSys{}
	fs.inode2Index = make(map[uint64]int)

	fs.inodes = append(fs.inodes, newDir())
	fs.inode2Index[0] = 0
	fs.nextRetired = 1 << 63

	return &fs
}

// initEntry adds a file or directory that was on disk before the program
// started (an INIT op), so it is durable from the start.
func (fs *fileSys) initEntry(path string, ino uint64, kind int, data []byte) error {
	parent, err := fs.pathInode(filepath.Dir(path))
	if err != nil {
		return err
	}
	if parent.kind != DIRECTORY {
		return fmt.Errorf("%s: parent is not a directory", path)
	}
	name := filepath.Base(path)
	parent.vChildren[name] = ino
	parent.dChildren[name] = ino
	if _, ok := fs.inode2Index[ino]; ok {
		return nil // a hard link to a file we already have
	}
	child := &inode{kind: FILE, vData: data, dData: bytes.Clone(data)}
	if kind == DIRECTORY {
		child = newDir()
	}
	fs.inode2Index[ino] = len(fs.inodes)
	fs.inodes = append(fs.inodes, child)
	return nil
}

func (fs *fileSys) exportTo(ino uint64, currentDir string, childName string) error {
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

func (fs *fileSys) simOperations(ops []Op, spec CrashSpec) error {
	pers := make(map[uint64]bool)
	for _, seqnum := range spec.PersistSeq {
		pers[seqnum] = true
	}
	for _, op := range ops {
		if op.Seq > spec.CrashAfter {
			return nil
		}
		if op.Err != "" {
			continue // it failed, so it changed nothing on disk
		}

		var err error
		switch op.Op {
		case "INIT_DIR":
			err = fs.initEntry(op.Path, op.Inode, DIRECTORY, nil)
		case "INIT_FILE":
			err = fs.initEntry(op.Path, op.Inode, FILE, op.Data)
		case "CREATE":
			err = fs.create(op.Path, op.ChildIno, pers[op.Seq])
		case "MKDIR":
			err = fs.mkdir(op.Path, op.ChildIno, pers[op.Seq])
		case "OPEN":
			if strings.Contains(op.Flags, "O_TRUNC") {
				err = fs.truncate(op.Inode, 0, pers[op.Seq])
			}
		case "UNLINK":
			err = fs.unlink(op.Path, FILE, pers[op.Seq])
		case "RMDIR":
			err = fs.unlink(op.Path, DIRECTORY, pers[op.Seq])
		case "TRUNCATE":
			err = fs.truncate(op.Inode, op.Size, pers[op.Seq])
		case "FALLOCATE":
			err = fs.fallocate(op.Inode, op.Off, op.Size, op.Flags, pers[op.Seq])
		case "READ":
		case "WRITE":
			err = fs.write(op.Inode, op.Off, op.Data, pers[op.Seq])
		case "RENAME":
			// RENAME_NOREPLACE only succeeds if NewPath didn't exist, which
			// makes it a plain rename. RENAME_EXCHANGE swaps two names.
			if op.Flags != "" && op.Flags != "RENAME_NOREPLACE" {
				err = fmt.Errorf("rename flags %s are not supported", op.Flags)
			} else {
				err = fs.rename(op.Path, op.NewPath, pers[op.Seq])
			}
		case "FSYNC":
			fallthrough
		case "FDATASYNC":
			fallthrough
		case "FSYNCDIR":
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

func (fs *fileSys) write(ino uint64, off int64, data []byte, persist bool) error {
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
	if persist {
		st := off - off%PGSIZE
		end := min((writeEnd+PGSIZE-1)/PGSIZE*PGSIZE, int64(len(file.vData)))
		if int64(len(file.dData)) < end {
			file.dData = append(file.dData, make([]byte, end-int64(len(file.dData)))...)
		}
		copy(file.dData[st:end], file.vData[st:end])
	}
	return nil
}

func (fs *fileSys) create(path string, ino uint64, persist bool) error {
	return fs.newEntry(path, ino, &inode{kind: FILE}, persist)
}

func (fs *fileSys) mkdir(path string, ino uint64, persist bool) error {
	return fs.newEntry(path, ino, newDir(), persist)
}

// newEntry adds child, a new file or directory, at path with inode number ino.
func (fs *fileSys) newEntry(path string, ino uint64, child *inode, persist bool) error {
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
	if _, ok := fs.inode2Index[ino]; ok {
		fs.retire(ino)
	}
	fs.inode2Index[ino] = len(fs.inodes)
	fs.inodes = append(fs.inodes, child)
	parent.vChildren[name] = ino
	if persist {
		fs.fsyncdir(parent)
	}
	return nil
}

// retire gives the inode now known as ino an unused number, so ino can name
// a new file. The real file system reuses inode numbers after an unlink, but
// a durable directory may still point at the old file, and must keep finding
// the old file's data.
func (fs *fileSys) retire(ino uint64) {
	retired := fs.nextRetired
	fs.nextRetired++
	fs.inode2Index[retired] = fs.inode2Index[ino]
	delete(fs.inode2Index, ino)
	for _, dir := range fs.inodes {
		if dir.kind != DIRECTORY {
			continue
		}
		for _, children := range []map[string]uint64{dir.vChildren, dir.dChildren} {
			for name, c := range children {
				if c == ino {
					children[name] = retired
				}
			}
		}
	}
}

// unlink removes the name path (unlink() for a FILE, rmdir() for a
// DIRECTORY). The inode stays in the model: a durable directory may still
// name it, and an open fd may still write to it.
func (fs *fileSys) unlink(path string, kind int, persist bool) error {
	parent, err := fs.pathInode(filepath.Dir(path))
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	ino, ok := parent.vChildren[name]
	if !ok {
		return fmt.Errorf("%s: no such file or directory", path)
	}
	file := fs.inodes[fs.inode2Index[ino]]
	if kind == FILE && file.kind == DIRECTORY {
		return fmt.Errorf("%s: is a directory", path)
	}
	if kind == DIRECTORY {
		if file.kind != DIRECTORY {
			return fmt.Errorf("%s: not a directory", path)
		}
		if len(file.vChildren) != 0 {
			return fmt.Errorf("%s: directory not empty", path)
		}
	}
	delete(parent.vChildren, name)
	if persist {
		fs.fsyncdir(parent)
	}
	return nil
}

// truncate sets the file's size: cuts the end off, or grows it with zeros.
func (fs *fileSys) truncate(ino uint64, size int64, persist bool) error {
	index, ok := fs.inode2Index[ino]
	if !ok {
		return fmt.Errorf("inode number %d does not exist", ino)
	}
	file := fs.inodes[index]
	if file.kind != FILE {
		return fmt.Errorf("inode number %d: truncate of a directory", ino)
	}
	file.vData = resize(file.vData, size)
	if persist {
		file.dData = resize(file.dData, size)
	}
	return nil
}

// resize cuts b to size bytes, or grows it with zeros.
func resize(b []byte, size int64) []byte {
	if oldlen := int64(len(b)); size < oldlen {
		return b[:size]
	} else {
		return append(b, make([]byte, size-oldlen)...)
	}
}

// fallocate replays fallocate(): it reserves space, and grows the file with
// zeros if off+length is past the end, unless FALLOC_FL_KEEP_SIZE is set.
func (fs *fileSys) fallocate(ino uint64, off int64, length int64, flags string, persist bool) error {
	index, ok := fs.inode2Index[ino]
	if !ok {
		return fmt.Errorf("inode number %d does not exist", ino)
	}
	file := fs.inodes[index]
	switch flags {
	case "FALLOC_FL_KEEP_SIZE":
		return nil // only reserves space: nothing the model can see
	case "":
	default:
		return fmt.Errorf("fallocate flags %s are not supported", flags)
	}
	if end := off + length; end > int64(len(file.vData)) {
		file.vData = resize(file.vData, end)
		if persist {
			file.dData = resize(file.dData, max(end, int64(len(file.dData))))
		}
	}
	return nil
}

// rename moves the name oldPath to newPath, replacing whatever newPath named.
// Like create, it only changes vChildren: each directory's change becomes
// durable when that directory is fsynced.
func (fs *fileSys) rename(oldPath string, newPath string, persist bool) error {
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
	if persist {
		fs.fsyncdir(oldParent)
		if oldParent != newParent {
			fs.fsyncdir(newParent)
		}
	}
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
	switch file.kind {
	case FILE:
		file.dData = append(file.dData[:0], file.vData...)
	case DIRECTORY:
		fs.fsyncdir(file)
	}
	return nil
}

func (fs *fileSys) fsyncdir(file *inode) {
	file.dChildren = make(map[string]uint64, len(file.vChildren))
	for k, v := range file.vChildren {
		file.dChildren[k] = v
	}
}

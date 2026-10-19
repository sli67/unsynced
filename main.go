// Command unsynced runs a program on top of a FUSE filesystem and records
// every file operation it makes. The trace is later replayed into a model of
// the disk to work out what a power loss could leave behind.
//
// How FUSE works, in short:
//
//   - mountDir starts as an ordinary empty directory.
//   - fs.Mount attaches our filesystem to it. From then on, whenever any
//     program opens, writes or closes a file under mountDir, the kernel sends
//     that request to THIS program instead of to the disk.
//   - We handle each request ourselves (see recnode.go): pass it through to
//     the real files in backingDir, and record it in the trace.
//   - When we unmount, mountDir becomes an empty directory again.
//
// Usage: pass the program to test (and any arguments for it):
//
//	go build -o /tmp/unsynced/example ./examples
//	go run ./cmd/unsynced /tmp/unsynced/example
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const (
	// backingDir holds the real files behind mountDir.
	backingDir = "/tmp/unsynced/backing"

	// mountDir is where the FUSE filesystem appears. The example program
	// runs with this as its working directory, so its "db" means
	// mountDir/db, which we store as backingDir/db.
	mountDir = "/tmp/unsynced/mnt"

	// testDir holds the files of the tested program after it crashes, simulated by the our internal model
	crashDir = "/tmp/unsynced/crash"

	PGSIZE = 4096
)

type CrashSpec struct {
	CrashAfter int   `json:"crashAfter"`
	PersistSeq []int `json:"persistSeq"`
}

// harness is shared by every node in the filesystem. It numbers operations
// and keeps the trace.
//
// mu makes each operation one indivisible step (see apply). FUSE runs
// requests on many goroutines at once; without mu, an operation could be
// recorded in a different order than it took effect.
type harness struct {
	mu  sync.Mutex
	seq uint64
	ops []Op // the trace, saved as JSON
}

// apply performs one operation as a single step: run effect (the operation's
// real work), then record the op in the trace.
func (h *harness) apply(o *Op, effect func(o *Op) syscall.Errno) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	errno := effect(o)
	h.seq++
	o.Seq, o.Err = h.seq, errName(errno)
	h.ops = append(h.ops, *o)
	return errno
}

func main() {
	initDir := flag.String("init", "", "copy this directory into the filesystem before the program runs (default: start empty)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: unsynced [-init DIR] PROGRAM [ARGS...]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	//Record operations
	// run loads the disk as it was before the program started into fs, and
	// returns the ops it recorded, even when it also returns an error, e.g.
	// when the program crashed partway: its trace is still worth testing.
	fs := initFs()
	ops, err := run(fs, flag.Arg(0), flag.Args()[1:], *initDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
		if ops == nil {
			os.Exit(1)
		}
	}

	//Simulate Operations
	if err := fs.simOperations(ops); err != nil {
		fmt.Println(err)
	}
	if err := os.RemoveAll(crashDir); err != nil {
		fmt.Println(err)
	}
	if err := os.MkdirAll(crashDir, 0o755); err != nil {
		fmt.Println(err)
	}
	fs.fsync(0)
	if err := fs.exportTo(0, crashDir, ""); err != nil {
		fmt.Println(err)
	}
}

// run tests program (a compiled binary, run with args) on our filesystem.
// If initDir is not empty, its contents are on the filesystem when the
// program starts, and are loaded into fs before it runs. It returns every
// op recorded, even if it also returns an error.
func run(fs *fileSys, program string, args []string, initDir string) ([]Op, error) {
	// The example runs inside mountDir, so a relative path like ./example must
	// be turned into an absolute one first. LookPath also finds programs in
	// $PATH and checks that the file is executable.
	program, err := exec.LookPath(program)
	if err != nil {
		return nil, err
	}
	if program, err = filepath.Abs(program); err != nil {
		return nil, err
	}

	// Step 1: create both directories and an empty db.
	if err := os.RemoveAll(backingDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(backingDir, 0o755); err != nil {
		return nil, err
	}
	if initDir != "" {
		if err := os.CopyFS(backingDir, os.DirFS(initDir)); err != nil {
			return nil, fmt.Errorf("copy -init dir: %w", err)
		}
	}
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return nil, err
	}

	// Load what's on disk now into the model, before the program changes
	// anything. It must read backingDir, not initDir: the copies have their
	// own inode numbers, and those are the ones FUSE records.
	if err := fs.importDir(backingDir); err != nil {
		return nil, fmt.Errorf("import %s: %w", backingDir, err)
	}

	// Steps 2-5: run the example on our filesystem until it exits.
	h := &harness{}
	if err := record(h, program, args); err != nil {
		return h.ops, err
	}

	// Save the acks the example made, one per line.
	var acks bytes.Buffer
	for _, o := range h.ops {
		if o.Op == "ACK" {
			acks.Write(o.Data)
			if !bytes.HasSuffix(o.Data, []byte("\n")) {
				acks.WriteByte('\n')
			}
		}
	}
	if err := os.WriteFile("/tmp/unsynced/ack", acks.Bytes(), 0o644); err != nil {
		return h.ops, err
	}
	fmt.Println("Acks written to /tmp/unsynced/ack")

	if err := writeTrace("/tmp/unsynced/trace.jsonl", h.ops); err != nil {
		return h.ops, err
	}
	fmt.Println("Trace written to /tmp/unsynced/trace.jsonl")

	return h.ops, nil
}

func record(h *harness, program string, args []string) error {
	// Step 2: build the root of our filesystem: a loopback over backingDir,
	// wrapped in a recNode so its operations go through us.
	root, err := fs.NewLoopbackRoot(backingDir)
	if err != nil {
		return err
	}
	loop := root.(*fs.LoopbackNode)
	rootNode := &recNode{LoopbackNode: loop, h: h}

	// Step 3: mount it. From here until Unmount, file operations under
	// mountDir are handled by the recNode methods.
	server, err := fs.Mount(mountDir, rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{FsName: "unsynced", Name: "unsynced"},
	})
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	fmt.Printf("[unsynced] mounted %s (disk is %s)\n", mountDir, backingDir)

	// Step 5 (runs last, via defer): unmount, so mountDir is empty again.
	defer func() {
		server.Unmount()
		fmt.Printf("[unsynced] unmounted %s\n", mountDir)
	}()

	// If you press Ctrl+C, the example gets the signal and exits. Catching it
	// here keeps us alive long enough to unmount.
	signal.Notify(make(chan os.Signal, 1), os.Interrupt)

	// Step 4: run the example with mountDir as its working directory.
	cmd := exec.Command(program, args...)
	cmd.Dir = mountDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("example program: %w", err)
	}
	return nil
}

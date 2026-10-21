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
// Usage: record a program (and any arguments for it), then simulate:
//
//	go build -o /tmp/unsynced/bb ./examples/bb
//	go run . record /tmp/unsynced/bb
//	go run . simulate
package main

import (
	"flag"
	"fmt"
	"math"
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

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  unsynced record [-init DIR] [-out DIR] PROGRAM [ARGS...]   run PROGRAM on FUSE and save its trace
  unsynced simulate [-trace FILE] [-spec FILE] [-out DIR]    replay a trace in the model and write the crash state`)
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

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "record":
		recordCmd(os.Args[2:])
	case "simulate":
		if err := simulateCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

// recordCmd runs a program on our filesystem and saves its trace and acks.
// The trace is saved even if the program failed partway: it's still worth
// testing.
func recordCmd(args []string) {
	flags := flag.NewFlagSet("record", flag.ExitOnError)
	initDir := flags.String("init", "", "copy this directory into the filesystem before the program runs (default: start empty)")
	outDir := flags.String("out", "/tmp/unsynced", "where to save trace.jsonl, data.bin and ack")
	flags.Usage = usage
	flags.Parse(args)
	if flags.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
		os.Exit(1)
	}
	ackPath := filepath.Join(*outDir, "ack")
	tracePath := filepath.Join(*outDir, "trace.jsonl")

	ops, err := run(flags.Arg(0), flags.Args()[1:], *initDir, dataPath(tracePath))
	if ops != nil {
		if err := writeAcks(ackPath, ops, math.MaxUint64); err != nil {
			fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Acks written to", ackPath)

		if err := writeTrace(tracePath, ops); err != nil {
			fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Trace written to", tracePath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
		os.Exit(1)
	}
}

// simulateCmd replays a saved trace in our model of the disk and writes out
// what would survive a power loss after the last op. It doesn't use FUSE.
func simulateCmd(args []string) error {
	flags := flag.NewFlagSet("simulate", flag.ExitOnError)
	tracePath := flags.String("trace", "/tmp/unsynced/trace.jsonl", "trace to replay")
	outDir := flags.String("out", crashDir, "where to write the crash state")
	specPath := flags.String("spec", "", "CrashSpec JSON file (default: crash after the last op, no unsynced data survives)")
	flags.Usage = usage
	flags.Parse(args)

	ops, err := readTrace(*tracePath)
	if err != nil {
		return err
	}

	var spec CrashSpec
	if *specPath != "" {
		if spec, err = readCrashSpec(*specPath); err != nil {
			return err
		}
	} else {
		spec.CrashAfter = 0xFFFFFFFF
	}

	fs := initFs()
	if err := fs.simOperations(ops, spec); err != nil {
		return err
	}
	if err := os.RemoveAll(*outDir); err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	if err := fs.exportTo(0, *outDir, ""); err != nil {
		return err
	}
	fmt.Printf("Results written to directory %s\n", *outDir)

	// The program only promised what it acked before the power failed: a
	// checker must not expect anything acked later.
	ackPath := filepath.Clean(*outDir) + ".ack"
	if err := writeAcks(ackPath, ops, spec.CrashAfter); err != nil {
		return err
	}
	fmt.Println("Acks made before the crash written to", ackPath)
	return nil
}

// run tests program (a compiled binary, run with args) on our filesystem.
// If initDir is not empty, its contents are on the filesystem when the
// program starts. The trace starts with INIT ops describing those files, so
// it can be replayed later without them. It returns every op recorded, even
// if it also returns an error.
func run(program string, args []string, initDir string, dataPath string) ([]Op, error) {
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

	// Describe what's on disk now, before the program changes anything. It
	// must read backingDir, not initDir: the copies have their own inode
	// numbers, and those are the ones FUSE records.
	initOps, err := snapshot(backingDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", backingDir, err)
	}

	// Steps 2-5: run the example on our filesystem until it exits.
	// Written bytes go to dataPath as they happen, not into the trace.
	data, err := os.Create(dataPath)
	if err != nil {
		return nil, err
	}
	defer data.Close()
	h := &harness{data: data}
	for i := range initOps {
		h.store(&initOps[i])
	}
	h.ops = initOps
	if err := record(h, program, args); err != nil {
		return h.ops, err
	}
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

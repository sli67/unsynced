package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"

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
)

// recordCmd runs a program on our filesystem and saves its trace and acks.
// The trace is saved even if the program failed partway: it's still worth
// testing.
func recordCmd(args []string) {
	flags := flag.NewFlagSet("record", flag.ExitOnError)
	initDir := flags.String("init", "", "copy this directory into the filesystem before the program runs (default: start empty)")
	outDir := flags.String("out", "/tmp/unsynced", "where to save trace.jsonl, data.bin and ack")
	flags.Parse(args)
	if flags.NArg() < 1 {
		flags.Usage()
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

// run tests program (a compiled binary, run with args) on our filesystem.
// If initDir is not empty, its contents are on the filesystem when the
// program starts. The trace starts with INIT ops describing those files, so
// it can be replayed later without them. It returns every op recorded.
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
	record(h, program, args)
	return h.ops, nil
}

// record mounts the filesystem and runs the program on it. Failing to set up
// is a bug here and panics; the program failing is not, so the trace is still
// saved.
func record(h *harness, program string, args []string) {
	// Step 2: build the root of our filesystem: a loopback over backingDir,
	// wrapped in a recNode so its operations go through us.
	root, err := fs.NewLoopbackRoot(backingDir)
	if err != nil {
		panic(err)
	}
	loop := root.(*fs.LoopbackNode)
	rootNode := &recNode{LoopbackNode: loop, h: h}

	// Step 3: mount it. From here until Unmount, file operations under
	// mountDir are handled by the recNode methods.
	server, err := fs.Mount(mountDir, rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{FsName: "unsynced", Name: "unsynced"},
	})
	if err != nil {
		panic(fmt.Errorf("mount: %w", err))
	}
	fmt.Printf("[unsynced] mounted %s (disk is %s)\n", mountDir, backingDir)

	// Step 5 (runs last, via defer): unmount, so mountDir is empty again.
	defer func() {
		server.Unmount()
		os.Remove(mountDir)
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
		fmt.Fprintf(os.Stderr, "unsynced: example program: %v\n", err)
	}
}

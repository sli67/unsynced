// Command unsynced runs a program on top of a FUSE filesystem that behaves
// like a disk with a page cache, simulates a power loss, and then checks
// whether the program's data survived.
//
// How FUSE works, in short:
//
//   - mountDir starts as an ordinary empty directory.
//   - fs.Mount attaches our filesystem to it. From then on, whenever any
//     program opens, writes or closes a file under mountDir, the kernel sends
//     that request to THIS program instead of to the disk.
//   - We handle each request ourselves (see recnode.go). Writes are held in
//     memory (pagecache.go) and only reach the real files in backingDir when
//     the program calls fsync, the same rule a real disk follows.
//   - When we unmount, mountDir becomes an empty directory again.
//
// Usage: pass the program to test (and any arguments for it):
//
//	go build -o /tmp/unsynced/example ./examples
//	go run ./cmd/unsynced /tmp/unsynced/example
package main

import (
	"bytes"
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
	// backingDir is our "disk": it holds only data that was fsynced.
	backingDir = "/tmp/unsynced/backing"

	// mountDir is where the FUSE filesystem appears. The example program
	// runs with this as its working directory, so its "db" means
	// mountDir/db, which we store as backingDir/db.
	mountDir = "/tmp/unsynced/mnt"

	// crashAfter is the operation after which the power goes out.
	crashAfter = "FLUSH"

	// expectedDB is what the example believes db holds once it has run on an
	// empty db: it appended "x", closed the file, and got no errors.
)

// harness is shared by every node in the filesystem. It numbers operations,
// owns the page cache, and pulls the plug at the crash point.
//
// mu makes each operation one indivisible step (see apply). FUSE runs
// requests on many goroutines at once; without mu, an operation could be
// recorded in a different order than it took effect, or touch the disk
// after the power loss.
type harness struct {
	mu      sync.Mutex
	seq     uint64
	cache   *pageCache
	crashed chan struct{} // closed at the moment of power loss
	ops     []Op          // the trace, saved as JSON
}

func newHarness() *harness {
	return &harness{cache: newPageCache(), crashed: make(chan struct{})}
}

// apply performs one operation as a single step: check the power is on, run
// effect (the operation's real work), record the op in the trace, and cut the
// power if this is the crash point. Once the power is off, nothing runs and
// every operation fails with EIO.
func (h *harness) apply(o Op, effect func() syscall.Errno) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.isCrashed() {
		return syscall.EIO
	}
	errno := effect()
	h.seq++
	o.Seq, o.Err = h.seq, errName(errno)
	h.ops = append(h.ops, o)
	if o.Op == crashAfter {
		h.powerLossLocked("after " + o.Op)
	}
	return errno
}

func (h *harness) isCrashed() bool {
	select {
	case <-h.crashed:
		return true
	default:
		return false
	}
}

// powerLoss simulates pulling the plug: everything still in the page cache
// is gone. Calls after the first have no effect.
func (h *harness) powerLoss(when string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.powerLossLocked(when)
}

// powerLossLocked is powerLoss for callers that already hold h.mu.
func (h *harness) powerLossLocked(when string) {
	if h.isCrashed() {
		return
	}
	writes, lost := h.cache.dropAll()
	fmt.Printf("[unsynced] *** POWER LOSS %s: %d unsynced write(s), %d byte(s) lost ***\n", when, writes, lost)
	close(h.crashed)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: unsynced PROGRAM [ARGS...]")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
		os.Exit(1)
	}
}

// run tests program (a compiled binary, run with args) on our filesystem.
func run(program string, args []string) error {
	// The example runs inside mountDir, so a relative path like ./example must
	// be turned into an absolute one first. LookPath also finds programs in
	// $PATH and checks that the file is executable.
	program, err := exec.LookPath(program)
	if err != nil {
		return err
	}
	if program, err = filepath.Abs(program); err != nil {
		return err
	}

	// Step 1: create both directories and an empty db.
	if err := os.MkdirAll(backingDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(backingDir, "db"), nil, 0o644); err != nil {
		return err
	}
	// Steps 2-5: run the example on our filesystem until the power goes out.
	h := newHarness()
	if err := runUntilCrash(h, program, args); err != nil {
		return err
	}

	// Save only the acks the example made before the power loss, one per line.
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
		return err
	}
	fmt.Println("Acks written to /tmp/unsynced/ack")

	if err := writeTrace("/tmp/unsynced/trace.jsonl", h.ops); err != nil {
		return err
	}
	fmt.Println("Trace written to /tmp/unsynced/trace.jsonl")

	return nil
}

func runUntilCrash(h *harness, program string, args []string) error {
	// Step 2: build the root of our filesystem: a loopback over backingDir,
	// wrapped in a recNode so its operations go through us.
	root, err := fs.NewLoopbackRoot(backingDir)
	if err != nil {
		return err
	}
	loop := root.(*fs.LoopbackNode)
	rootNode := &recNode{LoopbackNode: loop, h: h}
	loop.RootData.RootNode = rootNode

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

	// Step 4: run the example with mountDir as its working directory, and
	// kill it the moment the power goes out.
	cmd := exec.Command(program, args...)
	cmd.Dir = mountDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case <-h.crashed:
		cmd.Process.Kill()
		<-exited
	case err := <-exited:
		if err != nil {
			return fmt.Errorf("example program: %w", err)
		}
		// The example finished without reaching the crash point. Cut the
		// power now anyway: anything it never fsynced is still at risk.
		h.powerLoss("after the example exited")
	}
	return nil
}

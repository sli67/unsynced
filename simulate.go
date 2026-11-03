package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// crashDir holds the files of the tested program after it crashes, simulated by our internal model.
const crashDir = "/tmp/unsynced/crash"

const verifyDir = "/tmp/unsynced/verify"

// simulateCmd replays a saved trace in our model of the disk and writes out
// what would survive a power loss after the last op. It doesn't use FUSE.
func simulateCmd(args []string) error {
	flags := flag.NewFlagSet("simulate", flag.ExitOnError)
	tracePath := flags.String("trace", "/tmp/unsynced/trace.jsonl", "trace to replay")
	outDir := flags.String("out", crashDir, "where to write the crash state")
	specPath := flags.String("spec", "", "CrashSpec JSON file (default: crash after the last op, no unsynced data survives)")
	ext4 := flags.Bool("ext4", true, "name and size changes made before the crash persist, as an ext4 journal commit would")
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
	if *ext4 {
		// Name and size changes persist up to the last sync before the crash:
		// a sync commits the journal, which holds every change made before it.
		pending := []uint64{}
		for _, op := range ops {
			if op.Seq > spec.CrashAfter {
				break
			}
			if isMeta(op) {
				pending = append(pending, op.Seq)
			}
			if isSync(op) {
				spec.PersistSeq = append(spec.PersistSeq, pending...)
				pending = pending[:0]
			}
		}
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

func verifyCmd(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	tracePath := flags.String("trace", "/tmp/unsynced/trace.jsonl", "trace to replay")
	checkerPath := flags.String("checker", "", "checker path")
	ext4 := flags.Bool("ext4", true, "name and size changes made before the crash persist, as an ext4 journal commit would")
	flags.Parse(args)
	if *checkerPath == "" {
		flags.Usage()
		os.Exit(2)
	}

	ops, err := readTrace(*tracePath)
	if err != nil {
		return err
	}
	// LookPath fails if checkerPath is empty, missing, a directory, or not
	// executable. A path with no "/" is searched for in $PATH, not here.
	checker, err := exec.LookPath(*checkerPath)
	if err != nil {
		return err
	}

	fails := []Op{}
	checkerErrs := 0
	persist := []uint64{}
	pendingPers := []uint64{}
	lastAck := false
	fmt.Printf("Crash points failed:\n")
	for _, op := range ops {
		if *ext4 && isMeta(op) {
			pendingPers = append(pendingPers, op.Seq)
		}
		if op.Op == "ACK" {
			lastAck = true
		} else if isReadOnly(op) {
			// Changes no file, so it doesn't end a stream of acks: the crash
			// point is the first op after the stream that does.
		} else if lastAck || isSync(op) {
			if *ext4 && isSync(op) {
				persist = append(persist, pendingPers...)
				pendingPers = pendingPers[:0]
			}
			lastAck = false
			fs := initFs()
			if err := fs.simOperations(ops, CrashSpec{CrashAfter: op.Seq, PersistSeq: persist}); err != nil {
				return err
			}
			if err := os.RemoveAll(verifyDir); err != nil {
				return err
			}
			if err := os.MkdirAll(verifyDir, 0o755); err != nil {
				return err
			}
			if err := fs.exportTo(0, verifyDir, ""); err != nil {
				return err
			}
			if err := writeAcks(verifyDir+".ack", ops, op.Seq); err != nil {
				return err
			}
			cmd := exec.Command(checker, verifyDir, verifyDir+".ack")
			// Exit 1 is a bug. Any other failure means the checker itself
			// broke, so it says nothing about this crash point.
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			switch {
			case err == nil:
			case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
				fails = append(fails, op)
				fmt.Printf("%d %s %s\n", op.Seq, op.Op, op.Path)
			default:
				checkerErrs++
				fmt.Printf("%d %s %s: checker error: %v\n%s", op.Seq, op.Op, op.Path, err, out)
			}
		}
	}
	if checkerErrs > 0 {
		fmt.Printf("%d crash points not checked: the checker failed\n", checkerErrs)
	}
	if len(fails) == 0 && checkerErrs == 0 {
		fmt.Printf("All crash points succeeded\n")
	}
	return nil
}

// isMeta reports whether op is a successful name or size change: what an
// ext4 journal commit makes durable.
func isMeta(op Op) bool {
	switch op.Op {
	case "CREATE", "MKDIR", "RENAME", "UNLINK", "RMDIR", "TRUNCATE", "FALLOCATE":
		return op.Err == ""
	}
	return false
}

func isSync(op Op) bool {
	switch op.Op {
	case "FSYNC", "FDATASYNC", "FSYNCDIR":
		return op.Err == ""
	}
	return false
}

// isReadOnly reports whether op leaves every file as it was.
func isReadOnly(op Op) bool {
	switch op.Op {
	case "READ", "FLUSH", "RELEASE":
		return true
	case "OPEN":
		return !strings.Contains(op.Flags, "O_TRUNC")
	}
	return false
}

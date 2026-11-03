package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func verifyCmd(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	tracePath := flags.String("trace", "/tmp/unsynced/trace.jsonl", "trace to replay")
	checkerPath := flags.String("checker", "", "checker path")
	ext4 := flags.Bool("ext4", false, "name and size changes made before the crash persist, as an ext4 journal commit would")
	flags.Parse(args)

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
	persist := []uint64{}
	lastAck := false
	for _, op := range ops {
		switch op.Op {
		case "CREATE", "MKDIR", "RENAME", "UNLINK", "RMDIR", "TRUNCATE", "FALLOCATE":
			if *ext4 && op.Err == "" {
				persist = append(persist, op.Seq)
			}
		}
		if op.Op == "ACK" {
			lastAck = true
		} else if lastAck || op.Op == "FSYNC" || op.Op == "FDATASYNC" || op.Op == "FSYNCDIR" {
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
			if _, err := cmd.CombinedOutput(); err != nil {
				fails = append(fails, op)
			}
		}
	}
	if len(fails) != 0 {
		fmt.Printf("Crash points failed:\n")
		for _, op := range fails {
			fmt.Printf("%d %s %s\n", op.Seq, op.Op, op.Path)
		}
	} else {
		fmt.Printf("All crash points succeeded\n")
	}
	return nil
}

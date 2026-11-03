// Usage: record a program (and any arguments for it), then simulate:
//
//	go build -o /tmp/unsynced/bb ./examples/bb
//	go run . record /tmp/unsynced/bb
//	go run . simulate
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	flag.Parse()
	switch flag.Arg(0) {
	case "record":
		recordCmd(flag.Args()[1:])
	case "simulate":
		if err := simulateCmd(flag.Args()[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
			os.Exit(1)
		}
	case "verify":
		if err := verifyCmd(flag.Args()[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "unsynced: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: unsynced record|simulate|verify [flags]")
		os.Exit(2)
	}
}

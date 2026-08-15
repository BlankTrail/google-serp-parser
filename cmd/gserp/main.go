// SPDX-License-Identifier: MIT

// Command gserp is the Google SERP parser: a web server, a CLI, and — on this
// milestone — a preflight check against a running BlankTrail Proxy.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/blanktrail/google-serp-parser/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gserp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "doctor":
		return doctorCommand(args[1:])
	case "version", "--version", "-v":
		fmt.Println("gserp", version.Version())
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func doctorCommand(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var opts doctorOptions
	fs.StringVar(&opts.ControlURL, "bt-url", "http://127.0.0.1:8891", "BlankTrail control API base URL")
	fs.StringVar(&opts.APIKey, "bt-key", os.Getenv("BLANKTRAIL_API_KEY"), "BlankTrail API key")
	fs.IntVar(&opts.Threads, "threads", 2, "parsing threads the run will use")
	fs.IntVar(&opts.PortsPerThread, "ports-per-thread", 3, "BlankTrail ports per thread")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runDoctor(context.Background(), os.Stdout, opts)
}

func usage() {
	fmt.Println(`gserp — open-source Google SERP parser, powered by BlankTrail Proxy

Usage:
  gserp doctor [flags]   check a BlankTrail instance against an intended run
  gserp version          print the version

Doctor flags:
  --bt-url string             control API base URL (default http://127.0.0.1:8891)
  --bt-key string             API key (or set BLANKTRAIL_API_KEY)
  --threads int               parsing threads (default 2)
  --ports-per-thread int      ports per thread (default 3)`)
}

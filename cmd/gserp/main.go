// SPDX-License-Identifier: MIT

// Command gserp is the Google SERP parser: a web server, a CLI, and — on this
// milestone — a preflight check and a run that saves what it finds.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/blanktrail/google-serp-parser/internal/version"
)

// interruptExit is what the shell is told when the user stopped the job.
// A shell reports a program a signal ended as 128 plus that signal's number,
// and a job stopped on purpose is not the same event as one that failed.
const interruptExit = 130

func main() {
	if err := dispatch(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gserp:", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], os.Stdout)
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
	fs.StringVar(&opts.ControlURL, "bt-url", defaultControlURL, "BlankTrail control API base URL")
	fs.StringVar(&opts.APIKey, "bt-key", os.Getenv(envAPIKey), "BlankTrail API key")
	fs.IntVar(&opts.Threads, "threads", 2, "parsing threads the run will use")
	fs.IntVar(&opts.PortsPerThread, "ports-per-thread", 3, "BlankTrail ports per thread")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runDoctor(context.Background(), os.Stdout, opts)
}

func usage() { fmt.Print(usageText) }

// usageText is the help. It is a value rather than a series of prints so a test
// can hold it against the flags themselves: help that has drifted from the
// flags is read instead of the code, and believed.
const usageText = `gserp — open-source Google SERP parser, powered by BlankTrail Proxy

Usage:
  gserp run [flags]      work a list of queries, saving each one as it lands
  gserp doctor [flags]   check a BlankTrail instance against an intended run
  gserp version          print the version

Run flags:
  --queries string            file holding one query per line; blank lines
                              and lines starting with # are passed over
  --db string                 history database to write (default gserp.db)
  --out string                file to export the results into
  --format string             export format: csv or jsonl (default csv)
  --pages int                 result pages per query (default 1)
  --threads int               queries taken at once (default 2)
  --ports int                 ports per thread (default 3)
  --country string            two-letter country code, e.g. de
  --language string           language code, e.g. de
  --name string               name to file the job under (default: the query
                              list's file name)
  --resume                    take up the last unfinished job of this name
                              instead of starting one; it runs at the depth and
                              in the country it was created with
  --dry-run                   print the estimate and send nothing

Doctor flags:
  --bt-url string             control API base URL (default http://127.0.0.1:8891)
  --bt-key string             API key (or set BLANKTRAIL_API_KEY)
  --threads int               parsing threads (default 2)
  --ports-per-thread int      ports per thread (default 3)

Environment:
  BLANKTRAIL_URL              control API base URL for gserp run
                              (default http://127.0.0.1:8891)
  BLANKTRAIL_API_KEY          API key
  GSERP_PROXY_LIST_URL        address list to egress through; direct when unset

The key and the address list are read from the environment and are not flags:
a key on a command line is a key in the shell history.
`

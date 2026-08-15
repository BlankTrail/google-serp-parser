// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/blanktrail/google-serp-parser/blanktrail"
)

// doctorOptions is everything the check needs to know about the intended run.
type doctorOptions struct {
	ControlURL     string
	APIKey         string
	Threads        int
	PortsPerThread int
}

// checkedDomains are the hosts a run cannot work without. Google's search
// domains are listed explicitly rather than by wildcard because a restricted
// licence names them one by one, and the report has to say which one is missing.
var checkedDomains = []string{"www.google.com", "google.com"}

// runDoctor prints the preflight report and returns an error when the run would
// be pointless. Every finding is printed, not just the blocking ones: a warning
// about solver capacity is the difference between a slow run and a puzzling one.
func runDoctor(ctx context.Context, out io.Writer, opts doctorOptions) error {
	if strings.TrimSpace(opts.ControlURL) == "" {
		return errors.New("control URL is required (--bt-url)")
	}
	client, err := blanktrail.NewClient(opts.ControlURL, opts.APIKey)
	if err != nil {
		return err
	}

	threads, perThread := opts.Threads, opts.PortsPerThread
	if threads < 1 {
		threads = 1
	}
	if perThread < 1 {
		perThread = 1
	}
	ports := threads * perThread

	fmt.Fprintf(out, "Checking %s for a run of %d ports (%d threads × %d) against %s\n\n",
		opts.ControlURL, ports, threads, perThread, strings.Join(checkedDomains, ", "))

	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: checkedDomains,
		Ports:   ports,
	})
	for _, f := range report.Findings {
		fmt.Fprintf(out, "[%s] %s\n", f.Severity, f.Title)
		if f.Detail != "" {
			fmt.Fprintf(out, "      %s\n", f.Detail)
		}
		if f.Action != "" {
			fmt.Fprintf(out, "      → %s\n", f.Action)
		}
		fmt.Fprintln(out)
	}

	if gws := report.Gateways.Gateways; len(gws) > 0 {
		fmt.Fprintf(out, "VPN gateways available as egress channels: %d\n", len(gws))
		for _, g := range gws {
			state := "stopped"
			if g.Running {
				state = "running"
			}
			fmt.Fprintf(out, "  %-24s %-8s %s\n", g.Name, g.Kind, state)
		}
		fmt.Fprintln(out)
	}

	if !report.OK() {
		return fmt.Errorf("preflight failed with %d blocking finding(s)", len(report.Blocking()))
	}
	fmt.Fprintln(out, "Preflight passed. The proxy is ready for this run.")
	return nil
}

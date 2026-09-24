// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strings"
	"time"
)

// envProfiler names the address the running server answers Go's profiles on,
// for somebody taking apart where a run's threads wait. Unset, nothing listens.
//
// It exists because the question it answers could not be answered any other
// way: a job on the server spent half its threads' time taking sessions while
// the same run in the lab spent under one per cent there, the processor was
// idle, and the only thing that says what a waiting goroutine waits on is the
// program itself.
const envProfiler = "GSERP_PPROF"

// profilerAddr is where the profiles are served when the variable asks for them
// without naming an address.
const profilerAddr = "127.0.0.1:6060"

// startProfiler serves Go's profiles on a loopback address when the environment
// asks for them, and says where. An address that is not loopback is refused:
// the profiles hold the program's whole memory, keys included.
func startProfiler(out io.Writer, log *slog.Logger) {
	asked := strings.TrimSpace(os.Getenv(envProfiler))
	if asked == "" {
		return
	}
	addr := profilerAddr
	if strings.Contains(asked, ":") {
		addr = asked
	}
	ln, err := profilerListener(addr)
	if err != nil {
		log.Warn("the profiler was asked for and could not be started", "error", err)
		return
	}
	// Where goroutines wait on each other and on locks is the question, so both
	// of those profiles are switched on; each costs a little on every wait, and
	// only a server started to be taken apart pays it.
	runtime.SetBlockProfileRate(int(time.Millisecond))
	runtime.SetMutexProfileFraction(10)
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	go func() { _ = http.Serve(ln, mux) }()
	_, _ = io.WriteString(out, "profiles are served on http://"+ln.Addr().String()+"/debug/pprof/\n")
}

// profilerListener listens on the address if it is a loopback one.
func profilerListener(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, &net.AddrError{Err: "the profiler listens on loopback only", Addr: addr}
	}
	return net.Listen("tcp", addr)
}

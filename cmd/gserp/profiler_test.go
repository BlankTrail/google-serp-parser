// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestStartProfiler_ServesTheProfilesOnLoopbackOnlyWhenAskedTo(t *testing.T) {
	// The profiles hold the whole of the program's memory, keys included, so
	// they are served only when the environment asks and only on loopback.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var said bytes.Buffer
	t.Setenv(envProfiler, "")
	startProfiler(&said, quiet)
	if said.Len() != 0 {
		t.Fatalf("the profiler started without being asked: %q", said.String())
	}

	t.Setenv(envProfiler, "127.0.0.1:0")
	startProfiler(&said, quiet)
	line := strings.TrimSpace(said.String())
	at := strings.TrimPrefix(line, "profiles are served on ")
	if at == line {
		t.Fatalf("the profiler did not say where it listens: %q", line)
	}
	resp, err := http.Get(at + "goroutine?debug=1")
	if err != nil {
		t.Fatalf("asking the profiler: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "goroutine profile") {
		t.Errorf("the goroutine profile answered %d: %.120s", resp.StatusCode, body)
	}

	said.Reset()
	t.Setenv(envProfiler, "0.0.0.0:0")
	startProfiler(&said, quiet)
	if said.Len() != 0 {
		t.Errorf("the profiler listened on an address open to the network: %q", said.String())
	}
}

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

// sideBySide stands for a control API that takes its time over closing a port.
// Every close is held for a while before it is carried out, and the most closes
// ever under way at once is counted: a pool that gives its ports back one after
// another never has more than one.
type sideBySide struct {
	rt   http.RoundTripper
	hold time.Duration

	mu   sync.Mutex
	now  int
	most int
}

func (s *sideBySide) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/api/v1/ports/close" {
		return s.rt.RoundTrip(req)
	}
	s.mu.Lock()
	s.now++
	s.most = max(s.most, s.now)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.now--
		s.mu.Unlock()
	}()
	select {
	case <-time.After(s.hold):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return s.rt.RoundTrip(req)
}

// atOnce is the most closes that were ever under way together.
func (s *sideBySide) atOnce() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.most
}

// poolConfigThrough is testPoolConfig with every call to the control API
// passing through rt.
func poolConfigThrough(t *testing.T, fake *fakebt.Server, threads int, rt http.RoundTripper) PoolConfig {
	t.Helper()
	cfg := testPoolConfig(t, fake, newFakeClock(), threads, 1)
	c, err := NewClient(fake.URL(), fake.Key(), WithHTTPClient(&http.Client{Transport: rt, Timeout: 15 * time.Second}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cfg.Client = c
	return cfg
}

func TestPoolClose_GivesThePortsBackSideBySide(t *testing.T) {
	// A pool was given back one port after another, each close waiting on the
	// service, and each able to wait out its five seconds on a service still
	// busy with the run that had just stopped: a hundred ports, minutes. That was
	// the stretch a stopped job went on counting as running for, with its page
	// offering the stop somebody had just pressed. They go back side by side.
	fake := fakebt.New(t)
	slow := &sideBySide{rt: http.DefaultTransport, hold: 200 * time.Millisecond}
	p, err := NewPool(context.Background(), poolConfigThrough(t, fake, 8, slow))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 0 {
		t.Errorf("%d ports are still open after Close", got)
	}
	if got := slow.atOnce(); got < 2 {
		t.Errorf("the ports were given back one at a time (at most %d at once)", got)
	}
}

func TestPoolClose_GivesBackNoMoreThanItsWidthAtOnce(t *testing.T) {
	// Side by side, and not all at once: a hundred requests landing together on
	// a service still busy with the run is a burst it has never been sent, and
	// a hundred connections opened for one moment.
	fake := fakebt.New(t)
	slow := &sideBySide{rt: http.DefaultTransport, hold: 100 * time.Millisecond}
	p, err := NewPool(context.Background(), poolConfigThrough(t, fake, closeWidth+8, slow))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(fake.OpenPorts()); got != 0 {
		t.Errorf("%d ports are still open after Close", got)
	}
	if got := slow.atOnce(); got > closeWidth {
		t.Errorf("%d ports were given back at once, more than the %d allowed", got, closeWidth)
	}
}

func TestPoolShrink_GivesTheGrownPortsBackSideBySide(t *testing.T) {
	// The same for a pool that keeps some ports warm between jobs: what a job
	// opened on top of them goes back when it lets go, and a stopped job waits
	// for that too.
	fake := fakebt.New(t)
	slow := &sideBySide{rt: http.DefaultTransport, hold: 200 * time.Millisecond}
	p, err := NewPool(context.Background(), poolConfigThrough(t, fake, 2, slow))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if got := p.KeepWarm(); got != 2 {
		t.Fatalf("KeepWarm marked %d ports, want 2", got)
	}
	if err := p.Grow(context.Background(), 6); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	gone, err := p.Shrink(context.Background())
	if err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if gone != 6 {
		t.Errorf("Shrink gave back %d ports, want the six that were grown", gone)
	}
	if got := len(fake.OpenPorts()); got != 2 {
		t.Errorf("%d ports are open after Shrink, want the two kept warm", got)
	}
	if got := slow.atOnce(); got < 2 {
		t.Errorf("the grown ports were given back one at a time (at most %d at once)", got)
	}
}

// refuseClose stands for a service that will not close one port: it answers
// the close of that port with an error and carries every other call through.
type refuseClose struct {
	rt   http.RoundTripper
	port int
}

func (r *refuseClose) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/api/v1/ports/close" {
		return r.rt.RoundTrip(req)
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	if !strings.Contains(string(raw), fmt.Sprintf(`"port":%d`, r.port)) {
		req.Body = io.NopCloser(bytes.NewReader(raw))
		return r.rt.RoundTrip(req)
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Status:     http.StatusText(http.StatusInternalServerError),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"could not close"}`)),
		Request:    req,
	}, nil
}

func TestPoolClose_SaysAPortTheServiceWouldNotCloseAndClosesTheRest(t *testing.T) {
	// Side by side, a refusal still reaches whoever gave the pool up — a port
	// left open on the service is one this program is still holding — and one
	// refusal does not keep the other ports from going back.
	fake := fakebt.New(t)
	refusing := &refuseClose{rt: http.DefaultTransport}
	p, err := NewPool(context.Background(), poolConfigThrough(t, fake, 6, refusing))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	open := fake.OpenPorts()
	if len(open) != 6 {
		t.Fatalf("%d ports are open, want 6", len(open))
	}
	refusing.port = open[3]
	if err := p.Close(); err == nil {
		t.Error("Close said nothing about a port the service would not close")
	}
	if got := fake.OpenPorts(); len(got) != 1 || got[0] != refusing.port {
		t.Errorf("after Close the service holds %v, want only the refused %d", got, refusing.port)
	}
}

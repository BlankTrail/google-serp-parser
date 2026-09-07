//go:build live

// SPDX-License-Identifier: MIT

package run

// Whether reading a hidden address needs a session at all.
//
// A lookup is a GET to Google's redirector and the answer is a Location header.
// It carries no challenge and reads no page, so on the face of it none of what
// a search needs applies to it: no Challenge Breaker, no cookie jar, no
// continuity with whoever captured the page. If that holds, the lookups belong
// on their own ports — off the solver, which is a licensed and limited thing,
// and out of the sessions the searches are built in.
//
// Three arms over the same links, each link through a fresh identity:
//
//	as now       — the ports a search runs on: solver on, cookie jar on
//	no solver    — the same ports with Challenge Breaker off
//	plain        — solver off and no cookie jar either, so every request is a
//	               stranger
//
//	go test -tags live -run TestLivePlainPorts -timeout 40m ./internal/run/ -v
//
// It opens three pools of four ports and spends a few hundred requests.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// plainTries is how many identities one link is carried to before it is given
// up on here. It is the program's own allowance cut down to keep the
// measurement inside its hour: three addresses in four carry nothing, so a link
// wants several whatever the port is made of.
const plainTries = 12

func TestLivePlainPorts_WhetherAHiddenAddressNeedsASessionToBeRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)
	control, key, listURL := liveEnv(t)
	ups := addressList(ctx, t, listURL)

	// The capture needs everything a search needs; only the lookups are in
	// question. It is done once and both arms read the same links.
	query := google.Query{Text: "купить кондиционер", Country: "ru", Language: "ru"}
	var serps []google.SERP
	for try := 1; try <= 15; try++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			fatalf(t, "acquiring an identity to capture with: %v", err)
		}
		cl := lease.Client()
		session := google.NewSession(cl.Transport)
		session.Client.Timeout = cl.Timeout
		got, err := google.SearchDepth(ctx, session, query, 3)
		lease.Release()
		if err != nil {
			logf(t, "capture attempt %d did not get through: %v", try, err)
			continue
		}
		serps = got
		break
	}
	if len(serps) == 0 {
		t.Fatal("no identity carried the capture")
	}

	origin := serps[0].Origin
	var hidden []google.Result
	for _, serp := range serps {
		for _, one := range serp.Results {
			if !one.Resolved() && one.Link != "" {
				hidden = append(hidden, one)
			}
		}
	}
	logf(t, "MEASUREMENT capture: %d pages, %d addresses to read", len(serps), len(hidden))
	if len(hidden) < 9 {
		t.Skip("this capture hid too few addresses to split three ways")
	}

	solver := blanktrail.DefaultPortSpec()

	noSolver := blanktrail.DefaultPortSpec()
	noSolver.JSSolver = false

	plain := blanktrail.DefaultPortSpec()
	plain.JSSolver = false
	plain.KeepSessions = false

	third := len(hidden) / 3
	arms := []struct {
		name string
		spec blanktrail.PortSpec
		of   []google.Result
	}{
		{"as now: solver on, cookie jar on", solver, hidden[:third]},
		{"Challenge Breaker off", noSolver, hidden[third : 2*third]},
		{"plain: no solver, no cookie jar", plain, hidden[2*third:]},
	}
	for _, a := range arms {
		logf(t, "MEASUREMENT %s: %s", a.name, readThrough(ctx, t, control, key, ups, a.spec, origin, a.of))
	}
}

// readThrough opens a pool of ports made to one spec and reads every link
// through it, a fresh identity per attempt, and says what that cost.
func readThrough(ctx context.Context, t *testing.T, control, key string,
	ups []blanktrail.Upstream, spec blanktrail.PortSpec, origin string, rs []google.Result) string {
	t.Helper()

	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		fatalf(t, "control client: %v", err)
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"}, Ports: 4,
	})
	if !pre.OK() {
		t.Skip("preflight refused the run")
	}
	p, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 4, PortsPerThread: 1,
		Spec: spec, CA: pre.CA,
		Channels:     []blanktrail.Channel{blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(ups))},
		Cooldown:     time.Second,
		ReviveAfter:  time.Minute,
		NoKeepAlives: true,
	})
	if err != nil {
		return fmt.Sprintf("the pool would not open: %v", err)
	}
	defer func() { _ = p.Close() }()

	began := time.Now()
	var read, attempts, lost int
	kinds := map[string]int{}
	perLink := map[int]int{}
	for i := range rs {
		got := 0
		for try := 1; try <= plainTries; try++ {
			lease, err := p.Acquire(ctx)
			if err != nil {
				fatalf(t, "acquiring an identity: %v", err)
			}
			cl := lease.Client()
			resolver := google.NewResolver(cl.Transport)
			resolver.Client.Timeout = cl.Timeout
			copyOf := []google.Result{rs[i]}
			rep := resolver.ResolveResults(ctx, origin, copyOf, 1)
			attempts++
			if rep.Resolved == 0 {
				kinds[kindOfResolveFailure(rep.Errs)]++
				_ = lease.Reject(ctx)
			}
			lease.Release()
			if rep.Resolved == 1 {
				got = try
				break
			}
		}
		if got == 0 {
			lost++
			continue
		}
		read++
		perLink[got]++
	}
	took := time.Since(began)

	var spread []string
	var tries []int
	for n := range perLink {
		tries = append(tries, n)
	}
	sort.Ints(tries)
	for _, n := range tries {
		spread = append(spread, fmt.Sprintf("%d×%d", n, perLink[n]))
	}
	var why []string
	for kind, n := range kinds {
		why = append(why, fmt.Sprintf("%s×%d", kind, n))
	}
	sort.Strings(why)

	said := fmt.Sprintf("%d of %d read over %d attempts in %v (%v a link); identities per address: %s",
		read, len(rs), attempts, took.Round(time.Second),
		(took / time.Duration(max(len(rs), 1))).Round(time.Millisecond),
		strings.Join(spread, ", "))
	if lost > 0 {
		said += fmt.Sprintf("; %d never came", lost)
	}
	if len(why) > 0 {
		said += "; the failures: " + strings.Join(why, ", ")
	}
	return said
}

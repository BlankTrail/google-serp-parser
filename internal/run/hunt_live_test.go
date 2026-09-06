//go:build live

// SPDX-License-Identifier: MIT

package run

// What a request gives up on, and what that costs — on a pool that has just
// been opened.
//
// AddressesPerRequest is how many addresses one request may be carried to when
// they fail to carry it at all. It is one, deliberately: the looking is done by
// the thirty identities a phrase is taken to, each a fresh port with its own
// address, and a request that walks fifteen addresses on one port does that
// same looking twice over while the rest of the pool sits idle. Measured
// against a reference client on the same list, that arrangement answered 71% of
// its requests against 29% for the walk.
//
// This measures the other end of it, and the two do not disagree. On a pool
// whose ports have not yet found an address that answers, walking wins by
// everything there is: none of ten links against ten of ten. What it says is
// what a cold pool costs — not that the default is wrong, but that the pool has
// to be allowed to warm. A run that keeps taking the address a port has just
// proved out of that port is a run whose pool is cold for ever, and that is
// what counting a lookup's redirect as the wall was doing.
//
//	go test -tags live -run TestLiveHunt -timeout 30m ./internal/run/ -v
//
// It opens two pools of four ports and spends one request per link per arm.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

func TestLiveHunt_WhatARequestThatWalksTheListIsWorth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)
	control, key, listURL := liveEnv(t)
	ups := addressList(ctx, t, listURL)

	// One capture, kept, so both arms look up the same links. The links expire,
	// so the arms run one after the other and the second is not handed a stale
	// set: they are minutes apart at most.
	query := google.Query{Text: "купить кондиционер", Country: "ru", Language: "ru"}
	var serps []google.SERP
	for try := 1; try <= 12; try++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			fatalf(t, "acquiring an identity to capture with: %v", err)
		}
		cl := lease.Client()
		session := google.NewSession(cl.Transport)
		session.Client.Timeout = cl.Timeout
		got, err := google.SearchDepth(ctx, session, query, 2)
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
	logf(t, "MEASUREMENT capture: %d pages, %d addresses to look up", len(serps), len(hidden))
	if len(hidden) < 8 {
		t.Skip("this capture hid too few addresses to compare the two arms")
	}
	half := len(hidden) / 2

	one := huntArm(ctx, t, control, key, ups, 1, origin, hidden[:half])
	logf(t, "MEASUREMENT giving up on the first dead address: %s", one)
	fifteen := huntArm(ctx, t, control, key, ups, 15, origin, hidden[half:])
	logf(t, "MEASUREMENT walking the list inside the request:  %s", fifteen)
}

// huntArm opens a pool with one setting of AddressesPerRequest and looks up
// every link through it, one request each, and says what that cost.
func huntArm(ctx context.Context, t *testing.T, control, key string,
	ups []blanktrail.Upstream, addresses int, origin string, rs []google.Result) string {
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
		Spec: blanktrail.DefaultPortSpec(), CA: pre.CA,
		Channels:            []blanktrail.Channel{blanktrail.NewListChannel("list", blanktrail.NewStaticRotor(ups))},
		DelayMin:            2 * time.Second,
		DelayMax:            5 * time.Second,
		ReviveAfter:         time.Minute,
		NoKeepAlives:        true,
		AddressesPerRequest: addresses,
	})
	if err != nil {
		fatalf(t, "opening a pool with %d addresses per request: %v", addresses, err)
	}
	defer func() { _ = p.Close() }()

	began := time.Now()
	var read int
	kinds := map[string]int{}
	for i := range rs {
		lease, err := p.Acquire(ctx)
		if err != nil {
			fatalf(t, "acquiring an identity: %v", err)
		}
		cl := lease.Client()
		resolver := google.NewResolver(cl.Transport)
		resolver.Client.Timeout = cl.Timeout
		copyOf := []google.Result{rs[i]}
		rep := resolver.ResolveResults(blanktrail.RedirectIsTheAnswer(ctx), origin, copyOf, 1)
		if rep.Resolved == 1 {
			read++
		} else {
			kinds[kindOfResolveFailure(rep.Errs)]++
		}
		lease.Release()
	}
	took := time.Since(began)
	st := p.Stats()

	var why []string
	for kind, n := range kinds {
		why = append(why, fmt.Sprintf("%s×%d", kind, n))
	}
	said := fmt.Sprintf("%d of %d links read in one request each, in %v (%v a link); the pool put %d attempts on the wire for %d requests",
		read, len(rs), took.Round(time.Millisecond),
		(took / time.Duration(max(len(rs), 1))).Round(time.Millisecond),
		st.Attempts, st.Requests)
	if len(why) > 0 {
		said += "; the rest: " + strings.Join(why, ", ")
	}
	return said
}

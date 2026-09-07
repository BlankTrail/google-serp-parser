//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"io"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// TestWarmCost_LiveWhatASecondRequestOnOneIdentityCosts measures the whole
// premise of keeping identities warm: that a port which has already answered
// answers again quickly.
//
// It measures it twice on two ports of the same pool — once through a session
// built the way this program builds them, and once through a session that keeps
// what the answer sets. If the two look the same, warming buys nothing and the
// standing pool is an expense with no return; if they differ, the difference is
// the number that says what has to be kept between requests.
//
// Nothing here is asserted. It reports, and the numbers decide.
func TestWarmCost_LiveWhatASecondRequestOnOneIdentityCosts(t *testing.T) {
	// The history to read the connection beside comes from the environment. A
	// path written into this file would be one machine's, and this file is
	// published.
	db := os.Getenv(envLiveDB)
	if db == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: db}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" {
		t.Skipf("no connection is saved beside the history %s names", envLiveDB)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	want, err := o.dial(ctx, saved, theFirstProfile(saved, true), 1, 6, blanktrail.DeviceDesktop, 0, false, false)
	if err != nil {
		t.Fatalf("opening two identities: %v", o.clean(err.Error()))
	}
	pool := want.Search
	defer func() { _ = pool.Close() }()

	for _, keep := range []bool{false, true} {
		label := "as this program builds a session (nothing kept)"
		if keep {
			label = "keeping what the answers set"
		}
		// An address from the list can be dead, and a dead one answers nothing at
		// any temperature. So the first request is carried to another identity
		// until one answers, and only what happens after that is the measurement.
		var (
			lease *blanktrail.Lease
			sess  *google.Session
		)
		for tried := 1; tried <= 6; tried++ {
			l, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("taking an identity: %v", o.clean(err.Error()))
			}
			s := google.NewSession(l.Client().Transport)
			s.Client.Timeout = l.Client().Timeout
			if keep {
				jar, err := cookiejar.New(nil)
				if err != nil {
					t.Fatalf("cookie jar: %v", err)
				}
				s.Client.Jar = jar
			}
			started := time.Now()
			serp, err := s.Search(ctx, google.Query{Text: "weather", Country: "us", Language: "en"})
			took := time.Since(started)
			if err != nil {
				t.Logf("port %d — %s: first request failed after %v: %s",
					l.Port(), label, took.Round(time.Millisecond), o.clean(err.Error()))
				_ = l.Reject(ctx)
				continue
			}
			t.Logf("port %d — %s", l.Port(), label)
			t.Logf("  request 1 (cold): %v — %d results", took.Round(time.Millisecond), len(serp.Results))
			lease, sess = l, s
			break
		}
		if lease == nil {
			t.Fatalf("no identity answered a first request — %s measures nothing", label)
		}

		for i, phrase := range []string{"train times", "recipes", "dictionary"} {
			started := time.Now()
			serp, err := sess.Search(ctx, google.Query{Text: phrase, Country: "us", Language: "en"})
			took := time.Since(started)
			what := fmt.Sprintf("%d results", len(serp.Results))
			if err != nil {
				what = "failed: " + o.clean(err.Error())
			}
			t.Logf("  request %d: %v — %s", i+2, took.Round(time.Millisecond), what)
		}
		lease.Release()
	}
}

// envLiveDB names the history to read the connection beside: the one the
// browser interface is using, so what is measured is what this machine runs.
const envLiveDB = "GSERP_LIVE_DB"

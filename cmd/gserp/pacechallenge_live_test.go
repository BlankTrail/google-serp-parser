//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// envPaceGaps and envPaceBudget let the operator set the arms of the
// measurement without editing a published file: the gaps to try, in seconds,
// and how many requests each arm may spend.
const (
	envPaceGaps   = "GSERP_LIVE_PACE_GAPS"
	envPaceBudget = "GSERP_LIVE_PACE_BUDGET"
)

// slowEnoughToBeAChallenge is where a request stops looking like an answer and
// starts looking like a challenge being solved behind it.
//
// Measured in this repository: a request through an identity that has answered
// costs one to two seconds, and the first request on a cold one costs one to
// three minutes. Ten seconds is far outside the first and far inside the
// second, so nothing turns on where exactly the line is drawn.
const slowEnoughToBeAChallenge = 10 * time.Second

// TestPace_LiveWhetherRestingBuysRequestsBeforeAChallenge answers the one
// question that decides whether more than one port per thread is worth having.
//
// The case for it is that an identity asked less often lasts longer before
// Google challenges it, so a thread with several ports and a pause between
// requests gets more answers out of each identity than a thread hammering one.
// The case against is that identities go cold: an identity that rests is an
// identity that pays one to three minutes on its next request, and a pool of
// many rarely-used identities pays that over and over.
//
// Both are plausible and they point opposite ways, so this measures the first
// half directly: one identity, warmed, then asked at a fixed gap until it is
// challenged or the budget runs out. Repeated at several gaps, on a fresh
// identity each time — a challenge in one arm would otherwise be inherited by
// the next.
//
// Nothing here is asserted. It reports, and the numbers decide.
func TestPace_LiveWhetherRestingBuysRequestsBeforeAChallenge(t *testing.T) {
	db := os.Getenv(envLiveDB)
	if db == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: db}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" {
		t.Skipf("no connection is saved beside the history %s names", envLiveDB)
	}

	gaps := gapsAsked(t)
	budget := budgetAsked(t)

	// Long enough for every arm plus the cold start each of them opens with.
	var patience time.Duration
	for _, gap := range gaps {
		patience += time.Duration(budget)*(gap+30*time.Second) + 5*time.Minute
	}
	ctx, cancel := context.WithTimeout(t.Context(), patience)
	defer cancel()

	// One identity per arm and a few in reserve: an address from the list can be
	// dead, and a dead one answers nothing at any pace.
	pool, err := o.dial(ctx, saved, 1, len(gaps)+4, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening the identities: %v", o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	t.Logf("gaps: %v, budget: %d requests an arm", gaps, budget)
	type arm struct {
		gap       time.Duration
		answered  int
		challenge time.Duration
		died      string
	}
	var arms []arm

	for _, gap := range gaps {
		lease, sess := warmed(ctx, t, o, pool)
		if lease == nil {
			t.Errorf("gap %v: no identity answered a first request, so this arm measures nothing", gap)
			continue
		}
		a := arm{gap: gap}
		t.Logf("gap %v — port %d", gap, lease.Port())
		for i := range budget {
			if gap > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(gap):
				}
			}
			// A break inside that select would leave the select and go straight
			// on to another request, which is how a measurement runs past its own
			// deadline printing timeouts.
			if ctx.Err() != nil {
				t.Logf("  out of time after %d quick answers", a.answered)
				break
			}
			started := time.Now()
			serp, err := sess.Search(ctx, google.Query{
				Text: phraseFor(i), Country: "us", Language: "en"})
			took := time.Since(started)
			switch {
			case err != nil:
				a.died = o.clean(err.Error())
				t.Logf("  request %d after %v: failed — %s", i+1, took.Round(time.Millisecond), a.died)
			case took >= slowEnoughToBeAChallenge:
				a.challenge = took
				t.Logf("  request %d: %v — a challenge, after %d quick answers",
					i+1, took.Round(time.Millisecond), a.answered)
			default:
				a.answered++
				t.Logf("  request %d: %v — %d results", i+1, took.Round(time.Millisecond), len(serp.Results))
			}
			if a.challenge > 0 || a.died != "" {
				break
			}
		}
		lease.Release()
		arms = append(arms, a)
	}

	t.Log("")
	t.Log("gap between requests | quick answers before a challenge | what ended the arm")
	for _, a := range arms {
		ended := "the budget ran out"
		switch {
		case a.died != "":
			ended = "the identity failed: " + a.died
		case a.challenge > 0:
			ended = "a challenge, " + a.challenge.Round(time.Second).String()
		}
		t.Logf("%-20v | %-32d | %s", a.gap, a.answered, ended)
	}
	t.Log("")
	t.Log("If the quick answers climb with the gap, resting an identity buys requests")
	t.Log("and more than one port per thread with a pause is worth its cold starts.")
	t.Log("If they do not, the gap buys nothing and one port per thread is the whole")
	t.Log("of it.")
}

// warmed takes identities until one answers, and hands back that identity and
// the session over it. What is measured is what happens after this, because a
// first request is the cold start every arm pays alike.
func warmed(ctx context.Context, t *testing.T, o serveOptions, pool *blanktrail.Pool) (*blanktrail.Lease, *google.Session) {
	t.Helper()
	for tried := 1; tried <= 5; tried++ {
		l, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("taking an identity: %v", o.clean(err.Error()))
		}
		s := google.NewSession(l.Client().Transport)
		s.Client.Timeout = l.Client().Timeout
		started := time.Now()
		if _, err := s.Search(ctx, google.Query{Text: "weather", Country: "us", Language: "en"}); err != nil {
			t.Logf("  warming: an identity failed after %v: %s",
				time.Since(started).Round(time.Millisecond), o.clean(err.Error()))
			_ = l.Reject(ctx)
			l.Release()
			continue
		}
		l.Answered()
		t.Logf("  warmed in %v", time.Since(started).Round(time.Millisecond))
		return l, s
	}
	return nil, nil
}

// phraseFor is an ordinary one-word query, and a different one each time: the
// same phrase asked twice may be answered from something other than a search.
func phraseFor(i int) string {
	words := []string{
		"weather", "recipes", "dictionary", "train times", "calculator",
		"news", "maps", "translate", "hardware store", "opening hours",
		"football scores", "flight status", "currency", "postcode", "pharmacy",
		"bus timetable", "cinema", "library", "car hire", "dentist",
	}
	return words[i%len(words)]
}

func gapsAsked(t *testing.T) []time.Duration {
	t.Helper()
	raw := os.Getenv(envPaceGaps)
	if raw == "" {
		return []time.Duration{0, 15 * time.Second, 60 * time.Second}
	}
	var gaps []time.Duration
	for _, part := range splitList(raw) {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			t.Fatalf("%s: %q is not a number of seconds", envPaceGaps, part)
		}
		gaps = append(gaps, time.Duration(n)*time.Second)
	}
	return gaps
}

func budgetAsked(t *testing.T) int {
	t.Helper()
	raw := os.Getenv(envPaceBudget)
	if raw == "" {
		return 15
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		t.Fatalf("%s: %q is not a number of requests", envPaceBudget, raw)
	}
	return n
}

// splitList reads "0,15,60" into its parts, ignoring the empty ones so a
// trailing comma is not a mistake.
func splitList(raw string) []string {
	var out []string
	part := ""
	for _, r := range raw + "," {
		if r == ',' || r == ' ' {
			if part != "" {
				out = append(out, part)
				part = ""
			}
			continue
		}
		part += string(r)
	}
	return out
}

//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"os"
	"sort"
	"strconv"
	"sync"
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
	// envPacePorts is how many identities to open. The default is an arm each
	// and a few in reserve, which is enough on a list where most addresses
	// answer. On one where most do not, the arms hunt through the same few ports
	// and three of six found nothing — so the pool is worth widening by hand.
	envPacePorts = "GSERP_LIVE_PACE_PORTS"
)

// slowEnoughToBeAChallenge is where a request stops looking like an answer and
// starts looking like a challenge being solved behind it.
//
// Measured in this repository: a request through an identity that has answered
// costs one to two seconds, and the first request on a cold one costs one to
// three minutes. Ten seconds is far outside the first and far inside the
// second, so nothing turns on where exactly the line is drawn.
const slowEnoughToBeAChallenge = 10 * time.Second

// warmingTries is how many identities one arm may work through to find one that
// answers a first request.
const warmingTries = 30

// TestPace_LiveWhetherRestingBuysRequestsBeforeAChallenge answers the one
// question that decides whether more than one port per thread is worth having.
//
// The case for it is that an identity asked less often lasts longer before
// Google challenges it, so a thread with several ports and a pause between
// requests gets more answers out of each identity than a thread hammering one.
// The case against is that identities go cold: an identity that rests pays one
// to three minutes on its next request, and a pool of many rarely-used
// identities pays that over and over.
//
// Every arm runs at the same time, on its own identity, through one gateway —
// so all of them share one exit address, and share it in the same condition at
// every moment. Run one after another they would not: the first arm would meet
// a fresh address and the last one an address that had already carried
// everything before it, and a difference between them would say as much about
// the address as about the pace. Running together, what differs between the
// arms is the gap and nothing else.
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

	// The arms run together, so the measurement is as long as its slowest arm —
	// plus room for the cold start each identity opens with.
	var longest time.Duration
	for _, gap := range gaps {
		if gap > longest {
			longest = gap
		}
	}
	patience := time.Duration(budget)*(longest+30*time.Second) + 20*time.Minute
	ctx, cancel := context.WithTimeout(t.Context(), patience)
	defer cancel()

	// One identity per arm and a few in reserve: an address can be dead, and a
	// dead one answers nothing at any pace.
	ports := len(gaps) + 4
	if raw := os.Getenv(envPacePorts); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < len(gaps) {
			t.Fatalf("%s: %q is not a number of identities, and there must be at least one an arm", envPacePorts, raw)
		}
		ports = n
	}
	pool, err := o.dial(ctx, saved, 1, ports, blanktrail.DeviceDesktop, 0)
	if err != nil {
		t.Fatalf("opening the identities: %v", o.clean(err.Error()))
	}
	defer func() { _ = pool.Close() }()

	t.Logf("gaps: %v, budget: %d requests an arm, %d identities, every arm at once", gaps, budget, ports)

	// Warmed together, for the reason they are run together: an identity warmed
	// a quarter of an hour before another is an identity of a different age.
	type held struct {
		lease *blanktrail.Lease
		sess  *google.Session
	}
	warm := make([]held, len(gaps))
	var warming sync.WaitGroup
	var say sync.Mutex
	for i := range gaps {
		warming.Add(1)
		go func(i int) {
			defer warming.Done()
			l, s := warmed(ctx, t, &say, o, pool)
			warm[i] = held{lease: l, sess: s}
		}(i)
	}
	warming.Wait()

	type arm struct {
		gap  time.Duration
		port int
		// egress is where this arm's identity sends its traffic. Printed because
		// an arm is only about the pace when the address is its own: six arms
		// behind one address measure that address, and the first run of this
		// measurement proved it — every arm died inside a minute whatever its
		// gap, because what was walled was the exit.
		egress    string
		answered  int
		challenge time.Duration
		died      string
		ran       time.Duration
	}
	arms := make([]arm, len(gaps))

	var running sync.WaitGroup
	for i, gap := range gaps {
		if warm[i].lease == nil {
			say.Lock()
			t.Errorf("gap %v: no identity answered a first request, so this arm measures nothing", gap)
			say.Unlock()
			continue
		}
		arms[i] = arm{gap: gap, port: warm[i].lease.Port(), egress: warm[i].lease.Egress().String()}
		running.Add(1)
		go func(i int, gap time.Duration, h held) {
			defer running.Done()
			defer h.lease.Release()
			began := time.Now()
			for n := range budget {
				if gap > 0 {
					select {
					case <-ctx.Done():
					case <-time.After(gap):
					}
				}
				if ctx.Err() != nil {
					break
				}
				started := time.Now()
				// A different phrase per arm as well as per request: several
				// identities behind one address asking the same words in the
				// same second is a pattern of its own, and not the one being
				// measured.
				serp, err := h.sess.Search(ctx, google.Query{
					Text: phraseFor(n*len(gaps) + i), Country: "us", Language: "en"})
				took := time.Since(started)
				say.Lock()
				switch {
				case err != nil:
					arms[i].died = o.clean(err.Error())
					t.Logf("gap %-4v port %d request %3d: failed after %v — %s",
						gap, h.lease.Port(), n+1, took.Round(time.Millisecond), arms[i].died)
				case took >= slowEnoughToBeAChallenge:
					arms[i].challenge = took
					t.Logf("gap %-4v port %d request %3d: %v — a challenge, after %d quick answers",
						gap, h.lease.Port(), n+1, took.Round(time.Millisecond), arms[i].answered)
				default:
					arms[i].answered++
					t.Logf("gap %-4v port %d request %3d: %v — %d results",
						gap, h.lease.Port(), n+1, took.Round(time.Millisecond), len(serp.Results))
				}
				done := arms[i].challenge > 0 || arms[i].died != ""
				say.Unlock()
				if done {
					break
				}
			}
			say.Lock()
			arms[i].ran = time.Since(began)
			say.Unlock()
		}(i, gap, warm[i])
	}
	running.Wait()

	sort.SliceStable(arms, func(a, b int) bool { return arms[a].gap < arms[b].gap })
	t.Log("")
	t.Log("gap    port   egress                          quick answers   arm lasted   what ended it")
	for _, a := range arms {
		if a.port == 0 {
			continue
		}
		ended := "the budget ran out"
		switch {
		case a.died != "":
			ended = "the identity failed: " + a.died
		case a.challenge > 0:
			ended = "a challenge, " + a.challenge.Round(time.Second).String()
		}
		t.Logf("%-6v %-6d %-31s %-15d %-12v %s", a.gap, a.port, a.egress, a.answered, a.ran.Round(time.Second), ended)
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
func warmed(ctx context.Context, t *testing.T, say *sync.Mutex, o serveOptions, pool *blanktrail.Pool) (*blanktrail.Lease, *google.Session) {
	t.Helper()
	// Enough tries to find a live address on a list where most are not. Measured
	// on this one: roughly one address in twelve carries anything, so five tries
	// leave two arms in three with no identity at all — which is an hour spent
	// measuring nothing.
	for tried := 1; tried <= warmingTries; tried++ {
		l, err := pool.Acquire(ctx)
		if err != nil {
			say.Lock()
			t.Errorf("taking an identity: %v", o.clean(err.Error()))
			say.Unlock()
			return nil, nil
		}
		s := google.NewSession(l.Client().Transport)
		s.Client.Timeout = l.Client().Timeout
		started := time.Now()
		if _, err := s.Search(ctx, google.Query{Text: "weather", Country: "us", Language: "en"}); err != nil {
			say.Lock()
			t.Logf("  warming port %d: failed after %v — %s", l.Port(),
				time.Since(started).Round(time.Millisecond), o.clean(err.Error()))
			say.Unlock()
			// Straight to another address rather than through the pool's own
			// counting: a rejection moves a port only after three failures in a
			// row, so hunting for a live address on a list where most are dead
			// went round the same few addresses and three arms of six never
			// found one at all.
			_ = pool.RotateEgressFor(ctx, l.Port())
			l.Release()
			continue
		}
		l.Answered()
		say.Lock()
		t.Logf("  warmed port %d in %v", l.Port(), time.Since(started).Round(time.Millisecond))
		say.Unlock()
		return l, s
	}
	return nil, nil
}

// phraseFor is an ordinary query, and a different one each time: the same
// phrase asked twice may be answered from something other than a search.
func phraseFor(i int) string {
	words := []string{
		"weather", "recipes", "dictionary", "train times", "calculator",
		"news", "maps", "translate", "hardware store", "opening hours",
		"football scores", "flight status", "currency", "postcode", "pharmacy",
		"bus timetable", "cinema", "library", "car hire", "dentist",
		"coffee near me", "petrol prices", "tax return", "bank holidays", "tide times",
		"museum tickets", "vet", "locksmith", "plumber", "hotel deals",
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

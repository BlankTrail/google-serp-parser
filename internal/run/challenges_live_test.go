//go:build live

// SPDX-License-Identifier: MIT

package run

// Whether Google's check can be counted at all, and by what.
//
// The screen reports how often a run's sessions are made to pass one, and the
// whole reading turns on the signal being there: a counter that reads nought
// through a run that met a dozen is worse than no counter, because it says the
// pace is fine.
//
// Two signals are measured against each other here. The one the program uses is
// the clearance — Google issues a cookie when its check is passed, so a session
// holding one it did not hold before has just paid for one. The other is the
// clock, which is what an operator would reach for first: a check is solved
// inside the request that met it, so that request takes tens of seconds.
//
// What the run prints is how many answers came back with a new clearance and
// how many took longer than half a minute. The first tells whether the signal
// exists at all; the two together tell whether the clock could have stood in
// for it.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// checkLivePhrases are asked once each. None of them appears in the other live
// tests, so no answer can come from a cache one of them filled.
var checkLivePhrases = []string{
	"стеклопакеты цена", "ремонт стиральных машин", "доставка воды",
	"курсы программирования", "шиномонтаж круглосуточно", "букет с доставкой",
	"натяжной потолок цена", "аренда спецтехники",
}

// slowAnswer is how long an answer has to take before the clock would call it a
// check. Thirty seconds, the number an operator reaches for.
const slowAnswer = 30 * time.Second

func TestLiveChallenges_AreCountedByTheClearanceGoogleLeaves(t *testing.T) {
	ctx := context.Background()
	control, key, listURL := liveEnv(t)
	ups := addressList(ctx, t, listURL)
	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		fatalf(t, "control client: %v", err)
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"}, Ports: 2})
	if !pre.OK() {
		t.Skip("preflight refused the run")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		fatalf(t, "store.Open: %v", err)
	}
	defer st.Close()

	k := sessions.NewKeeper(st)
	want := sessions.Want{Device: blanktrail.DeviceDesktop, Pause: time.Minute, UpTo: 2 * time.Minute}
	spec := blanktrail.DefaultPortSpec()
	spec.FirstHop = liveFirstHop(t)
	switch {
	case spec.FirstHop.Gateway != "":
		logf(t, "MEASUREMENT the ports go through a first hop: the gateway %s", spec.FirstHop.Gateway)
	case spec.FirstHop.Proxy != "":
		logf(t, "MEASUREMENT the ports go through a first hop: a SOCKS5 proxy")
	default:
		logf(t, "MEASUREMENT the ports go to their addresses directly")
	}

	const threads = 2
	p, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: threads, PortsPerThread: 1, Spec: spec, CA: pre.CA,
		Channels: []blanktrail.Channel{blanktrail.NewListChannel("list",
			blanktrail.NewStaticRotor(ups, blanktrail.WithRest(time.Hour)))},
		Sessions: true, Choose: k.Choose, AddressesPerRequest: 15,
		ReviveAfter: time.Minute, WaitForIdentity: true,
	})
	if err != nil {
		fatalf(t, "opening the pool: %v", err)
	}
	defer p.Close()

	counting := &Challenges{}
	refusals := &asks{}
	var mu sync.Mutex
	var answers, slow int
	var longest time.Duration

	qs := make([]google.Query, len(checkLivePhrases))
	for i, ph := range checkLivePhrases {
		qs[i] = google.Query{Text: ph, Country: "ru", Language: "ru"}
	}

	began := time.Now()
	rep := (&Runner{Pool: p, Threads: threads, Keeper: k, Want: want, Challenges: counting,
		Watch: func(s Step) {
			if s.Stage != StageAsk {
				return
			}
			if s.Err != nil {
				refusals.note(s.Err)
				return
			}
			mu.Lock()
			answers++
			if s.Took >= slowAnswer {
				slow++
			}
			if s.Took > longest {
				longest = s.Took
			}
			mu.Unlock()
		}}).Run(ctx, Job{Queries: qs, Pages: 1})
	took := time.Since(began)

	done := 0
	for _, q := range rep.Results {
		if q.Err == nil && len(q.Pages) > 0 {
			done++
		}
	}
	rhythm := counting.Rhythm()
	made, dropped := k.Count()

	logf(t, "MEASUREMENT %d of %d queries answered in %v; %d sessions known, %d held",
		done, len(rep.Results), took.Round(time.Second), made, dropped)
	logf(t, "MEASUREMENT %d answers: %d brought a clearance Google had not given that session before, "+
		"%d took %v or longer (longest %v)",
		answers, rhythm.Met, slow, slowAnswer, longest.Round(time.Second))
	if rhythm.Known {
		logf(t, "MEASUREMENT the run gets %.1f requests for each check it meets", rhythm.Between)
	}
	logf(t, "MEASUREMENT refusals Google judged: %s", refusals.tally())

	// The signal has to be there at all. A run of eight phrases on a list this
	// machine reaches badly meets checks — the whole reason the solver is
	// licensed — so nothing counted means the clearance never reached this
	// program, and the counter on the screen would be a lie told quietly.
	if answers == 0 {
		fatalf(t, "nothing was answered, so nothing can be said about the checks")
	}
	if rhythm.Met == 0 && slow == 0 {
		logf(t, "MEASUREMENT no check was met by either signal: a run that met none says nothing about either")
	}
	if rhythm.Met == 0 && slow > 0 {
		errorf(t, "%d answers waited %v or longer and not one of them brought a clearance: "+
			"the clock saw checks the clearance did not", slow, slowAnswer)
	}
}

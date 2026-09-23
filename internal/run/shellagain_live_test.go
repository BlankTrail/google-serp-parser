//go:build live

// SPDX-License-Identifier: MIT

package run

// What a second asking at the same address is worth.
//
// A page Google would not show — the shell, its check on the address handed
// back unsolved — used to condemn the address at once: the session was taken
// off it, landed on another one, and paid Google's check to be let in there.
// The chain costs a page, an address and a check, and it is the check that
// shows on the screen as a run being asked too fast.
//
// The other reading is that a shell is the solver's browser failing to open a
// connection through that exit this minute, and an exit that failed once may
// carry the next request. Then the session should stay where it is and ask
// again, and only a second shell condemns the address.
//
// Which reading is right is not a thing to argue about, so this asks the live
// list both ways and prints what each one cost. The arms run one after another
// on the same list, in the order A B B A, so a list that is getting worse or
// better as the hour goes on costs both arms the same.
//
// What is read off the numbers:
//   - shells met, and how many of them the second asking got past. With one
//     second chance, every address condemned is a shell that was asked again
//     and refused again: two shells and one rejection. So the shells that
//     passed on the second asking are the shells left over — S-2R of them.
//   - checks paid. This is the point: a session that stays where it is does not
//     arrive anywhere new, and does not pay to be let in.
//   - queries answered, and how long it took.

import (
	"context"
	"crypto/x509"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// shellAgainPhrases are asked once per round. None of them appears in the other
// live tests, so no answer can come from a cache one of them filled.
var shellAgainPhrases = []string{
	"монтаж кондиционеров", "изготовление ключей", "вывоз строительного мусора",
	"перетяжка мебели", "заточка инструмента", "чистка подушек",
	"поверка счётчиков воды", "установка дверей цена",
}

// shellArm is one of the two rules, and what a round under it cost.
type shellArm struct {
	name string
	// tries is how many second chances a page gets at the address it was
	// refused from. Nought is the rule as it was.
	tries int

	rounds   int
	queries  int
	done     int
	pages    int
	shells   int
	rejected int64
	checks   int
	asked    int
	made     int
	requests int64
	took     time.Duration
}

func TestLiveShellAgain_MeasuresWhatASecondAskingAtTheSameAddressBuys(t *testing.T) {
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
	hop := liveFirstHop(t)
	switch {
	case hop.Gateway != "":
		logf(t, "MEASUREMENT the ports go through a first hop: the gateway %s", hop.Gateway)
	case hop.Proxy != "":
		logf(t, "MEASUREMENT the ports go through a first hop: a SOCKS5 proxy")
	default:
		logf(t, "MEASUREMENT the ports go to their addresses directly")
	}

	arms := []*shellArm{
		{name: "condemned at once", tries: 0},
		{name: "asked again where it stands", tries: 1},
	}
	// A B B A: whatever the list is doing as the hour passes, each arm gets one
	// early round and one late one.
	for i, at := range []int{0, 1, 1, 0} {
		arm := arms[at]
		round := runShellArm(ctx, t, client, pre.CA, ups, hop, arm.tries)
		logf(t, "MEASUREMENT round %d, %s: %d of %d answered in %v, %d shells, %d addresses condemned, "+
			"%d checks, %d sessions, %d leases taken",
			i+1, arm.name, round.done, round.queries, round.took.Round(time.Second),
			round.shells, round.rejected, round.checks, round.made, round.requests)
		arm.add(round)
	}

	for _, arm := range arms {
		logf(t, "MEASUREMENT %s: %s", arm.name, arm.reading())
	}

	// The measurement stands on shells having happened at all. A list that met
	// none was not asked the question, and the two arms are then the same code
	// reporting the same thing twice.
	met := 0
	for _, arm := range arms {
		met += arm.shells
	}
	if met == 0 {
		t.Skip("no shell was met in any round, so neither rule was exercised")
	}
}

// add folds a round into the arm's totals.
func (a *shellArm) add(r shellArm) {
	a.rounds++
	a.queries += r.queries
	a.done += r.done
	a.pages += r.pages
	a.shells += r.shells
	a.rejected += r.rejected
	a.checks += r.checks
	a.asked += r.asked
	a.made += r.made
	a.requests += r.requests
	a.took += r.took
}

// reading is what the arm cost, in the terms the rule is chosen by.
func (a *shellArm) reading() string {
	s := fmt.Sprintf("%d of %d queries answered over %d rounds in %v (%.1f a minute); "+
		"%d shells met, %d addresses condemned; %d checks paid; %d leases taken; %d sessions made",
		a.done, a.queries, a.rounds, a.took.Round(time.Second),
		float64(a.done)/a.took.Minutes(), a.shells, a.rejected, a.checks, a.requests, a.made)
	if a.tries > 0 && a.shells > 0 {
		// Every address condemned under this rule took two shells; the rest
		// were asked again and answered.
		passed := a.shells - 2*int(a.rejected)
		s += fmt.Sprintf("; %d of the shells were got past by asking again where the session stood", passed)
	}
	return s
}

// runShellArm works one round of the phrases under one rule and reports what it
// cost. Each round gets a store, a keeper and a pool of its own: sessions from
// the round before would carry their clearance into it, and the first thing
// this measures is what being let in costs.
func runShellArm(ctx context.Context, t *testing.T, client *blanktrail.Client, ca *x509.CertPool,
	ups []blanktrail.Upstream, hop blanktrail.FirstHop, tries int) shellArm {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		fatalf(t, "store.Open: %v", err)
	}
	defer st.Close()

	k := sessions.NewKeeper(st)
	want := sessions.Want{Device: blanktrail.DeviceDesktop, Pause: time.Minute, UpTo: 2 * time.Minute}
	spec := blanktrail.DefaultPortSpec()
	spec.FirstHop = hop

	const threads = 2
	p, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: threads, PortsPerThread: 1, Spec: spec, CA: ca,
		Channels: []blanktrail.Channel{blanktrail.NewListChannel("list",
			blanktrail.NewStaticRotor(ups, blanktrail.WithRest(time.Hour)))},
		Sessions: true, Choose: k.Choose, AddressesPerRequest: 15,
		ReviveAfter: time.Minute, WaitForIdentity: true,
	})
	if err != nil {
		fatalf(t, "opening the pool: %v", err)
	}
	defer p.Close()

	counting := NewChallenges()
	widening := NewRamp(want.Longest())
	refusals := &asks{}
	var mu sync.Mutex
	answers := 0

	qs := make([]google.Query, len(shellAgainPhrases))
	for i, ph := range shellAgainPhrases {
		qs[i] = google.Query{Text: ph, Country: "ru", Language: "ru"}
	}

	began := time.Now()
	rep := (&Runner{Pool: p, Threads: threads, Keeper: k, Want: want,
		Challenges: counting, Ramp: widening, ShellTries: tries,
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
			mu.Unlock()
		}}).Run(ctx, Job{Queries: qs, Pages: 1, Tries: shellAgainTries})

	out := shellArm{tries: tries, queries: len(rep.Results), took: time.Since(began),
		requests: rep.Requests, shells: refusals.count("shell"),
		rejected: p.Stats().Rejections, checks: counting.Rhythm().Met,
		asked: answers, made: widening.Ramping().Made}
	for _, q := range rep.Results {
		if q.Err == nil && len(q.Pages) > 0 {
			out.done++
		}
		out.pages += len(q.Pages)
	}
	return out
}

// shellAgainTries is how many tries a phrase gets in the measurement below. It
// is high on purpose: the arithmetic there turns on every shell having had a
// second asking to spend, and a phrase that runs out of tries meets its last
// shell with none — which would be counted as an address condemned although it
// was never asked again.
const shellAgainTries = 9

func TestLiveShellAgain_MeasuresHowOftenAShellPassesOnTheSecondAsking(t *testing.T) {
	// Whether a shell is the address failing this minute or for good.
	//
	// It is the whole question the rule rests on, and the round above could
	// barely ask it: through a first hop the solver's browser opens its
	// connections and shells are rare, so a run meets two or three and any
	// share read off them is noise. So this one goes to the addresses directly,
	// whatever first hop the environment names — on this list that is the road
	// where the solver cannot connect at all and nearly every search comes back
	// a shell, which is the only place with enough of them to count.
	//
	// What is counted: every shell either had a second asking or was the second
	// asking. A second asking that came back a shell again condemns the address
	// and is the one thing that reports a rejection to the pool. So with S
	// shells and R rejections there were S-R second askings, R of them refused
	// the same way and the rest got past — and the share of the two is what says
	// whether the address deserved the second chance.
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

	round := runShellArm(ctx, t, client, pre.CA, ups, blanktrail.FirstHop{}, 1)
	logf(t, "MEASUREMENT going to the addresses directly: %d of %d queries answered in %v; "+
		"%d shells met, %d addresses condemned; %d checks paid; %d sessions made",
		round.done, round.queries, round.took.Round(time.Second),
		round.shells, round.rejected, round.checks, round.made)

	if round.shells == 0 {
		t.Skip("no shell was met, so there is nothing to say about what a second asking gets past")
	}
	again := round.shells - int(round.rejected)
	logf(t, "MEASUREMENT %d of the %d shells were asked again where the session stood: "+
		"%d came back a shell and the address was condemned, %d got past it",
		again, round.shells, round.rejected, again-int(round.rejected))
}

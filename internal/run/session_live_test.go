//go:build live

// SPDX-License-Identifier: MIT

package run

// How long one identity is worth asking, and what the address lookups cost it.
//
// Two questions, one shape of answer. The pause between two requests on one
// identity is measured — asked every two seconds it answered twelve requests
// before Google challenged it, every five about forty — but that measurement
// stops at the first challenge and says nothing about the rest of the
// identity's life. What a run needs to know is when to let an identity go: too
// early throws away the requests a solved challenge just paid for, too late
// spends the run on an identity that is challenged every time.
//
// The second question is what else is going through that identity. A search is
// paced; the lookups that read a hidden address are not paced at all — up to
// ten of them through one identity, four at a time, with no gap — and they go
// out through the same pool the searches come from. So the arms differ in that
// and nothing else.
//
//	go test -tags live -run TestLiveSession -timeout 60m ./internal/run/ -v
//
// It holds four identities and spends a few hundred requests.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// The run this models: a five-second gap on one identity, which is what the
// form offers, and a page of results per search.
const (
	sessionPace  = 5 * time.Second
	sessionAsks  = 45
	sessionArms  = 2 // identities per arm
	sessionSolve = 6 * time.Second
)

// phrases are asked in turn so an identity is not repeating one query, which is
// its own reason to be challenged and not the one being measured.
var phrases = []string{
	"купить кондиционер", "доставка пиццы", "ремонт квартиры",
	"детские кроватки", "туры в турцию", "автосервис рядом",
	"пластиковые окна", "стоматология цены", "аренда авто",
}

// ask is one search through the identity being measured.
type ask struct {
	at      time.Duration // since the identity was first asked
	took    time.Duration
	results int
	// lookups is how many hidden addresses were read after this search, and
	// lookedFor how long that took. Both are nought in the arm that does not.
	lookups   int
	lookedFor time.Duration
	err       error
}

func TestLiveSession_HowLongOneIdentityIsWorthAsking(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)

	type arm struct {
		name    string
		lookups bool
		lives   [][]ask
	}
	arms := []*arm{
		{name: "searches alone, paced"},
		{name: "searches and the address lookups", lookups: true},
	}

	// Both arms at the same minute, or the second one measures a different hour
	// of Google's day and reads as the arm it is not.
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, a := range arms {
		a.lives = make([][]ask, sessionArms)
		for i := 0; i < sessionArms; i++ {
			wg.Add(1)
			go func(a *arm, i int) {
				defer wg.Done()
				life := liveOneIdentity(ctx, t, pool, a.lookups)
				mu.Lock()
				a.lives[i] = life
				mu.Unlock()
			}(a, i)
		}
	}
	wg.Wait()

	for _, a := range arms {
		for i, life := range a.lives {
			say(t, a.name, i+1, life)
		}
	}
}

// liveOneIdentity holds one identity and asks it until it stops answering or
// the allowance is spent, returning what each ask cost.
func liveOneIdentity(ctx context.Context, t *testing.T, pool *blanktrail.Pool, lookups bool) []ask {
	// An identity that will not carry the first search is a dead address, not a
	// life to measure. Take another.
	var lease *blanktrail.Lease
	var session *google.Session
	var first google.SERP
	for try := 1; try <= 20; try++ {
		l, err := pool.Acquire(ctx)
		if err != nil {
			return nil
		}
		cl := l.Client()
		s := google.NewSession(cl.Transport)
		s.Client.Timeout = cl.Timeout
		serp, err := s.Search(ctx, google.Query{Text: phrases[0], Country: "ru", Language: "ru"})
		if err != nil {
			_ = l.Reject(ctx)
			l.Release()
			continue
		}
		lease, session, first = l, s, serp
		break
	}
	if lease == nil {
		return nil
	}
	defer lease.Release()

	began := time.Now()
	life := []ask{{at: 0, took: 0, results: len(first.Results)}}
	if lookups {
		n, took := readTheAddresses(ctx, lease, &first)
		life[0].lookups, life[0].lookedFor = n, took
	}

	for i := 1; i < sessionAsks; i++ {
		if err := pool.Sleep(ctx, sessionPace); err != nil {
			break
		}
		at := time.Since(began)
		began2 := time.Now()
		serp, err := session.Search(ctx, google.Query{
			Text: phrases[i%len(phrases)], Country: "ru", Language: "ru"})
		one := ask{at: at, took: time.Since(began2), results: len(serp.Results), err: err}
		if err == nil && lookups {
			one.lookups, one.lookedFor = readTheAddresses(ctx, lease, &serp)
		}
		life = append(life, one)
		if err != nil {
			// Three refusals in a row is an identity that is finished, and going
			// on measures how long a dead identity stays dead.
			if len(life) >= 3 && life[len(life)-2].err != nil && life[len(life)-3].err != nil {
				break
			}
		}
	}
	return life
}

// readTheAddresses reads the hidden addresses of one page through the identity
// that captured it, the way the run reads them: four at a time, no pause.
func readTheAddresses(ctx context.Context, lease *blanktrail.Lease, serp *google.SERP) (int, time.Duration) {
	cl := lease.Client()
	resolver := google.NewResolver(cl.Transport)
	resolver.Client.Timeout = cl.Timeout
	began := time.Now()
	rep := resolver.ResolveAll(ctx, serp, resolveWorkers)
	return rep.Attempted, time.Since(began)
}

// say writes down one identity's life: every ask, and the readings a policy
// would be chosen from.
func say(t *testing.T, arm string, n int, life []ask) {
	t.Helper()
	if len(life) == 0 {
		logf(t, "MEASUREMENT %s, identity %d: no identity carried the first search", arm, n)
		return
	}

	var answered, solved, refused, lookups int
	var firstSolve = -1
	var firstRefusal = -1
	var series []string
	for i, one := range life {
		switch {
		case one.err != nil:
			refused++
			if firstRefusal < 0 {
				firstRefusal = i + 1
			}
			series = append(series, "×")
		default:
			answered++
			if one.took >= sessionSolve {
				solved++
				if firstSolve < 0 {
					firstSolve = i + 1
				}
				series = append(series, fmt.Sprintf("C%.0f", one.took.Seconds()))
			} else {
				series = append(series, fmt.Sprintf("%.1f", one.took.Seconds()))
			}
		}
		lookups += one.lookups
	}

	logf(t, "MEASUREMENT %s, identity %d: %d asks, %d answered, %d of them past %v, %d refused; %d addresses looked up",
		arm, n, len(life), answered, solved, sessionSolve, refused, lookups)
	logf(t, "MEASUREMENT %s, identity %d: first slow answer at ask %d, first refusal at ask %d",
		arm, n, firstSolve, firstRefusal)
	logf(t, "MEASUREMENT %s, identity %d: seconds per ask, C for a slow one, × for a refusal: %s",
		arm, n, strings.Join(series, " "))

	// What a rule of "let it go after k asks" would have been worth, read off
	// this life rather than modelled: how many answers it keeps and what each
	// of them cost in seconds of the identity's time.
	for _, k := range []int{5, 10, 15, 20, 30, 45} {
		if k > len(life) {
			break
		}
		var kept int
		var spent time.Duration
		for i := 0; i < k; i++ {
			if life[i].err == nil {
				kept++
			}
			spent += life[i].took + life[i].lookedFor
		}
		if kept == 0 {
			continue
		}
		logf(t, "MEASUREMENT %s, identity %d: letting go after %2d asks keeps %2d answers at %.1fs of identity time each",
			arm, n, k, kept, spent.Seconds()/float64(kept))
	}
}

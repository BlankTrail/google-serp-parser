//go:build live

// SPDX-License-Identifier: MIT

package run

// A job several pages deep on the live service, one port a thread, through the
// program's own sessions.
//
// What it is here to show is the whole of the arrangement working against
// Google rather than against a stand-in: that the deeper pages are asked for at
// the addresses the pages before them carried — tags and all, as Google issued
// them to that session — that a thread with one port does not stand still while
// its sessions rest, and that the queries come back with the depth they were
// asked for.
//
// The addresses it runs on are the list as it is: wingate exits that come and
// go, through the first hop when GSERP_FIRST_HOP names one. A page asked for at
// an address of our own making would be answered too — Google serves it — so
// nothing but the address of the request can tell the two apart, and that is
// what this counts.

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// deepLivePhrases are asked to a depth. None of them appears in the other live
// tests, so no answer can come from a cache one of them filled.
var deepLivePhrases = []string{
	"купить велосипед", "сварочный аппарат", "отдых в сочи",
	"укладка плитки", "кофемашина для дома", "корм для кошек",
}

// deepLivePages is how deep each phrase is taken. Five is past the point where
// the pages are all alike: Google's second page is often served from the same
// answer as the first, and the fifth is not.
const deepLivePages = 5

// carriedPages remembers every address a page offered as its next, and every
// address a request actually went to. The two together say whether the walk
// asked where it was told to.
type carriedPages struct {
	mu sync.Mutex
	// offered are the addresses the pages carried, exactly as they carried them.
	offered map[string]bool
	// asked is every search that went out, in order.
	asked []string
	// deep are the ones that went to an address a page had offered, and rebuilt
	// the ones that reached for a deeper page by some address of their own.
	deep, rebuilt int
	// rebuiltAt keeps one example, for a failure that has to say what went out.
	rebuiltAt string
}

func (c *carriedPages) offer(next string) {
	if next == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offered[next] = true
}

// ask counts one outgoing search. A request carrying an offset is one for a
// page after the first: either it is an address a page offered, or the walk
// built it, and there is nothing else it can be.
func (c *carriedPages) ask(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, u)
	switch {
	case c.offered[u]:
		c.deep++
	case deeperThanTheFirst(u):
		c.rebuilt++
		if c.rebuiltAt == "" {
			c.rebuiltAt = u
		}
	}
}

// deeperThanTheFirst says whether an address asks for a page past the first.
func deeperThanTheFirst(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	start := parsed.Query().Get("start")
	return start != "" && start != "0"
}

func TestLiveDeepPages_AreAskedForAtTheAddressesThePagesCarry(t *testing.T) {
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

	book := newSessionBook(st)
	k := sessions.NewKeeper(book)
	want := sessions.Want{Device: blanktrail.DeviceDesktop, Pause: time.Minute}
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

	// Two threads, one port each: the arrangement this is all for. Held the old
	// way a thread would sleep the pause on its only port, and a run of six
	// queries five pages deep would take thirty rests end to end.
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
	// The pool is not paced here. In a pool of sessions the rest between two
	// requests on one session is the keeper's to hold, and a thread that walks
	// takes no pause of its own: it works another session while this one rests.

	carried := &carriedPages{offered: map[string]bool{}}
	refusals := &asks{}
	var mu sync.Mutex
	asking := time.Duration(0) // how long the threads spent with a request in the air

	qs := make([]google.Query, len(deepLivePhrases))
	for i, ph := range deepLivePhrases {
		qs[i] = google.Query{Text: ph, Country: "ru", Language: "ru"}
	}

	began := time.Now()
	rep := (&Runner{Pool: p, Threads: threads, Keeper: k, Want: want, Watch: func(s Step) {
		if s.Stage != StageAsk {
			return
		}
		mu.Lock()
		asking += s.Took
		mu.Unlock()
		if s.Err != nil {
			refusals.note(s.Err)
		}
	}}).Run(ctx, Job{
		Queries: qs, Pages: deepLivePages,
		Asking:   carried.ask,
		Captured: func(page google.SERP) { carried.offer(page.NextPage) },
	})
	took := time.Since(began)

	// What came back.
	pages, answered, shallow := 0, 0, 0
	depths := make([]string, 0, len(rep.Results))
	for _, q := range rep.Results {
		pages += len(q.Pages)
		if q.Err == nil && len(q.Pages) > 0 {
			answered++
		}
		if len(q.Pages) == 1 {
			shallow++
		}
		depths = append(depths, fmt.Sprintf("%d", len(q.Pages)))
	}
	made, dropped := book.counts()
	all, _ := k.Count()
	idle := time.Duration(threads)*took - asking

	logf(t, "MEASUREMENT %d of %d queries answered, %d pages in %v (depths %s, %d stopped at one page)",
		answered, len(rep.Results), pages, took.Round(time.Second), strings.Join(depths, "/"), shallow)
	carried.mu.Lock()
	logf(t, "MEASUREMENT %d searches went out: %d at an address a page carried, %d built for a deeper page",
		len(carried.asked), carried.deep, carried.rebuilt)
	deep, rebuilt, rebuiltAt := carried.deep, carried.rebuilt, carried.rebuiltAt
	carried.mu.Unlock()
	logf(t, "MEASUREMENT %d sessions made, %d given up, %d known at the end; %d ports for %d threads",
		made, dropped, all, threads, threads)
	logf(t, "MEASUREMENT the threads were asking for %v of %v thread-time; %v (%.0f%%) was spent waiting for a session",
		asking.Round(time.Second), (time.Duration(threads) * took).Round(time.Second),
		idle.Round(time.Second), 100*float64(idle)/float64(time.Duration(threads)*took))
	logf(t, "MEASUREMENT refusals Google judged: %s", refusals.tally())

	// The walk asked where it was told to. A page reached for by an address of
	// our own making is the failure this whole arrangement exists to avoid: the
	// tags Google issued with the page — ei, sa, ved — name the search it showed
	// this session, and an address built from the query and an offset carries
	// none of them.
	if rebuilt != 0 {
		errorf(t, "%d searches reached for a deeper page at an address no page offered, e.g. %s", rebuilt, rebuiltAt)
	}
	// And it went deep at all. A run where every query stopped on its first page
	// would satisfy the count above by having nothing to count.
	if deep == 0 || pages <= answered {
		fatalf(t, "nothing went deeper than one page: %d pages over %d answered queries, %d deep searches",
			pages, answered, deep)
	}
	if answered < len(rep.Results) {
		errorf(t, "%d queries came back with nothing", len(rep.Results)-answered)
	}
}

// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// answeringOrigin serves a result page to every search. An estimate sends
// nothing, but the pool it is asked about is the pool the job would run on, so
// the tests build the same one the rest of this package runs against.
func answeringOrigin(t *testing.T) *origin {
	t.Helper()
	return newOrigin(t, func(*http.Request, int) string { return serpBody("example.com") })
}

// tenSecondGap widens the pause the pool keeps between two requests on one
// port, so a floor measured in it is a number a reader can check by hand.
func tenSecondGap(c *blanktrail.PoolConfig) { c.Cooldown = 10 * time.Second }

func TestEstimate_CountsEveryPageOfEveryQuery(t *testing.T) {
	f := poolFacing(t, answeringOrigin(t).addr(), 4)

	r := &Runner{Pool: f.Pool, Threads: 2}
	est := r.Estimate(Job{Queries: make([]google.Query, 100), Pages: 3})

	if est.Queries != 100 || est.Pages != 3 {
		t.Errorf("Queries=%d Pages=%d, want 100 and 3", est.Queries, est.Pages)
	}
	if est.Searches != 300 {
		t.Errorf("Searches=%d, want 300", est.Searches)
	}
	if est.Warmups != 4 {
		t.Errorf("Warmups=%d, want one for each port the work reaches", est.Warmups)
	}
	if est.Requests != 304 {
		t.Errorf("Requests=%d, want the searches and the front-page visits together", est.Requests)
	}
	if est.Ports != 4 {
		t.Errorf("Ports=%d, want the pool's four", est.Ports)
	}
}

func TestEstimate_VisitsTheFrontPageOnlyForThePortsTheWorkReaches(t *testing.T) {
	// Two queries never reach the other two ports. Counting a visit for a port
	// nothing is sent through bills a user for requests nobody makes.
	f := poolFacing(t, answeringOrigin(t).addr(), 4)

	r := &Runner{Pool: f.Pool, Threads: 4}
	est := r.Estimate(Job{Queries: make([]google.Query, 2), Pages: 3})

	if est.Warmups != 2 {
		t.Errorf("Warmups=%d, want one per query while there are fewer queries than ports", est.Warmups)
	}
	if est.Requests != 8 {
		t.Errorf("Requests=%d, want six searches and two front-page visits", est.Requests)
	}
}

func TestEstimate_TheCeilingRetakesTheRefusedPageRatherThanTheWholeWalk(t *testing.T) {
	// A carried query resumes at the page it stopped on, so moving it on costs
	// that one page again and a front-page visit for the identity it lands on.
	// Charging the whole depth to every identity would size an address budget at
	// several times the work.
	f := poolFacing(t, answeringOrigin(t).addr(), 4)

	r := &Runner{Pool: f.Pool, Threads: 2}
	est := r.Estimate(Job{Queries: make([]google.Query, 10), Pages: 2, Tries: 4})

	// Per query: two pages, three of them taken again, and a visit for each of
	// the four identities.
	if est.MaxRequests != 90 {
		t.Errorf("MaxRequests=%d, want 90", est.MaxRequests)
	}
}

func TestEstimate_TimesTheFloorByThePortsAndNotByTheThreads(t *testing.T) {
	// Throughput is bounded by how often a port may be asked again, not by how
	// many threads ask. A hundred threads over four ports is still four ports.
	f := poolFacing(t, answeringOrigin(t).addr(), 4, tenSecondGap)

	r := &Runner{Pool: f.Pool, Threads: 100}
	est := r.Estimate(Job{Queries: make([]google.Query, 40), Pages: 1})

	// Ten queries fall to each port, with nine gaps between them.
	if want := 90 * time.Second; est.Floor != want {
		t.Errorf("Floor=%v, want %v", est.Floor, want)
	}
	if est.Cooldown != 10*time.Second {
		t.Errorf("Cooldown=%v, want the pool's own", est.Cooldown)
	}
}

func TestEstimate_TheFloorDoesNotGrowWithTheDepthOfAWalk(t *testing.T) {
	// A walk of any depth is one query on one identity, so taking every query
	// five pages deep buys pages rather than time. An estimate that multiplied
	// the floor by the depth would quote five hours for one hour of work.
	f := poolFacing(t, answeringOrigin(t).addr(), 4, tenSecondGap)

	r := &Runner{Pool: f.Pool, Threads: 4}
	est := r.Estimate(Job{Queries: make([]google.Query, 40), Pages: 5})

	if want := 90 * time.Second; est.Floor != want {
		t.Errorf("Floor=%v, want %v", est.Floor, want)
	}
}

func TestEstimate_AJobSmallerThanThePoolWaitsOnNothing(t *testing.T) {
	// Three queries over four ports never ask any port twice, so nothing in the
	// pacing holds them up and the whole cost is the network.
	f := poolFacing(t, answeringOrigin(t).addr(), 4, tenSecondGap)

	r := &Runner{Pool: f.Pool, Threads: 2}
	est := r.Estimate(Job{Queries: make([]google.Query, 3), Pages: 2})

	if est.Floor != 0 {
		t.Errorf("Floor=%v, want none", est.Floor)
	}
}

func TestEstimate_AJobThatNamesNoDepthIsCountedAtOnePage(t *testing.T) {
	// Job leaves both at zero to mean the usual, and an estimate that read the
	// zeroes literally would report a job of no pages and no retries at all.
	f := poolFacing(t, answeringOrigin(t).addr(), 4)

	r := &Runner{Pool: f.Pool, Threads: 2}
	est := r.Estimate(Job{Queries: make([]google.Query, 3)})

	if est.Pages != 1 || est.Searches != 3 {
		t.Errorf("Pages=%d Searches=%d, want 1 and 3", est.Pages, est.Searches)
	}
	// Per query: one page, two more taken again, and a visit for each of the
	// three identities.
	if est.MaxRequests != 18 {
		t.Errorf("MaxRequests=%d, want 18", est.MaxRequests)
	}
}

func TestEstimate_AnEmptyJobCostsNothing(t *testing.T) {
	f := poolFacing(t, answeringOrigin(t).addr(), 4, tenSecondGap)

	r := &Runner{Pool: f.Pool, Threads: 2}
	est := r.Estimate(Job{})

	if est.Searches != 0 || est.Warmups != 0 || est.Requests != 0 || est.MaxRequests != 0 {
		t.Errorf("Searches=%d Warmups=%d Requests=%d MaxRequests=%d, want an empty job to cost nothing",
			est.Searches, est.Warmups, est.Requests, est.MaxRequests)
	}
	if est.Floor != 0 {
		t.Errorf("Floor=%v, want none", est.Floor)
	}
}

func TestEstimate_MatchesWhatACleanRunActuallyCosts(t *testing.T) {
	// The best case is a claim about the run, not about arithmetic, so it is
	// checked against one: six queries answered first time over three ports.
	o := answeringOrigin(t)
	f := poolFacing(t, o.addr(), 3)

	r := &Runner{Pool: f.Pool, Threads: 2}
	j := Job{Queries: usQueries(6)}

	est := r.Estimate(j)
	rep := r.Run(context.Background(), j)

	if rep.Done != 6 {
		t.Fatalf("Done=%d, want 6 (failed %d)", rep.Done, rep.Failed)
	}
	if got := o.searches.Load(); got != int64(est.Searches) {
		t.Errorf("%d searches, estimated %d", got, est.Searches)
	}
	if got := o.homes.Load(); got != int64(est.Warmups) {
		t.Errorf("%d visits to the front page, estimated %d", got, est.Warmups)
	}
	if got := o.searches.Load() + o.homes.Load(); got != int64(est.Requests) {
		t.Errorf("%d requests left the machine, estimated %d", got, est.Requests)
	}
}

func TestEstimate_AnsweredWithoutAPortToRunOn(t *testing.T) {
	// A user asking what a job costs after the pool has been closed gets an
	// answer, not a panic: the counts still hold and only the time is unknowable.
	f := poolFacing(t, answeringOrigin(t).addr(), 4)
	if err := f.Pool.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r := &Runner{Pool: f.Pool, Threads: 2}
	est := r.Estimate(Job{Queries: make([]google.Query, 10), Pages: 2})

	if est.Ports != 0 {
		t.Errorf("Ports=%d, want none", est.Ports)
	}
	if est.Searches != 20 || est.Requests != 20 {
		t.Errorf("Searches=%d Requests=%d, want 20 and 20", est.Searches, est.Requests)
	}
	if est.Floor != 0 {
		t.Errorf("Floor=%v, want none", est.Floor)
	}
}

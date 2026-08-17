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

func TestEstimateFor_AnswersBeforeThereIsAPoolToAskAbout(t *testing.T) {
	// The first thing a user does with ten thousand queries is ask what it will
	// cost, and that answer must not require opening the ports first: the cost
	// of finding out would then be a share of the cost being asked about.
	est := EstimateFor(Job{Queries: make([]google.Query, 100), Pages: 3}, 4, 10*time.Second)

	if est.Searches != 300 || est.Warmups != 4 || est.Requests != 304 {
		t.Errorf("Searches=%d Warmups=%d Requests=%d, want 300, 4 and 304",
			est.Searches, est.Warmups, est.Requests)
	}
	if est.Ports != 4 || est.Cooldown != 10*time.Second {
		t.Errorf("Ports=%d Cooldown=%v, want the four and the ten seconds it was given",
			est.Ports, est.Cooldown)
	}
	if est.Floor != 240*time.Second {
		t.Errorf("Floor=%v, want 240s — twenty-five queries a port, twenty-four gaps", est.Floor)
	}
}

func TestEstimate_AsksThePoolItWillRunOn(t *testing.T) {
	// The method exists so a caller holding a pool does not have to take it
	// apart to describe it. Reading the size and the gap from somewhere else
	// would quote a job on a pool it is not going to run on.
	f := poolFacing(t, answeringOrigin(t).addr(), 4, tenSecondGap)

	r := &Runner{Pool: f.Pool, Threads: 2}
	j := Job{Queries: make([]google.Query, 100), Pages: 3}
	want := EstimateWith(j, f.Pool.Size(), r.Threads, f.Pool.Cooldown(), MeasuredPace)
	if got := r.Estimate(j); got != want {
		t.Errorf("Estimate=%+v, want the same as EstimateWith on the pool's own numbers %+v", got, want)
	}
}

func TestEstimateFor_AssumesEnoughThreadsToKeepEveryPortBusy(t *testing.T) {
	// EstimateFor is never told a thread count. Assuming one thread would quote
	// eight times the time for a pool of eight, and a user sizing a pool would
	// read it as the ports having bought them nothing.
	j := Job{Queries: make([]google.Query, 20), Pages: 2}
	want := EstimateWith(j, 8, 8, 7*time.Second, MeasuredPace)
	if got := EstimateFor(j, 8, 7*time.Second); got != want {
		t.Errorf("EstimateFor=%+v, want %+v", got, want)
	}
}

func TestEstimateWith_CountsGettingToEachPortOnceAndEveryAnswerAfterThat(t *testing.T) {
	// Twenty queries two pages deep over eight ports: eight ports to reach,
	// twenty first answers, twenty later ones, over four threads.
	p := Pace{ReachPort: 60 * time.Second, FirstRequest: 10 * time.Second, LaterRequest: 2 * time.Second}
	est := EstimateWith(Job{Queries: make([]google.Query, 20), Pages: 2}, 8, 4, 0, p)

	if want := 180 * time.Second; est.Expected != want {
		t.Errorf("Expected=%v, want %v — (8×60 + 20×10 + 20×2) over four threads", est.Expected, want)
	}
}

func TestEstimateWith_PaysToReachOnlyThePortsTheWorkActuallyUses(t *testing.T) {
	// Three queries never reach the other five ports. Charging for those quotes
	// a cost against work nobody does — the same reason the warm-ups are counted
	// per port reached rather than per port held.
	p := Pace{ReachPort: 60 * time.Second, FirstRequest: 10 * time.Second, LaterRequest: 2 * time.Second}
	est := EstimateWith(Job{Queries: make([]google.Query, 3), Pages: 1}, 8, 4, 0, p)

	if want := 70 * time.Second; est.Expected != want {
		t.Errorf("Expected=%v, want %v — (3×60 + 3×10) over the three threads that have work", est.Expected, want)
	}
}

func TestEstimateWith_AddingThreadsPastThePortCountBuysNothingAndTooFewCostsTime(t *testing.T) {
	// A port answers one thread at a time, so threads beyond the ports wait. But
	// a job given fewer threads than ports leaves ports idle, and an estimate
	// blind to that quotes a run the caller has not asked for.
	p := Pace{ReachPort: 20 * time.Second, FirstRequest: 10 * time.Second, LaterRequest: 5 * time.Second}
	j := Job{Queries: make([]google.Query, 8), Pages: 1}

	crowded := EstimateWith(j, 4, 100, 0, p)
	if want := 40 * time.Second; crowded.Expected != want {
		t.Errorf("Expected=%v with a hundred threads over four ports, want %v", crowded.Expected, want)
	}
	starved := EstimateWith(j, 4, 1, 0, p)
	if want := 160 * time.Second; starved.Expected != want {
		t.Errorf("Expected=%v with one thread over four ports, want %v", starved.Expected, want)
	}
}

func TestEstimateWith_NeverQuotesLessThanThePacingItIsHeldTo(t *testing.T) {
	// The two bounds are on the same run. A job whose requests are cheap is still
	// held to the gap between two requests on one port, and quoting under the
	// floor would contradict the line printed beside it.
	est := EstimateWith(Job{Queries: make([]google.Query, 100), Pages: 3}, 4, 4, 10*time.Second, Pace{})

	if est.Floor != 240*time.Second {
		t.Fatalf("Floor=%v, want 240s", est.Floor)
	}
	if est.Expected != est.Floor {
		t.Errorf("Expected=%v, want the floor %v when a request is quoted as free", est.Expected, est.Floor)
	}
}

func TestEstimateWith_AJobWithNoPortsToRunOnIsNotQuotedATime(t *testing.T) {
	// Dividing the work over no threads at all is the one arithmetic in here that
	// can crash, and a user asking about a closed pool gets counts, not a panic.
	est := EstimateWith(Job{Queries: make([]google.Query, 10), Pages: 2}, 0, 0, 10*time.Second, MeasuredPace)

	if est.Expected != 0 || est.Floor != 0 {
		t.Errorf("Expected=%v Floor=%v, want neither quoted without a port to run on", est.Expected, est.Floor)
	}
}

func TestMeasuredPace_StaysWithinAFactorOfTwoOfTheRunsItWasCheckedAgainst(t *testing.T) {
	// The three runs the pace was checked against: twenty queries two pages deep,
	// eight ports, four threads, a seven-second gap. A model fitted to three
	// points is a lookup table, so this pins the agreement it happens to have
	// rather than claiming any more than that.
	est := EstimateWith(Job{Queries: make([]google.Query, 20), Pages: 2}, 8, 4, 7*time.Second, MeasuredPace)

	if want := 14 * time.Second; est.Floor != want {
		t.Errorf("Floor=%v, want the %v those runs were quoted", est.Floor, want)
	}
	for _, took := range []time.Duration{
		6*time.Minute + 44*time.Second,
		8*time.Minute + 23*time.Second,
		11*time.Minute + 22*time.Second,
	} {
		if est.Expected < took/2 || est.Expected > 2*took {
			t.Errorf("Expected=%v against a run of %v, which is more than a factor of two out", est.Expected, took)
		}
	}
}

func TestEstimateSize_CostsAJobKnownOnlyByItsSizeExactlyAsOneKnownByItsQueries(t *testing.T) {
	// A screen following a running job knows how many queries the job holds and
	// not what they say. Costing it through EstimateWith would mean building a
	// list of that many empty queries every time the screen is drawn, so the same
	// arithmetic has a door that takes the count — and the two doors have to lead
	// to the same room, or a reader is quoted one number before the job and
	// another during it.
	//
	// The four numbers are all different, so an estimate that took the depth for
	// the thread count, or the ports for either, cannot answer the same as one
	// that did not.
	const (
		queries  = 40
		pages    = 3
		ports    = 7
		threads  = 2
		cooldown = 9 * time.Second
	)
	whole := EstimateWith(Job{Queries: make([]google.Query, queries), Pages: pages},
		ports, threads, cooldown, MeasuredPace)
	sized := EstimateSize(queries, pages, ports, threads, cooldown, MeasuredPace)

	if sized != whole {
		t.Errorf("a job of %d queries costs %+v by its size and %+v by its queries",
			queries, sized, whole)
	}
	if sized.Expected == 0 {
		t.Fatal("the estimate came to nothing at all, so nothing here was compared")
	}
	// The lanes and the ports enter the arithmetic differently, so an estimate
	// that had them the wrong way round would answer differently. Without this
	// the test above passes on a pair of functions that agree and are both wrong.
	if swapped := EstimateSize(queries, pages, threads, ports, cooldown, MeasuredPace); swapped == sized {
		t.Error("the ports and the threads are interchangeable in this estimate, so neither is being read")
	}
}

// SPDX-License-Identifier: MIT

package run

import "time"

// Cost is one measured per-request cost: the quickest it was ever seen to be,
// and the middle of the range it was seen in.
//
// Both are kept because they answer different questions. The middle is what a
// job is likely to cost; the quickest is what no run of this shape has ever
// beaten, which is the only honest way to say "not sooner than". A single
// number cannot be both, and an estimate built on the middle alone has no floor
// to offer but the pacing — which counts none of the network and is wrong by
// two orders of magnitude for it.
//
// Fast left at nought means the cost was never measured at its quick end, and
// the floor built on it simply does not count that term.
type Cost struct {
	Fast    time.Duration
	Typical time.Duration
}

// Pace is what a request costs, measured rather than assumed.
type Pace struct {
	ReachPort    Cost // getting to a port that answers
	FirstRequest Cost // that port's first answer
	LaterRequest Cost // an answer from a port that has already answered
}

// MeasuredPace is the pace of one live run: one list of addresses, one country,
// one day. Every figure in it is the middle of a measured range, and every range
// was wide.
//
//   - Reaching a port that answered at all took between 27s and 9m10s across six
//     series. A factor of twenty, and the estimate inherits all of it: this term
//     dominates every job that is not enormous.
//   - A port's first answer took 4.7s to 6.7s in the series that behaved. Two did
//     not — one took 17.5s, one 3m21s — and those are left out of the midpoint
//     rather than averaged into it, so a bad day is not quoted as the normal one.
//   - A later answer from a port that had already answered took a median of 1.4s
//     to 2.7s.
//
// These are measurements of one run and not constants of the world. Another list,
// another country or another day gives other numbers, and a caller who has
// measured their own should pass their own Pace to EstimateWith. The arithmetic
// built on this was checked against three runs of the same job and landed within
// a factor of two of each; three points do not make it right anywhere else.
// Each Fast below is the quick end of the range named above it, not a separate
// measurement: the fastest a port was reached, the fastest a first answer came
// back, the fastest a later one did. Nothing has been rounded down to make a
// floor look better.
var MeasuredPace = Pace{
	ReachPort: Cost{
		Fast:    27 * time.Second,
		Typical: (27*time.Second + 9*time.Minute + 10*time.Second) / 2,
	},
	FirstRequest: Cost{
		Fast:    4700 * time.Millisecond,
		Typical: (4700*time.Millisecond + 6700*time.Millisecond) / 2,
	},
	LaterRequest: Cost{
		Fast:    1400 * time.Millisecond,
		Typical: (1400*time.Millisecond + 2700*time.Millisecond) / 2,
	},
}

// Estimate is what a job will cost, worked out before it is started.
type Estimate struct {
	Queries int
	Pages   int
	// Searches is every query taken to its full depth once, which is the work
	// the job is actually for. A walk that ends early because the page said the
	// results ran out costs less.
	Searches int
	// Warmups is the home-page visit a session pays before its first search. It
	// is one per port rather than one per query, because the session is kept for
	// as long as the identity it belongs to, and a job with fewer queries than
	// ports never reaches the rest of them.
	//
	// It is a ceiling on that visit and not a promise of it: a lease goes to an
	// identity that has answered before one that never has, so a job with more
	// ports than threads keeps returning to the same few and may never touch the
	// rest. What it quotes is the pool it was given.
	Warmups int
	// Requests is what the two above add up to: what will actually leave the
	// machine in the best case. Report.Requests counts something else — the
	// identities a job took — so the two are not the same number and a run that
	// lands on it exactly has not confirmed this one.
	Requests int
	// MaxRequests is the ceiling: every query taken to its depth, the one page
	// each move to another identity makes it take again, and a home-page visit
	// for every identity it is carried to. A user sizing an address budget needs
	// this one, not the happy one.
	MaxRequests int

	Ports int
	// Cooldown is the gap the pool keeps between two requests on one port.
	Cooldown time.Duration
	// Floor is the shortest this run could go: the later of two bounds, since
	// both hold at once.
	//
	// The first is the pacing. One query holds one identity however deep it is
	// taken, so a port carries one query per gap, and the busiest port waits out
	// one gap between each query and the next.
	//
	// The second is the work itself at the quickest each request was ever
	// measured at. This is the half that used to be missing, and its absence was
	// not a rounding error: a live run of ten queries over eight ports was quoted
	// a floor of two seconds and took 6m15s — out by a factor of 187, sitting on
	// the same screen as an expectation that was out by 1.6. The pacing alone is
	// a true bound and a useless one, because nothing in it counts the network.
	//
	// It is still a floor and still not a promise: it counts no retries, no
	// challenge that has to be solved and no walk that ends early, and the quick
	// ends behind it are the quickest **seen**, not a speed the world guarantees.
	// A run that comes in under it means the pace should be measured again.
	Floor time.Duration
	// Expected is how long the job is likely to take, and it is the number to
	// read. The floor is bound by the pacing; a real run is bound by getting to
	// a port that answers and then by the answers themselves, which is why the
	// three runs behind MeasuredPace took 6m44s, 8m23s and 11m22s against a floor
	// of fourteen seconds.
	//
	// It is built from measured per-request cost, so it is only ever as good as
	// the measurement: MeasuredPace comes from one list on one day, and the range
	// its middle was taken from spans a factor of twenty. Expected is a quantity
	// to plan around, not one to check a run against. It is never below Floor,
	// because both bound the same run.
	Expected time.Duration
}

// Estimate works out what a job will cost before it is started. A user
// launching ten thousand queries has the right to know how many requests that
// is, and roughly how long, before pressing the button rather than an hour
// later.
//
// Throughput is bounded by the ports and the gap between two requests on one
// port. The thread count enters as a cap and never as a multiplier: a hundred
// threads over four ports is still four ports, so adding threads past the ports
// buys nothing, while a job given fewer threads than ports leaves ports idle and
// takes correspondingly longer.
func (r *Runner) Estimate(j Job) Estimate {
	return EstimateWith(j, r.Pool.Size(), r.Threads, r.Pool.Cooldown(), MeasuredPace)
}

// EstimateFor works out the same cost for a pool that has not been opened yet.
//
// It takes the two numbers the estimate turns on rather than a pool, because
// the question comes up before there is one: a user asking what ten thousand
// queries will cost should not have to open the ports to find out, and the
// answer is the same either way.
func EstimateFor(j Job, ports int, cooldown time.Duration) Estimate {
	// Nobody named a thread count here, so the estimate assumes there are enough
	// of them to keep every port busy. Assuming one instead would quote eight
	// times the time for a pool of eight and tell a user sizing that pool that
	// the ports bought them nothing.
	return EstimateWith(j, ports, ports, cooldown, MeasuredPace)
}

// EstimateWith works out the same cost against a pace the caller has measured.
//
// The default entry points pass MeasuredPace, which came off one list on one
// day. Anyone who has timed their own run has better numbers than that, and this
// is where they put them.
func EstimateWith(j Job, ports, threads int, cooldown time.Duration, p Pace) Estimate {
	return estimate(len(j.Queries), j.Pages, j.Tries, ports, threads, cooldown, p)
}

// EstimateSize works out the same cost for a job named by how big it is rather
// than by what it holds.
//
// The screen that follows a running job knows how many queries the job has and
// not what any of them says. Going through EstimateWith would mean building a
// list of that many empty queries to be counted and thrown away, every time the
// screen is drawn, which on a job of a million queries is tens of megabytes of
// nothing per reader.
//
// The number of identities one query may be taken to is left at the default,
// because a job read back out of a history carries no other answer.
func EstimateSize(queries, pages, ports, threads int, cooldown time.Duration, p Pace) Estimate {
	return estimate(queries, pages, 0, ports, threads, cooldown, p)
}

// estimate is the arithmetic both doors lead to. Keeping it in one place is what
// makes the number a reader is quoted before a job starts the same number they
// are held to while it runs.
func estimate(queries, wanted, wantedTries, ports, threads int, cooldown time.Duration, p Pace) Estimate {
	pages := wanted
	if pages < 1 {
		pages = 1
	}
	tries := wantedTries
	if tries < 1 {
		tries = defaultTries
	}

	est := Estimate{
		Queries:  queries,
		Pages:    pages,
		Searches: queries * pages,
		Ports:    ports,
		Cooldown: cooldown,
	}

	est.Warmups = min(est.Queries, est.Ports)
	est.Requests = est.Searches + est.Warmups

	// A query moved to another identity resumes at the page it stopped on, so
	// each move costs that page again rather than the depth again, and a fresh
	// identity pays the home-page visit once more. Charging every page to every
	// identity would quote several times the work.
	est.MaxRequests = est.Queries * (pages + 2*tries - 1)

	if est.Queries == 0 || est.Ports == 0 {
		return est
	}
	// The busiest port carries this many queries, and waits out the gap between
	// each of them and the next. The last query is not followed by a wait, so the
	// job is one gap shorter than it has queries to place on that port.
	perPort := (est.Queries + est.Ports - 1) / est.Ports
	paced := time.Duration(perPort-1) * est.Cooldown

	// That work is shared between the threads, but a thread with no port to run
	// on is not a lane, and neither is one with no query left to take.
	lanes := min(threads, est.Ports, est.Queries)
	if lanes < 1 {
		lanes = 1 // a job is taken by at least one thread, whatever it was told
	}

	est.Expected = shaped(est.Warmups, est.Queries, pages,
		p.ReachPort.Typical, p.FirstRequest.Typical, p.LaterRequest.Typical) / time.Duration(lanes)

	// The same shape at the quickest each request was ever measured at. A pace
	// with no quick end leaves this at nought and the floor is the pacing alone,
	// which is what it always was.
	quickest := shaped(est.Warmups, est.Queries, pages,
		p.ReachPort.Fast, p.FirstRequest.Fast, p.LaterRequest.Fast) / time.Duration(lanes)

	// Both bound the same run, so the later of the two is the floor: waiting out
	// the pauses and doing the work are not alternatives.
	est.Floor = max(paced, quickest)

	// And the expectation cannot sit under its own floor. It also means a caller
	// who passes no pace at all is answered with the pacing bound rather than
	// with nothing.
	if est.Expected < est.Floor {
		est.Expected = est.Floor
	}
	return est
}

// shaped is what a job of this shape costs at the three per-request costs given.
//
// Every query is taken on one identity, so it pays one first answer and then a
// later answer for each page after the first. Getting to a port that answers is
// paid once per port the work reaches, which is the count the warm-ups already
// stand for.
//
// It is one function rather than two because the expectation and the floor
// differ only in which end of each measured range they are handed. Written
// twice, they would drift, and the two numbers on the screen would stop being
// the same arithmetic over the same job.
func shaped(warmups, queries, pages int, reach, first, later time.Duration) time.Duration {
	return time.Duration(warmups)*reach +
		time.Duration(queries)*first +
		time.Duration(queries*(pages-1))*later
}

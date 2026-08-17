// SPDX-License-Identifier: MIT

package run

import "time"

// Pace is what a request costs, measured rather than assumed.
type Pace struct {
	ReachPort    time.Duration // getting to a port that answers
	FirstRequest time.Duration // that port's first answer
	LaterRequest time.Duration // an answer from a port that has already answered
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
var MeasuredPace = Pace{
	ReachPort:    (27*time.Second + 9*time.Minute + 10*time.Second) / 2,
	FirstRequest: (4700*time.Millisecond + 6700*time.Millisecond) / 2,
	LaterRequest: (1400*time.Millisecond + 2700*time.Millisecond) / 2,
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
	// is one per port the work reaches rather than one per query, because the
	// session is kept for as long as the identity it belongs to, and a job with
	// fewer queries than ports never reaches the rest of them.
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
	// Floor is the shortest this can take. One query holds one identity however
	// deep it is taken, so a port carries one query per gap, and the busiest port
	// waits out one gap between each query and the next.
	//
	// It is a floor and nothing more. It counts no network time, no retries and
	// no walk that ends early, so the real run is longer. It is stated as the
	// minimum precisely so it cannot be read as a promise.
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
	est.Floor = time.Duration(perPort-1) * est.Cooldown

	// What the job costs, laid out as the work is shaped. Every query is taken on
	// one identity, so it pays one first answer and then a later answer for each
	// page after the first. Getting to a port that answers is paid once per port
	// the work reaches, which is the count the warm-ups already stand for.
	work := time.Duration(est.Warmups)*p.ReachPort +
		time.Duration(est.Queries)*p.FirstRequest +
		time.Duration(est.Queries*(pages-1))*p.LaterRequest

	// That work is shared between the threads, but a thread with no port to run
	// on is not a lane, and neither is one with no query left to take.
	lanes := min(threads, est.Ports, est.Queries)
	if lanes < 1 {
		lanes = 1 // a job is taken by at least one thread, whatever it was told
	}
	est.Expected = work / time.Duration(lanes)

	// Both numbers bound the same run, so the larger is the honest one. It also
	// means a caller who passes no pace at all is answered with the pacing bound
	// rather than with nothing.
	if est.Expected < est.Floor {
		est.Expected = est.Floor
	}
	return est
}

// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/blanktrail/google-serp-parser/google"
	"github.com/blanktrail/google-serp-parser/run"
	"github.com/blanktrail/google-serp-parser/store"
)

// noFigure stands where a figure would stand if there were anything to work it
// out from.
//
// A share of nothing is not nought per cent: nought per cent is a measurement
// somebody could act on, and nothing has been settled yet. The mark says as
// much without a sentence, and keeps the cell where the eye already is.
const noFigure = "—"

// reasons is every kind of answer a refused query can be filed under, each with
// the phrase that names it and the cell it is drawn in.
//
// The empty class stands for a query nothing came back from at all: nothing was
// classified because nothing arrived, and filing that under any of the others
// would report a kind of answer nobody received.
//
// The order is what breaks a tie between two reasons that came back as often as
// each other, so the same numbers are drawn the same way twice rather than
// swapping places between two readings of one screen.
var reasons = []struct {
	Class google.Class
	Key   string
	Cell  string
}{
	{google.ClassWall, "state.class.wall", "reason-wall"},
	{google.ClassBanned, "state.class.banned", "reason-banned"},
	{google.ClassShell, "state.class.shell", "reason-shell"},
	{google.ClassHTTP, "state.class.http", "reason-http"},
	{google.ClassEmpty, "state.class.empty", "reason-empty"},
	{google.ClassSERP, "state.class.serp", "reason-serp"},
	{"", "state.class.silent", "reason-silent"},
}

// statePage is the screen an operator sits in front of.
//
// It shows what is happening and says nothing about whether that is good. The
// reader knows what this program does not — that the address list is fresh, that
// the job is large, that it is the middle of the night where the target is — and
// a program that calls a job slow will one day call a healthy one slow, after
// which nothing else it says is believed either.
type statePage struct {
	page
	// Running is the job in flight, or nil when there is none.
	Running *runningView
	// Failures is that same job's refusals. It goes with the job: a share of
	// nothing, kept on the screen after the job it belonged to, is a number about
	// a run that is over.
	Failures *failureView
	Pool     poolView
	Queue    queueView
}

// runningView is the job in flight, drawn large.
type runningView struct {
	ID      int64
	Name    string
	Total   int
	Done    int
	Failed  int
	Pending int
	// Elapsed is how long it is since the job was written down, and Expected what
	// the estimate quoted for the whole of it. They stand beside each other and
	// the screen draws no conclusion from the pair.
	Elapsed  string
	Expected string
	// Rest is what is left at the pace the job has actually kept, which is the
	// mark when nothing has been settled yet and there is no pace to measure.
	Rest string
}

// failureView is how much of a job has been refused, and what came back.
type failureView struct {
	// Share is the part of the queries settled so far that were refused. It is of
	// what has been settled rather than of the whole job, because early on the
	// whole job is mostly work nobody has reached, and a share taken over that
	// reads as nothing being wrong however badly it is going.
	Share   string
	Failed  int
	Settled int
	Reasons []reasonView
}

// reasonView is one kind of answer and how often it came back.
type reasonView struct {
	Key   string
	Cell  string
	Count int
}

// poolView is what the identities report, whether or not anything is running.
//
// It is drawn either way. These counts are one of the three things that make an
// operator step in, and a screen that only showed them while a job was in flight
// would have nothing to say at exactly the moment somebody is deciding whether
// to start one.
type poolView struct {
	Alive       int
	Ports       int
	Rotations   int64
	Quarantined int
	Revived     int64
}

// queueView is what will be taken next, in the order it will be taken.
//
// A job whose plan was never finished being written is not here and cannot be:
// it can neither be started nor taken up, so it is never in this queue and never
// the job in flight. It looks like an ordinary job in one place only — the job
// list, where it is drawn with counts like anybody else's — and that is where
// saying so belongs.
type queueView struct {
	Waiting int
	Jobs    []queuedView
}

// queuedView is one job waiting its turn, and where in the queue it stands.
type queuedView struct {
	ID    int64
	Name  string
	Place int
}

// state draws what is happening right now.
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	lang := rememberLang(w, r)
	view, err := s.stateOf(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view.page = frame(r, lang, "state.title")
	s.render(w, r, "state.html", view)
}

// stateOf gathers the screen out of what is already being collected: the
// history for the job, the supervisor for the queue and the pool.
//
// Nothing here measures anything of its own. Every number on this screen is one
// some other part of the program already keeps, which is what makes the screen
// and the thing it describes impossible to disagree.
func (s *Server) stateOf(ctx context.Context) (statePage, error) {
	var view statePage
	if s.sup == nil {
		// A server started to read a history runs nothing and holds no
		// identities. It says so with the same screen rather than a special one.
		return view, nil
	}
	facts := s.sup.pool()
	view.Pool = poolView{
		Alive:       facts.Stats.Available,
		Ports:       facts.Stats.Ports,
		Rotations:   facts.Stats.EgressRotations,
		Quarantined: facts.Stats.Quarantined,
		Revived:     facts.Stats.Revivals,
	}

	// The queue comes from the supervisor and never from the history. A job is
	// written down when it is created, so every unfinished job in the history
	// reads as waiting, including ones nobody has asked for.
	queued := s.sup.Queued()
	view.Queue.Waiting = len(queued)
	for place, id := range queued {
		sum, err := s.store.Progress(ctx, id)
		if err != nil {
			return statePage{}, err
		}
		view.Queue.Jobs = append(view.Queue.Jobs,
			queuedView{ID: sum.ID, Name: sum.Name, Place: place + 1})
	}

	id, running := s.sup.Running()
	if !running {
		return view, nil
	}
	sum, err := s.store.Progress(ctx, id)
	if err != nil {
		return statePage{}, err
	}
	view.Running = s.runningView(sum, facts)
	failures, err := s.failuresOf(ctx, sum)
	if err != nil {
		return statePage{}, err
	}
	view.Failures = failures
	return view, nil
}

// runningView is the job in flight, with the two times the operator reads
// together.
//
// The elapsed time is counted from when the job was written down and the
// expected time is the estimate for the whole of it, so the two are about the
// same thing and may honestly be put side by side. Neither is compared to the
// other here; that is the reader's to do, with what they know and this does not.
func (s *Server) runningView(sum store.JobSummary, facts poolFacts) *runningView {
	elapsed := s.now().Sub(sum.CreatedAt)
	est := run.EstimateSize(sum.Total, sum.Pages,
		facts.Stats.Ports, facts.Threads, facts.Cooldown, run.MeasuredPace)

	view := &runningView{
		ID:       sum.ID,
		Name:     sum.Name,
		Total:    sum.Total,
		Done:     sum.Done,
		Failed:   sum.Failed,
		Pending:  sum.Pending,
		Elapsed:  spell(elapsed),
		Expected: spell(est.Expected),
		Rest:     noFigure,
	}
	// What is left, at the pace this job has actually kept rather than the one it
	// was quoted. It waits for a query to settle: a pace measured over none of
	// them is a division by nothing.
	if settled := sum.Done + sum.Failed; settled > 0 && elapsed > 0 {
		view.Rest = spell(elapsed / time.Duration(settled) * time.Duration(sum.Pending))
	}
	return view
}

// failuresOf is how much of a job has been refused and what came back.
//
// The reasons are read out of what was written down when each query was
// settled. A history keeps text and not values, so the class is recovered by
// the package that wrote it — see google.ClassInText — rather than guessed at
// here from the wording.
func (s *Server) failuresOf(ctx context.Context, sum store.JobSummary) (*failureView, error) {
	counted := make([]int, len(reasons))
	err := s.store.Failures(ctx, sum.ID, func(why string) error {
		class, _ := google.ClassInText(why)
		counted[bucketFor(class)]++
		return nil
	})
	if err != nil {
		return nil, err
	}

	view := &failureView{Share: noFigure, Failed: sum.Failed, Settled: sum.Done + sum.Failed}
	if view.Settled > 0 {
		view.Share = strconv.Itoa(sum.Failed*100/view.Settled) + "%"
	}
	for i, r := range reasons {
		if counted[i] == 0 {
			// A reason nothing came back as is a line of noughts between the
			// reader and the ones that did.
			continue
		}
		view.Reasons = append(view.Reasons, reasonView{Key: r.Key, Cell: r.Cell, Count: counted[i]})
	}
	// Commonest first, and the declared order where two are as common as each
	// other, so one screen read twice reads the same way twice.
	slices.SortStableFunc(view.Reasons, func(a, b reasonView) int { return b.Count - a.Count })
	return view, nil
}

// bucketFor is the reason a refusal is counted under.
//
// Every class the classifier produces is named in reasons today, and the last
// line is what keeps one added tomorrow out of whichever bucket happened to be
// first: a refusal this screen cannot name is counted as one nothing came back
// for, which is the closest true thing it can say about it.
func bucketFor(class google.Class) int {
	for i, r := range reasons {
		if r.Class == class {
			return i
		}
	}
	return len(reasons) - 1
}

// spell writes a duration the way a screen is read rather than the way it is
// measured.
//
// A screen is glanced at: "1h32m17.394s" is a measurement and "1h 32m" is an
// answer, and the seconds under an hour were never worth the width. The units
// are letters rather than words in the reader's language, exactly as the
// estimate on the new-job page already writes them, so the two pages quote a
// duration the same way.
func spell(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + "s"
	}
	// Rounded whole and then split, because rounding the minutes on their own
	// turns an hour and fifty-nine and a half minutes into "1h 60m".
	d = d.Round(time.Minute)
	hours, minutes := d/time.Hour, d%time.Hour/time.Minute
	if hours == 0 {
		return strconv.Itoa(int(minutes)) + "m"
	}
	if minutes == 0 {
		return strconv.Itoa(int(hours)) + "h"
	}
	return strconv.Itoa(int(hours)) + "h " + strconv.Itoa(int(minutes)) + "m"
}

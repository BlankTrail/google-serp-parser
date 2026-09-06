// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// stateRefresh is how often this screen asks the server to draw it again while
// there is something on it that moves.
//
// It is slower than the job page's poll because what comes back is the whole
// screen — the pool, the queue and the refusals with it — rather than four
// counts, and because not one of those figures says anything different a second
// later. What it buys for that is a screen that can never be half new: every
// number on it was worked out in one reading of one server.
const stateRefresh = 5 * time.Second

// listRefresh is how often the list of jobs asks to be drawn again while
// something on it is moving.
//
// Slower than the screen a job is watched from: the list says which jobs there
// are and roughly where each has got to, and nobody follows a single query on
// it. The job's own page is where that is watched.
const listRefresh = 5 * time.Second

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
	// Answered is that same job's share of queries that came back. It goes with
	// the job: a share of nothing, kept on the screen after the job it belonged
	// to, is a number about a run that is over.
	Answered *successView
	Pool     poolView
	Queue    queueView
	// Back is where a button pressed on this screen brings the reader: back here.
	// Stopping a run is watched, and a stop that moved the operator to another
	// page would take the screen away at the moment they most want to read it.
	Back string
}

// runningView is the job in flight, drawn large.
type runningView struct {
	ID      int64
	Name    string
	Total   int
	Done    int
	Failed  int
	Pending int
	// Elapsed is how long it is since the job was written down.
	//
	// The estimate that used to stand beside it is gone. It was read as a promise
	// and was not one: what dominates it is the time to reach an identity that
	// answers, measured at anything from half a minute to nine, and a figure that
	// wrong beside a real elapsed time makes every other figure on the screen
	// suspect.
	Elapsed string
	// Speed is how fast the job is settling queries now, and Rest is what is left
	// at that speed. Both are the mark when too little has settled to measure,
	// which is not the same as a job that has stopped.
	Speed string
	// PageSpeed is the same measurement in result pages a minute — the requests
	// themselves rather than the queries they belong to.
	PageSpeed string
	Rest      string
}

// successView is how much of a job is coming back answered, and what the rest
// came back as.
//
// It counts up rather than down. A share of refusals reads as a fault report
// even at nought, and the figure somebody glances at while a job runs is "is
// this working" — which is a number that should be high when things are well.
// The refusals are still every one of them on the screen, underneath, where
// they say what to do about it.
type successView struct {
	// Share is the part of the queries settled so far that came back answered. It
	// is of what has been settled rather than of the whole job, because early on
	// the whole job is mostly work nobody has reached, and a share taken over that
	// reads the same however badly it is going.
	Share   string
	Done    int
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
	Alive int
	Ports int
	// Warm is how many of those ports have brought back an answer under their
	// current identity. It is drawn beside the count of ports because the two
	// differ for the first quarter of an hour of every machine's day, and that
	// difference is the whole of what a job started in that window feels: a cold
	// identity's first request costs minutes and a warm one's costs seconds.
	Warm int
	// Queueing is how many threads are standing in the queue for an egress right
	// now, because every one they could use is already carrying as many
	// identities as it may.
	//
	// It is not a fault. A job of a hundred threads on thirty-two gateways is
	// arithmetic, and a screen that did not show this would leave the reader to
	// work out on their own why a pool of three hundred looks idle.
	Queueing    int
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
	lang := s.rememberLang(w, r)
	view, err := s.stateOf(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view.page = s.frame(r, lang, "state.title", stateAt)
	view.Back = stateAt
	// The screen asks for itself again only while the supervisor is holding
	// something. With nothing running and nothing waiting, every figure on it —
	// the counts, the queue, the ports — is moved by a run and by nothing else, so
	// it will read at three in the morning exactly as it reads now.
	if view.Running != nil || view.Queue.Waiting > 0 {
		view.Refresh = stateRefresh.Milliseconds()
	}
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
		Warm:        facts.Stats.Warm,
		Queueing:    facts.Stats.Waiting,
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
	pace, err := s.store.Pace(ctx, sum.ID)
	if err != nil {
		return statePage{}, err
	}
	view.Running = s.runningView(sum, pace)
	answered, err := s.answeredOf(ctx, sum)
	if err != nil {
		return statePage{}, err
	}
	view.Answered = answered
	return view, nil
}

// runningView is the job in flight: how long it has been going, how fast it is
// going now, and what is left at that speed.
//
// All three are measured from what the job has actually done. Nothing here is
// an estimate made before the run, and nothing here compares the job to one:
// the figure that used to do that was read as a promise and was not one.
func (s *Server) runningView(sum store.JobSummary, pace store.Pace) *runningView {
	view := &runningView{
		ID:        sum.ID,
		Name:      sum.Name,
		Total:     sum.Total,
		Done:      sum.Done,
		Failed:    sum.Failed,
		Pending:   sum.Pending,
		Elapsed:   spell(s.now().Sub(sum.CreatedAt)),
		Speed:     noFigure,
		PageSpeed: noFigure,
		Rest:      noFigure,
	}
	if !pace.Known {
		return view
	}
	// The speed the job is keeping now, and what is left at it. Measured over the
	// last few settled queries rather than over the whole run: on a job that
	// spent its first hour crawling, an average answers wrongly for the rest of
	// the day, and both figures here are read by somebody asking about now.
	view.Speed = perMinute(pace.PerMinute())
	view.PageSpeed = perMinute(pace.PagesPerMinute())
	if perMin := pace.PerMinute(); perMin > 0 && sum.Pending > 0 {
		view.Rest = spell(time.Duration(float64(sum.Pending) / perMin * float64(time.Minute)))
	}
	return view
}

// perMinute writes a speed the way a reader reads one: whole queries a minute
// once there are some, and one decimal below that, where the difference between
// half a query a minute and two is the difference between a job worth watching
// and one worth stopping.
func perMinute(rate float64) string {
	if rate <= 0 {
		return noFigure
	}
	if rate < 10 {
		return strconv.FormatFloat(rate, 'f', 1, 64)
	}
	return strconv.Itoa(int(rate + 0.5))
}

// failuresOf is how much of a job has been refused and what came back.
//
// The reasons are read out of what was written down when each query was
// settled. A history keeps text and not values, so the class is recovered by
// the package that wrote it — see google.ClassInText — rather than guessed at
// here from the wording.
func (s *Server) answeredOf(ctx context.Context, sum store.JobSummary) (*successView, error) {
	counted := make([]int, len(reasons))
	err := s.store.Failures(ctx, sum.ID, func(why string) error {
		class, _ := google.ClassInText(why)
		counted[bucketFor(class)]++
		return nil
	})
	if err != nil {
		return nil, err
	}

	view := &successView{
		Share:   noFigure,
		Done:    sum.Done,
		Failed:  sum.Failed,
		Settled: sum.Done + sum.Failed,
	}
	if view.Settled > 0 {
		view.Share = strconv.Itoa(sum.Done*100/view.Settled) + "%"
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

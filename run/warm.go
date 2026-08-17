// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/google"
)

// IdleBeforeWarming is how long a standing port is left alone before it is
// warmed again.
//
// Fifteen minutes, and only if nothing else has used it in that time: a port
// the run is working through is warm by definition, and warming it as well
// would be this program competing with itself for its own identities. It is
// also the point of the whole arrangement — a port kept just active enough to
// stay recognised, and no more visible than that.
const IdleBeforeWarming = 15 * time.Minute

// warmingSpacing is how long the warmer leaves between starting one warming and
// starting the next.
//
// A second, so a machine keeping sixty identities warms them across the minute
// rather than making sixty requests in the same instant. The bound above says
// how many may be in flight at once; this says how fast they may be started, and
// the two answer different questions: one is about how much of the set is held
// away from the work, the other is about what this machine looks like from the
// other end. Nothing a person does produces thirty searches in one second.
const warmingSpacing = time.Second

// warmingRound is how often the warmer looks for a port worth warming.
//
// It is shorter than the idle span because a port becomes due at whatever
// moment it was last used, not on the round's own clock: looking every minute
// warms a port within a minute of it falling due, and asks nothing of anybody
// in between.
const warmingRound = time.Minute

// warmingSubjects and warmingAsks are the two halves a warming phrase is built
// from, and warmingPhrase puts them together.
//
// A fixed list stood here, of ten. At one warming per identity per quarter of an
// hour that is four an hour, so an identity warmed through an afternoon asked
// the same handful of questions over and over — and every identity on the
// machine asked from the same handful. Built from two lists instead, the same
// amount of writing gives a few hundred phrases, and a repeat on one identity
// stops being something that happens by lunchtime.
//
// Every word is ordinary and dull on purpose, and every pairing has to read like
// something a person would type. Nothing is done with what comes back: the
// request exists to be made, not to be read.
var warmingSubjects = []string{
	"weather", "train times", "recipes", "dictionary", "news",
	"maps", "calculator", "translate", "opening hours", "football results",
	"bus timetable", "post office", "pharmacy", "hardware store", "library",
	"cinema", "swimming pool", "car park", "dentist", "bakery",
	"pizza", "coffee", "haircut", "laundry", "petrol station",
}

// warmingAsks are what a person adds to a subject when a bare word is not the
// whole question.
var warmingAsks = []string{
	"near me", "open now", "prices", "reviews", "phone number",
	"today", "this weekend", "for beginners", "how much", "best",
	"delivery", "booking",
}

// warmingPhrase is one thing to search for.
//
// A third of them are the bare subject, because that is how a good deal of real
// searching is done, and a machine whose every request carried a tail would be
// as recognisable as one that asked the same ten questions.
func warmingPhrase() string {
	subject := warmingSubjects[rand.IntN(len(warmingSubjects))]
	if rand.IntN(3) == 0 {
		return subject
	}
	return subject + " " + warmingAsks[rand.IntN(len(warmingAsks))]
}

// Warmer keeps a pool's standing ports warm.
//
// What "warm" buys is measured: a cold identity meets a challenge on its first
// request and the answer arrives minutes later, while a port that has answered
// recently answers again in seconds. A machine that keeps a few open pays for
// that once instead of at the start of every job.
type Warmer struct {
	// Pool holds the standing ports. Required.
	Pool *blanktrail.Pool
	// Log is where a failed warming is reported. A warming that fails is not a
	// fault worth stopping anything for — the port is one of many and the next
	// round will try another — but a machine where every one of them fails is a
	// machine whose identities are all dead, and that has to be readable.
	Log *slog.Logger
	// idle and round are the two spans, overridable so a test does not have to
	// wait a quarter of an hour to watch this work.
	idle  time.Duration
	round time.Duration
	// warmed is called after each warming that got through, so a test can count
	// them without reading the pool's own statistics.
	warmed func()
	// warmAtOnce overrides how many identities are warmed at the same time, so a
	// test can pin the number instead of deriving it from the set's size.
	warmAtOnce int
	// spacing overrides the gap left between starting one warming and the next,
	// so a test does not have to wait a second per identity.
	spacing time.Duration
	// beforeSearch runs just before each warming request goes out, so a test can
	// hold them all and see whether they are in flight together.
	beforeSearch func()
}

// Run warms every standing port that is due, until the context ends.
//
// It returns when the context does. Nothing about it is urgent: a round that
// finds nothing due does nothing, and a round that finds one warms one and
// leaves the rest to the next.
func (w *Warmer) Run(ctx context.Context) {
	idle, round := w.idle, w.round
	if idle <= 0 {
		idle = IdleBeforeWarming
	}
	if round <= 0 {
		round = warmingRound
	}

	// The first round is at once rather than after the interval: the ports have
	// just been opened and every one of them is cold, which is the moment this
	// is most worth doing.
	for {
		w.oneRound(ctx, idle)
		select {
		case <-ctx.Done():
			return
		case <-time.After(round):
		}
	}
}

// oneRound warms every port that has been idle long enough, several at a time.
//
// Several, and not all of them, because they are warmed through the pool the
// work uses: a round that took every idle port at once would hold the whole
// standing set while it waited on Google, and a job starting in that moment
// would find nothing free. Half the set is held at most, so there is always as
// much left free as is being warmed.
//
// It was one at a time, and one at a time was the defect a machine felt. The
// first request on a cold identity costs one to three minutes, measured; a set
// of twelve opened from cold therefore took a quarter of an hour or more to
// become worth anything, and a job started inside that window ran on identities
// the operator had been told were warm.
func (w *Warmer) oneRound(ctx context.Context, idle time.Duration) {
	var wg sync.WaitGroup
	defer wg.Wait()

	held := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if held > 0 && !w.wait(ctx, w.spaced()) {
			// The gap between one warming and the next. It is taken before the
			// next port is taken and not after the last one is started, so a
			// round that has nothing more to warm does not sit on a pause it owes
			// nobody.
			return
		}
		if held >= w.atOnce() {
			// As many as may be held at once are in flight. Waiting for all of
			// them beats waiting for one: they finish at their own pace, and the
			// next round picks up whatever is still due.
			return
		}
		lease, ok := w.Pool.AcquireIdleHot(idle)
		if !ok {
			return
		}
		held++
		if w.Log != nil {
			w.Log.Info("warming an identity that is being kept open",
				"port", lease.Port(), "this round", held, "at once", w.atOnce())
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.warmOne(ctx, lease)
		}()
	}
}

// spaced is the gap left between starting one warming and the next.
func (w *Warmer) spaced() time.Duration {
	if w.spacing > 0 {
		return w.spacing
	}
	return warmingSpacing
}

// wait sleeps for the given span and reports whether it ran out rather than
// being cut short. A warmer told to stop stops in the pause as well as in the
// request.
func (w *Warmer) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// atOnce is how many standing identities may be warmed at the same time.
//
// Half of what the machine keeps, and never fewer than one: whatever is being
// warmed is held, and a job that starts mid-round finds the other half. On a set
// of one that half is the whole of it, which is the same thing.
func (w *Warmer) atOnce() int {
	if w.warmAtOnce > 0 {
		return w.warmAtOnce
	}
	if hot := w.Pool.Hot(); hot > 1 {
		return hot / 2
	}
	return 1
}

// warmOne makes one ordinary search through a port and throws the answer away.
func (w *Warmer) warmOne(ctx context.Context, lease *blanktrail.Lease) {
	defer lease.Release()

	if w.beforeSearch != nil {
		w.beforeSearch()
	}
	sess := google.NewSession(lease.Client().Transport)
	sess.Client.Timeout = lease.Client().Timeout
	// No country and no language. A warming request is not the work: what it is
	// for is that this identity has been to Google once, and the locale of the
	// results is a parameter of each request rather than a property of the
	// identity — a job asks for whatever locale it wants afterwards and the
	// identity stays warm. Asking for none lets Google answer as it would answer
	// whoever is behind this address, which is what an ordinary visitor gets.
	q := google.Query{Text: warmingPhrase()}
	if _, err := sess.Search(ctx, q); err != nil {
		if ctx.Err() == nil && w.Log != nil {
			w.Log.Info("keeping an identity warm did not get through, which is what the next round is for",
				"port", lease.Port(), "error", err)
		}
		return
	}
	// The identity answered, so the pool may offer it before a cold one. This is
	// the whole point of the round: warming a port that nothing then prefers is
	// a request made for nobody.
	lease.Answered()
	if w.warmed != nil {
		w.warmed()
	}
}

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

// warmingRound is how often the warmer looks for a port worth warming.
//
// It is shorter than the idle span because a port becomes due at whatever
// moment it was last used, not on the round's own clock: looking every minute
// warms a port within a minute of it falling due, and asks nothing of anybody
// in between.
const warmingRound = time.Minute

// warmingPhrases are what a warming request asks for.
//
// They are ordinary and dull on purpose, and there are several so that a port
// warmed all afternoon is not a port asking the same question every fifteen
// minutes. Nothing is done with what comes back: the request exists to be made,
// not to be read.
var warmingPhrases = []string{
	"weather", "train times", "recipes", "dictionary", "news",
	"maps", "calculator", "translate", "opening hours", "football results",
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
	// Country and Language are what a warming request asks for, so a warmed port
	// has been through the same conversation the work will have with it.
	Country, Language string

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
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.warmOne(ctx, lease)
		}()
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
	q := google.Query{
		Text:     warmingPhrases[rand.IntN(len(warmingPhrases))],
		Country:  w.Country,
		Language: w.Language,
	}
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

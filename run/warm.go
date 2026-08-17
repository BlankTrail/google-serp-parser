// SPDX-License-Identifier: MIT

package run

import (
	"context"
	"log/slog"
	"math/rand/v2"
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

// oneRound warms every port that has been idle long enough, one at a time.
//
// One at a time because they are warmed through the pool the work uses: a round
// that took every idle port at once would hold the whole standing set while it
// waited on Google, and a job starting in that moment would find nothing free.
func (w *Warmer) oneRound(ctx context.Context, idle time.Duration) {
	for {
		if ctx.Err() != nil {
			return
		}
		lease, ok := w.Pool.AcquireIdleHot(idle)
		if !ok {
			return
		}
		w.warmOne(ctx, lease)
	}
}

// warmOne makes one ordinary search through a port and throws the answer away.
func (w *Warmer) warmOne(ctx context.Context, lease *blanktrail.Lease) {
	defer lease.Release()

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
	if w.warmed != nil {
		w.warmed()
	}
}

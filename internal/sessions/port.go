// SPDX-License-Identifier: MIT

package sessions

import (
	"context"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// DefaultRest and DefaultRestUpTo are the span a session rests when nobody has
// said otherwise: thirty seconds to a minute, as the operator set it.
//
// They live here rather than at each of the doors a job comes in by, because a
// job set up in the browser and the same job from the command line have to cost
// the same.
//
// The span was a minute to two, and was chosen again by measurement: one list
// of 8514 queries run three times at a hundred threads on the wingate list, at
// a minute to two, thirty seconds to a minute and fifteen to thirty. Past the
// start, the middle one was the fastest and the cheapest for the challenge
// solver — 790 result pages a minute at 27 captchas a thousand pages, against
// 726 at 45 and 717 at 49. Google looks again at a session about every half an
// hour or so whatever it is asked, so the longer rest paid for the twice as
// many sessions it kept in turn; the shorter one was asked again often enough
// that Google looked at each session more often, and made the solver finish
// two in three of those in a browser rather than one in four.
const (
	DefaultRest     = 30 * time.Second
	DefaultRestUpTo = time.Minute
)

// Want is what a caller asks a session to be.
type Want struct {
	// Device is the kind of result page: "desktop" or "mobile". A phone's
	// cookies are a phone's, and a session is never handed to the other kind.
	Device string
	// Browser, OS and Release narrow the fingerprint the way the job did;
	// empty and nought are any.
	Browser string
	OS      string
	Release int
	// Pause and UpTo are the rest a session takes after it was last used,
	// before it may be handed out again: the least and the most of it. Each
	// session draws its own rest between the two afresh at every use, so a
	// session asked again every sixty seconds to the millisecond — which is a
	// description of a program and not of a reader — does not happen.
	//
	// The rest is the taker's rather than the keeper's: a job, the warmer and a
	// search answered inside a request share the sessions, and not the rest.
	// UpTo below Pause is a caller naming one end only, and the other is that
	// end and half again — what the single number meant before there were two.
	Pause time.Duration
	UpTo  time.Duration
}

// Longest is the most a session may rest between two requests under this want:
// the far end where the caller named one, and the near end and half again where
// they named only that.
//
// It is here rather than in the keeper because it is the same reading of the
// same pair of numbers, and two places working out what a span means is two
// places to change when it changes.
func (w Want) Longest() time.Duration {
	if w.UpTo > w.Pause {
		return w.UpTo
	}
	return w.Pause + time.Duration(float64(w.Pause)*restSpread)
}

// matches says whether a session's fingerprint is what the want narrows to.
func (w Want) matches(browser, os string, release int) bool {
	return (w.Browser == "" || w.Browser == browser) &&
		(w.OS == "" || w.OS == os) &&
		(w.Release == 0 || w.Release == release)
}

// Fingerprint is what a port wears, by the name the service holds it under.
type Fingerprint struct {
	Profile string
	Browser string
	OS      string
	Release int
}

// Port is a leased port as the keeper needs it: where it goes out, what it may
// be moved to, and the three calls that put a session on it.
//
// The three are made in the one order the proxy service allows: the
// fingerprint, then the address, then the TLS tickets. A new fingerprint or a
// new address wipes a port's tickets, so tickets loaded earlier are lost without
// a word — found out only by a challenge.
type Port interface {
	Number() int
	// Exit is where the port goes out now: "addr:" and the address whole,
	// "gw:" and a gateway's name, or empty.
	Exit() string
	// Rests says whether the address has stopped carrying requests and is
	// serving its rest, whether or not the list still holds it.
	Rests(address string) bool
	// Stay says whether the port keeps its address whatever it meets while this
	// session is on it: a request its address does not carry ends there rather
	// than being carried to another address.
	Stay(on bool)
	// Candidates are the addresses the port may be moved onto.
	Candidates() []string
	// Limit is how many sessions may work through one address at once.
	Limit() int
	MoveTo(ctx context.Context, address string) error
	// Wear puts a named fingerprint on the port.
	Wear(ctx context.Context, profile string) error
	// Freshen gives the port a fresh fingerprint from its template.
	Freshen(ctx context.Context) (Fingerprint, error)
	// PutTickets loads TLS tickets onto the port, replacing what it held, and
	// says whether it stands on another address than the one named.
	PutTickets(ctx context.Context, address string, tickets []byte) (elsewhere bool, err error)
	// TakeTickets reads the port's TLS tickets without spending them.
	TakeTickets(ctx context.Context) ([]byte, error)
}

// An exit is written "addr:" and the address whole — login and password
// included, because a provider that sets the exit by the login has one host and
// port for all of them — or "gw:" and a gateway's name.
const (
	addrExit = "addr:"
	gateExit = "gw:"
)

// addressOf is the address an exit names, and whether it names one.
func addressOf(exit string) (string, bool) {
	a, ok := strings.CutPrefix(exit, addrExit)
	return a, ok && a != ""
}

// onGateway says the exit is a gateway. A session made behind one stays with it:
// there is no carrying it to another exit without a challenge, and one gateway
// can carry as many sessions as there are fingerprints to wear.
func onGateway(exit string) bool { return strings.HasPrefix(exit, gateExit) }

// hasAnswered says the session has been answered at least once: it holds cookies
// Google gave it, or tickets the service handed out for it. Such a session holds
// a clearance for its address, and moving it is spending the clearance.
func hasAnswered(r store.Session) bool {
	return len(r.Tickets) > 0 || (len(r.Cookies) > 0 && string(r.Cookies) != "[]")
}

// candidatesOf is what a port may be moved onto. A list with every address
// resting leaves nothing to choose from, and then the port's own address is the
// choice — which is what the pool does for a port of its own: an address that
// is probably still dead beats standing still until the rests run out.
func candidatesOf(p Port) []string {
	if c := p.Candidates(); len(c) > 0 {
		return c
	}
	if a, ok := addressOf(p.Exit()); ok {
		return []string{a}
	}
	return nil
}

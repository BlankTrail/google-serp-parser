// SPDX-License-Identifier: MIT

package sessions

import (
	"context"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/store"
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
	// Pause is how long a session rests after it was last used before it may be
	// handed out again. It is the taker's rather than the keeper's: a job, the
	// warmer and a search answered inside a request share the sessions, and not
	// the pause.
	Pause time.Duration
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
	// Offers says whether the port may be moved onto the address: the list
	// holds it and it is not resting.
	Offers(address string) bool
	// Knows says whether the list holds the address at all, resting or not.
	Knows(address string) bool
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

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// The methods in this file are a leased port as a keeper of sessions sees it:
// where the port goes out, what it may be moved to, and the three calls that put
// a session on it — the fingerprint, the address and the TLS tickets, in that
// order, because a new fingerprint or a new address wipes the port's tickets.
//
// They live on the lease because the lease is what knows all of it at once: the
// port number, the template the port was opened under, the channel its addresses
// come from, and the pool whose record of the port has to stay what the service
// says. A session's address put on a port behind the pool's back is a port the
// pool later hands out as if it stood where it used to — measured: a session
// went out through another exit and paid a challenge for it.

// Exit is where the port goes out now, written the way a session keeps it:
// "addr:" and the address whole, "gw:" and a gateway's name, or empty for a port
// that goes out directly.
func (l *Lease) Exit() string {
	eg := l.pt.egress()
	switch {
	case eg.Gateway != "":
		return "gw:" + eg.Gateway
	case eg.Upstream != "":
		return "addr:" + eg.Upstream
	default:
		return ""
	}
}

// Offers says whether the port may be moved onto the address: the list the
// port's addresses come from holds it, and it is not resting.
func (l *Lease) Offers(address string) bool {
	h, ok := l.pt.ch.(interface{ Holds(string) bool })
	return ok && h.Holds(address)
}

// Knows says whether the list the port's addresses come from holds the address
// at all, resting or not. A session whose address is resting waits for it; one
// whose address has left the list takes another.
func (l *Lease) Knows(address string) bool {
	h, ok := l.pt.ch.(interface{ Lists(string) bool })
	return ok && h.Lists(address)
}

// Stay says whether the port keeps its address whatever it meets, for as long as
// this lease lasts.
//
// It is what a session that has answered asks for. It holds a clearance for its
// address; a request that address does not carry is not carried to another,
// where the clearance would be spent on a challenge — the address is put to rest
// and the request ends, and the session waits for its address. A session that
// has never answered has nothing to lose and lets its requests go wherever an
// address will carry them.
func (l *Lease) Stay(on bool) {
	l.pt.mu.Lock()
	l.pt.stay = on
	l.pt.mu.Unlock()
}

// Candidates are the addresses the port may be moved onto.
func (l *Lease) Candidates() []string {
	free, ok := l.pt.ch.(interface{ Free() []Egress })
	if !ok {
		return nil
	}
	all := free.Free()
	out := make([]string, 0, len(all))
	for _, eg := range all {
		out = append(out, eg.Upstream)
	}
	return out
}

// Limit is how many sessions may work through one address at the same time.
func (l *Lease) Limit() int {
	if n := l.pool.cfg.MaxPerUpstream; n > 0 {
		return n
	}
	return 1
}

// MoveTo puts an address on the port through the pool's own record of it.
func (l *Lease) MoveTo(ctx context.Context, address string) error {
	if l.pt.egress().Gateway != "" {
		return fmt.Errorf("blanktrail: port %d goes out through a gateway and cannot be moved to an address", l.pt.num)
	}
	return l.pool.putEgress(ctx, l.pt, Egress{Upstream: address})
}

// Worn is the browser, system and release of the template the port was opened
// under.
func (l *Lease) Worn() Worn { return wornOf(l.pt.spec) }

// wornOf reads a template back into the identity it names: "chrome_153" is
// Chrome at release 153, and a bare "chrome" is Chrome at no named release.
func wornOf(spec PortSpec) Worn {
	w := Worn{Browser: spec.Browser, OS: spec.OS}
	if i := strings.LastIndexByte(spec.Browser, '_'); i >= 0 {
		if n, err := strconv.Atoi(spec.Browser[i+1:]); err == nil {
			w.Browser, w.Release = spec.Browser[:i], n
		}
	}
	return w
}

// Wear puts a named fingerprint on the port, with keep_sessions off.
func (l *Lease) Wear(ctx context.Context, profile string) error {
	got, err := l.pool.cl.WearSession(ctx, l.pt.num, profile)
	if err != nil {
		return err
	}
	if got.Name != "" && got.Name != profile {
		// The service put something else on the port. Cookies won under one
		// fingerprint sent under another are a session that disagrees with
		// itself.
		return fmt.Errorf("blanktrail: port %d wears %q after being asked for %q", l.pt.num, got.Name, profile)
	}
	l.pt.changedIdentity()
	return nil
}

// Freshen gives the port a fresh fingerprint from the template it was opened
// under.
func (l *Lease) Freshen(ctx context.Context) (Profile, error) {
	prof, err := l.pool.cl.FreshProfile(ctx, l.pt.num, l.pt.spec)
	if err != nil {
		return Profile{}, err
	}
	l.pt.changedIdentity()
	return prof, nil
}

// PutTickets loads a session's TLS tickets onto the port, replacing whatever it
// held, and says whether the port stands on another address than the one named.
func (l *Lease) PutTickets(ctx context.Context, address string, tickets []byte) (bool, error) {
	got, err := l.pool.cl.ImportSession(ctx, l.pt.num, address, tickets)
	if err != nil {
		return false, err
	}
	return got.IdentityMismatch, nil
}

// TakeTickets reads the port's TLS tickets without spending them.
func (l *Lease) TakeTickets(ctx context.Context) ([]byte, error) {
	out, err := l.pool.cl.ExportSession(ctx, l.pt.num)
	if err != nil {
		return nil, err
	}
	return out.Tickets, nil
}

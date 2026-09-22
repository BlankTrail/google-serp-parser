// SPDX-License-Identifier: MIT

package blanktrail

import (
	"errors"
	"fmt"
	"strings"
)

// FirstHop is the leg a port's traffic takes before it reaches the port's
// address: a SOCKS5 proxy, or one of the service's gateways, that carries it on
// to the address the port is on. The zero value goes to the address directly.
//
// It exists for addresses this machine cannot reach well. Measured on a wingate
// list, 2026-09-22: straight from here, sixteen searches through the program's
// own sessions brought no page at all — the ones that reached Google met its
// JavaScript check, and the solver's browser could not open a single connection
// through the address to pass it (the SOCKS connect ended in EOF or a reset, and
// after ten seconds the service handed the check back as the answer). Through a
// first hop, the same list answered forty-six searches in forty-eight, the
// check passed by the solver, and a session asked again answered in one to
// fourteen seconds. The address was never the trouble; the road to it was.
//
// The address stays what the session is kept against. The first hop changes the
// road and not the exit Google sees, so a session made through one first hop is
// the same session through another.
type FirstHop struct {
	// Proxy is a SOCKS5 proxy, scheme://[user:pass@]host:port, kept whole the
	// way an address is.
	Proxy string
	// Gateway is the name of one of the service's gateways. The service raises
	// its tunnel and runs the port through it.
	Gateway string
}

// gatewayHop is how a first hop through a gateway is written: the prefix and
// the gateway's name, the way a session kept on a gateway is written.
const gatewayHop = "gw:"

// IsZero says there is no first hop: the port goes to its address directly.
func (h FirstHop) IsZero() bool { return h == FirstHop{} }

// String is the first hop as a profile keeps it: nothing for none, "gw:" and a
// gateway's name, or the proxy's address whole. ParseFirstHop reads it back.
func (h FirstHop) String() string {
	switch {
	case h.Gateway != "":
		return gatewayHop + h.Gateway
	default:
		return h.Proxy
	}
}

// ParseFirstHop reads a first hop as a profile keeps it.
//
// A proxy is read the way an address on a list is — no scheme is SOCKS5, and
// the login may be written on either side of the address — and it has to be a
// SOCKS5 one: that is the only way the service reaches a first hop, and an HTTP
// proxy named there would be a port that never carries anything, found out an
// hour into a job rather than when the profile was saved.
func ParseFirstHop(kept string) (FirstHop, error) {
	kept = strings.TrimSpace(kept)
	if kept == "" {
		return FirstHop{}, nil
	}
	if name, ok := strings.CutPrefix(kept, gatewayHop); ok {
		name = strings.TrimSpace(name)
		if name == "" {
			return FirstHop{}, errors.New("blanktrail: a first hop through a gateway names no gateway")
		}
		return FirstHop{Gateway: name}, nil
	}
	ups, bad := Parse(kept, "socks5")
	if len(bad) > 0 || len(ups) != 1 {
		return FirstHop{}, fmt.Errorf("blanktrail: %q is not a proxy address", kept)
	}
	if s := ups[0].Scheme; s != "socks5" && s != "socks5h" {
		return FirstHop{}, fmt.Errorf("blanktrail: a first hop is a SOCKS5 proxy, not %s", s)
	}
	return FirstHop{Proxy: ups[0].URL()}, nil
}

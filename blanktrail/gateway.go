// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"fmt"
)

// gatewayScheme marks a rotor entry that names a stored VPN gateway rather than
// a proxy address.
//
// A gateway is a name and nothing else — the configuration behind it lives in
// BlankTrail — so it does not fit the shape of an address. It is carried in one
// anyway, and deliberately: what a rotor does is hand things out in turn, put a
// failing one aside for a while, keep the bench from swallowing the list and
// bring the longest-rested back first. Every one of those rules was arrived at
// by measurement, and a second copy of them for gateways would be a second copy
// to keep in step. Only this file makes these entries and only this file reads
// them.
const gatewayScheme = "gateway"

// GatewayUpstreams turns gateway names into rotor entries.
func GatewayUpstreams(names []string) []Upstream {
	out := make([]Upstream, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		out = append(out, Upstream{Scheme: gatewayScheme, Host: name})
	}
	return out
}

// gatewayChannel hands out stored VPN gateways in turn, the way a list channel
// hands out addresses.
type gatewayChannel struct {
	name  string
	rotor *Rotor
}

// NewGatewayListChannel rotates through the gateways a rotor holds. Renew moves
// to the next one.
//
// The rotor must have been filled by GatewayUpstreams: what this hands back is
// the name out of each entry, as a gateway rather than a proxy URL.
func NewGatewayListChannel(name string, r *Rotor) Channel {
	return &gatewayChannel{name: name, rotor: r}
}

func (c *gatewayChannel) Name() string      { return c.name }
func (c *gatewayChannel) Kind() ChannelKind { return KindGateway }

func (c *gatewayChannel) Next() (Egress, bool) {
	u, ok := c.rotor.Next()
	if !ok {
		return Egress{}, false
	}
	return Egress{Gateway: u.Host}, true
}

func (c *gatewayChannel) Renew(_ context.Context, _ Egress) (Egress, error) {
	eg, ok := c.Next()
	if !ok {
		return Egress{}, fmt.Errorf("blanktrail: channel %q has no gateways left", c.name)
	}
	return eg, nil
}

func (c *gatewayChannel) MarkBad(eg Egress) {
	if eg.Gateway != "" {
		c.rotor.MarkBad(Upstream{Scheme: gatewayScheme, Host: eg.Gateway})
	}
}

func (c *gatewayChannel) MarkDead(eg Egress) {
	if eg.Gateway != "" {
		c.rotor.MarkDead(Upstream{Scheme: gatewayScheme, Host: eg.Gateway})
	}
}

func (c *gatewayChannel) Close() { c.rotor.Close() }

// Len, Resting and ReleaseAll are what the proxy screen counts and clears, and
// they mean here exactly what they mean for a list of addresses.
func (c *gatewayChannel) Len() int        { return c.rotor.Len() }
func (c *gatewayChannel) Resting() int    { return c.rotor.RestingHere() }
func (c *gatewayChannel) ReleaseAll() int { return c.rotor.ReleaseAll() }

// How long a gateway that failed is left out is not decided here. It is the
// rotor's rest, set by whoever builds the rotor from the ban the operator
// chose, and WithRest already keeps the default when that ban is nought — so a
// gateway rests exactly as long as an address does. A second answer to the
// question lived here for a while, was never called, and is not missed.

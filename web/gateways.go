// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"sort"
	"strings"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/settings"
)

// gatewayChoice is one stored VPN configuration as the proxy screen offers it.
type gatewayChoice struct {
	Name string
	// Kind is what it speaks — vless, openvpn, shadowsocks — and Ping is the
	// last round trip the service measured, in milliseconds. Nought means it has
	// not been measured, which is not the same as instant.
	Kind   string
	Ping   int
	Timed  bool
	Chosen bool
}

// gatewayGroup is a subscription and the configurations that came with it.
type gatewayGroup struct {
	// Name is the subscription. Empty is the group for configurations that were
	// uploaded on their own.
	Name    string
	Items   []gatewayChoice
	Chosen  int
	Offered int
}

// subscriptionOf is the subscription a configuration belongs to.
//
// The service has no field for it — asked directly, it answers a flat list —
// but the names it hands back carry it: a configuration that arrived with a
// subscription is named for it, "WiseKeys.DE-Germaniya", and one uploaded on its
// own has no dot in it at all. Reading the name is the only way there is, and it
// is right often enough to be worth doing: on a live service, 26 of 32
// configurations grouped and the remaining 6 were the ones uploaded by hand.
func subscriptionOf(name string) string {
	before, _, found := strings.Cut(name, ".")
	if !found {
		return ""
	}
	return before
}

// gatewaysOffered asks the service what it holds and lays it out for the screen,
// with whatever is already chosen ticked.
//
// A name that is chosen and no longer on the service is not drawn: it cannot be
// ticked or unticked, and drawing a box for something that is not there invites
// the reader to fix a thing they cannot reach. What it does do is come back in
// the count, so the screen can say that fewer are being used than were chosen.
func gatewaysOffered(list blanktrail.GatewayList, chosen []string) ([]gatewayGroup, int) {
	want := map[string]bool{}
	for _, name := range chosen {
		want[name] = true
	}

	byName := map[string]*gatewayGroup{}
	var order []string
	found := 0
	for _, g := range list.Gateways {
		sub := subscriptionOf(g.Name)
		group, seen := byName[sub]
		if !seen {
			group = &gatewayGroup{Name: sub}
			byName[sub] = group
			order = append(order, sub)
		}
		item := gatewayChoice{Name: g.Name, Kind: g.Kind, Chosen: want[g.Name]}
		if item.Chosen {
			found++
			group.Chosen++
		}
		group.Offered++
		group.Items = append(group.Items, item)
	}

	// Named subscriptions first and in their own order; the ones uploaded on
	// their own last, because that group is a remainder rather than a thing
	// somebody bought.
	sort.SliceStable(order, func(i, j int) bool {
		if (order[i] == "") != (order[j] == "") {
			return order[j] == ""
		}
		return order[i] < order[j]
	})

	groups := make([]gatewayGroup, 0, len(order))
	for _, sub := range order {
		g := byName[sub]
		sort.SliceStable(g.Items, func(i, j int) bool { return g.Items[i].Name < g.Items[j].Name })
		groups = append(groups, *g)
	}
	return groups, len(chosen) - found
}

// askForGateways reads what the service holds, using the connection that is
// saved.
//
// It is asked for only when the saved source is the gateways: the screen redraws
// itself every few seconds while a job runs, and a page that asked on every
// redraw would put a request to the service behind every one of them.
func (s *Server) askForGateways(ctx context.Context, saved settings.Settings) (blanktrail.GatewayList, error) {
	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		return blanktrail.GatewayList{}, err
	}
	return client.Gateways(ctx)
}

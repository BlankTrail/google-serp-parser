// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/settings"
)

// gatewayChoice is one stored VPN configuration as the proxy screen offers it.
type gatewayChoice struct {
	Name string
	// Kind is what it speaks — vless, openvpn, shadowsocks — and Ping is the
	// last round trip the service measured, in milliseconds. Nought means it has
	// not been measured, which is not the same as instant.
	Kind string
	// Ping is the round trip in milliseconds and Timed says there is one to
	// show. Tried without Timed is a gateway that was measured and did not
	// answer, which is worth saying out loud rather than drawing as nought.
	Ping   int
	Timed  bool
	Tried  bool
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
		item := gatewayChoice{
			Name:   g.Name,
			Kind:   g.Kind,
			Ping:   g.Ping.MS,
			Timed:  g.Ping.Answered,
			Tried:  g.Ping.Tried,
			Chosen: want[g.Name],
		}
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

// gatewaysHeldFor is how long the list the service gave is drawn again without
// asking for it afresh.
//
// The screen redraws itself every few seconds while a job runs, and a page that
// asked on every redraw would put a request to the service behind every one of
// them. What the service holds changes when somebody adds a configuration or
// measures the tunnels, which is not something that happens between two redraws
// — and when it is, the refresh button is the way to say so.
const gatewaysHeldFor = 2 * time.Minute

// gatewaysHeld is the last list the service gave and when it gave it.
type gatewaysHeld struct {
	mu    sync.Mutex
	list  blanktrail.GatewayList
	taken time.Time
}

// askForGateways reads what the service holds, using the connection that is
// saved, and hands back when the reading was taken.
//
// afresh is the refresh button: it asks the service whatever is held, so a
// reader who has just added a configuration or measured the tunnels sees that
// rather than the answer from two minutes ago.
func (s *Server) askForGateways(ctx context.Context, saved settings.Settings, afresh bool) (blanktrail.GatewayList, time.Time, error) {
	now := time.Now()
	s.gateways.mu.Lock()
	held, taken := s.gateways.list, s.gateways.taken
	s.gateways.mu.Unlock()
	if !afresh && !taken.IsZero() && now.Sub(taken) < gatewaysHeldFor {
		return held, taken, nil
	}

	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		return blanktrail.GatewayList{}, time.Time{}, err
	}
	list, err := client.Gateways(ctx)
	if err != nil {
		return blanktrail.GatewayList{}, time.Time{}, err
	}
	s.gateways.mu.Lock()
	s.gateways.list, s.gateways.taken = list, now
	s.gateways.mu.Unlock()
	return list, now, nil
}

// gatewayFault names why the list could not be asked for, in the reader's
// language.
//
// The four answers are four different things to go and do, which is the whole
// reason they are told apart: a service that is not running, a key it will not
// take, a version that has never heard of gateways, and anything else it chose
// to say. One sentence for all of them sends the reader to check the address
// when the address was never the problem.
func gatewayFault(err error) string {
	if blanktrail.IsUnauthorized(err) {
		return "proxies.gateways.refused"
	}
	var answered *blanktrail.APIError
	if errors.As(err, &answered) {
		if answered.Status == http.StatusNotFound {
			return "proxies.gateways.unknown"
		}
		return "proxies.gateways.failed"
	}
	return "proxies.gateways.unreachable"
}

// refreshGateways asks the service for its configurations again and shows the
// screen.
//
// The list is held for a couple of minutes so that a screen redrawing itself
// while a job runs does not ask behind every redraw. That is right until the
// reader has just added a configuration or measured the tunnels, and this is
// how they say so — which is also why it is a press and not a link: it changes
// what the program has, even though it changes nothing the program keeps.
func (s *Server) refreshGateways(w http.ResponseWriter, r *http.Request) {
	saved, _ := s.current()
	if saved.ControlURL != "" {
		// The fault, if there is one, is drawn on the screen this redirects to:
		// asking again is not a thing that can fail differently from drawing.
		_, _, _ = s.askForGateways(r.Context(), saved, true)
	}
	http.Redirect(w, r, proxiesAt, http.StatusSeeOther)
}

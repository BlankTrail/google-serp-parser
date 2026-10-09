// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrPortNotOurs is a port number this program opened that now carries a port
// somebody else opened.
//
// The service keeps no owner for a port: a number is whoever opened it last. A
// restart of the service frees every number at once, and another program on it
// opens its own ports on them within seconds. On 2026-10-09 the service was
// restarted to 1.4.1077 under both demo parsers: they went on warming sessions
// through 20014–20022, which by then belonged to the Xrumer pool, and the owner
// of that pool saw its ports reconfigured through the control API by somebody
// else. A call meant for our port, made on that number, is made on theirs.
var ErrPortNotOurs = errors.New("blanktrail: the port number now carries a port somebody else opened")

// ownFresh is how old the service's list of ports may be when a call on one of
// ours is checked against it.
//
// The list is one request for the whole client, about a kilobyte a port and 12
// ms for a hundred ports on the live service, and every pool of the program
// shares it. Two seconds is a call a second at most whatever the traffic, and
// the window in which a restart and somebody else's open can go unseen.
const ownFresh = 2 * time.Second

// ledger is what a client knows about which ports are its own: when the
// service says each was opened, read from its list of ports.
type ledger struct {
	mu sync.Mutex
	// mine is every port this client opened and has not closed. Its creation
	// time is empty until the first list read after the open names it.
	mine map[int]claim
	// listed is the service's last list: each port open on it and when it was
	// opened; listedAt is when it was read.
	listed   map[int]seen
	listedAt time.Time
	// opens counts the ports this client has opened, and listedOpens is how
	// many it had when the list was read. A list read before a port was opened
	// cannot speak for it however recent it is: a clock reads the same instant
	// for an open and a list read a moment apart, and the opens never do.
	opens, listedOpens int
	// reading keeps a hundred callers from reading the list a hundred times.
	reading sync.Mutex
}

// claim is one port this client opened. lost is a number found carrying
// somebody else's port: it stays refused until this client opens a port on it
// again, since every later call on it would be a call on theirs.
type claim struct {
	created string
	// set is what the port was set to the last time the list showed it ours.
	set  string
	open int
	lost bool
}

// seen is one port as the service's list shows it: when it was opened, and
// what it is set to, in one string two ports compare by.
type seen struct {
	created string
	set     string
}

func (c *Client) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// claim records that this client has just opened the port.
func (c *Client) claim(port int) {
	c.own.mu.Lock()
	defer c.own.mu.Unlock()
	if c.own.mine == nil {
		c.own.mine = map[int]claim{}
	}
	c.own.opens++
	c.own.mine[port] = claim{open: c.own.opens}
}

// unclaim records that the port is no longer this client's.
func (c *Client) unclaim(port int) {
	c.own.mu.Lock()
	defer c.own.mu.Unlock()
	delete(c.own.mine, port)
}

// Owns says whether the port is still the one this client opened.
//
// Nil is the port is ours, or the question cannot be answered — the service is
// away, or the port was never opened through this client — and a call made on
// it then fails or succeeds on its own. ErrPortNotOurs is a number that now
// carries somebody else's port: nothing may be asked of it. A port no longer on
// the service at all is answered the way the service answers a call on it, a
// not-found, which is what reopens it (see notOpen).
func (c *Client) Owns(ctx context.Context, port int) error {
	c.own.mu.Lock()
	mine, ok := c.own.mine[port]
	stale := c.now().Sub(c.own.listedAt) >= ownFresh || c.own.listedOpens < mine.open
	c.own.mu.Unlock()
	if !ok {
		return nil
	}
	if stale {
		if err := c.readList(ctx, mine.open); err != nil {
			return nil
		}
	}

	c.own.mu.Lock()
	defer c.own.mu.Unlock()
	mine, ok = c.own.mine[port]
	if !ok {
		return nil
	}
	if mine.lost {
		return ErrPortNotOurs
	}
	now, open := c.own.listed[port]
	switch {
	case !open:
		return &APIError{Status: http.StatusNotFound, Path: "/api/v1/ports",
			Message: "port " + strconv.Itoa(port) + " is not on the service's list"}
	case mine.created == "" || now.created == mine.created:
		// The first list read since the open says what was opened; every one
		// after it, what the port is set to now.
		mine.created, mine.set = now.created, now.set
		c.own.mine[port] = mine
		return nil
	}
	// Opened again since. Either the service restored it at a restart — it
	// opens every port it held again, as it was, under a new creation time —
	// or somebody else opened the number. The settings alone cannot tell them
	// apart: 99 of the Xrumer pool's ports on 8893 were set exactly as a
	// session port of ours. What can is the service's word that it restores:
	// it writes down every port at each open and close and gives each number
	// back to whoever held it, so a number that comes back set as ours is ours.
	c.own.mu.Unlock()
	restored := now.set == mine.set && c.restores(ctx)
	c.own.mu.Lock()
	mine, ok = c.own.mine[port]
	if !ok {
		return nil
	}
	if restored {
		mine.created = now.created
		c.own.mine[port] = mine
		return nil
	}
	mine.lost = true
	c.own.mine[port] = mine
	return ErrPortNotOurs
}

// restores asks the service whether it opens its ports again when it starts.
// Not knowing is no: a port taken for somebody else's is a port lost, and one
// taken for ours that is not is somebody else's port used.
func (c *Client) restores(ctx context.Context) bool {
	var out struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/presets/autosave", nil, &out); err != nil {
		return false
	}
	return out.Enabled
}

// readList reads the service's list of ports unless somebody has read it, since
// the open numbered open, within ownFresh.
func (c *Client) readList(ctx context.Context, open int) error {
	c.own.reading.Lock()
	defer c.own.reading.Unlock()
	c.own.mu.Lock()
	fresh := c.now().Sub(c.own.listedAt) < ownFresh && c.own.listedOpens >= open
	opens := c.own.opens
	c.own.mu.Unlock()
	if fresh {
		return nil
	}
	at := c.now()
	var out struct {
		Ports []struct {
			Port    int    `json:"port"`
			Created string `json:"created_at"`
			// What the port is set to, as far as this program sets it.
			Protocol     string `json:"protocol"`
			Upstream     string `json:"upstream"`
			Gateway      string `json:"upstream_gateway"`
			ChainProxy   string `json:"chain_proxy"`
			ChainGateway string `json:"chain_gateway"`
			Mode         string `json:"mode"`
			Browser      string `json:"browser_filter"`
			OS           string `json:"os_filter"`
			JSSolver     bool   `json:"js_solver"`
			KeepSessions bool   `json:"keep_sessions"`
		} `json:"ports"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/ports", nil, &out); err != nil {
		return err
	}
	listed := make(map[int]seen, len(out.Ports))
	for _, p := range out.Ports {
		listed[p.Port] = seen{created: p.Created, set: strings.Join([]string{p.Protocol, p.Upstream, p.Gateway,
			p.ChainProxy, p.ChainGateway, p.Mode, p.Browser, p.OS,
			strconv.FormatBool(p.JSSolver), strconv.FormatBool(p.KeepSessions)}, "|")}
	}
	c.own.mu.Lock()
	c.own.listed, c.own.listedAt, c.own.listedOpens = listed, at, opens
	c.own.mu.Unlock()
	return nil
}

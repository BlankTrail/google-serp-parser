//go:build live

// SPDX-License-Identifier: MIT

package run

// A job run twice through the program's own sessions on the live service: the
// sessions are made, reused, written down with their tickets, and found again by
// a keeper that starts over on the same history.
//
// What the second run did with each session the first one left is read from the
// history itself, before and after: a session used again on the same exit with
// the same clearance was taken up without a challenge; a new clearance is a
// challenge paid; a new exit is a session a request carried elsewhere. Timing
// alone cannot tell these apart — a request walked through seven dead addresses
// takes as long as one that met a challenge.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// sessionsLivePhrases are what the two runs ask; none repeats, so no answer can
// come from a cache between them.
var sessionsLivePhrases = []string{
	"купить кондиционер", "доставка пиццы", "ремонт квартиры", "детские кроватки",
	"туры в турцию", "автосервис рядом", "пластиковые окна", "стоматология цены",
	"аренда авто", "натяжные потолки", "курсы английского", "шины зимние",
}

// sessionLook is a session as the history holds it at one moment: where it goes
// out, the clearance Google gave it, and when it was last used. The exit is
// compared and never printed — it carries the proxy's password.
type sessionLook struct {
	exit      string
	clearance string
	used      time.Time
	tickets   bool
}

func lookAtSessions(ctx context.Context, t *testing.T, st *store.Store) map[int64]sessionLook {
	t.Helper()
	all, err := st.Sessions(ctx, blanktrail.DeviceDesktop, time.Time{})
	if err != nil {
		fatalf(t, "reading the sessions: %v", err)
	}
	out := map[int64]sessionLook{}
	for _, s := range all {
		var cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal(s.Cookies, &cookies)
		look := sessionLook{exit: s.Exit, used: s.UsedAt, tickets: len(s.Tickets) > 0}
		for _, c := range cookies {
			if c.Name == "GOOGLE_ABUSE_EXEMPTION" {
				look.clearance = c.Value
			}
		}
		out[s.ID] = look
	}
	return out
}

// attempts counts what the pool saw of each request attempt: one that carried
// nothing is an address walked away from, and one answered after half a minute
// or more met a challenge on the way.
type attempts struct {
	mu       sync.Mutex
	failed   int
	answered int
	slow     int
}

func (a *attempts) note(tr blanktrail.RequestTrace) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case tr.Err != nil:
		a.failed++
	case tr.Status != 0:
		a.answered++
		if tr.Total >= sessionMoveSlow {
			a.slow++
		}
	}
}

func TestLiveSessions_AreMadeReusedWrittenDownAndFoundAgain(t *testing.T) {
	ctx := context.Background()
	control, key, listURL := liveEnv(t)
	ups := addressList(ctx, t, listURL)
	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		fatalf(t, "control client: %v", err)
	}
	pre := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"}, Ports: 4})
	if !pre.OK() {
		t.Skip("preflight refused the run")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		fatalf(t, "store.Open: %v", err)
	}
	defer st.Close()
	want := sessions.Want{Device: blanktrail.DeviceDesktop, Pause: 5 * time.Second}

	// pass runs six phrases through a pool of sessions opened afresh, on a
	// keeper started afresh over the same history.
	pass := func(label string, phrases []string) {
		k := sessions.NewKeeper(st)
		seen := &attempts{}
		p, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
			Client: client, Threads: 2, PortsPerThread: 2, Spec: blanktrail.DefaultPortSpec(), CA: pre.CA,
			Channels: []blanktrail.Channel{blanktrail.NewListChannel("list",
				blanktrail.NewStaticRotor(ups, blanktrail.WithRest(time.Hour)))},
			Sessions: true, Choose: k.Choose, AddressesPerRequest: 15,
			ReviveAfter: time.Minute, WaitForIdentity: true, Trace: seen.note,
		})
		if err != nil {
			fatalf(t, "%s: opening the pool: %v", label, err)
		}
		defer p.Close()
		p.PaceAt(want.Pause)

		qs := make([]google.Query, len(phrases))
		for i, ph := range phrases {
			qs[i] = google.Query{Text: ph, Country: "ru", Language: "ru"}
		}
		began := time.Now()
		rep := (&Runner{Pool: p, Threads: 2, Keeper: k, Want: want}).Run(ctx, Job{Queries: qs, Pages: 1})
		answered := 0
		for _, q := range rep.Results {
			if q.Err == nil {
				answered++
			}
		}
		all, _ := k.Count()
		seen.mu.Lock()
		logf(t, "MEASUREMENT %s: %d of %d answered in %v; %d sessions known; attempts: %d answered "+
			"(%d of them after 30 s or more), %d carried nothing",
			label, answered, len(rep.Results), time.Since(began).Round(time.Second), all,
			seen.answered, seen.slow, seen.failed)
		seen.mu.Unlock()
	}

	pass("first run", sessionsLivePhrases[:6])
	before := lookAtSessions(ctx, t, st)
	withTickets, withClearance := 0, 0
	for _, s := range before {
		if s.tickets {
			withTickets++
		}
		if s.clearance != "" {
			withClearance++
		}
	}
	logf(t, "MEASUREMENT %d sessions written down, %d with TLS tickets, %d with a clearance",
		len(before), withTickets, withClearance)
	if len(before) == 0 {
		fatalf(t, "no session was written down after the first run")
	}

	pass("second run, a keeper started over", sessionsLivePhrases[6:])
	after := lookAtSessions(ctx, t, st)
	clean, challenged, moved, unused, gone := 0, 0, 0, 0, 0
	for id, b := range before {
		a, still := after[id]
		switch {
		case !still:
			gone++
		case !a.used.After(b.used):
			unused++
		case a.exit != b.exit:
			moved++
		case a.clearance != b.clearance:
			challenged++
		default:
			clean++
		}
	}
	logf(t, "MEASUREMENT of the first run's %d sessions, the second run took up %d on their own exit with "+
		"no new challenge, %d paid a new challenge on their own exit, %d were carried to another exit, "+
		"%d were not needed, %d were given up",
		len(before), clean, challenged, moved, unused, gone)
	if withTickets == 0 {
		errorf(t, "no session was written down with TLS tickets")
	}
	if clean == 0 && challenged > 0 {
		errorf(t, "every session taken up again on its own exit paid a new challenge")
	}
}

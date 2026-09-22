//go:build live

// SPDX-License-Identifier: MIT

package run

// A job run twice through the program's own sessions on the live service, on the
// list as it is — wingate addresses that come and go — with a pause of a minute
// a session, and with the service restarted in the middle of the second run the
// way an update restarts it.
//
// What the second run did with each session the first one left is read from the
// history itself, before and after: a session used again on the same exit with
// the same clearance was taken up without a challenge; a new clearance is a
// challenge paid; a new exit is a session a request carried elsewhere; a session
// not used is one that waited for its address or was not needed; a session gone
// was given up. Timing alone cannot tell these apart — a request walked through
// seven dead addresses takes as long as one that met a challenge.
//
// A session is given up only for refusals Google read and judged, so each one
// given up is told with the refusals that cost it. An address that does not
// carry a request never reads as one: the service says so by dropping the
// connection — measured for a closed port, a hang-up, every refusal a SOCKS5 or
// HTTP proxy gives and an address nobody has — and never with a status of its
// own that could pass for Google's.
//
// The restart is not made by this test. The test watches the service and says
// when it went away and when it came back; whoever runs it restarts the service
// once the second run is under way. What the restart may not cost is the
// sessions: nothing is held against one while the service is away, and the
// sessions the first run left go on answering once it is back.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
	"ламинат купить", "юрист консультация", "билеты на поезд", "фитнес клуб",
	"ноутбук недорого", "грузоперевозки",
}

// sessionLook is a session as the history holds it at one moment: where it goes
// out, the clearance Google gave it, and when it was last used. The exit is
// compared and never printed — it carries the proxy's password.
//
// answered is the keeper's own reading of the record: a session holding cookies
// Google gave it or tickets the service handed out has answered, and waits for
// its address rather than moving to another.
type sessionLook struct {
	exit      string
	clearance string
	used      time.Time
	tickets   bool
	answered  bool
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
		look := sessionLook{exit: s.Exit, used: s.UsedAt, tickets: len(s.Tickets) > 0,
			answered: len(s.Tickets) > 0 || (len(s.Cookies) > 0 && string(s.Cookies) != "[]")}
		for _, c := range cookies {
			if c.Name == "GOOGLE_ABUSE_EXEMPTION" {
				look.clearance = c.Value
			}
		}
		out[s.ID] = look
	}
	return out
}

// sessionBook is the history as the keeper writes it, with a note of every time
// a session answered and every refusal held against one. The history itself
// forgets a session the moment it is given up; the note is how the test can
// still say what cost it.
type sessionBook struct {
	*store.Store
	mu       sync.Mutex
	made     int
	answered map[int64][]time.Time
	refused  map[int64][]time.Time
	dropped  map[int64]bool
}

func newSessionBook(st *store.Store) *sessionBook {
	return &sessionBook{Store: st, answered: map[int64][]time.Time{},
		refused: map[int64][]time.Time{}, dropped: map[int64]bool{}}
}

func (b *sessionBook) NewSession(ctx context.Context, s store.Session) (int64, error) {
	id, err := b.Store.NewSession(ctx, s)
	if err == nil {
		b.mu.Lock()
		b.made++
		b.mu.Unlock()
	}
	return id, err
}

// counts is how many sessions were made and how many given up so far.
func (b *sessionBook) counts() (made, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.made, len(b.dropped)
}

func (b *sessionBook) SessionAnswered(ctx context.Context, id int64, a store.Answer, at time.Time) error {
	err := b.Store.SessionAnswered(ctx, id, a, at)
	b.mu.Lock()
	b.answered[id] = append(b.answered[id], at)
	b.mu.Unlock()
	return err
}

func (b *sessionBook) SessionFailed(ctx context.Context, id int64, at time.Time) (bool, error) {
	dropped, err := b.Store.SessionFailed(ctx, id, at)
	b.mu.Lock()
	b.refused[id] = append(b.refused[id], at)
	if dropped {
		b.dropped[id] = true
	}
	b.mu.Unlock()
	return dropped, err
}

// refusedWithin counts the refusals held against any session between two times.
func (b *sessionBook) refusedWithin(from, to time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, times := range b.refused {
		for _, at := range times {
			if at.After(from) && at.Before(to) {
				n++
			}
		}
	}
	return n
}

// answeredAfter says whether the session answered after the time.
func (b *sessionBook) answeredAfter(id int64, from time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, at := range b.answered[id] {
		if at.After(from) {
			return true
		}
	}
	return false
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

// asks is what came of the asks that brought no page: those Google read and
// judged, by what it answered and when, and those that never reached it.
type asks struct {
	mu     sync.Mutex
	judged []judgedAsk
	never  int
}

type judgedAsk struct {
	at   time.Time
	what string
}

var statusInError = regexp.MustCompile(`HTTP (\d{3})`)

func (a *asks) note(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	class, judged := google.ClassOf(err)
	if !judged {
		a.never++
		return
	}
	what := string(class)
	if m := statusInError.FindStringSubmatch(err.Error()); m != nil {
		what += " " + m[1]
	}
	a.judged = append(a.judged, judgedAsk{at: time.Now(), what: what})
}

// tally is the judged refusals by what they were, most frequent first.
func (a *asks) tally() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	counts := map[string]int{}
	for _, j := range a.judged {
		counts[j.what]++
	}
	var kinds []string
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return counts[kinds[i]] > counts[kinds[j]] })
	var parts []string
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// before is the last judged refusal seen at or before the time and no more than
// a few seconds before it: a refusal is reported to the watcher and then held
// against the session, on the same thread, in that order.
func (a *asks) before(at time.Time) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.judged) - 1; i >= 0; i-- {
		j := a.judged[i]
		if !j.at.After(at.Add(time.Second)) && at.Sub(j.at) < 5*time.Second {
			return j.what
		}
	}
	return "unknown"
}

// serviceWatch asks the service whether it is there twice a second and writes
// down each time it went away and came back.
//
// Any answer at all is the service being there — a refused key is still
// somebody answering — and only a connection nobody takes is its absence. It is
// away only once it has been seen, so a watch started a moment before the
// service reports no outage that began before the run did.
type serviceWatch struct {
	mu      sync.Mutex
	seen    bool
	away    bool
	outages []outage
}

type outage struct{ away, back time.Time }

func watchService(ctx context.Context, control, key string) *serviceWatch {
	w := &serviceWatch{}
	hc := &http.Client{Timeout: 3 * time.Second}
	go func() {
		for ctx.Err() == nil {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, control+"/api/v1/health", nil)
			req.Header.Set("X-API-Key", key)
			resp, err := hc.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			up, now := err == nil, time.Now()
			w.mu.Lock()
			switch {
			case up && !w.seen:
				w.seen = true
			case !up && w.seen && !w.away && ctx.Err() == nil:
				w.away = true
				w.outages = append(w.outages, outage{away: now})
			case up && w.away:
				w.away = false
				w.outages[len(w.outages)-1].back = now
			}
			w.mu.Unlock()
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return w
}

func (w *serviceWatch) seenAway() []outage {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]outage(nil), w.outages...)
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
	book := newSessionBook(st)
	want := sessions.Want{Device: blanktrail.DeviceDesktop, Pause: time.Minute}
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	service := watchService(watchCtx, control, key)
	// Every judged refusal of both runs, for telling what gave a session up: its
	// two refusals in a row may have come one in each run.
	everyRefusal := &asks{}

	// pass runs phrases through a pool of sessions opened afresh, on a keeper
	// started afresh over the same history.
	type passed struct {
		began, ended time.Time
		answeredAt   []time.Time
		reopened     int64
		asks         *asks
	}
	pass := func(label string, phrases []string) passed {
		k := sessions.NewKeeper(book)
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

		var mu sync.Mutex
		out := passed{asks: &asks{}}
		qs := make([]google.Query, len(phrases))
		for i, ph := range phrases {
			qs[i] = google.Query{Text: ph, Country: "ru", Language: "ru"}
		}
		logf(t, "MEASUREMENT %s under way", label)
		madeBefore, droppedBefore := book.counts()
		out.began = time.Now()
		rep := (&Runner{Pool: p, Threads: 2, Keeper: k, Want: want, Watch: func(s Step) {
			if s.Stage != StageAsk {
				return
			}
			if s.Err != nil {
				out.asks.note(s.Err)
				everyRefusal.note(s.Err)
				return
			}
			mu.Lock()
			out.answeredAt = append(out.answeredAt, time.Now())
			mu.Unlock()
		}}).Run(ctx, Job{Queries: qs, Pages: 1})
		out.ended = time.Now()
		answered := 0
		for _, q := range rep.Results {
			if q.Err == nil {
				answered++
			}
		}
		out.reopened = p.Stats().Reopenings
		all, _ := k.Count()
		seen.mu.Lock()
		logf(t, "MEASUREMENT %s: %d of %d answered in %v; %d sessions known; %d ports opened again; attempts: "+
			"%d answered (%d of them after 30 s or more), %d carried nothing",
			label, answered, len(rep.Results), out.ended.Sub(out.began).Round(time.Second), all, out.reopened,
			seen.answered, seen.slow, seen.failed)
		seen.mu.Unlock()
		out.asks.mu.Lock()
		never := out.asks.never
		out.asks.mu.Unlock()
		made, dropped := book.counts()
		logf(t, "MEASUREMENT %s: refusals Google judged: %s; asks that never reached Google: %d; "+
			"%d sessions made, %d given up", label, out.asks.tally(), never, made-madeBefore, dropped-droppedBefore)
		if answered < len(rep.Results) {
			errorf(t, "%s: %d queries were not answered", label, len(rep.Results)-answered)
		}
		return out
	}

	pass("first run", sessionsLivePhrases[:8])
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

	second := pass("second run, a keeper started over", sessionsLivePhrases[8:])
	after := lookAtSessions(ctx, t, st)
	clean, challenged, moved, movedAnswered, unused, gone := 0, 0, 0, 0, 0, 0
	var goneIDs []int64
	for id, b := range before {
		a, still := after[id]
		switch {
		case !still:
			gone++
			goneIDs = append(goneIDs, id)
		case !a.used.After(b.used):
			unused++
		case a.exit != b.exit:
			moved++
			if b.answered {
				movedAnswered++
			}
		case a.clearance != b.clearance:
			challenged++
		default:
			clean++
		}
	}
	logf(t, "MEASUREMENT of the first run's %d sessions, the second run took up %d on their own exit with "+
		"no new challenge, %d paid a new challenge on their own exit, %d were carried to another exit "+
		"(%d of them had answered), %d waited or were not needed, %d were given up",
		len(before), clean, challenged, moved, movedAnswered, unused, gone)
	// The list is the same list throughout, so no address a session answered
	// through ever left it: such a session waits for its address and is never
	// carried to another. Only one that had not answered goes where it can.
	if movedAnswered > 0 {
		errorf(t, "%d sessions that had answered were carried to another exit while their address was "+
			"still in the list", movedAnswered)
	}
	sort.Slice(goneIDs, func(i, j int) bool { return goneIDs[i] < goneIDs[j] })
	for _, id := range goneIDs {
		book.mu.Lock()
		times := append([]time.Time(nil), book.refused[id]...)
		book.mu.Unlock()
		var told []string
		for _, at := range times {
			told = append(told, fmt.Sprintf("%v into the second run (%s)", at.Sub(second.began).Round(time.Second),
				everyRefusal.before(at)))
		}
		logf(t, "MEASUREMENT session %d was given up after the refusals %s", id, strings.Join(told, ", "))
	}

	outages := service.seenAway()
	if len(outages) == 0 {
		logf(t, "MEASUREMENT the service was not seen away during the runs")
	}
	for i, o := range outages {
		if o.back.IsZero() {
			errorf(t, "the service went away at %v into the second run and was not seen back",
				o.away.Sub(second.began).Round(time.Second))
			continue
		}
		answeredAfter := 0
		for _, at := range second.answeredAt {
			if at.After(o.back) {
				answeredAfter++
			}
		}
		oldAnswering := 0
		for id := range before {
			if book.answeredAfter(id, o.back) {
				oldAnswering++
			}
		}
		// A refusal written down in the first seconds of an outage may be
		// Google's answer to a request that arrived just before the service
		// went; the watch sees the service go only on its next look.
		heldAgainst := book.refusedWithin(o.away.Add(2*time.Second), o.back)
		logf(t, "MEASUREMENT outage %d: the service went away %v into the second run and was back %v later; "+
			"%d refusals were held against sessions while it was away; after it came back %d asks were "+
			"answered, and %d of the first run's %d sessions answered again",
			i+1, o.away.Sub(second.began).Round(time.Second), o.back.Sub(o.away).Round(time.Second),
			heldAgainst, answeredAfter, oldAnswering, len(before))
		if heldAgainst > 0 {
			errorf(t, "outage %d: %d refusals were held against sessions while the service was away",
				i+1, heldAgainst)
		}
		if !o.back.After(second.ended) {
			if answeredAfter == 0 {
				errorf(t, "outage %d: nothing was answered after the service came back", i+1)
			}
			if oldAnswering == 0 {
				errorf(t, "outage %d: none of the first run's sessions answered after the service came back", i+1)
			}
		}
	}
	if withTickets == 0 {
		errorf(t, "no session was written down with TLS tickets")
	}
	if clean == 0 && challenged > 0 {
		errorf(t, "every session taken up again on its own exit paid a new challenge")
	}
	if gone == len(before) {
		errorf(t, "the second run gave up every session the first one left")
	}
}

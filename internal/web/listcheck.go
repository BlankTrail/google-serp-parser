// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// checksEgress is what a check asks. It is an interface so the test does not
// need a service, and because the only thing wanted of the client here is the
// one call.
type checksEgress interface {
	TestEgress(ctx context.Context, eg blanktrail.Egress, hop blanktrail.FirstHop,
		checks ...string) (map[string]blanktrail.CheckResult, error)
}

const (
	// listCheckSample is how many addresses are asked about when the form names
	// no number. A hundred is enough for a share to mean something on a list of
	// fifteen thousand and short enough to watch.
	listCheckSample = 100
	// listCheckThreads is how many are asked at once by default. The check is a
	// question to the service, which answers it by making a request of its own,
	// so this is the width of the road being measured rather than work on this
	// machine.
	listCheckThreads = 20
	// listCheckMostThreads and listCheckMostSample bound what a form may ask
	// for: a check is a load on the same service the jobs run through, and a
	// box somebody typed four zeroes into should not become one.
	listCheckMostThreads = 200
	listCheckMostSample  = 2000
	// listCheckPatience is how long one address is given before the check gives
	// up on it. The service's own attempt is bounded well inside this; what
	// this bounds is a service that has stopped answering at all.
	listCheckPatience = 30 * time.Second
)

// roadTally is what one road carried.
type roadTally struct {
	// Asked is how many addresses were put to the service along this road, OK
	// how many it reached, and Refused how many it could not. Broke counts the
	// ones the service would not answer about at all, which is neither.
	Asked   int
	OK      int
	Refused int
	Broke   int
	// Took is the time spent along this road, so an address can be priced
	// against the other road rather than only counted.
	Took time.Duration
}

// each is the time one address cost along this road.
func (t roadTally) each() time.Duration {
	if t.Asked == 0 {
		return 0
	}
	return t.Took / time.Duration(t.Asked)
}

// share is how many of the asked addresses answered, as a percentage.
func (t roadTally) share() int {
	if t.Asked == 0 {
		return 0
	}
	return t.OK * 100 / t.Asked
}

// listCheck is one profile's addresses asked about along the road its ports
// will take, and along the one they will not.
//
// Both roads are measured because on a list reached through a first hop they
// give opposite answers, and only one of them is the answer about this profile:
// measured live on a list of fifteen thousand, the service reached 8 of 30
// addresses asked directly and 23 of 30 asked through the hop. A check that
// took the road nobody uses would call a working list dead, and whoever read it
// would go looking for another list.
type listCheck struct {
	mu sync.Mutex
	// profile is whose check this is, and name what it was called, so a reading
	// left on the screen cannot be read under another profile's heading.
	profile int64
	name    string
	// running says one is going on now; began and ended are when.
	running bool
	began   time.Time
	ended   time.Time
	// holds is how many addresses the list turned out to have and sample how
	// many of them are being asked about.
	holds  int
	sample int
	// threads is how many were asked at once.
	threads int
	// hop is the first hop the ports of this profile take, empty where they go
	// to their addresses directly.
	hop blanktrail.FirstHop
	// through and direct are the two roads. Where the profile names no first
	// hop there is only one road and only direct is filled.
	through roadTally
	direct  roadTally
	// done counts the addresses finished along both roads together, for the
	// line the screen shows while it runs.
	done int
	// fault is why the check could not be made at all: a list that would not
	// load, a connection that is not set up.
	fault string
	// stop ends the check early, and is nil when none is running.
	stop context.CancelFunc
}

// checkReading is what the screen draws. It is a copy taken under the lock: the
// check goes on writing while the page is rendered.
type checkReading struct {
	// Asked says a check has been made or is being made, which is what decides
	// whether anything is drawn at all.
	Asked   bool
	Running bool
	// Of is the profile the reading belongs to, and OfName its name.
	Of     int64
	OfName string
	// Holds is how many addresses the list has, Sample how many are being asked
	// about, Done how many questions have been answered so far, and Threads how
	// many are asked at once.
	Holds   int
	Sample  int
	Done    int
	Threads int
	// Hop says the ports of this profile take a first hop, and HopName what it
	// is — a gateway by name, or the word for a proxy, never the proxy itself:
	// a first hop carries a password.
	Hop     bool
	Gateway string
	// Through is the road the ports take and Direct the one they do not. Where
	// there is no first hop the two are the same road and only Direct is drawn.
	Through checkRoad
	Direct  checkRoad
	// Took is how long the whole check ran.
	Took string
	// Fault is why there is nothing to show.
	Fault string
}

// checkRoad is one road, drawn.
type checkRoad struct {
	Asked   int
	OK      int
	Refused int
	Broke   int
	Share   int
	Each    string
}

// Reading is the check as it stands this instant.
func (c *listCheck) Reading() checkReading {
	if c == nil {
		return checkReading{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.began.IsZero() && c.fault == "" {
		return checkReading{}
	}
	out := checkReading{
		Asked: true, Running: c.running, Of: c.profile, OfName: c.name,
		Holds: c.holds, Sample: c.sample, Done: c.done, Threads: c.threads,
		Hop: !c.hop.IsZero(), Gateway: c.hop.Gateway, Fault: c.fault,
		Through: roadShown(c.through), Direct: roadShown(c.direct),
	}
	end := c.ended
	if c.running {
		end = time.Now()
	}
	if !c.began.IsZero() && !end.IsZero() {
		out.Took = end.Sub(c.began).Round(time.Second).String()
	}
	return out
}

func roadShown(t roadTally) checkRoad {
	return checkRoad{Asked: t.Asked, OK: t.OK, Refused: t.Refused, Broke: t.Broke,
		Share: t.share(), Each: t.each().Round(time.Millisecond).String()}
}

// Running says a check is going on, which is what makes the screen ask for
// itself again.
func (c *listCheck) Running() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// Stop ends whatever is running.
func (c *listCheck) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	stop := c.stop
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// listCheckAsk is one press of the button: whose list, how much of it, and how
// many at once.
type listCheckAsk struct {
	Profile store.Profile
	Sample  int
	Threads int
}

// start begins a check and returns at once. A check already running is left
// alone: two of them would be two loads on one service reported as one reading.
func (c *listCheck) start(ctx context.Context, cl checksEgress, ask listCheckAsk,
	load func(context.Context) ([]blanktrail.Egress, error), done func()) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	// Field by field rather than over the whole of it: the lock is part of this
	// struct, and assigning a fresh one over a held lock unlocks something else.
	hop, err := blanktrail.ParseFirstHop(ask.Profile.FirstHop)
	c.profile, c.name = ask.Profile.ID, ask.Profile.Name
	c.began, c.ended = time.Now(), time.Time{}
	c.threads, c.sample = threadsAsked(ask.Threads), sampleAsked(ask.Sample)
	c.hop, c.holds, c.done, c.fault = hop, 0, 0, ""
	c.through, c.direct = roadTally{}, roadTally{}
	c.running = true
	if err != nil {
		c.running, c.fault = false, "proxies.check.hop"
		c.mu.Unlock()
		return
	}
	ctx, stop := context.WithCancel(ctx)
	c.stop = stop
	c.mu.Unlock()

	go func() {
		defer stop()
		defer func() {
			c.mu.Lock()
			c.running, c.ended = false, time.Now()
			c.mu.Unlock()
			if done != nil {
				done()
			}
		}()
		egresses, err := load(ctx)
		if err != nil {
			c.mu.Lock()
			c.fault = "proxies.check.list"
			c.mu.Unlock()
			return
		}
		c.run(ctx, cl, egresses)
	}()
}

// run asks about the sample along both roads.
func (c *listCheck) run(ctx context.Context, cl checksEgress, ups []blanktrail.Egress) {
	c.mu.Lock()
	want, threads, hop := c.sample, c.threads, c.hop
	c.holds = len(ups)
	if want > len(ups) {
		want = len(ups)
		c.sample = want
	}
	c.mu.Unlock()
	if want == 0 {
		c.mu.Lock()
		c.fault = "proxies.check.empty"
		c.mu.Unlock()
		return
	}

	// Which addresses. A list is not sorted by anything that matters, but the
	// first hundred of it are the same hundred every time — and a list whose
	// first hundred are dead is a list somebody would stop using on the
	// strength of a hundred addresses out of fifteen thousand. So they are
	// spread over the whole of it.
	chosen := spread(ups, want)

	// The road the ports take first. Where there is no first hop the two roads
	// are the same one, and asking twice would be asking the same question
	// twice and reporting it as two answers.
	if !hop.IsZero() {
		c.along(ctx, cl, chosen, hop, threads, &c.through)
	}
	c.along(ctx, cl, chosen, blanktrail.FirstHop{}, threads, &c.direct)
}

// along asks about every address along one road, threads of them at a time.
func (c *listCheck) along(ctx context.Context, cl checksEgress, ups []blanktrail.Egress,
	hop blanktrail.FirstHop, threads int, into *roadTally) {
	work := make(chan blanktrail.Egress)
	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for eg := range work {
				at := time.Now()
				one, cancel := context.WithTimeout(ctx, listCheckPatience)
				res, err := cl.TestEgress(one, eg, hop, "http")
				cancel()
				took := time.Since(at)

				ok := verdictOf(res)
				c.mu.Lock()
				into.Asked++
				into.Took += took
				c.done++
				switch {
				case err != nil:
					into.Broke++
				case ok:
					into.OK++
				default:
					into.Refused++
				}
				c.mu.Unlock()
			}
		}()
	}
	for _, up := range ups {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return
		case work <- up:
		}
	}
	close(work)
	wg.Wait()
}

// verdictOf reads the service's answer: every check it made has to have passed.
//
// What the service said about the road is deliberately not kept: it quotes the
// address it went through, and an address on a list is not a thing to draw on a
// screen somebody may be looking at over a shoulder or photographing.
//
// An answer with no checks in it is not a pass. The service answers that way
// when it skipped everything it was asked, and reading it as success would
// report a list nobody tested as a list that works.
func verdictOf(res map[string]blanktrail.CheckResult) bool {
	if len(res) == 0 {
		return false
	}
	for _, one := range res {
		if !one.OK {
			return false
		}
	}
	return true
}

// spread takes want addresses from across the whole list rather than from the
// top of it, and shuffles them so two checks of one list do not walk it in the
// same order — a list handed out in blocks answers differently at its ends.
func spread(ups []blanktrail.Egress, want int) []blanktrail.Egress {
	if want >= len(ups) {
		out := append([]blanktrail.Egress(nil), ups...)
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	step := len(ups) / want
	out := make([]blanktrail.Egress, 0, want)
	for i := 0; len(out) < want && i*step < len(ups); i++ {
		out = append(out, ups[i*step])
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// threadsAsked and sampleAsked are what the form asked for, within what a check
// may be.
func threadsAsked(n int) int {
	switch {
	case n <= 0:
		return listCheckThreads
	case n > listCheckMostThreads:
		return listCheckMostThreads
	}
	return n
}

func sampleAsked(n int) int {
	switch {
	case n <= 0:
		return listCheckSample
	case n > listCheckMostSample:
		return listCheckMostSample
	}
	return n
}

// sampleField and threadsField are how the form names what to check and how
// widely.
const (
	sampleField      = "sample"
	checkThreadField = "checkthreads"
	// listScheme is what a line with no scheme on it is taken to be. It is the
	// same answer the run gives, because a check that read the list differently
	// from the run would be checking other addresses than the ones the ports
	// will stand on.
	listScheme = "socks5"
)

// checkList puts a profile's addresses to the service along the road that
// profile's ports take, and along the one they do not.
//
// It answers at once and the check goes on behind it: a list of a thousand
// addresses takes minutes, and a page that waited for it would be a page the
// browser gave up on. What it started is drawn on the profile's own screen,
// which asks for itself again while it runs.
func (s *Server) checkList(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r, profileField)
	back := proxiesAt + "?" + profileField + "=" + strconv.FormatInt(id, 10)

	profile, err := s.store.Profile(r.Context(), id)
	if err != nil {
		s.log.Error("a profile could not be read for a check", "profile", id, "error", err)
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	saved, _ := s.current()
	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		s.checking.refuse(profile, "proxies.check.connection")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	ask := listCheckAsk{Profile: profile,
		Sample:  atoiField(r, sampleField),
		Threads: atoiField(r, checkThreadField)}
	// The context is the server's rather than the request's: the request is
	// answered now and the check goes on after it.
	s.checking.start(context.Background(), client, ask, egressesOf(profile), nil)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// stopCheckList ends a check somebody started and is no longer waiting for.
func (s *Server) stopCheckList(w http.ResponseWriter, r *http.Request) {
	s.checking.Stop()
	http.Redirect(w, r, proxiesAt+"?"+profileField+"="+strconv.FormatInt(atoi64(r, profileField), 10),
		http.StatusSeeOther)
}

// egressesOf is where this profile's exits come from: the addresses of its
// list, or the gateways it names.
func egressesOf(p store.Profile) func(context.Context) ([]blanktrail.Egress, error) {
	return func(ctx context.Context) ([]blanktrail.Egress, error) {
		if p.Kind == settings.ProxyGateways {
			out := make([]blanktrail.Egress, 0, len(p.Gateways))
			for _, name := range p.Gateways {
				out = append(out, blanktrail.Egress{Gateway: name})
			}
			return out, nil
		}
		ups, _, err := (blanktrail.Source{
			Kind: p.Kind, Location: p.Location, DefaultScheme: listScheme,
		}).Load(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]blanktrail.Egress, 0, len(ups))
		for _, up := range ups {
			out = append(out, blanktrail.Egress{Upstream: up.URL()})
		}
		return out, nil
	}
}

// refuse records that a check could not be started at all, so the screen says
// why rather than showing nothing after a press.
func (c *listCheck) refuse(p store.Profile, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return
	}
	c.profile, c.name = p.ID, p.Name
	c.began, c.ended = time.Now(), time.Now()
	c.holds, c.done, c.sample = 0, 0, 0
	c.through, c.direct = roadTally{}, roadTally{}
	c.fault = why
}

// atoiField is a whole number out of a form box, and nought where the box was
// empty or held something else.
func atoiField(r *http.Request, name string) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
	if err != nil {
		return 0
	}
	return n
}

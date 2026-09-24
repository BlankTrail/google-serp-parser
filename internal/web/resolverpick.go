// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// A profile's way of resolving names, picked by trying it on the profile's own
// addresses.
//
// Handing the name to the proxy costs nothing: no lookup before the request, no
// connection of its own. It is also at the proxy's mercy — a residential gateway
// refused www.gstatic.com by name on every exit, and a port that handed it that
// name never loaded the script of Google's check. The service's own ways of
// resolving get round such a proxy, and each lookup is a connection down the
// whole road; on a list reached through a first hop that was a storm the
// provider answered by shutting the road. So the cheap way is kept wherever it
// reaches everything a search needs, and the others are tried only where it
// does not — in the order the operator set: the exit provider's resolvers, then
// the public pool.

// pickHosts are what a search and the check in front of it need to reach: the
// search itself, and the hosts the page of Google's check loads its script, its
// pictures and its fonts from. The first is also how an address is known to be
// alive at all.
var pickHosts = []string{
	"https://www.google.com/generate_204",
	"https://www.gstatic.com/generate_204",
	"https://ssl.gstatic.com/generate_204",
	"https://fonts.gstatic.com/",
}

// pickOrder is the ways tried, in turn, and the first to reach everything is
// kept. Delegating is first because it is the one that costs nothing.
var pickOrder = []string{blanktrail.ResolverDelegate, blanktrail.ResolverISP, blanktrail.ResolverPool}

const (
	// pickSample is how many addresses a way is tried on: enough that one dead
	// address does not decide it, few enough to be over in a minute or two.
	pickSample = 20
	// pickThreads is how many are tried at once. Each is a port opened and a
	// handful of requests down the road the jobs take, and a road a provider
	// watches is not one to be asked widely.
	pickThreads = 5
	// pickShare is how much of what answered a way must reach every host, in
	// per cent. Not all of it: one exit that drops one connection is not a way
	// of resolving that blocks a host.
	pickShare = 80
)

// pickClient is what the pick asks of the service. An interface, so a test
// stands in for a service and its addresses.
type pickClient interface {
	ProbeHosts(ctx context.Context, spec blanktrail.PortSpec, eg blanktrail.Egress, ca *x509.CertPool,
		urls []string) ([]blanktrail.HostProbe, error)
	FetchCAPool(ctx context.Context) (*x509.CertPool, error)
}

// pickTally is what one way reached on the addresses it was tried on.
type pickTally struct {
	Method string
	// Asked is how many addresses it was tried on, and Live how many of them
	// reached the search host — the ones whose answer about the other hosts
	// means anything.
	Asked int
	Live  int
	// Reached is, host by host in the order of pickHosts, how many of the live
	// addresses reached it.
	Reached []int
}

// passes says the way reached every host on nearly every address that was
// alive under it.
func (t pickTally) passes() bool {
	if t.Live == 0 {
		return false
	}
	for _, n := range t.Reached {
		if n*100 < t.Live*pickShare {
			return false
		}
	}
	return true
}

// picked is the first of the tallies, in the order they were tried, that
// passes, and empty when none does.
func picked(tallies []pickTally) string {
	for _, t := range tallies {
		if t.passes() {
			return t.Method
		}
	}
	return ""
}

// resolverPick is the last pick made, or the one going on now.
type resolverPick struct {
	mu      sync.Mutex
	profile int64
	running bool
	began   time.Time
	ended   time.Time
	holds   int
	sample  int
	// trying is the way being tried now, and done how many addresses it has
	// been tried on so far.
	trying string
	done   int
	// tallies are the ways tried, in order; chosen is the one kept, and saved
	// says it was written into the profile.
	tallies []pickTally
	chosen  string
	saved   bool
	fault   string
}

// pickReading is the pick as the screen draws it.
type pickReading struct {
	Asked   bool
	Running bool
	Of      int64
	Holds   int
	Sample  int
	Trying  string
	Done    int
	Hosts   []string
	Tallies []pickTally
	Chosen  string
	Saved   bool
	Fault   string
	Took    string
}

// Reading is the pick as it stands this instant.
func (p *resolverPick) Reading() pickReading {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.began.IsZero() {
		return pickReading{}
	}
	out := pickReading{Asked: true, Running: p.running, Of: p.profile, Holds: p.holds,
		Sample: p.sample, Trying: p.trying, Done: p.done, Hosts: hostsOf(pickHosts),
		Chosen: p.chosen, Saved: p.saved, Fault: p.fault}
	out.Tallies = append(out.Tallies, p.tallies...)
	end := p.ended
	if p.running {
		end = time.Now()
	}
	out.Took = end.Sub(p.began).Round(time.Second).String()
	return out
}

// Running says a pick is going on, which is what makes the screen ask for
// itself again.
func (p *resolverPick) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// hostsOf is the host of each address, which is what the screen names.
func hostsOf(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, one := range urls {
		if u, err := url.Parse(one); err == nil {
			out = append(out, u.Hostname())
		}
	}
	return out
}

// start begins a pick and returns at once; one already running is left alone.
func (p *resolverPick) start(ctx context.Context, cl pickClient, prof store.Profile,
	load func(context.Context) ([]blanktrail.Egress, error),
	save func(context.Context, store.Profile) error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.profile = prof.ID
	p.began, p.ended = time.Now(), time.Time{}
	p.holds, p.sample, p.trying, p.done = 0, 0, "", 0
	p.tallies, p.chosen, p.saved, p.fault = nil, "", false, ""
	hop, err := blanktrail.ParseFirstHop(prof.FirstHop)
	if err != nil {
		p.fault, p.ended = "proxies.check.hop", time.Now()
		p.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			p.running, p.ended, p.trying = false, time.Now(), ""
			p.mu.Unlock()
		}()
		p.run(ctx, cl, prof, hop, load, save)
	}()
}

func (p *resolverPick) run(ctx context.Context, cl pickClient, prof store.Profile, hop blanktrail.FirstHop,
	load func(context.Context) ([]blanktrail.Egress, error),
	save func(context.Context, store.Profile) error) {
	all, err := load(ctx)
	if err != nil {
		p.fail("proxies.check.list")
		return
	}
	chosen := spread(all, pickSample)
	p.mu.Lock()
	p.holds, p.sample = len(all), len(chosen)
	p.mu.Unlock()
	if len(chosen) == 0 {
		p.fail("proxies.check.empty")
		return
	}
	// The service's own certificate, so the leg to the port is checked the way
	// a job's is. Without it the probe still says what it came to say — whether
	// a host was reached — so it goes on.
	ca, _ := cl.FetchCAPool(ctx)

	for _, method := range pickOrder {
		tally := p.try(ctx, cl, prof, hop, ca, method, chosen)
		p.mu.Lock()
		p.tallies = append(p.tallies, tally)
		p.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if tally.passes() {
			break
		}
	}

	p.mu.Lock()
	tallies := append([]pickTally(nil), p.tallies...)
	p.mu.Unlock()
	way := picked(tallies)
	if way == "" {
		why := "proxies.pick.none"
		if tallies[0].Live == 0 {
			why = "proxies.pick.dead"
		}
		p.fail(why)
		return
	}
	// A way other than delegating is kept because delegating could not reach a
	// host, which a proxy does by refusing its name — so the name is kept from
	// the proxy altogether, or the service's fallback to it the first time an
	// address fails to answer would walk straight back into the refusal.
	prof.Resolver, prof.StrictBypass = way, way != blanktrail.ResolverDelegate
	err = save(ctx, prof)
	p.mu.Lock()
	p.chosen, p.saved = way, err == nil
	if err != nil {
		p.fault = "proxies.pick.unsaved"
	}
	p.mu.Unlock()
}

// try is one way of resolving tried on every chosen address, a few at a time.
func (p *resolverPick) try(ctx context.Context, cl pickClient, prof store.Profile, hop blanktrail.FirstHop,
	ca *x509.CertPool, method string, chosen []blanktrail.Egress) pickTally {
	p.mu.Lock()
	p.trying, p.done = method, 0
	p.mu.Unlock()

	spec := blanktrail.DefaultPortSpec()
	spec.Protocol = blanktrail.ProtocolOr(prof.Protocol)
	spec.FirstHop = hop
	spec.AllowMITMUpstream = prof.AllowMITM
	spec.VDNSMode = prof.VDNSMode
	// Nothing here is a search, so there is no check to solve.
	spec.JSSolver = false
	spec.Resolver = method
	// Tried on its own: a way that could not resolve a name and fell back to
	// handing it to the proxy would be delegating under another name, and its
	// answer would be delegating's.
	spec.VDNSStrictBypass = method != blanktrail.ResolverDelegate

	tally := pickTally{Method: method, Reached: make([]int, len(pickHosts))}
	var mu sync.Mutex
	work := make(chan blanktrail.Egress)
	var wg sync.WaitGroup
	for i := 0; i < pickThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for eg := range work {
				got, err := cl.ProbeHosts(ctx, spec, eg, ca, pickHosts)
				mu.Lock()
				tally.Asked++
				if err == nil && len(got) == len(pickHosts) && got[0].Reached() {
					tally.Live++
					for i, one := range got {
						if one.Reached() {
							tally.Reached[i]++
						}
					}
				}
				mu.Unlock()
				p.mu.Lock()
				p.done++
				p.mu.Unlock()
			}
		}()
	}
feed:
	for _, eg := range chosen {
		select {
		case <-ctx.Done():
			break feed
		case work <- eg:
		}
	}
	close(work)
	wg.Wait()
	return tally
}

func (p *resolverPick) fail(why string) {
	p.mu.Lock()
	p.fault = why
	p.mu.Unlock()
}

// refuse records that a pick could not be started at all, so the screen says
// why rather than showing nothing after a press.
func (p *resolverPick) refuse(prof store.Profile, why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return
	}
	p.profile, p.began, p.ended = prof.ID, time.Now(), time.Now()
	p.holds, p.sample, p.trying, p.done = 0, 0, "", 0
	p.tallies, p.chosen, p.saved, p.fault = nil, "", false, why
}

// pickResolverAt is where the button that picks a profile's way of resolving
// names sends its form.
const pickResolverAt = "/proxies/resolver"

// pickResolver tries the ways of resolving names on the profile's own
// addresses and keeps the cheapest one that reaches everything a search needs.
// It answers at once: the pick goes on behind it, and the profile's screen asks
// for itself again while it does.
func (s *Server) pickResolver(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r, profileField)
	back := proxiesAt + "?" + profileField + "=" + strconv.FormatInt(id, 10)
	prof, err := s.store.Profile(r.Context(), id)
	if err != nil {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	saved, _ := s.current()
	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		s.picking.refuse(prof, "proxies.check.connection")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	s.picking.start(context.Background(), client, prof, egressesOf(prof), s.store.SaveProfile)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrPoolExhausted is returned when every port the acquire could have used has
// been quarantined. For Acquire that is the whole pool; for AcquireSpec it is
// every port of the named template, and the error wraps this sentinel with that
// name — the rest of the pool may be perfectly healthy at that moment, and an
// unqualified "the pool is quarantined" would contradict what Stats() reports.
// Both forms answer errors.Is(err, ErrPoolExhausted).
var ErrPoolExhausted = errors.New("blanktrail: every candidate port is quarantined")

// ErrUnknownSpec is returned by AcquireSpec when no port in the pool was opened
// under the requested template name.
var ErrUnknownSpec = errors.New("blanktrail: no port carries this spec name")

// DeriveCooldown computes the default port cooldown from the ring the caller
// described: with portsPerThread ports and a per-request delay somewhere in
// [delayMin, delayMax], a port in a rigid ring would come back after
// portsPerThread × the midpoint of that range. A shared pool keeps that same
// guarantee while wasting less time idle.
func DeriveCooldown(portsPerThread int, delayMin, delayMax time.Duration) time.Duration {
	if portsPerThread < 1 {
		portsPerThread = 1
	}
	if delayMax < delayMin {
		delayMin, delayMax = delayMax, delayMin
	}
	mid := (delayMin + delayMax) / 2
	return time.Duration(portsPerThread) * mid
}

// PoolConfig configures a pool of worker ports.
type PoolConfig struct {
	// Client is the control-API client. Required.
	Client *Client
	// ProxyHost is the host the opened ports listen on. Defaults to the control
	// host, which is right when the parser runs on the same machine.
	ProxyHost string

	// Threads × PortsPerThread is the pool size. Ports are shared across all
	// threads rather than pinned to one, so a fast thread never idles on its own
	// cold port while another thread's ports sit free.
	Threads        int
	PortsPerThread int

	// Spec is the fingerprint/behaviour template every port is opened with.
	Spec PortSpec
	// Specs, when non-empty, opens ports under several named templates and lets
	// AcquireSpec ask for one by name. Spec is ignored when this is set.
	//
	// How the ports are shared out, in full:
	//
	//   - Every named template is guaranteed one port before weight is looked
	//     at, so no template is ever rounded away.
	//   - What is left is split by NamedSpec.Weight using largest-remainder
	//     allocation; equal remainders are broken in declaration order.
	//   - Templates are interleaved along the opening order, so a pool that
	//     fails half way through opening still holds a mix rather than all of
	//     one kind.
	//   - Every template is spread over Channels as evenly as the counts allow,
	//     so a template is never confined to one egress: a comparison between
	//     two templates has to be a comparison of the templates, not of the IPs
	//     they happened to share.
	//   - The whole layout is a pure function of this configuration. Two runs of
	//     the same config produce the same one, which is what makes their
	//     numbers comparable.
	//
	// NewPool returns ErrTooFewPorts when Size() is smaller than len(Specs):
	// dropping a template would leave the run measuring one device profile and
	// labelling it with two.
	Specs []NamedSpec
	// Channels are the egress sources ports are spread over. Empty means direct.
	Channels []Channel

	// PortRange, when its upper bound is non-zero, restricts opened ports to
	// [PortRange[0], PortRange[1]]. Otherwise the proxy suggests free ports.
	PortRange [2]int

	// CA is the proxy's MITM CA. Insecure skips verification instead — only for
	// tests, never in production.
	CA       *x509.CertPool
	Insecure bool

	// DelayMin and DelayMax bound the pause a caller should leave between its
	// own requests. The pool does not enforce them; it uses them to derive the
	// cooldown and offers NextDelay so callers have one place to ask.
	DelayMin, DelayMax time.Duration
	// Cooldown is the minimum gap between two requests on the SAME port. Zero
	// derives it from PortsPerThread and the delay range.
	Cooldown time.Duration

	// RequestTimeout bounds one request through a leased port, retries included
	// (default 300s). A single request through a proxy port can legitimately
	// take minutes to produce a response, and this is the client-side ceiling —
	// well above the port's own 60s default on the proxy side. Keep it
	// comfortably above MaxRetriesPerReq × maxRetryAfter, or a throttled target
	// will exhaust the deadline in pauses before a retry can run.
	RequestTimeout time.Duration
	// MaxRetriesPerReq is how many times the ladder retries a blocked request
	// before handing the blocked response back (default 4).
	MaxRetriesPerReq int
	// RotateAfterFailures is how many consecutive failed attempts a port may
	// collect before its egress is replaced (default 3). A failure is any non-2xx
	// response or a transport error — this package does not reason about why.
	RotateAfterFailures int
	// CountFailure decides whether a non-2xx response counts towards the
	// consecutive-failure count that replaces a port's egress. Nil counts every
	// non-2xx.
	//
	// The pool does not know why a request failed and does not try to: that
	// belongs to whoever knows the target. But some statuses mean "your request
	// was wrong", and rotating the egress on those burns a proxy for a fault
	// that travels with the request.
	CountFailure func(status int) bool
	// RenewAfterRequests renews a port's whole identity — fingerprint, egress IP
	// and cookie jar — once it has served this many requests. Zero disables it.
	RenewAfterRequests int
	// RenewAfterInterval renews a port's identity once this much time has passed
	// since the last renewal. Zero disables it.
	RenewAfterInterval time.Duration
	// MaxPortStrikes is how many times a port may spend its whole retry budget
	// without getting a usable response before it is quarantined (default 3).
	MaxPortStrikes int

	// Now and Sleep are clock seams for tests. Both default to the real clock.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// Size is how many ports the pool will open.
func (cfg PoolConfig) Size() int {
	t, k := cfg.Threads, cfg.PortsPerThread
	if t < 1 {
		t = 1
	}
	if k < 1 {
		k = 1
	}
	return t * k
}

// poolPort is one opened port with its dedicated transport and identity state.
type poolPort struct {
	num    int
	base   *http.Transport
	client *http.Client
	ch     Channel

	// specName is the named template this port was opened under; empty when the
	// pool has a single unnamed template. spec is that template, kept on the
	// port so a renewal reopens it as itself rather than as the pool default.
	specName string
	spec     PortSpec

	mu          sync.Mutex
	eg          Egress
	lastUsed    time.Time
	leased      bool
	quarantined bool
	broken      bool // renewal closed it but could not reopen it
	failures    int  // consecutive failed attempts; any success clears it
	requests    int
	strikes     int
	renewedAt   time.Time
	session     uint64 // bumped whenever the port's identity changes
}

func (pt *poolPort) egress() Egress {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return pt.eg
}

func (pt *poolPort) setEgress(eg Egress) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.eg = eg
}

// Pool is a set of worker ports, each a distinct identity (fingerprint, cookie
// jar and egress IP). Acquire leases the coldest ready port; the leased
// *http.Client retries, refreshes the fingerprint and rotates the egress IP on a
// block, transparently.
type Pool struct {
	cfg   PoolConfig
	cl    *Client
	mixer *Mixer
	host  string
	cool  time.Duration

	mu        sync.Mutex
	ports     []*poolPort
	byNum     map[int]*poolPort
	closed    bool // set inside closeOnce.Do; renewIfDue checks it before reopening a port
	closeOnce sync.Once
	stats     Stats
}

// SpecStats is per-template port accounting, so a progress screen can say
// "16 ports → 8 desktop, 8 mobile" instead of one opaque total, and so a
// template running out of healthy ports is visible before the run's numbers
// quietly become one-sided.
type SpecStats struct {
	Ports       int
	Available   int
	Quarantined int
}

// Stats is a snapshot of pool activity, for the progress screen and the logs.
type Stats struct {
	Ports       int // ports the pool holds
	Available   int // ports not quarantined
	Quarantined int

	// Specs breaks the counts above down by template name. It is empty when the
	// pool has a single unnamed template.
	Specs map[string]SpecStats

	Requests         int64
	ProfileRotations int64
	EgressRotations  int64
	Renewals         int64
	Quarantines      int64
}

// Stats returns a snapshot of pool activity.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stats
	st.Ports = len(p.ports)
	st.Quarantined = 0

	var bySpec map[string]SpecStats
	for _, pt := range p.ports {
		pt.mu.Lock()
		quarantined, name := pt.quarantined, pt.specName
		pt.mu.Unlock()
		if quarantined {
			st.Quarantined++
		}
		if name == "" {
			continue
		}
		if bySpec == nil {
			bySpec = map[string]SpecStats{}
		}
		s := bySpec[name]
		s.Ports++
		if quarantined {
			s.Quarantined++
		} else {
			s.Available++
		}
		bySpec[name] = s
	}
	st.Available = st.Ports - st.Quarantined
	st.Specs = bySpec
	return st
}

// NewPool opens Threads × PortsPerThread ports and returns a ready pool. On any
// failure it closes every port it had already opened.
func NewPool(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	if cfg.Client == nil {
		return nil, errors.New("blanktrail: PoolConfig.Client is required")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 300 * time.Second
	}
	if cfg.MaxRetriesPerReq <= 0 {
		cfg.MaxRetriesPerReq = 4
	}
	if cfg.Spec.Browser == "" {
		cfg.Spec = DefaultPortSpec()
	}
	// cfg is a copy but its slice is not: defaulting the templates in place
	// would reach back into the caller's own slice.
	if len(cfg.Specs) > 0 {
		specs := append([]NamedSpec(nil), cfg.Specs...)
		for i := range specs {
			// A template that says nothing at all wants the defaults. A template
			// that says something but forgets Browser cannot be completed: the
			// booleans have no "unset" state, so a field-wise merge would invent
			// answers, and swapping the whole struct for DefaultPortSpec() would
			// throw the caller's OS away and open a Windows desktop port that
			// every label in the run then calls "mobile". Refuse loudly instead.
			if specs[i].Spec == (PortSpec{}) {
				specs[i].Spec = DefaultPortSpec()
				continue
			}
			if specs[i].Spec.Browser == "" {
				return nil, fmt.Errorf("blanktrail: spec %q sets some PortSpec fields but leaves Browser empty; "+
					"start from DefaultPortSpec() and change what differs, or leave Spec entirely zero to get it whole",
					specs[i].Name)
			}
		}
		cfg.Specs = specs
	}
	if cfg.DelayMin <= 0 {
		cfg.DelayMin = 3 * time.Second
	}
	if cfg.DelayMax < cfg.DelayMin {
		cfg.DelayMax = cfg.DelayMin
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.RotateAfterFailures <= 0 {
		cfg.RotateAfterFailures = 3
	}
	if cfg.MaxPortStrikes <= 0 {
		cfg.MaxPortStrikes = 3
	}

	cool := cfg.Cooldown
	if cool <= 0 {
		cool = DeriveCooldown(cfg.PortsPerThread, cfg.DelayMin, cfg.DelayMax)
	}

	channels := cfg.Channels
	if len(channels) == 0 {
		channels = []Channel{NewDirectChannel("direct")}
	}

	host := cfg.ProxyHost
	if host == "" {
		host = cfg.Client.ControlHost()
	}

	p := &Pool{
		cfg:   cfg,
		cl:    cfg.Client,
		mixer: NewMixer(channels...),
		host:  host,
		cool:  cool,
		byNum: map[int]*poolPort{},
	}

	size := cfg.Size()
	specNames, err := planSpecs(cfg.Specs, size)
	if err != nil {
		return nil, err
	}
	// The mixer decides how many ports each channel gets; spreadSpecs decides
	// which ports those are, so that no template ends up confined to one egress.
	groups := groupChannels(p.mixer.Assign(size))
	counts := make([]int, len(groups))
	total := 0
	for i, g := range groups {
		counts[i] = len(g)
		total += len(g)
	}
	if total != size {
		// Assign hands back nothing once every channel has been penalised to a
		// weight of zero.
		return nil, errors.New("blanktrail: no usable egress channel")
	}
	layout := spreadSpecs(specNames, counts)
	taken := make([]int, len(groups))

	used := map[int]bool{}
	for i := 0; i < size; i++ {
		g := layout[i]
		ch := groups[g][taken[g]]
		taken[g]++
		eg, ok := ch.Next()
		if !ok {
			_ = p.Close()
			return nil, fmt.Errorf("blanktrail: channel %q has no egress to hand out", ch.Name())
		}
		num, err := p.pickPort(ctx, used)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		used[num] = true

		spec := cfg.Spec
		if specNames[i] != "" {
			spec = specByName(cfg.Specs, specNames[i])
		}
		if _, err := p.cl.OpenPort(ctx, num, spec, eg); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("blanktrail: open port %d: %w", num, err)
		}

		pt := &poolPort{
			num:       num,
			ch:        ch,
			eg:        eg,
			specName:  specNames[i],
			spec:      spec,
			base:      newBaseTransport(host, num, cfg.CA, cfg.Insecure),
			renewedAt: cfg.Now(),
		}
		// lastUsed stays zero so a fresh port is immediately available.
		pt.client = &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: &ladder{rt: pt.base, port: num, rem: p},
		}
		p.mu.Lock()
		p.ports = append(p.ports, pt)
		p.byNum[num] = pt
		p.mu.Unlock()
	}
	return p, nil
}

// groupChannels collects one assignment slot list per channel, keeping both the
// channels and the slots within each channel in the order Assign produced them.
// Each group's length is that channel's share of the pool, so handing the slots
// out in order preserves the mixer's proportions exactly.
//
// Channels are grouped by name rather than by comparing the interface values:
// the mixer already keys a channel's weight by its name, and == on a caller's
// own Channel implementation panics if its dynamic type is not comparable.
func groupChannels(assigned []Channel) [][]Channel {
	var groups [][]Channel
	at := make(map[string]int, len(assigned))
	for _, ch := range assigned {
		if ch == nil {
			continue
		}
		i, ok := at[ch.Name()]
		if !ok {
			i = len(groups)
			at[ch.Name()] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], ch)
	}
	return groups
}

func (p *Pool) pickPort(ctx context.Context, used map[int]bool) (int, error) {
	if p.cfg.PortRange[1] > 0 {
		for n := p.cfg.PortRange[0]; n <= p.cfg.PortRange[1]; n++ {
			if !used[n] {
				return n, nil
			}
		}
		return 0, fmt.Errorf("blanktrail: port range %d-%d exhausted (need %d ports)",
			p.cfg.PortRange[0], p.cfg.PortRange[1], p.cfg.Size())
	}
	for i := 0; i < 3*p.cfg.Size()+12; i++ {
		n, err := p.cl.SuggestPort(ctx)
		if err != nil {
			return 0, err
		}
		if !used[n] {
			return n, nil
		}
	}
	return 0, errors.New("blanktrail: could not find enough free ports to open")
}

// Size reports how many ports the pool holds.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ports)
}

// Cooldown is the minimum gap between two requests on the same port.
func (p *Pool) Cooldown() time.Duration { return p.cool }

// NextDelay returns a random pause inside the configured delay range. Callers
// use it to pace their own requests; a perfectly even interval is itself a
// behavioural fingerprint.
func (p *Pool) NextDelay() time.Duration {
	spread := p.cfg.DelayMax - p.cfg.DelayMin
	if spread <= 0 {
		return p.cfg.DelayMin
	}
	return p.cfg.DelayMin + time.Duration(rand.Int63n(int64(spread)+1))
}

// Acquire leases the coldest ready port of any template, waiting until one is
// available or ctx is done. Always Release the lease.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	return p.acquire(ctx, "")
}

// AcquireSpec leases the coldest ready port opened under the named template.
// It returns ErrUnknownSpec when no port carries that name — a typo has to fail
// at once, because waiting for a port that can never arrive is indistinguishable
// from a slow run — and ErrPoolExhausted, naming the template, when every port
// of that template is quarantined, even if other templates are healthy. A closed
// pool answers ErrPoolExhausted as well: shutting down is not a misspelling.
func (p *Pool) AcquireSpec(ctx context.Context, name string) (*Lease, error) {
	if name == "" {
		return p.acquire(ctx, "")
	}
	if !p.hasSpec(name) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSpec, name)
	}
	return p.acquire(ctx, name)
}

func (p *Pool) acquire(ctx context.Context, specName string) (*Lease, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pt, wait, err := p.take(specName)
		if err != nil {
			return nil, err
		}
		if pt != nil {
			if err := p.renewIfDue(ctx, pt); err != nil {
				// The renewal left the port closed. renewFailed has marked it
				// broken — and quarantined it if it keeps failing — so give it
				// back and take another rather than hand out a lease on a port
				// that no longer exists on the proxy.
				p.giveBack(pt)
				continue
			}
			return &Lease{pt: pt, pool: p}, nil
		}
		if wait <= 0 {
			wait = 5 * time.Millisecond // every matching port is leased right now
		}
		if err := p.cfg.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// hasSpec reports whether any port in the pool carries this template name.
//
// A closed pool holds no ports at all, so every name would look like a typo. A
// worker shutting down has to hear "there is nothing left to lease", not "you
// misspelled a name that was valid a millisecond ago" — so a closed pool answers
// yes and lets take() give the caller ErrPoolExhausted, exactly as Acquire does.
func (p *Pool) hasSpec(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return true
	}
	for _, pt := range p.ports {
		if pt.specName == name {
			return true
		}
	}
	return false
}

// take returns the coldest available port carrying specName, or nil plus how
// long until the nearest one is ready. An empty specName matches every port.
func (p *Pool) take(specName string) (*poolPort, time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.cfg.Now()
	var best *poolPort
	var bestUsed time.Time
	soonest := time.Duration(-1)
	alive := 0

	for _, pt := range p.ports {
		if specName != "" && pt.specName != specName {
			continue
		}
		pt.mu.Lock()
		quarantined, leased, last := pt.quarantined, pt.leased, pt.lastUsed
		pt.mu.Unlock()
		if quarantined {
			continue
		}
		alive++
		if leased {
			continue
		}
		if elapsed := now.Sub(last); elapsed >= p.cool {
			if best == nil || last.Before(bestUsed) {
				best, bestUsed = pt, last
			}
			continue
		}
		remaining := p.cool - now.Sub(last)
		if soonest < 0 || remaining < soonest {
			soonest = remaining
		}
	}

	if alive == 0 {
		if specName != "" {
			// alive counted only ports of this template, so the bare sentinel
			// would describe a pool state that may not exist.
			return nil, 0, fmt.Errorf("%w: spec %q", ErrPoolExhausted, specName)
		}
		return nil, 0, ErrPoolExhausted
	}
	if best != nil {
		best.mu.Lock()
		best.leased = true
		best.mu.Unlock()
		return best, 0, nil
	}
	if soonest < 0 {
		soonest = 0
	}
	return nil, soonest, nil
}

// giveBack returns a port taken by take without counting a request against it,
// and starts its cooldown so a failing port is not retried in a tight loop.
func (p *Pool) giveBack(pt *poolPort) {
	pt.mu.Lock()
	pt.leased = false
	pt.lastUsed = p.cfg.Now()
	pt.mu.Unlock()
}

// Close closes every opened port and every channel. Safe to call more than once.
func (p *Pool) Close() error {
	var firstErr error
	p.closeOnce.Do(func() {
		p.mu.Lock()
		ports := append([]*poolPort(nil), p.ports...)
		p.ports = nil
		p.byNum = map[int]*poolPort{}
		p.closed = true
		p.mu.Unlock()

		for _, pt := range ports {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := p.cl.ClosePort(ctx, pt.num); err != nil && firstErr == nil {
				firstErr = err
			}
			cancel()
			pt.base.CloseIdleConnections()
		}
		p.mixer.Close()
	})
	return firstErr
}

// isClosed reports whether Close has run. Renewal checks it because it reopens a
// port through several calls, and the pool may be torn down in between.
func (p *Pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Lease is an exclusive hold on one port.
type Lease struct {
	pt       *poolPort
	pool     *Pool
	released bool
}

// Client returns the leased port's HTTP client.
func (l *Lease) Client() *http.Client { return l.pt.client }

// Do is shorthand for l.Client().Do(req).
func (l *Lease) Do(req *http.Request) (*http.Response, error) { return l.pt.client.Do(req) }

// Port is the port number backing this lease.
func (l *Lease) Port() int { return l.pt.num }

// SpecName is the named template the leased port was opened under, or the empty
// string when the pool has a single unnamed template.
func (l *Lease) SpecName() string { return l.pt.specName }

// Egress is where this port currently sends its traffic.
func (l *Lease) Egress() Egress { return l.pt.egress() }

// Session identifies the port's current identity — egress IP, fingerprint and
// cookie jar together. It changes whenever any of them changes, because the
// proxy discards a solved challenge on exactly those events: clearance cookies
// bind to IP, UA and TLS at once.
//
// Bind per-session state to this value. A target that expects a stable device or
// visitor id must mint a new one when Session changes, or its traffic looks like
// one visitor whose device changed underneath it.
func (l *Lease) Session() string {
	l.pt.mu.Lock()
	defer l.pt.mu.Unlock()
	return strconv.Itoa(l.pt.num) + "#" + strconv.FormatUint(l.pt.session, 10)
}

// Release returns the port to the pool and starts its cooldown. Safe to call
// more than once.
func (l *Lease) Release() {
	if l.released {
		return
	}
	l.released = true
	l.pool.giveBack(l.pt)

	l.pt.mu.Lock()
	l.pt.requests++
	l.pt.mu.Unlock()

	l.pool.mu.Lock()
	l.pool.stats.Requests++
	l.pool.mu.Unlock()
}

// --- remedy implementation (used by the ladder) ---

func (p *Pool) port(num int) *poolPort {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byNum[num]
}

func (p *Pool) rotateProfile(ctx context.Context, num int) error {
	if _, err := p.cl.RotateProfile(ctx, num); err != nil {
		return err
	}
	p.mu.Lock()
	p.stats.ProfileRotations++
	p.mu.Unlock()
	return nil
}

func (p *Pool) rotateEgress(ctx context.Context, num int) error {
	pt := p.port(num)
	if pt == nil {
		return fmt.Errorf("blanktrail: port %d is not in the pool", num)
	}
	cur := pt.egress()
	next, err := pt.ch.Renew(ctx, cur)
	if errors.Is(err, ErrRenewUnsupported) {
		// This channel has one fixed IP; there is nothing to rotate to. Say so
		// rather than looping, and let the caller's retry budget run out.
		return err
	}
	if err != nil {
		return err
	}
	pt.setEgress(next)
	if next.Gateway != "" {
		// A gateway hop is chosen at open time and cannot be swapped live.
		return nil
	}
	if err := p.cl.SetUpstream(ctx, num, next.Upstream); err != nil {
		return err
	}
	pt.mu.Lock()
	pt.session++
	pt.mu.Unlock()
	p.mu.Lock()
	p.stats.EgressRotations++
	p.mu.Unlock()
	return nil
}

// markBadEgress reports that an egress failed at the connection level, so the
// channel can stop handing that address out.
//
// It deliberately does not penalise the channel: this runs on every attempt, and
// a single request against a dead proxy would spend the channel's whole weight
// in one go. Channel weight is lowered where a port is actually given up — see
// exhausted and renewFailed.
func (p *Pool) markBadEgress(num int) {
	pt := p.port(num)
	if pt == nil {
		return
	}
	pt.ch.MarkBad(pt.egress())
}

// attemptFailed records a failed attempt on a port and reports whether the
// egress has failed often enough in a row to be replaced. The count is
// consecutive: a single success clears it, so an occasional 404 in a healthy run
// never adds up to a rotation.
func (p *Pool) attemptFailed(num int) bool {
	pt := p.port(num)
	if pt == nil {
		return false
	}
	pt.mu.Lock()
	pt.failures++
	rotate := pt.failures >= p.cfg.RotateAfterFailures
	if rotate {
		pt.failures = 0
	}
	pt.mu.Unlock()
	return rotate
}

// attemptFailedStatus records a failed attempt that produced a response. A
// status the consumer excludes still resets nothing and still counts as "not a
// success" — it simply does not push the port towards a new egress.
func (p *Pool) attemptFailedStatus(num, status int) bool {
	if p.cfg.CountFailure != nil && !p.cfg.CountFailure(status) {
		return false
	}
	return p.attemptFailed(num)
}

// attemptSucceeded clears a port's consecutive-failure count.
func (p *Pool) attemptSucceeded(num int) {
	pt := p.port(num)
	if pt == nil {
		return
	}
	pt.mu.Lock()
	pt.failures = 0
	pt.mu.Unlock()
}

// renewIfDue replaces a port's whole identity when a proactive trigger has
// fired, or repairs a port that a previous renewal left closed.
//
// Renewal means reopening the port: a fresh fingerprint and a fresh egress IP
// can be set on a live port, but the cookie jar cannot be cleared through the
// control API, and a jar carried across an IP change is exactly the
// inconsistency an origin looks for.
func (p *Pool) renewIfDue(ctx context.Context, pt *poolPort) error {
	now := p.cfg.Now()

	pt.mu.Lock()
	broken := pt.broken
	byCount := p.cfg.RenewAfterRequests > 0 && pt.requests >= p.cfg.RenewAfterRequests
	byTime := p.cfg.RenewAfterInterval > 0 && now.Sub(pt.renewedAt) >= p.cfg.RenewAfterInterval
	pt.mu.Unlock()

	// A broken port was closed by an earlier renewal that could not finish. It
	// has to be repaired before it can serve anything, whatever the triggers say.
	if !broken && !byCount && !byTime {
		return nil
	}

	eg := pt.egress()
	if next, err := pt.ch.Renew(ctx, eg); err == nil {
		eg = next
	} else if !errors.Is(err, ErrRenewUnsupported) {
		return p.renewFailed(pt, fmt.Errorf("blanktrail: renew port %d: egress: %w", pt.num, err))
	}

	// Past this point the port is torn down, so every failure leaves it closed.
	if err := p.cl.ClosePort(ctx, pt.num); err != nil {
		return p.renewFailed(pt, fmt.Errorf("blanktrail: renew port %d: close: %w", pt.num, err))
	}
	if _, err := p.cl.OpenPort(ctx, pt.num, pt.spec, eg); err != nil {
		return p.renewFailed(pt, fmt.Errorf("blanktrail: renew port %d: reopen: %w", pt.num, err))
	}

	// The pool may have been closed while this renewal was in flight. The port is
	// open again but nothing owns it any more, so close it here — otherwise it
	// outlives the program and no later run may reclaim it: taking over a port
	// this program did not open is exactly what the pool refuses to do.
	if p.isClosed() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.cl.ClosePort(ctx, pt.num)
		cancel()
		return fmt.Errorf("blanktrail: renew port %d: pool closed during renewal", pt.num)
	}

	// Rotating after reopening guarantees a different fingerprint even if the
	// proxy handed back the same one on open.
	prof, err := p.cl.RotateProfile(ctx, pt.num)
	if err != nil {
		return p.renewFailed(pt, fmt.Errorf("blanktrail: renew port %d: rotate: %w", pt.num, err))
	}
	// Whether the control API's rotation stays inside the browser/os filters the
	// port was opened with is an assumption nobody has measured. If it does not,
	// a mobile port comes back as whatever the profile database offered while
	// the pool goes on calling it mobile — and a port that is quarantined is
	// recoverable, where a mislabelled measurement is not.
	if err := profileMatchesSpec(pt.spec, prof); err != nil {
		return p.renewFailed(pt, fmt.Errorf("blanktrail: renew port %d: rotate: %w", pt.num, err))
	}

	pt.base.CloseIdleConnections()
	pt.mu.Lock()
	pt.eg = eg
	pt.requests = 0
	pt.failures = 0
	pt.strikes = 0
	pt.broken = false
	pt.renewedAt = now
	pt.session++
	pt.mu.Unlock()

	p.mu.Lock()
	p.stats.Renewals++
	p.stats.ProfileRotations++
	p.mu.Unlock()
	return nil
}

// profileMatchesSpec reports whether a fingerprint the proxy handed back still
// answers the filters the port was opened with.
//
// A field the spec left open is not checked, and neither is a field the proxy
// did not report: the point is to catch a profile that contradicts the template,
// not to quarantine a healthy port because a response omitted a key. Comparison
// is case-insensitive — which spelling the API returns is not something to
// quarantine a port over either.
func profileMatchesSpec(spec PortSpec, prof Profile) error {
	if spec.Browser != "" && prof.Browser != "" && !strings.EqualFold(spec.Browser, prof.Browser) {
		return fmt.Errorf("profile is browser %q but the port was opened for %q", prof.Browser, spec.Browser)
	}
	if spec.OS != "" && prof.OS != "" && !strings.EqualFold(spec.OS, prof.OS) {
		return fmt.Errorf("profile is OS %q but the port was opened for %q", prof.OS, spec.OS)
	}
	return nil
}

// renewFailed marks a port broken after a failed renewal and quarantines it once
// it has failed too often. Strikes are shared with exhausted() on purpose: both
// mean the same thing — this port keeps failing us — and a successful renewal
// clears the slate.
func (p *Pool) renewFailed(pt *poolPort, err error) error {
	pt.mu.Lock()
	pt.broken = true
	pt.strikes++
	quarantine := pt.strikes >= p.cfg.MaxPortStrikes && !pt.quarantined
	if quarantine {
		pt.quarantined = true
	}
	pt.mu.Unlock()

	if quarantine {
		p.mu.Lock()
		p.stats.Quarantines++
		p.mu.Unlock()
		p.mixer.Penalise(pt.ch)
	}
	return err
}

// exhausted records that a port spent its whole retry budget and still came back
// blocked. Enough strikes and the port is quarantined: continuing to hand it out
// only burns proxies and time.
func (p *Pool) exhausted(num int) {
	pt := p.port(num)
	if pt == nil {
		return
	}
	pt.mu.Lock()
	pt.strikes++
	quarantine := pt.strikes >= p.cfg.MaxPortStrikes && !pt.quarantined
	if quarantine {
		pt.quarantined = true
	}
	pt.mu.Unlock()

	if quarantine {
		p.mu.Lock()
		p.stats.Quarantines++
		p.mu.Unlock()
		p.mixer.Penalise(pt.ch)
	}
}

func (p *Pool) maxRetries() int { return p.cfg.MaxRetriesPerReq }

func (p *Pool) wait(ctx context.Context, d time.Duration) error { return p.cfg.Sleep(ctx, d) }

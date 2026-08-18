// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"slices"
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

// DefaultCooldown is the gap NewPool keeps between two requests on one port when
// the caller names neither Cooldown nor a delay range.
//
// It rests on a sweep of 240 answers taken on held ports at gaps of 0, 2, 5, 15,
// 30 and 60 seconds, forty answers at each gap. Three of the 240 were slow: one
// at no gap at all, two at thirty seconds, none anywhere else. Scattered across
// gaps two orders of magnitude apart, they are not explained by the gap, and
// forty back-to-back requests with no wait between them gave thirty-nine fast.
//
// So this is a small deliberate pause and not a derived quantity. What the sweep
// establishes is that the gap does not drive slow answers over forty requests to
// a port, which is not the same as establishing that it never could over ten
// thousand; and the whole margin of two seconds over none rests on one answer in
// forty. A caller who has measured their own list should set Cooldown from what
// they measured rather than inherit this.
const DefaultCooldown = 2 * time.Second

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

	// NoKeepAlives opens a fresh connection for every request instead of keeping
	// one alive between them.
	//
	// Reuse is worth a handshake and nothing more, and it is not always worth
	// even that: a request handed a pooled tunnel has been measured waiting tens
	// of seconds — minutes, at worst — for a first byte that a request on its own
	// connection got in four, with the whole of that spent after it had been
	// sent. What costs it is the proxy's, not this program's, so this is a switch
	// rather than a rule.
	NoKeepAlives bool

	// Trace, when set, is told how every request through every port went: where
	// it was when it stopped waiting, and what it got. It is off unless somebody
	// asks, because the timing hooks cost a little on each request — and because
	// a line per request is a great deal of reading for a program that is
	// working.
	Trace func(RequestTrace)

	// CA is the proxy's MITM CA. Insecure skips verification instead — only for
	// tests, never in production.
	CA       *x509.CertPool
	Insecure bool

	// DelayMin and DelayMax bound the pause a caller should leave between its
	// own requests. The pool does not enforce them; it uses them to derive the
	// cooldown and offers NextDelay so callers have one place to ask.
	DelayMin, DelayMax time.Duration
	// Cooldown is the minimum gap between two requests on the SAME port. Zero
	// derives it from PortsPerThread and the delay range, or takes
	// DefaultCooldown when no delay range was given either.
	Cooldown time.Duration

	// RequestTimeout bounds one request through a leased port, retries included
	// (default 300s). Keep it comfortably above MaxRetriesPerReq × maxRetryAfter,
	// or a throttled target will exhaust the deadline in pauses before a retry
	// can run.
	RequestTimeout time.Duration
	// MaxRetriesPerReq is how many times the ladder retries a blocked request
	// before handing the blocked response back (default 4).
	MaxRetriesPerReq int
	// AddressesPerRequest is how many addresses one request may be carried to
	// when they fail to carry it at all (default 15).
	//
	// It is a separate budget from the retries above, because the two are spent
	// on different things: a refused answer is waited out, and a dead address is
	// walked away from. On a list where most addresses are dead — which is what
	// a large cheap list is — walking away is nearly free and finding a live one
	// is the whole job, so this is the larger of the two and takes no pause
	// between tries.
	AddressesPerRequest int
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
	// ReviveAfter is how long a quarantined port waits before it is offered
	// another egress and put back into rotation. Zero derives two minutes; a
	// negative value leaves a quarantined port quarantined for the life of the
	// pool.
	//
	// A port is quarantined for the answers it gave, and those answers came
	// through an egress. On a list where a large share of the addresses are dead
	// before the run starts, a port that drew three bad ones in a row is unlucky
	// rather than broken, and a pool that cannot take it back stops a job in
	// which nothing is wrong.
	ReviveAfter time.Duration
	// MaxRevivals is how many times one port may be offered another egress
	// (default 3). Past it the port stays quarantined: taking a port that is
	// genuinely finished back for ever costs a rotation on every acquisition and
	// buys nothing.
	MaxRevivals int

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

	// hot marks a port of the standing set: one this machine keeps open and
	// warm between jobs. A job that needs more than there are grows the pool and
	// gives the growth back when it ends, and this is what says which ports the
	// growth was — the standing ones are never the ones given back.
	//
	// It used to decide which port a lease is offered first as well, and that
	// was wrong: being kept is not being warm. A standing port that has never
	// made a request meets the same challenge a fresh one does — measured at one
	// to three minutes against one to two seconds — so what a lease is offered
	// first is decided by answered below.
	hot bool

	mu sync.Mutex
	// answered says this port has brought back an answer somebody accepted since
	// its identity was last changed. It is what "warm" actually means: the first
	// request on an identity meets a challenge and costs minutes, and every
	// request after it on the same identity costs seconds. It is the pool's own
	// word for it, said by the caller through Lease.Answered, because only the
	// caller knows whether what came back was a page or a refusal.
	//
	// It is cleared wherever the identity changes, because the challenge is
	// solved against the identity and not against the port number.
	answered    bool
	eg          Egress
	lastUsed    time.Time
	leased      bool
	quarantined bool
	broken      bool // renewal closed it but could not reopen it
	failures    int  // consecutive failed attempts; any success clears it
	requests    int
	strikes     int
	// quarantinedAt is when the quarantine began, and is zero while the port is
	// not quarantined. revivals counts how many times the port has been offered
	// another egress to come back on.
	quarantinedAt time.Time
	revivals      int
	renewedAt     time.Time
	session       uint64 // bumped whenever the port's identity changes
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

	// Rejections counts answers a caller handed back as unusable even though the
	// request carrying them succeeded. A run whose rejections climb while its
	// requests do not is being refused, not failing.
	Rejections int64

	// Revivals counts quarantined ports given another egress and put back into
	// rotation.
	Revivals int64

	// Warm counts the ports whose current identity has brought back an answer
	// somebody accepted. It is what a machine keeping identities open has to
	// report: twelve kept and two warm is a set that is still worth almost
	// nothing, and a screen saying only "twelve" cannot tell anybody that.
	Warm int
}

// Stats returns a snapshot of pool activity.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stats
	st.Ports = len(p.ports)
	st.Quarantined = 0

	var bySpec map[string]SpecStats
	st.Warm = 0
	for _, pt := range p.ports {
		pt.mu.Lock()
		quarantined, name, answered := pt.quarantined, pt.specName, pt.answered
		pt.mu.Unlock()
		if quarantined {
			st.Quarantined++
		}
		if answered {
			st.Warm++
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

// NewPool opens Threads × PortsPerThread ports and returns a ready pool. A port
// number that turns out to be taken costs another number, not the pool — see
// openOne. On any failure it closes every port it had already opened.
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
	if cfg.AddressesPerRequest <= 0 {
		cfg.AddressesPerRequest = 15
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
	// The delay range is filled in below for NextDelay's sake, so whether the
	// caller described one has to be read before that happens: a filled-in range
	// would otherwise derive a cooldown from a pause nobody asked for.
	pacedByCaller := cfg.DelayMin > 0 || cfg.DelayMax > 0
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
	if cfg.ReviveAfter == 0 {
		cfg.ReviveAfter = 2 * time.Minute
	}
	if cfg.MaxRevivals <= 0 {
		cfg.MaxRevivals = 3
	}

	cool := cfg.Cooldown
	switch {
	case cool > 0:
	case pacedByCaller:
		cool = DeriveCooldown(cfg.PortsPerThread, cfg.DelayMin, cfg.DelayMax)
	default:
		cool = DefaultCooldown
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

	if err := p.openBatch(ctx, cfg.Size(), false); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

// openBatch opens count more ports and lays them over the channels and the
// templates the pool was configured with.
//
// It is one function because a pool is built and grown the same way: the
// alternative is two layouts that agree today and one of them changed later,
// after which a grown pool would be spread over its egress channels differently
// from a pool of the same size opened whole — and no two runs would be
// comparable again.
//
// hot says whether these are the standing ports. The ones a job grows the pool
// by are not, and they are what Shrink gives back.
func (p *Pool) openBatch(ctx context.Context, count int, hot bool) error {
	if count <= 0 {
		return nil
	}
	specNames, err := planSpecs(p.cfg.Specs, count)
	if err != nil {
		return err
	}
	// The mixer decides how many ports each channel gets; spreadSpecs decides
	// which ports those are, so that no template ends up confined to one egress.
	groups := groupChannels(p.mixer.Assign(count))
	counts := make([]int, len(groups))
	total := 0
	for i, g := range groups {
		counts[i] = len(g)
		total += len(g)
	}
	if total != count {
		// Assign hands back nothing once every channel has been penalised to a
		// weight of zero.
		return errors.New("blanktrail: no usable egress channel")
	}
	layout := spreadSpecs(specNames, counts)
	taken := make([]int, len(groups))

	// The numbers already in this pool are spent. Without them a growth would be
	// handed a number it is already listening on, and the open would be refused
	// for a reason that reads as the range being full.
	used := map[int]bool{}
	p.mu.Lock()
	for num := range p.byNum {
		used[num] = true
	}
	host := p.host
	p.mu.Unlock()

	for i := 0; i < count; i++ {
		g := layout[i]
		ch := groups[g][taken[g]]
		taken[g]++
		eg, ok := ch.Next()
		if !ok {
			return fmt.Errorf("blanktrail: channel %q has no egress to hand out", ch.Name())
		}
		spec := p.cfg.Spec
		if specNames[i] != "" {
			spec = specByName(p.cfg.Specs, specNames[i])
		}
		num, err := p.openOne(ctx, used, spec, eg)
		if err != nil {
			return err
		}

		pt := &poolPort{
			num:       num,
			ch:        ch,
			eg:        eg,
			specName:  specNames[i],
			spec:      spec,
			hot:       hot,
			base:      newBaseTransport(host, num, p.cfg.CA, p.cfg.Insecure, p.cfg.NoKeepAlives),
			renewedAt: p.cfg.Now(),
		}
		// lastUsed stays zero so a fresh port is immediately available.
		pt.client = &http.Client{
			Timeout:   p.cfg.RequestTimeout,
			Transport: &ladder{rt: pt.base, port: num, rem: p, trace: p.cfg.Trace},
		}
		p.mu.Lock()
		p.ports = append(p.ports, pt)
		p.byNum[num] = pt
		p.mu.Unlock()
	}
	return nil
}

// Grow opens extra more ports on top of the ones already open.
//
// It is how a job larger than the standing pool is run: the ports this machine
// keeps warm stay as they are, the job's own are opened beside them, and one
// pool hands out both. Opened as a second pool they would be a second set of
// statistics, a second cooldown and a second answer to "how many identities is
// this run on".
//
// A growth that fails part way leaves what it managed open. They are ports of
// this pool like any other and Shrink gives them back; the alternative is
// unwinding a partial growth while a job is already leasing from it.
func (p *Pool) Grow(ctx context.Context, extra int) error {
	if p.isClosed() {
		return ErrPoolExhausted
	}
	return p.openBatch(ctx, extra, false)
}

// Shrink closes every port this pool has grown by, and keeps the standing ones.
//
// It is what a job's end does. A port still leased is closed with the rest: the
// caller has let go of the pool by the time this is called, and a port left open
// because somebody forgot to release it is a port nothing will ever close.
func (p *Pool) Shrink(ctx context.Context) (int, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, nil
	}
	var keep, drop []*poolPort
	for _, pt := range p.ports {
		if pt.hot {
			keep = append(keep, pt)
			continue
		}
		drop = append(drop, pt)
		delete(p.byNum, pt.num)
	}
	p.ports = keep
	p.mu.Unlock()

	var firstErr error
	for _, pt := range drop {
		if err := p.cl.ClosePort(ctx, pt.num); err != nil && firstErr == nil {
			firstErr = err
		}
		pt.base.CloseIdleConnections()
	}
	return len(drop), firstErr
}

// AcquireIdleHot leases a standing port that nobody has used for at least the
// given span, and reports whether there was one.
//
// It is what keeps the standing ports warm without getting in the way of the
// work: a port the run is using does not need warming, and one the run has just
// used is not idle. Nothing waits here — a pool with nothing idle answers no
// straight away, and the caller comes back later.
func (p *Pool) AcquireIdleHot(idle time.Duration) (*Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false
	}
	now := p.cfg.Now()
	// Never answered first, and only then the ones that have gone quiet. A set
	// just opened is entirely of the first kind and worth nothing to anybody
	// until that changes, while a port that answered ten minutes ago is warm
	// already and warming it again buys nothing this round.
	for _, cold := range []bool{true, false} {
		for _, pt := range p.ports {
			if !pt.hot {
				continue
			}
			pt.mu.Lock()
			free := !pt.leased && !pt.quarantined &&
				pt.answered != cold && now.Sub(pt.lastUsed) >= idle
			if free {
				pt.leased = true
				pt.lastUsed = now
			}
			pt.mu.Unlock()
			if free {
				return &Lease{pool: p, pt: pt}, true
			}
		}
	}
	return nil, false
}

// KeepWarm declares every port this pool now holds to be a standing one, and
// says how many that is.
//
// It is said rather than assumed, because a pool opened for one job and a pool
// a machine keeps open all day are the same object and only the caller knows
// which it is building. Assumed, the mark would be on every pool ever opened,
// and a job's own identities would be shrunk instead of closed — held open for
// nothing, for as long as the program runs.
//
// Ports opened afterwards are the growth, and Shrink is what gives them back.
func (p *Pool) KeepWarm() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pt := range p.ports {
		pt.hot = true
	}
	return len(p.ports)
}

// ReduceTo closes ports until the pool holds no more than want of them, and
// says how many went.
//
// It is what lowering the number of identities a machine keeps warm does. The
// ones opened last go first: the ones opened earliest have been warm longest,
// and warmth is the whole reason any of them are open.
//
// A port somebody is holding is left alone and counted against the total, so a
// number lowered while a job runs takes effect as the job lets go rather than
// by closing a port out from under it.
func (p *Pool) ReduceTo(ctx context.Context, want int) (int, error) {
	if want < 0 {
		want = 0
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, nil
	}
	var keep, drop []*poolPort
	// Walked from the newest backwards, so what is dropped is what was opened
	// last. Held ports are kept whatever the count says.
	for i := len(p.ports) - 1; i >= 0; i-- {
		pt := p.ports[i]
		pt.mu.Lock()
		leased := pt.leased
		pt.mu.Unlock()
		if leased || len(p.ports)-len(drop) <= want {
			keep = append(keep, pt)
			continue
		}
		drop = append(drop, pt)
		delete(p.byNum, pt.num)
	}
	// Put back the way round they were opened, so "the newest" goes on meaning
	// the same thing next time.
	slices.Reverse(keep)
	p.ports = keep
	p.mu.Unlock()

	var firstErr error
	for _, pt := range drop {
		if err := p.cl.ClosePort(ctx, pt.num); err != nil && firstErr == nil {
			firstErr = err
		}
		pt.base.CloseIdleConnections()
	}
	return len(drop), firstErr
}

// Hot is how many ports of the standing set this pool holds.
func (p *Pool) Hot() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, pt := range p.ports {
		if pt.hot {
			n++
		}
	}
	return n
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

// openOne opens one port under spec on eg and returns the number it settled on.
// Every number it worked through, opened or not, is recorded in used.
//
// A number the proxy just suggested can still be held by something else. The
// proxy knows about the ports it holds itself and nothing else on the machine,
// and a number this program has only just closed is kept by the operating system
// for a while afterwards. With a pool that lives for one job — opened and closed
// all day — that stops being rare, so a taken number costs another number rather
// than the whole pool.
//
// Whether a number is free is asked of THIS MACHINE, before the proxy is asked
// to stand on it — see numberFree. Asking the proxy is useless, and that is
// measured rather than assumed: against the live service on 127.0.0.1:8891, a
// number an ordinary listener on this machine already held was answered with
// 200 and "opened", was listed among the open ports afterwards, and then never
// answered a single request through it — twelve seconds and nothing. Both the
// answer and the list say the same thing about a port that works and a port that
// cannot exist, so neither can be believed. A listener can.
//
// The check races and nothing can stop it racing: between this program closing
// its listener and the proxy binding the number, some other program on the
// machine can take it. The window is the width of one control-API call, against
// a port that then lives for a whole job, so narrowing it is worth what it
// costs — but narrowing is all it is, and that is precisely why the check does
// not replace the handling of a refusal below. The two cover different halves:
// the check catches what this machine can see is taken, the refusal catches what
// it cannot, including whatever slips into that window.
//
// A number that was refused is left exactly as it was found: whatever holds it,
// this program did not open it, and reaching for a port it does not own is the
// one thing the pool never does.
func (p *Pool) openOne(ctx context.Context, used map[int]bool, spec PortSpec, eg Egress) (int, error) {
	var lastTaken error
	tries := p.portAttempts()
	for i := 0; i < tries; i++ {
		num, err := p.pickPort(ctx, used)
		if err != nil {
			if lastTaken != nil {
				// Without this the caller reads "the range is exhausted" as a
				// range too small for the pool, when in truth its numbers were
				// held by something the proxy cannot see.
				return 0, fmt.Errorf("%w; a number tried before that was already taken: %w", err, lastTaken)
			}
			return 0, err
		}
		// The number is spent whether or not it opens: pickPort avoids only what
		// used holds, so a refused number left unmarked would be handed back on
		// the next turn and refused again for as long as the attempts last. That
		// holds for a number this machine refuses just as it holds for one the
		// proxy refuses — the number is no freer the second time it is looked at.
		used[num] = true

		if err := p.numberFree(num); err != nil {
			lastTaken = err
			continue
		}

		if _, err := p.cl.OpenPort(ctx, num, spec, eg); err != nil {
			if !numberTaken(err) {
				return 0, fmt.Errorf("blanktrail: open port %d: %w", num, err)
			}
			lastTaken = err
			continue
		}
		return num, nil
	}
	return 0, fmt.Errorf("blanktrail: %d port numbers in a row were already taken, the last with: %w",
		tries, lastTaken)
}

// numberFree asks this machine whether num is still free on the host the ports
// live on, and returns nil when it is. A non-nil error means the number is held
// by something and the pool should take a different one.
//
// The question is asked the only way it can be answered honestly: by opening a
// listener on that exact address and closing it again immediately. Immediately,
// and not on a defer at the end of the open: the number has to be free again
// before the proxy is asked to bind it, or the only program standing in the
// proxy's way would be this one.
//
// A bind can also fail for reasons that say nothing about the number — the host
// may not be an address of this machine at all, which is the case whenever the
// proxy runs somewhere else. Telling that apart needs no error codes: ask the
// same host for any port at all. If even that is refused, the failure was about
// the host, this check has no opinion, and the number goes to the proxy to be
// judged there. Reading such a failure as "taken" would work through every
// number in the range and open no ports whatsoever.
//
// Binding is also what keeps the answer honest from one system to the next. The
// check asks the question the proxy's own bind will ask, so whatever the rules
// are about a number just released — Windows refuses it for a while, Linux with
// SO_REUSEADDR does not — the check inherits them rather than guessing at them.
func (p *Pool) numberFree(num int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(p.host, strconv.Itoa(num)))
	if err == nil {
		_ = ln.Close()
		return nil
	}
	// Any port at all on the same host: this separates a number that is held
	// from a host this machine cannot bind on in the first place.
	probe, probeErr := net.Listen("tcp", net.JoinHostPort(p.host, "0"))
	if probeErr != nil {
		return nil
	}
	_ = probe.Close()
	return fmt.Errorf("blanktrail: port %d is already held on %s by something outside the proxy: %w", num, p.host, err)
}

// numberTaken reports whether an OpenPort failure was about the port NUMBER —
// something already holds it — and could therefore succeed on another one.
//
// This is the one judgement in the retry, and it cannot be made with certainty.
// 409 Conflict is the status HTTP has for "what you named is already taken" and
// what the fake control API answers, but nothing rules out a proxy reporting the
// bind failure as a 500, or a 400 with the reason in the body.
//
// What IS measured is that the live service answers none of those. Asked to open
// a number this machine already held, it answered 200 and "opened" and listed
// the port afterwards; requests through that port then hung until the deadline.
// So against that service this test never fires, and the reason a number is
// stepped over is numberFree above, not anything read out of an answer. It is
// kept because it costs nothing, because it is right for a service that does
// report the conflict, and because the window numberFree cannot close is exactly
// the one a conflict would be reported in.
//
// So the list is deliberately short and errs towards NOT trying another number.
// Both mistakes are possible and they are not equal:
//
//   - Reading a taken number as something else gives up on it — which is what
//     this program did before any of this existed: one clear sentence, at once.
//   - Reading "the key was rejected" or "no licence for a pool" as a taken
//     number retries an answer that will be identical on every number, and turns
//     a sentence the operator can act on into a long silence and then a vaguer
//     one.
//
// The second is the worse trade, so anything not on this list ends the open and
// carries its own error out. If some other proxy is ever seen answering a busy
// number with a status of its own, this is the place to add it — but do not add
// the one measured here, because what it answers is 200.
//
// A failure that is not an *APIError never reached the proxy at all — a refused
// connection, a spent deadline — and no other number would fare better.
func numberTaken(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusConflict
}

// portAttempts is how many port numbers one open may work through. It is also
// the ceiling pickPort uses on the proxy's suggestions, kept in one place so the
// search for a number and the giving up on it cannot drift apart.
func (p *Pool) portAttempts() int { return 3*p.cfg.Size() + 12 }

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
	for i := 0; i < p.portAttempts(); i++ {
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

// Sleep pauses for d and returns early if ctx ends first.
//
// The pause between a caller's own requests is the caller's to take: the pool
// derives the delay but cannot know when the caller is about to ask again. A
// caller left to its own timer would keep a clock the rest of the pool does not
// run on, and a test that wound the pool forward would then sit through every
// pause for real. A non-positive d reports only whether ctx has ended.
func (p *Pool) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	return p.cfg.Sleep(ctx, d)
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
			if err := p.reviveIfDue(ctx, pt); err != nil {
				// The port could not be given another egress, so it stays
				// quarantined. Give it back and take another.
				p.giveBack(pt)
				continue
			}
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
	var bestWarm bool
	soonest := time.Duration(-1)
	alive := 0

	for _, pt := range p.ports {
		if specName != "" && pt.specName != specName {
			continue
		}
		pt.mu.Lock()
		quarantined, leased, last := pt.quarantined, pt.leased, pt.lastUsed
		revivals, since := pt.revivals, pt.quarantinedAt
		answered := pt.answered
		pt.mu.Unlock()
		// A port that has waited out its quarantine is a candidate again. take
		// holds p.mu and must not call the control API, so it only decides that
		// the port is due; acquire does the rotation that brings it back.
		if quarantined && !p.revivableAt(revivals, since, now) {
			continue
		}
		alive++
		if leased {
			continue
		}
		if elapsed := now.Sub(last); elapsed >= p.cool {
			// Warm before cold, and within each the one that has rested longest.
			// A warm port answers in seconds where a cold one waits minutes on a
			// challenge; the cooldown above keeps this from becoming "always the
			// same ten". Warm is "has answered", not "is kept": a standing port
			// that has never made a request is as cold as a fresh one, and
			// offering it first was the whole of what a job felt when it started
			// on a set that was still warming up.
			switch {
			case best == nil,
				answered && !bestWarm,
				answered == bestWarm && last.Before(bestUsed):
				best, bestUsed, bestWarm = pt, last, answered
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

// revivableAt reports whether a port quarantined at since, and offered another
// egress revivals times already, has waited long enough to be offered one more.
// It takes values rather than the port, and no lock of its own, so a caller can
// read the port's state once and decide from it.
func (p *Pool) revivableAt(revivals int, since, now time.Time) bool {
	if p.cfg.ReviveAfter < 0 || revivals >= p.cfg.MaxRevivals {
		return false
	}
	// A quarantine with no beginning has no wait to measure, and reading the zero
	// time as "long ago" would bring such a port back on the very next acquire.
	return !since.IsZero() && now.Sub(since) >= p.cfg.ReviveAfter
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

// Answered says this port's identity brought back an answer that was accepted.
//
// It is what makes the port warm, and it is said by the caller because only the
// caller can tell a page from a refusal: the request that carries a challenge
// back succeeds at every level this package can see. What it buys is measured —
// the first request on an identity costs one to three minutes and every one
// after it costs one to two seconds — so a pool that could not tell the two
// apart would offer a job the identity that has never answered as readily as
// the one that has.
//
// Safe to call more than once, and safe to call before Release.
func (l *Lease) Answered() {
	l.pt.mu.Lock()
	l.pt.answered = true
	l.pt.mu.Unlock()
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

// Reject tells the pool that this port produced an answer the caller cannot
// use, even though the request carrying it succeeded.
//
// The pool cannot see that on its own and must not try to: it does not know the
// target and has no business reasoning about what a good answer looks like. But
// a refusal arriving with a 2xx status is still a refusal, and left unsaid it
// would clear the port's consecutive-failure count and hand the same identity
// out again. So a rejection counts exactly as a failed attempt does: enough of
// them in a row and the egress is replaced, enough of those and the port is
// quarantined. The address is blamed on every rejection, because the request
// went through — whatever was refused, it was not the request.
//
// A returned lease is not rejected: by then the port may belong to another
// thread, whose work must not spend a strike.
//
// The lease stays held. A caller that wants a different port releases this one
// and acquires another.
func (l *Lease) Reject(ctx context.Context) error {
	if l.released {
		return nil
	}
	num := l.pt.num
	p := l.pool

	p.mu.Lock()
	p.stats.Rejections++
	p.mu.Unlock()

	p.markBadEgress(num)
	if !p.attemptFailed(num) {
		return nil
	}
	if err := p.rotateEgress(ctx, num); err != nil {
		// There is nowhere to move to, or moving failed. Either way the port has
		// spent its whole budget on this answer, which is what a strike records.
		p.exhausted(num)
		if errors.Is(err, ErrRenewUnsupported) {
			return nil
		}
		return err
	}
	return nil
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

// RotateEgressFor moves one port to another address, as a failed request makes
// the pool do on its own.
//
// It is exported so that a measurement can tell two things apart that look the
// same from outside: a port that is spent, and an address that is dead. The
// pool's own rotation is bound up with counting failures, and a test of "what
// happens after a rotation" has to be able to ask for one.
func (p *Pool) RotateEgressFor(ctx context.Context, num int) error {
	return p.rotateEgress(ctx, num)
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
	// Every connection this transport is holding open was a tunnel through the
	// address just abandoned, and changing the upstream does not change where an
	// already-open tunnel goes. Handed back on the next request, one of them
	// sends the work down the very route this rotation exists to leave — and
	// what comes back is a torn-down connection, which counts as another failure,
	// rotates again, and finds another stale tunnel. Dropping them is what makes
	// a rotation mean anything.
	pt.base.CloseIdleConnections()
	pt.mu.Lock()
	pt.session++
	pt.answered = false
	pt.mu.Unlock()
	p.mu.Lock()
	p.stats.EgressRotations++
	p.mu.Unlock()
	return nil
}

// markDeadEgress reports that an egress did not carry the request at all, so
// the channel can stop handing that address out now rather than after counting.
func (p *Pool) markDeadEgress(num int) {
	pt := p.port(num)
	if pt == nil {
		return
	}
	pt.ch.MarkDead(pt.egress())
}

// hunt is how many addresses one request may be carried to.
func (p *Pool) hunt() int { return p.cfg.AddressesPerRequest }

// markBadEgress reports that an egress carried its work and the answer was
// refused, so the channel can count that against the address.
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

// reviveIfDue takes a quarantined port back once it has waited long enough, and
// leaves every other port alone.
//
// Coming back starts with another egress and cannot be anything less. The port
// was quarantined for the answers it gave, and those answers came through the
// egress it still holds, so clearing the flag alone would put it straight back
// into the state it was quarantined for. A rotation that fails leaves the port
// quarantined — there is nothing to bring it back to — and restarts its wait, so
// a port whose egress cannot be replaced is not tried again on every acquire.
func (p *Pool) reviveIfDue(ctx context.Context, pt *poolPort) error {
	now := p.cfg.Now()

	pt.mu.Lock()
	due := pt.quarantined && p.revivableAt(pt.revivals, pt.quarantinedAt, now)
	if due {
		// The attempt is what costs, not the outcome: a rotation that fails is
		// as expensive as one that works, so both count against MaxRevivals.
		pt.revivals++
	}
	pt.mu.Unlock()
	if !due {
		return nil
	}

	p.markBadEgress(pt.num)
	if err := p.rotateEgress(ctx, pt.num); err != nil {
		pt.mu.Lock()
		pt.quarantinedAt = now
		pt.mu.Unlock()
		return err
	}

	pt.mu.Lock()
	pt.quarantined = false
	pt.quarantinedAt = time.Time{}
	pt.strikes = 0
	pt.failures = 0
	pt.mu.Unlock()

	p.mu.Lock()
	p.stats.Revivals++
	p.mu.Unlock()
	return nil
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
	pt.answered = false
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
		pt.quarantinedAt = p.cfg.Now()
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
		pt.quarantinedAt = p.cfg.Now()
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

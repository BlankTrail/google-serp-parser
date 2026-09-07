// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ChannelKind names how a channel produces egress addresses.
type ChannelKind string

const (
	// KindList rotates through a user-supplied proxy list.
	KindList ChannelKind = "list"
	// KindRotating uses one entry point whose IP changes when a provider URL is called.
	KindRotating ChannelKind = "rotating"
	// KindGateway egresses through a VPN gateway config stored in BlankTrail.
	KindGateway ChannelKind = "gateway"
	// KindDirect egresses from the host's own IP.
	KindDirect ChannelKind = "direct"
)

// ErrRenewUnsupported is returned by Renew on channels whose egress IP cannot be
// changed on demand. A caller seeing it should stop asking and quarantine the
// port instead of retrying forever.
var ErrRenewUnsupported = errors.New("blanktrail: this channel cannot change its egress IP")

// Channel is a source of egress addresses for proxy ports.
type Channel interface {
	// Name is the user-facing label of this channel.
	Name() string
	// Kind reports how the channel produces addresses.
	Kind() ChannelKind
	// Next returns an egress for a freshly opened port. The bool is false when
	// the channel has nothing to hand out.
	Next() (Egress, bool)
	// Renew changes the egress IP behind cur and returns the egress to use from
	// now on. It returns ErrRenewUnsupported when the channel has a fixed IP.
	Renew(ctx context.Context, cur Egress) (Egress, error)
	// MarkBad records that an egress carried a request and the answer was
	// refused. Whether that costs the address anything is the channel's own
	// business.
	MarkBad(Egress)
	// MarkDead records that an egress did not carry the request at all. A
	// channel that keeps a list is expected to stop handing this one out.
	MarkDead(Egress)
	// Close releases any background resources.
	Close()
}

// --- list channel ---

type listChannel struct {
	name  string
	rotor *Rotor
}

// NewListChannel rotates through a proxy list. Renew advances to the next entry.
func NewListChannel(name string, r *Rotor) Channel {
	return &listChannel{name: name, rotor: r}
}

func (c *listChannel) Name() string      { return c.name }
func (c *listChannel) Kind() ChannelKind { return KindList }

func (c *listChannel) Next() (Egress, bool) {
	u, ok := c.rotor.Next()
	if !ok {
		return Egress{}, false
	}
	return Egress{Upstream: u.URL()}, true
}

// NextFree is Next without taking an address off the bench early. See
// Rotor.NextFree for why the two answers are both wanted.
func (c *listChannel) NextFree() (Egress, bool) {
	u, ok := c.rotor.NextFree()
	if !ok {
		return Egress{}, false
	}
	return Egress{Upstream: u.URL()}, true
}

func (c *listChannel) Renew(_ context.Context, _ Egress) (Egress, error) {
	eg, ok := c.Next()
	if !ok {
		return Egress{}, fmt.Errorf("blanktrail: channel %q has no upstreams left", c.name)
	}
	return eg, nil
}

func (c *listChannel) MarkBad(eg Egress) {
	if u, ok := oneUpstream(eg); ok {
		c.rotor.MarkBad(u)
	}
}

func (c *listChannel) MarkDead(eg Egress) {
	if u, ok := oneUpstream(eg); ok {
		c.rotor.MarkDead(u)
	}
}

// freeEgress asks a channel for an address nothing is resting on, and reports
// whether there was one.
//
// A channel that cannot tell the difference answers the way it always does. The
// distinction only exists for a list: a gateway or a single rotating address
// has no bench to be resting on.
func freeEgress(ch Channel) (Egress, bool) {
	if free, ok := ch.(interface{ NextFree() (Egress, bool) }); ok {
		return free.NextFree()
	}
	return ch.Next()
}

// oneUpstream reads back the address an egress was built from, and says no when
// the text is not one address.
func oneUpstream(eg Egress) (Upstream, bool) {
	ups, _ := Parse(eg.Upstream, "socks5")
	if len(ups) != 1 {
		return Upstream{}, false
	}
	return ups[0], true
}

func (c *listChannel) Close() { c.rotor.Close() }

// Len is how many addresses this channel's list holds, and Resting is the ones
// set aside off a failure. They are here rather than on Channel because a
// direct connection and a fixed address have no list to answer about.
func (c *listChannel) Len() int        { return c.rotor.Len() }
func (c *listChannel) Resting() int    { return c.rotor.RestingHere() }
func (c *listChannel) ReleaseAll() int { return c.rotor.ReleaseAll() }

// --- rotating channel ---

type rotatingChannel struct {
	name        string
	up          Upstream
	rotateURL   string
	minInterval time.Duration

	hc  *http.Client
	now func() time.Time

	mu      sync.Mutex
	lastHit time.Time
	everHit bool
}

// RotatingOption customises a rotating channel.
type RotatingOption func(*rotatingChannel)

// WithRotateHTTPClient overrides the HTTP client used to call the rotate URL.
func WithRotateHTTPClient(hc *http.Client) RotatingOption {
	return func(c *rotatingChannel) { c.hc = hc }
}

// WithRotateClock overrides the clock, for tests.
func WithRotateClock(now func() time.Time) RotatingOption {
	return func(c *rotatingChannel) { c.now = now }
}

// NewRotatingChannel drives a rotating proxy: one fixed entry point whose exit
// IP changes when rotateURL is called. minInterval is the shortest gap the
// provider tolerates between calls; renewals arriving sooner are skipped rather
// than queued, because the port is going to cool down anyway.
func NewRotatingChannel(name string, up Upstream, rotateURL string, minInterval time.Duration, opts ...RotatingOption) Channel {
	c := &rotatingChannel{
		name:        name,
		up:          up,
		rotateURL:   rotateURL,
		minInterval: minInterval,
		hc:          &http.Client{Timeout: 30 * time.Second},
		now:         time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *rotatingChannel) Name() string         { return c.name }
func (c *rotatingChannel) Kind() ChannelKind    { return KindRotating }
func (c *rotatingChannel) Next() (Egress, bool) { return Egress{Upstream: c.up.URL()}, true }

// A channel with one address has nowhere to move to, so neither report
// changes anything for it. The rotate URL is what replaces its address.
func (c *rotatingChannel) MarkBad(Egress)  {}
func (c *rotatingChannel) MarkDead(Egress) {}
func (c *rotatingChannel) Close()          {}

func (c *rotatingChannel) Renew(ctx context.Context, cur Egress) (Egress, error) {
	if c.rotateURL == "" {
		return cur, ErrRenewUnsupported
	}

	c.mu.Lock()
	now := c.now()
	if c.everHit && now.Sub(c.lastHit) < c.minInterval {
		c.mu.Unlock()
		return cur, nil // too soon; the provider would refuse or silently ignore it
	}
	c.lastHit, c.everHit = now, true
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.rotateURL, nil)
	if err != nil {
		return cur, fmt.Errorf("blanktrail: channel %q: build rotate request: %w", c.name, err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return cur, fmt.Errorf("blanktrail: channel %q: call rotate URL: %w", c.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cur, fmt.Errorf("blanktrail: channel %q: rotate URL answered HTTP %d", c.name, resp.StatusCode)
	}
	return cur, nil // same entry point, different exit IP
}

// --- gateway and direct channels ---

type fixedChannel struct {
	name string
	kind ChannelKind
	eg   Egress
}

// NewGatewayChannel egresses through a named BlankTrail gateway config.
func NewGatewayChannel(name, gateway string) Channel {
	return &fixedChannel{name: name, kind: KindGateway, eg: Egress{Gateway: gateway}}
}

// NewDirectChannel egresses from the host's own IP.
func NewDirectChannel(name string) Channel {
	return &fixedChannel{name: name, kind: KindDirect}
}

func (c *fixedChannel) Name() string         { return c.name }
func (c *fixedChannel) Kind() ChannelKind    { return c.kind }
func (c *fixedChannel) Next() (Egress, bool) { return c.eg, true }

// A fixed address is the caller's choice and stays whatever it does.
func (c *fixedChannel) MarkBad(Egress)  {}
func (c *fixedChannel) MarkDead(Egress) {}
func (c *fixedChannel) Close()          {}

func (c *fixedChannel) Renew(_ context.Context, cur Egress) (Egress, error) {
	return cur, ErrRenewUnsupported
}

// --- mixer ---

// initialWeight is how many assignment slots a healthy channel starts with.
// Four is enough that a channel survives a few isolated blocks but dies within
// one run if it is genuinely burned.
const initialWeight = 4

// Mixer spreads ports over several channels and demotes the ones that keep
// getting blocked. It is safe for concurrent use.
type Mixer struct {
	mu       sync.Mutex
	channels []Channel
	weights  map[string]int
}

// NewMixer builds a mixer over the given channels, all starting equal.
func NewMixer(chs ...Channel) *Mixer {
	m := &Mixer{weights: map[string]int{}}
	for _, ch := range chs {
		if ch == nil {
			continue
		}
		m.channels = append(m.channels, ch)
		m.weights[ch.Name()] = initialWeight
	}
	return m
}

// Weight reports a channel's current assignment weight (0 = excluded).
func (m *Mixer) Weight(ch Channel) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.weights[ch.Name()]
}

// Penalise lowers a channel's weight after a block. At zero it stops receiving
// ports until Reward brings it back.
func (m *Mixer) Penalise(ch Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.weights[ch.Name()]; w > 0 {
		m.weights[ch.Name()] = w - 1
	}
}

// Reward raises a channel's weight after a success, capped at twice the start.
func (m *Mixer) Reward(ch Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.weights[ch.Name()]; w < initialWeight*2 {
		m.weights[ch.Name()] = w + 1
	}
}

// Channels lists every channel this mixer holds, healthy or not.
//
// A penalised channel still has its list, and a reading of how many addresses
// there are that left out the channel nobody is drawing from any more would say
// the pool has fewer addresses than it does.
func (m *Mixer) Channels() []Channel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Channel(nil), m.channels...)
}

// Healthy lists the channels still receiving ports.
func (m *Mixer) Healthy() []Channel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.healthyLocked()
}

func (m *Mixer) healthyLocked() []Channel {
	var out []Channel
	for _, ch := range m.channels {
		if m.weights[ch.Name()] > 0 {
			out = append(out, ch)
		}
	}
	return out
}

// Assign picks a channel for each of n ports, proportionally to weight. Distinct
// channels are preferred while there are more channels than ports, so a small
// run still spreads over every configured egress. Returns fewer than n entries
// only when no channel is healthy.
func (m *Mixer) Assign(n int) []Channel {
	if n <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	healthy := m.healthyLocked()
	if len(healthy) == 0 {
		return nil
	}

	// Build a deterministic slot list: each channel repeated by its weight,
	// interleaved so consecutive ports land on different channels.
	sorted := append([]Channel(nil), healthy...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return m.weights[sorted[i].Name()] > m.weights[sorted[j].Name()]
	})

	var slots []Channel
	for round := 0; ; round++ {
		added := false
		for _, ch := range sorted {
			if m.weights[ch.Name()] > round {
				slots = append(slots, ch)
				added = true
			}
		}
		if !added {
			break
		}
	}

	out := make([]Channel, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, slots[i%len(slots)])
	}
	return out
}

// Close closes every channel in the mixer.
func (m *Mixer) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.channels {
		ch.Close()
	}
}

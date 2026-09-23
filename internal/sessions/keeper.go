// SPDX-License-Identifier: MIT

package sessions

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// KeptFor is how long a session nobody has used is kept.
//
// Twelve hours, as the operator set it: long enough that a job run twice a day
// finds the sessions the morning run made, and short enough that what is kept
// is still something Google remembers.
const KeptFor = 12 * time.Hour

// WarmAfter is how long a session may go unused before the warmer takes it up:
// an hour short of KeptFor, so a session warmed on time never runs out, and a
// session costs one warming request in half a day rather than one every quarter
// of an hour. The operator chose it over warming every idle session every
// fifteen minutes, which at two hundred sessions was eight hundred requests an
// hour spent on nothing but staying known.
const WarmAfter = KeptFor - time.Hour

// sweepEvery is how often the keeper gives up the sessions that ran out while
// the program was up. The history is swept when it is first read; a server up
// for days reads it once.
const sweepEvery = time.Hour

// History is what the keeper needs from the history: the sessions, and a record
// of what became of each one.
type History interface {
	Sessions(ctx context.Context, device string, since time.Time) ([]store.Session, error)
	NewSession(ctx context.Context, s store.Session) (int64, error)
	SessionAnswered(ctx context.Context, id int64, a store.Answer, at time.Time) error
	SessionFailed(ctx context.Context, id int64, at time.Time) (bool, error)
	DropSession(ctx context.Context, id int64) error
	DropStaleSessions(ctx context.Context, before time.Time) (int, error)
}

var (
	// ErrNotHeld is returned when a session is given back twice, or by a handle
	// to a session another thread has taken since.
	ErrNotHeld = errors.New("sessions: the session is not held")
	// ErrNoAddress is returned when every address the list offers is already
	// carrying as many sessions at once as it may. It is a wait, not a fault.
	ErrNoAddress = errors.New("sessions: no address is free for a session")
	// ErrPortElsewhere is returned when the port does not stand on the address
	// the session was put on it for.
	ErrPortElsewhere = errors.New("sessions: the port stands on another address than the session's")
	// ErrNothingDue is returned by TakeColdest when no session has rested long
	// enough.
	ErrNothingDue = errors.New("sessions: no session is due")
)

// Keeper hands sessions to whoever asks and writes down what became of them.
//
// One keeper serves the whole program: a job, the warmer and a search answered
// inside a request all take sessions from it, and a session is held by one of
// them at a time.
//
// How many sessions there are is not a number anybody sets. A caller asks for
// one; if a session is free, fits what the caller wants, fits the port and has
// rested the caller's pause, it gets the one used most recently; otherwise a new
// one is made. So a short pause settles on few sessions and a long one on many.
type Keeper struct {
	history History
	now     func() time.Time
	// rand draws each session's rest between the pause and half again more; a
	// test pins it.
	rand func() float64

	mu    sync.Mutex
	known map[int64]*kept
	// loaded are the kinds of result page whose sessions have been read.
	loaded map[string]bool
	// used is when each address last carried a session. It outlives the
	// sessions on it: "the one that has rested longest" is about the address.
	used map[string]time.Time
	// reserved counts, per address, new sessions chosen for it and not yet
	// written down, so two made at once do not both take the last free place.
	reserved  map[string]int
	lastSweep time.Time
}

// kept is one session as the keeper holds it between uses.
type kept struct {
	record store.Session
	jar    *Jar
	held   bool
	// spread is how much longer than the taker's pause this session rests
	// since it was last used, as a share of the pause: from nought to a half.
	// Drawn again at every use, so no session is asked again on a metronome.
	spread float64
}

// restSpread is how far above the pause a session's rest may reach, as a share
// of it: half, so a pause of sixty seconds is a rest of sixty to ninety — the
// span the operator measured as the one a session is best asked again in.
const restSpread = 0.5

// rested says whether a session has rested for the taker's pause, drawn out by
// its own spread.
func (s *kept) rested(pause time.Duration, now time.Time) bool {
	rest := pause + time.Duration(float64(pause)*s.spread)
	return now.Sub(s.record.UsedAt) >= rest
}

// Held is a session a caller holds: which one, and the jar to search with.
type Held struct {
	ID      int64
	Profile string
	Jar     *Jar
	keeper  *Keeper
	done    bool
}

// NewKeeper returns a keeper over a history.
func NewKeeper(history History) *Keeper {
	return &Keeper{
		history: history, now: time.Now, rand: rand.Float64,
		known: map[int64]*kept{}, loaded: map[string]bool{},
		used: map[string]time.Time{}, reserved: map[string]int{},
	}
}

// Take hands the caller a session on the port it holds — one that has rested,
// or a new one — and puts it on the port.
func (k *Keeper) Take(ctx context.Context, p Port, w Want) (*Held, error) {
	if err := k.load(ctx, w.Device); err != nil {
		return nil, err
	}
	if err := k.sweep(ctx); err != nil {
		return nil, err
	}
	if s := k.pick(p, w); s != nil {
		return k.put(ctx, p, s)
	}
	return k.fresh(ctx, p, w)
}

// TakeColdest hands the caller the session unused longest, provided it has been
// unused for at least idle, and puts it on the port. It is what warming takes.
func (k *Keeper) TakeColdest(ctx context.Context, p Port, w Want, idle time.Duration) (*Held, error) {
	if err := k.load(ctx, w.Device); err != nil {
		return nil, err
	}
	if err := k.sweep(ctx); err != nil {
		return nil, err
	}
	s := k.coldest(p, w.Device, idle)
	if s == nil {
		return nil, ErrNothingDue
	}
	return k.put(ctx, p, s)
}

// fitsPort says whether a session may go on this port: a gateway's session only
// on its gateway, an address's only on an address, and not on an address already
// working as many sessions as it may.
//
// A session that has answered and whose address is resting waits for it. It
// holds a clearance for that address, and taken elsewhere it would spend it on a
// challenge; the rest runs out, and the address carries it again or leaves the
// list. A session that has never answered has nothing to wait for.
func fitsPort(s *kept, portExit string, busy map[string]int, limit int, p Port) bool {
	if onGateway(portExit) || onGateway(s.record.Exit) {
		return s.record.Exit == portExit
	}
	a, ok := addressOf(s.record.Exit)
	if !ok {
		return true
	}
	offered := p.Offers(a)
	if !offered && hasAnswered(s.record) && p.Knows(a) {
		return false
	}
	if offered && busy[a] >= limit {
		return false
	}
	return true
}

// pick takes the free session that fits, rested for the caller's pause and used
// most recently, and marks it held.
func (k *Keeper) pick(p Port, w Want) *kept {
	portExit, limit := p.Exit(), p.Limit()
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	busy := k.busyLocked()
	var best *kept
	for _, s := range k.known {
		r := s.record
		if s.held || r.Device != w.Device || !w.matches(r.Browser, r.OS, r.Release) ||
			!s.rested(w.Pause, now) || now.Sub(r.UsedAt) > KeptFor ||
			!fitsPort(s, portExit, busy, limit, p) {
			continue
		}
		if best == nil || r.UsedAt.After(best.record.UsedAt) ||
			(r.UsedAt.Equal(best.record.UsedAt) && r.ID > best.record.ID) {
			best = s
		}
	}
	if best != nil {
		best.held = true
	}
	return best
}

// coldest takes the free session of the kind that fits the port and has been
// unused longest, if that is at least idle, and marks it held.
func (k *Keeper) coldest(p Port, device string, idle time.Duration) *kept {
	portExit, limit := p.Exit(), p.Limit()
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	busy := k.busyLocked()
	var best *kept
	for _, s := range k.known {
		r := s.record
		if s.held || r.Device != device || now.Sub(r.UsedAt) < idle || now.Sub(r.UsedAt) > KeptFor ||
			!fitsPort(s, portExit, busy, limit, p) {
			continue
		}
		if best == nil || r.UsedAt.Before(best.record.UsedAt) {
			best = s
		}
	}
	if best != nil {
		best.held = true
	}
	return best
}

// busyLocked counts, per address, the sessions working through it now and the
// new ones chosen for it and not yet written down.
func (k *Keeper) busyLocked() map[string]int {
	busy := map[string]int{}
	for _, s := range k.known {
		if a, ok := addressOf(s.record.Exit); ok && s.held {
			busy[a]++
		}
	}
	for a, n := range k.reserved {
		busy[a] += n
	}
	return busy
}

// put puts a held session on the port: fingerprint, address, tickets. A session
// that does not go on is put back for another caller rather than given up —
// a port that will not take it is the port's trouble, not the session's.
func (k *Keeper) put(ctx context.Context, p Port, s *kept) (*Held, error) {
	if err := p.Wear(ctx, s.record.Profile); err != nil {
		k.release(s)
		return nil, fmt.Errorf("sessions: putting session %d's fingerprint on port %d: %w", s.record.ID, p.Number(), err)
	}
	address := ""
	pinned := hasAnswered(s.record)
	if !onGateway(s.record.Exit) {
		a, ok := addressOf(s.record.Exit)
		// A session that has never answered goes wherever an address is free.
		// One that has answered moves only when its address has left the list
		// — a resting one it waited for — and pays a challenge where it lands:
		// that is what changing exit costs.
		if move := !ok || (pinned && !p.Knows(a)) || (!pinned && !p.Offers(a)); move {
			if a, ok = k.reserve(candidatesOf(p), p.Limit()); !ok {
				k.release(s)
				return nil, ErrNoAddress
			}
			defer k.unreserve(a)
		}
		if p.Exit() != addrExit+a {
			if err := p.MoveTo(ctx, a); err != nil {
				k.release(s)
				return nil, fmt.Errorf("sessions: moving port %d to session %d's address: %w", p.Number(), s.record.ID, err)
			}
		}
		k.mu.Lock()
		s.record.Exit = addrExit + a
		k.mu.Unlock()
		address = a
	}
	// A session that has answered keeps its port on its address for the length
	// of the lease; one that never has lets its requests go where they can.
	p.Stay(pinned)
	elsewhere, err := p.PutTickets(ctx, address, s.record.Tickets)
	if err != nil {
		k.release(s)
		return nil, fmt.Errorf("sessions: loading session %d's tickets onto port %d: %w", s.record.ID, p.Number(), err)
	}
	if elsewhere {
		k.release(s)
		return nil, fmt.Errorf("%w: port %d, session %d", ErrPortElsewhere, p.Number(), s.record.ID)
	}
	return &Held{ID: s.record.ID, Profile: s.record.Profile, Jar: s.jar, keeper: k}, nil
}

// fresh makes a new session on the port: a fresh fingerprint from the port's
// template, an address by the rule — or the port's gateway — and no tickets.
func (k *Keeper) fresh(ctx context.Context, p Port, w Want) (*Held, error) {
	fp, err := p.Freshen(ctx)
	if err != nil {
		return nil, fmt.Errorf("sessions: a fresh fingerprint on port %d: %w", p.Number(), err)
	}
	if fp.Profile == "" {
		return nil, fmt.Errorf("sessions: port %d does not say which fingerprint it wears", p.Number())
	}
	exit, address := p.Exit(), ""
	if !onGateway(exit) {
		a, ok := k.reserve(candidatesOf(p), p.Limit())
		if !ok {
			return nil, ErrNoAddress
		}
		defer k.unreserve(a)
		if exit != addrExit+a {
			if err := p.MoveTo(ctx, a); err != nil {
				return nil, fmt.Errorf("sessions: moving port %d to a new session's address: %w", p.Number(), err)
			}
		}
		exit, address = addrExit+a, a
	}
	// A new session has nothing to lose: its first request goes wherever an
	// address will carry it.
	p.Stay(false)
	// A new session starts with no tickets, and the port may still hold the
	// last session's. Loading an empty set is what wipes them.
	elsewhere, err := p.PutTickets(ctx, address, nil)
	if err != nil {
		return nil, fmt.Errorf("sessions: clearing port %d's tickets for a new session: %w", p.Number(), err)
	}
	if elsewhere {
		return nil, fmt.Errorf("%w: port %d, a new session", ErrPortElsewhere, p.Number())
	}
	now := k.now()
	record := store.Session{Profile: fp.Profile, Browser: fp.Browser, OS: fp.OS, Release: fp.Release,
		Device: w.Device, Exit: exit, CreatedAt: now, UsedAt: now}
	id, err := k.history.NewSession(ctx, record)
	if err != nil {
		return nil, err
	}
	record.ID = id
	jar := NewJar()
	jar.now = k.now
	k.mu.Lock()
	k.known[id] = &kept{record: record, jar: jar, held: true}
	if address != "" {
		k.used[address] = now
	}
	k.mu.Unlock()
	return &Held{ID: id, Profile: fp.Profile, Jar: jar, keeper: k}, nil
}

// Choose is the rule for giving a session an address, for the pool to use when a
// request has to be carried elsewhere: the address carrying the fewest sessions,
// and of those the one rested longest; one already working as many sessions at
// once as it may is passed over.
func (k *Keeper) Choose(candidates []string, limit int) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.chooseLocked(candidates, limit)
}

func (k *Keeper) chooseLocked(candidates []string, limit int) (string, bool) {
	if limit < 1 {
		limit = 1
	}
	on := map[string]int{}
	for _, s := range k.known {
		if a, ok := addressOf(s.record.Exit); ok {
			on[a]++
		}
	}
	for a, n := range k.reserved {
		on[a] += n
	}
	busy := k.busyLocked()
	best, found := "", false
	for _, a := range candidates {
		if busy[a] >= limit {
			continue
		}
		if !found || on[a] < on[best] || (on[a] == on[best] && k.used[a].Before(k.used[best])) {
			best, found = a, true
		}
	}
	return best, found
}

// reserve chooses an address and holds a place on it until unreserve, so two
// new sessions made at once do not both take the last free place.
func (k *Keeper) reserve(candidates []string, limit int) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	a, ok := k.chooseLocked(candidates, limit)
	if ok {
		k.reserved[a]++
	}
	return a, ok
}

func (k *Keeper) unreserve(a string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.reserved[a]--; k.reserved[a] <= 0 {
		delete(k.reserved, a)
	}
}

// release lets go of a session without anything recorded against it.
func (k *Keeper) release(s *kept) {
	k.mu.Lock()
	s.held = false
	k.mu.Unlock()
}

// load reads the history once per kind of result page: the sessions used in
// the last KeptFor, after the older ones are swept.
func (k *Keeper) load(ctx context.Context, device string) error {
	k.mu.Lock()
	done := k.loaded[device]
	k.mu.Unlock()
	if done {
		return nil
	}
	now := k.now()
	if _, err := k.history.DropStaleSessions(ctx, now.Add(-KeptFor)); err != nil {
		return err
	}
	all, err := k.history.Sessions(ctx, device, now.Add(-KeptFor))
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.loaded[device] {
		return nil
	}
	for _, one := range all {
		if _, have := k.known[one.ID]; have {
			continue
		}
		jar, err := ReadJar(one.Cookies)
		if err != nil {
			// A jar that cannot be read is a session that cannot be resumed. It
			// is left to the sweep rather than given up here: a history this
			// program cannot read is a fault to report, not to delete.
			continue
		}
		jar.now = k.now
		k.known[one.ID] = &kept{record: one, jar: jar, spread: k.rand() * restSpread}
		if a, ok := addressOf(one.Exit); ok && one.UsedAt.After(k.used[a]) {
			k.used[a] = one.UsedAt
		}
	}
	k.loaded[device] = true
	k.lastSweep = now
	return nil
}

// sweep gives up, at most once an hour, the sessions that ran out while the
// program was up — in the history and here.
func (k *Keeper) sweep(ctx context.Context) error {
	now := k.now()
	k.mu.Lock()
	due := now.Sub(k.lastSweep) >= sweepEvery
	if due {
		k.lastSweep = now
		for id, s := range k.known {
			if !s.held && now.Sub(s.record.UsedAt) > KeptFor {
				delete(k.known, id)
			}
		}
	}
	k.mu.Unlock()
	if !due {
		return nil
	}
	_, err := k.history.DropStaleSessions(ctx, now.Add(-KeptFor))
	return err
}

// Save writes the session down after an answer and keeps holding it: its
// cookies, the port's tickets for it, and where the port goes out now.
//
// It is for a walk, which keeps one session for all its pages: a run can end
// between two pages, and a clearance won on page one and never written down is
// paid for again. Where the port goes out now is written because a request an
// address did not carry is taken to another, and the session goes with it.
func (h *Held) Save(ctx context.Context, p Port) error { return h.keeper.write(ctx, h, p, false) }

// Answered writes the session down after an answer and gives it back.
func (h *Held) Answered(ctx context.Context, p Port) error { return h.keeper.write(ctx, h, p, true) }

// Failed gives the session back after a refusal, and says whether it was given
// up — at the second refusal in a row.
func (h *Held) Failed(ctx context.Context) (bool, error) { return h.keeper.failed(ctx, h) }

// PutBack gives the session back with nothing recorded against it.
func (h *Held) PutBack() {
	k := h.keeper
	k.mu.Lock()
	defer k.mu.Unlock()
	if h.done {
		return
	}
	h.done = true
	if s, ok := k.known[h.ID]; ok {
		s.held = false
	}
}

func (k *Keeper) write(ctx context.Context, h *Held, p Port, giveBack bool) error {
	k.mu.Lock()
	s, ok := k.known[h.ID]
	if h.done || !ok || !s.held {
		k.mu.Unlock()
		return ErrNotHeld
	}
	exit, tickets := s.record.Exit, s.record.Tickets
	k.mu.Unlock()

	if giveBack {
		// Given back whatever the writing does: a session left held because
		// the history would not take a line is a session nobody uses again.
		defer func() {
			k.mu.Lock()
			h.done, s.held = true, false
			k.mu.Unlock()
		}()
	}
	if got, err := p.TakeTickets(ctx); err == nil {
		tickets = got
	}
	if now := p.Exit(); now != "" && !onGateway(exit) {
		exit = now
	}
	written, err := s.jar.MarshalJSON()
	if err != nil {
		return err
	}
	at := k.now()
	if err := k.history.SessionAnswered(ctx, h.ID, store.Answer{Cookies: written, Tickets: tickets, Exit: exit}, at); err != nil {
		return err
	}
	k.mu.Lock()
	s.record.UsedAt, s.record.Failures, s.record.Tickets, s.record.Exit = at, 0, tickets, exit
	s.record.Cookies = written
	s.spread = k.rand() * restSpread
	if a, ok := addressOf(exit); ok {
		k.used[a] = at
	}
	k.mu.Unlock()
	return nil
}

// MoveOn puts the session on another address and keeps it in hand.
//
// It is for a page that never reached Google: the road failed, not the session,
// and there is nothing to rest from — nothing was answered. So the session
// stays held, the port is moved to another address the list offers, and the
// caller asks again at once. Nothing is held against the session, and the
// tickets go with the address they were handed out by.
//
// A session on a gateway has nowhere to go: the gateway is the whole of what it
// is. Saying so lets the caller spend its tries on the gateway rather than on a
// move that cannot happen.
func (h *Held) MoveOn(ctx context.Context, p Port) error { return h.keeper.moveOn(ctx, h, p) }

func (k *Keeper) moveOn(ctx context.Context, h *Held, p Port) error {
	k.mu.Lock()
	s, ok := k.known[h.ID]
	if h.done || !ok || !s.held {
		k.mu.Unlock()
		return ErrNotHeld
	}
	was := s.record.Exit
	k.mu.Unlock()
	if onGateway(was) {
		return fmt.Errorf("%w: session %d is on a gateway", ErrNoAddress, h.ID)
	}
	// The address it is leaving is not among the choices: it has just shown it
	// cannot carry this session's request.
	var elsewhere []string
	leaving, _ := addressOf(was)
	for _, a := range candidatesOf(p) {
		if a != leaving {
			elsewhere = append(elsewhere, a)
		}
	}
	to, ok := k.reserve(elsewhere, p.Limit())
	if !ok {
		return ErrNoAddress
	}
	defer k.unreserve(to)
	if err := p.MoveTo(ctx, to); err != nil {
		return fmt.Errorf("sessions: moving session %d to another address: %w", h.ID, err)
	}
	// The tickets belonged to the exit it has left and mean nothing at this
	// one; the port is given none.
	if _, err := p.PutTickets(ctx, to, nil); err != nil {
		return fmt.Errorf("sessions: clearing session %d's tickets on port %d: %w", h.ID, p.Number(), err)
	}
	k.mu.Lock()
	s.record.Exit, s.record.Tickets = addrExit+to, nil
	k.mu.Unlock()
	return nil
}

// GiveUp takes the session out of the history: it is dead.
//
// It is for a session whose walk cannot go on — the deep pages it was keeping
// are addressed to an exit it no longer has, and Google has answered it with a
// refusal from the new one. Handing it out again would spend a port on a
// session that can only fail.
func (h *Held) GiveUp(ctx context.Context) error { return h.keeper.giveUp(ctx, h) }

func (k *Keeper) giveUp(ctx context.Context, h *Held) error {
	k.mu.Lock()
	s, ok := k.known[h.ID]
	if h.done || !ok || !s.held {
		k.mu.Unlock()
		return ErrNotHeld
	}
	h.done, s.held = true, false
	delete(k.known, h.ID)
	k.mu.Unlock()
	if err := k.history.DropSession(ctx, h.ID); err != nil && !errors.Is(err, store.ErrNoSession) {
		return err
	}
	return nil
}

// Elsewhere gives the session back and takes it off the address it went out
// through, holding nothing against it.
//
// It is for an address that could not carry the session past a check Google set
// on it. Such a check is the address's to pass — the service passes it in a
// browser of its own through that same address — and an address whose browser
// could not open a connection to pass it once will not pass it for the next
// request either. The session is not what failed: its cookies, its tickets and
// its fingerprint are what they were, and only where it goes out is decided
// afresh the next time it is taken.
//
// The tickets go with the address. They are what that exit's TLS handed out and
// mean nothing at another.
//
// A session on a gateway stays where it is. The gateway is the whole of what
// such a session is — moved to another exit it would be another session — so
// there is nowhere to take it.
func (h *Held) Elsewhere(ctx context.Context) error { return h.keeper.elsewhere(ctx, h) }

func (k *Keeper) elsewhere(ctx context.Context, h *Held) error {
	k.mu.Lock()
	s, ok := k.known[h.ID]
	if h.done || !ok || !s.held {
		k.mu.Unlock()
		return ErrNotHeld
	}
	h.done, s.held = true, false
	exit := s.record.Exit
	k.mu.Unlock()
	if onGateway(exit) {
		return nil
	}
	written, err := s.jar.MarshalJSON()
	if err != nil {
		return err
	}
	at := k.now()
	if err := k.history.SessionAnswered(ctx, h.ID, store.Answer{Cookies: written}, at); err != nil {
		return err
	}
	k.mu.Lock()
	s.record.UsedAt, s.record.Failures, s.record.Tickets, s.record.Exit = at, 0, nil, ""
	s.record.Cookies = written
	s.spread = k.rand() * restSpread
	if a, ok := addressOf(exit); ok {
		// The address carried a request and is spent for the pause all the
		// same: what it could not do was pass the check at the end of it.
		k.used[a] = at
	}
	k.mu.Unlock()
	return nil
}

func (k *Keeper) failed(ctx context.Context, h *Held) (bool, error) {
	k.mu.Lock()
	s, ok := k.known[h.ID]
	if h.done || !ok || !s.held {
		k.mu.Unlock()
		return false, ErrNotHeld
	}
	h.done, s.held = true, false
	k.mu.Unlock()

	now := k.now()
	dropped, err := k.history.SessionFailed(ctx, h.ID, now)
	if err != nil && !errors.Is(err, store.ErrNoSession) {
		return false, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if a, ok := addressOf(s.record.Exit); ok {
		k.used[a] = now
	}
	if dropped || errors.Is(err, store.ErrNoSession) {
		delete(k.known, h.ID)
		return true, nil
	}
	// A refusal is a use like any other as far as the pause is concerned.
	s.record.UsedAt = now
	s.record.Failures++
	s.spread = k.rand() * restSpread
	return false, nil
}

// Count is how many sessions the keeper knows, and how many are held now.
func (k *Keeper) Count() (all, held int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, s := range k.known {
		all++
		if s.held {
			held++
		}
	}
	return all, held
}

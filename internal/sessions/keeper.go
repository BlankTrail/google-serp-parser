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
	// moved says this session was put on an address other than the one it last
	// answered from. It is worked out afresh every time the session is put on a
	// port, so an answer from the new address leaves it false the next time
	// round. What reads it is the count of Google's checks: a session arriving
	// from somewhere new is a stranger there whatever pace it is asked at, so
	// the check it pays says nothing about the pace.
	moved bool
	// spread is where between the two ends of the taker's span this session's
	// own rest falls: nought is the least it may rest and one the most. Drawn
	// again at every use, so no session is asked again on a metronome.
	spread float64
}

// restSpread is how far above the floor a session's rest reaches when the taker
// names no ceiling, as a share of the floor: half, so a pause of sixty seconds
// is a rest of sixty to ninety. It is what the one number meant before a taker
// could name both ends.
const restSpread = 0.5

// rested says whether a session has rested the span the taker asks for.
func (s *kept) rested(w Want, now time.Time) bool {
	return now.Sub(s.record.UsedAt) >= s.restFor(w)
}

// restFor is the rest this session owes a taker asking for this span: the least
// of it, and its own draw of the distance to the most. The draw is the
// session's and is taken again at every use — see kept.spread — so two sessions
// resting the same span do not come due together, and neither comes due on a
// metronome.
func (s *kept) restFor(w Want) time.Duration {
	from, to := w.Pause, w.Longest()
	return from + time.Duration(float64(to-from)*s.spread)
}

// Held is a session a caller holds: which one, and the jar to search with.
type Held struct {
	ID      int64
	Profile string
	Jar     *Jar
	// Fresh says this session was made for this taking rather than handed on
	// from an earlier one.
	//
	// A caller watching how a run gets up to speed reads it: a thread makes a
	// session only when none it could use has rested, so a run still making them
	// is a run still widening, and one that has stopped has as many as its
	// threads can keep busy.
	Fresh  bool
	keeper *Keeper
	done   bool
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
	if s := k.pick(p, w, nil); s != nil {
		return k.put(ctx, p, s, false)
	}
	return k.fresh(ctx, p, w)
}

// TakeOneOf hands the caller whichever of the named sessions is due, and makes
// none: where Take would answer with a new session, this answers ErrNothingDue.
//
// It is what a caller asks when it has no work a new session could do. A thread
// walking queries names the sessions carrying one part way: the page it would
// take next is addressed to one of those and to no other, so a new session
// could not take it, and a thread that asked Take every time it looked for one
// would make a session for every glance.
func (k *Keeper) TakeOneOf(ctx context.Context, p Port, w Want, only []int64) (*Held, error) {
	if err := k.load(ctx, w.Device); err != nil {
		return nil, err
	}
	if err := k.sweep(ctx); err != nil {
		return nil, err
	}
	among := make(map[int64]bool, len(only))
	for _, id := range only {
		among[id] = true
	}
	s := k.pick(p, w, among)
	if s == nil {
		return nil, ErrNothingDue
	}
	return k.put(ctx, p, s, false)
}

// TakeStranded hands the caller one of the given sessions that is stranded —
// it has answered, has served its own rest, and is waiting for nothing but its
// own address to come back from a rest — and puts it on the port at an address
// that is free.
//
// A session that has answered waits for its own address rather than moving: it
// holds a clearance there, and taken elsewhere it pays a check. This is the one
// exception, and the user chose it. At the end of a job, with nothing left to
// open and nothing due, the last queries of the speed test waited for their
// sessions' addresses to come off a ban — an hour on the profile it ran on —
// where the check a move costs is under a minute. The caller asks for this only
// then; while there is anything else to do, a session waits.
func (k *Keeper) TakeStranded(ctx context.Context, p Port, w Want, only []int64) (*Held, error) {
	if err := k.load(ctx, w.Device); err != nil {
		return nil, err
	}
	if err := k.sweep(ctx); err != nil {
		return nil, err
	}
	among := make(map[int64]bool, len(only))
	for _, id := range only {
		among[id] = true
	}
	s := k.stranded(p, w, among)
	if s == nil {
		return nil, ErrNothingDue
	}
	return k.put(ctx, p, s, true)
}

// stranded takes, among only, the session that has waited longest for its own
// resting address and would otherwise be due, and marks it held.
func (k *Keeper) stranded(p Port, w Want, only map[int64]bool) *kept {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	var best *kept
	for _, s := range k.known {
		r := s.record
		if !only[r.ID] || s.held || !hasAnswered(r) || r.Device != w.Device ||
			!w.matches(r.Browser, r.OS, r.Release) || !s.rested(w, now) || now.Sub(r.UsedAt) > KeptFor {
			continue
		}
		if a, ok := addressOf(r.Exit); !ok || !p.Rests(a) {
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
	return k.put(ctx, p, s, false)
}

// fitsPort says whether a session may go on this port: a gateway's session only
// on its gateway, an address's only on an address, and not on an address already
// working as many sessions as it may.
//
// A session's address is its own for as long as the address carries its
// requests, and whether the list still holds it does not come into it. A list
// read again is somebody else's new copy of it, not a verdict on the addresses
// it leaves out: measured on a wingate list, half the addresses changed between
// two readings twenty seconds apart, while addresses read ten minutes earlier
// answered as often as fresh ones. A session carried off an address that still
// works pays a challenge where it lands, for nothing. What takes a session off
// its address is the address stopping: a request it did not carry puts it to
// rest.
//
// A session that has answered and whose address is resting waits for it. It
// holds a clearance for that address, and taken elsewhere it would spend it on a
// challenge; the rest runs out, and the address carries it again. A session
// that has never answered has nothing to wait for, and goes where an address is
// free.
func fitsPort(s *kept, portExit string, busy map[string]int, limit int, p Port) bool {
	if onGateway(portExit) || onGateway(s.record.Exit) {
		return s.record.Exit == portExit
	}
	a, ok := addressOf(s.record.Exit)
	if !ok {
		return true
	}
	if p.Rests(a) {
		return !hasAnswered(s.record)
	}
	return busy[a] < limit
}

// pick takes the free session that fits, rested for the caller's pause and used
// most recently, and marks it held. Where only is given, no session outside it
// is considered.
func (k *Keeper) pick(p Port, w Want, only map[int64]bool) *kept {
	portExit, limit := p.Exit(), p.Limit()
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	busy := k.busyLocked()
	var best *kept
	for _, s := range k.known {
		r := s.record
		if only != nil && !only[r.ID] {
			continue
		}
		if s.held || r.Device != w.Device || !w.matches(r.Browser, r.OS, r.Release) ||
			!s.rested(w, now) || now.Sub(r.UsedAt) > KeptFor ||
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
//
// away moves a session that has answered off its own address when that address
// is resting, which only TakeStranded asks for.
func (k *Keeper) put(ctx context.Context, p Port, s *kept, away bool) (*Held, error) {
	if err := p.Wear(ctx, s.record.Profile); err != nil {
		k.release(s)
		return nil, fmt.Errorf("sessions: putting session %d's fingerprint on port %d: %w", s.record.ID, p.Number(), err)
	}
	address := ""
	pinned := hasAnswered(s.record)
	if !onGateway(s.record.Exit) {
		a, ok := addressOf(s.record.Exit)
		// A session goes out through its own address, listed or not, until the
		// address stops carrying its requests. One that has never answered and
		// whose address is resting goes wherever an address is free; one that
		// has answered waited for its address instead (see fitsPort), and is
		// moved off it only by a request it did not carry — paying a challenge
		// where it lands, which is what changing exit costs — or at the end of
		// a job, when TakeStranded asks for it.
		if move := !ok || ((!pinned || away) && p.Rests(a)); move {
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
		// Somewhere other than where it last answered from: a stranger at this
		// address until it answers from it.
		s.moved = addrExit+a != s.record.Exit
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
	return &Held{ID: id, Profile: fp.Profile, Jar: jar, Fresh: true, keeper: k}, nil
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
		k.known[one.ID] = &kept{record: one, jar: jar, spread: k.rand()}
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

// clearanceCookie is what Google leaves on a session that has passed its check.
//
// It is the only sign there is. The page that comes back from a request that
// waited for the check is the page that was asked for — nothing in it says a
// check was met — and the cookie is issued at the moment one is passed, so a
// value that was not there before, or one that has changed, is a check newly
// paid for.
const clearanceCookie = "GOOGLE_ABUSE_EXEMPTION"

// Clearance is what Google's check left on this session, and empty where this
// session has never passed one.
//
// Every host the jar holds is looked at rather than one. A search for Russia
// wins its clearance on google.ru and one for Germany on google.de, and a
// caller asking "has this session just passed a check" has no business knowing
// which host answered it.
func (h *Held) Clearance() string {
	if h == nil || h.Jar == nil {
		return ""
	}
	for _, c := range h.Jar.Held() {
		if c.Name == clearanceCookie {
			return c.Value
		}
	}
	return ""
}

// Admitted says Google has already answered this session at least once: it
// holds cookies it was given, or tickets the port kept for it.
//
// What it is for is telling a session's first answer from the rest. A fresh
// session pays for a check to be let in at all, whatever pace it is asked at,
// so counting that one against the pace would read every run that opens
// sessions as one asking too fast — and the remedy for asking too fast, a
// longer rest, makes more fresh sessions rather than fewer.
func (h *Held) Admitted() bool {
	if h == nil || h.keeper == nil {
		return false
	}
	k := h.keeper
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.known[h.ID]
	return ok && hasAnswered(s.record)
}

// Moved says this session is going out from an address other than the one it
// last answered from.
//
// A session is moved when the address it answered on would not carry a request,
// or could not carry it past a check, and it arrives at the new one as a
// stranger: the same cookies from another part of the world are what a check is
// for. So a check met just after a move is the price of the address rather than
// a reading of how fast the session is being asked.
func (h *Held) Moved() bool {
	if h == nil || h.keeper == nil {
		return false
	}
	k := h.keeper
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.known[h.ID]
	return ok && s.moved
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
	s.spread = k.rand()
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
	// The handle is spent at once, and the session stays in hand until its
	// record says where it stands. Let go first, it was free while the line
	// was being written, with the moment it was last used from before and the
	// address it was leaving — and another thread took it and carried on the
	// query this one was ending.
	h.done = true
	exit := s.record.Exit
	k.mu.Unlock()
	defer k.release(s)
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
	s.spread = k.rand()
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
	// Spent at once and let go once its record says so, for the reason
	// elsewhere gives.
	h.done = true
	k.mu.Unlock()
	defer k.release(s)

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
	s.spread = k.rand()
	return false, nil
}

// Standing is how the sessions one caller can use stand at this moment: how
// many there are, how many are in somebody's hands, and how many of the rest
// are still resting.
//
// The three are what a screen needs to answer "is this run waiting on its own
// sessions". What is left over — neither working nor resting — is a session
// nobody took, and a run with none of those to spare is a run that will wait
// the moment a thread comes back for one.
func (k *Keeper) Standing(w Want) (all, held, resting int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	for _, s := range k.known {
		r := s.record
		if r.Device != w.Device || !w.matches(r.Browser, r.OS, r.Release) {
			continue
		}
		all++
		switch {
		case s.held:
			held++
		case !s.rested(w, now):
			resting++
		}
	}
	return all, held, resting
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

// SPDX-License-Identifier: MIT

package sessions

import (
	"context"
	"errors"
	"fmt"
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

// History is what the keeper needs from the history: the sessions, and a record
// of what became of each one.
type History interface {
	Sessions(ctx context.Context, device string, since time.Time) ([]store.Session, error)
	NewSession(ctx context.Context, s store.Session) (int64, error)
	SessionAnswered(ctx context.Context, id int64, cookies []byte, at time.Time) error
	SessionFailed(ctx context.Context, id int64, at time.Time) (bool, error)
	DropStaleSessions(ctx context.Context, before time.Time) (int, error)
}

// Wearer puts a session's fingerprint on a port, and reads the one a port has.
//
// It is the one place where a session meets a port, and the only thing about
// the keeper that depends on the proxy service. An empty profile is a new
// session: the port is taken as it is, and what it is wearing becomes the
// session's.
type Wearer interface {
	WearSession(ctx context.Context, port int, profile string) (string, error)
}

// Keeper hands sessions to threads and writes down what became of them.
//
// How many sessions there are is not a number anybody sets. A thread asks for
// one; if there is a session that nobody is holding and that has rested for the
// job's pause since it was last asked through, it gets that one, the one used
// most recently first; if there is none, a new one is made. So a job with a
// short pause and few threads settles on a few sessions, one with a long pause
// settles on more, and neither has to be worked out in advance — the count is
// wherever the pause and the number of threads put it.
type Keeper struct {
	history History
	wearer  Wearer
	device  string
	// pause is the job's gap between two requests on one identity. A session
	// is not handed out again until it has rested that long.
	pause time.Duration
	now   func() time.Time

	mu sync.Mutex
	// known are the sessions this keeper has read or made, by id. A session
	// given up is taken out of it.
	known map[int64]*kept
	// loaded says the history has been read. It is read once, on the first
	// Take, and after that the keeper is the one writing to it.
	loaded bool
}

// kept is one session as the keeper holds it between two uses.
type kept struct {
	record store.Session
	jar    *Jar
	// held says a thread has it now.
	held bool
	// ready is the earliest it may be handed out again.
	ready time.Time
}

// Held is a session a thread holds: which one, and the jar to search with.
type Held struct {
	ID      int64
	Profile string
	Jar     *Jar
	keeper  *Keeper
	done    bool
}

// ErrNotHeld is returned when a session is given back twice.
var ErrNotHeld = errors.New("sessions: the session is not held")

// NewKeeper returns a keeper for one kind of result page.
func NewKeeper(history History, wearer Wearer, device string, pause time.Duration) *Keeper {
	return &Keeper{
		history: history, wearer: wearer, device: device, pause: pause,
		now: time.Now, known: map[int64]*kept{},
	}
}

// Take hands a thread a session on the port it is holding: one that is free and
// has rested, or a new one.
//
// The session's fingerprint is put on the port before it is handed over, so the
// port and the jar the thread searches with are the same session. A session
// that cannot be put on the port is left for another thread rather than given
// up: a port that will not take a profile is the port's trouble, not the
// session's.
func (k *Keeper) Take(ctx context.Context, port int) (*Held, error) {
	if err := k.load(ctx); err != nil {
		return nil, err
	}
	if s := k.pick(); s != nil {
		profile, err := k.wearer.WearSession(ctx, port, s.record.Profile)
		if err != nil {
			k.putBack(s)
			return nil, fmt.Errorf("sessions: putting session %d on port %d: %w", s.record.ID, port, err)
		}
		if profile != "" && profile != s.record.Profile {
			// The service put something else on the port. The session is the
			// fingerprint and the cookies together, and cookies won under one
			// fingerprint sent under another are a session that disagrees with
			// itself — so this one is not used on this port.
			k.putBack(s)
			return nil, fmt.Errorf("sessions: port %d wears %q after being asked for %q",
				port, profile, s.record.Profile)
		}
		return &Held{ID: s.record.ID, Profile: s.record.Profile, Jar: s.jar, keeper: k}, nil
	}
	return k.fresh(ctx, port)
}

// pick takes the free, rested session used most recently, and marks it held.
func (k *Keeper) pick() *kept {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	var best *kept
	for _, s := range k.known {
		if s.held || s.ready.After(now) {
			continue
		}
		if best == nil || s.record.UsedAt.After(best.record.UsedAt) ||
			(s.record.UsedAt.Equal(best.record.UsedAt) && s.record.ID > best.record.ID) {
			best = s
		}
	}
	if best != nil {
		best.held = true
	}
	return best
}

// putBack lets go of a session without anything having been asked through it.
func (k *Keeper) putBack(s *kept) {
	k.mu.Lock()
	s.held = false
	k.mu.Unlock()
}

// fresh makes a new session on the port: the port as it is, with an empty jar,
// and whatever fingerprint the port is wearing written down as the session's.
func (k *Keeper) fresh(ctx context.Context, port int) (*Held, error) {
	profile, err := k.wearer.WearSession(ctx, port, "")
	if err != nil {
		return nil, fmt.Errorf("sessions: making port %d ready for a new session: %w", port, err)
	}
	if profile == "" {
		return nil, fmt.Errorf("sessions: port %d does not say which fingerprint it wears", port)
	}
	now := k.now()
	record := store.Session{Profile: profile, Device: k.device, CreatedAt: now, UsedAt: now}
	id, err := k.history.NewSession(ctx, record)
	if err != nil {
		return nil, err
	}
	record.ID = id
	jar := NewJar()
	jar.now = k.now
	k.mu.Lock()
	k.known[id] = &kept{record: record, jar: jar, held: true}
	k.mu.Unlock()
	return &Held{ID: id, Profile: profile, Jar: jar, keeper: k}, nil
}

// load reads the history once: the sessions of this kind of result page used in
// the last KeptFor, after the ones older than that are swept.
func (k *Keeper) load(ctx context.Context) error {
	k.mu.Lock()
	if k.loaded {
		k.mu.Unlock()
		return nil
	}
	k.mu.Unlock()

	now := k.now()
	if _, err := k.history.DropStaleSessions(ctx, now.Add(-KeptFor)); err != nil {
		return err
	}
	all, err := k.history.Sessions(ctx, k.device, now.Add(-KeptFor))
	if err != nil {
		return err
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.loaded {
		return nil
	}
	for _, one := range all {
		jar, err := ReadJar(one.Cookies)
		if err != nil {
			// A jar that cannot be read is a session that cannot be resumed.
			// It is left to the sweep rather than given up here: nothing about
			// it has been asked, and a history this program cannot read is a
			// fault to report, not to delete.
			continue
		}
		jar.now = k.now
		k.known[one.ID] = &kept{record: one, jar: jar, ready: one.UsedAt.Add(k.pause)}
	}
	k.loaded = true
	return nil
}

// Answered gives the session back after an answer: its cookies are written
// down as they are now, and it rests for the job's pause before it is handed
// out again.
func (h *Held) Answered(ctx context.Context) error {
	return h.keeper.answered(ctx, h)
}

// Save writes the session's cookies down after an answer and keeps holding it.
//
// It is for a query walked page by page. The walk keeps one session for all
// its pages — a visitor paging through results does not become someone else
// between page one and page two — so it cannot give the session back after
// every page; but a run can end between two pages, and a clearance won on page
// one and never written down is a clearance the next run pays for again.
func (h *Held) Save(ctx context.Context) error {
	return h.keeper.save(ctx, h)
}

func (k *Keeper) save(ctx context.Context, h *Held) error {
	k.mu.Lock()
	s, ok := k.known[h.ID]
	if h.done || !ok || !s.held {
		k.mu.Unlock()
		return ErrNotHeld
	}
	k.mu.Unlock()

	now := k.now()
	written, err := s.jar.MarshalJSON()
	if err != nil {
		return err
	}
	if err := k.history.SessionAnswered(ctx, s.record.ID, written, now); err != nil {
		return err
	}
	k.mu.Lock()
	s.record.UsedAt, s.record.Failures = now, 0
	k.mu.Unlock()
	return nil
}

// Failed gives the session back after a refusal. It says whether the session
// was given up — at the second refusal in a row.
func (h *Held) Failed(ctx context.Context) (bool, error) {
	return h.keeper.failed(ctx, h)
}

func (k *Keeper) answered(ctx context.Context, h *Held) error {
	s, err := k.release(h)
	if err != nil {
		return err
	}
	now := k.now()
	written, err := s.jar.MarshalJSON()
	if err != nil {
		return err
	}
	if err := k.history.SessionAnswered(ctx, s.record.ID, written, now); err != nil {
		return err
	}
	k.mu.Lock()
	s.record.UsedAt, s.record.Failures = now, 0
	s.ready = now.Add(k.pause)
	k.mu.Unlock()
	return nil
}

func (k *Keeper) failed(ctx context.Context, h *Held) (bool, error) {
	s, err := k.release(h)
	if err != nil {
		return false, err
	}
	now := k.now()
	dropped, err := k.history.SessionFailed(ctx, s.record.ID, now)
	if err != nil && !errors.Is(err, store.ErrNoSession) {
		return false, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if dropped || errors.Is(err, store.ErrNoSession) {
		delete(k.known, s.record.ID)
		return true, nil
	}
	s.record.UsedAt = now
	s.record.Failures++
	// A refusal is a request like any other as far as the pause is concerned:
	// the session was asked through, and asking it again at once is the same
	// push the pause is there to prevent.
	s.ready = now.Add(k.pause)
	return false, nil
}

// release marks a held session as given back, once.
func (k *Keeper) release(h *Held) (*kept, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if h.done {
		return nil, ErrNotHeld
	}
	s, ok := k.known[h.ID]
	if !ok || !s.held {
		return nil, ErrNotHeld
	}
	h.done = true
	s.held = false
	return s, nil
}

// Count is how many sessions the keeper knows, and how many of them a thread is
// holding now. It is what a screen reporting the run's sessions reads.
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

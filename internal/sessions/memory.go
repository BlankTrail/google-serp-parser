// SPDX-License-Identifier: MIT

package sessions

import (
	"context"
	"sync"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// Memory is a history kept in memory, by the same rules the store keeps: a
// session given up at the second refusal in a row, and swept when it has not
// been used for as long as the caller says.
//
// It is what a run with no history database keeps its sessions in — they last
// as long as the run — and what the tests of this package run against, so a
// keeper test is testing the keeper and not a second idea of when a session
// dies.
type Memory struct {
	mu       sync.Mutex
	next     int64
	sessions map[int64]store.Session
}

// NewMemory returns an empty history.
func NewMemory() *Memory { return &Memory{sessions: map[int64]store.Session{}} }

// Sessions are the sessions of one kind of result page used at or after since.
func (m *Memory) Sessions(_ context.Context, device string, since time.Time) ([]store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Session
	for _, s := range m.sessions {
		if s.Device == device && !s.UsedAt.Before(since) {
			out = append(out, s)
		}
	}
	return out, nil
}

// NewSession writes a session down and hands back its id.
func (m *Memory) NewSession(_ context.Context, s store.Session) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	s.ID = m.next
	m.sessions[s.ID] = s
	return s.ID, nil
}

// SessionAnswered records an answer: cookies, tickets and exit as they are
// now, and the refusals in a row back to nought.
func (m *Memory) SessionAnswered(_ context.Context, id int64, a store.Answer, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return store.ErrNoSession
	}
	s.Cookies, s.Tickets, s.Exit, s.UsedAt, s.Failures = a.Cookies, a.Tickets, a.Exit, at, 0
	m.sessions[id] = s
	return nil
}

// DropSession gives one session up by id, as the history does.
func (m *Memory) DropSession(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

// SessionFailed counts a refusal and gives the session up at the second in a
// row, and says whether it did.
func (m *Memory) SessionFailed(_ context.Context, id int64, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false, store.ErrNoSession
	}
	s.Failures++
	s.UsedAt = at
	if s.Failures >= store.SessionFailuresAllowed {
		delete(m.sessions, id)
		return true, nil
	}
	m.sessions[id] = s
	return false, nil
}

// DropStaleSessions gives up every session last used before the given moment.
func (m *Memory) DropStaleSessions(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if s.UsedAt.Before(before) {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}

// Get is one session as the history holds it now, for a test to look at.
func (m *Memory) Get(id int64) (store.Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

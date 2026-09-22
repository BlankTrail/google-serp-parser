// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessions_KeepWhatASessionIsAcrossTheWriteAndTheRead(t *testing.T) {
	// A session outlives the run it was made in — that is the reason for
	// writing it down — so what comes back has to be what went in: the
	// fingerprint it is put back on a port under, and the jar.
	s := testStore(t)
	ctx := context.Background()
	made := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	id, err := s.NewSession(ctx, Session{
		Profile: "Chrome_153_win", Browser: "chrome", OS: "windows", Device: "desktop",
		Cookies:   []byte(`[{"from":"https://www.google.ru/","name":"NID","value":"x"}]`),
		CreatedAt: made,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	got, err := s.Sessions(ctx, "desktop", made.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d sessions read back, want the one written", len(got))
	}
	one := got[0]
	if one.ID != id || one.Profile != "Chrome_153_win" || one.Browser != "chrome" || one.OS != "windows" {
		t.Errorf("the session read back as %+v", one)
	}
	if string(one.Cookies) != `[{"from":"https://www.google.ru/","name":"NID","value":"x"}]` {
		t.Errorf("the jar read back as %s", one.Cookies)
	}
	if !one.UsedAt.Equal(made) {
		t.Errorf("a new session was last used at %v, want the moment it was made", one.UsedAt)
	}
}

func TestSessions_HandsAPhoneJobOnlyPhoneSessions(t *testing.T) {
	// A phone's cookies are a phone's. Google answers the two kinds of page
	// differently, and a session that has only ever been a desktop handed to a
	// phone job is a phone that has been a desktop all along.
	s := testStore(t)
	ctx := context.Background()
	for _, device := range []string{"desktop", "mobile", "desktop"} {
		if _, err := s.NewSession(ctx, Session{Profile: "p", Device: device}); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
	}
	phones, err := s.Sessions(ctx, "mobile", time.Time{})
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(phones) != 1 {
		t.Errorf("a phone job was offered %d sessions, want the one phone", len(phones))
	}
}

func TestSessions_OffersTheOneUsedLastFirst(t *testing.T) {
	// The session that answered a minute ago is warmer than one that answered
	// eleven hours ago, and a run taking the first free one should be taking
	// the warm one.
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	old, _ := s.NewSession(ctx, Session{Profile: "old", Device: "desktop", CreatedAt: base})
	warm, _ := s.NewSession(ctx, Session{Profile: "warm", Device: "desktop", CreatedAt: base})
	if err := s.SessionAnswered(ctx, warm, Answer{}, base.Add(11*time.Hour)); err != nil {
		t.Fatalf("SessionAnswered: %v", err)
	}

	got, err := s.Sessions(ctx, "desktop", time.Time{})
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(got) != 2 || got[0].ID != warm || got[1].ID != old {
		t.Errorf("sessions offered in the order %v, want the warm one first", ids(got))
	}
}

func TestSessions_GivesASessionUpAtTheSecondRefusalInARow(t *testing.T) {
	// One refusal is the address under the session as often as it is the
	// session. Two in a row, across what is usually two addresses, is the
	// session — and an answer in between means the count starts again.
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	id, _ := s.NewSession(ctx, Session{Profile: "p", Device: "desktop", CreatedAt: now})

	if dropped, err := s.SessionFailed(ctx, id, now); err != nil || dropped {
		t.Fatalf("the first refusal: dropped=%v err=%v, want the session kept", dropped, err)
	}
	// An answer in between: the refusal before it was the address.
	if err := s.SessionAnswered(ctx, id, Answer{}, now.Add(time.Minute)); err != nil {
		t.Fatalf("SessionAnswered: %v", err)
	}
	if dropped, err := s.SessionFailed(ctx, id, now.Add(2*time.Minute)); err != nil || dropped {
		t.Fatalf("a refusal after an answer: dropped=%v err=%v, want the count started again", dropped, err)
	}
	dropped, err := s.SessionFailed(ctx, id, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("SessionFailed: %v", err)
	}
	if !dropped {
		t.Error("the second refusal in a row left the session standing")
	}
	if got, _ := s.Sessions(ctx, "desktop", time.Time{}); len(got) != 0 {
		t.Errorf("a given-up session is still offered: %v", ids(got))
	}
	// And a session that is gone is said to be gone.
	if _, err := s.SessionFailed(ctx, id, now); !errors.Is(err, ErrNoSession) {
		t.Errorf("failing a session that is gone answered %v, want ErrNoSession", err)
	}
}

func TestSessions_ForgetsTheOnesNobodyHasUsedForTwelveHours(t *testing.T) {
	// The sweep is the caller's to time; what this checks is that it takes
	// exactly the sessions last used before the line and leaves the rest.
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	stale, _ := s.NewSession(ctx, Session{Profile: "stale", Device: "desktop", CreatedAt: now.Add(-13 * time.Hour)})
	fresh, _ := s.NewSession(ctx, Session{Profile: "fresh", Device: "desktop", CreatedAt: now.Add(-11 * time.Hour)})

	n, err := s.DropStaleSessions(ctx, now.Add(-12*time.Hour))
	if err != nil {
		t.Fatalf("DropStaleSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("%d sessions were swept, want the one past twelve hours", n)
	}
	got, _ := s.Sessions(ctx, "desktop", time.Time{})
	if len(got) != 1 || got[0].ID != fresh {
		t.Errorf("after the sweep the sessions are %v, want only %d (and not %d)", ids(got), fresh, stale)
	}
}

func ids(all []Session) []int64 {
	out := make([]int64, 0, len(all))
	for _, one := range all {
		out = append(out, one.ID)
	}
	return out
}

func TestSessions_KeepWhereTheyGoOutTheirTicketsAndTheirRelease(t *testing.T) {
	// A session is put back on a port the way it left one: on its own exit,
	// resuming TLS with its own tickets, wearing a fingerprint of the release a
	// job asked for. All three have to come back from the history as they went
	// in — and an answer has to move all three at once, because a request can
	// carry a session to another address on its way.
	s := testStore(t)
	ctx := context.Background()
	made := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	id, err := s.NewSession(ctx, Session{
		Profile: "Chrome_153_win", Device: "desktop", Release: 153,
		Exit:    "addr:socks5://user-session-1:pw@gw.example:1080",
		Tickets: []byte(`[{"host":"www.google.ru","tickets":[]}]`), CreatedAt: made,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	one := onlySession(t, s)
	if one.ID != id || one.Release != 153 || one.Exit != "addr:socks5://user-session-1:pw@gw.example:1080" ||
		string(one.Tickets) != `[{"host":"www.google.ru","tickets":[]}]` {
		t.Fatalf("the session read back as %+v", one)
	}

	moved := Answer{Cookies: []byte(`[]`), Tickets: []byte(`[{"host":"www.google.com","tickets":[]}]`),
		Exit: "addr:socks5://user-session-2:pw@gw.example:1080"}
	if err := s.SessionAnswered(ctx, id, moved, made.Add(time.Minute)); err != nil {
		t.Fatalf("SessionAnswered: %v", err)
	}
	one = onlySession(t, s)
	if one.Exit != moved.Exit || string(one.Tickets) != string(moved.Tickets) {
		t.Errorf("after the answer the session is at %q with %s, want %q with %s",
			one.Exit, one.Tickets, moved.Exit, moved.Tickets)
	}
}

// onlySession reads back the one desktop session a test wrote.
func onlySession(t *testing.T, s *Store) Session {
	t.Helper()
	got, err := s.Sessions(context.Background(), "desktop", time.Time{})
	if err != nil || len(got) != 1 {
		t.Fatalf("Sessions: %d sessions, err %v; want exactly one", len(got), err)
	}
	return got[0]
}

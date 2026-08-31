// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateKey_HandsBackASecretTheDatabaseDoesNotHold(t *testing.T) {
	// The file sits on a disk, goes into backups and travels with them. A key
	// readable out of it is a key that leaks with the file.
	s := testStore(t)
	secret, key, err := s.CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if len(secret) < 32 {
		t.Errorf("the secret is %d characters, too few to be worth guessing at", len(secret))
	}

	var name, prefix, hash string
	if err := s.db.QueryRow(
		`SELECT name, prefix, hash FROM api_keys WHERE id = ?`, key.ID).
		Scan(&name, &prefix, &hash); err != nil {
		t.Fatalf("reading the row back: %v", err)
	}
	for _, stored := range []struct{ column, value string }{
		{"name", name}, {"prefix", prefix}, {"hash", hash},
	} {
		if strings.Contains(stored.value, secret) {
			t.Errorf("the %s column carries the secret itself", stored.column)
		}
	}
	if name != "nightly" {
		t.Errorf("the key was stored under the name %q, want %q", name, "nightly")
	}
	if prefix != key.Prefix {
		t.Errorf("the stored prefix is %q, but %q was handed back", prefix, key.Prefix)
	}
	if key.Prefix == "" || !strings.HasPrefix(secret, key.Prefix) {
		t.Errorf("prefix %q is not the start of the secret it belongs to", key.Prefix)
	}
	if len(key.Prefix) >= len(secret) {
		t.Errorf("the prefix is %d characters of %d, which is the whole key",
			len(key.Prefix), len(secret))
	}
}

func TestCheckKey_AcceptsTheSecretItIssued(t *testing.T) {
	s := testStore(t)
	secret, made, err := s.CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	got, err := s.CheckKey(context.Background(), secret)
	if err != nil {
		t.Fatalf("CheckKey: %v", err)
	}
	if got.ID != made.ID || got.Name != "nightly" || got.Prefix != made.Prefix {
		t.Errorf("CheckKey returned %+v, want the key just made, %+v", got, made)
	}
}

func TestCheckKey_RefusesASecretItNeverIssued(t *testing.T) {
	// A key that works is in the fixture, so an answer of "no" has to come from
	// the lookup rather than from an empty table.
	s := testStore(t)
	if _, _, err := s.CreateKey(context.Background(), "nightly"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	other, _, err := s.CreateKey(context.Background(), "spare")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	// A secret of the right shape, differing from a real one in its last
	// character, so nothing but the value itself can tell them apart.
	forged := other[:len(other)-1] + string(rune('a'+(other[len(other)-1]-'a'+1)%26))

	if _, err := s.CheckKey(context.Background(), forged); !errors.Is(err, ErrBadKey) {
		t.Errorf("CheckKey returned %v for a secret nobody issued, want ErrBadKey", err)
	}
	if _, err := s.CheckKey(context.Background(), "not-a-key-at-all"); !errors.Is(err, ErrBadKey) {
		t.Errorf("CheckKey returned %v for nonsense, want ErrBadKey", err)
	}
}

func TestRevokeKey_StopsOneKeyAndLeavesTheRestWorking(t *testing.T) {
	// Revoking has to take effect at once: a key handed to somebody who has left
	// is revoked precisely because it must stop working now. It must also stop
	// exactly one key, since the way to find out otherwise is that every caller
	// is locked out at the same moment.
	s := testStore(t)
	doomed, key, err := s.CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	spared, other, err := s.CreateKey(context.Background(), "hourly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	if err := s.RevokeKey(context.Background(), key.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := s.CheckKey(context.Background(), doomed); !errors.Is(err, ErrBadKey) {
		t.Errorf("a revoked key still works: %v", err)
	}
	got, err := s.CheckKey(context.Background(), spared)
	if err != nil {
		t.Fatalf("revoking one key stopped another: %v", err)
	}
	if got.ID != other.ID {
		t.Errorf("CheckKey returned key %d, want %d", got.ID, other.ID)
	}
}

func TestRevokeKey_SaysSoWhenNoSuchKeyExists(t *testing.T) {
	// Somebody revoking a key after a leak needs to hear that nothing happened,
	// rather than walk away believing a key that still works has been stopped.
	s := testStore(t)
	_, key, err := s.CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := s.RevokeKey(context.Background(), key.ID+1); !errors.Is(err, ErrBadKey) {
		t.Errorf("RevokeKey on an id nothing was stored under returned %v, want ErrBadKey", err)
	}
}

func TestCheckKey_AnswersAnEmptySecretWithoutAskingTheDatabase(t *testing.T) {
	// A request carrying no key at all is the most common request there is, and
	// answering it costs no lookup. The closed store is the proof: an answer that
	// arrives anyway is an answer that never went to the database.
	s := testStore(t)
	if _, _, err := s.CreateKey(context.Background(), "nightly"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.CheckKey(context.Background(), ""); !errors.Is(err, ErrBadKey) {
		t.Errorf("CheckKey(\"\") returned %v, want ErrBadKey", err)
	}
}

func TestCheckKey_RefusesAnEmptySecretEvenWhenARowCarriesItsHash(t *testing.T) {
	// Nothing that finds its way into the table may turn "no key" into a key.
	// The row here is exactly what a bug elsewhere would leave behind, and the
	// empty secret must not match it.
	s := testStore(t)
	if _, err := s.db.Exec(
		`INSERT INTO api_keys(name, prefix, hash, created_at)
		 VALUES('planted', '', ?, '2026-08-16T10:00:00Z')`, hashSecret("")); err != nil {
		t.Fatalf("planting the row: %v", err)
	}
	if _, err := s.CheckKey(context.Background(), ""); !errors.Is(err, ErrBadKey) {
		t.Errorf("CheckKey(\"\") returned %v, want ErrBadKey", err)
	}
}

func TestCreateKey_NeverIssuesTheSameSecretTwice(t *testing.T) {
	s := testStore(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		secret, _, err := s.CreateKey(context.Background(), "k")
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if seen[secret] {
			t.Fatalf("the same secret came back twice after %d keys", i)
		}
		seen[secret] = true
	}
}

func TestCreateKey_DrawsASecretRatherThanCountingOnFromTheLast(t *testing.T) {
	// Distinct is not enough: a counter is distinct too, and every key it issues
	// gives away every key beside it. Two secrets drawn at random differ nearly
	// everywhere, a counter differs in its last character or two.
	s := testStore(t)
	first, _, err := s.CreateKey(context.Background(), "one")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	second, _, err := s.CreateKey(context.Background(), "two")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("secrets are %d and %d characters long, want one length", len(first), len(second))
	}
	differ := 0
	for i := range first {
		if first[i] != second[i] {
			differ++
		}
	}
	if differ*2 < len(first) {
		t.Errorf("two secrets differ in %d of %d characters, which is a sequence rather than a draw",
			differ, len(first))
	}
}

func TestKeys_ListsWhatExistsAndNothingAnyoneCouldUse(t *testing.T) {
	// The listing is what a person reads to find their own key. It has to carry
	// enough to recognise one and too little to be one.
	s := testStore(t)
	secrets := map[string]string{}
	for _, name := range []string{"one", "two"} {
		secret, _, err := s.CreateKey(context.Background(), name)
		if err != nil {
			t.Fatalf("CreateKey(%q): %v", name, err)
		}
		secrets[name] = secret
	}

	keys, err := s.Keys(context.Background())
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != len(secrets) {
		t.Fatalf("%d keys listed, want %d", len(keys), len(secrets))
	}
	for _, k := range keys {
		secret, ok := secrets[k.Name]
		if !ok {
			t.Errorf("the listing names a key %q that was never made", k.Name)
			continue
		}
		if k.Prefix != secret[:prefixLen] {
			t.Errorf("key %q is listed under %q, want the first %d characters of its secret",
				k.Name, k.Prefix, prefixLen)
		}
		if len(k.Prefix) >= len(secret) {
			t.Errorf("key %q is listed with %d characters of a %d character secret",
				k.Name, len(k.Prefix), len(secret))
		}
		if _, err := s.CheckKey(context.Background(), k.Prefix); !errors.Is(err, ErrBadKey) {
			t.Errorf("what the listing shows for key %q works as a key: %v", k.Name, err)
		}
		if k.CreatedAt.IsZero() {
			t.Errorf("key %q is listed without a date", k.Name)
		}
	}
}

func TestKeys_SaysWhichKeysNoLongerWork(t *testing.T) {
	// A revoked key stays in the listing so a person can see it was stopped
	// rather than wonder where it went.
	s := testStore(t)
	_, revoked, err := s.CreateKey(context.Background(), "gone")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, _, err := s.CreateKey(context.Background(), "live"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := s.RevokeKey(context.Background(), revoked.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	keys, err := s.Keys(context.Background())
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	want := map[string]bool{"gone": true, "live": false}
	if len(keys) != len(want) {
		t.Fatalf("%d keys listed, want %d", len(keys), len(want))
	}
	for _, k := range keys {
		if k.Revoked != want[k.Name] {
			t.Errorf("key %q is listed as revoked=%v, want %v", k.Name, k.Revoked, want[k.Name])
		}
	}
}

func TestCheckKey_RecordsThatTheKeyWasUsedAndOnlyThatKey(t *testing.T) {
	// Somebody auditing a leak needs to know which key has been used and when.
	// The second key is never used, and a listing that stamps it too says
	// nothing about anything.
	s := testStore(t)
	secret, used, err := s.CreateKey(context.Background(), "used")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, _, err := s.CreateKey(context.Background(), "idle"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	before := time.Now().UTC().Add(-time.Second)
	if _, err := s.CheckKey(context.Background(), secret); err != nil {
		t.Fatalf("CheckKey: %v", err)
	}

	keys, err := s.Keys(context.Background())
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	var found bool
	for _, k := range keys {
		switch k.Name {
		case "used":
			found = true
			if k.ID != used.ID {
				t.Errorf("the used key is listed as %d, want %d", k.ID, used.ID)
			}
			if k.LastUsedAt.IsZero() {
				t.Error("the key carries no record of having been used")
			} else if k.LastUsedAt.Before(before) {
				t.Errorf("the key was last used at %v, before it was checked", k.LastUsedAt)
			}
		case "idle":
			if !k.LastUsedAt.IsZero() {
				t.Errorf("a key nobody used is stamped %v", k.LastUsedAt)
			}
		default:
			t.Errorf("the listing names a key %q that was never made", k.Name)
		}
	}
	if !found {
		t.Fatal("the key that was checked is missing from the listing")
	}
}

func TestCheckKey_AnswersEvenWhenTheRecordOfUseCannotBeWritten(t *testing.T) {
	// The caller is entitled to their answer. The note is for whoever audits
	// later, and losing it is not a reason to refuse a key that is good.
	s := testStore(t)
	secret, key, err := s.CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := s.db.Exec(
		`CREATE TRIGGER refuse_the_note BEFORE UPDATE OF last_used_at ON api_keys
		 BEGIN SELECT RAISE(ABORT, 'the note cannot be written'); END`); err != nil {
		t.Fatalf("standing in the way of the note: %v", err)
	}
	// Without this the test would hold whether the note failed or not.
	if _, err := s.db.Exec(
		`UPDATE api_keys SET last_used_at = '2026-08-16T10:00:00Z' WHERE id = ?`, key.ID); err == nil {
		t.Fatal("the note can still be written, so this test proves nothing")
	}

	got, err := s.CheckKey(context.Background(), secret)
	if err != nil {
		t.Fatalf("a valid key was refused because its use could not be noted: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("CheckKey returned key %d, want %d", got.ID, key.ID)
	}
}

func TestOpen_LetsADatabaseWrittenBeforeKeysExistedIssueThem(t *testing.T) {
	// An upgrade that loses a user's history is worse than no upgrade, and one
	// that leaves the new functions with nothing to write to is no upgrade at
	// all. Both are checked on the same file.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := jobWith(t, s, "a")
	mustRecord(t, s, id, 0, "one.test")
	windBackToVersionOne(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a version-1 database: %v", err)
	}
	defer func() { _ = again.Close() }()

	jobs, err := again.Jobs(context.Background(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("%d jobs survived the upgrade, want 1", len(jobs))
	}

	secret, key, err := again.CreateKey(context.Background(), "after")
	if err != nil {
		t.Fatalf("the upgraded database cannot hold a key: %v", err)
	}
	got, err := again.CheckKey(context.Background(), secret)
	if err != nil {
		t.Fatalf("the upgraded database cannot check a key it issued: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("CheckKey returned key %d, want %d", got.ID, key.ID)
	}
}

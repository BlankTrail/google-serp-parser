// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/store"
)

// keysAt opens a history of the test's own, so that what the command did can be
// held against the database rather than against what the command said it did.
func keysAt(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// hexRun returns the longest field of hex digits in the text. A secret has no
// other shape, and reading it back out of the output is how a test tells a key
// printed whole from one printed short.
func hexRun(text string) string {
	longest := ""
	for _, f := range strings.Fields(text) {
		if len(f) <= len(longest) {
			continue
		}
		if _, err := hex.DecodeString(f); err != nil {
			continue
		}
		longest = f
	}
	return longest
}

// lineWith returns the one line of the text carrying the needle.
func lineWith(t *testing.T, text, needle string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("nothing in the output carries %q:\n%s", needle, text)
	return ""
}

func TestKeyNew_PrintsTheWholeSecretAndWarnsInTheSameBreath(t *testing.T) {
	// Somebody who does not save the secret finds out at their first request,
	// and the only way out is another key. A line carrying the key without the
	// warning is a line that gets scrolled past.
	db := filepath.Join(t.TempDir(), "keys.db")
	var out bytes.Buffer
	if err := keyCommand(context.Background(), []string{"new", "-db", db, "-name", "nightly"}, &out); err != nil {
		t.Fatalf("key new: %v", err)
	}

	secret := hexRun(out.String())
	if len(secret) < 32 {
		t.Fatalf("nothing printed is long enough to be a key:\n%s", out.String())
	}
	// The store is the judge of whether the whole key was printed: a key short
	// of a character is unusable and cannot be recovered.
	if _, err := keysAt(t, db).CheckKey(context.Background(), secret); err != nil {
		t.Fatalf("the secret as printed is not one this history accepts: %v", err)
	}

	line := lineWith(t, out.String(), secret)
	for _, want := range []string{"once", "cannot be read back"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line carrying the key does not say %q:\n%s", want, line)
		}
	}
}

func TestKeyNew_PutsTheSecretOnTheWriterItWasHandedAndNowhereElse(t *testing.T) {
	// The secret is printed on purpose, to whoever asked for it. A log is kept,
	// rotated and shipped somewhere else, so a key that reaches one has leaked
	// to everyone who reads that.
	dir := t.TempDir()
	var logged bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	errFile, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stderr := os.Stderr
	os.Stderr = errFile
	t.Cleanup(func() { os.Stderr = stderr; _ = errFile.Close() })

	var out bytes.Buffer
	if err := keyCommand(context.Background(),
		[]string{"new", "-db", filepath.Join(dir, "keys.db"), "-name", "nightly"}, &out); err != nil {
		t.Fatalf("key new: %v", err)
	}
	secret := hexRun(out.String())
	if len(secret) < 32 {
		t.Fatalf("nothing printed is long enough to be a key:\n%s", out.String())
	}

	if logged.Len() != 0 {
		t.Errorf("the command logged, and it has nothing to log:\n%s", logged.String())
	}
	aside, err := os.ReadFile(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for what, text := range map[string]string{"log": logged.String(), "standard error": string(aside)} {
		if strings.Contains(text, secret) {
			t.Errorf("the key reached the %s:\n%s", what, text)
		}
	}
}

func TestKeyList_NamesEveryKeyAndHandsOutNothingUsable(t *testing.T) {
	// The listing exists so a person can find the key they mean to revoke. The
	// prefix does that; the secret and the stored hash do that and more.
	db := filepath.Join(t.TempDir(), "keys.db")
	secret, key, err := keysAt(t, db).CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	var out bytes.Buffer
	if err := keyCommand(context.Background(), []string{"list", "-db", db}, &out); err != nil {
		t.Fatalf("key list: %v", err)
	}
	got := out.String()

	if strings.Contains(got, secret) {
		t.Errorf("the listing hands out the key itself:\n%s", got)
	}
	hash := sha256.Sum256([]byte(secret))
	if strings.Contains(got, hex.EncodeToString(hash[:])) {
		t.Errorf("the listing hands out what the database compares against:\n%s", got)
	}
	if !strings.Contains(got, key.Prefix) {
		t.Errorf("the listing does not carry the prefix %q, so nothing in it names a key:\n%s", key.Prefix, got)
	}
	if !strings.Contains(got, "nightly") {
		t.Errorf("the listing does not carry the name the key was filed under:\n%s", got)
	}
	if !strings.Contains(got, strconv.FormatInt(key.ID, 10)) {
		t.Errorf("the listing does not carry the id revoking one takes:\n%s", got)
	}
}

func TestKeyRevoke_LeavesTheKeyRefusedByTheHistoryItself(t *testing.T) {
	// A command that prints "revoked" and revokes nothing satisfies any check
	// made on its output, so the history is asked instead.
	db := filepath.Join(t.TempDir(), "keys.db")
	s := keysAt(t, db)
	secret, key, err := s.CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := s.CheckKey(context.Background(), secret); err != nil {
		t.Fatalf("the key was refused before anything revoked it: %v", err)
	}

	var out bytes.Buffer
	if err := keyCommand(context.Background(),
		[]string{"revoke", "-db", db, "-id", strconv.FormatInt(key.ID, 10)}, &out); err != nil {
		t.Fatalf("key revoke: %v", err)
	}
	if _, err := s.CheckKey(context.Background(), secret); !errors.Is(err, store.ErrBadKey) {
		t.Errorf("the history still accepts the key, and CheckKey returned %v", err)
	}
}

func TestKeyRevoke_RefusesWithoutAnIdInsteadOfChoosingOne(t *testing.T) {
	db := filepath.Join(t.TempDir(), "keys.db")
	s := keysAt(t, db)
	secret, _, err := s.CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	var out bytes.Buffer
	err = keyCommand(context.Background(), []string{"revoke", "-db", db}, &out)
	if err == nil {
		t.Fatalf("revoking nothing in particular was accepted:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "-id") {
		t.Errorf("the refusal does not name the flag that was missing: %v", err)
	}
	if _, err := s.CheckKey(context.Background(), secret); err != nil {
		t.Errorf("a revoke that named no key stopped one anyway: %v", err)
	}
}

func TestKeyRevoke_SaysNothingWasStoppedWhenNoSuchKeyExists(t *testing.T) {
	// Whoever is revoking after a leak reads this and stops looking. Silence
	// here sends them away believing a working key has been stopped.
	db := filepath.Join(t.TempDir(), "keys.db")
	_, key, err := keysAt(t, db).CreateKey(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	var out bytes.Buffer
	err = keyCommand(context.Background(),
		[]string{"revoke", "-db", db, "-id", strconv.FormatInt(key.ID+1, 10)}, &out)
	if !errors.Is(err, store.ErrBadKey) {
		t.Fatalf("revoking a key that was never issued returned %v, want ErrBadKey:\n%s", err, out.String())
	}
}

func TestKeyCommand_NamesTheThreeThingsItDoesWhenAskedForNone(t *testing.T) {
	var out bytes.Buffer
	err := keyCommand(context.Background(), nil, &out)
	if err == nil {
		t.Fatalf("a key command with nothing to do was accepted:\n%s", out.String())
	}
	for _, want := range []string{"new", "list", "revoke"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// helpSection returns the block of the help under a heading. The flags are
// looked for inside it rather than anywhere in the help, because --db and
// --name are named by other commands' sections, and either would stand in for
// a section that never mentions them.
func helpSection(t *testing.T, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(usageText, heading+"\n")
	if !ok {
		t.Fatalf("the help has no %q section", heading)
	}
	block, _, _ := strings.Cut(rest, "\n\n")
	return block
}

func TestUsage_ListsEveryFlagTheKeyCommandsTake(t *testing.T) {
	// Help that has drifted from the flags is the same defect as documentation
	// that is wrong: it is read instead of the code, and it is believed.
	flags := helpSection(t, "Key flags:")
	commands := helpSection(t, "Usage:")

	for _, action := range []string{"new", "list", "revoke"} {
		var opts keyOptions
		fs := keyFlags(action, &opts)

		if !strings.Contains(commands, "gserp key "+action) {
			t.Errorf("the help does not list the key %s command", action)
		}
		fs.VisitAll(func(f *flag.Flag) {
			if !strings.Contains(flags, "--"+f.Name) {
				t.Errorf("the key flags do not mention --%s, which key %s takes", f.Name, action)
			}
		})
	}
}

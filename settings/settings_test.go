// SPDX-License-Identifier: MIT

package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// asJSON is how the redaction tests look at the whole value rather than at the
// one field the author of the test happened to remember.
func asJSON(t *testing.T, s Settings) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("writing the settings out as JSON: %v", err)
	}
	return string(raw)
}

func TestSave_AndLoadBringBackWhatWasPutIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	want := Settings{
		ControlURL: "http://127.0.0.1:1", APIKey: "a-secret",
		HotPorts: 3, HotDevice: "desktop",
		Proxy:    ProxySource{Kind: "url", Location: "https://example.test/list", Refresh: time.Hour},
		Language: "ru",
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("came back as %+v, want %+v", got, want)
	}
}

func TestLoad_TakesAMissingFileToMeanNothingHasBeenSetYet(t *testing.T) {
	// A fresh install has no file, and that is not a fault: it is somebody who
	// has not opened the settings yet.
	got, err := Load(filepath.Join(t.TempDir(), "nothing.json"))
	if err != nil {
		t.Fatalf("Load of a file that is not there: %v", err)
	}
	if !reflect.DeepEqual(got, Defaults()) {
		t.Errorf("came back as %+v, want the defaults", got)
	}
}

func TestLoad_RefusesAFileItCannotReadRatherThanPretendingItIsEmpty(t *testing.T) {
	// Handing back defaults for a file that exists and is damaged sends the
	// program somewhere its owner did not tell it to go — at somebody else's
	// address, with somebody else's key, or with no key at all.
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{ this is not settings"), 0o600); err != nil {
		t.Fatalf("writing the damaged file: %v", err)
	}
	if _, err := Load(path); !errors.Is(err, ErrUnreadable) {
		t.Errorf("Load returned %v, want ErrUnreadable", err)
	}
}

func TestSave_LeavesTheFileReadableOnlyByItsOwner(t *testing.T) {
	// The key is in it. A file the whole machine can read is a key the whole
	// machine has.
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not how Windows decides this; the ACL is the owner's own by default")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := Save(path, Defaults()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode is %04o, want 0600", mode)
	}
}

func TestSave_LeavesTheOldSettingsIntactWhenItCannotFinish(t *testing.T) {
	// A save cut off half way must not leave somebody with a file that is
	// neither the old settings nor the new ones. Writing beside and renaming
	// is what makes the swap one step.
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	first := Defaults()
	first.APIKey = "the-old-one"
	if err := Save(path, first); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A directory where the temporary file wants to be cannot be written over,
	// so the second save fails after the first has landed.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatalf("blocking the temporary name: %v", err)
	}
	second := first
	second.APIKey = "the-new-one"
	if err := Save(path, second); err == nil {
		t.Fatal("Save succeeded with its temporary name taken")
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after the failed save: %v", err)
	}
	if got.APIKey != "the-old-one" {
		t.Errorf("the key is now %q — the failed save damaged what was there", got.APIKey)
	}
}

func TestRedacted_CarriesNoKeyInAnyField(t *testing.T) {
	// This is what goes to the browser and into the log. A key that reaches
	// either of them is a key in somebody's terminal scrollback.
	s := Defaults()
	s.APIKey = "0123456789abcdef0123456789abcdef"
	got := asJSON(t, s.Redacted())
	if strings.Contains(got, "0123456789abcdef") {
		t.Errorf("the redacted form still carries the key:\n%s", got)
	}
	if !strings.Contains(got, s.APIKey[len(s.APIKey)-4:]) {
		t.Error("the redacted form carries no tail, so nobody can tell which key is set")
	}
}

func TestRedacted_SaysNothingWhereNoKeyIsSet(t *testing.T) {
	// A tail invented for an empty key reads as a key that is set.
	got := asJSON(t, Defaults().Redacted())
	if strings.Contains(got, "…") || strings.Contains(got, "***") {
		t.Errorf("an unset key is shown as though something were there:\n%s", got)
	}
}

func TestRedacted_LeavesEverythingElseAlone(t *testing.T) {
	s := Defaults()
	s.APIKey = "secret"
	s.ControlURL = "http://127.0.0.1:1"
	s.HotPorts = 12
	s.HotDevice = "mobile"
	got := s.Redacted()
	if got.ControlURL != s.ControlURL || got.HotPorts != s.HotPorts || got.HotDevice != s.HotDevice {
		t.Errorf("redacting changed something other than the key: %+v", got)
	}
}

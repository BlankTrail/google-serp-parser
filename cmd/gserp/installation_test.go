// SPDX-License-Identifier: MIT

package main

import (
	"path/filepath"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
)

func TestNameInstallation_NamesEachHistoryApartAndTheSameOneAlike(t *testing.T) {
	t.Cleanup(func() { blanktrail.SetOwner("") })
	dir := t.TempDir()
	a := nameInstallation(filepath.Join(dir, "a", "gserp.db"))
	if again := nameInstallation(filepath.Join(dir, "a", "gserp.db")); again != a {
		t.Errorf("the same history is named %q and then %q", a, again)
	}
	if b := nameInstallation(filepath.Join(dir, "b", "gserp.db")); b == a {
		t.Errorf("two histories are both named %q: one parser would take the other's ports for its leftovers", a)
	}
}

func TestNameInstallation_IsWhatTheNextClientLabelsItsPortsWith(t *testing.T) {
	t.Cleanup(func() { blanktrail.SetOwner("") })
	name := nameInstallation(filepath.Join(t.TempDir(), "gserp.db"))
	c, err := blanktrail.NewClient("http://127.0.0.1:1", "key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.Label(); len(got) <= len("gserp-"+name+"-") || got[:len("gserp-"+name+"-")] != "gserp-"+name+"-" {
		t.Errorf("a client labels its ports %q, want the installation %q in it", got, name)
	}
}

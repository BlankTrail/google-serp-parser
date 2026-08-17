// SPDX-License-Identifier: MIT

package web

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/settings"
)

// listTree is a directory holding one of each thing the chooser has an opinion
// about: a folder to walk into, a file worth choosing, and a file that is not a
// list. All three are needed — a tree of only lists cannot tell a chooser that
// offers everything from one that offers lists.
func listTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "inner"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, name := range []string{"proxies.txt", "more.LIST", "notes.pdf", "gserp.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	return dir
}

func browseAtDir(t *testing.T, s *Server, dir string) string {
	t.Helper()
	return get(t, s, browseAt+"?at="+dir).Body.String()
}

func TestBrowse_OffersTheListsAndTheWayDeeperAndNothingElse(t *testing.T) {
	// A chooser is for finding one file among many. A listing that puts every
	// binary on the machine beside it is one nobody can use, and it is also a way
	// of learning what a machine holds — which this page has no business helping
	// with.
	s, _ := serverWithSettings(t, settings.Settings{})
	body := browseAtDir(t, s, listTree(t))

	for _, want := range []string{"inner", "proxies.txt", "more.LIST"} {
		if !strings.Contains(body, want) {
			t.Errorf("the listing does not offer %q:\n%s", want, body)
		}
	}
	for _, gone := range []string{"notes.pdf", "gserp.exe"} {
		if strings.Contains(body, gone) {
			t.Errorf("the listing offers %q, which is not a list of addresses:\n%s", gone, body)
		}
	}
}

func TestBrowse_HandsAChosenFileToTheBoxWithoutSavingIt(t *testing.T) {
	// Choosing is not saving. The reader sees what they picked standing where
	// they would have typed it, and what is on disk changes when they press save
	// — the same rule the key box already follows.
	saved := settings.Settings{ControlURL: "http://127.0.0.1:1"}
	s, path := serverWithSettings(t, saved)
	chosen := filepath.Join(listTree(t), "proxies.txt")

	body := get(t, s, settingsAt+"?"+whereField+"="+chosen).Body.String()
	if !strings.Contains(body, chosen) {
		t.Errorf("the box does not hold the file that was chosen:\n%s", body)
	}

	after, err := settings.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if after.Proxy.Location != "" {
		t.Errorf("choosing a file wrote %q to the settings", after.Proxy.Location)
	}
}

func TestBrowse_OpensWhereTheSettingsAlreadyPointRatherThanSomewhereToWalkBackFrom(t *testing.T) {
	// Somebody changing a list they set up last month should not have to walk
	// there from their home directory again.
	dir := listTree(t)
	s, _ := serverWithSettings(t, settings.Settings{
		Proxy: settings.ProxySource{Kind: "file", Location: filepath.Join(dir, "proxies.txt")},
	})

	body := get(t, s, browseAt).Body.String()
	if !strings.Contains(body, dir) {
		t.Errorf("the chooser did not open in the folder the settings name:\n%s", body)
	}
}

func TestBrowse_SaysSoWhenAFolderCannotBeReadRatherThanFallingOver(t *testing.T) {
	// A folder this machine will not open is not a fault in the program, and it
	// is not a reason for the page to break: the reader has to be able to walk
	// back out of it.
	s, _ := serverWithSettings(t, settings.Settings{})
	missing := filepath.Join(t.TempDir(), "no-such-folder")

	rec := get(t, s, browseAt+"?at="+missing)
	if rec.Code != 200 {
		t.Fatalf("browsing a folder that is not there gave %d, want a page", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("browse.unreadable")) {
		t.Errorf("nothing says the folder could not be read:\n%s", rec.Body.String())
	}
}

func TestStartFrom_AnswersWithAnAbsolutePathWhateverItWasHanded(t *testing.T) {
	// Everything on the page is built from this one string. A relative path would
	// mean one thing to the program, which has its own working directory, and
	// another to the reader — and the check would then be on something other than
	// what is read.
	for _, asked := range []string{".", filepath.Join("..", "somewhere")} {
		if got := startFrom(asked, ""); !filepath.IsAbs(got) {
			t.Errorf("startFrom(%q) = %q, which is not absolute", asked, got)
		}
	}
}

func TestAbove_StopsAtTheRootRatherThanCirclingOnIt(t *testing.T) {
	// Every path that has run out answers with itself, so a chooser that trusted
	// the answer would offer "up" forever at the top.
	root := filepath.VolumeName(mustAbs(t, string(filepath.Separator))) + string(filepath.Separator)
	if up := above(root); up != "" {
		t.Errorf("above(%q) = %q, want nothing above the root", root, up)
	}
	deep := mustAbs(t, filepath.Join("a", "b"))
	if up := above(deep); up == "" || up == deep {
		t.Errorf("above(%q) = %q, want the folder above it", deep, up)
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q): %v", path, err)
	}
	return abs
}

func TestSplit_KeepsEveryWayDeeperEvenWhenThereAreMoreOfThemThanItShows(t *testing.T) {
	// Walking is how the reader reaches the file, so the folders are the one
	// thing a limit may not take. The tree here holds MORE folders than the page
	// shows: with a handful of folders and a heap of files, a chooser that
	// trimmed folders and files alike would look exactly like one that does not,
	// and this test would say nothing about the rule it exists for.
	dir := t.TempDir()
	const folders, files = browseLimit + 5, 20
	for i := range folders {
		if err := os.Mkdir(filepath.Join(dir, "folder"+strconv.Itoa(i)), 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
	}
	for i := range files {
		name := "list" + strconv.Itoa(i) + ".txt"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	gotFolders, gotFiles, left := split(dir, entries)
	if len(gotFolders) != folders {
		t.Errorf("%d folders of %d, so a way deeper was dropped to make room for names",
			len(gotFolders), folders)
	}
	if len(gotFiles)+left != files {
		t.Errorf("%d files shown and %d left of %d, which loses files without saying so",
			len(gotFiles), left, files)
	}
	if left == 0 {
		t.Error("nothing was reported as left out, though more was there than the page shows")
	}
}

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

// rootedAt is a server whose chooser goes no further up than the tree handed
// in. Where a test binary sits is not something a test can arrange, so the root
// is named rather than found.
func rootedAt(t *testing.T, dir string, saved settings.Settings) *Server {
	t.Helper()
	s, _ := serverWithSettings(t, saved)
	s.browseRoot = dir
	return s
}

func TestBrowse_OffersTheListsAndTheWayDeeperAndNothingElse(t *testing.T) {
	// A chooser is for finding one file among many. A listing that puts every
	// binary on the machine beside it is one nobody can use, and it is also a way
	// of learning what a machine holds — which this page has no business helping
	// with.
	dir := listTree(t)
	body := browseAtDir(t, rootedAt(t, dir, settings.Settings{}), dir)

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
	inner := filepath.Join(dir, "inner")
	if err := os.WriteFile(filepath.Join(inner, "kept.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s := rootedAt(t, dir, settings.Settings{
		Proxy: settings.ProxySource{Kind: "file", Location: filepath.Join(inner, "kept.txt")},
	})

	body := get(t, s, browseAt).Body.String()
	if !strings.Contains(body, "kept.txt") {
		t.Errorf("the chooser did not open in the folder the settings name:\n%s", body)
	}
}

func TestBrowse_SaysSoWhenAFolderCannotBeReadRatherThanFallingOver(t *testing.T) {
	// A folder this machine will not open is not a fault in the program, and it
	// is not a reason for the page to break: the reader has to be able to walk
	// back out of it.
	dir := listTree(t)
	missing := filepath.Join(dir, "no-such-folder")

	rec := get(t, rootedAt(t, dir, settings.Settings{}), browseAt+"?at="+missing)
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
	root := t.TempDir()
	for _, asked := range []string{".", filepath.Join("..", "somewhere")} {
		if got := startFrom(asked, "", root); !filepath.IsAbs(got) {
			t.Errorf("startFrom(%q) = %q, which is not absolute", asked, got)
		}
	}
}

func TestStartFrom_NeverLeavesTheProgramsOwnDirectory(t *testing.T) {
	// The chooser is for the lists put beside this program. Walking above that
	// turns a page for picking a file into a way of reading what the machine
	// holds, and the address bar is the obvious way to try it.
	root := t.TempDir()
	inner := filepath.Join(root, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// A name beside the root that begins the same way is not inside it, and a
	// test on the text alone would say it was.
	beside := root + "-next-door"

	for _, asked := range []string{filepath.Dir(root), beside,
		filepath.Join(root, "..", ".."), string(filepath.Separator)} {
		if got := startFrom(asked, "", root); got != root {
			t.Errorf("startFrom(%q) = %q, want the root %q", asked, got, root)
		}
	}
	// And what is inside is left alone, or the clamp would be a chooser that
	// never moves.
	if got := startFrom(inner, "", root); got != inner {
		t.Errorf("startFrom(%q) = %q, want the folder that was asked for", inner, got)
	}
}

func TestAbove_StopsAtTheProgramsOwnDirectory(t *testing.T) {
	// Walking up stops where the chooser starts. Without this the "up" link is
	// the way out of the root that startFrom refuses to take.
	root := t.TempDir()
	if up := above(root, root); up != "" {
		t.Errorf("above(%q) = %q, want nothing above the root", root, up)
	}
	inner := filepath.Join(root, "inner")
	if up := above(inner, root); up != root {
		t.Errorf("above(%q) = %q, want the root %q", inner, up, root)
	}
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

func TestSettings_PutsTheChooserAwayWhenTheListIsNotReadFromAFile(t *testing.T) {
	// Looking through folders for a list is an offer worth making only when the
	// addresses come from a file. Left up, it leads somewhere the reader has no
	// use for and then back again.
	//
	// No line of the script runs here — there is no runtime to run it with — so
	// what is pinned is the seam: the script decides by the box named here and
	// puts away the link written here, and renaming either on its own leaves a
	// page where the offer never goes away.
	script := mustAsset(t, "static/app.js")
	for _, part := range []string{"[name=source]", "/settings/browse", `"file"`} {
		if !strings.Contains(script, part) {
			t.Errorf("the script never names %q, so the chooser stays up whatever is chosen", part)
		}
	}

	s, _ := serverWithSettings(t, settings.Defaults())
	body := getBody(t, s, settingsAt)
	if !strings.Contains(body, `name="source"`) {
		t.Error("the page has no box saying where the addresses are read from")
	}
	if !strings.Contains(body, `value="file"`) {
		t.Error("the page offers no file as a source, so the script's one condition can never hold")
	}
}

func TestBrowse_ShowsItsListingAsSomethingThatScrollsWithinItself(t *testing.T) {
	// Five hundred entries laid out down the page put the way back out of the
	// chooser below everything, and the reader who most needs it is the one in
	// the largest folder.
	style := mustAsset(t, "static/app.css")
	at := strings.Index(style, ".browse {")
	if at < 0 {
		t.Fatal("the listing has no style of its own, so it runs the length of the page")
	}
	rule := style[at:min(at+400, len(style))]
	if !strings.Contains(rule, "max-height") {
		t.Errorf("the listing is given no height to stay within: %q", rule)
	}
	if !strings.Contains(rule, "overflow-y: auto") {
		t.Errorf("the listing does not scroll within itself: %q", rule)
	}
}

func TestBrowse_ShowsWhereItIsFromTheProgramRatherThanFromTheMachine(t *testing.T) {
	// The reader is choosing among what was put beside the program. The
	// machine's own arrangement above that is neither theirs to walk nor this
	// page's to put on a screen somebody photographs.
	dir := listTree(t)
	body := browseAtDir(t, rootedAt(t, dir, settings.Settings{}), filepath.Join(dir, "inner"))

	if strings.Contains(body, dir) {
		t.Errorf("the page carries this machine's own path to the folder:\n%s", body)
	}
	if !strings.Contains(body, "inner") {
		t.Errorf("the page does not say which folder is being shown:\n%s", body)
	}
}

func TestBrowse_OffersNoWayOutOfTheProgramsOwnDirectory(t *testing.T) {
	// The clamp on the address is only half of it: a link up out of the root
	// would walk the reader out through the page itself.
	dir := listTree(t)
	body := browseAtDir(t, rootedAt(t, dir, settings.Settings{}), dir)

	if strings.Contains(body, LangEN.T("browse.up")) {
		t.Errorf("the root offers a way further up:\n%s", body)
	}
	// And a folder inside it does offer one, or the rule would be a chooser
	// nobody can walk back through.
	inner := browseAtDir(t, rootedAt(t, dir, settings.Settings{}), filepath.Join(dir, "inner"))
	if !strings.Contains(inner, LangEN.T("browse.up")) {
		t.Errorf("a folder inside the root offers no way back up:\n%s", inner)
	}
}

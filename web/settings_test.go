// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/settings"
)

// stubConnect stands where the ports go. It records the settings it was asked
// to open, because what a swap is built from is half of what a save has to get
// right: settings written to the file and an engine built from the ones before
// them is a server running on what the reader replaced.
// stubConnect stands where the pools are opened from, and records the settings
// and the size each raise was asked for. The size is recorded because it is the
// job's own now: a raiser that took it from anywhere else would look identical
// from outside without it.
// errNoLivePool is what the stand answers with instead of opening anything.
var errNoLivePool = errors.New("no pool is opened in these tests")

type stubConnect struct {
	mu    sync.Mutex
	with  []settings.Settings
	sizes [][2]int
	err   error
}

func (c *stubConnect) open(_ context.Context, saved settings.Settings, ports, threads int) (*blanktrail.Pool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.with = append(c.with, saved)
	c.sizes = append(c.sizes, [2]int{ports, threads})
	if c.err != nil {
		return nil, c.err
	}
	// These tests are about which settings and which size a raise was asked for,
	// never about the pool itself, so none is opened. The refusal is what keeps
	// the supervisor from running a job on nothing.
	return nil, errNoLivePool
}

// lastSize is the size the last raise was asked for.
func (c *stubConnect) lastSize() [2]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sizes) == 0 {
		return [2]int{}
	}
	return c.sizes[len(c.sizes)-1]
}

// asked is the last settings an engine was built from, and whether one ever was.
func (c *stubConnect) asked() (settings.Settings, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.with) == 0 {
		return settings.Settings{}, false
	}
	return c.with[len(c.with)-1], true
}

// settingsFile writes a settings file in a directory of this test's own and
// returns where it is.
func settingsFile(t *testing.T, saved settings.Settings) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gserp-settings.json")
	if err := settings.Save(path, saved); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

// serverWithSettings is a server that keeps the settings handed in, and has
// nothing to run a job on.
func serverWithSettings(t *testing.T, saved settings.Settings) (*Server, string) {
	t.Helper()
	path := settingsFile(t, saved)
	s, err := New(Config{Store: testStore(t), Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, path
}

// serverWithRunningJob is a server with a job in flight that will not end on its
// own, and a stand-in where the ports go.
//
// The job is held rather than merely started: every question this file asks
// about a running job is a question about the moment it is running in, and a
// job that had already finished would answer all of them the same way whatever
// the handler did.
func serverWithRunningJob(t *testing.T) (*Server, string, *stubConnect) {
	t.Helper()
	st := testStore(t)
	v := newSupervisor(st, &heldEngine{hold: make(chan struct{})})
	t.Cleanup(func() { _ = v.Close() })

	path := settingsFile(t, settings.Settings{
		ControlURL: "http://127.0.0.1:1", APIKey: "keep-me", SearchPorts: 3,
	})
	opener := &stubConnect{}
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.connect = opener.open

	id := enqueue(t, v, "nightly", "a", "b", "c")
	waitUntilRunning(t, v, id)
	return s, path, opener
}

// getBody is what a page carries, as text.
func getBody(t *testing.T, s *Server, path string) string {
	t.Helper()
	rec := get(t, s, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s gave %d, want 200:\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// postBody is what an answer to a posted form carries, as text.
func postBody(t *testing.T, s *Server, path string, values url.Values) string {
	t.Helper()
	return postForm(t, s, path, values).Body.String()
}

// loaded is the settings as they now stand on disk.
func loaded(t *testing.T, path string) settings.Settings {
	t.Helper()
	got, err := settings.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return got
}

func TestSettings_ShowsTheKeyOnlyAsATail(t *testing.T) {
	// What the page carries reaches the browser's history, the operator's
	// screen and anything that captures either.
	s, _ := serverWithSettings(t, settings.Settings{
		ControlURL: "http://127.0.0.1:1", APIKey: "0123456789abcdef0123456789abcdef",
	})
	body := getBody(t, s, settingsAt)
	if strings.Contains(body, "0123456789abcdef") {
		t.Error("the page carries the key")
	}
	if !strings.Contains(body, "cdef") {
		t.Error("the page shows no tail, so nobody can tell which key is set")
	}
}

func TestSettings_LeavesTheKeyBoxEmptyRatherThanFillingItWithTheTail(t *testing.T) {
	// The tail is a name for the key and not the key. A box holding it is a box
	// the next save would write back as the key itself, and the connection would
	// be replaced by four characters of its own name.
	s, path := serverWithSettings(t, settings.Settings{APIKey: "0123456789abcdef"})
	body := getBody(t, s, settingsAt)

	box := openingTag(t, body, `input id="api_key"`)
	if !strings.Contains(box, `value=""`) {
		t.Errorf("the key box is not empty: <%s>", box)
	}
	// The other half of the same rule, read from the other end: what the page
	// carries is not what the file holds.
	postForm(t, s, settingsAt, url.Values{})
	if got := loaded(t, path); got.APIKey != "0123456789abcdef" {
		t.Errorf("the key is now %q — the page handed its own tail back as the key", got.APIKey)
	}
}

func TestSaveSettings_AnEmptyKeyFieldKeepsTheKeyThatWasThere(t *testing.T) {
	// The field cannot show the key, so somebody opening the settings to change
	// the port count and pressing save would otherwise lose their connection.
	// This is the first thing anybody will do.
	s, path := serverWithSettings(t, settings.Settings{APIKey: "keep-me", SearchPorts: 3})
	postForm(t, s, settingsAt, url.Values{"api_key": {""}, "search_ports": {"12"}})

	got := loaded(t, path)
	if got.APIKey != "keep-me" {
		t.Errorf("the key is now %q — an empty field erased it", got.APIKey)
	}
	if got.SearchPorts != 12 {
		t.Errorf("Ports=%d, want the 12 that was asked for", got.SearchPorts)
	}
}

func TestSaveSettings_ANewKeyReplacesTheOldOne(t *testing.T) {
	// The other half of the rule above: empty means keep, anything else means
	// change. Without this test the first rule could be "never change the key".
	s, path := serverWithSettings(t, settings.Settings{APIKey: "old"})
	postForm(t, s, settingsAt, url.Values{"api_key": {"new"}})

	if got := loaded(t, path); got.APIKey != "new" {
		t.Errorf("the key is %q, want the new one", got.APIKey)
	}
}

func TestSaveSettings_WritesNothingDownWhenItRefuses(t *testing.T) {
	// A form checked after it has been written is a form that has already taken
	// effect. Nought ports is not the fault here — it is how the search address
	// is turned off — so the fault this posts is a count that is not one.
	s, path := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1", SearchPorts: 3})
	rec := postForm(t, s, settingsAt, url.Values{
		"control_url": {"http://127.0.0.1:2"}, "search_ports": {"a handful"},
	})

	if rec.Code == http.StatusSeeOther {
		t.Fatal("a port count that is not a number was accepted")
	}
	got := loaded(t, path)
	if got.SearchPorts != 3 || got.ControlURL != "http://127.0.0.1:1" {
		t.Errorf("the refused form was written down anyway: %+v", got)
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("settings.search.count")) {
		t.Errorf("the page does not say what is wrong:\n%s", rec.Body.String())
	}
}

func TestSaveSettings_KeepsWhatWasTypedWhenItRefuses(t *testing.T) {
	// A form that loses an address somebody has just typed over a mistake in the
	// box beside it is a form filled in twice.
	s, _ := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	body := postBody(t, s, settingsAt, url.Values{
		"control_url": {"http://127.0.0.1:9"}, "search_ports": {"nought"},
	})

	if !strings.Contains(body, "http://127.0.0.1:9") {
		t.Errorf("the address that was typed was not given back with the complaint:\n%s", body)
	}
}

func TestSaveSettings_HandsTheJobsAfterThisOneTheConnectionItJustWrote(t *testing.T) {
	// Nothing is opened by a save any more: a job puts up its own pool when it
	// starts, so what a save hands over is the way to open one. The test is
	// therefore about the next job, not about the moment of saving — a raiser
	// recorded but never reached would look identical from the settings page.
	s, path := serverWithSettings(t, settings.Settings{
		ControlURL: "http://127.0.0.1:1", APIKey: "keep-me", SearchPorts: 3,
	})
	st := testStore(t)
	v := newSupervisor(st, nil)
	t.Cleanup(func() { _ = v.Close() })
	s.sup = v
	opener := &stubConnect{}
	s.connect = opener.open

	postForm(t, s, settingsAt, url.Values{
		"control_url": {"http://127.0.0.1:2"}, "api_key": {""}, "search_ports": {"12"},
	})
	enqueueSized(t, v, "after the save", 9, 4, "a")

	waitUntil(t, "the job to have asked for a pool", func() bool {
		_, asked := opener.asked()
		return asked
	})
	built, _ := opener.asked()
	if built != loaded(t, path) {
		t.Errorf("the pool was raised from %+v, and %+v was saved", built, loaded(t, path))
	}
	if built.ControlURL != "http://127.0.0.1:2" || built.APIKey != "keep-me" {
		t.Errorf("raised from %+v, want the address just typed and the key that was kept", built)
	}
	// The size is the job's, not the settings': the two are different numbers
	// here on purpose, so a raiser reading the wrong one cannot pass.
	if got := opener.lastSize(); got != [2]int{9, 4} {
		t.Errorf("the pool was raised at %v, want the 9 ports and 4 threads the job asked for", got)
	}
}
func TestSaveSettings_SavesAConnectionItCannotOpenRatherThanRefusingIt(t *testing.T) {
	// Nothing is opened by a save. A job puts up its own pool when it starts, so
	// a connection that will not open is discovered there and reported there —
	// and refusing to write it down here would leave somebody unable to save a
	// setting they are half way through repairing.
	//
	// What the reader has instead is the check beside the save, which tries what
	// is in the boxes and writes nothing.
	s, path := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	st := testStore(t)
	v := newSupervisor(st, nil)
	t.Cleanup(func() { _ = v.Close() })
	s.sup = v
	s.connect = (&stubConnect{err: http.ErrServerClosed}).open

	rec := postForm(t, s, settingsAt, url.Values{"control_url": {"http://127.0.0.1:2"}})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("saving a connection that will not open came back %d, want the settings saved",
			rec.Code)
	}
	if got := loaded(t, path); got.ControlURL != "http://127.0.0.1:2" {
		t.Errorf("the address that was typed was not saved: %+v", got)
	}
}
func TestSaveSettings_KeepsThePauseAndTheListItWasGiven(t *testing.T) {
	// The two boxes read in units a person thinks in and the file holds lengths
	// of time. A box read as nanoseconds is a box filled in once, and the pause
	// nobody asked for is then blamed on the program.
	s, path := serverWithSettings(t, settings.Settings{})
	postForm(t, s, settingsAt, url.Values{
		"cooldown": {"3"}, "source": {"url"},
		"source_at": {"https://example.test/list"}, "source_refresh": {"15"},
	})

	got := loaded(t, path)
	if got.Cooldown != 3*time.Second {
		t.Errorf("the pause is %v, want the 3 seconds that were typed", got.Cooldown)
	}
	want := settings.ProxySource{
		Kind: "url", Location: "https://example.test/list", Refresh: 15 * time.Minute,
	}
	if got.Proxy != want {
		t.Errorf("the list is %+v, want %+v", got.Proxy, want)
	}
}

func TestSaveSettings_TakesNoSourceToMeanNoListRatherThanNoChange(t *testing.T) {
	// A list has to be switchable off. Every other box on this page keeps what
	// is saved when it arrives empty, so the one that turns the list off is a
	// choice with a name rather than a box left blank.
	s, path := serverWithSettings(t, settings.Settings{
		Proxy: settings.ProxySource{Kind: "file", Location: "list.txt", Refresh: time.Hour},
	})
	postForm(t, s, settingsAt, url.Values{"source": {""}, "source_at": {"list.txt"}})

	if got := loaded(t, path); got.Proxy != (settings.ProxySource{}) {
		t.Errorf("the list is still %+v after being switched off", got.Proxy)
	}
}

func TestSaveSettings_RefusesAPauseThatIsNotALengthOfTime(t *testing.T) {
	// The other half of the two boxes above: what cannot be read is said rather
	// than rounded to nought, which would be a pause nobody asked for.
	s, path := serverWithSettings(t, settings.Settings{Cooldown: 2 * time.Second})
	body := postBody(t, s, settingsAt, url.Values{"cooldown": {"-1"}})

	if !strings.Contains(body, LangEN.T("settings.pause.length")) {
		t.Errorf("the page does not say what is wrong with the pause:\n%s", body)
	}
	if got := loaded(t, path); got.Cooldown != 2*time.Second {
		t.Errorf("the pause is now %v — a refused form was written down", got.Cooldown)
	}
}

func TestSaveSettings_RefusesALanguageThisInterfaceIsNotWrittenIn(t *testing.T) {
	// Taken and saved, it would leave every page of this program answering in a
	// language nobody wrote, which is to say in bare keys.
	s, path := serverWithSettings(t, settings.Settings{Language: string(LangRU)})
	body := postBody(t, s, settingsAt, url.Values{"language": {"fr"}})

	if !strings.Contains(body, LangRU.T("settings.language.unknown")) {
		t.Errorf("the page does not say the language is not one of this program's:\n%s", body)
	}
	if got := loaded(t, path); got.Language != string(LangRU) {
		t.Errorf("the language is now %q", got.Language)
	}
}

func TestSaveSettings_SaysSoWhenTheFileCannotBeWritten(t *testing.T) {
	// Answering a save that never happened with the page it would have gone to
	// is telling the reader their connection is set up when it is not, and every
	// screen after it agrees with them.
	s, err := New(Config{
		Store: testStore(t), Logger: quiet(),
		SettingsPath: filepath.Join(t.TempDir(), "nowhere", "gserp-settings.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := postForm(t, s, settingsAt, url.Values{"control_url": {"http://127.0.0.1:2"}})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a save that could not be written came back %d, want a refusal", rec.Code)
	}
}

func TestSettings_DrawThemselvesOverAFileTheyCannotRead(t *testing.T) {
	// It is the file somebody's connection is kept in. Refusing to draw the page
	// would leave them with no way of writing settings this program accepts, and
	// the one screen that can repair the file is the one reporting it.
	s, path := serverWithSettings(t, settings.Settings{})
	if err := os.WriteFile(path, []byte("{not settings"), 0o600); err != nil {
		t.Fatalf("writing the file: %v", err)
	}

	if body := getBody(t, s, settingsAt); !strings.Contains(body, LangEN.T("settings.unreadable")) {
		t.Errorf("the page says nothing about a file it could not read:\n%s", body)
	}
	postForm(t, s, settingsAt, url.Values{"control_url": {"http://127.0.0.1:2"}, "search_ports": {"6"}})
	if got := loaded(t, path); got.ControlURL != "http://127.0.0.1:2" || got.SearchPorts != 6 {
		t.Errorf("saving over the damaged file left %+v", got)
	}
}

func TestCheckConnection_SaysSoWhenTheAddressIsNotOne(t *testing.T) {
	// Nothing can be tried against it, and an empty list of findings under a
	// heading reads as a check that passed.
	s, _ := serverWithSettings(t, settings.Settings{})
	body := postBody(t, s, checkAt, url.Values{"control_url": {"://not an address"}})

	if !strings.Contains(body, LangEN.T("settings.address.unusable")) {
		t.Errorf("the page does not say the address cannot be reached:\n%s", body)
	}
	if strings.Contains(body, LangEN.T("settings.check.nothing")) {
		t.Errorf("a check that never happened is drawn as one that found nothing:\n%s", body)
	}
}

func TestCheckConnection_TriesWithoutSaving(t *testing.T) {
	// A wrong address must not overwrite a working one before anybody finds
	// out it is wrong.
	s, path := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	postForm(t, s, checkAt, url.Values{"control_url": {"http://127.0.0.1:2"}})

	if got := loaded(t, path); got.ControlURL != "http://127.0.0.1:1" {
		t.Errorf("checking saved the settings: %q", got.ControlURL)
	}
}

func TestCheckConnection_ShowsEveryFindingRatherThanAVerdict(t *testing.T) {
	// Preflight says what is wrong and what to do about it. Reducing that to
	// "failed" throws away the only part that helps.
	s, _ := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	body := postBody(t, s, checkAt, url.Values{"control_url": {"http://127.0.0.1:1"}})

	if !strings.Contains(body, "fail") && !strings.Contains(body, "Fail") {
		t.Errorf("no finding is shown:\n%s", body)
	}
}

func TestCheckConnection_ShowsWhatToDoAboutEveryOneOfThem(t *testing.T) {
	// One finding shown is a verdict wearing a list's clothes. What is asked
	// here is that a check with several answers shows all of them — the ones
	// that would stop a run and the ones that would only spoil it — and shows
	// what each of them says to do, which is the half a verdict throws away.
	//
	// What the findings say is never written down here. This package knows a
	// finding has a title, a detail and an action, and nothing whatever about
	// what any of them is about, so the check is run again beside the page and
	// the page is asked to carry what came back.
	fake := fakebt.New(t)
	fake.SetLicense(fakebt.License{
		Activated: true, Plan: "Pro", Pool: true,
		JsSolverMaxProcs: 8, JsSolverProcs: 1,
	})
	s, _ := serverWithSettings(t, settings.Settings{})
	body := postBody(t, s, checkAt, url.Values{
		"control_url": {fake.URL()}, "api_key": {fake.Key()},
		"search_ports": {"4"},
	})

	client, err := blanktrail.NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	report := blanktrail.Preflight(t.Context(), client,
		blanktrail.PreflightInput{Domains: reachedDomains, Ports: 4})
	if len(report.Findings) < 2 {
		t.Fatalf("this check came back with %d findings, so it cannot tell a list from a verdict",
			len(report.Findings))
	}
	if len(report.Blocking()) == len(report.Findings) {
		t.Fatal("every finding here would stop a run, so a page showing only those would pass this")
	}
	for _, f := range report.Findings {
		for what, said := range map[string]string{"title": f.Title, "detail": f.Detail, "action": f.Action} {
			if said == "" {
				continue
			}
			// Escaped as the template escapes it: a page that is perfectly correct
			// carries an apostrophe as an entity, and a search for the raw sentence
			// would call that a fault.
			if !strings.Contains(body, template.HTMLEscapeString(said)) {
				t.Errorf("the page does not carry the %s of one finding (%q):\n%s", what, said, body)
			}
		}
	}
}

func TestCheckConnection_TriesWhatIsInTheBoxesRatherThanWhatIsSaved(t *testing.T) {
	// A check of the saved settings answers about the connection the reader is
	// replacing. It writes nothing either way, so nothing else in this file
	// would notice: the page would simply report on another machine.
	//
	// The saved address is looked for on the page rather than the findings being
	// read, because a check that went there says so by quoting where it got no
	// answer, and the box on the screen now holds the other address.
	fake := fakebt.New(t)
	s, _ := serverWithSettings(t, settings.Settings{
		ControlURL: "http://127.0.0.1:1", APIKey: fake.Key(),
	})
	body := postBody(t, s, checkAt, url.Values{"control_url": {fake.URL()}})

	if !strings.Contains(body, LangEN.T("settings.check.title")) {
		t.Fatalf("nothing was checked at all:\n%s", body)
	}
	if strings.Contains(body, "127.0.0.1:1") {
		t.Errorf("the check went to the saved address rather than the one in the box:\n%s", body)
	}
}

func TestCheckConnection_UsesTheSavedKeyWhenTheBoxIsEmpty(t *testing.T) {
	// The box cannot show the key, so it is empty on every page this program
	// draws. A check that took that emptiness for the key would report a
	// connection that is perfectly good as refused, and whoever read it would go
	// looking for a fault in a key that has none.
	//
	// The three answers are compared rather than read, so that this package goes
	// on knowing nothing about what a check says. The wrong key is what gives the
	// comparison teeth: without it, a check that ignored the key entirely would
	// satisfy the first assertion exactly as one that used the saved key does.
	fake := fakebt.New(t)
	s, _ := serverWithSettings(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	at := url.Values{"control_url": {fake.URL()}}

	typed := postBody(t, s, checkAt, withKey(at, fake.Key()))
	empty := postBody(t, s, checkAt, withKey(at, ""))
	wrong := postBody(t, s, checkAt, withKey(at, "not-the-key"))

	if empty != typed {
		t.Errorf("the empty box was checked as if it were the key:\n%s", empty)
	}
	if wrong == typed {
		t.Error("the check pays no attention to the key at all, so the comparison above proves nothing")
	}
}

// withKey is the same form with one key in the key box.
func withKey(at url.Values, key string) url.Values {
	with := url.Values{}
	for name, values := range at {
		with[name] = values
	}
	with.Set("api_key", key)
	return with
}

func TestSaveSettings_DoesNotDisturbTheJobThatIsRunning(t *testing.T) {
	// The question this used to ask — «now, or after this job?» — existed because
	// one set of identities was shared by everything on the machine. A job holds
	// the pool it raised for itself until it ends, so there is nothing left to
	// take away from it and nobody to ask.
	s, path, _ := serverWithRunningJob(t)
	before, running := s.sup.Running()
	if !running {
		t.Fatal("the fixture has no job in flight, so this test asks nothing")
	}

	rec := postForm(t, s, settingsAt, url.Values{"search_ports": {"12"}})
	if rec.Code >= 400 {
		t.Fatalf("saving while a job ran gave %d", rec.Code)
	}
	if got, ok := s.sup.Running(); !ok || got != before {
		t.Errorf("running=%d,%v — saving settings took the job down", got, ok)
	}
	if body := rec.Body.String(); strings.Contains(body, "settings.running") {
		t.Errorf("the page still asks about the running job: %s", body)
	}
	if got := loaded(t, path); got.SearchPorts != 12 {
		t.Errorf("SearchPorts=%d — the settings were not saved", got.SearchPorts)
	}
}

func TestSettings_AreOfferedFromEveryScreenAndOnlyWhereTheyCanBeSaved(t *testing.T) {
	// A link to a page that cannot save is worse than no link: what it takes is
	// dropped, and nothing on the screen says so until the next restart.
	kept, _ := serverWithSettings(t, settings.Settings{})
	for _, at := range []string{stateAt, jobsAt, newAt, historyAt} {
		if !strings.Contains(getBody(t, kept, at), `href="`+settingsAt+`"`) {
			t.Errorf("%s does not offer the settings", at)
		}
	}
	if body := getBody(t, testServer(t), jobsAt); strings.Contains(body, `href="`+settingsAt+`"`) {
		t.Error("a server that keeps no settings offers a link to them anyway")
	}
	if code := get(t, testServer(t), settingsAt).Code; code != http.StatusNotFound {
		t.Errorf("GET %s on a server that keeps no settings gave %d, want 404", settingsAt, code)
	}
}

func TestSettings_AreReachedWithoutTheScriptThatSwapsScreens(t *testing.T) {
	// The script takes a press over only inside the strip of tabs. A settings
	// link inside it would be swapped in like a screen, and the page would then
	// have to be dismissed by something — which is the script every page here
	// works without.
	s, _ := serverWithSettings(t, settings.Settings{})
	strip := oneTag(t, getBody(t, s, stateAt), "nav")
	if !strings.Contains(strip, `id="`+tabsAnchor+`"`) {
		t.Fatalf("the first navigation on the page is not the tabs:\n%s", strip)
	}
	if strings.Contains(strip, settingsAt) {
		t.Errorf("the settings stand among the tabs:\n%s", strip)
	}
}

func TestSettings_LoadNothingFromAnywhereElse(t *testing.T) {
	// Everything ships inside the binary. A page that reaches out fails on the
	// machine this program is most likely to run on — the one with no way out.
	s, _ := serverWithSettings(t, settings.Settings{})
	loads := resourcesOf(getBody(t, s, settingsAt))
	if len(loads) == 0 {
		t.Error("the settings page was read as loading nothing at all, not even its stylesheet")
	}
	for _, address := range loads {
		if !strings.HasPrefix(address, "/") || strings.HasPrefix(address, "//") {
			t.Errorf("the settings page loads %q from outside this binary", address)
		}
	}
}

func TestSettings_ShowNoBareKeyWhereAPhraseBelongs(t *testing.T) {
	// A key on the page is a phrase that was never looked up. It survives every
	// test written about a particular phrase, because it lands on the phrases
	// nobody thought to check.
	//
	// The page as it is opened, the page complaining and the page carrying a
	// check are all drawn, since between them they put every phrase this screen
	// has on a screen.
	s, _ := serverWithSettings(t, settings.Settings{APIKey: "0123456789abcdef"})
	running, _, _ := serverWithRunningJob(t)
	for _, l := range Languages() {
		lang := "?lang=" + string(l)
		bodies := []string{
			getBody(t, s, settingsAt+lang),
			postBody(t, s, settingsAt+lang, url.Values{"search_ports": {"nought"}, "source": {"nowhere"}}),
			postBody(t, s, checkAt+lang, url.Values{"control_url": {"http://127.0.0.1:1"}}),
			postBody(t, running, settingsAt+lang, url.Values{"search_ports": {"12"}}),
		}
		for _, body := range bodies {
			for key := range catalogue[l] {
				if strings.Contains(body, key) {
					t.Errorf("the %s settings page shows the key %q where its text belongs", l, key)
				}
			}
		}
	}
}

func TestSettings_AnswerInTheLanguageThisMachineWasSetUpIn(t *testing.T) {
	// A language saved and never read is a box that does nothing, and somebody
	// will set it and conclude the program ignores what it is told.
	s, _ := serverWithSettings(t, settings.Settings{Language: string(LangRU)})
	if body := getBody(t, s, jobsAt); !strings.Contains(body, LangRU.T("jobs.none")) {
		t.Errorf("the page did not come back in the language this machine is set up in:\n%s", body)
	}

	// What the reader asks for still wins. The saved language is a decision made
	// on this machine, and the reader in front of it is the one person who knows
	// better than the machine which language they read.
	if body := getBody(t, s, jobsAt+"?lang=en"); !strings.Contains(body, LangEN.T("jobs.none")) {
		t.Errorf("the saved language overruled the reader's own:\n%s", body)
	}
}

func TestSaveSettings_TellsThisBrowserTheLanguageItJustChose(t *testing.T) {
	// The reader chose it outright. Without this the choice they made with the
	// switcher last week goes on overruling the one they just made, and the box
	// looks broken.
	s, _ := serverWithSettings(t, settings.Settings{})
	rec := postForm(t, s, settingsAt, url.Values{"language": {string(LangRU)}})

	var written string
	for _, c := range rec.Result().Cookies() {
		if c.Name == langCookie {
			written = c.Value
		}
	}
	if written != string(LangRU) {
		t.Errorf("the answer remembered %q, want the language just chosen", written)
	}
}

func TestNewJob_RefusesToStartUntilThereIsSomethingToRunItOn(t *testing.T) {
	// A server whose connection has not been set up holds a job it is given
	// rather than losing it, so a job taken from this form would sit in the
	// queue looking started while nothing ran. This page is the one place that
	// can say why, and it says where to go about it.
	st := testStore(t)
	v := NewSupervisorWithoutAPool(st)
	t.Cleanup(func() { _ = v.Close() })
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := postForm(t, s, "/new?do=start", url.Values{
		"name": {"nightly"}, "queries": {"iphone 13"}, "pages": {"1"},
	})
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a job was accepted by a server with nothing to run it on")
	}
	if !strings.Contains(rec.Body.String(), LangEN.T("form.notsetup")) {
		t.Errorf("the reader was not told why nothing happened:\n%s", rec.Body.String())
	}
	jobs, err := s.store.Jobs(t.Context(), 0)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs were written down by a server that cannot run them", len(jobs))
	}
}

func TestResume_RefusesUntilThereIsSomethingToRunItOn(t *testing.T) {
	// The same as above from the other page: a job carried on where nothing can
	// run it waits in the queue with nothing saying why.
	st := testStore(t)
	v := NewSupervisorWithoutAPool(st)
	t.Cleanup(func() { _ = v.Close() })
	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := seedJob(t, s, "nightly", 3, 0, 0)

	rec := postForm(t, s, "/api/resume", url.Values{"job": {strconv.FormatInt(id, 10)}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("carrying the job on gave %d, want a refusal", rec.Code)
	}
	if q := v.Queued(); len(q) != 0 {
		t.Errorf("the job was queued anyway: %v", q)
	}
}

// openingTag reads back one opening tag of a page, so a test can ask about the
// attributes of a single box rather than about whether the whole page mentions
// something somewhere.
func openingTag(t *testing.T, body, opening string) string {
	t.Helper()
	_, after, ok := strings.Cut(body, "<"+opening)
	if !ok {
		t.Fatalf("the page carries no <%s:\n%s", opening, body)
	}
	rest, _, ok := strings.Cut(after, ">")
	if !ok {
		t.Fatalf("the <%s tag is never closed:\n%s", opening, body)
	}
	return opening + rest
}

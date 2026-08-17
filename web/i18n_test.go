// SPDX-License-Identifier: MIT

package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCatalogue_BothLanguagesSayTheSameThings(t *testing.T) {
	// A key present in one language and missing in the other renders as a hole
	// on the page, and only a reader of that language would ever notice.
	en, ru := catalogue[LangEN], catalogue[LangRU]
	for key := range en {
		if _, ok := ru[key]; !ok {
			t.Errorf("ru is missing %q", key)
		}
	}
	for key := range ru {
		if _, ok := en[key]; !ok {
			t.Errorf("en is missing %q", key)
		}
	}
}

func TestCatalogue_SaysNothingTwiceTheSameWay(t *testing.T) {
	// Two languages holding the same key with the same text means one of them
	// was never translated. Names of programs and units would be the honest
	// exception, and the catalogue holds none: it is sentences.
	for key, text := range catalogue[LangEN] {
		if catalogue[LangRU][key] == text {
			t.Errorf("%q reads the same in both languages: %q", key, text)
		}
	}
}

func TestCheckCatalogue_RefusesALanguageShortOfTextAnotherHas(t *testing.T) {
	// The check exists so a half-finished translation stops the program at
	// startup instead of showing a bare key to the one reader of that language.
	short := map[Lang]map[string]string{
		LangEN: {"jobs.none": "No jobs yet.", "jobs.title": "Jobs"},
		LangRU: {"jobs.none": "Заданий пока нет."},
	}
	err := checkCatalogue(short)
	if !errors.Is(err, ErrMissingText) {
		t.Fatalf("checkCatalogue gave %v, want ErrMissingText", err)
	}
	if !strings.Contains(err.Error(), "jobs.title") {
		t.Errorf("the complaint does not name the missing key: %v", err)
	}
	if err := checkCatalogue(catalogue); err != nil {
		t.Errorf("the catalogue this program ships was refused: %v", err)
	}
}

func TestT_FallsBackToTheKeyRatherThanToNothing(t *testing.T) {
	// An empty string on a page is invisible; the key is ugly and reports
	// itself, which is what an unfinished translation should do.
	if got := LangRU.T("no.such.key"); got != "no.such.key" {
		t.Errorf("T of an unknown key gave %q, want the key back", got)
	}
}

func TestPickLang_PrefersTheChoiceOverTheBrowser(t *testing.T) {
	// Someone who clicked the switch has said what they want, and the browser's
	// own preference must not undo it on the next page.
	r := httptest.NewRequest(http.MethodGet, "/?lang=ru", nil)
	r.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	if got := pickLang(r); got != LangRU {
		t.Errorf("pickLang=%q, want ru", got)
	}
}

func TestPickLang_PrefersTheChoiceOverWhatWasRemembered(t *testing.T) {
	// Clicking the switch is how a reader changes their mind, and a remembered
	// answer that outranks the click makes the switch do nothing.
	r := httptest.NewRequest(http.MethodGet, "/?lang=en", nil)
	r.AddCookie(&http.Cookie{Name: langCookie, Value: string(LangRU)})
	if got := pickLang(r); got != LangEN {
		t.Errorf("pickLang=%q, want the just-clicked en", got)
	}
}

func TestPickLang_RemembersTheChoiceFromTheCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: langCookie, Value: string(LangRU)})
	r.Header.Set("Accept-Language", "en")
	if got := pickLang(r); got != LangRU {
		t.Errorf("pickLang=%q, want the remembered ru", got)
	}
}

func TestPickLang_ReadsTheBrowserWhenNobodyHasChosen(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")
	if got := pickLang(r); got != LangRU {
		t.Errorf("pickLang=%q, want ru", got)
	}
}

func TestPickLang_ReadsATagThatCarriesOnlyARegion(t *testing.T) {
	// Every element of this header carries a region, so nothing here matches a
	// bare language code and the answer depends on cutting at the hyphen.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "ru-RU")
	if got := pickLang(r); got != LangRU {
		t.Errorf("pickLang=%q, want ru", got)
	}
}

func TestPickLang_ReadsATagThatCarriesOnlyAWeight(t *testing.T) {
	// The one language here that this program has is the one written with its
	// weight attached, so the answer depends on dropping what follows the
	// semicolon.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "fr, ru;q=0.7")
	if got := pickLang(r); got != LangRU {
		t.Errorf("pickLang=%q, want ru", got)
	}
}

func TestPickLang_TakesTheFirstLanguageItHasRatherThanTheHeaviest(t *testing.T) {
	// A browser sends its languages in the order it prefers them, so the first
	// one this program can answer in is the best answer it has.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "en;q=0.2,ru;q=0.9")
	if got := pickLang(r); got != LangEN {
		t.Errorf("pickLang=%q, want the first one offered", got)
	}
}

func TestPickLang_FallsBackToEnglishForALanguageWeDoNotHave(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?lang=fr", nil)
	if got := pickLang(r); got != LangEN {
		t.Errorf("pickLang=%q, want en", got)
	}
}

func TestPickLang_KeepsTheRememberedAnswerWhenTheQueryIsNonsense(t *testing.T) {
	// A language this program does not have is not a choice, and it must not
	// wipe out the choice the reader did make.
	r := httptest.NewRequest(http.MethodGet, "/?lang=fr", nil)
	r.AddCookie(&http.Cookie{Name: langCookie, Value: string(LangRU)})
	if got := pickLang(r); got != LangRU {
		t.Errorf("pickLang=%q, want the remembered ru", got)
	}
}

// shortGerman is a translation that says some of what the interface says and
// nothing like all of it.
//
// Every test below rests on that. A file holding every key would make "shown
// because the file said it" and "filled in from English" the same string, and
// half of what is checked here would pass whether the filling in worked or not.
const shortGerman = `{
  "lang.name": "Deutsch",
  "jobs.title": "Aufträge",
  "jobs.none": "Noch keine Aufträge."
}`

// filledKey is a phrase the German file above does not say, so it can only
// arrive from English. spokenKey is one it does say.
const (
	filledKey = "job.title"
	spokenKey = "jobs.title"
)

// langDE is the language nobody built into this program.
const langDE Lang = "de"

// translationDir writes a directory of translation files and gives back where
// it is.
func translationDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "gserp-translations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("making the translations directory: %v", err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return dir
}

// loadInto reads a directory and puts back what this program said before, so
// one test's languages are not still there in the next one.
func loadInto(t *testing.T, dir string) (added []Lang, incomplete map[Lang][]string, err error) {
	t.Helper()
	before := spoken.Load()
	t.Cleanup(func() { spoken.Store(before) })
	return LoadTranslations(dir)
}

func TestLoadTranslations_AddsALanguageNobodyBuiltIn(t *testing.T) {
	// The point of the whole thing: a language arrives in a directory beside
	// the program, and the program does not have to be built again to answer in
	// it.
	added, _, err := loadInto(t, translationDir(t, map[string]string{"de.json": shortGerman}))
	if err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}
	if !slices.Equal(added, []Lang{langDE}) {
		t.Fatalf("added=%v, want just the language the directory holds", added)
	}
	if got := langDE.T(spokenKey); got != "Aufträge" {
		t.Errorf("T(%q)=%q, want what the file says", spokenKey, got)
	}

	// The switcher is the only way a reader reaches it, so a language missing
	// from there is a language nobody can ask for.
	langs := Languages()
	if !slices.Contains(langs, langDE) {
		t.Fatalf("Languages()=%v, want the added language among them", langs)
	}
	if want := []Lang{LangEN, LangRU, langDE}; !slices.Equal(langs, want) {
		t.Errorf("Languages()=%v, want the built-in ones first and the file's after: %v", langs, want)
	}
	offered := switcher(httptest.NewRequest(http.MethodGet, "/", nil), LangEN)
	if !slices.ContainsFunc(offered, func(l langLink) bool {
		return l.Name == "Deutsch" && strings.Contains(l.URL, "lang=de")
	}) {
		t.Errorf("the switcher offers %+v, want the added language under the name it calls itself", offered)
	}

	// Asking for it has to work as well, or the link in the switcher leads back
	// to English.
	if got := pickLang(httptest.NewRequest(http.MethodGet, "/?lang=de", nil)); got != langDE {
		t.Errorf("pickLang=%q, want the added language", got)
	}
}

func TestLoadTranslations_LetsAFileReplaceABuiltInPhrase(t *testing.T) {
	// One line somebody disagreed with is one line they should be able to
	// change, and changing it must not cost them the rest of the language.
	added, incomplete, err := loadInto(t, translationDir(t, map[string]string{
		"ru.json": `{"jobs.title": "Задачи"}`,
	}))
	if err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}
	if len(added) != 0 {
		t.Errorf("added=%v, want nothing: the file names a language this program already has", added)
	}
	if len(incomplete) != 0 {
		t.Errorf("incomplete=%v, want nothing: what the file leaves out this program already says", incomplete)
	}
	if got := LangRU.T("jobs.title"); got != "Задачи" {
		t.Errorf("T(jobs.title)=%q, want what the file says", got)
	}

	// The rest of the language is the whole of this test. A file read as a
	// replacement rather than as a correction would leave every other phrase
	// speaking English.
	if got, want := LangRU.T("jobs.none"), catalogue[LangRU]["jobs.none"]; got != want {
		t.Errorf("T(jobs.none)=%q, want the built-in %q: one changed phrase erased the others", got, want)
	}
	if got := catalogue[LangRU]["jobs.title"]; got != "Задания" {
		t.Errorf("the built-in catalogue now reads %q; a file must not edit this program's own text", got)
	}
}

func TestLoadTranslations_KeepsRunningWhenAFileIsShortOfKeys(t *testing.T) {
	// The difference this task turns on. A language of this repository's own
	// that is short of text stops the server; a language somebody dropped in a
	// directory is their data, and data does not get to stop a program.
	if _, _, err := loadInto(t, translationDir(t, map[string]string{"de.json": shortGerman})); err != nil {
		t.Fatalf("a file short of keys was refused: %v", err)
	}
	s, err := New(Config{Store: testStore(t), Logger: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := get(t, s, jobsAt+"?lang=de")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the job list in the added language gave %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Aufträge") {
		t.Error("the page is drawn without a phrase the file says")
	}
	for key := range catalogue[LangEN] {
		if strings.Contains(body, key) {
			t.Errorf("the page shows the key %q where a phrase belongs", key)
		}
	}
}

func TestLoadTranslations_NamesWhatAFileIsMissingRatherThanHidingIt(t *testing.T) {
	// A translation nobody is told is unfinished stays unfinished. This list is
	// the only account of it anybody gets.
	_, incomplete, err := loadInto(t, translationDir(t, map[string]string{"de.json": shortGerman}))
	if err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}
	missing := incomplete[langDE]
	if len(missing) == 0 {
		t.Fatal("nothing is reported missing from a file that says three phrases out of a catalogue")
	}
	if !slices.Contains(missing, filledKey) {
		t.Errorf("%q is not among the missing, and the file does not say it", filledKey)
	}
	if slices.Contains(missing, spokenKey) {
		t.Errorf("%q is reported missing, and the file says it", spokenKey)
	}
	if !slices.IsSorted(missing) {
		t.Error("the missing keys are in no order, so the same directory reads differently each time")
	}
}

func TestLoadTranslations_FallsBackToEnglishForWhatAFileDoesNotSay(t *testing.T) {
	// A key on a page is this repository's shorthand and means nothing to a
	// reader; an empty string is invisible and means less. The English is a
	// sentence somebody can act on.
	if _, _, err := loadInto(t, translationDir(t, map[string]string{"de.json": shortGerman})); err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}
	got := langDE.T(filledKey)
	if want := catalogue[LangEN][filledKey]; got != want {
		t.Errorf("T(%q)=%q, want the English %q", filledKey, got, want)
	}
	if got == "" || got == filledKey {
		t.Errorf("T(%q)=%q, which is nothing a reader can read", filledKey, got)
	}
}

func TestLoadTranslations_FallsBackToTheEnglishThisMachineShows(t *testing.T) {
	// English is itself a language a file can correct, and what fills in for a
	// half-finished translation is the English the reader would have seen — not
	// the one this program was built with. Both files are in one directory, and
	// the German is read before the English is corrected, so this holds only
	// because the filling in waits until every file has had its say.
	if _, _, err := loadInto(t, translationDir(t, map[string]string{
		"de.json": shortGerman,
		"en.json": `{"` + filledKey + `": "Assignment"}`,
	})); err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}
	if got := LangEN.T(filledKey); got != "Assignment" {
		t.Fatalf("T(%q) in English =%q, want what the file says", filledKey, got)
	}
	if got := langDE.T(filledKey); got != "Assignment" {
		t.Errorf("T(%q) in the added language =%q, want the English this machine shows", filledKey, got)
	}
}

func TestLoadTranslations_IgnoresABlankPhraseRatherThanShowingNothing(t *testing.T) {
	// A key with nothing after it is a translator part way through a line. It
	// is not a phrase, and drawn on a page it is a button with no words on it.
	_, incomplete, err := loadInto(t, translationDir(t, map[string]string{
		"de.json": `{"jobs.title": "   ", "jobs.none": "Noch keine Aufträge."}`,
		"ru.json": `{"jobs.title": ""}`,
	}))
	if err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}
	if got, want := langDE.T("jobs.title"), catalogue[LangEN]["jobs.title"]; got != want {
		t.Errorf("T(jobs.title) in the added language =%q, want the English %q", got, want)
	}
	if !slices.Contains(incomplete[langDE], "jobs.title") {
		t.Error("a blank phrase is counted as said, so nobody is told it is missing")
	}
	if got, want := LangRU.T("jobs.title"), catalogue[LangRU]["jobs.title"]; got != want {
		t.Errorf("T(jobs.title)=%q, want the built-in %q kept", got, want)
	}
}

func TestLoadTranslations_SkipsADamagedFileAndSaysWhich(t *testing.T) {
	// One unreadable file must not cost the reader the languages that were
	// readable, and it must not go by in silence either: from inside the
	// browser a language that was never loaded looks exactly like one nobody
	// wrote.
	added, _, err := loadInto(t, translationDir(t, map[string]string{
		"de.json": `{"jobs.title": "Aufträge"`,
		"fr.json": `{"lang.name": "Français", "jobs.title": "Travaux"}`,
	}))
	if !errors.Is(err, ErrUnreadableTranslation) {
		t.Fatalf("LoadTranslations gave %v, want a complaint about the damaged file", err)
	}
	if !strings.Contains(err.Error(), "de.json") {
		t.Errorf("the complaint does not name the file: %v", err)
	}
	if !slices.Equal(added, []Lang{Lang("fr")}) {
		t.Errorf("added=%v, want the file that could be read and not the one that could not", added)
	}
	if slices.Contains(Languages(), langDE) {
		t.Error("a language was added out of a file that could not be read")
	}
	if got := langDE.T(spokenKey); got != spokenKey {
		t.Errorf("T(%q) in a language nobody has =%q, want the key back", spokenKey, got)
	}
}

func TestLoadTranslations_TreatsAFileWithNothingInItAsOneItCannotRead(t *testing.T) {
	// A file of no bytes is a copy that never finished or an editor that never
	// saved, and a file whose whole content is a null is what a tool writes when
	// it had nothing to write. Taken as a language either one would add a
	// switcher entry that is English throughout and says nothing about why.
	added, _, err := loadInto(t, translationDir(t, map[string]string{
		"de.json": "",
		"nl.json": "null",
	}))
	if !errors.Is(err, ErrUnreadableTranslation) {
		t.Fatalf("LoadTranslations gave %v, want a complaint about both files", err)
	}
	for _, name := range []string{"de.json", "nl.json"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the complaint does not name %s: %v", name, err)
		}
	}
	if len(added) != 0 {
		t.Errorf("added=%v, want nothing out of a file that says nothing", added)
	}
	if got, want := LangRU.T("jobs.title"), catalogue[LangRU]["jobs.title"]; got != want {
		t.Errorf("T(jobs.title)=%q, want the built-in %q untouched", got, want)
	}
}

func TestLoadTranslations_RefusesAFileLargerThanEverythingItSays(t *testing.T) {
	// The directory is somebody else's, and a program that reads whatever it is
	// pointed at can be pointed at a disk.
	huge := `{"jobs.title": "` + strings.Repeat("a", maxTranslationFile) + `"}`
	added, _, err := loadInto(t, translationDir(t, map[string]string{"de.json": huge}))
	if !errors.Is(err, ErrUnreadableTranslation) {
		t.Fatalf("LoadTranslations gave %v, want a complaint about the size", err)
	}
	if len(added) != 0 {
		t.Errorf("added=%v, want nothing out of a file that was not read", added)
	}
}

func TestLoadTranslations_ReadsOnlyTheNamesThatAreLanguageCodes(t *testing.T) {
	// The name of the file decides which language it is, and the name comes
	// from a directory this program does not own. Only a bare code is read, so
	// nothing in that directory can name a file anywhere else.
	dir := translationDir(t, map[string]string{
		"deutsch.json": shortGerman,
		"de-DE.json":   shortGerman,
		"DE.json":      shortGerman,
		"..json":       shortGerman,
		".json":        shortGerman,
		"de.txt":       shortGerman,
		"de.json.bak":  shortGerman,
	})
	// A directory named like a translation is not one either.
	if err := os.MkdirAll(filepath.Join(dir, "nl.json"), 0o755); err != nil {
		t.Fatalf("making the directory named like a translation: %v", err)
	}

	added, incomplete, err := loadInto(t, dir)
	if err != nil {
		t.Fatalf("LoadTranslations complained about files it should have passed over: %v", err)
	}
	if len(added) != 0 || len(incomplete) != 0 {
		t.Errorf("added=%v incomplete=%v, want nothing read out of any of those names", added, incomplete)
	}
	if langs := Languages(); !slices.Equal(langs, []Lang{LangEN, LangRU}) {
		t.Errorf("Languages()=%v, want only the built-in ones", langs)
	}
}

func TestTranslationName_TakesOnlyABareCodeAndNothingThatIsAPath(t *testing.T) {
	// The guard above, at the one line it lives on: every way of naming a file
	// outside the directory needs a character this refuses.
	read := map[string]Lang{"en.json": LangEN, "de.json": langDE, "ces.json": "ces"}
	for name, want := range read {
		if code, ok := translationName(name); !ok || Lang(code) != want {
			t.Errorf("translationName(%q)=%q,%v, want %q", name, code, ok, want)
		}
	}
	for _, name := range []string{
		"..", "../en.json", "en/../../en.json", `..\en.json`, "/etc/en.json",
		".json", "e.json", "abcd.json", "en.JSON", "en", "en.json.json",
	} {
		if code, ok := translationName(name); ok {
			t.Errorf("translationName(%q) read a language %q out of it", name, code)
		}
	}
}

func TestLoadTranslations_WorksWhenThereIsNoDirectoryAtAll(t *testing.T) {
	// Nearly every copy of this program runs with nothing beside it, and that
	// is the ordinary case rather than a fault: both built-in languages are
	// already in the binary.
	added, incomplete, err := loadInto(t, filepath.Join(t.TempDir(), "nothing-of-the-sort"))
	if err != nil {
		t.Fatalf("a directory that is not there was called a fault: %v", err)
	}
	if len(added) != 0 || len(incomplete) != 0 {
		t.Errorf("added=%v incomplete=%v, want nothing from nowhere", added, incomplete)
	}
	if langs := Languages(); !slices.Equal(langs, []Lang{LangEN, LangRU}) {
		t.Errorf("Languages()=%v, want the two built in", langs)
	}
	if got, want := LangRU.T("jobs.title"), catalogue[LangRU]["jobs.title"]; got != want {
		t.Errorf("T(jobs.title)=%q, want %q", got, want)
	}
}

func TestLoadTranslations_SaysWhatTheDirectoryHoldsNowAndNotWhatItHeld(t *testing.T) {
	// Read twice, the second reading is the answer. A language whose file was
	// taken away that went on being offered would be one nobody could remove.
	if _, _, err := loadInto(t, translationDir(t, map[string]string{"de.json": shortGerman})); err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}
	if _, _, err := loadInto(t, translationDir(t, nil)); err != nil {
		t.Fatalf("LoadTranslations of an empty directory: %v", err)
	}
	if langs := Languages(); !slices.Equal(langs, []Lang{LangEN, LangRU}) {
		t.Errorf("Languages()=%v, want the language whose file is gone to be gone", langs)
	}
}

func TestLanguages_CannotBeReorderedByWhoeverAsksForIt(t *testing.T) {
	// The switcher is built from this list, and a caller sorting its own copy
	// would otherwise reorder the switcher for every reader afterwards.
	first := Languages()
	first[0], first[1] = first[1], first[0]
	if Languages()[0] != LangEN {
		t.Error("the order of the languages was changed by a caller")
	}
}

func TestLoadTranslations_OffersALanguageThatDoesNotNameItselfByItsCode(t *testing.T) {
	// The switcher is read by somebody who cannot read the page they are on. A
	// file that never says what its language is called leaves the code, which
	// is what its reader would have typed to ask for it, and not the name of
	// some other language.
	added, _, err := loadInto(t, translationDir(t, map[string]string{
		"nl.json": `{"jobs.title": "Taken"}`,
	}))
	if err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}
	if !slices.Equal(added, []Lang{Lang("nl")}) {
		t.Fatalf("added=%v, want the one language the directory holds", added)
	}
	if got := Lang("nl").Name(); got != "nl" {
		t.Errorf("Name()=%q, want the code of a language that does not name itself", got)
	}
}

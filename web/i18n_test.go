// SPDX-License-Identifier: MIT

package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestLanguages_CannotBeReorderedByWhoeverAsksForIt(t *testing.T) {
	// The switcher is built from this list, and a caller sorting its own copy
	// would otherwise reorder the switcher for every reader afterwards.
	first := Languages()
	first[0], first[1] = first[1], first[0]
	if Languages()[0] != LangEN {
		t.Error("the order of the languages was changed by a caller")
	}
}

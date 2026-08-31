// SPDX-License-Identifier: MIT

package google

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

func TestClassify_RealPagesAreSERPs(t *testing.T) {
	for _, name := range []string{
		"serp_goto_ru.html", "serp_cards_ru.html", "serp_site_ru.html",
		"serp_ads_us.html", "serp_direct_us.html",
	} {
		got, err := Classify(200, "https://www.google.com/search?q=x", fixture(t, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got != ClassSERP {
			t.Errorf("%s classified as %q, want %q", name, got, ClassSERP)
		}
		if !got.Usable() {
			t.Errorf("%s: a SERP reports itself unusable", name)
		}
	}
}

func TestClassify_TheJavaScriptShellIsNotAnEmptyResult(t *testing.T) {
	// This is the whole reason classification exists. Google answers a plain
	// HTTP client with a valid HTTP 200 page that contains no results and
	// never will. Reported as "0 results" it is indistinguishable from an
	// honest empty answer, and a rank tracker would record a lost position
	// that never moved.
	got, err := Classify(200, "https://www.google.com/search?q=x", fixture(t, "jsshell.html"))
	if !errors.Is(err, ErrNotSERP) {
		t.Fatalf("err=%v, want ErrNotSERP", err)
	}
	if got != ClassShell {
		t.Errorf("class=%q, want %q", got, ClassShell)
	}
	if got.Usable() {
		t.Error("the shell reports itself usable")
	}
}

func TestClassify_ResultsWinOverWallMarkers(t *testing.T) {
	// Order is load-bearing and was established by measurement: checking
	// wall markers before results declared a page carrying real results to
	// be a wall. The body below carries an actual wallMarkers phrase — not a
	// string the classifier ignores — so this test fails if the order is
	// ever reversed.
	body := append([]byte(`<html><body><p>our systems have detected unusual traffic</p>`),
		append(fixture(t, "serp_goto_ru.html"), []byte(`</body></html>`)...)...)
	got, err := Classify(200, "https://www.google.com/search?q=x", body)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got != ClassSERP {
		t.Errorf("class=%q, want %q — a page with results is never a wall", got, ClassSERP)
	}
}

func TestClassify_SorryScriptDoesNotShadowAGenuineEmptyAnswer(t *testing.T) {
	// "/sorry/index" turns up in Google's own inline JavaScript on ordinary
	// pages, not only on walls — measured on 17 of 17 real captures. A true
	// zero on a site: query must still come out as ClassEmpty, never ClassWall,
	// even with that string sitting right there in a script tag.
	body := []byte(`<html><body><script>var u="/sorry/index?continue=x";</script>` +
		`<div id="search"><div id="rso"></div>` +
		`<p>По запросу <em>site:blanktrail.com</em> ничего не найдено.</p></div></body></html>`)
	got, err := Classify(200, "https://www.google.com/search?q=site:blanktrail.com", body)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got != ClassEmpty {
		t.Errorf("class=%q, want %q", got, ClassEmpty)
	}
}

func TestClassify_SorryPathIsAWall(t *testing.T) {
	got, err := Classify(200, "https://www.google.com/sorry/index?continue=x", []byte("<html></html>"))
	if !errors.Is(err, ErrNotSERP) {
		t.Fatalf("err=%v, want ErrNotSERP", err)
	}
	if got != ClassWall {
		t.Errorf("class=%q, want %q", got, ClassWall)
	}
}

func TestClassify_StatusCodesMapToTheirOwnClasses(t *testing.T) {
	cases := []struct {
		status int
		want   Class
	}{
		{429, ClassWall},
		{403, ClassBanned},
		{503, ClassHTTP},
		{404, ClassHTTP},
	}
	for _, tc := range cases {
		got, err := Classify(tc.status, "https://www.google.com/search?q=x", []byte("<html></html>"))
		if !errors.Is(err, ErrNotSERP) {
			t.Errorf("status %d: err=%v, want ErrNotSERP", tc.status, err)
		}
		if got != tc.want {
			t.Errorf("status %d classified as %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestClassify_AnHonestEmptyAnswerIsUsable(t *testing.T) {
	// Google genuinely finding nothing is a successful capture, not a
	// failure, and must not be retried.
	body := []byte(`<html><body><div id="search"><div id="rso"></div>` +
		`<p>Your search - <b>zzqqxx</b> - did not match any documents.</p></div></body></html>`)
	got, err := Classify(200, "https://www.google.com/search?q=zzqqxx", body)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got != ClassEmpty {
		t.Errorf("class=%q, want %q", got, ClassEmpty)
	}
	if !got.Usable() {
		t.Error("an honest empty answer reports itself unusable")
	}
}

func TestClassify_TheOtherEnglishWordingIsAlsoAnEmptyAnswer(t *testing.T) {
	// A missed empty marker degrades to ClassShell, which is retried forever
	// against a page that will never change its mind.
	body := []byte(`<html><body><div id="search"><div id="rso"></div>` +
		`<p>No results found for <b>zzqqxx</b>.</p></div></body></html>`)
	got, err := Classify(200, "https://www.google.com/search?q=zzqqxx", body)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got != ClassEmpty {
		t.Errorf("class=%q, want %q", got, ClassEmpty)
	}
}

func TestClassOf_ReadsTheClassOutOfAWrappedError(t *testing.T) {
	// A caller decides from the class whether the query is worth asking again
	// and over which transport. An error travels up through wrapping, so the
	// class has to survive it; recovering it from the message instead would tie
	// that decision to the wording of the message.
	inner := &ResponseError{Class: ClassShell, Query: "iphone 13", op: "search"}
	wrapped := fmt.Errorf("google: page %d of %q: %w", 2, "iphone 13", inner)

	got, ok := ClassOf(wrapped)
	if !ok {
		t.Fatal("the class was not recoverable from a wrapped error")
	}
	if got != ClassShell {
		t.Errorf("class=%q, want %q", got, ClassShell)
	}
}

func TestClassOf_SaysNothingForAnErrorThatCarriesNoClass(t *testing.T) {
	// A request that never arrived was never classified, and a caller that took
	// the zero Class for an answer would be branching on a class nothing
	// measured.
	if got, ok := ClassOf(errors.New("dial tcp: connection refused")); ok {
		t.Errorf("ClassOf reported %q for an error that carries no class", got)
	}
}

func TestClass_SurvivesBeingWrittenDownAsTextAndReadBack(t *testing.T) {
	// A history keeps text, not values. Whatever wrote the class into the
	// message and whatever reads it back are one pair, and they are both in this
	// package so that rewording one without the other fails here rather than in
	// a caller that never knew the wording was load-bearing.
	//
	// Every class is put through, because a marker that survives one wording and
	// not another is a screen that quietly loses a whole kind of answer.
	for _, class := range []Class{ClassSERP, ClassEmpty, ClassShell, ClassWall, ClassBanned, ClassHTTP} {
		written := (&ResponseError{
			Class: class,
			Query: "iphone 13",
			op:    "search",
			err:   fmt.Errorf("%w: something", ErrNotSERP),
		}).Error()

		got, ok := ClassInText(written)
		if !ok {
			t.Errorf("the class did not survive being written down: %q", written)
			continue
		}
		if got != class {
			t.Errorf("%q reads back as class %q, want %q", written, got, class)
		}
	}
}

func TestClass_IsReadFromWhereItWasWrittenAndNotFromTheQuery(t *testing.T) {
	// The query is a reader's own text and may say anything at all, including
	// the very words this marker is written in. A reader that took the first one
	// it found would report the class somebody typed into a search box.
	// The query carries the marker whole, spacing included. A decoy that differs
	// from the real marker by so much as the space in front of it is one a reader
	// walking from the left would step over, and the test would pass on a reader
	// that reads the wrong end of the message.
	written := (&ResponseError{
		Class: ClassWall,
		Query: "what does" + classMark + "serp) mean",
		op:    "search",
		err:   fmt.Errorf("%w: something", ErrNotSERP),
	}).Error()
	if strings.Index(written, classMark) == strings.LastIndex(written, classMark) {
		t.Fatalf("the decoy is not in %q, so nothing here has two places to read from", written)
	}

	got, ok := ClassInText(written)
	if !ok {
		t.Fatalf("no class was read out of %q", written)
	}
	if got != ClassWall {
		t.Errorf("%q reads back as class %q, want %q — the query was read as the class", written, got, ClassWall)
	}
}

func TestClass_IsNotInventedForTextThatCarriesNone(t *testing.T) {
	// Nothing arrived, so nothing was classified. A reader that answered with an
	// empty class would file a request that never completed under a kind of
	// response, and a word nobody measured would be reported as one that was.
	for _, message := range []string{
		"run: recording \"iphone 13\": dial tcp: connection refused",
		"google: search \"x\": (class ",
		"google: search \"x\": (class nonsense)",
		"",
	} {
		if got, ok := ClassInText(message); ok {
			t.Errorf("%q was read as class %q, and it carries none", message, got)
		}
	}
}

func TestResponseError_StaysMatchableAsNotASERP(t *testing.T) {
	// Callers written before the class existed branch on ErrNotSERP. Adding a
	// type must not take that away from them.
	err := &ResponseError{Class: ClassWall, Query: "x", op: "search", err: fmt.Errorf("%w: challenge page", ErrNotSERP)}
	if !errors.Is(err, ErrNotSERP) {
		t.Error("a response error no longer matches ErrNotSERP")
	}
}

func TestClassify_AnOrdinaryRussianPhraseIsNotAnEmptyAnswer(t *testing.T) {
	// "по запросу" is an everyday Russian phrase — "on request", "by query" —
	// and carries no claim that nothing was found. As an empty marker it would
	// turn any resultless page that happens to use the words into a recorded
	// zero, and ClassEmpty is Usable, so that zero is kept and never retried.
	// A false Empty is the most expensive misclassification in the set.
	body := []byte(`<html><body><div id="search"><div id="rso"></div>` +
		`<p>Документы предоставляются по запросу пользователя.</p></div></body></html>`)
	got, err := Classify(200, "https://www.google.com/search?q=x", body)
	if got == ClassEmpty {
		t.Errorf("class=%q — an ordinary phrase was recorded as a genuine zero", got)
	}
	if !errors.Is(err, ErrNotSERP) {
		t.Errorf("err=%v, want ErrNotSERP so the capture is retried", err)
	}
}

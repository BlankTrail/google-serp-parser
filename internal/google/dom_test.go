// SPDX-License-Identifier: MIT

package google

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func frag(t *testing.T, h string) *goquery.Selection {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(h))
	if err != nil {
		t.Fatalf("parse fragment: %v", err)
	}
	return doc.Find("body").Children().First()
}

func TestOwnText_IgnoresNestedElements(t *testing.T) {
	// A snippet is the text a container holds itself. Taking all descendant
	// text would swallow the title and the breadcrumb into every snippet.
	s := frag(t, `<div>outer <span>inner</span> tail</div>`)
	if got := ownText(s); got != "outer tail" {
		t.Errorf("ownText=%q, want %q", got, "outer tail")
	}
}

func TestFirstHeading_AcceptsBothLayouts(t *testing.T) {
	// Two layouts are in the wild. One puts the title in an h3; the other, in
	// an element carrying role=heading and no h3 anywhere on the page. A
	// parser that knows only the first silently returns zero results on the
	// second — measured, not hypothetical.
	withH3 := frag(t, `<a><h3>  Title  one </h3></a>`)
	got, viaRole := firstHeading(withH3)
	if got != "Title one" || viaRole {
		t.Errorf("h3 layout: got %q viaRole=%v, want %q false", got, viaRole, "Title one")
	}

	withRole := frag(t, `<a><div role="heading" aria-level="3">Card title</div></a>`)
	got, viaRole = firstHeading(withRole)
	if got != "Card title" || !viaRole {
		t.Errorf("card layout: got %q viaRole=%v, want %q true", got, viaRole, "Card title")
	}

	if got, _ := firstHeading(frag(t, `<a><span>no heading</span></a>`)); got != "" {
		t.Errorf("no heading: got %q, want empty", got)
	}
}

func TestCiteHost_RejectsACiteThatIsNotAnAddress(t *testing.T) {
	// The cite slot is reused for rich-snippet metadata. Roughly one in
	// twenty carries a line like "20+ comments · 2 years ago"; taking it as a
	// host would invent a domain that does not exist.
	ok := frag(t, `<div><cite>https://habr.com › ru › articles</cite></div>`)
	host, path, valid := citeHost(ok)
	if !valid || host != "habr.com" {
		t.Errorf("host=%q valid=%v, want habr.com true", host, valid)
	}
	if path != "ru › articles" {
		t.Errorf("path=%q, want the displayed breadcrumb", path)
	}

	notAURL := frag(t, `<div><cite>Комментариев: более 20 · 2 года назад</cite></div>`)
	if _, _, valid := citeHost(notAURL); valid {
		t.Error("a metadata line was accepted as a host")
	}

	none := frag(t, `<div><span>no cite here</span></div>`)
	if _, _, valid := citeHost(none); valid {
		t.Error("a container with no cite reported a host")
	}
}

func TestIsAd_RecognisesEveryAdContainer(t *testing.T) {
	// Ads carry the same heading markup as organic results, so without this
	// they are counted as organic — which inflates every ranking by the
	// number of ads above it.
	for _, wrapper := range []string{
		`<div id="tads"><div data-text-ad="1"><a><h3>x</h3></a></div></div>`,
		`<div id="bottomads"><div data-text-ad="1"><a><h3>x</h3></a></div></div>`,
		`<div id="rhs"><a href="/aclk?x"><h3>x</h3></a></div>`,
	} {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(wrapper))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		link := doc.Find("a").First()
		if !isAd(link) {
			t.Errorf("link inside %.30s… not recognised as an ad", wrapper)
		}
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(
		`<div id="rso"><div><a href="https://example.com"><h3>x</h3></a></div></div>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if isAd(doc.Find("a").First()) {
		t.Error("an organic result was classified as an ad")
	}
}

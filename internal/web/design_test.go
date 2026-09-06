// SPDX-License-Identifier: MIT

package web

import (
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"testing"
)

// mustAsset hands back one of the files built into the binary, as text.
func mustAsset(t *testing.T, name string) string {
	t.Helper()
	body, err := fs.ReadFile(assets, "assets/"+name)
	if err != nil {
		t.Fatalf("reading the embedded %s: %v", name, err)
	}
	return string(body)
}

// declaration is one line of a stylesheet: the rule it stands in, what it sets,
// and what it sets it to.
//
// The stylesheet is read by walking its lines rather than by matching a pattern
// against it, because nothing in this repository matches text with regular
// expressions. That costs one convention: a selector opens on its own line, a
// closing brace stands on its own line, and one declaration is one line. The
// stylesheet is written that way, and a line the walk cannot read is a line it
// reports rather than skips.
type declaration struct {
	Selector string
	Property string
	Value    string
}

// withoutComments drops every comment so a colour named in prose is not read as
// a colour written into a rule.
func withoutComments(css string) string {
	var kept strings.Builder
	for {
		opened := strings.Index(css, "/*")
		if opened < 0 {
			kept.WriteString(css)
			return kept.String()
		}
		kept.WriteString(css[:opened])
		closed := strings.Index(css[opened:], "*/")
		if closed < 0 {
			return kept.String()
		}
		css = css[opened+closed+len("*/"):]
	}
}

// declarationsIn reads every declaration of a stylesheet, each carrying the
// selectors it is nested under joined into one string, so a test can ask which
// rule a colour was written into.
func declarationsIn(css string) []declaration {
	var open []string
	var out []declaration
	for _, raw := range strings.Split(withoutComments(css), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			continue
		case strings.HasSuffix(line, "{"):
			open = append(open, strings.TrimSpace(strings.TrimSuffix(line, "{")))
			continue
		case line == "}":
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
			continue
		}
		// A selector list runs over several lines, each ending in a comma. Those
		// lines are gathered onto the one that opens the block.
		if strings.HasSuffix(line, ",") && len(open) == 0 {
			continue
		}
		property, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out = append(out, declaration{
			Selector: strings.Join(open, " "),
			Property: strings.TrimSpace(property),
			Value:    strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), ";")),
		})
	}
	return out
}

// stylesheet is every declaration of the one stylesheet this program ships.
//
// It refuses an implausibly short answer. A walk that quietly read nothing
// would let every test built on it pass over an empty list, which is the shape
// of a test that cannot fail.
func stylesheet(t *testing.T) []declaration {
	t.Helper()
	decls := declarationsIn(mustAsset(t, "static/app.css"))
	if len(decls) < 40 {
		t.Fatalf("the stylesheet was read as %d declarations, which is not a stylesheet", len(decls))
	}
	return decls
}

// namedColours are the words CSS reads as colours. The list is the ones a
// stylesheet plausibly reaches for rather than every one the standard names:
// what it has to catch is somebody typing a colour into a rule, and nobody
// types lightgoldenrodyellow by accident.
var namedColours = map[string]bool{
	"aqua": true, "azure": true, "beige": true, "black": true, "blue": true,
	"brown": true, "coral": true, "crimson": true, "cyan": true, "fuchsia": true,
	"gold": true, "gray": true, "green": true, "grey": true, "indigo": true,
	"ivory": true, "khaki": true, "lavender": true, "lime": true, "magenta": true,
	"maroon": true, "navy": true, "olive": true, "orange": true, "orchid": true,
	"pink": true, "plum": true, "purple": true, "red": true, "salmon": true,
	"silver": true, "teal": true, "tomato": true, "turquoise": true,
	"violet": true, "white": true, "yellow": true,
}

// colourIn says whether a value writes a colour out, and which one.
func colourIn(value string) (string, bool) {
	lower := strings.ToLower(value)
	for _, written := range []string{"#", "rgb(", "rgba(", "hsl(", "hsla(", "oklch(", "oklab(", "color-mix("} {
		if strings.Contains(lower, written) {
			return written, true
		}
	}
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return r == ' ' || r == ',' || r == '(' || r == ')' || r == '/'
	})
	for _, word := range fields {
		if namedColours[word] {
			return word, true
		}
	}
	return "", false
}

func TestStyles_WriteEveryColourWhereItIsNamedAndNowhereElse(t *testing.T) {
	// A rule that writes a colour where it is used is a rule the dark block
	// cannot reach, and it is the one that glares. Colours are named once and
	// taken from there, which is what makes dark mode a consequence rather than
	// a second stylesheet.
	var named int
	for _, d := range stylesheet(t) {
		if strings.HasPrefix(d.Property, "--") {
			if _, ok := colourIn(d.Value); ok {
				named++
			}
			continue
		}
		if written, ok := colourIn(d.Value); ok {
			t.Errorf("%s sets %s to a colour written out as %q", d.Selector, d.Property, written)
		}
	}
	if named == 0 {
		t.Error("the stylesheet names no colour at all, so this test read nothing")
	}
}

// colourVariablesIn is every custom property holding a colour that is declared
// under the given selector, by name and by value.
func colourVariablesIn(decls []declaration, selector string) map[string]string {
	found := map[string]string{}
	for _, d := range decls {
		if d.Selector != selector || !strings.HasPrefix(d.Property, "--") {
			continue
		}
		if _, ok := colourIn(d.Value); ok {
			found[d.Property] = d.Value
		}
	}
	return found
}

func TestStyles_GiveEveryColourAValueForTheDarkAsWellAsTheLight(t *testing.T) {
	// Dark mode is a consequence of taking colours from variables, not a second
	// stylesheet. A variable that exists in light and not in dark is the one
	// that will glare, because it goes on holding its light value on a page
	// where everything around it has gone dark.
	//
	// Only the colours are asked for. A radius does not change when the lights
	// go out, and demanding a dark value for one would be demanding noise.
	decls := stylesheet(t)
	light := colourVariablesIn(decls, ":root")
	dark := colourVariablesIn(decls, ":root[data-theme=\"dark\"]")
	if len(light) == 0 {
		t.Fatal("no colour is named in the light block, so this test read nothing")
	}
	if len(dark) == 0 {
		t.Fatal("there is no dark block, so every colour holds its light value in the dark")
	}
	for name, value := range light {
		darkValue, ok := dark[name]
		if !ok {
			t.Errorf("%s has no dark value", name)
			continue
		}
		if darkValue == value {
			t.Errorf("%s is the same colour in the dark as in the light", name)
		}
	}
}

func TestStyles_LeaveTheThemeToTheReaderRatherThanToTheirMachine(t *testing.T) {
	// The interface is light, and the dark one is what somebody asked for. A
	// stylesheet that reads the machine's own preference decides this for a
	// reader who never said, and overrules one who did: an operator on a dark
	// desktop had no way of reading this program in the light.
	//
	// The whole of that rule is that the dark block is reached by an attribute
	// the server writes and by nothing else, so what is checked is that nothing
	// in the stylesheet asks the machine at all.
	for _, d := range stylesheet(t) {
		if strings.Contains(d.Selector, "prefers-color-scheme") {
			t.Errorf("%s sets %s from what the machine prefers, and the theme is the reader's to choose",
				d.Selector, d.Property)
		}
	}
}

func TestStyles_FillOnlyTheProgressBarWithTheAccent(t *testing.T) {
	// One accent per screen: exactly one thing is filled with colour, and it is
	// the bar. A coloured button beside it means two accents, and two accents
	// mean the eye is told nothing by either.
	var uses int
	for _, d := range stylesheet(t) {
		if strings.HasPrefix(d.Property, "--") || !strings.Contains(d.Value, "var(--accent") {
			continue
		}
		uses++
		if !strings.Contains(d.Selector, "progress-bar") {
			t.Errorf("%s fills %s with the accent, which belongs to the bar alone", d.Selector, d.Property)
		}
	}
	if uses == 0 {
		t.Error("nothing on any page is filled with the accent, so there is no accent")
	}
}

// resolved is a value with any single var() in it replaced by what the light
// block declares that variable to be, so a test can compare two sizes that are
// both written as names.
func resolved(decls []declaration, value string) string {
	name, rest, ok := strings.Cut(value, "var(")
	if !ok || strings.TrimSpace(name) != "" {
		return value
	}
	name, _, _ = strings.Cut(rest, ")")
	for _, d := range decls {
		if d.Selector == ":root" && d.Property == strings.TrimSpace(name) {
			return d.Value
		}
	}
	return value
}

func TestStyles_DrawTheFigureLargerThanTheLabelUnderIt(t *testing.T) {
	// The number is what a card is looked at for and has to be readable across
	// a desk; the label names it and must not compete. A card whose label is as
	// loud as its figure is a card that has to be read rather than glanced at.
	decls := stylesheet(t)
	sizes := map[string]float64{}
	for _, d := range decls {
		if d.Property != "font-size" {
			continue
		}
		if d.Selector != ".figure" && d.Selector != ".label" {
			continue
		}
		size := resolved(decls, d.Value)
		px, err := strconv.ParseFloat(strings.TrimSuffix(size, "px"), 64)
		if err != nil {
			t.Fatalf("%s is sized %q, which is not a size in pixels this test can compare", d.Selector, size)
		}
		sizes[d.Selector] = px
	}
	if len(sizes) != 2 {
		t.Fatalf("the figure and its label are not both sized in the stylesheet: %v", sizes)
	}
	if sizes[".figure"] <= sizes[".label"] {
		t.Errorf("the figure is %gpx and its label %gpx, so the label competes with the number",
			sizes[".figure"], sizes[".label"])
	}
}

// embeddedFiles is every file this program ships inside itself.
func embeddedFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(assets, "assets", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := fs.ReadFile(assets, name)
		if err != nil {
			return err
		}
		files[name] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded files: %v", err)
	}
	if len(files) < 6 {
		t.Fatalf("only %d files are built into this binary, which is not the interface", len(files))
	}
	return files
}

func TestInterface_HoldsItsStructureWithHairlinesAndAirRatherThanShadows(t *testing.T) {
	// Separation is by half-tone and distance. A shadow or a gradient is depth
	// drawn on a screen that has none, and it is what turns a plain interface
	// into a dated one.
	//
	// Every embedded file is read, not the stylesheet alone: a shadow written
	// into a page's own markup would ship exactly as far as one written here.
	for name, body := range embeddedFiles(t) {
		for _, drawn := range []string{"shadow", "gradient"} {
			if strings.Contains(strings.ToLower(body), drawn) {
				t.Errorf("%s draws a %s", name, drawn)
			}
		}
	}
}

func TestPages_LeaveEveryAppearanceToTheOneStylesheet(t *testing.T) {
	// A page that declares its own colours and distances has left the visual
	// language within a month, and nothing says it has. There is one stylesheet
	// and the pages carry class names into it.
	for name, body := range embeddedFiles(t) {
		if path.Ext(name) != ".html" {
			continue
		}
		for _, own := range []string{`style="`, "<style"} {
			if strings.Contains(body, own) {
				t.Errorf("%s carries appearance of its own in %s", name, own)
			}
		}
	}
}

// resourcesOf is every address a page tells the browser to fetch on its own.
//
// The line is drawn between what the browser fetches without being asked and
// what it fetches because somebody clicked. A stylesheet, a script and an
// @import are the page's own furniture and have to come out of this binary; the
// href of an <a> is a search result, and a search result is an address on
// somebody else's machine by definition. Drawing the line by position instead —
// the markup against the table of results — would hold only until the first
// result rendered outside a table.
func resourcesOf(body string) []string {
	var fetched []string
	for _, after := range strings.Split(body, "<")[1:] {
		tag, _, _ := strings.Cut(after, ">")
		name, attributes, _ := strings.Cut(tag, " ")
		if strings.EqualFold(name, "a") {
			// Where the reader may go, not what the page loads.
			continue
		}
		for _, attribute := range []string{"href", "src", "srcset", "action", "poster"} {
			_, value, ok := strings.Cut(attributes, " "+attribute+`="`)
			if !ok && !strings.HasPrefix(attributes, attribute+`="`) {
				continue
			}
			if !ok {
				value = strings.TrimPrefix(attributes, attribute+`="`)
			}
			value, _, _ = strings.Cut(value, `"`)
			fetched = append(fetched, value)
		}
	}
	return fetched
}

func TestStyles_PullInNoSheetOrFontFromAnywhereElse(t *testing.T) {
	// A stylesheet reaches out with no tag around it: @import fetches another
	// sheet and url() fetches a font or an image. Neither appears in the markup
	// of any page, so neither is caught by reading the pages, and a font pulled
	// from a CDN is a page that renders in the wrong type on the machine this
	// program is most likely to run on — the one with no way out.
	css := mustAsset(t, "static/app.css")
	for _, reach := range []string{"@import", "url(", "http"} {
		if strings.Contains(css, reach) {
			t.Errorf("the stylesheet fetches something with %q", reach)
		}
	}
}

func TestPages_LoadNothingFromAnywhereElse(t *testing.T) {
	// Everything ships inside the binary. A page that reaches out fails on a
	// machine with no route to the internet, which is where this often runs,
	// and puts a stranger between an operator and their own records where it
	// does not fail.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 2, 1)
	seedHistory(t, s, "example.com", 2)

	var carried bool
	for _, at := range []string{"/", jobsAt, "/new", "/history?host=example.com", jobPath(id)} {
		body := get(t, s, at).Body.String()
		carried = carried || strings.Contains(body, "https://")
		loaded := resourcesOf(body)
		if len(loaded) == 0 {
			t.Errorf("%s was read as loading nothing at all, not even its stylesheet", at)
		}
		for _, address := range loaded {
			if !strings.HasPrefix(address, "/") || strings.HasPrefix(address, "//") {
				t.Errorf("%s loads %q from outside this binary", at, address)
			}
		}
	}
	// None of the above says anything unless a page carried an address on
	// somebody else's machine in the first place. Without a result on one of
	// them this test would pass on a rule that never had to tell the two apart.
	if !carried {
		t.Error("no page under test showed a result, so nothing here separated a result from a resource")
	}
}

func TestLayout_NamesTheProductInTheHeaderOfEveryPage(t *testing.T) {
	// The operator has to know which program is answering, and the header is
	// where they look. The header is cut out of the page and read on its own,
	// because the name also stands in the title of the tab, and a page that had
	// dropped it from the header would satisfy a search over the whole page.
	s := testServer(t)
	id := seedJob(t, s, "nightly", 3, 2, 1)

	for _, at := range []string{"/", jobsAt, "/new", "/history", jobPath(id)} {
		rec := get(t, s, at)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s gave %d, want 200", at, rec.Code)
		}
		_, header, ok := strings.Cut(rec.Body.String(), "<header")
		if !ok {
			t.Fatalf("%s carries no header at all", at)
		}
		header, _, ok = strings.Cut(header, "</header>")
		if !ok {
			t.Fatalf("%s never closes its header", at)
		}
		if !strings.Contains(header, "BlankTrail Google Parser") {
			t.Errorf("the header of %s does not name the product:\n%s", at, header)
		}
	}
}

func TestLayout_LaysTheJobSettingsOutUnderOneNameSoNoValueIsCrushed(t *testing.T) {
	// The list of how a job was set up was laid out twice: once under the class
	// it carries, and once as "the dl inside the settings card". The second won
	// on specificity and pinned the first column to the width its content asked
	// for, which left the value beside it whatever remained. Below about 860px
	// that was eighteen pixels, and "Parsing" came down the page a letter to a
	// line. Nothing reported it — the page rendered, and the stylesheet was
	// valid.
	//
	// So: one element is laid out under one name, and a grid that puts a list
	// into columns says the least width a column may have, so a window too
	// narrow for two gets one instead of two crushed ones.
	decls := stylesheet(t)

	var byTheCard []declaration
	var columns []declaration
	for _, d := range decls {
		if strings.HasPrefix(d.Selector, ".settings ") {
			byTheCard = append(byTheCard, d)
		}
		if d.Property == "grid-template-columns" && d.Selector == ".settings-list" {
			columns = append(columns, d)
		}
	}

	if len(byTheCard) > 0 {
		t.Errorf("the settings card styles what is inside it by where it sits rather than by what it is, "+
			"which is how one list came to be laid out by two rules: %v", byTheCard)
	}
	if len(columns) != 1 {
		t.Fatalf("the job settings are given columns by %d rules, want exactly one: %v", len(columns), columns)
	}
	if value := columns[0].Value; !strings.Contains(value, "auto-fit") || !strings.Contains(value, "minmax(") {
		t.Errorf("the job settings are laid out %q, which fixes the columns whatever the width is; "+
			"they are meant to be asked for with auto-fit and a minmax that says when one no longer fits", value)
	}
}

func TestLayout_StandsASettingAboveItsValueWhenTheyCannotStandSideBySide(t *testing.T) {
	// One setting is a label and its value. Held in columns, the value got
	// whatever the label left: the label took the width its own text asked for
	// at every window, and a value may break at any letter — that is how an
	// address as long as a query is kept from pushing the card sideways — so at
	// 320px what remained of the value was one glyph a line.
	//
	// So the pair is allowed to come apart onto two lines, and the value states
	// the width it needs before it gives way. Measured on /job/1: the two stand
	// side by side down to a row of 370px and one above the other from 365px,
	// each on a single line either way.
	decls := stylesheet(t)

	var wraps bool
	var pinned []declaration
	var basis string
	for _, d := range decls {
		switch d.Selector {
		case ".settings-list > div":
			if d.Property == "flex-wrap" && d.Value == "wrap" {
				wraps = true
			}
			if d.Property == "grid-template-columns" {
				pinned = append(pinned, d)
			}
		case ".settings-list dd":
			if d.Property == "flex" {
				basis = d.Value
			}
		}
	}

	if !wraps {
		t.Error("a setting and its value are held on one line whatever the width is, " +
			"so the narrower of the two is squeezed rather than moved under the other")
	}
	if len(pinned) > 0 {
		t.Errorf("the pair is laid out in columns that cannot give way: %v", pinned)
	}
	if basis == "" {
		t.Fatal("the value of a setting names no width of its own, so it is worth whatever the label leaves")
	}
	fields := strings.Fields(basis)
	width := resolved(decls, fields[len(fields)-1])
	unit := ""
	for _, suffix := range []string{"rem", "px"} {
		if strings.HasSuffix(width, suffix) {
			unit = suffix
		}
	}
	size, err := strconv.ParseFloat(strings.TrimSuffix(width, unit), 64)
	if unit == "" || err != nil || size <= 0 {
		t.Errorf("the value of a setting falls back to %q, which is not a width; "+
			"a value left to its content is a value one letter wide, because it breaks anywhere", width)
	}
}

func TestLayout_KeepsAWindowFromHavingToBeDraggedSideways(t *testing.T) {
	// Two things on these pages are wider than a narrow window: the strip of
	// tabs, and a table of results. Neither may push the page out, because a
	// page that scrolls sideways hides the right-hand half of every screen at
	// once — measured at 320px, the strip alone put the whole page 82 pixels
	// past the edge.
	//
	// They give way differently, and both ways are here so that neither is
	// quietly dropped: the strip comes apart onto a second line, since a tab
	// nobody can reach is a screen nobody can leave; the table scrolls inside
	// its own card, since rows read across and wrapping them would not make
	// them readable.
	decls := stylesheet(t)
	has := func(selector, property, value string) bool {
		for _, d := range decls {
			if d.Selector == selector && d.Property == property && d.Value == value {
				return true
			}
		}
		return false
	}

	if !has(".pages", "display", "flex") || !has(".pages", "flex-wrap", "wrap") {
		t.Error("the strip of tabs is held on one line whatever the width is, " +
			"so a window too narrow for it is a window that has to be dragged sideways")
	}
	if !has(".card > table", "overflow-x", "auto") {
		t.Error("a table wider than the card it stands in pushes the page out " +
			"rather than scrolling within itself")
	}
}

// setIn is every value the stylesheet gives one property under one rule, and it
// fails when the rule is not there at all: a test that asked a rule nobody
// writes any more would pass over nothing.
func setIn(t *testing.T, decls []declaration, selector, property string) []string {
	t.Helper()
	var known, values []string
	for _, d := range decls {
		if d.Selector != selector {
			continue
		}
		known = append(known, d.Property)
		if d.Property == property {
			values = append(values, d.Value)
		}
	}
	if len(known) == 0 {
		t.Fatalf("the stylesheet has no rule for %q, so this test read nothing", selector)
	}
	return values
}

func TestStyles_LetEveryBoxNarrowWithTheWindow(t *testing.T) {
	// Three things in this file ask for a width before anything else is
	// considered, and none of them will go below it on its own: a box is as wide
	// as its longest option or its size attribute, a fieldset is as wide as what
	// is inside it, and a flex item is as wide as its content. One of those left
	// alone is a page that goes sideways at a narrow window, and the reader
	// drags the page to reach a button.
	//
	// Measured at 320px before this: the profile picker on a job's page stood 4px
	// outside the page, the group of boxes on the new job form 39px, and the box
	// asking where a list is read from 31px. Every one of them is one of the
	// three floors below.
	decls := stylesheet(t)
	for _, one := range []struct {
		selector string
		property string
		want     string
		said     string
	}{
		// The boxes themselves. Recorded under the last name of the group they
		// are written in, which is how this file reads a selector list.
		{"textarea", "max-width", "100%", "a box wider than what holds it"},
		{"textarea", "min-width", "0", "a box that will not shrink"},
		{"fieldset", "min-width", "0", "a group of boxes that will not shrink"},
		{".field", "min-width", "0", "a field that will not shrink around its box"},
	} {
		var found bool
		for _, value := range setIn(t, decls, one.selector, one.property) {
			if value == one.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not set %s to %s, which leaves %s",
				one.selector, one.property, one.want, one.said)
		}
	}
}

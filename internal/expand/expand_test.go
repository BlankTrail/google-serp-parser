// SPDX-License-Identifier: MIT

package expand

import (
	"errors"
	"strings"
	"testing"
)

func made(t *testing.T, formats []string, queries ...string) []string {
	t.Helper()
	e, err := NewExpander(formats)
	if err != nil {
		t.Fatalf("NewExpander(%q): %v", formats, err)
	}
	var out []string
	for _, q := range queries {
		if err := e.Expand(q, func(s string) error { out = append(out, s); return nil }); err != nil {
			t.Fatalf("Expand: %v", err)
		}
	}
	return out
}

func TestExpand_PutsTheQueryWhereTheFormatSays(t *testing.T) {
	got := made(t, []string{"site:{query}", `"{query}"`, "{qery} review"}, "coffee maker")
	want := []string{"site:coffee maker", `"coffee maker"`, "coffee maker review"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("made %q, want %q", got, want)
	}
}

func TestExpand_WithNoFormatIsTheQueryAsItIs(t *testing.T) {
	if got := made(t, nil, "coffee"); len(got) != 1 || got[0] != "coffee" {
		t.Errorf("made %q, want the query alone", got)
	}
	if got := made(t, []string{"", "  "}, "coffee"); len(got) != 1 || got[0] != "coffee" {
		t.Errorf("blank formats made %q, want the query alone", got)
	}
}

func TestExpand_LettersGoRoundByRound(t *testing.T) {
	got := made(t, []string{"{query} {ABC:a:c:2}"}, "x")
	want := []string{"x a", "x b", "x c", "x aa", "x ab", "x ac", "x ba", "x bb", "x bc", "x ca", "x cb", "x cc"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("made %q, want %q", got, want)
	}
	if n := len(made(t, []string{"{query} {ABC:a:z:2}"}, "x")); n != 26+26*26 {
		t.Errorf("a to z in two rounds made %d, want %d", n, 26+26*26)
	}
	if got := made(t, []string{"{query} {ABC:а:в:1}"}, "x"); strings.Join(got, "|") != "x а|x б|x в" {
		t.Errorf("Cyrillic letters made %q", got)
	}
}

func TestExpand_NumbersRunFromTheFirstToTheLast(t *testing.T) {
	got := made(t, []string{"{query} {num:1:1000}"}, "x")
	if len(got) != 1000 || got[0] != "x 1" || got[999] != "x 1000" {
		t.Errorf("made %d, first %q, last %q; want 1000 from x 1 to x 1000", len(got), got[0], got[len(got)-1])
	}
	if got := made(t, []string{"{query} {num:-1:1}"}, "x"); strings.Join(got, "|") != "x -1|x 0|x 1" {
		t.Errorf("made %q", got)
	}
}

func TestExpand_TwoMacrosMakeEveryPair(t *testing.T) {
	got := made(t, []string{"{query} {ABC:a:b:1} {num:1:2}"}, "x")
	want := []string{"x a 1", "x a 2", "x b 1", "x b 2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("made %q, want %q", got, want)
	}
}

func TestExpand_KeepsWhatTheFormatsMakeOnceAcrossTheJob(t *testing.T) {
	// Two formats that make the same query, a line repeated in the list, and
	// two different lines that meet: each is asked once.
	got := made(t, []string{"{query}", "{query}", "{query} {num:1:2}"}, "x", "x", "x 1")
	want := []string{"x", "x 1", "x 2", "x 1 1", "x 1 2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("made %q, want %q", got, want)
	}
}

func TestParse_RefusesWhatCannotBeRun(t *testing.T) {
	for _, c := range []struct {
		format string
		want   error
	}{
		{"site:example.com", ErrNoQuery},
		{"{query} {num:5:1}", ErrMacro},
		{"{query} {num:a:9}", ErrMacro},
		{"{query} {ABC:a:z:0}", ErrMacro},
		{"{query} {ABC:z:a:1}", ErrMacro},
		{"{query} {ABC:ab:z:1}", ErrMacro},
		{"{query} {ABC:a:z:5}", ErrMacro},
		{"{query} {num:1:1000} {num:1:1001}", ErrTooMany},
	} {
		if _, err := Parse(c.format); !errors.Is(err, c.want) {
			t.Errorf("Parse(%q) = %v, want %v", c.format, err, c.want)
		}
	}
	// Braces that are no macro of this package are text.
	if got := made(t, []string{"{query} {other}"}, "x"); len(got) != 1 || got[0] != "x {other}" {
		t.Errorf("made %q, want the unknown braces kept", got)
	}
}

func TestExpand_StopsAtTheFirstRefusalOfWhatItIsHanded(t *testing.T) {
	e, err := NewExpander([]string{"{query} {num:1:10}"})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	stop := errors.New("stop")
	err = e.Expand("x", func(string) error {
		n++
		if n == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || n != 3 {
		t.Errorf("handed %d and returned %v, want three and the refusal", n, err)
	}
}

func TestExpander_PerQueryCountsEveryFormat(t *testing.T) {
	e, err := NewExpander([]string{"{query}", "{query} {ABC:a:z:2}", "{query} {num:1:1000}"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := e.PerQuery(), 1+702+1000; got != want {
		t.Errorf("PerQuery = %d, want %d", got, want)
	}
}

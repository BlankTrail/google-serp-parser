// SPDX-License-Identifier: MIT

package export

import (
	"errors"
	"slices"
	"testing"
)

func TestCatalogs_NameTheColumnsTheirFilesAlwaysCarried(t *testing.T) {
	// A file written with nothing chosen has to be the file this program always
	// wrote, and the catalogs are where its columns now come from. Their names
	// and their order are therefore the old headers, word for word.
	want := map[string][]string{
		"results":  {"ordinal", "query", "page", "rank", "title", "url", "link", "host", "snippet", "display_path"},
		"ads":      {"ordinal", "query", "page", "position", "placement", "title", "host", "url", "snippet"},
		"related":  {"ordinal", "query", "page", "position", "text"},
		"verdicts": {"ordinal", "address", "held"},
	}
	got := map[string][]string{
		"results": Results.Names(), "ads": Ads.Names(),
		"related": Suggestions.Names(), "verdicts": Verdicts.Names(),
	}
	for part, names := range want {
		if !slices.Equal(got[part], names) {
			t.Errorf("the %s are written as %v, want %v", part, got[part], names)
		}
	}
}

func TestCatalog_RefusesALayoutItCannotWrite(t *testing.T) {
	// Refused before a byte is written: a file that stops half way looks
	// finished to whoever downloaded it.
	refused := map[string]Layout{
		"a format nobody writes":   {Format: "xlsx", Fields: []string{ColURL}, EOL: LF, Sep: TabSeparator},
		"no column at all":         {Format: "csv", EOL: LF, Sep: TabSeparator},
		"a column results lack":    {Format: "csv", Fields: []string{ColPlacement}, EOL: LF, Sep: TabSeparator},
		"the same column twice":    {Format: "csv", Fields: []string{ColURL, ColURL}, EOL: LF, Sep: TabSeparator},
		"a line ending of its own": {Format: "csv", Fields: []string{ColURL}, EOL: "\r", Sep: TabSeparator},
	}
	for name, l := range refused {
		err := Results.Check(l)
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if !errors.Is(err, ErrLayout) && !errors.Is(err, ErrUnknownFormat) {
			t.Errorf("%s was refused with %v, want ErrLayout or ErrUnknownFormat", name, err)
		}
	}
	if err := Results.Check(DefaultLayout("csv", Results.Names())); err != nil {
		t.Errorf("the layout written when nobody chose was refused: %v", err)
	}
}

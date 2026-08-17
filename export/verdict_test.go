// SPDX-License-Identifier: MIT

package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func verdicts() []Verdict {
	return []Verdict{
		{Ordinal: 0, Target: "example.com/page", Held: true},
		{Ordinal: 1, Target: "example.com/gone, or so it seems", Held: false},
	}
}

func TestVerdictCSV_CarriesTheAddressesNothingWasFoundFor(t *testing.T) {
	// This is the whole reason this shape exists. An index job is run to learn
	// which addresses are held and which are not, and a file holding only the
	// held ones answers half the question while looking complete.
	var buf bytes.Buffer
	w := NewVerdictCSV(&buf)
	for _, v := range verdicts() {
		if err := w.Write(v); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recs, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("the file this wrote does not read back as CSV: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("%d records, want a header and both addresses", len(recs))
	}
	if got := recs[0]; got[0] != "ordinal" || got[1] != "address" || got[2] != "held" {
		t.Errorf("header is %v, want the three columns a verdict has", got)
	}
	if recs[1][1] != "example.com/page" || recs[1][2] != "true" {
		t.Errorf("the held address came back as %v", recs[1])
	}
	if recs[2][1] != "example.com/gone, or so it seems" || recs[2][2] != "false" {
		t.Errorf("the address nothing was found for came back as %v", recs[2])
	}
}

func TestVerdictCSV_SaysWhichLineOfTheListEachAnswerBelongsTo(t *testing.T) {
	// A file of a million addresses is read beside the file it was made from.
	// Without the ordinal, an answer cannot be laid against the line it is about.
	var buf bytes.Buffer
	w := NewVerdictCSV(&buf)
	if err := w.Write(Verdict{Ordinal: 41, Target: "late.test", Held: true}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recs, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if recs[1][0] != "41" {
		t.Errorf("the ordinal came back as %q, want 41", recs[1][0])
	}
}

func TestVerdictCSV_WritesTheHeaderWhenNoAddressWasEverChecked(t *testing.T) {
	// A job stopped before it reached anything has no verdicts. An empty file
	// reads as a download that broke; a table with no rows reads as an answer.
	var buf bytes.Buffer
	if err := NewVerdictCSV(&buf).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "ordinal,address,held") {
		t.Errorf("an export that checked nothing wrote %q", buf.String())
	}
}

func TestVerdictJSONL_CarriesHeldAsSomethingAProgramCanBranchOn(t *testing.T) {
	// The file exists to be read by another program. A verdict spelled as a word
	// makes every reader guess which words mean no.
	var buf bytes.Buffer
	w := NewVerdictJSONL(&buf)
	for _, v := range verdicts() {
		if err := w.Write(v); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want one per address checked", len(lines))
	}
	var second struct {
		Ordinal int    `json:"ordinal"`
		Target  string `json:"target"`
		Held    bool   `json:"held"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("the second line does not read back as an object: %v", err)
	}
	if second.Held {
		t.Error("the address nothing was found for came back held")
	}
	if second.Target != "example.com/gone, or so it seems" {
		t.Errorf("the address came back as %q", second.Target)
	}
	if second.Ordinal != 1 {
		t.Errorf("the ordinal came back as %d, want 1", second.Ordinal)
	}
}

func TestNewVerdicts_WritesTheSameFormatsTheResultsOfAJobAreWrittenIn(t *testing.T) {
	// An operator picks a format, not a format and a kind of job. A format that
	// works for one job and refuses for another is a menu that lies.
	for _, format := range Formats() {
		var buf bytes.Buffer
		w, err := NewVerdicts(format, &buf)
		if err != nil {
			t.Fatalf("NewVerdicts(%q): %v", format, err)
		}
		if err := w.Write(Verdict{Ordinal: 0, Target: "a.test", Held: true}); err != nil {
			t.Fatalf("Write to %q: %v", format, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close of %q: %v", format, err)
		}
		if !strings.Contains(buf.String(), "a.test") {
			t.Errorf("%q wrote %q, which does not carry the address", format, buf.String())
		}
	}
}

func TestNewVerdicts_RefusesAFormatItCannotWrite(t *testing.T) {
	if _, err := NewVerdicts("xlsx", &bytes.Buffer{}); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("NewVerdicts(\"xlsx\") returned %v, want ErrUnknownFormat", err)
	}
}

func TestVerdictWriters_RefuseAnAddressThatArrivesAfterTheFileIsFinished(t *testing.T) {
	// The file has been handed over by then. Taking the address would leave the
	// caller believing an answer reached the operator when it did not.
	for _, format := range Formats() {
		var buf bytes.Buffer
		w, err := NewVerdicts(format, &buf)
		if err != nil {
			t.Fatalf("NewVerdicts(%q): %v", format, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close of %q: %v", format, err)
		}
		if err := w.Write(Verdict{Target: "late.test"}); !errors.Is(err, ErrClosed) {
			t.Errorf("a write to a finished %q export returned %v, want ErrClosed", format, err)
		}
		if strings.Contains(buf.String(), "late.test") {
			t.Errorf("%q took the address anyway: %q", format, buf.String())
		}
	}
}

func TestVerdictCSV_SaysSoWhenTheFileCannotBeWritten(t *testing.T) {
	// A disk that has run out has to reach the caller. An export that swallows it
	// hands over a file that stops part way and looks finished.
	w := NewVerdictCSV(brokenWriter{})
	if err := w.Write(verdicts()[0]); err == nil {
		if err := w.Close(); err == nil {
			t.Error("writing to a full disk was reported as a file that finished")
		}
	}
}

// SPDX-License-Identifier: MIT

package google

import "testing"

func TestCountries_OfferEveryCountryThisProgramHasADomainFor(t *testing.T) {
	// A country with a domain of its own answers from that domain, and one
	// without it answers from google.com with gl set. The two are different
	// pages. So a chooser that left out a country this program has a domain for
	// would hide the better of the two behind a code the reader has to know.
	// Some domains answer to more than one code — the United Kingdom to both gb
	// and uk — and one of the two in the chooser covers it. Offering both would
	// put the same country in the list twice.
	offered := map[string]bool{}
	for _, c := range Countries() {
		offered[c.Code] = true
	}
	for host, codes := range KnownDomains() {
		reached := false
		for _, code := range codes {
			if offered[code] {
				reached = true
			}
		}
		if !reached {
			t.Errorf("%s is a domain of this program's own and no code in the chooser reaches it (%v)", host, codes)
		}
	}
}

func TestChoices_CarryACodeAndANameAndNoDuplicates(t *testing.T) {
	// A chooser is a list of pairs, and a half-filled pair is an entry a reader
	// cannot use: a code with no name is a code they have to know already, and a
	// name with no code sets nothing.
	for _, list := range []struct {
		what string
		of   []Choice
	}{{"language", Languages()}, {"country", Countries()}} {
		seen := map[string]bool{}
		for _, c := range list.of {
			if c.Code == "" || c.Name == "" {
				t.Errorf("a %s is offered as %+v", list.what, c)
			}
			if seen[c.Code] {
				t.Errorf("%s %q is offered twice", list.what, c.Code)
			}
			seen[c.Code] = true
		}
		if len(list.of) < 20 {
			t.Errorf("the %s chooser offers %d, which is too few to be a shortcut", list.what, len(list.of))
		}
	}
}

func TestChoices_AreACopySoAScreenCannotEditTheList(t *testing.T) {
	first := Languages()
	first[0].Name = "changed"
	if Languages()[0].Name == "changed" {
		t.Error("a caller edited the list every other caller reads")
	}
}

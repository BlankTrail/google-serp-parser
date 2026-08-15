// SPDX-License-Identifier: MIT

package version

import (
	"strings"
	"testing"
)

func TestUserAgent_CarriesTheProjectURL(t *testing.T) {
	// The User-Agent is the only place a scraped origin can see who is asking.
	// Dropping the URL would make this tool anonymous traffic, which is exactly
	// what an open-source scraper should not be.
	ua := UserAgent()
	if want := "gserp/" + Version(); !strings.Contains(ua, want) {
		t.Errorf("UserAgent()=%q, want it to contain %q", ua, want)
	}
	if !strings.Contains(ua, "github.com/blanktrail/google-serp-parser") {
		t.Errorf("UserAgent()=%q, want it to carry the project URL", ua)
	}
}

func TestVersion_IsNotEmpty(t *testing.T) {
	if Version() == "" {
		t.Error("Version() is empty; the binary must always be able to say what it is")
	}
}

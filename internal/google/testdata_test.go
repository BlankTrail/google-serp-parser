// SPDX-License-Identifier: MIT

package google

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtures are the pages every parser test runs against, with the size each
// must stay under. They are written by hand, not captured: this repository
// publishes one product and nothing else, and a third party's pages are not
// ours to ship.
var fixtures = map[string]int{
	"serp_goto_ru.html":   16 * 1024,
	"serp_cards_ru.html":  16 * 1024,
	"serp_site_ru.html":   16 * 1024,
	"serp_ads_us.html":    24 * 1024,
	"serp_direct_us.html": 16 * 1024,
	"jsshell.html":        4 * 1024,
}

func TestFixtures_ArePresentAndSmall(t *testing.T) {
	for name, budget := range fixtures {
		info, err := os.Stat(filepath.Join("testdata", name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
		if info.Size() > int64(budget) {
			t.Errorf("%s is %d bytes, over its %d budget", name, info.Size(), budget)
		}
	}
}

func TestFixtures_AreOurOwnWorkNotCaptures(t *testing.T) {
	// A captured page is recognisable: it carries Google's session-scoped
	// telemetry payloads. Ours carry the attribute names the parser anchors
	// on and nothing else. This test is what keeps a well-meaning "let me use
	// a real page, it will be more accurate" out of the repository.
	for name := range fixtures {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			continue
		}
		body := string(raw)
		for _, marker := range []string{"sca_esv", "sxsrf", "gstatic.com", "data:image/"} {
			if strings.Contains(body, marker) {
				t.Errorf("%s contains %q — that is a captured page, not a written one", name, marker)
			}
		}
		if len(body) > 0 && !strings.Contains(body, "<!-- synthetic fixture") {
			t.Errorf("%s lacks the synthetic-fixture banner", name)
		}
	}
}

func TestFixtures_AreDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "README.md"))
	if err != nil {
		t.Fatalf("testdata/README.md: %v", err)
	}
	for name := range fixtures {
		if !strings.Contains(string(raw), name) {
			t.Errorf("README.md does not document %s", name)
		}
	}
}

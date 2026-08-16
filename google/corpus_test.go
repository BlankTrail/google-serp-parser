//go:build corpus

// SPDX-License-Identifier: MIT

package google

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorpus_ParsesRealCapturedPages runs the parser against real result
// pages held OUTSIDE this repository, and is the answer to the one weakness
// the written fixtures have: they encode our understanding of the markup, so
// on their own they cannot tell us the understanding has gone stale.
//
// Point it at a directory of captured pages — the tool never fetches anything
// itself:
//
//	mkdir -p testdata-corpus && cp /path/to/*.html testdata-corpus/
//	go test -tags corpus ./google/ -run TestCorpus -v
//
// testdata-corpus/ is git-ignored on purpose. Captured pages are not part of
// this product and must never be committed.
//
// It asserts invariants rather than counts, because the corpus is whatever
// the developer put there: every result carries a title, positions run 1..N
// without gaps, no result is a Google property, and no ad leaked into the
// organic set. A page that produces zero results is reported loudly — that is
// exactly the drift this test exists to catch.
func TestCorpus_ParsesRealCapturedPages(t *testing.T) {
	dir := os.Getenv("GSERP_CORPUS")
	if dir == "" {
		dir = "../testdata-corpus"
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.html"))
	if err != nil || len(files) == 0 {
		t.Skipf("no corpus in %s — see this test's comment", dir)
	}

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			class, cerr := Classify(200, "", raw)
			t.Logf("class=%s err=%v", class, cerr)
			if !class.Usable() {
				return // a shell or a wall in the corpus is legitimate input
			}

			serp, err := ParseSERP("corpus", raw)
			if err != nil {
				t.Fatalf("ParseSERP: %v", err)
			}
			if len(serp.Results) == 0 {
				t.Errorf("a page classified %s yielded no results — the parser has drifted", class)
			}
			forms := map[LinkForm]int{}
			for i, r := range serp.Results {
				if r.Position != i+1 {
					t.Errorf("result %d carries position %d", i+1, r.Position)
				}
				if r.Title == "" {
					t.Errorf("result %d has no title", i+1)
				}
				if r.Host != "" && isGoogleHost(r.Host) {
					t.Errorf("result %d is a Google property: %s", i+1, r.Host)
				}
				forms[r.Form]++
			}
			t.Logf("organic=%d forms=%v ads=%d related=%d",
				len(serp.Results), forms, len(serp.Ads), len(serp.Related))
		})
	}
}

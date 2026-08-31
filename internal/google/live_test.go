//go:build live

// SPDX-License-Identifier: MIT

package google_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// TestLive_MeasuresWhatTheSpecLeftOpen answers, against a running BlankTrail,
// the questions the design deliberately refused to guess:
//
//  1. Which link form does a logged-out session actually receive?
//  2. How long does an encrypted link stay resolvable after capture?
//  3. Does any page state a result total, given every capture so far has
//     carried an empty #result-stats?
//
// Run it deliberately:
//
//	BLANKTRAIL_API_KEY=... BLANKTRAIL_URL=... go test -tags live ./google/ -run TestLive -v
func TestLive_MeasuresWhatTheSpecLeftOpen(t *testing.T) {
	key := os.Getenv("BLANKTRAIL_API_KEY")
	if key == "" {
		t.Skip("BLANKTRAIL_API_KEY is not set")
	}
	// The address is required rather than defaulted. A live test that falls
	// back to a well-known address measures whatever happens to be listening
	// there, and reports it as this one's result.
	control := os.Getenv("BLANKTRAIL_URL")
	if control == "" {
		t.Skip("BLANKTRAIL_URL is not set")
	}

	ctx := context.Background()
	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{
		Domains: []string{"www.google.com", "google.com"},
		Ports:   1,
	})
	for _, f := range report.Findings {
		t.Logf("[%s] %s — %s → %s", f.Severity, f.Title, f.Detail, f.Action)
	}
	if !report.OK() {
		t.Fatal("preflight failed; fix the findings above")
	}

	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client:         client,
		Threads:        1,
		PortsPerThread: 1,
		Spec:           blanktrail.DefaultPortSpec(),
		Channels:       []blanktrail.Channel{blanktrail.NewDirectChannel("direct")},
		CA:             report.CA,
		DelayMin:       3 * time.Second,
		DelayMax:       8 * time.Second,
	})
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	lease, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()

	session := google.NewSession(lease.Client().Transport)
	serp, err := session.Search(ctx, google.Query{Text: "test query", Country: "us", Language: "en"})
	if err != nil {
		t.Fatalf("live search: %v", err)
	}

	// Measurement 1: the link form.
	forms := map[google.LinkForm]int{}
	for _, r := range serp.Results {
		forms[r.Form]++
	}
	t.Logf("MEASUREMENT link forms: %v over %d results", forms, len(serp.Results))
	if len(serp.Results) == 0 {
		t.Fatal("live capture produced no results")
	}

	// Measurement 3: does the page state a total?
	t.Logf("MEASUREMENT total stated: %v (%d)", serp.HasTotal, serp.TotalResults)

	// Measurement 2: resolution, immediately and after a delay.
	//
	// The link is origin-relative under the encrypted form, so it is joined to
	// the origin the page came from. Hardcoding www.google.com here would be
	// wrong for every capture that is not Country "us" — the page, and the
	// redirector on it, live on the ccTLD the query was aimed at.
	t.Logf("MEASUREMENT origin: %q", serp.Origin)
	var encrypted string
	for _, r := range serp.Results {
		if r.Form == google.LinkEncrypted && r.Link != "" {
			if serp.Origin == "" {
				t.Fatal("the capture reported no origin; an origin-relative link cannot be resolved")
			}
			encrypted = serp.Origin + r.Link
			break
		}
	}
	if encrypted == "" {
		t.Log("MEASUREMENT resolution: no encrypted link in this capture; nothing to resolve")
		return
	}
	resolver := google.NewResolver(lease.Client().Transport)
	now, err := resolver.Resolve(ctx, encrypted)
	t.Logf("MEASUREMENT resolve immediately: %q err=%v", now, err)

	time.Sleep(60 * time.Second)
	later, err := resolver.Resolve(ctx, encrypted)
	t.Logf("MEASUREMENT resolve after 60s: %q err=%v", later, err)
}

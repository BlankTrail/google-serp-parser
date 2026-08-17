//go:build live

// SPDX-License-Identifier: MIT

package google

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/blanktrail"
)

// echoAt is a service that answers with the headers it was sent.
//
// It is named here rather than passed in so that what this measurement talked
// to is written down beside its numbers. Nothing about this machine or its
// settings goes to it: the request is a bare GET, and what is being measured is
// exactly the headers this program and the proxy put on it.
const echoAt = "https://postman-echo.com/headers"

// TestLiveHeaders_SaysWhatActuallyLeavesThroughAPort answers a question nothing
// in this repository can answer on its own.
//
// This program sets some headers, and the proxy — with header spoofing on —
// sets others: the user agent, the client hints, the order they go in. Which of
// the two ends up setting what is not written down anywhere, and the one header
// that matters most here is the one the proxy is documented never to touch: the
// browser's language. A capture asking Google for English while claiming to
// prefer nothing is a mismatch this program would be creating itself.
//
// Run it deliberately:
//
//	BLANKTRAIL_API_KEY=... BLANKTRAIL_URL=... go test -tags live ./google/ -run TestLiveHeaders -v
func TestLiveHeaders_SaysWhatActuallyLeavesThroughAPort(t *testing.T) {
	key, control := os.Getenv("BLANKTRAIL_API_KEY"), os.Getenv("BLANKTRAIL_URL")
	if key == "" || control == "" {
		t.Skip("BLANKTRAIL_API_KEY and BLANKTRAIL_URL are not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{Ports: 1})
	if !report.OK() {
		for _, f := range report.Findings {
			t.Logf("[%s] %s — %s", f.Severity, f.Title, f.Detail)
		}
		t.Fatal("preflight failed; fix the findings above")
	}

	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 1, PortsPerThread: 1,
		Spec: blanktrail.DefaultPortSpec(), CA: report.CA,
	})
	if err != nil {
		t.Fatalf("opening one identity: %v", err)
	}
	defer func() { _ = pool.Close() }()

	lease, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("leasing it: %v", err)
	}
	defer lease.Release()

	// The same session the parser uses, so what is measured is what a capture
	// sends and not what a test made up.
	sess := &Session{Client: lease.Client()}
	sent := headersEchoed(t, ctx, sess, Query{Text: "x", Country: "us", Language: "en"})

	names := make([]string, 0, len(sent))
	for name := range sent {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("MEASUREMENT header: %s: %s", name, sent[name])
	}

	// Case is not compared. HTTP header names are case-insensitive and what
	// echoes them back is free to normalise; a test that missed a header because
	// the far end lower-cased it would report a program sending nothing.
	sent = folded(sent)

	// The one this program is responsible for. The proxy is documented never to
	// set it, so if it is missing here it is missing from every capture.
	if sent["accept-language"] == "" {
		t.Error("MEASUREMENT: no Accept-Language arrived, so every capture asks for a " +
			"language in the address and claims to prefer none")
	}
	// The rest of what a browser sends. Each is reported rather than demanded:
	// what this test is for is finding out which of the two ends sets them.
	for _, name := range []string{"accept", "accept-encoding", "user-agent",
		"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "sec-fetch-user",
		"upgrade-insecure-requests", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform"} {
		if sent[name] == "" {
			t.Errorf("MEASUREMENT: %s did not arrive at all", name)
		}
	}
}

// headersEchoed asks a service that answers with what it was sent, through the
// session under test.
func headersEchoed(t *testing.T, ctx context.Context, sess *Session, q Query) map[string]string {
	t.Helper()
	body, err := sess.fetch(ctx, echoAt, q)
	if err != nil {
		t.Fatalf("asking %s what arrived: %v", echoAt, err)
	}
	var answer struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("%s answered with something that is not its usual report: %v\n%s",
			echoAt, err, string(body))
	}
	return answer.Headers
}

// folded is the same headers under lower-case names.
func folded(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, value := range in {
		out[strings.ToLower(name)] = value
	}
	return out
}

func TestLiveHeaders_SaysWhatAPhoneSendsAndThatItIsAPhone(t *testing.T) {
	// The other half of the same question. A run labelled "mobile" that went out
	// on Windows would be a measurement of the wrong thing filed under the right
	// name, and nothing in the results themselves would say so — Google simply
	// answers with a different page.
	key, control := os.Getenv("BLANKTRAIL_API_KEY"), os.Getenv("BLANKTRAIL_URL")
	if key == "" || control == "" {
		t.Skip("BLANKTRAIL_API_KEY and BLANKTRAIL_URL are not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, err := blanktrail.NewClient(control, key)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	report := blanktrail.Preflight(ctx, client, blanktrail.PreflightInput{Ports: 2})
	if !report.OK() {
		for _, f := range report.Findings {
			t.Logf("[%s] %s — %s", f.Severity, f.Title, f.Detail)
		}
		t.Fatal("preflight failed; fix the findings above")
	}

	// Two ports, so both phones are opened: the pool guarantees every template
	// at least one.
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: client, Threads: 2, PortsPerThread: 1,
		Spec:  blanktrail.DefaultPortSpec(),
		Specs: blanktrail.SpecsFor(blanktrail.DeviceMobile),
		CA:    report.CA,
	})
	if err != nil {
		t.Fatalf("opening two phones: %v", err)
	}
	defer func() { _ = pool.Close() }()

	for _, phone := range []string{"ios", "android"} {
		lease, err := pool.AcquireSpec(ctx, phone)
		if err != nil {
			t.Errorf("leasing the %s port: %v", phone, err)
			continue
		}
		sess := &Session{Client: lease.Client(), Mobile: true}
		sent := folded(headersEchoed(t, ctx, sess, Query{Text: "x", Country: "us", Language: "en"}))
		lease.Release()

		t.Logf("MEASUREMENT %s: user-agent: %s", phone, sent["user-agent"])
		t.Logf("MEASUREMENT %s: accept-language: %s", phone, sent["accept-language"])
		t.Logf("MEASUREMENT %s: accept: %s", phone, sent["accept"])
		t.Logf("MEASUREMENT %s: sec-ch-ua-mobile: %q sec-ch-ua-platform: %q",
			phone, sent["sec-ch-ua-mobile"], sent["sec-ch-ua-platform"])

		if sent["accept-language"] == "" {
			t.Errorf("the %s port sent no language at all", phone)
		}
		if !strings.Contains(strings.ToLower(sent["user-agent"]), "mobile") &&
			!strings.Contains(strings.ToLower(sent["user-agent"]), "iphone") &&
			!strings.Contains(strings.ToLower(sent["user-agent"]), "android") {
			t.Errorf("the %s port says it is %q, which is not a phone", phone, sent["user-agent"])
		}
	}
}

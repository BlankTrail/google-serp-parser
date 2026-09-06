//go:build live

// SPDX-License-Identifier: MIT

package run

// What a hidden-address lookup looks like on the wire, written out for whoever
// has to teach a proxy which answers to leave alone.
//
// A lookup asks Google's redirector for one link and reads the address out of
// the Location header. It is not a page, it carries no challenge, and nothing
// about it needs solving — but from the proxy's side it is a request to a
// Google domain that answers 3xx, which is also what a block page looks like.
// This writes down enough of both to tell them apart by rule: the request line,
// every response header, and how long each took, for lookups beside a search
// through the same identity.
//
//	GSERP_DUMP=C:\path\to\goto-dump.txt \
//	go test -tags live -run TestLiveGotoDump -timeout 30m ./internal/run/ -v
//
// The file is written where GSERP_DUMP says and nowhere else; without it the
// test says so and stops, because a dump of live traffic is not something to
// leave in a temporary directory by accident.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

func TestLiveGotoDump_WhatALookupLooksLikeOnTheWire(t *testing.T) {
	path := os.Getenv("GSERP_DUMP")
	if path == "" {
		t.Skip("GSERP_DUMP is not set: say where the dump goes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)

	var out strings.Builder
	say := func(format string, args ...any) {
		out.WriteString(hide(fmt.Sprintf(format, args...)))
		out.WriteString("\n")
	}

	say("What one address lookup looks like on the wire.")
	say("Written %s by the parser's live harness.", time.Now().UTC().Format(time.RFC3339))
	say("")
	say("A lookup is a GET to the redirector on the same Google domain the page")
	say("came from. It is answered 302 with the destination in Location, it")
	say("carries no challenge, and following it is never wanted: the client is")
	say("built not to. A search on the same identity is dumped below it for")
	say("comparison, so a rule can be written narrowly.")
	say("")

	query := google.Query{Text: "купить кондиционер", Country: "ru", Language: "ru"}
	var serps []google.SERP
	var searchLease *blanktrail.Lease
	for try := 1; try <= 12; try++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			fatalf(t, "acquiring an identity to capture with: %v", err)
		}
		cl := lease.Client()
		session := google.NewSession(cl.Transport)
		session.Client.Timeout = cl.Timeout
		began := time.Now()
		got, err := google.SearchDepth(ctx, session, query, 1)
		if err != nil {
			logf(t, "capture attempt %d did not get through: %v", try, err)
			_ = lease.Reject(ctx)
			lease.Release()
			continue
		}
		say("=== the search that captured the page ===")
		say("  query      %q, country ru, language ru", query.Text)
		say("  took       %v (the whole capture, challenge included)", time.Since(began).Round(time.Millisecond))
		say("  results    %d", len(got[0].Results))
		say("  origin     %s", got[0].Origin)
		say("")
		searchLease = lease
		serps = got
		break
	}
	if len(serps) == 0 {
		t.Fatal("no identity carried the capture")
	}
	defer searchLease.Release()

	origin := serps[0].Origin
	var hidden []google.Result
	forms := map[google.LinkForm]int{}
	for _, one := range serps[0].Results {
		forms[one.Form]++
		if !one.Resolved() && one.Link != "" {
			hidden = append(hidden, one)
		}
	}
	say("=== what the page's links looked like ===")
	var kinds []string
	for form, n := range forms {
		kinds = append(kinds, fmt.Sprintf("%s×%d", form, n))
	}
	sort.Strings(kinds)
	say("  forms      %s", strings.Join(kinds, ", "))
	say("  of those, %d carry no address and have to be looked up", len(hidden))
	say("")
	if len(hidden) == 0 {
		t.Skip("this capture hid no addresses, so there is nothing to dump")
	}

	// A plain request to the same origin, no redirector: the baseline a rule
	// has to leave alone.
	dumpOne(ctx, t, say, searchLease, "a plain page on the same origin, for the baseline", origin+"/")

	// The lookups themselves, through the identity that captured the page and
	// through fresh ones, until enough of each outcome has been seen.
	seen, failed := 0, 0
	for i := range hidden {
		link, err := absolute(origin, hidden[i].Link)
		if err != nil {
			continue
		}
		lease := searchLease
		var fresh *blanktrail.Lease
		if i > 0 {
			fresh, err = pool.Acquire(ctx)
			if err != nil {
				break
			}
			lease = fresh
		}
		what := "a lookup through the identity that captured the page"
		if fresh != nil {
			what = "a lookup through a fresh identity"
		}
		ok := dumpOne(ctx, t, say, lease, what, link)
		if fresh != nil {
			if !ok {
				_ = fresh.Reject(ctx)
			}
			fresh.Release()
		}
		if ok {
			seen++
		} else {
			failed++
		}
		if seen >= 4 && failed >= 2 {
			break
		}
		if seen+failed >= 14 {
			break
		}
	}

	say("=== how to tell them apart ===")
	say("  A lookup is a GET whose path is /goto (or /url) on a Google domain,")
	say("  answered 3xx with a Location header pointing off Google. Nothing in")
	say("  it needs solving and the client never follows it: the answer IS the")
	say("  header. A block page is the other 3xx — its Location stays on Google")
	say("  and lands on /sorry/.")
	say("")
	say("  Lookups answered: %d; lookups the address would not carry: %d", seen, failed)

	if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
		fatalf(t, "writing the dump: %v", err)
	}
	logf(t, "the dump is at %s (%d bytes)", path, len(out.String()))
}

// dumpOne sends one GET through a lease and writes down everything about it
// that a rule could be written against. It reports whether the far end answered
// at all.
func dumpOne(ctx context.Context, t *testing.T, say func(string, ...any),
	lease *blanktrail.Lease, what, target string) bool {
	t.Helper()
	cl := lease.Client()
	client := &http.Client{
		Transport: cl.Transport,
		Timeout:   cl.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var connected, wrote, firstByte time.Duration
	began := time.Now()
	trace := &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { connected = time.Since(began) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { wrote = time.Since(began) },
		GotFirstResponseByte: func() { firstByte = time.Since(began) },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, target, nil)
	if err != nil {
		say("=== %s ===", what)
		say("  the request could not be built: %v", err)
		say("")
		return false
	}

	say("=== %s ===", what)
	say("  %s %s", req.Method, req.URL.String())
	say("  request headers set by this program: %s", headersOf(req.Header))
	resp, err := client.Do(req)
	took := time.Since(began)
	if err != nil {
		say("  no answer after %v: %v", took.Round(time.Millisecond), err)
		say("  connected at %v, request written at %v",
			connected.Round(time.Millisecond), wrote.Round(time.Millisecond))
		say("")
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()

	say("  %s %d %s", resp.Proto, resp.StatusCode, http.StatusText(resp.StatusCode))
	var names []string
	for name := range resp.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range resp.Header[name] {
			say("  < %s: %s", name, v)
		}
	}
	say("  body        %d bytes read of %d declared", len(body), resp.ContentLength)
	if len(body) > 0 {
		say("  body starts %q", strings.TrimSpace(string(body[:min(len(body), 160)])))
	}
	say("  timing      connected %v, written %v, first byte %v, whole %v",
		connected.Round(time.Millisecond), wrote.Round(time.Millisecond),
		firstByte.Round(time.Millisecond), took.Round(time.Millisecond))
	say("")
	return true
}

// headersOf names the headers a request carries, without their values: what
// matters here is which of them this program sets and which the proxy adds.
func headersOf(h http.Header) string {
	if len(h) == 0 {
		return "none — the proxy writes them all"
	}
	var names []string
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// absolute joins a page-relative link to the origin it came from.
func absolute(origin, link string) (string, error) {
	if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
		return link, nil
	}
	if origin == "" {
		return "", fmt.Errorf("no origin for %q", link)
	}
	return strings.TrimRight(origin, "/") + link, nil
}

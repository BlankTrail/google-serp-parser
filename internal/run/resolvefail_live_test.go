//go:build live

// SPDX-License-Identifier: MIT

package run

// Why two thirds of the addresses do not come back.
//
// On a live run of a region that hides them, 859 of 2 691 results were written
// with an address and the rest without: the lookups happen — that is the whole
// of the last fix — and most of them fail. This measures what the failure is,
// and tests the one thing about the arrangement that was assumed rather than
// measured on this region: that the address behind a link can be read by a
// client other than the one that captured the page.
//
//	go test -tags live -run TestLiveResolve -timeout 30m ./internal/run/ -v
//
// It spends two identities and a few dozen requests.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

func TestLiveResolve_WhoCanReadTheAddressBehindALink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)

	// One identity captures, and keeps its lease: half the links are then looked
	// up through that same identity and half through another, which is what the
	// program does today. If the two halves come back differently, the deferred
	// lookup is reading a link that belongs to a session it is not in.
	// Through as many identities as it takes: on a list of this kind most
	// addresses are dead, and one that answers is what the rest of the
	// measurement is about. The lease that captured is kept.
	query := google.Query{Text: "генерировать изображение", Country: "ru", Language: "ru"}
	var captured *blanktrail.Lease
	var client *http.Client
	var serps []google.SERP
	for try := 1; try <= 12; try++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquiring an identity to capture with: %v", err)
		}
		cl := lease.Client()
		session := google.NewSession(cl.Transport)
		session.Client.Timeout = cl.Timeout
		got, err := google.SearchDepth(ctx, session, query, 3)
		if err != nil {
			logf(t, "capture attempt %d did not get through: %v", try, err)
			_ = lease.Reject(ctx)
			lease.Release()
			continue
		}
		captured, client, serps = lease, cl, got
		break
	}
	if captured == nil {
		t.Fatal("no identity carried the capture")
	}
	defer captured.Release()

	forms := map[google.LinkForm]int{}
	var hidden []google.Result
	var results int
	for _, serp := range serps {
		for _, one := range serp.Results {
			results++
			forms[one.Form]++
			if !one.Resolved() && one.Link != "" {
				hidden = append(hidden, one)
			}
		}
	}
	logf(t, "MEASUREMENT capture: %d pages, %d results, forms=%v, %d of them hiding the address",
		len(serps), results, forms, len(hidden))
	if len(hidden) < 4 {
		t.Skip("this capture hid too few addresses to measure the two ways of reading them")
	}

	origin := serps[0].Origin
	half := len(hidden) / 2

	// The two arms, one after the other rather than side by side: the same
	// origin is being asked either way, and two arms at once would measure how
	// it answers under load instead of who is asking.
	same := readBack(ctx, t, client.Transport, origin, hidden[:half])
	logf(t, "MEASUREMENT through the identity that captured the page: %s", same)

	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring the second identity: %v", err)
	}
	defer other.Release()
	fresh := other.Client()
	elsewhere := readBack(ctx, t, fresh.Transport, origin, hidden[half:])
	logf(t, "MEASUREMENT through another identity, which is what the program does: %s", elsewhere)
}

// readBack looks up every link through the transport given, and says what came
// of it: how many answered with an address, and what the rest answered with.
func readBack(ctx context.Context, t *testing.T, rt interface {
	RoundTrip(*http.Request) (*http.Response, error)
}, origin string, rs []google.Result) string {
	t.Helper()
	resolver := google.NewResolver(rt)
	resolver.Client.Timeout = 30 * time.Second

	var read int
	kinds := map[string]int{}
	for i := range rs {
		one := rs[i]
		copyOf := []google.Result{one}
		rep := resolver.ResolveResults(ctx, origin, copyOf, 1)
		if rep.Resolved == 1 {
			read++
			continue
		}
		kinds[kindOfResolveFailure(rep.Errs)]++
	}
	var said []string
	for kind, n := range kinds {
		said = append(said, kind+"×"+itoa(n))
	}
	return "read " + itoa(read) + " of " + itoa(len(rs)) + "; the rest: " + strings.Join(said, ", ")
}

// kindOfResolveFailure is the shape of a failure rather than its text, so a
// dozen of the same thing counts as a dozen of one thing.
func kindOfResolveFailure(errs []error) string {
	if len(errs) == 0 {
		return "no answer and no reason"
	}
	err := errs[0]
	text := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(text, "Client.Timeout"):
		return "timed out"
	case strings.Contains(text, "HTTP "):
		at := strings.Index(text, "HTTP ")
		return "answered " + strings.TrimSpace(hide(text[at:at+9]))
	case strings.Contains(text, "no Location"):
		return "answered without a Location"
	// The three ways an address that is dead, or dies mid-request, says so.
	case strings.Contains(text, "forcibly closed") || strings.Contains(text, "reset by peer"):
		return "the address dropped the connection"
	case strings.Contains(text, "proxyconnect"):
		return "the port would not carry it"
	case strings.Contains(text, "EOF"):
		return "the address answered nothing at all"
	case strings.Contains(text, "refused"):
		return "the address refused the connection"
	case strings.Contains(text, "tls") || strings.Contains(text, "certificate"):
		return "the address swapped the certificate"
	}
	return "failed: " + hide(shorten(text))
}

func shorten(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }

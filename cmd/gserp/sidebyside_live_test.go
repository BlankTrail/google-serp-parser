//go:build live

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// TestSideBySide_LiveTheSameAddressesThroughTheProductAndPastIt takes the
// comparison this whole investigation has been circling and makes it direct.
//
// Fifty addresses are first sifted without the product at all — a plain socks5
// dial and one request each, so what is kept is a set known to carry traffic at
// this minute. Then each of those is asked twice more, at the same minute: once
// through a port the product opens on it, and once past the product entirely.
//
// That separates the two things that have been indistinguishable all night. If
// the addresses answer past the product and fail through it, the product or the
// way this program opens its ports is at fault. If they fail both ways, the
// addresses were never as good as their number suggests, and no client can do
// better than the list allows.
//
// Note what the two arms can and cannot mean. Past the product, a plain client
// gets whatever Google serves a plain client — often a page with no results in
// it — so that arm is scored on whether the exchange completed at all, which is
// what "the address carries traffic" means. Through the product, the same
// request is scored twice: the exchange, and whether a page of results came
// back. Nothing is asserted; the three numbers report.
func TestSideBySide_LiveTheSameAddressesThroughTheProductAndPastIt(t *testing.T) {
	path := os.Getenv(envLiveDB)
	if path == "" {
		t.Skipf("%s is not set", envLiveDB)
	}
	o := serveOptions{DB: path}
	saved, ok := o.saved(io.Discard)
	if !ok || saved.APIKey == "" || saved.Proxy.Kind == "" {
		t.Skip("no connection and list are saved")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()

	ups, _, err := blanktrail.Source{
		Kind: saved.Proxy.Kind, Location: saved.Proxy.Location, DefaultScheme: listScheme,
	}.Load(ctx)
	if err != nil {
		t.Fatalf("loading the list: %v", o.clean(err.Error()))
	}
	t.Logf("%d addresses in the list", len(ups))

	// --- sift, without the product ------------------------------------------
	const want, sift = 50, 600
	good := siftDirect(ctx, t, ups, want, sift)
	t.Logf("sifted %d addresses that carried a request on their own", len(good))
	if len(good) == 0 {
		t.Skip("no address in the sample carried anything, so there is nothing to compare")
	}

	// --- the same addresses, both ways, at the same minute -------------------
	client, err := blanktrail.NewClient(saved.ControlURL, saved.APIKey)
	if err != nil {
		t.Fatalf("NewClient: %v", o.clean(err.Error()))
	}
	report, err := o.checked(ctx, client, len(good))
	if err != nil {
		t.Fatalf("checked: %v", o.clean(err.Error()))
	}

	var mu sync.Mutex
	var pastOK, throughOK, throughParsed int

	work := make(chan blanktrail.Upstream, len(good))
	for _, u := range good {
		work <- u
	}
	close(work)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range work {
				if ctx.Err() != nil {
					return
				}
				past := carriesDirect(ctx, u, "weather")
				through, parsed := carriesThroughProduct(ctx, client, report.CA, u)

				mu.Lock()
				if past {
					pastOK++
				}
				if through {
					throughOK++
				}
				if parsed {
					throughParsed++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	n := len(good)
	t.Logf("of %d addresses that had just carried a request on their own:", n)
	t.Logf("  past the product, again:              %2d/%d (%.0f%%)", pastOK, n, pct(pastOK, n))
	t.Logf("  through a port the product opened:    %2d/%d (%.0f%%)", throughOK, n, pct(throughOK, n))
	t.Logf("  and of those, a page of results came: %2d/%d (%.0f%%)", throughParsed, n, pct(throughParsed, n))
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

// siftDirect tries addresses without the product until it has enough that
// carry a request, or has looked at as many as it was allowed.
func siftDirect(ctx context.Context, t *testing.T, ups []blanktrail.Upstream, want, look int) []blanktrail.Upstream {
	t.Helper()
	if look > len(ups) {
		look = len(ups)
	}
	// Spread the sample over the list rather than taking the first few hundred:
	// a gateway hands out ports in blocks, and one block is not the list.
	step := len(ups) / look
	if step < 1 {
		step = 1
	}

	var mu sync.Mutex
	var good []blanktrail.Upstream
	work := make(chan blanktrail.Upstream, look)
	for i := 0; i < look; i++ {
		work <- ups[(i*step)%len(ups)]
	}
	close(work)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range work {
				mu.Lock()
				enough := len(good) >= want
				mu.Unlock()
				if enough || ctx.Err() != nil {
					return
				}
				if !carriesDirect(ctx, u, "weather") {
					continue
				}
				mu.Lock()
				if len(good) < want {
					good = append(good, u)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return good
}

// carriesDirect asks Google through the address itself, with no product in the
// way, and reports whether the exchange completed.
//
// What comes back is not judged: a plain client is served whatever Google
// serves a plain client, which is often a page with no results in it at all.
// The question here is only whether the address carries traffic.
func carriesDirect(ctx context.Context, u blanktrail.Upstream, phrase string) bool {
	var auth *proxy.Auth
	if u.User != "" {
		auth = &proxy.Auth{User: u.User, Password: u.Pass}
	}
	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(u.Host, u.Port), auth, &net.Dialer{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return false
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return false
	}

	cl := &http.Client{
		Timeout: 25 * time.Second,
		Transport: &http.Transport{
			DialContext:         contextDialer.DialContext,
			TLSClientConfig:     &tls.Config{},
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	q := google.Query{Text: phrase, Country: "us", Language: "en"}
	target, err := q.URL()
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) "+
			"Chrome/140.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", q.AcceptLanguage())
	resp, err := cl.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode > 0
}

// carriesThroughProduct opens one port on this address, asks the same thing
// through it, and reports whether the exchange completed and whether a page of
// results came back.
func carriesThroughProduct(ctx context.Context, cl *blanktrail.Client,
	ca *x509.CertPool, u blanktrail.Upstream) (carried, parsed bool) {
	pool, err := blanktrail.NewPool(ctx, blanktrail.PoolConfig{
		Client: cl, Threads: 1, PortsPerThread: 1,
		Spec: blanktrail.DefaultPortSpec(), CA: ca,
		Channels:            []blanktrail.Channel{blanktrail.NewListChannel("one", blanktrail.NewStaticRotor([]blanktrail.Upstream{u}))},
		NoKeepAlives:        true,
		MaxRetriesPerReq:    1,
		RotateAfterFailures: 1000,
		Cooldown:            time.Nanosecond,
	})
	if err != nil {
		return false, false
	}
	defer func() { _ = pool.Close() }()

	lease, err := pool.Acquire(ctx)
	if err != nil {
		return false, false
	}
	defer lease.Release()

	sess := google.NewSession(lease.Client().Transport)
	sess.Client.Timeout = lease.Client().Timeout
	serp, err := sess.Search(ctx, google.Query{Text: "weather", Country: "us", Language: "en"})
	if err != nil {
		return false, false
	}
	return true, len(serp.Results) > 0
}

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"time"
)

// RequestTrace is what one request through one port did, timed at the points
// where a request can actually be stuck.
//
// It exists because "the job is slow" has several causes that look identical
// from outside: a proxy that never completes the tunnel, one that completes it
// and then never answers, and a target that answers slowly are three different
// faults with three different remedies, and the only way to tell them apart is
// to time the request where it waits.
//
// The spans are cumulative from the start of the attempt, not from each other,
// so a zero says a stage never happened rather than that it was instant. Total
// is the whole attempt, and is set even when it failed.
type RequestTrace struct {
	Port    int
	Attempt int // 0 for the first try of a request, 1 upwards for its retries
	// Reused says the request went out on a connection that was already open, in
	// which case Connect and TLS are zero because neither happened again.
	Reused bool
	// Connect is when the tunnel to the proxy was established, TLS when the
	// handshake through it finished, Wrote when the request had been sent, and
	// FirstByte when the first byte of the response came back. A request that
	// hangs waiting for an answer shows Wrote set and FirstByte zero — which is a
	// different fault from one that never reaches Connect at all.
	Connect, TLS, Wrote, FirstByte, Total time.Duration
	// Status is what came back, or zero when nothing did; Err is why.
	Status int
	Err    error
}

// newBaseTransport builds the HTTP-CONNECT transport that talks to one proxy
// port. The port terminates the tunnelled TLS and presents a certificate chained
// to the proxy's CA, so the client must trust that CA.
func newBaseTransport(proxyHost string, port int, ca *x509.CertPool, insecure, noKeepAlives bool) *http.Transport {
	proxyURL := &url.URL{Scheme: "http", Host: net.JoinHostPort(proxyHost, strconv.Itoa(port))}
	tlsCfg := &tls.Config{}
	if insecure {
		tlsCfg.InsecureSkipVerify = true
	} else if ca != nil {
		tlsCfg.RootCAs = ca
	}
	return &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		TLSClientConfig:     tlsCfg,
		DisableKeepAlives:   noKeepAlives,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
}

// remedy is the narrow view of the pool that the ladder needs. Keeping it an
// interface is what lets the ladder be tested without opening a single port.
//
// Note the division of labour: the ladder knows HTTP and nothing else, while
// every policy decision — how many consecutive failures a port may collect
// before its egress is replaced, when a port is beyond saving — lives in the
// pool behind this interface.
type remedy interface {
	// attemptFailed records one failed attempt on a port and reports whether the
	// egress should be replaced now.
	attemptFailed(port int) (rotate bool)
	// attemptFailedStatus is attemptFailed for a failure that produced a
	// response, so the pool can consult CountFailure. attemptFailed stays for
	// transport errors, where there is no status to judge.
	attemptFailedStatus(port int, status int) (rotate bool)
	// attemptSucceeded clears the port's consecutive-failure count.
	attemptSucceeded(port int)
	rotateEgress(ctx context.Context, port int) error
	markBadEgress(port int)
	// exhausted reports that the whole retry budget was spent and the response is
	// still not usable.
	exhausted(port int)
	maxRetries() int
	wait(ctx context.Context, d time.Duration) error
}

// ladder is the RoundTripper that makes a port resilient. It repeats what is
// worth repeating, replaces the egress when the pool says the port has failed
// too often, and otherwise stays out of the way — the response body is passed
// to the caller unread.
type ladder struct {
	rt   http.RoundTripper
	port int
	rem  remedy
	// trace, when set, is told how each attempt went. It is nil unless somebody
	// asked for it: the timing hooks it installs cost a little on every request,
	// and a program nobody is debugging should not pay for them.
	trace func(RequestTrace)
}

func (t *ladder) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only idempotent, body-less requests are safe to replay. Anything else goes
	// through exactly once — replaying a POST could duplicate a side effect.
	replayable := (req.Method == http.MethodGet || req.Method == http.MethodHead) && req.Body == nil
	retryBudget := t.rem.maxRetries()
	if !replayable {
		retryBudget = 0
	}

	var delay time.Duration
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := t.rem.wait(req.Context(), delay); err != nil {
				return nil, err
			}
		}

		sent := req.Clone(req.Context())
		var mark *stopwatch
		if t.trace != nil {
			sent, mark = timed(sent)
		}
		resp, err := t.rt.RoundTrip(sent)
		if t.trace != nil {
			t.trace(mark.done(t.port, attempt, resp, err))
		}
		if err != nil {
			// The egress did not carry the request at all: blame it, not the origin.
			t.rem.markBadEgress(t.port)
			if t.rem.attemptFailed(t.port) {
				_ = t.rem.rotateEgress(req.Context(), t.port)
			}
			if attempt >= retryBudget {
				return nil, err
			}
			delay = backoff(attempt + 1)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.rem.attemptSucceeded(t.port)
			return resp, nil
		}

		// Any non-2xx counts against the port, whatever the reason, unless the
		// caller has said this particular status should not. A dead proxy, a
		// refused egress and a broken gateway are indistinguishable from here and
		// have the same remedy; the caller may know better about some statuses.
		if t.rem.attemptFailedStatus(t.port, resp.StatusCode) {
			_ = t.rem.rotateEgress(req.Context(), t.port)
		}

		if !Retryable(resp.StatusCode) {
			return resp, nil
		}
		if attempt >= retryBudget {
			t.rem.exhausted(t.port)
			return resp, nil
		}

		delay = backoff(attempt + 1)
		if after := RetryAfter(resp.Header); after > delay {
			delay = after
		}
		drainAndClose(resp)
	}
}

// drainAndClose consumes what is left of a response about to be discarded, so
// the underlying connection returns to the pool instead of being torn down.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// backoff returns an exponential delay with jitter for retry attempt n (n >= 1),
// capped at 8 seconds.
func backoff(n int) time.Duration {
	const base = 400 * time.Millisecond
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 5 {
		shift = 5
	}
	d := base << uint(shift)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stopwatch records when each stage of one request happened.
//
// The hooks fire on the transport's own goroutines, and the request they belong
// to is over before done is called, so the fields are written before they are
// read and no lock is needed between them.
type stopwatch struct {
	began                             time.Time
	reused                            bool
	connect, tls, wrote, firstByte    time.Duration
	connectAt, tlsAt, wroteAt, byteAt time.Time
}

// timed returns the request with timing hooks attached and the stopwatch that
// will hold what they saw.
func timed(req *http.Request) (*http.Request, *stopwatch) {
	w := &stopwatch{began: time.Now()}
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			w.reused = info.Reused
			if info.Reused {
				w.connectAt = time.Now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				w.connectAt = time.Now()
			}
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				w.tlsAt = time.Now()
			}
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { w.wroteAt = time.Now() },
		GotFirstResponseByte: func() { w.byteAt = time.Now() },
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), w
}

// done closes the stopwatch and reports what the attempt did.
func (w *stopwatch) done(port, attempt int, resp *http.Response, err error) RequestTrace {
	since := func(at time.Time) time.Duration {
		if at.IsZero() {
			return 0
		}
		return at.Sub(w.began)
	}
	out := RequestTrace{
		Port:      port,
		Attempt:   attempt,
		Reused:    w.reused,
		Connect:   since(w.connectAt),
		TLS:       since(w.tlsAt),
		Wrote:     since(w.wroteAt),
		FirstByte: since(w.byteAt),
		Total:     time.Since(w.began),
		Err:       err,
	}
	if resp != nil {
		out.Status = resp.StatusCode
	}
	return out
}

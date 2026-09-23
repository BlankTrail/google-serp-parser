// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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
	// Reason is the word the service put on an answer it composed itself —
	// why it could not carry this request — and is empty on every answer that
	// came from the far end.
	//
	// It is in the trace rather than only in the error because the two answer
	// different questions: the error says what happened to one request, and a
	// tally of these says what a run is up against. A run whose answers carry
	// no reason at all is meeting something neither side has a name for yet,
	// which is the most interesting thing this can say.
	Reason string
}

// newBaseTransport builds the HTTP-CONNECT transport that talks to one proxy
// port. The port terminates the tunnelled TLS and presents a certificate chained
// to the proxy's CA, so the client must trust that CA.
func newBaseTransport(proxyHost string, port int, protocol string, ca *x509.CertPool,
	insecure, noKeepAlives bool) *http.Transport {
	// The scheme has to be the one the port was opened as. A port opened for
	// SOCKS5 and dialled as an HTTP proxy answers nothing intelligible, and the
	// failure reads as the address being dead rather than as this program
	// talking the wrong protocol at it.
	proxyURL := &url.URL{
		Scheme: ProtocolOr(protocol),
		Host:   net.JoinHostPort(proxyHost, strconv.Itoa(port)),
	}
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
	// failed records one failure of a kind, for the reading a screen shows.
	failed(kind Failure)
	// attempted records that a request was put on the wire, which is what a
	// share of failures is read against.
	attempted()
	rotateEgress(ctx context.Context, port int) error
	// reopenPort asks for the port to be opened again as it is, keeping its
	// egress. It is the remedy for a port that is not there any more, which is
	// nothing to do with where the port was sending its traffic.
	reopenPort(port int)
	markBadEgress(port int)
	// markDeadEgress reports that the address did not carry the request at all,
	// which is final rather than a count towards anything.
	markDeadEgress(port int)
	// leaveAddress records one failure to carry a request and reports whether
	// this port should move to another address now.
	leaveAddress(port int) bool
	// hunt is how many addresses one request through this port may be carried
	// to before it gives up, which is a different budget from the retries a
	// refused answer gets.
	hunt(port int) int
	// stays says the port keeps its address whatever it meets: the session on it
	// has answered through that address and holds a clearance for it, and
	// carrying its request to another would spend the clearance for a
	// challenge somewhere else. An address that fails is still put to rest; the
	// request simply ends there.
	stays(port int) bool
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
	// Two budgets, because they answer two questions. hunted counts the
	// addresses this request has been carried to, and is spent finding one that
	// works; attempt counts the times a refused answer was asked for again, and
	// is spent waiting out something at the other end.
	hunted := 0
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
		t.rem.attempted()
		resp, err := t.rt.RoundTrip(sent)
		if t.trace != nil {
			told := mark.done(t.port, attempt, resp, err)
			if refusal, ours := serviceRefusal(resp); ours {
				told.Reason = refusal.Reason
			}
			t.trace(told)
		}
		if err != nil {
			kind := failureOf(err, 0)
			t.rem.failed(kind)
			if kind == FailurePort {
				// The proxy's own port did not answer, so nothing went through
				// it and the address behind it has done nothing. Blaming it here
				// is how one restart of the service put a whole list away.
				//
				// What the port does need is opening again. A listener that has
				// gone stays gone until somebody opens it and nothing else in
				// the pool ever looks, so without this the port fails this way
				// for the rest of the run — and one restart of the service takes
				// every port in the job with it.
				t.rem.reopenPort(t.port)
				return nil, err
			}
			// The request never arrived. Whether that is the address's fault is
			// the pool's to say: an address that has answered before is allowed
			// one miss, and one that has never answered is simply dead.
			if t.rem.leaveAddress(t.port) {
				t.rem.markDeadEgress(t.port)
				if !t.rem.stays(t.port) {
					_ = t.rem.rotateEgress(req.Context(), t.port)
				}
			}
			hunted++
			if hunted >= t.rem.hunt(t.port) {
				return nil, err
			}
			// No backoff here. There is nothing to wait out: either the address
			// is gone and the next one is a different machine entirely, or it is
			// one that works and has just hiccuped.
			delay = 0
			continue
		}

		// An answer the service composed about itself is not an answer at all:
		// the request never went out. It is read before the status is looked at,
		// because what it says the status cannot — a 523 the service wrote about
		// an address it could not reach and a 523 from something at the far end
		// are the same number about different things.
		if refusal, ours := serviceRefusal(resp); ours {
			drainAndClose(resp)
			switch {
			case errors.Is(refusal, ErrUpstreamUnreachable):
				// The address could not be reached. That is the address's, and
				// it is answered the way a request that never arrived is: leave
				// it and try another, with no pause in between — there is
				// nothing at the far end to wait out.
				t.rem.failed(FailureTransport)
				if t.rem.leaveAddress(t.port) {
					t.rem.markDeadEgress(t.port)
					if !t.rem.stays(t.port) {
						_ = t.rem.rotateEgress(req.Context(), t.port)
					}
				}
				hunted++
				if hunted >= t.rem.hunt(t.port) {
					return nil, refusal
				}
				delay = 0
				continue

			case errors.Is(refusal, ErrSolverWorking), errors.Is(refusal, ErrSolverBusy):
				// The challenge, not the road. A solve that outran this request
				// is still going and pins itself to this port, so the warm place
				// to ask from is the one we are standing on: leaving it throws
				// away the wait already paid for and starts the challenge again
				// somewhere else. A solver with nothing free is the same port
				// and the same address, a moment later.
				t.rem.failed(FailureTimeout)
				if attempt >= retryBudget {
					t.rem.exhausted(t.port)
					return nil, refusal
				}
				delay = solverAgain
				if errors.Is(refusal, ErrSolverBusy) {
					delay = solverQueueAgain
				}
				continue

			default:
				// Anything else the service says about itself — a road that is
				// down, a browser that tried and failed — is not this address's
				// to answer for, and leaving the address for it would spend a
				// list on something that was never in it.
				t.rem.failed(FailureTransport)
				return nil, refusal
			}
		}

		if answered(resp.StatusCode) {
			t.rem.attemptSucceeded(t.port)
			return resp, nil
		}

		t.rem.failed(failureOf(nil, resp.StatusCode))

		// Any non-2xx counts against the port, whatever the reason, unless the
		// caller has said this particular status should not. A dead proxy, a
		// refused egress and a broken gateway are indistinguishable from here and
		// have the same remedy; the caller may know better about some statuses.
		if t.rem.attemptFailedStatus(t.port, resp.StatusCode) && !t.rem.stays(t.port) {
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

// answered reports whether a response is the far end answering rather than the
// port failing to carry the request.
//
// A redirect is an answer. It used to be held against the port, on the reading
// that a search answered with one has been sent to Google's block page — and
// that reading is right about what the redirect means and wrong about who it
// blames. The port carried the request: something at the other end composed a
// reply and sent it back, which is the whole of what this layer can know.
//
// What the reply means belongs to whoever asked. That caller already says so:
// a search reads the page it lands on and rejects the identity when it is a
// refusal, which is the one place that can tell a challenge from a result. This
// layer guessing at it as well cost more than it ever caught. A hidden address
// is read out of the Location header of a redirect, so every lookup that works
// answers 302 — and three of those in a row took the address that was carrying
// them out of the port, by the same count that exists to walk away from a dead
// one. A session's first request is answered with a redirect too, from a
// country domain to another; that one cost an address every time it was
// followed by a single miss.
func answered(status int) bool {
	return status >= 200 && status < 400
}

// drainAndClose consumes what is left of a response about to be discarded, so
// the underlying connection returns to the pool instead of being torn down.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// backoff returns an exponential delay with jitter for retry attempt n (n >= 1),
// capped at 8 seconds.
// solverAgain is how long to wait before asking the same port again while a
// challenge it met is still being solved, and solverQueueAgain how long when
// the solver had nothing free to start one with.
//
// The first is short because the thing being waited for is nearly done — the
// solve is running and will pin this port — and the second is longer because
// what is being waited for is somebody else's solve finishing. Neither is a
// backoff against a far end that is angry with us: this is our own service
// asking for a moment.
const (
	solverAgain      = 3 * time.Second
	solverQueueAgain = 10 * time.Second
)

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
// The stages are kept as the moments they happened rather than as durations
// from the start: one subtraction at the end turns a moment into a duration,
// and a stage that never happened is a zero moment, which is how "it did not
// get that far" is told from "it took no time".
type stopwatch struct {
	began                             time.Time
	reused                            bool
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

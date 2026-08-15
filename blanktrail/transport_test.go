// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeRT replays a scripted sequence of responses and errors.
type fakeRT struct {
	steps []func() (*http.Response, error)
	calls int
}

func (f *fakeRT) RoundTrip(*http.Request) (*http.Response, error) {
	i := f.calls
	f.calls++
	if i >= len(f.steps) {
		i = len(f.steps) - 1
	}
	return f.steps[i]()
}

func respond(status int, h http.Header, body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		if h == nil {
			h = http.Header{}
		}
		return &http.Response{
			StatusCode: status,
			Header:     h.Clone(),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

func failWith(err error) func() (*http.Response, error) {
	return func() (*http.Response, error) { return nil, err }
}

// spyBody records whether anything read the body before the caller did, and how
// many times it was closed.
type spyBody struct {
	r      io.Reader
	read   *bool
	closed *int
}

func (s *spyBody) Read(p []byte) (int, error) { *s.read = true; return s.r.Read(p) }

func (s *spyBody) Close() error {
	if s.closed != nil {
		*s.closed++
	}
	return nil
}

// fakeRemedy records what the ladder asked for.
type fakeRemedy struct {
	failures       int
	successes      int
	rotations      int
	markedBad      int
	exhaustedCalls int
	waits          []time.Duration

	retries     int
	rotateOnNth int // attemptFailed returns true on this failure number (0 = never)
}

func (r *fakeRemedy) attemptFailed(int) bool {
	r.failures++
	return r.rotateOnNth > 0 && r.failures == r.rotateOnNth
}
func (r *fakeRemedy) attemptFailedStatus(port, _ int) bool    { return r.attemptFailed(port) }
func (r *fakeRemedy) attemptSucceeded(int)                    { r.successes++ }
func (r *fakeRemedy) rotateEgress(context.Context, int) error { r.rotations++; return nil }
func (r *fakeRemedy) markBadEgress(int)                       { r.markedBad++ }
func (r *fakeRemedy) exhausted(int)                           { r.exhaustedCalls++ }
func (r *fakeRemedy) maxRetries() int                         { return r.retries }

func (r *fakeRemedy) wait(_ context.Context, d time.Duration) error {
	r.waits = append(r.waits, d)
	return nil
}

func newReq(t *testing.T, method, body string) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "https://example.test/x", rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

func TestLadder_PassesSuccessThroughUntouched(t *testing.T) {
	rt := &fakeRT{steps: []func() (*http.Response, error){respond(200, nil, `{"ok":true}`)}}
	rem := &fakeRemedy{retries: 3}
	l := &ladder{rt: rt, port: 20001, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("body=%q, want the original payload", body)
	}
	if rt.calls != 1 {
		t.Errorf("transport calls=%d, want 1", rt.calls)
	}
	if rem.successes != 1 {
		t.Errorf("attemptSucceeded calls=%d, want 1", rem.successes)
	}
	if rem.failures+rem.rotations+rem.markedBad != 0 {
		t.Errorf("remedies applied to a successful response: %+v", rem)
	}
}

func TestLadder_DoesNotReadTheBodyOfASuccess(t *testing.T) {
	// The ladder used to buffer every response into memory to inspect it.
	// Bodies reach 16 MiB and every request produced one, so streaming straight
	// through to the caller is the point of this design.
	var wasRead bool
	rt := &fakeRT{steps: []func() (*http.Response, error){
		func() (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{},
				Body:       &spyBody{r: strings.NewReader("payload"), read: &wasRead},
			}, nil
		},
	}}
	l := &ladder{rt: rt, port: 20002, rem: &fakeRemedy{retries: 3}}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if wasRead {
		t.Fatal("the ladder read the response body; it must hand it to the caller unread")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "payload" {
		t.Errorf("body=%q, want the caller to get the whole payload", body)
	}
}

func TestLadder_RetriesServerErrorsThenSucceeds(t *testing.T) {
	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(503, nil, "unavailable"),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 3}
	l := &ladder{rt: rt, port: 20003, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status=%d, want 200 after the retry", resp.StatusCode)
	}
	if rem.failures != 1 || rem.successes != 1 {
		t.Errorf("failures=%d successes=%d, want 1 and 1", rem.failures, rem.successes)
	}
}

func TestLadder_DrainsAndClosesDiscardedResponses(t *testing.T) {
	// A response about to be replaced by a retry must be drained and closed, or
	// its connection is torn down instead of returning to the pool and every
	// retry pays for a fresh TCP and TLS handshake to the proxy port.
	var drained bool
	var closed int
	rt := &fakeRT{steps: []func() (*http.Response, error){
		func() (*http.Response, error) {
			return &http.Response{
				StatusCode: 503,
				Header:     http.Header{},
				Body:       &spyBody{r: strings.NewReader("unavailable"), read: &drained, closed: &closed},
			}, nil
		},
		respond(200, nil, "data"),
	}}
	l := &ladder{rt: rt, port: 20011, rem: &fakeRemedy{retries: 2}}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200 after the retry", resp.StatusCode)
	}
	if !drained {
		t.Error("the discarded response was not drained; its connection cannot be reused")
	}
	if closed != 1 {
		t.Errorf("discarded body Close calls=%d, want exactly 1", closed)
	}
}

func TestLadder_HonoursRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(429, h, "slow down"),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 3}
	l := &ladder{rt: rt, port: 20004, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(rem.waits) != 1 {
		t.Fatalf("waits=%v, want exactly one", rem.waits)
	}
	if rem.waits[0] < 7*time.Second {
		t.Errorf("waited %v, want at least the 7s the origin asked for", rem.waits[0])
	}
}

func TestLadder_DoesNotRepeatADefinitiveAnswer(t *testing.T) {
	rt := &fakeRT{steps: []func() (*http.Response, error){respond(404, nil, "not found")}}
	rem := &fakeRemedy{retries: 5}
	l := &ladder{rt: rt, port: 20005, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("status=%d, want 404 handed straight back", resp.StatusCode)
	}
	if rt.calls != 1 {
		t.Errorf("transport calls=%d, want 1: repeating a 404 cannot change it", rt.calls)
	}
	if rem.failures != 1 {
		t.Errorf("failures=%d, want 1: it still counts against the port", rem.failures)
	}
	if rem.exhaustedCalls != 0 {
		t.Errorf("exhausted calls=%d, want 0: the budget was not spent", rem.exhaustedCalls)
	}
}

func TestLadder_RotatesEgressWhenTheRemedySaysSo(t *testing.T) {
	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(500, nil, "boom"),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 3, rotateOnNth: 1}
	l := &ladder{rt: rt, port: 20006, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if rem.rotations != 1 {
		t.Errorf("rotations=%d, want 1", rem.rotations)
	}
}

func TestLadder_ExhaustsTheBudgetAndReturnsTheLastResponse(t *testing.T) {
	rt := &fakeRT{steps: []func() (*http.Response, error){respond(429, nil, "slow down")}}
	rem := &fakeRemedy{retries: 2}
	l := &ladder{rt: rt, port: 20007, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip returned an error instead of the last response: %v", err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("status=%d, want the last response handed back", resp.StatusCode)
	}
	if rt.calls != 3 {
		t.Errorf("transport calls=%d, want 3 (initial + 2 retries)", rt.calls)
	}
	if rem.exhaustedCalls != 1 {
		t.Errorf("exhausted calls=%d, want 1", rem.exhaustedCalls)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "slow down" {
		t.Errorf("body=%q, want the last body still readable", body)
	}
}

func TestLadder_DoesNotReplayRequestsWithABody(t *testing.T) {
	rt := &fakeRT{steps: []func() (*http.Response, error){respond(503, nil, "unavailable")}}
	rem := &fakeRemedy{retries: 5}
	l := &ladder{rt: rt, port: 20008, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodPost, `{"a":1}`))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 503 {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
	if rt.calls != 1 {
		t.Errorf("transport calls=%d, want 1: a request with a body goes through once", rt.calls)
	}
}

func TestLadder_ConnectionFailureBlamesTheEgress(t *testing.T) {
	boom := errors.New("dial tcp: connection refused")
	rt := &fakeRT{steps: []func() (*http.Response, error){
		failWith(boom),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 2, rotateOnNth: 1}
	l := &ladder{rt: rt, port: 20009, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status=%d, want 200 after rotating away from the dead proxy", resp.StatusCode)
	}
	if rem.markedBad != 1 {
		t.Errorf("markedBad=%d, want 1", rem.markedBad)
	}
	if rem.rotations != 1 {
		t.Errorf("rotations=%d, want 1", rem.rotations)
	}
}

func TestLadder_ConnectionFailurePersistsReturnsError(t *testing.T) {
	boom := errors.New("dial tcp: connection refused")
	rt := &fakeRT{steps: []func() (*http.Response, error){failWith(boom)}}
	rem := &fakeRemedy{retries: 1}
	l := &ladder{rt: rt, port: 20010, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); !errors.Is(err, boom) {
		t.Errorf("err=%v, want the underlying dial error", err)
	}
}

func TestBackoff_GrowsAndStaysBounded(t *testing.T) {
	for n := 1; n <= 8; n++ {
		d := backoff(n)
		if d <= 0 {
			t.Fatalf("backoff(%d)=%v, want a positive delay", n, d)
		}
		if d > 8*time.Second {
			t.Errorf("backoff(%d)=%v, want no more than 8s", n, d)
		}
	}
}

func TestSleepCtx_HonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled", err)
	}
}

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"io"
	"net"
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
	// kinds counts what the ladder named each failure, so a test can check that
	// a dead address and a wall are told apart rather than added together, and
	// attempts counts what went on the wire, which is what a share of failures
	// is read against.
	kinds    map[Failure]int
	attempts int

	failures       int
	successes      int
	rotations      int
	markedBad      int
	exhaustedCalls int
	waits          []time.Duration

	reopened   int
	markedDead int
	// keeps says the port's address has answered before, so a first miss is a
	// hiccup rather than a verdict.
	keeps  bool
	misses int

	retries     int
	rotateOnNth int // attemptFailed returns true on this failure number (0 = never)
	addresses   int // how many addresses one request may be carried to (0 = five)
}

func (r *fakeRemedy) attemptFailed(int) bool {
	r.failures++
	return r.rotateOnNth > 0 && r.failures == r.rotateOnNth
}
func (r *fakeRemedy) attemptFailedStatus(port, _ int) bool    { return r.attemptFailed(port) }
func (r *fakeRemedy) attemptSucceeded(int)                    { r.successes++ }
func (r *fakeRemedy) rotateEgress(context.Context, int) error { r.rotations++; return nil }
func (r *fakeRemedy) markBadEgress(int)                       { r.markedBad++ }
func (r *fakeRemedy) reopenPort(int)                          { r.reopened++ }
func (r *fakeRemedy) markDeadEgress(int)                      { r.markedDead++ }

func (r *fakeRemedy) leaveAddress(int) bool {
	r.misses++
	if !r.keeps {
		return true
	}
	return r.misses >= 2
}
func (r *fakeRemedy) exhausted(int)   { r.exhaustedCalls++ }
func (r *fakeRemedy) maxRetries() int { return r.retries }

func (r *fakeRemedy) hunt() int {
	if r.addresses > 0 {
		return r.addresses
	}
	return 5
}

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
	// Dead, not merely bad: the request never arrived, so the address gets no
	// count against it — it is put away.
	if rem.markedDead != 1 {
		t.Errorf("markedDead=%d, want 1", rem.markedDead)
	}
	if rem.markedBad != 0 {
		t.Errorf("markedBad=%d, want none: nothing came back to be refused", rem.markedBad)
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

// TestLadder_LeavesADeadAddressAtOnceRatherThanCountingToThree pins the rule
// the measurement bought.
//
// A transport error says this address did not carry the request. Repeating
// through it cannot change that, and on a live list it never did: 0 answers in
// 18 repeats, against 3 in 15 for the first request after a rotation. So the
// rotation happens on the failure itself, whatever the pool's own count of
// consecutive failures says — that count stays for what it is actually for,
// which is giving up on a port nothing can save.
func TestLadder_LeavesADeadAddressAtOnceRatherThanCountingToThree(t *testing.T) {
	boom := errors.New("read tcp: connection reset by peer")
	rt := &fakeRT{steps: []func() (*http.Response, error){
		failWith(boom),
		respond(200, nil, "data"),
	}}
	// The pool would not rotate for a long while yet, and the ladder rotates
	// anyway: a request that never left is not worth repeating down the same
	// route.
	rem := &fakeRemedy{retries: 2, rotateOnNth: 99}
	l := &ladder{rt: rt, port: 20021, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status=%d, want the 200 the retry got after moving address", resp.StatusCode)
	}
	if rem.rotations != 1 {
		t.Errorf("rotations=%d, want one on the failure itself", rem.rotations)
	}
	if rem.markedDead != 1 {
		t.Errorf("markedDead=%d, want the address put away once", rem.markedDead)
	}
}

// TestLadder_StillCountsToTheThresholdForARefusedAnswer keeps the other half of
// the rule honest: a response that arrived and was refused is not an address
// that failed to carry anything, and the pool's count is what decides there.
func TestLadder_StillCountsToTheThresholdForARefusedAnswer(t *testing.T) {
	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(503, nil, "nope"),
		respond(503, nil, "nope"),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 3, rotateOnNth: 99}
	l := &ladder{rt: rt, port: 20022, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if rem.rotations != 0 {
		t.Errorf("rotations=%d, want none: the pool's count had not been reached", rem.rotations)
	}
}

// TestLadder_CarriesARequestToFifteenAddressesBeforeGivingUp pins the hunt, and
// that it is a budget of its own.
//
// On a list where most addresses are dead — which is what a large cheap list is
// — finding a live one is the whole job, and each dead one costs about two
// seconds and no pause, because there is nothing at the other end to wait out.
// Five was not enough: a live trace of a job at fifty threads settled four
// queries a minute while the same list, hunted properly, answered ten times
// that.
func TestLadder_CarriesARequestToFifteenAddressesBeforeGivingUp(t *testing.T) {
	boom := errors.New("read tcp: connection reset by peer")
	steps := make([]func() (*http.Response, error), 0, 15)
	for range 14 {
		steps = append(steps, failWith(boom))
	}
	steps = append(steps, respond(200, nil, "data"))

	rt := &fakeRT{steps: steps}
	// The retry budget for a refused answer is small and must not bound this.
	rem := &fakeRemedy{retries: 1, addresses: 15}
	l := &ladder{rt: rt, port: 20023, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status=%d, want the 200 the fifteenth address gave", resp.StatusCode)
	}
	if rem.rotations != 14 {
		t.Errorf("rotations=%d, want one for each address that did not carry it", rem.rotations)
	}
	// And no waiting between them: a dead address is not a busy one.
	for i, d := range rem.waits {
		if d != 0 {
			t.Errorf("wait %d was %v, want none before trying another address", i+1, d)
		}
	}
}

// TestLadder_StopsHuntingAtTheBudget keeps the other end honest: the hunt is
// bounded, or a request through a list of nothing but dead addresses would walk
// the whole list.
func TestLadder_StopsHuntingAtTheBudget(t *testing.T) {
	boom := errors.New("EOF")
	steps := make([]func() (*http.Response, error), 0, 20)
	for range 20 {
		steps = append(steps, failWith(boom))
	}
	rt := &fakeRT{steps: steps}
	rem := &fakeRemedy{retries: 1, addresses: 3}
	l := &ladder{rt: rt, port: 20024, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the failure of the last address tried", err)
	}
	if rem.markedDead != 3 {
		t.Errorf("markedDead=%d, want the three the budget allowed", rem.markedDead)
	}
}

// TestLadder_KeepsAnAddressThatHasAnsweredThroughOneMiss pins the half of the
// rule that a live list is worth.
//
// Roughly one address in twelve on a large cheap list carries anything at all,
// so an address that has answered is the one thing worth having — and it answers
// every time it is asked: 60 of 60 across six of them, measured. A single miss
// on such an address is a hiccup, and handing it back for it means starting the
// search over.
func TestLadder_KeepsAnAddressThatHasAnsweredThroughOneMiss(t *testing.T) {
	boom := errors.New("EOF")
	rt := &fakeRT{steps: []func() (*http.Response, error){
		failWith(boom),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 1, addresses: 15, keeps: true}
	l := &ladder{rt: rt, port: 20025, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status=%d, want the 200 the same address gave on the second ask", resp.StatusCode)
	}
	if rem.rotations != 0 {
		t.Errorf("rotations=%d, want none: the address had answered before and missed once",
			rem.rotations)
	}
	if rem.markedDead != 0 {
		t.Errorf("markedDead=%d, want none: an address that works is not dead for one miss",
			rem.markedDead)
	}
}

// TestLadder_LeavesAnAddressThatHasAnsweredAfterTwoMissesInARow is the other
// end of it: kept through one, left on the second, which is where a hiccup
// stops being a hiccup.
func TestLadder_LeavesAnAddressThatHasAnsweredAfterTwoMissesInARow(t *testing.T) {
	boom := errors.New("EOF")
	rt := &fakeRT{steps: []func() (*http.Response, error){
		failWith(boom),
		failWith(boom),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 1, addresses: 15, keeps: true}
	l := &ladder{rt: rt, port: 20026, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if rem.rotations != 1 {
		t.Errorf("rotations=%d, want the one the second miss in a row calls for", rem.rotations)
	}
}

func (f *fakeRemedy) attempted() { f.attempts++ }

func (f *fakeRemedy) failed(kind Failure) {
	if f.kinds == nil {
		f.kinds = map[Failure]int{}
	}
	f.kinds[kind]++
}

func TestLadder_NamesEveryFailureItMeetsAndNoneOfTheSuccesses(t *testing.T) {
	// The counting has to happen where the failure does. Every other place that
	// could do it — the caller, the attempt, the screen — sees a query rather
	// than a request, and a query that succeeded on its fourth address would be
	// counted as one thing going well instead of three going wrong and one
	// going right.
	for _, tc := range []struct {
		name string
		step func() (*http.Response, error)
		want Failure
	}{
		{"never arrived", failWith(errors.New("read tcp: connection was forcibly closed")), FailureTransport},
		{"relay refused", respond(526, nil, ""), FailureRelay},
		{"walled", respond(403, nil, ""), FailureOther},
	} {
		rem := &fakeRemedy{keeps: true}
		l := &ladder{rt: &fakeRT{steps: []func() (*http.Response, error){tc.step}}, port: 1, rem: rem}
		resp, _ := l.RoundTrip(newReq(t, http.MethodGet, ""))
		if resp != nil {
			drainAndClose(resp)
		}
		// Every attempt that failed is one failure, and a ladder that walked
		// four addresses counts four: what is pinned here is the kind, and that
		// nothing else was counted alongside it.
		if rem.kinds[tc.want] < 1 {
			t.Errorf("%s: %q was not counted at all (counted %v)", tc.name, tc.want, rem.kinds)
		}
		if len(rem.kinds) != 1 {
			t.Errorf("%s: counted %v, want only %q", tc.name, rem.kinds, tc.want)
		}
	}

	// And an answer that succeeded is not a failure of any kind.
	rem := &fakeRemedy{keeps: true}
	l := &ladder{rt: &fakeRT{steps: []func() (*http.Response, error){respond(200, nil, "data")}}, port: 1, rem: rem}
	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	drainAndClose(resp)
	if len(rem.kinds) != 0 {
		t.Errorf("an answer that succeeded was counted as %v", rem.kinds)
	}
}

func TestLadder_OpensAPortAgainWhenItsOwnListenerHasGone(t *testing.T) {
	// A restart of the proxy service leaves every port in a running job with a
	// number nothing is listening on. Nothing else in the pool ever looks at a
	// port again on its own, so without this the job goes on dialling a socket
	// that is not there until somebody notices — which on a live run was every
	// port, for the rest of the run.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		func() (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		},
	}}
	rem := &fakeRemedy{retries: 3}
	l := &ladder{rt: rt, port: 20090, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err == nil {
		t.Fatal("a dial that failed came back as a success")
	}
	if rem.reopened != 1 {
		t.Errorf("the port was asked to reopen %d times, want once", rem.reopened)
	}
	// And the address is still blameless: it carried nothing, so it did nothing.
	if rem.markedDead != 0 || rem.rotations != 0 {
		t.Errorf("the address was blamed for a port that never answered: dead=%d rotations=%d", rem.markedDead, rem.rotations)
	}
}

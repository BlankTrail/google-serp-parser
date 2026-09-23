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
	// staying says the port keeps its address, as the pool says for a port
	// carrying a session that has answered.
	staying bool
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

func (r *fakeRemedy) hunt(int) int {
	if r.staying {
		return 1
	}
	if r.addresses > 0 {
		return r.addresses
	}
	return 5
}

func (r *fakeRemedy) stays(int) bool { return r.staying }

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

func (r *fakeRemedy) attempted() { r.attempts++ }

func (r *fakeRemedy) failed(kind Failure) {
	if r.kinds == nil {
		r.kinds = map[Failure]int{}
	}
	r.kinds[kind]++
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

func TestLadder_CreditsThePortWithARedirect(t *testing.T) {
	// A redirect is the far end answering, and this layer knows nothing else
	// about it. It used to be held against the port on the reading that a
	// search answered with one has been sent to a block page — right about the
	// meaning, wrong about the blame, and expensive: a hidden address is read
	// out of the Location header of a redirect, so three lookups that all
	// worked took the address that was carrying them out of the port. A
	// session's first request is answered with a redirect too, from one country
	// domain to another.
	//
	// The refusal a search really meets is read off the page it lands on, by
	// the layer that knows what a Google page says, and reaches the pool as a
	// rejected lease.
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		rt := &fakeRT{steps: []func() (*http.Response, error){respond(status, nil, "")}}
		rem := &fakeRemedy{retries: 3, rotateOnNth: 1}
		l := &ladder{rt: rt, port: 20101, rem: rem}

		resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
		if err != nil {
			t.Fatalf("RoundTrip on %d: %v", status, err)
		}
		if resp.StatusCode != status {
			t.Errorf("status=%d, want the caller handed the %d it was answered with", resp.StatusCode, status)
		}
		drainAndClose(resp)
		if rem.successes != 1 {
			t.Errorf("%d: attemptSucceeded calls=%d, want the port credited with an answer", status, rem.successes)
		}
		if rem.failures != 0 || rem.rotations != 0 || rem.markedBad != 0 {
			t.Errorf("%d: remedies applied to an answer: %+v", status, rem)
		}
	}
}

func TestLadder_StillHoldsARefusalAgainstThePort(t *testing.T) {
	// Only what the far end answered with is forgiven. A refusal and a rate
	// limit are the same faults they always were.
	for _, status := range []int{503, 429, 404} {
		rt := &fakeRT{steps: []func() (*http.Response, error){respond(status, nil, "")}}
		rem := &fakeRemedy{rotateOnNth: 1}
		l := &ladder{rt: rt, port: 20103, rem: rem}

		resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
		if err != nil {
			t.Fatalf("RoundTrip on %d: %v", status, err)
		}
		drainAndClose(resp)
		if rem.failures == 0 || rem.successes != 0 {
			t.Errorf("%d: failures=%d successes=%d, want it held against the port", status, rem.failures, rem.successes)
		}
	}
}

func TestLadder_KeepsAStayingPortOnItsAddressAndEndsTheRequestThere(t *testing.T) {
	// A session that has answered through an address holds a clearance for
	// it. Carried to another address its request would meet a challenge there
	// and the clearance would be spent; so the address that failed is put to
	// rest and the request ends, and the session waits for its address.
	boom := errors.New("read tcp: connection reset by peer")
	rt := &fakeRT{steps: []func() (*http.Response, error){
		failWith(boom),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 2, rotateOnNth: 99, staying: true}
	l := &ladder{rt: rt, port: 20021, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err == nil {
		t.Fatal("a staying port carried the request on and answered it")
	}
	if rem.rotations != 0 {
		t.Errorf("rotations=%d, want none: the port keeps its address", rem.rotations)
	}
	if rem.markedDead != 1 {
		t.Errorf("markedDead=%d, want the address put to rest all the same", rem.markedDead)
	}
}

func TestLadder_DoesNotMoveAStayingPortForARefusedAnswerEither(t *testing.T) {
	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(503, nil, "busy"),
		respond(200, nil, "data"),
	}}
	rem := &fakeRemedy{retries: 2, rotateOnNth: 1, staying: true}
	l := &ladder{rt: rt, port: 20021, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if rem.rotations != 0 {
		t.Errorf("rotations=%d, want none: the port keeps its address", rem.rotations)
	}
}

// serviceSays is an answer the proxy composed about itself rather than one it
// carried: the header names the reason.
func serviceSays(status int, reason string) func() (*http.Response, error) {
	h := http.Header{}
	h.Set(serviceErrorHeader, reason)
	return respond(status, h, "the service could not carry this")
}

func TestLadder_ReadsAnUnreachableAddressAsTheRoadAndNotAsAnAnswer(t *testing.T) {
	// The service saying it could not reach the address is not a page, and it
	// is not a refusal of the identity either: nothing went out. Answered as a
	// status it would be retried four times against an address that cannot be
	// reached at all, and the query that met it would be recorded as refused by
	// Google — which is the opposite of what happened.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		serviceSays(523, "upstream_unreachable"),
		respond(200, nil, "the page"),
	}}
	rem := &fakeRemedy{retries: 3, addresses: 5}
	l := &ladder{rt: rt, port: 20101, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("the request came back %d, want the page from the address it was carried to", resp.StatusCode)
	}
	for _, waited := range rem.waits {
		if waited > 0 {
			t.Errorf("the ladder waited %v before trying another address, want no pause: there is "+
				"nothing at the far end to wait out", waited)
		}
	}
	if rem.markedDead != 1 || rem.rotations != 1 {
		t.Errorf("the address was marked dead %d times and the port moved %d, want once each",
			rem.markedDead, rem.rotations)
	}
	if got := rem.kindsOf(FailureTransport); got != 1 {
		t.Errorf("%d failures were counted as a request that never arrived, want one", got)
	}
}

func TestLadder_DoesNotSpendTheAddressOnAServiceRefusalOfItsOwn(t *testing.T) {
	// A browser that tried and did not clear the challenge is about this
	// request and this identity, not about whether the address can be reached.
	// Answered by benching the address, a run would spend a list of fifteen
	// thousand on challenges it met.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		serviceSays(403, "solver_failed"),
	}}
	rem := &fakeRemedy{retries: 3, addresses: 5}
	l := &ladder{rt: rt, port: 20102, rem: rem}

	_, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if !errors.Is(err, ErrServiceRefused) {
		t.Fatalf("RoundTrip returned %v, want the service's own refusal", err)
	}
	if errors.Is(err, ErrUpstreamUnreachable) {
		t.Error("a refusal of the service's own was read as the address being unreachable")
	}
	if rem.markedDead != 0 || rem.rotations != 0 {
		t.Errorf("the address was marked dead %d times and the port moved %d, want neither",
			rem.markedDead, rem.rotations)
	}
	if rt.calls != 1 {
		t.Errorf("the request was put on the wire %d times, want the one: retrying is what the service "+
			"just said it could not do", rt.calls)
	}
}

func TestLadder_StopsHuntingWhereThePortKeepsItsAddress(t *testing.T) {
	// A port carrying a session that has answered stays where it is, so there
	// is nowhere to hunt to: one unreachable answer ends the request, and what
	// to do about the session is the run's to decide.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		serviceSays(523, "upstream_unreachable"),
		respond(200, nil, "never reached"),
	}}
	rem := &fakeRemedy{retries: 3, staying: true}
	l := &ladder{rt: rt, port: 20103, rem: rem}

	_, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if !errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("RoundTrip returned %v, want the address being unreachable", err)
	}
	if rem.rotations != 0 {
		t.Errorf("the port was moved %d times although it keeps its address", rem.rotations)
	}
	if rt.calls != 1 {
		t.Errorf("the request went out %d times, want the one it was allowed", rt.calls)
	}
}

func TestLadder_LeavesAPlain523FromTheFarEndAlone(t *testing.T) {
	// The number alone means nothing: a 523 without the header is something at
	// the far end answering, and reading it as the service's own would take an
	// address out of the list for a page somebody's server sent.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		respond(523, nil, "somebody else's error page"),
		respond(200, nil, "the page"),
	}}
	rem := &fakeRemedy{retries: 3, addresses: 5}
	l := &ladder{rt: rt, port: 20104, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if rem.markedDead != 0 {
		t.Errorf("the address was marked dead %d times for a page from the far end", rem.markedDead)
	}
	if len(rem.waits) == 0 {
		t.Error("the ladder retried without waiting, so it did not treat this as something to wait out")
	}
}

// kindsOf is how many failures of one kind the ladder named.
func (r *fakeRemedy) kindsOf(k Failure) int { return r.kinds[k] }

func TestLadder_AsksTheSamePortAgainWhileAChallengeIsStillBeingSolved(t *testing.T) {
	// A solve that outran the request it met is still going, and it pins itself
	// to the port that met it. Leaving for another address throws away the wait
	// already paid for and starts the challenge again somewhere else; asking the
	// same port again usually walks straight through.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		serviceSays(403, "solver_timeout"),
		respond(200, nil, "the page the solve cleared"),
	}}
	rem := &fakeRemedy{retries: 3, addresses: 5}
	l := &ladder{rt: rt, port: 20201, rem: rem}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("the request came back %d, want the page the second asking got", resp.StatusCode)
	}
	if rem.markedDead != 0 || rem.rotations != 0 {
		t.Errorf("the address was marked dead %d times and the port moved %d, want neither: the "+
			"challenge is not the address's doing", rem.markedDead, rem.rotations)
	}
	if len(rem.waits) != 1 || rem.waits[0] != solverAgain {
		t.Errorf("the ladder waited %v before asking again, want the one short wait for a solve to land",
			rem.waits)
	}
}

func TestLadder_WaitsLongerWhereTheSolverHadNothingFree(t *testing.T) {
	// No attempt was made at all: the queue is full or there is no window. The
	// address is not at fault and asking again at once asks the same thing of
	// the same queue.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		serviceSays(403, "solver_capacity"),
		respond(200, nil, "the page"),
	}}
	rem := &fakeRemedy{retries: 3, addresses: 5}
	l := &ladder{rt: rt, port: 20202, rem: rem}

	if _, err := l.RoundTrip(newReq(t, http.MethodGet, "")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if rem.markedDead != 0 || rem.rotations != 0 {
		t.Errorf("the address was spent on a solver with nothing free: dead %d, moved %d",
			rem.markedDead, rem.rotations)
	}
	if len(rem.waits) != 1 || rem.waits[0] != solverQueueAgain {
		t.Errorf("the ladder waited %v, want the longer wait for somebody else's solve to finish", rem.waits)
	}
}

func TestLadder_LeavesTheAddressAloneWhereTheRoadItselfIsDown(t *testing.T) {
	// The first hop being unreachable is every address at once. Walking the
	// list for it spends the whole list on one road.
	rt := &fakeRT{steps: []func() (*http.Response, error){
		serviceSays(523, "chain_unreachable"),
	}}
	rem := &fakeRemedy{retries: 3, addresses: 5}
	l := &ladder{rt: rt, port: 20203, rem: rem}

	_, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if !errors.Is(err, ErrChainUnreachable) {
		t.Fatalf("RoundTrip returned %v, want the road being unreachable", err)
	}
	if rem.markedDead != 0 || rem.rotations != 0 {
		t.Errorf("the address was marked dead %d times and the port moved %d for a road that is down",
			rem.markedDead, rem.rotations)
	}
	if rt.calls != 1 {
		t.Errorf("the request went out %d times, want the one: the road is the same for every address",
			rt.calls)
	}
}

func TestLadder_TellsTheTraceWhatTheServiceCalledIt(t *testing.T) {
	// A tally of these is what says what a run is up against, and an answer
	// carrying no reason at all is something neither side has a name for.
	var told []RequestTrace
	rt := &fakeRT{steps: []func() (*http.Response, error){
		serviceSays(403, "solver_timeout"),
		respond(429, nil, "google is cross"),
		respond(200, nil, "the page"),
	}}
	l := &ladder{rt: rt, port: 20204, rem: &fakeRemedy{retries: 3, addresses: 5},
		trace: func(tr RequestTrace) { told = append(told, tr) }}

	resp, err := l.RoundTrip(newReq(t, http.MethodGet, ""))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if len(told) != 3 {
		t.Fatalf("the trace was told about %d attempts, want the three that went out", len(told))
	}
	if told[0].Reason != "solver_timeout" {
		t.Errorf("the first attempt is traced as %q, want what the service called it", told[0].Reason)
	}
	for i, one := range told[1:] {
		if one.Reason != "" {
			t.Errorf("attempt %d came from the far end and is traced as %q, want no reason at all",
				i+2, one.Reason)
		}
	}
}

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParse_AcceptsEveryCommonListFormat(t *testing.T) {
	raw := `
# a comment
// another comment
; and another

socks5://user:pass@1.1.1.1:1080
http://2.2.2.2:8080
3.3.3.3:3128
u2:p2@4.4.4.4:4444
5.5.5.5:5555:u5:p5
[2001:db8::1]:6666
`
	ups, bad := Parse(raw, "socks5")
	if len(bad) != 0 {
		t.Fatalf("unparsable lines: %v", bad)
	}
	if len(ups) != 6 {
		t.Fatalf("got %d upstreams, want 6: %+v", len(ups), ups)
	}

	want := []string{
		"socks5://user:pass@1.1.1.1:1080",
		"http://2.2.2.2:8080",
		"socks5://3.3.3.3:3128",
		"socks5://u2:p2@4.4.4.4:4444",
		"socks5://u5:p5@5.5.5.5:5555",
		"socks5://[2001:db8::1]:6666",
	}
	for i, w := range want {
		if got := ups[i].URL(); got != w {
			t.Errorf("upstream %d URL=%q, want %q", i, got, w)
		}
	}
}

func TestParse_ReadsTheFourFieldFormWrittenInEitherOrder(t *testing.T) {
	// Lists are sold in both orders and neither marks which it is. A parser that
	// knows one of them turns every line of the other into an address named after
	// somebody's user name, which resolves nowhere and is reported as a proxy
	// that would not connect.
	for _, c := range []struct {
		line string
		want string
	}{
		{"1.1.1.1:8080:bob:secret", "socks5://bob:secret@1.1.1.1:8080"},
		{"bob:secret:1.1.1.1:8080", "socks5://bob:secret@1.1.1.1:8080"},
		{"http://proxy.example.test:3128:bob:secret", "http://bob:secret@proxy.example.test:3128"},
		{"http://bob:secret:proxy.example.test:3128", "http://bob:secret@proxy.example.test:3128"},
		// A password of digits alone fits both readings on the port test. The
		// field written like an address settles it.
		{"bob:1234:1.1.1.1:8080", "socks5://bob:1234@1.1.1.1:8080"},
	} {
		ups, bad := Parse(c.line, "socks5")
		if len(bad) != 0 {
			t.Errorf("%q was reported unusable", c.line)
			continue
		}
		if got := ups[0].URL(); got != c.want {
			t.Errorf("%q read as %q, want %q", c.line, got, c.want)
		}
	}
}

func TestParse_ReadsALineFittingBothOrdersAsAddressFirst(t *testing.T) {
	// Two addresses and two port numbers answer to both readings and nothing in
	// the line says which was meant. What matters is that the whole list is read
	// the same way: a rule decided line by line would read part of a list one way
	// and part the other, and nothing on any screen could tell them apart.
	ups, bad := Parse("1.1.1.1:8080:2.2.2.2:9090", "socks5")
	if len(bad) != 0 {
		t.Fatalf("the line was reported unusable: %v", bad)
	}
	if got, want := ups[0].URL(), "socks5://2.2.2.2:9090@1.1.1.1:8080"; got != want {
		t.Errorf("read as %q, want %q: the address comes first", got, want)
	}
}

func TestParse_ReportsAFourFieldLineWithNoPortInIt(t *testing.T) {
	// Four fields and not a port among them is not a proxy at all. Read as one,
	// it would sit in the rotation as an address nothing can connect to, and the
	// run would blame the failures on the proxy rather than on the line.
	ups, bad := Parse("one:two:three:four", "socks5")
	if len(ups) != 0 {
		t.Errorf("got %d upstreams, want none: %+v", len(ups), ups)
	}
	if len(bad) != 1 {
		t.Errorf("bad=%v, want the line reported", bad)
	}
}

func TestParse_ReportsBadLinesWithoutDroppingGoodOnes(t *testing.T) {
	ups, bad := Parse("1.1.1.1:1080\nnot a proxy\nftp://2.2.2.2:21\n3.3.3.3:9999", "socks5")
	if len(ups) != 2 {
		t.Errorf("got %d upstreams, want 2", len(ups))
	}
	if !reflect.DeepEqual(bad, []string{"not a proxy", "ftp://2.2.2.2:21"}) {
		t.Errorf("bad=%v, want the two unusable lines verbatim", bad)
	}
}

func TestParse_RejectsOutOfRangePort(t *testing.T) {
	ups, bad := Parse("1.1.1.1:70000", "socks5")
	if len(ups) != 0 {
		t.Errorf("got %d upstreams, want 0", len(ups))
	}
	if len(bad) != 1 {
		t.Errorf("bad=%v, want the out-of-range line reported", bad)
	}
}

func TestParse_HandlesEveryLineEndingConvention(t *testing.T) {
	// Proxy lists arrive from Windows (\r\n), from Unix (\n) and, rarely, from
	// tools that emit a lone \r. A splitter that only knows \n silently glues
	// neighbouring entries into one unparsable line and loses working proxies.
	raw := "1.1.1.1:1080\r\n2.2.2.2:2222\n3.3.3.3:3333\r4.4.4.4:4444"

	ups, bad := Parse(raw, "socks5")
	if len(bad) != 0 {
		t.Fatalf("bad=%v, want none: every line is a valid proxy", bad)
	}
	if len(ups) != 4 {
		t.Fatalf("got %d upstreams, want 4: %+v", len(ups), ups)
	}
	for i, want := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		if ups[i].Host != want {
			t.Errorf("upstream %d host=%q, want %q", i, ups[i].Host, want)
		}
	}
}

func TestSource_LoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")
	if err := os.WriteFile(path, []byte("1.1.1.1:1080\n2.2.2.2:1080\n"), 0o600); err != nil {
		t.Fatalf("write list: %v", err)
	}

	ups, bad, err := Source{Kind: "file", Location: path}.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(bad) != 0 {
		t.Errorf("bad=%v, want none", bad)
	}
	if len(ups) != 2 {
		t.Errorf("got %d upstreams, want 2", len(ups))
	}
}

func TestSource_LoadFromAnAddressAsksTheAddress(t *testing.T) {
	// The kind says how the location is read, and the two ways are not
	// interchangeable. A location read the wrong way answers with a refusal from
	// a file system or from a socket, and either one arrives as "no addresses" —
	// which is what an empty list looks like too.
	served := "1.1.1.1:1080\n2.2.2.2:1080\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, served)
	}))
	defer srv.Close()

	ups, bad, err := Source{Kind: "url", Location: srv.URL}.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(bad) != 0 {
		t.Errorf("bad=%v, want none", bad)
	}
	if len(ups) != 2 {
		t.Fatalf("got %d upstreams, want the 2 the address served", len(ups))
	}
	if got := ups[0].URL(); got != "socks5://1.1.1.1:1080" {
		t.Errorf("first upstream=%q, want the first line of what was served", got)
	}
}

func TestRotor_RoundRobinAndSkipsBurnedProxies(t *testing.T) {
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	r := NewStaticRotor(ups)

	var seen []string
	for i := 0; i < 3; i++ {
		u, ok := r.Next()
		if !ok {
			t.Fatal("Next returned false on a non-empty rotor")
		}
		seen = append(seen, u.Host)
	}
	if !reflect.DeepEqual(seen, []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}) {
		t.Errorf("round-robin order = %v, want the list order", seen)
	}

	// Burn the second proxy past the failure threshold.
	for i := 0; i < 3; i++ {
		r.MarkBad(ups[1])
	}
	seen = nil
	for i := 0; i < 4; i++ {
		u, _ := r.Next()
		seen = append(seen, u.Host)
	}
	for _, h := range seen {
		if h == "2.2.2.2" {
			t.Fatalf("burned proxy still handed out: %v", seen)
		}
	}
}

func TestRotor_ABenchedAddressRestsBeforeItIsOfferedAgain(t *testing.T) {
	// A refusal a minute ago rarely means the address is dead: it was busy, or
	// it was turned away for a while, and both pass. Handing it straight back
	// means beating on it, and dropping it forever throws away most of a list.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	r := NewStaticRotor(ups, WithRest(6*time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	if got := r.Benched(); got != 1 {
		t.Fatalf("Benched()=%d, want 1", got)
	}
	for i := 0; i < 4; i++ {
		u, ok := r.Next()
		if !ok {
			t.Fatal("Next reported an empty list")
		}
		if u.Key() == ups[0].Key() {
			t.Fatalf("the benched address was offered again after %d handouts", i+1)
		}
	}

	clock = clock.Add(6 * time.Hour)
	var seen bool
	for i := 0; i < 4 && !seen; i++ {
		u, _ := r.Next()
		seen = u.Key() == ups[0].Key()
	}
	if !seen {
		t.Error("the address never came back after its rest")
	}
	if got := r.Benched(); got != 0 {
		t.Errorf("Benched()=%d after the rest elapsed, want 0", got)
	}
}

func TestRotor_AReturningAddressGoesToTheBackOfTheList(t *testing.T) {
	// A returning address must not compete for a handout with addresses that
	// have never let anyone down. The back of the list is that statement, and
	// on a list of thousands it means the second chance comes much later.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	clock = clock.Add(time.Hour)

	// The first two handouts are the addresses that never failed; the returning
	// one comes after them, not in its original place at the front.
	var order []string
	for i := 0; i < 3; i++ {
		u, _ := r.Next()
		order = append(order, u.Host)
	}
	if order[len(order)-1] != "1.1.1.1" {
		t.Errorf("handout order %v, want the returning address last", order)
	}
}

func TestRotor_AReturningAddressDoesNotOvertakeTheCursor(t *testing.T) {
	// The cursor counts positions, so moving an address out of the stretch it
	// has already walked shifts everything behind it forward. Left uncorrected
	// the cursor lands on the returning address, which would hand it out first
	// of all — the opposite of the back of the list.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3\n4.4.4.4:4", "socks5")
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		if u, _ := r.Next(); u.Host != ups[i].Host {
			t.Fatalf("handout %d was %s, want %s", i+1, u.Host, ups[i].Host)
		}
	}
	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	clock = clock.Add(time.Hour)

	if u, _ := r.Next(); u.Host != "4.4.4.4" {
		t.Errorf("handout after the release was %s, want the address the cursor had reached", u.Host)
	}
	if u, _ := r.Next(); u.Host != "1.1.1.1" {
		t.Errorf("handout after that was %s, want the returning address", u.Host)
	}
}

func TestRotor_AFailureShortOfTheLimitDoesNotBenchAnAddress(t *testing.T) {
	// One refusal is noise. Benching on it would empty a healthy list.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	r := NewStaticRotor(ups, WithClock(func() time.Time { return clock }))

	r.MarkBad(ups[0])
	r.MarkBad(ups[0])
	if got := r.Benched(); got != 0 {
		t.Errorf("Benched()=%d after two failures of three, want 0", got)
	}
}

func TestRotor_AFailureReportedDuringARestDoesNotPushTheReturnBack(t *testing.T) {
	// Failures arrive from callers that took the address before it was benched,
	// so they keep coming for a while after it has left the rotation. If each
	// one restarted the rest, a busy list would hold its addresses far longer
	// than the rest it is configured with, and a steady trickle would hold one
	// out for good.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	clock = clock.Add(30 * time.Minute)
	r.MarkBad(ups[0])
	r.MarkBad(ups[0])
	clock = clock.Add(30 * time.Minute)

	var seen bool
	for i := 0; i < 3 && !seen; i++ {
		u, _ := r.Next()
		seen = u.Key() == ups[0].Key()
	}
	if !seen {
		t.Error("the address did not come back an hour after it was benched")
	}
}

func TestRotor_HandsOutTheLongestRestedWhenEveryAddressIsBenched(t *testing.T) {
	// A list where everything has failed recently is a systemic fault, not a
	// list problem, and the caller still has to be told something. The address
	// that has rested longest is the one closest to eligible, so it is the
	// honest choice — and it keeps the rest meaningful for all the others,
	// which forgiving the whole list would not.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	r := NewStaticRotor(ups, WithRest(6*time.Hour), WithClock(func() time.Time { return clock }))

	// The one benched first sits in the middle of the list, so neither end of it
	// can be mistaken for the answer.
	for _, u := range []Upstream{ups[1], ups[0], ups[2]} {
		for i := 0; i < 3; i++ {
			r.MarkBad(u)
		}
		clock = clock.Add(time.Minute)
	}

	u, ok := r.Next()
	if !ok {
		t.Fatal("Next gave up on a fully benched list instead of offering the longest rested")
	}
	if u.Key() != ups[1].Key() {
		t.Errorf("offered %s, want the one benched first", u.Host)
	}
	if got := r.Benched(); got != 2 {
		t.Errorf("Benched()=%d, want only the offered address released and the others still resting", got)
	}
}

func TestRotor_ANoughtRestIsNoRestAndANilClockLeavesTheDefault(t *testing.T) {
	// Nought used to mean "as it comes", on the reasoning that options are read
	// from configuration where an unset field is nought. That reasoning has
	// moved: the settings carry the default themselves now, so a nought reaching
	// here is somebody who typed one — an operator saying no address is ever put
	// away — and reading it as an hour made the one setting that can be switched
	// off look as though it could not.
	//
	// A nil clock is the other case and unchanged: nothing was said, so the
	// rotor keeps its own.
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	r := NewStaticRotor(ups, WithRest(0), WithClock(nil))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		u, ok := r.Next()
		if !ok {
			t.Fatal("Next reported an empty list")
		}
		seen[u.Key()] = true
	}
	if !seen[ups[0].Key()] {
		t.Error("the address that failed was put away although no rest was asked for")
	}
	if resting := r.RestingHere(); resting != 0 {
		t.Errorf("%d addresses are resting although the rest is nought", resting)
	}
}

func TestRotor_AReloadedListDoesNotRestartOrCancelARest(t *testing.T) {
	// The rest belongs to the address, not to the copy of the list it was read
	// from. A source that reloads every few minutes would otherwise wipe every
	// rest it holds, and one that reloads often enough would never let a rest
	// run out at all.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	clock = clock.Add(30 * time.Minute)
	reloaded, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	r.reconcile(reloaded)

	if got := r.Benched(); got != 1 {
		t.Fatalf("Benched()=%d after a reload that still lists the address, want 1", got)
	}
	for i := 0; i < 3; i++ {
		if u, _ := r.Next(); u.Key() == ups[0].Key() {
			t.Fatal("the reload cancelled the rest")
		}
	}

	clock = clock.Add(30 * time.Minute)
	var seen bool
	for i := 0; i < 3 && !seen; i++ {
		u, _ := r.Next()
		seen = u.Key() == ups[0].Key()
	}
	if !seen {
		t.Error("the address did not come back an hour after it was benched, so the reload restarted its rest")
	}
}

func TestRotor_AReloadForgetsAnAddressItNoLongerLists(t *testing.T) {
	// An address the source has dropped cannot be handed out again, so its rest
	// is a record of nothing. Kept, it would inflate the count of resting
	// addresses for as long as the rotor lives.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	reloaded, _ := Parse("2.2.2.2:2\n3.3.3.3:3", "socks5")
	r.reconcile(reloaded)

	if got := r.Benched(); got != 0 {
		t.Errorf("Benched()=%d after a reload that dropped the address, want 0", got)
	}
}

func TestRotor_AReloadThatReordersTheListKeepsEachRestWithItsOwnAddress(t *testing.T) {
	// A reload replaces the list wholesale and the source is free to return the
	// same addresses in a different order. The records are held per address, so
	// nothing may be looked up by the position an address happens to occupy: read
	// by position, a reordering reload would move one address's rest onto another
	// and hand out the one that is resting.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	reloaded, _ := Parse("3.3.3.3:3\n2.2.2.2:2\n1.1.1.1:1", "socks5")
	r.reconcile(reloaded)

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		u, ok := r.Next()
		if !ok {
			t.Fatal("Next reported an empty list")
		}
		seen[u.Host]++
	}
	if seen["1.1.1.1"] != 0 {
		t.Errorf("the resting address was handed out %d times after a reload reordered the list", seen["1.1.1.1"])
	}
	if seen["2.2.2.2"] != 3 || seen["3.3.3.3"] != 3 {
		t.Errorf("handouts %v, want the two addresses that never failed shared evenly", seen)
	}
}

func TestRotor_ARecordStillMatchesItsAddressAfterAReleaseReordersTheList(t *testing.T) {
	// Releasing an address moves it to the back, which shifts every address it
	// passed. A record read by position rather than by address would survive the
	// first release and only go wrong at the next failure, benching a bystander
	// and leaving the address that actually failed in the rotation.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
	}
	clock = clock.Add(time.Hour)
	if _, ok := r.Next(); !ok { // the rest ends here and reorders the list
		t.Fatal("Next reported an empty list")
	}

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[2])
	}
	if got := r.Benched(); got != 1 {
		t.Fatalf("Benched()=%d after failing one address, want 1", got)
	}

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		u, _ := r.Next()
		seen[u.Host]++
	}
	if seen["3.3.3.3"] != 0 {
		t.Errorf("the address that failed was handed out %d times", seen["3.3.3.3"])
	}
	if seen["1.1.1.1"] == 0 || seen["2.2.2.2"] == 0 {
		t.Errorf("handouts %v, want both addresses that are not resting still in the rotation", seen)
	}
}

func TestRotor_OneRestEndingDoesNotDelayTheNextOne(t *testing.T) {
	// Addresses are benched as they fail, so their rests end one after another,
	// each on its own clock. Serving one rest must not move when the rest behind
	// it is served: an address held past its rest is out of the rotation for
	// longer than the rotor is configured to hold it, and on a list that fails
	// steadily every rest after the first would drift further out.
	clock := time.Unix(1700000000, 0)
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2\n3.3.3.3:3", "socks5")
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))

	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0]) // rests until the hour
	}
	clock = clock.Add(30 * time.Minute)
	for i := 0; i < 3; i++ {
		r.MarkBad(ups[1]) // rests until half an hour after that
	}

	clock = clock.Add(30 * time.Minute)
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		u, _ := r.Next()
		seen[u.Host] = true
	}
	if !seen["1.1.1.1"] {
		t.Fatal("the first address did not come back when its rest ended")
	}
	if seen["2.2.2.2"] {
		t.Fatal("the second address came back half an hour early")
	}

	clock = clock.Add(30 * time.Minute)
	var back bool
	for i := 0; i < 4 && !back; i++ {
		u, _ := r.Next()
		back = u.Host == "2.2.2.2"
	}
	if !back {
		t.Error("the second address never came back, so serving the first rest pushed it out")
	}
}

func TestRotor_EmptyListReportsFalse(t *testing.T) {
	if _, ok := NewStaticRotor(nil).Next(); ok {
		t.Error("Next on an empty rotor returned true")
	}
}

func TestRotor_CloseIsIdempotentAndConcurrencySafe(t *testing.T) {
	// A static rotor has no background loop, but Close must still be safe: Rotor
	// is a public type, and a second Close used to panic on a closed channel.
	NewStaticRotor(nil).Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")
	if err := os.WriteFile(path, []byte("1.1.1.1:1080\n"), 0o600); err != nil {
		t.Fatalf("write list: %v", err)
	}
	live, err := NewRotor(context.Background(), Source{Kind: "file", Location: path, Refresh: time.Hour})
	if err != nil {
		t.Fatalf("NewRotor: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			live.Close()
		}()
	}
	wg.Wait()
}

// listOfSize builds a list the size a bought one actually is. The addresses are
// from the documentation range, one per port.
func listOfSize(n int) []Upstream {
	ups := make([]Upstream, n)
	for i := range ups {
		ups[i] = Upstream{Scheme: "socks5", Host: "192.0.2.1", Port: strconv.Itoa(i + 1)}
	}
	return ups
}

func BenchmarkRotorNextOnAFullSizeList(b *testing.B) {
	r := NewStaticRotor(listOfSize(15000))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Next(); !ok {
			b.Fatal("Next reported an empty list")
		}
	}
}

func BenchmarkRotorNextWhenNearlyEveryAddressIsResting(b *testing.B) {
	ups := listOfSize(15000)
	clock := time.Unix(1700000000, 0)
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))
	for _, u := range ups[:len(ups)-1] {
		for i := 0; i < 3; i++ {
			r.MarkBad(u)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Next(); !ok {
			b.Fatal("Next reported an empty list")
		}
	}
}

// BenchmarkRotorNextWithHalfTheListRestingAndNothingDue is the steady state of
// a list that has been in use for a while: a large share of it is resting, no
// rest is anywhere near over, and the cursor still finds a free address within a
// step or two. The other benchmarks cannot show what that costs — one has an
// empty bench, and the one with a full bench spends nearly all its time walking
// past resting addresses, which hides everything else.
func BenchmarkRotorNextWithHalfTheListRestingAndNothingDue(b *testing.B) {
	ups := listOfSize(15000)
	clock := time.Unix(1700000000, 0)
	r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))
	for j := 0; j < len(ups); j += 2 {
		for i := 0; i < 3; i++ {
			r.MarkBad(ups[j])
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Next(); !ok {
			b.Fatal("Next reported an empty list")
		}
	}
}

func BenchmarkRotorNextReleasingATenthOfAFullSizeList(b *testing.B) {
	ups := listOfSize(15000)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		clock := time.Unix(1700000000, 0)
		r := NewStaticRotor(ups, WithRest(time.Hour), WithClock(func() time.Time { return clock }))
		for j := 0; j < len(ups); j += 10 {
			for k := 0; k < 3; k++ {
				r.MarkBad(ups[j])
			}
		}
		clock = clock.Add(time.Hour)
		b.StartTimer()
		if _, ok := r.Next(); !ok {
			b.Fatal("Next reported an empty list")
		}
	}
}

func TestRotor_MarkDeadPutsAnAddressAwayAtOnce(t *testing.T) {
	// A request that never arrived says something final about the address, and
	// counting to three before acting on it spends two more requests that cannot
	// succeed. Measured on a live list: a repeat through an address that has
	// just failed answered 0 of 18, and an address that does answer keeps
	// answering — 60 of 60.
	ups, bad := Parse("10.0.0.1:1080\n10.0.0.2:1080", "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	r := NewStaticRotor(ups)

	r.MarkDead(ups[0])
	if got := r.Benched(); got != 1 {
		t.Fatalf("%d addresses are resting after one was found dead, want one", got)
	}
	// And it is not handed out again while it rests: every turn of the rotation
	// comes back with the other one.
	for i := range 4 {
		got, ok := r.Next()
		if !ok {
			t.Fatalf("turn %d: the rotor has nothing left, and one address is fine", i+1)
		}
		if got.Key() == ups[0].Key() {
			t.Errorf("turn %d handed out the address that did not carry a request", i+1)
		}
	}
}

func TestRotor_MarkBadStillCountsToTheThreshold(t *testing.T) {
	// The other half of the rule. An answer that was refused came back, so the
	// address carried it: what was refused is about the target, and one refusal
	// is not a reason to spend an address.
	ups, _ := Parse("10.0.0.1:1080\n10.0.0.2:1080", "socks5")
	r := NewStaticRotor(ups)

	r.MarkBad(ups[0])
	if got := r.Benched(); got != 0 {
		t.Errorf("%d addresses rest after one refused answer, want none", got)
	}
	r.MarkBad(ups[0])
	r.MarkBad(ups[0])
	if got := r.Benched(); got != 1 {
		t.Errorf("%d addresses rest after three refusals, want the one", got)
	}
}

func TestRotor_RestingAndRestoreCarryABenchAcrossARestart(t *testing.T) {
	// A program restarted without its bench walks straight back into every
	// address it spent yesterday learning to avoid, which on a list where most
	// are dead is most of the first hour.
	ups, _ := Parse("10.0.0.1:1080\n10.0.0.2:1080\n10.0.0.3:1080", "socks5")
	clock := time.Now()
	first := NewStaticRotor(ups, WithClock(func() time.Time { return clock }))
	first.MarkDead(ups[0])

	rests := first.Resting()
	if len(rests) != 1 {
		t.Fatalf("the bench holds %d addresses, want the one that was found dead", len(rests))
	}

	// A new rotor, as a restart builds: it knows nothing until it is told.
	next := NewStaticRotor(ups, WithClock(func() time.Time { return clock.Add(time.Minute) }))
	if got := next.Benched(); got != 0 {
		t.Fatalf("a fresh rotor already rests %d addresses", got)
	}
	next.Restore(rests)
	if got := next.Benched(); got != 1 {
		t.Errorf("%d addresses rest after the bench was carried over, want one", got)
	}

	// And a rest that has already run out is not restarted: it belongs to the
	// address, not to the program that wrote it down.
	late := NewStaticRotor(ups, WithClock(func() time.Time { return clock.Add(defaultRest + time.Minute) }))
	late.Restore(rests)
	if got := late.Benched(); got != 0 {
		t.Errorf("%d addresses rest after their rest had already elapsed", got)
	}
}

func TestRotor_OnBenchIsToldEachAddressPutAway(t *testing.T) {
	// The hook exists so the bench can be written down as it is made rather than
	// swept up at shutdown, which a program that is killed never reaches.
	ups, _ := Parse("10.0.0.1:1080\n10.0.0.2:1080", "socks5")
	var seen []string
	r := NewStaticRotor(ups, WithOnBench(func(key string, _ time.Time) {
		seen = append(seen, key)
	}))

	r.MarkDead(ups[0])
	r.MarkDead(ups[0]) // already resting: said once, not twice
	if len(seen) != 1 || seen[0] != ups[0].Key() {
		t.Errorf("the hook heard %v, want the one address put away once", seen)
	}
}

func TestRotor_NeverRestsTheWholeListAtOnce(t *testing.T) {
	// A list is not spent because every address in it has missed once. On a
	// backconnect gateway a miss is about the minute rather than the door, and a
	// bench with no ceiling turns a bad ten minutes into a pool with nothing to
	// hand out: measured live, 14999 of fifteen thousand addresses were resting
	// at once, 92 per cent of every request never arrived, and the run was down
	// to thirteen queries a minute — because the only addresses left to offer
	// were ones already known bad.
	lines := make([]string, 0, 20)
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("10.0.0.%d:1080", i+1))
	}
	ups, bad := Parse(strings.Join(lines, "\n"), "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	clock := newFakeClock()
	r := NewStaticRotor(ups)
	r.now = clock.Now

	// Every address in the list is found dead, one after another.
	for _, u := range ups {
		clock.Advance(time.Second)
		r.MarkDead(u)
	}

	resting := r.RestingHere()
	ceiling := int(float64(len(ups)) * benchShare)
	if resting > ceiling {
		t.Errorf("%d of %d addresses are resting, want no more than %d",
			resting, len(ups), ceiling)
	}
	if resting == len(ups) {
		t.Fatal("the whole list is resting: there is nothing left to hand out")
	}

	// And what came back is what had rested longest — whatever was true when it
	// was put away is the least likely to still be true.
	back := r.Resting()
	for _, u := range ups[:len(ups)-ceiling] {
		if _, still := back[u.Key()]; still {
			t.Errorf("%s was put away first and is still resting while later ones came back",
				u.Key())
		}
	}

	// The rotor can still hand something out, which is the whole point.
	if _, ok := r.Next(); !ok {
		t.Error("the rotor has nothing to hand out after every address missed once")
	}
}

func TestDefaultRest_KeepsARunClearOfWhatItHasAlreadyFoundDead(t *testing.T) {
	// Six hours was a sentence passed on evidence that expires in minutes, and
	// five minutes was the other way over: an address found dead came back
	// before the run had finished walking the rest of the list, so the same dead
	// addresses were tried again and again inside one job.
	//
	// What keeps a list from being spent is the ceiling on the bench rather than
	// the length of the rest, which is what lets this be an hour.
	if defaultRest < 30*time.Minute {
		t.Errorf("an address rests only %s, which brings it back inside the job "+
			"that found it dead", defaultRest)
	}
	if defaultRest > 4*time.Hour {
		t.Errorf("an address rests %s on evidence about one minute", defaultRest)
	}
}

func TestRotorNext_SpreadsTheWorkOverTheWholeListRatherThanFavouringAny(t *testing.T) {
	// What a rotation is for. An address handed out twice while another has not
	// been handed out at all is a list being worked like a shorter one: the
	// favoured addresses collect the attention of the origin, and the rest sit
	// there paid for and idle.
	//
	// The check is on the counts rather than on the order, because the order is
	// the rotor's own business — what has to hold is that after a full pass no
	// address has been used more than one time more than any other.
	const addresses, handouts = 10, 100
	lines := make([]string, 0, addresses)
	for i := range addresses {
		lines = append(lines, fmt.Sprintf("10.0.0.%d:1080", i+1))
	}
	ups, bad := Parse(strings.Join(lines, "\n"), "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	r := NewStaticRotor(ups)

	used := map[string]int{}
	for range handouts {
		u, ok := r.Next()
		if !ok {
			t.Fatal("the rotor ran out of addresses to hand out")
		}
		used[u.Key()]++
	}

	if len(used) != addresses {
		t.Errorf("%d of %d addresses were ever handed out", len(used), addresses)
	}
	low, high := handouts, 0
	for _, u := range ups {
		n := used[u.Key()]
		if n < low {
			low = n
		}
		if n > high {
			high = n
		}
	}
	if high-low > 1 {
		t.Errorf("the most-used address was handed out %d times and the least-used %d: "+
			"%d handouts over %d addresses should differ by at most one",
			high, low, handouts, addresses)
	}
}

func TestRotorNext_KeepsSpreadingOverWhatIsLeftWhileSomeAddressesRest(t *testing.T) {
	// A resting address is skipped, and the ones still in play have to go on
	// sharing the work evenly between them rather than the cursor settling on
	// whichever follows the gap.
	const addresses, handouts = 8, 60
	lines := make([]string, 0, addresses)
	for i := range addresses {
		lines = append(lines, fmt.Sprintf("10.0.1.%d:1080", i+1))
	}
	ups, bad := Parse(strings.Join(lines, "\n"), "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	r := NewStaticRotor(ups)
	r.MarkDead(ups[2])
	r.MarkDead(ups[5])

	used := map[string]int{}
	for range handouts {
		u, ok := r.Next()
		if !ok {
			t.Fatal("the rotor ran out of addresses to hand out")
		}
		used[u.Key()]++
	}

	for _, resting := range []int{2, 5} {
		if n := used[ups[resting].Key()]; n != 0 {
			t.Errorf("a resting address was handed out %d times", n)
		}
	}
	low, high := handouts, 0
	for i, u := range ups {
		if i == 2 || i == 5 {
			continue
		}
		n := used[u.Key()]
		if n < low {
			low = n
		}
		if n > high {
			high = n
		}
	}
	if high-low > 1 {
		t.Errorf("of the addresses still in play the most-used had %d handouts and "+
			"the least-used %d, want a difference of at most one", high, low)
	}
}

func TestNewRotor_TakesTheLengthOfABanFromWhoeverOpensIt(t *testing.T) {
	// The default is an hour, and how fast a gateway's exits turn over is a
	// property of the list rather than of this program — so the number is the
	// operator's, and the rotor has to actually keep the one it is handed.
	ups, bad := Parse(strings.Join([]string{"1.1.1.1:1", "2.2.2.2:2"}, "\n"), "socks5")
	if len(bad) > 0 {
		t.Fatalf("Parse rejected %v", bad)
	}
	clock := newFakeClock()

	r := NewStaticRotor(ups, WithRest(90*time.Minute), WithClock(clock.Now))
	r.MarkDead(ups[0])
	if got := r.RestingHere(); got != 1 {
		t.Fatalf("%d addresses are resting, want the one found dead", got)
	}

	// An hour in, the address a ninety-minute ban covers is still out.
	clock.Advance(time.Hour)
	if _, ok := r.Next(); !ok {
		t.Fatal("the rotor has nothing to hand out")
	}
	if got := r.RestingHere(); got != 1 {
		t.Errorf("the address came back after an hour of a ninety-minute ban")
	}

	// And past it, back it comes.
	clock.Advance(31 * time.Minute)
	if _, ok := r.Next(); !ok {
		t.Fatal("the rotor has nothing to hand out")
	}
	if got := r.RestingHere(); got != 0 {
		t.Errorf("%d addresses are still resting after the ban ran out", got)
	}
}

func TestRotor_WithNoRestNothingIsEverPutAway(t *testing.T) {
	// Nought minutes of ban is an operator saying the whole list stays in play.
	// A bench of no length would mean "put away until the next sweep" instead,
	// which is a different thing said by accident.
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	r := NewStaticRotor(ups, WithRest(0))
	first, ok := r.Next()
	if !ok {
		t.Fatal("a fresh rotor handed out nothing")
	}
	r.MarkDead(first)

	if resting := r.RestingHere(); resting != 0 {
		t.Errorf("%d addresses are resting although the rest is nought", resting)
	}
	// And it is still offered: two addresses, and both of them come round.
	seen := map[string]bool{}
	for range 4 {
		u, ok := r.Next()
		if !ok {
			t.Fatal("the rotor ran out although nothing was put away")
		}
		seen[u.Key()] = true
	}
	if len(seen) != 2 {
		t.Errorf("%d of two addresses were offered after one was marked dead", len(seen))
	}
}

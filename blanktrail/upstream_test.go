// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

func TestRotor_ForgivesEveryoneWhenAllAreBurned(t *testing.T) {
	ups, _ := Parse("1.1.1.1:1\n2.2.2.2:2", "socks5")
	r := NewStaticRotor(ups)
	for i := 0; i < 3; i++ {
		r.MarkBad(ups[0])
		r.MarkBad(ups[1])
	}

	if _, ok := r.Next(); !ok {
		t.Error("Next returned false with every proxy burned; it must forgive and keep going rather than stall")
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

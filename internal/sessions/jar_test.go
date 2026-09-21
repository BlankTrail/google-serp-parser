// SPDX-License-Identifier: MIT

package sessions

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func at(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u
}

// names is what a jar would send to an address, by name.
func names(j *Jar, u *url.URL) map[string]string {
	out := map[string]string{}
	for _, c := range j.Cookies(u) {
		out[c.Name] = c.Value
	}
	return out
}

func TestJar_WritesDownEveryHostTheSessionTouched(t *testing.T) {
	// A search for Russia wins its clearance on google.ru while the consent
	// flow sets cookies on google.com. The first time a session was carried
	// between ports, what was carried was the jar's answer for google.com —
	// four cookies — and the two the challenge had been won with stayed behind
	// on google.ru. A session written down for one host is a session missing
	// the part of it that was paid for.
	j := NewJar()
	ru, com := at(t, "https://www.google.ru/search"), at(t, "https://www.google.com/")
	j.SetCookies(ru, []*http.Cookie{
		{Name: "GOOGLE_ABUSE_EXEMPTION", Value: "cleared", Path: "/"},
		{Name: "NID", Value: "ru-nid", Domain: ".google.ru", Path: "/"},
	})
	j.SetCookies(com, []*http.Cookie{{Name: "SOCS", Value: "consent", Domain: ".google.com", Path: "/"}})

	written, err := j.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	back, err := ReadJar(written)
	if err != nil {
		t.Fatalf("ReadJar: %v", err)
	}

	if got := names(back, ru); got["GOOGLE_ABUSE_EXEMPTION"] != "cleared" || got["NID"] != "ru-nid" {
		t.Errorf("read back, the jar sends google.ru %v — the clearance did not survive", got)
	}
	if got := names(back, com); got["SOCS"] != "consent" {
		t.Errorf("read back, the jar sends google.com %v", got)
	}
	// And each cookie goes back only where it belongs: the clearance was set by
	// www.google.ru with no domain, so it is that host's alone.
	if got := names(back, com); got["GOOGLE_ABUSE_EXEMPTION"] != "" {
		t.Errorf("the google.ru clearance is sent to google.com after reading back: %v", got)
	}
}

func TestJar_ForgetsWhatTheServerDeleted(t *testing.T) {
	// A server deletes a cookie by setting it again already expired. A record
	// that kept the old value would hand it back the next time the session was
	// read — a session that has been told to forget something and remembers it.
	j := NewJar()
	u := at(t, "https://www.google.ru/")
	j.SetCookies(u, []*http.Cookie{{Name: "NID", Value: "old", Path: "/"}})
	j.SetCookies(u, []*http.Cookie{{Name: "NID", Value: "", Path: "/", MaxAge: -1}})

	for _, c := range j.Held() {
		if c.Name == "NID" {
			t.Fatalf("a deleted cookie is still written down: %+v", c)
		}
	}
	written, _ := j.MarshalJSON()
	back, err := ReadJar(written)
	if err != nil {
		t.Fatalf("ReadJar: %v", err)
	}
	if got := names(back, u); got["NID"] != "" {
		t.Errorf("a deleted cookie came back on reading: %v", got)
	}
}

func TestJar_DoesNotReadBackWhatExpiredWhileItWasWrittenDown(t *testing.T) {
	// A session is written down and read back hours later — up to twelve of
	// them, which is how long an unused one is kept. A cookie that ran out in
	// between is not part of it any more.
	j := NewJar()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	j.now = func() time.Time { return now }
	u := at(t, "https://www.google.ru/")
	j.SetCookies(u, []*http.Cookie{
		{Name: "SHORT", Value: "x", Path: "/", MaxAge: 60},
		{Name: "LONG", Value: "y", Path: "/", Expires: now.Add(48 * time.Hour)},
	})
	written, _ := j.MarshalJSON()

	back := NewJar()
	back.now = func() time.Time { return now.Add(2 * time.Hour) }
	var held []Cookie
	if err := jsonUnmarshal(written, &held); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	back.restore(held)

	got := map[string]bool{}
	for _, c := range back.Held() {
		got[c.Name] = true
	}
	if got["SHORT"] {
		t.Error("a cookie that expired while the session was written down was read back")
	}
	if !got["LONG"] {
		t.Error("a cookie still in date was lost on reading back")
	}
}

func TestJar_KeepsMaxAgeAsAMomentRatherThanAsASpan(t *testing.T) {
	// Max-Age counts from the moment the cookie arrived. Written down as it
	// arrived, it would count from every moment the session was read back, and
	// a cookie meant to last an hour would last forever.
	j := NewJar()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	j.now = func() time.Time { return now }
	j.SetCookies(at(t, "https://www.google.ru/"), []*http.Cookie{{Name: "A", Value: "1", Path: "/", MaxAge: 3600}})

	held := j.Held()
	if len(held) != 1 {
		t.Fatalf("the jar holds %d cookies, want 1", len(held))
	}
	if want := now.Add(time.Hour); !held[0].Expires.Equal(want) {
		t.Errorf("the cookie is written down to expire at %v, want %v", held[0].Expires, want)
	}
}

func TestReadJar_TakesAnEmptySessionAsAnEmptyJar(t *testing.T) {
	// A session that has never been asked through has nothing written down,
	// and that is a session rather than a fault.
	j, err := ReadJar(nil)
	if err != nil {
		t.Fatalf("ReadJar(nil): %v", err)
	}
	if got := j.Held(); len(got) != 0 {
		t.Errorf("an empty session reads back holding %v", got)
	}
}

// jsonUnmarshal reads a written-down jar as its cookies, for a test that has to
// restore them under a clock of its own.
func jsonUnmarshal(written []byte, into *[]Cookie) error {
	return json.Unmarshal(written, into)
}

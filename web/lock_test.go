// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blanktrail/google-serp-parser/settings"
)

// answering is a handler that says it was reached, so a test can tell a request
// that got through from one that was turned away.
func answering() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("through"))
	})
}

func TestLocked_TurnsAwayAnotherMachineWithoutThePassword(t *testing.T) {
	// The pages carry no key of their own. On loopback that is safe — whoever
	// can reach the port is at the machine, with the settings file and the
	// history already — and on a network it is the settings, the queue and every
	// result offered to whoever asks.
	hash, err := settings.LockPassword("a good enough password")
	if err != nil {
		t.Fatalf("LockPassword: %v", err)
	}
	guarded := Locked(answering(), hash)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.44:51000"
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a request from the network was answered with %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("nothing tells the browser to ask for a password, so it never will")
	}

	// The wrong password is no password.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.44:51000"
	req.SetBasicAuth("", "not the password")
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password was answered with %d, want 401", rec.Code)
	}

	// The right one gets through.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.44:51000"
	req.SetBasicAuth("", "a good enough password")
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("the right password was answered with %d, want 200", rec.Code)
	}
}

func TestLocked_LetsThisMachineThroughWithoutAsking(t *testing.T) {
	// A password on loopback is a lock on a door the reader is already inside,
	// and one they would have to type every time they glanced at a job.
	hash, err := settings.LockPassword("a good enough password")
	if err != nil {
		t.Fatalf("LockPassword: %v", err)
	}
	guarded := Locked(answering(), hash)

	for _, from := range []string{"127.0.0.1:51000", "[::1]:51000"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = from
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("a request from %s was answered with %d, want 200", from, rec.Code)
		}
	}
}

func TestLocked_ReadsTheAddressRatherThanAHeaderTheAskerWrote(t *testing.T) {
	// X-Forwarded-For and its relatives are written by whoever is asking. A
	// guard that read them would be one anybody walks through by typing
	// 127.0.0.1 into a header.
	hash, err := settings.LockPassword("a good enough password")
	if err != nil {
		t.Fatalf("LockPassword: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.9:51000"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Real-IP", "127.0.0.1")
	rec := httptest.NewRecorder()
	Locked(answering(), hash).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a header claiming to be loopback got through with %d", rec.Code)
	}
}

func TestLocked_WithoutAPasswordGuardsNothing(t *testing.T) {
	// The switch cannot be turned on without a password, so an empty hash means
	// the interface is on loopback and there is nothing to ask about. A guard
	// that let everybody through while looking like a guard would be worse than
	// none.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:51000"
	rec := httptest.NewRecorder()
	Locked(answering(), "").ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("a loopback request with no password set was answered with %d", rec.Code)
	}
}

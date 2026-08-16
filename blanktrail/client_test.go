// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func newTestClient(t *testing.T) (*Client, *fakebt.Server) {
	t.Helper()
	fake := fakebt.New(t)
	c, err := NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, fake
}

func TestDefaultPortSpec_ArmsChallengeBreakerAndKeepsSessions(t *testing.T) {
	s := DefaultPortSpec()
	if !s.JSSolver {
		t.Error("JSSolver must default to true: the target is behind a JS challenge")
	}
	if !s.KeepSessions {
		t.Error("KeepSessions must default to true: a port is a session with its own cookie jar")
	}
	if s.Mode != "db" {
		t.Errorf("Mode=%q, want \"db\": a real profile from the curated database, not a synthetic one", s.Mode)
	}
}

// TestDefaultPortSpec_LeavesMaxConcurrentUnset pins the new default: ports open
// with no per-port concurrency limit at all, so the proxy applies its own.
func TestDefaultPortSpec_LeavesMaxConcurrentUnset(t *testing.T) {
	s := DefaultPortSpec()
	if s.MaxConcurrent != 0 {
		t.Errorf("MaxConcurrent=%d, want 0 (unset)", s.MaxConcurrent)
	}
}

// TestDefaultPortSpec_TimeoutSecondsIs30 pins the default per-request,
// port-side timeout.
func TestDefaultPortSpec_TimeoutSecondsIs30(t *testing.T) {
	s := DefaultPortSpec()
	if s.TimeoutSeconds != 30 {
		t.Errorf("TimeoutSeconds=%d, want 30", s.TimeoutSeconds)
	}
}

// TestClient_OpenPortSendsTimeoutSeconds proves the default reaches the wire:
// a struct-only assertion on DefaultPortSpec() would not catch a regression in
// PortSpec.request that stopped sending the field.
func TestClient_OpenPortSendsTimeoutSeconds(t *testing.T) {
	c, fake := newTestClient(t)

	if _, err := c.OpenPort(context.Background(), 20011, DefaultPortSpec(), Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}

	var sent map[string]any
	for _, r := range fake.Requests() {
		if r.Path == "/api/v1/ports/open" {
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("decode recorded open body: %v", err)
			}
		}
	}
	if sent == nil {
		t.Fatal("no request recorded for /api/v1/ports/open")
	}
	if got, ok := sent["timeout_seconds"].(float64); !ok || got != 30 {
		t.Errorf("open body timeout_seconds=%v, want 30", sent["timeout_seconds"])
	}
}

// TestClient_OpenPortOmitsTimeoutSecondsWhenUnset proves a zero TimeoutSeconds
// leaves the key out of the body entirely, so the proxy applies its own
// default instead of receiving an explicit zero.
func TestClient_OpenPortOmitsTimeoutSecondsWhenUnset(t *testing.T) {
	c, fake := newTestClient(t)

	spec := DefaultPortSpec()
	spec.TimeoutSeconds = 0

	if _, err := c.OpenPort(context.Background(), 20012, spec, Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}

	var sent map[string]any
	for _, r := range fake.Requests() {
		if r.Path == "/api/v1/ports/open" {
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("decode recorded open body: %v", err)
			}
		}
	}
	if sent == nil {
		t.Fatal("no request recorded for /api/v1/ports/open")
	}
	if v, present := sent["timeout_seconds"]; present {
		t.Errorf("open body carries timeout_seconds=%v, want the key absent so the proxy governs it", v)
	}
}

// TestClient_OpenPortCarriesTimeoutSecondsAndIdleSecondsIndependently proves
// the two proxy-side settings are wired separately: one governs how long an
// idle port survives, the other bounds a single request, and setting them to
// different values must not collapse them into one.
func TestClient_OpenPortCarriesTimeoutSecondsAndIdleSecondsIndependently(t *testing.T) {
	c, fake := newTestClient(t)

	spec := DefaultPortSpec()
	spec.TimeoutSeconds = 45
	spec.IdleSeconds = 120

	if _, err := c.OpenPort(context.Background(), 20013, spec, Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}

	var sent map[string]any
	for _, r := range fake.Requests() {
		if r.Path == "/api/v1/ports/open" {
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("decode recorded open body: %v", err)
			}
		}
	}
	if sent == nil {
		t.Fatal("no request recorded for /api/v1/ports/open")
	}
	if got, ok := sent["timeout_seconds"].(float64); !ok || got != 45 {
		t.Errorf("open body timeout_seconds=%v, want 45", sent["timeout_seconds"])
	}
	if got, ok := sent["idle_seconds"].(float64); !ok || got != 120 {
		t.Errorf("open body idle_seconds=%v, want 120", sent["idle_seconds"])
	}
}

func TestClient_OpenPortSendsSpecAndParsesProfile(t *testing.T) {
	c, fake := newTestClient(t)

	info, err := c.OpenPort(context.Background(), 20001, DefaultPortSpec(),
		Egress{Upstream: "socks5://user:pass@1.2.3.4:1080"})
	if err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	if info.Port != 20001 {
		t.Errorf("Port=%d, want 20001", info.Port)
	}
	if info.Profile.Name == "" {
		t.Error("Profile.Name is empty; the open response carries current_profile")
	}

	var sent map[string]any
	for _, r := range fake.Requests() {
		if r.Path == "/api/v1/ports/open" {
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("decode recorded open body: %v", err)
			}
		}
	}
	if sent == nil {
		t.Fatal("no request recorded for /api/v1/ports/open")
	}
	for _, key := range []string{"js_solver", "keep_sessions", "h2_spoofing", "spoof_headers"} {
		if v, ok := sent[key].(bool); !ok || !v {
			t.Errorf("open body %s=%v, want true", key, sent[key])
		}
	}
	if got, _ := sent["upstream"].(string); got != "socks5://user:pass@1.2.3.4:1080" {
		t.Errorf("open body upstream=%q, want the egress upstream", got)
	}
	if _, present := sent["auto_rotate"]; present {
		t.Error("open body carries auto_rotate; that key was removed from the control API")
	}
}

// TestClient_OpenPortOmitsMaxConcurrentWhenUnset proves the omission reaches the
// wire: a struct-only assertion on DefaultPortSpec() would not catch a
// regression in PortSpec.request that started sending the zero value.
func TestClient_OpenPortOmitsMaxConcurrentWhenUnset(t *testing.T) {
	c, fake := newTestClient(t)

	if _, err := c.OpenPort(context.Background(), 20010, DefaultPortSpec(), Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}

	var sent map[string]any
	for _, r := range fake.Requests() {
		if r.Path == "/api/v1/ports/open" {
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("decode recorded open body: %v", err)
			}
		}
	}
	if sent == nil {
		t.Fatal("no request recorded for /api/v1/ports/open")
	}
	if v, present := sent["max_concurrent"]; present {
		t.Errorf("open body carries max_concurrent=%v, want the key absent so the proxy governs it", v)
	}
}

func TestClient_OpenPortWithGatewayOmitsUpstream(t *testing.T) {
	c, fake := newTestClient(t)

	if _, err := c.OpenPort(context.Background(), 20002, DefaultPortSpec(),
		Egress{Gateway: "vless-nl"}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}

	var sent map[string]any
	for _, r := range fake.Requests() {
		if r.Path == "/api/v1/ports/open" {
			_ = json.Unmarshal([]byte(r.Body), &sent)
		}
	}
	if got, _ := sent["upstream_gateway"].(string); got != "vless-nl" {
		t.Errorf("upstream_gateway=%q, want \"vless-nl\"", got)
	}
	if _, present := sent["upstream"]; present {
		t.Error("upstream must be absent when the egress is a gateway")
	}
}

func TestClient_SetUpstreamAndRotate(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	if _, err := c.OpenPort(ctx, 20003, DefaultPortSpec(), Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	if err := c.SetUpstream(ctx, 20003, "http://9.9.9.9:8080"); err != nil {
		t.Fatalf("SetUpstream: %v", err)
	}
	if got := fake.UpstreamOf(20003); got != "http://9.9.9.9:8080" {
		t.Errorf("fake upstream=%q, want the new one", got)
	}

	p, err := c.RotateProfile(ctx, 20003)
	if err != nil {
		t.Fatalf("RotateProfile: %v", err)
	}
	if p.Name == "" {
		t.Error("RotateProfile returned an empty profile name")
	}
	if n := fake.RotateCount(20003); n != 1 {
		t.Errorf("RotateCount=%d, want 1", n)
	}
}

func TestClient_UnauthorizedIsTyped(t *testing.T) {
	fake := fakebt.New(t)
	c, err := NewClient(fake.URL(), "wrong-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	err = c.Health(context.Background())
	if err == nil {
		t.Fatal("Health with a wrong key returned nil error")
	}
	if !IsUnauthorized(err) {
		t.Errorf("IsUnauthorized(%v) = false, want true", err)
	}
	var apiErr *APIError
	if !errorsAs(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Message != "authentication required" {
		t.Errorf("Message=%q, want the server's error text", apiErr.Message)
	}
}

func TestClient_SuggestAndClose(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	p, err := c.SuggestPort(ctx)
	if err != nil {
		t.Fatalf("SuggestPort: %v", err)
	}
	if p < 1 || p > 65535 {
		t.Fatalf("SuggestPort returned %d, want a valid TCP port", p)
	}
	if _, err := c.OpenPort(ctx, p, DefaultPortSpec(), Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	if err := c.ClosePort(ctx, p); err != nil {
		t.Fatalf("ClosePort: %v", err)
	}
	if len(fake.OpenPorts()) != 0 {
		t.Errorf("OpenPorts=%v, want empty after close", fake.OpenPorts())
	}
}

func TestNewClient_RejectsBadInput(t *testing.T) {
	if _, err := NewClient("", "k"); err == nil {
		t.Error("NewClient with an empty base URL returned nil error")
	}
	if _, err := NewClient("http://", "k"); err == nil {
		t.Error("NewClient with a host-less URL returned nil error")
	}
}

func TestAPIError_MessageFallsBackToBody(t *testing.T) {
	fake := fakebt.New(t)
	fake.FailNext("/api/v1/health", http.StatusInternalServerError, "boom, not json")
	c, _ := NewClient(fake.URL(), fake.Key())

	err := c.Health(context.Background())
	var apiErr *APIError
	if !errorsAs(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if !strings.Contains(apiErr.Message, "boom") {
		t.Errorf("Message=%q, want the raw body when it is not JSON", apiErr.Message)
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

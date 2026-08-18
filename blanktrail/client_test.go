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

// TestDefaultPortSpec_BoundsSilenceAndLeavesTheIdleSpanAlone pins the two
// spans this client names, and the one it does not.
//
// The thirty seconds bound silence and not the request: they cover the dial and
// then the wait for a first byte, and are spent once one arrives. They need no
// margin for a challenge, because the proxy stretches its own waits while it
// solves one — a longer span buys nothing but longer silences, which is what a
// live list showed when a hundred and eighty answered half as much.
//
// The idle span stays the proxy's. This client sent thirty into it while
// believing it bounded a request, which asked the proxy to close idle tunnels
// after thirty seconds while the transport here kept them for ninety.
func TestDefaultPortSpec_BoundsSilenceAndLeavesTheIdleSpanAlone(t *testing.T) {
	s := DefaultPortSpec()
	if s.ConnectTimeoutSeconds != 5 {
		t.Errorf("ConnectTimeoutSeconds=%d, want 5", s.ConnectTimeoutSeconds)
	}
	if s.RequestTimeoutSeconds != 30 {
		t.Errorf("RequestTimeoutSeconds=%d, want 30", s.RequestTimeoutSeconds)
	}
	if s.IdleTimeoutSeconds != 0 {
		t.Errorf("IdleTimeoutSeconds=%d, want it left to the proxy", s.IdleTimeoutSeconds)
	}
}

// TestClient_OpenPortSendsTheSpansItNames proves the decision above reaches the
// wire. A struct-only assertion on DefaultPortSpec would not catch a request
// builder that had stopped sending them.
func TestClient_OpenPortSendsTheSpansItNames(t *testing.T) {
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
	if got, ok := sent["connect_timeout_seconds"].(float64); !ok || got != 5 {
		t.Errorf("open body connect_timeout_seconds=%v, want 5", sent["connect_timeout_seconds"])
	}
	if got, ok := sent["request_timeout_seconds"].(float64); !ok || got != 30 {
		t.Errorf("open body request_timeout_seconds=%v, want 30", sent["request_timeout_seconds"])
	}
	// The idle span is not one of them: this client keeps no connection idle and
	// has no reason to hold an opinion about when the proxy closes its own.
	if v, present := sent["timeout_seconds"]; present {
		t.Errorf("open body carries timeout_seconds=%v, want the key absent", v)
	}
}

// TestClient_OpenPortOmitsASpanNobodySet proves a zero leaves the key out of
// the body entirely, so the proxy applies its own default instead of receiving
// an explicit zero — which it would read as "no wait at all".
func TestClient_OpenPortOmitsASpanNobodySet(t *testing.T) {
	c, fake := newTestClient(t)

	spec := DefaultPortSpec()
	spec.ConnectTimeoutSeconds = 0
	spec.RequestTimeoutSeconds = 0

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
	for _, key := range []string{"connect_timeout_seconds", "request_timeout_seconds", "timeout_seconds"} {
		if v, present := sent[key]; present {
			t.Errorf("open body carries %s=%v, want the key absent so the proxy governs it", key, v)
		}
	}
}

// TestClient_OpenPortCarriesEverySpanIndependently proves the four proxy-side
// spans are wired separately. They govern different waits — reaching an
// address, the request after it, an idle connection, and the port's own life —
// and any two of them collapsed into one would be a setting that silently
// changes another.
func TestClient_OpenPortCarriesEverySpanIndependently(t *testing.T) {
	c, fake := newTestClient(t)

	spec := DefaultPortSpec()
	spec.ConnectTimeoutSeconds = 7
	spec.RequestTimeoutSeconds = 45
	spec.IdleTimeoutSeconds = 600
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
	for _, want := range []struct {
		key string
		n   float64
	}{
		{"connect_timeout_seconds", 7},
		{"request_timeout_seconds", 45},
		{"timeout_seconds", 600},
		{"idle_seconds", 120},
	} {
		if got, ok := sent[want.key].(float64); !ok || got != want.n {
			t.Errorf("open body %s=%v, want %v", want.key, sent[want.key], want.n)
		}
	}
}

func TestClient_OpenPortSendsUpstreamTLSInsecureOnlyWhenSet(t *testing.T) {
	// The certificate this is about belongs to the leg between the control
	// service and the upstream proxy, and only an https:// proxy has one. Sent
	// unconditionally, an unset false would read to the proxy the same as an
	// operator who typed it in on purpose — and this one is a deliberate trade
	// against the CONNECT targets and the proxy's own credentials, made for a
	// proxy the caller already trusts.
	c, fake := newTestClient(t)

	spec := DefaultPortSpec()
	spec.UpstreamTLSInsecure = true
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
	if got, ok := sent["upstream_tls_insecure"].(bool); !ok || !got {
		t.Errorf("open body upstream_tls_insecure=%v, want true", sent["upstream_tls_insecure"])
	}

	c2, fake2 := newTestClient(t)
	if _, err := c2.OpenPort(context.Background(), 20014, DefaultPortSpec(), Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	sent = nil
	for _, r := range fake2.Requests() {
		if r.Path == "/api/v1/ports/open" {
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("decode recorded open body: %v", err)
			}
		}
	}
	if sent == nil {
		t.Fatal("no request recorded for /api/v1/ports/open")
	}
	if v, present := sent["upstream_tls_insecure"]; present {
		t.Errorf("open body carries upstream_tls_insecure=%v when nobody asked for it", v)
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

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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

func TestDefaultPortSpec_ArmsChallengeBreakerAndKeepsNoSessionOnThePort(t *testing.T) {
	s := DefaultPortSpec()
	if !s.JSSolver {
		t.Error("JSSolver must default to true: the target is behind a JS challenge")
	}
	if s.KeepSessions {
		t.Error("KeepSessions must default to false: the program keeps its own sessions, and a port " +
			"keeping a jar of its own would carry two — ours in the header and the service's underneath")
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
	for _, key := range []string{"js_solver", "h2_spoofing", "spoof_headers"} {
		if v, ok := sent[key].(bool); !ok || !v {
			t.Errorf("open body %s=%v, want true", key, sent[key])
		}
	}
	// Said, and said as false: ports keep no session of their own since the
	// program keeps its own, and a flag left out would leave the port to the
	// service's default.
	if v, ok := sent["keep_sessions"].(bool); !ok || v {
		t.Errorf("open body keep_sessions=%v, want false and present", sent["keep_sessions"])
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

	// Asked of an endpoint that actually wants the key: health is open to
	// everybody, so a wrong key gets 200 from it and this test would be asking
	// nothing.
	_, err = c.LicenseStatus(context.Background())
	if err == nil {
		t.Fatal("reading the licence with a wrong key returned nil error")
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

func TestDefaultPortSpec_AllowsAnUpstreamThatTerminatesTLSItself(t *testing.T) {
	// Roughly seven addresses in ten on the lists this parser is pointed at
	// terminate TLS themselves and present their own certificate. A port that
	// refuses them answers 526 instead of a search, so a run on such a list gets
	// a third of what it paid for. Refusing an upstream nobody vouched for is
	// the right default for the proxy and the wrong one here: the list is the
	// operator's own choice, and what travels through it is a public search.
	if !DefaultPortSpec().AllowMITMUpstream {
		t.Error("the default spec refuses an upstream that terminates TLS itself")
	}

	body, err := json.Marshal(DefaultPortSpec().request(1, Egress{}))
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	if !strings.Contains(string(body), `"allow_mitm_upstream":true`) {
		t.Errorf("the open request does not allow a MITM upstream: %s", body)
	}
}

func TestPortSpec_OpensAndDialsOverSOCKS5UnlessHTTPWasNamed(t *testing.T) {
	// A port is opened as SOCKS5 and reached as SOCKS5, and the two have to be
	// the same word. An HTTP forward proxy speaks CONNECT and therefore only
	// TCP, so everything the port could otherwise do over UDP — QUIC, and
	// resolving names at the far end rather than here — has nowhere to travel
	// and quietly does not happen.
	//
	// The failure this guards against is silent in the worst way: a port opened
	// for one protocol and dialled with the other does not refuse, it hangs,
	// and what the trace shows is an address that looks dead.
	if got := DefaultPortSpec().Protocol; got != ProtocolSOCKS5 {
		t.Errorf("the default spec opens ports as %q, want socks5", got)
	}

	body, err := json.Marshal(DefaultPortSpec().request(1, Egress{}))
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	if !strings.Contains(string(body), `"protocol":"socks5"`) {
		t.Errorf("the open request does not ask for socks5: %s", body)
	}

	// And what a spec names is what both halves use, so an operator who puts it
	// back to HTTP gets a port opened for HTTP and dialled as one.
	spec := DefaultPortSpec()
	spec.Protocol = ProtocolHTTP
	body, err = json.Marshal(spec.request(1, Egress{}))
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	if !strings.Contains(string(body), `"protocol":"http"`) {
		t.Errorf("a spec that named http did not ask for it: %s", body)
	}

	for _, tc := range []struct{ named, want string }{
		{"", "socks5"},
		{ProtocolSOCKS5, "socks5"},
		{ProtocolHTTP, "http"},
		{"nonsense", "socks5"},
	} {
		tr := newBaseTransport("127.0.0.1", 20000, tc.named, nil, false, false)
		u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}})
		if err != nil {
			t.Fatalf("Proxy(%q): %v", tc.named, err)
		}
		if u.Scheme != tc.want {
			t.Errorf("a port named %q is dialled over %q, want %q", tc.named, u.Scheme, tc.want)
		}
	}
}

func TestWearSession_PutsOneOfOurOwnSessionsOnALivePort(t *testing.T) {
	// How a session is resumed. A session is a fingerprint and a set of
	// cookies; the cookies are this program's to keep, and the fingerprint is
	// the service's, named by a profile it holds. Putting that name on a port
	// that is already open is what lets a session move between ports without
	// either of them being reopened — and reopening is the thing to avoid,
	// because a port is opened on an address and an address that has answered
	// is the scarce thing.
	f := fakebt.New(t)
	cl, err := NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()
	port, err := cl.SuggestPort(ctx)
	if err != nil {
		t.Fatalf("SuggestPort: %v", err)
	}
	if _, err := cl.OpenPort(ctx, port, DefaultPortSpec(), Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}

	got, err := cl.WearSession(ctx, port, "Firefox_155_lin")
	if err != nil {
		t.Fatalf("WearSession: %v", err)
	}
	if got.Name != "Firefox_155_lin" {
		t.Errorf("the port came back wearing %q, want the profile that was asked for", got.Name)
	}
	// And the service says the same thing when asked afterwards, which is what
	// makes it the port's state rather than one answer.
	if held := f.ProfileOf(port); held.Name != "Firefox_155_lin" {
		t.Errorf("the service holds the port as wearing %q", held.Name)
	}
}

func TestResetSolverSessions_ClearsWhatTheSolverLeftOnAPort(t *testing.T) {
	// What makes a port reusable by another session. A port that carried one
	// session and is handed to the next without this would put the first one's
	// clearance under the second one's cookies, and the second session would
	// be two sessions to whoever is reading them.
	f := fakebt.New(t)
	cl, err := NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()
	port, err := cl.SuggestPort(ctx)
	if err != nil {
		t.Fatalf("SuggestPort: %v", err)
	}
	if _, err := cl.OpenPort(ctx, port, DefaultPortSpec(), Egress{}); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}

	if got := f.ResetsOf(port); got != 0 {
		t.Fatalf("a port that was just opened has been reset %d times", got)
	}
	if err := cl.ResetSolverSessions(ctx, port); err != nil {
		t.Fatalf("ResetSolverSessions: %v", err)
	}
	if got := f.ResetsOf(port); got != 1 {
		t.Errorf("the port was reset %d times, want once", got)
	}
}

func TestClient_CarriesAPortsTicketsOutAndBackIn(t *testing.T) {
	// A session is put back on a port with the TLS tickets it left another one
	// with. Out: what the port holds, without spending it. In: the tickets
	// replace whatever the port held — a port carrying one session's tickets
	// into the next session's requests would tie the two together in front of
	// the server.
	c, fake := newTestClient(t)
	ctx := context.Background()
	info, err := c.OpenPort(ctx, 20021, DefaultPortSpec(), Egress{Upstream: "socks5://1.2.3.4:1080"})
	if err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	fake.SetTickets(info.Port, `[{"host":"www.google.ru","tickets":[{"ticket":"dA==","state":"cw=="}]}]`)

	out, err := c.ExportSession(ctx, info.Port)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if !strings.Contains(string(out.Tickets), "www.google.ru") {
		t.Fatalf("exported tickets %s, want the port's", out.Tickets)
	}
	if fake.TicketsOf(info.Port) == "" {
		t.Error("exporting spent the port's tickets; it must only read them")
	}

	got, err := c.ImportSession(ctx, info.Port, "socks5://1.2.3.4:1080", json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if got.IdentityMismatch {
		t.Error("the port stands on the address named, yet the import reported a mismatch")
	}
	if left := fake.TicketsOf(info.Port); left != "" {
		t.Errorf("an empty import left %s on the port, want its tickets replaced by none", left)
	}
}

func TestClient_SaysWhenAPortStandsOnAnotherAddress(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	info, _ := c.OpenPort(ctx, 20022, DefaultPortSpec(), Egress{Upstream: "socks5://1.2.3.4:1080"})

	got, err := c.ImportSession(ctx, info.Port, "socks5://5.6.7.8:1080", nil)
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if !got.IdentityMismatch {
		t.Error("the port stands on another address and the import did not say so")
	}
}

func TestClient_NamesAPortThatKeepsNoTickets(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	info, _ := c.OpenPort(ctx, 20023, DefaultPortSpec(), Egress{Upstream: "socks5://1.2.3.4:1080"})
	fake.SetResumptionOff(info.Port)

	_, err := c.ImportSession(ctx, info.Port, "", nil)
	if !errors.Is(err, ErrResumptionOff) {
		t.Errorf("an import onto a port that keeps no tickets answered %v, want ErrResumptionOff", err)
	}
}

func TestClient_GivesAPortAFreshFingerprintFromItsTemplate(t *testing.T) {
	// A new session is not to wear what the last session on the port wore: the
	// port goes back to the filter it was opened under and the service picks a
	// fingerprint within it.
	c, fake := newTestClient(t)
	ctx := context.Background()
	spec := DefaultPortSpec()
	spec.Browser = "chrome_153"
	info, _ := c.OpenPort(ctx, 20024, spec, Egress{Upstream: "socks5://1.2.3.4:1080"})
	if _, err := c.WearSession(ctx, info.Port, "Chrome_150_win"); err != nil {
		t.Fatalf("WearSession: %v", err)
	}

	prof, err := c.FreshProfile(ctx, info.Port, spec)
	if err != nil {
		t.Fatalf("FreshProfile: %v", err)
	}
	if prof.Name == "" || prof.Name == "Chrome_150_win" {
		t.Errorf("the port came back wearing %q, want a fresh fingerprint", prof.Name)
	}
	if fake.RotateCount(info.Port) != 1 {
		t.Errorf("the service was asked for %d fresh fingerprints, want one", fake.RotateCount(info.Port))
	}
	if keep, told := fake.KeepSessionsOf(info.Port); !told || keep {
		t.Error("putting the port back on its template did not keep keep_sessions off")
	}
}

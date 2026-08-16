// SPDX-License-Identifier: MIT

package blanktrail

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a typed wrapper over the BlankTrail Proxy control API. Every call
// authenticates with the X-API-Key header. A Client is safe for concurrent use.
type Client struct {
	base *url.URL
	key  string
	hc   *http.Client
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client used for control-API calls.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.hc = hc }
}

// NewClient returns a control-API client for the BlankTrail instance at baseURL
// (for example "http://127.0.0.1:8891"), authenticating with apiKey.
func NewClient(baseURL, apiKey string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("blanktrail: empty base URL")
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("blanktrail: bad base URL %q: %w", baseURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("blanktrail: base URL %q has no host", baseURL)
	}
	c := &Client{base: u, key: apiKey, hc: &http.Client{Timeout: 15 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// ControlHost is the host of the control API, which is also the default host
// the opened proxy ports listen on (loopback unless LAN exposure is enabled).
func (c *Client) ControlHost() string { return c.base.Hostname() }

// APIError is returned when the control API responds with a non-2xx status.
// Message is the server's human sentence and Code its machine-readable kind,
// both extracted from the JSON error envelope when present.
type APIError struct {
	Status  int
	Path    string
	Code    string
	Message string
	Body    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Body
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return fmt.Sprintf("blanktrail: %s -> HTTP %d: %s", e.Path, e.Status, msg)
}

// IsUnauthorized reports whether err is a control-API rejection of the API key.
func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

// Egress names where a port sends its traffic. At most one field is set; both
// empty means the host's own IP.
type Egress struct {
	Upstream string // proxy URL: scheme://[user:pass@]host:port
	Gateway  string // name of a gateway config stored in BlankTrail
}

// IsDirect reports whether the egress is the host's own IP.
func (e Egress) IsDirect() bool { return e.Upstream == "" && e.Gateway == "" }

// String renders the egress for logs.
func (e Egress) String() string {
	switch {
	case e.Gateway != "":
		return "gateway:" + e.Gateway
	case e.Upstream != "":
		return e.Upstream
	default:
		return "direct"
	}
}

// PortSpec is the template applied to every port the Pool opens.
type PortSpec struct {
	Mode    string // profile source: db | auto | specific | custom (never "random" with filters)
	Browser string // chrome | firefox | safari | edge
	OS      string // windows | macos | linux | ios | android

	H2Spoofing     bool // spoof the HTTP/2 SETTINGS fingerprint
	SpoofHeaders   bool // spoof request header order
	SpoofUserAgent bool // let the proxy own a consistent UA and sec-ch-ua
	JSSolver       bool // Challenge Breaker: solve JS challenges upstream
	KeepSessions   bool // give the port its own per-domain cookie jar
	Decompress     bool // hand the client an identity-encoded body
	EnableHTTP3    bool // re-originate over HTTP/3 where the target offers it

	MaxConcurrent int // in-flight requests allowed on the port
	RetryDelayMs  int // proxy-side retry delay
	IdleSeconds   int // per-port idle timeout (0 = inherit the global one)
	// TimeoutSeconds bounds how long the port waits on one request; zero leaves
	// it to the proxy. Keep PoolConfig.RequestTimeout well above this value, or
	// the client gives up on the request before the port-side timeout can.
	TimeoutSeconds int
	LeakGuard      string // "", off, warn, enforce
}

// DefaultPortSpec is the configuration a session-oriented scraper wants: a real
// profile from the curated database, Challenge Breaker armed, and a private
// cookie jar.
func DefaultPortSpec() PortSpec {
	return PortSpec{
		Mode:           "db",
		Browser:        "chrome",
		OS:             "windows",
		H2Spoofing:     true,
		SpoofHeaders:   true,
		SpoofUserAgent: true,
		JSSolver:       true,
		KeepSessions:   true,
		Decompress:     true,
		// MaxConcurrent is left unset (0) so the proxy applies its own default.
		TimeoutSeconds: 30,
		LeakGuard:      "warn",
	}
}

// Profile is the fingerprint a port is currently wearing.
type Profile struct {
	Name      string `json:"name"`
	UserAgent string `json:"user_agent"`
	Browser   string `json:"browser"`
	OS        string `json:"os"`
}

// PortInfo describes an opened port.
type PortInfo struct {
	Port     int
	Protocol string
	Profile  Profile
}

// openPortRequest mirrors the control API's open body. Booleans are pointers so
// the server can tell "set false" from "unspecified".
//
// Note: auto_rotate is deliberately absent. It was removed from the control API
// — a profile is applied on open by itself.
type openPortRequest struct {
	Port            int     `json:"port"`
	Protocol        string  `json:"protocol"`
	Mode            string  `json:"mode,omitempty"`
	Browser         string  `json:"browser,omitempty"`
	OS              string  `json:"os,omitempty"`
	Upstream        *string `json:"upstream,omitempty"`
	UpstreamGateway string  `json:"upstream_gateway,omitempty"`
	H2Spoofing      *bool   `json:"h2_spoofing,omitempty"`
	SpoofHeaders    *bool   `json:"spoof_headers,omitempty"`
	SpoofUserAgent  *bool   `json:"spoof_user_agent,omitempty"`
	JSSolver        *bool   `json:"js_solver,omitempty"`
	KeepSessions    *bool   `json:"keep_sessions,omitempty"`
	Decompress      *bool   `json:"decompress,omitempty"`
	EnableHTTP3     *bool   `json:"enable_http3,omitempty"`
	MaxConcurrent   *int    `json:"max_concurrent,omitempty"`
	RetryDelayMs    *int    `json:"retry_delay_ms,omitempty"`
	IdleSeconds     *int    `json:"idle_seconds,omitempty"`
	TimeoutSeconds  *int    `json:"timeout_seconds,omitempty"`
	LeakGuard       string  `json:"leak_guard,omitempty"`
}

func (s PortSpec) request(port int, eg Egress) openPortRequest {
	h2, hdr, ua := s.H2Spoofing, s.SpoofHeaders, s.SpoofUserAgent
	js, jar, dec, h3 := s.JSSolver, s.KeepSessions, s.Decompress, s.EnableHTTP3
	req := openPortRequest{
		Port:           port,
		Protocol:       "http", // HTTP CONNECT forward proxy
		Mode:           s.Mode,
		Browser:        s.Browser,
		OS:             s.OS,
		H2Spoofing:     &h2,
		SpoofHeaders:   &hdr,
		SpoofUserAgent: &ua,
		JSSolver:       &js,
		KeepSessions:   &jar,
		Decompress:     &dec,
		EnableHTTP3:    &h3,
		LeakGuard:      s.LeakGuard,
	}
	if s.MaxConcurrent > 0 {
		n := s.MaxConcurrent
		req.MaxConcurrent = &n
	}
	if s.RetryDelayMs > 0 {
		n := s.RetryDelayMs
		req.RetryDelayMs = &n
	}
	if s.IdleSeconds > 0 {
		n := s.IdleSeconds
		req.IdleSeconds = &n
	}
	if s.TimeoutSeconds > 0 {
		n := s.TimeoutSeconds
		req.TimeoutSeconds = &n
	}
	switch {
	case eg.Gateway != "":
		req.UpstreamGateway = eg.Gateway
	case eg.Upstream != "":
		up := eg.Upstream
		req.Upstream = &up
	}
	return req
}

// OpenPort opens a forward-proxy port with the given spec and egress.
func (c *Client) OpenPort(ctx context.Context, port int, spec PortSpec, eg Egress) (PortInfo, error) {
	var out struct {
		Port     int     `json:"port"`
		Protocol string  `json:"protocol"`
		Profile  Profile `json:"current_profile"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/ports/open", spec.request(port, eg), &out); err != nil {
		return PortInfo{}, err
	}
	if out.Port == 0 {
		out.Port = port
	}
	return PortInfo{Port: out.Port, Protocol: out.Protocol, Profile: out.Profile}, nil
}

// ClosePort closes a previously opened port.
func (c *Client) ClosePort(ctx context.Context, port int) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/ports/close", map[string]int{"port": port}, nil)
}

// SetUpstream swaps the egress proxy of a live port without closing it.
func (c *Client) SetUpstream(ctx context.Context, port int, upstream string) error {
	path := "/api/v1/port/" + strconv.Itoa(port) + "/upstream"
	return c.doJSON(ctx, http.MethodPut, path, map[string]string{"upstream": upstream}, nil)
}

// RotateProfile picks a fresh fingerprint for a live port.
func (c *Client) RotateProfile(ctx context.Context, port int) (Profile, error) {
	var p Profile
	path := "/api/v1/port/" + strconv.Itoa(port) + "/rotate"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &p); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// SuggestPort asks the proxy for a free port number to open.
func (c *Client) SuggestPort(ctx context.Context) (int, error) {
	var out struct {
		Port int `json:"port"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/ports/suggest", nil, &out); err != nil {
		return 0, err
	}
	if out.Port == 0 {
		return 0, errors.New("blanktrail: /ports/suggest returned no port")
	}
	return out.Port, nil
}

// ListPorts reports the port numbers currently open on the proxy, including
// ports this program did not open. Callers must not take those over.
func (c *Client) ListPorts(ctx context.Context) ([]int, error) {
	var out struct {
		Ports []struct {
			Port int `json:"port"`
		} `json:"ports"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/ports", nil, &out); err != nil {
		return nil, err
	}
	ports := make([]int, 0, len(out.Ports))
	for _, p := range out.Ports {
		ports = append(ports, p.Port)
	}
	return ports, nil
}

// FetchCAPool downloads the proxy's MITM CA and returns a pool trusting it.
// Traffic through a MITM port must trust this CA; never fall back to
// InsecureSkipVerify in production code.
func (c *Client) FetchCAPool(ctx context.Context) (*x509.CertPool, error) {
	body, err := c.doRaw(ctx, http.MethodGet, "/api/v1/ca")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, errors.New("blanktrail: /api/v1/ca did not return a valid PEM certificate")
	}
	return pool, nil
}

// Health verifies the control API is reachable and the API key is accepted.
func (c *Client) Health(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/api/v1/health", nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("blanktrail: encode %s: %w", path, err)
		}
		body = bytes.NewReader(buf)
	}
	raw, err := c.do(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("blanktrail: decode %s: %w", path, err)
	}
	return nil
}

func (c *Client) doRaw(ctx context.Context, method, path string) ([]byte, error) {
	return c.do(ctx, method, path, nil, "")
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return nil, fmt.Errorf("blanktrail: build request %s: %w", path, err)
	}
	if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blanktrail: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("blanktrail: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(resp.StatusCode, path, raw)
	}
	return raw, nil
}

// newAPIError builds an APIError, pulling {"code":…,"error":…} out of the body
// when the server sent its JSON error envelope.
func newAPIError(status int, path string, raw []byte) *APIError {
	e := &APIError{Status: status, Path: path, Body: string(raw)}
	var env struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Error != "" {
		e.Code, e.Message = env.Code, env.Error
	} else {
		e.Message = strings.TrimSpace(string(raw))
	}
	return e
}

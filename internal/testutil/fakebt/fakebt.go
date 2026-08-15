// SPDX-License-Identifier: MIT

// Package fakebt is an in-process stand-in for the BlankTrail Proxy control
// API. Tests point a blanktrail.Client at it instead of a live proxy.
package fakebt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// License is the entitlement snapshot GET /api/v1/license/status reports.
type License struct {
	Activated         bool
	Plan              string
	Label             string
	Pool              bool
	JsSolverMaxProcs  int
	JsSolverProcs     int
	JsSolverLiveProcs int
	AllowedDomains    []string
}

// Gateway is one entry of GET /api/v1/ovpn.
type Gateway struct {
	Name    string
	Kind    string
	Remote  string
	Via     string
	Running bool
	Ports   int
}

// Recorded is one request the fake saw.
type Recorded struct {
	Method string
	Path   string
	Body   string
}

// Profile is the browser/OS pair a port was opened with. Tests assert on it to
// prove a port really wears the template it was assigned, rather than trusting
// that the right JSON was built.
type Profile struct {
	Browser string
	OS      string
}

type failure struct {
	status int
	body   string
}

// Server is a fake control API bound to a random loopback port.
type Server struct {
	ts  *httptest.Server
	key string

	mu       sync.Mutex
	license  License
	gateways []Gateway
	ca       []byte
	ports    map[int]string // port -> upstream
	profiles map[int]Profile
	rotates  map[int]int
	fails    map[string][]failure
	seen     []Recorded
	nextPort int
}

// New starts a fake control API and registers its shutdown with t.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		key: "test-api-key",
		license: License{
			Activated:         true,
			Plan:              "Pro",
			Pool:              true,
			JsSolverMaxProcs:  8,
			JsSolverProcs:     8,
			JsSolverLiveProcs: 0,
		},
		ports:    map[int]string{},
		profiles: map[int]Profile{},
		rotates:  map[int]int{},
		fails:    map[string][]failure{},
		nextPort: 20000,
	}
	s.ts = httptest.NewServer(http.HandlerFunc(s.route))
	t.Cleanup(s.ts.Close)
	return s
}

// URL is the base URL of the fake control API.
func (s *Server) URL() string { return s.ts.URL }

// Key is the API key the fake requires in X-API-Key.
func (s *Server) Key() string { return s.key }

// SetLicense replaces the entitlement snapshot.
func (s *Server) SetLicense(l License) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.license = l
}

// SetGateways replaces the gateway list.
func (s *Server) SetGateways(g []Gateway) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateways = g
}

// SetCA sets the PEM bytes GET /api/v1/ca returns.
func (s *Server) SetCA(pemBytes []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ca = pemBytes
}

// FailNext makes the next request to path fail with status and body.
//
// It intercepts in the router, before the API key check and before any handler
// runs, so it can only simulate a failure — it cannot fake a success. State a
// real request would change (a port being registered, say) is left untouched,
// so a test that expects a "successful" FailNext to have opened a port is
// checking nothing.
func (s *Server) FailNext(path string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[path] = append(s.fails[path], failure{status: status, body: body})
}

// OpenPorts lists the currently open port numbers, ascending.
func (s *Server) OpenPorts() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.ports))
	for p := range s.ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// UpstreamOf reports the upstream currently set on a port.
func (s *Server) UpstreamOf(port int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ports[port]
}

// ProfileOf reports the browser/OS pair a port is currently open with.
func (s *Server) ProfileOf(port int) Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profiles[port]
}

// RotateCount reports how many times a port's profile was rotated.
func (s *Server) RotateCount(port int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotates[port]
}

// Requests returns every request the fake has seen, in order.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.seen...)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	s.mu.Lock()
	s.seen = append(s.seen, Recorded{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	if q := s.fails[r.URL.Path]; len(q) > 0 {
		f := q[0]
		s.fails[r.URL.Path] = q[1:]
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
		return
	}
	s.mu.Unlock()

	if r.Header.Get("X-API-Key") != s.key {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	switch r.URL.Path {
	case "/api/v1/health":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	case "/api/v1/license/status":
		s.serveLicense(w)
	case "/api/v1/ovpn":
		s.serveGateways(w)
	case "/api/v1/ca":
		s.serveCA(w)
	case "/api/v1/ports/suggest":
		s.serveSuggest(w)
	case "/api/v1/ports":
		s.serveList(w)
	case "/api/v1/ports/open":
		s.serveOpen(w, body)
	case "/api/v1/ports/close":
		s.serveClose(w, body)
	case "/api/v1/upstream/test":
		writeJSON(w, http.StatusOK, map[string]any{
			"http": map[string]any{"ok": true, "detail": "direct"},
		})
	default:
		s.servePortScoped(w, r, body)
	}
}

func (s *Server) serveLicense(w http.ResponseWriter) {
	s.mu.Lock()
	l := s.license
	s.mu.Unlock()
	out := map[string]any{
		"activated":            l.Activated,
		"plan":                 l.Plan,
		"pool":                 l.Pool,
		"js_solver_max_procs":  l.JsSolverMaxProcs,
		"js_solver_procs":      l.JsSolverProcs,
		"js_solver_live_procs": l.JsSolverLiveProcs,
	}
	if l.Label != "" {
		out["label"] = l.Label
	}
	if len(l.AllowedDomains) > 0 {
		out["allowed_domains"] = l.AllowedDomains
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) serveGateways(w http.ResponseWriter) {
	s.mu.Lock()
	gws := append([]Gateway(nil), s.gateways...)
	s.mu.Unlock()

	configs := make([]map[string]any, 0, len(gws))
	for _, g := range gws {
		entry := map[string]any{
			"name":        g.Name,
			"kind":        g.Kind,
			"remote":      g.Remote,
			"uploaded_at": "2026-08-14T00:00:00Z",
		}
		if g.Via != "" {
			entry["via"] = g.Via
		}
		if g.Running {
			entry["tunnel"] = map[string]any{"running": true, "ports": g.Ports}
		}
		configs = append(configs, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"configs": configs, "available": true})
}

func (s *Server) serveCA(w http.ResponseWriter) {
	s.mu.Lock()
	ca := s.ca
	s.mu.Unlock()
	if len(ca) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no CA configured"})
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ca)
}

func (s *Server) serveSuggest(w http.ResponseWriter) {
	s.mu.Lock()
	for s.ports[s.nextPort] != "" || s.nextPort == 0 {
		s.nextPort++
	}
	p := s.nextPort
	s.nextPort++
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"port": p})
}

func (s *Server) serveList(w http.ResponseWriter) {
	ports := s.OpenPorts()
	list := make([]map[string]any, 0, len(ports))
	for _, p := range ports {
		list = append(list, map[string]any{"port": p, "protocol": "http"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ports": list, "total_open": len(list), "max_ports": 1000,
	})
}

func (s *Server) serveOpen(w http.ResponseWriter, body []byte) {
	var req struct {
		Port     int     `json:"port"`
		Protocol string  `json:"protocol"`
		Upstream *string `json:"upstream"`
		Browser  string  `json:"browser"`
		OS       string  `json:"os"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Port == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	s.mu.Lock()
	if _, busy := s.ports[req.Port]; busy {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "port already open"})
		return
	}
	up := ""
	if req.Upstream != nil {
		up = *req.Upstream
	}
	// Store a sentinel for the direct case so the port counts as open.
	if up == "" {
		up = "direct"
	}
	s.ports[req.Port] = up
	s.profiles[req.Port] = Profile{
		Browser: firstNonEmpty(req.Browser, "chrome"),
		OS:      firstNonEmpty(req.OS, "windows"),
	}
	s.mu.Unlock()

	proto := req.Protocol
	if proto == "" {
		proto = "http"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"port":     req.Port,
		"protocol": proto,
		"status":   "opened",
		"current_profile": map[string]string{
			"name":       "Chrome_145_win_0001",
			"user_agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/145.0.0.0",
			"browser":    firstNonEmpty(req.Browser, "chrome"),
			"os":         firstNonEmpty(req.OS, "windows"),
		},
	})
}

func (s *Server) serveClose(w http.ResponseWriter, body []byte) {
	var req struct {
		Port int `json:"port"`
	}
	_ = json.Unmarshal(body, &req)
	s.mu.Lock()
	delete(s.ports, req.Port)
	delete(s.profiles, req.Port)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"port": req.Port, "status": "closed"})
}

// servePortScoped handles /api/v1/port/{n}/... routes.
func (s *Server) servePortScoped(w http.ResponseWriter, r *http.Request, body []byte) {
	const prefix = "/api/v1/port/"
	if len(r.URL.Path) <= len(prefix) || r.URL.Path[:len(prefix)] != prefix {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	rest := r.URL.Path[len(prefix):]
	i := 0
	for i < len(rest) && rest[i] != '/' {
		i++
	}
	port, err := strconv.Atoi(rest[:i])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid port number"})
		return
	}
	action := ""
	if i < len(rest) {
		action = rest[i+1:]
	}

	s.mu.Lock()
	_, open := s.ports[port]
	s.mu.Unlock()
	if !open {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "port not open"})
		return
	}

	switch action {
	case "rotate":
		s.mu.Lock()
		s.rotates[port]++
		n := s.rotates[port]
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{
			"name":       "Chrome_145_win_" + strconv.Itoa(1000+n),
			"user_agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/145.0.0.0",
			"browser":    "chrome",
			"os":         "windows",
		})
	case "upstream":
		var req struct {
			Upstream string `json:"upstream"`
		}
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		s.ports[port] = firstNonEmpty(req.Upstream, "direct")
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"upstream": req.Upstream})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

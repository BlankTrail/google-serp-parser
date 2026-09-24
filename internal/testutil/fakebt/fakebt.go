// SPDX-License-Identifier: MIT

// Package fakebt is an in-process stand-in for the BlankTrail Proxy control
// API. Tests point a blanktrail.Client at it instead of a live proxy.
package fakebt

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
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
	// Pinged says the config carries a measurement at all, and PingMS is what it
	// carries: a number, nought — which is how the real service says it has no
	// measurement — or a negative, which sends a measurement with a time and no
	// number in it.
	Pinged bool
	PingMS int
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
	// Name is the fingerprint the port is wearing by the name the service
	// holds it under. It is empty on a port opened by a filter rather than by
	// a name, and it is what putting a named profile on a live port sets.
	Name string
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
	// held are the fingerprints the service says it holds, for the listing. The
	// map above is a different thing with a nearly identical name: that one is
	// what each open port is wearing, this one is the catalogue.
	held []StoredProfile

	// maxPorts is the ceiling the service reports, and elsewhere is how many
	// ports it says are held by something other than the caller. Both are nought
	// until a test says otherwise, which is a service with room to spare.
	maxPorts  int
	elsewhere int

	// gatewaysOff is the reason the service gives for having no gateway backend,
	// and empty on a service that has one.
	gatewaysOff string
	ca          []byte
	ports       map[int]string // port -> upstream
	// deadGateways are the ones whose tunnel will not start.
	deadGateways map[string]bool
	profiles     map[int]Profile
	rotates      map[int]int
	// resets counts how often the solver's pin and cookies were cleared off
	// each port.
	resets map[int]int
	// tickets are the TLS session tickets each port holds, as the real service
	// writes them out. The fake does no TLS, so a test puts them on a port the
	// way traffic would have; what the fake keeps honest is that they are wiped
	// exactly where the service wipes them — a new fingerprint, a new address.
	tickets map[int]string
	// noResumption are the ports on which the service keeps no tickets at all.
	noResumption map[int]bool
	// keep is keep_sessions as each port was last told it, where it was told.
	keep map[int]bool
	// down makes the service answer everything with 503, the way it does while
	// it restarts.
	down        bool
	rotateDrift bool
	// namesIgnored makes a port told to wear a fingerprint by name go on
	// wearing what it wore.
	namesIgnored bool
	fails        map[string][]failure
	seen         []Recorded
	nextPort     int

	// solverQueue is what the service reports the challenge solver has in hand.
	solverQueue SolverQueue

	// integrationKey is the developer's key a program has stamped in, which is
	// how the service is told whose work brought the user.
	integrationKey string
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
		resets:   map[int]int{},
		fails:    map[string][]failure{},

		tickets:      map[int]string{},
		noResumption: map[int]bool{},
		keep:         map[int]bool{},
		nextPort:     freePort(),
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

// SetGatewayBackendMissing makes the service answer that it has no gateway
// backend, with the reason it gives for it.
//
// It is here because that answer is one of the things the check reports on, and
// a service that always says the backend is there is a service the reporting of
// it cannot be tested against. Empty puts the backend back.
func (s *Server) SetGatewayBackendMissing(why string) {
	s.mu.Lock()
	s.gatewaysOff = why
	s.mu.Unlock()
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

// SetRotateDrift makes every profile rotation come back on a different OS than
// the port was opened with — the behaviour of a control API whose rotation does
// not constrain itself to the port's browser/os filters.
//
// Whether the live API does constrain itself is unmeasured, which is exactly
// why this knob exists: a caller that only ever sees an obedient rotation
// cannot tell a working guard from an absent one, and a port silently rotated
// off its template keeps its label while its traffic changes device.
func (s *Server) SetRotateDrift(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotateDrift = on
}

// IgnoreNamedFingerprints makes a port told to wear a fingerprint by name answer
// 200 and go on wearing what it wore — what the service did up to 1.4.973
// whenever neither the mode nor the filter changed along with the name.
//
// A caller that only ever meets a service that obeys cannot tell a guard that
// reads what the port says it wears from one that trusts the status, and the
// status was 200 either way.
func (s *Server) IgnoreNamedFingerprints(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.namesIgnored = on
}

// otherOS returns an OS that is deliberately not the one given.
func otherOS(cur string) string {
	if cur == "windows" {
		return "linux"
	}
	return "windows"
}

// RotateCount reports how many times a port's profile was rotated.
func (s *Server) RotateCount(port int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotates[port]
}

// RefuseGateway makes the fake answer an open on this gateway the way the real
// service answers one whose tunnel will not start: 409, with the gateway named.
func (s *Server) RefuseGateway(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadGateways == nil {
		s.deadGateways = map[string]bool{}
	}
	s.deadGateways[name] = true
}

// AllowGateway undoes RefuseGateway: the tunnel starts again, which is what
// every gateway that refused on a live service did when asked later.
func (s *Server) AllowGateway(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deadGateways, name)
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
	if s.down {
		// A service restarting: everything it is asked is answered "not ready".
		s.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "starting"})
		return
	}
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

	// The service leaves this one door open, and so does the fake: /api/v1/health
	// answers whatever key it is shown, including none. That is what makes it
	// usable as the question "are you there at all", which is a different
	// question from "will you have me", and the two have different answers to
	// give the reader.
	//
	// A fake stricter than the thing it stands in for is worse than no fake: it
	// held the branch that names a refused key green for months while nothing
	// could reach that branch in the field, because in the field the refusal
	// arrives at the NEXT call, not this one.
	if r.URL.Path != "/api/v1/health" && r.Header.Get("X-API-Key") != s.key {
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
	case "/api/v1/profiles":
		s.serveProfiles(w, r)
	case "/api/v1/solver/queue":
		s.serveSolverQueue(w)
	case "/api/v1/ports/open":
		s.serveOpen(w, body)
	case "/api/v1/ports/close":
		s.serveClose(w, body)
	case "/api/v1/settings/integration-key":
		s.serveIntegrationKey(w, r, body)
	case "/api/v1/upstream/test":
		// The road the check took is said back, because that is the whole of
		// what a caller asking about a first hop wants to know: a service that
		// answered about the direct road is not answering the question.
		road := "direct"
		var asked struct {
			ChainProxy   string `json:"chain_proxy"`
			ChainGateway string `json:"chain_gateway"`
		}
		_ = json.Unmarshal(body, &asked)
		if asked.ChainProxy != "" {
			road = "via chain " + asked.ChainProxy
		}
		if asked.ChainGateway != "" {
			road = "via gateway " + asked.ChainGateway
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"http": map[string]any{"ok": true, "detail": road},
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
		if g.Pinged {
			ping := map[string]any{"at": "2026-08-14T00:00:00Z"}
			if g.PingMS >= 0 {
				ping["ms"] = g.PingMS
			}
			entry["ping"] = ping
		}
		configs = append(configs, entry)
	}
	s.mu.Lock()
	off := s.gatewaysOff
	s.mu.Unlock()
	if off != "" {
		// A service with no gateway backend installed: it answers, and what it
		// answers is that it cannot carry a port through one.
		writeJSON(w, http.StatusOK, map[string]any{"configs": configs, "available": false, "reason": off})
		return
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
		s.nextPort = freePort()
	}
	p := s.nextPort
	s.nextPort = freePort()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"port": p})
}

// freePort is a port number this machine has just proved nothing is listening
// on.
//
// It used to be a counter from 20000, and that was a real fault rather than an
// untidy one: the product's own proxies are opened in that range, so a test
// that dialled a port it believed closed reached a live proxy instead and sat
// there until its patience ran out. The failure looked like the code under
// test being slow, and it only appeared while something else was running.
//
// Asking the operating system for a port and letting it go leaves a small
// window in which something else could take it. That is a far smaller risk
// than naming a range and hoping, and the port is released here rather than
// held because these ports stand for proxies that are *not* answering: a test
// dials them expecting to be refused.
func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		// Nothing here can recover from a machine that cannot open a socket, and
		// a fake control API that hands out a port it did not check would put the
		// old fault back quietly.
		panic("fakebt: cannot find a free port: " + err.Error())
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

func (s *Server) serveList(w http.ResponseWriter) {
	ports := s.OpenPorts()
	list := make([]map[string]any, 0, len(ports))
	for _, p := range ports {
		list = append(list, map[string]any{"port": p, "protocol": "http"})
	}
	s.mu.Lock()
	elsewhere, ceiling := s.elsewhere, s.maxPorts
	s.mu.Unlock()
	if ceiling == 0 {
		ceiling = 1000
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ports": list, "total_open": len(list) + elsewhere, "max_ports": ceiling,
	})
}

// serveProfiles answers the fingerprint listing, filtered by browser the way the
// service filters it.
func (s *Server) serveProfiles(w http.ResponseWriter, r *http.Request) {
	want := strings.ToLower(r.URL.Query().Get("browser"))
	s.mu.Lock()
	held := append([]StoredProfile(nil), s.held...)
	s.mu.Unlock()

	out := make([]map[string]any, 0, len(held))
	for _, p := range held {
		if want != "" && strings.ToLower(p.Browser) != want {
			continue
		}
		out = append(out, map[string]any{
			"id": len(out) + 1, "name": p.Name, "browser": p.Browser,
			"version": p.Version, "user_agent": "", "created_at": "2026-01-01T00:00:00Z",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profiles": out, "total": len(out), "limit": len(out), "offset": 0,
	})
}

// serveSolverQueue answers how much work the challenge solver has in hand.
func (s *Server) serveSolverQueue(w http.ResponseWriter) {
	s.mu.Lock()
	queue := s.solverQueue
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   []any{},
		"queued":  queue.Queued,
		"running": queue.Running,
		"max_len": queue.MaxLen,
	})
}

// SolverQueue is what the fake reports the solver has in hand.
type SolverQueue struct{ Queued, Running, MaxLen int }

// SetSolverQueue makes the service report a queue of its own.
//
// It cannot be produced by asking this fake for pages, because the queue is a
// property of the real solver rather than of anything this program does: it is
// what a tariff's processes are up against, including work from other programs
// on the same machine.
func (s *Server) SetSolverQueue(q SolverQueue) {
	s.mu.Lock()
	s.solverQueue = q
	s.mu.Unlock()
}

// ResetsOf is how often the solver's pin and cookies were cleared off a port.
func (s *Server) ResetsOf(port int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resets[port]
}

// Restart makes the service forget every port it held, as a restart does: the
// numbers are free again and nothing listens on them.
func (s *Server) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ports = map[int]string{}
	s.profiles = map[int]Profile{}
	s.tickets = map[int]string{}
	s.keep = map[int]bool{}
}

// SetDown makes the service answer everything "not ready", as it does while it
// restarts, or brings it back.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// SetTickets puts TLS session tickets on a port, as traffic through it would.
func (s *Server) SetTickets(port int, raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[port] = raw
}

// TicketsOf is what a port holds; empty is none.
func (s *Server) TicketsOf(port int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tickets[port]
}

// SetResumptionOff makes a port one the service keeps no tickets on, which the
// real service answers an import on with 409.
func (s *Server) SetResumptionOff(port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noResumption[port] = true
}

// KeepSessionsOf is keep_sessions as the port was last told it, and whether it
// was told at all.
func (s *Server) KeepSessionsOf(port int) (keep, told bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep, told = s.keep[port]
	return keep, told
}

// StoredProfile is one fingerprint the fake says it holds.
type StoredProfile struct{ Name, Browser, Version string }

// SetProfiles gives the fake a set of fingerprints to report.
//
// A service with none is a service nothing can be spread over, and a test that
// wanted a spread would then be measuring the empty case by accident.
func (s *Server) SetProfiles(held ...StoredProfile) {
	s.mu.Lock()
	s.held = append([]StoredProfile(nil), held...)
	s.mu.Unlock()
}

// SetPortRoom makes the service report a ceiling of its own and a number of
// ports held by something other than the caller.
//
// Both are what a real service reports and neither can be produced by opening
// ports through this fake: the ceiling belongs to a tariff, and the ports held
// elsewhere belong to another program on the same machine. A pool that must
// stop growing before it reaches either has no other way to be shown one.
func (s *Server) SetPortRoom(ceiling, heldElsewhere int) {
	s.mu.Lock()
	s.maxPorts, s.elsewhere = ceiling, heldElsewhere
	s.mu.Unlock()
}

func (s *Server) serveOpen(w http.ResponseWriter, body []byte) {
	var req struct {
		Port     int     `json:"port"`
		Protocol string  `json:"protocol"`
		Upstream *string `json:"upstream"`
		Gateway  string  `json:"upstream_gateway"`
		Browser  string  `json:"browser"`
		OS       string  `json:"os"`
		// KeepSessions is remembered where it is said, so a test can see what a
		// port was opened as.
		KeepSessions *bool `json:"keep_sessions"`
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
	// A gateway that will not start, answered the way the real service answers
	// it: the same 409 as a taken port number, with the gateway named in it.
	if req.Gateway != "" && s.deadGateways[req.Gateway] {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("upstream gateway %q: xray gw %q: exited during startup (exit status 0xffffffff); stderr: ", req.Gateway, req.Gateway),
		})
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
	if req.KeepSessions != nil {
		s.keep[req.Port] = *req.KeepSessions
	}
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
	if _, open := s.ports[req.Port]; !open {
		// As the real service answers it: a port it does not have — one it
		// never opened, or one it lost to a restart — is not found.
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("port %d not open", req.Port)})
		return
	}
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
	case "reset_solver_sessions":
		// What the solver left on the port: the fingerprint it pinned and the
		// cookies it won. The fake counts the resets, because what a caller
		// has to get right is resetting a port before another session is put
		// on it — and a count is the only way a test can see that happen.
		s.mu.Lock()
		s.resets[port]++
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"port": port, "reset": true})
	case "config":
		// Either one named fingerprint put on a live port — the fake remembers
		// it, so asking what the port wears afterwards answers what was asked
		// for rather than what it was opened with — or the port put back on the
		// filter it was opened under, which wears no name until it is rotated.
		// keep_sessions is remembered where it is said, and a new fingerprint
		// wipes the port's tickets, as the service does.
		var req struct {
			Mode            string `json:"mode"`
			SpecificProfile string `json:"specific_profile"`
			Browser         string `json:"browser"`
			OS              string `json:"os"`
			KeepSessions    *bool  `json:"keep_sessions"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		s.mu.Lock()
		prof := s.profiles[port]
		was := prof.Name
		switch {
		case req.SpecificProfile != "":
			if !s.namesIgnored {
				prof.Name = req.SpecificProfile
			}
		case req.Mode != "" && req.Mode != "specific":
			prof.Name = ""
			if req.Browser != "" {
				prof.Browser = req.Browser
			}
			if req.OS != "" {
				prof.OS = req.OS
			}
		}
		if prof.Name != was {
			delete(s.tickets, port)
		}
		s.profiles[port] = prof
		if req.KeepSessions != nil {
			s.keep[port] = *req.KeepSessions
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"port": port, "status": "reconfigured",
			"current_profile": map[string]any{
				"name": prof.Name, "browser": prof.Browser, "os": prof.OS,
			},
		})
	case "rotate":
		// A rotation hands the port a fresh fingerprint of the same kind it was
		// opened with, and the fake remembers it: hard-coding chrome/windows
		// here, or leaving profiles untouched, made ProfileOf report the
		// open-time value forever and let any rotation bug through unseen.
		s.mu.Lock()
		s.rotates[port]++
		n := s.rotates[port]
		prof := s.profiles[port]
		if s.rotateDrift {
			prof.OS = otherOS(prof.OS)
		}
		// The fingerprint handed out is the one the port wears from now on,
		// and a new fingerprint wipes the port's tickets.
		prof.Name = prof.Browser + "_145_" + prof.OS + "_" + strconv.Itoa(1000+n)
		s.profiles[port] = prof
		delete(s.tickets, port)
		s.mu.Unlock()
		// The real API reports the browser family, while the open filter can
		// include a release (chrome_153). Echoing that filter hid renewal bugs.
		browser := prof.Browser
		if i := strings.LastIndexByte(browser, '_'); i >= 0 {
			if _, err := strconv.Atoi(browser[i+1:]); err == nil {
				browser = browser[:i]
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"name":       prof.Name,
			"user_agent": "Mozilla/5.0 (" + prof.OS + ") " + prof.Browser + "/145.0.0.0",
			"browser":    browser,
			"os":         prof.OS,
		})
	case "upstream":
		var req struct {
			Upstream string `json:"upstream"`
		}
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		next := firstNonEmpty(req.Upstream, "direct")
		if s.ports[port] != next {
			// A new address wipes the port's tickets, as the service does.
			delete(s.tickets, port)
		}
		s.ports[port] = next
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"upstream": req.Upstream})
	case "session":
		s.serveSession(w, r, port, body)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// serveSession is the session a port carries, out and back in. Out does not
// spend it; in replaces the port's tickets whole, an empty set included, and
// says when the port stands on another address than the one named.
func (s *Server) serveSession(w http.ResponseWriter, r *http.Request, port int, body []byte) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		raw := s.tickets[port]
		up := s.ports[port]
		name := s.profiles[port].Name
		s.mu.Unlock()
		doc := map[string]any{"version": 1, "identity": map[string]any{"profile": name, "upstream": up}}
		if raw != "" {
			doc["tls_tickets"] = json.RawMessage(raw)
		}
		writeJSON(w, http.StatusOK, doc)
	case http.MethodPut:
		var req struct {
			Version  int `json:"version"`
			Identity struct {
				Upstream string `json:"upstream"`
			} `json:"identity"`
			Tickets json.RawMessage `json:"tls_tickets"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Version != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session document"})
			return
		}
		s.mu.Lock()
		if s.noResumption[port] {
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "session resumption is disabled on this port"})
			return
		}
		var hosts []struct {
			Tickets []json.RawMessage `json:"tickets"`
		}
		_ = json.Unmarshal(req.Tickets, &hosts)
		offered := 0
		for _, h := range hosts {
			offered += len(h.Tickets)
		}
		if offered == 0 {
			delete(s.tickets, port)
		} else {
			s.tickets[port] = string(req.Tickets)
		}
		mismatch := req.Identity.Upstream != "" && req.Identity.Upstream != s.ports[port]
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "imported", "tickets_offered": offered, "tickets_applied": offered,
			"tickets_undecoded": 0, "identity_mismatch": mismatch,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
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

// serveIntegrationKey answers the developer's key and takes a new one, the way
// the service does: a PUT of the empty string clears it.
func (s *Server) serveIntegrationKey(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Method == http.MethodPut {
		var in struct {
			IntegrationKey string `json:"integration_key"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad body"})
			return
		}
		s.mu.Lock()
		s.integrationKey = strings.TrimSpace(in.IntegrationKey)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	s.mu.Lock()
	key := s.integrationKey
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"integration_key": key, "set": key != ""})
}

// IntegrationKey is what a test reads to see what a program stamped in, and
// IntegrationKey reports what the service has been stamped with, so a test can
// check what the program wrote — or that it wrote nothing.
func (s *Server) IntegrationKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.integrationKey
}

// SetIntegrationKey is how a test says somebody else got there first.
func (s *Server) SetIntegrationKey(key string) {
	s.mu.Lock()
	s.integrationKey = key
	s.mu.Unlock()
}

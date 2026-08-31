// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"net/http"
	"strings"
)

// LicenseStatus is the entitlement snapshot GET /api/v1/license/status reports.
// It is the source of truth for what the running BlankTrail instance is allowed
// to do — check it before opening ports rather than guessing from failures.
type LicenseStatus struct {
	Activated bool    `json:"activated"`
	Plan      string  `json:"plan"`
	PeriodEnd *string `json:"period_end"`

	// Pool reports whether the Pro multi-port pool is entitled. Without it only
	// a single port is available.
	Pool bool `json:"pool"`

	// JsSolverMaxProcs is the licensed ceiling on Challenge Breaker browser
	// processes. Zero means the feature is not entitled at all.
	JsSolverMaxProcs int `json:"js_solver_max_procs"`
	// JsSolverProcs is how many processes are configured right now.
	JsSolverProcs int `json:"js_solver_procs"`
	// JsSolverLiveProcs is how many solver processes are running at this instant.
	// It is a load gauge, not a setting: processes are spawned on demand once
	// ports with the solver enabled start taking requests, and are reaped when
	// idle. Zero is the normal reading before any port is open, so never gate a
	// pre-flight check on this field — use JsSolverMaxProcs and JsSolverProcs.
	JsSolverLiveProcs int `json:"js_solver_live_procs"`

	// Label is the display suffix of a service-scoped promo tariff, e.g. "PROMO".
	Label string `json:"label"`
	// AllowedDomains is the domain allowlist such a tariff is restricted to.
	// Empty means unrestricted.
	AllowedDomains []string `json:"allowed_domains"`
}

// ChallengeBreakerEntitled reports whether the licence includes the JS solver.
func (s LicenseStatus) ChallengeBreakerEntitled() bool { return s.JsSolverMaxProcs > 0 }

// Restricted reports whether the licence is limited to a domain allowlist.
func (s LicenseStatus) Restricted() bool { return len(s.AllowedDomains) > 0 }

// DomainAllowed reports whether host may be reached under this licence. An
// unrestricted licence allows everything. A "*.example.com" pattern matches any
// single-or-multi-label subdomain but NOT the bare apex — list the apex
// separately if it is meant to be reachable.
func (s LicenseStatus) DomainAllowed(host string) bool {
	if !s.Restricted() {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, pat := range s.AllowedDomains {
		pat = strings.ToLower(strings.TrimSpace(pat))
		switch {
		case pat == "":
			continue
		case strings.HasPrefix(pat, "*."):
			if strings.HasSuffix(host, pat[1:]) && len(host) > len(pat)-1 {
				return true
			}
		case host == pat:
			return true
		}
	}
	return false
}

// MissingDomains returns the subset of hosts this licence would refuse, in the
// order given. It is empty for an unrestricted licence.
func (s LicenseStatus) MissingDomains(hosts []string) []string {
	if !s.Restricted() {
		return nil
	}
	var missing []string
	for _, h := range hosts {
		if !s.DomainAllowed(h) {
			missing = append(missing, h)
		}
	}
	return missing
}

// LicenseStatus fetches the entitlement snapshot.
func (c *Client) LicenseStatus(ctx context.Context) (LicenseStatus, error) {
	var st LicenseStatus
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/license/status", nil, &st); err != nil {
		return LicenseStatus{}, err
	}
	return st, nil
}

// Gateway is one stored VPN gateway config (OpenVPN or VLESS) and the state of
// its tunnel, if one is up.
type Gateway struct {
	Name    string
	Kind    string // "openvpn" | "vless"
	Remote  string
	Via     string // gateway this one routes its own outbound through
	Running bool
	Ports   int // how many proxy ports currently use it
	Ping    GatewayPing
}

// GatewayPing is the last round trip the service measured to a gateway.
//
// Three states, and they are three different things to know: never measured is
// not slow, and unreachable is not nought. The service says so by what it sends
// — no ping at all until somebody measures, and a measurement that carries the
// time it was taken but no number when the gateway did not answer.
type GatewayPing struct {
	Tried    bool
	Answered bool
	MS       int
}

// GatewayList is the result of listing gateway configs.
type GatewayList struct {
	Gateways  []Gateway
	Available bool   // the gateway backend is usable at all
	Reason    string // why it is not, when Available is false
}

// Gateways lists the stored gateway configs. These become the ready-made
// choices a user ticks when composing a job — no manual entry needed.
func (c *Client) Gateways(ctx context.Context) (GatewayList, error) {
	var out struct {
		Configs []struct {
			Name   string `json:"name"`
			Kind   string `json:"kind"`
			Remote string `json:"remote"`
			Via    string `json:"via"`
			Tunnel *struct {
				Running bool `json:"running"`
				Ports   int  `json:"ports"`
			} `json:"tunnel"`
			Ping *struct {
				MS *int   `json:"ms"`
				At string `json:"at"`
			} `json:"ping"`
		} `json:"configs"`
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/ovpn", nil, &out); err != nil {
		return GatewayList{}, err
	}
	list := GatewayList{Available: out.Available, Reason: out.Reason}
	for _, cfg := range out.Configs {
		g := Gateway{Name: cfg.Name, Kind: cfg.Kind, Remote: cfg.Remote, Via: cfg.Via}
		if cfg.Tunnel != nil {
			g.Running, g.Ports = cfg.Tunnel.Running, cfg.Tunnel.Ports
		}
		if cfg.Ping != nil {
			g.Ping.Tried = true
			// Nought is how the service says it has no measurement, not that a
			// tunnel answered instantly. Read as a number it draws every
			// unmeasured gateway as the fastest one on the list.
			if cfg.Ping.MS != nil && *cfg.Ping.MS > 0 {
				g.Ping.Answered, g.Ping.MS = true, *cfg.Ping.MS
			}
		}
		list.Gateways = append(list.Gateways, g)
	}
	return list, nil
}

// CheckResult is the verdict of one pre-flight egress check.
type CheckResult struct {
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped"`
	Detail  string `json:"detail"`
}

// TestEgress probes an egress chain before any port is opened. Valid checks are
// "http", "udp" and "leak"; passing none defaults to "http". This is what backs
// the "test my proxy list" button — failing here costs a second, failing an
// hour into a job costs the job.
func (c *Client) TestEgress(ctx context.Context, eg Egress, checks ...string) (map[string]CheckResult, error) {
	if len(checks) == 0 {
		checks = []string{"http"}
	}
	body := map[string]any{
		"checks":           checks,
		"protocol":         "http",
		"upstream":         eg.Upstream,
		"upstream_gateway": eg.Gateway,
	}
	out := map[string]CheckResult{}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/upstream/test", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"crypto/x509"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HostProbe is one address asked once through one port: what came back, or why
// nothing did.
type HostProbe struct {
	URL string
	// Status is what the far end answered with, and nought where nothing came
	// back at all.
	Status int
	// Reason is the service's own word on an answer it composed itself — why it
	// could not carry the request — and empty on an answer from the far end.
	Reason string
	Err    error
	Took   time.Duration
}

// Reached says the far end answered: a status came back, and it was not the
// service answering for it.
//
// Any status counts. What a probe asks is whether the road gets to the host,
// not what the host makes of a bare request — a redirect or a not-found is a
// host that answered, where a name the proxy would not look up is a host the
// road never reached.
func (h HostProbe) Reached() bool { return h.Err == nil && h.Reason == "" && h.Status > 0 }

// probePatience is how long one address is given. The service tries an address
// three times before it gives up on it, five seconds each, and what is left over
// is the answer itself.
const probePatience = 20 * time.Second

// probeOpening keeps two probes from being handed one port number: the service
// suggests a number nothing holds, and two asked at once are given the same one.
var probeOpening sync.Mutex

// ProbeHosts opens one port on the egress, made as the spec says, asks each
// address through it once, and closes the port again.
//
// It is how a way of resolving names is tried on the road a job takes. The port
// is a job's port in everything but its length of life — the same first hop,
// the same exits, the same answer about exits that terminate TLS — so what it
// reaches is what a job's ports would reach, which is the question.
func (c *Client) ProbeHosts(ctx context.Context, spec PortSpec, eg Egress, ca *x509.CertPool,
	urls []string) ([]HostProbe, error) {
	probeOpening.Lock()
	port, err := c.SuggestPort(ctx)
	if err == nil {
		_, err = c.OpenPort(ctx, port, spec, eg)
	}
	probeOpening.Unlock()
	if err != nil {
		return nil, err
	}
	defer func() {
		// Closed whatever became of the probe: a port left open is a place in
		// the tariff nobody will ever give back.
		closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = c.ClosePort(closing, port)
	}()
	rt := newBaseTransport(c.ControlHost(), port, spec.Protocol, ca, ca == nil, true)
	defer rt.CloseIdleConnections()
	return probeWith(ctx, rt, urls), nil
}

// probeWith asks each address once over rt, one after another, and follows no
// redirect: the first answer is the one that says whether the host was reached.
func probeWith(ctx context.Context, rt http.RoundTripper, urls []string) []HostProbe {
	client := &http.Client{Transport: rt, Timeout: probePatience,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	out := make([]HostProbe, 0, len(urls))
	for _, target := range urls {
		at := time.Now()
		p := HostProbe{URL: target}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err == nil {
			var resp *http.Response
			if resp, err = client.Do(req); err == nil {
				p.Status = resp.StatusCode
				p.Reason = strings.TrimSpace(resp.Header.Get(serviceErrorHeader))
				drainAndClose(resp)
			}
		}
		p.Err, p.Took = err, time.Since(at)
		out = append(out, p)
	}
	return out
}

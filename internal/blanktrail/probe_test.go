// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestProbeWith_TellsAHostThatAnsweredFromOneTheServiceAnsweredFor(t *testing.T) {
	// A redirect or a not-found is a host that answered; a refusal the service
	// wrote itself — a name the proxy would not look up — is a host the road
	// never reached, whatever status it came with.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Host {
		case "answers.test":
			w.WriteHeader(http.StatusNoContent)
		case "moved.test":
			w.Header().Set("Location", "https://elsewhere.test/")
			w.WriteHeader(http.StatusFound)
		case "refused.test":
			w.Header().Set(serviceErrorHeader, "upstream_unreachable")
			w.WriteHeader(523)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(proxy.Close)
	at, _ := url.Parse(proxy.URL)
	rt := &http.Transport{Proxy: http.ProxyURL(at)}

	got := probeWith(context.Background(), rt, []string{
		"http://answers.test/", "http://moved.test/", "http://refused.test/", "http://missing.test/"})
	want := []bool{true, true, false, true}
	for i, p := range got {
		if p.Reached() != want[i] {
			t.Errorf("%s: reached=%v (status %d, reason %q, err %v), want %v",
				p.URL, p.Reached(), p.Status, p.Reason, p.Err, want[i])
		}
	}
	if got[2].Reason != "upstream_unreachable" {
		t.Errorf("the service's own word was read as %q", got[2].Reason)
	}
	if got[1].Status != http.StatusFound {
		t.Errorf("the redirect was followed: status %d", got[1].Status)
	}
}

func TestClientProbeHosts_OpensThePortAsTheSpecSaysAndClosesIt(t *testing.T) {
	// The port a probe stands on is a job's port in everything but its life:
	// the way it resolves names is the thing being tried, and it has to reach
	// the service as asked. And it is closed again whatever the probe met — a
	// port left open is a place in the tariff nobody gives back.
	fake := fakebt.New(t)
	c, err := NewClient(fake.URL(), fake.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	spec := DefaultPortSpec()
	spec.Resolver, spec.VDNSStrictBypass = ResolverISP, true
	got, err := c.ProbeHosts(context.Background(), spec, Egress{Upstream: "socks5://192.0.2.7:1080"}, nil,
		[]string{"https://www.google.com/generate_204"})
	if err != nil {
		t.Fatalf("ProbeHosts: %v", err)
	}
	if len(got) != 1 || got[0].Reached() {
		t.Errorf("a port nothing listens on reached the host: %+v", got)
	}
	var opened map[string]any
	for _, r := range fake.Requests() {
		if strings.HasSuffix(r.Path, "/ports/open") {
			_ = json.Unmarshal([]byte(r.Body), &opened)
		}
	}
	if opened["resolver_strategy"] != ResolverISP || opened["vdns_strict_bypass"] != true {
		t.Errorf("the port was opened with %v / %v, want the provider's resolvers with names kept away",
			opened["resolver_strategy"], opened["vdns_strict_bypass"])
	}
	if open := fake.OpenPorts(); len(open) != 0 {
		t.Errorf("ports %v were left open after the probe", open)
	}
}

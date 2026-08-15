// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"reflect"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestLicenseStatus_ReadsEntitlements(t *testing.T) {
	c, fake := newTestClient(t)
	fake.SetLicense(fakebt.License{
		Activated: true, Plan: "Promo", Label: "PROMO",
		Pool: true, JsSolverMaxProcs: 4, JsSolverProcs: 3, JsSolverLiveProcs: 1,
		AllowedDomains: []string{"*.shop.example", "market.example"},
	})

	st, err := c.LicenseStatus(context.Background())
	if err != nil {
		t.Fatalf("LicenseStatus: %v", err)
	}
	if !st.Activated || st.Plan != "Promo" || st.Label != "PROMO" {
		t.Errorf("got %+v, want an activated Promo licence labelled PROMO", st)
	}
	if st.JsSolverMaxProcs != 4 || st.JsSolverProcs != 3 || st.JsSolverLiveProcs != 1 {
		t.Errorf("solver counters = %d/%d/%d, want 4/3/1",
			st.JsSolverMaxProcs, st.JsSolverProcs, st.JsSolverLiveProcs)
	}
	if !st.ChallengeBreakerEntitled() {
		t.Error("ChallengeBreakerEntitled=false with a cap of 4")
	}
	if !st.Restricted() {
		t.Error("Restricted=false with a non-empty allowed_domains list")
	}
}

func TestLicenseStatus_ChallengeBreakerNotEntitled(t *testing.T) {
	c, fake := newTestClient(t)
	fake.SetLicense(fakebt.License{Activated: true, Plan: "Lite", JsSolverMaxProcs: 0})

	st, err := c.LicenseStatus(context.Background())
	if err != nil {
		t.Fatalf("LicenseStatus: %v", err)
	}
	if st.ChallengeBreakerEntitled() {
		t.Error("ChallengeBreakerEntitled=true with a cap of 0")
	}
	if st.Restricted() {
		t.Error("Restricted=true with an empty allowed_domains list")
	}
}

func TestLicenseStatus_DomainAllowed(t *testing.T) {
	unrestricted := LicenseStatus{}
	if !unrestricted.DomainAllowed("anything.example") {
		t.Error("an empty allowlist must allow every domain")
	}

	st := LicenseStatus{AllowedDomains: []string{"*.shop.example", "market.example", "*.cdn.example"}}
	cases := map[string]bool{
		"search.shop.example": true,
		"card.shop.example":   true,
		"market.example":      true,
		"node-12.cdn.example": true,
		"shop.example":        false, // "*.shop.example" matches a label, not the bare apex
		"api.telegram.org":    false,
		"evil-shop.example":   false,
		"SEARCH.SHOP.EXAMPLE": true, // host comparison is case-insensitive
	}
	for host, want := range cases {
		if got := st.DomainAllowed(host); got != want {
			t.Errorf("DomainAllowed(%q)=%v, want %v", host, got, want)
		}
	}
}

func TestLicenseStatus_MissingDomains(t *testing.T) {
	st := LicenseStatus{AllowedDomains: []string{"*.shop.example"}}
	got := st.MissingDomains([]string{"search.shop.example", "node-1.cdn.example", "api.telegram.org"})
	want := []string{"node-1.cdn.example", "api.telegram.org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MissingDomains=%v, want %v", got, want)
	}

	if n := (LicenseStatus{}).MissingDomains([]string{"a.example"}); len(n) != 0 {
		t.Errorf("MissingDomains on an unrestricted licence = %v, want empty", n)
	}
}

func TestGateways_ListsConfigsWithTunnelState(t *testing.T) {
	c, fake := newTestClient(t)
	fake.SetGateways([]fakebt.Gateway{
		{Name: "nl-vless", Kind: "vless", Remote: "1.2.3.4:443", Running: true, Ports: 2},
		{Name: "de-ovpn", Kind: "openvpn", Remote: "5.6.7.8:1194", Via: "nl-vless"},
	})

	list, err := c.Gateways(context.Background())
	if err != nil {
		t.Fatalf("Gateways: %v", err)
	}
	if !list.Available {
		t.Error("Available=false, want true")
	}
	if len(list.Gateways) != 2 {
		t.Fatalf("got %d gateways, want 2", len(list.Gateways))
	}
	if g := list.Gateways[0]; g.Name != "nl-vless" || g.Kind != "vless" || !g.Running || g.Ports != 2 {
		t.Errorf("first gateway = %+v, want the running vless one with 2 ports", g)
	}
	if g := list.Gateways[1]; g.Via != "nl-vless" {
		t.Errorf("second gateway Via=%q, want \"nl-vless\"", g.Via)
	}
}

func TestTestEgress_ReturnsPerCheckVerdicts(t *testing.T) {
	c, _ := newTestClient(t)

	res, err := c.TestEgress(context.Background(), Egress{}, "http")
	if err != nil {
		t.Fatalf("TestEgress: %v", err)
	}
	got, ok := res["http"]
	if !ok {
		t.Fatalf("no \"http\" verdict in %v", res)
	}
	if !got.OK {
		t.Errorf("http check OK=false, detail=%q", got.Detail)
	}
}

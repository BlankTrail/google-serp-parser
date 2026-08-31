// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestPreflight_HappyPath(t *testing.T) {
	c, fake := newTestClient(t)
	fake.SetCA(genTestCA(t))
	fake.SetGateways([]fakebt.Gateway{{Name: "nl", Kind: "vless"}})

	rep := Preflight(context.Background(), c, PreflightInput{
		Domains: []string{"search.example", "card.example"},
		Ports:   4,
	})

	if !rep.OK() {
		t.Fatalf("Preflight not OK; blocking findings: %+v", rep.Blocking())
	}
	if rep.CA == nil {
		t.Error("CA pool is nil after a successful preflight")
	}
	if len(rep.Gateways.Gateways) != 1 {
		t.Errorf("got %d gateways, want 1", len(rep.Gateways.Gateways))
	}
}

func TestPreflight_UnreachableStopsEarly(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:1", "k")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	rep := Preflight(context.Background(), c, PreflightInput{Ports: 1})

	f, ok := rep.Find("unreachable")
	if !ok {
		t.Fatalf("no \"unreachable\" finding; got %+v", rep.Findings)
	}
	if f.Severity != SeverityFail {
		t.Errorf("severity=%v, want Fail", f.Severity)
	}
	if f.Action == "" {
		t.Error("the unreachable finding carries no Action for the user")
	}
	if len(rep.Findings) != 1 {
		t.Errorf("got %d findings, want exactly 1: an unreachable proxy must stop the run", len(rep.Findings))
	}
}

func TestPreflight_WrongKey(t *testing.T) {
	// The service answers its health endpoint without a key and refuses
	// everything else, so a rejected key is not what the first call finds — it
	// is what the second one does. A reader given "could not read the licence
	// status" goes and looks at a licence that is fine; two support rounds were
	// spent that way. The key is what is wrong and the key is what this has to
	// say.
	fake := fakebt.New(t)
	c, _ := NewClient(fake.URL(), "wrong")

	rep := Preflight(context.Background(), c, PreflightInput{Ports: 1})

	if _, ok := rep.Find("license_inactive"); ok {
		t.Errorf("a rejected key is reported as a licence fault: %+v", rep.Findings)
	}
	f, ok := rep.Find("unauthorized")
	if !ok {
		t.Fatalf("no \"unauthorized\" finding; got %+v", rep.Findings)
	}
	if f.Severity != SeverityFail {
		t.Errorf("severity=%v, want Fail", f.Severity)
	}
	if !strings.Contains(strings.ToLower(f.Action), "key") {
		t.Errorf("Action=%q, want it to send the reader to the API key", f.Action)
	}
	if rep.OK() {
		t.Error("Report.OK()=true with a rejected API key")
	}
}

func TestPreflight_KeyRefusedLaterIsStillAboutTheKey(t *testing.T) {
	// A key can stop being taken part-way through the check — revoked while it
	// runs, or accepted by one endpoint and not another. Whichever call meets
	// the refusal, the answer to give is the same one, so the reading of a 401
	// belongs to the check and not to whichever call happened to be first.
	for _, at := range []string{"/api/v1/license/status", "/api/v1/ovpn", "/api/v1/ca"} {
		t.Run(at, func(t *testing.T) {
			c, fake := newTestClient(t)
			fake.SetCA(genTestCA(t))
			fake.FailNext(at, http.StatusUnauthorized, `{"error":"authentication required"}`)

			rep := Preflight(context.Background(), c, PreflightInput{Ports: 1})

			if _, ok := rep.Find("unauthorized"); !ok {
				t.Fatalf("a 401 from %s is not read as a refused key; got %+v", at, rep.Findings)
			}
			if rep.OK() {
				t.Error("Report.OK()=true after the key was refused")
			}
		})
	}
}

func TestPreflight_ChallengeBreakerMissingIsFatal(t *testing.T) {
	c, fake := newTestClient(t)
	fake.SetCA(genTestCA(t))
	fake.SetLicense(fakebt.License{Activated: true, Plan: "Lite", Pool: true, JsSolverMaxProcs: 0})

	rep := Preflight(context.Background(), c, PreflightInput{Ports: 2})

	f, ok := rep.Find("challenge_breaker")
	if !ok {
		t.Fatalf("no \"challenge_breaker\" finding; got %+v", rep.Findings)
	}
	if f.Severity != SeverityFail {
		t.Errorf("severity=%v, want Fail: the target cannot be reached without it", f.Severity)
	}
	if !strings.Contains(strings.ToLower(f.Action), "tariff") &&
		!strings.Contains(strings.ToLower(f.Action), "plan") {
		t.Errorf("Action=%q, want it to point at the tariff", f.Action)
	}
}

func TestPreflight_SolverCapacityWarnsWhenPortsExceedProcs(t *testing.T) {
	c, fake := newTestClient(t)
	fake.SetCA(genTestCA(t))
	fake.SetLicense(fakebt.License{
		Activated: true, Plan: "Pro", Pool: true,
		JsSolverMaxProcs: 4, JsSolverProcs: 4,
	})

	rep := Preflight(context.Background(), c, PreflightInput{Ports: 50})

	f, ok := rep.Find("solver_capacity")
	if !ok {
		t.Fatalf("no \"solver_capacity\" finding; got %+v", rep.Findings)
	}
	if f.Severity != SeverityWarn {
		t.Errorf("severity=%v, want Warn: this slows the run, it does not break it", f.Severity)
	}
	if !rep.OK() {
		t.Error("a warning must not make the report fail")
	}
	if !strings.Contains(f.Detail, "50") || !strings.Contains(f.Detail, "4") {
		t.Errorf("Detail=%q, want both numbers named so the user can act", f.Detail)
	}
}

func TestPreflight_SolverEntitledButSwitchedOffIsFatal(t *testing.T) {
	c, fake := newTestClient(t)
	fake.SetCA(genTestCA(t))
	fake.SetLicense(fakebt.License{
		Activated: true, Plan: "Pro", Pool: true,
		JsSolverMaxProcs: 4, JsSolverProcs: 0,
	})

	rep := Preflight(context.Background(), c, PreflightInput{Ports: 2})

	f, ok := rep.Find("solver_capacity")
	if !ok {
		t.Fatalf("no \"solver_capacity\" finding; got %+v", rep.Findings)
	}
	if f.Severity != SeverityFail {
		t.Errorf("severity=%v, want Fail: entitled but zero processes means no challenge is ever solved", f.Severity)
	}
	if rep.OK() {
		t.Error("Report.OK()=true with the solver switched off — a run in this state cannot work")
	}
	if f.Action == "" {
		t.Error("the finding carries no Action for the user")
	}
	if !strings.Contains(f.Detail, "4") {
		t.Errorf("Detail=%q, want the licence ceiling named so the user knows what to raise it to", f.Detail)
	}
}

func TestPreflight_MissingDomainsAreNamedIndividually(t *testing.T) {
	c, fake := newTestClient(t)
	fake.SetCA(genTestCA(t))
	fake.SetLicense(fakebt.License{
		Activated: true, Plan: "Promo", Label: "PROMO", Pool: true,
		JsSolverMaxProcs: 4, JsSolverProcs: 4,
		AllowedDomains: []string{"*.allowed.example"},
	})

	rep := Preflight(context.Background(), c, PreflightInput{
		Domains:         []string{"a.allowed.example", "b.blocked.example", "c.blocked.example"},
		OptionalDomains: []string{"api.telegram.org"},
		Ports:           2,
	})

	f, ok := rep.Find("domains")
	if !ok {
		t.Fatalf("no \"domains\" finding; got %+v", rep.Findings)
	}
	if f.Severity != SeverityFail {
		t.Errorf("severity=%v, want Fail", f.Severity)
	}
	for _, host := range []string{"b.blocked.example", "c.blocked.example"} {
		if !strings.Contains(f.Detail, host) {
			t.Errorf("Detail=%q does not name %q; the user must learn which domains are missing", f.Detail, host)
		}
	}
	if strings.Contains(f.Detail, "a.allowed.example") {
		t.Errorf("Detail=%q names an allowed domain", f.Detail)
	}

	opt, ok := rep.Find("optional_domains")
	if !ok {
		t.Fatal("no \"optional_domains\" finding for the blocked optional host")
	}
	if opt.Severity != SeverityWarn {
		t.Errorf("optional_domains severity=%v, want Warn: an optional domain must not block the run", opt.Severity)
	}
}

// genTestCA generates a throwaway self-signed CA in PEM form. Tests use a
// freshly generated certificate rather than a baked-in constant because
// x509.CertPool rejects malformed PEM outright.
func genTestCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

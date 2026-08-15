// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestRunDoctor_ReportsAHealthyProxy(t *testing.T) {
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, doctorOptions{
		ControlURL: fake.URL(), APIKey: fake.Key(), Threads: 2, PortsPerThread: 3,
	})
	if err != nil {
		t.Fatalf("runDoctor on a healthy proxy: %v", err)
	}
	if !strings.Contains(out.String(), "google.com") {
		t.Errorf("output does not mention the domain it checked:\n%s", out.String())
	}
}

func TestRunDoctor_FailsAndExplainsWhenChallengeBreakerIsMissing(t *testing.T) {
	// This is the single most common way a run is dead on arrival, so the
	// message must name the cause and the fix, not just refuse.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	fake.SetLicense(fakebt.License{Activated: true, Plan: "Lite", Pool: true})

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, doctorOptions{
		ControlURL: fake.URL(), APIKey: fake.Key(), Threads: 1, PortsPerThread: 1,
	})
	if err == nil {
		t.Fatal("runDoctor succeeded without Challenge Breaker, want a failure")
	}
	text := out.String()
	if !strings.Contains(text, "Challenge Breaker") {
		t.Errorf("output does not name Challenge Breaker:\n%s", text)
	}
	if !strings.Contains(text, "→") {
		t.Errorf("output carries no action line:\n%s", text)
	}
}

func TestRunDoctor_ReportsABadAPIKeyAsSuch(t *testing.T) {
	fake := fakebt.New(t)

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, doctorOptions{
		ControlURL: fake.URL(), APIKey: "wrong-key", Threads: 1, PortsPerThread: 1,
	})
	if err == nil {
		t.Fatal("runDoctor accepted a wrong API key")
	}
	if !strings.Contains(out.String(), "API key") {
		t.Errorf("output does not mention the API key:\n%s", out.String())
	}
}

func TestRunDoctor_RejectsAnEmptyControlURL(t *testing.T) {
	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, doctorOptions{APIKey: "k", Threads: 1, PortsPerThread: 1})
	if err == nil {
		t.Fatal("runDoctor accepted an empty control URL")
	}
}

// testCAPEM is a throwaway self-signed certificate. Its only job is to be valid
// PEM so FetchCAPool accepts it; nothing here verifies a chain against it.
var testCAPEM = []byte(`-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----
`)

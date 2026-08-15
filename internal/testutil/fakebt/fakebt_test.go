// SPDX-License-Identifier: MIT

package fakebt

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFake_HealthRequiresKey(t *testing.T) {
	s := New(t)

	resp, err := http.Get(s.URL() + "/api/v1/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("health without key: status=%d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/api/v1/health", nil)
	req.Header.Set("X-API-Key", s.Key())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get health with key: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health with key: status=%d, want 200", resp.StatusCode)
	}
}

func TestFake_OpenTracksPortAndUpstream(t *testing.T) {
	s := New(t)

	body := `{"port":20001,"protocol":"http","upstream":"socks5://1.2.3.4:1080"}`
	req, _ := http.NewRequest(http.MethodPost, s.URL()+"/api/v1/ports/open", strings.NewReader(body))
	req.Header.Set("X-API-Key", s.Key())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Port           int `json:"port"`
		CurrentProfile struct {
			Name string `json:"name"`
		} `json:"current_profile"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode open response: %v", err)
	}
	if out.Port != 20001 {
		t.Errorf("port=%d, want 20001", out.Port)
	}
	if out.CurrentProfile.Name == "" {
		t.Error("open response carries no current_profile.name")
	}
	if got := s.UpstreamOf(20001); got != "socks5://1.2.3.4:1080" {
		t.Errorf("UpstreamOf=%q, want the upstream from the open body", got)
	}
	if ports := s.OpenPorts(); len(ports) != 1 || ports[0] != 20001 {
		t.Errorf("OpenPorts=%v, want [20001]", ports)
	}
}

func TestFake_FailNextAppliesOnce(t *testing.T) {
	s := New(t)
	s.FailNext("/api/v1/ports/suggest", http.StatusServiceUnavailable, `{"error":"no_free_port"}`)

	get := func() int {
		req, _ := http.NewRequest(http.MethodGet, s.URL()+"/api/v1/ports/suggest", nil)
		req.Header.Set("X-API-Key", s.Key())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("suggest: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := get(); got != http.StatusServiceUnavailable {
		t.Errorf("first suggest: status=%d, want 503", got)
	}
	if got := get(); got != http.StatusOK {
		t.Errorf("second suggest: status=%d, want 200 (FailNext is one-shot)", got)
	}
}

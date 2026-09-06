//go:build manualpreview

// SPDX-License-Identifier: MIT

package web

// The screens, written to files so pictures can be taken of them.
//
// It is behind a build tag and named for what it is: not a test, a way of
// getting the real markup of every screen out of the program without a service,
// a licence and a live list behind it. Nothing here asserts anything.
//
//	go test -tags manualpreview -run TestWritePreviews ./internal/web/ -args -dir=<where>
//
// What comes out is the pages as they really are — the same templates, the same
// phrases, the same classes — with a plausible job and two profiles behind
// them. The figures are put in afterwards, by whatever is taking the pictures:
// a warm pool of three hundred identities is not something a machine with no
// BlankTrail can be made to have, and a screenshot of zeroes teaches nobody
// anything.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

var previewDir = flag.String("dir", "", "where to write the pages")

func TestWritePreviews(t *testing.T) {
	if *previewDir == "" {
		t.Skip("no -dir given")
	}
	if err := os.MkdirAll(*previewDir, 0o755); err != nil {
		t.Fatalf("making the directory: %v", err)
	}

	// A service that answers, so the gateway screen has gateways on it. The
	// names are invented and belong to nobody.
	fake := fakebt.New(t)
	fake.SetGateways([]fakebt.Gateway{
		{Name: "WiseKeys.AE-OAE", Kind: "vless"},
		{Name: "WiseKeys.DE-Germaniya", Kind: "vless"},
		{Name: "WiseKeys.FR-Franciya", Kind: "vless"},
		{Name: "WiseKeys.IT-Italiya", Kind: "vless"},
		{Name: "WiseKeys.NL-Niderlandy-Torrent", Kind: "vless"},
		{Name: "WiseKeys.SE-Shveciya", Kind: "vless"},
		{Name: "WiseKeys.SG-Singapur", Kind: "vless"},
		{Name: "astro-via-185", Kind: "openvpn"},
		{Name: "vless-udptest", Kind: "vless"},
	})

	path := settingsFile(t, settings.Settings{
		ControlURL: fake.URL(), APIKey: fake.Key(),
		HotPorts: 300, HotDevice: blanktrail.DeviceDesktop,
	})

	st := testStore(t)
	list, err := st.CreateProfile(t.Context(), store.Profile{
		Name: "Datacentre list", Kind: "url", Location: "https://example.com/proxies.txt",
		Refresh: 30 * time.Minute, Ban: time.Hour, ThreadsPerUpstream: 3,
		RenewEvery: time.Hour, Protocol: "socks5",
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	gateways, err := st.CreateProfile(t.Context(), store.Profile{
		Name: "BlankTrail gateways", Kind: settings.ProxyGateways,
		Ban: 10 * time.Minute, ThreadsPerUpstream: 10, RenewEvery: 10 * time.Minute,
		Protocol: "socks5",
		Gateways: []string{"WiseKeys.DE-Germaniya", "WiseKeys.FR-Franciya",
			"WiseKeys.NL-Niderlandy-Torrent", "astro-via-185"},
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	// A supervisor whose engine never finishes, so the job below is still in
	// flight while its screens are drawn.
	v := newSupervisor(st, &heldEngine{hold: make(chan struct{})})
	t.Cleanup(func() { _ = v.Close() })

	s, err := New(Config{Store: st, Supervisor: v, Logger: quiet(), SettingsPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id, err := v.Enqueue(store.JobSpec{
		Name: "ai photography", Pages: 10, Country: "us", Language: "en",
		Threads: 100, Ports: 3, Tries: 50, Cooldown: 5 * time.Second,
		UniqueBy: store.UniqueURL, ProfileID: list,
	}, []string{"midjourney", "stable diffusion", "ai photography", "text to image"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitUntilRunning(t, v, id)

	for _, lang := range []string{"en", "ru"} {
		for _, page := range []struct{ name, at string }{
			{"status", "/?lang=" + lang},
			{"proxies", proxiesAt + "?profile=" + fmt.Sprint(list) + "&lang=" + lang},
			{"proxies-gateways", proxiesAt + "?profile=" + fmt.Sprint(gateways) + "&lang=" + lang},
			{"new-job", newAt + "?lang=" + lang},
			{"job", jobPath(id) + "?lang=" + lang},
		} {
			rec := get(t, s, page.at)
			if rec.Code != 200 {
				t.Fatalf("%s answered %d", page.at, rec.Code)
			}
			out := filepath.Join(*previewDir, page.name+"-"+lang+".html")
			if err := os.WriteFile(out, rec.Body.Bytes(), 0o644); err != nil {
				t.Fatalf("writing %s: %v", out, err)
			}
			t.Logf("wrote %s", out)
		}
	}
}

// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/api"
	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/internal/web"
)

// skipWithoutExits skips a live measurement whose saved settings name no exits,
// and says so. Without it the measurement fails on web.ErrNoExits, which reads
// as the stand refusing when the cause is the settings file.
func skipWithoutExits(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, web.ErrNoExits) {
		t.Skipf("the saved settings name no proxy list or gateways, so there is nothing to open ports on: %v", err)
	}
}

// listAt writes a list of the given lines and answers where it is.
func listAt(t *testing.T, lines string) string {
	t.Helper()
	at := filepath.Join(t.TempDir(), "exits.txt")
	if err := os.WriteFile(at, []byte(lines), 0o600); err != nil {
		t.Fatalf("writing the list: %v", err)
	}
	return at
}

func TestRaise_OpensNothingAndAsksTheServiceNothingForAProfileWithNoExits(t *testing.T) {
	// Ports opened with nowhere to go out through can never carry a session: on
	// 2026-10-19 twenty threads on such ports sent the service thirty thousand
	// calls in two minutes. Each way a profile can name nothing is answered with
	// web.ErrNoExits before anything is asked of the service — reading the
	// profile's own list is all it costs — and the answer names the profile and
	// not where its list lives.
	cases := []struct {
		name string
		prof func(t *testing.T) store.Profile
	}{
		{"no source at all", func(*testing.T) store.Profile {
			return store.Profile{Name: "Default"}
		}},
		{"a list with no location", func(*testing.T) store.Profile {
			return store.Profile{Name: "Default", Kind: "file"}
		}},
		{"a list with nothing in it", func(t *testing.T) store.Profile {
			return store.Profile{Name: "Default", Kind: "file", Location: listAt(t, "")}
		}},
		{"a list whose every line is refused", func(t *testing.T) store.Profile {
			return store.Profile{Name: "Default", Kind: "file",
				Location: listAt(t, "# the exits go here\nnot an address\n:::\n")}
		}},
		{"gateways with none ticked", func(*testing.T) store.Profile {
			return store.Profile{Name: "Default", Kind: settings.ProxyGateways}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := fakebt.New(t)
			fake.SetCA(testCAPEM)
			opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
			saved, _ := opts.saved(io.Discard)
			prof := c.prof(t)

			_, err := opts.raise(saved, false, &warmSet{})(t.Context(), web.Wanted{Profile: prof,
				Threads: 20, Ports: 1, Device: blanktrail.DeviceDesktop})
			if !errors.Is(err, web.ErrNoExits) {
				t.Fatalf("raising a profile with %s: %v, want web.ErrNoExits", c.name, err)
			}
			if prof.Location != "" && strings.Contains(err.Error(), filepath.Base(prof.Location)) {
				t.Errorf("the answer says where the list lives: %v", err)
			}
			if !strings.Contains(err.Error(), `"Default"`) {
				t.Errorf("the answer does not name the profile: %v", err)
			}
			// And the same from dial itself, which the standing identities and a
			// connection saved in the browser are raised through as well.
			if _, err := opts.dial(t.Context(), saved, web.Wanted{Profile: prof,
				Threads: 20, Ports: 1, Device: blanktrail.DeviceDesktop}); !errors.Is(err, web.ErrNoExits) {
				t.Errorf("dialling a profile with %s: %v, want web.ErrNoExits", c.name, err)
			}
			if asked := fake.Requests(); len(asked) != 0 {
				t.Errorf("the service was asked %d things, starting %s %s, for a profile with no exits",
					len(asked), asked[0].Method, asked[0].Path)
			}
		})
	}
}

func TestRaise_AsksTheServiceOnlyForItsGatewaysWhenNoneOfTheTickedOnesIsThere(t *testing.T) {
	// The gateways live in the service, so finding that none of the ticked ones
	// is there any more is one question to it — and nothing else.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	fake.SetGateways([]fakebt.Gateway{{Name: "somebody-else", Kind: "vless"}})
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	prof := store.Profile{Name: "vpn", Kind: settings.ProxyGateways, Gateways: []string{"gone-one"}}

	_, err := opts.raise(saved, false, &warmSet{})(t.Context(), web.Wanted{Profile: prof,
		Threads: 2, Ports: 1, Device: blanktrail.DeviceDesktop})
	if !errors.Is(err, web.ErrNoExits) {
		t.Fatalf("raising a profile whose gateways are all gone: %v, want web.ErrNoExits", err)
	}
	for _, r := range fake.Requests() {
		if r.Method != "GET" || r.Path != "/api/v1/ovpn" {
			t.Errorf("the service was asked %s %s, want only the gateways read", r.Method, r.Path)
		}
	}
}

func TestRaise_DoesNotRunAJobOnTheStandingIdentitiesOnceTheirProfileNamesNoExits(t *testing.T) {
	// The standing identities were opened on the default profile while it had
	// exits. A job on that profile once it names none is answered as any other
	// such job: it does not start on what was left open from before.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	standing, err := opts.dial(t.Context(), saved, web.Wanted{Profile: withExitsAs(t, 1), Threads: 1, Ports: 2,
		Device: blanktrail.DeviceDesktop})
	if err != nil {
		t.Fatalf("opening the standing identities: %v", err)
	}
	t.Cleanup(func() { _ = standing.Search.Close() })
	standing.Search.KeepWarm()
	warm := &warmSet{pool: standing.Search, device: blanktrail.DeviceDesktop, profile: 1}
	before := len(fake.Requests())

	_, err = opts.raise(saved, false, warm)(t.Context(), web.Wanted{Profile: store.Profile{ID: 1, Name: "Default"},
		Threads: 4, Ports: 1, Device: blanktrail.DeviceDesktop})
	if !errors.Is(err, web.ErrNoExits) {
		t.Fatalf("raising a job on the standing identities' profile with no exits: %v, want web.ErrNoExits", err)
	}
	if asked := fake.Requests()[before:]; len(asked) != 0 {
		t.Errorf("the service was asked %d things for a job on a profile with no exits, starting %s %s",
			len(asked), asked[0].Method, asked[0].Path)
	}
}

func TestServe_OpensNothingOnADefaultProfileWithNoExitsAndStartsItsJobOnceItHasSome(t *testing.T) {
	// A fresh machine: the settings name no list, so the default profile carried
	// out of them names no exits. Nothing is opened, not even the identities
	// kept warm, and the interface still takes jobs: one on that profile waits
	// in the queue, says why, and starts once the profile names somewhere to go
	// — without the program being started again.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, "")
	t.Setenv(envAPIKey, "")
	t.Setenv(envProxyList, "")
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key(), HotPorts: 2})
	st, err := store.Open(opts.DB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	saved, _ := opts.saved(io.Discard)
	if _, err := st.CarryProxySettings(t.Context(), theFirstProfile(saved, true)); err != nil {
		t.Fatalf("carrying the settings into a profile: %v", err)
	}

	var out bytes.Buffer
	sup, warm := opts.jobs(t.Context(), &out, st)
	t.Cleanup(func() { _ = warm.Close() })
	t.Cleanup(func() { _ = sup.Close() })

	if !sup.CanRun() {
		t.Fatalf("the interface runs nothing at all on a default profile with no exits:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "names no exits") {
		t.Errorf("nothing says why no identities are kept warm:\n%s", out.String())
	}
	if asked := fake.Requests(); len(asked) != 0 {
		t.Errorf("the service was asked %d things at start, starting %s %s, with nowhere to go out through",
			len(asked), asked[0].Method, asked[0].Path)
	}

	id, err := sup.Enqueue(store.JobSpec{Name: "fresh", Pages: 1, Country: "us", Language: "en",
		Threads: 2, Ports: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the job waits for exits", func() bool { _, ok := sup.WaitingForExits(id); return ok })
	if len(fake.OpenPorts()) != 0 || len(fake.Requests()) != 0 {
		t.Errorf("a job on a profile with no exits asked the service %d things and opened %d ports",
			len(fake.Requests()), len(fake.OpenPorts()))
	}

	// Exits are added to the profile, as the proxies screen does it.
	prof, err := st.DefaultProfile(t.Context())
	if err != nil {
		t.Fatalf("DefaultProfile: %v", err)
	}
	prof.Kind, prof.Location = "file", listAt(t, "192.0.2.7:1080\n")
	if err := st.SaveProfile(t.Context(), prof); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	sup.ExitsChanged()
	waitFor(t, "the job's ports are opened", func() bool { return len(fake.OpenPorts()) > 0 })
	// The pool goes up first and the job stops waiting the moment it is handed
	// over, so the two are a breath apart.
	waitFor(t, "the job stops waiting for exits", func() bool { _, ok := sup.WaitingForExits(id); return !ok })
}

func TestWarmSet_OpensNothingOnAProfileWithNoExitsAndComesUpOnceItHasSome(t *testing.T) {
	// The identities kept warm are the default profile's. With nowhere to go out
	// through none is opened, and what the settings asked for is not forgotten:
	// once the profile names exits the set comes up by itself.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)

	var mu sync.Mutex
	prof := store.Profile{ID: 1, Name: "Default"}
	warm := &warmSet{tend: 20 * time.Millisecond}
	warm.dial = func(ctx context.Context, want int, device string) (*blanktrail.Pool, int64, error) {
		mu.Lock()
		on := prof
		mu.Unlock()
		got, err := opts.dial(ctx, saved, web.Wanted{Profile: on, Threads: 1, Ports: want, Device: device})
		if err != nil {
			return nil, 0, err
		}
		return got.Search, on.ID, nil
	}
	t.Cleanup(func() { _ = warm.Close() })

	if err := warm.bring(t.Context(), 2, blanktrail.DeviceDesktop); !errors.Is(err, web.ErrNoExits) {
		t.Fatalf("bringing the set up on a profile with no exits: %v, want web.ErrNoExits", err)
	}
	time.Sleep(100 * time.Millisecond)
	if warm.Pool() != nil || len(fake.Requests()) != 0 {
		t.Fatalf("a set on a profile with no exits asked the service %d things", len(fake.Requests()))
	}

	mu.Lock()
	prof = withExitsAs(t, 1)
	mu.Unlock()
	waitFor(t, "the set comes up on the profile's new exits", func() bool { return warm.Pool() != nil })
	if got := len(fake.OpenPorts()); got != 2 {
		t.Errorf("the set holds %d ports, want the 2 the settings asked for", got)
	}
}

func TestSkipWithoutExits_SkipsOnlyForSettingsThatNameNoExits(t *testing.T) {
	var skipped, other bool
	t.Run("no exits", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		skipWithoutExits(t, noExits(store.Profile{Name: "Default"}))
	})
	t.Run("another refusal", func(t *testing.T) {
		defer func() { other = t.Skipped() }()
		skipWithoutExits(t, errors.New("the stand refused"))
	})
	if !skipped || other {
		t.Errorf("skipped on no exits: %v, on another refusal: %v; want only the first", skipped, other)
	}
}

func TestRaise_TakesASuggestionsJobOnAProfileWithNoExitsOutDirectly(t *testing.T) {
	// The rule that a profile with no exits raises nothing is for jobs that ask
	// through sessions. A search suggestions job keeps none, and on such a
	// profile it goes out directly through the service's ports, as it always
	// did — the user's choice. A search job on the same profile still waits.
	for _, c := range []struct {
		name string
		prof func(t *testing.T) store.Profile
	}{
		{"no source at all", func(*testing.T) store.Profile { return store.Profile{Name: "Default"} }},
		{"a list with nothing in it", func(t *testing.T) store.Profile {
			return store.Profile{Name: "Default", Kind: "file", Location: listAt(t, "")}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := fakebt.New(t)
			fake.SetCA(testCAPEM)
			opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
			saved, _ := opts.saved(io.Discard)
			raise := opts.raise(saved, false, &warmSet{})
			prof := c.prof(t)

			if _, err := raise(t.Context(), web.Wanted{Profile: prof, Threads: 2, Ports: 1,
				Device: blanktrail.DeviceDesktop}); !errors.Is(err, web.ErrNoExits) {
				t.Fatalf("a search job on a profile with no exits: %v, want web.ErrNoExits", err)
			}
			if n := len(fake.OpenPorts()); n != 0 {
				t.Fatalf("a search job on a profile with no exits opened %d ports", n)
			}

			got, err := raise(t.Context(), web.Wanted{Profile: prof, Threads: 2, Ports: 1,
				Device: blanktrail.DeviceDesktop, KeepAlive: true})
			if err != nil {
				t.Fatalf("a suggestions job on a profile with no exits: %v, want its ports opened", err)
			}
			t.Cleanup(func() { _ = got.Search.Close() })
			ports := fake.OpenPorts()
			if len(ports) == 0 {
				t.Fatal("a suggestions job on a profile with no exits opened no ports")
			}
			for _, port := range ports {
				// The fake writes "direct" for a port opened with no upstream.
				if up := fake.UpstreamOf(port); up != "direct" {
					t.Errorf("port %d goes out through an address, want directly", port)
				}
			}
		})
	}
}

func TestServe_RunsASuggestionsJobOnADefaultProfileWithNoExitsAndHoldsASearchJob(t *testing.T) {
	// The same through the queue: on a fresh machine with nothing in its
	// default profile a suggestions job is raised and runs, and a search job
	// waits for exits.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	t.Setenv(envControlURL, "")
	t.Setenv(envAPIKey, "")
	t.Setenv(envProxyList, "")
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	st, err := store.Open(opts.DB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	saved, _ := opts.saved(io.Discard)
	if _, err := st.CarryProxySettings(t.Context(), theFirstProfile(saved, true)); err != nil {
		t.Fatalf("carrying the settings into a profile: %v", err)
	}
	sup, warm := opts.jobs(t.Context(), io.Discard, st)
	t.Cleanup(func() { _ = warm.Close() })
	t.Cleanup(func() { _ = sup.Close() })

	search, err := sup.Enqueue(store.JobSpec{Name: "search", Pages: 1, Country: "us", Language: "en",
		Threads: 1, Ports: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the search job waits for exits", func() bool { _, ok := sup.WaitingForExits(search); return ok })
	if n := len(fake.OpenPorts()); n != 0 {
		t.Fatalf("a search job on a profile with no exits opened %d ports", n)
	}

	suggest, err := sup.Enqueue(store.JobSpec{Name: "suggest", Kind: store.KindSuggest, Pages: 1,
		Country: "us", Language: "en", Threads: 1, Ports: 1}, []string{"tea"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the suggestions job's ports are opened", func() bool { return len(fake.OpenPorts()) > 0 })
	if _, ok := sup.WaitingForExits(suggest); ok {
		t.Error("a suggestions job is waiting for exits")
	}
	if _, ok := sup.WaitingForExits(search); !ok {
		t.Error("the search job stopped waiting although its profile still names no exits")
	}
}

func TestWarmSearch_FindsTheWarmIdentitiesThatCameUpAfterTheServerStarted(t *testing.T) {
	// The search answered inside a request goes through the identities kept
	// warm. On a default profile with no exits there are none when the server
	// starts, and they come up by themselves later; the search looks them up at
	// each request rather than refusing until a restart.
	fake := fakebt.New(t)
	fake.SetCA(testCAPEM)
	opts := configured(t, settings.Settings{ControlURL: fake.URL(), APIKey: fake.Key()})
	saved, _ := opts.saved(io.Discard)
	warm := &warmSet{}
	search := warmSearch{warm: warm}

	q := google.Query{Text: "tea", Country: "us", Language: "en"}
	if _, err := search.Search(t.Context(), q); !errors.Is(err, api.ErrNoSearcher) {
		t.Fatalf("a search with no warm identities: %v, want api.ErrNoSearcher", err)
	}

	got, err := opts.dial(t.Context(), saved, web.Wanted{Profile: withExits(t), Threads: 1, Ports: 1,
		Device: blanktrail.DeviceDesktop})
	if err != nil {
		t.Fatalf("opening the warm identities: %v", err)
	}
	warm.start(got.Search, blanktrail.DeviceDesktop, 0)
	t.Cleanup(func() { _ = warm.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if _, err := search.Search(ctx, q); errors.Is(err, api.ErrNoSearcher) {
		t.Fatal("the search still has nothing to search with after the warm identities came up")
	}
}

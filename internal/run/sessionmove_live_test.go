//go:build live

// SPDX-License-Identifier: MIT

package run

// Does a session survive being moved to another port?
//
// This is the question a session store turns on. The design is that this
// program keeps the sessions — a fingerprint, named by a profile the service
// holds, and a set of cookies — and hands one to whichever thread wants it, on
// whichever port that thread is holding. Ports stop being identities and become
// wires.
//
// The service was changed for it: a port with keep_sessions off keeps neither
// the solver's pin nor its jar, the cookies won by passing a challenge come back
// with the answered page, and reset_solver_sessions clears a port. Measured on
// the rebuilt stand: the first request pays for the challenge and comes back as
// a page carrying GOOGLE_ABUSE_EXEMPTION, and every request after that on the
// same jar answers in seconds with no new cookie at all. So the question is no
// longer whether the cookies can be had. It is whether they are the session.
//
// Three arms, all on one profile:
//
//	settle — addresses tried until one gets an answered page; that session,
//	         its port and its jar, is the settled one, and is asked on.
//	moved  — another port, the same profile put on it, the settled jar.
//	bare   — another port, the same profile, and an empty jar.
//
// A session is portable if moved answers without paying for a challenge and
// bare pays for one. If both pay, the cookies are not the session and a session
// has to stay on the port it was made on.
//
// What an arm skips and what it counts is the whole of whether the answer can
// be trusted. An address that never reached Google — the proxy under it dead —
// says nothing about sessions and is skipped. An address that did reach Google
// is counted whatever Google said, because what Google said is the measurement.
//
// An earlier version proved every address with a session of its own, cleared
// the port and started the arm's session on it. It measured the proving instead:
// a new session from an address that had just passed a challenge failed at once,
// 2m6s and then refusals in under five seconds with the solver never engaging.
//
//	go test -tags live -count=1 -run TestLiveSessionMove -timeout 40m ./internal/run/ -v
//
// It holds three identities and spends about twenty requests.

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
)

// sessionMoveSlow is how long an answer takes when it went through the solver.
//
// Thirty seconds. Measured on this stand, a challenge solved costs 1m39s to
// 1m58s and a warm request costs 2.7s to 7.0s; six seconds, which this was,
// counted two warm answers of 6.3s and 7.0s as challenges.
const sessionMoveSlow = 30 * time.Second

// sessionMoveAsks is how many searches an arm makes.
const sessionMoveAsks = 4

// sessionMoveTries is how many addresses an arm works through to find one that
// reaches Google at all. On a large cheap list three addresses in four carry
// nothing.
const sessionMoveTries = 12

func TestLiveSessionMove_WhetherASessionIsPortableBetweenPorts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)
	client := liveClient(t)

	settled, lease, jar, profile, ok := settle(ctx, t, pool, client)
	if !ok {
		t.Skip("no address gave an answered page, so there is no session to move")
	}
	defer lease.Release()

	// What the session is made of, host by host. A search for Russia wins its
	// clearance on google.ru; the consent flow sets cookies on google.com.
	for _, host := range sessionHosts {
		held := jar.Cookies(hostAt(t, host))
		names := make([]string, 0, len(held))
		for _, one := range held {
			names = append(names, one.Name)
		}
		logf(t, "MEASUREMENT the settled session holds %d cookies for %s: %v", len(held), host, names)
	}

	// The move that keeps the address: another port, the settled session's own
	// upstream put on it, the same profile, the same jar. The last run of this
	// found the clearance does not survive a change of address — a moved
	// session paid 1m40s, the empty control 1m54s — which is what the pool's
	// own notes say: clearance binds to IP, UA and TLS at once. So this is the
	// arm the design turns on: if a port is only a wire, a session that brings
	// its address with it should need no challenge at all.
	sameIP := sameAddress(ctx, t, pool, client, lease, profile, jar)
	// The move to another address: the jar, the profile, a different upstream.
	moved := arm(ctx, t, pool, client, "moved", profile, jar)

	logf(t, "MEASUREMENT %s", "----------------------------------------------------------")
	for _, r := range []asked{settled, sameIP, moved} {
		logf(t, "MEASUREMENT %s: port %d, %d asked, %d answered, %d through the solver, median %v, %d Set-Cookie over %d responses",
			r.name, r.port, r.made, r.answered, r.slow, r.median.Round(time.Millisecond),
			r.setCookies, r.responses)
	}
	logf(t, "MEASUREMENT a session is portable between ports if same-ip paid for no challenge")
}

// settle tries addresses until one gets an answered page, and asks on through
// that session. The session is kept exactly as it was made: nothing is cleared
// and nothing is started again on the address, because a new session from an
// address that has just passed a challenge is refused at once.
func settle(ctx context.Context, t *testing.T, pool *blanktrail.Pool, client *blanktrail.Client) (
	asked, *blanktrail.Lease, http.CookieJar, string, bool) {
	t.Helper()
	for try := 1; try <= sessionMoveTries; try++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			logf(t, "MEASUREMENT settle: no identity to try: %v", err)
			return asked{}, nil, nil, "", false
		}
		if _, err := client.WearSession(ctx, lease.Port(), ""); err != nil {
			logf(t, "MEASUREMENT settle: port %d would not take our session: %v", lease.Port(), err)
			lease.Release()
			return asked{}, nil, nil, "", false
		}
		jar, _ := cookiejar.New(nil)
		result := askThrough(ctx, t, "settle", lease, jar, sessionMoveAsks)
		if result.answered == 0 {
			logf(t, "MEASUREMENT settle: address %d on port %d answered nothing, trying another", try, lease.Port())
			_ = lease.Reject(ctx)
			lease.Release()
			continue
		}
		profile, err := client.PortProfile(ctx, lease.Port())
		if err != nil || profile.Name == "" {
			logf(t, "MEASUREMENT settle: port %d does not say what it wears: %v", lease.Port(), err)
			lease.Release()
			return asked{}, nil, nil, "", false
		}
		logf(t, "MEASUREMENT settle: session settled on port %d, wearing %s", lease.Port(), profile.Name)
		return result, lease, jar, profile.Name, true
	}
	logf(t, "MEASUREMENT settle: no address gave an answered page in %d tries", sessionMoveTries)
	return asked{}, nil, nil, "", false
}

// arm takes another port, puts the profile on it, and asks through it with the
// jar it is given — or an empty one. An address that never reaches Google is
// skipped; one that does is the arm, whatever Google answered.
func arm(ctx context.Context, t *testing.T, pool *blanktrail.Pool, client *blanktrail.Client,
	name, profile string, carry http.CookieJar) asked {
	t.Helper()
	for try := 1; try <= sessionMoveTries; try++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			logf(t, "MEASUREMENT %s: no identity to try: %v", name, err)
			return asked{name: name}
		}
		worn, err := client.WearSession(ctx, lease.Port(), profile)
		if err != nil {
			logf(t, "MEASUREMENT %s: port %d would not take the session: %v", name, lease.Port(), err)
			lease.Release()
			return asked{name: name}
		}
		if worn.Name != profile {
			logf(t, "MEASUREMENT %s: port %d wears %q after being asked for %q", name, lease.Port(), worn.Name, profile)
		}
		jar := carry
		if jar == nil {
			fresh, _ := cookiejar.New(nil)
			jar = fresh
		}
		result := askThrough(ctx, t, name, lease, jar, sessionMoveAsks)
		if result.reachedGoogle == 0 {
			// The proxy under this port never got a request to Google. That is
			// the address, and it says nothing about the session.
			logf(t, "MEASUREMENT %s: address %d on port %d never reached Google, trying another", name, try, lease.Port())
			_ = lease.Reject(ctx)
			lease.Release()
			continue
		}
		lease.Release()
		return result
	}
	logf(t, "MEASUREMENT %s: no address reached Google in %d tries", name, sessionMoveTries)
	return asked{name: name}
}

// sameAddress takes another port, puts the settled session's own upstream on it,
// then its profile, and asks through it with the settled jar. The upstream is
// read off the settled lease and handed to the service; it is never written to
// the log, because it carries the proxy's credentials.
func sameAddress(ctx context.Context, t *testing.T, pool *blanktrail.Pool, client *blanktrail.Client,
	settled *blanktrail.Lease, profile string, jar http.CookieJar) asked {
	t.Helper()
	upstream := settled.Egress().Upstream
	if upstream == "" {
		logf(t, "MEASUREMENT same-ip: the settled session has no upstream to carry")
		return asked{name: "same-ip"}
	}
	lease, err := pool.Acquire(ctx)
	if err != nil {
		logf(t, "MEASUREMENT same-ip: no identity to try: %v", err)
		return asked{name: "same-ip"}
	}
	defer lease.Release()
	if lease.Port() == settled.Port() {
		logf(t, "MEASUREMENT same-ip: the pool handed back the settled port itself")
		return asked{name: "same-ip"}
	}
	if err := client.SetUpstream(ctx, lease.Port(), upstream); err != nil {
		logf(t, "MEASUREMENT same-ip: port %d would not take the settled address", lease.Port())
		return asked{name: "same-ip"}
	}
	worn, err := client.WearSession(ctx, lease.Port(), profile)
	if err != nil {
		logf(t, "MEASUREMENT same-ip: port %d would not take the session: %v", lease.Port(), err)
		return asked{name: "same-ip"}
	}
	logf(t, "MEASUREMENT same-ip: port %d carries the settled address and wears %s", lease.Port(), worn.Name)
	return askThrough(ctx, t, "same-ip", lease, jar, sessionMoveAsks)
}

// asked is what one arm cost.
type asked struct {
	name     string
	port     int
	made     int
	answered int
	slow     int
	// reachedGoogle counts the requests Google answered at all — a page, a
	// shell, a challenge — as opposed to the ones the proxy never delivered.
	reachedGoogle int
	median        time.Duration
	// responses is how many answers came back at all, and setCookies how many
	// Set-Cookie headers they carried between them.
	responses  int
	setCookies int
}

// askThrough makes n searches through one lease on one jar, and says what they
// cost. The jar is this program's rather than the port's: the port keeps neither
// pin nor jar, so the cookies that come back reach here and the ones that go out
// come from here.
func askThrough(ctx context.Context, t *testing.T, name string, lease *blanktrail.Lease,
	jar http.CookieJar, n int) asked {
	t.Helper()
	cl := lease.Client()
	// Wrapped so the raw Set-Cookie of every response is counted.
	watched := &countsCookies{under: cl.Transport}
	s := google.NewSession(watched)
	s.Client.Timeout = cl.Timeout
	s.Client.Jar = jar

	out := asked{name: name, port: lease.Port()}
	var took []time.Duration
	for i := 0; i < n; i++ {
		began := time.Now()
		_, err := s.Search(ctx, google.Query{
			Text: phrases[i%len(phrases)], Country: "ru", Language: "ru"})
		spent := time.Since(began)
		out.made++
		if err != nil {
			if _, judged := google.ClassOf(err); judged {
				out.reachedGoogle++
				logf(t, "MEASUREMENT %s: request %d on port %d refused by Google after %v",
					name, i+1, lease.Port(), spent.Round(time.Millisecond))
			} else {
				logf(t, "MEASUREMENT %s: request %d on port %d never reached Google (%v)",
					name, i+1, lease.Port(), spent.Round(time.Millisecond))
				if out.reachedGoogle == 0 && out.answered == 0 {
					// A dead address on the first request: nothing after it
					// will be any different, and the arm tries another.
					break
				}
			}
			continue
		}
		out.answered++
		out.reachedGoogle++
		took = append(took, spent)
		if spent >= sessionMoveSlow {
			out.slow++
		}
		logf(t, "MEASUREMENT %s: request %d on port %d answered in %v",
			name, i+1, lease.Port(), spent.Round(time.Millisecond))
	}
	out.median = medianOf(took)
	out.setCookies = watched.set
	out.responses = watched.seen
	return out
}

// countsCookies passes every request through and counts the Set-Cookie headers
// coming back.
type countsCookies struct {
	under http.RoundTripper
	mu    sync.Mutex
	seen  int
	set   int
}

func (c *countsCookies) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.under.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	c.mu.Lock()
	c.seen++
	c.set += len(resp.Header.Values("Set-Cookie"))
	c.mu.Unlock()
	return resp, nil
}

// sessionHosts are the hosts a Russian search session sets cookies on: the
// country's own domain, where the search and its clearance live, and the .com
// one the consent flow can pass through.
var sessionHosts = []string{"www.google.ru", "www.google.com"}

func hostAt(t *testing.T, host string) *url.URL {
	t.Helper()
	at, err := url.Parse("https://" + host + "/")
	if err != nil {
		t.Fatalf("parsing the address: %v", err)
	}
	return at
}

// medianOf is the middle of what was measured, which says what a request cost
// without one challenge dragging the average with it.
func medianOf(all []time.Duration) time.Duration {
	if len(all) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), all...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted[len(sorted)/2]
}

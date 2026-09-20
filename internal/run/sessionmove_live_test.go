//go:build live

// SPDX-License-Identifier: MIT

package run

// Does a session survive being moved to another port?
//
// This is the question the whole of a session store turns on. The design is
// that this program keeps the sessions — a fingerprint, named by a profile the
// service holds, and a set of cookies — and hands one to whichever thread wants
// it, on whichever port that thread happens to be holding. Ports stop being
// identities and become wires.
//
// It only works if a session presented on a second port is the same session to
// Google. What makes that doubtful is the clearance cookie: a challenge is
// solved by the service, on a port, and with keep_sessions off the port merges
// its own solved clearance over the request's for those names. That pin belongs
// to the port. Whether our copy of it, replayed from our own jar onto a port
// that has never solved anything, is accepted is not something the code can be
// read for.
//
// Three arms on one profile, in order:
//
//	settle — a port, our own cookie jar, asked until a challenge is met and
//	         passed. That is a session, and its cookies are ours.
//	moved  — a second port wearing the same profile, our jar carried over.
//	bare   — a third port wearing the same profile and an empty jar.
//
// If moved answers quickly and bare pays for a challenge, a session is portable
// and the store is worth building. If both pay, a session belongs to its port
// and the design has to change before a line of it is written.
//
//	go test -tags live -run TestLiveSessionMove -timeout 40m ./internal/run/ -v
//
// It holds three identities and spends about thirty requests.

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
// A request through an identity that has answered costs one to two seconds; a
// challenge costs tens.
const sessionMoveSlow = 6 * time.Second

// sessionMoveAsks is how many searches an arm makes after it is settled.
const sessionMoveAsks = 3

func TestLiveSessionMove_WhetherASessionIsPortableBetweenPorts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)
	client := liveClient(t)

	// The first port, and the profile every arm will wear. It is read off the
	// port rather than chosen, because what matters is that all three wear the
	// same one — not which one it is.
	first, err := pool.Acquire(ctx)
	if err != nil {
		t.Skipf("no identity to settle a session on: %v", err)
	}
	defer first.Release()

	profile, err := client.PortProfile(ctx, first.Port())
	if err != nil || profile.Name == "" {
		t.Skipf("the port does not say which profile it wears: %v (%+v)", err, profile)
	}
	logf(t, "MEASUREMENT the session is settled on profile %s", profile.Name)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	settled := askThrough(ctx, t, "settle", first, jar, sessionMoveAsks)
	if settled.answered == 0 {
		t.Skip("the first port answered nothing, so there is no session to move")
	}
	kept := cookiesIn(t, jar)
	logf(t, "MEASUREMENT the settled session holds %d cookies for google.com", len(kept))
	if len(kept) == 0 {
		logf(t, "MEASUREMENT no cookie reached this program at all, so nothing can be carried")
	}

	// The move. A second port, the same profile put on it, and the jar the
	// first port filled.
	moved := arm(ctx, t, pool, client, "moved", profile.Name, kept)
	bare := asked{name: "bare"}
	if !testing.Short() {
		// The control. A third port, the same profile, and nothing carried.
		bare = arm(ctx, t, pool, client, "bare", profile.Name, nil)
	}

	logf(t, "MEASUREMENT %s", "----------------------------------------------------------")
	for _, r := range []asked{settled, moved, bare} {
		logf(t, "MEASUREMENT %s: %d asked, %d answered, %d through the solver, median %v, %d Set-Cookie over %d responses",
			r.name, r.made, r.answered, r.slow, r.median.Round(time.Millisecond),
			r.setCookies, r.responses)
	}
	logf(t, "MEASUREMENT a session is portable if moved went through the solver and bare did not")
}

// arm takes a fresh identity, puts the named profile on it, seeds a jar with
// what was carried, and asks through it.
func arm(ctx context.Context, t *testing.T, pool *blanktrail.Pool, client *blanktrail.Client,
	name, profile string, carry []*http.Cookie) asked {
	t.Helper()
	lease, err := pool.Acquire(ctx)
	if err != nil {
		logf(t, "MEASUREMENT %s: no identity to ask through: %v", name, err)
		return asked{name: name}
	}
	defer lease.Release()

	worn, err := client.WearProfile(ctx, lease.Port(), profile)
	if err != nil {
		logf(t, "MEASUREMENT %s: the profile could not be put on the port: %v", name, err)
		return asked{name: name}
	}
	if worn.Name != profile {
		logf(t, "MEASUREMENT %s: the port wears %q after being asked for %q", name, worn.Name, profile)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	if len(carry) > 0 {
		jar.SetCookies(googleAt(t), carry)
	}
	return askThrough(ctx, t, name, lease, jar, sessionMoveAsks)
}

// asked is what one arm cost.
type asked struct {
	name              string
	made              int
	answered          int
	slow              int
	median            time.Duration
	firstThroughSolve int
	// responses is how many answers came back at all, and setCookies how many
	// Set-Cookie headers they carried between them.
	responses  int
	setCookies int
}

// askThrough makes n searches through one lease on one jar, and says what they
// cost. The jar is this program's rather than the port's, which is the whole
// point: the port is opened with keep_sessions off, so the cookies that come
// back reach here and the ones that go out come from here.
func askThrough(ctx context.Context, t *testing.T, name string, lease *blanktrail.Lease,
	jar http.CookieJar, n int) asked {
	t.Helper()
	cl := lease.Client()
	// Wrapped so the raw Set-Cookie of every response is counted. An empty jar
	// has two explanations — the far end set nothing, or the port kept it — and
	// they lead to different programs. This is the only place that can tell
	// them apart.
	watched := &countsCookies{under: cl.Transport}
	s := google.NewSession(watched)
	s.Client.Timeout = cl.Timeout
	s.Client.Jar = jar

	out := asked{name: name, firstThroughSolve: -1}
	var took []time.Duration
	for i := 0; i < n; i++ {
		began := time.Now()
		_, err := s.Search(ctx, google.Query{
			Text: phrases[i%len(phrases)], Country: "ru", Language: "ru"})
		spent := time.Since(began)
		out.made++
		if err != nil {
			logf(t, "MEASUREMENT %s: request %d refused after %v", name, i+1, spent.Round(time.Millisecond))
			continue
		}
		out.answered++
		took = append(took, spent)
		if spent >= sessionMoveSlow {
			out.slow++
			if out.firstThroughSolve < 0 {
				out.firstThroughSolve = i + 1
			}
		}
		logf(t, "MEASUREMENT %s: request %d answered in %v", name, i+1, spent.Round(time.Millisecond))
	}
	out.median = medianOf(took)
	out.setCookies = watched.set
	out.responses = watched.seen
	logf(t, "MEASUREMENT %s: %d responses carried %d Set-Cookie headers in all",
		name, watched.seen, watched.set)
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

// cookiesIn is what a jar holds for Google, as cookies to put in another jar.
func cookiesIn(t *testing.T, jar http.CookieJar) []*http.Cookie {
	t.Helper()
	return jar.Cookies(googleAt(t))
}

func googleAt(t *testing.T) *url.URL {
	t.Helper()
	at, err := url.Parse("https://www.google.com/")
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

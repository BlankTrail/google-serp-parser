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
// The service was changed for it: a port with keep_sessions off now keeps
// neither the solver's pin nor its jar, the cookies won by passing a challenge
// come back with the response, and reset_solver_sessions clears both off a port
// so the next session starts on a clean one. So the question is no longer
// whether the cookies can be had. It is whether they are the session.
//
// Three arms, all on one profile, each on an address proved to answer and a
// port proved clean:
//
//	settle — asked until a challenge is met and passed. That is a session, and
//	         its cookies are now this program's.
//	moved  — another port, the same profile put on it, that jar carried over.
//	bare   — another port, the same profile, and nothing carried.
//
// A session is portable if moved answers without paying for a challenge and
// bare pays for one. If both pay, the cookies are not the session and the store
// has to be built around a session that stays where it was made.
//
// Every arm is given an address that has already answered and a port that has
// then been cleared, so an arm that fails is failing on what it is about rather
// than on a dead proxy — which is what the first two runs of this measured
// instead: one arm answered nothing at all.
//
//	go test -tags live -run TestLiveSessionMove -timeout 40m ./internal/run/ -v
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
// A request through an identity that has answered costs one to two seconds; a
// challenge costs tens.
const sessionMoveSlow = 6 * time.Second

// sessionMoveAsks is how many searches an arm makes.
const sessionMoveAsks = 4

// sessionMoveTries is how many addresses an arm works through to find one that
// answers. On a large cheap list three addresses in four carry nothing.
const sessionMoveTries = 12

func TestLiveSessionMove_WhetherASessionIsPortableBetweenPorts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	pool := livePool(ctx, t)
	client := liveClient(t)

	// The first port, and the profile every arm will wear. It is read off the
	// port rather than chosen, because what matters is that all three wear the
	// same one — not which one it is.
	first, ok := workingPort(ctx, t, pool, client, "settle")
	if !ok {
		t.Skip("no address answered, so there is nothing to settle a session on")
	}
	defer first.Release()

	profile, err := client.PortProfile(ctx, first.Port())
	if err != nil || profile.Name == "" {
		t.Skipf("the port does not say which profile it wears: %v (%+v)", err, profile)
	}
	logf(t, "MEASUREMENT every arm wears %s", profile.Name)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	settled := askThrough(ctx, t, "settle", first, jar, sessionMoveAsks)
	if settled.answered == 0 {
		t.Skip("the first port answered nothing, so there is no session to move")
	}
	// What the session is made of, host by host. A search for Russia goes to
	// google.ru, and that is where the clearance is set: the first run of this
	// carried only what the jar held for google.com — four cookies, and not the
	// two the challenge was won with — and so moved a session that was missing
	// the one part of it that mattered.
	for _, host := range sessionHosts {
		held := jar.Cookies(hostAt(t, host))
		names := make([]string, 0, len(held))
		for _, one := range held {
			names = append(names, one.Name)
		}
		logf(t, "MEASUREMENT the settled session holds %d cookies for %s: %v", len(held), host, names)
	}

	// The move. Another port, the same profile put on it, and the very jar the
	// first port filled — the whole of it, every host, which is what a session
	// store would hand over.
	moved := arm(ctx, t, pool, client, "moved", profile.Name, jar)
	// The control. Another port, the same profile, and a jar with nothing in it.
	bare := arm(ctx, t, pool, client, "bare", profile.Name, nil)

	logf(t, "MEASUREMENT %s", "----------------------------------------------------------")
	for _, r := range []asked{settled, moved, bare} {
		logf(t, "MEASUREMENT %s: %d asked, %d answered, %d through the solver, median %v, %d Set-Cookie over %d responses",
			r.name, r.made, r.answered, r.slow, r.median.Round(time.Millisecond),
			r.setCookies, r.responses)
	}
	logf(t, "MEASUREMENT a session is portable if moved paid for no challenge and bare paid for one")
}

// workingPort leases identities until one answers a search, then clears what
// that search left on it.
//
// Both halves matter. Without the first, an arm can be handed an address that
// carries nothing, and its numbers then say so rather than saying anything
// about sessions — which is what the first two runs of this measured. Without
// the second, the arm starts on a port that has already been through whatever
// the proving request met, so a control arm would not be one.
func workingPort(ctx context.Context, t *testing.T, pool *blanktrail.Pool,
	client *blanktrail.Client, name string) (*blanktrail.Lease, bool) {
	t.Helper()
	for try := 1; try <= sessionMoveTries; try++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			logf(t, "MEASUREMENT %s: no identity to try: %v", name, err)
			return nil, false
		}
		// The port is put into our-session mode before anything is asked
		// through it. With keep_sessions off it keeps neither the solver's pin
		// nor a jar of its own, which is the mode the whole measurement is
		// about; the pool opens its ports with it on, as every run does today.
		if _, err := client.WearSession(ctx, lease.Port(), ""); err != nil {
			logf(t, "MEASUREMENT %s: port %d would not take our session: %v", name, lease.Port(), err)
			// Which half failed: the connection to the service, or this one
			// call on this one port. A reading that goes through says the
			// service is there and the port is the problem.
			if q, qerr := client.SolverQueue(ctx); qerr != nil {
				logf(t, "MEASUREMENT %s: the service is not answering at all: %v", name, qerr)
			} else {
				logf(t, "MEASUREMENT %s: the service answers (queue %+v), so it is this port", name, q)
			}
			if prof, perr := client.PortProfile(ctx, lease.Port()); perr != nil {
				logf(t, "MEASUREMENT %s: port %d has no profile to read: %v", name, lease.Port(), perr)
			} else {
				logf(t, "MEASUREMENT %s: port %d wears %s", name, lease.Port(), prof.Name)
			}
			lease.Release()
			return nil, false
		}
		// A jar, even for the proving request. In our-session mode the port
		// keeps none, and a search session visits the front page before it
		// searches: without a jar the cookies that page sets are dropped and
		// the search arrives carrying nothing. Measured: twelve addresses in a
		// row answered nothing that way, and the same addresses answer with a
		// jar. In this mode a jar is not an improvement, it is the session.
		proving, err := cookiejar.New(nil)
		if err != nil {
			t.Fatalf("cookiejar: %v", err)
		}
		cl := lease.Client()
		s := google.NewSession(cl.Transport)
		s.Client.Timeout = cl.Timeout
		s.Client.Jar = proving
		began := time.Now()
		if _, err := s.Search(ctx, google.Query{Text: phrases[0], Country: "ru", Language: "ru"}); err != nil {
			logf(t, "MEASUREMENT %s: address %d did not answer", name, try)
			_ = lease.Reject(ctx)
			lease.Release()
			continue
		}
		logf(t, "MEASUREMENT %s: address proved in %v, clearing the port",
			name, time.Since(began).Round(time.Millisecond))
		if err := client.ResetSolverSessions(ctx, lease.Port()); err != nil {
			logf(t, "MEASUREMENT %s: the port could not be cleared: %v", name, err)
			lease.Release()
			return nil, false
		}
		return lease, true
	}
	logf(t, "MEASUREMENT %s: no address answered in %d tries", name, sessionMoveTries)
	return nil, false
}

// arm takes an identity that has answered and been cleared, puts the named
// profile on it, seeds a jar with what was carried, and asks through it.
func arm(ctx context.Context, t *testing.T, pool *blanktrail.Pool, client *blanktrail.Client,
	name, profile string, carry http.CookieJar) asked {
	t.Helper()
	lease, ok := workingPort(ctx, t, pool, client, name)
	if !ok {
		return asked{name: name}
	}
	defer lease.Release()

	worn, err := client.WearSession(ctx, lease.Port(), profile)
	if err != nil {
		logf(t, "MEASUREMENT %s: the profile could not be put on the port: %v", name, err)
		return asked{name: name}
	}
	if worn.Name != profile {
		logf(t, "MEASUREMENT %s: the port wears %q after being asked for %q", name, worn.Name, profile)
	}

	jar := carry
	if jar == nil {
		fresh, err := cookiejar.New(nil)
		if err != nil {
			t.Fatalf("cookiejar: %v", err)
		}
		jar = fresh
	} else {
		logf(t, "MEASUREMENT %s: the settled session's jar carried onto the port", name)
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
// point: the port keeps neither pin nor jar, so the cookies that come back
// reach here and the ones that go out come from here.
func askThrough(ctx context.Context, t *testing.T, name string, lease *blanktrail.Lease,
	jar http.CookieJar, n int) asked {
	t.Helper()
	cl := lease.Client()
	// Wrapped so the raw Set-Cookie of every response is counted. An empty jar
	// has two explanations — the far end set nothing, or something between
	// kept it — and they lead to different programs.
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

// sessionHosts are the hosts a Russian search session sets cookies on: the
// country's own domain, where the search and its clearance live, and the
// .com one the consent flow can pass through.
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

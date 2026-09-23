// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/settings"
	"github.com/blanktrail/google-serp-parser/internal/store"
)

// askedCheck is a service that answers whatever the test says, and remembers
// what it was asked and how many were asked at once.
type askedCheck struct {
	mu sync.Mutex
	// asked is every question, in the order the answers were given.
	asked []askedOne
	// at is how many were being asked this instant, and most the highest that
	// ever was — which is what says whether the threads were held to.
	at, most int
	// answer decides each verdict. Nil answers yes to everything.
	answer func(eg blanktrail.Egress, hop blanktrail.FirstHop) (ok bool, err error)
	// hold, when set, is how long each answer takes, so a test can watch two
	// questions overlap.
	hold time.Duration
}

type askedOne struct {
	Egress blanktrail.Egress
	Hop    blanktrail.FirstHop
}

func (a *askedCheck) TestEgress(ctx context.Context, eg blanktrail.Egress, hop blanktrail.FirstHop,
	checks ...string) (map[string]blanktrail.CheckResult, error) {
	a.mu.Lock()
	a.at++
	if a.at > a.most {
		a.most = a.at
	}
	a.asked = append(a.asked, askedOne{Egress: eg, Hop: hop})
	answer, hold := a.answer, a.hold
	a.mu.Unlock()

	if hold > 0 {
		select {
		case <-time.After(hold):
		case <-ctx.Done():
		}
	}
	ok, err := true, error(nil)
	if answer != nil {
		ok, err = answer(eg, hop)
	}
	a.mu.Lock()
	a.at--
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return map[string]blanktrail.CheckResult{"http": {OK: ok, Detail: "said"}}, nil
}

// alongEach is how many questions took each road.
func (a *askedCheck) alongEach() (through, direct int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, one := range a.asked {
		if one.Hop.IsZero() {
			direct++
		} else {
			through++
		}
	}
	return through, direct
}

// addressesAsked are the addresses that were put to the service, whichever road
// they took.
func (a *askedCheck) addressesAsked() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.asked))
	for _, one := range a.asked {
		out = append(out, one.Egress.Upstream)
	}
	return out
}

// listOf is a source of n addresses, numbered so a test can say which of them
// were chosen.
func listOf(n int) func(context.Context) ([]blanktrail.Egress, error) {
	return func(context.Context) ([]blanktrail.Egress, error) {
		out := make([]blanktrail.Egress, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, blanktrail.Egress{Upstream: fmt.Sprintf("socks5://192.0.2.1:%d", 1000+i)})
		}
		return out, nil
	}
}

// ranCheck runs a check to the end and hands back the reading.
func ranCheck(t *testing.T, c *listCheck, cl checksEgress, ask listCheckAsk,
	load func(context.Context) ([]blanktrail.Egress, error)) checkReading {
	t.Helper()
	done := make(chan struct{})
	c.start(context.Background(), cl, ask, load, func() { close(done) })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the check did not finish")
	}
	return c.Reading()
}

func profileWithHop(hop string) store.Profile {
	return store.Profile{ID: 7, Name: "list", Kind: "url", Location: "http://example.invalid/list",
		FirstHop: hop}
}

func TestListCheck_AsksAboutBothRoadsWhereThereIsAFirstHop(t *testing.T) {
	// The road the ports take is the answer about this profile, and the other
	// one is what makes it readable: on a list reachable only through a hop the
	// two are opposite, and a check that showed one number would be showing the
	// wrong one half the time.
	cl := &askedCheck{}
	var check listCheck
	got := ranCheck(t, &check, cl, listCheckAsk{Profile: profileWithHop("socks5://192.0.2.9:2334"),
		Sample: 5, Threads: 2}, listOf(50))

	through, direct := cl.alongEach()
	if through != 5 || direct != 5 {
		t.Errorf("%d addresses were asked about through the hop and %d directly, want five each", through, direct)
	}
	if got.Through.Asked != 5 || got.Direct.Asked != 5 {
		t.Errorf("the reading counts %d through the hop and %d directly, want five each",
			got.Through.Asked, got.Direct.Asked)
	}
	if !got.Hop {
		t.Error("the reading does not say the ports of this profile take a first hop")
	}
}

func TestListCheck_AsksOneRoadWhereThereIsNoFirstHop(t *testing.T) {
	// With no hop there is one road, and asking it twice would report one
	// answer as two and double what the check costs the service.
	cl := &askedCheck{}
	var check listCheck
	got := ranCheck(t, &check, cl, listCheckAsk{Profile: profileWithHop(""), Sample: 4, Threads: 2}, listOf(50))

	through, direct := cl.alongEach()
	if through != 0 || direct != 4 {
		t.Errorf("%d asked through a hop and %d directly, want none and four", through, direct)
	}
	if got.Hop || got.Through.Asked != 0 {
		t.Errorf("the reading draws a first hop that this profile does not have: %+v", got.Through)
	}
}

func TestListCheck_AsksNoMoreAtOnceThanItWasTold(t *testing.T) {
	// The check is a load on the same service the jobs run through. A box
	// saying twenty that ran two hundred would be this screen taking the
	// service down to answer a question about it.
	cl := &askedCheck{hold: 20 * time.Millisecond}
	var check listCheck
	ranCheck(t, &check, cl, listCheckAsk{Profile: profileWithHop(""), Sample: 30, Threads: 4}, listOf(50))

	cl.mu.Lock()
	most := cl.most
	cl.mu.Unlock()
	if most > 4 {
		t.Errorf("%d addresses were asked about at once, want no more than the four it was told", most)
	}
	if most < 2 {
		t.Errorf("only %d address was asked about at a time, so nothing was asked at once and this "+
			"test proves nothing", most)
	}
}

func TestListCheck_TakesItsSampleFromAcrossTheWholeList(t *testing.T) {
	// The first hundred of a list are the same hundred every time, and a list
	// whose first hundred are dead is a list somebody would stop using on the
	// strength of a hundred addresses out of fifteen thousand.
	cl := &askedCheck{}
	var check listCheck
	ranCheck(t, &check, cl, listCheckAsk{Profile: profileWithHop(""), Sample: 10, Threads: 5}, listOf(1000))

	asked := cl.addressesAsked()
	if len(asked) != 10 {
		t.Fatalf("%d addresses were asked about, want ten", len(asked))
	}
	fromTheTop := 0
	for _, one := range asked {
		for i := 0; i < 10; i++ {
			if strings.HasSuffix(one, fmt.Sprintf(":%d", 1000+i)) {
				fromTheTop++
			}
		}
	}
	if fromTheTop == len(asked) {
		t.Errorf("all ten came from the first ten of a thousand: %v", asked)
	}
}

func TestListCheck_CountsWhatAnsweredAndWhatDidNot(t *testing.T) {
	// Three outcomes and they are not the same thing: an address that answered,
	// one the service reached and was refused by, and one the service would not
	// answer about at all. Folded together, a service that had stopped talking
	// would read as a list that had stopped working.
	cl := &askedCheck{answer: func(eg blanktrail.Egress, _ blanktrail.FirstHop) (bool, error) {
		switch {
		case strings.HasSuffix(eg.Upstream, ":1000"):
			return false, fmt.Errorf("the service said nothing")
		case strings.HasSuffix(eg.Upstream, ":1001"), strings.HasSuffix(eg.Upstream, ":1002"):
			return false, nil
		}
		return true, nil
	}}
	var check listCheck
	got := ranCheck(t, &check, cl, listCheckAsk{Profile: profileWithHop(""), Sample: 6, Threads: 6}, listOf(6))

	if got.Direct.OK != 3 || got.Direct.Refused != 2 || got.Direct.Broke != 1 {
		t.Errorf("the reading is %+v, want three answered, two refused and one that could not be asked about",
			got.Direct)
	}
	if got.Direct.Share != 50 {
		t.Errorf("the share reads %d%%, want the half that answered", got.Direct.Share)
	}
}

func TestListCheck_SaysSoWhenTheListWouldNotLoad(t *testing.T) {
	// A check that reported nothing after a press would read as a check that
	// found nothing, which is the opposite of what happened.
	cl := &askedCheck{}
	var check listCheck
	got := ranCheck(t, &check, cl, listCheckAsk{Profile: profileWithHop(""), Sample: 5, Threads: 2},
		func(context.Context) ([]blanktrail.Egress, error) {
			return nil, fmt.Errorf("the list is not there")
		})

	if got.Fault != "proxies.check.list" {
		t.Errorf("the reading says %q, want the list being unreadable", got.Fault)
	}
	if got.Running {
		t.Error("the check is still marked as going on after it ended")
	}
}

func TestListCheck_LeavesACheckAlreadyGoingAlone(t *testing.T) {
	// Two checks at once are two loads on one service reported as one reading,
	// and the second would put the first's counters back to nought under the
	// reader's eye.
	cl := &askedCheck{hold: 50 * time.Millisecond}
	var check listCheck
	done := make(chan struct{})
	check.start(context.Background(), cl, listCheckAsk{Profile: profileWithHop(""), Sample: 8, Threads: 1},
		listOf(20), func() { close(done) })
	check.start(context.Background(), cl, listCheckAsk{Profile: profileWithHop(""), Sample: 8, Threads: 1},
		listOf(20), nil)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the check did not finish")
	}

	cl.mu.Lock()
	asked := len(cl.asked)
	cl.mu.Unlock()
	if asked != 8 {
		t.Errorf("%d addresses were asked about, want the eight of the one check that was going", asked)
	}
}

func TestProxies_DrawsACheckOnlyUnderTheProfileItWasMadeFor(t *testing.T) {
	// One check runs at a time and it is about one profile's addresses. Drawn
	// under another profile's boxes it would be this screen reporting one
	// list's addresses as another's, which is the mistake the counters above it
	// were already taught not to make.
	s, _ := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	mine := onlyProfile(t, s)
	other, err := s.store.CreateProfile(t.Context(), store.Profile{Name: "somewhere else", Kind: "url",
		Location: "http://example.invalid/other"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	cl := &askedCheck{}
	ranCheck(t, &s.checking, cl, listCheckAsk{
		Profile: store.Profile{ID: mine, Name: "mine"}, Sample: 3, Threads: 1}, listOf(10))

	if body := getBody(t, s, boxesOf(mine)); !strings.Contains(body, `id="check-direct"`) {
		t.Error("the profile the check was made for does not draw it")
	}
	if body := getBody(t, s, boxesOf(other)); strings.Contains(body, `id="check-direct"`) {
		t.Error("another profile's boxes draw a check that was made about this one's addresses")
	}
}

func TestProxies_OffersTheCheckOnAProfileNobodyHasCheckedYet(t *testing.T) {
	// The button is what the screen is for here. A page that only grew one
	// after the first check would be a page nobody could make a first check
	// from.
	s, _ := serverWithSettings(t, settings.Settings{ControlURL: "http://127.0.0.1:1"})
	body := getBody(t, s, boxesOf(onlyProfile(t, s)))

	for _, want := range []string{`action="/proxies/check"`, `name="sample"`, `name="checkthreads"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the profile screen has no %s", want)
		}
	}
	if strings.Contains(body, `id="check-direct"`) {
		t.Error("the screen draws a reading of a check nobody has made")
	}
}

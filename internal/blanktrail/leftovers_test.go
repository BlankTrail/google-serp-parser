// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
)

func TestClient_LabelsItsPortsWithTheInstallationAndTheProcess(t *testing.T) {
	t.Cleanup(func() { SetOwner("") })
	SetOwner("inst1")
	a, _ := NewClient("http://127.0.0.1:1", "k")
	time.Sleep(5 * time.Millisecond) // a job's client is made long after the standing set's
	b, _ := NewClient("http://127.0.0.1:1", "k")
	if !strings.HasPrefix(a.Label(), "gserp-inst1-") {
		t.Errorf("label %q, want the installation in it", a.Label())
	}
	if a.Label() != b.Label() {
		t.Errorf("two clients of one process label %q and %q: a job's ports would be taken for leftovers of the standing set's", a.Label(), b.Label())
	}
	if OwnerOf("host", "/a/gserp.db") == OwnerOf("host", "/b/gserp.db") || OwnerOf("host", "/a/gserp.db") != OwnerOf("host", "/a/gserp.db") {
		t.Error("OwnerOf does not tell two histories apart, or names one history two ways")
	}
}

func TestClient_ClosesWhatAnEarlierRunOfItsInstallationLeftAndNothingElse(t *testing.T) {
	// A run killed for an update closes nothing, and a standing port is never
	// closed for idling: ten ports of the run before stood open after the
	// update of 2026-10-10.
	t.Cleanup(func() { SetOwner("") })
	SetOwner("inst1")
	fake := fakebt.New(t)
	p, _ := ownedPool(t, fake, 1)
	current := fake.OpenPorts()[0]
	left := map[int]string{
		61001: "gserp-inst1-0000000000000000", // an earlier run of this installation
		61002: "gserp-inst2-0000000000000000", // another parser on the same service
		61003: "gserp-0123456789abcdef",       // a version that named no installation
		61004: "",                             // somebody else's
		61005: "gserp-inst1x-000000000000000", // a lookalike installation name
	}
	for port, label := range left {
		fake.TakePort(port)
		fake.SetLabel(port, label)
	}

	n, err := p.cl.ReclaimLeftovers(context.Background())
	if err != nil {
		t.Fatalf("ReclaimLeftovers: %v", err)
	}
	open := fake.OpenPorts()
	if n != 1 || slices.Contains(open, 61001) {
		t.Errorf("closed %d, and 61001 open %v; want the earlier run's one port closed", n, slices.Contains(open, 61001))
	}
	for _, kept := range []int{current, 61002, 61003, 61004, 61005} {
		if !slices.Contains(open, kept) {
			t.Errorf("port %d (%q) was closed; it is not an earlier run's of this installation", kept, left[kept])
		}
	}
}

func TestClient_ReclaimsNothingWhereNoInstallationWasNamed(t *testing.T) {
	SetOwner("")
	fake := fakebt.New(t)
	p, _ := ownedPool(t, fake, 1)
	fake.TakePort(61001)
	fake.SetLabel(61001, "gserp-0000000000000000")
	if n, err := p.cl.ReclaimLeftovers(context.Background()); n != 0 || err != nil {
		t.Errorf("ReclaimLeftovers = %d, %v with no installation named; want nothing done", n, err)
	}
}

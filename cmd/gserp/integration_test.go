// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/testutil/fakebt"
	"github.com/blanktrail/google-serp-parser/internal/version"
)

// saidNothing is a log that keeps what it was told, so a test can see that a
// failure was reported rather than swallowed.
type saidNothing struct{ lines []string }

func (s *saidNothing) Info(msg string, _ ...any) { s.lines = append(s.lines, msg) }

// stampedOn builds a client against a fake service.
func stampedOn(t *testing.T) (*fakebt.Server, *blanktrail.Client) {
	t.Helper()
	f := fakebt.New(t)
	c, err := blanktrail.NewClient(f.URL(), f.Key())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return f, c
}

func TestStampIntegration_TellsTheServiceWhoseWorkBroughtTheUser(t *testing.T) {
	// The key travels in the body of the service's own licence requests rather
	// than in any link, so stamping it into the service is the whole of what
	// this program can do about attribution — and it has to actually do it.
	f, c := stampedOn(t)
	version.SetIntegrationKeyForTest(t, "dk_this_program")

	stampIntegration(context.Background(), c, &saidNothing{})
	if got := f.IntegrationKey(); got != "dk_this_program" {
		t.Errorf("the service carries %q, want this program's key", got)
	}
}

func TestStampIntegration_LeavesAnotherIntegratorsKeyAlone(t *testing.T) {
	// Somebody else's credit. Taking it because this program happened to start
	// later would be theft with extra steps.
	f, c := stampedOn(t)
	f.SetIntegrationKey("dk_somebody_else")
	version.SetIntegrationKeyForTest(t, "dk_this_program")

	stampIntegration(context.Background(), c, &saidNothing{})
	if got := f.IntegrationKey(); got != "dk_somebody_else" {
		t.Errorf("the service carries %q: another integrator's key was written over", got)
	}
}

func TestStampIntegration_AsksTheServiceNothingFromABuildThatCarriesNoKey(t *testing.T) {
	// An unstamped source tree has nothing to say, so it should not say it. The
	// test is that the service is never spoken to at all: reading the key back
	// would prove nothing, because leaving another integrator's key alone would
	// leave the same empty slot untouched for a different reason.
	f, c := stampedOn(t)
	version.SetIntegrationKeyForTest(t, "")

	stampIntegration(context.Background(), c, &saidNothing{})
	for _, r := range f.Requests() {
		if strings.Contains(r.Path, "integration-key") {
			t.Fatalf("a build with no key of its own sent %s %s", r.Method, r.Path)
		}
	}
}

func TestStampIntegration_SaysSoAndCarriesOnWhenTheServiceRefuses(t *testing.T) {
	// None of this has anything to do with parsing, and a program that refused
	// to work over its own attribution would deserve none.
	f := fakebt.New(t)
	c, err := blanktrail.NewClient(f.URL(), "the-wrong-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	version.SetIntegrationKeyForTest(t, "dk_this_program")

	log := &saidNothing{}
	stampIntegration(context.Background(), c, log)
	if len(log.lines) == 0 {
		t.Error("a service that refused the request was passed over in silence")
	}
	if got := f.IntegrationKey(); got != "" {
		t.Errorf("the service carries %q after a refused request", got)
	}
}

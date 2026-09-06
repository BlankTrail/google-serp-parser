// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// aProfile is a profile with every field filled in with a different value, so a
// pair written into each other's column shows up as a swap rather than as two
// equal numbers that pass.
func aProfile(name string) Profile {
	return Profile{
		Name:               name,
		Kind:               "url",
		Location:           "https://example.com/proxies.txt",
		Refresh:            30 * time.Minute,
		Ban:                time.Hour,
		ThreadsPerUpstream: 3,
		RenewEvery:         10 * time.Minute,
		Protocol:           "http",
		Gateways:           []string{"nl-one", "de-two"},
	}
}

func TestCreateProfile_ReadsBackEverythingItWasGiven(t *testing.T) {
	// Every field, because a profile is the whole of what a job runs through: a
	// column that quietly did not survive the round trip is a setting the
	// operator set and the run did not use.
	s := testStore(t)
	ctx := context.Background()

	id, err := s.CreateProfile(ctx, aProfile("datacentre"))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	got, err := s.Profile(ctx, id)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	want := aProfile("datacentre")
	want.ID, want.Default = id, true // the first one written is the default one
	if got.Name != want.Name || got.Kind != want.Kind || got.Location != want.Location ||
		got.Refresh != want.Refresh || got.Ban != want.Ban ||
		got.ThreadsPerUpstream != want.ThreadsPerUpstream || got.RenewEvery != want.RenewEvery ||
		got.Protocol != want.Protocol || got.Default != want.Default {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	if len(got.Gateways) != 2 || got.Gateways[0] != "nl-one" || got.Gateways[1] != "de-two" {
		t.Errorf("gateways read back as %q, want the two it was given in order", got.Gateways)
	}
}

func TestCreateProfile_MakesTheFirstOneTheDefaultWhateverItSays(t *testing.T) {
	// A database with profiles and no default is one where nothing knows what to
	// keep warm, what the API's own search goes through, or what a job that
	// named no profile runs on. So the first one is it, asked for or not.
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateProfile(ctx, Profile{Name: "first"}); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	def, err := s.DefaultProfile(ctx)
	if err != nil {
		t.Fatalf("DefaultProfile: %v", err)
	}
	if def.Name != "first" {
		t.Errorf("the default profile is %q, want the only one there is", def.Name)
	}
}

func TestProfiles_KeepExactlyOneDefaultHoweverTheMarkIsMoved(t *testing.T) {
	// Two defaults would give three questions two answers each. The database
	// refuses it rather than leaving the program to pick, so this asks whether
	// the mark actually moves rather than piles up.
	s := testStore(t)
	ctx := context.Background()

	first, err := s.CreateProfile(ctx, Profile{Name: "first"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	second, err := s.CreateProfile(ctx, Profile{Name: "second", Default: true})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if def, _ := s.DefaultProfile(ctx); def.ID != second {
		t.Errorf("the default is %d, want the one that asked for it (%d)", def.ID, second)
	}
	if err := s.SetDefaultProfile(ctx, first); err != nil {
		t.Fatalf("SetDefaultProfile: %v", err)
	}
	if def, _ := s.DefaultProfile(ctx); def.ID != first {
		t.Errorf("the default is %d, want %d after the mark was moved", def.ID, first)
	}
	all, err := s.Profiles(ctx)
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	marked := 0
	for _, p := range all {
		if p.Default {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("%d of %d profiles are marked default, want exactly one", marked, len(all))
	}
	if len(all) > 0 && !all[0].Default {
		t.Error("the listing does not put the default profile first")
	}
}

func TestSaveProfile_RefusesANameAnotherProfileAlreadyCarries(t *testing.T) {
	// The name is how a profile is picked out of a list and how a job says which
	// one it runs on. Two of them called the same thing is a choice the reader
	// cannot make.
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateProfile(ctx, Profile{Name: "datacentre"}); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if _, err := s.CreateProfile(ctx, Profile{Name: "datacentre"}); !errors.Is(err, ErrProfileName) {
		t.Errorf("a repeated name gave %v, want ErrProfileName", err)
	}
	if _, err := s.CreateProfile(ctx, Profile{Name: "   "}); !errors.Is(err, ErrProfileName) {
		t.Errorf("a blank name gave %v, want ErrProfileName", err)
	}
}

func TestDeleteProfile_KeepsTheLastOneAndPassesTheDefaultOn(t *testing.T) {
	// Something has to be default, so the last profile stays and the mark moves
	// when the profile carrying it goes.
	s := testStore(t)
	ctx := context.Background()

	only, err := s.CreateProfile(ctx, Profile{Name: "only"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := s.DeleteProfile(ctx, only); !errors.Is(err, ErrLastProfile) {
		t.Errorf("deleting the last profile gave %v, want ErrLastProfile", err)
	}
	other, err := s.CreateProfile(ctx, Profile{Name: "other"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := s.DeleteProfile(ctx, only); err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	def, err := s.DefaultProfile(ctx)
	if err != nil {
		t.Fatalf("DefaultProfile after deleting the one that was default: %v", err)
	}
	if def.ID != other {
		t.Errorf("the default is %d, want the one left standing (%d)", def.ID, other)
	}
}

func TestProfileFor_FallsBackToTheDefaultRatherThanRefusing(t *testing.T) {
	// A job whose profile was deleted still has queries waiting. Refusing to
	// start it would be worse than starting it on the exits everything else is
	// using, which is also what it did before profiles existed.
	s := testStore(t)
	ctx := context.Background()

	def, err := s.CreateProfile(ctx, Profile{Name: "default"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	for _, named := range []int64{0, 9999} {
		got, err := s.ProfileFor(ctx, named)
		if err != nil {
			t.Fatalf("ProfileFor(%d): %v", named, err)
		}
		if got.ID != def {
			t.Errorf("a job naming %d runs on profile %d, want the default (%d)", named, got.ID, def)
		}
	}
}

func TestJobs_CarryTheProfileThroughBothDoorsTheyAreWrittenBy(t *testing.T) {
	// A job is written by two calls with the same column list — CreateJob for a
	// list held in memory and OpenPlan for one streamed from a file — and the
	// browser only ever uses the second. A column added to one and forgotten in
	// the other is a job silently run through the wrong exits, and nothing on
	// any screen would say so.
	s := testStore(t)
	ctx := context.Background()

	id, err := s.CreateProfile(ctx, aProfile("chosen"))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	held, err := s.CreateJob(ctx, JobSpec{Name: "held", Pages: 1, ProfileID: id}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	plan, err := s.OpenPlan(ctx, JobSpec{Name: "streamed", Pages: 1, ProfileID: id})
	if err != nil {
		t.Fatalf("OpenPlan: %v", err)
	}
	if err := plan.Add(ctx, "a"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := plan.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	for _, job := range []struct {
		door string
		id   int64
	}{{"CreateJob", held}, {"OpenPlan", plan.JobID()}} {
		sum, err := s.Progress(ctx, job.id)
		if err != nil {
			t.Fatalf("Progress after %s: %v", job.door, err)
		}
		if sum.ProfileID != id {
			t.Errorf("a job written by %s runs on profile %d, want %d", job.door, sum.ProfileID, id)
		}
	}
}

func TestSetJobProfile_PointsAJobSomewhereElse(t *testing.T) {
	// A job that has not started, or one that was stopped, can be pointed at
	// another set of exits and taken up again — which is the thing profiles are
	// for on a job that is already written down.
	s := testStore(t)
	ctx := context.Background()

	first, err := s.CreateProfile(ctx, Profile{Name: "first"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	second, err := s.CreateProfile(ctx, Profile{Name: "second"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	job, err := s.CreateJob(ctx, JobSpec{Name: "nightly", Pages: 1, ProfileID: first}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobProfile(ctx, job, second); err != nil {
		t.Fatalf("SetJobProfile: %v", err)
	}
	sum, err := s.Progress(ctx, job)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if sum.ProfileID != second {
		t.Errorf("the job runs on profile %d, want %d", sum.ProfileID, second)
	}
	if err := s.SetJobProfile(ctx, 9999, second); !errors.Is(err, ErrNoJob) {
		t.Errorf("pointing a job that is not there gave %v, want ErrNoJob", err)
	}
}

func TestCarryProxySettings_WritesOneProfileAndThenLeavesItAlone(t *testing.T) {
	// A machine that has been running since before profiles existed keeps its
	// exits in the settings file. This carries them over on one start and does
	// nothing on every start after it — including after the operator has edited
	// the profile, which is the case that matters: carrying twice would put the
	// old settings back over their work.
	s := testStore(t)
	ctx := context.Background()

	made, err := s.CarryProxySettings(ctx, aProfile("Default"))
	if err != nil {
		t.Fatalf("CarryProxySettings: %v", err)
	}
	if !made {
		t.Fatal("nothing was carried into an empty database")
	}
	carried, err := s.DefaultProfile(ctx)
	if err != nil {
		t.Fatalf("DefaultProfile: %v", err)
	}
	if carried.Location != "https://example.com/proxies.txt" || carried.Ban != time.Hour {
		t.Errorf("carried %+v, want the settings it was given", carried)
	}

	edited := carried
	edited.Location = "https://example.com/better.txt"
	if err := s.SaveProfile(ctx, edited); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	again, err := s.CarryProxySettings(ctx, aProfile("Default"))
	if err != nil {
		t.Fatalf("CarryProxySettings a second time: %v", err)
	}
	if again {
		t.Error("the settings were carried a second time, over what the operator had edited")
	}
	after, err := s.DefaultProfile(ctx)
	if err != nil {
		t.Fatalf("DefaultProfile: %v", err)
	}
	if after.Location != "https://example.com/better.txt" {
		t.Errorf("the profile reads %q, want the edit to have survived the second start", after.Location)
	}
	all, err := s.Profiles(ctx)
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("%d profiles after two starts, want one", len(all))
	}

	// And after it has been renamed, which is the case the name alone cannot
	// answer: the guard is that there are profiles at all, not that this one is
	// still called what it was called when it was carried. Without that, every
	// start after a rename would leave another Default behind it.
	renamed := after
	renamed.Name = "datacentre"
	if err := s.SaveProfile(ctx, renamed); err != nil {
		t.Fatalf("SaveProfile after renaming: %v", err)
	}
	if _, err := s.CarryProxySettings(ctx, aProfile("Default")); err != nil {
		t.Fatalf("CarryProxySettings after renaming: %v", err)
	}
	all, err = s.Profiles(ctx)
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("%d profiles after the carried one was renamed, want one: a start after a rename carried the settings again", len(all))
	}
}

func TestProfile_IsEmptyWhenItNamesNoWayOut(t *testing.T) {
	// A profile that names nothing sends every request from the machine the
	// operator is sitting at. That is the state a fresh install is in — the
	// first profile is written out of settings that named nothing — and it is
	// worth telling apart from a profile that is set up, because everything else
	// about the two looks the same.
	//
	// Each source is asked what it needs and nothing else: a list is a path or an
	// address, and the gateways live in the service, so what makes that one empty
	// is naming none of them.
	for _, one := range []struct {
		said  string
		p     Profile
		empty bool
	}{
		{"a profile with nothing in it", Profile{Name: "Default"}, true},
		{"a file with no path", Profile{Name: "a", Kind: "file"}, true},
		{"a file", Profile{Name: "a", Kind: "file", Location: "proxies.txt"}, false},
		{"an address with nothing at it", Profile{Name: "a", Kind: "url", Location: "  "}, true},
		{"an address", Profile{Name: "a", Kind: "url", Location: "http://list.example/p"}, false},
		{"the gateways, none of them chosen", Profile{Name: "a", Kind: "gateways"}, true},
		{"the gateways", Profile{Name: "a", Kind: "gateways", Gateways: []string{"one"}}, false},
		// A path left behind under the gateways is not a way out: what that
		// source uses is the list of names beside it, and nothing else.
		{"the gateways, with an old path still in the box",
			Profile{Name: "a", Kind: "gateways", Location: "proxies.txt"}, true},
	} {
		if got := one.p.Empty(); got != one.empty {
			t.Errorf("%s reads as empty=%v, want %v", one.said, got, one.empty)
		}
	}
}

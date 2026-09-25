// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestAddChecks_AddsEveryRunsChecksToTheJob(t *testing.T) {
	// How many of Google's checks a job paid for — the captchas the solver
	// solved for it — was known only while it ran, and gone with the run. It is
	// written down at the end of every run and adds up over a job's runs, so two
	// jobs run at different rests can be compared once both are done.
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateJob(ctx, JobSpec{Name: "nightly", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if sum, err := s.Progress(ctx, id); err != nil || sum.ChecksMet != 0 {
		t.Fatalf("a job nothing has run on reads %d checks (%v), want none", sum.ChecksMet, err)
	}
	for _, n := range []int{3, 5, 0} {
		if err := s.AddChecks(ctx, id, n); err != nil {
			t.Fatalf("AddChecks(%d): %v", n, err)
		}
	}
	if sum, err := s.Progress(ctx, id); err != nil || sum.ChecksMet != 8 {
		t.Errorf("after runs of three, five and none the job reads %d checks (%v), want eight", sum.ChecksMet, err)
	}
	listed, err := s.Jobs(ctx, 10)
	if err != nil || len(listed) != 1 || listed[0].ChecksMet != 8 {
		t.Errorf("the listing reads %+v (%v), want the job with eight checks", listed, err)
	}
	if err := s.AddChecks(ctx, id+100, 1); !errors.Is(err, ErrNoJob) {
		t.Errorf("checks added to a job that is not there gave %v, want ErrNoJob", err)
	}
}

func TestOpen_CountsNoChecksForAJobWrittenBeforeTheCountWasKept(t *testing.T) {
	// A job run before the count was kept reads nought after the upgrade: what
	// it cost was never written down, and nought is what there is to show.
	path := filepath.Join(t.TempDir(), "gserp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, err := s.CreateJob(t.Context(), JobSpec{Name: "before", Pages: 1}, []string{"a"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	windBackToVersion27(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("opening a version-27 database: %v", err)
	}
	defer func() { _ = again.Close() }()
	if sum, err := again.Progress(t.Context(), id); err != nil || sum.ChecksMet != 0 {
		t.Errorf("a job from before the count reads %d checks (%v), want none", sum.ChecksMet, err)
	}
	if err := again.AddChecks(t.Context(), id, 4); err != nil {
		t.Fatalf("AddChecks: %v", err)
	}
	if sum, _ := again.Progress(t.Context(), id); sum.ChecksMet != 4 {
		t.Errorf("after the upgrade the job counts %d checks, want the four added", sum.ChecksMet)
	}
}

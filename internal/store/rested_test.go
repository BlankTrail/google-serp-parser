// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"
)

func TestRestedAndRest_CarryABenchAcrossARestart(t *testing.T) {
	// The whole point of writing a rest down: a program restarted without it
	// walks straight back into every address the day before spent its time
	// learning to avoid, and on a list where most are dead that is most of the
	// first hour.
	s := testStore(t)
	at := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)

	if err := s.Rest(context.Background(), "socks5|10.0.0.1:1080", at); err != nil {
		t.Fatalf("Rest: %v", err)
	}
	got, err := s.Rested(context.Background())
	if err != nil {
		t.Fatalf("Rested: %v", err)
	}
	if len(got) != 1 || !got["socks5|10.0.0.1:1080"].Equal(at) {
		t.Errorf("the bench reads back as %v, want the one address at %v", got, at)
	}

	// Found dead again later: the rest is counted from the fresher finding, or
	// an address that keeps failing would come back on the strength of the first
	// time it did.
	later := at.Add(2 * time.Hour)
	if err := s.Rest(context.Background(), "socks5|10.0.0.1:1080", later); err != nil {
		t.Fatalf("Rest again: %v", err)
	}
	got, err = s.Rested(context.Background())
	if err != nil {
		t.Fatalf("Rested: %v", err)
	}
	if !got["socks5|10.0.0.1:1080"].Equal(later) {
		t.Errorf("the rest began at %v, want the later finding %v",
			got["socks5|10.0.0.1:1080"], later)
	}
}

func TestForgetRestsBefore_KeepsTheTableFromGrowingForever(t *testing.T) {
	// Fifteen thousand addresses cycled through for a month would otherwise
	// leave a row for every one that ever failed, long after its rest ran out.
	s := testStore(t)
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	for key, when := range map[string]time.Time{
		"socks5|10.0.0.1:1080": old,
		"socks5|10.0.0.2:1080": recent,
	} {
		if err := s.Rest(context.Background(), key, when); err != nil {
			t.Fatalf("Rest: %v", err)
		}
	}

	if err := s.ForgetRestsBefore(context.Background(), recent.Add(-time.Hour)); err != nil {
		t.Fatalf("ForgetRestsBefore: %v", err)
	}
	got, err := s.Rested(context.Background())
	if err != nil {
		t.Fatalf("Rested: %v", err)
	}
	if _, still := got["socks5|10.0.0.1:1080"]; still {
		t.Error("a rest that began before the cut is still on the bench")
	}
	if _, kept := got["socks5|10.0.0.2:1080"]; !kept {
		t.Error("a rest that began after the cut was forgotten with the old ones")
	}
}

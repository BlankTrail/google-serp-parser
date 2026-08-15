// SPDX-License-Identifier: MIT

package blanktrail

import (
	"errors"
	"strings"
	"testing"
)

func countByName(names []string) map[string]int {
	out := map[string]int{}
	for _, n := range names {
		out[n]++
	}
	return out
}

func TestPlanSpecs_NoSpecsMeansOneUnnamedTemplate(t *testing.T) {
	got, err := planSpecs(nil, 4)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d names, want 4", len(got))
	}
	for i, n := range got {
		if n != "" {
			t.Errorf("names[%d]=%q, want the empty name so PoolConfig.Spec applies", i, n)
		}
	}
}

func TestPlanSpecs_EqualWeightsSplitEvenly(t *testing.T) {
	specs := []NamedSpec{{Name: "desktop"}, {Name: "mobile"}}
	got, err := planSpecs(specs, 8)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	counts := countByName(got)
	if counts["desktop"] != 4 || counts["mobile"] != 4 {
		t.Errorf("counts=%v, want 4 desktop and 4 mobile", counts)
	}
}

func TestPlanSpecs_InterleavesSoAPartialOpenIsStillAMix(t *testing.T) {
	// Ports are opened in this order. If opening fails half way through, what
	// did open must still cover every template — otherwise a run that dies
	// early silently measures one device only.
	specs := []NamedSpec{{Name: "desktop"}, {Name: "mobile"}}
	got, err := planSpecs(specs, 8)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	if got[0] == got[1] {
		t.Errorf("names=%v, want consecutive ports to carry different templates", got)
	}
	first4 := countByName(got[:4])
	if first4["desktop"] == 0 || first4["mobile"] == 0 {
		t.Errorf("first four = %v, want both templates present", got[:4])
	}
}

func TestPlanSpecs_WeightsShareOutTheRemainder(t *testing.T) {
	specs := []NamedSpec{{Name: "desktop", Weight: 3}, {Name: "mobile", Weight: 1}}
	got, err := planSpecs(specs, 8)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	counts := countByName(got)
	if counts["desktop"] != 6 || counts["mobile"] != 2 {
		t.Errorf("counts=%v, want 6 desktop and 2 mobile", counts)
	}
}

func TestPlanSpecs_EveryTemplateGetsAPortEvenWhenWeightIsTiny(t *testing.T) {
	// Straight proportional rounding would hand mobile zero ports here, and the
	// job would report desktop numbers under a mobile label. Every named
	// template gets one port before weight is considered.
	specs := []NamedSpec{{Name: "desktop", Weight: 99}, {Name: "mobile", Weight: 1}}
	got, err := planSpecs(specs, 2)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	counts := countByName(got)
	if counts["desktop"] != 1 || counts["mobile"] != 1 {
		t.Errorf("counts=%v, want one port each", counts)
	}
}

func TestPlanSpecs_TotalAlwaysMatchesTheRequestedSize(t *testing.T) {
	specs := []NamedSpec{{Name: "a", Weight: 5}, {Name: "b", Weight: 3}, {Name: "c", Weight: 2}}
	for size := 3; size <= 40; size++ {
		got, err := planSpecs(specs, size)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if len(got) != size {
			t.Fatalf("size %d: got %d names", size, len(got))
		}
		counts := countByName(got)
		for _, s := range specs {
			if counts[s.Name] < 1 {
				t.Errorf("size %d: template %q got no port (counts=%v)", size, s.Name, counts)
			}
		}
	}
}

func TestPlanSpecs_RefusesAPoolSmallerThanTheTemplateCount(t *testing.T) {
	specs := []NamedSpec{{Name: "desktop"}, {Name: "mobile"}}
	_, err := planSpecs(specs, 1)
	if !errors.Is(err, ErrTooFewPorts) {
		t.Fatalf("err=%v, want ErrTooFewPorts", err)
	}
	// The message has to name both numbers: the operator's next action is to
	// raise one of them, and a bare error name does not say which.
	if !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "2") {
		t.Errorf("err=%q, want it to state both the port count and the template count", err)
	}
}

func TestPlanSpecs_RejectsUnusableTemplateSets(t *testing.T) {
	cases := []struct {
		name  string
		specs []NamedSpec
		want  string
	}{
		{"empty name", []NamedSpec{{Name: ""}, {Name: "mobile"}}, "empty name"},
		{"blank name", []NamedSpec{{Name: "   "}, {Name: "mobile"}}, "empty name"},
		{"duplicate", []NamedSpec{{Name: "desktop"}, {Name: "desktop"}}, "duplicate"},
		{"negative weight", []NamedSpec{{Name: "desktop", Weight: -1}}, "negative weight"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planSpecs(tc.specs, 4)
			if err == nil {
				t.Fatalf("planSpecs accepted %v, want an error", tc.specs)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestPlanSpecs_IsDeterministic(t *testing.T) {
	specs := []NamedSpec{{Name: "a", Weight: 5}, {Name: "b", Weight: 3}}
	first, err := planSpecs(specs, 11)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := planSpecs(specs, 11)
		if err != nil {
			t.Fatalf("planSpecs: %v", err)
		}
		if strings.Join(again, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d gave %v, first run gave %v", i, again, first)
		}
	}
}

func TestSpecByName_FallsBackToTheZeroSpec(t *testing.T) {
	mobile := PortSpec{Browser: "chrome", OS: "android"}
	specs := []NamedSpec{{Name: "mobile", Spec: mobile}}
	if got := specByName(specs, "mobile"); got.OS != "android" {
		t.Errorf("specByName(mobile).OS=%q, want android", got.OS)
	}
	if got := specByName(specs, "nope"); got.OS != "" {
		t.Errorf("specByName on an unknown name returned %+v, want the zero spec", got)
	}
}

func TestPlanSpecs_EqualWeightsBreakTiesInDeclarationOrder(t *testing.T) {
	// Three equal templates over five ports: two of them must get the spare
	// ports, and it has to be the first two as declared. Ties broken by map
	// order or by an unstable sort would reshuffle the pool between runs on an
	// unchanged configuration, which makes two runs incomparable.
	specs := []NamedSpec{{Name: "a", Weight: 1}, {Name: "b", Weight: 1}, {Name: "c", Weight: 1}}
	got, err := planSpecs(specs, 5)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	counts := countByName(got)
	if counts["a"] != 2 || counts["b"] != 2 || counts["c"] != 1 {
		t.Errorf("counts=%v, want a=2 b=2 c=1 (spares go to the earliest declared)", counts)
	}
}

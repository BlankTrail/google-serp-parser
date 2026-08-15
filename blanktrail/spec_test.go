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

// spreadMatrix counts, per channel, how many ports of each template it carries.
func spreadMatrix(specNames []string, chanCounts []int) []map[string]int {
	out := make([]map[string]int, len(chanCounts))
	for i := range out {
		out[i] = map[string]int{}
	}
	for port, ch := range spreadSpecs(specNames, chanCounts) {
		out[ch][specNames[port]]++
	}
	return out
}

func TestSpreadSpecs_PutsEveryTemplateOnEveryChannel(t *testing.T) {
	// Two channels, two templates, four ports: the case the whole product turns
	// on. Both channels must see both device profiles, or the comparison
	// between them is a comparison of IPs.
	names, err := planSpecs([]NamedSpec{{Name: "desktop"}, {Name: "mobile"}}, 4)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	got := spreadMatrix(names, []int{2, 2})
	for c, row := range got {
		if row["desktop"] != 1 || row["mobile"] != 1 {
			t.Errorf("channel %d carries %v, want one of each (matrix=%v)", c, row, got)
		}
	}
}

func TestSpreadSpecs_SpreadsARareTemplateWideningTheWeights(t *testing.T) {
	// Six desktop and two mobile ports over two equal channels. The two mobile
	// ports are the entire mobile measurement; both on one channel makes every
	// mobile number a property of that one IP.
	names, err := planSpecs([]NamedSpec{{Name: "desktop", Weight: 3}, {Name: "mobile", Weight: 1}}, 8)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	got := spreadMatrix(names, []int{4, 4})
	for c, row := range got {
		if row["mobile"] != 1 || row["desktop"] != 3 {
			t.Errorf("channel %d carries %v, want 3 desktop and 1 mobile (matrix=%v)", c, row, got)
		}
	}
}

func TestSpreadSpecs_SpreadsARareTemplateAcrossUnequalChannels(t *testing.T) {
	// A penalised channel holds fewer ports, but "fewer" is not "none of the
	// rare template": four ports on one channel and two on the other still
	// leaves room for a mobile port on each.
	names, err := planSpecs([]NamedSpec{{Name: "desktop", Weight: 2}, {Name: "mobile", Weight: 1}}, 6)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	got := spreadMatrix(names, []int{4, 2})
	if got[0]["mobile"] == 0 || got[1]["mobile"] == 0 {
		t.Errorf("matrix=%v, want the mobile ports split over both channels", got)
	}
}

func TestSpreadSpecs_SingleChannelTakesEverything(t *testing.T) {
	names, err := planSpecs([]NamedSpec{{Name: "desktop"}, {Name: "mobile"}}, 8)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	for port, ch := range spreadSpecs(names, []int{8}) {
		if ch != 0 {
			t.Fatalf("port %d went to channel %d, want the only channel", port, ch)
		}
	}
}

func TestSpreadSpecs_KeepsBothMarginalsAndStaysCloseToTheGlobalMix(t *testing.T) {
	// The contract in one test: the mixer's per-channel port counts survive
	// untouched, every template keeps the number of ports planSpecs gave it, and
	// no channel's share of a template is off the global mix by a whole port.
	specSets := [][]NamedSpec{
		{{Name: "desktop"}, {Name: "mobile"}},
		{{Name: "desktop", Weight: 3}, {Name: "mobile", Weight: 1}},
		{{Name: "a", Weight: 5}, {Name: "b", Weight: 3}, {Name: "c", Weight: 2}},
		{{Name: "a", Weight: 9}, {Name: "b"}},
	}
	chanShapes := [][]int{{1, 1}, {2, 1}, {4, 1}, {1, 1, 1}, {3, 2, 1}}

	for _, specs := range specSets {
		for _, shape := range chanShapes {
			for size := len(specs); size <= 24; size++ {
				names, err := planSpecs(specs, size)
				if err != nil {
					t.Fatalf("planSpecs(%v, %d): %v", specs, size, err)
				}
				chanCounts := splitEvenly(size, shape)
				if len(chanCounts) == 0 {
					continue // more channels than ports; the mixer would not build this
				}
				got := spreadMatrix(names, chanCounts)

				wantSpec := countByName(names)
				gotSpec := map[string]int{}
				for c, row := range got {
					total := 0
					for name, n := range row {
						total += n
						gotSpec[name] += n
					}
					if total != chanCounts[c] {
						t.Fatalf("size %d shape %v: channel %d holds %d ports, want %d (matrix=%v)",
							size, shape, c, total, chanCounts[c], got)
					}
					for name, want := range wantSpec {
						// n/chanCounts[c] must be within one port of want/size.
						lo := want * chanCounts[c]
						if diff := row[name]*size - lo; diff >= size || diff <= -size {
							t.Fatalf("size %d shape %v: channel %d carries %d of %q, want within a port of %.2f (matrix=%v)",
								size, shape, c, row[name], name, float64(lo)/float64(size), got)
						}
					}
				}
				for name, want := range wantSpec {
					if gotSpec[name] != want {
						t.Fatalf("size %d shape %v: %q got %d ports, want %d (matrix=%v)",
							size, shape, name, gotSpec[name], want, got)
					}
				}
			}
		}
	}
}

// splitEvenly shares size out over channels weighted by shape, the way the
// mixer's assignment does. It returns nil when a channel would get no port.
func splitEvenly(size int, shape []int) []int {
	total := 0
	for _, w := range shape {
		total += w
	}
	out := make([]int, len(shape))
	handed := 0
	for i, w := range shape {
		out[i] = size * w / total
		handed += out[i]
	}
	for i := 0; handed < size; i = (i + 1) % len(out) {
		out[i]++
		handed++
	}
	for _, n := range out {
		if n == 0 {
			return nil
		}
	}
	return out
}

func TestSpreadSpecs_IsDeterministic(t *testing.T) {
	names, err := planSpecs([]NamedSpec{{Name: "a", Weight: 5}, {Name: "b", Weight: 3}, {Name: "c"}}, 17)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	first := spreadSpecs(names, []int{7, 6, 4})
	for i := 0; i < 20; i++ {
		again := spreadSpecs(names, []int{7, 6, 4})
		for port := range first {
			if again[port] != first[port] {
				t.Fatalf("run %d differs at port %d: %v vs %v", i, port, again, first)
			}
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

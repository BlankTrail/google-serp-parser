// SPDX-License-Identifier: MIT

package blanktrail

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrTooFewPorts is returned when the pool would be smaller than the number of
// named templates, so at least one template would get no port at all. Rounding
// a template away silently would mean a job that asked for two device profiles
// quietly measuring one of them and labelling it with both.
var ErrTooFewPorts = errors.New("blanktrail: fewer ports than named specs")

// NamedSpec is one fingerprint template in a pool that holds several. Ports are
// opened under each template and AcquireSpec asks for one by name.
//
// A template exists because the fingerprint is fixed when the port is OPENED,
// not when a request is sent: a pool of one template can only ever speak with
// one device identity, and comparing two identities in a single run is the
// ordinary case, not an exotic one.
//
// PoolConfig.Specs documents how many ports each template receives.
type NamedSpec struct {
	// Name identifies the template. It must be non-empty and unique in the pool.
	Name string

	// Spec is the template ports under this name are opened with.
	//
	// Either leave it entirely zero — NewPool then opens these ports with
	// DefaultPortSpec() — or fill it in completely, starting from
	// DefaultPortSpec() and changing only what differs. A half-filled spec is
	// refused at startup rather than completed: PortSpec's booleans cannot tell
	// "unset" from "deliberately false", so there is no honest field-wise merge,
	// and quietly substituting DefaultPortSpec() would open a Windows desktop
	// port under a template named "mobile" and report it as mobile everywhere.
	Spec PortSpec

	// Weight is this template's share of the ports that remain once every
	// template has been guaranteed one. Zero and one mean the same thing, one
	// share: a template left at zero beside one weighted 5 gets one sixth of the
	// remainder, not half of it.
	Weight int
}

// validateSpecs checks the templates are usable as a set. It is deliberately
// strict: every one of these mistakes produces a pool that runs and returns
// plausible numbers under the wrong label.
func validateSpecs(specs []NamedSpec) error {
	seen := make(map[string]bool, len(specs))
	for i, s := range specs {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return fmt.Errorf("blanktrail: Specs[%d] has an empty name", i)
		}
		if seen[name] {
			return fmt.Errorf("blanktrail: duplicate spec name %q", name)
		}
		seen[name] = true
		if s.Weight < 0 {
			return fmt.Errorf("blanktrail: spec %q has a negative weight", name)
		}
	}
	return nil
}

// planSpecs assigns a template name to each of size ports.
//
// Every named template gets one port before weight is considered; the remainder
// is shared out by weight with largest-remainder allocation. The result is
// interleaved so consecutive ports carry different templates: when opening
// fails part way through, what did open is still a mix rather than all of one
// kind.
//
// An empty specs slice yields size empty names, which is how a pool with a
// single unnamed template — PoolConfig.Spec — is expressed.
func planSpecs(specs []NamedSpec, size int) ([]string, error) {
	if size < 1 {
		return nil, errors.New("blanktrail: pool size must be at least 1")
	}
	if len(specs) == 0 {
		return make([]string, size), nil
	}
	if err := validateSpecs(specs); err != nil {
		return nil, err
	}
	if size < len(specs) {
		return nil, fmt.Errorf("%w: %d ports for %d specs", ErrTooFewPorts, size, len(specs))
	}

	counts := make([]int, len(specs))
	weights := make([]int, len(specs))
	totalWeight := 0
	for i, s := range specs {
		counts[i] = 1
		weights[i] = s.Weight
		if weights[i] <= 0 {
			weights[i] = 1
		}
		totalWeight += weights[i]
	}

	// Largest-remainder allocation of what is left after the guaranteed port.
	rest := size - len(specs)
	type share struct{ idx, remainder int }
	shares := make([]share, len(specs))
	handed := 0
	for i := range specs {
		exact := rest * weights[i]
		whole := exact / totalWeight
		counts[i] += whole
		handed += whole
		shares[i] = share{idx: i, remainder: exact % totalWeight}
	}
	// SliceStable keeps equal remainders in declaration order, so the plan does
	// not drift between runs on an otherwise identical configuration.
	sort.SliceStable(shares, func(a, b int) bool {
		return shares[a].remainder > shares[b].remainder
	})
	for i := 0; i < rest-handed; i++ {
		counts[shares[i].idx]++
	}

	out := make([]string, 0, size)
	left := append([]int(nil), counts...)
	for len(out) < size {
		for i, s := range specs {
			if left[i] == 0 {
				continue
			}
			out = append(out, s.Name)
			left[i]--
			if len(out) == size {
				break
			}
		}
	}
	return out, nil
}

// spreadSpecs decides which channel each port egresses through, given the
// template plan and how many ports each channel is owed.
//
// specNames is planSpecs' output, one template name per port in opening order.
// chanCounts[c] is how many ports channel c must end up with; the counts come
// from Mixer.Assign, so channel proportionality is settled before this function
// sees them and is preserved exactly. The result is one channel index per port.
//
// This exists because pairing the two plans by index confounds the templates
// with the egresses. Both sequences cycle — the channel plan with the length of
// the mixer's slot list, the template plan with the number of templates — so
// they lock step: two proxies and two templates over four ports put every
// desktop port on proxy A and every mobile port on proxy B. A run then reports
// "desktop rank 4, mobile rank 7" as a device difference when it may be a
// difference of geography, ASN or IP reputation, and nothing in the numbers
// says which. That is the product's headline comparison, silently confounded.
//
// So the channel is not read off a parallel sequence: each template is shared
// out over the channels in its own right, by largest remainder, in proportion
// to the ports each channel still has free. Allocating against the free
// capacity rather than the original weight is what makes it self-correcting —
// a channel that took more than its share of one template has less room for
// the next — and it makes the final per-channel totals land exactly on
// chanCounts, because by the last template each channel's free capacity is
// precisely what it has left to receive.
//
// Deterministic throughout: no map is iterated, ties fall to the lower channel
// index, and templates are processed in the order planSpecs emitted them.
func spreadSpecs(specNames []string, chanCounts []int) []int {
	out := make([]int, len(specNames))
	if len(chanCounts) <= 1 || len(specNames) == 0 {
		return out // one channel: everything goes there and there is nothing to spread
	}

	// Templates in first-appearance order, which is planSpecs' declaration
	// order. The map is only ever looked up in, never ranged over.
	var names []string
	var counts []int
	index := make(map[string]int, len(chanCounts))
	for _, n := range specNames {
		i, ok := index[n]
		if !ok {
			i = len(names)
			index[n] = i
			names = append(names, n)
			counts = append(counts, 0)
		}
		counts[i]++
	}

	// quota[c][t] is how many ports of template t channel c must take.
	quota := make([][]int, len(chanCounts))
	for c := range quota {
		quota[c] = make([]int, len(names))
	}
	free := append([]int(nil), chanCounts...)
	left := len(specNames)

	type remainder struct{ ch, rem int }
	rems := make([]remainder, len(free))
	for t := range names {
		want := counts[t]
		given := 0
		for c, f := range free {
			exact := want * f // a fraction over left, kept in integers
			quota[c][t] = exact / left
			given += quota[c][t]
			rems[c] = remainder{ch: c, rem: exact % left}
		}
		// SliceStable with a strict comparison leaves equal remainders in
		// channel order, so an unchanged configuration lays out the same way on
		// every run and two runs stay comparable.
		sort.SliceStable(rems, func(a, b int) bool { return rems[a].rem > rems[b].rem })
		for given < want {
			progress := false
			for _, r := range rems {
				if given == want {
					break
				}
				if quota[r.ch][t] >= free[r.ch] {
					continue // this channel is already full; the next one takes it
				}
				quota[r.ch][t]++
				given++
				progress = true
			}
			if !progress {
				break // unreachable: the free capacity always covers what is left
			}
		}
		for c := range free {
			free[c] -= quota[c][t]
		}
		left -= want
	}

	// Hand the quotas out along the opening order, moving the cursor on after
	// every port so consecutive ports still prefer different channels: a pool
	// that dies half way through opening is then spread over its egresses as
	// well as over its templates.
	cursor := 0
	for i, n := range specNames {
		t := index[n]
		for k := 0; k < len(chanCounts); k++ {
			c := (cursor + k) % len(chanCounts)
			if quota[c][t] > 0 {
				quota[c][t]--
				out[i] = c
				cursor = c + 1
				break
			}
		}
	}
	return out
}

// specByName returns the template stored under name, or the zero PortSpec when
// there is none.
func specByName(specs []NamedSpec, name string) PortSpec {
	for _, s := range specs {
		if s.Name == name {
			return s.Spec
		}
	}
	return PortSpec{}
}

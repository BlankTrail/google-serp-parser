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

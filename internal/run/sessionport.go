// SPDX-License-Identifier: MIT

package run

import (
	"context"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/sessions"
)

// leasePort is a leased port as the keeper of sessions sees it. The lease has
// every call the keeper makes; this only turns the service's word for a
// fingerprint into the keeper's, and fills in the release from the template the
// port was opened under — the service names the browser and the system of a
// fingerprint, and the release is what the template narrowed it to.
type leasePort struct{ *blanktrail.Lease }

var _ sessions.Port = leasePort{}

func (p leasePort) Number() int { return p.Port() }

func (p leasePort) Freshen(ctx context.Context) (sessions.Fingerprint, error) {
	prof, err := p.Lease.Freshen(ctx)
	if err != nil {
		return sessions.Fingerprint{}, err
	}
	worn := p.Worn()
	fp := sessions.Fingerprint{Profile: prof.Name, Browser: prof.Browser, OS: prof.OS, Release: worn.Release}
	if fp.Browser == "" {
		fp.Browser = worn.Browser
	}
	if fp.OS == "" {
		fp.OS = worn.OS
	}
	return fp, nil
}

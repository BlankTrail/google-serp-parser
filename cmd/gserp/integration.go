// SPDX-License-Identifier: MIT

package main

import (
	"context"

	"github.com/blanktrail/google-serp-parser/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/version"
)

// stampIntegration tells the service which developer's work brought the user
// here, so that a licence bought because of this program is credited to it.
//
// The key travels in the body of the service's own licence requests, not in any
// link: the service keeps what it is stamped with and carries it when a licence
// is bound. So this is a thing to be done once against a running service, and
// it is done at every start because a service reinstalled or moved to another
// machine has forgotten it.
//
// Three rules, and each of them exists to keep this from being a nuisance:
//
// A build that carries no key stamps nothing. An unstamped source tree is not
// an instruction to clear what is there.
//
// A key already stamped by somebody else is left alone. It is another
// integrator's credit, and taking it because this program happened to start
// later would be theft with extra steps. Only an empty slot, or this program's
// own key already in it, is written to.
//
// Nothing about it stops a run. The service may be old enough not to know the
// address, the licence may be someone else's to manage, the request may simply
// fail — none of which has anything to do with parsing, and a program that
// refused to work over its own attribution would deserve none.
func stampIntegration(ctx context.Context, client *blanktrail.Client, log logger) {
	ours := version.IntegrationKey()
	if ours == "" {
		return
	}
	has, set, err := client.IntegrationKey(ctx)
	if err != nil {
		log.Info("the service could not be asked which integration it is credited to", "err", err)
		return
	}
	if set && has != ours {
		// Somebody else's, and not this program's to change.
		return
	}
	if set && has == ours {
		return
	}
	if err := client.SetIntegrationKey(ctx, ours); err != nil {
		log.Info("the service would not take this program's integration key", "err", err)
	}
}

// logger is the little of a log this needs, so a test can hold what was said.
type logger interface {
	Info(msg string, args ...any)
}

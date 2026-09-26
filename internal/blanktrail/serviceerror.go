// SPDX-License-Identifier: MIT

package blanktrail

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// serviceErrorHeader is the header the proxy puts on an answer it composed
// itself, saying why it could not carry the request.
//
// It matters because without it the two are the same event. A request the
// service could not carry used to come back as a closed connection with nothing
// on it, which is exactly what a dead address looks like — so a run spent its
// list punishing addresses for roads that were never theirs: measured live, 212
// of 275 requests ended that way in two and a half minutes, and a list of
// fifteen thousand lost over a thousand addresses to the bench in an hour.
const serviceErrorHeader = "X-BlankTrail-Error"

// upstreamDetailHeader is where the service says, in its own words, what it met
// on the way to the address when it could not carry a request.
const upstreamDetailHeader = "X-BlankTrail-Upstream-Detail"

// ErrResumeDefect is the service's own TLS failing to resume a session with the
// far end: its uTLS cannot reprocess a resumed session's key when the server
// answers with a HelloRetryRequest, and it says so as an address it could not
// reach. The address answered. On job 20 of 2026-09-26 this was 1263 of the
// 2359 such answers the lookup ports got, every one of them blamed on an
// address that was alive; the defect is in the service and is being mended
// there. Until it is, the address keeps its standing, and the port's own TLS
// state is what wants clearing: every attempt through it carries the same
// ticket.
var ErrResumeDefect = errors.New("blanktrail: the service could not resume its TLS session; the address is not to blame")

// resumeDefectDetail is the service's words for it.
const resumeDefectDetail = "reprocessing of PSK"

// ErrUpstreamUnreachable is the service saying it could not reach the address
// this port stands on. Nothing went out, so nothing about it is the session's
// or the query's — what it is about is the address.
var ErrUpstreamUnreachable = errors.New("blanktrail: the service could not reach the address")

// ErrServiceRefused is the service saying it could not carry the request for
// some other reason of its own. The address is not blamed: what the reason is
// about is the service, and answering it by leaving the address would spend a
// list on something that was never in it.
var ErrServiceRefused = errors.New("blanktrail: the service did not carry the request")

// ErrSolverWorking is a challenge still being solved when the request ran out
// of patience. The port is the warm one — the solve pins itself to it — so the
// thing to do is ask again through the same port rather than anywhere else.
var ErrSolverWorking = errors.New("blanktrail: the challenge is still being solved")

// ErrSolverBusy is the solver with nothing free to give this request. Asking
// again at once asks the same thing of the same queue.
var ErrSolverBusy = errors.New("blanktrail: the challenge solver has nothing free")

// ErrChainUnreachable is the first hop being unreachable. Every address is
// behind it, so this is not one address's failure and walking the list for it
// would spend the whole of it on one road being down.
var ErrChainUnreachable = errors.New("blanktrail: the service could not reach the first hop")

// The words the service uses, each with a different thing to do about it.
const (
	// unreachableReason is an address that did not answer. The next address is
	// the answer.
	unreachableReason = "upstream_unreachable"
	// chainUnreachableReason is the road itself being down. Every address is
	// behind it, so taking the next one buys nothing and spends the list.
	chainUnreachableReason = "chain_unreachable"
	// solverWorkingReason is a challenge the solver did not finish inside this
	// request. It goes on working and pins the port, so the same port asked
	// again usually walks straight through — and a caller that leaves throws
	// away the wait it has already paid for.
	solverWorkingReason = "solver_timeout"
	// solverBusyReason is no attempt at all: the queue is full, no window is
	// free, or the solver is off. Nothing is wrong with the address and nothing
	// is gained by asking faster.
	solverBusyReason = "solver_capacity"
	// solverFailedReason is a browser that tried and did not clear the
	// challenge. That one is about where the request is going out from.
	solverFailedReason = "solver_failed"
)

// ServiceError is one of those answers, with the word the service used.
type ServiceError struct {
	// Reason is the machine-readable word from the header.
	Reason string
	// Detail is what the service met on the way, where it said.
	Detail string
	// Status is what the answer came with, for a reader taking it apart.
	Status int
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("blanktrail: the service did not carry the request (%s, HTTP %d)", e.Reason, e.Status)
}

// Is makes a service error match the two sentinels above, so a caller can ask
// "was this the address" without knowing the vocabulary.
func (e *ServiceError) Is(target error) bool {
	switch target {
	case ErrUpstreamUnreachable:
		return e.Reason == unreachableReason && !e.resumeDefect()
	case ErrResumeDefect:
		return e.resumeDefect()
	case ErrChainUnreachable:
		return e.Reason == chainUnreachableReason
	case ErrSolverWorking:
		return e.Reason == solverWorkingReason
	case ErrSolverBusy:
		return e.Reason == solverBusyReason
	case ErrServiceRefused:
		return true
	}
	return false
}

// serviceRefusal reads an answer the service composed about itself, and says
// whether this was one.
//
// Only the header decides. A status alone cannot: 523 from the service is its
// own answer, and 523 from something at the far end is a page — and on a list
// where most requests fail, guessing wrong either way is thousands of addresses
// blamed or thousands of refusals mistaken for pages.
// ChallengeUnsolved says the service's own browser met Google's check on the
// way and could not clear it.
//
// What stood behind such an answer is the page a check leaves a client that
// will not run its script — the JavaScript shell — and a caller that reads
// pages already has a name for that. The service used to hand the shell back
// as the page; it now says so in words of its own, and without this a shell it
// named would read as a request that never reached Google at all: the address
// kept for nothing, the session carried off it as if the road had failed, and
// a phrase that did reach Google left as one that never had.
func (e *ServiceError) ChallengeUnsolved() bool { return e.Reason == solverFailedReason }

// resumeDefect says the answer is the service's TLS defect rather than the
// address: see ErrResumeDefect.
func (e *ServiceError) resumeDefect() bool {
	return e.Reason == unreachableReason && strings.Contains(e.Detail, resumeDefectDetail)
}

func serviceRefusal(resp *http.Response) (*ServiceError, bool) {
	if resp == nil {
		return nil, false
	}
	reason := strings.TrimSpace(resp.Header.Get(serviceErrorHeader))
	if reason == "" {
		return nil, false
	}
	return &ServiceError{Reason: reason, Detail: resp.Header.Get(upstreamDetailHeader), Status: resp.StatusCode}, true
}

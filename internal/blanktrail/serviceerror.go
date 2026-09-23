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

// ErrUpstreamUnreachable is the service saying it could not reach the address
// this port stands on. Nothing went out, so nothing about it is the session's
// or the query's — what it is about is the address.
var ErrUpstreamUnreachable = errors.New("blanktrail: the service could not reach the address")

// ErrServiceRefused is the service saying it could not carry the request for
// some other reason of its own. The address is not blamed: what the reason is
// about is the service, and answering it by leaving the address would spend a
// list on something that was never in it.
var ErrServiceRefused = errors.New("blanktrail: the service did not carry the request")

// unreachableReason is what the service calls an address it could not reach.
const unreachableReason = "upstream_unreachable"

// ServiceError is one of those answers, with the word the service used.
type ServiceError struct {
	// Reason is the machine-readable word from the header.
	Reason string
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
		return e.Reason == unreachableReason
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
func serviceRefusal(resp *http.Response) (*ServiceError, bool) {
	if resp == nil {
		return nil, false
	}
	reason := strings.TrimSpace(resp.Header.Get(serviceErrorHeader))
	if reason == "" {
		return nil, false
	}
	return &ServiceError{Reason: reason, Status: resp.StatusCode}, true
}

// SPDX-License-Identifier: MIT

package blanktrail

import (
	"errors"
	"io"
	"net"
	"net/http"
)

// Unreachable says an error from the control API is the service not being there
// — a connection refused, reset or cut off, a request that timed out, or the
// service answering that it is not ready yet (502, 503, 504) — rather than the
// service refusing what it was asked.
//
// It is what a restart of the service looks like from here, and an update of it
// is a restart. Such a failure is the service's and it passes: it is waited out,
// and held against nothing — not against a port, which was never asked anything,
// and not against a session, which never reached Google. Held against them it
// took every port of a pool into quarantine and every session of a history to
// its second failure in a row within the minute the service was away.
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	var api *APIError
	if errors.As(err, &api) {
		switch api.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// The request never had an answer: the client wraps what went wrong on the
	// way — a dial refused, a reset, a deadline — in a url.Error, which is a
	// net.Error; an answer cut short is an EOF.
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

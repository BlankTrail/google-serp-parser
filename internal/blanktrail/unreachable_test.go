// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnreachable_TellsTheServiceBeingAwayFromItRefusing(t *testing.T) {
	// A service that is restarting refuses the connection or says it is not
	// ready; a service that is up and refuses what was asked says so with a
	// status of its own. Only the first passes by itself.
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	c, err := NewClient(url, "k")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, refused := c.PortProfile(context.Background(), 20000)
	if !Unreachable(refused) {
		t.Errorf("a connection refused is not taken for the service being away: %v", refused)
	}

	for _, c := range []struct {
		status int
		want   bool
	}{
		{http.StatusServiceUnavailable, true},
		{http.StatusBadGateway, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusNotFound, false},
		{http.StatusConflict, false},
		{http.StatusBadRequest, false},
	} {
		err := error(&APIError{Status: c.status, Path: "/api/v1/port/20000/config"})
		if got := Unreachable(err); got != c.want {
			t.Errorf("HTTP %d: Unreachable = %v, want %v", c.status, got, c.want)
		}
	}
	if Unreachable(errors.New("blanktrail: port 20000 wears \"x\" after being asked for \"y\"")) {
		t.Error("a refusal of what was asked is taken for the service being away")
	}
	if Unreachable(nil) {
		t.Error("no error is taken for the service being away")
	}
}

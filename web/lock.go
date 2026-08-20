// SPDX-License-Identifier: MIT

package web

import (
	"net"
	"net/http"

	"github.com/blanktrail/google-serp-parser/settings"
)

// Locked wraps a handler so that a request arriving from anywhere but this
// machine has to carry the password.
//
// The pages carry no key of their own: they were written for a program
// listening on loopback, where whoever can reach the port is already sitting at
// the machine. Opened to a network they are the settings, the queue and
// everything every job has collected, offered to whoever asks — so the moment
// the interface answers the network, it asks for a password.
//
// Loopback is let through without one. Someone at the keyboard has the settings
// file, the history and the program itself; a password would be a lock on a
// door they are already inside, and one they would have to type every time they
// glanced at a job.
//
// Basic authentication is what asks. It is the one kind a browser handles by
// itself — a prompt, remembered for the session, no login page, no cookie, and
// nothing this program has to store. What it does not do is hide the password
// on the way: on plain HTTP it travels in every request, readable by anything
// between. That is a fact about the arrangement, not a defect that can be
// mended here, and it is what the note beside the switch says.
func Locked(next http.Handler, hash string) http.Handler {
	if hash == "" {
		// Nothing to check against. A guard that let everybody through would be
		// worse than none: it would read as protection.
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromThisMachine(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok || !settings.PasswordMatches(hash, password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="gserp", charset="UTF-8"`)
			http.Error(w, "a password is needed to reach this from another machine",
				http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fromThisMachine says whether a request came from the machine the program runs
// on.
//
// The address is read rather than trusted from a header: X-Forwarded-For and
// its relatives are written by whoever is asking, so a guard that read them
// would be a guard anybody could walk through by writing 127.0.0.1 into one.
func fromThisMachine(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

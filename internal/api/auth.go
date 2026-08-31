// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/blanktrail/google-serp-parser/internal/store"
)

// bearerWord is the one authorization scheme this interface knows.
const bearerWord = "bearer"

// keyParam is the query parameter a key may also arrive in.
const keyParam = "api_key"

// refused is what every failed key check says, whatever failed about it.
//
// A key that was never issued, a key that was revoked this morning and no key
// at all answer with this same sentence. The difference between them is worth
// something only to somebody working through keys one at a time, and telling
// them apart tells that person which of their guesses was once real.
const refused = "unauthorized"

// authed puts the key check in front of a handler.
//
// A key is taken two ways. Authorization: Bearer <key> is the right one, and
// the one to use where there is a choice. ?api_key=<key> is the way another
// service takes it, which makes it the way somebody else's code already sends
// it, and accepting it is the reason this interface exists: their program
// should work after changing one address and nothing else.
//
// The second way has a cost, and it is paid by whoever uses it. A key in a
// query string is part of the address, and addresses are what intermediaries
// write down: every proxy, gateway and load balancer between the caller and
// this program keeps that line, in its access log, in the clear, for as long as
// its logs are kept. A header is written down by none of them. A caller who can
// set headers should set the header, and a key that has travelled in an address
// should be treated as one that has been read.
//
// Whether the key is good is a question for the history, which holds hashes and
// knows what has been revoked. Nothing here compares secrets itself.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := bearerKey(r.Header.Get("Authorization"))
		if secret == "" {
			// Only the query is read. Asking the request for a form value would
			// consume the body on a post, and the body is what the handler
			// behind this check was written to read.
			secret = r.URL.Query().Get(keyParam)
		}

		if _, err := s.store.CheckKey(r.Context(), secret); err != nil {
			if !errors.Is(err, store.ErrBadKey) {
				// The key may well be good and the history unreadable. Saying
				// so would be a fault on this side reported as the caller's, so
				// the operator is told instead.
				s.log.Error("a key could not be checked", "path", r.URL.Path, "error", err)
				writeError(w, http.StatusInternalServerError, "the server could not check the key")
				return
			}
			writeError(w, http.StatusUnauthorized, refused)
			return
		}
		next(w, r)
	}
}

// bearerKey reads the secret out of an Authorization header, and returns an
// empty string for anything that is not this scheme.
//
// The word is compared without regard to case because the standard that defines
// it says it carries none, and clients spell it every way there is. What
// follows must be separated from it by a space: a header whose first word runs
// into the key names no scheme at all, and taking a key out of it would accept
// something nobody meant to send.
func bearerKey(header string) string {
	word, rest, split := strings.Cut(header, " ")
	if !split || !strings.EqualFold(word, bearerWord) {
		return ""
	}
	return strings.TrimSpace(rest)
}

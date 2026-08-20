// SPDX-License-Identifier: MIT

package settings

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrNoPassword says a password was asked for and none was given.
var ErrNoPassword = errors.New("settings: the interface is open to the network and no password is set")

// lockIterations is how much work one check of a password costs.
//
// A password is chosen by a person, so there is a dictionary to try and the
// only defence is making each guess expensive. This is not the count used for
// an API key, which is thirty-two random bytes with no dictionary behind it and
// is hashed once cheaply — the two are different problems and get different
// answers.
//
// Six hundred thousand is what OWASP asks for PBKDF2-HMAC-SHA256. It costs a
// fraction of a second on the machine doing the asking, which nobody notices
// once a browser has been told the password, and a great deal of somebody
// else's time.
const lockIterations = 600_000

// saltBytes is how much salt each password gets, so that two machines with the
// same password do not have the same hash and neither can be looked up in a
// table built in advance.
const saltBytes = 16

// LockPassword turns a password into what may be written down: the salt and the
// derived key, hex, separated by a dollar. The password itself is never stored
// and cannot be read back out of this.
func LockPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", ErrNoPassword
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("settings: drawing a salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, lockIterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("settings: hashing the password: %w", err)
	}
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(key), nil
}

// PasswordMatches says whether the password is the one behind the stored hash.
//
// A stored value it cannot read is not a match. That is the safe way round: a
// settings file damaged into something unparseable locks the interface rather
// than opening it.
func PasswordMatches(stored, password string) bool {
	saltHex, keyHex, ok := strings.Cut(stored, "$")
	if !ok {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := hex.DecodeString(keyHex)
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, lockIterations, len(want))
	if err != nil {
		return false
	}
	// Compared in constant time: a comparison that stops at the first wrong byte
	// tells whoever is guessing how much of their guess was right.
	return subtle.ConstantTimeCompare(got, want) == 1
}

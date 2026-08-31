// SPDX-License-Identifier: MIT

package settings

import (
	"strings"
	"testing"
)

func TestLockPassword_CannotBeReadBackAndMatchesOnlyItself(t *testing.T) {
	// What is written down is a salt and a derived key. The password is not in
	// it, and a settings file somebody photographs does not hand it over.
	const password = "correct horse battery staple"
	hash, err := LockPassword(password)
	if err != nil {
		t.Fatalf("LockPassword: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatal("the stored value contains the password itself")
	}
	if !PasswordMatches(hash, password) {
		t.Error("the password does not match what was made from it")
	}
	if PasswordMatches(hash, password+" ") {
		t.Error("a different password matched")
	}
	if PasswordMatches(hash, "") {
		t.Error("an empty password matched")
	}

	// Two hashes of one password differ, so the same password on two machines
	// cannot be recognised as the same, nor looked up in a table built ahead of
	// time.
	again, err := LockPassword(password)
	if err != nil {
		t.Fatalf("LockPassword: %v", err)
	}
	if again == hash {
		t.Error("hashing the same password twice gave the same value, so it carries no salt")
	}
	if !PasswordMatches(again, password) {
		t.Error("the second hash does not match the password it was made from")
	}
}

func TestLockPassword_RefusesNothing(t *testing.T) {
	// A blank password is not a password, and hashing one would put a lock on
	// the interface that anybody opens by pressing enter.
	for _, blank := range []string{"", " ", "\t\n"} {
		if _, err := LockPassword(blank); err == nil {
			t.Errorf("a password of %q was accepted", blank)
		}
	}
}

func TestPasswordMatches_TreatsWhatItCannotReadAsNoMatch(t *testing.T) {
	// A settings file damaged into something unparseable has to lock the
	// interface rather than open it.
	for _, stored := range []string{"", "nonsense", "$", "zz$zz", "aabb$", "$aabb"} {
		if PasswordMatches(stored, "anything") {
			t.Errorf("a stored value of %q let a password through", stored)
		}
	}
}

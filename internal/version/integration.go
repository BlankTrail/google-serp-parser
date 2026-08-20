// SPDX-License-Identifier: MIT

package version

// integrationKey names this program to BlankTrail's cabinet, so that a licence
// bought because of it is credited to whoever wrote it.
//
// It is not a secret and never was: it names an integrator rather than a
// person, it is the same in every copy of this program, and anybody who reads
// the source or the binary can see it. What it must be is right — the service
// takes it as it is given, and a wrong one credits nobody.
//
// It is a variable rather than a constant so that a build can be stamped with
// another one:
//
//	go build -ldflags "-X …/internal/version.integrationKey=dk_something"
//
// Empty is a build that says nothing, which is what an unstamped source tree
// is: the program then leaves whatever the service already carries alone.
var integrationKey = ""

// IntegrationKey is the developer's key this build carries, or the empty string
// when it carries none.
func IntegrationKey() string { return integrationKey }

// SetIntegrationKeyForTest points this build's key at another value for the
// length of one test.
//
// It lives here rather than in a test file because the value it moves is
// unexported and belongs to this package: a test elsewhere has no other way to
// say "a build stamped with this key" without the key becoming settable by
// anything that imports this.
func SetIntegrationKeyForTest(t interface{ Cleanup(func()) }, key string) {
	was := integrationKey
	integrationKey = key
	t.Cleanup(func() { integrationKey = was })
}

// SPDX-License-Identifier: MIT

// Package version is the single place the build's identity lives, so a release
// changes one constant rather than a string scattered across the binary.
package version

// current is overridden at release time with -ldflags "-X ...version.current=1.2.3".
var current = "0.1.0-dev"

// Version reports the build's version string.
func Version() string { return current }

// UserAgent is what this program calls itself when it is not wearing a browser
// profile — control-API calls and update checks, never a request to a scraped
// origin, where the proxy owns the User-Agent instead.
func UserAgent() string {
	return "gserp/" + current + " (+https://github.com/blanktrail/google-serp-parser)"
}

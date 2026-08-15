// SPDX-License-Identifier: MIT

// Package blanktrail is a reusable client for the BlankTrail Proxy control API.
//
// It turns a running BlankTrail Proxy instance into a pool of local proxy ports.
// Each port emulates a real browser's TLS and HTTP/2 fingerprint, owns its own
// cookie jar, egresses through a chosen channel (a proxy from a list, a rotating
// proxy, a VPN gateway or the host's own IP), and solves JS challenges through
// BlankTrail's Challenge Breaker.
//
// A caller leases a port and sends ordinary Go HTTP requests through it. The
// leased *http.Client transparently retries, refreshes the fingerprint and
// rotates the egress IP when the target pushes back, and the pool guarantees a
// port is never handed out again before its cooldown has elapsed.
//
// Nothing in this package is specific to any target site.
package blanktrail

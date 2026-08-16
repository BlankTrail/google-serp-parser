// SPDX-License-Identifier: MIT

// Package google turns Google result pages into typed data.
//
// It knows nothing about proxies. It is handed an *http.Client and parses
// whatever that client brings back, which is what lets it be tested against
// captured pages without a network and reused behind any transport.
//
// Two things about Google shape everything here, and both were measured
// rather than assumed. A plain HTTP client is not served results at all — it
// gets a JavaScript shell with HTTP 200 and no results in it — so a response
// must be classified before it is parsed. And the form of a result's link
// varies from session to session, so all three known forms are supported and
// the one in play is decided per page at runtime.
package google

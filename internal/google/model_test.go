// SPDX-License-Identifier: MIT

package google

import "testing"

func TestResult_ResolvedDistinguishesADomainFromAnAddress(t *testing.T) {
	// Google serves some sessions an encrypted redirector, and then the exact
	// address is simply not in the page. A result that carries only a host is
	// a legitimate, useful result — but a caller must be able to tell it from
	// one that carries the real address, or a "URL" column silently becomes a
	// mixture of exact links and prettified guesses.
	domainOnly := Result{Host: "habr.com", DisplayPath: "articles", Form: LinkEncrypted}
	if domainOnly.Resolved() {
		t.Error("a result with no URL reports itself as resolved")
	}
	exact := Result{Host: "habr.com", URL: "https://habr.com/ru/articles/704090/", Form: LinkEncrypted}
	if !exact.Resolved() {
		t.Error("a result carrying a URL reports itself as unresolved")
	}
}

func TestSERP_HasTotalSeparatesZeroFromUnknown(t *testing.T) {
	// "Google found nothing" and "Google did not say how much it found" are
	// different answers, and an int64 alone cannot express both.
	var unknown SERP
	if unknown.HasTotal {
		t.Error("a zero SERP claims to know its total")
	}
	found := SERP{TotalResults: 0, HasTotal: true}
	if !found.HasTotal {
		t.Error("an explicit zero total was lost")
	}
}

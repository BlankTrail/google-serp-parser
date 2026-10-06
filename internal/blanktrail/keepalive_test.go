// SPDX-License-Identifier: MIT

package blanktrail

import "testing"

func TestBaseTransport_KeepsAnIdleConnectionForEveryLookupAPortCarries(t *testing.T) {
	// A port the hidden addresses are read through carries ten at once. Kept
	// alive, a transport holding fewer idle connections than that closes the
	// rest the moment their answers are in.
	rt := newBaseTransport("127.0.0.1", 20000, "socks5", nil, true, false)
	if rt.MaxIdleConnsPerHost < 10 || rt.MaxIdleConns < rt.MaxIdleConnsPerHost {
		t.Errorf("holds %d idle a host and %d in all, want ten or more of each",
			rt.MaxIdleConnsPerHost, rt.MaxIdleConns)
	}
	if rt.DisableKeepAlives {
		t.Error("a transport asked to keep alive does not")
	}
	if !newBaseTransport("127.0.0.1", 20000, "socks5", nil, true, true).DisableKeepAlives {
		t.Error("a transport asked not to keep alive does")
	}
}

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

func TestBaseTransport_ResumesTLSWithTheServiceFromACacheOfThePortsOwn(t *testing.T) {
	// A port that opens a connection a request resumes its TLS session with
	// the service rather than paying a full handshake for every page; and a
	// ticket one port was given is never shown to another.
	a := newBaseTransport("127.0.0.1", 20000, "socks5", nil, true, true)
	b := newBaseTransport("127.0.0.1", 20001, "socks5", nil, true, true)
	if a.TLSClientConfig.ClientSessionCache == nil {
		t.Fatal("a port keeps no TLS sessions to resume")
	}
	if a.TLSClientConfig.ClientSessionCache == b.TLSClientConfig.ClientSessionCache {
		t.Error("two ports share one cache of TLS sessions")
	}
}

// SPDX-License-Identifier: MIT

package fakebt

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
)

// Join stands in for one of the proxy's ports: it takes whatever the client
// speaks, joins it to the origin, and splices the two until either end goes.
//
// It answers both protocols a port may be opened as, chosen by what arrives
// rather than by what the test was told. A port opened for SOCKS5 and dialled
// as an HTTP proxy hangs — the client waits for a reply to a CONNECT nobody is
// reading, and the test times out three minutes later with nothing to say about
// why. Sniffing the first byte means the stand-in is never the thing that is
// wrong, and both paths stay covered by every test that goes through it.
//
// The target the client asks for is read and thrown away. The stand-in has one
// origin and the point is the tunnel, not the routing.
func Join(c net.Conn, originAddr string) {
	defer func() { _ = c.Close() }()

	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == socksVersion {
		joinSOCKS5(c, br, originAddr)
		return
	}
	joinConnect(c, br, originAddr)
}

// The parts of SOCKS5 this stand-in speaks. Everything else about the protocol
// is refused by silence, which is what a client meets from a port that is not
// listening for it either.
const (
	socksVersion  = 0x05
	socksNoAuth   = 0x00
	socksConnect  = 0x01
	socksIPv4     = 0x01
	socksDomain   = 0x03
	socksIPv6     = 0x04
	socksGranted  = 0x00
	socksReserved = 0x00
)

// joinSOCKS5 answers the greeting and the connect request, then splices.
func joinSOCKS5(c net.Conn, br *bufio.Reader, originAddr string) {
	// The greeting: version, how many methods follow, and the methods.
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil {
		return
	}
	if _, err := io.CopyN(io.Discard, br, int64(head[1])); err != nil {
		return
	}
	// No authentication, which is what a port on this machine wants.
	if _, err := c.Write([]byte{socksVersion, socksNoAuth}); err != nil {
		return
	}

	// The request: version, command, reserved, address type.
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return
	}
	if req[1] != socksConnect {
		return
	}
	if !skipAddress(br, req[3]) {
		return
	}

	up, err := net.Dial("tcp", originAddr)
	if err != nil {
		// The reply's own address is all zeroes: the client has no use for it,
		// and a stand-in that invented one would be saying something untrue.
		_, _ = c.Write([]byte{socksVersion, 0x01, socksReserved, socksIPv4, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = up.Close() }()
	if _, err := c.Write([]byte{socksVersion, socksGranted, socksReserved, socksIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	splice(c, br, up)
}

// skipAddress reads past the address and port of a SOCKS5 request and says
// whether it could.
func skipAddress(br *bufio.Reader, kind byte) bool {
	var n int64
	switch kind {
	case socksIPv4:
		n = net.IPv4len
	case socksIPv6:
		n = net.IPv6len
	case socksDomain:
		length, err := br.ReadByte()
		if err != nil {
			return false
		}
		n = int64(length)
	default:
		return false
	}
	if _, err := io.CopyN(io.Discard, br, n); err != nil {
		return false
	}
	// The port, which is read for the same reason as the address: so that what
	// follows on the wire is the tunnel and not the tail of the request.
	var port [2]byte
	if _, err := io.ReadFull(br, port[:]); err != nil {
		return false
	}
	_ = binary.BigEndian.Uint16(port[:])
	return true
}

// joinConnect answers an HTTP CONNECT, then splices.
func joinConnect(c net.Conn, br *bufio.Reader, originAddr string) {
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	up, err := net.Dial("tcp", originAddr)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = up.Close() }()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	splice(c, br, up)
}

// splice copies in both directions until either end goes. What the client has
// already sent is read from the buffer rather than the connection: a reader
// that had buffered the first bytes of the tunnel and was then dropped would
// lose them, and the origin would see a request with its opening cut off.
func splice(c net.Conn, br *bufio.Reader, up net.Conn) {
	go func() {
		_, _ = io.Copy(up, br)
		_ = up.Close()
	}()
	_, _ = io.Copy(c, up)
}

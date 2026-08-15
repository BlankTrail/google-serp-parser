// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Upstream is a single normalised upstream proxy.
type Upstream struct {
	Scheme string // http | https | socks5 | socks5h | socks4
	Host   string
	Port   string
	User   string
	Pass   string
}

// URL renders the upstream in the scheme://user:pass@host:port form the
// BlankTrail control API accepts.
func (u Upstream) URL() string {
	uu := url.URL{Scheme: u.Scheme, Host: net.JoinHostPort(u.Host, u.Port)}
	switch {
	case u.User != "" && u.Pass != "":
		uu.User = url.UserPassword(u.User, u.Pass)
	case u.User != "":
		uu.User = url.User(u.User)
	}
	return uu.String()
}

// Key identifies an upstream by scheme and address, ignoring credentials.
func (u Upstream) Key() string { return u.Scheme + "|" + net.JoinHostPort(u.Host, u.Port) }

var validSchemes = map[string]bool{
	"http": true, "https": true,
	"socks5": true, "socks5h": true, "socks": true, "socks4": true,
}

// Parse turns a raw proxy list (any popular line format) into normalised
// upstreams. It returns the parsed upstreams plus the raw text of any lines it
// could not understand, so a caller can report them. defaultScheme (default
// "socks5") is applied to lines that omit a scheme.
//
// Accepted per-line forms:
//
//	scheme://user:pass@host:port
//	scheme://host:port
//	user:pass@host:port
//	host:port
//	host:port:user:pass
//	[ipv6]:port (optionally with a scheme and/or user:pass@)
//
// Blank lines and comments (# // ;) are skipped.
func Parse(raw, defaultScheme string) (ups []Upstream, bad []string) {
	if defaultScheme == "" {
		defaultScheme = "socks5"
	}
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line == "" || isComment(line) {
			continue
		}
		u, err := parseLine(line, defaultScheme)
		if err != nil {
			bad = append(bad, line)
			continue
		}
		ups = append(ups, u)
	}
	return ups, bad
}

func isComment(line string) bool {
	return strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, ";")
}

func parseLine(line, defaultScheme string) (Upstream, error) {
	scheme := strings.ToLower(defaultScheme)
	rest := line
	if i := strings.Index(rest, "://"); i >= 0 {
		scheme = strings.ToLower(rest[:i])
		rest = rest[i+3:]
	}
	if scheme == "socks" {
		scheme = "socks5"
	}
	if !validSchemes[scheme] {
		return Upstream{}, fmt.Errorf("unsupported scheme %q", scheme)
	}

	// user:pass@host:port
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		user, pass := splitPair(rest[:at])
		host, port, err := splitHostPort(rest[at+1:])
		if err != nil {
			return Upstream{}, err
		}
		return newUpstream(scheme, host, port, user, pass)
	}

	// host:port
	if host, port, err := splitHostPort(rest); err == nil {
		return newUpstream(scheme, host, port, "", "")
	}

	// host:port:user:pass (bare colon form, IPv4/hostname only)
	if parts := strings.Split(rest, ":"); len(parts) == 4 {
		return newUpstream(scheme, parts[0], parts[1], parts[2], parts[3])
	}

	return Upstream{}, fmt.Errorf("cannot parse %q", line)
}

// splitHostPort accepts host:port and [ipv6]:port; it rejects anything with
// extra colons so the caller can fall back to the host:port:user:pass form.
func splitHostPort(s string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(s)
	if err != nil {
		return "", "", err
	}
	if host == "" || port == "" {
		return "", "", fmt.Errorf("empty host or port in %q", s)
	}
	return host, port, nil
}

func splitPair(s string) (a, b string) {
	if i := strings.Index(s, ":"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func newUpstream(scheme, host, port, user, pass string) (Upstream, error) {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return Upstream{}, fmt.Errorf("invalid port %q", port)
	}
	if strings.TrimSpace(host) == "" {
		return Upstream{}, fmt.Errorf("empty host")
	}
	return Upstream{Scheme: scheme, Host: host, Port: port, User: user, Pass: pass}, nil
}

// Source describes where the upstream list comes from and how often to reload it.
type Source struct {
	Kind          string        // "file" | "url"
	Location      string        // path or URL
	Refresh       time.Duration // reload interval (0 = never)
	DefaultScheme string        // scheme for lines without one (default "socks5")
}

// Load reads and parses the source once.
func (s Source) Load(ctx context.Context) (ups []Upstream, bad []string, err error) {
	var raw []byte
	switch strings.ToLower(s.Kind) {
	case "file", "":
		raw, err = os.ReadFile(s.Location)
	case "url":
		raw, err = httpGet(ctx, s.Location)
	default:
		return nil, nil, fmt.Errorf("blanktrail: unknown upstream source kind %q", s.Kind)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("blanktrail: load upstreams from %s %q: %w", s.Kind, s.Location, err)
	}
	ups, bad = Parse(string(raw), s.DefaultScheme)
	return ups, bad, nil
}

func httpGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// Rotor hands out upstreams round-robin from a live list, skips proxies that
// keep failing to connect, and (optionally) reloads the list on an interval
// while keeping its cursor position stable.
type Rotor struct {
	mu       sync.Mutex
	ups      []Upstream
	pos      int
	fails    map[string]int
	maxFails int

	src  Source
	stop chan struct{}
	done chan struct{}

	closeOnce sync.Once
}

// NewStaticRotor rotates over a fixed list with no background refresh.
func NewStaticRotor(ups []Upstream) *Rotor {
	return &Rotor{ups: ups, fails: map[string]int{}, maxFails: 3}
}

// NewRotor loads the source once and, if Source.Refresh > 0, starts a
// background reloader. Call Close to stop it.
func NewRotor(ctx context.Context, src Source) (*Rotor, error) {
	ups, _, err := src.Load(ctx)
	if err != nil {
		return nil, err
	}
	if len(ups) == 0 {
		return nil, fmt.Errorf("blanktrail: upstream source %q yielded no usable proxies", src.Location)
	}
	r := &Rotor{ups: ups, fails: map[string]int{}, maxFails: 3, src: src}
	if src.Refresh > 0 {
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
		go r.refreshLoop()
	}
	return r, nil
}

// Len reports the current number of upstreams.
func (r *Rotor) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ups)
}

// Next returns the next healthy upstream in round-robin order. The bool is
// false only when the list is empty.
func (r *Rotor) Next() (Upstream, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.ups)
	if n == 0 {
		return Upstream{}, false
	}
	for i := 0; i < n; i++ {
		u := r.ups[r.pos%n]
		r.pos = (r.pos + 1) % n
		if r.fails[u.Key()] < r.maxFails {
			return u, true
		}
	}
	// Everything is marked bad — forgive all and hand out the next one so the
	// caller keeps making progress rather than stalling on a fully-burned list.
	r.fails = map[string]int{}
	u := r.ups[r.pos%n]
	r.pos = (r.pos + 1) % n
	return u, true
}

// MarkBad records a connection-level failure for an upstream. After maxFails it
// is skipped until the list is reloaded (or all upstreams burn out).
func (r *Rotor) MarkBad(u Upstream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fails[u.Key()]++
}

// Close stops the background reloader, if any. Safe to call more than once and
// from several goroutines at once: only the first call closes the stop channel.
func (r *Rotor) Close() {
	r.closeOnce.Do(func() {
		if r.stop == nil {
			return
		}
		close(r.stop)
		<-r.done
	})
}

func (r *Rotor) refreshLoop() {
	defer close(r.done)
	t := time.NewTicker(r.src.Refresh)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			ups, _, err := r.src.Load(ctx)
			cancel()
			if err != nil || len(ups) == 0 {
				continue // keep the old list on a failed refresh
			}
			r.reconcile(ups)
		}
	}
}

// reconcile swaps in a freshly loaded list while keeping the cursor position and
// forgetting failure counts for proxies that are no longer present.
func (r *Rotor) reconcile(ups []Upstream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	present := make(map[string]bool, len(ups))
	for _, u := range ups {
		present[u.Key()] = true
	}
	for k := range r.fails {
		if !present[k] {
			delete(r.fails, k)
		}
	}
	r.ups = ups
	if len(ups) > 0 {
		r.pos %= len(ups)
	} else {
		r.pos = 0
	}
}

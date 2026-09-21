// SPDX-License-Identifier: MIT

// Package sessions keeps the sessions this program searches through: a
// fingerprint, named by a profile the proxy service holds, and the cookies the
// search engine has handed it.
//
// They used to live inside the ports. A port opened with keep_sessions on kept
// its own cookie jar and the solver's clearance, and a session was therefore
// whatever port it happened to be on — gone when the port was closed, and
// unable to follow a thread to another one. With keep_sessions off the port
// keeps nothing, the cookies a challenge is won with come back with the
// answer, and a session becomes something this program can hold, write down
// and hand to any port it likes.
package sessions

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"sync"
	"time"
)

// Jar is a cookie jar whose contents can be written down and read back.
//
// The standard one decides everything that matters — which cookie goes to
// which host, which path, when one has expired — and cannot be listed: it
// answers what it holds for a given address and nothing about what it holds in
// all. A session outlives the process it was made in, so what it holds has to
// be listable. This keeps the standard jar for the deciding and a record of
// every cookie it was given for the writing down; reading it back replays that
// record into a fresh standard jar, so the deciding is done by the same code
// on the way back in as it was on the way out.
//
// Every host is kept, not one. A search for Russia goes to google.ru and wins
// its clearance there, while the consent flow sets cookies on google.com; a
// jar written down for one host is a session missing the part of it that was
// paid for — measured, the first time a session was carried between ports it
// took four cookies for google.com and left the two the challenge was won with
// behind on google.ru.
type Jar struct {
	mu   sync.Mutex
	std  *cookiejar.Jar
	kept map[cookieKey]Cookie
	now  func() time.Time
}

// Cookie is one cookie as a session writes it down: what it is, and the address
// it was set from, which is what decides where it goes back to.
type Cookie struct {
	// From is the address the cookie was set by. The standard jar reads a
	// cookie's scope off it — a cookie with no Domain belongs to that host and
	// no other — so it is kept rather than reconstructed.
	From     string    `json:"from"`
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HttpOnly bool      `json:"http_only,omitempty"`
	SameSite int       `json:"same_site,omitempty"`
}

// cookieKey is what makes two cookies the same cookie: a second Set-Cookie
// with the same name, for the same host, domain and path, replaces the first.
type cookieKey struct {
	host, name, domain, path string
}

var _ http.CookieJar = (*Jar)(nil)

// NewJar returns an empty jar.
func NewJar() *Jar {
	std, _ := cookiejar.New(nil) // New(nil) cannot fail
	return &Jar{std: std, kept: map[cookieKey]Cookie{}, now: time.Now}
}

// SetCookies is http.CookieJar: it hands the cookies to the standard jar and
// writes each one down.
//
// A cookie that arrives already expired is how a server deletes one, and it is
// taken out of the record as well as out of the jar — a record that kept it
// would put it back the next time the session was read.
func (j *Jar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.std.SetCookies(u, cookies)
	now := j.now()
	for _, c := range cookies {
		key := cookieKey{host: u.Hostname(), name: c.Name, domain: c.Domain, path: c.Path}
		if gone(c, now) {
			delete(j.kept, key)
			continue
		}
		expires := c.Expires
		if c.MaxAge > 0 {
			// Max-Age wins over Expires, as the standard says, and it is
			// relative: written down as it arrived, it would mean a different
			// moment every time the session was read back.
			expires = now.Add(time.Duration(c.MaxAge) * time.Second)
		}
		j.kept[key] = Cookie{
			From:     (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}).String(),
			Name:     c.Name,
			Value:    c.Value,
			Path:     c.Path,
			Domain:   c.Domain,
			Expires:  expires,
			Secure:   c.Secure,
			HttpOnly: c.HttpOnly,
			SameSite: int(c.SameSite),
		}
	}
}

// Cookies is http.CookieJar, answered by the standard jar.
func (j *Jar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.std.Cookies(u)
}

// Held is every cookie the jar holds that has not expired, in a fixed order.
func (j *Jar) Held() []Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	out := make([]Cookie, 0, len(j.kept))
	for key, c := range j.kept {
		if !c.Expires.IsZero() && !c.Expires.After(now) {
			// Expired while it was held. It is dropped here as the standard
			// jar drops it, so what is written down is what would be sent.
			delete(j.kept, key)
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].From != out[b].From {
			return out[a].From < out[b].From
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// MarshalJSON writes the jar down.
func (j *Jar) MarshalJSON() ([]byte, error) {
	return json.Marshal(j.Held())
}

// ReadJar reads a jar written down by MarshalJSON.
//
// Every cookie is handed back to a fresh standard jar from the address it was
// set by, so scope, path and expiry are decided on the way back in by the same
// code that decided them on the way out. A cookie that expired while the
// session was written down is not read back at all.
func ReadJar(written []byte) (*Jar, error) {
	j := NewJar()
	if len(written) == 0 {
		return j, nil
	}
	var held []Cookie
	if err := json.Unmarshal(written, &held); err != nil {
		return nil, err
	}
	j.restore(held)
	return j, nil
}

// restore hands a written-down set of cookies back to the jar.
func (j *Jar) restore(held []Cookie) {
	now := j.now()
	for _, c := range held {
		if !c.Expires.IsZero() && !c.Expires.After(now) {
			continue
		}
		from, err := url.Parse(c.From)
		if err != nil || from.Host == "" {
			continue
		}
		j.SetCookies(from, []*http.Cookie{{
			Name:     c.Name,
			Value:    c.Value,
			Path:     c.Path,
			Domain:   c.Domain,
			Expires:  c.Expires,
			Secure:   c.Secure,
			HttpOnly: c.HttpOnly,
			SameSite: http.SameSite(c.SameSite),
		}})
	}
}

// gone says whether a cookie arriving now is a deletion rather than a value.
func gone(c *http.Cookie, now time.Time) bool {
	if c.MaxAge < 0 {
		return true
	}
	return c.MaxAge == 0 && !c.Expires.IsZero() && !c.Expires.After(now)
}

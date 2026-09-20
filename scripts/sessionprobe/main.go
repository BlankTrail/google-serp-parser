// SPDX-License-Identifier: MIT

// sessionprobe measures one Google session on one dedicated BlankTrail port.
// It deliberately bypasses the parser pool: no client retries, renewals, or
// egress changes can silently turn the experiment into a different session.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/blanktrail/google-serp-parser/internal/blanktrail"
	"github.com/blanktrail/google-serp-parser/internal/google"
	"github.com/blanktrail/google-serp-parser/internal/settings"
)

type control struct {
	base, key string
	http      *http.Client
}

func (c control) call(ctx context.Context, method, path string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("control %s: HTTP %d", path, resp.StatusCode)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

type portState struct {
	Port          int    `json:"port"`
	Created       string `json:"created_at"`
	Profile       string `json:"current_profile"`
	LastActivity  string `json:"last_activity"`
	KeepSessions  bool   `json:"keep_sessions"`
	Solver        bool   `json:"js_solver"`
	CaptchaAction string `json:"captcha_action"`
	IdleSeconds   int    `json:"effective_idle_seconds"`
}

type stats struct {
	Attempts int64 `json:"total_attempts"`
	Solved   int64 `json:"total_solved"`
	Families []struct {
		Family   string `json:"family"`
		Attempts int64  `json:"attempts"`
		Solved   int64  `json:"solved"`
	} `json:"families"`
	Seen struct {
		Total int64 `json:"total"`
		Since int64 `json:"since_unix_ms"`
	} `json:"seen"`
}

type snapshot struct {
	Port      portState `json:"port"`
	OpenPorts int       `json:"open_ports"`
	Stats     stats     `json:"stats"`
}

func (c control) snapshot(ctx context.Context, port int) (snapshot, error) {
	var s snapshot
	var listing struct {
		Ports []portState `json:"ports"`
	}
	if err := c.call(ctx, "GET", "/api/v1/ports", nil, &listing); err != nil {
		return s, err
	}
	s.OpenPorts = len(listing.Ports)
	for _, p := range listing.Ports {
		if p.Port == port {
			s.Port = p
		}
	}
	if s.Port.Port == 0 {
		return s, errors.New("the experiment port disappeared")
	}
	err := c.call(ctx, "GET", "/api/v1/solver/stats", nil, &s.Stats)
	return s, err
}

type wireEvent struct {
	Host          string           `json:"host"`
	Path          string           `json:"path"`
	Status        int              `json:"status"`
	HeaderSeconds float64          `json:"header_seconds"`
	SetCookies    []cookieMetadata `json:"set_cookies,omitempty"`
}

// Values are deliberately absent: the probe needs expiry/scope changes and
// newly introduced names, not reusable Google session credentials.
type cookieMetadata struct {
	Name    string     `json:"name"`
	Domain  string     `json:"domain,omitempty"`
	Path    string     `json:"path,omitempty"`
	MaxAge  int        `json:"max_age,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
}
type observedTransport struct {
	base   http.RoundTripper
	events []wireEvent
}

func (t *observedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	e := wireEvent{Host: req.URL.Host, Path: req.URL.Path, HeaderSeconds: time.Since(start).Seconds()}
	if resp != nil {
		e.Status = resp.StatusCode
		for _, cookie := range resp.Cookies() {
			meta := cookieMetadata{Name: cookie.Name, Domain: cookie.Domain, Path: cookie.Path, MaxAge: cookie.MaxAge}
			if !cookie.Expires.IsZero() {
				expiry := cookie.Expires
				meta.Expires = &expiry
			}
			e.SetCookies = append(e.SetCookies, meta)
		}
	}
	t.events = append(t.events, e)
	return resp, err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	config := flag.String("settings", "gserp-settings.json", "connection settings (read only)")
	upstream := flag.String("upstream", "", "fixed SOCKS5 proxy URL; credentials may instead be set in GSERP_PROBE_UPSTREAM")
	gap := flag.Duration("gap", time.Minute, "minimum idle interval after each completed search")
	count := flag.Int("count", 22, "number of searches, including the cold start")
	requestTimeout := flag.Duration("request-timeout", 6*time.Minute, "timeout for each HTTP exchange")
	deadline := flag.Duration("duration", 45*time.Minute, "maximum duration of the experiment")
	output := flag.String("output", "", "JSONL file, written immediately instead of shell-buffered stdout")
	maxErrors := flag.Int("max-errors", 0, "stop after this many consecutive failed searches (0 disables)")
	flag.Parse()
	if *upstream == "" {
		*upstream = os.Getenv("GSERP_PROBE_UPSTREAM")
	}
	u, err := url.Parse(*upstream)
	if err != nil || u.Host == "" || u.Scheme != "socks5" {
		return errors.New("a fixed socks5:// upstream is required")
	}
	if *gap < 0 || *count < 1 || *deadline <= 0 || *requestTimeout <= 0 || *maxErrors < 0 {
		return errors.New("invalid count or duration")
	}
	raw, err := os.ReadFile(*config)
	if err != nil {
		return err
	}
	var saved settings.Settings
	if err = json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	base, err := url.Parse(saved.ControlURL)
	if err != nil || base.Host == "" || saved.APIKey == "" {
		return errors.New("connection settings are incomplete")
	}
	base.Path = ""
	c := control{base: base.String(), key: saved.APIKey, http: &http.Client{Timeout: 15 * time.Second}}
	// Never print raw upstream credentials or control response bodies.
	base.Path = "/"
	client, err := blanktrail.NewClient(base.String(), saved.APIKey)
	if err != nil {
		return err
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, *deadline)
	defer cancel()
	writer := os.Stdout
	if *output != "" {
		writer, err = os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer writer.Close()
	}
	out := json.NewEncoder(writer)
	emit := func(v any) {
		if err := out.Encode(v); err != nil {
			cancel()
		}
	}
	var build struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := c.call(ctx, "GET", "/api/v1/status", nil, &build); err != nil {
		return err
	}
	ca, err := client.FetchCAPool(ctx)
	if err != nil {
		return errors.New("cannot obtain BlankTrail CA")
	}
	port, err := client.SuggestPort(ctx)
	if err != nil {
		return errors.New("cannot obtain a free BlankTrail port")
	}
	spec := blanktrail.DefaultPortSpec()
	// Single threaded describes our request loop. MaxConcurrent limits open
	// tunnels inside BlankTrail, including solver traffic, so setting it to one
	// would make the diagnostic itself block the solver.
	spec.IdleSeconds = int((*deadline + *requestTimeout + time.Minute).Seconds())
	if _, err := client.OpenPort(ctx, port, spec, blanktrail.Egress{Upstream: *upstream}); err != nil {
		return fmt.Errorf("cannot open dedicated port %d", port)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		err := client.ClosePort(cleanup, port)
		emit(map[string]any{"event": "closed", "port": port, "ok": err == nil})
	}()
	// Stop the service from changing the identity after an unresolved CAPTCHA.
	// Solving stays enabled; a failed solve is returned to this experiment.
	if err := c.call(ctx, "PUT", fmt.Sprintf("/api/v1/port/%d/config", port), map[string]any{
		"captcha_action": "return_to_script", "skip_retry": true,
	}, nil); err != nil {
		return err
	}
	initial, err := c.snapshot(ctx, port)
	if err != nil {
		return err
	}
	emit(map[string]any{"event": "start", "at": time.Now().UTC(), "build": build, "port": port,
		"upstream_host": u.Host, "gap_seconds": gap.Seconds(), "count": *count, "initial": initial,
		"limitations": "solver counters are service-wide; pin identity/LastOK are not exposed by the API"})
	proxyURL := &url.URL{Scheme: "socks5", Host: net.JoinHostPort(base.Hostname(), strconv.Itoa(port))}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: ca},
		DisableKeepAlives: true, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second}
	defer transport.CloseIdleConnections()
	observed := &observedTransport{base: transport}
	sess := google.NewSession(observed)
	sess.Client.Timeout = *requestTimeout
	ipClient := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	checkIP := func(phase string) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org?format=json", nil)
		resp, err := ipClient.Do(req)
		if err != nil {
			emit(map[string]any{"event": "exit_ip", "phase": phase, "available": false})
			return
		}
		defer resp.Body.Close()
		var result struct {
			IP string `json:"ip"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result)
		emit(map[string]any{"event": "exit_ip", "phase": phase, "available": err == nil && net.ParseIP(result.IP) != nil, "ip": result.IP})
	}
	checkIP("start")
	phrases := []string{"weather", "recipes", "dictionary", "train times", "calculator", "news", "maps", "translate",
		"hardware store", "opening hours", "football scores", "flight status", "currency", "postcode", "pharmacy",
		"bus timetable", "cinema", "library", "car hire", "dentist", "museum tickets", "bank holidays"}
	start := time.Now()
	var finished time.Time
	var good, failed int
	var consecutiveErrors int
	for n := 0; n < *count; n++ {
		if !finished.IsZero() {
			remaining := *gap - time.Since(finished)
			if remaining > 0 {
				timer := time.NewTimer(remaining)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := c.snapshot(ctx, port)
		if err != nil {
			return err
		}
		observed.events = nil
		cold := good == 0
		began := time.Now()
		idle := 0.0
		if !finished.IsZero() {
			idle = began.Sub(finished).Seconds()
		}
		emit(map[string]any{"event": "request_start", "n": n + 1, "at": began.UTC(), "age_seconds": began.Sub(start).Seconds()})
		serp, searchErr := sess.Search(ctx, google.Query{Text: phrases[n%len(phrases)], Country: "us", Language: "en"})
		finished = time.Now()
		after, snapshotErr := c.snapshot(ctx, port)
		class := google.ClassSERP
		if searchErr != nil {
			failed++
			consecutiveErrors++
			class = "transport"
			if v, ok := google.ClassOf(searchErr); ok {
				class = v
			}
		} else {
			good++
			consecutiveErrors = 0
		}
		row := map[string]any{"event": "request", "n": n + 1, "cold": cold, "at": began.UTC(),
			"age_seconds": began.Sub(start).Seconds(), "idle_seconds": idle, "seconds": finished.Sub(began).Seconds(),
			"class": class, "results": len(serp.Results), "wire": observed.events, "before": before, "after": after,
			"snapshot_ok": snapshotErr == nil}
		if searchErr != nil {
			message := strings.ReplaceAll(searchErr.Error(), saved.APIKey, "[redacted]")
			message = strings.ReplaceAll(message, *upstream, u.Redacted())
			row["error"] = message
		}
		if snapshotErr == nil {
			row["challenges_delta"] = after.Stats.Seen.Total - before.Stats.Seen.Total
			row["solves_delta"] = after.Stats.Attempts - before.Stats.Attempts
			row["solved_delta"] = after.Stats.Solved - before.Stats.Solved
			row["counters_isolated"] = before.OpenPorts == 1 && after.OpenPorts == 1 && before.Stats.Seen.Since == after.Stats.Seen.Since
			row["port_identity_stable"] = initial.Port.Created == after.Port.Created && initial.Port.Profile == after.Port.Profile
		}
		emit(row)
		if snapshotErr != nil {
			return snapshotErr
		}
		if initial.Port.Created != after.Port.Created || initial.Port.Profile != after.Port.Profile {
			return errors.New("port identity changed: experiment stopped")
		}
		if *maxErrors > 0 && consecutiveErrors >= *maxErrors {
			emit(map[string]any{"event": "stopped", "reason": "consecutive_errors", "good": good, "failed": failed})
			return errors.New("consecutive search errors: experiment stopped")
		}
		if (n+1)%5 == 0 {
			checkIP(fmt.Sprintf("after_%d", n+1))
		}
	}
	checkIP("end")
	emit(map[string]any{"event": "summary", "good": good, "failed": failed, "seconds": time.Since(start).Seconds()})
	return nil
}

//go:build stagingacceptance

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// response is one HTTP exchange. Body is kept in memory only (never written anywhere unscrubbed).
type response struct {
	Status    int
	Header    http.Header
	Body      []byte
	RequestID string
	Dur       time.Duration
}

func (r *response) obj() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return nil
	}
	return m
}

func (r *response) arr() []any {
	var a []any
	if err := json.Unmarshal(r.Body, &a); err != nil {
		return nil
	}
	return a
}

// code is the API error code of an error envelope ("" when the body is not one).
func (r *response) code() string {
	if m := r.obj(); m != nil {
		if s, ok := m["code"].(string); ok {
			return s
		}
	}
	return ""
}

func (r *response) ok() bool { return r.Status >= 200 && r.Status < 300 }

// brief is a short, scrubbed-by-the-reporter description of a response for failure details.
func (r *response) brief() string {
	b := strings.Join(strings.Fields(string(r.Body)), " ")
	if len(b) > 220 {
		b = b[:220] + "..."
	}
	return fmt.Sprintf("HTTP %d %s (request_id=%s)", r.Status, b, r.RequestID)
}

// pick walks nested maps by key.
func pick(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func pickStr(v any, path ...string) string {
	s, _ := pick(v, path...).(string)
	return s
}

func pickNum(v any, path ...string) (float64, bool) {
	f, ok := pick(v, path...).(float64)
	return f, ok
}

type client struct {
	base     *url.URL
	hc       *http.Client
	rep      *reporter
	runID    string
	seq      atomic.Int64
	cur      atomic.Value // current check name (string)
	debug    bool
	debugOut io.Writer
	insecure bool
}

func newClient(base string, rep *reporter, runID string, timeout time.Duration, debug bool, debugOut io.Writer) (*client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("BASE_URL must be an absolute http(s) URL")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("BASE_URL must not carry credentials, a query or a fragment")
	}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	c := &client{
		base: u,
		hc: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			// Never follow redirects: a redirect on an API route is itself a finding.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		rep: rep, runID: runID, debug: debug, debugOut: debugOut,
	}
	c.cur.Store("setup")
	return c, nil
}

type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// call performs a request. body may be nil, a []byte/string (sent verbatim) or any JSON-marshalable value.
// The request carries X-Request-Id: acc-<run>-<n> so the operator can find the structured log line in CloudWatch.
func (c *client) call(method, path, token string, body any, opts ...reqOpt) (*response, error) {
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	u := *c.base
	if i := strings.IndexByte(path, '?'); i >= 0 {
		u.Path = strings.TrimRight(c.base.Path, "/") + path[:i]
		u.RawQuery = path[i+1:]
	} else {
		u.Path = strings.TrimRight(c.base.Path, "/") + path
	}
	req, err := http.NewRequest(method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	rid := fmt.Sprintf("acc-%s-%04d", c.runID, c.seq.Add(1))
	req.Header.Set("X-Request-Id", rid)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "igaming-staging-acceptance/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, o := range opts {
		o(req)
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	dur := time.Since(start)
	if err != nil {
		// Network errors can embed the URL (no secrets in it by construction) - keep it short.
		return nil, fmt.Errorf("%s %s: %v", method, path, shortErr(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %v", method, path, shortErr(err))
	}
	out := &response{Status: resp.StatusCode, Header: resp.Header, Body: raw, RequestID: resp.Header.Get("X-Request-Id"), Dur: dur}
	if out.RequestID == "" {
		out.RequestID = rid
	}
	check, _ := c.cur.Load().(string)
	c.rep.noteRequest(reqEvidence{Check: check, Method: method, Path: pathOnly(path), RequestID: out.RequestID, Status: out.Status, Duration: dur})
	if c.debug && c.debugOut != nil {
		fmt.Fprintf(c.debugOut, "DEBUG|%s|%s %s|%d|%s\n", check, method, pathOnly(path), out.Status, c.rep.scrub.clean(strings.Join(strings.Fields(string(raw)), " ")))
	}
	return out, nil
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

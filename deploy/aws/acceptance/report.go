//go:build stagingacceptance

package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type status string

const (
	statusPass status = "PASS"
	statusFail status = "FAIL"
	statusSkip status = "SKIP"
)

// skipKind separates a SKIP caused by a documented missing prerequisite (the operator can fix it by supplying the
// input) from a SKIP caused by an earlier FAIL (the dependent flow could not be attempted). Both are SKIP lines; the
// evidence summary counts them apart so a FAIL cascade is never mistaken for "just missing inputs".
type skipKind string

const (
	skipPrereq  skipKind = "prerequisite"
	skipBlocked skipKind = "blocked-by-fail"
	skipManual  skipKind = "operator-evidence"
	skipGap     skipKind = "platform-gap"
)

type result struct {
	Name     string
	Status   status
	Detail   string
	Skip     skipKind
	Duration time.Duration
}

// scrubber removes secret values from every string that leaves the process (stdout, the results file, stderr).
// Known secrets (passwords, TOTP secrets, tokens learned at run time) are registered and replaced literally; anything
// that merely looks like a credential (JWT, long opaque token, Bearer header, otpauth URI) is replaced by shape.
type scrubber struct {
	mu      sync.Mutex
	secrets map[string]struct{}
}

var (
	reJWT     = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
	reBearer  = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	reOTPAuth = regexp.MustCompile(`otpauth://[^\s"']+`)
	reOpaque  = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)
	reNumCode = regexp.MustCompile(`(?i)("?code"?\s*[:=]\s*"?)\d{6,10}("?)`)
	reKV      = regexp.MustCompile(`(?i)("?(?:password|secret|secret_base32|token|access_token|refresh_token|challenge_token|recovery_code|recovery_codes|authorization)"?\s*[:=]\s*)("[^"]*"|\[[^\]]*\]|[^\s,}&]+)`)
)

func newScrubber() *scrubber { return &scrubber{secrets: map[string]struct{}{}} }

// add registers a literal secret. Very short values are ignored (they would shred ordinary text).
func (s *scrubber) add(v string) {
	v = strings.TrimSpace(v)
	if len(v) < 6 {
		return
	}
	s.mu.Lock()
	s.secrets[v] = struct{}{}
	s.mu.Unlock()
}

func (s *scrubber) clean(in string) string {
	s.mu.Lock()
	lits := make([]string, 0, len(s.secrets))
	for k := range s.secrets {
		lits = append(lits, k)
	}
	s.mu.Unlock()
	sort.Slice(lits, func(i, j int) bool { return len(lits[i]) > len(lits[j]) })
	out := in
	for _, l := range lits {
		out = strings.ReplaceAll(out, l, "[REDACTED]")
	}
	out = reOTPAuth.ReplaceAllString(out, "[REDACTED-OTPAUTH]")
	out = reJWT.ReplaceAllString(out, "[REDACTED-JWT]")
	out = reBearer.ReplaceAllString(out, "Bearer [REDACTED]")
	out = reKV.ReplaceAllString(out, `${1}"[REDACTED]"`)
	out = reNumCode.ReplaceAllString(out, `${1}[REDACTED]${2}`)
	// Opaque tokens last: UUIDs contain dashes and are 36 chars; keep them (they are evidence, not secrets).
	out = reOpaque.ReplaceAllStringFunc(out, func(m string) string {
		// Evidence is not a secret: UUIDs, and identifiers that are plain words (check names, route segments) carry no digit or no letter.
		if reUUID.MatchString(m) || !strings.ContainsAny(m, "0123456789") || !reHasLetter.MatchString(m) {
			return m
		}
		return "[REDACTED-OPAQUE]"
	})
	return out
}

var reHasLetter = regexp.MustCompile(`[A-Za-z]`)

var reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// oneLine makes a detail safe for the RESULT|name|STATUS|detail format: no newlines, no field separator.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 600 {
		s = s[:600] + "...(truncated)"
	}
	return s
}

type reporter struct {
	mu      sync.Mutex
	scrub   *scrubber
	out     io.Writer // results file
	echo    io.Writer // stdout
	results []result
	started time.Time
	reqs    []reqEvidence
	// counters
	httpCalls int
}

type reqEvidence struct {
	Check, Method, Path, RequestID string
	Status                         int
	Duration                       time.Duration
}

func newReporter(scrub *scrubber, out, echo io.Writer) *reporter {
	return &reporter{scrub: scrub, out: out, echo: echo, started: time.Now()}
}

func (r *reporter) line(format string, a ...any) {
	l := r.scrub.clean(fmt.Sprintf(format, a...))
	if r.out != nil {
		_, _ = fmt.Fprintln(r.out, l)
	}
	if r.echo != nil {
		_, _ = fmt.Fprintln(r.echo, l)
	}
}

func (r *reporter) add(res result) {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
	detail := res.Detail
	if res.Status == statusSkip && res.Skip != "" {
		detail = "[" + string(res.Skip) + "] " + detail
	}
	r.line("RESULT|%s|%s|%s", res.Name, res.Status, oneLine(detail))
}

func (r *reporter) noteRequest(e reqEvidence) {
	r.mu.Lock()
	r.httpCalls++
	r.reqs = append(r.reqs, e)
	r.mu.Unlock()
}

type counts struct {
	Pass, Fail, Skip, SkipPrereq, SkipBlocked, SkipManual, SkipGap int
}

func (r *reporter) tally() counts {
	r.mu.Lock()
	defer r.mu.Unlock()
	var c counts
	for _, x := range r.results {
		switch x.Status {
		case statusPass:
			c.Pass++
		case statusFail:
			c.Fail++
		case statusSkip:
			c.Skip++
			switch x.Skip {
			case skipBlocked:
				c.SkipBlocked++
			case skipManual:
				c.SkipManual++
			case skipGap:
				c.SkipGap++
			default:
				c.SkipPrereq++
			}
		}
	}
	return c
}

// summary writes the evidence block: SUMMARY (counts, durations) and EVIDENCE lines (request ids for the operator's
// CloudWatch correlation). It returns the counts.
func (r *reporter) summary(meta map[string]string) counts {
	c := r.tally()
	r.mu.Lock()
	total := time.Since(r.started)
	n := len(r.results)
	calls := r.httpCalls
	reqs := append([]reqEvidence(nil), r.reqs...)
	results := append([]result(nil), r.results...)
	r.mu.Unlock()

	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.line("META|%s|%s", k, oneLine(meta[k]))
	}
	r.line("SUMMARY|checks|%d", n)
	r.line("SUMMARY|pass|%d", c.Pass)
	r.line("SUMMARY|fail|%d", c.Fail)
	r.line("SUMMARY|skip|%d", c.Skip)
	r.line("SUMMARY|skip_prerequisite|%d", c.SkipPrereq)
	r.line("SUMMARY|skip_blocked_by_fail|%d", c.SkipBlocked)
	r.line("SUMMARY|skip_operator_evidence|%d", c.SkipManual)
	r.line("SUMMARY|skip_platform_gap|%d", c.SkipGap)
	r.line("SUMMARY|http_requests|%d", calls)
	r.line("SUMMARY|duration_seconds|%.1f", total.Seconds())
	var slow []result
	slow = append(slow, results...)
	sort.Slice(slow, func(i, j int) bool { return slow[i].Duration > slow[j].Duration })
	for i := 0; i < len(slow) && i < 5; i++ {
		r.line("SUMMARY|slowest|%s|%.1fs", slow[i].Name, slow[i].Duration.Seconds())
	}
	// Request ids of the non-2xx and of the first request of every check: enough to find the structured http_request
	// lines in CloudWatch without listing every call.
	seen := map[string]bool{}
	for _, e := range reqs {
		first := !seen[e.Check]
		seen[e.Check] = true
		if first || e.Status >= 500 {
			r.line("EVIDENCE|request|%s|%s %s|%d|%s|%dms", e.Check, e.Method, e.Path, e.Status, e.RequestID, e.Duration.Milliseconds())
		}
	}
	return c
}

func openResultsFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

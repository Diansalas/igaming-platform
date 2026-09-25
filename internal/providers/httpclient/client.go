// Package httpclient provides a generic, transport-agnostic outbound HTTP
// client that a future real provider adapter (casino, sportsbook,
// payments, identity-compliance) composes once that adapter is built
// against an actual documented vendor contract.
//
// This is scaffolding only, per docs/decisions/0080-provider-integration-
// readiness-without-external-contracts.md ("Provider Integration
// Readiness Without External Contracts", Decision 3). Stage 8 found no
// available documentation, base URL, or credentials for the "Dummy
// Sportsbook"/"Dummy Casino" APIs originally scoped for this stage, so
// this package deliberately never assumes any specific real-world
// provider's endpoint paths, authentication scheme, or payload shape -
// every provider-specific detail is either a caller-supplied Request
// field or config the caller passes into ClientConfig. This package makes
// no network call on its own initiative; it only does so when a caller
// explicitly invokes Client.Do.
//
// Per Stage 8 §12's explicit instruction, this implements exactly the
// "minimum production-quality" behavior a provider boundary needs and
// nothing more - no service mesh, no circuit-breaker framework, no
// sophisticated backoff algorithm:
//
//   - An explicit, configured per-call timeout applied via
//     context.WithTimeout on every attempt (never an inherited-forever
//     context).
//   - Bounded retry ONLY when the caller explicitly marks a request
//     idempotent (Request.Idempotent) and only for transport-level
//     failures or a 5xx response - never for a 4xx, and never for a
//     non-idempotent request under any failure class. This package never
//     infers idempotency itself from the HTTP method or anything else;
//     only the caller, who knows whether replaying this exact request is
//     safe, states it.
//   - Four sentinel error categories (ErrProviderTimeout,
//     ErrProviderUnavailable, ErrProviderRejected,
//     ErrProviderMalformedResponse) a caller's own adapter error mapping
//     can switch over instead of handling raw net/http/context errors.
//     TimeoutError/UnavailableError additionally carry a Sent field
//     distinguishing "definitely never reached the provider" from
//     "possibly reached the provider" - the fail-fast-with-no-state vs.
//     must-write-a-pending/unknown-state-and-reconcile decision a
//     money-moving adapter needs (see TimeoutError.Sent's doc comment).
//   - No automatic redirect following: the default-constructed
//     *http.Client refuses every redirect (CheckRedirect returns
//     http.ErrUseLastResponse), so a 3xx is returned to the caller as a
//     Response rather than silently followed - closing a
//     credential-exfiltration path a redirecting/compromised provider
//     could otherwise use against ClientConfig.AuthHeaderName (see New's
//     own doc comment).
//   - Do's retry loop treats the caller's own ctx being done as a
//     distinct condition from the provider being slow/unhealthy: it
//     checks ctx.Err() before every iteration and short-circuits
//     immediately (never burning the rest of the retry budget) with a
//     TimeoutError whose CallerCanceled field is true (see Do's own doc
//     comment).
//   - One OTel span per Do call (tracer "igaming-platform/providers",
//     span "provider.call"), with attributes for provider name,
//     operation, duration, and outcome - never a credential, auth header
//     value, or raw request/response body. This is the first
//     outbound-call tracing convention in this repo (see
//     internal/observability/tracing.go's own doc comment: no domain
//     package created spans before this one).
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	tracerName = "igaming-platform/providers"
	spanName   = "provider.call"

	// defaultTimeout applies only when ClientConfig.Timeout is left zero.
	// A future real adapter is expected to source this from
	// internal/providers.ProviderConfig instead of relying on this
	// fallback.
	defaultTimeout = 10 * time.Second

	// maxResponseBodyBytes bounds how much of a response body this
	// package will buffer into memory. This is a defensive limit, not a
	// statement about any real provider's actual response sizes (none is
	// known - see package doc comment).
	maxResponseBodyBytes = 10 << 20 // 10 MiB

	// backoffBase/backoffCap bound the simple exponential backoff between
	// retry attempts. Deliberately unsophisticated per Stage 8 §12 - this
	// is not meant to be a tunable, adaptive, or jittered backoff
	// algorithm.
	backoffBase = 10 * time.Millisecond
	backoffCap  = 200 * time.Millisecond

	// maxErrorContentBytes bounds how much of a response body (and, per
	// value, how much of a response header) is retained on
	// RejectedError.Body/.Header and UnavailableError.Body/.Header -
	// a defensive cap independent of maxResponseBodyBytes above, which
	// bounds how much of the wire response this package will read into
	// memory AT ALL. This bound instead governs what stays behind on a
	// long-lived error struct - and anywhere that struct is subsequently
	// logged, formatted, or serialized - for the life of the call and
	// after.
	//
	// This is a Stage 9.1 (`integrations`) hardening of the residual gap
	// ADR 0080 Decision 3 / Stage 9 addendum documented and left open: the
	// existing redactCredential/redactCredentialHeader only scrub the ONE
	// secret this package itself was configured with (ClientConfig.
	// AuthHeaderValue), and do nothing for content this package has no way
	// to recognize as sensitive (a session token, a different credential,
	// a PII fragment) that a vendor's own response might echo back - e.g.
	// a debug endpoint reflecting the full request, or a large gateway
	// error page. Deliberately NOT a heuristic/content-sniffing "does this
	// look like a secret" detector - that would be false confidence, and
	// this package's own "no cleverness" convention (see the package doc
	// comment) rules it out. A hard length bound is the honest version of
	// this mitigation: it cannot stop an in-bound secret from being
	// present, but it does stop an unbounded or enormous vendor payload
	// from sitting verbatim in application state/logs indefinitely - see
	// boundResponseBody/boundResponseHeader's own doc comments for the
	// exact mechanism and its documented limits.
	//
	// 4 KiB (well below maxResponseBodyBytes' 10 MiB wire-read cap) is
	// generous for a diagnostic 4xx/5xx body while still bounding the
	// blast radius - Body/Header exist for a human or an adapter's
	// error-path logging to read, not to reproduce the provider's full
	// response.
	maxErrorContentBytes = 4 << 10 // 4 KiB
)

// truncationMarkerFmt is appended after a Body/header value this package
// cut off at maxErrorContentBytes, so a reader (human or log parser) can
// tell "the platform truncated this" apart from "the provider's response
// genuinely ended here" - and recover the original length even though the
// content past the cut is gone.
const truncationMarkerFmt = "...[truncated by platform, original length %d bytes]"

// ClientConfig configures a Client. BaseURL, Timeout, MaxRetries, and the
// auth header fields are all supplied by the caller at construction time
// - this package never reads an environment variable or secret store
// itself (see internal/providers.ProviderConfig/ResolveAPIKey, Decision
// 4, for how a real adapter is expected to resolve AuthHeaderValue before
// constructing a Client).
type ClientConfig struct {
	// ProviderName identifies the provider for tracing only (e.g. a
	// tenant-configured provider code) - never used to select behavior
	// within this package.
	ProviderName string

	// BaseURL is prefixed to every Request.Path. A trailing slash is
	// trimmed so callers may supply either form.
	BaseURL string

	// Timeout bounds every single HTTP attempt (context.WithTimeout,
	// applied per attempt - a retried request gets a fresh timeout
	// window for each attempt, not one shared budget across all of
	// them). Defaults to defaultTimeout if zero or negative.
	Timeout time.Duration

	// MaxRetries is the maximum number of retry attempts made *in
	// addition to* the initial attempt, and only ever applies when the
	// request is marked Idempotent and the failure is a transport-level
	// error or a 5xx. Negative values are treated as zero.
	MaxRetries int

	// HTTPClient, if set, is used instead of a default client. This
	// package still enforces Timeout via context.WithTimeout on every
	// attempt regardless of HTTPClient's own Timeout field, so a caller
	// overriding HTTPClient for e.g. a custom Transport does not
	// accidentally lose the per-attempt timeout guarantee.
	//
	// New() installs a CheckRedirect policy that never follows a
	// redirect (see New's own doc comment) on the client it builds by
	// default - but New() cannot safely mutate a caller-owned
	// *http.Client, so when HTTPClient is set here, that policy is NOT
	// applied for you. A caller supplying HTTPClient is responsible for
	// setting an equivalent CheckRedirect (e.g. returning
	// http.ErrUseLastResponse, or an equally strict policy) on it
	// themselves. Leaving Go's default redirect-following behavior in
	// place is a credential-exfiltration risk: Go's default
	// shouldCopyHeaderOnRedirect only strips Authorization/
	// WWW-Authenticate/Cookie/Cookie2 on a cross-domain redirect, so
	// ClientConfig.AuthHeaderName (deliberately an arbitrary
	// vendor-defined header name, e.g. a bespoke X-API-Key) would be
	// forwarded verbatim to whatever host a redirect names - including an
	// attacker-controlled one if the provider (or something impersonating
	// it) is compromised or misconfigured. See ADR 0080 Decision 3.
	HTTPClient *http.Client

	// AuthHeaderName/AuthHeaderValue, if AuthHeaderName is non-empty, are
	// set as a header on every request. The value is supplied by the
	// caller (e.g. resolved via internal/providers.ProviderConfig.
	// ResolveAPIKey) - this package treats it as an opaque secret: it is
	// never logged, never added as a span/trace attribute, and never
	// echoed back in any error.
	AuthHeaderName  string
	AuthHeaderValue string
}

// Client is a generic outbound HTTP client for a single provider
// endpoint. It is safe for concurrent use by multiple goroutines (it
// holds no mutable state after construction).
type Client struct {
	providerName    string
	baseURL         string
	timeout         time.Duration
	maxRetries      int
	httpClient      *http.Client
	authHeaderName  string
	authHeaderValue string
}

// New constructs a Client from cfg, applying the documented defaults for
// any zero-value field that has one.
//
// The default-constructed *http.Client (used whenever cfg.HTTPClient is
// left nil) never automatically follows a redirect: CheckRedirect always
// returns http.ErrUseLastResponse, so a 3xx response is returned to the
// caller as-is (see attempt's response classification) instead of being
// followed to whatever host the redirect names. This closes a
// credential-exfiltration path - Go's default redirect behavior
// (shouldCopyHeaderOnRedirect) only strips the Authorization/
// WWW-Authenticate/Cookie/Cookie2 headers on a cross-domain redirect, and
// forwards any other header verbatim, which is exactly the shape of
// ClientConfig.AuthHeaderName (an arbitrary vendor-defined header, e.g.
// X-API-Key). Refusing to follow a redirect at all is simpler and safer
// than trying to selectively strip headers, per Stage 8 §12's "no
// cleverness" instruction (ADR 0080 Decision 3). See ClientConfig.
// HTTPClient's own doc comment for what a caller overriding HTTPClient
// must do to preserve this guarantee.
func New(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxRetries := cfg.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Client{
		providerName:    cfg.ProviderName,
		baseURL:         strings.TrimSuffix(cfg.BaseURL, "/"),
		timeout:         timeout,
		maxRetries:      maxRetries,
		httpClient:      httpClient,
		authHeaderName:  cfg.AuthHeaderName,
		authHeaderValue: cfg.AuthHeaderValue,
	}
}

// Request is one outbound call. Idempotent is the caller's explicit,
// affirmative declaration that retrying this exact request is safe (per
// Stage 8 §12: "no automatic retry of non-idempotent operations unless
// idempotency is guaranteed") - Client never infers this from Method or
// anything else.
type Request struct {
	Method string
	Path   string
	Body   []byte

	// Idempotent must be explicitly set true by the caller for Do to
	// ever retry this request. Left false (the zero value) means "assume
	// unsafe to retry" - the conservative default.
	Idempotent bool

	// Operation is a caller-supplied label (e.g. "launch", "place_bet")
	// used as a span attribute instead of the raw path, since a path may
	// contain a sensitive identifier (a player/round/tx id).
	Operation string

	// Headers are additional headers to set on the request (beyond the
	// configured auth header). A caller-supplied value here is not
	// treated as a secret by this package - callers must not put a
	// credential here if they don't want it to end up in ordinary
	// request logging some future transport layer might add. The
	// configured AuthHeaderName/AuthHeaderValue is the mechanism this
	// package guarantees never gets logged.
	Headers map[string]string
}

// Response is the outcome of a successful (2xx) or passthrough (1xx/3xx)
// attempt. A 3xx is never a followed redirect - New's default *http.Client
// refuses to follow one (see New's own doc comment), so a 3xx Response is
// the redirect response itself, Location header and all, for the caller
// to act on. Client.Do returns a nil Response whenever it returns a
// non-nil error - every field the caller needs when a call fails is on
// the typed error itself (RejectedError.StatusCode/Body,
// UnavailableError.StatusCode/Body, etc.), not on Response.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// DecodeJSON unmarshals Body into v. It never inspects or assumes a
// shape itself - only the caller, who knows what response shape this
// provider's operation actually documents, does. A decode failure is
// wrapped as ErrProviderMalformedResponse (via *MalformedResponseError)
// so a caller's error-mapping switch can treat it identically to any
// other malformed-response case.
func (r *Response) DecodeJSON(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return &MalformedResponseError{Err: err}
	}
	return nil
}

// Do performs req, applying the configured timeout to every attempt and
// retrying only when req.Idempotent is true and the failure observed is a
// transport-level error or a 5xx (bounded by ClientConfig.MaxRetries,
// with a short fixed-cap exponential backoff between attempts). A 4xx is
// never retried. Exactly one OTel span is created per call to Do,
// regardless of how many underlying HTTP attempts it makes.
//
// Before every iteration (including the first), Do checks ctx.Err()
// itself and short-circuits immediately if it is non-nil, rather than
// proceeding into another attempt/backoff cycle. Without this check, a
// caller-cancelled (or caller-deadline-exceeded) ctx would make every
// remaining retry iteration fail near-instantly (sleepBackoff itself
// returns early on ctx.Done, and the subsequent attempt's
// context.WithTimeout(ctx, ...) is already-done from the start), burning
// the entire configured retry budget in a tight loop and, worse,
// misclassifying the outcome as ErrProviderUnavailable/ErrProviderTimeout
// as if the *provider* were unhealthy or slow - when in fact the
// platform's own caller gave up. The short-circuit instead returns
// immediately (attempts left as whatever was already made) with a
// *TimeoutError whose CallerCanceled field is true, so a caller can tell
// "our own context ended" apart from "the provider's per-attempt timeout
// actually elapsed" (see TimeoutError.CallerCanceled's own doc comment
// and ADR 0080 Decision 3 for why this reuses ErrProviderTimeout instead
// of adding a fifth sentinel).
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	tracer := otel.Tracer(tracerName)
	ctx, span := tracer.Start(ctx, spanName)
	defer span.End()
	span.SetAttributes(
		attribute.String("provider.name", c.providerName),
		attribute.String("provider.operation", req.Operation),
	)

	start := time.Now()

	maxAttempts := 1
	if req.Idempotent && c.maxRetries > 0 {
		maxAttempts = 1 + c.maxRetries
	}

	var resp *Response
	var callErr error
	attempts := 0
	everSent := false

	for {
		attempts++

		if ctxErr := ctx.Err(); ctxErr != nil {
			// The parent context is already done - do not make another
			// attempt or wait out a backoff first. Sent reflects whether
			// any *prior* attempt in this same Do call reached the wire
			// (this iteration itself never dispatched anything).
			resp = nil
			callErr = &TimeoutError{
				Attempts:       attempts,
				CallerCanceled: true,
				Sent:           everSent,
				Err:            ctxErr,
			}
			break
		}

		if attempts > 1 {
			sleepBackoff(ctx, attempts-1)
		}

		r, err, retryable := c.attempt(ctx, req)
		resp, callErr = r, err
		if err != nil && wasSent(err) {
			everSent = true
		}

		if err == nil || !retryable || attempts >= maxAttempts {
			break
		}
	}

	if callErr != nil {
		setAttempts(callErr, attempts)
	}

	outcome := outcomeFor(callErr)
	span.SetAttributes(
		attribute.Int64("provider.duration_ms", time.Since(start).Milliseconds()),
		attribute.String("provider.outcome", outcome),
		attribute.Int("provider.attempts", attempts),
	)
	if callErr != nil {
		span.SetStatus(codes.Error, outcome)
		return nil, callErr
	}
	span.SetStatus(codes.Ok, "")
	return resp, nil
}

// attempt performs exactly one HTTP round trip. retryable reports
// whether this failure class is one Do's retry loop is allowed to retry
// (subject to req.Idempotent and the attempt budget) - true for a
// transport-level failure or a 5xx, false for a 4xx or success.
func (c *Client) attempt(ctx context.Context, req Request) (resp *Response, err error, retryable bool) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var bodyReader io.Reader
	if len(req.Body) > 0 {
		bodyReader = bytes.NewReader(req.Body)
	}

	httpReq, buildErr := http.NewRequestWithContext(attemptCtx, req.Method, c.baseURL+req.Path, bodyReader)
	if buildErr != nil {
		// A malformed method/path/URL is a caller programming error, not
		// a transport condition - never retried. Nothing was ever
		// dispatched, so Sent is definitely false.
		return nil, &UnavailableError{Err: buildErr, Sent: false}, false
	}
	if c.authHeaderName != "" {
		httpReq.Header.Set(c.authHeaderName, c.authHeaderValue)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	httpResp, doErr := c.httpClient.Do(httpReq)
	if doErr != nil {
		// requestWasSent distinguishes a dial failure (connection never
		// established - Sent false) from everything else this package can
		// observe from an http.Client.Do failure (a write/read failure
		// after a connection existed, or a mid-flight reset - Sent true,
		// the conservative assumption; see requestWasSent's own doc
		// comment).
		sent := requestWasSent(doErr)
		if errors.Is(doErr, context.DeadlineExceeded) {
			return nil, &TimeoutError{Err: doErr, Sent: sent}, true
		}
		return nil, &UnavailableError{Err: doErr, Sent: sent}, true
	}
	defer func() { _ = httpResp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBodyBytes))
	if readErr != nil {
		// A response was already being received when the read failed, so
		// the request was definitely sent.
		if errors.Is(readErr, context.DeadlineExceeded) {
			return nil, &TimeoutError{Err: readErr, Sent: true}, true
		}
		return nil, &UnavailableError{Err: readErr, Sent: true}, true
	}

	switch {
	case httpResp.StatusCode >= 200 && httpResp.StatusCode < 300:
		return &Response{StatusCode: httpResp.StatusCode, Header: httpResp.Header, Body: body}, nil, false
	case httpResp.StatusCode >= 400 && httpResp.StatusCode < 500:
		// redactCredential (Stage 9 hardening, ADR 0080 Decision 3's own
		// documented P3 gap) runs BEFORE boundResponseBody/
		// boundResponseHeader (Stage 9.1 hardening) - see boundResponseBody's
		// own doc comment for why that order, not the reverse, is what
		// makes redaction and truncation compose correctly.
		return nil, &RejectedError{
			StatusCode: httpResp.StatusCode,
			Body:       boundResponseBody(redactCredential(body, c.authHeaderValue)),
			Header:     boundResponseHeader(redactCredentialHeader(httpResp.Header, c.authHeaderValue)),
		}, false
	case httpResp.StatusCode >= 500:
		// An HTTP response was received, so the provider definitely got
		// the request - Sent is always true here.
		return nil, &UnavailableError{
			StatusCode: httpResp.StatusCode,
			Body:       boundResponseBody(redactCredential(body, c.authHeaderValue)),
			Header:     boundResponseHeader(redactCredentialHeader(httpResp.Header, c.authHeaderValue)),
			Sent:       true,
		}, true
	default:
		// 1xx, and 3xx now that the default *http.Client's CheckRedirect
		// refuses to follow a redirect (see New's own doc comment): a 3xx
		// response here IS the response Do returns to the caller, not a
		// response silently followed to another host first - the caller
		// decides what to do with it (e.g. inspect Location itself, or
		// treat it as a failure). This generic client still has no
		// opinion on informational (1xx) responses for a not-yet-known
		// provider contract - pass those through as well rather than
		// guessing.
		return &Response{StatusCode: httpResp.StatusCode, Header: httpResp.Header, Body: body}, nil, false
	}
}

// redactedPlaceholder replaces every occurrence of the configured
// credential value found in a provider's error response - see
// redactCredential's own doc comment.
const redactedPlaceholder = "[REDACTED-BY-PLATFORM]"

// redactCredential returns a copy of body with every literal occurrence of
// secret replaced by redactedPlaceholder, or body unchanged (same slice,
// no copy) if secret is empty. This closes the gap ADR 0080 Decision 3
// documented and deliberately left unfixed at Stage 8 ("P3 provider
// response body logging"): RejectedError.Body/UnavailableError.Body is
// exactly what a future adapter's own error handling is most likely to log
// verbatim for debugging a rejected/failed call, and a provider that
// reflects a request back in its error body (a surprisingly common vendor
// behavior for a 4xx "here is what you sent us" diagnostic response, or a
// 5xx proxy/gateway error page that echoes request headers) would
// otherwise leak ClientConfig.AuthHeaderValue - the same secret New's
// CheckRedirect hardening (see New's own doc comment) already protects on
// the request path - into every place that error ends up.
//
// This is scoped to the two error-carrying types only (RejectedError,
// UnavailableError), never a successful Response.Body: a caller MUST
// receive an untouched success body to decode it correctly, whereas an
// error body exists purely for diagnostics, where a scrubbed credential
// costs nothing. This is also a genuinely partial mitigation, not a
// guarantee of no leak: it can only scrub the ONE secret this package
// itself knows about (ClientConfig.AuthHeaderValue) - a provider echoing
// some other sensitive value this package has no visibility into (a
// session token issued in an earlier call, a signed URL, a second
// credential a caller passed via Request.Headers) would not be caught
// here. Callers still must not log a RejectedError/UnavailableError's
// Body/Header as an unqualified assumption of safety; this only removes
// the one leak this package's own design could otherwise directly cause.
func redactCredential(body []byte, secret string) []byte {
	if secret == "" || len(body) == 0 || !bytes.Contains(body, []byte(secret)) {
		return body
	}
	return bytes.ReplaceAll(body, []byte(secret), []byte(redactedPlaceholder))
}

// redactCredentialHeader returns a shallow copy of h with every header
// value that contains secret replaced by redactedPlaceholder (the whole
// value, not just the matched substring, since a header value containing a
// credential is itself not useful for diagnostics once scrubbed). Returns
// h unchanged (same map, no copy) if secret is empty. See redactCredential
// for the full rationale; this covers the identical risk for
// RejectedError.Header/UnavailableError.Header (e.g. a gateway/proxy error
// response echoing a request header back, which some do for
// diagnostics).
func redactCredentialHeader(h http.Header, secret string) http.Header {
	if secret == "" || len(h) == 0 {
		return h
	}
	out := make(http.Header, len(h))
	for k, values := range h {
		copied := make([]string, len(values))
		for i, v := range values {
			if strings.Contains(v, secret) {
				copied[i] = redactedPlaceholder
			} else {
				copied[i] = v
			}
		}
		out[k] = copied
	}
	return out
}

// boundResponseBody returns body unchanged if it is already at or under
// maxErrorContentBytes, or a copy truncated to that length with a
// trailing marker (truncationMarkerFmt) noting the truncation and the
// original length otherwise.
//
// Callers MUST apply this AFTER redactCredential, never before: this
// package's own configured credential is matched as a whole-string
// substring, so if the raw body were truncated first, a credential value
// that happened to straddle the cut point would survive as an unmatched,
// un-redacted PREFIX of itself in the retained bytes - redactCredential
// only replaces a fully-intact occurrence of the secret, so a partial one
// left behind by an earlier truncation would leak silently. Running
// redactCredential over the full, untruncated body first (its own
// existing maxResponseBodyBytes wire-read cap already bounds that cost)
// and only then truncating means the secret is always matched against its
// complete form before any of the body is ever discarded.
//
// This function itself has no notion of what is "sensitive" beyond what
// redactCredential already scrubbed - it is a size bound only, not a
// scrubber, and deliberately does no content-sniffing/heuristic detection
// of what "looks like" a secret or PII (see maxErrorContentBytes' own doc
// comment for why that is a deliberate non-goal, not an oversight). A
// bounded Body can still contain a provider-echoed secret or PII fragment
// this package has no way to recognize - callers must not treat a bounded
// Body as safe to log unconditionally, only as "no longer capable of
// growing without bound".
func boundResponseBody(body []byte) []byte {
	if len(body) <= maxErrorContentBytes {
		return body
	}
	marker := fmt.Sprintf(truncationMarkerFmt, len(body))
	out := make([]byte, 0, maxErrorContentBytes+len(marker))
	out = append(out, body[:maxErrorContentBytes]...)
	out = append(out, marker...)
	return out
}

// boundResponseHeader returns h unchanged if every value is already at or
// under maxErrorContentBytes, or a copy with every over-long value
// truncated (identically to boundResponseBody, marker included)
// otherwise. Callers MUST apply this AFTER redactCredentialHeader, for
// the identical straddling-the-cut-point reason boundResponseBody's own
// doc comment explains for Body - see it for the full rationale and its
// documented "size bound, not a scrubber" limits, which apply here
// unchanged.
func boundResponseHeader(h http.Header) http.Header {
	overLong := false
	for _, values := range h {
		for _, v := range values {
			if len(v) > maxErrorContentBytes {
				overLong = true
				break
			}
		}
		if overLong {
			break
		}
	}
	if !overLong {
		return h
	}
	out := make(http.Header, len(h))
	for k, values := range h {
		copied := make([]string, len(values))
		for i, v := range values {
			if len(v) > maxErrorContentBytes {
				copied[i] = v[:maxErrorContentBytes] + fmt.Sprintf(truncationMarkerFmt, len(v))
			} else {
				copied[i] = v
			}
		}
		out[k] = copied
	}
	return out
}

// requestWasSent classifies a transport-level failure returned by
// http.Client.Do as either "definitely never reached the provider" or
// "possibly reached the provider" - see TimeoutError.Sent's doc comment
// for why this distinction matters to a money-moving caller. A dial
// failure (TCP connection never established, surfaced by net/http as a
// *net.OpError with Op "dial", generally wrapped in a *url.Error) is the
// one transport failure this package can confidently say never put any
// bytes on the wire. Every other shape (a write failure after a
// connection existed, a read failure, a mid-flight connection reset,
// anything else) is treated as "possibly sent" - the conservative
// assumption this package deliberately makes for a financial caller
// rather than trying to enumerate every ambiguous case as "not sent".
func requestWasSent(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return false
	}
	return true
}

// wasSent reports err's Sent field if err is a *TimeoutError or
// *UnavailableError produced by attempt, or false otherwise (including
// for a *RejectedError, which is always sent by definition, but is never
// retried so is never the err this helper is called on from Do's retry
// loop).
func wasSent(err error) bool {
	switch e := err.(type) {
	case *TimeoutError:
		return e.Sent
	case *UnavailableError:
		return e.Sent
	}
	return false
}

// sleepBackoff waits a short, fixed-cap exponential backoff before retry
// number n (n=1 is the wait before the second overall attempt), or
// returns early if ctx is done first.
func sleepBackoff(ctx context.Context, n int) {
	d := backoffBase * (1 << uint(n-1))
	if d > backoffCap {
		d = backoffCap
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// setAttempts records the total attempt count on the final error, for
// callers/logs that want to know how much retry work a failed call cost.
func setAttempts(err error, attempts int) {
	switch e := err.(type) {
	case *TimeoutError:
		e.Attempts = attempts
	case *UnavailableError:
		e.Attempts = attempts
	}
}

// outcomeFor maps err to the span outcome attribute value. Deliberately
// does not (and cannot) report "malformed" - that classification only
// exists after a caller calls Response.DecodeJSON, which happens outside
// Do's own span. A caller wanting a "malformed" outcome on its own span
// adds that attribute itself from the error DecodeJSON returns - this
// package does not reopen or mutate an already-ended span to do it (see
// package/Decision 3 doc comment: "don't over-design this").
func outcomeFor(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, ErrProviderTimeout):
		return "timeout"
	case errors.Is(err, ErrProviderRejected):
		return "rejected"
	case errors.Is(err, ErrProviderUnavailable):
		return "unavailable"
	default:
		return "error"
	}
}

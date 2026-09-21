package httpclient

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel error categories every future real adapter's own error mapping
// switches over, instead of handling raw net/http/context errors itself.
// Exactly one of these (or nil) is what Client.Do ever returns; a
// caller-supplied decode step (Response.DecodeJSON) is the only source of
// ErrProviderMalformedResponse, since this package has no opinion on what
// shape a provider's response body should be.
var (
	// ErrProviderTimeout means the configured per-call timeout elapsed
	// before a response was received. Never retried automatically beyond
	// the caller's own idempotent retry budget - see TimeoutError.
	ErrProviderTimeout = errors.New("httpclient: provider request timed out")

	// ErrProviderUnavailable covers both transport-level failures
	// (connection refused/reset, DNS failure, etc.) and a 5xx response
	// after the retry budget (if any) was exhausted. See UnavailableError
	// for which of the two occurred.
	ErrProviderUnavailable = errors.New("httpclient: provider unavailable")

	// ErrProviderRejected means the provider returned a 4xx response.
	// Never retried, regardless of Request.Idempotent - a 4xx is a
	// definite rejection of this exact request, not a transient
	// condition retrying could fix. See RejectedError for the status
	// code/body.
	ErrProviderRejected = errors.New("httpclient: provider rejected request")

	// ErrProviderMalformedResponse means Response.DecodeJSON (or any
	// other caller-supplied decode step) could not parse the response
	// body. This package never inspects a response body's shape itself;
	// only the caller, who knows what shape to expect, can determine
	// this.
	ErrProviderMalformedResponse = errors.New("httpclient: provider response malformed")
)

// TimeoutError is the concrete error Client.Do returns when a call
// (including all attempts, if the request was retried) ultimately failed
// because the configured timeout elapsed - or because Do's own retry
// loop observed the caller's context already done before starting the
// next iteration (see CallerCanceled). Attempts is the total number of
// HTTP attempts made (always 1 for a non-idempotent request, per Stage 8
// §12's rule that a non-idempotent request is never retried under any
// failure class).
type TimeoutError struct {
	Attempts int

	// CallerCanceled is true when this error was produced by Do's retry
	// loop observing ctx.Err() != nil at the top of a retry iteration
	// (before making another attempt), rather than by the per-attempt
	// context.WithTimeout actually elapsing waiting on the provider - see
	// Do's own doc comment. This distinguishes "the platform's own caller
	// cancelled its context, or its own deadline expired" (not a provider
	// health signal at all, and not something the retry budget should be
	// burned re-litigating) from an ordinary provider-side timeout
	// (CallerCanceled false, Err wraps context.DeadlineExceeded from the
	// per-attempt timeout). ErrProviderTimeout is deliberately reused for
	// both cases rather than adding a fifth sentinel - see ADR 0080
	// Decision 3 for why - so a caller that cares about the distinction
	// checks this field, not a different errors.Is target.
	CallerCanceled bool

	// Sent reports whether the request behind this attempt may have
	// reached the provider over the wire - the distinction a
	// money-moving adapter needs between "definitely never reached the
	// provider" (safe to fail fast with no state) and "possibly reached
	// the provider" (must write a pending/unknown-state record and
	// reconcile). False only when: request construction failed (e.g.
	// http.NewRequestWithContext rejected the method/URL - no connection
	// was ever attempted), or the failure was a dial failure (TCP
	// connection never established, detected via *net.OpError{Op:
	// "dial"}). True for everything else this package can observe,
	// including a response-body read failure (the request was sent and a
	// response had already started coming back) and a mid-flight
	// connection reset - the latter is genuinely ambiguous (the request
	// may or may not have been fully processed before the reset), and
	// this package deliberately assumes the conservative "might have been
	// processed" reading in that ambiguous case, since that is the safe
	// assumption for a financial caller. Also true whenever
	// CallerCanceled is true and a prior attempt in the same Do call had
	// already been sent (this field then reflects that prior attempt's
	// own Sent value, not the aborted final iteration, since no new
	// attempt was made).
	Sent bool

	Err error // the underlying context/transport error, for logs only - never logged verbatim by this package itself
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("httpclient: provider request timed out after %d attempt(s): %v", e.Attempts, e.Err)
}

func (e *TimeoutError) Unwrap() error { return ErrProviderTimeout }

// GoString mirrors this codebase's established SEC-4I-C-02 remedy
// (internal/jurisdiction's redacted GoString implementations): Err may
// wrap a *url.Error carrying an unredacted request URL/credential, so
// fmt.Sprintf("%#v", err) must not be allowed to bypass Error()'s own
// safe formatting.
func (e *TimeoutError) GoString() string {
	return fmt.Sprintf("httpclient.TimeoutError{Attempts:%d, CallerCanceled:%v, Sent:%v, Err:<redacted>}", e.Attempts, e.CallerCanceled, e.Sent)
}

// UnavailableError is the concrete error Client.Do returns when a call
// ultimately failed because of a transport-level failure or a 5xx
// response. StatusCode is 0 for a transport-level failure (no HTTP
// response was ever received); otherwise it is the last 5xx status code
// observed. Body is the last response body observed (empty for a
// transport-level failure). Err is the underlying transport error, nil
// for a 5xx.
type UnavailableError struct {
	Attempts   int
	StatusCode int
	// Body/Header are scrubbed of the configured ClientConfig.
	// AuthHeaderValue (if any) before being stored here - see
	// redactCredential's doc comment (client.go) for the exact rule and
	// its documented limits. This does not make Body/Header generically
	// safe to log: a provider may echo a different sensitive value this
	// package has no way to recognize.
	Body   []byte
	Header http.Header // nil for a transport-level failure (no HTTP response was ever received)

	// Sent reports whether the request behind this attempt may have
	// reached the provider over the wire. See TimeoutError.Sent's doc
	// comment for the exact rule this package applies (request
	// construction and dial failures are false; everything else,
	// including a mid-flight connection reset, is true - the
	// conservative assumption for a financial caller). Always true when
	// StatusCode != 0, since an HTTP response was received, which by
	// definition means the provider got the request.
	Sent bool

	Err error
}

func (e *UnavailableError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("httpclient: provider unavailable after %d attempt(s): last status %d", e.Attempts, e.StatusCode)
	}
	return fmt.Sprintf("httpclient: provider unavailable after %d attempt(s): %v", e.Attempts, e.Err)
}

func (e *UnavailableError) Unwrap() error { return ErrProviderUnavailable }

// GoString mirrors TimeoutError.GoString's reasoning: Body/Header are
// scrubbed of the platform's own configured credential only (see
// redactCredential's documented limits) and may still carry a different
// sensitive value a provider echoed - %#v must not bypass that.
func (e *UnavailableError) GoString() string {
	return fmt.Sprintf("httpclient.UnavailableError{Attempts:%d, StatusCode:%d, Sent:%v, Body:<redacted>, Header:<redacted>, Err:<redacted>}", e.Attempts, e.StatusCode, e.Sent)
}

// RejectedError is the concrete error Client.Do returns when a provider
// responds with a 4xx status. It is never the product of a retry - a 4xx
// is classified and returned on the first attempt that observes it,
// regardless of Request.Idempotent or ClientConfig.MaxRetries.
type RejectedError struct {
	StatusCode int
	// Body/Header are scrubbed of the configured ClientConfig.
	// AuthHeaderValue (if any) before being stored here - see
	// redactCredential's doc comment (client.go) for the exact rule and
	// its documented limits. This does not make Body/Header generically
	// safe to log: a provider may echo a different sensitive value this
	// package has no way to recognize.
	Body   []byte
	Header http.Header
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("httpclient: provider rejected request: status %d", e.StatusCode)
}

func (e *RejectedError) Unwrap() error { return ErrProviderRejected }

// GoString mirrors TimeoutError.GoString's reasoning - see
// UnavailableError.GoString for the identical Body/Header caveat.
func (e *RejectedError) GoString() string {
	return fmt.Sprintf("httpclient.RejectedError{StatusCode:%d, Body:<redacted>, Header:<redacted>}", e.StatusCode)
}

// MalformedResponseError is the concrete error Response.DecodeJSON
// returns when the response body cannot be decoded into the
// caller-supplied shape. It unwraps to both ErrProviderMalformedResponse
// (for switch-style classification) and the underlying decode error (for
// diagnostics), via the multi-error Unwrap form (errors.Is/As handle
// both).
type MalformedResponseError struct {
	Err error
}

func (e *MalformedResponseError) Error() string {
	return fmt.Sprintf("httpclient: provider response malformed: %v", e.Err)
}

func (e *MalformedResponseError) Unwrap() []error {
	return []error{ErrProviderMalformedResponse, e.Err}
}

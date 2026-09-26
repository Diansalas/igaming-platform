// This file is a provider-BOUNDARY / contract-test suite for the generic
// internal/providers/httpclient.Client, per docs/decisions/0080-provider-
// integration-readiness-without-external-contracts.md Decision 3. It
// proves Client's own retry/timeout/classification behavior against
// local httptest.Server fixtures built by the
// internal/providers/httpclient/conformance package.
//
// These are explicitly NOT integration tests of any external "Dummy
// Sportsbook"/"Dummy Casino" API or any other real vendor. No such API's
// documentation, base URL, or credentials exist in this environment (see
// ADR 0080's Context section) - every server used below is a synthetic
// local fixture simulating a generic HTTP failure/success mode, never a
// fake of a real, specific provider's actual documented contract.
package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/providers/httpclient/conformance"
)

func TestDo_Success(t *testing.T) {
	srv := conformance.NewSuccessServer(t, []byte(`{"status":"ok"}`), 200)

	c := New(ClientConfig{
		ProviderName: "test-provider",
		BaseURL:      srv.URL,
		Timeout:      time.Second,
	})

	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/anything", Operation: "test_op"})
	if err != nil {
		t.Fatalf("Do returned unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}

	var decoded struct {
		Status string `json:"status"`
	}
	if err := resp.DecodeJSON(&decoded); err != nil {
		t.Fatalf("DecodeJSON returned unexpected error: %v", err)
	}
	if decoded.Status != "ok" {
		t.Fatalf("decoded.Status = %q, want %q", decoded.Status, "ok")
	}
}

func TestDo_Timeout(t *testing.T) {
	srv := conformance.NewTimeoutServer(t, 300*time.Millisecond)

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    30 * time.Millisecond,
		MaxRetries: 2,
	})

	// Non-idempotent: must never retry, so exactly 1 attempt.
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op", Idempotent: false})
	if !errors.Is(err, ErrProviderTimeout) {
		t.Fatalf("err = %v, want ErrProviderTimeout", err)
	}
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("errors.As(err, *TimeoutError) failed, err = %v", err)
	}
	if timeoutErr.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1 (non-idempotent must never retry)", timeoutErr.Attempts)
	}
}

func TestDo_Timeout_RetriedWhenIdempotent(t *testing.T) {
	srv := conformance.NewTimeoutServer(t, 300*time.Millisecond)

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    30 * time.Millisecond,
		MaxRetries: 2,
	})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op", Idempotent: true})
	if !errors.Is(err, ErrProviderTimeout) {
		t.Fatalf("err = %v, want ErrProviderTimeout", err)
	}
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("errors.As(err, *TimeoutError) failed, err = %v", err)
	}
	if timeoutErr.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3 (1 initial + 2 retries)", timeoutErr.Attempts)
	}
}

func TestDo_TransportFailure_NonIdempotentNeverRetried(t *testing.T) {
	srv := conformance.NewTransportFailureServer(t)

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    time.Second,
		MaxRetries: 3,
	})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op", Idempotent: false})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if unavailableErr.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1 (non-idempotent must never retry)", unavailableErr.Attempts)
	}
	if unavailableErr.StatusCode != 0 {
		t.Fatalf("StatusCode = %d, want 0 (no HTTP response was ever received)", unavailableErr.StatusCode)
	}
}

func TestDo_TransportFailure_RetriedWhenIdempotent(t *testing.T) {
	srv := conformance.NewTransportFailureServer(t)

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    time.Second,
		MaxRetries: 3,
	})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op", Idempotent: true})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if unavailableErr.Attempts != 4 {
		t.Fatalf("Attempts = %d, want 4 (1 initial + 3 retries)", unavailableErr.Attempts)
	}
}

func TestDo_Rejected4xx_NeverRetriedEvenWhenIdempotent(t *testing.T) {
	srv := conformance.NewStatusServer(t, 422, []byte(`{"error":"invalid"}`))

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    time.Second,
		MaxRetries: 5,
	})

	resp, err := c.Do(context.Background(), Request{Method: "POST", Path: "/", Operation: "test_op", Idempotent: true})
	if resp != nil {
		t.Fatalf("resp = %+v, want nil on error", resp)
	}
	if !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("err = %v, want ErrProviderRejected", err)
	}
	var rejectedErr *RejectedError
	if !errors.As(err, &rejectedErr) {
		t.Fatalf("errors.As(err, *RejectedError) failed, err = %v", err)
	}
	if rejectedErr.StatusCode != 422 {
		t.Fatalf("StatusCode = %d, want 422", rejectedErr.StatusCode)
	}

	// A 4xx must be classified and returned on the very first attempt -
	// prove the server was only ever called once, despite Idempotent:
	// true and MaxRetries: 5.
	if got := rejectedErr.Header.Get("X-Conformance-Call-Count"); got != "1" {
		t.Fatalf("X-Conformance-Call-Count = %q, want %q (4xx must never be retried)", got, "1")
	}
}

func TestDo_5xx_RetriedThenSucceeds(t *testing.T) {
	srv := conformance.NewFlakyServer(t, 2, 200)

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    time.Second,
		MaxRetries: 2,
	})

	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op", Idempotent: true})
	if err != nil {
		t.Fatalf("Do returned unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Conformance-Call-Count"); got != "3" {
		t.Fatalf("X-Conformance-Call-Count = %q, want %q (1 initial + 2 retries)", got, "3")
	}
}

func TestDo_5xx_ExhaustedRetries(t *testing.T) {
	srv := conformance.NewStatusServer(t, 503, []byte(`{"error":"down"}`))

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    time.Second,
		MaxRetries: 2,
	})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op", Idempotent: true})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if unavailableErr.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3 (1 initial + 2 retries)", unavailableErr.Attempts)
	}
	if unavailableErr.StatusCode != 503 {
		t.Fatalf("StatusCode = %d, want 503", unavailableErr.StatusCode)
	}
}

func TestDo_5xx_NonIdempotentNeverRetried(t *testing.T) {
	srv := conformance.NewStatusServer(t, 503, []byte(`{"error":"down"}`))

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    time.Second,
		MaxRetries: 2,
	})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op", Idempotent: false})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if unavailableErr.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1 (non-idempotent must never retry)", unavailableErr.Attempts)
	}
}

func TestDo_MalformedResponse(t *testing.T) {
	srv := conformance.NewMalformedBodyServer(t, []byte(`not-json{`))

	c := New(ClientConfig{
		BaseURL: srv.URL,
		Timeout: time.Second,
	})

	resp, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op"})
	if err != nil {
		t.Fatalf("Do returned unexpected error: %v (a malformed body is a decode-time failure, not a transport/status failure)", err)
	}

	var decoded struct{ X string }
	decodeErr := resp.DecodeJSON(&decoded)
	if decodeErr == nil {
		t.Fatal("DecodeJSON returned nil error, want a malformed-response error")
	}
	if !errors.Is(decodeErr, ErrProviderMalformedResponse) {
		t.Fatalf("decodeErr = %v, want ErrProviderMalformedResponse", decodeErr)
	}
	var malformedErr *MalformedResponseError
	if !errors.As(decodeErr, &malformedErr) {
		t.Fatalf("errors.As(decodeErr, *MalformedResponseError) failed, decodeErr = %v", decodeErr)
	}
	if malformedErr.Err == nil {
		t.Fatal("MalformedResponseError.Err is nil, want the underlying json error")
	}
}

// TestDo_DuplicateAck_CallerRetryIsNotDeduped documents and proves a
// deliberate boundary: httpclient.Client never deduplicates calls on the
// caller's behalf. If a caller-level retry (e.g. after losing track of
// whether an earlier Do call actually succeeded) issues the same logical
// request twice, the provider sees two calls and acks both - proving why
// domain-level idempotency enforcement (a unique constraint on e.g.
// (provider_id, provider_tx_id), per CLAUDE.md's ledger rules) is what
// actually prevents a double financial effect, not this transport
// package.
func TestDo_DuplicateAck_CallerRetryIsNotDeduped(t *testing.T) {
	srv, counter := conformance.NewDuplicateAckServer(t)

	c := New(ClientConfig{
		BaseURL: srv.URL,
		Timeout: time.Second,
	})

	req := Request{Method: "POST", Path: "/settle", Operation: "settle", Idempotent: true}

	resp1, err1 := c.Do(context.Background(), req)
	if err1 != nil {
		t.Fatalf("first Do returned unexpected error: %v", err1)
	}
	if resp1.StatusCode != 200 {
		t.Fatalf("first StatusCode = %d, want 200", resp1.StatusCode)
	}

	// Simulate the caller itself retrying (not Client's own internal
	// retry loop, which never fires here since the first call already
	// succeeded) because it lost track of the outcome.
	resp2, err2 := c.Do(context.Background(), req)
	if err2 != nil {
		t.Fatalf("second Do returned unexpected error: %v", err2)
	}
	if resp2.StatusCode != 200 {
		t.Fatalf("second StatusCode = %d, want 200", resp2.StatusCode)
	}

	if got := counter.Count(); got != 2 {
		t.Fatalf("server call count = %d, want 2 (httpclient.Client does not dedupe caller-level retries)", got)
	}
}

// TestDo_AuthHeaderAppliedUnderConfiguredNameOnly proves the configured
// auth header is set on the outbound request under exactly the
// caller-configured name/value - not duplicated under some other header
// this package might otherwise be tempted to also set (e.g.
// "Authorization").
// TestDo_DoesNotFollowRedirect_CredentialNeverReachesTarget is a
// permanent regression test for the P1 finding (ADR 0080 Decision 3):
// without an explicit no-follow CheckRedirect policy, Go's default
// redirect behavior forwards any header that isn't
// Authorization/WWW-Authenticate/Cookie/Cookie2 verbatim to a
// cross-domain redirect target - exactly the shape of
// a per-call Authenticator header, which is deliberately an arbitrary
// vendor-defined header name (e.g. a bespoke X-API-Key). This uses two
// distinct httptest.Servers (distinct hosts/ports) so a redirect from one
// to the other is a genuine opportunity for Go's default client to copy
// the header - and proves the actual property this package relies on
// ("a redirect is never followed at all", via http.ErrUseLastResponse),
// which is strictly stronger than "the header is stripped on a
// cross-domain redirect" and holds regardless of same-host-or-not.
func TestDo_DoesNotFollowRedirect_CredentialNeverReachesTarget(t *testing.T) {
	const secretValue = "super-secret-value"

	var targetCalled bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalled = true
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("redirect target received X-Api-Key header %q, want it never delivered", got)
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/landed", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	c := New(ClientConfig{
		BaseURL: origin.URL,
		Timeout: time.Second,
	})

	resp, err := c.Do(context.Background(), Request{Auth: NewHeaderAuthenticator("X-Api-Key", secretValue), Method: "GET", Path: "/", Operation: "test_op"})
	if err != nil {
		t.Fatalf("Do returned unexpected error: %v", err)
	}
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		t.Fatalf("StatusCode = %d, want a 3xx (the redirect response itself, never followed)", resp.StatusCode)
	}
	if targetCalled {
		t.Fatal("redirect target received a request at all, want the redirect to never be followed")
	}
}

// TestDo_ContextAlreadyCanceled_ShortCircuitsImmediately is a permanent
// regression test for the P2 finding (ADR 0080 Decision 3): the retry
// loop must check ctx.Err() before every iteration and short-circuit
// immediately rather than burning the entire retry budget near-instantly
// and misclassifying the outcome as a provider-health signal. The server
// deliberately would block far longer than the test's own timeout budget
// if Do ever actually attempted a request against it, so a slow test run
// here would itself prove the bug is back.
func TestDo_ContextAlreadyCanceled_ShortCircuitsImmediately(t *testing.T) {
	srv := conformance.NewTimeoutServer(t, 5*time.Second)

	c := New(ClientConfig{
		BaseURL:    srv.URL,
		Timeout:    time.Second,
		MaxRetries: 5,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	resp, err := c.Do(ctx, Request{Method: "GET", Path: "/", Operation: "test_op", Idempotent: true})
	elapsed := time.Since(start)

	if resp != nil {
		t.Fatalf("resp = %+v, want nil on error", resp)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Do took %v, want it to return promptly instead of burning the retry budget", elapsed)
	}
	if !errors.Is(err, ErrProviderTimeout) {
		t.Fatalf("err = %v, want ErrProviderTimeout", err)
	}
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("errors.As(err, *TimeoutError) failed, err = %v", err)
	}
	if !timeoutErr.CallerCanceled {
		t.Fatal("CallerCanceled = false, want true (ctx was already canceled before Do could make any attempt)")
	}
	if timeoutErr.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1 (must short-circuit before making any attempt, not burn the retry budget)", timeoutErr.Attempts)
	}
}

// TestDo_Sent_RequestConstructionFailure proves the Sent=false rule for
// a request-construction failure (never even attempted a network
// connection).
func TestDo_Sent_RequestConstructionFailure(t *testing.T) {
	c := New(ClientConfig{
		BaseURL: "http://unused.invalid",
		Timeout: time.Second,
	})

	_, err := c.Do(context.Background(), Request{Method: "BAD METHOD", Path: "/", Operation: "test_op"})
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if unavailableErr.Sent {
		t.Fatal("Sent = true, want false (request construction failed before any network attempt was made)")
	}
}

// TestDo_Sent_DialFailureNeverSent proves the Sent=false rule for a dial
// failure (TCP connection never established).
func TestDo_Sent_DialFailureNeverSent(t *testing.T) {
	srv := conformance.NewTransportFailureServer(t)

	c := New(ClientConfig{BaseURL: srv.URL, Timeout: time.Second})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op"})
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if unavailableErr.Sent {
		t.Fatal("Sent = true, want false (a dial failure means the connection was never established)")
	}
}

// TestDo_Sent_5xxAlwaysTrue proves the Sent=true rule whenever an HTTP
// response (even a 5xx) was actually received - the provider definitely
// got the request.
func TestDo_Sent_5xxAlwaysTrue(t *testing.T) {
	srv := conformance.NewStatusServer(t, 503, []byte(`{"error":"down"}`))

	c := New(ClientConfig{BaseURL: srv.URL, Timeout: time.Second})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op"})
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if !unavailableErr.Sent {
		t.Fatal("Sent = false, want true (an HTTP response was received, so the provider got the request)")
	}
}

func TestDo_AuthHeaderAppliedUnderConfiguredNameOnly(t *testing.T) {
	const secretValue = "super-secret-value"

	var gotConfiguredHeader, gotAuthorizationHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotConfiguredHeader = r.Header.Get("X-Api-Key")
		gotAuthorizationHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := New(ClientConfig{
		BaseURL: srv.URL,
		Timeout: time.Second,
	})

	_, err := c.Do(context.Background(), Request{Auth: NewHeaderAuthenticator("X-Api-Key", secretValue), Method: "GET", Path: "/", Operation: "test_op"})
	if err != nil {
		t.Fatalf("Do returned unexpected error: %v", err)
	}
	if gotConfiguredHeader != secretValue {
		t.Fatalf("X-Api-Key header = %q, want %q", gotConfiguredHeader, secretValue)
	}
	if gotAuthorizationHeader != "" {
		t.Fatalf("Authorization header = %q, want empty (auth header must only be set under the configured name)", gotAuthorizationHeader)
	}
}

// TestDo_Rejected4xx_EchoedCredentialIsRedacted is a permanent regression
// test for the P3 finding ADR 0080 Decision 3 documented but deliberately
// left unfixed at Stage 8 ("provider response body logging" - a provider
// that reflects a request back in its 4xx diagnostic body/headers would
// otherwise leak ClientConfig.AuthHeaderValue into RejectedError.Body/
// .Header, exactly the field a future adapter's own error-path logging is
// most likely to include verbatim). The fake server here simulates a
// vendor "here is what you sent us" diagnostic response, echoing the
// caller's own auth header value back in both the body and a response
// header, to prove neither ever reaches RejectedError un-redacted.
func TestDo_Rejected4xx_EchoedCredentialIsRedacted(t *testing.T) {
	const secretValue = "super-secret-api-key-value"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Received-Auth", r.Header.Get("X-Api-Key"))
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"invalid request","you_sent":{"X-Api-Key":"` + secretValue + `"}}`))
	}))
	t.Cleanup(srv.Close)

	c := New(ClientConfig{
		BaseURL: srv.URL,
		Timeout: time.Second,
	})

	_, err := c.Do(context.Background(), Request{Auth: NewHeaderAuthenticator("X-Api-Key", secretValue), Method: "POST", Path: "/", Operation: "test_op"})
	var rejectedErr *RejectedError
	if !errors.As(err, &rejectedErr) {
		t.Fatalf("errors.As(err, *RejectedError) failed, err = %v", err)
	}
	if bytesContains(rejectedErr.Body, secretValue) {
		t.Fatalf("RejectedError.Body contains the raw credential: %s", rejectedErr.Body)
	}
	if got := rejectedErr.Header.Get("X-Echo-Received-Auth"); got == secretValue {
		t.Fatalf("RejectedError.Header[X-Echo-Received-Auth] contains the raw credential: %q", got)
	}
}

// TestDo_5xx_EchoedCredentialIsRedacted mirrors
// TestDo_Rejected4xx_EchoedCredentialIsRedacted for UnavailableError's 5xx
// path (e.g. a gateway/proxy error page echoing request headers back).
func TestDo_5xx_EchoedCredentialIsRedacted(t *testing.T) {
	const secretValue = "super-secret-api-key-value"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Received-Auth", r.Header.Get("X-Api-Key"))
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"error":"bad gateway","request_headers":{"X-Api-Key":"` + secretValue + `"}}`))
	}))
	t.Cleanup(srv.Close)

	c := New(ClientConfig{
		BaseURL: srv.URL,
		Timeout: time.Second,
	})

	_, err := c.Do(context.Background(), Request{Auth: NewHeaderAuthenticator("X-Api-Key", secretValue), Method: "GET", Path: "/", Operation: "test_op", Idempotent: false})
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if bytesContains(unavailableErr.Body, secretValue) {
		t.Fatalf("UnavailableError.Body contains the raw credential: %s", unavailableErr.Body)
	}
	if got := unavailableErr.Header.Get("X-Echo-Received-Auth"); got == secretValue {
		t.Fatalf("UnavailableError.Header[X-Echo-Received-Auth] contains the raw credential: %q", got)
	}
}

// TestGoStringRedaction_NeverLeaksCredentialOrBodyViaSharpV is the
// Stage 9 architect-review fix for the residual gap in
// TestDo_Rejected4xx_EchoedCredentialIsRedacted /
// TestDo_5xx_EchoedCredentialIsRedacted: scrubbing Body/Header at
// construction time does not stop fmt.Sprintf("%#v", err) from dumping
// every field verbatim via Go's default GoStringer-less formatting,
// bypassing Error() entirely - the identical bypass class this
// codebase's own SEC-4I-C-02 fix closed for internal/jurisdiction's
// types. Mirrors that fix (a redacting GoString method) on all three
// httpclient error types that carry a credential or a provider-supplied
// body.
func TestGoStringRedaction_NeverLeaksCredentialOrBodyViaSharpV(t *testing.T) {
	const secretValue = "super-secret-api-key-value"
	const echoedProviderSecret = "provider-side-sensitive-value"

	rejected := &RejectedError{StatusCode: 422, Body: []byte(echoedProviderSecret), Header: http.Header{"X-Echo": []string{secretValue}}}
	if got := fmt.Sprintf("%#v", rejected); bytesContains([]byte(got), secretValue) || bytesContains([]byte(got), echoedProviderSecret) {
		t.Fatalf("RejectedError.GoString leaked a secret: %s", got)
	}

	unavailable := &UnavailableError{Attempts: 2, StatusCode: 502, Body: []byte(echoedProviderSecret), Header: http.Header{"X-Echo": []string{secretValue}}, Err: fmt.Errorf("dial tcp %s:443: connect: %s", secretValue, "connection refused")}
	if got := fmt.Sprintf("%#v", unavailable); bytesContains([]byte(got), secretValue) || bytesContains([]byte(got), echoedProviderSecret) {
		t.Fatalf("UnavailableError.GoString leaked a secret: %s", got)
	}

	timeout := &TimeoutError{Attempts: 1, Err: fmt.Errorf("context deadline exceeded fetching %s?api_key=%s", "https://provider.example/v1", secretValue)}
	if got := fmt.Sprintf("%#v", timeout); bytesContains([]byte(got), secretValue) {
		t.Fatalf("TimeoutError.GoString leaked a secret: %s", got)
	}
}

func bytesContains(body []byte, substr string) bool {
	return strings.Contains(string(body), substr)
}

// TestBoundResponseBody_UnderBoundUnchanged proves boundResponseBody is a
// true no-op (same content, no truncation marker) when body is already
// within maxErrorContentBytes - the common case, since a real diagnostic
// body is ordinarily small.
func TestBoundResponseBody_UnderBoundUnchanged(t *testing.T) {
	body := []byte(`{"error":"invalid request"}`)
	got := boundResponseBody(body)
	if string(got) != string(body) {
		t.Fatalf("boundResponseBody(%q) = %q, want unchanged", body, got)
	}
}

// TestBoundResponseBody_OverBoundTruncated is the Stage 9.1 permanent
// regression test for the residual gap ADR 0080 Decision 3 / Stage 9
// addendum disclosed and left open: an oversized vendor-echoed body must
// never be retained past maxErrorContentBytes.
func TestBoundResponseBody_OverBoundTruncated(t *testing.T) {
	body := bytes.Repeat([]byte("x"), maxErrorContentBytes+5000)
	got := boundResponseBody(body)
	if len(got) <= maxErrorContentBytes {
		t.Fatalf("boundResponseBody result length = %d, want it to include the truncation marker (> %d)", len(got), maxErrorContentBytes)
	}
	if !bytes.HasPrefix(got, body[:maxErrorContentBytes]) {
		t.Fatal("boundResponseBody result does not start with the first maxErrorContentBytes bytes of the original body")
	}
	wantMarker := fmt.Sprintf(truncationMarkerFmt, len(body))
	if !strings.HasSuffix(string(got), wantMarker) {
		t.Fatalf("boundResponseBody result does not end with the expected truncation marker %q, got %q", wantMarker, got)
	}
}

// TestBoundResponseHeader_OverBoundTruncated mirrors
// TestBoundResponseBody_OverBoundTruncated for header values.
func TestBoundResponseHeader_OverBoundTruncated(t *testing.T) {
	longValue := strings.Repeat("y", maxErrorContentBytes+100)
	h := http.Header{"X-Diagnostic": []string{longValue}, "X-Short": []string{"fine"}}

	got := boundResponseHeader(h)

	gotLong := got.Get("X-Diagnostic")
	if len(gotLong) <= maxErrorContentBytes {
		t.Fatalf("X-Diagnostic length = %d, want it to include the truncation marker (> %d)", len(gotLong), maxErrorContentBytes)
	}
	wantMarker := fmt.Sprintf(truncationMarkerFmt, len(longValue))
	if !strings.HasSuffix(gotLong, wantMarker) {
		t.Fatalf("X-Diagnostic does not end with the expected truncation marker %q, got %q", wantMarker, gotLong)
	}
	if got.Get("X-Short") != "fine" {
		t.Fatalf("X-Short = %q, want unchanged %q", got.Get("X-Short"), "fine")
	}
}

// TestDo_Rejected4xx_LargeBodyIsTruncated proves Client.Do itself (not
// just the boundResponseBody unit) bounds an oversized 4xx diagnostic
// body before it ever reaches RejectedError.
func TestDo_Rejected4xx_LargeBodyIsTruncated(t *testing.T) {
	hugeBody := bytes.Repeat([]byte("b"), maxErrorContentBytes*3)
	srv := conformance.NewStatusServer(t, 422, hugeBody)

	c := New(ClientConfig{BaseURL: srv.URL, Timeout: time.Second})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op"})
	var rejectedErr *RejectedError
	if !errors.As(err, &rejectedErr) {
		t.Fatalf("errors.As(err, *RejectedError) failed, err = %v", err)
	}
	if len(rejectedErr.Body) >= len(hugeBody) {
		t.Fatalf("RejectedError.Body length = %d, want it bounded well under the original %d bytes", len(rejectedErr.Body), len(hugeBody))
	}
	if len(rejectedErr.Body) > maxErrorContentBytes+100 {
		t.Fatalf("RejectedError.Body length = %d, want it within maxErrorContentBytes plus a short marker", len(rejectedErr.Body))
	}
}

// TestDo_5xx_LargeBodyIsTruncated mirrors
// TestDo_Rejected4xx_LargeBodyIsTruncated for UnavailableError's 5xx path.
func TestDo_5xx_LargeBodyIsTruncated(t *testing.T) {
	hugeBody := bytes.Repeat([]byte("c"), maxErrorContentBytes*3)
	srv := conformance.NewStatusServer(t, 502, hugeBody)

	c := New(ClientConfig{BaseURL: srv.URL, Timeout: time.Second})

	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Operation: "test_op"})
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("errors.As(err, *UnavailableError) failed, err = %v", err)
	}
	if len(unavailableErr.Body) >= len(hugeBody) {
		t.Fatalf("UnavailableError.Body length = %d, want it bounded well under the original %d bytes", len(unavailableErr.Body), len(hugeBody))
	}
	if len(unavailableErr.Body) > maxErrorContentBytes+100 {
		t.Fatalf("UnavailableError.Body length = %d, want it within maxErrorContentBytes plus a short marker", len(unavailableErr.Body))
	}
}

// TestDo_Rejected4xx_TruncationComposesWithRedaction_SecretStraddlesBoundary
// is the composition proof the residual-gap fix requires: it constructs a
// body where the configured credential straddles the maxErrorContentBytes
// cut point (part of the secret's bytes fall before the cut, part after),
// so a WRONG implementation that truncated before redacting would leave
// the surviving prefix of the raw credential behind, unredacted, in
// RejectedError.Body. The actual implementation (redact, then truncate -
// see boundResponseBody's own doc comment) must never do that: the full
// credential is matched and replaced before any byte is discarded, so no
// fragment of it can survive truncation, regardless of where the vendor's
// body happened to place it.
func TestDo_Rejected4xx_TruncationComposesWithRedaction_SecretStraddlesBoundary(t *testing.T) {
	const secretValue = "super-secret-api-key-value-0123456789" // 38 bytes

	// Padding placed so the RAW secret straddles maxErrorContentBytes (30
	// bytes before the cut, 8 after, secret is 38 bytes long) - but its
	// redacted-placeholder replacement (shorter than the raw secret) lands
	// entirely before the cut, so the truncation marker this test checks
	// for below isn't itself coincidentally split by the same cut.
	leadingPadding := bytes.Repeat([]byte("A"), maxErrorContentBytes-30)
	trailingPadding := bytes.Repeat([]byte("Z"), 2000)

	var body []byte
	body = append(body, leadingPadding...)
	body = append(body, []byte(secretValue)...)
	body = append(body, trailingPadding...)

	srv := conformance.NewStatusServer(t, 422, body)

	c := New(ClientConfig{
		BaseURL: srv.URL,
		Timeout: time.Second,
	})

	_, err := c.Do(context.Background(), Request{Auth: NewHeaderAuthenticator("X-Api-Key", secretValue), Method: "GET", Path: "/", Operation: "test_op"})
	var rejectedErr *RejectedError
	if !errors.As(err, &rejectedErr) {
		t.Fatalf("errors.As(err, *RejectedError) failed, err = %v", err)
	}

	// Truncation must still have happened.
	if len(rejectedErr.Body) > maxErrorContentBytes+200 {
		t.Fatalf("RejectedError.Body length = %d, want it bounded", len(rejectedErr.Body))
	}
	// The full secret must never appear.
	if bytesContains(rejectedErr.Body, secretValue) {
		t.Fatalf("RejectedError.Body contains the raw credential: %s", rejectedErr.Body)
	}
	// Nor may any meaningfully-long prefix of it survive as an unredacted
	// fragment - the exact failure mode a truncate-then-redact ordering
	// would produce.
	if leakedPrefix := secretValue[:10]; bytesContains(rejectedErr.Body, leakedPrefix) {
		t.Fatalf("RejectedError.Body contains an unredacted fragment of the credential (%q): %s", leakedPrefix, rejectedErr.Body)
	}
	// Redaction must have actually run (proves this isn't passing only
	// because the secret got trimmed away by coincidence).
	if !bytesContains(rejectedErr.Body, redactedPlaceholder) {
		t.Fatalf("RejectedError.Body does not contain the redaction placeholder, want %q present: %s", redactedPlaceholder, rejectedErr.Body)
	}
}

// TestGoStringRedaction_StillFullyElidesOversizedContent extends
// TestGoStringRedaction_NeverLeaksCredentialOrBodyViaSharpV with
// larger-than-bound payloads, proving GoString's blanket elision (a fixed
// placeholder, not a bounded rendering) makes the maxErrorContentBytes
// truncation moot for %#v specifically - it never had any content to
// bound in the first place - across all three error types.
func TestGoStringRedaction_StillFullyElidesOversizedContent(t *testing.T) {
	const secretValue = "super-secret-api-key-value"
	hugeEchoed := strings.Repeat("provider-side-sensitive-value-", 1000) // far over maxErrorContentBytes

	rejected := &RejectedError{StatusCode: 422, Body: []byte(hugeEchoed), Header: http.Header{"X-Echo": []string{secretValue}}}
	if got := fmt.Sprintf("%#v", rejected); bytesContains([]byte(got), secretValue) || bytesContains([]byte(got), hugeEchoed) || len(got) > 500 {
		t.Fatalf("RejectedError.GoString leaked or bloated on oversized content: len=%d", len(got))
	}

	unavailable := &UnavailableError{Attempts: 2, StatusCode: 502, Body: []byte(hugeEchoed), Header: http.Header{"X-Echo": []string{secretValue}}, Err: fmt.Errorf("dial tcp: %s", secretValue)}
	if got := fmt.Sprintf("%#v", unavailable); bytesContains([]byte(got), secretValue) || bytesContains([]byte(got), hugeEchoed) || len(got) > 500 {
		t.Fatalf("UnavailableError.GoString leaked or bloated on oversized content: len=%d", len(got))
	}

	timeout := &TimeoutError{Attempts: 1, Err: fmt.Errorf("context deadline exceeded: %s", strings.Repeat(secretValue, 1000))}
	if got := fmt.Sprintf("%#v", timeout); bytesContains([]byte(got), secretValue) || len(got) > 500 {
		t.Fatalf("TimeoutError.GoString leaked or bloated on an oversized wrapped error: len=%d", len(got))
	}
}

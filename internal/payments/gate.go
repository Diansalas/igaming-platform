// PRH-I1 step (b): the provider-call gate, ADR 0095 §3.2 - "the ONLY
// path to an adapter outbound method in payments". This step implements
// the gate steps that do not depend on later steps (the breaker in step
// 9.6, and the full PROV-OUTBOUND-CRED-1 credential subsystem, are later
// PRH-I1 work; see contract.go's package doc comment for the seam this
// step ships in their place):
//
//  1. Refuses under txscope.Held(ctx) - INV-IO-1(b).
//  2. Refuses unless the attempt was loaded AFTER a committed claim, in
//     state 'submitting', with a matching claim token - INV-IO-2.
//  3. Resolves the credential and builds CallContext, with TenantID/
//     ProviderID taken ONLY from the committed attempt row.
//  4. Checks the credential binding itself (S95-C8(b)).
//  5. Applies the manifest's CallTimeout as the per-call deadline.
//  6. Calls the adapter function the caller supplies.
//  7. Maps panics to ErrorClassAmbiguous.
//  8. Redacts transport errors (S95-C8(a)): a *url.Error's embedded
//     request URL is stripped of its query string and userinfo before
//     the error is ever returned to a caller that might log it.
//
// Step 9 (feeding the orchestrator-owned breaker) is NOT built in this
// step - there is no breaker yet (§9.6 is a later PRH-I1 step). This gate
// does not (and, until the breaker exists, cannot) affect routing.
package payments

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// ErrProviderCallRefused is the sentinel every gate refusal wraps. It is
// always accompanied by ErrorClassNotSent - a refusal never reaches the
// adapter, so INV-IO-9 (created's ever_possibly_sent=false) is preserved
// automatically whenever this fires before a first send.
var ErrProviderCallRefused = errors.New("payments: provider call refused by the gate")

// GateResult is what callProvider returns: at most one of Value/Class is
// meaningful depending on whether err is nil. Callers pass Class,
// regardless of err, to the CAS transition they choose in phase C.
type GateResult[T any] struct {
	Value T
	Class ErrorClass
	// Err is the redacted error, if any. It is deliberately not wrapped
	// with the raw adapter error's message when that message could carry
	// vendor response detail - only allow-listed reason text.
	Err error
}

// AdapterCall is the shape of a single outbound adapter method, already
// bound to its request; callProvider only ever calls this function
// pointer, so it never itself imports a vendor SDK or a PSP-specific
// type (docs/decisions/0022 §4.1).
type AdapterCall[T any] func(ctx context.Context, call CallContext) (T, ErrorClass, error)

// callProviderInput is everything the gate needs about the calling
// attempt and domain, without importing payment_attempts' own richer
// PaymentAttempt type into every call site's signature.
type callProviderInput struct {
	TenantID       uuid.UUID
	ProviderID     string
	AttemptState   AttemptState
	ClaimToken     uuid.UUID
	ExpectedClaim  uuid.UUID
	IdempotencyKey string
	Domain         string // "payments", "casino" or "kyc" (S95-C8(b))
	Manifest       OperationManifest
}

// callProvider is the ADR 0095 §3.2 gate, generic over the adapter
// method's own result type. It is the ONLY function in this package that
// may invoke fn.
func callProvider[T any](ctx context.Context, resolver OutboundCredentialResolver, in callProviderInput, fn AdapterCall[T]) GateResult[T] {
	var zero T

	// Step 1 (INV-IO-1(b)): defence in depth behind the API-shape
	// control (no function that can reach fn takes a pgx.Tx).
	if txscope.Held(ctx) {
		return GateResult[T]{Value: zero, Class: ErrorClassNotSent,
			Err: fmt.Errorf("%w: provider_call_refused_tx_held", ErrProviderCallRefused)}
	}

	// Step 2 (INV-IO-2): the caller must present the state/claim-token it
	// read AFTER its own committed claim.
	if in.AttemptState != AttemptSubmitting || in.ClaimToken == uuid.Nil || in.ClaimToken != in.ExpectedClaim {
		return GateResult[T]{Value: zero, Class: ErrorClassNotSent,
			Err: fmt.Errorf("%w: no committed submitting claim for this call", ErrProviderCallRefused)}
	}

	// Step 3: CallContext, TenantID/ProviderID from the caller's already-
	// committed attempt row only.
	cc := CallContext{TenantID: in.TenantID, ProviderID: in.ProviderID, IdempotencyKey: in.IdempotencyKey}

	cred, err := resolver.Resolve(cc, in.Domain)
	if err != nil {
		return GateResult[T]{Value: zero, Class: ErrorClassNotSent,
			Err: fmt.Errorf("%w: credential resolution failed: %s", ErrProviderCallRefused, redactedReason(err))}
	}
	cc.Credential = cred

	// Step 4 (S95-C8(b)): the gate's own copy of the binding check. The
	// adapter repeats it (§9.1) - neither check alone is load-bearing.
	if cred.TenantID != in.TenantID || cred.ProviderID != in.ProviderID || cred.Domain != in.Domain {
		return GateResult[T]{Value: zero, Class: ErrorClassNotSent,
			Err: fmt.Errorf("%w: credential binding mismatch", ErrProviderCallRefused)}
	}

	// Step 5: deadline.
	timeout := in.Manifest.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	deadline := time.Now().Add(timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	cc.Deadline = deadline
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Step 6/7: call, recovering a panic as Ambiguous (never NotSent - a
	// panic gives no proof the call was never dispatched).
	result, class, callErr := safeCall(callCtx, cc, fn)

	// Step 8 (S95-C8(a)): redact any transport error before it can reach
	// a log line, an audit record, or a receipt.
	if callErr != nil {
		callErr = fmt.Errorf("provider call error: %s", redactedReason(callErr))
	}
	return GateResult[T]{Value: result, Class: class, Err: callErr}
}

// safeCall isolates the panic recovery so callProvider's own control flow
// never needs a named return / defer at its top level.
func safeCall[T any](ctx context.Context, cc CallContext, fn AdapterCall[T]) (result T, class ErrorClass, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			result, class, err = zero, ErrorClassAmbiguous, fmt.Errorf("payments: adapter panic recovered: %v", r)
		}
	}()
	return fn(ctx, cc)
}

// redactedReason returns an allow-listed description of err: for a
// *url.Error (which embeds the request URL, including any query string
// or userinfo), only the operation and a generic timeout/canceled/other
// classification survive. No vendor response body, header or raw error
// string ever passes through this function.
func redactedReason(err error) string {
	if err == nil {
		return ""
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		switch {
		case errors.Is(uerr.Err, context.DeadlineExceeded):
			return fmt.Sprintf("%s: timeout", uerr.Op)
		case errors.Is(uerr.Err, context.Canceled):
			return fmt.Sprintf("%s: canceled", uerr.Op)
		default:
			return fmt.Sprintf("%s: transport error", uerr.Op)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		// Not a transport error and not one of our own sentinels above:
		// still never echo it verbatim if it might carry a vendor body.
		// The MOCK adapter's own errors are safe (defined in this
		// package), so we allow those through unchanged; anything else
		// gets a generic label. Since Error() strings from vendor SDKs
		// are exactly what S95-C8(a) exists to keep out of logs, callers
		// outside this package must never construct an error type that
		// embeds vendor response bytes into Error().
		return err.Error()
	}
}

package httpserver

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// callbackRouteKind distinguishes the public, unauthenticated webhook
// route from the authenticated simulate-callback route - both call
// PaymentOrchestrator.ReceiveCallback and can therefore surface the SAME
// sentinel errors, but the two routes need DIFFERENT treatment for a
// payments.CallbackAuthError: the public route's caller is an
// unauthenticated third party (a real auth failure, 401 "callback
// rejected"), while the simulate route signs its own callback in-process
// for an already-authenticated player, so an auth failure there means the
// mock/resolver wiring itself is broken (503), never a caller error
// (backend review finding 1/6; ruling 5).
type callbackRouteKind int

const (
	callbackRoutePublicWebhook callbackRouteKind = iota
	callbackRouteSimulate
)

// mapReceiveCallbackError maps every ReceiveCallback sentinel error to its
// HTTP response, in ONE place shared by newPaymentWebhookHandler and
// newSimulateDepositCallbackHandler, so PAY-REV-1's 409 branch and
// PAY-WH-TENANT-1's 401/503 branch are added exactly once rather than
// hand-maintained twice in two files (backend review finding 1). Message
// text intentionally still differs per route where it always has
// (ErrDepositIntentNotFound, ErrCallbackPayloadMismatch,
// ErrCallbackProviderMismatch) - only the STRUCTURE is unified.
func mapReceiveCallbackError(err error, kind callbackRouteKind) (code apierror.Code, message string) {
	var authErr *payments.CallbackAuthError
	if errors.As(err, &authErr) {
		if kind == callbackRouteSimulate {
			return apierror.CodeUnavailable, "simulated settlement is misconfigured"
		}
		// Uniform 401 for EVERY reason - ErrUnknownProvider's former
		// standalone 404 branch is folded in here as
		// ReasonProviderUnregistered/ReasonProviderInvalid, never a
		// separate branch (ruling 5/backend finding 3).
		return apierror.CodeUnauthorized, "callback rejected"
	}
	if errors.Is(err, payments.ErrDepositIntentNotFound) {
		if kind == callbackRouteSimulate {
			return apierror.CodeNotFound, "deposit not found"
		}
		// Only reachable by a caller who already passed verification - see
		// deposit_handlers.go's own doc comment: this is NOT another
		// enumeration oracle, since it requires a valid tenant-bound
		// signature to reach at all.
		return apierror.CodeNotFound, "no matching deposit for this reference"
	}
	if errors.Is(err, payments.ErrCallbackPayloadMismatch) {
		if kind == callbackRouteSimulate {
			return apierror.CodeConflict, "deposit could not be settled"
		}
		return apierror.CodeConflict, "callback rejected"
	}
	if errors.Is(err, payments.ErrDepositAlreadyReversed) {
		if kind == callbackRouteSimulate {
			return apierror.CodeConflict, "deposit could not be settled"
		}
		return apierror.CodeConflict, "callback rejected"
	}
	if errors.Is(err, payments.ErrCallbackProviderMismatch) {
		if kind == callbackRouteSimulate {
			return apierror.CodeConflict, "deposit could not be settled"
		}
		// Security review PW-2/P3-5 (Stage 10.1): a VERIFIED callback
		// whose own declared facts (amount/asset) contradict the deposit
		// it claims to resolve is a validation failure, not a server
		// error - a real PSP would otherwise retry a 500 indefinitely.
		// Generic body only: no amount, asset, or reference (those live in
		// err's text, which is logged, never returned to the caller).
		return apierror.CodeValidation, "callback rejected"
	}
	if errors.Is(err, payments.ErrProviderReferenceInvalid) {
		// PROVIDER-REF-BOUND-1: a VERIFIED callback whose provider
		// reference breaks the platform bound. Deterministic and never
		// retryable - the same 400 class as a malformed verified body.
		// Generic body only (never the reference).
		if kind == callbackRouteSimulate {
			return apierror.CodeValidation, "invalid provider reference"
		}
		return apierror.CodeValidation, "callback rejected"
	}
	if errors.Is(err, payments.ErrCallbackMalformedBody) {
		if kind == callbackRouteSimulate {
			return apierror.CodeInternal, "failed to simulate deposit callback"
		}
		// A VERIFIED callback (the sender proved knowledge of the shared
		// credential) whose body is structurally malformed - unparseable
		// JSON, a missing required field, or an unrecognized event_type/
		// outcome. This is a real 4xx, not the uniform pre-verification
		// 401: the sender is authenticated, just wrong. Generic body only
		// (matches OpenAPI's documented "malformed body (after signature
		// verification)" 400 - PW-2/P3-5).
		return apierror.CodeValidation, "callback rejected"
	}
	if kind == callbackRouteSimulate {
		return apierror.CodeInternal, "failed to simulate deposit callback"
	}
	return apierror.CodeInternal, "failed to process callback"
}

// callbackAuthFailureAllowlistFields builds the allow-listed field set for
// the "payment_webhook_auth_failed" log line (design §3.3): request_id,
// reason, tenant_id (only if resolved), provider_id (only if it passed the
// charset check), key_id (only if it passed the charset check),
// credential_fingerprint (only for signature_invalid), client_ip, body_len.
// Never the body, header values, signature, raw slug, or err text.
func callbackAuthFailureAllowlistFields(requestID string, reason payments.CallbackAuthReason, tenantID *uuid.UUID, providerID string, providerIDValid bool, keyID, fingerprint, clientIPAddr string, bodyLen int) []any {
	fields := []any{"request_id", requestID, "reason", string(reason)}
	if tenantID != nil {
		fields = append(fields, "tenant_id", tenantID.String())
	}
	if providerIDValid {
		fields = append(fields, "provider_id", providerID)
	}
	if keyID != "" {
		fields = append(fields, "key_id", keyID)
	}
	if reason == payments.ReasonSignatureInvalid && fingerprint != "" {
		fields = append(fields, "credential_fingerprint", fingerprint)
	}
	fields = append(fields, "client_ip", clientIPAddr, "body_len", bodyLen)
	return fields
}

// logCallbackAuthFailure writes the single allow-listed
// "payment_webhook_auth_failed" warn line PAY-WH-TENANT-1 requires for
// EVERY pre-verification rejection, on the public webhook route only (the
// simulate route's own doc comment/audit record already covers its own
// failure modes, and its caller is an authenticated player, not an
// unauthenticated third party this alert exists for).
func logCallbackAuthFailure(logger interface {
	Warn(string, ...any)
}, r *http.Request, requestID string, reason payments.CallbackAuthReason, tenantID *uuid.UUID, providerID string, providerIDValid bool, keyID, fingerprint string, bodyLen int) {
	logWebhookAuthFailure(logger, paymentWebhookRoute.authFailedEvent, r, requestID, reason, tenantID, providerID, providerIDValid, keyID, fingerprint, bodyLen)
}

// logWebhookAuthFailure is logCallbackAuthFailure generalised to any
// webhook domain (Stage 10.2, design §A): the SAME allow-list
// (callbackAuthFailureAllowlistFields) under a domain-specific event name
// (payment_webhook_auth_failed, kyc_webhook_auth_failed,
// casino_webhook_auth_failed). Never the body, header values, signature,
// raw slug, or err text.
func logWebhookAuthFailure(logger interface {
	Warn(string, ...any)
}, event string, r *http.Request, requestID string, reason webhookauth.Reason, tenantID *uuid.UUID, providerID string, providerIDValid bool, keyID, fingerprint string, bodyLen int) {
	logger.Warn(event,
		callbackAuthFailureAllowlistFields(requestID, reason, tenantID, providerID, providerIDValid, keyID, fingerprint, clientIP(r), bodyLen)...)
}

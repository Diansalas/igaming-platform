package httpserver

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/payments"
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
		return apierror.CodeInternal, "failed to process callback"
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
	logger.Warn("payment_webhook_auth_failed",
		callbackAuthFailureAllowlistFields(requestID, reason, tenantID, providerID, providerIDValid, keyID, fingerprint, clientIP(r), bodyLen)...)
}

// Package apierror defines the platform-wide error envelope so every
// service returns errors in the same machine-readable shape. See
// docs/architecture/04-api-architecture.md "Conventions".
package apierror

import (
	"encoding/json"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Callers (frontend,
// back office, partner integrations) branch on Code, never on the human
// message, which may change wording without notice.
type Code string

const (
	CodeValidation     Code = "validation_error"
	CodeUnauthorized   Code = "unauthorized"
	CodeForbidden      Code = "forbidden"
	CodeNotFound       Code = "not_found"
	CodeConflict       Code = "conflict"
	CodeInternal       Code = "internal_error"
	CodeUnavailable    Code = "service_unavailable"
	CodeTenantMismatch Code = "tenant_mismatch"
	CodeRateLimited    Code = "rate_limited"

	// Stage 10.3 W2a provider-credential lifecycle (ADR 0093; security
	// review 07-w2a-design-review-security.md §6). CodeInvalidRequest is
	// the syntactic-only 400; every semantic registration failure is the
	// ONE byte-identical CodeCredentialRegistrationRejected 409.
	CodeInvalidRequest                 Code = "invalid_request"
	CodeCredentialRegistrationRejected Code = "credential_registration_rejected"
	CodeApprovalRejected               Code = "approval_rejected"
	CodeCredentialActivationRejected   Code = "credential_activation_rejected"
	CodeCredentialTransitionRejected   Code = "credential_transition_rejected"

	// The codes below deliberately break this package's lowercase-snake
	// convention: they are ADR 0088 §9.3's exact, pinned literal codes for
	// the Stage 10 W1 sportsbook settlement test-support route
	// (POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event), and
	// several are reused byte-for-byte as internal/sportsbook's own
	// rejection-code constants (SettlementRejectPayloadMismatch etc.) - one
	// literal string, never two different spellings of the same failure
	// between the service layer and the wire response. Not used by any
	// other route.
	CodeSettlementValidationFailed        Code = "VALIDATION_FAILED"
	CodeSettlementNotFound                Code = "NOT_FOUND"
	CodeSettlementPayloadMismatch         Code = "SETTLEMENT_PAYLOAD_MISMATCH"
	CodeSettlementTombstoned              Code = "SETTLEMENT_TOMBSTONED"
	CodeSettlementBetVoided               Code = "BET_VOIDED"
	CodeSettlementBetAlreadySettled       Code = "BET_ALREADY_SETTLED"
	CodeSettlementGenerationOutOfSequence Code = "GENERATION_OUT_OF_SEQUENCE"
	CodeSettlementIntegrity               Code = "SETTLEMENT_INTEGRITY"
	CodeSettlementPayoutInvalid           Code = "PAYOUT_INVALID"
	CodeSettlementAssetMismatch           Code = "ASSET_MISMATCH"
	CodeSettlementTenantNotActive         Code = "SETTLEMENT_TENANT_NOT_ACTIVE"
	// CodeTenantCloseBlockedOpenRounds: a tenant closure was refused because
	// open gaming rounds exist (owner decision Q-GP-1, 2026-10-06, ADR 0095
	// section 40.6; internal/tenant.CodeCloseBlockedOpenRounds). 409.
	CodeTenantCloseBlockedOpenRounds Code = "TENANT_CLOSE_BLOCKED_OPEN_ROUNDS"
	// CodeTenantOrBrandNotActive: HTTP deposit/withdrawal initiation was
	// refused because the tenant or the player's brand is not 'active'
	// (H-SEC-5 / H-SEC-11). 409; only CREATION is refused and nothing was
	// created, so a retry with the same idempotency key after reactivation is a
	// normal first request. A retry of an already-created intent/request is NOT
	// refused (it returns the original, read-only).
	CodeTenantOrBrandNotActive Code = "TENANT_OR_BRAND_NOT_ACTIVE"
	// CodeBrandNotAcceptingRegistrations: player registration was refused because the
	// brand or its tenant is not 'active' (ADR 0112 section 7.3; a brand is created
	// pending_launch). 409; nothing was created.
	CodeBrandNotAcceptingRegistrations Code = "BRAND_NOT_ACCEPTING_REGISTRATIONS"
	// B13 (ADR 0111 2.4 / 2.9). 409 both. NOT_ACCEPTED is the ONE generic
	// player-facing registration refusal (invalid detail, a card number, an
	// unknown kind or rail, a fingerprint owned by another person): the same
	// response for every cause, so registration is not an enumeration oracle.
	// IN_USE: the player may not revoke an instrument a live withdrawal binds.
	CodePayoutInstrumentNotAccepted Code = "PAYOUT_INSTRUMENT_NOT_ACCEPTED"
	CodePayoutInstrumentInUse       Code = "PAYOUT_INSTRUMENT_IN_USE"
	// B13-B (ADR 0111 2.4). REQUIRED / NOT_USABLE: a withdrawal request named no payout instrument /
	// an unusable one (nothing was created, the idempotency key is not consumed). DESTINATION_NOT_USABLE:
	// the staff submit (T1p) destination gate refused; the request stays `approved`. All 409.
	// PAYMENT_METHOD_MISMATCH: the staff body named a payment method other than the bound instrument's
	// rail (400, A-10).
	CodePayoutInstrumentRequired   Code = "PAYOUT_INSTRUMENT_REQUIRED"
	CodePayoutInstrumentNotUsable  Code = "PAYOUT_INSTRUMENT_NOT_USABLE"
	CodePayoutDestinationNotUsable Code = "PAYOUT_DESTINATION_NOT_USABLE"
	CodePaymentMethodMismatch      Code = "PAYMENT_METHOD_MISMATCH"
)

// Error is the wire format for an API error response.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// RequestID lets an operator correlate a returned error with server
	// logs/traces without exposing internal details in Message.
	RequestID string `json:"request_id,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// httpStatus maps an error code to its HTTP status. Kept in one place so
// every handler produces a consistent status for the same error kind.
func httpStatus(c Code) int {
	switch c {
	case CodeValidation, CodeSettlementValidationFailed, CodeInvalidRequest, CodePaymentMethodMismatch:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden, CodeTenantMismatch:
		return http.StatusForbidden
	case CodeNotFound, CodeSettlementNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeSettlementPayloadMismatch, CodeSettlementTombstoned, CodeSettlementBetVoided,
		CodeSettlementBetAlreadySettled, CodeSettlementGenerationOutOfSequence, CodeSettlementIntegrity,
		CodeCredentialRegistrationRejected, CodeApprovalRejected, CodeCredentialActivationRejected,
		CodeCredentialTransitionRejected, CodeSettlementTenantNotActive, CodeTenantCloseBlockedOpenRounds, CodeTenantOrBrandNotActive, CodeBrandNotAcceptingRegistrations,
		CodePayoutInstrumentNotAccepted, CodePayoutInstrumentInUse,
		CodePayoutInstrumentRequired, CodePayoutInstrumentNotUsable, CodePayoutDestinationNotUsable:
		return http.StatusConflict
	case CodeSettlementPayoutInvalid, CodeSettlementAssetMismatch:
		return http.StatusUnprocessableEntity
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// Write sends the standard error envelope. message is always safe to show
// a caller - never pass an internal error's raw Error() string for
// CodeInternal; log the detail server-side and return a generic message
// here instead.
func Write(w http.ResponseWriter, requestID string, code Code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus(code))
	_ = json.NewEncoder(w).Encode(Error{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	})
}

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
	case CodeValidation, CodeSettlementValidationFailed, CodeInvalidRequest:
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
		CodeCredentialTransitionRejected:
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

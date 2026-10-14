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
	// brand or its tenant is suspended or closed (ADR 0112 section 7.3). A pending_launch
	// brand or tenant is NOT reported this way: it answers 404 like an unknown brand
	// (security S-5). 409; nothing was created.
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

	// ADR 0112 section 9 (slice 4): the closed LAUNCH_* codes of the launch authorisation
	// routes. Every one is machine-readable and stable; the message is a fixed sentence, never
	// the underlying error text. 409 unless noted. A launch decision is a recorded human
	// decision, never a legal authorisation, and none of these codes states a legal conclusion.
	CodeLaunchPreconditionFailed   Code = "LAUNCH_PRECONDITION_FAILED"       // hard precondition H-1..H-3 (check_codes)
	CodeLaunchUnacknowledgedChecks Code = "LAUNCH_UNACKNOWLEDGED_CHECKS"     // H-5 (check_codes)
	CodeLaunchRequestPending       Code = "LAUNCH_REQUEST_PENDING"           // one pending request per subject
	CodeLaunchSelfApproval         Code = "LAUNCH_SELF_APPROVAL"             // requester / same Person
	CodeLaunchIllegalTransition    Code = "LAUNCH_ILLEGAL_TRANSITION"        // section 3 state machine
	CodeLaunchSubjectClosed        Code = "LAUNCH_SUBJECT_CLOSED"            // closed is terminal
	CodeLaunchWithdrawalsHolding   Code = "LAUNCH_WITHDRAWALS_HOLDING_FUNDS" // H-6, LA023
	CodeLaunchOpenBetsBlockClosure Code = "LAUNCH_OPEN_BETS_BLOCK_CLOSURE"   // LA024 (brand closure over open bets)
	CodeLaunchRetryableLockTimeout Code = "LAUNCH_RETRYABLE_LOCK_TIMEOUT"    // lock timeout / deadlock: retry
	CodeLaunchRequestNotPending    Code = "LAUNCH_REQUEST_NOT_PENDING"       // decided, executed, expired, superseded
	CodeLaunchAlreadyDecided       Code = "LAUNCH_ALREADY_DECIDED"           // a replayed approve / reject by the same Person
	CodeLaunchStaleReadiness       Code = "LAUNCH_STALE_READINESS"           // the decider's readiness hash is stale
	CodeLaunchPriorReject          Code = "LAUNCH_PRIOR_REJECT"              // identical request rejected within the TTL (LA014)
	CodeLaunchPriorRejecter        Code = "LAUNCH_PRIOR_REJECTER"            // approver's Person rejected a variant
	CodeLaunchNotLegacyBaseline    Code = "LAUNCH_NOT_LEGACY_BASELINE"       // ratify only a legacy baseline subject
	// ADR 0114 S6b: a release request is already open for this kill switch (the one-open unique index). 409.
	CodeKillSwitchReleaseRequestAlreadyOpen Code = "KILL_SWITCH_RELEASE_REQUEST_ALREADY_OPEN"
	CodeLaunchRefusedAtExecution            Code = "LAUNCH_REFUSED_AT_EXECUTION"      // the final approval was recorded; execution refused
	CodeLaunchGuardRefused                  Code = "LAUNCH_GUARD_REFUSED"             // another database guard (LA0xx)
	CodeLaunchProofRefused                  Code = "LAUNCH_PROOF_REFUSED"             // 403: the ADR 0110 actor proof guard (AP0xx)
	CodeLaunchNoPerson                      Code = "LAUNCH_NO_PERSON"                 // 403: the staff account has no readable Person (four-eyes cannot be evaluated)
	CodeLaunchUnavailable                   Code = "LAUNCH_AUTHORISATION_UNAVAILABLE" // 503: the service is not wired in this process
)

// ADR 0115 slice S2 (Responsible Gaming limit write APIs): the closed RG_* vocabulary. Messages are
// fixed sentences, never the underlying database error text.
const (
	CodeRGLimitAboveCeiling   Code = "RG_LIMIT_ABOVE_CEILING"  // 422: value above the composed ceiling
	CodeRGPeriodNotAllowed    Code = "RG_PERIOD_NOT_ALLOWED"   // 422: period outside the composed allowed_periods
	CodeRGDurationNotAllowed  Code = "RG_DURATION_NOT_ALLOWED" // 422: time-out duration outside the configured set
	CodeRGNotOffered          Code = "RG_NOT_OFFERED"          // 422: the configuration does not offer this change
	CodeRGBackdated           Code = "RG_BACKDATED"            // 422: effective_from in the past (RL003)
	CodeRGSyntheticRefused    Code = "RG_SYNTHETIC_REFUSED"    // 422: SYNTHETIC-NOT-LEGAL configuration refused (RL008)
	CodeRGNoChange            Code = "RG_NO_CHANGE"            // 409: the value equals the limit in force / nothing to remove
	CodeRGChangeNotPending    Code = "RG_CHANGE_NOT_PENDING"   // 409: not the latest, not pending, or does not await confirmation
	CodeRGCoolingOff          Code = "RG_COOLING_OFF"          // 409: confirm before the cooling-off period has run
	CodeRGConfigMissing       Code = "RG_CONFIG_MISSING"       // 409: increase / removal with no configuration in force
	CodeRGPendingCap          Code = "RG_PENDING_CAP"          // 409: too many open pending changes
	CodeRGIdempotencyMismatch Code = "RG_IDEMPOTENCY_MISMATCH" // 409: Idempotency-Key reused with a different request
	CodeRGOperatorLoosens     Code = "RG_OPERATOR_LOOSENS"     // 409: an operator row would loosen the floor / tenant row (RL004)
	CodeRGNoFloor             Code = "RG_NO_FLOOR_IN_FORCE"    // 409: nothing to tighten (RL005)
	CodeRGSelfApproval        Code = "RG_SELF_APPROVAL"        // 409: the requester's Person may not decide (RL007)
	CodeRGRequestNotPending   Code = "RG_REQUEST_NOT_PENDING"  // 409: decided, expired or cancelled change request
	CodeRGGuardRefused        Code = "RG_GUARD_REFUSED"        // 409: another database guard (RL0xx)
	CodeRGStepUpRequired      Code = "RG_STEP_UP_REQUIRED"     // 403: staff increase / removal (ADR 0017 step-up not implemented)
	CodeRGSelfTarget          Code = "RG_SELF_TARGET"          // 403: staff may not change their own Person's limits
	CodeRGIdentityReview      Code = "RG_IDENTITY_REVIEW"      // 403: account under identity review may not loosen a person-level limit
	CodeRGProofRefused        Code = "RG_PROOF_REFUSED"        // 403: the ADR 0110 actor proof guard (AP0xx)
	CodeRGUnavailable         Code = "RG_UNAVAILABLE"          // 503: no proof issuer / temporary lock contention
)

// ADR 0119 slice S3 (staff MFA): the closed staff authentication vocabulary (section 14.2). Lowercase like the
// generic codes. invalid_challenge and invalid_enrolment_token are each ONE answer for every cause (unknown, expired,
// consumed, exhausted, wrong purpose, mismatched credentials) and never distinguish them.
const (
	CodeMFAEnrolmentRequired  Code = "mfa_enrolment_required"  // 403: correct password, no active second factor
	CodeMFALocked             Code = "mfa_locked"              // 403: the factor is soft- or hard-locked; nothing was compared
	CodeInvalidMFACode        Code = "invalid_mfa_code"        // 401: wrong / malformed / replayed code (an attempt was spent)
	CodeInvalidChallenge      Code = "invalid_challenge"       // 401: the challenge token is not usable
	CodeInvalidEnrolmentToken Code = "invalid_enrolment_token" // 401: the enrolment token or the credentials presented with it
	CodeMFAEnrolmentExpired   Code = "mfa_enrolment_expired"   // 409: the pending factor is unknown or its 10 minutes passed
)

// ADR 0119 slice S4a (section 7.3): the per-request staff liveness refusal. ONE answer for every cause (session
// revoked or unknown, absolute lifetime over, staff member not active, role or staff version changed, second factor
// revoked or hard-locked); the cause goes to the metric and the log, never to the client.
const (
	CodeSessionRevoked Code = "session_revoked" // 401: the staff session behind this access token is no longer live
)

// ADR 0119 slice S4b (sections 6.6, 7.3, 9.3): the step-up refusal. 403 with max_age (seconds) and the RFC 9470
// WWW-Authenticate challenge (set by auth.WriteStepUpRequired, the only caller of WriteStepUpRequired).
const (
	CodeStepUpRequired Code = "step_up_required"
)

// ADR 0119 slices S5 + S6 (section 14.2): the staff lifecycle refusals. Lowercase like the staff auth vocabulary.
const (
	CodeLastAdmin                  Code = "last_admin"                   // 409: SF020 / SF021, the last active enrolled admin
	CodeSelfTargetForbidden        Code = "self_target_forbidden"        // 403: the target is the caller (SF013)
	CodePersonRequired             Code = "person_required"              // 422: an approve-capable role without a Person (SF014)
	CodeRoleNotGrantable           Code = "role_not_grantable"           // 403: the caller may not grant / act on that role
	CodeLifecycleBusy              Code = "lifecycle_busy"               // 409: suspension / offboarding phase 2 lock timeout (twice)
	CodeLifecycleTransitionInvalid Code = "lifecycle_transition_invalid" // 409: the target is not in a state the action applies to
	CodeFourEyesViolation          Code = "four_eyes_violation"          // 403: platform four-eyes distinctness (SF032)
)

// Error is the wire format for an API error response.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// RequestID lets an operator correlate a returned error with server
	// logs/traces without exposing internal details in Message.
	RequestID string `json:"request_id,omitempty"`
	// CheckCodes is set only by the launch authorisation routes (WriteWithCheckCodes).
	CheckCodes []string `json:"check_codes,omitempty"`
	// MaxAge is set only with CodeStepUpRequired (WriteStepUpRequired): the step-up freshness, in seconds.
	MaxAge *int `json:"max_age,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// httpStatus maps an error code to its HTTP status. Kept in one place so
// every handler produces a consistent status for the same error kind.
func httpStatus(c Code) int {
	switch c {
	case CodeValidation, CodeSettlementValidationFailed, CodeInvalidRequest, CodePaymentMethodMismatch:
		return http.StatusBadRequest
	case CodeUnauthorized, CodeInvalidMFACode, CodeInvalidChallenge, CodeInvalidEnrolmentToken, CodeSessionRevoked:
		return http.StatusUnauthorized
	case CodeForbidden, CodeTenantMismatch, CodeLaunchProofRefused, CodeLaunchNoPerson,
		CodeRGStepUpRequired, CodeRGSelfTarget, CodeRGIdentityReview, CodeRGProofRefused,
		CodeMFAEnrolmentRequired, CodeMFALocked, CodeStepUpRequired,
		CodeSelfTargetForbidden, CodeRoleNotGrantable, CodeFourEyesViolation:
		return http.StatusForbidden
	case CodeNotFound, CodeSettlementNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeSettlementPayloadMismatch, CodeSettlementTombstoned, CodeSettlementBetVoided,
		CodeSettlementBetAlreadySettled, CodeSettlementGenerationOutOfSequence, CodeSettlementIntegrity,
		CodeCredentialRegistrationRejected, CodeApprovalRejected, CodeCredentialActivationRejected,
		CodeCredentialTransitionRejected, CodeSettlementTenantNotActive, CodeTenantCloseBlockedOpenRounds, CodeTenantOrBrandNotActive, CodeBrandNotAcceptingRegistrations,
		CodePayoutInstrumentNotAccepted, CodePayoutInstrumentInUse,
		CodePayoutInstrumentRequired, CodePayoutInstrumentNotUsable, CodePayoutDestinationNotUsable,
		CodeLaunchPreconditionFailed, CodeLaunchUnacknowledgedChecks, CodeLaunchRequestPending, CodeLaunchSelfApproval,
		CodeLaunchIllegalTransition, CodeLaunchSubjectClosed, CodeLaunchWithdrawalsHolding, CodeLaunchOpenBetsBlockClosure,
		CodeLaunchRetryableLockTimeout, CodeLaunchRequestNotPending, CodeLaunchAlreadyDecided, CodeLaunchStaleReadiness, CodeLaunchPriorReject,
		CodeLaunchPriorRejecter, CodeLaunchNotLegacyBaseline, CodeLaunchRefusedAtExecution, CodeLaunchGuardRefused,
		CodeKillSwitchReleaseRequestAlreadyOpen, CodeMFAEnrolmentExpired,
		CodeLastAdmin, CodeLifecycleBusy, CodeLifecycleTransitionInvalid,
		CodeRGNoChange, CodeRGChangeNotPending, CodeRGCoolingOff, CodeRGConfigMissing, CodeRGPendingCap,
		CodeRGIdempotencyMismatch, CodeRGOperatorLoosens, CodeRGNoFloor, CodeRGSelfApproval, CodeRGRequestNotPending, CodeRGGuardRefused:
		return http.StatusConflict
	case CodeSettlementPayoutInvalid, CodeSettlementAssetMismatch, CodePersonRequired,
		CodeRGLimitAboveCeiling, CodeRGPeriodNotAllowed, CodeRGDurationNotAllowed, CodeRGNotOffered, CodeRGBackdated, CodeRGSyntheticRefused:
		return http.StatusUnprocessableEntity
	case CodeUnavailable, CodeLaunchUnavailable, CodeRGUnavailable:
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
	WriteWithCheckCodes(w, requestID, code, message, nil)
}

// WriteWithCheckCodes is Write plus the machine-readable check codes of a refused launch
// precondition or readiness acknowledgement (ADR 0112 section 9: "LAUNCH_PRECONDITION_FAILED
// (with check codes)"). The codes are a closed vocabulary (H-n:..., R-...), never free text.
func WriteWithCheckCodes(w http.ResponseWriter, requestID string, code Code, message string, checkCodes []string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus(code))
	_ = json.NewEncoder(w).Encode(Error{
		Code:       code,
		Message:    message,
		RequestID:  requestID,
		CheckCodes: checkCodes,
	})
}

// WriteStepUpRequired writes 403 step_up_required with max_age (seconds). The
// caller sets the RFC 9470 WWW-Authenticate header first.
func WriteStepUpRequired(w http.ResponseWriter, requestID string, maxAgeSeconds int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus(CodeStepUpRequired))
	ma := maxAgeSeconds
	_ = json.NewEncoder(w).Encode(Error{
		Code:      CodeStepUpRequired,
		Message:   "a fresh second-factor verification is required",
		RequestID: requestID,
		MaxAge:    &ma,
	})
}

// Status exposes the HTTP status an error code maps to (for contract tests).
func Status(c Code) int { return httpStatus(c) }

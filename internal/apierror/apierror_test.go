package apierror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite_StatusCodeMapping(t *testing.T) {
	cases := []struct {
		code       Code
		wantStatus int
	}{
		{CodeValidation, http.StatusBadRequest},
		{CodeUnauthorized, http.StatusUnauthorized},
		{CodeForbidden, http.StatusForbidden},
		{CodeTenantMismatch, http.StatusForbidden},
		{CodeNotFound, http.StatusNotFound},
		{CodeConflict, http.StatusConflict},
		{CodeTenantCloseBlockedOpenRounds, http.StatusConflict},
		{CodeUnavailable, http.StatusServiceUnavailable},
		{CodeRateLimited, http.StatusTooManyRequests},
		{CodeInternal, http.StatusInternalServerError},
		{Code("something_unrecognized"), http.StatusInternalServerError}, // safe default
	}

	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, "req-123", tc.code, "a message")

			if rec.Code != tc.wantStatus {
				t.Errorf("code %q: expected status %d, got %d", tc.code, tc.wantStatus, rec.Code)
			}

			var body Error
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode response body: %v", err)
			}
			if body.Code != tc.code {
				t.Errorf("expected body code %q, got %q", tc.code, body.Code)
			}
			if body.RequestID != "req-123" {
				t.Errorf("expected request id 'req-123', got %q", body.RequestID)
			}
		})
	}
}

// ADR 0112 slice 4: the closed LAUNCH_* codes keep their literal strings and statuses.
func TestLaunchCodes_LiteralsAndStatuses(t *testing.T) {
	cases := []struct {
		code    Code
		literal string
		status  int
	}{
		{CodeLaunchPreconditionFailed, "LAUNCH_PRECONDITION_FAILED", 409},
		{CodeLaunchUnacknowledgedChecks, "LAUNCH_UNACKNOWLEDGED_CHECKS", 409},
		{CodeLaunchRequestPending, "LAUNCH_REQUEST_PENDING", 409},
		{CodeLaunchSelfApproval, "LAUNCH_SELF_APPROVAL", 409},
		{CodeLaunchIllegalTransition, "LAUNCH_ILLEGAL_TRANSITION", 409},
		{CodeLaunchSubjectClosed, "LAUNCH_SUBJECT_CLOSED", 409},
		{CodeLaunchWithdrawalsHolding, "LAUNCH_WITHDRAWALS_HOLDING_FUNDS", 409},
		{CodeLaunchOpenBetsBlockClosure, "LAUNCH_OPEN_BETS_BLOCK_CLOSURE", 409},
		{CodeLaunchRetryableLockTimeout, "LAUNCH_RETRYABLE_LOCK_TIMEOUT", 409},
		{CodeLaunchRequestNotPending, "LAUNCH_REQUEST_NOT_PENDING", 409},
		{CodeLaunchStaleReadiness, "LAUNCH_STALE_READINESS", 409},
		{CodeLaunchAlreadyDecided, "LAUNCH_ALREADY_DECIDED", 409},
		{CodeLaunchPriorReject, "LAUNCH_PRIOR_REJECT", 409},
		{CodeLaunchPriorRejecter, "LAUNCH_PRIOR_REJECTER", 409},
		{CodeLaunchNotLegacyBaseline, "LAUNCH_NOT_LEGACY_BASELINE", 409},
		{CodeKillSwitchReleaseRequestAlreadyOpen, "KILL_SWITCH_RELEASE_REQUEST_ALREADY_OPEN", 409},
		{CodeLaunchRefusedAtExecution, "LAUNCH_REFUSED_AT_EXECUTION", 409},
		{CodeLaunchGuardRefused, "LAUNCH_GUARD_REFUSED", 409},
		{CodeLaunchProofRefused, "LAUNCH_PROOF_REFUSED", 403},
		{CodeLaunchNoPerson, "LAUNCH_NO_PERSON", 403},
		{CodeLaunchUnavailable, "LAUNCH_AUTHORISATION_UNAVAILABLE", 503},
	}
	for _, tc := range cases {
		if string(tc.code) != tc.literal || Status(tc.code) != tc.status {
			t.Errorf("%s: literal %q status %d (want %d)", tc.literal, tc.code, Status(tc.code), tc.status)
		}
	}
}

// Migration 0177: the closed LICENSING_CHANGE_* codes keep their literal strings and statuses.
func TestLicensingChangeCodes_LiteralsAndStatuses(t *testing.T) {
	cases := []struct {
		code    Code
		literal string
		status  int
	}{
		{CodeLicensingChangeSelfApproval, "LICENSING_CHANGE_SELF_APPROVAL", 409},
		{CodeLicensingChangeNotPending, "LICENSING_CHANGE_NOT_PENDING", 409},
		{CodeLicensingChangeAlreadyDecided, "LICENSING_CHANGE_ALREADY_DECIDED", 409},
		{CodeLicensingChangeDuplicatePending, "LICENSING_CHANGE_DUPLICATE_PENDING", 409},
		{CodeLicensingChangeApprovalsStale, "LICENSING_CHANGE_APPROVALS_STALE", 409},
		{CodeLicensingChangeRefused, "LICENSING_CHANGE_REFUSED", 409},
		{CodeLicensingChangeGuardRefused, "LICENSING_CHANGE_GUARD_REFUSED", 409},
		{CodeLicensingChangeRetryableLockWait, "LICENSING_CHANGE_RETRYABLE_LOCK_TIMEOUT", 409},
		{CodeLicensingChangeProofRefused, "LICENSING_CHANGE_PROOF_REFUSED", 403},
		{CodeLicensingChangeNoPerson, "LICENSING_CHANGE_NO_PERSON", 403},
		{CodeLicensingChangeUnavailable, "LICENSING_CHANGE_UNAVAILABLE", 503},
	}
	for _, tc := range cases {
		if string(tc.code) != tc.literal || Status(tc.code) != tc.status {
			t.Errorf("%s: literal %q status %d (want %d)", tc.literal, tc.code, Status(tc.code), tc.status)
		}
	}
}

// ADR 0115 slice S2: the closed RG_* codes keep their literal strings and statuses.
func TestRGCodes_LiteralsAndStatuses(t *testing.T) {
	cases := []struct {
		code    Code
		literal string
		status  int
	}{
		{CodeRGLimitAboveCeiling, "RG_LIMIT_ABOVE_CEILING", 422},
		{CodeRGPeriodNotAllowed, "RG_PERIOD_NOT_ALLOWED", 422},
		{CodeRGDurationNotAllowed, "RG_DURATION_NOT_ALLOWED", 422},
		{CodeRGNotOffered, "RG_NOT_OFFERED", 422},
		{CodeRGBackdated, "RG_BACKDATED", 422},
		{CodeRGSyntheticRefused, "RG_SYNTHETIC_REFUSED", 422},
		{CodeRGNoChange, "RG_NO_CHANGE", 409},
		{CodeRGChangeNotPending, "RG_CHANGE_NOT_PENDING", 409},
		{CodeRGCoolingOff, "RG_COOLING_OFF", 409},
		{CodeRGConfigMissing, "RG_CONFIG_MISSING", 409},
		{CodeRGPendingCap, "RG_PENDING_CAP", 409},
		{CodeRGIdempotencyMismatch, "RG_IDEMPOTENCY_MISMATCH", 409},
		{CodeRGOperatorLoosens, "RG_OPERATOR_LOOSENS", 409},
		{CodeRGNoFloor, "RG_NO_FLOOR_IN_FORCE", 409},
		{CodeRGSelfApproval, "RG_SELF_APPROVAL", 409},
		{CodeRGRequestNotPending, "RG_REQUEST_NOT_PENDING", 409},
		{CodeRGGuardRefused, "RG_GUARD_REFUSED", 409},
		{CodeRGStepUpRequired, "RG_STEP_UP_REQUIRED", 403},
		{CodeRGSelfTarget, "RG_SELF_TARGET", 403},
		{CodeRGIdentityReview, "RG_IDENTITY_REVIEW", 403},
		{CodeRGProofRefused, "RG_PROOF_REFUSED", 403},
		{CodeRGUnavailable, "RG_UNAVAILABLE", 503},
	}
	for _, tc := range cases {
		if string(tc.code) != tc.literal || Status(tc.code) != tc.status {
			t.Errorf("%s: literal %q status %d (want %d)", tc.literal, tc.code, Status(tc.code), tc.status)
		}
	}
}

// A10c: the staff Idempotency-Key codes keep their literal strings and statuses.
func TestIdempotencyCodes_LiteralsAndStatuses(t *testing.T) {
	for code, lit := range map[Code]string{CodeIdempotencyMismatch: "IDEMPOTENCY_MISMATCH", CodeIdempotencyInProgress: "IDEMPOTENCY_IN_PROGRESS"} {
		if string(code) != lit || Status(code) != 409 {
			t.Errorf("%s: literal %q status %d (want 409)", lit, code, Status(code))
		}
	}
}

func TestWriteWithCheckCodes_CarriesTheClosedCodes(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteWithCheckCodes(rec, "r1", CodeLaunchPreconditionFailed, "refused", []string{"H-2:licence_id_required", "R-LIC-VALID"})
	var body Error
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 409 || len(body.CheckCodes) != 2 || body.CheckCodes[0] != "H-2:licence_id_required" {
		t.Fatalf("%d %+v", rec.Code, body)
	}
	rec = httptest.NewRecorder()
	Write(rec, "r1", CodeNotFound, "x")
	if got := rec.Body.String(); contains(got, "check_codes") {
		t.Fatalf("a plain error carries no check_codes: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestWrite_SetsJSONContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, "req-1", CodeValidation, "bad input")

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json; charset=utf-8" {
		t.Errorf("expected JSON content type, got %q", ct)
	}
}

// lic11 (migration 0182): the closed WITHDRAWAL_RESTRICTION_POLICY_* codes keep their literal strings and statuses.
func TestWithdrawalRestrictionPolicyCodes_LiteralsAndStatuses(t *testing.T) {
	cases := []struct {
		code    Code
		literal string
		status  int
	}{
		{CodeWRPSelfApproval, "WITHDRAWAL_RESTRICTION_POLICY_SELF_APPROVAL", 409},
		{CodeWRPNotPending, "WITHDRAWAL_RESTRICTION_POLICY_NOT_PENDING", 409},
		{CodeWRPAlreadyDecided, "WITHDRAWAL_RESTRICTION_POLICY_ALREADY_DECIDED", 409},
		{CodeWRPDuplicatePending, "WITHDRAWAL_RESTRICTION_POLICY_DUPLICATE_PENDING", 409},
		{CodeWRPApprovalsStale, "WITHDRAWAL_RESTRICTION_POLICY_APPROVALS_STALE", 409},
		{CodeWRPNotConfigurable, "WITHDRAWAL_RESTRICTION_POLICY_NOT_CONFIGURABLE", 409},
		{CodeWRPGuardRefused, "WITHDRAWAL_RESTRICTION_POLICY_GUARD_REFUSED", 409},
		{CodeWRPRetryableLockWait, "WITHDRAWAL_RESTRICTION_POLICY_RETRYABLE_LOCK_TIMEOUT", 409},
		{CodeWRPProofRefused, "WITHDRAWAL_RESTRICTION_POLICY_PROOF_REFUSED", 403},
		{CodeWRPNoPerson, "WITHDRAWAL_RESTRICTION_POLICY_NO_PERSON", 403},
		{CodeWRPUnavailable, "WITHDRAWAL_RESTRICTION_POLICY_UNAVAILABLE", 503},
	}
	for _, tc := range cases {
		if string(tc.code) != tc.literal || Status(tc.code) != tc.status {
			t.Errorf("%s: literal %q status %d (want %d)", tc.literal, tc.code, Status(tc.code), tc.status)
		}
	}
}

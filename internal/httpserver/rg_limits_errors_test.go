package httpserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

// ADR 0115 S2: the refusal -> HTTP mapping is exact (code and status) for every refusal.
func TestRGErrorCode_ExactMappingAndStatus(t *testing.T) {
	cases := []struct {
		refusal rg.RefusalCode
		code    apierror.Code
		status  int
	}{
		{rg.RefusedInvalid, apierror.CodeValidation, 400},
		{rg.RefusedNotFound, apierror.CodeNotFound, 404},
		{rg.RefusedForbidden, apierror.CodeForbidden, 403},
		{rg.RefusedAboveCeiling, apierror.CodeRGLimitAboveCeiling, 422},
		{rg.RefusedPeriodNotAllowed, apierror.CodeRGPeriodNotAllowed, 422},
		{rg.RefusedDurationNotAllowed, apierror.CodeRGDurationNotAllowed, 422},
		{rg.RefusedNotOffered, apierror.CodeRGNotOffered, 422},
		{rg.RefusedBackdated, apierror.CodeRGBackdated, 422},
		{rg.RefusedSyntheticRefused, apierror.CodeRGSyntheticRefused, 422},
		{rg.RefusedNoChange, apierror.CodeRGNoChange, 409},
		{rg.RefusedChangeNotPending, apierror.CodeRGChangeNotPending, 409},
		{rg.RefusedCoolingOff, apierror.CodeRGCoolingOff, 409},
		{rg.RefusedConfigMissing, apierror.CodeRGConfigMissing, 409},
		{rg.RefusedPendingCap, apierror.CodeRGPendingCap, 409},
		{rg.RefusedIdempotencyMismatch, apierror.CodeRGIdempotencyMismatch, 409},
		{rg.RefusedOperatorLoosens, apierror.CodeRGOperatorLoosens, 409},
		{rg.RefusedNoFloor, apierror.CodeRGNoFloor, 409},
		{rg.RefusedSelfApproval, apierror.CodeRGSelfApproval, 409},
		{rg.RefusedRequestNotPending, apierror.CodeRGRequestNotPending, 409},
		{rg.RefusedGuard, apierror.CodeRGGuardRefused, 409},
		{rg.RefusedStepUpRequired, apierror.CodeRGStepUpRequired, 403},
		{rg.RefusedSelfTarget, apierror.CodeRGSelfTarget, 403},
		{rg.RefusedIdentityReview, apierror.CodeRGIdentityReview, 403},
		{rg.RefusedProof, apierror.CodeRGProofRefused, 403},
		{rg.RefusedUnavailable, apierror.CodeRGUnavailable, 503},
	}
	seen := map[apierror.Code]bool{}
	for _, c := range cases {
		code, msg := rgErrorCode(&rg.Refusal{Code: c.refusal, Detail: "d"})
		if code != c.code || apierror.Status(code) != c.status || msg == "" {
			t.Errorf("%s: got %s/%d %q, want %s/%d", c.refusal, code, apierror.Status(code), msg, c.code, c.status)
		}
		if c.refusal != rg.RefusedInvalid && c.refusal != rg.RefusedOperatorLoosens && msg == "d" {
			t.Errorf("%s: the message must be a fixed sentence, not the refusal detail", c.refusal)
		}
		seen[code] = true
	}
	if len(seen) != len(cases) {
		t.Errorf("two refusals map to the same code")
	}
	// an unknown refusal fails to internal, never to a 2xx / 4xx that hides the fault
	if code, _ := rgErrorCode(&rg.Refusal{Code: "bogus"}); code != apierror.CodeInternal {
		t.Errorf("unknown refusal -> %s", code)
	}
	// the loosened field is named, the database detail is not
	if _, msg := rgErrorCode(&rg.Refusal{Code: rg.RefusedOperatorLoosens, Detail: "ceiling"}); msg == "" || !strings.Contains(msg, "ceiling") {
		t.Errorf("operator_loosens message: %q", msg)
	}
}

func TestWriteRGError_ClassifiesAndNeverLeaks(t *testing.T) {
	c := rgCall{requestID: "rid"}
	c.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := httptest.NewRecorder()
	rf := writeRGError(rec, c, "op", &rg.Refusal{Code: rg.RefusedUnavailable, Detail: "busy"})
	if rf == nil || rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("unavailable: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	if rf := writeRGError(rec, c, "op", errString("pq: relation player_rg_limit_changes secret")); rf == nil || rf.Code != rgRefusedInternal || rec.Code != 500 {
		t.Fatalf("internal: %d", rec.Code)
	}
	var e apierror.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != apierror.CodeInternal || strings.Contains(e.Message, "player_rg_limit_changes") {
		t.Fatalf("an internal error reveals nothing: %s", rec.Body.String())
	}
}

type errString string

func (e errString) Error() string { return string(e) }

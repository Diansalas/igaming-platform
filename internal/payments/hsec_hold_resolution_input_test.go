package payments

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// HSEC-APPROVED-HOLD-RELEASE-1: the input validators (no database needed). The reason
// code pattern is ^[a-z][a-z0-9_]{0,63}$ (security L-6); the evidence reference is a hex
// SHA-256 only; the only kind is release_hold_to_player.
func TestHSEC_HoldResolutionInputValidation(t *testing.T) {
	ok := HoldResolutionRequestInput{WithdrawalRequestID: uuid.New(), Kind: HoldReleaseToPlayer, ReasonCode: "tenant_suspended",
		EvidenceRefHash: strings.Repeat("ab", 32)}
	if err := ok.validate(); err != nil {
		t.Fatalf("a valid input: %v", err)
	}
	mut := func(f func(*HoldResolutionRequestInput)) HoldResolutionRequestInput { c := ok; f(&c); return c }
	for name, in := range map[string]HoldResolutionRequestInput{
		"nil withdrawal": mut(func(i *HoldResolutionRequestInput) { i.WithdrawalRequestID = uuid.Nil }),
		"unknown kind":   mut(func(i *HoldResolutionRequestInput) { i.Kind = "release_everything" }),
		"empty kind":     mut(func(i *HoldResolutionRequestInput) { i.Kind = "" }),
		"empty reason":   mut(func(i *HoldResolutionRequestInput) { i.ReasonCode = "" }),
		"upper reason":   mut(func(i *HoldResolutionRequestInput) { i.ReasonCode = "Bad" }),
		"dash reason":    mut(func(i *HoldResolutionRequestInput) { i.ReasonCode = "bad-code" }),
		"digit-first":    mut(func(i *HoldResolutionRequestInput) { i.ReasonCode = "1bad" }),
		"65-char reason": mut(func(i *HoldResolutionRequestInput) { i.ReasonCode = "a" + strings.Repeat("b", 64) }),
		"empty evidence": mut(func(i *HoldResolutionRequestInput) { i.EvidenceRefHash = "" }),
		"upper evidence": mut(func(i *HoldResolutionRequestInput) { i.EvidenceRefHash = strings.Repeat("AB", 32) }),
		"short evidence": mut(func(i *HoldResolutionRequestInput) { i.EvidenceRefHash = "abcd" }),
		"over-long note": mut(func(i *HoldResolutionRequestInput) { i.Note = strings.Repeat("n", 1001) }),
	} {
		if err := in.validate(); !errors.Is(err, ErrHoldResolutionInvalid) {
			t.Errorf("%s: want ErrHoldResolutionInvalid, got %v", name, err)
		}
	}
	if err := mut(func(i *HoldResolutionRequestInput) { i.ReasonCode = "a" + strings.Repeat("b", 63) }).validate(); err != nil {
		t.Errorf("a 64-char reason is allowed: %v", err)
	}
}

// The classifier maps by SQLSTATE only and never to a token that leaks text.
func TestHSEC_HoldResolutionClassifier(t *testing.T) {
	cases := map[string]ResolutionErrClass{
		"HR014": ResolutionErrDisabled, "HR010": ResolutionErrPrecondition, "HR041": ResolutionErrPrecondition,
		"HR001": ResolutionErrForbidden, "HR003": ResolutionErrForbidden, "HR011": ResolutionErrForbidden, "HR032": ResolutionErrForbidden,
		"AP001": ResolutionErrForbidden, "AP005": ResolutionErrForbidden, "HR002": ResolutionErrSession, "CG020": ResolutionErrSession,
		"HR030": ResolutionErrConflict, "HR031": ResolutionErrConflict, "HR020": ResolutionErrConflict, "HR050": ResolutionErrConflict,
		"CG030": ResolutionErrConflict, "23505": ResolutionErrConflict, "40001": ResolutionErrRetryable, "23514": ResolutionErrInvalid,
	}
	for code, want := range cases {
		err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})
		if got := ClassifyHoldResolutionError(err); got != want {
			t.Errorf("%s: class %s, want %s", code, got, want)
		}
	}
	if HoldResolutionToken(ResolutionErrOther) != "" || HoldResolutionToken(ResolutionErrInvalid) != "" {
		t.Error("other/invalid classes must map to no token (500/400)")
	}
	for _, c := range []ResolutionErrClass{ResolutionErrDisabled, ResolutionErrForbidden, ResolutionErrPrecondition, ResolutionErrConflict, ResolutionErrExpired, ResolutionErrNotFound} {
		if !strings.HasPrefix(HoldResolutionToken(c), "hold_resolution_") {
			t.Errorf("class %s token %q", c, HoldResolutionToken(c))
		}
	}
}

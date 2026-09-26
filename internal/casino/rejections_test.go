package casino

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// Stage 10.3 W2b (CAS-RECON-1): every recorded rejection class maps from
// its sentinel (even when wrapped with context, as postWin does), and
// nothing else is ever wrapped - in particular no pre-verification
// AuthError and no retryable/validation error.
func TestRejectionClassFor_EveryClassAndNothingElse(t *testing.T) {
	cases := []struct {
		err  error
		want RejectionClass
	}{
		{ErrOriginalTombstoned, RejectionOriginalTombstoned},
		{ErrAmbiguousMultiOriginRound, RejectionAmbiguousRound},
		{ErrCorrelationWalletCollision, RejectionWalletCollision},
		{ErrMixedFundingUnsupported, RejectionMixedFunding},
		{ErrLockAlreadyReleased, RejectionLockAlreadyReleased},
		{ErrBonusBetNotLocked, RejectionBonusBetNotLocked},
		{ErrBetNotFound, RejectionBetNotFound},
		{ErrAlreadyRolledBack, RejectionAlreadyRolledBack},
		{ErrProviderTxPayloadMismatch, RejectionPayloadMismatch},
		{ErrProviderRoundOwnershipConflict, RejectionRoundOwnershipConflict},
		// mapReplayPayloadMismatch's own wrapping shape.
		{fmt.Errorf("%w: %w", ErrProviderTxPayloadMismatch, ledger.ErrIdempotencyPayloadMismatch), RejectionPayloadMismatch},
	}
	for _, c := range cases {
		wrapped := fmt.Errorf("%w: round=r provider=p", c.err)
		got, ok := rejectionClassFor(wrapped)
		if !ok || got != c.want {
			t.Errorf("rejectionClassFor(%v) = %q,%t; want %q", c.err, got, ok, c.want)
		}
	}
	for _, err := range []error{
		nil, errors.New("boom"), ErrInvalidInput, ErrCallbackMalformedBody, ErrOutcomeNotSucceeded,
		ErrLaunchSessionRequired, ErrProviderUnavailable, ErrInsufficientFunds,
		&webhookauth.AuthError{Reason: webhookauth.ReasonSignatureInvalid},
	} {
		if got, ok := rejectionClassFor(err); ok {
			t.Errorf("rejectionClassFor(%v) must not classify, got %q", err, got)
		}
	}
}

func TestWrapRejection_PreservesErrorsIsAndResult(t *testing.T) {
	ev := CallbackEvent{EventType: CallbackEventWin, ProviderTxID: "w", RoundID: "r", AssetCode: "EUR", Amount: 9}
	base := fmt.Errorf("%w: ctx", ErrOriginalTombstoned)
	_, err := wrapRejection("p", ev)(ReceiveCallbackResult{}, base)
	var rej *CallbackRejectedError
	if !errors.As(err, &rej) || !errors.Is(err, ErrOriginalTombstoned) || err.Error() != base.Error() {
		t.Fatalf("wrapped error lost its identity: %v", err)
	}
	if rej.ProviderID != "p" || rej.Rejection.ProviderTxID != "w" || rej.Rejection.Amount != 9 {
		t.Fatalf("unexpected rejection: %+v", rej)
	}
	res := ReceiveCallbackResult{Outcome: OutcomeSucceeded}
	got, err := wrapRejection("p", ev)(res, nil)
	if err != nil || got.Outcome != OutcomeSucceeded {
		t.Fatalf("a success must pass through unchanged: %+v %v", got, err)
	}
}

// roundCorrelationIDKnownVector pins roundCorrelationID to a literal that
// internal/reconciliation's C1 check (casRoundCorrelationID, a deliberate
// copy - reconciliation must not import casino) pins to as well.
const roundCorrelationIDKnownVector = "a304a518-6bb1-53f4-8cd1-bdfa3a1a376b"

func TestRoundCorrelationID_KnownVector(t *testing.T) {
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if got := roundCorrelationID(tenant, "mock-casino", "round-1").String(); got != roundCorrelationIDKnownVector {
		t.Fatalf("roundCorrelationID vector changed: %s", got)
	}
}

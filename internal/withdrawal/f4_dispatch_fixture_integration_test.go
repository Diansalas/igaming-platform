//go:build integration

package withdrawal

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// f4AllowDispatch is the F4 FIXTURE step before a test's own E7 (MarkSubmitted / MarkSubmittedPending / a raw
// approved -> submitted UPDATE): it takes the request's L1 lock (ErrStateConflict when the request is not
// `approved`, exactly what the E7 writers answer) and RECORDS an allowing WD-RG-1 dispatch decision, which
// migration 0169 requires in the transaction of the transition (WD053). These fixtures test the state machine,
// not the gate (the gate itself is tested in f4_dispatch_gate_integration_test.go and in internal/payments); the
// database still re-checks the account and wallet status (WD052) at the transition.
func f4AllowDispatch(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	wr, err := LockApprovedForSubmission(ctx, tx, id)
	if err != nil {
		return err
	}
	// A migration test drives a scratch database whose schema predates 0169 (no table, no backstop): nothing to record.
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.withdrawal_gate_decisions') IS NOT NULL`).Scan(&present); err != nil || !present {
		return err
	}
	return RecordDispatchGateDecision(ctx, tx, wr, DispatchStageDispatch, nil, withdrawalgate.Decision{Allowed: true, Outcome: "allow"})
}

// f4ThenSubmit runs f4AllowDispatch and then the test's own E7 writer.
func f4ThenSubmit(ctx context.Context, tx pgx.Tx, id uuid.UUID, e7 func() error) error {
	if err := f4AllowDispatch(ctx, tx, id); err != nil {
		return err
	}
	return e7()
}

// f4AllowDispatchNoLock records the signed fixture ALLOW without taking the request's L1 lock first.
func f4AllowDispatchNoLock(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	wr, err := GetByID(ctx, tx, id)
	if err != nil {
		return err
	}
	return RecordDispatchGateDecision(ctx, tx, wr, DispatchStageDispatch, nil, withdrawalgate.Decision{Allowed: true, Outcome: "allow"})
}

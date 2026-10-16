//go:build integration

// Migration 0162 (A5 / WITHDRAWAL-REVERSAL-1) on a THROWAWAY scratch database: up -> down restores the
// 0160 schema byte for byte (the 0132 / 0034 function bodies, the 0026 UNIQUE, the decision CHECK, the
// dropped column) -> up again equals the first up (whole-schema snapshot); and the down refuses (WD097)
// while a reversal row exists, changing nothing. Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL.
package withdrawal

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const migration0162Version = 162

// Migration 0163 (A5 / PAYOUT-AMOUNT-DISPUTE-1, the M2 admission): up -> down restores the 0115 / 0125
// function bodies byte for byte -> up again equals the first up. (Its down refusal, MR099, is tested in
// internal/payments, which has the M2 world.)
func TestMigration0163_UpDownUp_Snapshot(t *testing.T) {
	ctx := context.Background()
	pool := m132Scratch(t, "m0163_")
	if _, err := pool.MigrateUp(ctx, m132Dir(t, 162)); err != nil {
		t.Fatalf("migrate to 0162: %v", err)
	}
	snapBefore := m132Snapshot(t, pool)
	full := m132Dir(t, 163)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0163 up: %v", err)
	}
	snapUp := m132Snapshot(t, pool)
	if snapUp == snapBefore {
		t.Fatal("0163 changed nothing (non-vacuity)")
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("migrate 0163 down: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapBefore {
		t.Fatalf("0163 down must restore the 0162 schema byte for byte:\n%s", m132Diff(snapBefore, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0163 up again: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("0163 up again must equal the first up:\n%s", m132Diff(snapUp, got))
	}
}

func TestMigration0162_UpDownUp_SnapshotAndDownRefusal(t *testing.T) {
	ctx := context.Background()
	pool := m132Scratch(t, "m0162_")
	if _, err := pool.MigrateUp(ctx, m132Dir(t, migration0162Version-1)); err != nil {
		t.Fatalf("migrate to 0161: %v", err)
	}
	snapBefore := m132Snapshot(t, pool)
	full := m132Dir(t, migration0162Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0162 up: %v", err)
	}
	snapUp := m132Snapshot(t, pool)
	if snapUp == snapBefore {
		t.Fatal("0162 changed nothing (non-vacuity)")
	}
	for _, want := range []string{
		"column:withdrawal_requests:reversal_ledger_transaction_id:uuid:YES:",
		"index:ledger_transactions:ledger_transactions_one_withdrawal_reversal:",
		"index:withdrawal_approvals:withdrawal_approvals_one_reverse_decision_per_approver:",
		"constraint:withdrawal_approvals:withdrawal_approvals_reverse_reason_shape:",
	} {
		if !strings.Contains(snapUp, want) {
			t.Fatalf("0162 up must create %q", want)
		}
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("migrate 0162 down: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapBefore {
		t.Fatalf("0162 down must restore the 0161 schema byte for byte:\n%s", m132Diff(snapBefore, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0162 up again: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("0162 up again must equal the first up:\n%s", m132Diff(snapUp, got))
	}

	// Down refusal: one 'reverse' decision row is enough (rows are never deleted to make a down pass).
	f := a5ScratchCompleted(t, pool)
	staff := uuid.New()
	person := uuid.New()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, f.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, 'x', 'finance', 'active', $4)`, staff, f.TenantID, staff.String()+"@example.com", person); err != nil {
			return err
		}
		return a5Vote(ctx, tx, f, staff, ReversalReasonPrefix+strings.Repeat("9a", 32))
	}); err != nil {
		t.Fatalf("plant a reverse decision: %v", err)
	}
	_, err := pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "WD097") {
		t.Fatalf("0162 down must refuse WD097 while a reversal row exists, got %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("a refused down must change nothing:\n%s", m132Diff(snapUp, got))
	}
}

// a5ScratchCompleted builds a completed request on the scratch database through the Go writers (the
// owner role there: the 0132/0162 rules are role-independent).
func a5ScratchCompleted(t *testing.T, pool *db.Pool) WithdrawalRequest {
	t.Helper()
	f := seedFixture(t, pool, 10_000)
	ref := "a5m-" + uuid.NewString()[:8]
	wr := mustRequestWithdrawal(t, pool, f, 100, "a5m-"+uuid.NewString())
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MoveToPendingReview(ctx, tx, wr.ID) })
	approver := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, decision,
			threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, $3, 'approve', 0, 100)`, f.tenantID, wr.ID, approver)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, wr.ID)
		return err
	})
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MarkSubmitted(ctx, tx, wr.ID, "a5psp", "out-"+ref) })
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return Complete(ctx, tx, wr.ID, "a5psp", ref) })
	var got WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = GetByID(ctx, tx, wr.ID)
		return err
	})
	return got
}

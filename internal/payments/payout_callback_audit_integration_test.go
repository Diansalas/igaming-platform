//go:build integration

// PAY-PAYOUT-CALLBACK-AUDIT-1: the payout receipt-callback cells that park an attempt (T14 success
// after a decline, T15 success for a never-sent attempt, a mismatched amount/asset success, a
// tombstoned reference) write ONE payments.payout_callback_dispute audit row in the same
// transaction as the dispute, before the B12 raise. No state-machine change, no money movement.
package payments

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

type r8Cell struct {
	name   string
	reason string       // closed terminal reason == closed alert reason
	before AttemptState // attempt state the callback finds (the state after is always disputed)
	ref    string
	amount int64
	setup  func(t *testing.T, e rbEnv) (withdrawal.WithdrawalRequest, PaymentAttempt)
}

func r8Cells() []r8Cell {
	return []r8Cell{
		{name: "T14_success_after_payout_declined", reason: "success_after_payout_declined", before: AttemptDeclined, ref: "r8-t14-ref", amount: 500,
			setup: func(t *testing.T, e rbEnv) (withdrawal.WithdrawalRequest, PaymentAttempt) {
				wr, a := e.claim(t, "r8-t14")
				e.b12Decline(t, wr, a)
				return wr, a
			}},
		{name: "T15_success_for_never_sent_attempt", reason: TerminalReasonSuccessForNeverSentAttempt, before: AttemptCreated, ref: "r8-t15-ref", amount: 500,
			setup: func(t *testing.T, e rbEnv) (withdrawal.WithdrawalRequest, PaymentAttempt) {
				wr, a := notSentPayoutAttempt(t, e.pool, e.orch, e.f, 500, "r8-t15")
				if a.State != AttemptCreated {
					t.Fatalf("setup: want created, got %s", a.State)
				}
				return wr, a
			}},
		{name: "callback_amount_asset_mismatch", reason: TerminalReasonCallbackAmountAssetMismatch, before: AttemptSubmitting, ref: "r8-cam-ref", amount: 499,
			setup: func(t *testing.T, e rbEnv) (withdrawal.WithdrawalRequest, PaymentAttempt) {
				return e.claim(t, "r8-cam")
			}},
		{name: "tombstone_precedes_success", reason: TerminalReasonTombstonePrecedesSuccess, before: AttemptSubmitting, ref: "r8-tomb-ref", amount: 500,
			setup: func(t *testing.T, e rbEnv) (withdrawal.WithdrawalRequest, PaymentAttempt) {
				ref := "r8-tomb-ref"
				if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
						TenantID: e.f.tenantID, TransactionType: ledger.TxTombstone,
						IdempotencyKey: "tombstone:" + e.pid + ":" + ref, ProviderID: &e.pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
					})
					return err
				}); err != nil {
					t.Fatalf("seed tombstone: %v", err)
				}
				return e.claim(t, "r8-tomb")
			}},
	}
}

func (e rbEnv) r8Events(t *testing.T) int {
	t.Helper()
	return fpCount(t, e.pool, e.f.tenantID, `SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1`, e.f.tenantID)
}

func (e rbEnv) r8DisputeAudits(t *testing.T, attemptID uuid.UUID) int {
	t.Helper()
	return e.b12AuditCount(t, auditActionPayoutCallbackDispute, attemptID)
}

// Each cell: exactly one audit row with the right attempt, tenant, actor, outcome and closed
// context; the dispute and the B12 alert are as before; no money moves; a replay adds nothing.
func TestPayoutCallbackAudit_EachCell_OneRow_CorrectContext_ReplayIdempotent_NoMoney(t *testing.T) {
	pool := depositV2ScratchPool(t)
	for i, c := range r8Cells() {
		t.Run(c.name, func(t *testing.T) {
			pid := fmt.Sprintf("mock-r8-cell-%d", i)
			f, orch, _ := fpOrch(t, pool, pid)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
			other, _, _ := fpOrch(t, pool, pid+"-other")
			wr, a := c.setup(t, e)
			snap := e.b12Snapshot(t, wr)
			if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != c.before {
				t.Fatalf("setup: want %s, got %s", c.before, got.State)
			}

			e.b12Callback(t, a, OutcomeSucceeded, c.ref, c.amount)

			got := mustGetAttempt(t, pool, f.tenantID, a.ID)
			if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != c.reason {
				t.Fatalf("want disputed/%s, got %s %v", c.reason, got.State, got.TerminalReason)
			}
			rows := r8ReadAudit(t, e, f.tenantID, auditActionPayoutCallbackDispute, a.ID)
			if len(rows) != 1 {
				t.Fatalf("want exactly one %s row, got %d", auditActionPayoutCallbackDispute, len(rows))
			}
			r := rows[0]
			if r.ActorType != "system" || r.Outcome != "denied" || r.TargetType != "payment_attempt" || r.TargetID != a.ID.String() {
				t.Fatalf("audit shape: %+v", r)
			}
			m := r.Metadata
			want := map[string]any{
				"terminal_reason": c.reason, "attempt_state_before": string(c.before), "attempt_state_after": "disputed",
				"evidence": "callback", "provider_id": pid, "withdrawal_request_id": wr.ID.String(),
				"stored_amount": float64(500), "stored_asset_code": "EUR",
				"echoed_amount": float64(c.amount), "echoed_asset_code": "EUR", "echoed_provider_reference": c.ref,
			}
			for k, v := range want {
				if m[k] != v {
					t.Fatalf("audit metadata[%s] = %v, want %v (all: %v)", k, m[k], v, m)
				}
			}
			if len(m) != len(want) {
				t.Fatalf("audit metadata has unexpected keys: %v", m)
			}
			// Tenant binding: another tenant's session reads none of it.
			if n := len(r8ReadAudit(t, e, other.tenantID, auditActionPayoutCallbackDispute, a.ID)); n != 0 {
				t.Fatalf("another tenant read %d rows", n)
			}
			// The alert is unchanged from B12 (one P1, reason == the cell's reason).
			e.b12AssertOneAlert(t, a.ID, c.reason, c.ref)
			e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: snap})
			if e.r8Events(t) != 1 {
				t.Fatalf("want exactly one receipt row, got %d", e.r8Events(t))
			}

			// Replays (the byte-identical redelivery and a stale-snapshot redelivery): receipt dedupe,
			// the attempt is already disputed -> no new audit row, no alert occurrence, no money.
			for k := 0; k < 3; k++ {
				e.b12Callback(t, a, OutcomeSucceeded, c.ref, c.amount)
			}
			if n := e.r8DisputeAudits(t, a.ID); n != 1 {
				t.Fatalf("replay added audit rows: %d", n)
			}
			e.b12AssertOneAlert(t, a.ID, c.reason) // occurrences == 1
			if e.r8Events(t) != 1 {
				t.Fatalf("replay must dedupe the receipt, got %d rows", e.r8Events(t))
			}
			e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: snap})
		})
	}
}

// The park, its audit row and its alert are one transaction. An audit failure rolls the whole
// delivery back (nothing parked, no receipt, no alert); redelivery converges to one of each. A
// transient alert failure also rolls the audit row back; a deterministic alert failure keeps the
// dispute and its audit row (the audit is written before the raise, never after it).
func TestPayoutCallbackAudit_Atomicity_AuditAndAlertFailureSemantics(t *testing.T) {
	pool := depositV2ScratchPool(t)
	for i, c := range r8Cells() {
		t.Run(c.name+"/audit_failure_rolls_back_then_converges", func(t *testing.T) {
			pid := fmt.Sprintf("mock-r8-af-%d", i)
			f, orch, _ := fpOrch(t, pool, pid)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
			wr, a := c.setup(t, e)
			snap := e.b12Snapshot(t, wr)
			t.Run("faulty", func(t *testing.T) {
				r8InstallAuditFailure(t, e, auditActionPayoutCallbackDispute)
				if err := e.b12CallbackErr(a, OutcomeSucceeded, c.ref, c.amount); err == nil {
					t.Fatalf("an audit failure must propagate, never be swallowed")
				}
				if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != c.before {
					t.Fatalf("the park must roll back with its audit row, got %s", got.State)
				}
				if e.r8Events(t) != 0 || e.r8DisputeAudits(t, a.ID) != 0 {
					t.Fatalf("receipt/audit rows survived the rollback")
				}
				if rows := e.b12Rows(t); len(rows) != 0 {
					t.Fatalf("no alert on a rolled-back delivery: %+v", rows)
				}
				e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: snap})
			})
			if err := e.b12CallbackErr(a, OutcomeSucceeded, c.ref, c.amount); err != nil {
				t.Fatalf("redelivery: %v", err)
			}
			if n := e.r8DisputeAudits(t, a.ID); n != 1 {
				t.Fatalf("want one audit row after recovery, got %d", n)
			}
			e.b12AssertOneAlert(t, a.ID, c.reason)
			e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: snap})
		})
		t.Run(c.name+"/transient_alert_failure_rolls_back_the_audit_row", func(t *testing.T) {
			pid := fmt.Sprintf("mock-r8-tr-%d", i)
			f, orch, _ := fpOrch(t, pool, pid)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
			_, a := c.setup(t, e)
			alertinject.Install(t, pool, f.tenantID, alertinject.Persistent, "40P01")
			if err := e.b12CallbackErr(a, OutcomeSucceeded, c.ref, c.amount); err == nil {
				t.Fatalf("a transient alert failure must propagate")
			}
			if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != c.before {
				t.Fatalf("whole transaction must roll back, got %s", got.State)
			}
			if n := e.r8DisputeAudits(t, a.ID); n != 0 {
				t.Fatalf("the audit row must roll back with the dispute, got %d", n)
			}
		})
		t.Run(c.name+"/deterministic_alert_failure_keeps_dispute_and_audit", func(t *testing.T) {
			pid := fmt.Sprintf("mock-r8-det-%d", i)
			f, orch, _ := fpOrch(t, pool, pid)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
			wr, a := c.setup(t, e)
			snap := e.b12Snapshot(t, wr)
			alertinject.Install(t, pool, f.tenantID, alertinject.Persistent, "P0001")
			if err := e.b12CallbackErr(a, OutcomeSucceeded, c.ref, c.amount); err != nil {
				t.Fatalf("a deterministic alert failure must not fail the delivery: %v", err)
			}
			if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptDisputed {
				t.Fatalf("the dispute must commit, got %s", got.State)
			}
			if n := e.r8DisputeAudits(t, a.ID); n != 1 {
				t.Fatalf("the audit row (written before the raise) must commit, got %d", n)
			}
			e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: snap})
		})
	}
}

// Existing behaviour is untouched: cells that do NOT park a payout write no dispute audit row (a
// matching success settles; a pending callback binds; a decline releases), and a decline then a
// success keeps the B12 single-alert behaviour.
func TestPayoutCallbackAudit_NonParkingCells_WriteNoDisputeRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-r8-nopark")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-r8-nopark"}

	wr, a := e.claim(t, "r8-np-ok")
	e.b12Callback(t, a, OutcomeSucceeded, "r8-np-ref", 500) // matching amount/asset: settles
	if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("a matching callback settles, got %s", got.State)
	}
	if n := e.r8DisputeAudits(t, a.ID); n != 0 {
		t.Fatalf("a settling callback writes no dispute audit, got %d", n)
	}
	e.b12Callback(t, a, OutcomeSucceeded, "r8-np-ref", 500) // duplicate success: no-op
	_ = wr

	wr2, a2 := e.claim(t, "r8-np-decl")
	e.b12Callback(t, a2, OutcomeDeclined, "r8-np-ref2", 500)
	if got := mustGetAttempt(t, pool, f.tenantID, a2.ID); got.State != AttemptDeclined {
		t.Fatalf("a decline callback declines, got %s", got.State)
	}
	if n := e.r8DisputeAudits(t, a2.ID); n != 0 {
		t.Fatalf("a decline writes no dispute audit, got %d", n)
	}
	if w := fpReqState(t, pool, f, wr2.ID); w.State != withdrawal.StateFailed {
		t.Fatalf("the decline releases the hold, got %s", w.State)
	}
	if rows := e.b12Rows(t); len(rows) != 0 {
		t.Fatalf("no alert for non-parking cells: %+v", rows)
	}
	loAssertBalanced(t, pool, f.tenantID)
}

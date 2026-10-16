//go:build integration

package payments

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2e / H-W1 second half (LF-A11-8): both M2 kinds are refused unless the provider's payment_statement
// reconciliation is current (last run succeeded, not truncated, no later failure, not stale).

// k3StatementRun runs the REAL payment_statement stream once for (tenant, mock's provider) over the MOCK statement
// source, in continuation mode (as the production scoped sweep does); keys seal the import when given.
func k3StatementRun(t *testing.T, pool *db.Pool, tenantID uuid.UUID, mock *MockProvider, keys statement.ImportSealKeys) reconciliation.StreamOutcome {
	t.Helper()
	src := NewMockStatementSource(mock, MockCredentialResolver{})
	now := time.Now()
	out := reconciliation.ReconcilePaymentStatementForTenant(context.Background(), pool, nil, tenantID, now.Add(-time.Hour), now, src,
		reconciliation.PaymentStatementOptions{Continuation: true, ImportKeys: keys})
	if out.Err != nil {
		t.Fatalf("payment_statement run: %+v", out)
	}
	return out
}

// f2eRecordFailure audits a payment_statement failure for the provider exactly as the stream does (the stream's
// own fail() writes this row; reproducing a real fetch failure needs no more than its audit row here).
func f2eRecordFailure(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider string) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run_failed",
			TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
			Metadata: map[string]any{"stream": "payment_statement", "phase": "fetch", "provider_id": provider, "severity": "P1"}})
	}); err != nil {
		t.Fatal(err)
	}
}

func f2eRequestM2(w *k3World, attempt uuid.UUID, kind ResolutionKind) (ManualResolution, error) {
	return w.request(w.f1, w.m2In(attempt, kind))
}

func TestF2e_M2_RefusedWithoutASuccessfulRun_AdmittedAfterOne(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1, noStatementRun: true})
	_, a := w.ambiguousPayout(500)
	for _, kind := range []ResolutionKind{ResolutionM2DeclareNotPaid, ResolutionM2DeclarePaid} {
		_, err := f2eRequestM2(w, a.ID, kind)
		if !errors.Is(err, ErrResolutionReconciliationNotCurrent) || ClassifyResolutionError(err) != ResolutionErrPrecondition {
			t.Fatalf("%s with no payment_statement run: want ErrResolutionReconciliationNotCurrent (precondition), got %v", kind, err)
		}
	}
	if n := w.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.f.tenantID); n != 0 {
		t.Fatalf("a refused submission leaves nothing, got %d rows", n)
	}
	k3StatementRun(t, w.pool, w.f.tenantID, w.mock, nil)
	if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); err != nil {
		t.Fatalf("after a successful run the M2 is admitted: %v", err)
	}
}

func TestF2e_M2_RefusedAfterAFailedOrTruncatedRunOrWhenStale(t *testing.T) {
	t.Run("failed_after_success", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(500)
		time.Sleep(5 * time.Millisecond)
		f2eRecordFailure(t, w.pool, w.f.tenantID, w.provider)
		if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
			t.Fatalf("a failure after the last success must refuse: %v", err)
		}
		// Another provider's failure does not count.
		w2 := newK3World(t, k3Opts{base: 1})
		_, a2 := w2.ambiguousPayout(500)
		f2eRecordFailure(t, w2.pool, w2.f.tenantID, "some-other-psp")
		if _, err := f2eRequestM2(w2, a2.ID, ResolutionM2DeclareNotPaid); err != nil {
			t.Fatalf("another provider's failure must not refuse: %v", err)
		}
		// A new successful run clears it.
		k3StatementRun(t, w.pool, w.f.tenantID, w.mock, nil)
		if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); err != nil {
			t.Fatalf("a successful run after the failure re-admits: %v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1, noStatementRun: true})
		w.ambiguousPayout(500)
		_, a := w.ambiguousPayout(500)
		src := NewMockStatementSource(w.mock, MockCredentialResolver{})
		out := reconciliation.ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src,
			reconciliation.PaymentStatementOptions{Continuation: true, RunLineBound: 1})
		if out.Err != nil || !out.Truncated {
			t.Fatalf("setup: want a truncated run: %+v", out)
		}
		if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
			t.Fatalf("a truncated last run must refuse: %v", err)
		}
	})
	t.Run("stale", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(500)
		w.svc = w.svc.WithStatementEvidenceMaxAge(time.Millisecond)
		time.Sleep(20 * time.Millisecond)
		if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
			t.Fatalf("a stale last run must refuse: %v", err)
		}
	})
}

// Execution re-checks: a failure audited between the request and the final approval ends the resolution
// refused_at_execution (reconciliation_not_current), nothing posted, the attempt and the withdrawal unchanged.
func TestF2e_M2_ExecutionRefusedWhenReconciliationFailsAfterTheRequest(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(500)
	res, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	f2eRecordFailure(t, w.pool, w.f.tenantID, w.provider)
	txBefore := w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID)
	out, err := w.decide(w.f2, res, ResolutionApprove)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !out.Refused || out.Resolution.State != ResolutionRefusedAtExecute || out.Resolution.RefusalCode == nil || *out.Resolution.RefusalCode != resolutionRefusedReconNotCurrent {
		t.Fatalf("want refused_at_execution / reconciliation_not_current, got %+v", out)
	}
	if n := w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID); n != txBefore {
		t.Fatal("a refused execution posts nothing")
	}
	if got := mustGetAttempt(t, w.pool, w.f.tenantID, a.ID); got.State != AttemptAmbiguous {
		t.Fatalf("the attempt is unchanged: %s", got.State)
	}
	if got := w.withdrawalOf(wr.ID); got.State != "submitted" {
		t.Fatalf("the withdrawal is unchanged: %s", got.State)
	}
}

// SR11-10 for the gate: with import seal keys, only a run backed by a SEALED import that verifies is evidence; a
// run row plus an unsealed import forged as the runtime role is not.
func TestF2e_M2_WithKeys_OnlyASealedRunIsEvidence(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1, noStatementRun: true})
	m := make([]byte, 32)
	fp := make([]byte, 32)
	for i := range m {
		m[i], fp[i] = byte(i+1), byte(200-i)
	}
	keys, err := payoutinstrument.NewKeys("f2e", map[string][]byte{"f2e": m}, "f2e-fp", map[string][]byte{"f2e-fp": fp})
	if err != nil {
		t.Fatal(err)
	}
	w.svc = w.svc.WithImportSealKeys(keys)
	_, a := w.ambiguousPayout(500)
	// The A11-mode evidence (an unsealed genuine run) is NOT evidence once the process has keys.
	k3StatementRun(t, w.pool, w.f.tenantID, w.mock, nil)
	if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
		t.Fatalf("an unsealed run must not be evidence when the process has keys: %v", err)
	}
	k3StatementRun(t, w.pool, w.f.tenantID, w.mock, keys)
	if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); err != nil {
		t.Fatalf("a sealed, verifying run is evidence: %v", err)
	}
}

func TestF2e_ImportedByServiceRestatementPinned(t *testing.T) {
	if importedByServicePaymentStatement != reconciliation.ImportedByServicePaymentStatement {
		t.Fatalf("restatement drifted: %q vs %q", importedByServicePaymentStatement, reconciliation.ImportedByServicePaymentStatement)
	}
}

// Without keys the A11 rule applies to the evidence: a run row with NO backing import (written as the world's
// unprivileged session role) is not evidence of a successful run.
func TestF2e_M2_UnbackedRunRowIsNotEvidence(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1, noStatementRun: true})
	_, a := w.ambiguousPayout(500)
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated)
			VALUES ($1, 'payment_statement', now() - interval '1 hour', now(), 'clean', now(), $2, false)`, w.f.tenantID, w.provider)
		return err
	})
	if _, err := f2eRequestM2(w, a.ID, ResolutionM2DeclareNotPaid); !errors.Is(err, ErrResolutionReconciliationNotCurrent) {
		t.Fatalf("a run row with no backing import must not count as a successful run: %v", err)
	}
}

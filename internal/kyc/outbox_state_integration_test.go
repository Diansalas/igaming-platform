//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 section 10.5 test 43 / 10.8 test 59): the staff-visible
// derived submission state over REAL outbox rows (identity-compliance C1/R1/R2),
// and "no idle-in-transaction session while the vendor is being called".

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func staffState(t *testing.T, r *rig, id uuid.UUID) SubmissionState {
	t.Helper()
	var st SubmissionState
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = StaffSubmissionState(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatalf("StaffSubmissionState: %v", err)
	}
	return st
}

// 43. T-I: each state combination over real rows, staff read, tenant-scoped.
func TestOutboxState_43_TI_StaffDerivedState(t *testing.T) {
	r := newRig(t)

	// none: a verification with no outbox row at all.
	bare := rawOrphan(t, r.pool, r.f)
	if got := staffState(t, r, bare); got != (SubmissionState{State: SubmissionNone}) {
		t.Errorf("no rows: %+v", got)
	}

	// queued: create pending.
	second := seedSecondAccount(t, r.pool, r.f)
	vq, _ := requestCreate(t, r.pool, second, "mock")
	if got := staffState(t, r, vq.ID); got.State != SubmissionQueued {
		t.Errorf("create pending: %+v", got)
	}

	// failed: create failed_terminal (class shown, nothing else).
	third := seedSecondAccount(t, r.pool, r.f)
	vf, _ := requestCreate(t, r.pool, third, "mock")
	passUntilQuiet(t, workerFor(r.pool, mismatchedKYCOutboundResolver{}, r.spy))
	if got := staffState(t, r, vf.ID); got != (SubmissionState{State: SubmissionFailed, LastErrorClass: "credential_binding_mismatch"}) {
		t.Errorf("create failed_terminal: %+v", got)
	}

	// sent, then the R2 stranded case: create sent, latest submit cancelled
	// provider_deconfigured MUST surface as cancelled, not sent.
	vs := r.createSent()
	if got := staffState(t, r, vs.ID); got.State != SubmissionSent {
		t.Errorf("create sent: %+v", got)
	}
	seedDocument(t, r.pool, r.f, vs.ID, DocumentPassport, "p.png")
	orch := NewOrchestrator(map[string]KYCProvider{"mock": r.spy, "mock2": renamedMock{NewMockKYCProvider(), "mock2"}}, nil)
	passUntilQuiet(t, newScopedWorker(r.pool, orch, NewMockOutboundResolver()))
	if got := staffState(t, r, vs.ID); got != (SubmissionState{State: SubmissionCancelled, CancelReason: "provider_deconfigured"}) {
		t.Errorf("R2 stranded case (sent + cancelled provider_deconfigured): %+v", got)
	}
	// ...and a re-upload's NEW pending row is the latest submit: queued again.
	seedDocument(t, r.pool, r.f, vs.ID, DocumentSelfie, "s.png")
	if got := staffState(t, r, vs.ID); got.State != SubmissionQueued {
		t.Errorf("a fresh pending submit row must show queued: %+v", got)
	}
	passUntilQuiet(t, r.w)
	if got := staffState(t, r, vs.ID); got.State != SubmissionSent {
		t.Errorf("once the newer submit is sent: %+v", got)
	}

	// The batch read agrees and is tenant-scoped: another tenant sees none.
	b := seedFixture(t, r.pool)
	if err := r.pool.WithTenant(context.Background(), b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		m, err := StaffSubmissionStates(ctx, tx, []uuid.UUID{vs.ID, vq.ID, vf.ID})
		if err != nil {
			return err
		}
		for id, st := range m {
			if st.State != SubmissionNone {
				t.Errorf("tenant B must see no outbox state for tenant A's verification %s: %+v", id, st)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 59. No transaction is open while the vendor is being called on the worker
// path: no `idle in transaction` session exists during the call, with a positive
// control proving the probe can see one.
func TestOutboxState_59_NoIdleInTransactionDuringVendorCall(t *testing.T) {
	r := newRig(t)
	probe := rtPool(t, 2)
	idle := func(p *db.Pool) int {
		var n int
		if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			                          WHERE datname = current_database() AND state = 'idle in transaction' AND pid <> pg_backend_pid()`).Scan(&n)
		}); err != nil {
			t.Fatalf("probe pg_stat_activity: %v", err)
		}
		return n
	}

	// Positive control: with a transaction deliberately held open elsewhere the
	// probe sees it.
	holder := rtPool(t, 2)
	var seen int
	if err := holder.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
			return err
		}
		seen = idle(probe)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen < 1 {
		t.Fatalf("vacuity: the probe must see a held-open transaction, saw %d", seen)
	}

	var duringCreate, duringSubmit = -1, -1
	r.createImpl = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		duringCreate = idle(probe)
		return ProviderResult{ProviderReference: "idle-probe-ref-" + uuid.NewString()[:8], Outcome: ProviderPending, Reason: "created"}, nil
	}
	v := r.createSent()
	r.submitImpl = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		duringSubmit = idle(probe)
		return ProviderResult{ProviderReference: ref, Outcome: ProviderReviewRequired, Reason: "manual_review"}, nil
	}
	seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")
	passUntilQuiet(t, r.w)
	if duringCreate != 0 || duringSubmit != 0 {
		t.Fatalf("no transaction may be open during a vendor call: idle-in-transaction sessions during create=%d submit=%d", duringCreate, duringSubmit)
	}
}

//go:build integration

package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
)

// C-47 (LF D-9): Go <-> SQL allow-list parity. payment_m2_admits takes the OLD
// state and reason as ARGUMENTS, so with an executing resolution of the right
// kind in this transaction the function's answer IS the SQL allow-list; it must
// equal M2ResolvableDispute for every classified reason, NULL, an unknown reason
// and every state.
func TestK3_C47_AllowListParityGoAndSQL(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	check := func(kind ResolutionKind, newState string) {
		_, a := w.ambiguousPayout(120)
		r := w.mustRequest(w.f1, w.m2In(a.ID, kind))
		reasons := []*string{nil}
		for reason := range PayoutDisputeReasons() {
			rr := reason
			reasons = append(reasons, &rr)
		}
		unknown := "some_future_payout_reason"
		prefixed := "invalid_provider_reference:control_char"
		reasons = append(reasons, &unknown, &prefixed)
		states := []AttemptState{AttemptCreated, AttemptSubmitting, AttemptPending, AttemptAmbiguous, AttemptSucceeded, AttemptDeclined, AttemptRejected, AttemptDisputed}
		cases, admitted := 0, 0
		if err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
			for _, st := range states {
				for _, rs := range reasons {
					var got bool
					if err := tx.QueryRow(ctx, `SELECT payment_m2_admits($1, $2, $3, $4, $5, 'operator')`,
						a.ID, string(st), rs, *a.WithdrawalRequestID, newState).Scan(&got); err != nil {
						return err
					}
					want := M2ResolvableDispute(st, rs)
					cases++
					if got {
						admitted++
					}
					if got != want {
						t.Errorf("%s -> %s: state=%s reason=%v: SQL admits=%v, Go admits=%v", kind, newState, st, rs, got, want)
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("parity run: %v", err)
		}
		if cases < len(states)*5 || admitted == 0 || admitted == cases {
			t.Fatalf("vacuous parity run: %d cases, %d admitted", cases, admitted)
		}
	}
	check(ResolutionM2DeclarePaid, "succeeded")
	check(ResolutionM2DeclareNotPaid, "declined")
	// Evidence other than operator is never admitted; a wrong target/kind pair is not either.
	_, a := w.ambiguousPayout(120)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid))
	if err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
		for _, ev := range []string{"sync", "callback", "query_status", "sweeper", "platform", "legacy"} {
			var got bool
			if err := tx.QueryRow(ctx, `SELECT payment_m2_admits($1, 'ambiguous', NULL, $2, 'succeeded', $3)`, a.ID, *a.WithdrawalRequestID, ev).Scan(&got); err != nil {
				return err
			}
			if got {
				t.Errorf("payment_m2_admits admitted %s evidence", ev)
			}
		}
		var got bool
		if err := tx.QueryRow(ctx, `SELECT payment_m2_admits($1, 'ambiguous', NULL, $2, 'declined', 'operator')`, a.ID, *a.WithdrawalRequestID).Scan(&got); err != nil {
			return err
		}
		if got {
			t.Error("a declare-paid resolution must not admit a declined target")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// C-5c: the attempt guard's own re-check (payment_m2_admits) of the withdrawal
// state at WRITE time. The resolution guard checks it at insert and at
// `-> executing`, and withdrawal.Complete/Fail CAS on it; this is the fourth,
// last-line restatement, reached only by moving the withdrawal inside the
// executing transaction.
func TestK3_C5c_AdmitsRechecksTheWithdrawalStateAtWriteTime(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(110)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
		// Sanity: while the withdrawal is submitted the write is admitted.
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='declined', last_evidence_kind='operator', resolved_at=now(), next_action_at=NULL WHERE id = $1`, a.ID)
			if err != nil {
				t.Errorf("the matching write was refused while the withdrawal is submitted: %v", err)
			}
			return errK3Rollback
		}); err != nil && err != errK3Rollback {
			return err
		}
		// Move the withdrawal out of `submitted` inside the executing transaction.
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'rejected' WHERE id = $1`, wr.ID); err != nil {
			t.Fatalf("setup: cannot move the withdrawal: %v", err)
		}
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='declined', last_evidence_kind='operator', resolved_at=now(), next_action_at=NULL WHERE id = $1`, a.ID)
			return err
		}); err == nil {
			t.Error("the attempt guard admitted an operator write for a withdrawal that is no longer submitted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// T-12: payment_m2_admits returns false, never an error, in sessions that cannot
// see resolutions (the sweeper's WithTenant shape and a no-tenant session).
func TestK3_T12_AdmitsIsFalseNotAnErrorWhereResolutionsAreInvisible(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(130)
	w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid)) // a pending resolution exists
	for _, new := range []string{"succeeded", "declined"} {
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			var got bool
			if err := tx.QueryRow(ctx, `SELECT payment_m2_admits($1, 'ambiguous', NULL, $2, $3, 'operator')`, a.ID, *a.WithdrawalRequestID, new).Scan(&got); err != nil {
				t.Errorf("the tenant-GUC-only shape must not error: %v", err)
			}
			if got {
				t.Errorf("admits(%s) = true with no executing resolution visible", new)
			}
			return nil
		})
		if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			var got bool
			if err := tx.QueryRow(ctx, `SELECT payment_m2_admits($1, 'ambiguous', NULL, $2, $3, 'operator')`, a.ID, *a.WithdrawalRequestID, new).Scan(&got); err != nil {
				return err
			}
			if got {
				t.Errorf("admits(%s) = true in a no-tenant session", new)
			}
			return nil
		}); err != nil {
			t.Errorf("a no-tenant session must not error: %v", err)
		}
	}
	// A NULL attempt id or withdrawal id is false, not an error.
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT payment_m2_admits(NULL, 'ambiguous', NULL, NULL, 'succeeded', 'operator')`).Scan(&got); err != nil || got {
			t.Errorf("NULL arguments: got=%v err=%v, want false and no error", got, err)
		}
		return nil
	})
}

// T-3: the reconciliation session (app.tenant_id only, runtime role) sees
// executed m2_* rows and sees no pending row and no approvals.
func TestK3_T3_SystemReadSeesOnlyExecutedM2Rows(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a1 := w.ambiguousPayout(140)
	executed := w.executeM2(a1.ID, ResolutionM2DeclareNotPaid)
	_, a2 := w.ambiguousPayout(150)
	pending := w.mustRequest(w.f1, w.m2In(a2.ID, ResolutionM2DeclarePaid))
	d := w.disputedDeposit(5000)
	m1 := w.mustRequest(w.f1, w.m1In(d.ID, "awaiting_psp_refund"))
	if out, err := w.decide(w.f2, m1, ResolutionApprove); err != nil || !out.Executed {
		t.Fatalf("M1: %v %+v", err, out)
	}

	rows := w.sysQuery(`SELECT id::text AS id, kind, state FROM payment_manual_resolutions`)
	seen := map[string]string{}
	for _, r := range rows {
		seen[r["id"].(string)] = r["kind"].(string) + "/" + r["state"].(string)
	}
	if got := seen[executed.ID.String()]; got != "m2_declare_not_paid/executed" {
		t.Errorf("the system session must see the executed M2 row, got %q (all: %v)", got, seen)
	}
	if _, ok := seen[pending.ID.String()]; ok {
		t.Error("the system session sees a PENDING resolution")
	}
	// R-2 (tightened): an EXECUTED M1 row is invisible to the system session too.
	if got, ok := seen[m1.ID.String()]; ok {
		t.Errorf("the system session sees an executed M1 row (%q)", got)
	}
	if n := len(w.sysQuery(`SELECT 1 FROM payment_manual_resolution_approvals`)); n != 0 {
		t.Errorf("the system session sees %d approval rows", n)
	}
	// Z01: a PLAYER session (the beneficiary) sees no resolution row at all.
	if err := w.pool.WithPlayerScope(context.Background(), w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_manual_resolutions`).Scan(&n); err != nil {
			return nil // no privilege at all is also a refusal
		}
		if n != 0 {
			t.Errorf("a player session sees %d resolution rows", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// C-32 (QA W1, HD-PRH2-2 (c)): the sock-puppet cases for a payment_force_resolve
// grant. A tenant admin cannot approve a grant request (the approval is a
// platform decision), and a tenant-minted twin account of the same Person cannot
// approve it either.
func TestK3_C32_SockPuppetGrantRequestsAreRefused(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	ctx := context.Background()
	puppet := k3MkStaff(t, w.pool, w.f.tenantID, "finance", uuid.New())
	var reqID uuid.UUID
	if err := w.pool.WithPrincipalScope(ctx, w.f.tenantID, w.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := capability.CreateRequest(ctx, tx, w.f.tenantID, capability.NewRequestInput{
			GranteeStaffID: puppet.ID, Capability: capability.CapabilityPaymentForceResolveApprove, ReasonCode: "k3-sock"})
		reqID = r.ID
		return err
	}); err != nil {
		t.Fatalf("grant request: %v", err)
	}
	decide := func(principal uuid.UUID) error {
		return w.pool.WithPrincipalScope(ctx, w.f.tenantID, principal, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := capability.DecideAndGrant(ctx, tx, w.f.tenantID, reqID, "approve", "k3-sock")
			return err
		})
	}
	if err := decide(w.tenantAdmin.ID); err == nil {
		t.Error("a tenant admin approved a payment_force_resolve grant request")
	}
	twin := k3MkStaff(t, w.pool, w.f.tenantID, "tenant_admin", w.tenantAdmin.PersonID)
	if err := decide(twin.ID); err == nil {
		t.Error("a same-Person twin of the requester approved the grant request")
	}
	var n int
	if err := w.pool.WithPlatformAdmin(ctx, w.adminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grants WHERE request_id = $1`, reqID).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a refused sock-puppet approval left %d grant rows (%v)", n, err)
	}
}

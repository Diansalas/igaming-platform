//go:build integration

package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// setStaff mutates a tenant staff row by DB fixture (there is no suspend or
// role-change API: STAFF-LIFECYCLE-1).
func (w *k3World) setStaff(id uuid.UUID, column, value string) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE staff_users SET `+column+` = $2 WHERE id = $1`, id, value)
		if err == nil && tag.RowsAffected() != 1 {
			w.t.Fatalf("set staff %s: %d rows", column, tag.RowsAffected())
		}
		return err
	})
}

// mkUnlinkedStaff inserts a tenant staff member with NO linked Person.
func (w *k3World) mkUnlinkedStaff() k3Staff {
	w.t.Helper()
	s := k3Staff{ID: uuid.New(), TenantID: w.f.tenantID, Role: "finance"}
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, $2, $3, 'x', 'finance', 'active')`,
			s.ID, w.f.tenantID, "k3-np-"+s.ID.String()+"@test.invalid")
		return err
	})
	return s
}

// C-10 (AZ, S-12): the beneficiary may neither request nor approve; an unlinked
// staff Person is refused, for M1 and M2.
func TestK3_C10_BeneficiaryExclusion(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, pa := w.ambiguousPayout(100)
	dep := w.disputedDeposit(5000)
	ins := map[string]ResolutionRequestInput{
		"m2": w.m2In(pa.ID, ResolutionM2DeclareNotPaid),
		"m1": w.m1In(dep.ID, "awaiting_psp_refund"),
	}
	beneficiary := k3MkStaff(t, w.pool, w.f.tenantID, "finance", w.f.personID)
	w.grantTenant(beneficiary, capability.CapabilityPaymentForceResolveRequest)
	w.grantTenant(beneficiary, capability.CapabilityPaymentForceResolveApprove)
	unlinked := w.mkUnlinkedStaff()
	for name, in := range ins {
		t.Run(name+"/beneficiary_requester", func(t *testing.T) {
			_, err := w.request(beneficiary, in)
			k3RequireCode(t, err, "MR032")
		})
		t.Run(name+"/unlinked_requester", func(t *testing.T) {
			_, err := w.request(unlinked, in)
			k3RequireCode(t, err, "MR032")
		})
		t.Run(name+"/beneficiary_approver", func(t *testing.T) {
			r := w.mustRequest(w.f1, in)
			_, err := w.decide(beneficiary, r, ResolutionApprove)
			k3RequireCode(t, err, "MR032")
			if got := w.resolution(r.ID).State; got != ResolutionPending {
				t.Fatalf("state %s", got)
			}
			// cleanup the pending slot
			if _, err := w.svc.Cancel(k3Ctx(w.f1), w.target(w.f1), r.ID, ResolutionMeta{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	w.assertInvariants()
}

// C-11 (AZ): self-approval, the same Person under two principals, no grant, a
// grant revoked, a suspended actor, a policy author as requester: refused or not
// counted.
func TestK3_C11_AuthorizationMatrix(t *testing.T) {
	t.Run("self_approval", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		_, err := w.decide(w.f1, r, ResolutionApprove)
		k3RequireCode(t, err, "MR031")
	})
	t.Run("same_person_two_principals", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		twin := k3MkStaff(t, w.pool, w.f.tenantID, "finance", w.f1.PersonID)
		w.grantTenant(twin, capability.CapabilityPaymentForceResolveApprove)
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		_, err := w.decide(twin, r, ResolutionApprove)
		k3RequireCode(t, err, "MR031")
	})
	t.Run("no_grant", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		_, err := w.request(w.financeNoGrant, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		k3RequireCode(t, err, "MR003")
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		_, err = w.decide(w.financeNoGrant, r, ResolutionApprove)
		k3RequireCode(t, err, "MR003")
	})
	t.Run("approver_grant_revoked_before_final_approval_not_counted", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 2})
		_, a := w.ambiguousPayout(100)
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		if out, err := w.decide(w.f2, r, ResolutionApprove); err != nil || out.Executed || out.Counted != 1 {
			t.Fatalf("first approval: %v %+v", err, out)
		}
		w.revokeGrant(w.f2.ID, capability.CapabilityPaymentForceResolveApprove)
		out, err := w.decide(w.f3, r, ResolutionApprove)
		if err != nil || out.Executed || out.Counted != 1 {
			t.Fatalf("a revoked approver was counted: %v %+v", err, out)
		}
		if w.attempt(a.ID).State != AttemptAmbiguous {
			t.Fatal("attempt moved without a valid count")
		}
		w.assertInvariants()
	})
	t.Run("suspended_approver_not_counted", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 2})
		_, a := w.ambiguousPayout(100)
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		if out, err := w.decide(w.f2, r, ResolutionApprove); err != nil || out.Counted != 1 {
			t.Fatalf("first approval: %v %+v", err, out)
		}
		w.setStaff(w.f2.ID, "status", "suspended")
		out, err := w.decide(w.f3, r, ResolutionApprove)
		if err != nil || out.Executed || out.Counted != 1 {
			t.Fatalf("a suspended approver was counted: %v %+v", err, out)
		}
	})
	t.Run("suspended_actor_with_a_live_token_refused_S4", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		w.setStaff(w.f1.ID, "status", "suspended")
		_, err := w.request(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid)) // the JWT is still valid
		if err == nil || ResolutionToken(ClassifyResolutionError(err)) != TokenForceResolveNotPermitted {
			t.Fatalf("a suspended actor was admitted: %v", err)
		}
	})
	t.Run("demoted_actor_refused_S4", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		w.setStaff(w.f1.ID, "role", "support") // the token still says finance
		_, err := w.request(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		k3RequireCode(t, err, "MR003")
	})
	t.Run("policy_author_cannot_request_S2iii", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		// A platform principal sharing adminB's Person (the policy APPROVER; the
		// grant requester adminA must be a distinct Person) acts in the tenant under
		// a valid grant.
		twin := k3MkStaff(t, w.pool, uuid.Nil, "platform_admin", w.adminB.PersonID)
		w.grantActing(twin, capability.CapabilityPaymentForceResolveRequest)
		_, err := w.request(twin, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		k3RequireCode(t, err, "MR011")
	})
}

// T-6 (R-3(ii)): a principal holding ONLY ledger_adjustment grants can neither
// request nor approve a payment_force_resolve, in the tenant family and the
// acting family.
func TestK3_T6_CapabilityIsSpecific(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	laOnly := k3MkStaff(t, w.pool, w.f.tenantID, "finance", uuid.New())
	w.grantTenant(laOnly, capability.CapabilityLedgerAdjustmentInitiate)
	w.grantTenant(laOnly, capability.CapabilityLedgerAdjustmentApprove)
	_, err := w.request(laOnly, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	k3RequireCode(t, err, "MR003")
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	_, err = w.decide(laOnly, r, ResolutionApprove)
	k3RequireCode(t, err, "MR003")

	actingLA := k3MkStaff(t, w.pool, uuid.Nil, "platform_admin", uuid.New())
	w.grantActing(actingLA, capability.CapabilityLedgerAdjustmentInitiate)
	w.grantActing(actingLA, capability.CapabilityLedgerAdjustmentApprove)
	_, err = w.request(actingLA, w.m2In(a.ID, ResolutionM2DeclarePaid))
	k3RequireCode(t, err, "MR003")
	_, err = w.decide(actingLA, r, ResolutionApprove)
	k3RequireCode(t, err, "MR003")
	// A grant-only-for-approve principal cannot request.
	apOnly := k3MkStaff(t, w.pool, w.f.tenantID, "finance", uuid.New())
	w.grantTenant(apOnly, capability.CapabilityPaymentForceResolveApprove)
	_, err = w.request(apOnly, w.m2In(a.ID, ResolutionM2DeclarePaid))
	k3RequireCode(t, err, "MR003")
}

// T-4 / K3-S2 (R-3 (iv)): the DB recounts at pending -> executing. Too few
// counted approvals are refused by the DATABASE (a direct UPDATE), and the
// requester is re-checked at execution (revoked :request grant).
func TestK3_T4_DBRecountAndRequesterRecheck(t *testing.T) {
	t.Run("too_few_counted_approvals_refused_by_the_database", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 2})
		_, a := w.ambiguousPayout(100)
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		// Only one approval exists after f2's own, but two are required.
		err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error { return nil })
		k3RequireCode(t, err, "MR030")
		if w.resolution(r.ID).State != ResolutionPending {
			t.Fatal("state changed")
		}
	})
	t.Run("requester_grant_revoked_before_execution", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		w.revokeGrant(w.f1.ID, capability.CapabilityPaymentForceResolveRequest)
		// The DB refuses the executing UPDATE...
		err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error { return nil })
		k3RequireCode(t, err, "MR030")
		// ... and the executor leaves the resolution pending (no execution).
		out, err := w.decide(w.f2, r, ResolutionApprove)
		if err != nil || out.Executed || out.Resolution.State != ResolutionPending {
			t.Fatalf("executed with an invalid requester: %v %+v", err, out)
		}
		if w.attempt(a.ID).State != AttemptAmbiguous {
			t.Fatal("attempt moved")
		}
		w.assertInvariants()
	})
	t.Run("requester_suspended_before_execution", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		w.setStaff(w.f1.ID, "status", "suspended")
		out, err := w.decide(w.f2, r, ResolutionApprove)
		if err != nil || out.Executed {
			t.Fatalf("executed with a suspended requester: %v %+v", err, out)
		}
	})
}

// T-8 / R-5: a closed tenant admits only platform_acting actors - at insert, at
// approval and at counting (including a tenant closed AFTER submission).
func TestK3_T8_ClosedTenantActorScope(t *testing.T) {
	t.Run("tenant_scope_requester_refused_at_insert", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		w.setTenantStatus("closed")
		_, err := w.request(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		k3RequireCode(t, err, "MR030")
	})
	t.Run("tenant_scope_approver_refused_at_approval_insert", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		r := w.mustRequest(w.acting, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		w.setTenantStatus("closed")
		_, err := w.decide(w.f2, r, ResolutionApprove)
		k3RequireCode(t, err, "MR030")
	})
	t.Run("tenant_closed_after_submission_tenant_scope_requester_and_approvals_not_counted", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		wr, a := w.ambiguousPayout(100)
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		w.setTenantStatus("closed")
		// Even a platform_acting approver cannot execute a resolution whose requester
		// is tenant-scope in a closed tenant (K3-S2 requester_valid).
		out, err := w.decide(w.acting2, r, ResolutionApprove)
		if err != nil || out.Executed || out.Resolution.State != ResolutionPending {
			t.Fatalf("executed a tenant-scope requester's resolution in a closed tenant: %v %+v", err, out)
		}
		// The database refuses the executing UPDATE too.
		err = w.inExecutingActing(r, w.acting, func(ctx context.Context, tx pgx.Tx) error { return nil })
		k3RequireCode(t, err, "MR030")
		if w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted {
			t.Fatal("withdrawal moved")
		}
	})
	t.Run("platform_acting_actors_work_and_status_is_recorded_C21", func(t *testing.T) {
		for _, status := range []string{"suspended", "closed"} {
			w := newK3World(t, k3Opts{base: 1})
			wr, a := w.ambiguousPayout(100)
			w.setTenantStatus(status)
			r := w.mustRequest(w.acting, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
			out, err := w.decide(w.acting2, r, ResolutionApprove)
			if err != nil || !out.Executed {
				t.Fatalf("%s: acting path failed: %v %+v", status, err, out)
			}
			got := w.resolution(r.ID)
			if got.TenantStatusAtSubmission != status || got.TenantStatusAtExecution == nil || *got.TenantStatusAtExecution != status {
				t.Fatalf("%s: tenant status not recorded: %q / %v", status, got.TenantStatusAtSubmission, got.TenantStatusAtExecution)
			}
			if w.withdrawalOf(wr.ID).State != withdrawal.StateFailed {
				t.Fatalf("%s: withdrawal not released", status)
			}
		}
	})
	t.Run("suspended_tenant_scope_still_works_C21", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		w.setTenantStatus("suspended")
		w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	})
}

// T-9 (R-6): the attempt state or terminal reason changes between submission and
// execution -> refused_at_execution (the executor) and refused by the database
// (a direct UPDATE).
func TestK3_T9_PinnedFactualBasis(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	// The attempt is parked (still allow-listed) after the approvers' view.
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, "provider_reference_mismatch")
	})
	err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error { return nil })
	k3RequireCode(t, err, "MR030")
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil || !out.Refused || out.Resolution.State != ResolutionRefusedAtExecute || out.Resolution.RefusalCode == nil || *out.Resolution.RefusalCode != resolutionRefusedAttempt {
		t.Fatalf("want refused_at_execution/attempt_changed, got %v %+v", err, out)
	}
	if w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted || w.ledgerTxCount("withdrawal_failed") != 0 {
		t.Fatal("a stale approval moved money")
	}
	if !contains(w.auditActions("payment_manual_resolution", r.ID.String()), "payment.manual_resolution_refused") {
		t.Fatal("no refused audit row")
	}
	w.assertInvariants()
}

// T-5 / O-K1 (R-3 (v)): after a governed posting in this transaction every exit
// from `executing` other than `executed` is refused; but a PENDING M2 can still
// be cancelled after the withdrawal was failed by evidence.
func TestK3_T5_ExitsOutOfExecutingRefusedAfterPosting_OK1(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
		if err := applyOperatorResolution(ctx, tx, a, AttemptDeclined); err != nil {
			return err
		}
		if err := withdrawal.Fail(ctx, tx, wr.ID, DeclaredNotPaidReason); err != nil {
			return err
		}
		// A posting exists: refused_at_execution is refused (MR042); rejected,
		// cancelled and expired are not transitions out of executing at all.
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'refused_at_execution', refusal_code = 'x' WHERE id = $1`, r.ID)
			return err
		}); k3Code(err) != "MR042" {
			t.Errorf("refused_at_execution after a posting: want MR042, got %v", err)
		}
		for _, to := range []string{"rejected", "cancelled", "expired"} {
			to := to
			if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = $2 WHERE id = $1`, r.ID, to)
				return err
			}); k3Code(err) != "MR030" {
				t.Errorf("executing -> %s: want MR030, got %v", to, err)
			}
		}
		return nil
	})
	k3RequireNoErr(t, err, "T-5 body")

	// O-K1: a pending M2, then the withdrawal is failed by sweeper-style evidence;
	// the requester can still cancel.
	w2 := newK3World(t, k3Opts{base: 1})
	wr2, a2 := w2.ambiguousPayout(100)
	r2 := w2.mustRequest(w2.f1, w2.m2In(a2.ID, ResolutionM2DeclareNotPaid))
	w2.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := ApplyDecline(ctx, tx, a2.ID, DeclineEvidence{Evidence: EvidenceQueryStatus, Reason: "provider_declined", Stage: DeclineAfterAcceptance}); err != nil {
			return err
		}
		return withdrawal.Fail(ctx, tx, wr2.ID, "provider_declined")
	})
	got, err := w2.svc.Cancel(k3Ctx(w2.f1), w2.target(w2.f1), r2.ID, ResolutionMeta{})
	if err != nil || got.State != ResolutionCancelled {
		t.Fatalf("a pending M2 could not be cancelled after the withdrawal was failed by evidence: %v %+v", err, got)
	}
	// ... and a new M2 for the (now declined) attempt is no longer possible, but the
	// pending slot is free.
	if w2.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE attempt_id = $1 AND state = 'pending'`, a2.ID) != 0 {
		t.Fatal("the pending slot is still occupied")
	}
}

// C-13 (FL/RB): a failure in Complete/Fail rolls back everything; a committed
// `executing` is refused (the deferred check).
func TestK3_C13_FailureInjectionAndNoExecutingCommit(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid))

	testHookResolutionBeforePost = func(ctx context.Context, id uuid.UUID) error {
		return errors.New("injected failure before the posting")
	}
	_, err := w.decide(w.f2, r, ResolutionApprove)
	testHookResolutionBeforePost = nil
	if err == nil {
		t.Fatal("the injected failure was swallowed")
	}
	if got := w.resolution(r.ID).State; got != ResolutionPending {
		t.Fatalf("resolution state %s after a rolled-back execution", got)
	}
	if w.attempt(a.ID).State != AttemptAmbiguous || w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted || w.ledgerTxCount("withdrawal_completed") != 0 {
		t.Fatal("a failed execution left a partial effect")
	}
	if w.countRows(`SELECT count(*) FROM payment_manual_resolution_approvals WHERE resolution_id = $1`, r.ID) != 0 {
		t.Fatal("the approval of a rolled-back execution survived")
	}
	// The retry (no injection) executes once.
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("retry: %v %+v", err, out)
	}
	w.assertInvariants()

	// A resolution may never COMMIT in `executing`: the deferred check refuses.
	w2 := newK3World(t, k3Opts{base: 1})
	_, a2 := w2.ambiguousPayout(100)
	r2 := w2.mustRequest(w2.f1, w2.m2In(a2.ID, ResolutionM2DeclareNotPaid))
	err = w2.pool.WithPrincipalScope(context.Background(), w2.f.tenantID, w2.f2.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := k3InsertApproval(ctx, tx, w2.f.tenantID, r2, "approve"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executing' WHERE id = $1`, r2.ID)
		return err
	})
	k3RequireCode(t, err, "MR041")
}

// C-3 (ADV): M1 never links a ledger transaction or a target (CHECK + trigger);
// the executor never posts for M1.
func TestK3_C3_M1NeverLinksOrPosts(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	dep := w.disputedDeposit(5000)
	r := w.mustRequest(w.f1, w.m1In(dep.ID, "awaiting_psp_refund"))
	anyTx := w.sysQuery(`SELECT id FROM ledger_transactions WHERE tenant_id = $1 LIMIT 1`, w.f.tenantID)[0]["id"]
	// A direct attempt to attach a ledger transaction to an M1 resolution (a
	// tenant principal session that can see the pending row).
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET ledger_transaction_id = $2 WHERE id = $1`, r.ID, anyTx)
			if err == nil && tag.RowsAffected() != 1 {
				t.Errorf("setup: the UPDATE matched %d rows", tag.RowsAffected())
			}
			return err
		})
		if err == nil {
			t.Error("a ledger link was attached to a pending M1 resolution")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := w.ledgerTxCount("deposit") + w.ledgerTxCount("withdrawal_completed") + w.ledgerTxCount("withdrawal_failed") + w.ledgerTxCount("manual_adjustment")
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil || !out.Executed || out.Resolution.LedgerTransactionID != nil || out.Resolution.TargetState != nil {
		t.Fatalf("M1 execution: %v %+v", err, out)
	}
	after := w.ledgerTxCount("deposit") + w.ledgerTxCount("withdrawal_completed") + w.ledgerTxCount("withdrawal_failed") + w.ledgerTxCount("manual_adjustment")
	if before != after {
		t.Fatalf("M1 produced ledger transactions: %d -> %d", before, after)
	}
	if got := w.attempt(dep.ID); got.State != AttemptDisputed || !equalOptString(got.TerminalReason, dep.TerminalReason) || got.LastEvidenceKind != dep.LastEvidenceKind {
		t.Fatalf("M1 changed the attempt: %+v", got)
	}
	w.assertInvariants()
}

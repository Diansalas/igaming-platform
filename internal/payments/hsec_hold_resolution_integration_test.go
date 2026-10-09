//go:build integration

// HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 section 6, review conditions section 11):
// the governed four-eyes release of an `approved` withdrawal hold on a tenant/brand
// that is not active. The decisions under test (ADR 0095 section 44, 13-18): NO
// automatic release; the hold remains until a controlled staff resolution; request,
// approval and execution are four-eyes regardless of amount; no unilateral
// single-staff action; kill-switch semantics intact. Runtime role, private scratch
// database, synthetic data.
package payments

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// Happy path: an approved hold on a SUSPENDED tenant; the request and two distinct
// approvers (one of them is the platform-acting requester's peer) release the hold
// to player_cash exactly once; the withdrawal is rejected; the ledger is balanced;
// the projection equals its rebuild; every audit row exists.
func TestHSEC_HoldRelease_HappyPath_TwoApprovers(t *testing.T) {
	h := newHSR(t, 2)
	wr := h.hold(2500)
	if h.walletBalance("player_withdrawal_hold") != 2500 || h.walletBalance("player_cash") != 1_000_000-2500 {
		t.Fatalf("setup: hold=%d cash=%d", h.walletBalance("player_withdrawal_hold"), h.walletBalance("player_cash"))
	}
	h.suspendTenant()

	r := h.mustRequest(h.reqA, wr.ID)
	if r.State != ResolutionPending || r.Kind != HoldReleaseToPlayer || r.Amount != 2500 || r.AssetCode != "EUR" ||
		r.TenantStatusAtSubmission != "suspended" || r.BrandStatusAtSubmission != "active" ||
		r.WithdrawalStateAtSubmit != "approved" || r.RequiredAtSubmission != 2 || r.RequestedByScope != "platform_acting" {
		t.Fatalf("request row: %+v", r)
	}
	// NO automatic release: nothing moved on request.
	if got := h.wr(wr.ID); got.State != withdrawal.StateApproved || h.governedTxCount(wr.ID) != 0 {
		t.Fatalf("request alone moved money or state: %s, %d", got.State, h.governedTxCount(wr.ID))
	}
	out := h.mustDecide(h.apprB, r, ResolutionApprove)
	if out.Executed || out.Refused || out.Counted != 1 || out.Required != 2 {
		t.Fatalf("first approval: %+v", out)
	}
	if got := h.wr(wr.ID); got.State != withdrawal.StateApproved || h.governedTxCount(wr.ID) != 0 {
		t.Fatalf("one of two approvals moved money or state: %s", got.State)
	}
	out = h.mustDecide(h.apprC, r, ResolutionApprove)
	if !out.Executed || out.Refused || out.Counted != 2 {
		t.Fatalf("final approval: %+v", out)
	}

	done := h.res(r.ID)
	if done.State != ResolutionExecuted || done.LedgerTransactionID == nil || done.TenantStatusAtExecution == nil ||
		*done.TenantStatusAtExecution != "suspended" || done.BrandStatusAtExecution == nil || *done.BrandStatusAtExecution != "active" {
		t.Fatalf("executed row: %+v", done)
	}
	got := h.wr(wr.ID)
	if got.State != withdrawal.StateRejected || got.ReleaseLedgerTransactionID == nil || *got.ReleaseLedgerTransactionID != *done.LedgerTransactionID {
		t.Fatalf("withdrawal after release: %+v", got)
	}
	// The hold returned to the player's own cash on the SAME wallet; nothing else moved.
	if hold, cash := h.walletBalance("player_withdrawal_hold"), h.walletBalance("player_cash"); hold != 0 || cash != 1_000_000 {
		t.Fatalf("hold=%d cash=%d, want 0 and 1000000", hold, cash)
	}
	// Exact key, correlation, reversal and shape.
	if n := h.governedTxCount(wr.ID); n != 1 {
		t.Fatalf("governed release transactions = %d", n)
	}
	var typ string
	var corr, reverses uuid.UUID
	var provider *string
	var nEntries int
	h.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT transaction_type, correlation_id, reverses_transaction_id, provider_id FROM ledger_transactions WHERE id = $1`,
			*done.LedgerTransactionID).Scan(&typ, &corr, &reverses, &provider); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE ledger_transaction_id = $1`, *done.LedgerTransactionID).Scan(&nEntries)
	})
	if typ != "withdrawal_rejected" || corr != wr.ID || reverses != *wr.HoldLedgerTransactionID || provider != nil || nEntries != 2 {
		t.Fatalf("release tx: type=%s corr=%s reverses=%s provider=%v entries=%d", typ, corr, reverses, provider, nEntries)
	}
	h.assertInvariants()

	// Audit: request, both approvals, execution, plus the withdrawal transition - the
	// executed row names the actor, BOTH approvers, the withdrawal, the reason, the
	// resulting state and the statuses.
	for action, want := range map[string]int{
		"withdrawal.hold_resolution_requested": 1, "withdrawal.hold_resolution_approved": 1, "withdrawal.hold_resolution_executed": 1,
	} {
		if n := h.auditCount(action, r.ID); n != want {
			t.Errorf("audit %s = %d, want %d", action, n, want)
		}
	}
	if n := h.auditCount("withdrawal.hold_released_governed", wr.ID); n != 1 {
		t.Errorf("withdrawal.hold_released_governed audit = %d", n)
	}
	var md string
	var approvers int
	h.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT metadata::text, jsonb_array_length(metadata->'approvers') FROM audit_log
			WHERE action = 'withdrawal.hold_resolution_executed' AND target_id = $1`, r.ID.String()).Scan(&md, &approvers); err != nil {
			return err
		}
		return nil
	})
	for _, must := range []string{`"after_state": "executed"`, `"reason_code": "` + hsrReason + `"`, wr.ID.String(), h.apprC.ID.String(), `"tenant_status_at_execution": "suspended"`} {
		if !strings.Contains(md, must) {
			t.Errorf("executed audit row lacks %q: %s", must, md)
		}
	}
	if approvers != 2 || !strings.Contains(md, h.apprB.ID.String()) {
		t.Errorf("executed audit approvers = %d (%s)", approvers, md)
	}

	// Exactly once: a second approval, a second request and a second posting are refused.
	if _, err := h.decide(h.apprD, r, ResolutionApprove); !errors.Is(err, ErrHoldResolutionNotPending) {
		t.Errorf("decide after execution: %v", err)
	}
	if _, err := h.request(h.reqA, wr.ID); !hsrIs(err, "HR010") {
		t.Errorf("a second request for a released withdrawal: want HR010, got %v", err)
	}
	if n := h.governedTxCount(wr.ID); n != 1 {
		t.Errorf("governed transactions after replay = %d", n)
	}
	h.assertInvariants()
}

// A baseline of 1 is still four-eyes: the requester plus one DISTINCT approver. The
// requester alone can never execute.
func TestHSEC_HoldRelease_BaselineOne_IsStillTwoPersons(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(700)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	if r.RequiredAtSubmission != 1 {
		t.Fatalf("required_at_submission = %d", r.RequiredAtSubmission)
	}
	// The requester cannot approve their own request - the only way to "execute" alone.
	if _, err := h.decide(h.reqA, r, ResolutionApprove); !hsrIs(err, "HR031") {
		t.Fatalf("requester self-approval: want HR031, got %v", err)
	}
	if h.wr(wr.ID).State != withdrawal.StateApproved || h.governedTxCount(wr.ID) != 0 {
		t.Fatal("a single staff member moved money")
	}
	out := h.mustDecide(h.apprB, r, ResolutionApprove)
	if !out.Executed {
		t.Fatalf("requester + one distinct approver must execute: %+v", out)
	}
	h.assertInvariants()
}

// The four-eyes refusal matrix.
func TestHSEC_HoldRelease_FourEyesMatrix(t *testing.T) {
	h := newHSR(t, 2)

	t.Run("single actor cannot request and approve", func(t *testing.T) {
		wr := h.hold(100)
		h.suspendTenant()
		defer h.activateTenant()
		r := h.mustRequest(h.reqA, wr.ID)
		if _, err := h.decide(h.reqA, r, ResolutionApprove); !hsrIs(err, "HR031") {
			t.Fatalf("self-approval: want HR031, got %v", err)
		}
		if _, err := h.decide(h.reqA, r, ResolutionReject); !hsrIs(err, "HR031") {
			t.Fatalf("self-reject must also be refused (a distinct Person decides): got %v", err)
		}
		if h.res(r.ID).State != ResolutionPending {
			t.Fatal("the resolution moved")
		}
	})
}

func TestHSEC_HoldRelease_FourEyesMatrix_Rest(t *testing.T) {
	h := newHSR(t, 2)
	wr := h.hold(100)
	other := h.hold(50)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)

	// Same approver twice: the same Person already decided.
	h.mustDecide(h.apprB, r, ResolutionApprove)
	if _, err := h.decide(h.apprB, r, ResolutionApprove); !hsrIs(err, "HR031") {
		t.Fatalf("the same approver twice: want HR031, got %v", err)
	}
	if got := h.count(`SELECT count(*) FROM audit_log WHERE action = 'withdrawal.hold_resolution_approved' AND target_id = $1`, r.ID.String()); got != 1 {
		t.Fatalf("approved audit rows = %d", got)
	}
	// One distinct approver of two required is not enough; nothing moved.
	if h.res(r.ID).State != ResolutionPending || h.wr(wr.ID).State != withdrawal.StateApproved || h.governedTxCount(wr.ID) != 0 {
		t.Fatal("one of two approvals executed")
	}
	// An approver with an acting session but WITHOUT the capability is refused (HR003);
	// so is one holding only the K3 payment_force_resolve capability.
	if _, err := h.decide(h.noHR, r, ResolutionApprove); !hsrIs(err, "HR003") {
		t.Fatalf("approver without the capability: want HR003, got %v", err)
	}
	if _, err := h.request(h.noHR, other.ID); !hsrIs(err, "HR003") {
		t.Fatalf("requester without the capability: want HR003, got %v", err)
	}
	// An approver whose hash differs from the pinned payload.
	bad := r
	bad.PayloadHash = k3EvidenceHash()
	if _, err := h.decide(h.apprC, bad, ResolutionApprove); !hsrIs(err, "HR031") {
		t.Fatalf("approval of a different payload hash: want HR031, got %v", err)
	}
	// A reject by a distinct Person ends the resolution without moving money.
	rej := h.mustDecide(h.apprC, r, ResolutionReject)
	if rej.Resolution.State != ResolutionRejected || rej.Executed {
		t.Fatalf("reject: %+v", rej)
	}
	if h.wr(wr.ID).State != withdrawal.StateApproved || h.governedTxCount(wr.ID) != 0 {
		t.Fatal("a rejected resolution moved money")
	}
	if n := h.auditCount("withdrawal.hold_resolution_rejected", r.ID); n != 1 {
		t.Fatalf("rejected audit rows = %d", n)
	}
	h.assertInvariants()
}

// S-12: a staff principal whose Person is the withdrawing player's Person is the
// beneficiary and may neither request nor approve.
func TestHSEC_HoldRelease_BeneficiaryExcluded(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	h.suspendTenant()
	self := k3MkStaff(t, h.pool, uuid.Nil, "platform_admin", h.f.personID)
	h.grantActing(self, capability.CapabilityWithdrawalHoldResolutionRequest)
	h.grantActing(self, capability.CapabilityWithdrawalHoldResolutionApprove)
	if _, err := h.request(self, wr.ID); !hsrIs(err, "HR032") {
		t.Fatalf("beneficiary request: want HR032, got %v", err)
	}
	r := h.mustRequest(h.reqA, wr.ID)
	if _, err := h.decide(self, r, ResolutionApprove); !hsrIs(err, "HR032") {
		t.Fatalf("beneficiary approval: want HR032, got %v", err)
	}
	if h.governedTxCount(wr.ID) != 0 {
		t.Fatal("the beneficiary moved money")
	}
}

// Inert until a platform policy row is approved through the governed flow (HD-PRH2-3):
// with no baseline, a request is refused (HR014) and nothing is written.
func TestHSEC_HoldRelease_InertWithoutPlatformPolicy(t *testing.T) {
	h := newHSR(t, 0)
	wr := h.hold(100)
	h.suspendTenant()
	_, err := h.request(h.reqA, wr.ID)
	if !hsrIs(err, "HR014") {
		t.Fatalf("no policy row: want HR014, got %v", err)
	}
	if ClassifyHoldResolutionError(err) != ResolutionErrDisabled {
		t.Fatalf("class = %s", ClassifyHoldResolutionError(err))
	}
	if n := h.count(`SELECT count(*) FROM audit_log WHERE action = 'withdrawal.hold_resolution_requested'`); n != 0 {
		t.Fatalf("a refused request wrote a success audit row: %d", n)
	}
}

// Tenant staff of every tenant role are denied: at the service (before any DB work),
// at the database (no tenant policy; the guard refuses a non-acting scope), and the
// grants cannot even be issued to a tenant role (eligible_tenant_roles is empty).
func TestHSEC_HoldRelease_TenantStaffDeniedAtServiceAndDatabase(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)

	for _, actor := range []k3Staff{h.f1, h.tenantAdmin, h.financeNoGrant} {
		if _, err := h.svc.Request(k3Ctx(actor), h.target(actor), h.reqIn(wr.ID), h.meta()); !errors.Is(err, ErrHoldResolutionNotPermitted) {
			t.Errorf("%s request via the service: %v", actor.Role, err)
		}
		if _, err := h.svc.Decide(k3Ctx(actor), h.target(actor), r.ID, HoldResolutionDecisionInput{Decision: ResolutionApprove, PayloadHash: r.PayloadHash, ReasonCode: "x_y"}, h.meta()); !errors.Is(err, ErrHoldResolutionNotPermitted) {
			t.Errorf("%s approve via the service: %v", actor.Role, err)
		}
		if _, err := h.svc.Get(k3Ctx(actor), h.target(actor), r.ID, h.meta()); !errors.Is(err, ErrHoldResolutionNotPermitted) {
			t.Errorf("%s read via the service: %v", actor.Role, err)
		}
		if ClassifyHoldResolutionError(ErrHoldResolutionNotPermitted) != ResolutionErrForbidden {
			t.Fatal("not-permitted must classify as forbidden")
		}
	}
	// The database, bypassing the service: raw INSERT as a finance staff member with
	// every grant a tenant could hold. The refusal is HR001 (the guard) or 42501 (RLS).
	rawInsert := func() error {
		return h.rt.WithPrincipalScope(context.Background(), h.f.tenantID, h.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO withdrawal_hold_resolutions
					(tenant_id, withdrawal_request_id, kind, brand_id, player_account_id, wallet_id, amount, asset_code,
					 withdrawal_state_at_submission, tenant_status_at_submission, brand_status_at_submission, reason_code, evidence_ref_hash,
					 payload_hash, requested_by, requested_by_scope, requested_by_person_id, required_at_submission, contributing_policy_ids, expires_at)
				VALUES ($1, $2, 'release_hold_to_player', $3, $3, $3, 1, '-', '-', '-', '-', 'x_y', $4, '-', $3, 'platform_acting', $3, 1, '{}', now())`,
				h.f.tenantID, wr.ID, uuid.Nil, k3EvidenceHash())
			return err
		})
	}
	if err := rawInsert(); !hsrIs(err, "HR001", "42501") {
		t.Fatalf("tenant-scope raw INSERT: want HR001 or 42501, got %v", err)
	}
	// Each layer ALONE: the beneficiary guard fires first (and also refuses a non-acting
	// scope), so lift it for ONE statement - the resolution guard itself must still say HR001.
	ddl := func(sql string) {
		if err := h.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
	ddl(`ALTER TABLE withdrawal_hold_resolutions DISABLE TRIGGER withdrawal_hold_resolutions_beneficiary_guard`)
	err := rawInsert()
	ddl(`ALTER TABLE withdrawal_hold_resolutions ENABLE TRIGGER withdrawal_hold_resolutions_beneficiary_guard`)
	if !hsrIs(err, "HR001") {
		t.Fatalf("tenant-scope raw INSERT with the beneficiary guard lifted: want HR001 from the resolution guard, got %v", err)
	}
	// A tenant session sees no resolution rows at all.
	var n int
	if err := h.rt.WithPrincipalScope(context.Background(), h.f.tenantID, h.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_hold_resolutions`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("tenant session sees %d resolution rows (%v)", n, err)
	}
	// A tenant-scope approval insert and a tenant-scope state UPDATE are refused too.
	err = h.rt.WithPrincipalScope(context.Background(), h.f.tenantID, h.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'executing' WHERE id = $1`, r.ID)
		return err
	})
	if err != nil {
		t.Fatalf("a tenant UPDATE matching zero RLS-visible rows must not error, got %v", err)
	}
	if h.res(r.ID).State != ResolutionPending {
		t.Fatal("a tenant session changed the resolution")
	}
	// The capability cannot be granted to a tenant role.
	var reqID uuid.UUID
	err = h.pool.WithPrincipalScope(context.Background(), h.f.tenantID, h.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		rq, e := capability.CreateRequest(ctx, tx, h.f.tenantID, capability.NewRequestInput{GranteeStaffID: h.f1.ID,
			Capability: capability.CapabilityWithdrawalHoldResolutionApprove, ReasonCode: "hsr-test"})
		reqID = rq.ID
		return e
	})
	if err == nil {
		t.Fatalf("a tenant-role grant of withdrawal_hold_resolution:approve was accepted (request %s)", reqID)
	}
}

// A-19: refused at REQUEST when tenant and brand are both active; the variants pin
// the tenant status and the brand status separately.
func TestHSEC_HoldRelease_ActiveAtRequest_Refused(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	if _, err := h.request(h.reqA, wr.ID); !hsrIs(err, "HR010") {
		t.Fatalf("both active: want HR010, got %v", err)
	}
	if ClassifyHoldResolutionError(errors.New("x")) != ResolutionErrOther {
		t.Fatal("classifier sanity")
	}
	if n := h.count(`SELECT count(*) FROM audit_log WHERE action = 'withdrawal.hold_resolution_requested'`); n != 0 {
		t.Fatalf("refused request wrote an audit row: %d", n)
	}
	for _, c := range []struct {
		name      string
		tenantSet string
		brandSet  string
	}{
		{"tenant_suspended_only", "suspended", ""},
		{"tenant_closed_only", "closed", ""},
		{"brand_suspended_only", "", "suspended"},
		{"brand_closed_only", "", "closed"},
		{"both", "suspended", "suspended"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hh := newHSR(t, 1)
			w2 := hh.hold(100)
			if c.tenantSet != "" {
				hh.setTenantStatus(c.tenantSet)
			}
			if c.brandSet != "" {
				setBrandStatus(t, hh.pool, hh.f.tenantID, hh.f.brandID, c.brandSet)
			}
			r, err := hh.request(hh.reqA, w2.ID)
			if err != nil {
				t.Fatalf("a non-active %s must admit a request: %v", c.name, err)
			}
			wantT, wantB := "active", "active"
			if c.tenantSet != "" {
				wantT = c.tenantSet
			}
			if c.brandSet != "" {
				wantB = c.brandSet
			}
			if r.TenantStatusAtSubmission != wantT || r.BrandStatusAtSubmission != wantB {
				t.Fatalf("pinned statuses %s/%s, want %s/%s", r.TenantStatusAtSubmission, r.BrandStatusAtSubmission, wantT, wantB)
			}
			out := hh.mustDecide(hh.apprB, r, ResolutionApprove)
			if !out.Executed {
				t.Fatalf("not executed: %+v", out)
			}
			hh.assertInvariants()
		})
	}
}

// A-19 at EXECUTION: reactivation between request and the final approval ends the
// resolution refused_at_execution (committed, audited); the hold stays. Separate pins
// for the tenant and the brand: only when BOTH are active again is it refused.
func TestHSEC_HoldRelease_ActiveAgainAtExecution_Refused(t *testing.T) {
	for _, c := range []struct {
		name string
		// suspend before the request / reactivate before the final approval
		suspendTenant, suspendBrand, reactTenant, reactBrand bool
		wantExecuted                                         bool
	}{
		{"tenant_only_then_tenant_back", true, false, true, false, false},
		{"brand_only_then_brand_back", false, true, false, true, false},
		{"both_then_both_back", true, true, true, true, false},
		{"both_then_only_tenant_back_brand_still_suspended", true, true, true, false, true},
		{"both_then_only_brand_back_tenant_still_suspended", true, true, false, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHSR(t, 2)
			wr := h.hold(900)
			if c.suspendTenant {
				h.suspendTenant()
			}
			if c.suspendBrand {
				h.suspendBrand()
			}
			r := h.mustRequest(h.reqA, wr.ID)
			h.mustDecide(h.apprB, r, ResolutionApprove)
			if c.reactTenant {
				h.activateTenant()
			}
			if c.reactBrand {
				h.activateBrand()
			}
			out, err := h.decide(h.apprC, r, ResolutionApprove)
			if err != nil {
				t.Fatalf("final approval: %v", err)
			}
			if c.wantExecuted {
				if !out.Executed || out.Refused || h.wr(wr.ID).State != withdrawal.StateRejected || h.governedTxCount(wr.ID) != 1 {
					t.Fatalf("one of tenant/brand still non-active must execute: %+v", out)
				}
				h.assertInvariants()
				return
			}
			if out.Executed || !out.Refused || out.Resolution.State != ResolutionRefusedAtExecute ||
				out.Resolution.RefusalCode == nil || *out.Resolution.RefusalCode != holdRefusedActiveAgain {
				t.Fatalf("active again must be refused_at_execution/%s: %+v", holdRefusedActiveAgain, out)
			}
			if got := h.wr(wr.ID); got.State != withdrawal.StateApproved || got.ReleaseLedgerTransactionID != nil || h.governedTxCount(wr.ID) != 0 {
				t.Fatalf("a refused release moved the withdrawal: %+v", got)
			}
			if hold := h.walletBalance("player_withdrawal_hold"); hold != 900 {
				t.Fatalf("hold = %d, want 900 (the hold REMAINS)", hold)
			}
			if n := h.auditCount("withdrawal.hold_resolution_refused", r.ID); n != 1 {
				t.Fatalf("refused audit rows = %d", n)
			}
			h.assertInvariants()
		})
	}
}

// No-attempt precondition, at request and at execution.
func TestHSEC_HoldRelease_AttemptExists_Refused(t *testing.T) {
	h := newHSR(t, 1)

	// Execution: request while suspended, reactivate, dispatch (attempt created,
	// withdrawal submitted), re-suspend, force the row back to approved (an
	// inconsistent state a bug could produce): the executor refuses on the attempt.
	wr := h.hold(300)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	h.activateTenant()
	claim, err := h.orch.ClaimForDispatch(context.Background(), h.pool, KYCEnforcementPayoutGate{}, h.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil || claim.Attempt.ID == uuid.Nil {
		t.Fatalf("setup dispatch: %v", err)
	}
	h.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, wr.ID)
		return err
	})
	h.suspendTenant()
	out := h.mustDecide(h.apprB, r, ResolutionApprove)
	if out.Executed || !out.Refused || out.Resolution.RefusalCode == nil || *out.Resolution.RefusalCode != holdRefusedAttemptExists {
		t.Fatalf("attempt exists at execution: %+v", out)
	}

	// Request: a withdrawal that already has an attempt is refused at INSERT.
	if _, err := h.request(h.reqA, wr.ID); !hsrIs(err, "HR010") {
		t.Fatalf("attempt exists at request: want HR010, got %v", err)
	}
	if h.governedTxCount(wr.ID) != 0 {
		t.Fatal("a release posted for a withdrawal with an attempt")
	}
	h.assertInvariants()
}

// Expiry: a stale resolution takes no approval and does not execute; expiry is a
// committed, audited transition; pending -> expired is proof-less ONLY when due.
func TestHSEC_HoldRelease_Expired(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)

	// Early expiry is refused (the guard, HR030) ...
	err := h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'expired' WHERE id = $1`, r.ID)
		return err
	})
	if !hsrIs(err, "HR030") {
		t.Fatalf("early expiry: want HR030, got %v", err)
	}
	// ... and by the proof trigger on its own, with the base guard lifted for ONE statement.
	ctx := context.Background()
	must := func(sql string) {
		if err := h.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
	must(`ALTER TABLE withdrawal_hold_resolutions DISABLE TRIGGER withdrawal_hold_resolutions_guard`)
	err = h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'expired' WHERE id = $1`, r.ID)
		return err
	})
	must(`ALTER TABLE withdrawal_hold_resolutions ENABLE TRIGGER withdrawal_hold_resolutions_guard`)
	if !hsrIs(err, "HR030") {
		t.Fatalf("zz_actor_proof_guard must itself refuse an early proof-less expiry: got %v", err)
	}

	h.backdate(r.ID)
	// An approval of an expired resolution is refused by the approvals guard.
	err = h.acting(h.apprB, func(ctx context.Context, tx pgx.Tx) error {
		if err := h.proof(ctx, tx, "withdrawal_hold_resolution:approve", r.ID.String(), r.PayloadHash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_hold_resolution_approvals
			(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'platform_acting', $4, 0, 'x_y')`, h.f.tenantID, r.ID, r.PayloadHash, uuid.Nil)
		return err
	})
	if !hsrIs(err, "HR031") {
		t.Fatalf("approval of an expired resolution: want HR031, got %v", err)
	}
	out, err := h.decide(h.apprB, r, ResolutionApprove)
	if !errors.Is(err, ErrHoldResolutionExpired) || !out.Expired || out.Executed {
		t.Fatalf("decide on an expired resolution: %+v %v", out, err)
	}
	if got := h.res(r.ID).State; got != ResolutionExpired {
		t.Fatalf("state = %s", got)
	}
	if n := h.auditCount("withdrawal.hold_resolution_expired", r.ID); n != 1 {
		t.Fatalf("expired audit rows = %d", n)
	}
	if h.wr(wr.ID).State != withdrawal.StateApproved || h.governedTxCount(wr.ID) != 0 {
		t.Fatal("an expired resolution moved money")
	}
	// A new request for the same withdrawal is possible after expiry (the pending
	// unique index no longer holds the slot).
	h.mustRequest(h.reqA, wr.ID)
}

// Cancel: requester only, with a proof; another actor cannot.
func TestHSEC_HoldRelease_Cancel_RequesterOnly(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	if _, err := h.svc.Cancel(k3Ctx(h.apprB), h.target(h.apprB), r.ID, h.meta()); !hsrIs(err, "HR030", "AP004") {
		t.Fatalf("cancel by a non-requester: got %v", err)
	}
	got, err := h.svc.Cancel(k3Ctx(h.reqA), h.target(h.reqA), r.ID, h.meta())
	if err != nil || got.State != ResolutionCancelled {
		t.Fatalf("cancel by the requester: %+v %v", got, err)
	}
	if n := h.auditCount("withdrawal.hold_resolution_cancelled", r.ID); n != 1 {
		t.Fatalf("cancelled audit rows = %d", n)
	}
	if _, err := h.decide(h.apprB, r, ResolutionApprove); !errors.Is(err, ErrHoldResolutionNotPending) {
		t.Fatalf("approve after cancel: %v", err)
	}
}

// RLS cross-tenant: an acting session for ANOTHER tenant sees nothing of this
// tenant's resolutions and cannot create one for it; a platform principal with no
// grant for that tenant cannot open the session at all.
func TestHSEC_HoldRelease_CrossTenantIsolation(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)

	other := seedPayoutFixture(t, h.pool, 1000, true)
	// reqA has no grant in the other tenant: the acting session cannot open (CG020).
	err := h.rt.WithPlatformActingInTenant(context.Background(), h.reqA.ID, other.tenantID, uuid.New(), OperationKindHoldResolution,
		func(ctx context.Context, tx pgx.Tx) error { return nil })
	if err == nil {
		t.Fatal("an acting session opened in a tenant without a grant")
	}
	// A tenant session of the OTHER tenant sees no row of this tenant (no tenant policy at all).
	var n int
	if err := h.rt.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_hold_resolutions`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("other tenant sees %d rows (%v)", n, err)
	}
	// An acting session of THIS tenant cannot insert a row naming the other tenant.
	err = h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO withdrawal_hold_resolutions
				(tenant_id, withdrawal_request_id, kind, brand_id, player_account_id, wallet_id, amount, asset_code,
				 withdrawal_state_at_submission, tenant_status_at_submission, brand_status_at_submission, reason_code, evidence_ref_hash,
				 payload_hash, requested_by, requested_by_scope, requested_by_person_id, required_at_submission, contributing_policy_ids, expires_at)
			VALUES ($1, $2, 'release_hold_to_player', $3, $3, $3, 1, '-', '-', '-', '-', 'x_y', $4, '-', $3, 'platform_acting', $3, 1, '{}', now())`,
			other.tenantID, wr.ID, uuid.Nil, k3EvidenceHash())
		return err
	})
	if err == nil {
		t.Fatal("an acting session created a resolution for another tenant")
	}
	// The other tenant's withdrawal id with this tenant's id: not found in the session tenant.
	if _, err := h.request(h.reqA, uuid.New()); !errors.Is(err, ErrHoldResolutionNotFound) {
		t.Fatalf("unknown withdrawal: %v", err)
	}
	if h.res(r.ID).TenantID != h.f.tenantID {
		t.Fatal("tenant mismatch")
	}
}

// Replay: executing twice is impossible; a stale retry with a fresh proof posts
// nothing; one pending resolution per withdrawal.
func TestHSEC_HoldRelease_ReplayAndDuplicateRequest(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	// A second request while one is pending: the partial UNIQUE (pending) refuses.
	if _, err := h.request(h.reqA, wr.ID); !hsrIs(err, "23505") {
		t.Fatalf("second pending request: want 23505, got %v", err)
	}
	h.mustDecide(h.apprB, r, ResolutionApprove)
	if h.governedTxCount(wr.ID) != 1 {
		t.Fatal("not executed")
	}
	// A retry of the very same decision (a fresh proof is issued per attempt) posts nothing.
	for i := 0; i < 3; i++ {
		if _, err := h.decide(h.apprC, r, ResolutionApprove); !errors.Is(err, ErrHoldResolutionNotPending) {
			t.Fatalf("replayed decide %d: %v", i, err)
		}
	}
	var approvals int
	if err := h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_hold_resolution_approvals WHERE resolution_id = $1`, r.ID).Scan(&approvals)
	}); err != nil {
		t.Fatal(err)
	}
	if h.governedTxCount(wr.ID) != 1 || approvals != 1 {
		t.Fatalf("replay changed state: governed=%d approvals=%d", h.governedTxCount(wr.ID), approvals)
	}
	h.assertInvariants()
}

// Concurrency: two final approvals race; exactly one executes, the other sees a
// non-pending resolution. One governed posting, balanced ledger.
func TestHSEC_HoldRelease_Concurrency_TwoFinalApprovals(t *testing.T) {
	h := newHSR(t, 2)
	wr := h.hold(400)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	h.mustDecide(h.apprB, r, ResolutionApprove)

	var wg sync.WaitGroup
	type res struct {
		out HoldResolutionOutcome
		err error
	}
	results := make([]res, 2)
	start := make(chan struct{})
	for i, a := range []k3Staff{h.apprC, h.apprD} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := h.decide(a, r, ResolutionApprove)
			results[i] = res{out, err}
		}()
	}
	close(start)
	wg.Wait()
	executed := 0
	for _, x := range results {
		if x.err == nil && x.out.Executed {
			executed++
		} else if x.err != nil && !errors.Is(x.err, ErrHoldResolutionNotPending) {
			t.Errorf("loser error: %v", x.err)
		}
	}
	if executed != 1 || h.governedTxCount(wr.ID) != 1 || h.wr(wr.ID).State != withdrawal.StateRejected {
		t.Fatalf("executed=%d governed=%d state=%s", executed, h.governedTxCount(wr.ID), h.wr(wr.ID).State)
	}
	h.assertInvariants()
}

// Concurrency: execute vs reactivation. The executor holds the tenant status lock
// SHARED from step 2, so a concurrent reactivation WAITS and the release commits
// first; had the reactivation committed first, the release is refused (covered by
// ActiveAgainAtExecution).
func TestHSEC_HoldRelease_Concurrency_ExecuteVsReactivation(t *testing.T) {
	for _, scope := range []string{"tenant", "brand"} {
		t.Run(scope, func(t *testing.T) {
			h := newHSR(t, 1)
			wr := h.hold(400)
			if scope == "tenant" {
				h.suspendTenant()
			} else {
				h.suspendBrand()
			}
			r := h.mustRequest(h.reqA, wr.ID)

			reached := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			releaseOnce := func() { once.Do(func() { close(release) }) }
			testHookHoldResolutionAfterLocks = func(ctx context.Context, id uuid.UUID) {
				close(reached)
				<-release
			}
			// A failing assertion must never leave the executor parked on the hook (it would hold
			// its connection and hang the pool's Close): release it on every exit.
			defer func() { testHookHoldResolutionAfterLocks = nil }()
			defer releaseOnce()

			execDone := make(chan error, 1)
			var out HoldResolutionOutcome
			go func() {
				var err error
				out, err = h.decide(h.apprB, r, ResolutionApprove)
				execDone <- err
			}()
			<-reached
			reactDone := make(chan struct{})
			go func() {
				if scope == "tenant" {
					h.activateTenant()
				} else {
					h.activateBrand()
				}
				close(reactDone)
			}()
			select {
			case <-reactDone:
				t.Fatal("the reactivation committed while the release held the status lock")
			case <-time.After(700 * time.Millisecond):
			}
			releaseOnce()
			if err := <-execDone; err != nil {
				t.Fatalf("execute: %v", err)
			}
			<-reactDone
			if !out.Executed || h.wr(wr.ID).State != withdrawal.StateRejected || h.governedTxCount(wr.ID) != 1 {
				t.Fatalf("release did not win: %+v", out)
			}
			h.assertInvariants()
		})
	}
}

// Concurrency: execute vs submit. The submit path takes the same L1 withdrawal lock
// first; it waits for the release and then finds the withdrawal rejected. The release
// never races a dispatch: no attempt, no provider call.
func TestHSEC_HoldRelease_Concurrency_ExecuteVsSubmit(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(400)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)

	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseOnce := func() { once.Do(func() { close(release) }) }
	testHookHoldResolutionAfterLocks = func(ctx context.Context, id uuid.UUID) {
		close(reached)
		<-release
	}
	defer func() { testHookHoldResolutionAfterLocks = nil }()
	defer releaseOnce()
	execDone := make(chan error, 1)
	go func() {
		_, err := h.decide(h.apprB, r, ResolutionApprove)
		execDone <- err
	}()
	<-reached
	claimDone := make(chan error, 1)
	go func() {
		_, err := h.orch.ClaimForDispatch(context.Background(), h.pool, KYCEnforcementPayoutGate{}, h.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
		claimDone <- err
	}()
	select {
	case err := <-claimDone:
		t.Fatalf("the submit did not wait for the L1 lock held by the release: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	releaseOnce()
	if err := <-execDone; err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := <-claimDone; err == nil {
		t.Fatal("the submit succeeded after the hold was released")
	}
	if n := h.count(`SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, wr.ID); n != 0 {
		t.Fatalf("attempts = %d", n)
	}
	if h.mock.AttemptCount() != 0 {
		t.Fatalf("provider calls = %d", h.mock.AttemptCount())
	}
	h.assertInvariants()
}

// A claim that commits first (tenant active at that moment) leaves a submitted
// withdrawal with an attempt: the later release is refused and the dispatch intact.
func TestHSEC_HoldRelease_SubmitFirst_ThenReleaseRefused(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(400)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)
	h.activateTenant()
	if _, err := h.orch.ClaimForDispatch(context.Background(), h.pool, KYCEnforcementPayoutGate{}, h.f.tenantID, wr.ID, "bank_transfer", testSubmitActor()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	h.suspendTenant()
	out := h.mustDecide(h.apprB, r, ResolutionApprove)
	if out.Executed || !out.Refused || out.Resolution.RefusalCode == nil || *out.Resolution.RefusalCode != holdRefusedNotApproved {
		t.Fatalf("release after submit: %+v", out)
	}
	if got := h.wr(wr.ID); got.State != withdrawal.StateSubmitted || h.governedTxCount(wr.ID) != 0 {
		t.Fatalf("the dispatch was disturbed: %s", got.State)
	}
	h.assertInvariants()
}

// The request path takes the L1 withdrawal lock first (ADR 0111 6.5): a request for a
// withdrawal whose row is locked by another transaction WAITS for it.
func TestHSEC_HoldRelease_Request_TakesTheWithdrawalL1Lock(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(100)
	h.suspendTenant()
	locked := make(chan struct{})
	unlock := make(chan struct{})
	holderDone := make(chan error, 1)
	var once sync.Once
	unlockOnce := func() { once.Do(func() { close(unlock) }) }
	defer unlockOnce()
	go func() {
		holderDone <- h.pool.WithTenant(context.Background(), h.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, wr.ID).Scan(&id); err != nil {
				return err
			}
			close(locked)
			<-unlock
			return nil
		})
	}()
	<-locked
	reqDone := make(chan error, 1)
	go func() {
		_, err := h.request(h.reqA, wr.ID)
		reqDone <- err
	}()
	select {
	case err := <-reqDone:
		unlockOnce()
		t.Fatalf("the request did not wait for the L1 lock held by another transaction: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	unlockOnce()
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-reqDone; err != nil {
		t.Fatalf("the request after the lock was released: %v", err)
	}
}

// Execute vs grant revocation: the executor holds the approvers' grants FOR SHARE from
// step 5, so a concurrent revocation WAITS for the release to commit (it cannot slip in
// between the recount and the posting).
func TestHSEC_HoldRelease_Concurrency_ExecuteVsGrantRevocation(t *testing.T) {
	h := newHSR(t, 1)
	wr := h.hold(400)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)

	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseOnce := func() { once.Do(func() { close(release) }) }
	testHookHoldResolutionAfterShareLocks = func(ctx context.Context, id uuid.UUID) {
		close(reached)
		<-release
	}
	defer func() { testHookHoldResolutionAfterShareLocks = nil }()
	defer releaseOnce()

	execDone := make(chan error, 1)
	var out HoldResolutionOutcome
	go func() {
		var err error
		out, err = h.decide(h.apprB, r, ResolutionApprove)
		execDone <- err
	}()
	<-reached
	revDone := make(chan struct{})
	go func() {
		h.revokePlatformGrant(h.apprB, capability.CapabilityWithdrawalHoldResolutionApprove)
		close(revDone)
	}()
	select {
	case <-revDone:
		releaseOnce()
		t.Fatal("the grant revocation committed while the executor held the grants FOR SHARE")
	case <-time.After(700 * time.Millisecond):
	}
	releaseOnce()
	if err := <-execDone; err != nil {
		t.Fatalf("execute: %v", err)
	}
	<-revDone
	if !out.Executed || h.wr(wr.ID).State != withdrawal.StateRejected || h.governedTxCount(wr.ID) != 1 {
		t.Fatalf("the release did not win: %+v", out)
	}
	h.assertInvariants()
}

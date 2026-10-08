//go:build integration

// HSEC-APPROVED-HOLD-RELEASE-1: the resolution/approval GUARDS attacked with raw SQL as
// the runtime role in an acting session (what a stolen runtime credential plus a valid
// acting grant could try): S-2(iii), payload immutability, the state machine, the
// database-side re-check at `executing`, and the CHECK constraints.
package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// S-2(iii): a Person who authored or approved a contributing policy row may neither
// request nor approve a resolution under it.
func TestHSEC_HoldRelease_PolicyAuthorsCannotRequestOrApprove(t *testing.T) {
	h := newHSR(t, 2)
	wr := h.hold(100)
	h.suspendTenant()
	// A fresh pair authors and approves the in-force (superseding, tighter) platform row.
	ax, ay := h.staffMember(uuid.Nil, "platform_admin"), h.staffMember(uuid.Nil, "platform_admin")
	for _, s := range []k3Staff{ax, ay} {
		h.grantActing(s, capability.CapabilityWithdrawalHoldResolutionRequest)
		h.grantActing(s, capability.CapabilityWithdrawalHoldResolutionApprove)
	}
	h.approvePolicy(adjustment.PolicyChangeInput{
		ChangeKind: adjustment.ChangeKindPolicy, OperationKind: OperationKindHoldResolution, Level: adjustment.LevelPlatform,
		AssetCode: k3StrPtr("EUR"), BaseRequiredApprovals: k3IntPtr(3),
	}, ax, ay)
	if _, err := h.request(ax, wr.ID); !hsrIs(err, "HR011") {
		t.Fatalf("policy author as requester: want HR011, got %v", err)
	}
	if _, err := h.request(ay, wr.ID); !hsrIs(err, "HR011") {
		t.Fatalf("policy approver as requester: want HR011, got %v", err)
	}
	r := h.mustRequest(h.reqA, wr.ID)
	if r.RequiredAtSubmission != 3 {
		t.Fatalf("the superseding row must be in force: required %d", r.RequiredAtSubmission)
	}
	if _, err := h.decide(ax, r, ResolutionApprove); !hsrIs(err, "HR011") {
		t.Fatalf("policy author as approver: want HR011, got %v", err)
	}
	if _, err := h.decide(ay, r, ResolutionApprove); !hsrIs(err, "HR011") {
		t.Fatalf("policy approver as approver: want HR011, got %v", err)
	}
	if h.governedTxCount(wr.ID) != 0 {
		t.Fatal("a policy author moved money")
	}
}

// The resolution guard against raw UPDATEs: the payload and actor are immutable, the
// state machine is closed, and `executing` re-runs the recount and every precondition in
// the database itself (not only in the Go executor).
func TestHSEC_HoldRelease_ResolutionGuard_RawStateMachineAndImmutability(t *testing.T) {
	h := newHSR(t, 2)
	wr := h.hold(100)
	h.suspendTenant()
	r := h.mustRequest(h.reqA, wr.ID)

	upd := func(sql string, args ...any) error {
		return h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	for name, sql := range map[string]string{
		"amount":            `UPDATE withdrawal_hold_resolutions SET amount = amount + 1 WHERE id = $1`,
		"evidence":          `UPDATE withdrawal_hold_resolutions SET evidence_ref_hash = repeat('c', 64) WHERE id = $1`,
		"reason":            `UPDATE withdrawal_hold_resolutions SET reason_code = 'other_reason' WHERE id = $1`,
		"requested_by":      `UPDATE withdrawal_hold_resolutions SET requested_by = gen_random_uuid() WHERE id = $1`,
		"payload_hash":      `UPDATE withdrawal_hold_resolutions SET payload_hash = repeat('0', 64) WHERE id = $1`,
		"expires_at":        `UPDATE withdrawal_hold_resolutions SET expires_at = now() + interval '30 days' WHERE id = $1`,
		"withdrawal":        `UPDATE withdrawal_hold_resolutions SET withdrawal_request_id = gen_random_uuid() WHERE id = $1`,
		"pending->executed": `UPDATE withdrawal_hold_resolutions SET state = 'executed' WHERE id = $1`,
		"pending->executing, no approval in the txn": `UPDATE withdrawal_hold_resolutions SET state = 'executing' WHERE id = $1`,
		"pending->refused, no approval in the txn":   `UPDATE withdrawal_hold_resolutions SET state = 'refused_at_execution', refusal_code = 'x' WHERE id = $1`,
		"pending->rejected, no reject decision":      `UPDATE withdrawal_hold_resolutions SET state = 'rejected' WHERE id = $1`,
	} {
		if err := upd(sql, r.ID); !hsrIs(err, "HR030", "23514") {
			t.Errorf("%s: want HR030, got %v", name, err)
		}
	}
	// A cancelled resolution is terminal.
	if _, err := h.svc.Cancel(k3Ctx(h.reqA), h.target(h.reqA), r.ID, h.meta()); err != nil {
		t.Fatal(err)
	}
	if err := upd(`UPDATE withdrawal_hold_resolutions SET state = 'pending' WHERE id = $1`, r.ID); !hsrIs(err, "HR030") {
		t.Errorf("a terminal resolution must stay terminal: got %v", err)
	}

	// The CHECKs: a reason code outside the pattern and a non-hex evidence hash never insert.
	bad := func(reason, evidence string) error {
		return h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
			if err := h.proof(ctx, tx, "withdrawal_hold_resolution:request", "new", actorproofDigest(h, wr.ID, evidence, reason)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO withdrawal_hold_resolutions
					(tenant_id, withdrawal_request_id, kind, brand_id, player_account_id, wallet_id, amount, asset_code,
					 withdrawal_state_at_submission, tenant_status_at_submission, brand_status_at_submission, reason_code, evidence_ref_hash,
					 payload_hash, requested_by, requested_by_scope, requested_by_person_id, required_at_submission, contributing_policy_ids, expires_at)
				VALUES ($1, $2, 'release_hold_to_player', $3, $3, $3, 1, '-', '-', '-', '-', $4, $5, '-', $3, 'platform_acting', $3, 1, '{}', now())`,
				h.f.tenantID, wr.ID, uuid.Nil, reason, evidence)
			return err
		})
	}
	for name, c := range map[string][2]string{
		"upper-case reason": {"Bad", k3EvidenceHash()}, "dash reason": {"bad-code", k3EvidenceHash()}, "leading digit": {"1bad", k3EvidenceHash()},
		"65-char reason": {"a" + repeatStr("b", 64), k3EvidenceHash()}, "short evidence": {hsrReason, "abc"},
		"upper-case evidence": {hsrReason, repeatStr("A", 64)},
	} {
		if err := bad(c[0], c[1]); !hsrIs(err, "23514") {
			t.Errorf("%s: want the CHECK violation 23514, got %v", name, err)
		}
	}
	if err := bad("a"+repeatStr("b", 63), k3EvidenceHash()); err != nil && !hsrIs(err, "23505") {
		t.Errorf("a 64-char reason is allowed by the pattern: %v", err)
	}
}

func repeatStr(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func actorproofDigest(h *hsr, wrID uuid.UUID, evidence, reason string) string {
	return actorproofDigestOf(h.f.tenantID.String(), wrID.String(), string(HoldReleaseToPlayer), evidence, reason)
}

// `-> executing` in the DATABASE re-checks the recount and every precondition, whatever the
// Go executor did (here the Go executor is bypassed with raw SQL in one acting session).
func TestHSEC_HoldRelease_ExecutingTransition_DatabaseRechecks(t *testing.T) {
	rawExecute := func(h *hsr, r HoldResolution, approvers ...k3Staff) error {
		// one session per approver (each carries its own proof); the LAST also moves to executing.
		for i, a := range approvers {
			last := i == len(approvers)-1
			err := h.acting(a, func(ctx context.Context, tx pgx.Tx) error {
				if err := h.proof(ctx, tx, "withdrawal_hold_resolution:approve", r.ID.String(), r.PayloadHash); err != nil {
					return err
				}
				if err := hsrRawApprove(ctx, tx, h, r, "approve"); err != nil {
					return err
				}
				if !last {
					return nil
				}
				_, err := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'executing' WHERE id = $1`, r.ID)
				return err
			})
			if err != nil {
				return err
			}
		}
		return nil
	}

	t.Run("fewer counted approvals than required", func(t *testing.T) {
		h := newHSR(t, 2)
		wr := h.hold(100)
		h.suspendTenant()
		r := h.mustRequest(h.reqA, wr.ID)
		if err := rawExecute(h, r, h.apprB); !hsrIs(err, "HR030") {
			t.Fatalf("want HR030, got %v", err)
		}
	})
	t.Run("both active again", func(t *testing.T) {
		h := newHSR(t, 1)
		wr := h.hold(100)
		h.suspendTenant()
		r := h.mustRequest(h.reqA, wr.ID)
		h.activateTenant()
		if err := rawExecute(h, r, h.apprB); !hsrIs(err, "HR010") {
			t.Fatalf("want HR010, got %v", err)
		}
	})
	t.Run("withdrawal no longer approved", func(t *testing.T) {
		h := newHSR(t, 1)
		wr := h.hold(100)
		h.suspendTenant()
		r := h.mustRequest(h.reqA, wr.ID)
		h.tx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'submitted' WHERE id = $1`, wr.ID)
			return err
		})
		if err := rawExecute(h, r, h.apprB); !hsrIs(err, "HR010") {
			t.Fatalf("want HR010, got %v", err)
		}
	})
	t.Run("an attempt exists", func(t *testing.T) {
		h := newHSR(t, 1)
		wr := h.hold(100)
		h.suspendTenant()
		r := h.mustRequest(h.reqA, wr.ID)
		h.activateTenant()
		if _, err := h.orch.ClaimForDispatch(context.Background(), h.pool, KYCEnforcementPayoutGate{}, h.f.tenantID, wr.ID, "bank_transfer", testSubmitActor()); err != nil {
			t.Fatal(err)
		}
		h.tx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, wr.ID)
			return err
		})
		h.suspendTenant()
		if err := rawExecute(h, r, h.apprB); !hsrIs(err, "HR010") {
			t.Fatalf("want HR010, got %v", err)
		}
		if h.wr(wr.ID).State != withdrawal.StateApproved {
			t.Fatal("state moved")
		}
	})
	t.Run("the policy baseline vanished is not testable; requester no longer valid", func(t *testing.T) {
		h := newHSR(t, 1)
		wr := h.hold(100)
		h.suspendTenant()
		r := h.mustRequest(h.reqA, wr.ID)
		// the requester's grant is revoked between request and execution: the recount refuses.
		h.revokePlatformGrant(h.reqA, capability.CapabilityWithdrawalHoldResolutionRequest)
		if err := rawExecute(h, r, h.apprB); !hsrIs(err, "HR030") {
			t.Fatalf("want HR030 (requester no longer qualifies), got %v", err)
		}
	})
}

func actorproofDigestOf(tenantID, wrID, kind, evidence, reason string) string {
	return actorproof.Digest(actorproof.S(tenantID), actorproof.S(wrID), actorproof.S(kind), actorproof.S(evidence), actorproof.S(reason))
}

// revokePlatformGrant revokes a platform principal's in-force grant (a platform admin acts).
func (h *hsr) revokePlatformGrant(staff k3Staff, c capability.Capability) {
	h.t.Helper()
	gid, ok := h.grantIDs[k3GrantKey(staff.ID, c)]
	if !ok {
		h.t.Fatalf("no grant recorded for %s %s", staff.ID, c)
	}
	if err := h.pool.WithPlatformAdmin(context.Background(), h.adminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return capability.RevokeGrant(ctx, tx, h.f.tenantID, gid, "hsr_test_revoke")
	}); err != nil {
		h.t.Fatalf("revoke: %v", err)
	}
}

// The policy lookup ignores tenant- and brand-level rows while the tenant OR the brand is
// not active (K2-1 extended): a tenant admin's tighter row governs an ACTIVE tenant but can
// never steer the baseline of a platform operation on a non-active one.
func TestHSEC_HoldRelease_PolicyLookup_IgnoresTenantRowsWhenNonActive(t *testing.T) {
	h := newHSR(t, 2)
	ctx := context.Background()
	ta2 := h.staffMember(h.f.tenantID, "tenant_admin")
	in := adjustment.PolicyChangeInput{ChangeKind: adjustment.ChangeKindPolicy, OperationKind: OperationKindHoldResolution,
		Level: adjustment.LevelTenant, BaseRequiredApprovals: k3IntPtr(4)}
	var c adjustment.PolicyChange
	if err := h.pool.WithPrincipalScope(ctx, h.f.tenantID, h.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = adjustment.ProposePolicyChangeInTx(ctx, tx, adjustment.PolicyCall{ActorID: h.tenantAdmin.ID, TenantID: h.f.tenantID}, in)
		return err
	}); err != nil {
		t.Fatalf("propose tenant row: %v", err)
	}
	if err := h.pool.WithPrincipalScope(ctx, h.f.tenantID, ta2.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := adjustment.DecidePolicyChangeInTx(ctx, tx, adjustment.PolicyCall{ActorID: ta2.ID, TenantID: h.f.tenantID}, c.ID, adjustment.DecisionApprove, c.ContentHash, "hsr_test")
		return err
	}); err != nil {
		t.Fatalf("approve tenant row: %v", err)
	}
	required := func() int {
		var n int
		if err := h.pool.WithPrincipalScope(ctx, h.f.tenantID, h.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT required FROM financial_policy_required_approvals('withdrawal_hold_resolution', $1, $2, 'EUR', 100, now())`,
				h.f.tenantID, h.f.brandID).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := required(); got != 4 {
		t.Fatalf("an active tenant and brand: the tenant row (4) must govern, got %d", got)
	}
	h.suspendTenant()
	if got := required(); got != 2 {
		t.Fatalf("a suspended tenant: only the platform baseline (2) applies, got %d", got)
	}
	h.activateTenant()
	h.suspendBrand()
	if got := required(); got != 2 {
		t.Fatalf("a suspended brand: only the platform baseline (2) applies, got %d", got)
	}
	// And the K3 operation keeps its own rule (a tenant row still applies for an active tenant).
	var k3 int
	if err := h.pool.WithPrincipalScope(ctx, h.f.tenantID, h.tenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT required FROM financial_policy_required_approvals('payment_force_resolve', $1, $2, 'EUR', 100, now())`,
			h.f.tenantID, h.f.brandID).Scan(&k3)
	}); err != nil || k3 != 1 {
		t.Fatalf("payment_force_resolve lookup changed: %d %v", k3, err)
	}
}

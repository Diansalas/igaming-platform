//go:build integration

package adjustment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// evaluate calls the ONE evaluator from a tenant session of w.
func (w *world) evaluate(op string, amount *int64) Evaluation {
	w.t.Helper()
	var ev Evaluation
	brand := w.Brand
	if err := w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ev, err = EvaluateInTx(ctx, tx, op, w.Tenant, &brand, w.Asset, amount)
		return err
	}); err != nil {
		w.t.Fatalf("evaluate: %v", err)
	}
	return ev
}

func i64(v int64) *int64 { return &v }

// B-8 (R): tightened between submission and execution -> an extra approval
// is needed; loosened -> the pinned (submission-time) value holds (§3.6).
func TestB8_TightenAfterSubmissionNeedsMoreLoosenKeepsPin(t *testing.T) {
	t.Run("tightened by the tenant after submission", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1})
		r, err := w.submit(w.F1, w.credit(100, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		ta2 := w.staff(w.Tenant, "tenant_admin")
		w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelTenant,
			BaseRequiredApprovals: intPtr(2)}, w.TenantAdmin, ta2)
		out, err := w.decide(w.F2, r, DecisionApprove)
		if err != nil {
			t.Fatal(err)
		}
		if out.Executed || out.Required != 2 || out.Counted != 1 {
			t.Fatalf("tightening not applied at execution: %+v", out)
		}
		out, err = w.decide(w.F3, r, DecisionApprove)
		if err != nil || !out.Executed {
			t.Fatalf("second approval should execute: %v %+v", err, out)
		}
		w.assertInvariants()
	})
	t.Run("loosened by the platform after submission: the pin holds", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 2})
		r, err := w.submit(w.F1, w.credit(100, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
			AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(1)}, w.AdminA, w.AdminB)
		if ev := w.evaluate(OperationKind, i64(100)); ev.Required != 1 {
			t.Fatalf("loosening did not take effect for new evaluations: %+v", ev)
		}
		out, err := w.decide(w.F2, r, DecisionApprove)
		if err != nil {
			t.Fatal(err)
		}
		if out.Executed || out.Required != 2 {
			t.Fatalf("pinned requirement not honoured: %+v", out)
		}
		w.assertInvariants()
	})
}

// B-9 (R): no platform baseline -> disabled; tenant row only -> disabled;
// an unresolvable tenant jurisdiction with a jurisdiction row -> disabled
// (fail closed; HD-PRH2-3: nothing is seeded).
func TestB9_DisabledWithoutPlatformBaseline(t *testing.T) {
	w := newWorld(t, worldOpts{})
	if ev := w.evaluate(OperationKind, i64(1)); ev.Enabled {
		t.Fatalf("no platform baseline must be disabled: %+v", ev)
	}
	if _, err := w.submit(w.F1, w.credit(1, ReasonOperationalErrorCorrection)); pgCode(err) != "MA014" {
		t.Fatalf("submission without a baseline: expected MA014, got %v", err)
	}
	ta2 := w.staff(w.Tenant, "tenant_admin")
	w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelTenant,
		BaseRequiredApprovals: intPtr(3)}, w.TenantAdmin, ta2)
	if ev := w.evaluate(OperationKind, i64(1)); ev.Enabled {
		t.Fatalf("a tenant row alone must not enable: %+v", ev)
	}

	// Jurisdiction row + a tenant with no resolvable jurisdiction.
	w2 := newWorld(t, worldOpts{base: 1})
	if ev := w2.evaluate(OperationKind, i64(1)); !ev.Enabled {
		t.Fatalf("baseline must enable: %+v", ev)
	}
	jur := w2.newJurisdiction()
	w2.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelJurisdiction,
		JurisdictionID: &jur, AssetCode: strPtr(w2.Asset), BaseRequiredApprovals: intPtr(2)}, w2.AdminA, w2.AdminB)
	if ev := w2.evaluate(OperationKind, i64(1)); ev.Enabled {
		t.Fatalf("a jurisdiction row with an unresolvable tenant jurisdiction must disable: %+v", ev)
	}
	// Resolving the tenant's jurisdiction (licence in jur) enables it, and
	// the jurisdiction row contributes (MAX).
	w2.assignLicence(jur)
	ev := w2.evaluate(OperationKind, i64(1))
	if !ev.Enabled || ev.Required != 2 || len(ev.ContributingPolicyIDs) != 2 {
		t.Fatalf("resolved jurisdiction: %+v", ev)
	}
	// ... also from an acting session (the 0113 own-licence acting read).
	var acting Evaluation
	brand := w2.Brand
	if err := w2.pool.WithPlatformActingInTenant(context.Background(), w2.Acting.ID, w2.Tenant, uuid.Nil, OperationKind, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acting, err = EvaluateInTx(ctx, tx, OperationKind, w2.Tenant, &brand, w2.Asset, i64(1))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !acting.Enabled || acting.Required != 2 {
		t.Fatalf("acting evaluation differs from tenant evaluation: %+v vs %+v", acting, ev)
	}
}

func (w *world) newJurisdiction() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.WithPlatformAdmin(context.Background(), w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'K2 synthetic jurisdiction') RETURNING id`,
			"K2J-"+uuid.NewString()[:8]).Scan(&id)
	}); err != nil {
		w.t.Fatalf("jurisdiction: %v", err)
	}
	return id
}

func (w *world) assignLicence(jurisdiction uuid.UUID) {
	w.t.Helper()
	if err := w.pool.WithPlatformAdmin(context.Background(), w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		var lic uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', $2) RETURNING id`,
			jurisdiction, "K2-LIC-"+uuid.NewString()[:8]).Scan(&lic); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, w.Tenant, lic)
		return err
	}); err != nil {
		w.t.Fatalf("assign licence: %v", err)
	}
}

// B-10 (ADV): tenant loosening refused (MA010); a single-Person platform
// change refused; the policy author (or approver) as initiator or as
// approver refused (C-100-1, S-2(iii), MA011).
func TestB10_PolicyGovernanceRefusals(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	ta2 := w.staff(w.Tenant, "tenant_admin")
	w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelTenant,
		BaseRequiredApprovals: intPtr(2)}, w.TenantAdmin, ta2)

	// Tenant loosening (2 -> 1): refused at proposal.
	if _, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelTenant,
		BaseRequiredApprovals: intPtr(1)}, w.TenantAdmin); pgCode(err) != "MA010" {
		t.Fatalf("tenant loosening: expected MA010, got %v", err)
	}
	// A platform-requested loosening approved by a TENANT principal: refused.
	c, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelTenant,
		TenantID: &w.Tenant, BaseRequiredApprovals: intPtr(1)}, w.AdminA)
	if err != nil {
		t.Fatalf("platform proposes loosening: %v", err)
	}
	if err := w.decidePolicy(c, ta2, DecisionApprove); pgCode(err) != "MA010" {
		t.Fatalf("non-tightening approved by a tenant principal: expected MA010, got %v", err)
	}
	// ... and approved by a second platform principal: allowed (§6.7 remedy).
	if err := w.decidePolicy(c, w.AdminB, DecisionApprove); err != nil {
		t.Fatalf("platform/platform non-tightening: %v", err)
	}
	// MAX, never most-specific-wins (S-2 (ii); ADR 0100 §3.2): with the
	// tenant row now at 1 and the platform floor raised to 2, the platform
	// floor still governs.
	w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
		AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(2)}, w.AdminA, w.AdminB)
	if ev := w.evaluate(OperationKind, i64(1)); ev.Required != 2 || len(ev.ContributingPolicyIDs) != 2 {
		t.Fatalf("a less-specific floor must still bind (MAX, not most-specific): %+v", ev)
	}

	// Single-Person platform change: the approver shares the requester's Person.
	twin := mkStaff(t, w.pool, uuid.Nil, "platform_admin", w.AdminA.PersonID)
	c2, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
		AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(3)}, w.AdminA)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.decidePolicy(c2, twin, DecisionApprove); pgCode(err) != "MA012" {
		t.Fatalf("same-Person platform approval: expected MA012, got %v", err)
	}
	if err := w.decidePolicy(c2, w.AdminA, DecisionApprove); pgCode(err) != "MA012" {
		t.Fatalf("self-approval of a policy change: expected MA012, got %v", err)
	}
	// A tenant principal may never author a platform row.
	if _, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
		AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(5)}, w.TenantAdmin); pgCode(err) != "MA012" {
		t.Fatalf("tenant authoring a platform row: expected MA012, got %v", err)
	}

	// S-2(iii): authors of a contributing policy (TenantAdmin/ta2 authored
	// the tenant row; AdminA/AdminB the platform rows) may not initiate or
	// approve under it. A finance principal sharing AdminA's Person (AdminA
	// authored the in-force baseline AND the platform/platform tenant row):
	authorTwin := mkStaff(t, w.pool, w.Tenant, "finance", w.AdminA.PersonID)
	w.grantTenant(authorTwin, "ledger_adjustment:initiate")
	w.grantTenant(authorTwin, "ledger_adjustment:approve")
	if _, err := w.submit(authorTwin, w.credit(10, ReasonOperationalErrorCorrection)); pgCode(err) != "MA011" {
		t.Fatalf("policy approver as initiator: expected MA011, got %v", err)
	}
	r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.decide(authorTwin, r, DecisionApprove); pgCode(err) != "MA011" {
		t.Fatalf("policy approver as approver: expected MA011, got %v", err)
	}
	// And a platform policy author acting in the tenant (AdminB's Person).
	authorActing := mkStaff(t, w.pool, uuid.Nil, "platform_admin", w.AdminB.PersonID)
	w.grantActing(authorActing, "ledger_adjustment:approve")
	if _, err := w.decide(authorActing, r, DecisionApprove); pgCode(err) != "MA011" {
		t.Fatalf("platform policy author approving while acting: expected MA011, got %v", err)
	}
	w.assertInvariants()
}

// B-20 (R): the classification is pinned; a base = 0 row for the mandatory
// class is refused by trigger (MA013); and - on a scratch DB where that
// trigger is disabled to plant a base = 0 fixture row (LF test 8) - the
// evaluator still returns GREATEST(1, ...) = 1.
func TestB20_ClassificationAndGreatestOne(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	err := w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
		var class string
		if err := tx.QueryRow(ctx, `SELECT class FROM financial_control_classifications WHERE operation_kind = 'ledger_adjustment'`).Scan(&class); err != nil {
			return err
		}
		if class != "mandatory_four_eyes" {
			t.Fatalf("classification: %s", class)
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT c`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE financial_control_classifications SET class = 'outside_mandatory_class' WHERE operation_kind = 'ledger_adjustment'`)
		if err == nil && tag.RowsAffected() != 0 {
			t.Fatal("the classification must not be writable by the app session")
		}
		_, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT c`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
		AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(0)}, w.AdminA)
	if err != nil {
		t.Fatalf("propose base=0: %v", err)
	}
	if err := w.decidePolicy(c, w.AdminB, DecisionApprove); pgCode(err) != "MA013" {
		t.Fatalf("base = 0 for the mandatory class: expected MA013, got %v", err)
	}

	pool := scratchPoolThrough(t, "k2b20_", 113)
	s := newWorldOn(t, pool, worldOpts{})
	ctx := context.Background()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE financial_approval_policy_changes DISABLE TRIGGER financial_approval_policy_changes_guard`,
			`ALTER TABLE financial_approval_policies DISABLE TRIGGER financial_approval_policies_guard`,
			`ALTER TABLE financial_approval_policy_changes NO FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE financial_approval_policies NO FORCE ROW LEVEL SECURITY`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		chg := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO financial_approval_policy_changes (id, change_kind, operation_kind, level, asset_code, base_required_approvals,
			effective_from, content_hash, requested_by, requested_by_scope, requested_by_person_id, status, expires_at)
			VALUES ($1, 'policy', 'ledger_adjustment', 'platform', $2, 0, now() - interval '1 minute', 'x', $3, 'platform', $4, 'approved', now())`,
			chg, s.Asset, s.AdminA.ID, s.AdminA.PersonID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO financial_approval_policies (operation_kind, level, asset_code, base_required_approvals, effective_from,
			change_id, author_person_id, approver_person_id) VALUES ('ledger_adjustment', 'platform', $1, 0, now() - interval '1 minute', $2, $3, $4)`,
			s.Asset, chg, s.AdminA.PersonID, s.AdminB.PersonID); err != nil {
			return err
		}
		for _, q := range []string{
			`ALTER TABLE financial_approval_policies FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE financial_approval_policy_changes FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE financial_approval_policies ENABLE TRIGGER financial_approval_policies_guard`,
			`ALTER TABLE financial_approval_policy_changes ENABLE TRIGGER financial_approval_policy_changes_guard`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("plant base=0 fixture (scratch): %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	ev := s.evaluate(OperationKind, i64(1))
	if !ev.Enabled || ev.Required != 1 {
		t.Fatalf("GREATEST(1, ...) must hold for a base = 0 row: %+v", ev)
	}
}

// B-26 (R): C-100-3 - a non-active tenant refuses goodwill_credit and allows
// the other codes; K2-1 (§6.7) - a non-active tenant's tenant-level
// tightening is IGNORED for payment_force_resolve and APPLIED for
// ledger_adjustment.
func TestB26_NonActiveTenant(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: "payment_force_resolve", Level: LevelPlatform,
		AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(1)}, w.AdminA, w.AdminB)
	ta2 := w.staff(w.Tenant, "tenant_admin")
	for _, op := range []string{OperationKind, "payment_force_resolve"} {
		w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: op, Level: LevelTenant,
			BaseRequiredApprovals: intPtr(3)}, w.TenantAdmin, ta2)
	}
	if ev := w.evaluate("payment_force_resolve", i64(1)); ev.Required != 3 {
		t.Fatalf("active tenant: tenant tightening must apply to force-resolve: %+v", ev)
	}
	w.setTenantStatus("suspended")
	if ev := w.evaluate("payment_force_resolve", i64(1)); ev.Required != 1 || *ev.TenantStatus != "suspended" {
		t.Fatalf("K2-1: a non-active tenant's tenant rows must be ignored for payment_force_resolve: %+v", ev)
	}
	if ev := w.evaluate(OperationKind, i64(1)); ev.Required != 3 {
		t.Fatalf("K2-1: tenant rows still apply to ledger_adjustment: %+v", ev)
	}
	if _, err := w.submit(w.F1, w.credit(10, ReasonGoodwillCredit)); pgCode(err) != "MA023" {
		t.Fatalf("goodwill on a non-active tenant: expected MA023, got %v", err)
	}
	r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("operational correction on a non-active tenant must be allowed: %v", err)
	}
	if r.TenantStatusAtSubmit != "suspended" {
		t.Fatalf("tenant status not recorded: %s", r.TenantStatusAtSubmit)
	}
	w.assertInvariants()
}

func (w *world) setTenantStatus(status string) {
	w.t.Helper()
	if err := launchfix.TrySetTenantStatusOn(context.Background(), w.t, w.pool, w.Tenant, status); err != nil {
		w.t.Fatalf("tenant status: %v", err)
	}
}

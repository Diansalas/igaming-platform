//go:build integration

package kyc

// lic10 (migration 0181; audit G8, owner question Q21): real-PostgreSQL tests of the residence determination maker-checker.
// NOT RUN by their author (the database harness was refused in the authoring session): the owner runs them. They drive the real
// residenceapproval.Service (signed actor proofs, principal-bound sessions) and the real evidence read.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/residenceapproval"
	"github.com/Diansalas/igaming-platform/internal/testsupport/staffsv"
)

func rmService(t *testing.T, pool *db.Pool) *residenceapproval.Service {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	iss := prooftest.Install(t, u)
	svc, err := residenceapproval.New(residenceapproval.Config{Pool: pool, Proofs: iss})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func rmActor(f fixture, staff uuid.UUID) residenceapproval.Actor {
	return residenceapproval.Actor{StaffID: staff, Scope: residenceapproval.ScopeTenant, TenantID: f.tenantID, RequestID: "rm-test"}
}

// rmDetermine runs one reviewer determination. served: nil leaves app.residence_into_open_market unset.
func rmDetermine(t *testing.T, pool *db.Pool, f fixture, verificationID, staff uuid.UUID, country string, served *string) error {
	t.Helper()
	return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if served != nil {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.residence_into_open_market', $1, true)`, *served); err != nil {
				return err
			}
		}
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{VerificationID: verificationID, StaffID: staff, NewStatus: StatusApproved,
			Reason: "docs_verified", VerifiedResidenceCountry: &country})
		return err
	})
}

func rmEvidence(t *testing.T, pool *db.Pool, f fixture) (string, bool) {
	t.Helper()
	var c string
	var ok bool
	if err := pool.WithTenantReadOnly(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, ok, err = ReadMarketResidenceEvidence(ctx, tx, f.tenantID, f.playerID)
		return err
	}); err != nil {
		t.Fatalf("evidence read: %v", err)
	}
	return c, ok
}

func rmPending(t *testing.T, pool *db.Pool, f fixture) []PendingResidenceApproval {
	t.Helper()
	var out []PendingResidenceApproval
	if err := pool.WithTenantReadOnly(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = ListPendingResidenceApprovals(ctx, tx, f.tenantID, 0)
		return err
	}); err != nil {
		t.Fatalf("pending list: %v", err)
	}
	return out
}

func rmSetAll(t *testing.T, svc *residenceapproval.Service, f fixture, staff uuid.UUID, mode residenceapproval.Mode) {
	t.Helper()
	res, err := svc.SetPolicy(context.Background(), rmActor(f, staff), mode, "test_policy")
	if err != nil || !res.ExecutedImmediately {
		t.Fatalf("a stricter mode must execute at once: %+v %v", res, err)
	}
}

// DEFAULT: no policy means mode none and today's behaviour: a single reviewer's determination is evidence at once, and a check
// row exists that records "no approval required".
func TestResidenceMakerChecker_DefaultIsToday_SingleReviewerDeterminationIsEvidence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	v := seedVerification(t, pool, f)
	r1 := seedComplianceStaff(t, pool, f)
	if err := rmDetermine(t, pool, f, v, r1, "MT", nil); err != nil {
		t.Fatalf("determination under the default: %v", err)
	}
	if c, ok := rmEvidence(t, pool, f); !ok || c != "MT" {
		t.Fatalf("evidence = %q %v, want MT true (behaviour unchanged by default)", c, ok)
	}
	if n := len(rmPending(t, pool, f)); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
	svc := rmService(t, pool)
	p, err := svc.ReadPolicy(context.Background(), rmActor(f, r1))
	if err != nil || p.EffectiveMode != residenceapproval.ModeNone || len(p.Versions) != 0 {
		t.Fatalf("default policy = %+v %v, want mode none and no versions", p, err)
	}
}

// Mode all: a determination is PENDING (not evidence); the determiner cannot approve (staff and Person); a second compliance
// officer's approval makes it evidence; replay is refused.
func TestResidenceMakerChecker_ModeAll_PendingIsNotEvidence_SecondPersonApproves(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	svc := rmService(t, pool)
	r1, r2 := seedComplianceStaff(t, pool, f), seedComplianceStaff(t, pool, f)
	rmSetAll(t, svc, f, r1, residenceapproval.ModeAll)

	v := seedVerification(t, pool, f)
	if err := rmDetermine(t, pool, f, v, r1, "MT", nil); err != nil {
		t.Fatalf("determination: %v", err)
	}
	if _, ok := rmEvidence(t, pool, f); ok {
		t.Fatal("a PENDING determination must not be market evidence (fail closed)")
	}
	pend := rmPending(t, pool, f)
	if len(pend) != 1 || pend[0].VerificationID != v || pend[0].DeterminedBy != r1 || pend[0].PolicyMode != "all" {
		t.Fatalf("pending = %+v", pend)
	}
	ctx := context.Background()
	if err := svc.ApproveDetermination(ctx, rmActor(f, r1), v, pend[0].DeterminedAt, "second_check"); !errors.Is(err, residenceapproval.ErrFourEyes) {
		t.Fatalf("the determiner approving their own determination: %v, want ErrFourEyes", err)
	}
	if _, ok := rmEvidence(t, pool, f); ok {
		t.Fatal("still pending after a refused self-approval")
	}
	// A stale binding (a different determined_at) is not the determination the approver saw.
	if err := svc.ApproveDetermination(ctx, rmActor(f, r2), v, pend[0].DeterminedAt.Add(time.Second), "second_check"); !errors.Is(err, residenceapproval.ErrNotFound) {
		t.Fatalf("approval bound to another determined_at: %v, want ErrNotFound", err)
	}
	if err := svc.ApproveDetermination(ctx, rmActor(f, r2), v, pend[0].DeterminedAt, "second_check"); err != nil {
		t.Fatalf("the second Person's approval: %v", err)
	}
	if c, ok := rmEvidence(t, pool, f); !ok || c != "MT" {
		t.Fatalf("after approval evidence = %q %v, want MT true", c, ok)
	}
	if n := len(rmPending(t, pool, f)); n != 0 {
		t.Fatalf("pending after approval = %d", n)
	}
	if err := svc.ApproveDetermination(ctx, rmActor(f, r2), v, pend[0].DeterminedAt, "second_check"); !errors.Is(err, residenceapproval.ErrNotPending) {
		t.Fatalf("a second approval of the same determination: %v, want ErrNotPending", err)
	}
}

// into_open_market: with the setting MISSING the determination is REFUSED (fail closed); 'false' (not served) needs no approval;
// 'true' (served) is pending.
func TestResidenceMakerChecker_IntoOpenMarket_NeedsTheMarketDeclaration(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	svc := rmService(t, pool)
	r1 := seedComplianceStaff(t, pool, f)
	rmSetAll(t, svc, f, r1, residenceapproval.ModeIntoOpenMarket)

	v := seedVerification(t, pool, f)
	if err := rmDetermine(t, pool, f, v, r1, "MT", nil); err == nil {
		t.Fatal("into_open_market with no market declaration must refuse the determination")
	}
	no := "false"
	if err := rmDetermine(t, pool, f, v, r1, "MT", &no); err != nil {
		t.Fatalf("not served: %v", err)
	}
	if _, ok := rmEvidence(t, pool, f); !ok {
		t.Fatal("a determination into a market the brand does not serve needs no approval and is evidence")
	}
}

func TestResidenceMakerChecker_IntoOpenMarket_ServedIsPending(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	svc := rmService(t, pool)
	r1 := seedComplianceStaff(t, pool, f)
	rmSetAll(t, svc, f, r1, residenceapproval.ModeIntoOpenMarket)
	v := seedVerification(t, pool, f)
	yes := "true"
	if err := rmDetermine(t, pool, f, v, r1, "MT", &yes); err != nil {
		t.Fatal(err)
	}
	if _, ok := rmEvidence(t, pool, f); ok {
		t.Fatal("a determination into a served market must be pending")
	}
	if len(rmPending(t, pool, f)) != 1 {
		t.Fatal("expected one pending determination")
	}
}

// The mode is SNAPSHOTTED: tightening applies to future determinations; an existing effective determination is unchanged.
func TestResidenceMakerChecker_ModeIsSnapshottedOnTheDetermination(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	svc := rmService(t, pool)
	r1 := seedComplianceStaff(t, pool, f)
	v := seedVerification(t, pool, f)
	if err := rmDetermine(t, pool, f, v, r1, "MT", nil); err != nil {
		t.Fatal(err)
	}
	rmSetAll(t, svc, f, r1, residenceapproval.ModeAll)
	if _, ok := rmEvidence(t, pool, f); !ok {
		t.Fatal("a determination made under mode none stays effective after the policy tightens (applies to future determinations)")
	}
}

// Policy governance: stricter at once; less strict pending; the requester cannot approve; one Person approves once; two
// approvals by distinct Persons activate it; a stale request (mode changed since) is refused.
func TestResidenceMakerChecker_PolicyGovernance_StricterAtOnce_LooserNeedsTwoDistinctPersons(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	svc := rmService(t, pool)
	ctx := context.Background()
	r1, r2, r3, r4 := seedComplianceStaff(t, pool, f), seedComplianceStaff(t, pool, f), seedComplianceStaff(t, pool, f), seedComplianceStaff(t, pool, f)

	rmSetAll(t, svc, f, r1, residenceapproval.ModeAll)
	if _, err := svc.SetPolicy(ctx, rmActor(f, r1), residenceapproval.ModeAll, "again"); !errors.Is(err, residenceapproval.ErrNotPending) {
		t.Fatalf("the mode already in force: %v, want ErrNotPending (no change)", err)
	}
	loose, err := svc.SetPolicy(ctx, rmActor(f, r1), residenceapproval.ModeNone, "relax_review")
	if err != nil || loose.ExecutedImmediately || loose.Status != "pending" || loose.RequiredApprovals != 2 {
		t.Fatalf("a less strict mode must be filed pending with two approvals: %+v %v", loose, err)
	}
	if p, _ := svc.ReadPolicy(ctx, rmActor(f, r1)); p.EffectiveMode != residenceapproval.ModeAll {
		t.Fatalf("effective mode = %q, want all while the loosening is pending", p.EffectiveMode)
	}
	if _, err := svc.ApprovePolicy(ctx, rmActor(f, r1), loose.PolicyID, "self"); !errors.Is(err, residenceapproval.ErrFourEyes) {
		t.Fatalf("the requester approving: %v, want ErrFourEyes", err)
	}
	a1, err := svc.ApprovePolicy(ctx, rmActor(f, r2), loose.PolicyID, "ok_one")
	if err != nil || a1.Status != "pending" || a1.Approvals != 1 {
		t.Fatalf("first approval: %+v %v", a1, err)
	}
	if _, err := svc.ApprovePolicy(ctx, rmActor(f, r2), loose.PolicyID, "ok_again"); err == nil {
		t.Fatal("the same Person approving twice must be refused")
	}
	a2, err := svc.ApprovePolicy(ctx, rmActor(f, r3), loose.PolicyID, "ok_two")
	if err != nil || a2.Status != "active" || a2.Approvals != 2 {
		t.Fatalf("second approval must activate: %+v %v", a2, err)
	}
	if p, _ := svc.ReadPolicy(ctx, rmActor(f, r1)); p.EffectiveMode != residenceapproval.ModeNone {
		t.Fatalf("effective mode = %q, want none after two approvals", p.EffectiveMode)
	}
	if _, err := svc.ApprovePolicy(ctx, rmActor(f, r4), loose.PolicyID, "late"); !errors.Is(err, residenceapproval.ErrNotPending) {
		t.Fatalf("approving an already active request: %v, want ErrNotPending", err)
	}
	// Stale: file a loosening from all, then tighten elsewhere, then try to approve the old request.
	rmSetAll(t, svc, f, r1, residenceapproval.ModeAll)
	stale, err := svc.SetPolicy(ctx, rmActor(f, r1), residenceapproval.ModeIntoOpenMarket, "relax_one_step")
	if err != nil || stale.Status != "pending" {
		t.Fatalf("pending loosening: %+v %v", stale, err)
	}
	// Loosen all -> none by a quorum, then re-tighten to into_open_market so the recorded previous mode (all) is no longer in force.
	l2, _ := svc.SetPolicy(ctx, rmActor(f, r1), residenceapproval.ModeNone, "relax_all_the_way")
	if _, err := svc.ApprovePolicy(ctx, rmActor(f, r2), l2.PolicyID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApprovePolicy(ctx, rmActor(f, r3), l2.PolicyID, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApprovePolicy(ctx, rmActor(f, r2), stale.PolicyID, "stale"); !errors.Is(err, residenceapproval.ErrNotPending) {
		t.Fatalf("approving a request filed against a mode no longer in force: %v, want ErrNotPending", err)
	}
}

// Authorisation: only compliance may file or approve; a player-less tenant admin (a different role) is refused by the DATABASE
// even if a route let it through; a foreign tenant's staff cannot touch another tenant's policy.
func TestResidenceMakerChecker_Authorisation_RoleAndTenant(t *testing.T) {
	pool := testPool(t)
	f, g := seedFixture(t, pool), seedFixture(t, pool)
	svc := rmService(t, pool)
	ctx := context.Background()
	rf := seedComplianceStaff(t, pool, f)
	rg := seedComplianceStaff(t, pool, g)
	// A staff member of tenant g acting on tenant f: the session tenant is g, so the function refuses (tenant mismatch / forbidden).
	foreign := residenceapproval.Actor{StaffID: rg, Scope: residenceapproval.ScopeTenant, TenantID: f.tenantID}
	if _, err := svc.SetPolicy(ctx, foreign, residenceapproval.ModeAll, "x"); err == nil {
		t.Fatal("a foreign tenant's staff must not set another tenant's policy")
	}
	rmSetAll(t, svc, f, rf, residenceapproval.ModeAll)
	if p, _ := svc.ReadPolicy(ctx, rmActor(g, rg)); p.EffectiveMode != residenceapproval.ModeNone || len(p.Versions) != 0 {
		t.Fatalf("tenant g must not see or inherit tenant f's policy: %+v", p)
	}
}

// The runtime role has no write privilege on any maker-checker table, and a direct UPDATE of a check is refused: only the
// definer functions can change state.
func TestResidenceMakerChecker_RuntimeCannotWriteTheTablesDirectly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	svc := rmService(t, pool)
	r1 := seedComplianceStaff(t, pool, f)
	rmSetAll(t, svc, f, r1, residenceapproval.ModeAll)
	v := seedVerification(t, pool, f)
	if err := rmDetermine(t, pool, f, v, r1, "MT", nil); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE residence_determination_checks SET approved_by = $1, approved_by_person_id = $1, approved_at = now(), approval_reason_code = 'forged'`,
		`INSERT INTO residence_approval_policies (tenant_id, mode, previous_mode, status, required_approvals, reason_code, payload_hash, requested_by, requested_by_person_id, requested_by_scope) VALUES ($2, 'none', 'all', 'active', 0, 'x', repeat('a', 64), $1, $1, 'tenant')`,
		`DELETE FROM residence_determination_checks`,
	} {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, stmt, uuid.New(), f.tenantID)
			return e
		})
		if err == nil {
			t.Errorf("the runtime role must not be able to run: %s", stmt)
		}
	}
	if _, ok := rmEvidence(t, pool, f); ok {
		t.Fatal("a forged approval attempt must leave the determination pending")
	}
}

// Tenant isolation: tenant B's pending list never contains tenant A's determination and B's staff cannot approve A's.
func TestResidenceMakerChecker_TenantIsolation(t *testing.T) {
	pool := testPool(t)
	a, b := seedFixture(t, pool), seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, a.tenantID)
	svc := rmService(t, pool)
	ra := seedComplianceStaff(t, pool, a)
	rb := seedComplianceStaff(t, pool, b)
	rmSetAll(t, svc, a, ra, residenceapproval.ModeAll)
	v := seedVerification(t, pool, a)
	if err := rmDetermine(t, pool, a, v, ra, "MT", nil); err != nil {
		t.Fatal(err)
	}
	pend := rmPending(t, pool, a)
	if len(pend) != 1 {
		t.Fatalf("tenant a pending = %d", len(pend))
	}
	if n := len(rmPending(t, pool, b)); n != 0 {
		t.Fatalf("tenant b sees %d of tenant a's pending determinations", n)
	}
	if err := svc.ApproveDetermination(context.Background(), rmActor(b, rb), v, pend[0].DeterminedAt, "cross"); err == nil {
		t.Fatal("tenant b's staff must not approve tenant a's determination")
	}
}

// lic18 L4: only a compliance-role staff member of the tenant can approve a determination (the role is re-checked by the database).
func TestResidenceMakerChecker_L4_ApprovalNeedsTheComplianceRole(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	svc := rmService(t, pool)
	r1 := seedComplianceStaff(t, pool, f)
	rmSetAll(t, svc, f, r1, residenceapproval.ModeAll)
	v := seedVerification(t, pool, f)
	if err := rmDetermine(t, pool, f, v, r1, "MT", nil); err != nil {
		t.Fatal(err)
	}
	pend := rmPending(t, pool, f)
	if len(pend) != 1 {
		t.Fatalf("pending = %+v", pend)
	}
	var admin uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p, err := staffsv.PersonIfRequired(ctx, tx, string(identity.StaffRoleTenantAdmin))
		if err != nil {
			return err
		}
		s, err := identity.CreateStaffUser(ctx, tx, f.tenantID, "admin-"+uuid.NewString()+"@example.com", "hash", identity.StaffRoleTenantAdmin, p)
		admin = s.ID
		return err
	}); err != nil {
		t.Fatalf("seed tenant admin: %v", err)
	}
	if err := svc.ApproveDetermination(context.Background(), rmActor(f, admin), v, pend[0].DeterminedAt, "second_check"); err == nil {
		t.Fatal("a non-compliance staff member must not approve a residence determination (L4)")
	}
	if _, ok := rmEvidence(t, pool, f); ok {
		t.Fatal("still pending after the refused approval")
	}
}

// lic18 S3: once a determination is approved, the residence columns cannot be changed under it. A change under an unchanged
// set_at is refused by the database; a genuine new determination (set_at advances) gets its OWN check row and is pending again.
func TestResidenceMakerChecker_S3_CountryCannotChangeUnderAnApprovedCheck(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	svc := rmService(t, pool)
	r1, r2 := seedComplianceStaff(t, pool, f), seedComplianceStaff(t, pool, f)
	rmSetAll(t, svc, f, r1, residenceapproval.ModeAll)
	v := seedVerification(t, pool, f)
	if err := rmDetermine(t, pool, f, v, r1, "MT", nil); err != nil {
		t.Fatal(err)
	}
	pend := rmPending(t, pool, f)
	if len(pend) != 1 {
		t.Fatalf("pending = %+v", pend)
	}
	if err := svc.ApproveDetermination(context.Background(), rmActor(f, r2), v, pend[0].DeterminedAt, "second_check"); err != nil {
		t.Fatal(err)
	}
	if c, ok := rmEvidence(t, pool, f); !ok || c != "MT" {
		t.Fatalf("evidence = %q %v", c, ok)
	}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_verifications SET verified_residence_country = 'DE' WHERE id = $1`, v)
		return err
	})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "RD005" {
		t.Fatalf("changing the country under an unchanged determination time: %v, want RD005", err)
	}
	if c, ok := rmEvidence(t, pool, f); !ok || c != "MT" {
		t.Fatalf("the refused change must leave the evidence: %q %v", c, ok)
	}
	// A new determination written as a plain UPDATE with an advanced time is a NEW determination: pending again, so no evidence.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_verifications SET verified_residence_country = 'DE', verified_residence_set_at = clock_timestamp() WHERE id = $1`, v)
		return err
	}); err != nil {
		t.Logf("a plain UPDATE that advances the time is refused (%v): acceptable, the new determination must go through ReviewVerification", err)
		return
	}
	if c, ok := rmEvidence(t, pool, f); ok {
		t.Fatalf("a new determination must be pending (no evidence), got %q", c)
	}
}

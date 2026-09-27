//go:build integration

// Real-PostgreSQL tests for ADR 0096 (PRH-I3): EvaluateEnforcement's
// outcome mapping, latest-row read semantics (§2.6), the withdrawal
// structural rule (§3.2 point 1), the deposit/play threshold triggers
// (§3.2 point 2 / §3.5), and migration 0100's RLS/lifecycle/append-only
// guarantees. Shares kyc_integration_test.go's fixture/testPool helpers
// (same package, same //go:build integration tag).
package kyc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

func setVerification(t *testing.T, pool *db.Pool, f fixture, status VerificationStatus, expiresAt *time.Time) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, 'mock', $7)`,
			uuid.New(), f.tenantID, f.brandID, f.playerID, f.personID, string(status), expiresAt)
		return err
	})
	if err != nil {
		t.Fatalf("seed verification status=%s: %v", status, err)
	}
}

func evalWithdrawal(t *testing.T, pool *db.Pool, f fixture) EnforcementDecision {
	t.Helper()
	var decision EnforcementDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = EvaluateEnforcement(ctx, tx, EnforcementParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			Operation: EnforcementWithdrawalHold, AssetCode: "EUR", Amount: 1000, CorrelationID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("EvaluateEnforcement: %v", err)
	}
	return decision
}

// --- §2.3 outcome mapping, exhaustive over verification state ---

func TestEvaluateEnforcement_NoVerification_Failed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomeFailed || d.Allowed {
		t.Fatalf("expected failed/deny with no verification row, got %+v", d)
	}
}

func TestEvaluateEnforcement_Approved_Passed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomePassed || !d.Allowed {
		t.Fatalf("expected passed/allow, got %+v", d)
	}
}

func TestEvaluateEnforcement_Pending_Denied(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusPending, nil)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomePending || d.Allowed {
		t.Fatalf("expected pending/deny, got %+v", d)
	}
}

func TestEvaluateEnforcement_ReviewRequired_MapsToPending(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusReviewRequired, nil)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomePending || d.Allowed {
		t.Fatalf("expected pending/deny for review_required, got %+v", d)
	}
}

func TestEvaluateEnforcement_Rejected_Failed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusRejected, nil)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomeFailed || d.Allowed {
		t.Fatalf("expected failed/deny, got %+v", d)
	}
}

// TestEvaluateEnforcement_ApprovedButExpired_Failed proves §2.6(b): an
// approved row whose expires_at has passed evaluates failed, NOT passed,
// even though the stored status column still literally reads 'approved'.
func TestEvaluateEnforcement_ApprovedButExpired_Failed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	past := time.Now().Add(-time.Hour)
	setVerification(t, pool, f, StatusApproved, &past)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomeFailed || d.Allowed {
		t.Fatalf("expected an expired approval to evaluate failed/deny, got %+v", d)
	}
}

// TestEvaluateEnforcement_ApprovedNotYetExpired_Passed proves the
// boundary case: a future expires_at still allows.
func TestEvaluateEnforcement_ApprovedNotYetExpired_Passed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	future := time.Now().Add(time.Hour)
	setVerification(t, pool, f, StatusApproved, &future)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomePassed || !d.Allowed {
		t.Fatalf("expected a not-yet-expired approval to allow, got %+v", d)
	}
}

// TestEvaluateEnforcement_LatestRowWins proves §2.6(a): an OLDER approved
// row never satisfies the check when a NEWER row for the same player is
// rejected - the EXISTS(status='approved') bug this ADR names explicitly.
func TestEvaluateEnforcement_LatestRowWins(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)
	time.Sleep(10 * time.Millisecond) // created_at ordering must be unambiguous
	setVerification(t, pool, f, StatusRejected, nil)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomeFailed || d.Allowed {
		t.Fatalf("expected the newer rejected row to govern, got %+v", d)
	}
}

// TestEvaluateEnforcement_LatestRowWins_ReverseOrder is the mirror case:
// a newer approval after an older rejection allows - proving this is a
// genuine ordering check, not merely "any rejection anywhere denies".
func TestEvaluateEnforcement_LatestRowWins_ReverseOrder(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusRejected, nil)
	time.Sleep(10 * time.Millisecond)
	setVerification(t, pool, f, StatusApproved, nil)
	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomePassed || !d.Allowed {
		t.Fatalf("expected the newer approved row to govern, got %+v", d)
	}
}

// --- Withdrawal structural rule: no history exemption (security C1 / ledger-finance C3) ---

// TestEvaluateEnforcement_WithdrawalNoFirstWithdrawalExemption proves the
// corrected rule (ADR 0096 §3.2 point 1): a player who has a prior
// APPROVED verification that is now REJECTED is denied - there is no
// "already withdrawn once" exemption. This replaces the original,
// defective test the ADR's own revision record names.
func TestEvaluateEnforcement_WithdrawalNoFirstWithdrawalExemption(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)
	time.Sleep(10 * time.Millisecond)
	setVerification(t, pool, f, StatusRejected, nil)
	d := evalWithdrawal(t, pool, f)
	if d.Allowed {
		t.Fatalf("expected a currently-rejected player to be denied regardless of withdrawal history, got %+v", d)
	}
}

// TestEvaluateEnforcement_WithdrawalCrossAccountRejectedOverlayDenies
// proves the security-N1-prescribed deny-only overlay: a SECOND
// PlayerAccount under the SAME Person, in the SAME tenant, whose own
// latest verification is rejected, denies a withdrawal from the FIRST
// account even though the first account's own latest row is approved.
func TestEvaluateEnforcement_WithdrawalCrossAccountRejectedOverlayDenies(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)

	// A second brand/PlayerAccount under the SAME person, same tenant.
	var secondPlayerID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var secondBrandID uuid.UUID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Second Brand')`,
			secondBrandID, f.tenantID, "b2-"+secondBrandID.String()[:8]); err != nil {
			return err
		}
		secondPlayerID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			secondPlayerID, f.tenantID, secondBrandID, f.personID, secondPlayerID.String()+"@example.com"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, 'rejected', 'mock')`,
			uuid.New(), f.tenantID, secondBrandID, secondPlayerID, f.personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed second player/rejected verification: %v", err)
	}

	d := evalWithdrawal(t, pool, f)
	if d.Allowed {
		t.Fatalf("expected the cross-account rejected overlay to deny, got %+v", d)
	}
	if d.Outcome != OutcomeFailed {
		t.Fatalf("expected outcome failed from the overlay, got %+v", d)
	}
}

// --- ADR 0096 §2.6(g), RV-PRH-I2 KYC review F1: never-submitted orphan
// rows are excluded from "latest" enforcement selection ---

// setOrphanVerification inserts a kyc_verifications row shaped exactly like
// CreateVerification's own phase-A orphan (ADR 0095 §15.2):
// status='unverified', provider_reference NULL - a row that never received
// ANY decision (phase B/C never completed a provider round-trip for it).
func setOrphanVerification(t *testing.T, pool *db.Pool, f fixture) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference)
			 VALUES ($1, $2, $3, $4, $5, 'unverified', 'mock', NULL)`,
			uuid.New(), f.tenantID, f.brandID, f.playerID, f.personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed orphan verification: %v", err)
	}
}

// TestEvaluateEnforcement_OrphanAfterApproval_StillPassed is F1's own
// required test (RV-PRH-I2 KYC code review/security review): an approved
// player who starts a re-verification (a routine renewal, or one triggered
// by a fresh threshold) while the vendor happens to be unreachable commits
// a harmless orphan row (ADR 0095 §15.2's own documented failure mode) -
// this orphan being NEWER than the approval must never, on its own, turn
// withdrawal enforcement from passed to failed. Before the fix, this test
// reproduced code review's own P3 finding exactly.
func TestEvaluateEnforcement_OrphanAfterApproval_StillPassed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)
	time.Sleep(10 * time.Millisecond)
	setOrphanVerification(t, pool, f)

	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomePassed || !d.Allowed {
		t.Fatalf("F1: expected a newer, never-decided orphan to never mask an existing approval, got %+v", d)
	}
}

// TestEvaluateEnforcement_OrphanOnAnotherAccount_DoesNotMaskRejection is
// F1's cross-account shape: account B's latest DECIDED row is rejected;
// account B then accumulates a newer orphan (a later re-verification
// attempt whose vendor call failed). Account A's withdrawal must still be
// DENIED by the cross-account overlay - B's orphan must never mask B's own
// rejection just by being newer.
func TestEvaluateEnforcement_OrphanOnAnotherAccount_DoesNotMaskRejection(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)

	var secondBrandID, secondPlayerID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		secondBrandID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Second Brand')`,
			secondBrandID, f.tenantID, "b2-"+secondBrandID.String()[:8]); err != nil {
			return err
		}
		secondPlayerID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			secondPlayerID, f.tenantID, secondBrandID, f.personID, secondPlayerID.String()+"@example.com"); err != nil {
			return err
		}
		// Account B's DECIDED latest row: rejected.
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, 'rejected', 'mock')`,
			uuid.New(), f.tenantID, secondBrandID, secondPlayerID, f.personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed second player/rejected verification: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	// Account B accumulates a NEWER, never-decided orphan (a later
	// re-verification attempt whose vendor call failed) - this must not
	// mask B's own decided rejection from the cross-account overlay.
	secondFixture := fixture{tenantID: f.tenantID, brandID: secondBrandID, personID: f.personID, playerID: secondPlayerID}
	setOrphanVerification(t, pool, secondFixture)

	d := evalWithdrawal(t, pool, f)
	if d.Allowed {
		t.Fatalf("F1: expected account B's decided rejection to still deny account A's withdrawal despite B's newer orphan, got %+v", d)
	}
	if d.Outcome != OutcomeFailed {
		t.Fatalf("expected outcome failed from the overlay, got %+v", d)
	}
}

// TestEvaluateEnforcement_OrphanOnly_TreatedAsNoVerification proves the
// "changes nothing when every row is an orphan" case: an account with
// ONLY never-decided orphan rows is denied exactly like an account with NO
// verification row at all (found=false), never treated as some other
// state.
func TestEvaluateEnforcement_OrphanOnly_TreatedAsNoVerification(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setOrphanVerification(t, pool, f)
	setOrphanVerification(t, pool, f)

	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomeFailed || d.Allowed {
		t.Fatalf("expected orphan-only rows to evaluate exactly like no verification at all, got %+v", d)
	}
}

// TestEvaluateEnforcement_DecidedOrderingIgnoresInterveningOrphans proves
// ordering by DECISION time survives an orphan landing in between two
// decided rows: approved, then an orphan, then rejected - the latest
// DECIDED row (rejected) must still govern, exactly as if the orphan were
// never there.
func TestEvaluateEnforcement_DecidedOrderingIgnoresInterveningOrphans(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)
	time.Sleep(10 * time.Millisecond)
	setOrphanVerification(t, pool, f)
	time.Sleep(10 * time.Millisecond)
	setVerification(t, pool, f, StatusRejected, nil)

	d := evalWithdrawal(t, pool, f)
	if d.Outcome != OutcomeFailed || d.Allowed {
		t.Fatalf("expected the latest DECIDED row (rejected) to govern despite an intervening orphan, got %+v", d)
	}
}

// --- Cross-tenant isolation (§2.6(f)) ---

// TestEvaluateEnforcement_CrossTenant_NeverEvaluatesAnotherTenantsRows
// proves a tenant-B-scoped transaction evaluating a tenant-A
// player_account_id never succeeds - RLS on kyc_verifications
// (tenant_isolation) makes the row invisible, so the read finds nothing
// and the outcome is `failed` (deny), never `passed`.
func TestEvaluateEnforcement_CrossTenant_NeverEvaluatesAnotherTenantsRows(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	setVerification(t, pool, fA, StatusApproved, nil)
	fB := seedFixture(t, pool)

	var decision EnforcementDecision
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = EvaluateEnforcement(ctx, tx, EnforcementParams{
			TenantID: fB.tenantID, BrandID: fA.brandID, PlayerAccountID: fA.playerID, PersonID: fA.personID,
			Operation: EnforcementWithdrawalHold, AssetCode: "EUR", Amount: 1000, CorrelationID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("EvaluateEnforcement: %v", err)
	}
	if decision.Allowed {
		t.Fatalf("expected tenant B's scope to never see tenant A's approved verification, got %+v", decision)
	}
}

// --- No client-supplied outcome path (§2.5) ---

// TestEvaluateEnforcement_ResultIsAlwaysServerComputed proves
// EvaluateEnforcement accepts no field that could carry a pre-computed
// outcome - EnforcementParams has no such field, so this is a structural/
// compile-time guarantee exercised here by confirming two calls with
// IDENTICAL params but a DIFFERENT database state produce different,
// freshly-computed decisions (i.e. nothing is cached or passed through).
func TestEvaluateEnforcement_ResultIsAlwaysServerComputed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	first := evalWithdrawal(t, pool, f)
	if first.Allowed {
		t.Fatalf("expected deny with no verification, got %+v", first)
	}
	setVerification(t, pool, f, StatusApproved, nil)
	second := evalWithdrawal(t, pool, f)
	if !second.Allowed {
		t.Fatalf("expected allow after seeding an approval, got %+v", second)
	}
}

// --- Deposit threshold trigger (§3.2 point 2) ---

func evalDeposit(t *testing.T, pool *db.Pool, f fixture, amount int64) EnforcementDecision {
	t.Helper()
	var decision EnforcementDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = EvaluateEnforcement(ctx, tx, EnforcementParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			Operation: EnforcementDeposit, AssetCode: "EUR", Amount: amount, CorrelationID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("EvaluateEnforcement: %v", err)
	}
	return decision
}

// TestEvaluateEnforcement_DepositDormantByDefault proves ADR 0096 §3.2
// point 2: with no licence bound (an ordinary, unlicensed test tenant)
// AND with no active cumulative_deposit policy, a deposit of any size
// evaluates not_required/allow - the "mechanism now, values later"
// contract, with zero rows seeded by migration 0100 (§3.7).
func TestEvaluateEnforcement_DepositDormantByDefault(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	d := evalDeposit(t, pool, f, 1_000_000_000)
	if d.Outcome != OutcomeNotRequired || !d.Allowed {
		t.Fatalf("expected not_required/allow with no active policy, got %+v", d)
	}
}

// --- Migration 0100 CHECK constraints: manual branch-coverage checklist ---
// (ADR 0096 §7.1: "no mutation tool applies to a SQL CHECK constraint,
// use a manual true/false checklist" - the same discipline this
// codebase's own precedent already uses for migration 0075.)

func TestMigration0100_CheckConstraints_TriggerTypeShapes(t *testing.T) {
	pool := testPool(t)
	principal := uuid.New()
	jurisdictionID := mustSeedJurisdiction(t, pool)

	cases := []struct {
		name        string
		triggerType string
		threshold   any
		assetCode   any
		tier        any
		playOp      any
		wantErr     bool
	}{
		{"cumulative_deposit valid", "cumulative_deposit", "100", "EUR", nil, nil, false},
		{"cumulative_deposit missing threshold", "cumulative_deposit", nil, "EUR", nil, nil, true},
		{"cumulative_deposit missing asset", "cumulative_deposit", "100", nil, nil, nil, true},
		{"cumulative_deposit carries tier (rejected)", "cumulative_deposit", "100", "EUR", "basic", nil, true},
		{"registration_tier valid", "registration_tier", nil, nil, "basic", nil, false},
		{"registration_tier missing tier", "registration_tier", nil, nil, nil, nil, true},
		{"registration_tier carries threshold (rejected)", "registration_tier", "100", nil, "basic", nil, true},
		{"play valid", "play", nil, nil, nil, "casino_play", false},
		{"play missing play_operation", "play", nil, nil, nil, nil, true},
		{"play carries asset (rejected)", "play", nil, "EUR", nil, "casino_play", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.WithPlatformAdmin(context.Background(), principal, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO kyc_enforcement_policies
						(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code,
						 required_tier, play_operation, reason_code, created_by_actor_type, created_by_actor_id)
					VALUES ($1, $2, 'draft', $3, $4, $5, $6, 'test', 'platform_admin', $7)`,
					jurisdictionID, tc.triggerType, tc.threshold, tc.assetCode, tc.tier, tc.playOp, principal)
				return err
			})
			if tc.wantErr && err == nil {
				t.Fatalf("expected the CHECK constraint to reject this shape, but insert succeeded")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected a valid shape to insert cleanly, got: %v", err)
			}
		})
	}
}

// TestMigration0100_ActiveRequiresLegalReviewReference is the fail-closed
// checklist entry for the `status <> 'active' OR legal_review_reference
// IS NOT NULL` CHECK - an active row with no legal_review_reference must
// never be insertable, full stop, regardless of how it later gets
// activated (draft rows may omit it).
func TestMigration0100_ActiveRequiresLegalReviewReference(t *testing.T) {
	pool := testPool(t)
	principal := uuid.New()
	jurisdictionID := mustSeedJurisdiction(t, pool)

	// A draft with no legal_review_reference inserts cleanly...
	var id uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), principal, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code,
				 reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', 100, 'EUR', 'test', 'platform_admin', $2)
			RETURNING id`, jurisdictionID, principal).Scan(&id)
	})
	if err != nil {
		t.Fatalf("expected a draft with no legal_review_reference to insert, got: %v", err)
	}

	// ...but activating it (draft -> active) with no legal_review_reference
	// must be refused by the CHECK constraint, not merely by convention.
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected activating a row with no legal_review_reference to be rejected")
	}
}

func mustSeedJurisdiction(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'KYC Enforcement Test Jurisdiction')`,
			id, "kyc-test-"+id.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction: %v", err)
	}
	return id
}

// mustLicenseTenant binds f.tenantID to a fresh licence in a fresh
// jurisdiction, returning the jurisdiction id - required for every
// deposit/play test below, since EvaluateEnforcement resolves
// LicensingJurisdictionID from tenants.licence_id -> licences.
// jurisdiction_id and treats "no licence bound" as not_required (§15.2),
// never reaching the policy lookup at all.
func mustLicenseTenant(t *testing.T, pool *db.Pool, f fixture) uuid.UUID {
	t.Helper()
	jurisdictionID := mustSeedJurisdiction(t, pool)
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		lic, err := jurisdiction.CreateLicence(ctx, tx, jurisdiction.CreateLicenceParams{
			JurisdictionID: jurisdictionID, Licensee: "platform", LicenceNumber: "LIC-" + f.tenantID.String()[:8],
			Actor: jurisdiction.ActorContext{ActorID: uuid.New(), ReasonCode: "test"},
		})
		if err != nil {
			return err
		}
		_, err = jurisdiction.AssignTenantLicence(ctx, tx, jurisdiction.AssignTenantLicenceParams{
			TenantID: f.tenantID, LicenceID: &lic.ID, Actor: jurisdiction.ActorContext{ActorID: uuid.New(), ReasonCode: "test"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("license tenant: %v", err)
	}
	return jurisdictionID
}

// mustActivePolicy creates a draft kyc_enforcement_policies row (by one
// principal) and activates it (by a DIFFERENT principal, satisfying the
// four-eyes lifecycle trigger), returning its id.
func mustActivePolicy(t *testing.T, pool *db.Pool, p CreateEnforcementPolicyParams) uuid.UUID {
	t.Helper()
	p.LegalReviewReference = "test-legal-ref"
	if p.ReasonCode == "" {
		p.ReasonCode = "test"
	}
	var id uuid.UUID
	creator := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = CreateEnforcementPolicy(ctx, tx, p)
		return err
	})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return ActivateEnforcementPolicy(ctx, tx, id)
	})
	if err != nil {
		t.Fatalf("activate policy: %v", err)
	}
	return id
}

func strPtr(s string) *string { return &s }

// --- Deposit threshold trigger: full outcome coverage (R2, T2) ---

func TestEvaluateEnforcement_Deposit_LicensedButDormant(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	mustLicenseTenant(t, pool, f)
	d := evalDeposit(t, pool, f, 1_000_000_000)
	if d.Outcome != OutcomeNotRequired || !d.Allowed {
		t.Fatalf("expected not_required/allow with a licence bound but no active policy, got %+v", d)
	}
}

func TestEvaluateEnforcement_Deposit_BelowThreshold_NotRequired(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{
		LicensingJurisdictionID: jurisdictionID, TriggerType: "cumulative_deposit",
		ThresholdMinorUnits: strPtr("100000"), AssetCode: strPtr("EUR"),
	})
	d := evalDeposit(t, pool, f, 500) // well below threshold, no prior deposits
	if d.Outcome != OutcomeNotRequired || !d.Allowed {
		t.Fatalf("expected not_required/allow below threshold, got %+v", d)
	}
}

// TestEvaluateEnforcement_Deposit_AtThreshold_RequiresVerification proves
// the boundary: a deposit that reaches the threshold exactly requires a
// passed verification (>= threshold triggers, per evaluateDepositThreshold's
// total.Cmp(threshold) < 0 check).
func TestEvaluateEnforcement_Deposit_AtThreshold_RequiresVerification(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{
		LicensingJurisdictionID: jurisdictionID, TriggerType: "cumulative_deposit",
		ThresholdMinorUnits: strPtr("100000"), AssetCode: strPtr("EUR"),
	})

	denied := evalDeposit(t, pool, f, 100000)
	if denied.Outcome != OutcomeFailed || denied.Allowed {
		t.Fatalf("expected failed/deny at the threshold with no verification, got %+v", denied)
	}

	setVerification(t, pool, f, StatusApproved, nil)
	allowed := evalDeposit(t, pool, f, 100000)
	if allowed.Outcome != OutcomePassed || !allowed.Allowed {
		t.Fatalf("expected passed/allow at the threshold with an approved verification, got %+v", allowed)
	}
}

// TestEvaluateEnforcement_Deposit_AssetNotCovered_Unavailable proves
// security N2: an active cumulative_deposit policy exists for this
// jurisdiction (a different asset), so the requested asset's deposit
// fails closed as `unavailable`, never `not_required` (MX2's own target).
func TestEvaluateEnforcement_Deposit_AssetNotCovered_Unavailable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{
		LicensingJurisdictionID: jurisdictionID, TriggerType: "cumulative_deposit",
		ThresholdMinorUnits: strPtr("100000"), AssetCode: strPtr("EUR"),
	})
	setVerification(t, pool, f, StatusApproved, nil) // approved, but must not matter here
	d := evalDeposit2(t, pool, f, 100_000_000, "USD")
	if d.Outcome != OutcomeUnavailable || d.Allowed {
		t.Fatalf("expected unavailable/deny for an asset with no covering policy row, got %+v", d)
	}
}

func evalDeposit2(t *testing.T, pool *db.Pool, f fixture, amount int64, assetCode string) EnforcementDecision {
	t.Helper()
	var decision EnforcementDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = EvaluateEnforcement(ctx, tx, EnforcementParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			Operation: EnforcementDeposit, AssetCode: assetCode, Amount: amount, CorrelationID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("EvaluateEnforcement: %v", err)
	}
	return decision
}

// --- Play trigger: full outcome coverage (R1/R2, T2) ---

func evalPlay(t *testing.T, pool *db.Pool, f fixture, op EnforcementOperation) EnforcementDecision {
	t.Helper()
	var decision EnforcementDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = EvaluateEnforcement(ctx, tx, EnforcementParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			Operation: op, AssetCode: "EUR", Amount: 1000, CorrelationID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("EvaluateEnforcement: %v", err)
	}
	return decision
}

func TestEvaluateEnforcement_Play_NoActivePolicy_NotRequired(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	mustLicenseTenant(t, pool, f)
	d := evalPlay(t, pool, f, EnforcementCasinoPlay)
	if d.Outcome != OutcomeNotRequired || !d.Allowed {
		t.Fatalf("expected not_required/allow with no active play policy, got %+v", d)
	}
}

func TestEvaluateEnforcement_Play_ActivePolicy_DeniesUnverified(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{
		LicensingJurisdictionID: jurisdictionID, TriggerType: "play", PlayOperation: strPtr("casino_play"),
	})
	d := evalPlay(t, pool, f, EnforcementCasinoPlay)
	if d.Outcome != OutcomeFailed || d.Allowed {
		t.Fatalf("expected failed/deny with an active casino_play policy and no verification, got %+v", d)
	}
}

func TestEvaluateEnforcement_Play_ActivePolicy_AllowsApproved(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{
		LicensingJurisdictionID: jurisdictionID, TriggerType: "play", PlayOperation: strPtr("casino_play"),
	})
	setVerification(t, pool, f, StatusApproved, nil)
	d := evalPlay(t, pool, f, EnforcementCasinoPlay)
	if d.Outcome != OutcomePassed || !d.Allowed {
		t.Fatalf("expected passed/allow with an approved verification, got %+v", d)
	}
}

func TestEvaluateEnforcement_Play_ActivePolicy_PendingDenies(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{
		LicensingJurisdictionID: jurisdictionID, TriggerType: "play", PlayOperation: strPtr("casino_play"),
	})
	setVerification(t, pool, f, StatusPending, nil)
	d := evalPlay(t, pool, f, EnforcementCasinoPlay)
	if d.Outcome != OutcomePending || d.Allowed {
		t.Fatalf("expected pending/deny, got %+v", d)
	}
}

// TestEvaluateEnforcement_Play_OperationScoped proves play_operation is
// enforced, not advisory: a casino_play policy never governs
// sportsbook_play.
func TestEvaluateEnforcement_Play_OperationScoped(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustLicenseTenant(t, pool, f)
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{
		LicensingJurisdictionID: jurisdictionID, TriggerType: "play", PlayOperation: strPtr("casino_play"),
	})
	// No verification at all - would deny under casino_play, but
	// sportsbook_play has no active policy of its own and must allow.
	d := evalPlay(t, pool, f, EnforcementSportsbookPlay)
	if d.Outcome != OutcomeNotRequired || !d.Allowed {
		t.Fatalf("expected sportsbook_play to be unaffected by a casino_play-only policy, got %+v", d)
	}
}

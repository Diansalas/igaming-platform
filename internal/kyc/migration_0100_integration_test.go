//go:build integration

// Migration mechanics for 0100 (PRH-I3, ADR 0096): up -> down -> up
// round-trip on a fresh, throwaway database (this codebase's own
// migration-reversibility CI rule), plus RLS/FORCE checks for both new
// tables and the append-only/lifecycle triggers on
// kyc_enforcement_policies.
package kyc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func migration0100MigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	return dir
}

func migration0100ScratchPool(t *testing.T) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, "kyc_m0100_")
	pool, err := db.Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// stepsThrough100 returns how many MigrateDown steps are needed, from the
// current tip, to roll back migration 0100 itself (inclusive) - NOT a
// hardcoded 1. Later migrations (e.g. 0101, merged in by another
// workstream after 0100 was applied) can move ahead of 0100 in the
// chain; a hardcoded "down 1" would then roll back the wrong migration
// and silently pass a guard test for the wrong reason. applied is the
// (ascending, per db.Pool.MigrateUp's contract) list of migration
// numbers just applied to a fresh scratch database.
func stepsThrough100(t *testing.T, applied []int64) int {
	t.Helper()
	steps := 0
	for _, n := range applied {
		if n >= 100 {
			steps++
		}
	}
	if steps == 0 {
		t.Fatalf("migration 100 not found in the applied chain %v", applied)
	}
	return steps
}

func regclassExists(t *testing.T, pool *db.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists)
	})
	if err != nil {
		t.Fatalf("check relation %s: %v", name, err)
	}
	return exists
}

// TestMigration0100_UpDownUpRoundTrip proves both new tables round-trip
// cleanly on a fresh database (QA gap 2 / ADR 0096 §7.2).
func TestMigration0100_UpDownUpRoundTrip(t *testing.T) {
	pool := migration0100ScratchPool(t)
	dir := migration0100MigrationsDir(t)

	appliedUp, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	if !regclassExists(t, pool, "kyc_enforcement_policies") {
		t.Fatal("expected kyc_enforcement_policies to exist after migrating up")
	}
	if !regclassExists(t, pool, "kyc_enforcement_decisions") {
		t.Fatal("expected kyc_enforcement_decisions to exist after migrating up")
	}

	steps := stepsThrough100(t, appliedUp)
	rolledBack, err := pool.MigrateDown(context.Background(), dir, steps)
	if err != nil {
		t.Fatalf("migrate down through 0100: %v", err)
	}
	found100 := false
	for _, n := range rolledBack {
		if n == 100 {
			found100 = true
		}
	}
	if !found100 {
		t.Fatalf("expected migration 100 to be among the rolled-back migrations, got %v", rolledBack)
	}
	if regclassExists(t, pool, "kyc_enforcement_policies") {
		t.Fatal("expected kyc_enforcement_policies to be dropped after rolling back migration 0100")
	}
	if regclassExists(t, pool, "kyc_enforcement_decisions") {
		t.Fatal("expected kyc_enforcement_decisions to be dropped after rolling back migration 0100")
	}

	rolledUp, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("re-apply migration 0100 (and anything else rolled back with it): %v", err)
	}
	foundReapplied100 := false
	for _, n := range rolledUp {
		if n == 100 {
			foundReapplied100 = true
		}
	}
	if !foundReapplied100 {
		t.Fatalf("expected migration 100 to be among the re-applied migrations, got %v", rolledUp)
	}
	if !regclassExists(t, pool, "kyc_enforcement_policies") || !regclassExists(t, pool, "kyc_enforcement_decisions") {
		t.Fatal("expected both tables to exist again after re-applying migration 0100")
	}
}

// TestMigration0100_DownRefusesWhileDecisionsHoldRows proves B2 (code
// review rv-prh-i3-code-review.md): the down migration must refuse,
// loudly, rather than silently destroying the append-only KYC decision
// audit trail, mirroring migrations 0048/0052/0075's own precedent.
func TestMigration0100_DownRefusesWhileDecisionsHoldRows(t *testing.T) {
	pool := migration0100ScratchPool(t)
	dir := migration0100MigrationsDir(t)
	appliedUp, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	steps := stepsThrough100(t, appliedUp)

	tenantID, brandID, playerID, personID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'T', 'under_platform_licence')`, tenantID, "t-"+tenantID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'B')`, brandID, tenantID, "b-"+brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerID, tenantID, brandID, personID, playerID.String()+"@example.com"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO kyc_enforcement_decisions (tenant_id, brand_id, player_account_id, operation, outcome, allowed, policy_version, correlation_id)
			VALUES ($1, $2, $3, 'withdrawal_hold', 'failed', false, 'test', $4)`,
			tenantID, brandID, playerID, uuid.New())
		return err
	})
	if err != nil {
		t.Fatalf("seed decision row: %v", err)
	}

	if _, err := pool.MigrateDown(context.Background(), dir, steps); err == nil {
		t.Fatal("expected migrating down through 0100 to be refused while kyc_enforcement_decisions holds rows")
	}
	if !regclassExists(t, pool, "kyc_enforcement_decisions") {
		t.Fatal("expected kyc_enforcement_decisions to still exist after the refused rollback")
	}
}

// TestMigration0100_ForceRLSOnBothTables proves FORCE ROW LEVEL SECURITY
// is set on both new tables (security condition 2's own required test:
// "a superuser-equivalent bypass would otherwise silently defeat every
// policy above").
func TestMigration0100_ForceRLSOnBothTables(t *testing.T) {
	pool := testPool(t)
	for _, table := range []string{"kyc_enforcement_policies", "kyc_enforcement_decisions"} {
		var enabled, forced bool
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&enabled, &forced)
		})
		if err != nil {
			t.Fatalf("read RLS posture for %s: %v", table, err)
		}
		if !enabled || !forced {
			t.Fatalf("expected %s to have ENABLE+FORCE row level security, got enabled=%v forced=%v", table, enabled, forced)
		}
	}
}

// TestMigration0100_TenantScopedConnectionCannotWritePolicy proves
// security condition 2: a tenant-scoped connection (even one holding
// app.platform_admin_principal_id, simulating a forged/leaked GUC) can
// never INSERT into kyc_enforcement_policies - the NULLIF(tenant_id)
// predicate, not the role name, is what gates.
func TestMigration0100_TenantScopedConnectionCannotWritePolicy(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionID := mustSeedJurisdiction(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, uuid.New().String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', 100, 'EUR', 'test', 'platform_admin', $2)`,
			jurisdictionID, uuid.New())
		return err
	})
	if err == nil {
		t.Fatal("expected a tenant-scoped connection's INSERT to be rejected regardless of app.platform_admin_principal_id")
	}
}

// TestMigration0100_PlatformServiceScopeCannotWritePolicy proves the
// platform-service (not platform-admin) scope also cannot write - only a
// genuine platform-admin principal, per db.Pool.WithPlatformAdmin, may.
func TestMigration0100_PlatformServiceScopeCannotWritePolicy(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', 100, 'EUR', 'test', 'platform_admin', $2)`,
			jurisdictionID, uuid.New())
		return err
	})
	if err == nil {
		t.Fatal("expected a bare/no-platform-admin-principal connection's INSERT to be rejected")
	}
}

// TestMigration0100_GenuinePlatformAdminCanWriteAndProvenanceMatches
// proves the positive case: a genuine platform-admin-scoped connection
// succeeds, and the inserted row's created_by_actor_id matches the acting
// principal (security condition 2's provenance-forging test).
func TestMigration0100_GenuinePlatformAdminCanWriteAndProvenanceMatches(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	principal := uuid.New()

	var id uuid.UUID
	var createdBy uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), principal, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', 100, 'EUR', 'test', 'platform_admin', $2)
			RETURNING id, created_by_actor_id`,
			jurisdictionID, principal).Scan(&id, &createdBy)
	})
	if err != nil {
		t.Fatalf("expected a genuine platform-admin write to succeed, got: %v", err)
	}
	if createdBy != principal {
		t.Fatalf("expected created_by_actor_id %s, got %s", principal, createdBy)
	}
}

// TestMigration0100_DecisionsTableCrossTenantIsolation proves a
// db.Pool.WithTenant connection scoped to tenant B cannot read tenant A's
// kyc_enforcement_decisions rows.
func TestMigration0100_DecisionsTableCrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO kyc_enforcement_decisions
				(tenant_id, brand_id, player_account_id, operation, outcome, allowed, policy_version, correlation_id)
			VALUES ($1, $2, $3, 'withdrawal_hold', 'failed', false, 'test', $4)`,
			fA.tenantID, fA.brandID, fA.playerID, uuid.New())
		return err
	})
	if err != nil {
		t.Fatalf("seed decision row for tenant A: %v", err)
	}

	var count int
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_enforcement_decisions WHERE player_account_id = $1`, fA.playerID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query as tenant B: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected tenant B to see 0 rows of tenant A's decisions, got %d", count)
	}
}

// TestMigration0100_DecisionsTableAppendOnly proves UPDATE/DELETE are
// both refused, and the DecidedAt/outcome fields cannot be altered after
// insert.
func TestMigration0100_DecisionsTableAppendOnly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var id uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_decisions
				(tenant_id, brand_id, player_account_id, operation, outcome, allowed, policy_version, correlation_id)
			VALUES ($1, $2, $3, 'withdrawal_hold', 'failed', false, 'test', $4)
			RETURNING id`,
			f.tenantID, f.brandID, f.playerID, uuid.New()).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed decision row: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_decisions SET outcome = 'passed' WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected UPDATE on kyc_enforcement_decisions to be rejected")
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM kyc_enforcement_decisions WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected DELETE on kyc_enforcement_decisions to be rejected")
	}
}

// TestMigration0100_PolicyLifecycleTransitions proves the lifecycle
// trigger permits exactly draft->active, draft->withdrawn, active->
// withdrawn (each by a DIFFERENT principal than the row's creator, per
// the four-eyes check) and refuses every other transition, including
// re-opening a withdrawn row.
func TestMigration0100_PolicyLifecycleTransitions(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	creator := uuid.New()
	activator := uuid.New()

	var id uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code, legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', 100, 'EUR', 'legal-ref-1', 'test', 'platform_admin', $2)
			RETURNING id`, jurisdictionID, creator).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed draft policy: %v", err)
	}

	// Same-principal activation must be refused (four-eyes).
	err = pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected the creator activating their own draft to be refused (four-eyes)")
	}

	// A different principal activating succeeds.
	err = pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = $1`, id)
		return err
	})
	if err != nil {
		t.Fatalf("expected a different principal's activation to succeed: %v", err)
	}

	// active -> draft is illegal.
	err = pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'draft' WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected active -> draft to be refused")
	}

	// active -> withdrawn, by a different principal, succeeds.
	withdrawer := uuid.New()
	err = pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'withdrawn' WHERE id = $1`, id)
		return err
	})
	if err != nil {
		t.Fatalf("expected active -> withdrawn to succeed: %v", err)
	}

	// withdrawn is terminal: no reopening.
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected withdrawn -> active to be refused (terminal state)")
	}
}

// TestSupersedeEnforcementPolicy_AtomicWithdrawAndActivateReplacement
// proves the security F3 partial mitigation: withdrawing an active row
// and activating its replacement commit together, in one transaction,
// with the withdraw/create/activate steps each attributed to the correct
// distinct principal.
func TestSupersedeEnforcementPolicy_AtomicWithdrawAndActivateReplacement(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	creator, activator := uuid.New(), uuid.New()

	var oldID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code, legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', 100, 'EUR', 'legal-ref-old', 'test', 'platform_admin', $2)
			RETURNING id`, jurisdictionID, creator).Scan(&oldID)
	})
	if err != nil {
		t.Fatalf("seed draft policy: %v", err)
	}
	if err := pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
		return ActivateEnforcementPolicy(ctx, tx, oldID)
	}); err != nil {
		t.Fatalf("activate old policy: %v", err)
	}

	withdrawer, newCreator, newActivator := uuid.New(), uuid.New(), uuid.New()
	var newID uuid.UUID
	err = pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		newID, err = SupersedeEnforcementPolicy(ctx, tx, SupersedeEnforcementPolicyParams{
			WithdrawID: oldID, WithdrawFromStatus: "active", WithdrawnBy: withdrawer,
			NewPolicy: CreateEnforcementPolicyParams{
				LicensingJurisdictionID: jurisdictionID, TriggerType: "cumulative_deposit",
				ThresholdMinorUnits: strPtr("200000"), AssetCode: strPtr("EUR"),
				LegalReviewReference: "legal-ref-new", ReasonCode: "test",
			},
			CreatedBy: newCreator, ActivatedBy: newActivator,
		})
		return err
	})
	if err != nil {
		t.Fatalf("SupersedeEnforcementPolicy: %v", err)
	}

	var oldStatus, newStatus string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM kyc_enforcement_policies WHERE id = $1`, oldID).Scan(&oldStatus)
	}); err != nil {
		t.Fatalf("read old status: %v", err)
	}
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM kyc_enforcement_policies WHERE id = $1`, newID).Scan(&newStatus)
	}); err != nil {
		t.Fatalf("read new status: %v", err)
	}
	if oldStatus != "withdrawn" {
		t.Fatalf("expected old policy withdrawn, got %q", oldStatus)
	}
	if newStatus != "active" {
		t.Fatalf("expected new policy active, got %q", newStatus)
	}
}

// TestSupersedeEnforcementPolicy_RollsBackAtomically proves the whole
// supersede rolls back together if any step fails - never a withdrawn
// old row with no activated replacement.
func TestSupersedeEnforcementPolicy_RollsBackAtomically(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	creator, activator := uuid.New(), uuid.New()

	var oldID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code, legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', 100, 'EUR', 'legal-ref-old2', 'test', 'platform_admin', $2)
			RETURNING id`, jurisdictionID, creator).Scan(&oldID)
	})
	if err != nil {
		t.Fatalf("seed draft policy: %v", err)
	}
	if err := pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
		return ActivateEnforcementPolicy(ctx, tx, oldID)
	}); err != nil {
		t.Fatalf("activate old policy: %v", err)
	}

	withdrawer := uuid.New()
	err = pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SupersedeEnforcementPolicy(ctx, tx, SupersedeEnforcementPolicyParams{
			WithdrawID: oldID, WithdrawFromStatus: "active", WithdrawnBy: withdrawer,
			NewPolicy: CreateEnforcementPolicyParams{
				LicensingJurisdictionID: jurisdictionID, TriggerType: "cumulative_deposit",
				ThresholdMinorUnits: strPtr("200000"), AssetCode: strPtr("EUR"),
				LegalReviewReference: "legal-ref-new2", ReasonCode: "test",
			},
			// Same principal creates and activates - the four-eyes trigger
			// must refuse this, and the whole transaction (including the
			// withdrawal of oldID) must roll back with it.
			CreatedBy: withdrawer, ActivatedBy: withdrawer,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected SupersedeEnforcementPolicy to fail when create/activate share one principal")
	}

	var oldStatus string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM kyc_enforcement_policies WHERE id = $1`, oldID).Scan(&oldStatus)
	}); err != nil {
		t.Fatalf("read old status: %v", err)
	}
	if oldStatus != "active" {
		t.Fatalf("expected old policy to remain active after the rolled-back supersede, got %q", oldStatus)
	}
}

// TestMigration0100_ActivatingUnwiredTriggerTypeRefused proves security
// re-verification N3: edd_amount/registration_tier can be authored as
// drafts but never activated - the evaluator does not consult them, so
// activation must fail closed at the database, not merely by convention.
func TestMigration0100_ActivatingUnwiredTriggerTypeRefused(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	creator := uuid.New()

	var id uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, required_tier, legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'registration_tier', 'draft', 'basic', 'legal-ref-2', 'test', 'platform_admin', $2)
			RETURNING id`, jurisdictionID, creator).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed draft registration_tier policy: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected activating a registration_tier (evaluator-unwired) row to be refused")
	}
}

//go:build integration

// Migration mechanics for 0100 (PRH-I3, ADR 0096): up -> down -> up
// round-trip on a fresh, throwaway database (this codebase's own
// migration-reversibility CI rule), plus RLS/FORCE checks for both new
// tables and the append-only/lifecycle triggers on
// kyc_enforcement_policies.
package kyc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

const migration0100Version = int64(100)

// migrationFileVersion parses the 4-digit numeric version prefix off a
// migration filename (mirrors internal/ledger's own helper of the same
// name/shape - each package needing this keeps its own unexported copy).
func migrationFileVersion(name string) (int64, error) {
	if len(name) < 4 {
		return 0, fmt.Errorf("migration filename %q is too short to carry a version prefix", name)
	}
	v, err := strconv.ParseInt(name[:4], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migration filename %q has no numeric version prefix: %w", name, err)
	}
	return v, nil
}

// stagedMigrations0100 copies the real migrations directory into a temp
// dir, holding back EVERY migration numbered ABOVE 100 - dynamically, by
// parsing each file's version prefix, never a hand-maintained list of
// specific later version numbers (internal/ledger's own
// stagedMigrations0092 precedent, added after migrations 0101/0102
// broke this package's own hardcoded "down 1 step = migration 100"
// assumption twice). This keeps TestMigration0100_UpDownUpRoundTrip and
// TestMigration0100_DownRefusesWhileDecisionsHoldRows exercising 0100's
// own up/down mechanics in isolation, so 100 is always the actual chain
// tip in their scratch database regardless of what lands above it later
// (0101, 0102, 0103, ...) - "MigrateDown(dir, 1)" then always targets
// 0100 itself, never whatever migration happens to sit above it at HEAD,
// and can never be defeated by a LATER migration's own down-guard
// refusing first. When includeMigration100 is false, 0100 itself is also
// held back (used by tests that need to observe a pre-0100 database,
// none currently in this file, kept symmetrical with the 0092 precedent
// for whoever adds one next).
func stagedMigrations0100(t *testing.T, includeMigration100 bool) (dir string, addHeld func()) {
	t.Helper()
	src := migration0100MigrationsDir(t)
	dir = t.TempDir()

	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var held []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := migrationFileVersion(e.Name())
		if err != nil {
			t.Fatalf("%v", err)
		}
		heldBack := version > migration0100Version
		if !includeMigration100 && version == migration0100Version {
			heldBack = true
		}
		if heldBack {
			held = append(held, e.Name())
			continue
		}
		copyMigrationFile0100(t, src, dir, e.Name())
	}
	return dir, func() {
		for _, name := range held {
			if strings.HasPrefix(name, "0100_") {
				copyMigrationFile0100(t, src, dir, name)
			}
		}
	}
}

func copyMigrationFile0100(t *testing.T, src, dst, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(src, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dst, name), content, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
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
	dir, _ := stagedMigrations0100(t, true)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full (held-at-0100) chain: %v", err)
	}
	if !regclassExists(t, pool, "kyc_enforcement_policies") {
		t.Fatal("expected kyc_enforcement_policies to exist after migrating up")
	}
	if !regclassExists(t, pool, "kyc_enforcement_decisions") {
		t.Fatal("expected kyc_enforcement_decisions to exist after migrating up")
	}

	rolledBack, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil {
		t.Fatalf("migrate down 1 (0100): %v", err)
	}
	if len(rolledBack) != 1 || rolledBack[0] != 100 {
		t.Fatalf("expected exactly migration 100 to be rolled back, got %v", rolledBack)
	}
	if regclassExists(t, pool, "kyc_enforcement_policies") {
		t.Fatal("expected kyc_enforcement_policies to be dropped after rolling back migration 0100")
	}
	if regclassExists(t, pool, "kyc_enforcement_decisions") {
		t.Fatal("expected kyc_enforcement_decisions to be dropped after rolling back migration 0100")
	}

	rolledUp, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("re-apply migration 0100: %v", err)
	}
	if len(rolledUp) != 1 || rolledUp[0] != 100 {
		t.Fatalf("expected exactly migration 100 to be re-applied, got %v", rolledUp)
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
	dir, _ := stagedMigrations0100(t, true)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	tenantID, brandID, playerID, personID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
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

	if _, err := pool.MigrateDown(context.Background(), dir, 1); err == nil {
		t.Fatal("expected migrating down 0100 to be refused while kyc_enforcement_decisions holds rows")
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

	// Security F3 (ADR 0096 §17.3, migration 0103): active -> withdrawn
	// with NO superseded_by_policy_id is refused, even by a different
	// principal.
	withdrawer := uuid.New()
	err = pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'withdrawn' WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected active -> withdrawn with no superseded_by_policy_id to be refused (security F3)")
	}

	// active -> withdrawn WITH a genuine, active, key-matching successor
	// named in the SAME transaction succeeds.
	var successorID uuid.UUID
	err = pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code, legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', 200, 'EUR', 'legal-ref-2', 'test', 'platform_admin', $2)
			RETURNING id`, jurisdictionID, creator).Scan(&successorID)
	})
	if err != nil {
		t.Fatalf("seed successor draft policy: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'withdrawn', superseded_by_policy_id = $2 WHERE id = $1`, id, successorID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = $1`, successorID)
		return err
	})
	if err != nil {
		t.Fatalf("expected active -> withdrawn with a genuine same-transaction successor to succeed: %v", err)
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

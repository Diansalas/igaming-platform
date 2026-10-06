//go:build integration

// PRH-2 K2 hard gates carried from security's K1 re-check
// (docs/plans/prh2-hardening-round/reviews/k1-security-recheck.md,
// ADR 0100 §19): K2-G1's dynamic complement to A-18, K2-G2 (the
// acting-tenant scoping pins that kill M9 and the masked "widen acting to
// any tenant" mutant) and K2-G3 (the acting-session status re-check that
// kills M10). These are K1 follow-ups: they exercise only migration 0112's
// setter/validator and the 0113 restrictive fences, never a K2 posting.
package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// k2ActingScope says which rows of a table a VALID acting session for
// tenant X may legitimately see (ADR 0099 §6.5 plus ADR 0100 §10.7 and the
// 0113 implementation-record additions). Every table with at least one
// visible row must be either here or on a18SelectAllowlist; every row
// visible in a table here must satisfy its predicate.
type k2ActingScope struct {
	// foreignRowsSQL counts rows the acting session sees that it must NOT
	// see. $1 = acting tenant X, $2 = acting principal P.
	foreignRowsSQL string
}

var k2ActingScopedTables = map[string]k2ActingScope{
	// K1 (0112): own platform row plus tenant X's staff rows only.
	"staff_users": {`SELECT count(*) FROM staff_users WHERE tenant_id IS DISTINCT FROM $1 AND id <> $2`},
	// K1 (0112): tenant X's grants only (M9's target).
	"staff_capability_grants": {`SELECT count(*) FROM staff_capability_grants WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	// K2 (0113), ADR 0100 §10.7 / ADR 0099 §6.5.
	"player_accounts":           {`SELECT count(*) FROM player_accounts WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"wallets":                   {`SELECT count(*) FROM wallets WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"ledger_accounts":           {`SELECT count(*) FROM ledger_accounts WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"ledger_transactions":       {`SELECT count(*) FROM ledger_transactions WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"ledger_entries":            {`SELECT count(*) FROM ledger_entries WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"wallet_balance_projection": {`SELECT count(*) FROM wallet_balance_projection WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"payment_attempts":          {`SELECT count(*) FROM payment_attempts WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"deposit_intents":           {`SELECT count(*) FROM deposit_intents WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	// K2 (0113) implementation-record additions (ADR 0100 §20; ADR 0099
	// §6.5 amendment): the acting executor's own reads.
	"asset_authorizations":            {`SELECT count(*) FROM asset_authorizations WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"staff_capability_grant_requests": {`SELECT count(*) FROM staff_capability_grant_requests WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	// K2 (0113) tables: platform-level (tenant_id NULL) policy rows are
	// legitimately readable (family A on NULL rows, ADR 0100 §10.2).
	"financial_approval_policies":      {`SELECT count(*) FROM financial_approval_policies WHERE tenant_id IS NOT NULL AND tenant_id <> $1 AND $2::uuid IS NOT NULL`},
	"tenant_financial_policy_profiles": {`SELECT count(*) FROM tenant_financial_policy_profiles WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"ledger_adjustment_requests":       {`SELECT count(*) FROM ledger_adjustment_requests WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
	"ledger_adjustment_approvals":      {`SELECT count(*) FROM ledger_adjustment_approvals WHERE tenant_id IS DISTINCT FROM $1 AND $2::uuid IS NOT NULL`},
}

// k2NonRLSBookkeepingTables are the only public tables without row-level
// security at all (found by this dynamic probe; ADR 0099 §1's "every table
// has RLS enabled" claim holds for every DOMAIN table). schema_migrations
// holds only migration versions and checksums - no tenant data, no PII -
// and the runtime role has SELECT only on it.
var k2NonRLSBookkeepingTables = map[string]bool{
	"schema_migrations": true,
}

// k2OwnerOnlyProofTables (PRH-2 R5, ADR 0110, migration 0120) are OWNER-ONLY:
// no privilege for PUBLIC or the runtime role, RLS enabled with no policy. They
// are deliberately not FORCE RLS because the SECURITY DEFINER verifier runs as
// the owner. THIS probe runs as the OWNER role (which RLS does not bind unless
// FORCEd), so it sees them; the runtime role cannot read them at all, which is
// asserted by TestActorProof_RuntimeRoleCannotReadKeysNoncesOrMint and by the
// has_table_privilege / pg_policies checks there.
var k2OwnerOnlyProofTables = map[string]bool{
	"actor_proof_keys":   true,
	"actor_proof_nonces": true,
}

// k2ActingCannotSeeLicencesExceptOwn: licences is on the §6.2 reference
// allowlist, but 0113's acting policy exposes ONLY tenant X's own licence
// row, never another tenant's - asserted separately.
const k2ForeignLicencesSQL = `SELECT count(*) FROM licences l
	WHERE NOT EXISTS (SELECT 1 FROM tenants t WHERE t.id = $1 AND t.licence_id = l.id) AND $2::uuid IS NOT NULL`

// k2SeedNullTenantFencedRows creates one NULL-tenant (platform-level) row
// in each of the two tables migration 0113 additionally fences
// (a18AdditionallyFencedTables), each on a FRESH, test-only jurisdiction
// or asset so no other test's platform floor or eligibility is affected.
// It returns the two row counts' identifying keys for the assertion.
func k2SeedNullTenantFencedRows(t *testing.T, pool *Pool) (jurisdictionCode, assetCode string) {
	t.Helper()
	ctx := context.Background()
	adminA := cgStaff(t, pool, uuid.Nil, "platform_admin")
	adminB := cgStaff(t, pool, uuid.Nil, "platform_admin")

	jurisdictionCode = "K2G1-" + strings.ToUpper(uuid.New().String()[:8])
	if err := pool.WithPlatformAdmin(ctx, adminA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'K2-G1 synthetic test jurisdiction')`, jurisdictionCode); err != nil {
			return fmt.Errorf("insert jurisdiction: %w", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO open_bet_self_exclusion_policies (jurisdiction_code, tenant_id, policy_value, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES ($1, NULL, 'SETTLE_NORMALLY', 'k2g1-test-floor', 'staff', $2)`, jurisdictionCode, adminA)
		if err != nil {
			return fmt.Errorf("insert open-bet floor: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed open_bet_self_exclusion_policies NULL-tenant row: %v", err)
	}

	assetCode = "KG" + strings.ToUpper(uuid.New().String()[:8])
	k2FileAndApprove(t, pool, assetregistry.ChangeCreate, assetCode, adminA, adminB, "", "")
	if err := pool.WithPlatformAdmin(ctx, adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.CreateAsset(ctx, tx, assetregistry.CreateAssetParams{
			Code: assetCode, AssetType: assetregistry.AssetTypeFiat, DecimalExponent: 2, DisplayName: "K2-G1 test asset",
			Actor: k2AssetActor(adminA),
		})
		return err
	}); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	k2FileAndApprove(t, pool, assetregistry.ChangeActivate, assetCode, adminA, adminB, "", "")
	if err := pool.WithPlatformAdmin(ctx, adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.SetActive(ctx, tx, assetCode, true, k2AssetActor(adminA))
		return err
	}); err != nil {
		t.Fatalf("activate asset: %v", err)
	}
	k2FileAndApprove(t, pool, assetregistry.ChangePlatformAuthorize, assetCode, adminA, adminB, "", "")
	if err := pool.WithPlatformAdmin(ctx, adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.SetPlatformAuthorized(ctx, tx, assetCode, true, k2AssetActor(adminA))
		return err
	}); err != nil {
		t.Fatalf("platform-authorize asset: %v", err)
	}
	k2FileAndApprove(t, pool, assetregistry.ChangePlatformOperationEligibility, assetCode, adminA, adminB, assetregistry.OperationReporting, "")
	if err := pool.WithPlatformAdmin(ctx, adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.ConfigureOperationEligibility(ctx, tx, assetregistry.ConfigureEligibilityParams{
			AssetCode: assetCode, Operation: assetregistry.OperationReporting, Eligible: true, Actor: k2AssetActor(adminA),
		})
		return err
	}); err != nil {
		t.Fatalf("configure platform eligibility: %v", err)
	}
	return jurisdictionCode, assetCode
}

func k2AssetActor(id uuid.UUID) assetregistry.ActorContext {
	return assetregistry.ActorContext{ActorID: id, ReasonCode: "test", RequestID: uuid.NewString()}
}

func k2FileAndApprove(t *testing.T, pool *Pool, op assetregistry.ChangeOperation, code string, requester, approver uuid.UUID, eligOp assetregistry.Operation, eligProduct string) {
	t.Helper()
	var reqID uuid.UUID
	params := assetregistry.FileChangeRequestParams{
		Operation: op, AssetCode: code, EligibilityOperation: eligOp, EligibilityProduct: eligProduct, Actor: k2AssetActor(requester),
	}
	if op == assetregistry.ChangeCreate {
		params.AssetType = assetregistry.AssetTypeFiat
		params.DecimalExponent = 2
	}
	if err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := assetregistry.FileChangeRequest(ctx, tx, params)
		reqID = req.ID
		return err
	}); err != nil {
		t.Fatalf("file %s request: %v", op, err)
	}
	if err := pool.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.DecideChangeRequest(ctx, tx, assetregistry.DecideChangeRequestParams{RequestID: reqID, Approve: true, Actor: k2AssetActor(approver)})
		return err
	}); err != nil {
		t.Fatalf("approve %s request: %v", op, err)
	}
}

// TestK2G1_ActingSessionRowVisibilityMatchesAllowlist is K2-G1's DYNAMIC
// complement to the lexical A-18 replay: a real, VALID acting session
// (opened through the sole setter) counts the rows it can see on EVERY
// public table. A table with any visible row must be on the ADR 0099 §6.2
// reference allowlist (a18SelectAllowlist) or on the closed acting-scoped
// list above; and in an acting-scoped table every visible row must belong
// to the acting tenant (or be the principal's own staff row, or a
// platform-level policy row). Foreign-tenant rows (a second grant
// fixture) and NULL-tenant rows in the two 0113-fenced tables are seeded
// first so the check is not vacuous where it matters most.
func TestK2G1_ActingSessionRowVisibilityMatchesAllowlist(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	fx := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
	fy := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentApprove)
	_ = cgStaff(t, pool, fy.TenantID, "finance")
	jurisdictionCode, assetCode := k2SeedNullTenantFencedRows(t, pool)

	var tables []string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			tables = append(tables, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) < 100 {
		t.Fatalf("expected the full public schema (>100 tables), got %d - the enumeration is broken", len(tables))
	}

	visible := map[string]int64{}
	var unexpected, leaked []string
	err := pool.WithPlatformActingInTenant(ctx, fx.GranteeID, fx.TenantID, uuid.Nil, "k2g1_dynamic_probe", func(ctx context.Context, tx pgx.Tx) error {
		for _, table := range tables {
			if _, err := tx.Exec(ctx, `SAVEPOINT k2g1`); err != nil {
				return err
			}
			var n int64
			err := tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n)
			if err != nil {
				if _, rbErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT k2g1`); rbErr != nil {
					return rbErr
				}
				continue // not readable at all: not visible
			}
			if n == 0 {
				continue
			}
			visible[table] = n
			scope, scoped := k2ActingScopedTables[table]
			if !scoped && !a18SelectAllowlist[table] && !k2NonRLSBookkeepingTables[table] && !k2OwnerOnlyProofTables[table] {
				unexpected = append(unexpected, fmt.Sprintf("%s (%d rows)", table, n))
				continue
			}
			if scoped {
				var foreign int64
				if err := tx.QueryRow(ctx, scope.foreignRowsSQL, fx.TenantID, fx.GranteeID).Scan(&foreign); err != nil {
					return fmt.Errorf("%s foreign-row count: %w", table, err)
				}
				if foreign != 0 {
					leaked = append(leaked, fmt.Sprintf("%s (%d foreign rows)", table, foreign))
				}
			}
		}
		var foreignLicences int64
		if err := tx.QueryRow(ctx, k2ForeignLicencesSQL, fx.TenantID, fx.GranteeID).Scan(&foreignLicences); err != nil {
			return err
		}
		if foreignLicences != 0 {
			leaked = append(leaked, fmt.Sprintf("licences (%d rows of another tenant's licence)", foreignLicences))
		}
		var fencedFloor, fencedElig int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM open_bet_self_exclusion_policies WHERE jurisdiction_code = $1`, jurisdictionCode).Scan(&fencedFloor); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM asset_operation_eligibility WHERE asset_code = $1`, assetCode).Scan(&fencedElig); err != nil {
			return err
		}
		if fencedFloor != 0 || fencedElig != 0 {
			leaked = append(leaked, fmt.Sprintf("0113-fenced NULL-tenant rows visible: open_bet=%d asset_operation_eligibility=%d", fencedFloor, fencedElig))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("acting probe: %v", err)
	}
	sort.Strings(unexpected)
	sort.Strings(leaked)
	if len(unexpected) > 0 {
		t.Errorf("K2-G1: a valid acting session sees rows in table(s) on neither the §6.2 reference allowlist nor the closed acting-scoped list:\n  %s", strings.Join(unexpected, "\n  "))
	}
	if len(leaked) > 0 {
		t.Errorf("K2-G1: a valid acting session sees rows outside its tenant/own-row scope:\n  %s", strings.Join(leaked, "\n  "))
	}
	// Non-vacuity: the probe must have seen its own staff row and grant.
	if visible["staff_users"] < 1 || visible["staff_capability_grants"] < 1 {
		t.Fatalf("expected the acting session to see at least its own staff row and grant, got staff_users=%d grants=%d (probe is vacuous)",
			visible["staff_users"], visible["staff_capability_grants"])
	}
	// And the same seeded rows ARE visible to a plain platform session, so
	// the zero above is the fence, not an empty seed.
	if err := pool.WithPlatformAdmin(ctx, fx.RequesterID, func(ctx context.Context, tx pgx.Tx) error {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM open_bet_self_exclusion_policies WHERE jurisdiction_code = $1`, jurisdictionCode).Scan(&n); err != nil {
			return err
		}
		var m int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM asset_operation_eligibility WHERE asset_code = $1 AND tenant_id IS NULL`, assetCode).Scan(&m); err != nil {
			return err
		}
		if n != 1 || m != 1 {
			return fmt.Errorf("seeded NULL-tenant rows not visible to a platform session: open_bet=%d aoe=%d", n, m)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestK2G2_ActingSessionCannotReadAnotherTenantsGrants is K2-G2's A-4
// case: an acting session for tenant X reads tenant Y's grants and sees 0
// rows (and 0 of Y's staff rows), while it does see X's own grant. This
// kills M9 (the staff_capability_grants acting_read tenant predicate
// removed), which previously survived because no test ever looked for a
// foreign tenant's grant under an acting session.
func TestK2G2_ActingSessionCannotReadAnotherTenantsGrants(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fx := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
	fy := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
	yFinance := cgStaff(t, pool, fy.TenantID, "finance")

	err := pool.WithPlatformActingInTenant(ctx, fx.GranteeID, fx.TenantID, uuid.Nil, "k2g2_probe", func(ctx context.Context, tx pgx.Tx) error {
		var own, foreign, foreignByID, foreignStaff int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grants WHERE tenant_id = $1`, fx.TenantID).Scan(&own); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grants WHERE tenant_id = $1`, fy.TenantID).Scan(&foreign); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grants WHERE id = $1`, fy.GrantID).Scan(&foreignByID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE id = $1`, yFinance).Scan(&foreignStaff); err != nil {
			return err
		}
		if own != 1 {
			return fmt.Errorf("expected to see exactly the own-tenant grant (1), got %d", own)
		}
		if foreign != 0 || foreignByID != 0 {
			return fmt.Errorf("M9: an acting session for X sees tenant Y's grants (by tenant: %d, by id: %d)", foreign, foreignByID)
		}
		if foreignStaff != 0 {
			return fmt.Errorf("an acting session for X sees tenant Y's staff row")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestK2G2_LayeredWidenedActingReadStillRefusesForeignGrant is K2-G2's
// rolled-back-DDL layered test (the security-methodology precedent of
// K1-C3): it removes ONE layer - the acting_read RLS tenant predicate on
// staff_capability_grants - inside a transaction that is always rolled
// back, proves the widening took effect (tenant Y's grant becomes visible
// to the acting-shaped session), and then shows the acting-session open
// step (financial_acting_session_open(), the exact statement
// WithPlatformActingInTenant runs) STILL refuses CG020 for a principal
// whose only grant is for another tenant: the validator's own
// staff_capability_grant_in_force(X, P, ...) tenant argument is an
// independent layer, not masked by RLS. The GUCs are set with raw
// set_config here, in a _test.go file, precisely so the DDL and the open
// step share one rolled-back transaction (A-16 scans non-test files only).
//
// The second half repeats the same on a scratch database where the
// widening is COMMITTED, so the real setter itself runs against it.
func TestK2G2_LayeredWidenedActingReadStillRefusesForeignGrant(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fx := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
	fy := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)

	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER POLICY acting_read ON staff_capability_grants USING (financial_acting_gucs_exact())`); err != nil {
		t.Fatalf("widen acting_read (rolled back): %v", err)
	}
	// fy.GranteeID holds a grant for tenant Y only; target X.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_platform_principal_id', $1, true)`, fy.GranteeID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true)`, fx.TenantID.String()); err != nil {
		t.Fatal(err)
	}
	var seesForeign int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grants WHERE id = $1`, fy.GrantID).Scan(&seesForeign); err != nil {
		t.Fatal(err)
	}
	if seesForeign != 1 {
		t.Fatalf("layer removal did not take effect: expected the widened policy to expose Y's grant (1), got %d", seesForeign)
	}
	_, err = tx.Exec(ctx, `SELECT financial_acting_session_open()`)
	if !cgIsCode(err, "CG020") {
		t.Fatalf("with acting_read widened, the open step must still refuse a principal whose grant is for another tenant (CG020), got %v", err)
	}
	_ = tx.Rollback(ctx)

	// Scratch half: committed widening, real setter.
	scratch, _ := scratchPoolThrough(t, "k2g2lay", 113)
	sx := mustBuildActingGrantFixtureWithCapability(t, scratch, capability.CapabilityLedgerAdjustmentInitiate)
	sy := mustBuildActingGrantFixtureWithCapability(t, scratch, capability.CapabilityLedgerAdjustmentInitiate)
	if err := scratch.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER POLICY acting_read ON staff_capability_grants USING (financial_acting_gucs_exact())`)
		return err
	}); err != nil {
		t.Fatalf("widen acting_read on scratch: %v", err)
	}
	// The widening is live: an acting session for Y now sees X's grant.
	if err := scratch.WithPlatformActingInTenant(ctx, sy.GranteeID, sy.TenantID, uuid.Nil, "k2g2_layer", func(ctx context.Context, tx pgx.Tx) error {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grants WHERE id = $1`, sx.GrantID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("expected the committed widening to expose X's grant to Y's acting session, got %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// And the real setter still refuses Y's grantee acting in X.
	called := false
	err = scratch.WithPlatformActingInTenant(ctx, sy.GranteeID, sx.TenantID, uuid.Nil, "k2g2_layer", func(ctx context.Context, tx pgx.Tx) error {
		called = true
		return nil
	})
	assertActingOpenRefusal(t, err)
	if called {
		t.Fatal("fn ran on a refused acting session")
	}
}

// TestK2G3_SuspendedGranteeActingOpenRefused is K2-G3's A-8 acting case:
// the grantee holds a valid, in-force G-P2 grant, then is suspended (status
// set by DB fixture - there is no suspend API, STAFF-LIFECYCLE-1); the sole
// setter must then refuse CG020 at its own open step. This kills M10
// (financial_acting_session_valid()'s status = 'active' check removed),
// which previously survived.
func TestK2G3_SuspendedGranteeActingOpenRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)

	// Positive control first: the grant opens a session.
	if err := pool.WithPlatformActingInTenant(ctx, f.GranteeID, f.TenantID, uuid.Nil, "k2g3", func(ctx context.Context, tx pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("positive control: valid grant must open: %v", err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, f.GranteeID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to suspend 1 row, got %d", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("suspend grantee: %v", err)
	}
	called := false
	err := pool.WithPlatformActingInTenant(ctx, f.GranteeID, f.TenantID, uuid.Nil, "k2g3", func(ctx context.Context, tx pgx.Tx) error {
		called = true
		return nil
	})
	assertActingOpenRefusal(t, err)
	if called {
		t.Fatal("fn ran for a suspended grantee")
	}
	// The grant row itself is untouched (ADR 0099 §8.4).
	if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		var revoked *time.Time
		if err := tx.QueryRow(ctx, `SELECT revoked_at FROM staff_capability_grants WHERE id = $1`, f.GrantID).Scan(&revoked); err != nil {
			return err
		}
		if revoked != nil {
			return errors.New("suspension must not write the grant row")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// scratchPoolThrough migrates a fresh scratch database with ONLY the
// migration files numbered <= version (the scratchPoolThrough0112 pattern,
// ADR 0100 §19: K2 tests never depend on a later lane's migration).
func scratchPoolThrough(t *testing.T, prefix string, version int64) (*Pool, string) {
	t.Helper()
	src := "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < 4 {
			continue
		}
		n, perr := strconv.ParseInt(name[:4], 10, 64)
		if perr != nil || n > version {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(src, name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(filepath.Join(dir, name), b, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	url := scratchdb.New(t, prefix)
	pool, err := Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through %d: %v", version, err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != version {
		t.Fatalf("expected %d to be the last applied migration, got %v", version, applied)
	}
	return pool, dir
}

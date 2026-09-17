//go:build integration

// Stage 4H-B0-R6, Workstream E: real-PostgreSQL tests for migration
// 0043's schema (open_bet_self_exclusion_policies,
// self_exclusion_enumeration_runs) and the Go resolution/write service in
// self_exclusion_policy.go / self_exclusion_enumeration.go. Follows the
// exact fixture/testPool conventions rg_integration_test.go already
// established in this package.
package rg

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// seedJurisdiction mirrors internal/risk/risk_integration_test.go's
// identical helper (unexported per-package, since Go test helpers are
// not shared across package boundaries in this codebase's convention).
func seedJurisdiction(t *testing.T, pool *db.Pool) string {
	t.Helper()
	code := "TEST-" + uuid.NewString()[:8]
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'Test Jurisdiction')`, code)
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction: %v", err)
	}
	return code
}

func setFloor(t *testing.T, pool *db.Pool, jurisdiction string, value OpenBetSelfExclusionPolicy) OpenBetSelfExclusionPolicyRow {
	t.Helper()
	var row OpenBetSelfExclusionPolicyRow
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = SetOpenBetSelfExclusionPolicy(ctx, tx, SetOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, PolicyValue: value, ReasonCode: "test-floor", ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("set jurisdiction floor: %v", err)
	}
	return row
}

func dbNow(t *testing.T, pool *db.Pool) time.Time {
	t.Helper()
	var now time.Time
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	})
	if err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	return now
}

// --- SetOpenBetSelfExclusionPolicy: tighten-only at WRITE time ---

func TestSetOpenBetSelfExclusionPolicy_JurisdictionFloorRoundTrip(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)

	row := setFloor(t, pool, jurisdiction, PolicySettleNormally)
	if row.PolicyValue != PolicySettleNormally {
		t.Fatalf("expected SETTLE_NORMALLY, got %v", row.PolicyValue)
	}
	if row.TenantID != nil || row.BrandID != nil {
		t.Fatalf("expected a jurisdiction-level row (nil tenant/brand), got %+v", row)
	}
}

func TestSetOpenBetSelfExclusionPolicy_TenantOverrideRequiresExistingFloor(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)
	tenantID := seedTenant(t, pool)

	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetOpenBetSelfExclusionPolicy(ctx, tx, SetOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, TenantID: &tenantID, PolicyValue: PolicyVoidOnSelfExclusion,
			ReasonCode: "test", ActorType: audit.ActorSystem,
		})
		return err
	})
	if !errors.Is(err, ErrNoJurisdictionFloor) {
		t.Fatalf("expected ErrNoJurisdictionFloor, got %v", err)
	}

	// Database-level backstop: a direct INSERT bypassing this package's Go
	// pre-check entirely must ALSO fail - the trigger is the authoritative
	// enforcement point, not merely the Go function's own discipline.
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO open_bet_self_exclusion_policies
				(id, jurisdiction_code, tenant_id, policy_value, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, $2, 'VOID_ON_SELF_EXCLUSION', 'bypass-attempt', 'system', gen_random_uuid())`,
			jurisdiction, tenantID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the database trigger to reject a direct insert with no jurisdiction floor, got nil error")
	}
}

func TestSetOpenBetSelfExclusionPolicy_RejectsLooseningTenantOverride(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)
	setFloor(t, pool, jurisdiction, PolicyVoidOnSelfExclusion)
	tenantID := seedTenant(t, pool)

	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetOpenBetSelfExclusionPolicy(ctx, tx, SetOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, TenantID: &tenantID, PolicyValue: PolicySettleNormally,
			ReasonCode: "test", ActorType: audit.ActorSystem,
		})
		return err
	})
	if !errors.Is(err, ErrPolicyWouldLoosenFloor) {
		t.Fatalf("expected ErrPolicyWouldLoosenFloor, got %v", err)
	}

	// The database trigger independently rejects the identical attempt
	// via raw SQL - proving config-write-time enforcement does not rely
	// solely on the Go layer (QA's Stage 4H-B0-R5 finding: both write-time
	// AND read-time enforcement are required, and write-time itself must
	// not be a single-layer, bypassable check).
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO open_bet_self_exclusion_policies
				(id, jurisdiction_code, tenant_id, policy_value, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, $2, 'SETTLE_NORMALLY', 'bypass-attempt', 'system', gen_random_uuid())`,
			jurisdiction, tenantID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the database trigger to reject a direct loosening insert, got nil error")
	}
}

func TestSetOpenBetSelfExclusionPolicy_AllowsTighteningTenantOverride(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)
	setFloor(t, pool, jurisdiction, PolicySettleNormally)
	tenantID := seedTenant(t, pool)

	var row OpenBetSelfExclusionPolicyRow
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = SetOpenBetSelfExclusionPolicy(ctx, tx, SetOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, TenantID: &tenantID, PolicyValue: PolicyVoidOnSelfExclusion,
			ReasonCode: "brand wants to be stricter", ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected tightening override to succeed, got %v", err)
	}
	if row.PolicyValue != PolicyVoidOnSelfExclusion {
		t.Fatalf("expected VOID_ON_SELF_EXCLUSION, got %v", row.PolicyValue)
	}
}

func TestSetOpenBetSelfExclusionPolicy_BrandOverrideBoundByTenantOverrideNotJustJurisdiction(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)
	// Jurisdiction floor is the PERMISSIVE value...
	setFloor(t, pool, jurisdiction, PolicySettleNormally)
	tenantID := seedTenant(t, pool)
	a := seedAccount(t, pool, tenantID, uuid.Nil)

	// ...but the tenant has already tightened to VOID.
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetOpenBetSelfExclusionPolicy(ctx, tx, SetOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, TenantID: &tenantID, PolicyValue: PolicyVoidOnSelfExclusion,
			ReasonCode: "tenant policy", ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("set tenant override: %v", err)
	}

	// A brand under that tenant must not be able to loosen BACK toward
	// the (looser) jurisdiction floor once its own tenant has tightened -
	// this is the trigger's extension beyond ADR 0034 §14.2's literal
	// jurisdiction-only text (documented in migration 0043's own comment).
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetOpenBetSelfExclusionPolicy(ctx, tx, SetOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, TenantID: &tenantID, BrandID: &a.brandID, PolicyValue: PolicySettleNormally,
			ReasonCode: "brand wants to loosen", ActorType: audit.ActorSystem,
		})
		return err
	})
	if !errors.Is(err, ErrPolicyWouldLoosenFloor) {
		t.Fatalf("expected ErrPolicyWouldLoosenFloor for a brand row loosening below its own tenant's override, got %v", err)
	}
}

// --- ResolveOpenBetSelfExclusionPolicy: fail-closed, as-of correctness (S-8), max() backstop (item 2) ---

func TestResolveOpenBetSelfExclusionPolicy_FailsClosedWhenNoRowsExist(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)

	var resolved ResolvedOpenBetSelfExclusionPolicy
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = ResolveOpenBetSelfExclusionPolicy(ctx, tx, ResolveOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, AsOf: time.Now().UTC(), ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved.Configured {
		t.Fatalf("expected Configured=false (fail-closed) with no rows at all, got %+v", resolved)
	}
}

// TestResolveOpenBetSelfExclusionPolicy_AsOfPinsToEffectiveTimestamp is
// the direct regression test for security finding S-8: resolution must
// use the self-exclusion's OWN effective timestamp, never a later "now"
// read - so a permissive-to-strict (or strict-to-permissive) config
// change committed AFTER that instant must never retroactively change
// what an earlier instant resolves to.
func TestResolveOpenBetSelfExclusionPolicy_AsOfPinsToEffectiveTimestamp(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)

	setFloor(t, pool, jurisdiction, PolicySettleNormally)
	asOfEarly := dbNow(t, pool)

	// A later, independent config change tightens the floor.
	setFloor(t, pool, jurisdiction, PolicyVoidOnSelfExclusion)
	asOfLate := dbNow(t, pool)

	resolveAt := func(asOf time.Time) ResolvedOpenBetSelfExclusionPolicy {
		var resolved ResolvedOpenBetSelfExclusionPolicy
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			resolved, err = ResolveOpenBetSelfExclusionPolicy(ctx, tx, ResolveOpenBetSelfExclusionPolicyParams{
				JurisdictionCode: jurisdiction, AsOf: asOf, ActorType: audit.ActorSystem,
			})
			return err
		})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return resolved
	}

	early := resolveAt(asOfEarly)
	if !early.Configured || early.Value != PolicySettleNormally {
		t.Fatalf("expected the EARLY as-of instant to resolve SETTLE_NORMALLY (the value in force at that instant), got %+v", early)
	}

	late := resolveAt(asOfLate)
	if !late.Configured || late.Value != PolicyVoidOnSelfExclusion {
		t.Fatalf("expected the LATE as-of instant to resolve VOID_ON_SELF_EXCLUSION, got %+v", late)
	}
}

// TestResolveOpenBetSelfExclusionPolicy_MaxAcrossScopesEvenIfWriteBypassed
// is the direct regression test for directive item 2's read-time
// backstop: even if the write-time trigger is bypassed (simulating a bug
// or a direct-SQL write outside this package), resolution must never
// surface a value looser than the strictest applicable row.
func TestResolveOpenBetSelfExclusionPolicy_MaxAcrossScopesEvenIfWriteBypassed(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)
	setFloor(t, pool, jurisdiction, PolicyVoidOnSelfExclusion)
	tenantID := seedTenant(t, pool)

	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE open_bet_self_exclusion_policies DISABLE TRIGGER open_bet_self_exclusion_policies_tighten_only`); err != nil {
			return err
		}
		defer tx.Exec(ctx, `ALTER TABLE open_bet_self_exclusion_policies ENABLE TRIGGER open_bet_self_exclusion_policies_tighten_only`)
		_, err := tx.Exec(ctx,
			`INSERT INTO open_bet_self_exclusion_policies
				(id, jurisdiction_code, tenant_id, policy_value, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, $2, 'SETTLE_NORMALLY', 'simulated-bypass', 'system', gen_random_uuid())`,
			jurisdiction, tenantID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("simulate bypassed write: %v", err)
	}

	var resolved ResolvedOpenBetSelfExclusionPolicy
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = ResolveOpenBetSelfExclusionPolicy(ctx, tx, ResolveOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, TenantID: &tenantID, AsOf: time.Now().UTC(),
			AuditTenantID: tenantID, ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !resolved.Configured || resolved.Value != PolicyVoidOnSelfExclusion {
		t.Fatalf("expected max() to still resolve VOID_ON_SELF_EXCLUSION despite a bypassed looser tenant row, got %+v", resolved)
	}
}

func TestResolveOpenBetSelfExclusionPolicy_TenantRowAloneNeverSubstitutesForMissingFloor(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)
	tenantID := seedTenant(t, pool)

	// Bypass the trigger to create a tenant-only row with NO jurisdiction
	// floor at all - an otherwise-impossible state through this package's
	// own write path, constructed here purely to prove Resolve's
	// fail-closed behavior does not depend on that impossibility holding.
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE open_bet_self_exclusion_policies DISABLE TRIGGER open_bet_self_exclusion_policies_tighten_only`); err != nil {
			return err
		}
		defer tx.Exec(ctx, `ALTER TABLE open_bet_self_exclusion_policies ENABLE TRIGGER open_bet_self_exclusion_policies_tighten_only`)
		_, err := tx.Exec(ctx,
			`INSERT INTO open_bet_self_exclusion_policies
				(id, jurisdiction_code, tenant_id, policy_value, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, $2, 'VOID_ON_SELF_EXCLUSION', 'simulated-bypass', 'system', gen_random_uuid())`,
			jurisdiction, tenantID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("simulate bypassed write: %v", err)
	}

	var resolved ResolvedOpenBetSelfExclusionPolicy
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = ResolveOpenBetSelfExclusionPolicy(ctx, tx, ResolveOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, TenantID: &tenantID, AsOf: time.Now().UTC(),
			AuditTenantID: tenantID, ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Configured {
		t.Fatalf("expected Configured=false: a tenant override can never substitute for a missing jurisdiction floor, got %+v", resolved)
	}
}

// --- Concurrency ---

// TestConcurrent_RacingPolicyWritesNeverProduceLooserThanFloor is the
// directive-required concurrency test: many goroutines race to write a
// tenant-level override for the SAME scope, some attempting to loosen
// below the jurisdiction floor. None of the loosening attempts may ever
// commit, and the final resolved state is never looser than the floor.
func TestConcurrent_RacingPolicyWritesNeverProduceLooserThanFloor(t *testing.T) {
	pool := testPool(t)
	jurisdiction := seedJurisdiction(t, pool)
	setFloor(t, pool, jurisdiction, PolicyVoidOnSelfExclusion)
	tenantID := seedTenant(t, pool)

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value := PolicyVoidOnSelfExclusion
			if i%2 == 0 {
				value = PolicySettleNormally // an attempted loosening - must never win
			}
			results[i] = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := SetOpenBetSelfExclusionPolicy(ctx, tx, SetOpenBetSelfExclusionPolicyParams{
					JurisdictionCode: jurisdiction, TenantID: &tenantID, PolicyValue: value,
					ReasonCode: "race", ActorType: audit.ActorSystem,
				})
				return err
			})
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if i%2 == 0 {
			if !errors.Is(err, ErrPolicyWouldLoosenFloor) {
				t.Errorf("goroutine %d: expected a loosening attempt to be rejected, got %v", i, err)
			}
		} else if err != nil {
			t.Errorf("goroutine %d: expected a tightening (VOID) attempt to succeed, got %v", i, err)
		}
	}

	var resolved ResolvedOpenBetSelfExclusionPolicy
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = ResolveOpenBetSelfExclusionPolicy(ctx, tx, ResolveOpenBetSelfExclusionPolicyParams{
			JurisdictionCode: jurisdiction, TenantID: &tenantID, AsOf: time.Now().UTC(),
			AuditTenantID: tenantID, ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !resolved.Configured || resolved.Value != PolicyVoidOnSelfExclusion {
		t.Fatalf("expected the final resolved value to remain VOID_ON_SELF_EXCLUSION after the race, got %+v", resolved)
	}
}

// --- SQL/Go strictness parity ---

func TestOpenBetSelfExclusionPolicyStrictness_MatchesDatabase(t *testing.T) {
	pool := testPool(t)
	for _, v := range []OpenBetSelfExclusionPolicy{PolicySettleNormally, PolicyVoidOnSelfExclusion} {
		goValue, err := openBetSelfExclusionPolicyStrictness(v)
		if err != nil {
			t.Fatalf("go strictness(%s): %v", v, err)
		}
		var dbValue int
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT open_bet_self_exclusion_policy_strictness($1)`, string(v)).Scan(&dbValue)
		})
		if err != nil {
			t.Fatalf("db strictness(%s): %v", v, err)
		}
		if goValue != dbValue {
			t.Fatalf("strictness mismatch for %s: go=%d db=%d - migration 0043's SQL function and rg.openBetSelfExclusionPolicyStrictness have drifted apart", v, goValue, dbValue)
		}
	}
}

// --- Enumeration completion records (S-9) ---

func TestEnumerationRun_CreateStartCompleteLifecycle(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)
	a := seedAccount(t, pool, tenantID, uuid.Nil)
	personID := personIDFor(t, pool, a)

	// self_exclusion_enumeration_runs.restriction_id FKs into
	// player_restrictions - seed a real one via the package's own
	// CreateSelfExclusion (under db.WithPlayerScope, matching migration
	// 0037's player_self_insert RLS policy) rather than a raw INSERT that
	// would need to satisfy that policy's own predicates by hand.
	var restrictionID uuid.UUID
	err := pool.WithPlayerScope(context.Background(), tenantID, a.accountID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := CreateSelfExclusion(ctx, tx, CreateSelfExclusionParams{TenantID: tenantID, PlayerAccountID: a.accountID})
		restrictionID = r.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed restriction: %v", err)
	}

	var run EnumerationRun
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, err = CreateEnumerationRun(ctx, tx, CreateEnumerationRunParams{
			RestrictionID: restrictionID, TenantID: tenantID, PersonID: personID, PolicyAsOf: time.Now().UTC(),
			ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("create enumeration run: %v", err)
	}
	if run.DispatchStatus != DispatchPending {
		t.Fatalf("expected pending, got %v", run.DispatchStatus)
	}

	// Idempotent replay must return the SAME row, not create a second one.
	var replay EnumerationRun
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		replay, err = CreateEnumerationRun(ctx, tx, CreateEnumerationRunParams{
			RestrictionID: restrictionID, TenantID: tenantID, PersonID: personID, PolicyAsOf: time.Now().UTC(),
			ActorType: audit.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("replay create enumeration run: %v", err)
	}
	if replay.ID != run.ID {
		t.Fatalf("expected idempotent replay to return the same run id %s, got %s", run.ID, replay.ID)
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := StartEnumerationRun(ctx, tx, run.ID, audit.ActorSystem, uuid.Nil)
		return err
	})
	if err != nil {
		t.Fatalf("start enumeration run: %v", err)
	}

	var completed EnumerationRun
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		completed, err = CompleteEnumerationRun(ctx, tx, run.ID, 3, audit.ActorSystem, uuid.Nil)
		return err
	})
	if err != nil {
		t.Fatalf("complete enumeration run: %v", err)
	}
	if completed.DispatchStatus != DispatchCompleted || completed.BetsInScopeCount != 3 || completed.CompletedAt == nil {
		t.Fatalf("expected a completed run with count=3, got %+v", completed)
	}

	// Completing an already-completed run must not silently succeed again
	// (the DB row is frozen once completed - migration 0043's own trigger).
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CompleteEnumerationRun(ctx, tx, run.ID, 5, audit.ActorSystem, uuid.Nil)
		return err
	})
	if !errors.Is(err, ErrEnumerationRunNotFound) {
		t.Fatalf("expected re-completing an already-completed run to be rejected, got %v", err)
	}
}

func TestFindMissingEnumerationRuns_DetectsGapAndClearsOnceRecorded(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)
	a := seedAccount(t, pool, tenantID, uuid.Nil)
	personID := personIDFor(t, pool, a)

	var restrictionID uuid.UUID
	err := pool.WithPlayerScope(context.Background(), tenantID, a.accountID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := CreateSelfExclusion(ctx, tx, CreateSelfExclusionParams{TenantID: tenantID, PlayerAccountID: a.accountID})
		restrictionID = r.ID
		return err
	})
	if err != nil {
		t.Fatalf("create self-exclusion: %v", err)
	}

	var gaps []EnumerationGap
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gaps, err = FindMissingEnumerationRuns(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("find missing enumeration runs: %v", err)
	}
	found := false
	for _, g := range gaps {
		if g.RestrictionID == restrictionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the just-created self-exclusion (no enumeration run yet) to be reported as a gap, got %+v", gaps)
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, err := CreateEnumerationRun(ctx, tx, CreateEnumerationRunParams{
			RestrictionID: restrictionID, TenantID: tenantID, PersonID: personID, PolicyAsOf: time.Now().UTC(),
			ActorType: audit.ActorSystem,
		})
		if err != nil {
			return err
		}
		_, err = CompleteEnumerationRun(ctx, tx, run.ID, 0, audit.ActorSystem, uuid.Nil)
		return err
	})
	if err != nil {
		t.Fatalf("record and complete enumeration run: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gaps, err = FindMissingEnumerationRuns(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("find missing enumeration runs (after recording): %v", err)
	}
	for _, g := range gaps {
		if g.RestrictionID == restrictionID {
			t.Fatalf("expected the gap to clear once an enumeration run is recorded, still present: %+v", gaps)
		}
	}
}

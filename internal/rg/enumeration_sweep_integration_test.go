//go:build integration

// Stage 4H-B0-R7, Workstream D (directive item 4): real-PostgreSQL tests
// for enumeration_sweep.go - the scheduler wiring that closes Stage
// 4H-B0-R6's "FindStalledEnumerationRuns has zero callers in a running
// system" security finding (and the identical, newly-found gap for
// FindMissingEnumerationRuns). Follows the exact fixture/testPool
// conventions this package's other integration test files already
// established, and mirrors internal/reconciliation/scheduler_integration_
// test.go's own per-tenant-isolation testing shape for the sibling
// scheduler this file's mechanism was modeled on.
package rg

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// seedSelfExclusionRestriction creates a real player_restrictions row
// (via the package's own CreateSelfExclusion, matching migration 0037's
// player_self_insert RLS policy) for tenantID/a, and returns its id.
func seedSelfExclusionRestriction(t *testing.T, pool *db.Pool, tenantID uuid.UUID, a account) uuid.UUID {
	t.Helper()
	var restrictionID uuid.UUID
	err := pool.WithPlayerScope(context.Background(), tenantID, a.accountID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := CreateSelfExclusion(ctx, tx, CreateSelfExclusionParams{TenantID: tenantID, PlayerAccountID: a.accountID})
		restrictionID = r.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed self-exclusion restriction: %v", err)
	}
	return restrictionID
}

// findEnumerationSweepOutcome locates tenantID's own outcome from a
// sweep result set - mirrors internal/reconciliation's findOutcome
// helper and its own reasoning (this package's tests share a dev
// database with every other test run this session, so the result set
// legitimately contains other tenants too).
func findEnumerationSweepOutcome(t *testing.T, outcomes []EnumerationSweepOutcome, tenantID uuid.UUID) EnumerationSweepOutcome {
	t.Helper()
	for _, o := range outcomes {
		if o.TenantID == tenantID {
			return o
		}
	}
	t.Fatalf("RunEnumerationReconciliationSweep result did not include tenant %s", tenantID)
	return EnumerationSweepOutcome{}
}

// TestRunEnumerationReconciliationSweep_DetectsMissingRun proves the
// sweep surfaces a self-exclusion restriction with NO
// self_exclusion_enumeration_runs row at all (the "never started" gap
// FindMissingEnumerationRuns detects) for the correct tenant, without
// leaking into an unrelated clean tenant's own outcome - the same
// per-tenant isolation property internal/reconciliation's sweep tests
// establish for its own stream.
func TestRunEnumerationReconciliationSweep_DetectsMissingRun(t *testing.T) {
	pool := testPool(t)

	gapTenant := seedTenant(t, pool)
	gapAccount := seedAccount(t, pool, gapTenant, uuid.Nil)
	gapRestrictionID := seedSelfExclusionRestriction(t, pool, gapTenant, gapAccount)

	cleanTenant := seedTenant(t, pool)
	cleanAccount := seedAccount(t, pool, cleanTenant, uuid.Nil)
	cleanPersonID := personIDFor(t, pool, cleanAccount)
	cleanRestrictionID := seedSelfExclusionRestriction(t, pool, cleanTenant, cleanAccount)
	// cleanTenant's restriction DOES have a completed run, so it must be
	// reported as neither missing nor stalled.
	err := pool.WithTenant(context.Background(), cleanTenant, func(ctx context.Context, tx pgx.Tx) error {
		run, err := CreateEnumerationRun(ctx, tx, CreateEnumerationRunParams{
			RestrictionID: cleanRestrictionID, TenantID: cleanTenant, PersonID: cleanPersonID, PolicyAsOf: time.Now().UTC(),
			ActorType: audit.ActorSystem,
		})
		if err != nil {
			return err
		}
		_, err = CompleteEnumerationRun(ctx, tx, run.ID, 0, audit.ActorSystem, uuid.Nil)
		return err
	})
	if err != nil {
		t.Fatalf("seed clean tenant's completed run: %v", err)
	}

	outcomes, err := RunEnumerationReconciliationSweep(context.Background(), pool, nil, time.Hour)
	if err != nil {
		t.Fatalf("RunEnumerationReconciliationSweep: %v", err)
	}

	gapOutcome := findEnumerationSweepOutcome(t, outcomes, gapTenant)
	if gapOutcome.Err != nil {
		t.Fatalf("expected gapTenant's own attempt to succeed, got %v", gapOutcome.Err)
	}
	foundGap := false
	for _, g := range gapOutcome.MissingRuns {
		if g.RestrictionID == gapRestrictionID {
			foundGap = true
		}
	}
	if !foundGap {
		t.Fatalf("expected gapTenant's missing runs to include restriction %s, got %+v", gapRestrictionID, gapOutcome.MissingRuns)
	}

	cleanOutcome := findEnumerationSweepOutcome(t, outcomes, cleanTenant)
	for _, g := range cleanOutcome.MissingRuns {
		if g.RestrictionID == cleanRestrictionID {
			t.Fatalf("expected cleanTenant's completed-run restriction to NOT be reported as missing, got %+v", cleanOutcome.MissingRuns)
		}
	}
	for _, g := range cleanOutcome.MissingRuns {
		if g.RestrictionID == gapRestrictionID {
			t.Fatalf("cross-tenant leak: gapTenant's missing restriction %s appeared in cleanTenant's own outcome", gapRestrictionID)
		}
	}

	// Directive's audit-ordering requirement: the sweep's own finding
	// must itself be reconstructable from audit_log alone.
	assertSweepAuditRecorded(t, pool, gapTenant, "rg.self_exclusion_enumeration_reconciliation.sweep_run", audit.OutcomeSuccess)
}

// TestRunEnumerationReconciliationSweep_DetectsStalledRun proves the
// sweep surfaces a self_exclusion_enumeration_runs row that exists but
// has not reached 'completed' (fix 3(b)'s stalled case) - this is the
// exact security finding (FindStalledEnumerationRuns had zero callers)
// this stage's directive requires closed. Uses the same "threshold of
// ~0 duration reports even a just-created run as stalled" technique
// TestFindStalledEnumerationRuns_DetectsStalledAndRejectsWrongScope
// already established, rather than backdating created_at directly.
func TestRunEnumerationReconciliationSweep_DetectsStalledRun(t *testing.T) {
	pool := testPool(t)

	tenantID := seedTenant(t, pool)
	a := seedAccount(t, pool, tenantID, uuid.Nil)
	personID := personIDFor(t, pool, a)
	restrictionID := seedSelfExclusionRestriction(t, pool, tenantID, a)

	var runID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, err := CreateEnumerationRun(ctx, tx, CreateEnumerationRunParams{
			RestrictionID: restrictionID, TenantID: tenantID, PersonID: personID, PolicyAsOf: time.Now().UTC(),
			ActorType: audit.ActorSystem,
		})
		runID = run.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed pending enumeration run: %v", err)
	}

	// A generous threshold: not yet stalled.
	outcomes, err := RunEnumerationReconciliationSweep(context.Background(), pool, nil, time.Hour)
	if err != nil {
		t.Fatalf("RunEnumerationReconciliationSweep (generous threshold): %v", err)
	}
	outcome := findEnumerationSweepOutcome(t, outcomes, tenantID)
	for _, s := range outcome.StalledRuns {
		if s.ID == runID {
			t.Fatalf("expected the just-created run to not be reported as stalled with a 1-hour threshold, got %+v", s)
		}
	}

	// A ~0 threshold: reported as stalled.
	outcomes, err = RunEnumerationReconciliationSweep(context.Background(), pool, nil, time.Nanosecond)
	if err != nil {
		t.Fatalf("RunEnumerationReconciliationSweep (zero threshold): %v", err)
	}
	outcome = findEnumerationSweepOutcome(t, outcomes, tenantID)
	found := false
	for _, s := range outcome.StalledRuns {
		if s.ID == runID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected run %s to be reported as stalled with a near-zero threshold, got %+v", runID, outcome.StalledRuns)
	}

	assertSweepAuditRecorded(t, pool, tenantID, "rg.self_exclusion_enumeration_reconciliation.sweep_run", audit.OutcomeSuccess)
}

// TestRunEnumerationReconciliationSweep_ZeroThresholdSubstitutesDefault
// proves a caller-supplied non-positive stalledOlderThan does not
// silently disable stalled detection (which would be indistinguishable
// from "the sweep works, but never actually reports anything") - it
// substitutes StalledRunThresholdDefault instead, mirroring
// FindStalledEnumerationRuns' own "olderThan must be positive" contract
// at the sweep's boundary rather than propagating an invalid value into
// it.
func TestRunEnumerationReconciliationSweep_ZeroThresholdSubstitutesDefault(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)
	a := seedAccount(t, pool, tenantID, uuid.Nil)
	personID := personIDFor(t, pool, a)
	restrictionID := seedSelfExclusionRestriction(t, pool, tenantID, a)

	var runID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, err := CreateEnumerationRun(ctx, tx, CreateEnumerationRunParams{
			RestrictionID: restrictionID, TenantID: tenantID, PersonID: personID, PolicyAsOf: time.Now().UTC(),
			ActorType: audit.ActorSystem,
		})
		runID = run.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed pending enumeration run: %v", err)
	}

	// A run created microseconds ago is younger than
	// StalledRunThresholdDefault (15 minutes), so passing 0 must NOT
	// report it as stalled - proving 0 substituted the generous default,
	// not a near-zero one.
	outcomes, err := RunEnumerationReconciliationSweep(context.Background(), pool, nil, 0)
	if err != nil {
		t.Fatalf("RunEnumerationReconciliationSweep(0): %v", err)
	}
	outcome := findEnumerationSweepOutcome(t, outcomes, tenantID)
	for _, s := range outcome.StalledRuns {
		if s.ID == runID {
			t.Fatalf("expected stalledOlderThan=0 to substitute the generous StalledRunThresholdDefault, not a near-zero one; run %s was reported stalled immediately after creation", runID)
		}
	}
}

// TestRunEnumerationReconciliationSweep_MultiTenantIsolation seeds a
// missing-run gap in one tenant and a stalled run in a second, sharing a
// single sweep invocation - each tenant's own outcome must contain only
// its own finding, proving the per-tenant db.Pool.WithTenant scoping
// this sweep relies on (the exact same scoping class the Stage 4H-B0-R6
// wrongly-scoped-caller finding was about) actually isolates results
// rather than accidentally aggregating or cross-contaminating them.
func TestRunEnumerationReconciliationSweep_MultiTenantIsolation(t *testing.T) {
	pool := testPool(t)

	missingTenant := seedTenant(t, pool)
	missingAccount := seedAccount(t, pool, missingTenant, uuid.Nil)
	missingRestrictionID := seedSelfExclusionRestriction(t, pool, missingTenant, missingAccount)

	stalledTenant := seedTenant(t, pool)
	stalledAccount := seedAccount(t, pool, stalledTenant, uuid.Nil)
	stalledPersonID := personIDFor(t, pool, stalledAccount)
	stalledRestrictionID := seedSelfExclusionRestriction(t, pool, stalledTenant, stalledAccount)
	var stalledRunID uuid.UUID
	err := pool.WithTenant(context.Background(), stalledTenant, func(ctx context.Context, tx pgx.Tx) error {
		run, err := CreateEnumerationRun(ctx, tx, CreateEnumerationRunParams{
			RestrictionID: stalledRestrictionID, TenantID: stalledTenant, PersonID: stalledPersonID, PolicyAsOf: time.Now().UTC(),
			ActorType: audit.ActorSystem,
		})
		stalledRunID = run.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed stalled tenant's run: %v", err)
	}

	outcomes, err := RunEnumerationReconciliationSweep(context.Background(), pool, nil, time.Nanosecond)
	if err != nil {
		t.Fatalf("RunEnumerationReconciliationSweep: %v", err)
	}

	missingOutcome := findEnumerationSweepOutcome(t, outcomes, missingTenant)
	if len(missingOutcome.StalledRuns) != 0 {
		for _, s := range missingOutcome.StalledRuns {
			if s.ID == stalledRunID {
				t.Fatalf("cross-tenant leak: stalledTenant's run %s appeared in missingTenant's outcome", stalledRunID)
			}
		}
	}
	foundMissing := false
	for _, g := range missingOutcome.MissingRuns {
		if g.RestrictionID == missingRestrictionID {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Fatalf("expected missingTenant's own gap to be reported, got %+v", missingOutcome.MissingRuns)
	}

	stalledOutcome := findEnumerationSweepOutcome(t, outcomes, stalledTenant)
	for _, g := range stalledOutcome.MissingRuns {
		if g.RestrictionID == missingRestrictionID {
			t.Fatalf("cross-tenant leak: missingTenant's restriction %s appeared in stalledTenant's outcome", missingRestrictionID)
		}
	}
	foundStalled := false
	for _, s := range stalledOutcome.StalledRuns {
		if s.ID == stalledRunID {
			foundStalled = true
		}
	}
	if !foundStalled {
		t.Fatalf("expected stalledTenant's own stalled run to be reported, got %+v", stalledOutcome.StalledRuns)
	}
}

// assertSweepAuditRecorded confirms the sweep's own attempt is itself
// durably recorded in audit_log - the directive's "audit records prove
// relevant ordering of events" requirement applied to the sweep step
// itself: a regulator must be able to see, from audit_log alone, that a
// reconciliation attempt ran for this tenant at all, not merely infer it
// from the (mutable-by-nature, UPDATE-based) self_exclusion_enumeration_
// runs table.
func assertSweepAuditRecorded(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string, outcome audit.Outcome) {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3 AND outcome = $4`,
			tenantID, action, tenantID.String(), string(outcome),
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if count == 0 {
		t.Fatalf("expected at least one %q audit_log row for tenant %s with outcome %s, found none", action, tenantID, outcome)
	}
}

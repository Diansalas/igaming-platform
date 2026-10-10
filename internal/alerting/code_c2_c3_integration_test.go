//go:build integration

package alerting

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// spyPrincipalRunner wraps a real ScopeTenantPrincipal runner and records
// every Run invocation's transaction_isolation - used the same way
// snapshotRunner is used in lf_c2_integration_test.go, but over the
// original (unmutated) runner instance itself so a test can assert
// Flush's detached retry never calls p.runner.Run a second time (LF N-1 /
// mutant MF re-verified for the principal-scope path: the retry must go
// through a freshly-CONSTRUCTED ScopedRunner, never this exact instance,
// even though both would report the same "read committed" isolation for
// this scope kind - invocation count is what actually distinguishes
// them).
type spyPrincipalRunner struct {
	pool        *db.Pool
	tenantID    uuid.UUID
	principalID uuid.UUID

	mu             sync.Mutex
	isolationsSeen []string
}

func (r *spyPrincipalRunner) Scope() RaiseScope {
	return RaiseScope{Kind: ScopeTenantPrincipal, TenantID: r.tenantID, PrincipalID: r.principalID}
}
func (r *spyPrincipalRunner) Run(ctx context.Context, fn db.TxFunc) error {
	return r.pool.WithPrincipalScope(ctx, r.tenantID, r.principalID, func(ctx context.Context, tx pgx.Tx) error {
		iso, ierr := currentTransactionIsolation(ctx, tx)
		if ierr == nil {
			r.mu.Lock()
			r.isolationsSeen = append(r.isolationsSeen, iso)
			r.mu.Unlock()
		}
		return fn(ctx, tx)
	})
}
func (r *spyPrincipalRunner) Pool() *db.Pool { return r.pool }

// seedTenantStaffWithID is like seedTenantStaff but lets the caller pick
// the staff_users id - needed by
// TestFlush_PrincipalRunner_PersistsWithRaisedByScope, which must raise
// from a specific principalID BEFORE that principal's staff_users row
// exists (to force a swallowable P0001 from alerting_session_scope), then
// create the row with that exact id before calling Flush.
func seedTenantStaffWithID(t *testing.T, pool *db.Pool, tenantID, staffID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			staffID, tenantID, "staff-"+staffID.String()+"@tenant.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant staff with id: %v", err)
	}
}

// TestFlush_PlatformAdminRunner_PersistsWithRaisedByScope is code review
// C-2: freshReadCommittedRunner's ScopePlatformAdmin case must actually be
// exercised by a genuine in-tx-swallow-then-detached-retry round trip,
// not just asserted not to be a refusingRunner in isolation
// (TestFreshReadCommittedRunner_KnownKindsDoNotRefuse already covers
// that). Removing the ScopePlatformAdmin case from
// freshReadCommittedRunner (mutant MG) makes Flush's retry go through
// refusingRunner instead - RaiseDetached would then fail every attempt
// and exhaust to the §6.3 terminal fallback (alerting.raise_failed)
// instead of ever persisting the original platform-owned alert. This
// test forces a genuine, transient in-tx swallow (a subject_tenant_id FK
// violation - class 23, swallowable) by raising with a subject tenant
// that does not exist yet, then creates that tenant for real before
// Flush, so only a correctly-reconstructed ScopePlatformAdmin runner can
// make the retry succeed.
func TestFlush_PlatformAdminRunner_PersistsWithRaisedByScope(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)

	notYetTenantID := uuid.New()
	discriminator := "switch:" + uuid.NewString()

	runner := NewPlatformAdminRunner(pool, admin)
	pending, err := InTx(context.Background(), runner, func(ctx context.Context, tx pgx.Tx) error {
		if rerr := RaiseGuarded(ctx, tx, Alert{
			Kind:            KindReconciliationLedgerProjectionDrift,
			SubjectTenantID: notYetTenantID,
			Discriminator:   discriminator,
			Attributes: map[string]AttrValue{
				"run_id":         "run-" + uuid.NewString(),
				"mismatch_count": 1,
			},
		}); rerr != nil {
			t.Fatalf("RaiseGuarded must never propagate a swallowable FK violation: %v", rerr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	// Nothing was persisted yet - the FK violation on subject_tenant_id
	// rolled back the savepoint.
	var countBeforeFlush int
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, discriminator).Scan(&countBeforeFlush)
	}); err != nil {
		t.Fatalf("count before flush: %v", err)
	}
	if countBeforeFlush != 0 {
		t.Fatalf("expected no alert to exist before the subject tenant is created, got %d row(s)", countBeforeFlush)
	}

	// Make the swallow's cause transient: create the subject tenant for
	// real. A correctly-reconstructed platform-admin retry runner will
	// now succeed.
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, $2, $3, 'own_licence', 'active')`,
			notYetTenantID, "alerting-c2-"+notYetTenantID.String()[:8], "alerting-c2-"+notYetTenantID.String()[:8])
		return err
	}); err != nil {
		t.Fatalf("create subject tenant: %v", err)
	}

	pending.Flush(context.Background())

	var raisedByScope string
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT ao.raised_by_scope
			FROM alert_occurrences ao
			JOIN alerts a ON a.id = ao.alert_id
			WHERE a.discriminator = $1
		`, discriminator).Scan(&raisedByScope)
	}); err != nil {
		t.Fatalf("read back after flush: %v (mutant MG: removing the ScopePlatformAdmin case from "+
			"freshReadCommittedRunner would make the detached retry refuse and the alert would never persist)", err)
	}
	if raisedByScope != "platform_admin" {
		t.Fatalf("expected raised_by_scope = platform_admin, got %q", raisedByScope)
	}
}

// TestFlush_PrincipalRunner_PersistsWithRaisedByScope is code review C-3:
// a Flush test through NewPrincipalRunner, combined with LF N-1's
// isolation-level assertion in the same test (the coordinator's own
// suggested combination). It forces a genuine, transient in-tx swallow by
// raising from a principalID that has no staff_users row yet -
// alerting_session_scope() raises a bare P0001 ('% is not a staff
// principal of tenant %'), which is in the swallow allowlist - then
// creates that staff_users row for real before calling Flush, so only a
// correctly-reconstructed, READ COMMITTED ScopeTenantPrincipal runner
// (never p.runner itself, never a refusingRunner) can make the retry
// succeed.
func TestFlush_PrincipalRunner_PersistsWithRaisedByScope(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	notYetPrincipalID := uuid.New()
	discriminator := "switch:" + uuid.NewString()

	runner := &spyPrincipalRunner{pool: pool, tenantID: tenantA, principalID: notYetPrincipalID}
	pending, err := InTx(context.Background(), runner, func(ctx context.Context, tx pgx.Tx) error {
		iso, ierr := currentTransactionIsolation(ctx, tx)
		if ierr != nil {
			t.Fatalf("read isolation: %v", ierr)
		}
		if iso != "read committed" {
			t.Fatalf("expected the original raise itself to run at read committed (NewPrincipalRunner never opens a snapshot), got %q", iso)
		}

		if rerr := RaiseGuarded(ctx, tx, Alert{
			Kind:            KindPaymentKillSwitchEngaged,
			SubjectTenantID: tenantA,
			Discriminator:   discriminator,
			Attributes:      map[string]AttrValue{"reason_code": "c3-principal-not-yet-staff"},
		}); rerr != nil {
			t.Fatalf("RaiseGuarded must never propagate a swallowable P0001: %v", rerr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	var countBeforeFlush int
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, discriminator).Scan(&countBeforeFlush)
	}); err != nil {
		t.Fatalf("count before flush: %v", err)
	}
	if countBeforeFlush != 0 {
		t.Fatalf("expected no alert to exist before the principal's staff_users row is created, got %d row(s)", countBeforeFlush)
	}

	// Make the swallow's cause transient: create the staff_users row for
	// the exact principal id the runner's scope carries.
	seedTenantStaffWithID(t, pool, tenantA, notYetPrincipalID)

	pending.Flush(context.Background())

	var raisedByScope string
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT ao.raised_by_scope
			FROM alert_occurrences ao
			JOIN alerts a ON a.id = ao.alert_id
			WHERE a.discriminator = $1
		`, discriminator).Scan(&raisedByScope); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("read back after flush: %v (mutant MG-style regression: removing the ScopeTenantPrincipal case "+
			"from freshReadCommittedRunner would make the detached retry refuse and the alert would never persist)", err)
	}
	if raisedByScope != "tenant_principal" {
		t.Fatalf("expected raised_by_scope = tenant_principal, got %q", raisedByScope)
	}

	// LF N-1 / mutant MF, re-verified for the principal-scope path:
	// spyPrincipalRunner must have been invoked EXACTLY ONCE (the original
	// raise, inside InTx) - if Flush's detached retry used p.runner
	// directly instead of freshReadCommittedRunner, it would show up here
	// as a second invocation.
	runner.mu.Lock()
	seen := append([]string(nil), runner.isolationsSeen...)
	runner.mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("expected the principal runner to be invoked exactly once (the original raise), got %d invocations (isolations: %v) - "+
			"the detached retry must use freshReadCommittedRunner, never the original runner", len(seen), seen)
	}
	if seen[0] != "read committed" {
		t.Fatalf("expected the one recorded invocation (the original raise) to be read committed, got %q", seen[0])
	}
}

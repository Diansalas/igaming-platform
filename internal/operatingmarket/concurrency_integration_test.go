//go:build integration

// Concurrency tests for CreateOperatingCountryPolicyVersion and
// CreateLicenceCountryCeilingVersion (ADR 0045 §9). MANDATORY technique
// (binding, non-negotiable): a deterministic conflict via an UNCOMMITTED
// COMPETING ROW, confirmed by polling pg_stat_activity - NEVER a
// sync.WaitGroup-only barrier or a time.Sleep-based synchronization. This
// exact mistake cost Stage 4I Phase D two fix rounds; see
// internal/jurisdiction/evaluation_policy_integration_test.go's own
// TestCreateEvaluationPolicyVersion_DeterministicConflictViaUncommittedCompetingRow
// for the technique this file reproduces for two new tables.
package operatingmarket

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// waitForBlockedStatement polls pg_stat_activity - fully deterministic,
// no timing assumption - for a backend genuinely blocked (wait_event_type
// = 'Lock') on a statement matching queryFragment.
func waitForBlockedStatement(t *testing.T, pool *db.Pool, queryFragment string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT count(*) FROM pg_stat_activity
				 WHERE wait_event_type = 'Lock' AND query ILIKE '%' || $1 || '%'`, queryFragment).Scan(&count)
		})
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if count > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestCreateOperatingCountryPolicyVersion_DeterministicConflictViaUncommittedCompetingRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "TT"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	blockerReady := make(chan struct{})
	proceedToCommit := make(chan struct{})
	blockerErrCh := make(chan error, 1)

	go func() {
		blockerErrCh <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO operating_country_policies (
					tenant_id, scope_kind, country_code, state, status, authorization_reference,
					reason_code, policy_version, created_by_actor_type, created_by_actor_id
				) VALUES ($1, 'tenant', $2, 'enabled', 'active', 'blocker-ref', 'deterministic-conflict-test-blocker', $3, 'staff', $4)`,
				f.tenantID, cc, PolicyVersion, f.staffActorID)
			if err != nil {
				return err
			}
			close(blockerReady)
			<-proceedToCommit
			return nil
		})
	}()

	<-blockerReady

	realCallResult := make(chan error, 1)
	go func() {
		realCallResult <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
				Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
				AuthorizationReference: "real-call-ref", Actor: testActor(f.staffActorID, "deterministic-conflict-test-real-call"),
			})
			return err
		})
	}()

	blocked := waitForBlockedStatement(t, pool, "INSERT INTO operating_country_policies")
	if !blocked {
		close(proceedToCommit)
		t.Fatal("timed out waiting for the real call's INSERT to block on the uncommitted blocker row (pg_stat_activity never reported a Lock wait on that statement)")
	}

	close(proceedToCommit)

	if err := <-blockerErrCh; err != nil {
		t.Fatalf("blocker transaction: expected nil error (its own insert should succeed once committed), got %v", err)
	}
	if err := <-realCallResult; !errors.Is(err, ErrConcurrentPolicyWrite) {
		t.Fatalf("expected ErrConcurrentPolicyWrite from the real call once the blocker committed (a genuine 23505 unique violation), got %v", err)
	}

	var openCount, totalCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2 AND effective_to IS NULL`, f.tenantID, cc).Scan(&openCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2`, f.tenantID, cc).Scan(&totalCount)
	})
	if err != nil {
		t.Fatalf("verify final state: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("expected exactly 1 open row (the blocker's) after the real call's failed conflicting insert, got %d", openCount)
	}
	if totalCount != 1 {
		t.Fatalf("expected exactly 1 row total for this key (the failed real call left nothing behind), got %d", totalCount)
	}
}

func TestCreateLicenceCountryCeilingVersion_DeterministicConflictViaUncommittedCompetingRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "GY"

	blockerReady := make(chan struct{})
	proceedToCommit := make(chan struct{})
	blockerErrCh := make(chan error, 1)

	go func() {
		blockerErrCh <- pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO licence_country_ceilings (
					licence_id, country_code, state, status, authorization_reference,
					reason_code, policy_version, created_by_actor_type, created_by_actor_id
				) VALUES ($1, $2, 'enabled', 'active', 'blocker-ref', 'deterministic-conflict-test-blocker', $3, 'staff', $4)`,
				f.licenceID, cc, PolicyVersion, f.platformAdmin)
			if err != nil {
				return err
			}
			close(blockerReady)
			<-proceedToCommit
			return nil
		})
	}()

	<-blockerReady

	realCallResult := make(chan error, 1)
	go func() {
		realCallResult <- pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateLicenceCountryCeilingVersion(ctx, tx, CreateLicenceCountryCeilingVersionParams{
				LicenceID: f.licenceID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
				AuthorizationReference: "real-call-ref", Actor: testActor(f.platformAdmin, "deterministic-conflict-test-real-call"),
			})
			return err
		})
	}()

	blocked := waitForBlockedStatement(t, pool, "INSERT INTO licence_country_ceilings")
	if !blocked {
		close(proceedToCommit)
		t.Fatal("timed out waiting for the real call's INSERT to block on the uncommitted blocker row")
	}

	close(proceedToCommit)

	if err := <-blockerErrCh; err != nil {
		t.Fatalf("blocker transaction: expected nil error, got %v", err)
	}
	if err := <-realCallResult; !errors.Is(err, ErrConcurrentPolicyWrite) {
		t.Fatalf("expected ErrConcurrentPolicyWrite from the real call once the blocker committed, got %v", err)
	}

	var openCount, totalCount int
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = $2 AND effective_to IS NULL`, f.licenceID, cc).Scan(&openCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = $2`, f.licenceID, cc).Scan(&totalCount)
	})
	if err != nil {
		t.Fatalf("verify final state: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("expected exactly 1 open row, got %d", openCount)
	}
	if totalCount != 1 {
		t.Fatalf("expected exactly 1 row total, got %d", totalCount)
	}
}

// TestOperatingCountryPolicy_ConcurrentCreatesNeverCorruptState is the
// loosened, invariant-only test for the genuine two-goroutine case: it
// asserts ONLY what holds under every legitimate interleaving - at most
// one open version exists for the key, the version chain has no gap and
// no overlap, and every audit row matches a real version - and NEVER a
// specific success/failure count.
func TestOperatingCountryPolicy_ConcurrentCreatesNeverCorruptState(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "SR"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	const n = 8
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			results <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
					Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
					AuthorizationReference: "concurrent-ref", Actor: testActor(f.staffActorID, "concurrent-create"),
				})
				return err
			})
		}(i)
	}

	successes, conflicts := 0, 0
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConcurrentPolicyWrite):
			conflicts++
		default:
			t.Fatalf("unexpected error from a concurrent create: %v", err)
		}
	}
	if successes+conflicts != n {
		t.Fatalf("expected every call to resolve to success or ErrConcurrentPolicyWrite, got %d successes + %d conflicts != %d", successes, conflicts, n)
	}
	if successes < 1 {
		t.Fatal("expected at least one call to succeed")
	}

	// Invariant: at most one open version for the key.
	var openCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2 AND effective_to IS NULL`, f.tenantID, cc).Scan(&openCount)
	})
	if err != nil {
		t.Fatalf("count open rows: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("expected exactly 1 open version regardless of interleaving, got %d", openCount)
	}

	// Invariant: no gap, no overlap in the version chain, and every row's
	// version count matches the number of successful calls.
	var totalRows int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2`, f.tenantID, cc).Scan(&totalRows)
	})
	if err != nil {
		t.Fatalf("count total rows: %v", err)
	}
	if totalRows != successes {
		t.Fatalf("expected exactly %d rows (one per successful call), got %d", successes, totalRows)
	}

	rows, err := poolQueryRows(pool, f.tenantID, cc)
	if err != nil {
		t.Fatalf("read version chain: %v", err)
	}
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		if prev.effectiveTo == nil || !prev.effectiveTo.Equal(cur.effectiveFrom) {
			t.Fatalf("version chain has a gap or overlap between rows %d and %d: prev.effective_to=%v cur.effective_from=%v", i-1, i, prev.effectiveTo, cur.effectiveFrom)
		}
	}

	// Invariant: every audit row for this key matches a real version.
	var auditCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND metadata->>'country_code' = $2`, f.tenantID, cc).Scan(&auditCount)
	})
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if auditCount != successes {
		t.Fatalf("expected exactly %d audit row(s) for this key, got %d", successes, auditCount)
	}
}

type chainRow struct {
	effectiveFrom time.Time
	effectiveTo   *time.Time
}

func poolQueryRows(pool *db.Pool, tenantID uuid.UUID, cc string) ([]chainRow, error) {
	var out []chainRow
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT effective_from, effective_to FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2
			 ORDER BY effective_from ASC`, tenantID, cc)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r chainRow
			if err := rows.Scan(&r.effectiveFrom, &r.effectiveTo); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

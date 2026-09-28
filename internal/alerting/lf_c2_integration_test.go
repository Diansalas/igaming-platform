//go:build integration

package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// snapshotRunner is a test-only ScopedRunner over db.WithTenantSnapshot
// (REPEATABLE READ) - no production ScopedRunner constructor does this
// (§7.3a: REPEATABLE READ sites use RaiseDetached/RaisePostCommit
// directly, never RaiseGuarded+InTx), so this exists purely to exercise
// RaiseGuarded's OWN defence-in-depth isolation check (LF C-2/AL-11) as
// if a future business call site mistakenly called RaiseGuarded inside a
// snapshot transaction.
type snapshotRunner struct {
	pool     *db.Pool
	tenantID uuid.UUID
}

func (r snapshotRunner) Scope() RaiseScope {
	return RaiseScope{Kind: ScopeTenant, TenantID: r.tenantID}
}
func (r snapshotRunner) Run(ctx context.Context, fn db.TxFunc) error {
	return r.pool.WithTenantSnapshot(ctx, r.tenantID, fn)
}
func (r snapshotRunner) Pool() *db.Pool { return r.pool }

// TestLFC2_RaiseGuardedDefersInsteadOfRaisingUnderRepeatableRead is LF
// C-2/AL-11: RaiseGuarded must never execute an alert statement inside a
// REPEATABLE READ transaction. It defers to Pending instead, and the
// alert still persists after commit via the mandatory detached retry -
// through a FRESH READ COMMITTED runner (freshReadCommittedRunner),
// never the snapshot runner itself.
func TestLFC2_RaiseGuardedDefersInsteadOfRaisingUnderRepeatableRead(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	discriminator := "switch:" + uuid.NewString()

	runner := snapshotRunner{pool: pool, tenantID: tenantA}
	var isolationSeen string
	pending, err := InTx(context.Background(), runner, func(ctx context.Context, tx pgx.Tx) error {
		iso, ierr := currentTransactionIsolation(ctx, tx)
		if ierr != nil {
			return ierr
		}
		isolationSeen = iso

		if rerr := RaiseGuarded(ctx, tx, Alert{
			Kind:            KindPaymentKillSwitchEngaged,
			SubjectTenantID: tenantA,
			Discriminator:   discriminator,
			Attributes:      map[string]AttrValue{"reason_code": "rr-deferred-test"},
		}); rerr != nil {
			t.Fatalf("RaiseGuarded must never propagate: %v", rerr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if isolationSeen != "repeatable read" {
		t.Fatalf("expected the test transaction itself to be repeatable read, got %q", isolationSeen)
	}

	// No alert statement ran in the snapshot: nothing exists yet.
	var countBeforeFlush int
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, discriminator).Scan(&countBeforeFlush)
	}); err != nil {
		t.Fatalf("count before flush: %v", err)
	}
	if countBeforeFlush != 0 {
		t.Fatalf("expected no alert statement to have run inside the snapshot, got %d row(s)", countBeforeFlush)
	}

	pending.Flush(context.Background())

	var countAfterFlush int
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, discriminator).Scan(&countAfterFlush)
	}); err != nil {
		t.Fatalf("count after flush: %v", err)
	}
	if countAfterFlush != 1 {
		t.Fatalf("expected the alert to persist after the post-commit detached retry, got %d row(s)", countAfterFlush)
	}
}

//go:build integration

// PRH-2 G1 code review F-4 (ADR 0104 §7 "Readers unchanged (R)"; SA-1):
// firstTombstoningRollbackReference's own explicit `a.tenant_id = t.tenant_id`
// equi-join structurally excludes a subject row (tenant_id IS NULL,
// subject_tenant_id set instead) regardless of what tenant that subject
// row concerns - this pins that with a genuine subject row present for
// the SAME tenant and the SAME action/target shape the reader looks for.
package casino

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func f4SeedTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	tenantID := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'F-4 Test Tenant', 'under_platform_licence', 'active')`,
			tenantID, "f4-"+tenantID.String()[:8])
		return err
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tenantID
}

func f4SeedPlatformAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			id, "f4-admin-"+id.String()+"@example.com")
		return err
	}); err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	return id
}

// TestFirstTombstoningRollbackReference_IgnoresSubjectRowForSameTenant is
// F-4/SA-1: a subject audit row for the SAME tenant, with the exact
// action ("casino_rollback.tombstoned") and target shape
// (target_type='ledger_transaction', target_id=<tombstoneTxID>) this
// reader filters on, plus a created_at matching the ledger row's
// posted_at (the reader's own join key), must never be picked up - the
// reader's explicit `a.tenant_id = t.tenant_id` join can never match a
// subject row, whose tenant_id is always NULL by the platform-only CHECK
// (migration 0109).
func TestFirstTombstoningRollbackReference_IgnoresSubjectRowForSameTenant(t *testing.T) {
	pool := testPool(t)
	tenantID := f4SeedTenant(t, pool)
	admin := f4SeedPlatformAdmin(t, pool)
	tombstoneTxID := uuid.New()

	// A SUBJECT row (platform-scope, subject_tenant_id = tenantID) with the
	// EXACT action/target shape the reader queries for. audit_log is
	// append-only (UPDATE is always refused, even for a platform
	// connection), so this must be inserted FIRST and its own created_at
	// read back, rather than trying to force it to match a pre-existing
	// ledger row's posted_at after the fact.
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			ActorType: audit.ActorStaff, ActorID: admin, Action: "casino_rollback.tombstoned",
			TargetType: "ledger_transaction", TargetID: tombstoneTxID.String(), Outcome: audit.OutcomeSuccess,
			SubjectTenantID: tenantID, Metadata: map[string]any{"rollback_provider_tx_id": "f4-should-never-be-read"},
		})
	}); err != nil {
		t.Fatalf("seed subject audit row: %v", err)
	}
	var subjectCreatedAt time.Time
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT created_at FROM audit_log WHERE action = 'casino_rollback.tombstoned' AND target_id = $1`, tombstoneTxID.String()).Scan(&subjectCreatedAt)
	}); err != nil {
		t.Fatalf("read back the subject row's created_at: %v", err)
	}

	// A real ledger_transactions row this reader's own JOIN key
	// (a.created_at = t.posted_at) can match against - posted_at forced to
	// EXACTLY the subject row's own created_at, so if the reader's tenant
	// equi-join were ever weakened, this test would actually observe the
	// leak rather than passing for an unrelated timestamp-mismatch reason.
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id, posted_at)
			VALUES ($1, $2, 'tombstone', $3, $1, $4)`,
			tombstoneTxID, tenantID, "f4-tombstone-"+tombstoneTxID.String(), subjectCreatedAt)
		return err
	}); err != nil {
		t.Fatalf("seed ledger_transactions row: %v", err)
	}

	var ref string
	var found bool
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ref, found, err = firstTombstoningRollbackReference(ctx, tx, tenantID, tombstoneTxID)
		return err
	}); err != nil {
		t.Fatalf("firstTombstoningRollbackReference: %v", err)
	}
	if found {
		t.Fatalf("F-4/SA-1: expected the subject row to be invisible to this reader, got found=true ref=%q", ref)
	}
}

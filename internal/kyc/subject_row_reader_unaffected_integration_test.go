//go:build integration

// PRH-2 G1 code review F-4 (ADR 0104 §7 "Readers unchanged (R)"):
// provider.go's held-for-review de-duplication EXISTS check
// (heldForReviewDedupQuery) can never match a subject row - its tenant_id
// is always NULL by migration 0109's platform-only CHECK, whatever its
// subject_tenant_id. R-2 (security re-review): this test now calls
// production's own heldForReviewDedupQuery constant directly, rather than
// a hand-copied duplicate of its SQL text that could silently drift from
// what production actually runs.
package kyc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func f4SeedTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	tenantID := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'F-4 KYC Test Tenant', 'under_platform_licence', 'active')`,
			tenantID, "f4kyc-"+tenantID.String()[:8])
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
			id, "f4kyc-admin-"+id.String()+"@example.com")
		return err
	}); err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	return id
}

// TestHeldForReviewDedup_IgnoresSubjectRowForSameTenant runs production's
// own heldForReviewDedupQuery against a subject row that matches every
// OTHER predicate (same tenant, same action, same target_id, same
// provider_outcome) - it must still read as "not already recorded",
// because the subject row's tenant_id is NULL, never equal to a real
// tenant id.
func TestHeldForReviewDedup_IgnoresSubjectRowForSameTenant(t *testing.T) {
	pool := testPool(t)
	tenantID := f4SeedTenant(t, pool)
	admin := f4SeedPlatformAdmin(t, pool)
	verificationID := uuid.New()
	const outcome = "under_review"

	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			ActorType: audit.ActorStaff, ActorID: admin, Action: "kyc.provider_result_held_for_review",
			TargetType: "kyc_verification", TargetID: verificationID.String(), Outcome: audit.OutcomeSuccess,
			SubjectTenantID: tenantID, Metadata: map[string]any{"provider_outcome": outcome},
		})
	}); err != nil {
		t.Fatalf("seed subject audit row: %v", err)
	}

	var alreadyRecorded bool
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, heldForReviewDedupQuery, tenantID, verificationID.String(), outcome).Scan(&alreadyRecorded)
	}); err != nil {
		t.Fatalf("run the dedup query: %v", err)
	}
	if alreadyRecorded {
		t.Fatal("F-4: expected the held-for-review dedup check to ignore the subject row, got alreadyRecorded=true")
	}
}

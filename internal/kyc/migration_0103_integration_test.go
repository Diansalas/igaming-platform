//go:build integration

// Migration 0103 (security F3, DR-PRHI3-07, ADR 0096 §17.3): DB-level
// closure of "withdrawing an ACTIVE kyc_enforcement_policies row must
// name a genuine, active, key-matching successor". These tests exercise
// the raw SQL/trigger behaviour directly (not through
// kyc.SupersedeEnforcementPolicy, which is covered in
// migration_0100_integration_test.go) so the "arbitrary/draft row" and
// "wrong key" gaps the orchestrator specifically named are proven closed
// independent of the Go helper's own ordering discipline.
package kyc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// seedActivePolicy authors and activates a policy row directly by SQL,
// returning its id. Used only to set up fixtures for these tests.
func seedActivePolicy(t *testing.T, pool *db.Pool, jurisdictionID uuid.UUID, creator, activator uuid.UUID, legalRef string) uuid.UUID {
	t.Helper()
	id := rawDraft(t, pool, creator, jurisdictionID, "cumulative_deposit", "100", "EUR", nil, nil, legalRef)
	if err := pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
		return ActivateEnforcementPolicy(ctx, tx, id, pol("fixture_activate"))
	}); err != nil {
		t.Fatalf("activate policy: %v", err)
	}
	return id
}

// TestMigration0103_WithdrawActiveRefusedWithNoSuccessor is the DB-level
// proof security F3 asked for: WithdrawEnforcementPolicy (which never
// sets superseded_by_policy_id) fails against an ACTIVE row, at the
// database, not merely by Go-level convention.
func TestMigration0103_WithdrawActiveRefusedWithNoSuccessor(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	creator, activator, withdrawer := polAdmin(t, pool), polAdmin(t, pool), polAdmin(t, pool)
	id := seedActivePolicy(t, pool, jurisdictionID, creator, activator, "legal-f3-nosucc")

	err := pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		return WithdrawEnforcementPolicy(ctx, tx, id, "active", pol("fixture_withdraw"))
	})
	if err == nil {
		t.Fatal("expected WithdrawEnforcementPolicy on an ACTIVE row with no successor to be refused by the database (security F3)")
	}

	var status string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM kyc_enforcement_policies WHERE id = $1`, id).Scan(&status)
	}); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "active" {
		t.Fatalf("expected the policy to remain active after the refused withdrawal, got %q", status)
	}
}

// TestMigration0103_SupersessionRefusedIfSuccessorNeverActivated proves
// the "arbitrary/draft row" gap the orchestrator named is closed: naming
// a successor that stays 'draft' by commit time is refused.
func TestMigration0103_SupersessionRefusedIfSuccessorNeverActivated(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	creator, activator, withdrawer := polAdmin(t, pool), polAdmin(t, pool), polAdmin(t, pool)
	oldID := seedActivePolicy(t, pool, jurisdictionID, creator, activator, "legal-f3-draft-succ-old")

	draftSuccessorID := rawDraft(t, pool, creator, jurisdictionID, "cumulative_deposit", "100", "EUR", nil, nil, "legal-f3-draft-succ-new")

	err := pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		// Never activated - stays 'draft' through commit.
		return withdrawEnforcementPolicyWithSuccessor(ctx, tx, oldID, "active", draftSuccessorID, pol("fixture_supersede"))
	})
	if err == nil {
		t.Fatal("expected supersession naming a still-draft successor to be refused at commit")
	}

	var status string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM kyc_enforcement_policies WHERE id = $1`, oldID).Scan(&status)
	}); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "active" {
		t.Fatalf("expected the original policy to remain active (the whole transaction rolled back), got %q", status)
	}
}

// TestMigration0103_SupersessionRefusedIfSuccessorKeyDiffers proves the
// "wrong key" gap the orchestrator named is closed: a genuinely active
// successor for a DIFFERENT enforcement key (here: a different
// jurisdiction) does not satisfy the check.
func TestMigration0103_SupersessionRefusedIfSuccessorKeyDiffers(t *testing.T) {
	pool := testPool(t)
	jurisdictionA := mustSeedJurisdiction(t, pool)
	jurisdictionB := mustSeedJurisdiction(t, pool)
	creator, activator, withdrawer := polAdmin(t, pool), polAdmin(t, pool), polAdmin(t, pool)
	oldID := seedActivePolicy(t, pool, jurisdictionA, creator, activator, "legal-f3-keymismatch-old")
	// A genuinely active policy, but for jurisdictionB - not the same key.
	wrongKeySuccessorID := seedActivePolicy(t, pool, jurisdictionB, creator, activator, "legal-f3-keymismatch-new")

	err := pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		return withdrawEnforcementPolicyWithSuccessor(ctx, tx, oldID, "active", wrongKeySuccessorID, pol("fixture_supersede"))
	})
	if err == nil {
		t.Fatal("expected supersession naming an active-but-wrong-key successor to be refused at commit")
	}

	var status string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM kyc_enforcement_policies WHERE id = $1`, oldID).Scan(&status)
	}); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "active" {
		t.Fatalf("expected the original policy to remain active (the whole transaction rolled back), got %q", status)
	}
}

// TestMigration0103_SupersessionRefusedIfSelfReferencing proves a policy
// cannot name itself as its own successor.
func TestMigration0103_SupersessionRefusedIfSelfReferencing(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	creator, activator, withdrawer := polAdmin(t, pool), polAdmin(t, pool), polAdmin(t, pool)
	id := seedActivePolicy(t, pool, jurisdictionID, creator, activator, "legal-f3-self")

	err := pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		return withdrawEnforcementPolicyWithSuccessor(ctx, tx, id, "active", id, pol("fixture_supersede"))
	})
	if err == nil {
		t.Fatal("expected a policy naming itself as its own successor to be refused")
	}
}

// TestMigration0103_DraftWithdrawalCarriesNoSuccessorRequirement proves
// draft->withdrawn (a policy that was never enforced) is UNAFFECTED by
// the supersession requirement - only active->withdrawn is gated.
func TestMigration0103_DraftWithdrawalCarriesNoSuccessorRequirement(t *testing.T) {
	pool := testPool(t)
	jurisdictionID := mustSeedJurisdiction(t, pool)
	creator, withdrawer := polAdmin(t, pool), polAdmin(t, pool)

	id := rawDraft(t, pool, creator, jurisdictionID, "cumulative_deposit", "100", "EUR", nil, nil, "legal-f3-draft-withdraw")

	if err := pool.WithPlatformAdmin(context.Background(), withdrawer, func(ctx context.Context, tx pgx.Tx) error {
		return WithdrawEnforcementPolicy(ctx, tx, id, "draft", pol("fixture_withdraw"))
	}); err != nil {
		t.Fatalf("expected withdrawing a DRAFT row (never active) to succeed with no successor: %v", err)
	}
}

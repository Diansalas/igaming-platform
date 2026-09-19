//go:build integration

// Real-PostgreSQL tests for migration 0074's jurisdiction_evidence_
// collection_active table and this package's SetEvidenceCollectionActive/
// IsEvidenceCollectionActive/ListEvidenceCollectionActive accessors.
// Mirrors jurisdiction_integration_test.go's own B-6
// (jurisdiction_resolution_active) test suite exactly - same fixture,
// same RLS/append-only assertions, adjusted for this table's own columns.
package jurisdiction

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestSetEvidenceCollectionActive_RoundTripAndIsActiveAccessor(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	staffID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetEvidenceCollectionActive(ctx, tx, SetEvidenceCollectionActiveParams{
			TenantID: f.tenantID, EvidenceType: EvidenceDeclaredResidence, Active: true, ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-phase-b-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("SetEvidenceCollectionActive: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		active, err := IsEvidenceCollectionActive(ctx, tx, f.tenantID, EvidenceDeclaredResidence)
		if err != nil {
			return err
		}
		if !active {
			t.Fatal("expected IsEvidenceCollectionActive to report true after SetEvidenceCollectionActive(true)")
		}
		notSet, err := IsEvidenceCollectionActive(ctx, tx, f.tenantID, EvidenceVerifiedResidence)
		if err != nil {
			return err
		}
		if notSet {
			t.Fatal("expected IsEvidenceCollectionActive to fail closed (false) for a pair with no row at all")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("IsEvidenceCollectionActive: %v", err)
	}

	// An audit_log entry must exist for the change (CLAUDE.md's audit rule).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'jurisdiction_evidence_collection_active.changed'`,
			f.tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 audit_log row, got %d", count)
		}
		var reasonCode string
		if err := tx.QueryRow(ctx,
			`SELECT metadata ->> 'reason_code' FROM audit_log WHERE tenant_id = $1 AND action = 'jurisdiction_evidence_collection_active.changed'`,
			f.tenantID).Scan(&reasonCode); err != nil {
			return err
		}
		if reasonCode != "stage-4i-phase-b-test" {
			t.Fatalf("expected the audit record to carry the caller's reason_code, got %q", reasonCode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit check: %v", err)
	}

	// A write with no reason code is rejected outright.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetEvidenceCollectionActive(ctx, tx, SetEvidenceCollectionActiveParams{
			TenantID: f.tenantID, EvidenceType: EvidenceVerifiedResidence, Active: true,
			ActorType: ActorStaff, ActorID: staffID,
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for a missing reason_code, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("missing-reason-code check: %v", err)
	}

	// An unknown evidence_type is rejected outright.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetEvidenceCollectionActive(ctx, tx, SetEvidenceCollectionActiveParams{
			TenantID: f.tenantID, EvidenceType: EvidenceType("bogus"), Active: true,
			ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-phase-b-test",
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for an unknown evidence_type, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("invalid-evidence-type check: %v", err)
	}
}

func TestJurisdictionEvidenceCollectionActive_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	staffID := uuid.New()

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetEvidenceCollectionActive(ctx, tx, SetEvidenceCollectionActiveParams{
			TenantID: f.tenantID, EvidenceType: EvidenceDeclaredResidence, Active: true, ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-phase-b-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		active, err := IsEvidenceCollectionActive(ctx, tx, f.tenantID, EvidenceDeclaredResidence)
		if err != nil {
			return err
		}
		if active {
			t.Fatal("tenant B's connection must not be able to observe tenant A's evidence-collection-active fact as true")
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_evidence_collection_active`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("tenant B must see 0 rows of tenant A's jurisdiction_evidence_collection_active, saw %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant isolation check: %v", err)
	}
}

// SEC-4I-F4-equivalent regression guard, built into migration 0074 from
// day one rather than as a later fix: no DELETE policy exists on this
// table. There is no application DELETE path for it; the absence of a
// DELETE policy is the backstop for that fact.
func TestJurisdictionEvidenceCollectionActive_DeleteIsDeniedByRLS(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	staffID := uuid.New()

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetEvidenceCollectionActive(ctx, tx, SetEvidenceCollectionActiveParams{
			TenantID: f.tenantID, EvidenceType: EvidenceDeclaredResidence, Active: true, ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-phase-b-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM jurisdiction_evidence_collection_active WHERE tenant_id = $1 AND evidence_type = $2`,
			f.tenantID, string(EvidenceDeclaredResidence))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the DELETE to affect 0 rows (no DELETE policy must exist on jurisdiction_evidence_collection_active), affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("DELETE attempt: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		active, err := IsEvidenceCollectionActive(ctx, tx, f.tenantID, EvidenceDeclaredResidence)
		if err != nil {
			return err
		}
		if !active {
			t.Fatal("expected the evidence-collection-active fact to survive the DELETE attempt")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-delete IsEvidenceCollectionActive: %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT cmd FROM pg_policies WHERE tablename = 'jurisdiction_evidence_collection_active' ORDER BY cmd`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var cmds []string
		for rows.Next() {
			var cmd string
			if err := rows.Scan(&cmd); err != nil {
				return err
			}
			cmds = append(cmds, cmd)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		want := map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true}
		if len(cmds) != len(want) {
			t.Fatalf("expected exactly SELECT/INSERT/UPDATE policies on jurisdiction_evidence_collection_active, got %v", cmds)
		}
		for _, cmd := range cmds {
			if !want[cmd] {
				t.Fatalf("unexpected policy command %q on jurisdiction_evidence_collection_active (ALL and DELETE are both forbidden), full set %v", cmd, cmds)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("policy shape check: %v", err)
	}
}

// TestJurisdictionEvidenceCollectionActive_TruncateIsDenied is the
// regression guard for migration 0074's BEFORE TRUNCATE deny trigger,
// built in from day one rather than as a later fix like
// jurisdiction_resolution_active's own migration 0073. PostgreSQL RLS
// does not apply to TRUNCATE at all.
func TestJurisdictionEvidenceCollectionActive_TruncateIsDenied(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	staffID := uuid.New()

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetEvidenceCollectionActive(ctx, tx, SetEvidenceCollectionActiveParams{
			TenantID: f.tenantID, EvidenceType: EvidenceVerifiedResidence, Active: true,
			ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-phase-b-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE jurisdiction_evidence_collection_active`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE to be rejected on jurisdiction_evidence_collection_active - RLS does not cover TRUNCATE, so only a BEFORE TRUNCATE trigger can deny it")
	}
	assertPgCode(t, err, pgRaisedError)

	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE jurisdiction_evidence_collection_active`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE to be rejected from a player-scoped connection too")
	}
	assertPgCode(t, err, pgRaisedError)

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		active, err := IsEvidenceCollectionActive(ctx, tx, f.tenantID, EvidenceVerifiedResidence)
		if err != nil {
			return err
		}
		if !active {
			t.Fatal("expected the evidence-collection-active fact to survive both TRUNCATE attempts")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-truncate IsEvidenceCollectionActive: %v", err)
	}
}

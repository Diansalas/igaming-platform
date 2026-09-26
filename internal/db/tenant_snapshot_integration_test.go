//go:build integration

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestWithTenantAndWithTenantSnapshot_SetIdenticalTenantSessionState is
// CODE-HYGIENE-10.3-1 item 1's own regression test: WithTenant and
// WithTenantSnapshot are both thin wrappers around the shared withTenantTx
// helper (tenant_rls.go), and this test proves what that sharing actually
// gives them today - the same "app.tenant_id" GUC value, the same RLS
// enforcement (both are blocked from seeing another tenant's row, both
// read their own row identically), and a difference in transaction
// isolation level only. What PREVENTS a future divergence is the shared
// helper itself, not this test (code review F-5): this test only reads
// app.tenant_id, RLS visibility on tenant_jurisdiction_configs, and
// transaction_isolation, so a hypothetical future change that adds a GUC
// (a statement_timeout, a role switch) to one path only, without touching
// any of those three things, would not be caught here.
func TestWithTenantAndWithTenantSnapshot_SetIdenticalTenantSessionState(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)
	jurisdiction := createTestJurisdiction(t, pool)

	err := pool.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, $3)`,
			tenantA, jurisdiction, "snapshot-parity-a",
		)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed tenant A's config: %v", err)
	}
	err = pool.WithTenant(ctx, tenantB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, $3)`,
			tenantB, jurisdiction, "snapshot-parity-b",
		)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed tenant B's config: %v", err)
	}

	type observed struct {
		guc         string
		isoLevel    string
		ownRuleset  string
		sawTenantB  bool
		crossTenant error
	}

	observe := func(run func(ctx context.Context, tenantID uuid.UUID, fn TxFunc) error) observed {
		var out observed
		err := run(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&out.guc); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&out.isoLevel); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx,
				`SELECT kyc_ruleset_id FROM tenant_jurisdiction_configs WHERE tenant_id = $1`, tenantA,
			).Scan(&out.ownRuleset); err != nil {
				return err
			}

			rows, err := tx.Query(ctx, `SELECT tenant_id FROM tenant_jurisdiction_configs`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					return err
				}
				if id == tenantB {
					out.sawTenantB = true
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}

			out.crossTenant = tx.QueryRow(ctx,
				`SELECT kyc_ruleset_id FROM tenant_jurisdiction_configs WHERE tenant_id = $1`, tenantB,
			).Scan(new(string))
			return nil
		})
		if err != nil {
			t.Fatalf("observation transaction failed: %v", err)
		}
		return out
	}

	viaWithTenant := observe(pool.WithTenant)
	viaSnapshot := observe(pool.WithTenantSnapshot)

	if viaWithTenant.guc != tenantA.String() {
		t.Fatalf("WithTenant: app.tenant_id = %q, want %q", viaWithTenant.guc, tenantA.String())
	}
	if viaSnapshot.guc != tenantA.String() {
		t.Fatalf("WithTenantSnapshot: app.tenant_id = %q, want %q", viaSnapshot.guc, tenantA.String())
	}
	if viaWithTenant.guc != viaSnapshot.guc {
		t.Fatalf("app.tenant_id diverged between WithTenant (%q) and WithTenantSnapshot (%q)", viaWithTenant.guc, viaSnapshot.guc)
	}

	if viaWithTenant.ownRuleset != "snapshot-parity-a" || viaSnapshot.ownRuleset != "snapshot-parity-a" {
		t.Fatalf("both paths must read tenant A's own row identically, got WithTenant=%q WithTenantSnapshot=%q", viaWithTenant.ownRuleset, viaSnapshot.ownRuleset)
	}
	if viaWithTenant.sawTenantB || viaSnapshot.sawTenantB {
		t.Fatal("both paths must be blocked from seeing tenant B's row by RLS")
	}
	if viaWithTenant.crossTenant != pgx.ErrNoRows || viaSnapshot.crossTenant != pgx.ErrNoRows {
		t.Fatalf("both paths must deny an explicit cross-tenant lookup identically, got WithTenant=%v WithTenantSnapshot=%v", viaWithTenant.crossTenant, viaSnapshot.crossTenant)
	}

	// The one and only expected difference: isolation level.
	if viaWithTenant.isoLevel != "read committed" {
		t.Fatalf("WithTenant: transaction_isolation = %q, want %q", viaWithTenant.isoLevel, "read committed")
	}
	if viaSnapshot.isoLevel != "repeatable read" {
		t.Fatalf("WithTenantSnapshot: transaction_isolation = %q, want %q", viaSnapshot.isoLevel, "repeatable read")
	}
}

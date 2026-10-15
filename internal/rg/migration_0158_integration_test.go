//go:build integration

// ADR 0115 S4-min, migration 0158 on THROWAWAY scratch databases: a whole-schema up/down/up snapshot (the
// down restores the pre-0158 schema byte for byte), the down's refusal while an enforcement activation
// exists (RLS-independent: an activation is a recorded compliance decision and is never dropped by a
// migration), and the catalogue pins (FORCE RLS, the append-only triggers, the re-link lock trigger, the
// minimal runtime grant). Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL; a skip is NOT evidence.
package rg

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const migration0158Version = 158

func TestMigration0158_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := m135Scratch(t, "m0158s_")
	prev := m135Dir(t, migration0158Version-1)
	if _, err := pool.MigrateUp(ctx, prev); err != nil {
		t.Fatal(err)
	}
	before := m135Snapshot(t, pool)
	full := m135Dir(t, migration0158Version)
	applied, err := pool.MigrateUp(ctx, full)
	if err != nil || len(applied) != 1 || applied[0] != migration0158Version {
		t.Fatalf("up 0158: %v %v", applied, err)
	}
	after := m135Snapshot(t, pool)
	if before == after {
		t.Fatal("0158 changed nothing in the schema snapshot (vacuous)")
	}
	for _, must := range []string{
		"table:rg_enforcement_activations:rls=true:force=true",
		"trigger:rg_enforcement_activations:rg_enforcement_activations_10_actor:",
		"trigger:rg_enforcement_activations:rg_enforcement_activations_immutable:",
		"trigger:rg_enforcement_activations:rg_enforcement_activations_no_truncate:",
		"trigger:player_accounts:player_accounts_person_relink_lock:",
		"policy:rg_enforcement_activations:rg_enforcement_activations_read:",
		"policy:rg_enforcement_activations:rg_enforcement_activations_insert:",
		"constraint:rg_enforcement_activations:rg_enforcement_activations_scope_key:",
	} {
		if !strings.Contains(after, must) {
			t.Errorf("0158 snapshot lacks %q", must)
		}
	}
	// No UPDATE or DELETE policy: the switch is one-way (and the immutability trigger backs it).
	policies := 0
	for _, l := range strings.Split(after, "\n") {
		if strings.HasPrefix(l, "policy:rg_enforcement_activations:") {
			policies++
			if !strings.Contains(l, ":SELECT:") && !strings.Contains(l, ":INSERT:") {
				t.Errorf("rg_enforcement_activations must have only SELECT and INSERT policies: %s", l)
			}
		}
	}
	if policies != 2 {
		t.Errorf("want exactly 2 policies on rg_enforcement_activations, found %d", policies)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down with no activation: %v", err)
	}
	if got := m135Snapshot(t, pool); got != before {
		t.Fatalf("down did not restore the pre-0158 schema byte for byte:\n%s", m135Diff(before, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := m135Snapshot(t, pool); got != after {
		t.Fatalf("up after down differs from the first up:\n%s", m135Diff(after, got))
	}
}

func TestMigration0158_Down_RefusesWhileAnActivationExists(t *testing.T) {
	ctx := context.Background()
	pool, _ := m135Scratch(t, "m0158d_")
	full := m135Dir(t, migration0158Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	var tenantID, person, staff string
	if err := pool.WithPlatformAdmin(ctx, mustUUID(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO tenants (name, slug, licensing_model) VALUES ('m0158', $1, 'under_platform_licence') RETURNING id::text`,
			"m0158-"+mustUUID().String()[:8]).Scan(&tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO persons DEFAULT VALUES RETURNING id::text`).Scan(&person)
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, mustParse(tenantID), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO staff_users (tenant_id, email, password_hash, role, person_id) VALUES ($1, $2, '!x', 'tenant_admin', $3) RETURNING id::text`,
			tenantID, "m0158-"+mustUUID().String()+"@fixture.invalid", person).Scan(&staff)
	}); err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	if err := pool.WithPrincipalScope(ctx, mustParse(tenantID), mustParse(staff), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO rg_enforcement_activations (tenant_id, reason_code, attestation_ref) VALUES ($1, 'm0158', 'm0158 test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("seed an activation: %v", err)
	}
	before := m135Snapshot(t, pool)
	_, err := pool.MigrateDown(ctx, full, 1)
	if codeOf(err) != "RL099" {
		t.Fatalf("down must refuse with RL099 while an activation exists, got %v", err)
	}
	if got := m135Snapshot(t, pool); got != before {
		t.Fatalf("a refused down changed the schema:\n%s", m135Diff(before, got))
	}
}

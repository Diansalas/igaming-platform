//go:build integration

// Stage 4I Phase B: real-PostgreSQL tests for
// SetPlayerAccountDeclaredResidence/GetDeclaredResidence. Mirrors this
// file's own package's existing integration-test conventions
// (testPool/createTestTenant/createTestBrand from identity_integration_
// test.go, TestPlayerAccount_CrossTenantReadDenied's own two-part
// pattern).
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func mustRegisterTestPlayer(t *testing.T, pool *db.Pool, tenantID uuid.UUID, brand Brand, email string) PlayerAccount {
	t.Helper()
	var account PlayerAccount
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		account, err = RegisterPlayer(ctx, tx, brand, email, "hash")
		return err
	})
	if err != nil {
		t.Fatalf("failed to register player: %v", err)
	}
	return account
}

func TestSetPlayerAccountDeclaredResidence_FirstSetChangeAndResubmit(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "declared-residence-"+uuid.NewString()+"@example.com")

	// First-time set: hadPrevious=false.
	var hadPrevious, changed bool
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		hadPrevious, changed, err = SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "US", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("first set: unexpected error: %v", err)
	}
	if hadPrevious {
		t.Error("expected hadPrevious=false on the first set")
	}
	if !changed {
		t.Error("expected changed=true on the first set")
	}

	// Change to a different value: hadPrevious=true, changed=true.
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		hadPrevious, changed, err = SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "CA", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("change: unexpected error: %v", err)
	}
	if !hadPrevious {
		t.Error("expected hadPrevious=true on the change")
	}
	if !changed {
		t.Error("expected changed=true on the change")
	}

	// Re-submit the SAME value: hadPrevious=true, changed=false.
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		hadPrevious, changed, err = SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "CA", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("resubmit: unexpected error: %v", err)
	}
	if !hadPrevious {
		t.Error("expected hadPrevious=true on the resubmit")
	}
	if changed {
		t.Error("expected changed=false resubmitting the same value")
	}
}

func TestSetPlayerAccountDeclaredResidence_UnknownPlayerAccountID(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: uuid.New(), TenantID: tenant.ID, CountryCode: "US", ActorType: audit.ActorPlayer, ActorID: uuid.New(),
		})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown player_account_id, got: %v", err)
	}
}

func TestSetPlayerAccountDeclaredResidence_ActorReasonCodeValidation(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "actor-validation-"+uuid.NewString()+"@example.com")

	// Staff actor with an empty reason_code -> error.
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "US", ActorType: audit.ActorStaff, ActorID: uuid.New(),
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a staff actor with an empty reason_code, got: %v", err)
	}

	// Player actor with a non-empty reason_code -> error.
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "US", ActorType: audit.ActorPlayer, ActorID: account.ID,
			ReasonCode: "should-not-be-here",
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a player actor with a non-empty reason_code, got: %v", err)
	}
}

// TestSetPlayerAccountDeclaredResidence_AuditRecordShape asserts the exact
// metadata key set and, critically, that the country VALUE (or any hash of
// it) is not present anywhere in the audit row.
func TestSetPlayerAccountDeclaredResidence_AuditRecordShape(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "audit-shape-"+uuid.NewString()+"@example.com")

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "JP", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'player.declared_residence_set' AND target_id = $2`,
			tenant.ID, account.ID.String()).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 audit_log row, got %d", count)
		}

		var rawRow, rawMetadata string
		if err := tx.QueryRow(ctx,
			`SELECT row_to_json(audit_log)::text, metadata::text FROM audit_log WHERE tenant_id = $1 AND action = 'player.declared_residence_set' AND target_id = $2`,
			tenant.ID, account.ID.String()).Scan(&rawRow, &rawMetadata); err != nil {
			return err
		}

		// The exact metadata key set - no more, no less.
		var metadata map[string]any
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			t.Fatalf("failed to unmarshal metadata: %v", err)
		}
		wantKeys := map[string]bool{"provenance": true, "had_previous_value": true, "value_changed": true, "captured_at": true}
		if len(metadata) != len(wantKeys) {
			t.Fatalf("expected exactly %d metadata keys %v, got %v", len(wantKeys), wantKeys, metadata)
		}
		for k := range metadata {
			if !wantKeys[k] {
				t.Fatalf("unexpected metadata key %q, full metadata: %v", k, metadata)
			}
		}
		if metadata["provenance"] != "player_self_declared" {
			t.Fatalf("expected provenance=player_self_declared, got %v", metadata["provenance"])
		}

		// The country value ("JP") must never appear anywhere in the row.
		if strings.Contains(rawRow, "JP") {
			t.Fatalf("audit row must never contain the raw country value, got: %s", rawRow)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit check: %v", err)
	}
}

func TestGetDeclaredResidence_NoFactOnFileAndAfterSet(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "get-residence-"+uuid.NewString()+"@example.com")

	// No fact on file yet.
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, ok, err := GetDeclaredResidence(ctx, tx, account.ID)
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("expected ok=false before any declared residence has been set")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "DE", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("set: unexpected error: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		country, capturedAt, ok, err := GetDeclaredResidence(ctx, tx, account.ID)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("expected ok=true after a declared residence has been set")
		}
		if country != "DE" {
			t.Fatalf("expected country DE, got %q", country)
		}
		if capturedAt.IsZero() {
			t.Fatal("expected a non-zero captured_at")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("get after set: unexpected error: %v", err)
	}
}

// TestGetDeclaredResidence_CrossTenantReadReturnsNotOK mirrors
// TestPlayerAccount_CrossTenantReadDenied's own two-part pattern: a
// DIFFERENT tenant's properly-scoped connection sees zero rows (ok=false),
// the ordinary RLS-driven case - never an error.
func TestGetDeclaredResidence_CrossTenantReadReturnsNotOK(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	brandA := createTestBrand(t, pool, tenantA)
	accountA := mustRegisterTestPlayer(t, pool, tenantA.ID, brandA, "cross-tenant-residence-"+uuid.NewString()+"@example.com")
	tenantB := createTestTenant(t, pool)

	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: accountA.ID, TenantID: tenantA.ID, CountryCode: "FR", ActorType: audit.ActorPlayer, ActorID: accountA.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: unexpected error: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, ok, err := GetDeclaredResidence(ctx, tx, accountA.ID)
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("expected tenant B's scoped connection to NOT see tenant A's declared residence (ok=false)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant read: unexpected error: %v", err)
	}
}

// TestGetDeclaredResidence_UnscopedConnectionErrors is the explicit
// regression guard the architect ruling calls out: a connection with NO
// tenant context at all (pool.WithoutTenant) must ERROR, never silently
// report ok=false - "not found" and "you have no scope to ask" must never
// look identical.
func TestGetDeclaredResidence_UnscopedConnectionErrors(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "unscoped-residence-"+uuid.NewString()+"@example.com")

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "IT", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: unexpected error: %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, _, ok, err := GetDeclaredResidence(ctx, tx, account.ID)
		if err == nil {
			t.Fatalf("expected an error calling GetDeclaredResidence on an unscoped connection, got ok=%v, err=nil", ok)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
}

// TestPlayerAccounts_DeclaredResidencePairCheckConstraint proves the
// paired-NULL invariant (declared_residence_country <-> _captured_at) is
// enforced by the database itself, not merely by
// SetPlayerAccountDeclaredResidence always writing both columns together
// (CLAUDE.md: invariants must be enforced by the database, not
// application-code discipline).
func TestPlayerAccounts_DeclaredResidencePairCheckConstraint(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "check-constraint-"+uuid.NewString()+"@example.com")

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE player_accounts SET declared_residence_country = 'US' WHERE id = $1`, account.ID)
		return err
	})
	if !db.IsCheckViolation(err) {
		t.Fatalf("expected a CHECK-constraint violation setting declared_residence_country alone, got: %v", err)
	}
}

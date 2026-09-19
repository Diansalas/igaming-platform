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
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
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

// enableDeclaredResidenceCollection flips jurisdiction_evidence_collection_active
// ON for (tenantID, declared_residence) - required before every successful
// SetPlayerAccountDeclaredResidence call now that PHASE-B-ARCH-1 moved the
// gate check inside that function itself. Mirrors
// internal/kyc's own enableVerifiedResidenceCollection helper exactly.
func enableDeclaredResidenceCollection(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := jurisdiction.SetEvidenceCollectionActive(ctx, tx, jurisdiction.SetEvidenceCollectionActiveParams{
			TenantID: tenantID, EvidenceType: jurisdiction.EvidenceDeclaredResidence, Active: true,
			ActorType: jurisdiction.ActorStaff, ActorID: uuid.New(), ReasonCode: "declared-residence-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable declared residence evidence collection: %v", err)
	}
}

func TestSetPlayerAccountDeclaredResidence_FirstSetChangeAndResubmit(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "declared-residence-"+uuid.NewString()+"@example.com")
	enableDeclaredResidenceCollection(t, pool, tenant.ID)

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
	enableDeclaredResidenceCollection(t, pool, tenant.ID)

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
	enableDeclaredResidenceCollection(t, pool, tenant.ID)

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
	enableDeclaredResidenceCollection(t, pool, tenant.ID)

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
	enableDeclaredResidenceCollection(t, pool, tenantA.ID)

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
	enableDeclaredResidenceCollection(t, pool, tenant.ID)

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

// --- PHASE-B-ARCH-1: activation-gate enforcement now lives INSIDE
// SetPlayerAccountDeclaredResidence itself, not only in the HTTP handler.
// The tests below prove the gate is enforced at this service/domain
// boundary directly - calling the Go function with no HTTP layer involved
// at all - closing the enforcement-asymmetry finding against KYC's
// equivalent, already-structural gate. ---

// TestSetPlayerAccountDeclaredResidence_ActivationGateOff_ServiceLayerDenied
// is the single most important regression guard this hardening pass adds:
// a DIRECT service-layer call (no HTTP handler in the loop at all) must be
// refused when the tenant's declared_residence collection switch is OFF -
// proving the gate cannot be bypassed by any internal caller, present or
// future.
func TestSetPlayerAccountDeclaredResidence_ActivationGateOff_ServiceLayerDenied(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "gate-off-"+uuid.NewString()+"@example.com")
	// Deliberately NOT calling enableDeclaredResidenceCollection - default
	// OFF (fail closed, migration 0074), exactly like KYC's own gate-off test.

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "US", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if !errors.Is(err, ErrEvidenceCollectionInactive) {
		t.Fatalf("expected ErrEvidenceCollectionInactive calling the service layer directly with the gate OFF, got: %v", err)
	}

	// Nothing was written: no residence value, no audit row.
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, ok, err := GetDeclaredResidence(ctx, tx, account.ID)
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("expected no declared residence to have been written while the gate was off")
		}
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'player.declared_residence_set' AND target_id = $2`,
			tenant.ID, account.ID.String()).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected zero audit_log rows while the gate was off, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-denial check: %v", err)
	}
}

// TestSetPlayerAccountDeclaredResidence_ActivationGateOff_StaffActorAlsoDenied
// proves the gate applies to EVERY actor type, including the
// currently-unused staff-correction branch (PHASE-B-ARCH-1's architect
// ruling: the gate answers a lawful-basis question about the data, not
// about who is writing it - a future staff-correction endpoint inherits
// this, it does not get to re-open the question).
func TestSetPlayerAccountDeclaredResidence_ActivationGateOff_StaffActorAlsoDenied(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "gate-off-staff-"+uuid.NewString()+"@example.com")

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "US",
			ActorType: audit.ActorStaff, ActorID: uuid.New(), ReasonCode: "correction",
		})
		return err
	})
	if !errors.Is(err, ErrEvidenceCollectionInactive) {
		t.Fatalf("expected ErrEvidenceCollectionInactive for a staff actor with the gate off, got: %v", err)
	}
}

// TestSetPlayerAccountDeclaredResidence_ActivationToggleOffAfterOn proves a
// SECOND write is rejected once the gate is turned back off, and that the
// first, lawfully-written value is left untouched (a later gate closure is
// prospective only - it does not retroactively invalidate or erase
// already-collected data, per the architect ruling).
func TestSetPlayerAccountDeclaredResidence_ActivationToggleOffAfterOn(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "gate-toggle-"+uuid.NewString()+"@example.com")
	enableDeclaredResidenceCollection(t, pool, tenant.ID)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "PT", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("first write (gate on): unexpected error: %v", err)
	}

	// Turn the gate back OFF.
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := jurisdiction.SetEvidenceCollectionActive(ctx, tx, jurisdiction.SetEvidenceCollectionActiveParams{
			TenantID: tenant.ID, EvidenceType: jurisdiction.EvidenceDeclaredResidence, Active: false,
			ActorType: jurisdiction.ActorStaff, ActorID: uuid.New(), ReasonCode: "clearance-withdrawn",
		})
		return err
	})
	if err != nil {
		t.Fatalf("toggle gate off: unexpected error: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "ES", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if !errors.Is(err, ErrEvidenceCollectionInactive) {
		t.Fatalf("expected ErrEvidenceCollectionInactive for the second write after the gate was turned back off, got: %v", err)
	}

	// The first value is untouched.
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		country, _, ok, err := GetDeclaredResidence(ctx, tx, account.ID)
		if err != nil {
			return err
		}
		if !ok || country != "PT" {
			t.Fatalf("expected the original value PT to survive the rejected second write, got ok=%v country=%q", ok, country)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-toggle read: %v", err)
	}
}

// TestSetPlayerAccountDeclaredResidence_MisscopedTransactionErrors proves a
// player-scoped connection (db.Pool.WithPlayerScope) - a natural-looking
// but WRONG choice for this write, since jurisdiction_evidence_collection_
// active's own RLS policies are invisible to a player-scoped connection -
// is rejected with a distinct, non-policy error, never silently
// misreported as ErrEvidenceCollectionInactive. This is the exact hazard
// the architect ruling's connection-scope assertion exists to close: a
// misleading "collection is off" response while collection is in fact on.
func TestSetPlayerAccountDeclaredResidence_MisscopedTransactionErrors(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "misscoped-"+uuid.NewString()+"@example.com")
	enableDeclaredResidenceCollection(t, pool, tenant.ID)

	err := pool.WithPlayerScope(context.Background(), tenant.ID, account.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "US", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected an error on a player-scoped transaction, got nil")
	}
	if errors.Is(err, ErrEvidenceCollectionInactive) {
		t.Fatalf("a player-scoped transaction must NOT be misreported as a closed gate (collection IS active for this tenant) - got: %v", err)
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected the connection-scope assertion's own distinct error, not %v", err)
	}
}

// TestSetPlayerAccountDeclaredResidence_WrongTenantIDErrors proves a
// forged/mismatched p.TenantID (one that does not match the connection's
// actual app.tenant_id scope) is rejected by the same connection-scope
// assertion, never silently accepted or misreported as a closed gate -
// closing the exact "caller-supplied tenantID divergence" trap named in
// kyc.GetVerifiedResidence's own doc comment, applied here to the write
// side.
func TestSetPlayerAccountDeclaredResidence_WrongTenantIDErrors(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	brandA := createTestBrand(t, pool, tenantA)
	accountA := mustRegisterTestPlayer(t, pool, tenantA.ID, brandA, "wrong-tenant-"+uuid.NewString()+"@example.com")
	tenantB := createTestTenant(t, pool)
	enableDeclaredResidenceCollection(t, pool, tenantA.ID)
	enableDeclaredResidenceCollection(t, pool, tenantB.ID)

	// Connected as tenant B, but claiming (via the params struct) to be
	// tenant A.
	err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: accountA.ID, TenantID: tenantA.ID, CountryCode: "US", ActorType: audit.ActorPlayer, ActorID: accountA.ID,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected an error for a forged/mismatched TenantID, got nil")
	}
	if errors.Is(err, ErrEvidenceCollectionInactive) {
		t.Fatalf("a forged TenantID must not be misreported as a closed gate - got: %v", err)
	}
}

// TestSetPlayerAccountDeclaredResidence_CrossTenantWriteTargetDenied is the
// canonical "tenant B's valid, self-consistent token must never reach
// tenant A's data" case for the WRITE path (PHASE-B-ARCH-1 security review:
// the committed WrongTenantIDErrors test above only covers a MISMATCHED
// p.TenantID/connection-scope pair, which is caught by the scope assertion
// before ever touching player_accounts; this test is the different,
// previously-untested case where the connection scope and p.TenantID
// AGREE (both tenant B) but the targeted player_account_id belongs to
// tenant A - RLS on player_accounts itself, via the CTE's own
// "AND tenant_id = $2" predicate, must be what stops this, and it must
// leave tenant A's data and audit trail completely untouched under EITHER
// tenant's scope).
func TestSetPlayerAccountDeclaredResidence_CrossTenantWriteTargetDenied(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	brandA := createTestBrand(t, pool, tenantA)
	accountA := mustRegisterTestPlayer(t, pool, tenantA.ID, brandA, "cross-tenant-write-"+uuid.NewString()+"@example.com")
	enableDeclaredResidenceCollection(t, pool, tenantA.ID)

	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: accountA.ID, TenantID: tenantA.ID, CountryCode: "FR", ActorType: audit.ActorPlayer, ActorID: accountA.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant A's value: unexpected error: %v", err)
	}

	tenantB := createTestTenant(t, pool)
	enableDeclaredResidenceCollection(t, pool, tenantB.ID)

	// Connected as tenant B, self-consistent scope (p.TenantID = B), but
	// targeting tenant A's player_account_id.
	err = pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: accountA.ID, TenantID: tenantB.ID, CountryCode: "DE", ActorType: audit.ActorPlayer, ActorID: accountA.ID,
		})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a cross-tenant write target, got: %v", err)
	}

	// Tenant A's value is untouched.
	err = pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		country, _, ok, err := GetDeclaredResidence(ctx, tx, accountA.ID)
		if err != nil {
			return err
		}
		if !ok || country != "FR" {
			t.Fatalf("expected tenant A's value FR to survive the cross-tenant attempt, got ok=%v country=%q", ok, country)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-attempt read: %v", err)
	}

	// Zero audit rows under tenant B's scope for this action.
	err = pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'player.declared_residence_set'`,
			tenantB.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected zero audit_log rows under tenant B for the cross-tenant attempt, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit check: %v", err)
	}
}

// TestSetPlayerAccountDeclaredResidence_InvalidCountryCodeIsError proves
// the domain function itself (not merely the HTTP handler's own upstream
// validation) rejects an unassigned two-letter code that would otherwise
// satisfy the DB's shape-only `^[A-Z]{2}$` CHECK constraint
// (PHASE-B-ARCH-1 security review finding F-1).
func TestSetPlayerAccountDeclaredResidence_InvalidCountryCodeIsError(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)
	account := mustRegisterTestPlayer(t, pool, tenant.ID, brand, "invalid-country-"+uuid.NewString()+"@example.com")
	enableDeclaredResidenceCollection(t, pool, tenant.ID)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := SetPlayerAccountDeclaredResidence(ctx, tx, SetDeclaredResidenceParams{
			PlayerAccountID: account.ID, TenantID: tenant.ID, CountryCode: "ZZ", ActorType: audit.ActorPlayer, ActorID: account.ID,
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for an unassigned ISO code, got: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, ok, err := GetDeclaredResidence(ctx, tx, account.ID)
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("expected nothing to have been written for an invalid country code")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-rejection read: %v", err)
	}
}

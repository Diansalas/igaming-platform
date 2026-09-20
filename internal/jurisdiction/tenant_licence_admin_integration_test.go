//go:build integration

// Stage 4I Phase A: real-Postgres tests for AssignTenantLicence, the
// write path that closes the gap resolver.go's own header comment names
// (tenants.licence_id had no application writer at all). Follows
// jurisdiction_integration_test.go's fixture conventions exactly -
// seedFixture already gives us a tenant WITH a licence assigned
// (f.tenantID/f.licenceID) and a tenant WITHOUT one (f.otherTenantID), a
// second jurisdiction (f.jurisdiction2) to hang additional licences off,
// and a platform-admin principal (f.platformAdmin).
//
// STAGE 4I PHASE E-SECURITY (migration 0077) CHANGED EVERY TEST IN THIS
// FILE: AssignTenantLicence's contract moved from db.Pool.WithTenant to
// db.Pool.WithPlatformAdmin (tenant_licence_admin.go's own header comment
// explains why - `tenants`/`licences` gained RLS with no tenant-scoped
// write policy of any kind), and its audit row moved from tenant-scoped
// (tenant_id = the affected tenant) to platform-scoped (tenant_id IS
// NULL). Every call site below was updated accordingly, and the audit
// read-back helpers now query `tenant_id IS NULL` under
// db.Pool.WithPlatformAdmin (or WithoutTenant - audit_log's own
// dual_scope_isolation policy keys only on app.tenant_id, which both
// leave unset identically) rather than db.Pool.WithTenant.
package jurisdiction

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// waitForBlockedStatement polls pg_stat_activity - fully deterministic, no
// timing assumption - for a backend genuinely blocked (wait_event_type =
// 'Lock') on a statement matching queryFragment. Mirrors
// internal/operatingmarket/concurrency_integration_test.go's own helper of
// the same name exactly (this package's MANDATORY, non-negotiable
// concurrency-test technique - see that file's header comment: NEVER a
// bare sync.WaitGroup barrier, which cannot prove concurrent execution
// actually occurred).
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

func mustCreatePlatformLicence(t *testing.T, pool *db.Pool, platformAdmin, jurisdictionID uuid.UUID, licensee string) Licence {
	t.Helper()
	var l Licence
	err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		l, err = CreateLicence(ctx, tx, CreateLicenceParams{
			JurisdictionID: jurisdictionID, Licensee: licensee, LicenceNumber: "TL-" + uuid.NewString()[:8],
			Actor: ActorContext{ActorID: platformAdmin, ReasonCode: "test-setup"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("create licence fixture: %v", err)
	}
	return l
}

// insertNonActiveLicence bypasses CreateLicence (which always creates
// 'active' rows - see registry_admin.go's own doc comment) via a direct
// SQL insert, matching how other edge-case fixtures in this package are
// constructed. Stage 4I Phase E-SECURITY (migration 0077): `licences`
// writes now require a genuinely platform-admin-scoped transaction.
func insertNonActiveLicence(t *testing.T, pool *db.Pool, jurisdictionID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, 'platform', $3, $4)`,
			id, jurisdictionID, "TL-"+id.String()[:8], status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 licence row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert non-active licence fixture: %v", err)
	}
	return id
}

// tenantLicenceID reads tenants.licence_id. `tenants_read` is USING(true)
// (migration 0077), so this read still works identically from a
// WithoutTenant connection - unchanged.
func tenantLicenceID(t *testing.T, pool *db.Pool, tenantID uuid.UUID) *uuid.UUID {
	t.Helper()
	var licenceID *uuid.UUID
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, tenantID).Scan(&licenceID)
	})
	if err != nil {
		t.Fatalf("read tenant licence_id: %v", err)
	}
	return licenceID
}

// countTenantLicenceAuditRows counts targetTenantID's own
// tenant_licence_assigned audit rows. Stage 4I Phase E-SECURITY (migration
// 0077): AssignTenantLicence's audit write moved to PLATFORM scope
// (tenant_id IS NULL) - the read-back therefore queries tenant_id IS NULL
// (not tenant_id = targetTenantID) and runs under db.Pool.WithPlatformAdmin,
// which satisfies audit_log's dual_scope_isolation policy identically to
// WithoutTenant (that policy keys only on app.tenant_id, never on
// app.platform_admin_principal_id).
func countTenantLicenceAuditRows(t *testing.T, pool *db.Pool, targetTenantID uuid.UUID) int {
	t.Helper()
	var count int
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id IS NULL AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $1`,
			targetTenantID.String()).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return count
}

func TestAssignTenantLicence_ValidActiveMatchingLicensee_Succeeds(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	licence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")

	// AssignTenantLicence now requires a PLATFORM-scoped transaction
	// (Stage 4I Phase E-SECURITY, migration 0077) - see this file's own
	// header comment and tenant_licence_admin.go's.
	var state TenantLicenceState
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		state, err = AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.otherTenantID, LicenceID: &licence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "onboarding"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("AssignTenantLicence: %v", err)
	}
	if state.TenantID != f.otherTenantID {
		t.Fatalf("expected tenant id %s, got %s", f.otherTenantID, state.TenantID)
	}
	if state.LicenceID == nil || *state.LicenceID != licence.ID {
		t.Fatalf("expected licence id %s, got %v", licence.ID, state.LicenceID)
	}

	got := tenantLicenceID(t, pool, f.otherTenantID)
	if got == nil || *got != licence.ID {
		t.Fatalf("expected tenants.licence_id = %s, got %v", licence.ID, got)
	}

	if count := countTenantLicenceAuditRows(t, pool, f.otherTenantID); count != 1 {
		t.Fatalf("expected exactly 1 audit row, got %d", count)
	}

	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var tenantIDCol *uuid.UUID
		var beforeLicenceID, afterLicenceID *string
		var jurisdictionCode *string
		var reasonCode string
		if err := tx.QueryRow(ctx, `
			SELECT tenant_id, metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id',
			       metadata ->> 'licence_jurisdiction_code', metadata ->> 'reason_code'
			  FROM audit_log
			 WHERE tenant_id IS NULL AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $1`,
			f.otherTenantID.String()).Scan(&tenantIDCol, &beforeLicenceID, &afterLicenceID, &jurisdictionCode, &reasonCode); err != nil {
			return err
		}
		// Pins the Stage 4I Phase E-SECURITY fix directly: this row's
		// tenant_id column is now NULL (platform-scoped), not the affected
		// tenant.
		if tenantIDCol != nil {
			t.Fatalf("expected audit row tenant_id to be NULL (platform-scoped), got %s", *tenantIDCol)
		}
		if beforeLicenceID != nil {
			t.Fatalf("expected before.licence_id to be nil (tenant had none), got %v", *beforeLicenceID)
		}
		if afterLicenceID == nil || *afterLicenceID != licence.ID.String() {
			t.Fatalf("expected after.licence_id %s, got %v", licence.ID, afterLicenceID)
		}
		wantCode := "TJ-" + f.jurisdiction2.String()[:8]
		if jurisdictionCode == nil || *jurisdictionCode != wantCode {
			t.Fatalf("expected licence_jurisdiction_code %q, got %v", wantCode, jurisdictionCode)
		}
		if reasonCode != "onboarding" {
			t.Fatalf("expected reason_code 'onboarding', got %q", reasonCode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit metadata check: %v", err)
	}
}

// TestAssignTenantLicence_BYOLOwnLicenceMatchingTenantLicensee_Succeeds
// mirrors TestAssignTenantLicence_ValidActiveMatchingLicensee_Succeeds
// exactly, but flipped to the hybrid licensing model's OTHER arm (ADR
// 0006): a tenant provisioned 'own_licence' (BYOL) being bound to a
// 'tenant'-licensee licence. Every other success-path test in this file
// only exercises 'under_platform_licence' + licensee 'platform' - this is
// the one that proves AssignTenantLicence is genuinely generic across the
// hybrid model, not accidentally coupled to the platform-licensee case.
func TestAssignTenantLicence_BYOLOwnLicenceMatchingTenantLicensee_Succeeds(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	byolTenantID := uuid.New()
	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'BYOL Test Tenant', 'own_licence')`,
			byolTenantID, "byol-"+byolTenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed BYOL tenant fixture: %v", err)
	}

	licence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "tenant")

	var state TenantLicenceState
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		state, err = AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: byolTenantID, LicenceID: &licence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "byol-onboarding"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("AssignTenantLicence (BYOL): %v", err)
	}
	if state.TenantID != byolTenantID {
		t.Fatalf("expected tenant id %s, got %s", byolTenantID, state.TenantID)
	}
	if state.LicenceID == nil || *state.LicenceID != licence.ID {
		t.Fatalf("expected licence id %s, got %v", licence.ID, state.LicenceID)
	}

	got := tenantLicenceID(t, pool, byolTenantID)
	if got == nil || *got != licence.ID {
		t.Fatalf("expected tenants.licence_id = %s, got %v", licence.ID, got)
	}

	if count := countTenantLicenceAuditRows(t, pool, byolTenantID); count != 1 {
		t.Fatalf("expected exactly 1 audit row, got %d", count)
	}

	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var tenantIDCol *uuid.UUID
		var beforeLicenceID, afterLicenceID *string
		var jurisdictionCode *string
		var reasonCode string
		if err := tx.QueryRow(ctx, `
			SELECT tenant_id, metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id',
			       metadata ->> 'licence_jurisdiction_code', metadata ->> 'reason_code'
			  FROM audit_log
			 WHERE tenant_id IS NULL AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $1`,
			byolTenantID.String()).Scan(&tenantIDCol, &beforeLicenceID, &afterLicenceID, &jurisdictionCode, &reasonCode); err != nil {
			return err
		}
		if tenantIDCol != nil {
			t.Fatalf("expected audit row tenant_id to be NULL (platform-scoped), got %s", *tenantIDCol)
		}
		if beforeLicenceID != nil {
			t.Fatalf("expected before.licence_id to be nil (tenant had none), got %v", *beforeLicenceID)
		}
		if afterLicenceID == nil || *afterLicenceID != licence.ID.String() {
			t.Fatalf("expected after.licence_id %s, got %v", licence.ID, afterLicenceID)
		}
		wantCode := "TJ-" + f.jurisdiction2.String()[:8]
		if jurisdictionCode == nil || *jurisdictionCode != wantCode {
			t.Fatalf("expected licence_jurisdiction_code %q, got %v", wantCode, jurisdictionCode)
		}
		if reasonCode != "byol-onboarding" {
			t.Fatalf("expected reason_code 'byol-onboarding', got %q", reasonCode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit metadata check: %v", err)
	}
}

func TestAssignTenantLicence_LicenseeMismatch_RejectedNoMutationNoAudit(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// f.otherTenantID's licensing_model is 'under_platform_licence'
	// (expected_licensee = 'platform') - a 'tenant' licensee licence must
	// be rejected by the composite FK.
	mismatched := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "tenant")

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.otherTenantID, LicenceID: &mismatched.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "attempt"},
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a licensee/licensing_model mismatch, got %v", err)
	}

	if got := tenantLicenceID(t, pool, f.otherTenantID); got != nil {
		t.Fatalf("expected tenants.licence_id to remain NULL after a rejected assignment, got %v", got)
	}
	if count := countTenantLicenceAuditRows(t, pool, f.otherTenantID); count != 0 {
		t.Fatalf("expected NO audit row for a rejected assignment (transaction must roll back), got %d", count)
	}
}

func TestAssignTenantLicence_UnknownLicenceID_Rejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	unknown := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.otherTenantID, LicenceID: &unknown,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "attempt"},
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for an unknown licence_id, got %v", err)
	}
}

func TestAssignTenantLicence_NonActiveLicence_Rejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	for _, status := range []string{"suspended", "expired"} {
		status := status
		t.Run(status, func(t *testing.T) {
			licenceID := insertNonActiveLicence(t, pool, f.jurisdiction2, status)
			err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
				_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
					TenantID: f.otherTenantID, LicenceID: &licenceID,
					Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "attempt"},
				})
				return err
			})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput for a %s licence, got %v", status, err)
			}
		})
	}
}

func TestAssignTenantLicence_UnknownTenantID_ReturnsErrNotFound(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	licence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")
	unknownTenant := uuid.New()

	// unknownTenant doesn't need to exist as a real tenants row.
	// AssignTenantLicence's own UPDATE ... WHERE tenants.id = before.id
	// then correctly returns no rows, mapped to ErrNotFound, before any
	// audit write is attempted.
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: unknownTenant, LicenceID: &licence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "attempt"},
		})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown tenant_id, got %v", err)
	}
}

func TestAssignTenantLicence_Unassign_Succeeds(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// f.tenantID already has f.licenceID assigned by seedFixture.
	var state TenantLicenceState
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		state, err = AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.tenantID, LicenceID: nil,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "offboarding"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("AssignTenantLicence (unassign): %v", err)
	}
	if state.LicenceID != nil {
		t.Fatalf("expected LicenceID nil after unassign, got %v", state.LicenceID)
	}
	if got := tenantLicenceID(t, pool, f.tenantID); got != nil {
		t.Fatalf("expected tenants.licence_id NULL after unassign, got %v", got)
	}

	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var beforeLicenceID, afterLicenceID *string
		if err := tx.QueryRow(ctx, `
			SELECT metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id'
			  FROM audit_log
			 WHERE tenant_id IS NULL AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $1`,
			f.tenantID.String()).Scan(&beforeLicenceID, &afterLicenceID); err != nil {
			return err
		}
		if beforeLicenceID == nil || *beforeLicenceID != f.licenceID.String() {
			t.Fatalf("expected before.licence_id %s, got %v", f.licenceID, beforeLicenceID)
		}
		if afterLicenceID != nil {
			t.Fatalf("expected after.licence_id nil, got %v", *afterLicenceID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit metadata check: %v", err)
	}
}

func TestAssignTenantLicence_ReassignSameLicence_NoOpStillAudited(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// f.tenantID already has f.licenceID assigned - re-assign the same one.
	var state TenantLicenceState
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		state, err = AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.tenantID, LicenceID: &f.licenceID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "reconfirm"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("AssignTenantLicence (no-op reassign): %v", err)
	}
	if state.LicenceID == nil || *state.LicenceID != f.licenceID {
		t.Fatalf("expected licence id %s, got %v", f.licenceID, state.LicenceID)
	}

	if count := countTenantLicenceAuditRows(t, pool, f.tenantID); count != 1 {
		t.Fatalf("expected exactly 1 audit row even for a no-op reassignment, got %d", count)
	}

	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var beforeLicenceID, afterLicenceID *string
		if err := tx.QueryRow(ctx, `
			SELECT metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id'
			  FROM audit_log
			 WHERE tenant_id IS NULL AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $1`,
			f.tenantID.String()).Scan(&beforeLicenceID, &afterLicenceID); err != nil {
			return err
		}
		if beforeLicenceID == nil || afterLicenceID == nil || *beforeLicenceID != *afterLicenceID {
			t.Fatalf("expected before == after for a no-op reassignment, got before=%v after=%v", beforeLicenceID, afterLicenceID)
		}
		if *beforeLicenceID != f.licenceID.String() {
			t.Fatalf("expected before/after licence_id %s, got %s", f.licenceID, *beforeLicenceID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit metadata check: %v", err)
	}
}

func TestAssignTenantLicence_MissingReasonCode_Errors(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	licence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.otherTenantID, LicenceID: &licence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin},
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a missing reason_code, got %v", err)
	}
}

// TestAssignTenantLicence_TenantScopedTransactionIsRejected is the new
// Stage 4I Phase E-SECURITY regression: a tenant-scoped transaction
// (db.Pool.WithTenant) - the OLD, pre-migration-0077 contract this
// function used to require - must now be refused by assertPlatformScope
// as this function's first statement, with NO mutation and NO audit
// record, distinctly from every other ErrInvalidInput/ErrNotFound this
// file exercises.
func TestAssignTenantLicence_TenantScopedTransactionIsRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	licence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")

	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.otherTenantID, LicenceID: &licence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "attack-attempt"},
		})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope for a tenant-scoped transaction, got %v", err)
	}

	if got := tenantLicenceID(t, pool, f.otherTenantID); got != nil {
		t.Fatalf("expected tenants.licence_id to remain NULL after a rejected tenant-scoped attempt, got %v", got)
	}
	if count := countTenantLicenceAuditRows(t, pool, f.otherTenantID); count != 0 {
		t.Fatalf("expected NO audit row for a rejected tenant-scoped attempt, got %d", count)
	}

	// A scopeless (WithoutTenant) transaction must be refused identically.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.otherTenantID, LicenceID: &licence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "attack-attempt-2"},
		})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope for a scopeless transaction, got %v", err)
	}
}

// auditChainRow is one tenant_licence_assigned audit row's before/after
// licence_id pair, as read back by fetchTenantLicenceAuditChain.
type auditChainRow struct {
	before *string
	after  *string
}

// fetchTenantLicenceAuditChain reads back every tenant_licence_assigned
// audit row for targetTenantID, ordered by created_at (i.e. real
// commit/insert order) - used to prove the concurrency test's actual
// before/after CHAIN, not just that two rows exist. Stage 4I Phase
// E-SECURITY (migration 0077): queries tenant_id IS NULL (platform-scoped
// audit row) under db.Pool.WithPlatformAdmin, not tenant_id = targetTenantID
// under db.Pool.WithTenant.
func fetchTenantLicenceAuditChain(t *testing.T, pool *db.Pool, targetTenantID uuid.UUID) []auditChainRow {
	t.Helper()
	var rows []auditChainRow
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		r, err := tx.Query(ctx, `
			SELECT metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id'
			  FROM audit_log
			 WHERE tenant_id IS NULL AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $1
			 ORDER BY created_at ASC`,
			targetTenantID.String())
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var row auditChainRow
			if err := r.Scan(&row.before, &row.after); err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("fetch tenant licence audit chain: %v", err)
	}
	return rows
}

// TestAssignTenantLicence_ConcurrentAssignmentsSerializeCleanly proves the
// FOR UPDATE lock on the tenants row (this file's own atomic
// WITH before AS (... FOR UPDATE) UPDATE statement) makes two concurrent
// assignments of DIFFERENT valid, licensee-matching licences to the SAME
// tenant serialize cleanly rather than deadlock or corrupt the row: both
// are individually valid operations, so BOTH are expected to succeed, just
// not concurrently.
//
// REWRITTEN (Stage 4I Phase E-SECURITY fix round, Fix 7 - DB/RLS review):
// the prior version of this test launched two goroutines with a bare
// sync.WaitGroup and no real synchronization barrier - internal/
// operatingmarket/concurrency_integration_test.go's own header comment
// calls this exact anti-pattern "MANDATORY, non-negotiable... NEVER a
// sync.WaitGroup barrier... this exact mistake cost Stage 4I Phase D two
// fix rounds". It could not reliably prove concurrent execution actually
// occurred (the two goroutines could run fully sequentially and the test
// would still pass), and its chain-ordering assertion sorted audit rows by
// audit_log.created_at (transaction START time), which does not reflect
// actual commit order under contention - a transaction that begins later
// can still win the FOR UPDATE lock race and commit first (QA's review
// reproduced this test failing ~25% of the time under artificial CPU
// contention for exactly this reason).
//
// This rewrite uses the deterministic uncommitted-competing-row technique
// (operatingmarket/concurrency_integration_test.go's own MANDATORY
// pattern): a "blocker" goroutine runs a genuine AssignTenantLicence call
// (licenceA) inside a transaction held open past its own commit point via
// a channel barrier; the second, "real" call (licenceB) is only started
// once the blocker has SUCCEEDED (proving its own write happened), and the
// test polls pg_stat_activity for the real call's UPDATE genuinely BLOCKED
// on a lock before releasing the blocker to commit. This makes the
// before/after chain unambiguous BY CONSTRUCTION - the blocker
// deterministically commits first - rather than by wall-clock inference
// from created_at, and the audit chain assertions below identify each row
// by its own after.licence_id value (not by created_at sort order).
func TestAssignTenantLicence_ConcurrentAssignmentsSerializeCleanly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	licenceA := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdictionID, "platform")
	licenceB := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")

	blockerReady := make(chan struct{})
	proceedToCommit := make(chan struct{})
	blockerErrCh := make(chan error, 1)

	go func() {
		blockerErrCh <- pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
				TenantID: f.otherTenantID, LicenceID: &licenceA.ID,
				Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "concurrent-test-blocker"},
			})
			if err != nil {
				return err
			}
			close(blockerReady)
			<-proceedToCommit
			return nil
		})
	}()

	<-blockerReady

	secondCallResult := make(chan error, 1)
	go func() {
		secondCallResult <- pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
				TenantID: f.otherTenantID, LicenceID: &licenceB.ID,
				Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "concurrent-test-second-call"},
			})
			return err
		})
	}()

	blocked := waitForBlockedStatement(t, pool, "UPDATE tenants SET licence_id")
	if !blocked {
		close(proceedToCommit)
		t.Fatal("timed out waiting for the second call's UPDATE to genuinely block on the blocker's uncommitted FOR UPDATE lock (pg_stat_activity never reported a Lock wait)")
	}

	close(proceedToCommit)

	if err := <-blockerErrCh; err != nil {
		t.Fatalf("blocker (first-committed) assignment: expected nil error, got %v", err)
	}
	if err := <-secondCallResult; err != nil {
		t.Fatalf("second (deterministically second-committed) assignment: expected nil error (both are individually valid, non-conflicting operations), got %v", err)
	}

	// Deterministic BY CONSTRUCTION, not inferred from created_at: the
	// blocker's own AssignTenantLicence call already returned success
	// (and closed blockerReady) before the second call was even started,
	// and the second call only proceeded past its lock wait once the
	// blocker committed - so the final licence_id must be licenceB.
	final := tenantLicenceID(t, pool, f.otherTenantID)
	if final == nil || *final != licenceB.ID {
		t.Fatalf("expected the final licence_id to be licenceB (%s), the deterministically second-committed call, got %v", licenceB.ID, final)
	}

	// Exactly two audit rows (one per call, both committed).
	if count := countTenantLicenceAuditRows(t, pool, f.otherTenantID); count != 2 {
		t.Fatalf("expected exactly 2 audit rows (one per successful call), got %d", count)
	}

	// Identify each audit row by its OWN after.licence_id value - never by
	// created_at sort order (Fix 7(b): created_at reflects transaction
	// START time, not commit order, and must never be relied upon to
	// determine which transaction "won").
	chain := fetchTenantLicenceAuditChain(t, pool, f.otherTenantID)
	if len(chain) != 2 {
		t.Fatalf("expected exactly 2 audit rows in the chain, got %d", len(chain))
	}
	var rowA, rowB *auditChainRow
	for i := range chain {
		switch {
		case chain[i].after != nil && *chain[i].after == licenceA.ID.String():
			rowA = &chain[i]
		case chain[i].after != nil && *chain[i].after == licenceB.ID.String():
			rowB = &chain[i]
		}
	}
	if rowA == nil {
		t.Fatalf("expected an audit row with after.licence_id = licenceA (%s), got %+v", licenceA.ID, chain)
	}
	if rowB == nil {
		t.Fatalf("expected an audit row with after.licence_id = licenceB (%s), got %+v", licenceB.ID, chain)
	}
	// The blocker (licenceA) committed first and started from an
	// unassigned tenant.
	if rowA.before != nil {
		t.Fatalf("expected the blocker's (licenceA) before.licence_id to be nil (tenant started unassigned), got %v", *rowA.before)
	}
	// The second call (licenceB), which by construction committed only
	// after the blocker, must have seen the blocker's own committed result
	// as its "before" state - not a stale pre-transaction read.
	if rowB.before == nil || *rowB.before != licenceA.ID.String() {
		t.Fatalf("expected the second call's (licenceB) before.licence_id to equal the blocker's after.licence_id (%s) - a stale read would break this chain, got %v", licenceA.ID, rowB.before)
	}
}

// TestAssignTenantLicence_ConcurrentExclusiveBYOLBindToTwoDistinctTenantsIsSerialized
// is Fix 8 (Stage 4I Phase E-SECURITY fix round, DB/RLS review): the
// uq_tenants_exclusive_own_licence partial unique index (migration 0077)
// introduces a genuinely NEW concurrency scenario that did not exist
// before that migration - two concurrent AssignTenantLicence calls
// attempting to bind the SAME BYOL (licensee='tenant') licence to two
// DIFFERENT tenants. AssignTenantLicence's existing `FOR SHARE OF l` lock
// does NOT serialize this (SHARE locks are mutually compatible, and the
// two tenant rows being updated are distinct rows) - the unique index
// itself is the only thing preventing a double-bind, and there was no
// test proving this before this fix.
//
// Uses the same deterministic uncommitted-competing-row technique as the
// test above: the blocker's own uncommitted UPDATE of tenantX's licence_id
// to the shared BYOL licence holds the relevant unique-index entry
// provisionally, so the second call's UPDATE of tenantY to the SAME
// licence_id genuinely BLOCKS (confirmed via pg_stat_activity) until the
// blocker resolves, then fails with a REAL 23505 unique violation, mapped
// by Fix 1 to ErrInvalidInput (never a raw *pgconn.PgError).
func TestAssignTenantLicence_ConcurrentExclusiveBYOLBindToTwoDistinctTenantsIsSerialized(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	byolLicence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "tenant")

	tenantX := uuid.New()
	tenantY := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		for id, name := range map[uuid.UUID]string{tenantX: "Concurrent BYOL X", tenantY: "Concurrent BYOL Y"} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, $3, 'own_licence')`,
				id, "conc-byol-"+id.String()[:8], name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed two BYOL tenants: %v", err)
	}

	blockerReady := make(chan struct{})
	proceedToCommit := make(chan struct{})
	blockerErrCh := make(chan error, 1)

	go func() {
		blockerErrCh <- pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
				TenantID: tenantX, LicenceID: &byolLicence.ID,
				Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "concurrent-byol-blocker"},
			})
			if err != nil {
				return err
			}
			close(blockerReady)
			<-proceedToCommit
			return nil
		})
	}()

	<-blockerReady

	secondCallResult := make(chan error, 1)
	go func() {
		secondCallResult <- pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
				TenantID: tenantY, LicenceID: &byolLicence.ID,
				Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "concurrent-byol-second"},
			})
			return err
		})
	}()

	blocked := waitForBlockedStatement(t, pool, "UPDATE tenants SET licence_id")
	if !blocked {
		close(proceedToCommit)
		t.Fatal("timed out waiting for the second tenant's bind to genuinely block on the blocker's uncommitted exclusive-licence bind (pg_stat_activity never reported a Lock wait) - this would mean uq_tenants_exclusive_own_licence provides no real serialization")
	}

	close(proceedToCommit)

	if err := <-blockerErrCh; err != nil {
		t.Fatalf("blocker (tenantX) BYOL bind: expected nil error, got %v", err)
	}
	secondErr := <-secondCallResult
	if secondErr == nil {
		t.Fatal("expected the second (tenantY) BYOL bind of the SAME exclusive licence to be refused")
	}
	if !errors.Is(secondErr, ErrInvalidInput) {
		t.Fatalf("expected the second call's error to be mapped to ErrInvalidInput (Fix 1), got %T: %v", secondErr, secondErr)
	}
	var pgErr *pgconn.PgError
	if errors.As(secondErr, &pgErr) {
		t.Fatalf("expected the raw *pgconn.PgError (23505) to be mapped away by Fix 1, but it still escaped: %v", secondErr)
	}

	// Exactly one tenant ends up bound to the licence.
	if got := tenantLicenceID(t, pool, tenantX); got == nil || *got != byolLicence.ID {
		t.Fatalf("expected tenantX to hold the BYOL licence %s, got %v", byolLicence.ID, got)
	}
	if got := tenantLicenceID(t, pool, tenantY); got != nil {
		t.Fatalf("expected tenantY's licence_id to remain NULL after the refused concurrent bind, got %v", got)
	}
}

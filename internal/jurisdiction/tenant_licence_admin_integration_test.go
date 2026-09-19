//go:build integration

// Stage 4I Phase A: real-Postgres tests for AssignTenantLicence, the
// write path that closes the gap resolver.go's own header comment names
// (tenants.licence_id had no application writer at all). Follows
// jurisdiction_integration_test.go's fixture conventions exactly -
// seedFixture already gives us a tenant WITH a licence assigned
// (f.tenantID/f.licenceID) and a tenant WITHOUT one (f.otherTenantID), a
// second jurisdiction (f.jurisdiction2) to hang additional licences off,
// and a platform-admin principal (f.platformAdmin).
package jurisdiction

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

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
// constructed.
func insertNonActiveLicence(t *testing.T, pool *db.Pool, jurisdictionID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, 'platform', $3, $4)`,
			id, jurisdictionID, "TL-"+id.String()[:8], status)
		return err
	})
	if err != nil {
		t.Fatalf("insert non-active licence fixture: %v", err)
	}
	return id
}

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

// countTenantLicenceAuditRows counts this tenant's own
// tenant_licence_assigned audit rows. The write is TENANT-scoped (fix for
// the security finding that a platform-wide, tenant_id-NULL audit row for
// a tenant-specific mutation is structurally unreadable by that tenant's
// own PermAuditRead), so the read-back must run under
// db.Pool.WithTenant(tenantID, ...) - a WithoutTenant/WithPlatformAdmin
// connection satisfies neither arm of audit_log's dual-scope RLS policy
// for a non-NULL tenant_id row and would always see zero rows regardless
// of what the table holds.
func countTenantLicenceAuditRows(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $2`,
			tenantID, tenantID.String()).Scan(&count)
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

	// AssignTenantLicence's audit write is TENANT-scoped to p.TenantID
	// (f.otherTenantID here), so the transaction it runs in must be
	// db.Pool.WithTenant(f.otherTenantID, ...), not WithPlatformAdmin -
	// see this file's own header comment and tenant_licence_admin.go's.
	var state TenantLicenceState
	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
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

	err = pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		var tenantIDCol uuid.UUID
		var beforeLicenceID, afterLicenceID *string
		var jurisdictionCode *string
		var reasonCode string
		if err := tx.QueryRow(ctx, `
			SELECT tenant_id, metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id',
			       metadata ->> 'licence_jurisdiction_code', metadata ->> 'reason_code'
			  FROM audit_log
			 WHERE tenant_id = $1 AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $2`,
			f.otherTenantID, f.otherTenantID.String()).Scan(&tenantIDCol, &beforeLicenceID, &afterLicenceID, &jurisdictionCode, &reasonCode); err != nil {
			return err
		}
		// Pins the security fix directly: this row's tenant_id column is
		// the affected tenant, not NULL/platform-scoped.
		if tenantIDCol != f.otherTenantID {
			t.Fatalf("expected audit row tenant_id %s, got %s", f.otherTenantID, tenantIDCol)
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
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'BYOL Test Tenant', 'own_licence')`,
			byolTenantID, "byol-"+byolTenantID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed BYOL tenant fixture: %v", err)
	}

	licence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "tenant")

	var state TenantLicenceState
	err = pool.WithTenant(context.Background(), byolTenantID, func(ctx context.Context, tx pgx.Tx) error {
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

	err = pool.WithTenant(context.Background(), byolTenantID, func(ctx context.Context, tx pgx.Tx) error {
		var tenantIDCol uuid.UUID
		var beforeLicenceID, afterLicenceID *string
		var jurisdictionCode *string
		var reasonCode string
		if err := tx.QueryRow(ctx, `
			SELECT tenant_id, metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id',
			       metadata ->> 'licence_jurisdiction_code', metadata ->> 'reason_code'
			  FROM audit_log
			 WHERE tenant_id = $1 AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $2`,
			byolTenantID, byolTenantID.String()).Scan(&tenantIDCol, &beforeLicenceID, &afterLicenceID, &jurisdictionCode, &reasonCode); err != nil {
			return err
		}
		if tenantIDCol != byolTenantID {
			t.Fatalf("expected audit row tenant_id %s, got %s", byolTenantID, tenantIDCol)
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

	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
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
	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
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
			err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
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

	// unknownTenant doesn't need to exist as a real tenants row for
	// WithTenant to accept it - it only sets a GUC. AssignTenantLicence's
	// own UPDATE ... WHERE tenants.id = before.id then correctly returns
	// no rows, mapped to ErrNotFound, before any audit write is attempted.
	err := pool.WithTenant(context.Background(), unknownTenant, func(ctx context.Context, tx pgx.Tx) error {
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
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
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

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var beforeLicenceID, afterLicenceID *string
		if err := tx.QueryRow(ctx, `
			SELECT metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id'
			  FROM audit_log
			 WHERE tenant_id = $1 AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $2`,
			f.tenantID, f.tenantID.String()).Scan(&beforeLicenceID, &afterLicenceID); err != nil {
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
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
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

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var beforeLicenceID, afterLicenceID *string
		if err := tx.QueryRow(ctx, `
			SELECT metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id'
			  FROM audit_log
			 WHERE tenant_id = $1 AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $2`,
			f.tenantID, f.tenantID.String()).Scan(&beforeLicenceID, &afterLicenceID); err != nil {
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
	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
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

// auditChainRow is one tenant_licence_assigned audit row's before/after
// licence_id pair, as read back by fetchTenantLicenceAuditChain.
type auditChainRow struct {
	before *string
	after  *string
}

// fetchTenantLicenceAuditChain reads back every tenant_licence_assigned
// audit row for tenantID, ordered by created_at (i.e. real commit/insert
// order) - used to prove the concurrency test's actual before/after
// CHAIN, not just that two rows exist.
func fetchTenantLicenceAuditChain(t *testing.T, pool *db.Pool, tenantID uuid.UUID) []auditChainRow {
	t.Helper()
	var rows []auditChainRow
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := tx.Query(ctx, `
			SELECT metadata -> 'before' ->> 'licence_id', metadata -> 'after' ->> 'licence_id'
			  FROM audit_log
			 WHERE tenant_id = $1 AND action = 'jurisdiction_registry.tenant_licence_assigned' AND target_id = $2
			 ORDER BY created_at ASC`,
			tenantID, tenantID.String())
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
// tenant serialize cleanly rather than deadlock or corrupt the row: one
// of the two must win, the other must simply run after it (both are
// individually valid operations, so BOTH are expected to succeed, just
// not concurrently) and the tenant ends up holding exactly one licence
// id, which must be one of the two submitted.
//
// Beyond that, it proves the actual anti-stale-before-state property the
// FOR UPDATE lock exists for: the two audit rows, ordered by real
// insert/commit order (created_at), form a genuine before/after CHAIN -
// whichever call actually committed second saw the first call's own
// result as its "before" state, not a stale pre-transaction read. This
// would fail if the FOR UPDATE lock were removed (a stale read could then
// report "before: nil" for both concurrent calls even though one had
// already committed) even though the weaker "both succeeded, 2 audit
// rows exist" assertions above would still pass. The assertion is
// deliberately order-independent - it reads whichever row actually
// landed first per created_at, not a fixed goroutine index, since either
// goroutine can genuinely win the race.
func TestAssignTenantLicence_ConcurrentAssignmentsSerializeCleanly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	licenceA := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdictionID, "platform")
	licenceB := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make([]error, 2)

	run := func(idx int, licenceID uuid.UUID) {
		defer wg.Done()
		errs[idx] = pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
				TenantID: f.otherTenantID, LicenceID: &licenceID,
				Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "concurrent-test"},
			})
			return err
		})
	}
	go run(0, licenceA.ID)
	go run(1, licenceB.ID)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent assignment %d failed (expected clean serialization, not a deadlock or error): %v", i, err)
		}
	}

	final := tenantLicenceID(t, pool, f.otherTenantID)
	if final == nil {
		t.Fatal("expected the tenant to end up with a non-nil licence_id")
	}
	if *final != licenceA.ID && *final != licenceB.ID {
		t.Fatalf("expected the final licence_id to be one of the two submitted, got %v", *final)
	}

	// Exactly two audit rows (one per call, both committed since both
	// calls individually succeeded), no partial/corrupted write.
	if count := countTenantLicenceAuditRows(t, pool, f.otherTenantID); count != 2 {
		t.Fatalf("expected exactly 2 audit rows (one per successful concurrent call), got %d", count)
	}

	chain := fetchTenantLicenceAuditChain(t, pool, f.otherTenantID)
	if len(chain) != 2 {
		t.Fatalf("expected exactly 2 audit rows in the chain, got %d", len(chain))
	}
	if chain[0].before != nil {
		t.Fatalf("expected the first-committed assignment's before.licence_id to be nil (tenant started unassigned), got %v", *chain[0].before)
	}
	if chain[0].after == nil {
		t.Fatal("expected the first-committed assignment's after.licence_id to be set")
	}
	if chain[1].before == nil || *chain[1].before != *chain[0].after {
		t.Fatalf("expected the second-committed assignment's before.licence_id (%v) to equal the first-committed assignment's after.licence_id (%v) - a stale read would break this chain", chain[1].before, chain[0].after)
	}
	if chain[1].after == nil {
		t.Fatal("expected the second-committed assignment's after.licence_id to be set")
	}
	// The two after-values must be exactly the two submitted licences, in
	// whichever order they actually committed.
	gotAfter := map[string]bool{*chain[0].after: true, *chain[1].after: true}
	if !gotAfter[licenceA.ID.String()] || !gotAfter[licenceB.ID.String()] {
		t.Fatalf("expected the two audit rows' after.licence_id values to be exactly {%s, %s}, got %v", licenceA.ID, licenceB.ID, gotAfter)
	}
}

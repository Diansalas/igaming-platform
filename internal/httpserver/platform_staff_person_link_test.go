//go:build integration

// Stage 4H-B0-R6 fix dispatch, fix 1 (P1, launch-blocking): before this
// fix, no code path anywhere in the platform could ever set person_id on
// a platform_admin (tenant_id IS NULL) staff account - cmd/seed-admin
// always created one with person_id = nil, and the Stage 3D person-link
// remediation route (newLinkStaffPersonHandler) runs under
// db.WithTenant(targetTenantID, ...), which staff_users' own dual_scope_
// isolation policy (migration 0011) makes structurally blind to
// tenant_id IS NULL rows. These tests exercise the new platform-scoped
// counterpart (newLinkPlatformStaffPersonHandler) end to end (HTTP ->
// service -> RLS), mirroring stage3d_withdrawal_governance_test.go's own
// remediation-path test for the tenant-scoped route.
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// TestPlatformStaffPersonLink_RemediatesUnlinkedPlatformAdmin proves the
// positive remediation path: an existing, unlinked platform_admin account
// (exactly what cmd/seed-admin produced before this fix, and what already
// exists on any DB seeded before it) can be linked to a person by another
// platform_admin, closing the gap the parallel Asset Registry four-eyes
// hardening depends on.
func TestPlatformStaffPersonLink_RemediatesUnlinkedPlatformAdmin(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	unlinked := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "an-unlinked-admin-pw-1")
	if unlinked.PersonID != nil {
		t.Fatalf("expected the seeded platform_admin to be unlinked, got person_id=%v", unlinked.PersonID)
	}

	caller := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "caller-admin-pw-1")
	callerToken := mustLoginStaff(t, srv, "", caller.Email, "caller-admin-pw-1")

	newPersonID := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, newPersonID)
		return err
	}); err != nil {
		t.Fatalf("seed remediation person: %v", err)
	}

	resp := postJSON(t, srv, "/v1/admin/platform-staff/"+unlinked.ID.String()+"/person-link",
		callerToken.AccessToken, map[string]string{"person_id": newPersonID.String()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 204 linking the platform_admin account, got %d: %+v", resp.StatusCode, apiErr)
	}

	var personID *uuid.UUID
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		staff, err := identity.GetStaffUserByID(ctx, tx, unlinked.ID)
		personID = staff.PersonID
		return err
	})
	if err != nil {
		t.Fatalf("reload staff: %v", err)
	}
	if personID == nil || *personID != newPersonID {
		t.Fatalf("expected person_id=%s after linking, got %v", newPersonID, personID)
	}
}

// TestPlatformStaffPersonLink_TenantScopedCallerForbidden proves the
// deliberate restriction: a tenant_admin (which also holds
// PermStaffManage) must not be able to touch a platform-wide staff
// account through this route, even though the permission check alone
// would allow it.
func TestPlatformStaffPersonLink_TenantScopedCallerForbidden(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	unlinked := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "an-unlinked-admin-pw-2")

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-forbidden-pw-1")
	tenantAdminToken := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-forbidden-pw-1")

	newPersonID := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, newPersonID)
		return err
	}); err != nil {
		t.Fatalf("seed remediation person: %v", err)
	}

	resp := postJSON(t, srv, "/v1/admin/platform-staff/"+unlinked.ID.String()+"/person-link",
		tenantAdminToken.AccessToken, map[string]string{"person_id": newPersonID.String()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for a tenant-scoped caller, got %d: %+v", resp.StatusCode, apiErr)
	}

	var personID *uuid.UUID
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		staff, err := identity.GetStaffUserByID(ctx, tx, unlinked.ID)
		personID = staff.PersonID
		return err
	}); err != nil {
		t.Fatalf("reload staff: %v", err)
	}
	if personID != nil {
		t.Fatalf("expected the platform_admin to remain unlinked after a forbidden attempt, got person_id=%v", personID)
	}
}

// TestPlatformStaffPersonLink_CannotRelinkOnceSet proves the same
// append-only guarantee (migration 0034's staff_users_person_id_append_
// only trigger) holds on this new route exactly as it does on the
// tenant-scoped one.
func TestPlatformStaffPersonLink_CannotRelinkOnceSet(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	// mustCreateStaff always creates an ALREADY-linked account.
	alreadyLinked := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "already-linked-pw-1")

	caller := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "caller-admin-pw-2")
	callerToken := mustLoginStaff(t, srv, "", caller.Email, "caller-admin-pw-2")

	anotherPersonID := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, anotherPersonID)
		return err
	}); err != nil {
		t.Fatalf("seed second person: %v", err)
	}

	resp := postJSON(t, srv, "/v1/admin/platform-staff/"+alreadyLinked.ID.String()+"/person-link",
		callerToken.AccessToken, map[string]string{"person_id": anotherPersonID.String()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 409 re-linking an already-linked platform_admin, got %d: %+v", resp.StatusCode, apiErr)
	}
}

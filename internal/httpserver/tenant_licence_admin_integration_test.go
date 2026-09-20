//go:build integration

// Stage 4I Phase A: HTTP-level tests for
// PUT /v1/admin/tenants/{tenantID}/licence, mirroring
// jurisdiction_admin_integration_test.go's own conventions - real
// Postgres, real tokens, the actual route table.
package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// mustCreateJurisdictionOverHTTP creates a fresh jurisdiction via the
// jurisdiction admin HTTP endpoint under test elsewhere in this package
// and returns its id.
func mustCreateJurisdictionOverHTTP(t *testing.T, srv *httptest.Server, token string) string {
	t.Helper()
	code := newJurisdictionCodeForHTTP()
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdictions", http.MethodPost, token, map[string]any{
		"code": code, "name": "Tenant Licence HTTP Test", "reason_code": "test-setup"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating jurisdiction fixture, got %d", resp.StatusCode)
	}
	var j jurisdictionResponse
	decodeBody(t, resp, &j)
	return j.ID
}

// mustCreateActiveLicenceOverHTTP creates a fresh jurisdiction and an
// active licence under it (with the given licensee) via the jurisdiction
// admin HTTP endpoints under test elsewhere in this package, and returns
// the licence id - so this file's own tests never need to reach into
// internal/jurisdiction directly.
func mustCreateActiveLicenceOverHTTP(t *testing.T, srv *httptest.Server, token, licensee string) string {
	t.Helper()
	jurisdictionID := mustCreateJurisdictionOverHTTP(t, srv, token)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/licences", http.MethodPost, token, map[string]any{
		"jurisdiction_id": jurisdictionID, "licensee": licensee, "licence_number": "HTTP-TL-" + uuid.NewString()[:8], "reason_code": "test-setup"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating licence fixture, got %d", resp.StatusCode)
	}
	var l licenceResponse
	decodeBody(t, resp, &l)
	return l.ID
}

// insertSuspendedLicenceOverHTTP bypasses the licence-creation endpoint
// (which always creates 'active' rows) via a direct SQL insert, mirroring
// internal/jurisdiction's own insertNonActiveLicence fixture convention,
// so this file's own domain-error-mapping tests can exercise
// AssignTenantLicence's non-active-licence rejection at the HTTP layer.
func insertSuspendedLicenceOverHTTP(t *testing.T, pool *db.Pool, jurisdictionID string) string {
	t.Helper()
	jid, err := uuid.Parse(jurisdictionID)
	if err != nil {
		t.Fatalf("parse jurisdiction id fixture: %v", err)
	}
	id := uuid.New()
	// Stage 4I Phase E-SECURITY (migration 0077): `licences` writes now
	// require a genuinely platform-admin-scoped transaction.
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, 'platform', $3, 'suspended')`,
			id, jid, "HTTP-TL-SUS-"+id.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 licence row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert suspended licence fixture: %v", err)
	}
	return id.String()
}

// TestAssignTenantLicenceAPI_HappyPath exercises the full round trip: a
// platform_admin token, a valid active platform-licensee licence, a
// tenant provisioned 'under_platform_licence' (mustCreateTenant's
// default) - 200 with the correct response body.
func TestAssignTenantLicenceAPI_HappyPath(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-happy-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-happy-1").AccessToken
	tenant := mustCreateTenant(t, pool)

	licenceID := mustCreateActiveLicenceOverHTTP(t, srv, token, "platform")

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": licenceID, "reason_code": "onboarding",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body assignTenantLicenceResponse
	decodeBody(t, resp, &body)
	if body.TenantID != tenant.ID.String() {
		t.Errorf("expected tenant_id %s, got %s", tenant.ID, body.TenantID)
	}
	if body.LicenceID == nil || *body.LicenceID != licenceID {
		t.Errorf("expected licence_id %s, got %v", licenceID, body.LicenceID)
	}
}

// TestAssignTenantLicenceAPI_MissingLicenceIDKey proves the key's absence
// (as opposed to a JSON null) is its own distinct 400.
func TestAssignTenantLicenceAPI_MissingLicenceIDKey(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-missing-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-missing-1").AccessToken
	tenant := mustCreateTenant(t, pool)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"reason_code": "onboarding",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing licence_id key, got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_NullUnassigns proves a literal JSON null
// unassigns and returns 200.
func TestAssignTenantLicenceAPI_NullUnassigns(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-null-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-null-1").AccessToken
	tenant := mustCreateTenant(t, pool)

	licenceID := mustCreateActiveLicenceOverHTTP(t, srv, token, "platform")
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": licenceID, "reason_code": "onboarding",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 assigning, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": nil, "reason_code": "offboarding",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for licence_id: null, got %d", resp.StatusCode)
	}
	var body assignTenantLicenceResponse
	decodeBody(t, resp, &body)
	if body.LicenceID != nil {
		t.Errorf("expected licence_id nil after unassigning, got %v", *body.LicenceID)
	}
}

// TestAssignTenantLicenceAPI_MalformedLicenceID proves a non-UUID,
// non-null value is a 400.
func TestAssignTenantLicenceAPI_MalformedLicenceID(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-malformed-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-malformed-1").AccessToken
	tenant := mustCreateTenant(t, pool)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": "not-a-uuid", "reason_code": "onboarding",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed licence_id, got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_MissingReasonCode proves reason_code is
// mandatory, like every other mutating admin surface in this package.
func TestAssignTenantLicenceAPI_MissingReasonCode(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-noreason-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-noreason-1").AccessToken
	tenant := mustCreateTenant(t, pool)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": nil,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing reason_code, got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_TenantAdminDenied proves this is a
// permission gate, not a tenant-scope gate: a tenant_admin token (even
// for its own tenant) must be denied, since RequirePermission alone
// guards this route (it is deliberately NOT wrapped in
// RequireTenantScope).
func TestAssignTenantLicenceAPI_TenantAdminDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-tl-denied-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-tl-denied-1").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": nil, "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a tenant_admin (even for its own tenant), got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_PlayerDenied proves a player token is denied.
func TestAssignTenantLicenceAPI_PlayerDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	email := "player-tl-" + uuid.NewString() + "@example.com"
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "a-decent-password-1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 registering player fixture, got %d", resp.StatusCode)
	}
	var tokens tokenPairResponse
	decodeBody(t, resp, &tokens)

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, tokens.AccessToken, map[string]any{
		"licence_id": nil, "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a player token, got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_NoTokenUnauthenticated proves the route
// requires a bearer token at all.
func TestAssignTenantLicenceAPI_NoTokenUnauthenticated(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, "", map[string]any{
		"licence_id": nil, "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_PlatformAdminCanTargetAnyTenant proves a
// platform_admin (which carries no tenant scope of its own) is
// explicitly permitted to target ANY tenant through this endpoint - not
// a violation: this handler now opens its transaction via
// deps.DB.WithPlatformAdmin (Stage 4I Phase E-SECURITY, migration 0077),
// and platform_admin is the only role holding PermTenantLicenceAssign at
// all.
func TestAssignTenantLicenceAPI_PlatformAdminCanTargetAnyTenant(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-cross-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-cross-1").AccessToken

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	licenceID := mustCreateActiveLicenceOverHTTP(t, srv, token, "platform")

	for _, tgt := range []identity.Tenant{tenantA, tenantB} {
		resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tgt.ID.String()+"/licence", http.MethodPut, token, map[string]any{
			"licence_id": licenceID, "reason_code": "onboarding",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 assigning to tenant %s, got %d", tgt.ID, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestAssignTenantLicenceAPI_UnknownTenantID proves an unknown tenantID
// path segment (well-formed UUID, no such tenants row) maps to exactly
// 404, not a 400 or a raw 500, exercising AssignTenantLicence's
// ErrNotFound branch through the HTTP layer.
func TestAssignTenantLicenceAPI_UnknownTenantID(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-unk-tenant-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-unk-tenant-1").AccessToken
	licenceID := mustCreateActiveLicenceOverHTTP(t, srv, token, "platform")

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+uuid.NewString()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": licenceID, "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown tenantID, got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_LicenseeMismatchRejected proves a licence
// whose licensee does not match the target tenant's licensing_model (the
// composite-FK-backed check AssignTenantLicence maps to ErrInvalidInput)
// surfaces as a controlled 400 with a non-empty, structured error body,
// not a raw internal error or a 500.
func TestAssignTenantLicenceAPI_LicenseeMismatchRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-mismatch-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-mismatch-1").AccessToken
	tenant := mustCreateTenant(t, pool) // 'under_platform_licence' -> expects licensee 'platform'
	mismatched := mustCreateActiveLicenceOverHTTP(t, srv, token, "tenant")

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": mismatched, "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a licensee/licensing_model mismatch, got %d", resp.StatusCode)
	}
	var body map[string]any
	decodeBody(t, resp, &body)
	if len(body) == 0 {
		t.Fatal("expected a non-empty, structured JSON error body")
	}
}

// TestAssignTenantLicenceAPI_SuspendedLicenceRejected proves a
// non-active (suspended) licence is a controlled 400, not silently
// accepted or a 500.
func TestAssignTenantLicenceAPI_SuspendedLicenceRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-suspended-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-suspended-1").AccessToken
	tenant := mustCreateTenant(t, pool)
	jurisdictionID := mustCreateJurisdictionOverHTTP(t, srv, token)
	suspended := insertSuspendedLicenceOverHTTP(t, pool, jurisdictionID)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": suspended, "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a suspended licence, got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_UnknownLicenceIDRejected proves a
// well-formed but non-existent licence_id is a controlled 400 (mapped
// from ErrInvalidInput), not a 500 or a 404.
func TestAssignTenantLicenceAPI_UnknownLicenceIDRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-unk-licence-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-unk-licence-1").AccessToken
	tenant := mustCreateTenant(t, pool)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": uuid.NewString(), "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-existent licence_id, got %d", resp.StatusCode)
	}
}

// TestAssignTenantLicenceAPI_ExclusiveLicenceAlreadyBoundRejected proves
// the migration 0077 uq_tenants_exclusive_own_licence unique-violation
// path is mapped to a diagnosable 400, not an opaque 500 (Fix 1, this fix
// round): a BYOL (licensee='tenant') licence already bound to one
// own_licence tenant must be rejected, at the HTTP layer, when a second
// own_licence tenant attempts to bind the SAME licence.
func TestAssignTenantLicenceAPI_ExclusiveLicenceAlreadyBoundRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-excl-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-excl-1").AccessToken

	byolLicence := mustCreateActiveLicenceOverHTTP(t, srv, token, "tenant")

	createByolTenant := func(nameSuffix string) identity.Tenant {
		resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants", http.MethodPost, token, map[string]any{
			"name": "Exclusive Licence Tenant " + nameSuffix, "slug": "excl-lic-" + nameSuffix + "-" + uuid.NewString()[:8],
			"licensing_model": "own_licence", "reason_code": "test-setup",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 creating BYOL tenant fixture, got %d", resp.StatusCode)
		}
		var tr tenantResponse
		decodeBody(t, resp, &tr)
		id, err := uuid.Parse(tr.ID)
		if err != nil {
			t.Fatalf("parse created tenant id: %v", err)
		}
		return identity.Tenant{ID: id, Name: tr.Name, Slug: tr.Slug, LicensingModel: tr.LicensingModel, Status: tr.Status}
	}

	tenantX := createByolTenant("x")
	tenantY := createByolTenant("y")

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenantX.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": byolLicence, "reason_code": "byol-first-bind",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the first tenant's BYOL bind, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenantY.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": byolLicence, "reason_code": "byol-second-bind-attempt",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 (not 500) for a licence already exclusively bound to another tenant, got %d", resp.StatusCode)
	}
	var body map[string]any
	decodeBody(t, resp, &body)
	if len(body) == 0 {
		t.Fatal("expected a non-empty, structured JSON error body")
	}
}

// TestAssignTenantLicenceAPI_ForgedExtraFieldRejected proves a genuinely
// UNKNOWN field name in the body (e.g. an attempt to smuggle tenant_id
// through the body rather than the path) is rejected by decodeJSON's
// DisallowUnknownFields, not silently ignored. This does NOT prove field-
// name smuggling is rejected in general: encoding/json's
// DisallowUnknownFields matches struct tags case-insensitively, so a
// case-variant of an already-known field name would not be caught by this
// mechanism - that is a pre-existing, platform-wide decodeJSON
// characteristic, not something this endpoint (or this fix) changes.
func TestAssignTenantLicenceAPI_ForgedExtraFieldRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-tl-forged-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-tl-forged-1").AccessToken
	tenant := mustCreateTenant(t, pool)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/licence", http.MethodPut, token, map[string]any{
		"licence_id": nil, "reason_code": "attempt", "tenant_id": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field (tenant_id) in the request body, got %d", resp.StatusCode)
	}
}

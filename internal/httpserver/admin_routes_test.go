package httpserver

// Adversarial coverage for the staff-creation allowlist fix (Stage 4H-B1
// Wave 2 Phase 3, security-architecture.md's P1 finding): "the
// promotions_manager and bonus_operations roles may only be created by a
// platform administrator" - a tenant-scoped admin (tc.TenantID != Nil)
// must be REJECTED before ever reaching the database, exactly mirroring
// the pre-existing finance/risk_manager restrictions this fix extends.
//
// These tests exercise newCreateStaffHandler directly (no router, no
// live DB) rather than through internal/httpserver's route table,
// because the rejection path this dispatch is verifying returns before
// deps.DB is ever touched - see newCreateStaffHandler's own body: every
// role-allowlist check runs strictly before the deps.DB.WithTenant call.
// A nil *db.Pool is therefore safe for every case tested here as long as
// the assertion is "rejected with 403 and the DB is never reached" - if
// a change ever made a case wrongly fall through to the DB call, this
// test would panic on the nil pool rather than silently pass, which is
// the correct failure mode for this test to have.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func newCreateStaffRequest(t *testing.T, tc tenant.Context, targetTenantID uuid.UUID, role string) *http.Request {
	t.Helper()
	body, err := json.Marshal(createStaffRequest{
		Email:    "new-staff@example.com",
		Password: "a-sufficiently-long-password",
		Role:     role,
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants/"+targetTenantID.String()+"/staff", bytes.NewReader(body))
	req.SetPathValue("tenantID", targetTenantID.String())
	req = req.WithContext(tenant.WithContext(req.Context(), tc))
	return req
}

// TestCreateStaff_TenantScopedAdmin_CannotSelfEscalateToBonusOperations is
// the adversarial test this dispatch's own item 9 requires: a
// tenant-scoped admin (tc.TenantID == its own tenant, not uuid.Nil)
// attempting to create a bonus_operations staff account for ITS OWN
// tenant (canActOnTenant would otherwise allow this - it is not a
// cross-tenant attempt, it is a same-tenant self-escalation attempt) must
// be refused with 403, never reaching deps.DB.
func TestCreateStaff_TenantScopedAdmin_CannotSelfEscalateToBonusOperations(t *testing.T) {
	deps := Deps{DB: nil} // must never be dereferenced on this path
	handler := newCreateStaffHandler(deps)

	tenantID := uuid.New()
	tc := tenant.Context{TenantID: tenantID, Role: "tenant_admin", PrincipalType: "staff", Subject: uuid.New().String()}

	req := newCreateStaffRequest(t, tc, tenantID, "bonus_operations")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for a tenant-scoped admin creating bonus_operations, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateStaff_TenantScopedAdmin_CannotSelfEscalateToPromotionsManager
// is the identical adversarial case for promotions_manager - the OTHER
// role this fix's allowlist covers, tested separately since the
// production code branches on an OR of two role checks and a test
// asserting only one arm would not prove the other is actually gated.
func TestCreateStaff_TenantScopedAdmin_CannotSelfEscalateToPromotionsManager(t *testing.T) {
	deps := Deps{DB: nil}
	handler := newCreateStaffHandler(deps)

	tenantID := uuid.New()
	tc := tenant.Context{TenantID: tenantID, Role: "tenant_admin", PrincipalType: "staff", Subject: uuid.New().String()}

	req := newCreateStaffRequest(t, tc, tenantID, "promotions_manager")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for a tenant-scoped admin creating promotions_manager, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateStaff_TenantScopedAdmin_OrdinaryRolesStillPermitted is the
// SEP-1-H1-style anti-inertness control for this same allowlist: proving
// the handler is not simply refusing every request from a tenant-scoped
// caller (which would make the two tests above pass for the wrong
// reason). A tenant_admin creating an ordinary "support" account must
// pass every allowlist check and reach deps.DB - proven here by the test
// panicking on the nil pool at that exact point, which is the intended,
// documented signal (see this file's own top-level comment) that this
// case's rejection checks were all satisfied and execution fell through
// to the DB call.
func TestCreateStaff_TenantScopedAdmin_OrdinaryRolesStillPermitted(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected a nil-pointer panic on deps.DB.WithTenant - the allowlist checks unexpectedly rejected an ordinary role before reaching the DB call")
		}
	}()

	deps := Deps{DB: nil}
	handler := newCreateStaffHandler(deps)

	tenantID := uuid.New()
	tc := tenant.Context{TenantID: tenantID, Role: "tenant_admin", PrincipalType: "staff", Subject: uuid.New().String()}

	req := newCreateStaffRequest(t, tc, tenantID, "support")
	rec := httptest.NewRecorder()
	handler(rec, req)
}

// TestCreateStaff_PlatformAdmin_CanCreateBonusOperations proves the
// allowlist is not accidentally unconditional (i.e. that it correctly
// distinguishes "tenant-scoped caller" from "platform_admin caller",
// rather than refusing bonus_operations/promotions_manager for
// everyone): a platform_admin caller (tc.TenantID == uuid.Nil, per
// docs/decisions/0011) is the one case doc/security-architecture.md's
// wiring table names as the sole permitted issuer of this role, so this
// case must fall through to the DB call rather than being refused at the
// allowlist. Same nil-pool-panic signal as the ordinary-roles case above.
func TestCreateStaff_PlatformAdmin_CanCreateBonusOperations(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected a nil-pointer panic on deps.DB.WithTenant - the allowlist unexpectedly rejected a platform_admin caller creating bonus_operations")
		}
	}()

	deps := Deps{DB: nil}
	handler := newCreateStaffHandler(deps)

	targetTenantID := uuid.New()
	tc := tenant.Context{TenantID: uuid.Nil, Role: "platform_admin", PrincipalType: "staff", Subject: uuid.New().String()}

	req := newCreateStaffRequest(t, tc, targetTenantID, "bonus_operations")
	rec := httptest.NewRecorder()
	handler(rec, req)
}

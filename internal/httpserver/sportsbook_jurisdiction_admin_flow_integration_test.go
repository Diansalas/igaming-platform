//go:build integration

// Stage 9.2 (ADR 0083 §5.2/§9.2) HTTP-layer tests for the sportsbook
// jurisdiction-restriction admin surface: platform_admin can create/list/
// withdraw; a tenant-scoped staff role (holding no
// sportsbook_jurisdiction_restriction:* permission) is refused; an
// unauthenticated caller is refused. Mirrors risk_flow_integration_test.go's
// established structure.
package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// mustSeedSportsbookEvent mirrors mustSeedSportsbookSelection but returns
// the owning event id too - the admin restriction endpoint's own scope
// target.
func mustSeedSportsbookEvent(t *testing.T, pool *db.Pool) (eventID uuid.UUID) {
	t.Helper()
	ref := uuid.NewString()[:8]
	err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		var sportID, compID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_sports (external_ref, code, name) VALUES ($1, $2, 'Admin Test Sport') RETURNING id`,
			"admin-sport-"+ref, "admin-sport-"+ref).Scan(&sportID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_competitions (sport_id, external_ref, name) VALUES ($1, $2, 'Admin Test Competition') RETURNING id`,
			sportID, "admin-comp-"+ref).Scan(&compID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO sb_events (competition_id, external_ref, name, start_time, status) VALUES ($1, $2, 'Admin Test Event', $3, 'scheduled') RETURNING id`,
			compID, "admin-event-"+ref, time.Now().Add(48*time.Hour)).Scan(&eventID)
	})
	if err != nil {
		t.Fatalf("seed sportsbook event: %v", err)
	}
	return eventID
}

func mustSeedJurisdictionCodeForAdminTest(t *testing.T, pool *db.Pool) string {
	t.Helper()
	code := "ADMJT-" + uuid.NewString()[:8]
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Admin Flow Test Jurisdiction')`, uuid.New(), code)
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction code: %v", err)
	}
	return code
}

// TestSportsbookJurisdictionRestrictionAdmin_PlatformAdminFullLifecycle
// proves the create -> list -> withdraw -> list round trip via HTTP,
// gated by PermSportsbookJurisdictionRestrictionManage/Read.
func TestSportsbookJurisdictionRestrictionAdmin_PlatformAdminFullLifecycle(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	platformAdmin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-jur-admin-pw-1")
	token := mustLoginStaff(t, srv, "", platformAdmin.Email, "pa-jur-admin-pw-1")

	eventID := mustSeedSportsbookEvent(t, pool)
	code := mustSeedJurisdictionCodeForAdminTest(t, pool)

	createResp := postJSON(t, srv, "/v1/admin/sportsbook/jurisdiction-restrictions", token.AccessToken, map[string]any{
		"scope_kind": "event", "event_id": eventID.String(), "jurisdiction_code": code,
		"authorization_reference": "http-flow-authz-ref", "reason_code": "http-flow-reason",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		var apiErr apierror.Error
		decodeBody(t, createResp, &apiErr)
		t.Fatalf("expected 201 creating a restriction, got %d: %+v", createResp.StatusCode, apiErr)
	}
	var created jurisdictionRestrictionResponse
	decodeBody(t, createResp, &created)
	if created.ID == "" || created.Status != "active" {
		t.Fatalf("expected an active restriction with a real id, got %+v", created)
	}

	listResp := getJSON(t, srv, "/v1/admin/sportsbook/jurisdiction-restrictions", token.AccessToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing restrictions, got %d", listResp.StatusCode)
	}
	var listed []jurisdictionRestrictionResponse
	decodeBody(t, listResp, &listed)
	var found bool
	for _, r := range listed {
		if r.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the created restriction to appear in the list")
	}

	withdrawResp := postJSON(t, srv, "/v1/admin/sportsbook/jurisdiction-restrictions/"+created.ID+"/withdraw", token.AccessToken, map[string]any{
		"reason_code": "http-flow-withdrawn",
	})
	defer withdrawResp.Body.Close()
	if withdrawResp.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, withdrawResp, &apiErr)
		t.Fatalf("expected 200 withdrawing the restriction, got %d: %+v", withdrawResp.StatusCode, apiErr)
	}
	var withdrawn jurisdictionRestrictionResponse
	decodeBody(t, withdrawResp, &withdrawn)
	if withdrawn.Status != "withdrawn" {
		t.Fatalf("expected status 'withdrawn', got %q", withdrawn.Status)
	}

	// Withdrawing an already-withdrawn restriction is refused with 404.
	secondWithdraw := postJSON(t, srv, "/v1/admin/sportsbook/jurisdiction-restrictions/"+created.ID+"/withdraw", token.AccessToken, map[string]any{
		"reason_code": "http-flow-withdrawn-again",
	})
	defer secondWithdraw.Body.Close()
	if secondWithdraw.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 withdrawing an already-withdrawn restriction, got %d", secondWithdraw.StatusCode)
	}
}

// TestSportsbookJurisdictionRestrictionAdmin_TenantAdminForbidden proves a
// tenant-scoped staff role holding no
// sportsbook_jurisdiction_restriction:* permission cannot reach either
// write endpoint - this is a platform-only surface, exactly like PUT
// /v1/admin/casino/games.
func TestSportsbookJurisdictionRestrictionAdmin_TenantAdminForbidden(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-jur-admin-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-jur-admin-pw-1")

	eventID := mustSeedSportsbookEvent(t, pool)
	code := mustSeedJurisdictionCodeForAdminTest(t, pool)

	resp := postJSON(t, srv, "/v1/admin/sportsbook/jurisdiction-restrictions", token.AccessToken, map[string]any{
		"scope_kind": "event", "event_id": eventID.String(), "jurisdiction_code": code,
		"authorization_reference": "should-not-be-created", "reason_code": "should-not-be-created",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for a tenant_admin creating a jurisdiction restriction, got %d: %+v", resp.StatusCode, apiErr)
	}
}

// TestSportsbookJurisdictionRestrictionAdmin_UnauthenticatedForbidden
// proves the surface requires a bearer token at all.
func TestSportsbookJurisdictionRestrictionAdmin_UnauthenticatedForbidden(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	resp := getJSON(t, srv, "/v1/admin/sportsbook/jurisdiction-restrictions", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unauthenticated list request, got %d", resp.StatusCode)
	}
}

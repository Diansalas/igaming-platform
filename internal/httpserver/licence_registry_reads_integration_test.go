//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// lic4 slice 4a: the platform READ APIs (jurisdictions / licences by id with bound tenants, licence country ceilings,
// operating country policies of a tenant, tenant jurisdiction configurations): platform_admin only, paginated, read-only.

type pageIT struct {
	Items  []map[string]any `json:"items"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
	Total  int              `json:"total"`
}

type readsWorld struct {
	lcHTTP
	do       func(method, path string, body any) *http.Response
	adminID  uuid.UUID
	tenantA  uuid.UUID
	tenantB  uuid.UUID
	jurID    string
	adminTok string
}

func newReadsWorld(t *testing.T, pw string) *readsWorld {
	t.Helper()
	h, do := lcSetup(t, pw)
	w := &readsWorld{lcHTTP: h, do: do}
	pool := launchfix.OwnerPool(t)
	// Resolve the platform admin id and the jurisdiction of the fixture licence from the API itself.
	resp := do(http.MethodGet, "/v1/admin/licences/"+h.licID, nil)
	var l struct {
		JurisdictionID string   `json:"jurisdiction_id"`
		TenantIDs      []string `json:"tenant_ids"`
	}
	decodeBody(t, resp, &l)
	w.jurID = l.JurisdictionID
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM staff_users WHERE tenant_id IS NULL AND status = 'active' ORDER BY created_at DESC LIMIT 1`).Scan(&w.adminID)
	}); err != nil {
		t.Fatal(err)
	}
	ownerPlatform := func(fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := pool.WithPlatformAdmin(context.Background(), w.adminID, fn); err != nil {
			t.Fatal(err)
		}
	}
	var ta, tb uuid.UUID
	for _, dst := range []*uuid.UUID{&ta, &tb} {
		ownerPlatform(func(ctx context.Context, tx pgx.Tx) error {
			tn, err := launchfix.CreateTenant(ctx, tx, "reads", "reads-"+uuid.NewString(), "under_platform_licence")
			*dst = tn.ID
			return err
		})
	}
	w.tenantA, w.tenantB = ta, tb
	licID := uuid.MustParse(h.licID)
	ownerPlatform(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, licID, ta)
		return err
	})
	// One enabled-then-disabled ceiling history for the licence (owner path, through the real writer).
	actor := operatingmarket.ActorContext{ActorID: w.adminID, IPAddress: "127.0.0.1", RequestID: "reads", ReasonCode: "reads_fixture"}
	ownerPlatform(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: licID, CountryCode: "MT", State: operatingmarket.StateEnabled, Status: operatingmarket.StatusActive, AuthorizationReference: "REF-MT", Actor: actor}); err != nil {
			return err
		}
		_, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: licID, CountryCode: "MT", State: operatingmarket.StateDisabled, Status: operatingmarket.StatusActive, Actor: actor})
		return err
	})
	// A disabled tenant-rung policy for each tenant (tenant-scoped owner path).
	for _, tid := range []uuid.UUID{ta, tb} {
		if err := pool.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
				Scope: operatingmarket.ScopeTenant, TenantID: tid, CountryCode: "MT", State: operatingmarket.StateDisabled, Status: operatingmarket.StatusActive, Actor: actor})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A configuration version for tenant A.
	ownerPlatform(func(ctx context.Context, tx pgx.Tx) error {
		_, err := jurisdiction.SetTenantJurisdictionConfigVersion(ctx, tx, jurisdiction.SetTenantJurisdictionConfigVersionParams{
			TenantID: ta, JurisdictionID: uuid.MustParse(w.jurID),
			Content: jurisdiction.TenantJurisdictionConfigContent{AllowedCurrencies: []string{}, AllowedPaymentMethods: []string{}, GeoBlockList: []string{"MT"}},
			Actor:   jurisdiction.ActorContext{ActorID: w.adminID, IPAddress: "127.0.0.1", RequestID: "reads", ReasonCode: "reads_fixture"}})
		return err
	})
	return w
}

func TestReadAPIs_PlatformAdmin_RoundTrip(t *testing.T) {
	w := newReadsWorld(t, "pa-reads-pw-1")

	// jurisdictions: list contains it, by id works, unknown 404, malformed 400.
	resp := w.do(http.MethodGet, "/v1/admin/jurisdictions/"+w.jurID, nil)
	var j jurisdictionResponse
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get jurisdiction: %d", resp.StatusCode)
	}
	decodeBody(t, resp, &j)
	if j.ID != w.jurID {
		t.Fatalf("jurisdiction %+v", j)
	}
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/jurisdictions/"+uuid.NewString(), nil), http.StatusNotFound, "unknown jurisdiction")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/jurisdictions/nope", nil), http.StatusBadRequest, "malformed jurisdiction id")

	// licences: by id with the bound tenant; list carries tenant_ids too.
	resp = w.do(http.MethodGet, "/v1/admin/licences/"+w.licID, nil)
	var lic struct {
		ID        string   `json:"id"`
		TenantIDs []string `json:"tenant_ids"`
		Status    string   `json:"status"`
	}
	decodeBody(t, resp, &lic)
	if lic.ID != w.licID || len(lic.TenantIDs) != 1 || lic.TenantIDs[0] != w.tenantA.String() {
		t.Fatalf("licence detail %+v, want tenant %s bound", lic, w.tenantA)
	}
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/licences/"+uuid.NewString(), nil), http.StatusNotFound, "unknown licence")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/licences/nope", nil), http.StatusBadRequest, "malformed licence id")
	resp = w.do(http.MethodGet, "/v1/admin/licences", nil)
	var list []struct {
		ID        string   `json:"id"`
		TenantIDs []string `json:"tenant_ids"`
	}
	decodeBody(t, resp, &list)
	found := false
	for _, l := range list {
		if l.ID == w.licID {
			found = true
			if len(l.TenantIDs) != 1 || l.TenantIDs[0] != w.tenantA.String() {
				t.Fatalf("list tenant_ids %+v", l)
			}
		} else if l.TenantIDs == nil {
			t.Fatalf("tenant_ids must be an array (never null): %+v", l)
		}
	}
	if !found {
		t.Fatal("licence not listed")
	}

	// ceilings: two versions, paginated, current-only.
	var pg pageIT
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings?licence_id="+w.licID, nil), &pg)
	if pg.Total != 2 || len(pg.Items) != 2 || pg.Limit != 50 || pg.Offset != 0 {
		t.Fatalf("ceilings page %+v", pg)
	}
	if pg.Items[0]["created_by"] == nil {
		t.Fatal("a platform viewer sees the authoring staff account")
	}
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings?licence_id="+w.licID+"&current=true", nil), &pg)
	if pg.Total != 1 || len(pg.Items) != 1 || pg.Items[0]["state"] != "disabled" {
		t.Fatalf("current ceilings %+v", pg)
	}
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings?licence_id="+w.licID+"&limit=1&offset=1", nil), &pg)
	if pg.Total != 2 || len(pg.Items) != 1 || pg.Limit != 1 || pg.Offset != 1 {
		t.Fatalf("paged ceilings %+v", pg)
	}
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings?licence_id="+w.licID+"&limit=100000", nil), &pg)
	if pg.Limit != 200 {
		t.Fatalf("the shared page clamp (max 200) applies: %+v", pg)
	}
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings?licence_id="+w.licID+"&country_code=DE", nil), &pg)
	if pg.Total != 0 || pg.Items == nil {
		t.Fatalf("a country with no versions: %+v", pg)
	}
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings", nil), http.StatusBadRequest, "licence_id required")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings?licence_id="+uuid.NewString(), nil), http.StatusNotFound, "unknown licence")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings?licence_id="+w.licID+"&country_code=mt", nil), http.StatusBadRequest, "lowercase country")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/licence-country-ceilings?licence_id="+w.licID+"&current=maybe", nil), http.StatusBadRequest, "bad current")

	// policies: only the named tenant's rows.
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/operating-country-policies?tenant_id="+w.tenantA.String(), nil), &pg)
	if pg.Total != 1 || len(pg.Items) != 1 || pg.Items[0]["tenant_id"] != w.tenantA.String() || pg.Items[0]["scope"] != "tenant" {
		t.Fatalf("tenant A policies %+v", pg)
	}
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/operating-country-policies?tenant_id="+w.tenantB.String()+"&current=true", nil), &pg)
	if pg.Total != 1 || pg.Items[0]["tenant_id"] != w.tenantB.String() {
		t.Fatalf("tenant B policies %+v", pg)
	}
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/operating-country-policies", nil), http.StatusBadRequest, "tenant_id required")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/operating-country-policies?tenant_id="+uuid.NewString(), nil), http.StatusNotFound, "unknown tenant")

	// jurisdiction configs of tenant A.
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/tenants/"+w.tenantA.String()+"/jurisdiction-configs", nil), &pg)
	if pg.Total != 1 || len(pg.Items) != 1 || pg.Items[0]["jurisdiction_id"] != w.jurID || pg.Items[0]["effective_to"] != nil {
		t.Fatalf("configs %+v", pg)
	}
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/tenants/"+w.tenantB.String()+"/jurisdiction-configs", nil), &pg)
	if pg.Total != 0 || pg.Items == nil {
		t.Fatalf("tenant B has no configuration: %+v", pg)
	}
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/tenants/"+uuid.NewString()+"/jurisdiction-configs", nil), http.StatusNotFound, "unknown tenant")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/tenants/nope/jurisdiction-configs", nil), http.StatusBadRequest, "malformed tenant id")
}

// Every new read route is platform_admin only: a tenant admin is 403, an anonymous caller 401, and (read-only) nothing is created.
func TestReadAPIs_NonPlatformPrincipalsRefused(t *testing.T) {
	w := newReadsWorld(t, "pa-reads-pw-2")
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-reads-pw-1")
	co := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "co-reads-pw-1")
	tokens := map[string]string{
		"tenant_admin": mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-reads-pw-1").AccessToken,
		"compliance":   mustLoginStaff(t, srv, tenant.Slug, co.Email, "co-reads-pw-1").AccessToken,
	}
	paths := []string{
		"/v1/admin/jurisdictions", "/v1/admin/jurisdictions/" + w.jurID,
		"/v1/admin/licences", "/v1/admin/licences/" + w.licID,
		"/v1/admin/licences/" + w.licID + "/versions",
		"/v1/admin/licence-country-ceilings?licence_id=" + w.licID,
		"/v1/admin/operating-country-policies?tenant_id=" + w.tenantA.String(),
		"/v1/admin/tenants/" + w.tenantA.String() + "/jurisdiction-configs",
	}
	for _, p := range paths {
		for role, tok := range tokens {
			r := sendAssetJSON(t, srv.URL+p, http.MethodGet, tok, nil)
			if r.StatusCode != http.StatusForbidden {
				t.Errorf("%s GET %s: want 403, got %d", role, p, r.StatusCode)
			}
			r.Body.Close()
		}
		r := sendAssetJSON(t, srv.URL+p, http.MethodGet, "", nil)
		if r.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s: want 401, got %d", p, r.StatusCode)
		}
		r.Body.Close()
	}
}

// G-14: the server has NO default licensing model: a tenant creation without one, with an empty one or with an unknown one is
// refused (the Back Office form must collect an explicit choice).
func TestG14_NoServerSideLicensingModelDefault(t *testing.T) {
	w := newReadsWorld(t, "pa-reads-pw-3")
	for name, body := range map[string]map[string]any{
		"absent":  {"name": "G14", "slug": "g14-" + uuid.NewString()[:8], "reason_code": "g14"},
		"empty":   {"name": "G14", "slug": "g14-" + uuid.NewString()[:8], "licensing_model": "", "reason_code": "g14"},
		"unknown": {"name": "G14", "slug": "g14-" + uuid.NewString()[:8], "licensing_model": "platform", "reason_code": "g14"},
	} {
		r := w.do(http.MethodPost, "/v1/admin/tenants", body)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s licensing_model: want 400, got %d", name, r.StatusCode)
		}
		r.Body.Close()
	}
}

func expectStatus(t *testing.T, r *http.Response, want int, what string) {
	t.Helper()
	defer r.Body.Close()
	if r.StatusCode != want {
		t.Errorf("%s: want %d, got %d", what, want, r.StatusCode)
	}
}

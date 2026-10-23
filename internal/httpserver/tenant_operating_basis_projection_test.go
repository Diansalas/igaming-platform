package httpserver

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

func fullBasisRecord() operatingmarket.BasisRecord {
	s := func(v string) *string { return &v }
	u := func() *uuid.UUID { id := uuid.New(); return &id }
	due := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	return operatingmarket.BasisRecord{
		ID: uuid.New(), TenantID: uuid.New(), BrandID: u(), CountryCode: "MT", OperationCode: "deposit", ProductCode: s("casino"),
		Kind: operatingmarket.BasisNoLicenceDocumented, LicenceID: u(), JurisdictionID: u(),
		DeterminationReference: "DET-SECRET-1", DeterminedByRole: "role_secret", EvidenceReference: s("EVID-SECRET-1"),
		RestrictionsNote: s("NOTE-SECRET-TEXT"), RestrictedProductCodes: []string{"sportsbook"}, ReviewDueAt: &due, NeedsRereview: true,
		LastReviewReference: s("REV-SECRET-1"), Status: operatingmarket.StatusWithdrawn, ReasonCode: "reason_secret", PolicyVersion: "pv1",
		EffectiveFrom: to.Add(-time.Hour), EffectiveTo: &to, ChangeRequestID: u(), CreatedByActorID: u(), CreatedAt: to,
	}
}

// The tenant projection's JSON field set is pinned (golden). Adding a field is a reviewed diff of THIS list.
var tenantOperatingBasisGoldenFields = []string{
	"basis_kind", "brand_id", "country_code", "created_at", "disclaimer", "effective_from", "effective_to", "has_restrictions_note", "id",
	"is_current", "needs_rereview", "operation_code", "policy_version", "product_code", "restricted_product_codes", "review_due_at", "status",
}

func TestTenantOperatingBasisProjection_GoldenFieldSetAndRedaction(t *testing.T) {
	rec := fullBasisRecord()
	raw, err := json.Marshal(toTenantOperatingBasisResponse(rec))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, tenantOperatingBasisGoldenFields) {
		t.Fatalf("tenant projection field set drifted:\n got %v\nwant %v", got, tenantOperatingBasisGoldenFields)
	}
	// None of the withheld values, ids or names may appear anywhere in the payload.
	for _, forbidden := range []string{
		"DET-SECRET-1", "role_secret", "EVID-SECRET-1", "NOTE-SECRET-TEXT", "REV-SECRET-1", "reason_secret",
		rec.TenantID.String(), rec.LicenceID.String(), rec.JurisdictionID.String(), rec.ChangeRequestID.String(), rec.CreatedByActorID.String(),
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("tenant payload leaks %q: %s", forbidden, raw)
		}
	}
	for _, k := range []string{"tenant_id", "licence_id", "jurisdiction_id", "determination_reference", "determined_by_role", "basis_evidence_reference",
		"restrictions_note", "last_review_reference", "reason_code", "change_request_id", "created_by", "decision", "approvals"} {
		if _, ok := m[k]; ok {
			t.Errorf("forbidden key %q present", k)
		}
	}
	if m["status"] != "withdrawn" || m["is_current"] != false || m["has_restrictions_note"] != true || m["review_due_at"] != "2027-01-02" || m["needs_rereview"] != true {
		t.Errorf("projection values wrong: %s", raw)
	}
	// Open version, no note, nil lists.
	rec.EffectiveTo, rec.RestrictionsNote, rec.RestrictedProductCodes, rec.ReviewDueAt = nil, nil, nil, nil
	raw, _ = json.Marshal(toTenantOperatingBasisResponse(rec))
	if !strings.Contains(string(raw), `"restricted_product_codes":[]`) || !strings.Contains(string(raw), `"is_current":true`) || !strings.Contains(string(raw), `"has_restrictions_note":false`) {
		t.Errorf("open version / nil handling wrong: %s", raw)
	}
}

// The response struct itself has no staffRef or tenant id field (compile-time shape check independent of the values above).
func TestTenantOperatingBasisResponse_StructHasNoStaffOrTenantField(t *testing.T) {
	typ := reflect.TypeOf(tenantOperatingBasisResponse{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if strings.Contains(f.Type.String(), "staffRef") {
			t.Errorf("field %s carries a staff reference", f.Name)
		}
		if strings.Contains(strings.ToLower(f.Name), "tenant") || strings.Contains(strings.ToLower(f.Name), "actor") {
			t.Errorf("field %s must not exist in the tenant projection", f.Name)
		}
	}
}

// Static: the tenant routes are RequireTenantScope + the dedicated permission + stepUpNone, and the handler file reads no tenant
// from the request and opens only WithTenant transactions.
func TestTenantOperatingBasisRoutes_StaticGating(t *testing.T) {
	b, err := os.ReadFile("jurisdiction_admin_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, pat := range []string{"GET /v1/admin/tenant/operating-bases\"", "GET /v1/admin/tenant/operating-bases/{basisID}\""} {
		i := strings.Index(string(b), pat)
		if i < 0 {
			t.Fatalf("route %s not mounted", pat)
		}
		rest := string(b)[i:]
		end := strings.Index(rest, "\n\tmux.Handle")
		if end < 0 {
			end = len(rest)
		}
		line := rest[:end]
		for _, want := range []string{"stepUpNone", "auth.RequireTenantScope(", "auth.PermOperatingBasisTenantRead"} {
			if !strings.Contains(line, want) {
				t.Errorf("%s lacks %s: %s", pat, want, line)
			}
		}
		if strings.Contains(line, "PermJurisdictionRegistryRead") {
			t.Errorf("%s must not use the platform registry permission", pat)
		}
	}
	h, err := os.ReadFile("tenant_operating_basis_read_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(h)
	code := src[strings.Index(src, "\nimport"):]
	if strings.Contains(code, "WithPlatformAdmin(") || strings.Contains(code, "WithoutTenant(") {
		t.Error("the tenant handlers must open tenant transactions only")
	}
	if strings.Contains(code, `Get("tenant_id")`) || strings.Contains(code, `PathValue("tenantID")`) {
		t.Error("the tenant handlers must never read a client tenant id")
	}
	if n := strings.Count(code, "deps.DB.WithTenant("); n != 2 {
		t.Errorf("expected 2 tenant transactions, found %d", n)
	}
}

// The OpenAPI entries exist, name the dedicated permission, document 401/403/404 and list every projected field (and none of the
// withheld ones as properties of TenantOperatingBasis).
func TestOpenAPI_TenantOperatingBases_Documented(t *testing.T) {
	spec := launchSpec(t)
	for _, p := range []string{"/v1/admin/tenant/operating-bases", "/v1/admin/tenant/operating-bases/{basisID}"} {
		block := openAPIPathBlock(t, spec, p)
		for _, want := range []string{"    get:\n", "operating_basis:tenant_read", "bearerAuth", `"401"`, `"403"`, `"404"`} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: OpenAPI entry lacks %q", p, want)
			}
		}
		if strings.Contains(block, "name: tenant_id") {
			t.Errorf("%s must not declare a tenant_id parameter", p)
		}
	}
	i := strings.Index(spec, "\n    TenantOperatingBasis:\n")
	j := strings.Index(spec, "\n    TenantOperatingBasisPage:\n")
	if i < 0 || j < i {
		t.Fatal("TenantOperatingBasis schema not found")
	}
	schema := spec[i:j]
	props := schema[strings.Index(schema, "      properties:\n"):strings.Index(schema, "      required:")]
	for _, f := range tenantOperatingBasisGoldenFields {
		if !strings.Contains(props, "        "+f+":") {
			t.Errorf("schema lacks projected field %s", f)
		}
	}
	if n := strings.Count(props, "\n        "); n != len(tenantOperatingBasisGoldenFields) {
		t.Errorf("schema has %d properties, golden has %d", n, len(tenantOperatingBasisGoldenFields))
	}
}

//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/licensingchange"
)

// c1: every reachable PRECHECK refusal of the licensing change routes (and the duplicate jurisdiction code) over the real HTTP
// stack as igaming_runtime, against the real database: exact status, the unchanged generic API code, the exact closed code in
// check_codes, the fixed sentence, and NOTHING written (row counts of every governed table are identical before and after).

func (w *lhWorld) lcCounts() map[string]int {
	w.t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"licensing_change_requests", "licensing_change_approvals", "licences", "licence_country_ceilings",
		"operating_country_policies", "tenant_jurisdiction_configs", "operating_bases", "jurisdictions", "audit_log"} {
		var n int
		if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n)
		}); err != nil {
			w.t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	var bound int
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE licence_id IS NOT NULL`).Scan(&bound)
	}); err != nil {
		w.t.Fatal(err)
	}
	out["tenants_bound"] = bound
	return out
}

func (w *lhWorld) lcLicence(jid uuid.UUID, licensee, status string) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, $3, $4, $5)`,
			id, jid, licensee, "C1-"+id.String()[:8], status)
		return err
	}); err != nil {
		w.t.Fatalf("seed licence: %v", err)
	}
	return id
}

func (w *lhWorld) lcOwnLicenceTenant() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, 'c1 own', $2, 'own_licence', 'active')`, id, "c1-own-"+id.String())
		return err
	}); err != nil {
		w.t.Fatalf("seed own-licence tenant: %v", err)
	}
	return id
}

// lcFile files a change as the platform requester; lcExecute files and has two other Persons approve it.
func (w *lhWorld) lcFile(token, kind, reason string, payload map[string]any) cgResp {
	return w.do("POST", "/v1/admin/licensing-changes", token, map[string]any{"kind": kind, "reason_code": reason, "payload": payload})
}

func (w *lhWorld) lcExecute(kind string, payload map[string]any) {
	w.t.Helper()
	r := w.lcFile(w.tokReq(), kind, "c1_setup", payload)
	if r.status != http.StatusCreated {
		w.t.Fatalf("setup %s: %d %s", kind, r.status, r.body)
	}
	var f licensingChangeResponseIT
	r.decode(w.t, &f)
	if !f.Executed {
		for _, tok := range []string{w.tokAp1(), w.tokAp2()} {
			if a := w.lcApprove(tok, f.ChangeRequest.ID); a.status != http.StatusOK {
				w.t.Fatalf("setup approve %s: %d %s", kind, a.status, a.body)
			}
		}
	}
}

type refusalWire struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	CheckCodes []string `json:"check_codes"`
}

func TestLicensingClosedRefusals_EveryReachablePrecheckOverHTTP(t *testing.T) {
	w := newLHWorld(t)
	jid := w.lcJurisdiction()
	tenA, tenB := w.activeTenant(), w.activeTenant() // both under_platform_licence
	lp := w.lcLicence(jid, "platform", "active")
	lSusp := w.lcLicence(jid, "platform", "suspended")
	lTen := w.lcLicence(jid, "tenant", "active")
	ownC, ownD := w.lcOwnLicenceTenant(), w.lcOwnLicenceTenant()

	// Real governed setup: bind tenant A to the platform licence; the licence enables MT; the own-licence tenant C holds the tenant licence.
	w.lcExecute("tenant_licence_bind", map[string]any{"tenant_id": tenA, "licence_id": lp})
	w.lcExecute("licence_ceiling_version", map[string]any{"licence_id": lp, "country_code": "MT", "state": "enabled", "status": "active", "authorization_reference": "C1-AUTH-1"})
	w.lcExecute("tenant_licence_bind", map[string]any{"tenant_id": ownC, "licence_id": lTen})

	brandB := w.activeBrand(tenB)
	_ = w.activeBrand(tenA)
	compA := w.tenantTok(tenA, w.staff(tenA, identity.StaffRoleCompliance), auth.RoleCompliance)
	adminA := w.tenantTok(tenA, w.staff(tenA, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)
	compB := w.tenantTok(tenB, w.staff(tenB, identity.StaffRoleCompliance), auth.RoleCompliance)
	plat := w.tokReq()
	unknown := uuid.New()

	policy := func(tenant uuid.UUID, scope string, brand any, country, state string) map[string]any {
		return map[string]any{"tenant_id": tenant, "scope": scope, "brand_id": brand, "operation_code": nil, "product_code": nil,
			"country_code": country, "state": state, "status": "active", "authorization_reference": "C1-REF"}
	}
	basis := map[string]any{"tenant_id": tenA, "brand_id": nil, "country_code": "MT", "operation_code": "deposit", "product_code": nil,
		"basis_kind": "no_licence_documented", "licence_id": nil, "jurisdiction_id": jid, "determination_reference": "DET-C1",
		"determined_by_role": "head_of_compliance", "basis_evidence_reference": nil, "restrictions_note": nil, "restricted_product_codes": []string{},
		"review_due_at": "2999-01-01"}
	cfg := func(tenant, jur uuid.UUID, currencies []string) map[string]any {
		return map[string]any{"tenant_id": tenant, "jurisdiction_id": jur, "action": "set", "kyc_ruleset_id": "kyc1", "aml_ruleset_id": "aml1",
			"rg_ruleset_id": "rg1", "reporting_ruleset_id": nil, "allowed_currencies": currencies, "allowed_payment_methods": []string{"card"}, "geo_block_list": []string{}}
	}

	cases := []struct {
		name   string
		token  string
		kind   string
		pay    map[string]any
		status int
		api    string
		code   string
	}{
		{"licence_create: unknown jurisdiction", plat, "licence_create", map[string]any{"jurisdiction_id": unknown, "licensee": "platform", "licence_number": "C1-X", "permitted_products": []string{}, "permitted_markets": []string{}}, 400, "validation_error", "jurisdiction_unknown"},
		{"bind: unknown tenant", plat, "tenant_licence_bind", map[string]any{"tenant_id": unknown, "licence_id": lp}, 404, "not_found", "tenant_unknown"},
		{"bind: unknown licence", plat, "tenant_licence_bind", map[string]any{"tenant_id": tenB, "licence_id": unknown}, 400, "validation_error", "licence_unknown"},
		{"bind: licence not active", plat, "tenant_licence_bind", map[string]any{"tenant_id": tenB, "licence_id": lSusp}, 400, "validation_error", "licence_not_active"},
		{"bind: licensee mismatch", plat, "tenant_licence_bind", map[string]any{"tenant_id": tenB, "licence_id": lTen}, 400, "validation_error", "licence_licensee_mismatch"},
		{"bind: tenant licence bound to another tenant", plat, "tenant_licence_bind", map[string]any{"tenant_id": ownD, "licence_id": lTen}, 400, "validation_error", "licence_bound_to_other_tenant"},
		{"unbind: unknown tenant", plat, "tenant_licence_unbind", map[string]any{"tenant_id": unknown}, 404, "not_found", "tenant_unknown"},
		{"ceiling: unknown licence", plat, "licence_ceiling_version", map[string]any{"licence_id": unknown, "country_code": "MT", "state": "enabled", "status": "active", "authorization_reference": "R"}, 400, "validation_error", "licence_unknown"},
		{"policy: unknown tenant", plat, "operating_policy_version", policy(unknown, "tenant", nil, "MT", "enabled"), 400, "validation_error", "tenant_unknown"},
		{"policy: no licence and no basis (platform)", plat, "operating_policy_version", policy(tenB, "tenant", nil, "MT", "enabled"), 409, "LICENSING_CHANGE_REFUSED", "tenant_without_licence_or_basis"},
		{"policy: no licence and no basis (own tenant staff)", compB, "operating_policy_version", policy(tenB, "tenant", nil, "MT", "enabled"), 409, "LICENSING_CHANGE_REFUSED", "tenant_without_licence_or_basis"},
		{"policy: ceiling missing for the country (platform)", plat, "operating_policy_version", policy(tenA, "tenant", nil, "DE", "enabled"), 409, "LICENSING_CHANGE_REFUSED", "ceiling_not_enabled_for_country"},
		{"policy: ceiling missing for the country (own tenant staff)", compA, "operating_policy_version", policy(tenA, "tenant", nil, "DE", "enabled"), 409, "LICENSING_CHANGE_REFUSED", "ceiling_not_enabled_for_country"},
		{"policy: foreign brand (tenant admin)", adminA, "operating_policy_version", policy(tenA, "brand", brandB, "MT", "enabled"), 404, "not_found", "brand_not_owned"},
		{"policy: unknown brand (tenant admin)", adminA, "operating_policy_version", policy(tenA, "brand", unknown, "MT", "enabled"), 404, "not_found", "brand_not_owned"},
		{"basis: no-licence basis clashes with the licence ceiling", plat, "operating_basis_set", basis, 400, "validation_error", "basis_clashes_with_licence_ceiling"},
		{"config: unknown tenant", plat, "tenant_jurisdiction_config_version", cfg(unknown, jid, []string{"EUR"}), 400, "validation_error", "tenant_unknown"},
		{"config: unknown jurisdiction", plat, "tenant_jurisdiction_config_version", cfg(tenA, unknown, []string{"EUR"}), 400, "validation_error", "jurisdiction_unknown"},
		{"config: currency not an active asset", plat, "tenant_jurisdiction_config_version", cfg(tenA, jid, []string{"ZZ9"}), 400, "validation_error", "currency_not_active_asset"},
		{"config: currency not an active asset (own tenant staff)", compA, "tenant_jurisdiction_config_version", cfg(tenA, jid, []string{"ZZ9"}), 400, "validation_error", "currency_not_active_asset"},
		{"status: unknown licence", plat, "licence_status_change", map[string]any{"licence_id": unknown, "status": "suspended"}, 404, "not_found", "licence_unknown"},
		{"status: unchanged", plat, "licence_status_change", map[string]any{"licence_id": lp, "status": "active"}, 400, "validation_error", "licence_status_unchanged"},
		{"validity dates: unchanged", plat, "licence_validity_dates_change", map[string]any{"licence_id": lp, "issued_at": nil, "expires_at": nil}, 400, "validation_error", "licence_validity_dates_unchanged"},
		{"review due: unchanged", plat, "licence_review_due_change", map[string]any{"licence_id": lp, "review_due_at": nil}, 400, "validation_error", "licence_review_due_unchanged"},
		{"evidence reference: unchanged", plat, "licence_evidence_reference_change", map[string]any{"licence_id": lp, "licence_evidence_reference": nil}, 400, "validation_error", "licence_evidence_reference_unchanged"},
	}
	seen := map[string]bool{}
	var foreignBrand, unknownBrand []byte
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := w.lcCounts()
			r := w.lcFile(c.token, c.kind, "c1_check", c.pay)
			if r.status != c.status {
				t.Fatalf("status %d want %d: %s", r.status, c.status, r.body)
			}
			var e refusalWire
			r.decode(t, &e)
			if !strings.EqualFold(e.Code, c.api) {
				t.Fatalf("api code %q want %q (unchanged generic code): %s", e.Code, c.api, r.body)
			}
			if len(e.CheckCodes) != 1 || e.CheckCodes[0] != c.code {
				t.Fatalf("check_codes %v want [%s]: %s", e.CheckCodes, c.code, r.body)
			}
			if _, ok := licensingchange.ClosedRefusals[c.code]; !ok {
				t.Fatalf("%s is not in the closed table", c.code)
			}
			for _, bad := range []string{"unknown ", "licence_id", "tenant_id", tenB.String(), brandB.String(), unknown.String(), "operating_", "ceiling", "SQLSTATE", "LC0"} {
				if strings.Contains(e.Message, bad) {
					t.Fatalf("the sentence leaks %q: %q", bad, e.Message)
				}
			}
			if after := w.lcCounts(); !equalCounts(before, after) {
				t.Fatalf("a refusal wrote something: before %v after %v", before, after)
			}
			seen[c.code] = true
			if c.name == "policy: foreign brand (tenant admin)" {
				foreignBrand = scrubRequestID(t, r.body)
			}
			if c.name == "policy: unknown brand (tenant admin)" {
				unknownBrand = scrubRequestID(t, r.body)
			}
		})
	}
	// Tenant A learns nothing about tenant B from a brand refusal: a foreign brand and an unknown one are the SAME answer.
	if string(foreignBrand) != string(unknownBrand) || len(foreignBrand) == 0 {
		t.Errorf("foreign brand and unknown brand must be indistinguishable:\n%s\n%s", foreignBrand, unknownBrand)
	}
	// Filing for another tenant's id is refused before any precheck, with no code and no other-tenant data.
	for _, other := range []uuid.UUID{tenB} {
		before := w.lcCounts()
		r := w.lcFile(compA, "operating_policy_version", "c1_check", policy(other, "tenant", nil, "MT", "enabled"))
		if r.status != http.StatusNotFound || strings.Contains(string(r.body), "check_codes") || strings.Contains(string(r.body), other.String()) {
			t.Errorf("cross-tenant filing: %d %s", r.status, r.body)
		}
		if after := w.lcCounts(); !equalCounts(before, after) {
			t.Errorf("cross-tenant filing wrote something")
		}
	}
	// A refusal is a refusal of the REQUEST: an authorisation answer carries no refusal code (tenant admin may not file a tenant-rung policy).
	if r := w.lcFile(adminA, "operating_policy_version", "c1_check", policy(tenA, "tenant", nil, "MT", "enabled")); r.status != http.StatusForbidden || strings.Contains(string(r.body), "check_codes") {
		t.Errorf("authorisation refusal: %d %s", r.status, r.body)
	}

	// Duplicate jurisdiction code: 400 validation_error (unchanged) plus the closed code; the second insert wrote nothing.
	code := "C1-" + strings.ToUpper(uuid.NewString()[:10])
	body := map[string]any{"code": code, "name": "c1 dup", "reason_code": "c1_check"}
	if r := w.do("POST", "/v1/admin/jurisdictions", plat, body); r.status != http.StatusCreated {
		t.Fatalf("first jurisdiction: %d %s", r.status, r.body)
	}
	before := w.lcCounts()
	r := w.do("POST", "/v1/admin/jurisdictions", plat, body)
	var e refusalWire
	r.decode(t, &e)
	if r.status != http.StatusBadRequest || e.Code != "validation_error" || len(e.CheckCodes) != 1 || e.CheckCodes[0] != "duplicate_jurisdiction_code" {
		t.Fatalf("duplicate jurisdiction: %d %s", r.status, r.body)
	}
	if after := w.lcCounts(); !equalCounts(before, after) {
		t.Fatalf("a duplicate wrote something: %v %v", before, after)
	}
	seen["duplicate_jurisdiction_code"] = true

	// Positive control: the counting is not vacuous (a request that is accepted changes the counts, and the tables are readable).
	c0 := w.lcCounts()
	if c0["licences"] == 0 || c0["licensing_change_requests"] == 0 || c0["licence_country_ceilings"] == 0 {
		t.Fatalf("the counters read nothing: %v", c0)
	}
	if r := w.lcFile(plat, "licence_status_change", "c1_control", map[string]any{"licence_id": lp, "status": "suspended"}); r.status != http.StatusCreated {
		t.Fatalf("control filing: %d %s", r.status, r.body)
	}
	if equalCounts(c0, w.lcCounts()) {
		t.Fatal("an accepted request changed no count: the nothing-written assertions would be vacuous")
	}

	// Report which closed codes the wire test provoked (the rest are execution-time or database guards: unit-tested through classify).
	var got []string
	for c := range seen {
		got = append(got, c)
	}
	sort.Strings(got)
	t.Logf("provoked over HTTP (%d of %d): %s", len(got), len(licensingchange.ClosedRefusals), strings.Join(got, ","))
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func scrubRequestID(t *testing.T, body []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "request_id")
	out, _ := json.Marshal(m)
	return out
}

// The execution-time guard LC024 (a tenant-requested narrowing that now widens) reaches the wire as 409 LICENSING_CHANGE_GUARD_REFUSED
// with the closed code change_now_widens; the request stays open and nothing is written.
func TestLicensingClosedRefusals_GuardChangeNowWidensOverHTTP(t *testing.T) {
	w := newLHWorld(t)
	jid := w.lcJurisdiction()
	ten := w.activeTenant()
	comp := w.tenantTok(ten, w.staff(ten, identity.StaffRoleCompliance), auth.RoleCompliance)
	cfg := func(kyc string, cur []string) map[string]any {
		return map[string]any{"tenant_id": ten, "jurisdiction_id": jid, "action": "set", "kyc_ruleset_id": kyc, "aml_ruleset_id": "aml1",
			"rg_ruleset_id": "rg1", "reporting_ruleset_id": nil, "allowed_currencies": cur, "allowed_payment_methods": []string{"card"}, "geo_block_list": []string{}}
	}
	w.lcExecute("tenant_jurisdiction_config_version", cfg("kyc1", []string{"EUR", "USD"}))
	if r := w.lcFile(w.tokReq(), "tenant_jurisdiction_config_version", "c1_narrow", cfg("kyc1", []string{"EUR"})); r.status != http.StatusCreated {
		t.Fatalf("platform narrowing: %d %s", r.status, r.body)
	}
	tr := w.lcFile(comp, "tenant_jurisdiction_config_version", "c1_tenant_narrow", cfg("kyc1", []string{}))
	var tres licensingChangeResponseIT
	tr.decode(t, &tres)
	if tr.status != http.StatusCreated || tres.Executed || tres.ChangeRequest.Widening || tres.ChangeRequest.RequiredApprovals != 1 {
		t.Fatalf("tenant narrowing: %d %s", tr.status, tr.body)
	}
	w.lcExecute("tenant_jurisdiction_config_version", cfg("kyc2", []string{"EUR"})) // a ruleset change widens

	before := w.lcCounts()
	r := w.lcApprove(w.tokAp1(), tres.ChangeRequest.ID)
	var e refusalWire
	r.decode(t, &e)
	if r.status != http.StatusConflict || e.Code != "LICENSING_CHANGE_GUARD_REFUSED" || len(e.CheckCodes) != 1 || e.CheckCodes[0] != "change_now_widens" {
		t.Fatalf("LC024 on the wire: %d %s", r.status, r.body)
	}
	after := w.lcCounts()
	// The approval row and the executed state rolled back; only the denied-decision audit row may be added.
	for k, v := range before {
		if k != "audit_log" && after[k] != v {
			t.Fatalf("%s changed on a refused approval: %d -> %d", k, v, after[k])
		}
	}
	g := w.do("GET", "/v1/admin/licensing-changes/"+tres.ChangeRequest.ID, w.tokAp1(), nil)
	var got licensingChangeResponseIT
	g.decode(t, &got)
	if g.status != http.StatusOK || got.ChangeRequest.Status != "requested" || len(got.ChangeRequest.Approvals) != 0 {
		t.Fatalf("the refused approval must leave the request open and unapproved: %d %s", g.status, g.body)
	}
}

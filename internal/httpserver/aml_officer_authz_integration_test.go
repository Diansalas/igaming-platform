//go:build integration

package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// c1 task 2: who can reach which layer of the screening configuration authority check, and what each actor class is answered.
//
// The layers, in the order a request meets them:
//   L1  staffRoute: the token (401), the step-up class (critical), then auth.RequirePermission(aml_hold:manage) (403, no body code)
//   L2  amlHandler: the path tenant must be the caller's own tenant, or the caller is a platform token (404 for a foreign tenant)
//   L3  the aml.Service session: a tenant principal session, or a platform_acting session in the path tenant
//       (db.WithPlatformActingInTenant: CG020 when the platform principal holds no in-force grant for the tenant)
//   L4  aml_authorize in the database: AM001 when the actor holds no in-force aml_officer grant of its scope, then the signed proof,
//       then AM002 (HINT platform_officer_required) when the session is not platform_acting, then AM003 when the quorum is short.
// platform_officer_required is therefore reachable on the wire ONLY by a tenant-scope aml_officer grant holder (a tenant `compliance`
// staff member whose grant was requested by a tenant admin and approved by a platform admin), which is what this test creates.

func c1PG(err error) (code, hint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.Hint
	}
	return "", ""
}

// c1Staff creates a person-linked staff account (owner-provisioned fixture).
func (w *amlHTTP) c1Staff(tenant uuid.UUID, role string) uuid.UUID {
	w.t.Helper()
	id, person := uuid.New(), uuid.New()
	insert := func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		var tid any
		if tenant != uuid.Nil {
			tid = tenant
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1, $2, $3, '!x', $4, 'active', $5)`,
			id, tid, "c1-"+id.String()+"@fixture.invalid", role, person)
		return err
	}
	var err error
	if tenant == uuid.Nil {
		err = w.owner.WithoutTenant(context.Background(), insert)
	} else {
		err = w.owner.WithTenant(context.Background(), tenant, insert)
	}
	if err != nil {
		w.t.Fatalf("seed staff: %v", err)
	}
	return id
}

// c1Count counts rows of the world's tenant (the AML tables are tenant-RLS: a platform-admin session reads none of them), through the
// table owner's pool. The intended-path subtests assert that a SUCCESS moves the count, so the "nothing written" checks are not vacuous.
func (w *amlHTTP) c1Count(q string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(&n)
	}); err != nil {
		w.t.Fatalf("count: %v", err)
	}
	return n
}

// c1Jurisdiction gives the tenant a jurisdiction it operates in (a configuration row) and binds a platform licence of it.
func (w *amlHTTP) c1Jurisdiction() uuid.UUID {
	w.t.Helper()
	jid, lic := uuid.New(), uuid.New()
	ctx := context.Background()
	if err := w.owner.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'c1 officer')`, jid, "C1O-"+jid.String()[:12]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`, lic, jid, "C1O-"+lic.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, w.tenant, lic)
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction: %v", err)
	}
	if err := w.owner.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods, effective_from)
			VALUES ($1, $2, 'kyc1', 'aml1', 'rg1', '["EUR"]', '["card"]', now() - interval '1 hour')`, w.tenant, jid)
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction config: %v", err)
	}
	return jid
}

func (w *amlHTTP) c1SetupTenantOfficer(tenant uuid.UUID, ta, platformApprover, grantee uuid.UUID) {
	w.t.Helper()
	taTok := w.api.token(ta, tenant, auth.RoleTenantAdmin)
	r := w.api.do("POST", "/v1/admin/tenants/"+tenant.String()+"/capability-grants/requests", taTok,
		map[string]any{"grantee_staff_id": grantee, "capability": "aml_officer", "reason_code": "c1_officer"})
	if r.status != http.StatusCreated {
		w.t.Fatalf("tenant-originated aml_officer request: %d %s", r.status, r.body)
	}
	var req struct {
		ID string `json:"id"`
	}
	r.decode(w.t, &req)
	a := w.api.do("POST", "/v1/admin/tenants/"+tenant.String()+"/capability-grants/requests/"+req.ID+"/approve",
		w.api.token(platformApprover, uuid.Nil, auth.RolePlatformAdmin), map[string]any{"reason_code": "c1_officer"})
	if a.status != http.StatusOK {
		w.t.Fatalf("platform approval of the tenant officer: %d %s", a.status, a.body)
	}
}

func c1PolicyBody(jid uuid.UUID) map[string]any {
	return map[string]any{"jurisdiction_id": jid, "lists": []string{"pep", "sanctions"}, "rescreen_interval_seconds": 7200, "backfill_grace_seconds": 3600,
		"effective_from": time.Now().Add(time.Minute).UTC().Format(time.RFC3339), "legal_review_reference": "C1-REVIEW-1", "reason_code": "c1_policy"}
}

func c1TriggerBody(jid uuid.UUID) map[string]any {
	return map[string]any{"jurisdiction_id": jid, "events": []string{"registration"}, "gate_withdrawal": false, "gate_alert_after_seconds": 0,
		"effective_from": time.Now().Add(time.Minute).UTC().Format(time.RFC3339), "legal_review_reference": "C1-REVIEW-2", "reason_code": "c1_trigger"}
}

// Static map: the only roles that pass layer L1 for the configuration routes are platform_admin and compliance. There is no other
// platform role (staff_users: role = 'platform_admin' AND tenant_id IS NULL, migration 0011), so "platform staff without the aml
// permission" cannot exist as an account; every other role of the permission map is refused at L1.
func TestC1_AMLConfigRoutes_StaticGateHoldsOnlyPlatformAdminAndCompliance(t *testing.T) {
	holders := map[auth.Role]bool{}
	for _, r := range []auth.Role{auth.RolePlatformAdmin, auth.RoleTenantAdmin, auth.RoleSupport, auth.RoleCompliance, auth.RoleFinance, auth.RolePlayer,
		auth.RoleRiskManager, auth.RolePromotionsManager, auth.RoleBonusOperations, auth.Role("unknown_role"), auth.Role("")} {
		if auth.RoleHasPermission(r, auth.PermAMLHoldManage) {
			holders[r] = true
		}
	}
	if len(holders) != 2 || !holders[auth.RolePlatformAdmin] || !holders[auth.RoleCompliance] {
		t.Fatalf("aml_hold:manage holders among the known roles: %v (want exactly platform_admin and compliance)", holders)
	}
}

func TestC1_PlatformOfficerRequired_ActorMatrixOverHTTP(t *testing.T) {
	w := newAMLHTTP(t, "under_platform_licence") // owner scope 'platform': the screening configuration is the platform officer's
	jid := w.c1Jurisdiction()
	ctx := context.Background()
	q := amlfix.Bootstrap(t, w.owner, w.tenant, 2) // two platform acting officers through the real K1 flow (the quorum)
	x1 := w.api.token(q.Officers[0].StaffID, uuid.Nil, auth.RolePlatformAdmin)

	// actors
	ta := w.c1Staff(w.tenant, "tenant_admin")
	c0 := w.c1Staff(w.tenant, "compliance")      // compliance without any grant
	ct := w.c1Staff(w.tenant, "compliance")      // compliance WITH a tenant-scope aml_officer grant (created below)
	png := w.c1Staff(uuid.Nil, "platform_admin") // platform admin WITHOUT the officer grant
	approver := w.c1Staff(uuid.Nil, "platform_admin")
	w.c1SetupTenantOfficer(w.tenant, ta, approver, ct)

	// foreign tenant Q with its own two platform officers
	var qTenant uuid.UUID
	if err := w.owner.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "c1q", "c1q-"+uuid.NewString(), "under_platform_licence")
		qTenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	qq := amlfix.Bootstrap(t, w.owner, qTenant, 2)
	foreignOfficer := w.api.token(qq.Officers[0].StaffID, uuid.Nil, auth.RolePlatformAdmin)
	foreignTenantStaff := w.api.token(w.c1Staff(qTenant, "compliance"), qTenant, auth.RoleCompliance)

	tokens := map[string]string{
		"tenant_admin":             w.api.token(ta, w.tenant, auth.RoleTenantAdmin),
		"tenant_compliance":        w.api.token(c0, w.tenant, auth.RoleCompliance),
		"tenant_officer_grant":     w.api.token(ct, w.tenant, auth.RoleCompliance),
		"platform_no_grant":        w.api.token(png, uuid.Nil, auth.RolePlatformAdmin),
		"foreign_platform_officer": foreignOfficer,
		"foreign_tenant_staff":     foreignTenantStaff,
		"platform_officer":         x1,
	}
	routes := []struct {
		name, path string
		body       map[string]any
		table      string
	}{
		{"screening policy", "/v1/admin/tenants/" + w.tenant.String() + "/screening/policy", c1PolicyBody(jid), "aml_screening_policies"},
		{"trigger policy", "/v1/admin/tenants/" + w.tenant.String() + "/screening/trigger-policy", c1TriggerBody(jid), "aml_screening_trigger_policies"},
	}
	type want struct {
		status int
		checks []string // exact check_codes
		layer  string   // documentation of which layer decided; asserted by the probes below
	}
	cases := []struct {
		actor string
		want  want
	}{
		{"tenant_admin", want{403, nil, "L1 route permission gate (tenant_admin lacks aml_hold:manage)"}},
		{"tenant_compliance", want{403, nil, "L4 database AM001 (no in-force aml_officer grant)"}},
		{"tenant_officer_grant", want{403, []string{"platform_officer_required"}, "L4 database AM002 HINT platform_officer_required"}},
		{"platform_no_grant", want{403, nil, "L3 session open CG020 (no in-force grant for the tenant)"}},
		{"foreign_platform_officer", want{403, nil, "L3 session open CG020 (an officer of another tenant holds no grant here)"}},
		{"foreign_tenant_staff", want{404, nil, "L2 amlHandler: a foreign tenant is not found"}},
	}
	for _, rt := range routes {
		before := w.c1Count(`SELECT count(*) FROM ` + rt.table)
		beforeGrants := w.c1Count(`SELECT count(*) FROM staff_capability_grants WHERE tenant_id = $1`, w.tenant)
		for _, c := range cases {
			t.Run(rt.name+"/"+c.actor, func(t *testing.T) {
				r := w.api.do("POST", rt.path, tokens[c.actor], rt.body)
				if r.status != c.want.status {
					t.Fatalf("status %d want %d (%s): %s", r.status, c.want.status, c.want.layer, r.body)
				}
				got := r.checkCodes(t)
				if strings.Join(got, ",") != strings.Join(c.want.checks, ",") {
					t.Fatalf("check_codes %v want %v (%s): %s", got, c.want.checks, c.want.layer, r.body)
				}
				if strings.Contains(string(r.body), "platform officer") || strings.Contains(string(r.body), "AM00") || strings.Contains(string(r.body), "grant") {
					t.Fatalf("the body names an internal: %s", r.body)
				}
				if n := w.c1Count(`SELECT count(*) FROM ` + rt.table); n != before {
					t.Fatalf("a refused request wrote a %s row: %d -> %d", rt.table, before, n)
				}
			})
		}
		// the intended path
		t.Run(rt.name+"/platform_officer_succeeds", func(t *testing.T) {
			r := w.api.do("POST", rt.path, tokens["platform_officer"], rt.body)
			if r.status != http.StatusCreated {
				t.Fatalf("platform admin acting as AML officer: %d %s", r.status, r.body)
			}
			if n := w.c1Count(`SELECT count(*) FROM ` + rt.table); n != before+1 {
				t.Fatalf("exactly one version expected: %d -> %d", before, n)
			}
		})
		if n := w.c1Count(`SELECT count(*) FROM staff_capability_grants WHERE tenant_id = $1`, w.tenant); n != beforeGrants {
			t.Fatalf("a configuration call changed the grants: %d -> %d", beforeGrants, n)
		}
	}

	// ---- layer evidence -------------------------------------------------------------------------------------------------
	t.Run("layer: the compliance actors pass L1 and L2 (a malformed body is a 400 from the handler, before any database call)", func(t *testing.T) {
		for _, a := range []string{"tenant_compliance", "tenant_officer_grant"} {
			if r := w.api.do("POST", routes[0].path, tokens[a], map[string]any{"jurisdiction_id": "not-a-uuid"}); r.status != http.StatusBadRequest {
				t.Errorf("%s: %d %s (L1/L2 must have passed)", a, r.status, r.body)
			}
		}
		// tenant_admin is refused at L1 even with a malformed body
		if r := w.api.do("POST", routes[0].path, tokens["tenant_admin"], map[string]any{"jurisdiction_id": "not-a-uuid"}); r.status != http.StatusForbidden {
			t.Errorf("tenant_admin: %d %s (L1 must refuse before the body is read)", r.status, r.body)
		}
	})
	t.Run("layer: the database SQLSTATE of each actor class", func(t *testing.T) {
		raw := func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT aml_screening_policy_author($1, $2, ARRAY['pep'], 7200, 0, now() + interval '1 minute', 'REF', 'r_1')`, w.tenant, jid)
			return err
		}
		// no grant: aml_authorize raises AM001 before the proof is even looked at
		err := w.rt.WithPrincipalScope(ctx, w.tenant, c0, func(ctx context.Context, tx pgx.Tx) error { return raw(tx) })
		if code, _ := c1PG(err); code != "AM001" {
			t.Errorf("tenant compliance without a grant: %v, want AM001", err)
		}
		// a tenant-scope grant holder passes the grant check and stops at the proof when none is attached (AP class), which proves
		// aml_authorize accepted the grant; the AM002 branch is reached through the service, which signs the proof (below)
		err = w.rt.WithPrincipalScope(ctx, w.tenant, ct, func(ctx context.Context, tx pgx.Tx) error { return raw(tx) })
		if code, _ := c1PG(err); code == "AM001" || code == "" {
			t.Errorf("tenant-scope officer must pass the grant check, got %v", err)
		}
		// platform principals: the acting session cannot be opened without an in-force grant for the tenant
		for name, staff := range map[string]uuid.UUID{"platform admin without the officer grant": png, "an officer of another tenant": qq.Officers[0].StaffID} {
			err = w.rt.WithPlatformActingInTenant(ctx, staff, w.tenant, uuid.Nil, "compliance", func(ctx context.Context, tx pgx.Tx) error { return nil })
			if code, _ := c1PG(err); code != "CG020" {
				t.Errorf("%s: %v, want CG020 at session open", name, err)
			}
		}
		// the officer's own acting session opens
		if err := w.rt.WithPlatformActingInTenant(ctx, q.Officers[0].StaffID, w.tenant, uuid.Nil, "compliance", func(ctx context.Context, tx pgx.Tx) error { return nil }); err != nil {
			t.Errorf("the officer's acting session: %v", err)
		}
	})
	t.Run("layer: the service answer of the tenant-scope grant holder is ErrForbidden with the closed code (AM002 + HINT)", func(t *testing.T) {
		svc := amlfix.Service(t, w.rt)
		in := aml.SetScreeningPolicyInput{JurisdictionID: jid, Lists: []aml.ListType{"pep"}, RescreenInterval: 2 * time.Hour, EffectiveFrom: time.Now().Add(time.Minute),
			LegalReviewReference: "C1-REVIEW", ReasonCode: "c1_policy"}
		_, err := svc.AuthorScreeningPolicy(ctx, aml.Actor{StaffID: ct, TenantID: w.tenant, Scope: aml.ScopeTenant}, in)
		if !errors.Is(err, aml.ErrForbidden) || aml.RefusalCode(err) != "platform_officer_required" {
			t.Fatalf("tenant-scope officer: %v (code %q)", err, aml.RefusalCode(err))
		}
	})

	// ---- the other AM002 sites (uncoded): hold routes, tenant-scope officer on a platform-owned tenant ------------------------
	t.Run("holds: a tenant-scope officer on a platform-owned tenant cannot place or see platform-owned holds", func(t *testing.T) {
		person, _ := w.subject()
		hold := w.api.do("POST", w.base()+"/holds", x1, map[string]any{"person_id": person, "scope": "withdrawal_and_payout", "reason_class": "manual", "reason_code": "c1_hold"})
		if hold.status != http.StatusCreated {
			t.Fatalf("the platform officer places a hold (intended path): %d %s", hold.status, hold.body)
		}
		var h amlID
		hold.decode(t, &h)
		holds := w.c1Count(`SELECT count(*) FROM aml_holds WHERE tenant_id = $1`, w.tenant)
		ctTok := tokens["tenant_officer_grant"]
		if r := w.api.do("POST", w.base()+"/holds", ctTok, map[string]any{"person_id": person, "scope": "withdrawal_and_payout", "reason_class": "manual", "reason_code": "c1_hold"}); r.status != http.StatusForbidden || len(r.checkCodes(t)) != 0 {
			t.Errorf("tenant-scope officer places a hold: %d %s", r.status, r.body)
		}
		lift := w.api.do("POST", w.base()+"/holds/"+h.HoldID+"/lift", ctTok, map[string]any{"reason_code": "c1_lift"})
		if lift.status != http.StatusNotFound && lift.status != http.StatusForbidden {
			t.Errorf("tenant-scope officer lifts a platform-owned hold: %d %s", lift.status, lift.body)
		}
		t.Logf("tenant-scope officer lift of a platform-owned hold answered %d %s", lift.status, lift.body)
		if n := w.c1Count(`SELECT count(*) FROM aml_holds WHERE tenant_id = $1`, w.tenant); n != holds {
			t.Errorf("holds changed: %d -> %d", holds, n)
		}
		var lifted int
		lifted = w.c1Count(`SELECT count(*) FROM aml_hold_lifts WHERE hold_id = $1`, uuid.MustParse(h.HoldID))
		if lifted != 0 {
			t.Errorf("the platform-owned hold was lifted by a tenant-scope officer")
		}
	})
}

// An own-licence tenant (owner scope 'tenant') with a full tenant-scope officer quorum: its officers run the case and hold work of
// the tenant, but the screening configuration is the platform officer's, so even a quorate tenant officer is refused with the closed
// code and nothing is written. An unauthenticated caller never reaches a handler.
func TestC1_PlatformOfficerRequired_TenantOwnedScopeQuorateOfficersAreStillRefused(t *testing.T) {
	w := newAMLHTTP(t, "own_licence")
	if w.scope != "tenant" {
		t.Fatalf("owner scope %q", w.scope)
	}
	jid := w.c1Jurisdictionless()
	q := amlfix.Bootstrap(t, w.owner, w.tenant, 2)
	for i, o := range q.Officers {
		tok := w.officerToken(o)
		for _, rt := range []struct {
			path, table string
			body        map[string]any
		}{
			{"/v1/admin/tenants/" + w.tenant.String() + "/screening/policy", "aml_screening_policies", c1PolicyBody(jid)},
			{"/v1/admin/tenants/" + w.tenant.String() + "/screening/trigger-policy", "aml_screening_trigger_policies", c1TriggerBody(jid)},
		} {
			before := w.c1Count(`SELECT count(*) FROM ` + rt.table)
			r := w.api.do("POST", rt.path, tok, rt.body)
			if got := r.checkCodes(t); r.status != http.StatusForbidden || len(got) != 1 || got[0] != "platform_officer_required" {
				t.Fatalf("officer %d %s: %d %s", i, rt.path, r.status, r.body)
			}
			if n := w.c1Count(`SELECT count(*) FROM ` + rt.table); n != before {
				t.Fatalf("%s written: %d -> %d", rt.table, before, n)
			}
			if r := w.api.do("POST", rt.path, "", rt.body); r.status != http.StatusUnauthorized {
				t.Fatalf("no token: %d %s", r.status, r.body)
			}
		}
	}
	// The same officers DO work the tenant's own scope (a hold), so the refusal above is about the configuration authority only.
	person, _ := w.subject()
	if r := w.api.do("POST", w.base()+"/holds", w.officerToken(q.Officers[0]), map[string]any{"person_id": person, "scope": "withdrawal_and_payout", "reason_class": "manual", "reason_code": "c1_hold"}); r.status != http.StatusCreated {
		t.Fatalf("tenant-scope officer places a hold in a tenant-owned scope: %d %s", r.status, r.body)
	}
}

// c1Jurisdictionless seeds a jurisdiction and the tenant's configuration row for it WITHOUT binding a platform licence (the tenant
// holds its own licence).
func (w *amlHTTP) c1Jurisdictionless() uuid.UUID {
	w.t.Helper()
	jid := uuid.New()
	ctx := context.Background()
	if err := w.owner.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'c1 own')`, jid, "C1P-"+jid.String()[:12])
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	if err := w.owner.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods, effective_from)
			VALUES ($1, $2, 'kyc1', 'aml1', 'rg1', '["EUR"]', '["card"]', now() - interval '1 hour')`, w.tenant, jid)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return jid
}

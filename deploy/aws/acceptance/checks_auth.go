//go:build stagingacceptance

package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

func (w *world) staffAuthChecks() {
	admin := w.actor("ADMIN")
	w.check("auth.staff_password_only_yields_no_token", func(t *T) {
		r := t.call("POST", "/v1/staff/auth/login", "", admin.loginBody())
		if r == nil {
			return
		}
		body := string(r.Body)
		if strings.Contains(body, "access_token") || strings.Contains(body, "refresh_token") {
			t.fail("a correct password alone returned a token pair (HTTP %d): MFA is bypassable", r.Status)
			return
		}
		switch {
		case r.Status == 200 && pickStr(r.obj(), "challenge", "type") == "mfa":
			t.pass("password step returns an MFA challenge only (methods=%v)", pick(r.obj(), "challenge", "methods"))
		case r.Status == 403 && r.code() == "mfa_enrolment_required":
			t.pass("password step refused with mfa_enrolment_required (no factor yet): no token without a second factor")
		default:
			t.fail("unexpected password-step answer: %s", r.brief())
		}
	})
	w.check("auth.staff_wrong_password_rejected", func(t *T) {
		// One attempt with a wrong password against a throwaway identity (never the real admin: no lockout side effect).
		b := map[string]any{"email": w.email("nobody-staff"), "password": "definitely-not-the-password-" + w.cfg.RunID}
		r := t.call("POST", "/v1/staff/auth/login", "", b)
		if !t.expect(r, 401, "", "staff login with unknown identity") {
			return
		}
		t.pass("unknown identity answers 401 with no token")
	})
	w.check("auth.staff_login_mfa", func(t *T) {
		if err := admin.signIn(); err != nil {
			t.fail("%v", err)
			return
		}
		d := "platform admin signed in with password + TOTP (aal2)"
		if admin.enrolled {
			d += "; first-time enrolment performed, TOTP secret and recovery codes stored in the 0600 files named by ADMIN_TOTP_SECRET_FILE (not printed)"
		}
		t.pass("%s", d)
	})
	w.check("auth.staff_mfa_status", func(t *T) {
		if !t.need("auth.staff_login_mfa") {
			return
		}
		r := t.acall("ADMIN", "GET", "/v1/staff/me/mfa", nil)
		if !t.expect(r, 200, "", "GET /v1/staff/me/mfa") {
			return
		}
		m := r.obj()
		if enrolled, _ := m["enrolled"].(bool); !enrolled {
			t.fail("status says not enrolled right after an MFA sign-in: %s", r.brief())
			return
		}
		if locked, _ := m["hard_locked"].(bool); locked {
			t.fail("factor is hard-locked")
			return
		}
		t.pass("enrolled=true factor_type=%v recovery_codes_remaining=%v", m["factor_type"], m["recovery_codes_remaining"])
	})
	w.check("auth.stepup_critical_route", func(t *T) {
		if !t.need("auth.staff_login_mfa") {
			return
		}
		if err := admin.stepUp(); err != nil {
			t.fail("%v", err)
			return
		}
		// POST /v1/admin/tenants is a critical-class route. An EMPTY body is rejected by validation AFTER the step-up gate:
		// 400 proves the gate let the request through and nothing was created; 403 step_up_required would be the gate.
		r := t.acall("ADMIN", "POST", "/v1/admin/tenants", map[string]any{})
		if r == nil {
			return
		}
		if r.Status == 403 {
			t.fail("critical route still demands a step-up right after a successful one: %s", r.brief())
			return
		}
		if r.Status != 400 {
			t.fail("expected validation 400 (gate passed, nothing created), got %s", r.brief())
			return
		}
		t.pass("step-up accepted (fresh critical window); critical route passed the gate and refused the empty body with 400")
	})
	w.check("auth.stepup_unauthenticated_refused", func(t *T) {
		r := t.call("POST", "/v1/staff/auth/step-up", "", map[string]any{"code": "000000"})
		if r == nil {
			return
		}
		if r.Status != 401 {
			t.fail("step-up without a session answered %s", r.brief())
			return
		}
		t.pass("step-up without a staff session is 401")
	})
	w.check("auth.staff_token_denied_player_routes", func(t *T) {
		if !t.need("auth.staff_login_mfa") {
			return
		}
		r := t.acall("ADMIN", "GET", "/v1/me/wallets", nil)
		if r == nil {
			return
		}
		if r.Status != 403 && r.Status != 401 {
			t.fail("a staff token on a player route answered %s", r.brief())
			return
		}
		t.pass("staff token refused on a player route (HTTP %d)", r.Status)
	})
}

// readiness is the last launch-readiness evaluation of the synthetic brand (check code -> result).
type readinessView struct {
	env    string
	checks map[string]string
	detail map[string]string
}

func (w *world) setupChecks() {
	w.check("setup.resolve_tenant_brand", func(t *T) {
		if !t.need("auth.staff_login_mfa") {
			return
		}
		r := t.acall("ADMIN", "GET", "/v1/admin/tenants?limit=200&q="+url.QueryEscape(w.cfg.Tenant), nil)
		if !t.expect(r, 200, "", "list tenants") {
			return
		}
		for _, it := range items(r) {
			if pickStr(it, "slug") == w.cfg.Tenant {
				w.tenantID = pickStr(it, "id")
				w.facts["tenant_status"] = pickStr(it, "status")
			}
		}
		if w.tenantID == "" {
			t.fail("tenant slug %q not found: create/launch the synthetic tenant first (plan section 6.5, README prerequisites)", w.cfg.Tenant)
			return
		}
		rb := t.acall("ADMIN", "GET", "/v1/admin/tenants/"+w.tenantID+"/brands", nil)
		if !t.expect(rb, 200, "", "list brands") {
			return
		}
		for _, it := range items(rb) {
			if pickStr(it, "slug") == w.cfg.Brand {
				w.brandID = pickStr(it, "id")
				w.facts["brand_status"] = pickStr(it, "status")
			}
		}
		if w.brandID == "" {
			t.fail("brand slug %q not found under tenant %q", w.cfg.Brand, w.cfg.Tenant)
			return
		}
		if w.facts["tenant_status"] != "active" || w.facts["brand_status"] != "active" {
			t.fail("tenant status=%s brand status=%s: both must be active (launch governance, plan section 6.5) before players can register", w.facts["tenant_status"], w.facts["brand_status"])
			return
		}
		t.pass("tenant %s (%s) and brand %s (%s) found and active", w.cfg.Tenant, w.tenantID, w.cfg.Brand, w.brandID)
	})
	for _, role := range []string{"ADMIN2", "TENANT_ADMIN", "FINANCE1", "FINANCE2"} {
		role := role
		w.check("setup.signin_"+strings.ToLower(role), func(t *T) {
			a := w.actor(role)
			if a == nil {
				t.skipPrereq("%s is not configured (%s_EMAIL, %s_PASSWORD_FILE, %s_TOTP_SECRET_FILE[, %s_BOOTSTRAP_TOKEN_FILE]); %s", role, role, role, role, role, roleNeed(role))
				return
			}
			if a.spec.TOTPFile == "" {
				t.skipPrereq("%s_TOTP_SECRET_FILE is not set", role)
				return
			}
			if err := a.signIn(); err != nil {
				t.fail("%v", err)
				return
			}
			d := role + " signed in with password + TOTP"
			if a.enrolled {
				d += " (enrolled this run; secret stored in file)"
			}
			t.pass("%s", d)
		})
	}
	w.check("setup.payments_capability", func(t *T) {
		if !w.cfg.Configure {
			t.skipPrereq("CONFIGURE=false: payments capability expected to be pre-configured")
			return
		}
		if !t.need("setup.signin_tenant_admin") {
			return
		}
		a := w.actor("TENANT_ADMIN")
		if err := a.freshCritical(); err != nil {
			t.fail("%v", err)
			return
		}
		body := map[string]any{
			"supported_fiat_currencies": w.assets(), "supported_payment_methods": []string{"card"},
			"supports_deposit": true, "supports_withdrawal": true, "priority": 0, "status": "active",
		}
		r := t.acall("TENANT_ADMIN", "PUT", "/v1/admin/providers/mock-payments/capability", body)
		if r == nil {
			return
		}
		if !r.ok() {
			t.fail("PUT mock payments capability: %s", r.brief())
			return
		}
		t.pass("mock-payments capability set for %v (card, deposit+withdrawal), idempotent PUT", w.assets())
	})
	w.check("setup.casino_capability", func(t *T) {
		if !w.cfg.Configure {
			t.skipPrereq("CONFIGURE=false: casino capability expected to be pre-configured")
			return
		}
		if !t.need("setup.signin_tenant_admin") {
			return
		}
		if err := w.actor("TENANT_ADMIN").freshCritical(); err != nil {
			t.fail("%v", err)
			return
		}
		body := map[string]any{
			"supports_catalogue": true, "supports_launch": true, "supports_balance": true, "supports_bet": true, "supports_win": true,
			"supports_rollback": true, "supported_assets": w.assets(), "supported_game_types": []string{"slot", "table", "live"},
			"priority": 0, "status": "active",
		}
		r := t.acall("TENANT_ADMIN", "PUT", "/v1/admin/casino/providers/mock-casino/capability", body)
		if r == nil {
			return
		}
		if !r.ok() {
			t.fail("PUT mock-casino capability: %s", r.brief())
			return
		}
		t.pass("mock-casino capability set (launch, bet, win, rollback), idempotent PUT")
	})
	w.check("setup.casino_catalogue", func(t *T) {
		if !w.cfg.Configure {
			t.skipPrereq("CONFIGURE=false: catalogue expected to be pre-configured")
			return
		}
		if !t.need("auth.staff_login_mfa", "setup.resolve_tenant_brand") {
			return
		}
		body := map[string]any{
			"provider_id": "mock-casino", "provider_game_id": "acceptance-slot-1", "name": "Acceptance Slot", "game_type": "slot",
			"supported_assets": w.assets(), "mobile_supported": true, "demo_supported": false, "status": "active",
		}
		r := t.acall("ADMIN", "PUT", "/v1/admin/casino/games", body)
		if r == nil {
			return
		}
		if !r.ok() {
			t.fail("PUT /v1/admin/casino/games: %s", r.brief())
			return
		}
		gid := pickStr(r.obj(), "id")
		if gid == "" {
			t.fail("catalogue upsert returned no game id: %s", r.brief())
			return
		}
		w.facts["game_id"] = gid
		if w.actor("TENANT_ADMIN") == nil {
			t.skipPrereq("game %s upserted in the platform catalogue, but TENANT_ADMIN is not configured to opt the tenant in", gid)
			return
		}
		if err := w.actor("TENANT_ADMIN").signIn(); err != nil {
			t.fail("%v", err)
			return
		}
		ra := t.acall("TENANT_ADMIN", "PUT", "/v1/admin/casino/games/"+gid+"/availability", map[string]any{"enabled": true})
		if ra == nil {
			return
		}
		if !ra.ok() {
			t.fail("opt the tenant into game %s: %s", gid, ra.brief())
			return
		}
		t.pass("game %s in the platform catalogue and enabled for the tenant", gid)
	})
}

func roleNeed(role string) string {
	switch role {
	case "ADMIN2":
		return "needed for four-eyes on platform actions (kill-switch release, launch) and tenant-B provisioning"
	case "TENANT_ADMIN":
		return "needed to apply provider capabilities and the casino opt-in (or pre-configure them and set CONFIGURE=false)"
	case "FINANCE1", "FINANCE2":
		return "both needed for the withdrawal four-eyes flow (two distinct tenant finance staff with distinct Persons)"
	}
	return ""
}

func (w *world) platformChecks() {
	w.check("platform.version_evidence", func(t *T) {
		t.skipManual("platform-api exposes no build/version endpoint; startup evidence is the `starting platform-api environment=...` log line and the ECS task image digest (README: CloudWatch queries). /readyz 200 proves the process is up with a reachable database")
	})
	w.check("platform.migration_head", func(t *T) {
		t.skipManual("no HTTP surface reports the applied migration head; evidence is the migrate task log (`migrate status` / `migrate verify`, plan 6.1). The suite cannot read the database")
	})
	var view *readinessView
	w.check("platform.launch_readiness", func(t *T) {
		if !t.need("setup.resolve_tenant_brand") {
			return
		}
		r := t.acall("ADMIN", "GET", "/v1/admin/tenants/"+w.tenantID+"/launch-readiness?action=activate&brand_id="+w.brandID, nil)
		if !t.expect(r, 200, "", "launch-readiness") {
			return
		}
		m := r.obj()
		rd, _ := m["readiness"].(map[string]any)
		if rd == nil || m["disclaimer"] == nil {
			t.fail("response lacks readiness or the fixed disclaimer: %s", r.brief())
			return
		}
		v := &readinessView{env: pickStr(rd, "environment"), checks: map[string]string{}, detail: map[string]string{}}
		cs, _ := rd["checks"].([]any)
		for _, c := range cs {
			code := pickStr(c, "code")
			v.checks[code] = pickStr(c, "result")
			v.detail[code] = pickStr(c, "detail_code")
		}
		if len(v.checks) == 0 {
			t.fail("no readiness checks returned")
			return
		}
		view = v
		keys := make([]string, 0, len(v.checks))
		for k := range v.checks {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v.checks[k]))
		}
		w.facts["readiness_snapshot_version"] = fmt.Sprint(rd["version"])
		t.pass("%d checks: %s", len(keys), strings.Join(parts, " "))
	})
	w.check("platform.environment_anchor", func(t *T) {
		if !t.need("platform.launch_readiness") {
			return
		}
		if view.env != w.cfg.ExpectEnv {
			t.fail("the running process reports APP_ENV=%q, expected %q (EXPECT_APP_ENV): wrong stack?", view.env, w.cfg.ExpectEnv)
			return
		}
		t.pass("platform-api reports environment=%s (matches EXPECT_APP_ENV); the database_environment anchor itself is read only by migrate/restore tooling", view.env)
	})
	w.check("platform.readiness_alerts_item", func(t *T) {
		if !t.need("platform.launch_readiness") {
			return
		}
		res, ok := view.checks["R-ALERTS"]
		if !ok {
			t.fail("R-ALERTS missing from the readiness checks")
			return
		}
		// R-ALERTS passes only with approved p1 AND p2 human-notification routes (plan 10.2). Before the owner authors
		// them it is expected not to pass; report the state without claiming alert delivery.
		if res == "pass" {
			t.pass("R-ALERTS=pass: approved p1 and p2 routes exist (delivery itself is verified by the operator at the receiver)")
			return
		}
		t.skipPrereq("R-ALERTS=%s (%s): p1/p2 alert routes not yet authored and approved by two platform admins (plan 10.2, owner action)", res, view.detail["R-ALERTS"])
	})
	w.check("platform.readiness_market_config", func(t *T) {
		if !t.need("platform.launch_readiness") {
			return
		}
		res := view.checks["R-MARKET-CONFIG"]
		switch w.cfg.MarketExpect {
		case "open":
			if res != "pass" {
				t.fail("R-MARKET-CONFIG=%s (%s): the operating-market rows for this brand are missing or closed (plan owner action C); registration/deposit/launch will be refused", res, view.detail["R-MARKET-CONFIG"])
				return
			}
			t.pass("R-MARKET-CONFIG=pass: a country is open for registration, deposit and casino wagering")
		default:
			if res == "pass" {
				t.fail("MARKET_EXPECT=closed but R-MARKET-CONFIG=pass")
				return
			}
			t.pass("R-MARKET-CONFIG=%s (%s) as expected for a closed market", res, view.detail["R-MARKET-CONFIG"])
		}
	})
}

// waitUntil sleeps until the clock reaches t.
func (w *world) waitUntil(until time.Time) {
	if d := until.Sub(w.now()); d > 0 {
		w.sleep(d)
	}
}

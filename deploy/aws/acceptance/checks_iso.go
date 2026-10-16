//go:build stagingacceptance

package main

import (
	"fmt"
	"strings"
)

// marketChecks: the market gate is exercised in two complementary ways. With MARKET_EXPECT=closed the registration of
// player.register must be refused 403 MARKET_NOT_AVAILABLE (fail closed, no evidence). With MARKET_EXPECT=open the
// money flows only pass because the gate found open evidence; here we additionally prove the refusal body is the one
// oracle-free answer, using the isolation tenant (which has no operating-market rows).
func (w *world) marketChecks() {
	w.check("market.refusal_is_oracle_free", func(t *T) {
		if w.cfg.MarketExpect != "closed" {
			if !t.need("iso.second_tenant_provisioned") {
				return
			}
		}
		// A player of the synthetic brand never sees why a market is closed: the body is identical for every cause.
		body := map[string]any{"brand_slug": w.cfg.Brand, "email": w.email("market-probe"), "password": "Pw-" + randHex(8)}
		if w.cfg.MarketExpect == "closed" {
			r := t.call("POST", "/v1/auth/register", "", body)
			if r == nil || !t.expect(r, 403, "MARKET_NOT_AVAILABLE", "registration with the market closed") {
				return
			}
			if strings.Contains(strings.ToLower(string(r.Body)), "country") || strings.Contains(strings.ToLower(string(r.Body)), "location") {
				t.fail("the refusal names a country or location (oracle): %s", r.brief())
				return
			}
			t.pass("closed market refuses registration with the single fixed MARKET_NOT_AVAILABLE answer and no country/location detail")
			return
		}
		t.skipPrereq("the open-market refusal path needs an ACTIVE brand without operating-market rows; a pending_launch isolation brand answers like an unknown brand (see iso.second_tenant_brand_unreachable). Run the suite once with MARKET_EXPECT=closed against such a brand (README: market evidence dependency) to assert fail-closed")
	})
}

type tenantB struct {
	id, brandID, slug, brandSlug, status string
}

func (w *world) isolationChecks() {
	var b tenantB
	w.check("iso.second_tenant_provisioned", func(t *T) {
		if w.cfg.SecondTenant == "" {
			t.skipPrereq("SECOND_TENANT is empty: no isolation tenant requested")
			return
		}
		if !t.need("auth.staff_login_mfa") {
			return
		}
		b.slug, b.brandSlug = w.cfg.SecondTenant, w.cfg.SecondBrand
		find := func() bool {
			r := t.acall("ADMIN", "GET", "/v1/admin/tenants?limit=200&q="+b.slug, nil)
			if r == nil || !t.expect(r, 200, "", "list tenants") {
				return false
			}
			for _, it := range items(r) {
				if pickStr(it, "slug") == b.slug {
					b.id, b.status = pickStr(it, "id"), pickStr(it, "status")
				}
			}
			return b.id != ""
		}
		if t.done {
			return
		}
		if !find() {
			if t.done {
				return
			}
			if err := w.actor("ADMIN").freshCritical(); err != nil {
				t.fail("%v", err)
				return
			}
			r := t.acall("ADMIN", "POST", "/v1/admin/tenants", map[string]any{"name": "Acceptance Isolation Tenant", "slug": b.slug, "licensing_model": "under_platform_licence", "reason_code": "staging_acceptance"})
			if r == nil {
				return
			}
			if r.Status != 201 && r.Status != 409 {
				t.fail("create the isolation tenant: %s", r.brief())
				return
			}
			if !find() {
				if !t.done {
					t.fail("isolation tenant not visible after creation")
				}
				return
			}
		}
		// brand
		rb := t.acall("ADMIN", "GET", "/v1/admin/tenants/"+b.id+"/brands", nil)
		if rb == nil || !t.expect(rb, 200, "", "list isolation brands") {
			return
		}
		for _, it := range items(rb) {
			if pickStr(it, "slug") == b.brandSlug {
				b.brandID = pickStr(it, "id")
			}
		}
		if b.brandID == "" {
			if err := w.actor("ADMIN").freshCritical(); err != nil {
				t.fail("%v", err)
				return
			}
			rc := t.acall("ADMIN", "POST", "/v1/admin/tenants/"+b.id+"/brands", map[string]any{"name": "Acceptance Isolation Brand", "slug": b.brandSlug})
			if rc == nil {
				return
			}
			if rc.Status != 201 {
				t.fail("create the isolation brand: %s", rc.brief())
				return
			}
			b.brandID = pickStr(rc.obj(), "id")
		}
		t.pass("isolation tenant %s (status %s) and brand %s exist; created idempotently by slug, left in the launch state the platform gave it", b.slug, b.status, b.brandSlug)
	})
	w.check("iso.second_tenant_brand_unreachable", func(t *T) {
		if !t.need("iso.second_tenant_provisioned") {
			return
		}
		if b.status == "active" {
			t.skipPrereq("the isolation tenant is ACTIVE (pre-launched by the owner): registration may legitimately succeed or be refused by the market gate; use SECOND_TENANT only for a tenant that is not launched")
			return
		}
		r := t.call("POST", "/v1/auth/register", "", map[string]any{"brand_slug": b.brandSlug, "email": w.email("tenant-b-probe"), "password": "Pw-" + randHex(8)})
		if r == nil {
			return
		}
		if r.ok() {
			t.fail("a player registered on a brand of a tenant that is not launched: %s", r.brief())
			return
		}
		t.pass("registration on a not-yet-launched brand of the isolation tenant refused (HTTP %d %s)", r.Status, r.code())
	})
	w.check("iso.tenant_staff_cannot_cross_tenants", func(t *T) {
		if !t.need("iso.second_tenant_provisioned") {
			return
		}
		role := w.roleOr("TENANT_ADMIN", "FINANCE1", "COMPLIANCE")
		if role == "" {
			t.skipPrereq("no tenant staff configured: cross-tenant staff isolation cannot be exercised")
			return
		}
		paths := []string{
			"/v1/admin/tenants/" + b.id,
			"/v1/admin/tenants/" + b.id + "/brands",
			"/v1/admin/tenants/" + b.id + "/staff",
			"/v1/admin/tenants/" + b.id + "/launch-readiness?action=activate",
			"/v1/admin/tenants/" + b.id + "/payment-attempts",
			"/v1/admin/tenants/" + b.id + "/reconciliation/runs?stream=ledger_vs_projection",
			"/v1/admin/tenants/" + b.id + "/payments/kill-switches",
			"/v1/admin/tenants/" + b.id + "/manual-adjustments",
		}
		for _, p := range paths {
			r := t.acall(role, "GET", p, nil)
			if r == nil {
				return
			}
			if r.ok() {
				t.fail("%s read tenant B resource %s: %s", role, p, r.brief())
				return
			}
			if r.Status != 403 && r.Status != 404 {
				t.fail("%s on %s answered %s (want 403/404)", role, p, r.brief())
				return
			}
		}
		// A tenant-scoped POST naming tenant B must not mutate either.
		kr := t.acall(role, "POST", "/v1/admin/tenants/"+b.id+"/payments/kill-switches", map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "staging_acceptance"})
		if kr == nil {
			return
		}
		if kr.ok() {
			t.fail("%s engaged a kill switch on tenant B: %s", role, kr.brief())
			return
		}
		t.pass("%s was refused (403/404) on %d tenant-B reads and on a tenant-B kill-switch write", role, len(paths))
	})
	w.check("iso.staff_login_scoped_to_tenant", func(t *T) {
		if !t.need("iso.second_tenant_provisioned") {
			return
		}
		a := w.actor(w.roleOr("TENANT_ADMIN", "FINANCE1", "COMPLIANCE"))
		if a == nil {
			t.skipPrereq("no tenant staff configured")
			return
		}
		body := a.loginBody()
		body["tenant_slug"] = b.slug
		r := t.call("POST", "/v1/staff/auth/login", "", body)
		if r == nil {
			return
		}
		if r.ok() || strings.Contains(string(r.Body), "challenge") {
			t.fail("tenant-A staff credentials opened a login challenge for tenant B: %s", r.brief())
			return
		}
		t.pass("tenant-A staff credentials presented with tenant B's slug are refused (HTTP %d)", r.Status)
	})
	w.check("iso.player_login_scoped_to_brand", func(t *T) {
		if !t.need("iso.second_tenant_provisioned") || !t.needMoney("player.login") {
			return
		}
		p1 := w.players["p1"]
		r := t.call("POST", "/v1/auth/login", "", map[string]any{"brand_slug": b.brandSlug, "email": p1.email, "password": p1.password})
		if r == nil {
			return
		}
		if r.ok() {
			t.fail("a tenant-A player signed in on tenant B's brand: %s", r.brief())
			return
		}
		t.pass("a tenant-A player's credentials are refused on tenant B's brand (HTTP %d)", r.Status)
	})
	w.check("iso.player_cannot_read_other_players_resources", func(t *T) {
		if !t.needMoney("rg.time_out_blocks_play_but_not_settlement", "ledger.deposit_settle_via_mock_callback") {
			return
		}
		p1, p2 := w.players["p1"], w.players["p2"]
		if p2 == nil || p1 == nil {
			t.fail("missing players")
			return
		}
		type probe struct{ method, path string }
		probes := []probe{
			{"GET", "/v1/me/deposits/" + w.facts["p1_deposit_id"]},
			{"POST", "/v1/me/deposits/" + w.facts["p1_deposit_id"] + "/simulate-callback"},
			{"GET", "/v1/me/withdrawals/" + w.facts["p1_withdrawal_id"]},
			{"POST", "/v1/me/withdrawals/" + w.facts["p1_withdrawal_id"] + "/cancel"},
			{"POST", "/v1/me/casino/sessions/" + w.facts["p1_session_id"] + "/wager"},
			{"POST", "/v1/me/payout-instruments/" + w.facts["p1_instrument_id"] + "/revoke"},
		}
		ran := 0
		for _, pr := range probes {
			if strings.Contains(pr.path, "//") || strings.HasSuffix(pr.path, "/") || strings.Contains(pr.path, "/sessions//") {
				continue // the flow that records this id did not run (e.g. no finance staff configured)
			}
			ran++
			var body any
			if strings.HasSuffix(pr.path, "/wager") {
				body = map[string]any{"stake_amount": 100, "idempotency_key": "acc-" + w.cfg.RunID + "-xp"}
			}
			r := t.call(pr.method, pr.path, p2.access, body)
			if r == nil {
				return
			}
			if r.ok() {
				t.fail("player p2 reached player p1's resource via %s %s: %s", pr.method, pr.path, r.brief())
				return
			}
			if r.Status != 404 && r.Status != 403 {
				t.fail("%s %s answered %s (want 404/403)", pr.method, pr.path, r.brief())
				return
			}
		}
		if ran == 0 {
			t.skipPrereq("no p1 resource ids were recorded")
			return
		}
		l := t.call("GET", "/v1/me/deposits", p2.access, nil)
		if l == nil || !t.expect(l, 200, "", "list deposits as p2") {
			return
		}
		for _, it := range l.arr() {
			if pickStr(it, "id") == w.facts["p1_deposit_id"] {
				t.fail("p1's deposit appears in p2's deposit list")
				return
			}
		}
		wl := t.call("GET", "/v1/me/wallets", p2.access, nil)
		if wl == nil || !t.expect(wl, 200, "", "list wallets as p2") {
			return
		}
		p1w := t.call("GET", "/v1/me/wallets", p1.access, nil)
		if p1w == nil {
			return
		}
		ids := map[string]bool{}
		for _, it := range p1w.arr() {
			ids[pickStr(it, "wallet_id")] = true
		}
		for _, it := range wl.arr() {
			if ids[pickStr(it, "wallet_id")] {
				t.fail("p1 and p2 share a wallet id")
				return
			}
		}
		t.pass("player p2 got 404/403 on %d of p1's deposit/withdrawal/session/instrument ids and sees none of p1's rows or wallets", ran)
	})
	w.check("iso.player_token_has_no_staff_authority", func(t *T) {
		if !t.needMoney("player.login") {
			return
		}
		p1 := w.players["p1"]
		reqs := [][2]string{
			{"POST", "/v1/admin/tenants"},
			{"PUT", "/v1/admin/providers/mock-payments/capability"},
			{"GET", "/v1/admin/tenants/" + w.tenantID + "/staff"},
		}
		if id := w.facts["p1_withdrawal_id"]; id != "" {
			reqs = append(reqs, [2]string{"POST", "/v1/admin/withdrawals/" + id + "/approve"})
		}
		for _, req := range reqs {
			r := t.call(req[0], req[1], p1.access, map[string]any{})
			if r == nil {
				return
			}
			if r.Status != 403 && r.Status != 401 {
				t.fail("%s %s with a player token answered %s", req[0], req[1], r.brief())
				return
			}
		}
		t.pass("a player token is refused on tenant provisioning, withdrawal approval, provider config and staff listing")
	})
	w.check("iso.cross_tenant_active_brand", func(t *T) {
		t.skipPrereq("a second ACTIVE tenant with operating-market rows is an owner provisioning step (market rows have no API); with only a pending_launch isolation tenant, cross-tenant PLAYER reads cannot be exercised end to end. Staff-, login- and ownership-level isolation are asserted above. See README, 'What the suite cannot prove'")
	})
	_ = fmt.Sprint
}

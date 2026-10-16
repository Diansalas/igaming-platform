//go:build stagingacceptance

package main

import "fmt"

// player is one synthetic B2C player created by this run.
type player struct {
	tag      string
	email    string
	password string
	id       string
	access   string
	refresh  string
}

// needMoney is need() for the flows that only exist when the market is expected open.
func (t *T) needMoney(names ...string) bool {
	if t.w.cfg.MarketExpect == "closed" {
		t.skipPrereq("MARKET_EXPECT=closed: money flows are intentionally unreachable while the market is closed (fail-closed is asserted by player.register)")
		return false
	}
	return t.need(names...)
}

func (w *world) newPlayer(tag string) *player {
	p := &player{tag: tag, email: w.email(tag), password: "Pw-" + randHex(10)}
	w.scrub.add(p.password)
	w.players[tag] = p
	return p
}

func (w *world) takePlayerTokens(p *player, r *response) bool {
	m := r.obj()
	if t, ok := m["tokens"].(map[string]any); ok {
		m = t
	}
	p.access, p.refresh = pickStr(m, "access_token"), pickStr(m, "refresh_token")
	w.scrub.add(p.access)
	w.scrub.add(p.refresh)
	return p.access != ""
}

func (w *world) playerChecks() {
	p1 := w.newPlayer("p1")
	w.check("player.register", func(t *T) {
		if !t.need("setup.resolve_tenant_brand") {
			return
		}
		r := t.call("POST", "/v1/auth/register", "", map[string]any{"brand_slug": w.cfg.Brand, "email": p1.email, "password": p1.password})
		if r == nil {
			return
		}
		if w.cfg.MarketExpect == "closed" {
			if !t.expect(r, 403, "MARKET_NOT_AVAILABLE", "registration while the market is closed") {
				return
			}
			t.pass("registration refused 403 MARKET_NOT_AVAILABLE with no player created: the gate fails closed without market evidence")
			return
		}
		if !t.expect(r, 201, "", "POST /v1/auth/register") {
			return
		}
		if !w.takePlayerTokens(p1, r) {
			t.fail("registration returned no access token: %s", r.brief())
			return
		}
		t.pass("player registered on brand %s (HTTP 201, token pair issued; id read by player.me)", w.cfg.Brand)
	})
	w.check("player.duplicate_registration_refused", func(t *T) {
		if !t.needMoney("player.register") {
			return
		}
		r := t.call("POST", "/v1/auth/register", "", map[string]any{"brand_slug": w.cfg.Brand, "email": p1.email, "password": p1.password})
		if r == nil {
			return
		}
		if r.ok() {
			t.fail("registering the same email twice succeeded: %s", r.brief())
			return
		}
		t.pass("second registration of the same email refused (HTTP %d %s)", r.Status, r.code())
	})
	w.check("player.activation_seam", func(t *T) {
		if !t.needMoney("player.register") {
			return
		}
		if p1.access == "" {
			// registration returned no tokens: sign in first.
			r := t.call("POST", "/v1/auth/login", "", map[string]any{"brand_slug": w.cfg.Brand, "email": p1.email, "password": p1.password})
			if r == nil {
				return
			}
			if !t.expect(r, 200, "", "login before activation") {
				return
			}
			w.takePlayerTokens(p1, r)
		}
		r := t.call("POST", "/v1/me/email-verification/request", p1.access, nil)
		if r == nil {
			return
		}
		if r.Status == 204 {
			t.skipPrereq("the stack answers 204 (no token in the body): TEST_SUPPORT_ENDPOINTS_ENABLED is off, so a black-box client cannot complete e-mail activation (mock e-mail provider delivers nowhere)")
			return
		}
		if !t.expect(r, 200, "", "request e-mail verification (test-support token in body)") {
			return
		}
		tok := pickStr(r.obj(), "token")
		w.scrub.add(tok)
		if tok == "" {
			t.fail("200 without a token")
			return
		}
		rc := t.call("POST", "/v1/auth/email-verification/confirm", "", map[string]any{"token": tok})
		if !t.expect(rc, 204, "", "confirm e-mail token") {
			return
		}
		rr := t.call("POST", "/v1/auth/email-verification/confirm", "", map[string]any{"token": tok})
		if rr == nil {
			return
		}
		if rr.ok() {
			t.fail("the single-use verification token was accepted twice")
			return
		}
		t.pass("verification token issued (test support), confirmed 204, replay refused (HTTP %d)", rr.Status)
	})
	w.check("player.login", func(t *T) {
		if !t.needMoney("player.register") {
			return
		}
		bad := t.call("POST", "/v1/auth/login", "", map[string]any{"brand_slug": w.cfg.Brand, "email": p1.email, "password": p1.password + "x"})
		if !t.expect(bad, 401, "", "login with a wrong password") {
			return
		}
		r := t.call("POST", "/v1/auth/login", "", map[string]any{"brand_slug": w.cfg.Brand, "email": p1.email, "password": p1.password})
		if !t.expect(r, 200, "", "login") {
			return
		}
		if !w.takePlayerTokens(p1, r) {
			t.fail("login returned no access token: %s", r.brief())
			return
		}
		t.pass("wrong password 401, correct password 200 with a token pair")
	})
	w.check("player.me", func(t *T) {
		if !t.needMoney("player.login") {
			return
		}
		r := t.call("GET", "/v1/me", p1.access, nil)
		if !t.expect(r, 200, "", "GET /v1/me") {
			return
		}
		m := r.obj()
		p1.id = pickStr(m, "id")
		if p1.id == "" {
			t.fail("GET /v1/me carried no player id: %s", r.brief())
			return
		}
		if tid := pickStr(m, "tenant_id"); tid != w.tenantID || pickStr(m, "brand_id") != w.brandID {
			t.fail("the player is bound to tenant=%s brand=%s, expected the synthetic tenant/brand", tid, pickStr(m, "brand_id"))
			return
		}
		if st := pickStr(m, "status"); st != "active" {
			t.fail("player status=%s after e-mail activation, want active", st)
			return
		}
		w.facts["player_status"] = pickStr(m, "status")
		t.pass("GET /v1/me ok: id=%s status=active bound to the synthetic tenant and brand", p1.id)
	})
	w.check("player.token_refresh_rotation", func(t *T) {
		if !t.needMoney("player.login") {
			return
		}
		old := p1.refresh
		if old == "" {
			t.fail("no refresh token to exercise")
			return
		}
		r := t.call("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": old})
		if !t.expect(r, 200, "", "refresh") {
			return
		}
		prev := p1.access
		if !w.takePlayerTokens(p1, r) || p1.refresh == old {
			t.fail("refresh did not rotate the refresh token")
			return
		}
		_ = prev
		again := t.call("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": old})
		if again == nil {
			return
		}
		if again.ok() {
			t.fail("a rotated refresh token was accepted again (no replay protection)")
			return
		}
		// A replayed refresh token may revoke the whole family: sign in again to keep a usable session.
		rl := t.call("POST", "/v1/auth/login", "", map[string]any{"brand_slug": w.cfg.Brand, "email": p1.email, "password": p1.password})
		if rl == nil || !w.takePlayerTokens(p1, rl) {
			t.fail("could not sign in again after the refresh replay probe")
			return
		}
		t.pass("refresh rotates the token; the old refresh token is refused (HTTP %d)", again.Status)
	})
	w.check("auth.player_token_denied_admin_routes", func(t *T) {
		if !t.needMoney("player.login") {
			return
		}
		for _, path := range []string{"/v1/admin/tenants", "/v1/admin/players", "/v1/admin/alerts", "/v1/admin/withdrawals", "/v1/admin/audit-log"} {
			r := t.call("GET", path, p1.access, nil)
			if r == nil {
				return
			}
			if r.Status != 403 && r.Status != 401 {
				t.fail("a player token on %s answered %s", path, r.brief())
				return
			}
		}
		t.pass("player token refused on 5 staff routes")
	})
	w.check("player.residence_declared", func(t *T) {
		if !t.needMoney("player.login") {
			return
		}
		if w.cfg.Residence == "" {
			t.skipPrereq("PLAYER_RESIDENCE_COUNTRY not supplied: the KYC-residence evidence branch (market-evidence-kyc-residence) is not assumed; set it when the staging config requires residence evidence")
			return
		}
		r := t.call("PUT", "/v1/me/residence", p1.access, map[string]any{"country_code": w.cfg.Residence})
		if r == nil {
			return
		}
		if !r.ok() {
			t.fail("declaring residence %s: %s", w.cfg.Residence, r.brief())
			return
		}
		g := t.call("GET", "/v1/me/residence", p1.access, nil)
		if !t.expect(g, 200, "", "read back residence") {
			return
		}
		if pickStr(g.obj(), "country_code") != w.cfg.Residence {
			t.fail("residence read-back differs: %s", g.brief())
			return
		}
		t.pass("declared residence %s read back", w.cfg.Residence)
	})
}

var _ = fmt.Sprint

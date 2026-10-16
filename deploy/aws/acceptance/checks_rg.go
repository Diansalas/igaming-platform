//go:build stagingacceptance

package main

// makePlayer registers, activates (test-support token) and signs in a synthetic player on the synthetic brand. On
// any failure it concludes the check and returns nil.
func (t *T) makePlayer(tag string) *player {
	w := t.w
	if p := w.players[tag]; p != nil && p.access != "" && p.id != "" {
		return p
	}
	p := w.newPlayer(tag)
	r := t.call("POST", "/v1/auth/register", "", map[string]any{"brand_slug": w.cfg.Brand, "email": p.email, "password": p.password})
	if r == nil || !t.expect(r, 201, "", "register "+tag) {
		return nil
	}
	if !w.takePlayerTokens(p, r) {
		t.fail("registration of %s returned no token", tag)
		return nil
	}
	rv := t.call("POST", "/v1/me/email-verification/request", p.access, nil)
	if rv == nil {
		return nil
	}
	if rv.Status != 200 {
		t.skipPrereq("activation of a second synthetic player needs the test-support token (HTTP %d)", rv.Status)
		return nil
	}
	tok := pickStr(rv.obj(), "token")
	w.scrub.add(tok)
	if rc := t.call("POST", "/v1/auth/email-verification/confirm", "", map[string]any{"token": tok}); rc == nil || !t.expect(rc, 204, "", "confirm "+tag) {
		return nil
	}
	rl := t.call("POST", "/v1/auth/login", "", map[string]any{"brand_slug": w.cfg.Brand, "email": p.email, "password": p.password})
	if rl == nil || !t.expect(rl, 200, "", "login "+tag) || !w.takePlayerTokens(p, rl) {
		return nil
	}
	me := t.call("GET", "/v1/me", p.access, nil)
	if me == nil || !t.expect(me, 200, "", "GET /v1/me "+tag) {
		return nil
	}
	p.id = pickStr(me.obj(), "id")
	return p
}

func (w *world) rgChecks() {
	asset := w.cfg.Asset
	w.check("rg.enforcement_off_by_default", func(t *T) {
		role := "TENANT_ADMIN"
		if w.actor(role) == nil {
			role = "COMPLIANCE"
		}
		if w.actor(role) == nil {
			t.skipPrereq("rg_config:read needs TENANT_ADMIN or COMPLIANCE (neither configured)")
			return
		}
		r := t.acall(role, "GET", "/v1/admin/rg/enforcement", nil)
		if r == nil || !t.expect(r, 200, "", "GET /v1/admin/rg/enforcement") {
			return
		}
		on, _ := pick(r.obj(), "enforcement_enabled").(bool)
		if on {
			t.fail("RG limit enforcement is ON for the synthetic tenant: the switch is one-way and the default state OFF was expected; the suite never enables it (rg-enforcement-activation.md)")
			return
		}
		t.pass("enforcement_enabled=false (default OFF; the suite only reads this and never switches it on)")
	})
	w.check("rg.player_surfaces_and_default_off", func(t *T) {
		if !t.needMoney("player.login") {
			return
		}
		p1 := w.players["p1"]
		st := t.call("GET", "/v1/me/rg/status", p1.access, nil)
		if st == nil || !t.expect(st, 200, "", "GET /v1/me/rg/status") {
			return
		}
		if len(st.arr()) != 0 {
			t.fail("the fresh synthetic player already has %d restriction(s)", len(st.arr()))
			return
		}
		op := t.call("GET", "/v1/me/rg/options?asset_code="+asset, p1.access, nil)
		if op == nil || !t.expect(op, 200, "", "GET /v1/me/rg/options") {
			return
		}
		if on, present := pick(op.obj(), "limit_enforcement_enabled").(bool); present && on {
			t.fail("the player options report limit_enforcement_enabled=true")
			return
		}
		lm := t.call("GET", "/v1/me/rg/limits", p1.access, nil)
		if lm == nil || !t.expect(lm, 200, "", "GET /v1/me/rg/limits") {
			return
		}
		t.pass("status empty, options report limit enforcement off, limits readable; the earlier deposits/bets/withdrawals ran with no player limit set (enforcement OFF)")
	})
	w.check("rg.time_out_blocks_play_but_not_settlement", func(t *T) {
		if !t.needMoney("ledger.deposit_settle_via_mock_callback") {
			return
		}
		p2 := t.makePlayer("p2")
		if p2 == nil {
			return
		}
		_, id, st := t.deposit(p2, asset, 2000, "acc-"+w.cfg.RunID+"-rg-dep")
		if id == "" || st != "pending" {
			t.fail("could not create the funding deposit for the RG probe player")
			return
		}
		if cb := t.call("POST", "/v1/me/deposits/"+id+"/simulate-callback", p2.access, nil); cb == nil || !t.expect(cb, 200, "", "settle") {
			return
		}
		to := t.call("POST", "/v1/me/rg/time-out", p2.access, map[string]any{"duration_hours": 1}, withHeader("Idempotency-Key", "acc-"+w.cfg.RunID+"-rg-to"))
		if to == nil {
			return
		}
		if to.Status != 201 && to.Status != 200 {
			t.fail("start a 1-hour time-out: %s", to.brief())
			return
		}
		blocked := 0
		dep := t.call("POST", "/v1/me/deposits", p2.access, map[string]any{"asset_code": asset, "amount": 500, "payment_method": "card", "idempotency_key": "acc-" + w.cfg.RunID + "-rg-dep2"})
		if dep == nil {
			return
		}
		if !dep.ok() || pickStr(dep.obj(), "status") == "declined" {
			blocked++
		} else {
			t.fail("a deposit by a player in a time-out was accepted: %s", dep.brief())
			return
		}
		gid := w.facts["game_id"]
		if gid != "" {
			l := t.call("POST", "/v1/me/casino/games/"+gid+"/launch", p2.access, map[string]any{"asset_code": asset, "mode": "real"})
			if l == nil {
				return
			}
			if l.ok() {
				t.fail("a casino launch by a player in a time-out was accepted: %s", l.brief())
				return
			}
			blocked++
		}
		wl, ok := t.wallet(p2, asset)
		if !ok {
			return
		}
		if wl.Cash != 2000 {
			t.fail("balance %d changed during the time-out probe (want 2000)", wl.Cash)
			return
		}
		rs := t.call("GET", "/v1/me/rg/status", p2.access, nil)
		if rs == nil || !t.expect(rs, 200, "", "status") {
			return
		}
		active := false
		for _, it := range rs.arr() {
			if a, _ := pick(it, "active").(bool); a {
				active = true
			}
		}
		if !active {
			t.fail("the time-out is not reported active in /v1/me/rg/status")
			return
		}
		t.pass("time-out is enforced without any enforcement switch: %d of deposit/launch refused, balance untouched, restriction reported active", blocked)
	})
}

// roleOr returns the first configured role of the list ("" when none).
func (w *world) roleOr(roles ...string) string {
	for _, r := range roles {
		if w.actor(r) != nil {
			return r
		}
	}
	return ""
}

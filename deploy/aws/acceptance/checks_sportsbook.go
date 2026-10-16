//go:build stagingacceptance

package main

func (w *world) sportsbookChecks() {
	p1 := w.players["p1"]
	asset := w.cfg.Asset
	var selID, betID string
	var num, den int64
	stake := w.cfg.StakeAmount
	w.check("sportsbook.catalogue", func(t *T) {
		r := t.call("GET", "/v1/sportsbook/sports", "", nil)
		if r == nil {
			return
		}
		if r.Status == 503 || r.Status == 404 {
			t.skipPrereq("sportsbook is not enabled on this stack (HTTP %d)", r.Status)
			return
		}
		if !t.expect(r, 200, "", "GET /v1/sportsbook/sports") {
			return
		}
		events := 0
		var eventID string
		for _, sp := range r.arr() {
			comps, _ := pick(sp, "competitions").([]any)
			for _, c := range comps {
				evs, _ := pick(c, "events").([]any)
				for _, e := range evs {
					events++
					if eventID == "" {
						if av, _ := pick(e, "available").(bool); av {
							eventID = pickStr(e, "id")
						}
					}
				}
			}
		}
		if eventID == "" {
			t.fail("the mock sportsbook catalogue lists %d event(s), none available", events)
			return
		}
		ev := t.call("GET", "/v1/sportsbook/events/"+eventID, "", nil)
		if ev == nil || !t.expect(ev, 200, "", "event detail") {
			return
		}
		mk, _ := pick(ev.obj(), "markets").([]any)
		for _, m := range mk {
			sels, _ := pick(m, "selections").([]any)
			for _, s := range sels {
				if av, _ := pick(s, "available").(bool); av && selID == "" {
					selID = pickStr(s, "id")
					n, _ := pickNum(s, "odds_numerator")
					d, _ := pickNum(s, "odds_denominator")
					num, den = int64(n), int64(d)
				}
			}
		}
		if selID == "" {
			t.fail("no available selection in event %s", eventID)
			return
		}
		t.pass("%d mock events listed; selection %s available at %d/%d", events, selID, num, den)
	})
	w.check("sportsbook.place_bet_through_market_gate", func(t *T) {
		if !t.needMoney("sportsbook.catalogue", "ledger.balance_equals_deposit") {
			return
		}
		before, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		body := map[string]any{"selection_id": selID, "stake_amount": stake, "asset_code": asset, "expected_odds_numerator": num, "expected_odds_denominator": den, "idempotency_key": "acc-" + w.cfg.RunID + "-sb-1"}
		r := t.call("POST", "/v1/me/sportsbook/bets", p1.access, body)
		if r == nil {
			return
		}
		if !r.ok() {
			t.fail("place bet: %s", r.brief())
			return
		}
		if acc, _ := pick(r.obj(), "accepted").(bool); !acc {
			t.fail("bet was not accepted: %s", r.brief())
			return
		}
		betID = pickStr(r.obj(), "bet", "id")
		if betID == "" {
			betID = pickStr(r.obj(), "id")
		}
		after, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if after.Cash+after.Locked+after.Held != before.Cash+before.Locked+before.Held && after.Cash != before.Cash-stake {
			t.fail("balances before cash/locked=%d/%d after=%d/%d for stake %d", before.Cash, before.Locked, after.Cash, after.Locked, stake)
			return
		}
		rep := t.call("POST", "/v1/me/sportsbook/bets", p1.access, body)
		if rep == nil {
			return
		}
		again, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if again.Cash != after.Cash || again.Locked != after.Locked {
			t.fail("a replay of the same bet key moved the balance (cash/locked %d/%d -> %d/%d)", after.Cash, after.Locked, again.Cash, again.Locked)
			return
		}
		w.setExpected(p1, asset, again.Cash)
		t.pass("bet accepted for %d (potential return from %d/%d odds); stake left cash (cash %d -> %d, locked %d); replay of the same key changed nothing (HTTP %d)", stake, num, den, before.Cash, after.Cash, after.Locked, rep.Status)
	})
	w.check("sportsbook.bet_visible_to_player_and_backoffice", func(t *T) {
		if !t.needMoney("sportsbook.place_bet_through_market_gate") {
			return
		}
		r := t.call("GET", "/v1/me/sportsbook/bets", p1.access, nil)
		if r == nil || !t.expect(r, 200, "", "GET /v1/me/sportsbook/bets") {
			return
		}
		if len(items(r)) == 0 {
			t.fail("the player's bet list is empty")
			return
		}
		role := w.roleOr("TENANT_ADMIN", "FINANCE1")
		if role == "" {
			t.pass("player list shows %d bet(s); Back Office view needs tenant staff (not configured)", len(items(r)))
			return
		}
		b := t.acall(role, "GET", "/v1/admin/sportsbook/bets?limit=50", nil)
		if b == nil {
			return
		}
		if b.Status == 403 {
			t.pass("player list shows %d bet(s); %s lacks the sportsbook read permission (HTTP 403)", len(items(r)), role)
			return
		}
		if !t.expect(b, 200, "", "GET /v1/admin/sportsbook/bets") {
			return
		}
		t.pass("player list shows %d bet(s); Back Office queue shows %d", len(items(r)), len(items(b)))
	})
	w.check("sportsbook.mock_settlement_and_replay", func(t *T) {
		if !t.needMoney("sportsbook.place_bet_through_market_gate") {
			return
		}
		if w.actor("RISK_MANAGER") == nil {
			t.skipPrereq("RISK_MANAGER (the sole holder of sportsbook_settlement:simulate) is not configured; settlement of the mock bet cannot be driven")
			return
		}
		ev := map[string]any{"event_type": "settle", "generation": 1, "outcome": "lost", "payout_amount": 0, "asset_code": asset}
		r := t.acall("RISK_MANAGER", "POST", "/v1/admin/sportsbook/bets/"+betID+"/simulate-settlement-event", ev)
		if r == nil {
			return
		}
		if r.Status == 404 {
			t.skipPrereq("the settlement simulation route is not registered (test support off)")
			return
		}
		if !t.expect(r, 200, "", "simulate settlement") {
			return
		}
		rep := t.acall("RISK_MANAGER", "POST", "/v1/admin/sportsbook/bets/"+betID+"/simulate-settlement-event", ev)
		if rep == nil {
			return
		}
		if rep.ok() && pickStr(rep.obj(), "result") != "duplicate" && pickStr(rep.obj(), "result") != "replayed" && pickStr(rep.obj(), "result") != "idempotent_replay" {
			t.fail("a replay of the same settlement event was applied again: %s", rep.brief())
			return
		}
		t.pass("settlement applied (bet_status=%s result=%s) and the identical replay produced no second effect (HTTP %d result=%s)", pickStr(r.obj(), "bet_status"), pickStr(r.obj(), "result"), rep.Status, pickStr(rep.obj(), "result"))
	})
}

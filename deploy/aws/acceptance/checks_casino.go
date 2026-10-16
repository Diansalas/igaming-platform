//go:build stagingacceptance

package main

import (
	"fmt"
	"strings"
)

func (w *world) casinoChecks() {
	p1 := w.players["p1"]
	asset := w.cfg.Asset
	stake := w.cfg.StakeAmount
	var gameID, sessionID, wagerTx, wagerLedger, winTx string
	runKey := func(s string) string { return "acc-" + w.cfg.RunID + "-" + s }

	w.check("casino.catalogue_visible", func(t *T) {
		if !t.needMoney("ledger.balance_equals_deposit") {
			return
		}
		r := t.call("GET", "/v1/me/casino/games", p1.access, nil)
		if !t.expect(r, 200, "", "GET /v1/me/casino/games") {
			return
		}
		want := w.facts["game_id"]
		for _, g := range items(r) {
			id := pickStr(g, "id")
			if want != "" && id == want {
				gameID = id
			}
			if want == "" && gameID == "" && pickStr(g, "provider_id") == "mock-casino" {
				gameID = id
			}
		}
		if gameID == "" {
			t.fail("no launchable mock-casino game in the player's catalogue (%d games listed); the catalogue/opt-in setup is missing (setup.casino_catalogue)", len(items(r)))
			return
		}
		t.pass("game %s is in the tenant's catalogue (%d listed)", gameID, len(items(r)))
	})
	w.check("casino.launch", func(t *T) {
		if !t.needMoney("casino.catalogue_visible") {
			return
		}
		r := t.call("POST", "/v1/me/casino/games/"+gameID+"/launch", p1.access, map[string]any{"asset_code": asset, "mode": "real"})
		if !t.expect(r, 201, "", "launch") {
			return
		}
		sessionID = pickStr(r.obj(), "session_id")
		if sessionID == "" || pickStr(r.obj(), "launch_url") == "" {
			t.fail("launch returned no session: %s", r.brief())
			return
		}
		t.pass("single-use launch session %s minted through the market gate", sessionID)
	})
	sim := func(t *T, action string, body map[string]any) *response {
		return t.call("POST", "/v1/me/casino/sessions/"+sessionID+"/"+action, p1.access, body)
	}
	w.check("casino.wager", func(t *T) {
		if !t.needMoney("casino.launch") {
			return
		}
		r := sim(t, "wager", map[string]any{"stake_amount": stake, "idempotency_key": runKey("bet-1")})
		if r == nil {
			return
		}
		if r.Status == 404 || r.Status == 503 {
			t.skipPrereq("casino play simulation is not available (HTTP %d): TEST_SUPPORT_ENDPOINTS_ENABLED / mock casino wiring is off", r.Status)
			return
		}
		if !t.expect(r, 200, "", "wager") {
			return
		}
		m := r.obj()
		if pickStr(m, "outcome") != "succeeded" {
			t.fail("bet outcome=%s: %s", pickStr(m, "outcome"), r.brief())
			return
		}
		wagerTx, wagerLedger = pickStr(m, "provider_tx_id"), pickStr(m, "ledger_transaction_id")
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		want := w.expected(p1, asset) - stake
		if wl.Cash != want {
			t.fail("balance after the bet is %d, want %d (expected %d - stake %d)", wl.Cash, want, w.expected(p1, asset), stake)
			return
		}
		w.setExpected(p1, asset, want)
		t.pass("bet of %d settled through the provider-callback pipeline; balance now %d; ledger tx %s", stake, wl.Cash, wagerLedger)
	})
	w.check("casino.wager_replay_idempotent", func(t *T) {
		if !t.needMoney("casino.wager") {
			return
		}
		r := sim(t, "wager", map[string]any{"stake_amount": stake, "idempotency_key": runKey("bet-1")})
		if r == nil {
			return
		}
		if !t.expect(r, 200, "", "replayed wager") {
			return
		}
		m := r.obj()
		if pickStr(m, "provider_tx_id") != wagerTx || (wagerLedger != "" && pickStr(m, "ledger_transaction_id") != wagerLedger) {
			t.fail("a replay of the same bet produced a different transaction: %s", r.brief())
			return
		}
		mis := sim(t, "wager", map[string]any{"stake_amount": stake + 1, "idempotency_key": runKey("bet-1")})
		if mis == nil {
			return
		}
		if mis.ok() && pickStr(mis.obj(), "outcome") == "succeeded" {
			t.fail("the same bet key with a different stake was accepted: %s", mis.brief())
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if wl.Cash != w.expected(p1, asset) {
			t.fail("balance moved to %d on a replay (want %d): duplicate debit", wl.Cash, w.expected(p1, asset))
			return
		}
		t.pass("replay returns the original transaction %s without a second debit; same key + different stake refused (HTTP %d)", wagerTx, mis.Status)
	})
	w.check("casino.insufficient_funds_declined", func(t *T) {
		if !t.needMoney("casino.wager") {
			return
		}
		r := sim(t, "wager", map[string]any{"stake_amount": w.expected(p1, asset) + 1_000_000, "idempotency_key": runKey("bet-big")})
		if r == nil {
			return
		}
		if r.Status != 200 && r.Status != 409 && r.Status != 422 {
			t.fail("over-balance bet answered %s", r.brief())
			return
		}
		if r.Status == 200 && pickStr(r.obj(), "outcome") == "succeeded" {
			t.fail("a bet larger than the balance succeeded (negative balance): %s", r.brief())
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if wl.Cash != w.expected(p1, asset) || wl.Cash < 0 {
			t.fail("balance %d after a declined bet, want %d", wl.Cash, w.expected(p1, asset))
			return
		}
		t.pass("over-balance bet declined (HTTP %d outcome=%s), balance unchanged and non-negative", r.Status, pickStr(r.obj(), "outcome"))
	})
	w.check("casino.win", func(t *T) {
		if !t.needMoney("casino.wager") {
			return
		}
		win := stake * 3
		r := sim(t, "win", map[string]any{"win_amount": win, "idempotency_key": runKey("win-1")})
		if r == nil || !t.expect(r, 200, "", "win") {
			return
		}
		if pickStr(r.obj(), "outcome") != "succeeded" {
			t.fail("win outcome=%s: %s", pickStr(r.obj(), "outcome"), r.brief())
			return
		}
		winTx = pickStr(r.obj(), "provider_tx_id")
		rep := sim(t, "win", map[string]any{"win_amount": win, "idempotency_key": runKey("win-1")})
		if rep == nil || !t.expect(rep, 200, "", "replayed win") {
			return
		}
		if pickStr(rep.obj(), "provider_tx_id") != winTx {
			t.fail("replayed win produced a different transaction")
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		want := w.expected(p1, asset) + win
		if wl.Cash != want {
			t.fail("balance after the win and its replay is %d, want %d (credited once)", wl.Cash, want)
			return
		}
		w.setExpected(p1, asset, want)
		t.pass("win of %d credited once (replay returned the same transaction); balance %d", win, wl.Cash)
	})
	w.check("casino.rollback_of_unowned_transaction_refused", func(t *T) {
		if !t.needMoney("casino.win") {
			return
		}
		// The play simulation only rolls back a transaction that belongs to the caller's own round, so the
		// "rollback before the original" tombstone path (a signed provider callback) cannot be driven black-box.
		unseen := "acc-" + w.cfg.RunID + "-never-seen"
		r := sim(t, "rollback", map[string]any{"original_provider_tx_id": unseen})
		if r == nil {
			return
		}
		if r.ok() && pickStr(r.obj(), "outcome") == "succeeded" {
			t.fail("a rollback naming a transaction that is not in the round succeeded: %s", r.brief())
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if wl.Cash != w.expected(p1, asset) {
			t.fail("the refused rollback moved the balance to %d (want %d)", wl.Cash, w.expected(p1, asset))
			return
		}
		t.pass("rollback naming a transaction outside the round refused (HTTP %d %s); balance unchanged", r.Status, r.code())
	})
	w.check("casino.rollback_compensates_once", func(t *T) {
		if !t.needMoney("casino.win") {
			return
		}
		r := sim(t, "rollback", map[string]any{"original_provider_tx_id": wagerTx})
		if r == nil || !t.expect(r, 200, "", "rollback of the bet") {
			return
		}
		if pickStr(r.obj(), "outcome") != "succeeded" {
			t.fail("rollback outcome=%s: %s", pickStr(r.obj(), "outcome"), r.brief())
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		want := w.expected(p1, asset) + stake
		if wl.Cash != want {
			t.fail("balance after rolling back the bet is %d, want %d (stake refunded by a compensating entry)", wl.Cash, want)
			return
		}
		again := sim(t, "rollback", map[string]any{"original_provider_tx_id": wagerTx})
		if again == nil {
			return
		}
		if again.Status != 409 && !(again.Status == 200 && pickStr(again.obj(), "outcome") != "succeeded") {
			t.fail("a second rollback of the same bet was not refused/idempotent: %s", again.brief())
			return
		}
		wl2, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if wl2.Cash != want {
			t.fail("a second rollback moved the balance to %d (double reversal), want %d", wl2.Cash, want)
			return
		}
		w.setExpected(p1, asset, want)
		t.pass("rollback refunded the stake once via a compensating entry; the repeat answered HTTP %d with no second reversal; balance %d", again.Status, wl2.Cash)
	})
	w.check("casino.round_history", func(t *T) {
		if !t.needMoney("casino.win") {
			return
		}
		r := t.call("GET", "/v1/me/casino/rounds", p1.access, nil)
		if !t.expect(r, 200, "", "GET /v1/me/casino/rounds") {
			return
		}
		n := len(items(r))
		if n == 0 {
			t.fail("round history is empty after a bet and a win")
			return
		}
		if w.actor("TENANT_ADMIN") == nil {
			t.pass("player round history lists %d round(s); the Back Office view needs TENANT_ADMIN (not configured)", n)
			return
		}
		b := t.acall("TENANT_ADMIN", "GET", "/v1/admin/casino/rounds?limit=50", nil)
		if !t.expect(b, 200, "", "GET /v1/admin/casino/rounds") {
			return
		}
		if len(items(b)) == 0 {
			t.fail("the Back Office casino round queue is empty although the player has rounds")
			return
		}
		t.pass("player history lists %d round(s); Back Office queue lists %d", n, len(items(b)))
	})
	w.check("casino.unsigned_webhook_rejected", func(t *T) {
		if !t.needMoney("casino.win") {
			return
		}
		body := fmt.Sprintf(`{"event":"bet","tx_id":"acc-%s-forged","amount":%d}`, w.cfg.RunID, 999_999)
		r := t.call("POST", "/v1/webhooks/casino/"+w.cfg.Tenant+"/mock-casino", "", body)
		if r == nil {
			return
		}
		if r.ok() {
			t.fail("an UNSIGNED casino provider callback was accepted: %s", r.brief())
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if wl.Cash != w.expected(p1, asset) {
			t.fail("balance changed to %d after an unsigned callback", wl.Cash)
			return
		}
		t.pass("unsigned provider callback refused (HTTP %d %s); balance unchanged", r.Status, r.code())
	})
	w.check("casino.reconciliation_and_rejection_surfaces", func(t *T) {
		if !t.needMoney("casino.win") {
			return
		}
		role := "TENANT_ADMIN"
		if w.actor("FINANCE1") != nil {
			role = "FINANCE1"
		}
		if w.actor(role) == nil {
			t.skipPrereq("no tenant staff configured for the casino reconciliation read surfaces")
			return
		}
		parts := []string{}
		for _, p := range []string{"/v1/admin/casino/reconciliation/runs", "/v1/admin/casino/reconciliation/mismatches", "/v1/admin/casino/callback-rejections"} {
			r := t.acall(role, "GET", p+"?limit=20", nil)
			if r == nil {
				return
			}
			if r.Status == 403 && role == "FINANCE1" {
				r = t.acall("TENANT_ADMIN", "GET", p+"?limit=20", nil)
				if r == nil {
					return
				}
			}
			if !r.ok() {
				t.fail("GET %s: %s", p, r.brief())
				return
			}
			parts = append(parts, fmt.Sprintf("%s=%d", p[strings.LastIndex(p, "/")+1:], len(items(r))))
		}
		t.pass("casino reconciliation runs/mismatches and callback-rejection read surfaces answer 200 (%s)", strings.Join(parts, " "))
	})
}

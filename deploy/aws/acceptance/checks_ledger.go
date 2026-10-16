//go:build stagingacceptance

package main

import (
	"fmt"
	"sync"
)

// wallet is the player-visible balance summary of one asset.
type wallet struct {
	ID, Asset, Status             string
	Cash, Available, Held, Locked int64
	Bonus                         int64
}

func parseWallet(v any) (wallet, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return wallet{}, false
	}
	num := func(k string) int64 { f, _ := m[k].(float64); return int64(f) }
	w := wallet{ID: pickStr(m, "wallet_id"), Asset: pickStr(m, "asset_code"), Status: pickStr(m, "status"),
		Cash: num("cash_balance"), Available: num("available_balance"), Held: num("held_for_withdrawal"), Locked: num("locked_balance"), Bonus: num("bonus_balance")}
	return w, w.ID != "" && w.Asset != ""
}

// wallet reads GET /v1/me/wallets/{asset}.
func (t *T) wallet(p *player, asset string) (wallet, bool) {
	r := t.call("GET", "/v1/me/wallets/"+asset, p.access, nil)
	if r == nil {
		return wallet{}, false
	}
	if !t.expect(r, 200, "", "GET /v1/me/wallets/"+asset) {
		return wallet{}, false
	}
	wl, ok := parseWallet(r.obj())
	if !ok {
		t.fail("unparseable wallet summary: %s", r.brief())
		return wallet{}, false
	}
	return wl, true
}

// deposit initiates a deposit and returns the intent id and status.
func (t *T) deposit(p *player, asset string, amount int64, key string) (*response, string, string) {
	r := t.call("POST", "/v1/me/deposits", p.access, map[string]any{"asset_code": asset, "amount": amount, "payment_method": "card", "idempotency_key": key})
	if r == nil {
		return nil, "", ""
	}
	return r, pickStr(r.obj(), "id"), pickStr(r.obj(), "status")
}

func (w *world) ledgerChecks() {
	p1 := w.players["p1"]
	asset := w.cfg.Asset
	var depositID string
	depositKey := "acc-" + w.cfg.RunID + "-dep-1"

	w.check("ledger.deposit_initiate", func(t *T) {
		if !t.needMoney("player.me") {
			return
		}
		r, id, st := t.deposit(p1, asset, w.cfg.DepositAmount, depositKey)
		if r == nil {
			return
		}
		if r.Status != 201 && r.Status != 200 {
			t.fail("POST /v1/me/deposits: %s (is the mock-payments capability configured and the market open?)", r.brief())
			return
		}
		if id == "" || st != "pending" {
			t.fail("deposit intent id=%q status=%q, want a pending intent: %s", id, st, r.brief())
			return
		}
		depositID = id
		w.facts["p1_deposit_id"] = id
		t.pass("deposit intent %s pending for %d %s minor units via mock-payments", id, w.cfg.DepositAmount, asset)
	})
	w.check("ledger.deposit_idempotent_initiation", func(t *T) {
		if !t.needMoney("ledger.deposit_initiate") {
			return
		}
		r, id, _ := t.deposit(p1, asset, w.cfg.DepositAmount, depositKey)
		if r == nil {
			return
		}
		if !r.ok() || id != depositID {
			t.fail("replaying the same idempotency key must return the original intent %s, got %s", depositID, r.brief())
			return
		}
		mis, _, _ := t.deposit(p1, asset, w.cfg.DepositAmount+1, depositKey)
		if mis == nil {
			return
		}
		if mis.ok() {
			t.fail("the same idempotency key with a DIFFERENT amount was accepted: %s", mis.brief())
			return
		}
		lst := t.call("GET", "/v1/me/deposits", p1.access, nil)
		if lst == nil {
			return
		}
		n := 0
		for _, it := range lst.arr() {
			if pickStr(it, "id") == depositID {
				n++
			}
		}
		if n != 1 {
			t.fail("the deposit appears %d times in the player's list, want exactly 1", n)
			return
		}
		t.pass("same key returns the same intent; same key with another amount refused (HTTP %d %s); one intent listed", mis.Status, mis.code())
	})
	w.check("ledger.deposit_settle_via_mock_callback", func(t *T) {
		if !t.needMoney("ledger.deposit_initiate") {
			return
		}
		r := t.call("POST", "/v1/me/deposits/"+depositID+"/simulate-callback", p1.access, nil)
		if r == nil {
			return
		}
		if r.Status == 404 {
			t.skipPrereq("simulate-callback is not routed (TEST_SUPPORT_ENDPOINTS_ENABLED off): a black-box client cannot settle a mock-PSP deposit")
			return
		}
		if !t.expect(r, 200, "", "simulate-callback") {
			return
		}
		if st := pickStr(r.obj(), "status"); st != "succeeded" {
			t.fail("simulated callback outcome status=%q, want succeeded: %s", st, r.brief())
			return
		}
		t.pass("mock PSP callback processed through the real signature-verifying pipeline: deposit succeeded")
	})
	w.check("ledger.balance_equals_deposit", func(t *T) {
		if !t.needMoney("ledger.deposit_settle_via_mock_callback") {
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if wl.Cash != w.cfg.DepositAmount || wl.Available != w.cfg.DepositAmount {
			t.fail("wallet cash=%d available=%d, want exactly the deposit %d (projection must equal the ledger movement)", wl.Cash, wl.Available, w.cfg.DepositAmount)
			return
		}
		if wl.Held != 0 || wl.Locked != 0 || wl.Bonus != 0 {
			t.fail("unexpected held/locked/bonus amounts after a plain deposit: held=%d locked=%d bonus=%d", wl.Held, wl.Locked, wl.Bonus)
			return
		}
		w.setExpected(p1, asset, w.cfg.DepositAmount)
		t.pass("wallet %s cash=available=%d, nothing held/locked/bonus", wl.ID, wl.Cash)
	})
	w.check("ledger.duplicate_callback_idempotent", func(t *T) {
		if !t.needMoney("ledger.balance_equals_deposit") {
			return
		}
		r := t.call("POST", "/v1/me/deposits/"+depositID+"/simulate-callback", p1.access, nil)
		if r == nil {
			return
		}
		if r.Status != 409 && r.Status != 200 {
			t.fail("a second callback for a settled deposit answered %s (want 409 not-pending, or an idempotent 200)", r.brief())
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if wl.Cash != w.cfg.DepositAmount {
			t.fail("balance changed to %d after a duplicate callback (double credit): want %d", wl.Cash, w.cfg.DepositAmount)
			return
		}
		t.pass("duplicate callback answered HTTP %d; balance unchanged at %d (single credit)", r.Status, wl.Cash)
	})
	w.check("ledger.concurrent_callbacks_single_credit", func(t *T) {
		if !t.needMoney("ledger.balance_equals_deposit") {
			return
		}
		const amt = 700
		_, id, st := t.deposit(p1, asset, amt, "acc-"+w.cfg.RunID+"-dep-conc")
		if id == "" || st != "pending" {
			t.fail("could not create the second deposit for the concurrency probe")
			return
		}
		var wg sync.WaitGroup
		codes := make([]int, 6)
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r, err := w.c.call("POST", "/v1/me/deposits/"+id+"/simulate-callback", p1.access, nil)
				if err == nil {
					codes[i] = r.Status
				}
			}(i)
		}
		wg.Wait()
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		want := w.cfg.DepositAmount + amt
		if wl.Cash != want {
			t.fail("after 6 concurrent callbacks for one deposit the balance is %d, want exactly %d (credited once); statuses=%v", wl.Cash, want, codes)
			return
		}
		w.setExpected(p1, asset, want)
		t.pass("6 concurrent callbacks for one deposit credited it once (statuses %v); balance %d", codes, wl.Cash)
	})
	w.check("ledger.wallet_per_asset", func(t *T) {
		if !t.needMoney("ledger.balance_equals_deposit") {
			return
		}
		if w.cfg.SecondAsset == "" {
			t.skipPrereq("SECOND_ASSET is empty: only one fiat asset configured")
			return
		}
		a2 := w.cfg.SecondAsset
		r, id, st := t.deposit(p1, a2, 3300, "acc-"+w.cfg.RunID+"-dep-a2")
		if r == nil {
			return
		}
		if id == "" || st != "pending" {
			t.fail("deposit in the second asset %s refused: %s (asset not authorised for the tenant or not in the provider capability?)", a2, r.brief())
			return
		}
		cb := t.call("POST", "/v1/me/deposits/"+id+"/simulate-callback", p1.access, nil)
		if !t.expect(cb, 200, "", "settle second-asset deposit") {
			return
		}
		lst := t.call("GET", "/v1/me/wallets", p1.access, nil)
		if lst == nil {
			return
		}
		got := map[string]wallet{}
		ids := map[string]bool{}
		for _, it := range lst.arr() {
			if wl, ok := parseWallet(it); ok {
				if _, dup := got[wl.Asset]; dup {
					t.fail("two wallet rows for asset %s: a player must hold exactly one wallet per asset", wl.Asset)
					return
				}
				got[wl.Asset] = wl
				ids[wl.ID] = true
			}
		}
		e, ok1 := got[asset]
		u, ok2 := got[a2]
		if !ok1 || !ok2 || len(ids) != len(got) {
			t.fail("expected distinct wallets for %s and %s, got %d wallet(s)", asset, a2, len(got))
			return
		}
		if e.Cash != w.expected(p1, asset) || u.Cash != 3300 {
			t.fail("balances are not independent per asset: %s=%d (want %d) %s=%d (want 3300)", asset, e.Cash, w.expected(p1, asset), a2, u.Cash)
			return
		}
		w.setExpected(p1, a2, 3300)
		t.pass("separate wallets %s and %s with independent balances (%d / %d); no cross-asset balance mutation", asset, a2, e.Cash, u.Cash)
	})
	w.check("ledger.unknown_asset_refused", func(t *T) {
		if !t.needMoney("ledger.balance_equals_deposit") {
			return
		}
		r, id, _ := t.deposit(p1, "ZZZ", 1000, "acc-"+w.cfg.RunID+"-dep-zzz")
		if r == nil {
			return
		}
		if r.ok() && id != "" {
			t.fail("a deposit in an unregistered asset was accepted: %s", r.brief())
			return
		}
		t.pass("deposit in an unregistered asset refused (HTTP %d %s)", r.Status, r.code())
	})
	w.check("ledger.invalid_amounts_refused", func(t *T) {
		if !t.needMoney("ledger.balance_equals_deposit") {
			return
		}
		for _, amt := range []int64{0, -5} {
			r, _, _ := t.deposit(p1, asset, amt, fmt.Sprintf("acc-%s-dep-bad%d", w.cfg.RunID, amt))
			if r == nil {
				return
			}
			if r.ok() {
				t.fail("deposit amount %d accepted", amt)
				return
			}
		}
		r := t.call("POST", "/v1/me/deposits", p1.access, `{"asset_code":"`+asset+`","amount":10.5,"payment_method":"card","idempotency_key":"acc-`+w.cfg.RunID+`-frac"}`)
		if r == nil {
			return
		}
		if r.ok() {
			t.fail("a fractional (floating point) amount was accepted: %s", r.brief())
			return
		}
		t.pass("zero, negative and fractional amounts are refused (money is integer minor units)")
	})
}

// expected balances the suite has verified so far, per player and asset (minor units).
func (w *world) setExpected(p *player, asset string, v int64) {
	w.bal[p.tag+"/"+asset] = v
}

func (w *world) expected(p *player, asset string) int64 { return w.bal[p.tag+"/"+asset] }

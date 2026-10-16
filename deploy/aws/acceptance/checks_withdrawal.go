//go:build stagingacceptance

package main

import (
	"fmt"
	"strings"
	"time"
)

// syntheticIBAN builds a checksum-valid GB IBAN from the run id. It names no real account (bank code WEST is the ISO
// 13616 documentation example); it is unique per run because the platform refuses one destination shared by two players.
// The platform also refuses anything whose maximal digit run is a Luhn-valid 12-19 digit string (a possible card number),
// so the account digits are adjusted until the run is not Luhn-valid.
func syntheticIBAN(runID string) string {
	digits := ""
	for _, c := range runID + "0123456789abcdef" {
		digits += string(rune('0' + int(c)%10))
	}
	acct := []byte(digits[:14])
	for i := 13; luhnValid(string(acct)); {
		acct[i] = '0' + (acct[i]-'0'+1)%10
	}
	bban := "WEST" + string(acct)
	num := ""
	for _, c := range bban + "GB00" {
		if c >= 'A' && c <= 'Z' {
			num += fmt.Sprint(int(c-'A') + 10)
		} else {
			num += string(c)
		}
	}
	rem := 0
	for _, c := range num {
		rem = (rem*10 + int(c-'0')) % 97
	}
	return fmt.Sprintf("GB%02d%s", 98-rem, bban)
}

func luhnValid(digits string) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

func (w *world) withdrawalChecks() {
	p1 := w.players["p1"]
	asset := w.cfg.Asset
	amount := w.cfg.WithdrawAmount
	var instrID, wdID string
	need2 := func(t *T) bool {
		if w.actor("FINANCE1") == nil || w.actor("FINANCE2") == nil {
			t.skipPrereq("FINANCE1 and FINANCE2 (two distinct tenant finance staff with distinct Persons) are required for the withdrawal four-eyes flow; tenant staff cannot be created through the API in staging because invitations are delivered by the MOCK e-mail provider (README prerequisites)")
			return false
		}
		return true
	}

	w.check("kyc.mock_verification", func(t *T) {
		if !t.needMoney("ledger.balance_equals_deposit") {
			return
		}
		r := t.call("POST", "/v1/me/kyc/verifications", p1.access, nil)
		if r == nil {
			return
		}
		if r.Status == 503 {
			t.skipPrereq("player KYC answers 503: the MOCK KYC provider is gated off (test support disabled)")
			return
		}
		if r.Status != 201 && r.Status != 200 {
			t.fail("start verification: %s", r.brief())
			return
		}
		verID := pickStr(r.obj(), "id")
		if verID == "" {
			t.fail("no verification id: %s", r.brief())
			return
		}
		// The async outbox worker submits to the MOCK vendor (status unverified -> pending); the mock never approves by itself.
		deadline := w.now().Add(60 * time.Second)
		var status string
		for {
			g := t.call("GET", "/v1/me/kyc/verifications", p1.access, nil)
			if g == nil {
				return
			}
			if !t.expect(g, 200, "", "list own verifications") {
				return
			}
			for _, it := range g.arr() {
				if pickStr(it, "id") == verID {
					status = pickStr(it, "status")
				}
			}
			if status != "unverified" || w.now().After(deadline) {
				break
			}
			w.sleep(2 * time.Second)
		}
		if status == "unverified" {
			t.fail("the verification was never submitted to the mock vendor within 60s (KYC outbox worker not running?)")
			return
		}
		if w.actor("COMPLIANCE") == nil {
			t.skipPrereq("verification is %q at the mock vendor; approving it needs a COMPLIANCE staff account (POST /v1/admin/kyc/verifications/{id}/review) and none is configured: the withdrawal identity gate will refuse the player", status)
			return
		}
		body := map[string]any{"status": "approved", "reason": "staging acceptance synthetic player"}
		if w.cfg.KYCResidence != "" {
			body["verified_residence_country"] = w.cfg.KYCResidence
		}
		rv := t.acall("COMPLIANCE", "POST", "/v1/admin/kyc/verifications/"+verID+"/review", body)
		if rv == nil {
			return
		}
		if !rv.ok() {
			t.fail("compliance review: %s", rv.brief())
			return
		}
		status = pickStr(rv.obj(), "status")
		if status != "approved" {
			t.fail("review answered status=%q, want approved", status)
			return
		}
		t.pass("verification submitted to the mock vendor by the outbox worker, then approved by a compliance reviewer%s", ifs(w.cfg.KYCResidence != "", " with verified residence "+w.cfg.KYCResidence, ""))
	})
	w.check("withdrawal.payout_instrument_register", func(t *T) {
		if !t.needMoney("ledger.balance_equals_deposit") {
			return
		}
		iban := syntheticIBAN(w.cfg.RunID)
		body := map[string]any{"kind": "bank_account", "rail": "bank_transfer", "asset_codes": []string{asset}, "detail": map[string]string{"iban": iban}}
		r := t.call("POST", "/v1/me/payout-instruments", p1.access, body)
		if r == nil {
			return
		}
		if r.Status == 503 {
			t.skipPrereq("payout instruments answer 503: PAYOUT_INSTRUMENT_KEYS are not configured on the stack")
			return
		}
		if r.Status != 201 && r.Status != 200 {
			t.fail("register payout instrument: %s", r.brief())
			return
		}
		instrID = pickStr(r.obj(), "id")
		w.facts["p1_instrument_id"] = instrID
		if strings.Contains(string(r.Body), iban) || strings.Contains(string(r.Body), "WEST") {
			t.fail("the response echoes the raw IBAN: only a mask may be returned")
			return
		}
		if instrID == "" || pickStr(r.obj(), "state") != "verified" {
			t.fail("instrument id=%q state=%q, want a verified instrument: %s", instrID, pickStr(r.obj(), "state"), r.brief())
			return
		}
		again := t.call("POST", "/v1/me/payout-instruments", p1.access, body)
		if again == nil {
			return
		}
		if !again.ok() || pickStr(again.obj(), "id") != instrID {
			t.fail("registering the same destination again must return the same instrument: %s", again.brief())
			return
		}
		t.pass("instrument %s verified (mock verifier), masked as %s; re-registration idempotent", instrID, pickStr(r.obj(), "display_mask"))
	})
	w.check("withdrawal.request_holds_funds", func(t *T) {
		// The identity gate refuses a withdrawal without an approved KYC verification, so the flow depends on kyc.*.
		if !t.needMoney("withdrawal.payout_instrument_register", "kyc.mock_verification") {
			return
		}
		before, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		// Re-baseline: earlier flows (casino) have their own checks; this flow asserts deltas from here.
		w.setExpected(p1, asset, before.Cash)
		key := "acc-" + w.cfg.RunID + "-wd-1"
		body := map[string]any{"asset_code": asset, "amount": amount, "idempotency_key": key, "payout_instrument_id": instrID}
		r := t.call("POST", "/v1/me/withdrawals", p1.access, body)
		if r == nil {
			return
		}
		if r.Status == 503 {
			t.skipPrereq("withdrawals answer 503: payout-instrument keys are not configured")
			return
		}
		if r.Status != 201 && r.Status != 200 {
			t.fail("POST /v1/me/withdrawals: %s", r.brief())
			return
		}
		wdID = pickStr(r.obj(), "id")
		w.facts["p1_withdrawal_id"] = wdID
		st := pickStr(r.obj(), "state")
		if wdID == "" {
			t.fail("no withdrawal id: %s", r.brief())
			return
		}
		after, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if after.Held != before.Held+amount || after.Available != before.Available-amount || after.Cash != before.Cash-amount {
			t.fail("hold not reflected correctly: before cash/avail/held=%d/%d/%d after=%d/%d/%d for amount %d (the hold moves the amount out of cash/available into held_for_withdrawal)",
				before.Cash, before.Available, before.Held, after.Cash, after.Available, after.Held, amount)
			return
		}
		rep := t.call("POST", "/v1/me/withdrawals", p1.access, body)
		if rep == nil {
			return
		}
		if !rep.ok() || pickStr(rep.obj(), "id") != wdID {
			t.fail("replay of the same idempotency key returned another request: %s", rep.brief())
			return
		}
		again, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if again.Held != after.Held {
			t.fail("the replayed request placed a second hold (held %d -> %d)", after.Held, again.Held)
			return
		}
		t.pass("withdrawal %s state=%s; %d moved from cash/available into held_for_withdrawal; replay created no second hold", wdID, st, amount)
	})
	w.check("withdrawal.overdraw_refused", func(t *T) {
		if !t.needMoney("withdrawal.request_holds_funds") {
			return
		}
		before, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		r := t.call("POST", "/v1/me/withdrawals", p1.access, map[string]any{"asset_code": asset, "amount": before.Available + 1, "idempotency_key": "acc-" + w.cfg.RunID + "-wd-over", "payout_instrument_id": instrID})
		if r == nil {
			return
		}
		if r.ok() {
			t.fail("a withdrawal larger than the available balance was accepted: %s", r.brief())
			return
		}
		after, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if after.Held != before.Held || after.Cash != before.Cash || after.Available != before.Available {
			t.fail("balances moved on a refused withdrawal")
			return
		}
		t.pass("overdraw refused (HTTP %d %s); no hold placed", r.Status, r.code())
	})
	w.check("withdrawal.review_queue_visible", func(t *T) {
		if !t.needMoney("withdrawal.request_holds_funds") || !need2(t) {
			return
		}
		r := t.acall("FINANCE1", "GET", "/v1/admin/withdrawals?limit=100", nil)
		if !t.expect(r, 200, "", "GET /v1/admin/withdrawals") {
			return
		}
		for _, it := range items(r) {
			if pickStr(it, "id") == wdID {
				t.pass("request %s is in the finance review queue (state=%s)", wdID, pickStr(it, "state"))
				return
			}
		}
		t.fail("request %s is not in the finance review queue (%d items)", wdID, len(items(r)))
	})
	approve := func(t *T, role string) *response {
		a := w.actor(role)
		if err := a.freshFinancial(); err != nil {
			t.fail("%v", err)
			return nil
		}
		return t.acall(role, "POST", "/v1/admin/withdrawals/"+wdID+"/approve", map[string]any{})
	}
	w.check("withdrawal.four_eyes_first_approval_not_enough", func(t *T) {
		if !t.needMoney("withdrawal.review_queue_visible") {
			return
		}
		r := approve(t, "FINANCE1")
		if r == nil || !t.expect(r, 200, "", "first approval") {
			return
		}
		if ok, _ := pick(r.obj(), "approved").(bool); ok {
			t.fail("ONE approval completed the request: four-eyes is not enforced for this amount: %s", r.brief())
			return
		}
		t.pass("first approval recorded, request not yet approved (approved=false)")
	})
	w.check("withdrawal.four_eyes_same_approver_twice_refused", func(t *T) {
		if !t.needMoney("withdrawal.four_eyes_first_approval_not_enough") {
			return
		}
		r := approve(t, "FINANCE1")
		if r == nil {
			return
		}
		if r.ok() {
			t.fail("the same approver was allowed to approve twice: %s", r.brief())
			return
		}
		t.pass("second approval by the same staff member refused (HTTP %d %s)", r.Status, r.code())
	})
	w.check("withdrawal.submit_before_quorum_refused", func(t *T) {
		if !t.needMoney("withdrawal.four_eyes_first_approval_not_enough") {
			return
		}
		if err := w.actor("FINANCE1").freshFinancial(); err != nil {
			t.fail("%v", err)
			return
		}
		r := t.acall("FINANCE1", "POST", "/v1/admin/withdrawals/"+wdID+"/submit", map[string]any{"payment_method": "card"})
		if r == nil {
			return
		}
		if r.ok() {
			t.fail("a payout was submitted with one of two approvals: %s", r.brief())
			return
		}
		t.pass("submit before the approval quorum refused (HTTP %d %s)", r.Status, r.code())
	})
	w.check("withdrawal.four_eyes_second_approval", func(t *T) {
		if !t.needMoney("withdrawal.four_eyes_first_approval_not_enough") {
			return
		}
		r := approve(t, "FINANCE2")
		if r == nil || !t.expect(r, 200, "", "second approval by a different staff member") {
			return
		}
		if ok, _ := pick(r.obj(), "approved").(bool); !ok {
			t.fail("two distinct approvals did not complete the request: %s", r.brief())
			return
		}
		t.pass("second, distinct approver completed the quorum (approved=true)")
	})
	w.check("withdrawal.submit_mock_payout", func(t *T) {
		if !t.needMoney("withdrawal.four_eyes_second_approval") {
			return
		}
		if err := w.actor("FINANCE1").freshFinancial(); err != nil {
			t.fail("%v", err)
			return
		}
		var last *response
		for _, method := range []string{"bank_transfer"} {
			r := t.acall("FINANCE1", "POST", "/v1/admin/withdrawals/"+wdID+"/submit", map[string]any{"payment_method": method})
			if r == nil {
				return
			}
			last = r
			if r.ok() {
				t.pass("payout submitted to the mock PSP with payment_method=%s: %s", method, pickStr(r.obj(), "state"))
				return
			}
			if r.code() != "PAYMENT_METHOD_MISMATCH" && r.code() != "validation_error" {
				break
			}
		}
		t.fail("submit payout: %s", last.brief())
	})
	w.check("withdrawal.payout_in_flight", func(t *T) {
		if !t.needMoney("withdrawal.submit_mock_payout") {
			return
		}
		// A mock payout stays "pending" at the PSP: the mock exposes no black-box settlement. Staff resolution polls the
		// SAME provider reference (never a second payout) and must leave the request submitted with the hold intact.
		if err := w.actor("FINANCE1").freshFinancial(); err != nil {
			t.fail("%v", err)
			return
		}
		rr := t.acall("FINANCE1", "POST", "/v1/admin/withdrawals/"+wdID+"/resolve", map[string]any{})
		if rr == nil {
			return
		}
		if !rr.ok() && rr.Status != 409 {
			t.fail("resolve: %s", rr.brief())
			return
		}
		r := t.acall("FINANCE1", "GET", "/v1/admin/withdrawals/"+wdID, nil)
		if r == nil || !t.expect(r, 200, "", "GET /v1/admin/withdrawals/{id}") {
			return
		}
		state := pickStr(r.obj(), "state")
		w.facts["wd_main_state"] = state
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		switch state {
		case "submitted":
			if wl.Held != amount || wl.Cash != w.expected(p1, asset)-amount {
				t.fail("in-flight payout but held=%d cash=%d (want held=%d cash=%d)", wl.Held, wl.Cash, amount, w.expected(p1, asset)-amount)
				return
			}
			t.pass("payout is in flight at the mock PSP (state=submitted); the hold of %d stays in held_for_withdrawal; staff resolution (%d) is idempotent", amount, rr.Status)
		case "completed":
			t.pass("payout already completed (state=completed)")
		default:
			t.fail("unexpected state %q after submit", state)
		}
	})
	w.check("withdrawal.payout_completion", func(t *T) {
		if !t.needMoney("withdrawal.payout_in_flight") {
			return
		}
		if w.facts["wd_main_state"] == "completed" {
			t.pass("payout completed")
			return
		}
		t.skipGap("PLATFORM GAP: a mock payout cannot be completed black-box. MockProvider.Withdraw answers pending for every ordinary amount; there is no test-support payout-settlement route (only POST /v1/me/deposits/{id}/simulate-callback exists for deposits); the mock webhook secret is in-process, so a signed payout callback cannot be forged; staff resolve/sweeper only re-poll the mock, which stays pending. 'Mock payout completes -> ledger balanced' is therefore not provable through the HTTP API (see the record, finding SA-2)")
	})
	w.check("withdrawal.declined_payout_releases_hold", func(t *T) {
		if !t.needMoney("withdrawal.payout_in_flight") {
			return
		}
		// The mock PSP declines a payout of exactly 111 minor units without cascade (payments.MockAmountPlayerDeclineNoCascade).
		const declineAmount = 111
		before, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		r := t.call("POST", "/v1/me/withdrawals", p1.access, map[string]any{"asset_code": asset, "amount": declineAmount, "idempotency_key": "acc-" + w.cfg.RunID + "-wd-decline", "payout_instrument_id": instrID})
		if r == nil {
			return
		}
		if r.Status != 201 {
			t.fail("request the %d-unit withdrawal: %s", declineAmount, r.brief())
			return
		}
		id := pickStr(r.obj(), "id")
		for _, role := range []string{"FINANCE1", "FINANCE2"} {
			if err := w.actor(role).freshFinancial(); err != nil {
				t.fail("%v", err)
				return
			}
			if role == "FINANCE1" {
				// the review queue read promotes requested -> pending_review
				if q := t.acall(role, "GET", "/v1/admin/withdrawals?limit=100", nil); q == nil {
					return
				}
			}
			ap := t.acall(role, "POST", "/v1/admin/withdrawals/"+id+"/approve", map[string]any{})
			if ap == nil || !t.expect(ap, 200, "", role+" approval") {
				return
			}
		}
		sb := t.acall("FINANCE1", "POST", "/v1/admin/withdrawals/"+id+"/submit", map[string]any{"payment_method": "bank_transfer"})
		if sb == nil {
			return
		}
		if !sb.ok() && sb.Status != 409 && sb.Status != 502 && sb.Status != 503 {
			t.fail("submit the declinable payout: %s", sb.brief())
			return
		}
		var state string
		for i := 0; i < 8; i++ {
			g := t.acall("FINANCE1", "GET", "/v1/admin/withdrawals/"+id, nil)
			if g == nil {
				return
			}
			state = pickStr(g.obj(), "state")
			if state != "submitted" && state != "approved" {
				break
			}
			if rs := t.acall("FINANCE1", "POST", "/v1/admin/withdrawals/"+id+"/resolve", map[string]any{}); rs == nil {
				return
			}
			w.sleep(2 * time.Second)
		}
		after, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		if state != "failed" && state != "rejected" && state != "cancelled" {
			t.fail("a payout the mock PSP declines ended in state %q (HTTP %d on submit), want a terminal failure", state, sb.Status)
			return
		}
		if after.Held != before.Held || after.Cash != before.Cash {
			t.fail("after a declined payout the hold was not released: cash/held %d/%d, want %d/%d", after.Cash, after.Held, before.Cash, before.Held)
			return
		}
		t.pass("payout declined by the mock PSP: request ended %s and the %d-unit hold was released back to cash (cash/held %d/%d, unchanged from before)", state, declineAmount, after.Cash, after.Held)
	})
	w.check("withdrawal.balance_and_player_view", func(t *T) {
		if !t.needMoney("withdrawal.payout_in_flight") {
			return
		}
		wl, ok := t.wallet(p1, asset)
		if !ok {
			return
		}
		wantHeld := int64(0)
		if w.facts["wd_main_state"] == "submitted" {
			wantHeld = amount
		}
		want := w.expected(p1, asset) - amount
		if wl.Cash != want || wl.Held != wantHeld || wl.Available != want {
			t.fail("cash/available/held=%d/%d/%d, want %d/%d/%d (pre-withdrawal %d - withdrawal %d)", wl.Cash, wl.Available, wl.Held, want, want, wantHeld, w.expected(p1, asset), amount)
			return
		}
		w.setExpected(p1, asset, want)
		pr := t.call("GET", "/v1/me/withdrawals/"+wdID, p1.access, nil)
		if !t.expect(pr, 200, "", "GET /v1/me/withdrawals/{id}") {
			return
		}
		if st := pickStr(pr.obj(), "state"); st != w.facts["wd_main_state"] {
			t.fail("player sees state %q, staff see %q", st, w.facts["wd_main_state"])
			return
		}
		t.pass("wallet matches the ledger movements (cash=available=%d held=%d); player and staff agree on state=%s", wl.Cash, wl.Held, w.facts["wd_main_state"])
	})
	w.check("withdrawal.backoffice_history", func(t *T) {
		if !t.needMoney("withdrawal.payout_in_flight") {
			return
		}
		r := t.acall("FINANCE2", "GET", "/v1/admin/withdrawals/history?limit=100", nil)
		if !t.expect(r, 200, "", "GET /v1/admin/withdrawals/history") {
			return
		}
		for _, it := range items(r) {
			if pickStr(it, "id") == wdID {
				t.pass("request %s in the Back Office history (state=%s)", wdID, pickStr(it, "state"))
				return
			}
		}
		t.fail("request %s missing from the Back Office withdrawal history", wdID)
	})
}

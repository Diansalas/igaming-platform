//go:build stagingacceptance

package main

import (
	"fmt"
	"strings"
	"time"
)

var reconStreams = []string{"ledger_vs_projection", "payment_statement", "sportsbook_settlement"}

// reconReader picks a staff role that may read every reconciliation stream for the synthetic tenant. A platform admin
// is allowed for payment_statement and ledger_vs_projection; the sportsbook stream also needs tenant staff.
func (w *world) reconReader(stream string) string {
	if stream == "sportsbook_settlement" {
		return w.roleOr("FINANCE1", "TENANT_ADMIN", "COMPLIANCE")
	}
	return w.roleOr("ADMIN", "FINANCE1", "COMPLIANCE")
}

func (w *world) reconciliationChecks() {
	w.check("recon.runs_visible_and_no_open_mismatch", func(t *T) {
		if !t.need("setup.resolve_tenant_brand") {
			return
		}
		deadline := w.now().Add(w.cfg.ReconWait)
		for {
			counts := map[string]int{}
			mism := map[string]int{}
			for _, st := range reconStreams {
				role := w.reconReader(st)
				if role == "" {
					t.skipPrereq("no staff configured that may read the %s reconciliation stream", st)
					return
				}
				r := t.acall(role, "GET", "/v1/admin/tenants/"+w.tenantID+"/reconciliation/runs?stream="+st+"&limit=20", nil)
				if r == nil {
					return
				}
				if !t.expect(r, 200, "", "reconciliation runs "+st) {
					return
				}
				counts[st] = len(items(r))
				for _, it := range items(r) {
					if s := pickStr(it, "status"); s != "clean" && s != "mismatches_found" {
						t.fail("run of stream %s has status %q", st, s)
						return
					}
				}
				mr := t.acall(role, "GET", "/v1/admin/tenants/"+w.tenantID+"/reconciliation/mismatches?stream="+st+"&status=open&limit=50", nil)
				if mr == nil {
					return
				}
				if !t.expect(mr, 200, "", "reconciliation mismatches "+st) {
					return
				}
				mism[st] = len(items(mr))
			}
			open := 0
			for _, n := range mism {
				open += n
			}
			if open > 0 {
				t.fail("%d OPEN reconciliation mismatch(es) for the synthetic tenant (P1 if ledger_vs_projection): runs=%v open=%v", open, counts, mism)
				return
			}
			if counts["ledger_vs_projection"] > 0 {
				t.pass("reconciliation is visible and clean: runs=%v, open mismatches=%v (a non-zero ledger_vs_projection drift would be a P1)", counts, mism)
				return
			}
			if w.now().After(deadline) {
				t.skipPrereq("no ledger_vs_projection reconciliation run exists yet (the cycle is hourly: RECONCILIATION_INTERVAL_SECONDS); re-run after the first cycle or pass RECON_WAIT_SECONDS. Open mismatches: %v", mism)
				return
			}
			w.sleep(15 * time.Second)
		}
	})
	w.check("recon.payments_and_attempts_views", func(t *T) {
		if !t.need("setup.resolve_tenant_brand") {
			return
		}
		role := w.roleOr("FINANCE1", "ADMIN")
		if role == "" {
			t.skipPrereq("no staff for the payment-attempt read surface")
			return
		}
		r := t.acall(role, "GET", "/v1/admin/tenants/"+w.tenantID+"/payment-attempts?limit=50", nil)
		if r == nil || !t.expect(r, 200, "", "payment attempts") {
			return
		}
		m := r.obj()
		n := len(items(r))
		if _, ok := m["items"]; !ok {
			if a := m["attempts"]; a != nil {
				n = len(a.([]any))
			}
		}
		if w.cfg.MarketExpect == "open" && n == 0 {
			t.fail("no payment attempts visible although deposits and a payout were made")
			return
		}
		if strings.Contains(string(r.Body), "provider_reference\":\"") {
			t.fail("the payment-attempt view exposes a provider reference text")
			return
		}
		t.pass("%d payment attempt(s) visible read-only to %s; no provider reference text exposed", n, role)
	})
}

func (w *world) alertingChecks() {
	w.check("alerting.status", func(t *T) {
		if !t.need("auth.staff_login_mfa") {
			return
		}
		r := t.acall("ADMIN", "GET", "/v1/admin/alerting/status", nil)
		if r == nil || !t.expect(r, 200, "", "GET /v1/admin/alerting/status") {
			return
		}
		sev, _ := pick(r.obj(), "severities").([]any)
		if len(sev) == 0 {
			t.fail("no per-severity readiness in the answer: %s", r.brief())
			return
		}
		var parts []string
		for _, s := range sev {
			parts = append(parts, fmt.Sprintf("%s:routes=%v/enabled=%v/human=%v/ready=%v", pickStr(s, "severity"), pick(s, "routes_configured"), pick(s, "routes_enabled"), pick(s, "human_notification_routes"), pick(s, "ready")))
		}
		w.facts["alerting_ready"] = fmt.Sprint(pick(r.obj(), "ready"))
		t.pass("endpoint answers; ready=%v; %s", pick(r.obj(), "ready"), strings.Join(parts, " "))
	})
	w.check("alerting.alerts_list", func(t *T) {
		if !t.need("auth.staff_login_mfa") {
			return
		}
		r := t.acall("ADMIN", "GET", "/v1/admin/alerts?limit=50", nil)
		if r == nil || !t.expect(r, 200, "", "GET /v1/admin/alerts") {
			return
		}
		kinds := map[string]int{}
		for _, it := range items(r) {
			kinds[pickStr(it, "kind")]++
		}
		t.pass("%d durable alert row(s) listed, kinds=%v", len(items(r)), kinds)
	})
	w.check("alerting.delivery_to_receiver", func(t *T) {
		t.skipManual("webhook/e-mail delivery is verified by the operator at the receiver (plan 10.1/10.2): the suite sees the durable alert row and the dispatcher configuration, never the receiver. The alert row raised by killswitch.engage_raises_alert is the trigger to look for")
	})
}

func (w *world) killSwitchChecks() {
	var ksID, reqID string
	released := false
	provider := "mock-payments"
	base := func() string { return "/v1/admin/tenants/" + w.tenantID + "/payments/kill-switches" }
	// release is also the cleanup: whatever happened above, the synthetic tenant must not be left with deposits stopped.
	release := func(t *T) bool {
		if ksID == "" || released {
			return true
		}
		a1, a2 := w.actor("ADMIN"), w.actor("ADMIN2")
		if a1 == nil || a2 == nil {
			return false
		}
		if err := a1.freshCritical(); err != nil {
			t.fail("%v", err)
			return false
		}
		rr := t.acall("ADMIN", "POST", base()+"/"+ksID+"/release-requests", map[string]any{"reason_code": "staging_acceptance"})
		if rr == nil {
			return false
		}
		if rr.Status != 201 && rr.Status != 200 {
			t.fail("request release: %s", rr.brief())
			return false
		}
		reqID = pickStr(rr.obj(), "id")
		if err := a2.freshCritical(); err != nil {
			t.fail("%v", err)
			return false
		}
		ap := t.acall("ADMIN2", "POST", "/v1/admin/tenants/"+w.tenantID+"/payments/kill-switch-release-requests/"+reqID+"/approve", map[string]any{})
		if ap == nil {
			return false
		}
		if !ap.ok() {
			t.fail("approve release: %s", ap.brief())
			return false
		}
		released = true
		return true
	}
	w.check("killswitch.engage_raises_alert", func(t *T) {
		if !w.cfg.KillSwitchDrill {
			t.skipPrereq("KILL_SWITCH_DRILL=false")
			return
		}
		if !t.need("setup.resolve_tenant_brand") {
			return
		}
		if w.actor("ADMIN") == nil || w.actor("ADMIN2") == nil {
			t.skipPrereq("the drill needs ADMIN and ADMIN2 (engage, then four-eyes release)")
			return
		}
		engagedAt := w.now()
		r := t.acall("ADMIN", "POST", base(), map[string]any{"provider_scope": provider, "operation_scope": "deposit", "reason_code": "staging_acceptance"})
		if r == nil {
			return
		}
		if r.Status != 201 && r.Status != 200 {
			t.fail("engage the deposit kill switch: %s", r.brief())
			return
		}
		ksID = pickStr(r.obj(), "id")
		if on, _ := pick(r.obj(), "engaged").(bool); !on || ksID == "" {
			t.fail("the switch is not reported engaged: %s", r.brief())
			return
		}
		// the durable alert is written after commit: poll briefly
		deadline := w.now().Add(30 * time.Second)
		for {
			al := t.acall("ADMIN", "GET", "/v1/admin/alerts?kind=payment.kill_switch_engaged&limit=20", nil)
			if al == nil {
				return
			}
			if al.Status == 400 {
				al = t.acall("ADMIN", "GET", "/v1/admin/alerts?limit=200", nil)
				if al == nil {
					return
				}
			}
			if !t.expect(al, 200, "", "list alerts") {
				return
			}
			for _, it := range items(al) {
				if pickStr(it, "kind") == "payment.kill_switch_engaged" {
					w.facts["synthetic_alert_kind"] = "payment.kill_switch_engaged"
					t.pass("kill switch engaged (single actor, no step-up) and the durable alert payment.kill_switch_engaged (severity %s, state %s) is listed; engage-to-visible %.1fs; the operator now looks for the delivery at the receiver", pickStr(it, "severity"), pickStr(it, "state"), w.now().Sub(engagedAt).Seconds())
					return
				}
			}
			if w.now().After(deadline) {
				t.fail("kill switch engaged but no payment.kill_switch_engaged alert appeared within 30s")
				return
			}
			w.sleep(2 * time.Second)
		}
	})
	w.check("killswitch.blocks_deposit", func(t *T) {
		if !t.needMoney("killswitch.engage_raises_alert") {
			return
		}
		p1 := w.players["p1"]
		before, ok := t.wallet(p1, w.cfg.Asset)
		if !ok {
			return
		}
		r, id, st := t.deposit(p1, w.cfg.Asset, 900, "acc-"+w.cfg.RunID+"-dep-ks")
		if r == nil {
			return
		}
		if r.ok() && id != "" && st == "pending" {
			t.fail("a deposit was initiated while the deposit kill switch is engaged: %s", r.brief())
			return
		}
		after, ok := t.wallet(p1, w.cfg.Asset)
		if !ok {
			return
		}
		if after.Cash != before.Cash {
			t.fail("balance changed during containment")
			return
		}
		t.pass("deposit initiation withheld while engaged (HTTP %d status=%q %s); balance untouched", r.Status, st, r.code())
	})
	w.check("killswitch.four_eyes_release", func(t *T) {
		if ksID == "" {
			if !t.need("killswitch.engage_raises_alert") {
				return
			}
		}
		// requester and approver must be different people: the same admin approving its own request must be refused
		a1 := w.actor("ADMIN")
		if err := a1.freshCritical(); err != nil {
			t.fail("%v", err)
			return
		}
		rr := t.acall("ADMIN", "POST", base()+"/"+ksID+"/release-requests", map[string]any{"reason_code": "staging_acceptance"})
		if rr == nil {
			return
		}
		if rr.Status != 201 && rr.Status != 200 {
			t.fail("request release: %s", rr.brief())
			return
		}
		reqID = pickStr(rr.obj(), "id")
		self := t.acall("ADMIN", "POST", "/v1/admin/tenants/"+w.tenantID+"/payments/kill-switch-release-requests/"+reqID+"/approve", map[string]any{})
		if self == nil {
			return
		}
		if self.ok() {
			t.fail("the requester approved its own release request (four-eyes violated): %s", self.brief())
			return
		}
		a2 := w.actor("ADMIN2")
		if err := a2.freshCritical(); err != nil {
			t.fail("%v", err)
			return
		}
		ap := t.acall("ADMIN2", "POST", "/v1/admin/tenants/"+w.tenantID+"/payments/kill-switch-release-requests/"+reqID+"/approve", map[string]any{})
		if ap == nil {
			return
		}
		if !ap.ok() {
			t.fail("approve release as a second admin: %s", ap.brief())
			return
		}
		released = true
		g := t.acall("ADMIN", "GET", base()+"/"+ksID, nil)
		if g == nil || !t.expect(g, 200, "", "read the switch") {
			return
		}
		if on, _ := pick(g.obj(), "engaged").(bool); on {
			t.fail("the switch is still engaged after an approved release")
			return
		}
		t.pass("self-approval refused (HTTP %d); a second platform admin approved; the switch reads engaged=false", self.Status)
	})
	w.check("killswitch.deposit_works_after_release", func(t *T) {
		if !t.needMoney("killswitch.four_eyes_release") {
			return
		}
		p1 := w.players["p1"]
		_, id, st := t.deposit(p1, w.cfg.Asset, 900, "acc-"+w.cfg.RunID+"-dep-ks2")
		if id == "" || st != "pending" {
			t.fail("deposit still refused after the release (status %q)", st)
			return
		}
		cb := t.call("POST", "/v1/me/deposits/"+id+"/simulate-callback", p1.access, nil)
		if cb == nil || !t.expect(cb, 200, "", "settle") {
			return
		}
		t.pass("deposit initiation and settlement work again after the four-eyes release")
	})
	// Cleanup: never leave the tenant contained. Reported as its own check so a failure is loud.
	w.check("killswitch.cleanup_not_left_engaged", func(t *T) {
		if ksID == "" {
			t.skipPrereq("no kill switch was engaged by this run")
			return
		}
		if released {
			t.pass("the kill switch engaged by this run was released")
			return
		}
		if !release(t) {
			if !t.done {
				t.fail("COULD NOT RELEASE the deposit kill switch %s: the synthetic tenant is still contained; release it per kill-switch-and-containment.md section 1.3", ksID)
			}
			return
		}
		t.pass("cleanup released the kill switch")
	})
}

func (w *world) observabilityChecks() {
	w.check("observability.error_envelope_carries_request_id", func(t *T) {
		r := t.call("GET", "/v1/admin/tenants", "", nil)
		if r == nil {
			return
		}
		if rid := pickStr(r.obj(), "request_id"); rid == "" || rid != r.Header.Get("X-Request-Id") {
			t.fail("error body request_id=%q differs from the X-Request-Id header %q: log correlation would break", rid, r.Header.Get("X-Request-Id"))
			return
		}
		t.pass("error bodies echo the same request_id as the response header, which the structured http_request log line also carries")
	})
	w.check("observability.cloudwatch_http_request_lines", func(t *T) {
		t.skipManual("structured `http_request` lines with request_id are read by the operator in CloudWatch /ecs/igaming-staging/platform-api; every request of this run carries X-Request-Id acc-%s-NNNN (README has the Logs Insights queries)", w.cfg.RunID)
	})
	w.check("observability.metrics", func(t *T) {
		t.skipManual("the A9 /metrics series are bound to loopback inside the task and there is no scrape path (plan G4): metric verification stays local; /metrics is proven NOT public by health.metrics_not_public")
	})
}

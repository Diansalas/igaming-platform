//go:build stagingacceptance

package main

import (
	"sort"
	"strings"
)

// backofficeChecks read the Back Office surfaces an operator uses to inspect the synthetic activity (plan 6.6: "Back
// Office audit and ledger views"). They are read-only.
func (w *world) backofficeChecks() {
	w.check("bo.player_directory", func(t *T) {
		if !t.needMoney("player.me") {
			return
		}
		role := w.roleOr("TENANT_ADMIN", "COMPLIANCE", "FINANCE1")
		if role == "" {
			t.skipPrereq("no tenant staff configured for the player directory")
			return
		}
		p1 := w.players["p1"]
		lst := t.acall(role, "GET", "/v1/admin/players?q=acc-"+w.cfg.RunID+"&limit=100", nil)
		if lst == nil {
			return
		}
		if lst.Status == 403 {
			// finance does not hold player:read; fall back to the tenant admin when configured
			if role = w.roleOr("TENANT_ADMIN", "COMPLIANCE"); role == "" {
				t.skipPrereq("the configured tenant staff may not read players (player:read needs tenant_admin or compliance)")
				return
			}
			if lst = t.acall(role, "GET", "/v1/admin/players?q=acc-"+w.cfg.RunID+"&limit=100", nil); lst == nil {
				return
			}
		}
		if !t.expect(lst, 200, "", "GET /v1/admin/players") {
			return
		}
		found := false
		for _, it := range items(lst) {
			if pickStr(it, "id") == p1.id {
				found = true
			}
			if bid := pickStr(it, "brand_id"); bid != "" && bid != w.brandID {
				t.fail("the directory of tenant %s lists a player of another brand (%s)", w.cfg.Tenant, bid)
				return
			}
		}
		if !found {
			t.fail("the synthetic player %s is not in the Back Office directory", p1.id)
			return
		}
		one := t.acall(role, "GET", "/v1/admin/players/"+p1.id, nil)
		if one == nil || !t.expect(one, 200, "", "GET /v1/admin/players/{id}") {
			return
		}
		if pickStr(one.obj(), "status") != "active" {
			t.fail("Back Office shows status %q for the activated player", pickStr(one.obj(), "status"))
			return
		}
		t.pass("%s finds the synthetic player in the directory (%d match(es)) and reads it by id: status=active kyc_tier=%v", role, len(items(lst)), pick(one.obj(), "kyc_tier"))
	})
	w.check("bo.audit_trail", func(t *T) {
		if !t.need("auth.staff_login_mfa") {
			return
		}
		role := w.roleOr("TENANT_ADMIN", "COMPLIANCE")
		if role == "" {
			t.skipPrereq("audit:read for the tenant needs TENANT_ADMIN or COMPLIANCE (not configured)")
			return
		}
		r := t.acall(role, "GET", "/v1/admin/audit-log?limit=200", nil)
		if r == nil || !t.expect(r, 200, "", "GET /v1/admin/audit-log") {
			return
		}
		actions := map[string]int{}
		for _, it := range items(r) {
			actions[pickStr(it, "action")]++
		}
		if len(items(r)) == 0 {
			t.fail("the tenant audit log is empty after a full acceptance run")
			return
		}
		if w.facts["p1_withdrawal_id"] != "" {
			found := false
			for a := range actions {
				if strings.Contains(a, "withdraw") {
					found = true
				}
			}
			if !found {
				t.fail("withdrawal flow ran but no withdrawal audit action is visible in the newest 200 tenant audit rows")
				return
			}
		}
		pr := t.acall("ADMIN", "GET", "/v1/admin/platform/audit-log?limit=50", nil)
		if pr == nil || !t.expect(pr, 200, "", "GET /v1/admin/platform/audit-log") {
			return
		}
		keys := make([]string, 0, len(actions))
		for a := range actions {
			keys = append(keys, a)
		}
		sort.Strings(keys)
		if len(keys) > 12 {
			keys = keys[:12]
		}
		t.pass("tenant audit log lists %d row(s), %d distinct action(s) (e.g. %s); the platform audit log lists %d row(s)", len(items(r)), len(actions), strings.Join(keys, ", "), len(items(pr)))
	})
}

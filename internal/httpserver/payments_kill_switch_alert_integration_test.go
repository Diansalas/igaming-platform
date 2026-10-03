//go:build integration

package httpserver

import (
	"net/http"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// ADR 0102 8 row 8: the engage raises payment.kill_switch_engaged (p2),
// post-commit, in the engage's own scope, keyed by the switch id, with only the
// allowlisted attributes.
func TestIWire_KillSwitch_Engage_RaisesP2_TenantAndPlatformScope(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	resp := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "incident_iw_1"})
	if resp.status != http.StatusOK {
		t.Fatalf("engage: %d %s", resp.status, resp.body)
	}
	var ks killSwitchDTO
	resp.decode(t, &ks)

	rows := alertinject.Find(alertinject.ForSubject(t, a.pool, tenant), string(alerting.KindPaymentKillSwitchEngaged))
	if len(rows) != 1 {
		t.Fatalf("want exactly one kill-switch alert, got %+v", rows)
	}
	r := rows[0]
	if r.Severity != "p2" || r.Discriminator != "switch:"+ks.ID {
		t.Fatalf("severity/discriminator: %+v", r)
	}
	if r.Attributes["provider_scope"] != "*" || r.Attributes["operation_scope"] != "deposit" || r.Attributes["reason_code"] != "incident_iw_1" ||
		r.Attributes["changed_by_scope"] != "tenant" || r.Attributes["is_platform_takeover"] != false {
		t.Fatalf("attributes: %v", r.Attributes)
	}

	// A platform admin takeover of the tenant-engaged switch: same switch, same open
	// alert (stable key), a second occurrence.
	plat := a.platformAdmin()
	ptok := a.token(plat, [16]byte{}, auth.RolePlatformAdmin, auth.PrincipalStaff)
	resp = a.do("POST", base+"/kill-switches", ptok, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "takeover_iw"})
	if resp.status != http.StatusOK {
		t.Fatalf("platform engage: %d %s", resp.status, resp.body)
	}
	rows = alertinject.Find(alertinject.ForSubject(t, a.pool, tenant), string(alerting.KindPaymentKillSwitchEngaged))
	if len(rows) != 1 || rows[0].Occurrences != 2 {
		t.Fatalf("a repeated engage of the same switch is one open alert with 2 occurrences: %+v", rows)
	}
}

// LF test 10: a kill-switch engage is unaffected by an injected PERSISTENT
// failure in the post-commit raise, first P0001 then 40P01 (deadlock class). The
// response, the switch and its audit row are exactly as without the failure.
func TestIWire_KillSwitch_Engage_UnaffectedByPersistentRaiseFailure(t *testing.T) {
	for _, code := range []string{"P0001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			a := newKSAPI(t)
			tenant := a.tenant()
			admin := a.tenantStaff(tenant, "tenant_admin")
			tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
			base := "/v1/admin/tenants/" + tenant.String() + "/payments"
			alertinject.Install(t, a.pool, tenant, alertinject.Persistent, code)

			resp := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "incident_iw_2"})
			if resp.status != http.StatusOK {
				t.Fatalf("engage must succeed regardless of the alert path: %d %s", resp.status, resp.body)
			}
			var ks killSwitchDTO
			resp.decode(t, &ks)
			if !ks.Engaged {
				t.Fatalf("the switch must be engaged: %+v", ks)
			}
			if a.auditCount(tenant, "payments_kill_switch.engage") != 1 {
				t.Fatal("the engage audit row must be committed")
			}
			if rows := alertinject.ForSubject(t, a.pool, tenant); len(rows) != 0 {
				t.Fatalf("persistent failure: no tenant-visible alert expected, got %+v", rows)
			}
		})
	}
}

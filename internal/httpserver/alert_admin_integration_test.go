//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

func iwRaiseAlert(t *testing.T, a *ksAPI, tenant uuid.UUID, disc string) {
	t.Helper()
	if err := alerting.RaiseDetached(context.Background(), alerting.NewTenantRunner(a.pool, tenant), alerting.Alert{
		Kind: alerting.KindPaymentKillSwitchEngaged, SubjectTenantID: tenant, Discriminator: disc,
		Attributes: map[string]alerting.AttrValue{"reason_code": "iw-admin"},
	}); err != nil {
		t.Fatalf("raise: %v", err)
	}
}

func iwAlertState(t *testing.T, a *ksAPI, tenant uuid.UUID, disc string) (id uuid.UUID, state string, ackedBy, resolvedBy *uuid.UUID, reason *string) {
	t.Helper()
	// ForSubject hides ids; read directly through the tenant's subject-read family.
	if err := a.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, state, acked_by, resolved_by, resolve_reason_code FROM alerts WHERE discriminator = $1`, disc).
			Scan(&id, &state, &ackedBy, &resolvedBy, &reason)
	}); err != nil {
		t.Fatalf("read alert: %v", err)
	}
	return
}

func iwAlertAudits(t *testing.T, a *ksAPI, tenant uuid.UUID, action string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := a.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata, actor_id::text, COALESCE(ip_address::text, '') FROM audit_log WHERE action = $1 AND subject_tenant_id = $2`, action, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			var actor, ip string
			if err := rows.Scan(&raw, &actor, &ip); err != nil {
				return err
			}
			m := map[string]any{}
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			m["_actor"] = actor
			m["_ip"] = ip
			out = append(out, m)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	return out
}

func TestAlertAdmin_AckResolveAudited_PlatformScopeOnly(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	disc := "switch:" + uuid.NewString()
	iwRaiseAlert(t, a, tenant, disc)
	id, state, _, _, _ := iwAlertState(t, a, tenant, disc)
	if state != "open" {
		t.Fatalf("setup state %s", state)
	}
	plat := a.platformAdmin()
	ptok := a.token(plat, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	base := "/v1/admin/alerts/" + id.String()

	resp := a.do("POST", base+"/ack", ptok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("ack: %d %s", resp.status, resp.body)
	}
	_, state, ackedBy, _, _ := iwAlertState(t, a, tenant, disc)
	if state != "acked" || ackedBy == nil || *ackedBy != plat {
		t.Fatalf("state=%s acked_by=%v: the actor must come from the validated session", state, ackedBy)
	}
	audits := iwAlertAudits(t, a, tenant, "alerts.ack")
	if len(audits) != 1 || audits[0]["_actor"] != plat.String() || audits[0]["before_state"] != "open" || audits[0]["after_state"] != "acked" ||
		audits[0]["kind"] != string(alerting.KindPaymentKillSwitchEngaged) {
		t.Fatalf("exactly one ack audit with actor and before/after, visible to the subject tenant: %+v", audits)
	}
	// QA O-5: the audit record carries the caller's IP (the test server's loopback).
	if ip, _ := audits[0]["_ip"].(string); !strings.Contains(ip, "127.0.0.1") {
		t.Fatalf("the ack audit must record the caller IP, got %q", ip)
	}

	// A resolve needs a closed-shape reason code.
	if r := a.do("POST", base+"/resolve", ptok, map[string]any{}); r.status != http.StatusBadRequest && r.status != http.StatusUnprocessableEntity {
		t.Fatalf("resolve without reason_code must be rejected, got %d", r.status)
	}
	if r := a.do("POST", base+"/resolve", ptok, map[string]any{"reason_code": "Free Text Reason!"}); r.status == http.StatusOK {
		t.Fatal("resolve with a free-text reason must be rejected")
	}
	if got := len(iwAlertAudits(t, a, tenant, "alerts.resolve")); got != 0 {
		t.Fatalf("a rejected resolve must leave no audit row, got %d", got)
	}
	resp = a.do("POST", base+"/resolve", ptok, map[string]any{"reason_code": "fixed_upstream"})
	if resp.status != http.StatusOK {
		t.Fatalf("resolve: %d %s", resp.status, resp.body)
	}
	_, state, _, resolvedBy, reason := iwAlertState(t, a, tenant, disc)
	if state != "resolved" || resolvedBy == nil || *resolvedBy != plat || reason == nil || *reason != "fixed_upstream" {
		t.Fatalf("state=%s resolved_by=%v reason=%v", state, resolvedBy, reason)
	}
	audits = iwAlertAudits(t, a, tenant, "alerts.resolve")
	if len(audits) != 1 || audits[0]["reason_code"] != "fixed_upstream" || audits[0]["before_state"] != "acked" {
		t.Fatalf("resolve audit: %+v", audits)
	}

	// A resolved alert is terminal: ack/resolve again is a conflict, no new audit.
	if r := a.do("POST", base+"/ack", ptok, nil); r.status != http.StatusConflict {
		t.Fatalf("ack of a resolved alert: %d, want 409", r.status)
	}
	if r := a.do("POST", base+"/resolve", ptok, map[string]any{"reason_code": "again"}); r.status != http.StatusConflict {
		t.Fatalf("resolve of a resolved alert: %d, want 409", r.status)
	}
	if len(iwAlertAudits(t, a, tenant, "alerts.ack")) != 1 || len(iwAlertAudits(t, a, tenant, "alerts.resolve")) != 1 {
		t.Fatal("refused transitions must not write audit rows")
	}

	// Re-raising after a resolve creates a NEW open alert (ADR 0102 3.2).
	iwRaiseAlert(t, a, tenant, disc)
	rows := alertinject.Find(alertinject.ForSubject(t, a.pool, tenant), string(alerting.KindPaymentKillSwitchEngaged))
	var open, resolved int
	for _, r := range rows {
		if r.Discriminator != disc {
			continue
		}
		if r.State == "open" {
			open++
		}
		if r.State == "resolved" {
			resolved++
		}
	}
	if open != 1 || resolved != 1 {
		t.Fatalf("after resolve + re-raise: open=%d resolved=%d, want 1/1", open, resolved)
	}
}

// Authorization: only a platform admin (alert:manage) can ack or resolve. A
// tenant admin cannot, even for an alert whose subject is its own tenant; a
// player and an unauthenticated caller cannot. Refusals change nothing.
func TestAlertAdmin_Authorization_TenantAndPlayerAndAnonymousRefused(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	disc := "switch:" + uuid.NewString()
	iwRaiseAlert(t, a, tenant, disc)
	id, _, _, _, _ := iwAlertState(t, a, tenant, disc)
	base := "/v1/admin/alerts/" + id.String()

	tenantAdmin := a.tenantStaff(tenant, "tenant_admin")
	tatok := a.token(tenantAdmin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	playertok := a.token(a.player(tenant), tenant, auth.RolePlayer, auth.PrincipalPlayer)

	for name, tok := range map[string]string{"tenant_admin": tatok, "player": playertok} {
		for _, op := range []string{"/ack", "/resolve"} {
			r := a.do("POST", base+op, tok, map[string]any{"reason_code": "x_y"})
			if r.status != http.StatusForbidden {
				t.Fatalf("%s %s: status %d, want 403", name, op, r.status)
			}
		}
	}
	if r := a.do("POST", base+"/ack", "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", r.status)
	}
	_, state, ackedBy, resolvedBy, _ := iwAlertState(t, a, tenant, disc)
	if state != "open" || ackedBy != nil || resolvedBy != nil {
		t.Fatalf("a refused request changed the alert: state=%s", state)
	}
	if len(iwAlertAudits(t, a, tenant, "alerts.ack"))+len(iwAlertAudits(t, a, tenant, "alerts.resolve")) != 0 {
		t.Fatal("refused requests must not write success audit rows")
	}

	// The platform admin can; unknown id is 404; bad id is 400.
	plat := a.platformAdmin()
	ptok := a.token(plat, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	if r := a.do("POST", "/v1/admin/alerts/"+uuid.NewString()+"/ack", ptok, nil); r.status != http.StatusNotFound {
		t.Fatalf("unknown alert: %d, want 404", r.status)
	}
	if r := a.do("POST", "/v1/admin/alerts/not-a-uuid/ack", ptok, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad id: %d, want 400", r.status)
	}
}

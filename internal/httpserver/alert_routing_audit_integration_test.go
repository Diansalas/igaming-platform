//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	tenantctx "github.com/Diansalas/igaming-platform/internal/tenant"
)

func (a *arAPI) auditTargets(tenantID uuid.UUID, action string) []string {
	a.t.Helper()
	var out []string
	run := func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT COALESCE(target_id,'<null>') FROM audit_log WHERE action = $1 AND tenant_id IS NOT DISTINCT FROM $2 ORDER BY created_at, id`, action, nilIfZero(tenantID))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	}
	var err error
	if tenantID == uuid.Nil {
		err = a.w.Pool.WithPlatformAdmin(context.Background(), a.admin, run)
	} else {
		err = a.w.Pool.WithTenant(context.Background(), tenantID, run)
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return out
}

func nilIfZero(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// Security F3 / code review C3: the denied audit never records the raw, client-controlled path segment.
func TestAlertAdmin_DeniedAuditNeverRecordsAHostilePathSegment(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	hostile := strings.Repeat("A", 6000) + "%0A%7B%22forged%22:true%7D"
	tenantTok := a.token(a.tenantStaff(tenant, "tenant_admin"), tenant, auth.RoleTenantAdmin)
	complianceTok := a.token(a.admin, uuid.Nil, auth.RoleCompliance)
	segment, err := url.PathUnescape(hostile)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"ack", "resolve"} {
		if r := a.do("POST", "/v1/admin/alerts/"+hostile+"/"+op, tenantTok, map[string]any{"reason_code": "x_y"}); r.status != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403", op, r.status)
		}
		// ADR 0119 S4a: a platform admin's id under a compliance claim matches no live session (401 over HTTP); the
		// platform-scope denial and its audit are pinned behind the chain with the same claims.
		if r := a.do("POST", "/v1/admin/alerts/"+hostile+"/"+op, complianceTok, map[string]any{"reason_code": "x_y"}); r.status != http.StatusUnauthorized {
			t.Fatalf("%s with mismatched claims over HTTP: status %d, want 401", op, r.status)
		}
		if rec := behindChain(t, newAlertTransitionHandler(a.deps, op), "POST", "/v1/admin/alerts/x/"+op, map[string]string{"alertID": segment},
			tenantctx.Context{Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff), Subject: a.admin.String()},
			map[string]any{"reason_code": "x_y"}); rec.Code != http.StatusForbidden {
			t.Fatalf("%s behind the chain: status %d, want 403", op, rec.Code)
		}
		for _, targets := range [][]string{a.auditTargets(tenant, "alerts."+op), a.auditTargets(uuid.Nil, "alerts."+op)} {
			if len(targets) != 1 || targets[0] != "invalid_id" {
				t.Fatalf("%s: audit target ids = %q, want exactly [invalid_id] (never the raw segment)", op, targets)
			}
		}
	}
	// A well-formed id is recorded as the parsed UUID.
	id := uuid.New()
	a.do("POST", "/v1/admin/alerts/"+id.String()+"/ack", tenantTok, nil)
	got := a.auditTargets(tenant, "alerts.ack")
	if len(got) != 2 || got[1] != id.String() {
		t.Fatalf("parsed id not recorded: %q", got)
	}
}

// Security F5: a recipient-only change is visible in the audit without either value.
func TestAlertRoutingAPI_AuditShowsRecipientChangeWithoutTheValue(t *testing.T) {
	a := newARAPI(t, false)
	tok := a.adminToken()
	first := a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(map[string]any{"recipient_ref": "rota-one"})).json()
	second := a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(map[string]any{"recipient_ref": "rota-two", "supersedes_id": first["id"], "reason_code": "on_call_change"})).json()
	a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(map[string]any{"recipient_ref": "rota-two", "supersedes_id": second["id"], "reason_code": "correction"}))
	var changed []any
	for _, m := range a.platformAudits("alert_route.create") {
		if m["_outcome"] != "success" {
			continue
		}
		changed = append(changed, m["recipient_changed"])
		if raw, _ := json.Marshal(m); strings.Contains(string(raw), "rota-") {
			t.Fatalf("a recipient value reached the audit: %s", raw)
		}
	}
	if len(changed) != 3 || changed[0] != false || changed[1] != true || changed[2] != false {
		t.Fatalf("recipient_changed per write = %v, want [false true false]", changed)
	}
}

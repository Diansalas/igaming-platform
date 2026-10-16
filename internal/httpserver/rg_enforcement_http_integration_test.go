//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

// ADR 0115 S4-min HTTP surface through the real stack as igaming_runtime: the staff enforcement switch, the
// player options, and the status route's time-out visibility.

func TestRGEnforcement_HTTP_StaffSwitch(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	comp := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	body := map[string]any{"reason_code": "enable_rg_limits", "attestation_ref": "compliance officer, test"}

	// off by default
	r := w.call("GET", "/v1/admin/rg/enforcement", comp, "", nil)
	r.want(t, http.StatusOK, "")
	if m := r.obj(t); m["enforcement_enabled"] != false {
		t.Fatalf("enforcement must be off by default: %v", m)
	}

	// SR6-1: the switch is step-up CRITICAL. A stale session (older than the 5 minute window) is refused before any handler runs.
	stale := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, f.compA, f.tenantA, auth.RoleCompliance, 11*time.Minute)
	sr := w.call("POST", "/v1/admin/rg/enforcement", stale, "", body)
	if sr.status != http.StatusForbidden || !strings.Contains(string(sr.body), "step_up_required") {
		t.Fatalf("a stale session must get 403 step_up_required, got %d %s", sr.status, sr.body)
	}
	// SR6-1 guard: no jurisdiction configuration and no floors yet: refused 409 with the closed readiness detail, audited, nothing written.
	r = w.call("POST", "/v1/admin/rg/enforcement", comp, "", body)
	r.want(t, http.StatusConflict, "conflict")
	if !strings.Contains(string(r.body), "no_jurisdiction_referenced") {
		t.Fatalf("the refusal must name the readiness detail: %s", r.body)
	}
	w.tenantConfig(f.tenantA, f.jur)
	r = w.call("POST", "/v1/admin/rg/enforcement", comp, "", body)
	r.want(t, http.StatusConflict, "conflict")
	if !strings.Contains(string(r.body), "floor_missing_deposit") {
		t.Fatalf("the refusal must name the missing floor: %s", r.body)
	}
	r = w.call("GET", "/v1/admin/rg/enforcement", comp, "", nil)
	if m := r.obj(t); m["enforcement_enabled"] != false {
		t.Fatalf("a refused activation must write nothing: %v", m)
	}
	for _, kind := range []string{"deposit", "loss", "wager"} {
		w.floor(f.jur, kind, rgfix.Spec{"attestation_ref": "sr6-1-test", "asset_code": "EUR"})
	}

	// compliance switches it on for the tenant; a repeat answers 200 with the stored row
	r = w.call("POST", "/v1/admin/rg/enforcement", comp, "", body)
	r.want(t, http.StatusCreated, "")
	first := r.obj(t)
	if first["level"] != "tenant" || first["reason_code"] != "enable_rg_limits" {
		t.Fatalf("created row: %v", first)
	}
	r = w.call("POST", "/v1/admin/rg/enforcement", comp, "", body)
	r.want(t, http.StatusOK, "")
	if r.obj(t)["id"] != first["id"] {
		t.Fatal("a repeat must return the stored row")
	}
	r = w.call("GET", "/v1/admin/rg/enforcement", comp, "", nil)
	r.want(t, http.StatusOK, "")
	if m := r.obj(t); m["enforcement_enabled"] != true || len(m["items"].([]any)) != 1 {
		t.Fatalf("list after activation: %v", m)
	}

	// a brand-level row for a brand of this tenant; a brand of ANOTHER tenant is refused
	b := map[string]any{"brand_id": f.brandA1.String(), "reason_code": "enable_rg_limits", "attestation_ref": "compliance officer, test"}
	w.call("POST", "/v1/admin/rg/enforcement", comp, "", b).want(t, http.StatusCreated, "")
	b["brand_id"] = f.brandB.String()
	w.call("POST", "/v1/admin/rg/enforcement", comp, "", b).want(t, http.StatusBadRequest, "validation_error")

	// permissions and principals
	w.call("POST", "/v1/admin/rg/enforcement", w.staff(f.tenantA, f.supportA, auth.RoleSupport), "", body).want(t, http.StatusForbidden, "")
	w.call("POST", "/v1/admin/rg/enforcement", w.staff(f.tenantA, f.adminA, auth.RoleTenantAdmin), "", body).want(t, http.StatusForbidden, "") // read, not write
	w.call("GET", "/v1/admin/rg/enforcement", w.staff(f.tenantA, f.adminA, auth.RoleTenantAdmin), "", nil).want(t, http.StatusOK, "")
	w.call("POST", "/v1/admin/rg/enforcement", w.platform(w.ops.Requester), "", body).want(t, http.StatusForbidden, "")
	w.call("POST", "/v1/admin/rg/enforcement", w.player(f, f.acctA1), "", body).want(t, http.StatusForbidden, "")
	w.call("POST", "/v1/admin/rg/enforcement", "", "", body).want(t, http.StatusUnauthorized, "")

	// validation: empty fields, non-printable, unknown field
	for name, bad := range map[string]any{
		"empty reason":      map[string]any{"reason_code": "", "attestation_ref": "x"},
		"empty attestation": map[string]any{"reason_code": "x", "attestation_ref": ""},
		"blank reason":      map[string]any{"reason_code": "   ", "attestation_ref": "x"},
		"unknown field":     map[string]any{"reason_code": "x", "attestation_ref": "x", "tenant_id": f.tenantB.String()},
		"too long reason":   map[string]any{"reason_code": string(make([]byte, 65)), "attestation_ref": "x"},
	} {
		if r := w.call("POST", "/v1/admin/rg/enforcement", comp, "", bad); r.status != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", name, r.status, r.body)
		}
	}

	// tenant B sees nothing and is not enabled by tenant A's row
	r = w.call("GET", "/v1/admin/rg/enforcement", w.staff(f.tenantB, f.compB, auth.RoleCompliance), "", nil)
	r.want(t, http.StatusOK, "")
	if m := r.obj(t); m["enforcement_enabled"] != false {
		t.Fatalf("tenant B must not see or inherit tenant A's activation: %v", m)
	}

	// audit: one success row for the tenant row and one for the brand row
	var n int
	if err := w.owner.WithTenant(context.Background(), f.tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'rg.enforcement.activated'`, f.tenantA).Scan(&n)
	}); err != nil || n != 2 {
		t.Fatalf("want 2 rg.enforcement.activated audit rows (a repeat writes none), got %d err=%v", n, err)
	}
	if err := w.owner.WithTenant(context.Background(), f.tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'rg.enforcement.activation_denied'`, f.tenantA).Scan(&n)
	}); err != nil || n < 1 {
		t.Fatalf("a refused activation must be audited as rg.enforcement.activation_denied, got %d err=%v", n, err)
	}
}

func TestRGOptions_HTTP(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	player := w.player(f, f.acctA1)

	// no floors: every period of the vocabulary is offered, nothing is mandatory, engineering-default durations
	r := w.call("GET", "/v1/me/rg/options?asset_code=EUR", player, "", nil)
	r.want(t, http.StatusOK, "")
	m := r.obj(t)
	limits := m["limits"].([]any)
	if len(limits) != 3 || m["limit_enforcement_enabled"] != false {
		t.Fatalf("options without configuration: %v", m)
	}
	l0 := limits[0].(map[string]any)
	if l0["config_in_force"] != false || len(l0["allowed_periods"].([]any)) != 6 || l0["mandatory"] != false {
		t.Fatalf("no configuration: all periods, not mandatory: %v", l0)
	}
	if len(m["time_out_hours"].([]any)) == 0 || len(m["self_exclusion_days"].([]any)) == 0 || m["self_exclusion_indefinite_allowed"] != true {
		t.Fatalf("durations: %v", m)
	}

	// with floors: periods and the ceiling come from the composed configuration; the switch is reported
	for _, kind := range []string{"deposit", "loss", "wager"} {
		w.floor(f.jur, kind, rgfix.Spec{"attestation_ref": "rgs2-options-test", "mandatory": kind == "deposit"})
	}
	w.floor(f.jur, "time_out", rgfix.Spec{"attestation_ref": "rgs2-options-test", "allowed_durations_any": false, "allowed_durations": []string{"01:00:00", "24:00:00", "00:30:00"}})
	r = w.call("GET", "/v1/me/rg/options?asset_code=EUR", player, "", nil)
	r.want(t, http.StatusOK, "")
	m = r.obj(t)
	for _, raw := range m["limits"].([]any) {
		l := raw.(map[string]any)
		if l["config_in_force"] != true || l["ceiling"] != "5000" || len(l["allowed_periods"].([]any)) != 2 {
			t.Fatalf("a configured kind: %v", l)
		}
		if l["kind"] == "deposit" && l["mandatory"] != true {
			t.Fatalf("deposit is mandatory in this configuration: %v", l)
		}
		if l["increase_cooldown_seconds"] != float64(2) {
			t.Fatalf("cooldown seconds: %v", l)
		}
	}
	hours := m["time_out_hours"].([]any)
	if len(hours) != 2 || hours[0] != float64(1) || hours[1] != float64(24) { // 30 minutes is not reachable through a whole-hours API
		t.Fatalf("configured time-out durations (whole hours only): %v", hours)
	}

	// the switch is reported once compliance turns it on (SR6-1 guard: needs a configuration and the floors, set up here)
	w.tenantConfig(f.tenantA, f.jur)
	w.call("POST", "/v1/admin/rg/enforcement", w.staff(f.tenantA, f.compA, auth.RoleCompliance), "",
		map[string]any{"reason_code": "r", "attestation_ref": "a"}).want(t, http.StatusCreated, "")
	r = w.call("GET", "/v1/me/rg/options?asset_code=EUR", player, "", nil)
	if r.obj(t)["limit_enforcement_enabled"] != true {
		t.Fatal("the switch must be reported")
	}

	// validation and principals
	w.call("GET", "/v1/me/rg/options", player, "", nil).want(t, http.StatusBadRequest, "validation_error")
	w.call("GET", "/v1/me/rg/options?asset_code=bad%20asset!", player, "", nil).want(t, http.StatusBadRequest, "validation_error")
	w.call("GET", "/v1/me/rg/options?asset_code=EUR", "", "", nil).want(t, http.StatusUnauthorized, "")
	w.call("GET", "/v1/me/rg/options?asset_code=EUR", w.staff(f.tenantA, f.compA, auth.RoleCompliance), "", nil).want(t, http.StatusForbidden, "")
	// tenant B's player never sees tenant A's tenant-level activation
	r = w.call("GET", "/v1/me/rg/options?asset_code=EUR", w.player(f, f.acctB), "", nil)
	r.want(t, http.StatusOK, "")
	if r.obj(t)["limit_enforcement_enabled"] != false {
		t.Fatal("tenant A's activation must not be reported to tenant B's player")
	}
}

func TestRGStatus_HTTP_ShowsATimeOutAndTheOptionsDurationsAreAccepted(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	player := w.player(f, f.acctA1)
	r := w.call("GET", "/v1/me/rg/options?asset_code=EUR", player, "", nil)
	r.want(t, http.StatusOK, "")
	hours := r.obj(t)["time_out_hours"].([]any)
	d := int(hours[len(hours)-1].(float64))
	w.call("POST", "/v1/me/rg/time-out", player, uuid.NewString(), map[string]any{"duration_hours": d}).want(t, http.StatusCreated, "")
	r = w.call("GET", "/v1/me/rg/status", player, "", nil)
	r.want(t, http.StatusOK, "")
	var arr []map[string]any
	if err := json.Unmarshal(r.body, &arr); err != nil || len(arr) != 1 || arr[0]["restriction_type"] != "time_out" || arr[0]["active"] != true {
		t.Fatalf("status must list the active time-out: %s (%v)", r.body, err)
	}
	// a self-exclusion with a day count from the options list is accepted
	days := w.call("GET", "/v1/me/rg/options?asset_code=EUR", w.player(f, f.acctX), "", nil).obj(t)["self_exclusion_days"].([]any)
	w.call("POST", "/v1/me/rg/self-exclusion", w.player(f, f.acctX), "", map[string]any{"duration_days": int(days[0].(float64))}).want(t, http.StatusCreated, "")
}

// tenantConfig gives a tenant an in-effect jurisdiction configuration (EUR) for the jurisdiction code, as owner.
func (w *rgW) tenantConfig(tenant uuid.UUID, jurCode string) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		var jid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM jurisdictions WHERE code = $1`, jurCode).Scan(&jid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods, effective_from)
			VALUES ($1, $2, 'kyc1', 'aml1', 'rg1', '["EUR"]', '["card"]', now() - interval '1 minute') ON CONFLICT DO NOTHING`, tenant, jid)
		return err
	}); err != nil {
		w.t.Fatalf("tenant jurisdiction config: %v", err)
	}
}

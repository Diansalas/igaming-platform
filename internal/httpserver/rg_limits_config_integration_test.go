//go:build integration

package httpserver

// ADR 0115 slice S2: operator configuration rows, the jurisdiction floor four-eyes flow (request /
// approve / reject / cancel, signed actor proofs, audit) and the time-out, through the real HTTP stack.

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

func opDeposit(over rgBody) rgBody {
	c := rgBody{"control_kind": "deposit", "asset_code": "EUR", "enabled": true, "mandatory": false, "ceiling": rgBody{"value": "3000"},
		"increase_cooldown_seconds": 3600, "increase_requires_reconfirmation": true, "scope": "account",
		"allowed_periods": []string{"rolling_24h", "calendar_day"}, "calendar_tz": "UTC", "week_start": 1,
		"pending_intent_ttl": rgBody{"none": true}, "reason_code": "s2_operator", "attestation_ref": "operator-ref-1"}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func TestRGLimitsS2_Operator_TightenWritesAndComposes(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	comp := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	r := w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": opDeposit(nil)})
	r.want(t, 201, "")
	o := r.obj(t)
	if o["level"] != "tenant" || o["jurisdiction_code"] != f.jur || o["created_by"] != f.compA.String() {
		t.Fatalf("operator row: %v", o)
	}
	cfg := o["config"].(map[string]any)
	if cfg["ceiling"].(map[string]any)["value"] != "3000" || cfg["increase_cooldown_seconds"] != float64(3600) || cfg["pending_intent_ttl"].(map[string]any)["none"] != true {
		t.Fatalf("stored config echo: %v", cfg)
	}
	// a brand row, stricter still
	r = w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"brand_id": f.brandA1.String(), "config": opDeposit(rgBody{"ceiling": rgBody{"value": "2000"}})})
	r.want(t, 201, "")
	if r.obj(t)["level"] != "brand" || r.obj(t)["brand_id"] != f.brandA1.String() {
		t.Fatalf("brand row: %v", r.obj(t))
	}
	// the composition is strictest-wins: brand A1's player is held to 2000, brand A2's to 3000, tenant B to the floor's 5000
	pa1, pa2 := w.player(f, f.acctA1), w.player(f, f.acctA2)
	w.call("POST", "/v1/me/rg/limits", pa1, "o1", setLimit("deposit", "EUR", "rolling_24h", "2001")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	w.call("POST", "/v1/me/rg/limits", pa1, "o2", setLimit("deposit", "EUR", "rolling_24h", "2000")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", pa2, "o3", setLimit("deposit", "EUR", "rolling_24h", "3001")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	w.call("POST", "/v1/me/rg/limits", pa2, "o4", setLimit("deposit", "EUR", "rolling_24h", "3000")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", w.player(f, f.acctB), "o5", setLimit("deposit", "EUR", "rolling_24h", "5000")).want(t, 201, "") // tenant B: floor only
	// the operator's longer cooldown and reconfirmation apply: an increase now waits 3600 s and needs a confirm
	inc := w.call("POST", "/v1/me/rg/limits", pa2, "o6", setLimit("deposit", "EUR", "rolling_24h", "3000"))
	inc.want(t, 409, "RG_NO_CHANGE")
	w.call("POST", "/v1/me/rg/limits", pa2, "o7", setLimit("deposit", "EUR", "rolling_24h", "100")).want(t, 201, "")
	up := w.call("POST", "/v1/me/rg/limits", pa2, "o8", setLimit("deposit", "EUR", "rolling_24h", "200"))
	up.want(t, 202, "")
	if d := parseTime(up.obj(t)["effective_at"]).Sub(parseTime(up.obj(t)["requested_at"])); d < 3599*time.Second || d > 3601*time.Second || up.obj(t)["requires_confirmation"] != true {
		t.Fatalf("operator cooldown / reconfirmation: %v", up.obj(t))
	}
	// listing: the floor rows and this tenant's rows, with the floor's creator redacted
	ls := w.call("GET", "/v1/admin/rg/limit-configs?control_kind=deposit&limit=200", comp, "", nil)
	ls.want(t, 200, "")
	if strings.Contains(string(ls.body), w.ops.Requester.String()) || strings.Contains(string(ls.body), w.ops.Approver1.String()) {
		t.Fatalf("a tenant caller never sees a platform staff id: %s", ls.body)
	}
	levels := map[string]int{}
	for _, it := range ls.obj(t)["items"].([]any) {
		m := it.(map[string]any)
		if m["jurisdiction_code"] != f.jur {
			continue
		}
		levels[m["level"].(string)]++
		if m["level"] == "floor" && m["created_by"] != nil {
			t.Errorf("a floor row's creator is platform staff and must be null for a tenant caller: %v", m)
		}
		if m["level"] == "tenant" && m["created_by"] != f.compA.String() {
			t.Errorf("an operator row shows its own tenant's staff: %v", m)
		}
	}
	if levels["floor"] != 1 || levels["tenant"] != 1 || levels["brand"] != 1 {
		t.Fatalf("levels in view: %v", levels)
	}
	// tenant B sees the floor but none of tenant A's operator rows
	lb := w.call("GET", "/v1/admin/rg/limit-configs?control_kind=deposit&limit=200", w.staff(f.tenantB, f.compB, auth.RoleCompliance), "", nil)
	lb.want(t, 200, "")
	if strings.Contains(string(lb.body), "operator-ref-1") || strings.Contains(string(lb.body), f.compA.String()) {
		t.Fatalf("tenant B must not see tenant A's operator rows: %s", lb.body)
	}
	// audit: before / after and the attestation reference
	au := w.auditRows(f.tenantA, "rg.limit_config.operator_written")
	if len(au) != 2 {
		t.Fatalf("one audit row per operator write, got %d", len(au))
	}
	first := au[0]
	if first["attestation_ref"] != "operator-ref-1" || first["reason_code"] != "s2_operator" || first["before"] != nil || first["_actor"] != f.compA.String() || first["_ip"] == "" {
		t.Errorf("audit row: %v", first)
	}
	if first["after"].(map[string]any)["attestation_ref"] != "operator-ref-1" {
		t.Errorf("audit after: %v", first["after"])
	}
	if au[1]["before"] == nil && au[1]["brand_id"] == nil {
		t.Errorf("second write audit: %v", au[1])
	}
}

func TestRGLimitsS2_Operator_RefusalsAreAudited(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	comp := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	post := func(cfg rgBody) rgResp {
		return w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": cfg})
	}
	loosen := map[string]rgBody{
		"enabled":           opDeposit(rgBody{"enabled": false}),
		"ceiling":           opDeposit(rgBody{"ceiling": rgBody{"value": "6000"}}),
		"increase_cooldown": opDeposit(rgBody{"increase_cooldown_seconds": 1}),
		"allowed_periods":   opDeposit(rgBody{"allowed_periods": []string{"rolling_24h"}}),
	}
	for field, cfg := range loosen {
		r := post(cfg)
		r.want(t, 409, "RG_OPERATOR_LOOSENS")
		if !strings.Contains(string(r.body), field) || strings.Contains(string(r.body), "parent") {
			t.Errorf("%s: the refusal names the field and nothing from the database: %s", field, r.body)
		}
	}
	// a ceiling of "none" where the floor has a numeric ceiling is also a loosening
	post(opDeposit(rgBody{"ceiling": rgBody{"none": true}})).want(t, 409, "RG_OPERATOR_LOOSENS")
	den := w.auditRows(f.tenantA, "rg.limit_config.operator_denied")
	if len(den) != 5 {
		t.Fatalf("every refusal is audited, got %d", len(den))
	}
	fields := map[string]bool{}
	for _, d := range den {
		if d["code"] != "operator_loosens" || d["_outcome"] != "denied" || d["_actor"] != f.compA.String() {
			t.Errorf("denial row: %v", d)
		}
		fields[fmt.Sprint(d["field"])] = true
	}
	for f := range loosen {
		if !fields[f] {
			t.Errorf("the denial of %s records the loosened field: %v", f, fields)
		}
	}
	// nothing was written
	var n int
	w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_configs WHERE tenant_id = $1`, f.tenantA).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("refused operator rows are not stored: %d", n)
	}
	// no floor in force for the kind: nothing to tighten
	post(rgBody{"control_kind": "wager", "asset_code": "EUR", "enabled": true, "mandatory": false, "ceiling": rgBody{"none": true}, "increase_cooldown_seconds": 10,
		"increase_requires_reconfirmation": true, "scope": "account", "allowed_periods": []string{"rolling_24h"}, "calendar_tz": "UTC", "week_start": 1,
		"count_bonus_funded": true, "reason_code": "r", "attestation_ref": "a"}).want(t, 409, "RG_NO_FLOOR_IN_FORCE")
	// shape: partial / extra / malformed
	post(opDeposit(rgBody{"ceiling": nil})).want(t, 400, "validation_error")
	post(opDeposit(rgBody{"pending_intent_ttl": nil})).want(t, 400, "validation_error")
	// F10: a configured TTL has no effect yet, so it is refused rather than presented as protective.
	post(opDeposit(rgBody{"pending_intent_ttl": rgBody{"value": 7200}})).want(t, 400, "validation_error")
	post(opDeposit(rgBody{"count_bonus_funded": true})).want(t, 400, "validation_error") // does not apply to a deposit
	post(opDeposit(rgBody{"increase_cooldown_seconds": 0})).want(t, 400, "validation_error")
	post(opDeposit(rgBody{"calendar_tz": "Not/AZone"})).want(t, 400, "validation_error")
	post(opDeposit(rgBody{"attestation_ref": "café"})).want(t, 400, "validation_error")
	post(opDeposit(rgBody{"ceiling": rgBody{"none": true, "value": "5"}})).want(t, 400, "validation_error")
	post(opDeposit(rgBody{"asset_code": "NOPE"})).want(t, 409, "RG_NO_FLOOR_IN_FORCE") // an unknown asset has no floor to tighten (the tighten-only trigger precedes the FK)
	for _, extra := range []string{"tenant_id", "jurisdiction_code", "created_by"} {
		c := opDeposit(nil)
		c[extra] = uuid.NewString()
		post(c).want(t, 400, "validation_error")
	}
	top := w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": opDeposit(nil), "tenant_id": f.tenantB.String()})
	top.want(t, 400, "validation_error")
	// a brand of another tenant (composite FK): refused, not stored
	w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"brand_id": f.brandB.String(), "config": opDeposit(nil)}).want(t, 400, "validation_error")
	// no backdating: an effective_from in the past is refused
	post(opDeposit(rgBody{"effective_from": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)})).want(t, 422, "RG_BACKDATED")
	// an attestation that merely RESEMBLES the synthetic marker is refused (only the exact fixture marker is ever accepted, and only in dev/test)
	post(opDeposit(rgBody{"attestation_ref": "Synthetic-but-not-quite"})).want(t, 422, "RG_SYNTHETIC_REFUSED")
	// a future effective_from is accepted and does not change the composition until then
	r := post(opDeposit(rgBody{"effective_from": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "ceiling": rgBody{"value": "100"}}))
	r.want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", w.player(f, f.acctA1), "ff1", setLimit("deposit", "EUR", "rolling_24h", "4000")).want(t, 201, "")
	// the foreign tenant's token cannot write rows for tenant A: the row is always the session tenant's
	rb := w.call("POST", "/v1/admin/rg/limit-configs", w.staff(f.tenantB, f.compB, auth.RoleCompliance), "", rgBody{"config": opDeposit(rgBody{"attestation_ref": "b-ref"})})
	rb.want(t, 201, "")
	var tenantOfRow uuid.UUID
	w.ownerTenant(f.tenantB, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT tenant_id FROM rg_limit_configs WHERE attestation_ref = 'b-ref'`).Scan(&tenantOfRow)
	})
	if tenantOfRow != f.tenantB {
		t.Fatalf("an operator row belongs to the session's tenant: %s", tenantOfRow)
	}
}

func TestRGLimitsS2_Operator_AuditFailClosed(t *testing.T) {
	var fail atomic.Bool
	lim := rg.NewLimits(rg.LimitsConfig{Audit: func(ctx ctxT, tx txT, e auditEntry) error {
		if fail.Load() {
			return fmt.Errorf("injected audit failure")
		}
		return audit.Record(ctx, tx, e)
	}})
	w := newRGW(t, rgWOpt{limits: lim})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	comp := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	fail.Store(true)
	w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": opDeposit(nil)}).want(t, 500, "internal_error")
	var n int
	w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_configs WHERE tenant_id = $1`, f.tenantA).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("the operator row must roll back with its audit record: %d", n)
	}
	fail.Store(false)
	w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": opDeposit(nil)}).want(t, 201, "")
}

// ---- floor four-eyes -------------------------------------------------------------------------------------

func floorConfig(effective time.Time, over rgBody) rgBody {
	c := rgBody{"control_kind": "wager", "asset_code": "EUR", "effective_from": effective.UTC().Format(time.RFC3339Nano), "enabled": true, "mandatory": false,
		"ceiling": rgBody{"value": "4000"}, "increase_cooldown_seconds": 2, "increase_requires_reconfirmation": false, "scope": "account",
		"allowed_periods": []string{"rolling_24h", "calendar_day"}, "calendar_tz": "UTC", "week_start": 1, "count_bonus_funded": false,
		"reason_code": "s2_floor", "attestation_ref": "floor-ref-1"}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

const floorPath = "/v1/admin/platform/rg/limit-config-change-requests"

func (w *rgW) requestFloor(tok, jur string, cfg rgBody) rgResp {
	return w.call("POST", floorPath, tok, "", rgBody{"jurisdiction_code": jur, "config": cfg})
}

func (w *rgW) floorRows(jur string) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithPlatformAdmin(ctxBackground(), w.ops.Requester, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_configs WHERE jurisdiction_code = $1 AND tenant_id IS NULL`, jur).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestRGLimitsS2_Floor_FourEyesFlow(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture() // no floor in this jurisdiction yet
	req := w.platform(w.ops.Requester)
	app1 := w.platform(w.ops.Approver1)
	app2 := w.platform(w.ops.Approver2)
	samePerson := w.platform(w.samePersonPlatform)

	eff := time.Now().Add(3 * time.Second)
	r := w.requestFloor(req, f.jur, floorConfig(eff, nil))
	r.want(t, 201, "")
	fr := r.obj(t)
	id := fr["id"].(string)
	if fr["status"] != "pending" || fr["requested_by"] != w.ops.Requester.String() || fr["payload_hash"] == "" || fr["decided_by"] != nil {
		t.Fatalf("request: %v", fr)
	}
	if w.floorRows(f.jur) != 0 {
		t.Fatal("nothing is in force until a different Person approves")
	}
	// the requester cannot approve; neither can another staff account of the requester's Person
	w.call("POST", floorPath+"/"+id+"/approve", req, "", nil).want(t, 409, "RG_SELF_APPROVAL")
	w.call("POST", floorPath+"/"+id+"/approve", samePerson, "", nil).want(t, 409, "RG_SELF_APPROVAL")
	if w.floorRows(f.jur) != 0 {
		t.Fatal("a refused approval applies nothing")
	}
	// the queue shows it; a tenant caller cannot reach the queue
	q := w.call("GET", floorPath+"?status=pending", app1, "", nil)
	q.want(t, 200, "")
	found := false
	for _, it := range q.obj(t)["items"].([]any) {
		if it.(map[string]any)["id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("the pending request is in the queue: %s", q.body)
	}
	// two different approvers race: exactly one wins, the other is refused as not pending; one floor row
	var wg sync.WaitGroup
	res := make([]rgResp, 2)
	for i, tk := range []string{app1, app2} {
		wg.Add(1)
		go func(i int, tk string) {
			defer wg.Done()
			res[i] = w.call("POST", floorPath+"/"+id+"/approve", tk, "", nil)
		}(i, tk)
	}
	wg.Wait()
	ok, notPending := 0, 0
	for _, x := range res {
		switch {
		case x.status == 200:
			ok++
			if x.obj(t)["status"] != "approved" || x.obj(t)["applied_config_id"] == nil || x.obj(t)["decided_by"] == nil {
				t.Fatalf("approval: %s", x.body)
			}
		case x.status == 409 && x.code(t) == "RG_REQUEST_NOT_PENDING":
			notPending++
		default:
			t.Fatalf("unexpected approval outcome %d %s", x.status, x.body)
		}
	}
	if ok != 1 || notPending != 1 {
		t.Fatalf("exactly one approval wins: ok=%d notPending=%d", ok, notPending)
	}
	if w.floorRows(f.jur) != 1 {
		t.Fatalf("the approved payload is applied exactly once, got %d floor rows", w.floorRows(f.jur))
	}
	// single use: a later decision is refused
	w.call("POST", floorPath+"/"+id+"/reject", app1, "", nil).want(t, 409, "RG_REQUEST_NOT_PENDING")
	w.call("POST", floorPath+"/"+id+"/cancel", req, "", nil).want(t, 409, "RG_REQUEST_NOT_PENDING")
	// once in force the floor governs players (after effective_from): ceiling 4000 for wagers
	sleepPast(eff)
	pa := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", pa, "fl1", setLimit("wager", "EUR", "rolling_24h", "4001")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	w.call("POST", "/v1/me/rg/limits", pa, "fl2", setLimit("wager", "EUR", "rolling_24h", "4000")).want(t, 201, "")
	// ... and is visible to tenants with the platform creator redacted
	ls := w.call("GET", "/v1/admin/rg/limit-configs?control_kind=wager", w.staff(f.tenantA, f.compA, auth.RoleCompliance), "", nil)
	ls.want(t, 200, "")
	if !strings.Contains(string(ls.body), "floor-ref-1") || strings.Contains(string(ls.body), w.ops.Requester.String()) || strings.Contains(string(ls.body), w.ops.Approver1.String()) || strings.Contains(string(ls.body), w.ops.Approver2.String()) {
		t.Fatalf("the floor is visible to tenants and no platform staff id is: %s", ls.body)
	}
	// audit (platform-scope rows): requested + approved with before / after, attestation and the hash
	ar := w.auditRows(uuid.Nil, "rg.limit_config.floor_requested")
	var reqRow map[string]any
	for _, a := range ar {
		if a["_target"] == id {
			reqRow = a
		}
	}
	if reqRow == nil || reqRow["attestation_ref"] != "floor-ref-1" || reqRow["payload_hash"] != fr["payload_hash"] || reqRow["_actor"] != w.ops.Requester.String() || reqRow["after"] == nil {
		t.Fatalf("floor_requested audit: %v", reqRow)
	}
	var apprRow map[string]any
	for _, a := range w.auditRows(uuid.Nil, "rg.limit_config.floor_approved") {
		if a["_target"] == id {
			apprRow = a
		}
	}
	if apprRow == nil || apprRow["attestation_ref"] != "floor-ref-1" || apprRow["applied_config_id"] == nil || apprRow["after"] == nil || apprRow["status_after"] != "approved" {
		t.Fatalf("floor_approved audit: %v", apprRow)
	}
	if n := w.auditRows(uuid.Nil, "rg.limit_config.floor_denied"); len(n) < 2 {
		t.Fatalf("the refused decisions are audited: %d", len(n))
	}
	// a tenant caller sees none of the platform audit rows (platform-scope rows are not tenant rows)
	for _, a := range w.auditRows(f.tenantA, "rg.limit_config.floor_requested") {
		t.Fatalf("platform-scope audit row leaked into a tenant: %v", a)
	}
}

func TestRGLimitsS2_Floor_RejectCancelAndValidation(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	req, app1 := w.platform(w.ops.Requester), w.platform(w.ops.Approver1)
	future := time.Now().Add(30 * time.Minute)

	// reject: the requester cannot, an approver can; nothing applied
	a := w.requestFloor(req, f.jur, floorConfig(future, nil))
	a.want(t, 201, "")
	aid := a.obj(t)["id"].(string)
	w.call("POST", floorPath+"/"+aid+"/reject", req, "", nil).want(t, 409, "RG_SELF_APPROVAL")
	rj := w.call("POST", floorPath+"/"+aid+"/reject", app1, "", nil)
	rj.want(t, 200, "")
	if rj.obj(t)["status"] != "rejected" || rj.obj(t)["applied_config_id"] != nil {
		t.Fatalf("reject: %s", rj.body)
	}
	if w.floorRows(f.jur) != 0 {
		t.Fatal("a rejected request applies nothing")
	}
	// cancel: only the requester
	b := w.requestFloor(req, f.jur, floorConfig(future, nil))
	b.want(t, 201, "")
	bid := b.obj(t)["id"].(string)
	w.call("POST", floorPath+"/"+bid+"/cancel", app1, "", nil).want(t, 403, "forbidden")
	cn := w.call("POST", floorPath+"/"+bid+"/cancel", req, "", nil)
	cn.want(t, 200, "")
	if cn.obj(t)["status"] != "cancelled" {
		t.Fatalf("cancel: %s", cn.body)
	}
	w.call("POST", floorPath+"/"+bid+"/approve", app1, "", nil).want(t, 409, "RG_REQUEST_NOT_PENDING")
	// unknown id
	w.call("POST", floorPath+"/"+uuid.NewString()+"/approve", app1, "", nil).want(t, 404, "not_found")
	w.call("POST", floorPath+"/not-a-uuid/approve", app1, "", nil).want(t, 404, "not_found")

	// request validation
	w.requestFloor(req, f.jur, floorConfig(future, rgBody{"effective_from": nil})).want(t, 400, "validation_error")
	w.requestFloor(req, f.jur, floorConfig(time.Now().Add(-time.Minute), nil)).want(t, 400, "validation_error")
	w.requestFloor(req, f.jur, floorConfig(time.Now().Add(73*time.Hour), nil)).want(t, 400, "validation_error")
	w.requestFloor(req, "NO-SUCH-JURISDICTION", floorConfig(future, nil)).want(t, 400, "validation_error")
	w.requestFloor(req, f.jur, floorConfig(future, rgBody{"ceiling": nil})).want(t, 400, "validation_error")
	w.requestFloor(req, f.jur, floorConfig(future, rgBody{"attestation_ref": ""})).want(t, 400, "validation_error")
	w.requestFloor(req, "", floorConfig(future, nil)).want(t, 400, "validation_error")
	// the body cannot name a tenant / brand: a floor has neither
	bad := floorConfig(future, nil)
	bad["tenant_id"] = f.tenantA.String()
	w.requestFloor(req, f.jur, bad).want(t, 400, "validation_error")
	// tenant-scoped callers (even with a platform_admin ROLE claim) cannot reach the platform routes
	w.call("POST", floorPath, w.tok(w.ops.Requester, f.tenantA, auth.RolePlatformAdmin, auth.PrincipalStaff), "", rgBody{"jurisdiction_code": f.jur, "config": floorConfig(future, nil)}).want(t, 403, "forbidden")
	w.call("GET", floorPath, w.staff(f.tenantA, f.compA, auth.RoleCompliance), "", nil).want(t, 403, "forbidden")
	// a platform token cannot use the tenant-scoped surface
	w.call("GET", "/v1/admin/rg/limit-configs", req, "", nil).want(t, 403, "forbidden")
	// the apply is refused once effective_from has passed (no backdating), and the request stays pending
	c := w.requestFloor(req, f.jur, floorConfig(time.Now().Add(1200*time.Millisecond), nil))
	c.want(t, 201, "")
	time.Sleep(7 * time.Second)
	w.call("POST", floorPath+"/"+c.obj(t)["id"].(string)+"/approve", app1, "", nil).want(t, 422, "RG_BACKDATED")
	st := w.call("GET", floorPath+"?status=pending&limit=200", app1, "", nil)
	st.want(t, 200, "")
	still := false
	for _, it := range st.obj(t)["items"].([]any) {
		if it.(map[string]any)["id"] == c.obj(t)["id"] {
			still = true
		}
	}
	if !still || w.floorRows(f.jur) != 0 {
		t.Fatalf("a refused apply rolls the whole approval back: pending=%v floors=%d", still, w.floorRows(f.jur))
	}
	w.call("GET", floorPath+"?status=bogus", app1, "", nil).want(t, 400, "validation_error")
}

// The signed actor proof is the database's gate: a proof signed with a key the database does not hold
// is refused, and nothing is stored.
func TestRGLimitsS2_Floor_ForgedProofIsRefused(t *testing.T) {
	forged, err := actorproof.NewIssuer("forged-kid-1", map[string][]byte{"forged-kid-1": []byte(strings.Repeat("f", 32))})
	if err != nil {
		t.Fatal(err)
	}
	w := newRGW(t, rgWOpt{limits: rg.NewLimits(rg.LimitsConfig{Issuer: forged})})
	f := w.fixture()
	r := w.requestFloor(w.platform(w.ops.Requester), f.jur, floorConfig(time.Now().Add(time.Hour), nil))
	r.want(t, 403, "RG_PROOF_REFUSED")
	var n int
	if err := w.owner.WithPlatformAdmin(ctxBackground(), w.ops.Requester, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_config_change_requests WHERE jurisdiction_code = $1`, f.jur).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a refused proof stores no request: %d", n)
	}
}

// Floor writes are audit fail-closed: a failing audit write rolls the request / the approval back.
func TestRGLimitsS2_Floor_AuditFailClosed(t *testing.T) {
	var fail atomic.Bool
	lim := rg.NewLimits(rg.LimitsConfig{Audit: func(ctx ctxT, tx txT, e auditEntry) error {
		if fail.Load() {
			return fmt.Errorf("injected audit failure")
		}
		return audit.Record(ctx, tx, e)
	}})
	w := newRGW(t, rgWOpt{limits: lim})
	f := w.fixture()
	req, app1 := w.platform(w.ops.Requester), w.platform(w.ops.Approver1)
	count := func() (reqs, floors int) {
		if err := w.owner.WithPlatformAdmin(ctxBackground(), w.ops.Requester, func(ctx ctxT, tx txT) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_config_change_requests WHERE jurisdiction_code = $1`, f.jur).Scan(&reqs); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_configs WHERE jurisdiction_code = $1`, f.jur).Scan(&floors)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	fail.Store(true)
	w.requestFloor(req, f.jur, floorConfig(time.Now().Add(time.Hour), nil)).want(t, 500, "internal_error")
	if rq, fl := count(); rq != 0 || fl != 0 {
		t.Fatalf("the request rolls back with its audit record: %d %d", rq, fl)
	}
	fail.Store(false)
	r := w.requestFloor(req, f.jur, floorConfig(time.Now().Add(time.Hour), nil))
	r.want(t, 201, "")
	fail.Store(true)
	w.call("POST", floorPath+"/"+r.obj(t)["id"].(string)+"/approve", app1, "", nil).want(t, 500, "internal_error")
	fail.Store(false)
	var status string
	if err := w.owner.WithPlatformAdmin(ctxBackground(), w.ops.Requester, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT status FROM rg_limit_config_change_requests WHERE id = $1`, r.obj(t)["id"]).Scan(&status)
	}); err != nil {
		t.Fatal(err)
	}
	if rq, fl := count(); rq != 1 || fl != 0 || status != "pending" {
		t.Fatalf("the approval and its apply roll back with the audit record: requests=%d floors=%d status=%s", rq, fl, status)
	}
	w.call("POST", floorPath+"/"+r.obj(t)["id"].(string)+"/approve", app1, "", nil).want(t, 200, "")
}

// ---- time-out ------------------------------------------------------------------------------------------------

func restrictionRow(t *testing.T, w *rgW, f rgFix, id string) (tenant *uuid.UUID, rtype string, ends time.Time) {
	t.Helper()
	w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT tenant_id, restriction_type, ends_at FROM player_restrictions WHERE id = $1`, id).Scan(&tenant, &rtype, &ends)
	})
	return
}

func TestRGLimitsS2_TimeOut_DefaultsReplayAndStatus(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture() // no time_out configuration: protective writes still succeed
	tok := w.player(f, f.acctA1)
	r := w.call("POST", "/v1/me/rg/time-out", tok, "to1", rgBody{"duration_hours": 24})
	r.want(t, 201, "")
	o := r.obj(t)
	if o["restriction_type"] != "time_out" || o["indefinite"] != false || o["active"] != true || o["source"] != "player_self_service" {
		t.Fatalf("time-out: %v", o)
	}
	tenant, rtype, ends := restrictionRow(t, w, f, o["id"].(string))
	if tenant != nil || rtype != "time_out" {
		t.Fatalf("an unconfigured time-out is platform_person (tenant NULL): %v %s", tenant, rtype)
	}
	if d := time.Until(ends); d < 23*time.Hour+59*time.Minute || d > 24*time.Hour+time.Minute {
		t.Fatalf("ends_at is now+24h: %v", d)
	}
	// replay: same id, 200 + header; different duration under the same key: 409; the other player is unaffected
	rep := w.call("POST", "/v1/me/rg/time-out", tok, "to1", rgBody{"duration_hours": 24})
	rep.want(t, 200, "")
	if rep.obj(t)["id"] != o["id"] || rep.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %s", rep.body)
	}
	w.call("POST", "/v1/me/rg/time-out", tok, "to1", rgBody{"duration_hours": 48}).want(t, 409, "RG_IDEMPOTENCY_MISMATCH")
	// concurrent identical requests: one row
	var wg sync.WaitGroup
	ids := make([]any, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			x := w.call("POST", "/v1/me/rg/time-out", w.player(f, f.acctX), "to-par", rgBody{"duration_hours": 6})
			if x.status != 201 && x.status != 200 {
				t.Errorf("parallel time-out: %d %s", x.status, x.body)
				return
			}
			ids[i] = x.obj(t)["id"]
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("parallel identical requests must produce one time-out: %v", ids)
		}
	}
	// it shows in the player's restriction status (existing endpoint) and the audit trail
	st := w.call("GET", "/v1/me/rg/status", tok, "", nil)
	st.want(t, 200, "")
	if !strings.Contains(string(st.body), `"restriction_type":"time_out"`) {
		t.Fatalf("status: %s", st.body)
	}
	au := w.auditRows(f.tenantA, "rg.time_out.created")
	if len(au) < 2 || au[0]["applied_scope"] != "platform_person" || au[0]["requested_scope"] != "platform_person" || au[0]["_actor"] != f.acctA1.String() {
		t.Fatalf("time-out audit: %v", au)
	}
	// shape
	for i, b := range []any{rgBody{"duration_hours": 0}, rgBody{"duration_hours": -1}, rgBody{"duration_hours": rg.MaxTimeOutHours + 1}, rgBody{}, rgBody{"duration_hours": 5, "tenant_id": f.tenantA.String()}, rgBody{"duration_hours": 1.5}} {
		w.call("POST", "/v1/me/rg/time-out", tok, fmt.Sprintf("to-bad-%d", i), b).want(t, 400, "validation_error")
	}
	w.call("POST", "/v1/me/rg/time-out", tok, "", rgBody{"duration_hours": 1}).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/time-out", tok, "to-max", rgBody{"duration_hours": rg.MaxTimeOutHours}).want(t, 201, "") // the bound itself is accepted
	w.call("POST", "/v1/me/rg/time-out", tok, "to-min", rgBody{"duration_hours": 1}).want(t, 201, "")
	// withdrawals / wallet are untouched by a time-out: the account status is unchanged
	var status string
	w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT status FROM player_accounts WHERE id = $1`, f.acctA1).Scan(&status)
	})
	if status != "active" {
		t.Fatalf("a time-out never changes the account status: %s", status)
	}
}

func TestRGLimitsS2_TimeOut_ConfiguredScopeAndDurations(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "time_out", rgfix.Spec{"scope": "account", "allowed_durations_any": false, "allowed_durations": []string{"24:00:00", "72:00:00"}})
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/time-out", tok, "c1", rgBody{"duration_hours": 48}).want(t, 422, "RG_DURATION_NOT_ALLOWED")
	r := w.call("POST", "/v1/me/rg/time-out", tok, "c2", rgBody{"duration_hours": 72})
	r.want(t, 201, "")
	tenant, _, _ := restrictionRow(t, w, f, r.obj(t)["id"].(string))
	if tenant == nil || *tenant != f.tenantA {
		t.Fatalf("a configured account scope is applied as tenant_person (the broader, more protective reach), tenant pinned from the session: %v", tenant)
	}
	au := w.auditRows(f.tenantA, "rg.time_out.created")
	if len(au) != 1 || au[0]["requested_scope"] != "account" || au[0]["applied_scope"] != "tenant_person" {
		t.Fatalf("the audit row names both scopes: %v", au)
	}
	if den := w.auditRows(f.tenantA, "rg.time_out.denied"); len(den) != 1 || den[0]["code"] != "duration_not_allowed" {
		t.Fatalf("refusal audit: %v", den)
	}
	// a tenant_person configuration pins the tenant too; a platform_person one leaves it NULL
	f2 := w.fixture()
	w.floor(f2.jur, "time_out", rgfix.Spec{"scope": "tenant_person"})
	r2 := w.call("POST", "/v1/me/rg/time-out", w.player(f2, f2.acctA1), "c3", rgBody{"duration_hours": 5})
	r2.want(t, 201, "")
	if tn, _, _ := restrictionRow(t, w, f2, r2.obj(t)["id"].(string)); tn == nil || *tn != f2.tenantA {
		t.Fatalf("tenant_person scope: %v", tn)
	}
	// enabled=false / unconfigured never blocks a time-out (protective)
	f3 := w.fixture()
	w.floor(f3.jur, "time_out", rgfix.Spec{"enabled": false})
	w.call("POST", "/v1/me/rg/time-out", w.player(f3, f3.acctA1), "c4", rgBody{"duration_hours": 5}).want(t, 201, "")
}

// pgx import guard for helpers shared above.
var _ = pgx.ErrNoRows

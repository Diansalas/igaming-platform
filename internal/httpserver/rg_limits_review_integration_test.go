//go:build integration

package httpserver

// ADR 0115 S2 review follow-ups (security F1-F4, ledger-finance C1-C4 and the "other" items), through
// the real HTTP stack as the runtime role.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

// F1: a confirm loosens the limit. An account under identity review cannot confirm a person-level
// increase, and only the account that made the request can confirm it.
func TestRGLimitsS2_Review_ConfirmGates(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "wager", rgfix.Spec{"scope": "tenant_person", "increase_requires_reconfirmation": true})
	setStatus := func(acct uuid.UUID, status string) {
		w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
			_, err := tx.Exec(ctx, `UPDATE player_accounts SET status = $2 WHERE id = $1`, acct, status)
			return err
		})
	}
	// (a) identity review
	x := w.player(f, f.acctX)
	w.call("POST", "/v1/me/rg/limits", x, "r1", setLimit("wager", "EUR", "rolling_24h", "100")).want(t, 201, "")
	inc := w.call("POST", "/v1/me/rg/limits", x, "r2", setLimit("wager", "EUR", "rolling_24h", "300"))
	inc.want(t, 202, "")
	sleepPast(parseTime(inc.obj(t)["effective_at"]))
	setStatus(f.acctX, "identity_review_required")
	id := inc.obj(t)["id"].(string)
	w.call("POST", "/v1/me/rg/limits/changes/"+id+"/confirm", x, "r3", nil).want(t, 403, "RG_IDENTITY_REVIEW")
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", x, "", nil), "wager"); v != "100" {
		t.Fatalf("a refused confirm realizes nothing: %q", v)
	}
	setStatus(f.acctX, "active")
	w.call("POST", "/v1/me/rg/limits/changes/"+id+"/confirm", x, "r4", nil).want(t, 200, "")

	// (b) only the requesting account confirms (acctA1 and acctA2 are the same Person)
	a1, a2 := w.player(f, f.acctA1), w.player(f, f.acctA2)
	w.call("POST", "/v1/me/rg/limits", a1, "q1", setLimit("wager", "EUR", "rolling_24h", "100")).want(t, 201, "")
	up := w.call("POST", "/v1/me/rg/limits", a1, "q2", setLimit("wager", "EUR", "rolling_24h", "400"))
	up.want(t, 202, "")
	sleepPast(parseTime(up.obj(t)["effective_at"]))
	uid := up.obj(t)["id"].(string)
	w.call("POST", "/v1/me/rg/limits/changes/"+uid+"/confirm", a2, "q3", nil).want(t, 403, "forbidden")
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", a1, "", nil), "wager"); v != "100" {
		t.Fatalf("a refused confirm realizes nothing: %q", v)
	}
	w.call("POST", "/v1/me/rg/limits/changes/"+uid+"/confirm", a1, "q4", nil).want(t, 200, "")
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", a2, "", nil), "wager"); v != "400" {
		t.Fatalf("the requester's confirm realizes the person-level limit: %q", v)
	}
	// cancelling is protective: the Person's other account may cancel a pending change
	again := w.call("POST", "/v1/me/rg/limits", a1, "q5", setLimit("wager", "EUR", "rolling_24h", "500"))
	again.want(t, 202, "")
	w.call("POST", "/v1/me/rg/limits/changes/"+again.obj(t)["id"].(string)+"/cancel", a2, "q6", nil).want(t, 200, "")
}

// F3: a staff replay is bound to the staff member; a different staff member with the same key is 409.
func TestRGLimitsS2_Review_StaffReplayBoundToStaff(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	other := uuid.New()
	w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
		person := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, $2, $3, '!x', 'compliance', $4)`,
			other, f.tenantA, "rgs2-"+other.String()+"@fixture.invalid", person)
		return err
	})
	path := "/v1/admin/players/" + f.acctA1.String() + "/rg/limits"
	c1, c2 := w.staff(f.tenantA, f.compA, auth.RoleCompliance), w.staff(f.tenantA, other, auth.RoleCompliance)
	w.call("POST", path, c1, "same", staffSet("deposit", "EUR", "rolling_24h", "400")).want(t, 201, "")
	w.call("POST", path, c1, "same", staffSet("deposit", "EUR", "rolling_24h", "400")).want(t, 201, "") // own replay
	w.call("POST", path, c2, "same", staffSet("deposit", "EUR", "rolling_24h", "400")).want(t, 409, "RG_IDEMPOTENCY_MISMATCH")
	if n := w.changeCount(f.tenantA, f.acctA1); n != 1 {
		t.Fatalf("one row: %d", n)
	}
	// F2: a player key that LOOKS like the staff namespace is rejected, and cannot replay a staff row
	player := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", player, "staff same", setLimit("deposit", "EUR", "rolling_24h", "400")).want(t, 400, "validation_error")
	// ...and a player key spelled like the old colon namespace does not collide with the staff row "same"
	w.call("POST", "/v1/me/rg/limits", player, "staff:same", setLimit("deposit", "EUR", "rolling_24h", "300")).want(t, 201, "")
}

// F4: free text is printable ASCII or 400 - never a database 500.
func TestRGLimitsS2_Review_FreeTextIs400NotFiveHundred(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	comp := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	path := "/v1/admin/players/" + f.acctX.String() + "/rg/limits"
	body := func(reason, evidence string) string {
		return `{"kind":"deposit","asset_code":"EUR","period":"rolling_24h","value":"100","reason_code":"` + reason + `","evidence_ref":"` + evidence + `"}`
	}
	for i, b := range []string{body(`a\u0000b`, "e"), body("r", `e\u0000`), body(`café`, "e"), body("r", `ticket\n9`)} {
		w.call("POST", path, comp, "fx"+string(rune('a'+i)), b).want(t, 400, "validation_error")
	}
	c := opDeposit(rgBody{"reason_code": "a\x00b"})
	w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": c}).want(t, 400, "validation_error")
	c = opDeposit(rgBody{"attestation_ref": "a\x00b"})
	w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": c}).want(t, 400, "validation_error")
	req := w.platform(w.ops.Requester)
	w.requestFloor(req, f.jur, floorConfig(time.Now().Add(time.Hour), rgBody{"reason_code": "r\x00"})).want(t, 400, "validation_error")
	w.requestFloor(req, "J\x00", floorConfig(time.Now().Add(time.Hour), nil)).want(t, 400, "validation_error")
	if n := w.changeCount(f.tenantA, f.acctX); n != 0 {
		t.Fatalf("nothing stored: %d", n)
	}
	// SR-2: a NUL (which jsonb cannot hold) must not suppress the denial audit: every refusal above left a row
	if n := len(w.auditRows(f.tenantA, "rg.limit.change_denied")); n != 4 {
		t.Fatalf("staff NUL / non-ASCII refusals each leave a denial row, got %d", n)
	}
	if n := len(w.auditRows(f.tenantA, "rg.limit_config.operator_denied")); n != 2 {
		t.Fatalf("operator NUL refusals each leave a denial row, got %d", n)
	}
	if n := len(w.auditRows(uuid.Nil, "rg.limit_config.floor_denied")); n < 2 {
		t.Fatalf("floor NUL refusals each leave a denial row, got %d", n)
	}
	// SR-3: whitespace-only free text is 400
	for i, b := range []string{body("   ", "e"), body("r", "  ")} {
		w.call("POST", path, comp, "ws"+string(rune('a'+i)), b).want(t, 400, "validation_error")
	}
	w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": opDeposit(rgBody{"reason_code": "   "})}).want(t, 400, "validation_error")
}

// SR-1: caller-supplied fields are bounded and cleaned before they reach the append-only audit row: a
// 900 KB value is a 400 AND a small denial row.
func TestRGLimitsS2_Review_OversizedFieldsMakeSmallDenialRows(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	huge := strings.Repeat("A", 900*1024)
	comp := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	player := w.player(f, f.acctA1)
	req := w.platform(w.ops.Requester)
	maxRow := func(tenant uuid.UUID, action string, want int) {
		t.Helper()
		rows := w.auditRows(tenant, action)
		if len(rows) < want {
			t.Fatalf("%s: want >= %d denial rows, got %d", action, want, len(rows))
		}
		for _, r := range rows {
			b, _ := json.Marshal(r)
			if len(b) > 2048 {
				t.Fatalf("%s: a denial row is %d bytes: %.200s", action, len(b), b)
			}
		}
	}
	lim := func(kind, asset, period string) rgBody {
		return rgBody{"kind": kind, "asset_code": asset, "period": period, "value": "1"}
	}
	w.call("POST", "/v1/me/rg/limits", player, "o1", lim(huge, "EUR", "rolling_24h")).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/limits", player, "o2", lim("deposit", huge, "rolling_24h")).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/limits", player, "o3", lim("deposit", "EUR", huge)).want(t, 400, "validation_error")
	maxRow(f.tenantA, "rg.limit.change_denied", 3)
	sp := "/v1/admin/players/" + f.acctX.String() + "/rg/limits"
	sb := staffSet("deposit", "EUR", "rolling_24h", "1")
	sb["reason_code"] = huge
	w.call("POST", sp, comp, "o4", sb).want(t, 400, "validation_error")
	maxRow(f.tenantA, "rg.limit.change_denied", 4)
	for _, over := range []rgBody{{"reason_code": huge}, {"attestation_ref": huge}, {"asset_code": huge}, {"control_kind": huge}} {
		w.call("POST", "/v1/admin/rg/limit-configs", comp, "", rgBody{"config": opDeposit(over)}).want(t, 400, "validation_error")
	}
	maxRow(f.tenantA, "rg.limit_config.operator_denied", 4)
	for _, over := range []rgBody{{"reason_code": huge}, {"attestation_ref": huge}, {"control_kind": huge}} {
		w.requestFloor(req, f.jur, floorConfig(time.Now().Add(time.Hour), over)).want(t, 400, "validation_error")
	}
	w.requestFloor(req, huge, floorConfig(time.Now().Add(time.Hour), nil)).want(t, 400, "validation_error")
	maxRow(uuid.Nil, "rg.limit_config.floor_denied", 4)
}

// LF C1: a decrease is a protective write and succeeds above a since-lowered ceiling or on a since-dropped period.
func TestRGLimitsS2_Review_DecreaseSucceedsAboveLoweredCeilingAndDroppedPeriod(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil) // ceiling 5000, periods rolling_24h + calendar_day
	player := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", player, "c1", setLimit("deposit", "EUR", "rolling_24h", "5000")).want(t, 201, "")
	// the operator lowers the ceiling to 3000
	w.call("POST", "/v1/admin/rg/limit-configs", w.staff(f.tenantA, f.compA, auth.RoleCompliance), "", rgBody{"config": opDeposit(nil)}).want(t, 201, "")
	d := w.call("POST", "/v1/me/rg/limits", player, "c2", setLimit("deposit", "EUR", "rolling_24h", "4000"))
	d.want(t, 201, "")
	if d.obj(t)["change_kind"] != "decrease" {
		t.Fatalf("a decrease above the lowered ceiling is accepted: %v", d.obj(t))
	}
	// loosening and new limits stay bound by the lowered ceiling
	w.call("POST", "/v1/me/rg/limits", player, "c3", setLimit("deposit", "EUR", "rolling_24h", "4500")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	w.call("POST", "/v1/me/rg/limits", player, "c4", setLimit("deposit", "EUR", "calendar_day", "3500")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	w.call("POST", "/v1/me/rg/limits", player, "c5", setLimit("deposit", "EUR", "rolling_24h", "3000")).want(t, 201, "")

	// a dropped period: the floor is replaced by one without calendar_day
	g := w.fixture()
	w.floor(g.jur, "deposit", nil)
	pg := w.player(g, g.acctA1)
	w.call("POST", "/v1/me/rg/limits", pg, "p1", setLimit("deposit", "EUR", "calendar_day", "500")).want(t, 201, "")
	w.floor(g.jur, "deposit", rgfix.Spec{"allowed_periods": []string{"rolling_24h"}})
	w.call("POST", "/v1/me/rg/limits", pg, "p2", setLimit("deposit", "EUR", "calendar_day", "400")).want(t, 201, "") // decrease: allowed
	w.call("POST", "/v1/me/rg/limits", pg, "p3", setLimit("deposit", "EUR", "calendar_day", "450")).want(t, 422, "RG_PERIOD_NOT_ALLOWED")
	w.call("POST", "/v1/me/rg/limits", pg, "p4", setLimit("deposit", "EUR", "calendar_day", nil)).want(t, 422, "RG_PERIOD_NOT_ALLOWED")
	w.call("POST", "/v1/me/rg/limits", w.player(g, g.acctX), "p5", setLimit("deposit", "EUR", "calendar_day", "10")).want(t, 422, "RG_PERIOD_NOT_ALLOWED") // initial on the dropped period
}

// LF C4: a denial audit row carries the attempted value and the config versions in force.
func TestRGLimitsS2_Review_DenialAuditCarriesValueAndConfigVersions(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	player := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", player, "d1", setLimit("deposit", "EUR", "rolling_24h", "123456")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	rows := w.auditRows(f.tenantA, "rg.limit.change_denied")
	if len(rows) != 1 || rows[0]["value"] != "123456" || rows[0]["code"] != "above_ceiling" {
		t.Fatalf("denial: %v", rows)
	}
	if ids, _ := rows[0]["config_version_ids"].([]any); len(ids) == 0 {
		t.Fatalf("the config versions in force are recorded: %v", rows[0])
	}
}

// LF C2: a 38-digit ceiling and a >2^53 interval round-trip through request, approval, apply and audit exactly.
func TestRGLimitsS2_Review_FloorLargeValuesRoundTrip(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	nines := strings.Repeat("9", 38)
	r := w.requestFloor(w.platform(w.ops.Requester), f.jur, floorConfig(time.Now().Add(4*time.Second), rgBody{"ceiling": rgBody{"value": nines}}))
	r.want(t, 201, "")
	id := r.obj(t)["id"].(string)
	w.call("POST", floorPath+"/"+id+"/approve", w.platform(w.ops.Approver1), "", nil).want(t, 200, "")
	var stored string
	if err := w.owner.WithPlatformAdmin(ctxBackground(), w.ops.Requester, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT ceiling::text FROM rg_limit_configs WHERE jurisdiction_code = $1`, f.jur).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	if stored != nines {
		t.Fatalf("the stored ceiling is exact: %s", stored)
	}
	var after map[string]any
	for _, a := range w.auditRows(uuid.Nil, "rg.limit_config.floor_approved") {
		if a["_target"] == id {
			after, _ = a["after"].(map[string]any)
		}
	}
	if after == nil || after["ceiling"].(map[string]any)["value"] != nines {
		t.Fatalf("the audit record keeps the exact value: %v", after)
	}
	// a player at that ceiling: a 38-digit limit is accepted (no float64 on the request path)
	sleepPast(time.Now().Add(4 * time.Second))
	w.call("POST", "/v1/me/rg/limits", w.player(f, f.acctA1), "big1", setLimit("wager", "EUR", "rolling_24h", nines)).want(t, 201, "")
	got := w.call("GET", "/v1/me/rg/limits", w.player(f, f.acctA1), "", nil)
	if v, _ := limitValue(t, got, "wager"); v != nines {
		t.Fatalf("the 38-digit value round-trips: %q", v)
	}
}

// I22: the isolation guard refuses a REPEATABLE READ caller for limit changes and time-outs.
func TestRGLimitsS2_Review_RepeatableReadCallerIsRefused(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	svc := rg.NewLimits(rg.LimitsConfig{})
	run := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		ctx := context.Background()
		tx, err := w.rt.Raw().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for k, v := range map[string]string{"app.tenant_id": f.tenantA.String(), "app.player_account_id": f.acctA1.String()} {
			if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, k, v); err != nil {
				t.Fatal(err)
			}
		}
		return fn(ctx, tx)
	}
	err := run(func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.CreateTimeOut(ctx, tx, rg.TimeOutInput{TenantID: f.tenantA, PlayerAccountID: f.acctA1, DurationHours: 1, IdempotencyKey: "rr1"})
		return err
	})
	if rf, ok := rg.IsRefusal(err); !ok || rf.Code != rg.RefusedUnavailable {
		t.Fatalf("time-out under REPEATABLE READ must be refused: %v", err)
	}
	v := "100"
	p := rg.PeriodRolling24h
	err = run(func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.SetLimit(ctx, tx, rg.SetLimitInput{TenantID: f.tenantA, PlayerAccountID: f.acctA1,
			Limit: rg.LimitInput{Kind: rg.ControlDeposit, AssetCode: strPtr("EUR"), Period: &p, Value: &v, IdempotencyKey: "rr2"}})
		return err
	})
	if rf, ok := rg.IsRefusal(err); !ok || rf.Code != rg.RefusedUnavailable {
		t.Fatalf("a limit change under REPEATABLE READ must be refused: %v", err)
	}
	if n := w.changeCount(f.tenantA, f.acctA1); n != 0 {
		t.Fatalf("nothing stored: %d", n)
	}
}

func strPtr(s string) *string { return &s }

// I23: when the realization instant moves between preview and insert, the change is re-classified under
// the same person lock (the stored row is the trigger's classification).
func TestRGLimitsS2_Review_ReclassifiesWhenTheRealizationInstantMoves(t *testing.T) {
	var armed atomic.Bool
	var mu sync.Mutex
	var attempts []int
	lim := rg.NewLimits(rg.LimitsConfig{TestHookBeforeInsert: func(attempt int) {
		if armed.Load() {
			mu.Lock()
			attempts = append(attempts, attempt)
			mu.Unlock()
			if attempt == 0 {
				time.Sleep(1500 * time.Millisecond) // the earlier pending increase becomes effective meanwhile
			}
		}
	}})
	w := newRGW(t, rgWOpt{limits: lim})
	f := w.fixture()
	w.floor(f.jur, "deposit", rgfix.Spec{"increase_cooldown": "00:00:01"})
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "h1", setLimit("deposit", "EUR", "rolling_24h", "100")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", tok, "h2", setLimit("deposit", "EUR", "rolling_24h", "300")).want(t, 202, "")
	armed.Store(true)
	// previewed as an increase over 100, inserted as a decrease from the now-effective 300
	r := w.call("POST", "/v1/me/rg/limits", tok, "h3", setLimit("deposit", "EUR", "rolling_24h", "200"))
	r.want(t, 201, "")
	if r.obj(t)["change_kind"] != "decrease" {
		t.Fatalf("re-classified: %v", r.obj(t))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0] != 0 || attempts[1] != 1 {
		t.Fatalf("exactly one re-classification: %v", attempts)
	}
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", tok, "", nil), "deposit"); v != "200" {
		t.Fatalf("value: %q", v)
	}
}

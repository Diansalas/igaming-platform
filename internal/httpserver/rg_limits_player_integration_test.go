//go:build integration

package httpserver

// ADR 0115 slice S2: the player limit APIs through the real HTTP stack (runtime role, RLS, triggers).

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

func setLimit(kind, asset, period string, value any) rgBody {
	b := rgBody{"kind": kind, "value": value}
	if asset != "" {
		b["asset_code"] = asset
	}
	if period != "" {
		b["period"] = period
	}
	return b
}

func limitValue(t *testing.T, r rgResp, kind string) (string, bool) {
	t.Helper()
	r.want(t, 200, "")
	for _, l := range r.obj(t)["limits"].([]any) {
		m := l.(map[string]any)
		if m["kind"] == kind {
			if m["value"] == nil {
				return "", m["has_value"].(bool)
			}
			return m["value"].(string), m["has_value"].(bool)
		}
	}
	return "", false
}

// tighten-only / cooldown semantics through the API: an initial value and a decrease apply now, an
// increase is pending (202) and is NOT effective before requested_at + cooldown, then it is.
func TestRGLimitsS2_Player_InitialDecreaseIncreaseCooldown(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	tok := w.player(f, f.acctA1)

	r := w.call("POST", "/v1/me/rg/limits", tok, "k1", setLimit("deposit", "EUR", "rolling_24h", "1000"))
	r.want(t, 201, "")
	if o := r.obj(t); o["change_kind"] != "initial" || o["value"] != "1000" || o["pending"] != false || o["scope"] != "account" {
		t.Fatalf("initial: %v", o)
	}
	// a decrease applies now
	r = w.call("POST", "/v1/me/rg/limits", tok, "k2", setLimit("deposit", "EUR", "rolling_24h", 600)) // an integer JSON number is accepted
	r.want(t, 201, "")
	if o := r.obj(t); o["change_kind"] != "decrease" || o["value"] != "600" {
		t.Fatalf("decrease: %v", o)
	}
	got := w.call("GET", "/v1/me/rg/limits", tok, "", nil)
	if v, has := limitValue(t, got, "deposit"); v != "600" || !has {
		t.Fatalf("after the decrease the realized value is 600, got %q (has=%v) %s", v, has, got.body)
	}
	// an increase is pending: 202, effective in the future, NOT effective now
	r = w.call("POST", "/v1/me/rg/limits", tok, "k3", setLimit("deposit", "EUR", "rolling_24h", "900"))
	r.want(t, 202, "")
	inc := r.obj(t)
	if inc["change_kind"] != "increase" || inc["pending"] != true {
		t.Fatalf("increase: %v", inc)
	}
	eff := parseTime(inc["effective_at"])
	if d := eff.Sub(parseTime(inc["requested_at"])); d < 2*time.Second || d > 3*time.Second {
		t.Fatalf("effective_at - requested_at = %v, want the 2s cooldown", d)
	}
	got = w.call("GET", "/v1/me/rg/limits", tok, "", nil)
	if v, _ := limitValue(t, got, "deposit"); v != "600" {
		t.Fatalf("the increase must not be effective before the cooldown: %q", v)
	}
	var pend map[string]any
	for _, l := range got.obj(t)["limits"].([]any) {
		if p := l.(map[string]any)["pending"]; p != nil {
			pend = p.(map[string]any)
		}
	}
	if pend == nil || pend["id"] != inc["id"] {
		t.Fatalf("GET must show the open pending change: %v", got.body)
	}
	sleepPast(eff)
	got = w.call("GET", "/v1/me/rg/limits", tok, "", nil)
	if v, _ := limitValue(t, got, "deposit"); v != "900" {
		t.Fatalf("after the cooldown (no reconfirmation configured) the increase is realized: %q %s", v, got.body)
	}
}

func TestRGLimitsS2_Player_CancelPendingAndDecreaseSupersedes(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", rgfix.Spec{"increase_cooldown": "00:00:30"})
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "a1", setLimit("deposit", "EUR", "calendar_day", "500")).want(t, 201, "")
	inc := w.call("POST", "/v1/me/rg/limits", tok, "a2", setLimit("deposit", "EUR", "calendar_day", "800"))
	inc.want(t, 202, "")
	id := inc.obj(t)["id"].(string)
	// cancel
	c := w.call("POST", "/v1/me/rg/limits/changes/"+id+"/cancel", tok, "a3", nil)
	c.want(t, 200, "")
	if o := c.obj(t); o["change_kind"] != "cancel_pending" || o["value"] != "500" || o["supersedes_change_id"] != id {
		t.Fatalf("cancel: %v", o)
	}
	// the limit stays at 500; cancelling again is not pending
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", tok, "", nil), "deposit"); v != "500" {
		t.Fatalf("after cancel the value stays 500: %q", v)
	}
	w.call("POST", "/v1/me/rg/limits/changes/"+id+"/cancel", tok, "a4", nil).want(t, 409, "RG_CHANGE_NOT_PENDING")
	// a new increase, then a decrease: the decrease is immediate and the older pending increase can no longer be cancelled/confirmed
	inc2 := w.call("POST", "/v1/me/rg/limits", tok, "a5", setLimit("deposit", "EUR", "calendar_day", "900"))
	inc2.want(t, 202, "")
	w.call("POST", "/v1/me/rg/limits", tok, "a6", setLimit("deposit", "EUR", "calendar_day", "400")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits/changes/"+inc2.obj(t)["id"].(string)+"/cancel", tok, "a7", nil).want(t, 409, "RG_CHANGE_NOT_PENDING")
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", tok, "", nil), "deposit"); v != "400" {
		t.Fatalf("the decrease is immediate: %q", v)
	}
}

func TestRGLimitsS2_Player_ReconfirmationAndConfirm(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "wager", rgfix.Spec{"increase_requires_reconfirmation": true})
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "c1", setLimit("wager", "EUR", "rolling_24h", "100")).want(t, 201, "")
	inc := w.call("POST", "/v1/me/rg/limits", tok, "c2", setLimit("wager", "EUR", "rolling_24h", "300"))
	inc.want(t, 202, "")
	o := inc.obj(t)
	if o["requires_confirmation"] != true {
		t.Fatalf("reconfirmation configured: %v", o)
	}
	id := o["id"].(string)
	// confirm inside the cooling-off period is refused
	w.call("POST", "/v1/me/rg/limits/changes/"+id+"/confirm", tok, "c3", nil).want(t, 409, "RG_COOLING_OFF")
	eff := parseTime(o["effective_at"])
	sleepPast(eff)
	// without a confirm the increase is NOT realized, even after the cooldown
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", tok, "", nil), "wager"); v != "100" {
		t.Fatalf("an increase awaiting confirmation is not realized: %q", v)
	}
	cf := w.call("POST", "/v1/me/rg/limits/changes/"+id+"/confirm", tok, "c4", nil)
	cf.want(t, 200, "")
	if cf.obj(t)["change_kind"] != "confirm" || cf.obj(t)["value"] != "300" {
		t.Fatalf("confirm: %s", cf.body)
	}
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", tok, "", nil), "wager"); v != "300" {
		t.Fatalf("after confirm the value is 300: %q", v)
	}
	// a second confirm is not pending any more
	w.call("POST", "/v1/me/rg/limits/changes/"+id+"/confirm", tok, "c5", nil).want(t, 409, "RG_CHANGE_NOT_PENDING")
	// removal: the value is null, pending, needs the cooldown and a confirm
	rm := w.call("POST", "/v1/me/rg/limits", tok, "c6", setLimit("wager", "EUR", "rolling_24h", nil))
	rm.want(t, 202, "")
	if o := rm.obj(t); o["change_kind"] != "removal" || o["value"] != nil || o["requires_confirmation"] != true {
		t.Fatalf("removal: %v", o)
	}
}

// ADR 0115 S2 duties: the API (not the database) refuses a value above the ceiling and a period
// outside allowed_periods, and an increase/removal of a kind the configuration does not offer.
func TestRGLimitsS2_Player_CeilingPeriodAndOfferRules(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil) // ceiling 5000 EUR; periods rolling_24h, calendar_day
	w.floor(f.jur, "session_duration", nil)
	w.floor(f.jur, "loss", rgfix.Spec{"enabled": false})
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "d1", setLimit("deposit", "EUR", "rolling_24h", "5001")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	w.call("POST", "/v1/me/rg/limits", tok, "d2", setLimit("deposit", "EUR", "rolling_24h", "5000")).want(t, 201, "") // the ceiling itself is allowed
	w.call("POST", "/v1/me/rg/limits", tok, "d3", setLimit("deposit", "EUR", "rolling_7d", "100")).want(t, 422, "RG_PERIOD_NOT_ALLOWED")
	w.call("POST", "/v1/me/rg/limits", tok, "d4", setLimit("deposit", "EUR", "calendar_week", "100")).want(t, 422, "RG_PERIOD_NOT_ALLOWED")
	// a 39-digit value is a shape error, not a ceiling error
	w.call("POST", "/v1/me/rg/limits", tok, "d5", setLimit("deposit", "EUR", "rolling_24h", "999999999999999999999999999999999999999")).want(t, 400, "validation_error")
	// session_duration: its own ceiling (36000) applies
	w.call("POST", "/v1/me/rg/limits", tok, "s1", setLimit("session_duration", "", "", "36001")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	w.call("POST", "/v1/me/rg/limits", tok, "s2", setLimit("session_duration", "", "", "3600")).want(t, 201, "")
	// a kind the configuration does not enable: protective writes (initial / decrease) succeed, an increase is not offered
	w.call("POST", "/v1/me/rg/limits", tok, "l1", setLimit("loss", "EUR", "rolling_24h", "100")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", tok, "l2", setLimit("loss", "EUR", "rolling_24h", "50")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", tok, "l3", setLimit("loss", "EUR", "rolling_24h", "80")).want(t, 422, "RG_NOT_OFFERED")
	w.call("POST", "/v1/me/rg/limits", tok, "l4", setLimit("loss", "EUR", "rolling_24h", nil)).want(t, 422, "RG_NOT_OFFERED")
	// the value equal to the limit in force changes nothing
	w.call("POST", "/v1/me/rg/limits", tok, "l5", setLimit("loss", "EUR", "rolling_24h", "50")).want(t, 409, "RG_NO_CHANGE")
	// the refused requests wrote nothing and were audited as denials
	if n := w.changeCount(f.tenantA, f.acctA1); n != 4 { // d2, s2, l1, l2 are the only accepted writes
		t.Fatalf("only the accepted writes are stored, got %d rows", n)
	}
	if rows := w.auditRows(f.tenantA, "rg.limit.change_denied"); len(rows) < 8 {
		t.Fatalf("every refusal is audited, got %d denial rows", len(rows))
	}
}

// With no configuration in force: protective writes succeed, an increase / removal is refused.
func TestRGLimitsS2_Player_NoConfigurationFailsClosedForLoosening(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture() // no floor at all
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "n1", setLimit("wager", "EUR", "rolling_24h", "100")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", tok, "n2", setLimit("wager", "EUR", "rolling_24h", "50")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", tok, "n3", setLimit("wager", "EUR", "rolling_24h", "70")).want(t, 409, "RG_CONFIG_MISSING")
	w.call("POST", "/v1/me/rg/limits", tok, "n4", setLimit("wager", "EUR", "rolling_24h", nil)).want(t, 409, "RG_CONFIG_MISSING")
}

// Idempotency-Key: replay returns the stored row; a different body is 409; the key is required.
func TestRGLimitsS2_Player_IdempotencyReplayAndMismatch(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	tok := w.player(f, f.acctA1)
	first := w.call("POST", "/v1/me/rg/limits", tok, "idem-1", setLimit("deposit", "EUR", "rolling_24h", "700"))
	first.want(t, 201, "")
	replay := w.call("POST", "/v1/me/rg/limits", tok, "idem-1", setLimit("deposit", "EUR", "rolling_24h", "700"))
	replay.want(t, 201, "")
	if replay.header.Get("Idempotent-Replayed") != "true" || first.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("replay header: first=%q replay=%q", first.header.Get("Idempotent-Replayed"), replay.header.Get("Idempotent-Replayed"))
	}
	if replay.obj(t)["id"] != first.obj(t)["id"] || replay.obj(t)["requested_at"] != first.obj(t)["requested_at"] {
		t.Fatal("a replay returns the stored row")
	}
	if n := w.changeCount(f.tenantA, f.acctA1); n != 1 {
		t.Fatalf("a replay writes nothing, got %d rows", n)
	}
	// the same key with a different body (value / period / kind) is 409
	for i, b := range []rgBody{
		setLimit("deposit", "EUR", "rolling_24h", "701"),
		setLimit("deposit", "EUR", "calendar_day", "700"),
		setLimit("deposit", "USD", "rolling_24h", "700"),
		setLimit("session_duration", "", "", "700"),
		setLimit("deposit", "EUR", "rolling_24h", nil),
	} {
		w.call("POST", "/v1/me/rg/limits", tok, "idem-1", b).want(t, 409, "RG_IDEMPOTENCY_MISMATCH")
		_ = i
	}
	// an increase replayed after its cooldown is still the same pending row (202), not a second change
	inc := w.call("POST", "/v1/me/rg/limits", tok, "idem-2", setLimit("deposit", "EUR", "rolling_24h", "900"))
	inc.want(t, 202, "")
	again := w.call("POST", "/v1/me/rg/limits", tok, "idem-2", setLimit("deposit", "EUR", "rolling_24h", "900"))
	again.want(t, 202, "")
	if again.obj(t)["id"] != inc.obj(t)["id"] {
		t.Fatal("the replay of an increase is the same row")
	}
	// the key is per player account: another player may use it for their own request
	other := w.player(f, f.acctX)
	w.call("POST", "/v1/me/rg/limits", other, "idem-1", setLimit("deposit", "EUR", "rolling_24h", "123")).want(t, 201, "")
	// missing / malformed key
	w.call("POST", "/v1/me/rg/limits", tok, "", setLimit("deposit", "EUR", "rolling_24h", "1")).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/limits", tok, "bad key", setLimit("deposit", "EUR", "rolling_24h", "1")).want(t, 400, "validation_error")
}

// N parallel requests with the same key produce exactly one row; the rest replay it.
func TestRGLimitsS2_Player_IdempotencyConcurrentSameKey(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	tok := w.player(f, f.acctA1)
	const n = 12
	var wg sync.WaitGroup
	res := make([]rgResp, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i] = w.call("POST", "/v1/me/rg/limits", tok, "same-key", setLimit("deposit", "EUR", "rolling_24h", "250"))
		}(i)
	}
	wg.Wait()
	ids := map[any]bool{}
	for _, r := range res {
		r.want(t, 201, "")
		ids[r.obj(t)["id"]] = true
	}
	if len(ids) != 1 {
		t.Fatalf("one stored row expected, got %d distinct ids", len(ids))
	}
	if c := w.changeCount(f.tenantA, f.acctA1); c != 1 {
		t.Fatalf("exactly one row, got %d", c)
	}
}

// Parallel increases and decreases of ONE limit through the API: no server error, the history is a
// consistent total order, the pending cap holds, and the realized value is one the caller asked for.
func TestRGLimitsS2_Player_ConcurrentIncreaseDecrease(t *testing.T) {
	w := newRGW(t, rgWOpt{limits: rg.NewLimits(rg.LimitsConfig{PendingCap: 3})})
	f := w.fixture()
	w.floor(f.jur, "deposit", rgfix.Spec{"increase_cooldown": "00:00:01"})
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "seed", setLimit("deposit", "EUR", "rolling_24h", "2000")).want(t, 201, "")
	const n = 24
	var wg sync.WaitGroup
	res := make([]rgResp, n)
	asked := map[string]bool{"2000": true}
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		v := fmt.Sprintf("%d", 100+i*150) // a mix of values below and above 2000
		mu.Lock()
		asked[v] = true
		mu.Unlock()
		go func(i int, v string) {
			defer wg.Done()
			res[i] = w.call("POST", "/v1/me/rg/limits", tok, fmt.Sprintf("par-%d", i), setLimit("deposit", "EUR", "rolling_24h", v))
		}(i, v)
	}
	wg.Wait()
	for i, r := range res {
		switch r.status {
		case 201, 202:
		case 409:
			if c := r.code(t); c != "RG_NO_CHANGE" && c != "RG_PENDING_CAP" {
				t.Fatalf("request %d: unexpected 409 %s", i, c)
			}
		default:
			t.Fatalf("request %d: unexpected %d %s", i, r.status, r.body)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	got := w.call("GET", "/v1/me/rg/limits", tok, "", nil)
	v, _ := limitValue(t, got, "deposit")
	if !asked[v] {
		t.Fatalf("the realized value %q was never requested", v)
	}
	// the history is ordered and the open pending count never exceeded the cap
	hist := got.obj(t)["history"].([]any)
	var prev time.Time
	pendingOpen := 0
	for i := len(hist) - 1; i >= 0; i-- {
		h := hist[i].(map[string]any)
		rt := parseTime(h["requested_at"])
		if rt.Before(prev) {
			t.Fatal("history is not ordered by requested_at")
		}
		prev = rt
		if h["pending"] == true {
			pendingOpen++
		}
	}
	if pendingOpen > 3 {
		t.Fatalf("open pending changes %d exceed the cap 3", pendingOpen)
	}
	// every stored row was classified by the database: decrease rows are < the previous realized, etc. - spot check shape
	var bad int
	w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM player_rg_limit_changes WHERE tenant_id = $1 AND player_account_id = $2
			AND ((change_kind IN ('initial','decrease') AND effective_at <> requested_at) OR (change_kind = 'increase' AND effective_at <= requested_at))`,
			f.tenantA, f.acctA1).Scan(&bad)
	})
	if bad != 0 {
		t.Fatalf("%d rows violate the server's effective_at rule", bad)
	}
}

func TestRGLimitsS2_Player_PendingCap(t *testing.T) {
	w := newRGW(t, rgWOpt{limits: rg.NewLimits(rg.LimitsConfig{PendingCap: 1})})
	f := w.fixture()
	w.floor(f.jur, "deposit", rgfix.Spec{"increase_cooldown": "00:00:30"})
	w.floor(f.jur, "session_duration", rgfix.Spec{"increase_cooldown": "00:00:30"})
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "p1", setLimit("deposit", "EUR", "rolling_24h", "100")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", tok, "p2", setLimit("session_duration", "", "", "100")).want(t, 201, "")
	first := w.call("POST", "/v1/me/rg/limits", tok, "p3", setLimit("deposit", "EUR", "rolling_24h", "200"))
	first.want(t, 202, "")
	w.call("POST", "/v1/me/rg/limits", tok, "p4", setLimit("session_duration", "", "", "200")).want(t, 409, "RG_PENDING_CAP")
	// a decrease is a protective write: never blocked by the cap
	w.call("POST", "/v1/me/rg/limits", tok, "p5", setLimit("session_duration", "", "", "50")).want(t, 201, "")
	// cancelling frees a slot
	w.call("POST", "/v1/me/rg/limits/changes/"+first.obj(t)["id"].(string)+"/cancel", tok, "p6", nil).want(t, 200, "")
	w.call("POST", "/v1/me/rg/limits", tok, "p7", setLimit("session_duration", "", "", "80")).want(t, 202, "")
}

// The pending cap is race-free: N parallel increases of N different limits with cap 1 yield exactly
// one pending change (the person lock serialises the count and the insert).
func TestRGLimitsS2_Player_PendingCapIsRaceFree(t *testing.T) {
	w := newRGW(t, rgWOpt{limits: rg.NewLimits(rg.LimitsConfig{PendingCap: 1})})
	f := w.fixture()
	w.floor(f.jur, "deposit", rgfix.Spec{"increase_cooldown": "00:00:30"})
	tok := w.player(f, f.acctA1)
	periods := []string{"rolling_24h", "calendar_day"}
	assets := []string{"EUR"}
	_ = assets
	for i, p := range periods {
		w.call("POST", "/v1/me/rg/limits", tok, fmt.Sprintf("seed-%d", i), setLimit("deposit", "EUR", p, "100")).want(t, 201, "")
	}
	const n = 8
	var wg sync.WaitGroup
	res := make([]rgResp, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i] = w.call("POST", "/v1/me/rg/limits", tok, fmt.Sprintf("race-%d", i), setLimit("deposit", "EUR", periods[i%2], fmt.Sprintf("%d", 200+i)))
		}(i)
	}
	wg.Wait()
	accepted := 0
	for _, r := range res {
		switch r.status {
		case 202:
			accepted++
		case 409:
			if c := r.code(t); c != "RG_PENDING_CAP" && c != "RG_NO_CHANGE" {
				t.Fatalf("unexpected 409 %s", c)
			}
		default:
			t.Fatalf("unexpected %d %s", r.status, r.body)
		}
	}
	// cap 1 over two limits: one request per limit can be pending at most, and the cap admits exactly one in total
	if accepted != 1 {
		t.Fatalf("exactly one pending change may be accepted under cap 1, got %d", accepted)
	}
}

// An account under identity review (ADR 0027 Uncertain registration: a fresh Person, no person-level
// privileges) may not loosen a tenant_person limit.
func TestRGLimitsS2_Player_IdentityReviewCannotLoosenPersonLimit(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "wager", rgfix.Spec{"scope": "tenant_person"})
	tok := w.player(f, f.acctX)
	w.call("POST", "/v1/me/rg/limits", tok, "i1", setLimit("wager", "EUR", "rolling_24h", "100")).want(t, 201, "")
	o := w.call("GET", "/v1/me/rg/limits", tok, "", nil).obj(t)["limits"].([]any)[0].(map[string]any)
	if o["scope"] != "tenant_person" {
		t.Fatalf("the scope is the configured one, not a client choice: %v", o)
	}
	w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
		_, err := tx.Exec(ctx, `UPDATE player_accounts SET status = 'identity_review_required' WHERE id = $1`, f.acctX)
		return err
	})
	w.call("POST", "/v1/me/rg/limits", tok, "i2", setLimit("wager", "EUR", "rolling_24h", "300")).want(t, 403, "RG_IDENTITY_REVIEW")
	// decreases remain allowed
	w.call("POST", "/v1/me/rg/limits", tok, "i3", setLimit("wager", "EUR", "rolling_24h", "50")).want(t, 201, "")
}

// A person-level (tenant_person) limit is visible to, and changeable from, the Person's other
// account in the same tenant, never from another tenant.
func TestRGLimitsS2_Player_TenantPersonScopeAcrossBrands(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "wager", rgfix.Spec{"scope": "tenant_person"})
	a1, a2, b := w.player(f, f.acctA1), w.player(f, f.acctA2), w.player(f, f.acctB)
	w.call("POST", "/v1/me/rg/limits", a1, "t1", setLimit("wager", "EUR", "rolling_24h", "400")).want(t, 201, "")
	if v, has := limitValue(t, w.call("GET", "/v1/me/rg/limits", a2, "", nil), "wager"); v != "400" || !has {
		t.Fatalf("the Person's other account (brand A2) sees the tenant_person limit: %q", v)
	}
	// ...and a decrease from the other account applies to the shared key
	w.call("POST", "/v1/me/rg/limits", a2, "t2", setLimit("wager", "EUR", "rolling_24h", "300")).want(t, 201, "")
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", a1, "", nil), "wager"); v != "300" {
		t.Fatalf("shared key: %q", v)
	}
	// the same Person's account in ANOTHER tenant has no such limit (OQ-3: limits stop at the operator boundary)
	rb := w.call("GET", "/v1/me/rg/limits", b, "", nil)
	if _, has := limitValue(t, rb, "wager"); has {
		t.Fatalf("a limit must not cross tenants: %s", rb.body)
	}
}

// Request-shape hardening: a client never names the tenant, person, scope, change kind or a time.
func TestRGLimitsS2_Player_RejectsClientSuppliedAuthoritativeFields(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	tok := w.player(f, f.acctA1)
	for _, extra := range []string{"tenant_id", "person_id", "scope", "change_kind", "effective_at", "requested_at", "player_account_id", "brand_id", "jurisdiction_code"} {
		b := setLimit("deposit", "EUR", "rolling_24h", "100")
		b[extra] = uuid.NewString()
		w.call("POST", "/v1/me/rg/limits", tok, "x-"+extra, b).want(t, 400, "validation_error")
	}
	w.call("POST", "/v1/me/rg/limits", tok, "x1", rgBody{"kind": "deposit", "asset_code": "EUR", "period": "rolling_24h"}).want(t, 400, "validation_error") // value absent
	w.call("POST", "/v1/me/rg/limits", tok, "x2", setLimit("deposit", "EUR", "rolling_24h", "0")).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/limits", tok, "x3", setLimit("deposit", "EUR", "rolling_24h", "-5")).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/limits", tok, "x4", setLimit("deposit", "EUR", "rolling_24h", 1.5)).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/limits", tok, "x5", setLimit("deposit", "", "rolling_24h", "5")).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/limits", tok, "x6", setLimit("time_out", "", "", "5")).want(t, 400, "validation_error")
	w.call("POST", "/v1/me/rg/limits", tok, "x7", setLimit("deposit", "NOPE", "rolling_24h", "5")).want(t, 400, "validation_error") // unknown asset: FK
	w.call("POST", "/v1/me/rg/limits", tok, "x8", "{not json").want(t, 400, "validation_error")
	if n := w.changeCount(f.tenantA, f.acctA1); n != 0 {
		t.Fatalf("no refused request may store a row, got %d", n)
	}
}

func TestRGLimitsS2_Player_AuditRecordsAreComplete(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "au1", setLimit("deposit", "EUR", "rolling_24h", "500")).want(t, 201, "")
	inc := w.call("POST", "/v1/me/rg/limits", tok, "au2", setLimit("deposit", "EUR", "rolling_24h", "700"))
	inc.want(t, 202, "")
	rows := w.auditRows(f.tenantA, "rg.limit.change_requested")
	if len(rows) != 2 {
		t.Fatalf("one success audit row per accepted request, got %d", len(rows))
	}
	r := rows[1]
	for _, k := range []string{"person_id", "player_account_id", "control_kind", "asset_code", "period", "change_kind", "effective_value_before",
		"effective_value_after", "pending_value", "effective_at", "config_version_ids", "reason_code", "source"} {
		if _, ok := r[k]; !ok {
			t.Errorf("audit metadata lacks %q: %v", k, r)
		}
	}
	if r["effective_value_before"] != "500" || r["effective_value_after"] != "500" || r["pending_value"] != "700" || r["change_kind"] != "increase" {
		t.Errorf("before / after / pending: %v", r)
	}
	if r["_outcome"] != "success" || r["_actor"] != f.acctA1.String() || r["_ip"] == "" || r["_request_id"] == "" || r["_target"] != inc.obj(t)["id"] {
		t.Errorf("actor / outcome / ip / request id / target: %v", r)
	}
	if ids, _ := r["config_version_ids"].([]any); len(ids) == 0 {
		t.Errorf("the config version used is recorded: %v", r)
	}
	// a refusal is audited with its closed code
	w.call("POST", "/v1/me/rg/limits", tok, "au3", setLimit("deposit", "EUR", "rolling_24h", "9999")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	den := w.auditRows(f.tenantA, "rg.limit.change_denied")
	if len(den) != 1 || den[0]["code"] != "above_ceiling" || den[0]["_outcome"] != "denied" {
		t.Fatalf("denial audit: %v", den)
	}
}

// Audit is fail-closed: when the audit write fails, the mutation does not happen.
func TestRGLimitsS2_AuditFailClosed_Player(t *testing.T) {
	failing := rg.NewLimits(rg.LimitsConfig{Audit: func(ctx ctxT, tx txT, e auditEntry) error { return fmt.Errorf("injected audit failure") }})
	w := newRGW(t, rgWOpt{limits: failing})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	tok := w.player(f, f.acctA1)
	r := w.call("POST", "/v1/me/rg/limits", tok, "af1", setLimit("deposit", "EUR", "rolling_24h", "100"))
	r.want(t, 500, "internal_error")
	if n := w.changeCount(f.tenantA, f.acctA1); n != 0 {
		t.Fatalf("the limit change must roll back with its audit record, got %d rows", n)
	}
	// the time-out: no restriction row either
	tr := w.call("POST", "/v1/me/rg/time-out", tok, "af2", rgBody{"duration_hours": 24})
	tr.want(t, 500, "internal_error")
	var n int
	w.ownerTenant(f.tenantA, func(ctx ctxT, tx txT) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM player_restrictions WHERE player_account_id = $1`, f.acctA1).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("the time-out must roll back with its audit record, got %d rows", n)
	}
}

func TestRGLimitsS2_Player_RateLimited(t *testing.T) {
	clock := newFakeClockForRG()
	w := newRGW(t, rgWOpt{limiter: newRGLimiterForTest(clock, 0.001, 2)})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	tok := w.player(f, f.acctA1)
	w.call("POST", "/v1/me/rg/limits", tok, "r1", setLimit("deposit", "EUR", "rolling_24h", "100")).want(t, 201, "")
	w.call("POST", "/v1/me/rg/limits", tok, "r2", setLimit("deposit", "EUR", "rolling_24h", "90")).want(t, 201, "")
	r := w.call("POST", "/v1/me/rg/limits", tok, "r3", setLimit("deposit", "EUR", "rolling_24h", "80"))
	r.want(t, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Fatal("Retry-After must be set")
	}
	// per player: another player is unaffected
	w.call("POST", "/v1/me/rg/limits", w.player(f, f.acctX), "r4", setLimit("deposit", "EUR", "rolling_24h", "80")).want(t, 201, "")
	if n := w.changeCount(f.tenantA, f.acctA1); n != 2 {
		t.Fatalf("a rate-limited request stores nothing, got %d rows", n)
	}
}

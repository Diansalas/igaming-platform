//go:build integration

// A10c (ADR 0114 gaps G1, G2, G5, G6, G9, G10) over real HTTP on a private scratch database migrated to the
// latest on-disk migration (migration 0164 included): the attempt provider / age filters and event history, the
// reconciliation single-run / single-mismatch reads and the run_id filter, the hold-candidate list, the paged K1
// lists and the staff Idempotency-Key wrapper on the K1 / K3 / HSEC mutating routes. Synthetic data, MOCK provider.
package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// doRaw is cgAPI.do with a raw body and extra headers.
func (a *cgAPI) doRaw(method, path, token string, raw []byte, hdr map[string][]string) cgResp {
	a.t.Helper()
	var rdr io.Reader
	if raw != nil {
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	return cgResp{status: resp.StatusCode, body: b, header: resp.Header.Clone()}
}

// isAlreadyApplied reports whether body is the fixed replay body (SR10C-1: a replay never carries the stored DTO).
func isAlreadyApplied(body []byte) bool {
	return strings.TrimSpace(string(body)) == `{"status":"already_applied"}`
}

func idemHdr(k string) map[string][]string { return map[string][]string{"Idempotency-Key": {k}} }

func a10cJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (a *cgAPI) idemRows(t *testing.T, tenantID uuid.UUID) (n int) {
	t.Helper()
	if err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_idempotency_keys`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- K1: paging and Idempotency-Key ---

func TestA10c_K1_PagedListsAndIdempotency(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	other := a.tenant()
	tenantAdmin := a.staff(tenantID, "tenant_admin")
	tok := a.token(tenantAdmin, tenantID, auth.RoleTenantAdmin)
	base := "/v1/admin/tenants/" + tenantID.String() + "/capability-grants"
	var grantees []uuid.UUID
	for i := 0; i < 3; i++ {
		grantees = append(grantees, a.staff(tenantID, "finance"))
	}
	body := func(g uuid.UUID, reason string) []byte {
		return a10cJSON(t, map[string]any{"grantee_staff_id": g.String(), "capability": "ledger_adjustment:initiate", "reason_code": reason})
	}

	// Idempotency: the same key + same body replays the stored 201 byte for byte and creates ONE request.
	first := a.doRaw("POST", base+"/requests", tok, body(grantees[0], "t1"), idemHdr("k1-create"))
	if first.status != http.StatusCreated {
		t.Fatalf("create: %d %s", first.status, first.body)
	}
	if first.header.Get("Idempotent-Replayed") != "" {
		t.Error("the first response must not be marked as a replay")
	}
	replay := a.doRaw("POST", base+"/requests", tok, body(grantees[0], "t1"), idemHdr("k1-create"))
	if replay.status != http.StatusCreated || !isAlreadyApplied(replay.body) || replay.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d replayed=%q: %s", replay.status, replay.header.Get("Idempotent-Replayed"), replay.body)
	}
	if n := a.requestCount(tenantID, grantees[0]); n != 1 {
		t.Fatalf("a replayed create made %d requests, want 1", n)
	}
	// Without the key the same request is the old state refusal (409): the key is what makes it a replay.
	if res := a.doRaw("POST", base+"/requests", tok, body(grantees[0], "t1"), nil); res.status != http.StatusConflict {
		t.Fatalf("unkeyed duplicate: %d %s", res.status, res.body)
	}
	// The same key with a different request is 409 IDEMPOTENCY_MISMATCH and does not act.
	mm := a.doRaw("POST", base+"/requests", tok, body(grantees[1], "t1"), idemHdr("k1-create"))
	if mm.status != http.StatusConflict || !strings.Contains(string(mm.body), "IDEMPOTENCY_MISMATCH") {
		t.Fatalf("mismatch: %d %s", mm.status, mm.body)
	}
	if n := a.requestCount(tenantID, grantees[1]); n != 0 {
		t.Fatalf("a mismatching key created %d requests", n)
	}
	// A refused (non-2xx) request stores nothing: the same key may be retried with a corrected request.
	bad := a.doRaw("POST", base+"/requests", tok, a10cJSON(t, map[string]any{"grantee_staff_id": "nope", "capability": "x", "reason_code": "t"}), idemHdr("k1-retry"))
	if bad.status < 400 || bad.status >= 500 {
		t.Fatalf("bad request: %d %s", bad.status, bad.body)
	}
	if res := a.doRaw("POST", base+"/requests", tok, body(grantees[1], "t2"), idemHdr("k1-retry")); res.status != http.StatusCreated {
		t.Fatalf("retry of a refused key: %d %s", res.status, res.body)
	}
	if res := a.doRaw("POST", base+"/requests", tok, body(grantees[2], "t3"), idemHdr("k1-3")); res.status != http.StatusCreated {
		t.Fatalf("third create: %d %s", res.status, res.body)
	}
	// Cancel (204, empty body) replays too.
	var created struct {
		ID string `json:"id"`
	}
	first.decode(t, &created)
	cancel := base + "/requests/" + created.ID + "/cancel"
	c1 := a.doRaw("POST", cancel, tok, nil, idemHdr("k1-cancel"))
	c2 := a.doRaw("POST", cancel, tok, nil, idemHdr("k1-cancel"))
	if c1.status != http.StatusNoContent || c2.status != http.StatusNoContent || c2.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("cancel replay: %d / %d replayed=%q", c1.status, c2.status, c2.header.Get("Idempotent-Replayed"))
	}
	if res := a.doRaw("POST", cancel, tok, nil, nil); res.status != http.StatusConflict {
		t.Fatalf("unkeyed cancel replay: %d", res.status)
	}
	// Key validation: empty, over-long, control character and a repeated header are 400 and create nothing.
	for name, h := range map[string]map[string][]string{
		"empty": {"Idempotency-Key": {""}}, "long": idemHdr(strings.Repeat("k", 121)), "control": idemHdr("a\tb"),
		"repeated": {"Idempotency-Key": {"a", "b"}},
	} {
		if res := a.doRaw("POST", base+"/requests", tok, body(grantees[2], "t4"), h); res.status != http.StatusBadRequest {
			t.Errorf("key %s: %d %s", name, res.status, res.body)
		}
	}
	// Authorization is decided first: a role without the permission is 403 and reserves nothing; a foreign tenant too.
	before := a.idemRows(t, tenantID)
	sup := a.token(a.staff(tenantID, "support"), tenantID, auth.RoleSupport)
	if res := a.doRaw("POST", base+"/requests", sup, body(grantees[2], "t5"), idemHdr("k-support")); res.status != http.StatusForbidden {
		t.Fatalf("support with a key: %d", res.status)
	}
	otherAdmin := a.token(a.staff(other, "tenant_admin"), other, auth.RoleTenantAdmin)
	if res := a.doRaw("POST", base+"/requests", otherAdmin, body(grantees[2], "t5"), idemHdr("k-foreign")); res.status != http.StatusForbidden {
		t.Fatalf("foreign tenant admin with a key: %d", res.status)
	}
	if after := a.idemRows(t, tenantID); after != before || a.idemRows(t, other) != 0 {
		t.Fatalf("a refused caller reserved keys: tenant %d->%d, other %d", before, after, a.idemRows(t, other))
	}
	// RLS: the other tenant's scope sees none of this tenant's keys.
	if a.idemRows(t, other) != 0 {
		t.Fatal("the other tenant's scope can read this tenant's idempotency keys")
	}

	// Paging: a bare array, X-Total-Count, stable order, no overlap, legacy call unchanged.
	all := a.doRaw("GET", base+"/requests", tok, nil, nil)
	var every []map[string]any
	all.decode(t, &every)
	if all.status != http.StatusOK || len(every) != 3 || all.header.Get("X-Total-Count") != "3" {
		t.Fatalf("legacy list: %d n=%d total=%q", all.status, len(every), all.header.Get("X-Total-Count"))
	}
	seen := map[string]bool{}
	for off := 0; off < 3; off += 2 {
		res := a.doRaw("GET", base+"/requests?limit=2&offset="+strconv.Itoa(off), tok, nil, nil)
		var page []map[string]any
		res.decode(t, &page)
		if res.status != http.StatusOK || res.header.Get("X-Total-Count") != "3" {
			t.Fatalf("page off=%d: %d total=%q %s", off, res.status, res.header.Get("X-Total-Count"), res.body)
		}
		wantN := map[int]int{0: 2, 2: 1}[off]
		if len(page) != wantN {
			t.Fatalf("page off=%d has %d rows, want %d", off, len(page), wantN)
		}
		for _, r := range page {
			id := r["id"].(string)
			if seen[id] {
				t.Errorf("row %s appears on two pages", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 3 {
		t.Errorf("pages covered %d rows, want 3", len(seen))
	}
	// Order equals the legacy order.
	p0 := a.doRaw("GET", base+"/requests?limit=1", tok, nil, nil)
	var first1 []map[string]any
	p0.decode(t, &first1)
	if len(first1) != 1 || first1[0]["id"] != every[0]["id"] {
		t.Errorf("first page row %v != first legacy row %v", first1, every[0]["id"])
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=x", "offset=-1", "foo=bar", "limit=1&limit=2", "tenant_id=" + other.String()} {
		if res := a.doRaw("GET", base+"/requests?"+q, tok, nil, nil); res.status != http.StatusBadRequest {
			t.Errorf("query %q: %d", q, res.status)
		}
		if res := a.doRaw("GET", base+"?"+q, tok, nil, nil); res.status != http.StatusBadRequest {
			t.Errorf("grants query %q: %d", q, res.status)
		}
	}
	g := a.doRaw("GET", base+"?limit=2", tok, nil, nil)
	if g.status != http.StatusOK || g.header.Get("X-Total-Count") != "0" || strings.TrimSpace(string(g.body)) != "[]" {
		t.Errorf("empty grants page: %d total=%q %s", g.status, g.header.Get("X-Total-Count"), g.body)
	}
	// Tenant isolation of the list: the other tenant's admin cannot page this tenant's rows.
	if res := a.doRaw("GET", base+"/requests?limit=2", otherAdmin, nil, nil); res.status != http.StatusForbidden {
		t.Errorf("foreign paged list: %d", res.status)
	}
}

// An in_progress reservation of the SAME request is 409 IDEMPOTENCY_IN_PROGRESS (a retry racing the first), and a
// different request under it is the mismatch.
func TestA10c_Idempotency_InProgressReservation(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	admin := a.staff(tenantID, "tenant_admin")
	tok := a.token(admin, tenantID, auth.RoleTenantAdmin)
	grantee := a.staff(tenantID, "finance")
	path := "/v1/admin/tenants/" + tenantID.String() + "/capability-grants/requests"
	raw := a10cJSON(t, map[string]any{"grantee_staff_id": grantee.String(), "capability": "ledger_adjustment:initiate", "reason_code": "t"})
	sum := sha256.New()
	sum.Write([]byte("POST\n" + path + "\n\n"))
	sum.Write(raw)
	if err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_idempotency_keys (tenant_id, actor_id, idempotency_key, route, request_hash, state)
			VALUES ($1, $2, 'racing', $3, $4, 'in_progress')`, tenantID, admin, "POST "+path, sum.Sum(nil))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res := a.doRaw("POST", path, tok, raw, idemHdr("racing"))
	if res.status != http.StatusConflict || !strings.Contains(string(res.body), "IDEMPOTENCY_IN_PROGRESS") {
		t.Fatalf("in-progress: %d %s", res.status, res.body)
	}
	if n := a.requestCount(tenantID, grantee); n != 0 {
		t.Fatalf("an in-progress key let the handler run (%d requests)", n)
	}
	// The key belongs to the actor: another actor of the same tenant using the same key string is not blocked.
	admin2 := a.staff(tenantID, "tenant_admin")
	tok2 := a.token(admin2, tenantID, auth.RoleTenantAdmin)
	if res := a.doRaw("POST", path, tok2, raw, idemHdr("racing")); res.status != http.StatusCreated {
		t.Fatalf("another actor with the same key string: %d %s", res.status, res.body)
	}
}

// --- K3 / HSEC: Idempotency-Key, filters, events, reconciliation reads, hold candidates ---

func TestA10c_K3_IdempotentRequestAndDecisionReplay(t *testing.T) {
	w := newSRWorld(t)
	body := a10cJSON(t, map[string]any{"attempt_id": w.depA[0].String(), "kind": "m1_deposit_evidence", "finding_code": "awaiting_psp_refund",
		"reason_code": "http-k3", "note": "a10c"})
	r1 := w.a.doRaw("POST", w.base(), w.tok(w.f1), body, idemHdr("k3-req"))
	if r1.status != http.StatusCreated {
		t.Fatalf("request: %d %s", r1.status, r1.body)
	}
	r2 := w.a.doRaw("POST", w.base(), w.tok(w.f1), body, idemHdr("k3-req"))
	if r2.status != http.StatusCreated || !isAlreadyApplied(r2.body) || r2.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s", r2.status, r2.body)
	}
	var d frDTO
	r1.decode(t, &d)
	list := w.a.do("GET", w.base(), w.tok(w.f1), nil)
	if strings.Count(string(list.body), d.ID) != 1 || strings.Count(string(list.body), `"attempt_id"`) != 1 {
		t.Fatalf("a replayed request created a second resolution: %s", list.body)
	}
	// Cancel by the requester replays (no second cancel, no 409), then an unkeyed cancel is the state refusal.
	cancel := w.base() + "/" + d.ID + "/cancel"
	c1 := w.a.doRaw("POST", cancel, w.tok(w.f1), nil, idemHdr("k3-cancel"))
	c2 := w.a.doRaw("POST", cancel, w.tok(w.f1), nil, idemHdr("k3-cancel"))
	if c1.status != http.StatusOK || c2.status != http.StatusOK || !isAlreadyApplied(c2.body) || c2.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("cancel replay: %d %s / %d %s", c1.status, c1.body, c2.status, c2.body)
	}
	if res := w.a.doRaw("POST", cancel, w.tok(w.f1), nil, nil); res.status < 400 {
		t.Fatalf("unkeyed second cancel: %d", res.status)
	}
	// A role without the permission is refused before the key is looked at: no row for the foreign key.
	n0 := w.a.idemRows(t, w.tenant)
	if res := w.a.doRaw("POST", w.base(), w.tok(w.supp), body, idemHdr("k3-support")); res.status != http.StatusForbidden {
		t.Fatalf("support: %d", res.status)
	}
	if n1 := w.a.idemRows(t, w.tenant); n1 != n0 {
		t.Fatalf("a forbidden caller reserved a key (%d -> %d)", n0, n1)
	}
}

func TestA10c_R1_ProviderAndAgeFilters(t *testing.T) {
	w := newSRWorld(t)
	tok := w.tok(w.f1)
	count := func(q string) int {
		res := w.a.do("GET", w.attemptsURL(w.tenant, q), tok, nil)
		if res.status != http.StatusOK {
			t.Fatalf("%s: %d %s", q, res.status, res.body)
		}
		return len(decodePage(t, res).Items)
	}
	for q, want := range map[string]int{
		"provider_id=mock": 6, "provider_id=nobody": 0, "min_age_seconds=0": 6, "min_age_seconds=86400": 0, "min_age_seconds=31536000": 0,
		"provider_id=mock&state=ambiguous": 1, "provider_id=mock&min_age_seconds=0&operation=payout": 3,
	} {
		if got := count(q); got != want {
			t.Errorf("filter %q: %d rows, want %d", q, got, want)
		}
	}
	for _, q := range []string{"provider_id=", "provider_id=a%20b", "provider_id=" + strings.Repeat("a", 200), "provider_id=x&provider_id=y",
		"min_age_seconds=-1", "min_age_seconds=31536001", "min_age_seconds=abc", "min_age_seconds=", "min_age_seconds=1.5", "min_age_seconds=01",
		"min_age_seconds=1&min_age_seconds=2", "provider=mock"} {
		if res := w.a.do("GET", w.attemptsURL(w.tenant, q), tok, nil); res.status != http.StatusBadRequest {
			t.Errorf("query %q: got %d, want 400: %s", q, res.status, res.body)
		}
	}
	// Tenant isolation: the other tenant's finance, with the same filters, sees only its own attempt.
	res := w.a.do("GET", w.attemptsURL(w.other, "provider_id=mock"), w.tok(w.otherFin), nil)
	if pg := decodePage(t, res); res.status != http.StatusOK || len(pg.Items) != 1 || pg.Items[0].ID != w.depB.String() {
		t.Errorf("tenant B filtered view: %d %+v", res.status, pg.Items)
	}
}

// A non-zero age threshold is a real predicate: after a pause every attempt is older than 2 s but none older than an hour.
func TestA10c_R1_AttemptAgeFilterIsATimePredicate(t *testing.T) {
	w := newSRWorld(t)
	time.Sleep(2200 * time.Millisecond)
	for q, want := range map[string]int{"min_age_seconds=2": 6, "min_age_seconds=3600": 0, "min_age_seconds=2&operation=deposit": 3} {
		res := w.a.do("GET", w.attemptsURL(w.tenant, q), w.tok(w.f1), nil)
		if got := len(decodePage(t, res).Items); res.status != http.StatusOK || got != want {
			t.Errorf("%s: %d rows (status %d), want %d", q, got, res.status, want)
		}
	}
}

func (w *srWorld) seedEvents(t *testing.T, tenantID, attempt uuid.UUID, n int) {
	t.Helper()
	secret := "EVT-SECRET-REF-" + attempt.String()[:8]
	w.secrets = append(w.secrets, secret, "MERCH-SECRET-EVT", "DECLINE-FREE-TEXT")
	if err := w.a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			fp := sha256.Sum256([]byte(attempt.String() + strconv.Itoa(i)))
			if _, err := tx.Exec(ctx, `INSERT INTO payment_provider_events (id, tenant_id, provider_id, event_type, provider_reference, merchant_reference,
				outcome, amount, asset_code, cascadable, decline_stage, decline_reason, event_fingerprint, disposition_at_receipt, attempt_id, received_at)
				VALUES ($1, $2, 'mock', 'deposit', $3, 'MERCH-SECRET-EVT', $4, $5, $6, $7, $8, 'DECLINE-FREE-TEXT', $9, 'applied', $10, now() - make_interval(mins => $11))`,
				uuid.New(), tenantID, secret, map[bool]string{true: "succeeded", false: "declined"}[i == 0],
				map[bool]any{true: 5000, false: nil}[i == 0], map[bool]any{true: "USD", false: nil}[i == 0],
				map[bool]any{true: nil, false: true}[i == 0], map[bool]any{true: nil, false: "at_submission"}[i == 0], fp[:], attempt, n-i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed events: %v", err)
	}
}

func TestA10c_R1_AttemptEventHistory(t *testing.T) {
	w := newSRWorld(t)
	w.seedEvents(t, w.tenant, w.unbAtt, 3)
	evURL := func(tenant, attempt uuid.UUID, q string) string {
		u := w.attemptsURL(tenant, "") + "/" + attempt.String() + "/events"
		if q != "" {
			u += "?" + q
		}
		return u
	}
	res := w.a.do("GET", evURL(w.tenant, w.unbAtt, ""), w.tok(w.f1), nil)
	if res.status != http.StatusOK {
		t.Fatalf("events: %d %s", res.status, res.body)
	}
	w.noLeak(t, "events", res.body)
	for _, bad := range []string{"fingerprint", "decline_reason", "provider_reference", "merchant_reference", "settlement_reference"} {
		if strings.Contains(string(res.body), bad) {
			t.Errorf("event body contains %q: %s", bad, res.body)
		}
	}
	var pg pagedResponse[paymentAttemptEventDTO]
	res.decode(t, &pg)
	if pg.Total != 3 || len(pg.Items) != 3 || pg.Items[0].Outcome != "succeeded" || pg.Items[0].AmountMinorUnits == nil || *pg.Items[0].AmountMinorUnits != "5000" ||
		pg.Items[0].AssetCode == nil || *pg.Items[0].AssetCode != "USD" || pg.Items[1].DeclineStage == nil || *pg.Items[1].DeclineStage != "at_submission" {
		t.Fatalf("events page = %+v", pg)
	}
	if pg.Items[0].ReceivedAt > pg.Items[1].ReceivedAt || pg.Items[1].ReceivedAt > pg.Items[2].ReceivedAt {
		t.Errorf("events are not oldest first: %+v", pg.Items)
	}
	p2 := w.a.do("GET", evURL(w.tenant, w.unbAtt, "limit=2&offset=2"), w.tok(w.comp), nil)
	var pg2 pagedResponse[paymentAttemptEventDTO]
	p2.decode(t, &pg2)
	if p2.status != http.StatusOK || len(pg2.Items) != 1 || pg2.Total != 3 || pg2.Items[0].ID != pg.Items[2].ID {
		t.Fatalf("second page: %d %+v", p2.status, pg2)
	}
	// An attempt with no events: 200 and an empty page; an unknown attempt: 404.
	empty := w.a.do("GET", evURL(w.tenant, w.destAtt, ""), w.tok(w.f1), nil)
	if empty.status != http.StatusOK || !strings.Contains(string(empty.body), `"items":[]`) || !strings.Contains(string(empty.body), `"total":0`) {
		t.Errorf("no-event attempt: %d %s", empty.status, empty.body)
	}
	if res := w.a.do("GET", evURL(w.tenant, uuid.New(), ""), w.tok(w.f1), nil); res.status != http.StatusNotFound {
		t.Errorf("unknown attempt: %d", res.status)
	}
	// Authorization: only payment_force_resolve:read holders.
	for name, c := range map[string]struct {
		tok  string
		want int
	}{
		"platform_admin": {w.tok(w.pa), 200}, "compliance": {w.tok(w.comp), 200}, "tenant_admin": {w.tok(w.ta), 403},
		"support": {w.tok(w.supp), 403}, "risk": {w.rmTok, 403}, "none": {"", 401},
	} {
		if res := w.a.do("GET", evURL(w.tenant, w.unbAtt, ""), c.tok, nil); res.status != c.want {
			t.Errorf("%s: %d want %d", name, res.status, c.want)
		}
	}
	// Tenant isolation: tenant B's finance cannot read tenant A's attempt events, by either tenant path.
	for _, tn := range []uuid.UUID{w.tenant, w.other} {
		if res := w.a.do("GET", evURL(tn, w.unbAtt, ""), w.tok(w.otherFin), nil); res.status != http.StatusNotFound {
			t.Errorf("tenant B finance, path tenant %s: %d (want 404)", tn, res.status)
		}
	}
	for _, q := range []string{"limit=0", "limit=201", "cursor=x", "foo=1", "limit=1&limit=2"} {
		if res := w.a.do("GET", evURL(w.tenant, w.unbAtt, q), w.tok(w.f1), nil); res.status != http.StatusBadRequest {
			t.Errorf("query %q: %d", q, res.status)
		}
	}
	if res := w.a.do("GET", w.attemptsURL(w.tenant, "")+"/not-a-uuid/events", w.tok(w.f1), nil); res.status != http.StatusBadRequest {
		t.Errorf("bad attempt id: %d", res.status)
	}
	// A platform read is audited (tenant-visible platform-action row), and a ghost tenant writes none.
	before := w.platformReadAudits(t, w.tenant, "payment_attempts.platform_read")
	if res := w.a.do("GET", evURL(w.tenant, w.unbAtt, ""), w.tok(w.pa), nil); res.status != http.StatusOK {
		t.Fatalf("platform events read: %d", res.status)
	}
	if after := w.platformReadAudits(t, w.tenant, "payment_attempts.platform_read"); after != before+1 {
		t.Errorf("platform events read wrote %d audit rows, want 1", after-before)
	}
	ghost := uuid.New()
	if res := w.a.do("GET", evURL(ghost, w.unbAtt, ""), w.tok(w.pa), nil); res.status != http.StatusNotFound {
		t.Errorf("ghost tenant: %d", res.status)
	}
	if n := w.platformReadAudits(t, ghost, "payment_attempts.platform_read"); n != 0 {
		t.Errorf("ghost tenant produced %d audit rows", n)
	}
	// Events of tenant A are not visible through tenant B's own scope even for a B attempt id lookup.
	w.seedEvents(t, w.other, w.depB, 1)
	b := w.a.do("GET", evURL(w.other, w.depB, ""), w.tok(w.otherFin), nil)
	var pgB pagedResponse[paymentAttemptEventDTO]
	b.decode(t, &pgB)
	if b.status != http.StatusOK || pgB.Total != 2 {
		t.Errorf("tenant B own events: %d %+v", b.status, pgB)
	}
}

func TestA10c_R2_SingleReadsAndRunFilter(t *testing.T) {
	w := newSRWorld(t)
	s := w.seedRecon(t, w.tenant)
	tokComp := w.tok(w.comp)
	one := func(kind, id, stream, tok string, tenant uuid.UUID) cgResp {
		return w.a.do("GET", w.reconURL(tenant, kind, "")+"/"+id+"?stream="+stream, tok, nil)
	}
	// Single run: found, wrong stream 404, unknown 404.
	run := one("runs", s.runPay.String(), "payment_statement", tokComp, w.tenant)
	var rd staffReconciliationRunDTO
	run.decode(t, &rd)
	if run.status != http.StatusOK || rd.ID != s.runPay.String() || rd.Stream != "payment_statement" || rd.Status != "mismatches_found" {
		t.Fatalf("run: %d %s", run.status, run.body)
	}
	if res := one("runs", s.runPay.String(), "ledger_vs_projection", tokComp, w.tenant); res.status != http.StatusNotFound {
		t.Errorf("run of another stream: %d", res.status)
	}
	if res := one("runs", uuid.NewString(), "payment_statement", tokComp, w.tenant); res.status != http.StatusNotFound {
		t.Errorf("unknown run: %d", res.status)
	}
	// Single mismatch equals its list row, without any free text.
	list := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream=payment_statement&limit=200"), tokComp, nil)
	var pg pagedResponse[staffReconciliationMismatchDTO]
	list.decode(t, &pg)
	if len(pg.Items) != 3 {
		t.Fatalf("payment mismatches = %d", len(pg.Items))
	}
	for _, it := range pg.Items {
		res := one("mismatches", it.ID, "payment_statement", tokComp, w.tenant)
		w.noLeak(t, "single mismatch", res.body)
		for _, bad := range []string{"reconciliation_key", "expected_value", "actual_value", "MREF-SECRET", "SECRET-PSP"} {
			if strings.Contains(string(res.body), bad) {
				t.Errorf("single mismatch body contains %q", bad)
			}
		}
		var got staffReconciliationMismatchDTO
		res.decode(t, &got)
		gj, _ := json.Marshal(got)
		lj, _ := json.Marshal(it)
		if res.status != http.StatusOK || string(gj) != string(lj) {
			t.Errorf("single mismatch %s differs from its list row:\n%s\n%s", it.ID, gj, lj)
		}
		// The same id through another stream is 404.
		if res := one("mismatches", it.ID, "ledger_vs_projection", tokComp, w.tenant); res.status != http.StatusNotFound {
			t.Errorf("mismatch %s through the wrong stream: %d", it.ID, res.status)
		}
	}
	// run_id filter: payment run has 3, the ledger run's id on the payment stream has 0, and it composes with status.
	count := func(stream, q string) int {
		res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream="+stream+q), tokComp, nil)
		if res.status != http.StatusOK {
			t.Fatalf("%s %s: %d %s", stream, q, res.status, res.body)
		}
		var p pagedResponse[staffReconciliationMismatchDTO]
		res.decode(t, &p)
		return p.Total
	}
	if got := count("payment_statement", "&run_id="+s.runPay.String()); got != 3 {
		t.Errorf("run_id=payment run: %d", got)
	}
	if got := count("payment_statement", "&run_id="+s.runLedger.String()); got != 0 {
		t.Errorf("run_id=ledger run on the payment stream: %d", got)
	}
	if got := count("ledger_vs_projection", "&run_id="+s.runLedger.String()+"&status=open"); got != 2 {
		t.Errorf("run_id=ledger run & open: %d", got)
	}
	if got := count("payment_statement", "&run_id="+uuid.NewString()); got != 0 {
		t.Errorf("unknown run_id: %d", got)
	}
	for _, q := range []string{"&run_id=nope", "&run_id=", "&run_id=" + strings.ToUpper(uuid.NewString()), "&run_id=" + uuid.Nil.String(), "&run_id=a&run_id=b"} {
		if res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream=payment_statement"+q), tokComp, nil); res.status != http.StatusBadRequest {
			t.Errorf("run_id query %q: %d", q, res.status)
		}
	}
	// Per-stream permission on the single reads: sportsbook is tenant_admin/compliance/finance only; payment_statement
	// is finance/compliance/platform_admin: tenant_admin is refused on payment, support on everything.
	if res := one("runs", s.runSB.String(), "sportsbook_settlement", w.tok(w.ta), w.tenant); res.status != http.StatusOK {
		t.Errorf("tenant_admin sportsbook run: %d", res.status)
	}
	for _, c := range []struct {
		stream, id string
		tok        string
		want       int
	}{
		{"payment_statement", s.runPay.String(), w.tok(w.ta), 403}, {"payment_statement", s.runPay.String(), w.tok(w.supp), 403},
		{"ledger_vs_projection", s.runLedger.String(), w.tok(w.f1), 200}, {"ledger_vs_projection", s.runLedger.String(), w.tok(w.ta), 403},
		{"payment_statement", s.runPay.String(), "", 401},
	} {
		if res := one("runs", c.id, c.stream, c.tok, w.tenant); res.status != c.want {
			t.Errorf("run single read of %s: %d want %d", c.stream, res.status, c.want)
		}
	}
	// Bad input: missing / unknown stream, bad id, extra parameters.
	for _, p := range []string{"/runs/" + s.runPay.String(), "/runs/" + s.runPay.String() + "?stream=casino_consistency", "/runs/not-a-uuid?stream=payment_statement",
		"/runs/" + s.runPay.String() + "?stream=payment_statement&x=1", "/mismatches/" + uuid.Nil.String() + "?stream=payment_statement",
		"/mismatches/" + strings.ToUpper(uuid.NewString()) + "?stream=payment_statement"} {
		if res := w.a.do("GET", w.reconURL(w.tenant, "", "")+strings.TrimPrefix(p, "/"), tokComp, nil); res.status != http.StatusBadRequest && res.status != http.StatusNotFound {
			t.Errorf("path %q: %d", p, res.status)
		}
	}
	// Tenant isolation: tenant B's finance (same permission) cannot read tenant A's run or mismatch.
	bTok := w.tok(w.otherFin)
	if res := one("runs", s.runPay.String(), "payment_statement", bTok, w.tenant); res.status != http.StatusNotFound {
		t.Errorf("foreign run by path: %d", res.status)
	}
	if res := one("runs", s.runPay.String(), "payment_statement", bTok, w.other); res.status != http.StatusNotFound {
		t.Errorf("foreign run through own tenant: %d", res.status)
	}
	if res := one("mismatches", pg.Items[0].ID, "payment_statement", bTok, w.other); res.status != http.StatusNotFound {
		t.Errorf("foreign mismatch through own tenant: %d", res.status)
	}
	// A platform single read is audited.
	before := w.platformReadAudits(t, w.tenant, "reconciliation.platform_read")
	if res := one("runs", s.runPay.String(), "payment_statement", w.tok(w.pa), w.tenant); res.status != http.StatusOK {
		t.Fatalf("platform run read: %d", res.status)
	}
	if after := w.platformReadAudits(t, w.tenant, "reconciliation.platform_read"); after != before+1 {
		t.Errorf("platform single read wrote %d audit rows", after-before)
	}
}

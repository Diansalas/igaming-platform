//go:build integration

package httpserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// ADR 0114 S1b: keyset pagination, acted-by timestamps and actors, the kind filter and the
// single-alert read of the platform alerts API. Real runtime role, private database.

// seedAlertsOneTx inserts n open platform-owned alerts about tenant in ONE transaction, so every
// row gets the same first_seen_at (the guard sets now(), which is constant inside a transaction):
// the deterministic way to build ties. The returned ids are in no particular order.
func (a *arAPI) seedAlertsOneTx(tenant uuid.UUID, kind alerting.Kind, n int) []uuid.UUID {
	a.t.Helper()
	ids := make([]uuid.UUID, 0, n)
	if err := a.w.Pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes)
				VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`, tenant, string(kind), "s1b:"+uuid.NewString()).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	}); err != nil {
		a.t.Fatalf("seed alerts: %v", err)
	}
	return ids
}

// dbOrder returns every platform alert id in the contract order (first_seen_at DESC, id DESC).
func (a *arAPI) dbOrder() []string {
	a.t.Helper()
	var out []string
	if err := a.w.Pool.WithPlatformAdmin(context.Background(), a.admin, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM alerts WHERE tenant_id IS NULL ORDER BY first_seen_at DESC, id DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	}); err != nil {
		a.t.Fatalf("db order: %v", err)
	}
	return out
}

type alertPage struct {
	ids  []string
	next string // "" = null
	raw  map[string]any
}

func (a *arAPI) alertPage(tok, query string) (alertPage, int) {
	a.t.Helper()
	r := a.do("GET", "/v1/admin/alerts"+query, tok, nil)
	if r.status != http.StatusOK {
		return alertPage{}, r.status
	}
	m := r.json()
	p := alertPage{raw: m}
	for _, it := range m["alerts"].([]any) {
		p.ids = append(p.ids, it.(map[string]any)["id"].(string))
	}
	if n, ok := m["next_cursor"].(string); ok {
		p.next = n
	}
	if _, present := m["next_cursor"]; !present {
		a.t.Fatalf("next_cursor must always be present (null on the last page): %s", r.body)
	}
	return p, r.status
}

// walk pages through the whole filtered list and returns every id in served order.
func (a *arAPI) walk(tok, base string, limit int) []string {
	a.t.Helper()
	var all []string
	cursor := ""
	for i := 0; i < 1000; i++ {
		q := fmt.Sprintf("?limit=%d%s", limit, base)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		p, st := a.alertPage(tok, q)
		if st != http.StatusOK {
			a.t.Fatalf("page %d: status %d", i, st)
		}
		if cursor != "" && len(p.ids) == 0 {
			a.t.Fatalf("page %d is empty after a cursor: a final full page must not advertise a next_cursor", i)
		}
		all = append(all, p.ids...)
		if p.next == "" {
			return all
		}
		if len(p.ids) != limit {
			a.t.Fatalf("a page with a next_cursor must be full: got %d of %d", len(p.ids), limit)
		}
		cursor = p.next
	}
	a.t.Fatal("pagination did not terminate")
	return nil
}

// Beyond 200: 205 alerts (a 200-row first page, then 5), mixed with ties (same first_seen_at) and
// separate transactions; the walk returns exactly the contract order, once each.
func TestAlertListS1b_KeysetReachesBeyond200_TiesAndOrder(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	tok := a.adminToken()
	a.seedAlertsOneTx(tenant, alerting.KindPaymentKillSwitchEngaged, 120)           // one tie group of 120
	a.seedAlertsOneTx(tenant, alerting.KindReconciliationLedgerProjectionDrift, 85) // another tie group of 85
	want := a.dbOrder()
	if len(want) != 205 {
		t.Fatalf("setup: %d alerts", len(want))
	}
	for _, limit := range []int{200, 50, 7} { // limit 1 across a tie group: TestAlertListS1b_TieBoundaryInsideATieGroup
		got := a.walk(tok, "", limit)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("limit %d: served order differs from first_seen_at DESC, id DESC (got %d ids, want %d)", limit, len(got), len(want))
		}
	}
	// Page one of 200 is exactly 200 and carries a cursor; the page after it holds the other 5.
	p1, _ := a.alertPage(tok, "?limit=200")
	if len(p1.ids) != 200 || p1.next == "" {
		t.Fatalf("first page: %d rows, next=%q", len(p1.ids), p1.next)
	}
	p2, _ := a.alertPage(tok, "?limit=200&cursor="+url.QueryEscape(p1.next))
	if len(p2.ids) != 5 || p2.next != "" {
		t.Fatalf("second page: %d rows, next=%q", len(p2.ids), p2.next)
	}
}

// Singleton limit walk over a small tied set: the id tie-break is what makes a page boundary
// inside a tie group safe.
func TestAlertListS1b_TieBoundaryInsideATieGroup(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	tok := a.adminToken()
	a.seedAlertsOneTx(tenant, alerting.KindPaymentKillSwitchEngaged, 9)
	want := a.dbOrder()
	got := a.walk(tok, "", 1)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("limit 1 across a 9-row tie group lost or repeated rows:\n got %v\nwant %v", got, want)
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] > got[j] }) {
		t.Fatal("within a tie group the order must be id DESC")
	}
	// A page that exactly exhausts the set carries no cursor (no phantom empty page).
	if p, _ := a.alertPage(tok, "?limit=9"); len(p.ids) != 9 || p.next != "" {
		t.Fatalf("exact-fit page: %d rows next=%q", len(p.ids), p.next)
	}
	if p, _ := a.alertPage(tok, "?limit=8"); len(p.ids) != 8 || p.next == "" {
		t.Fatalf("limit 8 of 9: %d rows next=%q", len(p.ids), p.next)
	}
}

// Stability under concurrent inserts: alerts raised after page one was served never repeat or
// displace anything on the later pages; they are visible from a fresh first page.
func TestAlertListS1b_KeysetStableUnderConcurrentInserts(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	tok := a.adminToken()
	a.seedAlertsOneTx(tenant, alerting.KindPaymentKillSwitchEngaged, 4)
	a.seedAlertsOneTx(tenant, alerting.KindReconciliationLedgerProjectionDrift, 6)
	before := a.dbOrder()

	p1, _ := a.alertPage(tok, "?limit=3")
	fresh := a.seedAlertsOneTx(tenant, alerting.KindPaymentKillSwitchEngaged, 3) // newer than the cursor
	p2, st := a.alertPage(tok, "?limit=3&cursor="+url.QueryEscape(p1.next))
	if st != http.StatusOK {
		t.Fatalf("page 2: %d", st)
	}
	if strings.Join(p1.ids, ",") != strings.Join(before[:3], ",") || strings.Join(p2.ids, ",") != strings.Join(before[3:6], ",") {
		t.Fatalf("pages shifted by an insert after page one:\n p1 %v p2 %v\n before %v", p1.ids, p2.ids, before)
	}
	for _, id := range fresh {
		for _, got := range append(append([]string{}, p1.ids...), p2.ids...) {
			if got == id.String() {
				t.Fatalf("a newly raised alert %s appeared inside an older cursor walk", id)
			}
		}
	}
	// A fresh walk sees all of them.
	if got := a.walk(tok, "", 4); len(got) != len(before)+3 {
		t.Fatalf("fresh walk: %d, want %d", len(got), len(before)+3)
	}

	// Real concurrency: a writer raises alerts while the reader pages; every pre-existing
	// alert is served exactly once and nothing is ever served twice.
	existing := a.dbOrder()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			select {
			case <-stop:
				return
			default:
			}
			a.seedAlertsOneTx(tenant, alerting.KindPaymentKillSwitchEngaged, 1)
		}
	}()
	got := a.walk(tok, "", 2)
	close(stop)
	wg.Wait()
	seen := map[string]int{}
	for _, id := range got {
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("alert %s served twice during concurrent inserts", id)
		}
	}
	for _, id := range existing {
		if seen[id] != 1 {
			t.Fatalf("pre-existing alert %s served %d times during concurrent inserts", id, seen[id])
		}
	}
}

// A cursor is not a credential, but a bad one is refused (400, nothing returned), never a 500 and
// never a silently different set.
func TestAlertListS1b_CursorTamperAndFilterBinding(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	tok := a.adminToken()
	a.seedAlertsOneTx(tenant, alerting.KindPaymentKillSwitchEngaged, 5)
	p1, _ := a.alertPage(tok, "?limit=2&state=open")
	if p1.next == "" {
		t.Fatal("setup: expected a cursor")
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	good := p1.next
	filter := "open||"
	wire := func(v int, ts int64, id, f string) string {
		return enc(fmt.Sprintf(`{"v":%d,"t":%d,"i":%q,"f":%q}`, v, ts, id, f))
	}
	ok := uuid.NewString()
	bad := map[string]string{
		"not base64":          "!!!notbase64!!!",
		"padded base64":       good + "==",
		"empty":               "",
		"not json":            enc("nope"),
		"empty object":        enc("{}"),
		"unknown field":       enc(fmt.Sprintf(`{"v":1,"t":1700000000000000,"i":%q,"f":%q,"x":1}`, ok, filter)),
		"trailing json":       enc(fmt.Sprintf(`{"v":1,"t":1700000000000000,"i":%q,"f":%q}{}`, ok, filter)),
		"wrong version":       wire(2, 1700000000000000, ok, filter),
		"time before 2000":    wire(1, 1, ok, filter),
		"time after 2200":     wire(1, 9999999999999999, ok, filter),
		"negative time":       wire(1, -5, ok, filter),
		"id not a uuid":       wire(1, 1700000000000000, "abc", filter),
		"uppercase uuid":      wire(1, 1700000000000000, strings.ToUpper(ok), filter),
		"sql in id":           wire(1, 1700000000000000, "' OR 1=1 --", filter),
		"other filter":        wire(1, 1700000000000000, ok, "resolved||"),
		"oversized":           strings.Repeat("A", 401),
		"filter smuggle":      wire(1, 1700000000000000, ok, filter+"x"),
		"string time":         enc(fmt.Sprintf(`{"v":1,"t":"1700000000000000","i":%q,"f":%q}`, ok, filter)),
		"float time":          enc(fmt.Sprintf(`{"v":1,"t":1.7e15,"i":%q,"f":%q}`, ok, filter)),
		"null id":             enc(fmt.Sprintf(`{"v":1,"t":1700000000000000,"i":null,"f":%q}`, filter)),
		"nil uuid still json": "",
	}
	delete(bad, "nil uuid still json")
	for name, c := range bad {
		r := a.do("GET", "/v1/admin/alerts?state=open&limit=2&cursor="+url.QueryEscape(c), tok, nil)
		if r.status != http.StatusBadRequest || strings.Contains(string(r.body), `"alerts"`) {
			t.Errorf("%s: status %d body %s, want 400 and no data", name, r.status, r.body)
		}
	}
	// A cursor issued for one filter is refused under another (state, severity, kind).
	for _, q := range []string{"?state=acked", "?state=open&severity=p2", "?state=open&kind=payment.kill_switch_engaged", ""} {
		if r := a.do("GET", "/v1/admin/alerts"+q+ifElse(q == "", "?", "&")+"cursor="+url.QueryEscape(good), tok, nil); r.status != http.StatusBadRequest {
			t.Errorf("cursor reused under %q: %d, want 400", q, r.status)
		}
	}
	// Repeated cursor parameters are refused rather than first-wins.
	if r := a.do("GET", "/v1/admin/alerts?state=open&cursor="+url.QueryEscape(good)+"&cursor="+url.QueryEscape(good), tok, nil); r.status != http.StatusBadRequest {
		t.Errorf("duplicate cursor params: %d", r.status)
	}
	// The untouched cursor still works under its own filter.
	if _, st := a.alertPage(tok, "?state=open&limit=2&cursor="+url.QueryEscape(good)); st != http.StatusOK {
		t.Fatalf("valid cursor refused: %d", st)
	}
	// A forged but well-formed cursor is just a position in the caller's own readable set: it
	// returns rows of that set only (here, nothing older than year 2000 exists).
	forged := wire(1, 946684800000000, ok, filter)
	if p, st := a.alertPage(tok, "?state=open&cursor="+url.QueryEscape(forged)); st != http.StatusOK || len(p.ids) != 0 {
		t.Fatalf("forged early position: %d %v", st, p.ids)
	}
	// The limit validation is unchanged.
	for _, l := range []string{"0", "201", "x", "-1"} {
		if r := a.do("GET", "/v1/admin/alerts?limit="+l, tok, nil); r.status != http.StatusBadRequest {
			t.Errorf("limit %s: %d", l, r.status)
		}
	}
}

func ifElse(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// The list returns acked_at/resolved_at and the acting staff ids, forced from the validated
// sessions of two different platform admins; an open alert carries none of the four keys, and an
// ack-less resolve records the resolver as the acker too (the database guard).
func TestAlertListS1b_ActedByAndTimestamps_AuditedActors(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	second := a.w.SeedAdmin()
	tokA, tokB := a.adminToken(), a.token(second, uuid.Nil, auth.RolePlatformAdmin)
	open := a.w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	acked := a.w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	both := a.w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	direct := a.w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())

	if r := a.do("POST", "/v1/admin/alerts/"+acked.String()+"/ack", tokA, nil); r.status != http.StatusOK {
		t.Fatalf("ack: %d %s", r.status, r.body)
	}
	if r := a.do("POST", "/v1/admin/alerts/"+both.String()+"/ack", tokA, nil); r.status != http.StatusOK {
		t.Fatalf("ack: %d %s", r.status, r.body)
	}
	if r := a.do("POST", "/v1/admin/alerts/"+both.String()+"/resolve", tokB, map[string]any{"reason_code": "fixed"}); r.status != http.StatusOK {
		t.Fatalf("resolve: %d %s", r.status, r.body)
	}
	if r := a.do("POST", "/v1/admin/alerts/"+direct.String()+"/resolve", tokB, map[string]any{"reason_code": "fixed"}); r.status != http.StatusOK {
		t.Fatalf("resolve: %d %s", r.status, r.body)
	}

	byID := map[string]map[string]any{}
	p, _ := a.alertPage(tokA, "?limit=50")
	for _, it := range p.raw["alerts"].([]any) {
		m := it.(map[string]any)
		byID[m["id"].(string)] = m
	}
	o := byID[open.String()]
	for _, k := range []string{"acked_at", "acked_by", "resolved_at", "resolved_by"} {
		if _, present := o[k]; present {
			t.Errorf("open alert must not carry %s: %v", k, o)
		}
	}
	ak := byID[acked.String()]
	if ak["state"] != "acked" || ak["acked_by"] != a.admin.String() || ak["acked_at"] == nil {
		t.Errorf("acked alert: %v", ak)
	}
	if _, present := ak["resolved_by"]; present {
		t.Errorf("acked alert must not carry resolved_by: %v", ak)
	}
	bo := byID[both.String()]
	if bo["state"] != "resolved" || bo["acked_by"] != a.admin.String() || bo["resolved_by"] != second.String() || bo["acked_at"] == nil || bo["resolved_at"] == nil {
		t.Errorf("acked-then-resolved alert: %v", bo)
	}
	di := byID[direct.String()]
	if di["acked_by"] != second.String() || di["resolved_by"] != second.String() {
		t.Errorf("resolve of an open alert: the guard records the resolver as acker too: %v", di)
	}

	// The same shape from the single read, and the shown actors equal the audited actors.
	one := a.do("GET", "/v1/admin/alerts/"+both.String(), tokA, nil)
	if one.status != http.StatusOK {
		t.Fatalf("get: %d %s", one.status, one.body)
	}
	got := one.json()["alert"].(map[string]any)
	if got["acked_by"] != a.admin.String() || got["resolved_by"] != second.String() {
		t.Errorf("single read: %v", got)
	}
	for action, want := range map[string]string{"alerts.ack": a.admin.String(), "alerts.resolve": second.String()} {
		found := false
		for _, au := range a.platformAudits(action) {
			if au["_actor"] == want && au["_outcome"] == "success" {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s audit row for %s", action, want)
		}
	}
	// The filter views agree with the states.
	for state, n := range map[string]int{"open": 1, "acked": 1, "resolved": 2} {
		pg, _ := a.alertPage(tokA, "?state="+state)
		if len(pg.ids) != n {
			t.Errorf("state %s: %d rows, want %d", state, len(pg.ids), n)
		}
	}
}

// Tenant isolation and caller classes: only a platform admin session reaches the list, the cursor
// path or the single read. Tenant staff of the alert's own subject tenant, of another tenant, other
// platform-scope roles, and anonymous callers are refused with no alert data and no staff id.
func TestAlertListS1b_TenantAndRoleMatrix_NoLeak(t *testing.T) {
	a := newARAPI(t, false)
	tenantA := a.w.SeedTenant(a.admin)
	tenantB := a.w.SeedTenant(a.admin)
	id := a.w.SeedAlert(tenantA, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	a.seedAlertsOneTx(tenantA, alerting.KindReconciliationLedgerProjectionDrift, 3)
	adminTok := a.adminToken()
	if r := a.do("POST", "/v1/admin/alerts/"+id.String()+"/ack", adminTok, nil); r.status != http.StatusOK {
		t.Fatalf("ack: %d", r.status)
	}
	p1, _ := a.alertPage(adminTok, "?limit=2")

	paths := []string{
		"/v1/admin/alerts", "/v1/admin/alerts?limit=2&cursor=" + url.QueryEscape(p1.next),
		"/v1/admin/alerts?kind=payment.kill_switch_engaged", "/v1/admin/alerts/" + id.String(),
	}
	type caller struct{ name, tok string }
	var callers []caller
	for _, ten := range []uuid.UUID{tenantA, tenantB} {
		for _, role := range []auth.Role{auth.RoleTenantAdmin, auth.RoleCompliance, auth.RoleFinance, auth.RoleSupport, auth.RoleRiskManager, auth.RolePromotionsManager, auth.RoleBonusOperations} {
			callers = append(callers, caller{fmt.Sprintf("%s@%s", role, ten), a.token(a.tenantStaff(ten, string(role)), ten, role)})
		}
	}
	callers = append(callers, caller{"anonymous", ""})
	for _, c := range callers {
		for _, p := range paths {
			r := a.do("GET", p, c.tok, nil)
			want := http.StatusForbidden
			if c.tok == "" {
				want = http.StatusUnauthorized
			}
			if r.status != want {
				t.Errorf("%s GET %s: %d, want %d", c.name, p, r.status, want)
			}
			body := string(r.body)
			if strings.Contains(body, a.admin.String()) || strings.Contains(body, id.String()) || strings.Contains(body, `"alerts"`) || strings.Contains(body, "acked_by") {
				t.Errorf("%s GET %s leaked alert data: %s", c.name, p, body)
			}
		}
	}
	// A tenant caller cannot probe existence either: the same 403 for a real and a random id.
	tok := a.token(a.tenantStaff(tenantA, "tenant_admin"), tenantA, auth.RoleTenantAdmin)
	real, random := a.do("GET", "/v1/admin/alerts/"+id.String(), tok, nil), a.do("GET", "/v1/admin/alerts/"+uuid.NewString(), tok, nil)
	if real.status != random.status || string(real.body) == "" {
		t.Errorf("existence oracle: %d vs %d", real.status, random.status)
	}
	// And no read of this surface writes anything or changes a state.
	if pg, _ := a.alertPage(adminTok, "?state=open"); len(pg.ids) != 3 {
		t.Errorf("open alerts after the refused reads: %d", len(pg.ids))
	}
}

// Single read and the kind filter.
func TestAlertListS1b_SingleReadAndKindFilter(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	tok := a.adminToken()
	ks := a.seedAlertsOneTx(tenant, alerting.KindPaymentKillSwitchEngaged, 3)
	a.seedAlertsOneTx(tenant, alerting.KindReconciliationLedgerProjectionDrift, 2)

	got := a.walk(tok, "&kind=payment.kill_switch_engaged", 2)
	if len(got) != 3 {
		t.Fatalf("kind filter: %d rows, want 3", len(got))
	}
	want := map[string]bool{}
	for _, id := range ks {
		want[id.String()] = true
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("kind filter returned %s of another kind", id)
		}
	}
	if r := a.do("GET", "/v1/admin/alerts?kind=no.such_kind", tok, nil); r.status != http.StatusBadRequest {
		t.Errorf("unknown kind: %d", r.status)
	}
	if r := a.do("GET", "/v1/admin/alerts?kind=%27%3B%20DROP", tok, nil); r.status != http.StatusBadRequest {
		t.Errorf("hostile kind: %d", r.status)
	}
	r := a.do("GET", "/v1/admin/alerts/"+ks[0].String(), tok, nil)
	if r.status != http.StatusOK {
		t.Fatalf("get: %d %s", r.status, r.body)
	}
	it := r.json()["alert"].(map[string]any)
	if it["id"] != ks[0].String() || it["kind"] != string(alerting.KindPaymentKillSwitchEngaged) || it["delivery_state"] == nil {
		t.Errorf("single read: %v", it)
	}
	if r := a.do("GET", "/v1/admin/alerts/"+uuid.NewString(), tok, nil); r.status != http.StatusNotFound {
		t.Errorf("unknown id: %d", r.status)
	}
	if r := a.do("GET", "/v1/admin/alerts/not-a-uuid", tok, nil); r.status != http.StatusBadRequest {
		t.Errorf("malformed id: %d", r.status)
	}
}

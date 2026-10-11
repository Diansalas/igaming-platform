//go:build integration

// ADR 0112 slice 6 (launch API completions), through the real HTTP stack as igaming_runtime:
// compliance read access, queue names + keyset paging, per-caller can_approve, launch_kind on list
// rows. Requires TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL (a skip is not evidence).
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

type s6Item struct {
	ID         string  `json:"id"`
	TenantID   string  `json:"tenant_id"`
	Status     string  `json:"status"`
	BrandID    *string `json:"brand_id"`
	TenantName *string `json:"tenant_name"`
	BrandName  *string `json:"brand_name"`
	CanApprove *bool   `json:"can_approve"`
	Reason     string  `json:"cannot_approve_reason"`
	CreatedAt  string  `json:"created_at"`
}

type s6Page struct {
	Items      []s6Item `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

func (w *lhWorld) s6Queue(tok, query string) (cgResp, s6Page) {
	w.t.Helper()
	r := w.do("GET", "/v1/admin/launch-authorisations"+query, tok, nil)
	var p s6Page
	if r.status == http.StatusOK {
		r.decode(w.t, &p)
	}
	return r, p
}

// s6Expected is the queue order as the database defines it, read by the owner.
func (w *lhWorld) s6Expected() []string {
	w.t.Helper()
	var ids []string
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM launch_authorisation_requests ORDER BY created_at DESC, id DESC`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return ids
}

// a brand activation request filed by the platform requester on a fresh pending brand.
func (w *lhWorld) s6BrandRequest(tenantID uuid.UUID, jid uuid.UUID) (brand uuid.UUID, id string) {
	w.t.Helper()
	b := w.pendingBrand(tenantID)
	acks := w.nonPassing(tenantID, &b, "activate", &jid)
	c := w.createRequest(w.tokReq(), tenantID, lhActivationBody(&b, jid, acks))
	return b, c.Request.ID
}

func TestLaunchHTTP6_Queue_NamesLimitCursor_StableUnderConcurrentInserts(t *testing.T) {
	w := newLHWorld(t)
	a := w.activeTenant()
	jid := w.jurisdiction(a)
	brand1, _ := w.s6BrandRequest(a, jid)
	brand2, _ := w.s6BrandRequest(a, jid)
	pt := w.pendingTenant()
	ptj := w.jurisdiction(pt)
	tenantReq := w.createRequest(w.tokReq(), pt, lhActivationBody(nil, ptj, w.nonPassing(pt, nil, "activate", &ptj)))

	ap := w.tokAp1()

	// Bounded, validated parameters (400, never a clamp or a 500).
	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=201", "?limit=500", "?limit=abc", "?limit=1.5", "?cursor=", "?cursor=garbage", "?cursor=" + url.QueryEscape("v1.1.1"), "?status=nope"} {
		if r, _ := w.s6Queue(ap, q); r.status != http.StatusBadRequest {
			t.Errorf("queue %s: want 400, got %d %s", q, r.status, r.body)
		}
	}
	if r, p := w.s6Queue(ap, "?limit=200"); r.status != http.StatusOK || len(p.Items) > 200 {
		t.Errorf("limit=200: %d, %d items", r.status, len(p.Items))
	}
	if r, p := w.s6Queue(ap, "?limit=1"); r.status != http.StatusOK || len(p.Items) != 1 || p.NextCursor == nil {
		t.Errorf("limit=1 must return exactly one row and a next cursor: %d %s", r.status, r.body)
	}

	expected := w.s6Expected()
	n := len(expected)
	if n < 3 {
		t.Fatalf("fixture: expected at least 3 requests, have %d", n)
	}
	// Default page size is 50.
	if _, p := w.s6Queue(ap, ""); len(p.Items) != min(n, 50) || (n > 50) != (p.NextCursor != nil) {
		t.Errorf("default page: %d items for %d requests, cursor=%v", len(p.Items), n, p.NextCursor)
	}

	limit := max(2, n/3)
	if limit > 200 {
		limit = 200
	}
	_, p1 := w.s6Queue(ap, "?limit="+strconv.Itoa(limit))
	if len(p1.Items) != limit || p1.NextCursor == nil {
		t.Fatalf("first page: %d items (limit %d), cursor %v", len(p1.Items), limit, p1.NextCursor)
	}
	got := []string{}
	byID := map[string]s6Item{}
	take := func(p s6Page) {
		for _, it := range p.Items {
			got = append(got, it.ID)
			byID[it.ID] = it
		}
	}
	take(p1)

	// Concurrent inserts between page 1 and page 2: a new request must not shift, duplicate or drop
	// any row of the remaining pages (it sorts before the first page).
	_, newID := w.s6BrandRequest(a, jid)
	next := p1.NextCursor
	for pages := 0; next != nil; pages++ {
		if pages > n+2 {
			t.Fatal("cursor loop does not terminate")
		}
		r, p := w.s6Queue(ap, "?limit="+strconv.Itoa(limit)+"&cursor="+url.QueryEscape(*next))
		if r.status != http.StatusOK {
			t.Fatalf("page: %d %s", r.status, r.body)
		}
		if len(p.Items) > limit {
			t.Fatalf("page larger than limit: %d", len(p.Items))
		}
		if p.NextCursor != nil && len(p.Items) != limit {
			t.Fatalf("a non-last page must be full: %d of %d", len(p.Items), limit)
		}
		take(p)
		next = p.NextCursor
	}
	if len(got) != n {
		t.Fatalf("walk returned %d rows, snapshot had %d (new request %s)", len(got), n, newID)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("row %d: got %s, want %s (deterministic (created_at,id) DESC order)", i, got[i], expected[i])
		}
		if got[i] == newID {
			t.Fatalf("the concurrently inserted request leaked into a later page")
		}
	}
	// A fresh first page now starts with the new request.
	if _, fresh := w.s6Queue(ap, "?limit=1"); len(fresh.Items) != 1 || fresh.Items[0].ID != newID {
		t.Fatalf("fresh first page must start with the new request: %+v", fresh.Items)
	}

	// Names: tenant_name and brand_name (display), brand rows only for brand subjects.
	for _, c := range []struct {
		id         string
		tenantName string
		brandName  *string
	}{
		{tenantReq.Request.ID, "lh pending", nil},
	} {
		it, ok := byID[c.id]
		if !ok || it.TenantName == nil || *it.TenantName != c.tenantName || it.BrandName != nil {
			t.Errorf("tenant-subject row: %+v", it)
		}
	}
	named := 0
	for _, it := range byID {
		if it.BrandID != nil && (it.TenantID == a.String()) {
			if it.TenantName == nil || *it.TenantName != "lh" || it.BrandName == nil || *it.BrandName != "lh pending brand" {
				t.Errorf("brand-subject row names: %+v", it)
			}
			named++
		}
	}
	if named < 2 {
		t.Errorf("expected the brand rows of tenant A, saw %d (brands %s %s)", named, brand1, brand2)
	}

	// A status filter composes with the cursor and keeps the database order.
	pos := map[string]int{}
	for i, id := range expected {
		pos[id] = i
	}
	last := -1
	cur := ""
	for pages := 0; pages < n+2; pages++ {
		q := "?status=pending&limit=" + strconv.Itoa(max(1, limit/2))
		if cur != "" {
			q += "&cursor=" + url.QueryEscape(cur)
		}
		r, p := w.s6Queue(ap, q)
		if r.status != http.StatusOK {
			t.Fatalf("filtered page: %d %s", r.status, r.body)
		}
		for _, it := range p.Items {
			if it.Status != "pending" {
				t.Fatalf("status filter leaked %+v", it)
			}
			at, known := pos[it.ID]
			if known && at <= last {
				t.Fatalf("filtered walk left the database order at %s", it.ID)
			}
			if known {
				last = at
			}
		}
		if p.NextCursor == nil {
			break
		}
		cur = *p.NextCursor
	}

	// Platform only; anonymous is 401; a tenant cursor-bearing call is still 403.
	ta := w.staff(a, identity.StaffRoleTenantAdmin)
	co := w.staff(a, identity.StaffRoleCompliance)
	for name, tok := range map[string]string{"tenant_admin": w.tenantTok(a, ta, auth.RoleTenantAdmin), "compliance": w.tenantTok(a, co, auth.RoleCompliance)} {
		if r, _ := w.s6Queue(tok, "?limit=1"); r.status != http.StatusForbidden {
			t.Errorf("%s queue: want 403, got %d %s", name, r.status, r.body)
		}
	}
	if r, _ := w.s6Queue("", ""); r.status != http.StatusUnauthorized {
		t.Errorf("anonymous queue: %d", r.status)
	}
}

// can_approve is computed for the CALLER, mirrors Decide's refusals, never names anyone, and the
// server stays authoritative (every "false" is also refused by the decision route).
func TestLaunchHTTP6_CanApprove_PerCaller_MirrorsDecide(t *testing.T) {
	w := newLHWorld(t)
	a := w.activeTenant()
	jid := w.jurisdiction(a)
	other := w.extraPlatformStaff()
	tokOther := w.api.token(other, uuid.Nil, auth.RolePlatformAdmin)

	// A second account of the REQUESTER's Person.
	var reqPerson uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, w.ops.Requester).Scan(&reqPerson)
	}); err != nil {
		t.Fatal(err)
	}
	twin := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!lh-not-a-login', 'platform_admin', $3)`, twin, "lh-twin-"+twin.String()+"@fixture.invalid", reqPerson)
		return err
	}); err != nil {
		t.Fatalf("seed twin: %v", err)
	}
	tokTwin := w.api.token(twin, uuid.Nil, auth.RolePlatformAdmin)

	state := func(tok, tenantID, id string) (bool, string) {
		t.Helper()
		var it struct {
			Request s6Item `json:"request"`
		}
		r := w.do("GET", "/v1/admin/tenants/"+tenantID+"/launch-authorisations/"+id, tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("detail: %d %s", r.status, r.body)
		}
		r.decode(t, &it)
		if it.Request.CanApprove == nil {
			t.Fatalf("a read must carry can_approve: %s", r.body)
		}
		// The queue row says the same as the detail.
		qr, q := w.s6Queue(tok, "?limit=200")
		if qr.status != http.StatusForbidden { // tenant callers have no queue
			found := false
			for _, row := range q.Items {
				if row.ID != id {
					continue
				}
				found = true
				if row.CanApprove == nil || *row.CanApprove != *it.Request.CanApprove || row.Reason != it.Request.Reason {
					t.Fatalf("queue row %+v disagrees with detail %+v", row, it.Request)
				}
			}
			if !found {
				t.Fatalf("request %s missing from the newest 200 queue rows (status %d): the agreement check must not be skipped", id, qr.status)
			}
		}
		return *it.Request.CanApprove, it.Request.Reason
	}
	want := func(name string, tok string, tenantID uuid.UUID, id string, can bool, reason string) {
		t.Helper()
		if c, r := state(tok, tenantID.String(), id); c != can || r != reason {
			t.Errorf("%s: can_approve=%v reason=%q, want %v %q", name, c, r, can, reason)
		}
	}

	// Tenant activation: two approvals needed.
	pt := w.pendingTenant()
	pj := w.jurisdiction(pt)
	c := w.createRequest(w.tokReq(), pt, lhActivationBody(nil, pj, w.nonPassing(pt, nil, "activate", &pj)))
	rid := c.Request.ID
	want("requester", w.tokReq(), pt, rid, false, launchgov.CannotSelfApproval)
	want("requester's other account", tokTwin, pt, rid, false, launchgov.CannotSamePerson)
	want("approver 1", w.tokAp1(), pt, rid, true, "")
	want("approver 2", w.tokAp2(), pt, rid, true, "")
	// The server agrees with every "false" (stable 409) ...
	if r := w.approve(tokTwin, pt, rid); r.status != http.StatusConflict || r.code(t) != "LAUNCH_SELF_APPROVAL" {
		t.Errorf("twin approve: %d %s", r.status, r.body)
	}
	if r := w.approve(w.tokReq(), pt, rid); r.status != http.StatusConflict || r.code(t) != "LAUNCH_SELF_APPROVAL" {
		t.Errorf("requester approve: %d %s", r.status, r.body)
	}
	// ... and the answer is a code: no staff id of anybody appears in a not-approvable answer.
	r := w.do("GET", "/v1/admin/tenants/"+pt.String()+"/launch-authorisations/"+rid, tokTwin, nil)
	for _, id := range []uuid.UUID{twin, w.ops.Approver1, w.ops.Approver2, other, reqPerson} {
		if id != w.ops.Requester && strings.Contains(string(r.body), id.String()) {
			t.Errorf("response leaks %s", id)
		}
	}
	// First approval -> that approver's Person is "already_decided"; the other approver still may.
	if first := w.approve(w.tokAp1(), pt, rid); first.status != http.StatusOK {
		t.Fatalf("first approval: %d %s", first.status, first.body)
	}
	want("approver 1 after approving", w.tokAp1(), pt, rid, false, launchgov.CannotAlreadyDecided)
	want("approver 2 still", w.tokAp2(), pt, rid, true, "")
	want("a third platform admin", tokOther, pt, rid, true, "")
	if r := w.approve(w.tokAp1(), pt, rid); r.status != http.StatusConflict || r.code(t) != "LAUNCH_ALREADY_DECIDED" {
		t.Errorf("approver 1 again: %d %s", r.status, r.body)
	}
	if second := w.approve(w.tokAp2(), pt, rid); second.status != http.StatusOK {
		t.Fatalf("second approval: %d %s", second.status, second.body)
	}
	want("after execution", w.tokAp1(), pt, rid, false, launchgov.CannotNotPending)
	want("after execution, requester", w.tokReq(), pt, rid, false, launchgov.CannotNotPending)

	// An earlier APPROVAL by my Person is not a prior reject: after ap1 approved (1 of 2) and the
	// requester cancelled, a variant without a note is approvable by ap1 again.
	pt2 := w.pendingTenant()
	pj2 := w.jurisdiction(pt2)
	first2 := w.createRequest(w.tokReq(), pt2, lhActivationBody(nil, pj2, w.nonPassing(pt2, nil, "activate", &pj2)))
	if r := w.approve(w.tokAp1(), pt2, first2.Request.ID); r.status != http.StatusOK {
		t.Fatalf("approve first variant: %d %s", r.status, r.body)
	}
	if r := w.do("POST", lhBase(pt2)+"/"+first2.Request.ID+"/cancel", w.tokReq(), map[string]any{"reason_code": "requester_withdrew"}); r.status != http.StatusOK {
		t.Fatalf("cancel first variant: %d %s", r.status, r.body)
	}
	noNote := lhActivationBody(nil, pj2, w.nonPassing(pt2, nil, "activate", &pj2))
	noNote["conditions_note"] = ""
	second2 := w.createRequest(w.tokReq(), pt2, noNote)
	want("approver who only APPROVED an earlier variant", w.tokAp1(), pt2, second2.Request.ID, true, "")

	// Tenant staff are never approvers (listing and detail), whatever the request says.
	b, brid := w.s6BrandRequest(a, jid)
	ta := w.staff(a, identity.StaffRoleTenantAdmin)
	co := w.staff(a, identity.StaffRoleCompliance)
	for name, tok := range map[string]string{"tenant_admin": w.tenantTok(a, ta, auth.RoleTenantAdmin), "compliance": w.tenantTok(a, co, auth.RoleCompliance)} {
		want(name, tok, a, brid, false, launchgov.CannotNotPlatformApprover)
		lr := w.do("GET", lhBase(a), tok, nil)
		var l s6Page
		lr.decode(t, &l)
		if lr.status != http.StatusOK || len(l.Items) == 0 {
			t.Fatalf("%s list: %d %s", name, lr.status, lr.body)
		}
		for _, it := range l.Items {
			if it.CanApprove == nil || *it.CanApprove || it.Reason != launchgov.CannotNotPlatformApprover {
				t.Errorf("%s list row: %+v", name, it)
			}
			if it.TenantName != nil || it.BrandName != nil {
				t.Errorf("per-tenant list rows carry no names: %+v", it)
			}
		}
	}

	// prior reject without a change note: ap1 rejects a first variant, then a variant without a
	// note is not approvable by ap1's Person (LAUNCH_PRIOR_REJECTER), but is by ap2.
	w.decide(w.tokAp1(), a, brid, "reject", "policy_concern", "")
	want("rejected request", w.tokAp2(), a, brid, false, launchgov.CannotNotPending)
	bodyNoNote := lhActivationBody(&b, jid, w.nonPassing(a, &b, "activate", &jid))
	bodyNoNote["conditions_note"] = ""
	v := w.createRequest(w.tokReq(), a, bodyNoNote)
	want("prior rejecter, no note", w.tokAp1(), a, v.Request.ID, false, launchgov.CannotPriorReject)
	want("other approver, no note", w.tokAp2(), a, v.Request.ID, true, "")
	if r := w.approve(w.tokAp1(), a, v.Request.ID); r.status != http.StatusConflict || r.code(t) != "LAUNCH_PRIOR_REJECTER" {
		t.Errorf("prior rejecter approve: %d %s", r.status, r.body)
	}
	if r := w.do("POST", lhBase(a)+"/"+v.Request.ID+"/cancel", w.tokReq(), map[string]any{"reason_code": "requester_withdrew"}); r.status != http.StatusOK {
		t.Fatalf("cancel variant: %d %s", r.status, r.body)
	}
	bodyNote := lhActivationBody(&b, jid, w.nonPassing(a, &b, "activate", &jid))
	bodyNote["conditions_note"] = "licence reference corrected after the rejection"
	v2 := w.createRequest(w.tokReq(), a, bodyNote)
	want("prior rejecter, with a change note", w.tokAp1(), a, v2.Request.ID, true, "")

	// Expired: a service clock past the TTL reports the pending request as expired.
	late := w.service(launchgov.Config{Clock: func() time.Time { return time.Now().Add(launchgov.RequestTTL + time.Hour) }})
	w.serve(late)
	want("expired", w.tokAp2(), a, v2.Request.ID, false, launchgov.CannotExpired)
	_, q := w.s6Queue(w.tokAp2(), "?status=expired&limit=200")
	found := false
	for _, it := range q.Items {
		if it.ID == v2.Request.ID {
			found = it.Status == "expired" && it.CanApprove != nil && !*it.CanApprove && it.Reason == launchgov.CannotExpired
		}
	}
	if !found {
		t.Errorf("the expired request must appear in the expired queue with reason expired")
	}
}

// compliance gains own-tenant tenant/brand read (brand discovery) and nothing more; foreign
// tenants are refused; support / finance still cannot read; writes stay refused.
func TestLaunchHTTP6_Compliance_TenantBrandRead_Authz(t *testing.T) {
	w := newLHWorld(t)
	a, b := w.activeTenant(), w.activeTenant()
	brandA := w.pendingBrand(a)
	brandB := w.pendingBrand(b)
	co := w.staff(a, identity.StaffRoleCompliance)
	ta := w.staff(a, identity.StaffRoleTenantAdmin)
	sup := w.staff(a, identity.StaffRoleSupport)
	fin := w.staff(a, identity.StaffRoleFinance)
	tokCo, tokTA := w.tenantTok(a, co, auth.RoleCompliance), w.tenantTok(a, ta, auth.RoleTenantAdmin)
	tokSup, tokFin := w.tenantTok(a, sup, auth.RoleSupport), w.tenantTok(a, fin, auth.RoleFinance)

	ta1 := "/v1/admin/tenants/" + a.String()
	tb := "/v1/admin/tenants/" + b.String()
	for name, tok := range map[string]string{"compliance": tokCo, "tenant_admin": tokTA} {
		for _, p := range []string{ta1, ta1 + "/brands", ta1 + "/brands/" + brandA.String()} {
			if r := w.do("GET", p, tok, nil); r.status != http.StatusOK {
				t.Errorf("%s GET %s: want 200, got %d %s", name, p, r.status, r.body)
			}
		}
		// Brand discovery returns id, name, slug, status of the tenant's own brands only.
		var l struct {
			Items []struct {
				ID         string  `json:"id"`
				TenantID   string  `json:"tenant_id"`
				Name       string  `json:"name"`
				Slug       string  `json:"slug"`
				Status     string  `json:"status"`
				LaunchKind *string `json:"launch_kind"`
			} `json:"items"`
		}
		w.do("GET", ta1+"/brands", tok, nil).decode(t, &l)
		seen := false
		for _, it := range l.Items {
			if it.TenantID != a.String() {
				t.Errorf("%s brand list leaks another tenant: %+v", name, it)
			}
			if strings.EqualFold(it.ID, brandA.String()) {
				seen = it.Name != "" && it.Slug != "" && it.Status == "pending_launch"
			}
		}
		if !seen {
			t.Errorf("%s: the tenant's brand must be discoverable with name, slug and status", name)
		}
		// Foreign tenant: refused, no data.
		for _, p := range []string{tb, tb + "/brands", tb + "/brands/" + brandB.String()} {
			if r := w.do("GET", p, tok, nil); r.status != http.StatusForbidden && r.status != http.StatusNotFound {
				t.Errorf("%s GET foreign %s: want 403/404, got %d %s", name, p, r.status, r.body)
			}
		}
		// Own brand id under the foreign tenant path, and the foreign brand under the own path: 403 / 404.
		if r := w.do("GET", ta1+"/brands/"+brandB.String(), tok, nil); r.status != http.StatusNotFound {
			t.Errorf("%s GET foreign brand under own tenant: want 404, got %d", name, r.status)
		}
		// The platform-only tenant list stays platform only.
		if r := w.do("GET", "/v1/admin/tenants", tok, nil); r.status != http.StatusForbidden {
			t.Errorf("%s tenant list: want 403, got %d", name, r.status)
		}
	}
	for name, tok := range map[string]string{"support": tokSup, "finance": tokFin} {
		for _, p := range []string{ta1, ta1 + "/brands", ta1 + "/brands/" + brandA.String()} {
			if r := w.do("GET", p, tok, nil); r.status != http.StatusForbidden {
				t.Errorf("%s GET %s: want 403, got %d", name, p, r.status)
			}
		}
	}

	// No escalation: compliance cannot write tenants/brands/staff, approve, or act on the tenant subject.
	if r := w.do("POST", ta1+"/brands", tokCo, map[string]any{"name": "x", "slug": "x-" + uuid.NewString()}); r.status != http.StatusForbidden {
		t.Errorf("compliance create brand: want 403, got %d", r.status)
	}
	if r := w.do("POST", "/v1/admin/tenants", tokCo, map[string]any{"name": "x", "slug": "x", "licensing_model": "own_licence", "reason_code": "x"}); r.status != http.StatusForbidden {
		t.Errorf("compliance create tenant: want 403, got %d", r.status)
	}
	for _, op := range []string{"approve", "reject"} {
		if r := w.do("POST", lhBase(a)+"/"+uuid.NewString()+"/"+op, tokCo, map[string]any{"reason_code": "reviewed_ok", "seen_readiness_hash": strings.Repeat("a", 64)}); r.status != http.StatusForbidden {
			t.Errorf("compliance %s: want 403, got %d", op, r.status)
		}
	}
	if r := w.do("POST", ta1+"/suspend", tokCo, map[string]any{"reason_code": "compliance_hold"}); r.status != http.StatusForbidden {
		t.Errorf("compliance suspend tenant: want 403, got %d", r.status)
	}
	// Cross-tenant launch reads with compliance's own token: the same 404 as an unknown tenant.
	for _, p := range []string{tb + "/launch-authorisations", tb + "/launch-status-history", tb + "/launch-readiness?action=activate&brand_id=" + brandB.String()} {
		if r := w.do("GET", p, tokCo, nil); r.status != http.StatusNotFound {
			t.Errorf("compliance foreign launch read %s: want 404, got %d %s", p, r.status, r.body)
		}
	}
	// Tenant-view redactions are preserved: a platform requester's staff id is never shown to tenant staff.
	jid := w.jurisdiction(a)
	_, rid := w.s6BrandRequest(a, jid)
	for _, p := range []string{lhBase(a), lhBase(a) + "/" + rid} {
		r := w.do("GET", p, tokCo, nil)
		if r.status != http.StatusOK || strings.Contains(string(r.body), w.ops.Requester.String()) || strings.Contains(string(r.body), `"environment":"`+"test") {
			t.Errorf("tenant view of %s leaks platform detail: %d %s", p, r.status, r.body)
		}
	}
}

// launch_kind: the kind of the newest transition on list rows; null when none; platform tenant list
// and the brand list agree with the database; a tenant caller sees only its own brands' kinds.
func TestLaunchHTTP6_ListRows_LaunchKind(t *testing.T) {
	w := newLHWorld(t)
	a := w.activeTenant() // owner_provisioned
	activeBrand := w.activeBrand(a)
	pendBrand := w.pendingBrand(a)
	jid := w.jurisdiction(a)
	// A governed activation: approve a brand request to completion.
	govBrand, rid := w.s6BrandRequest(a, jid)
	if r := w.approve(w.tokAp1(), a, rid); r.status != http.StatusOK {
		t.Fatalf("approve: %d %s", r.status, r.body)
	}
	if got := w.status(a, &govBrand); got != "active" {
		t.Fatalf("brand status %s", got)
	}

	dbKind := func(tenantID uuid.UUID, brandID *uuid.UUID) *string {
		var k *string
		if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `SELECT kind FROM launch_status_transitions WHERE tenant_id = $1 AND subject_kind = $2
				AND brand_id IS NOT DISTINCT FROM $3 ORDER BY created_at DESC, id DESC LIMIT 1`, tenantID, map[bool]string{true: "brand", false: "tenant"}[brandID != nil], brandID).Scan(&k)
			if err == pgx.ErrNoRows {
				k = nil
				return nil
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return k
	}
	// Newest, not oldest: suspend the owner-provisioned brand and tenant; the newest transition is governed.
	suspBrand := w.activeBrand(a)
	suspTenant := w.activeTenant()
	if r := w.do("POST", "/v1/admin/tenants/"+a.String()+"/brands/"+suspBrand.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusOK {
		t.Fatalf("suspend brand: %d %s", r.status, r.body)
	}
	if r := w.do("POST", "/v1/admin/tenants/"+suspTenant.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusOK {
		t.Fatalf("suspend tenant: %d %s", r.status, r.body)
	}
	type row struct {
		ID         string  `json:"id"`
		LaunchKind *string `json:"launch_kind"`
	}
	type page struct {
		Items []row `json:"items"`
	}
	same := func(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

	ta := w.staff(a, identity.StaffRoleTenantAdmin)
	co := w.staff(a, identity.StaffRoleCompliance)
	for name, tok := range map[string]string{"platform": w.tokAp1(), "tenant_admin": w.tenantTok(a, ta, auth.RoleTenantAdmin), "compliance": w.tenantTok(a, co, auth.RoleCompliance)} {
		r := w.do("GET", "/v1/admin/tenants/"+a.String()+"/brands?limit=200", tok, nil)
		var p page
		r.decode(t, &p)
		if r.status != http.StatusOK {
			t.Fatalf("%s brands: %d %s", name, r.status, r.body)
		}
		kinds := map[string]*string{}
		for _, it := range p.Items {
			kinds[it.ID] = it.LaunchKind
		}
		for _, b := range []uuid.UUID{activeBrand, pendBrand, govBrand, suspBrand} {
			bb := b
			if want := dbKind(a, &bb); !same(kinds[b.String()], want) {
				t.Errorf("%s brand %s launch_kind=%v, db=%v", name, b, kinds[b.String()], want)
			}
		}
		if k := kinds[activeBrand.String()]; k == nil || *k != "owner_provisioned" {
			t.Errorf("%s: owner-provisioned brand kind=%v", name, k)
		}
		if k := kinds[govBrand.String()]; k == nil || *k != "governed" {
			t.Errorf("%s: governed brand kind=%v", name, k)
		}
		if k := kinds[suspBrand.String()]; k == nil || *k != "governed" {
			t.Errorf("%s: a suspended owner-provisioned brand must show the NEWEST kind (governed), got %v", name, k)
		}
		if _, ok := kinds[pendBrand.String()]; !ok {
			t.Errorf("%s: the pending brand must be listed with an explicit (null) launch_kind", name)
		}
		if !strings.Contains(string(r.body), `"launch_kind":null`) && kinds[pendBrand.String()] == nil {
			t.Errorf("%s: launch_kind must be present as null: %s", name, r.body)
		}
	}
	// Tenant list (platform): kinds agree with the database for the tenants we know.
	pt := w.pendingTenant()
	found := 0
	for off := 0; off < 20000 && found < 3; off += 200 {
		r := w.do("GET", "/v1/admin/tenants?limit=200&offset="+strconv.Itoa(off), w.tokAp1(), nil)
		var tp page
		r.decode(t, &tp)
		if r.status != http.StatusOK {
			t.Fatalf("tenant list: %d %s", r.status, r.body)
		}
		if len(tp.Items) > 0 && !strings.Contains(string(r.body), `"launch_kind"`) {
			t.Fatalf("tenant list rows must carry launch_kind: %s", r.body)
		}
		for _, it := range tp.Items {
			for _, id := range []uuid.UUID{a, pt, suspTenant} {
				if it.ID == id.String() {
					found++
					if want := dbKind(id, nil); !same(it.LaunchKind, want) {
						t.Errorf("tenant %s launch_kind=%v, db=%v", id, it.LaunchKind, want)
					}
					if id == suspTenant && (it.LaunchKind == nil || *it.LaunchKind != "governed") {
						t.Errorf("a suspended owner-provisioned tenant must show the NEWEST kind (governed), got %v", it.LaunchKind)
					}
					if id == a && (it.LaunchKind == nil || *it.LaunchKind != "owner_provisioned") {
						t.Errorf("owner-provisioned tenant kind=%v", it.LaunchKind)
					}
				}
			}
		}
		if len(tp.Items) < 200 {
			break
		}
	}
	if found != 3 {
		t.Errorf("all fixture tenants must be found in the tenant list, found %d", found)
	}

	// Detail responses are unchanged in shape (no launch_kind).
	if d := w.do("GET", "/v1/admin/tenants/"+a.String(), w.tokAp1(), nil); strings.Contains(string(d.body), "launch_kind") {
		t.Errorf("tenant detail must not carry launch_kind: %s", d.body)
	}
}

type s6FailPool struct{}

func (s6FailPool) WithPlatformAdmin(context.Context, uuid.UUID, db.TxFunc) error {
	return errors.New("injected lookup failure")
}
func (s6FailPool) WithPrincipalScope(context.Context, uuid.UUID, uuid.UUID, db.TxFunc) error {
	return errors.New("injected lookup failure")
}
func (s6FailPool) WithTenantReadOnly(context.Context, uuid.UUID, db.TxFunc) error {
	return errors.New("injected lookup failure")
}

// A launch_kind lookup failure must not fail the list: the rows come back with launch_kind null.
func TestLaunchHTTP6_ListRows_LaunchKindLookupFailure_RendersNull(t *testing.T) {
	w := newLHWorld(t)
	a := w.activeTenant()
	b := w.activeBrand(a) // would be owner_provisioned if the lookup worked
	failing, err := launchgov.New(launchgov.Config{Pool: s6FailPool{}, Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	w.serve(failing)
	ta := w.staff(a, identity.StaffRoleTenantAdmin)
	for name, tc := range map[string]struct{ path, tok string }{
		"brands":  {"/v1/admin/tenants/" + a.String() + "/brands", w.tenantTok(a, ta, auth.RoleTenantAdmin)},
		"tenants": {"/v1/admin/tenants?limit=200", w.tokAp1()},
	} {
		r := w.do("GET", tc.path, tc.tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("%s list must survive a launch_kind failure: %d %s", name, r.status, r.body)
		}
		if strings.Contains(string(r.body), `"launch_kind":"`) {
			t.Errorf("%s: launch_kind must be null on a failed lookup: %s", name, r.body)
		}
	}
	if r := w.do("GET", "/v1/admin/tenants/"+a.String()+"/brands", w.tenantTok(a, ta, auth.RoleTenantAdmin), nil); !strings.Contains(string(r.body), b.String()) {
		t.Errorf("the brand must still be listed: %s", r.body)
	}
}

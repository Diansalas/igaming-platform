//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// c2: the tenant-scoped, read-only operating bases API through the real HTTP stack as igaming_runtime. The owner pool only seeds
// fixtures (owner-path rows carry a NULL change_request_id, which is irrelevant to a read).

const (
	secNote = "NOTE-SECRET-TEXT"
	secEvid = "EVID-SECRET-1"
	secDet  = "DET-SECRET-1"
	secRev  = "REV-SECRET-1"
	secRsn  = "reason_secret"
)

type tbWorld struct {
	*lhWorld
	jid                         uuid.UUID
	tA, tB                      uuid.UUID
	brandA, brandB              uuid.UUID
	idsA, idsB                  []string
	activeA, withdrawnA, closed string // ids in tenant A
}

func (w *tbWorld) seed(tenantID uuid.UUID, brand *uuid.UUID, country, op, status string, closed bool) string {
	w.t.Helper()
	id := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO operating_bases (id, tenant_id, brand_id, country_code, operation_code, basis_kind, jurisdiction_id,
			determination_reference, determined_by_role, evidence_reference, restrictions_note, last_review_reference, review_due_at, needs_rereview,
			status, reason_code, policy_version, effective_from, effective_to)
			VALUES ($1,$2,$3,$4,$5,'no_licence_documented',$6,$7,'head_of_compliance',$8,$9,$10,current_date + 30,false,$11,$12,'fixture',
			        now() - interval '2 hours', CASE WHEN $13 THEN now() - interval '1 hour' ELSE NULL END)`,
			id, tenantID, brand, country, op, w.jid, secDet, secEvid, secNote, secRev, status, secRsn, closed)
		return err
	}); err != nil {
		w.t.Fatalf("seed basis: %v", err)
	}
	return id.String()
}

func newTBWorld(t *testing.T) *tbWorld {
	t.Helper()
	w := &tbWorld{lhWorld: newLHWorld(t)}
	w.tA, w.tB = w.activeTenant(), w.activeTenant()
	w.jid = w.jurisdiction(w.tA)
	w.brandA, w.brandB = w.activeBrand(w.tA), w.activeBrand(w.tB)
	// Tenant A: an active tenant-wide row, a withdrawn row, a closed (superseded) row, and a brand row. Tenant B: two rows.
	w.activeA = w.seed(w.tA, nil, "MT", "deposit", "active", false)
	w.withdrawnA = w.seed(w.tA, nil, "MT", "registration", "withdrawn", false)
	w.closed = w.seed(w.tA, nil, "DE", "deposit", "active", true)
	brandRow := w.seed(w.tA, &w.brandA, "MT", "wagering", "active", false)
	w.idsA = []string{w.activeA, w.withdrawnA, w.closed, brandRow}
	bRow := w.seed(w.tB, nil, "MT", "deposit", "active", false)
	bBrandRow := w.seed(w.tB, &w.brandB, "FR", "deposit", "active", false)
	w.idsB = []string{bRow, bBrandRow}
	return w
}

func (w *tbWorld) ids(t *testing.T, r cgResp) []string {
	t.Helper()
	var pg struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Total int `json:"total"`
	}
	r.decode(t, &pg)
	out := []string{}
	for _, it := range pg.Items {
		out = append(out, it.ID)
	}
	sort.Strings(out)
	return out
}

func tbSorted(s []string) []string { c := append([]string(nil), s...); sort.Strings(c); return c }

func tbEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const tbList = "/v1/admin/tenant/operating-bases"

func TestC2_TenantOperatingBases_OwnRowsOnly_AndRedacted(t *testing.T) {
	w := newTBWorld(t)
	taA := w.tenantTok(w.tA, w.staff(w.tA, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)
	coA := w.tenantTok(w.tA, w.staff(w.tA, identity.StaffRoleCompliance), auth.RoleCompliance)
	taB := w.tenantTok(w.tB, w.staff(w.tB, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)

	// Authorised roles see exactly their own tenant's rows (open, withdrawn and closed versions).
	for name, tok := range map[string]string{"tenant_admin": taA, "compliance": coA} {
		r := w.do("GET", tbList, tok, nil)
		wantStatus(t, name+" list", r, http.StatusOK, "")
		if got := w.ids(t, r); !tbEq(got, tbSorted(w.idsA)) {
			t.Fatalf("%s: got %v want %v", name, got, tbSorted(w.idsA))
		}
	}
	// Tenant B sees only B's rows, never A's.
	r := w.do("GET", tbList, taB, nil)
	if got := w.ids(t, r); !tbEq(got, tbSorted(w.idsB)) {
		t.Fatalf("tenant B list: got %v want %v", got, tbSorted(w.idsB))
	}

	// Statuses and currency are reported correctly.
	byID := map[string]map[string]any{}
	var pg struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
		Limit int              `json:"limit"`
	}
	w.do("GET", tbList, taA, nil).decode(t, &pg)
	for _, it := range pg.Items {
		byID[it["id"].(string)] = it
	}
	if it := byID[w.activeA]; it["status"] != "active" || it["is_current"] != true || it["effective_to"] != nil || it["has_restrictions_note"] != true {
		t.Errorf("active row: %v", it)
	}
	if it := byID[w.withdrawnA]; it["status"] != "withdrawn" || it["is_current"] != true {
		t.Errorf("withdrawn row: %v", it)
	}
	if it := byID[w.closed]; it["status"] != "active" || it["is_current"] != false || it["effective_to"] == nil {
		t.Errorf("closed (superseded) row: %v", it)
	}
	// current=true drops the closed version only.
	r = w.do("GET", tbList+"?current=true", taA, nil)
	if got := w.ids(t, r); len(got) != 3 || tbContains(got, w.closed) {
		t.Errorf("current=true: %v", got)
	}

	// Golden field set over the wire, and no forbidden value anywhere in list or detail bodies.
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	r = w.do("GET", tbList, taA, nil)
	r.decode(t, &raw)
	for _, it := range raw.Items {
		var keys []string
		for k := range it {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !tbEq(keys, tenantOperatingBasisGoldenFields) {
			t.Fatalf("wire field set drifted: %v", keys)
		}
	}
	d := w.do("GET", tbList+"/"+w.activeA, taA, nil)
	wantStatus(t, "detail own", d, http.StatusOK, "")
	for _, body := range []string{string(r.body), string(d.body)} {
		for _, forbidden := range []string{secNote, secEvid, secDet, secRev, secRsn, w.tA.String(), w.jid.String(), "head_of_compliance", `"decision"`, "approver", "created_by", "change_request"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("response leaks %q: %s", forbidden, body)
			}
		}
	}
	var dm map[string]json.RawMessage
	d.decode(t, &dm)
	var dk []string
	for k := range dm {
		dk = append(dk, k)
	}
	sort.Strings(dk)
	if !tbEq(dk, tenantOperatingBasisGoldenFields) {
		t.Fatalf("detail wire field set drifted: %v", dk)
	}
}

func tbContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestC2_TenantOperatingBases_CrossTenantIsolation_NoOracle(t *testing.T) {
	w := newTBWorld(t)
	taA := w.tenantTok(w.tA, w.staff(w.tA, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)
	taB := w.tenantTok(w.tB, w.staff(w.tB, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)

	// By id: B asking for A's row == B asking for an unknown id (same status, code and message); and vice versa.
	foreign := w.do("GET", tbList+"/"+w.activeA, taB, nil)
	unknown := w.do("GET", tbList+"/"+uuid.NewString(), taB, nil)
	wantStatus(t, "foreign id", foreign, http.StatusNotFound, "")
	wantStatus(t, "unknown id", unknown, http.StatusNotFound, "")
	if tbShape(t, foreign) != tbShape(t, unknown) {
		t.Fatalf("foreign and unknown ids are distinguishable: %s vs %s", foreign.body, unknown.body)
	}
	if strings.Contains(string(foreign.body), w.activeA) || strings.Contains(string(foreign.body), w.tA.String()) {
		t.Errorf("not-found body echoes ids: %s", foreign.body)
	}
	wantStatus(t, "A reads B's id", w.do("GET", tbList+"/"+w.idsB[0], taA, nil), http.StatusNotFound, "")

	// By brand filter: B filtering by A's brand == B filtering by an unknown brand (404, identical); own brand works.
	fb := w.do("GET", tbList+"?brand_id="+w.brandA.String(), taB, nil)
	ub := w.do("GET", tbList+"?brand_id="+uuid.NewString(), taB, nil)
	wantStatus(t, "foreign brand", fb, http.StatusNotFound, "")
	wantStatus(t, "unknown brand", ub, http.StatusNotFound, "")
	if tbShape(t, fb) != tbShape(t, ub) {
		t.Fatalf("foreign and unknown brands are distinguishable: %s vs %s", fb.body, ub.body)
	}
	ob := w.do("GET", tbList+"?brand_id="+w.brandB.String(), taB, nil)
	wantStatus(t, "own brand", ob, http.StatusOK, "")
	if got := w.ids(t, ob); len(got) != 1 || got[0] != w.idsB[1] {
		t.Errorf("own brand filter: %v", got)
	}
	wantStatus(t, "malformed brand", w.do("GET", tbList+"?brand_id=nope", taB, nil), http.StatusBadRequest, "")

	// By pagination: walking every page as B never yields an A row, whatever the page size or offset; total counts only B's rows.
	seen := []string{}
	for off := 0; off < 6; off++ {
		r := w.do("GET", tbList+"?limit=1&offset="+tbItoa(off), taB, nil)
		wantStatus(t, "page", r, http.StatusOK, "")
		seen = append(seen, w.ids(t, r)...)
		var pg struct{ Total int }
		r.decode(t, &pg)
		if pg.Total != len(w.idsB) {
			t.Fatalf("total leaks other tenants' rows: %d", pg.Total)
		}
	}
	if !tbEq(tbSorted(seen), tbSorted(w.idsB)) {
		t.Fatalf("pagination as B: %v", seen)
	}
	// Oversized limit is clamped (bounded), never unbounded.
	var pg struct{ Limit int }
	w.do("GET", tbList+"?limit=100000", taB, nil).decode(t, &pg)
	if pg.Limit != maxPageLimit {
		t.Errorf("limit not clamped: %d", pg.Limit)
	}
	// By a client tenant parameter: ignored, never honoured (own data comes back regardless).
	for _, q := range []string{"?tenant_id=" + w.tA.String(), "?tenantID=" + w.tA.String()} {
		r := w.do("GET", tbList+q, taB, nil)
		wantStatus(t, "tenant param ignored", r, http.StatusOK, "")
		if got := w.ids(t, r); !tbEq(got, tbSorted(w.idsB)) {
			t.Fatalf("tenant param honoured?! %v", got)
		}
	}
	// Country filter stays inside the tenant.
	if got := w.ids(t, w.do("GET", tbList+"?country_code=DE", taB, nil)); len(got) != 0 {
		t.Errorf("country filter crossed tenants: %v", got)
	}
	wantStatus(t, "lowercase country", w.do("GET", tbList+"?country_code=de", taB, nil), http.StatusBadRequest, "")
	wantStatus(t, "bad current", w.do("GET", tbList+"?current=maybe", taB, nil), http.StatusBadRequest, "")
	wantStatus(t, "malformed id", w.do("GET", tbList+"/nope", taB, nil), http.StatusBadRequest, "")
}

func TestC2_TenantOperatingBases_Authorization(t *testing.T) {
	w := newTBWorld(t)
	wantStatus(t, "unauthenticated list", w.do("GET", tbList, "", nil), http.StatusUnauthorized, "")
	wantStatus(t, "unauthenticated detail", w.do("GET", tbList+"/"+w.activeA, "", nil), http.StatusUnauthorized, "")
	wantStatus(t, "garbage token", w.do("GET", tbList, "not-a-token", nil), http.StatusUnauthorized, "")

	for name, role := range map[string]struct {
		sr identity.StaffRole
		ar auth.Role
	}{
		"support": {identity.StaffRoleSupport, auth.RoleSupport}, "finance": {identity.StaffRoleFinance, auth.RoleFinance},
		"risk_manager": {identity.StaffRoleRiskManager, auth.RoleRiskManager},
	} {
		tok := w.tenantTok(w.tA, w.staff(w.tA, role.sr), role.ar)
		wantStatus(t, name+" list", w.do("GET", tbList, tok, nil), http.StatusForbidden, "")
		wantStatus(t, name+" detail", w.do("GET", tbList+"/"+w.activeA, tok, nil), http.StatusForbidden, "")
	}

	// A platform_admin token has no tenant: refused on the tenant route even when it supplies a tenant parameter.
	pa := w.tokReq()
	wantStatus(t, "platform list", w.do("GET", tbList, pa, nil), http.StatusForbidden, "")
	wantStatus(t, "platform list + tenant_id", w.do("GET", tbList+"?tenant_id="+w.tA.String(), pa, nil), http.StatusForbidden, "")
	wantStatus(t, "platform detail", w.do("GET", tbList+"/"+w.activeA, pa, nil), http.StatusForbidden, "")

	// A tenant role is refused on the platform route that takes a tenant parameter (the platform route is not a back door).
	taB := w.tenantTok(w.tB, w.staff(w.tB, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)
	wantStatus(t, "tenant on platform list", w.do("GET", "/v1/admin/operating-bases?tenant_id="+w.tA.String(), taB, nil), http.StatusForbidden, "")
	wantStatus(t, "tenant on platform detail", w.do("GET", "/v1/admin/operating-bases/"+w.activeA, taB, nil), http.StatusForbidden, "")

	// Read-only: no write verb is routed.
	taA := w.tenantTok(w.tA, w.staff(w.tA, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		r := w.do(m, tbList, taA, map[string]any{})
		if r.status != http.StatusMethodNotAllowed && r.status != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 405/404", m, tbList, r.status)
		}
	}
}

// The database layer, independent of the handler: a tenant-scoped session sees zero foreign rows whatever the query, a handler-level
// function called with a manipulated tenant id is refused, and a session with no tenant sees nothing.
func TestC2_TenantOperatingBases_RLS_HoldsBelowTheHandler(t *testing.T) {
	w := newTBWorld(t)
	ctx := context.Background()
	if err := w.rt.WithTenant(ctx, w.tB, func(ctx context.Context, tx pgx.Tx) error {
		var n, foreign, byID int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases`).Scan(&n); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases WHERE tenant_id = $1`, w.tA).Scan(&foreign); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases WHERE id = $1`, w.activeA).Scan(&byID); err != nil {
			return err
		}
		if n != len(w.idsB) || foreign != 0 || byID != 0 {
			t.Errorf("tenant B session: total=%d foreign=%d byID=%d (want %d,0,0)", n, foreign, byID, len(w.idsB))
		}
		// Manipulated context: the service functions are handed tenant A's id inside tenant B's session.
		if _, _, err := operatingmarket.ListOwnOperatingBasesPage(ctx, tx, w.tA, nil, "", false, 50, 0); !errors.Is(err, operatingmarket.ErrTransactionScope) {
			t.Errorf("list with a manipulated tenant id must be a scope error, got %v", err)
		}
		if _, err := operatingmarket.GetOwnOperatingBasis(ctx, tx, w.tA, uuid.MustParse(w.activeA)); !errors.Is(err, operatingmarket.ErrTransactionScope) {
			t.Errorf("get with a manipulated tenant id must be a scope error, got %v", err)
		}
		// Even bypassing the scope assertion with B's own id, A's brand and A's row are not found (RLS + the explicit tenant predicate).
		if _, _, err := operatingmarket.ListOwnOperatingBasesPage(ctx, tx, w.tB, &w.brandA, "", false, 50, 0); !errors.Is(err, operatingmarket.ErrNotFound) {
			t.Errorf("foreign brand must be not found, got %v", err)
		}
		if _, err := operatingmarket.GetOwnOperatingBasis(ctx, tx, w.tB, uuid.MustParse(w.activeA)); !errors.Is(err, operatingmarket.ErrNotFound) {
			t.Errorf("foreign row must be not found, got %v", err)
		}
		// The tenant session is read-only through these functions and cannot write the table at all.
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A tenant session cannot insert or close rows (no tenant write policy).
	err := w.rt.WithTenant(ctx, w.tB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operating_bases SET effective_to = now() WHERE tenant_id = $1`, w.tB)
		return err
	})
	if err == nil {
		// UPDATE with no admitted row is a 0-row no-op under RLS; prove nothing changed.
		var open int
		_ = w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases WHERE tenant_id = $1 AND effective_to IS NULL`, w.tB).Scan(&open)
		})
		if open != len(w.idsB) {
			t.Errorf("a tenant session changed rows (open=%d)", open)
		}
	}
	// No tenant context at all (plain session): zero rows.
	if err := w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("a session without tenant context sees %d rows", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A player-scoped session on tenant A (app.player_account_id set) sees none of A's rows.
	if err := w.rt.WithTenant(ctx, w.tA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.player_account_id', $1, true)`, uuid.NewString()); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("a player-scoped session sees %d rows", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// FORCE RLS is in force on the table.
	var force, enabled bool
	if err := w.owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT relforcerowsecurity, relrowsecurity FROM pg_class WHERE oid = 'public.operating_bases'::regclass`).Scan(&force, &enabled)
	}); err != nil || !force || !enabled {
		t.Fatalf("operating_bases must be ENABLE+FORCE RLS: force=%v enabled=%v err=%v", force, enabled, err)
	}
}

// shape reduces an error body to its stable fields (status, code, message), dropping the per-request id.
func tbShape(t *testing.T, r cgResp) string {
	t.Helper()
	var m map[string]any
	r.decode(t, &m)
	delete(m, "request_id")
	delete(m, "requestId")
	b, _ := json.Marshal(m)
	return http.StatusText(r.status) + string(b)
}

func tbItoa(i int) string { b, _ := json.Marshal(i); return string(b) }

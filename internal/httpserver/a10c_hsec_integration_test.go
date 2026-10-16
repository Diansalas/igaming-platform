//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A10c: the hold-candidate list (ADR 0114 R3) and the Idempotency-Key wrapper on the HSEC request route.
func TestA10c_HSEC_CandidatesAndIdempotency(t *testing.T) {
	w := newHRWorld(t, true)
	wr := w.approvedWithdrawalOnSuspendedTenant(750)
	url := "/v1/admin/tenants/" + w.tenant.String() + "/withdrawal-hold-candidates"
	pa := w.tokenFor(w.p1)

	res := w.a.do("GET", url, pa, nil)
	if res.status != http.StatusOK {
		t.Fatalf("candidates: %d %s", res.status, res.body)
	}
	var pg pagedResponse[holdCandidateDTO]
	res.decode(t, &pg)
	if pg.Total != 1 || len(pg.Items) != 1 {
		t.Fatalf("candidates = %+v", pg)
	}
	c := pg.Items[0]
	if c.WithdrawalRequestID != wr.String() || c.AmountMinorUnits != "750" || c.AssetCode != "USD" || c.Exponent != 2 || c.TenantStatus != "suspended" {
		t.Errorf("candidate = %+v", c)
	}
	for _, id := range []uuid.UUID{w.player, w.wallet} {
		if strings.Contains(string(res.body), id.String()) {
			t.Errorf("candidate body names a player or wallet id %s: %s", id, res.body)
		}
	}
	for _, bad := range []string{"player", "wallet", "destination", "provider_reference", "idempotency"} {
		if strings.Contains(string(res.body), bad) {
			t.Errorf("candidate body contains %q: %s", bad, res.body)
		}
	}
	// Authorization: platform_admin only; every tenant role and a tenant-scoped token are refused; no token 401.
	for name, tok := range map[string]string{"tenant_admin": w.tok(w.ta), "finance": w.tok(w.f1), "compliance": w.tok(w.comp), "support": w.tok(w.supp)} {
		if r := w.a.do("GET", url, tok, nil); r.status != http.StatusForbidden {
			t.Errorf("%s: %d", name, r.status)
		}
	}
	if r := w.a.do("GET", url, "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("no token: %d", r.status)
	}
	// Tenant scope: another tenant lists nothing of this tenant; an unknown tenant is 404.
	if r := w.a.do("GET", "/v1/admin/tenants/"+w.other.String()+"/withdrawal-hold-candidates", pa, nil); r.status != http.StatusOK || !strings.Contains(string(r.body), `"total":0`) {
		t.Errorf("other tenant candidates: %d %s", r.status, r.body)
	}
	if r := w.a.do("GET", "/v1/admin/tenants/"+uuid.NewString()+"/withdrawal-hold-candidates", pa, nil); r.status != http.StatusNotFound {
		t.Errorf("ghost tenant: %d", r.status)
	}
	for _, q := range []string{"limit=0", "limit=201", "offset=-1", "foo=1", "limit=1&limit=2", "tenant_id=" + w.other.String()} {
		if r := w.a.do("GET", url+"?"+q, pa, nil); r.status != http.StatusBadRequest {
			t.Errorf("query %q: %d", q, r.status)
		}
	}
	var audits int
	if err := w.a.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal_hold_candidates.platform_read' AND subject_tenant_id = $1 AND tenant_id IS NULL`, w.tenant).Scan(&audits)
	}); err != nil || audits < 1 {
		t.Errorf("platform candidate reads audited %d times (err %v)", audits, err)
	}

	// Idempotency on the HSEC request: the replay returns the same 201 and creates ONE resolution.
	body := a10cJSON(t, map[string]any{"withdrawal_request_id": wr.String(), "kind": "release_hold_to_player", "reason_code": "tenant_suspended_release",
		"evidence_ref_hash": strings.Repeat("ab", 32), "note": "a10c"})
	r1 := w.a.doRaw("POST", w.base(), pa, body, idemHdr("hsec-req"))
	if r1.status != http.StatusCreated {
		t.Fatalf("request: %d %s", r1.status, r1.body)
	}
	r2 := w.a.doRaw("POST", w.base(), pa, body, idemHdr("hsec-req"))
	if r2.status != http.StatusCreated || !isAlreadyApplied(r2.body) || r2.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s", r2.status, r2.body)
	}
	var d hrDTO
	r1.decode(t, &d)
	list := w.a.do("GET", w.base(), pa, nil)
	if strings.Count(string(list.body), d.ID) != 1 {
		t.Fatalf("a replayed request created more than one resolution: %s", list.body)
	}
	if st, _ := w.decide(w.p1, d.ID, "approve", d.PayloadHash); st == http.StatusOK {
		t.Fatalf("the requester must still not approve with the key wrapper in place")
	}
	// A tenant role with a key: 403 first, nothing reserved.
	n0 := w.a.idemRows(t, w.tenant)
	if r := w.a.doRaw("POST", w.base(), w.tok(w.f1), body, idemHdr("hsec-tenant")); r.status != http.StatusForbidden {
		t.Fatalf("tenant role: %d", r.status)
	}
	if n1 := w.a.idemRows(t, w.tenant); n1 != n0 {
		t.Fatalf("a forbidden tenant role reserved a key (%d -> %d)", n0, n1)
	}
	// A platform caller naming a tenant that does not exist: the foreign-key violation of the reservation falls through to the
	// handler's own refusal (never a 500, never a 2xx) and nothing is stored for that tenant.
	gr := w.a.doRaw("POST", "/v1/admin/tenants/"+uuid.NewString()+"/withdrawal-hold-resolutions", pa, body, idemHdr("hsec-ghost"))
	if gr.status < 400 || gr.status >= 500 {
		t.Fatalf("ghost tenant with a key: %d %s", gr.status, gr.body)
	}
	// Mismatch.
	other := a10cJSON(t, map[string]any{"withdrawal_request_id": wr.String(), "kind": "release_hold_to_player", "reason_code": "tenant_suspended_release",
		"evidence_ref_hash": strings.Repeat("cd", 32), "note": "changed"})
	if r := w.a.doRaw("POST", w.base(), pa, other, idemHdr("hsec-req")); r.status != http.StatusConflict || !strings.Contains(string(r.body), "IDEMPOTENCY_MISMATCH") {
		t.Fatalf("mismatch: %d %s", r.status, r.body)
	}
}

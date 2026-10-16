//go:build integration

// A10c security-review regressions for the staff Idempotency-Key table (SR10C-1 no stored response, SR10C-2 immutable
// rows, SR10C-3 documented replay semantics, SR10C-4 bounded table and reclaimable orphan) and the QA cases the review
// asked for: replay after session revocation, replay after grant revocation, a truly concurrent pair.
package httpserver

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

func reqHash(method, path string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method + "\n" + path + "\n\n"))
	h.Write(body)
	return h.Sum(nil)
}

func (a *cgAPI) execTenant(t *testing.T, tenantID uuid.UUID, sql string, args ...any) error {
	t.Helper()
	return a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// SR10C-1: the table cannot hold a response body, and a plain tenant-scoped session (no acting session) reads nothing
// sensitive from it after governed K1 / K3 / HSEC calls were stored.
func TestA10c_SR10C1_TableHoldsNoSensitiveData(t *testing.T) {
	w := newSRWorld(t)
	body := a10cJSON(t, map[string]any{"attempt_id": w.depA[0].String(), "kind": "m1_deposit_evidence", "finding_code": "awaiting_psp_refund",
		"reason_code": "http-k3", "note": "sr10c1"})
	r := w.a.doRaw("POST", w.base(), w.tok(w.f1), body, idemHdr("sr1-k3"))
	if r.status != http.StatusCreated {
		t.Fatalf("k3 request: %d %s", r.status, r.body)
	}
	var d frDTO
	r.decode(t, &d)

	var cols []string
	var dump strings.Builder
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'staff_idempotency_keys'`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			cols = append(cols, c)
		}
		rows.Close()
		if rows.Err() != nil {
			return rows.Err()
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_idempotency_keys`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("rows = %d, err %v", n, err)
		}
		var text string
		if err := tx.QueryRow(ctx, `SELECT t::text FROM staff_idempotency_keys t`).Scan(&text); err != nil {
			return err
		}
		dump.WriteString(text)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(cols)
	want := []string{"actor_id", "completed_at", "created_at", "id", "idempotency_key", "request_hash", "response_status", "route", "state", "tenant_id"}
	if strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Fatalf("staff_idempotency_keys columns = %v, want exactly %v (no response body column may exist)", cols, want)
	}
	// Nothing of the governed response is in the row: no resolution id, payload hash, attempt id, amount, staff id.
	for _, secret := range []string{d.ID, d.PayloadHash, d.AttemptID} {
		if secret != "" && strings.Contains(dump.String(), secret) {
			t.Errorf("the idempotency row carries %q: %s", secret, dump.String())
		}
	}
}

// SR10C-2 / SR10C-4: the guard trigger.
func TestA10c_SR10C2_RowsAreImmutableExceptTheOneShotCompletion(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	actor := uuid.New()
	ins := func(key, state string, ageMin int) {
		t.Helper()
		var err error
		if state == "completed" {
			err = a.execTenant(t, tenantID, `INSERT INTO staff_idempotency_keys (tenant_id, actor_id, idempotency_key, route, request_hash, state, response_status, created_at, completed_at)
				VALUES ($1, $2, $3, 'POST /x', $4, 'completed', 201, now() - make_interval(mins => $5), now() - make_interval(mins => $5))`,
				tenantID, actor, key, reqHash("POST", "/x", []byte(key)), ageMin)
		} else {
			err = a.execTenant(t, tenantID, `INSERT INTO staff_idempotency_keys (tenant_id, actor_id, idempotency_key, route, request_hash, state, created_at)
				VALUES ($1, $2, $3, 'POST /x', $4, 'in_progress', now() - make_interval(mins => $5))`,
				tenantID, actor, key, reqHash("POST", "/x", []byte(key)), ageMin)
		}
		if err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	ins("done", "completed", 1)
	ins("run", "in_progress", 1)
	ins("old", "completed", 25*60)

	mustFail := func(what, code, sql string, args ...any) {
		t.Helper()
		err := a.execTenant(t, tenantID, sql, args...)
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Errorf("%s: want %s, got %v", what, code, err)
		}
	}
	mustFail("rewrite the stored status of a completed row", "SK001", `UPDATE staff_idempotency_keys SET response_status = 200 WHERE idempotency_key = 'done'`)
	mustFail("reopen a completed row", "SK001", `UPDATE staff_idempotency_keys SET state = 'in_progress', response_status = NULL, completed_at = NULL WHERE idempotency_key = 'done'`)
	mustFail("change the request hash of an in_progress row", "SK001", `UPDATE staff_idempotency_keys SET request_hash = $1 WHERE idempotency_key = 'run'`, reqHash("POST", "/y", nil))
	mustFail("move an in_progress row to another actor", "SK001", `UPDATE staff_idempotency_keys SET actor_id = $1 WHERE idempotency_key = 'run'`, uuid.New())
	mustFail("delete a fresh completed row", "SK002", `DELETE FROM staff_idempotency_keys WHERE idempotency_key = 'done'`)
	// The permitted paths.
	if err := a.execTenant(t, tenantID, `UPDATE staff_idempotency_keys SET state = 'completed', response_status = 201, completed_at = now() WHERE idempotency_key = 'run'`); err != nil {
		t.Errorf("the one-shot completion was refused: %v", err)
	}
	mustFail("complete twice", "SK001", `UPDATE staff_idempotency_keys SET state = 'completed', response_status = 200, completed_at = now() WHERE idempotency_key = 'run'`)
	if err := a.execTenant(t, tenantID, `DELETE FROM staff_idempotency_keys WHERE idempotency_key = 'old'`); err != nil {
		t.Errorf("retention delete of a completed row older than 24 h was refused: %v", err)
	}
	ins("res", "in_progress", 1)
	if err := a.execTenant(t, tenantID, `DELETE FROM staff_idempotency_keys WHERE idempotency_key = 'res'`); err != nil {
		t.Errorf("releasing an in_progress reservation was refused: %v", err)
	}
	// Row-level security: the other tenant's scope cannot touch this tenant's rows (zero rows affected, no error).
	other := a.tenant()
	if err := a.execTenant(t, other, `DELETE FROM staff_idempotency_keys`); err != nil {
		t.Errorf("foreign-scope delete: %v", err)
	}
	if n := a.idemRows(t, tenantID); n != 2 {
		t.Errorf("rows after the matrix = %d, want 2 (done, run)", n)
	}
}

// SR10C-4: an expired in_progress key is reclaimed, a live one blocks, and expired rows of the tenant are swept.
func TestA10c_SR10C4_BoundedTableAndReclaimableOrphan(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	admin := a.staff(tenantID, "tenant_admin")
	tok := a.token(admin, tenantID, auth.RoleTenantAdmin)
	path := "/v1/admin/tenants/" + tenantID.String() + "/capability-grants/requests"
	mk := func() ([]byte, uuid.UUID) {
		g := a.staff(tenantID, "finance")
		return a10cJSON(t, map[string]any{"grantee_staff_id": g.String(), "capability": "ledger_adjustment:initiate", "reason_code": "t"}), g
	}
	seed := func(key string, body []byte, ageMin int) {
		t.Helper()
		if err := a.execTenant(t, tenantID, `INSERT INTO staff_idempotency_keys (tenant_id, actor_id, idempotency_key, route, request_hash, state, created_at)
			VALUES ($1, $2, $3, $4, $5, 'in_progress', now() - make_interval(mins => $6))`, tenantID, admin, key, "POST "+path, reqHash("POST", path, body), ageMin); err != nil {
			t.Fatal(err)
		}
	}
	// A 5-minute-old in_progress key still blocks.
	b1, g1 := mk()
	seed("live", b1, 5)
	if r := a.doRaw("POST", path, tok, b1, idemHdr("live")); r.status != http.StatusConflict || !strings.Contains(string(r.body), "IDEMPOTENCY_IN_PROGRESS") {
		t.Fatalf("live in_progress key: %d %s", r.status, r.body)
	}
	if n := a.requestCount(tenantID, g1); n != 0 {
		t.Fatalf("a live in_progress key let the handler run (%d)", n)
	}
	// A 20-minute-old one is an orphan: reclaimed, the handler runs once, the key completes.
	b2, g2 := mk()
	seed("orphan", b2, 20)
	if r := a.doRaw("POST", path, tok, b2, idemHdr("orphan")); r.status != http.StatusCreated {
		t.Fatalf("orphaned key not reclaimed: %d %s", r.status, r.body)
	}
	if n := a.requestCount(tenantID, g2); n != 1 {
		t.Fatalf("requests after reclaim = %d, want 1", n)
	}
	if r := a.doRaw("POST", path, tok, b2, idemHdr("orphan")); r.status != http.StatusCreated || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay after reclaim: %d replayed=%q", r.status, r.header.Get("Idempotent-Replayed"))
	}
	// Expired completed rows are swept by any keyed request of the tenant; fresh ones stay.
	if err := a.execTenant(t, tenantID, `INSERT INTO staff_idempotency_keys (tenant_id, actor_id, idempotency_key, route, request_hash, state, response_status, created_at, completed_at)
		VALUES ($1, $2, 'ancient', 'POST /x', $3, 'completed', 201, now() - interval '30 hours', now() - interval '30 hours')`, tenantID, admin, reqHash("POST", "/x", nil)); err != nil {
		t.Fatal(err)
	}
	b3, _ := mk()
	if r := a.doRaw("POST", path, tok, b3, idemHdr("sweeper")); r.status != http.StatusCreated {
		t.Fatalf("sweeper request: %d %s", r.status, r.body)
	}
	var ancient, fresh int
	if err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_idempotency_keys WHERE idempotency_key = 'ancient'`).Scan(&ancient); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_idempotency_keys WHERE idempotency_key IN ('orphan', 'sweeper')`).Scan(&fresh)
	}); err != nil {
		t.Fatal(err)
	}
	if ancient != 0 || fresh != 2 {
		t.Fatalf("after the sweep: ancient=%d (want 0) fresh=%d (want 2)", ancient, fresh)
	}
	// The sweep is per tenant: another tenant's expired row is untouched by this tenant's traffic.
	other := a.tenant()
	if err := a.execTenant(t, other, `INSERT INTO staff_idempotency_keys (tenant_id, actor_id, idempotency_key, route, request_hash, state, response_status, created_at, completed_at)
		VALUES ($1, $2, 'theirs', 'POST /x', $3, 'completed', 201, now() - interval '30 hours', now() - interval '30 hours')`, other, uuid.New(), reqHash("POST", "/x", nil)); err != nil {
		t.Fatal(err)
	}
	b4, _ := mk()
	if r := a.doRaw("POST", path, tok, b4, idemHdr("again")); r.status != http.StatusCreated {
		t.Fatalf("again: %d", r.status)
	}
	if n := a.idemRows(t, other); n != 1 {
		t.Fatalf("another tenant's expired row was swept by this tenant's traffic (%d rows)", n)
	}
}

// QA: replay after the actor's session is revoked is refused by the chain (401) and is never marked as a replay.
func TestA10c_Replay_AfterSessionRevocationIsRefused(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	admin := a.staff(tenantID, "tenant_admin")
	tok := a.token(admin, tenantID, auth.RoleTenantAdmin)
	path := "/v1/admin/tenants/" + tenantID.String() + "/capability-grants/requests"
	g := a.staff(tenantID, "finance")
	body := a10cJSON(t, map[string]any{"grantee_staff_id": g.String(), "capability": "ledger_adjustment:initiate", "reason_code": "t"})
	if r := a.doRaw("POST", path, tok, body, idemHdr("rev")); r.status != http.StatusCreated {
		t.Fatalf("first: %d %s", r.status, r.body)
	}
	claims, err := a.issuer.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	sid := uuid.MustParse(claims.SessionID)
	if err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.session_internal_op_id', $1, true)`, sid.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1`, sid)
		return err
	}); err != nil {
		t.Fatalf("revoke the session: %v", err)
	}
	r := a.doRaw("POST", path, tok, body, idemHdr("rev"))
	if r.status != http.StatusUnauthorized || r.header.Get("Idempotent-Replayed") != "" || isAlreadyApplied(r.body) {
		t.Fatalf("replay after revocation: %d replayed=%q %s", r.status, r.header.Get("Idempotent-Replayed"), r.body)
	}
}

// QA / SR10C-3: after the requester's capability grant is revoked a keyed replay still answers "already applied" (nothing is
// acted on, no data is returned), while a NEW request (no replay) is refused by the handler's grant check.
func TestA10c_SR10C3_Replay_AfterGrantRevocation_IsDocumentedBehaviour(t *testing.T) {
	w := newSRWorld(t)
	body := a10cJSON(t, map[string]any{"attempt_id": w.depA[0].String(), "kind": "m1_deposit_evidence", "finding_code": "awaiting_psp_refund",
		"reason_code": "http-k3", "note": "sr10c3"})
	if r := w.a.doRaw("POST", w.base(), w.tok(w.f1), body, idemHdr("gp2")); r.status != http.StatusCreated {
		t.Fatalf("first: %d %s", r.status, r.body)
	}
	var grantID uuid.UUID
	if err := w.a.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM staff_capability_grants WHERE tenant_id = $1 AND grantee_staff_id = $2
			AND capability = 'payment_force_resolve:request' AND revoked_at IS NULL LIMIT 1`, w.tenant, w.f1).Scan(&grantID)
	}); err != nil {
		t.Skipf("no request grant for the requester in this world: %v", err)
	}
	rv := w.a.do("POST", "/v1/admin/tenants/"+w.tenant.String()+"/capability-grants/"+grantID.String()+"/revoke", w.tok(w.pa), map[string]any{"reason_code": "test"})
	if rv.status != http.StatusNoContent {
		t.Skipf("could not revoke the grant through the API in this world: %d %s", rv.status, rv.body)
	}
	replay := w.a.doRaw("POST", w.base(), w.tok(w.f1), body, idemHdr("gp2"))
	if replay.status != http.StatusCreated || replay.header.Get("Idempotent-Replayed") != "true" || !isAlreadyApplied(replay.body) {
		t.Fatalf("documented replay after grant revocation: %d replayed=%q %s", replay.status, replay.header.Get("Idempotent-Replayed"), replay.body)
	}
	fresh := a10cJSON(t, map[string]any{"attempt_id": w.depA[1].String(), "kind": "m1_deposit_evidence", "finding_code": "awaiting_psp_refund",
		"reason_code": "http-k3", "note": "sr10c3 new"})
	if r := w.a.doRaw("POST", w.base(), w.tok(w.f1), fresh, idemHdr("gp2-new")); r.status < 400 {
		t.Fatalf("a NEW request after the grant was revoked must be refused, got %d %s", r.status, r.body)
	}
}

// QA: a truly concurrent pair with the same key and body: the handler runs exactly once.
func TestA10c_ConcurrentPair_SameKey_RunsTheHandlerOnce(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	admin := a.staff(tenantID, "tenant_admin")
	tok := a.token(admin, tenantID, auth.RoleTenantAdmin)
	path := "/v1/admin/tenants/" + tenantID.String() + "/capability-grants/requests"
	for i := 0; i < 8; i++ {
		g := a.staff(tenantID, "finance")
		body := a10cJSON(t, map[string]any{"grantee_staff_id": g.String(), "capability": "ledger_adjustment:initiate", "reason_code": "t"})
		key := "race-" + uuid.NewString()[:8]
		var wg sync.WaitGroup
		res := make([]cgResp, 2)
		start := make(chan struct{})
		for j := range res {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				<-start
				res[j] = a.doRaw("POST", path, tok, body, idemHdr(key))
			}(j)
		}
		close(start)
		wg.Wait()
		created, other := 0, 0
		for _, r := range res {
			switch {
			case r.status == http.StatusCreated && r.header.Get("Idempotent-Replayed") == "":
				created++
			case r.status == http.StatusCreated && isAlreadyApplied(r.body):
				other++
			case r.status == http.StatusConflict && strings.Contains(string(r.body), "IDEMPOTENCY_IN_PROGRESS"):
				other++
			default:
				t.Fatalf("round %d: unexpected response %d %s", i, r.status, r.body)
			}
		}
		if created != 1 || other != 1 {
			t.Fatalf("round %d: %d original, %d replay/in-progress (want 1 and 1): %d / %d", i, created, other, res[0].status, res[1].status)
		}
		if n := a.requestCount(tenantID, g); n != 1 {
			t.Fatalf("round %d: the handler ran %d times", i, n)
		}
	}
}

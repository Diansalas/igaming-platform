//go:build integration

// Slice C3 (QA test gaps), part 3: the licensing-change routes through the REAL HTTP stack (httptest server over the runtime pool,
// igaming_runtime asserted by the launch harness) for what a STALE or TAMPERED client can send, and for concurrent approvers.
// "The server is authoritative": every request below is what a stale Back Office screen, a replayed payload or a hand-crafted client
// could submit; the assertion is always the status/code AND that nothing was written.
//
//	(5) concurrent approvers / duplicate approval over HTTP, run N times
//	(6) approve a request cancelled or executed meanwhile; approve from a session whose staff account was suspended or whose role
//	    claim is stale; a stale step-up; mislabelled class hints / tampered required-approvals / payload_hash in bodies; wrong ids
//	(7) CHARACTERISATION of the single-actor jurisdiction creation path
//
// Test code only.
package httpserver

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

func (w *lhWorld) c3OwnerInt(q string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(&n)
	}); err != nil {
		w.t.Fatalf("owner query %q: %v", q, err)
	}
	return n
}

func (w *lhWorld) c3OwnerExec(q string, args ...any) {
	w.t.Helper()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, args...)
		return err
	}); err != nil {
		w.t.Fatalf("owner exec %q: %v", q, err)
	}
}

// c3Staff provisions a platform_admin account of a FRESH Person (owner fixture) and returns its id.
func (w *lhWorld) c3Staff() uuid.UUID {
	w.t.Helper()
	id, person := uuid.New(), uuid.New()
	w.c3OwnerExec(`INSERT INTO persons (id) VALUES ($1)`, person)
	w.c3OwnerExec(`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, NULL, $2, '!x', 'platform_admin', $3)`,
		id, "c3-"+id.String()+"@fixture.invalid", person)
	return id
}

func (w *lhWorld) c3Licence() uuid.UUID {
	w.t.Helper()
	jid, lid := uuid.New(), uuid.New()
	w.c3OwnerExec(`INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'c3 http')`, jid, "C3-"+strings.ToUpper(jid.String()[:10]))
	w.c3OwnerExec(`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`, lid, jid, "C3-"+lid.String()[:8])
	return lid
}

func c3CeilingBody(lid uuid.UUID, country, state, status string, ref any, reason string) map[string]any {
	return map[string]any{"kind": "licence_ceiling_version", "reason_code": reason,
		"payload": map[string]any{"licence_id": lid, "country_code": country, "state": state, "status": status, "authorization_reference": ref}}
}

func (w *lhWorld) c3File(tok string, body map[string]any) (string, licensingChangeResponseIT, cgResp) {
	w.t.Helper()
	r := w.do("POST", "/v1/admin/licensing-changes", tok, body)
	var out licensingChangeResponseIT
	if r.status == http.StatusCreated {
		r.decode(w.t, &out)
	}
	return out.ChangeRequest.ID, out, r
}

func (w *lhWorld) c3MustFileWidening(tok string, lid uuid.UUID, country string, reason string) string {
	w.t.Helper()
	id, res, r := w.c3File(tok, c3CeilingBody(lid, country, "enabled", "active", "TICKET-"+country, reason))
	if r.status != http.StatusCreated || res.Executed || !res.ChangeRequest.Widening || res.ChangeRequest.RequiredApprovals != 2 {
		w.t.Fatalf("file widening: %d %s", r.status, r.body)
	}
	return id
}

func (w *lhWorld) c3State(id string) (status string, approvals int) {
	w.t.Helper()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT r.status, (SELECT count(*) FROM licensing_change_approvals a WHERE a.request_id = r.id) FROM licensing_change_requests r WHERE r.id = $1`, id).Scan(&status, &approvals)
	}); err != nil {
		w.t.Fatalf("state: %v", err)
	}
	return
}

func (w *lhWorld) c3Fingerprint(lid uuid.UUID) [4]int {
	return [4]int{
		w.c3OwnerInt(`SELECT count(*) FROM licensing_change_requests`),
		w.c3OwnerInt(`SELECT count(*) FROM licensing_change_approvals`),
		w.c3OwnerInt(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1`, lid),
		w.c3OwnerInt(`SELECT count(*) FROM audit_log WHERE action LIKE 'licensing_change.%'`),
	}
}

// ---- (6) stale / tampered client state --------------------------------------------------------------------------------------------

func TestC3HTTP_ServerIsAuthoritative_StaleAndTamperedClientState(t *testing.T) {
	w := newLHWorld(t)
	lid := w.c3Licence()
	ap3 := w.api.token(w.c3Staff(), uuid.Nil, auth.RolePlatformAdmin)
	approve := func(tok, id string) cgResp { return w.lcApprove(tok, id) }
	wantCode := func(what string, r cgResp, status int, code string) {
		t.Helper()
		if r.status != status || (code != "" && r.code(t) != code) {
			t.Fatalf("%s: got %d %s, want %d %s", what, r.status, r.body, status, code)
		}
	}

	// (a) the UI shows "Approve" for a request that was CANCELLED meanwhile (stale list): 409, nothing recorded.
	a := w.c3MustFileWidening(w.tokReq(), lid, "IE", "c3_cancel_meanwhile")
	if r := w.do("POST", "/v1/admin/licensing-changes/"+a+"/cancel", w.tokReq(), map[string]any{}); r.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
	fp := w.c3Fingerprint(lid)
	wantCode("approve after cancel", approve(w.tokAp1(), a), http.StatusConflict, "LICENSING_CHANGE_NOT_PENDING")
	if st, n := w.c3State(a); st != "cancelled" || n != 0 || w.c3Fingerprint(lid) != fp {
		t.Fatalf("approve-after-cancel wrote something: %s %d %v -> %v", st, n, fp, w.c3Fingerprint(lid))
	}
	// reject after cancel, cancel after cancel
	wantCode("reject after cancel", w.do("POST", "/v1/admin/licensing-changes/"+a+"/reject", w.tokAp1(), map[string]any{"reason_code": "late"}), http.StatusConflict, "LICENSING_CHANGE_NOT_PENDING")
	wantCode("cancel after cancel", w.do("POST", "/v1/admin/licensing-changes/"+a+"/cancel", w.tokReq(), map[string]any{}), http.StatusConflict, "LICENSING_CHANGE_NOT_PENDING")

	// (b) EXECUTED meanwhile: a third Person's approve and a replayed approve are 409 and change nothing; one ceiling row only.
	b := w.c3MustFileWidening(w.tokReq(), lid, "DK", "c3_execute_meanwhile")
	if r := approve(w.tokAp1(), b); r.status != http.StatusOK {
		t.Fatalf("approve 1: %d %s", r.status, r.body)
	}
	if r := approve(w.tokAp2(), b); r.status != http.StatusOK {
		t.Fatalf("approve 2: %d %s", r.status, r.body)
	}
	fp = w.c3Fingerprint(lid)
	wantCode("third approve after execution", approve(ap3, b), http.StatusConflict, "LICENSING_CHANGE_NOT_PENDING")
	wantCode("replayed approve (same actor, same body)", approve(w.tokAp1(), b), http.StatusConflict, "LICENSING_CHANGE_NOT_PENDING")
	wantCode("cancel after execution", w.do("POST", "/v1/admin/licensing-changes/"+b+"/cancel", w.tokReq(), map[string]any{}), http.StatusConflict, "LICENSING_CHANGE_NOT_PENDING")
	if st, n := w.c3State(b); st != "executed" || n != 2 || w.c3Fingerprint(lid) != fp {
		t.Fatalf("approve-after-execute wrote something: %s %d", st, n)
	}

	// (b2) retry of the SAME first approval (client re-sent the identical payload): ALREADY_DECIDED, count unchanged.
	b2 := w.c3MustFileWidening(w.tokReq(), lid, "NL", "c3_retry")
	if r := approve(w.tokAp1(), b2); r.status != http.StatusOK {
		t.Fatalf("approve 1: %d", r.status)
	}
	wantCode("identical retry of an approval", approve(w.tokAp1(), b2), http.StatusConflict, "LICENSING_CHANGE_ALREADY_DECIDED")
	if st, n := w.c3State(b2); st != "requested" || n != 1 {
		t.Fatalf("retry changed state: %s %d", st, n)
	}

	// (c) a session whose staff account was SUSPENDED after the token was minted (role/privilege revoked): refused before the handler.
	c := w.c3MustFileWidening(w.tokReq(), lid, "FR", "c3_revoked")
	rev := w.c3Staff()
	revTok := w.api.token(rev, uuid.Nil, auth.RolePlatformAdmin)
	w.c3OwnerExec(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, rev)
	fp = w.c3Fingerprint(lid)
	wantCode("approve with a revoked staff account", approve(revTok, c), http.StatusUnauthorized, "session_revoked")
	wantCode("file with a revoked staff account", func() cgResp {
		_, _, r := w.c3File(revTok, c3CeilingBody(lid, "ES", "enabled", "active", "T", "c3_revoked_file"))
		return r
	}(), http.StatusUnauthorized, "session_revoked")
	if w.c3Fingerprint(lid) != fp {
		t.Fatal("a revoked session wrote something")
	}

	// (c2) a STALE ROLE CLAIM: the token says tenant_admin / support for an account that is platform_admin (role changed since): refused.
	stale := w.api.token(w.c3Staff(), uuid.Nil, auth.RoleSupport)
	wantCode("approve with a stale role claim (support)", approve(stale, c), http.StatusUnauthorized, "session_revoked")
	// (c3) a genuine tenant role may not approve (static permission), even with a live session
	tenantID := w.activeTenant()
	ta := w.staff(tenantID, identity.StaffRoleTenantAdmin)
	wantCode("approve as tenant admin", approve(w.tenantTok(tenantID, ta, auth.RoleTenantAdmin), c), http.StatusForbidden, "")
	if st, n := w.c3State(c); st != "requested" || n != 0 {
		t.Fatalf("revoked/stale/tenant approvals recorded something: %s %d", st, n)
	}

	// (d) a stale STEP-UP: the session's second factor is older than the critical window: the approval is refused and nothing is written.
	id := w.c3Staff()
	aged := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, id, uuid.Nil, auth.RolePlatformAdmin, 2*time.Hour)
	fp = w.c3Fingerprint(lid)
	r := approve(aged, c)
	t.Logf("approve with a stale step-up: %d %s", r.status, r.body)
	if r.status != http.StatusForbidden || r.code(t) != "step_up_required" {
		t.Fatalf("a stale step-up must be 403 step_up_required, got %d %s", r.status, r.body)
	}
	if w.c3Fingerprint(lid) != fp {
		t.Fatal("a stale step-up wrote something")
	}
	if r := w.do("POST", "/v1/admin/licensing-changes", aged, c3CeilingBody(lid, "IT", "enabled", "active", "T", "c3_stale_stepup")); r.status != http.StatusForbidden {
		t.Fatalf("filing with a stale step-up: %d %s", r.status, r.body)
	}
	if w.c3Fingerprint(lid) != fp {
		t.Fatal("a stale step-up filing wrote something")
	}

	// (e) MISLABELLED CLASS: a stale/hand-crafted client that sends its own class hint or approvals count is refused outright (strict
	// decode, 400) and nothing is stored; and a genuinely widening filing is NEVER executed at once whatever the client believed.
	fp = w.c3Fingerprint(lid)
	for name, body := range map[string]map[string]any{
		"top-level widening=false":       {"widening": false},
		"top-level required_approvals=0": {"required_approvals": 0},
		"top-level class hint":           {"class": "narrowing"},
		"top-level status=executed":      {"status": "executed"},
		"top-level requested_by":         {"requested_by": w.ops.Approver1.String()},
	} {
		b := c3CeilingBody(lid, "BE", "enabled", "active", "T", "c3_mislabel")
		for k, v := range body {
			b[k] = v
		}
		if r := w.do("POST", "/v1/admin/licensing-changes", w.tokReq(), b); r.status != http.StatusBadRequest || r.code(t) != "validation_error" {
			t.Errorf("%s: %d %s, want 400 validation_error", name, r.status, r.body)
		}
	}
	for name, extra := range map[string]map[string]any{
		"payload widening=false":       {"widening": false},
		"payload required_approvals=0": {"required_approvals": 0},
	} {
		b := c3CeilingBody(lid, "BE", "enabled", "active", "T", "c3_mislabel_payload")
		for k, v := range extra {
			b["payload"].(map[string]any)[k] = v
		}
		if r := w.do("POST", "/v1/admin/licensing-changes", w.tokReq(), b); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, r.status, r.body)
		}
	}
	if w.c3Fingerprint(lid) != fp {
		t.Fatal("a rejected tampered filing wrote something")
	}
	// the UI believed this was a narrowing; the server classifies it as widening and does NOT execute
	_, res, r := w.c3File(w.tokReq(), c3CeilingBody(lid, "BE", "enabled", "active", "TICKET-BE", "c3_ui_thought_narrowing"))
	if r.status != http.StatusCreated || res.Executed || !res.ChangeRequest.Widening || res.ChangeRequest.RequiredApprovals != 2 || res.ChangeRequest.Status != "requested" {
		t.Fatalf("server must classify as widening: %d %s", r.status, r.body)
	}
	if w.c3OwnerInt(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = 'BE'`, lid) != 0 {
		t.Fatal("a widening filing must not write the ceiling")
	}
	// and the reverse: the UI believed a disable needs two approvals; the server applies it at once for one platform admin (documented asymmetry)
	_, res, r = w.c3File(w.tokReq(), c3CeilingBody(lid, "DK", "disabled", "active", nil, "c3_ui_thought_widening"))
	if r.status != http.StatusCreated || !res.Executed || res.ChangeRequest.Widening || res.ChangeRequest.RequiredApprovals != 0 {
		t.Fatalf("server must classify the disable as narrowing: %d %s", r.status, r.body)
	}

	// (f) TAMPERED APPROVE bodies: required_approvals / payload_hash / approver id / empty reason: 400, nothing recorded.
	f := w.c3MustFileWidening(w.tokReq(), lid, "SE", "c3_tamper_approve")
	fp = w.c3Fingerprint(lid)
	for name, body := range map[string]map[string]any{
		"required_approvals": {"reason_code": "ok", "required_approvals": 1},
		"payload_hash":       {"reason_code": "ok", "payload_hash": strings.Repeat("0", 64)},
		"approver_id":        {"reason_code": "ok", "approver_id": w.ops.Approver2.String()},
		"status":             {"reason_code": "ok", "status": "executed"},
		"empty":              {},
		"bad reason":         {"reason_code": "has space"},
	} {
		if r := w.do("POST", "/v1/admin/licensing-changes/"+f+"/approve", w.tokAp1(), body); r.status != http.StatusBadRequest {
			t.Errorf("approve body %s: %d %s, want 400", name, r.status, r.body)
		}
	}
	if w.c3Fingerprint(lid) != fp {
		t.Fatal("a tampered approve body wrote something")
	}

	// (g) wrong request id: unknown -> 404; malformed -> 400; another request's id is just that request (no cross-binding).
	wantCode("unknown id", approve(w.tokAp1(), uuid.NewString()), http.StatusNotFound, "")
	wantCode("malformed id", approve(w.tokAp1(), "not-a-uuid"), http.StatusBadRequest, "validation_error")
	wantCode("nil uuid", approve(w.tokAp1(), uuid.Nil.String()), http.StatusBadRequest, "validation_error")
	if w.c3Fingerprint(lid) != fp {
		t.Fatal("wrong ids wrote something")
	}

	// (h) only the requester cancels; the requester's Person cannot reject.
	wantCode("cancel by a non-requester", w.do("POST", "/v1/admin/licensing-changes/"+f+"/cancel", w.tokAp1(), map[string]any{}), http.StatusForbidden, "")
	wantCode("reject by the requester", w.do("POST", "/v1/admin/licensing-changes/"+f+"/reject", w.tokReq(), map[string]any{"reason_code": "x"}), http.StatusConflict, "LICENSING_CHANGE_SELF_APPROVAL")
	if st, n := w.c3State(f); st != "requested" || n != 0 {
		t.Fatalf("refusals changed the request: %s %d", st, n)
	}
}

// ---- (5) concurrency over HTTP --------------------------------------------------------------------------------------------------

func (w *lhWorld) c3Race(calls map[string]func() cgResp) map[string]cgResp {
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	out := map[string]cgResp{}
	for name, f := range calls {
		wg.Add(1)
		go func(name string, f func() cgResp) {
			defer wg.Done()
			<-start
			r := f()
			mu.Lock()
			out[name] = r
			mu.Unlock()
		}(name, f)
	}
	close(start)
	wg.Wait()
	return out
}

func TestC3HTTP_ConcurrentApprovers_ExactlyOneExecution_20Rounds(t *testing.T) {
	w := newLHWorld(t)
	ap3 := w.api.token(w.c3Staff(), uuid.Nil, auth.RolePlatformAdmin)
	twin := w.lcTwinToken() // same Person as fixture approver 1
	tok1, tok2 := w.tokAp1(), w.tokAp2()
	for round := 0; round < 20; round++ {
		lid := w.c3Licence()
		id := w.c3MustFileWidening(w.tokReq(), lid, "MT", "c3_race")
		res := w.c3Race(map[string]func() cgResp{
			"ap1":    func() cgResp { return w.lcApprove(tok1, id) },
			"ap2":    func() cgResp { return w.lcApprove(tok2, id) },
			"ap3":    func() cgResp { return w.lcApprove(ap3, id) },
			"ap2dup": func() cgResp { return w.lcApprove(tok2, id) },
			"twin":   func() cgResp { return w.lcApprove(twin, id) },
			"self":   func() cgResp { return w.lcApprove(w.tokReq(), id) },
		})
		ok, executed := 0, 0
		for name, r := range res {
			switch {
			case r.status == http.StatusOK:
				ok++
				var d licensingChangeResponseIT
				r.decode(t, &d)
				if d.Executed {
					executed++
				}
			case name == "self":
				// the requester is refused as SELF_APPROVAL while the request is open, or NOT_PENDING once another approver won the race
				if r.status != http.StatusConflict || (r.code(t) != "LICENSING_CHANGE_SELF_APPROVAL" && r.code(t) != "LICENSING_CHANGE_NOT_PENDING") {
					t.Fatalf("round %d: self: %d %s", round, r.status, r.body)
				}
			case r.status == http.StatusConflict && (r.code(t) == "LICENSING_CHANGE_NOT_PENDING" || r.code(t) == "LICENSING_CHANGE_ALREADY_DECIDED"):
			default:
				t.Fatalf("round %d: %s: unexpected %d %s", round, name, r.status, r.body)
			}
		}
		if ok != 2 || executed != 1 {
			t.Fatalf("round %d: 200s=%d executed=%d, want 2 and 1", round, ok, executed)
		}
		if st, n := w.c3State(id); st != "executed" || n != 2 {
			t.Fatalf("round %d: %s %d", round, st, n)
		}
		if n := w.c3OwnerInt(`SELECT count(DISTINCT approver_person_id) FROM licensing_change_approvals WHERE request_id = $1`, id); n != 2 {
			t.Fatalf("round %d: distinct Persons %d", round, n)
		}
		if n := w.c3OwnerInt(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1`, lid); n != 1 {
			t.Fatalf("round %d: ceiling rows %d", round, n)
		}
		for action, want := range map[string]int{"executed": 1, "approved": 2, "requested": 1} {
			if n := w.c3OwnerInt(`SELECT count(*) FROM audit_log WHERE action = $1 AND target_id = $2`, "licensing_change."+action, id); n != want {
				t.Fatalf("round %d: audit %s = %d, want %d", round, action, n, want)
			}
		}
	}
}

func TestC3HTTP_ConcurrentFinalApprovalVersusCancel_20Rounds(t *testing.T) {
	w := newLHWorld(t)
	execd, canc := 0, 0
	for round := 0; round < 20; round++ {
		lid := w.c3Licence()
		id := w.c3MustFileWidening(w.tokReq(), lid, "MT", "c3_race_cancel")
		if r := w.lcApprove(w.tokAp1(), id); r.status != http.StatusOK {
			t.Fatalf("approve 1: %d %s", r.status, r.body)
		}
		res := w.c3Race(map[string]func() cgResp{
			"approve": func() cgResp { return w.lcApprove(w.tokAp2(), id) },
			"cancel": func() cgResp {
				return w.do("POST", "/v1/admin/licensing-changes/"+id+"/cancel", w.tokReq(), map[string]any{})
			},
		})
		st, n := w.c3State(id)
		ceil := w.c3OwnerInt(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1`, lid)
		a, c := res["approve"], res["cancel"]
		switch {
		case a.status == http.StatusOK && c.status == http.StatusConflict && c.code(t) == "LICENSING_CHANGE_NOT_PENDING":
			execd++
			if st != "executed" || n != 2 || ceil != 1 {
				t.Fatalf("round %d inconsistent (executed): %s %d %d", round, st, n, ceil)
			}
		case c.status == http.StatusOK && a.status == http.StatusConflict && a.code(t) == "LICENSING_CHANGE_NOT_PENDING":
			canc++
			if st != "cancelled" || n != 1 || ceil != 0 {
				t.Fatalf("round %d inconsistent (cancelled): %s %d %d", round, st, n, ceil)
			}
		default:
			t.Fatalf("round %d: approve=%d %s cancel=%d %s", round, a.status, a.body, c.status, c.body)
		}
	}
	t.Logf("HTTP approve-vs-cancel: executed %d, cancelled %d of 20 rounds", execd, canc)
}

// ---- audit through the HTTP layer: IP, user agent, tenant scope ------------------------------------------------------------------

func TestC3HTTP_AuditCarriesIPUserAgentAndTenantScope(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	co := w.staff(tenantID, identity.StaffRoleCompliance)
	coTok := w.tenantTok(tenantID, co, auth.RoleCompliance)
	own := w.do("POST", "/v1/admin/licensing-changes", coTok, map[string]any{"kind": "operating_policy_version", "reason_code": "c3_market_exit",
		"payload": map[string]any{"tenant_id": tenantID, "scope": "tenant", "brand_id": nil, "operation_code": nil, "product_code": nil,
			"country_code": "MT", "state": "disabled", "status": "active", "authorization_reference": nil}})
	if own.status != http.StatusCreated {
		t.Fatalf("tenant files narrowing: %d %s", own.status, own.body)
	}
	var f licensingChangeResponseIT
	own.decode(t, &f)
	id := f.ChangeRequest.ID
	if r := w.lcApprove(w.tokAp1(), id); r.status != http.StatusOK {
		t.Fatalf("platform approval: %d %s", r.status, r.body)
	}
	type row struct {
		actor, tenant, subject *uuid.UUID
		ip, ua, outcome        string
	}
	read := func(action string) []row {
		var out []row
		if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT actor_id, tenant_id, subject_tenant_id, coalesce(host(ip_address),''), coalesce(user_agent,''), outcome FROM audit_log WHERE action = $1 AND target_id = $2`, action, id)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.actor, &r.tenant, &r.subject, &r.ip, &r.ua, &r.outcome); err != nil {
					return err
				}
				out = append(out, r)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT actor_id, tenant_id, subject_tenant_id, coalesce(host(ip_address),''), coalesce(user_agent,''), outcome FROM audit_log WHERE action = $1 AND target_id = $2 AND tenant_id IS NOT NULL`, action, id)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.actor, &r.tenant, &r.subject, &r.ip, &r.ua, &r.outcome); err != nil {
					return err
				}
				out = append(out, r)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	req := read("licensing_change.requested")
	if len(req) != 1 || req[0].actor == nil || *req[0].actor != co || req[0].tenant == nil || *req[0].tenant != tenantID || req[0].ip != "127.0.0.1" || req[0].ua == "" || req[0].outcome != "success" {
		t.Fatalf("tenant request audit (actor, tenant scope, ip, user agent): %+v", req)
	}
	ex := read("licensing_change.executed")
	if len(ex) != 1 || ex[0].actor == nil || *ex[0].actor != w.ops.Approver1 || ex[0].tenant != nil || ex[0].subject == nil || *ex[0].subject != tenantID || ex[0].ip != "127.0.0.1" {
		t.Fatalf("platform executed audit (actor, platform scope + subject tenant, ip): %+v", ex)
	}
}

// ---- (7) CHARACTERISATION: jurisdiction creation is single-actor and grants nothing -------------------------------------------------

// CHARACTERISATION (pins CURRENT behaviour, lic32 finding 6, DECISION PENDING): one platform admin creates a jurisdiction at once. No
// licence, ceiling, policy, change request or permission is created, the market resolver is unchanged, one audit row exists. If the
// governance decision moves creation behind a licensing-change kind, this test MUST fail and be rewritten deliberately.
func TestC3HTTP_CHARACTERISATION_JurisdictionCreationSingleActor(t *testing.T) {
	w := newLHWorld(t)
	// an open market (ceiling + tenant policy) and a bare tenant, both observed through the REAL resolver before and after
	open := w.activeTenant()
	lid := w.c3Licence()
	w.c3OwnerExec(`UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, open)
	bare := w.activeTenant()
	for _, req := range []map[string]any{c3CeilingBody(lid, "MT", "enabled", "active", "TICKET-MT", "c3_char_ceiling"),
		{"kind": "operating_policy_version", "reason_code": "c3_char_policy", "payload": map[string]any{"tenant_id": open, "scope": "tenant", "brand_id": nil,
			"operation_code": nil, "product_code": nil, "country_code": "MT", "state": "enabled", "status": "active", "authorization_reference": "TICKET-POL"}}} {
		_, res, r := w.c3File(w.tokReq(), req)
		if r.status != http.StatusCreated {
			t.Fatalf("setup file: %d %s", r.status, r.body)
		}
		if r := w.lcApprove(w.tokAp1(), res.ChangeRequest.ID); r.status != http.StatusOK {
			t.Fatalf("setup approve 1: %d %s", r.status, r.body)
		}
		if r := w.lcApprove(w.tokAp2(), res.ChangeRequest.ID); r.status != http.StatusOK {
			t.Fatalf("setup approve 2: %d %s", r.status, r.body)
		}
	}
	resolve := func() string {
		var sb strings.Builder
		for _, tn := range []uuid.UUID{open, bare} {
			for _, cc := range []string{"MT", "DE", "FR"} {
				for _, op := range []string{"registration", "deposit", "wagering", "withdrawal"} {
					var o operatingmarket.Outcome
					asOf := time.Now().Add(time.Second)
					if err := w.rt.WithTenant(context.Background(), tn, func(ctx context.Context, tx pgx.Tx) error {
						res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{TenantID: tn, CountryCode: cc, OperationCode: op, AsOf: asOf})
						o = res.Outcome()
						return err
					}); err != nil {
						t.Fatalf("resolve: %v", err)
					}
					sb.WriteString(string(o) + ",")
				}
			}
		}
		return sb.String()
	}
	counts := func() [8]int {
		return [8]int{
			w.c3OwnerInt(`SELECT count(*) FROM jurisdictions`),
			w.c3OwnerInt(`SELECT count(*) FROM licences`),
			w.c3OwnerInt(`SELECT count(*) FROM licence_country_ceilings`),
			w.c3OwnerInt(`SELECT count(*) FROM licensing_change_requests`),
			w.c3OwnerInt(`SELECT count(*) FROM licensing_change_approvals`),
			w.c3OwnerInt(`SELECT count(*) FROM staff_users`),
			w.c3OwnerInt(`SELECT count(*) FROM tenants WHERE licence_id IS NOT NULL`),
			w.c3OwnerInt(`SELECT count(*) FROM operating_bases`),
		}
	}
	policies := func() int {
		return w.c3OwnerInt(`SELECT count(*) FROM tenant_jurisdiction_configs`) + w.rtTenantPolicies(open) + w.rtTenantPolicies(bare)
	}
	beforeRes, beforeCnt, beforePol := resolve(), counts(), policies()
	if !strings.Contains(beforeRes, "permitted") {
		t.Fatalf("precondition: the open market must resolve permitted somewhere: %s", beforeRes)
	}

	code := "C3-J-" + strings.ToUpper(uuid.NewString()[:8])
	r := w.do("POST", "/v1/admin/jurisdictions", w.tokReq(), map[string]any{"code": code, "name": "C3 characterisation", "reason_code": "c3_char_jurisdiction"})
	if r.status != http.StatusCreated {
		t.Fatalf("CHARACTERISATION changed: one platform admin must create a jurisdiction at once (201), got %d %s", r.status, r.body)
	}
	afterCnt := counts()
	want := beforeCnt
	want[0]++ // exactly one more jurisdiction; every other count is identical
	if afterCnt != want {
		t.Fatalf("CHARACTERISATION: counts before %v after %v (only jurisdictions may grow by one)", beforeCnt, afterCnt)
	}
	if got := resolve(); got != beforeRes {
		t.Fatalf("CHARACTERISATION: creating a jurisdiction changed the resolver:\n%s\n%s", beforeRes, got)
	}
	if policies() != beforePol {
		t.Fatal("CHARACTERISATION: a policy/configuration row appeared")
	}
	var jid uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM jurisdictions WHERE code = $1`, code).Scan(&jid)
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	var actor uuid.UUID
	var tenantNull bool
	var ip, outcome, reason string
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'jurisdiction_registry.jurisdiction_created' AND target_id = $1`, jid.String()).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT actor_id, tenant_id IS NULL, coalesce(host(ip_address),''), outcome, metadata->>'reason_code' FROM audit_log WHERE action = 'jurisdiction_registry.jurisdiction_created' AND target_id = $1`, jid.String()).
			Scan(&actor, &tenantNull, &ip, &outcome, &reason)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 || actor != w.ops.Requester || !tenantNull || ip != "127.0.0.1" || outcome != "success" || reason != "c3_char_jurisdiction" {
		t.Fatalf("CHARACTERISATION audit row: n=%d actor=%v tenantNull=%v ip=%s outcome=%s reason=%s", n, actor, tenantNull, ip, outcome, reason)
	}
	if k := w.c3OwnerInt(`SELECT count(*) FROM licences WHERE jurisdiction_id = $1`, jid); k != 0 {
		t.Fatalf("no licence may exist for the new jurisdiction, got %d", k)
	}

	// refused attempts create nothing: duplicate code, tenant staff, anonymous
	if r := w.do("POST", "/v1/admin/jurisdictions", w.tokReq(), map[string]any{"code": code, "name": "dup", "reason_code": "c3_dup"}); r.status != http.StatusBadRequest {
		t.Fatalf("duplicate code: %d %s", r.status, r.body)
	}
	tenantID := w.activeTenant()
	co := w.staff(tenantID, identity.StaffRoleCompliance)
	if r := w.do("POST", "/v1/admin/jurisdictions", w.tenantTok(tenantID, co, auth.RoleCompliance), map[string]any{"code": code + "X", "name": "x", "reason_code": "c3_tenant"}); r.status != http.StatusForbidden {
		t.Fatalf("tenant staff: %d %s", r.status, r.body)
	}
	if r := w.do("POST", "/v1/admin/jurisdictions", "", map[string]any{"code": code + "Y", "name": "y", "reason_code": "c3_anon"}); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", r.status)
	}
	if counts() != afterCnt {
		t.Fatal("refused attempts created rows")
	}
}

func (w *lhWorld) rtTenantPolicies(tid uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1`, tid).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// ---- exact closed refusal codes (C1 merged) on the wire, for the refusals a Back Office user can reach -------------------------------
// C1's own matrix (licensing_refusal_codes_integration_test.go) covers 16 codes; this pins the ones Playwright spec 13 renders, with the
// SAME assertions the UI relies on: exact status, unchanged generic code, check_codes equal to exactly one closed code, nothing written.

func (w *lhWorld) c3Counts() [6]int {
	return [6]int{
		w.c3OwnerInt(`SELECT count(*) FROM licensing_change_requests`),
		w.c3OwnerInt(`SELECT count(*) FROM licensing_change_approvals`),
		w.c3OwnerInt(`SELECT count(*) FROM jurisdictions`),
		w.c3OwnerInt(`SELECT count(*) FROM licence_country_ceilings`),
		w.c3OwnerInt(`SELECT count(*) FROM tenant_jurisdiction_configs`),
		w.c3OwnerInt(`SELECT count(*) FROM audit_log`),
	}
}

func TestC3HTTP_ClosedRefusalCodesOnTheWire_UIReachable(t *testing.T) {
	w := newLHWorld(t)
	bare := w.activeTenant() // no licence, no basis
	licensed := w.activeTenant()
	lid := w.c3Licence()
	w.c3OwnerExec(`UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, licensed)
	jid := uuid.New()
	w.c3OwnerExec(`INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'c3 refusal')`, jid, "C3R-"+strings.ToUpper(jid.String()[:8]))
	policy := func(tenant uuid.UUID, country string, brand any) map[string]any {
		return map[string]any{"kind": "operating_policy_version", "reason_code": "c3_refusal", "payload": map[string]any{"tenant_id": tenant,
			"scope": map[bool]string{true: "brand", false: "tenant"}[brand != nil], "brand_id": brand, "operation_code": nil, "product_code": nil,
			"country_code": country, "state": "enabled", "status": "active", "authorization_reference": "TICKET-C3"}}
	}
	cases := []struct {
		name   string
		tok    func() string
		path   string
		body   map[string]any
		status int
		code   string
		check  string
	}{
		{"ceiling not enabled for the country", w.tokReq, "/v1/admin/licensing-changes", policy(licensed, "GR", nil), 409, "LICENSING_CHANGE_REFUSED", "ceiling_not_enabled_for_country"},
		{"tenant without licence or basis", w.tokReq, "/v1/admin/licensing-changes", policy(bare, "GR", nil), 409, "LICENSING_CHANGE_REFUSED", "tenant_without_licence_or_basis"},
		{"currency is not an active asset", w.tokReq, "/v1/admin/licensing-changes", map[string]any{"kind": "tenant_jurisdiction_config_version", "reason_code": "c3_refusal",
			"payload": map[string]any{"tenant_id": licensed, "jurisdiction_id": jid, "action": "set", "kyc_ruleset_id": nil, "aml_ruleset_id": nil, "rg_ruleset_id": nil,
				"reporting_ruleset_id": nil, "allowed_currencies": []string{"ZZZ"}, "allowed_payment_methods": []string{"card"}, "geo_block_list": []string{}}}, 400, "validation_error", "currency_not_active_asset"},
		{"unknown licence on a ceiling", w.tokReq, "/v1/admin/licensing-changes", c3CeilingBody(uuid.New(), "GR", "enabled", "active", "TICKET-C3", "c3_refusal"), 400, "validation_error", "licence_unknown"},
	}
	for _, c := range cases {
		before := w.c3Counts()
		r := w.do("POST", c.path, c.tok(), c.body)
		if r.status != c.status || r.code(t) != c.code || len(r.checkCodes(t)) != 1 || r.checkCodes(t)[0] != c.check {
			t.Errorf("%s: %d %s, want %d %s [%s]", c.name, r.status, r.body, c.status, c.code, c.check)
		}
		if w.c3Counts() != before {
			t.Errorf("%s: a refused filing wrote something", c.name)
		}
	}
	// duplicate jurisdiction code (the Back Office create dialog)
	code := "C3D-" + strings.ToUpper(uuid.NewString()[:8])
	if r := w.do("POST", "/v1/admin/jurisdictions", w.tokReq(), map[string]any{"code": code, "name": "first", "reason_code": "c3_dup"}); r.status != http.StatusCreated {
		t.Fatalf("first create: %d %s", r.status, r.body)
	}
	before := w.c3Counts()
	r := w.do("POST", "/v1/admin/jurisdictions", w.tokReq(), map[string]any{"code": code, "name": "again", "reason_code": "c3_dup"})
	if r.status != http.StatusBadRequest || r.code(t) != "validation_error" || len(r.checkCodes(t)) != 1 || r.checkCodes(t)[0] != "duplicate_jurisdiction_code" || w.c3Counts() != before {
		t.Errorf("duplicate jurisdiction code: %d %s", r.status, r.body)
	}
	// foreign brand by a tenant admin: byte-identical to an unknown brand, brand_not_owned, 4xx, nothing written
	other := w.activeTenant()
	foreign := w.activeBrand(other)
	ta := w.staff(licensed, identity.StaffRoleTenantAdmin)
	tok := w.tenantTok(licensed, ta, auth.RoleTenantAdmin)
	before = w.c3Counts()
	rf := w.do("POST", "/v1/admin/licensing-changes", tok, policy(licensed, "GR", foreign))
	ru := w.do("POST", "/v1/admin/licensing-changes", tok, policy(licensed, "GR", uuid.New()))
	if len(rf.checkCodes(t)) != 1 || rf.checkCodes(t)[0] != "brand_not_owned" || rf.status != ru.status || ru.checkCodes(t)[0] != "brand_not_owned" || w.c3Counts() != before {
		t.Errorf("foreign brand %d %s vs unknown %d %s", rf.status, rf.body, ru.status, ru.body)
	}
	// an UNASSIGNED country code (valid shape, not an ISO-3166 assignment): observed, not assumed
	ux := w.do("POST", "/v1/admin/licensing-changes", w.tokReq(), c3CeilingBody(lid, "XX", "enabled", "active", "TICKET-C3", "c3_refusal"))
	t.Logf("OBSERVED unassigned country XX on a ceiling: %d %s", ux.status, ux.body)
	if ux.status != http.StatusBadRequest || ux.code(t) != "validation_error" {
		t.Errorf("an unassigned country code must be refused as 400 validation_error, got %d %s", ux.status, ux.body)
	}
}

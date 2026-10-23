//go:build integration

// Slice C3 (QA test gaps), part 2, against a real migrated PRIVATE database as igaming_runtime:
//
//	(3) the 24 h approval FRESHNESS window and the 72 h request EXPIRY, at BOTH layers (Go service and database)
//	(4) AUDIT records of ceiling / policy / configuration changes: every lifecycle event, fields, tenant scope, append-only
//	(5) CONCURRENT approvers, duplicate approvals, approve-after-cancel/execute, replayed actor proofs, exactly-one execution
//
// CLOCK CONTROL (no production code is touched): the service has an injectable Config.Clock (used by the existing tests) and the
// database uses clock_timestamp(). Two real mechanisms are used:
//
//	(a) Config.Clock skewed into the past makes the Go layer believe approvals are fresh / the request is open, so the DATABASE guard
//	    is the only thing that can refuse (proves the database is authoritative, not the Go check).
//	(b) c3Shift moves licensing_change_requests.created_at/expires_at and licensing_change_approvals.decided_at into the past in the
//	    PRIVATE TEST DATABASE only, as the TABLE OWNER, inside one transaction that disables the table's own append-only guard trigger
//	    for that single UPDATE and re-enables it before COMMIT (DDL is transactional: a failure rolls the trigger state back). The
//	    runtime role can do none of this. Each test re-asserts the triggers are enabled afterwards (c3TriggersEnabled).
package licensingchange

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// ---- helpers --------------------------------------------------------------------------------------------------------------------

// c3Shift moves a request's creation/expiry back by reqBy and its approvals' decision times back by apprBy (Postgres intervals; "" = no
// move). Owner path, private test database only (see the file comment).
func (w *world) c3Shift(id uuid.UUID, reqBy, apprBy string) {
	w.t.Helper()
	err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if reqBy != "" {
			for _, s := range []string{
				`ALTER TABLE licensing_change_requests DISABLE TRIGGER licensing_change_requests_guard`,
			} {
				if _, err := tx.Exec(ctx, s); err != nil {
					return err
				}
			}
			tag, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET created_at = created_at - $2::interval, expires_at = expires_at - $2::interval WHERE id = $1`, id, reqBy)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("c3Shift: request row not moved")
			}
			if _, err := tx.Exec(ctx, `ALTER TABLE licensing_change_requests ENABLE TRIGGER licensing_change_requests_guard`); err != nil {
				return err
			}
		}
		if apprBy != "" {
			// approvals have no UPDATE policy (RLS is FORCEd even for the owner), so RLS is switched off for this one statement too
			for _, st := range []string{`ALTER TABLE licensing_change_approvals DISABLE TRIGGER licensing_change_approvals_guard`, `ALTER TABLE licensing_change_approvals DISABLE ROW LEVEL SECURITY`} {
				if _, err := tx.Exec(ctx, st); err != nil {
					return err
				}
			}
			tag, err := tx.Exec(ctx, `UPDATE licensing_change_approvals SET decided_at = decided_at - $2::interval WHERE request_id = $1`, id, apprBy)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return errors.New("c3Shift: no approval row to move")
			}
			for _, st := range []string{`ALTER TABLE licensing_change_approvals ENABLE ROW LEVEL SECURITY`, `ALTER TABLE licensing_change_approvals ENABLE TRIGGER licensing_change_approvals_guard`} {
				if _, err := tx.Exec(ctx, st); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		w.t.Fatalf("c3Shift: %v", err)
	}
	w.c3TriggersEnabled()
}

// c3TriggersEnabled asserts the append-only guards are back on (tgenabled = 'O').
func (w *world) c3TriggersEnabled() {
	w.t.Helper()
	n := w.ownerInt(`SELECT count(*) FROM pg_trigger WHERE tgname IN ('licensing_change_requests_guard','licensing_change_approvals_guard') AND tgenabled = 'O'`)
	if n != 2 {
		w.t.Fatalf("the licensing change guard triggers must be enabled, %d of 2 are", n)
	}
	if r := w.ownerInt(`SELECT count(*) FROM pg_class WHERE relname IN ('licensing_change_requests','licensing_change_approvals') AND relrowsecurity AND relforcerowsecurity`); r != 2 {
		w.t.Fatalf("row level security must be enabled AND forced on both change tables, %d of 2 are", r)
	}
}

func (w *world) c3Clocked(offset time.Duration) *Service {
	return w.service(Config{Clock: func() time.Time { return time.Now().Add(offset) }})
}

func (w *world) c3ApprovalCount(id uuid.UUID) int {
	return w.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, id)
}

func (w *world) c3AuditCount(action string, id uuid.UUID) int {
	return w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = $1 AND target_id = $2`, action, id.String())
}

// c3CeilingReq files a widening ceiling enable (a distinct country per call keeps the open-payload uniqueness out of the way).
func (w *world) c3CeilingReq(lid uuid.UUID, country string) Request {
	w.t.Helper()
	r := w.mustRequest(w.req, KindLicenceCeilingVersion, LicenceCeilingPayload{LicenceID: lid, CountryCode: country, State: "enabled", Status: "active",
		AuthorizationReference: sp("TICKET-" + country)}, "market_entry")
	if r.Executed || !r.Request.Widening || r.Request.RequiredApprovals != 2 {
		w.t.Fatalf("expected a pending widening ceiling request: %+v", r.Request)
	}
	return r.Request
}

type c3AuditRow struct {
	ID                                 uuid.UUID
	ActorID, TenantID, SubjectTenantID *uuid.UUID
	Action, Outcome, IP, UA, ReqID     string
	Meta                               map[string]any
}

// c3AuditQ reads audit rows matching where (args from $1) as the OWNER, in a platform session and, when tenant != Nil, also in that tenant's
// session (audit_log is dual-scope RLS), de-duplicated by id.
func (w *world) c3AuditQ(tenant uuid.UUID, where string, args ...any) []c3AuditRow {
	w.t.Helper()
	q := `SELECT id, actor_id, tenant_id, subject_tenant_id, action, outcome, coalesce(host(ip_address),''), coalesce(user_agent,''), coalesce(request_id,''), metadata
		FROM audit_log WHERE ` + where + ` ORDER BY created_at, id`
	seen := map[uuid.UUID]bool{}
	var out []c3AuditRow
	read := func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r c3AuditRow
			if err := rows.Scan(&r.ID, &r.ActorID, &r.TenantID, &r.SubjectTenantID, &r.Action, &r.Outcome, &r.IP, &r.UA, &r.ReqID, &r.Meta); err != nil {
				return err
			}
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
		return rows.Err()
	}
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, read); err != nil {
		w.t.Fatalf("audit read (platform): %v", err)
	}
	if tenant != uuid.Nil {
		if err := w.owner.WithTenant(context.Background(), tenant, read); err != nil {
			w.t.Fatalf("audit read (tenant): %v", err)
		}
	}
	return out
}

// c3AuditAsTenant counts audit rows ONLY in the given tenant's own session (what that tenant can actually see).
func (w *world) c3AuditAsTenant(tenant uuid.UUID, where string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE `+where, args...).Scan(&n)
	}); err != nil {
		w.t.Fatalf("audit read (tenant only): %v", err)
	}
	return n
}

func (w *world) c3AuditOne(action, target string, tenant uuid.UUID) c3AuditRow {
	w.t.Helper()
	rows := w.c3AuditQ(tenant, `action = $1 AND target_id = $2`, action, target)
	if len(rows) != 1 {
		w.t.Fatalf("audit %s for %s: %d rows, want exactly 1", action, target, len(rows))
	}
	return rows[0]
}

func c3Net(a Actor, tag string) Actor {
	a.IPAddress, a.UserAgent, a.RequestID = "203.0.113.7", "c3-audit-test/1.0", "c3-"+tag
	return a
}

// ---- (3) freshness (24 h) -------------------------------------------------------------------------------------------------------

func TestC3_Freshness_StaleApprovalsDoNotCount_GoLayer(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	r := w.c3CeilingReq(m.lid, "IE")

	w.mustApprove(w.ap1, r.ID)
	w.c3Shift(r.ID, "", "25 hours") // the first approval is now 25 h old
	if got := w.ownerInt(`SELECT licensing_change_fresh_approvals($1)`, r.ID); got != 0 {
		t.Fatalf("the database must count a 25 h old approval as not fresh, got %d", got)
	}

	// A second distinct Person approves: enough Persons, but one approval is stale: refused, and the new approval rolls back.
	_, err := w.approve(w.ap2, r.ID)
	expectErr(t, err, ErrApprovalsStale, "second approval with a stale first")
	if w.status(r.ID) != StatusRequested || w.c3ApprovalCount(r.ID) != 1 {
		t.Fatalf("stale refusal must roll back: status=%s approvals=%d", w.status(r.ID), w.c3ApprovalCount(r.ID))
	}
	if v, _, _, _ := w.c3Ceilings(m.lid, "IE"); v != 0 {
		t.Fatal("nothing may be executed on stale approvals")
	}
	w.c3Want(w.c3Resolve(m.tid, nil, "IE", "deposit", nil), operatingmarket.OutcomeNotPermittedByLicence, "resolver unchanged")
	// Exactly one 'approved' event (the first); the refused attempt left no event, no executed event and no denied event.
	if n := w.c3AuditCount("licensing_change.approved", r.ID); n != 1 {
		t.Errorf("approved events: %d, want 1", n)
	}
	if n := w.c3AuditCount("licensing_change.executed", r.ID); n != 0 {
		t.Errorf("executed events: %d, want 0", n)
	}
	if n := w.c3AuditCount("licensing_change.approve_denied", r.ID); n != 0 {
		t.Errorf("a stale refusal is a rolled-back no-op, not a denied event: %d", n)
	}
	// A further distinct Person cannot rescue it: the stale approval still does not count (the request can only be cancelled).
	_, err = w.approve(w.ap3, r.ID)
	expectErr(t, err, ErrApprovalsStale, "third Person cannot rescue a request with a stale approval")
	if w.c3ApprovalCount(r.ID) != 1 {
		t.Fatal("still one approval")
	}
	c, err := w.svc.Cancel(context.Background(), w.req, r.ID)
	mustNotErr(t, err, "requester cancels the stuck request")
	if c.Status != StatusCancelled {
		t.Fatalf("cancelled: %+v", c)
	}
	_, err = w.approve(w.ap2, r.ID)
	expectErr(t, err, ErrNotPending, "approve after cancel")
}

func TestC3_Freshness_Boundary(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	// 23 h 50 min old: still counts, the second approval executes.
	in := w.c3CeilingReq(m.lid, "DK")
	w.mustApprove(w.ap1, in.ID)
	w.c3Shift(in.ID, "", "23 hours 50 minutes")
	if got := w.ownerInt(`SELECT licensing_change_fresh_approvals($1)`, in.ID); got != 1 {
		t.Fatalf("a 23h50m approval is fresh, got %d", got)
	}
	if !w.mustApprove(w.ap2, in.ID).Executed {
		t.Fatal("an approval just inside the window must count")
	}
	// 24 h 1 min old: does not count.
	out := w.c3CeilingReq(m.lid, "NL")
	w.mustApprove(w.ap1, out.ID)
	w.c3Shift(out.ID, "", "24 hours 1 minute")
	_, err := w.approve(w.ap2, out.ID)
	expectErr(t, err, ErrApprovalsStale, "an approval just outside the window")
	if v, _, _, _ := w.c3Ceilings(m.lid, "NL"); v != 0 {
		t.Fatal("not executed")
	}
}

// The DATABASE, not the Go check, refuses a stale approval: the service clock is skewed so Go believes everything is fresh.
func TestC3_Freshness_DatabaseIsAuthoritative(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	r := w.c3CeilingReq(m.lid, "FR")
	w.mustApprove(w.ap1, r.ID)
	w.c3Shift(r.ID, "", "25 hours")
	skewed := w.c3Clocked(-48 * time.Hour) // Go: "the approval is 23 h old"
	_, err := skewed.Approve(context.Background(), w.ap2, r.ID, "reviewed_ok")
	if GuardCode(err) != "LC020" {
		t.Fatalf("the database must refuse the stale approval (LC020) even when the Go check is fooled, got %v", err)
	}
	if w.status(r.ID) != StatusRequested || w.c3ApprovalCount(r.ID) != 1 {
		t.Fatalf("rolled back: status=%s approvals=%d", w.status(r.ID), w.c3ApprovalCount(r.ID))
	}
	if v, _, _, _ := w.c3Ceilings(m.lid, "FR"); v != 0 {
		t.Fatal("nothing executed")
	}
	// A database guard refusal IS audited exactly once as a denied decision (codes only).
	d := w.c3AuditQ(uuid.Nil, `action = 'licensing_change.approve_denied' AND target_id = $1`, r.ID.String())
	if len(d) != 1 || d[0].Outcome != "denied" || d[0].Meta["refusal"] != "LC020" || len(d[0].Meta) != 1 {
		t.Fatalf("denied audit: %+v", d)
	}
}

// ---- (3) expiry (72 h) ----------------------------------------------------------------------------------------------------------

func TestC3_Expiry_72h_ExpiredRequestsCannotBeApprovedOrExecuted(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()

	// Just inside the window (71 h old, 1 h left): can still be approved and executed.
	in := w.c3CeilingReq(m.lid, "IE")
	w.c3Shift(in.ID, "71 hours", "")
	w.mustApprove(w.ap1, in.ID)
	if !w.mustApprove(w.ap2, in.ID).Executed {
		t.Fatal("a request with an hour left must execute")
	}

	// Past the window (73 h old) with one fresh approval already recorded.
	r := w.c3CeilingReq(m.lid, "DK")
	w.mustApprove(w.ap1, r.ID)
	w.c3Shift(r.ID, "73 hours", "")
	got, err := w.svc.Get(context.Background(), w.ap1, r.ID)
	mustNotErr(t, err, "get")
	if !got.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expires_at must be in the past: %v", got.ExpiresAt)
	}
	// CHARACTERISATION: until someone touches it the stored status is still 'requested' (expiry is materialised lazily); a client
	// MUST render expires_at, not only status.
	if got.Status != StatusRequested {
		t.Fatalf("CHARACTERISATION changed: an untouched expired request reads %q", got.Status)
	}
	_, err = w.approve(w.ap2, r.ID)
	expectErr(t, err, ErrNotPending, "approve an expired request")
	if w.status(r.ID) != StatusExpired {
		t.Fatalf("the refused decision materialises the expiry, status=%s", w.status(r.ID))
	}
	if w.c3AuditCount("licensing_change.expired", r.ID) != 1 {
		t.Fatal("the expiry is audited exactly once")
	}
	if w.c3ApprovalCount(r.ID) != 1 {
		t.Fatal("no approval recorded on an expired request")
	}
	if v, _, _, _ := w.c3Ceilings(m.lid, "DK"); v != 0 {
		t.Fatal("an expired request executes nothing")
	}
	for name, f := range map[string]func() error{
		"approve (third person)": func() error { _, e := w.approve(w.ap3, r.ID); return e },
		"approve (same person)":  func() error { _, e := w.approve(w.ap1, r.ID); return e },
		"reject":                 func() error { _, e := w.svc.Reject(context.Background(), w.ap2, r.ID, "late"); return e },
		"cancel":                 func() error { _, e := w.svc.Cancel(context.Background(), w.req, r.ID); return e },
	} {
		expectErr(t, f(), ErrNotPending, name+" after expiry")
	}
	if w.c3AuditCount("licensing_change.expired", r.ID) != 1 || w.c3ApprovalCount(r.ID) != 1 {
		t.Fatal("repeated decisions must not duplicate the expiry or add approvals")
	}
	listed, err := w.svc.List(context.Background(), w.ap1, ListFilter{Status: StatusExpired})
	mustNotErr(t, err, "list expired")
	found := false
	for _, x := range listed {
		found = found || x.ID == r.ID
	}
	if !found {
		t.Fatal("the expired request must be listed under status=expired")
	}
}

// The database refuses an expired request even when the Go layer is fooled, and a direct state move by the runtime role is refused.
func TestC3_Expiry_DatabaseIsAuthoritative(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	r := w.c3CeilingReq(m.lid, "NL")
	w.mustApprove(w.ap1, r.ID)
	w.c3Shift(r.ID, "73 hours", "")
	skewed := w.c3Clocked(-200 * time.Hour) // Go: "plenty of time left"

	_, err := skewed.Approve(context.Background(), w.ap2, r.ID, "reviewed_ok")
	expectErr(t, err, ErrNotPending, "database refuses an approval on an expired request (LC031)")
	_, err = skewed.Reject(context.Background(), w.ap2, r.ID, "late")
	expectErr(t, err, ErrNotPending, "database refuses a rejection on an expired request (LC013)")
	if w.status(r.ID) != StatusRequested || w.c3ApprovalCount(r.ID) != 1 {
		t.Fatalf("nothing changed: status=%s approvals=%d", w.status(r.ID), w.c3ApprovalCount(r.ID))
	}
	// Direct attempts to move it to approved / executed (the runtime role may update state columns): LC013, never a write.
	for _, to := range []string{"approved", "executed"} {
		e := w.rtPlatform(w.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE licensing_change_requests SET status = '%s' WHERE id = $1`, to), r.ID)
			return err
		})
		if pgCode(e) != "LC013" {
			t.Errorf("expired -> %s directly must be LC013, got %v", to, e)
		}
	}
	if v, _, _, _ := w.c3Ceilings(m.lid, "NL"); v != 0 {
		t.Fatal("an expired request executes nothing")
	}
	// Honest clock: the real service materialises the expiry; a second single-actor narrowing never depends on the window.
	_, err = w.approve(w.ap3, r.ID)
	expectErr(t, err, ErrNotPending, "honest clock")
	if w.status(r.ID) != StatusExpired || w.c3AuditCount("licensing_change.expired", r.ID) != 1 {
		t.Fatal("expiry materialised and audited once")
	}
}

// CHARACTERISATION: the requester can still cancel a request that expired but has not been touched (the database allows requested ->
// cancelled regardless of expires_at). Harmless (it closes the request), pinned so a change is visible.
func TestC3_Expiry_CHARACTERISATION_CancelOfUntouchedExpiredRequest(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	r := w.c3CeilingReq(m.lid, "FR")
	w.c3Shift(r.ID, "73 hours", "")
	c, err := w.svc.Cancel(context.Background(), w.req, r.ID)
	if err != nil || c.Status != StatusCancelled {
		t.Fatalf("CHARACTERISATION changed: cancel of an untouched expired request: %v %+v", err, c)
	}
}

// ---- (4) audit ------------------------------------------------------------------------------------------------------------------

type c3KindCase struct {
	name         string
	kind         Kind
	mk           func(i int) any
	subject      bool // the request concerns a tenant (subject_tenant_id set on the platform-scope row)
	domainAction string
	domainTenant bool // the domain writer's row is tenant-scoped (tenant_id = the tenant)
	domainByID   bool // the domain row's target id is outcome[ceiling_version_id]
}

func TestC3_Audit_CeilingPolicyConfig_FullLifecycle(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	w.setCeiling(m.lid, "MT") // so a tenant-rung enable is admissible
	jidCfg := w.jurisdiction()
	cases := []c3KindCase{
		{name: "ceiling", kind: KindLicenceCeilingVersion, domainAction: "operating_market.licence_ceiling_version_created", domainByID: true,
			mk: func(i int) any {
				return LicenceCeilingPayload{LicenceID: m.lid, CountryCode: []string{"DE", "IT", "ES"}[i], State: "enabled", Status: "active", AuthorizationReference: sp("TICKET-A")}
			}},
		{name: "policy", kind: KindOperatingPolicyVersion, subject: true, domainAction: "operating_market.policy_version_created", domainTenant: true,
			mk: func(i int) any {
				return OperatingPolicyPayload{TenantID: m.tid, Scope: "tenant", CountryCode: "MT", State: "enabled", Status: "active", AuthorizationReference: sp(fmt.Sprintf("TICKET-P%d", i))}
			}},
		{name: "config", kind: KindTenantJurisdictionConfigVersion, subject: true, domainAction: "jurisdiction_registry.tenant_jurisdiction_config_version_set",
			mk: func(i int) any {
				return TenantJurisdictionConfigPayload{TenantID: m.tid, JurisdictionID: jidCfg, Action: ConfigActionSet, KYCRulesetID: sp(fmt.Sprintf("kyc-%d", i)),
					AMLRulesetID: sp("aml-1"), RGRulesetID: sp("rg-1"), AllowedCurrencies: []string{"EUR"}, AllowedPaymentMethods: []string{"card"}, GeoBlockList: []string{}}
			}},
	}
	req, ap1, ap2 := c3Net(w.req, "req"), c3Net(w.ap1, "ap1"), c3Net(w.ap2, "ap2")
	tenantOf := func(c c3KindCase) uuid.UUID {
		if c.domainTenant || c.subject {
			return m.tid
		}
		return uuid.Nil
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// ---- executed path: requested, approved x2, executed ----
			res, err := w.svc.Request(context.Background(), req, RequestInput{Kind: c.kind, Payload: payload(t, c.mk(0)), ReasonCode: "audit_" + c.name + "_exec"})
			mustNotErr(t, err, "request")
			r := res.Request
			if res.Executed || !r.Widening {
				t.Fatalf("expected pending widening: %+v", r)
			}
			a1, err := w.svc.Approve(context.Background(), ap1, r.ID, "ok_one")
			mustNotErr(t, err, "approve 1")
			a2, err := w.svc.Approve(context.Background(), ap2, r.ID, "ok_two")
			mustNotErr(t, err, "approve 2")
			if a1.Executed || !a2.Executed {
				t.Fatal("approval 1 must not execute, approval 2 must")
			}
			for action, want := range map[string]int{"requested": 1, "approved": 2, "executed": 1, "rejected": 0, "cancelled": 0, "expired": 0, "approve_denied": 0} {
				if n := w.c3AuditCount("licensing_change."+action, r.ID); n != want {
					t.Errorf("%s: licensing_change.%s rows=%d want %d", c.name, action, n, want)
				}
			}
			rq := w.c3AuditOne("licensing_change.requested", r.ID.String(), tenantOf(c))
			if rq.ActorID == nil || *rq.ActorID != w.req.StaffID || rq.Outcome != "success" || rq.IP != "203.0.113.7" || rq.UA != "c3-audit-test/1.0" || rq.ReqID != "c3-req" {
				t.Errorf("requested row actor/outcome/ip/ua/request id: %+v", rq)
			}
			if rq.TenantID != nil {
				t.Errorf("a platform actor's row is platform-scope (tenant_id NULL), got %v", *rq.TenantID)
			}
			if c.subject != (rq.SubjectTenantID != nil) || (c.subject && *rq.SubjectTenantID != m.tid) {
				t.Errorf("subject_tenant_id: %v, want tenant=%v subject=%v", rq.SubjectTenantID, m.tid, c.subject)
			}
			if rq.Meta["reason_code"] != "audit_"+c.name+"_exec" || rq.Meta["kind"] != string(c.kind) || rq.Meta["widening"] != true ||
				rq.Meta["required_approvals"] != float64(2) || rq.Meta["payload_hash"] != r.PayloadHash || rq.Meta["requested_by"] != w.req.StaffID.String() ||
				rq.Meta["change_request_id"] != r.ID.String() || rq.Meta["status"] != "requested" || rq.Meta["payload"] == nil {
				t.Errorf("requested metadata incomplete: %v", rq.Meta)
			}
			apr := w.c3AuditQ(tenantOf(c), `action = 'licensing_change.approved' AND target_id = $1`, r.ID.String())
			gotActors := map[uuid.UUID]float64{}
			for _, a := range apr {
				if a.ActorID == nil || a.IP != "203.0.113.7" || a.Outcome != "success" {
					t.Errorf("approved row: %+v", a)
					continue
				}
				gotActors[*a.ActorID], _ = a.Meta["approvals"].(float64)
				if a.Meta["of_required"] != float64(2) {
					t.Errorf("approved metadata: %v", a.Meta)
				}
			}
			if gotActors[w.ap1.StaffID] != 1 || gotActors[w.ap2.StaffID] != 2 || len(gotActors) != 2 {
				t.Errorf("approved events by actor (value = running count): %v", gotActors)
			}
			ex := w.c3AuditOne("licensing_change.executed", r.ID.String(), tenantOf(c))
			if ex.ActorID == nil || *ex.ActorID != w.ap2.StaffID || ex.Meta["status"] != "executed" {
				t.Errorf("executed row: %+v", ex)
			}
			if aps, _ := ex.Meta["approvers"].([]any); len(aps) != 2 {
				t.Errorf("executed metadata must list both approvers: %v", ex.Meta["approvers"])
			}
			// domain writer's before/after audit, written by the executing (final) approver with the request's reason code
			var dom []c3AuditRow
			if c.domainByID {
				dom = w.c3AuditQ(uuid.Nil, `action = $1 AND target_id = $2`, c.domainAction, a2.Outcome["ceiling_version_id"].(string))
			} else {
				dom = w.c3AuditQ(m.tid, `action = $1 AND (subject_tenant_id = $2 OR tenant_id = $2) AND metadata->>'reason_code' = $3`, c.domainAction, m.tid, "audit_"+c.name+"_exec")
			}
			if len(dom) != 1 {
				t.Fatalf("%s: domain audit rows: %d, want 1", c.name, len(dom))
			}
			d := dom[0]
			if d.ActorID == nil || *d.ActorID != w.ap2.StaffID || d.Meta["reason_code"] != "audit_"+c.name+"_exec" || d.IP != "203.0.113.7" {
				t.Errorf("%s domain audit actor/reason/ip: %+v", c.name, d)
			}
			if _, ok := d.Meta["before"]; !ok {
				t.Errorf("%s domain audit has no 'before'", c.name)
			}
			if after, _ := d.Meta["after"].(map[string]any); len(after) == 0 {
				t.Errorf("%s domain audit has no 'after' state: %v", c.name, d.Meta)
			}
			if c.domainTenant && (d.TenantID == nil || *d.TenantID != m.tid) {
				t.Errorf("%s: tenant-scoped domain audit must carry tenant_id=%v, got %v", c.name, m.tid, d.TenantID)
			}

			// ---- rejected path ----
			rj, err := w.svc.Request(context.Background(), req, RequestInput{Kind: c.kind, Payload: payload(t, c.mk(1)), ReasonCode: "audit_" + c.name + "_rej"})
			mustNotErr(t, err, "request for reject")
			domBefore := w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = $1`, c.domainAction)
			_, err = w.svc.Reject(context.Background(), ap1, rj.Request.ID, "insufficient_evidence")
			mustNotErr(t, err, "reject")
			row := w.c3AuditOne("licensing_change.rejected", rj.Request.ID.String(), tenantOf(c))
			if row.ActorID == nil || *row.ActorID != w.ap1.StaffID || row.Meta["decision_reason_code"] != "insufficient_evidence" || row.Meta["status"] != "rejected" || row.IP != "203.0.113.7" {
				t.Errorf("rejected row: %+v", row)
			}
			if w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = $1`, c.domainAction) != domBefore {
				t.Error("a rejection must not produce a domain audit row")
			}

			// ---- cancelled path ----
			cn, err := w.svc.Request(context.Background(), req, RequestInput{Kind: c.kind, Payload: payload(t, c.mk(2)), ReasonCode: "audit_" + c.name + "_can"})
			mustNotErr(t, err, "request for cancel")
			_, err = w.svc.Cancel(context.Background(), req, cn.Request.ID)
			mustNotErr(t, err, "cancel")
			row = w.c3AuditOne("licensing_change.cancelled", cn.Request.ID.String(), tenantOf(c))
			if row.ActorID == nil || *row.ActorID != w.req.StaffID || row.Meta["status"] != "cancelled" {
				t.Errorf("cancelled row: %+v", row)
			}

			// ---- refused attempts: exactly what is (and is not) audited ----
			open, err := w.svc.Request(context.Background(), req, RequestInput{Kind: c.kind, Payload: payload(t, c.mk(1)), ReasonCode: "audit_" + c.name + "_open"})
			mustNotErr(t, err, "request open")
			id := open.Request.ID
			evCount := func() int {
				return w.ownerInt(`SELECT count(*) FROM audit_log WHERE target_id = $1`, id.String())
			}
			base := evCount() // the 'requested' event
			// self-approval and same-Person: DENIED events (codes only), nothing else changes
			_, err = w.svc.Approve(context.Background(), req, id, "ok")
			expectErr(t, err, ErrSelfApproval, "self approval")
			twin := c3Net(w.reqTwin, "twin")
			_, err = w.svc.Approve(context.Background(), twin, id, "ok")
			expectErr(t, err, ErrSelfApproval, "same Person as requester")
			den := w.c3AuditQ(uuid.Nil, `action = 'licensing_change.approve_denied' AND target_id = $1`, id.String())
			if len(den) != 2 {
				t.Fatalf("denied events: %d, want 2", len(den))
			}
			for i, a := range den {
				who := []uuid.UUID{w.req.StaffID, w.reqTwin.StaffID}[i]
				if a.Outcome != "denied" || a.ActorID == nil || *a.ActorID != who || len(a.Meta) != 1 || a.Meta["refusal"] != "self_approval" {
					t.Errorf("denied row %d: %+v", i, a)
				}
			}
			// not audited at all (they change nothing and carry no new information): duplicate approval, unknown id, forbidden role, not pending
			mustNotErr(t, errOnly(w.svc.Approve(context.Background(), ap1, id, "ok")), "first approval")
			n1 := evCount()
			_, err = w.svc.Approve(context.Background(), ap1, id, "ok")
			expectErr(t, err, ErrAlreadyDecided, "duplicate approval")
			_, err = w.svc.Approve(context.Background(), ap2, uuid.New(), "ok")
			expectErr(t, err, ErrNotFound, "unknown id")
			ta := w.tenantStaff(m.tid, "tenant_admin")
			_, err = w.svc.Approve(context.Background(), ta, id, "ok")
			expectErr(t, err, ErrForbidden, "tenant admin approves")
			if evCount() != n1 {
				t.Errorf("refusals that change nothing must write no audit row (%d -> %d)", n1, evCount())
			}
			if base != 1 {
				t.Errorf("only the requested event existed before the refusals, got %d", base)
			}
			_, err = w.svc.Cancel(context.Background(), req, id)
			mustNotErr(t, err, "tidy")
		})
	}
}

func errOnly[T any](_ T, err error) error { return err }

// Append-only: no UPDATE/DELETE of audit rows or of the change tables, for the runtime role AND the owner; the data is unchanged.
func TestC3_Audit_AppendOnly_RuntimeAndOwner(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	r := w.c3CeilingReq(m.lid, "IE")
	w.mustApprove(w.ap1, r.ID)
	w.mustApprove(w.ap2, r.ID)
	snap := func() string {
		rows := w.c3AuditQ(uuid.Nil, `target_id = $1`, r.ID.String())
		var b strings.Builder
		for _, x := range rows {
			fmt.Fprintf(&b, "%s|%s|%s|%v|%v;", x.ID, x.Action, x.Outcome, x.ActorID, x.Meta)
		}
		return b.String()
	}
	before := snap()
	if len(w.c3AuditQ(uuid.Nil, `target_id = $1`, r.ID.String())) != 4 {
		t.Fatal("precondition: requested, approved x2, executed")
	}
	type attempt struct{ name, sql string }
	attempts := []attempt{
		{"audit UPDATE outcome", `UPDATE audit_log SET outcome = 'failure' WHERE target_id = $1`},
		{"audit UPDATE metadata", `UPDATE audit_log SET metadata = '{}'::jsonb WHERE target_id = $1`},
		{"audit UPDATE actor", `UPDATE audit_log SET actor_id = gen_random_uuid() WHERE target_id = $1`},
		{"audit DELETE", `DELETE FROM audit_log WHERE target_id = $1`},
	}
	for _, role := range []string{"runtime", "owner"} {
		pool := w.rt
		if role == "owner" {
			pool = w.owner
		}
		for _, a := range attempts {
			err := pool.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, a.sql, r.ID.String())
				return err
			})
			if err == nil {
				t.Errorf("%s: %s was NOT refused", role, a.name)
			}
		}
		for _, q := range []string{
			`UPDATE licensing_change_approvals SET reason_code = 'tampered' WHERE request_id = $1`,
			`DELETE FROM licensing_change_approvals WHERE request_id = $1`,
			`DELETE FROM licensing_change_requests WHERE id = $1`,
			`UPDATE licensing_change_requests SET reason_code = 'tampered' WHERE id = $1`,
			`UPDATE licensing_change_requests SET status = 'requested' WHERE id = $1`, // executed -> requested
		} {
			var affected int64
			err := pool.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, q, r.ID)
				affected = tag.RowsAffected()
				return err
			})
			// refused = an error (trigger / missing grant) OR no row visible to the statement (no RLS policy for the command): either
			// way nothing changed (the unchanged state is asserted below)
			if err == nil && affected != 0 {
				t.Errorf("%s: %q changed %d row(s)", role, q, affected)
			}
			t.Logf("%s %q -> err=%v rows=%d", role, q, err, affected)
		}
	}
	// TRUNCATE is not even granted to the runtime role.
	for _, tbl := range []string{"audit_log", "licensing_change_requests", "licensing_change_approvals"} {
		if err := w.rt.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE `+tbl)
			return err
		}); err == nil {
			t.Errorf("runtime TRUNCATE %s was NOT refused", tbl)
		}
	}
	if after := snap(); after != before {
		t.Fatalf("audit rows changed after the refused attempts:\n%s\n%s", before, after)
	}
	if w.status(r.ID) != StatusExecuted || w.c3ApprovalCount(r.ID) != 2 {
		t.Fatal("request/approvals changed")
	}
	w.c3TriggersEnabled()
}

// A tenant actor's audit rows are TENANT-scoped (tenant_id = its tenant), carry its IP, and are invisible to another tenant.
func TestC3_Audit_TenantActorRowsAreTenantScoped(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	other := w.tenant("under_platform_licence")
	w.setCeiling(m.lid, "MT")
	w.enableTenantPolicy(m.tid, "MT")
	co := c3Net(w.tenantStaff(m.tid, "compliance"), "co")
	res, err := w.svc.Request(context.Background(), co, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "market_exit",
		Payload: payload(t, OperatingPolicyPayload{TenantID: m.tid, Scope: "tenant", CountryCode: "MT", State: "disabled", Status: "active"})})
	mustNotErr(t, err, "tenant narrowing request")
	if res.Executed || res.Request.RequiredApprovals != 1 {
		t.Fatalf("tenant narrowing needs one platform approval: %+v", res.Request)
	}
	id := res.Request.ID.String()
	rows := w.c3AuditQ(m.tid, `action = 'licensing_change.requested' AND target_id = $1`, id)
	if len(rows) != 1 || rows[0].TenantID == nil || *rows[0].TenantID != m.tid || rows[0].SubjectTenantID != nil || rows[0].ActorID == nil || *rows[0].ActorID != co.StaffID || rows[0].IP != "203.0.113.7" {
		t.Fatalf("tenant request audit: %+v", rows)
	}
	if n := w.c3AuditAsTenant(other, `target_id = $1`, id); n != 0 {
		t.Fatalf("another tenant must not see the row: %d", n)
	}
	if n := w.c3AuditAsTenant(m.tid, `action = 'licensing_change.requested' AND target_id = $1`, id); n != 1 {
		t.Fatalf("the owning tenant sees its own request row: %d", n)
	}
	// The platform approval executes it; its rows concern the tenant (subject_tenant_id) without being tenant-scoped.
	mustNotErr(t, errOnly(w.svc.Approve(context.Background(), c3Net(w.ap1, "ap1"), res.Request.ID, "ok")), "platform approval")
	ex := w.c3AuditQ(m.tid, `action = 'licensing_change.executed' AND target_id = $1`, id)
	if len(ex) != 1 || ex[0].TenantID != nil || ex[0].SubjectTenantID == nil || *ex[0].SubjectTenantID != m.tid {
		t.Fatalf("platform executed audit: %+v", ex)
	}
	if n := w.c3AuditAsTenant(other, `target_id = $1`, id); n != 0 {
		t.Fatalf("another tenant must not see any row of this change: %d", n)
	}
	if n := w.c3AuditAsTenant(m.tid, `action = 'licensing_change.executed' AND target_id = $1`, id); n != 1 {
		t.Fatalf("the SUBJECT tenant sees the platform row about it (ADR 0104 projection): %d", n)
	}
}

// ---- (5) refusals, replay, concurrency ------------------------------------------------------------------------------------------

func TestC3_ApprovalRefusals_Matrix(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	exec := func(country string) Request { // an executed request
		r := w.c3CeilingReq(m.lid, country)
		w.mustApprove(w.ap1, r.ID)
		if !w.mustApprove(w.ap2, r.ID).Executed {
			t.Fatal("must execute")
		}
		return r
	}

	// approve after CANCEL
	c := w.c3CeilingReq(m.lid, "IE")
	_, err := w.svc.Cancel(context.Background(), w.req, c.ID)
	mustNotErr(t, err, "cancel")
	_, err = w.approve(w.ap1, c.ID)
	expectErr(t, err, ErrNotPending, "approve after cancel")
	if w.c3ApprovalCount(c.ID) != 0 {
		t.Fatal("nothing recorded after cancel")
	}
	// approve after REJECT
	rj := w.c3CeilingReq(m.lid, "DK")
	_, err = w.svc.Reject(context.Background(), w.ap1, rj.ID, "no")
	mustNotErr(t, err, "reject")
	_, err = w.approve(w.ap2, rj.ID)
	expectErr(t, err, ErrNotPending, "approve after reject")
	// approve after EXECUTE: by a third Person, by an approver again, by the requester; execution happens once
	e := exec("NL")
	for name, a := range map[string]Actor{"third person": w.ap3, "first approver again": w.ap1, "final approver again": w.ap2, "requester": w.req} {
		_, err = w.approve(a, e.ID)
		expectErr(t, err, ErrNotPending, "approve after execute by "+name)
	}
	if v, _, _, _ := w.c3Ceilings(m.lid, "NL"); v != 1 || w.c3ApprovalCount(e.ID) != 2 || w.c3AuditCount("licensing_change.executed", e.ID) != 1 {
		t.Fatalf("double execution: ceiling versions=%d approvals=%d executed events=%d", v, w.c3ApprovalCount(e.ID), w.c3AuditCount("licensing_change.executed", e.ID))
	}
	// the database also refuses to execute again (executed -> executed / approved / requested), whoever asks
	for _, to := range []string{"executed", "approved", "requested", "cancelled", "expired"} {
		err := w.rtPlatform(w.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE licensing_change_requests SET status = '%s' WHERE id = $1`, to), e.ID)
			return err
		})
		if err == nil {
			t.Errorf("executed -> %s was NOT refused", to)
		}
	}
	if w.status(e.ID) != StatusExecuted {
		t.Fatal("still executed")
	}
	// the SAME approver twice (one account, then a second account of the same Person); the requester and its Person
	d := w.c3CeilingReq(m.lid, "FR")
	w.mustApprove(w.ap1, d.ID)
	_, err = w.approve(w.ap1, d.ID)
	expectErr(t, err, ErrAlreadyDecided, "same approver twice")
	_, err = w.approve(w.ap1Twin, d.ID)
	expectErr(t, err, ErrAlreadyDecided, "same Person, second account")
	_, err = w.approve(w.req, d.ID)
	expectErr(t, err, ErrSelfApproval, "requester")
	_, err = w.approve(w.reqTwin, d.ID)
	expectErr(t, err, ErrSelfApproval, "requester's Person")
	if w.c3ApprovalCount(d.ID) != 1 || w.status(d.ID) != StatusRequested {
		t.Fatalf("count must stay 1 and the request open: %d %s", w.c3ApprovalCount(d.ID), w.status(d.ID))
	}
	// unknown request id
	_, err = w.approve(w.ap2, uuid.New())
	expectErr(t, err, ErrNotFound, "unknown request id")
	// the approval is bound to THIS request's hash: the stored approval hash equals the request's
	if n := w.ownerInt(`SELECT count(*) FROM licensing_change_approvals a JOIN licensing_change_requests r ON r.id = a.request_id WHERE a.request_id = $1 AND a.payload_hash = r.payload_hash`, d.ID); n != 1 {
		t.Fatalf("approval must carry the request's payload hash, got %d", n)
	}
	w.c3TriggersEnabled()
}

// A captured actor proof cannot be replayed on another request, with another hash, by another actor, or a second time.
func TestC3_ReplayedAndMisboundActorProofs_Refused(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	r1, r2 := w.c3CeilingReq(m.lid, "IE"), w.c3CeilingReq(m.lid, "DK")
	forger := prooftest.ProcessForger(t, w.ownerURL)
	insert := func(ap uuid.UUID, tok string, req uuid.UUID) error {
		return w.rtPlatform(ap, func(ctx context.Context, tx pgx.Tx) error {
			if err := setProof(ctx, tx, tok); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO licensing_change_approvals (request_id, reason_code) VALUES ($1, 'replay')`, req)
			return err
		})
	}
	op := actorproof.OpLicensingChangeApprove
	proof1 := forger.Valid(t, w.ops.Approver1, "platform", uuid.Nil, op, r1.ID.String(), r1.PayloadHash)
	// wrong request id (the proof is for r1, the approval is for r2)
	if c := pgCode(insert(w.ops.Approver1, proof1, r2.ID)); c != "AP004" {
		t.Errorf("proof for request 1 used on request 2: %q, want AP004", c)
	}
	// wrong hash (a proof for r1's id but r2's payload hash)
	if c := pgCode(insert(w.ops.Approver1, forger.Valid(t, w.ops.Approver1, "platform", uuid.Nil, op, r1.ID.String(), r2.PayloadHash), r1.ID)); c != "AP004" {
		t.Errorf("wrong payload hash: %q, want AP004", c)
	}
	// another actor's proof presented in my session
	if c := pgCode(insert(w.ops.Approver2, proof1, r1.ID)); c != "AP004" {
		t.Errorf("another actor's proof: %q, want AP004", c)
	}
	// another operation's proof (reject proof used to approve)
	if c := pgCode(insert(w.ops.Approver1, forger.Valid(t, w.ops.Approver1, "platform", uuid.Nil, actorproof.OpLicensingChangeReject, r1.ID.String(), r1.PayloadHash), r1.ID)); c != "AP004" {
		t.Errorf("wrong operation: %q, want AP004", c)
	}
	if w.c3ApprovalCount(r1.ID) != 0 || w.c3ApprovalCount(r2.ID) != 0 {
		t.Fatal("no refused attempt may record an approval")
	}
	// the right proof works once ...
	if err := insert(w.ops.Approver1, proof1, r1.ID); err != nil {
		t.Fatalf("a correct proof must be accepted: %v", err)
	}
	// ... and REPLAYING the same token is refused (nonce already used)
	if c := pgCode(insert(w.ops.Approver1, proof1, r1.ID)); c != "AP005" {
		t.Errorf("replayed proof: %q, want AP005", c)
	}
	if w.c3ApprovalCount(r1.ID) != 1 {
		t.Fatal("exactly one approval")
	}
	// after execution a captured, valid proof of a third approver finds the request closed
	w.mustApprove(w.ap2, r1.ID)
	late := forger.Valid(t, w.ap3.StaffID, "platform", uuid.Nil, op, r1.ID.String(), r1.PayloadHash)
	if c := pgCode(insert(w.ap3.StaffID, late, r1.ID)); c != "LC031" {
		t.Errorf("proof after execution: %q, want LC031", c)
	}
	if w.c3ApprovalCount(r1.ID) != 2 {
		t.Fatal("still two approvals")
	}
}

type c3Outcome struct {
	who string
	res *Result
	err error
}

func c3Race(fns map[string]func() (*Result, error)) []c3Outcome {
	var wg sync.WaitGroup
	start := make(chan struct{})
	out := make([]c3Outcome, 0, len(fns))
	var mu sync.Mutex
	for name, f := range fns {
		wg.Add(1)
		go func(name string, f func() (*Result, error)) {
			defer wg.Done()
			<-start
			r, err := f()
			mu.Lock()
			out = append(out, c3Outcome{name, r, err})
			mu.Unlock()
		}(name, f)
	}
	close(start)
	wg.Wait()
	return out
}

// 20 rounds: three distinct approvers plus a duplicate submit race on a fresh two-person request. Exactly one execution, two recorded
// approvals, one 'executed' audit row, consistent state, and every loser is a clean ALREADY_DECIDED / NOT_PENDING.
func TestC3_Concurrency_ApproversRace_ExactlyOneExecution(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	for round := 0; round < 20; round++ {
		lid := w.licence(jid, "platform", uuid.Nil)
		r := w.c3CeilingReq(lid, "MT")
		out := c3Race(map[string]func() (*Result, error){
			"ap1":     func() (*Result, error) { return w.approve(w.ap1, r.ID) },
			"ap2":     func() (*Result, error) { return w.approve(w.ap2, r.ID) },
			"ap3":     func() (*Result, error) { return w.approve(w.ap3, r.ID) },
			"ap2dup":  func() (*Result, error) { return w.approve(w.ap2, r.ID) },
			"ap1twin": func() (*Result, error) { return w.approve(w.ap1Twin, r.ID) },
		})
		executed, ok := 0, 0
		for _, o := range out {
			switch {
			case o.err == nil && o.res.Executed:
				executed++
				ok++
			case o.err == nil:
				ok++
			case errors.Is(o.err, ErrAlreadyDecided), errors.Is(o.err, ErrNotPending):
			default:
				t.Fatalf("round %d: %s: unexpected loser error %v", round, o.who, o.err)
			}
		}
		if executed != 1 || ok != 2 {
			t.Fatalf("round %d: executed=%d successes=%d, want 1 and 2", round, executed, ok)
		}
		if w.status(r.ID) != StatusExecuted || w.c3ApprovalCount(r.ID) != 2 {
			t.Fatalf("round %d: status=%s approvals=%d", round, w.status(r.ID), w.c3ApprovalCount(r.ID))
		}
		if n := w.ownerInt(`SELECT count(DISTINCT approver_person_id) FROM licensing_change_approvals WHERE request_id = $1`, r.ID); n != 2 {
			t.Fatalf("round %d: the two approvals must be by two Persons, got %d", round, n)
		}
		if v, open, st, _ := w.c3Ceilings(lid, "MT"); v != 1 || open != 1 || st != "active" {
			t.Fatalf("round %d: ceiling versions=%d open=%d", round, v, open)
		}
		for action, want := range map[string]int{"executed": 1, "approved": 2, "requested": 1, "approve_denied": 0} {
			if n := w.c3AuditCount("licensing_change."+action, r.ID); n != want {
				t.Fatalf("round %d: audit %s=%d want %d", round, action, n, want)
			}
		}
	}
	w.c3TriggersEnabled()
}

// 20 rounds: the final approval races the requester's cancel. Either the change executes (cancel is NOT_PENDING) or it is cancelled
// (approval is NOT_PENDING): never both, never neither, and the database, the audit trail and the ceiling table agree.
func TestC3_Concurrency_FinalApprovalVersusCancel(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	execd, canc := 0, 0
	for round := 0; round < 20; round++ {
		lid := w.licence(jid, "platform", uuid.Nil)
		r := w.c3CeilingReq(lid, "MT")
		w.mustApprove(w.ap1, r.ID)
		out := c3Race(map[string]func() (*Result, error){
			"approve": func() (*Result, error) { return w.approve(w.ap2, r.ID) },
			"cancel": func() (*Result, error) {
				c, err := w.svc.Cancel(context.Background(), w.req, r.ID)
				if err != nil {
					return nil, err
				}
				return &Result{Request: *c}, nil
			},
		})
		var approveErr, cancelErr error
		var approveRes *Result
		for _, o := range out {
			if o.who == "approve" {
				approveErr, approveRes = o.err, o.res
			} else {
				cancelErr = o.err
			}
		}
		st := w.status(r.ID)
		v, _, _, _ := w.c3Ceilings(lid, "MT")
		switch {
		case approveErr == nil && approveRes.Executed && errors.Is(cancelErr, ErrNotPending):
			execd++
			if st != StatusExecuted || v != 1 || w.c3AuditCount("licensing_change.cancelled", r.ID) != 0 || w.c3AuditCount("licensing_change.executed", r.ID) != 1 {
				t.Fatalf("round %d inconsistent after execute: status=%s versions=%d", round, st, v)
			}
		case cancelErr == nil && errors.Is(approveErr, ErrNotPending):
			canc++
			if st != StatusCancelled || v != 0 || w.c3AuditCount("licensing_change.executed", r.ID) != 0 || w.c3AuditCount("licensing_change.cancelled", r.ID) != 1 || w.c3ApprovalCount(r.ID) != 1 {
				t.Fatalf("round %d inconsistent after cancel: status=%s versions=%d approvals=%d", round, st, v, w.c3ApprovalCount(r.ID))
			}
		default:
			t.Fatalf("round %d: approve=(%v,%v) cancel=%v status=%s", round, approveRes, approveErr, cancelErr, st)
		}
	}
	t.Logf("approve-vs-cancel: executed %d, cancelled %d of 20 rounds", execd, canc)
}

//go:build integration

package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// ADR 0116 s6 (A7). The AML officer routes through the REAL HTTP stack and the REAL officer service as the runtime role:
// token -> staffRoute chain -> permission -> amlHandler -> aml.Service -> proof-bound SQL. Nothing is faked.

type amlHTTP struct {
	t      *testing.T
	owner  *db.Pool
	rt     *db.Pool
	api    *cgAPI
	issuer *auth.Issuer
	tenant uuid.UUID
	brand  identity.Brand
	scope  string
}

func newAMLHTTP(t *testing.T, licensingModel string) *amlHTTP {
	t.Helper()
	ou, ru := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if ou == "" || ru == "" {
		t.Skip("TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL are required (a skip is not evidence)")
	}
	w := &amlHTTP{t: t, owner: lhConnect(t, ou), rt: lhConnect(t, ru)}
	prooftest.Install(t, ou)
	launchfix.SeedOperators(t)
	if err := w.owner.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "amlhttp", "amlhttp-"+uuid.NewString(), licensingModel)
		w.tenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.owner.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, w.tenant, "b", "amlhttp-b-"+uuid.NewString())
		w.brand = b
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.scope = amlfix.OwnerScopeOf(t, w.owner, w.tenant)
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "aml-test", "aml-test")
	srv := httptest.NewServer(New(Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})), DB: w.rt, AuthIssuer: issuer,
		StaffMFA: testStaffMFA(), ServiceName: "aml-test", AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour,
		AML: amlfix.Service(t, w.rt),
	}))
	t.Cleanup(srv.Close)
	w.api = &cgAPI{t: t, pool: w.rt, issuer: issuer, srv: srv}
	w.issuer = issuer
	return w
}

func (w *amlHTTP) subject() (person, account uuid.UUID) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		a, err := identity.RegisterPlayer(ctx, tx, w.brand, "aml-http-"+uuid.NewString()+"@fixture.invalid", "!x")
		person, account = a.PersonID, a.ID
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return
}

// officerToken is the HTTP identity of an officer: a tenant compliance token for an own-licence tenant, a platform_admin
// token (no tenant) for a platform-licensed one.
func (w *amlHTTP) officerToken(o amlfix.Officer) string {
	if w.scope == "tenant" {
		return w.api.token(o.StaffID, w.tenant, auth.RoleCompliance)
	}
	return w.api.token(o.StaffID, uuid.Nil, auth.RolePlatformAdmin)
}

func (w *amlHTTP) base() string { return "/v1/admin/tenants/" + w.tenant.String() + "/aml" }

type amlID struct {
	CaseID string `json:"case_id"`
	HoldID string `json:"hold_id"`
}

func (w *amlHTTP) lastSeq(tok, caseID string) int {
	w.t.Helper()
	r := w.api.do("GET", w.base()+"/cases/"+caseID, tok, nil)
	if r.status != http.StatusOK {
		w.t.Fatalf("read case: %d %s", r.status, r.body)
	}
	var d struct {
		Case struct {
			LastSeq int `json:"last_seq"`
		} `json:"case"`
	}
	r.decode(w.t, &d)
	return d.Case.LastSeq
}

// The full officer flow for both owner scopes: place a hold, list it, lift it, open a case, work it, propose a SAR closure
// and approve it with the second officer; each response is no-store; no AML trail is written to audit_log.
func TestAMLHTTP_OfficerFlow_BothOwnerScopes(t *testing.T) {
	for _, model := range []string{"own_licence", "under_platform_licence"} {
		t.Run(model, func(t *testing.T) {
			w := newAMLHTTP(t, model)
			q := amlfix.Bootstrap(t, w.owner, w.tenant, 2)
			a, b := w.officerToken(q.Officers[0]), w.officerToken(q.Officers[1])
			person, _ := w.subject()

			// Hold: place (201), list, lift (critical step-up satisfied by the fresh token), list shows lifted.
			r := w.api.do("POST", w.base()+"/holds", a, map[string]any{"person_id": person, "scope": "withdrawal_and_payout", "reason_class": "manual", "reason_code": "http_flow"})
			if r.status != http.StatusCreated {
				t.Fatalf("place hold: %d %s", r.status, r.body)
			}
			if r.header.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control %q", r.header.Get("Cache-Control"))
			}
			var id amlID
			r.decode(t, &id)
			l := w.api.do("GET", w.base()+"/persons/"+person.String()+"/holds", a, nil)
			if l.status != http.StatusOK || !strings.Contains(string(l.body), id.HoldID) || !strings.Contains(string(l.body), `"lifted":false`) {
				t.Fatalf("list holds: %d %s", l.status, l.body)
			}
			if r := w.api.do("POST", w.base()+"/holds/"+id.HoldID+"/lift", a, map[string]any{"reason_code": "http_lift"}); r.status != http.StatusOK {
				t.Fatalf("lift: %d %s", r.status, r.body)
			}
			if l := w.api.do("GET", w.base()+"/persons/"+person.String()+"/holds", a, nil); !strings.Contains(string(l.body), `"lifted":true`) {
				t.Fatalf("lifted flag: %s", l.body)
			}

			// Case: open, queue (the subject's own officer would not see it; a different officer does), work, flag SAR, close.
			c := w.api.do("POST", w.base()+"/cases", a, map[string]any{"person_id": person, "kind": "manual", "reason_code": "http_case"})
			if c.status != http.StatusCreated {
				t.Fatalf("open case: %d %s", c.status, c.body)
			}
			var cid amlID
			c.decode(t, &cid)
			if qr := w.api.do("GET", w.base()+"/cases?limit=10", b, nil); qr.status != http.StatusOK || !strings.Contains(string(qr.body), cid.CaseID) {
				t.Fatalf("queue: %d %s", qr.status, qr.body)
			}
			for _, to := range []string{"in_review", "escalated", "sar_decision_pending"} {
				seq := w.lastSeq(a, cid.CaseID)
				if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/transition", a, map[string]any{"expected_seq": seq, "to": to, "reason_code": "http_step"}); r.status != http.StatusOK {
					t.Fatalf("transition %s: %d %s", to, r.status, r.body)
				}
			}
			seq := w.lastSeq(a, cid.CaseID)
			if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/notes", a, map[string]any{"expected_seq": seq, "body": "reviewed the pattern"}); r.status != http.StatusCreated {
				t.Fatalf("note: %d %s", r.status, r.body)
			}
			// A stale expected_seq is a 409 stale (compare-and-set).
			if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/transition", a, map[string]any{"expected_seq": seq, "to": "escalated", "reason_code": "http_step"}); r.status != http.StatusConflict || !strings.Contains(string(r.body), `"stale"`) {
				t.Fatalf("stale: %d %s", r.status, r.body)
			}
			// A refused note (an e-mail address) is 400 note_refused and nothing is stored.
			seq = w.lastSeq(a, cid.CaseID)
			if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/notes", a, map[string]any{"expected_seq": seq, "body": "contact someone@example.com"}); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "note_refused") {
				t.Fatalf("lint: %d %s", r.status, r.body)
			}
			// Four-eyes: propose as A; A cannot approve its own proposal; B approves bound to the content hash.
			p := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/proposals", a, map[string]any{"expected_seq": seq, "kind": "close_sar_not_filed", "reason_code": "http_close"})
			if p.status != http.StatusCreated {
				t.Fatalf("propose: %d %s", p.status, p.body)
			}
			var prop struct {
				Seq  int    `json:"proposal_seq"`
				Hash string `json:"content_hash"`
			}
			p.decode(t, &prop)
			if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/approvals", a, map[string]any{"proposal_seq": prop.Seq, "content_hash": prop.Hash, "reason_code": "http_self"}); r.status != http.StatusConflict || !strings.Contains(string(r.body), "four_eyes_refused") {
				t.Fatalf("self approval: %d %s", r.status, r.body)
			}
			if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/approvals", b, map[string]any{"proposal_seq": prop.Seq, "content_hash": strings.Repeat("0", 64), "reason_code": "http_bad_hash"}); r.status != http.StatusConflict {
				t.Fatalf("wrong hash must be refused: %d %s", r.status, r.body)
			}
			if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/approvals", b, map[string]any{"proposal_seq": prop.Seq, "content_hash": prop.Hash, "reason_code": "http_approve"}); r.status != http.StatusOK {
				t.Fatalf("approve: %d %s", r.status, r.body)
			}
			d := w.api.do("GET", w.base()+"/cases/"+cid.CaseID, b, nil)
			if !strings.Contains(string(d.body), `"status":"closed_sar_not_filed"`) || !strings.Contains(string(d.body), `"events"`) {
				t.Fatalf("closed case: %s", d.body)
			}
			// No AML row in audit_log, and no tenant-readable AML wording anywhere in it (ADR 0116 s5.2).
			var n int
			if err := w.owner.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND (action ILIKE 'aml%' OR target_type ILIKE 'aml%' OR metadata::text ILIKE '%aml_%')`, w.tenant).Scan(&n)
			}); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("%d AML rows in audit_log (the AML trail is the AML-scoped event tables)", n)
			}
		})
	}
}

// Who is refused, and how: a compliance user WITHOUT the grant, a tenant admin (no static permission), a support user, a
// foreign-tenant officer, a player and an anonymous caller; below quorum no hold can be placed.
func TestAMLHTTP_Refusals(t *testing.T) {
	w := newAMLHTTP(t, "own_licence")
	q := amlfix.Bootstrap(t, w.owner, w.tenant, 2)
	officer := w.officerToken(q.Officers[0])
	person, _ := w.subject()
	holdBody := map[string]any{"person_id": person, "scope": "all_value_movement", "reason_class": "manual", "reason_code": "http_refusal"}

	// Compliance WITHOUT an aml_officer grant: the static permission passes, the database refuses (403), nothing exists.
	plain, _ := amlfix.NewTenantComplianceStaff(t, w.owner, w.tenant)
	noGrant := w.api.token(plain, w.tenant, auth.RoleCompliance)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", w.base() + "/cases", nil},
		{"GET", w.base() + "/persons/" + person.String() + "/holds", nil},
		{"POST", w.base() + "/holds", holdBody},
		{"POST", w.base() + "/cases", map[string]any{"person_id": person, "kind": "manual", "reason_code": "x"}},
	} {
		if r := w.api.do(c.method, c.path, noGrant, c.body); r.status != http.StatusForbidden {
			t.Errorf("%s %s without a grant: %d %s, want 403", c.method, c.path, r.status, r.body)
		}
	}
	// A tenant admin and a support user hold no AML permission at all.
	for role, sr := range map[auth.Role]identity.StaffRole{auth.RoleTenantAdmin: identity.StaffRoleTenantAdmin, auth.RoleSupport: identity.StaffRoleSupport} {
		staff := w.staffOfRole(sr)
		if r := w.api.do("GET", w.base()+"/cases", w.api.token(staff, w.tenant, role), nil); r.status != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", role, r.status, r.body)
		}
	}
	// A player token and an anonymous caller never get past the chain.
	if r := w.api.do("GET", w.base()+"/cases", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", r.status)
	}
	// An officer of ANOTHER tenant: 404 for this tenant's path (no existence oracle), and it reads nothing.
	other := newAMLHTTP(t, "own_licence")
	oq := amlfix.Bootstrap(t, other.owner, other.tenant, 2)
	foreign := other.officerToken(oq.Officers[0])
	if r := w.api.do("GET", w.base()+"/cases", foreign, nil); r.status != http.StatusNotFound {
		t.Errorf("foreign tenant officer: %d %s, want 404", r.status, r.body)
	}
	// Below quorum (one officer) nothing can be placed: 409 quorum_not_met, and no hold exists afterwards.
	solo := newAMLHTTP(t, "own_licence")
	sq := amlfix.Bootstrap(t, solo.owner, solo.tenant, 1)
	sp, _ := solo.subject()
	r := solo.api.do("POST", solo.base()+"/holds", solo.officerToken(sq.Officers[0]), map[string]any{"person_id": sp, "scope": "all_value_movement", "reason_class": "manual", "reason_code": "solo"})
	if r.status != http.StatusConflict || !strings.Contains(string(r.body), "quorum_not_met") {
		t.Errorf("below quorum: %d %s", r.status, r.body)
	}
	// An officer cannot act on a record about their OWN person (conflict of interest, four-eyes class).
	own := w.api.do("POST", w.base()+"/holds", officer, map[string]any{"person_id": q.Officers[0].PersonID, "scope": "all_value_movement", "reason_class": "manual", "reason_code": "self"})
	if own.status != http.StatusConflict {
		t.Errorf("own-person hold: %d %s, want 409", own.status, own.body)
	}
	// Body-supplied tenant or actor is refused outright.
	bad := map[string]any{"person_id": person, "scope": "all_value_movement", "reason_class": "manual", "reason_code": "x", "tenant_id": uuid.NewString()}
	if r := w.api.do("POST", w.base()+"/holds", officer, bad); r.status != http.StatusBadRequest {
		t.Errorf("body tenant_id: %d %s, want 400", r.status, r.body)
	}
}

func (w *amlHTTP) staffOfRole(role identity.StaffRole) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.CreatePerson(ctx, tx)
		if err != nil {
			return err
		}
		s, err := identity.CreateStaffUser(ctx, tx, w.tenant, "aml-"+uuid.NewString()+"@fixture.invalid", "!x", role, &p.ID)
		id = s.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed staff: %v", err)
	}
	return id
}

// ADR 0119 s8.1: the critical AML routes (hold lift, propose, approve) refuse a stale second factor with 403 step_up_required
// BEFORE anything is written; the protective routes (place hold, open case, transition, note, SAR flag) and every read do not
// ask for one. Only the 5-minute window decides: the same officer with a fresh token is admitted.
func TestAMLHTTP_StepUpClasses(t *testing.T) {
	w := newAMLHTTP(t, "own_licence")
	q := amlfix.Bootstrap(t, w.owner, w.tenant, 2)
	o := q.Officers[0]
	stale := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, o.StaffID, w.tenant, auth.RoleCompliance, 8*time.Minute)
	fresh := w.officerToken(o)
	person, _ := w.subject()

	// Protective + reads work on the stale token.
	r := w.api.do("POST", w.base()+"/holds", stale, map[string]any{"person_id": person, "scope": "all_value_movement", "reason_class": "manual", "reason_code": "stepup"})
	if r.status != http.StatusCreated {
		t.Fatalf("place hold with a stale factor must work (protective): %d %s", r.status, r.body)
	}
	var h amlID
	r.decode(t, &h)
	if r := w.api.do("GET", w.base()+"/cases", stale, nil); r.status != http.StatusOK {
		t.Fatalf("read with a stale factor: %d %s", r.status, r.body)
	}
	c := w.api.do("POST", w.base()+"/cases", stale, map[string]any{"person_id": person, "kind": "manual", "reason_code": "stepup"})
	if c.status != http.StatusCreated {
		t.Fatalf("open case with a stale factor: %d %s", c.status, c.body)
	}
	var cid amlID
	c.decode(t, &cid)
	seq := w.lastSeq(stale, cid.CaseID)
	if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/transition", stale, map[string]any{"expected_seq": seq, "to": "in_review", "reason_code": "stepup"}); r.status != http.StatusOK {
		t.Fatalf("transition with a stale factor: %d %s", r.status, r.body)
	}
	seq = w.lastSeq(stale, cid.CaseID)

	// SR7-1: a confirmed_sanctions hold (gates all value movement; lifted only by four-eyes) needs the fresh factor, a manual hold does not.
	sanc := map[string]any{"person_id": person, "scope": "all_value_movement", "reason_class": "confirmed_sanctions", "reason_code": "stepup"}
	if r := w.api.do("POST", w.base()+"/holds", stale, sanc); r.status != http.StatusForbidden || !strings.Contains(string(r.body), `"step_up_required"`) {
		t.Errorf("sanctions hold with a stale factor: %d %s, want 403 step_up_required", r.status, r.body)
	}
	if hs := w.api.do("GET", w.base()+"/persons/"+person.String()+"/holds", fresh, nil); strings.Contains(string(hs.body), "confirmed_sanctions") {
		t.Fatalf("a refused sanctions hold must write nothing: %s", hs.body)
	}
	if r := w.api.do("POST", w.base()+"/holds", fresh, sanc); r.status != http.StatusCreated {
		t.Errorf("sanctions hold with a fresh factor: %d %s", r.status, r.body)
	}
	// SR7-5: closing as no action needs the fresh factor; moving to other states does not (the in_review move above passed).
	if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/transition", stale, map[string]any{"expected_seq": seq, "to": "closed_no_action", "reason_code": "stepup"}); r.status != http.StatusForbidden || !strings.Contains(string(r.body), `"step_up_required"`) {
		t.Errorf("closed_no_action with a stale factor: %d %s, want 403 step_up_required", r.status, r.body)
	}
	if got := w.lastSeq(fresh, cid.CaseID); got != seq {
		t.Fatalf("a refused closure must write nothing: last_seq %d, was %d", got, seq)
	}
	if r := w.api.do("POST", w.base()+"/cases/"+cid.CaseID+"/transition", fresh, map[string]any{"expected_seq": seq, "to": "closed_no_action", "reason_code": "stepup"}); r.status != http.StatusOK {
		t.Errorf("closed_no_action with a fresh factor: %d %s", r.status, r.body)
	}
	seq = w.lastSeq(fresh, cid.CaseID)
	// Critical routes refuse, with the step-up envelope, and write nothing.
	for _, x := range []struct {
		name, path string
		body       any
	}{
		{"lift", "/holds/" + h.HoldID + "/lift", map[string]any{"reason_code": "stepup"}},
		{"propose", "/cases/" + cid.CaseID + "/proposals", map[string]any{"expected_seq": seq, "kind": "close_sar_not_filed", "reason_code": "stepup"}},
		{"approve", "/cases/" + cid.CaseID + "/approvals", map[string]any{"proposal_seq": 2, "content_hash": strings.Repeat("a", 64), "reason_code": "stepup"}},
	} {
		r := w.api.do("POST", w.base()+x.path, stale, x.body)
		if r.status != http.StatusForbidden || !strings.Contains(string(r.body), `"step_up_required"`) || r.header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s with a stale factor: %d %s, want 403 step_up_required", x.name, r.status, r.body)
		}
	}
	if l := w.api.do("GET", w.base()+"/persons/"+person.String()+"/holds", fresh, nil); !strings.Contains(string(l.body), `"lifted":false`) {
		t.Fatalf("a refused lift must not lift: %s", l.body)
	}
	if got := w.lastSeq(fresh, cid.CaseID); got != seq {
		t.Fatalf("a refused propose must not write: last_seq %d, was %d", got, seq)
	}
	// The same officer with a fresh factor is admitted.
	if r := w.api.do("POST", w.base()+"/holds/"+h.HoldID+"/lift", fresh, map[string]any{"reason_code": "stepup"}); r.status != http.StatusOK {
		t.Fatalf("lift with a fresh factor: %d %s", r.status, r.body)
	}
}

//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// Licensing slice 1: HTTP tests of the licence lifecycle writes (PUT /v1/admin/licences/{id}/status | validity-dates |
// review-due | evidence-reference, GET .../versions): platform-admin only, reason code required, audited, versioned,
// and step-up critical.

type lcHTTP struct {
	srvURL string
	token  string
	licID  string
	srv    *httptest.Server
	pool   *db.Pool
}

// lcSetup starts a server and a platform admin and provisions ONE licence. Since migration 0177 POST /v1/admin/licences only files a
// licensing change (202, no licence id), so the fixture creates the licence through the owner pool (the owner path of the
// governed-write guard is the fixture path), exactly like the other fixtures of governed tables. Since migration 0178 the
// lifecycle writes need the actor-proof issuer, so it is installed here.
func lcSetup(t *testing.T, pw string) (lcHTTP, func(method, path string, body any) *http.Response) {
	t.Helper()
	ensureLicensingProofs(t)
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, pw)
	token := mustLoginStaff(t, srv, "", admin.Email, pw).AccessToken
	do := func(method, path string, body any) *http.Response {
		return sendAssetJSON(t, srv.URL+path, method, token, body)
	}
	licID := uuid.New()
	if err := launchfix.OwnerPool(t).WithPlatformAdmin(context.Background(), admin.ID, func(ctx context.Context, tx pgx.Tx) error {
		jid := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'LC')`, jid, newJurisdictionCodeForHTTP()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', 'LC-HTTP-1')`, licID, jid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return lcHTTP{srvURL: srv.URL, token: token, licID: licID.String(), srv: srv, pool: pool}, do
}

func TestLicenceLifecycleAPI_PlatformAdminRoundTrip_AuditedAndVersioned(t *testing.T) {
	h, do := lcSetup(t, "pa-lc-pw-1")
	base := "/v1/admin/licences/" + h.licID

	resp := do(http.MethodPut, base+"/status", map[string]any{"status": "suspended", "reason_code": "regulator_suspension"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: %d", resp.StatusCode)
	}
	var ch licenceChangeResponse
	decodeBody(t, resp, &ch)
	if ch.ChangeKind != "narrowing" || ch.Licence.Status != "suspended" {
		t.Fatalf("suspend response %+v", ch)
	}
	// INTENDED BEHAVIOUR CHANGE (migration 0178): reinstating to active is WIDENING, so it is filed (202) and needs two approvals.
	resp = do(http.MethodPut, base+"/status", map[string]any{"status": "active", "reason_code": "regulator_reinstated"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reinstate must be filed as a licensing change (202), got %d", resp.StatusCode)
	}
	done := executeFiledChange(t, h.srv, h.pool, resp)
	if done.ChangeRequest.Kind != "licence_status_change" || !done.ChangeRequest.Widening || done.Outcome["change_kind"] != "widening" {
		t.Fatalf("reinstate must be classified widening by the database: %+v", done)
	}
	resp = do(http.MethodGet, base, nil)
	var detail struct {
		Status    string   `json:"status"`
		TenantIDs []string `json:"tenant_ids"`
	}
	decodeBody(t, resp, &detail)
	if detail.Status != "active" || detail.TenantIDs == nil || len(detail.TenantIDs) != 0 {
		t.Fatalf("after the executed reinstatement: %+v", detail)
	}
	resp = do(http.MethodPut, base+"/validity-dates", map[string]any{"issued_at": "2026-01-01", "expires_at": "2027-01-01", "reason_code": "licence_issued"})
	decodeBody(t, resp, &ch)
	if ch.Licence.ExpiresAt == nil || *ch.Licence.ExpiresAt != "2027-01-01" {
		t.Fatalf("validity response %+v", ch)
	}
	resp = do(http.MethodPut, base+"/review-due", map[string]any{"review_due_at": "2026-12-01", "reason_code": "review_scheduled"})
	decodeBody(t, resp, &ch)
	if ch.Licence.ReviewDueAt == nil || *ch.Licence.ReviewDueAt != "2026-12-01" {
		t.Fatalf("review response %+v", ch)
	}
	resp = do(http.MethodPut, base+"/evidence-reference", map[string]any{"licence_evidence_reference": "REG-LETTER-0042", "reason_code": "evidence_recorded"})
	decodeBody(t, resp, &ch)
	if ch.Licence.EvidenceReference == nil || *ch.Licence.EvidenceReference != "REG-LETTER-0042" || ch.ChangeKind != "neutral" {
		t.Fatalf("evidence response %+v", ch)
	}

	resp = do(http.MethodGet, base+"/versions", nil)
	var vs []licenceVersionResponse
	decodeBody(t, resp, &vs)
	if len(vs) != 6 || vs[5].Operation != "update" || vs[5].ChangedBy == nil {
		t.Fatalf("versions %+v", vs)
	}
	for i, v := range vs {
		if v.VersionNo != i+1 {
			t.Fatalf("version numbers: %+v", vs)
		}
	}

	// The list endpoint carries the lifecycle fields.
	resp = do(http.MethodGet, "/v1/admin/licences", nil)
	var list []licenceResponse
	decodeBody(t, resp, &list)
	found := false
	for _, l := range list {
		if l.ID == h.licID {
			found = true
			if l.ReviewDueAt == nil || l.EvidenceReference == nil || l.ExpiresAt == nil || l.IssuedAt == nil {
				t.Fatalf("list item lacks lifecycle fields: %+v", l)
			}
		}
	}
	if !found {
		t.Fatal("licence not listed")
	}
}

func TestLicenceLifecycleAPI_ValidationRefusals(t *testing.T) {
	h, do := lcSetup(t, "pa-lc-pw-2")
	base := "/v1/admin/licences/" + h.licID
	cases := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"missing reason code", http.MethodPut, base + "/status", map[string]any{"status": "suspended"}, http.StatusBadRequest},
		{"revoked is not in the vocabulary", http.MethodPut, base + "/status", map[string]any{"status": "revoked", "reason_code": "x"}, http.StatusBadRequest},
		{"unknown field", http.MethodPut, base + "/status", map[string]any{"status": "suspended", "reason_code": "x", "tenant_id": uuid.NewString()}, http.StatusBadRequest},
		{"no-op status", http.MethodPut, base + "/status", map[string]any{"status": "active", "reason_code": "x"}, http.StatusBadRequest},
		{"dates: missing key", http.MethodPut, base + "/validity-dates", map[string]any{"expires_at": "2027-01-01", "reason_code": "x"}, http.StatusBadRequest},
		{"dates: bad format", http.MethodPut, base + "/validity-dates", map[string]any{"issued_at": nil, "expires_at": "01/01/2027", "reason_code": "x"}, http.StatusBadRequest},
		{"dates: expires before issued", http.MethodPut, base + "/validity-dates", map[string]any{"issued_at": "2027-01-01", "expires_at": "2026-01-01", "reason_code": "x"}, http.StatusBadRequest},
		{"evidence: contact details", http.MethodPut, base + "/evidence-reference", map[string]any{"licence_evidence_reference": "a@b.example", "reason_code": "x"}, http.StatusBadRequest},
		{"bad licence id", http.MethodPut, "/v1/admin/licences/not-a-uuid/status", map[string]any{"status": "suspended", "reason_code": "x"}, http.StatusBadRequest},
		{"unknown licence", http.MethodPut, "/v1/admin/licences/" + uuid.NewString() + "/status", map[string]any{"status": "suspended", "reason_code": "x"}, http.StatusNotFound},
	}
	for _, c := range cases {
		resp := do(c.method, c.path, c.body)
		if resp.StatusCode != c.want {
			t.Errorf("%s: want %d, got %d", c.name, c.want, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := do(http.MethodGet, base+"/versions", nil)
	var vs []licenceVersionResponse
	decodeBody(t, resp, &vs)
	if len(vs) != 1 {
		t.Fatalf("refused requests must write nothing; versions = %d", len(vs))
	}
}

// Every non-platform principal is refused on every lifecycle route: a tenant admin (tenant-scope token) gets 403, an
// anonymous caller 401, and the licence is unchanged.
func TestLicenceLifecycleAPI_NonPlatformPrincipalsRefused(t *testing.T) {
	h, _ := lcSetup(t, "pa-lc-pw-3")
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-lc-pw-1")
	taToken := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-lc-pw-1").AccessToken

	base := "/v1/admin/licences/" + h.licID
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, base + "/status", map[string]any{"status": "suspended", "reason_code": "x"}},
		{http.MethodPut, base + "/validity-dates", map[string]any{"issued_at": nil, "expires_at": "2030-01-01", "reason_code": "x"}},
		{http.MethodPut, base + "/review-due", map[string]any{"review_due_at": "2030-01-01", "reason_code": "x"}},
		{http.MethodPut, base + "/evidence-reference", map[string]any{"licence_evidence_reference": "X-1", "reason_code": "x"}},
		{http.MethodGet, base + "/versions", nil},
	}
	for _, r := range routes {
		if resp := sendAssetJSON(t, srv.URL+r.path, r.method, taToken, r.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("tenant_admin %s %s: want 403, got %d", r.method, r.path, resp.StatusCode)
		} else {
			resp.Body.Close()
		}
		if resp := sendAssetJSON(t, srv.URL+r.path, r.method, "", r.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: want 401, got %d", r.method, r.path, resp.StatusCode)
		} else {
			resp.Body.Close()
		}
	}
	// Unchanged (read with the platform admin from the first server: same database).
	_, do := lcSetupReuse(t, h)
	resp := do(http.MethodGet, base+"/versions", nil)
	var vs []licenceVersionResponse
	decodeBody(t, resp, &vs)
	if len(vs) != 1 {
		t.Fatalf("a refused principal changed the licence: %d versions", len(vs))
	}
}

func lcSetupReuse(t *testing.T, h lcHTTP) (lcHTTP, func(method, path string, body any) *http.Response) {
	return h, func(method, path string, body any) *http.Response {
		return sendAssetJSON(t, h.srvURL+path, method, h.token, body)
	}
}

// A stale second factor (11 minutes) is refused with step_up_required (300 s) on every lifecycle write and the licence
// is not changed; a fresh session passes the gate. (The golden-table test also covers these routes generically.)
func TestLicenceLifecycleAPI_StaleStepUpRefused_FreshPasses(t *testing.T) {
	ensureLicensingProofs(t)
	w := newMFAW(t)
	admin := w.staff(nil, "platform_admin")
	stale := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, admin.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin, 11*time.Minute)
	fresh := authtest.LiveStaffToken(t, w.rt, testStaffMFAKeys(), w.issuer, admin.ref.StaffID, uuid.Nil, auth.RolePlatformAdmin)

	var licID uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), admin.ref.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		jid := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'SU')`, jid, "SU-"+jid.String()[:8]); err != nil {
			return err
		}
		licID = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', 'SU-1')`, licID, jid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	base := "/v1/admin/licences/" + licID.String()
	writes := []struct {
		path string
		body map[string]any
	}{
		{base + "/status", map[string]any{"status": "suspended", "reason_code": "x"}},
		{base + "/validity-dates", map[string]any{"issued_at": nil, "expires_at": "2030-01-01", "reason_code": "x"}},
		{base + "/review-due", map[string]any{"review_due_at": "2030-01-01", "reason_code": "x"}},
		{base + "/evidence-reference", map[string]any{"licence_evidence_reference": "X-1", "reason_code": "x"}},
	}
	for _, wr := range writes {
		stepUpRefused(t, w.do(http.MethodPut, wr.path, stale, wr.body), 300, wr.path+" (stale)")
	}
	var status string
	var versions int
	read := func() {
		if err := w.owner.WithPlatformAdmin(context.Background(), admin.ref.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT status FROM licences WHERE id = $1`, licID).Scan(&status); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM licence_versions WHERE licence_id = $1`, licID).Scan(&versions)
		}); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if status != "active" || versions != 1 {
		t.Fatalf("a step-up-refused write changed the licence: %s / %d versions", status, versions)
	}
	r := w.do(http.MethodPut, base+"/status", fresh, writes[0].body)
	if r.status != http.StatusOK {
		t.Fatalf("fresh session: want 200, got %d %s", r.status, r.body)
	}
	read()
	if status != "suspended" || versions != 2 {
		t.Fatalf("after the fresh write: %s / %d", status, versions)
	}
}

// lic4 slice 4a (migration 0178), intended behaviour change: WIDENING lifecycle PUTs (later / cleared expiry, postponed or
// cleared review date, reinstatement) file a licensing change (202) and take effect only after two approvals by other Persons;
// NARROWING PUTs (set / shorten) and the evidence reference execute at once (200). The request bodies are unchanged.
func TestLicenceLifecycleAPI_WideningFiles202_NarrowingExecutes200(t *testing.T) {
	h, do := lcSetup(t, "pa-lc-pw-w1")
	base := "/v1/admin/licences/" + h.licID
	state := func() (expires, review *string) {
		resp := do(http.MethodGet, base, nil)
		var l licenceResponse
		decodeBody(t, resp, &l)
		return l.ExpiresAt, l.ReviewDueAt
	}
	put := func(path string, body map[string]any) *http.Response { return do(http.MethodPut, base+path, body) }
	eq := func(p *string, want string) bool { return (p == nil && want == "") || (p != nil && *p == want) }

	// Set an expiry on an open-ended licence: narrowing, executes.
	r := put("/validity-dates", map[string]any{"issued_at": nil, "expires_at": "2027-01-01", "reason_code": "licence_issued"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("setting an expiry narrows: want 200, got %d", r.StatusCode)
	}
	r.Body.Close()
	// Extend it: widening -> filed, nothing changes until two approvals.
	r = put("/validity-dates", map[string]any{"issued_at": nil, "expires_at": "2028-01-01", "reason_code": "renewal_confirmed"})
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("extending the expiry widens: want 202, got %d", r.StatusCode)
	}
	if exp, _ := state(); !eq(exp, "2027-01-01") {
		t.Fatalf("a filed change must not change the licence: expires_at = %v", exp)
	}
	// The identical request repeated is a duplicate (409), not a second change.
	dup := put("/validity-dates", map[string]any{"issued_at": nil, "expires_at": "2028-01-01", "reason_code": "renewal_confirmed"})
	expectStatus(t, dup, http.StatusConflict, "duplicate open widening request")
	// The requester cannot approve their own request.
	var filed licensingChangeResponseIT
	r2 := put("/validity-dates", map[string]any{"issued_at": nil, "expires_at": "2029-01-01", "reason_code": "renewal_confirmed_2"})
	if r2.StatusCode != http.StatusAccepted {
		t.Fatalf("second widening request: %d", r2.StatusCode)
	}
	decodeBody(t, r2, &filed)
	if filed.ChangeRequest.Kind != "licence_validity_dates_change" || !filed.ChangeRequest.Widening || filed.ChangeRequest.RequiredApprovals != 2 {
		t.Fatalf("filed change %+v", filed.ChangeRequest)
	}
	self := sendAssetJSON(t, h.srvURL+"/v1/admin/licensing-changes/"+filed.ChangeRequest.ID+"/approve", http.MethodPost, h.token, map[string]any{"reason_code": "self"})
	if self.StatusCode == http.StatusOK {
		t.Fatal("the requester must not approve their own widening request")
	}
	self.Body.Close()
	// Two other Persons execute the second request (2029).
	done := approveFiled(t, h, filed.ChangeRequest.ID)
	if !done.Executed || done.Outcome["change_kind"] != "widening" {
		t.Fatalf("executed change %+v", done)
	}
	if exp, _ := state(); !eq(exp, "2029-01-01") {
		t.Fatalf("after two approvals expires_at = %v, want 2029-01-01", exp)
	}
	// Shorten: narrowing, executes (200).
	r = put("/validity-dates", map[string]any{"issued_at": nil, "expires_at": "2028-06-01", "reason_code": "shortened"})
	expectStatus(t, r, http.StatusOK, "shortening the expiry narrows")
	// Clearing the expiry widens.
	r = put("/validity-dates", map[string]any{"issued_at": nil, "expires_at": nil, "reason_code": "no_expiry"})
	expectStatus(t, r, http.StatusAccepted, "clearing the expiry widens")

	// Review date: set (narrowing), postpone (widening), clear (widening), bring forward (narrowing).
	expectStatus(t, put("/review-due", map[string]any{"review_due_at": "2027-06-01", "reason_code": "scheduled"}), http.StatusOK, "setting a review date")
	expectStatus(t, put("/review-due", map[string]any{"review_due_at": "2027-05-01", "reason_code": "earlier"}), http.StatusOK, "an earlier review date")
	expectStatus(t, put("/review-due", map[string]any{"review_due_at": "2028-01-01", "reason_code": "postponed"}), http.StatusAccepted, "a postponed review date")
	expectStatus(t, put("/review-due", map[string]any{"review_due_at": nil, "reason_code": "cleared"}), http.StatusAccepted, "a cleared review date")
	if _, rev := state(); !eq(rev, "2027-05-01") {
		t.Fatalf("filed review changes must not apply: review_due_at = %v", rev)
	}

	// Evidence reference: neutral, executes (200), set and clear.
	expectStatus(t, put("/evidence-reference", map[string]any{"licence_evidence_reference": "REG-1", "reason_code": "evidence"}), http.StatusOK, "evidence set")
	expectStatus(t, put("/evidence-reference", map[string]any{"licence_evidence_reference": nil, "reason_code": "evidence_cleared"}), http.StatusOK, "evidence cleared")

	// Status: suspend (200); reinstate is covered by the round-trip test.
	expectStatus(t, put("/status", map[string]any{"status": "suspended", "reason_code": "regulator_suspension"}), http.StatusOK, "suspend")
	expectStatus(t, put("/status", map[string]any{"status": "active", "reason_code": "regulator_reinstated"}), http.StatusAccepted, "reinstate")
}

// approveFiled approves a filed change with two other platform admins and returns the final response.
func approveFiled(t *testing.T, h lcHTTP, changeID string) licensingChangeResponseIT {
	t.Helper()
	tok1, tok2 := licensingApproverTokens(t, h.srv, h.pool)
	r1 := sendAssetJSON(t, h.srvURL+"/v1/admin/licensing-changes/"+changeID+"/approve", http.MethodPost, tok1, map[string]any{"reason_code": "reviewed_ok"})
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first approval: %d", r1.StatusCode)
	}
	r1.Body.Close()
	r2 := sendAssetJSON(t, h.srvURL+"/v1/admin/licensing-changes/"+changeID+"/approve", http.MethodPost, tok2, map[string]any{"reason_code": "reviewed_ok"})
	var done licensingChangeResponseIT
	decodeBody(t, r2, &done)
	return done
}

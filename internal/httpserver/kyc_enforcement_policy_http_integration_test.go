//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// lic10 (L-20): HTTP tests of the KYC enforcement policy authoring routes. The threshold values below are TEST fixtures only;
// the platform ships none.

type kycPolicyW struct {
	srv          *httptest.Server
	pool         *db.Pool
	tokA, tokB   string
	adminA       identity.StaffUser
	jurisdiction string
}

func kycPolicySetup(t *testing.T) *kycPolicyW {
	t.Helper()
	ensureLicensingProofs(t)
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	a := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "kycpol-pw-a")
	b := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "kycpol-pw-b")
	w := &kycPolicyW{srv: srv, pool: pool, adminA: a}
	w.tokA = mustLoginStaff(t, srv, "", a.Email, "kycpol-pw-a").AccessToken
	w.tokB = mustLoginStaff(t, srv, "", b.Email, "kycpol-pw-b").AccessToken
	jid := uuid.New()
	if err := launchfix.OwnerPool(t).WithPlatformAdmin(context.Background(), a.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'KP')`, jid, newJurisdictionCodeForHTTP())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.jurisdiction = jid.String()
	return w
}

func (w *kycPolicyW) do(t *testing.T, tok, method, path string, body any) *http.Response {
	t.Helper()
	return sendAssetJSON(t, w.srv.URL+path, method, tok, body)
}

func (w *kycPolicyW) createDeposit(t *testing.T, tok, threshold string) kycEnforcementPolicyResponse {
	t.Helper()
	resp := w.do(t, tok, http.MethodPost, "/v1/admin/kyc/enforcement-policies", map[string]any{
		"licensing_jurisdiction_id": w.jurisdiction, "trigger_type": "cumulative_deposit", "threshold_minor_units": threshold,
		"asset_code": "EUR", "legal_review_reference": "TEST-REF-1", "reason_code": "test_policy",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var p kycEnforcementPolicyResponse
	decodeBody(t, resp, &p)
	return p
}

func TestKYCEnforcementPolicyAPI_CreateIsDraft_FourEyesActivation_Audited(t *testing.T) {
	w := kycPolicySetup(t)
	p := w.createDeposit(t, w.tokA, "100000")
	if p.Status != "draft" || p.TriggerType != "cumulative_deposit" || p.ThresholdMinorUnits == nil || *p.ThresholdMinorUnits != "100000" {
		t.Fatalf("created: %+v", p)
	}
	act := "/v1/admin/kyc/enforcement-policies/" + p.ID + "/activate"
	// The author may not activate (migration 0100 four-eyes).
	if resp := w.do(t, w.tokA, http.MethodPost, act, map[string]any{"reason_code": "activate_test"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("author activation: %d, want 403", resp.StatusCode)
	}
	// A different administrator can.
	resp := w.do(t, w.tokB, http.MethodPost, act, map[string]any{"reason_code": "activate_test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other admin activation: %d", resp.StatusCode)
	}
	var got kycEnforcementPolicyResponse
	decodeBody(t, resp, &got)
	if got.Status != "active" {
		t.Fatalf("activated: %+v", got)
	}
	// Re-activating is a conflict, not a silent success.
	if resp := w.do(t, w.tokB, http.MethodPost, act, map[string]any{"reason_code": "activate_test"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second activation: %d, want 409", resp.StatusCode)
	}
	// List by jurisdiction and status.
	resp = w.do(t, w.tokA, http.MethodGet, "/v1/admin/kyc/enforcement-policies?licensing_jurisdiction_id="+w.jurisdiction+"&status=active", nil)
	var list []kycEnforcementPolicyResponse
	decodeBody(t, resp, &list)
	if len(list) != 1 || list[0].ID != p.ID {
		t.Fatalf("list: %+v", list)
	}
	// Audit: created and activated, each with the reason code and request evidence.
	var n int
	if err := launchfix.OwnerPool(t).WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target_type = 'kyc_enforcement_policy' AND target_id = $1
			AND action IN ('kyc_enforcement_policy.created', 'kyc_enforcement_policy.activated') AND request_id IS NOT NULL`, p.ID).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("audit rows with request evidence: %d, want 2", n)
	}
}

func TestKYCEnforcementPolicyAPI_ActiveCannotBeSwitchedOff_SupersedeNeedsThirdAdminAndSameKey(t *testing.T) {
	w := kycPolicySetup(t)
	c := mustCreateStaff(t, w.pool, uuid.Nil, identity.StaffRolePlatformAdmin, "kycpol-pw-c")
	tokC := mustLoginStaff(t, w.srv, "", c.Email, "kycpol-pw-c").AccessToken

	old := w.createDeposit(t, w.tokA, "100000")
	if resp := w.do(t, w.tokB, http.MethodPost, "/v1/admin/kyc/enforcement-policies/"+old.ID+"/activate", map[string]any{"reason_code": "r1"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("activate: %d", resp.StatusCode)
	}
	// Withdrawing an ACTIVE policy is refused by the database (no successor): enforcement is never silently disabled.
	if resp := w.do(t, w.tokB, http.MethodPost, "/v1/admin/kyc/enforcement-policies/"+old.ID+"/withdraw", map[string]any{"reason_code": "r2"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("withdraw of an active policy: %d, want 409", resp.StatusCode)
	}
	// A second ACTIVE policy for the same key cannot be created by activating another draft while the first is active.
	succ := w.createDeposit(t, w.tokB, "200000")
	if resp := w.do(t, tokC, http.MethodPost, "/v1/admin/kyc/enforcement-policies/"+succ.ID+"/activate", map[string]any{"reason_code": "r3"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("activation while the key already has an active policy: %d, want 409", resp.StatusCode)
	}
	// Supersede: the old policy's author (A) and the successor's author (B) may not perform it; a third administrator may.
	sup := "/v1/admin/kyc/enforcement-policies/" + old.ID + "/supersede"
	for name, tok := range map[string]string{"old author": w.tokA, "successor author": w.tokB} {
		if resp := w.do(t, tok, http.MethodPost, sup, map[string]any{"successor_policy_id": succ.ID, "reason_code": "r4"}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("supersede by the %s: %d, want 403", name, resp.StatusCode)
		}
	}
	resp := w.do(t, tokC, http.MethodPost, sup, map[string]any{"successor_policy_id": succ.ID, "reason_code": "r4"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("supersede by a third administrator: %d", resp.StatusCode)
	}
	var now kycEnforcementPolicyResponse
	decodeBody(t, resp, &now)
	if now.Status != "active" || now.ID != succ.ID {
		t.Fatalf("successor after supersede: %+v", now)
	}
	resp = w.do(t, w.tokA, http.MethodGet, "/v1/admin/kyc/enforcement-policies?licensing_jurisdiction_id="+w.jurisdiction+"&status=withdrawn", nil)
	var wd []kycEnforcementPolicyResponse
	decodeBody(t, resp, &wd)
	if len(wd) != 1 || wd[0].ID != old.ID || wd[0].SuccessorPolicyID == nil || *wd[0].SuccessorPolicyID != succ.ID {
		t.Fatalf("withdrawn list: %+v", wd)
	}
}

func TestKYCEnforcementPolicyAPI_DraftWithdraw_Validation_UnwiredTypes_Authorization(t *testing.T) {
	w := kycPolicySetup(t)
	d := w.createDeposit(t, w.tokA, "5")
	// A draft that was never active can be withdrawn, but (migration 0100: four-eyes on every withdrawal) not by its own author.
	if resp := w.do(t, w.tokA, http.MethodPost, "/v1/admin/kyc/enforcement-policies/"+d.ID+"/withdraw", map[string]any{"reason_code": "mistake"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("author withdrawing their own draft: %d, want 403 (database four-eyes)", resp.StatusCode)
	}
	if resp := w.do(t, w.tokB, http.MethodPost, "/v1/admin/kyc/enforcement-policies/"+d.ID+"/withdraw", map[string]any{"reason_code": "mistake"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("draft withdraw: %d", resp.StatusCode)
	}
	create := func(body map[string]any) int {
		return w.do(t, w.tokA, http.MethodPost, "/v1/admin/kyc/enforcement-policies", body).StatusCode
	}
	base := func() map[string]any {
		return map[string]any{"licensing_jurisdiction_id": w.jurisdiction, "trigger_type": "cumulative_deposit", "threshold_minor_units": "10",
			"asset_code": "EUR", "legal_review_reference": "REF", "reason_code": "x"}
	}
	for name, mut := range map[string]func(map[string]any){
		"unwired edd_amount": func(m map[string]any) { m["trigger_type"] = "edd_amount" },
		"unwired registration": func(m map[string]any) {
			m["trigger_type"] = "registration_tier"
			delete(m, "threshold_minor_units")
			delete(m, "asset_code")
		},
		"zero threshold":          func(m map[string]any) { m["threshold_minor_units"] = "0" },
		"negative threshold":      func(m map[string]any) { m["threshold_minor_units"] = "-5" },
		"fractional threshold":    func(m map[string]any) { m["threshold_minor_units"] = "1.5" },
		"missing legal reference": func(m map[string]any) { m["legal_review_reference"] = "" },
		"missing reason":          func(m map[string]any) { m["reason_code"] = "" },
		"unknown asset":           func(m map[string]any) { m["asset_code"] = "NOPE_ASSET" },
		"unknown jurisdiction":    func(m map[string]any) { m["licensing_jurisdiction_id"] = uuid.NewString() },
		"unknown field":           func(m map[string]any) { m["status"] = "active" },
	} {
		m := base()
		mut(m)
		if got := create(m); got != http.StatusBadRequest && got != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d, want a validation refusal", name, got)
		}
	}
	// play shape: needs a play_operation and nothing else.
	if got := create(map[string]any{"licensing_jurisdiction_id": w.jurisdiction, "trigger_type": "play", "play_operation": "casino_play",
		"legal_review_reference": "REF", "reason_code": "x"}); got != http.StatusCreated {
		t.Errorf("play create: %d", got)
	}
	if got := create(map[string]any{"licensing_jurisdiction_id": w.jurisdiction, "trigger_type": "play", "play_operation": "casino_play",
		"threshold_minor_units": "5", "legal_review_reference": "REF", "reason_code": "x"}); got == http.StatusCreated {
		t.Error("play with a threshold must be refused")
	}
	// A tenant-scoped staff member and an unauthenticated caller are refused (platform_admin only).
	tnt := mustCreateTenant(t, w.pool)
	ta := mustCreateStaff(t, w.pool, tnt.ID, identity.StaffRoleTenantAdmin, "kycpol-pw-t")
	tt := mustLoginStaff(t, w.srv, tnt.Slug, ta.Email, "kycpol-pw-t").AccessToken
	for _, tok := range []string{tt, ""} {
		for _, rq := range []struct{ m, p string }{
			{http.MethodGet, "/v1/admin/kyc/enforcement-policies"},
			{http.MethodPost, "/v1/admin/kyc/enforcement-policies"},
			{http.MethodPost, "/v1/admin/kyc/enforcement-policies/" + uuid.NewString() + "/activate"},
		} {
			if got := w.do(t, tok, rq.m, rq.p, map[string]any{}).StatusCode; got != http.StatusForbidden && got != http.StatusUnauthorized {
				t.Errorf("%s %s with token present=%v: %d, want 401/403", rq.m, rq.p, tok != "", got)
			}
		}
	}
	// An unknown policy id is a 404, never a 500.
	if got := w.do(t, w.tokB, http.MethodPost, "/v1/admin/kyc/enforcement-policies/"+uuid.NewString()+"/activate", map[string]any{"reason_code": "x"}).StatusCode; got != http.StatusConflict && got != http.StatusNotFound {
		t.Errorf("unknown policy: %d", got)
	}
}

// lic18 S4 / L6: two staff accounts of ONE Person cannot make and activate the same policy (the database rule compares accounts);
// the audit rows carry the AFTER state.
func TestKYCEnforcementPolicyAPI_PersonDistinctFourEyes_AndAuditAfterState(t *testing.T) {
	w := kycPolicySetup(t)
	pool := w.pool
	person := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c := mustCreateStaffWithPerson(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "kycpol-pw-c", &person)
	d := mustCreateStaffWithPerson(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "kycpol-pw-d", &person)
	tokC := mustLoginStaff(t, w.srv, "", c.Email, "kycpol-pw-c").AccessToken
	tokD := mustLoginStaff(t, w.srv, "", d.Email, "kycpol-pw-d").AccessToken
	p := w.createDeposit(t, tokC, "250000")
	act := "/v1/admin/kyc/enforcement-policies/" + p.ID + "/activate"
	if resp := w.do(t, tokD, http.MethodPost, act, map[string]any{"reason_code": "activate_test"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a second account of the author's Person: %d, want 403", resp.StatusCode)
	}
	resp := w.do(t, w.tokB, http.MethodGet, "/v1/admin/kyc/enforcement-policies?licensing_jurisdiction_id="+w.jurisdiction+"&status=draft", nil)
	var list []kycEnforcementPolicyResponse
	decodeBody(t, resp, &list)
	if len(list) != 1 || list[0].Status != "draft" {
		t.Fatalf("the refused activation must leave the draft: %+v", list)
	}
	if resp := w.do(t, w.tokB, http.MethodPost, act, map[string]any{"reason_code": "activate_test"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a different Person activates: %d", resp.StatusCode)
	}
	var withAfter int
	if err := launchfix.OwnerPool(t).WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target_type = 'kyc_enforcement_policy' AND target_id = $1
			AND action IN ('kyc_enforcement_policy.created', 'kyc_enforcement_policy.activated')
			AND metadata -> 'after' ->> 'trigger_type' = 'cumulative_deposit' AND metadata -> 'after' ->> 'threshold_minor_units' = '250000'`, p.ID).Scan(&withAfter)
	}); err != nil {
		t.Fatal(err)
	}
	if withAfter != 2 {
		t.Fatalf("audit rows carrying the after state: %d, want 2", withAfter)
	}
}

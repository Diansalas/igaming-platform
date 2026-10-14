//go:build integration

package httpserver

// ADR 0119 S5 + S6 HTTP tests (section 17.1 / 17.5): the lifecycle state machine incl. illegal edges, sessions ended
// by every lifecycle change (sv mismatch), self-targeting, role authority, tenant isolation, the platform four-eyes
// (distinct staff id AND Person), MFA reset, audit and event completeness, and the "no token in any response" rule.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

type lcResp struct {
	status int
	body   []byte
}

func (r lcResp) code() string {
	var e apierror.Error
	_ = json.Unmarshal(r.body, &e)
	return string(e.Code)
}

func lcDo(t *testing.T, srv *httptest.Server, method, path, token string, body any) lcResp {
	t.Helper()
	var resp *http.Response
	if method == http.MethodGet {
		resp = getJSON(t, srv, path, token)
	} else {
		resp = postJSON(t, srv, path, token, body)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return lcResp{status: resp.StatusCode, body: b}
}

func lcExpect(t *testing.T, r lcResp, status int, code string, what string) {
	t.Helper()
	if r.status != status || (code != "" && r.code() != code) {
		t.Fatalf("%s: got %d %s, want %d %s", what, r.status, r.body, status, code)
	}
}

func lcStaffRow(t *testing.T, pool *db.Pool, tenantID, staffID uuid.UUID) (status, role string, sv int64) {
	t.Helper()
	fn := func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, role, staff_version FROM staff_users WHERE id = $1`, staffID).Scan(&status, &role, &sv)
	}
	var err error
	if tenantID != uuid.Nil {
		err = pool.WithTenant(context.Background(), tenantID, fn)
	} else {
		err = pool.WithoutTenant(context.Background(), fn)
	}
	if err != nil {
		t.Fatalf("staff row: %v", err)
	}
	return
}

// lcLiveSessions counts staffID's unrevoked sessions in the principal's own scope (a bare tenant session does not
// see session rows: such a count would be vacuous).
func lcLiveSessions(t *testing.T, pool *db.Pool, tenantID, staffID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithPrincipalScope(context.Background(), tenantID, staffID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE principal_type = 'staff' AND principal_id = $1 AND revoked_at IS NULL`, staffID).Scan(&n)
	}); err != nil {
		t.Fatalf("live sessions: %v", err)
	}
	return n
}

func lcCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, q string, args ...any) int {
	t.Helper()
	var n int
	fn := func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(&n) }
	var err error
	if tenantID != uuid.Nil {
		err = pool.WithTenant(context.Background(), tenantID, fn)
	} else {
		err = pool.WithoutTenant(context.Background(), fn)
	}
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The full tenant state machine over HTTP: invite -> accept (+enrol) -> suspend -> reactivate -> role change ->
// MFA reset -> offboard, every illegal edge refused with lifecycle_transition_invalid, every change ending the
// target's sessions (the old token is 401 session_revoked: staff_version / status), events and audit rows complete.
func TestLifecycle_TenantFullStateMachine(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mail := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-lc-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-lc-pw-1").AccessToken
	base := "/v1/admin/tenants/" + tn.ID.String() + "/staff/"

	addr := "lc-support-" + uuid.NewString() + "@example.com"
	created := mustInviteStaff(t, srv, paTok, tn.ID, addr, "support", uuid.Nil)
	sid := uuid.MustParse(created.ID)
	if status, _, sv := lcStaffRow(t, pool, tn.ID, sid); status != "invited" || sv != 1 {
		t.Fatalf("invited row: %s sv=%d", status, sv)
	}
	// Illegal edges from invited.
	for _, p := range []string{"suspend", "reactivate", "offboard", "mfa-reset"} {
		lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/"+p, paTok, map[string]string{"reason_code": firstReason(p)}),
			http.StatusConflict, "lifecycle_transition_invalid", "invited -> "+p)
	}
	// A password login of an invited account fails (no password): invalid credentials, never a challenge.
	if r := staffFixturePost(t, srv, "/v1/staff/auth/login", map[string]string{"tenant_slug": tn.Slug, "email": addr, "password": "anything-long"}); r.status != http.StatusUnauthorized {
		t.Fatalf("invited login: %d %s", r.status, r.body)
	}
	pair := fixtureAcceptAndEnrol(t, srv, lastMailedToken(t, mail, addr), "a-decent-password-1")
	// The token was single use.
	if r := staffFixturePost(t, srv, "/v1/staff/invitations/accept", map[string]string{"token": lastMailedToken(t, mail, addr), "password": "another-password-1"}); r.status != http.StatusUnauthorized || r.code() != "invalid_enrolment_token" {
		t.Fatalf("accept replay: %d %s", r.status, r.body)
	}
	if r := lcDo(t, srv, http.MethodGet, "/v1/staff/me/mfa", pair.AccessToken, nil); r.status != http.StatusOK {
		t.Fatalf("the accepted staff member's session: %d %s", r.status, r.body)
	}
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/reactivate", paTok, map[string]string{"reason_code": "other", "note": "a sufficiently long note"}),
		http.StatusConflict, "lifecycle_transition_invalid", "active -> reactivate")

	// Suspend: the old token is refused at once; a refresh is refused; status suspended, sv bumped.
	if lcLiveSessions(t, pool, tn.ID, sid) == 0 {
		t.Fatal("non-vacuity: the target must hold a live session before the suspension")
	}
	_, _, svBefore := lcStaffRow(t, pool, tn.ID, sid)
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/suspend", paTok, map[string]string{"reason_code": "investigation"}), http.StatusOK, "", "suspend")
	if status, _, sv := lcStaffRow(t, pool, tn.ID, sid); status != "suspended" || sv != svBefore+1 {
		t.Fatalf("suspended row: %s sv=%d (was %d)", status, sv, svBefore)
	}
	lcExpect(t, lcDo(t, srv, http.MethodGet, "/v1/staff/me/mfa", pair.AccessToken, nil), http.StatusUnauthorized, "session_revoked", "suspended token")
	if r := staffFixturePost(t, srv, "/v1/auth/refresh", map[string]string{"refresh_token": pair.RefreshToken}); r.status == http.StatusOK {
		t.Fatalf("a suspended staff member's refresh must fail: %s", r.body)
	}
	if n := lcLiveSessions(t, pool, tn.ID, sid); n != 0 {
		t.Fatalf("suspension left %d live sessions", n)
	}
	// suspended -> suspend / role: illegal.
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/suspend", paTok, map[string]string{"reason_code": "investigation"}), http.StatusConflict, "lifecycle_transition_invalid", "suspend twice")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/role", paTok, map[string]string{"role": "compliance", "reason_code": "role_change_approved"}), http.StatusConflict, "lifecycle_transition_invalid", "role on suspended")

	// Reactivate (factor intact: no email).
	before := mailCount(mail, addr)
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/reactivate", paTok, map[string]string{"reason_code": "investigation_closed"}), http.StatusOK, "", "reactivate")
	if mailCount(mail, addr) != before {
		t.Fatal("a reactivation with a usable factor must not email a reset token")
	}
	tok2 := mustLoginStaff(t, srv, tn.Slug, addr, "a-decent-password-1")

	// Role change: the old token is refused (role and sv); a fresh login carries the new role.
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/role", paTok, map[string]string{"role": "risk_manager", "reason_code": "role_change_approved"}), http.StatusOK, "", "role change")
	lcExpect(t, lcDo(t, srv, http.MethodGet, "/v1/staff/me/mfa", tok2.AccessToken, nil), http.StatusUnauthorized, "session_revoked", "token after role change")
	if _, role, _ := lcStaffRow(t, pool, tn.ID, sid); role != "risk_manager" {
		t.Fatalf("role after change: %s", role)
	}
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/role", paTok, map[string]string{"role": "risk_manager", "reason_code": "role_change_approved"}), http.StatusConflict, "lifecycle_transition_invalid", "same role")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/role", paTok, map[string]string{"role": "compliance", "reason_code": "role_change_approved"}), http.StatusUnprocessableEntity, "person_required", "compliance without Person")

	// MFA reset: factor revoked, sessions revoked, an mfa_reset email; redeem with the password and re-enrol.
	tok3 := mustLoginStaff(t, srv, tn.Slug, addr, "a-decent-password-1")
	before = mailCount(mail, addr)
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/mfa-reset", paTok, map[string]string{"reason_code": "device_lost"}), http.StatusOK, "", "mfa reset")
	if mailCount(mail, addr) != before+1 {
		t.Fatal("mfa reset must email one token")
	}
	lcExpect(t, lcDo(t, srv, http.MethodGet, "/v1/staff/me/mfa", tok3.AccessToken, nil), http.StatusUnauthorized, "session_revoked", "token after mfa reset")
	if r := staffFixturePost(t, srv, "/v1/staff/auth/login", map[string]string{"tenant_slug": tn.Slug, "email": addr, "password": "a-decent-password-1"}); r.status != http.StatusForbidden || r.code() != "mfa_enrolment_required" {
		t.Fatalf("login after reset: %d %s", r.status, r.body)
	}
	rd := staffFixturePost(t, srv, "/v1/staff/auth/enrol/redeem", map[string]string{"token": lastMailedToken(t, mail, addr), "tenant_slug": tn.Slug, "email": addr, "password": "a-decent-password-1"})
	if rd.status != http.StatusOK {
		t.Fatalf("redeem the mfa_reset token: %d %s", rd.status, rd.body)
	}
	var ch staffChallengeWire
	_ = json.Unmarshal(rd.body, &ch)
	fixtureEnrolWithChallenge(t, srv, ch.Challenge.Token)

	// Offboard (terminal): password removed, factor revoked, every later action refused.
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/offboard", paTok, map[string]string{"reason_code": "employment_ended"}), http.StatusOK, "", "offboard")
	if n := lcCount(t, pool, tn.ID, `SELECT count(*) FROM staff_users WHERE id = $1 AND status = 'offboarded' AND password_hash IS NULL AND offboarded_at IS NOT NULL`, sid); n != 1 {
		t.Fatal("offboarded row must have no password and an offboarded_at")
	}
	if n := lcCount(t, pool, tn.ID, `SELECT count(*) FROM staff_mfa_factors WHERE staff_user_id = $1 AND status IN ('active', 'pending')`, sid); n != 0 {
		t.Fatal("offboarding must revoke every factor")
	}
	for _, p := range []string{"suspend", "reactivate", "offboard", "mfa-reset"} {
		lcExpect(t, lcDo(t, srv, http.MethodPost, base+created.ID+"/"+p, paTok, map[string]string{"reason_code": firstReason(p)}),
			http.StatusConflict, "lifecycle_transition_invalid", "offboarded -> "+p)
	}

	// Events and audit: one event per change, with actor, reason and before / after.
	ev := lcDo(t, srv, http.MethodGet, base+created.ID+"/lifecycle-events", paTok, nil)
	var events staffLifecycleEventListDTO
	if ev.status != http.StatusOK || json.Unmarshal(ev.body, &events) != nil {
		t.Fatalf("events: %d %s", ev.status, ev.body)
	}
	want := []string{"offboarded", "mfa_reset", "role_changed", "reactivated", "suspended", "invite_accepted", "invited"}
	var got []string
	for _, e := range events.Events {
		if e.Event == "mfa_enrolled" {
			continue
		}
		got = append(got, e.Event)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", got, want)
	}
	if n := lcCount(t, pool, tn.ID, `SELECT count(*) FROM audit_log WHERE target_id = $1 AND action IN ('staff.invited', 'staff.invite_accepted',
		'staff.suspended', 'staff.reactivated', 'staff.role_changed', 'staff.mfa_reset', 'staff.offboarded') AND metadata ? 'after'`, sid.String()); n != 7 {
		t.Fatalf("lifecycle audit rows: %d, want 7", n)
	}
	if n := lcCount(t, pool, tn.ID, `SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'staff.access_revoked'`, sid.String()); n != 2 {
		t.Fatalf("phase-1 access revocation audit rows: %d, want 2 (suspend, offboard)", n)
	}
	// No response or audit row ever carried a mailed token.
	for _, m := range mail.Sent() {
		if m.To != addr {
			continue
		}
		tokn := m.Body[strings.Index(m.Body, "token=")+6:]
		tokn = strings.Fields(strings.SplitN(tokn, "&", 2)[0])[0]
		if n := lcCount(t, pool, tn.ID, `SELECT count(*) FROM audit_log WHERE metadata::text LIKE '%' || $1 || '%'`, tokn); n != 0 {
			t.Fatal("a mailed token reached the audit log")
		}
		if strings.Contains(string(ev.body), tokn) {
			t.Fatal("a mailed token reached a response")
		}
	}
}

func firstReason(path string) string {
	switch path {
	case "suspend":
		return "investigation"
	case "reactivate":
		return "investigation_closed"
	case "offboard":
		return "employment_ended"
	case "mfa-reset":
		return "device_lost"
	case "invitation/revoke":
		return "no_longer_required"
	}
	return "other"
}

// Self-targeting is refused by the HTTP layer (403 self_target_forbidden) for every action, and by SF013 in the
// database when Go is bypassed.
func TestLifecycle_SelfTarget_RefusedByHTTPAndDatabase(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _ := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ta-self-pw-1")
	tok := mustLoginStaff(t, srv, tn.Slug, ta.Email, "ta-self-pw-1").AccessToken
	base := "/v1/admin/tenants/" + tn.ID.String() + "/staff/" + ta.ID.String() + "/"
	for _, p := range []string{"suspend", "reactivate", "offboard", "mfa-reset", "invitation/revoke"} {
		lcExpect(t, lcDo(t, srv, http.MethodPost, base+p, tok, map[string]string{"reason_code": firstReason(p)}), http.StatusForbidden, "self_target_forbidden", "self "+p)
	}
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+"role", tok, map[string]string{"role": "support", "reason_code": "role_change_approved"}), http.StatusForbidden, "self_target_forbidden", "self role")
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-self-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-self-pw-1").AccessToken
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/platform-staff/"+pa.ID.String()+"/suspend", paTok, map[string]string{"reason_code": "investigation"}), http.StatusForbidden, "self_target_forbidden", "platform self suspend")
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/platform/staff-lifecycle-requests", paTok, map[string]string{"kind": "platform_mfa_reset", "target_staff_id": pa.ID.String(), "reason_code": "device_lost"}), http.StatusForbidden, "self_target_forbidden", "platform self reset request")

	// SF013 in the database (Go bypassed): each actor GUC refuses a self-targeted status or role change.
	for _, guc := range []string{"app.principal_id", "app.actor_staff_id"} {
		err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, guc, ta.ID.String()); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, ta.ID)
			return err
		})
		if pgCodeOf(err) != "SF013" {
			t.Fatalf("SF013 via %s: %v", guc, err)
		}
	}
}

// Role authority (section 17.1): a tenant admin may not invite into, act on, or change into the platform-only roles;
// compliance reads but never manages; support neither.
func TestLifecycle_RoleAuthorityMatrix(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _ := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ta-auth-pw-1")
	co := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleCompliance, "co-auth-pw-1")
	su := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleSupport, "su-auth-pw-1")
	fi := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleFinance, "fi-auth-pw-1")
	taTok := mustLoginStaff(t, srv, tn.Slug, ta.Email, "ta-auth-pw-1").AccessToken
	coTok := mustLoginStaff(t, srv, tn.Slug, co.Email, "co-auth-pw-1").AccessToken
	suTok := mustLoginStaff(t, srv, tn.Slug, su.Email, "su-auth-pw-1").AccessToken
	base := "/v1/admin/tenants/" + tn.ID.String() + "/staff"

	for _, p := range []string{"suspend", "offboard", "mfa-reset"} {
		lcExpect(t, lcDo(t, srv, http.MethodPost, base+"/"+fi.ID.String()+"/"+p, taTok, map[string]string{"reason_code": firstReason(p)}), http.StatusForbidden, "role_not_grantable", "tenant_admin -> finance "+p)
	}
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+"/"+su.ID.String()+"/role", taTok, map[string]string{"role": "bonus_operations", "reason_code": "role_change_approved"}), http.StatusForbidden, "role_not_grantable", "tenant_admin -> bonus_operations")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+"/invitations", taTok, map[string]string{"email": "x-" + uuid.NewString() + "@example.com", "role": "promotions_manager"}), http.StatusForbidden, "role_not_grantable", "tenant_admin invites promotions_manager")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+"/invitations", taTok, map[string]string{"email": "x-" + uuid.NewString() + "@example.com", "role": "compliance"}), http.StatusUnprocessableEntity, "person_required", "compliance without Person")
	// compliance reads its tenant's directory, but holds no staff:manage.
	lcExpect(t, lcDo(t, srv, http.MethodGet, base, coTok, nil), http.StatusOK, "", "compliance directory")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+"/"+su.ID.String()+"/suspend", coTok, map[string]string{"reason_code": "investigation"}), http.StatusForbidden, "forbidden", "compliance manage")
	lcExpect(t, lcDo(t, srv, http.MethodGet, base, suTok, nil), http.StatusForbidden, "forbidden", "support directory")
	// tenant_admin may act on support.
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+"/"+su.ID.String()+"/suspend", taTok, map[string]string{"reason_code": "policy_violation"}), http.StatusOK, "", "tenant_admin suspends support")
	// A note is required for other, and the reason list is closed.
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+"/"+su.ID.String()+"/reactivate", taTok, map[string]string{"reason_code": "other"}), http.StatusBadRequest, "validation_error", "other without note")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+"/"+su.ID.String()+"/reactivate", taTok, map[string]string{"reason_code": "employment_ended"}), http.StatusBadRequest, "validation_error", "a reason of another action")
}

// Tenant isolation (section 17.1): another tenant's staff member is 404 (never data); a tenant caller never reaches a
// platform row; the tenant directory never lists platform staff; a tenant viewer sees a platform actor as null.
func TestLifecycle_TenantIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _ := newLifecycleTestServer(t, pool, issuer)
	tnA, tnB := mustCreateTenant(t, pool), mustCreateTenant(t, pool)
	taA := mustCreateStaff(t, pool, tnA.ID, identity.StaffRoleTenantAdmin, "ta-a-pw-1")
	suB := mustCreateStaff(t, pool, tnB.ID, identity.StaffRoleSupport, "su-b-pw-1")
	suA := mustCreateStaff(t, pool, tnA.ID, identity.StaffRoleSupport, "su-a-pw-1")
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-iso-pw-1")
	tokA := mustLoginStaff(t, srv, tnA.Slug, taA.Email, "ta-a-pw-1").AccessToken
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-iso-pw-1").AccessToken

	// Another tenant in the path, or another tenant's staff under one's own path: 404.
	lcExpect(t, lcDo(t, srv, http.MethodGet, "/v1/admin/tenants/"+tnB.ID.String()+"/staff", tokA, nil), http.StatusNotFound, "not_found", "foreign tenant directory")
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/tenants/"+tnB.ID.String()+"/staff/"+suB.ID.String()+"/suspend", tokA, map[string]string{"reason_code": "investigation"}), http.StatusNotFound, "not_found", "foreign tenant suspend")
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/tenants/"+tnA.ID.String()+"/staff/"+suB.ID.String()+"/suspend", tokA, map[string]string{"reason_code": "investigation"}), http.StatusNotFound, "not_found", "foreign staff under own path")
	lcExpect(t, lcDo(t, srv, http.MethodGet, "/v1/admin/tenants/"+tnA.ID.String()+"/staff/"+pa.ID.String(), tokA, nil), http.StatusNotFound, "not_found", "platform row under tenant path")
	lcExpect(t, lcDo(t, srv, http.MethodGet, "/v1/admin/platform-staff", tokA, nil), http.StatusForbidden, "forbidden", "tenant caller on platform directory")
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/platform-staff/"+pa.ID.String()+"/suspend", tokA, map[string]string{"reason_code": "investigation"}), http.StatusForbidden, "forbidden", "tenant caller suspends platform")
	lcExpect(t, lcDo(t, srv, http.MethodGet, "/v1/admin/platform/staff-lifecycle-requests", tokA, nil), http.StatusForbidden, "forbidden", "tenant caller four-eyes queue")
	if suB.ID == uuid.Nil {
		t.Fatal("fixture")
	}
	// The directory lists only the tenant's own staff.
	d := lcDo(t, srv, http.MethodGet, "/v1/admin/tenants/"+tnA.ID.String()+"/staff", tokA, nil)
	if d.status != http.StatusOK || strings.Contains(string(d.body), pa.ID.String()) || strings.Contains(string(d.body), suB.ID.String()) || !strings.Contains(string(d.body), suA.ID.String()) {
		t.Fatalf("tenant directory leak: %s", d.body)
	}
	// A platform actor's action on a tenant row: a tenant viewer sees actor.staff_id null; the platform viewer sees it.
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/tenants/"+tnA.ID.String()+"/staff/"+suA.ID.String()+"/suspend", paTok, map[string]string{"reason_code": "investigation"}), http.StatusOK, "", "platform suspends tenant support")
	for viewer, tok := range map[string]string{"tenant": tokA, "platform": paTok} {
		r := lcDo(t, srv, http.MethodGet, "/v1/admin/tenants/"+tnA.ID.String()+"/staff/"+suA.ID.String()+"/lifecycle-events", tok, nil)
		var evs staffLifecycleEventListDTO
		if r.status != http.StatusOK || json.Unmarshal(r.body, &evs) != nil || len(evs.Events) == 0 {
			t.Fatalf("%s events: %d %s", viewer, r.status, r.body)
		}
		hidden := strings.Contains(string(r.body), `"staff_id":null`) && !strings.Contains(string(r.body), pa.ID.String())
		if (viewer == "tenant") != hidden {
			t.Fatalf("%s viewer redaction wrong: %s", viewer, r.body)
		}
	}
}

func pgCodeOf(err error) string {
	type coded interface{ SQLState() string }
	for e := err; e != nil; {
		if c, ok := e.(coded); ok {
			return c.SQLState()
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return ""
		}
		e = u.Unwrap()
	}
	return ""
}

var _ = email.NewMockProvider

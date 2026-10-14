//go:build integration

package httpserver

// ADR 0119 S5 + S6 section 10.4 over HTTP: the platform-staff four-eyes. The approver must be another staff member
// AND another Person than the requester and than the target (403 four_eyes_violation; SF032 in the database), the
// payload_hash is frozen, a request executes in the approval's transaction, reject / cancel close it, step-up is
// critical for request and approve, and platform_staff:govern is platform_admin only.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

const lcRequests = "/v1/admin/platform/staff-lifecycle-requests"

func lcCreateRequest(t *testing.T, srv *httptest.Server, token string, body map[string]string) platformStaffLifecycleRequestDTO {
	t.Helper()
	r := lcDo(t, srv, http.MethodPost, lcRequests, token, body)
	if r.status != http.StatusCreated {
		t.Fatalf("create request %v: %d %s", body, r.status, r.body)
	}
	var out platformStaffLifecycleRequestDTO
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLifecycle_PlatformFourEyes_InviteReactivateResetRejectCancel(t *testing.T) {
	pool, issuer := testEnv(t)
	s, mail := newLifecycleTestServer(t, pool, issuer)
	srv := s
	a := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-a-pw-1")
	b := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-b-pw-1")
	// c shares a's Person: another staff id, the same Person (must not count as a second pair of eyes).
	c := mustCreateStaffWithPerson(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-c-pw-1", a.PersonID)
	aTok := mustLoginStaff(t, s, "", a.Email, "pa-a-pw-1").AccessToken
	bTok := mustLoginStaff(t, s, "", b.Email, "pa-b-pw-1").AccessToken
	cTok := mustLoginStaff(t, s, "", c.Email, "pa-c-pw-1").AccessToken

	// 1. platform_invite: a requests; a and c (same Person) are refused; b approves and the invite executes.
	invitee := "pa-invitee-" + uuid.NewString() + "@example.com"
	person := mustSeedPerson(t, pool)
	req := lcCreateRequest(t, srv, aTok, map[string]string{"kind": "platform_invite", "email": invitee, "person_id": person.String()})
	if req.State != "pending" || req.RequestedBy != a.ID.String() || req.RequiredApprovals != 1 {
		t.Fatalf("request: %+v", req)
	}
	approve := func(tok, hash string) lcResp {
		return lcDo(t, s, http.MethodPost, lcRequests+"/"+req.ID+"/approve", tok, map[string]string{"payload_hash": hash})
	}
	lcExpect(t, approve(aTok, req.PayloadHash), http.StatusForbidden, "four_eyes_violation", "requester approves")
	lcExpect(t, approve(cTok, req.PayloadHash), http.StatusForbidden, "four_eyes_violation", "requester's Person approves")
	lcExpect(t, approve(bTok, "0000000000000000000000000000000000000000000000000000000000000000"), http.StatusBadRequest, "validation_error", "a payload_hash that is not the request's")
	if mailCount(mail, invitee) != 0 {
		t.Fatal("no invitation before the approval")
	}
	ok := approve(bTok, req.PayloadHash)
	lcExpect(t, ok, http.StatusOK, "", "distinct approver")
	var done platformStaffLifecycleRequestDTO
	_ = json.Unmarshal(ok.body, &done)
	if done.State != "executed" || done.TargetStaffID == nil {
		t.Fatalf("executed request: %+v", done)
	}
	lcExpect(t, approve(bTok, req.PayloadHash), http.StatusConflict, "lifecycle_transition_invalid", "approve twice")
	newAdmin := uuid.MustParse(*done.TargetStaffID)
	if status, role, _ := lcStaffRow(t, pool, uuid.Nil, newAdmin); status != "invited" || role != "platform_admin" {
		t.Fatalf("the invited platform admin: %s %s", status, role)
	}
	fixtureAcceptAndEnrol(t, s, lastMailedToken(t, mail, invitee), "a-decent-password-1")
	if n := lcCount(t, pool, uuid.Nil, `SELECT count(*) FROM staff_lifecycle_approvals WHERE request_id = $1 AND decided_by = $2 AND decision = 'approve'`, uuid.MustParse(req.ID), b.ID); n != 1 {
		t.Fatalf("approval rows: %d", n)
	}
	if n := lcCount(t, pool, uuid.Nil, `SELECT count(*) FROM staff_lifecycle_events WHERE staff_user_id = $1 AND event = 'invited' AND lifecycle_request_id = $2`, newAdmin, uuid.MustParse(req.ID)); n != 1 {
		t.Fatalf("invited event bound to the request: %d", n)
	}

	// 2. Suspend (single actor) then platform_reactivate through the four-eyes; the target may not approve.
	lcExpect(t, lcDo(t, s, http.MethodPost, "/v1/admin/platform-staff/"+c.ID.String()+"/suspend", bTok, map[string]string{"reason_code": "investigation"}), http.StatusOK, "", "platform suspend")
	re := lcCreateRequest(t, srv, bTok, map[string]string{"kind": "platform_reactivate", "target_staff_id": c.ID.String(), "reason_code": "investigation_closed"})
	// a shares the target's Person: refused (approver != target Person).
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests+"/"+re.ID+"/approve", aTok, map[string]string{"payload_hash": re.PayloadHash}), http.StatusForbidden, "four_eyes_violation", "the target's Person approves")
	d := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-d-pw-1")
	dTok := mustLoginStaff(t, s, "", d.Email, "pa-d-pw-1").AccessToken
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests+"/"+re.ID+"/approve", dTok, map[string]string{"payload_hash": re.PayloadHash}), http.StatusOK, "", "reactivate approved")
	if status, _, _ := lcStaffRow(t, pool, uuid.Nil, c.ID); status != "active" {
		t.Fatalf("reactivated: %s", status)
	}

	// 3. platform_mfa_reset; reject; cancel (requester only).
	rs := lcCreateRequest(t, srv, bTok, map[string]string{"kind": "platform_mfa_reset", "target_staff_id": d.ID.String(), "reason_code": "device_replaced"})
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests+"/"+rs.ID+"/cancel", dTok, map[string]string{}), http.StatusForbidden, "forbidden", "cancel by a non-requester")
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests+"/"+rs.ID+"/reject", aTok, map[string]string{}), http.StatusOK, "", "reject")
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests+"/"+rs.ID+"/approve", aTok, map[string]string{"payload_hash": rs.PayloadHash}), http.StatusConflict, "lifecycle_transition_invalid", "approve a rejected request")
	rs2 := lcCreateRequest(t, srv, bTok, map[string]string{"kind": "platform_mfa_reset", "target_staff_id": d.ID.String(), "reason_code": "device_replaced"})
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests+"/"+rs2.ID+"/cancel", bTok, map[string]string{}), http.StatusOK, "", "cancel by the requester")
	rs3 := lcCreateRequest(t, srv, bTok, map[string]string{"kind": "platform_mfa_reset", "target_staff_id": d.ID.String(), "reason_code": "device_replaced"})
	before := mailCount(mail, d.Email)
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests+"/"+rs3.ID+"/approve", aTok, map[string]string{"payload_hash": rs3.PayloadHash}), http.StatusOK, "", "reset approved")
	if mailCount(mail, d.Email) != before+1 {
		t.Fatal("the platform MFA reset must email the target")
	}
	lcExpect(t, lcDo(t, s, http.MethodGet, "/v1/staff/me/mfa", dTok, nil), http.StatusUnauthorized, "session_revoked", "the reset target's session")

	// 4. platform_password_reset is NOT IMPLEMENTED; an unknown kind is 400.
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests, bTok, map[string]string{"kind": "platform_password_reset", "target_staff_id": a.ID.String(), "reason_code": "device_lost"}), http.StatusBadRequest, "validation_error", "password reset kind")
	// The list shows every request, newest first.
	l := lcDo(t, s, http.MethodGet, lcRequests, bTok, nil)
	var list platformStaffLifecycleRequestListDTO
	if l.status != http.StatusOK || json.Unmarshal(l.body, &list) != nil || len(list.Requests) < 5 {
		t.Fatalf("list: %d %s", l.status, l.body)
	}

	// 5. Step-up: the request and the approval are critical (a 6-minute-old step-up is refused, nothing is written).
	stale := authtest.LiveStaffTokenAged(t, pool, testStaffMFAKeys(), issuer, b.ID, uuid.Nil, auth.RolePlatformAdmin, 6*time.Minute)
	n0 := lcCount(t, pool, uuid.Nil, `SELECT count(*) FROM audit_log WHERE action = 'staff.lifecycle_requested'`)
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests, stale, map[string]string{"kind": "platform_mfa_reset", "target_staff_id": a.ID.String(), "reason_code": "device_lost"}), http.StatusForbidden, "step_up_required", "stale step-up request")
	if lcCount(t, pool, uuid.Nil, `SELECT count(*) FROM audit_log WHERE action = 'staff.lifecycle_requested'`) != n0 {
		t.Fatal("a step-up refusal must write nothing")
	}
}

// platform_staff:govern is platform_admin only; a tenant admin holding staff:manage is refused on the four-eyes.
func TestLifecycle_PlatformGovern_TenantAdminRefused(t *testing.T) {
	pool, issuer := testEnv(t)
	s, _ := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ta-gov-pw-1")
	tok := mustLoginStaff(t, s, tn.Slug, ta.Email, "ta-gov-pw-1").AccessToken
	lcExpect(t, lcDo(t, s, http.MethodPost, lcRequests, tok, map[string]string{"kind": "platform_invite", "email": "x@example.com", "person_id": uuid.NewString()}), http.StatusForbidden, "forbidden", "tenant admin requests")
}

// SF032 in the database: an approval inserted by SQL with a proof for the requester's Person is refused even when Go
// is bypassed; an approval without a proof is AP001.
func TestLifecycle_SF032_And_ProofRequired_DirectSQL(t *testing.T) {
	pool, issuer := testEnv(t)
	s, _ := newLifecycleTestServer(t, pool, issuer)
	a := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sf32a-pw-1")
	c := mustCreateStaffWithPerson(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sf32c-pw-1", a.PersonID)
	aTok := mustLoginStaff(t, s, "", a.Email, "pa-sf32a-pw-1").AccessToken
	req := lcCreateRequest(t, s, aTok, map[string]string{"kind": "platform_invite", "email": "sf32-" + uuid.NewString() + "@example.com", "person_id": mustSeedPerson(t, pool).String()})
	ins := func(actor uuid.UUID) error {
		return pool.WithPlatformAdmin(context.Background(), actor, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO staff_lifecycle_approvals (request_id, decision, payload_hash) VALUES ($1, 'approve', $2)`,
				uuid.MustParse(req.ID), req.PayloadHash)
			return err
		})
	}
	// c is another staff id with a's Person: SF032 fires before the proof guard.
	if code := pgCodeOf(ins(c.ID)); code != "SF032" {
		t.Fatalf("same-Person approval by SQL: %s", code)
	}
	d := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sf32d-pw-1")
	if code := pgCodeOf(ins(d.ID)); code != "AP001" {
		t.Fatalf("an approval without a proof: %s", code)
	}
}

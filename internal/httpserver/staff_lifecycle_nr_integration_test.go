//go:build integration

package httpserver

// ADR 0119 S5 + S6 security re-review fix NR-1: an MFA-reset token is checked against its ISSUER only. The holder's
// original inviter (staff_users.invited_by) has nothing to do with the reset, so an inviter who has since left must not
// block a staff member's device recovery. An invitation token still checks both (SR-2).

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// Tenant: TA invites support W; W accepts; PA offboards TA; PA (active) resets W's MFA; W redeems -> 200.
func TestNR1_MFAResetRedeemsAfterTheOriginalInviterLeft_Tenant(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mail := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-nr1-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-nr1-pw-1").AccessToken
	ta := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ta-nr1-pw-1")
	taTok := mustLoginStaff(t, srv, tn.Slug, ta.Email, "ta-nr1-pw-1").AccessToken
	base := "/v1/admin/tenants/" + tn.ID.String() + "/staff/"

	addr := "nr1-w-" + uuid.NewString() + "@example.com"
	w := mustInviteStaff(t, srv, taTok, tn.ID, addr, "support", uuid.Nil)
	fixtureAcceptAndEnrol(t, srv, lastMailedToken(t, mail, addr), "a-decent-password-1")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+ta.ID.String()+"/offboard", paTok, map[string]string{"reason_code": "employment_ended"}), http.StatusOK, "", "offboard the inviter")
	if status, _, _ := lcStaffRow(t, pool, tn.ID, ta.ID); status != "offboarded" {
		t.Fatalf("non-vacuity: the inviter must be offboarded, got %s", status)
	}
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+w.ID+"/mfa-reset", paTok, map[string]string{"reason_code": "device_lost"}), http.StatusOK, "", "mfa reset by a live issuer")
	r := staffFixturePost(t, srv, "/v1/staff/auth/enrol/redeem", map[string]string{"token": lastMailedToken(t, mail, addr), "tenant_slug": tn.Slug, "email": addr, "password": "a-decent-password-1"})
	if r.status != http.StatusOK {
		t.Fatalf("an MFA reset issued by a live admin must redeem although the original inviter left: %d %s", r.status, r.body)
	}
	var ch staffChallengeWire
	if err := json.Unmarshal(r.body, &ch); err != nil || ch.Challenge.Type != "enrol" {
		t.Fatalf("redeem challenge: %s", r.body)
	}
}

// Platform: a requests the platform_invite of X (invited_by = a), b approves; X accepts; b offboards a; a
// platform_mfa_reset of X filed by b and approved by d; X redeems -> 200.
func TestNR1_MFAResetRedeemsAfterTheOriginalInviterLeft_Platform(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mail := newLifecycleTestServer(t, pool, issuer)
	a := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-nr1a-pw-1")
	b := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-nr1b-pw-1")
	d := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-nr1d-pw-1")
	aTok := mustLoginStaff(t, srv, "", a.Email, "pa-nr1a-pw-1").AccessToken
	bTok := mustLoginStaff(t, srv, "", b.Email, "pa-nr1b-pw-1").AccessToken
	dTok := mustLoginStaff(t, srv, "", d.Email, "pa-nr1d-pw-1").AccessToken

	invitee := "nr1-x-" + uuid.NewString() + "@example.com"
	req := lcCreateRequest(t, srv, aTok, map[string]string{"kind": "platform_invite", "email": invitee, "person_id": mustSeedPerson(t, pool).String()})
	ok := lcDo(t, srv, http.MethodPost, lcRequests+"/"+req.ID+"/approve", bTok, map[string]string{"payload_hash": req.PayloadHash})
	lcExpect(t, ok, http.StatusOK, "", "approve the invite")
	var done platformStaffLifecycleRequestDTO
	if err := json.Unmarshal(ok.body, &done); err != nil || done.TargetStaffID == nil {
		t.Fatalf("executed invite: %s", ok.body)
	}
	x := uuid.MustParse(*done.TargetStaffID)
	if n := lcCount(t, pool, uuid.Nil, `SELECT count(*) FROM staff_users WHERE id = $1 AND invited_by = $2`, x, a.ID); n != 1 {
		t.Fatal("non-vacuity: the invitee's invited_by must be the requester")
	}
	fixtureAcceptAndEnrol(t, srv, lastMailedToken(t, mail, invitee), "a-decent-password-1")
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/platform-staff/"+a.ID.String()+"/offboard", bTok, map[string]string{"reason_code": "employment_ended"}), http.StatusOK, "", "offboard the original requester")

	rs := lcCreateRequest(t, srv, bTok, map[string]string{"kind": "platform_mfa_reset", "target_staff_id": x.String(), "reason_code": "device_lost"})
	lcExpect(t, lcDo(t, srv, http.MethodPost, lcRequests+"/"+rs.ID+"/approve", dTok, map[string]string{"payload_hash": rs.PayloadHash}), http.StatusOK, "", "approve the reset")
	r := staffFixturePost(t, srv, "/v1/staff/auth/enrol/redeem", map[string]string{"token": lastMailedToken(t, mail, invitee), "email": invitee, "password": "a-decent-password-1"})
	if r.status != http.StatusOK {
		t.Fatalf("a platform MFA reset must redeem although the original inviter left: %d %s", r.status, r.body)
	}
}

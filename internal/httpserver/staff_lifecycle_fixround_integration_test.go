//go:build integration

package httpserver

// ADR 0119 S5 + S6 review fix round, over HTTP:
//   - SR-2: suspending / offboarding a staff member withdraws what they issued (same-scope invitations offboarded, live
//     invite / MFA-reset tokens revoked), and an invitation acceptance or an MFA-reset redemption whose issuer is no
//     longer active is refused (the backstop for a cross-scope issuer the suspension cannot see under RLS);
//   - SR-3: the approver sees the invitee Person and its live platform links; a Person already bound to a live
//     platform account is refused at filing (409) and at execution (409);
//   - SR-4: both person-link routes refuse a self link, enforce the role authority of the other lifecycle routes,
//     and re-assert the caller in the transaction;
//   - SR-9: the invitation acceptance password floor is 12;
//   - F5: an approve answers whether the email was handed off.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// frBumpSV bumps a staff member's staff_version (a password change, SF012), making any token they hold stale.
func frBumpSV(t *testing.T, pool *db.Pool, tenantID, staffID uuid.UUID) {
	t.Helper()
	fn := func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE staff_users SET password_hash = password_hash || 'x' WHERE id = $1`, staffID)
		if err == nil && ct.RowsAffected() != 1 {
			t.Fatalf("fixture: sv bump touched %d rows", ct.RowsAffected())
		}
		return err
	}
	var err error
	if tenantID == uuid.Nil {
		err = pool.WithoutTenant(context.Background(), fn)
	} else {
		err = pool.WithTenant(context.Background(), tenantID, fn)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestFixRound_SR2_AcceptRefusedOnceTheInviterIsSuspended(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mail := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sr2-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-sr2-pw-1").AccessToken
	base := "/v1/admin/tenants/" + tn.ID.String() + "/staff/"

	// 1. Same scope: a tenant admin invites; a platform admin suspends the tenant admin -> the invitation is
	// offboarded (event invite_revoked, reason issuer_removed) and its token is refused.
	ta := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ta-sr2-pw-1")
	taTok := mustLoginStaff(t, srv, tn.Slug, ta.Email, "ta-sr2-pw-1").AccessToken
	x := "sr2-x-" + uuid.NewString() + "@example.com"
	xr := mustInviteStaff(t, srv, taTok, tn.ID, x, "support", uuid.Nil)
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+ta.ID.String()+"/suspend", paTok, map[string]string{"reason_code": "investigation"}), http.StatusOK, "", "suspend the inviter")
	if status, _, _ := lcStaffRow(t, pool, tn.ID, uuid.MustParse(xr.ID)); status != "offboarded" {
		t.Fatalf("the suspended inviter's pending invitation: %s, want offboarded", status)
	}
	if n := lcCount(t, pool, tn.ID, `SELECT count(*) FROM staff_lifecycle_events WHERE staff_user_id = $1 AND event = 'invite_revoked' AND reason_code = 'issuer_removed'`, uuid.MustParse(xr.ID)); n != 1 {
		t.Fatalf("invite_revoked / issuer_removed events: %d", n)
	}
	lcExpect(t, func() lcResp {
		r := staffFixturePost(t, srv, "/v1/staff/invitations/accept", map[string]string{"token": lastMailedToken(t, mail, x), "password": "a-decent-password-1"})
		return lcResp(r)
	}(), http.StatusUnauthorized, "invalid_enrolment_token", "accept after the inviter's suspension")

	// 2. Cross scope: a platform admin invites tenant staff, then is suspended by another platform admin. The
	// suspension (platform scope) cannot see the tenant invitation, so the row stays invited - and the acceptance
	// refuses it on the issuer check (audit reason issuer_not_live).
	pa2 := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa2-sr2-pw-1")
	pa2Tok := mustLoginStaff(t, srv, "", pa2.Email, "pa2-sr2-pw-1").AccessToken
	y := "sr2-y-" + uuid.NewString() + "@example.com"
	yr := mustInviteStaff(t, srv, pa2Tok, tn.ID, y, "support", uuid.Nil)
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/platform-staff/"+pa2.ID.String()+"/suspend", paTok, map[string]string{"reason_code": "investigation"}), http.StatusOK, "", "suspend the platform inviter")
	if status, _, _ := lcStaffRow(t, pool, tn.ID, uuid.MustParse(yr.ID)); status != "invited" {
		t.Fatalf("non-vacuity: the cross-scope invitation must still be invited, got %s", status)
	}
	r := staffFixturePost(t, srv, "/v1/staff/invitations/accept", map[string]string{"token": lastMailedToken(t, mail, y), "password": "a-decent-password-1"})
	if r.status != http.StatusUnauthorized || (lcResp(r)).code() != "invalid_enrolment_token" {
		t.Fatalf("accept after the cross-scope inviter's suspension: %d %s", r.status, r.body)
	}
	if status, _, _ := lcStaffRow(t, pool, tn.ID, uuid.MustParse(yr.ID)); status != "invited" {
		t.Fatalf("a refused acceptance must change nothing, got %s", status)
	}
	if n := lcCount(t, pool, tn.ID, `SELECT count(*) FROM audit_log WHERE action = 'staff.enrolment_token_rejected' AND metadata->>'reason' = 'issuer_not_live' AND target_id = $1`, yr.ID); n != 1 {
		t.Fatalf("issuer_not_live audit rows: %d", n)
	}

	// 3. The other order: accepted BEFORE the inviter's suspension -> the account stays active.
	ta2 := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ta2-sr2-pw-1")
	ta2Tok := mustLoginStaff(t, srv, tn.Slug, ta2.Email, "ta2-sr2-pw-1").AccessToken
	z := "sr2-z-" + uuid.NewString() + "@example.com"
	zr := mustInviteStaff(t, srv, ta2Tok, tn.ID, z, "support", uuid.Nil)
	fixtureAcceptAndEnrol(t, srv, lastMailedToken(t, mail, z), "a-decent-password-1")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+ta2.ID.String()+"/suspend", paTok, map[string]string{"reason_code": "investigation"}), http.StatusOK, "", "suspend after the acceptance")
	if status, _, _ := lcStaffRow(t, pool, tn.ID, uuid.MustParse(zr.ID)); status != "active" {
		t.Fatalf("an invitation accepted before the inviter's suspension stays active, got %s", status)
	}
}

func TestFixRound_SR2_MFAResetRedeemRefusedOnceTheIssuerIsSuspended(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mail := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sr2r-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-sr2r-pw-1").AccessToken
	base := "/v1/admin/tenants/" + tn.ID.String() + "/staff/"
	redeem := func(addr, password string) lcResp {
		r := staffFixturePost(t, srv, "/v1/staff/auth/enrol/redeem", map[string]string{"token": lastMailedToken(t, mail, addr), "tenant_slug": tn.Slug, "email": addr, "password": password})
		return lcResp(r)
	}
	for _, sc := range []struct {
		name     string
		platform bool
	}{{"same-scope tenant issuer (token revoked)", false}, {"cross-scope platform issuer (issuer check)", true}} {
		t.Run(sc.name, func(t *testing.T) {
			addr := "sr2r-" + uuid.NewString() + "@example.com"
			w := mustInviteStaff(t, srv, paTok, tn.ID, addr, "support", uuid.Nil)
			fixtureAcceptAndEnrol(t, srv, lastMailedToken(t, mail, addr), "a-decent-password-1")
			var issuerID uuid.UUID
			var issuerTok, suspendPath string
			if sc.platform {
				p := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pi-sr2r-pw-1")
				issuerID, issuerTok = p.ID, mustLoginStaff(t, srv, "", p.Email, "pi-sr2r-pw-1").AccessToken
				suspendPath = "/v1/admin/platform-staff/" + p.ID.String() + "/suspend"
			} else {
				ta := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ti-sr2r-pw-1")
				issuerID, issuerTok = ta.ID, mustLoginStaff(t, srv, tn.Slug, ta.Email, "ti-sr2r-pw-1").AccessToken
				suspendPath = base + ta.ID.String() + "/suspend"
			}
			lcExpect(t, lcDo(t, srv, http.MethodPost, base+w.ID+"/mfa-reset", issuerTok, map[string]string{"reason_code": "device_lost"}), http.StatusOK, "", "mfa reset")
			lcExpect(t, lcDo(t, srv, http.MethodPost, suspendPath, paTok, map[string]string{"reason_code": "investigation"}), http.StatusOK, "", "suspend the issuer")
			lcExpect(t, redeem(addr, "a-decent-password-1"), http.StatusUnauthorized, "invalid_enrolment_token", "redeem after the issuer's suspension")
			scope := tn.ID
			if sc.platform {
				scope = uuid.Nil
			}
			if status, _, _ := lcStaffRow(t, pool, scope, issuerID); status != "suspended" {
				t.Fatalf("non-vacuity: the issuer must be suspended, got %s", status)
			}
		})
	}
	// Control: an issuer still active -> the redemption proceeds (the check is not a blanket refusal).
	addr := "sr2r-ok-" + uuid.NewString() + "@example.com"
	w := mustInviteStaff(t, srv, paTok, tn.ID, addr, "support", uuid.Nil)
	fixtureAcceptAndEnrol(t, srv, lastMailedToken(t, mail, addr), "a-decent-password-1")
	lcExpect(t, lcDo(t, srv, http.MethodPost, base+w.ID+"/mfa-reset", paTok, map[string]string{"reason_code": "device_lost"}), http.StatusOK, "", "mfa reset (control)")
	lcExpect(t, redeem(addr, "a-decent-password-1"), http.StatusOK, "", "redeem with a live issuer")
}

func TestFixRound_SR3_ApproverSeesInvitePersonAndInUseRefused(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mail := newLifecycleTestServer(t, pool, issuer)
	a := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sr3a-pw-1")
	b := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sr3b-pw-1")
	aTok := mustLoginStaff(t, srv, "", a.Email, "pa-sr3a-pw-1").AccessToken
	bTok := mustLoginStaff(t, srv, "", b.Email, "pa-sr3b-pw-1").AccessToken

	// Filing with a Person already bound to a live platform account (a's own Person): 409.
	lcExpect(t, lcDo(t, srv, http.MethodPost, lcRequests, bTok, map[string]string{"kind": "platform_invite", "email": "sr3-" + uuid.NewString() + "@example.com", "person_id": a.PersonID.String()}),
		http.StatusConflict, "conflict", "invite with a Person in use")

	// A fresh Person: the request shows it, with no live link.
	p := mustSeedPerson(t, pool)
	invitee := "sr3-ok-" + uuid.NewString() + "@example.com"
	req := lcCreateRequest(t, srv, aTok, map[string]string{"kind": "platform_invite", "email": invitee, "person_id": p.String()})
	if req.InvitePersonID == nil || *req.InvitePersonID != p.String() || req.InvitePersonStaff == nil || len(req.InvitePersonStaff) != 0 {
		t.Fatalf("the created request must carry invite_person_id and an empty invite_person_staff: %+v", req)
	}
	// F5: the approve answers email_delivery.
	ok := lcDo(t, srv, http.MethodPost, lcRequests+"/"+req.ID+"/approve", bTok, map[string]string{"payload_hash": req.PayloadHash})
	lcExpect(t, ok, http.StatusOK, "", "approve")
	var done platformStaffLifecycleRequestDTO
	if err := json.Unmarshal(ok.body, &done); err != nil || done.EmailDelivery != "sent" || mailCount(mail, invitee) != 1 {
		t.Fatalf("approve must answer email_delivery=sent: %+v %v", done, err)
	}

	// The Person becomes bound AFTER filing (a direct row, standing in for a concurrent link): the approver sees the
	// binding, and the execution refuses (409) and invites nobody.
	q := mustSeedPerson(t, pool)
	invitee2 := "sr3-late-" + uuid.NewString() + "@example.com"
	req2 := lcCreateRequest(t, srv, aTok, map[string]string{"kind": "platform_invite", "email": invitee2, "person_id": q.String()})
	bound := mustCreateStaffWithPerson(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sr3q-pw-1", &q)
	g := lcDo(t, srv, http.MethodGet, lcRequests+"/"+req2.ID, bTok, nil)
	var seen platformStaffLifecycleRequestDTO
	if g.status != http.StatusOK || json.Unmarshal(g.body, &seen) != nil || len(seen.InvitePersonStaff) != 1 ||
		seen.InvitePersonStaff[0].ID != bound.ID.String() || seen.InvitePersonStaff[0].Status != "active" {
		t.Fatalf("the approver must see the Person's live platform link: %d %s", g.status, g.body)
	}
	// The list carries the same view (it enriches after closing its result set: a nested query on the open rows
	// answered 503 "conn busy" before).
	l := lcDo(t, srv, http.MethodGet, lcRequests, bTok, nil)
	var list platformStaffLifecycleRequestListDTO
	if l.status != http.StatusOK || json.Unmarshal(l.body, &list) != nil {
		t.Fatalf("list: %d %s", l.status, l.body)
	}
	found := false
	for _, x := range list.Requests {
		if x.ID == req2.ID {
			found = len(x.InvitePersonStaff) == 1 && x.InvitePersonStaff[0].ID == bound.ID.String()
		}
	}
	if !found {
		t.Fatalf("the list must show the invitee Person's live platform link: %s", l.body)
	}
	lcExpect(t, lcDo(t, srv, http.MethodPost, lcRequests+"/"+req2.ID+"/approve", bTok, map[string]string{"payload_hash": req2.PayloadHash}),
		http.StatusConflict, "conflict", "execute an invite whose Person is now in use")
	if mailCount(mail, invitee2) != 0 {
		t.Fatal("a refused execution must invite nobody")
	}
}

func TestFixRound_SR4_PersonLinkRoutes(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _ := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ta-sr4-pw-1")
	taTok := mustLoginStaff(t, srv, tn.Slug, ta.Email, "ta-sr4-pw-1").AccessToken
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sr4-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-sr4-pw-1").AccessToken
	tenantLink := func(staffID uuid.UUID) string {
		return "/v1/admin/tenants/" + tn.ID.String() + "/staff/" + staffID.String() + "/person-link"
	}
	platformLink := func(staffID uuid.UUID) string { return "/v1/admin/platform-staff/" + staffID.String() + "/person-link" }
	body := func() map[string]string { return map[string]string{"person_id": mustSeedPerson(t, pool).String()} }

	// Self links: 403 self_target_forbidden on both routes.
	lcExpect(t, lcDo(t, srv, http.MethodPost, tenantLink(ta.ID), taTok, body()), http.StatusForbidden, "self_target_forbidden", "tenant self link")
	lcExpect(t, lcDo(t, srv, http.MethodPost, platformLink(pa.ID), paTok, body()), http.StatusForbidden, "self_target_forbidden", "platform self link")

	// Role authority: a tenant admin may not link the platform-granted roles (the other lifecycle routes' rule).
	for _, role := range []identity.StaffRole{identity.StaffRoleFinance, identity.StaffRoleRiskManager} {
		target := mustCreateStaff(t, pool, tn.ID, role, "tgt-sr4-pw-1")
		lcExpect(t, lcDo(t, srv, http.MethodPost, tenantLink(target.ID), taTok, body()), http.StatusForbidden, "role_not_grantable", "tenant admin links "+string(role))
	}

	// A support account without a Person: a tenant admin may link it (204); an existing link is 409.
	su := mustCreateUnlinkedStaff(t, pool, tn.ID, identity.StaffRoleSupport, "su-sr4-pw-1")
	lcExpect(t, lcDo(t, srv, http.MethodPost, tenantLink(su.ID), taTok, body()), http.StatusNoContent, "", "tenant admin links support")
	lcExpect(t, lcDo(t, srv, http.MethodPost, tenantLink(su.ID), taTok, body()), http.StatusConflict, "conflict", "relink")

	// In-transaction re-assertion: a stale caller (sv bumped) is 401 session_revoked and links nothing.
	su2 := mustCreateUnlinkedStaff(t, pool, tn.ID, identity.StaffRoleSupport, "su2-sr4-pw-1")
	frBumpSV(t, pool, tn.ID, ta.ID)
	lcExpect(t, lcDo(t, srv, http.MethodPost, tenantLink(su2.ID), taTok, body()), http.StatusUnauthorized, "session_revoked", "stale tenant caller")
	if n := lcCount(t, pool, tn.ID, `SELECT count(*) FROM staff_users WHERE id = $1 AND person_id IS NOT NULL`, su2.ID); n != 0 {
		t.Fatal("a refused link must write nothing")
	}

	// Platform route: a LEGACY unlinked platform admin (pre-0139 rows are not backfilled).
	legacy := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "legacy-sr4-pw-1").ID
	// A Person already bound to another live platform account is refused (409) for an approve-capable role.
	lcExpect(t, lcDo(t, srv, http.MethodPost, platformLink(legacy), paTok, map[string]string{"person_id": pa.PersonID.String()}), http.StatusConflict, "conflict", "link a Person in use")
	lcExpect(t, lcDo(t, srv, http.MethodPost, platformLink(legacy), paTok, body()), http.StatusNoContent, "", "platform link")
	// A stale platform caller: 401.
	legacy2 := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "legacy2-sr4-pw-1").ID
	frBumpSV(t, pool, uuid.Nil, pa.ID)
	lcExpect(t, lcDo(t, srv, http.MethodPost, platformLink(legacy2), paTok, body()), http.StatusUnauthorized, "session_revoked", "stale platform caller")
	// A tenant caller on the platform route: 403.
	ta2 := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleTenantAdmin, "ta2-sr4-pw-1")
	ta2Tok := mustLoginStaff(t, srv, tn.Slug, ta2.Email, "ta2-sr4-pw-1").AccessToken
	lcExpect(t, lcDo(t, srv, http.MethodPost, platformLink(legacy2), ta2Tok, body()), http.StatusForbidden, "forbidden", "tenant caller on the platform route")
}

func TestFixRound_SR9_AcceptPasswordFloor(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mail := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-sr9-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-sr9-pw-1").AccessToken
	addr := "sr9-" + uuid.NewString() + "@example.com"
	created := mustInviteStaff(t, srv, paTok, tn.ID, addr, "support", uuid.Nil)
	tok := lastMailedToken(t, mail, addr)
	r := staffFixturePost(t, srv, "/v1/staff/invitations/accept", map[string]string{"token": tok, "password": "elevenchars"})
	lcExpect(t, lcResp(r), http.StatusBadRequest, "validation_error", "an 11-character password")
	if status, _, _ := lcStaffRow(t, pool, tn.ID, uuid.MustParse(created.ID)); status != "invited" {
		t.Fatalf("a refused acceptance must change nothing: %s", status)
	}
	// Exactly 12 is accepted (the token was not consumed by the refusal).
	fixtureAcceptAndEnrol(t, srv, tok, "twelve-chars")
}

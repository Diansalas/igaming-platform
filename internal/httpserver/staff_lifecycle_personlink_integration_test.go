//go:build integration

package httpserver

// ADR 0119 S5 + S6 (section 13.1): linking a Person to a staff member is a staff_version change (SF012 bumps it when
// person_id changes) and ends every session of the target (stafflifecycle.PersonLinkedInTx): the target's
// previously-live token is refused with 401 session_revoked (sv mismatch, and the session row is revoked).

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestLifecycle_PersonLink_BumpsStaffVersionAndEndsSessions(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mail := newLifecycleTestServer(t, pool, issuer)
	tn := mustCreateTenant(t, pool)
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-pl-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "pa-pl-pw-1").AccessToken

	addr := "lc-pl-" + uuid.NewString() + "@example.com"
	created := mustInviteStaff(t, srv, paTok, tn.ID, addr, "support", uuid.Nil)
	sid := uuid.MustParse(created.ID)
	pair := fixtureAcceptAndEnrol(t, srv, lastMailedToken(t, mail, addr), "a-decent-password-1")
	if r := lcDo(t, srv, http.MethodGet, "/v1/staff/me/mfa", pair.AccessToken, nil); r.status != http.StatusOK {
		t.Fatalf("the accepted staff member's session: %d %s", r.status, r.body)
	}
	_, _, svBefore := lcStaffRow(t, pool, tn.ID, sid)
	if lcLiveSessions(t, pool, tn.ID, sid) == 0 {
		t.Fatal("non-vacuity: the target must hold a live session before the link")
	}

	person := mustSeedPerson(t, pool)
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/tenants/"+tn.ID.String()+"/staff/"+created.ID+"/person-link", paTok,
		map[string]string{"person_id": person.String()}), http.StatusNoContent, "", "person link")

	_, _, svAfter := lcStaffRow(t, pool, tn.ID, sid)
	if svAfter != svBefore+1 {
		t.Fatalf("staff_version %d -> %d, want +1", svBefore, svAfter)
	}
	if n := lcLiveSessions(t, pool, tn.ID, sid); n != 0 {
		t.Fatalf("%d session(s) of the target survived the person link", n)
	}
	lcExpect(t, lcDo(t, srv, http.MethodGet, "/v1/staff/me/mfa", pair.AccessToken, nil), http.StatusUnauthorized, "session_revoked", "the pre-link token")
}

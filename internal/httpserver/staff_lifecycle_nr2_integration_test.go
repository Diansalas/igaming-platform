//go:build integration

package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// Security re-review NR-2: two LEGACY unlinked platform accounts held by one human must not be able to cross-link each
// other (and then act as two distinct Persons on a four-eyes request). A person link by an actor that has no Person is
// refused (403, nothing written), on the platform route and on the tenant route.
func TestNR2_PersonLinkByAnActorWithoutAPersonRefused(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _ := newLifecycleTestServer(t, pool, issuer)
	a := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "nr2-a-pw-1")
	b := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "nr2-b-pw-1")
	aTok := mustLoginStaff(t, srv, "", a.Email, "nr2-a-pw-1").AccessToken
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/platform-staff/"+b.ID.String()+"/person-link", aTok,
		map[string]string{"person_id": mustSeedPerson(t, pool).String()}), http.StatusForbidden, "forbidden", "unlinked actor links another platform account")
	if n := lcCount(t, pool, uuid.Nil, `SELECT count(*) FROM staff_users WHERE id = $1 AND person_id IS NOT NULL`, b.ID); n != 0 {
		t.Fatal("a refused link must write nothing")
	}
	tn := mustCreateTenant(t, pool)
	su := mustCreateUnlinkedStaff(t, pool, tn.ID, identity.StaffRoleSupport, "nr2-su-pw-1")
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/tenants/"+tn.ID.String()+"/staff/"+su.ID.String()+"/person-link", aTok,
		map[string]string{"person_id": mustSeedPerson(t, pool).String()}), http.StatusForbidden, "forbidden", "unlinked actor on the tenant route")
	// Control: a linked platform admin may link b.
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "nr2-pa-pw-1")
	paTok := mustLoginStaff(t, srv, "", pa.Email, "nr2-pa-pw-1").AccessToken
	lcExpect(t, lcDo(t, srv, http.MethodPost, "/v1/admin/platform-staff/"+b.ID.String()+"/person-link", paTok,
		map[string]string{"person_id": mustSeedPerson(t, pool).String()}), http.StatusNoContent, "", "linked actor (control)")
}

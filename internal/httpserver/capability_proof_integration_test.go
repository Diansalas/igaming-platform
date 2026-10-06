//go:build integration

package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/capability"
)

// SIGNED-ACTOR-PROOF for K1 over real HTTP (ADR 0110, review finding H2): the
// capability routes sign for the AUTHENTICATED principal (token subject) after
// the route's permission check, NOT through the integration test hook. The test
// hook is switched off for the duration of the test, so a request/approve/
// cancel/revoke flow can only succeed if the handler's own WithProofActor
// context carries the principal.
func TestCapabilityAPI_H2_ProofsAreSignedForTheAuthenticatedPrincipal_NotTheTestHook(t *testing.T) {
	a := newCGAPI(t)
	saved := capability.TestSessionProofHook
	capability.TestSessionProofHook = nil
	t.Cleanup(func() { capability.TestSessionProofHook = saved })

	tenantID := a.tenant()
	tenantAdmin := a.staff(tenantID, "tenant_admin")
	finance := a.staff(tenantID, "finance")
	platform := a.staff(uuid.Nil, "platform_admin")
	tTok := a.token(tenantAdmin, tenantID, auth.RoleTenantAdmin)
	pTok := a.token(platform, uuid.Nil, auth.RolePlatformAdmin)
	base := "/v1/admin/tenants/" + tenantID.String() + "/capability-grants"

	create := func() string {
		resp := a.do(http.MethodPost, base+"/requests", tTok, map[string]any{
			"grantee_staff_id": finance.String(), "capability": "ledger_adjustment:initiate", "reason_code": "test",
		})
		if resp.status != http.StatusCreated {
			t.Fatalf("create_request (tenant admin, proof signed by the handler): %d %s", resp.status, resp.body)
		}
		var c struct {
			ID string `json:"id"`
		}
		resp.decode(t, &c)
		return c.ID
	}
	// tenant cancel (initiator, proof-bound) ...
	id := create()
	if resp := a.do(http.MethodPost, base+"/requests/"+id+"/cancel", tTok, nil); resp.status != http.StatusNoContent {
		t.Fatalf("cancel_request: %d %s", resp.status, resp.body)
	}
	// ... platform approve (grant created), platform revoke.
	id = create()
	resp := a.do(http.MethodPost, base+"/requests/"+id+"/approve", pTok, map[string]any{"reason_code": "test"})
	if resp.status != http.StatusOK {
		t.Fatalf("approve_request (platform, NULL-tenant proof): %d %s", resp.status, resp.body)
	}
	var g struct {
		ID string `json:"id"`
	}
	resp.decode(t, &g)
	if resp := a.do(http.MethodPost, base+"/"+g.ID+"/revoke", pTok, map[string]any{"reason_code": "test"}); resp.status != http.StatusNoContent {
		t.Fatalf("revoke_grant: %d %s", resp.status, resp.body)
	}
}

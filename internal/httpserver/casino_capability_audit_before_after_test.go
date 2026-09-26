//go:build integration

// Stage 10.3 CAS-CAP-ROLLBACK-1 step 8 (F6/G-5): the capability write
// audit record must carry before/after state of every supports_*/status/
// supported_assets field, not just the after-state fields it recorded
// before this stage (F6). This closes a CLAUDE.md audit gap: "every
// mutating administrative/financial action writes an audit record ...
// before/after state".
package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// TestWriteCasinoCapability_AuditRecordsBeforeAfter is G-5: the FIRST
// write (no prior row) records before=null; a SUBSEQUENT write (enable ->
// disable) records the prior state as before and the new state as after -
// every supports_*/status/priority field, not just status/priority (F6).
func TestWriteCasinoCapability_AuditRecordsBeforeAfter(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-g5-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-g5-pw-1")

	firstResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody())
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on first write, got %d", firstResp.StatusCode)
	}
	var firstBody map[string]any
	if err := json.NewDecoder(firstResp.Body).Decode(&firstBody); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	firstResp.Body.Close()
	capabilityID, _ := firstBody["id"].(string)
	if capabilityID == "" {
		t.Fatal("expected a capability id in the first write's response")
	}

	firstMeta := mustGetLatestTenantAuditMetadata(t, pool, tenant.ID, "casino_provider_capability.configured", capabilityID)
	if before, ok := firstMeta["before"]; !ok || before != nil {
		t.Fatalf("expected the FIRST write's audit record to have before=null, got %+v", firstMeta["before"])
	}
	after1, ok := firstMeta["after"].(map[string]any)
	if !ok {
		t.Fatalf("expected an 'after' object in the first write's audit metadata, got %+v", firstMeta)
	}
	if after1["status"] != "active" || after1["supports_bet"] != true {
		t.Fatalf("expected after.status=active, after.supports_bet=true on the first write, got %+v", after1)
	}

	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"
	secondResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	if secondResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on second write, got %d", secondResp.StatusCode)
	}
	secondResp.Body.Close()

	secondMeta := mustGetLatestTenantAuditMetadata(t, pool, tenant.ID, "casino_provider_capability.configured", capabilityID)
	before2, ok := secondMeta["before"].(map[string]any)
	if !ok {
		t.Fatalf("expected a 'before' object in the second write's audit metadata (the prior enabled state), got %+v", secondMeta)
	}
	if before2["status"] != "active" {
		t.Fatalf("expected before.status=active (the prior state) on the second write, got %+v", before2)
	}
	after2, ok := secondMeta["after"].(map[string]any)
	if !ok {
		t.Fatalf("expected an 'after' object in the second write's audit metadata, got %+v", secondMeta)
	}
	if after2["status"] != "disabled" {
		t.Fatalf("expected after.status=disabled on the second write, got %+v", after2)
	}
}

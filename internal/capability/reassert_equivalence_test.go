package capability_test

// Ledger-finance review LF-3: capability.reassertActor re-implements db.CheckActorStaffLive (import cycle). This test
// runs both on one table of cases and requires the same verdict, so neither can drift alone.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func TestReassertDecision_EquivalentToCheckActorStaffLive(t *testing.T) {
	me := uuid.New()
	type c struct {
		name         string
		actor        uuid.UUID
		wantRole     string
		wantSV       int64
		visible      bool
		role, status string
		sv           int64
		live         bool
	}
	cases := []c{
		{"live", me, "platform_admin", 3, true, "platform_admin", "active", 3, true},
		{"suspended", me, "platform_admin", 3, true, "platform_admin", "suspended", 3, false},
		{"offboarded", me, "platform_admin", 3, true, "platform_admin", "offboarded", 3, false},
		{"invited", me, "platform_admin", 3, true, "platform_admin", "invited", 3, false},
		{"role mismatch", me, "platform_admin", 3, true, "tenant_admin", "active", 3, false},
		{"sv behind", me, "platform_admin", 3, true, "platform_admin", "active", 4, false},
		{"sv ahead", me, "platform_admin", 4, true, "platform_admin", "active", 3, false},
		{"not visible", me, "platform_admin", 3, false, "", "", 0, false},
		{"nil actor", uuid.Nil, "platform_admin", 1, true, "platform_admin", "active", 1, false},
		{"empty role", me, "", 1, true, "", "active", 1, false},
		{"zero sv", me, "platform_admin", 0, true, "platform_admin", "active", 0, false},
		{"tenant admin live", me, "tenant_admin", 1, true, "tenant_admin", "active", 1, true},
	}
	for _, k := range cases {
		capErr := capability.ReassertDecisionForTest(k.actor, k.wantRole, k.wantSV, k.visible, k.role, k.status, k.sv)
		var rows []db.ActorStaffRow
		if k.visible {
			rows = []db.ActorStaffRow{{ID: k.actor, Role: k.role, Status: k.status, StaffVersion: k.sv}}
		}
		dbErr := db.CheckActorStaffLive(rows, k.actor, db.ActorExpectation{Role: k.wantRole, StaffVersion: k.wantSV})
		if (capErr == nil) != k.live || (dbErr == nil) != k.live {
			t.Errorf("%s: capability=%v db=%v, want live=%v", k.name, capErr, dbErr, k.live)
		}
	}
}

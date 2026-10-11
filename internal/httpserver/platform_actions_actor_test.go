package httpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// ADR 0114 S0: DB-free proof that actor identity on the platform-actions projection is an explicit
// allow-list (payments_kill_switch.*), in the entry and in the approval chain.
func TestPlatformActionsActor_AllowListOnly(t *testing.T) {
	ks, k1, k2 := uuid.New(), uuid.New(), uuid.New()
	name := "Ops Person"
	names := map[uuid.UUID]*string{ks: &name, k1: &name, k2: &name}
	key := "switch-1"
	ttype := "payment_kill_switch"
	now := time.Now()
	engage := platformActionRawRow{rowID: "r1", action: "payments_kill_switch.engage", actorID: &ks, targetType: &ttype, targetID: &key, createdAt: now}
	// a non-kill-switch row that nevertheless keys into the same chain (kill_switch_id metadata)
	sneaky := platformActionRawRow{rowID: "r2", action: "capability_grant.approved", actorID: &k1, createdAt: now, metadata: map[string]any{"kill_switch_id": key}}
	entries := buildPlatformActionEntries([]platformActionRawRow{engage, sneaky}, []platformActionRawRow{engage, sneaky}, names, audit.Presentation{})
	for _, e := range entries {
		switch e.Action {
		case "payments_kill_switch.engage":
			if e.Actor.StaffID != ks.String() || e.Actor.DisplayName == nil {
				t.Errorf("kill-switch actor must stay identified: %+v", e.Actor)
			}
			for _, c := range e.ApprovalChain {
				if c.ActorStaffID != "" || c.ActorDisplayName != nil {
					t.Errorf("a non-kill-switch chain entry must be scope-only: %+v", c)
				}
			}
		default:
			if e.Actor.StaffID != "" || e.Actor.DisplayName != nil || e.Actor.Scope != "platform" {
				t.Errorf("%s must be scope-only: %+v", e.Action, e.Actor)
			}
			for _, c := range e.ApprovalChain {
				if strings.Contains(c.ActorStaffID, "-") && c.Action != "payments_kill_switch.engage" {
					t.Errorf("chain leaks %+v", c)
				}
			}
		}
	}
	if actorIdentifiedToTenant("capability_grant.requested") || actorIdentifiedToTenant("ledger_adjustment.submit") ||
		actorIdentifiedToTenant("launch_authorisation.approved") || actorIdentifiedToTenant("payment.manual_resolution_denied") || !actorIdentifiedToTenant("payments_kill_switch.approve_release") {
		t.Error("allow-list wrong")
	}
}

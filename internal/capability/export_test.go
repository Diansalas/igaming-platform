package capability

import "github.com/google/uuid"

// ReassertDecisionForTest exposes actorLiveDecision to the external equivalence test (test builds only).
func ReassertDecisionForTest(actor uuid.UUID, wantRole string, wantVersion int64, visible bool, role, status string, version int64) error {
	var row *actorRow
	if visible {
		row = &actorRow{Role: role, Status: status, StaffVersion: version}
	}
	return actorLiveDecision(ProofActor{Actor: actor, Role: wantRole, StaffVersion: wantVersion}, row)
}

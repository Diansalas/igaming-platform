package payments

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AllowAllDepositKYCGate is an explicit, clearly-named test-only stand-in
// for DepositKYCGate. It is NOT a default and must never be reachable
// from non-test code (PRH-I1 deposit cutover, ADR 0095 §27) - production
// wiring (cmd/platform-api) always constructs KYCEnforcementDepositGate
// (kycgate.go). Every constructor that accepts a DepositKYCGate documents
// that this value means "KYC deposit enforcement is NOT ACTIVE", never
// "no KYC required by policy".
type AllowAllDepositKYCGate struct{}

func (AllowAllDepositKYCGate) EvaluateDeposit(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, string) (bool, string, error) {
	return true, "", nil
}

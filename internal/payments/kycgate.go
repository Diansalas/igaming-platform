// PRH-I1 step (b): the deposit KYC gate seam (ADR 0095 §4.3 T1+T2 "the
// KYC deposit gate (ADR 0096)"; T2 "the KYC deposit gate (LF95-C10(e))").
//
// STATUS: this is an interface seam, not the finished gate. ADR 0096's
// enforcement function is being built by `identity-compliance` in PRH-I3,
// in parallel with this work, and is named generically by ADR 0095 §27
// as "the ADR 0096 enforcement function exported by internal/kyc". At
// the time this step was written it was not yet merged to
// origin/claude/focused-wright-jw88w9. Per the orchestrator's instruction,
// this package therefore defines the interface its own call site needs
// and ships a clearly-named, explicitly-labeled stand-in
// (AllowAllDepositKYCGate) - not a TODO comment - so that wiring the real
// PRH-I3 function in is a one-line adapter-construction change, never a
// call-site rewrite. AllowAllDepositKYCGate must NEVER be used outside a
// test or a MOCK-only environment; InitiateDepositAttempt takes the gate
// as a required constructor argument specifically so the caller (the
// eventual httpserver wiring) cannot forget to pass a real one.
package payments

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DepositKYCGate is the payments-side seam for ADR 0096's deposit
// enforcement point. It runs inside the SAME phase-A transaction as the
// attempt claim/insert (ADR 0095 §4.3 T1+T2/T2), after RG and before the
// kill-switch predicate, so a deny commits the intent as declined with no
// attempt row and no provider call - exactly like the existing RG check
// today.
type DepositKYCGate interface {
	// EvaluateDeposit reports whether playerAccountID may deposit amount
	// of assetCode right now. A false result must carry a non-empty,
	// canonical (never free-text) denyReason.
	EvaluateDeposit(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, amount int64, assetCode string) (allowed bool, denyReason string, err error)
}

// AllowAllDepositKYCGate is the explicit, clearly-named stand-in used
// until PRH-I3's real gate lands. It is NOT a default - callers must
// choose it deliberately, and every constructor that accepts a
// DepositKYCGate documents that this value means "KYC deposit
// enforcement is NOT ACTIVE", never "no KYC required by policy".
type AllowAllDepositKYCGate struct{}

func (AllowAllDepositKYCGate) EvaluateDeposit(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, int64, string) (bool, string, error) {
	return true, "", nil
}

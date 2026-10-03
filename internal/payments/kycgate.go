// PRH-I1 step (b): the deposit KYC gate seam (ADR 0095 §4.3 T1+T2 "the
// KYC deposit gate (ADR 0096)"; T2 "the KYC deposit gate (LF95-C10(e))").
//
// CUTOVER (PRH-I1 deposit cutover, ADR 0095 §27): the real gate,
// KYCEnforcementDepositGate, wraps internal/kyc.EvaluateEnforcement (ADR
// 0096 §15 implementation record) and is now the ONLY production
// DepositKYCGate constructed by cmd/platform-api (registrations.go). The
// earlier explicitly-labeled stand-in, AllowAllDepositKYCGate, has been
// moved to a _test.go file in this package (kycgate_mock_test.go) - it
// must never be reachable from non-test code, since it is not "no KYC
// required by policy", it is "KYC deposit enforcement is NOT ACTIVE".
package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// DepositKYCGate is the payments-side seam for ADR 0096's deposit
// enforcement point. It runs inside the SAME phase-A transaction as the
// attempt claim/insert (ADR 0095 §4.3 T1+T2/T2), after RG and before the
// kill-switch predicate, so a deny commits the intent as declined with no
// attempt row and no provider call - exactly like the existing RG check
// today.
//
// personID is the RG-resolved Person for this deposit (ADR 0096's
// enforcement key is per Person, not per PlayerAccount - see
// kyc.EnforcementParams.PersonID's own doc comment) - both call sites
// (InitiateDepositAttempt's phase A, driveCreatedAttempt's cascade T2)
// already compute it via rg.EvaluateEligibility immediately before this
// call, so the gate never needs its own extra identity lookup.
type DepositKYCGate interface {
	// EvaluateDeposit reports whether playerAccountID may deposit amount
	// of assetCode right now. A false result must carry a non-empty,
	// canonical (never free-text) denyReason.
	EvaluateDeposit(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerAccountID, personID uuid.UUID, amount int64, assetCode string) (allowed bool, denyReason string, err error)
}

// KYCEnforcementDepositGate is the real DepositKYCGate, wired to ADR
// 0096's EvaluateEnforcement. It never calls a KYCProvider/vendor itself
// (EvaluateEnforcement is a pure DB read plus in-process comparison) and
// takes no row lock beyond what EvaluateEnforcement's own plain SELECTs
// take.
//
// Any non-nil error from EvaluateEnforcement, or a decision whose Outcome
// is OutcomeUnavailable, is a DENY (ADR 0096 §2.2/§2.6(c): "any non-nil
// error is a DENY at the call site"; OutcomeUnavailable already carries
// Allowed=false), never treated as "no KYC required".
type KYCEnforcementDepositGate struct{}

func (KYCEnforcementDepositGate) EvaluateDeposit(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerAccountID, personID uuid.UUID, amount int64, assetCode string) (bool, string, error) {
	params := kyc.EnforcementParams{
		TenantID: tenantID, BrandID: brandID, PlayerAccountID: playerAccountID, PersonID: personID,
		Operation: kyc.EnforcementDeposit, AssetCode: assetCode, Amount: amount, CorrelationID: uuid.New(),
	}
	decision, err := kyc.EvaluateEnforcement(ctx, tx, params)
	if err != nil {
		return false, "", fmt.Errorf("payments: evaluate kyc enforcement: %w", err)
	}
	// DECISION-ROWS-1 (PRH-2 F-pay, ADR 0096 §3.6/§7.6, security F-3): ONE
	// kyc_enforcement_decisions row (plus its audit) per evaluation - allow,
	// deny and unavailable alike - written HERE, in the caller's phase-A
	// transaction (InitiateDepositAttempt's T1+T2, driveCreatedAttempt's
	// cascade T2). The row records the EVALUATION, not the claim: this gate
	// runs BEFORE routing (phase A) and before the parent lock and claim
	// (cascade T2), so an `allow` row is followed by a no-routable-provider
	// or kill-switch decline in the same, committing transaction and stays
	// in place. An auditor must not infer from an allow row that a deposit
	// proceeded; read the intent/attempt state for that (FP-1, ADR 0096
	// §24.2). A deny row commits with the declined intent (the call site
	// finalizes the intent in this same tx and returns nil, so the row is
	// not rolled back). The payout path differs on purpose: it writes the
	// allow row after routing. That tx contains no provider I/O (IC F4):
	// the adapter call runs only after its commit. An `unavailable`
	// evaluation reads inside a savepoint (KYC-ENF-OUTAGE-1), so tx is still
	// usable for this insert.
	if err := kyc.RecordDecision(ctx, tx, params, decision); err != nil {
		return false, "", fmt.Errorf("payments: record deposit kyc decision: %w", err)
	}
	if !decision.Allowed {
		return false, decision.Code, nil
	}
	return true, "", nil
}

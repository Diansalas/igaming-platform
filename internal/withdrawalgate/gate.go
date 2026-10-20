// Package withdrawalgate holds the CONTRACT of the WD-RG-1 withdrawal gate (owner decision 2026-10-15): the RG
// half (account and wallet status) and the risk half (the central risk engine) of the decision on a NEW
// withdrawal request. It is a leaf package on purpose: internal/withdrawal imports it, and it imports nothing
// of the platform, so internal/withdrawal imports neither internal/rg nor internal/risk. That matters because
// two packages' own tests import internal/withdrawal (internal/risk through its integration test support, and
// internal/wallet directly), and internal/rg imports internal/wallet while internal/withdrawal -> risk would
// close a loop: a withdrawal -> rg or withdrawal -> risk edge is a test-only import cycle.
//
// The implementation that calls rg and risk is internal/withdrawalgate/standard. withdrawal.RequestParams
// carries the gate as a REQUIRED dependency (nil is a fail-closed refusal, like the payout-instrument gate),
// so no caller can create a request without the gate by forgetting it.
package withdrawalgate

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Gate codes (ADR 0113 WD class, application level; they are NOT SQLSTATEs - see internal/withdrawal/rggate.go).
const (
	// CodeRG: the RG half refused (account or wallet not active).
	CodeRG = "WD050"
	// CodeRisk: the risk evaluation returned DENY or REVIEW.
	CodeRisk = "WD051"
	// CodeRestriction (migration 0182, lic11): the tenant's CONFIGURED withdrawal restriction policy holds or blocks the
	// withdrawal at a dispatch point because a restriction cause (a licence state) is in force. Never produced by a Gate
	// implementation: internal/withdrawal evaluates the policy before the gate. A default never refuses.
	CodeRestriction = "WD056"
)

// Input is the server-resolved identity and amount of a NEW withdrawal request. The payment method is not
// known yet at request time (it follows from the payout instrument, which is locked after the gate to respect
// the ADR 0082 lock order), so a risk rule scoped to a payment method fails closed (the evaluator's
// missing-scope refusal) rather than being skipped.
type Input struct {
	TenantID, BrandID, PlayerAccountID, WalletID uuid.UUID
	AssetCode                                    string
	Amount                                       int64
}

// Decision is the gate's outcome. When Allowed is false, GateCode names the half (CodeRG / CodeRisk), Reason
// is that half's stable decision code and Outcome (risk only) is the risk engine's closed word. When Allowed is
// true, RestrictionInForce names a self-exclusion / time-out that did NOT block the withdrawal (ADR 0115 allows
// withdrawals during both) so the request's audit record can say so.
type Decision struct {
	Allowed            bool
	GateCode           string
	Reason             string
	Message            string
	Outcome            string
	RestrictionInForce string
	// Restriction is the withdrawal restriction policy provenance evaluated WITH this decision (migration 0182). It is set
	// by internal/withdrawal (EvaluateDispatchGate), never by a Gate implementation (any value a Gate returns is
	// overwritten), and recorded on the withdrawal_gate_decisions row. nil only for a decision built by a test fixture.
	Restriction *RestrictionProvenance
}

// RestrictionProvenance is the closed, recordable provenance of a withdrawal restriction policy evaluation.
type RestrictionProvenance struct {
	// Source is "configured", "default_unconfigured" (the interim engineering default) or "none" (no cause in force).
	Source string
	// SetVersion is the tenant's policy set version read by the evaluation (0 = nothing configured).
	SetVersion int64
	// Cause is the deciding cause ("" when Source is "none").
	Cause string
	// Action is the effective action of the deciding cause ("allow" when no cause is in force).
	Action string
}

// Gate evaluates the RG and risk policy for a new withdrawal request inside the caller's transaction. It may
// take the L0.4 person lock, so the caller must run it before any L1+ lock (ADR 0082 R8). An error means
// "no decision": the request is not accepted and the caller's transaction aborts. A denial is a Decision, not
// an error; the CALLER writes the denial audit record.
type Gate interface {
	Evaluate(ctx context.Context, tx pgx.Tx, in Input) (Decision, error)
}

// Package standard is the production implementation of withdrawalgate.Gate: the RG half calls
// rg.EvaluateWithdrawalEligibility and the risk half asks the central risk engine (internal/risk, ADR 0031) for
// a decision on Operation 'withdrawal'. See package withdrawalgate for why it is a separate package.
//
// The RG half: the player account must be 'active' and the wallet 'active'. A withdrawal is NOT refused while
// the Person is self-excluded or in a time-out, nor by an exhausted deposit / loss / wager limit or
// rg_config_missing: ADR 0115 2.3, 3.1, 3.2 and 6 keep a player's own funds reachable while a protective
// control is in force (owner-reversible interpretation, docs/plans/a6-rg-enforcement-record.md).
//
// The risk half: ALLOW proceeds; DENY and REVIEW both refuse (no compliance-review queue exists to route a
// REVIEW to, the same disclosed simplification as casino launch); a non-nil error or an unrecognised outcome is
// an ERROR (no decision), never an allow (ADR 0031 section 6).
package standard

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// Gate implements withdrawalgate.Gate.
type Gate struct{}

// New returns the production gate.
func New() Gate { return Gate{} }

var _ withdrawalgate.Gate = Gate{}

// Evaluate runs the RG half (it takes the L0.4 person lock) and, if it allows, the risk half. The licensing
// model is resolved server-side from the tenant row, never from the request.
func (Gate) Evaluate(ctx context.Context, tx pgx.Tx, in withdrawalgate.Input) (withdrawalgate.Decision, error) {
	d, err := rg.EvaluateWithdrawalEligibility(ctx, tx, rg.EligibilityParams{
		TenantID: in.TenantID, BrandID: in.BrandID, PlayerAccountID: in.PlayerAccountID, WalletID: in.WalletID,
	})
	if err != nil {
		return withdrawalgate.Decision{}, fmt.Errorf("standard gate: rg: %w", err)
	}
	if !d.Allowed {
		return withdrawalgate.Decision{Allowed: false, GateCode: withdrawalgate.CodeRG, Reason: d.Code, Message: d.Message}, nil
	}

	tn, err := identity.GetTenantByID(ctx, tx, in.TenantID)
	if err != nil {
		return withdrawalgate.Decision{}, fmt.Errorf("standard gate: resolve tenant licensing mode: %w", err)
	}
	rd, err := risk.Evaluate(ctx, tx, risk.RiskRequest{
		TenantID: in.TenantID, BrandID: in.BrandID, PlayerAccountID: in.PlayerAccountID,
		LicensingMode: tn.LicensingModel, Product: "payments", Operation: risk.OperationWithdrawal,
		AssetCode: in.AssetCode, Amount: in.Amount,
	})
	if err != nil {
		return withdrawalgate.Decision{}, fmt.Errorf("standard gate: risk: %w", err)
	}
	switch rd.Outcome {
	case risk.OutcomeAllow:
		return withdrawalgate.Decision{Allowed: true, RestrictionInForce: d.RestrictionInForce, Outcome: string(rd.Outcome)}, nil
	case risk.OutcomeDeny, risk.OutcomeReview:
		return withdrawalgate.Decision{Allowed: false, GateCode: withdrawalgate.CodeRisk, Reason: rd.Code, Message: rd.Message, Outcome: string(rd.Outcome)}, nil
	default:
		return withdrawalgate.Decision{}, fmt.Errorf("standard gate: unrecognised risk outcome %q", rd.Outcome)
	}
}

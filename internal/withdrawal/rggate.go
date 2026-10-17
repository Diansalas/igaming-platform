package withdrawal

// WD-RG-1 (owner decision 2026-10-15; completion audit A6): the RG and RISK gate on a NEW withdrawal
// request.
//
// What the gate is, exactly (every line is an interpretation of ADR 0115 and the owner decision; the
// reversible ones are listed in docs/plans/a6-rg-enforcement-record.md):
//
//   - RG: the player account must be 'active' and the wallet 'active' (the same status refusals as
//     gameplay). A withdrawal is NOT refused while the Person is self-excluded or in a time-out, and is not
//     refused by an exhausted deposit / loss / wager limit or by rg_config_missing: ADR 0115 2.3, 3.1, 3.2
//     and 6 keep a player's own funds reachable while a protective control is in force. The restriction in
//     force is recorded in the request's audit trail.
//   - RISK: the central risk engine with Operation 'withdrawal' (an operation the risk vocabulary has
//     declared since ADR 0031 but no code evaluated until now). ALLOW proceeds; DENY and REVIEW refuse; an
//     evaluation error refuses and aborts (fail closed, never ALLOW).
//
// Both halves live behind the injected withdrawalgate.Gate (production: withdrawalgate/standard), because
// internal/withdrawal must not import internal/rg or internal/risk (see package withdrawalgate for the
// import-cycle reasons). The gate is a REQUIRED part of RequestParams: nil is a refusal
// (ErrGateUnavailable), so a caller cannot skip it by forgetting it.
//
// Placement (ADR 0082, the forward note of rv-prh-architect 4.1): the RG half takes the L0.4 person lock,
// so the gate runs right after the tenant/brand status gate and BEFORE the payout-instrument gate (its
// FOR SHARE lock on the instrument row) and before every ledger lock. No withdrawal_requests row exists
// yet at that point. A replay of an existing idempotency key returns before the gate (ADR 0096 s8 item 3:
// a status change after the original request never turns a placed hold into a denial).
//
// Denial semantics mirror the KYC denial (LF-I3-3): the denial AUDIT record is written in THIS
// transaction and a typed *GateDeniedError is returned; the caller captures it, returns nil from its
// WithTenant closure so the audit commits (and nothing else: no row, no hold), and answers the player.
// The idempotency key is not consumed.
//
// Check codes. The WD class of ADR 0113 (WD010-012 subject active, WD020-025 state machine, WD030-034
// hold linkage, WD040-043 hold legs) gains the application-level gate codes withdrawalgate.CodeRG (WD050)
// and CodeRisk (WD051). They are NOT SQLSTATEs: no database trigger raises them (a database cannot evaluate
// a risk rule, and an RG-status backstop is a recorded residual); they are the closed reason vocabulary of
// the denial audit record and of GateDeniedError.
//
// Dispatch (F4, closes A6 residual 4 / SR6 I-2): the payout dispatch points that lead to a NEW Withdraw
// call (payments T1p, T2, T12) re-run this same gate before their L1 lock (ADR 0082 N1), and migration
// 0169 backstops them in the database: see dispatchgate.go. Because the database cannot evaluate a risk
// rule, the WD050 / WD051 codes themselves stay application-level; the backstop's own SQLSTATEs are
// WD052 (account / wallet status re-checked in SQL) and WD053 (no allowing decision recorded in the
// transaction).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// Gate codes (re-exported from the contract package for callers and tests of this package).
const (
	GateCodeRG   = withdrawalgate.CodeRG
	GateCodeRisk = withdrawalgate.CodeRisk
)

// ErrGateUnavailable: the RG or risk evaluation could not produce a decision (or no gate was supplied).
// Retryable, nothing was written; never a policy decision and never shown as one.
var ErrGateUnavailable = errors.New("withdrawal: rg/risk gate evaluation unavailable")

// GateDeniedError is returned by RequestWithdrawal when the WD-RG-1 gate refuses a NEW request. The
// denial audit record is already written in the caller's transaction (see the file comment).
type GateDeniedError struct {
	// GateCode is GateCodeRG or GateCodeRisk.
	GateCode string
	// Reason is the decision code (an rg.Code* value or a risk.Code* value).
	Reason  string
	Message string
}

func (e *GateDeniedError) Error() string {
	return fmt.Sprintf("withdrawal: refused by the rg/risk gate (%s: %s)", e.GateCode, e.Reason)
}

// evaluateRGRiskGate runs the gate for a new request. It returns a *GateDeniedError (after writing the
// denial audit) on a refusal, an error wrapping ErrGateUnavailable when it could not decide, and the
// restriction in force (empty if none) on success.
func evaluateRGRiskGate(ctx context.Context, tx pgx.Tx, p RequestParams) (restrictionInForce string, err error) {
	if p.Gate == nil {
		return "", fmt.Errorf("%w: no gate was supplied", ErrGateUnavailable)
	}
	d, err := p.Gate.Evaluate(ctx, tx, withdrawalgate.Input{
		TenantID: p.TenantID, BrandID: p.BrandID, PlayerAccountID: p.PlayerAccountID, WalletID: p.WalletID,
		AssetCode: p.AssetCode, Amount: p.Amount,
	})
	if err != nil {
		// A non-nil error is NEVER an ALLOW (ADR 0031 6): the request is not accepted and the
		// transaction aborts. The cause stays server-side.
		return "", fmt.Errorf("%w: %w", ErrGateUnavailable, err)
	}
	if d.Allowed {
		return d.RestrictionInForce, nil
	}
	if d.GateCode != GateCodeRG && d.GateCode != GateCodeRisk {
		return "", fmt.Errorf("%w: unrecognised gate code %q", ErrGateUnavailable, d.GateCode)
	}
	action := "withdrawal.denied_by_rg"
	var extra map[string]any
	if d.GateCode == GateCodeRisk {
		action = "withdrawal.denied_by_risk"
		extra = map[string]any{"risk_outcome": d.Outcome}
	}
	if aerr := auditGateDenial(ctx, tx, p, action, d.GateCode, d.Reason, extra); aerr != nil {
		return "", aerr
	}
	return "", &GateDeniedError{GateCode: d.GateCode, Reason: d.Reason, Message: d.Message}
}

func auditGateDenial(ctx context.Context, tx pgx.Tx, p RequestParams, action, gateCode, reason string, extra map[string]any) error {
	md := map[string]any{
		"gate_code": gateCode, "reason_code": reason, "brand_id": p.BrandID.String(), "wallet_id": p.WalletID.String(),
		"asset_code": p.AssetCode, "amount": p.Amount,
	}
	for k, v := range extra {
		md[k] = v
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: p.TenantID, ActorType: audit.ActorPlayer, ActorID: p.PlayerAccountID, Action: action,
		TargetType: "player_account", TargetID: p.PlayerAccountID.String(), Outcome: audit.OutcomeDenied, Metadata: md,
		IPAddress: p.IPAddress, UserAgent: p.UserAgent, RequestID: p.RequestID,
	}); err != nil {
		return fmt.Errorf("withdrawal: audit gate denial: %w", err)
	}
	return nil
}

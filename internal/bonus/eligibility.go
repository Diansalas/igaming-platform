// The three-way live gate every Bonus Engine value-moving checkpoint runs
// (docs/architecture/10-bonus-engine-architecture.md "doc 10" §T.1):
//
//	AssetAuthorization.CheckEligibility -> rg.EvaluateEligibility -> risk.Evaluate
//
// This is Bonus Engine's own composition decision (T.1), not a claim of
// authority over ADR 0031 item (f)'s platform-wide question. All three
// calls happen inside the SAME database transaction as the Grant's own
// state-changing effect, before that effect commits (T.1's own binding
// rule) - every call site in this package that reaches a value-moving
// checkpoint MUST call GateCheckpoint (or GateActivationOrRewardCredit)
// inside its own already-open transaction, never in a separate one.
package bonus

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
)

// GateOutcome is the result of running the three-way gate. Exactly one of
// Allowed or a denial reason is populated - never a fourth, uninstructed
// outcome (doc 10 T.3/T.12).
type GateOutcome struct {
	Allowed bool

	// DeniedBy names which of the three checks fired, for the Progress
	// entry's own reason-code discipline (doc 10 §10.1): "asset_authorization"
	// | "rg" | "risk".
	DeniedBy string
	// Code is that check's own reason/decision code, carried verbatim
	// (never re-derived) onto the Progress entry.
	Code string
	// Message is a human-readable denial detail, never itself the
	// authoritative reason - Code is.
	Message string
}

// GateParams is every field the three-way gate's three calls need,
// resolved by the caller from authenticated server-side context only
// (CLAUDE.md) - never client-supplied.
type GateParams struct {
	TenantID         uuid.UUID
	BrandID          uuid.UUID
	PlayerAccountID  uuid.UUID
	WalletID         uuid.UUID
	JurisdictionID   uuid.UUID // jurisdictions.id - resolved by the caller; zero value is an immediate AssetAuthorization denial, never "unscoped" (ADR 0037 §C.2)
	JurisdictionCode string    // risk_rules' own scoping dimension - a plain code string, resolved identically to internal/casino's pattern
	LicensingMode    string
	AssetCode        string
	Amount           int64 // minor units, zero is valid for an amount-less operation (RiskRequest.Amount's existing contract)
	RiskOperation    risk.Operation

	// SkipAssetAuthorization is true ONLY at Grant creation ((none) ->
	// issued, doc 10 §T.2): "Not checked at creation at all:
	// AssetAuthorization.CheckEligibility for any operation. issued is 'a
	// decision, not a movement'... a Grant decision that has not yet
	// moved any value into a wallet has nothing for an authorization gate
	// to protect." Every other checkpoint (activation, reward credit,
	// conversion) leaves this false and runs the full three-way gate.
	SkipAssetAuthorization bool
}

// GateCheckpoint runs doc 10 §T.1's three-way gate:
// AssetAuthorization (operation = wagering, per doc 10's Genuine Gap 1
// resolution - now closed by internal/assetregistry's real
// OperationWagering constant) -> RG -> Risk, in that fixed order,
// short-circuiting on the first denial - or, at Grant creation only
// (SkipAssetAuthorization), RG -> Risk (doc 10 §T.2).
//
// tx must already be the caller's own open transaction; this function
// never opens or commits one. Every call is expected to run inside the
// SAME transaction as the checkpoint's own posting/state change (T.1).
func GateCheckpoint(ctx context.Context, tx pgx.Tx, p GateParams) (GateOutcome, error) {
	if !p.SkipAssetAuthorization {
		assetAuth := assetregistry.AssetAuthorization{}
		eligible, reason, err := assetAuth.CheckEligibility(ctx, tx, p.TenantID, p.BrandID, p.JurisdictionID, p.AssetCode,
			assetregistry.OperationScope{Product: "bonus", Operation: assetregistry.OperationWagering})
		if err != nil || !eligible {
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			return GateOutcome{Allowed: false, DeniedBy: "asset_authorization", Code: string(reason), Message: msg}, nil
		}
	}

	rgDecision, err := rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{
		TenantID: p.TenantID, BrandID: p.BrandID, PlayerAccountID: p.PlayerAccountID, WalletID: p.WalletID,
	})
	if err != nil {
		return GateOutcome{}, fmt.Errorf("bonus: rg.EvaluateEligibility: %w", err)
	}
	if !rgDecision.Allowed {
		return GateOutcome{Allowed: false, DeniedBy: "rg", Code: rgDecision.Code, Message: rgDecision.Message}, nil
	}

	riskDecision, err := risk.Evaluate(ctx, tx, risk.RiskRequest{
		TenantID: p.TenantID, BrandID: p.BrandID, PlayerAccountID: p.PlayerAccountID,
		JurisdictionCode: p.JurisdictionCode, LicensingMode: p.LicensingMode,
		Product: "bonus", Operation: p.RiskOperation, AssetCode: p.AssetCode, Amount: p.Amount,
	})
	// A non-nil Risk error is ALWAYS a denial (doc 10 §4; ADR 0031 §1/§25)
	// - this is also how a not-yet-existent Operation value would fail
	// closed automatically via risk.ErrUnknownOperation, with no
	// special-casing needed here. ("bonus_conversion" was exactly such a
	// value until Stage 4H-B1 Wave 2 Phase 4 added it to
	// internal/risk.Operation's closed set - see conversion.go's own doc
	// comment.)
	if err != nil {
		return GateOutcome{Allowed: false, DeniedBy: "risk", Code: "risk_evaluation_error", Message: err.Error()}, nil
	}
	// REVIEW is treated as a block, identically to DENY, until a
	// compliance-review queue exists (doc 10 §4/ADR 0031 §17).
	if riskDecision.Outcome != risk.OutcomeAllow {
		return GateOutcome{Allowed: false, DeniedBy: "risk", Code: riskDecision.Code, Message: riskDecision.Message}, nil
	}

	return GateOutcome{Allowed: true}, nil
}

// resolveJurisdictionID looks up jurisdictions.id from a jurisdictions.code
// value (ADR 0006's registry). Bonus Engine consumes jurisdiction context
// exactly as internal/risk/internal/casino do - a plain scope-matching
// value resolved server-side by whatever caller establishes it (doc 10
// §7) - and, per casino's own precedent (orchestrator.go's
// "TODO(jurisdiction)"), no per-player jurisdiction resolver exists
// anywhere in this codebase yet. An empty code returns uuid.Nil, which
// AssetAuthorization.CheckEligibility treats as an immediate, correct
// denial (never "unscoped, skip the layer") - this is the SAME
// known, platform-wide gap casino already carries, not a new one this
// package invents a workaround for.
func resolveJurisdictionID(ctx context.Context, tx pgx.Tx, code string) (uuid.UUID, error) {
	if code == "" {
		return uuid.Nil, nil
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM jurisdictions WHERE code = $1`, code).Scan(&id)
	if err == pgx.ErrNoRows {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("bonus: resolve jurisdiction id: %w", err)
	}
	return id, nil
}

// resolveLicensingMode mirrors internal/casino's own
// resolveLicensingMode exactly (doc 10 §4: "LicensingMode from
// tenants.licensing_model via the identical resolveLicensingMode
// pattern") - re-implemented here rather than imported, since
// internal/casino does not export it and this package must not import an
// unrelated domain's internal helper.
func resolveLicensingMode(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (string, error) {
	var mode string
	err := tx.QueryRow(ctx, `SELECT licensing_model FROM tenants WHERE id = $1`, tenantID).Scan(&mode)
	if err != nil {
		return "", fmt.Errorf("bonus: resolve licensing mode: %w", err)
	}
	return mode, nil
}

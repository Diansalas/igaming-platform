package adjustment

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// holdCheck is the AML verdict for a wallet's player plus the ids needed to record the real reason in the AML-scoped evidence.
type holdCheck struct {
	verdict   aml.HoldVerdict
	tenantID  uuid.UUID
	accountID uuid.UUID
	personID  uuid.UUID
}

func (h holdCheck) denied() bool { return h.verdict.Denied() }

// refusal is the error a hold-caused refusal returns. It renders EXACTLY like an ordinary conflict (ErrClassConflict, no SQLSTATE,
// no AML token anywhere: security F4, option (b), a reversible orchestrator default, not owner-approved). The real reason travels in
// the error value only so the Service can write it to the owner-only AML decision log AFTER the rolled-back transaction.
func (h holdCheck) refusal(requestID uuid.UUID) error {
	return &amlRefusal{holdCheck: h, requestID: requestID}
}

// amlRefusal satisfies errors.Is(err, ErrAccountUnderReview) (an internal sentinel, never rendered) and carries the evidence.
type amlRefusal struct {
	holdCheck
	requestID uuid.UUID
}

func (*amlRefusal) Error() string        { return ErrAccountUnderReview.Error() }
func (*amlRefusal) Is(target error) bool { return target == ErrAccountUnderReview }

// walletHoldCheck evaluates the AML hold for the player that owns walletID, for the operation manual_adjustment (ANY active hold
// refuses: ADR 0116 s5.1 "Held balances"). It runs inside the caller's transaction and NEVER raises the evaluator-failure alert
// there: every caller refuses by returning an error (its transaction rolls back), so the alert is raised, and the evidence recorded,
// by the Service after the rollback (ledger-finance C3). A wallet that does not resolve is left to the existing payload rules
// (wallet_not_found): no verdict is invented for it.
func walletHoldCheck(ctx context.Context, tx pgx.Tx, tenantID, walletID uuid.UUID) (holdCheck, error) {
	h := holdCheck{tenantID: tenantID}
	err := tx.QueryRow(ctx, `SELECT w.player_account_id, pa.person_id
		FROM wallets w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
		WHERE w.id = $1 AND w.tenant_id = $2`, walletID, tenantID).Scan(&h.accountID, &h.personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return holdCheck{}, nil
	}
	if err != nil {
		h.verdict = aml.HoldVerdict{EvalFailed: true}
		return h, fmt.Errorf("adjustment: resolve the wallet's player for the AML hold check: %w", err)
	}
	h.verdict, err = aml.EvaluateHoldDeferringAlert(ctx, tx, tenantID, h.personID, h.accountID, aml.OpManualAdjustment)
	return h, err
}

// recordAMLRefusal writes the REAL reason of a hold-caused refusal to the owner-only AML decision log (and raises the evaluator-failure
// alert), in their own transactions after the refusing transaction ended. Best effort: the refusal itself stands either way.
func (s *Service) recordAMLRefusal(ctx context.Context, err error) {
	var r *amlRefusal
	if !errors.As(err, &r) || r.tenantID == uuid.Nil {
		return
	}
	dctx := context.WithoutCancel(ctx)
	outcome := aml.HoldOutcomeHeld
	if !r.verdict.Held {
		outcome = aml.HoldOutcomeEvalError
	}
	_ = s.pool.WithTenant(dctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return aml.RecordEnforcementDecision(ctx, tx, aml.EnforcementRecord{
			TenantID: r.tenantID, PlayerAccountID: r.accountID, PersonID: r.personID, Operation: "manual_adjustment",
			KYCOutcome: "not_evaluated", HoldOutcome: outcome, PublicOutcome: "review", Allowed: false,
			HoldID: r.verdict.HoldID, CorrelationID: r.requestID,
		})
	})
	if r.verdict.EvalFailed && !r.verdict.Held {
		_ = aml.RaiseHoldEvalFailedDetached(dctx, s.pool, r.tenantID, aml.OpManualAdjustment)
	}
}

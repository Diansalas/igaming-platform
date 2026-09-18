// The general Grant lifecycle Progress trail (docs/architecture/10-bonus-
// engine-architecture.md "doc 10" §1.1's fourth persistent layer, §10.1's
// completeness requirement), backed by migration 0062's
// bonus_grant_progress table. Deferred by Phase 2, built here.
//
// DISTINCT from WageringProgress (wagering_progress.go, migration 0058):
// that table is narrowly the P_net/P_firm contribution record. This file
// is the append-only trail of every lifecycle fact about a Grant - every
// transition, every gate decision, every reason code - the record §10.1
// says "a disputing player's case is resolved from."
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// TransitionType is bonus_grant_progress.transition_type's closed set
// (migration 0062).
type TransitionType string

const (
	TransitionIssued                        TransitionType = "issued"
	TransitionActivated                     TransitionType = "activated"
	TransitionInProgressContribution        TransitionType = "in_progress_contribution"
	TransitionInProgressContributionReverse TransitionType = "in_progress_contribution_reversed"
	TransitionCompleted                     TransitionType = "completed"
	TransitionConverted                     TransitionType = "converted"
	TransitionConversionBlocked             TransitionType = "conversion_blocked"
	TransitionExpired                       TransitionType = "expired"
	TransitionCancelled                     TransitionType = "cancelled"
	TransitionForfeited                     TransitionType = "forfeited"
	TransitionPendingSettlementDeferred     TransitionType = "pending_settlement_deferred"
	TransitionPendingSettlementFinalized    TransitionType = "pending_settlement_finalized"
	TransitionReversed                      TransitionType = "reversed"
	TransitionActivationDenied              TransitionType = "activation_denied"
	TransitionRewardCreditDenied            TransitionType = "reward_credit_denied"
	TransitionHeldDispositionCreated        TransitionType = "held_disposition_created"
	TransitionHeldDispositionResolved       TransitionType = "held_disposition_resolved"
	TransitionAdjustmentApplied             TransitionType = "adjustment_applied"
	TransitionSuggestionActivationLinked    TransitionType = "suggestion_activation_linked"
)

// TriggerType is bonus_grant_progress.trigger_type's closed set (doc 10
// §1.3's own "trigger type" column).
type TriggerType string

const (
	TriggerAutomatedRuleEvaluation TriggerType = "automated_rule_evaluation"
	TriggerPlayerAction            TriggerType = "player_action"
	TriggerStaffAction             TriggerType = "staff_action"
	TriggerProviderCallback        TriggerType = "provider_callback"
	TriggerSystem                  TriggerType = "system"
)

// reasonCodeRequired mirrors doc 10 §10.1's table: "a reason code
// (mandatory for forfeited/cancelled/reversed)". Enforced here (the one
// Go writer) rather than only by convention, since migration 0062's own
// CHECK cannot express "mandatory for these specific values of another
// column" without duplicating this whole table.
func reasonCodeRequired(t TransitionType) bool {
	switch t {
	case TransitionForfeited, TransitionCancelled, TransitionReversed:
		return true
	default:
		return false
	}
}

// GrantProgressEntry mirrors one bonus_grant_progress row.
type GrantProgressEntry struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	GrantID         uuid.UUID

	SequenceNumber int64

	TransitionType   TransitionType
	TriggerType      TriggerType
	TriggerReference *string

	BeforeStatus *string
	AfterStatus  *string

	ActorType ActorType
	ActorID   *uuid.UUID

	ReasonCode *string

	Amount    *big.Int
	AssetCode *string

	RiskDecisionCode             *string
	RGDecisionCode               *string
	AssetAuthorizationReasonCode *string
	LedgerTransactionID          *uuid.UUID

	CorrelationID *uuid.UUID
	Detail        []byte // JSONB

	OccurredAt time.Time
}

var (
	// ErrReasonCodeRequired is returned by AppendGrantProgress when
	// TransitionType requires one (doc 10 §10.1) and none was supplied.
	ErrReasonCodeRequired = errors.New("bonus: reason code is required for this transition type")
)

const grantProgressColumns = `
	id, tenant_id, brand_id, player_account_id, grant_id, sequence_number,
	transition_type, trigger_type, trigger_reference,
	before_status, after_status, actor_type, actor_id, reason_code,
	amount, asset_code, risk_decision_code, rg_decision_code, asset_authorization_reason_code,
	ledger_transaction_id, correlation_id, detail, occurred_at`

func scanGrantProgressEntry(row rowScanner) (GrantProgressEntry, error) {
	var (
		e              GrantProgressEntry
		transitionType string
		triggerType    string
		actorType      string
		amount         pgtype.Numeric
	)
	err := row.Scan(
		&e.ID, &e.TenantID, &e.BrandID, &e.PlayerAccountID, &e.GrantID, &e.SequenceNumber,
		&transitionType, &triggerType, &e.TriggerReference,
		&e.BeforeStatus, &e.AfterStatus, &actorType, &e.ActorID, &e.ReasonCode,
		&amount, &e.AssetCode, &e.RiskDecisionCode, &e.RGDecisionCode, &e.AssetAuthorizationReasonCode,
		&e.LedgerTransactionID, &e.CorrelationID, &e.Detail, &e.OccurredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return GrantProgressEntry{}, ErrNotFound
	}
	if err != nil {
		return GrantProgressEntry{}, fmt.Errorf("bonus: scan grant progress entry: %w", err)
	}
	e.TransitionType = TransitionType(transitionType)
	e.TriggerType = TriggerType(triggerType)
	e.ActorType = ActorType(actorType)
	if amount.Valid {
		amt, err := numericToBigInt(amount)
		if err != nil {
			return GrantProgressEntry{}, err
		}
		e.Amount = amt
	}
	return e, nil
}

// AppendGrantProgress inserts the next sequence_number for GrantID and
// writes one append-only Progress row, all inside tx (the caller's own
// already-open transaction, which MUST also hold the (tenant_id,
// grant_id) advisory lock per doc 10 §9 before calling this - this
// function does not itself acquire that lock, mirroring
// bonus.LockGrantForUpdate's own division of labor). The sequence number
// is computed as 1 + MAX(sequence_number) for this Grant, read inside the
// SAME transaction under the caller's lock, so it is race-free without a
// separate sequence object.
func AppendGrantProgress(ctx context.Context, tx pgx.Tx, e GrantProgressEntry) (GrantProgressEntry, error) {
	if reasonCodeRequired(e.TransitionType) && (e.ReasonCode == nil || *e.ReasonCode == "") {
		return GrantProgressEntry{}, fmt.Errorf("%w: %s", ErrReasonCodeRequired, e.TransitionType)
	}
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.Detail == nil {
		e.Detail = []byte("{}")
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_grant_progress (
			id, tenant_id, brand_id, player_account_id, grant_id, sequence_number,
			transition_type, trigger_type, trigger_reference,
			before_status, after_status, actor_type, actor_id, reason_code,
			amount, asset_code, risk_decision_code, rg_decision_code, asset_authorization_reason_code,
			ledger_transaction_id, correlation_id, detail
		) VALUES (
			$1,$2,$3,$4,$5,
			1 + COALESCE((SELECT MAX(sequence_number) FROM bonus_grant_progress WHERE tenant_id = $2 AND grant_id = $5), 0),
			$6,$7,$8,
			$9,$10,$11,$12,$13,
			$14,$15,$16,$17,$18,
			$19,$20,$21
		) RETURNING `+grantProgressColumns,
		e.ID, e.TenantID, e.BrandID, e.PlayerAccountID, e.GrantID,
		string(e.TransitionType), string(e.TriggerType), e.TriggerReference,
		e.BeforeStatus, e.AfterStatus, string(e.ActorType), e.ActorID, e.ReasonCode,
		bigIntToNumeric(e.Amount), e.AssetCode, e.RiskDecisionCode, e.RGDecisionCode, e.AssetAuthorizationReasonCode,
		e.LedgerTransactionID, e.CorrelationID, nonNilJSON(e.Detail),
	)
	return scanGrantProgressEntry(row)
}

// ListGrantProgress returns a Grant's full Progress trail, in sequence
// order.
func ListGrantProgress(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) ([]GrantProgressEntry, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+grantProgressColumns+` FROM bonus_grant_progress WHERE tenant_id = $1 AND grant_id = $2 ORDER BY sequence_number ASC`,
		tenantID, grantID,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: list grant progress: %w", err)
	}
	defer rows.Close()
	var out []GrantProgressEntry
	for rows.Next() {
		e, err := scanGrantProgressEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

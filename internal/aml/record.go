package aml

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Hold outcomes recorded in aml_enforcement_decisions.
const (
	HoldOutcomeHeld      = "held"
	HoldOutcomeEvalError = "eval_error"
)

// EnforcementRecord is one row of the AML-scoped enforcement decision log (ADR 0116 s5.1): the TRUE outcome and the matched
// hold of an evaluation whose public result is the generic `review`. Written in the gate's own transaction by
// kyc.RecordDecision, only when a hold (or an evaluator error) took part in the decision.
type EnforcementRecord struct {
	TenantID        uuid.UUID
	PlayerAccountID uuid.UUID
	PersonID        uuid.UUID
	Operation       string // the KYC enforcement operation name (deposit, withdrawal_hold, ...)
	KYCOutcome      string // the KYC side's own outcome before the hold was combined
	HoldOutcome     string // HoldOutcomeHeld | HoldOutcomeEvalError
	PublicOutcome   string // what the tenant-visible log recorded
	Allowed         bool
	HoldID          uuid.UUID // uuid.Nil unless HoldOutcome == held and the match lookup succeeded
	CorrelationID   uuid.UUID
}

// RecordEnforcementDecision calls aml_record_enforcement_decision() (migration 0141) in the caller's transaction.
func RecordEnforcementDecision(ctx context.Context, tx pgx.Tx, r EnforcementRecord) error {
	var holdID *uuid.UUID
	if r.HoldID != uuid.Nil {
		holdID = &r.HoldID
	}
	if r.HoldOutcome != HoldOutcomeHeld && r.HoldOutcome != HoldOutcomeEvalError {
		return fmt.Errorf("%w: hold outcome %q", ErrInvalidInput, r.HoldOutcome)
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT aml_record_enforcement_decision($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.TenantID, r.PlayerAccountID, r.PersonID, r.Operation, r.KYCOutcome, r.HoldOutcome, r.PublicOutcome, r.Allowed, holdID, r.CorrelationID,
	).Scan(&id); err != nil {
		return fmt.Errorf("aml: record enforcement decision: %w", classify(err))
	}
	return nil
}

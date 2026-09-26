package httpserver

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
)

// recordCasinoCallbackRejection writes the Stage 10.3 W2b (CAS-RECON-1)
// rejection record for a VERIFIED casino callback that ReceiveCallback
// rejected with an error (E10, the G-1 409 classes, bet_not_found,
// already_rolled_back, payload_mismatch, round_ownership_conflict).
//
// The callback's own transaction has already been rolled back by
// db.Pool.WithTenant (ReceiveCallback returned a non-nil error), so the row
// is written in a SEPARATE, freshly-opened tenant-scoped transaction - the
// Stage 10.1 PAY-REV-1 "separately committed denial audit" pattern
// (ADR 0088 §4.7), mirrored from newPaymentWebhookHandler's
// ErrDepositAlreadyReversed branch. It changes no money: the only write is
// one INSERT ... ON CONFLICT DO NOTHING into casino_callback_rejections.
//
// Only a *casino.CallbackRejectedError produces a row. ReceiveCallback
// builds one only AFTER verification succeeded, so a pre-verification
// *webhookauth.AuthError (or any other error) is a no-op here - I1 is
// preserved by construction, not by the caller's branch order. A failure
// to write the row is logged and never changes the provider response
// (paper 02 §2.14). tenantID is the route-resolved tenant, never a body
// field.
//
// Detached context (gate 10.3-W2/W3 security review R-1): the row is
// reconciliation evidence (the input to C6/C7), so it must not be lost
// because the provider hung up. The write runs on
// context.WithoutCancel(ctx) - it keeps the request's values but not its
// cancellation or deadline - bounded by casinoRejectionWriteTimeout so a
// slow database cannot hold the handler indefinitely. Every caller (the
// provider webhook route and the play-simulation routes) goes through
// this helper, so all of them get the same guarantee.
func recordCasinoCallbackRejection(ctx context.Context, deps Deps, logger interface {
	Error(string, ...any)
}, tenantID uuid.UUID, requestID string, err error) {
	recordCasinoCallbackRejectionWithin(ctx, casinoRejectionWriteTimeout, deps, logger, tenantID, requestID, err)
}

// casinoRejectionWriteTimeout bounds the detached rejection-record write:
// one INSERT ... ON CONFLICT DO NOTHING in a fresh transaction.
const casinoRejectionWriteTimeout = 2 * time.Second

func recordCasinoCallbackRejectionWithin(ctx context.Context, timeout time.Duration, deps Deps, logger interface {
	Error(string, ...any)
}, tenantID uuid.UUID, requestID string, err error) {
	var rejected *casino.CallbackRejectedError
	if !errors.As(err, &rejected) || deps.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if writeErr := deps.DB.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := casino.RecordCallbackRejection(ctx, tx, tenantID, rejected.ProviderID, rejected.Rejection, requestID)
		return err
	}); writeErr != nil {
		// Allow-listed fields only: never the provider reference, amount
		// or the wrapped error text (which may embed caller-supplied
		// values).
		logger.Error("casino_callback_rejection_record_failed",
			"tenant_id", tenantID.String(), "provider_id", rejected.ProviderID,
			"reason_class", string(rejected.Rejection.Class), "request_id", requestID)
	}
}

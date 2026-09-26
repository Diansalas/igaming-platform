package httpserver

import (
	"context"
	"errors"

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
func recordCasinoCallbackRejection(ctx context.Context, deps Deps, logger interface {
	Error(string, ...any)
}, tenantID uuid.UUID, requestID string, err error) {
	var rejected *casino.CallbackRejectedError
	if !errors.As(err, &rejected) || deps.DB == nil {
		return
	}
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

package casino

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// Stage 10.3 W2b, CAS-RECON-1: the verified-only casino callback rejection
// record (migration 0097; docs/plans/stage-10.3-planning/
// 02-casino-financial-analysis.md §2.6; gate 10.3-W1 ledger-finance C9 and
// its extension; ADR 0025 Stage 10.3 amendment item 9).
//
// A row records ONE fact: "a provider callback that passed signature
// verification asserted a financial event, and the platform refused it
// for reason class R". It is the durable input the casino_consistency
// reconciliation stream (internal/reconciliation, checks C6/C7) turns into
// findings. It never changes money: no ledger, projection, round, session
// or capability row is written by anything in this file.
//
// Verified-only (invariant I1): a CallbackRejection can only be built
// from a CallbackEvent that ReceiveCallback obtained AFTER verifyCallback
// and the adapter's HandleCallback both succeeded. Every pre-verification
// failure returns a *webhookauth.AuthError before any event exists, so an
// unverified caller can never create a row.
//
// Two write paths, chosen by whether the callback's own transaction
// commits:
//
//   - The callback is ACKNOWLEDGED (E3's 200 declined, E9's 200 for a
//     distinct second rollback reference): the row is written inside the
//     callback's own transaction, so it commits atomically with the
//     decision (and with E3's audit row).
//   - The callback is REJECTED with an error (E10, the G-1 409 classes,
//     bet_not_found, already_rolled_back, payload_mismatch,
//     round_ownership_conflict): db.Pool.WithTenant rolls the callback's
//     transaction back, so nothing recorded on it would survive.
//     ReceiveCallback wraps the error in a *CallbackRejectedError carrying
//     the verified event's identifiers, and the HTTP layer records the row
//     in a SEPARATE, freshly-opened tenant-scoped transaction - the Stage
//     10.1 PAY-REV-1 "separately committed denial audit" pattern (ADR 0088
//     §4.7). A failure to write it is logged and never changes the
//     provider response.
//
// Idempotency: the table's UNIQUE (tenant_id, provider_id, event_type,
// provider_tx_id, reason_class) plus INSERT ... ON CONFLICT DO NOTHING -
// a redelivered rejection never adds a row, and concurrent identical
// rejections collapse to one.

// RejectionClass is casino_callback_rejections.reason_class.
type RejectionClass string

const (
	// RejectionOriginalTombstoned: E3 (a bet) or E10 (a win) whose own
	// provider_tx_id a tombstone already covers.
	RejectionOriginalTombstoned RejectionClass = "original_tombstoned"
	// The G-1 409 abort classes.
	RejectionAmbiguousRound      RejectionClass = "ambiguous_round"
	RejectionWalletCollision     RejectionClass = "wallet_collision"
	RejectionMixedFunding        RejectionClass = "mixed_funding"
	RejectionLockAlreadyReleased RejectionClass = "lock_already_released"
	RejectionBonusBetNotLocked   RejectionClass = "bonus_bet_not_locked"
	// Other verified, terminal financial rejections.
	RejectionBetNotFound            RejectionClass = "bet_not_found"
	RejectionAlreadyRolledBack      RejectionClass = "already_rolled_back"
	RejectionPayloadMismatch        RejectionClass = "payload_mismatch"
	RejectionRoundOwnershipConflict RejectionClass = "round_ownership_conflict"
	// RejectionRollbackOfTombstonedOriginal: E9 with a DIFFERENT
	// reference - a second, distinct rollback reference naming an
	// original that is already tombstoned. Acknowledged 200 (the
	// idempotent tombstone result), with no ledger or audit record of its
	// own; this row is its only durable trace.
	RejectionRollbackOfTombstonedOriginal RejectionClass = "rollback_of_tombstoned_original"

	// RejectionTenantNotActive: a verified bet/win/rollback that would have
	// created a NEW posting for a suspended or closed tenant (owner decision
	// R3-GAME-POSTINGS-NONACTIVE-1, ADR 0095 section 40.5). It is NOT a value
	// of casino_callback_rejections.reason_class: the table's classes are
	// ledger-finance-ruled and pinned against its CHECK constraint by the
	// reconciliation partition test, so a new class needs a ruling and a
	// reconciliation change that this decision explicitly excludes.
	// RecordCallbackRejection writes this class as an append-only audit_log
	// row (action casino_callback.rejected_tenant_not_active) instead.
	RejectionTenantNotActive RejectionClass = "tenant_not_active"

	// RejectionBrandNotActive: a verified NEW bet for a player of a brand
	// that is not 'active' (ADR 0112 LF1, slice 2). Like
	// RejectionTenantNotActive it is NOT a casino_callback_rejections
	// reason_class (same reasoning); it is written as an append-only
	// audit_log row (action casino_callback.rejected_brand_not_active).
	RejectionBrandNotActive RejectionClass = "brand_not_active"
)

// AuditActionCallbackRejectedTenantNotActive is the audit_log action that
// carries a RejectionTenantNotActive record.
const AuditActionCallbackRejectedTenantNotActive = "casino_callback.rejected_tenant_not_active"

// AuditActionCallbackRejectedBrandNotActive is the audit_log action that
// carries a RejectionBrandNotActive record.
const AuditActionCallbackRejectedBrandNotActive = "casino_callback.rejected_brand_not_active"

// rejectionClassFor maps a post-verification error from postBet/postWin/
// postRollback to its rejection class. ok=false means "not a recorded
// rejection": a pre-verification failure never reaches the caller of this
// function at all, and the remaining post-verification errors are either
// retryable (ErrProviderUnavailable 503, a generic 500) or structural
// validation failures of the payload itself (ErrInvalidInput,
// ErrCallbackMalformedBody, ErrOutcomeNotSucceeded,
// ErrLaunchSessionRequired) - none of which is a financial rejection
// decision. Order matters only for errors that wrap more than one
// sentinel; ErrProviderTxPayloadMismatch is checked first because
// mapReplayPayloadMismatch wraps a ledger error inside it.
func rejectionClassFor(err error) (RejectionClass, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrTenantNotActive):
		return RejectionTenantNotActive, true
	case errors.Is(err, ErrBrandNotActive):
		return RejectionBrandNotActive, true
	case errors.Is(err, ErrProviderTxPayloadMismatch):
		return RejectionPayloadMismatch, true
	case errors.Is(err, ErrOriginalTombstoned):
		return RejectionOriginalTombstoned, true
	case errors.Is(err, ErrAmbiguousMultiOriginRound):
		return RejectionAmbiguousRound, true
	case errors.Is(err, ErrCorrelationWalletCollision):
		return RejectionWalletCollision, true
	case errors.Is(err, ErrMixedFundingUnsupported):
		return RejectionMixedFunding, true
	case errors.Is(err, ErrLockAlreadyReleased):
		return RejectionLockAlreadyReleased, true
	case errors.Is(err, ErrBonusBetNotLocked):
		return RejectionBonusBetNotLocked, true
	case errors.Is(err, ErrBetNotFound):
		return RejectionBetNotFound, true
	case errors.Is(err, ErrAlreadyRolledBack):
		return RejectionAlreadyRolledBack, true
	case errors.Is(err, ErrProviderRoundOwnershipConflict):
		return RejectionRoundOwnershipConflict, true
	default:
		return "", false
	}
}

// CallbackRejection is one row of the rejection record, built only from a
// verified CallbackEvent.
type CallbackRejection struct {
	Class                RejectionClass
	EventType            CallbackEventType
	ProviderTxID         string
	OriginalProviderTxID string
	RoundID              string
	AssetCode            string
	// Amount is the provider-asserted amount in minor units; ignored (NULL)
	// for a rollback.
	Amount int64
	// SubReason is an internal diagnostic detail recorded only in the audit
	// row of a tenant_not_active refusal (e.g. stake_return_mismatch). It is
	// never part of any HTTP response (no oracle for a caller).
	SubReason string
}

// subReasonFor names why a non-active-tenant stake return was refused.
func subReasonFor(err error) string {
	switch {
	case errors.Is(err, ErrStakeReturnMismatch):
		return "stake_return_mismatch"
	case errors.Is(err, ErrStakeReturnWinOutstanding):
		return "stake_return_round_has_win"
	default:
		return ""
	}
}

func newCallbackRejection(class RejectionClass, event CallbackEvent) CallbackRejection {
	return CallbackRejection{
		Class: class, EventType: event.EventType, ProviderTxID: event.ProviderTxID,
		OriginalProviderTxID: event.OriginalProviderTxID, RoundID: event.RoundID,
		AssetCode: event.AssetCode, Amount: event.Amount,
	}
}

// CallbackRejectedError wraps a verified callback's rejection error with
// the identifiers the rejection record needs. Unwrap returns the original
// error, so every existing errors.Is mapping (HTTP status, log line) is
// unchanged. Error() returns the original error's text unchanged.
type CallbackRejectedError struct {
	// ProviderID is the route-resolved provider id ReceiveCallback was
	// called with (never a body field).
	ProviderID string
	Rejection  CallbackRejection
	Err        error
}

func (e *CallbackRejectedError) Error() string { return e.Err.Error() }
func (e *CallbackRejectedError) Unwrap() error { return e.Err }

// wrapRejection is applied by ReceiveCallback to every dispatch result -
// i.e. only ever AFTER verification succeeded. Curried so it composes with
// a multi-value call: wrapRejection(event)(postX(...)).
func wrapRejection(providerID string, event CallbackEvent) func(ReceiveCallbackResult, error) (ReceiveCallbackResult, error) {
	return func(result ReceiveCallbackResult, err error) (ReceiveCallbackResult, error) {
		class, ok := rejectionClassFor(err)
		if !ok {
			return result, err
		}
		rej := newCallbackRejection(class, event)
		rej.SubReason = subReasonFor(err)
		return result, &CallbackRejectedError{ProviderID: providerID, Rejection: rej, Err: err}
	}
}

// RecordCallbackRejection inserts one rejection row under tx (which must
// be tenant-scoped to tenantID, never player-scoped - the table's RLS
// refuses otherwise). inserted=false means an identical row (same tenant,
// provider, event type, provider_tx_id and class) already existed. It
// writes nothing else.
func RecordCallbackRejection(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, rej CallbackRejection, requestID string) (inserted bool, err error) {
	if tenantID == uuid.Nil || providerID == "" || rej.ProviderTxID == "" || rej.Class == "" {
		return false, fmt.Errorf("%w: a callback rejection requires tenant, provider, provider_tx_id and class", ErrInvalidInput)
	}
	if rej.Class == RejectionTenantNotActive || rej.Class == RejectionBrandNotActive {
		// Durable evidence for staff and reconciliation, in the append-only
		// audit store (no casino_callback_rejections row; see the class doc).
		// The tenant and brand refusals keep distinct actions and reasons.
		action, reason, what := AuditActionCallbackRejectedTenantNotActive, "tenant_not_active", "tenant"
		if rej.Class == RejectionBrandNotActive {
			action, reason, what = AuditActionCallbackRejectedBrandNotActive, "brand_not_active", "brand"
		}
		md := map[string]any{
			"provider_id": providerID, "event_type": string(rej.EventType), "provider_tx_id": rej.ProviderTxID,
			"round_id": rej.RoundID, "asset_code": rej.AssetCode, "reason": reason,
			"request_id": requestID,
		}
		if rej.EventType == CallbackEventRollback {
			md["original_provider_tx_id"] = rej.OriginalProviderTxID
		} else if rej.Amount > 0 {
			md["amount"] = rej.Amount
		}
		if rej.SubReason != "" {
			md["sub_reason"] = rej.SubReason
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: action,
			TargetType: "casino_provider_tx", TargetID: providerID + ":" + rej.ProviderTxID, Outcome: audit.OutcomeDenied,
			RequestID: requestID, Metadata: md,
		}); err != nil {
			return false, fmt.Errorf("casino: audit callback refused for non-active %s: %w", what, err)
		}
		return true, nil
	}
	var original, amount any
	if rej.EventType == CallbackEventRollback {
		original = rej.OriginalProviderTxID
	} else if rej.Amount > 0 {
		amount = rej.Amount
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO casino_callback_rejections
			(tenant_id, provider_id, event_type, provider_tx_id, original_provider_tx_id,
			 round_id, asset_code, amount, reason_class, request_id)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9, NULLIF($10, ''))
		ON CONFLICT (tenant_id, provider_id, event_type, provider_tx_id, reason_class) DO NOTHING`,
		tenantID, providerID, string(rej.EventType), rej.ProviderTxID, original,
		rej.RoundID, rej.AssetCode, amount, string(rej.Class), requestID)
	if err != nil {
		return false, fmt.Errorf("casino: record callback rejection: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// StoredCallbackRejection is a casino_callback_rejections row as read back
// (staff-only, read-only).
type StoredCallbackRejection struct {
	ID                   uuid.UUID
	ProviderID           string
	EventType            string
	ProviderTxID         string
	OriginalProviderTxID *string
	RoundID              *string
	AssetCode            *string
	Amount               *string // NUMERIC(38,0) as text; never float
	ReasonClass          string
	RequestID            *string
	FirstSeenAt          time.Time
}

// ListCallbackRejections reads one page of the tenant's rejection record,
// newest first, plus the total count. Read-only; tx must be tenant-scoped.
func ListCallbackRejections(ctx context.Context, tx pgx.Tx, limit, offset int) ([]StoredCallbackRejection, int, error) {
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("casino: count callback rejections: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, provider_id, event_type, provider_tx_id, original_provider_tx_id, round_id,
		       asset_code, amount::text, reason_class, request_id, first_seen_at
		  FROM casino_callback_rejections
		 ORDER BY first_seen_at DESC, id
		 LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("casino: list callback rejections: %w", err)
	}
	defer rows.Close()
	var out []StoredCallbackRejection
	for rows.Next() {
		var r StoredCallbackRejection
		if err := rows.Scan(&r.ID, &r.ProviderID, &r.EventType, &r.ProviderTxID, &r.OriginalProviderTxID, &r.RoundID,
			&r.AssetCode, &r.Amount, &r.ReasonClass, &r.RequestID, &r.FirstSeenAt); err != nil {
			return nil, 0, fmt.Errorf("casino: scan callback rejection: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// firstTombstoningRollbackReference returns the provider reference of the
// rollback that originally wrote the tombstone tombstoneTxID - read from
// that tombstone's own casino_rollback.tombstoned audit row, written in
// the same transaction as the tombstone itself (postRollback). found=false
// when no such audit row is visible.
//
// Both rows default their timestamp to now() - the SAME transaction start
// time - so audit_log.created_at equals the tombstone's posted_at exactly.
// Filtering on it lets the lookup use idx_audit_log_tenant_time instead of
// scanning the tenant's whole audit history; the target filter then makes
// it exact.
func firstTombstoningRollbackReference(ctx context.Context, tx pgx.Tx, tenantID, tombstoneTxID uuid.UUID) (ref string, found bool, err error) {
	var r *string
	err = tx.QueryRow(ctx, `
		SELECT a.metadata->>'rollback_provider_tx_id'
		  FROM ledger_transactions t
		  JOIN audit_log a
		    ON a.tenant_id = t.tenant_id AND a.created_at = t.posted_at
		 WHERE t.tenant_id = $1 AND t.id = $2
		   AND a.action = 'casino_rollback.tombstoned'
		   AND a.target_type = 'ledger_transaction' AND a.target_id = $2::text
		 ORDER BY a.id
		 LIMIT 1`, tenantID, tombstoneTxID).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("casino: read tombstoning rollback reference: %w", err)
	}
	if r == nil {
		return "", false, nil
	}
	return *r, true, nil
}

// PRH-I1 step (c): the sweeper (ADR 0095 §7), deposit path. Payout-attempt
// sweeping (T2 re-claim, T12 resubmission, QueryStatus resolution) is
// implemented in payout_sweep.go, a separate file kept deliberately
// minimal-diff against this one (this task's own payout-scoped-edits
// instruction) - processAttempt below dispatches to it for
// operation='payout' rows; every other function in THIS file remains
// deposit-only, unchanged. Scope explicitly bounded for the deposit path,
// named here rather than silently assumed:
//
//   - Batch lease claim uses FOR UPDATE SKIP LOCKED per tenant (§7.2
//     point 2) and writes only lease columns, exactly as specified.
//   - 'created', interactive=false rows are routed and driven through
//     driveCreatedAttempt (T2, phase B, phase C, including a further
//     cascade insert on an eligible decline) - drive.go's shared core.
//   - 'created', interactive=true rows past the presence window expire
//     (T3, expired_before_submission).
//   - 'submitting' (lease expired), 'pending' and 'ambiguous' rows are
//     resolved via QueryStatus and the evidence matrix (§4.4).
//
// NOT implemented in this step (residuals, not silently accepted):
//   - T12 (idempotent resubmission of an 'ambiguous' attempt) - an
//     ambiguous attempt converges only via QueryStatus/callback in this
//     step, never a resend.
//   - §4.5's authoritative not_found rule for a deposit - the MOCK
//     adapter's QueryStatus returns a bare Go error for an unknown
//     reference, not a typed not-found result (StatusResult has no Found
//     bool yet, per §9.2's "StatusResult gains Found bool", not built in
//     this step) - a QueryStatus error is therefore always treated as
//     inconclusive (reschedule), never as evidence of a decline.
//   - Global and per-(tenant, provider) concurrency caps (§7.3) - this
//     sweeper processes one tenant, one item at a time.
//   - The kill switch (§7.2 point 3's in-statement predicate) - migration
//     0102 does not exist yet.
//   - Deferred-receipt re-application (§7.1's second paragraph) - no
//     receipt exists yet (that is step (d), callbacks/receipts).
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// Sweeper bounds and defaults (§7.3 RECOMMENDATION values, reduced only
// where this step's scope is narrower than the full spec).
const (
	SweeperDefaultBatchPerTenant  = 20
	SweeperDefaultLease           = 60 * time.Second
	SweeperDefaultPresenceWindow  = 30 * time.Minute
	SweeperDefaultPollBackoffBase = 30 * time.Second
	SweeperDefaultPollBackoffCap  = 30 * time.Minute
)

// Sweeper drives deposit payment_attempts rows past their next_action_at
// (ADR 0095 §7). One Sweeper instance is safe to call RunOnce on
// repeatedly (e.g. from a ticker); it holds no per-call mutable state.
type Sweeper struct {
	Pool         *db.Pool
	Orchestrator *Orchestrator
	KYCGate      DepositKYCGate
	CredResolver OutboundCredentialResolver
	// PayoutKYCGate enables payout-attempt sweeping (payout_sweep.go, this
	// task's items 5/8) when non-nil - a nil value leaves any payout
	// payment_attempts row exactly as leased (processPayoutAttempt's own
	// no-op), matching this file's existing convention for scope this
	// sweeper is not configured to drive.
	PayoutKYCGate PayoutKYCGate
	// MaxResubmits bounds T12 (C1/B1) - see (*Sweeper).maxResubmits().
	// <= 0 means payoutMaxResubmits.
	MaxResubmits int

	BatchPerTenant  int
	Lease           time.Duration
	PresenceWindow  time.Duration
	PollBackoffBase time.Duration
	PollBackoffCap  time.Duration
}

// NewSweeper returns a Sweeper with §7.3's default bounds.
func NewSweeper(pool *db.Pool, orch *Orchestrator, kycGate DepositKYCGate, credResolver OutboundCredentialResolver) *Sweeper {
	return &Sweeper{
		Pool: pool, Orchestrator: orch, KYCGate: kycGate, CredResolver: credResolver,
		BatchPerTenant: SweeperDefaultBatchPerTenant, Lease: SweeperDefaultLease,
		PresenceWindow:  SweeperDefaultPresenceWindow,
		PollBackoffBase: SweeperDefaultPollBackoffBase, PollBackoffCap: SweeperDefaultPollBackoffCap,
	}
}

// SweepStats is RunOnce's summary - test/observability only.
type SweepStats struct {
	Claimed   int
	Processed int
	Errors    []error
}

// RunOnce runs exactly one sweep pass over tenantIDs, in the given
// order (§7.2 point 1: "active tenants in round-robin order" - the
// caller supplies that order; this method does not discover tenants
// itself). A per-item error is recorded in Stats.Errors and does not
// stop the sweep of the remaining items or tenants (one tenant's
// misbehaving provider must never block another's sweep).
func (s *Sweeper) RunOnce(ctx context.Context, tenantIDs []uuid.UUID) SweepStats {
	var stats SweepStats
	for _, tenantID := range tenantIDs {
		ids, err := s.claimBatch(ctx, tenantID)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Errorf("payments: sweeper claim batch for tenant %s: %w", tenantID, err))
			continue
		}
		stats.Claimed += len(ids)
		for _, id := range ids {
			if err := s.processAttempt(ctx, tenantID, id); err != nil {
				stats.Errors = append(stats.Errors, fmt.Errorf("payments: sweeper process attempt %s: %w", id, err))
				continue
			}
			stats.Processed++
		}
	}
	return stats
}

// claimBatch is §7.2 point 2's batch lease tx: SELECT ... FOR UPDATE
// SKIP LOCKED, writing ONLY lease_owner/lease_until/next_action_at. It
// never changes a state, never locks a parent row, and never waits on a
// lock - SKIP LOCKED guarantees that. This is the one exception to
// "parent before attempt" (ADR 0095 §14).
func (s *Sweeper) claimBatch(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	leaseUntil := time.Now().Add(s.Lease)
	var ids []uuid.UUID
	err := s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(actx,
			`SELECT id FROM payment_attempts
			 WHERE tenant_id = $1 AND next_action_at IS NOT NULL AND next_action_at <= now()
			 ORDER BY next_action_at
			 LIMIT $2
			 FOR UPDATE SKIP LOCKED`,
			tenantID, s.batchSize())
		if err != nil {
			return fmt.Errorf("select due attempts: %w", err)
		}
		var due []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			due = append(due, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for _, id := range due {
			if _, err := tx.Exec(actx,
				`UPDATE payment_attempts SET lease_owner = 'sweeper', lease_until = $2, next_action_at = $2, updated_at = now() WHERE id = $1`,
				id, leaseUntil,
			); err != nil {
				return fmt.Errorf("lease attempt %s: %w", id, err)
			}
		}
		ids = due
		return nil
	})
	return ids, err
}

func (s *Sweeper) batchSize() int {
	if s.BatchPerTenant <= 0 {
		return SweeperDefaultBatchPerTenant
	}
	return s.BatchPerTenant
}

func (s *Sweeper) backoff(pollCount int) time.Time {
	base := s.PollBackoffBase
	if base <= 0 {
		base = SweeperDefaultPollBackoffBase
	}
	cap := s.PollBackoffCap
	if cap <= 0 {
		cap = SweeperDefaultPollBackoffCap
	}
	// Exponential with a hard cap; no jitter in this step (a documented
	// simplification - §7.3 asks for "exponential with jitter, capped").
	d := base
	for i := 0; i < pollCount && d < cap; i++ {
		d *= 2
	}
	if d > cap {
		d = cap
	}
	return time.Now().Add(d)
}

// processAttempt re-reads the leased attempt (and, for a 'created' item,
// its parent intent) and dispatches per §7.1's table.
func (s *Sweeper) processAttempt(ctx context.Context, tenantID, attemptID uuid.UUID) error {
	attempt, err := getAttemptInTenant(ctx, s.Pool, tenantID, attemptID)
	if err != nil {
		return err
	}
	if attempt.Operation == AttemptOperationPayout {
		return s.processPayoutAttempt(ctx, tenantID, attempt)
	}
	if attempt.Operation != AttemptOperationDeposit || attempt.DepositIntentID == nil {
		// Out of this step's scope; leave it exactly as leased - it will
		// be reconsidered next tick.
		return nil
	}

	switch attempt.State {
	case AttemptCreated:
		return s.processCreated(ctx, tenantID, attempt)
	case AttemptSubmitting, AttemptPending, AttemptAmbiguous:
		return s.processViaQueryStatus(ctx, tenantID, attempt)
	default:
		// Terminal - nothing to do (next_action_at should already be
		// NULL for a terminal row per the table's own CHECK; reaching
		// here at all would mean the lease claim raced a concurrent
		// terminal transition, which is harmless).
		return nil
	}
}

func (s *Sweeper) processCreated(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	var loadedIntent DepositIntent
	if loadErr := s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		var e error
		loadedIntent, e = GetDepositIntentByID(actx, tx, *attempt.DepositIntentID)
		return e
	}); loadErr != nil {
		return loadErr
	}

	if attempt.Interactive {
		if time.Since(attempt.CreatedAt) < s.presenceWindow() {
			// Not expired yet - leave it; the batch lease already moved
			// next_action_at forward, so it will be reconsidered.
			return nil
		}
		return s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(actx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, loadedIntent.ID); err != nil {
				return err
			}
			if err := RejectCreated(actx, tx, attempt.ID, EvidencePlatform, "expired_before_submission"); err != nil {
				return err
			}
			_, err := s.Orchestrator.finalizeDeclined(actx, tx, loadedIntent, nil, nil, "expired_before_submission")
			return err
		})
	}

	// Non-interactive: drive it (route, T2, phase B/C), looping across
	// any further cascade this decline is eligible for (§4.6 case (b)).
	current := attempt
	for {
		updatedIntent, updatedAttempt, child, _, _, err := s.Orchestrator.driveCreatedAttempt(ctx, s.Pool, s.KYCGate, s.CredResolver, loadedIntent, current, true)
		if err != nil {
			return err
		}
		loadedIntent = updatedIntent
		_ = updatedAttempt
		if child == nil {
			return nil
		}
		current = *child
	}
}

func (s *Sweeper) presenceWindow() time.Duration {
	if s.PresenceWindow <= 0 {
		return SweeperDefaultPresenceWindow
	}
	return s.PresenceWindow
}

// processViaQueryStatus resolves a submitting/pending/ambiguous attempt
// through StatusQuery (§5.3) plus the §4.4 evidence matrix, via the same
// provider-call gate every other outbound call uses (no adapter-method
// call runs while a transaction is held - QueryStatus is called with the
// plain, unmarked ctx here, never one obtained from inside a WithTenant
// callback).
func (s *Sweeper) processViaQueryStatus(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	if attempt.ProviderID == nil || attempt.ProviderReference == nil {
		// No reference yet to look up (e.g. a 'submitting' attempt whose
		// lease expired before the provider ever acknowledged it) -
		// reschedule; there is nothing to query.
		return s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
			return RescheduleNonTerminal(actx, tx, attempt.ID, s.backoff(attempt.PollCount))
		})
	}
	provider, ok := s.Orchestrator.Provider(*attempt.ProviderID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSweeperProviderNotRegistered, *attempt.ProviderID)
	}

	manifest := provider.Capabilities().Manifest
	in := callProviderInput{
		TenantID: tenantID, ProviderID: *attempt.ProviderID,
		AttemptState: attempt.State, ClaimToken: uuid.Nil, ExpectedClaim: uuid.Nil, ReadOnly: true,
		Domain: "payments", Manifest: manifest,
	}
	// QueryStatus is read-only at the provider and always retryable
	// (§5.3); ReadOnly: true above tells the gate to skip the committed-
	// submitting-claim check T4/T7 money calls need (gate.go's own doc
	// comment on ReadOnly) - everything else the gate does (txscope
	// refusal, credential binding, deadline, panic recovery, redaction)
	// still applies identically.
	gr := callProvider(ctx, s.CredResolver, in, func(callCtx context.Context, cc CallContext) (StatusResult, ErrorClass, error) {
		res, err := provider.QueryStatus(callCtx, *attempt.ProviderReference)
		if err != nil {
			return res, ErrorClassAmbiguous, err
		}
		switch res.Outcome {
		case OutcomePending:
			return res, ErrorClassPending, nil
		case OutcomeSucceeded:
			return res, ErrorClassSucceeded, nil
		case OutcomeDeclined:
			return res, ErrorClassDefiniteDecline, nil
		default:
			return res, ErrorClassAmbiguous, nil
		}
	})

	return s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(actx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, *attempt.DepositIntentID); err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(actx, tx, *attempt.DepositIntentID)
		if err != nil {
			return err
		}
		return s.applyStatusEvidence(actx, tx, intent, attempt, gr)
	})
}

// applyStatusEvidence maps one QueryStatus GateResult onto the §4.4
// matrix's submitting/pending/ambiguous rows. Called with the intent's
// parent lock already held.
func (s *Sweeper) applyStatusEvidence(ctx context.Context, tx pgx.Tx, intent DepositIntent, attempt PaymentAttempt, gr GateResult[StatusResult]) error {
	nextPoll := s.backoff(attempt.PollCount)

	if gr.Err != nil || gr.Class == ErrorClassNotSent {
		// Transport/credential failure resolving the query itself, or a
		// gate refusal: inconclusive, never treated as failure (§8's
		// "timeout after dispatch is Ambiguous and never a failure",
		// restated here for the poll path itself).
		return RescheduleNonTerminal(ctx, tx, attempt.ID, nextPoll)
	}

	res := gr.Value
	switch gr.Class {
	case ErrorClassPending:
		if attempt.State == AttemptPending {
			// no-op (reschedule) - already pending, nothing changed.
			return RescheduleNonTerminal(ctx, tx, attempt.ID, nextPoll)
		}
		if err := MarkAccepted(ctx, tx, attempt.ID, EvidenceQueryStatus, res.ProviderReference, nextPoll); err != nil {
			return err
		}
		if _, err := setIntentAttempt(ctx, tx, intent.ID, attempt.ProviderID, &res.ProviderReference, DepositIntentPending); err != nil {
			return err
		}
		// RV-PRH-I1 ledger-finance H2: see drive.go's identical comment -
		// this poll is the sweeper's own T9 site.
		attempt.ProviderReference = &res.ProviderReference
		_, err := ApplyDeferredReceiptsForAttempt(ctx, tx, s.Orchestrator, attempt)
		return err

	case ErrorClassSucceeded:
		// postedTxID, not updated.LedgerTransactionID - PRH-I5 finding
		// (LF95-C6(a)/T13); see drive.go's identical comment.
		_, postedTxID, err := s.Orchestrator.postDepositSuccess(ctx, tx, intent, *attempt.ProviderID, res.ProviderReference, attempt.Amount, attempt.AssetCode)
		if err != nil {
			return err
		}
		if err := ApplySuccess(ctx, tx, attempt.ID, SuccessEvidence{
			Evidence: EvidenceQueryStatus, ProviderReference: res.ProviderReference, LedgerTransactionID: &postedTxID,
		}); err != nil {
			return err
		}
		// RV-PRH-I1 ledger-finance H4: see drive.go's identical comment.
		return rejectCreatedSiblings(ctx, tx, attempt)

	case ErrorClassDefiniteDecline:
		var refPtr *string
		if res.ProviderReference != "" {
			refPtr = &res.ProviderReference
		}
		providerIDStr := ""
		if attempt.ProviderID != nil {
			providerIDStr = *attempt.ProviderID
		}
		reason, err := boundedDeclineReasonAudited(ctx, tx, attempt.TenantID, "payment_attempt", attempt.ID.String(), providerIDStr, res.DeclineReason)
		if err != nil {
			return err
		}
		updated, err := s.Orchestrator.finalizeDeclined(ctx, tx, intent, attempt.ProviderID, refPtr, reason)
		if err != nil {
			return err
		}
		cascadable := res.Cascadable
		if err := ApplyDecline(ctx, tx, attempt.ID, DeclineEvidence{
			Evidence: EvidenceQueryStatus, Reason: reason, Stage: DeclineAfterAcceptance,
			Cascadable: &cascadable, ProviderRef: refPtr,
		}); err != nil {
			return err
		}
		if cascadeEligible(attempt, updated.Status, cascadable, s.Orchestrator.maxCascadeDepth(), true) {
			_, err := insertCascadeAttemptIfEligible(ctx, tx, attempt)
			return err
		}
		return nil

	default: // ErrorClassAmbiguous
		if attempt.State == AttemptPending {
			// T11: the provider "forgot" an accepted attempt - a real
			// state change (P1 anomaly), not a plain reschedule.
			return MarkAmbiguousFromPending(ctx, tx, attempt.ID, EvidenceQueryStatus, nextPoll)
		}
		return RescheduleNonTerminal(ctx, tx, attempt.ID, nextPoll)
	}
}

// ErrSweeperProviderNotRegistered is returned (wrapped) when a lease
// names a provider_id no longer present in the adapter registry - a
// deploy/config mismatch, never expected in steady state.
var ErrSweeperProviderNotRegistered = errors.New("payments: sweeper: provider not registered")

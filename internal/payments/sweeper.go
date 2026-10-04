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

	"github.com/Diansalas/igaming-platform/internal/audit"
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

// SweeperBatchLeaseOwner is claimBatch's OWN lease_owner literal (PAY-SEC-S-L2,
// rv-prh-i1-payout-security.md): distinct from every genuine PER-ITEM
// sweeper-driven lease (drive.go's cascade "sweeper" leaseOwner, and
// payout_sweep.go's "sweeper-payout-reclaim"/"sweeper-payout-resubmit").
// Before this fix, claimBatch and every one of payout.go's
// lease_owner='sweeper' in-flight-guard exemptions (N7) used the SAME bare
// literal "sweeper" - conflating "this row is merely leased by the
// sweeper's OWN batch-claim tx (which never dispatches anything itself)"
// with "this row's REAL dispatch/resend lease happens to be owned by a
// sweeper-driven per-item path" was harmless only because those two
// literals never needed to be told apart. A distinct constant, used ONLY
// by claimBatch and ONLY compared against by the guards that specifically
// mean "the sweeper's own batch lease, never a live dispatch", makes that
// distinction explicit and typo-proof.
const SweeperBatchLeaseOwner = "sweeper-batch"

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

	// TenantAdvisoryHint (PRH-2 H, LF-8) makes claimBatch take a per-tenant
	// pg_try_advisory_xact_lock at the start of the phase-A claim tx and skip
	// the tenant this pass when another instance holds it. It is ONLY an
	// efficiency hint that de-duplicates phase A across instances: it lasts one
	// short transaction, is never held across a provider call, and correctness
	// never depends on it (the row lease with SKIP LOCKED, the claim-token CAS,
	// ledger idempotency and the 0107 indexes carry exactly-once). false (the
	// zero value) is always safe.
	TenantAdvisoryHint bool
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
//
// PAY-SEC-S-L2/SP-C (rv-prh-i1-payout-security.md; FH-6 ledger-finance
// ruling on `4b544f5`, docs/plans/payment-readiness/rv-prh-i1-payout-ledger.md):
// V1. An earlier attempt at this same fix excluded any row under a live
// non-batch lease REGARDLESS of state, which regressed a wide swath of the
// sweeper suite - a per-item claim/resend deliberately sets next_action_at
// EARLIER than its own lease_until while still `submitting` (H3/B2's own
// crash-recovery pattern: the lease is there to detect an abandoned claim,
// not to defer the sweeper's own ordinary next look), so a bare
// lease-liveness check could not tell that apart from SP-C's actual shape.
// The real distinguishing fact ledger-finance's reproduction isolated:
// SP-C's own race requires the row to STILL be `submitting` with a live,
// non-batch lease AND a stale (already-due) next_action_at - every one of
// the regressed tests instead had phase C already move the row OUT of
// `submitting` (to `ambiguous`/`pending`/`created`) before the next claim,
// which this predicate does not touch at all. Restricting the exclusion to
// `state = 'submitting'` closes SP-C (a T12 resend's phase B is never
// mistaken for exhausted crash-recovery while it may still be running)
// without regressing the ordinary cross-tick continuation of a
// NON-submitting row.
func (s *Sweeper) claimBatch(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	leaseUntil := time.Now().Add(s.Lease)
	var ids []uuid.UUID
	err := s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		if s.TenantAdvisoryHint {
			// PRH-2 H efficiency hint only (see Sweeper.TenantAdvisoryHint): advisory
			// locks precede row locks (ADR 0082 R8), the xact lock ends with this
			// short tx, and a miss only means another instance is leasing this
			// tenant's batch right now.
			var got bool
			if err := tx.QueryRow(actx,
				`SELECT pg_try_advisory_xact_lock(hashtext('payments.sweeper.phaseA'), hashtext($1::text))`,
				tenantID.String()).Scan(&got); err != nil {
				return fmt.Errorf("sweeper advisory hint: %w", err)
			}
			if !got {
				return nil
			}
		}
		rows, err := tx.Query(actx,
			`SELECT id FROM payment_attempts
			 WHERE tenant_id = $1 AND next_action_at IS NOT NULL AND next_action_at <= now()
			   AND NOT (state = 'submitting' AND lease_until > now() AND lease_owner IS DISTINCT FROM $3)
			 ORDER BY next_action_at
			 LIMIT $2
			 FOR UPDATE SKIP LOCKED`,
			tenantID, s.batchSize(), SweeperBatchLeaseOwner)
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
				`UPDATE payment_attempts SET lease_owner = $3, lease_until = $2, next_action_at = $2, updated_at = now() WHERE id = $1`,
				id, leaseUntil, SweeperBatchLeaseOwner,
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
		// PRH-2 H (security addendum §2): a non-active tenant is resolution-only.
		// A created attempt (or a cascade child) is a NEW money-moving call, so it
		// is deferred exactly like an engaged kill switch - status read in-tx.
		if blocked, err := s.deferIfResolutionOnly(ctx, tenantID, current, "deposit_dispatch"); err != nil || blocked {
			return err
		}
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
	gr := callProvider(ctx, s.Pool, s.CredResolver, in, func(callCtx context.Context, cc CallContext) (StatusResult, ErrorClass, error) {
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
		// Ledger-finance review F3/F3b (rv-fh3-ledger.md, 076e42e):
		// attempt was read BEFORE the outbound QueryStatus call and BEFORE
		// the parent lock above - money-safe (CAS plus INV-DEP-1 catch
		// every stale decision regardless), but a concurrent callback that
		// moved the attempt in the meantime made applyStatusEvidence
		// decide from a snapshot the CAS predicates below then reject,
		// producing pure error/alert noise (and, on phase C's identical
		// pattern, a spurious error returned to a player whose deposit
		// actually succeeded). Re-read under the now-held lock, exactly as
		// ApplyReceiptEvidence already does.
		//
		// F3b correction: every attempt transition (callback, sweeper,
		// phase C) takes this SAME deposit_intents parent lock, so nothing
		// can move the attempt AFTER this re-read completes while this tx
		// still holds it - there is no "genuinely concurrent evidence
		// arriving after the fresh read" case left to hit a CAS conflict.
		// The re-read only fixes the decision's OWN staleness; it is
		// applyStatusEvidence's job (below) to actually decide from this
		// fresh state's §4.4 cell (e.g. succeeded x succeeded = no-op,
		// disputed = recorded only) instead of blindly re-running the
		// live-attempt flow and letting ITS OWN CAS transitions reject a
		// state they were never meant to apply to - the gap the original,
		// incorrect version of this comment mistook for an unavoidable
		// race.
		attempt, err = GetAttemptByID(actx, tx, attempt.ID)
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

	// PAY-SWEEP-CAS-NOISE-1 (PRH-2 D): the fresh-state short-circuit F3b added
	// for the success branch, applied to EVERY other branch. The attempt was
	// re-read under the intent lock, so if a concurrent callback already moved it
	// out of submitting/pending/ambiguous, a pending / ambiguous / decline /
	// transport-failure poll result has nothing to apply: §4.4 makes every such
	// cell a no-op (or recorded-only) for a terminal row, and attempting the CAS
	// (MarkAccepted, MarkAmbiguousFromPending, ApplyDecline, RescheduleNonTerminal -
	// the last needs next_action_at IS NOT NULL, which a terminal row no longer
	// has) only produced a benign-but-noisy CAS conflict. A poll SUCCESS keeps its
	// own terminal-state cells below (succeeded, disputed and declined/T13).
	if !attemptAwaitingEvidence(attempt.State) && (gr.Err != nil || gr.Class != ErrorClassSucceeded) {
		return nil
	}

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
		// PRH-2 D (FH7-06, ADR 0095 §36.6): an attempt polled BY its bound reference
		// keeps that reference through T9 and on the intent; the poll's echo is never
		// written over it (an empty one would violate the 0099 CHECK - an error loop -
		// and a different one would overwrite the intent's reference). Only an attempt
		// with no bound reference yet (the sweeper never polls one; direct callers of
		// this function may) learns the reference from the poll, as before.
		boundRef := res.ProviderReference
		if attempt.ProviderReference != nil && *attempt.ProviderReference != "" {
			boundRef = *attempt.ProviderReference
		}
		if err := MarkAccepted(ctx, tx, attempt.ID, EvidenceQueryStatus, boundRef, nextPoll); err != nil {
			return err
		}
		if _, err := setIntentAttempt(ctx, tx, intent.ID, attempt.ProviderID, &boundRef, DepositIntentPending); err != nil {
			return err
		}
		// RV-PRH-I1 ledger-finance H2: see drive.go's identical comment -
		// this poll is the sweeper's own T9 site.
		attempt.ProviderReference = &boundRef
		_, err := ApplyDeferredReceiptsForAttempt(ctx, tx, s.Orchestrator, attempt)
		return err

	case ErrorClassSucceeded:
		// Ledger-finance review F3b (rv-fh3-ledger.md): the attempt is
		// re-read fresh under the intent lock above, but a poll success
		// arriving for an attempt a CONCURRENT callback has already moved
		// to a terminal state must be mapped onto the §4.4 matrix's own
		// cells for that fresh state, exactly like applyResolvedReceipt
		// Evidence (receipt.go) already does for the callback path -
		// never blindly re-run the live-attempt flow below and let its
		// own CAS transitions (ApplySuccess's T7, applyMultipleSuccess
		// Dispute's T10) fail against a state they were never meant to
		// apply to. The misleading comment this replaces claimed "only
		// genuinely concurrent evidence arriving AFTER this fresh read
		// still hits the CAS-conflict path" - false: every attempt
		// transition takes this SAME intent lock, so nothing can move the
		// attempt after this locked re-read; the CAS conflicts this fix
		// removes were ALWAYS against evidence that raced BEFORE the
		// re-read, and the re-read alone was never enough to prevent them
		// (mutant F3S, the re-read removed, still survived the suite
		// before this fix for exactly that reason).
		switch attempt.State {
		case AttemptSucceeded:
			// succeeded x succeeded: a concurrent callback already
			// applied this exact success (the common case, ledger.Post's
			// own idempotency would make a re-post here a silent no-op
			// too) - genuinely no-op. A MISMATCHED echo (different
			// amount/asset than what is already on file) is the same
			// terminal contradiction applyResolvedReceiptEvidence audits
			// via auditTerminalAmountAssetMismatch, never a CAS attempt.
			//
			// PRH-2 D: decided by the same poll evidence rule as the live
			// branch (pollAmountEvidence), so an echo that merely OMITS the
			// amount (Missing) is not reported as a contradiction here.
			if pollAmountEvidence(attempt, res) == AmountEvidenceMismatch {
				// D1-F2: provider_reference is the BOUND reference; the echo is audited
				// only when valid (else reason, length and hash prefix).
				bound := ""
				if attempt.ProviderReference != nil {
					bound = *attempt.ProviderReference
				}
				var extra map[string]any
				if res.ProviderReference != "" && res.ProviderReference != bound {
					extra = echoAuditMeta(res.ProviderReference)
				}
				return auditTerminalAmountAssetMismatch(ctx, tx, attempt, ReceiptEvidence{
					ProviderReference: bound, Amount: res.Amount, AssetCode: res.AssetCode,
				}, extra)
			}
			return nil
		case AttemptDisputed:
			// disputed (any reason) is already terminal and already
			// resolved for the whole intent - recorded only, no state
			// change and no CAS attempt.
			return nil
		}
		// RV-PRH-I1 ledger-finance N2: see drive.go's identical comment -
		// a QueryStatus success naming a (provider_id, provider_reference)
		// a reversal tombstone already occupies must route to T10/T13t,
		// never straight into postDepositSuccess's own ledger insert
		// (which would otherwise surface the tombstone's unique index as
		// an untyped error, retried identically forever by the poll loop).
		// Unlike phase C, the sweeper CAN reach this with attempt.State
		// already 'declined' (a T13 second-capture re-drive), so both the
		// declined (T13t) and live (T10) tombstone cells apply here,
		// exactly as the receipt path's own matrix distinguishes them.
		//
		// PRH-2 D (PAY-POLL-AMOUNT-1 + FH7-06, ADR 0095 §36): everything from
		// here on keys on the attempt's BOUND reference, never the poll's echo.
		// The checks run in the order of ADR 0095 §36.1: amount/asset, echoed
		// reference, binding conflict, tombstone; INV-DEP-1 and the posting
		// follow below.
		if attempt.ProviderReference == nil || *attempt.ProviderReference == "" {
			// processViaQueryStatus never polls a reference-less attempt and the
			// reference is immutable once bound, so this is unreachable; refuse
			// rather than ever derive a posting key from the echo.
			return fmt.Errorf("payments: poll success for attempt %s which has no bound provider reference", attempt.ID)
		}
		boundRef := *attempt.ProviderReference
		if handled, err := s.checkPollSuccessEvidence(ctx, tx, intent, attempt, res, boundRef); err != nil || handled {
			return err
		}
		// ADR 0095 §28.3: routed through the choke-point wrapper (see
		// drive.go's identical comment) - a poll success for an intent
		// already financially resolved by ANOTHER attempt or posting
		// takes T10/T13d instead of Flow 1, exactly like phase C and the
		// receipt path. This is also T17 re-drive's own site (deposit_v2.go's
		// doc comment: T17 re-drive reuses this same evidence application).
		// postedTxID, not updated.LedgerTransactionID - PRH-I5 finding
		// (LF95-C6(a)/T13); see drive.go's identical comment.
		_, postedTxID, disputed, err := s.Orchestrator.postDepositSuccessOrDispute(ctx, tx, intent, attempt, *attempt.ProviderID, boundRef, attempt.Amount, attempt.AssetCode, EvidenceQueryStatus)
		if err != nil {
			return err
		}
		if disputed {
			return nil
		}
		if err := ApplySuccess(ctx, tx, attempt.ID, SuccessEvidence{
			Evidence: EvidenceQueryStatus, ProviderReference: boundRef, LedgerTransactionID: &postedTxID,
		}); err != nil {
			return err
		}
		// RV-PRH-I1 ledger-finance H4: see drive.go's identical comment.
		if err := rejectCreatedSiblings(ctx, tx, attempt, EvidenceQueryStatus); err != nil {
			return err
		}
		// PRH-2 D (PAY-DEFERRED-RECEIPT-SYNC-1, LF widening): a poll success on an
		// attempt T6 bound while a callback for that reference had been deferred
		// (phase B) is the transition that resolves it - drain the receipt now.
		// `attempt` already carries the bound provider_id/reference.
		_, err = ApplyDeferredReceiptsForAttempt(ctx, tx, s.Orchestrator, attempt)
		return err

	case ErrorClassDefiniteDecline:
		// PRH-2 D fix round (D1-F1): like Pending, a decline keeps the attempt's BOUND
		// reference on the intent and the attempt. The poll's echo is never written
		// over it (finalizeDeclined -> setIntentAttempt overwrites the intent's
		// reference; a foreign-bound or invalid echo would violate a unique index or
		// the 0099 CHECK and loop). A different non-empty echo is audit-only.
		var refPtr *string
		if attempt.ProviderReference != nil && *attempt.ProviderReference != "" {
			refPtr = attempt.ProviderReference
			if res.ProviderReference != "" && res.ProviderReference != *attempt.ProviderReference {
				meta := echoAuditMeta(res.ProviderReference)
				meta["provider_reference"] = *attempt.ProviderReference
				meta["deposit_intent_id"] = intent.ID.String()
				if err := audit.Record(ctx, tx, audit.Entry{
					TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.poll_decline_reference_mismatch",
					TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied, Metadata: meta,
				}); err != nil {
					return fmt.Errorf("payments: audit poll decline reference mismatch: %w", err)
				}
			}
		} else if res.ProviderReference != "" {
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
			// PRH-2 H: a cascade child is a new money-moving attempt, so a
			// non-active tenant gets none (in-tx status read); the decline above
			// is already final and stands.
			if resOnly, err := tenantResolutionOnly(ctx, tx, attempt.TenantID); err != nil {
				return err
			} else if resOnly {
				recordResolutionOnlyBlock(ctx, "deposit_cascade_child")
				return nil
			}
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

// PRH-2 H, security addendum §2 (ACCEPTED with amendments): a tenant whose
// status is not 'active' is RESOLUTION-ONLY for the sweeper.
//
//	allowed:  QueryStatus polls, evidence application (including dispute and
//	          T17 re-drive of an already-sent attempt), T3 expiry of an
//	          interactive created deposit (it makes no provider call), and every
//	          audit those paths already write.
//	never:    a NEW money-moving call: dispatching a `created` deposit attempt,
//	          creating or dispatching a cascade child, a payout T2 re-claim (new
//	          Withdraw) or a T12 resend (a Withdraw too).
//
// A non-active tenant is treated exactly like an engaged kill switch for new
// dispatch: the attempt is rescheduled on the ordinary backoff and nothing else
// changes, so reactivating a suspended tenant resumes it. The status is read per
// call, never cached per pass: inside the payout T2/T12 claim transactions, the
// poll's result transaction (cascade child), and - since B8 (PAY-H-FOLLOWUPS-1 (1)) -
// inside the deposit T2 claim transaction in drive.go (driveCreatedAttempt, after the
// intent lock and before the claim CAS) and the phase-C cascade insert in drive.go
// (applyDepositCallResult). The sweeper's separate short pre-read before
// driveCreatedAttempt (deferIfResolutionOnly) remains only as a cheap early skip; it
// is no longer the safeguard. The kill switch (INV-IO-15) and the synthetic-adapter
// tripwire still apply on top of this, and credentials come only from the per-tenant
// resolver (callProvider binds them to the attempt's own tenant).
package payments

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// tenantResolutionOnly reads tenants.status in tx and reports whether the tenant
// is anything other than 'active'. A missing row fails CLOSED (resolution-only):
// it is a broken invariant, never a licence to dispatch.
func tenantResolutionOnly(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (bool, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return status != "active", nil
}

// testHookAfterDispatchStatusPreRead is a test-only interleaving seam (B8): it
// runs in the sweeper between the pre-read above and driveCreatedAttempt, so a
// test can commit a suspension in exactly the gap the in-claim-tx read closes.
// It must never be assigned by non-test code (static guard).
var testHookAfterDispatchStatusPreRead func(tenantID uuid.UUID)

// resolutionOnlyDispatchBackoff is the default exponential poll backoff
// (SweeperDefaultPollBackoffBase doubled pollCount times, capped at
// SweeperDefaultPollBackoffCap) used when the T2 claim transaction itself defers a
// non-active tenant's created deposit; the Sweeper's own configured backoff is not
// reachable from the Orchestrator, and the rescheduled attempt is re-evaluated by
// the sweeper (which applies its own backoff) on its next pass.
func resolutionOnlyDispatchBackoff(pollCount int) time.Time {
	d := SweeperDefaultPollBackoffBase
	for i := 0; i < pollCount && d < SweeperDefaultPollBackoffCap; i++ {
		d *= 2
	}
	if d > SweeperDefaultPollBackoffCap {
		d = SweeperDefaultPollBackoffCap
	}
	return time.Now().Add(d)
}

// rescheduleCreatedForResolutionOnly defers a CREATED attempt from inside the T2 claim tx. Unlike
// RescheduleNonTerminal it is guarded by state='created': the attempt snapshot the caller holds can be
// stale (another worker may have claimed it and committed meanwhile), and a claimed `submitting` row's
// next_action_at is its claim lease, which a deferral must never rewrite (LF review B8-L1). Zero rows
// matched means the attempt moved on: nothing to defer, not an error (no claim was made here either).
func rescheduleCreatedForResolutionOnly(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, nextActionAt time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE payment_attempts SET next_action_at = $2, poll_count = poll_count + 1, updated_at = now()
		 WHERE id = $1 AND state = 'created' AND next_action_at IS NOT NULL`,
		attemptID, nextActionAt)
	return err
}

// deferIfResolutionOnly opens one short tenant tx, reads the status in it and,
// for a non-active tenant, reschedules the attempt (no state change) and returns
// blocked=true. It holds no transaction across anything else.
func (s *Sweeper) deferIfResolutionOnly(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt, site string) (blocked bool, err error) {
	next := s.backoff(attempt.PollCount)
	err = s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		resOnly, err := tenantResolutionOnly(actx, tx, tenantID)
		if err != nil || !resOnly {
			return err
		}
		if err := RescheduleNonTerminal(actx, tx, attempt.ID, next); err != nil {
			return err
		}
		blocked = true
		return nil
	})
	if blocked && err == nil {
		recordResolutionOnlyBlock(ctx, site)
	}
	return blocked, err
}

// checkPayoutResolutionOnly is the payout twin, called inside the T2/T12 claim
// tx (after the withdrawal lock, before the kill-switch and KYC gate) so the
// decision is made against a fresh read in the very transaction that would claim.
func checkPayoutResolutionOnly(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, nextActionAt time.Time, site string) (blocked bool, err error) {
	resOnly, err := tenantResolutionOnly(ctx, tx, attempt.TenantID)
	if err != nil || !resOnly {
		return false, err
	}
	if err := RescheduleNonTerminal(ctx, tx, attempt.ID, nextActionAt); err != nil {
		return false, err
	}
	recordResolutionOnlyBlock(ctx, site)
	return true, nil
}

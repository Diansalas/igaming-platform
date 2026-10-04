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
// call, never cached per pass: inside the payout T2/T12 claim transactions and the
// poll's result transaction (cascade child), but for DEPOSIT dispatch in a separate
// short transaction BEFORE driveCreatedAttempt (the T2 claim in drive.go reads no
// status), and the drive.go phase-C cascade insert is not gated: a tenant flipping in
// that gap can get one Deposit out, and a child inserted there is never driven
// (security H-SEC-1; the in-claim-tx fix is a registered follow-up). The kill switch (INV-IO-15) and
// the synthetic-adapter tripwire still apply on top of this, and credentials come
// only from the per-tenant resolver (callProvider binds them to the attempt's
// own tenant).
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

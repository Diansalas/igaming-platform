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
// H(8) (ADR 0095 section 44 decisions 19-23, section 45): a non-active BRAND is a SEPARATE
// policy check with the same effect on new dispatch (never merged with the tenant check; own
// helper, own metric sites, own audit action): no cascade child is created for it, a sweeper T2
// claim defers it (FOR SHARE brand read in the claim tx), nothing is cancelled or released.
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

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/tenant"
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
//
// H(8) decisions 20/23: the BRAND is checked here too as a SEPARATE policy check (plain
// lock-free read, cheap early skip; the authoritative FOR SHARE read is in the T2 claim tx in
// drive.go). It has its own metric site (site + "_brand") and never merges with the tenant reason.
func (s *Sweeper) deferIfResolutionOnly(ctx context.Context, tenantID, brandID uuid.UUID, attempt PaymentAttempt, site string) (blocked bool, err error) {
	next := s.backoff(attempt.PollCount)
	reason := site
	err = s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		resOnly, err := tenantResolutionOnly(actx, tx, tenantID)
		if err != nil {
			return err
		}
		if !resOnly {
			brandOnly, berr := brandResolutionOnly(actx, tx, tenantID, brandID)
			if berr != nil || !brandOnly {
				return berr
			}
			reason = site + "_brand"
		}
		if err := RescheduleNonTerminal(actx, tx, attempt.ID, next); err != nil {
			return err
		}
		blocked = true
		return nil
	})
	if blocked && err == nil {
		recordResolutionOnlyBlock(ctx, reason)
	}
	return blocked, err
}

// Metric site labels for deposit deferrals decided inside drive.go. Distinct per caller and per
// reason (ADR 0095 43.2 follow-up (c); H(8) decision 23).
const (
	siteDepositClaimHTTP          = "deposit_dispatch_claim_tx_http"
	siteDepositClaimSweeperTenant = "deposit_dispatch_claim_tx_sweeper"
	siteDepositClaimSweeperBrand  = "deposit_dispatch_claim_tx_sweeper_brand"
	siteDepositCascadeChildBrand  = "deposit_cascade_child_brand"
)

// brandResolutionOnly is the BRAND-only twin of tenantResolutionOnly (H(8)): a plain, lock-free
// read of brands.status for the brand of THIS tenant, true when it is anything but 'active'. A
// missing / foreign / nil brand fails CLOSED. It never reads the tenant (decision 23). Used only
// as the sweeper's cheap early skip; the claim tx uses tenant.RequireBrandActive (FOR SHARE).
func brandResolutionOnly(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID) (bool, error) {
	if brandID == uuid.Nil {
		return true, nil
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1 AND tenant_id = $2`, brandID, tenantID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return status != "active", nil
}

// skipCascadeChildForBrand is the H(8) decision 19 brand gate at cascade-CHILD CREATION (phase C
// and its poll-path twin), run in the decline's own tx right before insertCascadeAttemptIfEligible.
// A brand that is not active gets NO child (same precedent as the tenant skip; the decline already
// applied stands), an own audit row and an own metric site. It cancels and releases nothing. A
// read error is returned (fail closed), never read as "active".
func skipCascadeChildForBrand(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, intent DepositIntent) (skipped bool, err error) {
	berr := tenant.RequireBrandActive(ctx, tx, attempt.TenantID, intent.BrandID)
	if berr == nil {
		return false, nil
	}
	if !errors.Is(berr, tenant.ErrBrandNotActive) {
		return false, berr
	}
	recordResolutionOnlyBlock(ctx, siteDepositCascadeChildBrand)
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payment.cascade_skipped_brand_inactive",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{"deposit_intent_id": intent.ID.String(), "brand_id": intent.BrandID.String()},
	}); err != nil {
		return false, err
	}
	return true, nil
}

// skipCascadeChildForResolutionOnly is the TENANT half of the cascade-creation gate as a helper
// (B8 / PRH-2 H policy, byte-for-byte the inline treatment at phase C and the poll path): a tenant
// that is not 'active' gets no child, an own audit row and the metric site deposit_cascade_child.
// A read error is returned (fail closed). It never reads the brand (decision 23).
func skipCascadeChildForResolutionOnly(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, intentID uuid.UUID) (skipped bool, err error) {
	resOnly, err := tenantResolutionOnly(ctx, tx, attempt.TenantID)
	if err != nil || !resOnly {
		return false, err
	}
	recordResolutionOnlyBlock(ctx, "deposit_cascade_child")
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payment.cascade_skipped_resolution_only",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{"deposit_intent_id": intentID.String()},
	}); err != nil {
		return false, err
	}
	return true, nil
}

// gateReceiptCascadeChild is the receipt/callback cascade-creation gate (ADR 0095 section 44
// decision 19, section 47; closes security M-1 / LF F1). The receipt site only holds a STUB
// DepositIntent{ID, TenantID} (BrandID == uuid.Nil), so the intent's real row is loaded here, in the
// callback's own tx (RLS-scoped, then checked against the attempt's tenant): passing the stub to
// skipCascadeChildForBrand would refuse EVERY receipt-path child. Order: tenant (separate helper,
// reason, metric), then brand. Lock order: the intent row is already held FOR UPDATE by the caller's
// parent lock (parent -> attempt -> receipt -> intent -> brand FOR SHARE); the brand read is a leaf
// lock, taken before any alert raise (ADR 0102 7.7 raise-last is unaffected). Any read error is
// returned, which rolls back the whole delivery including its receipt dedupe row, so the redelivery
// converges.
func gateReceiptCascadeChild(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt) (skipped bool, err error) {
	if attempt.DepositIntentID == nil {
		return false, errors.New("payments: receipt cascade gate: attempt has no deposit intent")
	}
	intent, err := GetDepositIntentByID(ctx, tx, *attempt.DepositIntentID)
	if err != nil {
		return false, err
	}
	if intent.TenantID != attempt.TenantID {
		return false, errors.New("payments: receipt cascade gate: intent tenant differs from attempt tenant")
	}
	if skipped, err := skipCascadeChildForResolutionOnly(ctx, tx, attempt, intent.ID); err != nil || skipped {
		return skipped, err
	}
	return skipCascadeChildForBrand(ctx, tx, attempt, intent)
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

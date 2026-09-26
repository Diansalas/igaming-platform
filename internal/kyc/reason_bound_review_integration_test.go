//go:build integration

// Stage 10.3 W0 architect code-check addendum: ReviewVerification (the
// STAFF write path) writes to the same kyc_verifications.reason column
// migration 0095's CHECK (octet_length(reason) <= 512) now bounds. This
// file proves the fix at that write path: an oversized staff reason is
// normalized rather than reaching the database as a raw CHECK-constraint
// violation (an unhelpful 500).
package kyc

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestReviewVerification_OversizedReason_NormalizedNeverReachesDBAsError
// is the binding case: a staff reviewer's free-text reason far exceeding
// migration 0095's 512-byte bound, laced with control/bidi characters,
// must be normalized by ReviewVerification itself (never reach the
// database raw) - the call must succeed (never a 500 from the CHECK
// constraint), and the stored value must be bounded and clean.
func TestReviewVerification_OversizedReason_NormalizedNeverReachesDBAsError(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	staffID := seedComplianceStaff(t, pool, f)

	controlLaden := "\r\x1b[31mSTAFF NOTE\x1b[0m\u202Eevil-reversed-text"
	hugeReason := controlLaden + strings.Repeat("B", 4096) + controlLaden

	var result Verification
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusRejected, Reason: hugeReason,
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected an oversized staff reason to be normalized, not to fail the call: %v", err)
	}
	if result.Reason == hugeReason {
		t.Fatal("expected ReviewVerification to normalize (bound/clean) the staff reason, got the raw value verbatim")
	}
	if len(result.Reason) > MaxReasonBytes {
		t.Fatalf("expected the stored reason to be at most %d bytes, got %d", MaxReasonBytes, len(result.Reason))
	}
	for _, bad := range []string{"\r", "\x1b", "\u202E"} {
		if strings.Contains(result.Reason, bad) {
			t.Fatalf("expected control/bidi character %q stripped, got %q", bad, result.Reason)
		}
	}

	// Confirm what is actually PERSISTED matches what was returned - the
	// normalization must have happened before the write, not merely on
	// the return value.
	var stored string
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&stored)
	})
	if err != nil {
		t.Fatalf("read stored reason: %v", err)
	}
	if stored != result.Reason {
		t.Fatalf("expected the persisted reason to match the returned value, got %q vs %q", stored, result.Reason)
	}
}

// TestReviewVerification_CleanShortReason_StoredUnchanged is the
// no-false-positive-mutation control for the above: an ordinary, already
// short, clean staff reason must be stored exactly as given.
func TestReviewVerification_CleanShortReason_StoredUnchanged(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	staffID := seedComplianceStaff(t, pool, f)

	const reason = "documents_verified_manually"
	var result Verification
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved, Reason: reason,
		})
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != reason {
		t.Fatalf("expected the clean reason to survive unchanged, got %q", result.Reason)
	}
}

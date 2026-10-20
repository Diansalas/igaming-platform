//go:build integration

package kyc

// lic23 (security SN-6, migration 0186 and 0181): kyc_verifications cannot be rewritten into a different residence evidence.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestKYCVerificationTransitionGuard(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	exec := func(id uuid.UUID, sql string, args ...any) error {
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
			return err
		})
	}
	// seedVerification: created by the real worker path: pending with a provider reference.
	v := seedVerification(t, pool, f)
	for name, c := range map[string]struct {
		sql  string
		code string
	}{
		"provider_reference rewritten":                                  {`UPDATE kyc_verifications SET provider_reference = 'swapped' WHERE id = $1`, "KV002"},
		"provider_reference cleared (orphan the evidence read ignores)": {`UPDATE kyc_verifications SET provider_reference = NULL WHERE id = $1`, "KV002"},
		"created_at rewritten":                                          {`UPDATE kyc_verifications SET created_at = created_at - interval '1 day' WHERE id = $1`, "KV002"},
		"status moved back to unverified":                               {`UPDATE kyc_verifications SET status = 'unverified' WHERE id = $1`, "KV001"},
	} {
		if err := exec(v, c.sql); pgCode(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	// Forward moves still work, and a terminal status is final.
	if err := exec(v, `UPDATE kyc_verifications SET status = 'review_required' WHERE id = $1`); err != nil {
		t.Fatalf("pending -> review_required: %v", err)
	}
	if err := exec(v, `UPDATE kyc_verifications SET status = 'approved', expires_at = now() + interval '1 year' WHERE id = $1`); err != nil {
		t.Fatalf("review_required -> approved (setting expires_at once): %v", err)
	}
	for name, sql := range map[string]string{
		"approved -> rejected":   `UPDATE kyc_verifications SET status = 'rejected' WHERE id = $1`,
		"approved -> pending":    `UPDATE kyc_verifications SET status = 'pending' WHERE id = $1`,
		"approved -> unverified": `UPDATE kyc_verifications SET status = 'unverified' WHERE id = $1`,
	} {
		if err := exec(v, sql); pgCode(err) != "KV001" {
			t.Errorf("%s: %v, want KV001", name, err)
		}
	}
	if err := exec(v, `UPDATE kyc_verifications SET expires_at = now() - interval '1 day' WHERE id = $1`); pgCode(err) != "KV002" {
		t.Errorf("expires_at rewritten: %v, want KV002", err)
	}
	// An unrelated column still updates (the guard is narrow).
	if err := exec(v, `UPDATE kyc_verifications SET reason = 'ok' WHERE id = $1`); err != nil {
		t.Errorf("reason update: %v", err)
	}
}

// SN-6: the review path still moves a pending verification forward and the residence evidence cannot be pointed at an older
// determination by degrading the newest verification.
func TestResidenceEvidence_CannotBeRewrittenByDegradingTheNewestVerification(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	r1 := seedComplianceStaff(t, pool, f)
	old := seedVerification(t, pool, f)
	if err := rmDetermine(t, pool, f, old, r1, "MT", nil); err != nil {
		t.Fatal(err)
	}
	newest := seedVerification(t, pool, f)
	if err := rmDetermine(t, pool, f, newest, r1, "DE", nil); err != nil {
		t.Fatal(err)
	}
	if c, ok := rmEvidence(t, pool, f); !ok || c != "DE" {
		t.Fatalf("evidence = %q %v, want DE", c, ok)
	}
	// The attack: make the newest verification an "orphan" (unverified, no reference) so the older determination counts again.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_verifications SET status = 'unverified', provider_reference = NULL WHERE id = $1`, newest)
		return err
	})
	if err == nil {
		t.Fatal("degrading the newest approved verification must be refused")
	}
	if c, ok := rmEvidence(t, pool, f); !ok || c != "DE" {
		t.Fatalf("evidence after the refused attack = %q %v, want DE", c, ok)
	}
}

// SN-6: a future-dated determination time is refused (it would block every later determination).
func TestResidenceDetermination_FutureDatedTimeIsRefused(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	v := seedVerification(t, pool, f)
	staff := seedComplianceStaff(t, pool, f)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_verifications SET status = 'approved', verified_residence_country = 'MT', verified_residence_source = 'reviewer_determination',
			verified_residence_set_by = $2, verified_residence_set_at = now() + interval '30 days' WHERE id = $1`, v, staff)
		return err
	})
	if pgCode(err) != "RD005" {
		t.Fatalf("a determination dated 30 days ahead: %v, want RD005", err)
	}
}

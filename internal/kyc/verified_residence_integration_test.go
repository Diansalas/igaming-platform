//go:build integration

// Stage 4I Phase B: real-PostgreSQL tests for ReviewVerification's
// verified-residence determination extension and GetVerifiedResidence.
// Mirrors kyc_integration_test.go's own fixture conventions
// (testPool/seedFixture/seedVerification/assertRLSViolation).
package kyc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

func enableVerifiedResidenceCollection(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := jurisdiction.SetEvidenceCollectionActive(ctx, tx, jurisdiction.SetEvidenceCollectionActiveParams{
			TenantID: tenantID, EvidenceType: jurisdiction.EvidenceVerifiedResidence, Active: true,
			ActorType: jurisdiction.ActorStaff, ActorID: uuid.New(), ReasonCode: "kyc-verified-residence-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable verified residence evidence collection: %v", err)
	}
}

func seedComplianceStaff(t *testing.T, pool *db.Pool, f fixture) uuid.UUID {
	t.Helper()
	var staffID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		staff, err := identity.CreateStaffUser(ctx, tx, f.tenantID, "reviewer-"+uuid.NewString()+"@example.com", "hash", identity.StaffRoleCompliance, nil)
		staffID = staff.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed compliance staff: %v", err)
	}
	return staffID
}

func assertAuditCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action, targetID string, want int) {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
			tenantID, action, targetID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query audit_log for %q: %v", action, err)
	}
	if count != want {
		t.Fatalf("expected %d audit_log row(s) for action %q/target %q, got %d", want, action, targetID, count)
	}
}

// --- 1. A review with a residence determination alongside a terminal
// status succeeds: all four columns set, BOTH audit entries written,
// presence fields correct on a follow-up read. ---

func TestReviewVerification_WithVerifiedResidenceDetermination(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	staffID := seedComplianceStaff(t, pool, f)

	country := "US"
	var result Verification
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved, Reason: "docs_verified",
			VerifiedResidenceCountry: &country,
		})
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusApproved {
		t.Fatalf("expected status approved, got %q", result.Status)
	}
	if !result.HasVerifiedResidence {
		t.Fatal("expected HasVerifiedResidence=true on the returned Verification")
	}
	if result.VerifiedResidenceSetAt == nil {
		t.Fatal("expected VerifiedResidenceSetAt to be set")
	}
	if result.VerifiedResidenceSetBy == nil || *result.VerifiedResidenceSetBy != staffID {
		t.Fatalf("expected VerifiedResidenceSetBy=%s, got %v", staffID, result.VerifiedResidenceSetBy)
	}

	// All four raw columns, read directly.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var gotCountry, gotSource string
		var gotSetBy uuid.UUID
		var gotSetAt time.Time
		if err := tx.QueryRow(ctx,
			`SELECT verified_residence_country, verified_residence_source, verified_residence_set_by, verified_residence_set_at
			   FROM kyc_verifications WHERE id = $1`, verificationID,
		).Scan(&gotCountry, &gotSource, &gotSetBy, &gotSetAt); err != nil {
			return err
		}
		if gotCountry != "US" {
			t.Errorf("expected verified_residence_country=US, got %q", gotCountry)
		}
		if gotSource != "reviewer_determination" {
			t.Errorf("expected verified_residence_source=reviewer_determination, got %q", gotSource)
		}
		if gotSetBy != staffID {
			t.Errorf("expected verified_residence_set_by=%s, got %s", staffID, gotSetBy)
		}
		if gotSetAt.IsZero() {
			t.Error("expected a non-zero verified_residence_set_at")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("column check: %v", err)
	}

	// Both audit entries.
	assertAuditCount(t, pool, f.tenantID, "kyc.verification_status_changed", verificationID.String(), 1)
	assertAuditCount(t, pool, f.tenantID, "kyc.verified_residence_determined", verificationID.String(), 1)

	// Presence fields correct on a follow-up read.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		v, err := GetVerificationByID(ctx, tx, verificationID)
		if err != nil {
			return err
		}
		if !v.HasVerifiedResidence {
			t.Error("expected HasVerifiedResidence=true on a follow-up GetVerificationByID")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("follow-up read: %v", err)
	}
}

// --- 2. The critical regression case: a status-only review (nil
// VerifiedResidenceCountry) against a verification that ALREADY carries a
// residence determination must leave all four residence columns
// byte-identical. ---

func TestReviewVerification_StatusOnlyReviewLeavesResidenceUntouched(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	staffID := seedComplianceStaff(t, pool, f)

	country := "BR"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusReviewRequired, Reason: "needs_more_info",
			VerifiedResidenceCountry: &country,
		})
		return err
	})
	if err != nil {
		t.Fatalf("first review (with determination): %v", err)
	}

	type residenceSnapshot struct {
		country *string
		source  *string
		setBy   *uuid.UUID
		setAt   *time.Time
	}
	readSnapshot := func() residenceSnapshot {
		var s residenceSnapshot
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT verified_residence_country, verified_residence_source, verified_residence_set_by, verified_residence_set_at
				   FROM kyc_verifications WHERE id = $1`, verificationID,
			).Scan(&s.country, &s.source, &s.setBy, &s.setAt)
		})
		if err != nil {
			t.Fatalf("read residence snapshot: %v", err)
		}
		return s
	}

	before := readSnapshot()
	if before.country == nil || *before.country != "BR" {
		t.Fatalf("expected the seed determination to be BR, got %v", before.country)
	}

	// A second, STATUS-ONLY review (nil VerifiedResidenceCountry),
	// transitioning out of the non-terminal review_required status.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved, Reason: "now_approved",
		})
		return err
	})
	if err != nil {
		t.Fatalf("second, status-only review: %v", err)
	}

	after := readSnapshot()
	if before.country == nil || after.country == nil || *before.country != *after.country {
		t.Fatalf("verified_residence_country changed: before=%v after=%v", before.country, after.country)
	}
	if before.source == nil || after.source == nil || *before.source != *after.source {
		t.Fatalf("verified_residence_source changed: before=%v after=%v", before.source, after.source)
	}
	if before.setBy == nil || after.setBy == nil || *before.setBy != *after.setBy {
		t.Fatalf("verified_residence_set_by changed: before=%v after=%v", before.setBy, after.setBy)
	}
	if before.setAt == nil || after.setAt == nil || !before.setAt.Equal(*after.setAt) {
		t.Fatalf("verified_residence_set_at changed: before=%v after=%v", before.setAt, after.setAt)
	}
}

// --- 3. Activation gate OFF: the WHOLE call fails, status is NOT changed
// either - one transaction, all-or-nothing. ---

func TestReviewVerification_ActivationGateOff_WholeCallFails(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	staffID := seedComplianceStaff(t, pool, f)
	// Deliberately NOT calling enableVerifiedResidenceCollection - default
	// OFF (fail closed, migration 0074).

	var statusBefore VerificationStatus
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		v, err := GetVerificationByID(ctx, tx, verificationID)
		statusBefore = v.Status
		return err
	})
	if err != nil {
		t.Fatalf("read status before: %v", err)
	}

	country := "MX"
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved, Reason: "docs_verified",
			VerifiedResidenceCountry: &country,
		})
		return err
	})
	if !errors.Is(err, ErrEvidenceCollectionInactive) {
		t.Fatalf("expected ErrEvidenceCollectionInactive, got: %v", err)
	}

	var statusAfter VerificationStatus
	var hasResidence bool
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		v, err := GetVerificationByID(ctx, tx, verificationID)
		statusAfter = v.Status
		hasResidence = v.HasVerifiedResidence
		return err
	})
	if err != nil {
		t.Fatalf("read status after: %v", err)
	}
	if statusAfter != statusBefore {
		t.Fatalf("expected status to remain %q (unapplied), got %q", statusBefore, statusAfter)
	}
	if hasResidence {
		t.Fatal("expected no residence determination to have been applied")
	}
}

// --- 4. VerifiedResidenceCountry pointing to "" is an error, never
// treated as "clear the value". ---

func TestReviewVerification_EmptyStringVerifiedResidenceCountryIsError(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	staffID := seedComplianceStaff(t, pool, f)

	empty := ""
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved, Reason: "x",
			VerifiedResidenceCountry: &empty,
		})
		return err
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for an empty-string verified_residence_country, got: %v", err)
	}
}

// --- 5. An invalid (non-ISO) country code is an error. ---

func TestReviewVerification_InvalidVerifiedResidenceCountryCodeIsError(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	staffID := seedComplianceStaff(t, pool, f)

	bogus := "ZZ"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved, Reason: "x",
			VerifiedResidenceCountry: &bogus,
		})
		return err
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for an invalid ISO-3166-1 alpha-2 code, got: %v", err)
	}
}

// --- 6. GetVerifiedResidence: not-found, correct value, most-recent
// precedence, unscoped-connection error. ---

func TestGetVerifiedResidence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	staffID := seedComplianceStaff(t, pool, f)

	// ok=false: a verification exists but carries no determination.
	verificationNoDetermination := seedVerification(t, pool, f)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, ok, err := GetVerifiedResidence(ctx, tx, f.playerID)
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("expected ok=false with no approved verification carrying a determination")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("no-determination check: %v", err)
	}
	_ = verificationNoDetermination

	// Approve a first verification with a determination, then backdate its
	// verified_residence_set_at so a later one is unambiguously "most
	// recent" regardless of wall-clock timing.
	verificationOld := seedVerification(t, pool, f)
	countryOld := "US"
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationOld, StaffID: staffID, NewStatus: StatusApproved, Reason: "x",
			VerifiedResidenceCountry: &countryOld,
		})
		return err
	})
	if err != nil {
		t.Fatalf("approve old verification: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_verifications SET verified_residence_set_at = now() - interval '1 hour' WHERE id = $1`, verificationOld)
		return err
	})
	if err != nil {
		t.Fatalf("backdate old verification: %v", err)
	}

	// Correct value with exactly one determination on file.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		country, _, verID, ok, err := GetVerifiedResidence(ctx, tx, f.playerID)
		if err != nil {
			return err
		}
		if !ok || country != "US" || verID != verificationOld {
			t.Fatalf("expected (US, %s, true), got (%s, %s, %v)", verificationOld, country, verID, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("single-determination check: %v", err)
	}

	// A second, more recent approved verification with its OWN
	// determination must now take precedence.
	verificationNew := seedVerification(t, pool, f)
	countryNew := "CA"
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationNew, StaffID: staffID, NewStatus: StatusApproved, Reason: "x",
			VerifiedResidenceCountry: &countryNew,
		})
		return err
	})
	if err != nil {
		t.Fatalf("approve new verification: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		country, _, verID, ok, err := GetVerifiedResidence(ctx, tx, f.playerID)
		if err != nil {
			return err
		}
		if !ok || country != "CA" || verID != verificationNew {
			t.Fatalf("expected the MOST RECENT determination (CA, %s, true), got (%s, %s, %v)", verificationNew, country, verID, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("most-recent-precedence check: %v", err)
	}

	// Unscoped connection: must ERROR, never silently report ok=false.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, ok, err := GetVerifiedResidence(ctx, tx, f.playerID)
		if err == nil {
			t.Fatalf("expected an error calling GetVerifiedResidence on an unscoped connection, got ok=%v, err=nil", ok)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
}

// --- 7. Cross-tenant isolation: a DIFFERENT tenant's properly-scoped
// connection sees zero rows (ok=false, never an error) - mirrors
// identity.TestGetDeclaredResidence_CrossTenantReadReturnsNotOK's own
// two-part pattern. GetVerifiedResidence takes no tenantID parameter
// (deliberately - see its own doc comment), so this also proves RLS
// alone is sufficient scoping. ---

func TestGetVerifiedResidence_CrossTenantReadReturnsNotOK(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	enableVerifiedResidenceCollection(t, pool, fA.tenantID)
	staffA := seedComplianceStaff(t, pool, fA)
	verificationA := seedVerification(t, pool, fA)

	countryA := "FR"
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationA, StaffID: staffA, NewStatus: StatusApproved, Reason: "x",
			VerifiedResidenceCountry: &countryA,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: unexpected error: %v", err)
	}

	fB := seedFixture(t, pool)
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, ok, err := GetVerifiedResidence(ctx, tx, fA.playerID)
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("expected tenant B's scoped connection to NOT see tenant A's verified residence (ok=false)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant read: unexpected error: %v", err)
	}
}

// --- 8. Audit-record shape for kyc.verified_residence_determined: exact
// metadata key set (no "reason" key - Stage 4I Phase B review finding,
// see verification_service.go's own comment at the call site), and the
// country value never appears anywhere in the row, even when the
// reviewer's free-text Reason itself names the country. ---

func TestReviewVerification_VerifiedResidenceDeterminedAuditRecordShape(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	enableVerifiedResidenceCollection(t, pool, f.tenantID)
	staffID := seedComplianceStaff(t, pool, f)

	// Deliberately names the country in the free-text Reason - this is
	// exactly the scenario that leaked the country value before this
	// entry's metadata stopped including "reason".
	country := "AU"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{
			VerificationID: verificationID, StaffID: staffID, NewStatus: StatusApproved,
			Reason:                   "resident of AU per utility bill",
			VerifiedResidenceCountry: &country,
		})
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var rawRow, rawMetadata string
		if err := tx.QueryRow(ctx,
			`SELECT row_to_json(audit_log)::text, metadata::text FROM audit_log
			  WHERE tenant_id = $1 AND action = 'kyc.verified_residence_determined' AND target_id = $2`,
			f.tenantID, verificationID.String()).Scan(&rawRow, &rawMetadata); err != nil {
			return err
		}

		var metadata map[string]any
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			t.Fatalf("failed to unmarshal metadata: %v", err)
		}
		wantKeys := map[string]bool{
			"provenance": true, "had_previous_value": true, "value_changed": true,
			"player_account_id": true, "determined_at": true,
		}
		if len(metadata) != len(wantKeys) {
			t.Fatalf("expected exactly %d metadata keys %v, got %v", len(wantKeys), wantKeys, metadata)
		}
		for k := range metadata {
			if !wantKeys[k] {
				t.Fatalf("unexpected metadata key %q (in particular, \"reason\" must never appear here), full metadata: %v", k, metadata)
			}
		}
		if metadata["provenance"] != "reviewer_determination" {
			t.Fatalf("expected provenance=reviewer_determination, got %v", metadata["provenance"])
		}

		// The country value must never appear anywhere in this row - not
		// even via the reviewer's free-text reason, which DOES name it.
		if strings.Contains(rawRow, "AU") {
			t.Fatalf("audit row for kyc.verified_residence_determined must never contain the raw country value, got: %s", rawRow)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit check: %v", err)
	}
}

// --- 9. Raw-SQL CHECK/FK constraint enforcement - these invariants must
// be enforced by the database itself (CLAUDE.md), not just by the
// application code that happens to always write all four columns
// together. Mirrors internal/jurisdiction/jurisdiction_integration_test.
// go's own CHECK-constraint-violation test pattern. ---

func TestKYCVerifications_VerifiedResidenceCountryPairCheckConstraint(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_verifications SET verified_residence_country = 'US' WHERE id = $1`, verificationID)
		return err
	})
	if !db.IsCheckViolation(err) {
		t.Fatalf("expected a CHECK-constraint violation setting verified_residence_country alone, got: %v", err)
	}
}

func TestKYCVerifications_VerifiedResidenceSetByPairCheckConstraint(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	staffID := seedComplianceStaff(t, pool, f)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_verifications SET verified_residence_set_by = $1 WHERE id = $2`, staffID, verificationID)
		return err
	})
	if !db.IsCheckViolation(err) {
		t.Fatalf("expected a CHECK-constraint violation setting verified_residence_set_by alone, got: %v", err)
	}
}

func TestKYCVerifications_VerifiedResidenceSourcePairCheckConstraint(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_verifications SET verified_residence_source = 'reviewer_determination' WHERE id = $1`, verificationID)
		return err
	})
	if !db.IsCheckViolation(err) {
		t.Fatalf("expected a CHECK-constraint violation setting verified_residence_source alone, got: %v", err)
	}
}

func TestKYCVerifications_VerifiedResidenceSetByForeignKeyConstraint(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE kyc_verifications
			   SET verified_residence_country = 'US',
			       verified_residence_source  = 'reviewer_determination',
			       verified_residence_set_by  = $1,
			       verified_residence_set_at  = now()
			 WHERE id = $2`,
			uuid.New(), verificationID)
		return err
	})
	if !db.IsForeignKeyViolation(err) {
		t.Fatalf("expected a foreign-key violation for a verified_residence_set_by not present in staff_users, got: %v", err)
	}
}

//go:build integration

// Stage 4I Phase D: real-Postgres tests for migration 0075
// (jurisdiction_precedence_configs' widened evaluation-policy shape) and
// this package's ResolveEvaluationPolicy/CreateEvaluationPolicyVersion/
// ListEvaluationPolicyVersions. Follows jurisdiction_integration_test.go's
// fixture conventions (testPool/seedFixture/assertPgCode) exactly.
package jurisdiction

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// --- Fixture helpers ---

// rawPolicyRow is rawInsertPolicyRow's input - used to construct rows this
// package's own write API (CreateEvaluationPolicyVersion) deliberately
// cannot produce (an 'active' row), mirroring
// tenant_licence_admin_integration_test.go's insertNonActiveLicence bypass
// convention exactly.
type rawPolicyRow struct {
	licensingJurisdictionID uuid.UUID
	operationClass          OperationClass
	status                  string
	locationRequirement     string
	maxAgeSeconds           *int64
	precedencePolicyVersion string // "" defaults to PrecedencePolicyVersion
	legalReviewReference    *string
	closePriorOpen          bool
}

func rawInsertPolicyRow(t *testing.T, pool *db.Pool, platformAdmin uuid.UUID, r rawPolicyRow) (id uuid.UUID, effectiveFrom time.Time) {
	t.Helper()
	ppv := r.precedencePolicyVersion
	if ppv == "" {
		ppv = PrecedencePolicyVersion
	}
	err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		if r.closePriorOpen {
			if _, err := tx.Exec(ctx,
				`UPDATE jurisdiction_precedence_configs SET effective_to = now() WHERE licensing_jurisdiction_id = $1 AND operation_class = $2 AND effective_to IS NULL`,
				r.licensingJurisdictionID, string(r.operationClass)); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `
			INSERT INTO jurisdiction_precedence_configs (
				licensing_jurisdiction_id, operation_class, precedence, status,
				location_requirement, max_location_signal_age_seconds,
				precedence_status, resolver_policy_version, precedence_policy_version,
				legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id
			) VALUES ($1, $2, '[]'::jsonb, $3, $4, $5, 'unset', $6, $7, $8, 'raw-test-insert', 'staff', $9)
			RETURNING id, effective_from`,
			r.licensingJurisdictionID, string(r.operationClass), r.status, r.locationRequirement, r.maxAgeSeconds,
			PolicyVersion, ppv, r.legalReviewReference, platformAdmin,
		).Scan(&id, &effectiveFrom)
	})
	if err != nil {
		t.Fatalf("raw insert policy row: %v", err)
	}
	return id, effectiveFrom
}

func strPtr(s string) *string { return &s }
func i64Ptr(n int64) *int64   { return &n }

func countEvaluationPolicyAuditRows(t *testing.T, pool *db.Pool, targetID string) int {
	t.Helper()
	var count int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id IS NULL AND action = 'jurisdiction_evaluation_policy.version_created' AND target_id = $1`,
			targetID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count evaluation policy audit rows: %v", err)
	}
	return count
}

func countAllEvaluationPolicyAuditRows(t *testing.T, pool *db.Pool) int {
	t.Helper()
	var count int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'jurisdiction_evaluation_policy.version_created'`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count all evaluation policy audit rows: %v", err)
	}
	return count
}

func countOpenPolicyRows(t *testing.T, pool *db.Pool, licensingJurisdictionID uuid.UUID, oc OperationClass) int {
	t.Helper()
	var count int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM jurisdiction_precedence_configs WHERE licensing_jurisdiction_id = $1 AND operation_class = $2 AND effective_to IS NULL`,
			licensingJurisdictionID, string(oc)).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count open policy rows: %v", err)
	}
	return count
}

func countAllPolicyRows(t *testing.T, pool *db.Pool, licensingJurisdictionID uuid.UUID, oc OperationClass) int {
	t.Helper()
	var count int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM jurisdiction_precedence_configs WHERE licensing_jurisdiction_id = $1 AND operation_class = $2`,
			licensingJurisdictionID, string(oc)).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count all policy rows: %v", err)
	}
	return count
}

// --- Category 1: happy path, read ---

func TestResolveEvaluationPolicy_HappyPath_ActiveRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	id, effectiveFrom := rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: OperationCatalogueAvailability,
		status: "active", locationRequirement: "required", maxAgeSeconds: i64Ptr(300),
		legalReviewReference: strPtr("legal-ref-1"),
	})

	var policy EvaluationPolicy
	var prov EvaluationPolicyProvenance
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		policy, prov, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{
			TenantID: f.tenantID, OperationClass: OperationCatalogueAvailability, AsOf: time.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("ResolveEvaluationPolicy: %v", err)
	}
	if policy.LocationSignalRequirement != LocationRequired {
		t.Errorf("expected LocationRequired, got %q", policy.LocationSignalRequirement)
	}
	if policy.MaxLocationSignalAge == nil || *policy.MaxLocationSignalAge != 300*time.Second {
		t.Errorf("expected max age 300s, got %v", policy.MaxLocationSignalAge)
	}
	if prov.ConfigID != id {
		t.Errorf("expected provenance config id %s, got %s", id, prov.ConfigID)
	}
	if !prov.EffectiveFrom.Equal(effectiveFrom) {
		t.Errorf("expected provenance effective_from %v, got %v", effectiveFrom, prov.EffectiveFrom)
	}
	if prov.PrecedencePolicyVersion != PrecedencePolicyVersion {
		t.Errorf("expected precedence policy version %q, got %q", PrecedencePolicyVersion, prov.PrecedencePolicyVersion)
	}
}

// --- Category 2: happy path, write ---

func TestCreateEvaluationPolicyVersion_HappyPath_DraftInsert(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var rec EvaluationPolicyRecord
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: OperationPlay, Status: EvaluationPolicyDraft,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "author-draft"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("CreateEvaluationPolicyVersion: %v", err)
	}
	if rec.Status != EvaluationPolicyDraft {
		t.Errorf("expected status draft, got %q", rec.Status)
	}
	if rec.PrecedenceConfigured {
		t.Error("expected precedence_status unset -> PrecedenceConfigured false")
	}
	if rec.ResolverPolicyVersion != PolicyVersion {
		t.Errorf("expected resolver_policy_version %q, got %q", PolicyVersion, rec.ResolverPolicyVersion)
	}
	if rec.PrecedencePolicyVersion != PrecedencePolicyVersion {
		t.Errorf("expected precedence_policy_version %q, got %q", PrecedencePolicyVersion, rec.PrecedencePolicyVersion)
	}
	if rec.EffectiveFrom.IsZero() {
		t.Error("expected a DB-set, non-zero effective_from")
	}
	if rec.EffectiveTo != nil {
		t.Error("expected a freshly-inserted row to have a nil effective_to")
	}
	if time.Since(rec.EffectiveFrom) > time.Minute {
		t.Errorf("expected effective_from to be recent, got %v", rec.EffectiveFrom)
	}
}

// --- Category 3: unset / fail-closed, key level ---

func TestResolveEvaluationPolicy_NoRowAtAll_ErrPolicyNotConfigured(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var policy EvaluationPolicy
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		policy, _, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{
			TenantID: f.tenantID, OperationClass: OperationBonusIssuance, AsOf: time.Now(),
		})
		return err
	})
	if !errors.Is(err, ErrPolicyNotConfigured) {
		t.Fatalf("expected ErrPolicyNotConfigured, got %v", err)
	}
	if policy != (EvaluationPolicy{}) {
		t.Errorf("expected the zero EvaluationPolicy, got %+v", policy)
	}
}

// --- Category 4: unset / fail-closed, field level ---

func TestResolveEvaluationPolicy_FieldLevelUnset_FeedsPolicyUnsetToPrecedenceEngine(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// location_requirement = 'unset' explicitly on an ACTIVE row.
	rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: OperationPlay,
		status: "active", locationRequirement: "unset", maxAgeSeconds: nil,
		legalReviewReference: strPtr("legal-ref-2"),
	})

	var policy EvaluationPolicy
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		policy, _, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{
			TenantID: f.tenantID, OperationClass: OperationPlay, AsOf: time.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("ResolveEvaluationPolicy: %v", err)
	}
	if policy.LocationSignalRequirement != LocationRequirementUnset {
		t.Fatalf("expected LocationRequirementUnset, got %q", policy.LocationSignalRequirement)
	}

	_, err = DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeMarketAccessControl,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: time.Now().Add(-time.Hour), VerificationID: uuid.New()},
		},
		Policy: policy,
		AsOf:   time.Now(),
	})
	if !errors.Is(err, ErrPolicyUnset) {
		t.Fatalf("expected ErrPolicyUnset from a config-sourced unset location_requirement, got %v", err)
	}

	// Same for a NULL max age with location_requirement='required'.
	rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: OperationBonusIssuance,
		status: "active", locationRequirement: "required", maxAgeSeconds: nil,
		legalReviewReference: strPtr("legal-ref-3"),
	})
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		policy, _, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{
			TenantID: f.tenantID, OperationClass: OperationBonusIssuance, AsOf: time.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("ResolveEvaluationPolicy (nil age): %v", err)
	}
	asOf := time.Now()
	_, err = DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeMarketAccessControl,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOf.Add(-time.Hour), VerificationID: uuid.New()},
			LocationSignal:    &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOf.Add(-time.Minute), ProviderID: "p", ProviderReference: "r"},
		},
		Policy: policy,
		AsOf:   asOf,
	})
	if !errors.Is(err, ErrPolicyUnset) {
		t.Fatalf("expected ErrPolicyUnset from a config-sourced nil max age with an observed signal, got %v", err)
	}
}

// --- Category 5: inactive vs. not-configured distinction ---

func TestResolveEvaluationPolicy_DraftWithdrawnAbsent_Distinguishable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: OperationPlay,
		status: "draft", locationRequirement: "unset",
	})
	rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdiction2, operationClass: OperationPlay,
		status: "withdrawn", locationRequirement: "unset",
	})

	resolve := func(jurisdictionTenant uuid.UUID, oc OperationClass) error {
		return pool.WithTenant(context.Background(), jurisdictionTenant, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{
				TenantID: jurisdictionTenant, OperationClass: oc, AsOf: time.Now(),
			})
			return err
		})
	}

	if err := resolve(f.tenantID, OperationPlay); !errors.Is(err, ErrPolicyNotActive) {
		t.Errorf("draft in force: expected ErrPolicyNotActive, got %v", err)
	}
	// f.otherTenantID has no licence at all - use it only for the
	// "absent" case via a tenant that DOES have a licence but no row for
	// a distinct operation class instead, so the licensing-jurisdiction
	// derivation still succeeds.
	if err := resolve(f.tenantID, OperationBonusConversion); !errors.Is(err, ErrPolicyNotConfigured) {
		t.Errorf("absent: expected ErrPolicyNotConfigured, got %v", err)
	}
	if errors.Is(nil, ErrPolicyNotActive) {
		t.Fatal("sanity: nil must not satisfy errors.Is")
	}
}

// --- Category 7: invalid input rejection, write ---

func TestCreateEvaluationPolicyVersion_InvalidInputRejections(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	run := func(p CreateEvaluationPolicyVersionParams) error {
		return pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateEvaluationPolicyVersion(ctx, tx, p)
			return err
		})
	}
	base := CreateEvaluationPolicyVersionParams{
		LicensingJurisdictionID: f.jurisdictionID, OperationClass: OperationPlay, Status: EvaluationPolicyDraft,
		Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"},
	}

	t.Run("nil_jurisdiction_id", func(t *testing.T) {
		p := base
		p.LicensingJurisdictionID = uuid.Nil
		if err := run(p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	})
	t.Run("invalid_operation_class", func(t *testing.T) {
		p := base
		p.OperationClass = "not_a_class"
		if err := run(p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	})
	t.Run("active_status_refused", func(t *testing.T) {
		p := base
		p.OperationClass = OperationBonusConversion
		p.Status = EvaluationPolicyActive
		p.LegalReviewReference = "ref"
		if err := run(p); !errors.Is(err, ErrActivationNotAuthorized) {
			t.Fatalf("expected ErrActivationNotAuthorized, got %v", err)
		}
	})
	t.Run("unrecognized_location_requirement", func(t *testing.T) {
		p := base
		p.OperationClass = OperationCatalogueAvailability
		p.LocationSignalRequirement = LocationRequirement("bogus")
		if err := run(p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	})
	t.Run("non_positive_max_age", func(t *testing.T) {
		p := base
		p.OperationClass = OperationBonusIssuance
		zero := time.Duration(0)
		p.MaxLocationSignalAge = &zero
		if err := run(p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	})
	t.Run("sub_second_max_age", func(t *testing.T) {
		p := base
		p.OperationClass = OperationBonusIssuance
		d := 500 * time.Millisecond
		p.MaxLocationSignalAge = &d
		if err := run(p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	})
	t.Run("withdrawal_carrying_content", func(t *testing.T) {
		p := base
		p.Status = EvaluationPolicyWithdrawn
		p.LocationSignalRequirement = LocationAdvisory
		if err := run(p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	})
	t.Run("withdrawal_with_nothing_open", func(t *testing.T) {
		p := base
		p.Status = EvaluationPolicyWithdrawn
		if err := run(p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	})
	t.Run("bogus_status", func(t *testing.T) {
		p := base
		p.OperationClass = OperationBonusConversion
		p.Status = EvaluationPolicyStatus("suspended")
		if err := run(p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput, got %v", err)
		}
	})
}

// --- Category 8: effective-dated selection & non-overlap ---

func TestResolveEvaluationPolicy_EffectiveDatedSelectionAndNonOverlap(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationPlay

	beforeAny := time.Now()
	_, v1From := rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: oc,
		status: "active", locationRequirement: "advisory", legalReviewReference: strPtr("ref-v1"),
	})
	time.Sleep(10 * time.Millisecond)
	_, v2From := rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: oc,
		status: "active", locationRequirement: "required", maxAgeSeconds: i64Ptr(60),
		legalReviewReference: strPtr("ref-v2"), closePriorOpen: true,
	})
	time.Sleep(10 * time.Millisecond)
	// Withdraw (close v2, open a withdrawn tombstone).
	_, v3From := rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: oc,
		status: "withdrawn", locationRequirement: "unset", closePriorOpen: true,
	})

	resolveAt := func(asOf time.Time) (EvaluationPolicy, error) {
		var policy EvaluationPolicy
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			policy, _, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{TenantID: f.tenantID, OperationClass: oc, AsOf: asOf})
			return err
		})
		return policy, err
	}

	if _, err := resolveAt(beforeAny); !errors.Is(err, ErrPolicyNotConfigured) {
		t.Errorf("before any version: expected ErrPolicyNotConfigured, got %v", err)
	}
	p1, err := resolveAt(v1From)
	if err != nil {
		t.Fatalf("at v1.effective_from: %v", err)
	}
	if p1.LocationSignalRequirement != LocationAdvisory {
		t.Errorf("expected v1's policy (advisory), got %q", p1.LocationSignalRequirement)
	}
	p2, err := resolveAt(v2From)
	if err != nil {
		t.Fatalf("at v2.effective_from: %v", err)
	}
	if p2.LocationSignalRequirement != LocationRequired {
		t.Errorf("expected v2's policy (required), got %q", p2.LocationSignalRequirement)
	}
	if _, err := resolveAt(v3From); !errors.Is(err, ErrPolicyNotActive) {
		t.Errorf("at v3 (withdrawn): expected ErrPolicyNotActive, got %v", err)
	}

	// Non-overlap, asserted directly against the table.
	var overlaps int
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM jurisdiction_precedence_configs a
			 JOIN jurisdiction_precedence_configs b
			   ON a.licensing_jurisdiction_id = b.licensing_jurisdiction_id
			  AND a.operation_class = b.operation_class
			  AND a.id <> b.id
			 WHERE a.licensing_jurisdiction_id = $1 AND a.operation_class = $2
			   AND a.effective_from < COALESCE(b.effective_to, 'infinity'::timestamptz)
			   AND COALESCE(a.effective_to, 'infinity'::timestamptz) > b.effective_from`,
			f.jurisdictionID, string(oc)).Scan(&overlaps)
	})
	if err != nil {
		t.Fatalf("overlap check: %v", err)
	}
	if overlaps != 0 {
		t.Fatalf("expected zero overlapping [from,to) windows for this key, found %d overlapping pairs", overlaps)
	}
}

// --- Category 9: concurrency ---

// TestCreateEvaluationPolicyVersion_ConcurrentCreatesNeverCorruptState
// exercises two admins concurrently authoring the FIRST version for a key
// as a coarse smoke test of real concurrent access. It is intentionally
// NOT the proof that ErrConcurrentPolicyWrite is reachable - that is
// TestCreateEvaluationPolicyVersion_DeterministicConflictViaUncommittedCompetingRow
// below, which uses genuine Postgres locking semantics instead of
// goroutine-scheduling luck.
//
// Why this test cannot assert one specific outcome: SELECT ... FOR UPDATE
// on an empty key takes no row lock, so even with the barrier below
// forcing both transactions to have already BEGUN before either calls
// CreateEvaluationPolicyVersion, Postgres scheduling AFTER the barrier
// releases can still legitimately produce either of two DIFFERENT correct
// outcomes:
//   - a true race: both SELECT...FOR UPDATE run before either INSERT
//     commits -> exactly 1 success, 1 ErrConcurrentPolicyWrite.
//   - effective serialization/supersession: goroutine A's SELECT, INSERT,
//     and COMMIT all complete before goroutine B's SELECT even runs -> B's
//     SELECT sees A's committed row and cleanly supersedes it -> 2
//     successes, 2 rows (one open, one closed).
//
// A prior fix round asserted only the first outcome (exactly 1 success, 1
// conflict); a security re-verification pass reproduced 9/200 failures
// under CPU load and 1/60 without, because narrowing the goroutine barrier
// can shrink the scheduling window that produces the second outcome but
// can never eliminate it. This test instead asserts the invariants that
// hold under BOTH outcomes: state is never corrupted, no writer is ever
// left in an ambiguous or invalid state, and any failure is always
// ErrConcurrentPolicyWrite and never anything else.
func TestCreateEvaluationPolicyVersion_ConcurrentCreatesNeverCorruptState(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationBonusConversion

	var wg sync.WaitGroup
	wg.Add(2)
	var barrier sync.WaitGroup
	barrier.Add(2)
	errs := make([]error, 2)
	recs := make([]EvaluationPolicyRecord, 2)
	run := func(idx int) {
		defer wg.Done()
		errs[idx] = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			// Both transactions are already BEGUN and GUC-scoped by the time
			// fn is invoked (db.Pool.WithPlatformAdmin). Signal readiness and
			// wait for the other side before either calls
			// CreateEvaluationPolicyVersion, so both writers provably start
			// their own attempt at (approximately) the same time - see the
			// doc comment above for why this narrows, but cannot close, the
			// scheduling window between the two legitimate outcomes.
			barrier.Done()
			barrier.Wait()
			rec, err := CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
				LicensingJurisdictionID: f.jurisdictionID, OperationClass: oc, Status: EvaluationPolicyDraft,
				Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "concurrent-test"},
			})
			recs[idx] = rec
			return err
		})
	}
	go run(0)
	go run(1)
	wg.Wait()

	successes, conflicts := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			successes++
			_ = recs[i]
		case errors.Is(err, ErrConcurrentPolicyWrite):
			conflicts++
		default:
			t.Fatalf("unexpected error (must be nil or ErrConcurrentPolicyWrite, never anything else): %v", err)
		}
	}

	// Invariant 1: never 0 successes (both spuriously failing is never a
	// legitimate outcome), and never more than 2 (only 2 calls were made).
	if successes != 1 && successes != 2 {
		t.Fatalf("expected 1 or 2 successes (a true race or a clean supersession), got %d successes / %d conflicts (errs=%v)", successes, conflicts, errs)
	}
	if successes+conflicts != 2 {
		t.Fatalf("expected every call to either succeed or return ErrConcurrentPolicyWrite, got %d successes + %d conflicts", successes, conflicts)
	}

	// Invariant 2: exactly one row is EVER open for the key at the end,
	// regardless of which legitimate outcome occurred.
	if got := countOpenPolicyRows(t, pool, f.jurisdictionID, oc); got != 1 {
		t.Fatalf("expected exactly 1 open row after the concurrent creates, got %d", got)
	}

	// Invariant 3: no two rows for this key have overlapping
	// [effective_from, effective_to) windows - adapted from category 8's
	// non-overlap query.
	var overlaps int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM jurisdiction_precedence_configs a
			 JOIN jurisdiction_precedence_configs b
			   ON a.licensing_jurisdiction_id = b.licensing_jurisdiction_id
			  AND a.operation_class = b.operation_class
			  AND a.id <> b.id
			 WHERE a.licensing_jurisdiction_id = $1 AND a.operation_class = $2
			   AND a.effective_from < COALESCE(b.effective_to, 'infinity'::timestamptz)
			   AND COALESCE(a.effective_to, 'infinity'::timestamptz) > b.effective_from`,
			f.jurisdictionID, string(oc)).Scan(&overlaps)
	})
	if err != nil {
		t.Fatalf("overlap check: %v", err)
	}
	if overlaps != 0 {
		t.Fatalf("expected zero overlapping [from,to) windows for this key, found %d overlapping pairs", overlaps)
	}

	// Invariant 4: exactly `successes` rows exist for the key, and exactly
	// `successes` audit_log rows target them - no orphaned or missing
	// audit entries either way.
	if got := countAllPolicyRows(t, pool, f.jurisdictionID, oc); got != successes {
		t.Fatalf("expected exactly %d row(s) total for this key (one per successful call), got %d", successes, got)
	}
	var auditRowsForKey int
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM audit_log a
			 JOIN jurisdiction_precedence_configs c ON c.id::text = a.target_id
			 WHERE a.action = 'jurisdiction_evaluation_policy.version_created'
			   AND c.licensing_jurisdiction_id = $1 AND c.operation_class = $2`,
			f.jurisdictionID, string(oc)).Scan(&auditRowsForKey)
	})
	if err != nil {
		t.Fatalf("count audit rows for key: %v", err)
	}
	if auditRowsForKey != successes {
		t.Fatalf("expected exactly %d audit_log row(s) for this key's rows, got %d", successes, auditRowsForKey)
	}

	// Invariant 5: if exactly one call failed, its error must be
	// ErrConcurrentPolicyWrite - already guaranteed by the switch's default
	// branch above (which would have failed the test on any other error),
	// restated here as an explicit, named assertion of the required
	// invariant rather than relying solely on the loop's side effect.
	if conflicts == 1 && successes != 1 {
		t.Fatalf("a single conflict must always pair with a single success, got %d successes and %d conflicts", successes, conflicts)
	}
}

// TestCreateEvaluationPolicyVersion_DeterministicConflictViaUncommittedCompetingRow
// proves ErrConcurrentPolicyWrite is reachable and correctly mapped from a
// genuine 23505 unique violation, using ONLY deterministic PostgreSQL
// locking semantics - never goroutine-scheduling luck. This is standard,
// well-documented PostgreSQL behaviour, not a race: a second INSERT that
// would violate a unique index (here, uq_jurisdiction_precedence_configs_
// open) against an UNCOMMITTED row from another transaction BLOCKS
// (XactLockTableWait) until the first transaction resolves, then either
// fails with a real unique-violation if the first committed, or succeeds
// if the first rolled back.
//
// Sequence:
//  1. A "blocker" goroutine opens a platform-admin-scoped transaction,
//     inserts a competing OPEN row for a fresh key directly (bypassing
//     CreateEvaluationPolicyVersion, mirroring rawInsertPolicyRow's
//     bypass convention), signals blockerReady, and then blocks on
//     proceedToCommit before returning (which commits when
//     WithPlatformAdmin's fn returns nil).
//  2. The main goroutine, once blockerReady fires, starts the REAL
//     CreateEvaluationPolicyVersion call for the SAME key in its own
//     goroutine (it will block on its own INSERT statement).
//  3. The test polls pg_stat_activity - Postgres's own record of which
//     backend is waiting on which kind of wait event - for a backend
//     running this exact INSERT with wait_event_type = 'Lock'. This is
//     what makes "has the real call reached and blocked on its INSERT"
//     fully deterministic: no fixed sleep is used or needed for this half
//     of the synchronization.
//  4. Only once that poll confirms the real call is genuinely blocked does
//     the test signal proceedToCommit, letting the blocker's transaction
//     commit and unblocking the real call's INSERT, which must then fail
//     with a genuine 23505 that CreateEvaluationPolicyVersion maps to
//     ErrConcurrentPolicyWrite.
func TestCreateEvaluationPolicyVersion_DeterministicConflictViaUncommittedCompetingRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationBonusIssuance

	blockerReady := make(chan struct{})
	proceedToCommit := make(chan struct{})
	blockerErrCh := make(chan error, 1)
	blockerActorID := uuid.New()

	go func() {
		blockerErrCh <- pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO jurisdiction_precedence_configs (
					licensing_jurisdiction_id, operation_class, precedence, status,
					location_requirement, max_location_signal_age_seconds,
					precedence_status, resolver_policy_version, precedence_policy_version,
					legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id
				) VALUES ($1, $2, '[]'::jsonb, 'draft', 'unset', NULL, 'unset', $3, $4, NULL, $5, 'staff', $6)`,
				f.jurisdictionID, string(oc), PolicyVersion, PrecedencePolicyVersion,
				"deterministic-conflict-test-blocker", blockerActorID)
			if err != nil {
				return err
			}
			close(blockerReady)
			<-proceedToCommit
			return nil
		})
	}()

	<-blockerReady

	realCallResult := make(chan error, 1)
	go func() {
		realCallResult <- pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
				LicensingJurisdictionID: f.jurisdictionID, OperationClass: oc, Status: EvaluationPolicyDraft,
				Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "deterministic-conflict-test-real-call"},
			})
			return err
		})
	}()

	// Poll pg_stat_activity - fully deterministic, no timing assumption -
	// for the real call's own INSERT statement genuinely blocked on a lock
	// (i.e. on the blocker's uncommitted row via the partial unique
	// index). SELECT...FOR UPDATE never waits on an uncommitted INSERT of
	// a brand-new row (only on uncommitted UPDATE/DELETE of an
	// already-visible row, or on the INSERT's own unique-index check), so
	// the real call's SELECT completes immediately and it is the INSERT,
	// specifically, that blocks here.
	deadline := time.Now().Add(3 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var count int
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT count(*) FROM pg_stat_activity
				 WHERE wait_event_type = 'Lock'
				   AND query ILIKE '%INSERT INTO jurisdiction_precedence_configs%'`).Scan(&count)
		})
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if count > 0 {
			blocked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blocked {
		close(proceedToCommit)
		t.Fatal("timed out waiting for the real call's INSERT to block on the uncommitted blocker row (pg_stat_activity never reported a Lock wait on that statement)")
	}

	close(proceedToCommit)

	if err := <-blockerErrCh; err != nil {
		t.Fatalf("blocker transaction: expected nil error (its own insert should succeed once committed), got %v", err)
	}
	if err := <-realCallResult; !errors.Is(err, ErrConcurrentPolicyWrite) {
		t.Fatalf("expected ErrConcurrentPolicyWrite from the real call once the blocker committed (a genuine 23505 unique violation), got %v", err)
	}

	if got := countOpenPolicyRows(t, pool, f.jurisdictionID, oc); got != 1 {
		t.Fatalf("expected exactly 1 open row (the blocker's) after the real call's failed conflicting insert, got %d", got)
	}
	if got := countAllPolicyRows(t, pool, f.jurisdictionID, oc); got != 1 {
		t.Fatalf("expected exactly 1 row total for this key (the failed real call left nothing behind), got %d", got)
	}
}

// --- Category 10: supersession ---

func TestCreateEvaluationPolicyVersion_SupersessionClosesPredecessorByteIdentical(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationCatalogueAvailability

	var first EvaluationPolicyRecord
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: oc, Status: EvaluationPolicyDraft,
			LocationSignalRequirement: LocationAdvisory,
			Actor:                     ActorContext{ActorID: f.platformAdmin, ReasonCode: "v1"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}

	var second EvaluationPolicyRecord
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: oc, Status: EvaluationPolicyDraft,
			LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: durPtr(120 * time.Second),
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "v2"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("create v2 (supersede v1): %v", err)
	}

	closedFirst, err := readEvaluationPolicyRecordByIDForTest(t, pool, first.ID)
	if err != nil {
		t.Fatalf("read closed v1: %v", err)
	}
	if closedFirst.EffectiveTo == nil {
		t.Fatal("expected v1 to be closed (non-nil effective_to) after v2 was created")
	}
	if !closedFirst.EffectiveTo.Equal(second.EffectiveFrom) {
		t.Fatalf("expected v1.effective_to == v2.effective_from, got %v vs %v", closedFirst.EffectiveTo, second.EffectiveFrom)
	}
	// Byte-identical otherwise: every other field is unchanged from the
	// original insert.
	if closedFirst.LocationSignalRequirement != LocationAdvisory {
		t.Errorf("expected v1's own content unchanged (advisory), got %q", closedFirst.LocationSignalRequirement)
	}
	if closedFirst.ReasonCode != "v1" {
		t.Errorf("expected v1's own reason_code unchanged, got %q", closedFirst.ReasonCode)
	}
	if closedFirst.CreatedByActorID != f.platformAdmin {
		t.Errorf("expected v1's created_by_actor_id unchanged")
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }

// readEvaluationPolicyRecordByIDForTest is a test-only wrapper around the
// unexported readEvaluationPolicyRecordByID, run in its own transaction.
func readEvaluationPolicyRecordByIDForTest(t *testing.T, pool *db.Pool, id uuid.UUID) (EvaluationPolicyRecord, error) {
	t.Helper()
	var rec EvaluationPolicyRecord
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = readEvaluationPolicyRecordByID(ctx, tx, id)
		return err
	})
	return rec, err
}

// --- Category 11: withdrawal / rollback ---

func TestCreateEvaluationPolicyVersion_WithdrawalMakesTheKeyNotActive(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationPlay

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: oc, Status: EvaluationPolicyDraft,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "author"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: oc, Status: EvaluationPolicyWithdrawn,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "withdraw"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{TenantID: f.tenantID, OperationClass: oc, AsOf: time.Now()})
		return err
	})
	if !errors.Is(err, ErrPolicyNotActive) {
		t.Fatalf("expected ErrPolicyNotActive after withdrawal, got %v", err)
	}
}

// --- Category 12: immutability ---

func TestJurisdictionPrecedenceConfigs_Immutability(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	id, _ := rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: OperationPlay,
		status: "draft", locationRequirement: "unset",
	})

	run := func(sql string, args ...any) error {
		return pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}

	t.Run("update_content_column_rejected", func(t *testing.T) {
		err := run(`UPDATE jurisdiction_precedence_configs SET reason_code = 'tampered' WHERE id = $1`, id)
		if err == nil {
			t.Fatal("expected the update to be rejected")
		}
		assertPgCode(t, err, pgRaisedError)
	})

	t.Run("delete_rejected", func(t *testing.T) {
		// RLS has no DELETE policy at all, so this is denied by RLS (0 rows
		// affected, no error) rather than reaching the trigger - see the RLS
		// write test below for the explicit assertion of that. A DELETE
		// under FORCE RLS with no DELETE policy returns NO error, so
		// asserting only "if err != nil" would pass identically even if the
		// row had been deleted - assert RowsAffected()==0 explicitly, and
		// that the row still exists afterward, so this subtest genuinely
		// proves survival rather than merely not erroring.
		var rowsAffected int64
		err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM jurisdiction_precedence_configs WHERE id = $1`, id)
			if err != nil {
				return err
			}
			rowsAffected = tag.RowsAffected()
			return nil
		})
		if err != nil {
			assertPgCode(t, err, pgRaisedError)
		} else if rowsAffected != 0 {
			t.Fatalf("expected the DELETE to affect 0 rows (no DELETE policy exists), affected %d", rowsAffected)
		}

		var stillExists bool
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jurisdiction_precedence_configs WHERE id = $1)`, id).Scan(&stillExists)
		})
		if err != nil {
			t.Fatalf("verify row survives: %v", err)
		}
		if !stillExists {
			t.Fatal("expected the row to survive the denied DELETE attempt, but it is gone")
		}
	})

	t.Run("truncate_rejected", func(t *testing.T) {
		err := run(`TRUNCATE jurisdiction_precedence_configs`)
		if err == nil {
			t.Fatal("expected TRUNCATE to be rejected")
		}
		assertPgCode(t, err, pgRaisedError)
	})

	t.Run("clear_effective_to_rejected", func(t *testing.T) {
		// First legitimately close it.
		if err := run(`UPDATE jurisdiction_precedence_configs SET effective_to = now() WHERE id = $1`, id); err != nil {
			t.Fatalf("legitimate close: %v", err)
		}
		// Reopening (setting it back to NULL) must be rejected.
		err := run(`UPDATE jurisdiction_precedence_configs SET effective_to = NULL WHERE id = $1`, id)
		if err == nil {
			t.Fatal("expected clearing effective_to to be rejected")
		}
		assertPgCode(t, err, pgRaisedError)
	})

	t.Run("reclose_already_closed_rejected", func(t *testing.T) {
		err := run(`UPDATE jurisdiction_precedence_configs SET effective_to = now() WHERE id = $1`, id)
		if err == nil {
			t.Fatal("expected re-closing an already-closed row to be rejected")
		}
		assertPgCode(t, err, pgRaisedError)
	})
}

// --- Category 13: forged effective_from / provenance ---

func TestJurisdictionPrecedenceConfigs_ForgedEffectiveFromOverwritten(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	forged := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	var actualEffectiveFrom, actualCreatedAt time.Time
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO jurisdiction_precedence_configs (
				licensing_jurisdiction_id, operation_class, precedence, status,
				location_requirement, resolver_policy_version, precedence_policy_version,
				reason_code, created_by_actor_type, created_by_actor_id,
				effective_from, created_at
			) VALUES ($1, $2, '[]'::jsonb, 'draft', 'unset', $3, $4, 'forge-attempt', 'staff', $5, $6, $6)
			RETURNING effective_from, created_at`,
			f.jurisdictionID, string(OperationPlay), PolicyVersion, PrecedencePolicyVersion, f.platformAdmin, forged,
		).Scan(&actualEffectiveFrom, &actualCreatedAt)
	})
	if err != nil {
		t.Fatalf("forge attempt insert: %v", err)
	}
	if actualEffectiveFrom.Equal(forged) {
		t.Fatal("expected the trigger to overwrite a caller-supplied effective_from, but the forged value was stored")
	}
	if actualCreatedAt.Equal(forged) {
		t.Fatal("expected the trigger to overwrite a caller-supplied created_at, but the forged value was stored")
	}
	if time.Since(actualEffectiveFrom) > time.Minute {
		t.Errorf("expected effective_from to be genuinely recent, got %v", actualEffectiveFrom)
	}
}

// --- Category 14: RLS - read ---

func TestJurisdictionPrecedenceConfigs_RLSReadAllScopes(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: OperationPlay,
		status: "draft", locationRequirement: "unset",
	})

	countVisible := func(run func(func(context.Context, pgx.Tx) error) error) int {
		var count int
		err := run(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_precedence_configs WHERE licensing_jurisdiction_id = $1`, f.jurisdictionID).Scan(&count)
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return count
	}

	if got := countVisible(func(fn func(context.Context, pgx.Tx) error) error {
		return pool.WithTenant(context.Background(), f.tenantID, fn)
	}); got != 1 {
		t.Errorf("tenant-scoped read: expected 1 visible row, got %d", got)
	}
	if got := countVisible(func(fn func(context.Context, pgx.Tx) error) error {
		return pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, fn)
	}); got != 1 {
		t.Errorf("player-scoped read: expected 1 visible row, got %d", got)
	}
	if got := countVisible(func(fn func(context.Context, pgx.Tx) error) error {
		return pool.WithPlatformAdmin(context.Background(), f.platformAdmin, fn)
	}); got != 1 {
		t.Errorf("platform-scoped read: expected 1 visible row, got %d", got)
	}
	if got := countVisible(func(fn func(context.Context, pgx.Tx) error) error {
		return pool.WithoutTenant(context.Background(), fn)
	}); got != 1 {
		t.Errorf("bare-pool (WithoutTenant) read: expected 1 visible row, got %d", got)
	}
}

// --- Category 15: RLS - write ---

func TestJurisdictionPrecedenceConfigs_RLSWriteScopes(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	insertAttempt := func(run func(func(context.Context, pgx.Tx) error) error) error {
		return run(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO jurisdiction_precedence_configs (
					licensing_jurisdiction_id, operation_class, precedence, status,
					location_requirement, resolver_policy_version, precedence_policy_version,
					reason_code, created_by_actor_type, created_by_actor_id
				) VALUES ($1, $2, '[]'::jsonb, 'draft', 'unset', $3, $4, 'rls-test', 'staff', $5)`,
				f.jurisdictionID, string(OperationBonusIssuance), PolicyVersion, PrecedencePolicyVersion, f.platformAdmin)
			return err
		})
	}

	t.Run("tenant_scoped_insert_denied", func(t *testing.T) {
		err := insertAttempt(func(fn func(context.Context, pgx.Tx) error) error {
			return pool.WithTenant(context.Background(), f.tenantID, fn)
		})
		if err == nil {
			t.Fatal("expected a tenant-scoped INSERT to be denied by RLS")
		}
		assertPgCode(t, err, pgRLSViolation)
	})

	t.Run("player_scoped_insert_denied", func(t *testing.T) {
		err := insertAttempt(func(fn func(context.Context, pgx.Tx) error) error {
			return pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, fn)
		})
		if err == nil {
			t.Fatal("expected a player-scoped INSERT to be denied by RLS")
		}
		assertPgCode(t, err, pgRLSViolation)
	})

	t.Run("platform_admin_insert_permitted", func(t *testing.T) {
		err := insertAttempt(func(fn func(context.Context, pgx.Tx) error) error {
			return pool.WithPlatformAdmin(context.Background(), f.platformAdmin, fn)
		})
		if err != nil {
			t.Fatalf("expected a platform-admin-scoped INSERT to succeed, got %v", err)
		}
	})

	t.Run("delete_denied_even_for_platform_admin", func(t *testing.T) {
		id, _ := rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
			licensingJurisdictionID: f.jurisdiction2, operationClass: OperationBonusIssuance,
			status: "draft", locationRequirement: "unset",
		})
		err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM jurisdiction_precedence_configs WHERE id = $1`, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Fatalf("expected the DELETE to affect 0 rows (no DELETE policy exists), affected %d", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("delete attempt: %v", err)
		}
	})

	// Structural check: exactly SELECT/INSERT/UPDATE policies exist, no
	// DELETE, no FOR ALL - mirrors
	// TestJurisdictionResolutionActive_DeleteIsDeniedByRLS's own final
	// assertion.
	t.Run("policy_shape_has_no_delete_or_for_all", func(t *testing.T) {
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT cmd FROM pg_policies WHERE tablename = 'jurisdiction_precedence_configs' ORDER BY cmd`)
			if err != nil {
				return err
			}
			defer rows.Close()
			var cmds []string
			for rows.Next() {
				var cmd string
				if err := rows.Scan(&cmd); err != nil {
					return err
				}
				cmds = append(cmds, cmd)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			want := map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true}
			if len(cmds) != len(want) {
				t.Fatalf("expected exactly SELECT/INSERT/UPDATE policies, got %v", cmds)
			}
			for _, cmd := range cmds {
				if !want[cmd] {
					t.Fatalf("unexpected policy command %q (DELETE and ALL are both forbidden), full set %v", cmd, cmds)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("policy shape check: %v", err)
		}
	})
}

// --- Category 16: staleness semantics, config-sourced additions ---

func TestDeterminePlayerJurisdiction_ConfigSourcedPolicy_StalenessSemantics(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationBonusConversion

	rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: oc,
		status: "active", locationRequirement: "required", maxAgeSeconds: i64Ptr(60),
		legalReviewReference: strPtr("legal-ref-staleness"),
	})

	asOf := time.Now()
	var policy EvaluationPolicy
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		policy, _, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{TenantID: f.tenantID, OperationClass: oc, AsOf: asOf})
		return err
	})
	if err != nil {
		t.Fatalf("ResolveEvaluationPolicy: %v", err)
	}

	verified := &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOf.Add(-time.Hour), VerificationID: uuid.New()}

	// Inclusive boundary: age exactly == MaxLocationSignalAge counts as fresh.
	t.Run("inclusive_boundary_is_fresh", func(t *testing.T) {
		res, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose:  PurposeMarketAccessControl,
			Evidence: EvidenceSet{VerifiedResidence: verified, LocationSignal: &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOf.Add(-60 * time.Second)}},
			Policy:   policy, AsOf: asOf,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Outcome() != Resolved || res.LocationSignalState() != LocationObserved {
			t.Fatalf("expected resolved+observed at the inclusive boundary, got %s/%s", res.Outcome(), res.LocationSignalState())
		}
	})

	// Future-dated signal (age < 0) must be stale.
	t.Run("future_dated_signal_is_stale", func(t *testing.T) {
		res, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose:  PurposeMarketAccessControl,
			Evidence: EvidenceSet{VerifiedResidence: verified, LocationSignal: &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOf.Add(time.Hour)}},
			Policy:   policy, AsOf: asOf,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Outcome() != Unresolved || res.Reason() != ReasonLocationSignalUnusable {
			t.Fatalf("expected unresolved(location_signal_unusable) for a future-dated signal, got %s(%s)", res.Outcome(), res.Reason())
		}
	})

	// A very large configured age is stored and honoured as-is, with no
	// special-casing.
	t.Run("very_large_configured_age_honoured_as_is", func(t *testing.T) {
		rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
			licensingJurisdictionID: f.jurisdictionID, operationClass: OperationCatalogueAvailability,
			status: "active", locationRequirement: "required", maxAgeSeconds: i64Ptr(10 * 365 * 24 * 3600), // ~10 years
			legalReviewReference: strPtr("legal-ref-large-age"),
		})
		asOf2 := time.Now()
		var largeAgePolicy EvaluationPolicy
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			largeAgePolicy, _, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{TenantID: f.tenantID, OperationClass: OperationCatalogueAvailability, AsOf: asOf2})
			return err
		})
		if err != nil {
			t.Fatalf("ResolveEvaluationPolicy: %v", err)
		}
		wantSeconds := int64(10 * 365 * 24 * 3600)
		if largeAgePolicy.MaxLocationSignalAge == nil || int64(*largeAgePolicy.MaxLocationSignalAge/time.Second) != wantSeconds {
			t.Fatalf("expected the large age to be stored and read back unchanged, got %v", largeAgePolicy.MaxLocationSignalAge)
		}
		res, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose:  PurposeMarketAccessControl,
			Evidence: EvidenceSet{VerifiedResidence: verified, LocationSignal: &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOf2.Add(-5 * 365 * 24 * time.Hour)}},
			Policy:   largeAgePolicy, AsOf: asOf2,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Outcome() != Resolved {
			t.Fatalf("expected the very old signal to still be honoured as fresh under the very large configured age, got %s(%s)", res.Outcome(), res.Reason())
		}
	})
}

// --- Category 17: clock purity ---

func TestEvaluationPolicySQL_ContainsNoNowCall(t *testing.T) {
	src, err := os.ReadFile("evaluation_policy.go")
	if err != nil {
		t.Fatalf("read evaluation_policy.go: %v", err)
	}
	// Strip comment lines before scanning - the file's own doc comments
	// discuss the now()-purity rule in prose, which must not itself trip
	// this check. Only executable code (in particular, the SQL query
	// strings) is scanned.
	var codeOnly strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		codeOnly.WriteString(line)
		codeOnly.WriteString("\n")
	}
	if strings.Contains(codeOnly.String(), "now()") {
		t.Fatal("evaluation_policy.go's selection SQL must never call now() - AsOf is the only source of \"now\"")
	}
}

// --- Category 18: tenant-scope assertion ---

func TestResolveEvaluationPolicy_BarePoolConnectionErrorsNotNotConfigured(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{TenantID: f.tenantID, OperationClass: OperationPlay, AsOf: time.Now()})
		return err
	})
	if err == nil {
		t.Fatal("expected a genuine error on an unscoped connection")
	}
	if errors.Is(err, ErrPolicyNotConfigured) {
		t.Fatal("must not be misreported as ErrPolicyNotConfigured")
	}
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("expected ErrScopeMismatch, got %v", err)
	}
}

func TestResolveEvaluationPolicy_CrossTenantScopeIsScopeMismatch(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{TenantID: f.tenantID, OperationClass: OperationPlay, AsOf: time.Now()})
		return err
	})
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("expected ErrScopeMismatch for a cross-tenant lookup, got %v", err)
	}
}

func TestCreateEvaluationPolicyVersion_TenantScopedTxIsTransactionScope(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: OperationPlay, Status: EvaluationPolicyDraft,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "wrong-scope"},
		})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope for a tenant-scoped transaction, got %v", err)
	}
}

// --- Category 19: version mismatch ---

func TestResolveEvaluationPolicy_PrecedencePolicyVersionMismatch(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: OperationPlay,
		status: "active", locationRequirement: "advisory", precedencePolicyVersion: "stale-version-v0",
		legalReviewReference: strPtr("legal-ref-mismatch"),
	})

	var policy EvaluationPolicy
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		policy, _, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{TenantID: f.tenantID, OperationClass: OperationPlay, AsOf: time.Now()})
		return err
	})
	if !errors.Is(err, ErrPolicyVersionMismatch) {
		t.Fatalf("expected ErrPolicyVersionMismatch, got %v", err)
	}
	if policy != (EvaluationPolicy{}) {
		t.Errorf("expected the zero EvaluationPolicy on version mismatch, got %+v", policy)
	}
}

// --- Category 20: audit ---

// evaluationPolicyAuditMetadata reads the raw metadata JSONB for one
// jurisdiction_evaluation_policy.version_created audit row and unmarshals
// it, so before/after content can actually be asserted rather than merely
// declared-and-left-unread - a real audit row's metadata is only
// meaningful evidence if a test decodes it and compares field content,
// not merely confirms the row exists.
func evaluationPolicyAuditMetadata(t *testing.T, pool *db.Pool, targetID string) map[string]any {
	t.Helper()
	var raw string
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata::text FROM audit_log WHERE action = 'jurisdiction_evaluation_policy.version_created' AND target_id = $1`,
			targetID).Scan(&raw)
	})
	if err != nil {
		t.Fatalf("read audit metadata for %s: %v", targetID, err)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		t.Fatalf("unmarshal audit metadata: %v", err)
	}
	return metadata
}

// assertEvaluationPolicyAuditState asserts one side (before/after) of the
// audit metadata's policy-value content against the expected record's own
// evaluationPolicyRecordState rendering - the same function the write path
// itself uses to build the audit payload (evaluation_policy_admin.go).
func assertEvaluationPolicyAuditState(t *testing.T, metadata map[string]any, key string, want *EvaluationPolicyRecord) {
	t.Helper()
	got, present := metadata[key]
	if want == nil {
		if present && got != nil {
			t.Fatalf("expected metadata[%q] to be absent/null, got %v", key, got)
		}
		return
	}
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected metadata[%q] to be a JSON object, got %T (%v)", key, got, got)
	}
	wantMap := evaluationPolicyRecordState(*want)
	// Compare field-by-field with the exact JSON encodings
	// evaluationPolicyRecordState produces, rather than deep-equal on Go
	// values directly, since json.Unmarshal into `any` yields
	// float64/string types that do not match the source map's native types.
	wantJSON, err := json.Marshal(wantMap)
	if err != nil {
		t.Fatalf("marshal expected %s state: %v", key, err)
	}
	var wantAsAny map[string]any
	if err := json.Unmarshal(wantJSON, &wantAsAny); err != nil {
		t.Fatalf("round-trip expected %s state: %v", key, err)
	}
	if len(gotMap) != len(wantAsAny) {
		t.Fatalf("metadata[%q]: expected %d keys %v, got %d keys %v", key, len(wantAsAny), wantAsAny, len(gotMap), gotMap)
	}
	for k, wantV := range wantAsAny {
		gotV, ok := gotMap[k]
		if !ok {
			t.Fatalf("metadata[%q] missing key %q (full: %v)", key, k, gotMap)
		}
		if gotV != wantV {
			t.Fatalf("metadata[%q].%s: expected %v, got %v", key, k, wantV, gotV)
		}
	}
}

func TestCreateEvaluationPolicyVersion_AuditRecordShape(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var rec EvaluationPolicyRecord
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: OperationPlay, Status: EvaluationPolicyDraft,
			LocationSignalRequirement: LocationAdvisory,
			Actor:                     ActorContext{ActorID: f.platformAdmin, ReasonCode: "audit-shape", IPAddress: "10.0.0.1", UserAgent: "test-agent", RequestID: "req-123"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("CreateEvaluationPolicyVersion: %v", err)
	}

	if got := countEvaluationPolicyAuditRows(t, pool, rec.ID.String()); got != 1 {
		t.Fatalf("expected exactly 1 audit row, got %d", got)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var tenantID *uuid.UUID
		var ip, ua, reqID, reasonCode string
		if err := tx.QueryRow(ctx, `
			SELECT tenant_id, host(ip_address), user_agent, request_id, metadata ->> 'reason_code'
			  FROM audit_log
			 WHERE action = 'jurisdiction_evaluation_policy.version_created' AND target_id = $1`,
			rec.ID.String()).Scan(&tenantID, &ip, &ua, &reqID, &reasonCode); err != nil {
			return err
		}
		if tenantID != nil {
			t.Errorf("expected tenant_id IS NULL, got %v", *tenantID)
		}
		if ip != "10.0.0.1" || ua != "test-agent" || reqID != "req-123" {
			t.Errorf("expected the actor's IP/UA/request id to be recorded, got %q/%q/%q", ip, ua, reqID)
		}
		if reasonCode != "audit-shape" {
			t.Errorf("expected reason_code %q, got %q", "audit-shape", reasonCode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit shape check: %v", err)
	}

	t.Run("fresh_authoring_before_absent_after_matches", func(t *testing.T) {
		metadata := evaluationPolicyAuditMetadata(t, pool, rec.ID.String())
		assertEvaluationPolicyAuditState(t, metadata, "before", nil)
		assertEvaluationPolicyAuditState(t, metadata, "after", &rec)
	})

	t.Run("supersession_before_matches_prior_after_matches_new", func(t *testing.T) {
		var second EvaluationPolicyRecord
		err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			second, err = CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
				LicensingJurisdictionID: f.jurisdictionID, OperationClass: OperationPlay, Status: EvaluationPolicyDraft,
				LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: durPtr(90 * time.Second),
				Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "audit-shape-v2"},
			})
			return err
		})
		if err != nil {
			t.Fatalf("create v2 (supersede v1): %v", err)
		}
		// The captured "before" state is v1 AS IT WAS READ, prior to the
		// closing UPDATE in the same transaction - so its own EffectiveTo is
		// still nil at capture time, exactly matching rec's own state here.
		metadata := evaluationPolicyAuditMetadata(t, pool, second.ID.String())
		assertEvaluationPolicyAuditState(t, metadata, "before", &rec)
		assertEvaluationPolicyAuditState(t, metadata, "after", &second)
	})
}

func TestCreateEvaluationPolicyVersion_RejectedWriteProducesNoAuditNoRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	before := countAllEvaluationPolicyAuditRows(t, pool)
	beforeRows := countAllPolicyRows(t, pool, f.jurisdictionID, OperationPlay)

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: OperationPlay, Status: EvaluationPolicyActive,
			LegalReviewReference: "ref",
			Actor:                ActorContext{ActorID: f.platformAdmin, ReasonCode: "attempt"},
		})
		return err
	})
	if !errors.Is(err, ErrActivationNotAuthorized) {
		t.Fatalf("expected ErrActivationNotAuthorized, got %v", err)
	}

	if got := countAllEvaluationPolicyAuditRows(t, pool); got != before {
		t.Fatalf("expected zero new audit rows for a rejected write, before=%d after=%d", before, got)
	}
	if got := countAllPolicyRows(t, pool, f.jurisdictionID, OperationPlay); got != beforeRows {
		t.Fatalf("expected zero new config rows for a rejected write, before=%d after=%d", beforeRows, got)
	}
}

// --- Category 22: PC-GAP-3 seam, repo-wide assertion ---

// repoRoot walks up from the current working directory (a Go test's cwd is
// always its own package directory) until it finds go.mod, so a "repo-wide"
// scan test can actually be repo-wide rather than package-local.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve absolute path: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod by walking up from the test's working directory")
		}
		dir = parent
	}
}

// TestOperationPurposeMapping_SingleCanonicalOwner scans every non-test Go
// file in the ENTIRE repository (except operation_purpose.go itself) for a
// PurposeRequirement{...} composite literal - the structural proxy for "no
// other file maps OperationClass to Purpose". PurposeRequirement is
// exported, so another package could theoretically construct one; this
// test's own label ("repo-wide") previously did not match what it scanned
// (only this package's own directory), which this widening corrects.
func TestOperationPurposeMapping_SingleCanonicalOwner(t *testing.T) {
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if name == "operation_purpose.go" && filepath.Base(filepath.Dir(path)) == "jurisdiction" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "PurposeRequirement{") {
			t.Errorf("%s constructs a PurposeRequirement{} literal - RequiredPurposes (internal/jurisdiction/operation_purpose.go) must be the single canonical owner of OperationClass -> Purpose mapping content", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
}

// --- Category 23 (partial, DB-dependent): config-sourced policy case for
// the Phase C invariant test ---

// TestDetermine_NeverEmitsATenantOrFallbackBasis_ConfigSourcedPolicy
// extends precedence_invariants_test.go's
// TestDetermine_NeverEmitsATenantOrFallbackBasisOnAnyInput with a policy
// constructed via ResolveEvaluationPolicy from a REAL seeded config row,
// rather than a caller-constructed literal - proving the invariant holds
// end-to-end through the new configuration seam, not just against
// hand-written EvaluationPolicy values.
func TestDetermine_NeverEmitsATenantOrFallbackBasis_ConfigSourcedPolicy(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationBonusIssuance

	rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: oc,
		status: "active", locationRequirement: "required", maxAgeSeconds: i64Ptr(3600),
		legalReviewReference: strPtr("legal-ref-invariant"),
	})

	asOf := time.Now()
	var policy EvaluationPolicy
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		policy, _, err = ResolveEvaluationPolicy(ctx, tx, EvaluationPolicyLookup{TenantID: f.tenantID, OperationClass: oc, AsOf: asOf})
		return err
	})
	if err != nil {
		t.Fatalf("ResolveEvaluationPolicy: %v", err)
	}

	permittedBases := map[Basis]bool{
		BasisPlayerVerifiedResidence: true,
		BasisPlayerDeclaredResidence: true,
		BasisGeoSignal:               true,
	}
	inputs := []EvidenceSet{
		{},
		{VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOf.Add(-time.Hour), VerificationID: uuid.New()}},
		{VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOf.Add(-time.Hour), VerificationID: uuid.New()},
			LocationSignal: &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOf.Add(-time.Minute)}},
		{DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "DE", CapturedAt: asOf.Add(-time.Hour), PlayerAccountID: uuid.New()}},
	}
	for _, ev := range inputs {
		res, err := DeterminePlayerJurisdiction(DetermineParams{Purpose: PurposeMarketAccessControl, Evidence: ev, Policy: policy, AsOf: asOf})
		if err != nil {
			continue
		}
		if res.Outcome() != Resolved {
			continue
		}
		cands, cerr := res.Candidates()
		if cerr != nil {
			t.Fatalf("resolved result must expose Candidates(): %v", cerr)
		}
		for _, c := range cands {
			if !permittedBases[c.Basis()] {
				t.Fatalf("candidate carries forbidden basis %q from a config-sourced policy", c.Basis())
			}
		}
	}
}

// --- Category 24: ListEvaluationPolicyVersions - scope assertion and
// happy path (P2 security finding: this function previously took NO
// scope assertion at all, so any tenant- or player-scoped caller could
// enumerate another licensing jurisdiction's full version history) ---

func TestListEvaluationPolicyVersions_TenantScopedTxIsTransactionScope(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ListEvaluationPolicyVersions(ctx, tx, f.jurisdictionID, OperationPlay)
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope for a tenant-scoped transaction, got %v", err)
	}
}

func TestListEvaluationPolicyVersions_PlayerScopedTxIsTransactionScope(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ListEvaluationPolicyVersions(ctx, tx, f.jurisdictionID, OperationPlay)
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope for a player-scoped transaction, got %v", err)
	}
}

// TestListEvaluationPolicyVersions_HappyPathReturnsFullHistoryNewestFirst
// is the missing happy-path coverage: this file's own header comment
// claimed ListEvaluationPolicyVersions coverage while no test anywhere
// actually called it (confirmed by grep) - a real "claims coverage it
// doesn't have" issue under CLAUDE.md's no-fake-completion rule.
func TestListEvaluationPolicyVersions_HappyPathReturnsFullHistoryNewestFirst(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationBonusConversion

	var first, second EvaluationPolicyRecord
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: oc, Status: EvaluationPolicyDraft,
			LocationSignalRequirement: LocationAdvisory,
			Actor:                     ActorContext{ActorID: f.platformAdmin, ReasonCode: "list-v1"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = CreateEvaluationPolicyVersion(ctx, tx, CreateEvaluationPolicyVersionParams{
			LicensingJurisdictionID: f.jurisdictionID, OperationClass: oc, Status: EvaluationPolicyDraft,
			LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: durPtr(45 * time.Second),
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "list-v2"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("create v2 (supersede v1): %v", err)
	}

	var history []EvaluationPolicyRecord
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		history, err = ListEvaluationPolicyVersions(ctx, tx, f.jurisdictionID, oc)
		return err
	})
	if err != nil {
		t.Fatalf("ListEvaluationPolicyVersions: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected exactly 2 versions, got %d", len(history))
	}
	if history[0].ID != second.ID {
		t.Fatalf("expected newest-first ordering: history[0] should be v2 (%s), got %s", second.ID, history[0].ID)
	}
	if history[1].ID != first.ID {
		t.Fatalf("expected newest-first ordering: history[1] should be v1 (%s), got %s", first.ID, history[1].ID)
	}
	if history[0].LocationSignalRequirement != LocationRequired {
		t.Errorf("expected history[0] (v2) to carry LocationRequired, got %q", history[0].LocationSignalRequirement)
	}
	if history[1].LocationSignalRequirement != LocationAdvisory {
		t.Errorf("expected history[1] (v1) to carry LocationAdvisory, got %q", history[1].LocationSignalRequirement)
	}
	if history[1].EffectiveTo == nil {
		t.Error("expected v1 (now superseded) to have a non-nil effective_to")
	}
	if history[0].EffectiveTo != nil {
		t.Error("expected v2 (the currently open version) to have a nil effective_to")
	}
}

// --- Category 25: trigger-level contiguity, closing the P3 finding both
// architect and security independently reproduced ---

// TestJurisdictionPrecedenceConfigs_ForgedEffectiveToOverwritten proves
// that the append-only trigger, not just the sanctioned Go write path,
// makes it structurally impossible to construct an overlapping
// [effective_from, effective_to) window: a raw platform-admin UPDATE
// attempting to close a row at a caller-supplied FAR-FUTURE timestamp is
// silently forced to the real closing time instead, exactly mirroring how
// the BEFORE INSERT trigger already forges effective_from.
func TestJurisdictionPrecedenceConfigs_ForgedEffectiveToOverwritten(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	oc := OperationBonusIssuance

	id, _ := rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: oc,
		status: "draft", locationRequirement: "unset",
	})

	forged := time.Now().Add(365 * 24 * time.Hour) // one year in the future
	var actualEffectiveTo time.Time
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`UPDATE jurisdiction_precedence_configs SET effective_to = $2 WHERE id = $1 RETURNING effective_to`,
			id, forged).Scan(&actualEffectiveTo)
	})
	if err != nil {
		t.Fatalf("forged close attempt: %v", err)
	}
	if actualEffectiveTo.Equal(forged) {
		t.Fatal("expected the trigger to overwrite a caller-supplied future effective_to, but the forged value was stored")
	}
	if time.Since(actualEffectiveTo) > time.Minute {
		t.Errorf("expected effective_to to be genuinely recent (forced to now()), got %v", actualEffectiveTo)
	}

	// With the close forced to the real time, a successor's (also forced)
	// effective_from cannot land inside the closed row's window - no gap,
	// no overlap, regardless of what any writer asked for.
	_, successorFrom := rawInsertPolicyRow(t, pool, f.platformAdmin, rawPolicyRow{
		licensingJurisdictionID: f.jurisdictionID, operationClass: oc,
		status: "draft", locationRequirement: "unset",
	})
	if successorFrom.Before(actualEffectiveTo) {
		t.Fatalf("expected the successor's effective_from (%v) to be >= the forced close time (%v) - no overlap constructible", successorFrom, actualEffectiveTo)
	}

	var overlaps int
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM jurisdiction_precedence_configs a
			 JOIN jurisdiction_precedence_configs b
			   ON a.licensing_jurisdiction_id = b.licensing_jurisdiction_id
			  AND a.operation_class = b.operation_class
			  AND a.id <> b.id
			 WHERE a.licensing_jurisdiction_id = $1 AND a.operation_class = $2
			   AND a.effective_from < COALESCE(b.effective_to, 'infinity'::timestamptz)
			   AND COALESCE(a.effective_to, 'infinity'::timestamptz) > b.effective_from`,
			f.jurisdictionID, string(oc)).Scan(&overlaps)
	})
	if err != nil {
		t.Fatalf("overlap check: %v", err)
	}
	if overlaps != 0 {
		t.Fatalf("expected zero overlapping windows even after a forged-future-effective_to attempt, found %d", overlaps)
	}
}

// --- Category 26: raw-SQL bypass of each of migration 0075's four
// table-level CHECK constraints, one INSERT per constraint ---

func TestJurisdictionPrecedenceConfigs_TableCheckConstraints(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	insertRaw := func(oc OperationClass, status, locationRequirement string, maxAgeSeconds *int64, legalReviewReference *string) error {
		return pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO jurisdiction_precedence_configs (
					licensing_jurisdiction_id, operation_class, precedence, status,
					location_requirement, max_location_signal_age_seconds,
					precedence_status, resolver_policy_version, precedence_policy_version,
					legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id
				) VALUES ($1, $2, '[]'::jsonb, $3, $4, $5, 'unset', $6, $7, $8, 'check-bypass-attempt', 'staff', $9)`,
				f.jurisdictionID, string(oc), status, locationRequirement, maxAgeSeconds,
				PolicyVersion, PrecedencePolicyVersion, legalReviewReference, f.platformAdmin)
			return err
		})
	}

	t.Run("effective_to_after_from_violation", func(t *testing.T) {
		// The stamp-times trigger forces effective_from AND (since this fix
		// round) effective_to to now() unconditionally, so a caller-supplied
		// forged value can never reach either CHECK directly. But
		// PostgreSQL's now() is the TRANSACTION timestamp, constant for the
		// whole transaction - so INSERTing a row and then closing it within
		// the SAME transaction forces effective_from == effective_to
		// (identical, not strictly greater), which the
		// effective_to_after_from CHECK correctly rejects. This is the one
		// raw-SQL path that still reaches this specific constraint.
		err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `
				INSERT INTO jurisdiction_precedence_configs (
					licensing_jurisdiction_id, operation_class, precedence, status,
					location_requirement, resolver_policy_version, precedence_policy_version,
					reason_code, created_by_actor_type, created_by_actor_id
				) VALUES ($1, $2, '[]'::jsonb, 'draft', 'unset', $3, $4, 'check-bypass-attempt', 'staff', $5)
				RETURNING id`,
				f.jurisdictionID, string(OperationPlay), PolicyVersion, PrecedencePolicyVersion, f.platformAdmin,
			).Scan(&id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE jurisdiction_precedence_configs SET effective_to = now() WHERE id = $1`, id)
			return err
		})
		if err == nil {
			t.Fatal("expected a CHECK violation when closing a row within the same transaction it was created in")
		}
		assertPgCode(t, err, pgCheckViolation)
	})

	t.Run("precedence_status_matches_violation", func(t *testing.T) {
		// precedence_status = 'configured' while precedence remains '[]'::jsonb
		// violates jurisdiction_precedence_configs_precedence_status_matches -
		// exercised directly since this package's own write API cannot
		// produce this combination.
		err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO jurisdiction_precedence_configs (
					licensing_jurisdiction_id, operation_class, precedence, status,
					location_requirement, precedence_status, resolver_policy_version, precedence_policy_version,
					reason_code, created_by_actor_type, created_by_actor_id
				) VALUES ($1, $2, '[]'::jsonb, 'draft', 'unset', 'configured', $3, $4, 'check-bypass-attempt', 'staff', $5)`,
				f.jurisdictionID, string(OperationCatalogueAvailability), PolicyVersion, PrecedencePolicyVersion, f.platformAdmin)
			return err
		})
		if err == nil {
			t.Fatal("expected a CHECK violation for precedence_status='configured' with an empty precedence array")
		}
		assertPgCode(t, err, pgCheckViolation)
	})

	t.Run("withdrawn_no_content_violation", func(t *testing.T) {
		err := insertRaw(OperationBonusIssuance, "withdrawn", "required", i64Ptr(60), nil)
		if err == nil {
			t.Fatal("expected a CHECK violation for a withdrawn row carrying location_requirement content")
		}
		assertPgCode(t, err, pgCheckViolation)
	})

	t.Run("active_requires_legal_review_violation", func(t *testing.T) {
		err := insertRaw(OperationBonusConversion, "active", "required", i64Ptr(60), nil)
		if err == nil {
			t.Fatal("expected a CHECK violation for an active row with no legal_review_reference")
		}
		assertPgCode(t, err, pgCheckViolation)
	})
}

// --- Category 27: clock purity, precedence.go ---

// TestPrecedenceGoContainsNoNowCall mirrors
// TestEvaluationPolicySQL_ContainsNoNowCall's own style, applied to Go
// source instead of embedded SQL: precedence.go's own doc comment already
// asserts DeterminePlayerJurisdiction is a pure function that never calls
// time.Now() - AsOf is the only source of "now" - and this test pins that
// invariant so a future edit cannot reintroduce it silently.
func TestPrecedenceGoContainsNoNowCall(t *testing.T) {
	src, err := os.ReadFile("precedence.go")
	if err != nil {
		t.Fatalf("read precedence.go: %v", err)
	}
	var codeOnly strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		codeOnly.WriteString(line)
		codeOnly.WriteString("\n")
	}
	if strings.Contains(codeOnly.String(), "time.Now()") {
		t.Fatal("precedence.go must never call time.Now() - DeterminePlayerJurisdiction is a pure function and AsOf is the only source of \"now\"")
	}
}

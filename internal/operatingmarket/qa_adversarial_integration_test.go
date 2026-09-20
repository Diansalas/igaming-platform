//go:build integration

// Independent QA adversarial coverage for Stage 4I Phase E (ADR 0045),
// written by the qa specialist during independent review, beyond the
// architect's §12 coverage floor. Each test here targets a specific
// judgment call or a specific way the resolution algorithm could be
// wrongly implemented (most-specific-wins instead of top-down-first-
// disabled-wins; a ceiling flip that rewrites lower rows; a "duplicate
// open row" that the unique index should make structurally impossible;
// STEP 4 falling back to a lower-ranked live row when the top-ranked
// candidate is withdrawn; and a player-scoped connection producing a
// misleading answer instead of a diagnosable error).
package operatingmarket

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestQAAdversarial_BrandDisabledBlocksDespiteOperationExplicitlyEnabled
// constructs exactly the case the architect ruling names as the reason
// top-down-first-disabled-wins is used instead of most-specific-wins: a
// brand row is validly disabled AFTER a more-specific operation row under
// that same brand was validly enabled. A most-specific-wins implementation
// would wrongly return permitted; the correct algorithm must stop at the
// brand rung and report disabled_by_brand.
func TestQAAdversarial_BrandDisabledBlocksDespiteOperationExplicitlyEnabled(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "PR"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	// Brand enabled first, so the operation-scope enable below is accepted
	// by the write-time trigger (which requires the brand rung enabled at
	// WRITE time for a brand-specific operation row).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "brand-ok",
			Actor: testActor(f.staffActorID, "enable-brand"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable brand policy: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "op-ok",
			Actor: testActor(f.staffActorID, "enable-brand-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable brand-scoped operation policy: %v", err)
	}

	// Now the brand is explicitly disabled - the write-time trigger only
	// checks upward AT WRITE TIME, so the operation row above is now
	// orphaned (still 'enabled' in storage), exactly mirroring
	// TestOperatingCountryPolicy_ExplicitDisableOverridesInheritedEnable
	// one rung deeper.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateDisabled, Status: StatusActive,
			Actor: testActor(f.staffActorID, "disable-brand"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand policy: %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC(),
	})
	if res.Outcome() != OutcomeDisabledByBrand {
		t.Fatalf("expected disabled_by_brand (top-down first-disabled-wins must stop at the brand rung BEFORE ever inspecting the operation rung, even though the operation row is validly 'enabled'), got %s - a most-specific-wins implementation would wrongly return %s here", res.Outcome(), OutcomePermitted)
	}
	if res.Permitted() {
		t.Fatal("must never be Permitted() when a higher rung is disabled, regardless of what a lower rung says")
	}
}

// TestQAAdversarial_LicenceCeilingFlipLeavesLowerRowsPhysicallyUntouched
// goes beyond TestOperatingCountryPolicy_LicenceContractionImmediatelyUnavailable
// by directly inspecting the tenant/brand/operation rows in the database,
// byte-for-byte, before and after a ceiling contraction - confirming not
// merely that resolution flips, but that NO lower-scope row is rewritten,
// closed, or replaced as a side effect of the ceiling change.
func TestQAAdversarial_LicenceCeilingFlipLeavesLowerRowsPhysicallyUntouched(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "GD"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	tenantRec := enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	brandRec, err := writeBrandPolicy(t, pool, f, cc, StateEnabled)
	if err != nil {
		t.Fatalf("enable brand: %v", err)
	}
	casino := "casino"
	var opRec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		opRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering,
			ProductCode: &casino, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "op-ok", Actor: testActor(f.staffActorID, "enable-op"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable operation policy: %v", err)
	}

	before := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC(),
	})
	if before.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted before the ceiling flip, got %s", before.Outcome())
	}

	type snapshot struct {
		id            uuid.UUID
		state         string
		effectiveFrom time.Time
		effectiveTo   *time.Time
	}
	readSnapshot := func(id uuid.UUID) snapshot {
		var s snapshot
		s.id = id
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state, effective_from, effective_to FROM operating_country_policies WHERE id = $1`, id).
				Scan(&s.state, &s.effectiveFrom, &s.effectiveTo)
		})
		if err != nil {
			t.Fatalf("read snapshot for %s: %v", id, err)
		}
		return s
	}

	tenantBefore := readSnapshot(tenantRec.ID)
	brandBefore := readSnapshot(brandRec.ID)
	opBefore := readSnapshot(opRec.ID)

	disableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	after := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC(),
	})
	if after.Outcome() != OutcomeNotPermittedByLicence {
		t.Fatalf("expected not_permitted_by_licence immediately after the ceiling flip, got %s", after.Outcome())
	}

	tenantAfter := readSnapshot(tenantRec.ID)
	brandAfter := readSnapshot(brandRec.ID)
	opAfter := readSnapshot(opRec.ID)

	// Also confirm no NEW row was created for any of the three keys - the
	// row count for each key must still be exactly 1.
	var tenantCount, brandCount, opCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2`, f.tenantID, cc).Scan(&tenantCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'brand' AND brand_id = $2 AND country_code = $3`, f.tenantID, f.brandID, cc).Scan(&brandCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'operation' AND country_code = $2 AND operation_code = $3`, f.tenantID, cc, opWagering).Scan(&opCount)
	})
	if err != nil {
		t.Fatalf("count rows after ceiling flip: %v", err)
	}
	if tenantCount != 1 || brandCount != 1 || opCount != 1 {
		t.Fatalf("expected exactly 1 row per key after the ceiling flip (no rewrite, no new version), got tenant=%d brand=%d operation=%d", tenantCount, brandCount, opCount)
	}

	for label, pair := range map[string][2]snapshot{
		"tenant":    {tenantBefore, tenantAfter},
		"brand":     {brandBefore, brandAfter},
		"operation": {opBefore, opAfter},
	} {
		b, a := pair[0], pair[1]
		if b.state != a.state {
			t.Fatalf("%s row's state changed as a side effect of the ceiling flip: %q -> %q", label, b.state, a.state)
		}
		if !b.effectiveFrom.Equal(a.effectiveFrom) {
			t.Fatalf("%s row's effective_from changed as a side effect of the ceiling flip: %v -> %v", label, b.effectiveFrom, a.effectiveFrom)
		}
		bTo, aTo := "nil", "nil"
		if b.effectiveTo != nil {
			bTo = b.effectiveTo.String()
		}
		if a.effectiveTo != nil {
			aTo = a.effectiveTo.String()
		}
		if bTo != aTo {
			t.Fatalf("%s row's effective_to changed as a side effect of the ceiling flip: %s -> %s (a ceiling change must NEVER close or rewrite a lower-scope row)", label, bTo, aTo)
		}
	}
}

// TestQAAdversarial_DuplicateTrulyOpenRowsAreStructurallyImpossible attempts
// to construct two simultaneously-open (effective_to IS NULL) rows for the
// SAME tenant-scope key via raw SQL, bypassing this package's own
// application discipline entirely (not just the ordinary write path) -
// confirming the partial unique index makes this impossible even for a
// determined, misbehaving direct-SQL caller, as opposed to the
// effective_to-windowed overlap the DuplicateInForceYieldsConfigurationConflict
// test constructs (which deliberately does NOT collide with the index).
func TestQAAdversarial_DuplicateTrulyOpenRowsAreStructurallyImpossible(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "LC"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	rec := enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, country_code, state, status, authorization_reference,
				reason_code, policy_version, created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'tenant', $2, 'enabled', 'active', 'raw-sql-duplicate-attempt',
			          'adversarial-duplicate-open-row-attempt', $3, 'staff', $4)`,
			f.tenantID, cc, PolicyVersion, f.staffActorID)
		return err
	})
	if err == nil {
		t.Fatal("expected a second raw-SQL-inserted truly-open row for the same tenant-scope key to be REJECTED by the partial unique index - it succeeded instead")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		t.Fatalf("expected SQLSTATE %s (unique_violation) from the partial unique index, got %v", pgUniqueViolation, err)
	}

	// The original row must still be the sole open version.
	var openCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2 AND effective_to IS NULL`, f.tenantID, cc).Scan(&openCount)
	})
	if err != nil {
		t.Fatalf("count open rows: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("expected exactly 1 open row to survive the rejected duplicate attempt, got %d", openCount)
	}
	if rec.ID == uuid.Nil {
		t.Fatal("sanity: original record id must be set")
	}
}

// TestQAAdversarial_Step4WithdrawnTopCandidateInheritsWithNoFallbackToLiveRow
// constructs the exact scenario the implementing agent's handback
// disclosed as a judgment call: a MORE SPECIFIC operation-scope row (brand-
// specific) is withdrawn, while a LESS SPECIFIC operation-scope row
// (tenant-wide, for the same operation/country) exists and is validly
// enabled. Per ADR 0045 §3.5 STEP 4, "no candidate or status == withdrawn
// -> inherit (source unchanged)" - the withdrawn TOP-RANKED candidate must
// cause an outright inherit (falling through to whatever the brand/tenant
// rung already established), and must NOT fall back to consider the
// lower-ranked, live, tenant-wide row at all. This test fails if a plausible-
// but-wrong implementation instead treats "top candidate withdrawn" as "look
// at the next candidate".
func TestQAAdversarial_Step4WithdrawnTopCandidateInheritsWithNoFallbackToLiveRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "VC"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	if _, err := writeBrandPolicy(t, pool, f, cc, StateEnabled); err != nil {
		t.Fatalf("enable brand: %v", err)
	}

	// Lower-ranked candidate: tenant-wide (brand_id IS NULL), every-product,
	// operation row - ENABLED and left ACTIVE (live).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "tenant-wide-op-ok",
			Actor: testActor(f.staffActorID, "enable-tenant-wide-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable tenant-wide operation row: %v", err)
	}

	// Higher-ranked candidate: brand-specific operation row - created
	// ENABLED (valid at write time, since the brand is enabled), then
	// immediately WITHDRAWN via a second version. A withdrawn row's
	// `status` is 'withdrawn' and its CHECK forces state='disabled', so it
	// is unambiguously not a live enable - but it is still the MOST
	// SPECIFIC candidate by rank (brand_id IS NOT NULL beats brand_id IS
	// NULL), and must therefore govern step 4's decision (-> inherit),
	// never falling back to the lower-ranked tenant-wide row.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "brand-specific-op-ok",
			Actor: testActor(f.staffActorID, "enable-brand-specific-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable brand-specific operation row: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "withdraw-brand-specific-operation-ref",
			Actor: testActor(f.staffActorID, "withdraw-brand-specific-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw brand-specific operation row: %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC(),
	})
	// Both the tenant and brand rungs are enabled, and step 4 must
	// INHERIT (not evaluate the lower-ranked live row at all), so the
	// final outcome is permitted - sourced from the brand rung, not from
	// the withdrawn-but-most-specific operation row nor from the
	// lower-ranked tenant-wide operation row.
	if res.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted (withdrawn top candidate inherits from the brand rung above), got %s", res.Outcome())
	}
}

// TestQAAdversarial_PlayerScopedResolveGetsErrTransactionScopeNotAMisleadingAnswer
// re-verifies the implementing agent's third self-disclosed judgment call:
// calling ResolveOperatingCountryPolicy itself (not a raw SQL SELECT) under
// a player-scoped transaction must return ErrTransactionScope, never a
// misleading not_configured/false answer produced by RLS silently
// filtering every row to zero.
func TestQAAdversarial_PlayerScopedResolveGetsErrTransactionScopeNotAMisleadingAnswer(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "GY"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		playerID = uuid.New()
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerID, f.tenantID, f.brandID, personID, playerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed player: %v", err)
	}

	var gotErr error
	poolErr := pool.WithPlayerScope(context.Background(), f.tenantID, playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, gotErr = ResolveOperatingCountryPolicy(ctx, tx, Query{
			TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC(),
		})
		return nil
	})
	if poolErr != nil {
		t.Fatalf("pool error: %v", poolErr)
	}
	if gotErr == nil {
		t.Fatal("expected ResolveOperatingCountryPolicy to return a non-nil error for a player-scoped transaction - a nil error with a zero-value Result would be a misleading answer")
	}
	if !errors.Is(gotErr, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope specifically (a diagnosable server-side caller bug), got %v", gotErr)
	}

	// Same for IsRegistrationPermitted: must be (false, ErrTransactionScope),
	// never (false, nil) - which would be indistinguishable from a genuine
	// not-permitted answer.
	var permitted bool
	var regErr error
	poolErr = pool.WithPlayerScope(context.Background(), f.tenantID, playerID, func(ctx context.Context, tx pgx.Tx) error {
		permitted, regErr = IsRegistrationPermitted(ctx, tx, RegistrationQuery{
			TenantID: f.tenantID, CountryCode: cc, AsOf: time.Now().UTC(),
		})
		return nil
	})
	if poolErr != nil {
		t.Fatalf("pool error: %v", poolErr)
	}
	if permitted {
		t.Fatal("IsRegistrationPermitted must never return true under a player-scoped transaction")
	}
	if regErr == nil {
		t.Fatal("expected IsRegistrationPermitted to surface a non-nil error for a player-scoped transaction (fails closed, but should be diagnosable, not silently false)")
	}
	if !errors.Is(regErr, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope from IsRegistrationPermitted under player scope, got %v", regErr)
	}
}

// writeBrandPolicy is a small local helper (not in the shared fixture
// file) for tests in this file that need a brand-scope row without the
// tenant-wide enable/disable helpers' fixed semantics.
func writeBrandPolicy(t *testing.T, pool *db.Pool, f fixture, cc string, state State) (PolicyRecord, error) {
	t.Helper()
	var rec PolicyRecord
	var authRef string
	if state == StateEnabled {
		authRef = "brand-ref"
	}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: state, Status: StatusActive, AuthorizationReference: authRef,
			Actor: testActor(f.staffActorID, "write-brand-policy"),
		})
		return err
	})
	return rec, err
}

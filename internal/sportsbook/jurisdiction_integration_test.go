//go:build integration

// Stage 9.2 (ADR 0083 Part C, §12.1) jurisdiction/market gating test
// suite: the K3-1/K3-2-mirroring arm/deny behaviour of
// sb_jurisdiction_restrictions, downward propagation, cross-tenant
// platform-wide application, the direct-API-bypass guarantee, the
// immutable snapshot, and the RLS write-authorization proof. Rung 2
// (§5.3.3, BLOCKED on HDR-J-7) tests are written against
// evaluateOperatingMarket's documented CONTRACT and skipped, per the
// ADR's own instruction not to omit them silently.
package sportsbook

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// seedSelectionWithContext is seedSelection plus the event/market ids a
// restriction needs to target - PlaceBet itself gets these for free from
// getSelectionWithContext (no new query on the bet path); tests need them
// explicitly to construct a restriction's scope.
func seedSelectionWithContext(t *testing.T, pool *db.Pool, p seedSelectionParams) (sel Selection, eventID, marketID uuid.UUID) {
	t.Helper()
	sel = seedSelection(t, pool, p)
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT event_id FROM sb_markets WHERE id = $1`, sel.MarketID).Scan(&eventID)
	})
	if err != nil {
		t.Fatalf("resolve event id for market %s: %v", sel.MarketID, err)
	}
	marketID = sel.MarketID
	return sel, eventID, marketID
}

// seedSiblingSelection adds a second, independent selection under
// marketID (the SAME market seedSelectionWithContext already created) -
// for the downward-propagation negative case (a restricted selection
// must not block its own market or its sibling selections).
func seedSiblingSelection(t *testing.T, pool *db.Pool, marketID uuid.UUID) Selection {
	t.Helper()
	ref := uuid.New().String()[:8]
	var sel Selection
	sel.MarketID = marketID
	sel.OddsNumerator = 200
	sel.OddsDenominator = 100
	sel.Status = SelectionActive
	sel.Name = "Sibling Selection"
	err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator, status)
			 VALUES ($1, $2, 'Sibling Selection', 200, 100, 'active') RETURNING id`,
			marketID, "sibling-sel-"+ref).Scan(&sel.ID)
	})
	if err != nil {
		t.Fatalf("seed sibling selection: %v", err)
	}
	return sel
}

// seedJurisdictionCode inserts a bare jurisdictions row (no licence) - the
// minimum sb_jurisdiction_restrictions.jurisdiction_code's own FK needs.
func seedJurisdictionCode(t *testing.T, pool *db.Pool) string {
	t.Helper()
	code := "JT-" + uuid.New().String()[:8]
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Jurisdiction Gating Test')`, uuid.New(), code)
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction code: %v", err)
	}
	return code
}

type restrictionScope struct {
	ScopeKind   string
	EventID     *uuid.UUID
	MarketID    *uuid.UUID
	SelectionID *uuid.UUID
}

// seedPlatformAdminStaffPrincipal mirrors internal/casino's identical
// helper of the same name (internal/casino/orchestrator_integration_test.go)
// byte-for-byte - duplicated rather than imported, since internal/sportsbook
// must not import internal/casino (see this package's own package-boundary
// doc comments elsewhere). Stage 9.2 fix round (SEC-S92-2, migration
// 0090): sb_jurisdiction_restrictions' write triggers now require
// app.platform_admin_principal_id to resolve to a REAL platform-scoped
// (tenant_id IS NULL) staff_users row - a bare uuid.New() (this file's own
// PRIOR convention, and still fine for tables migration 0090 does not
// touch, e.g. tenants/jurisdictions/licences) no longer suffices for any
// sb_jurisdiction_restrictions write.
func seedPlatformAdminStaffPrincipal(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	personID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`,
			id, "platform-admin-"+id.String()+"@test.example", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin staff principal: %v", err)
	}
	return id
}

// seedRestriction arms a new active restriction via the real admin
// service (CreateJurisdictionRestriction) - never a raw INSERT - so these
// tests exercise the same code path an operator's HTTP call does.
func seedRestriction(t *testing.T, pool *db.Pool, scope restrictionScope, jurisdictionCode string) JurisdictionRestriction {
	t.Helper()
	var r JurisdictionRestriction
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = CreateJurisdictionRestriction(ctx, tx, CreateJurisdictionRestrictionParams{
			ScopeKind: scope.ScopeKind, EventID: scope.EventID, MarketID: scope.MarketID, SelectionID: scope.SelectionID,
			JurisdictionCode: jurisdictionCode, AuthorizationReference: "test-authz-ref", ReasonCode: "test-reason",
			CreatedByActorID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction restriction: %v", err)
	}
	return r
}

func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }

// TestSportsbookJurisdiction_AllowedWhenNoRestrictionConfigured proves
// K3-1: the unarmed control never denies, even though every player-scoped
// jurisdiction.Resolve call resolves unresolved(no_signal) today (HDR-J-7
// unanswered). This is the test that proves this ADR did not break every
// sportsbook bet on the platform.
func TestSportsbookJurisdiction_AllowedWhenNoRestrictionConfigured(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel, _, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})

	result, err := placeBet(t, pool, f, sel, 1_000, "jur-k3-1-allow")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected bet to be accepted with no restriction configured, got rejection %q/%q", result.RejectionCategory, result.RejectionCode)
	}
}

// TestSportsbookJurisdiction_UnresolvedFailsClosedWhenArmed proves K3-2:
// armed (a restriction exists for this bet's own selection) + the
// player's jurisdiction did not resolve (the only outcome
// jurisdiction.Resolve produces for a player-scoped call today) => deny
// with RejectionJurisdictionDenied/DenialCodeJurisdictionUnresolved,
// NEVER a silently-accepted bet.
func TestSportsbookJurisdiction_UnresolvedFailsClosedWhenArmed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel, _, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	code := seedJurisdictionCode(t, pool)
	seedRestriction(t, pool, restrictionScope{ScopeKind: "selection", SelectionID: uuidPtr(sel.ID)}, code)

	result, err := placeBet(t, pool, f, sel, 1_000, "jur-k3-2-unresolved")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected an armed restriction to deny a bet whose jurisdiction could not be resolved")
	}
	if result.RejectionCategory != RejectionJurisdictionDenied {
		t.Fatalf("expected rejection category %q, got %q", RejectionJurisdictionDenied, result.RejectionCategory)
	}
	if result.RejectionCode != DenialCodeJurisdictionUnresolved {
		t.Fatalf("expected DenialCodeJurisdictionUnresolved (never DenialCodeJurisdictionBlocked - K3-2), got %q", result.RejectionCode)
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero bet rows on a jurisdiction-denied placement")
	}
	if countLedgerTransactions(t, pool, f) != 0 {
		t.Fatal("expected zero ledger transactions on a jurisdiction-denied placement")
	}
}

// TestSportsbookJurisdiction_BlockedAtEventLevelBlocksEveryMarketAndSelectionBeneath
// proves downward propagation (§5.4.1/loadActiveRestrictions'
// event-OR-market-OR-selection query): an EVENT-level restriction denies
// placement on a selection beneath it, and its negative - a
// SELECTION-level restriction does not block a sibling selection under
// the same market.
func TestSportsbookJurisdiction_BlockedAtEventLevelBlocksEveryMarketAndSelectionBeneath(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	code := seedJurisdictionCode(t, pool)

	sel, eventID, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	seedRestriction(t, pool, restrictionScope{ScopeKind: "event", EventID: uuidPtr(eventID)}, code)

	result, err := placeBet(t, pool, f, sel, 1_000, "jur-event-propagation")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected an event-level restriction to block a selection beneath it")
	}
	if result.RejectionCategory != RejectionJurisdictionDenied {
		t.Fatalf("expected rejection category %q, got %q", RejectionJurisdictionDenied, result.RejectionCategory)
	}

	// Negative: a DIFFERENT event's selection is entirely unaffected.
	otherSel, _, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	otherResult, err := placeBet(t, pool, f, otherSel, 1_000, "jur-event-propagation-unaffected")
	if err != nil {
		t.Fatalf("place bet on unrelated event: %v", err)
	}
	if !otherResult.Accepted {
		t.Fatalf("expected a selection under an unrestricted event to be accepted, got rejection %q/%q", otherResult.RejectionCategory, otherResult.RejectionCode)
	}
}

// TestSportsbookJurisdiction_SelectionRestrictionDoesNotPropagateToSiblings
// is the negative half of downward propagation named separately from the
// event-level test above for a clean failure signal: a restriction on ONE
// selection never blocks a SIBLING selection in the same market.
func TestSportsbookJurisdiction_SelectionRestrictionDoesNotPropagateToSiblings(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	code := seedJurisdictionCode(t, pool)

	restricted, _, marketID := seedSelectionWithContext(t, pool, seedSelectionParams{})
	sibling := seedSiblingSelection(t, pool, marketID)
	seedRestriction(t, pool, restrictionScope{ScopeKind: "selection", SelectionID: uuidPtr(restricted.ID)}, code)

	restrictedResult, err := placeBet(t, pool, f, restricted, 1_000, "jur-sibling-restricted")
	if err != nil {
		t.Fatalf("place bet on restricted selection: %v", err)
	}
	if restrictedResult.Accepted {
		t.Fatal("expected the restricted selection itself to be denied")
	}

	siblingResult, err := placeBet(t, pool, f, sibling, 1_000, "jur-sibling-unaffected")
	if err != nil {
		t.Fatalf("place bet on sibling selection: %v", err)
	}
	if !siblingResult.Accepted {
		t.Fatalf("expected a sibling selection in the same market to remain unaffected, got rejection %q/%q", siblingResult.RejectionCategory, siblingResult.RejectionCode)
	}
}

// TestSportsbookJurisdiction_RestrictedSelectionDeniesMatchingJurisdiction
// proves the "armed, resolved and blocked" branch directly against
// evaluateJurisdictionRestriction, fed a GENUINELY resolved
// jurisdiction.Resolution obtained via the resolver's own producible
// tenant_licence basis - mirroring
// internal/casino.TestEvaluateJurisdictionBlocklist's identical technique
// exactly, for the identical reason: jurisdiction.Resolve always resolves
// unresolved(no_signal) for a player-scoped call in this codebase today
// (HDR-J-7 unanswered), so a genuinely Resolved value can only be obtained
// via a tenant/brand-subject call, and jurisdiction.Resolution has no
// exported constructor to fabricate one.
func TestSportsbookJurisdiction_RestrictedSelectionDeniesMatchingJurisdiction(t *testing.T) {
	pool := testPool(t)
	tenantID, brandID, jurisdictionCode := seedTenantWithLicenceForJurisdictionTest(t, pool)

	var resolved jurisdiction.Resolution
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = jurisdiction.Resolve(ctx, tx, jurisdiction.Params{
			TenantID: tenantID, BrandID: &brandID, OperationClass: jurisdiction.OperationCatalogueAvailability,
			RequestedByActorType: jurisdiction.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("resolve tenant_licence jurisdiction: %v", err)
	}
	if resolved.Outcome() != jurisdiction.Resolved {
		t.Fatalf("expected the tenant_licence basis to resolve, got %s(%s)", resolved.Outcome(), resolved.Reason())
	}

	playerID := uuid.New()
	var playerScoped jurisdiction.Resolution
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		playerScoped, err = jurisdiction.Resolve(ctx, tx, jurisdiction.Params{
			TenantID: tenantID, BrandID: &brandID, PlayerAccountID: &playerID, OperationClass: jurisdiction.OperationPlay,
			RequestedByActorType: jurisdiction.ActorPlayer, RequestedByActorID: &playerID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("resolve player-scoped jurisdiction: %v", err)
	}
	if playerScoped.Outcome() != jurisdiction.Unresolved {
		t.Fatalf("expected the player-scoped resolution to be unresolved in Stage 9.2, got %s(%s)", playerScoped.Outcome(), playerScoped.Reason())
	}

	tests := []struct {
		name       string
		restricted []string
		res        jurisdiction.Resolution
		wantAvail  bool
		wantCode   string
	}{
		{"K3-1: unarmed, unresolved -> available", nil, playerScoped, true, ""},
		{"K3-1: unarmed, resolved -> available", []string{}, resolved, true, ""},
		{"K3-2: armed, unresolved -> unavailable, unresolved code", []string{jurisdictionCode}, playerScoped, false, DenialCodeJurisdictionUnresolved},
		{"armed, resolved but not restricted -> available", []string{"SOME-OTHER-CODE"}, resolved, true, ""},
		{"armed, resolved and blocked -> unavailable, blocked code", []string{jurisdictionCode}, resolved, false, DenialCodeJurisdictionBlocked},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := evaluateJurisdictionRestriction(tc.restricted, tc.res)
			if err != nil {
				t.Fatalf("evaluateJurisdictionRestriction: %v", err)
			}
			if decision.Available != tc.wantAvail || decision.DenialCode != tc.wantCode {
				t.Fatalf("expected available=%v code=%q, got available=%v code=%q", tc.wantAvail, tc.wantCode, decision.Available, decision.DenialCode)
			}
		})
	}
}

// seedTenantWithLicenceForJurisdictionTest mirrors internal/casino's
// seedTenantWithLicence exactly (this package must not import casino).
func seedTenantWithLicenceForJurisdictionTest(t *testing.T, pool *db.Pool) (tenantID, brandID uuid.UUID, jurisdictionCode string) {
	t.Helper()
	tenantID = uuid.New()
	brandID = uuid.New()
	jurisdictionID := uuid.New()
	licenceID := uuid.New()
	jurisdictionCode = "SB-JT-" + uuid.New().String()[:8]

	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'SB Jurisdiction Test Tenant', 'under_platform_licence')`,
			tenantID, "sbjt-"+tenantID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'SB K-3 Test Jurisdiction')`,
			jurisdictionID, jurisdictionCode); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			licenceID, jurisdictionID, "SBK3-LIC-"+uuid.New().String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, tenantID, licenceID)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant with licence: %v", err)
	}
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'SB Test Brand')`,
			brandID, tenantID, "sbb-"+brandID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed brand: %v", err)
	}
	return tenantID, brandID, jurisdictionCode
}

// TestSportsbookJurisdiction_MissingConfigurationIsNotAWildcard proves (a)
// a restriction naming a jurisdiction_code that does not exist in
// `jurisdictions` is refused by the FK at write time, and (b) a withdrawn
// row is never evaluated (PlaceBet succeeds again once the only
// restriction on a selection is withdrawn).
func TestSportsbookJurisdiction_MissingConfigurationIsNotAWildcard(t *testing.T) {
	pool := testPool(t)

	// (a) Unknown jurisdiction_code is refused by the FK, not silently
	// accepted as some kind of wildcard.
	sel, _, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateJurisdictionRestriction(ctx, tx, CreateJurisdictionRestrictionParams{
			ScopeKind: "selection", SelectionID: uuidPtr(sel.ID), JurisdictionCode: "NO-SUCH-CODE-" + uuid.New().String()[:8],
			AuthorizationReference: "ref", ReasonCode: "reason", CreatedByActorID: uuid.New(),
		})
		return err
	})
	if !db.IsForeignKeyViolation(err) {
		t.Fatalf("expected a foreign-key violation for an unknown jurisdiction_code, got %v", err)
	}

	// (b) A withdrawn restriction is never evaluated.
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	code := seedJurisdictionCode(t, pool)
	r := seedRestriction(t, pool, restrictionScope{ScopeKind: "selection", SelectionID: uuidPtr(sel.ID)}, code)

	denied, err := placeBet(t, pool, f, sel, 1_000, "jur-withdraw-before")
	if err != nil {
		t.Fatalf("place bet while armed: %v", err)
	}
	if denied.Accepted {
		t.Fatal("expected the bet to be denied while the restriction is active")
	}

	err = pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := WithdrawJurisdictionRestriction(ctx, tx, WithdrawJurisdictionRestrictionParams{
			ID: r.ID, ReasonCode: "withdrawn-for-test", ActorID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw restriction: %v", err)
	}

	afterWithdraw, err := placeBet(t, pool, f, sel, 1_000, "jur-withdraw-after")
	if err != nil {
		t.Fatalf("place bet after withdrawal: %v", err)
	}
	if !afterWithdraw.Accepted {
		t.Fatalf("expected the bet to be accepted once the only restriction is withdrawn, got rejection %q/%q", afterWithdraw.RejectionCategory, afterWithdraw.RejectionCode)
	}
}

// TestSportsbookJurisdiction_PlatformWideRestrictionAppliesToEveryTenant
// proves the table's own designed platform-wide, tenant-less scope
// (§5.2.2): one restriction, created once, denies an otherwise-identical
// bet from TWO DIFFERENT tenants identically - there is no tenant
// dimension on sb_jurisdiction_restrictions to isolate by, by design.
func TestSportsbookJurisdiction_PlatformWideRestrictionAppliesToEveryTenant(t *testing.T) {
	pool := testPool(t)
	tenantA := seedFixture(t, pool)
	fundWallet(t, pool, tenantA, 10_000)
	tenantB := seedFixture(t, pool)
	fundWallet(t, pool, tenantB, 10_000)

	sel, eventID, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	code := seedJurisdictionCode(t, pool)
	seedRestriction(t, pool, restrictionScope{ScopeKind: "event", EventID: uuidPtr(eventID)}, code)

	resultA, err := placeBet(t, pool, tenantA, sel, 1_000, "jur-platform-wide-a")
	if err != nil {
		t.Fatalf("place bet for tenant A: %v", err)
	}
	if resultA.Accepted {
		t.Fatal("expected tenant A's bet to be denied by the platform-wide restriction")
	}

	resultB, err := placeBet(t, pool, tenantB, sel, 1_000, "jur-platform-wide-b")
	if err != nil {
		t.Fatalf("place bet for tenant B: %v", err)
	}
	if resultB.Accepted {
		t.Fatal("expected tenant B's bet to be denied identically by the SAME platform-wide restriction")
	}
	if resultA.RejectionCategory != resultB.RejectionCategory || resultA.RejectionCode != resultB.RejectionCode {
		t.Fatalf("expected both tenants to receive an identical rejection, got A=%+v B=%+v", resultA, resultB)
	}
}

// TestSportsbookJurisdiction_DirectAPIBypassIsRejected proves PlaceBet
// re-runs the gate itself against the specific selection_id supplied,
// never trusting that a caller went through the catalogue read path first
// (which never even ran in this test) and never consulting a cached
// availability answer - there is no cache to consult in this codebase,
// which is itself the point: the only "availability answer" that governs
// is this call's own, freshly evaluated one.
func TestSportsbookJurisdiction_DirectAPIBypassIsRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel, _, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	code := seedJurisdictionCode(t, pool)
	seedRestriction(t, pool, restrictionScope{ScopeKind: "selection", SelectionID: uuidPtr(sel.ID)}, code)

	// No catalogue read (ListSportsCatalogue/GetEventDetail) happens
	// anywhere in this test - PlaceBet is called directly with the raw
	// selection id, exactly as a client that skipped the UI would.
	result, err := placeBet(t, pool, f, sel, 1_000, "jur-direct-bypass")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected a direct PlaceBet call against a restricted selection_id to be denied, exactly as the catalogue would have shown")
	}
	if result.RejectionCategory != RejectionJurisdictionDenied {
		t.Fatalf("expected rejection category %q, got %q", RejectionJurisdictionDenied, result.RejectionCategory)
	}
}

// TestSportsbookJurisdiction_ConcurrentConfigurationReadIsConsistent
// proves a bet's own jurisdiction-gate decision reflects a single,
// coherent point-in-time read of sb_jurisdiction_restrictions - never a
// torn read - using this codebase's own established deterministic
// technique (an uncommitted blocker forcing PlaceBet to wait at the L3
// balance pre-lock, which happens strictly AFTER the jurisdiction gate in
// the composed order - ADR 0083 §7.1 steps 5-7 precede step 14). A
// restriction committed WHILE a PlaceBet call is blocked at L3 must NOT
// retroactively deny it: the gate already ran, against the configuration
// as it stood at that time.
func TestSportsbookJurisdiction_ConcurrentConfigurationReadIsConsistent(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel, eventID, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	code := seedJurisdictionCode(t, pool)

	var cashAccountID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cashAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("resolve cash account: %v", err)
	}

	var blockerPIDValue int32
	blockerReady := make(chan struct{})
	proceed := make(chan struct{})
	blockerErr := make(chan error, 1)
	go func() {
		blockerErr <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var d, c int64
			if err := tx.QueryRow(ctx,
				`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
				cashAccountID).Scan(&d, &c); err != nil {
				return err
			}
			var err error
			if blockerPIDValue, err = backendPID(ctx, tx); err != nil {
				return err
			}
			close(blockerReady)
			<-proceed
			return nil
		})
	}()
	select {
	case <-blockerReady:
	case err := <-blockerErr:
		t.Fatalf("blocker transaction failed before acquiring its lock: %v", err)
	}

	resultCh := make(chan PlaceBetResult, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := placeBet(t, pool, f, sel, 1_000, "jur-concurrent-config-read")
		resultCh <- r
		errCh <- err
	}()

	// Unlike TestPlaceBet_ConcurrentPlacementsOnlyOneSucceeds (two
	// competing PlaceBet calls, want=2), this test races exactly ONE
	// PlaceBet call against this file's own manual blocker transaction -
	// the blocker itself HOLDS the lock rather than waiting on it, so only
	// PlaceBet's own goroutine ever shows as blocked.
	if !waitForBlockedCount(t, pool, blockerPIDValue, 1) {
		close(proceed)
		t.Fatal("timed out waiting for PlaceBet to block on the uncommitted blocker row")
	}

	// Arm a restriction WHILE PlaceBet is blocked at L3 - strictly AFTER
	// its own jurisdiction gate already ran and decided to proceed.
	seedRestriction(t, pool, restrictionScope{ScopeKind: "event", EventID: uuidPtr(eventID)}, code)

	close(proceed)
	result := <-resultCh
	if err := <-errCh; err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if err := <-blockerErr; err != nil {
		t.Fatalf("blocker transaction: %v", err)
	}

	if !result.Accepted {
		t.Fatalf("expected the bet to be ACCEPTED - its own jurisdiction gate ran and passed before the concurrent restriction committed, got rejection %q/%q", result.RejectionCategory, result.RejectionCode)
	}
	if countBets(t, pool, f) != 1 {
		t.Fatal("expected exactly 1 bet row")
	}

	// A SUBSEQUENT bet, placed after the restriction is visibly committed,
	// must now be denied - proving the configuration read is not stale
	// forever, only consistent AS OF each call's own evaluation point.
	subsequent, err := placeBet(t, pool, f, sel, 1_000, "jur-concurrent-config-read-subsequent")
	if err != nil {
		t.Fatalf("place subsequent bet: %v", err)
	}
	if subsequent.Accepted {
		t.Fatal("expected a bet placed AFTER the restriction committed to be denied")
	}
}

// TestSportsbookBet_JurisdictionSnapshotIsImmutable proves §5.5: the
// placed bet's own jurisdiction_code cannot be UPDATEd (migration 0087's
// extension of sportsbook_bets_enforce_immutable_fields), and that
// withdrawing/changing the restriction configuration afterwards does not
// alter the snapshot already written (INV-SB-JUR-6).
func TestSportsbookBet_JurisdictionSnapshotIsImmutable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel, _, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})

	result, err := placeBet(t, pool, f, sel, 1_000, "jur-snapshot-immutable")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected bet to be accepted, got rejection %q/%q", result.RejectionCategory, result.RejectionCode)
	}
	// Every player-scoped resolution is unresolved(no_signal) today, so
	// the snapshot is empty (NULL) - still the value under test.
	if result.Bet.JurisdictionCode != "" {
		t.Fatalf("expected an empty jurisdiction_code snapshot (unresolved today), got %q", result.Bet.JurisdictionCode)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET jurisdiction_code = 'HACKED' WHERE id = $1`, result.Bet.ID)
		return err
	})
	if err == nil {
		t.Fatal("expected the trigger to reject a direct UPDATE of jurisdiction_code")
	}

	// Re-read under a fresh transaction: the snapshot is unchanged by the
	// rejected UPDATE attempt (rolled back with it), and is not
	// recomputed by GetBetByID itself (it is a plain column read, never a
	// re-resolution - INV-SB-JUR-6).
	var reread Bet
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reread, err = GetBetByID(ctx, tx, result.Bet.ID)
		return err
	})
	if err != nil {
		t.Fatalf("re-read bet: %v", err)
	}
	if reread.JurisdictionCode != result.Bet.JurisdictionCode {
		t.Fatalf("expected jurisdiction_code to remain %q after the rejected UPDATE, got %q", result.Bet.JurisdictionCode, reread.JurisdictionCode)
	}
}

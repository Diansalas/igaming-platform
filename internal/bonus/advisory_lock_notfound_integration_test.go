//go:build integration

// Regression suite for the ErrNotFound contract of the ADR 0082 §3.3
// grant advisory-lock precondition.
//
// WHY THIS FILE EXISTS. AdvisoryLockGrant gained a precondition this
// stage: it resolves the grant's player_account_id (to take the class
// L0.2 player bonus-scope lock) before taking the class L0.3 grant lock.
// That made it the FIRST statement to observe "this grant id does not
// exist in this tenant" for every public entry point that starts with it
// (ActivateGrant, ConvertGrant, TerminateGrant,
// RecordWageringContribution, CheckAndCompleteGrant). Its first
// implementation returned a BARE fmt.Errorf for that case, which silently
// downgraded every "bad grant id" from the ErrNotFound the HTTP layer maps
// to 404 (errors.Is, internal/httpserver) into an opaque 500. Before the
// precondition existed, the id fell through to LockGrantForUpdate/
// scanGrant, which returns ErrNotFound.
//
// No test in the tree caught that: the lifecycle suite always addresses
// grants it seeded itself, and the HTTP suite had no unknown-grant-id
// case for these endpoints at all. These tests are that missing coverage,
// asserted at the sentinel level (errors.Is, never a string match) so the
// mapping cannot regress again without failing here first.
package bonus

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestAdvisoryLockGrant_UnknownOrCrossTenantGrantWrapsErrNotFound(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	grantID := seedGrantRowDirectly(t, pool, f, co, f.playerID, f.walletID)

	t.Run("grant id that exists in this tenant is lockable", func(t *testing.T) {
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return AdvisoryLockGrant(ctx, tx, f.tenantID, grantID)
		}); err != nil {
			t.Fatalf("a real grant must be lockable without error: %v", err)
		}
	})

	t.Run("unknown grant id is ErrNotFound, never an opaque error", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return AdvisoryLockGrant(ctx, tx, f.tenantID, uuid.New())
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("AdvisoryLockGrant must wrap ErrNotFound for an unknown grant id (the HTTP layer maps it to "+
				"404 via errors.Is; a bare error is a 500 for what is a client mistake), got: %v", err)
		}
	})

	t.Run("cross-tenant grant id is ErrNotFound, never leaked", func(t *testing.T) {
		otherTenant := uuid.New()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return AdvisoryLockGrant(ctx, tx, otherTenant, grantID)
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("AdvisoryLockGrant must wrap ErrNotFound for a grant that is not this tenant's - a "+
				"cross-tenant reference must be indistinguishable from a non-existent one, got: %v", err)
		}
	})
}

// TestGrantEntryPoints_UnknownGrantSurfacesErrNotFound walks every public
// entry point whose FIRST statement is AdvisoryLockGrant, so the sentinel
// contract is pinned at the API surface each HTTP handler actually calls,
// not only at the helper.
func TestGrantEntryPoints_UnknownGrantSurfacesErrNotFound(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	missing := uuid.New()

	cases := []struct {
		name string
		call func(ctx context.Context, tx pgx.Tx) error
	}{
		{"ActivateGrant", func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := ActivateGrant(ctx, tx, f.tenantID, missing, ActivateGrantParams{
				ActorType: ActorStaff, ActorID: f.staffID, Amount: big.NewInt(1000),
			})
			return err
		}},
		{"ConvertGrant", func(ctx context.Context, tx pgx.Tx) error {
			_, err := ConvertGrant(ctx, tx, f.tenantID, missing, nil, ConvertGrantParams{
				ActorType: ActorStaff, ActorID: f.staffID,
			})
			return err
		}},
		{"TerminateGrant", func(ctx context.Context, tx pgx.Tx) error {
			_, err := TerminateGrant(ctx, tx, f.tenantID, missing, TerminateGrantParams{
				Resolution: TerminalResolutionForfeited, ReasonCode: "test", ActorType: ActorStaff, ActorID: f.staffID,
				TriggerType: TriggerStaffAction,
			})
			return err
		}},
		{"CheckAndCompleteGrant", func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := CheckAndCompleteGrant(ctx, tx, f.tenantID, missing, big.NewInt(1))
			return err
		}},
		{"RecordWageringContribution", func(ctx context.Context, tx pgx.Tx) error {
			return RecordWageringContribution(ctx, tx, f.tenantID, missing, RecordWageringContributionParams{
				OfferVersionID: uuid.New(), LockLedgerTransactionID: uuid.New(), CorrelationID: uuid.New(),
				AssetCode: f.assetCode, StakedBonusAmount: big.NewInt(100), ContributionWeightBP: 10000,
				QualifyingScaled: big.NewInt(100),
			})
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return c.call(ctx, tx)
			})
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s must surface ErrNotFound for a grant id that does not exist in this tenant "+
					"(it is what every HTTP handler errors.Is-checks to return 404), got: %v", c.name, err)
			}
		})
	}
}

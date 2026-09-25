//go:build integration

package bonus

// Stage 10 F-7 remediation (ADR 0020 amendment 2026-09-25,
// docs/governance/stage-10-f7-ledger-replay-audit.md §3 sites #14-#17,
// §6.4 "bonus"): every bonus posting is state-gated under the grant lock,
// so a second invocation never reaches ledger.Post. These pin that a
// double-invoked activation, conversion and termination each yield
// exactly one posting. (Held-disposition resolve, site #16, is pinned by
// the second-resolution ErrHeldDispositionNotHeld assertion in
// TestHeldDisposition_ResolveReforfeit_WithFourEyesAndSEP1,
// lifecycle_integration_test.go.)

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func f7GrantPostings(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, txType string) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND correlation_id = $2 AND transaction_type = $3`,
		tenantID, grantID, txType).Scan(&n)
	return n, err
}

func TestF7Bonus_ActivateAndConvertTwiceOnePostingEach(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := forceIssueAndActivateGrantForTest(t, ctx, tx, newTestOfferGrant(f, co, "f7-activate"), ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
		if _, err := forceActivateGrantForTestErr(ctx, tx, f.tenantID, g.ID, ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem}); !errors.Is(err, ErrIllegalTransition) {
			return fmt.Errorf("second activation: want ErrIllegalTransition, got %v", err)
		}
		if n, err := f7GrantPostings(ctx, tx, f.tenantID, g.ID, "bonus_grant"); err != nil || n != 1 {
			return fmt.Errorf("bonus_grant postings = %d (%v), want 1", n, err)
		}

		completed, ok, err := CheckAndCompleteGrant(ctx, tx, f.tenantID, g.ID, nil)
		if err != nil || !ok {
			return fmt.Errorf("complete: %v / %v", err, ok)
		}
		if res := forceConvertGrantForTest(t, ctx, tx, f.tenantID, completed.ID, nil, ConvertGrantParams{ActorType: ActorSystem}); !res.Converted {
			return fmt.Errorf("first conversion blocked: %s", res.BlockedReason)
		}
		if _, err := ConvertGrant(ctx, tx, f.tenantID, g.ID, nil, ConvertGrantParams{ActorType: ActorSystem}); !errors.Is(err, ErrIllegalTransition) {
			return fmt.Errorf("second conversion: want ErrIllegalTransition, got %v", err)
		}
		if n, err := f7GrantPostings(ctx, tx, f.tenantID, g.ID, "bonus_conversion"); err != nil || n != 1 {
			return fmt.Errorf("bonus_conversion postings = %d (%v), want 1", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestF7Bonus_TerminateTwiceOnePosting(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := forceIssueAndActivateGrantForTest(t, ctx, tx, newTestOfferGrant(f, co, "f7-terminate"), ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
		params := TerminateGrantParams{Resolution: TerminalResolutionExpired, ReasonCode: "time_limit_elapsed", ActorType: ActorSystem, TriggerType: TriggerAutomatedRuleEvaluation}
		if _, err := TerminateGrant(ctx, tx, f.tenantID, g.ID, params); err != nil {
			return fmt.Errorf("first termination: %w", err)
		}
		if _, err := TerminateGrant(ctx, tx, f.tenantID, g.ID, params); !errors.Is(err, ErrIllegalTransition) {
			return fmt.Errorf("second termination: want ErrIllegalTransition, got %v", err)
		}
		if n, err := f7GrantPostings(ctx, tx, f.tenantID, g.ID, "bonus_forfeiture"); err != nil || n != 1 {
			return fmt.Errorf("bonus_forfeiture postings = %d (%v), want 1", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

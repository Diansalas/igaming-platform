//go:build integration

// Stage 3C hardening (directive item 5 and adversarial test 8.O):
// "withdrawal policy is evaluated with different assets/precisions".
// Proves ResolveApprovalPolicy (policy.go) - the configuration boundary
// that replaced Stage 3B's single, asset-blind threshold constant -
// never compares one asset's minor units against a policy meant for a
// different asset, in either the zero-config default fallback or an
// explicitly configured policy row.
package withdrawal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestResolveApprovalPolicy_DefaultFallbackFailsClosedForEveryAsset proves
// the zero-config default (used when no withdrawal_policies row exists)
// requires full approval scrutiny (ThresholdAmount == 0, so
// wr.Amount >= 0 is always true) for every asset, regardless of that
// asset's decimal_exponent.
//
// An earlier design derived a per-asset "1000 major units" default from
// decimal_exponent alone. Specialist review (ledger-finance) rejected
// that: decimal precision is not real-world value - "1000 major units"
// of BTC and "1000 major units" of EUR are not comparable amounts, and
// for BTC specifically that design would have raised the effective
// four-eyes bar to roughly 1000 BTC, weakening protection relative to
// even Stage 3B's flat constant. A genuinely value-equivalent default
// would need FX/market-price data, which this stage explicitly excludes
// (docs/architecture/withdrawal-policy-configuration.md §3) - so the
// fallback fails closed instead, identically for every asset, until a
// tenant configures a real, asset-appropriate withdrawal_policies row.
func TestResolveApprovalPolicy_DefaultFallbackFailsClosedForEveryAsset(t *testing.T) {
	pool := testPool(t)
	tenantID := uuid.New() // no withdrawal_policies row will ever match this tenant - every asset below resolves via the fallback.

	for _, assetCode := range []string{"EUR", "USD", "USDT", "BTC"} {
		var policy ApprovalPolicy
		err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			policy, err = ResolveApprovalPolicy(ctx, tx, tenantID, uuid.New(), nil, assetCode, time.Now())
			return err
		})
		if err != nil {
			t.Fatalf("%s: ResolveApprovalPolicy: %v", assetCode, err)
		}
		if policy.ThresholdAmount != 0 {
			t.Fatalf("%s: expected the fail-closed default threshold 0, got %d", assetCode, policy.ThresholdAmount)
		}
		if policy.RequiredApprovals != defaultRequiredApprovals {
			t.Fatalf("%s: expected default RequiredApprovals %d, got %d", assetCode, defaultRequiredApprovals, policy.RequiredApprovals)
		}
		if policy.RequireStepUp {
			t.Fatalf("%s: default policy must never require step-up (MFA does not exist yet)", assetCode)
		}
	}
}

// TestResolveApprovalPolicy_ConfiguredPolicyNeverBleedsAcrossAssets is
// the direct proof-of-fix for Stage 3B's bug: two withdrawal_policies
// rows for the SAME tenant, one per asset, with very different
// thresholds. Resolving one asset must never return the other's row -
// the precise failure mode of comparing a withdrawal's raw minor-unit
// amount against a threshold configured for a different asset.
func TestResolveApprovalPolicy_ConfiguredPolicyNeverBleedsAcrossAssets(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 0) // a real tenant row: withdrawal_policies.tenant_id carries a FK.
	tenantID := f.tenantID

	mustSetWithdrawalPolicy(t, pool, tenantID, "EUR", 250_000, 2, time.Now().Add(-time.Hour))
	mustSetWithdrawalPolicy(t, pool, tenantID, "BTC", 5_000_000, 3, time.Now().Add(-time.Hour))

	var eurPolicy, btcPolicy, usdPolicy ApprovalPolicy
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if eurPolicy, err = ResolveApprovalPolicy(ctx, tx, tenantID, uuid.New(), nil, "EUR", time.Now()); err != nil {
			return err
		}
		if btcPolicy, err = ResolveApprovalPolicy(ctx, tx, tenantID, uuid.New(), nil, "BTC", time.Now()); err != nil {
			return err
		}
		// USD has no configured row for this tenant at all - must fall
		// back to ITS OWN default, never borrow EUR's or BTC's row just
		// because they share a tenant.
		usdPolicy, err = ResolveApprovalPolicy(ctx, tx, tenantID, uuid.New(), nil, "USD", time.Now())
		return err
	})
	if err != nil {
		t.Fatalf("resolve policies: %v", err)
	}

	if eurPolicy.ThresholdAmount != 250_000 || eurPolicy.RequiredApprovals != 2 {
		t.Fatalf("EUR policy bled or was lost: got %+v", eurPolicy)
	}
	if btcPolicy.ThresholdAmount != 5_000_000 || btcPolicy.RequiredApprovals != 3 {
		t.Fatalf("BTC policy bled or was lost: got %+v", btcPolicy)
	}
	if usdPolicy.ThresholdAmount != 0 || usdPolicy.RequiredApprovals != defaultRequiredApprovals {
		t.Fatalf("expected USD (unconfigured) to resolve its own default, got %+v - it must never inherit EUR's or BTC's configured row", usdPolicy)
	}
}

// TestResolveApprovalPolicy_UnknownAssetFailsClosed proves an asset code
// with neither a configured policy row nor an entry in the platform
// Asset registry is refused outright, never silently given a threshold
// of 0 (which would make every non-zero withdrawal in that "asset"
// require full approval - arguably safe, but ResolveApprovalPolicy
// should never guess at an asset's precision it has no record of).
func TestResolveApprovalPolicy_UnknownAssetFailsClosed(t *testing.T) {
	pool := testPool(t)
	tenantID := uuid.New()

	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ResolveApprovalPolicy(ctx, tx, tenantID, uuid.New(), nil, "NOTAREALASSET", time.Now())
		return err
	})
	if err == nil {
		t.Fatal("expected ResolveApprovalPolicy to fail for an asset absent from both withdrawal_policies and the asset registry")
	}
}

// TestApprove_RequireStepUpFailsClosedWhenAboveThreshold is directive
// item 6's proof: a tenant that configures RequireStepUp=true on a
// policy row, before MFA exists (ADR 0017 - preserved, not implemented
// here), must get a safe, loud ErrStepUpRequired for any decision at or
// above that policy's threshold - never a silently-ignored flag that
// approves as if a challenge had happened. A request BELOW threshold is
// unaffected: RequireStepUp only gates the above-threshold path, mirroring
// RequiredApprovals' own below/above-threshold split.
func TestApprove_RequireStepUpFailsClosedWhenAboveThreshold(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)

	mustSetWithdrawalPolicyStepUp(t, pool, f.tenantID, "EUR", 50_000, true)

	above := mustRequestWithdrawal(t, pool, f, 100_000, "wd-stepup-above")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, above.ID)
	})
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Approve(ctx, tx, above.ID, uuid.New(), false, nil)
		return err
	})
	if !errors.Is(err, ErrStepUpRequired) {
		t.Fatalf("expected ErrStepUpRequired for an above-threshold decision under a RequireStepUp policy, got %v", err)
	}
	// The refusal must not have recorded anything - a caller that
	// retries once step-up is actually implemented must not find a
	// phantom decision already on file.
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1`, above.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected zero recorded approvals after a step-up refusal, got %d", count)
		}
		return nil
	})

	below := mustRequestWithdrawal(t, pool, f, 10_000, "wd-stepup-below")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, below.ID)
	})
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, below.ID, uuid.New(), false, nil)
		if err != nil {
			return err
		}
		if !approved {
			t.Fatal("expected a below-threshold decision to succeed even under a RequireStepUp policy - step-up only gates the above-threshold path")
		}
		return nil
	})
}

// mustSetWithdrawalPolicyStepUp is mustSetWithdrawalPolicy's sibling for
// the one field that helper deliberately omits (require_step_up always
// false there, matching every other test's expectations).
func mustSetWithdrawalPolicyStepUp(t *testing.T, pool *db.Pool, tenantID uuid.UUID, assetCode string, thresholdAmount int64, requireStepUp bool) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, require_step_up, effective_from)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantID, assetCode, thresholdAmount, defaultRequiredApprovals, requireStepUp, time.Now().Add(-time.Hour),
		)
		return err
	})
	if err != nil {
		t.Fatalf("set withdrawal policy (step-up): %v", err)
	}
}

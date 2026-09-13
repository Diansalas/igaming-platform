// Stage 3C hardening (directive item 5, "withdrawal policy
// configuration"): Stage 3B's four-eyes rule compared a withdrawal's raw
// minor-unit amount against a single process-wide constant
// (httpserver.defaultWithdrawalApprovalThreshold, since removed),
// applied identically to every tenant, brand, and asset. That silently
// conflated two different assets' minor units - the same raw number
// means wildly different real value for EUR (2 decimal places) and BTC
// (8) - which is exactly the imprecision CLAUDE.md's "never use
// floating-point... per-currency exponent" ledger rule exists to
// prevent, just one layer up in the approval policy instead of the
// ledger itself.
//
// This file is the configuration boundary that replaces it:
// ResolveApprovalPolicy reads a tenant/brand/jurisdiction/asset/
// effective-time-scoped withdrawal_policies row (migration 0032) when
// one exists, and falls back to an explicit, documented test/development
// default - never a silently invented production number - when none
// does. See docs/architecture/withdrawal-policy-configuration.md for the
// full design and open decisions.
package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ApprovalPolicy is the resolved set of approval rules in force for one
// withdrawal decision, at the moment it is made. Approve/Reject never
// invent these values themselves - they always come from
// ResolveApprovalPolicy.
type ApprovalPolicy struct {
	// ThresholdAmount is in the SAME asset's minor units as the request
	// being decided - by construction (ResolveApprovalPolicy always
	// resolves one asset_code at a time), never compared across assets.
	ThresholdAmount int64
	// RequiredApprovals is how many DISTINCT human approvals a request
	// at or above ThresholdAmount needs. A request below ThresholdAmount
	// always needs exactly one - this package's existing two-tier model
	// (Stage 3B), generalized from a hardcoded "2" to a configured value.
	RequiredApprovals int
	// RequireStepUp is directive item 6's MFA/step-up enforcement
	// boundary - see ErrStepUpRequired's doc comment in withdrawal.go.
	RequireStepUp bool
}

// defaultApprovalThresholdMajorUnits is the Stage 3C placeholder
// approval-threshold policy, expressed in MAJOR units of whatever asset
// it is applied to - so it represents "the same real-world magnitude"
// for every asset, not "the same raw minor-unit number" (Stage 3B's
// bug). This is explicitly NOT a final production threshold: CLAUDE.md's
// "no fake completion" rule means this must be labeled, not silently
// treated as business policy. It exists only so withdrawals can be
// exercised and tested before any tenant configures a real
// withdrawal_policies row. Business/risk/compliance own the real number
// (docs/architecture/withdrawal-policy-configuration.md's open decision
// list) - this stage does not invent it.
const defaultApprovalThresholdMajorUnits = 1000

// defaultRequiredApprovals mirrors Stage 3B's existing "two distinct
// human approvers above threshold" rule - carried forward as the
// fallback's value, not re-litigated here.
const defaultRequiredApprovals = 2

// ResolveApprovalPolicy resolves the ApprovalPolicy in force for a
// (tenantID, brandID, assetCode) withdrawal decision at time at, inside
// tx (opened via db.Pool.WithTenant(ctx, tenantID, ...) by the caller -
// every other tenant-scoped read in this codebase follows the same
// pattern, and withdrawal_policies carries the same tenant_isolation RLS
// policy as every other tenant-owned financial configuration table).
//
// jurisdictionCode is accepted for forward compatibility (directive item
// 5's jurisdiction dimension) but every caller in this codebase passes
// nil today: no per-withdrawal jurisdiction assignment exists yet (a
// player_account belongs to a brand; a tenant, not a brand, is what
// resolves to a set of jurisdictions via tenant_jurisdiction_configs,
// and a tenant can serve several at once - see
// docs/architecture/15-jurisdiction-and-licensing-model.md). A
// jurisdiction-scoped withdrawal_policies row can be written today and
// will simply never match until that assignment exists - a documented
// open decision (docs/architecture/withdrawal-policy-configuration.md),
// not a bug in this function.
//
// Selection prefers the most specific matching row - an exact brand_id
// match over a tenant-wide NULL, then an exact jurisdiction_code match
// over a tenant-wide NULL - then the most recently effective row (and,
// among those, the highest policy_version) among ties. When no row
// matches at all, the caller gets defaultApprovalPolicy.
func ResolveApprovalPolicy(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, jurisdictionCode *string, assetCode string, at time.Time) (ApprovalPolicy, error) {
	if assetCode == "" {
		return ApprovalPolicy{}, fmt.Errorf("%w: asset code is required to resolve a withdrawal policy", ErrInvalidInput)
	}

	var (
		threshold         int64
		requiredApprovals int16
		requireStepUp     bool
	)
	err := tx.QueryRow(ctx,
		`SELECT approval_threshold_minor_units, required_approvals, require_step_up
		 FROM withdrawal_policies
		 WHERE tenant_id = $1 AND asset_code = $2 AND effective_from <= $3
		   AND (brand_id IS NULL OR brand_id = $4)
		   AND (jurisdiction_code IS NULL OR jurisdiction_code = $5)
		 ORDER BY (brand_id IS NOT NULL) DESC, (jurisdiction_code IS NOT NULL) DESC,
		          effective_from DESC, policy_version DESC
		 LIMIT 1`,
		tenantID, assetCode, at, brandID, jurisdictionCode,
	).Scan(&threshold, &requiredApprovals, &requireStepUp)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultApprovalPolicy(ctx, tx, assetCode)
	}
	if err != nil {
		return ApprovalPolicy{}, fmt.Errorf("withdrawal: resolve approval policy: %w", err)
	}
	return ApprovalPolicy{ThresholdAmount: threshold, RequiredApprovals: int(requiredApprovals), RequireStepUp: requireStepUp}, nil
}

// defaultApprovalPolicy is the Stage 3C fallback used only when no
// withdrawal_policies row matches. It derives an asset-precision-aware
// threshold from the asset's own decimal_exponent (the same Asset
// registry the ledger itself reads - internal/ledger's own "never assume
// an exponent" rule applies here too), rather than reusing Stage 3B's
// flat, asset-blind constant: "1000 major units of THIS asset", never
// "100000 of whatever minor unit happens to be in play".
func defaultApprovalPolicy(ctx context.Context, tx pgx.Tx, assetCode string) (ApprovalPolicy, error) {
	var exponent int
	if err := tx.QueryRow(ctx, `SELECT decimal_exponent FROM assets WHERE code = $1`, assetCode).Scan(&exponent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApprovalPolicy{}, fmt.Errorf("withdrawal: resolve approval policy: unknown asset %q", assetCode)
		}
		return ApprovalPolicy{}, fmt.Errorf("withdrawal: read asset registry for default policy: %w", err)
	}
	return ApprovalPolicy{
		ThresholdAmount:   defaultThresholdMinorUnits(exponent),
		RequiredApprovals: defaultRequiredApprovals,
		// MFA does not exist yet (ADR 0017) - the fallback must never
		// default to a policy this platform cannot actually enforce.
		RequireStepUp: false,
	}, nil
}

// defaultThresholdMinorUnits computes defaultApprovalThresholdMajorUnits
// in exponent's minor units, failing toward STRICTER approval (threshold
// 0, so every non-zero withdrawal needs the full RequiredApprovals)
// rather than toward a silently wrong huge number, if the multiplication
// would overflow int64. assets.decimal_exponent allows up to 18
// (migration 0003's own CHECK constraint), which for a 1000-major-unit
// default CAN overflow int64 (1000 * 10^18 > math.MaxInt64) - no asset
// actually seeded in this platform has an exponent that large today, but
// this function must not silently produce a wrong answer if one ever is.
func defaultThresholdMinorUnits(exponent int) int64 {
	threshold := int64(defaultApprovalThresholdMajorUnits)
	for i := 0; i < exponent; i++ {
		next := threshold * 10
		if next/10 != threshold {
			return 0
		}
		threshold = next
	}
	return threshold
}

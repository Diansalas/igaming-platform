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
// among those, the highest policy_version, then the most recently
// created row as a final deterministic tiebreaker - specialist review
// finding: without one, two rows tying on every other key make which
// policy "wins" unspecified across query re-executions, which is not
// acceptable for a control that gates four-eyes approval). When no row
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
		          effective_from DESC, policy_version DESC, created_at DESC, id DESC
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
// withdrawal_policies row matches.
//
// It was originally designed to derive a per-asset threshold from the
// asset's own decimal_exponent ("1000 major units of THIS asset").
// Specialist review (ledger-finance) rejected that design: decimal
// precision and real-world VALUE are different things a decimal exponent
// says nothing about market price, so "1000 major units" of BTC and
// "1000 major units" of EUR do not represent comparable real value -
// for BTC specifically it would have raised the effective four-eyes
// bar to roughly 1000 BTC, dramatically WEAKENING protection relative to
// Stage 3B's flat constant. Building a genuinely value-equivalent
// default would require FX/market-price data, which is explicitly out
// of this stage's scope (directive: no FX/conversion accounting).
//
// The fallback therefore fails CLOSED instead: ThresholdAmount is always
// 0, so every non-zero withdrawal in an asset with no configured policy
// requires the full RequiredApprovals - the maximally conservative
// behavior, and the same direction the overflow guard on a
// value-scaled default already failed toward. A tenant that wants a
// lighter-touch, asset-appropriate threshold must configure one
// explicitly via a withdrawal_policies row.
func defaultApprovalPolicy(ctx context.Context, tx pgx.Tx, assetCode string) (ApprovalPolicy, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE code = $1)`, assetCode).Scan(&exists); err != nil {
		return ApprovalPolicy{}, fmt.Errorf("withdrawal: read asset registry for default policy: %w", err)
	}
	if !exists {
		return ApprovalPolicy{}, fmt.Errorf("withdrawal: resolve approval policy: unknown asset %q", assetCode)
	}
	return ApprovalPolicy{
		ThresholdAmount:   0,
		RequiredApprovals: defaultRequiredApprovals,
		// MFA does not exist yet (ADR 0017) - the fallback must never
		// default to a policy this platform cannot actually enforce.
		RequireStepUp: false,
	}, nil
}

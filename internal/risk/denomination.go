package risk

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrUnknownAsset is returned when a request's AssetCode is not a row in
// the platform `assets` registry. Fail-closed: an amount-shaped rule can
// only be compared in minor units of a REAL registered asset, and an
// unknown asset means the request's own denomination is unknowable, not
// that there is no limit.
var ErrUnknownAsset = errors.New("risk: asset is not registered in the asset registry")

// ErrMissingThresholdDenomination is returned when an asset-agnostic
// amount-shaped rule does not declare the exponent its threshold's minor
// units are expressed in - i.e. a risk_rules row created BEFORE migration
// 0046, whose CHECK is deliberately NOT VALID so such rows are
// grandfathered as data rather than guessed at (ADR 0031 §35). The rule
// is refused, never compared against an assumed exponent.
var ErrMissingThresholdDenomination = errors.New("risk: an asset-agnostic amount rule does not declare its threshold denomination")

// ErrThresholdDenominationMismatch is returned when an asset-agnostic
// amount rule's declared threshold exponent differs from the exponent of
// the asset the request is actually denominated in (ADR 0031 §35,
// closing §32(d)).
//
// The evaluator deliberately does NOT rescale the threshold by
// 10^(reqExp - ruleExp): that would assert "N major units of asset A" is
// a comparable limit to "N major units of asset B", which is the exact
// design ledger-finance already rejected in this repository for
// withdrawal.defaultApprovalPolicy ("decimal precision and real-world
// VALUE are different things a decimal exponent says nothing about"). A
// value-equivalent normalization would need FX/market-price data, which
// Risk must never consult on an evaluation path. Failing closed keeps the
// rule BINDING on a newly authorized asset (so the asset is not silently
// uncapped) while refusing to invent a cap for it.
var ErrThresholdDenominationMismatch = errors.New("risk: rule threshold denomination does not match the request asset's exponent")

// assetExponents resolves and memoizes assets.decimal_exponent for the
// ONE asset a RiskRequest is denominated in.
//
// The `assets` table is the single source of exponent truth (CLAUDE.md:
// "per-currency exponent looked up from the Asset registry") - this
// package never hard-codes a decimal count, never derives one from a
// currency code, and never caches one across transactions. When
// internal/assetregistry lands (being built in parallel by another
// specialist this stage) this resolver is the one place that moves behind
// it; nothing else in internal/risk reads an exponent.
//
// Memoized per Evaluate call: the request's asset is fixed, so at most
// one query runs no matter how many amount-shaped rules match, and the
// value cannot change mid-evaluation.
type assetExponents struct {
	tx       pgx.Tx
	resolved bool
	exponent int16
	err      error
}

func (a *assetExponents) forRequest(ctx context.Context, assetCode string) (int16, error) {
	if a.resolved {
		return a.exponent, a.err
	}
	a.resolved = true
	var exp int16
	err := a.tx.QueryRow(ctx, `SELECT decimal_exponent FROM assets WHERE code = $1`, assetCode).Scan(&exp)
	if errors.Is(err, pgx.ErrNoRows) {
		a.err = fmt.Errorf("%w: %q", ErrUnknownAsset, assetCode)
		return 0, a.err
	}
	if err != nil {
		a.err = fmt.Errorf("risk: resolve asset exponent: %w", err)
		return 0, a.err
	}
	a.exponent = exp
	return exp, nil
}

// thresholdExponent returns the exponent r's Threshold is denominated in,
// verified against the exponent the request is actually denominated in.
//
// Two legitimate shapes, exactly one of which applies (migration 0046's
// CHECK enforces this for every new row):
//
//   - asset-scoped rule: the SCOPE is the denomination. matches() already
//     guarantees r.AssetCode == req.AssetCode for a rule that reached
//     here, so the request's own exponent IS the rule's; a
//     ThresholdExponent additionally present on such a row is verified
//     for equality rather than trusted (defense in depth against a row
//     written by direct SQL before the CHECK existed).
//   - asset-agnostic rule: ThresholdExponent is required, and must equal
//     the request asset's exponent - the rule binds every asset of that
//     exponent and fails closed for any other.
func (r Rule) thresholdExponent(reqExponent int16) (int16, error) {
	if r.AssetCode != "" {
		if r.ThresholdExponent != nil && *r.ThresholdExponent != reqExponent {
			return 0, fmt.Errorf("%w: rule %s is scoped to asset %s (exponent %d) but declares threshold exponent %d",
				ErrThresholdDenominationMismatch, r.ID, r.AssetCode, reqExponent, *r.ThresholdExponent)
		}
		return reqExponent, nil
	}
	if r.ThresholdExponent == nil {
		return 0, fmt.Errorf("%w: rule %s (limit_kind=%s) is asset-agnostic and pre-dates migration 0046",
			ErrMissingThresholdDenomination, r.ID, r.LimitKind)
	}
	if *r.ThresholdExponent != reqExponent {
		return 0, fmt.Errorf("%w: rule %s threshold is denominated at exponent %d, request asset has exponent %d",
			ErrThresholdDenominationMismatch, r.ID, *r.ThresholdExponent, reqExponent)
	}
	return reqExponent, nil
}

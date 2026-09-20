package operatingmarket

// This file implements the ONE sanctioned pre-account, anonymous-path
// read of operating-market policy (ADR 0045 §7.4).

import (
	"context"
)

// operationCodeRegistration is a COMPILE-TIME CONSTANT, never a caller
// argument - IsRegistrationPermitted's entire safety property depends on
// this never becoming a parameter.
const operationCodeRegistration = "registration"

// IsRegistrationPermitted is the ONLY sanctioned pre-account, anonymous-
// path read of operating-market policy. It is a NARROW SERVER-CONTROLLED
// PROJECTION by construction, not by convention:
//
//   - Its return type is (bool, error). It is STRUCTURALLY INCAPABLE of
//     carrying an Outcome, a blocking scope, a version id, a reason code,
//     or a licence id. There is nothing for a handler to leak.
//   - It internally calls ResolveOperatingCountryPolicy with
//     OperationCode = "registration" and ProductCode = nil - a
//     COMPILE-TIME CONSTANT, never a caller argument - and collapses
//     EVERY non-permitted outcome to false.
//   - It FAILS CLOSED on every error: any non-nil error returns
//     (false, err), and the caller must treat a non-nil error as "not
//     permitted". It never returns (true, err).
//   - It requires a TENANT-SCOPED, NON-PLAYER-SCOPED transaction. There is
//     no player_account_id at registration time by definition, and
//     operating_country_policies has no player-read policy - a
//     player-scoped connection would read zero rows and produce a
//     misleading false. The scope assertion (resolve()'s own
//     assertTenantScope) turns that into a diagnosable ErrTransactionScope.
//   - It NEVER reads, writes, or touches any player evidence. It cannot:
//     the package cannot import internal/identity or internal/kyc
//     (INV-M-1).
//
// It is NOT a raw policy-row read and MUST NOT be replaced by one.
//
// The deliberate consequence: registration eligibility is evaluated at
// the tenant/brand grain plus the `registration` operation rung, and an
// unconfigured tenant yields false. That is correct - ruling 5 (a tenant
// must explicitly opt in) applies to registration exactly as to
// everything else.
func IsRegistrationPermitted(ctx context.Context, q ReadOnlyQuerier, p RegistrationQuery) (bool, error) {
	result, err := ResolveOperatingCountryPolicy(ctx, q, Query{
		TenantID:      p.TenantID,
		BrandID:       p.BrandID,
		CountryCode:   p.CountryCode,
		OperationCode: operationCodeRegistration,
		ProductCode:   nil,
		AsOf:          p.AsOf,
	})
	if err != nil {
		return false, err
	}
	return result.Permitted(), nil
}

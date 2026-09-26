package providerkind

import (
	"fmt"
	"sort"
	"strings"
)

// Registration is one component a binary wires at startup - a provider
// adapter, a scanner, a storage backend, a webhook credential resolver, or
// a reconciliation statement source. Domain and Name are for the error
// message and audit trail only; RefuseSyntheticInProduction's actual
// decision is made purely from Component's dynamic type via the Synthetic/
// ProductionEligible marker interfaces above.
type Registration struct {
	// Domain groups related registrations for the error message (e.g.
	// "payments", "casino", "kyc", "sportsbook", "identity_resolution",
	// "email").
	Domain string
	// Name identifies the specific registration within Domain (e.g.
	// "provider", "webhook_resolver", "catalogue_source",
	// "malware_scanner").
	Name string
	// Component is the actual wired value. A nil Component (a domain that
	// registered nothing, e.g. KYC when test support is off) is silently
	// skipped - there is nothing to be synthetic or eligible about.
	Component any
}

// RefuseSyntheticInProduction is the pure MOCK-ADAPTER-PROD-1 startup
// guard (Stage 10.3, ADR 0085's Stage 10.3 amendment; security condition
// C13, ruling R8). It has no I/O, no globals, and touches no database -
// its ONLY inputs are environment (the caller's already-resolved
// GuardEnvironment() value - see internal/config.Config.GuardEnvironment's
// own doc comment for why a missing APP_ENV must already have been folded
// into "production" before it reaches here) and regs.
//
// When environment != "production" this always returns nil: the guard has
// no other configuration input, and no boolean anywhere in Config can make
// it refuse to start outside production (security condition C13's "a test
// runs the guard with every boolean in Config set to true, and again with
// every one set to false, and it still refuses in production" - the
// guard's own signature makes that trivially true, since it takes no
// Config at all, only the one string a caller must already have resolved).
//
// When environment == "production", every non-nil Component in regs MUST
// implement ProductionEligible and MUST NOT implement Synthetic. A
// component satisfying neither marker (an unmarked type - the default,
// safe state for anything new) is refused exactly like an explicitly
// Synthetic one - there is no third, permissive outcome. The only way past
// this function is to make Component itself production-eligible, never a
// flag; see ADR 0085's own "the guard's only configuration input is the
// environment" note.
//
// The returned error, when non-nil, names every offending registration so
// the operator sees the full list in one failure, not one crash per
// component.
func RefuseSyntheticInProduction(environment string, regs []Registration) error {
	if environment != "production" {
		return nil
	}

	var offending []string
	for _, r := range regs {
		if r.Component == nil {
			continue
		}
		_, synthetic := r.Component.(Synthetic)
		_, eligible := r.Component.(ProductionEligible)
		if synthetic || !eligible {
			offending = append(offending, fmt.Sprintf("%s/%s (%T)", r.Domain, r.Name, r.Component))
		}
	}
	if len(offending) == 0 {
		return nil
	}
	sort.Strings(offending)
	return fmt.Errorf(
		"providerkind: refusing to start with APP_ENV=production (or APP_ENV missing, which this guard treats "+
			"identically per security condition C13) - the following registered components are not production-"+
			"eligible: %s", strings.Join(offending, "; "))
}

// Package providerkind declares the two marker interfaces MOCK-ADAPTER-PROD-1
// uses to decide, at startup, whether a registered provider/scanner/
// storage/resolver/statement-source component may ever run in production
// (Stage 10.3, ADR 0085's Stage 10.3 amendment; docs/plans/stage-10.3-
// planning/01-provider-trust-analysis.md §3; security review C13, ruling
// R8). It is a deliberately dependency-free leaf package: every mock type
// across internal/* implements Synthetic (or a future real adapter
// implements ProductionEligible) purely by having a method with the right
// name and signature - Go's structural typing means NONE of those packages
// needs to import providerkind at all, so this package can sit underneath
// all of them with zero risk of an import cycle.
//
// # Why a positive marker, not a name-based deny-list
//
// Security review C13 (04-review-security.md §4, point 2) found the
// originally-proposed name-based scan ("every `type Mock\w+`") fails open:
// a synthetic double named `Fake…`, `Stub…`, `InMemory…` or `Dev…` without
// the marker would pass silently, and a mock embedding a real
// ProductionEligible type would inherit the positive marker it should
// never have. The required fix, adopted here, is the opposite polarity: a
// component needs to affirmatively claim ProductionEligible (and must NOT
// also claim Synthetic) before RefuseSyntheticInProduction lets it run in
// production. An unmarked new component - regardless of what it is named -
// is refused by default. Today no component in this codebase implements
// ProductionEligible at all, which is the honest, correct state: nothing
// has a real, vetted, production-grade implementation yet.
package providerkind

// Synthetic is implemented by every mock, fake, stub, or otherwise
// synthetic development/test double in this codebase - payments, KYC,
// casino and sportsbook mock adapters, the sportsbook mock catalogue
// source and mock settlement statement source, the mock malware scanner,
// mock document storage, the mock person resolver, the mock email
// provider, the mock geolocation provider, and the shared MOCK webhook
// credential resolver all implement it (see docs/plans/stage-10.3-
// planning/01-provider-trust-analysis.md §3's eight-mock inventory, plus
// the additional components found during W1b implementation).
//
// Implementing this method has no runtime behavior of its own - it exists
// purely so RefuseSyntheticInProduction (guard.go) can type-assert it, and
// so a human reading the type's declaration sees, right there, that it is
// not real. A component must never implement both Synthetic and
// ProductionEligible; RefuseSyntheticInProduction treats implementing
// Synthetic as disqualifying regardless of any other marker.
type Synthetic interface {
	// SyntheticComponent is a marker method with no meaningful body. Its
	// name deliberately does not collide with any real interface's method
	// set in this codebase.
	SyntheticComponent()
}

// ProductionEligible is implemented by a component that has a real, vetted
// production implementation behind it - a real PSP, KYC vendor, casino
// aggregator, sportsbook data feed, malware scanner, or document storage
// backend integration. RefuseSyntheticInProduction requires every
// registered component to implement this (and must NOT implement
// Synthetic) before the process is allowed to start with
// GuardEnvironment() == "production".
//
// No type in this codebase implements ProductionEligible yet (Stage
// 10.3, W1b) - that is deliberate and disclosed: nothing has a real
// vendor integration to back it. Wiring a genuine adapter later means
// implementing this marker on that adapter type, not merely omitting
// Synthetic.
type ProductionEligible interface {
	// MarkProductionEligible is a marker method with no meaningful body.
	MarkProductionEligible()
}

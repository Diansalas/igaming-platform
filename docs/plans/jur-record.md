# jur-closure record: TODO(jurisdiction) closure (payments / casino / risk)

Branch jur-closure from 76187cc. No migration added. No ADR/registry/HANDOVER/progress edits (proposals below).

## 1. Factual state at 76187cc
- internal/jurisdiction.Resolve: tenant/brand-subject operations resolve from the tenant licence (works; second jurisdiction = registry + licence rows). EVERY player-scoped operation returns unresolved(no_signal) (resolver.go; tests TestResolve_PlayerScopedOperationAlwaysUnresolvedNoSignal). DeterminePlayerJurisdiction (precedence.go) is a pure function with no production caller; resolver wiring to the evidence accessors is carried work gated on owner/legal decisions HDR-J-1 (no tenant fallback) and HDR-J-3 (player residence evidence). Wiring it would invent a legal-decision-bound feature, so it was NOT done.
- Casino LaunchGame: DOES call jurisdiction.Resolve (K-3), AssertScope, blocklist armed only for games with non-empty blocklist (unresolved => deny DenialCodeJurisdictionUnresolved), persists the code on the session; postBet reuses session.JurisdictionCode. Empty code + jurisdiction-scoped risk rule => risk.ErrMissingJurisdiction (fail closed; ADR 0031 s34).
- Risk: ErrMissingJurisdiction / ErrMissingScopeContext fail closed. Bonus eligibility: unresolved jurisdiction => AssetAuthorization denial (fail closed).
- Payments: RouteProvider has no jurisdiction dimension and there is NO jurisdiction-dependent routing policy store; deposit market restriction is the A8 operating-market gate (marketgate, markets are separate from jurisdictions); withdrawal_policies.jurisdiction_code is held NULL by CHECK withdrawal_policies_jurisdiction_not_yet_resolvable (migration 0033), callers pass nil. SupportedCountries is read only by capability write/declaration code.

## 2. Classification of each marker
| Marker | Class | Action |
|---|---|---|
| casino/launch.go:185, casino/orchestrator.go:1979 (postBet), casino adversarial test comment | PARTLY STALE (LaunchGame resolves via jurisdiction.Resolve; remaining gap is the player-scoped resolver being no_signal) | Comments rewritten to the precise fact; existing tests pin behaviour (adversarial_jurisdiction_isolation_test, jurisdiction_blocklist_integration_test, resolver_test) |
| risk/evaluator.go ErrMissingJurisdiction doc | same | Doc rewritten; existing risk fail_closed/evaluator tests pin it |
| payments/orchestrator.go RoutingRequest + ExcludeProviderIDs comment, types.go SupportedCountries note | REAL ONLY FOR A SECOND JURISDICTION / REAL PSP LICENCE MATRIX (no policy store exists, needs migration + security review; nothing fail-open today because nothing jurisdiction-dependent exists to skip) | TODO replaced by a precise design note: extension point, fail-closed contract, separation from SupportedCountries; pinned by tests below |
No marker was REAL AND REQUIRED for the first jurisdiction: the first jurisdiction's tenant-level resolution works and every player-level jurisdiction-dependent policy already fails closed.

## 3. Tests added
- internal/payments/jurisdiction_dimension_pin_test.go (unit): TestRoutingRequest_HasNoJurisdictionOrCountryDimension, TestPaymentsRouting_NeverReadsSupportedCountries (AST scan).
- internal/withdrawal/jurisdiction_scoped_policy_pin_integration_test.go: jurisdiction-scoped withdrawal_policies row rejected by CHECK 23514.
- internal/jurisdiction/jurisdiction_integration_test.go: TestResolve_SecondJurisdictionIsConfigurationOnly (second jurisdiction + licence + tenant by data only; player-scoped stays unresolved no_signal, no tenant fallback).
- internal/casino/second_jurisdiction_config_only_integration_test.go: two synthetic jurisdictions with their own rules enforced per session by the same postBet path; no-jurisdiction session fails closed with ErrMissingJurisdiction, no ledger effect.
No existing assertion changed or weakened.

## 4. Commands / results
- gofmt clean; go vet plain and -tags integration on payments, risk, casino, withdrawal, jurisdiction: clean.
- go test -count=1 (unit) payments risk jurisdiction marketgate bonus casino withdrawal: all ok.
- Integration (private DB, now run): jurisdiction (full package, after one fixture fix), casino, withdrawal, risk, marketgate, bonus: all ok (full packages, -p 1). First run revealed my new jurisdiction test used licensee 'second-licensee', violating CHECK licences_licensee_check; fixed to 'platform' (test fixture only). TestResolve_SecondJurisdictionIsConfigurationOnly and ResolveTenantLicence tests PASS (5/5 in targeted -v run). Payments subset -run 'Route|Routing|Capab|Jurisdiction|MarketGuard|Market': ok (90.7s). The casino and withdrawal new tests ran inside their passing full packages. Private DB dropped.

## 5. Residuals (labels)
- Player-scoped jurisdiction resolution (resolver wiring to residence evidence): NOT IMPLEMENTED, owner/legal dependent (HDR-J-1, HDR-J-3). Real players currently hit the fail-closed path whenever any jurisdiction-scoped rule/blocklist/AssetAuthorization exists.
- Payments routing dimension 2 (jurisdiction gate) and jurisdiction-scoped withdrawal policies: NOT IMPLEMENTED, second-jurisdiction / PSP-licence-matrix dependent; needs migration (lift CHECK / policy table) + security review. Single-jurisdiction behaviour pinned fail-closed/absent by the tests above.
- Full payments integration package not run (exceeds tool timeout); routing/capability/market subset run and green.

## 6. Proposed registry text (orchestrator applies)
"JUR-CLOSURE: all 7 TODO(jurisdiction) markers resolved to precise documented notes (no marker was first-jurisdiction-blocking). Payments dim-2 / jurisdiction-scoped withdrawal policy: NOT IMPLEMENTED (second-jurisdiction dependent; pins in internal/payments/jurisdiction_dimension_pin_test.go and withdrawal CHECK test). Player-scoped resolver wiring: NOT IMPLEMENTED (HDR-J-1/J-3). Second jurisdiction proven configuration-only (jurisdiction + casino tests; integration run pending DB)." The audit item "TODO(jurisdiction) filtering ... must close before a second jurisdiction" should be re-worded to "before a second jurisdiction: wire player-scoped resolver (HDR-J-1/J-3) and build payments dim-2 per the RoutingRequest design note".

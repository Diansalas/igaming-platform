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
- INTEGRATION TESTS NOT RUN: PostgreSQL at 127.0.0.1:5432 refused connections (pg_isready: no response, no postgres process), so priv_db.sh failed. Per rules I stopped and did not start/alter anything. The 3 new integration tests compile (vet -tags integration) but are UNRUN; they must be run by the orchestrator when the DB is up:
  export PRIV_DB=jurNNNNNNp1x01 PRIV_SRC=<worktree>; bash $S/priv_db.sh; bash $S/priv_test.sh -tags integration -count=1 -p 1 ./internal/jurisdiction ./internal/casino ./internal/withdrawal ./internal/risk ./internal/marketgate ./internal/bonus ./internal/payments -run 'SecondJurisdiction|JurisdictionScopedRow|Jurisdiction|Blocklist'

## 5. Residuals (labels)
- Player-scoped jurisdiction resolution (resolver wiring to residence evidence): NOT IMPLEMENTED, owner/legal dependent (HDR-J-1, HDR-J-3). Real players currently hit the fail-closed path whenever any jurisdiction-scoped rule/blocklist/AssetAuthorization exists.
- Payments routing dimension 2 (jurisdiction gate) and jurisdiction-scoped withdrawal policies: NOT IMPLEMENTED, second-jurisdiction / PSP-licence-matrix dependent; needs migration (lift CHECK / policy table) + security review. Single-jurisdiction behaviour pinned fail-closed/absent by the tests above.
- Integration tests above: written, UNRUN (DB down).

## 6. Proposed registry text (orchestrator applies)
"JUR-CLOSURE: all 7 TODO(jurisdiction) markers resolved to precise documented notes (no marker was first-jurisdiction-blocking). Payments dim-2 / jurisdiction-scoped withdrawal policy: NOT IMPLEMENTED (second-jurisdiction dependent; pins in internal/payments/jurisdiction_dimension_pin_test.go and withdrawal CHECK test). Player-scoped resolver wiring: NOT IMPLEMENTED (HDR-J-1/J-3). Second jurisdiction proven configuration-only (jurisdiction + casino tests; integration run pending DB)." The audit item "TODO(jurisdiction) filtering ... must close before a second jurisdiction" should be re-worded to "before a second jurisdiction: wire player-scoped resolver (HDR-J-1/J-3) and build payments dim-2 per the RoutingRequest design note".

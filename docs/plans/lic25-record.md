# lic25: scope-aware responsible-gaming selection (F1 of lic14, SN-3 of lic23)

Branch `lic25-rg-scope-aware` (from `integ-c` 6457e32; head 0186; this slice is migration **0187**). Software capability only; no value shipped; no
legal position. No jurisdiction is approved, no floor value is invented, no country is named in code or data.

## 1. Status

| Item | Label |
|---|---|
| RG exposure checks (deposit, wager, casino launch) select ONE jurisdiction from the player's scope (verified residence country, brand, operation, product) like KYC | IMPLEMENTED |
| Account-level RG calls (set / change / cancel / confirm a limit, options, time-out, operator bounds) use the STRICTEST FOLD of every jurisdiction the tenant relies on (licence plus approved bases) | IMPLEMENTED |
| Migration 0187: set-aware limit classification (cooling-off) and BEFORE INSERT trigger | IMPLEMENTED |
| Readiness R-RG-CONFIG and the activation guard evaluate floors in every relied-on jurisdiction; `bases_present_licence_rulebook_only` removed | IMPLEMENTED |
| Operator bounds written under every relied-on jurisdiction (all or nothing) | IMPLEMENTED (Go API; no HTTP change) |
| A legal comparison of regimes (which is "stricter" in substance) | NOT IMPLEMENTED, deliberately: only configured numbers are compared |
| Session-duration / reality-check enforcement | unchanged (NOT IMPLEMENTED, owner direction) |

## 2. What changed

### 2.1 Selection rules (internal/rg/reliance.go)

| Call site | Scope available | Selection | Cannot be resolved |
|---|---|---|---|
| `evaluateLimits` (EvaluateAction: deposit, casino wager, sportsbook wager, casino launch) | yes: player (residence evidence read in the same transaction), brand, operation, product | `scopedSelection`: no approved basis => the licence jurisdiction exactly as before; else `operatingmarket.ResolveRulebookJurisdiction`: the governing basis row's jurisdiction, or the tenant licence for a scope with no row | an error wrapping `ErrRulebookUnavailable`, returned by EvaluateAction as `ErrEvaluationUnavailable` (the caller's transaction rolls back = deny): no verified residence, a basis that does not permit, no row and no licence, an unreadable basis jurisdiction |
| `limits_service.go` `apply` (set / increase / removal / decrease / cancel / confirm) | no operation or product (a limit applies to both products) | `tenantReliance`: licence jurisdiction UNION jurisdictions of approved, active, in-force bases; strictest fold | increase, removal and confirm refused (`config_missing`) while a relied-on jurisdiction has no floor in force; initial values, decreases and cancels are never refused for it |
| `limits_timeout.go` (time-out) | none | same set; durations are the union of the jurisdictions that have a floor, none at all = platform defaults | never blocked (a time-out is protective) |
| `options.go` (player RG screen) | none | same set, strictest fold shown | refused for a tenant with an approved basis when a relied-on jurisdiction has no floor for deposit / loss / wager (the existing lic14 pin `TestHardening_H1_RGOptions_FailClosedForBasisTenantWithoutLicence` still passes unchanged); self-exclusion and limit setting do not depend on this read |
| `limits_usage_inputs.go` `ReadUsageConfig` / `ReadEffectiveLimitInputs` | none | same set; `UsageConfig.MissingFloors` reports a partial read | calendar_tz / week_start disagreement between relied-on jurisdictions is an error (windows cannot be chosen) |
| `limits_config.go` `WriteOperatorConfig` (tenant bound) | none | the row is written under EVERY relied-on jurisdiction, all or nothing (outer savepoint) | no resolvable jurisdiction: `no_floor`; a bound that loosens any one floor: refused as a whole (RL004 from that jurisdiction) |
| Migration 0135 trigger / `rg_limit_change_classify` | none | `rg_limit_change_classify_set` over the same set (cooling-off = the longest, reconfirmation if any requires it) | increase / removal: RL023 when the set is empty, incomplete or any member has no floor |

A tenant with NO approved operating basis resolves exactly as before at every site (same jurisdiction, same refusals, same classification; the
0135 signature is a one-element wrapper). Floors that are absent in a jurisdiction the tenant does not rely on are irrelevant.

"Stricter" is mechanical: the fold is the fold `rg_limit_config_compose` already applies to its own rows (numeric ceiling = minimum, ceiling
none only if all say none, cooling-off / post-session break = maximum, mandatory / enabled / count-bonus / concurrent-chain = OR, loss may go
negative = AND, option sets = UNION, scope = broadest, pending-intent ttl none dominates). No value is invented and no regime is judged;
`calendar_tz` / `week_start` are not orderable, so a disagreement fails closed (account-level usage read only; enforcement uses one jurisdiction).

### 2.2 Player-scope sessions

Every tenant-scoped read (licence chain, bases, residence evidence, rulebook) runs inside `withPlayerScopeCleared`, which clears
`app.player_account_id` and restores it afterwards (a restore failure is an error). The new SQL function `rg_basis_jurisdictions` refuses
(RL026) another tenant's id and a player-scoped session, so "no rows under RLS" can never read as "no basis". The trigger clears the player
scope before calling it, as it already did for the licence read.

### 2.3 Migration 0187

`rg_basis_jurisdictions(tenant, at)`, `rg_limit_change_classify_set(... text[], boolean ...)`, `rg_limit_change_classify` (0135 signature) as a
wrapper, and the trigger function replaced (`CREATE OR REPLACE`). No table, grant, role or data change; no SECURITY DEFINER. Down restores the 0135
bodies byte for byte (taken from the 0135 text by script) and drops the two new functions.

### 2.4 Readiness and activation

`rgConfigChecks` and `RGActivationReadiness` use `governedJurisdictions` (licence UNION approved bases; the helper R-KYC-POLICY already uses). No
licence and no approved basis still fails `no_licence_jurisdiction`; a basis-only tenant is now evaluated (floors needed in each basis jurisdiction);
`bases_present_licence_rulebook_only` no longer exists. **SnapshotVersion is NOT bumped**: no check was added or removed, only the detail codes of an
existing check changed (a stored snapshot that holds the removed code string stays valid history; I did not verify whether any consumer matches on it beyond the grep of
the repository, which found only docs and the one test replaced below; if the orchestrator wants a bump for audit clarity it is a one-line change in
launchgov, with the usual effect on in-flight snapshots).
OpenAPI descriptions (activation route, readiness code list) and `docs/runbooks/rg-enforcement-activation.md` (new section 3a), `docs/runbooks/launch-and-suspension.md`
are updated; no route, schema or error code changed, so the golden route table, step-up classes and OpenAPI route freeze are untouched. No Back Office
string referenced the removed code.

## 3. Pins and tests changed (declared)

- `TestLic23_SN3_RGConfigReadiness_BasesPresentIsAnAcknowledgeableNote` asserted the interim `bases_present_licence_rulebook_only`; the interim
  state is gone by design. It is replaced by `TestLic23_SN3_RGConfigReadiness_EvaluatesEveryReliedJurisdiction` (stronger: floor_missing_deposit while
  the basis jurisdiction lacks floors, pass when it has them, for both readiness and the activation guard; a basis-only tenant passes with floors;
  no licence and no basis unchanged).
- Everything else is unchanged and green: the lic14 RG pin (`ReadOptions` fails closed for a basis tenant without floors) passes as written.
- `ClassifyLimitChange` now calls `rg_limit_change_classify_set` (the 0135 function is kept and tested directly by the existing adversarial test).

## 4. Tests

New (all integration tests run as `igaming_runtime`): `internal/licensingchange/lic25_rg_scope_integration_test.go` (scoped deposit / casino wager /
launch / sportsbook fallback / brand-specific basis, basis-only tenant fail-closed matrix, no-basis unchanged, strictest fold of ceilings and cooling-off,
reliance without a floor: increase / removal / confirm refused and decrease / initial / cancel / time-out allowed, withdrawn basis releases it, raw trigger
inserts, isolation and the SQL function's scope refusals, operator bounds under every relied-on jurisdiction, unapproved owner-path row ignored,
incomplete flag), `internal/rg/reliance_fold_test.go` (pure fold rules incl. order independence), `internal/rg/reliance_static_test.go`.

Mutation checks, each killed by the named test (see section 5 of the report for the list run): scoped selection ignoring the basis, fold ceiling max, drop
licence from the set, no GUC restore, options without the missing-floor refusal, confirm without the check, no-residence falling back to the licence,
readiness / activation without the relied-on union, operator write to the first jurisdiction only, SQL cooling-off LEAST, SQL ignoring a missing floor,
SQL ignoring `p_incomplete`, SQL basis function without the player guard / the approved-request predicate / the status / the effective_to, trigger
passing the licence jurisdiction only.

## 5. Residuals (for the orchestrator)

1. Account-level reads ignore the player's country by design (no operation / product): the strictest fold may be stricter than the player's own regime
   needs; it is never weaker. Enforcement itself (the money path) uses the one governing jurisdiction.
2. A basis added later has no operator bounds until the tenant repeats the bound write (runbook says so). Existing rows are never rewritten.
3. Enforcement-time config for a scope uses only the governing jurisdiction's rows: tenant bounds written under the licence jurisdiction before this
   slice do not apply to basis-governed scopes until rewritten (see 2). This is a loosening relative to licence-only selection for such players in the
   window between approving a basis and rewriting the bound; readiness requires floors but cannot require operator bounds. Recommended: re-save bounds
   in the same change window as the basis approval.
4. `loss_computation` and `increase_requires_reconfirmation` are folded in SQL (classification) only; Go does not read them. Only `net_of_wins` exists.
5. Per money-path action the selection costs three extra reads (clock, `to_regclass`, basis existence) for every tenant, the same as the KYC hot path.
6. A tenant with an approved basis and enforcement OFF is unchanged: no limit is read (the switch is a separate one-way decision; the activation guard now
   requires floors in every relied-on jurisdiction).
7. The time-out duration union could offer a duration one relied-on regime does not list; it never removes a protective option.
8. Not run here: full `internal/httpserver`, `internal/payments`, `internal/casino`, `internal/sportsbook` (subsets only, see the report).

## 6. Proposed ADR text

> **AMENDMENT to ADR 0124 decision 5 (lic25).** The responsible-gaming rulebook follows the same selection as KYC. For an exposure action (deposit,
> wager, casino launch) it is the jurisdiction of the governing operating basis of the player's scope (verified residence country, brand, operation,
> product), or the tenant licence's for a scope with no basis row; a tenant with an approved basis whose scope cannot be resolved is refused, never
> read as "no configuration". For an account-level call that has no operation or product (limit setting, options, time-out, operator bounds, the
> cooling-off classification of an increase) it is the strictest fold of every jurisdiction the tenant relies on (licence and approved bases), using
> only the comparison rules `rg_limit_config_compose` already applies; a relied-on jurisdiction with no floor in force refuses an increase, a removal
> and a confirm, and never a decrease, an initial value, a cancel, a time-out or a self-exclusion. A tenant with no approved basis is unchanged.
> Launch readiness R-RG-CONFIG and the enforcement activation guard require floors in every relied-on jurisdiction. This records an engineering
> capability; which regime applies to which market is configuration chosen by the business, and nothing here is a legal position.

Registry row proposal: `LIC25-RG-SCOPE-AWARE | lic25 | F1 / SN-3 closed as IMPLEMENTED (software); residuals 1-3 | docs/plans/lic25-record.md`.

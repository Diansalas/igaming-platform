# ADR 0031 — Central Risk & Limits Engine

Status: Accepted. Part of Stage 4G ("Project Orchestration Governance +
Risk & Limits Engine"). Builds the platform's first Risk & Limits
foundation as ONE reusable engine, consumed by domain-specific
integration points - never a separate limit engine per product.

## Context

The platform needs a way to express and enforce limits (stake, deposit,
withdrawal, exposure, cumulative amounts) that vary by scope (platform,
jurisdiction, tenant, brand, player, provider, game, asset, payment
method, operation). Directive §11 explicitly requires this stay a
SEPARATE domain from Responsible Gaming (`internal/rg`, self-exclusion) -
they may compose in a shared enforcement point, never merge into one
concept.

## Decisions

### 1. Risk vs Responsible Gaming - kept separate

`internal/risk` has no self-exclusion concept and never reads/writes
`player_restrictions`. `internal/rg.EvaluateEligibility` remains the sole
authority for self-exclusion/account-status/wallet-status eligibility.
`internal/casino`'s orchestrator calls BOTH, in a fixed order: RG first
(`evaluateAndAuditEligibility`), then Risk (`evaluateAndAuditRisk`) -
never the reverse, and an RG denial short-circuits before Risk is ever
evaluated (no wasted evaluation, and no possibility of Risk's ALLOW
"overriding" an RG DENY, since RG's own denial already returns before
Risk runs at all).

### 2. Canonical decision boundary

```go
func Evaluate(ctx context.Context, tx pgx.Tx, req RiskRequest) (RiskDecision, error)
```

`RiskDecision.Outcome` is exactly one of `allow` / `deny` / `review`,
with a stable `Code`, human `Message`, the list of `MatchedRule`s that
contributed, and the caller's own `CorrelationID` for tracing - never a
provider-specific or ad-hoc type. Mirrors `rg.Decision`'s identical
"specific, distinguishable sentinel" contract.

### 3. Rule/policy model

`risk_rules` (migration 0041) - ONE table, dual-scope
(`tenant_id` NULL = platform-wide, non-NULL = tenant-owned, optionally
further narrowed by `brand_id`/`jurisdiction_code`/`player_account_id`/
`product`/`provider_id`/`game_id`/`asset_code`/`payment_method`),
mirroring `player_restrictions`' (migration 0037) already-reviewed
dual-scope pattern rather than inventing a second shape. `operation`
(casino_launch/casino_bet/deposit/withdrawal/sportsbook_bet/bonus_grant)
is always required - a rule with no operation could never be matched
deterministically.

### 4. Limit kinds - a small, fully-implemented set

`limit_kind` is exactly one of `min_amount`, `max_amount`,
`cumulative_amount` - directive §14's own "do not implement every future
rule unnecessarily, prioritize the architecture." `count`/`velocity`/
`exposure`/`loss` are documented, designed extension points, deliberately
NOT accepted by the database CHECK constraint or the evaluator: a rule
this package cannot evaluate must never be configurable in the first
place (stronger than defensively skipping it at evaluation time, and
closes the "malformed rule" fail-closed case directive §22 names).
Calendar-aligned windows (`calendar_day`/`calendar_month`) are a
documented future extension requiring jurisdiction-configured timezone
semantics, not implemented this stage - only `transaction` and four
UTC-anchored rolling windows (`rolling_hour`/`day`/`week`/`month`) exist.
"Session"-scoped limits (per casino launch session) are a documented
future extension requiring correlation to a launch-session identifier,
not implemented this stage.

### 5. Deterministic precedence

- **HARD_LIMIT**: enforced regardless of specificity. ALL matching hard
  limits are evaluated; a breach of ANY of them denies (or reviews, per
  that rule's own `action`), never overridden by a more specific
  configurable rule. Intended for legal/jurisdiction constraints.
- **CONFIGURABLE_LIMIT**: an ordinary, overridable commercial limit. For
  each `(limit_kind, time_window)` pair, the single MOST SPECIFIC
  matching rule wins - specificity order (most to least specific):
  player > game > provider > brand > tenant > jurisdiction >
  platform-wide/none, directive §16's own suggested ordering. This is
  NOT "most restrictive wins" - a player-specific override can be either
  stricter OR looser than a broader default, and whichever is more
  SPECIFIC applies either way (proven by
  `TestEvaluate_MostSpecificConfigurableRuleWins`/
  `TestReceiveCallback_PlayerRiskOverrideBeatsBrandDefault`, exercising
  directive's own worked example: player=50 beats brand=200).
- **Conflict detection**: two CONFIGURABLE rules tied at the SAME highest
  specificity for the same `(limit_kind, time_window)` is a genuine
  configuration error - `Evaluate` returns `ErrConflictingRules` rather
  than silently picking one by table order or insertion time
  (`TestEvaluate_ConflictingRulesAtSameSpecificityFailsClosed`).
- **RISK_SIGNAL**: contributes to a `review` outcome but never denies by
  itself.
- **Final priority**: DENY (hard or configurable) > REVIEW (from a
  review-action breach or any risk signal) > ALLOW - one fixed,
  documented order, never left implicit.

### 6. Fail-closed contract

Any non-nil error from `Evaluate` MUST be treated by the caller exactly
like a DENY - `internal/casino`'s `evaluateAndAuditRisk` propagates it as
a Go error, aborting the whole transaction (no ledger effect can have
happened yet at that point). This covers: a database error, an
unrecognized `limit_kind`/`rule_kind` (unreachable given the CHECK
constraints, but still handled rather than assumed), a conflicting-rule
configuration, and a rule requiring data the request didn't supply
(`ErrMissingAmount` for an amount-shaped rule on a zero-amount request,
`ErrUnsupportedCumulativeOperation` for a cumulative rule on an operation
with no ledger-transaction-type mapping). A `RiskDecision.Outcome ==
review` is treated identically to `deny` at every integration point this
stage ships (`internal/casino`) - no compliance-review workflow exists
yet to let a review-flagged operation proceed provisionally, so it fails
safe by blocking. Recorded as an explicit OPEN DECISION (§8 below),
not a resolved product choice.

### 7. Enforcement integration - only two of six designed operations

`internal/casino`'s `LaunchGame` (real-mode only, demo mode skips risk
evaluation entirely - no real financial exposure exists yet to gate) and
`postBet` (every delivery, after the RG check, before the balance lock -
identical position to the existing RG check) are the ONLY operations
actually wired to `risk.Evaluate` this stage. `deposit`/`withdrawal`/
`sportsbook_bet`/`bonus_grant` are designed (the `Operation` enum and
schema already support them) but NOT integrated - directive §21's "for
this stage, implement only the integrations that are safe and justified
by the existing architecture... document future integration points."
Wiring a future operation requires only: resolving its own
`RiskRequest` fields at that call site and calling `Evaluate` in the same
transaction as its own state-changing effect, before that effect
commits - no change to `internal/risk` itself.

### 8. Open decisions

- **Should a `REVIEW` outcome ever proceed provisionally** pending a
  compliance workflow, rather than blocking like `DENY`? Not resolved -
  this stage's only consumer (`internal/casino`) blocks on both. A future
  stage introducing an actual compliance review queue would revisit this.
  See §11 below for why this is a deliberately preserved three-way
  distinction, not a simplification opportunity.
- ~~Jurisdiction-scoped rules are reachable ONLY from `LaunchGame`, never
  from `postBet`~~ - **closed, Stage 4G-FINAL Part C.** See §9 below.
- ~~No licence-mode dimension exists~~ - **closed, Stage 4G-FINAL Part
  D.** See §10 below.
- **No role can create a genuinely platform-wide rule via HTTP** -
  `risk_config:manage` is held only by `RoleRiskManager`, which (like
  every non-`platform_admin` `StaffRole`) is always tenant-scoped
  (migration 0011's own CHECK constraint) - `RolePlatformAdmin` itself
  holds neither risk permission. Combined with the previous point, a
  legal/jurisdiction "hard ceiling" can today only be created
  tenant-scoped, and is itself tenant-disablable by that tenant's own
  `risk_manager` (audited, but not prevented) - see
  `internal/httpserver/risk_handlers.go`'s own doc comment for the
  identical `internal/rg.CreateStaffRestriction` precedent this mirrors.
  A genuinely non-negotiable, tenant-proof ceiling requires a future
  platform-scoped write path, not built this stage.
- **A tenant disabling a rule it can see but does not own returns 404,
  not 403** - `DisableRule` matches by id with no separate tenant check
  (RLS alone determines visibility), so attempting to disable a
  platform-wide rule from a tenant-scoped connection reports the same
  generic "not found" as a genuinely nonexistent id. This is intentional,
  not a bug: it mirrors this codebase's established "never confirm
  existence of a resource outside the caller's own authorization" pattern
  (identical to `internal/kyc`'s own 404-never-403 precedent, ADR 0029
  §4a) rather than leaking that a differently-scoped rule exists.

## Stage 4G-FINAL: hardening addendum

Stage 4G-FINAL's directive was explicit: no new business functionality,
harden the platform core so it is safe to build future domains on. The
sections below are that hardening, not a redesign - `risk.Evaluate`'s
signature, the rule table's shape, and every Stage 4G decision above are
unchanged except where a section below says otherwise.

### 9. Jurisdiction context architecture (Part C)

**What authoritative jurisdiction context Risk requires**: a single
string, `RiskRequest.JurisdictionCode`, matching a real `jurisdictions.
code` row (or empty, meaning "no jurisdiction context resolved for this
request"). `internal/risk` has never needed more than this - it is a
scope-matching value, not a geolocation computation, and this stage does
not add one.

**Where it originates**: `LaunchGameParams.JurisdictionCode`, resolved by
whatever caller invokes `LaunchGame` (an HTTP handler today) from
whatever source that caller trusts - a player's registered country, a
tenant's own default market, or (still, honestly) nothing at all. This
stage does not add a geolocation vendor or a per-player jurisdiction
resolver; `TODO(jurisdiction)` at that root cause is unchanged.

**How it is associated with the player/session/operation**: this is the
actual gap Stage 4G-FINAL closes. Before this stage, `LaunchGame` used
its resolved jurisdiction once, to evaluate risk at launch time, then
discarded it - the value never reached `casino_launch_sessions`. Migration
0042 adds `casino_launch_sessions.jurisdiction_code`, populated once at
`CreateLaunchSession` time (denormalized exactly like `provider_game_id`/
`asset_code` already were) and never updated afterward. The session is
therefore now the single source of jurisdiction context for its own
entire lifetime - launch AND every subsequent bet.

**How casino launch uses it**: unchanged - `LaunchGame` still resolves
`jurisdictionCode` from `params.JurisdictionCode` and evaluates risk with
it; the only change is that the SAME local value is now also passed to
`CreateLaunchSession` so it survives past the launch call.

**How casino bet uses it**: `postBet` now reads `session.JurisdictionCode`
(the value LaunchGame itself resolved and persisted) and forwards it into
its own `RiskRequest`, instead of leaving the field empty. A
jurisdiction-scoped `HARD_LIMIT` is therefore reachable from every bet in
a round, not just the round's own launch - closing the gap Stage 4G's
completion report disclosed. Regression test:
`TestReceiveCallback_BetDeniedByJurisdictionScopedRiskRuleViaLaunchSession`.

**How future deposits/withdrawals/sportsbook/bonus operations will use
it**: the identical pattern - whatever resolves a `RiskRequest` for that
operation supplies `JurisdictionCode` from whatever source THAT
operation's own authoritative context provides (e.g. a future
`wallet`/`player_accounts`-resolved jurisdiction for a deposit, unrelated
to casino sessions entirely). Nothing in `internal/risk` couples
jurisdiction resolution to casino specifically - `RiskRequest.
JurisdictionCode` is a plain string field any caller may populate from
its own domain's own trusted source.

**How conflicting or unavailable jurisdiction information is handled**
(simplified per documentation review - the prior wording restated this
three times): unavailable (empty) matches only jurisdiction-unscoped
rules, per `matches()`'s general "empty means wildcard, rule side only"
contract. A `RiskRequest` carries exactly one `JurisdictionCode` value,
never a set - resolving any genuinely ambiguous signal into that one
value is the caller's job, not `Evaluate`'s.

**How this remains provider-neutral**: `jurisdictions.code` is a
platform-registry concept (migration 0002, no RLS, ADR 0006) that predates
and is unrelated to any casino/payment provider - a jurisdiction code
means the same thing whether it reached a `RiskRequest` via a casino
launch, a future deposit, or a future sportsbook bet. No provider-specific
jurisdiction concept exists or is needed.

### 10. Licensing mode / BYOL architecture (Part D)

**Canonical representation**: `tenants.licensing_model` (migration 0001,
`'under_platform_licence'` or `'own_licence'`) - the ALREADY-EXISTING
canonical representation from ADR 0006's hybrid-licensing model, not a
new taxonomy invented this stage. `Rule.LicensingMode`/`RiskRequest.
LicensingMode` mirror its two values exactly.

**Policy-resolution contract**: identical shape to every other identity
field on `RiskRequest` (`TenantID`, `BrandID`, `JurisdictionCode`) -
resolved server-side by the CALLER, never looked up by `risk.Evaluate`
itself. `internal/casino` resolves it via the new `identity.
GetTenantByID` + a shared `resolveLicensingMode` helper, called once in
`LaunchGame` and once in `postBet` (never cached on the session, unlike
jurisdiction - a tenant's licensing model is a slow-changing
platform-registry fact, not a round-specific one, so a fresh lookup per
operation is correct and cheap).

**Why this matters**: before this stage, a platform-wide `HARD_LIMIT`
(`tenant_id IS NULL`) bound EVERY tenant identically, with no way to
express "this rule is OUR OWN platform licence's legal ceiling, not a
universal one." The moment a bring-your-own-licence tenant onboards
(operating under a different jurisdiction's own legal regime, ADR 0006),
that same platform-wide rule would incorrectly bind it too. Scoping such
a rule with `LicensingMode: "under_platform_licence"` closes this
exactly the way `JurisdictionCode`/`TenantID` already close their own
analogous gaps - a platform-wide rule left `LicensingMode`-unscoped still
applies to every tenant (unchanged default behavior; nothing existing
rules do today needs updating), and only a NEWLY authored rule that
needs the distinction sets it explicitly. Specificity ranks it just below
`JurisdictionCode` (a binary categorization is coarser than an actual
jurisdiction - see `types.go`'s own doc comment on `specificity()`).
Regression test:
`TestEvaluate_LicensingModeScopedHardLimitNeverBindsADifferentLicensingMode`
(proves isolation using today's single `under_platform_licence` tenant,
without a real BYOL tenant existing).

**No BYOL operator is onboarded or implemented this stage** - this is
the architectural contract a future BYOL onboarding will rely on, not a
BYOL feature itself. Future provider/domain code must resolve
`LicensingMode` from `tenants.licensing_model` exactly like `internal/
casino` now does, never assume `"under_platform_licence"` implicitly (the
one hardcoded assumption this stage specifically closes: before this
change, NOTHING in `internal/risk` could even express a
licensing-mode-dependent rule, which is itself a hardcoded single-mode
assumption by omission).

### 11. `REVIEW` semantics are preserved, not collapsed (Part E)

`Outcome` remains exactly three values - `allow`/`deny`/`review` - in
`internal/risk`'s own domain model; this stage changes nothing about
that. What Stage 4G already established and this stage reaffirms
explicitly:

- `REVIEW` represents a potentially human/compliance-resolvable outcome,
  semantically distinct from `DENY` (a REVIEW-flagged operation is not
  asserted to be prohibited, only that it warrants a human/compliance
  look before proceeding - see `RuleRiskSignal`'s own doc comment).
- Today's only two enforcement points (`internal/casino`'s `LaunchGame`/
  `postBet`) block on `REVIEW` exactly like `DENY`, purely because no
  compliance-review QUEUE exists yet to route a review-flagged operation
  to - this is an ENFORCEMENT-POINT choice (each caller's own
  `if riskDecision.Outcome != risk.OutcomeAllow` check), never a
  narrowing of `Outcome` itself down to two values.
- A future compliance workflow can be added WITHOUT changing `risk.
  Evaluate`'s signature or `RiskDecision`'s shape at all: it would consume
  the SAME `Outcome == review` value the type already carries, adding a
  new enforcement-point behavior (e.g., "post to a compliance queue and
  proceed provisionally, pending review") rather than a new field or a
  new return type. `MatchedRule`/`RiskDecision.Code`/`.Message` already
  carry enough explainability for such a queue to show a reviewer WHY an
  operation was flagged, without further design work.
- The open question from §8 ("should `REVIEW` ever proceed
  provisionally") is therefore an ENFORCEMENT-POLICY decision for a
  future stage to make per call site, not a blocker on `internal/risk`'s
  own architecture - which is precisely why it is recorded as an open
  decision rather than something this stage needed to resolve.

### 12. Extension model for future limit kinds (Part H)

Adding a genuinely new `LimitKind` (`count`, `velocity`, `exposure`,
`loss`, or a product-specific one like `stake`/`deposit`/`withdrawal`/
`bonus`/`session`) requires five additive changes, corrected here after
specialist review found the original three-step version understated the
real cost - never a redesign of `Evaluate`, the rule table's shape, or
the precedence algorithm, but every one of the five is required, not
optional:

1. **Migration**: widen the `limit_kind` CHECK constraint to accept the
   new value (and, if it needs a new column - e.g. a future `loss` limit
   might need a `realized_loss` computation column that `cumulative_amount`
   does not - add it additively, nullable, with its own CHECK). Also
   widen migration 0041's SECOND check on this column - the one coupling
   `limit_kind` to a valid `time_window` (`CHECK ((limit_kind IN
   ('min_amount','max_amount') AND time_window = 'transaction') OR
   (limit_kind = 'cumulative_amount' AND time_window <> 'transaction'))`)
   - a new `LimitKind` needs an explicit branch here too, or every
   `time_window` value becomes rejected for it.
2. **`internal/risk/types.go`**: add the new `LimitKind` constant.
3. **`internal/risk/evaluator.go`**: add a new `case` to `Rule.breach()`'s
   switch, implementing that limit kind's own comparison logic (the
   existing three cases - `min_amount`/`max_amount`/`cumulative_amount` -
   are the worked examples: a `transaction`-window comparison against
   `req.Amount` directly, or a windowed aggregate query against
   `ledger_transactions` for a stateful one).
4. **`internal/httpserver/risk_handlers.go`**: `newCreateRiskRuleHandler`
   hardcodes its own `v.RequireOneOf("limit_kind", ...)` allowlist,
   independent of the database CHECK - forgetting this step means the
   database and evaluator both accept the new kind while the HTTP API
   still rejects it with a 400, the exact inversion of the fail-closed
   principle below (evaluable but unconfigurable, rather than
   configurable but unevaluated).
5. **`docs/api/openapi/platform-api.yaml`**: the `limit_kind` enum
   appears twice (the POST request schema and the `RiskRule` response
   schema) and must be kept in sync with step 4's Go validation.

Nothing about `matches()`, `specificity()`, `isEffective()`, the
HARD_LIMIT/CONFIGURABLE_LIMIT/RISK_SIGNAL precedence algorithm, the
fail-closed contract, or any existing enforcement call site changes when
a new `LimitKind` is added - they are all generic over `LimitKind`
already. This is precisely WHY `LimitKind` was kept to a small,
fully-implemented set in Stage 4G rather than accepting every value the
directive listed: each of `count`/`velocity`/`exposure`/`loss` needs its
own real aggregation logic designed and tested against real data (a
"velocity" limit's own time-bucketing semantics, an "exposure" limit's
own definition of open exposure) - work that belongs to whichever future
stage actually needs that specific limit kind enforced, not invented
speculatively here. The architectural promise this section makes is that
building it later costs exactly the five steps above, never a rewrite -
"a rule the engine cannot evaluate must never be configurable" (Stage
4G's own principle) remains true only when ALL FIVE are updated together
in the same change (documentation review finding: an earlier version of
this section named only steps 1-3, which would have shipped a `LimitKind`
the database and evaluator both accept but the HTTP API still 400s -
corrected before this stage closed).

### 13. Every future domain must declare its own Risk integration (Part B)

Any future operation capable of affecting player financial exposure,
wagering exposure, payment exposure, bonus exposure, regulatory exposure,
or platform risk MUST declare its own `risk.RiskRequest` shape and call
`risk.Evaluate` in the same transaction as its own state-changing effect,
before that effect commits - exactly the pattern `internal/casino`
already establishes for `casino_launch`/`casino_bet`. Concretely, for
each domain listed in the Stage 4G-FINAL directive:

Status values below use CLAUDE.md's own seven-label vocabulary
("IMPLEMENTED", "PARTIALLY IMPLEMENTED", "MOCK", "STUB", "PROVIDER
DEPENDENT", "NOT IMPLEMENTED", "BLOCKED") rather than ad hoc wording, per
a documentation-review finding that the original table's "Enforced"/
"Designed, not wired" phrasing fell outside that vocabulary.

| Domain | Operation | Status |
|---|---|---|
| Casino launch | `casino_launch` | PARTIALLY IMPLEMENTED - `risk.Evaluate` is called and enforced (Stage 4G), and `JurisdictionCode`/`LicensingMode` are both correctly threaded through when supplied, but no HTTP handler populates `LaunchGameParams.JurisdictionCode` yet (`TODO(jurisdiction)` - no per-player jurisdiction resolver exists anywhere in this codebase), so jurisdiction-scoped rules are evaluable but never actually reached by a real production launch today - only by tests that populate it directly |
| Casino bet | `casino_bet` | PARTIALLY IMPLEMENTED - identical caveat: `risk.Evaluate` is called and enforced on every real bet, `LicensingMode` is always correctly populated (resolved from `tenants.licensing_model`, which is never empty), but `JurisdictionCode` is only non-empty when the session's own launch happened to have one (see Casino launch row) |
| Payments | `deposit`, `withdrawal` | NOT IMPLEMENTED - `Operation` enum + schema exist, `internal/payments` never calls `risk.Evaluate` - must call it before posting, mirroring `internal/casino`'s exact pattern, when that stage is authorized |
| Sportsbook | `sportsbook_bet` | NOT IMPLEMENTED - does not exist as a package yet (blocked per this stage's own stop condition) |
| Bonus | `bonus_grant` | NOT IMPLEMENTED - Bonus Engine is explicitly NOT started this stage (directive §32/Final Governance Rule); when it is, it MUST consume `internal/risk.Evaluate`, never build its own limit engine |

No new `Operation` enum values or schema changes were needed for this
declaration - `migrations/0041` already accepted all six from Stage 4G.
This section exists so a future domain's own directive can point here
rather than re-deriving the integration contract from scratch.

## Specialist review findings and fixes

An independent 9-area parallel review (risk architecture, financial
correctness, casino integration, RG integration/domain separation,
security, PostgreSQL/RLS, API, adversarial testing, multi-tenancy) found
and this stage fixed, before completion:

- **P1 (risk architecture, security, financial correctness, adversarial -
  4 independent findings)**: `Rule.specificity()` scored only the single
  highest-ranked scope dimension present, so a rule scoped by
  `(tenant)` and a rule scoped by `(tenant, asset_code)` were treated as
  an ARTIFICIAL TIE - forcing `ErrConflictingRules` and therefore a
  fail-closed outage of the ENTIRE operation for that tenant, for two
  rules that were never actually in conflict. **Fixed**: rescored as a
  bitmask summing every present scope dimension (player/game/provider/
  asset/payment_method/product/brand/tenant/jurisdiction), so an
  additional narrowing dimension always strictly increases specificity
  rather than being ignored. Regression test:
  `TestEvaluate_AdditionalScopeDimensionIsMoreSpecificNotATie`.
- **P1 (risk architecture)**: the configurable-rule aggregation loop
  iterated a Go map (randomized order) and unconditionally overwrote the
  aggregate action on every group, so a later REVIEW-action breach could
  silently DOWNGRADE an earlier DENY-action breach purely depending on
  map iteration order - a genuinely non-deterministic final `Outcome` for
  the identical rule set, run to run. **Fixed**: deny-priority merge
  (mirroring the hard-limit aggregation's own already-correct pattern),
  order-independent by construction; `listEffectiveRules` also gained an
  `ORDER BY id` for defense in depth. Regression test:
  `TestEvaluate_DenyBeatsReviewAcrossConfigurableGroupsRegardlessOfOrder`
  (20 iterations).
- **P1 (financial correctness, security - 2 independent findings)**: the
  cumulative-amount check summed `NUMERIC(38,0)` ledger amounts into a
  plain `int64`, which can silently wrap for an 18-exponent asset
  (CLAUDE.md's own crypto-precision rule) - a wrapped negative sum would
  fail OPEN exactly where a cumulative limit most needs to fail closed.
  **Fixed**: the SUM is now scanned as `pgtype.Numeric` and compared via
  `math/big`, never through a fixed-width integer.
- **P1 (financial correctness)**: the same query counted a bet's original
  debit even after a `casino_rollback` reversed it, permanently consuming
  cumulative capacity for a voided round. **Fixed**: the query now nets
  debits minus credits across both the operation's own transaction type
  and its rollback counterpart.
- **P1 (PostgreSQL/RLS, live-database confirmed)**: `risk_rules`' RLS
  policies omitted the `app.player_account_id IS NULL` guard every
  sibling dual-scope table (`player_restrictions`, `sessions`) carries -
  a `db.WithPlayerScope` connection could read every tenant's risk rules
  and successfully INSERT/UPDATE (disable) one, contradicting this
  codebase's own documented `WithPlayerScope` isolation contract. No
  player-facing code path reaches `risk_rules` today, so this was
  defense-in-depth, not a live incident - but it is exactly the kind of
  latent gap this project's own governance now exists to catch before it
  becomes one. **Fixed**: added the guard to all four policies.
  Regression test: `TestRiskRules_PlayerScopeConnectionCannotReadOrWrite`.
- **P1 (architect, RG integration - this ADR itself)**: this document did
  not exist at the time code/migration comments already cited it -
  **fixed** by its own existence; also closed the identical `docs/
  progress.md`/`docs/active-stage.md` gap the same reviewers found.
- **P0 (adversarial)**: `RuleRiskSignal` contributing to `REVIEW`, and a
  `HARD_LIMIT` rule with `action=review`, were real code paths with ZERO
  test coverage. **Fixed**: `TestEvaluate_RiskSignalContributesReview`,
  `TestEvaluate_HardLimitReviewAction`.
- **P0/P1 (adversarial)**: `Evaluate`'s own effective-window filtering
  (not just the unit-level `isEffective()` helper) and the cross-tenant
  DISABLE HTTP path had no integration-level test. **Fixed**:
  `TestEvaluate_EffectiveWindowIsEnforcedAtEvaluateLevel`,
  `TestRiskRules_CrossTenantDisableDenied`.
- **P2s fixed**: `docs/api/openapi/platform-api.yaml` updated for the
  three new risk endpoints (API review); `risk.rule_created`/
  `risk.rule_disabled` audit records now carry `IPAddress`/`UserAgent`/
  `RequestID` (security review); the HTTP create-rule handler now
  validates every enum field (`operation`/`limit_kind`/`time_window`/
  `rule_kind`/`action`/`product`) before reaching the database, returning
  400 instead of a generic 500 (API, security reviews); the audit
  metadata for a casino risk denial now includes `provider_id`/
  `game_id`/`asset_code`/`amount` (casino integration review); the down
  migration documents the `risk_manager`-row one-way-door explicitly
  (security, risk architecture reviews).
- **P2s recorded, not fixed this stage** (see "Known limitations" in
  `docs/progress.md`'s Stage 4G entry for the full list with reasoning):
  `CreateRule` does not cross-validate `limit_kind`/`operation`
  compatibility (a `max_amount` rule on `casino_launch`, or a
  `cumulative_amount` rule on an unenforced operation, is storable but
  will fail-closed-error every real request it matches); an oversized
  `threshold` inserted directly via SQL (bypassing `CreateRule`'s own
  `int64` parameter) can brick evaluation for an entire tenant+operation
  (consistent with this codebase's pre-existing, accepted `int64`-minor-
  units convention - `ledger.EntryInput.Amount` carries the identical
  limitation); no uniqueness constraint on `risk_rules` prevents two
  identical-specificity rules from being created in the first place (the
  evaluator's own `ErrConflictingRules` is the only backstop); GET
  `/v1/admin/risk/rules` has no pagination or default disabled-row
  filter; `risk_signal` rules force `action=review` server-side with no
  HTTP-layer feedback if the caller sent `action=deny`; no FK from
  `risk_rules.tenant_id` to `tenants(id)` (matching `player_restrictions`'
  own pre-existing, identical gap); the immutability trigger permits a
  disabled rule to be re-enabled and an `effective_until` to be cleared,
  with no dedicated audited Go path for either.


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
| Bonus | `bonus_grant` | NOT IMPLEMENTED - Bonus Engine is explicitly NOT started this stage (directive §32/Final Governance Rule); when it is, it MUST consume `internal/risk.Evaluate`, never build its own limit engine. Contract specified in full by Stage 4H-A, §15a-§15d below: `min_amount`/`max_amount` apply unchanged, `cumulative_amount` is NOT usable for this operation until `operationLedgerTransactionTypes` gains a bonus entry, and frequency requires a `count` `LimitKind` that does not exist |
| Gamification - tournament entry | `tournament_entry` (PROPOSED, documented only - §16) | NOT IMPLEMENTED - the `Operation` value does not exist in `internal/risk/types.go` or migration 0041's CHECK constraint, and is deliberately not added this stage (architecture-freeze) |
| Gamification - marketplace purchase | `marketplace_purchase` (PROPOSED, documented only - §16) | NOT IMPLEMENTED - same: proposed value, no code, no migration this stage |
| Rewards - redemption | `reward_redemption` (PROPOSED, CONDITIONAL - §16) | NOT IMPLEMENTED - needed only if a redemption can create player value WITHOUT going through a Bonus Engine Grant; if every redemption materializes as a Grant, `bonus_grant` already covers it and no new value should be added. Open decision, §17 |
| Points earning / points spending | none - deliberately out of Risk's scope (§15h) | NOT IMPLEMENTED, and deliberately so - Risk gates the money boundary (`bonus_grant`/`reward_redemption`/`marketplace_purchase`), never the points balance itself, for exactly as long as points cannot themselves become withdrawable value |
| Cross-domain aggregate player exposure | none - not expressible today (§17) | NOT IMPLEMENTED - `Evaluate` loads rules for exactly ONE `req.Operation` (`listEffectiveRules(ctx, tx, req.Operation)`) and `matches()` requires `r.Operation == req.Operation`, so no rule can span bonus + casino + sportsbook. Explicit OPEN DECISION for a future stage, not solved here |

No new `Operation` enum values or schema changes were needed for this
declaration - `migrations/0041` already accepted all six from Stage 4G.
This section exists so a future domain's own directive can point here
rather than re-deriving the integration contract from scratch.

**Stage 4H-A addendum**: the last five rows above were appended by Stage
4H-A's architecture freeze for Bonus/Gamification/Reward Orchestration.
Every `Operation` value in them marked PROPOSED is DOCUMENTED ONLY - no
Go constant, no migration, and no HTTP validation accepts any of them,
and none may be created before the extension steps in §16 are executed
together in one authorized change. The table's format and status
vocabulary are unchanged.

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

## Stage 4H-A: Bonus & Gamification Risk Integration

Status of this section: **architecture freeze only.** Stage 4H-A's
directive is explicit - no implementation, no new limit kinds, no new
`Operation` values in code, no migration, no enforcement wiring. This
section is a CONTRACT specification and a set of extension points,
exactly the way §12 already documents the `LimitKind` extension model
WITHOUT implementing any of it. Nothing in §14-§18 changes
`risk.Evaluate`'s signature, `risk_rules`' shape, the precedence
algorithm, the fail-closed contract, or any existing enforcement call
site; every Stage 4G and Stage 4G-FINAL decision above stands unmodified.

Integration points below are described against the OTHER domains'
concepts abstractly ("however the Bonus Engine represents a Grant",
"however the Reward Orchestrator represents a reward") because those
designs are being frozen in parallel and do not exist in final form. This
section deliberately commits to nothing about their internal shapes -
only to where `risk.Evaluate` sits relative to them.

### 14. The hard rule, restated and specialized for Bonus/Gamification

The rule established for Casino in §1/§2 and generalized in §13 applies
to Bonus, Gamification, Tournaments, Missions, the Marketplace, and the
Reward Orchestrator **unchanged, unweakened, and without reinterpretation
for this domain**:

- Every exposure-affecting decision in these domains MUST consult
  `internal/risk.Evaluate`. "Exposure-affecting" means the same thing it
  means in §13: capable of affecting player financial exposure, wagering
  exposure, payment exposure, bonus exposure, regulatory exposure, or
  platform risk.
- NEITHER Bonus NOR Gamification may build its own limit or risk engine.
  No `internal/bonus` (or gamification/tournament/marketplace equivalent)
  may contain a threshold comparison, a per-player cap table, a "max
  bonus per day" counter, a velocity check, or any other structure that
  is a limit engine under another name. They author `risk_rules` rows and
  interpret an `ALLOW`/`DENY`/`REVIEW` result; they never re-implement
  the comparison. This is the identical constraint `internal/casino`
  already operates under and the identical one `internal/payments` and
  the sportsbook will operate under.
- The call happens **inside the same database transaction as the
  state-changing effect it gates, before that effect commits** - the same
  positional contract §13 states, for the same reason: PostgreSQL inside
  the guarded transaction is the only authoritative correctness boundary
  for a cumulative check. A bonus grant evaluated in one transaction and
  written in another is not gated; it is merely advised. No cache, and no
  counter maintained by the Bonus Engine, may stand in for that read.
- Risk does NOT absorb Responsible Gaming. A self-excluded, cooled-off,
  or otherwise RG-ineligible player must not receive a grant, enter a
  tournament, or redeem a reward - and that determination belongs to
  `rg.EvaluateEligibility`, which remains the sole authority (§1). A
  bonus/gamification enforcement point composes BOTH, in the same fixed
  order `internal/casino` already uses (RG first, short-circuiting; Risk
  second). It must never be expressed as a `risk_rules` row, and Risk
  must never read `player_restrictions`. Ownership of that composition's
  RG half sits with `identity-compliance`, not with this ADR.
- A non-nil error from `Evaluate` is a DENY at every one of these new
  call sites, per §6. There is no bonus-specific softening: an
  unavailable evaluator, a malformed rule, conflicting rules, or a
  missing scope value must never resolve to "grant it anyway."

### 15. Integration points - the contract, decision by decision

Each item below states (i) whether the decision must call `Evaluate`,
(ii) which EXISTING mechanism covers it, and (iii) precisely what, if
anything, is genuinely missing. Where something is missing it is named as
a gap or an open decision - it is not designed around.

**(a) Bonus issuance - yes, and `bonus_grant` already exists.** Creating
a grant is exposure-affecting by construction (it creates a contingent
platform liability and, on wagering completion, a real one), so it is
analogous to `casino_launch`: the gate runs before the object that
carries the exposure exists. The Bonus Engine MUST call `Evaluate` with
`Operation: OperationBonusGrant` in the same transaction that inserts the
Grant, before that insert commits, and must abort the whole transaction
on `DENY`, on `REVIEW` (see §17's open decision - there is still no
compliance queue to route a review to), and on any error. The
`RiskRequest` it populates is the ordinary one: `TenantID`/`BrandID`/
`PlayerAccountID` resolved server-side only; `Product: "bonus"` (already
accepted by migration 0041's `product` CHECK); `AssetCode` set to the
asset the grant is denominated in; `Amount` set to the grant's own value
in that asset's minor units; `JurisdictionCode` and `LicensingMode`
resolved from the bonus domain's own trusted context per §9/§10 (NOT
from a casino launch session - nothing couples jurisdiction resolution to
casino, and a bonus granted outside any game session has no session to
read); `CorrelationID` set to whatever the Bonus Engine uses to identify
this issuance. `ProviderID`/`GameID`/`PaymentMethod` are left empty
unless the grant is genuinely scoped to one, which also means a rule
narrowed by those dimensions simply never matches a general grant - the
correct behavior, not a gap.

**(b) Bonus amount - covered unchanged by `max_amount`/`min_amount`; NOT
covered by `cumulative_amount`.** This splits into two genuinely
different answers and must not be reported as one:

- `LimitMaxAmount`/`LimitMinAmount` with `TimeWindow: transaction` apply
  to a bonus grant with **no change whatsoever**. `Rule.breach()`'s
  `min_amount`/`max_amount` cases compare `req.Amount` directly and are
  entirely generic over `Operation`; "max bonus grant of X for this
  brand/player/jurisdiction" is expressible today, and the full
  HARD_LIMIT vs CONFIGURABLE_LIMIT precedence and player-override
  behavior of §5 applies to it identically (a player-scoped max bonus
  beats a brand default, stricter or looser, by specificity).
- `LimitCumulativeAmount` ("no more than X in bonus value in a rolling
  week") is **a genuine gap today, and it fails closed rather than
  silently**. `Rule.breach()`'s cumulative case looks `req.Operation` up
  in `operationLedgerTransactionTypes`, which contains exactly one entry
  (`casino_bet`), and returns `ErrUnsupportedCumulativeOperation` for
  anything else. A `cumulative_amount` rule on `bonus_grant` is therefore
  storable today (`CreateRule` does not cross-validate
  `limit_kind`/`operation` compatibility - an already-recorded P2) but
  will fail-closed-error every grant it matches. Closing it is NOT a new
  `LimitKind` and NOT an `internal/risk` redesign; it is two additive
  facts that `ledger-finance`'s Bonus Accounting work (ADR 0032, in
  progress in parallel) must first establish and this ADR must then
  record: (1) the `ledger_transactions.transaction_type` a bonus grant
  posts, added to `operationLedgerTransactionTypes`, and (2) its
  reversal/forfeiture counterpart type, added to
  `operationLedgerRollbackTypes` so a forfeited or clawed-back bonus does
  not permanently consume the player's cumulative bonus capacity - the
  exact correctness property the `casino_bet`/`casino_rollback` netting
  already establishes. Until both exist, `cumulative_amount` on
  `bonus_grant` is documented as unsupported, not quietly assumed to
  work. Note also that the existing aggregation query nets
  `ledger_entries` filtered by `player_account_id` and `asset_code`; a
  grant that produces no ledger entry at issuance time (if ADR 0032
  decides a grant is recognized only on conversion) would aggregate to
  zero, which is a correctness question for `ledger-finance` to answer,
  not for Risk to guess.

**(c) Bonus frequency - genuinely requires a NEW `LimitKind`, named here,
NOT implemented here.** "No more than N bonuses per player per day" is a
COUNT over a time window, not an amount comparison, and it cannot be
expressed by any of the three implemented limit kinds. Faking it (e.g. a
`cumulative_amount` rule with a threshold chosen to approximate a count)
would be exactly the ad hoc logic §12 exists to prevent. The correct
value is the already-reserved `count` `LimitKind` named in §4 and §12 -
`LimitCount = "count"`, with a rolling `time_window`, whose `Threshold`
is a number of occurrences rather than minor units. It is **not added
this stage**: adding it costs all five steps of §12 (migration CHECK
widening including the `limit_kind`/`time_window` coupling CHECK, the Go
constant, a `Rule.breach()` case implementing the counting query, the
HTTP handler's own `RequireOneOf` allowlist, and both OpenAPI enum
occurrences), plus one further design decision §12's amount-shaped
examples do not answer: **what a `count` limit counts.** Ledger
transactions of the grant's type is the obvious answer for `bonus_grant`,
but a `count` limit is by nature reusable across operations, and counting
rows in the Bonus Engine's own grant table would couple `internal/risk`
to another domain's schema - which it does not do for any existing limit
kind and must not start doing. Whichever future stage implements `count`
owns resolving that, with `ledger-finance`. Until then, bonus frequency
is NOT configurable, which is the intended state: a rule the engine
cannot evaluate must never be configurable (§4). Note the semantic
mismatch that also has to be handled at that time: `Rule.breach()`
currently returns `ErrMissingAmount` for a zero-`Amount` request on every
amount-shaped kind, so a `count` rule must explicitly NOT require
`req.Amount` - a free-spin or non-monetary grant legitimately has no
minor-unit amount.

**(d) Promotional caps - split deliberately into two different things.**
A **per-player promotional cap** ("this player may receive at most X in
promotional value this month") is a Risk rule, scoped by
`player_account_id`/`brand_id`/`jurisdiction_code` like any other, and is
covered by (b) above with (b)'s cumulative caveat. A **campaign-level
budget cap** ("this campaign may issue at most X in total across ALL
players") is **not expressible by `internal/risk` and, on this ADR's
analysis, should not be**: every scope dimension on `risk_rules` narrows
toward a single player/brand/tenant, and the cumulative aggregation query
is explicitly keyed by `le.player_account_id = $2`. Cross-player
aggregation does not exist in this engine and adding it would change what
a "limit" means here. A campaign budget is a commercial inventory
constraint on a campaign object, not a player-protection or exposure
limit, and it belongs to whatever the Bonus Engine uses to represent a
campaign - with one non-negotiable guard, which this ADR asserts under
its §14 authority: **a campaign budget counter must never be used to
express a per-player limit.** The moment a proposed campaign-side
constraint is keyed by player, it is a limit engine under another name
and must be a `risk_rules` row instead. If a future stage decides
campaign budget genuinely must be a Risk concern, that is a new scope
dimension plus cross-player aggregation - an open decision (§17), not a
thing to retrofit quietly.

**(e) Tournament entry with a real-money entry fee - yes, gate it, and it
needs its own `Operation`.** A paid tournament entry debits a player's
wallet against a wagering outcome; it is a bet in everything but name,
and exempting it would create a route to real-money wagering exposure
that no risk rule can see. It must call `Evaluate` in the same
transaction that posts the entry-fee debit, before commit, with `Amount`
set to the entry fee in the wallet asset's minor units. It is NOT
`casino_bet`: reusing that value would silently make every existing
casino stake rule bind tournament entries and vice versa, which is a
configuration-semantics change to already-authored rules and the exact
kind of implicit coupling §13 exists to prevent. Proposed value:
`tournament_entry` (§16, documented only). A **free-entry** tournament
(no monetary fee, no monetary prize path) is out of scope for the same
reason (h) below is: it creates no monetary exposure.

**(f) Reward redemption - gate it; whether it needs its own `Operation`
is CONDITIONAL and currently OPEN.** Redemption is the point where an
earned, non-monetary entitlement becomes something of value, so it is
unambiguously exposure-affecting and must be gated. What it must be
gated AS depends on a design owned elsewhere and not yet frozen: if every
redemption materializes as a Bonus Engine Grant, then `bonus_grant`
already covers it and adding `reward_redemption` would be inventing a
second name for one decision. If a redemption can produce value WITHOUT a
Grant (e.g. crediting a wallet directly, or issuing an external
provider's reward), it needs its own operation because its rules,
thresholds, and ledger mapping are genuinely different. Recorded as an
open decision in §17 rather than resolved here under pressure to look
complete; the proposed value, if the second branch turns out to be true,
is `reward_redemption` (§16).

**(g) Marketplace purchases - yes, gate them.** A marketplace purchase
converts a balance (points, cash, or both) into an item that may itself
carry monetary value or a monetary path. Even in the purely
points-denominated case it is the boundary at which accumulated
non-monetary activity can become monetary value, which is precisely where
(h)'s reasoning says the gate belongs. Proposed value:
`marketplace_purchase` (§16, documented only). Where the purchase is paid
in cash, `Amount`/`AssetCode` are the ordinary wallet asset and minor
units and existing amount-shaped limit kinds apply unchanged; where it is
paid in points, see the precision constraint in §18 - points are not an
`assets` registry row, so a points-denominated THRESHOLD is not currently
configurable at all, and the gate would initially be about the item's
monetary value or about a future `count` limit, not about the points
spent.

**(h) Points earning and spending - Risk does NOT need to see them, under
one stated condition.** The test this ADR adopts is: *can a sequence of
points activity, with no other gated operation, increase the platform's
monetary liability or the player's withdrawable value?* If the answer is
no - points are non-convertible, non-withdrawable, non-transferable, and
carry no direct monetary path - then points earning and points spending
are NOT exposure-affecting, and requiring a `risk.Evaluate` call on every
points award would add a database round trip and a fail-closed dependency
to a non-financial event for no risk benefit. Risk instead gates the
**money boundary**: `bonus_grant`, `reward_redemption`, and
`marketplace_purchase` - the operations at which points can actually
become value. This is a deliberate scope decision, not an oversight, and
it is conditional in both directions: **if any future design makes points
directly convertible to cash, withdrawable, or transferable between
players, this conclusion is void** and points movements become
exposure-affecting operations requiring their own `Operation` values and
their own gates. Whoever proposes such a conversion path owns reopening
this paragraph; it may not be treated as settled on the strength of this
stage's freeze. (Note the related asymmetry: points ABUSE - farming,
multi-accounting, collusion in missions - is a real risk concern, but it
is a detection/`RISK_SIGNAL` concern that would need a `count`/`velocity`
limit kind and a points-side data source `internal/risk` has no access
to; it is not solved by gating individual points awards and is not solved
this stage.)

### 16. Proposed `Operation` values - DOCUMENTED ONLY

Per §13's own table, `bonus_grant` already exists and covers bonus
issuance, bonus amount, and (once `count` exists) bonus frequency - no
new operation is needed for ANY of the three, which is the single most
important negative result of this analysis. Frequency needs a new
`LimitKind`, not a new `Operation`, and conflating those two extension
axes would have produced operations nobody needs.

Three further values are proposed for the gamification/reward surface.
They are DOCUMENTED ONLY: no Go constant, no migration, no HTTP
validation, no OpenAPI enum entry is added this stage, and none of them
can be stored in `risk_rules` today (migration 0041's `operation` CHECK
rejects every value outside the original six, which is the intended
fail-closed posture - see §18).

| Proposed value | Gates | Why not an existing value |
|---|---|---|
| `tournament_entry` | A tournament entry that debits real money | Reusing `casino_bet` would retroactively rebind every existing casino stake rule to tournament entries (§15e) |
| `marketplace_purchase` | Spending a balance on a marketplace item | Neither a wager nor a grant; its own thresholds and its own ledger mapping (§15g) |
| `reward_redemption` | CONDITIONAL - only if a redemption can create value without a Bonus Engine Grant | If every redemption IS a Grant, `bonus_grant` covers it and this value must NOT be added (§15f, §17) |

Each slots into the existing `Operation` type's pattern exactly (a
`string`-typed constant in `internal/risk/types.go` alongside the current
six) and requires no change to `matches()`, `specificity()`,
`isEffective()`, the precedence algorithm, or the fail-closed contract -
`Operation` is only ever compared for equality and used as the
`listEffectiveRules` filter. Also note that `risk_rules.product`'s CHECK
constraint today accepts only `('casino','sportsbook','payments',
'bonus')`; if gamification is modeled as a product distinct from `bonus`,
that CHECK needs additive widening in the same change, otherwise a
gamification rule can only be authored `product`-unscoped or mislabeled
as `bonus`. Flagged, not decided - the product taxonomy is the
`architect`'s, not this ADR's.

**Extension model for adding an `Operation`** (the `Operation` analogue
of §12's `LimitKind` model; the two have different costs and must not be
confused):

1. **Migration**: widen migration 0041's `operation` CHECK constraint
   additively.
2. **`internal/risk/types.go`**: add the `Operation` constant.
3. **`internal/httpserver/risk_handlers.go`**: add it to
   `newCreateRiskRuleHandler`'s own `RequireOneOf("operation", ...)`
   allowlist, which is independent of the database CHECK - skipping this
   ships an operation the database accepts but the API 400s, §12's own
   documented inversion.
4. **`docs/api/openapi/platform-api.yaml`**: the `operation` enum appears
   in both the POST request schema and the `RiskRule` response schema and
   must be kept in sync with step 3.
5. **`operationLedgerTransactionTypes` / `operationLedgerRollbackTypes`
   (`internal/risk/evaluator.go`) - ONLY if `cumulative_amount` must work
   for it.** Omitting this is safe but not free: a `cumulative_amount`
   rule on the new operation is storable and will then fail-closed-error
   every matching request (`ErrUnsupportedCumulativeOperation`). The
   mapping values are `ledger-finance`'s to specify, never Risk's to
   invent.
6. **The enforcement call site itself**: resolve the `RiskRequest` in the
   owning domain and call `Evaluate` in the same transaction as the
   guarded effect, before commit (§13). Adding the enum value without
   this step gives a configurable rule that nothing consults - the other
   inversion, and the one most likely to be mistaken for "done."

Unlike a new `LimitKind`, a new `Operation` needs NO new `Rule.breach()`
case - the comparison logic is generic over `Operation` already. That is
why these are cheaper, and why the analysis above preferred new
operations over new limit kinds wherever both could have worked.

### 17. Open decisions introduced or confirmed by Stage 4H-A

- **Cross-domain aggregate player exposure is NOT expressible today, and
  no solution is invented here.** A rule such as "this player's total
  exposure across bonus + casino + sportsbook may not exceed X" cannot be
  written: `Evaluate` loads rules for exactly one operation
  (`listEffectiveRules(ctx, tx, req.Operation)`), `matches()` rejects any
  rule whose `Operation` differs from the request's, and the cumulative
  aggregation query nets exactly one operation's own transaction types.
  Every scope dimension narrows a rule; none of them broadens one across
  operations. Expressing this genuinely requires a concept the engine
  does not have - an operation SET or an "exposure scope" spanning
  operations, together with a definition of what "exposure" means when it
  spans a settled casino stake, an unsettled sportsbook position, and an
  outstanding bonus liability (three different things that are not
  summable without a decision about each). That definition is a joint
  `ledger-finance` + Risk design, and the `exposure` `LimitKind` reserved
  in §4/§12 is where it would land. **Recorded as an explicitly OPEN
  DECISION requiring its own future stage.** It is NOT partially
  approximated this stage, and a per-operation rule must not be described
  anywhere as if it provided aggregate exposure control.
- **Is `reward_redemption` a distinct operation, or is it `bonus_grant`?**
  Unresolvable until the Reward Orchestrator and the Bonus Engine's Grant
  representation are frozen (both in progress in parallel, owned
  elsewhere). Decision rule recorded in §15f so whoever closes it does
  not re-derive it.
- **Should campaign-level budget caps ever be a Risk concern?** This ADR's
  position is no (§15d), with the per-player guard stated. Revisiting it
  means adding a campaign scope dimension AND cross-player aggregation -
  the same class of change as the previous item.
- **Does a `REVIEW` outcome block a bonus grant, or hold it?** §8's
  original open decision, now with a second plausible consumer. A held
  grant is arguably a more natural fit for a review queue than a blocked
  bet is - but there is still no compliance review queue, so the
  fail-safe default of §6 stands: `REVIEW` blocks, exactly like `DENY`,
  at every enforcement point including every new one described here.
  Unchanged, unresolved, and deliberately not resolved by this stage.
- **What does a `count` limit count** (§15c) - ledger rows, or a domain
  table's rows? Owned by whichever stage implements `count`; the
  constraint this ADR imposes is that `internal/risk` must not query
  another domain's schema to answer it.
- **Points convertibility** (§15h) - the condition under which points
  legitimately stay outside Risk. If it is ever violated, §15h is void.

### 18. What remains impossible to configure after this section

This is the load-bearing claim of Stage 4H-A, stated explicitly per the
directive: **nothing in §14-§17 implements a new limit kind, a new
`Operation`, a new scope dimension, or any new enforcement wiring.** The
following remain genuinely impossible to configure, by database CHECK
constraint and by HTTP validation, and remain so after this stage:

- `count`, `velocity`, `exposure`, `loss` as `limit_kind` values -
  rejected by migration 0041's CHECK and by `newCreateRiskRuleHandler`'s
  allowlist. Bonus FREQUENCY is therefore not configurable at all (§15c),
  and must not be approximated with `cumulative_amount`.
- `tournament_entry`, `marketplace_purchase`, `reward_redemption` as
  `operation` values - rejected by the same two layers. A rule intended
  to govern a tournament entry cannot be stored today, which is correct:
  it would be a rule nothing evaluates.
- `cumulative_amount` on `bonus_grant` (and on every operation other than
  `casino_bet`) - storable, but guaranteed to fail closed with
  `ErrUnsupportedCumulativeOperation` on every matching request until
  `ledger-finance` supplies the transaction-type mapping (§15b). Treated
  as unavailable, not as working.
- Cross-operation aggregate exposure in any form (§17).
- A campaign-level (cross-player) budget cap (§15d).
- A points-denominated threshold - `risk_rules.asset_code` is
  `REFERENCES assets (code)` and `Threshold` is `int64` minor units
  interpreted against that asset's registered exponent. Points are not an
  `assets` row and have no registered exponent, so there is no way to
  express "at most N points" even if one wanted to; per CLAUDE.md's own
  precision rule, inventing an exponent-less numeric limit is not an
  acceptable workaround. Missing asset precision is a fail-closed case,
  not a rounding opportunity.
- Calendar-aligned windows and session-scoped limits - unchanged from §4.

The principle §4 established holds without exception through this stage:
a rule the engine cannot evaluate must never be configurable in the first
place. This section is an extension-point specification; the extension
points stay closed until an authorizing stage opens them with all the
steps in §12 or §16 executed together in one change.


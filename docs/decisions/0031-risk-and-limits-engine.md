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
| Bonus - issuance AND activation | `bonus_grant` (both checkpoints) | NOT IMPLEMENTED - Bonus Engine is explicitly NOT started this stage (directive §32/Final Governance Rule); when it is, it MUST consume `internal/risk.Evaluate`, never build its own limit engine. Contract specified in full by Stage 4H-A, §15a-§15d below: `min_amount`/`max_amount` apply unchanged, `cumulative_amount` is NOT usable for this operation until `operationLedgerTransactionTypes` gains a bonus entry, and frequency requires a `count` `LimitKind` that does not exist. Both the `issued` creation and the `issued` → `activated` transition call `Evaluate` under this same operation value - activation is re-evaluated, not assumed covered by the grant-time decision (§15a-ii); no new `Operation` value is needed for either, and `bonus_activate` is explicitly rejected |
| Bonus - completion/conversion | `bonus_conversion` (PROPOSED, documented only - §16) | NOT IMPLEMENTED - the `Operation` value does not exist in `internal/risk/types.go` or migration 0041's CHECK constraint, and is deliberately not added this stage (architecture-freeze). The `completed` → `converted` release MUST be gated separately with the ACTUAL released amount (max-cashout/partial-wagering capped), which is why it cannot reuse `bonus_grant` (§15a-ii). **Verified current status and the exact remaining steps: §16a (`NOT STARTED`, zero of six)** |
| Gamification - tournament entry | `tournament_entry` (PROPOSED, documented only - §16) | NOT IMPLEMENTED - the `Operation` value does not exist in `internal/risk/types.go` or migration 0041's CHECK constraint, and is deliberately not added this stage (architecture-freeze) |
| Gamification - marketplace purchase | `marketplace_purchase` (PROPOSED, documented only - §16) | NOT IMPLEMENTED - same: proposed value, no code, no migration this stage |
| Rewards - redemption | `reward_redemption` (PROPOSED, CONDITIONAL - §16) | NOT IMPLEMENTED - needed only if a redemption can create player value WITHOUT going through a Bonus Engine Grant; if every redemption materializes as a Grant, `bonus_grant` already covers it and no new value should be added. Open decision, §17 |
| Points earning / points spending | none - deliberately out of Risk's scope (§15h) | NOT IMPLEMENTED, and deliberately so - Risk gates the money boundary (`bonus_grant`/`reward_redemption`/`marketplace_purchase`), never the points balance itself, for exactly as long as points cannot themselves become withdrawable value |
| Cross-domain aggregate player exposure | none - not expressible today (§17) | NOT IMPLEMENTED - `Evaluate` loads rules for exactly ONE `req.Operation` (`listEffectiveRules(ctx, tx, req.Operation)`) and `matches()` requires `r.Operation == req.Operation`, so no rule can span bonus + casino + sportsbook. Explicit OPEN DECISION for a future stage, not solved here |

No new `Operation` enum values or schema changes were needed for this
declaration - `migrations/0041` already accepted all six from Stage 4G.
This section exists so a future domain's own directive can point here
rather than re-deriving the integration contract from scratch.

**Stage 4H-A addendum**: the last six rows above were appended by Stage
4H-A's architecture freeze for Bonus/Gamification/Reward Orchestration,
and the `Bonus - issuance AND activation` row was widened by the same
freeze to name activation as its own checkpoint.
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

**(a-ii) Bonus activation and completion/conversion - two further
checkpoints; one reuses `bonus_grant`, one needs a new `Operation`.**
`docs/architecture/10-bonus-engine-architecture.md` §4 correctly states
that the Bonus Engine calls `Evaluate` at three points, not one, and
defers the `Operation` question to this ADR. Resolved here:

- **Activation (`issued` → `activated`) - YES, its own `Evaluate` call,
  reusing `OperationBonusGrant` unchanged.** The grant-time check in (a)
  is NOT sufficient, because activation is not inherent to the grant
  having been approved: doc 10 §1.2 states an `issued` Grant carries "no
  wagering exposure yet", and §1.3 lists activation triggers (an opt-in
  click, a later qualifying `deposit.settled`, manual staff activation)
  that can occur arbitrarily long after issuance. Activation is the
  transition at which funds actually exist in the player's wallet, so by
  §13's own rule it is the exposure-affecting effect, and the rules in
  force at *that* moment (effective dates, a jurisdiction hard limit
  added since, a newly added player-scoped override) are the rules that
  must govern it. It reuses `OperationBonusGrant` - already a constant in
  `internal/risk/types.go` and already accepted by migration 0041 -
  rather than a new `bonus_activate` value, because it is the same policy
  question about the same money at a later instant; a distinct operation
  would force every operator to author each bonus cap twice, and one
  forgotten copy is a silent gap. This is the mirror image of (e): there,
  reuse would have created FALSE coupling between unrelated decisions;
  here, reuse creates the correct coupling. **No new code and no
  migration** - DOCUMENTED ONLY in the sense that no caller exists yet.
  One trap for (b)'s future ledger mapping: once `bonus_grant` gains
  `operationLedgerTransactionTypes` entries, a two-step grant must
  consume its cumulative capacity ONCE across the issuance/activation
  pair, not twice - otherwise activation is denied by the very grant
  being activated. That is `ledger-finance`'s to specify, per (b).
- **Completion/conversion (`completed` → `converted`) - YES, and it needs
  a DISTINCT `Operation`: `bonus_conversion` (§16, DOCUMENTED ONLY;
  verified status and remaining steps in §16a - `NOT STARTED`, zero of
  §16's six steps done).**
  Conversion releases withdrawable cash, so it is exposure-affecting in
  the strongest sense. It must NOT reuse `OperationBonusGrant`: the
  amount differs by design (max-cashout capping, partial wagering), and
  the thresholds differ in kind - a converted amount legitimately exceeds
  the grant that produced it (a 50-unit grant wagered up converts against
  a 250-unit max cashout). Reusing `bonus_grant` would make every
  existing `max_amount` grant rule silently bind conversions and deny
  legitimate ones, which is exactly the retroactive-rebinding failure
  §15e rejects for `casino_bet`/`tournament_entry`. The `RiskRequest` is
  (a)'s shape with `Amount` set to the **actual amount to be released** -
  payout rules applied FIRST, the result then gated - never the original
  grant value. Naming: `bonus_conversion`, superseding doc 10 §4's
  placeholder `bonus_convert` and consistent with §16's other noun-form
  proposals; `bonus_activate` is explicitly NOT adopted. Fail-closed
  still applies here without destroying a player entitlement: a `DENY`,
  `REVIEW`, or error blocks the transition and leaves the Grant in
  `completed`, which is non-terminal and retryable after review - it does
  not forfeit.

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
issuance, bonus amount, bonus **activation** (§15a-ii), and (once `count`
exists) bonus frequency - no new operation is needed for ANY of the four,
which is the single most important negative result of this analysis.
Frequency needs a new `LimitKind`, not a new `Operation`, and conflating
those two extension axes would have produced operations nobody needs.

Four further values are proposed. They are DOCUMENTED ONLY: no Go
constant, no migration, no HTTP validation, no OpenAPI enum entry is
added this stage, and none of them can be stored in `risk_rules` today
(migration 0041's `operation` CHECK rejects every value outside the
original six, which is the intended fail-closed posture - see §18).

| Proposed value | Gates | Why not an existing value |
|---|---|---|
| `bonus_conversion` | The `completed` → `converted` release of bonus value as withdrawable cash | The released amount differs from the granted amount by design (max cashout, partial wagering); reusing `bonus_grant` would silently rebind every existing grant-size rule to conversions (§15a-ii) |
| `tournament_entry` | A tournament entry that debits real money | Reusing `casino_bet` would retroactively rebind every existing casino stake rule to tournament entries (§15e) |
| `marketplace_purchase` | Spending a balance on a marketplace item | Neither a wager nor a grant; its own thresholds and its own ledger mapping (§15g) |
| `reward_redemption` | CONDITIONAL - only if a redemption can create value without a Bonus Engine Grant | If every redemption IS a Grant, `bonus_grant` covers it and this value must NOT be added (§15f, §17) |

`bonus_activate` was considered and **rejected** (§15a-ii): activation is
re-evaluated under `bonus_grant`, not under a value of its own.

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

### 16a. `bonus_conversion` — verified current status (added Stage 4H-B0-R1)

**STATUS: `NOT STARTED` — zero of §16's six extension-process steps are
completed. `bonus_conversion` exists today as documentation only, in this
ADR and in `docs/architecture/10-bonus-engine-architecture.md`. It blocks
any Bonus Engine `completed` → `converted` risk-gated check until closed.**

This subsection exists so no future reader has to infer readiness from
prose elsewhere. It was written by the `risk` specialist during Stage
4H-B0-R1 (governance correction) against the actual repository state at
that commit, not from memory of a prior stage's claims. It is Risk's
authoritative position and supersedes any readiness characterization of
this dependency made in another document.

**Verified repository state (each row checked directly, not assumed):**

| §16 step | Artifact | Verified state |
|---|---|---|
| 1. Migration CHECK | `migrations/0041_risk_limits_engine.up.sql`, `operation` CHECK | **NOT DONE** — accepts exactly `casino_launch, casino_bet, deposit, withdrawal, sportsbook_bet, bonus_grant`. A `bonus_conversion` rule row is rejected by the database today (the intended fail-closed posture, §18) |
| 2. Go constant | `internal/risk/types.go` | **NOT DONE** — the `Operation` const block declares exactly the same six values; `OperationBonusGrant` exists, there is no `OperationBonusConversion` |
| 3. HTTP allowlist | `internal/httpserver/risk_handlers.go`, `newCreateRiskRuleHandler`'s `RequireOneOf("operation", ...)` | **NOT DONE** — same six values |
| 4. OpenAPI enum | `docs/api/openapi/platform-api.yaml` | **NOT DONE** — same six values in all occurrences |
| 5. Ledger type mapping | `operationLedgerTransactionTypes` / `operationLedgerRollbackTypes` (`internal/risk/evaluator.go`) | **NOT DONE**, and conditional — the maps contain exactly one entry each (`casino_bet` → `casino_bet` / `casino_bet` → `casino_rollback`). Required only if `cumulative_amount` must work for `bonus_conversion`; see the caveat below |
| 6. Enforcement call site | `internal/bonus` | **NOT DONE, and cannot be done** — the package does not exist. No Bonus Engine code has ever been authorized (Stage 4G §32 / Stage 4H-A / Stage 4H-B0 were all architecture-only) |

Nothing partial exists: there is no half-landed constant, no dormant
migration, no feature-flagged path. The dependency has not been started.

**One correction to §16's own step 4, found during this verification.**
Step 4 says the `operation` enum appears in "both the POST request schema
and the `RiskRule` response schema." It appears in **three** places: those
two plus the `GET /v1/admin/risk/rules` `operation` **query parameter**
enum. The Go list handler (`newListRiskRulesHandler`) validates that
parameter only for non-emptiness, so omitting the third occurrence does
not break the running API — it produces a spec that declares a value
invalid which the implementation accepts, which is a documentation defect,
not a fail-open. Step 4 is to be read as "all three occurrences."

**Exactly what remains, as a single authorized change.** All six steps
land together (§16's closing rule, restated in §18 and §24: the extension
points stay closed until an authorizing stage opens them with all steps
executed in one change). Per-step, for `bonus_conversion` specifically:

1. Additively widen migration 0041's `operation` CHECK to include
   `'bonus_conversion'` — a new forward migration, never an edit to 0041.
2. Add `OperationBonusConversion Operation = "bonus_conversion"` to
   `internal/risk/types.go`. Naming is fixed by §15a-ii and is not
   re-openable: `bonus_conversion`, superseding doc 10 §4's placeholder
   `bonus_convert`; `bonus_activate` remains rejected.
3. Add it to `newCreateRiskRuleHandler`'s `RequireOneOf("operation", ...)`.
4. Add it to all three OpenAPI `operation` enum occurrences (above).
5. `operationLedgerTransactionTypes` / `operationLedgerRollbackTypes` —
   **omit deliberately for the first slice, with the consequence stated.**
   The mapping values are `ledger-finance`'s to specify (§15b), and the
   `bonus_conversion` `ledger_transactions.transaction_type` does not
   exist in that table's CHECK constraint yet either (ADR 0032). Omitting
   this step is safe (fail-closed), not free: once step 1 lands, a
   `cumulative_amount` rule on `bonus_conversion` becomes **storable**
   (`CreateRule` does not cross-validate `limit_kind`/`operation`
   compatibility — an already-recorded P2) and will then
   `ErrUnsupportedCumulativeOperation` on every conversion it matches.
   That is correct fail-closed behavior and a genuine configuration
   foot-gun; whoever authorizes the change must decide explicitly whether
   to land step 5 with `ledger-finance` or to accept the foot-gun and
   restrict first-slice rule authoring to `min_amount`/`max_amount` with
   `TimeWindow: transaction`.
6. Wire the enforcement call site in `internal/bonus`'s conversion path:
   apply payout rules FIRST, then call `Evaluate` with `Amount` set to the
   **actual amount to be released** (never the original grant value), in
   the same transaction as the conversion posting, before commit (§15a-ii,
   §13). A `DENY`/`REVIEW`/error leaves the Grant in `completed`, which is
   non-terminal and retryable — it never forfeits a player entitlement.
   This is the step most likely to be mistaken for "done" when only
   steps 1–4 have landed: an `Operation` value nothing consults is a
   configurable rule with no enforcement, §16 step 6's inversion.

**Ownership and sequencing.** Steps 1–5 are `risk`-owned. Step 6 is
`bonus-engine`-owned and is the only step that cannot precede
`internal/bonus` existing. Steps 1–4 have no structural dependency on ADR
0032's ledger CHECK widenings and may land before them; step 5 depends on
them. The dependency-request procedure in
`docs/governance/integration-protocol.md` is the route by which
`bonus-engine` requests steps 1–5 — Risk does not land them speculatively,
because an `Operation` value with no call site is precisely the inversion
step 6 warns about.

**Is anything ELSE on Risk's side blocking the Bonus Engine's first slice
(deposit, reload, cashback, generic wagering bonus, coupon)?** Checked
against `docs/architecture/10-bonus-engine-architecture.md` §1–§3 (read
only). Risk's authoritative answer: **no — `bonus_conversion` is the only
Risk-owned P0/P1 dependency.** Specifically:

- **Grant issuance and activation need ZERO Risk changes.**
  `OperationBonusGrant` is a real constant, accepted by migration 0041,
  present in the HTTP allowlist and the OpenAPI enums. `min_amount`/
  `max_amount` with `TimeWindow: transaction` work unmodified for it,
  with full HARD_LIMIT/CONFIGURABLE_LIMIT precedence and player-override
  behavior (§15b). `risk_rules.product` already accepts `'bonus'`; no
  widening is needed for the Bonus Engine (the §16 `product`-CHECK flag
  concerns *gamification*, which is out of the first slice).
- **`cumulative_amount` on `bonus_grant` is a known gap, correctly
  characterized by doc 10 §2 condition 1, and is NOT a blocker.** It fails
  closed (`ErrUnsupportedCumulativeOperation`), and restricting the first
  slice's rule authoring to `min_amount`/`max_amount` avoids it entirely.
  Risk confirms that restriction is sufficient for all five in-slice
  types and confirms it is a rule-authoring scope choice, not a code
  change. Note the same storable-but-erroring foot-gun applies here today.
- **Bonus frequency (`count` `LimitKind`) is not configurable at all**
  (§15c) and is confirmed NOT a first-slice blocker — none of the five
  types requires a frequency cap to function.
- **Campaign-level budget caps are NOT a Risk dependency and must never
  become one** (§15d). They belong to the Bonus Engine's campaign object,
  under the non-negotiable guard that a campaign budget counter must never
  be keyed by player — the moment it is, it is a limit engine under
  another name and must be a `risk_rules` row instead.
- **Self-exclusion / RG is out of scope by construction** (§1, §14).
  `rg.EvaluateEligibility` remains the sole authority; the Bonus Engine
  composes both at its enforcement point, exactly as `internal/casino`
  does. Nothing here is blocked on Risk.
- **§17's open decisions** (cross-domain aggregate player exposure,
  campaign budget as a Risk concern, what a `count` counts, points
  convertibility) are confirmed as genuine open decisions that do **not**
  block the first slice.

**One correction to doc 10 §2's condition 2, issued from Risk's own
authority.** That document describes the dependency as "the
`bonus_conversion` `Operation` value (Go constant in
`internal/risk/types.go` + an additive widening of migration 0041's
`Operation` CHECK constraint)" — two artifacts. Per §16, it is **six
steps**, of which that description names two. The omitted ones are not
cosmetic: skipping step 3 ships an operation the database accepts but the
API 400s (§12's documented inversion), and skipping step 6 ships a
configurable rule nothing consults. Doc 10's substantive claim — that this
is scoped, additive, fully specified, and not an open design question —
**stands and is confirmed by Risk.** Its *size* was understated. Doc 10 is
not edited by this stage; this paragraph is the authoritative correction.

**Also correct in doc 10, and confirmed:** conversion is on the first
slice's critical path, not a later slice's. All five in-slice types run
`issued` → `activated` → `completed` → `converted`, so the conversion
checkpoint cannot be deferred out of the first slice while still shipping
those five types end to end. A first slice that stopped at `completed`
would ship five bonus types that can never release value to a player.

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

## Stage 4H-B0: Retail Risk Integration

Status of this section: **architecture/scope freeze only — everything
below is `NOT IMPLEMENTED`.** Stage 4H-B0's directive is explicit: no
code. This section adds no `Operation` value, no `LimitKind`, no scope
dimension, no column, no migration, no HTTP validation, no OpenAPI enum
entry, and no enforcement wiring. It is a CONTRACT specification and a
set of extension points, in exactly the form §12 (`LimitKind` extension
model), §16 (`Operation` extension model) and §14-§18 (the Bonus/
Gamification freeze) already established. Nothing in §19-§24 changes
`risk.Evaluate`'s signature, `risk_rules`' shape, the precedence
algorithm, the fail-closed contract, or any existing enforcement call
site. Every Stage 4G, 4G-FINAL and 4H-A decision above stands unmodified,
and §14-§18 are untouched by this section.

**Business context.** A confirmed requirement introduces a retail
agent-hierarchy network — Operator → Partner → Super Agent → Agent →
Player/Cashier, configurable per tenant/licence/jurisdiction. The two
requirements this ADR owns are directive requirement #6 ("funding limits
and withdrawal limits by hierarchy level") and directive requirement #16
("Risk, AML/KYC, responsible-gaming and self-exclusion enforcement must
remain platform-wide and must not be bypassed through retail").

**Assumptions about the hierarchy model — stated so they can be
verified, not assumed silently.** The retail/agent-hierarchy domain
design (package ownership, table shape, actor model) is being produced in
parallel by the `architect` and does not exist in this repository: as of
this section's authorship, no `internal/retail` package, no hierarchy
table, no migration, and no retail document exist (the only occurrence of
"cashier" anywhere in `docs/` is a brand-frontend UI label in
`00-system-overview.md`). This section therefore describes retail
concepts abstractly — "however the retail domain represents a hierarchy
node" — in the same way §14-§18 described the Bonus Engine's Grant. It
commits to nothing about the retail domain's internal shape, only to
where `risk.Evaluate` sits relative to it. Four assumptions are
load-bearing and **must be confirmed by the Orchestrator against the
architect's parallel document before any of this is built**:

1. A hierarchy **node** is a persistent, server-side-identifiable entity
   with a stable identifier, owned by exactly one `tenant_id`.
2. A node has exactly one **type** (`hierarchy_node_type`, a tenant-owned
   configuration row, not a platform-wide ladder position — e.g.
   `partner`, `super_agent`, `agent`, `cashier` are *seed data*, not an
   enum) drawn from a small, tenant/licence/jurisdiction-configurable set
   — i.e. type is a configuration value, not a Go type or a code path
   (CLAUDE.md's "nothing brand-specific may become a code path", applied
   to hierarchy shape). **Wave-2 review correction (F5, P1)**: an earlier
   draft of this ADR called this dimension "level" throughout, which
   contradicted `docs/architecture/26-retail-operations-architecture.md`
   §1.2's explicit rejection of any level/ladder concept — a node has a
   `node_type_id`, never a level. Renamed to `hierarchy_node_type`
   everywhere in this section to match doc 26's actual entity.
3. A retail-originated operation has exactly ONE acting node resolvable
   server-side from the authenticated retail session — never a set, and
   never client-supplied.
4. A player transacting at retail is an identified `player_accounts` row,
   not an anonymous over-the-counter bearer ticket. See §22 — if this
   assumption is false, directive requirement #16 is structurally
   unsatisfiable and that is a `identity-compliance` blocker, not
   something Risk can fix.

### 19. The hard rule, restated and specialized for Retail — `ARCHITECTURAL DECISION`

The rule established for Casino in §1/§2, generalized in §13, and
specialized for Bonus/Gamification in §14 applies to the retail
agent-hierarchy domain **unchanged, unweakened, and without
reinterpretation for this domain**:

- Funding limits and withdrawal limits by hierarchy level (requirement
  #6) are `internal/risk` `Rule`s — rows in `risk_rules` — evaluated
  through the existing `Evaluate(ctx, tx, req RiskRequest)
  (RiskDecision, error)` boundary. They are not a retail feature that
  happens to involve limits.
- Whatever package ends up owning the retail/agent hierarchy **must not
  build a second limit engine.** No threshold comparison, no per-agent
  cap table, no "max float per day" counter, no velocity check, no
  per-level limit map in retail configuration code. The retail domain
  authors `risk_rules` rows and interprets `ALLOW`/`DENY`/`REVIEW`; it
  never re-implements the comparison. This is the identical constraint
  `internal/casino` already operates under and `internal/payments`, the
  sportsbook, the Bonus Engine and Gamification are all already bound by.
- The call happens **inside the same database transaction as the
  state-changing effect it gates, before that effect commits** (§13,
  §14). PostgreSQL inside the guarded transaction is the only
  authoritative correctness boundary for a cumulative check. A float
  advance evaluated in one transaction and posted in another is not
  gated; it is merely advised. No cache, and no counter maintained by the
  retail domain, may stand in for that read.
- A non-nil error from `Evaluate` is a DENY at every retail call site,
  per §6. There is no retail-specific softening — not for an offline
  terminal, not for a degraded-connectivity cashier, not for a
  store-and-forward queue. See §24's open decision on offline retail:
  the answer this ADR gives today is that an operation that cannot be
  risk-evaluated inside its own transaction cannot be authorized, and
  anything else is a product decision that must be taken explicitly, by a
  human, with `security` and `identity-compliance` in the room — never
  arrived at by an implementation detail.
- **Every existing platform-wide, jurisdiction-scoped, tenant-scoped and
  player-scoped rule continues to bind a retail-originated operation**,
  because `matches()` treats an unset scope dimension on the RULE as a
  wildcard. Retail is a new channel, not a new rulebook. This is the
  Risk half of requirement #16 and it holds by construction, not by
  discipline — there is no retail bypass to close because no code path
  exists by which a retail operation could skip rules that a non-retail
  operation matches, **provided** the retail domain actually calls
  `Evaluate` (§21) rather than transacting without it. That proviso is
  the only real risk here, and it is an integration obligation, not an
  engine gap.

### 20. Hierarchy scoping — two new scope dimensions, not a new precedence mechanism — `RECOMMENDATION`

Requirement #6 needs two genuinely different things, and they must not be
collapsed into one field:

- **By LEVEL** — "an `agent`-level node may fund at most €X per day; a
  `super_agent`-level node at most €Y." A categorical narrowing.
- **By SPECIFIC NODE** — "this one agent has a negotiated custom limit."
  An entity narrowing, exactly analogous to today's `player_account_id`
  override beating a brand default.

**Decision: both fit the existing `Rule` model as ordinary additive scope
fields, and neither requires a new precedence mechanism.** The extension
discipline of §12/§16 applies unchanged; nothing about `matches()`'s
equality contract, `isEffective()`, the HARD_LIMIT/CONFIGURABLE_LIMIT/
RISK_SIGNAL algorithm, the deny-priority merge, `ErrConflictingRules`, or
the fail-closed contract changes.

**(a) Shape.** Two fields on both `Rule` and `RiskRequest`, mirroring the
two shapes the struct already uses:

- `HierarchyNodeType string` — mirrors `Product`/`LicensingMode` exactly: a
  configuration-valued string, empty meaning "applies regardless of
  type." **Wave-2 review correction (F5, P1)**: unlike `Product`/
  `LicensingMode` (fixed platform vocabularies), a node type code is
  tenant-authored (doc 26 §1.4 explicitly allows two tenants to define
  the same code, e.g. `'agent'`, meaning genuinely different things). A
  platform-wide (`tenant_id IS NULL`) rule referencing a tenant-authored
  type code would silently bind every tenant's differently-meaning type.
  This field therefore carries the same `CHECK (hierarchy_node_type IS
  NULL OR tenant_id IS NOT NULL)` the `hierarchy_node_id` field below
  already has — a platform-wide rule may not scope by node type at all,
  only a tenant-scoped rule may.
- `HierarchyNodeID *uuid.UUID` on `Rule` / `uuid.UUID` on `RiskRequest` —
  mirrors `PlayerAccountID`/`BrandID` exactly, including the
  `NULL`-means-wildcard convention and (on the rule side) the
  `CHECK (hierarchy_node_id IS NULL OR tenant_id IS NOT NULL)` that
  `player_account_id` already carries, since a node belongs to exactly
  one tenant.

Both are resolved **server-side by the caller** from the authenticated
retail session, never client-supplied and never looked up by
`risk.Evaluate` itself — the identical resolution contract §10 states for
`LicensingMode` and CLAUDE.md states for `tenant_id`. A client-supplied
node id would let a cashier claim a level with a higher limit, which is
the single most obvious attack on this design.

**(b) Precedence.** `specificity()` is a bitmask summing every present
scope dimension (the P1 fix recorded in the specialist-review section
above). Two new dimensions mean two new bits and a renumbering of the
existing ones. Recommended order, most to least specific:

```
player > hierarchy_node > game > provider > asset > payment_method >
product > hierarchy_node_type > brand > tenant > jurisdiction >
licensing_mode
```

Rationale, consistent with the ordering rationale already in
`types.go`'s own doc comment: `hierarchy_node` names one specific entity
and so ranks with the entity-identifying dimensions, but immediately
BELOW `player_account_id`, because a node contains many players and a
single player is the narrowest possible subject — a player-specific
override must keep beating a node-specific one. `hierarchy_node_type` is a
categorical class, like `product` and `licensing_mode`, so it ranks with
those: below every entity-identifying and value-carrying dimension, and
above the operating-entity chain (`brand`/`tenant`/`jurisdiction`/
`licensing_mode`) that describes WHO operates rather than WHERE in the
distribution network the operation sits.

Three properties make this safe, and they are the reason this is an
extension rather than a redesign:

1. **Renumbering existing bits is free.** `specificity()` is computed per
   evaluation, compared only against other scores within the same
   request, and **never persisted** — no column, no API field, no stored
   value depends on the numeric bit positions. Only the relative order is
   load-bearing.
2. **One bit per dimension, never shared.** A rule scoped
   `hierarchy_node_type = 'agent'` and a rule scoped `hierarchy_node = N` are
   therefore NOT a tie — the node-scoped rule strictly wins, which is the
   intended "custom limit for this one agent beats the level default."
3. **Conflict detection extends automatically.** Two CONFIGURABLE rules
   both scoped by the same `hierarchy_node_type` and nothing else, for the
   same `(limit_kind, time_window)`, are a genuine tie and return
   `ErrConflictingRules` — fail-closed, per §5. Operationally this means
   a retail operator must author exactly one per-level default per
   `(level, limit_kind, time_window)`; a second one takes that level's
   operations down rather than silently picking one. That is the correct
   behavior and must be stated in the partner-console UX, not discovered
   in production.

**(c) What this deliberately does NOT do: subtree/ancestor inheritance.**
`matches()` is exact equality on every dimension. A rule authored on a
Partner node therefore binds **that node only** — it does NOT
automatically bind that Partner's Super Agents, Agents and Cashiers. A
"rules inherit down the tree" model is a genuinely different matching
semantic and is **not adopted this stage**, because it cannot be added
without changing two things §12's extension discipline says an extension
must not change:

- `matches()` would go from equality to set-membership for one dimension
  (the request would have to carry its whole ancestor path, or
  `internal/risk` would have to traverse the retail domain's own
  hierarchy table — which it must not do, per the constraint §15c already
  imposes: `internal/risk` does not query another domain's schema).
- `specificity()` would need a *within-dimension* ordering (nearest
  ancestor wins) that the bitmask cannot express — a presence bit says
  "this dimension is scoped", not "scoped at depth 3 rather than depth
  1". Two matching ancestors at different depths would tie and fail
  closed, incorrectly.

Recorded as an `OPEN DECISION` in §24. Requirement #6 as written ("by
hierarchy level") is fully served by (a) + (b) without inheritance: a
per-level rule binds every node at that level regardless of parentage,
which is the more predictable model anyway. Inheritance must not be
retrofitted quietly.

**(d) Schema/ownership dependencies — this is `BLOCKED` on work this
specialist does not own.** `risk_rules.hierarchy_node_id` wants a
composite foreign key `(hierarchy_node_id, tenant_id)` into the retail
domain's own node table, mirroring the existing
`FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts
(id, tenant_id)`. That table does not exist and is not Risk's to create.
`hierarchy_node_type`'s permitted values likewise belong to the retail
domain's configuration, not to a `CHECK` constraint invented here (the
level set is per tenant/licence/jurisdiction per the requirement, so a
hard-coded `CHECK` would be wrong — an unconstrained `TEXT` scope field,
like the existing `provider_id` and `payment_method`, is the correct
shape). Per `docs/governance/change-control.md`, the migration that adds
these columns needs `architect` (table ownership), `security` (RLS/RBAC
review of `risk_rules`, which the existing RLS policies' `app.
player_account_id IS NULL` guard already makes non-trivial), and
`ledger-finance` where it touches float accounting. None of that happens
this stage.

### 21. Retail operations — what needs a new `Operation`, and what does not — `RECOMMENDATION` / `OPEN DECISION`

Per §16's extension model, and with the same honest bookkeeping §16
applied to `tournament_entry`/`marketplace_purchase`: **none of the
values below exist.** `migrations/0041`'s `operation` CHECK accepts
exactly `casino_launch`, `casino_bet`, `deposit`, `withdrawal`,
`sportsbook_bet`, `bonus_grant` and nothing else; `internal/risk/types.
go` declares exactly those six constants; `newCreateRiskRuleHandler`'s
own `RequireOneOf("operation", ...)` allowlist repeats those six
independently of the database; and `docs/api/openapi/platform-api.yaml`
carries the same enum twice. **No retail operation can be stored in
`risk_rules` today, and none may be created before all six steps of
§16's extension model are executed together in one authorized change.**
That is the intended fail-closed posture, not an oversight.

| Proposed value | Gates | Status | Why not an existing value |
|---|---|---|---|
| `retail_funding` | A float advance/replenishment between two hierarchy nodes (e.g. Super Agent → Agent), and its reversal/settlement counterpart | `NOT IMPLEMENTED` — DOCUMENTED ONLY, no constant, no migration, no HTTP validation, no OpenAPI entry | No existing operation describes a value movement between two non-player entities. Reusing `deposit` would retroactively rebind every `deposit` rule an operator has authored to agent float movements — the exact failure §15e rejects for `casino_bet`/`tournament_entry` |
| `retail_withdrawal` | A player taking cash out at a retail location | `NOT IMPLEMENTED` — DOCUMENTED ONLY. **RESOLVED YES** (Wave-2 review, F4) — see below | Distinct: the cash-out debits an agent's float, never the platform's payment rails (ADR 0035 §1.1's decision plus Invariant R1) — a genuinely different money movement from a rails withdrawal |
| `retail_deposit` | A player putting cash in at a retail counter | `NOT IMPLEMENTED` — DOCUMENTED ONLY. **RESOLVED YES on the identical criterion**, decided in the same change as `retail_withdrawal`, never differently for in and out | Same reasoning |

**Wave-2 review correction (F4, P1)**: an earlier draft of this table
left `retail_withdrawal`/`retail_deposit` as `CONDITIONAL`, pending the
decision rule below. `docs/decisions/0035-retail-agent-network-
accounting.md` §1.1 and Invariant R1 have since answered the rule's own
question definitively: a retail deposit/withdrawal is `Dr agent_float /
Cr player_cash` (or the inverse) and **never** touches
`psp_clearing`/`psp_reserve`/a bank rail. Both values are therefore
**unconditionally required**, not merely documented as conditional — an
earlier draft of ADR 0035 and this ADR both independently reached the
"yes" answer without either recording it as closed. The decision rule
below is retained as the reasoning trail, not as a still-open question.

**Naming, corrected to a single authoritative set (Wave-2 review, F4)**:
`risk` owns `Operation` naming (§16). An earlier draft of
`docs/architecture/26-retail-operations-architecture.md` §4.3 (endorsed
verbatim by `docs/decisions/0036-retail-hierarchy-rbac-and-audit.md`
§8.6) independently proposed `retail_counter_deposit`/
`retail_counter_payout`/`retail_float_advance`/`retail_settlement`, and
`docs/architecture/07-payments-architecture.md` §4 floated a third,
unrelated pair. The authoritative set, binding on all three documents, is
exactly the three rows above: **`retail_deposit`, `retail_withdrawal`,
`retail_funding`** — `retail_funding` covers both a float advance and its
settlement/reversal counterpart (no separate `retail_settlement` value;
ledger-finance's own `agent_float_transfer` naming in ADR 0035 §4 is
renamed to `retail_funding` to match). Doc 26 §4.3, ADR 0036 §8.6, and
payments §4 are corrected to this set in their own documents.

**Gap surfaced, not yet closed (Wave-2 review, F4)**: ADR 0035's
`agent_settlement` and `agent_commission_payout` transaction types — an
outbound real-money payment to an agent node, which ADR 0035 §11.3 itself
notes has no approval state machine designed — currently have **no**
corresponding `Operation` under any of the three values above, so
`risk.Evaluate` never sees that money movement at all. This is a real
gap, not a naming question: `risk` + `ledger-finance` must jointly decide
whether it needs a fourth `Operation` or is gated entirely by the
approval workflow ADR 0035 flags as undesigned — not resolved in this
stage.

**Decision rule that produced the "yes" answer above** (recorded so a
future reader does not re-derive it, exactly as §15f does for
`reward_redemption`): *does the retail cash-out/cash-in debit or credit
an AGENT'S FLOAT rather than the platform's payment rails?* If yes, it is
a different money movement with a different counterparty, a different
ledger mapping, a different settlement path and different thresholds, and
it needs its own `Operation` — the retail case is then strongly
analogous to `bonus_conversion` (§15a-ii), not to a channel variant. If
no — if a retail cash-out is the same withdrawal from the same player
wallet, merely handed over at a counter — then **no new `Operation`
should be added**, and the retail distinction is expressible today at
zero extension cost by narrowing an ordinary `withdrawal`/`deposit` rule
with `payment_method = 'retail_cash'` (or whatever value the payments
domain registers): `payment_method` is already a `Rule`/`RiskRequest`
scope field, is already in the `specificity()` bitmask, is unconstrained
`TEXT` in migration 0041, and is **not** in the HTTP handler's
`RequireOneOf` allowlist — so it needs no migration, no Go constant and
no OpenAPI change at all.

This is the mirror image of §15e's reasoning and the asymmetry is
deliberate: reuse of an existing `Operation` is WRONG when the amount or
the money movement differs in kind (a bonus conversion is not a bonus
grant), and RIGHT when it is the same money leaving the same wallet
through a different channel. **Resolved (Wave-2 review, F4)**: this ADR's
own first-branch reading was correct — `ledger-finance`'s ADR 0035 §1.1/
Invariant R1 has since stated the ledger treatment definitively (retail
cash always moves against `agent_float`, never a PSP rail), closing this
as a decided question, not an open one. `retail_deposit`/
`retail_withdrawal` are both required, per the table above.

**Consequences that hold whichever branch is chosen:**

- **`product` needs additive widening if retail is a product.**
  `risk_rules.product`'s CHECK accepts only `('casino','sportsbook',
  'payments','bonus')`, and `newCreateRiskRuleHandler` repeats that list.
  A retail rule can today only be authored `product`-unscoped or
  mislabeled. Same flag §16 already raised for gamification; the product
  taxonomy is the `architect`'s, not this ADR's.
- **`min_amount`/`max_amount` work unchanged** for every retail
  operation once its `Operation` value exists. `Rule.breach()`'s
  `min_amount`/`max_amount` cases compare `req.Amount` directly and are
  entirely generic over `Operation`. "Max €X per single float advance to
  an `agent`-level node" is therefore expressible with no new
  `LimitKind`.
- **`cumulative_amount` does NOT work for `retail_funding`, and this is
  the most important negative result in this section.** Requirement #6's
  most natural reading — "an Agent may fund at most €X **per day**" — is
  a `cumulative_amount` limit, and it is not expressible for node-to-node
  float movement even after the `Operation` value is added, for two
  independent reasons, either of which alone is fatal:
  1. `Rule.breach()`'s cumulative case looks `req.Operation` up in
     `operationLedgerTransactionTypes`, which contains exactly one entry
     (`casino_bet`), and returns `ErrUnsupportedCumulativeOperation` for
     anything else. Fixing that needs the ledger transaction type and its
     reversal counterpart (`operationLedgerRollbackTypes`) — additive
     facts `ledger-finance` must establish, never Risk's to invent (§16
     step 5, §15b).
  2. More fundamentally, the cumulative aggregation query is keyed by
     `le.player_account_id = $2` and `breach()` returns `ErrInvalidInput`
     when `req.PlayerAccountID` is `uuid.Nil`. **A float advance between
     two agent nodes has no player at all.** Cumulative aggregation keyed
     by hierarchy node does not exist and is a new capability, not a
     configuration. See §23.

  Until both are closed, a `cumulative_amount` rule on a retail funding
  operation would be storable (`CreateRule` still does not cross-validate
  `limit_kind`/`operation` compatibility — an already-recorded P2) and
  would **fail closed with an error on every matching request**. Treated
  as unavailable, not as working. Per-transaction funding caps: yes.
  Per-day funding caps: **not yet, and must not be described as
  delivered.**
- **Every retail call site owes §13's declaration**: resolve its own
  `RiskRequest` (including `JurisdictionCode` and `LicensingMode` from
  the retail domain's own trusted context per §9/§10 — a retail
  operation has no casino launch session to read from) and call
  `Evaluate` in the same transaction as its own state-changing effect,
  before commit. Adding an enum value without this step yields a
  configurable rule that nothing consults — §16 step 6's "the inversion
  most likely to be mistaken for done."

### 22. Responsible Gaming and self-exclusion are NOT a Risk concern in retail either — `ARCHITECTURAL DECISION`

Unchanged from §1 and §14, restated because directive requirement #16
names RG and self-exclusion in the same sentence as Risk and the two must
not be blurred by that adjacency:

- `internal/rg.EvaluateEligibility` remains the **sole** authority for
  self-exclusion, cool-off, account status and wallet status, for retail
  exactly as for online. `internal/risk` has no self-exclusion concept,
  never reads or writes `player_restrictions`, and **must not** acquire a
  "retail RG" rule kind, a `self_excluded` scope dimension, or a
  `LimitKind` that encodes an RG determination. A self-exclusion
  expressed as a `risk_rules` row would be an RG bypass wearing a Risk
  costume: it would be tenant-`risk_manager`-disablable (§8), effective-
  dated, and overridable by specificity — none of which is acceptable
  for a self-exclusion.
- A retail enforcement point **composes both**, in the same fixed order
  `internal/casino` already uses: RG first, short-circuiting, then Risk
  (§1). An RG denial returns before Risk is ever evaluated, so Risk's
  ALLOW can never "override" it. Ownership of the RG half sits with
  `identity-compliance`, not with this ADR, and the retail domain's RG
  integration is theirs to specify — this section only confirms the
  boundary holds for retail and refuses to move it.
- KYC/AML at retail is likewise `identity-compliance`'s, not Risk's.
  A `RISK_SIGNAL` rule may contribute a `REVIEW` outcome for a pattern
  that *looks* like structuring, but a Risk `REVIEW` is not an AML
  determination, a suspicious-activity report, or a KYC decision, and
  must never be recorded or reported as one.

**One retail-specific escalation, raised because requirement #16 is a
hard requirement and this is the realistic way it fails.** RG and
self-exclusion enforcement both presuppose an identified player
(assumption 4 above). Anonymous or bearer-instrument retail play — cash
over the counter with no `player_accounts` row — would structurally
bypass RG, self-exclusion, KYC thresholds and every player-scoped Risk
rule simultaneously, and **no amount of Risk configuration can
compensate**, because `matches()` has nothing to match on and the
cumulative aggregation has no player to key by. This ADR does not have
the authority to decide whether anonymous retail play is permitted (it is
a licensing/legal question per CLAUDE.md's "when to stop and ask", and a
jurisdiction-by-jurisdiction one). It is escalated to the Orchestrator
for `identity-compliance` and the human: **if anonymous retail play is in
scope, requirement #16 is not satisfiable as written and that must be
surfaced before the retail domain is designed, not after.**

### 23. Hierarchy-wide / subtree aggregate exposure — a real gap, disclosed — `OPEN DECISION`

Checked for, per the directive, against the same class of gap Stage
4H-A's review found for bonus campaign budgets (§15d) and cross-domain
player exposure (§17). **The same class of gap exists here, and this ADR
discloses it rather than claiming coverage.**

The question is whether the platform can bound *an entire hierarchy
subtree's* cumulative exposure — "total funding flowing through this
Partner's whole network in a day", "total retail cash-out across every
Cashier under this Super Agent this week" — as distinct from any single
node's own limit. **It cannot, today, and nothing in §19-§22 makes it
possible.** Three independent structural reasons:

1. **No cross-player aggregation exists.** The cumulative query nets
   `ledger_entries` filtered by `le.player_account_id = $2`. Every scope
   dimension on `risk_rules` NARROWS toward a single subject; none
   broadens one across subjects. This is the identical finding §15d
   recorded for campaign budgets.
2. **No cross-NODE aggregation exists**, which is a strictly larger gap
   than (1): a node-keyed cumulative sum does not exist at all, not even
   for a single node, because the aggregation is player-keyed and
   `ledger_entries` carries no hierarchy node (§21). Even "this one Agent
   funded €X today" is not computable by `internal/risk` today.
3. **No transitive/subtree traversal exists, and adding one is blocked by
   an existing constraint.** Summing a subtree requires walking the
   hierarchy, i.e. `internal/risk` reading the retail domain's own
   schema — which §15c already forbids for exactly this reason, and
   which would also reintroduce the ancestor-matching problem of §20(c).

**Consequence, stated plainly so nobody infers coverage from §20:**
per-level and per-node caps (§20) bound each node *individually*. They do
**not** bound a network's aggregate. A Partner with 200 Agents each
capped at €10k/day is capped at €2M/day in aggregate, and there is no
rule that can say otherwise. If network-wide exposure control is a real
business requirement — and for a retail credit/float model it very
plausibly is, since the platform is extending float — it is a genuinely
new capability requiring: a node-keyed (not player-keyed) aggregation, a
subtree-aware aggregation on top of that, a definition of what "exposure"
means for an outstanding float advance (settled? outstanding? credit
extended?), and the `exposure` `LimitKind` reserved in §4/§12. That is a
joint `ledger-finance` + Risk + `architect` design owned by a future
stage. **Recorded as an explicitly `OPEN DECISION`. It is NOT partially
approximated this stage, and a per-node rule must not be described
anywhere as if it provided network-wide exposure control.**

**The §15d guard, restated for retail — `ARCHITECTURAL DECISION`.** There
is a legitimate retail-side construct that must not be confused with the
gap above, and an illegitimate one that this ADR forbids under its §14/§19
authority:

- **Legitimate**: an agent's float *balance*, and the check "this node
  cannot advance more float than it holds." That is a balance/accounting
  invariant, owned by `ledger-finance` and enforced against the ledger in
  the guarded transaction — the retail analogue of `internal/casino`'s
  cash-balance lock. It is not a limit engine and Risk does not claim it.
- **Forbidden**: a retail-side counter of the form "this node may move at
  most €X per day/week", or "at most N transactions per hour", or any
  per-node or per-level threshold table in retail configuration code.
  **The moment a proposed retail-side constraint is a threshold compared
  against accumulated activity over a window, it is a limit engine under
  another name and must be a `risk_rules` row instead.** If §23's gap
  means the rule cannot be expressed yet, the correct response is that
  the capability does not exist yet — not a shadow implementation in the
  retail domain.

### 24. Open decisions, RBAC, and what remains impossible to configure after this section

**RBAC — `ARCHITECTURAL DECISION`, with one escalation.** Risk-
configuration write access stays where it is: `risk_config:manage` is
held only by `RoleRiskManager`, which is always tenant-scoped (migration
0011's CHECK), and is deliberately separate from Compliance, Finance,
Tenant Admin and Platform Admin per `docs/governance/ownership.md`.
**No retail hierarchy actor — Partner, Super Agent, Agent or Cashier —
has, or may be given as part of the retail domain's own work, any
risk-configuration write access.** **Resolved (Wave-2 review correction,
F14, P3)**: an earlier draft of this bullet hedged that retail actors
were "in all likelihood not `StaffRole`s at all." `docs/decisions/0036-
retail-hierarchy-rbac-and-audit.md` §4.1 has since decided this: a
cashier (and every other retail hierarchy actor) is a `staff_users` row
with a retail role, assigned to a node — not a distinct actor population.
Either way, a commercial hierarchy participant
authoring the limits that constrain it is the limit engine defeating
itself. If the product genuinely requires "a Super Agent sets its
sub-agents' limits", that is a **delegated-authoring model** — a new
permission, a bounded authoring scope (a node may only author rules for
its own descendants), and a hard ceiling the delegate cannot exceed
(which today would have to be a `HARD_LIMIT`, itself subject to §8's
unresolved "no role can create a genuinely platform-wide rule via HTTP"
limitation). Per CLAUDE.md and `docs/governance/change-control.md`, that
is **not this specialist's to add unilaterally**: it needs the
`architect` (actor model), `security` (RBAC/RLS review of `risk_rules`)
and the Orchestrator. Escalated, not designed here. Rule writes already
audit (`risk.rule_created`/`risk.rule_disabled` with actor, IP, user
agent, request id) and that requirement extends to any delegated path
without exception.

**Human/business decisions surfaced — explicitly NOT this ADR's to
make.** The actual limit **amounts and thresholds** — what a Cashier's
per-transaction cash-out cap is, what an Agent's daily funding ceiling
is, per level, per jurisdiction, per licensing mode — are commercial and
regulatory decisions requiring the human and, where a legal ceiling is
involved, `identity-compliance`. This ADR specifies only the mechanism by
which such a number becomes enforceable. No number appears anywhere in
§19-§24, and none should be inferred from the illustrative €X/€Y
placeholders above.

**Open decisions introduced by Stage 4H-B0:**

- **Do rules inherit down the hierarchy (subtree/ancestor matching)?**
  §20(c). Not adopted; adopting it later is a change to `matches()` AND a
  new within-dimension ordering in `specificity()`, i.e. the first
  genuine change to the precedence algorithm since Stage 4G. It must go
  through the `architect` and be recorded here, never retrofitted
  quietly.
- **Is subtree-aggregate exposure a real requirement?** §23. If yes, it
  is a future stage with `ledger-finance`, landing on the reserved
  `exposure` `LimitKind`.
- **Offline / degraded-connectivity retail.** Retail terminals
  historically operate with intermittent connectivity and
  store-and-forward settlement. §6/§19's fail-closed contract says an
  operation that cannot be evaluated cannot be authorized. If the
  business requires offline retail authorization, that is a deliberate,
  human, `security`-and-compliance-reviewed acceptance of unbounded
  exposure between reconnections — **not** a cached limit, **not** a
  locally-replicated rule set, and **not** an engineering shortcut. This
  ADR's position is that a local cache of `risk_rules` evaluated outside
  the guarded transaction is precisely what §19 and CLAUDE.md's
  "Redis/cache is never authoritative" rule forbid. Flagged, unresolved,
  and deliberately not designed around.
- **Is a hierarchy node ever itself a subject of a player-shaped limit?**
  E.g. a Cashier who is also a player. If a single human can hold both
  roles, `PlayerAccountID` and `HierarchyNodeID` on the same request
  refer to different aspects of the same person, and `matches()`'s
  conjunctive semantics handle it correctly — but the *audit* and
  *conflict-of-interest* questions are `identity-compliance`'s and
  `security`'s. Noted, not resolved.
- **Anonymous retail play** (§22) — escalated as a potential blocker on
  requirement #16 itself.

**What remains impossible to configure after this section** — the
load-bearing negative claim, stated the way §18 states it. Everything
below remains blocked by database CHECK constraint and by HTTP
validation, and **remains so after this stage**:

- `retail_funding`, `retail_withdrawal`, `retail_deposit` as `operation`
  values — rejected by migration 0041's CHECK and by
  `newCreateRiskRuleHandler`'s allowlist. A rule intended to govern an
  agent float advance **cannot be stored today**, which is correct: it
  would be a rule nothing evaluates.
- `hierarchy_node_type` / `hierarchy_node_id` as scope dimensions — the
  columns do not exist, the `Rule`/`RiskRequest` fields do not exist,
  `specificity()` has no bits for them, and the HTTP handler accepts no
  such input. **Requirement #6 is therefore `NOT IMPLEMENTED` in every
  respect**, not partially implemented.
- `retail` as a `product` value — rejected by the same two layers.
- `cumulative_amount` on any retail operation — unavailable on two
  independent grounds (§21): no ledger transaction-type mapping, and no
  node-keyed aggregation. "Per-day funding limit by hierarchy level", the
  most natural reading of requirement #6, is therefore **not expressible
  even after the `Operation` and scope dimensions are added** — it
  additionally needs §23's node-keyed aggregation. This is the single
  most important thing for the Orchestrator to carry forward.
- Subtree/network-wide aggregate exposure in any form (§23).
- Rule inheritance down the hierarchy (§20c).
- `count`/`velocity`/`exposure`/`loss` limit kinds, cross-operation
  aggregate exposure, campaign-level caps, points-denominated thresholds,
  calendar-aligned windows and session-scoped limits — all unchanged from
  §18.

The principle §4 established holds without exception through this stage
too: a rule the engine cannot evaluate must never be configurable in the
first place. §19-§24 are an extension-point specification; the extension
points stay closed until an authorizing stage opens them with all the
steps in §12 and §16 executed together in one change.

## Stage 4H-B0-R4: Sportsbook Risk Integration

Status of this section: **architecture-only, no implementation — added
Stage 4H-B0-R4.** The directive for this stage is explicit: no code, no
migrations. §25-§31 below add no `Operation` value, no `LimitKind`, no
scope dimension, no column, no migration, no HTTP validation, no OpenAPI
enum entry, and no enforcement wiring. This is a CONTRACT specification
and a set of extension points, in exactly the form §12 (`LimitKind`
extension model), §16 (`Operation` extension model), §14-§18 (Bonus/
Gamification) and §19-§24 (Retail) already established. Nothing in
§25-§31 changes `risk.Evaluate`'s signature, `risk_rules`' shape, the
precedence algorithm, the fail-closed contract, or any existing
enforcement call site. Every prior stage's decision above stands
unmodified.

**Sources read for this section.** `docs/architecture/09-sportsbook-
architecture.md` (Stage-0 proposal — bet placement, settlement, void,
partial settlement, cashout, and re-settlement-after-correction each
named as distinct ledger events, never a single "resolve bet" operation)
and `internal/risk/types.go` / `internal/risk/evaluator.go` at this
stage's own commit (not from memory of a prior stage's claims — every
field, constant and query below is checked directly against that code).
`docs/decisions/0038-sportsbook-accounting-and-ledger-integration.md`
does **not exist in this repository as of this section's authorship**
(`ledger-finance`'s parallel work); this section therefore proceeds on
doc 09's own lifecycle framing per the directive's explicit fallback
instruction, and every claim below that would depend on ADR 0038's actual
ledger-transaction-type choices is named as depending on it, not guessed.
The sportsbook domain model itself (Bet/BetLeg/BetSlip/Market/Selection/
Event/Settlement/Cashout/Exposure/Liability — owned by `sportsbook`, in
parallel this stage) is treated exactly as §14 treated the unfrozen Bonus
Grant shape and §19 treated the unfrozen retail hierarchy: described
abstractly ("however the sportsbook domain represents a Market/
Selection"), committing to nothing about its internal shape, only to
where `risk.Evaluate` sits relative to it.

### 25. The hard rule, restated and specialized for Sportsbook — `ARCHITECTURAL DECISION`

The rule established for Casino in §1/§2, generalized in §13, specialized
for Bonus/Gamification in §14, and specialized for Retail in §19 applies
to the sportsbook domain **unchanged, unweakened, and without
reinterpretation for this domain**:

- Every exposure-affecting decision in the sportsbook domain MUST consult
  `internal/risk.Evaluate`. "Exposure-affecting" means the same thing it
  means in §13: capable of affecting player financial exposure, wagering
  exposure, payment exposure, regulatory exposure, or platform risk.
- The sportsbook — whether built on an external provider's widget/feed
  integration or, later, an in-house trading stack (doc 09's build/buy
  line; this section does not depend on which one) — **must not build a
  second limit or risk engine.** No threshold comparison, no per-player
  stake-cap table, no "max daily sportsbook loss" counter, no velocity
  check, may live inside `internal/sportsbook` (or an equivalent package
  name — not yet created) as its own structure. It authors `risk_rules`
  rows and interprets `ALLOW`/`DENY`/`REVIEW`; it never re-implements the
  comparison. This is the identical constraint `internal/casino` already
  operates under and the identical one `internal/payments`, the Bonus
  Engine and Gamification, and Retail are all already bound by. It binds
  equally whether the actual stake/odds/settlement computation happens
  inside the platform or inside a third-party provider's own systems —
  the provider computes the bet's terms; the platform still owns whether
  that bet is *permitted* to be placed/settled/cashed out, exactly as
  §12/CLAUDE.md's provider-abstraction rule already states for every
  other vendor-backed capability ("the vendor supplies the API; the
  platform still owns... the state machine").
- The call happens **inside the same database transaction as the
  state-changing effect it gates, before that effect commits** (§13,
  §14, §19). PostgreSQL inside the guarded transaction is the only
  authoritative correctness boundary for a cumulative check. A stake
  evaluated in one transaction and locked in another is not gated; it is
  merely advised. No cache, and no counter maintained by the sportsbook
  domain (in-house or provider-adjacent), may stand in for that read.
  This applies with equal force to the **widget/iframe** integration
  shape doc 09 recommends starting with: even though the provider renders
  the betting experience, the platform's own wallet debit/credit for
  stake and settlement is still a platform-owned ledger write, and that
  write is the one `risk.Evaluate` gates — a provider-hosted UI is never
  a reason to skip the platform's own transaction-boundary discipline.
- A non-nil error from `Evaluate` is a DENY at every sportsbook call site,
  per §6. There is no sportsbook-specific softening — not for a
  provider-widget bet, not for a live/in-play bet under latency pressure,
  not for a cashout offer about to expire. An unavailable evaluator, a
  malformed rule, conflicting rules, or a missing scope value must never
  resolve to "place/settle/pay it anyway."
- Risk does NOT absorb Responsible Gaming for sportsbook either — see §29.

### 26. New `Operation` values needed for sportsbook — `RECOMMENDATION`, documented only

**`sportsbook_bet` already exists** — `internal/risk/types.go`'s
`Operation` const block, migration 0041's `operation` CHECK, `internal/
httpserver/risk_handlers.go`'s `RequireOneOf` allowlist, and both OpenAPI
`operation` enum occurrences all already accept it (verified directly,
this stage — not assumed from §13's table). No new proposal is made for
it; it already covers **bet placement** (the stake debit at the moment a
bet is accepted), exactly as `casino_bet` covers a casino stake. `risk_
rules.product` also **already** accepts `'sportsbook'` (migration 0041's
`product` CHECK: `casino, sportsbook, payments, bonus`) — unlike Bonus/
Gamification (§16) and Retail (§21), **no `product` CHECK widening is
needed for sportsbook.** This is the single most important positive
result of this section: sportsbook bet placement requires zero Risk-side
schema or code change to become risk-gated once `internal/sportsbook`
exists and calls `Evaluate`.

Checked against doc 09's own named lifecycle events — bet placement,
settlement, void, partial settlement, cashout, and re-settlement after a
market correction — plus cancellation and reversal per this stage's
directive, using the identical decision rule §15a-ii/§15e/§21 already
apply: *does the money movement, and the amount it is computed against,
differ in KIND from an existing operation, such that reusing that
operation's `Operation` value would retroactively rebind rules never
intended to govern it?* Reuse is wrong when the answer is yes (a bonus
conversion is not a bonus grant, §15a-ii; a tournament entry is not a
casino bet, §15e); reuse is right when the money is the same wallet
movement through a different channel, and needs no new plumbing at all
(a retail cash withdrawal is the same withdrawal, §21).

**Two new values are proposed, and two lifecycle events need none:**

| Lifecycle event (doc 09) | Needs its own `Operation`? | Reasoning |
|---|---|---|
| Bet placement | No — **`sportsbook_bet` already exists** | Covers it unchanged; see above |
| Settlement (full) | **Yes — `sportsbook_settlement` (proposed)** | Settlement credits the player with the *realized* return on a graded outcome. That amount is computed from the bet's odds and doc 09's stated "potential return", not the stake — it can be many multiples of the stake by design (a long-odds accumulator). Reusing `sportsbook_bet` would silently bind every existing stake-sized `max_amount`/`min_amount` rule to a *payout*, which is the exact `bonus_grant`-vs-`bonus_conversion` failure mode §15a-ii rejects: the released amount legitimately and routinely exceeds the value that produced it, so a stake-scoped rule and a payout-scoped rule must be independently authorable |
| Partial settlement (bet-builder legs) | No — **reuses `sportsbook_settlement`** | Doc 09 states partial settlement is "each a distinct ledger event", but it is the *same policy question* §15a-ii's activation reasoning already resolved: a partial settlement is still "crediting realized winnings at bet resolution", just for a subset of legs/stake at a given instant. A distinct operation per settlement *shape* would force every settlement-payout rule to be authored twice (full and partial) with one forgotten copy a silent gap — exactly the trap §15a-ii names |
| Re-settlement after a market correction | No — **reuses `sportsbook_settlement`** | Doc 09: "requiring re-settlement as its own event, not a silent balance edit." The rules in force at the CORRECTED settlement's own instant are the rules that must govern it — identical reasoning to §15a-ii's "the rules in force at *that* moment... are the rules that must govern it" for activation. It is the same money question re-evaluated later, not a new kind of money movement |
| Cashout | **Yes — `sportsbook_cashout` (proposed)** | A cashout credits the player with a *live, provider/platform-computed buy-back price* before the bet naturally resolves — not a graded outcome. Its risk profile is a different kind of risk from settlement's: settlement risk is "was the payout correctly graded and is it unusually large" (a regulatory/integrity concern on a realized result); cashout risk is "is this pricing/timing being exploited" (mid-event arbitrage, latency abuse, collusion between a bettor and a confederate feeding live information) — an industry-recognized distinct fraud surface. Reusing `sportsbook_settlement` would conflate two genuinely different risk postures under one rule set, the same failure §15a-ii/§21 reject; a cashout-specific `max_amount`/`REVIEW` signal must be authorable without also constraining ordinary settlement payouts, and vice versa |
| Void (market/event cancelled, stake returned) | No — **not a Risk operation at all; a ledger reversal, like `casino_rollback`** | A void returns exactly the original stake — it creates no new exposure, it *retracts* an existing one, symmetric to the debit `sportsbook_bet` already gated. This is structurally identical to `casino_rollback`, which today is not its own `Operation` and is never itself evaluated by `risk.Evaluate` — it exists only as `operationLedgerRollbackTypes["casino_bet"]`, netted against `casino_bet`'s own cumulative-amount usage so a voided round doesn't permanently consume capacity. The sportsbook analogue is `operationLedgerRollbackTypes["sportsbook_bet"] = "<sportsbook void ledger transaction_type>"` — a fact `ledger-finance` must establish (ADR 0038), never a new `Operation` |
| Cancellation | No — **same as void**, for the same reason | Doc 09 does not distinguish cancellation from void as a separate money shape; both retract a stake rather than creating new exposure. If `ledger-finance`'s ADR 0038 gives cancellation a *different* `ledger_transactions.transaction_type` than void, both types are added to `operationLedgerRollbackTypes["sportsbook_bet"]`'s reversal set — still no new `Operation` |
| Reversal (general correction, e.g. clawback of an incorrectly-paid win) | No — **reuses `sportsbook_settlement`**, per the re-settlement row above | A correction that pays *less* than originally settled is the same re-settlement event, evaluated at the corrected amount; CLAUDE.md's own "corrections are compensating entries, never edits" already governs how this is *posted* — this ADR only adds that the compensating entry's gate, if the correction is a NEW payout, is `sportsbook_settlement` re-evaluated, not a bespoke path |

Following §16's own extension model exactly (six steps: migration CHECK,
Go constant, HTTP allowlist, all three OpenAPI enum occurrences per
§16a's own correction, ledger transaction-type mapping if `cumulative_
amount` must work for it, enforcement call site) — **none of the six
steps are executed this stage** for either proposed value. Both remain
unstorable in `risk_rules` today (migration 0041's `operation` CHECK
rejects everything outside the existing six), which is the intended
fail-closed posture, not an oversight.

**Naming discipline note, mirroring §21's Wave-2 correction (F4).** If
`docs/architecture/09-sportsbook-architecture.md`'s rewrite (in progress
in parallel by `sportsbook`) or a future ADR 0038 independently proposes
different names for these two checkpoints (e.g. a "resolve" or "grade"
verb for settlement, a "buyout" noun for cashout), **`risk` owns
`Operation` naming per §16**, exactly as it already does for Bonus and
Retail — `sportsbook_settlement`/`sportsbook_cashout` are this ADR's
authoritative proposal, and any other document's naming must be
reconciled to it, not the reverse, following the exact correction §21
already had to make for `docs/architecture/26-retail-operations-
architecture.md` and `docs/decisions/0036`.

**One gap surfaced, not closed, mirroring §21's `agent_settlement` gap.**
An operator-side correction that pays an agent/partner (e.g. a provider
revenue-share settlement, or a manual trading-desk adjustment credited
directly to the platform's own account rather than a player's) is
outside doc 09's player-facing lifecycle entirely and has no candidate
`Operation` above. Whether such a movement needs its own gate, or is
purely a back-office/ledger concern outside Risk's player-protection
scope, is not resolved here — flagged in §30.

### 27. Sportsbook `RiskRequest` dimensions — existing coverage vs. genuinely new dimensions — `RECOMMENDATION`

Checked field-by-field against `internal/risk/types.go`'s actual current
`RiskRequest`/`Rule` shape (`TenantID`, `BrandID`, `PlayerAccountID`,
`JurisdictionCode`, `LicensingMode`, `Product`, `Operation`, `ProviderID`,
`GameID`, `AssetCode`, `PaymentMethod`, `Amount`, `CorrelationID`), not
assumed.

**Already covered, zero extension needed:**

| Directive dimension | Existing field | Note |
|---|---|---|
| Player | `PlayerAccountID` | Unchanged; resolved server-side by the caller, never client-supplied, per every existing field's contract |
| Tenant | `TenantID` | Unchanged |
| Brand | `BrandID` | Unchanged |
| Jurisdiction | `JurisdictionCode` | Unchanged — resolved from the sportsbook domain's own trusted context per §9's own generalization ("whatever resolves a `RiskRequest` for that operation supplies `JurisdictionCode` from whatever source THAT operation's own authoritative context provides"); a sportsbook bet has no casino launch session to read from, exactly as §15a and §21 already state for bonus and retail |
| Asset | `AssetCode` | Unchanged; a sportsbook stake/payout is minor units of a registered asset like any other |
| Operation | `Operation` | `sportsbook_bet` (exists) plus `sportsbook_settlement`/`sportsbook_cashout` (§26, proposed) |
| Stake | `Amount` | Unchanged — `Amount` is already operation-generic ("minor units for THIS operation"); a sportsbook stake at placement, a realized payout at settlement, and a buy-back price at cashout are each just `Amount` for their own `Operation`, exactly as `bonus_conversion`'s released amount reuses `Amount` rather than needing its own field (§15a-ii) |
| Provider | `ProviderID` | **Already has precedent — reusable, not new.** `ProviderID` is unconstrained `TEXT` (migration 0041), already used to scope a casino rule to one game-content vendor. A sportsbook widget/feed vendor (Altenar/BetBy/Digitain, per doc 09's own examples — named descriptively, not as a real commercial relationship, per this stage's own constraint) is the identical kind of "which vendor" scope. No schema change, no HTTP change |

**Genuinely new — no current analogue, would require additive fields:**

`sport`, `competition`, `event`, `market`, `selection` have **no existing
field to map to.** `GameID` is not a stand-in: it is declared `game_id
UUID REFERENCES casino_games (id)` in migration 0041 — a literal foreign
key into the casino game catalogue. Reusing it for a sportsbook Event or
Selection would either violate that FK outright or require weakening it
to a bare `UUID` with no referential integrity, either of which is a
worse outcome than adding sportsbook-specific fields, and it is exactly
the kind of misleading dimension-reuse this ADR's own Wave-2 review
corrected elsewhere (§20's `hierarchy_node_type`/"level" naming
correction, §21's `Operation`-naming correction) — reuse must never
create false coupling between two domains' unrelated entities.

**How they would extend `RiskRequest`/`Rule` — additively, mirroring §20
exactly, not designed here.** Each of the five is a categorical-or-entity
scope dimension of the identical shape every existing one already has:
a plain field, empty/nil on the RULE side meaning "applies regardless of
that dimension" (`matches()`'s existing general contract, unchanged), and
an entity ID or a code, resolved server-side by the caller from the
sportsbook domain's own trusted context, never client-supplied. Per §12/
§16/§20's own extension discipline, adding any of them needs: an additive
migration column (with, per §20(a)'s own `hierarchy_node_type` precedent,
a possible `CHECK (dimension IS NULL OR tenant_id IS NOT NULL)` guard if
the value is tenant-authored rather than a platform registry row — see
below), the corresponding `Rule`/`RiskRequest` Go field, a new bit in
`specificity()`'s bitmask (renumbering existing bits is free per §20(b)(1)
— specificity scores are computed per evaluation and never persisted), and
an HTTP/OpenAPI change **only if** the dimension needs an allowlist
(most would not — `ProviderID`/`PaymentMethod`'s free-form `TEXT`
precedent, not `Product`/`LicensingMode`'s fixed-vocabulary precedent, is
the likely shape, since sports/competitions/events/markets/selections are
each large, frequently-changing catalogues, not small fixed enums).
**None of this is implemented or even named as fixed field names this
stage** — `SportCode`/`CompetitionID`/`EventID`/`MarketID`/`SelectionID`
above are illustrative only, to show the shape fits without redesigning
`Evaluate`, and MUST be reconciled to whatever `sportsbook`'s own parallel
domain-model work actually names these entities before any of it is
built — this ADR does not redesign that domain model, per this stage's
own scope.

**Two design questions this section deliberately does not resolve, flagged
for whoever eventually implements this (not this stage):**

1. **Platform registry or tenant-authored?** `Product`/`LicensingMode` are
   fixed platform vocabularies (no `CHECK (... IS NULL OR tenant_id IS
   NOT NULL)` guard needed). `HierarchyNodeType` (§20) is tenant-authored
   and needed that guard specifically because doc 26 §1.4 allows two
   tenants' same code to mean different things (a Wave-2, P1 correction
   after an earlier draft got this wrong). Whether "sport"/"competition"/
   "market" are platform-registry rows (more likely — sports and markets
   are largely universal across operators, unlike a retail hierarchy's
   tenant-invented org chart) or tenant-authored codes is the sportsbook
   domain model's decision, not Risk's, and determines whether the
   §20(a)-style guard is needed at all.
2. **One field per level, or type-plus-instance per level?** §20(a) itself
   split ONE conceptual dimension (hierarchy position) into TWO fields
   (`HierarchyNodeType` categorical, `HierarchyNodeID` specific-entity)
   because both a categorical rule ("agents get X") and an entity-specific
   override ("this one agent gets Y") were real, distinct requirements.
   "Market" plausibly needs the identical split (a market *type* — e.g.
   "1X2", "over/under" — versus one specific market *instance* on one
   specific event) and possibly "selection" does too. This section
   deliberately does not decide that split, because it depends on
   entity shapes `sportsbook`'s parallel domain-model work owns, not on
   anything `internal/risk` needs to pre-judge.

**`exposure`, `velocity`, and `cumulative activity`, addressed precisely
against this ADR's own already-disclosed §17 gap, as the directive
requires — not hand-waved:**

- **Cumulative activity maps to the EXISTING `cumulative_amount`
  `LimitKind` mechanism, and is NOT a new gap in kind — it is the same
  missing ledger-mapping fact every other new operation already needs.**
  `Rule.breach()`'s cumulative case (`internal/risk/evaluator.go`) sums
  `ledger_entries` over a rolling window for whatever `ledger_
  transactions.transaction_type` `operationLedgerTransactionTypes[req.
  Operation]` names, and today that map has exactly one entry
  (`casino_bet`). A `cumulative_amount` rule on `sportsbook_bet` (e.g.
  "no more than X staked per rolling day") or `sportsbook_settlement`
  (e.g. "no more than X paid out per rolling day", a plausible
  responsible-gambling-adjacent control) is **storable today** once
  §26's `Operation` values exist, but will `ErrUnsupportedCumulativeOperation`
  on every matching request until `ledger-finance`'s ADR 0038 supplies
  `sportsbook_bet`'s (and, if adopted, `sportsbook_settlement`'s) own
  `transaction_type` and its rollback/void counterpart — the identical
  gap pattern already disclosed for `bonus_grant` (§15b) and
  `retail_funding` (§21), not a new architectural problem.
- **Exposure is a GENUINELY DIFFERENT, sportsbook-specific gap from §17's
  cross-domain gap — not automatically the same mechanism, and not
  automatically solved by closing cumulative-activity above.** §17's
  disclosed gap is that `Evaluate` cannot express a rule spanning
  MULTIPLE operations ("total exposure across bonus + casino +
  sportsbook"), because `listEffectiveRules`/`matches()` both key on
  exactly one `Operation`. Sportsbook's own trading concept of "exposure"
  (doc 09: "An open-bet record carries potential return"; the parallel
  domain model's own `Exposure`/`Liability` entities) is a **different
  kind of quantity than either §17's or the existing `cumulative_amount`
  mechanism can express, even within sportsbook alone**, for a structural
  reason neither §17 nor §21/§23 needed to name: `cumulative_amount`
  sums **realized, already-posted ledger entries over a trailing time
  window** ("how much has this player staked in the last 24 hours").
  "Exposure" in the trading sense is **the platform's current
  outstanding contingent liability on OPEN, unsettled bets right now** —
  a state query over live positions and their potential (not yet
  realized) payout, not a windowed sum of past transactions. Doc 09
  itself distinguishes these: the stake move to `player_locked` at
  placement is a real ledger fact `cumulative_amount` could in principle
  aggregate; the "potential return" an open bet carries is explicitly
  described as something the open-bet record carries, not necessarily
  anything posted to the ledger at all. A `LimitKind` that could express
  "this player's total potential payout across every currently-open bet
  must not exceed X" would need to aggregate a **contingent, computed
  quantity that may never become a ledger entry** (most open bets lose
  and post no payout) — a shape none of `min_amount`/`max_amount`/
  `cumulative_amount` have, and one §12/§4 already anticipated in the
  abstract ("an exposure limit's own definition of open exposure") but
  never had a concrete consumer for until now.
- **Does sportsbook make §17's cross-domain gap MORE urgent, or is it
  separate?** Both, precisely: **separate as a requirement, but coupled
  as a design problem if the cross-domain requirement is ever adopted.**
  Sportsbook's own single-operation, single-domain open-position exposure
  (the bullet above) does not require solving §17's harder cross-domain
  aggregation to be useful on its own — a future `exposure` `LimitKind`
  scoped to `sportsbook_bet`/`sportsbook_settlement` alone would already
  answer a real trading/RG-adjacent question ("this player's live
  sportsbook liability") without touching casino or bonus at all. But if
  the business need is ever framed as this ADR's own §17 language already
  anticipates — "this player's total exposure across bonus + casino +
  sportsbook" — sportsbook makes that STRICTLY HARDER than §17 already
  disclosed, not merely more urgent: §17 was written against operations
  whose "exposure" is at least always a realized or grantable ledger
  quantity (a settled casino stake, an issued bonus liability). Sportsbook
  introduces a genuinely unrealized, contingent quantity into the same
  proposed sum, so a future cross-domain `exposure` design must first
  answer how to make a settled ledger amount and an open, possibly-never-
  realized contingent liability commensurable before they can be summed
  at all — a harder question than §17 flagged, not a restatement of it.
  **Recorded as a refinement of the existing §17 OPEN DECISION** (added to
  §30 below), not a new one: the `exposure` `LimitKind` reserved in §4/§12
  remains the landing point either way, and remains a joint `ledger-
  finance` + Risk + `sportsbook` design for a future stage, never invented
  speculatively here.
- **Velocity is unimplemented exactly as it already is for Bonus (§15c)
  — sportsbook is simply its most natural motivating case so far, not a
  reason to add it early.** "No more than N bets per minute" (a genuine,
  industry-standard control against latency-arbitrage/courtsiding on
  live/in-play markets) is a COUNT-over-a-time-window, identical in shape
  to the bonus-frequency gap §15c already named and rejects faking via
  `cumulative_amount`. It requires the same reserved `count`/`velocity`
  `LimitKind` (§4/§12), the same open "what does it count" question §17
  already records (ledger rows vs. a domain table's rows — sportsbook's
  in-play bet-slip submissions may not even post a ledger entry until
  accepted, making this arguably harder than the bonus-grant case §17
  poses it against), and is **NOT added this stage**, following the
  identical discipline.

### 28. Market/trading exposure management — a Sportsbook Engine concern, not a Risk & Limits Engine concern — `ARCHITECTURAL DECISION`

Applying this ADR's own established discipline for exactly this kind of
question — the one already applied to Bonus campaign budget caps (§15d)
and generalized for Retail network-wide exposure (§23) — rather than
assuming everything sportsbook-shaped belongs in Risk because the
directive's own framing ("sportsbook-trading/risk") juxtaposes the two
words.

**The test this ADR has consistently applied**: a constraint keyed by a
single PLAYER (or a specific player-scoped entity like a hierarchy node
acting as a funding subject) is a limit-engine concern and MUST be a
`risk_rules` row. A constraint keyed by an aggregate that spans MANY
players/subjects, expressing the platform's OWN commercial or operational
position rather than a bound on any one player's activity, is NOT a
limit-engine concern and belongs to the owning domain's own object —
§15d's campaign budget cap and §23's hierarchy-subtree exposure cap are
both already-decided instances of the second case.

**Market/selection/event-level liability management — odds-setting,
margin adjustment, and market suspension in response to how much money is
riding on a given outcome ACROSS ALL PLAYERS — is the second case, and is
therefore the Sportsbook (trading) Engine's own internal operational
concern, not gated by `risk.Evaluate`:**

- It is keyed by a MARKET or SELECTION (a betting object), aggregated
  across every player who has staked on it — never keyed by one player.
  This is structurally identical in shape to §15d's campaign budget
  ("this campaign may issue at most X across ALL players") and §23's
  subtree cap ("this Partner's whole network may move at most X"): every
  scope dimension `risk_rules` has narrows toward a single subject
  (player, tenant, brand, node); none of them broadens one across
  subjects, and this ADR's position (§15d, §23) has been consistently
  that adding cross-subject aggregation would change what a "limit" means
  in this engine, not extend it.
- The DECISION a trading system makes in response to that aggregate
  (suspend the market, move the price, cut the maximum stake it will
  accept on a selection going forward) is not an `ALLOW`/`DENY`/`REVIEW`
  verdict on one player's specific pending operation — it changes what
  the platform is willing to OFFER to the NEXT bettor, before any request
  from them exists to evaluate. `risk.Evaluate`'s entire contract is a
  per-request decision boundary (§2); a decision that has no specific
  `RiskRequest` to gate is outside that boundary by construction, not a
  gap in it.
- It CONSUMES the same underlying fact ("how much is staked on this
  outcome") that the sportsbook domain's own `Exposure`/`Liability`
  entities (per this stage's directive framing) will own — exactly as
  §15d's campaign object consumes ledger facts without being a Risk
  concern, and exactly as §23's legitimate float-balance check ("this
  node cannot advance more float than it holds") is a `ledger-finance`-
  owned accounting invariant, not a Risk one. Consuming risk-adjacent data
  is not the same thing as being gated by Risk.

**What stays a genuine `risk_rules` concern, and must not be confused
with the above — the split, restated in the sportsbook domain exactly as
§15d states it for Bonus:**

- **A per-player stake, payout, or cashout limit is a Risk rule**, even
  when the THRESHOLD for that specific player was set using trading
  intelligence (a known professional/"sharp" bettor given a lower max
  stake than a recreational player; a player flagged for suspicious
  betting patterns given a reduced max payout). The rule is still scoped
  by `PlayerAccountID` — a single subject — and belongs in `risk_rules`
  exactly like any other player-specific override (§5's own worked
  example: player=50 beats brand=200). Who or what INFORMS the threshold
  value (a trading analyst's judgement, an automated model) is irrelevant
  to where the CHECK lives; the check itself is always Risk's.
- **A market/selection-scoped rule that narrows toward a specific,
  identifiable market or selection (§27's proposed dimensions) is also a
  Risk rule**, provided it is still evaluated per-request against a
  specific player's specific bet (e.g. "no player may stake more than X
  on selection Y" is expressible with §27's proposed `SelectionID`
  dimension, unchanged from how `GameID` scopes a casino rule today). The
  line is not "does it mention a market" — it is "is it evaluated as one
  player's request against a threshold, or as a standing aggregate across
  every player."

**The non-negotiable guard, restated verbatim from §15d/§23's own
authority under §14/§19**: **a market/selection liability counter must
never be used to express or substitute for a per-player limit.** The
moment a proposed trading-side constraint is keyed by PLAYER rather than
by market/selection, it is a limit engine under another name and must be
a `risk_rules` row instead — the identical guard already stated for
campaign budgets and hierarchy subtrees, extended here rather than
re-derived, per this ADR's own consistency discipline.

### 29. Responsible Gaming relationship for sportsbook — pointer only, unchanged — `ARCHITECTURAL DECISION`

Unchanged from §1, §14, and §22 — restated briefly here because sportsbook
is a real-money wagering surface and the relationship must be confirmed
for it explicitly, not left to be inferred from casino's precedent alone.
`internal/rg.EvaluateEligibility` remains the **sole** authority for
self-exclusion, cool-off, account status and wallet status for sportsbook
exactly as for casino, bonus, and retail. `internal/risk` acquires no
self-exclusion concept, no `player_restrictions` read or write access,
and no RG-flavored `RuleKind`/`LimitKind` for sportsbook specifically. A
sportsbook enforcement point (bet placement, settlement, cashout) composes
BOTH, in the same fixed order every other domain already uses: RG first,
short-circuiting; Risk second (§1). The detailed sportsbook-specific
RG/KYC/AML integration design (deposit limits, loss limits, time-outs,
and self-exclusion as they apply to live/in-play betting specifically) is
`identity-compliance`'s to write in `docs/decisions/0034-bonus-
gamification-rg-kyc-identity-integration.md` (or a sportsbook-specific
successor, per that specialist's own scoping) this same stage, in
parallel — this ADR does not redesign that relationship and confirms
sportsbook needs no exception to it.

### 30. Open decisions introduced or refined by Stage 4H-B0-R4

- **Sportsbook exposure as a cross-domain `exposure`-`LimitKind` design
  question — refines §17, does not replace it.** As detailed in §27: a
  future `exposure` `LimitKind` design must define open-position,
  contingent (not-yet-ledger-posted) liability before it can be summed
  with settled casino/bonus amounts. Owned jointly by `ledger-finance`,
  Risk, and `sportsbook`, landing on the `exposure` `LimitKind` already
  reserved in §4/§12. Not resolved here.
- **Is "sport"/"competition"/"market" a platform-registry taxonomy or
  tenant-authored?** §27. Determines whether the §20(a)-style
  `CHECK (... IS NULL OR tenant_id IS NOT NULL)` guard is needed for any
  new scope dimension. Owned by the sportsbook domain model, not Risk.
- **Does "market" need a type-plus-instance split, the way `Hierarchy
  NodeType`/`HierarchyNodeID` did for retail (§20(a))?** §27. Same answer:
  depends on entity shapes not yet frozen; not resolved here.
- **Does an operator-side payment to a provider/partner (e.g. a revenue-
  share settlement) arising from sportsbook activity need its own
  `Operation`, or is it outside Risk's player-protection scope entirely?**
  §26's closing gap. Mirrors §21's disclosed-but-unresolved
  `agent_settlement`/`agent_commission_payout` gap in shape; not resolved
  here, and owned jointly by `risk` and `ledger-finance` once ADR 0038
  exists.
- **Velocity ("bets per minute") for live/in-play markets** — §27, same
  unresolved shape as §15c/§17's "what does a `count` limit count",
  sportsbook is simply its sharpest motivating case yet. Not resolved
  here.

### 31. Confirming no other Risk-side gap blocks sportsbook architecture closure

Re-using this ADR's own established self-check discipline (§16a's "is
anything else on Risk's side blocking..." section) rather than assuming
completeness. Checked against doc 09 and the directive's own six lifecycle
events, this stage's actual verified repository state (not memory of a
prior stage's claims):

- **Bet placement needs ZERO Risk-side changes.** `sportsbook_bet` is a
  real `Operation` constant, accepted by migration 0041, present in the
  HTTP allowlist and both OpenAPI enums, TODAY. `risk_rules.product`
  already accepts `'sportsbook'` — unlike every other domain integration
  recorded in this ADR (Bonus §16, Gamification §16, Retail §21), **no
  `product` CHECK widening is needed here.** `min_amount`/`max_amount`
  with `TimeWindow: transaction` work unmodified for a stake, with full
  HARD_LIMIT/CONFIGURABLE_LIMIT precedence and player-override behavior
  (§5), the moment `internal/sportsbook` exists and calls `Evaluate`
  before locking a stake.
- **Settlement and cashout are NOT first-slice-deferrable, mirroring
  §16a's finding that `bonus_conversion` sat on the Bonus Engine's own
  critical path.** A sportsbook that can accept bets but never pay out a
  win, or never offer a cashout the product requires, is not shippable.
  §26's two proposed `Operation` values (`sportsbook_settlement`,
  `sportsbook_cashout`) should therefore be treated by whoever authorizes
  sportsbook implementation as a near-term, load-bearing dependency —
  exactly the correction §16a made explicit for `bonus_conversion` after
  an earlier draft understated it — not as a "nice to have later" the way
  velocity or exposure genuinely are.
- **`cumulative_amount` on any sportsbook operation is a known,
  correctly-characterized gap, not a blocker.** It fails closed
  (`ErrUnsupportedCumulativeOperation`) exactly as it does for `bonus_
  grant` (§15b) and `retail_funding` (§21) until `ledger-finance`'s ADR
  0038 supplies the ledger transaction-type mapping. Restricting initial
  rule authoring to `min_amount`/`max_amount` with `TimeWindow:
  transaction` avoids it entirely, the identical mitigation §16a already
  confirmed sufficient for Bonus's first slice.
- **The new hierarchy dimensions (§27) are NOT needed for architecture
  closure or a first slice.** Player/tenant/brand/jurisdiction/asset/
  provider/operation/amount already fully support a first slice's stake
  and payout limits with zero schema change. Sport/competition/event/
  market/selection become necessary only once market- or selection-level
  limits are wanted, which is a later refinement, not a blocker.
- **`exposure`/`velocity` remain correctly unimplemented, per §18/§24's
  own standing principle**, restated here rather than re-derived: a rule
  the engine cannot evaluate must never be configurable in the first
  place, and neither is needed for a first slice to function.
- **RBAC introduces no sportsbook-specific gap.** `risk_config:manage`
  stays with `RoleRiskManager` exactly as for every other domain; unlike
  Retail (§24), sportsbook introduces no new class of actor (an "agent"
  or "cashier") that could plausibly be handed risk-configuration write
  access, so §24's delegated-authoring escalation does not recur here.
- **Self-exclusion/RG introduces no sportsbook-specific gap** (§29) —
  confirmed, not redesigned.
- **§17's cross-domain aggregate exposure gap is not a blocker to
  closure**, exactly as it was not a blocker for Bonus (§17) or Retail
  (§23) — it is refined, not newly created, by §27/§30, and remains an
  explicitly open decision for a future stage, never partially
  approximated.

**What remains impossible to configure after this section** — the
load-bearing negative claim, stated the way §18 and §24 state it.
Everything below remains blocked by database CHECK constraint and by HTTP
validation, and **remains so after this stage**:

- `sportsbook_settlement`, `sportsbook_cashout` as `operation` values —
  rejected by migration 0041's CHECK and by `newCreateRiskRuleHandler`'s
  allowlist. A rule intended to govern a settlement payout or a cashout
  **cannot be stored today**, which is correct: it would be a rule
  nothing evaluates.
- `sport`/`competition`/`event`/`market`/`selection` as scope dimensions
  — no columns, no `Rule`/`RiskRequest` fields, no `specificity()` bits,
  no HTTP input for any of them exist.
- `cumulative_amount` on `sportsbook_bet`, `sportsbook_settlement`, or any
  other sportsbook operation — unavailable until `ledger-finance` supplies
  the transaction-type mapping (§27), identical in kind to the same
  standing gap for `bonus_grant` (§15b) and `retail_funding` (§21).
- `count`/`velocity`/`exposure`/`loss` limit kinds, cross-operation
  aggregate exposure, campaign-level caps, subtree-aggregate exposure,
  points-denominated thresholds, calendar-aligned windows and
  session-scoped limits — all unchanged from §18/§24; sportsbook adds no
  new path to any of them.

The principle §4 established holds without exception through this stage
too: a rule the engine cannot evaluate must never be configurable in the
first place. §25-§31 are an extension-point specification; the extension
points stay closed until an authorizing stage opens them with all the
steps in §12 or §16 executed together in one change.


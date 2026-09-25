# ADR 0034 — Bonus/Gamification Integration with RG, KYC and Identity

Status: Accepted (architecture-freeze). Issued as part of Stage 4H-A
("Bonus/Gamification/Reward Orchestration — Architecture Freeze"),
directive §15 ("RG integration") and §16 ("KYC/Identity integration"),
both squarely owned by `identity-compliance`. Owner: `identity-compliance`.

This ADR does **not** design the Bonus/Gamification domain model itself
(campaigns, missions, tournaments, points ledger, marketplace, the Reward
Orchestrator, or the canonical event taxonomy) — those are owned,
concurrently, by `architect`, `bonus-engine`, `ledger-finance`, `risk`,
`sportsbook`, `backend`, and the Master Orchestrator, per the Stage 4H-A
task split. This ADR defines exactly the boundary those designs MUST
integrate against: how Bonus/Gamification consumes Responsible Gaming
(self-exclusion) and KYC/Identity, and where the line sits between "read a
fact" and "own a fact." No Go code, no migration, and no other file is
touched by this stage — this is a documentation-only architecture freeze,
per CLAUDE.md's stage-gate rule.

## Context

`docs/architecture/02-domain-and-service-boundaries.md`'s "Cross-domain
boundary verification" section already states Responsible Gaming's
authority precisely:

> Responsible Gaming | `internal/rg` | Self-exclusion/RG eligibility
> (`EvaluateEligibility`) | Never: Generic risk/limit policy — has no
> rule/threshold concept beyond RG's own restrictions

That boundary was drawn before Bonus/Gamification existed as a domain.
This ADR extends it to a new caller, changing nothing about `internal/rg`
or `internal/kyc` themselves: both packages, and the ADRs that define them
(0026, 0027, 0028), are read-only inputs to this stage, not something this
stage revises. Every gambling-action entry point built so far
(`internal/casino`'s `LaunchGame`/`postBet`) calls
`rg.EvaluateEligibility` as a hard precondition before doing anything
observable; Bonus/Gamification is simply the next entry point, and must
follow the identical discipline `docs/decisions/0031`'s §1 already
establishes for composing RG with Risk ("RG always short-circuiting Risk
on denial").

## Decisions

### 1. RG remains the sole authority — no parallel self-exclusion check, ever

Bonus/Gamification code MUST NOT read `player_restrictions` directly,
must not re-derive a "may this player receive value" answer from
`PlayerAccount.Status`/`Wallet.Status` on its own, and must not invent its
own restriction table or enum. Every player-facing bonus/gamification
action that grants, activates, or lets a player consume value calls
`rg.EvaluateEligibility` — the exact function that exists today, same
signature, same package:

```go
func rg.EvaluateEligibility(ctx context.Context, tx pgx.Tx, params rg.EligibilityParams) (rg.Decision, error)

type rg.EligibilityParams struct {
    TenantID        uuid.UUID
    BrandID         uuid.UUID
    PlayerAccountID uuid.UUID
    WalletID        uuid.UUID // uuid.Nil skips the wallet-status check
}

type rg.Decision struct {
    Allowed  bool
    Code     string // rg.CodeAllowed / CodePlayerAccountNotActive / CodeSelfExcluded / CodeWalletNotActive
    Message  string
    PersonID uuid.UUID
}
```

No new indirection layer is required or introduced by this ADR — the
signature already generalizes to any caller that can resolve a
`(TenantID, BrandID, PlayerAccountID)` and, where relevant, a `WalletID`,
which is exactly what Bonus/Gamification always has by the time it is
about to act. If the eventual Reward Orchestrator wants one shared
internal helper that wraps `EvaluateEligibility` plus its own audit call
(mirroring `internal/casino`'s `evaluateAndAuditEligibility`), that is an
implementation-time convenience for `bonus-engine`/the orchestrator to
build — it does not change what is being called or its authority.

**Concrete integration points** (illustrative, not exhaustive — the
Reward Orchestrator's own event taxonomy governs the full list):

- **First-deposit bonus grant**, triggered by a deposit event. Before
  creating the bonus grant record or crediting a bonus wallet, the Bonus
  Engine calls `EvaluateEligibility` with `WalletID` set to the bonus
  wallet being credited (once resolved) — exactly like `postBet` resolves
  a wallet and calls `EvaluateEligibility` before any ledger work. A
  `CodeSelfExcluded` or `CodePlayerAccountNotActive` decision means **no
  grant is created, no wallet entry is posted** — the deposit itself
  still completes (Payments' own concern), but the bonus attached to it
  simply never comes into existence.
- **Tournament/mission entry.** A player opts into a mission or
  tournament. Gamification calls `EvaluateEligibility` with
  `WalletID: uuid.Nil` (no wallet operation is happening yet) before
  inserting the participation row. A denial means the player is never
  enrolled — no partial participation state is created.
- **Marketplace reward redemption that credits a wallet** (e.g.,
  converting points into a cashback credit or a bonus-wallet credit).
  Exactly like a bet, this is a value-crediting operation: `WalletID` is
  set to the destination wallet, and `EvaluateEligibility` runs
  immediately before the ledger/points-debit work, never interleaved with
  it — mirroring `postBet`'s "resolve wallet → evaluate eligibility →
  only then touch balances" ordering (ADR 0026 §7).
- **Awarding points for an activity** (e.g., a completed wager or mission
  objective) follows the same rule if the award is itself a discrete,
  player-facing crediting action distinct from the activity that already
  passed its own eligibility gate (a casino bet already called
  `EvaluateEligibility` once; a points award derived from that same bet
  is a SEPARATE crediting action and gets its own check — cheap and
  correct, exactly like RG's own re-evaluation of the bet independent of
  the launch, per ADR 0026 §7's "re-evaluating independently... is
  deliberate, not redundant").

Composition with Risk, when both apply to one operation, follows the
fixed order ADR 0031 §1 already established and this ADR does not
revise: `rg.EvaluateEligibility` first, short-circuiting on denial, then
`risk.Evaluate`. RG's own policy has no rule/threshold concept — a
bonus-specific spend limit, cap, or eligibility rule belongs in Risk's
configurable engine or in the Bonus Engine's own template rules, never in
`internal/rg`.

### 2. Mid-lifecycle self-exclusion: prospective, not retroactive — the casino precedent, applied

The directive asks specifically: what happens to an already-active bonus
grant or in-progress mission if a player self-excludes mid-lifecycle?
`internal/casino`'s own precedent answers this directly and is adopted
here without modification:

- ADR 0026 §7: a `casino_launch_sessions` row can span an arbitrarily long
  round; a restriction applied mid-round is not retroactively applied to
  the already-launched session, but **must still stop the NEXT bet** —
  `postBet` re-evaluates independently of `LaunchGame`'s own check.
- ADR 0026 §11 (Consequences): `postWin`/`postRollback` deliberately do
  **not** call `EvaluateEligibility` — "settling/reversing an already-
  legitimate prior bet should not be blocked by a status change that
  occurred after the bet — blocking it would strand funds, and a rollback
  is itself a correction."

Applied to Bonus/Gamification, stated explicitly rather than left open:

1. **Every forward-going action re-evaluates fresh, every time.** Any new
   grant, new activation, new mission/tournament entry, new points award,
   or new marketplace redemption is gated by a fresh
   `EvaluateEligibility` call at the moment of that specific action. From
   the instant a self-exclusion becomes active (governed by `internal/rg`'s
   own `clock_timestamp()`-based immediacy — see `rg.go`'s own comment on
   why `now()` was wrong), every one of these is denied.
2. **Already-committed effects are never retroactively reversed or
   clawed back.** An active bonus grant's already-posted ledger entries,
   already-recorded mission progress, already-awarded points, and
   already-earned tournament placements as of the moment self-exclusion
   commits stand exactly as they are — self-exclusion is prospective,
   mirroring the casino session precedent exactly. Bonus/Gamification
   must not build a "claw back everything since self-exclusion" job; none
   is needed and building one would itself be a compliance risk (treating
   an already-legitimate action as if it never happened).
3. **An in-progress bonus/mission/tournament simply stops being able to
   progress.** No new wager may consume or contribute to it (already
   enforced transitively — every wager already calls
   `EvaluateEligibility` on its own, regardless of whether the stake is
   bonus or cash funds), no new mission-objective-completion event may be
   recorded against it, and no new leaderboard-affecting action may be
   processed. The grant/mission/tournament row itself is left exactly as
   it was — **not** cancelled, **not** marked forfeited, by RG. Whether
   and how an in-progress bonus that can now never complete should
   eventually be administratively expired/forfeited is a Bonus Engine
   lifecycle/T&C question (its own state machine's timeout handling), not
   an RG decision — RG's only job is the per-action eligibility gate, the
   same narrow role it has always had (`docs/architecture/02` — RG "has no
   rule/threshold concept beyond RG's own restrictions").
4. **Denials must be audited the same way casino denials are** — see §3.

**Flagged nuance, not resolved by analogy:** whether *completing* an
already-fully-satisfied wagering requirement (the qualifying activity
happened, and was itself correctly gated by `EvaluateEligibility` at the
time, before the self-exclusion instant; only a downstream, automatic
"unlock the bonus wallet balance for withdrawal/conversion" step remains)
should be allowed to proceed as a mechanical settlement of an
already-earned entitlement (mirroring `postWin`/`postRollback`'s "a
rollback is itself a correction" reasoning), or should itself be treated
as further bonus-lifecycle progress and blocked, is **not** the same kind
of question as a casino win. A casino win is a pure arithmetic resolution
of a bet that was already fully evaluated for eligibility; unlocking a
bonus's wagered-through balance arguably creates newly *usable* value the
player could not previously access (it moves funds from a locked/
non-withdrawable bonus wallet into something spendable or withdrawable).
This is exactly the kind of ambiguous, cross-cutting question Stage 4H-A
asks be resolved with reasoning rather than left vague — but this
specific sub-case is a genuine bonus-terms/product judgment call, not a
call `identity-compliance` is positioned to make unilaterally (it depends
on the bonus's own T&C design, which `bonus-engine`/`ledger-finance` own).
**Flagged for `bonus-engine`/product confirmation**: whichever way it is
decided, it must be an explicit rule in the Bonus Engine's own settlement
design, stated in its own ADR — never silently inherited from this ADR's
casino analogy without that confirmation.

### 3. Audit for RG denials in the bonus/gamification path

Every DENIED `EvaluateEligibility` call reached from a bonus/gamification
action writes an `audit.Entry` in the same transaction as the denial —
exactly the asymmetry ADR 0026 §11 already establishes (denials are
always audited here; an ALLOWED decision is evidenced by the downstream
action's own audit record, e.g. `bonus.grant_created`,
`gamification.mission_entered`, never double-audited). Action strings
follow the existing `<domain>.<event>_denied_by_rg_policy` convention
(e.g. `bonus.grant_denied_by_rg_policy`,
`gamification.mission_entry_denied_by_rg_policy`), carrying `Decision.Code`
and `PersonID` in `Metadata` — never a secret, never KYC evidence (see
§5).

## KYC / Identity integration (directive §16)

### 4. What Bonus/Gamification eligibility may depend on, and how it reads it

Bonus/Gamification eligibility may legitimately depend on:

- **`Person`** — the platform-wide identity cluster (ADR 0027), relevant
  to cross-brand scoping questions (§6 below) and to RG's own
  `PersonID`-keyed restriction (already covered by §1-§2 — no separate
  read needed for self-exclusion specifically).
- **`PlayerAccount`**, including its `Status` (already covered
  transitively by `EvaluateEligibility`'s own
  `CodePlayerAccountNotActive` check — an account in
  `identity_review_required`, `suspended`, `self_excluded`, or `closed`
  is already denied every forward-going bonus/gamification action with
  zero new code, exactly as ADR 0027 §11 already establishes for casino:
  "enforced by that SAME pre-existing check with zero new RG code path").
- **KYC verification STATE** — read-only, via `internal/kyc`'s existing
  ownership of `kyc_verifications`/`kyc_documents` (ADR 0028). Bonus/
  Gamification may read a verification's normalized `VerificationStatus`
  (`unverified | pending | review_required | approved | rejected |
  expired`) to decide whether a bonus may ACTIVATE. It must call INTO
  `internal/kyc` for this read (e.g., a read helper resolving the current/
  latest `kyc_verifications` row for a `PlayerAccountID`, in the same
  spirit as `GetVerificationByID`) — it must never query
  `kyc_verifications` directly from bonus/gamification code, and must
  never duplicate the six-state machine or its transition rules. This
  mirrors exactly how `internal/rg` reads `PlayerAccount.Status` without
  owning it, and is the identical extension point ADR 0026 §14 already
  reserved inside `EvaluateEligibility` itself for a future KYC/AML step —
  Bonus/Gamification's own eligibility composition should slot in a "read
  KYC status" step the same way, never invent a parallel status concept.
- **Identity resolution outcome** (ADR 0027) — already fully covered by
  the `PlayerAccount.Status` check above; `identity_review_required` is
  just another non-`active` status. No separate read of
  `internal/identityresolution` is needed or appropriate — that package
  is registration-time orchestration, not a per-action read surface.
- **Cross-brand identity** (`Person`, ADR 0027) — relevant to bonus
  *scoping*, addressed in §6.

**The hard rule (directive's explicit "do not store duplicate identity
evidence inside Gamification"), stated without qualification:**
Bonus/Gamification may READ a KYC verification **status** at
decision-time. It must **never** copy, cache, or persist any KYC
**evidence** — document content, extracted legal name/DOB/address,
government-ID references, provider raw payloads, or even a snapshotted
`VerificationStatus` value stored in a bonus-owned table for later reuse.
Every eligibility decision re-reads the current status from `internal/kyc`
at the moment it is needed. This is the identical discipline ADR 0027 §9
already applies to identity resolution ("no verified attribute value is
ever written to a log or audit record") and ADR 0028's own document
model (`SubmittedDocument` carries only references, never raw bytes) —
applied here to a THIRD consumer of the same evidence, not a new
principle.

### 5. Audit boundary (directive §22)

No sensitive KYC data may appear in a bonus/gamification `audit.Entry`,
or in any gamification-domain application log generally. Only a
verification **status reference** may appear — e.g.
`Metadata: {"kyc_status": "approved", "kyc_verification_id": "<uuid>"}` —
mirroring `audit.Record`'s existing actor/tenant/entity/before-after shape
exactly as every other domain already uses it, and mirroring
`identityresolution.ResolutionResult`'s own precedent (an opaque outcome
and `person_id`, never an evidence value). Concretely, forbidden in a
bonus/gamification `Metadata` field or log line: document content or
references beyond an opaque ID already owned by `internal/kyc`, legal
name, date of birth, address, phone, government-ID reference, or any raw
KYC provider payload/reason string that might itself contain PII (ADR
0027 already flags this identical risk for a resolver's own `Reason`
string — the same caution applies to any KYC-status-read helper's own
`Reason`/`reason` field, which must be treated as potentially unsafe to
log verbatim unless the KYC domain's own contract guarantees otherwise).

### 6. Contract: gating bonus activation on a KYC tier

Directive's concrete case — should a first-deposit bonus require KYC tier
X before activation — is a real and expected pattern (Blueprint's own
KYC-trigger model already names "cumulative deposit thresholds" and
"first withdrawal" as tier triggers; a bonus activation tied to a deposit
is a natural sibling trigger point). This ADR defines the CONTRACT, not
an implementation:

- A bonus template (owned by Bonus Engine's own configuration schema)
  carries an optional **minimum required KYC state** — expressed as a
  minimum `VerificationStatus` (in practice, `approved`) and/or a minimum
  `player_accounts.kyc_tier` value (Stage 2's existing, currently
  unenforced hook, exactly the same field ADR 0026 §14 already earmarked
  for a future KYC/AML `EvaluateEligibility` step) — this is
  Bonus Engine's configuration, not a new field on `kyc_verifications`.
- **Grant vs. activation are distinct points, and the gate belongs at
  activation.** A bonus may be provisionally GRANTED/queued (e.g., the
  triggering deposit happened, and the promotional entitlement is
  recorded) while KYC is still pending, exactly like a deposit itself is
  not blocked by pending KYC today. What the KYC-tier gate blocks is
  **activation** — the point where the bonus becomes usable/
  wagering-eligible — mirroring the Blueprint's own tiered model (a
  threshold gates a further action, not the fact that a threshold-
  triggering event occurred at all).
- The read itself is a single call into `internal/kyc`'s read surface
  (§4) at the moment of the activation attempt — never a cached/stored
  value on the bonus grant row. If the account is below the configured
  tier, the bonus stays in a Bonus-Engine-owned "awaiting verification"
  state (Bonus Engine's own state machine — not a KYC state, not an RG
  restriction) until a subsequent activation attempt finds the tier
  satisfied.
- This composes with, and is entirely independent of, the RG gate in §1:
  a bonus activation attempt must pass BOTH `EvaluateEligibility`
  (self-exclusion/account/wallet status) and, where configured, the KYC-
  tier check — neither substitutes for the other, and RG's own gate is
  checked exactly as it always is, regardless of the KYC-tier outcome.

No code implementing this contract is built in this stage — it is the
shape `bonus-engine`'s own template schema and activation logic should
target, and the exact function name/location of the `internal/kyc` read
helper is left to whoever implements it (a natural candidate:
`kyc.GetCurrentStatusForAccount(ctx, tx, playerAccountID) (VerificationStatus, bool, error)`,
following `GetVerificationByID`'s existing shape) — not prescribed here as
binding on `internal/kyc`'s own package design.

### 7. Cross-brand identity and first-deposit-bonus scoping — recommendation

Directive's concrete case: if a player is cross-brand-resolved to the
same `Person` (ADR 0027), should a "first deposit bonus" be scoped
per-brand, per-tenant, or per-Person platform-wide?

**Recommendation: the promotional entitlement itself defaults to
per-(tenant, brand) scope** — "first deposit at THIS brand" — with an
**independent, separately-configurable anti-abuse rule** that MAY be
scoped wider (tenant-wide or platform-wide) using the SAME nullable-scope
pattern `player_restrictions` already established (`tenant_id`/`brand_id`
nullability as the scope, never a separate mutable field).

Reasoning:

1. **Brand templates are already product-owned configuration, not
   platform-wide facts.** CLAUDE.md is explicit that "brand differences
   are configuration rows (theme, catalogue, payment methods, currencies,
   languages, RG defaults, **bonus templates**, jurisdiction rules...)" —
   a first-deposit bonus is exactly the kind of brand-specific
   promotional/commercial decision that principle already puts in the
   brand's own configuration, budget, and P&L. A brand must be able to
   run its own acquisition promotion for a player who happens to already
   have an account at a sibling brand under the same tenant — that is a
   normal, legitimate multi-brand operator pattern (e.g. a tenant running
   two differently-themed casino brands targeting different markets),
   not something RG/KYC has grounds to prohibit by forcing platform-wide
   scope.
2. **But cross-brand bonus abuse (opening multiple brand accounts
   specifically to farm repeated first-deposit bonuses) is a real,
   well-known industry fraud pattern**, and Person-resolution (ADR 0027)
   exists precisely to make a player recognizable across brands — it
   would be a wasted capability if Bonus Engine's fraud/eligibility rules
   had no way to consult it. The fix is not to force the entitlement's
   scope platform-wide; it is to give Bonus Engine (or Risk, per ADR
   0031, whichever owns the specific anti-abuse rule) a `PersonID`-keyed
   read — "has this Person already claimed a first-deposit-class bonus at
   ANY brand under this scope" — as an INPUT to its own configurable
   rule, independent of the entitlement's own default scope.
3. **This keeps the concerns separated correctly**: the promotional
   entitlement's scope is a commercial/marketing decision (Bonus Engine's
   own template configuration); the anti-abuse check that may deny a
   given claim because the SAME Person already claimed one elsewhere is a
   fraud/risk decision (Bonus Engine's or Risk's own rule, consuming
   Identity's `Person` linkage as a read-only fact) — neither belongs to
   `internal/rg`/`internal/kyc`, and this ADR does not attempt to own
   either.

**Flagged for human/product confirmation, not resolved as a purely
technical call**: how aggressively to prevent multi-brand bonus abuse
versus preserving each brand's ability to run independent acquisition
promotions is fundamentally a commercial/marketing policy question (an
operator relationship and revenue-attribution question, not an
engineering one), and CLAUDE.md's "when to stop and ask" section already
lists "commercial pricing"-adjacent decisions as exactly the kind
requiring human input rather than a specialist's own judgment call. The
recommendation above (per-brand default entitlement scope + optional
Person-keyed anti-abuse rule) is `identity-compliance`'s best technical
judgment for how to make BOTH options available without forcing a
premature choice into the schema, not a claim that the default is the
correct business answer — `bonus-engine`/product must confirm the actual
default behavior (and, per jurisdiction, whether any regulator requires a
stricter interpretation — see §8) before this ships as a real bonus
template.

## Multi-tenancy / jurisdiction touchpoints (RG/KYC-relevant)

### 8. Bonus/Gamification must consume, never invent, jurisdiction/licensing-mode context

RG restrictions and KYC tiers are inherently jurisdiction-sensitive: a
jurisdiction may require a higher KYC tier before any bonus activation,
or ban a bonus type outright for a risk-flagged category of player. ADR
0031 §9/§10 already establishes the exact contract every domain must
follow: `JurisdictionCode` (a string matching a `jurisdictions.code` row)
and `LicensingMode` (`tenants.licensing_model` — `under_platform_licence`
/ `own_licence`, per ADR 0006) are resolved server-side by the CALLER,
never looked up ad hoc, and never hardcoded per region. Bonus/Gamification
MUST resolve and pass these exactly the same way `internal/casino`
already does (`identity.GetTenantByID` + the shared
`resolveLicensingMode` helper for `LicensingMode`; the caller's own
trusted source — today, honestly, often nothing — for `JurisdictionCode`,
per ADR 0031's own disclosed limitation).

Concretely, out of scope for `internal/rg`/`internal/kyc` to decide, and
explicitly **in scope** for Bonus Engine's own template/rule
configuration or Risk's rule engine (ADR 0031), keyed on
`JurisdictionCode`/`LicensingMode` exactly like every other
jurisdiction-sensitive rule on this platform:

- A jurisdiction requiring KYC tier ≥ X before ANY bonus may activate
  (an additive constraint on top of, never a replacement for, §6's own
  per-template minimum).
- A jurisdiction banning a bonus TYPE outright (e.g., certain deposit-
  match structures restricted or banned in some EU markets).
- A jurisdiction imposing a stricter cross-brand abuse rule than the
  per-brand default recommended in §7 (e.g., mandating platform-wide
  first-deposit-bonus scoping as a consumer-protection requirement rather
  than leaving it to commercial preference).

`internal/rg` and `internal/kyc` themselves remain jurisdiction-agnostic
primitives — `EvaluateEligibility` and the KYC-status read (§4/§6) answer
the same "is this person self-excluded" / "what is their verification
status" question regardless of jurisdiction, exactly as they do for
casino today. The jurisdiction-specific THRESHOLD or BAN is a rule
Bonus Engine (or Risk) evaluates using `JurisdictionCode`/`LicensingMode`
as input — never a `if jurisdiction == "X"` branch inside
`internal/rg`/`internal/kyc`, and never a Bonus/Gamification-local
reimplementation of jurisdiction resolution.

## Consequences

- `internal/rg` and `internal/kyc` are unchanged by this ADR — it is a
  consumption contract for a new caller, not a redesign of either
  package, consistent with this stage's architecture-freeze/
  documentation-only scope.
- Every future Bonus/Gamification implementation stage inherits a
  concrete, non-negotiable rule: call `rg.EvaluateEligibility` before any
  player-facing grant/activation/entry/award/redemption, exactly like
  `internal/casino`; read KYC status through `internal/kyc`, never copy
  evidence; resolve jurisdiction/licensing-mode the same way every other
  domain already does. A future implementation stage's own specialist
  review should treat "does every bonus/gamification action call
  `EvaluateEligibility`" as a direct analogue of ADR 0026's own testing
  requirement for casino launch/bet.
- Two questions are explicitly flagged as needing human/product
  confirmation before they are implemented, not resolved here:
  1. Whether completing an already-fully-satisfied wagering requirement
     (unlocking a bonus wallet's balance) after a mid-lifecycle
     self-exclusion should be treated as a mechanical settlement
     (allowed) or as further bonus-lifecycle progress (blocked) — §2.
  2. Whether a first-deposit bonus's promotional entitlement should
     default to per-brand scope with an optional Person-keyed anti-abuse
     rule (this ADR's recommendation), or whether commercial/regulatory
     considerations require a stricter default — §7 (and, per
     jurisdiction, §8).
- This ADR does not claim any KYC/AML/RG capability beyond what ADRs
  0026-0028 already ship (self-exclusion enforcement mechanism; platform-
  owned KYC verification state model and provider abstraction, both
  `MOCK`/foundation-only) — see those ADRs' own "No fake completion"
  labels, unchanged by this stage.

## Cross-references

- `docs/decisions/0026-responsible-gaming-player-status-enforcement-foundation.md`
  — `EvaluateEligibility`, self-exclusion mechanism, casino mid-round
  precedent (§7), `postWin`/`postRollback` non-re-evaluation precedent
  (§11 Consequences).
- `docs/decisions/0027-person-resolution-and-cross-brand-identity-foundation.md`
  — `Person` clustering, cross-brand identity, evidence-minimization
  precedent (§9).
- `docs/decisions/0028-kyc-provider-abstraction-and-verification-model.md`
  — KYC verification state machine, provider interface, cross-tenant-reuse
  open decision (§7, unresolved there and not resolved here either).
- `docs/decisions/0031-risk-and-limits-engine.md` — RG/Risk composition
  order (§1), `JurisdictionCode`/`LicensingMode` contract (§9/§10), Bonus
  Engine's own future obligation to call `risk.Evaluate` (never build a
  parallel limit engine).
- `docs/architecture/02-domain-and-service-boundaries.md` — RG's
  authority boundary ("Cross-domain boundary verification" section),
  unchanged and reaffirmed by this ADR for its newest consumer.
- `docs/architecture/11-kyc-aml-rg-architecture.md` — Blueprint §4.7
  source material; tiered KYC trigger model referenced in §6.

## Sportsbook RG/KYC Integration (directive extension, Stage 4H-B0-R4)

Status: Accepted (architecture-freeze), added by Stage 4H-B0-R4
("Sportsbook architecture — dual-mode external/in-house — closing the
architecture before implementation"). Owner: `identity-compliance`,
unchanged.

This section extends this ADR to a **second domain** — Sportsbook — under
the same title's own scope ("...RG, KYC and Identity integration"). It
does not touch, restate for the purpose of changing, or supersede anything
in the Bonus/Gamification sections above (§1-§8): those stand as written.
It also does not design the Sportsbook domain model itself (bet
placement, acceptance, cashout, settlement, void, partial settlement) —
that is owned, concurrently and in parallel this same stage, by
`sportsbook` in the rewrite of `docs/architecture/09-sportsbook-
architecture.md`. This section defines exactly the boundary that rewrite
MUST integrate against, using the identical method §1-§2 above already
established for Bonus/Gamification: RG remains the sole authority, no
parallel self-exclusion/limit logic is ever built in `internal/sportsbook`,
and every conclusion below is reasoned from the same precedents (`postBet`/
`postWin`'s existing, hardened discipline; ADR 0034 §2's own "prospective,
not retroactive" principle), not assumed by analogy alone.

At the time of writing, `docs/architecture/09` is Stage-0 content (see
this ADR's own Cross-references pulling it in fresh) that frames bet
placement as effectively one step and does not yet define acceptance,
cashout, or settlement as separately-named lifecycle events with their own
ledger semantics — that richer lifecycle is exactly what `sportsbook`'s own
parallel rewrite this stage is producing. Every conclusion below is
written against the general shape doc 09 already commits to (an open bet
is a liability that can span days or months; placement, settlement, void,
partial settlement, and cashout are each distinct ledger events — doc 09's
own "What is genuinely platform-owned" section) and is explicitly flagged
where it needs reconciliation once `sportsbook`'s rewritten doc 09 lands
with its own concrete state machine.

### 9. Where RG must be evaluated relative to each sportsbook lifecycle event

**Bet placement — before the stake locks.** This is the direct sportsbook
analogue of `postBet`'s own pre-posting check (ADR 0026 §7; `internal/
casino/orchestrator.go`'s `postBet`): `rg.EvaluateEligibility` is called
with `WalletID` set to the wallet the stake is about to debit, immediately
before the stake moves to `player_locked` (doc 09's own term for the
placement-time ledger effect) and before any other observable financial
effect. A denial means **no stake is locked, no open-bet record is
created** — the same "zero financial effect on denial" shape `postBet`
already guarantees, not a new one invented for sportsbook.

**Bet acceptance — flagged as needing reconciliation with `sportsbook`'s
parallel doc 09 rewrite, not resolved here as a settled fact.** Some
sportsbook integrations (particularly a feed-and-API, in-house-UI mode)
genuinely separate "player submits a bet slip at displayed odds" from "the
bet is confirmed accepted," most commonly when odds moved between display
and submission and the player must confirm the new price (an
odds-change/confirmation flow) — a real, distinct step in that shape, not
a formality. Doc 09's current (Stage-0, pre-rewrite) framing treats
placement as effectively one step and does not name a separate acceptance
event; this ADR does not invent one where doc 09 does not yet define one.
**The rule this ADR fixes regardless of how that resolves**: if and when
`sportsbook`'s rewritten doc 09 defines acceptance as a genuinely distinct
step from initial submission, acceptance is the point where the stake
actually locks and the wager becomes binding — so it, not the earlier
submission, is the point `EvaluateEligibility` must gate, following
exactly the same "gate immediately before the financial/binding effect,
never at an earlier step that could go stale" principle §1 above already
applies to Bonus (e.g. "grant vs. activation are distinct points, and the
gate belongs at activation" — §6). If placement and acceptance collapse
into one step (the widget/iframe mode, and doc 09's current framing), the
single placement-time check above already covers it and no second check is
needed or introduced. `sportsbook` must confirm which shape applies, per
integration mode, in its own rewritten doc 09; this ADR does not decide it
unilaterally because the fact pattern (is there a real intervening step?)
is `sportsbook`'s domain model to define, not RG's.

**Cashout — yes, a fresh RG check is required, reasoned explicitly, not
assumed.** A cashout is not a passive wait for an outcome; it is a new,
discretionary, player-initiated action, taken at a point in time that can
be hours, days, or months after placement, that causes the platform to
proactively credit funds to the player's wallet *before* the bet's natural
outcome is known. That combination — player-initiated, time-displaced from
the original gate, and result-crediting — is structurally the same shape
as Bonus's marketplace redemption (§1: "exactly like a bet, this is a
value-crediting operation... immediately before the ledger... work"), not
the shape of a passive settlement. The player's RG status can genuinely
have changed in the interim (this is precisely the scenario §2 above
already contemplates for Bonus's longer-lived grants, and a sportsbook bet
can outlive any Bonus grant lifecycle in this codebase today). Therefore:
`EvaluateEligibility` runs fresh, in the same transaction as the cashout's
ledger posting, immediately before crediting the cashout amount, using the
same wallet-scoped params as placement. A denial means **the cashout does
not execute — no funds are credited early** — and the bet is left exactly
as it was: open, unsettled, proceeding toward its normal settlement (see
below). This is not a punitive action against the bet itself; it simply
means early/discretionary access to funds is gated exactly like every
other forward-going, value-crediting action on this platform, while the
bet's own eventual, non-discretionary settlement is unaffected by the same
denial (reasoned next).

**Settlement — no RG check, reasoned by the closer precedent (`postWin`),
not the more distant one (Bonus conversion).** ADR 0034 §2's own
already-flagged ambiguity for Bonus (whether unlocking a wagering
requirement's wagered-through balance is a mechanical settlement or further
bonus-lifecycle progress) is the right MODEL for how to reason about this
question, but it does not transfer its answer here, because sportsbook
settlement lacks the one feature that made Bonus's case genuinely
ambiguous: in Bonus, real player activity (wagering) occurs *after* the
grant and *contributes* to unlocking value that did not previously exist
in usable form — that is arguably new player-initiated progress happening
inside the window where RG status could have changed. A sportsbook bet's
settlement has no analogous intervening player activity: between
acceptance and settlement, the player does nothing that contributes to or
changes the bet's outcome — the outcome is entirely determined by the
sporting event, external to the player and to the platform, and the bet's
potential return was already fully fixed and disclosed at acceptance. This
is exactly the shape `postWin` already resolved for casino, stated in its
own doc comment verbatim: *"a win settles a bet that was already
legitimate when placed (postBet's own RG check already gated it). Blocking
the settlement of an already-placed bet because the player's status
changed AFTER the bet would strand the stake in house_gaming with no
compensating entry — the opposite of player protection, not an enforcement
of it."* A sportsbook WIN settlement is the same operation in substance:
crediting the disclosed, already-priced potential return of a bet that was
correctly gated at acceptance is realizing a pre-existing, already-fixed
entitlement, not initiating new player activity — so settlement (WIN,
LOSS, void, and partial settlement/bet-builder-leg resolution and market
correction/re-settlement alike, per doc 09's own list of distinct
settlement-family events) is **RG-exempt**, mirroring `postWin`/
`postRollback`'s existing exemption exactly, not a new exemption invented
for sportsbook. This is a reasoned conclusion, not an assumption carried
over from Bonus's different answer — the deciding difference is the
absence of intervening player-contributed progress between the RG-gated
event and the crediting event, which Bonus's flagged case has and
sportsbook settlement does not.

One consequence worth stating plainly, since §11 below depends on it: RG
being exempt at settlement, combined with cashout requiring a fresh (and
therefore possibly denying) check, means a self-excluded player with an
open bet has exactly one route to it resolving — waiting for normal
settlement — and no route to early access. No separate rule is needed to
produce that outcome; it falls out of §9's two conclusions directly.

### 10. How the architecture avoids a bypass

Every RG check introduced by this section follows the identical,
already-hardened discipline `postBet`/`internal/rg.EvaluateEligibility`
already established, with no new discipline invented for sportsbook:

- **Evaluated fresh, every time** — never cached, never read from a
  session flag, a widget-reported status, or any prior check's stored
  result (including the placement-time check itself: cashout and, where
  applicable, acceptance each call `EvaluateEligibility` again,
  independently, exactly as `postBet` already re-evaluates independently
  of `LaunchGame`'s own check — ADR 0026 §7's "re-evaluating
  independently... is deliberate, not redundant," restated here for a
  third caller).
- **`clock_timestamp()` semantics, not `now()`** — if a sportsbook
  placement/cashout check is implemented as a similar SQL-transaction
  pattern (a lock-then-check sequence analogous to `lockPerson` +
  `EvaluateEligibility`'s own restriction query), it inherits
  `internal/rg`'s existing fix for exactly this reason (`rg.go`'s own
  Stage 4G-FINAL comment: `now()` is STABLE per transaction and can miss a
  self-exclusion that committed after the transaction began but before the
  statement ran; `clock_timestamp()` re-evaluates the true current
  instant on every call). This is not a new fix `identity-compliance`
  proposes for sportsbook — it is `internal/rg.EvaluateEligibility`'s
  existing, unmodified behavior, which every caller (including a future
  sportsbook one) gets automatically by calling the same function; it is
  stated here only so a sportsbook-specific caller is never tempted to
  hand-roll its own eligibility SQL with `now()`.
- **Inside the same database transaction as the financial posting, before
  commit** — the placement check runs in the same transaction as the
  `player_locked` stake movement; the cashout check runs in the same
  transaction as the cashout credit. No sportsbook code path may post a
  financial effect first and check RG afterward, or in a separate
  transaction/best-effort follow-up call.
- **No override parameter, ever.** `rg.EligibilityParams` carries no
  "skip," "force," or "bypass" field, and this section does not ask for
  one to be added. A sportsbook operational need (e.g. a support agent
  manually completing a stuck settlement) is never satisfied by a runtime
  flag that skips this check — if a genuine administrative override is
  ever needed, it must be its own separately-audited, four-eyes-gated
  administrative action (per CLAUDE.md's own "manual balance adjustments
  require a reason code and four-eyes approval above a configurable
  threshold"), never a parameter on the eligibility call itself. This
  mirrors the standing pattern already established elsewhere in this
  codebase for exactly this class of risk — ADR 0036's RLS policies for
  the retail channel are designed to **fail closed** when their scoping
  GUC is unset, specifically so a bypass requires a code change (a new,
  deliberately-written code path) and can never arise from a runtime
  misconfiguration or an unset value. The same property holds here by
  construction: there is no configuration value, feature flag, or unset
  parameter that causes `EvaluateEligibility` to be skipped — the only way
  a sportsbook code path stops calling it is to be written that way,
  which is exactly the kind of change this ADR's own "Consequences"
  section (below) puts in scope for `identity-compliance`/architect review.

### 11. Self-exclusion mid-lifecycle for an OPEN, unsettled sportsbook bet

This is the genuinely new case sportsbook introduces. Bonus/Gamification's
existing answer (§2: "prospective, not retroactive... already-committed
effects stand") was reasoned for events that resolve in milliseconds to,
at most, the life of one launch session. A sportsbook bet can remain open
for days or months (doc 09's own framing), so "what happens if self-
exclusion commits while a bet is open" is a real, extended window, not a
race-condition edge case — it deserves its own reasoning, not a one-line
extension of §2 by analogy.

**Options considered:**

- **(a) The bet settles normally when the event concludes, regardless of
  the self-exclusion having commenced in the interim.** Funds already
  staked on a legally-placed wager are not returned mid-flight; the bet
  runs its course exactly as it would have absent the self-exclusion, and
  §9's own conclusion (settlement is RG-exempt) already produces this
  outcome with zero new logic.
- **(b) The bet is voided/refunded early because of the later
  self-exclusion.** Considered and rejected as a default: voiding a
  bet that was legitimately placed and correctly gated at the time changes
  the sportsbook's own priced exposure and liability after the fact (the
  bet was accepted, and — in an external-provider mode — very likely
  already hedged/laid off by that provider, against the ORIGINAL odds and
  outcome distribution; unwinding it later is not merely a wallet
  operation, it is re-opening a trading/risk position that was already
  closed). It also raises its own fairness question the other direction:
  returning a stake after the event has partially or fully played out (or
  after odds have since moved) is not a neutral no-op the way voiding an
  unlaunched casino round is.
- **(c) Something else** (e.g., a jurisdiction-specific carve-out, a
  discretionary compliance-team void on a case-by-case basis) — not ruled
  out as a POSSIBILITY, but not something this ADR can specify generically,
  since by construction it would be jurisdiction- or case-specific, not a
  platform-wide default.

**Recommended default: (a).** This is the closest available extension of
ADR 0034's own already-established principle — "prospective, not
retroactive... already-committed effects stand" — applied to a
already-committed effect (the bet) whose window before its own effects
fully resolve happens to be long, not to a fundamentally different
principle. It is also the general gambling-industry norm for an
already-accepted wager, and it composes cleanly with §9's own conclusions
with no additional mechanism: settlement is RG-exempt (§9), and the ONLY
alternative route to early resolution — cashout — is already denied fresh
by RG the moment self-exclusion is active (§9), so a self-excluded player
cannot accelerate or alter the bet's outcome after the fact; they can only
wait for the same natural settlement any other bettor would.

**This is flagged explicitly as needing human/compliance confirmation,
not silently decided by this ADR as a closed question.** Unlike Bonus's
"prospective, not retroactive" principle — which was a fairly direct
carry-over of an existing, already-accepted casino precedent — this
specific case (an open wager spanning a genuinely long window, across
potentially many jurisdictions with different regulatory expectations
about what a self-excluding player's open positions must do) is exactly
the shape of question CLAUDE.md's "When to stop and ask" section names:
"jurisdiction selection," RG-adjacent legal interpretation, and decisions
this specialist is not positioned to make unilaterally on the platform's
behalf. Some jurisdictions may have an explicit regulatory expectation
(e.g. a specific self-exclusion regime that requires open wagers to be
voided and stakes returned, or conversely one that explicitly permits (a))
that this ADR has no visibility into and must not assume either way. **(a)
is `identity-compliance`'s recommended engineering default, not a claim
that it is the compliant answer in every jurisdiction this platform will
ever operate in** — `sportsbook`/product/legal-compliance must confirm it
(and, per jurisdiction, whether option (c)'s carve-out is required) before
this ships as a real rule, exactly the same posture ADR 0034 §7/§8 already
takes toward its own two flagged, unresolved questions.

### 12. KYC boundary — no new requirement introduced by sportsbook

Confirmed explicitly, not silently assumed: sportsbook introduces **no**
new KYC requirement, threshold, or trigger beyond what ADR 0028 and the
platform's existing jurisdiction-configured model already establish.
KYC thresholds/triggers (cumulative deposit thresholds, first withdrawal,
a large-win payout threshold, etc.) remain a **jurisdiction-configured,
vendor-agnostic concern**, read the same way §4/§6 above already establish
for Bonus (a read into `internal/kyc`'s existing verification-status
surface, never a duplicated status concept, never evidence copied into a
sportsbook-owned table). A sportsbook-specific trigger (e.g. "a large
sportsbook win payout requires KYC tier ≥ X before the win settlement
credits a withdrawable balance") is not a new KYC concept — it is the
identical jurisdiction-configured tier-threshold pattern §6 already
defines for Bonus activation, applied to a different triggering event, and
belongs in the platform's existing jurisdiction/threshold configuration
(and, if it needs to gate a specific step, in `sportsbook`'s own
settlement/payout logic calling into `internal/kyc`'s read surface — never
a new gate inside `internal/kyc` itself). `internal/kyc` and `internal/rg`
remain unchanged and jurisdiction-agnostic exactly as §8 above already
states for Bonus; nothing in this section revises that.

### 13. External-provider vs. in-house mode — RG enforcement must be identical

Stated as a hard architectural requirement, not a preference: **RG
enforcement must be byte-for-byte identical regardless of which
integration mode placed the bet** — widget/iframe (an external provider
rendering the betting experience against the platform's own wallet, doc
09's "start here" recommendation) or a future in-house feed-and-API engine.
The enforcement point is always the platform's own boundary — wherever
the platform's own wallet/ledger posting for that lifecycle event
(placement, cashout) actually occurs — never delegated to, inferred from,
or trusted from an external provider's own systems.

Concretely:

- An external provider's widget may have its own, provider-side
  responsible-gambling UI (deposit/session limits, its own self-exclusion
  affordance inside the widget). That UI is the provider's own product
  surface and may exist for regulatory reasons of the provider's own
  jurisdiction/licence — it is never treated as a substitute for, input
  to, or evidence for this platform's own `EvaluateEligibility` decision.
  The platform does not read, trust, or short-circuit its own check based
  on any status the widget reports about itself.
- For a widget/iframe integration specifically, the platform's own
  `EvaluateEligibility` call runs at the moment the platform's own
  wallet/ledger posts the effect — i.e., when the platform receives and
  processes the provider's confirmation callback for the bet (the direct
  sportsbook analogue of `postBet` receiving and posting a provider
  callback) — never at the moment the provider's own widget merely
  displays a bet slip or accepts a click inside its own iframe. That
  earlier moment produces no platform-side financial effect yet and is not
  a boundary the platform controls or can atomically gate; the platform's
  callback-handling code is the first point it truly owns, so that is
  where the check belongs.
- For an in-house feed-and-API mode, the same rule applies identically —
  RG runs at the platform's own placement/cashout posting code, with no
  behavioral difference from the external-provider mode's check. This ADR
  does not carve out a lighter-weight or differently-timed check for
  either mode; the whole point of "the platform's own boundary" is that it
  does not move depending on who rendered the bet slip.
- This is the same discipline CLAUDE.md's "Provider abstraction" section
  already states platform-wide ("provider specifics never leak into core
  domain logic"; "the vendor supplies the API; the platform still owns the
  adapter, idempotency/retry semantics... the state machine") and the same
  reason an external provider integration is exactly the kind of boundary
  where a bypass could otherwise creep in — a provider's own widget
  offering its own RG controls is the most likely place a future
  implementer could be tempted to reason "the provider already checks
  this, so the platform's own check is redundant here." This section
  makes explicit that it is not: the platform's own `EvaluateEligibility`
  call is never optional, never provider-delegated, and never contingent
  on what the provider's own widget does on its own side, regardless of
  integration mode.

### Consequences (Sportsbook addendum)

- `internal/rg` and `internal/kyc` remain unchanged by this section, as
  they were by §1-§8 above — this is a consumption contract for a second
  domain, not a redesign.
- `sportsbook`'s implementation must, at minimum: call
  `rg.EvaluateEligibility` fresh, in-transaction, before posting the
  `player_locked` stake at placement (and at acceptance, if and where its
  own rewritten doc 09 defines a genuinely distinct acceptance step — §9)
  and before crediting a cashout amount; must NOT call it at settlement
  (WIN/LOSS/void/partial-settlement/re-settlement alike); and must resolve
  `JurisdictionCode`/`LicensingMode` the same server-side way §8 above
  already requires for Bonus, never inventing a sportsbook-local
  resolution path.
- One question in this section is explicitly flagged as needing human/
  compliance confirmation before it is implemented, not resolved here:
  whether an open, unsettled sportsbook bet should settle normally
  regardless of a self-exclusion that commenced while it was open (this
  section's recommended default, §11), or whether a specific jurisdiction
  requires voiding/refunding it instead — a jurisdiction-dependent
  regulatory question, not a purely technical one.
- The bet-acceptance question in §9 (whether it is ever a real, distinct
  lifecycle step) is left for `sportsbook`'s own rewritten doc 09 to
  settle as a domain-model fact; this section states only the rule that
  applies once that fact is known, not the fact itself.

### Cross-references (Sportsbook addendum)

- `docs/architecture/09-sportsbook-architecture.md` — Stage-0 sportsbook
  domain shape (open-bet-as-long-lived-liability; placement/settlement/
  void/partial-settlement/cashout as distinct ledger events); being
  rewritten in parallel this stage by `sportsbook`, whose rewrite this
  section's §9 acceptance-step question must be reconciled against.
- `internal/rg/rg.go` — `EvaluateEligibility`, `lockPerson`, and the
  `clock_timestamp()` vs. `now()` fix (Stage 4G-FINAL), unchanged and
  reused verbatim by this section, not re-implemented for sportsbook.
- `internal/casino/orchestrator.go` — `postBet` (pre-posting RG gate,
  mandatory template for sportsbook placement/cashout) and `postWin`
  (RG-exempt settlement precedent, mandatory template for sportsbook
  settlement), both cited verbatim in §9 above.
- `docs/decisions/0036-retail-hierarchy-rbac-and-audit.md` — fail-closed
  discipline (a bypass requires a code change, never a runtime
  misconfiguration), cited in §10 as the parallel precedent for "no
  override parameter."
- `docs/decisions/0028-kyc-provider-abstraction-and-verification-model.md`
  — KYC verification state machine and jurisdiction-configured trigger
  model, confirmed unaffected by sportsbook in §12.
- `docs/decisions/0031-risk-and-limits-engine.md` — RG/Risk composition
  order and `JurisdictionCode`/`LicensingMode` contract, which §9's
  Consequences bullet requires `sportsbook` to reuse rather than
  reinvent.

## §14 OpenBetSelfExclusionPolicy — configurable architecture (Stage 4H-B0-R5)

Status: Accepted (architecture-freeze), added by Stage 4H-B0-R5 ("final
pre-implementation gate — architecture/ADR only"). Owner:
`identity-compliance`, unchanged. This section does not modify, restate
for the purpose of changing, or supersede §9-13 above — they stand as
written, including §11's own reasoning and its recommended default. What
changed is the directive's framing: §11 reasoned toward a single
recommended default and flagged it as needing human confirmation. This
section instead defines the CONFIGURABLE POLICY ARCHITECTURE that lets a
human/compliance decision be expressed and enforced per jurisdiction/
tenant/brand — the "how the platform represents and applies whichever
answer is chosen," not a second attempt at the same answer. Nothing here
selects (a) or (b) as the shipped default; that remains open, restated
precisely in §14.9.

This section does not design Sportsbook's domain model (that is
`sportsbook`'s rewrite of `docs/architecture/09-sportsbook-architecture.md`,
already landed) and does not redesign the ledger posting shape ADR 0038
§8.1 already specifies for void — it is reused verbatim. It also does not
touch ADR 0038's `transaction_type` enum or `internal/rg`/`internal/kyc`
themselves, consistent with every prior section of this ADR.

### 14.1 The policy's value domain — two values, a third considered and rejected as unnecessary

`OpenBetSelfExclusionPolicy` is a configuration value with exactly two
members:

```
OpenBetSelfExclusionPolicy ∈ { SETTLE_NORMALLY, VOID_ON_SELF_EXCLUSION }
```

- **`SETTLE_NORMALLY`** — §11(a): the bet proceeds to its natural
  settlement (WIN/LOSS/push/partial-settlement/re-settlement) when the
  underlying event concludes, exactly as if self-exclusion had not
  occurred. This is the "zero new mechanism" branch — it is what already
  happens by construction today, per §9's conclusion that settlement is
  RG-exempt, so selecting this value requires no new financial code path,
  only the audit record §14.5 below adds.
- **`VOID_ON_SELF_EXCLUSION`** — §11(b): the bet is voided immediately
  upon self-exclusion becoming effective, full stake returned, using the
  existing void posting shape ADR 0038 §8.1 already defines (`Dr
  player_locked` / `Cr player_cash`/`player_bonus`, full stake `S`, or the
  post-settlement reversal-chain variant if settlement has already posted
  by the time the void is processed) — not redesigned here, only
  triggered by a new cause (§14.7).

**A third value — "settle normally but withhold the payout pending
compliance review" — is considered and rejected as a separate policy
value, not because the underlying operational need is illegitimate, but
because it does not require its own value in this enum.** That behavior
is already fully expressible as a COMPOSITION of `SETTLE_NORMALLY` with a
mechanism this ADR has already established for an unrelated trigger: §6's
KYC-tier gate on bonus activation, and §12's confirmation that a
sportsbook payout threshold is "the identical jurisdiction-configured
tier-threshold pattern §6 already defines... applied to a different
triggering event" (a large win requiring KYC tier ≥ X before the
settlement credits a withdrawable balance). A jurisdiction or tenant that
wants "settle normally, but hold the resulting balance for review before
it is spendable/withdrawable" gets exactly that by (1) selecting
`SETTLE_NORMALLY` for this policy, so the ledger settles cleanly and no
new void/reversal mechanism is invoked, and (2) separately configuring a
withdrawal-side or KYC/AML-side hold — which is Stage 3D withdrawal
governance's and/or KYC/AML's own configuration surface, not a value of
THIS policy. Collapsing the two into one enum member would conflate a
settlement-shape decision (does the bet resolve as a normal financial
event) with an access-control decision (can the resulting funds move
right away) — exactly the kind of two-different-concerns-modeled-as-one
mistake CLAUDE.md's provider-abstraction and domain-boundary principles
already warn against elsewhere in this ADR (e.g. §7's careful separation
of entitlement scope from anti-abuse rule). **If a genuine, additional
compliance-hold requirement surfaces that composition cannot express, it
is a new decision for whoever owns withdrawal-gating (`ledger-finance`/
Stage 3D) to raise, not something this section invents preemptively.**

### 14.2 Policy scope — jurisdiction is the primary axis; tenant/brand may only tighten, never loosen

**Primary scope: `JurisdictionCode`.** Self-exclusion's regulatory
expectations (per §11's own reasoning: "potentially many jurisdictions
with different regulatory expectations about what a self-excluding
player's open positions must do") are the kind of requirement this
platform already treats as jurisdiction-mandated, not brand-discretionary
— the same category as the jurisdiction-scoped `HARD_LIMIT`s ADR 0031 §9
already makes reachable platform-wide, and the same category §8 above
already assigns to "a jurisdiction requiring KYC tier ≥ X before ANY bonus
may activate" or "a jurisdiction banning a bonus TYPE outright." A
jurisdiction's regulator, not an individual tenant's product team, is the
party positioned to require one answer or the other for that
jurisdiction's licensed operators.

**Secondary scope: tenant and/or brand, using the same nullable-scope
pattern `player_restrictions` and §7's anti-abuse rule already
establish** (`tenant_id`/`brand_id` nullability as the scope dimension,
never a separate mutable "level" field) — but with a directional
constraint this policy needs and ADR 0031's limit engine already has a
proven precedent for: **`§5`'s HARD_LIMIT/CONFIGURABLE_LIMIT precedence,
where "a breach of ANY [hard limit] denies... never overridden by a more
specific configurable rule," applied here as "a jurisdiction's configured
value is a floor a tenant/brand-scoped override may only tighten, never
loosen."**

Concretely, ordering `VOID_ON_SELF_EXCLUSION` as the stricter/more
protective value (it removes the bet and the player's exposure to its
outcome immediately, at the cost of unwinding an already-accepted
wager — the exact trade-off §11(b) reasoned through) and
`SETTLE_NORMALLY` as the more permissive/default value (the bet runs its
ordinary course, the general industry norm §11 already cites):

- A jurisdiction's configured value is a **floor**. If a jurisdiction
  mandates `VOID_ON_SELF_EXCLUSION`, no tenant or brand licensed under
  that jurisdiction (or operating there under `own_licence` per ADR 0006/
  0031 §10's `LicensingMode`) may configure `SETTLE_NORMALLY` for that
  jurisdiction — exactly the "legal floor... never overridden downward by
  a more permissive tenant setting" rule this section is asked to mirror.
- A tenant or brand MAY configure `VOID_ON_SELF_EXCLUSION` even where its
  jurisdiction's floor is `SETTLE_NORMALLY` (or has no explicit
  requirement at all) — a brand adopting a stricter, more
  player-protective posture than its jurisdiction's legal minimum is
  always permitted, mirroring every other floor/ceiling pattern already
  established on this platform (a tenant may always be stricter than a
  jurisdiction's HARD_LIMIT floor by adding its own more restrictive
  CONFIGURABLE_LIMIT; RG's own restrictions are never something a tenant
  can loosen). The reverse direction is never permitted.
- Where no jurisdiction value AND no tenant/brand override is configured,
  resolution falls back to the platform-wide default — precisely the
  value §14.9 states remains an open human/compliance decision, not
  something this section assigns.

This is a genuine, if narrow, generalization of ADR 0031 §5's precedence
algorithm to a categorical (not numeric) policy value: "most specific
wins" does not directly apply (there is no meaningful "value" ordering
analogous to a betting limit's magnitude across most of ADR 0031's
existing limit kinds), so this section defines the ordering explicitly as
`VOID_ON_SELF_EXCLUSION` ≥ `SETTLE_NORMALLY` on a single "strictness" axis
with exactly two points, rather than importing ADR 0031's specificity
ladder unmodified. **This is the one place this section introduces
anything beyond direct reuse of an existing mechanism, and it is scoped
narrowly to this specific two-valued policy — it is not a new
general-purpose precedence primitive for `internal/risk` or any other
domain, and does not touch ADR 0031 §5's own algorithm or code.**

### 14.3 Effective time — the policy version in effect when self-exclusion becomes effective governs

The scenario is fixed: the bet is always placed before the self-exclusion
event; the only thing that can vary in time is which VERSION of this
platform's configured policy exists at the moment self-exclusion actually
becomes effective. **The policy version governing a given open bet is
resolved at, and only at, the instant that specific bet's player's
self-exclusion becomes effective — not the version in effect when the bet
was placed, and not the version in effect at any later time (e.g. when
the event eventually concludes, for `SETTLE_NORMALLY`).** This mirrors
`internal/rg`'s own `clock_timestamp()` discipline in spirit (§10: read the
true current state at the moment the triggering instant occurs, never a
value cached from an earlier point), applied here to a configuration read
rather than a live eligibility check: the decision this policy answers is
"what do we do about this open bet NOW that self-exclusion has occurred,"
not something decided in advance at placement or deferred to settlement
time. Once resolved, that specific version is the one applied to that
bet's disposition and is what the audit record (§14.5) captures — it is
not re-resolved again later even if the platform's configured policy
changes again before the bet's eventual natural settlement (for
`SETTLE_NORMALLY`) or before a queued void executes.

### 14.4 Jurisdiction interaction — reuses ADR 0031's contract; is orthogonal to whether self-exclusion itself varies by jurisdiction

The `JurisdictionCode`/`LicensingMode` resolution mechanism this policy's
jurisdiction scope (§14.2) depends on is not redesigned here — it is ADR
0031 §9/§10's existing contract, restated by §8 above for Bonus and
reused verbatim: resolved server-side by the caller, matching a real
`jurisdictions.code` row, never hardcoded or inferred client-side.

**Important distinction this section must not blur**: self-exclusion
**itself** — the restriction mechanism `internal/rg` enforces — is, per
`docs/architecture/11-kyc-aml-rg-architecture.md`'s Implementation status
section, **platform-wide by default for player self-service today**, with
no jurisdiction-varying rule in `internal/rg`'s own model (staff-initiated
restrictions are tenant/brand-scoped; player self-service self-exclusion
is platform-wide, full stop — doc 11 and ADR 0026 §2 do not vary this by
jurisdiction). `OpenBetSelfExclusionPolicy` does **not** change that: it
does not make self-exclusion itself apply differently per jurisdiction. It
answers an entirely separate, downstream question — once a (still
platform-wide) self-exclusion has taken effect for a given Person, what
happens to THAT Person's open sportsbook bets in the jurisdiction(s) each
bet's own tenant/brand operates under — which is why the policy is scoped
by the BET's jurisdiction context (mirroring §9 above and ADR 0031 §9's
`casino_launch_sessions.jurisdiction_code` precedent — a sportsbook bet
would carry the equivalent denormalized jurisdiction context from its own
placement time), not by the excluding Person's registration jurisdiction.
If Person P has open bets under two different tenants/brands in two
different jurisdictions when P's platform-wide self-exclusion commits,
each open bet resolves its OWN policy independently, from its own bet's
jurisdiction/tenant/brand scope — there is no single platform-wide answer
to "what happens to P's open bets," only a per-bet one, exactly as
self-exclusion's own enforcement is already per-action (§2, §9) rather
than a single global effect.

### 14.5 Audit — a genuinely new audit trigger point, not an inherited one

Confirmed per CLAUDE.md's standing audit rule and this ADR's own §3
pattern ("RG denials are audited... exactly as established"): **whichever
policy path is taken for a specific open bet, at the moment self-exclusion
takes effect, itself produces an `audit.Entry`** — one per affected open
bet, not one per self-exclusion event, since a single self-exclusion may
have zero, one, or many open bets in scope across tenants/brands/
jurisdictions (§14.4).

- **Actor**: system, attributed to the specific self-exclusion
  event/restriction row that triggered it (the `created_by_actor_type`/
  `created_by_actor_id`-shaped attribution ADR 0026 already uses for
  restriction records, referenced by id here rather than a human staff
  actor or an IP address — there is no request/IP context for a
  system-triggered background effect, unlike a staff-initiated action).
- **Tenant**: the specific bet's own tenant (not the excluding Person's
  registration tenant, which may differ — §14.4).
- **Entity**: the specific bet (its id/`provider_bet_reference`).
- **Before/after state**: for `VOID_ON_SELF_EXCLUSION`, `open` →
  `void`(as ADR 0038 §8.1's own before/after shape already implies for any
  void); for `SETTLE_NORMALLY`, `open` → `open` (unchanged) — the
  record's purpose in this branch is not to show a state transition but to
  evidence that the platform affirmatively considered this specific bet
  and applied a deliberate policy decision to it, not that it was silently
  skipped or simply never examined.
- **Reason code**: which policy value applied (`SETTLE_NORMALLY` /
  `VOID_ON_SELF_EXCLUSION`), the resolved scope it came from
  (jurisdiction-floor / tenant-override / brand-override / platform
  default — §14.2), and the policy version read (§14.3/§14.8) — mirroring
  §5 above's `Metadata` shape exactly (`{"policy": "...", "scope":
  "...", "policy_version": "..."}`), never any KYC evidence per §4/§5's
  standing prohibition.

**This is stated explicitly as NEW, not merely inherited**: nothing in
this codebase today treats self-exclusion COMMITTING as an event that
itself touches or even enumerates a player's existing open financial
positions. Every prior mechanism (§2's Bonus precedent, §9's Sportsbook
placement/cashout/settlement gates) is a **per-action, request-time**
check — self-exclusion changes what happens the NEXT time the player (or
the platform, at natural settlement) acts, but nothing today reaches back
into currently-open positions the instant self-exclusion commits. This
policy requires a genuinely new system behavior: on self-exclusion
becoming effective, enumerate that Person's currently-open sportsbook bets
(across every tenant/brand the Person is linked to, per §14.4), resolve
the applicable policy for each, and — for `VOID_ON_SELF_EXCLUSION` —
execute the void and its audit record synchronously with that resolution;
for `SETTLE_NORMALLY`, write only the audit record, with zero financial
effect. This enumeration/dispatch step is a new architectural component
this policy introduces (conceptually, a listener on the self-exclusion
commit event, per doc 22's canonical event taxonomy pattern already used
elsewhere in this ADR for `rg.status.changed`), not something `internal/
rg` itself needs to own or implement — `internal/rg` remains, as always,
ignorant of sportsbook/bonus specifics; the listener lives with whichever
domain owns the open-bet enumeration (`sportsbook`), consuming `rg.status.
changed` exactly as it is already "consumed — never produced — by
sportsbook" per doc 09 §8.

### 14.6 Settlement interaction (`SETTLE_NORMALLY`) — consistent with the existing settlement-is-RG-exempt precedent, not a new exception

If the resolved policy is `SETTLE_NORMALLY`, the eventual settlement (WIN,
LOSS, push, partial-settlement, re-settlement) proceeds exactly as §9
above already establishes for every open bet regardless of self-exclusion
— a WIN credits `player_cash`/`player_bonus` through the normal ADR 0038
posting shape, with **no new exception, no new gate, and no new
conditional logic keyed on the player's self-excluded status inside the
settlement path itself.** This is not a new rule being introduced here; it
is §9's already-reasoned conclusion ("settlement... is RG-exempt... a
sportsbook WIN settlement is... realizing a pre-existing, already-fixed
entitlement, not initiating new player activity") simply being confirmed
as unaffected by the existence of this policy — selecting
`SETTLE_NORMALLY` does not add anything to the settlement code path beyond
the audit record in §14.5, which is emitted at the self-exclusion instant,
not at settlement time.

**On what the player can subsequently do with that newly-settled
balance** — this ADR checked rather than assumed. Neither
`docs/architecture/11-kyc-aml-rg-architecture.md` nor this ADR's own §1-13
establishes an explicit "self-excluded players may withdraw but not
wager" rule anywhere in this codebase; no such rule exists to cross-
reference. What IS established, precisely, is narrower: **every
forward-going, value-consuming or wagering action remains blocked** (§1,
§9 — placement, cashout, new bonus/gamification actions all deny on
`CodeSelfExcluded`), while **crediting an already-earned entitlement
through settlement is exempt and proceeds** (§9, restated above) — a
distinction between "can the player DO something new" (blocked) and "does
an already-fixed, already-legitimate financial outcome get realized"
(not blocked), not a distinction between "withdraw" and "wager"
specifically. Whether the player can subsequently WITHDRAW the resulting
balance is a **Stage 3D withdrawal-governance question this ADR does not
cover and does not resolve**: `docs/decisions/0024-stage3d-withdrawal-
governance-final-gate.md` and `docs/architecture/withdrawal-state-
machine.md` contain no RG/self-exclusion gate today — withdrawal has never
been wired to `rg.EvaluateEligibility` or any self-exclusion check in this
codebase. This is a **pre-existing gap this section neither introduces nor
closes**, consistent with CLAUDE.md's "no uncontrolled scope expansion" —
flagged here only so `SETTLE_NORMALLY`'s crediting behavior is not
mistaken for an implicit claim about withdrawal access, which is a
separate, currently-open surface owned by `ledger-finance`/Stage 3D.

### 14.7 Financial correction requirements (`VOID_ON_SELF_EXCLUSION`) — a new void sub-reason, not a new `transaction_type`

If the resolved policy is `VOID_ON_SELF_EXCLUSION`, the ledger posting is
**identical in shape** to ADR 0038 §8.1's existing void flow — `Dr
player_locked` / `Cr player_cash`/`player_bonus`, full stake `S` (or the
post-settlement reversal-chain variant, §8.1's second row, if settlement
had already posted between the event concluding and the void being
processed — a race this section does not need to resolve differently from
how §8.1 already handles any late-arriving void), `transaction_type =
'sportsbook_void'`, unchanged. **This section does not propose a new
`transaction_type`** — that enum is `ledger-finance`'s file (ADR 0038) to
extend, not this one's.

> **Correction flagged by `sportsbook`'s independent review (Stage
> 4H-B0-R5, Wave 3): the "identical in shape... unchanged" claim above
> does not hold for every open-bet shape.** ADR 0038 §8.1 defines only two
> timing variants (before any settlement; after a *complete* settlement).
> It does not define a third: self-exclusion catching a multi-leg/
> bet-builder bet **after one or more legs have already partially settled**
> (ADR 0038 §8.2), leaving only `original_stake − R` locked. This
> paragraph's "full stake `S`" framing is therefore only correct for a
> single (or as-yet-fully-unresolved multi-leg) bet; for a
> partially-settled multi-leg bet, `VOID_ON_SELF_EXCLUSION` needs to void
> the **remaining** locked balance, not the original `S` — a gap now
> tracked in ADR 0038's "Open decisions referred upward," item 7, for
> `ledger-finance` to close. Until that item is closed,
> `VOID_ON_SELF_EXCLUSION` should not be treated as fully specified for a
> multi-leg/parlay bet caught mid-partial-settlement, even though it is
> fully specified for a single bet or an as-yet-untouched multi-leg bet.

**Recommended: yes, record this as a distinguishable void SUB-reason**,
for reporting/audit clarity, expressed as metadata/reason-code, mirroring
a pattern ADR 0038 itself already anticipated rather than inventing a new
one: §8.4's own treatment of a hypothetical future player-initiated
withdrawal recommends it be "distinguished by `reason_code`/`causation_id`
rather than a new `transaction_type`" if it is ever built — a
self-exclusion-triggered void is the same shape of problem (a
player-status-driven cause sharing an identical ledger posting with a
market-driven cause) and should be resolved the identical way: a
`void_reason` (or equivalently-named) metadata value such as
`player_self_exclusion`, sitting alongside whatever `market_cancelled` /
`data_error` / `push`-class reasons ADR 0038's void flow already
distinguishes, never a fork in `transaction_type`.

**One distinction worth flagging to `ledger-finance` explicitly, not
resolved here**: ADR 0038 §8.1 currently states void "is always
provider/event-initiated," and distinguishes it from a hypothetical
player-initiated withdrawal (§8.4) on exactly that axis. A
self-exclusion-triggered void is neither — it is **platform/compliance-
initiated**, a third causation category `sportsbook`/`ledger-finance`'s
own void model does not yet name. This section flags that ADR 0038 §8.1's
"always provider/event-initiated" framing would need a small amendment
(by `ledger-finance`, in their own file) if `VOID_ON_SELF_EXCLUSION` is
ever the value actually configured for a real jurisdiction/tenant — it is
not this section's place to make that edit, only to surface that it would
be needed. Likewise, §8.1's idempotency key (a provider void reference)
does not apply to a platform-initiated void; a real implementation would
need an idempotency key derived from the triggering restriction/bet pair
(e.g. `(tenant_id, bet_id, self_exclusion_event_id)`) instead — again,
`ledger-finance`'s to formalize, flagged here only so it is not missed.

> **Dependency note — amended 2026-09-25 (ADR 0088 §13, Stage 10 W1).**
> The `VOID_ON_SELF_EXCLUSION` consumer, when authorized, reuses ADR
> 0088's void operation (both timing variants), adds
> `player_self_exclusion` to `void_reason` by its own migration, and needs
> its own ADR 0019 actor row (platform/compliance-initiated) and the ADR
> 0038 §8.1 "always provider/event-initiated" amendment; it never uses the
> test-support route. *(The consumer itself is out of Stage 10 W1 scope —
> ADR 0088 §1.2, ruling R-2 — and remains `NOT IMPLEMENTED`; ADR 0042's
> default is not re-asked. ADR 0088's void operation is itself
> `MOCK`-driven in-house mode. Note also that ADR 0088's
> void-after-settlement is a `sportsbook_rollback` + `sportsbook_void`
> composite, not the single "reversal-chain" posting the paragraph above
> cites from ADR 0038 §8.1's superseded second row.)*

> **Finding flagged by `bonus-engine`'s independent review (Stage 4H-B0-R5,
> Wave 3), from the wagering-requirement-integrity angle, not decided
> here.** `VOID_ON_SELF_EXCLUSION` reuses ADR 0038 §8.1's void posting
> verbatim (confirmed above), which means a bonus-funded locked stake that
> gets voided this way returns its full `player_bonus` amount — but the
> wagering-progress **debit** that already posted at the bet's lock time is
> never itself reversed by that void, because the derived-read wagering-
> progress query (`financial-transaction-flows.md` §13) is not yet
> specified to net a debit against a later reversal of its own transaction.
> Net effect as currently specified: a player whose bonus-funded bet is
> voided by this policy keeps full wagering-requirement credit for a stake
> that was, financially, never actually risked. This is **not a defect
> introduced by this section** — the same gap exists for an ordinary
> (non-self-exclusion) sportsbook void of a bonus-funded lock, and would
> recur for any future casino contingent-state feature — but
> `VOID_ON_SELF_EXCLUSION` is a concrete, platform-triggered, high-volume
> trigger of it once configured, so it is flagged here as well as at its
> root cause. Filed as the same open item `financial-transaction-flows.md`
> §13 now tracks; not resolved by this ADR, and not a reason to reconsider
> §14's two-value policy design itself.

### 14.8 Future policy evolution — versioned, auditable configuration, never a code constant

`OpenBetSelfExclusionPolicy` is stored as **versioned, auditable
configuration**, resolvable as-of a point in time, exactly mirroring two
patterns already established elsewhere on this platform rather than
inventing a third: ADR 0031's rule versioning (a jurisdiction/tenant/brand
-scoped rule row, evaluated at request time against currently-effective
rows) and ADR 0021's rounding-rule versioning ("versioned and stored with,
or resolvable as-of, the transaction... a versioned, stored rule
identifier per transaction"). Concretely, the same shape as those two:
a scoped configuration row keyed on `(JurisdictionCode, tenant_id NULL-
able, brand_id NULLable)` per §14.2, carrying `policy_value`, an
`effective_from` (and, if ever needed, `effective_to`), and a stable
version identifier — resolved fresh at the moment self-exclusion becomes
effective (§14.3), never cached, never a Go constant or `if jurisdiction
== "X"` branch. A jurisdiction's regulatory requirement changing later
(a regulator newly mandating `VOID_ON_SELF_EXCLUSION` where it previously
required or allowed `SETTLE_NORMALLY`, or vice versa) is then a
**configuration change**, auditable exactly like any other rule change on
this platform, never a code deployment.

### 14.9 What remains a human/legal/compliance decision — restated crisply

The architecture above makes either answer expressible, per jurisdiction
and per tenant/brand, without this document choosing one. What it
explicitly does NOT decide, and what §11 above already flagged in
narrative form, restated now as a precise configuration question:

**Which of the two policy values — `SETTLE_NORMALLY` or
`VOID_ON_SELF_EXCLUSION` — should be the platform-wide DEFAULT for any
jurisdiction/tenant/brand that has not explicitly configured one, and does
either specific jurisdiction this platform is actively targeting (Anjouan,
per the fixed licence; or any of the Europe/LATAM jurisdictions under
consideration) have an existing legal requirement one way or the other —
remains a human/legal/compliance decision this document does not make.**
This document has no visibility into Anjouan's or any candidate European
or LATAM jurisdiction's specific regulatory text on this exact question,
and does not assume an answer for any of them either way, for the same
reason §11 already gave: this is squarely the shape of question CLAUDE.md's
"When to stop and ask" section names — jurisdiction-specific legal
interpretation this specialist is not positioned to make unilaterally on
the platform's behalf. `identity-compliance`'s only claim in this section
is that whichever value(s) a human/compliance decision ultimately
selects — a single global default, or a distinct value per jurisdiction —
can be configured, versioned, audited, and enforced by the architecture
above without a further redesign or a code change.

### 14.10 Mid-partial-settlement void — resolved (Stage 4H-B0-R7, closes ADR 0038 "Open decisions referred upward" item 7 for `VOID_ON_SELF_EXCLUSION`)

This closes the gap `sportsbook`'s Stage 4H-B0-R5 Wave 3 review flagged
against §14.7 above ("full stake `S`... is only correct for a single...
bet") and ADR 0038's own item 7: what a `VOID_ON_SELF_EXCLUSION`
execution must post for a multi-leg/bet-builder bet caught **after** one
or more legs have already partially settled (ADR 0038 §8.2), leaving
`original_stake − R` in `player_locked` rather than the full original
stake `S`.

**Answer, applying — not redesigning — the mechanism `ledger-finance`
already specified for exactly this class of problem**
(`ledger-accounting-model.md` §6.3.3.1/§6.4):

- The void MUST release only the bet's **currently-remaining** locked
  balance, per origin (cash/bonus), never the original stake `S`. This is
  §6.3.3.1's own **variant 2** query — the identical `correlation_id`-keyed
  recovery query used to recover a bet's cash/bonus split at unlock time,
  with the `transaction_type = 'sportsbook_bet'` filter **dropped** so it
  nets the original lock against every later `sportsbook_partial_
  settlement`/`sportsbook_cashout`/`sportsbook_void`/`sportsbook_rollback`
  transaction sharing the same `correlation_id`.
- That recovery read MUST execute inside the **same database transaction**
  as the void posting, after taking the identical lock discipline §6.4's
  HR-3 requires for every release-authorizing read on a sportsbook posting
  path: an advisory lock (or `SELECT ... FOR UPDATE`) scoped to
  `(tenant_id, correlation_id)` — the **bet**, not the delivery/event —
  taken before the read. `VOID_ON_SELF_EXCLUSION` is not a new,
  exempt release path; it is one more caller of the same release
  mechanism every other sportsbook unlock case uses, and it must fail
  closed on an empty/short/negative-remainder result exactly as HR-3
  already requires (escalated as an integrity alert, never treated as
  "release everything" or "release nothing").
- Entries: `Dr player_locked_cash <remaining cash-origin>` /
  `Cr player_cash <remaining cash-origin>`, and separately
  `Dr player_locked_bonus <remaining bonus-origin>` /
  `Cr player_bonus <remaining bonus-origin>` — `ledger-accounting-
  model.md` §6.3.3.2's own **case C-void** shape (each origin returns to
  its own account, no mirror pair, no proportionality question), sized to
  the **remaining** per-origin amounts rather than the original `C`/`B`
  split. For a single-origin bet, or a multi-leg bet with no prior partial
  settlement, remaining equals original, so this changes nothing observable
  from today's undifferentiated `Dr player_locked S / Cr player_cash|
  player_bonus S` framing — the fix only changes behavior once a partial
  settlement has actually posted, which is exactly the state §14.7's
  "identical in shape... unchanged" claim did not hold for.
- This confirms `sportsbook`'s own inference in ADR 0038's item 7 ("most
  likely `Dr player_locked <remaining>` · `Cr player_cash`/
  `player_bonus <remaining>`, mirroring §8.1's 'before settlement' row but
  substituting the remaining balance for the original `S`") as the
  specified answer for `VOID_ON_SELF_EXCLUSION`'s own trigger, per that
  item's own request that `ledger-finance` state the entries and confirm
  with `identity-compliance`. **`identity-compliance` confirms this
  reading.** Formalizing the equivalent third-timing-variant row in ADR
  0038 §8.1's own text remains `ledger-finance`'s file to edit — this
  section closes the question only for its own trigger (`VOID_ON_SELF_
  EXCLUSION`), restating no more of ADR 0038 than item 7 already licenses,
  per this platform's "no specialist redesigns shared architecture
  unilaterally" rule.
- Status: **RESOLVED (architecture) — still `NOT IMPLEMENTED`**, exactly
  like the rest of §14. No sportsbook void-execution code exists in this
  codebase yet (`internal/rg`'s own package doc comments are explicit that
  nothing here enumerates, voids, or settles a bet), so there is no
  `internal/rg` code to change for this item — it is a specification a
  future void-execution implementer (`sportsbook` + `ledger-finance`,
  jointly) must follow, not code this stage delivers.

### 14.11 Wagering-progress netting — the integration requirement `VOID_ON_SELF_EXCLUSION` places on `ledger-finance`'s parallel gate-G-3 design

`financial-transaction-flows.md` §13's `OPEN QUESTION` (`bonus-engine`'s
Stage 4H-B0-R5 Wave 3 finding, restated for `VOID_ON_SELF_EXCLUSION`
specifically at §14.7 above) is being designed this round by
`ledger-finance` as the gate G-3 wagering-progress-integrity fix.
`identity-compliance` does not redesign that mechanism here — this
section states the one concrete integration requirement `VOID_ON_SELF_
EXCLUSION`'s own trigger path places on it, found while checking whether
the two compose.

**Finding: a netting mechanism keyed solely on `reverses_transaction_id`
will not see the single most common shape a self-exclusion-triggered void
produces.** ADR 0038 §8.1 defines two posting shapes for a void: "before
settlement" (a plain new transaction, `Dr player_locked / Cr player_cash
|player_bonus` — **no `reverses_transaction_id` at all**; §8.1's own table
states this, and its prose gives the reason: "nothing was posted in
error") and "after settlement" (a full reversal chain that DOES set
`reverses_transaction_id`, pointing at the settlement). Per §14.6/§14.7
above, a self-exclusion-triggered void of an **open** bet is, by
construction, the first shape — the bet has not settled, that is why it
is still open, and settlement is RG-exempt and proceeds normally on the
rare occasion it beats the void (§14.6) — so the void transaction `VOID_
ON_SELF_EXCLUSION` produces will typically carry **no `reverses_
transaction_id` back to the lock at all**. `financial-transaction-
flows.md` §13's own prose frames the fix as excluding "any debit whose
originating transaction was subsequently reversed **via `reverses_
transaction_id`**" — if implemented literally (a join on `reverses_
transaction_id` only), the fix would correctly net a post-settlement
reversal but would **silently fail to net the pre-settlement void case**,
which is exactly the case `VOID_ON_SELF_EXCLUSION` most commonly produces.
That would reproduce, for the single highest-volume RG-triggered scenario,
the exact wagering-integrity gap the fix exists to close.

**Integration requirement (binding on whichever mechanism `ledger-finance`
designs, stated as a contract this section's caller must satisfy, not as
a design of that mechanism):** the wagering-progress-netting query MUST be
able to find and net a lock-time `player_bonus` debit against **any**
later transaction that reverses it economically, keyed on
**`correlation_id`** (the bet-scoped identifier ADR 0038 §3/§5 already
establishes and `ledger-accounting-model.md` §6.3.3.1 already uses for
exactly this class of "find every later event on this same bet" join),
**not exclusively** on `reverses_transaction_id` — because `correlation_
id` is the one linking field ADR 0038 guarantees is present on **every**
transaction type in a bet's lifecycle (lock, partial settlement, cashout,
void, rollback) regardless of timing, while `reverses_transaction_id` is
only ever populated on the post-settlement reversal-chain shape.
Concretely: for each `player_bonus`-debiting lock transaction, net against
the sum of later `sportsbook_void` transactions, and `sportsbook_rollback`
transactions that reverse the lock itself (never a rollback that reverses
a *settlement*), sharing its `correlation_id` and crediting the released
bonus-origin amount back to `player_bonus` — not merely those that also
happen to carry `reverses_transaction_id`. **`sportsbook_void` and a
lock-reversing `sportsbook_rollback` are the only transaction types named
here because they are the only ones that unconditionally nullify: they
mean the stake was never genuinely at risk.** `sportsbook_partial_
settlement` and `sportsbook_cashout` are explicitly **not** unconditional
nullifiers and are not part of this list — a partial settlement is a
market fact (risk-preserving: the stake was genuinely risked, and its
payout credit to `player_bonus` must never net against the lock), and a
cashout's treatment is a live, unresolved policy question, not a
mechanical one (`ledger-accounting-model.md` §6.5.10 FD-1 — as of this
writing that document's §6.6.5 predicate fails closed on
`sportsbook_cashout` rather than treating it as nullifying). Whether and
how partial-settlement/cashout transactions ever net against a lock is
governed entirely by `ledger-accounting-model.md` §6.6.5's exhaustive
per-type classification table, not by this list — a future implementer
MUST consult that table rather than infer nullification from this
section's example.

If `ledger-finance`'s chosen mechanism already keys on `correlation_id`
(consistent with §6.3.3.1's own precedent, and the natural join key given
`identity-compliance`'s reading of ADR 0038 above), no gap exists and this
section is confirmation, not a defect report. If it keys on `reverses_
transaction_id` alone, this is the concrete case that will not compose
with `VOID_ON_SELF_EXCLUSION`'s own trigger path — flagged here for
`ledger-finance` to close in its own G-3 design. `identity-compliance`
does not select or build the fix, per this stage's own scope boundary
(Workstream D does not touch Workstream E's/ledger-finance's wagering-
progress model design).

No `internal/rg` code change accompanies this section: no wagering-
progress-netting query exists in this codebase yet to modify
(`financial-transaction-flows.md` §13 confirms it is a derived read not
yet built), and no sportsbook void-execution code exists to thread a
`correlation_id` through. This is a requirement on a not-yet-built
mechanism, stated now so the two designs compose when both are eventually
built, not a fix to something that exists today.

### 14.12 As-of timestamp — re-confirmed sufficient; no remaining gap found

Stage 4H-B0-R6 (security finding S-8) made `ResolveOpenBetSelfExclusionPolicy`
reject a zero `AsOf` outright and require it be the self-exclusion's own
effective timestamp (`player_restrictions.starts_at`), never a bare "now"
read at whatever later instant enforcement happens to run — closing the
tampering window where a permissive config change could be raced into
effect between the self-exclusion instant and enforcement. Re-checked this
stage for any remaining gap the directive's re-mention of this item might
be pointing at:

**Closed — one-line justification**: `AsOf`'s source
(`player_restrictions.starts_at`) is append-only at the database level
(confirmed by `rg_integration_test.go`'s `TestPlayerRestrictions_RLS_
NoUpdatePossible`, which proves a direct `UPDATE` against
`player_restrictions` is rejected), and the jurisdiction floor it is
evaluated against can no longer be backdated
(`ErrJurisdictionFloorBackdated`, same Stage 4H-B0-R6 fix) — with both the
temporal input and the configuration timeline individually
immutable-after-the-fact in the direction that matters, no combination of
the two can reproduce S-8's original race. No code change accompanies
this section.

### 14.13 Audit ordering — the causal chain, checked end to end, one gap found and closed

The directive's audit-ordering requirement — a regulator must be able to
reconstruct self-exclusion event → enumeration → policy resolution → void
execution → wagering-progress netting, in the correct causal order, from
audit records alone — checked against every audit record this package
currently produces:

1. **Self-exclusion event**: the `player_restrictions` row itself
   (append-only, `starts_at` immutable — §14.12) plus its own creation
   audit trail (ADR 0026).
2. **Enumeration**: `self_exclusion_enumeration_runs` rows and their
   `rg.self_exclusion_enumeration_run.{created,started,completed,failed}`
   audit entries. **Gap found and closed this stage**: the `created`
   entry already carried `restriction_id` in its metadata, but `started`/
   `completed`/`failed` did not — a regulator reconstructing the chain
   from those three entries alone would have had to join back to the
   (mutable, `UPDATE`-based) `self_exclusion_enumeration_runs` table to
   learn which restriction a given run belonged to, which is not "from
   audit records alone." Fixed in `internal/rg/self_exclusion_
   enumeration.go`: all four enumeration-run audit entries now carry
   `restriction_id` in metadata. A second, related bug found and fixed
   while adding test coverage for this: `FailEnumerationRun` had no test
   coverage before this stage and did not backfill `started_at` the way
   `CompleteEnumerationRun` already does, so failing a run directly from
   `pending` (a transition its own `WHERE` clause always allowed) violated
   migration 0043's `CHECK (dispatch_status = 'pending' OR started_at IS
   NOT NULL)` outright — fixed to backfill identically.
3. **Policy resolution**: `rg.open_bet_self_exclusion_policy.resolved`,
   which already carried `RestrictionID` in metadata when the caller
   supplied it. Restated this stage as a hard requirement on the caller,
   not merely optional provenance: `ResolveOpenBetSelfExclusionPolicyParams.
   RestrictionID`'s own doc comment now states that any real
   self-exclusion-enforcement call MUST supply it (only a non-enforcement
   preview/dry-run resolution may omit it), so this link is never missing
   for a real enforcement decision. The Go type stays a pointer rather
   than becoming a required field, since Resolve is a general primitive
   used by more than one caller shape, not itself the enforcement listener.
4. **Void execution / wagering-progress netting**: does not exist in this
   codebase yet (no sportsbook implementation). §14.5 already specifies
   the required per-bet audit entry; this stage adds the explicit
   chaining requirement a future implementer MUST satisfy for the trail to
   be reconstructable: that per-bet audit entry's `Metadata` MUST include
   the triggering `restriction_id` (§14.5's "Actor... attributed to the
   specific self-exclusion event/restriction row" already implies this in
   spirit, but an audit query joins on `Metadata`/`TargetID`, not on
   `Actor`, so it is restated here as an explicit metadata requirement,
   not merely an actor-attribution one) and the `self_exclusion_
   enumeration_runs.id` of the run that discovered the bet; and — per
   §14.11 above — the void's own ledger transaction must carry the
   standard `correlation_id` ADR 0038 already guarantees for every posting
   on a bet, so the eventual wagering-progress-netting record is itself
   joinable back to the same chain via that same field, closing the loop
   without inventing a new identifier.

With (2)'s fix landed and (3)'s requirement restated as binding, the
causal chain is reconstructable end to end today for everything this
stage can actually build, and is stated unambiguously for a future
implementer for everything it cannot (4).

### Consequences (§14 addendum)

- `internal/rg`, `internal/kyc`, and ADR 0038's ledger posting shapes
  remain unchanged by this section — it defines a new, jurisdiction/
  tenant/brand-scoped configuration surface and a new self-exclusion-
  commit-triggered enumeration/audit step, not a redesign of any existing
  mechanism.
- A real implementation of this policy requires, at minimum: the scoped,
  versioned configuration table (§14.8); a listener on self-exclusion
  becoming effective that enumerates the affected Person's open sportsbook
  bets across tenants/brands and resolves + applies the policy per bet
  (§14.5); the void-sub-reason metadata convention, once `ledger-finance`
  formalizes it (§14.7); and no change at all to the settlement code path
  itself for the `SETTLE_NORMALLY` branch (§14.6). None of this is built
  in this stage — architecture only, per this stage's own gate.
- One question is carried forward, unresolved, exactly as §11 already
  flagged it, now stated as a configuration default rather than a
  narrative recommendation: the platform-wide default value of
  `OpenBetSelfExclusionPolicy`, and any specific jurisdiction's mandated
  override, per §14.9 — a human/legal/compliance decision, not a technical
  one this ADR settles.
- **Stage 4H-B0-R7 (Workstream D) additions, §14.10-§14.13**: the
  mid-partial-settlement void case is now fully specified (§14.10,
  applying `ledger-finance`'s existing §6.3.3.1/§6.4 recovery mechanism,
  not redesigning it); the integration requirement `VOID_ON_SELF_
  EXCLUSION` places on `ledger-finance`'s in-flight gate-G-3
  wagering-progress-netting design is stated explicitly (§14.11: net on
  `correlation_id`, not exclusively `reverses_transaction_id`); the
  as-of-timestamp fix (S-8) is re-confirmed closed with no remaining gap
  (§14.12); and the audit-ordering causal chain is checked end to end,
  with one real "reconstructable from audit records alone" gap found and
  closed in `internal/rg/self_exclusion_enumeration.go` (started/
  completed/failed entries now carry `restriction_id`) plus an unrelated
  latent bug found and fixed in the same file (`FailEnumerationRun` now
  backfills `started_at`, matching `CompleteEnumerationRun`) (§14.13).
  `FindMissingEnumerationRuns`/`FindStalledEnumerationRuns` are now wired
  into an actual running scheduler
  (`internal/rg/enumeration_sweep.go`), closing Stage 4H-B0-R6's "zero
  callers in a running system" security finding for both queries (only
  one of which the finding named, but both had the identical gap).

### Cross-references (§14 addendum)

- §9-13 above (this same ADR, Stage 4H-B0-R4) — the reasoning this section
  builds a configurable architecture around, unmodified.
- `docs/architecture/09-sportsbook-architecture.md` §8 — sportsbook's own
  confirmed RG consumption points (acceptance, cashout, settlement-exempt),
  which §14.6 relies on without restating its detail.
- `docs/decisions/0038-sportsbook-accounting-and-ledger-integration.md`
  §8.1 (void posting shape, reused verbatim by §14.1/§14.7) and §8.4
  (the `reason_code`/`causation_id` precedent §14.7 extends to a second,
  platform-initiated causation category).
- `docs/decisions/0031-risk-and-limits-engine.md` §5 (HARD_LIMIT/
  CONFIGURABLE_LIMIT precedence, generalized narrowly by §14.2) and §9/§10
  (`JurisdictionCode`/`LicensingMode` contract, reused verbatim by §14.2/
  §14.4).
- `docs/decisions/0021-multi-asset-accounting.md` (ADR 0021) — versioned,
  resolvable-as-of-transaction-time rounding-rule configuration pattern,
  reused by §14.8.
- `docs/architecture/11-kyc-aml-rg-architecture.md` — confirms self-
  exclusion's own platform-wide-by-default scope is unchanged by this
  section (§14.4) and confirms no "withdraw-only" rule for self-excluded
  players is currently documented anywhere on this platform (§14.6).
- `docs/decisions/0024-stage3d-withdrawal-governance-final-gate.md` and
  `docs/architecture/withdrawal-state-machine.md` — confirmed to carry no
  RG/self-exclusion gate today, cited in §14.6 as a pre-existing,
  out-of-scope gap this section does not close.

## Stage 4H-B1 Wave 1 — Bonus Engine RG and Identity/Multi-Account Integration Contracts (task 4HB1-05)

Status: Accepted (design/contract-only — Wave 1 of an 8-wave gated
implementation per `docs/governance/task-registry.md`'s Stage 4H-B1; no
code, no migration). Owner: `identity-compliance`. Written in parallel
with `bonus-engine`'s Wave 1 domain-model authorship
(`docs/architecture/10-bonus-engine-architecture.md`, task `4HB1-02`) and
`security`'s Wave 1 RBAC/audit/tenancy contract (task `4HB1-08`) —
reconciliation across all Wave 1 outputs happens before any Wave 2 coding
dispatch, per the stage's own gate. This section does not redesign
anything doc 10 §1.2/§1.3 (Grant state machine) or §5/"4. RG" (RG
composition, already frozen and citing this ADR verbatim) — those are
read as binding, confirmed consistent with §1-§14 above, and extended
here with the checkpoint-by-checkpoint specification and the identity/
multi-account contract the stage directive additionally asks for.

### 15. RG integration contract — exact Bonus Engine lifecycle checkpoints

**15.1 Confirmed unchanged.** Composition order (RG first, short-
circuiting Risk — ADR 0031 §1), the frozen `EvaluateEligibility`/
`EligibilityParams`/`Decision` signature (§1 above, restated verbatim in
doc 10's "4. RG"), and doc 10 §5's own corrected rule that a denial at
GRANT/ACTIVATION time is a `cancelled` Progress transition while a denial
at CONVERSION time leaves the Grant `completed` (non-terminal, retryable)
— never forfeited, per §2 above's "no clawback" principle. Nothing below
revises any of that; this subsection only makes doc 10's general rule
exhaustive against doc 10's own actual state machine.

**15.2 Checkpoint table.** Every row is a point where Bonus Engine (or the
Gamification/Reward Orchestrator components sharing doc 10's lifecycle)
MUST call `rg.EvaluateEligibility` fresh, in the same transaction as the
state-changing effect, before that effect commits — no checkpoint may be
satisfied by a cached result from an earlier checkpoint, mirroring
§14.3's "resolved fresh, every time" discipline restated for doc 10's
actual state machine (§1 above already stated this in general/
illustrative terms; this table is the exhaustive version against doc 10's
frozen states):

| Checkpoint | Bonus Engine event (doc 10 §1.2/§1.3) | `WalletID` | RG capability actually exercised (per `internal/rg` as it exists today) |
|---|---|---|---|
| **Campaign/Offer eligibility (opt-in)** | `issued`→`activated` triggered by *player action* (explicit opt-in click, doc 10 §1.3) | `uuid.Nil` — no wallet operation yet, mirroring §1's mission/tournament-entry precedent | Account-status + self-exclusion (`CodePlayerAccountNotActive`/`CodeSelfExcluded`) only; no wallet check is possible or needed at this step |
| **Grant creation** | *(none)*→`issued`, whether by automated rule evaluation on a canonical event, provider callback, or staff/bulk action | `uuid.Nil` if no bonus wallet is resolved yet at `issued` time (doc 10's own state table: `issued` may precede any wallet effect); the bonus wallet once resolved, otherwise | Account-status + self-exclusion, plus wallet-status (`CodeWalletNotActive`) once a wallet is resolved |
| **Activation** | `issued`→`activated` (deposit-triggered, staff, or opt-in paths in doc 10 §1.3) | The bonus wallet being credited | Full three-part decision, evaluated **independently** of the grant-time check even when grant and activation happen in the same request — never inferred from the earlier, already-passed check (§14.3's "never cached" discipline) |
| **Coded-bonus redemption** | A player-supplied code validated against an Offer — doc 10 §2's "Coupon" row: "the only lifecycle novelty is the *trigger* for `issued`→`activated`... otherwise identical to a deposit/cash bonus" | Same as Grant creation/Activation above — redemption is not a fifth checkpoint, it is the SAME `issued`/`activated` checkpoints reached through a different trigger, and gets no lighter-weight treatment | Same as Grant creation/Activation |
| **Conversion / release** | `completed`→`converted` (wagering multiplier satisfied, cashback window closed, marketplace/points redemption crediting a wallet) | The destination wallet (spendable/cash balance, or a different bonus-class wallet for a bonus-to-bonus conversion) | Full three-part decision; per doc 10 §5's corrected rule, `Allowed == false` here does **not** forfeit — it leaves the Grant `completed`, retryable |
| **Bulk assignment, per player** | Any of the above, triggered by a staff bulk-assignment action targeting N players at once | Per the individual player's own resolved wallet, exactly as a single-player grant | Identical to Grant creation — see §15.4 for why this is its own row despite being "the same checkpoint N times" |

**15.3 What is explicitly NOT a checkpoint.** Showing/listing an Offer to
a player (a campaign-eligibility *query* used for marketing display —
"which offers can this player see") is not itself a value-granting,
value-activating, or value-consuming action, and is therefore not
required to call `EvaluateEligibility` by this contract — consistent
with §1's own scoping ("every player-facing bonus/gamification action
that grants, activates, or lets a player consume value"). Recommended,
not required by this ADR: a self-excluded player should not be shown
promotional Offers, as a UX/reputational matter — but that is a product/
marketing-list filter, not an RG enforcement point, and building it does
not substitute for or weaken the checkpoints in §15.2 in any way.

**15.4 Bulk assignment — the checkpoint most likely to be silently
weakened, stated explicitly.** A bulk-assignment action (a staff operator
targeting a segment/campaign at many players at once) MUST evaluate
`EvaluateEligibility` once **per targeted player**, inside that player's
own grant-creation transaction, exactly as a single-player grant would —
never as a single pre-check pass whose result is reused for every
subsequent player in the batch (a real TOCTOU risk for a batch that can
run for minutes across thousands of players, the same class of race
`lockPerson`'s own doc comment exists to close for a single player,
restated here for N players run sequentially or concurrently). A player
who fails the check is **skipped**: their grant is never created, and a
`bonus.grant_denied_by_rg_policy` audit entry is written for that player
alone (§3 above), while the rest of the batch proceeds. The batch as a
whole is never all-or-nothing on one player's RG denial, and a
campaign-level pass never substitutes for each player's own account/
self-exclusion/wallet-status check.

### 16. Fail-closed contract — RG unavailable or erroring must never resolve to an implicit allow

Restating and making explicit, for the Bonus Engine implementation, the
discipline already binding on every other caller of `EvaluateEligibility`
in this codebase (`postBet`'s own error propagation; `identityresolution`'s
explicit "treat resolver-unavailable identically to Uncertain, never
silently create an unrestricted new Person" precedent, ADR 0027 §7):

- `EvaluateEligibility` returns `(Decision, error)`. A non-nil `error` —
  a database error, a failed wallet resolution, any failure that prevents
  the function from reaching a real `Decision` at all — is **not** a
  `Decision{Allowed: false}` and must never be treated as one by
  inference; it means the question was never actually answered. Every
  Bonus Engine call site MUST treat a non-nil error exactly like a hard
  failure of the entire enclosing operation: the transaction is aborted
  (rolled back), no Grant row is inserted or transitioned, no wallet is
  credited, and no Progress entry claiming a decision was reached is
  written. This is not a new rule invented here — it is what already
  happens by construction today at every existing `EvaluateEligibility`
  call site (an error return aborts the enclosing Go function before any
  write commits); this section exists only to make explicit that Bonus
  Engine's own implementation must preserve that property rather than,
  for example, wrapping the call in a "best-effort" pattern that logs the
  error and proceeds.
- A `Decision{Allowed: false}` (the ordinary denial path) is handled per
  §15's checkpoint table and doc 10 §5's `cancelled`/`completed`-
  non-terminal split — never re-interpreted as `Allowed: true` under any
  retry, timeout, or "the check was probably fine" fallback.
- **No override, bypass, or "skip RG" parameter exists on
  `EligibilityParams`, and this contract does not ask for one.** This
  restates §10 above's sportsbook rule verbatim for Bonus Engine's own
  implementation: a genuine operational need (e.g. a support agent
  manually completing a stuck bulk-assignment batch) is never satisfied
  by a runtime flag that skips this check — it is its own separately-
  audited, four-eyes-gated administrative action if it is ever needed,
  never a parameter on the eligibility call itself.
- This applies identically inside a bulk-assignment batch (§15.4): a
  partial-batch failure (RG returning an error for player K of N, e.g. a
  transient DB issue) skips and logs player K's own grant, and does not
  abort or roll back players 1..K-1's already-committed grants (each is
  its own transaction) — but it also never treats player K's error as an
  implicit allow for player K specifically.

### 17. The `OpenBetSelfExclusionPolicy` human decision — what Bonus Engine genuinely depends on, and the explicit configuration boundary for the gated portion

Per this stage's own instruction, §14's `OpenBetSelfExclusionPolicy`
default (ADR 0039 Decision 1) is **not selected here**. This section
instead determines, precisely, what slice of Bonus Engine functionality
that unmade decision actually blocks, versus what proceeds regardless —
the same fail-closed-by-scope discipline §14.9/ADR 0039 already apply to
sportsbook itself, extended to its one Bonus-relevant consequence.

**17.1 Ungated — proceeds now, depends on nothing this section flags.**
Every checkpoint in §15.2 — campaign/offer opt-in, grant creation,
activation, coded-bonus redemption, conversion/release, and bulk
assignment — resolves RG eligibility **as of the instant of that
specific action**, using `EvaluateEligibility`'s existing, already-
implemented self-exclusion/account-status/wallet-status check. None of
this depends on `OpenBetSelfExclusionPolicy` in any way: that policy
governs what happens to an *already-placed, still-open sportsbook bet*
when self-exclusion fires mid-bet (§14.1) — a question about a
**sportsbook** wager's own disposition, not about whether a Bonus Grant/
opt-in/conversion action is itself allowed right now. A Bonus Grant
funded entirely by cash, or wagered exclusively through casino play
(whose rounds resolve near-instantly — §2's own reasoning for why casino
needed no equivalent open-position policy), is completely unaffected by
Decision 1 remaining unmade. This includes ordinary wagering-progress
tracking for a bonus-funded **casino** bet — the open-position/self-
exclusion-timing ambiguity §11/§14 reasoned through is specific to a
sportsbook bet's long-lived, days-to-months-open liability window, which
casino rounds do not have.

**17.2 The one genuinely gated slice — already blocked by G-2; Decision 1
only widens, does not create, the block.** The concrete case the
directive asks about — "how an in-flight bonus-funded bet is treated if
self-exclusion fires mid-bet" — only exists at all once (a) a sportsbook
bet can be funded, wholly or partly, from a `player_bonus`-origin lock,
and (b) that bet's Grant can go terminal (`expired`/`cancelled`/
`forfeited`) while the bet is still open, and (c) a later settlement or
void credit arrives back against that now-terminal Grant. This is exactly
doc 10 §5's own "Terminal-Grant settlement-credit resolution" gap, which
ADR 0039 Decision 2 (options (a) re-forfeit / (b) route to cash / (c)
hold for review) already tracks as gate **G-2**, and which ADR 0039's own
record states, independently confirmed by `bonus-engine` and
`sportsbook`, **blocks bonus-funded sportsbook wagering directly** —
`player_locked` phase 2 code for the bonus-funded case is "not being
built or enabled until this is resolved" (ADR 0039 Decision 2, "Where
implementation actually stands"). In other words: **bonus-funded
sportsbook wagering as a feature does not exist yet to be affected by
Decision 1 in the first place** — it is already gated shut by Decision 2,
a separate, already-recorded human decision, independently of whether
Decision 1 is ever answered.

Self-exclusion firing mid-bet on a bonus-funded sportsbook stake is one
of the two named trigger paths into that SAME already-gated Terminal-
Grant question (doc 10 §5's "Cross-reference" paragraph: "(i) completing
an already-fully-satisfied wagering requirement after self-exclusion, and
(ii) a locked stake settling or voiding against a Grant that has already
gone terminal for any reason... resolve to the same single choice"). Once
G-2 (Decision 2) is answered, the specific self-exclusion-triggered
instance of it additionally requires `OpenBetSelfExclusionPolicy`
(Decision 1) to be configured for the relevant bet's jurisdiction,
because `ResolveOpenBetSelfExclusionPolicy` fails closed (`Configured:
false`) with no jurisdiction floor set (§14.9) — so a bonus-funded bet's
self-exclusion-triggered disposition (does it void, returning the lock to
`player_bonus` for G-2 to then dispose of; or settle normally) cannot
even be computed for a jurisdiction with no configured value, regardless
of Bonus Engine's own state.

**17.3 The explicit configuration boundary.** Bonus Engine's own
template/feature configuration (doc 10 §7/§8's jurisdiction/tenant/
brand-scoped configuration surface — the same shape as every other
jurisdiction-varying rule on this platform) MUST carry a distinct,
off-by-default gate — e.g. `bonus_funded_sportsbook_wagering_enabled` —
that is **never** implied by, or silently defaulted from, the existence
of an Offer or Campaign that would otherwise permit it. This gate may
only be set to enabled, per jurisdiction, once **both**:

1. ADR 0039 Decision 2 (Terminal-Grant settlement-credit resolution, gate
   G-2) has a recorded human answer and its corresponding state-machine
   transition exists in doc 10 §1.2, **and**
2. For that specific jurisdiction, `OpenBetSelfExclusionPolicy` resolves
   `Configured: true` (a jurisdiction floor is set, per ADR 0039 Decision
   1 and §14.2/§14.9 above) — either because the platform-wide default
   has been decided and applies, or because that jurisdiction has its own
   explicit override.

Neither Bonus Engine, nor any other domain, may substitute a code-level
assumption (e.g. "default to `SETTLE_NORMALLY` if unset") for condition
2 — that would exactly reproduce the tampering/fail-open risk
§14.9/ADR 0039 already reasoned through for sportsbook itself, now
reachable through a Bonus Engine feature flag instead of a direct
sportsbook code path. This is the one place this stage's Bonus Engine
design has a hard, named dependency on an unmade human decision; every
other checkpoint in §15 is unaffected, per §17.1.

## Identity / multi-account abuse contract (directive's Part B, task 4HB1-05)

### 18. Person-level history read — the capability that exists, and the one that does not

**18.1 Confirmed: `internal/identityresolution` is not, and must not
become, a per-action read surface for Bonus Engine.** This restates §4
above, re-verified against the current code
(`internal/identityresolution/register.go`): `PersonResolver.Resolve` is
called from exactly one production entry point,
`RegisterPlayerWithResolution`, itself called only from the HTTP
registration handler. There is no exported function in this package a
Bonus Engine eligibility check could call at grant/conversion time, and
this contract does not ask for one to be added — `identityresolution`
answers "does this NEW REGISTRATION correspond to an existing Person," a
question asked once, at account creation, not "does this Person already
hold N grants."

**18.2 Confirmed: `internal/identity` has no cross-brand/cross-tenant
PlayerAccount read today, and this contract does not build one.** Checked
directly against `internal/identity/player_account.go`: every read
function (`GetPlayerAccountByID`, `GetPlayerAccountByEmail`,
`ListPlayerAccounts`) operates under the caller's own tenant-scoped RLS
(`player_accounts`' sole policy is `tenant_isolation`, migration 0010 —
no platform-wide read policy exists, the identical gap
`internal/rg.CreateStaffRestriction`'s own doc comment already discloses
for `ScopePlatform`). There is therefore no existing function of the
shape "list every PlayerAccount linked to Person P, across every tenant/
brand" for Bonus Engine to call, and building one is **not** part of this
contract — it would be new, unreviewed cross-tenant read surface, exactly
the kind of unilateral shared-architecture change CLAUDE.md's specialist
rules reserve for `architect`+`security`, not something this ADR
authorizes by itself.

**18.3 What Bonus Engine should actually do instead — and why it needs no
new `internal/identity` capability at all.** The correct, minimum-surface
design is to make the Person-level query one Bonus Engine answers from
**its own data**, never a live cross-tenant join at query time — the
identical pattern `player_restrictions` already established (`person_id`
stored directly on the row, §1 above; `rg.ListRestrictionsForAccount`
resolves a Person once and then queries `player_restrictions WHERE
person_id = $1`, which works across tenants only because that table's own
RLS was deliberately built to allow it, migration 0037's `player_self_
read` policy). Concretely:

- Every Bonus Engine Grant row already carries `person_id`, resolved once
  at grant-creation time from **the same `rg.EvaluateEligibility` call
  §15.2 already requires** — `Decision.PersonID` is returned by that
  exact call, so no *additional* read into `internal/identity` is needed
  to obtain it (confirmed against `rg.go`: `Decision.PersonID` is
  populated from `identity.GetPlayerAccountByID(...).PersonID`, already
  resolved as a side effect of the mandatory RG check). This is also
  independently confirmed as already-frozen doc 10 architecture: doc 10
  §6 ("Activity/Event taxonomy") states every canonical event envelope
  Bonus consumes already carries `person_id` alongside
  `player_account_id` — so a Grant created from an event-driven trigger
  has `person_id` available from the event itself, and a Grant created
  from a direct API call (opt-in, staff action, bulk assignment) has it
  from the RG call. Either way, zero new plumbing into
  `internal/identity`/`internal/identityresolution` is required.
- Bonus Engine's own duplicate-grant/anti-abuse query (§19) is then an
  ordinary query **against Bonus Engine's own Grant table**, filtered
  `WHERE person_id = $1`, never a query that needs to resolve or
  enumerate a Person's PlayerAccounts at all. This sidesteps the missing
  cross-tenant `player_accounts` read entirely — Bonus Engine never needs
  to ask "which accounts does this Person have," only "how many grants of
  class C has this Person already received," which its own data answers
  directly.
- **RLS consequence, stated precisely so it is not assumed away**: for
  this query to see every relevant Grant across BRANDS under the SAME
  tenant, Bonus Engine's `bonus_grants` (or equivalent) table's RLS needs
  only the ordinary `tenant_isolation` policy — a same-tenant, cross-brand
  anti-abuse rule (the recommended default scope, §7 above) requires **no
  new capability at all**, since a tenant-scoped connection already sees
  every brand's Grant rows under `tenant_isolation` (brands never span
  tenants). If the anti-abuse rule's configured scope (§19.1) is ever set
  wider than one tenant (a true cross-tenant, platform-wide anti-abuse
  rule — §7/§8 above's own flagged, human-confirmed-only case), a
  platform-scoped read path analogous to `persons`' own `persons_
  platform_scope_read_write` policy (migration 0015) would be needed.
  **That platform-scoped path does not exist today for any Bonus table,
  is not built by this contract, and must not be silently assumed** —
  exactly the same disclosed gap §18.2 names for `player_accounts`.

### 19. Duplicate grant prevention — a configurable Bonus Engine policy, never an RG/KYC concept

**19.1 Precise definition.** "Duplicate grant prevention" is a Bonus-
Engine-owned, configurable rule of the shape: *no more than N grants of
Offer-class C may be issued to Person P within scope S*, where:

- **N** is a configured integer (commonly 1, e.g. "one welcome bonus
  ever"), never hardcoded.
- **C** is a Bonus-Engine-defined classification tag on a Campaign/Offer
  (e.g. `welcome`, `first_deposit`, `reload`) — an ordinary
  Offer-configuration field, not a new taxonomy this ADR invents.
- **P** is the Person resolved per §18.3 (never the PlayerAccount — this
  is the entire point of using Person rather than PlayerAccount: it is
  what makes the rule see through a player registering under a second
  PlayerAccount at a sibling brand).
- **S** is the configured scope this rule is evaluated over — brand /
  tenant-wide (default recommendation, §7 above) / platform-wide
  (requires the not-yet-built platform-scoped read, §18.3) — using the
  exact same nullable `tenant_id`/`brand_id` scope pattern doc 10 §8
  already establishes for Campaign scoping itself, never a new scope
  concept.
- An optional **look-back window** (e.g. "within the last 12 months") may
  further narrow N — also configuration, not a hardcoded rule.

**19.2 Where and how it is evaluated.** At **grant creation** (§15.2),
after `EvaluateEligibility` allows and before the Grant row is inserted,
inside the SAME transaction: Bonus Engine queries its own Grant table
(`WHERE person_id = $1 AND offer_class = $2 AND <scope predicate>` per
§19.1, optionally windowed) and counts existing non-`reversed` grants of
that class; if the count already meets or exceeds N, the grant is **not
created** — recorded as its own, Bonus-Engine-owned denial (a
`bonus.grant_denied_by_duplicate_policy` audit entry, distinct from
`bonus.grant_denied_by_rg_policy` §3, since this is not an RG decision and
must never be coded or reported as one) rather than a `cancelled`
Progress transition with an RG `Code` — no Grant row exists yet at this
point for a Progress trail to attach to.

**19.3 Composition — independent of, never a substitute for, RG.** This
check is entirely separate from, and composes with, §15's RG gate exactly
as doc 10 §5/ADR 0031 §1 already establish the RG→Risk ordering:
`EvaluateEligibility` still runs first and still short-circuits on
denial; the duplicate-grant policy is evaluated only once RG has already
allowed, as one more Bonus-Engine-owned rule alongside its own Risk-based
checks (§4 above's standing boundary: "a bonus-specific... eligibility
rule belongs in Risk's configurable engine or the Bonus Engine's own
template rules, never in `internal/rg`"). `internal/rg`/`internal/kyc`
are unchanged and gain no new concept from this section — Person-scoped
duplicate-grant prevention is, and remains, entirely Bonus Engine's own
configuration and query, never a restriction row, never a KYC state.

### 20. No automatic historical identity merging — confirmed, not attempted

Per this stage's explicit instruction and ADR 0027's own already-
established scope boundary (§4 above), this contract does **not**
trigger, request, or depend on any new historical Person-merge/
re-resolution logic. Confirmed against the actual code:
`identityresolution.RegisterPlayerWithResolution` is the only call site of
`PersonResolver.Resolve` in this codebase, and it runs exactly once, at
registration. Bonus Engine's duplicate-grant check (§19) works only with
whatever Person clustering already resulted from each PlayerAccount's own
original registration-time resolution outcome:

- If two of a Person's real-world accounts were correctly linked at
  registration (a `Match` outcome, e.g. because verified KYC evidence was
  available and matched) — §19's check sees both, correctly, with zero
  additional work.
- If two accounts were never linked because the resolver returned
  `NoMatch` or `Uncertain` (the honest, expected state of every
  registration today per `VerifiedAttributes.IsEmpty`'s own doc comment —
  no real KYC vendor is integrated yet, ADR 0027 §3) — §19's check will
  **not** catch that latent cross-brand duplication, because there is
  genuinely no shared `person_id` between the two PlayerAccounts to query
  against. **This is a disclosed, accepted limitation, not a defect this
  contract is asked to close.** Building a retroactive matching/merge
  capability to close it would be exactly the kind of new, unreviewed
  identity-matching logic ADR 0027 §3/§10 already declined to invent
  ("matching on a single weak, unverified attribute... would be worse
  than no matching at all") and this stage's own directive explicitly
  forbids adding.
- Consequence stated plainly so it is not mistaken for a stronger
  guarantee than it is: §19's duplicate-grant policy is only as strong as
  the platform's existing Person-resolution coverage. As real KYC
  verification (ADR 0028) becomes available and registration-time
  resolution improves (more `Match` outcomes, fewer `Uncertain`/
  `NoMatch`), §19's effectiveness improves automatically, with no Bonus
  Engine code change — but this contract makes no claim that it closes
  every cross-brand duplicate-grant vector today.

### Consequences (§15-§20 addendum)

- `internal/rg`, `internal/kyc`, `internal/identity`, and
  `internal/identityresolution` are all unchanged by this section — it is
  a consumption contract for Bonus Engine's own, doc-10-defined lifecycle,
  plus a precise statement of what identity capability Bonus Engine's
  duplicate-grant policy needs (none beyond what
  `EvaluateEligibility`/the canonical event envelope already provide, per
  §18.3) and does not need (no cross-tenant `player_accounts` read, no
  identity re-merge).
- One dependency on an unmade human decision is named precisely and
  narrowly: bonus-funded sportsbook wagering as a whole remains gated
  behind ADR 0039 Decision 2 (already recorded, independent of this
  stage), and the self-exclusion-specific instance of that same gap
  additionally requires ADR 0039 Decision 1 (`OpenBetSelfExclusionPolicy`
  default) to be configured per jurisdiction before Bonus Engine may
  enable its own `bonus_funded_sportsbook_wagering_enabled` gate for that
  jurisdiction (§17.3). No other Bonus Engine functionality in §15's
  checkpoint table depends on either decision.
- A genuine, disclosed architecture gap, found rather than assumed:
  neither `player_accounts` nor (by extension) any future Bonus Grant
  table has a platform-scoped (cross-tenant) read path today; a
  same-tenant, cross-brand anti-abuse rule needs no new capability, but a
  true cross-tenant, platform-wide one would (§18.3). Not built here, not
  silently assumed away.
- This section imposes no change on doc 10's own frozen state machine
  (§1.2/§1.3), RG ordering (§5/"4. RG"), or accounting boundary (§6) —
  `bonus-engine`'s Wave 1 authorship and this section are read as jointly
  binding, to be reconciled (not re-litigated) in the stage's Wave 1
  reconciliation pass.

### Cross-references (§15-§20 addendum)

- `docs/architecture/10-bonus-engine-architecture.md` §1.2/§1.3 (Grant
  state machine, cited verbatim in §15), §5/"4. RG" (RG composition and
  ordering, confirmed unrevised in §15.1), §6 (event envelope's
  `person_id` field, the basis for §18.3's "zero new plumbing" finding),
  §8 (Campaign/Offer nullable-scope pattern, reused by §19.1's scope
  dimension).
- `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`
  Decision 1 (`OpenBetSelfExclusionPolicy` default) and Decision 2
  (Terminal-Grant settlement-credit resolution, gate G-2) — both cited in
  §17 as the precise, narrow dependency this section names.
- `internal/rg/rg.go` (`EvaluateEligibility`, `Decision.PersonID`) and
  `internal/rg/self_exclusion_policy.go`
  (`ResolveOpenBetSelfExclusionPolicy`'s fail-closed `Configured` field)
  — the exact functions §15/§17 build on, unmodified.
- `internal/identity/player_account.go` and migration
  `0010_create_player_accounts.up.sql` — the tenant-only RLS confirmed in
  §18.2.
- `internal/identityresolution/register.go` and `types.go` — the single
  registration-time call site confirmed in §18.1/§20.
- `docs/governance/task-registry.md`, Stage 4H-B1, task `4HB1-05` — this
  section's own origin and its stated Wave 1 sibling tasks (`4HB1-02`
  bonus-engine, `4HB1-08` security, `4HB1-09` architect) it is written to
  be reconciled against.

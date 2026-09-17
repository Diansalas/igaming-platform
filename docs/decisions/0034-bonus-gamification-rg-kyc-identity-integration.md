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

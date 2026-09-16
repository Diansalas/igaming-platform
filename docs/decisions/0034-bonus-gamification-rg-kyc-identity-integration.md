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

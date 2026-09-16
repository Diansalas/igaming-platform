# 17 — Gamification Engine Architecture

Status: **`NOT IMPLEMENTED` — architecture freeze only (Stage 4H-A).** No
package, no schema, no migration, no HTTP surface exists for any concept in
this document. Owned by `architect` as a cross-domain boundary document;
individual sub-domains are handed to their own specialists when a future
stage is authorized to build them. Nothing here authorizes implementation.

This document defines the Gamification Engine as a domain **distinct from**
the Bonus Engine (`10-bonus-engine-architecture.md`) and from the Reward
Orchestrator (owned separately). It defines Gamification's own
responsibility boundary and the contracts it presents to and consumes from
neighbouring domains. It does not redesign Bonus, Reward Orchestration,
Risk, RG, or the ledger — where it needs something from those domains it
states the dependency and stops.

## 1. Why Gamification is a separate domain at all

The Bonus Engine answers "this player qualified for a promotional offer;
here are the funds, the wagering rules, and the forfeiture conditions." It
is a **monetary-liability** domain: every grant it makes is a real
obligation carried on the ledger (`promo_liability`, `player_bonus`).

Gamification answers a different question: "what has this player *done*,
what does that *mean* in progression terms, and does it warrant a reward?"
Its state (points, XP, level, achievement unlocks, mission progress,
tournament scores, leaderboard ranks) is **behavioural** state derived from
platform activity. Most of it is not money and never becomes money without
a separate, explicit reward decision fulfilled by another domain.

Collapsing the two produces the failure mode this codebase already refuses
elsewhere (ADR 0031 §1, Risk vs RG): one component owning two different
kinds of authority, where a change to a cosmetic progression rule requires
touching the code path that creates financial liability. They compose; they
never merge.

| | Bonus Engine | Gamification Engine |
|---|---|---|
| Primary state | Campaign/Offer/Grant/Progress | Points, XP/level, achievements, missions, tournaments, leaderboards, streaks |
| Financial nature | Directly creates monetary liability | Creates *reward decisions*; creates no monetary liability itself |
| Trigger | Eligibility + opt-in + deposit/wager events | Canonical platform activity events |
| Output | Bonus funds, free spins, wagering obligations | A reward decision handed to the Reward Orchestrator |
| Time shape | Per-grant lifecycle | Continuous accrual plus bounded competitive periods |

**Hard rule.** Gamification never posts to the ledger, never grants a
bonus, never credits a points balance itself, and never fulfils a reward.
It decides *that* a reward is owed and *what kind*; the Reward Orchestrator
decides how it is fulfilled and routes it to whichever domain actually owns
the fulfilment mechanism (Bonus Engine, points accounting, Casino free
rounds, an external fulfilment vendor).

## 2. Sub-domains and their boundaries

Gamification is one domain with seven sub-domains, each with its own
definition/instance split and its own ownership of state:

| Sub-domain | Owns | Never owns |
|---|---|---|
| Points rules | When/how much to award or spend | The points ledger and balances (`ledger-finance`, doc 24) |
| XP & Levels | XP accrual rules, level definitions, thresholds, current level | Any spendable balance |
| Achievements & Badges | Unlock conditions, unlock records | The reward fulfilment they trigger |
| Missions | Mission definitions, opt-in, progression, completion (doc 19) | Reward fulfilment |
| Tournaments | Definition, entry, scoring, settlement decision (doc 18) | Prize fulfilment, prize funding |
| Leaderboards | Ranking computation, period management, immutable result snapshots | Prize payment |
| Streaks | Streak definition, continuity evaluation, streak state | Reward fulfilment |
| *(placeholder)* Raffles, mini-games | Nothing yet — §11 | — |

The Reward Marketplace (doc 20) is deliberately **not** in this table: it
is a separate sibling domain that *spends* points and *requests* reward
fulfilment. It is a consumer of Gamification's points rules, not part of
the Gamification Engine.

## 3. Points — Gamification's responsibility boundary

### 3.1 What Gamification owns and does not own

`ledger-finance` owns the points accounting model in
`docs/architecture/24-points-accounting-architecture.md` (not written as of
this document; a hard dependency, see §13). That document owns: the points
account/balance model, whether points reuse the existing double-entry
ledger machinery or a parallel append-only structure, precision and
representation, idempotency of a points movement, expiry mechanics at the
accounting layer, and reconciliation.

Gamification owns exactly one thing about points: **the business rule that
decides WHEN a points movement should happen and HOW MUCH it should be.**
It produces a points *instruction* — "award 120 points of type `X` to
player `P` because canonical event `E` satisfied rule `R`" — and hands it
off. It never writes a balance, never reads a balance as authoritative
outside a transaction it does not control, and never implements its own
idempotency for a points movement.

This mirrors exactly the split already established for money: `internal/
casino` decides *that* a bet should be posted and for how much;
`internal/ledger` owns the posting. No second balance system, for money or
for points.

### 3.2 Multiple point types — required, not optional

**Decision: the model supports multiple point currencies/types from the
start. A single global "points" currency is explicitly rejected.**

Rationale, in order of weight:

1. **Multi-tenancy.** Points earned on tenant A's brand must never be
   spendable on tenant B's brand. A single global points currency makes
   this an application-code discipline problem; a point-type registry with
   an owning scope makes it a data-model property, consistent with
   CLAUDE.md's "enforced by RLS, not by discipline in application code."
2. **The platform's own precedent.** ADR 0007 rejected "one generic
   balance row with a currency field" for money and required a distinct
   wallet per asset. Points are a different kind of quantity, but the same
   argument applies verbatim: a player may plausibly hold a permanent
   loyalty balance *and* a seasonal campaign balance *and* a
   tenant-specific arcade currency simultaneously, with different expiry
   and different redemption catalogues. One balance with a type field
   loses exactly the properties ADR 0007 was written to protect.
3. **Different lifecycle rules per type.** A permanent loyalty currency
   that never expires and a 30-day seasonal currency cannot share one
   expiry policy. Encoding "which expiry applies" per movement rather than
   per type pushes lifecycle logic into every earn path.
4. **Retrofitting is the expensive direction.** Adding a second point type
   to a single-currency model means migrating every historical balance,
   every redemption record, and every report. Starting with a registry and
   populating it with one row costs almost nothing.

The shape (conceptual only — `ledger-finance` owns the real definition):

```
PointType                       (registry, mirrors the Asset registry's role)
  code, display_name
  owning_scope: platform | tenant | brand
  tenant_id (NULL for platform-wide types), brand_id (nullable)
  spendable: bool               (a non-spendable type is a pure counter)
  expiry_policy_ref             (owned by doc 24, referenced here)
  active
```

Non-negotiable constraints Gamification asserts on that registry:

- A point type is **never** a monetary asset and never appears in the
  `assets` registry. Points must not be representable in a `Wallet`, and a
  points balance must never be summed into a monetary balance projection.
- Converting points into monetary value (if ever offered) is an explicit,
  audited, rate-carrying operation requested through the Reward
  Orchestrator — conceptually the same discipline as ADR 0007's
  `ConversionOperation`, never an implicit credit. **Whether such a
  conversion is offered at all is an open product/legal question (§14)** —
  a points-to-cash path may have gambling-regulatory implications this
  document does not resolve.
- Cross-tenant point transfer does not exist. There is no design for it and
  none should be added without an ADR.

### 3.3 Earning and spending decisions

Every points instruction Gamification emits carries, at minimum: the point
type, the player, the tenant/brand context, the signed amount, the
canonical event or gamification entity that caused it, the rule/definition
version that decided it, and a caller-supplied correlation id. The
*idempotency key* for the resulting movement is derived from the causing
canonical event's identity — so a redelivered event can never award points
twice — but the enforcement of that idempotency is the points accounting
layer's job (a database constraint), never a "check then insert" in
Gamification, per CLAUDE.md's financial rules applied by analogy.

Points earning and spending **caps**, if they are ever built, are Risk's
job, not Gamification's — see §7. **Specialist-review correction (P1
fix)**: an earlier draft of this section, of doc 02's Gamification
boundary row, and of §7.1 below all stated or implied that Risk already
covers points earning/spending caps today. It does not, deliberately:
`docs/decisions/0031-risk-and-limits-engine.md` §15h explicitly puts
points earning/spending OUT of Risk's scope "while points are non-
convertible, non-withdrawable, non-transferable" (requiring a
`risk.Evaluate` call on every points award would add a database round
trip and a fail-closed dependency to a non-financial event for no risk
benefit), and §18 confirms there is no way to express "at most N points"
in `internal/risk` today (no points-denominated threshold exists). **Net
effect, disclosed rather than silently implied away: no points earning or
spending cap mechanism exists anywhere in this architecture today** — not
in Risk (deliberately out of scope while points aren't money) and not in
Gamification (deliberately refuses to build its own limit engine, per §7
below and this document's own "reimplements neither" rule). If abuse
patterns ever require a points-specific cap, it requires either (a) ADR
0031 §15h being revisited (points becoming Risk-relevant, e.g. because
they became convertible — itself a legal/licensing decision, §14 item 3),
or (b) a narrowly-scoped Gamification-owned rate-limit specifically for
points issuance velocity (analogous to Bonus Engine's own
device/payment-fingerprint detector, `docs/architecture/10-bonus-engine-
architecture.md` §1.4 — a detector, not a generic limit engine). Neither
is designed or authorized this stage; this gap is recorded, not solved.

## 4. XP and Levels

### 4.1 Decision: XP and redeemable points are distinct concepts

**Decision: XP and redeemable loyalty points are two separate quantities.
They are not the same field, not the same balance, and not derived from one
another by definition.**

Rationale:

- **Spending must not demote.** If level is a function of a spendable
  balance, every redemption reduces the player's status. That is either a
  product defect or forces a second "lifetime points" counter alongside the
  spendable one — at which point the two concepts already exist and have
  merely been given confusing names.
- **They answer different questions.** XP measures cumulative engagement
  and is monotonic. Points measure an outstanding entitlement and are a
  balance. A balance and a monotonic counter have different invariants,
  different expiry semantics, different reconciliation rules, and different
  regulatory exposure.
- **They need different accrual rules.** A tenant will plausibly want
  "every €1 wagered gives 1 XP but only 0.5 points," or "this promotional
  period doubles points but not XP." A single quantity cannot express that
  without a multiplier hack that reintroduces the distinction anyway.
- **Only one of them is financially interesting.** Points are potentially
  a liability (they can be exchanged for something of value). XP is not.
  Keeping them separate keeps the financially-interesting quantity in one
  place.

Consequence: XP lives in Gamification's own state. Points live in the
points accounting layer (doc 24). An event can produce both an XP accrual
and a points instruction; they are two outputs of one rule evaluation,
never one write read two ways.

XP is monotonic under normal operation. The two exceptions, both explicit
and audited, are: (a) a **correction** for XP awarded from activity that
was subsequently voided (a rolled-back bet), which is a compensating
adjustment carrying the reversing event's identity, never an edit of the
original accrual; and (b) an administrative correction, which requires a
reason code and follows CLAUDE.md's audit rules for administrative
actions. **Specialist-review correction (P1 fix, F9)**: since §4.2 below
makes level a pure, recomputed PROJECTION of XP ("writing XP *is* writing
level"), an administrative XP correction is mechanically the SAME
capability as a level override by another name — a support agent denied a
level override above the four-eyes threshold could otherwise achieve the
identical outcome (crossing a level threshold, triggering
`level_rewards[]`, attaching permanently-`retained_benefits[]`) by
labeling it an "administrative XP correction" instead, with a single
actor and no second pair of eyes. **The corrected rule: an administrative
XP correction whose resulting level (recomputed from the corrected XP)
differs from the player's current level carries the IDENTICAL threshold
and four-eyes treatment §4.2 requires for a direct level override.** This
does not remove the correction path — §4.2's own principle, that an
override must never be achieved by fabricating XP, is correct and stands
— it controls the one path that remained uncontrolled.

### 4.2 Level model

```
LevelProgramme          scope: platform-template | tenant | brand
  code, display_name, point_type/xp_basis, active
  effective_from, effective_to

LevelDefinition         belongs to a LevelProgramme
  level_ordinal, display_name, xp_threshold
  level_rewards[]        (reward decisions, fulfilled via Reward Orchestrator)
  retained_benefits[]    (e.g. cashback %, withdrawal priority — expressed
                          as capability flags other domains READ, never as
                          logic Gamification executes)

PlayerLevelState        per (player, programme)
  current_level, current_xp, attained_at
  manual_override_level, override_reason, override_expires_at
  last_evaluated_at
```

Key invariants and decisions:

- **Level is a projection of XP plus the programme version in effect**, not
  an independently mutable field. It is recomputed, not incremented. This
  mirrors "balances are projections recomputed from ledger entries" — a
  stored `current_level` is a materialized convenience that must be
  reproducible from XP history and the programme definition, and any drift
  between the two is a defect, not a tolerable divergence.
- **Definitions are versioned with effective dates and are immutable once
  effective.** Editing a live threshold in place would silently
  retroactively demote or promote players. A change creates a new version
  with its own `effective_from`; the old version remains queryable so a
  historical level attribution can be explained to a player or a regulator.
- **Downgrade rules are configurable per programme and default to
  "no downgrade."** Supported modes, none implemented: `never` (default),
  `on_period_review` (level recomputed against activity in a rolling or
  calendar window), `on_inactivity` (level decays after a configured
  inactive period). Whichever mode applies, a downgrade is an audited event
  with a recorded cause, and **a downgrade never claws back a reward
  already fulfilled** — an already-fulfilled reward is final; the Reward
  Orchestrator's own reversal path is the only mechanism that could undo
  one, and level changes do not invoke it.
- **Player overrides** (support/VIP-manager granting a level) are a
  first-class, audited, optionally time-bounded field, never achieved by
  writing fake XP. Fabricating XP to produce a level corrupts the projection
  invariant above and destroys the audit trail. **Decision, firmed up per
  specialist review (F9 above depends on this being settled, not
  hedged)**: overrides above a configurable level threshold REQUIRE the
  same four-eyes treatment CLAUDE.md requires for manual balance
  adjustments — this is a binding requirement for the implementation
  stage, not merely a recommendation deferred to a future `security`
  review. `security` review at build time confirms the mechanism, it does
  not decide whether the control exists.
- **Segmentation**: a programme may be restricted to a segment (see §6).
  A player in no eligible programme simply has no level; "no level" is a
  valid state, not level zero by default.
- **Audit**: every level change (up, down, override, programme version
  change affecting a player) writes an audit record through the existing
  immutable `audit_log` mechanism — never a bespoke gamification audit
  store.

## 5. Achievements, badges, and streaks

### 5.1 Achievements and badges

An **Achievement** is a definition with an unlock condition evaluated
against canonical events and/or derived gamification state. A **Badge** is
a display artefact awarded by an achievement unlock (or, rarely, granted
administratively). The distinction is worth keeping because a badge may be
awarded by more than one achievement, and because a badge is purely
presentational whereas an achievement carries the condition and the reward.

```
AchievementDefinition   scope: platform-template | tenant | brand
  code, display_name, description
  unlock_condition (expressed over canonical event types/fields and/or
                    gamification state — never provider payloads, §6)
  repeatable: bool, repeat_window
  reward_decision (optional; fulfilled via Reward Orchestrator)
  badge_ref (optional), segment_ref, effective_from/to, version

AchievementUnlock       per (player, achievement, occurrence_ordinal)
  unlocked_at, causing_event_ref, definition_version, reward_decision_ref
```

Invariants: an unlock record is append-only. A non-repeatable achievement
has a uniqueness constraint on `(player, achievement)` enforced by the
database, so a redelivered event cannot double-unlock. Unlock evaluation
carries the definition version so a later definition change never
retroactively invalidates a historical unlock.

Relationship to missions and tournaments: an achievement may be *triggered
by* a mission completion or a tournament result (both are canonical
gamification events), but an achievement is **not** a mission. A mission is
opt-in-able, has explicit progression a player can see and a deadline; an
achievement is ambient and typically permanent. Modelling one as the other
produces either missions that cannot expire or achievements that need an
opt-in flow. They stay separate, and the only coupling is event
consumption.

### 5.2 Streaks

A **Streak** is a continuity counter over a repeating qualifying period:
"N consecutive days on which the player did X." It is its own concept
because its distinguishing state is *continuity* — the fact that a break
resets it — which neither a mission's progress counter nor an XP
accumulator expresses.

```
StreakDefinition   scope, code, period (day/week/custom),
                   qualifying_condition (canonical events),
                   grace_periods_allowed, timezone_basis,
                   milestone_rewards[] (via Reward Orchestrator),
                   effective_from/to, version
StreakState        per (player, streak): current_length, best_length,
                   last_qualifying_period, broken_at
```

Design notes, all deferred to implementation:

- The **timezone basis is a configuration property of the definition**, not
  a hardcoded UTC assumption. "Daily" means different things to a player in
  Bogotá and one in Warsaw, and ADR 0031 §4 already flagged
  calendar-aligned windows as needing jurisdiction-configured timezone
  semantics. Gamification must consume that same mechanism when it exists,
  not invent a second one.
- Streak evaluation must be **idempotent and replay-safe**: recomputing a
  streak from the qualifying-activity history must produce the same answer
  as incremental evaluation did. A streak whose only truth is an
  incrementally-mutated counter cannot be audited or corrected.
- **Specialist-review addition**: any comparison of a streak's continuity
  window against "now" (has this period's grace window elapsed, is the
  streak broken) MUST use Postgres `clock_timestamp()`, never `now()`,
  inside the implementing transaction. `now()` is frozen at transaction
  start and does not re-evaluate per statement — this is the EXACT
  mechanism that produced a real, previously-undiscovered self-exclusion-
  enforcement bug in `internal/rg` (Stage 4G-FINAL Part F), and any new
  time-window read introduced by this stage's designs (streaks here,
  mission expiry in doc 19 §6, tournament freeze/settlement-lag in doc 18
  §6/§8, Bonus Engine's cashback-window-closing job in
  `docs/architecture/10-bonus-engine-architecture.md` §1.3/§2) is exposed
  to the identical failure mode if a transaction evaluating it is ever
  queued behind an advisory lock or other blocking wait. This is a
  binding implementation requirement for this and every sibling
  time-window comparison named above, not a suggestion.
- Relationship to missions: a streak may *be* a mission's completion
  condition ("complete a 7-day streak"), and a mission may be one of a
  streak's qualifying conditions. They compose through gamification events.
  A recurring mission is not a streak — it has no continuity requirement —
  and a streak is not a recurring mission — it has no per-instance opt-in
  or reward per occurrence. See doc 19 §4.

## 6. Leaderboards

A leaderboard is a ranked ordering of participants by a score over a
bounded period within a defined segment. It is used by tournaments (doc 18)
and can also exist standalone (e.g. a permanent "top wagerers this month"
board with no prize).

### 6.1 The authoritative-truth rule — the central decision

**Decision: the authoritative source of ranking truth is an append-only
score-contribution journal in PostgreSQL. Every ranking — live, cached, or
settled — is a projection recomputed from that journal. No cache, no
stream-processing state store, and no analytical store is ever the
authoritative source of a rank that determines a prize.**

This is the direct analogue of this codebase's existing money rule
("balances are projections recomputed from ledger entries; Redis never
holds an authoritative balance"), and it is required for the same reason:
the moment a prize depends on a number, that number needs to be
reproducible, explainable to a disputing player, and reconcilable. A
rank held only in a Redis sorted set is none of those things — it cannot be
recomputed after an eviction, cannot be explained after a correction, and
cannot survive a void/rollback of the underlying activity.

```
ScoreContribution   (append-only)
  leaderboard_id, participant (player/team), tenant/brand
  canonical_event_ref, scoring_rule_version
  score_delta (signed — a void writes a compensating negative delta,
               never an edit or delete of the original)
  occurred_at, recorded_at, idempotency_key
```

Corrections are compensating contributions, never mutations. A rolled-back
bet produces a negative contribution carrying the reversal's identity — the
same shape the ledger already uses, and the same reason.

### 6.2 Infrastructure roles — reasoned, not selected

No infrastructure choice is made in this document. What *is* decided is
which role each candidate may ever play:

| Technology | Permitted role | Never |
|---|---|---|
| PostgreSQL | Authoritative score-contribution journal; authoritative settled-result snapshots; the source every other store is derived from | — |
| Redis (or equivalent) | Near-real-time display ranking for the player-facing UI; rate-limiting; presence | Authoritative for any rank that determines a prize; read as truth at settlement |
| ClickHouse (or equivalent) | Large-scale historical ranking analytics, long-period aggregation, reporting | Authoritative for a live or settled competitive rank |
| Event streaming (Kafka/NATS) | Transport of canonical events into the scoring path; ordering and replay | A durable system of record for scores; a substitute for the journal |

The real design question is not "which store" but "what refresh contract
does the product need." Two tiers, both expressible on the model above:

- **Near-real-time (default).** Display ranking refreshed on an interval
  (seconds to a minute), derived from the journal. The player-facing API
  returns a ranking together with its `computed_at`, so the UI can be
  honest about staleness rather than implying a precision the system does
  not have. This is the recommended default for essentially every product
  case.
- **Real-time.** Ranking recomputed synchronously on each contribution.
  Materially more expensive and only justified for a small, bounded
  participant set. **`RECOMMENDATION`: do not build this until a product
  requirement demands it**, per CLAUDE.md's scope rule.

**Settlement never reads a derived store.** At period close the leaderboard
is frozen and the settled ranking is computed from the journal inside a
single transaction, then persisted as an immutable snapshot. That snapshot
— not the cache, not the last displayed ranking — is what the Reward
Orchestrator is handed and what a dispute is resolved against.

### 6.3 Periods, freezing, and late events

Periods are `daily | weekly | monthly | custom (explicit start/end) |
rolling`, with the period's timezone basis a property of the leaderboard
definition (same reasoning as streaks, §5.2).

The hard problem is **late-arriving events**: a provider callback for
activity that occurred inside the period but is delivered after it closed.
The model must handle this explicitly rather than by accident:

- A contribution's eligibility is determined by its **`occurred_at`**
  (business time), not `recorded_at` (arrival time).
- Each leaderboard defines a **settlement lag** — a configured grace window
  between period end and freeze, during which late contributions for that
  period are still accepted.
- After freeze, a late contribution for the frozen period is **recorded and
  rejected for scoring**, never silently dropped and never retroactively
  applied to a settled ranking. It is visible in the journal and surfaced
  for operational review.
- If a settled result must genuinely change (a material error, a confirmed
  provider protocol violation), that is a **recalculation**: a new snapshot
  version, an audited decision with a reason code, and a compensating
  reward decision through the Reward Orchestrator. The original snapshot is
  never edited. See doc 18 §9.

### 6.4 Tie handling and determinism

Ranking must be **totally ordered and deterministic**: two computations of
the same frozen journal must produce byte-identical rankings. Ties are
broken by a configured chain on the definition, evaluated in order; the
final element of the chain is always a deterministic, non-arbitrary
discriminator. Candidate tie-breakers: earliest time at which the
participant reached the final score, secondary metric (e.g. lower total
stake for the same net result), then a stable deterministic identifier.

Explicitly prohibited: resolving a tie by row insertion order, by a Go map
iteration, by a random draw at computation time, or by anything that
differs between two runs — the same class of non-determinism ADR 0031's own
review found and fixed in the risk evaluator's aggregation loop. If the
product genuinely wants a random tie-break, the randomness must be a seeded,
recorded draw that is part of the settlement record and reproducible.

**Shared-prize ties** (N participants tie for a rank, prize split) are a
prize-distribution concern, not a ranking concern — doc 18 §7.

### 6.5 Segmentation

A leaderboard's participant set is defined by a segment (§8). Cross-tenant
leaderboards **do not exist**: see §9.

### 6.6 Anti-manipulation controls

Leaderboards and tournaments are the most abuse-exposed surface in this
domain, because unlike a bonus they can pay out for *relative* performance,
which two colluding accounts can manufacture. Controls, all consuming
existing domains rather than reimplementing them:

| Concern | Control | Owning domain |
|---|---|---|
| Multi-accounting (one person, many accounts) | Read the already-resolved `Person`-keyed linkage each `PlayerAccount` carries (ADR 0027) — **specialist-review correction (P1 fix, F6)**: an earlier draft of this row named `internal/identityresolution` as the read surface, which directly contradicts `docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md` §4's explicit statement that "no separate read of `internal/identityresolution` is needed or appropriate — that package is registration-time orchestration, not a per-action read surface." The two documents are reconciled, not in conflict, once the distinction is stated: `internal/identityresolution` is the ORCHESTRATION that computes/updates a `PlayerAccount`'s `PersonID` linkage at registration/resolution time; Gamification never calls that orchestration itself (matching ADR 0034 §4) — it only READS the resulting, already-computed `PersonID` on each `PlayerAccount` row, the exact same read ADR 0034 §4 already relies on for its own bonus-eligibility purpose. Tournament/leaderboard collusion detection (a cross-participant check at settlement) and bonus eligibility (a single-player check at grant time) are different CONSUMERS of the identical `PersonID` field — neither invokes `internal/identityresolution`'s own registration-time logic | `Person.id`/`PlayerAccount.PersonID` (ADR 0027) |
| **Genuine two-party collusion (two DISTINCT people agreeing one deliberately loses/feeds score to the other) — `OPEN DECISION`, not solved by the row above** | **Specialist-review correction**: person-level resolution structurally cannot detect this — it only clusters the SAME physical person's accounts, and two colluding strangers are, correctly, two different persons. No behavioral/pattern detection (repeated head-to-head play between the same two accounts, one-sided loss patterns, device/IP/payment-method correlation short of common personhood) is designed here. This is named as the domain's own top concern in this section's opening sentence and must not be left silently answered by a control that doesn't cover it. A future stage must design this explicitly (candidate owner: `risk` as a new `RISK_SIGNAL` pattern, or a Gamification-owned detector analogous to Bonus Engine's own device/payment-fingerprint detector, §"Abuse-control's place in the lifecycle" in `docs/architecture/10-bonus-engine-architecture.md`) — not resolved this stage | `risk` and/or this domain — TBD, §14 |
| Wash/low-risk betting to farm score | Risk signals and exposure limits | `internal/risk` (ADR 0031) — §7 below |
| Excluded/restricted players scoring | RG eligibility check before any scoring or entry | `internal/rg` — §7 below |
| Voided activity inflating score | Compensating negative contributions | This domain (§6.1) |
| Demo/play-money activity scoring | Demo-mode activity is never scored — canonical events carry the real/demo distinction; a demo event contributes nothing | This domain |
| Bonus-funded wagering inflating score | **`OPEN DECISION`** — whether bonus-funded stake contributes to tournament score, and at what weight, is a product decision with real abuse implications. §14 |
| Score from a provider later found to have misreported | Recalculation path (§6.3) | This domain + operations |

A suspicious participant is **flagged and withheld from settlement**, not
silently removed from the ranking: removing them changes everyone else's
rank with no record of why. The settlement snapshot records the full
ranking and the withheld/void participants separately.

## 7. Consuming Risk and RG — Gamification reimplements neither

### 7.1 Risk & Limits (ADR 0031)

**Hard rule: Gamification has no limit engine, no threshold concept, no
velocity concept, and no cap of its own. Every exposure control it needs is
expressed as a rule in `internal/risk` and enforced by calling
`risk.Evaluate`.** This is ADR 0031 §13's standing obligation ("every
future domain must declare its own Risk integration") applied to this
domain, and it is the same "kept separate, never collapsed" discipline ADR
0031 §1 established between Risk and RG.

Gamification operations that will need their own `risk.Operation` value and
their own `RiskRequest` shape (none exist today; each requires the
five-step `LimitKind`/enum extension ADR 0031 §12 documents, and none is
authorized by this document):

| Gamification operation | Exposure being controlled | Registered in ADR 0031 §16's proposed-`Operation` list? |
|---|---|---|
| Points award | Points earning caps per period; anomalous accrual | **No — and ADR 0031 §15h explicitly declines this integration** while points remain non-convertible/non-withdrawable/non-transferable (see §3.3 above); not merely "not yet built," actively out of scope until that premise changes |
| Points spend | Points spending caps per period | Same as above |
| Tournament entry | Entry count/velocity; buy-in exposure | Yes — `tournament_entry` |
| Mission opt-in | Concurrent mission caps; promotional exposure | **No — specialist-review correction**: an earlier draft of this table implied this was a registered candidate; it is not one of ADR 0031 §16's four proposed values (`bonus_conversion`, `tournament_entry`, `marketplace_purchase`, `reward_redemption`). A mission with no monetary entry fee or monetary reward creates no exposure for Risk to gate at all (mirrors ADR 0031 §15e's free-tournament-entry reasoning); if a future mission design ever charges a monetary entry fee, that requires filing a genuinely new proposal through ADR 0031 §16's process, not assuming this row already covers it |
| Reward redemption | Redemption value caps per period | Yes (conditional) — `reward_redemption` |
| Marketplace purchase | Purchase limits per item/period (doc 20) | Yes — `marketplace_purchase` |

Each follows `internal/casino`'s established pattern exactly: resolve the
`RiskRequest` server-side (including `JurisdictionCode` and `LicensingMode`
from their canonical sources, ADR 0031 §9/§10), call `risk.Evaluate` in the
same transaction as the state-changing effect, before it commits, and treat
any non-nil error as a `DENY` (fail-closed, ADR 0031 §6).

Note explicitly: several of these want limit kinds that ADR 0031 §4
deliberately did **not** implement (`count`, `velocity`). Gamification does
not get to work around that by building its own counter. Either the limit
kind is added to `internal/risk` through ADR 0031 §12's five-step process,
or the control does not exist yet and is disclosed as not existing.

### 7.2 Responsible Gaming

**Hard rule: Gamification calls `rg.EvaluateEligibility` before any
player-facing gamification action, exactly as `internal/casino` already
does. It does not define a second self-exclusion concept, does not read or
write `player_restrictions`, and does not cache an eligibility answer.**

A self-excluded (or otherwise RG-restricted) player must be unable to:

- earn or spend points;
- opt into, progress, or complete a mission;
- enter or score in a tournament;
- unlock an achievement or advance a streak;
- redeem a marketplace reward;
- receive a reward fulfilment triggered by any gamification event.

Ordering, mirroring `internal/casino/orchestrator.go`: **RG first, then
Risk**, with an RG denial short-circuiting before Risk is evaluated at all
(ADR 0031 §1). Never the reverse; Risk's `ALLOW` must never be in a
position to appear to override an RG denial.

Two genuinely open questions this document flags rather than decides (§14):
what happens to a self-excluded player's **accrued** points, XP, and
in-flight tournament entries (forfeit, freeze, or preserve-for-return?),
and whether an RG-restricted player may still passively *hold* a level.
Both have regulatory dimensions and belong to `identity-compliance` plus a
human decision, not to Gamification.

## 8. Canonical event consumption — the hard rule

**Hard rule: Gamification subscribes only to canonical platform events. It
never accepts, parses, or branches on a casino-, sportsbook-, or
payments-provider-specific payload. No provider identifier, provider game
id, or provider payload field ever appears in a gamification rule
definition.**

Consequences that follow directly:

- A mission, achievement, tournament scoring rule, or streak condition is
  expressed **in terms of canonical event types and canonical fields**. If
  a condition cannot be expressed that way, the correct fix is to extend
  the canonical taxonomy (owned elsewhere), not to reach into a payload.
- Gamification has **no dependency on `internal/casino`,
  `internal/payments`, or any future sportsbook package**, in either
  direction. It depends on the event contract only. This is verifiable the
  same way ADR 0031's Risk/RG separation is: by inspecting imports.
- Adding a new provider, or a second aggregator for the same game, must
  require **zero** gamification changes. If it does not, the boundary has
  been violated.
- An event's **`idempotency_key`** — never `event_id` — is the basis of
  every gamification idempotency key. **Specialist-review correction**:
  an earlier draft of this bullet named `event_id`; per
  `docs/architecture/22-canonical-activity-event-taxonomy.md`'s own
  explicit envelope contract, `event_id` is unique per PUBLISH and may
  legitimately differ across redeliveries of the same fact, while
  `idempotency_key` is unique per BUSINESS FACT and is the field actually
  designed to stay stable across redelivery. Deduping on `event_id` would
  not dedupe a redelivery at all — it would insert a second contribution
  with a new `event_id`, a real double-count bug. A durable at-least-once
  bus will redeliver; every consumer must be idempotent on
  `idempotency_key` (this is already `internal/eventbus`'s own stated
  rationale for carrying `EventID` from Stage 1, refined here now that
  the canonical taxonomy distinguishes the two fields explicitly).

The canonical taxonomy itself is
`docs/architecture/22-canonical-activity-event-taxonomy.md`, owned by the
Master Orchestrator and **not available at the time this document was
written**. This document therefore states Gamification's *requirements* of
that taxonomy as assumptions, not as facts about it (§13). The assumed
minimum every gamification-relevant event must carry: stable event id,
event type, occurrence time (business time) distinct from record time,
tenant and brand, player account, a real-vs-demo indicator, a
funding-source indicator where money is involved, an asset code and amount
where money is involved, a reversal/void linkage where applicable, and a
correlation id. If the delivered taxonomy lacks any of these, the
corresponding gamification capability is blocked, not worked around.

## 9. Multi-tenancy and scoping

### 9.1 Scope levels per asset type

| Gamification asset | Platform-wide | Tenant-wide | Brand-specific | Player-specific |
|---|---|---|---|---|
| Point type | Yes (definition) | Yes | Yes | No |
| Level programme | Template only | Yes | Yes | Override only (§4.2) |
| Level threshold | Template only | Yes | Yes | No |
| Achievement / badge definition | Template only | Yes | Yes | No |
| Mission definition | Template only | Yes | Yes | Assignment only |
| Tournament definition | Template only | Yes | Yes | Entry only |
| Leaderboard | **No** | Yes | Yes | No |
| Streak definition | Template only | Yes | Yes | No |
| Marketplace item | Template only | Yes | Yes | No |
| All player state (XP, level, progress, score, unlocks) | No | Always tenant-scoped | Brand-attributed | Yes |

### 9.2 The template-vs-instance distinction

A platform-wide row is a **template or definition the platform offers**,
never a live player-facing competition shared across tenants. Concretely:
there can be a platform-wide "Weekend Warrior" tournament *template* that
every tenant may instantiate; there can never be one live tournament whose
leaderboard mixes tenant A's and tenant B's players.

This is a deliberate, load-bearing decision, not a simplification:

- A cross-tenant leaderboard would let one tenant's operational behaviour
  (a generous bonus, a provider outage, a configuration change) determine
  another tenant's players' prize outcomes.
- It would require a prize pool funded across tenant financial boundaries,
  which has no coherent answer under the hybrid licensing model (ADR 0006):
  a bring-your-own-licence tenant's players competing under our platform
  licence's promotional rules is a regulatory problem, not a feature.
- It would make tenant-scoped RLS insufficient for the player-facing read
  path, which is the isolation mechanism the whole platform rests on.

Introducing any cross-tenant competition later requires a new ADR, human
sign-off, and a compliance review. It is not a configuration flag.

### 9.3 RLS strategy sketch

Following the dual-scope pattern already established by `player_restrictions`
(migration 0037) and `risk_rules` (migration 0041), and reviewed there:

- **Definition tables** (point types, level programmes/definitions,
  achievement, mission, tournament, streak, marketplace-item definitions)
  are dual-scope: `tenant_id NULL` means a platform-wide template;
  `tenant_id` set means tenant-owned, optionally narrowed by `brand_id`.
  RLS policies let tenant staff `SELECT` both their own rows and
  platform-wide rows, but `INSERT`/`UPDATE`/`DELETE` only their own — a
  tenant can adopt a platform template, never edit it.
- **Every policy carries the `app.player_account_id IS NULL` guard** that
  ADR 0031's own PostgreSQL/RLS review found missing on `risk_rules` and
  fixed. Definition tables are staff-scope only; a player-scoped connection
  must not read or write them. This is called out here specifically so the
  same gap is not re-introduced in a new domain.
- **Player state tables** (XP/level state, mission progress, unlocks, score
  contributions, tournament entries) are always tenant-owned (`tenant_id`
  NOT NULL) and use the dual `tenant_staff_scope` + `player_self_scope`
  (SELECT-only) pattern already used by `casino_launch_sessions` and
  `withdrawal_requests` — a player may read their own progression, never
  another player's, and never write any of it.
- **Leaderboard rankings are the one read-path subtlety**: a player-facing
  leaderboard legitimately shows *other players'* ranks. That must be
  served from a purpose-built, minimal-disclosure projection (display name
  or pseudonym, rank, score — never account identifiers, never anything
  that supports enumeration), not by relaxing RLS on the underlying score
  journal. Exact policy shape is for `security` review at build time; the
  invariant recorded here is that no leaderboard requirement ever justifies
  a policy allowing a player-scoped connection to read another player's row
  in an underlying table.
- Definition edits are versioned and audited through the existing immutable
  `audit_log`; the same immutability-trigger pattern used for risk rules
  applies.

## 10. Jurisdiction and licensing

Gamification **consumes** the canonical jurisdiction/licensing model (ADR
0006, `15-jurisdiction-and-licensing-model.md`) and invents nothing:

- A gamification definition may be scoped by `jurisdiction_code` and/or
  `licensing_mode`, using exactly the values and semantics ADR 0031 §9/§10
  established (empty means unscoped/wildcard on the definition side; the
  request carries exactly one value, resolved server-side by the caller).
- **No hardcoded jurisdiction-specific gamification rule may exist in
  code.** "Tournaments are not permitted in market X" is a configuration
  row, exactly like a payment-method restriction or a KYC threshold.
- Promotional/gamification mechanics are regulated differently across
  markets (some jurisdictions restrict prize draws, leaderboard
  competitions, loss-based scoring, or gamified progression aimed at
  increasing play). This document does not interpret any of that — it
  ensures the architecture can express a per-jurisdiction restriction, and
  flags the legal interpretation as a human decision (§14).
- Under ADR 0006's hybrid model, a bring-your-own-licence tenant's
  gamification configuration is governed by *its* jurisdiction, not ours.
  Platform-wide templates must therefore be scopeable by `licensing_mode`
  so a template written for our own licence never binds a BYOL tenant —
  precisely the gap ADR 0031 §10 closed for risk rules.

## 11. Raffles and mini-games — placeholder only

**`NOT IMPLEMENTED`, and not scheduled.** Recorded here only to show the
domain model does not foreclose them, per the instruction to leave room
without building room.

- A **Raffle** is: a definition, a ticket-acquisition rule (earned by
  activity or purchased with points), an entry pool, a draw at a scheduled
  time, and a result. It fits the existing shape — tickets are gamification
  state, the draw produces a result snapshot, and prizes are reward
  decisions handed to the Reward Orchestrator, exactly like a tournament's.
  The one genuinely new requirement is a **provably fair, auditable,
  reproducible draw mechanism** (seed commitment, recorded seed, verifiable
  result). That is a real design problem with regulatory implications and
  is not solved here.
- A **Mini-game** is: an attempt-granting rule (attempts earned or
  purchased), an outcome-generating mechanism, and a result. The critical
  boundary question — **is a mini-game outcome a game of chance for a
  prize, and therefore regulated gambling content requiring certification?**
  — is a legal question, not an engineering one. If the answer is yes, a
  mini-game is *casino content* behind the existing `CasinoProvider`
  abstraction, not a gamification feature. This document takes no position
  and explicitly refuses to build an uncertified RNG-driven prize mechanism
  inside the gamification domain.

Neither may be implemented without its own ADR, and the mini-game question
above must be answered by the human before any design work starts.

## 12. Relationship to neighbouring domains — summary contract

| Domain | Gamification's relationship |
|---|---|
| Canonical event taxonomy (doc 22) | Consumes only. Never accepts provider payloads |
| Points accounting (doc 24, `ledger-finance`) | Instructs. Never writes balances or owns the ledger |
| Reward Orchestrator | Hands off every reward decision. Never fulfils |
| Bonus Engine (doc 10) | Sibling. A bonus-shaped reward is fulfilled by Bonus *via* the Orchestrator, never called directly from Gamification |
| `internal/risk` (ADR 0031) | Consumes `Evaluate`. Never builds a limit engine |
| `internal/rg` | Consumes `EvaluateEligibility`. Never defines a second self-exclusion |
| `internal/identityresolution` | **Specialist-review correction**: does NOT call this package directly for anti-collusion (see §6.6's corrected row) — reads the `PersonID` linkage that package's registration-time orchestration already produced on each `PlayerAccount`, per ADR 0034 §4. Never builds its own linking, never re-invokes registration-time resolution as a per-action read |
| `internal/ledger` / `internal/wallet` | **No direct relationship at all.** Gamification never posts money |
| `internal/audit` | Writes through the existing shared mechanism only |
| Tenant configuration | Reads. Brand differences are config rows, never code paths |

## 13. Dependencies and assumptions

Stated as assumptions because the owning documents did not exist when this
was written. None of these is a fact asserted about another domain:

1. **`22-canonical-activity-event-taxonomy.md`** will exist and will carry
   at least the fields listed in §8. If it does not, the affected
   gamification capabilities are `BLOCKED`, not worked around.
2. **`24-points-accounting-architecture.md`** (`ledger-finance`) will own
   the points balance/accounting model, will support multiple point types
   (§3.2), and will provide database-enforced idempotency for a points
   movement keyed on the causing event.
3. **The Reward Orchestrator architecture** will accept a reward decision
   with a stable shape, will own fulfilment, reversal, and failure, and
   will be the only path from a gamification outcome to anything of value.
4. **`internal/risk`** will gain the operations and limit kinds §7.1 needs,
   through ADR 0031 §12's process. Until then the corresponding controls do
   not exist.
5. **The event bus** will be a durable, at-least-once broker by the time
   Gamification is built. `internal/eventbus`'s in-memory implementation is
   a `STUB` and is explicitly unsuitable (it is not durable, not ordered,
   not cross-process) for anything that determines a prize.
6. **Per-player jurisdiction resolution** still does not exist
   (`TODO(jurisdiction)`, ADR 0031 §9). Jurisdiction-scoped gamification
   rules will be expressible but unreachable in production until it does —
   the same `PARTIALLY IMPLEMENTED` caveat the casino path already carries.

## 14. Open decisions (human / product / legal — not resolved here)

1. **Does bonus-funded wagering contribute to tournament/leaderboard score,
   and at what weight?** Real abuse surface; a product decision with
   financial consequences.
2. **What happens to accrued points, XP, level, and in-flight tournament
   entries on self-exclusion?** **Specialist-review correction**: the
   GENERAL rule is already resolved, not open —
   `docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md`
   §2 decides this prospectively (already-committed effects stand,
   in-progress progression simply stops, nothing is clawed back). Only a
   narrow sub-question remains genuinely open: whether *unlocking* an
   already-fully-satisfied wagering/progress requirement after exclusion
   counts as mechanical settlement (allowed) or further progress
   (blocked) — flagged in ADR 0034 itself for `bonus-engine`/product to
   decide, not a gap in this document.
3. **Are points ever convertible to withdrawable monetary value?** If yes,
   points become a financial liability with AML and licensing implications
   and the design constraints change materially.
4. **Which jurisdictions restrict leaderboard competitions, prize draws, or
   gamified progression mechanics?** Legal interpretation; needed before
   any market-facing launch.
5. **Is a mini-game with a prize outcome regulated gambling content?**
   (§11.) Determines whether mini-games belong to Gamification at all.
6. **Does the product need true real-time ranking, or is near-real-time
   with an honest `computed_at` sufficient?** Materially affects cost.
7. **Is there any commercial requirement for cross-tenant competition?**
   §9.2 says no by default; overriding it requires an ADR and compliance
   review.

**Engineering design item, not a human/product/legal decision** (recorded
separately from the list above per specialist review, so it is not
mistaken for something needing legal sign-off when it is really
undone technical work): §6.6's anti-manipulation table names
person-level resolution as the collusion control, but that control only
detects one person operating multiple accounts — it cannot detect two
genuinely distinct people agreeing to feed score to one of them. This is
the domain's own stated top abuse concern and has no designed control
yet. A future stage must design behavioral/pattern detection for this
(candidate owner `risk`, as a new pattern-based signal, or a
Gamification-owned detector) before any tournament with a real prize
pool ships.

## Cross-references

- Tournaments: `docs/architecture/18-tournament-architecture.md`
- Missions: `docs/architecture/19-mission-architecture.md`
- Reward marketplace: `docs/architecture/20-reward-marketplace-architecture.md`
- Bonus Engine: `docs/architecture/10-bonus-engine-architecture.md`
- Domain boundaries: `docs/architecture/02-domain-and-service-boundaries.md`
- Risk & Limits: `docs/decisions/0031-risk-and-limits-engine.md`
- RG enforcement: `docs/decisions/0026-responsible-gaming-player-status-enforcement-foundation.md`
- Multi-wallet precedent: `docs/decisions/0007-multi-wallet-per-player-model.md`
- Jurisdiction/licensing: `docs/architecture/15-jurisdiction-and-licensing-model.md`, `docs/decisions/0006-hybrid-licensing-and-jurisdiction-model.md`
- Ledger invariants this document mirrors: `docs/architecture/06-wallet-ledger-architecture.md`

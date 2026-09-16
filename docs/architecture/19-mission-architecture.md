# 19 — Mission and Challenge Architecture

Status: **`NOT IMPLEMENTED` — architecture freeze only (Stage 4H-A).** No
package, no schema, no migration, no API. Owned by `architect` as a
cross-domain boundary document. A sub-domain of the Gamification Engine
(`17-gamification-engine-architecture.md`) — every rule there applies here
and is not restated except where missions add something.

## 1. What a mission is, and what it is not

A **Mission** (used interchangeably with "challenge" — one concept, two
product names) is a defined objective a player progresses toward over a
bounded period, with visible progress and a reward on completion.

The three things it is deliberately **not**:

| Not a… | Because |
|---|---|
| Bonus offer | A bonus creates monetary liability at grant time with wagering obligations attached. A mission creates nothing until completion, and its reward may not be money at all. A mission whose reward happens to be a bonus is a mission that *requests* a bonus-shaped reward through the Reward Orchestrator |
| Achievement | An achievement is ambient, usually permanent, and has no opt-in or deadline. A mission has a window, visible progress, and may require opt-in |
| Streak | A streak's defining property is continuity — a break resets it. A recurring mission has no continuity requirement between occurrences (doc 17 §5.2) |

Missions compose with all three through canonical events; they never
subsume any of them.

## 2. Conceptual model

```
MissionDefinition          scope: platform-template | tenant | brand
  code, display_name, description, version, status
  mission_kind: single_step | multi_step | recurring | scheduled
  opt_in_required: bool, opt_in_window
  eligibility_ref, segment_ref
  jurisdiction_scope[], licensing_mode_scope   (ADR 0006 / 0031 §9-§10)
  availability_window (start, end, timezone_basis)
  expiry_policy (absolute | relative_to_opt_in | relative_to_start)
  reward_specification      (a reward DECISION template — never a payment)
  partial_completion_policy (§6)
  prerequisite_mission_refs[]  (ordering/unlock dependencies)
  effective_from, effective_to, audit metadata

MissionStep                belongs to a MissionDefinition
  step_ordinal, display_name
  completion_condition      (canonical event types/fields only — §4)
  target_value, progress_aggregation (count | sum | max | distinct)
  optional: bool, ordering: sequential | any_order
  step_reward_specification (optional — a reward decision per step)

MissionInstance            one occurrence of a recurring/scheduled definition
  definition_version (pinned), tenant_id, brand_id, window

PlayerMissionState         per (player, mission instance)
  state: available | opted_in | in_progress | completed
       | expired | abandoned | cancelled | void
  opted_in_at, started_at, completed_at, expires_at
  eligibility_decision_ref

PlayerMissionStepProgress  per (player, mission instance, step)
  current_value, target_value, completed_at
  contributing_events[]    (append-only — §5)
```

As with tournaments, **definition and instance are separate, and an
instance pins its definition version.** Editing a definition must never
change the rules of a mission a player is already progressing.

## 3. Mission kinds

| Kind | Shape | Notes |
|---|---|---|
| **Single-step** | One completion condition, one target | The common case; "wager €50 this week" |
| **Multi-step** | Ordered or unordered steps, each with its own condition and optional per-step reward | Sequential steps unlock in order; any-order steps progress independently. Sequential ordering must be enforced on progression — an event satisfying step 3 while step 2 is incomplete contributes nothing and is recorded as non-contributing, not silently applied |
| **Recurring** | A definition producing instances on a schedule (daily, weekly, monthly, custom) | Each occurrence is an independent instance with its own progress and reward. **No continuity requirement between occurrences** — that is a streak (doc 17 §5.2). Recurrence timezone basis is a definition property, never a hardcoded UTC assumption |
| **Scheduled** | A one-off instance with a fixed future window | Used for campaigns and events |

Mission **chains** (mission B becomes available on completing mission A)
are expressed through `prerequisite_mission_refs`, not through a separate
"journey" entity. A chain is an ordering constraint on availability, and
nothing more, in this design. Anything richer (branching journeys,
CRM-style orchestration) is out of scope — CLAUDE.md's boundary document
already says a CRM/campaign journey builder is bought, not built
(`02-domain-and-service-boundaries.md`).

## 4. The canonical-event contract — the hard rule

> **A mission's completion condition is expressed in terms of canonical
> event types and canonical fields, never a provider-specific payload
> shape.**

This is the same hard rule as doc 17 §8, restated here because missions are
where it is most likely to be violated: a product request like "complete 20
spins on *Starburst*" invites a provider-specific shortcut.

What this requires concretely:

- A condition references a **canonical event type** (e.g. a canonical
  casino round-settled event, a canonical sportsbook bet-settled event, a
  canonical deposit-settled event) and **canonical fields** on it.
- Game and provider targeting uses the **platform's own identifiers** — the
  platform-assigned `casino_games.id`, never a `provider_game_id`, which
  `08-casino-integration-architecture.md` §4 already establishes is never
  promoted to canonical identity. Targeting a specific title is legitimate;
  targeting it by the provider's own id is not.
- **Missions must work across Casino and Sportsbook with no
  product-specific mission logic.** "Place 5 bets" must be expressible as
  a condition over a canonical bet event that both products emit, with the
  product as an ordinary filterable field — not as two separate mission
  engines or two code branches. If a mission genuinely needs a
  sportsbook-only attribute (e.g. minimum odds), that attribute must be a
  canonical field on the canonical sportsbook event, and a casino event
  simply never matches the condition. The asymmetry lives in the data, not
  in the code.
- Adding a new provider, or a second aggregator for the same title, must
  require **zero** mission changes.
- The mission domain has **no import of** `internal/casino`,
  `internal/payments`, or any future sportsbook package. Verifiable by
  inspection, the same way ADR 0031's Risk/RG separation is.

Where a condition cannot be expressed canonically, the correct action is to
request an extension to the canonical taxonomy (owned by
`docs/architecture/22-canonical-activity-event-taxonomy.md`) through the
dependency-request procedure, and to record the mission capability as
`BLOCKED` until then — never to reach into a payload.

## 5. Progression

Progression is driven entirely by canonical events, and follows the same
append-only discipline as leaderboard scoring (doc 17 §6.1) for the same
reasons — reproducibility, dispute resolution, and correctness under voids.

- **Every progress change records its contributing event.** A player who
  disputes "I definitely completed that mission" is answered with the event
  trail, exactly as `10-bonus-engine-architecture.md`'s Progress layer
  answers a disputed wagering calculation. This is not optional decoration;
  it is the reason the mission domain is trustworthy.
- **`current_value` is a projection** of the contributing events, not an
  independently mutable counter. Recomputing it from the trail must
  reproduce it. Drift is a defect.
- **Idempotency**: every contribution is keyed on the canonical event's
  **`idempotency_key`** — never `event_id`, per
  `docs/architecture/22-canonical-activity-event-taxonomy.md`'s explicit
  distinction between the two (`event_id` is unique per publish and may
  differ across redeliveries; `idempotency_key` is unique per business
  fact and is the one designed to stay stable — **specialist-review
  correction** to an earlier draft that named `event_id`, which would not
  have deduped a redelivery at all) — enforced by a database uniqueness
  constraint. A redelivered event can never double-count. This is a
  constraint, never a check-then-insert.
- **Voids and reversals decrement.** A rolled-back bet writes a
  compensating negative contribution carrying the reversal's identity. The
  original contribution is never edited or deleted.
- **Progress regression after completion**: if a void arrives for activity
  that had already completed a mission, the mission does **not**
  automatically un-complete and the reward is **not** clawed back
  automatically. It is flagged for review and, if material, handled through
  an audited correction with a reason code — the same
  "prefer-withholding-over-reversal" stance tournaments take (doc 18 §9),
  and for the same reason: a fulfilled reward may be economically
  irreversible. Recorded as an open decision (§11).
- **Ordering**: a durable at-least-once bus may deliver out of order.
  Progress aggregations must be order-insensitive where possible (`count`,
  `sum`, `distinct` are; `max` is; a sequential multi-step mission is not).
  For sequential missions, out-of-order delivery must be handled by
  business time (`occurred_at`), not arrival order, and a late event that
  would have satisfied an earlier step is applied on business time within
  the mission's own lag window — after which it is recorded as
  non-contributing, never silently applied.

## 6. Completion, partial completion, and expiry

**Completion** is a state transition that emits a reward decision (§7). It
is idempotent: a mission already `completed` cannot complete twice, and the
reward decision's idempotency key is derived from
`(mission_instance, player, step_or_completion)` so a retried transition
emits nothing new.

**Partial completion** is a per-definition policy, explicit rather than
implicit:

| Policy | Behaviour at expiry with incomplete progress |
|---|---|
| `none` (default) | No reward. Progress is retained for reporting only |
| `per_step` | Rewards already emitted for completed steps stand; no completion reward |
| `pro_rata` | A reward proportional to progress, per a defined rounding rule. Only valid where the reward kind is divisible (points, cash-equivalent) — never for indivisible rewards (a physical item, a single tournament entry) |
| `threshold` | A reward if progress reached a configured minimum fraction |

The rounding rule for `pro_rata` must be specified exactly and reviewed by
`ledger-finance` where the reward has monetary value — the same requirement
tournaments' split-prize arithmetic carries (doc 18 §7).

**Expiry** is driven by the instance's own `expires_at`, computed from the
definition's expiry policy (absolute date, relative to opt-in, or relative
to instance start). Expiry is an explicit, audited state transition, not an
implicit consequence of a timestamp comparison at read time — otherwise
"when did it expire" has no answer and partial-completion rewards have no
trigger point. A player must be able to see the deadline before opting in.
**Specialist-review addition**: whatever transaction actually performs
this comparison against "now" MUST read Postgres `clock_timestamp()`,
never `now()` — see `docs/architecture/17-gamification-engine-architecture.md`
§5.2 for why this is a binding requirement, not a stylistic preference
(it is the exact mechanism behind a real bug Stage 4G-FINAL found and
fixed in `internal/rg`).

**Abandonment** (a player explicitly leaving an opted-in mission) is
supported as a distinct state from expiry, because the two mean different
things for reporting and for whether re-opt-in is allowed. Whether
re-opt-in after abandonment is permitted is a definition property.

## 7. Rewards — via the Reward Orchestrator only

> **Mission completion produces a reward *decision*. The mission domain
> never credits a wallet, never credits a points balance, never grants a
> bonus, and never fulfils a reward itself.**

Identical boundary to tournaments (doc 18 §1). A `reward_specification` on
a definition or a step names a reward **kind and parameters**; the Reward
Orchestrator resolves it and routes it to whichever domain owns the
fulfilment mechanism. The mission domain does not import `internal/ledger`,
`internal/wallet`, or the Bonus Engine, and has no code path capable of
moving value.

Consequences:

- A **fulfilment failure does not roll back completion.** The mission is
  completed; the reward decision is the Orchestrator's to retry. A mission
  that flips back to incomplete because a downstream credit failed would be
  both wrong and unexplainable to the player.
- **Per-step rewards** are independent decisions with their own idempotency
  keys, emitted at step completion, not held until mission completion.
- Whether a mission reward requires the player to **claim** it, or is
  fulfilled automatically, is a definition property. A claim step is
  sometimes a regulatory or product requirement and the model must not
  assume automatic fulfilment.

## 8. Eligibility, opt-in, RG and Risk

Evaluated at **opt-in** (or at first progress for automatic missions) and
re-evaluated at **completion**, because eligibility can change mid-mission.

Order, non-negotiable, mirroring `internal/casino` and ADR 0031 §1:

1. **RG** — `rg.EvaluateEligibility`. Denial short-circuits before Risk.
   A self-excluded player cannot opt into, progress, or complete a mission,
   and cannot receive a mission reward. No second self-exclusion concept
   exists in this domain.
2. **Risk** — `risk.Evaluate` with a `mission_opt_in` operation (a new
   operation requiring ADR 0031 §12's extension process; it does not exist
   today). Controls concurrent-mission caps and promotional exposure. **The
   mission domain builds no cap, counter, or threshold of its own.**
3. **Mission-specific conditions** — segment membership, level/VIP tier,
   prerequisite missions completed, availability window open,
   jurisdiction/market permitted, brand membership, opt-in window open.

Each returns a distinguishable reason code. The eligibility decision
references are persisted on `PlayerMissionState` so a later dispute does
not require re-deriving state that has since changed.

A player who becomes RG-ineligible mid-mission has their mission state
frozen; what happens to the in-flight progress and any earned-but-unclaimed
reward is the open question doc 17 §14.2 already records, and is not
decided here.

## 9. Segmentation and targeting

A mission's audience is defined by a **segment reference**, not by a
condition expression embedded in the mission. Segment definition is a
capability this document assumes rather than designs — CLAUDE.md's own
boundary says CRM/segmentation tooling is bought before it is built, and
`02-domain-and-service-boundaries.md` repeats it. What this document
requires of whatever provides segments:

- A segment is **resolvable server-side** and never client-supplied.
- Segment membership is **evaluated at eligibility time**, and the
  evaluation result is recorded on the mission state — a player who was in
  a segment at opt-in does not lose an in-flight mission because a segment
  definition changed underneath them.
- Segments are tenant-scoped. A platform-wide mission template referencing
  a tenant segment is a configuration error.

Additional targeting dimensions that are *not* segments and are ordinary
definition fields: brand, jurisdiction, licensing mode, level/VIP tier,
asset/currency, product (casino/sportsbook), and prerequisite missions.

## 10. Multi-tenancy, scoping, and audit

Per doc 17 §9, without exception:

- **Definitions** are dual-scope (`tenant_id NULL` = platform-wide
  template; set = tenant-owned, optionally narrowed by `brand_id`). Tenant
  staff read both, write only their own; a tenant adopts a platform
  template, never edits it. All staff-scope RLS policies carry the
  `app.player_account_id IS NULL` guard (the gap ADR 0031's review found on
  `risk_rules`).
- **Instances and all player state** are `tenant_id NOT NULL`, RLS-enforced,
  with a SELECT-only `player_self_scope` policy so a player reads their own
  mission progress and nothing else.
- **A mission instance never spans tenants.**
- Brand differences are configuration rows, never code paths.
- Jurisdiction and licensing-mode scoping use ADR 0006 / ADR 0031 §9–§10's
  existing mechanism; no mission-specific jurisdiction concept exists.

**Audit** (existing immutable `audit_log`, never a bespoke store):
definition created/versioned/disabled, instance scheduled/opened/closed,
player opted in/abandoned, eligibility denial (with RG and Risk decision
codes), completion, expiry, partial-completion award, reward decision
emitted, and every administrative override or manual completion. Manual
completion by staff requires a reason code and is audited like any other
administrative action.

## 11. Open decisions

1. **Does a post-completion void un-complete a mission or claw back its
   reward?** §5 says no automatic claw-back, review instead. Product and
   compliance should confirm.
2. **Rounding rule for `pro_rata` partial completion** (§6) —
   `ledger-finance` must specify before any monetary partial reward ships.
3. **Is a claim step required** for mission rewards in any target
   jurisdiction? (§7) Legal/compliance.
4. **What happens to in-flight missions and unclaimed mission rewards on
   self-exclusion?** Inherited from doc 17 §14.2.
5. **Where do segments come from** (§9) — buy vs. build, and on what
   timeline. Not a mission-domain decision.
6. **Do bonus-funded and demo-mode activity count toward mission
   progress?** Demo does not (doc 17 §6.6); bonus-funded is the open
   decision inherited from doc 17 §14.1.

## 12. What is explicitly not designed here

Mission recommendation/personalisation, a branching journey builder, the
player-facing API and UI, notification and reminder mechanics, mission
analytics beyond what the standard reporting pipeline provides, and the
fulfilment mechanics of any reward kind.

## Cross-references

- Gamification Engine: `docs/architecture/17-gamification-engine-architecture.md`
- Tournaments: `docs/architecture/18-tournament-architecture.md`
- Reward marketplace: `docs/architecture/20-reward-marketplace-architecture.md`
- Bonus Engine (the Progress-trail precedent this mirrors): `docs/architecture/10-bonus-engine-architecture.md`
- Risk & Limits: `docs/decisions/0031-risk-and-limits-engine.md`
- Canonical game identity precedent: `docs/architecture/08-casino-integration-architecture.md` §4
- Domain boundaries: `docs/architecture/02-domain-and-service-boundaries.md`

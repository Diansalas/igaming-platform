# 18 — Tournament Architecture

Status: **`NOT IMPLEMENTED` — architecture freeze only (Stage 4H-A).** No
package, no schema, no migration, no API. Owned by `architect` as a
cross-domain boundary document. A sub-domain of the Gamification Engine
(`17-gamification-engine-architecture.md`) — every rule in that document
applies here and is not restated except where tournaments add something.

## 1. Scope and the one hard boundary

A **Tournament** is a bounded competition: a defined population of players
accrues a score over a defined window under a defined scoring mechanism,
and at the end a ranking determines who receives which prize.

**Hard boundary, stated once and load-bearing throughout this document:**

> **Tournament settlement produces a reward *decision*. It never directly
> credits a wallet, never credits a points balance, never grants a bonus,
> and never fulfils an externally-fulfilled reward itself.**

Settlement's entire output is an immutable result snapshot plus a set of
reward decisions handed to the **Reward Orchestrator**, which owns
fulfilment, retry, failure, and reversal. The Tournament domain does not
import `internal/ledger`, `internal/wallet`, or the Bonus Engine, and has
no code path that could move value. This is the same discipline that keeps
`internal/casino` out of the ledger schema: the domain that decides *that*
something is owed is never the domain that executes it.

## 2. Conceptual model

```
TournamentDefinition      scope: platform-template | tenant | brand
  code, display_name, description, version, status
  jurisdiction_scope[], licensing_mode_scope   (ADR 0006 / 0031 §9-§10)
  entry_model, eligibility_ref, segment_ref
  scoring_mechanism_ref, scoring_parameters
  window (start, end, timezone_basis), settlement_lag
  leaderboard_ref
  prize_structure_ref, tie_break_chain
  qualifying_activity_filter  (canonical event types/fields only — §5)
  effective_from, effective_to, created/updated audit

TournamentInstance        one running occurrence of a definition
  definition_version (pinned at instantiation — never floats)
  tenant_id, brand_id, window_start, window_end
  state: draft | scheduled | open | running | closed | frozen
       | settling | settled | cancelled | recalculating
  prize_pool_snapshot, participant_count

TournamentEntry           one player's participation
  instance_id, player_account_id, entered_at, entry_source
  entry_cost_ref (nullable — see §4)
  state: active | withdrawn | disqualified | void
  eligibility_decision_ref  (RG + Risk decisions recorded at entry time)

ScoreContribution         (append-only — shared with leaderboards, doc 17 §6.1)
TournamentResultSnapshot  (immutable, versioned — §8)
RewardDecision            (handed off; owned by the Reward Orchestrator)
```

**Definition/instance separation is mandatory.** A definition is a reusable
template; an instance pins the definition version it runs under, so editing
a definition can never change the rules of a competition already in flight.
A recurring tournament ("every Saturday") is a definition plus a schedule
producing many instances, never one long-lived mutable row.

## 3. Eligibility

Eligibility is evaluated at **entry time** and re-evaluated at
**settlement time**, because a player can become ineligible mid-tournament
(self-exclusion, account suspension, a restriction applied by staff).

Order, mirroring `internal/casino/orchestrator.go` exactly and
non-negotiably (ADR 0031 §1):

1. **RG** — `rg.EvaluateEligibility`. A denial short-circuits; Risk is
   never evaluated. Gamification defines no second self-exclusion concept.
2. **Risk** — `risk.Evaluate` with a `tournament_entry` operation, **only
   when the entry carries a genuine monetary cost** (an entry fee, or a
   prize path with real monetary value). **Specialist-review correction
   (P2 fix)**: an earlier draft called this an unconditional step for
   every entry — this contradicts
   `docs/decisions/0031-risk-and-limits-engine.md` §15e's own explicit
   position: "a free-entry tournament (no monetary fee, no monetary prize
   path) is out of scope for the same reason... it creates no monetary
   exposure," and §18 confirms `tournament_entry` does not exist as a
   storable/enforceable `Operation` today (rejected by both the CHECK
   constraint and the HTTP allowlist). A free-entry tournament therefore
   skips this step entirely — mirroring `docs/architecture/19-mission-
   architecture.md` §8's identical, correctly-stated handling of
   `mission_opt_in`'s non-existence. When an entry DOES carry monetary
   cost, its own `RiskRequest` (tenant, brand, player, jurisdiction,
   licensing mode, amount) is evaluated exactly as described, and any
   non-nil error is treated as a `DENY` (fail-closed, ADR 0031 §6) —
   `tournament_entry` still requires ADR 0031 §16's extension process
   before any code accepts it, documented-only until then.
3. **Tournament-specific conditions** — segment membership, level/VIP tier
   requirement, jurisdiction/market permission, minimum account age,
   KYC-verification state where the prize structure requires it, brand
   membership, asset support.

Each condition returns a **distinguishable reason code**, following this
codebase's "specific, distinguishable sentinel" convention — a player
refused entry must be tellable *why*, and support must be able to
reproduce the decision. The entry record persists the decision references
so a later dispute does not require re-deriving state that has since
changed.

At settlement, a participant who has become RG-ineligible is **withheld**,
not deleted (doc 17 §6.6): the snapshot records them and their score,
records the withholding and its reason, and no reward decision is emitted
for them. Whether their would-be prize redistributes to the next ranked
participant or is simply not awarded is a **prize-structure configuration
property**, decided per definition, not an implicit behaviour. See §14.

**Specialist-review addition (P1 fix, F8) — the snapshot has TWO
projections, and this is binding, not an implementation detail.** The
full internal snapshot (participant, score, rank, tie-break resolution,
withheld/disqualified status AND ITS REASON — including an RG decision
`Code` such as a self-exclusion code) is staff/audit-only, gated by an
RG-aware permission (at minimum the same bar as `rg_restriction:read`).
**A player-facing view of the SAME settled period (live standings or
historical results) carries rank, display identity, and score ONLY — a
withheld or disqualified participant is either omitted from the
player-facing view entirely, or shown with no reason of any kind, and
NEVER with an RG-derived code.** An earlier draft of this document left
this undecided, and doc 25's player-facing leaderboard-history endpoint
sketch inherited the gap by not excluding withholding reasons — without
this rule, a player viewing a concluded tournament's standings could
learn, with certainty, that a specific named individual self-excluded
from gambling: under every RG regime this is among the most
confidentiality-sensitive facts the platform holds about a player, and it
is far cheaper to specify the two-projection rule now than to retrofit it
once a settled-snapshot shape is a public API contract. Mirrors doc 17
§9.3's identical minimal-disclosure rule for the live score journal,
applied here to the settled/historical snapshot as well.

## 4. Entry models

| Model | Description | Notes |
|---|---|---|
| Automatic | Every eligible player is entered on first qualifying activity | No opt-in surface; lowest friction |
| Opt-in | Player explicitly joins before or during the window | Required where the product must show the player the rules first |
| Invitation | Staff- or segment-driven assignment | Audited as an administrative action |
| Paid entry | Entry costs points or money | **See below** |

**Paid entry is the one model with a real financial boundary.** An entry
cost in points is a points *spend instruction* (doc 17 §3.3) routed through
the points accounting layer; an entry cost in money is a wallet debit that
only the ledger may perform. In both cases the Tournament domain **requests**
the debit and receives a result — it never performs one. A failed debit
fails the entry atomically: there must be no state in which a player is
entered but uncharged, or charged but not entered. That atomicity is the
responsibility of the transaction boundary at the entry call site, and is a
required test case before paid entry is ever shipped.

**`RECOMMENDATION`: do not build paid entry in the first tournament
implementation.** It introduces a money/points movement, refund semantics on
cancellation (§10), and its own AML/regulatory surface, none of which the
core tournament mechanism needs in order to be useful. Ship free entry
first; add paid entry when a product requirement exists, with
`ledger-finance` review.

## 5. Scoring mechanism — pluggable by construction

**Decision: scoring is a named, versioned, parameterised mechanism selected
by configuration. It is never a hardcoded metric and never a per-tournament
code branch.** A new scoring mechanism is a new registered implementation
plus a configuration row; adding one must never require touching entry,
settlement, ranking, or prize distribution.

Candidate mechanisms, listed to show the interface is wide enough — **none
is designed or implemented here**, and the actual scoring logic of each is
explicitly out of scope for this document:

| Mechanism | Scores on |
|---|---|
| `points_earned` | Gamification points accrued in the window |
| `wager_amount` | Total qualifying stake |
| `win_amount` | Total qualifying winnings |
| `win_count` | Number of winning outcomes |
| `net_result` | Winnings minus stake |
| `loss_amount` | Total net loss — **see the warning below** |
| `activity_count` | Count of qualifying events |
| `max_multiplier` | Highest single win-to-stake ratio |
| `custom_metric` | A registered, named metric resolved from canonical event fields |

The mechanism contract, expressed conceptually:

- **Input**: one canonical event plus the instance's scoring parameters and
  the participant's current contribution history. Nothing else. A scoring
  mechanism never sees a provider payload (doc 17 §8) and never queries
  another domain's tables.
- **Output**: a signed score delta, or nothing. The delta is appended to
  the score journal as a `ScoreContribution` (doc 17 §6.1), which is the
  authoritative record — a mechanism never writes a running total.
- **Purity and determinism**: replaying the same event sequence through the
  same mechanism version must produce the same contributions. This is what
  makes recalculation (§9) and dispute resolution possible at all.
- **Void handling**: a mechanism must define its response to a reversal
  event. The default is a compensating negative delta equal to the original
  contribution. A mechanism that cannot express its own reversal must not
  be registered.
- **Versioning**: the mechanism version is pinned on the instance and
  recorded on every contribution, so a mechanism fix never silently
  rewrites historical scores.

Scoring must respect the canonical event's real-vs-demo and funding-source
indicators: demo activity never scores (doc 17 §6.6), and whether
bonus-funded stake scores is an `OPEN DECISION` (doc 17 §14.1) that each
mechanism's parameters must be able to express either way once it is
answered.

**Warning recorded deliberately, not as an implementation note:** a
`loss_amount` or otherwise loss-rewarding mechanism rewards players for
losing more money, which is a responsible-gaming concern in several
jurisdictions and a reputational one everywhere. It is listed above because
the mechanism model must not structurally exclude it, not because it is
endorsed. Enabling it requires explicit compliance sign-off per
jurisdiction, and it should be restrictable by jurisdiction configuration
like any other market rule (doc 17 §10).

## 6. Leaderboard integration

A tournament instance owns exactly one leaderboard (doc 17 §6). Everything
in that section applies unchanged and is not duplicated here:

- The append-only score journal in PostgreSQL is authoritative.
- Redis/ClickHouse/streaming may serve display and analytics; **never**
  settlement.
- Near-real-time display with an honest `computed_at` is the default.
- Settlement recomputes the ranking from the journal inside one
  transaction and persists an immutable snapshot.
- Late events are governed by `occurred_at` plus the configured settlement
  lag; after freeze they are recorded and rejected for scoring, never
  silently dropped.

The one tournament-specific addition: the instance's **`frozen` state is
the formal boundary** between "ranking may still change" and "ranking is
final." Freezing is an explicit, audited state transition, not an implicit
consequence of the clock passing `window_end` — because the settlement lag
means those are different moments, and a dispute needs to know exactly
which one applied. **Specialist-review addition**: the transaction that
actually performs the freeze/settlement-lag comparison MUST read Postgres
`clock_timestamp()`, never `now()` — see
`docs/architecture/17-gamification-engine-architecture.md` §5.2 for why
this is a binding requirement, not a stylistic preference. Concretely: a
freeze job queued behind the same tenant/instance's advisory lock as a
concurrent score-cancelling rollback must not freeze the ranking having
missed a correction that, by wall-clock time, had already committed —
the same failure shape as the real `internal/rg` bug Stage 4G-FINAL
found, applied here to prize money instead of self-exclusion.

## 7. Prize pool, distribution, and ties

```
PrizeStructure       belongs to a TournamentDefinition
  pool_model: fixed | guaranteed | pooled_from_entries | hybrid
  distribution[]  (rank or rank-range -> prize specification)
  minimum_qualifying_score, minimum_participants
  tie_policy, withheld_participant_policy
  prize_specification: a REWARD DECISION TEMPLATE, never a payment
                       instruction (kind + parameters, resolved by the
                       Reward Orchestrator)
```

Decisions:

- **A prize specification names a reward kind and its parameters** (bonus,
  free spins, free bet, points, cash-equivalent, tournament entry, physical
  or virtual item). It never names a ledger account, an asset movement, or
  a bonus grant directly. Fulfilment mechanics belong to the Reward
  Orchestrator and the domain it routes to.
- **The prize pool is snapshotted on the instance at freeze time.** A
  pooled prize funded from entries must be computed from actual, settled
  entries, and that computed value recorded, before any reward decision is
  produced — so a later entry refund cannot silently change what was
  already awarded.
- **A guaranteed pool that exceeds collected entries is a real operator
  liability.** This document does not design that liability's accounting;
  it flags that `ledger-finance` must own it before pooled or guaranteed
  prize models ship. The first implementation should use `fixed` pools
  only.
- **Minimum thresholds** (`minimum_participants`, `minimum_qualifying_score`)
  must be expressible so a tournament with three participants does not pay
  a hundred-place prize table. Failing a minimum triggers the configured
  outcome — reduced distribution, cancellation (§10), or no award — never
  an implicit one.
- **Tie-breaking**: the ranking-level tie-break chain (doc 17 §6.4) runs
  first and is deterministic. Only if the chain is exhausted and
  participants remain genuinely tied does the **`tie_policy`** apply:
  `split_evenly` (combined prizes for the tied ranks divided, with a
  defined rounding rule and a defined recipient of the remainder — never
  floating point, never an unassigned remainder), `all_receive_higher`
  (deliberate over-award), or `deterministic_order` (the chain's final
  discriminator decides outright). The policy is configuration; the
  *arithmetic* of a split, including rounding and remainder, must be
  specified exactly and reviewed by `ledger-finance` when the prize has
  monetary value.
- Rank-range distribution ("places 11–50 receive X") must be supported so a
  large prize table is not N rows of configuration.

## 8. Settlement

Settlement is a **state machine transition with an immutable output**, not
a background job that mutates rows:

```
running --(window_end + settlement_lag)--> frozen
frozen --(compute)--> settling
settling --(snapshot persisted + reward decisions emitted)--> settled
```

Properties, each chosen to make settlement disputable and reproducible:

1. **The ranking is computed from the score journal inside one transaction**
   at freeze. No derived store is read.
1a. **Specialist-review addition (P1 fix, F5): before any reward decision
   is emitted, every ranked participant in a prize-bearing position is
   evaluated against doc 17 §6.6's anti-manipulation controls; an
   unavailable or not-yet-built control is a FAIL-CLOSED condition that
   withholds that participant from settlement, never a skipped step.**
   An earlier draft of this section specified settlement fully as six
   numbered properties, none of which was a collusion/multi-accounting
   check — the anti-manipulation table in doc 17 §6.6 existed only as a
   neighbouring document's aspiration, never as a gate this state machine
   actually enforces, so nothing in the settlement sequence as originally
   written would ever have invoked it. Concretely: `frozen --(compute)-->
   settling` MUST include, for every participant in a rank that would
   receive a prize, (i) a check against the `PersonID`-linkage
   multi-accounting control (doc 17 §6.6, corrected), (ii) a check against
   whatever wash-betting/velocity Risk signal exists for the relevant
   operation (doc 17 §7.1 — and if that signal does not exist yet because
   its `count`/`velocity` `LimitKind` has not been built, that is itself a
   reason to withhold, not a reason to skip the check), and (iii) the
   existing RG/Risk/tournament-condition re-evaluation already named in
   §3. **Launch flag**: a tournament with a monetary prize pool must not
   go live before doc 17 §6.6's controls actually exist and are wired
   here — this launch precondition is recorded, not waived, by this
   stage's architecture freeze.
2. **A `TournamentResultSnapshot` is persisted and is immutable**: every
   participant, final score, final rank, tie-break resolution applied,
   withheld/disqualified participants and their reasons, the definition and
   mechanism versions in force, and the prize-pool value. This snapshot —
   not a live query — is the answer to "why did I finish fourth."
3. **Reward decisions are emitted from the snapshot**, one per prize
   assignment, each carrying an idempotency key derived from
   `(instance_id, snapshot_version, participant, prize_rank)`. A retried or
   redelivered settlement can never produce a second reward for the same
   assignment; that uniqueness is enforced by the receiving side's database
   constraint, never by a check-then-act in the tournament code.
4. **Settlement is idempotent and resumable.** A crash between snapshot
   persistence and reward emission must resume by re-emitting from the
   persisted snapshot, relying on the idempotency keys — never by
   recomputing the ranking (which might now differ) and never by assuming
   nothing was emitted.
5. **Settlement never blocks on fulfilment.** A reward decision that fails
   to fulfil is the Reward Orchestrator's retry/failure problem. The
   tournament is `settled` once decisions are emitted; a fulfilment failure
   does not reopen it.
6. **Every transition is audited** through the existing immutable
   `audit_log` with actor (including a system actor for automated
   transitions), reason, and before/after state.

## 9. Recalculation

Sometimes a settled result is genuinely wrong — a provider retroactively
voids a round, a scoring bug is found, activity is confirmed fraudulent.

**Decision: a settled snapshot is never edited. A correction produces a new
snapshot version, and the difference between versions produces compensating
reward decisions.** This is the same discipline the ledger uses for
corrections (compensating entries, never edits), applied to ranking.

```
settled --(authorized recalculation)--> recalculating --> settled (v+1)
```

Required properties:

- Recalculation requires an **explicit, audited authorization with a reason
  code**, at a permission level above ordinary tournament configuration,
  and — given it can reverse a player's prize — should require four-eyes
  approval above a configurable materiality threshold, mirroring CLAUDE.md's
  rule for manual balance adjustments. Final shape is `security`'s call at
  build time.
- The new snapshot is computed from the journal as it stands, including the
  corrections (negative contributions) that motivated the recalculation.
- The **delta between snapshot versions** drives reward decisions: newly
  owed prizes are emitted; prizes now not owed produce a **reversal
  decision** the Reward Orchestrator may or may not be able to honour. A
  reward that has been consumed (bonus wagered, free spins played) may be
  economically irreversible. The architecture must surface that as an
  explicit outcome, never silently succeed.
- **`RECOMMENDATION`: prefer withholding before settlement over reversal
  after it.** The settlement-lag window and the withheld-participant
  mechanism (§3) exist precisely so that suspected manipulation is resolved
  before a prize is fulfilled. Post-settlement reversal should be rare,
  authorized, and documented — not a routine operational tool.
- Every snapshot version remains queryable forever. A player shown "you
  finished third" and later "you finished fourth" must be answerable with
  both records and the recorded reason for the change.

## 10. Cancellation

A tournament may be cancelled before settlement (regulatory instruction,
provider outage corrupting scores, configuration error, failure to meet a
minimum). Cancellation is an audited state transition with a reason code
and defined consequences:

| Consequence | Rule |
|---|---|
| Prizes | No reward decisions are emitted. If any were already emitted, this is a recalculation (§9), not a cancellation |
| Paid entries | Entry costs are **refunded** — as a refund *request* to the points accounting layer or the ledger via the Reward Orchestrator's reversal path, never a direct credit from the tournament domain |
| Scores | The journal is retained unchanged. Cancellation does not delete history |
| Player state earned incidentally | XP, achievements, and points awarded by *other* gamification rules that happened to fire on the same activity are **unaffected** — they were not tournament rewards and are not clawed back |
| Notification | Participants must be notifiable; the notification mechanism is not designed here |

A cancelled instance is terminal. "Uncancelling" does not exist; re-running
means a new instance.

## 11. Multi-tenancy and scoping

Per doc 17 §9, without exception:

- Tournament **definitions** are dual-scope: `tenant_id NULL` is a
  platform-wide **template** a tenant may instantiate; `tenant_id` set is
  tenant-owned, optionally narrowed by `brand_id`. Tenant staff may read
  both, write only their own.
- Tournament **instances, entries, contributions, and snapshots** are
  always tenant-owned, `tenant_id NOT NULL`, RLS-enforced, with the
  `app.player_account_id IS NULL` guard on staff-scope policies and a
  SELECT-only `player_self_scope` policy for a player's own entry.
- **A tournament instance never spans tenants** (doc 17 §9.2). A
  platform-wide template instantiated by five tenants produces five
  independent competitions with five independent prize pools and five
  independent leaderboards.
- Brand-specific tournaments are configuration, never a code path.
- Jurisdiction and licensing-mode scoping use ADR 0006/ADR 0031 §9–§10's
  existing mechanism. No tournament-specific jurisdiction concept exists.

## 12. Audit

Every one of these writes to the existing immutable `audit_log`, never a
bespoke store: definition created/versioned/disabled, instance
scheduled/opened/frozen/settled/cancelled, entry
created/withdrawn/disqualified/withheld, eligibility denial (with the RG
and Risk decision codes), prize structure changed, settlement executed
(with snapshot version), recalculation authorized and executed (with reason
code and approver), and every administrative override.

The audit trail must be sufficient to reconstruct, months later and without
access to any cache: who was eligible, what they scored and from which
events, how ties were broken, what was awarded, and what changed
afterwards.

## 13. What is explicitly not designed here

- Actual scoring logic for any mechanism (§5).
- Team/multi-player-unit tournaments. The model says `participant` rather
  than `player` in the score journal so this is not foreclosed, but nothing
  about teams is designed.
- Bracket/knockout tournaments. Only score-and-rank competitions are
  modelled. A bracket is a materially different structure and would need
  its own design.
- Cross-provider or cross-game-type tournaments beyond what the canonical
  event taxonomy naturally supports.
- Prize fulfilment mechanics of any kind (Reward Orchestrator, Bonus
  Engine).
- The notification/communication surface.
- The player-facing API and UI.

## 14. Open decisions

1. Does a withheld or disqualified participant's prize **redistribute** to
   the next ranked participant, or is it simply not awarded? Product
   decision; must be explicit per prize structure.
2. Exact **rounding and remainder rules** for split prizes (§7) —
   `ledger-finance` must specify before any monetary prize ships.
3. **Pooled and guaranteed prize pool accounting** (§7) — an operator
   liability `ledger-finance` must own.
4. Whether **paid entry** is needed at all in the first implementation
   (§4 recommends deferring it).
5. Whether **loss-based scoring** is permitted, and in which jurisdictions
   (§5) — compliance/legal.
6. Whether post-settlement **reward reversal** is operationally acceptable
   at all, given some rewards are economically irreversible (§9).
7. Whether **bonus-funded wagering** scores (inherited from doc 17 §14.1).

## Cross-references

- Gamification Engine: `docs/architecture/17-gamification-engine-architecture.md`
- Missions: `docs/architecture/19-mission-architecture.md`
- Reward marketplace: `docs/architecture/20-reward-marketplace-architecture.md`
- Risk & Limits: `docs/decisions/0031-risk-and-limits-engine.md`
- Domain boundaries: `docs/architecture/02-domain-and-service-boundaries.md`
- Correction-by-compensation precedent: `docs/architecture/06-wallet-ledger-architecture.md`

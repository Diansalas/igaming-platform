# 22 — Canonical Activity/Event Taxonomy

Status: Stage 4H-A architecture-freeze proposal. Owned by the Master
Orchestrator (cross-domain, new-domain work — no existing specialist owns
event taxonomy end-to-end; `architect` and every producing domain review
it). **Design only — no event is implemented, no producer is wired, no
consumer is wired this stage.** Builds on, and does not replace,
`internal/eventbus`'s existing `Event` envelope and `Publisher`/
`Subscriber` interfaces (Stage 1 stub, still unwired to any real domain).

## Purpose

Bonus, Gamification, and future engagement/CRM triggers must consume
platform activity without coupling to Casino/Sportsbook/Payments/
Identity/KYC/RG's internal representations. This document defines the
canonical event *taxonomy* (what events exist, what they mean, who owns
them) and the *envelope contract* (what every event carries, regardless
of type) that makes that possible. It does not implement a single
producer or consumer.

## Hard rule

**Bonus and Gamification never accept a provider-specific or
domain-internal payload directly.** They only ever consume canonical
events through this taxonomy's envelope. A canonical event's `Payload`
is itself a versioned, documented shape per `Type` — not the raw
provider callback, not a raw database row. This mirrors the existing
platform rule that provider specifics never leak into core domain logic
(CLAUDE.md's "Provider abstraction" section) — applied here to internal
domain-to-domain coupling as well as external providers.

## Event identity and envelope (extends `eventbus.Event`)

Every canonical event carries, at minimum:

| Field | Meaning | Notes |
|---|---|---|
| `event_id` | Globally unique event identity (UUID) | Already exists on `eventbus.Event`. Required for consumer-side idempotency under at-least-once redelivery (the same reasoning CLAUDE.md already applies to the ledger — a durable broker will redeliver). |
| `type` | The canonical event type string (e.g. `casino.bet.settled`) | Dot-namespaced by owning domain, versioned (see Versioning below). Already exists on `eventbus.Event` as `Type`. |
| `source` | The owning domain/package that produced this event (e.g. `casino`, `payments`, `identity`) | NEW field, not yet on `eventbus.Event`. Required so a consumer can distinguish "the event says X happened" from "who is authoritative for X" — critical when a domain like Bonus later needs to know whether to trust a field's freshness. |
| `tenant_id` | Server-derived tenant, never client-supplied | Already exists on `eventbus.Event` as `TenantID`. Every canonical event is tenant-scoped; there is no platform-wide canonical event in this taxonomy (a platform-wide *rule* can still react to tenant-scoped events — that's a Bonus/Gamification-side aggregation concern, not an event-shape concern). |
| `brand_id` | The brand context, when applicable | NEW. Nullable — some events (e.g. `kyc.verification.updated`) are person-scoped before any brand relationship exists. |
| `person_id` / `player_account_id` | The player/person this event concerns | NEW. At least one MUST be present for any player-scoped event; both may be present once cross-brand resolution (ADR 0027) has occurred. Never duplicate identity evidence in the event payload — a reference only. |
| `occurred_at` | The instant the underlying business fact became true (not when the event was published) | Already exists on `eventbus.Event` as `OccurredAt`. Must be set from the domain's authoritative timestamp (e.g. the ledger transaction's `created_at`), never `time.Now()` at publish time, so replay and ordering reason about business time, not publish time. |
| `recorded_at` | **Specialist-review addition**: the instant the platform actually recorded/received the fact (arrival time), distinct from `occurred_at` (business time) | NEW. Required by `docs/architecture/17-gamification-engine-architecture.md` §6.3's late-arriving-event handling (leaderboard settlement lag, mission/tournament deadline enforcement) — every window/deadline comparison is evaluated against `occurred_at`, never `recorded_at`, but a consumer needs `recorded_at` to detect and log lateness at all. |
| `is_real_money` | Boolean: whether the underlying activity was real-money or demo/play-money | **Specialist-review addition (P1 fix)**: an earlier draft of this envelope omitted this field entirely, while `docs/architecture/17-gamification-engine-architecture.md` §6.6/§8 and `docs/architecture/18-tournament-architecture.md` §5 both declare "demo activity never scores" a MANDATORY, non-optional anti-manipulation control and state explicitly that the corresponding capability is `BLOCKED`, not workable-around, if this taxonomy lacks the field. Every event with a real/demo distinction at its source (casino, sportsbook) MUST set this; Gamification/Bonus consumers MUST treat a demo event as contributing zero score/progress/eligibility, structurally (at the canonical-event-consumption layer, before any individual mission/tournament/streak rule ever evaluates the event), never left to each rule author's own discipline to remember. |
| `funding_source` | Present on events with a monetary dimension: `player_cash` \| `player_bonus` \| `mixed` \| n/a | **Specialist-review addition (P1 fix)**, same rationale as `is_real_money` — required by doc 17 §14.1/doc 18 §14.7/doc 19 §11.6's open "does bonus-funded wagering contribute to score" decision, which cannot be answered (in either direction) without this field existing; and by doc 10 §5's bonus-progress-contribution rules. Until that open decision is resolved, the documented fail-safe DEFAULT is that bonus-funded stake does NOT contribute to score (the reversible direction — see `docs/architecture/17-gamification-engine-architecture.md` §14 item 1). |
| `correlation_id` | A caller/request-scoped identifier threading a single logical operation across the events it produces | **Specialist-review addition (P1 fix)**, required by `docs/decisions/0033-provider-interoperability-and-external-bonus-engines.md`'s own stated assumption that this field would exist, and by every domain's audit/dispute-resolution needs (tying a Bonus Grant's Progress trail, a Gamification mission-completion event, and a Reward Orchestrator fulfillment record back to the one player action that caused all three). |
| `reverses_ref` | Present only on an event that reverses/voids/cancels a previously-published event: the `event_id` (or `idempotency_key`) of the event being reversed | **Specialist-review addition (P1 fix)**: makes reversal/void linkage explicit rather than implicit-via-a-differently-named-event-type (e.g. `casino.bet.rolled_back`), so a generic consumer (a leaderboard's compensating-negative-contribution logic, doc 17 §6.1) can detect "this reverses something" without a type-specific mapping table for every domain's own reversal-event naming convention. |
| `operation_ref` / `provider_ref` | The domain-specific identifier(s) this event is about (e.g. a `ledger_transactions.id`, a `casino_launch_sessions.id`, a `(provider_id, provider_tx_id)` pair) | NEW. Lets a consumer correlate an event back to its source-of-truth row without the event needing to carry that row's full content. |
| `asset_code` / `amount_minor_units` | Present only on events with a monetary dimension | NEW, optional. Same integer-minor-units-plus-exponent discipline as the ledger (CLAUDE.md's financial rules) — a canonical event never carries a float. An event with a monetary dimension is a *fact about* a ledger movement that already happened (or was denied) through the normal domain flow — it never itself triggers a ledger posting. |
| `idempotency_key` | A stable, deterministic key derived from the event's own identity (e.g. `type + operation_ref`), generated ONCE and reused verbatim on every redelivery of the same business fact | NEW. **Distinct from `event_id`, and this distinction is binding, not a stylistic note** — `event_id` is unique per *publish* and MAY legitimately differ across redeliveries of the same fact under some broker semantics; `idempotency_key` is unique per *business fact* and MUST NOT change across redeliveries. **Any consumer that dedupes on `event_id` instead of `idempotency_key` has built a double-count/double-award bug** — this exact confusion was found and corrected in this stage's own sibling documents (`docs/architecture/17-gamification-engine-architecture.md` §8, `docs/architecture/19-mission-architecture.md` §5) during specialist review; every consumer MUST dedupe on `idempotency_key` only. |
| `schema_version` | Integer, starts at 1 per `type` | NEW. See Versioning below. |
| `payload` | The event-type-specific fields, as a versioned struct/JSON shape documented per type | Already exists on `eventbus.Event` as `Payload []byte`; this document requires that raw bytes be a *documented, versioned* shape per `type`, not an ad hoc dump of an internal struct. |

**Ordering**: no cross-type global ordering is guaranteed or required.
Per-`operation_ref` ordering (e.g. a bet's `settled` event before its
`rollback` event) IS required, and is the producing domain's
responsibility to guarantee (in practice: publish from inside the same
database transaction that made the fact true, after commit, in the order
facts became true — this stage does not select a broker or design the
publish-after-commit mechanism; that is implementation work for whichever
future stage actually wires a producer).

**Replay semantics**: every consumer (Bonus, Gamification, future
CRM triggers) MUST be idempotent per `idempotency_key` — replaying the
full event history against a fresh consumer must produce the same
end state as the original delivery. This is what makes "recompute a
projection from event history" safe, mirroring the ledger's own
"balances are projections recomputed from entries" principle applied to
non-financial state (mission progress, points balances, leaderboard
standings).

**Schema evolution**: a breaking change to a `type`'s payload shape
increments `schema_version` and is published as a NEW type string suffix
or alongside the old version for a deprecation window (exact mechanism
deferred — this stage establishes that `schema_version` exists and must
be checked, not the migration tooling around it). Adding an optional
field is non-breaking and does not require a version bump.

## Event ownership rule

Each event type has exactly one producing domain (`source`). Every other
domain, Bonus and Gamification included, is a consumer only — never a
producer of another domain's event type, and never a producer of a
*second* event describing the same fact (e.g. Gamification does not
publish its own `casino.bet.settled`-equivalent; it consumes the one
Casino publishes).

## Candidate canonical event taxonomy

This is the candidate list from the Stage 4H-A directive, with ownership
assigned. **Not all of these are implemented this stage** — this table
is the taxonomy and ownership contract; wiring an actual producer/
consumer is future-stage implementation work, tracked per row.

| Event type | Owner (`source`) | Fires when | Status |
|---|---|---|---|
| `identity.person.registered` | identity | A `Person`/`PlayerAccount` is created | NOT IMPLEMENTED |
| `identity.email.verified` | identity | Email verification completes (ADR 0030) | NOT IMPLEMENTED |
| `kyc.verification.updated` | kyc | A KYC verification status changes (never carries raw evidence — status reference only, per this taxonomy's identity-evidence rule) | NOT IMPLEMENTED |
| `payments.deposit.settled` | payments | A deposit intent settles (ledger-posted) | NOT IMPLEMENTED |
| `payments.withdrawal.settled` | payments | A withdrawal completes | NOT IMPLEMENTED |
| `casino.launch.started` | casino | `LaunchGame` succeeds — **specialist-review correction**: `LaunchGame` runs the identical code path for `ModeReal` and `ModeDemo`; there is no existing branch that would let a producer emit this only for real mode without adding a new check. A future producer must decide (and this taxonomy does not yet decide) whether demo launches are worth a canonical event at all | NOT IMPLEMENTED |
| `casino.launch.denied` | casino | RG-denied (`orchestrator.go`'s `evaluateAndAuditEligibility` on launch, audit action `casino.launch_denied`) or Risk-denied (real mode only, audit action `casino.launch_denied_by_risk_policy`) launch attempt — **added per specialist review**: these are exactly as real, distinct, and already-audited as the bet/win/rollback events above and were an unexplained omission in the original draft | NOT IMPLEMENTED |
| `casino.bet.settled` | casino | `postBet` posts (Succeeded or Declined — both are facts worth knowing; Declined carries a `denial_code` field, no amount). **Precision note per specialist review**: in the current `internal/casino` code, `Declined` can only ever originate from a PLATFORM-side decision (RG, Risk, or insufficient funds) — a provider that self-reports its own bet decline is rejected upstream as malformed input (`ErrOutcomeNotSucceeded`) and never reaches this event at all. `denial_code` is not one formal enum; it holds an RG `Decision.Code`, a Risk `RiskDecision.Code`, or the literal string `"insufficient_funds"`, depending on which check declined | NOT IMPLEMENTED |
| `casino.win.settled` | casino | `postWin` posts | NOT IMPLEMENTED |
| `casino.bet.rolled_back` | casino | `postRollback` reverses a previously-posted bet or win. **Explicit scope note per specialist review** (mirroring how `casino.session.started` is deliberately excluded below): this event does NOT fire for a tombstoned rollback (a rollback naming a `provider_tx_id` that was never posted as a bet/win — `postRollbackTombstone`, a real, separately-audited, zero-entry code path). Bonus/Gamification never received a `casino.bet.settled` for an original that was never posted, so there is nothing for them to reverse — the omission is safe, but was previously unstated | NOT IMPLEMENTED |
| `sportsbook.bet.placed` | sportsbook | A sportsbook bet is placed, stake moves to `player_locked` (sportsbook does not exist as a package yet — event shape from the `sportsbook` specialist's Provider Interoperability ADR, `docs/decisions/0033` §2) | NOT IMPLEMENTED |
| `sportsbook.bet.settled` | sportsbook | A sportsbook bet settles — **corrected per specialist review**: this may fire MORE THAN ONCE per bet (partial legs, market corrections), carrying an `is_correction` flag; a cashout is a `sportsbook.bet.settled` with a `settlement_type` discriminator, not a separate event type. The original draft of this taxonomy collapsed placement and settlement into one event and omitted multi-fire/correction semantics, contradicting ADR 0033's own explicit design — fixed here to match the ADR exactly | NOT IMPLEMENTED |
| `sportsbook.bet.void_cancelled` | sportsbook | A sportsbook bet is voided/cancelled | NOT IMPLEMENTED |
| `bonus.grant.created` | bonus-engine | A bonus Grant is created | NOT IMPLEMENTED |
| `bonus.grant.activated` | bonus-engine | A bonus Grant transitions `issued`→`activated` (value enters the wallet) — **added, Stage 4H-B1 Wave 1 reconciliation**, per `architect`'s D-1/D-2 findings below | NOT IMPLEMENTED |
| `bonus.grant.progress_changed` | bonus-engine | A bonus Grant's wagering progress changes — **added, Stage 4H-B1 Wave 1 reconciliation**. Informational only, never authoritative: the authoritative P_net/P_firm values are always the derived ledger reads specified in `ledger-accounting-model.md` §6.6/§7.10, this event exists for downstream analytics/CRM only | NOT IMPLEMENTED |
| `bonus.grant.completed` | bonus-engine | A bonus Grant's wagering/conditions complete | NOT IMPLEMENTED |
| `bonus.grant.converted` | bonus-engine | A bonus Grant's `BonusConversion` posts — **added, Stage 4H-B1 Wave 1 reconciliation** | NOT IMPLEMENTED |
| `bonus.grant.expired` | bonus-engine | A bonus Grant expires unconverted | NOT IMPLEMENTED |
| `bonus.grant.cancelled` | bonus-engine | A bonus Grant is cancelled by an operator/system action, posting `bonus_forfeiture` (ADR 0032 §3.1) — **split from the original `bonus.grant.reversed`, Stage 4H-B1 Wave 1 reconciliation, `architect` finding D-1**: `cancelled` and `reversed` are different financial facts (different postings, different `reverses_ref` semantics) and the original single event made `reverses_ref` ambiguous | NOT IMPLEMENTED |
| `bonus.grant.forfeited` | bonus-engine | A bonus Grant is forfeited (a distinct terminal state per `docs/architecture/10-bonus-engine-architecture.md` §1.2, the one a disputing player's case most often turns on) — **added, Stage 4H-B1 Wave 1 reconciliation, `architect` finding D-2**: this state previously had no event at all | NOT IMPLEMENTED |
| `bonus.grant.reversed` | bonus-engine | A bonus Grant's posting is reversed (posts `bonus_reversal`, ADR 0032 §3.1) — narrowed to this one meaning, see `bonus.grant.cancelled` above | NOT IMPLEMENTED |
| `bonus.reward.requested` | bonus-engine | Reserved for a future `RewardFulfiller` seam (`docs/architecture/29-bonus-implementation-contract.md` §6) — not produced by Stage 4H-B1 | NOT IMPLEMENTED, reserved only |
| `gamification.mission.completed` | gamification (owned by `architect`'s design, `docs/architecture/19-mission-architecture.md`) | A mission's completion condition is met | NOT IMPLEMENTED |
| `gamification.tournament.entered` | gamification | A player enters a tournament | NOT IMPLEMENTED |
| `gamification.level.changed` | gamification | A player's level changes (up or down, if downgrades are supported — see `docs/architecture/17-gamification-engine-architecture.md`) | NOT IMPLEMENTED |
| `rg.status.changed` | rg | A self-exclusion/restriction becomes effective or lifts | NOT IMPLEMENTED — **consumed, never produced, by Bonus/Gamification**; RG remains sole authority (mirrors `internal/rg`'s existing exclusivity) |
| `risk.decision.denied` | risk | A `risk.Evaluate` call returns non-ALLOW, for any operation | NOT IMPLEMENTED — informational only; Bonus/Gamification still call `risk.Evaluate` synchronously themselves and must never rely solely on this event for enforcement (fail-closed enforcement is always the synchronous call, this event is for downstream analytics/CRM triggers only) |

Deliberately **not** created as canonical event types this stage (per
the directive's "do not blindly implement every event" instruction and
CLAUDE.md's no-scope-expansion rule): `casino.session.started` distinct
from `casino.launch.started` (redundant at this stage — one event
covers it), any granular per-field "profile updated" event, any
UI-interaction/clickstream event (out of scope for Bonus/Gamification
eligibility, belongs to `data-analytics` if ever needed).

## Consumer contract for Bonus/Gamification

Both domains:

1. Subscribe only to canonical event `type`s from this table, never to a
   domain-internal channel.
2. Treat every event as at-least-once — dedupe on `idempotency_key`
   before acting.
3. Never treat event delivery as a substitute for a synchronous
   `risk.Evaluate`/`rg.EvaluateEligibility` call on their own
   money/eligibility-affecting actions (events drive *progression and
   eligibility-checking triggers*, e.g. "re-evaluate this mission's
   progress" — they never themselves authorize a payout; the payout
   still goes through Risk/RG/Ledger synchronously at the moment of
   fulfillment, per `docs/architecture/21-reward-orchestration-architecture.md`).
4. Never mutate the producing domain's own tables — a mission-progress
   update lives in Gamification's own schema, never in `internal/casino`
   or `internal/payments` tables.

## Open decisions (for human/architect confirmation before implementation)

1. Broker selection (Kafka/NATS/other) and durability/ordering guarantees
   — explicitly deferred per `internal/eventbus`'s own Stage 1 stub
   status; not decided this stage.
2. Whether `risk.decision.denied` is worth publishing as an event at all
   before a concrete CRM/analytics consumer exists for it (candidate for
   removal if no stage claims it within a reasonable horizon —
   flagged, not resolved, here).
3. Exact versioning/deprecation mechanism for `schema_version` bumps.

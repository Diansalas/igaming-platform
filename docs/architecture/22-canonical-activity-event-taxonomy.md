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
| `brand_id` | The brand context, when applicable | NEW. Nullable — some events (e.g. `identity.person.kyc_verified`) are person-scoped before any brand relationship exists. |
| `person_id` / `player_account_id` | The player/person this event concerns | NEW. At least one MUST be present for any player-scoped event; both may be present once cross-brand resolution (ADR 0027) has occurred. Never duplicate identity evidence in the event payload — a reference only. |
| `occurred_at` | The instant the underlying business fact became true (not when the event was published) | Already exists on `eventbus.Event` as `OccurredAt`. Must be set from the domain's authoritative timestamp (e.g. the ledger transaction's `created_at`), never `time.Now()` at publish time, so replay and ordering reason about business time, not publish time. |
| `operation_ref` / `provider_ref` | The domain-specific identifier(s) this event is about (e.g. a `ledger_transactions.id`, a `casino_launch_sessions.id`, a `(provider_id, provider_tx_id)` pair) | NEW. Lets a consumer correlate an event back to its source-of-truth row without the event needing to carry that row's full content. |
| `asset_code` / `amount_minor_units` | Present only on events with a monetary dimension | NEW, optional. Same integer-minor-units-plus-exponent discipline as the ledger (CLAUDE.md's financial rules) — a canonical event never carries a float. An event with a monetary dimension is a *fact about* a ledger movement that already happened (or was denied) through the normal domain flow — it never itself triggers a ledger posting. |
| `idempotency_key` | A stable, deterministic key derived from the event's own identity (e.g. `type + operation_ref`) | NEW. Distinct from `event_id` (which is unique per *publish*, and would differ on redelivery-as-a-new-attempt in some broker semantics) — the idempotency key is unique per *business fact*, letting a consumer safely process the same fact twice. |
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
| `casino.launch.started` | casino | `LaunchGame` succeeds (real mode) | NOT IMPLEMENTED |
| `casino.bet.settled` | casino | `postBet` posts (Succeeded or Declined — both are facts worth knowing; Declined carries a `denial_code` field, no amount) | NOT IMPLEMENTED |
| `casino.win.settled` | casino | `postWin` posts | NOT IMPLEMENTED |
| `casino.bet.rolled_back` | casino | `postRollback` reverses a bet or win | NOT IMPLEMENTED |
| `sportsbook.bet.settled` | sportsbook | A sportsbook bet is placed/settled (sportsbook does not exist as a package yet — event shape proposed by the `sportsbook` specialist's Provider Interoperability ADR, `docs/decisions/0033`, this same stage) | NOT IMPLEMENTED |
| `sportsbook.bet.void_cancelled` | sportsbook | A sportsbook bet is voided/cancelled | NOT IMPLEMENTED |
| `bonus.grant.created` | bonus-engine | A bonus Grant is created | NOT IMPLEMENTED |
| `bonus.grant.completed` | bonus-engine | A bonus Grant's wagering/conditions complete | NOT IMPLEMENTED |
| `bonus.grant.expired` | bonus-engine | A bonus Grant expires unconverted | NOT IMPLEMENTED |
| `bonus.grant.reversed` | bonus-engine | A bonus Grant is cancelled/reversed | NOT IMPLEMENTED |
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

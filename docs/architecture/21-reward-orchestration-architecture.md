# 21 — Reward Orchestration Architecture

Status: Stage 4H-A architecture-freeze proposal. Owned by the Master
Orchestrator (new cross-cutting domain, per this project's established
pattern of direct Orchestrator implementation for cross-domain/new-domain
work — architect and every domain specialist review it). **Design only —
no code, no migrations.**

## Why a third domain, not two

The Stage 4H-A directive requires three distinct domains, not two:

- **Bonus Engine** decides economic/promotional reward *policy and
  lifecycle* (should this player get this bonus, under what wagering
  terms).
- **Gamification Engine** decides engagement/progression *policy*
  (has this mission been completed, what tier has this player reached,
  who won this tournament).
- **Reward Orchestrator** is neither — it is the single place that turns
  a reward *decision* (made by either of the above, or by a future
  domain not yet imagined) into an actual *fulfillment*, whether that
  fulfillment is internal (ledger posting, points credit, badge
  unlock) or external (a provider's own bonus engine, via
  `docs/architecture/23-external-reward-provider-contract.md`).

Without this third domain, Bonus Engine and Gamification Engine would
each need their own fulfillment logic, their own External Reward
Provider integration, and their own idempotency/audit discipline for
"how does a decision become a real thing the player receives" — exactly
the kind of duplicated authority CLAUDE.md's domain-boundary discipline
exists to prevent (mirrors why Risk and RG stay separate but share a
decision/enforcement layer, ADR 0031 §1 — Bonus and Gamification stay
separate but share a fulfillment layer).

## Responsibility

The Reward Orchestrator's SOLE job: given a `RewardDecision` (from Bonus,
Gamification, or any future producer), determine and execute the correct
fulfillment mechanism, exactly once, auditably, and report the outcome
back to the decision's owner. It never decides WHETHER a reward is
earned — that is always the calling domain's decision, made under its
own Risk/RG/eligibility rules before it ever reaches the Orchestrator.

## `RewardDecision` — the canonical input

A conceptual shape every producer supplies:

| Field | Meaning |
|---|---|
| `decision_id` | Producer-generated idempotency key (e.g. a Bonus Grant id, a Tournament settlement's prize-line id) |
| `tenant_id` / `brand_id` / `player_account_id` | Server-derived scope, never client-supplied |
| `source_domain` | `bonus`, `gamification`, or a future producer — for audit, never for branching fulfillment logic differently by producer where the reward *type* already determines the mechanism |
| `reward_type` | One of a canonical vocabulary: `cash_credit`, `bonus_credit` (i.e. `player_bonus` wallet), `free_spins`, `free_bet`, `points_credit`, `badge_unlock`, `tournament_entry_credit`, `external_provider_reward`, etc. — extensible, but each new type requires an explicit fulfillment-mechanism mapping below, never a default/fallback guess |
| `amount_minor_units` / `asset_code` | Present only for monetary `reward_type`s, same integer-minor-units discipline as the ledger |
| `points_amount` / `point_type` | Present only for points-related `reward_type`s (see `docs/architecture/24-points-accounting-architecture.md` for the points ledger this credits) |
| `fulfillment_hint` | Optional producer hint: "prefer external provider X if available" — the Orchestrator may override this if the hint's provider fails capability discovery, per campaign-level fallback policy owned by the producer domain, not invented here |
| `jurisdiction_code` / `licensing_mode` | Threaded through from the producer exactly like every other Risk-consuming operation (ADR 0031 §9/§10) — the Orchestrator does not resolve these itself |

## Fulfillment mechanism mapping (conceptual, not exhaustive)

| `reward_type` | Fulfillment mechanism | Owned by |
|---|---|---|
| `cash_credit` / `bonus_credit` | `internal/ledger.Post` (a ledger posting exactly like any other domain's) | ledger-finance's posting engine; Orchestrator supplies the split instruction it received from Bonus, never invents accounting treatment itself (`docs/decisions/0032-bonus-accounting.md`) |
| `free_spins` / `free_bet` | Casino/Sportsbook's own existing free-round mechanism (`docs/architecture/10-bonus-engine-architecture.md`'s "every provider exposes free spins differently... `casino` implements the per-provider adaptation") | casino / sportsbook |
| `points_credit` | Points ledger (`docs/architecture/24-points-accounting-architecture.md`) | ledger-finance-designed points ledger, NOT `internal/ledger` |
| `badge_unlock` / non-monetary gamification state | Gamification's own state store (no ledger involvement at all — see `docs/architecture/17-gamification-engine-architecture.md`'s explicit "which rewards touch the ledger and which don't" line) | gamification |
| `tournament_entry_credit` | Either a `points_credit` or a `cash_credit`-shaped entry-fee waiver, depending on campaign configuration — resolved to one of the above two rows, never a distinct mechanism | whichever mechanism it resolves to |
| `external_provider_reward` | `ExternalRewardProvider.RequestReward` (`docs/architecture/23-external-reward-provider-contract.md`) | Reward Orchestrator itself, via the External Reward Provider contract |

**The Orchestrator never invents a NEW fulfillment mechanism outside this
table.** A future reward type with no mapping is NOT IMPLEMENTED until
this table is extended — mirrors ADR 0031 §12's extension-model
discipline (documented steps, not silent ad hoc handling).

## Risk and RG — re-checked, not re-decided

The Reward Orchestrator does **not** re-run `risk.Evaluate` or
`rg.EvaluateEligibility` itself for a decision it receives — the
producing domain (Bonus, Gamification) already did that before emitting
the `RewardDecision`. The Orchestrator's own responsibility is narrower:
if fulfillment is delayed (e.g. an external provider request is
`Pending` for an extended period, or a scheduled cash conversion runs
minutes/hours after a wagering requirement completes), the Orchestrator
MUST re-check RG status (not Risk — RG's self-exclusion is the one
condition that can become newly true between decision and fulfillment
and must never be missed) immediately before actually crediting a wallet
or points balance. This mirrors `internal/casino`'s own Stage 4G-FINAL
lesson (the `clock_timestamp()` fix): a decision made at time T and
fulfilled at time T+δ must re-observe RG state at fulfillment time if δ
is non-trivial, never trust a stale eligibility check silently.

## Idempotency and concurrency

Every fulfillment is idempotent on `decision_id` — a re-delivered or
retried `RewardDecision` for the same `decision_id` produces the
identical fulfillment outcome, never a second credit. For mechanisms
that ultimately reach `internal/ledger.Post`, this is inherited for free
(the ledger's own idempotency key). For mechanisms that don't touch the
ledger (points, badges, external-provider requests), the Orchestrator's
OWN fulfillment-tracking table enforces the same discipline via a unique
constraint on `(tenant_id, decision_id)`, mirroring the ledger's
`(provider_id, provider_tx_id)` pattern. Concurrent duplicate
fulfillment attempts for the same `decision_id` are serialized the same
way Stage 4G-FINAL serialized concurrent duplicate bet deliveries — a
transaction-scoped advisory lock (or equivalent) keyed on
`(tenant_id, decision_id)`, acquired before the idempotency check, not
after.

## Audit

Every fulfillment attempt (success, failure, retry, `Unknown` outcome)
is audited via the existing `audit.Record` pattern
(actor=`system`/`reward-orchestrator`, tenant, entity=the
`decision_id`, before/after state, reason code) — this is the complete
lifecycle audit trail the External Reward Provider contract's
capability #9 requires, extended to internal fulfillment too, not just
external.

## Multi-tenancy / brand

The Orchestrator itself owns no tenant-facing configuration — it is pure
mechanism, driven entirely by the `RewardDecision` it's given and the
provider/ledger/points configuration those decisions' owning domains
already scoped correctly. It introduces no new tenant/brand-scoping
surface of its own.

## What this stage does NOT do

No `RewardDecision` struct is implemented in Go. No fulfillment table is
migrated. No External Reward Provider adapter exists. This document
establishes the contract three still-unimplemented domains (Bonus,
Gamification, and this one) will code against when a future stage
authorizes implementation.

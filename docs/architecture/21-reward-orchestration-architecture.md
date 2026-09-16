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
| `amount_minor_units` / `asset_code` | Present only for monetary `reward_type`s, same integer-minor-units discipline as the ledger — carried as a `NUMERIC(38,0)`-compatible decimal representation (decimal string over the wire), never an `int64` (an asset with exponent 18 per ADR 0021/CLAUDE.md overflows one); `points_amount` likewise per doc 24 §3. (Wave-2 ledger-finance review, P2.) |
| `points_amount` / `point_type` | Present only for points-related `reward_type`s (see `docs/architecture/24-points-accounting-architecture.md` for the points ledger this credits) |
| `fulfillment_hint` | Optional producer hint: "prefer external provider X if available" — the Orchestrator may override this if the hint's provider fails capability discovery, per campaign-level fallback policy owned by the producer domain, not invented here |
| `jurisdiction_code` / `licensing_mode` | Threaded through from the producer exactly like every other Risk-consuming operation (ADR 0031 §9/§10) — the Orchestrator does not resolve these itself |

## Fulfillment mechanism mapping (conceptual, not exhaustive)

| `reward_type` | Fulfillment mechanism | Owned by |
|---|---|---|
| `cash_credit` / `bonus_credit` | `internal/ledger.Post` (a ledger posting exactly like any other domain's) | ledger-finance's posting engine; Orchestrator supplies the split instruction it received from Bonus, never invents accounting treatment itself (`docs/decisions/0032-bonus-accounting.md`) |
| `free_spins` / `free_bet` | Casino/Sportsbook's own free-round mechanism — **to be built, not existing today**: `internal/casino`'s `CasinoProvider` interface has no free-round method and `LaunchRequest.Mode` has only `ModeReal`/`ModeDemo` (confirmed by direct inspection, specialist review); `08-casino-integration-architecture.md` §1 already records this as an `OPEN DECISION` deferred to the Bonus Engine implementation stage | casino / sportsbook |
| `points_credit` | Points ledger (`docs/architecture/24-points-accounting-architecture.md`) | ledger-finance-designed points ledger, NOT `internal/ledger` |
| `badge_unlock` / non-monetary gamification state | Gamification's own state store (no ledger involvement at all — **citation corrected per specialist review**: the governing rule is ADR 0032 §1's monetary test, "redemption value in a registered Asset, or we'd owe them in an Asset on request → ledger; otherwise no entry at all," not a doc 17 quotation that does not exist in that document) | gamification |
| `tournament_entry_credit` | A `points_credit` when the entry fee is denominated in a `PointType`, **or genuinely nothing when entry is free** (no ledger effect, Gamification state only) — **specialist-review correction (P2 fix)**: an earlier draft of this row also offered a "`cash_credit`-shaped entry-fee waiver" option, which conflicts with ADR 0032 §1's own worked example ("Free tournament entry with no cash-value alternative → No [ledger entry] → Gamification state only," calling a ledger posting for this case "a defect: it inflates `promo_liability` with an obligation that can never be discharged in an asset"). If a tournament's entry fee is genuinely asset-denominated, that is not a "waiver" at all — it is an ordinary `cash_credit`/`points_credit` per the rows above, resolved the same way any other priced item is | whichever mechanism it resolves to, or no ledger effect at all for a free entry |
| `external_provider_reward` | `ExternalRewardProvider.RequestReward` (`docs/architecture/23-external-reward-provider-contract.md`) — **zero ledger entries** while the declared fulfilment destination is `inside_provider` (ADR 0032 §6(c)); ordinary ledger postings apply if the declared destination is `into_platform_wallet` (doc 23's capability-discovery destination flag) | Reward Orchestrator itself, via the External Reward Provider contract |

**The Orchestrator never invents a NEW fulfillment mechanism outside this
table.** A future reward type with no mapping is NOT IMPLEMENTED until
this table is extended — mirrors ADR 0031 §12's extension-model
discipline (documented steps, not silent ad hoc handling).

## Reversal and compensation

**Specialist-review addition (P1 fix)**: an earlier draft of this document
defined no reversal path at all, while four sibling documents this same
stage explicitly delegate reversal TO the Reward Orchestrator (doc 17
§4.2: "the Reward Orchestrator's own reversal path is the only mechanism
that could undo one [a level-based reward]"; doc 18 §9: tournament
recalculation "produce[s] a reversal decision the Reward Orchestrator may
or may not be able to honour"; doc 19 §5: mission voids "decrement";
doc 20 §1/§8: a marketplace refund is "a reversal request through the
Reward Orchestrator"). Without a defined shape, an implementer has two bad
options: reuse the original `decision_id` (the unique constraint above
then swallows the reversal as a duplicate, silently no-opping it — a
permanent, undetected divergence between what the gamification/bonus
domain believes was clawed back and what actually was), or mint a fresh
`decision_id` (making the reversal structurally indistinguishable from a
brand-new, unvalidated award, with no linkage to what it reverses and no
gate on who may authorize it). This document now defines the shape:

- A **`RewardReversalDecision`** is a distinct input shape from
  `RewardDecision`, not a same-`decision_id` replay. It carries:
  `reversal_id` (its own idempotency key — distinct from, but linked to,
  the original `decision_id`), `original_decision_id` (mandatory,
  validated per mechanism as below), `reason_code` (mandatory), and
  `initiated_by` (the actor — system-driven for an automated
  void/rollback-triggered compensation, or staff-driven for a manual
  reversal).
- **Single-reversal guarantee, per mechanism.** **Wave-2 ledger-finance
  review correction (P1-5)**: an earlier draft placed this guarantee on
  "the reversal-tracking record" unconditionally, but the Orchestrator's
  own tracking table (see "Idempotency and concurrency," below) exists
  only for `badge_unlock`/non-monetary state and `external_provider_
  reward` — for `cash_credit`/`bonus_credit` (money ledger) and
  `points_credit` (points ledger) there is no such row, so a constraint
  on it would guarantee nothing for the mechanisms that actually move
  value. The guarantee is therefore the **owning ledger's own**, not a
  second registry: for `cash_credit`/`bonus_credit`/`points_credit`, the
  original transaction is selected `FOR UPDATE` and the reversal rejected
  if a transaction already reverses it (`docs/decisions/0032-bonus-
  accounting.md` §7; `docs/architecture/24-points-accounting-
  architecture.md` §4.2). `original_decision_id`'s existence and
  fulfilled-ness are established by looking that transaction up on the
  owning ledger's own idempotency key (`(tenant_id, idempotency_key =
  original decision_id)`), not by a tracking row the Orchestrator does
  not keep for these mechanisms. Only for `badge_unlock`/non-monetary
  state and `external_provider_reward` — the mechanisms that *do* have an
  Orchestrator tracking row — does the Orchestrator additionally carry
  `UNIQUE (tenant_id, original_decision_id)` on that row (mirroring doc
  24 §4.2's `FOR UPDATE`-guarded single-reversal pattern for points).
- **Reversal of a never-fulfilled decision writes a tombstone, it is not
  merely rejected.** **Wave-2 ledger-finance review correction (P1-5)**:
  an earlier draft said such a reversal "is rejected, not silently
  accepted" — rejection alone reopens exactly the race CLAUDE.md's
  rollback rule exists to prevent, because the late-arriving original
  `RewardDecision` can then fulfill *after* its own reversal was
  rejected, crediting a wallet or points balance for a reward that was
  already clawed back. A `RewardReversalDecision` whose
  `original_decision_id` the owning ledger has never posted instead
  writes a tombstone occupying that original's own idempotency slot — in
  the owning ledger for `cash_credit`/`bonus_credit`/`points_credit`
  (ADR 0032 §7 / doc 24 §4.2's `postRollbackTombstone`-equivalent
  mechanism), or in the Orchestrator's own tracking table for
  `badge_unlock`/`external_provider_reward` — so the late-arriving
  original is rejected rather than fulfilled after its own reversal.
- **Fulfillment-mechanism-specific reversal**: for `cash_credit`/
  `bonus_credit`/`points_credit`, the reversal is an ordinary compensating
  ledger/points entry via `reverses_transaction_id` (inherited, not
  reinvented — the identical pattern this codebase already uses for
  casino/payments reversals). For `badge_unlock`/non-monetary state, the
  reversal is a Gamification-owned state change (this document does not
  design what "un-unlocking" a badge means — that is Gamification's own
  call, per doc 17 §4.2's own note that a downgrade "never claws back a
  reward already fulfilled," which the Orchestrator must not silently
  override). For `external_provider_reward`, reversal is the External
  Reward Provider contract's `Revoke` capability (best-effort, per
  `docs/architecture/23-external-reward-provider-contract.md`) — an
  external revoke that the provider reports as `AlreadyFulfilled` is an
  **explicit failure outcome, never a silent success and never an
  auto-compensating credit invented on our own side** to paper over it.
- **Economically irreversible fulfillments** (a bonus already fully
  wagered, free spins already played, a voucher already redeemed) are
  correctly identified by doc 18/doc 20 as needing an explicit failure
  outcome rather than a fabricated reversal — this document adopts that
  same rule: `RewardReversalDecision` processing for an irreversible
  fulfillment returns a typed `IrreversibleFulfillment` outcome, audited,
  surfaced to a human decision, never silently marked "reversed" for
  something that cannot be undone.
- **Authorization**: a reversal initiated by staff carries the identical
  reason-code and, above a configurable materiality threshold,
  four-eyes requirement CLAUDE.md's manual-adjustment rule requires —
  this applies uniformly to "hands or removes player value," per doc 20
  §10's own correct framing, not only to a wallet credit.

## Risk and RG — re-checked, not re-decided

The Reward Orchestrator does **not** re-run `risk.Evaluate` itself for a
decision it receives — the producing domain (Bonus, Gamification) already
did that before emitting the `RewardDecision`, and Risk exposure was
already fixed at decision time. RG is different: the Orchestrator MUST
re-check RG status (not Risk — RG's self-exclusion is the one condition
that can become newly true between decision and fulfillment and must
never be missed) immediately before actually crediting a wallet or points
balance. **Specialist-review correction (P1 fix, F17)**: an earlier draft
made this re-check CONDITIONAL — "if fulfillment is delayed... if δ is
non-trivial" — with no threshold defined for "non-trivial." This directly
weakens `docs/decisions/0034-bonus-gamification-rg-kyc-identity-
integration.md` §2.1's absolute rule ("every forward-going action
re-evaluates fresh, every time") and creates a real gap: an implementer
classifying synchronous fulfillment as "δ≈0" and skipping the check would
miss a self-exclusion that commits between decision (time T) and
fulfillment (time T+2ms) — exactly the race Stage 4G-FINAL's
`clock_timestamp()` fix demonstrates can happen even at millisecond
scale, because `internal/rg`'s own immediacy is what makes the window
matter, not its wall-clock size. The corrected rule: **the RG re-check is
UNCONDITIONAL — every fulfillment, synchronous or delayed, re-checks RG
immediately before crediting, with no "is δ non-trivial" judgment call
for an implementer to get wrong.** This mirrors `internal/casino`'s own
Stage 4G-FINAL lesson (the `clock_timestamp()` fix) applied without a
carve-out.

**Specialist-review precision addition**: this re-check MUST happen (a)
INSIDE the same database transaction that holds the
`(tenant_id, decision_id)` fulfillment lock (see Idempotency and
concurrency, below), immediately before the credit — never as an earlier,
separate check whose already-stale result is then carried into the locked
transaction, and (b) via a literal, unmodified call to
`rg.EvaluateEligibility` — never a new time-window comparison written for
this purpose. The original `internal/rg` bug this mirrors was specifically
about a check that ran too early relative to when a blocked transaction
actually resumed; re-checking outside the lock, or re-checking with a
freshly-written comparison, would reproduce the same failure shape rather
than avoid it.

## Idempotency and concurrency

Every fulfillment is idempotent on `decision_id` — a re-delivered or
retried `RewardDecision` for the same `decision_id` produces the
identical fulfillment outcome, never a second credit. For mechanisms
that ultimately reach `internal/ledger.Post` OR the points ledger
(`docs/architecture/24-points-accounting-architecture.md`), idempotency
is inherited for free from THAT ledger's own idempotency key —
**specialist-review correction (P1 fix)**: an earlier draft of this
section additionally claimed the Orchestrator's OWN fulfillment-tracking
table enforces idempotency for points movements; that is a second,
independently-keyed idempotency registry for the same movement (the
Orchestrator's `(tenant_id, decision_id)` vs the points ledger's own
`(tenant_id, idempotency_key)`, doc 24 §8) — a divergence risk with no
benefit, since the points ledger already enforces this at the database
level. The corrected rule: the Orchestrator's OWN fulfillment-tracking
table (below) exists ONLY for mechanisms that have no ledger of their
own to inherit idempotency from — badges/achievements (Gamification's own
state store) and `external_provider_reward` requests (no local ledger
effect until/unless value lands in a platform wallet, per
`docs/decisions/0032-bonus-accounting.md` §6(c)) — via a unique
constraint on `(tenant_id, decision_id)`, mirroring the ledger's
`(provider_id, provider_tx_id)` pattern. For `cash_credit`/`bonus_credit`
and `points_credit` fulfillment, the Orchestrator relies entirely on the
owning ledger's own idempotency guarantee and does not duplicate it.
Concurrent duplicate fulfillment attempts for the same `decision_id` are
serialized the same way Stage 4G-FINAL serialized concurrent duplicate
bet deliveries — a transaction-scoped advisory lock (or equivalent) keyed
on `(tenant_id, decision_id)`, acquired before the idempotency check, not
after.

## Audit

Every fulfillment attempt (success, failure, retry, `Unknown` outcome)
is audited via the existing `audit.Record` pattern. **Specialist-review
correction**: an earlier draft named the actor as
`system`/`reward-orchestrator`, which is not a valid `audit.Entry` shape
— `internal/audit`'s `ActorSystem` requires a nil `ActorID`, and every
other actor type requires a non-nil one, so a bare "system" string loses
per-worker attribution entirely. The Orchestrator identifies itself as
`ActorService` with its own registered service identity (ADR 0014's
pattern, mirroring `docs/architecture/24-points-accounting-
architecture.md` §9's actor matrix), never a bare literal. Every audit
record carries tenant, entity=the `decision_id`, before/after state (in
`Metadata`, since `audit.Entry` has no dedicated before/after fields),
`IPAddress`/`UserAgent`/`RequestID` where the fulfillment was triggered
by an API-driven action, and a reason code — this is the complete
lifecycle audit trail `docs/architecture/23-external-reward-provider-
contract.md`'s callback/audit requirements build on for external
fulfillment, extended to internal fulfillment too.

## Multi-tenancy / brand

The Orchestrator owns no tenant-FACING configuration surface (no
operator-editable settings of its own) — it is pure mechanism, driven
entirely by the `RewardDecision` it's given and the provider/ledger/
points configuration those decisions' owning domains already scoped
correctly. **Specialist-review correction (P1 fix)**: an earlier draft of
this section concluded from that fact that the Orchestrator "introduces
no new tenant/brand-scoping surface of its own" — this is incorrect. The
fulfillment-tracking table introduced in "Idempotency and concurrency"
above IS a new tenant-scoped, player-attributable table (it records, for
badges/achievements and external-provider requests, a per-player
fulfillment outcome) and requires the identical RLS discipline every
sibling table in this stage's documents carries: `tenant_id NOT NULL`,
`FORCE ROW LEVEL SECURITY`, a `tenant_staff_scope` policy gated on
`app.tenant_id`, a `player_self_scope` read-only policy gated on
`app.player_account_id`, and — the specific gap an earlier ADR 0031
specialist review found missing on `risk_rules` once already — the
`app.player_account_id IS NULL` guard on the staff-scope policy, so a
player-scoped connection cannot read another player's fulfillment rows.
This table is not exempt from RLS merely because the Orchestrator itself
holds no *configuration*.

## What this stage does NOT do

No `RewardDecision` struct is implemented in Go. No fulfillment table is
migrated. No External Reward Provider adapter exists. This document
establishes the contract three still-unimplemented domains (Bonus,
Gamification, and this one) will code against when a future stage
authorizes implementation.

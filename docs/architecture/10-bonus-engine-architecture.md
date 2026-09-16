# 10 — Bonus Engine Architecture

Status: Stage 4H-A architecture-freeze design. Supersedes the Stage 0 stub
this document originally held (four-layer data model, configuration axes,
and the ledger/free-round separation are carried forward and strengthened,
not discarded). Source: Blueprint §4.5, `docs/decisions/0007`,
`06-wallet-ledger-architecture.md`, `docs/decisions/0031`,
`02-domain-and-service-boundaries.md`, `08-casino-integration-
architecture.md`. **Design-only** — no schema, no Go code, no migrations.
Owned by `bonus-engine`. Ledger interactions require `ledger-finance`
sign-off; Risk/RG integration must not diverge from the patterns those
domains already establish.

## 0. Relationship to parallel Stage 4H-A work

This document is written concurrently with, and does not redesign or
duplicate:

- **Gamification/Tournament/Mission/Marketplace architecture**
  (`docs/architecture/17-20`, `architect`) — Bonus Engine is one of
  several reward-producing domains; it does not own missions, tournaments,
  or a points/loyalty ledger.
- **Points accounting architecture + Bonus Accounting ADR**
  (`docs/architecture/24`, `docs/decisions/0032`, `ledger-finance`) — owns
  how bonus liability/expense/conversion becomes actual ledger postings.
  This document owns the *instructions* fed into that design, never the
  posting mechanism itself (§6).
- **Bonus/Gamification Risk integration addendum** (ADR 0031 appendix,
  `risk`) and **RG/KYC integration ADR for Bonus/Gamification**
  (`identity-compliance`) — this document states the integration contract
  from the Bonus Engine's calling side (§4, §5); the authoritative rule
  content and any new RG/KYC nuance belongs to those specialists.
- **Provider Interoperability ADR on external bonus engines**
  (`sportsbook`) — this document states, from the Bonus Engine's own
  perspective, when it defers to an externally-fulfilled grant (§3); the
  authoritative shape of provider-native bonus engine interop belongs to
  that ADR.
- **Reward Orchestrator, canonical Activity/Event taxonomy, and
  `ExternalRewardProvider` contract** (Master Orchestrator) — neither of
  these exists as a committed document yet. Every reference to them below
  is stated as an explicit **ASSUMPTION**, not a fact this document
  invents authority over. Where the assumption turns out wrong, this
  document's Bonus-Engine-side contract is the part that must change, not
  the other way around.

`bonus-engine` does not block on any of the above landing first — the
lifecycle, type matrix, and integration rules below are written so they
compose with whatever shape those parallel documents converge on, per the
stage directive.

## 1. Complete lifecycle: Campaign → Offer → Grant → Activation → Progress → Completion → Conversion/Release → Expiry → Cancellation → Reversal

### 1.1 The four persistent layers (unchanged from Stage 0, clarified)

- **Campaign** — the marketing/compliance container: name, time window,
  target segment definition, jurisdiction/licensing/tenant/brand scope
  (§8), budget cap, and — new this stage — a **fulfillment owner**
  (`internal` vs `external:<provider_id>`, §3).
- **Offer** — a versioned rule set under a Campaign: the five
  configuration axes from the Stage 0 stub (Eligibility, Reward, Wagering,
  Payout, Abuse controls), unchanged in shape, detailed per bonus type in
  §2. A Campaign may carry multiple Offer versions over its lifetime;
  a Grant is always issued against one immutable Offer version, never a
  "live" reference that could change underneath an already-issued Grant.
- **Grant** — one player's instance of one Offer version. This is the
  state-machine entity described below.
- **Progress** — the append-only trail of every fact relevant to a single
  Grant: not only wagering contribution, but every lifecycle transition,
  every Risk/RG decision consulted, every externally-reported status
  change, and every reason code. This is the record a disputing player's
  case is resolved from (§10's completeness requirement).

### 1.2 Grant states

| State | Meaning |
|---|---|
| `issued` | A Grant row exists: the platform (or an external bonus engine, §3) has decided this player qualifies for this Offer. No wagering exposure yet if the reward requires opt-in or a triggering deposit that hasn't happened. |
| `activated` | The Grant's reward is live and wagering-relevant: cash/bonus funds exist in the player's wallet, free spins/free bets have been requested from the Reward Orchestrator, or a cashback window has started. |
| `in_progress` | Wagering/qualifying activity is being tracked against the Offer's wagering rules (implicit sub-state of `activated`, surfaced separately because it is where most Progress entries accumulate). |
| `completed` | The Offer's completion condition is met (wagering multiplier satisfied, cashback window elapsed, mission/tournament trigger satisfied). Conversion-eligible, not yet converted. |
| `converted` | The bonus-side value has been released per the Offer's payout rules (subject to max-cashout, cash-first/bonus-first ordering) — a terminal success state. |
| `expired` | The Offer's time limit elapsed before `completed` was reached. Terminal, triggers forfeiture instructions. |
| `cancelled` | The Grant was withdrawn before completion — either by player choice (opt-out) or staff action — before any forfeiture-worthy breach occurred. Terminal. |
| `forfeited` | An abuse-control or wagering-rule breach (excluded game played, max-bet-while-wagering exceeded, manual-review outcome) ended the Grant before completion. Terminal, always carries a reason code. |
| `reversed` | A compensating transition applied *after* another terminal state, because the event that justified the Grant was itself reversed upstream (e.g. the triggering deposit was charged back, a round the Grant's wagering credited was rolled back). Never a deletion or edit of the prior terminal state — an additional Progress entry and a new Grant status. |

### 1.3 Transitions and triggers

| From → To | Trigger type | Concrete trigger |
|---|---|---|
| *(none)* → `issued` | Automated rule evaluation | Event-bus consumption (`player.registered`, `deposit.settled`) matched against an active Offer's eligibility axis, after RG (§5) and Risk (§4) both allow |
| *(none)* → `issued` | Provider callback | An external bonus engine (§3) reports a grant decision via the Reward Orchestrator; Bonus Engine records it, does not re-decide eligibility |
| `issued` → `activated` | Player action | Explicit opt-in click, where the Offer requires one |
| `issued` → `activated` | Automated rule evaluation | A qualifying `deposit.settled` event for a deposit-triggered Offer that required no separate opt-in |
| `issued` → `activated` | Staff action | Manual activation (customer-service goodwill grant, VIP desk) |
| `activated` → `in_progress` / Progress append | Automated rule evaluation | Every `round.settled` / `bet.settled` event whose stake the Offer's contribution rules recognize |
| `activated`/`in_progress` → `completed` | Automated rule evaluation | Wagering multiplier reached, or a cashback window's settlement job closes the window |
| `activated`/`in_progress` → `completed` | Provider callback | Externally-fulfilled Grant reports its own completion (§3) |
| `completed` → `converted` | Automated rule evaluation | Conversion job applies payout rules (max cashout, cash/bonus ordering) and emits the conversion split instruction (§6) |
| `completed` → `converted` | Staff action | Manual release override (documented exception, always audited, §10) |
| `activated`/`in_progress`/`issued` → `expired` | Automated rule evaluation | Time-limit reached with no `completed` transition |
| `issued`/`activated` → `cancelled` | Player action | Player opts out where the Offer permits it |
| `issued`/`activated` → `cancelled` | Staff action | Administrative cancellation (reason code required) |
| `activated`/`in_progress` → `forfeited` | Automated rule evaluation | Wagering-rule breach (excluded game, max-bet-while-wagering) detected on a `bet.settled`/`round.settled` event |
| `activated`/`in_progress` → `forfeited` | Staff action | Manual-review queue outcome (abuse control, §1.4) |
| Any terminal state → `reversed` | Automated rule evaluation / provider callback | Upstream reversal of the event that justified the Grant (deposit chargeback, casino/sportsbook rollback of a round the Grant's Progress had already credited) |

### 1.4 Abuse-control's place in the lifecycle

Velocity caps and device/payment fingerprint linking are **detectors**,
not a separate state machine: a detector firing routes a Grant to
`review` (a queue, not a Grant state — the Grant itself stays in its
current state, e.g. `activated`, while a staff actor evaluates the queue
entry) and, on a confirmed abuse finding, staff drives the Grant to
`forfeited` or `cancelled` via the ordinary staff-action transitions
above. Bonus Engine does not invent a `review` Grant state distinct from
Risk's own `review` `Outcome` (§4) — a Risk `review` outcome on a
bonus-relevant operation is itself one path into the manual-review queue,
alongside Bonus-Engine-specific detectors (velocity, fingerprint linking)
that are bonus-domain concerns Risk's generic `Rule` shape cannot express
on its own (e.g. "this device fingerprint is linked to N other accounts
that already claimed this Offer" is a Bonus Engine detector, not a Risk
rule — Risk rules are amount/frequency-shaped, not identity-graph-shaped).

## 2. Bonus type support matrix

| Type | Fits Campaign→Offer→Grant? | Fulfillment path | Notes |
|---|---|---|---|
| Deposit bonus | Yes, cleanly | Internal — Grant `activated` on `deposit.settled`; reward posted as `player_bonus` balance via split instruction (§6) | Eligibility axis governs first-deposit-only, min/max, deposit method |
| Reload bonus | Yes, cleanly | Internal, identical to deposit bonus | Distinguished only by eligibility (existing depositor, not first-deposit) — same Offer shape, no new lifecycle concept |
| Cashback | Yes, with a distinct completion trigger | Internal | Completion is time/window-based (a settlement job closing the cashback window and computing net loss), not wagering-multiplier-based — Offer's Wagering axis is effectively bypassed in favor of the Payout axis's cashback % and window; Grant still moves `issued→activated→completed→converted` |
| Free spins | Grant decision fits; fulfillment does not | Grant decision internal; fulfillment via Reward Orchestrator → `casino`'s normalised free-round interface | Bonus Engine issues the GRANT (game, count, bet level) and a `fulfillment_requested` Progress entry; it never calls a casino provider API directly. Fulfillment status (`fulfillment_confirmed`/`fulfillment_failed`) arrives back through the Reward Orchestrator and is appended to Progress, not re-derived |
| Free bets | Same as free spins, sportsbook side | Grant decision internal; fulfillment via Reward Orchestrator → sportsbook's own normalised interface (`09-sportsbook-architecture.md`, once built) | Identical boundary to free spins — Bonus Engine never talks to a sportsbook provider |
| Wagering bonus (generic multiplier bonus with no deposit trigger, e.g. mission/tournament payout requiring playthrough) | Yes | Internal | Wagering axis is the primary mechanism; eligibility axis may be empty/always-true if the triggering condition is external (mission/tournament completion signal, §2's "mission rewards"/"tournament rewards" rows) |
| Cash reward (no wagering requirement at all) | Fits, but Wagering axis is a no-op | Internal | `activated` and `completed` happen atomically (multiplier = 1x / immediately satisfied) — modeled as a Wagering axis with multiplier 0 or "already satisfied," not a bypass of the state machine, so the Progress trail still records a `completed` transition and reason |
| Coupon (a code a player redeems to trigger an Offer) | Fits as an alternate `issued` trigger | Internal | The only lifecycle novelty is the *trigger* for `issued`→`activated` being a player-supplied code validated against an Offer, rather than an automated event — otherwise identical to a deposit/cash bonus |
| Tournament reward | Grant decision fits; the tournament's own ranking/prize logic does not belong here | Grant issued once Gamification/Tournament architecture (docs/architecture/17-20) emits a completion signal | Bonus Engine does not compute tournament standings, leaderboard logic, or prize tiers — it consumes a "tournament awarded reward X to player Y" signal (assumed shape, forthcoming from the parallel Gamification work) and issues a Grant exactly as it would for any other Offer. **Flag**: the event contract for this signal does not exist yet — recorded as a dependency, not designed here |
| Mission reward | Same as tournament reward | Grant issued once Mission architecture emits a completion signal | Same boundary and same open dependency as tournament rewards |
| Loyalty reward (VIP tier perk, points redemption) | Grant decision fits; the points/loyalty balance itself does not belong here | Grant issued once the Points/Loyalty accounting design (`docs/architecture/24`, `ledger-finance`) emits a redemption signal | Bonus Engine does not own a points balance or a points ledger — it is a consumer of a redemption event, symmetrical to the mission/tournament boundary above |

**General rule established by this matrix**: any reward type whose
*trigger* originates outside Bonus Engine's own five subscribed events
(missions, tournaments, loyalty/points) is still expressed as an Offer and
a Grant — Bonus Engine is the common lifecycle/state-machine/Progress-trail
substrate for every reward type in this list — but Bonus Engine never
computes the domain logic (tournament ranking, mission progress, points
balance) that decides *whether* the trigger fires. It only reacts to that
decision once made, exactly as it reacts to `deposit.settled` for a
deposit bonus. Any reward type whose *fulfillment* requires talking to a
casino/sportsbook provider (free spins, free bets) is issued as a Grant
decision here and fulfilled exclusively through the Reward Orchestrator's
normalised interface — never a direct provider call from Bonus Engine.

## 3. Coexistence with external (provider-native) bonus engines

**Hard rule**: our Bonus Engine is never assumed to be the only bonus
engine in the platform. At least one future sportsbook provider is known
to run its own.

**ASSUMPTION** (the `ExternalRewardProvider` abstraction, owned by the
Master Orchestrator, is not yet a committed document): it exposes
capability discovery, grant, revoke/cancel, status, settlement, callbacks,
idempotency, a provider reference, error classification, retry semantics,
health, and tenant/brand/provider isolation — the same shape discipline
`CasinoProvider`/`PaymentProvider` already establish (§3 of
`08-casino-integration-architecture.md`, ADR 0022 §2.1). Everything below
is written against that assumed shape; if the real contract differs, this
section's Bonus-Engine-side responsibilities are what should be revised.

### 3.1 When Bonus Engine issues a grant directly vs. delegates

- A **Campaign** carries a `fulfillment_owner`: `internal`, or
  `external:<provider_id>`. This is a Campaign-level configuration
  decision, not a runtime branch inside Bonus Engine's evaluation logic —
  "does provider X have its own bonus engine" is answered once, when the
  Campaign targeting that provider's product is configured, by reading the
  provider's advertised capability (ExternalRewardProvider capability
  discovery), never hardcoded per provider name in Bonus Engine code.
- **`internal`**: Bonus Engine owns eligibility (§4/§5), the Offer's rule
  content, and every Grant transition in §1.3's "automated rule
  evaluation" column.
- **`external:<provider_id>`**: Bonus Engine still creates a Grant row
  (so ledger reconciliation and reporting have one consistent surface
  regardless of fulfiller, per the requirement below) but the Grant's
  transitions are DRIVEN by the external provider's own callbacks, relayed
  through the Reward Orchestrator's `ExternalRewardProvider` interface —
  Bonus Engine does not re-evaluate that provider's own wagering-progress
  math, does not run its own eligibility check a second time once the
  external engine has already decided, and does not invent a competing
  state machine. It records what the external engine reports.

### 3.2 What Bonus Engine must be able to do regardless of fulfiller

- **Know which provider currently owns a given campaign's fulfillment** —
  the Campaign's `fulfillment_owner` field, resolved once at Campaign
  configuration time, read by every Grant issued under it.
- **Track an externally-fulfilled Grant's status without owning its
  lifecycle state machine** — every inbound status callback (via the
  Reward Orchestrator) is appended to that Grant's Progress trail
  verbatim (state reported, provider reference, timestamp) and mapped to
  the nearest matching state in §1.2's vocabulary for reporting
  consistency, but Bonus Engine never overrides, second-guesses, or
  independently recomputes what the external engine reports. If an
  external report does not cleanly map to §1.2's states, the Progress
  entry still records the external engine's own raw status string — the
  mapping is best-effort for cross-fulfiller reporting, not authoritative.
- **Reconcile against the ledger regardless of which system actually
  fulfilled the reward** — the accounting boundary (§6) treats an
  `external`-fulfilled Grant's lifecycle events (activation, completion,
  expiry, reversal) identically to an `internal` one: Bonus Engine emits
  the same shape of split instruction/lifecycle event either way, sourced
  from our own recorded Grant/Progress state, so `ledger-finance`'s
  posting logic and reconciliation job never need a fulfiller-specific
  code path. What differs is only WHERE the triggering fact came from (our
  own rule evaluation vs. an external callback), never what Bonus Engine
  hands to the ledger boundary.

## 4. Risk integration

**Hard architectural rule**: Bonus Engine MUST call
`internal/risk.Evaluate` for every bonus-issuance-affecting decision
(grant, activation, and — because a conversion changes player financial
exposure exactly like a bet does — completion/conversion) and MUST NOT
build its own limit engine, cap table, or velocity-threshold concept that
duplicates what `internal/risk`'s `Rule`/`RiskRequest` model already
expresses. This mirrors `internal/casino`'s pattern exactly (ADR 0031 §1,
§13): call `risk.Evaluate` inside the same transaction as the Grant's own
state-changing effect, before that effect commits; treat any non-nil
error, and (until a compliance-review queue exists) any `Outcome ==
review`, as a block — identical fail-closed contract, no bonus-specific
carve-out.

`risk.Operation` already has `OperationBonusGrant` (ADR 0031 §13's table:
listed `NOT IMPLEMENTED` pending Bonus Engine's own build, schema already
accepts it, migration 0041). No new `Operation` value is needed for grant
issuance. Activation and completion/conversion are the two points this
document identifies as also risk-relevant that ADR 0031 §13 did not
separately enumerate — recorded here as a **dependency on the parallel
`risk` addendum** to confirm whether they are modeled as the same
`OperationBonusGrant` (evaluated again at each transition) or distinct new
`Operation` values (`bonus_activate`, `bonus_convert`). This document does
not decide that; it only establishes that Bonus Engine calls `Evaluate`
at all three points, using whichever `Operation` value(s) the `risk`
addendum settles on.

`RiskRequest` fields resolve for a bonus operation exactly as they do for
casino: `TenantID`/`BrandID`/`PlayerAccountID` from the authenticated
Grant's own owning records (never client-supplied); `JurisdictionCode`
from the same authoritative source a deposit/casino session would use for
that player (§7); `LicensingMode` from `tenants.licensing_model` via the
identical `resolveLicensingMode` pattern; `AssetCode` from the wallet the
reward will post to; `Amount` is the reward's own monetary value in that
asset's minor units (for a non-monetary reward like free spins, `Amount`
is the reward's carrying/liability value if the Offer assigns one, or
zero if the operation is amount-less — mirroring `RiskRequest.Amount`'s
existing "zero is valid for an amount-less operation" contract);
`CorrelationID` ties the decision to the Grant.

### 4.1 Concrete example bonus-specific Risk rules (existing `Rule` shape, no new Risk concepts)

1. **HARD_LIMIT, jurisdiction-scoped promotional cap**: `RuleKind =
   hard_limit`, `Operation = bonus_grant`, `LimitKind =
   cumulative_amount`, `TimeWindow = rolling_day`, `JurisdictionCode =
   "<market>"`, `Threshold` = that jurisdiction's legal daily
   per-player bonus cap. Enforced regardless of any more specific
   commercial rule, exactly like a jurisdiction-scoped stake ceiling
   today.
2. **CONFIGURABLE_LIMIT, per-campaign/per-tier grant size**: `RuleKind =
   configurable_limit`, `Operation = bonus_grant`, `LimitKind =
   max_amount`, `TimeWindow = transaction`, scoped by `TenantID` +
   `BrandID` (a commercial default), with a `PlayerAccountID`-scoped
   override for a VIP desk manually approving a larger goodwill grant —
   most-specific-wins resolves the same way a player-specific stake
   override beats a brand default today (ADR 0031 §5's worked example).
3. **RISK_SIGNAL, grant frequency/velocity**: `RuleKind = risk_signal`,
   `Operation = bonus_grant`, `LimitKind = cumulative_amount` (used as a
   proxy for "count" via a rolling window over grant events, until a real
   `count`/`velocity` `LimitKind` is built per ADR 0031 §12's five-step
   extension model — **not** invented ahead of that model here),
   `TimeWindow = rolling_week`, scoped by `TenantID`. A breach contributes
   to `review`, routing the Grant to the manual-review queue (§1.4)
   without denying outright — device/payment fingerprint linking itself
   remains a Bonus-Engine-owned detector (§1.4), not a Risk rule, because
   it is identity-graph-shaped rather than amount/frequency-shaped.

## 5. RG integration

**Hard rule**: self-exclusion and RG restrictions block bonus issuance,
activation, and redemption/conversion exactly like they block a casino bet
today, via `internal/rg.EvaluateEligibility` — never a duplicated
self-exclusion concept inside Bonus Engine. Order of composition at every
Bonus Engine enforcement point mirrors `internal/casino/orchestrator.go`
exactly: RG first, Risk second, RG's denial short-circuits before Risk
ever runs (ADR 0031 §1). `EligibilityParams` resolves identically
(`TenantID`/`BrandID`/`PlayerAccountID`/`WalletID` from the Grant's own
authoritative records). A `Decision.Allowed == false` at grant time,
activation time, or conversion time blocks that transition outright and is
recorded as a `forfeited`/`cancelled` Progress entry with the RG decision's
own `Code` as the reason (§10) — never silently skipped.

This is a **dependency** on the parallel identity-compliance RG/KYC
integration ADR for Bonus/Gamification for any bonus-specific nuance (e.g.
whether a *pending* self-exclusion cooling-off period should block
`activated`→`in_progress` progression even for an already-activated Grant)
— this document's contribution is the calling contract, not new RG policy
content.

## 6. Accounting boundary — CRITICAL, owned entirely by `ledger-finance`

All monetary movement — promo liability, bonus liability, bonus expense,
cash conversion on wagering completion, expiry/forfeiture write-off,
reversal/cancellation/rollback compensating entries, and the
provider-funded vs. operator-funded vs. externally-fulfilled distinction —
is designed and owned by `ledger-finance` in `docs/decisions/0032-bonus-
accounting.md` and `docs/architecture/24` (both in parallel development).
**This document does not invent, assume, or partially design that
accounting treatment.**

Bonus Engine's own, complete responsibility at this boundary is exactly
two things, mirroring the Stage 0 stub's already-established separation,
strengthened:

1. **Split instructions** — for a wagering event with a bonus-funded
   component, the cash/bonus contribution split implied by the Offer's
   Wagering axis (per-game/category/provider contribution %) and the
   Grant's current balance mix. Bonus Engine computes *what* the split
   should be; it never issues the `player_cash`/`player_bonus` ledger
   posting itself (that remains `wallet`/`ledger`'s sole write authority,
   per `02-domain-and-service-boundaries.md`'s "only `wallet` may write
   ledger tables" rule).
2. **Lifecycle events** — `granted`, `activated`, `completed`
   (conversion-eligible, carrying the Offer's payout rules: max cashout,
   cash-first/bonus-first ordering, partial-release thresholds),
   `expired` (forfeiture), `cancelled`, `reversed` — each carrying enough
   context (Grant id, Offer version, amounts, reason code, correlation id)
   for `ledger-finance`'s design to turn into actual balanced postings,
   without Bonus Engine specifying account types, posting order, or
   liability recognition timing.

This split holds identically whether the Grant is `internal`- or
`external`-fulfilled (§3.2) — Bonus Engine emits the same lifecycle event
shape either way; only the triggering source differs.

**Never**: a bonus liability or balance tracked only in a Bonus Engine
side table that cannot be reconciled against the ledger. Every Grant's
financial state must be reconstructable from ledger entries
`ledger-finance`'s design produces from these events, not from Bonus
Engine's own Progress trail alone (the Progress trail explains the
*bonus-domain reasoning* — why a Grant reached a state; the ledger
explains the *money* — never two disconnected sources of truth for the
same dollar).

## 7. Jurisdiction/licensing compatibility

Bonus Engine consumes the existing canonical jurisdiction/licensing-mode
context exactly as `internal/risk`/`internal/casino` do — `jurisdictions.
code` (ADR 0006, platform registry, no RLS) and `tenants.licensing_model`
(`under_platform_licence`/`own_licence`) — as plain scope-matching values
resolved server-side by whatever caller establishes them for a given
player/operation (a future per-player jurisdiction resolver, or a Grant's
own denormalized context analogous to `casino_launch_sessions.
jurisdiction_code`, ADR 0031 §9). Bonus Engine never invents its own
jurisdiction taxonomy and never resolves jurisdiction itself.

A jurisdiction that bans a bonus type (e.g. a market prohibiting
wagering-requirement deposit bonuses, or capping cashback %) is expressed
as one of:

- A Risk `HARD_LIMIT` scoped by `JurisdictionCode` on `bonus_grant` (§4),
  denying/reviewing regardless of Offer configuration — the correct
  mechanism for a hard legal ceiling; or
- Offer-level eligibility configuration disabling that Offer for that
  `JurisdictionCode`/`LicensingMode` (an ordinary, operator-editable
  configuration row, per Campaign/Offer scope in §8) — the correct
  mechanism for a commercial/product decision to not offer a bonus type in
  a market even where it is legally permitted.

Never an `if jurisdiction == "X"` branch inside Bonus Engine's evaluation
logic — both mechanisms above are data, not code, exactly like every
other jurisdiction-varying rule in this platform (CLAUDE.md's compliance
section).

## 8. Multi-tenancy/brand scoping

| Entity | Scope | Resolution |
|---|---|---|
| Campaign | Platform-wide (`tenant_id IS NULL`), tenant-wide, or brand-specific | Mirrors `risk_rules`/`player_restrictions`' dual-scope (nullable `tenant_id`, optionally narrowed by `brand_id`) — a brand-specific Campaign replaces a tenant-wide one entirely for that brand, never a per-field merge, identical to `casino_game_availability`'s most-specific-row-wins rule |
| Offer (template/version) | Same scope as its owning Campaign | An Offer cannot be scoped more broadly than its Campaign — narrowing only |
| Grant | Always tenant + brand + player scoped | One player's real instance; never platform-wide. RLS: dual `tenant_staff_scope` + `player_self_scope` (read-only) pattern, identical shape to `casino_launch_sessions`/`withdrawal_requests` (migration 0026's precedent) |
| Progress | Same scope as its owning Grant | Append-only, inherits Grant's tenant/brand/player scope; staff read access follows the same dual-scope pattern |

A genuinely platform-wide Campaign (e.g. a platform-run promotion spanning
every brand under our own licence) is expressible but, per ADR 0031 §8's
already-recorded observation about platform-wide `risk_rules` writes,
likely reachable only via a platform-scoped write path that does not exist
yet for any domain — recorded here as the same open dependency, not
newly discovered by Bonus Engine.

## 9. Idempotency/concurrency

General pattern, mirroring `internal/casino`'s already-hardened equivalent
(idempotency key on `provider_id + provider_tx_id`, ADR 0031's
`pg_advisory_xact_lock` fix in Stage 4G-FINAL Part F) — no new mechanism
invented:

- **Grant issuance**: idempotency key = `(tenant_id, campaign_id,
  offer_version_id, player_account_id, trigger_reference)`, where
  `trigger_reference` is the triggering event-bus message's own dedupe
  id (or the triggering deposit's `provider_tx_id`, or the external
  bonus engine's provider reference for an `external`-fulfilled Grant). A
  redelivered `deposit.settled` event for the same deposit must never
  produce a second Grant.
- **Redemption/completion**: idempotency key = `(grant_id,
  completion_trigger_reference)` — the specific `round.settled`/
  `bet.settled` event (or external callback) that pushed wagering progress
  over the Offer's threshold. Concurrent triggers for the *same* Grant
  (e.g. two bet-settlement events arriving near-simultaneously, both
  computing that the multiplier is now satisfied) require serialization —
  an advisory-lock-or-equivalent scoped to the Grant id, mirroring the
  exact fix `postBet` needed for the identical concurrent-redelivery
  hazard (Stage 4G-FINAL Part F's `pg_advisory_xact_lock` on
  `(tenant_id, provider_id, provider_tx_id)`; here scoped to `grant_id`).
  Without this, two concurrent triggers could each independently decide
  "this Grant just completed" and double-emit a conversion lifecycle
  event.
- **Reversal**: idempotency key = `(grant_id, reversing_event_reference)`.
  A reversal for a Grant/Progress state never actually reached writes a
  tombstone (mirrors `payments.postDepositReversalTombstone` and
  `internal/casino`'s rollback-of-a-never-seen-original handling) so a
  late-arriving original reversal request is rejected rather than
  silently applied twice or applied to the wrong state.

No exact SQL, locking primitive, or schema is specified here — this is the
general contract `ledger-finance`'s and `bonus-engine`'s own
implementation-stage design must satisfy.

## 10. Audit requirements

Every mutating action below writes an `audit.Record` entry in the same
transaction as the action itself (existing `internal/audit` pattern —
`ActorType`/`ActorID`, `TenantID` (or `uuid.Nil` for a platform-level
Campaign), `Action`, `TargetType`/`TargetID`, `Outcome`, `IPAddress`/
`UserAgent`/`RequestID` for staff/API-driven actions, and a `Metadata`
map carrying before/after state and, for any manual action, a mandatory
reason code — `audit.Entry` has no dedicated before/after fields, so this
follows the same convention every existing domain in this codebase already
uses):

| Action | Actor types | Reason code required |
|---|---|---|
| Campaign creation / rule (Offer) changes | Staff | No (routine configuration) |
| Bonus issuance (Grant `issued`) | System (automated), Staff (manual) | Only for staff-initiated grants |
| Activation | System, Player, Staff | Only for staff-initiated |
| Completion / conversion | System | No, unless a staff override bypasses normal completion rules (then yes) |
| Cancellation | Player, Staff | Yes, always |
| Expiry / forfeiture | System, Staff (manual-review outcome) | Yes, always — this is the exact record a disputing player's case turns on |
| Manual adjustment / administrative override (e.g. staff-forced conversion, manual grant-size override above §4's configured threshold) | Staff | Yes, always, and — per CLAUDE.md's existing four-eyes-approval-above-threshold rule for manual balance adjustments — any such override above a configurable threshold requires the same two-actor approval pattern. This document does not design that approval workflow; it is flagged here as a requirement the backoffice/RBAC implementation stage must satisfy, not deferred silently |

### 10.1 Progress trail completeness (tested requirement, not merely documented)

Every row in §1.3's transition table appends a Progress entry — no
transition is ever applied to a Grant without a corresponding, immutable
Progress row in the same transaction. Each Progress entry carries: a
monotonic sequence number (append-only ordering within the Grant), the
transition type, the trigger type and its concrete reference (event id /
staff actor id / provider callback reference), the before-state and
after-state, any amounts/contribution detail relevant to that transition,
a reason code (mandatory for `forfeited`/`cancelled`/`reversed`), the
Risk/RG decision(s) consulted (their `Code`, not their full internal
detail) where applicable, and — once a lifecycle event has been posted —
the resulting ledger transaction group id from `ledger-finance`'s posting
(§6), so a Grant's Progress trail and its ledger footprint are always
cross-referenceable without being the same record.

## Cross-references

- Ledger split/posting mechanism: `06-wallet-ledger-architecture.md`,
  forthcoming `docs/architecture/24` and `docs/decisions/0032`
  (`ledger-finance`).
- Risk consumption pattern this mirrors: `docs/decisions/0031-risk-and-
  limits-engine.md`, `internal/risk/types.go`.
- RG consumption pattern this mirrors: `docs/decisions/0026-responsible-
  gaming-player-status-enforcement-foundation.md`, `internal/rg/rg.go`.
- Free-round/free-bet fulfillment boundary: `08-casino-integration-
  architecture.md` §8 ("free-round/bonus-stake normalization... deferred
  to the Bonus Engine stage"), `09-sportsbook-architecture.md` (once
  built).
- Jurisdiction/licensing model: `docs/decisions/0006-hybrid-licensing-and-
  jurisdiction-model.md`, `15-jurisdiction-and-licensing-model.md`.
- Domain boundary table: `02-domain-and-service-boundaries.md`.
- Audit pattern: `docs/decisions/0013-audit-log-immutability-and-dual-
  scope-rls.md`, `internal/audit/audit.go`.

## Open questions / dependencies (not resolved by this document)

1. Whether Grant activation/completion use a new `risk.Operation`
   (`bonus_activate`/`bonus_convert`) or reuse `bonus_grant` re-evaluated
   at each transition — depends on the parallel `risk` addendum to ADR
   0031.
2. The exact event/signal contract for tournament/mission/loyalty-reward
   triggers into Bonus Engine — depends on the Gamification/Tournament/
   Mission architecture (`docs/architecture/17-20`) and the Points
   accounting design (`docs/architecture/24`).
3. The real shape of `ExternalRewardProvider` (capability discovery,
   callback payload, error classification) — depends on the Master
   Orchestrator's Reward Orchestrator design; §3 above is written against
   an explicitly stated assumption, not a confirmed contract.
4. The canonical Activity/Event taxonomy's relationship to the five
   event-bus events this document treats as Bonus Engine's own inputs —
   depends on the Master Orchestrator's taxonomy work; this document
   assumes the five named events remain valid inputs and that any future
   taxonomy is additive.
5. The manual-adjustment four-eyes-approval workflow named in §10 is a
   requirement, not a design — belongs to a future backoffice/RBAC
   implementation stage.
6. Whether a genuinely platform-wide Campaign write path exists — same
   open gap ADR 0031 §8 already records for platform-wide Risk rules; not
   newly resolved here.

## Ownership and stage mapping

Owned by `bonus-engine`. Ledger interactions require `ledger-finance`
review (§6). Risk/RG call sites must not diverge from `risk`/
`identity-compliance`'s own domain designs (§4, §5). This document is
Stage 4H-A's Bonus Architecture deliverable — architecture only, no
implementation authorized by this stage.

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
  (`internal` vs `external:<provider_id>`, §3). **Wave-2 ledger-finance
  review finding (P2)**: the budget cap field has no stated enforcement
  mechanism anywhere in this document or in `internal/risk`'s per-player
  rule shape (ADR 0031 §4.1's rules are all per-player, not
  aggregate-scoped). It is `NOT IMPLEMENTED` and advisory only this
  stage; if enforced, the natural point is server-side at `bonus_grant`
  posting time, checked against that campaign's outstanding
  `player_bonus` plus recognized `bonus_expense` — but that check, and
  its owner, are not designed here.
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
| `completed` → `converted` | Automated rule evaluation | Conversion job applies payout rules (max cashout, cash/bonus ordering), emits the wagering split instruction (§6 item 1), and emits the `converted` lifecycle event (§6 item 2) — **specialist-review correction (Wave-2 ledger-finance P1-2)**: an earlier draft called this "the conversion split instruction (§6)," conflating the split instruction with the lifecycle event; ADR 0032 §3.1 binds `converted` to a `bonus_conversion` posting and requires the event to exist |
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
| Cashback | Yes, with a distinct completion trigger | Internal | Completion is time/window-based (a settlement job closing the cashback window and computing net loss), not wagering-multiplier-based — Offer's Wagering axis is effectively bypassed in favor of the Payout axis's cashback % and window; Grant still moves `issued→activated→completed→converted`. **Specialist-review addition**: the settlement job's own comparison against "now" to decide the window has elapsed MUST read `clock_timestamp()`, never `now()` — see `docs/architecture/17-gamification-engine-architecture.md` §5.2; this is a binding requirement given a settlement job can be queued behind the same Grant-completion advisory lock discussed in §9 below |
| Free spins | Grant decision fits; fulfillment does not | Grant decision internal; fulfillment via Reward Orchestrator → `casino`'s normalised free-round interface, **forthcoming — not yet built** (confirmed by specialist review: `internal/casino`'s `CasinoProvider` interface has no free-round method today; `08-casino-integration-architecture.md` §1 records this as an `OPEN DECISION` deferred to the Bonus Engine implementation stage) | Bonus Engine issues the GRANT (game, count, bet level) and a `fulfillment_requested` Progress entry; it never calls a casino provider API directly. Fulfillment status (`fulfillment_confirmed`/`fulfillment_failed`) arrives back through the Reward Orchestrator and is appended to Progress, not re-derived |
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
- **Reconcile against the correct system of record regardless of which
  system actually fulfilled the reward.** **Specialist-review correction
  (P1 fix)**: an earlier draft of this bullet claimed `external`- and
  `internal`-fulfilled Grants receive IDENTICAL ledger treatment and that
  `ledger-finance`'s posting logic "never needs a fulfiller-specific code
  path" — this directly contradicted
  `docs/decisions/0032-bonus-accounting.md` §6(c), the authoritative
  accounting decision (Financial/Ledger's own veto applies here): an
  externally-fulfilled Grant (the provider's own bonus engine owns
  fulfillment end to end, value never enters a platform wallet) posts
  **ZERO ledger entries** — no `promo_liability`, no `player_bonus`, no
  `bonus_expense` — precisely because mirroring a balance held in a
  provider's own system is a second financial truth system, exactly what
  ADR 0032 §0 and this document's own §6 forbid. The corrected contract:
  Bonus Engine's lifecycle events carry an explicit
  fulfillment-destination flag (`into_platform_wallet` vs
  `inside_provider`, per ADR 0032 §6(c)'s "a reward type that does not
  declare it is rejected at configuration time rather than guessed at
  posting time"). For `into_platform_wallet` Grants (internal, or
  provider-*funded*-but-platform-fulfilled per ADR 0032 §6(b)),
  `ledger-finance`'s posting logic applies unchanged. For
  `inside_provider` Grants, Bonus Engine still records the Grant/Progress
  trail (for reporting, RG/limit visibility, and player-support
  answerability, per ADR 0032 §6(c)) but emits NO lifecycle event to the
  ledger boundary at all — reconciliation for these is the memo/audit
  stream ADR 0032 §6(c) and `reconciliation-model.md` §2.10 define, never
  a ledger-vs-ledger comparison. The moment external value genuinely lands
  in a platform wallet (the provider settles a free-bet win as a real
  payout), that is an ordinary provider settlement posting into
  `player_cash` under Flow 9's existing shape — not a bonus grant, and it
  must not create a bonus balance or wagering requirement on our side.

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
authoritative records). A `Decision.Allowed == false` at GRANT or
ACTIVATION time blocks that transition outright and is recorded as a
`cancelled` Progress entry with the RG decision's own `Code` as the reason
(§10) — never silently skipped.

**Specialist-review correction (P1 fix) — conversion is NOT
`forfeited`/`cancelled` on RG/Risk denial.** An earlier draft of this
section grouped conversion-time denial with grant/activation-time denial
and forfeited the Grant on any of the three — this directly contradicts
two other frozen documents from this same stage: ADR 0031 §15a-ii ("a
`DENY`, `REVIEW`, or error [at conversion] blocks the transition and
leaves the Grant in `completed`, which is non-terminal and retryable
after review — it does NOT forfeit") and
`docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md`
§2.2 ("already-committed effects are never retroactively reversed or
clawed back"). Forfeiture posts `Dr player_bonus / Cr promo_liability`
under ADR 0032 §5 — it EXTINGUISHES the player's already-earned bonus
balance. Auto-forfeiting a fully-wagered-through balance the instant a
player self-excludes turns self-exclusion into a financial penalty,
exactly the perverse incentive (delay self-excluding to avoid losing the
balance) ADR 0034 §2's own reasoning exists to prevent. The corrected
rule: a `Decision.Allowed == false` at CONVERSION time leaves the Grant in
`completed` (non-terminal, retryable once eligibility is re-checked and
passes, or resolved through whatever future compliance-review workflow
ADR 0031 §11/§17 anticipates) — it is recorded as a Progress entry with
the RG/Risk decision's own `Code`, but it is never a `forfeited` or
`cancelled` transition. This mirrors ADR 0034's own explicit position on
the flagged "unlock an already-wagered-through balance post-exclusion"
question: settle it as a mechanical entitlement, don't confiscate it.

This is a **dependency** on the parallel identity-compliance RG/KYC
integration ADR for Bonus/Gamification for any further bonus-specific
nuance (e.g. whether a *pending* self-exclusion cooling-off period should
block `activated`→`in_progress` progression even for an already-activated
Grant) — this document's contribution is the calling contract, not new RG
policy content.

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
   `converted`, `expired` (forfeiture), `cancelled`, `reversed` — each
   carrying enough context (Grant id, Offer version, amounts, reason code,
   correlation id) for `ledger-finance`'s design to turn into actual
   balanced postings, without Bonus Engine specifying account types,
   posting order, or liability recognition timing. **Specialist-review
   addition (Wave-2 ledger-finance P1-2)**: an earlier draft of this list
   omitted `converted` even though ADR 0032 §4 defines a posting
   (`bonus_conversion`) that only that event can trigger; ADR 0032 §3.1
   is the binding event-to-posting map, including which single event
   (`activated`, never `issued`) produces the one `bonus_grant` posting
   per Grant, and how `cancelled` and a direct `cash_credit` reward both
   post (`bonus_forfeiture` and a two-entry expense recognition
   respectively — neither was previously defined anywhere).

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
  an advisory-lock-or-equivalent scoped to **`(tenant_id, grant_id)`**,
  mirroring the exact fix `postBet` needed for the identical
  concurrent-redelivery hazard (Stage 4G-FINAL Part F's
  `pg_advisory_xact_lock` on `(tenant_id, provider_id, provider_tx_id)`).
  **Specialist-review correction**: an earlier draft of this section
  scoped the lock to `grant_id` alone; that is the same un-tenant-scoped
  shape three independent Stage 4G-FINAL reviewers (architect, security,
  multi-tenancy) already found and fixed once for `postBet`'s own lock,
  and citing that fix by name while not adopting its tenant-scoping would
  let a future implementer copy the mistake forward. `tenant_id` costs
  nothing extra here and closes the hash-collision risk regardless of
  whether `grant_id` generation is later proven collision-resistant
  across tenants. Without this lock, two concurrent triggers could each
  independently decide "this Grant just completed" and double-emit a
  conversion lifecycle event.
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

## Stage 4H-B0 — MVP Implementation Scope Plan

Status: **architecture/scope-planning only, `NOT IMPLEMENTED`.** Issued as
part of "STAGE 4H-B0 — BONUS + GAMIFICATION + RETAIL ARCHITECTURE/SCOPE
FREEZE" per that stage's directive. No schema, no Go code, no migration is
authorized by this section. It does not redesign anything in §1–§10
above (Stage 4H-A's frozen architecture) — it narrows *which parts of
that already-frozen design* the eventual Stage 4H-B1 implementation
stage would build first, per the Stage 4H-A Wave-2 `product-owner-proxy`
scope review recorded in `14-mvp-scope-and-roadmap.md`'s "Features
deliberately deferred" section and its "Bonus Engine implementation
scope note". Owner: `bonus-engine`.

### 1. Exact bonus types in the first implementation slice

`ARCHITECTURAL DECISION` (scope, not rule content — the rule content for
every row below is already frozen in §2's type matrix; this only decides
build order).

**IN the first slice** — exactly these five rows of §2's type matrix,
naming them by their exact §2 row label:

1. **Deposit bonus** — internal fulfillment, `activated` on
   `deposit.settled`, eligibility axis governs first-deposit-only/min-
   max/deposit method. No dependency outside what already exists
   (`wallet`/`ledger`, `internal/risk`, `internal/rg`).
2. **Reload bonus** — identical Offer shape to deposit bonus, eligibility
   axis only distinguishes it (existing depositor). No new lifecycle
   concept, no new dependency.
3. **Cashback** — distinct completion trigger (settlement job closing a
   time window, `clock_timestamp()`-based per §2's specialist-review
   addition), Payout axis's cashback %/window rather than the Wagering
   axis. No new dependency; the settlement job itself (a scheduled/cron
   invocation) is an implementation detail of this slice, not a new
   architectural component.
4. **Wagering bonus (generic multiplier bonus, no deposit trigger)** —
   §2's "generic wagering bonus" row, used as the general-purpose Offer
   shape for any operator-authored promotion whose eligibility is
   deposit/segment/coupon-based rather than mission/tournament-triggered.
   Explicitly: this slice does **not** build the mission/tournament
   *producers* of such a bonus (item 2 below) — only the Bonus Engine's
   own ability to run a wagering-multiplier Offer to completion for a
   Grant issued by one of the other four in-slice triggers.
5. **Coupon** — the only lifecycle novelty is the `issued`→`activated`
   trigger being a player-supplied code validated against an Offer,
   otherwise identical to a deposit/cash bonus. Requires one new
   surface not named elsewhere in §1–§10: a coupon-code validation/
   redemption endpoint (input validation + Offer lookup), scoped as
   ordinary `bonus-engine`-owned HTTP surface under
   `25-bonus-gamification-api-architecture.md`'s existing API/RBAC
   contract — no new architectural concept, just an additional handler.

**Explicitly OUT of the first slice**, named individually rather than
left implied:

- **Free spins / free bets** — Grant *decision* fits the frozen model
  (§2), but fulfillment does not: `internal/casino`'s `CasinoProvider`
  interface has no free-round method today, confirmed an `OPEN DECISION`
  in `08-casino-integration-architecture.md` §1 and restated in §2's own
  table above; sportsbook free bets are additionally blocked on
  `internal/sportsbook` not existing at all. Building the Grant-issuance
  half without a fulfillment path would ship a bonus type that can never
  leave `issued`/`activated` for a real player — explicitly deferred,
  not partially built.
- **Cash reward (no wagering requirement)** — architecturally the
  simplest row in §2's matrix and fully specified end-to-end by ADR 0032
  §3's two-entry posting, but it is **not** one of the five types the
  `product-owner-proxy` scope review named for the first slice
  (`14-mvp-scope-and-roadmap.md`'s "Bonus Engine implementation scope
  note" lists exactly deposit/reload/cashback/generic-wagering/coupon).
  Recorded here rather than silently added: this document does not
  expand the named list on its own authority (`CLAUDE.md`'s
  no-uncontrolled-scope-expansion rule cuts both ways — a specialist
  does not shrink *or* grow an already-scoped list unilaterally). If the
  Orchestrator/product owner wants goodwill/manual cash grants in the
  first slice, that is a one-line scope addition to make explicitly, not
  an assumption this document should make for them.
- **Tournament reward, Mission reward, Loyalty reward** — all three
  rows are blocked on the deferred Gamification Engine
  (`14-mvp-scope-and-roadmap.md`'s "Features deliberately deferred") and
  the Points accounting design (`docs/architecture/24`), neither of
  which is authorized for implementation. Per §2's own general rule,
  Bonus Engine is the common lifecycle substrate for these types but
  never the producer of the triggering signal — with no producer
  authorized to exist, there is nothing for these Offer shapes to react
  to. Out of the first slice, not implied-included, exactly as the
  `product-owner-proxy` review instructed.

### 2. Package/domain ownership — the Risk & Limits gate, answered directly

`ARCHITECTURAL DECISION` / gate-check finding, `NOT IMPLEMENTED`.

`docs/governance/ownership.md` currently lists `internal/bonus` as "not
yet created — blocked on Risk & Limits stability per Stage 4G §32"
(mirrored in `docs/governance/project-status.md`'s Blocked Stages list).
That block is a real gate, not a formality, and it is answered here
rather than waved through:

**The Risk & Limits engine's core mechanism is stable enough to unblock
`internal/bonus` package creation for the first slice named in §1,
subject to two named, additive, non-redesign conditions below — this is
a qualified pass, not an unconditional one.**

Evidence for the "core mechanism is stable" half:

- `internal/risk`'s rule precedence, fail-closed contract, RLS shape,
  and the `postBet`-class concurrency hazard (`pg_advisory_xact_lock`)
  were independently hardened across Stage 4G, Stage 4G-FINAL (11/11
  specialist areas reviewed), and the dedicated
  Stage 4G-FINAL-FINANCE-GATE follow-up, which produced an independent
  `ledger-finance` **PASS, sign-off GRANTED**, no P0/P1 findings, on the
  exact concurrency/correctness properties a bonus grant/conversion path
  would also depend on (`docs/progress.md`'s Stage 4G-FINAL-FINANCE-GATE
  entry).
- The Bonus-specific integration *contract* (not just "call
  `risk.Evaluate` somewhere") is now fully specified, not merely
  gestured at: ADR 0031 §14–§18 name the exact `Operation` value for
  each of the three checkpoints this document's §4 requires (grant/
  activation reuse `OperationBonusGrant`, already a real Go constant
  accepted by migration 0041; conversion needs a new `bonus_conversion`
  value, specified in full by §15a-ii but not yet created in code), the
  exact composition order with RG (§5, unchanged from `internal/casino`'s
  precedent), and which existing `LimitKind`s already work unmodified for
  a bonus operation (`min_amount`/`max_amount` with
  `TimeWindow: transaction` — "no change whatsoever" per ADR 0031 §15b).

Evidence for the "subject to two conditions" half — genuine, named gaps,
not invented caution:

1. **The first slice's own Risk rules must be restricted to
   `min_amount`/`max_amount` (transaction-window) limits only** —
   `cumulative_amount` (a rolling-week bonus cap) is a documented,
   fail-closed-erroring gap today (ADR 0031 §15b:
   `operationLedgerTransactionTypes` has no `bonus_grant` entry yet, so
   `Rule.breach()` returns `ErrUnsupportedCumulativeOperation` for any
   such rule), and frequency/velocity caps require a `count` `LimitKind`
   that does not exist yet (§15c). Neither blocks the five bonus types in
   §1 — a jurisdiction/campaign/tier `max_amount` per-grant ceiling is
   sufficient for all five, and this restriction is exactly what the
   `product-owner-proxy`'s minimal-first-slice recommendation already
   implies. This is a scoping choice for the first slice's *rule
   authoring*, not a code change.
2. **A small, already-fully-specified, additive Risk change must land
   before the conversion checkpoint can be wired**: the `bonus_conversion`
   `Operation` value (Go constant in `internal/risk/types.go` + an
   additive widening of migration 0041's `Operation` CHECK constraint).
   ADR 0031 §15a-ii and §16 already specify its exact shape and naming;
   this is a scoped, single-item dependency request `bonus-engine` files
   against `risk` at the start of implementation (per
   `docs/governance/integration-protocol.md`'s dependency-request
   procedure), not an open design question and not evidence the engine
   itself is unstable.

Neither condition requires `internal/risk`'s signature, rule-table shape,
or precedence algorithm to change, and neither is a new limit engine
built inside `internal/bonus` — both would violate §4's hard rule if they
were. On that basis: **the Stage 4G §32 block is lifted for the first
slice specifically.** It is not lifted for any bonus type this document's
§1 places outside the first slice (a `cumulative_amount`/`count`-shaped
rule is more likely to matter for a high-volume mission/tournament reward
producer than for five simple operator-authored Offer types) — re-assess
the gate when Gamification is ever authorized.

`internal/bonus` is confirmed as the correct package path, owned by
`bonus-engine` per `docs/governance/ownership.md`'s existing convention
(new tables/migrations owned by the domain that creates them, per that
document's rule 3) — no change to the ownership table's shape is needed,
only the removal of its current "blocked" qualifier once this scope plan
is authorized.

### 3. Migration sequencing for the first slice ONLY (ordered list, no SQL)

`ARCHITECTURAL DECISION` (sequencing only), `NOT IMPLEMENTED`. Every item
below is additive (no existing row/column is altered or dropped), per
this project's established migration discipline. Table/RLS pairing
follows the existing per-migration convention (a table and its own RLS
policies ship together, e.g. migration 0019's `wallets`) — not called out
per row below.

1. **`bonus_expense` account type** — additive widening of
   `ledger_accounts.account_type`'s `CHECK` constraint (ADR 0032 §2). No
   dependency on anything else in this list; the smallest, most isolated
   change, so it goes first, mirroring how casino's migration 0035 also
   led with its own `ledger_transactions` CHECK widening before any
   casino table existed.
2. **`bonus_grant`, `bonus_conversion`, `bonus_forfeiture`,
   `bonus_reversal` transaction types** — additive widening of
   `ledger_transactions.transaction_type`'s `CHECK` constraint (ADR 0032
   Consequences). Sequenced immediately after (1) rather than before it:
   a `bonus_conversion` posting's mirror leg debits `bonus_expense`
   (ADR 0032 §4), so the account type it will reference should exist
   first even though Postgres does not structurally enforce that
   ordering between two independent `CHECK` constraints — a practical
   safety ordering, not a hard schema dependency. Both (1) and (2) are
   `ledger-finance`-owned migrations (per `ownership.md`'s Financial/
   Ledger row); `bonus-engine` files a dependency request for them rather
   than authoring them directly.
3. **`risk_rules`/`RiskRequest.Operation`'s `bonus_conversion` value** —
   additive widening of migration 0041's `Operation` CHECK constraint +
   the corresponding `internal/risk/types.go` constant (ADR 0031 §15a-ii,
   §16). Cross-domain, `risk`-owned; no structural dependency on (1)–(2)
   or (4)–(7), but must land before the Grant conversion write path (part
   of (6)/(7) below) is wired, since conversion cannot be gated by an
   `Operation` value that does not exist. Listed third because it is the
   other cross-domain dependency this slice needs before any bonus-owned
   table's write path can be considered complete, not because of a
   database-level ordering requirement.
4. **`bonus_campaigns` table + RLS** — first of doc 10 §1.1's four
   layers, no FK dependents among the other three yet. `bonus-engine`-
   owned. Dual/nullable-`tenant_id` scope per §8's Campaign row
   (platform-wide/tenant-wide/brand-specific).
5. **`bonus_offers` table + RLS** — FK to `bonus_campaigns`, so it must
   follow (4). `bonus-engine`-owned. Scope inherits the owning Campaign's
   scope per §8's Offer row (narrowing only).
6. **`bonus_grants` table + RLS** — FK to `bonus_offers` (a Grant is
   always issued against one immutable Offer version, §1.1) and to the
   player's own `wallets`/`player_accounts` row; must follow (5). This is
   also the table whose write path needs (3)'s `Operation` value wired at
   the conversion transition — recorded here, not designed here (no
   schema detail for that column is specified by this scope plan).
   `bonus-engine`-owned. Always tenant + brand + player scoped, dual
   `tenant_staff_scope`/`player_self_scope` RLS per §8's Grant row,
   mirroring `casino_launch_sessions`/`withdrawal_requests`.
7. **`bonus_progress` table + RLS** — FK to `bonus_grants`; last, since
   Progress inherits its owning Grant's tenant/brand/player scope and its
   completeness requirement (§10.1) presupposes the Grant row it appends
   against already exists. `bonus-engine`-owned, append-only (mirrors
   `audit_log`'s enforcement pattern per §10's own audit convention).

No migration for a coupon-specific table is listed separately: per §1
item 5, a coupon is an alternate `issued`→`activated` trigger against an
ordinary Offer, not a new persistent layer — its validation/redemption
endpoint reads (5)/(6), it does not need its own table in this slice
(a redeemed-code uniqueness constraint, if needed, is a column/index on
(5), not a new migration item, and is left to the implementation stage to
specify).

### 4. What is explicitly NOT built in this first slice

`ARCHITECTURAL DECISION`, `NOT IMPLEMENTED` for all four items:

- **The Reward Orchestrator.** Confirmed assumption, matching
  `14-mvp-scope-and-roadmap.md`'s own "Bonus Engine implementation scope
  note": until a second concrete reward-producing domain exists (i.e.
  Gamification is authorized, or a real sportsbook free-bet fulfillment
  need materializes), Bonus Engine fulfills its two accounting-boundary
  responsibilities from §6 — split instructions and lifecycle events —
  **directly** through `wallet`/`ledger` (for the five in-slice types,
  every one of which is `into_platform_wallet` per §3.2's fulfillment-
  destination flag) with no separate orchestration package in between.
  This is not this document's call to make unilaterally — a separate
  Orchestrator-authored synthesis document is resolving the Reward
  Orchestrator's own fate across all of Stage 4H-A's frozen domains —
  but it is the assumption this scope plan is written against, and it
  matches the product-owner-proxy's own recorded reasoning (building a
  three-domain-ready orchestration layer ahead of a second concrete
  reward-producing domain is "generality for a hypothetical future need"
  under `CLAUDE.md`'s scope test). If the Orchestrator's synthesis
  resolves it differently, this section — not §6's accounting boundary
  — is what changes.
- **Free spins / free bets fulfillment**, including any call into
  `internal/casino`'s (nonexistent) free-round method or
  `internal/sportsbook` (nonexistent package) — blocked on those
  interfaces, per §1's "OUT of the first slice" list above. The Grant-
  decision *shape* for these types remains as frozen in §2; only its
  build is deferred.
- **The `ExternalRewardProvider` contract** — blocked on an actual
  sportsbook provider relationship existing (`14-mvp-scope-and-roadmap.md`
  defers `docs/decisions/0033`/`docs/architecture/23` "until sportsbook
  architecture actually starts"); §3 above remains an explicit
  **ASSUMPTION** section, not something this slice implements or needs to
  implement, since every first-slice Campaign is `fulfillment_owner:
  internal` by construction (no external bonus engine exists to delegate
  to yet).
- **Gamification entirely** — points/XP/levels/achievements/badges/
  missions/tournaments/leaderboards/streaks/the Reward Marketplace, per
  `14-mvp-scope-and-roadmap.md`'s "Features deliberately deferred". No
  first-slice table, endpoint, or Risk rule references any Gamification
  concept.

### 5. Retail interaction — no first-slice change, explicitly deferred rather than assumed

`RECOMMENDATION`, `NOT IMPLEMENTED`. This stage's parallel retail
agent-hierarchy work (`architect`'s forthcoming
`26-retail-operations-architecture.md`, `security`'s forthcoming ADR
0036, and `identity-compliance`'s own Stage 4H-B0 addendum to
`05-identity-architecture.md` — read for this section, not authored by
it) does **not** change anything in this first slice, for a reason
grounded in this document's own existing design rather than a new
assumption invented here:

- Bonus Engine's `issued`/`activated` automated triggers (§1.3) already
  react to event-bus facts (`deposit.settled`, `player.registered`), not
  to *how* those facts were produced. `05-identity-architecture.md`'s
  Stage 4H-B0 section records that a retail-originated player
  registration reuses the identical `identity.RegisterPlayer*` call path
  as an online one, distinguished only by a new `registration_channel`
  provenance column — an identity-domain fact Bonus Engine has no reason
  to read for eligibility unless a future Offer's eligibility axis is
  explicitly configured to key off it (an ordinary, already-supported
  configuration capability per §8, not a new mechanism). The same
  reasoning applies to a hypothetical retail cash deposit: **if** a
  future retail cash-deposit flow emits the same `deposit.settled` event
  shape the online deposit flow does, a deposit/reload bonus triggers
  identically with zero Bonus Engine code change. That retail cash flow
  does not exist yet (per `05-identity-architecture.md`'s own note, it is
  `payments`/`ledger-finance`'s to design when retail cash handling is
  implemented) — this document does not build toward it, only records
  that its own design does not need to change when it arrives.
- **Explicitly deferred, not assumed impossible**: (a) a retail-specific
  eligibility axis dimension (e.g. "only at location X" or "only via a
  specific agent tier") — no Blueprint or MVP-roadmap requirement names
  this, and §8's existing Campaign/Offer scope table (platform/tenant/
  brand) has no location dimension; (b) a cashier-initiated Grant
  issuance/activation as a new *staff action* trigger type (e.g. handing
  a player a printed coupon in person, or a VIP-desk-style manual
  activation performed at a retail counter rather than online) — §1.3
  already has a generic "Staff action" trigger column that would cover
  this mechanically, but this document does not claim it is validated
  for a retail cashier specifically (RBAC/permission scoping for a
  cashier-shaped staff principal is `identity-compliance`'s/`security`'s
  parallel work, not resolved here); (c) redeeming a coupon in person at
  a retail counter rather than by the player entering a code themselves
  — same mechanical fit via the "Staff action" trigger, same deferral for
  the same RBAC-scoping reason. None of (a)–(c) is required by the
  Blueprint or by `14-mvp-scope-and-roadmap.md`'s B2C MVP scope; all
  three are recorded here as deferred so a future retail-bonus need is a
  documented revisit, not a silent gap discovered in production.

### 6. Cross-references for this section

`14-mvp-scope-and-roadmap.md` ("Features deliberately deferred", "Bonus
Engine implementation scope note"); `docs/decisions/0032-bonus-
accounting.md` §§2–4, Consequences; `docs/decisions/0031-risk-and-limits-
engine.md` §§12–18; `docs/governance/ownership.md`; `docs/governance/
project-status.md` (Blocked Stages); `05-identity-architecture.md`'s
Stage 4H-B0 section (read, not authored, by §5 above).

Owner of this section: `bonus-engine`. Nothing in this section
authorizes writing `internal/bonus`, a migration, or a test — it is the
scope plan Stage 4H-B1 (if and when authorized) would follow.

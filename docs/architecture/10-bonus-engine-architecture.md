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

> **Gap flagged (`bonus-engine` self-review, Stage 4H-B0-R5 Wave 3, from
> reviewing ADR 0037 §C.5's asset-authorization RBAC surface): no state
> above cleanly covers a Grant that cannot complete because the platform,
> not the player, made completion impossible.** ADR 0037 §C.5.3
> deliberately requires **no** dual control to suspend/deactivate an asset
> (op 4) or revoke its wagering eligibility (layer 7) — a single-actor,
> fail-closed action that can happen at any time, including mid-campaign,
> against the exact asset a running bonus Grant is denominated in. If that
> happens while a Grant is `activated`/`in_progress`, the player can no
> longer place the bets needed to satisfy the wagering requirement, through
> no fault of their own — yet neither `forfeited` (defined above as a
> breach) nor `cancelled` (defined below as "before any forfeiture-worthy
> breach occurred," but posted with the *identical* Dr `player_bonus`/Cr
> `promo_liability` write-off shape as forfeiture, per ADR 0032 §3.1)
> accurately represents "the platform made this Grant uncompletable and
> extinguished the player's bonus balance anyway." A disputing player
> shown either reason code would see a state that does not honestly
> describe why their bonus balance disappeared — a direct conflict with
> this document's own Progress-trail mandate. This compounds the identical
> gap `ledger-finance`'s §6.3.5.1 focus question 4 (`ledger-accounting-
> model.md` §6.3.3.2/§6.3.5.3) already named for a bonus-funded stake
> **currently locked in an open bet** at forfeiture time — both are the
> same underlying unspecified case (a forfeiture-class event firing against
> value the standard write-off posting cannot cleanly reach or should not
> silently write off), reached by two different triggers. **Not resolved
> here**: whether a platform-caused inability to complete should write off
> the balance at all (as opposed to, e.g., converting it, extending the
> time limit, or refunding it), and whether it needs a distinct terminal
> state/reason code rather than overloading `cancelled`, is a `bonus-engine`
> + `ledger-finance` design question, filed as an open item, not decided by
> this review.
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

**Cross-reference — sportsbook's locked-stake window is a second, concrete
trigger path for the same open question above (Human decision required).**
The "unlock an already-wagered-through balance post-exclusion" nuance
flagged two paragraphs up (ADR 0034 §2, restated at its §14.9) is not the
only route into this one open question. `ledger-accounting-model.md` §6.3
/ ADR 0038 §15's sportsbook locked-stake window (`player_locked`, pending
the `player_locked`-origin-split reviewed by `bonus-engine` this stage, ADR
0032 §5/§10) creates a second, independent trigger: a sportsbook bet's
stake can remain locked past the point its funding Grant has separately
gone terminal (`expired`/`cancelled`/`forfeited`), because bet-settlement
timing and Grant-lifecycle timing are not coupled. When that locked stake's
later WIN settlement — or a `VOID_ON_SELF_EXCLUSION` void (ADR 0034 §14.7)
returning a bonus-funded locked stake to `player_bonus` — credits
`player_bonus` "continuing wagering progress" against a Grant that has
nothing left to continue, the Grant state machine (§1.2) has no transition
defined for a settlement/void credit arriving against an already-terminal
Grant.

This is the **identical unresolved question** as the ADR 0034 §2 nuance
above, reached by a different path — not a second question needing a
separate answer. Both trigger paths (i) completing an already-fully-
satisfied wagering requirement after self-exclusion, and (ii) a locked
stake settling or voiding against a Grant that has already gone terminal
for any reason (expiry, cancellation, forfeiture — not only
self-exclusion) — resolve to the same single choice among:

- **(a) Re-forfeit the credit immediately on arrival against a terminal
  Grant** — post `Dr player_bonus / Cr promo_liability` again on the
  newly-arrived amount, per ADR 0032 §5's existing forfeiture shape.
- **(b) Route it to `player_cash`** as a mechanical settlement of an
  already-earned entitlement — mirroring the `postWin`/`postRollback` "a
  rollback is itself a correction" precedent ADR 0034 §2 already applies to
  the self-exclusion case.
- **(c) Hold it in a manual-review/staff queue** pending a per-Grant human
  decision, rather than auto-resolving either way.

**Human decision required.** This document does not choose among (a)/(b)/
(c) — it is a genuine bonus-terms/product judgment call, exactly as ADR
0034 §2 already flagged it, and not an architecture decision `bonus-engine`
or any other specialist makes unilaterally. Whichever option a human/
product decision selects must be expressed as an explicit state-machine
transition in §1.2, with its own Progress-trail entry and reason code
(§10) covering both trigger paths identically — never silently inherited
from either precedent without that confirmation, and never resolved
differently depending on which of the two paths produced it.

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
7. **Human decision required**: what happens when a settlement or void
   credit arrives against a Grant that has already gone terminal (§5's
   cross-reference note, above) — re-forfeit, route to `player_cash`, or
   hold for manual review. One open question with two trigger paths (an
   already-fully-satisfied wagering requirement post-self-exclusion, ADR
   0034 §2; and a sportsbook locked stake settling/voiding after its Grant
   went terminal, ADR 0038 §15/ledger-accounting-model.md §6.3) — not two
   separate questions. Flagged for `bonus-engine`/product, not resolved by
   this document.

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
3a. **Rule B2 (extended) mirror generator in `internal/ledger`, then
   removal of HR-9's fail-closed guard** — Go code, not a migration;
   `ledger-finance`-owned (a third dependency on this slice's critical
   path, alongside (1) and (2)), for which `bonus-engine` files a
   dependency request rather than authoring it directly. HR-9
   (`ledger-accounting-model.md` §6.5.7) rejects every posting against
   `player_locked_bonus` (required) and `player_bonus` (recommended)
   until **both** `bonus_expense` (1) and this generator exist — the
   guard's removal is a conjunctive precondition on both, not on (1)'s
   migration alone, and it lands with the generator, not earlier.
   Sequenced after (3) — which the conversion write path also needs — and
   before (4)-(7): every one of those four tables' write path posts, or
   will post, against `player_bonus` (the `activated` → `Dr
   promo_liability · Cr player_bonus` posting (6)'s Grant table exists to
   support), so none of them can be exercised end to end while HR-9's
   guard is still up. This item was missing from this list; `bonus-engine`
   adds it here per `architect`'s Stage 4H-B0-R7 independent validation
   (`ledger-accounting-model.md` §6.6.16, V-16).
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

## Bonus Dependency Contract Freeze (Stage 4H-B0-R6)

Status: **documentation-freeze only, `NOT IMPLEMENTED`.** Issued per Stage
4H-B0-R6's Workstream F directive ("Bonus dependency contract freeze").
This section does not design, redesign, or implement anything — it
collects, verbatim where a concrete signature/invariant already exists in
an approved document, the exact contract Bonus Engine's eventual Stage
4H-B1 implementers must build against for each of the ten named
dependencies, so that stage does not re-derive or silently vary any of
them. No schema, no Go code, no migration is authorized by this section.
Every citation below is traceable to an already-approved document; where
no approved document gives a clear enough answer, that is named in
"Genuine gaps" below as an open item requiring a future ADR, not decided
here.

### 1. Asset Registry

Bonus needs exactly three facts about an asset, and must obtain them the
same way every other domain does — by lookup, never by hardcoding a
decimal count or an asset list (ADR 0037 §A.1, restating ADR 0007/0021's
standing rule):

- **Existence** — the asset's `code` row exists in the registry (`assets`,
  migrations 0003/0006, ADR 0037 Part A layer 1).
- **`decimal_exponent`** — looked up from the registry, never assumed
  (0–18, `CHECK`-constrained, ADR 0037 §A.4). Every bonus amount (grant
  face value, wagering-requirement target, per-game contribution split) is
  computed in that asset's own minor units against this value — the same
  discipline `06-wallet-ledger-architecture.md`'s Money representation
  section already requires of every domain.
- **`active`** (layer 2) — whether the asset currently functions at all.
  Doc 10 §1.2's flagged gap (an asset suspended mid-Grant via ADR 0037
  §C.5.3's deliberately single-actor, no-dual-control suspend path) is
  restated here, not re-litigated: this document's existing open item
  stands unchanged.

**How Bonus looks these up**: for a plain metadata read (exponent, display
name) needed to *compute* an amount, Bonus reads the Asset Registry the
same way `wallet`/`ledger` already do today — this is not an "eligibility
decision" and does not require `AssetAuthorization`. For any question of
the form "is this asset *allowed* for this operation" (deposit/withdrawal/
wagering/settlement/conversion/reporting), Bonus MUST NOT read
`assets`/`tenant_jurisdiction_configs`/the eligibility tables directly —
that is `AssetAuthorization`'s exclusive job (§2 below), per ADR 0037
§C.2's "one canonical authorization concept — not five reimplementations"
rule. A Bonus code path that queries those tables directly to make its own
allow/deny call is a `code-reviewer` blocking finding, identically to any
other domain (ADR 0037 §C.2).

### 2. AssetAuthorization

**Frozen signature** (ADR 0037 §C.2 — architecture-only; no
`internal/assetregistry` package or `AssetAuthorization` Go type exists in
this repository as of this stage, confirmed by direct search. **Cite the
ADR text verbatim below and re-verify it against whatever Workstream A
actually commits before Stage 4H-B1 starts** — see Genuine gaps item 2):

```go
func (a AssetAuthorization) CheckEligibility(
    ctx context.Context,
    tenant TenantID,
    brand BrandID,          // may be zero-value where an operation is not brand-scoped
    jurisdiction JurisdictionID,
    asset AssetCode,
    operation Operation,    // deposit | withdrawal | wagering | settlement
                            // | conversion | reporting
) (eligible bool, reason ReasonCode, err error)
```

Binding rules Bonus must follow, all restated from ADR 0037 §C.2, not
re-derived:

- `CheckEligibility` walks the full layer chain (existence → active →
  platform-authorized → tenant-authorized → brand-authorized →
  jurisdiction-authorized → operation-eligible), short-circuiting at the
  first failing layer, and returns a specific, distinguishable
  `ReasonCode` — the same "specific sentinel, never a generic denial"
  convention `rg.Decision`/`risk.RiskDecision` already use.
- **A non-nil `err` is always `eligible = false`, no exception, no
  fallback to a previously-known-good answer** — identical to `risk`'s own
  fail-closed error contract (ADR 0031 §1/§25), restated for this
  boundary by ADR 0037 §C.2.
- Absence of any configuration row at any layer is read as ineligible/deny,
  never as "no restriction configured, therefore permitted" (ADR 0037
  §C.1's fail-closed default).
- `AssetAuthorization` is the only component permitted to read the
  underlying tables for an eligibility decision — Bonus is one of the
  named downstream domains in ADR 0037 §C.2's list ("wallet, payments,
  sportsbook, casino, bonus, FX/Conversion, retail") that must call this
  function rather than reimplement any part of the layer chain.

### 3. Risk

**Frozen signature** (ADR 0031 line 35, already real code — this one, and
only this one of the three central decision points, has a committed Go
signature to cite verbatim):

```go
func Evaluate(ctx context.Context, tx pgx.Tx, req RiskRequest) (RiskDecision, error)
```

**Four-outcome contract**: `RiskDecision.Outcome` is `ALLOW` / `REVIEW` /
`DENY`, and a non-nil `error` is treated as the fourth outcome — an
unconditional `DENY` at every call site, no bonus-specific softening (ADR
0031 §1/§25, doc 10 §4). Priority order is fixed: `DENY` (hard or
configurable) > `REVIEW` > `ALLOW` (ADR 0031 §6/§8).

**Three checkpoints, exactly as doc 10 §4 already states and ADR 0031
§15a-ii resolves**:

1. **Grant (`issued`)** and **Activation (`issued`→`activated`)** both call
   `Evaluate` with `Operation: OperationBonusGrant` — already a real Go
   constant, accepted by migration 0041. Activation is re-evaluated fresh,
   never assumed covered by the grant-time decision, because activation —
   not grant — is the point funds actually enter the wallet.
2. **Completion/conversion (`completed`→`converted`)** requires a
   **distinct** `Operation`, `bonus_conversion` — documented in full (ADR
   0031 §15a-ii, §16) but **NOT STARTED as of the last verification (Stage
   4H-B0-R3): zero of the six required extension-process steps are
   complete.** This is a real, named, outstanding dependency (Genuine gaps
   item 4), not a design gap — the contract itself is fully specified.
3. Fail-closed behavior differs by checkpoint, and this is binding, not
   optional: at grant/activation, `DENY`/`REVIEW`/error blocks the
   transition outright (no compliance queue exists yet to route a
   `REVIEW` to, so it fails closed like `DENY` — ADR 0031 §17). **At
   conversion, `DENY`/`REVIEW`/error leaves the Grant in `completed`**
   (non-terminal, retryable after review) — it never forfeits (ADR 0031
   §15a-ii, doc 10 §5's own specialist-review correction). Forfeiting a
   fully-wagered-through balance on a conversion-time Risk denial would
   destroy an already-earned entitlement, which neither `internal/risk`
   nor `internal/rg`'s own precedent permits.

**Campaign-budget-cap boundary — restated so it is never re-litigated**:
per ADR 0031 §15d/§17, **campaign-level (cross-player) budget caps are NOT
a Risk dependency and must never become one.** They belong to the Bonus
Engine's own Campaign object. The non-negotiable guard: a campaign budget
counter must never be keyed by player — the moment it is, it is a limit
engine under another name and must be a `risk_rules` row instead. Doc 10
§1.1 already flags that this enforcement mechanism itself is
`NOT IMPLEMENTED` and has no assigned owner yet (Genuine gaps item 7) —
that gap is not closed here, only its boundary (outside `risk.Evaluate`)
is confirmed frozen.

**First-slice `LimitKind` restriction, confirmed by `risk` (ADR 0031
§15b/§16)**: `min_amount`/`max_amount` with `TimeWindow: transaction` apply
to a bonus grant with no change whatsoever. `cumulative_amount` on
`bonus_grant` is storable but fails closed
(`ErrUnsupportedCumulativeOperation`) until `ledger-finance` adds a
`bonus_grant` entry to `operationLedgerTransactionTypes` and its rollback
counterpart to `operationLedgerRollbackTypes`. Bonus frequency/velocity
(`count` `LimitKind`) is not configurable at all — no fake approximation
via `cumulative_amount` is permitted (ADR 0031 §4/§15c).

### 4. RG

**Frozen signature** (ADR 0034 §1, already real code):

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

**Ordering (binding, cited not re-derived)**: `rg.EvaluateEligibility`
first, short-circuiting on denial; `risk.Evaluate` second — the fixed
composition order ADR 0031 §1 established and ADR 0034 §1 explicitly does
not revise, mirroring `internal/casino/orchestrator.go`'s existing
`postBet` precedent exactly. Both calls happen in the **same transaction**
as the Grant's own state-changing effect, before that effect commits — no
bonus-specific carve-out.

**Consequence at each checkpoint**: a `Decision.Allowed == false` at
**grant or activation** time blocks that transition outright and is
recorded as a `cancelled` Progress entry carrying the RG decision's own
`Code` as the reason — never silently skipped (doc 10 §5). A
`Decision.Allowed == false` at **conversion** time leaves the Grant in
`completed` (non-terminal, retryable) — it is **never** `forfeited` or
`cancelled` on RG denial, mirroring Risk's identical conversion-time rule
above and ADR 0034 §2's explicit "already-committed effects are never
retroactively reversed or clawed back" position (doc 10 §5's corrected
rule).

**Mid-lifecycle self-exclusion is prospective, not retroactive** — the
`internal/casino` precedent, adopted by ADR 0034 §2 without modification:
self-exclusion blocks every *future* RG-gated transition; it does not
retroactively void wagering progress or forfeit an already-satisfied
entitlement.

### 5. Wallet/Ledger

**Posting boundary (absolute, restated from doc 10 §6 and
CLAUDE.md)**: Bonus Engine computes *what happened* — split instructions
and lifecycle events. `internal/ledger` posts it and enforces every
invariant. Bonus Engine never posts a ledger entry itself and never
maintains a shadow/side-table balance that isn't reconstructable from
ledger entries.

**Account types (ADR 0032 §2, verbatim)**:

- **`promo_liability`** — house-level, per `(tenant_id, asset_code)`,
  `wallet_id IS NULL`. The platform's counter-side for bonus funds in
  circulation. Debited when bonus value enters a player's bonus balance,
  credited when it leaves. Credit-positive `signed_balance` is negative.
- **`bonus_expense`** — the twelfth account type, house-level, per
  `(tenant_id, asset_code)`, `wallet_id IS NULL`, debit-normal. The
  operator's *recognized* promotional cost.

**Invariant B1 (bonus mirror), verbatim**:

> For every `(tenant_id, asset_code)`: `signed(promo_liability) +
> Σ signed(bonus-denominated player accounts) == 0` at every instant, with
> no tolerance band.

**Rule B2 (mirror rule), verbatim**:

> Every `LedgerEntry` against a `player_bonus` account is accompanied, in
> the **same `LedgerTransaction`**, by an entry of the opposite direction
> and equal amount against `promo_liability` for the same `(tenant_id,
> asset_code)`.

Rule B2 admits no exception by transaction type — it binds
`manual_adjustment`, `bonus_reversal`, casino/sportsbook postings and any
future type identically (ADR 0032 §2).

**Binding lifecycle event → posting map** (ADR 0032 §3.1, the single
authoritative answer — Bonus Engine's own §6 lifecycle events map onto it
exactly, never a Bonus-invented variant):

| Bonus Engine lifecycle event | Ledger effect | `transaction_type` |
|---|---|---|
| `granted` (`issued`) | None — a decision, not a movement | — |
| `activated` (funds enter `player_bonus`) | Dr `promo_liability` · Cr `player_bonus` | `bonus_grant` |
| `completed` | None — eligibility, not a movement | — |
| `converted` | Dr `player_bonus` · Cr `player_cash` · Dr `bonus_expense` · Cr `promo_liability` | `bonus_conversion` |
| `expired` / `cancelled` (forfeiture) | Dr `player_bonus` · Cr `promo_liability` on the outstanding balance — no `bonus_expense` recognized or reversed | `bonus_forfeiture` |
| `reversed` | Compensating entries per ADR 0032 §7 (not restated here — cite that section directly when building) | `bonus_reversal` |

A direct cash reward (no wagering requirement) is **never** modeled as a
zero-wagering `player_bonus` grant plus an immediate conversion — it is
two entries only, `Dr bonus_expense` · `Cr player_cash`, and
`promo_liability`/Invariant B1 are not involved at all (ADR 0032 §3).

### 6. Activity/Event taxonomy

Bonus subscribes only to canonical event `type`s from
`22-canonical-activity-event-taxonomy.md`'s table, never to a
domain-internal channel (doc 22's Consumer contract, items 1–4). Every
canonical event carries, at minimum, the frozen envelope fields Bonus must
read: `event_id`, `type`, `source`, `tenant_id`, `brand_id`,
`person_id`/`player_account_id`, `occurred_at`, `recorded_at`,
`is_real_money`, `funding_source` (monetary events only), `correlation_id`,
`reverses_ref` (reversal events only), `operation_ref`/`provider_ref`,
`asset_code`/`amount_minor_units` (monetary events only),
`idempotency_key`, `schema_version`, `payload` (doc 22, envelope table).

**Binding dedupe rule**: Bonus MUST dedupe on `idempotency_key`, never on
`event_id` — the two are deliberately distinct (`event_id` is unique per
*publish*, `idempotency_key` is unique per *business fact*), and deduping
on the wrong one is a double-award bug already found and corrected in this
taxonomy's sibling documents (doc 22).

Events never themselves authorize a payout — Bonus still calls
`risk.Evaluate`/`rg.EvaluateEligibility` synchronously at the moment of any
money-affecting action; an event is a progression/re-evaluation trigger
only (doc 22, Consumer contract item 3).

### 7. `rounding_rules`

**Frozen from ADR 0021, Stage 4H-B0-R3 DECISION RECORDED, verbatim
positions**:

- **DS-1** = round-half-up (ties away from zero).
- **DS-2** = round once, at the final monetary boundary, with every
  intermediate value carried at full `NUMERIC` precision — never an
  implicit database cast, never a bare `ROUND()` call, never a
  language/library default.
- **DS-3** = one platform-wide deterministic rounding rule by default,
  with room for a future per-asset/per-jurisdiction override without
  redesign, not exercised today.

**Exact algorithm**: round half away from zero — `result = sign(x) ×
floor(|x| + 0.5)` — via one named, shared function operating in minor-unit
terms with no hardcoded scale.

**Storage**: an immutable, append-only `rounding_rules` reference table
(one row per composite Q1+Q2 version); the applied identifier is
denormalized onto `ledger_transactions.rounding_rule_id` at post time,
immutable thereafter, and the upstream computation row (the Grant/Offer)
should store the same identifier alongside its own inputs.

**Per-bonus-type application, binding, not re-derivable**: for
Deposit-match/Reload/Cashback, round `min(exact_amount × rate_%, cap)`
**once**, after both the percentage multiply and the cap comparison — the
cap is part of computing what is actually granted. The
**wagering-requirement target** (`bonus_amount × multiplier`) is a
comparison threshold that gates a lifecycle-state transition and is
**never itself posted to the ledger** (ADR 0032 §3.1: "conversion
eligibility is a decision, not a movement") — DS-2's "final monetary
boundary" language does not apply to it in the posting sense, though the
same deterministic `NUMERIC` discipline still applies. **Per-game
contribution weighting** (`stake × contribution_%`) **is** monetary — it
determines the actual cash/bonus split instruction posted for a wagering
event — and DS-2's boundary for it is the split-instruction computation
itself, rounded once there, capped by the Grant's remaining bonus balance.

**Cashback residual, confirmed consequence**: no remainder-accumulation
mechanism exists anywhere in the architecture. Each cashback calculation
rounds independently and immediately, round-half-up, with no
accumulation; the discarded sub-minor-unit fraction is not owed to the
player and never becomes a ledger fact.

### 8. Conversion boundary

**Frozen constraint, stated as a boundary, not merely an observation**:
**Bonus accounting is always same-asset. Bonus Engine's grant, wagering,
and conversion computations never invoke a `ConversionOperation`.** This
is grounded in doc 27 §26.7's confirmed finding ("bonus computations
operate on wallets already denominated in already-registered assets;
nothing about bonus grant/wagering/conversion math depends on whether the
asset list is open or closed, or on whether an FX boundary exists") and
restated unchanged by ADR 0037's own Impact Assessment ("Bonus Stage
4H-B1: none... ADR 0021's rounding decision remains as recorded, reused by
reference, never reopened").

Concretely: a Grant's reward, its wagering contribution, and its payout
are always denominated and settled in the single asset the Grant's own
wallet already uses. **A future feature that would grant or convert value
across two different assets (e.g. "match a EUR deposit with a BTC-
denominated bonus") is a new architecture decision requiring its own ADR
and human sign-off — it is not something this document, ADR 0037, or the
first-slice scope plan already permits by silence.** No Bonus Engine code
path may call `ConversionOperation` without that decision being made
first.

### 9. `player_locked` origin — PROVISIONAL, not frozen

**Explicitly not settled.** Per `ledger-accounting-model.md` §6.3, Shape A
(splitting `player_locked` into `player_locked_cash`/`player_locked_bonus`
via an additive `account_type` CHECK widening) and Rule B2 extended
(`BONUS_SET = {player_bonus, player_locked_bonus}` for a given
`(tenant_id, asset_code)`; the mirror pair fires on value crossing that
set's boundary, never on an internal `player_bonus ↔ player_locked_bonus`
transfer) were **approved in shape** by all three independent Wave-2
reviewers (`sportsbook`, `architect`, `bonus-engine` itself) at Stage
4H-B0-R5. But `ledger-accounting-model.md` §6.3.5.1 states plainly: **"Still
requires human approval before migration,"** and several sub-pieces
(the C-win proportional-payout rule, C-cashout) remain explicit, unresolved
`OPEN QUESTION`s with no review at all.

**Therefore**: Bonus Engine's dependency on `player_locked_cash`/
`player_locked_bonus` — relevant the moment any locked-stake product
(sportsbook or otherwise) needs to track wagering contribution from a
bonus-funded, currently-locked stake — is **provisional, not frozen**,
pending Workstream C's continuing implementation-ADR review this same
stage (4H-B0-R6) and eventual human approval. A future Bonus Engine build
must re-check this section's status before assuming the split exists as
described.

The related, still-open **Human decision required** item (doc 10 §1.2's
flagged gap / §5's cross-reference note: what happens when a settlement or
void credit, or a platform-caused inability to complete, arrives against
an already-terminal Grant) is carried forward unchanged — not resolved
here, not to be silently assumed by a future implementer.

### 10. External provider-native bonus coexistence

**`fulfillment_owner`** (ADR 0033 §1): every Campaign/promotional context
carries an explicit, queryable `fulfillment_owner` — `platform` |
`external_provider:<provider_id>` — resolved from tenant-owned
promotion-campaign configuration, **never** inferred by an adapter or
guessed from provider capability at grant time.

**Two-independent-bonus-sources model, degenerating to one for in-house**
(ADR 0033 §3, Stage 4H-B0-R4 correction): for an external sportsbook
provider, two independent bonus sources can exist on the same player at
the same time (our own Bonus Engine, and the provider's own native bonus
engine). For the platform's own in-house sportsbook engine, there is
exactly **one** bonus source — `fulfillment_owner` is always `platform`
and no `external_reward_grant` row is ever created for that activity. This
is a correct, safe degeneration of the coexistence model, not a defect.

**Ledger treatment, frozen** (ADR 0032 §6(c), doc 10 §3.2's corrected
text): a Grant's lifecycle events carry an explicit fulfillment-destination
flag, `into_platform_wallet` vs `inside_provider`, declared at Offer/
reward-type configuration time — a reward type that does not declare it is
rejected at configuration time, never guessed at posting time.
`into_platform_wallet` Grants (internal, or provider-funded-but-platform-
fulfilled) post through `ledger-finance`'s ordinary posting logic
unchanged. `inside_provider` Grants post **zero** ledger entries — Bonus
Engine still records the Grant/Progress trail (reporting, RG/limit
visibility, player-support answerability) but emits no lifecycle event to
the ledger boundary at all; reconciliation for these is the memo/audit
stream (`reconciliation-model.md` §2.10), never a ledger-vs-ledger
comparison.

## Bonus must never build

Every prohibition below is a restatement of an already-approved boundary,
not a new rule invented by this section:

- **A parallel wallet or shadow balance** — because a Grant's financial
  state must be reconstructable from ledger entries alone; a
  Bonus-Engine-owned balance that isn't reconciled against
  `internal/ledger` is exactly the "side table nobody can reconcile"
  CLAUDE.md and doc 10 §6 both forbid outright.
- **A parallel ledger** — because only `wallet`/`ledger` may write ledger
  tables (`02-domain-and-service-boundaries.md`), and `SUM(DEBITS) ==
  SUM(CREDITS)` is a platform-wide invariant, not a per-subsystem one — a
  second ledger would fork the one thing the whole platform's
  reconciliation model depends on being singular.
- **A parallel risk/limit engine** — because `internal/risk` is "one
  reusable engine, never a separate limit engine per product" (ADR 0031's
  standing principle, restated by doc 10 §4's own hard rule: "MUST NOT
  build its own limit engine, cap table, or velocity-threshold concept");
  two independent, unreconciled policy engines governing the same player
  would silently diverge over time with no mechanism to detect it.
- **A parallel RG/self-exclusion engine** — because RG/AML/KYC are a
  single compliance subsystem behind vendor-agnostic interfaces, with
  enforcement as platform code, not duplicated per domain (CLAUDE.md's
  Compliance section; ADR 0034 §1: "Bonus/Gamification code MUST NOT read
  `player_restrictions` directly... must not invent its own restriction
  table or enum"); a second self-exclusion concept could disagree with the
  authoritative one and let an excluded player receive value.
- **A parallel asset registry** — because "adding a new asset must never
  require touching `internal/wallet` or `internal/ledger` code" and every
  downstream domain must call the one canonical
  `AssetAuthorization.CheckEligibility` rather than reimplement any layer
  of its eligibility chain (ADR 0037 §A.1/§C.2); a Bonus-owned asset
  concept would drift from the platform's own layered
  existence/active/platform/tenant/brand/jurisdiction/operation
  authorization and could let a bonus be denominated in, or paid out in,
  an asset the platform itself has deactivated or never authorized.

## Genuine gaps found (named, not resolved here)

None of these is decided by this document. Each requires a future ADR or
an explicit human/architect decision before Stage 4H-B1 implementation
reaches the affected surface — inventing an answer here would violate this
stage's own "freeze what exists, don't design what doesn't" scope:

1. **No document names which `AssetAuthorization.CheckEligibility`
   `Operation` value a bonus grant/activation/conversion should pass.**
   ADR 0037's six operation values (`deposit`/`withdrawal`/`wagering`/
   `settlement`/`conversion`/`reporting`) do not include anything named
   "bonus," and no document maps a Bonus lifecycle transition onto one of
   them. A plausible candidate is `wagering` (bonus value is only ever
   usable for wagering), but this is not decided anywhere and must not be
   assumed by a future implementer without an explicit decision.
2. **No `internal/assetregistry` package or `AssetAuthorization` Go type
   exists in this repository as of this stage** (confirmed by direct
   search of `internal/`). The §2 signature above is cited from ADR 0037
   §C.2 as architecture only; whoever implements Workstream A's Asset
   Registry work must be the source of truth for the final committed
   signature, and Bonus's Stage 4H-B1 implementers must re-verify §2
   above against that final code before relying on it.
3. **Doc 10 §1.3's five named event-bus inputs
   (`player.registered`/`deposit.settled`/`round.settled`/`bet.settled`/
   `session.started`) are not formally reconciled against doc 22's actual
   canonical, namespaced taxonomy** (`identity.person.registered`/
   `payments.deposit.settled`/`casino.bet.settled`/`casino.win.settled`/
   `casino.launch.started`). Doc 22 has no `round.settled` type at all —
   settlement is split into separate bet/win events — and doc 10 already
   flags this as an open assumption (its own "Open questions" item 4). A
   future stage must confirm the exact `type` strings before wiring any
   real subscription.
4. **`bonus_conversion`'s Risk `Operation` value is fully specified but
   not started** (ADR 0031 §16, zero of six steps complete as of the last
   verification, Stage 4H-B0-R3). Not a specification gap — a real,
   outstanding, scoped dependency that must close before §3's conversion
   checkpoint can be wired.
5. **The `player_locked` origin split (Shape A) is approved-in-shape but
   not human-approved**, with named unresolved sub-questions (C-win's
   proportional-payout/anti-structuring control, C-cashout). Bonus's
   dependency on it (§9) is provisional until Workstream C's review this
   stage closes and a human approves the migration.
6. **The Grant-state-machine gap for a settlement/void credit (or a
   platform-caused inability to complete) arriving against an
   already-terminal Grant** (doc 10 §1.2/§5) is an explicit **Human
   decision required** item with three named candidate resolutions
   (re-forfeit / route to `player_cash` / manual-review queue), none
   selected. Must not be silently resolved by a future implementer.
   **Confirmed by `bonus-engine`'s Stage 4H-B0-R6 Workstream C phase-1
   validation** (`ledger-accounting-model.md` §6.4.11, answering V-4): this
   gap gates **bonus-only** sportsbook funding directly (cases B/E/G/I of
   §6.4), not only a future mixed-funding case — a wholly bonus-funded,
   currently-locked stake is exposed to the identical Grant-lifecycle/
   bet-settlement timing mismatch. Named there as gate **G-2**.
7. **Campaign-level budget-cap enforcement has no designed mechanism or
   owner** (doc 10 §1.1's flagged P2). Confirmed out of `risk.Evaluate`'s
   scope (§3 above), but nothing else names where or how it would be
   enforced. Needs its own design before any Campaign budget cap can
   actually be enforced.
8. **`bonus_expense`'s statutory/reporting presentation** (P&L expense
   line vs. contra-revenue) is an explicit `OPEN DECISION` in ADR 0032 §2.
   Does not block Bonus's own build, but must not be silently assumed by
   whoever eventually builds bonus financial reporting.
9. **Wagering-progress does not net a stake-reversal credit against its
   own lock-time debit (new P1, found by `ledger-accounting-model.md` §6.4,
   confirmed by `bonus-engine` at §6.4.11, answering V-1).** ADR 0032 §0's
   binding progress definition ("a derived read over ledger entries that
   debited `player_bonus`") has no netting rule for a later
   `sportsbook_void`/`sportsbook_rollback`-of-a-lock credit (or, by the
   identical root cause, a casino round-rollback credit, ADR 0032 §7) that
   returns the same value with the underlying bet nullified — a
   player-reachable, no-downside wagering-progress-farming vector, not
   limited to sportsbook. Two things are required, neither built or fully
   designed here: (i) a corrected progress-read query that nets **only** a
   credit correlated to the reversal of the *lock* itself (via
   `correlation_id` plus `transaction_type`/`reverses_transaction_id`,
   precisely scoped in `ledger-accounting-model.md` §6.4.11 so a win
   payout or a settlement correction is never wrongly netted), and (ii) a
   new Progress-trail trigger point — doc 10 §1.3's transition table has no
   row today for "previously-counted progress reversed because its bet
   voided/rolled back," which this document's own testing mandate (the
   Progress trail must explain every state change to a disputing player)
   requires. Named gate **G-3**; blocks bonus-funded sportsbook placement
   (cases B/E/G/I) exactly as G-2 does, and does not block cash-only
   migration `0048` work, which touches no bonus account.

Owner of this section: `bonus-engine`. Nothing in this section authorizes
writing `internal/bonus`, a migration, or a test.

## Terminal-Grant Technical Contract (Stage 4H-B0-R7, Workstream C)

Status: **design-only, `NOT IMPLEMENTED`.** Issued per Stage 4H-B0-R7
("FINAL FINANCIAL/BONUS IMPLEMENTATION GATE"), Workstream C. This section
extends §1.2's Grant-state table and §1.3's transition table with the
exact state-machine mechanics needed so that, once the "Human decision
required" item (§5's cross-reference note, gate **G-2**, restated
precisely at §T.13 below) is resolved by a human/product decision, an
implementer has an unambiguous mechanism to build — no schema, no Go code,
no migration, and **the human decision itself is not made here.** This
section also closes, at the technical-contract level only, one half of
"Genuine gaps found" item 1 (which `AssetAuthorization.Operation` value a
bonus checkpoint passes remains undecided — that is not resolved here —
but exactly when `AssetAuthorization.CheckEligibility` is called, in what
order relative to RG/Risk, and what a denial does to the Grant state
machine, is fully specified below, so Genuine gap item 1's eventual
resolution is a one-value substitution, not a re-design).

### T.1 Composition order for Bonus Engine's three-way live gate

Doc10 §5 already fixes, as binding architecture, "RG first, Risk second,
RG's denial short-circuits before Risk ever runs" at every Bonus Engine
enforcement point. That relative order is **not revised here.** What this
document adds is where `AssetAuthorization.CheckEligibility` (ADR 0037
§C.2) sits relative to that fixed pair, which no prior document decides —
ADR 0031's own review found this an open, platform-wide gap (item (f):
"[e]very future enforcement point calls `rg.EvaluateEligibility`,
`AssetAuthorization.CheckEligibility` and `risk.Evaluate` in the same
transaction, so the mapping is load-bearing... flagged for `architect`").

**This document's own composition decision, scoped to Bonus Engine's call
sites only, not a platform-wide resolution of ADR 0031 item (f):**

```
AssetAuthorization.CheckEligibility  →  rg.EvaluateEligibility  →  risk.Evaluate
```

Justification: `AssetAuthorization`'s own internal layer chain is ordered
"cheapest/most platform-global checks first" (ADR 0037 §A.3) precisely
because a check that does not depend on which player is involved should
resolve before a check that does. The same reasoning applies one level up
the call stack: whether this **asset** is even a legitimate instrument for
this **operation**, in this tenant/jurisdiction, is a pure
configuration-level fact — it requires no player-row lookup at all for
layers 1–3, is the cheapest of the three checks, and is entirely
independent of which player's Grant is being evaluated. It therefore
belongs before RG's player-specific compliance gate, which itself already
sits before Risk's player-specific exposure gate. This ordering changes
nothing about doc10 §5's frozen RG-before-Risk relationship; it only
prepends a third, more-global gate. If `architect` later fixes a different
platform-wide order for ADR 0031 item (f), Bonus Engine's call sites
conform to that resolution instead — this is a placeholder composition
decision for Bonus Engine's own domain, not a claim of authority over the
platform-wide question.

**All three calls happen inside the same database transaction as the
Grant's own state-changing effect, before that effect commits** — no
exception, mirroring doc10 §4/§5's existing "same transaction, before
commit" rule for RG/Risk, now extended to include `AssetAuthorization`.

### T.2 Grant creation — what is fixed vs. what is evaluated later

At the instant a Grant row is created (`(none) → issued`), the following
facts are **captured once and denormalized onto the Grant row, immutable
for the Grant's entire lifetime**:

- `asset_code` — the wallet asset the Grant will be denominated in. Never
  re-chosen, never re-derived; a Grant is never re-denominated (ADR 0032
  §8: bonus accounting is always same-asset; a cross-asset bonus is "a new
  architecture decision requiring its own ADR and human sign-off," not
  something this contract permits by silence).
- `decimal_exponent` — looked up **once**, from the Asset Registry's
  existence layer (layer 1, a metadata read, not an eligibility decision —
  doc10's "Bonus Dependency Contract Freeze" §1 already draws this
  distinction), and frozen on the Grant row. This is safe to freeze,
  not merely convenient: ADR 0037 §C.5.4 makes `decimal_exponent`
  immutable at the asset-registry level itself ("no operation... may ever
  change these, at any layer, for any reason") once any ledger entry can
  reference the asset, so a live re-lookup at a later checkpoint would, by
  construction, only ever reproduce the same value — freezing it instead
  buys auditability (ADR 0032 §9: "the bonus amount must be exactly
  recomputable from the stored inputs") without giving up any safety a
  live re-check could add.
- The **Offer version reference** — the entire immutable rule set: the
  Eligibility/Reward/Wagering/Payout/Abuse-control axes (§2), the
  forfeiture rules, the wagering-contribution-per-category table, and the
  payout ordering rules. Doc10 §1.1 already states a Grant is "always
  issued against one immutable Offer version, never a 'live' reference
  that could change underneath an already-issued Grant" — this contract
  restates that this extends to every rule *content* field, not only the
  Offer row's identity.
- `funding_source`/`funding provider_id` where applicable (ADR 0032 §6:
  "fixed on the grant record at grant time and is immutable thereafter").
- `fulfillment_destination` (`into_platform_wallet` / `inside_provider`,
  doc10 §3.2, ADR 0032 §6(c)) — declared at Offer/reward-type
  configuration time, inherited unchanged by every Grant issued under it.
- The idempotency key / `trigger_reference` (doc10 §9).
- A **denormalized snapshot** of the jurisdiction/licensing-mode context
  and the Offer's own eligibility-axis facts (segment, country, deposit
  method, first-deposit-only, VIP tier) **as evaluated at that instant** —
  recorded so the Progress trail can show *why* `issued` was granted, but
  this snapshot is a historical record, not a value any later checkpoint
  re-reads to make a new decision (see §T.4).

Evaluated **at** creation but explicitly **not** frozen for reuse
elsewhere: the RG/Risk decision that gated `issued` itself. Per doc10 §4,
"[a]ctivation is re-evaluated fresh, never assumed covered by the
grant-time decision" — the `issued`-time RG/Risk pass authorizes only the
`issued` transition, nothing downstream.

**Not checked at creation at all**: `AssetAuthorization.CheckEligibility`
for any operation. `issued` is "a decision, not a movement" (ADR 0032
§3.1 — no ledger effect), and per T.1's "gate value-moving events, not
decisions" principle (§T.5), a Grant decision that has not yet moved any
value into a wallet has nothing for an authorization gate to protect. Only
the layer-1 existence lookup (for the exponent, above) occurs at this
point.

### T.3 Grant activation

At `issued → activated` (and identically at `issued`+`activated`
collapsing into one operation for a no-opt-in, deposit-triggered Offer,
per ADR 0032 §3.1's "one posting, not two" rule), the following are
checked **fresh, live, in T.1's order, in the same transaction as the
`bonus_grant` posting**:

1. `AssetAuthorization.CheckEligibility(tenant, brand, jurisdiction,
   Grant.asset_code, operation = <candidate: `wagering`, pending Genuine
   Gap 1's resolution>)` — this is the point bonus value first becomes
   real, wallet-resident, wagering-relevant value (ADR 0032 §3.1: the
   `activated` event is what produces the sole `bonus_grant` posting per
   Grant). A hard denial here (`eligible = false`, or `err != nil`, which
   is always treated as ineligible per ADR 0037 §C.2) **blocks the
   `activated` transition outright.**
2. `rg.EvaluateEligibility` — unchanged from doc10 §5's existing rule.
3. `risk.Evaluate(Operation: OperationBonusGrant)` — unchanged from doc10
   §4's existing rule.

**Consequence of a denial at any of the three** is identical, mirroring
the existing RG/Risk-at-activation rule exactly, now extended to
`AssetAuthorization`: the `activated` transition does not occur, and the
Grant is recorded as `cancelled` via a Progress entry carrying the
denying check's own reason code (`AssetAuthorization`'s `ReasonCode`,
`rg.Decision.Code`, or the Risk rule's own code — whichever fired) — never
silently skipped, never a fourth, uninstructed outcome. This is a
deliberate, low-risk extension of an already-frozen pattern (three checks
instead of two, same consequence shape), not a new design.

### T.4 Ongoing eligibility — snapshot once, or repeated per event?

**Both — for different classes of fact, and conflating them is exactly the
mistake the stage directive warns against.**

- The **Offer's own eligibility-axis configuration** (segment, country,
  deposit method, first-deposit-only, min/max, VIP tier, opt-in) is a
  **snapshot taken once**, at `issued`, against the player's facts as of
  that instant, against the frozen Offer version (§T.2). It is never
  re-evaluated. A Grant already `issued` is not retroactively un-issued
  because the player's segment, country, or VIP tier later changes — this
  mirrors casino/sportsbook's own treatment of an already-placed bet's
  terms.
- **RG status, Risk exposure, and Asset Authorization** are **not** part of
  that eligibility-axis snapshot. They are safety/compliance/instrument-
  legality gates, and are **repeated live** at every subsequent
  value-moving checkpoint (activation, reward credit, conversion — §T.3,
  §T.8, §T.7/T.9's conversion path) regardless of what the original
  `issued`-time snapshot recorded. This is not new: doc10 §4/§5 already
  establish this for RG/Risk ("[a]ctivation is re-evaluated fresh"); this
  contract only adds `AssetAuthorization` to that same live-repeated set
  (§T.11's table makes this the explicit, general rule).

**Jurisdiction eligibility specifically bifurcates into both categories**,
which is worth stating explicitly because it is the fact most likely to be
miscategorized: whether *the Offer* is configured to be available in a
jurisdiction (doc10 §7's "Offer-level eligibility configuration") is
snapshotted once at `issued`; whether *the asset* is authorized for that
jurisdiction (`AssetAuthorization` layer 6) and whether a jurisdiction-
scoped Risk hard-limit currently fires (doc10 §4/§7) are both re-evaluated
live, fresh, at every subsequent checkpoint.

### T.5 Asset deactivation mid-Grant (`assets.active = false`, layer 2)

**Neither "freeze" (a new Grant state), nor an automatic "force-forfeit,"
nor "continue fully unaffected."** The correct mechanical answer, derived
from already-frozen architecture rather than invented here:

- Doc10 §1.2's own flagged gap already establishes, as binding text, that
  **neither `forfeited` nor `cancelled` "accurately represents" this
  case**, and explicitly leaves "whether a platform-caused inability to
  complete should write off the balance at all... [as] a `bonus-engine` +
  `ledger-finance` design question, filed as an open item, not decided by
  this review." This contract does not walk that back by inventing an
  automatic-forfeiture path on deactivation, nor does it invent a new
  Grant *state* (e.g. `frozen`) unilaterally — adding a lifecycle state is
  exactly the kind of cross-cutting redesign CLAUDE.md reserves for
  `architect` + `ledger-finance` agreement, not a state-machine detail this
  dispatch may settle alone.
- **The mechanism instead**: the Grant's `status` field is **not
  transitioned** by asset deactivation. Every subsequent event that would
  otherwise move value **into** a player-facing balance in that asset —
  a fresh `activation` (moot, already activated), a **reward credit**
  event (§T.8; cashback settlement-job credit; a win settlement crediting
  `player_bonus`, §T.7), or a **conversion** attempt (§T.7) — is
  individually blocked at its own live `AssetAuthorization.CheckEligibility`
  gate (T.1/T.3's mechanism), each denial recorded as its own Progress
  entry with its own reason code (`AssetInactive`, from layer 2). The
  Grant remains wherever it was (`activated`/`in_progress`/`completed`).
- **Value-reducing/write-down transitions are never gated by
  `AssetAuthorization` at all** (see §T.5.1 below) — so the Grant's
  *existing* terminal paths keep working normally: its own time-limit
  clock still runs and can fire ordinary `expired` (§T.9); staff can still
  `cancel` it (§T.10); a `reversed` compensating entry can still post. None
  of these three require the deactivated asset to authorize anything new —
  they only extinguish exposure that already exists.
- **Net effect**: an asset-deactivated, still-`activated`/`in_progress`
  Grant is functionally stuck (every value-creating event denied,
  individually and honestly logged) until one of: (a) the asset is
  reactivated, at which point the Grant resumes exactly where it left off
  — nothing was lost, because nothing was force-transitioned; (b) its own
  natural `expired` transition fires; or (c) staff/player-driven
  `cancelled` fires. This is deliberately the least-destructive mechanism
  consistent with doc10's own already-stated objection to prematurely
  extinguishing a player's balance "through no fault of their own."

**T.5.1 — the value-creating/value-reducing asymmetry, stated as a general
rule** (used throughout this contract, and justified once here rather than
repeated per section):

`AssetAuthorization.CheckEligibility` gates every Bonus Engine transition
that **creates** a new bonus-denominated balance or **moves value into a
less-restricted, player-facing balance** in that asset: `activated`'s
`bonus_grant` posting, a reward-credit event, a win-settlement credit into
`player_bonus`, and `converted`'s `bonus_conversion` posting. It **never**
gates a transition that **extinguishes or reduces** the player's bonus
exposure: `expired`/`cancelled`-from-activated forfeiture (`Dr player_bonus
/ Cr promo_liability`), a void/rollback credit that merely **restores** a
previously-locked stake to its origin account (a reversal, not new value —
ADR 0032 §7's "restores... with no special-case code"), or a `reversed`
compensating transaction. This mirrors, one layer up, the exact asymmetry
ADR 0037 §C.5.3 already establishes for the Asset Registry's own
administrative actions ("[d]ual control is required for any operation that
brings a new fact into existence or flips a gate on... deliberately NOT
required for the reverse direction... [t]urning something off is the
fail-closed direction"). Applying the identical asymmetry here means a
deactivated/unauthorized asset can never *block* the platform from writing
down its own contingent liability — if it could, `promo_liability` and
`player_bonus` could be left stuck, unreconciled, for as long as an
incident-response deactivation lasted, which would itself become a new,
unintended reconciliation gap.

### T.6 Asset authorization removal mid-Grant (narrower than deactivation)

**Mechanically identical to §T.5, for a structural reason worth stating
explicitly rather than re-deriving per scenario**: `AssetAuthorization.
CheckEligibility` collapses ADR 0037 §A.3's full seven-layer chain
(existence → active → platform-authorized → tenant-authorized →
brand-authorized → jurisdiction-authorized → operation-eligible) into a
single `(eligible bool, reason ReasonCode, err error)` answer,
short-circuiting at the first failing layer. Whether the failure is a full
deactivation (layer 2), a revoked tenant authorization (layer 4), a
revoked brand authorization (layer 5), a jurisdiction ban (layer 6), or —
the narrowest case named in the task — the asset becoming ineligible for
the specific operation Bonus Engine passes (layer 7, e.g. the candidate
`wagering` value from Genuine Gap 1 being revoked specifically for that
asset), Bonus Engine's own call site receives the **same shape of answer**
and therefore needs exactly **one** handling path, not one per layer. This
is precisely the benefit ADR 0037 §C.2 names as the reason for a single
canonical `AssetAuthorization` service ("one canonical authorization
concept — not five reimplementations"): Bonus Engine's state machine does
not vary its mechanism by which of the seven layers failed. What *does*
vary, and is preserved in full, is the **specific `ReasonCode`** recorded
verbatim on the Progress entry — so a disputing player (or a future
auditor) can see precisely *which* authorization layer changed underneath
their Grant, even though the mechanical consequence (§T.5's blocked-event
mechanism) is identical regardless.

### T.7 Settlement — the G-2 decision point, modeled precisely

This is the exact mechanism the human decision (§5's cross-reference note)
governs. It is modeled here at the level of "what data exists, what are
the three candidate machine actions" — no option is selected.

**Trigger condition** (either of the two paths doc10 §5 already names as
the same question, plus the composed path this contract adds in §T.5/T.9):
a settlement (`round.settled`/`bet.settled` WIN) or a void/rollback event
arrives, correlated (via `correlation_id`, ADR 0038 §3) to an earlier
lock-time debit against `player_bonus`/`player_locked_bonus` under Grant
`G`, and — read live, `FOR UPDATE`, inside the same transaction as the
settlement posting, using the identical `(tenant_id, grant_id)` advisory
lock doc10 §9 already specifies for Grant-completion races —
`G.status ∈ {expired, cancelled, forfeited}` at that instant, **for any
reason** (natural time-limit expiry, staff/player cancellation, a
wagering-rule breach on a *different* bet, RG/self-exclusion-driven
forfeiture, or — per §T.5 — an asset-deactivation-driven expiry that fired
while this specific stake was still locked). A rare fourth entry path
exists in principle (a race where `G` reaches `converted` with this stake
still in flight) and is covered by the identical mechanism below; it is
not treated as a distinct case.

**Data available at the decision instant** (all read inside the same
transaction, before the credit posts):
- `G.status`, `G.terminal_reason_code`, `G.terminal_at` (why and when `G`
  went terminal).
- The inbound event's own kind (WIN settlement vs. VOID vs. ROLLBACK) and
  amount `X`, in `G.asset_code`'s frozen `decimal_exponent` (§T.2).
- The `correlation_id` linking this event to the original lock transaction,
  and (for a rollback) `reverses_transaction_id` pointing at that specific
  lock — the exact discriminator `ledger-accounting-model.md` §6.4.11's V-1
  fix already specifies must be used, so this decision point and the G-3
  netting fix key off the same correlation, never a looser "any credit to
  `player_bonus` for this Grant" match.
- `G`'s Progress trail up to and including the terminal transition (for
  the audit narrative any of the three actions below must produce).

**The exact three candidate machine actions** (mutually exclusive, exactly
one is selected by whichever action the human decision names):

- **ACTION_REFORFEIT**: post the inbound credit to `player_bonus` exactly
  as the non-terminal case would (the ordinary WIN/void posting shape,
  unchanged), **then**, in the same database transaction, post a second
  `bonus_forfeiture` transaction (`Dr player_bonus X / Cr promo_liability
  X`, ADR 0032 §5's existing shape) for the identical amount `X`, both
  carrying the same `correlation_id`, the forfeiture leg carrying a
  distinguishing `reason_code` (e.g. `terminal_grant_reforfeit`) and a
  reference to the triggering settlement/void event. `G.status` does not
  change (it is already terminal). A Progress entry is appended recording
  both transaction ids.
- **ACTION_ROUTE_TO_CASH**: substitute the credit's destination account at
  posting time — `Dr [stake-origin account] X / Cr player_cash X` instead
  of `Cr player_bonus X` — with no `promo_liability`/`bonus_expense` legs
  at all, since the value never re-enters a bonus-denominated account
  (mirroring ADR 0032 §3's direct-cash-reward shape, not the bonus-grant
  shape). This requires the settlement/void posting layer (owned by
  `ledger-finance`/`casino`/`sportsbook`, not Bonus Engine) to consult
  Grant status before choosing a destination account for this specific
  credit — a new call-back into Bonus Engine's Grant-status read that does
  not exist today and is the one piece of new plumbing this option
  requires, regardless of which action is ultimately chosen; recording
  that dependency here so it is not discovered mid-implementation. A
  distinguishing `reason_code` (e.g. `terminal_grant_cash_route`) is
  carried on the transaction. `G.status` does not change.
- **ACTION_HOLD_FOR_REVIEW**: the credit is not posted to either
  `player_bonus` or `player_cash` at this instant. It is held (a
  suspense/holding posting, or the underlying settlement posting is itself
  deferred — the exact holding mechanism is a `ledger-finance` design
  question this contract does not resolve, flagged as a sub-dependency of
  this option specifically) and a manual-review queue entry is created
  referencing `G`, the amount, the `correlation_id`, and both event
  references (the lock and the triggering settlement/void). `G.status`
  does not change. Staff later resolves the queue entry by choosing
  **either** ACTION_REFORFEIT or ACTION_ROUTE_TO_CASH manually — the held
  amount is then posted per that choice, carrying the staff actor id, a
  mandatory reason code, and (per CLAUDE.md's four-eyes rule, if the amount
  exceeds the configured threshold) a second approver.

**Every one of the three actions appends a Progress entry to `G` even
though `G` is already terminal** — this is not a new mechanism; §1.2
already establishes that a `reversed` transition can be appended after any
terminal state without editing the prior terminal record, and this is the
identical append-only pattern applied to a new trigger reason.

**Distinct from, and not to be confused with, an `AssetAuthorization`
denial on a WIN-settlement credit against a *non-terminal* Grant.** If the
asset itself is currently deactivated/unauthorized (§T.5) at the instant a
WIN settlement would credit `player_bonus` under a still-`activated`/
`in_progress` Grant, that denial is not G-2 (the Grant is not terminal) —
but it is also not constructible as a Bonus-Engine-isolated case: the WIN
settlement's cash-funded and bonus-funded portions post in one atomic
transaction owned by `casino`/`sportsbook`, and that whole transaction is
already gated by `casino`/`sportsbook`'s own `AssetAuthorization` check on
the `settlement`/`wagering` operation for that asset generally — an
asset-wide deactivation blocks the entire settlement, not a
bonus-funded portion of it in isolation. This is `casino`/`sportsbook`'s own
posting-boundary concern, not a fourth silent option this contract needs
to invent inside Bonus Engine's domain.

### T.8 Reward credit

**Evaluated live, at credit time, never carried forward from grant time**:
the three-way gate in T.1's order (`AssetAuthorization` →
`rg.EvaluateEligibility` → `risk.Evaluate`), exactly as at activation
(§T.3). This applies uniformly to every reward-credit-shaped event: the
primary `bonus_grant` posting at `activated` (§T.3), and — separately, at
its own instant, not inherited from whatever passed when the Offer's
cashback window opened — the cashback settlement job's own credit at
window close. A cashback settlement job is, for this purpose, an
`activated`-shaped event requiring its own fresh three-way gate, not a
mechanical continuation of a check performed when the window started.

**Carried forward from grant time, never re-derived**: the reward
*amount's* computation inputs — the frozen Offer version's rate/cap/
formula and rounding rule (§T.2; ADR 0032 §9's "exactly recomputable from
stored inputs"), and the frozen `decimal_exponent`. The amount is
recomputed from these stored, immutable inputs for reconciliation, but the
inputs themselves are never re-looked-up live.

### T.9 Expiry

**Two genuinely different things, not one mechanism wearing two names —
confirmed explicitly, as the task requires:**

- **Ordinary expiry** (the Offer's time limit elapses with no stake
  currently locked): this is the existing, already-fully-specified ADR
  0032 §5 mechanism (`Dr player_bonus / Cr promo_liability` on the
  outstanding, unlocked balance, clock-triggered via `clock_timestamp()`
  per §2's cashback note, generalized to every time-limited Offer). Per
  §T.5.1's asymmetry rule, this posting is **never gated by
  `AssetAuthorization`** — it is a pure write-down, and gating it would let
  an unrelated asset-deactivation incident indefinitely block the
  platform's own ability to extinguish a contingent liability it no longer
  owes anything against. **This needs no new design; it already exists.**
- **Expiry while a stake is locked**: the expiry posting can only write off
  what currently sits in `player_bonus` at that instant — a locked stake
  sits in `player_locked`/`player_locked_bonus` (provisional, doc10
  Dependency Contract Freeze §9), not `player_bonus`, so it is untouched by
  the expiry posting. The Grant goes `expired` while that stake remains
  outstanding. **This is not a fourth case** — it is one of the named
  trigger paths into §T.7's G-2 decision point ("for any reason —
  expiry, cancellation, forfeiture — not only self-exclusion," doc10 §5's
  own words), reached the moment that locked stake's settlement or
  void/rollback later arrives.

### T.10 Cancellation

**Symmetric to §T.9, not a new mechanism.** A `cancelled`-from-`activated`/
`in_progress` transition posts the identical `bonus_forfeiture` shape (ADR
0032 §3.1: "Dr player_bonus / Cr promo_liability on the outstanding
balance — identical shape to §5, no `bonus_expense` recognized or
reversed"), never gated by `AssetAuthorization` for the same §T.5.1
write-down reasoning, and requires no RG/Risk/AssetAuthorization check of
its own (cancellation only ever reduces exposure — there is nothing for a
value-creating gate to protect). If a stake is locked at the moment of
cancellation, that stake's eventual settlement/void again lands against an
already-terminal (`cancelled`) Grant — again §T.7's G-2 decision point, one
more instance of the same trigger set, not a separate mechanism.
`cancelled`-from-`issued` (before any funds ever entered a wallet) has no
ledger dependency at all and is unaffected by any of the above.

### T.11 Immutable-vs-live-evaluated table (governing table, stage directive requirement)

| Fact | Captured how | Justification |
|---|---|---|
| Asset identity (`asset_code`) | **Immutable at grant creation**, never re-evaluated | No mechanism re-denominates a Grant; ADR 0032 §8 forbids cross-asset bonus math without a new ADR |
| `decimal_exponent` | **Immutable**, looked up once (layer-1 metadata read) at creation, frozen on the Grant row, reused for every downstream computation | ADR 0037 §C.5.4 makes the registry's own value immutable post-creation, so freezing loses no safety while buying ADR 0032 §9's recomputability requirement |
| Offer version content (eligibility axis, wagering multiplier, per-category contribution, forfeiture rules, payout ordering) | **Immutable**, bound to the one Offer version the Grant references | Doc10 §1.1: a Grant is issued against one immutable Offer version, never a live reference |
| Offer-level jurisdiction/segment/country/VIP-tier eligibility (the eligibility *axis*) | **Snapshot once**, at `issued`, against the player's facts at that instant | A Grant is not retroactively un-issued when a player's segment/country/tier later changes (§T.4) |
| Funding attribution (`funding_source`, provider) | **Immutable** at grant time | ADR 0032 §6: "fixed on the grant record at grant time and is immutable thereafter" |
| Fulfillment destination flag | **Immutable**, set at Offer/reward-type configuration, inherited by the Grant | ADR 0032 §6(c): "a reward type that does not declare it is rejected at configuration time" |
| RG status (self-exclusion, restrictions) | **Re-evaluated live** at every value-moving checkpoint (activation, reward credit, conversion) | Doc10 §5: "[a]ctivation is re-evaluated fresh, never assumed covered by the grant-time decision"; mid-lifecycle self-exclusion is prospective, never retroactive |
| Risk exposure (`risk.Evaluate`) | **Re-evaluated live** at the same three checkpoints | Doc10 §4; ADR 0031 §15a-ii |
| Asset authorization status (all 7 `AssetAuthorization` layers) | **Re-evaluated live** at every value-moving checkpoint (activation, reward credit, conversion) — **never** frozen from grant time | This document's own extension (§T.3/§T.5/§T.8); freezing this is exactly what would let a since-revoked authorization be silently bypassed, which the stage directive forbids outright |
| Jurisdiction-scoped Risk hard-limit on `bonus_grant` | **Re-evaluated live**, folded into the `risk.Evaluate` calls above | Doc10 §7 |
| Wagering-contribution rules (per-game/category/provider %) | **Immutable**, part of the frozen Offer version | Same reasoning as Offer version content, row 3 |
| Forfeiture rules (excluded games, max-bet-while-wagering, time limit) | **Immutable**, part of the frozen Offer version | Same reasoning as row 3 |
| Grant's own current lifecycle `status` | **Live, read `FOR UPDATE` at every checkpoint that might act on it**, never assumed from a prior read | §T.7's G-2 mechanism depends entirely on reading current status inside the same transaction as the competing settlement/void event, under the existing `(tenant_id, grant_id)` advisory lock (doc10 §9) |

**Why getting this wrong breaks the platform in exactly the two directions
the stage directive names**: if RG/Risk/`AssetAuthorization` were frozen at
grant time instead of re-evaluated live, a Grant issued before a player
self-excluded, before a jurisdiction revoked an asset, or before an
authorization was pulled, could keep converting/crediting indefinitely
against rules that no longer hold — unsafe. If the Offer's own eligibility-
axis content, wagering rules, or forfeiture rules were re-evaluated live
against whatever the Campaign's *current* Offer version says, instead of
the frozen version the Grant was actually issued against, a disputing
player's Progress trail would explain a decision using rules that were not
the rules in force when that decision was made — un-auditable, and a
direct violation of doc10 §1.1's "never a live reference" rule.

### T.12 Guarantee: reward credit can never bypass live asset authorization

Every reward-credit-shaped event this contract defines (§T.3's
`activated`, §T.8's cashback/other reward credit, §T.7's win-settlement
credit into `player_bonus`, and the `converted` transition's
`bonus_conversion` posting) calls `AssetAuthorization.CheckEligibility`
**fresh, in T.1's order, in the same transaction as the posting**,
regardless of what authorization state existed at grant-creation time
(§T.11's table: this is one of the facts explicitly **not** frozen). A
hard denial (`eligible = false`, or any non-nil `err`, per ADR 0037 §C.2's
"non-nil error is always ineligible, no exception, no fallback to a
previously-known-good answer") **blocks that specific transition** — it
does not silently proceed, does not fall back to the grant-time answer,
and does not invent a fourth outcome:

- At **activation** (§T.3) and at a **cashback/other reward-credit event**
  (§T.8): denial blocks the transition outright, recorded as `cancelled`
  with the `AssetAuthorization` `ReasonCode` — identical consequence shape
  to an RG/Risk denial at the same checkpoint.
- At **conversion** (`completed → converted`): denial leaves the Grant in
  `completed` — non-terminal, retryable — **the identical, already-frozen
  rule doc10 §5 established for RG/Risk denial at conversion** ("a
  `Decision.Allowed == false` at CONVERSION time leaves the Grant in
  `completed`... it is never a `forfeited` or `cancelled` transition").
  This is **not** G-2: G-2 requires the Grant to already be terminal before
  a credit arrives, and a Grant denied at conversion is, by definition,
  still non-terminal (`completed`) at the moment of denial. Extending the
  already-frozen RG/Risk-at-conversion rule to `AssetAuthorization` is a
  consistent, low-risk generalization, not a new human decision and not a
  fourth silent option.
- At a **win-settlement credit against an already-terminal Grant** (`G`
  already `expired`/`cancelled`/`forfeited`): this **is** G-2, resolved by
  whichever of §T.7's three named actions the human decision selects —
  explicitly not a case this section resolves independently.

No path exists in this model by which a reward credit posts into, or a
bonus conversion produces, an asset that is not currently authorized for
that tenant/jurisdiction/operation at the instant of posting — every
posting-adjacent transition re-checks live, and every denial routes to
one of the two already-established non-G-2 outcomes above, or explicitly
to G-2 where the Grant is already terminal.

### T.13 The human decision, restated precisely against the modeled transitions

**Still required, still not selected here.** Restated in the identical
three-option form doc10 §5 already uses, now grounded in the exact
transition points §T.7 models:

> When a settlement (`round.settled`/`bet.settled` WIN) or a void/rollback
> event, correlated to a lock-time debit against `player_bonus`/
> `player_locked_bonus` under Grant `G`, arrives while `G.status ∈
> {expired, cancelled, forfeited}` — reached via any of: (i) `G`'s own
> natural time-limit expiry firing while this stake was still locked
> (§T.9), including where that expiry was itself caused by an
> asset-deactivation incident that left `G` unable to complete (§T.5);
> (ii) staff/player-initiated cancellation of `G` while this stake was
> still locked (§T.10); (iii) `G` being forfeited for a wagering-rule
> breach or manual-review outcome on a *different* bet under the same
> Grant; or (iv) an RG/self-exclusion-driven forfeiture — which of the
> following three machine actions (§T.7) should fire:
>
> **(a) ACTION_REFORFEIT** — post the credit normally, then immediately
> post a second `bonus_forfeiture` transaction nulling its player-facing
> effect.
>
> **(b) ACTION_ROUTE_TO_CASH** — redirect the credit's destination account
> from `player_bonus` to `player_cash` at posting time, as a mechanical
> settlement of an already-earned entitlement.
>
> **(c) ACTION_HOLD_FOR_REVIEW** — hold the credit and route it to a
> manual-review queue, where staff later selects (a) or (b) explicitly,
> under CLAUDE.md's four-eyes rule where the amount exceeds threshold.

This decision governs exactly **one** transition point in this contract —
the terminal-grant credit disposition modeled in full at §T.7 — reachable
by the four trigger paths listed above (all confirmed by
`ledger-accounting-model.md` §6.4.11's V-4 finding to gate **bonus-only**
sportsbook funding directly, not only a future mixed-funding case). It
does **not** govern, and is fully distinct from, the already-resolved
non-G-2 outcomes this contract specifies independently: RG/Risk/
`AssetAuthorization` denial at grant/activation (blocks, `cancelled`,
§T.3/§T.12), or denial at conversion on a still-non-terminal Grant (leaves
`completed`, retryable, §T.12). Whichever of (a)/(b)/(c) is selected, the
mechanism to implement it — the exact data available, the exact posting
shape, the exact Progress-trail append, and the exact idempotency/locking
discipline — already exists per §T.7 above; only the choice of action
remains open.

Owner of this section: `bonus-engine`. Nothing in this section authorizes
writing `internal/bonus`, a migration, or a test.

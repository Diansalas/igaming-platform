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

## Stage 4H-B1, Wave 1 — Core Domain Model, Canonical Mechanics Catalogue, and Governance Contract

Status: **DESIGN/CONTRACT ONLY, `NOT IMPLEMENTED`.** Issued per the Stage
4H-B1 directive §36, Wave 1 of an 8-wave gated implementation. No Go code,
no migration, no test is authorized by this section. This section does
**not** redesign anything already frozen above — §1–§10, the Bonus
Dependency Contract Freeze, and the Terminal-Grant Technical Contract
(§T.1–§T.13) all stand unchanged and are cited, not re-derived. Where this
section formalizes an object the directive names but the frozen sections
above only described informally (e.g. `GrantActivation`, `BonusReward`),
that formalization is stated explicitly as such, and is additive: it
introduces no new persistent store, no new ledger transaction type, and no
new bypass of RG/Risk/AssetAuthorization anywhere.

Terminology check performed before writing anything below, per the
directive's explicit instruction: this document already names Campaign,
Offer, Grant, and Progress (§1.1) — none is renamed. `GrantActivation`,
`BonusBalance`/`BonusAccount`, `WageringRequirement`, `WageringProgress`,
`BonusConversion`, `BonusAdjustment`, `BonusCancellation`, `BonusExpiry`,
and `BonusReward` are new names for objects the directive requires but
that were previously only described inline (e.g. inside §1.3's transition
table, §2's five configuration axes, or ledger-accounting-model.md §6.6's
contribution record) — each is defined below with an explicit statement of
whether it is a new persistent object or a named, typed view over an
already-designed one, because inventing a persistent duplicate of an
already-append-only record is exactly the "side table" CLAUDE.md and this
document's own §6 forbid.

### W0. Scope map — directive item to section

| Directive item | Section(s) |
|---|---|
| 1. Domain model (identity/audit/lifecycle template + all named objects) | W1, W2 |
| 2. Campaign versioning | W2.1 |
| 3. Offer, separate from Campaign | W2.2 |
| 4. Grant | W2.3 (cites §1.1/T.2, does not re-derive) |
| 5. Five approved bonus types, full detail | Already frozen at §2/Stage 4H-B0-R6's scope plan §1; W3 maps them onto canonical mechanics, adds nothing new to their rule content |
| 6. Full operational bonus catalogue, canonical mechanics | W3 |
| 7. Coded bonuses | W4 |
| 8. Manual and bulk assignment | W5 |
| 9. Bonus Suggestions (separate, non-financial) | W6 |
| 10. Player segmentation (reusable abstraction) | W7 |
| 11. Bonus-funded wagering / mixed-funding rejection | W8 |
| 12. Human-decision safety (G-2, `OpenBetSelfExclusionPolicy`, cashout+FD-1) | W9 |
| Forward interfaces for future reward-producing domains | W10 |
| Deferrals to parallel Wave-1 dispatches | W11 |

### W1. Common object contract

Every object named in W2 below, with no exception, carries:

- **Immutable identity**: a server-generated UUID primary key, never
  client-supplied, never reused.
- **Tenant/brand ownership**: `tenant_id` (nullable only where §8 already
  permits platform/tenant-wide scope — Campaign only; every other object
  below is always tenant+brand+player scoped, mirroring Grant's own rule,
  §8), resolved from authenticated server-side context only (CLAUDE.md).
- **Lifecycle/status**: an explicit enum, never inferred from the presence
  or absence of other fields.
- **`created_at`**: `clock_timestamp()`-sourced (never `now()` — the same
  discipline §2's cashback note and ADR 0034 §14.12 already require
  platform-wide for any time-sensitive bonus/RG computation), immutable.
- **Actor/source**: `actor_type` (system | player | staff | provider) and
  `actor_id`, never omitted even for an automated transition (`actor_type
  = system` is itself a recorded fact).
- **Audit correlation**: a `correlation_id` linking the object to the
  Grant (or Campaign/Offer/job) it belongs to, and — for any object
  produced by a mutating action — a same-transaction `audit.Record` per
  §10's existing pattern.
- **Idempotency**, where the object represents the effect of an
  externally-triggerable operation (Grant, GrantActivation,
  BonusConversion, BulkGrantJobItem, WageringProgress's contribution row):
  a DB-enforced unique constraint per §9's existing pattern, never
  check-then-insert.
- **No mutable financial history**: every object below that carries a
  monetary or lifecycle-defining fact is append-only. A correction is
  always a new row (a new Progress entry, a new BonusAdjustment, a
  compensating ledger transaction) — never an `UPDATE` of a historical
  fact. This restates CLAUDE.md's ledger rule one layer up, for the
  bonus-domain records that sit beside the ledger, not inside it.

### W2. Domain model catalogue

#### W2.1 Campaign — versioned configuration

§1.1 already establishes Campaign's shape informally. This formalizes the
versioning the directive requires without changing any of it: a
**Campaign** row is identity + scope (§8) + top-level status
(`draft`/`active`/`paused`/`ended`/`archived`, staff-actioned, audited) +
`fulfillment_owner` (§3.1). Its actual configuration content — the part
that can change over the Campaign's life — lives in append-only
**CampaignVersion** rows, one active at a time, monotonically numbered:
name/display copy, start/end window, target Segment reference +
SegmentVersion pin (W7, never an inlined criteria blob), jurisdiction/
asset/product restriction, budget cap (`NOT IMPLEMENTED` enforcement —
§1.1's already-flagged P2 gap stands unchanged, this formalization does
not close it), Risk-constraint references (rule ids/tags — Risk owns the
rule *content*, §4, Campaign only records which rules apply), the bonus
type / canonical mechanic this Campaign issues (W3), and the
terms-and-conditions text/version.

**Once any Offer version (W2.2) references a CampaignVersion, that
CampaignVersion's content is immutable forever** — a change is always a
new CampaignVersion row, never an edit, mirroring the Offer-version
immutability rule (§1.1, T.2) one level up. A Campaign's own top-level
status transition does not retroactively alter any Grant already issued
under a frozen CampaignVersion+OfferVersion pair (T.11's "why getting this
wrong breaks the platform" reasoning applies identically here).

#### W2.2 Offer — separate from Campaign, no duplicated logic

An **Offer** row: id, `campaign_id`, `campaign_version_id` (immutable
pin), status (`draft`/`active`/`retired`), and its own start/end, which
must fall within the pinned CampaignVersion's window (narrowing only,
§8). The actual rule content is the **OfferVersion**, immutable once any
Grant references it (§1.1), carrying the five axes already named in §2,
now with their field surface made explicit for Wave 1:

- **Eligibility axis**: segment reference + SegmentVersion pin (W7),
  country/jurisdiction list, deposit-method list, first-deposit-only flag,
  min/max qualifying amount, VIP-tier reference (itself a segment
  reference, W7 — never a Bonus-local enum), opt-in-required flag,
  KYC/RG level required (a reference into `identity-compliance`'s own
  levels, never a Bonus-local re-definition).
- **Reward axis** → a `BonusReward` object (W2.6).
- **Wagering axis** → a `WageringRequirement` object (W2.7).
- **Payout axis**: max-cashout amount/%, cash-first/bonus-first ordering,
  partial-release thresholds, time limit.
- **Abuse-control axis**: velocity-cap references into Risk (§4.1),
  device/payment-fingerprint-linking detector configuration (§1.4 — a
  Bonus-Engine-owned detector, not a Risk rule), manual-review routing.

Offer never computes eligibility itself; it configures the parameters T.1's
three-way gate and T.4's eligibility-axis snapshot consume. A `grant_policy`
field (`auto_issue` | `manual_approval_required` | `code_redeemed` |
`external_signal` | `manually_assigned`) selects which canonical trigger
mechanic (W3) applies — this is the one field that distinguishes, e.g., a
Coupon Offer from a Deposit-bonus Offer; nothing else about the Offer's
shape differs, which is precisely §2's already-established "the only
lifecycle novelty is the trigger" rule for Coupon, generalized.

#### W2.3 Grant

Unchanged from §1.1/T.2 — this subsection is an index, not a re-design.
The full immutable field set a Grant freezes at creation (`asset_code`,
`decimal_exponent`, the Offer-version reference denormalizing its
Campaign-version reference, `funding_source`/provider id,
`fulfillment_destination`, the idempotency/`trigger_reference`, and the
eligibility-axis snapshot) is specified in full at T.2 and governed by
T.11's immutable-vs-live table. Nothing here adds a field T.2 does not
already name.

#### W2.4 GrantActivation

**Design decision, stated explicitly: not a new persistent table.**
`GrantActivation` is the directive-required, typed, queryable name for
the Grant's `issued → activated` Progress-trail entry (§1.3, T.3) — a
projection joining that specific `bonus_progress` row (transition type
`activated`) with the resulting `bonus_grant` ledger transaction group id
(§10.1's "cross-referenceable without being the same record"). Inventing
a second, parallel table to hold activation facts that duplicate an
already-append-only Progress row is exactly the "side table" CLAUDE.md and
this document's own §6 forbid, and would create two places a disputing
player's "why was my bonus activated on this date" question could get two
different answers from.

Its field surface, as exposed to API/reporting consumers: `grant_id`,
`activated_at` (`clock_timestamp()`, immutable), `trigger` (player opt-in
/ automated event / staff action / code redemption, per §1.3), its
concrete `trigger_reference`, the three-way gate outcome consulted at
activation (`AssetAuthorization`/RG/Risk decision codes, T.3), and the
resulting `ledger_transaction_group_id`. This satisfies "immutable
identity, tenant ownership, lifecycle, timestamps, actor, audit
correlation" in full without a second source of truth.

#### W2.5 BonusAccount / BonusBalance

**Design decision: Bonus Engine owns no authoritative balance of any
kind.** `BonusBalance` is a read-model term, not a table: a query-time
projection over `wallet`/`ledger`'s own `Summary.BonusBalance` (and, once
Dependency Freeze §9's `player_locked_bonus` split is unblocked,
`LockedBonusBalance`) for a given wallet+asset. `BonusAccount`, as used in
this document, means exactly the ledger's own `player_bonus` (and,
eventually, `player_locked_bonus`) account — Bonus Engine has no
independent BonusAccount table. This is the direct, literal answer to the
directive's "must reconcile with `player_locked_bonus`/`player_bonus`, not
invent a parallel balance" requirement.

Multiple concurrent Grants can share one wallet+asset's single
`player_bonus` account (ADR 0032 §5 confirms per-grant attribution — FIFO,
last-in, or per-grant-lot — is a **Bonus Engine business rule**, not a
ledger concern). To make "this Grant's remaining bonus balance" answerable
without a maintained counter, Bonus Engine owns an append-only
**GrantLedgerAttribution** record: one row per (Grant, ledger transaction),
written in the same database transaction as the posting it attributes,
DB-unique on `(tenant_id, grant_id, ledger_transaction_id)`, carrying no
field the ledger does not already have except the Grant it belongs to
(the one fact the ledger deliberately does not carry, mirroring
`ledger-accounting-model.md` §6.6.4's justification for the WageringProgress
contribution record, applied one level up). A Grant's remaining bonus
balance is always a derived read (`Σ` over this table's attributed
entries), never a stored figure — the identical "derived, per-event,
reconciled" pattern §6.6.4 already establishes, not a new one invented
here.

#### W2.6 BonusReward

The canonical descriptor of *what is granted*, frozen inside the
OfferVersion's Reward axis: `reward_kind` (one of W3's canonical reward
mechanics), `asset_code`, `calculation` (the exact formula/cap/rate
shape), `rounding_rule_id` (Dependency Freeze §7), `fulfillment_destination`
(`into_platform_wallet` | `inside_provider`, immutable, ADR 0032 §6(c)),
and `funding_source` (`operator` | `provider:<id>`, immutable at grant
time, ADR 0032 §6). `BonusReward` never itself posts anything — Grant
issuance reads it to compute the grant amount and the §6/§3.1 lifecycle
events; it is configuration, not a movement.

#### W2.7 WageringRequirement

The Wagering axis, frozen inside the OfferVersion: multiplier (or
"already satisfied" for a no-wagering cash reward, §2), the per-game/
category/provider `contribution_weight_bp` table (integer basis points,
never float, ADR 0032 §9), max-bet-while-wagering, the excluded-games
list, and the time limit. A Cashback Offer's Wagering axis is a no-op by
design (§2) — its completion trigger is the Payout axis's window/%
instead.

#### W2.8 WageringProgress

**This is exactly `ledger-accounting-model.md` §6.6.4's contribution
record — cited, not redesigned.** One append-only row per (Grant, lock
ledger transaction): `tenant_id`, `grant_id`, `offer_version_id`,
`lock_ledger_transaction_id`, `correlation_id`, `asset_code`,
`staked_bonus_amount` (read from the posted ledger entry, never the
caller), `contribution_weight_bp` (copied immutably from the OfferVersion
at contribution time), `qualifying_scaled`, `rounding_rule_id`,
`created_at`. DB-unique on `(tenant_id, grant_id,
lock_ledger_transaction_id)`, written in the same database transaction as
the lock posting (HR-10), append-only, RLS per player-self-scope. The
table's name, package, and migration belong to `bonus-engine`'s own stage,
exactly as §6.6.4 already states.

Bonus Engine additionally owns the **`P_net`/`P_firm` derivation** —
read-only computations over this table plus `ledger_transactions`/
`ledger_entries` (§6.6.3/§6.6.5/§6.6.6), never a write and never a
maintained counter. Bonus Engine calls this derivation at every
completion/conversion checkpoint; **only `P_firm` may authorize
`completed → converted`** (invariant W1, §6.6.6), re-checked inside the
conversion's own database transaction (HR-12). Bonus Engine implements
§6.6.5's exhaustive, fail-closed transaction-type classification exactly
as specified — an unclassified transaction type excludes the contribution
from `P_firm` and raises an integrity alert; it is never treated as
nullifying or as risk-preserving by a permissive default. Bonus Engine
owns its half of reconciliation stream WP-R (§6.6.4) jointly with
`ledger-finance`.

**Progress-trail obligation, formalizing gap 9 (§6.6.1/§6.6.9 property 3)
as a Wave-1 requirement**: every ledger fact in §6.6.7's case table that
moves `q_eff` (accepted bet, void, partial void, rollback, settlement,
correction, reversal) triggers a `bonus_progress` append explaining the
movement — contribution id, the confirming/nullifying ledger transaction
id, before/after `q_eff`, and a reason code — in the same transaction as
the read/write that observes it. This closes the Progress-trail-
completeness half of gap 9; §6.6 itself already closes the ledger-
derivation half.

#### W2.9 BonusConversion

Bonus Engine's own record of a conversion decision, distinct from but
referencing the ledger's `bonus_conversion` transaction: `grant_id`,
`decided_amount` (≤ the Grant's attributed outstanding balance, W2.5,
capped by the Offer's max-cashout rule), the cash-first/bonus-first
ordering and partial-release threshold applied, the `P_firm` value read at
decision time (HR-12), the three-way gate outcome (T.12), the resulting
`ledger_transaction_id`, `decided_at`, `decided_by` (system | staff —
staff requires a reason code and §10's audit record). Exactly one
`BonusConversion` row per successful `completed → converted` transition,
keyed by §9's `(grant_id, completion_trigger_reference)`. A rejected
conversion attempt (HR-12's re-check fails, or a T.12 denial) produces
**no** `BonusConversion` row — only a Progress entry recording the
rejection reason, with the Grant left in `completed` per §5/T.12's
already-frozen rule.

#### W2.10 BonusAdjustment

A new, formalized object for the staff-driven, non-terminal modification
of an active Grant's terms that §10's audit table already names ("Manual
adjustment / administrative override") but doc 10 had not previously given
its own object shape. **Scope, stated narrowly and fail-closed**: a
`BonusAdjustment` may only ever *widen* a player's position (extend the
time limit, waive or reduce the remaining wagering-requirement target,
raise the max-cashout cap) or correct a computation error by recomputing
from the frozen OfferVersion's own stored inputs (ADR 0032 §9) — it may
never edit the OfferVersion's frozen content (T.2/T.11 stand unchanged)
and, if it has a monetary effect, it may only use one of §3.1's
already-defined lifecycle-event → posting shapes (never a new
`transaction_type` invented ad hoc; a correction touching value the player
has already consumed follows ADR 0032 §7's exact `manual_adjustment
against player_cash, never player_bonus` shape).

Every `BonusAdjustment`: mandatory reason code, staff actor id,
before/after value pair, four-eyes approval above CLAUDE.md's configured
threshold (§10's existing row, restated as an object rather than a table
footnote), a Progress entry, and — if monetary — a ledger instruction
under an existing transaction type. A change that *reduces* a player's
position (shrinks the wagering target, lowers the cap, shortens the time
limit) is out of scope for `BonusAdjustment` — that is a
`BonusCancellation`/`BonusExpiry`/forfeiture concern with its own
already-designed, reason-coded shape, never a disguised downward
adjustment.

#### W2.11 BonusCancellation

Formalizes the `cancelled`-transition Progress entry (§1.2/§1.3, §3.1) as
a named, typed view, mirroring `GrantActivation`'s pattern: `grant_id`,
`cancelled_at`, `actor` (player | staff), `reason_code` (mandatory, §10),
and whether an outstanding balance existed (which determines whether a
`bonus_forfeiture` posting accompanies it per §3.1's table — `cancelled`
from `issued` posts nothing; from `activated`/`in_progress` posts the
forfeiture shape). No new mechanism.

#### W2.12 BonusExpiry

Formalizes the `expired`-transition Progress entry: `grant_id`,
`expired_at` (`clock_timestamp()`-driven), the outstanding-balance
write-off amount, and the resulting `bonus_forfeiture` ledger transaction
id. Distinguishes T.9's two cases explicitly — "ordinary expiry" (fully
specified, no new design needed) versus "expiry while a stake is locked"
(one of G-2's trigger paths, not a fourth case) — a `BonusExpiry` record
is produced either way; the locked-stake case's later resolution is a
separate G-2 event on the now-terminal Grant (§T.7), never a second
`BonusExpiry`.

### W3. Canonical mechanics catalogue

The directive requires the full operational bonus catalogue to be modeled
as a small set of canonical, reusable mechanics — not twenty bespoke
implementations. Every catalogue item decomposes into exactly three
independent choices, each drawn from a small, closed set:

**Trigger mechanic** (what causes `(none) → issued`):

| Code | Mechanic | Already-frozen basis |
|---|---|---|
| T1 | Event-triggered | Automated rule evaluation against `deposit.settled`/`player.registered`/`session.started` (§1.3) |
| T2 | Code-redeemed | §2's Coupon row, generalized (W4) |
| T3 | Manually-assigned | §1.3's "Staff action" trigger; single/list/segment/bulk (W5) |
| T4 | External-signal-triggered | §2's general rule: mission/tournament/loyalty/referral signals Bonus Engine consumes but never computes |
| T5 | Provider-native | §3: Bonus Engine does not issue a Grant at all; `fulfillment_owner: external`, memo/audit trail only |

**Reward mechanic** (`BonusReward.reward_kind`, W2.6):

| Code | Mechanic | Already-frozen basis |
|---|---|---|
| R1 | Percentage-of-qualifying-amount-with-cap | Deposit/reload/welcome/multi-deposit match; cashback computed against net loss instead of deposit amount |
| R2 | Fixed-value grant | No-deposit, birthday/anniversary, flat goodwill/compensation, flat referral, flat mission/challenge reward |
| R3 | Free-round/free-bet grant, distinct redemption unit | §2's Free spins/Free bets rows — Grant decision internal, fulfillment via the Reward Orchestrator/casino's normalised interface, never built by Bonus Engine directly |
| R5 | Manually-assigned value | A staff-entered amount/count within Risk-enforced bounds (composes with T3) |
| R6 | External-signal value | The amount/count is supplied by the external producer's own signal; Bonus Engine records, never computes (composes with T4) |

(R4 is deliberately absent as a distinct code: "code-redeemed value" is
not a reward shape, it is trigger mechanic T2 composed with any of
R1/R2/R3 — listed here only to head off the mistake of treating "coded
bonus" as a sixth reward mechanic.)

**Completion mechanic**:

| Code | Mechanic | Already-frozen basis |
|---|---|---|
| C1 | Wagering-multiplier | §2's Wagering-bonus row; the default for anything carrying a `WageringRequirement` |
| C2 | Time-window settlement job | §2's Cashback row — completion is the window closing, not a multiplier |
| C3 | Already-satisfied / no-op | §2's Cash-reward row; ADR 0032 §3's two-entry shape, never modeled as a zero-wagering Grant plus immediate conversion |
| C4 | Externally-tracked | §3.1/§3.2 — Bonus Engine records the external engine's own reported state, never recomputes it |

**Catalogue mapping** (every item the directive names, decomposed; "in
first slice" restates, never expands, Stage 4H-B0's already-authorized
scope plan §1):

| Catalogue item | Trigger | Reward | Completion | Composes from §2 row | In first slice? |
|---|---|---|---|---|---|
| Welcome / first-deposit | T1 | R1 | C1 | Deposit bonus | Yes |
| Multi-deposit | T1 (repeated matches) | R1 | C1 | Deposit bonus (a multi-step eligibility-axis configuration, a Bonus Engine business rule — no new lifecycle concept) | Yes |
| Reload | T1 | R1 | C1 | Reload bonus | Yes |
| Cashback / loss-back | T1 (eligibility) | R1 (against net loss) | C2 | Cashback | Yes |
| No-deposit | T1 or T3 | R2 | C1 (if wagering attached) or C3 (if not) | Generic-wagering-bonus row (C1 shape, in slice) or Cash-reward row (C3 shape, **out** of slice per §2's explicit scope note) | Only the C1-shaped variant |
| Free spins / free rounds | T1 / T3 / T4 | R3 | C1 | Free spins row | **No** — fulfillment not built (§2, scope plan item 4) |
| Free bets | T1 / T3 / T4 | R3 | C1 | Free bets row | **No** — same, plus `internal/sportsbook` does not exist |
| Sportsbook bonuses | Depends on shape | R1/R2/R3 | C1 | Composes from Wagering-bonus (R1/R2) or Free-bet (R3) rows — no new mechanic | **No** — blocked on `internal/sportsbook` existing at all, a much larger gate than any Bonus Engine gap |
| Casino bonuses | Any | Any | Any | Composes from Deposit/Reload/Cashback/Wagering/Free-spin — no new mechanic | Per each composed row |
| Wagering / turnover bonuses | T1/T2/T3/T4 | R1/R2/R5/R6 | C1 | Generic wagering bonus | Yes |
| Promo codes / voucher / bonus codes | T2 | Any | Any | Coupon row (W4) | Yes |
| Manual bonuses | T3 | R2/R5 | C1 or C3 | Generic-wagering-bonus (C1, in slice) or Cash-reward (C3, **out** of slice) | Only the C1-shaped variant |
| Compensation / goodwill | T3 | R2 | C1 or C3 | Same flag as manual bonuses | Only the C1-shaped variant |
| VIP / loyalty | T4 (points/loyalty redemption signal) or T3 (manual VIP-desk grant) | R2/R5/R6 | C1/C3 | Loyalty-reward row (T4-shaped, **out** — blocked on Points/Gamification) or an ordinary manual bonus (T3-shaped, in-slice-composable) | Only the T3-shaped variant |
| Retention / reactivation | T1 (a dormancy signal — **not** one of Bonus Engine's five subscribed events) or T3 (manual) | R1/R2 | C1 | Composes from Deposit/Reload/Generic-wagering | Only the T3-shaped (manual) variant; the automated dormancy-trigger variant needs a new event producer this document does not build |
| Birthday / anniversary | T1 (a calendar/scheduled signal — likewise not one of the five subscribed events) or T3 (manual) | R2 | C1/C3 | Same flag as retention | Only the T3-shaped variant |
| Referral / affiliate | T4 (a referral-confirmation signal — does not exist yet) or T3 (manual) | R2 | C1/C3 | Symmetrical to the tournament/mission boundary already established (§2's general rule) | Only the T3-shaped variant |
| Tournament / competition rewards | T4 | R2/R6 | C1/C3 | Tournament-reward row | **No** — §2, blocked on Gamification |
| Mission / challenge rewards | T4 | R2/R6 | C1/C3 | Mission-reward row | **No** — same |
| Points / XP-triggered rewards | T4 | R2/R6 | C1/C3 | Loyalty-reward row | **No** — blocked on Points accounting, doc 24 |
| Provider-native bonuses | T5 | — (never enters our wallet) | C4 | §3/§3.2's `external`/`inside_provider` shape | **No** — requires an actual external provider relationship (scope plan item 4) |

**This mapping adds zero bonus types beyond the five already authorized
for the first slice** (Deposit, Reload, Cashback, Generic Wagering,
Coupon, per Stage 4H-B0's scope plan §1) — every catalogue row composes
from those five plus the already-named, already-deferred rows (Free
spins/bets, Cash reward, Tournament/Mission/Loyalty). Where a catalogue
item needs an event producer Bonus Engine's own five subscribed events do
not supply (dormancy signals, calendar/scheduled signals, referral
confirmation), that is named above as a genuine new dependency on a
producer this document does not build — not silently assumed available.

### W4. Coded bonuses

A "coded bonus" is trigger mechanic **T2** composed with any reward/
completion mechanic — not a parallel object. When `OfferVersion.grant_
policy = code_redeemed`, the eligibility axis carries: a code (or code-pool
reference), a validation rule (format/checksum), a per-code and/or
per-Offer `redemption_limit`, and any stacking/conflict predicate (e.g.
"excludes Campaign X" / "requires no other active Offer of type Y" for
this player) — evaluated at the same `issued`-time eligibility snapshot
(T.4), not a new mechanism.

**Enforcement of `redemption_limit`, stated honestly rather than
invented**: a *per-player* limit is enforced as an ordinary DB uniqueness
constraint on the redemption attempt (the same discipline as the already-
noted "redeemed-code uniqueness constraint... left to the implementation
stage" from Stage 4H-B0's migration-sequencing note, item 7) — not a Risk
rule, since no `count`-shaped `LimitKind` exists yet (Dependency Freeze §3).
A *global* redemption cap generalizes the already-named, already-open
"campaign-level budget cap has no enforcement mechanism or owner" gap
(Genuine Gaps item 7) from an amount ceiling to a count ceiling; this
document does not invent an enforcement mechanism for it, consistent with
that gap's own standing status.

**No bypass path, by construction, not by promise**: the code-redemption
endpoint (already named in Stage 4H-B0's scope plan §1 item 5) does
exactly three things — validate the code's format, resolve it to an Offer,
and check the redemption-limit constraint — before handing off to the
*identical* `(none) → issued` pipeline every other trigger uses:
`AssetAuthorization → RG → Risk` (T.1), the Offer eligibility-axis
snapshot (T.4), and §9's idempotency key. A code is only a different
`trigger_reference` value feeding that one existing transition row in
§1.3; it does not skip, reorder, or soften any step of it. Jurisdiction/
brand/tenant/segment restriction on a code is identical to any other
Offer-level restriction (§7/§8) — a code is not a parallel scoping
mechanism.

### W5. Manual and bulk assignment

**`BulkGrantJob`**: `id`, tenant/brand scope (always scoped, never
platform-wide, mirroring Grant, §8), `campaign_id`/`offer_version_id`,
`target` (a single `player_account_id`, an explicit list, or a Segment +
SegmentVersion reference resolved **live at run time**, W7), `requested_by`,
`requested_at`, `approval_state` (`pending_four_eyes` | `approved` |
`rejected`), `status` (`queued`/`running`/`completed`/
`partially_completed`/`failed`), and a job-level `idempotency_key` (a
resubmission of the identical job spec is a no-op against the same key,
never a second job).

**Resumability and per-player isolation**: an append-only
**`BulkGrantJobItem`** row per targeted player, each keyed
`(tenant_id, bulk_grant_job_id, player_account_id)` (DB-unique), each
independently running the **full** T.1 three-way gate for that one player
— bulk assignment is `N` individual `issued` transitions sharing one job
correlation id, never a batch-level bypass — and each recording its own
outcome (`issued` | `denied:<reason_code>` | `already_granted:<grant_id>` |
`error`) and timestamp. A crashed/resumed job re-walks its target list and
skips every player who already has a `BulkGrantJobItem` row of any
outcome: resumable and idempotent by construction, with duplicate-grant
prevention supplied by the ordinary `issued`-time idempotency key (§9),
not a bulk-specific mechanism.

**Live segment resolution, not a stale snapshot**: a segment-targeted
job's member list is resolved against the current SegmentVersion at
**job-run time**, not frozen at job-creation time — mirroring T.4's
"RG/Risk/AssetAuthorization repeated live" rule one level up, applied to
segment membership itself, so a player who has since become
RG-excluded, Risk-denied, or left the segment is never silently included
from a stale list.

**Four-eyes threshold — flagged, not invented here.** CLAUDE.md requires
four-eyes approval above a configurable threshold for manual balance
adjustments; a `BulkGrantJob` is exactly such an action at batch scale.
Per the directive's explicit instruction, the specific threshold (whether
evaluated per-item amount, aggregate job amount, or player count) is
coordinated with `security`'s parallel Wave 1 dispatch, not set
unilaterally here. **Fail-closed default until that threshold is set**:
any `BulkGrantJob` targeting more than one player, or any single-player
manual grant whose `BonusReward` amount exceeds the Campaign/tier's
existing per-transaction Risk `max_amount` ceiling (§4.1 item 2), requires
four-eyes approval before leaving `pending_four_eyes` — a conservative
placeholder, not a final policy.

**Tenant/brand isolation and auditability**: every `BulkGrantJob` and
`BulkGrantJobItem` inherits the ordinary Grant RLS pattern (§8) and writes
an `audit.Record` at creation, approval/rejection, start, completion, and
per-item outcome (§10's pattern), correlated by the job's own id, so a
single audit query answers "who authorized this batch, under what rules,
and what happened to every targeted player" without joining a mutable
status table.

### W6. Bonus Suggestions — a separate, non-financial domain

**`BonusSuggestion`** never creates a Grant, never moves money, and never
calls RG/Risk/AssetAuthorization's value-moving checkpoints — it is a
proposal artifact consumed by a human reviewer, structurally incapable of
becoming a financial fact on its own.

Lifecycle: `Generated → UnderReview → {Approved | Rejected | Edited} →
{Activated | Discarded}`.

- **Generated**: carries a mandatory `originating_rule`/`originating_signal`
  reference (a rule id, a model version, or a staff actor id for an ad hoc
  suggestion) and a `proposed_config` (an inert structured blob — not an
  Offer version until Activated).
- **UnderReview**: a reviewer is assigned or the queue entry is claimed; no
  financial commitment exists at this state.
- **Approved / Rejected / Edited**: `reviewer_id`, `decided_at`, and — for
  Edited — the specific modified fields, diffed against `proposed_config`,
  so a suggestion silently approved-with-private-edits never happens.
- **Activated**: only reachable from Approved (or Edited-then-Approved).
  Activation means exactly one thing: the reviewer's decision is submitted
  through the **ordinary, unmodified** Campaign/Offer/Grant (or
  `BulkGrantJob`, or single manual Grant) pipeline — Activation grants no
  shortcut through eligibility/RG/Risk/AssetAuthorization. The resulting
  Campaign/Offer/Grant carries an `originating_suggestion_id` back-reference
  for traceability.
- **Discarded**: terminal, no financial artifact, reason optional but
  recorded if given.

Every field the directive names is stored append-only with full audit
trail, own tenant/brand scope (never platform-wide, mirroring Grant).
**Why this is genuinely separate, not a Bonus Engine sub-object**: no
`BonusSuggestion` table carries any `player_bonus`-denominated field or
ledger reference until Activation produces one indirectly through the
ordinary Grant path — it trivially satisfies CLAUDE.md's "never track
liability only in a side table" rule because a suggestion is not liability
at all. This document does not decide whether `BonusSuggestion` lives in
`internal/bonus` or a separate package — an ordinary package-boundary call
for the implementation stage, not an architectural one, since either
choice preserves the one load-bearing property above (Activation never
bypasses the ordinary Grant pipeline).

**Explicitly out of Wave 1**: any recommendation engine that *populates*
`BonusSuggestion.Generated` rows (a scoring model, an LTV heuristic) — this
section designs the object and lifecycle a future recommendation engine
would populate, never the recommendation logic itself. Building a
generator ahead of a concrete first consumer (a staff member manually
creating a suggestion) is exactly the "generality for a hypothetical
future need" CLAUDE.md's scope test would flag.

### W7. Player segmentation — a reusable abstraction, not Bonus-owned

**Confirmed gap, checked directly**: no existing architecture document
(`02-domain-and-service-boundaries.md`, checked) currently owns a
reusable segmentation service. Every "segment" mention in this platform's
frozen documents today (this doc's §2 eligibility axis, ADR 0031's Risk
rule scoping) is a free-text field or a Risk-rule scope dimension, never a
first-class, queryable membership object.

**`ARCHITECTURAL DECISION` (scope only) / `RECOMMENDATION`, flagged for
`architect`'s cross-domain map**: Bonus Engine's Wave 1 does **not**
implement a segmentation service. It designs the minimal shape here, and
writes its own Offer eligibility axis to consume it **by reference**
(`segment_id` + `segment_version_id`, never an inlined criteria blob), so
that when a shared segmentation service is authorized elsewhere, Bonus
Engine needs a data migration to populate the reference, not a redesign.

Shape (design-only):

- **Segment**: id, tenant/brand scope, name, kind (`static` | `dynamic`),
  `created_by`/`created_at`.
- **SegmentVersion**: id, `segment_id`, `criteria` (a versioned, immutable
  AND/OR/NOT-composable predicate expression over player facts), effective
  timestamp, **immutable once referenced by any consumer** — the identical
  "frozen version, never a live reference" discipline this entire document
  already applies to Offer versions, extended to segmentation.
- **Static membership**: an explicit, staff-assigned, append-only
  addition/removal list, each entry audited — used for named lists, not
  derived criteria.
- **Dynamic membership**: never stored; evaluated live against the current
  SegmentVersion's criteria at the instant of use — mirroring T.4's split
  between a snapshotted eligibility-axis fact and a live-repeated
  compliance gate, applied one level up to segment membership itself.

**Hard boundary, restated because it is the one that matters most**:
segment criteria may reference only facts that are authoritative
elsewhere (deposit history via wallet/ledger read APIs, VIP tier via a
loyalty/CRM read API, jurisdiction via §7's canonical resolver) —
segmentation **never** becomes a second source of truth for RG/Risk/
KYC/licensing status, and segment membership **never** overrides or
bypasses a hard DENY from RG/Risk/AssetAuthorization/KYC. Concretely: a
Grant's `issued` transition still runs T.1's full three-way gate
regardless of which segment(s) qualified the Offer — segment membership
decides eligibility for an Offer's *terms*, never authorization to
*receive value*, and those remain the same two structurally distinct
steps T.4 already separates.

**Auditability**: a Grant's Progress trail records the `segment_id` +
`segment_version_id` (or static-list id) that qualified the player at
`issued` time, as part of the existing eligibility-axis snapshot (T.2) —
so a disputing player's or auditor's reconstruction shows exactly which
segment definition, at which version, produced the decision, with no
Bonus-owned copy of segmentation logic to drift from the authoritative
one.

Bonus Engine does **not** hard-code VIP/Risk/AML/KYC categories anywhere
in its own Campaign/Offer schema — every eligibility-axis dimension that
looks like "VIP tier" or "risk tier" is a segment reference, resolved
against whichever domain actually owns that classification, never a
Bonus-local enum.

### W8. Bonus-funded wagering and mixed-funding rejection

Bonus Engine never re-implements `player_cash`/`player_bonus`/
`player_locked_cash`/`player_locked_bonus` accounting — it calls the
authoritative wallet/ledger contract (`ledger-finance`'s parallel Wave 1
dispatch; §6/Dependency Freeze §5 above). Bonus Engine's own boundary here
is exactly the split-instruction computation (§6 item 1), never the
posting.

**Mixed cash+bonus funding for a single stake is rejected deterministically,
not invented around**, per the directive's own framing that this is
already HR-2-rejected, fail-closed. Concretely: Bonus Engine's
split-instruction computation for a wagering event always produces a
**single-origin** funding decision (cash-only or bonus-only), per the
Offer's Wagering-axis contribution rules and the wallet's own
already-authoritative balance-selection logic (a `wallet`-owned decision,
never reimplemented here) — never a fractional per-stake blend across the
two origins. Where a stake would need both origins to be fully funded,
Bonus Engine's instruction is a deterministic rejection (ordinary
insufficient-funds handling), never an automatic blend. The named,
not-yet-generalized configuration boundary this fixes:

> **`MixedFundingPolicy`** (config key, fails closed until set). Bonus
> Engine's own default and only currently-authorized value is
> **`REJECT_SINGLE_ORIGIN_ONLY`**: a stake exceeding the player's
> single-origin balance is rejected, never blended. No other value is
> designed, authorized, or implemented here — introducing one (e.g. any
> form of proportional cash+bonus stake splitting) is a new architecture
> decision requiring its own ADR and human sign-off, mirroring T.2's
> identical treatment of cross-asset bonus grants (ADR 0032 §8). Wave 1
> does not permit it by silence.

### W9. Human-decision safety analysis

For each of the three decisions in ADR 0039 (Human Decision Register),
per the directive's (A)/(B)/(C) framework:

**Decision 1 — `OpenBetSelfExclusionPolicy` default.**
(A) B1 does not depend on it. The first slice's five bonus types have no
sportsbook dependency (`internal/sportsbook` does not exist). (B)
Everything in Wave 1 can be built without it — no first-slice Offer/
Campaign/Grant path references sportsbook or this policy at all. Bonus
Engine's own RG-denial handling at activation/conversion (§5, Dependency
Freeze §4's "prospective, not retroactive" rule) is identical regardless
of which default is chosen. (C) Any future sportsbook-funded bonus type
needing to reason about a bonus-funded stake on self-exclusion stays
gated — but that already depends on `internal/sportsbook` existing at
all, an independently larger gate. Wave 1 invents no default here.

**Decision 2 — Terminal-Grant settlement-credit resolution (G-2).**
(A) B1 does not depend on it, independently confirmed by ADR 0039
("cash-only sportsbook wagering is unaffected... G-2... blocks
bonus-funded sportsbook wagering directly") and by Stage 4H-B0's scope
plan (sportsbook/free-bet fulfillment explicitly deferred out of the
first slice). No first-slice bet ever posts to `player_locked_bonus`. (B)
The full W2.8 WageringProgress/Model C design can be built without it —
Model C is exercised casino-only in the first slice (`P_firm == P_net`
identically, §6.6.3), a regime G-2 never reaches. (C) The Terminal-Grant
Technical Contract's §T.7 mechanism (ACTION_REFORFEIT /
ACTION_ROUTE_TO_CASH / ACTION_HOLD_FOR_REVIEW) stays fully specified and
unselected; Wave 1 builds none of the three actions and does not enable
bonus-funded locked-stake wagering of any kind until it is answered. Any
future locked-stake product must re-check Dependency Freeze §9's status
before assuming bonus-funded locking is available.

**Decision 3 — Mixed/bonus-funded sportsbook cashout policy + FD-1.**
(A) B1 does not depend on it — W8 above already rejects mixed cash+bonus
funding deterministically, and no sportsbook cashout code exists anywhere
in the platform (confirmed by ADR 0039's own "nothing is blocked, on any
timeline" finding for this decision). (B) Everything in Wave 1 can be
built without it — no first-slice object involves a cashout concept
(cashout is sportsbook-only, and sportsbook does not exist). (C) Any
future combination of bonus-funded sportsbook wagering **and** a
sportsbook cashout feature stays gated on both sub-questions (3a
proceeds-split, 3b FD-1 wagering-progress treatment) being answered
*together*, per ADR 0039's own framing — and even if cashout code were
built prematurely, W2.8's own §6.6.5 classification already fails closed
on `sportsbook_cashout` today (unclassified, excluded from `P_firm`,
integrity alert), so Wave 1's design would not silently authorize a
conversion from it regardless.

**Summary: none of the three human decisions blocks any part of this
Wave 1 domain-model design or Stage 4H-B0's already-authorized first
slice.** All three are carried forward as explicit, named boundaries that
fail closed (§T.7's three actions unselected; `MixedFundingPolicy` fixed
at `REJECT_SINGLE_ORIGIN_ONLY`; `sportsbook_cashout` unclassified) — none
is defaulted, guessed, or silently built around by this section.

### W10. Forward interfaces for future reward-producing domains (interfaces only, no implementation)

Per the directive's explicit request — Gamification, Points/XP, Missions,
Achievements, Tournaments, Leaderboards, the Reward Marketplace, and a
complete Reward Orchestrator remain fully out of scope; the two interface
points below are named contracts a future producer would implement, not
new Bonus Engine mechanism, and neither is implemented, stubbed, or given
a Go signature here:

- **`RewardTriggerSignal`** (consumed, never produced, by Bonus Engine):
  the shape a future external-signal producer (tournament ranking, mission
  completion, points redemption, referral confirmation) emits for Trigger
  mechanic T4 (W3) to react to, exactly as Bonus Engine reacts to
  `deposit.settled` today (§2's general rule). Minimal fields, drawn from
  doc 22's canonical envelope (Dependency Freeze §6): `event_id`/
  `idempotency_key`, `tenant_id`/`brand_id`/`player_account_id`,
  `producing_domain` (mission | tournament | loyalty | referral | other),
  either an `offer_reference` or a `reward_amount`/`reward_kind` (whichever
  the producing domain has already decided — Bonus Engine never
  re-derives it), and `correlation_id`. This is a naming convention over
  the subset of doc 22's taxonomy T4 consumes, not a new event-bus
  concept.
- **`ExternalRewardProvider`** (§3): unchanged by Wave 1, still an explicit
  ASSUMPTION section, still deferred to the Master Orchestrator's Reward
  Orchestrator synthesis and to an actual sportsbook provider relationship
  existing (scope plan item 4).

### W11. Deferrals to parallel Wave-1 dispatches, and open items carried forward

Recorded in one place for the orchestrator's cross-domain review — none of
these is resolved by this section:

- **`ledger-finance`**: the posting mechanics for every lifecycle event
  (§6/§3.1) and for `GrantLedgerAttribution` (W2.5) and the WageringProgress
  contribution record (W2.8) remain ledger-finance's exact SQL/Go design to
  produce, per §6.6.4's own ownership split; HR-9/HR-15 guard status
  (Dependency Freeze §9); the §T.7/G-2 posting mechanics once selected.
- **`risk`**: the `bonus_conversion` `Operation` value (still not started,
  Dependency Freeze item 4); a `count`/velocity `LimitKind` for coded-bonus
  redemption limits and campaign-level budget/redemption caps (W4, Genuine
  Gaps item 7); the specific four-eyes threshold for `BulkGrantJob` (W5).
- **`identity-compliance`**: RG/KYC nuance beyond the calling contract
  already frozen (§5/Dependency Freeze §4); jurisdiction-specific
  bonus-type bans (§7); any RG-level definition a `WageringRequirement`'s
  eligibility axis references.
- **`security`**: RBAC for the coded-bonus redemption endpoint (W4); the
  `BulkGrantJob` approval workflow and its four-eyes threshold (W5,
  coordinated, not set here); audit-trail sufficiency for
  `BonusSuggestion`/`BonusAdjustment` (W6/W2.10).
- **`architect`**: segmentation-service placement (W7) as a cross-domain
  map item; the platform-wide `AssetAuthorization.Operation`-composition-
  order question (T.1) if it resolves differently from Bonus Engine's own
  placeholder; whether `BonusSuggestion` lives in `internal/bonus` or a
  separate package (W6).
- **`sportsbook`**: the correlation point for free-bet fulfillment (§2/§3)
  and G-2/FD-1's sportsbook-specific triggers — not designed here.

**Genuinely unresolved without a human decision** (restated, not new):
G-2's three-action selection (§T.13); `OpenBetSelfExclusionPolicy`'s
platform-wide fallback default; the bonus-funded sportsbook cashout
proceeds-split (3a) and its required FD-1 companion (3b) — all three
carried forward unchanged from ADR 0039, none selected, guessed, or
defaulted by this section (W9).

Owner of this section: `bonus-engine`. Nothing in this section authorizes
writing `internal/bonus`, a migration, or a test.

## Stage 4H-B1, Wave 1.5 — Architecture Reconciliation Gate (dispatch 4HB1W15-02)

Status: **DESIGN/CONTRACT ONLY, `NOT IMPLEMENTED`.** Issued per the human-
interposed Wave 1.5 gate (`docs/governance/task-registry.md`, Stage
4H-B1 Wave 1.5), dispatch `4HB1W15-02`. No Go code, no migration, no test
is authorized by this section. It does not redesign §1–§10, the Bonus
Dependency Contract Freeze, the Terminal-Grant Technical Contract
(§T.1–§T.13), or Wave 1 (W0–W11) above — it extends them additively,
naming every addition and every correction explicitly as such, per the
same discipline Wave 1 itself used for `GrantActivation`/`BonusReward`.
**No Human Decision Register item (G-2, `OpenBetSelfExclusionPolicy`,
the cashout proceeds-split, or FD-1) is selected here** — ADR 0039 stands
untouched; §N1 below is an engineering mechanism that narrows *when* G-2
is reachable, not an answer to it.

This section runs in parallel with, and does not see, `casino`'s own
Wave 1.5 dispatch (`4HB1W15-01`, the casino-side `postWin` destination-
resolution design and its own G-2 boundary specification) or `architect`'s
Segmentation/CRM/Affiliate architecture (`4HB1W15-03`). Every place this
section depends on one of those without having seen it is named as such,
per the dispatch's own instruction, not silently assumed.

### N1. Grant terminal-state invariant — formal definition and proof (gate §A.9)

> **Status: REVISED, Stage 4H-B1 Wave 1.5 Fix Wave (dispatch `4HB1FW-01`),
> closing P0 `LF-2` and P1 `LF-11`.** `ledger-finance`'s P0 finding
> (`docs/governance/task-registry.md`) is accepted in full: N1.4 step 5 as
> originally written posted a late WIN credit to `player_bonus` using the
> ordinary settlement shape and then, in the same transaction, reforfeited
> whatever that posting freed up — economically indistinguishable from
> selecting `ACTION_REFORFEIT`, regardless that the Grant's status was
> labelled `pending_settlement` rather than `expired` at the instant of
> posting. The original §N1.8's argument that G-2's premise "does not
> occur" was, as `ledger-finance` correctly identified, a relabelling of
> the state, not a change to the money's path. **What changed, in one
> sentence**: a value-creating (WIN) credit against exposure that was
> already open when a Grant's wagering-eligibility closed is now never
> posted to `player_bonus` or `player_cash` at all until a human resolves
> G-2; it is captured in a Grant-attributed, ledger-visible holding
> representation instead. Value-reducing (loss) resolution of the
> identical exposure is unchanged — it was never the problem
> `ledger-finance` flagged and remains fully automatic. Every subsection
> below is corrected in place, named as such; nothing outside `N1` is
> edited, and this revision selects nothing from the Human Decision
> Register (the revised §N1.8 restates the non-selection argument more
> rigorously than the version it replaces). `LF-11` (the
> `FOR UPDATE`-on-`ledger_entries` claim) is fixed at N1.3/N1.4/N1.5 below,
> named individually where it occurred.

> **Addendum — Stage 4H-B1 Wave 1.5 Fix Round 2.** The Fix Wave above
> left the holding representation's exact account/schema shape open
> ("`ledger-finance`'s to finalize," N1.4.1) and this document's own
> N2.4 left the single-Grant staff-action-equivalent surface reachable
> by a machine caller with no volume-bounding precondition. Both are
> closed in this revision: N1.3/N1.4/N1.4.1/N1.9/N1.6/N1.11 adopt
> `ledger-finance`'s finalized `player_bonus_held` account and
> `bonus_held_dispositions` schema (`ledger-accounting-model.md` §7.7.2)
> and `casino`'s widened `ResolveTerminalGrantCredit` seam and still-held
> rollback transition (`08 §16.9–§16.18`) verbatim, retiring the
> two-branch split and the `bonus_g2_hold:<correlation_id>` key this
> document previously proposed; N1.12 (new) adopts `security`'s
> consolidated `SEP-1` contract (`REQ-SEP-BONUS-1`–`4`); N2.4/N2.4a (new)
> adopt `architect`'s `EconomicOperationIdentity` (doc 34) to close the
> CRM-decomposition vector this document's own Round 1 N2.4 left open.
> **G-2 itself remains unselected throughout** — every change below is to
> the technical mechanism that holds, represents, and authorizes access
> to disputed value, never to which of G-2's three answers applies.

> **Addendum — Stage 4H-B1 Wave 1.5 Fix Round 2, final closing pass.**
> `casino` closed a gap its own Phase 2 review found in this document: N1.4
> step 5a's ordinary value-reducing finalization and step 5c's
> `voided_by_rollback` transition both asserted that `G.status` "flips in
> the same transaction, no window" without ever naming the concrete call
> by which `casino`'s own transaction reaches `bonus-engine`'s
> Grant-status write. `08 §16.21` names that seam,
> `bonusengine.RecheckGrantExposure`, symmetric to the value-creating
> side's already-named `ResolveTerminalGrantCredit` (`08 §16.9`). This
> document adopts it at new §N1.4.2 and updates N1.4 step 5a, step 5c's
> `voided_by_rollback` bullet, and Scenario 6 (N1.6) to name it in place
> of the previously-asserted-but-unmechanized claim — the outcome each
> previously described is unchanged, only the mechanism that was missing
> is now named. This pass also confirms N1.4 step 5b/§N1.9 already match
> `casino`'s now-canonical, further-widened `ResolveTerminalGrantCredit`
> signature (unconditional hold-capture, `settlementLedgerTransactionID`
> in, `heldDispositionID` out) and corrects the two places (N1.4 step
> 5b(i), N1.10) that still cited the earlier, narrower signature. Finally,
> it updates N1.12's four-eyes-threshold citation to the now-ratified
> position (`architect`'s doc 34 §3.1, `security`'s self-correction,
> commit `6745e10`) in place of the superseded "unconditional" reading,
> and states LF-10's residual explicitly at N1.10 so a reader of this
> document alone is not misled into thinking that mechanism is fully
> closed. **G-2 itself remains unselected by any of this.**

#### N1.1 What this closes

This closes the engineering resolution the Orchestrator's Wave 1
reconciliation already sketched (`docs/governance/task-registry.md`,
finding 1): "*A Grant may only reach a genuinely terminal status
(expired/cancelled/forfeited-final) once its attributable locked balance
(`player_locked_bonus` tied to that Grant) reaches zero. A terminal
trigger firing while locked funds remain outstanding defers the Grant
into a pending-settlement sub-state that finalizes automatically once the
locked stake resolves (win or loss) — never crediting against an
already-terminal Grant, by construction.*" This section makes that
sketch precise enough to build from, proves it, and — because the
sketch's own wording ("locked funds," "locked stake") is sportsbook-
shaped and this document's own §T.7 already flags a `converted`-side
instance of the identical failure mode, and because `architect`'s doc 29
§4.3 independently proved the casino-only slice reaches the same failure
mode **without** any locked account at all — generalizes the sketch to
cover both shapes honestly, including the one place it does **not**
fully close.

#### N1.2 The invariant, stated formally

> **Invariant TI-1.** For every Grant `G` and every instant `t`: if
> `G.status(t) ∈ TERMINAL`, then `AOE(G, t) = ∅`.

Where:

- **`TERMINAL`** = `{expired, cancelled, forfeited, converted}`.
  **Correction to the gate directive's own framing, stated explicitly**:
  the directive names "expired/cancelled/forfeited-final." This section
  adds **`converted`** to the set, because §T.7 already documents "a rare
  fourth entry path... a race where `G` reaches `converted` with this
  stake still in flight... covered by the identical mechanism," and that
  claim was previously asserted, not built. A Grant's wagering-progress
  threshold is computed from **stake debits** (§6.6/§7.10's `P_net`/
  `P_firm`), not from settlement outcomes — so `completed → converted`
  can legitimately fire while a bonus-funded bet placed under `G` is
  still open, and that bet's later WIN/VOID/ROLLBACK credit lands against
  a Grant that has already converted, structurally the same failure
  shape as landing against an already-`expired` Grant. `reversed` is
  deliberately **excluded** from `TERMINAL` for this invariant's purpose:
  it is itself the append-only compensating mechanism §1.2 already uses
  for a fact discovered *after* terminality (an upstream reversal), not a
  forward-settlement-credit case, and nothing about TI-1 constrains it.
- **`AOE(G, t)`** ("Attributable Open Exposure") — defined in N1.3.

Equivalently, by contraposition: **a settlement or void credit can never
arrive against an already-terminal Grant, because a Grant is not
permitted to become terminal while that credit remains possible.** This
is the precise, checkable form of "never crediting against an
already-terminal Grant, by construction."

**Corollary TI-1a, added by this revision — TI-1 binds the forward-going
transition, not facts discovered afterward.** TI-1 is a claim about the
information available *at* the instant a transition into `TERMINAL` is
evaluated: the transition mechanism (N1.4) may never fire while `AOE`,
computed from every event observed as of that instant, is nonzero. It
does not, and cannot, claim that no fact bearing on `G` will ever surface
after a transition that was valid given everything knowable at the time —
N1.7 already discloses one such residual (a casino `win` callback
violating its own provider settlement-window SLA) and this revision
preserves that disclosure rather than papering over it. A fact of that
kind is handled exactly as §1.2 already handles an upstream reversal
discovered after terminality: an append-only correction on top of the
terminal record, never a retroactive claim that the earlier transition
itself violated TI-1. N1.6's Scenario 4 below proves this precisely for
the one place it matters — a late WIN.

#### N1.3 Attributable Open Exposure (AOE) — the concrete definition the gate directive asks for

`AOE(G, t)` is the union of two components, because this platform's two
provider-integration shapes (sportsbook's locked-account model and
casino's direct-post model) make "relevant money movement... capable of
affecting" concrete in two structurally different ways. Conflating them
would be exactly the mistake the gate directive's own phrasing (both
examples named side by side: "an outstanding `player_locked_bonus`
balance... an in-flight bet whose win/loss hasn't settled") warns
against.

**Component 1 — `LockedExposure(G, t)`** (sportsbook-shaped, once
Dependency Contract Freeze §9's `player_locked_bonus` split is
unblocked): the signed sum, read live (see N1.5 for the corrected
description of what actually serializes this read against a concurrent
settlement — **not** the `FOR UPDATE` clause itself, `LF-11`), of every
`player_locked_bonus` ledger entry attributable to `G` via
`GrantLedgerAttribution` (W2.5) that has not yet been nullified per
`ledger-accounting-model.md` §6.6.5/§6.6.7's own classification.
`LockedExposure(G, t) ≠ 0` means a stake funded by `G` is currently
locked, and by construction of the lock/settle/void protocol itself,
*something* (a WIN settlement, a LOSS-grading settlement, a VOID, or a
ROLLBACK) is still guaranteed to arrive and move that balance — sportsbook
settlement is exhaustive over win/loss/push per bet (`docs/decisions/
0038` confirms "settlement win/loss happy paths" as an explicit,
first-class case, not merely the win side). This component is **exact**:
it is a real, currently-posted ledger balance, cleared only by a genuine
event, never by a timeout guess.

**Component 2 — `InFlightExposure(G, t)`** (casino-shaped, no locked
account): the set of `WageringProgress` contribution rows (§W2.8)
attributable to `G` whose underlying stake-lock ledger transaction
(`casino_bet`, correlated via `roundCorrelationID`) has posted, for which
**no qualifying closing event** — a `casino_win` credit, a
`casino_rollback`, or (see N1.7) an elapsed settlement-window — has yet
been observed. `InFlightExposure(G, t) ≠ ∅` means a bonus-funded casino
stake placed under `G` could still, at some future point, cause a
`casino_win` credit to `player_bonus` under `G`.

**Why casino needs a second component at all, verified against code, not
assumed**: `internal/casino`'s `postBet` debits `player_bonus` (once
wagering split instructions post there) straight into `house_gaming` —
there is no intermediate locked account the way sportsbook's
`player_locked_bonus` provides. The moment a bonus-funded bet posts, its
stake has **already left** any account `LockedExposure` could observe.
If `AOE` were defined as `LockedExposure` alone, every bonus-funded
casino bet would show zero exposure the instant it is placed — which is
precisely the exposure this gate exists to close, per `architect`'s doc
29 §4.3 finding that G-2 is reachable in the casino-only slice with no
locked account and no sportsbook at all.

**Component 3 — `HeldDisposition(G, t)`** (added by the Fix Wave, closing
`LF-2`; **redefined, Stage 4H-B1 Wave 1.5 Fix Round 2, per
`ledger-accounting-model.md` §7.7.2.10 item 2's binding contract on this
document**: `HeldDisposition(G, t)` is a **live balance read of the
`player_bonus_held` ledger account, attributed to `G` via
`GrantLedgerAttribution`** (W2.5) — the identical
attribution-read pattern `LockedExposure` already uses, applied to a
different account — **never** "does a `bonus_held_dispositions` row exist
with `status = 'held'`." The row is a reconciled, richer projection over
that balance (name, actor, reason code, per-occurrence audit trail); the
ledger balance is the source of truth, and `ledger-finance`'s new `LF-12`
reconciliation stream (§7.7.2.8) is exactly the check that keeps the two
in agreement. This closes `LF-20` **structurally, not merely by
discipline**: `player_bonus_held` is a third, disjoint ledger account
(§7.7.2.1) — a dollar cannot be posted to it and to `player_locked_bonus`
at the same time, so `LockedExposure(G, t)` (a read of `player_locked_bonus`)
and `HeldDisposition(G, t)` (a read of `player_bonus_held`) cannot report
the same money twice by construction, not merely because this document
takes care not to double-count.

`HeldDisposition(G, t) ≠ ∅` means: a win happened, its value (the payout
`W` and, if the originating stake was locked, the released lock amount
`X` — both denormalized onto the corresponding `bonus_held_dispositions`
row, §N1.9) is sitting in `player_bonus_held` attributed to `G`, and it is
waiting on a human to answer G-2 (or on staff to apply an
already-answered G-2 to this specific occurrence) — not that anything
further will arrive from a provider for this stake. This is the
component the original proposal, and the Fix Wave's own pre-revision
text, omitted entirely, which is exactly how the pre-revision N1.4 step 5
was able to observe the underlying bet's `InFlightExposure`/
`LockedExposure` member clear (the win *arrived*) and treat that as
license to also treat `G`'s overall exposure as resolved (the win's
*disposition* was decided) — conflating "the bet is over" with "the
money's fate is decided." Once a value-creating credit is captured by the
hold-capture posting (N1.4 step 5b, `ledger-accounting-model.md` §7.7.2.2),
its originating `LockedExposure`/`InFlightExposure` member is removed
(nothing further will arrive for that specific stake — the underlying bet
truly is resolved) but is **replaced, not cleared**, by the new
`HeldDisposition` member — `AOE`'s total does not move to zero merely
because the bet resolved. Round 1's own text called this partition claim
correct but noted the concrete account/schema shape was still open; Round
2 makes it **literally, physically true**: the replacement is not a
bookkeeping fiction this document maintains by convention, it is what a
disjoint ledger account structurally forces to be true (LF-19/LF-20 are
therefore **closed**, not merely guarded against, per
`ledger-accounting-model.md` §7.7.2.1's own framing — "moot, not merely
satisfied").

`AOE(G, t) = LockedExposure(G, t) ∪ InFlightExposure(G, t) ∪
HeldDisposition(G, t)`, and `AOE(G, t) = ∅` (all three components empty)
is the precondition N1.4's mechanism enforces before `G` may become
`TERMINAL` (subject to N1.2's Corollary TI-1a for a fact that only
surfaces after a valid transition). Extending `AOE` with
`HeldDisposition` is the exact, minimal change this revision makes to
close `LF-2`: without it, the moment a WIN callback for the last open
stake arrived, `AOE` would read `∅` and the deferred disposition would
auto-finalize using whatever balance the WIN just created — precisely
the relabelled `ACTION_REFORFEIT` `ledger-finance` found. With it, `AOE`
correctly stays nonzero until the win's *disposition*, not merely its
*arrival*, is resolved. This is also the direct, non-new answer to this
revision's own conversion question (N1.10): a Grant with an outstanding
`HeldDisposition` record has `AOE ≠ ∅` by this same definition, so Path
A's existing block on `completed → converted` already covers it — no
second, parallel blocking rule is needed.

#### N1.4 The mechanism

**Revised by this fix wave to separate two concerns the pre-revision text
conflated (the fix wave's own unifying technical contract):**

> **`NewStakeEligibility(G, t) ∈ {open, closed}`.** Purely technical,
> carries **no claim** about what happens to value already at risk.
> `NewStakeEligibility(G, t) = open` whenever `G.status(t) ∈ {issued,
> activated, in_progress}`. It flips to `closed`, immediately and
> uncontestedly, the instant **any** termination trigger fires against
> `G` — natural time-limit expiry, staff/player cancellation, a
> wagering-rule-breach or manual-review forfeiture finding, or a
> conversion attempt (`completed → converted`, successful or blocked) —
> **regardless of whether `AOE(G, ·)` is zero at that instant.** No new
> stake may be authorized against `G` once `NewStakeEligibility(G, ·) =
> closed`; this is enforced at whichever domain's wagering-authorization
> checkpoint reads `G.status` before funding a stake from `player_bonus`
> (casino or sportsbook), not by N1.4's mechanism itself, and requires no
> G-2 involvement, ever — a pure eligibility gate, symmetric to every
> other live-re-evaluated gate this contract already specifies (§T.11).
> **Closing `NewStakeEligibility` is TECHNICAL in every case, including
> forfeiture**: a forfeiture-causing breach closes eligibility the instant
> it is detected, before any question about the already-open exposure's
> *disposition* is even asked. `NewStakeEligibility` closing and
> `G.status` flipping to a terminal value are **not the same event** —
> that gap is exactly what `pending_settlement` (below) exists to
> represent honestly: eligibility already closed, financial disposition
> of exposure open at that instant not yet finished.

The **financial disposition** of exposure that was open at the instant
`NewStakeEligibility` closed is where G-2 lives, and — this is the
correction this fix wave requires, `LF-2`'s exact subject — **only for
the value-creating sub-case.** Value-reducing resolution (a loss) of the
identical exposure is not G-2, creates nothing to dispute, and proceeds
immediately and technically, exactly as `casino`'s own `B(G)`/`L(G)`
split intended and as `ledger-finance` endorsed as correct; a balance
that was never at risk (free, unwagered bonus balance) at the moment of
the trigger is disposed of by the ordinary, already-authorized,
non-G-2, expiry/cancellation write-down (step 4 below) — a bonus
expiring unused being forfeited is standard, uncontested behavior, not
G-2.

Two distinct handling paths, deliberately not one, because the two
transition families already have structurally different holding
patterns available and reusing an existing one is cheaper and safer than
inventing a single mechanism to cover both:

**Path A — Conversion (`completed → converted`).** No new Grant status.
`AOE(G, t) ≠ ∅` at a conversion attempt is treated by the **identical,
already-frozen rule** §5/T.12 already specify for an RG/Risk/
`AssetAuthorization` denial at conversion: the transition does not occur,
`G` remains `completed` (non-terminal, retryable), and a Progress entry
records the block with its own reason code (`open_exposure_outstanding`,
new, alongside the existing RG/Risk/`AssetAuthorization` codes T.12
already uses at this same checkpoint). This is not a new mechanism — it
is one more entry in the set of things that can block `completed →
converted` without forfeiting or cancelling the Grant, exactly the set
T.12 already establishes. The natural re-trigger is the same event that
clears the exposure (see N1.6): every ledger event that reduces
`AOE(G, ·)` to `∅` for a Grant sitting in `completed` re-attempts the
conversion it previously blocked, in the same transaction as the event
that cleared the last unit of exposure.

**Path B — Expiry / cancellation / forfeiture.** These are transitions
**out of** `activated`/`in_progress`, where no existing "stay here,
retryable" state also communicates "this Grant's fate is decided, only
its finalization is pending." Leaving `G.status` at `activated`/
`in_progress` while its fate is already fixed would itself violate
§1.2's own standing objection to a state that "does not honestly
describe why" — a support agent or report reading `activated` would
believe the bonus is still a live, ongoing offer. This needs one new,
named status:

> **`pending_settlement`** — additive to §1.2's Grant-state table, not a
> replacement of any existing row. A Grant in `pending_settlement` has
> already had a terminal trigger fire against it (`NewStakeEligibility(G,
> ·) = closed`); its lifecycle fate (which terminal value it will
> eventually flip to) is decided; only the timing of the status flip is
> deferred, pending `AOE(G, ·)` reaching `∅` — where, per N1.3's revision,
> `AOE` reaching `∅` requires not merely that every open bet has
> *resolved*, but that every value-creating credit those bets produced
> has had its G-2 *disposition* recorded (N1.4 step 5, `HeldDisposition`).

Fields carried on `pending_settlement` (additive to the Grant row, no
new persistent object — mirroring `GrantActivation`'s "typed projection,
not a side table" discipline): `terminal_resolution` (one of `expired` /
`cancelled` / `forfeited` — never `converted`, which is Path A's
concern and cannot race with Path B: a Grant is never simultaneously
`completed` and `activated`/`in_progress`), `terminal_trigger_reason_code`
(the reason the original trigger fired — RG denial, staff cancellation,
wagering-rule breach, time-limit elapsed, asset-deactivation-driven
expiry per §T.5, etc.), `terminal_triggered_at` (`clock_timestamp()` of
the *original* trigger, immutable — this is the timestamp a disputing
player's Progress trail cites for "why," even though the status flip
happens later), and `terminal_trigger_correlation_id`.

**Mechanism, step by step, run inside `G`'s existing `(tenant_id,
grant_id)` advisory lock (doc10 §9), same transaction as the trigger's
own effect:**

1. The trigger fires exactly as §1.3/T.9/T.10 already specify (time
   limit elapsed, staff cancellation, wagering-rule breach, manual-review
   forfeiture outcome). `NewStakeEligibility(G, ·)` flips to `closed` at
   this instant — TECHNICAL, automatic, no G-2 involvement (see N1.8.1's
   exhaustive transition classification).
2. `AOE(G, ·)` is computed live, per N1.3, inside the same transaction —
   see N1.5 for the corrected description of what actually serializes
   this against a concurrent settlement (`LF-11`: not a `FOR UPDATE`
   claim on `ledger_entries`).
3. **If `AOE(G, ·) = ∅`**: proceed exactly as already specified — write
   the terminal status directly, no behavior change from §1.2/§T.9/§T.10.
   This is the ordinary case today and remains fully unmodified.
4. **If `AOE(G, ·) ≠ ∅`**: write `G.status = pending_settlement` instead,
   with the fields above populated. **Whatever portion of the write-down
   is already free right now still posts now** — per §7.7's own
   "forfeiture is explicitly frequently partial" design, the ordinary
   `bonus_forfeiture` shape posts immediately for the currently-
   unlocked, not-in-flight balance (this is not new — partial forfeiture
   already exists in the frozen design; this section only adds that the
   *status* does not flip to `forfeited`/`expired`/`cancelled` until the
   remaining exposure also clears). A Progress entry records the
   deferral, enumerating every outstanding exposure record (each open
   `WageringProgress` contribution id / each nonzero locked-balance
   attribution) so the trail is inspectable, not merely asserted.
5. **Resolution — corrected by this revision to close `LF-2`.** Every
   ledger event that could change one of `AOE`'s three components
   re-checks, in the same transaction, under the same advisory lock,
   whether `G` is `pending_settlement` (or already fully terminal, N1.6
   Scenario 4) and what **kind** of event this is. The two kinds are
   handled **differently in kind, not merely in outcome** — this is the
   exact distinction the pre-revision text collapsed, and un-collapsing
   it is this revision's entire fix:

   **5a. Value-reducing closing event** (a LOSS-grading settlement, a
   VOID, a ROLLBACK, or — casino only, N1.7 — an elapsed settlement
   window with no WIN observed) — **TECHNICAL, fires automatically, no
   G-2 involvement**, exactly as `casino`'s own `B(G)`/`L(G)` split
   intended and as `ledger-finance` endorsed as correct. Nothing is
   credited; the stake is absorbed/written off using whatever posting
   shape already applies to an ordinary loss (no new shape). The closed
   `LockedExposure`/`InFlightExposure` member is removed from
   `AOE(G, ·)`. If this brings the full three-component `AOE(G, ·)` to
   `∅`, the deferred disposition applies to whatever balance is now free
   (the ordinary §7.7 write-down, unchanged from the pre-revision text)
   and `G.status` flips `pending_settlement → terminal_resolution` **via
   `bonusengine.RecheckGrantExposure` (N1.4.2), called by `casino` in the
   same transaction as the closing posting above** — at the plain
   lock-rollback site in `postRollback` (`TriggerCasinoRollback`) and at
   `08 §16.5a(a)`'s settlement-timeout sweep
   (`TriggerCasinoSettlementTimeout`), `08 §16.21` sites 1 and 2 —
   immediately after the live Grant-status read that call site already
   performs finds `pending_settlement`. If `AOE` is still nonzero (another
   open bet, or an outstanding `HeldDisposition` record, remains), the
   recheck leaves `G` at `pending_settlement`.

   **5b. Value-creating closing event** (a WIN settlement credit
   correlated to exposure that was part of `AOE(G, ·)` at the instant
   `NewStakeEligibility(G, ·)` closed) — **this is G-2's exact scenario.
   The corrected behavior: the credit is never posted to `player_bonus`,
   `player_cash`, or any other player-accessible balance. Revised, Stage
   4H-B1 Wave 1.5 Fix Round 2, to adopt `ledger-accounting-model.md`
   §7.7.2.2's exact two-leg hold-capture posting and `08
   §16.9`/§16.14's widened `ResolveTerminalGrantCredit` seam signature
   verbatim — this document's own two variables below are renamed to
   match those documents' `W`/`X` naming so the three documents never
   again describe the identical posting under different names.** Instead:
      i. The fact that a WIN occurred is captured with its **two
         separately-tracked quantities** — the win payout `W` (the
         provider's settlement amount) and, if and only if the
         originating stake was locked, the released lock amount `X`
         (sportsbook's `net_outstanding_locked`, always; casino's too, if
         and only if `08 §16.10.1`'s locked-shape recommendation for
         bonus-funded casino bets is ever ratified — not decided or
         required here). Both are threaded, as **separate parameters,
         never combined into one `amount`**, through casino's widened
         seam `bonusengine.ResolveTerminalGrantCredit(ctx, tx, grantID,
         correlationID, creditKind, payoutAmount, releasedLockAmount,
         settlementLedgerTransactionID) (heldDispositionID, err)` (`08
         §16.9`, adopted here verbatim, including its call order — this
         document's own prior text, which omitted
         `settlementLedgerTransactionID`/`heldDispositionID` and did not
         state which side posts first, is corrected to match: `casino`
         posts the two-leg hold-capture posting (5b.ii) **itself**, via
         its own `ledger.Post` call, *before* calling this seam, which
         yields `settlementLedgerTransactionID`; this seam runs second,
         inside the same transaction, and does no posting of its own — it
         only writes the `bonus_held_dispositions` row (5b.iii) carrying
         that id, returning the row's own `id` as `heldDispositionID` for
         `casino`'s logging/observability only, never branched on).
         Neither quantity is in dispute at this step; both are preserved
         so the eventual disposition and the Progress trail have
         something correct to act on and explain. **Capture is
         unconditional**: this seam is called, and posts, for **every**
         terminal-Grant win credit, before any of G-2's three eventual
         answers is known or decided — never conditioned on
         `ACTION_HOLD_FOR_REVIEW` or any other outcome (`08 §16.9`'s Round
         2, final-closing-pass correction, adopted here; this was this
         document's own original requirement, N1.8 below, which `casino`
         was reconciling itself to, not a new constraint this document is
         adopting from `casino`).
      ii. **The hold-capture posting** — one balanced `LedgerTransaction`,
          posted in the same `ledger.Post` call as the rest of the
          settlement, never as a second transaction, exactly per
          `ledger-accounting-model.md` §7.7.2.2 / `08 §16.14`:
          - `Dr house_gaming payout_amount(W) / Cr player_bonus_held W` —
            **always** present.
          - `Dr player_locked_bonus released_lock_amount(X) / Cr
            player_bonus_held X` — present **only if** the originating
            stake was locked; **absent entirely** otherwise. There is no
            second account-type branch here (see N1.4.1's Round 2
            rewrite) — the presence or absence of this leg is a fact
            about the bet, never a design choice this document makes.

          `player_bonus_held` is the single, dedicated, Grant-attributed
          holding account (§7.7.2.1/§7.7.2.4) — never `player_bonus`,
          never `player_locked_bonus` relabeled. It is never posted to
          `player_bonus` first and then reversed — there is no such
          intermediate posting at all.
      iii. A `bonus_held_dispositions` row (N1.3 Component 3, N1.9) is
           created, **in the same database transaction as the
           hold-capture posting**, carrying `settlement_ledger_
           transaction_id` = that posting's own transaction id (never a
           separately-minted key — this is `LF-22`'s fix, N1.4.1 item 4),
           `payout_amount = W`, `released_lock_amount = X` (0 if never
           locked), `status = 'held'`. The closed `LockedExposure`/
           `InFlightExposure` member this row replaces is removed from
           `AOE` — `AOE(G, ·)` **does not** reach `∅` merely because the
           underlying bet resolved; it remains nonzero, via the balance
           this row's transaction just posted to `player_bonus_held`
           (N1.3 Component 3's redefinition), until that balance itself
           clears (5c, or `voided_by_rollback` below).
      iv. `G.status` **does not change** at this step — it stays
          `pending_settlement`, or, for N1.7's residual case, stays
          whatever terminal status it already validly reached (N1.2
          Corollary TI-1a, N1.6 Scenario 4). Nothing about this step
          selects, or even narrows, which of G-2's three answers
          applies — N1.8's revised argument proves this explicitly.
      v. **Concurrency — confirmed unaffected, adopting
         `ledger-accounting-model.md` §7.7.2.9 verbatim.** This posting
         acquires no lock participant beyond the two this section's N1.5
         already names (`(tenant_id, correlation_id)` then `(tenant_id,
         grant_id)`) — it never takes a `player_bonus` projection `FOR
         UPDATE`, because its destination is `player_bonus_held`, not
         `player_bonus`. HR-21 is not violated and needs no new
         participant for hold **creation**; HR-25 (the `bonus_held_
         dispositions` row lock) is the new, fourth participant, and it
         is required only for **resolution** and for the rollback
         transition below (N1.4 step 5c/5d, `ledger-accounting-model.md`
         §7.7.2.9).

   **5c. Resolving a `bonus_held_dispositions` row — POLICY-DEPENDENT,
   requires a human-supplied G-2 answer, never automatic. Revised, Fix
   Round 2, adopting `08 §16.16`'s resolution of this document's own
   flagged `ACTION_HOLD_FOR_REVIEW` contradiction and HR-25's resolution
   lock order (`ledger-accounting-model.md` §7.7.2.9) verbatim.**
   `casino`'s `08 §16.9` names the seam
   (`bonusengine.ResolveTerminalGrantCredit`); it operates on a *held*
   amount that has **already** been captured by step 5b's hold-capture
   posting — resolution never posts the win credit for the first time,
   it only moves an already-`player_bonus_held`-resident value onward.
   Resolution runs in its **own** later transaction, keyed by the
   `bonus_held_dispositions` row's own `id`, under **HR-25's** lock order
   (not part of the original settlement transaction, and not the
   `(correlation_id) → (grant_id)` order 5b uses): (1) `(tenant_id,
   grant_id)` advisory lock; (2) `SELECT ... FOR UPDATE` on the specific
   `bonus_held_dispositions` row (the new, fourth HR-21 participant) —
   **and nothing further, for either disposition-bearing action.** An
   earlier draft of this step took a third lock, a `player_bonus`
   projection `FOR UPDATE`, for `ACTION_REFORFEIT` specifically —
   withdrawn as false, per HR-25 corrected this pass
   (`ledger-accounting-model.md` §7.7.2.9): `ACTION_REFORFEIT` debits
   `player_bonus_held` directly, never via `player_bonus`, so there is
   nothing for such a lock to protect. Neither `ACTION_REFORFEIT` nor
   `ACTION_ROUTE_TO_CASH` ever acquires a `player_bonus` projection `FOR
   UPDATE`.
      - **`ACTION_REFORFEIT`**: `Dr player_bonus_held payout+released_
        lock_amount / Cr promo_liability` — the held amount posts
        directly from `player_bonus_held` into the ordinary §7.7
        forfeiture destination — **never via `player_bonus`**, even
        transiently. The row transitions `held → resolved_reforfeit`,
        carrying `resolved_by_actor_id`/`resolution_reason_code`
        (N1.9) and `resolution_ledger_transaction_id` = this posting's
        id. The net player-facing effect is identical to the pre-Fix-Wave
        text's outcome (the player nets zero on this credit), but it now
        happens only after an explicit human decision — never as an
        automatic, structurally-forced side effect of the win simply
        arriving. This is the exact distinction `ledger-finance`'s
        finding turns on: the *outcome* `ACTION_REFORFEIT` was never the
        defect; auto-selecting it **without a human decision, while
        calling it something else,** was.
      - **`ACTION_ROUTE_TO_CASH`**: `Dr player_bonus_held payout+released_
        lock_amount / Cr player_cash` — the held amount posts directly
        from `player_bonus_held` into `player_cash`. The row transitions
        `held → resolved_route_to_cash`, same audit fields as above.
      - **`ACTION_HOLD_FOR_REVIEW` — the contradiction this document
        flagged against casino's Round 1 text is now resolved, conforming
        to `08 §16.16`'s exact framing, stated against the correct pair
        of accounts rather than picked between.** `ACTION_HOLD_FOR_REVIEW`
        is **not** a distinct posting at resolution time, and it is
        **not** a fourth `status` value — `bonus_held_dispositions.status`
        admits exactly `held`/`resolved_reforfeit`/`resolved_route_to_
        cash`/`voided_by_rollback` (N1.9), and **`held`, persisting, IS
        `ACTION_HOLD_FOR_REVIEW`'s complete state.** The real, balanced
        posting this action is associated with **already happened**, at
        step 5b's hold-capture: that posting genuinely released the lock
        (`Dr player_locked_bonus X / Cr player_bonus_held X`, `L(G) → 0`
        exactly as every other outcome requires) **and** genuinely
        created no spendable value (neither leg ever touches `player_
        bonus`/`player_cash` — the entire value sits in `player_bonus_
        held`, unreadable by wagering/conversion/withdrawal, N1.4.1 item
        3). Both halves of this document's Round 1 text ("releases the
        lock" / "money no-op") were correct, about **different
        accounts** — `player_locked_bonus` genuinely empties;
        `player_bonus`/`player_cash` genuinely do not move — and neither
        needed to be withdrawn once stated against the right pair.
        Choosing to leave a row `held` performs no further posting at
        all; it is the absence of a resolution act, not an act of its
        own. Staff's later choice of `ACTION_REFORFEIT` or `ACTION_ROUTE_
        TO_CASH`, per §T.7's existing text, is simply an ordinary,
        later-arriving instance of one of the two bullets above.
      - **`voided_by_rollback` — new this round, adopting `ledger-
        accounting-model.md` §7.7.2.7 and `08 §16.15` verbatim; not a G-2
        answer at all.** A provider rollback naming this occurrence's
        `settlement_ledger_transaction_id` while `status = 'held'` is
        handled entirely inside `casino`'s `postRollback`, under the
        **same** HR-25 lock order this step already uses (grant, then the
        `bonus_held_dispositions` row `FOR UPDATE`): the generic
        entry-inversion (`Cr house_gaming payout_amount`, and `Cr
        player_locked_bonus released_lock_amount` **if present** —
        restoring the lock is deliberately never attempted) posts in the
        same transaction as the guarded compare-and-swap `UPDATE
        bonus_held_dispositions SET status = 'voided_by_rollback',
        resolution_ledger_transaction_id = <reversal id> WHERE id = ? AND
        status = 'held'`. **Bonus Engine's own Grant-side bookkeeping,
        the part this section owns**: this is a **TECHNICAL** transition,
        never a G-2 selection — it undoes a settlement event that never
        should have existed (symmetric to voiding any other not-yet-
        disposed ledger fact), not a disposition choice. The `Held
        Disposition` member this row represented is removed from
        `AOE(G, ·)` — **and is not replaced by a restored `LockedExposure`/
        `InFlightExposure` member.** This is deliberate, not an
        oversight: `L(G)` already closed to zero at hold-capture time
        (5b), and `ledger-accounting-model.md` §7.7.2.7 is explicit that
        reversing a win that never should have happened is "a straight
        reversal to `house_gaming`, not a resurrection of a lock" — the
        underlying bet is being treated as a fully reversed, closed fact
        (like an ordinary VOID/ROLLBACK, N1.4 step 5a), never as
        still-in-flight exposure reopened. If the underlying bet itself
        also needs a separate unwind, that is the platform's existing
        "two independent reversals" rule (`08 §16.11`), unaffected here.
      - Whichever transition clears the record (`resolved_reforfeit`,
        `resolved_route_to_cash`, or `voided_by_rollback` — `held`
        persisting under `ACTION_HOLD_FOR_REVIEW` does **not** clear it):
        if this was the **last** outstanding `AOE` component for `G`,
        `G.status` flips `pending_settlement → terminal_resolution`. For
        `resolved_reforfeit`/`resolved_route_to_cash`, that write is
        Bonus Engine's own, made directly in the same transaction as the
        resolution posting above — this is Bonus Engine's own resolution
        flow, not a cross-service call, so no seam is needed here; Bonus
        Engine already owns both the disposition write and the
        Grant-status write in that one transaction. For
        `voided_by_rollback`, by contrast, the disposition write and the
        closing reversal are **`casino`'s** transaction (`postRollback`),
        not Bonus Engine's — that flip happens **via
        `bonusengine.RecheckGrantExposure` (N1.4.2)**, which `casino`
        calls, in the same transaction, immediately after the guarded
        compare-and-swap above, whenever its own live Grant-status read
        finds `pending_settlement` (`08 §16.21` site 3,
        `TriggerHeldDispositionResolved`) — replacing this document's
        prior unmechanized "flips in the same transaction, no window"
        claim for this sub-case with that named seam. For N1.7's residual
        case (the record was created atop a Grant already fully
        terminal), no status flip occurs — only the `bonus_held_
        dispositions` row and a new Progress entry are appended, exactly
        as §T.7 already specifies ("every one of the three actions
        appends a Progress entry to `G` even though `G` is already
        terminal").
      - **No double-resolution under concurrency, adopted from `08
        §16.15`'s exhaustive proof**: a resolution racing a rollback of
        the same record, a duplicate rollback, or two distinct rollback
        attempts all contend for the same row's `FOR UPDATE` (HR-25 step
        2) and serialize; whichever commits first leaves `status` at a
        terminal value, and every loser's own guarded `UPDATE ... WHERE
        status = 'held'` affects zero rows — rejected, never applied,
        never a second disposition of the same value. A late resolution
        attempt arriving after a void finds `status ≠ 'held'` and is
        rejected on the identical guard.

   N1.4.1 (below) specifies the holding representation's required
   properties. N1.6 Scenarios 1, 4, 5 and 6 walk this mechanism through
   the sportsbook-shaped, casino-shaped, late-arrival, duplicate-delivery,
   and still-held-rollback cases in full.

6. **Re-entry while already `pending_settlement`**: a second terminal
   trigger firing on a Grant already `pending_settlement` (e.g. a
   wagering-rule breach on a *different* bet fires while the Grant is
   already deferred from an earlier time-limit expiry) is never silently
   discarded — it appends its own Progress entry. If it represents a
   more severe disposition (forfeiture for an abuse finding outranks a
   neutral expiry or a player-opt-out cancellation, since the reason
   code is what a disputing player's case and any compliance reporting
   turn on), `terminal_resolution` is updated to the more severe value;
   a less severe trigger arriving after a more severe one is recorded
   but never downgrades `terminal_resolution`. Concurrent triggers
   serialize under the same `(tenant_id, grant_id)` lock this document
   already relies on throughout — no new race is introduced.

**Why conversion does not get its own new status (parsimony, not an
oversight)**: `completed` already is a "decided outcome, pending a
live-gate re-check" holding state (T.12), so reusing it for "decided
outcome, pending exposure clearing" adds one reason code, not one new
persistent status. Introducing `pending_settlement` for conversion too
would double the number of new states for no additional expressive
power — exactly the kind of avoidable state-machine growth CLAUDE.md's
scope discipline flags.

**No partial/staged conversion is introduced.** While `G` sits blocked
in `completed` under Path A, the currently-free bonus balance is *not*
partially converted — ADR 0032/§7.6/W2.9 already fix conversion as one
atomic, all-or-nothing posting per Grant, and this section does not
reopen that. The full amount available at the moment `AOE` finally
clears converts in one posting, exactly as §7.6 already specifies.

#### N1.4.1 The holding representation — required properties (Fix Wave;
**account/schema shape now DECIDED, Stage 4H-B1 Wave 1.5 Fix Round 2 —
the two-branch split this document previously left open is retired**)

Per this fix wave's own unifying technical contract ("a win credit
reaching the G-2 seam is never posted to any player-accessible balance...
held... until a human supplies G-2"), and closing `security`'s Phase 2
finding (`SEC-W15-08`) that the prior design let a held credit leak into
a **different** Grant's wagering pool through the shared, fungible
`player_bonus` account — this section named the requirements any
concrete holding representation must satisfy, and left the exact
account/schema shape (specifically, item 2's two-branch split, keyed on a
fact only `casino`'s not-yet-seen fix would determine) to `ledger-finance`
to finalize. **`ledger-finance` has now finalized it
(`ledger-accounting-model.md` §7.7.2.1, "the shape `casino` and
`architect` both independently proposed"), and `casino` has adopted it
(`08 §16.14`). Per `ledger-accounting-model.md` §7.7.2.10 item 3
("`bonus-engine` must... retire doc 10 §N1.4.1's two-branch account-type
split (item 2)"), that split is retired here, verbatim, below.**

1. **Ledger-visible, not a side table.** The held value is a real posted
   `LedgerTransaction`, in the same database transaction as the
   settlement event, reachable by the same reconciliation machinery every
   other account already uses. CLAUDE.md: "no side-table-only bonus
   tracking that can't be reconciled"; doc10's own "Bonus must never
   build... a parallel wallet or shadow balance" applies identically to
   a held credit. **Unchanged, confirmed.**
2. **Grant-attributed, never fungible with another Grant's exposure —
   the specific fix for `SEC-W15-08`. Revised, Round 2: one account,
   never two branches.** A single, new, dedicated, Grant-attributed
   ledger account type, `player_bonus_held` (`ledger-accounting-model.md`
   §7.7.2.1/§7.7.2.4), **used identically whether or not the originating
   stake was locked.** The posting is attributable to the specific `G`
   via `GrantLedgerAttribution` (W2.5), the same mechanism `LockedExposure`
   already uses, and is **never** a credit to the shared, per-`(tenant_id,
   player, asset_code)` `player_bonus` account, which is fungible across
   every Grant that player holds — crediting `player_bonus` even
   momentarily is exactly how the pre-revision design let a held amount
   leak into a different Grant's wagering pool. **This document's Round 1
   text proposed two structurally sound realizations "depending on a fact
   only casino's parallel fix determines" — reusing `player_locked_bonus`
   in place for the already-locked branch, or a new dedicated account for
   the no-locked-account branch. `ledger-finance` rejected the first
   branch outright (LF-19, LF-20 — reusing `player_locked_bonus` makes
   §6.6.6's nullifiable predicate permanently true for the entire
   `correlation_id` for as long as the hold is open, and makes
   `LockedExposure`/`HeldDisposition` unable to be read independently,
   since both would be reads of the identical balance) and decided the
   single-account shape instead — the branching question this document
   posed no longer has two live branches; there is exactly one account,
   for every case.** The hold-capture posting (N1.4 step 5b) debits
   `player_locked_bonus` to exactly zero for the resolved stake (the same
   debit `08 §16.5a`'s `L(G) → 0` mandate already requires) and credits
   `player_bonus_held` with the net held value — the two-leg posting is
   present or absent a second leg purely as a fact about whether the
   originating stake was locked, never as a choice between two account
   *types*. `player_bonus_held` is a member of `BONUS_SET`
   (`ledger-accounting-model.md` §7.7.2.3) — the value is still
   bonus-origin and still potentially payable, so excluding it from
   `BONUS_SET` would let disputed bonus value sit outside invariant B1's
   aggregate. Migration `0050`/Rule B2 (extended)'s mirror-generator
   precondition (HR-9) is **unchanged and still blocking** — this section
   does not accelerate or weaken it (`ledger-accounting-model.md`
   §7.7.2.3's own confirmation, adopted verbatim: HR-9 blocks every
   posting to any `BONUS_SET` account today, including the new one).
3. **Excluded from wagering-progress and spend/withdrawal projections,
   for every Grant, not only `G`.** A held amount contributes to no
   Grant's `P_net`/`P_firm` (Model C) while held, is never counted in the
   player's withdrawable-cash projection, and is never a valid funding
   source for a new stake's authorization check, under **any** Grant,
   including `G` itself. This is a second, independent layer closing the
   same leak item 2 closes at the account level: even if a future defect
   made the holding account readable by a progress/authorization query,
   this rule requires that query to explicitly exclude `player_bonus_
   held`'s `transaction_type`(s), not merely rely on it being a different
   account. **`ledger-accounting-model.md` §7.7.2.4 independently
   guarantees the account-level half of this same exclusion**:
   `player_bonus_held` is never added to §6.6.6's nullifiable-predicate
   account list or to `08 §16.4`'s locked-origin-resolution query's
   `account_type IN (...)` list — held and locked stay permanently
   disjoint enumerations, "a future change merging them into one
   enumeration is a regression of this finding, not a simplification"
   (quoted verbatim, since this document's own read-side queries must
   honor the identical discipline).
4. **Idempotent, keyed off the settlement transaction's own id, never
   per-round. Revised, Round 2 — `LF-22` fixed.** This document's Round 1
   text proposed `bonus_g2_hold:<correlation_id>` as the idempotency key.
   `08 §16.4a` independently established that one round-level
   `correlation_id` can legitimately contain multiple, separately-settled
   bets (a multi-bet round) — a key scoped to `correlation_id` alone would
   silently collapse a second, genuinely distinct hold-worthy win under
   the same round into "already held," dropping real money's hold with no
   record at all. **The corrected key is `settlement_ledger_transaction_
   id`** — the settlement's own `ledger_transaction_id` (never
   `correlation_id`, never `grant_id`, never a freshly-minted per-attempt
   UUID, which would defeat idempotency entirely per HR-5/ADR 0032 §8).
   `ledger.Post`'s own `(tenant_id, idempotency_key)` uniqueness has
   already collapsed a redelivered win into a single settlement
   transaction *before* hold-creation logic ever runs (N1.6 Scenario 5's
   own reasoning, unchanged), so `settlement_ledger_transaction_id` is
   already guaranteed to name exactly one real occurrence, whether the
   round is single-bet or multi-bet. `bonus_held_dispositions.UNIQUE
   (tenant_id, settlement_ledger_transaction_id)` (N1.9) is the second,
   independent line of defense: a concurrent or retried attempt to create
   a second row for the same settlement transaction hits the unique
   constraint and is treated exactly like a duplicate `bonus_grant`
   (§7.11's row) — the existing row's id is returned, zero new rows
   written, never a second, silent hold. The round's `correlation_id`
   remains on the record purely as an audit-trail/lookup convenience
   (HR-24), present, never load-bearing for uniqueness.
5. **Reconcilable on its own, not folded silently into an existing sweep
   it would corrupt.** A held amount is neither `player_bonus` exposure
   (must never be read as available balance) nor yet a forfeiture/cash-out
   (its fate is undecided) — folding it into either sweep's existing
   zero-tolerance invariant would either overstate available bonus
   balance or misstate a disposition that has not happened. `ledger-
   finance`'s new `LF-12` reconciliation stream (`ledger-accounting-
   model.md` §7.7.2.8) now gives this its exact shape — hourly,
   zero-tolerance: (i) a balance-attribution check, every `held` row's
   `payout_amount + released_lock_amount` against the sum of
   `player_bonus_held` entries under its own `settlement_ledger_
   transaction_id`; (ii) `player_bonus_held`'s participation in B1
   (extended)'s existing hourly sweep as a third `BONUS_SET` member; (iii)
   an aging check surfacing every `held` row older than a configured
   threshold to the manual G-2 resolution queue. This section adopts that
   stream's existence; it does not design it (`ledger-finance`-owned).

#### N1.4.2 `RecheckGrantExposure` — the Grant-side implementation of `casino`'s named seam (new, Stage 4H-B1 Wave 1.5 Fix Round 2, final closing pass, closing `casino`'s own Phase 2 finding)

**The gap this closes.** N1.4 step 5a and step 5c's `voided_by_rollback`
bullet both asserted, correctly as a requirement but with no named
mechanism, that a value-reducing closing event flips `G.status` from
`pending_settlement` to its recorded `terminal_resolution` "in the same
transaction... no window." Nothing in this document, before this round,
named the concrete call by which `casino`'s own transaction
(`postRollback`, or `08 §16.5a(a)`'s settlement-timeout sweep) reaches
Bonus Engine's Grant-status write to make that true. `08 §16.21` names
that seam, symmetric to the value-creating side's already-named
`ResolveTerminalGrantCredit` (`08 §16.9`/N1.4 step 5b). This section is
Bonus Engine's own implementation of it, adopted verbatim from `08
§16.21`, not re-derived:

```
bonusengine.RecheckGrantExposure(
    ctx context.Context,
    tx <db tx>,
    grantID uuid.UUID,
    triggeringLedgerTransactionID uuid.UUID,
    triggerKind GrantExposureTriggerKind,
) (newStatus bonusengine.GrantStatus, err error)
```

**What "recompute `AOE(G, ·)` live" means concretely, against this
document's own model.** Nothing new is computed — this seam re-enters the
identical three-component sum N1.3 already defines and N1.4 steps 2–4
already evaluate at every other trigger point, just from `casino`'s call
site instead of Bonus Engine's own:

- **Component 1 (`LockedExposure`) and Component 2 (`InFlightExposure`)**
  — read live via §7.10's R1–R4 query family, exactly as N1.3/N1.5
  confirm no fifth query shape is needed for this seam either; the
  recheck is not a new read path, only a new caller of the existing one.
- **Component 3 (`HeldDisposition`)** — read live as a `player_bonus_held`
  balance via `GrantLedgerAttribution` (N1.3's redefinition, N1.10's
  confirmation), scoped to `grantID`.

The seam sums all three, inside the same transaction `tx` the caller
passed in, at the instant it is called — the identical live recompute
N1.4's own mechanism performs, never a cached or point-in-time-stale
value. **If `AOE(G, ·) = ∅`**, the implementation flips `G.status` from
`pending_settlement` to the value already recorded in `terminal_
resolution` (N1.4 step 1/N1.9) and appends a Progress entry citing
`triggeringLedgerTransactionID` and `triggerKind`, returning that new
terminal status as `newStatus`. **If `AOE(G, ·)` remains nonzero**,
`G.status` is left at `pending_settlement`, and `newStatus` reports that
unchanged value. Exactly as `08 §16.21` specifies, this implementation
never branches on which component closed or which call site invoked it
— `triggerKind` is carried into the Progress entry for audit/
observability only, never read by the recompute itself.

**Advisory-lock reuse — confirmed explicitly, no new lock.** This seam
acquires **no lock participant beyond the `(tenant_id, grant_id)`
advisory lock N1.5 already names**, which every call site below already
holds before reaching this seam, for its own unrelated reason (N1.5's
"every transaction that reads or writes anything about `G`" rule; HR-25's
row lock, additionally, at site 3 specifically, per N1.4 step 5c/`08
§16.15`). There is no fifth lock participant, no new lock primitive, and
nothing about this seam's own internals needs one: it is a live read
(the three-component sum) plus, conditionally, the same kind of `G.status`
write N1.4's own mechanism already performs elsewhere under the identical
lock. This mirrors N1.5's own "no new lock primitive" finding for
hold-capture *creation* (5b) — this section makes the identical finding
for exposure-clearing *rechecks*.

**Exactly where this is called — `08 §16.21`'s enumerated sites, adopted
verbatim:**

1. **`postRollback`, plain lock-rollback of a bet before any win/loss is
   known** (N1.4 step 5a) — `TriggerCasinoRollback`.
2. **`08 §16.5a(a)`'s settlement-timeout sweep**, per Grant, per swept
   lock (N1.4 step 5a, N1.7's residual mechanism) — `TriggerCasino
   SettlementTimeout`.
3. **`postRollback`, held-win rollback transition** (N1.4 step 5c's
   `voided_by_rollback` bullet, Scenario 6) — `TriggerHeldDisposition
   Resolved`.

**Not called from `postWin`'s hold-capture path (5b).** Capture never
reduces `AOE(G, ·)` — it replaces a `LockedExposure`/`InFlightExposure`
member with a `HeldDisposition` member of equal value (N1.3, `LF-20`'s
"replaced, not cleared") — so it can never be the event that brings `AOE`
to `∅`, and `08 §16.21` correctly never lists it as a trigger site.

**What this does not do.** It does not select G-2, does not decide `08
§16.10.3`'s open tension about whether an ordinary lock-rollback is
G-2-relevant at all, and redesigns nothing about N1.3/N1.4's own
mechanism — it names the boundary `casino`'s transactions cross to reach
it, exactly as N1.4 step 5b already does for the value-creating side.

#### N1.5 Locking and concurrency — corrected (`LF-11`)

**Corrected by this revision.** The pre-revision text claimed a `SELECT
... FOR UPDATE` on `player_locked_bonus`/`ledger_entries` rows was part
of what serializes N1.4's steps against a concurrent settlement. That
claim is wrong and is withdrawn: `ledger_entries` is **append-only**
(doc10's own standing rule, restated throughout `ledger-accounting-
model.md`) — a row lock taken on existing rows cannot block, delay, or
serialize a *new* `INSERT` from a concurrent transaction, because there
is no row for the lock to hold until after that `INSERT` commits. A
`FOR UPDATE` read in N1.3/N1.4 has exactly one legitimate purpose:
**read-consistency of the sum being computed within the current
transaction** (defense in depth against a concurrent mutation of a row
this computation is mid-reading) — it is **not**, and was never, the
mechanism preventing two concurrent transactions from both observing a
stale `AOE` and both proceeding as if exposure were still open (or both
closed).

**What actually provides that serialization, restated plainly**: the
`(tenant_id, grant_id)` advisory lock (doc10 §9), acquired by *every*
transaction that reads or writes anything about `G` — the expiry-timer
job, a settlement posting, a conversion attempt, a manual-review
resolution, N1.4 step 5c's hold-clearing step — before that transaction
reads `AOE` or writes `G.status`. Composed with the settlement/void
posting's own `(tenant_id, correlation_id)` lock (HR-3), acquired first,
in the pinned order `ledger-accounting-model.md` §7.11 already specifies
(HR-21: `correlation_id → grant_id → player_bonus projection`). Two
transactions contending for the same `G` literally cannot both be inside
their `AOE`-reading/`G`-writing critical section at once — one blocks at
`pg_advisory_xact_lock` until the other commits or rolls back. This is
Postgres's actual serialization primitive for this design, and the only
one; no `FOR UPDATE` claim is required or made for this purpose anywhere
in this revision. Every remaining reference in N1.3/N1.4 to a live
`FOR UPDATE` read is a read-consistency read only, retained for that
narrower reason, never cited as a concurrency guarantee.

No new lock primitive. N1.4.1's holding-representation posting acquires
no lock beyond the two already named above — it is posted inside the
same transaction, under the same `(tenant_id, grant_id)` lock, as the
settlement event that produced it. Identical order whether the
settlement event is closing out ordinary progress, resolving a
`pending_settlement` deferral's value-reducing branch, or capturing a
value-creating credit into a new `HeldDispositionRecord` (5b) or clearing
an existing one (5c).

#### N1.6 Scenario-by-scenario proof

**Scenario 1 — bet placed → grant expires before win arrives → win
arrives late (revised).**

*Sportsbook-shaped (locked account exists).* `t0`: bet locks stake under
`G`, `LockedExposure(G, t0) ≠ 0`. `t1 > t0`: `G`'s time limit elapses;
`NewStakeEligibility(G, t1)` flips to `closed`. Step 2 computes
`AOE(G, t1) ⊇ LockedExposure(G, t1) ≠ ∅` (the lock has not been
released — no settlement/void has occurred). Step 4 fires: `G.status =
pending_settlement`, `terminal_resolution = expired`. `t2 > t1`: the WIN
settlement (amount `W`) arrives, correlated to the original lock, read
under the same lock. Since eligibility already closed before this credit
arrived, **step 5b applies (the corrected mechanism)**: the credit is
**not** posted to `player_bonus`. `W` is captured and posted into the
holding representation (N1.4.1) attributed to `G`; a
`HeldDispositionRecord` is created; `LockedExposure(G, t2)` reaches `0`
but is **replaced**, not cleared — `AOE(G, t2) = HeldDisposition(G, t2) ≠
∅` (assuming no other bet is open). `G.status` **stays**
`pending_settlement`. **No forfeiture posts at `t2`** — there is nothing
to forfeit, because nothing was ever credited to a player-accessible
balance. `t3 > t2`: a human answers G-2; say the answer is
`ACTION_REFORFEIT`. Step 5c fires: `W` posts directly from the holding
representation into `promo_liability`, carrying the staff actor id,
reason code, and a Progress entry. This clears the last outstanding
`AOE` component; `AOE(G, t3) = ∅`; `G.status` flips
`pending_settlement → expired`. **Invariant holds**: at no instant did
`G.status = expired` while `AOE(G, ·) ≠ ∅` (`HeldDisposition` is part of
`AOE` precisely so this remains true across the whole episode); the WIN
credit never landed against an already-terminal Grant, **and — the
actual defect this revision closes — the WIN credit never landed
against `player_bonus` at all before its disposition was decided**, so
no post-then-reforfeit sequence exists anywhere in this trace to
relabel.

*Casino-shaped (no locked account).* `t0`: `postBet` posts a bonus-funded
stake straight to `house_gaming`; a `WageringProgress` row records the
contribution, `InFlightExposure(G, t0) = {that row}`. `t1 > t0`: `G`'s
time limit elapses; `NewStakeEligibility` closes. Step 2 computes
`AOE(G, t1) ⊇ InFlightExposure(G, t1) ≠ ∅`. Step 4 fires identically:
`pending_settlement`, `terminal_resolution = expired`. `t2 > t1`:
`postWin` delivers the round's win (amount `W`), correlated via
`roundCorrelationID`. **Corrected behavior, identical in kind to the
sportsbook case (this is exactly `architect`'s doc 29 §4.3 scenario, now
genuinely closed rather than relabelled)**: `W` is captured and parked in
`player_bonus_held` — Round 1's text described this as needing "a
dedicated holding account type, since casino's current shape has no
locked account to hold it in-place," pending `casino`'s own parallel fix
determining otherwise (N1.10); **this is no longer a hypothetical
pending a future determination — it is `ledger-finance`'s actual,
finalized design (`ledger-accounting-model.md` §7.7.2.1), used
identically for casino and sportsbook alike** (N1.4.1 item 2). Since
casino's `postBet` posted the stake straight to `house_gaming` with no
prior lock, the hold-capture posting here carries **only** the `W` leg
(`Dr house_gaming W / Cr player_bonus_held W`) — the `X` leg is absent
entirely, exactly as N1.4 step 5b(ii) describes for the never-locked
case; a `bonus_held_dispositions` row is created with `released_lock_
amount = 0`; `InFlightExposure`'s member is replaced by the new
`player_bonus_held` balance; `G` stays `pending_settlement`; nothing is
credited or reforfeited automatically. Resolution proceeds identically to
the sportsbook case once a human answers G-2 (step 5c). **Invariant
holds, subject to N1.7's disclosed bound** — and, unlike the pre-revision
text, holds in the sense `ledger-finance` actually required: no
credit-then-reforfeit sequence exists anywhere in this trace.

**Scenario 2 — Grant cancelled by staff while a bet is in flight.**
Identical mechanism, different trigger: staff cancellation is one more
row in §1.3's transition table feeding the same Path B mechanism. At the
instant of the cancellation request, `AOE(G, ·) ≠ ∅` (a bet is open) →
`pending_settlement`, `terminal_resolution = cancelled`,
`terminal_trigger_reason_code` = the staff-supplied reason (§10's
mandatory-reason-code rule for cancellation is satisfied at the
*original* trigger, carried through to the eventual `cancelled` Progress
entry — never lost by the deferral). The open bet resolves exactly as in
Scenario 1 (win, loss/timeout, or void — N1.7 covers the loss/timeout
case; a win is captured and held per step 5b, never posted-then-
reforfeited) and the Grant finalizes to `cancelled` only once `AOE = ∅`
— inclusive of any outstanding `HeldDisposition` member, per N1.3's
revision.
**Invariant holds identically** — cancellation is not a structurally
different trigger from expiry for this mechanism's purposes, exactly as
§T.10 already states expiry and cancellation share one posting shape.

**Scenario 3 — concurrent expiry-timer-fire and win-settlement.** Both
the expiry-timer job and the inbound WIN settlement attempt to act on
`G` at nearly the same instant. Both take `G`'s `(tenant_id, grant_id)`
advisory lock (N1.5's pinned order) before reading or writing anything
about `G`. Whichever acquires the lock first commits its full effect
(read `AOE`, decide, write) before the second's `SELECT ... FOR UPDATE`
of the relevant balance/status even begins evaluating. Two orderings,
both safe:
- **Expiry wins the race**: it observes `AOE(G, ·) ≠ ∅` (the win has not
  been observed yet from this transaction's point of view) and defers to
  `pending_settlement`, closing `NewStakeEligibility`. The win-settlement
  transaction, now second, observes `G.status = pending_settlement` —
  since eligibility already closed before this credit arrived, step 5b
  applies: the credit is captured into the holding representation, a
  `HeldDispositionRecord` is created, and `AOE` remains nonzero (now via
  that record) rather than clearing. `G` stays `pending_settlement` until
  a human later answers G-2 for this occurrence (step 5c) — exactly
  Scenario 1's corrected sequence, just compressed to near-simultaneous
  timestamps.
- **Win-settlement wins the race**: it posts its **ordinary** credit
  first (`G` is still `activated`/`in_progress` at this instant —
  `NewStakeEligibility` has not yet closed, the expiry trigger has not
  yet run — so this is genuinely **not** G-2 at all: an ordinary
  in-progress credit against a fully-eligible Grant), which clears that
  bet's contribution to `InFlightExposure`/`LockedExposure`. The
  expiry-timer transaction, now second, computes `AOE(G, ·)` **after**
  the win has posted; if no other bet is open, `AOE = ∅` and expiry
  writes the terminal status directly (N1.4 step 3's ordinary path — the
  mechanism this section adds never even engages, because by the time
  expiry's own transaction reads `AOE`, there is nothing left to defer
  for).

Both orderings are serialized by the same lock and produce the identical
final state (`G.status = expired`, the bet's stake fully accounted for)
regardless of which transaction the database happened to schedule
first — this is exactly the property `pg_advisory_xact_lock` already
provides for every other Grant-completion race this document specifies
(doc10 §9). **Invariant holds under both interleavings; no third
interleaving is reachable**, because both participants take the same
lock before observing or mutating `G`.

**Scenario 4 — new, added by this revision — a late WIN arrives after
`G` has already validly reached full terminal status (N1.7's disclosed
residual).** `t0`: bonus-funded bet placed under `G` (casino-shaped, no
locked account). `t1`: `G`'s time limit elapses; `AOE(G, t1)` is
computed — using N1.7's settlement-window mechanism, no WIN has been
observed within the configured window, so the bet is presumed a loss for
`AOE`-clearing purposes only (N1.7; this presumption is TECHNICAL,
value-reducing, no G-2 involvement); `AOE(G, t1)` reaches `∅` (nothing
else open) and `G` finalizes directly to `expired` (N1.4 step 3's
ordinary path). At this instant, TI-1 holds exactly: `AOE(G, t1) = ∅`
while `G.status(t1) = expired`, using every fact knowable at `t1`. `t2 >
t1`: the provider, in violation of its own settlement-window SLA,
delivers a genuine WIN callback (amount `W`) for the presumed-lost bet.
This is a **new fact**, not a re-evaluation of `t1` — N1.2's Corollary
TI-1a governs it: TI-1 is not retroactively falsified. **Corrected
handling, closing the actual danger the pre-revision text left open**:
this WIN is handled by the *identical* step 5b/N1.4.1 mechanism as any
other value-creating closing event against exposure that was open when
eligibility closed — `W` is captured and parked in the holding
representation, attributed to `G`, with a new `HeldDispositionRecord`,
**without reopening or changing `G.status`** (`G` is already, validly,
`expired`; nothing here contests that). A Progress entry is appended atop
the terminal record exactly as §T.7 already specifies for this case. A
human answers G-2 exactly as in Scenario 1, acting on the
`HeldDispositionRecord`, never on a posted-then-reforfeited balance.
**This is the one place TI-1 is disclosed, not proven, to hold only
probabilistically/by-SLA (N1.7) — but the disposition mechanism itself
is now identical in every branch**: whether the win is early (Scenario
1, `G` still `pending_settlement`) or late (this scenario, `G` already
fully terminal), the money takes the same path — captured, held, never
posted-then-reforfeited, disposed of only by a human-supplied G-2
answer. This uniformity is itself part of the fix: the pre-revision
text's defect was specifically that the "in-window" path (Scenario 1)
was handled differently — and incorrectly — from what §T.7 already
required for the "out-of-window" path (this scenario); this revision
removes that divergence rather than narrowing it.

**Scenario 5 — new, added by this revision — a duplicate/late-duplicate
delivery of a held WIN never creates unrestricted cash (proves item 4 of
the fix-wave directive).** A WIN callback for the stake in Scenario 1 or
4 is redelivered (provider retry, at-least-once delivery) after its
`HeldDispositionRecord` already exists (still `held`, or already
resolved). Two sub-cases: **(a) still held** — the redelivered event
carries the identical `correlation_id`/`provider_tx_id` as the first
delivery; `ledger.Post`'s own `(tenant_id, idempotency_key)` uniqueness
no-ops the settlement transaction itself before N1.4's mechanism re-runs
at all (N1.5's confirmation, unchanged by this revision) — no second
`bonus_held_dispositions` row, no second hold posting; N1.4.1 item 4's own
idempotency key (`UNIQUE (tenant_id, settlement_ledger_transaction_id)`,
corrected per `LF-22`) is a second, independent line of defense that
would also reject a duplicate hold posting even if the outer idempotency
check were somehow bypassed. **(b)
already resolved** (say `ACTION_ROUTE_TO_CASH` already posted `W` to
`player_cash`) — the redelivery hits the same outer idempotency key and
no-ops before reaching N1.4.1 at all; the already-resolved
`HeldDispositionRecord` is never re-read, re-posted, or re-decided. **In
neither sub-case does any additional value reach `player_bonus` or
`player_cash`.** No interleaving of duplicate delivery and G-2
resolution produces unrestricted cash, because the only two things
capable of moving value out of the holding representation are (i) the
outer, DB-enforced idempotency key rejecting the duplicate before it is
ever seen as "new," or (ii) a specific, once-only
`HeldDispositionRecord.status` transition (`held → resolved_reforfeit` /
`held → resolved_route_to_cash`) gated on that record's own current
status — a second attempt to resolve an already-resolved record is
rejected exactly as a second reversal of an already-reversed transaction
is rejected today (§7.7's double-reversal protection, reused unmodified,
not a new mechanism).

**Scenario 6 — new, Stage 4H-B1 Wave 1.5 Fix Round 2 — a provider
rollback of a still-`held` win voids the disposition and clears `AOE`
without resurrecting exposure (adopts `ledger-accounting-model.md`
§7.7.2.7 and `08 §16.15`'s Part B walkthrough verbatim, restated in this
document's own `AOE`/`pending_settlement` terms, which those two
documents do not use).** Continuing Scenario 1's casino-shaped trace:
`t2`: the WIN is captured per step 5b, `bonus_held_dispositions` row `H`
is created (`status = held`), `AOE(G, t2) = {H}` (assuming no other bet
open). `t2.5 > t2`, before a human answers G-2: the provider delivers a
rollback naming `H`'s own `settlement_ledger_transaction_id`. `casino`'s
`postRollback` finds `H` with `status = 'held'`, acquires `(tenant_id,
grant_id=G)` then `H`'s row `FOR UPDATE` (HR-25), and posts, in one
transaction: the generic entry-inversion (`Cr house_gaming W`, and `Cr
player_locked_bonus X` only if the `X` leg was present — never
resurrecting the lock) **plus** the guarded compare-and-swap `UPDATE
bonus_held_dispositions SET status = 'voided_by_rollback', resolution_
ledger_transaction_id = <reversal id> WHERE id = H.id AND status =
'held'`. **From this document's own `AOE` perspective (the part `casino`'s
document does not state, because `AOE`/`pending_settlement` are this
document's vocabulary, not casino's)**: this is N1.4 step 5c's `voided_
by_rollback` transition — TECHNICAL, not a G-2 answer. `H`'s contribution
to `AOE(G, ·)` (a `player_bonus_held` balance read, N1.3 Component 3) is
removed the instant the reversal posts (the balance the read observes has
gone back to zero for this occurrence), and is **not** replaced by a
restored `LockedExposure`/`InFlightExposure` member — the underlying bet
is now a fully reversed, closed fact, exactly like an ordinary VOID
(step 5a), never reopened as still-in-flight. If `H` was `AOE(G, ·)`'s
only remaining component, `postRollback` calls **`bonusengine.
RecheckGrantExposure(ctx, tx, G, <reversal id>,
TriggerHeldDispositionResolved)`** (N1.4.2, `08 §16.21` site 3) in the
**same** transaction as the reversal and the compare-and-swap, under the
one `(tenant_id, grant_id)` lock both `postRollback` and this recheck
already hold. Bonus Engine's own live `AOE` recompute inside that call
finds `AOE(G, ·) = ∅` and flips `G.status` from `pending_settlement` to
its recorded `terminal_resolution` (`expired`) before the transaction
commits — this named seam, not a bare assertion, is what makes there no
window in which `H` is `voided_by_rollback` while `G` still incorrectly
reads `AOE ≠ ∅` for a component that no longer exists. `t3 > t2.5`: a queued
staff action attempts to apply a G-2 answer to `H`, unaware of the
rollback (a realistic race — the resolution UI's queue entry was
populated before `t2.5`). Its own attempt at the identical guarded
pattern (`UPDATE ... WHERE status = 'held'`, under the same HR-25 lock
order) finds `status = 'voided_by_rollback' ≠ 'held'` — **zero rows
affected, rejected outright**, never silently succeeding against a
disposition that no longer exists and never reopening `G` (already
`expired`). **Invariant holds**: `AOE(G, ·) = ∅` and `G.status = expired`
agree at every instant after the rollback commits; no interleaving
(rollback-then-resolution, resolution-then-rollback, or two concurrent
rollback attempts, `08 §16.15`'s three-case proof) produces a
double-resolution, a resurrected lock, or a `G.status` left
`pending_settlement` for a component that has already been voided.

#### N1.7 The honest gap — a technical loss-side presumption, and its residual (revised)

**Stated plainly, not papered over**: `internal/casino`'s provider
protocol has exactly three callback kinds — `bet`, `win`, `rollback`
(verified at `internal/casino/types.go:396-398`) — **there is no fourth
"round closed, no win" or "loss confirmed" callback.** A casino loss is
represented by the *absence* of a future `win` callback, never by a
positive signal. This means `InFlightExposure(G, t)` cannot, from events
alone, ever be proven to have reached `∅` for a losing bet — the
platform cannot distinguish "this bet lost, nothing more will ever
arrive" from "this bet's win has simply not been delivered yet" without
an additional, engineering-asserted rule.

**The additional mechanism this requires, named rather than assumed**: a
per-provider (or platform-default) **settlement window** — a bounded,
configured maximum interval after which a bonus-funded bet with no `win`
callback is treated, **for `AOE`-clearing purposes only**, as resolved.
This is explicitly **not** a ledger-truth claim (nothing is posted for a
"timed-out" loss — a genuine loss already has its complete financial
fact recorded at `postBet` time, per §7.7/ADR 0032; the timeout only
releases the *Grant* from `pending_settlement`) and it is **not** a
guarantee that no `win` callback can ever arrive after the window — a
provider could still violate its own delivery SLA and deliver a `win`
callback after the window has already let `G` finalize to `expired`/
`cancelled`/`forfeited`.

**Correction this revision makes to the original framing (this is the
core of `LF-2`'s fix, restated here as a correction to N1.7
specifically):** the settlement-window mechanism above governs **only**
the loss-side (value-reducing) presumption used to clear
`InFlightExposure` for `AOE`'s purposes — it has **no bearing whatsoever
on how a WIN is handled**. The pre-revision text's error was treating
"still within the settlement window" as if it also meant "therefore any
WIN arriving now is not G-2." It is not: per step 5b/N1.4.1, **every**
WIN correlated to exposure that was open when eligibility closed is
handled by the hold-then-human-decide mechanism, regardless of how much
or how little time has elapsed. The settlement window's only job is:
after this much time with no WIN observed, presume the bet lost, for the
narrow, disclosed, value-reducing purpose of letting `AOE` clear and `G`
finalize — a purely **TECHNICAL** mechanism, because a genuine loss has
no dispute to have and nothing is credited.

**Consequence, restated precisely**: the settlement-window presumption
can be wrong exactly once per bet, in exactly one direction — it can
presume a loss that turns out to have been a win. When that happens,
N1.6 Scenario 4 is the mechanism that fires — **not** a bypass of G-2 and
**not** a return to post-then-reforfeit; the late WIN is captured and
held exactly as an in-window WIN would be. This is a materially smaller
residual than the pre-revision text disclosed: the earlier version's own
bound only protected "the normal path" from the P0 defect and left both
the late-arriving case *and* the normal case dependent on G-2-selection
risk (since the normal path itself silently reforfeited); this
revision's residual is purely about **how long a Grant may sit in
`pending_settlement`/already-terminal-with-an-open-hold before staff
notices** (an operational/monitoring concern, closed by N1.10's new
reconciliation stream, `LF-12`), not about any risk of silent G-2
selection, which no longer exists on any path. This section does not
select which of §T.7's three actions handles the held record — G-2
remains genuinely open for it, exactly as ADR 0039 leaves it.

**Two ways this residual could later be closed, named, neither designed
here**: (a) `casino` defines and contractually/technically enforces a
maximum settlement window per provider (a `casino`-owned dependency,
cross-referenced to `4HB1W15-01`'s parallel work — this section does not
set that number); or (b) casino's bonus-funded stakes are routed through
a locked-account model symmetric to sportsbook's `player_locked_bonus`
(eliminating `InFlightExposure`'s timeout dependency entirely by giving
casino the same exhaustive win/loss/void settlement guarantee sportsbook
already has) — a genuinely new architecture change, `casino`-owned,
requiring its own ADR and human sign-off, not something this document
authorizes by naming it.

**Sportsbook shape has no such gap.** Because sportsbook settlement is
exhaustive over win/loss/push (every locked bet receives an explicit
grading event, `docs/decisions/0038`), `LockedExposure` clears only by a
real event, never a timeout — TI-1 holds **exactly and unconditionally**
for the sportsbook shape, with no residual.

#### N1.8 Confirmation: this resolves G-2's original failure mode, without selecting G-2's answer (revised — the non-selection argument `LF-2` found missing)

ADR 0039 Decision 2's question is: "when a settlement or void credit
arrives against a bonus Grant that has **already gone terminal**... what
should the platform do with that credit?" This revision answers a
narrower, prior question first — **where does the credit's value
physically sit while that question is unanswered** — and the answer is:
nowhere a player, another Grant's wagering, or a withdrawal can reach,
and nowhere `ledger-finance`'s reconciliation cannot see (N1.4.1).

**Why parking the value is not itself a fourth, silent answer — the
argument `ledger-finance`'s finding showed the pre-revision text needed
and did not have.** Each of G-2's three named actions requires a
subsequent, distinguishable act before the player's position is actually
settled:

- `ACTION_REFORFEIT` requires an act that **extinguishes** the value (a
  `promo_liability` write-down).
- `ACTION_ROUTE_TO_CASH` requires an act that **releases** the value to
  `player_cash`.
- `ACTION_HOLD_FOR_REVIEW` requires an act that **queues** the value for
  a human, who then performs one of the two acts above.

**Parking the value performs none of these three acts.** It extinguishes
nothing (the value is still fully present, ledger-visible, in the
holding representation — reconcilable, not written off). It releases
nothing (the player cannot spend, withdraw, or wager against it, N1.4.1
item 3). It does not even, by itself, constitute "queuing for review" in
the operational sense `ACTION_HOLD_FOR_REVIEW` names, because parking
happens **automatically and identically regardless of which of the three
answers G-2 eventually receives** — it is not a queue entry created
*because* `ACTION_HOLD_FOR_REVIEW` was chosen; it is the substrate every
one of the three answers is applied *to*. That is exactly why N1.4.1
describes it as the parking state any of G-2's three eventual answers
would need to exist regardless of which is chosen, not as
`ACTION_HOLD_FOR_REVIEW` under a different name. (N1.10's new
reconciliation stream additionally guarantees a parked value can never
simply be forgotten, which is the one respect in which parking resembles
a queue — but resembling a queue's *visibility* property is not the same
as *being* the `ACTION_HOLD_FOR_REVIEW` *decision*.) Parking satisfies
none of the three actions' defining acts, and forecloses none of them —
a human choosing any of the three later finds the value exactly where it
needs to be to execute that choice, undamaged and undiminished by the
parking itself.

**Contrast with the defect this replaces, stated once more for the
record.** The pre-revision mechanism performed `ACTION_REFORFEIT`'s
exact defining act (a credit to `player_bonus` immediately followed by
an extinguishing `bonus_forfeiture` debit) automatically, on every
occurrence, with no human decision — that this happened while
`G.status = pending_settlement` rather than `expired` changed the
state-machine label, not the act performed. This revision's mechanism
performs **none** of the three defining acts until a human supplies the
missing answer; that is the actual, substantive difference
`ledger-finance`'s finding required, not a re-description of the same
behavior.

**This still selects nothing from the Human Decision Register.**
`ACTION_REFORFEIT`/`ACTION_ROUTE_TO_CASH`/`ACTION_HOLD_FOR_REVIEW` (§T.7)
remain unselected — they now apply uniformly to **every** value-creating
credit reaching G-2 (not narrowed to N1.7's residual, as the
pre-revision text incorrectly claimed) and are executed exactly as §T.7
already specifies, only now against a held amount rather than a
posted-then-reversed one. `OpenBetSelfExclusionPolicy` (Decision 1) is
untouched — a self-exclusion-driven void or forfeiture is one more Path
B trigger, using the identical mechanism, and this section takes no
position on `SETTLE_NORMALLY` vs. `VOID_ON_SELF_EXCLUSION`. The cashout
proceeds-split/FD-1 (Decision 3) is untouched — no cashout code exists,
and nothing here creates or assumes one. Wave 1's W9 human-decision
safety analysis stands unchanged.

#### N1.8.1 Every state transition, classified TECHNICAL or POLICY-DEPENDENT (exhaustive, per the fix wave's directive)

| # | Transition / event | Classification | Why |
|---|---|---|---|
| 1 | `issued`→`activated` and all pre-exposure transitions (§1.3), unaffected by N1 | TECHNICAL | No exposure exists yet |
| 2 | `activated`/`in_progress` → `pending_settlement` (any Path B trigger fires while `AOE ≠ ∅`) | TECHNICAL | `NewStakeEligibility` closing is, by definition, purely a "no new stakes" signal; it carries no claim about disposition and needs no G-2 answer to fire |
| 3 | `activated`/`in_progress` → terminal directly (Path B trigger fires while `AOE = ∅`) | TECHNICAL | No exposure existed to dispute |
| 4 | `completed` blocked (stays `completed`) at a conversion attempt while `AOE ≠ ∅` (including a nonzero `HeldDisposition`) | TECHNICAL | Same class of precondition-block T.12 already establishes for RG/Risk/`AssetAuthorization` denial; a block is not a disposition choice |
| 5 | `completed → converted`, once every `AOE` component (including every `HeldDisposition` record) clears | TECHNICAL | Mechanical re-trigger of an already-satisfied precondition — the flip itself is mechanical even though satisfying the precondition may itself depend on row 8/9/10 below having already resolved |
| 6 | Value-reducing closing event clears a `LockedExposure`/`InFlightExposure` member (loss, VOID, ROLLBACK, or presumed-loss settlement-window elapse) | TECHNICAL | No credit is created; nothing to dispute; matches `casino`'s endorsed `B(G)`/`L(G)` split |
| 7 | Value-creating closing event (WIN) captured into a new `HeldDispositionRecord`, replacing a `LockedExposure`/`InFlightExposure` member (step 5b) | TECHNICAL | Recording-and-parking a fact is not a disposition choice (N1.8's proof); it performs none of G-2's three defining acts |
| 8 | `HeldDispositionRecord` resolves via `ACTION_REFORFEIT` | POLICY-DEPENDENT | Requires a human-supplied G-2 answer before this act may occur |
| 9 | `HeldDispositionRecord` resolves via `ACTION_ROUTE_TO_CASH` | POLICY-DEPENDENT | Same |
| 10 | `HeldDispositionRecord` resolves via `ACTION_HOLD_FOR_REVIEW`'s own later manual sub-choice of row 8 or 9 | POLICY-DEPENDENT | Same, plus an additional manual step §T.7 already specifies |
| 11 | `pending_settlement → terminal_resolution`, once all `AOE` components (including every `HeldDisposition` record) clear | TECHNICAL trigger, gated on a precondition that may itself be POLICY-DEPENDENT | The flip is mechanical once the precondition holds; if the last outstanding component was a `HeldDisposition` record, satisfying the precondition required row 8/9/10 to have already happened |
| 12 | A late WIN captured into a `HeldDispositionRecord` atop a Grant already fully `TERMINAL` (N1.7 residual, N1.6 Scenario 4) | TECHNICAL | Identical reasoning to row 7; does not reopen or change `G.status` |
| 13 | Second/re-entry Path B trigger while already `pending_settlement`, severity-ranking `terminal_resolution` | TECHNICAL | Mechanical comparison of reason-code severity; unchanged from the pre-revision text |
| 14 | `Any → reversed` (append-only compensating transition) | TECHNICAL | Excluded from `TERMINAL` by N1.2; not a forward-settlement-credit case |
| 15 | `bonus_held_dispositions.status: held → voided_by_rollback` (Scenario 6, N1.4 step 5c, adopting `ledger-accounting-model.md` §7.7.2.7/`08 §16.15`) | TECHNICAL | Undoes a settlement event that never should have existed — symmetric to an ordinary VOID/ROLLBACK (row 6), not a G-2 selection; the underlying stake's exposure is not restored, only extinguished |

#### N1.9 Additive edits this section makes (named, not silently applied) — revised

- §1.2's Grant-state table gains one row: `pending_settlement` (N1.4),
  inserted as an additional non-`TERMINAL` status, no existing row
  edited.
- §1.3's transition table gains: `activated`/`in_progress →
  pending_settlement` (trigger: any Path B trigger firing while `AOE ≠
  ∅`) and `pending_settlement → {expired|cancelled|forfeited}` (trigger:
  every `AOE` component, including every `HeldDisposition` record,
  reaching `∅`) — both additive rows, no existing row's trigger
  conditions changed except that they are now understood to gate on
  `AOE(G, ·) = ∅` as a precondition (N1.4 step 2/3), which was previously
  unstated because `AOE` did not exist as a concept until this section.
- **`AOE`'s definition gains a third component, `HeldDisposition(G, t)`**
  (N1.3), closing `LF-2`: without it, a WIN's mere *arrival* was
  indistinguishable from its *disposition*, which is exactly how the
  pre-revision mechanism auto-selected `ACTION_REFORFEIT`.
- **A new table, `bonus_held_dispositions`** (Bonus-Engine-owned, built in
  Bonus Engine's own migration range `0054`+, the same
  `ledger-finance`-specifies/`bonus-engine`-builds split as
  `WageringProgress`/§6.6.4). **Revised, Stage 4H-B1 Wave 1.5 Fix Round
  2: this document's own Round 1 field list (`HeldDispositionRecord`:
  `grant_id`, `correlation_id`, `amount`, `asset_code`, `observed_at`,
  `origin_kind`, `status`, `resolved_at`, `resolved_by`,
  `resolution_reason_code`) is retired and replaced, verbatim, by
  `ledger-accounting-model.md` §7.7.2.5's binding contract — per
  §7.7.2.10 item 1 ("build `bonus_held_dispositions` to exactly §7.7.2.5's
  contract... not `correlation_id`, not `grant_id`") this document
  invents no field names of its own:**

  ```
  bonus_held_dispositions
    id                                UUID          PK
    tenant_id                         UUID          NOT NULL   -- RLS
    brand_id                          UUID          NOT NULL   -- denormalized from Wallet
    wallet_id                         UUID          NOT NULL   -- FK wallets.id
    player_account_id                 UUID          NOT NULL   -- denormalized from Wallet
    asset_code                        TEXT          NOT NULL   -- FK assets.code
    grant_id                          UUID          NOT NULL   -- FK Grant (this document's G)
    correlation_id                    UUID          NOT NULL   -- audit trail only, NEVER the idempotency key
    settlement_ledger_transaction_id  UUID          NOT NULL   -- FK ledger_transactions.id; THE idempotency key
    payout_amount                     NUMERIC(38,0) NOT NULL CHECK (>= 0)            -- W
    released_lock_amount              NUMERIC(38,0) NOT NULL DEFAULT 0 CHECK (>= 0)  -- X; 0 if never locked
    status                            TEXT          NOT NULL DEFAULT 'held'
                                       CHECK (status IN ('held','resolved_reforfeit',
                                                          'resolved_route_to_cash','voided_by_rollback'))
    created_at                        TIMESTAMPTZ   NOT NULL
    resolved_at                       TIMESTAMPTZ   NULL
    resolved_by_actor_id              UUID          NULL   -- feeds security's REQ-SEP-BONUS-4 (N1.12)
    resolution_reason_code            TEXT          NULL
    resolution_ledger_transaction_id  UUID          NULL   -- the transaction that moved the value out;
                                                            -- for voided_by_rollback, the reversal's id

    CHECK (payout_amount + released_lock_amount > 0)
    UNIQUE (tenant_id, settlement_ledger_transaction_id)
    CHECK ((status = 'held') = (resolved_at IS NULL))
    CHECK (status <> 'held' OR resolution_ledger_transaction_id IS NULL)
    -- RLS: tenant_id, mirroring every other tenant-owned table (CLAUDE.md)
  ```

  Every field a human resolving G-2 needs is present without a join back
  to `ledger_entries` — `payout_amount`/`released_lock_amount` are
  denormalized at hold-creation time from the posting that created them,
  never re-derived. `status`'s admitted values are exactly the four
  above: **no third economic outcome, no fourth status** — `held`
  (the open state, and `ACTION_HOLD_FOR_REVIEW`'s complete state, N1.4
  step 5c), the two G-2 disposition-bearing terminal values, and
  `voided_by_rollback` (a technical undo, N1.4 step 5c/Scenario 6 — not a
  G-2 answer). This table's name and this contract now **are** the
  authoritative name — every remaining reference in this document to
  "`HeldDispositionRecord`" (a Round-1-coined name that never had its own
  migration) means exactly this table's row, and this document's own
  earlier field list is superseded, not merely supplemented, by the block
  above.
- **Two new Progress reason codes**: `g2_credit_held` (a value-creating
  credit was captured and parked, not posted) and
  `g2_disposition_applied` (a human-supplied G-2 answer was applied to a
  specific `HeldDispositionRecord`) — both required by this document's
  own Progress-trail completeness mandate; N1.11 names the adversarial
  tests proving neither is ever skipped.
- **A correction to §T.7's `ACTION_REFORFEIT` bullet's literal
  posting-order text, routed here rather than edited in place** (§T.7 is
  frozen Technical Contract text this section may only extend, per this
  document's own Wave 1.5 dispatch instruction): wherever
  `ACTION_REFORFEIT` is applied, per this section's N1.4 step 5c, it
  posts from the holding representation directly to `promo_liability` —
  it never performs §T.7's literally-described "post the inbound
  credit... then... post a second `bonus_forfeiture`" sequence, because
  the inbound credit is never posted in the first place. Flagged for
  `ledger-finance` ratification, since §T.7's text is jointly relevant to
  this document's Terminal-Grant Technical Contract and `ledger-finance`'s
  own §7.7.
- **`ledger-finance`'s own §7.7 "Gated on G-2" paragraph's
  `ACTION_HOLD_FOR_REVIEW`-only holding-mechanism framing is
  generalized**: N1.4.1's holding representation is required for
  **every** value-creating G-2 occurrence, not only ones where staff has
  additionally chosen `ACTION_HOLD_FOR_REVIEW` as the eventual answer —
  restated as a correction routed to `ledger-finance`, not adopted
  unilaterally (N1.10).
- **`LF-11` correction** (N1.5): the `FOR UPDATE` claim is withdrawn as a
  concurrency-serialization claim; the advisory-lock composition is
  restated as the actual mechanism, unchanged in substance from what
  N1.5 always relied on operationally — only the prior text's
  *description* of why it works was wrong.
- §T.9's "expiry while a stake is locked... reached the moment that
  locked stake's settlement or void/rollback later arrives" is
  unchanged in substance; N1.4 is the mechanism that makes "reached the
  moment" precise (previously it meant "G-2 fires then, auto-resolved";
  now it means "the credit is captured and held then, disposed of only
  once a human answers G-2").
- A new Progress reason code, `open_exposure_outstanding` (N1.4 Path A),
  alongside the existing RG/Risk/`AssetAuthorization` reason codes at the
  conversion checkpoint (T.12) — unchanged from the pre-revision text,
  restated here because Path A's block now also fires on a nonzero
  `HeldDisposition` component (N1.3).
- A new reconciliation stream is named, not designed (N1.10), closing
  `LF-12`.
- **New this round, final closing pass**: `bonusengine.
  RecheckGrantExposure` (N1.4.2) is named as the Grant-side mechanism by
  which `casino`'s `postRollback` and settlement-timeout sweep flip
  `G.status` at the trigger points N1.4 step 5a/5c already describe — no
  new field, no new lock, and no new Progress reason code (it appends the
  same ordinary terminal-transition Progress entry N1.4 already writes
  whenever `G.status` flips, now additionally citing
  `triggeringLedgerTransactionID`/`triggerKind`); only a previously-
  unnamed call is now named.

#### N1.10 Cross-domain dependencies — revised, Stage 4H-B1 Wave 1.5 Fix Round 2 (most items below CLOSED this round, stated as such rather than left as Round 1's open questions)

- **`casino` — CLOSED this round.** Round 1 flagged this dependency as
  "not yet seen." `casino`'s own Round 2 dispatch (`08` §16.9/§16.14–
  §16.18) is now adopted verbatim throughout N1.4/N1.4.1/N1.6/N1.9 above:
  the widened `ResolveTerminalGrantCredit(ctx, tx, grantID, correlationID,
  creditKind, payoutAmount, releasedLockAmount,
  settlementLedgerTransactionID) (heldDispositionID, err)` signature
  (N1.4 step 5b(i)), its unconditional-capture call order (5b(i)), the
  two-leg hold-capture posting (5b(ii)), the single `player_bonus_held`
  account (N1.4.1 item 2, retiring the two-branch split this dependency
  previously turned on), and the still-held rollback compare-and-swap
  (5c/Scenario 6). **New this round, final closing pass**: `08 §16.21`'s
  `bonusengine.RecheckGrantExposure` seam, closing `casino`'s own Phase 2
  finding that N1.4 step 5a/5c's "flips in the same transaction, no
  window" claim never named a concrete mechanism — adopted at new N1.4.2
  and cited at N1.4 step 5a, step 5c's `voided_by_rollback` bullet, and
  Scenario 6. **Still open, confirmed by both documents**: `08
  §16.10.1`'s recommendation that a *future* bonus-funded casino bet
  adopt the locked-account shape (case B/G/I) rather than the
  immediate-absorb shape remains routed to `ledger-finance`/`architect`
  for ratification — this document's own design (N1.4.1 item 2, the
  single-account shape) works identically whether or not that
  recommendation is ever adopted, since the hold-capture posting's `X`
  leg is simply present or absent depending on whether a lock existed,
  never a second account-type choice. `08 §16.10.3`'s flagged tension
  (whether a rollback-of-the-lock should be G-2-relevant) also remains
  open, unresolved by this section per that section's own routing.
  **Also still open, confirmed by both `casino` and `ledger-finance`'s
  own documents (`08 §16.20`, `ledger-accounting-model.md` §7.7.2.11),
  and stated here so a reader of this document alone is not misled into
  thinking the mechanism is fully closed**: a rollback of an
  already-`resolved_reforfeit`/`resolved_route_to_cash` `bonus_held_
  dispositions` row — as distinct from Scenario 6's still-`held` case,
  which N1.4 step 5c and Scenario 6 close in full — is **LF-10**, routed
  to `ledger-finance`, not pre-selected or decided by this document.
- **`ledger-finance` — CLOSED this round for everything Round 1 asked.**
  `ledger-accounting-model.md` §7.7.2 ratifies and finalizes the account/
  schema shape (N1.4.1 item 2), the idempotency key (item 4, `LF-22`),
  the reconciliation stream (`LF-12`, item 5), and the `ACTION_REFORFEIT`
  posting-order correction and `ACTION_HOLD_FOR_REVIEW`-framing
  generalization this document previously flagged for ratification (both
  now adopted in N1.4 step 5c). The `AOE`-clearing read path for
  `LockedExposure`/`InFlightExposure` remains confirmed as §7.10's R1–R4
  family (unchanged); `HeldDisposition`'s own read is now named exactly
  as a `player_bonus_held` balance read via `GrantLedgerAttribution`
  (N1.3 Component 3) — the same read shape `LockedExposure` already uses,
  applied to a different account, so this is confirmed to need no fifth
  query shape, not merely named as an open question.
- **`security` — REQ-SEP-BONUS-4 specified this round (N1.12), the rest
  confirmed.** `security`'s consolidated `W15.1.12` contract (adopted in
  N1.12 below) confirms N1.4.1 items 2/3 close `SEC-W15-08` (a `held`
  balance in a disjoint, non-fungible account can never leak into another
  Grant's wagering pool — this is now a structural, not merely
  discipline-based, guarantee) and specifies exactly how `pending_
  settlement`'s existing four-eyes/RBAC treatment extends to `bonus_held_
  dispositions` resolution: a **new, dedicated** permission,
  `bonus_held_disposition:resolve`, `SEP-1`-gated, four-eyes
  tenant-configurable above `CLAUDE.md`'s threshold (default 0) — per
  N1.12's own table, the ratified position (`architect`'s doc 34 §3.1,
  `security`'s self-correction, commit `6745e10`), not the superseded
  "threshold 0, always" reading — not folded into
  `bonus_adjustment:write`/`bonus_bulk:execute`.
- **`architect` — REQ-BONUS-VOL-1/DEP-EOI-1 specified this round (N2.4
  rewrite), the CRM/Segmentation/Affiliate items from Round 1 still open
  and unaffected by this round's work.** `architect`'s doc 34
  (`EconomicOperationIdentity`) is adopted at N2.4 for the grant-causing
  decomposition vector (SEC-W15-02); it has no bearing on N1's own
  terminal-state mechanics.
- **`qa`**: N1.6's six scenarios (the original three, Scenario 4's
  late-arrival case, Scenario 5's duplicate-delivery case, and this
  round's new Scenario 6 still-held-rollback case), plus N1.11's three
  named adversarial tests (`G2-HOLD-1/2/3`), are the minimum test set for
  this mechanism.

#### N1.11 Adversarial tests this revision adds (design-level; `qa` owns the implementation, per the fix wave's own dispatch split)

- **Test `G2-HOLD-1` — "no post-then-reforfeit path exists."** Setup: a
  Grant `G` with open exposure (either shape) whose `NewStakeEligibility`
  has already closed (`pending_settlement`, or fully terminal per
  Scenario 4). Deliver a WIN settlement correlated to that exposure.
  Assertion (must hold for **any** implementation, not just the
  reference design): there is no point in the resulting ledger history at
  which a credit to `player_bonus` (or `player_cash`) attributable to
  this settlement's `correlation_id` exists **unless** it is immediately
  preceded, in Progress-trail order, by a `bonus_held_dispositions` row
  for the identical `correlation_id`/`settlement_ledger_transaction_id`
  whose `status` transitioned away from `held` carrying a `resolution_
  reason_code` and (above threshold) a second approver. Concretely:
  assert that no single database transaction ever contains **both** a
  `player_bonus` credit for this `correlation_id` **and** a
  `bonus_forfeiture` debit for the same amount, unless a `bonus_held_
  dispositions.resolved_at` timestamp for that row strictly predates that
  transaction. This test is written specifically to **fail** against a
  reimplementation of the pre-revision mechanism (post-then-reforfeit in
  one transaction, no prior `bonus_held_dispositions` row at all) — the
  exact regression this fix wave exists to prevent.
- **Test `G2-HOLD-2` — "a held credit is never spendable or wagerable
  before resolution."** Setup: a `bonus_held_dispositions` row with
  `status = held`, `payout_amount = W`, under Grant `G`, for a player who
  also holds a second, unrelated Grant `G2` on the same `(player,
  asset_code)`. Assertions, each independently sufficient to fail the
  test: (i) a wagering-authorization check for a new stake under `G2`
  returns an available-funding amount that does not include `W`; (ii) `W`
  does not appear in Model C's computed `P_net`/`P_firm` for `G`, `G2`,
  or any other Grant; (iii) `W` does not appear in the player's
  withdrawable-cash projection; (iv) a direct attempt to construct a
  funding-split instruction referencing `W` as a `player_bonus` or
  `player_cash` source is rejected for lack of a source balance, because
  no such balance was ever created; (v) — **new, Round 2** — a live read
  of `player_bonus_held`'s balance for `G` equals exactly `payout_amount +
  released_lock_amount` for every open row, confirming N1.3 Component 3's
  redefinition (a balance read, not a row-existence check) is actually
  wired to the real account, not merely asserted in prose. This test is
  written specifically to **fail** the instant any implementation posts a
  held credit into `player_bonus` even transiently before G-2 resolves it
  (closing `SEC-W15-08`'s exact leak) — a fungible-account posting would
  pass (i)–(iii) only by accident of timing, and the test must exercise
  `G2`'s wagering concurrently with `G`'s hold, not merely assert
  statically, to catch that accident.
- **Test `G2-HOLD-3` — new, Stage 4H-B1 Wave 1.5 Fix Round 2 — "a
  still-held rollback voids exactly once, never resurrects exposure, and
  a losing race to resolve it is rejected, not silently applied."**
  Setup: a `bonus_held_dispositions` row `H`, `status = held`, under
  Grant `G` with no other open exposure (so `AOE(G,·) = {H}`,
  `pending_settlement`). Fire, concurrently: (a) a provider rollback
  naming `H`'s `settlement_ledger_transaction_id`, and (b) a staff
  resolution attempt (`ACTION_REFORFEIT`) on `H`. Assertions: (i) exactly
  one of the two succeeds — `H.status` ends at exactly one of `voided_by_
  rollback`/`resolved_reforfeit`, never both, never neither; (ii) the
  loser's own attempted `UPDATE ... WHERE status = 'held'` affects zero
  rows, and no compensating/duplicate posting results from the loser's
  attempt; (iii) if the rollback wins, `player_locked_bonus` for the
  original stake is **not** credited (the lock is never resurrected) and
  `G.status` flips `pending_settlement → expired` (or whichever
  `terminal_resolution` was recorded) in the **same** transaction as the
  void, with `AOE(G,·) = ∅` holding at every instant thereafter; (iv) a
  third, later attempt to resolve `H` (e.g. a stale queue entry) after
  either outcome also finds `status ≠ 'held'` and is rejected. This test
  is written specifically to **fail** against any implementation that
  checks `status = 'held'` and then updates in a separate statement
  (check-then-update) rather than a single guarded `UPDATE ... WHERE
  status = 'held'` — the exact regression HR-25's compare-and-swap exists
  to prevent.

All three named here for `qa`'s test-matrix ownership, not designed as
code by this section.

#### N1.12 `SEP-1` adoption (REQ-SEP-BONUS-1 … 4) — new, Stage 4H-B1 Wave 1.5 Fix Round 2, closing SEC-W15-03 on Bonus Engine's side

`security`'s `security-architecture.md` §W15.1.12 is the consolidated,
binding adoption contract; this section is Bonus Engine's own
implementation of it, naming the exact table/enforcement point/resolver/
permission at each of the four required points, per that section's own
instruction that Bonus states where it is checked, not the mechanism
itself (`SEP-1`'s trigger design, cardinality assertion, and step-0
tenant-scope self-proof are `security`'s, §W15.1.3/§W15.1.9, cited and
adopted here, never re-derived).

| Req | Enforcement point (this document's object) | Resolver | Permission | Four-eyes |
|---|---|---|---|---|
| `REQ-SEP-BONUS-1` | Grant issuance/activation (§1.3), direct bonus Adjustment, staff-forced conversion/manual release override, `BulkGrantJob` execution (W5) | Scalar (`expected_count = 1`): target Grant's/wallet's `player_account_id → player_accounts.person_id`, for issuance/activation/adjustment/forced-conversion. **Set**, for `BulkGrantJob`: the persons behind the pinned, materialized recipient set (`expected_count` = the pinned row count) | `bonus_grant:issue`, `bonus_adjustment:write`, `bonus_bulk:execute` (existing, unchanged, `security`-owned §B1.1) | Existing §B1.2 four-eyes, unchanged; `SEP-1` is additional |
| `REQ-SEP-BONUS-2` | `BonusSuggestion` (N3) review (Approve/Reject/Edit) and Activation | The persons behind the resolved `proposed_player_population` (N3.1), same pinned-set discipline as `BulkGrantJob` | `bonus_suggestion:review` (existing, N3's own permission dependency); Activation consumes the underlying Grant/`BulkGrantJob` permission (N3.2's "Activation **is** `BulkGrantJob.Create()`/`Grant.Issue()`" — the identical command surface, so `REQ-SEP-BONUS-1`'s own `SEP-1` check fires there too, not a second, parallel one) | Activation's own existing four-eyes (unchanged); review itself is not separately four-eyes-gated |
| `REQ-SEP-BONUS-3` | The household/linked-account **detection** path (`security`'s §W15.1.5, outside this document's own object model) | n/a — signal-only, never a block | n/a | n/a |
| `REQ-SEP-BONUS-4` (**new this round**) | `bonus_held_dispositions` **resolution** (N1.4 step 5c): `ACTION_REFORFEIT`, `ACTION_ROUTE_TO_CASH`, and the manual sub-choice under `ACTION_HOLD_FOR_REVIEW`. **Not** the row's **creation** (N1.4 step 5b — N1.8.1 row 7 classifies this TECHNICAL, "recording-and-parking a fact," never an authorizing write) | Scalar (`expected_count = 1`): the row's Grant's `player_account_id → player_accounts.person_id` — identical shape to `REQ-SEP-BONUS-1`'s adjustment/forced-conversion resolver, because a resolution moves value onto or off of exactly that Grant's player | **New, dedicated**: `bonus_held_disposition:resolve` — `security` mints and owns this permission (§B1.1 extension); this document states only where it is checked (the resolution write in N1.4 step 5c) and that it must **never** be folded into `bonus_adjustment:write` or `bonus_bulk:execute` (security's own argument, §W15.1.12: a `bonus_held_dispositions` resolution is a distinct economic act with its own volume/risk/reporting need, and folding it in would let existing adjustment-authorized staff resolve deferred dispositions without any role-wiring decision ever made about it) | **Yes, tenant-configurable, above `CLAUDE.md`'s threshold (default 0)** — ratified by `architect`'s doc 34 §3.1, with `risk`'s dissent recorded there rather than adopted; `security` has self-corrected its own earlier "threshold 0, always" text to match (commit `6745e10`; see paragraph below). Every resolution is `POLICY-DEPENDENT` (N1.8.1 rows 8/9/10), requiring a human G-2 answer, at least as material as an ordinary adjustment |

**Wiring — `security`-owned, this document only confirms it does not
conflict with anything Bonus Engine specifies.** `bonus_held_disposition:
resolve` is granted to `RoleBonusOperations` only (the same role holding
`bonus_adjustment:write`/`bonus_grant:cancel`) — never `RolePromotions
Manager`, `RoleTenantAdmin`, `RoleFinance`, `RolePlatformAdmin`. §B1.1's
hard constraint 3 ("no role may hold both `bonus_offer:manage` and
`bonus_adjustment:write`") extends verbatim to `bonus_held_disposition:
resolve`.

**Fail-closed, at every point above, per the human directive's
instruction not to invent identity data that does not exist.** Where a
target's `player_accounts.person_id` cannot be resolved — an unlinked or
malformed player account, a cross-tenant reference, any resolver error —
the operation is **refused**, never defaulted to "proceed without the
check" and never defaulted to "proceed as if no conflict exists." This
applies identically at Grant issuance/activation/adjustment (existing),
`BulkGrantJob` execution (existing), `BonusSuggestion` review/activation
(existing), and `bonus_held_dispositions` resolution (new) — no
enforcement point in this document's object model is exempt.

**Interaction with N2.4's `EconomicOperationIdentity` requirement, stated
so the two mechanisms are not conflated.** `SEP-1` (this section) answers
"is the actor a different person from every beneficiary?" `parent_
operation_id`/EOI (N2.4) answers "is this execution inside a resolvable,
approved, unexpired authorization, within its budget?" They are
orthogonal and both apply at Grant issuance/`BulkGrantJob` execution —
neither substitutes for the other, exactly as `security`'s own text
states `SEP-1` and four-eyes are orthogonal (§W15.1). doc 34 §3.1 mints a
dedicated `bonus_held_disposition_resolution` `operation_type` for the
identical resolution act `REQ-SEP-BONUS-4` gates — **both** the EOI check
(N2.4's entry rule, applied to this operation type) and the `SEP-1` check
above run at a `bonus_held_dispositions` resolution; neither is optional
because the other exists. **This document previously flagged, rather than
resolved, an inconsistency here between `security`'s and `architect`'s
source documents; that inconsistency is now ratified, not merely flagged,
and this document cites the ratified position rather than reconciling it
itself.** Earlier Round 2 text (`security`'s §W15.1.12, "threshold 0,
always") is superseded: `security` has since self-corrected that text
(commit `6745e10`) to match `architect`'s doc 34 §3.1, which formally
ratifies — with `risk`'s dissent recorded there, not adopted — that
`bonus_held_disposition_resolution`'s four-eyes control is
**tenant-configurable, above `CLAUDE.md`'s threshold (default 0)**, not
unconditional. This requires no change to N1.4 step 5c's own resolution
write, which already requires an approver above `CLAUDE.md`'s threshold;
`SEP-1` (this section) remains separately, and still unconditionally,
required regardless of amount — doc 34 §3.1's own reasoning is that
`SEP-1` alone already closes the self-dealing scenario the four-eyes
threshold would otherwise need to be unconditional to defend against, not
re-derived here.

### N2. Bonus targeting / bulk-assignment validation (gate §C)

Re-validated against Wave 1's domain model (W4 coded bonuses, W5 bulk
assignment, W7 segmentation) mode by mode, per the gate directive's ten
named modes. Two genuine corrections found (N2.2, N2.3); everything else
confirmed as already covered.

| # | Mode | Wave 1 coverage | Verdict |
|---|---|---|---|
| 1 | Player enters a code | W4 (T2 trigger mechanic) | Confirmed, no change |
| 2 | Staff assigns to one player | W5's `BulkGrantJob.target` already accepts a single `player_account_id`; ordinary §1.3 staff-action `issued` also applies directly for a one-off grant with no job overhead | Confirmed, both paths valid and equivalent in outcome |
| 3 | Staff assigns to a list | W5's `target` = explicit list | Confirmed, no change |
| 4 | Staff assigns to a segment | W5's `target` = one Segment + SegmentVersion reference (W7) | Confirmed, no change |
| 5 | Staff assigns to multiple segments | **Not covered** — W5's `target` shape names only "a single... Segment + SegmentVersion reference" | **Corrected, N2.2** |
| 6 | Bulk assignment (general) | W5 | Confirmed, no change |
| 7 | API-driven assignment | Actor-type mismatch found | **Corrected, N2.3** |
| 8 | CRM-driven campaign assignment (NEW) | Not previously addressed | **Specified, N2.4** |
| 9 | Automated eligibility-triggered grants | §1.3 (T1 trigger mechanic) | Confirmed, no change |
| 10 | Provider-native bonus coexistence | §3/§3.1/§3.2 | Confirmed, no new gap (N2.6) |

#### N2.1 Confirmed modes (1, 2, 3, 4, 6, 9, 10) — no further detail needed

Each already composes from an existing, named Wave 1 mechanism with no
new object, no new bypass, and no new state. Mode 10 specifically: doc10
§3's `external`/`inside_provider` split, ADR 0032 §6(c)'s posting rule,
and T5/C4's catalogue placement are unchanged by anything in this
section — CRM/Affiliate/segmentation additions (N2.4, N4.2) never touch
a Campaign whose `fulfillment_owner = external:<provider_id>`, because
that Campaign-level field is resolved once at configuration time (§3.1),
upstream of any targeting-mode question.

#### N2.2 Correction — segment-set targeting (mode 5)

`BulkGrantJob.target` (W5) is widened, additively, from a discriminated
union of `{single_player_account_id | player_list | segment_reference}`
to `{single_player_account_id | player_list | segment_reference |
segment_set}`, where `segment_set` is a bounded, explicit list of
`(segment_id, segment_version_id)` pairs. Resolution semantics: **union**
— a player who is a live member of **any** listed SegmentVersion at
job-run time (W5's own "live segment resolution, not a stale snapshot"
rule applies identically, per-segment) is included exactly once. Per-
player deduplication requires no new mechanism: `BulkGrantJobItem`'s
existing `UNIQUE (tenant_id, bulk_grant_job_id, player_account_id)`
constraint (W5) already guarantees one job-item row per player regardless
of how many listed segments matched them.

**Intersection ("in segment A AND segment B") is deliberately not built
here** — it is expressible today by asking whichever service owns
segmentation (W7's open placement question, `architect`'s parallel
Segmentation Engine doc) to define one compound SegmentVersion whose own
criteria composes both underlying predicates, which W7's AND/OR/NOT-
composable criteria shape (§W7) already supports without any Bonus
Engine change. Building AND-semantics into `BulkGrantJob.target` itself
would duplicate a capability the segmentation criteria language already
owns — an unnecessary second place the same logic could drift.

#### N2.3 Correction — actor type, verified against `internal/audit`

**W1's common object contract states `actor_type (system | player |
staff | provider)`. This is wrong, checked against the actual, already-
implemented canonical enum every domain's audit trail uses**:
`internal/audit.ActorType` (`internal/audit/audit.go:21-24`) is exactly
`{player, staff, service, system}` — **there is no `provider` value.**

**Corrected here**: every Bonus Engine object's `actor_type` field is
`internal/audit.ActorType`, verbatim, never a Bonus-local redefinition
(the same "never invent a parallel enum" discipline this document
already applies to jurisdiction codes, §7, and asset codes, §T.2).
Every place §1–§10/W1–W11/T.1–T.13 refers to a "provider" actor (an
external bonus engine's inbound status callback, §3.2; a provider-
originated free-round fulfillment confirmation) is `ActorService` — the
calling identity is the Reward Orchestrator's own service credential
that verified and relayed the callback, never a distinct "provider"
principal type. `causation_id`/metadata (which service credential,
which upstream system) — not a proliferation of `actor_type` values —
is what distinguishes one `ActorService` caller from another.

**This directly answers mode 7 (API-driven assignment) and mode 8
(CRM-driven, N2.4): both are `ActorService` callers of the ordinary
Grant-issuance/`BulkGrantJob`-creation surface**, distinguished from a
staff member's own session-backed call only by `actor_id` (which service
credential authenticated the request) and a `source_system` metadata
field on the resulting audit record — no new `actor_type` value, no new
targeting mechanism, no new bypass surface. `security`'s RBAC contract
(`4HB1-08`) governs which `ActorService` credentials may call which
Bonus Engine endpoints — this section states the object-model fact
(one existing enum, reused), not the authorization policy.

#### N2.4 CRM interface boundary (mode 8), specified without assuming CRM's internal design

Per the gate directive's explicit instruction, this specifies the exact
boundary CRM calls **into**, not CRM's own architecture (`architect`'s
parallel `4HB1W15-03` dispatch owns that). Two surfaces, both already
existing in this document's own design, neither modified in mechanism:

1. **Command surface — request a Grant or a bulk campaign.** CRM,
   having decided (by whatever internal logic it owns) that a player, an
   explicit list, or a segment should receive an Offer, calls the
   **identical** `BulkGrantJob`-creation surface (W5) or single-Grant
   staff-action-equivalent surface (§1.3) that a staff member's back-
   office UI already calls — as an `ActorService` caller (N2.3), with a
   caller-supplied `campaign_id`/`offer_version_id` and a `target` in
   any of N2.2's shapes (a single player, an explicit list, a segment,
   or a segment set). If CRM resolves its own audiences via the same
   shared `internal/segment` package `architect`'s Segmentation Engine
   doc is expected to define (task registry OI-3), CRM passes a
   `segment_id`+`segment_version_id` reference exactly as staff does —
   no CRM-specific target shape is invented. **T.1's full three-way
   gate, T.4's eligibility-axis snapshot, and W5's full per-item
   isolation/idempotency/resumability run identically regardless of
   caller identity** — a CRM-originated `BulkGrantJob` cannot bypass
   anything an identical staff-originated one could not also bypass.
   **Revised, Stage 4H-B1 Wave 1.5 Fix Round 2 — this is no longer the
   whole story; see below.**
2. **Query surface — `CheckOfferEligibility` (new, read-only, no
   financial or Grant-creating effect).** `CheckOfferEligibility(
   offer_version_id, player_account_id) → (eligible bool, reason)`
   composes the **same** live checks (`AssetAuthorization` →
   `rg.EvaluateEligibility` → `risk.Evaluate`, T.1's order) a real grant
   attempt would run, but writes no Grant row, no Progress entry, no
   ledger effect, and is not idempotency-keyed — a point-in-time preview
   whose answer is **explicitly non-binding**: T.4's "repeated live"
   rule still governs the real attempt regardless of what this preview
   returned. This exists so CRM can size/validate a campaign's audience
   *before* committing a `BulkGrantJob`, without Bonus Engine building a
   second eligibility engine — it is the identical T.1 gate, called
   read-only, once per candidate player, not a new decision surface.
   This is also the natural surface for a reviewer evaluating a
   `BonusSuggestion` (§N3) to preview likely eligibility before
   Approving. **Unaffected by the rewrite below** — it creates nothing,
   so it has no `parent_operation_id` to carry.

#### N2.4a `EconomicOperationIdentity` enforcement — closing SEC-W15-02/REQ-BONUS-VOL-1/DEP-EOI-1, Stage 4H-B1 Wave 1.5 Fix Round 2

**This is the single most important correction this round makes to this
document.** Round 1's own text for the command surface above ended: *"a
CRM-originated `BulkGrantJob` cannot bypass anything an identical
staff-originated one could not also bypass (i.e., nothing)"* — true as
far as it went, but silent about the **single-Grant** staff-action-
equivalent surface, which this document's own Round 1 N2.3 text described
as reachable by "an `ActorService` caller... with no parent-operation
precondition." **That sentence does not survive this rewrite.** It was
Bonus Engine's own uncorrected version of exactly the decomposition
vector `security` found in Phase 2 (SEC-W15-02): nothing before this
round stopped CRM (or any `ActorService` caller) from replacing one
5,000-recipient `BulkGrantJob` with 1,000 individually sub-threshold
single-Grant calls, each one passing T.1's per-item gate on its own
merits, with no mechanism anywhere measuring their sum.

**`architect`'s doc 34 (`EconomicOperationIdentity`) is adopted here,
verbatim, as the fix — not re-derived.** Per doc 34 §5.2's own division
of labor ("the domain whose tables change is the domain that checks" —
"Bonus creates the value, so Bonus holds the ceiling"), this document is
where that check lives; CRM, per doc 31, only carries the identity.

**The requirement, stated exactly.** Every call to the `BulkGrantJob`-
creation surface (item 1 above), the single-Grant staff-action-equivalent
surface (§1.3), and any future `ActorService`-caller path (N2.3) —
**every surface capable of causing a Grant to exist, regardless of
caller** — now requires a resolvable `parent_operation_id` before it is
honored. There are exactly two ways to obtain one, and no third:

- **(a) The call itself is a genuine, freshly-authorized human act, and
  mints a fresh root.** A staff member's single-Grant action, carrying
  its own §10-mandated reason code and (above `CLAUDE.md`'s threshold) a
  second approver, mints a fresh `bonus_manual_grant` root (doc 34 §3.1)
  at that instant. A `BulkGrantJob`'s own creation-time four-eyes
  approval mints a fresh `bonus_bulk_grant` root. **These are the only
  two mint points this document authorizes for a fresh root** — both are
  Bonus Engine's own, already-required, human-backed authorizations; no
  new approval step is invented, only a new record of it (the EOI row
  itself, doc 34 §2.2).
- **(b) The call relays an already-minted, `approved`, unexpired root or
  child from elsewhere.** A CRM `crm_engagement_campaign_activation`
  (doc 31, minted at that campaign's own activation approval), a
  `BonusSuggestion` Activation inheriting whatever upstream mint (if any)
  produced its proposal, or a child of a prior `bonus_bulk_grant`/
  `bonus_manual_grant` root — supplied as `parent_operation_id`.

**An `ActorService` caller — including CRM — can never itself mint a
fresh root.** It has no reason-code/four-eyes act of its own to back one;
doc 34 §3.1 names exactly this shape (`api_initiated_grant`, "the case
that has no human approval today, and therefore the case §5.1's rejection
rule is most important for"). A caller with neither path (a) available to
it nor a resolvable, `approved` parent under path (b) is **rejected
outright** — this is what closes the "1,000 individually-sub-threshold
single-Grant calls" vector: none of the 1,000 can mint its own root (only
a staff reason-code+four-eyes act or a job's own four-eyes approval can),
so every one of them must carry the same upstream parent, and that
parent's root-subtree-scoped budget (doc 34 §3.4) then bounds the sum of
all 1,000 regardless of which surface — `BulkGrantJob` or single-Grant —
each of them individually used. Per doc 34 §5.5's worked example, applied
here: the 5,001st actual grant execution across the whole subtree,
whichever call it falls under, finds `remaining_recipient_budget = 0` at
the locking consume (below) and is rejected, loudly, with the operation
id in the audit record. Splitting into many small calls, or between
`BulkGrantJob` and the single-Grant surface, changes nothing about the
aggregate ceiling enforced, because enforcement is keyed to the root,
never to the calling surface or the call's own declared size.

**The entry-check contract (doc 34 §5.1), applied at every surface named
above, non-locking, before anything else runs:**

| Condition | Result |
|---|---|
| No `parent_operation_id` supplied and no fresh root minted via path (a) | **Reject** |
| `parent_operation_id` does not resolve, or resolves in another tenant | **Reject** |
| Resolved EOI's `approval_state` is not `approved` (or `not_required` with a recorded determination) | **Reject** |
| `status` is not `open`, or `expires_at` has passed | **Reject** |
| This item's scope exceeds remaining budget (doc 34 §3.2's five containment checks: `recipient_ceiling`, `intended_aggregate_value`, `subject_set ⊆` parent's, matching `asset_code`, `expires_at ≤` parent's) | **Reject** |
| Any error resolving any of the above | **Reject — fail closed** |

**Canonical lock ordering (doc 34 §5.3), adopted verbatim — Bonus Engine
invents no ordering of its own.** T.1's existing gate chain
(`AssetAuthorization → RG → Risk`, including Risk's own
`pg_advisory_xact_lock`, unchanged and unreordered — N2.6 confirms this
below) is never interleaved with the EOI mechanism:

1. The non-locking entry check above runs **first**, before `AssetAuthorization`
   even starts — cheap, and rejects an obviously-unauthorized or
   obviously-oversized call before any live gate evaluates anything.
2. T.1's gate chain runs next, exactly as today, for every
   `BulkGrantJobItem`/single-Grant action individually (N2.5, unchanged).
3. **The locking EOI consume happens exactly once, strictly AFTER the
   gate chain completes, immediately before the item's own effecting
   write** (the `(none) → issued` transition, in the same transaction as
   that write) — never earlier, never held across the gate chain. It is
   always taken on the **root** operation row (doc 34 §3.4), via the
   `asset_change_consume_approved_request`-pattern atomic consume (doc 34
   §5.4): `SELECT ... FOR UPDATE` on the root, the four-eyes predicate
   inside the selection predicate, payload containment against the
   pinned scope, the state transition in the same transaction, `RAISE`
   on no match. Risk's advisory lock is always acquired before the EOI
   row lock, never after — automatic under this ordering, and this is
   what makes an AB-BA deadlock between the two locks structurally
   unreachable.

**Consumption-record declaration (doc 34 §3.4, RK-W15P2-4) — named, not
left implicit.** `bonus_bulk_grant`'s consumption record is
`BulkGrantJobItem` rows keyed by `parent_operation_id`, counted by
`player_account_id` (recipient budget), valued by `granted_amount` (value
budget) — doc 34's own worked example, confirmed to fit this document's
existing W5 schema with one additive column
(`BulkGrantJobItem.parent_operation_id`). `bonus_manual_grant`/
`api_initiated_grant`'s consumption record, for the single-Grant surface
which writes no `BulkGrantJobItem` row, is the `Grant` row itself
(additive `Grant.parent_operation_id` column) — one Grant, one
consumption row, counted and valued identically. A domain that later
writes a second consumption-row shape for the same `operation_type`
without declaring it must raise, never under-count (doc 34's own bound
behavior); this document does not anticipate a second shape today, so it
names none, per that same binding discipline.

**Retries, pages, and resumption inherit, never re-authorize (doc 34
§3.2/§3.3), composing with W5's existing mechanisms rather than
replacing them.** A `BulkGrantJob` resubmission (its own existing
`idempotency_key`, W5) resolves to the same EOI, never mints a fresh one.
A crashed/resumed job (W5's existing resumability) re-attaches to the
existing EOI; `BulkGrantJobItem`'s existing `UNIQUE (tenant_id,
bulk_grant_job_id, player_account_id)` rows remain the per-item record of
what has already been consumed, exactly as doc 34 §3.2's `resume` row
specifies. No new idempotency mechanism is introduced; the EOI is an
additional, orthogonal authorization gate over the identical, unmodified
mechanism.

**No shortcut exists through either N2.4 surface.** Both terminate in the
same `(none) → issued` pipeline every other trigger uses (W4's own
"a code is only a different `trigger_reference` value feeding that one
existing transition row" reasoning, generalized to every targeting
mode): `AssetAuthorization → RG → Risk`, the eligibility-axis snapshot,
§9's idempotency key, and — new this round — the resolvable, `approved`,
in-budget `parent_operation_id` this section requires. CRM is a caller,
never a parallel authority, and — as of this rewrite — never a
volume-decomposing one either.

#### N2.5 Bulk-operation requirements — re-confirmed

- **Idempotent** — job-level `idempotency_key` (W5); a resubmission of
  the identical job spec is a no-op. ✓ unchanged.
- **Resumable** — a crashed/resumed job re-walks its target list,
  skipping every player with an existing `BulkGrantJobItem` row of any
  outcome (W5). ✓ unchanged, and N2.2's `segment_set` widening does not
  change this: the resolved member list (now a union across segments) is
  walked identically.
- **Per-player eligibility-checked** — every `BulkGrantJobItem`
  independently runs the full T.1 gate (W5). ✓ unchanged.
- **Tenant/brand-scoped** — `BulkGrantJob`/`BulkGrantJobItem` inherit the
  ordinary Grant RLS pattern (W5, §8); never platform-wide, mirroring
  Grant. ✓ unchanged.
- **Permission-controlled** — W5 does not itself define an RBAC role
  taxonomy; this is confirmed as `security`'s named dependency (`4HB1-08`,
  already listed in W11), not a Bonus Engine design gap: the object model
  (an ordinary, API-authenticated, audited action) does not preclude
  whatever role gate `security` attaches to `BulkGrantJob` creation/
  approval, for either a staff or an `ActorService` caller.
- **Auditable** — `audit.Record` at creation, approval/rejection, start,
  completion, and per-item outcome (W5). ✓ unchanged.
- **Duplicate-safe** — job-level idempotency key plus per-item DB
  uniqueness (W5). ✓ unchanged, and N2.2's union-of-segments resolution
  is itself duplicate-safe by the same per-item constraint (a player
  matched by two segments still gets exactly one item row).
- **Independently reportable** — `BulkGrantJob`/`BulkGrantJobItem`'s
  stable ids and per-item outcome make "who was targeted, under what
  authority, and what happened to each" answerable by an ordinary,
  RLS-scoped query with no join to a mutable status table. ✓ confirmed,
  not previously stated explicitly in W5 — recorded here as a
  confirmation, not a new mechanism.
- **Budget-bounded across the whole authorization, not per call — new,
  Round 2 (N2.4a).** Every `BulkGrantJobItem`'s effecting write, and
  every single-Grant action's, now also consumes against its
  `parent_operation_id`'s root-subtree-scoped `recipient_ceiling`/
  `intended_aggregate_value` (doc 34 §3.4), atomically, immediately
  before that write — this is additional to, not a replacement of, the
  per-item T.1 gate above.

#### N2.6 No-bypass guarantee, re-confirmed across every mode including the two new ones

W5's core guarantee — "bulk assignment is `N` individual `issued`
transitions sharing one job correlation id, never a batch-level bypass"
— is unchanged by N2.2 (segment-set is a resolution-time widening of
which players end up as job items, not a change to how each item is
processed), N2.3 (a new caller identity, not a new code path), or N2.4
(CRM is a caller of the same command surface, now additionally subject
to N2.4a's `EconomicOperationIdentity` requirement — see immediately
below). **KYC/jurisdiction/licensing/player-status are not a fourth live
gate distinct from RG/Risk/`AssetAuthorization`** — KYC/RG-level and
jurisdiction-Offer-availability are part of the Offer's eligibility-axis
snapshot (T.4, checked once at `issued`, for every mode including
bulk/CRM/API-driven); jurisdiction-asset-authorization and
jurisdiction-scoped Risk hard-limits are folded into the live
`AssetAuthorization`/`risk.Evaluate` calls (§4/§7, T.1) that every
`BulkGrantJobItem` runs individually.

**The `EconomicOperationIdentity` check (N2.4a) is likewise not a fourth
live gate in T.1's sense — it is an additional, orthogonal authorization
precondition, evaluated non-locking before the gate chain and consumed,
locking, strictly after it (doc 34 §5.3).** It changes *whether the call
is authorized to attempt a grant at all* and *how much aggregate volume
that authorization still has remaining*; it never changes, weakens, or
substitutes for T.1's own eligibility/risk/asset-authorization decision
for the individual item. **No mode named in this section reaches
`issued` by any path other than the one T.1/T.4/§9/N2.4a now, together,
fully specify** — and, per N2.4a's own closing argument, no mode can
reach `issued` by decomposing a large authorized volume into many small,
individually-authorized-looking calls either, which is the exact gap
this round closes that T.1/T.4/§9 alone did not.

### N3. Bonus Suggestion full specification (gate §D)

Elevates W6's sketch into a full object/lifecycle specification. Nothing
here changes W6's central, load-bearing property: **`BonusSuggestion`
never creates a Grant, never moves money, and never calls RG/Risk/
`AssetAuthorization`'s value-moving checkpoints.**

#### N3.1 Object shape — every field the directive names, with a concrete home

| Field | Meaning | Immutable once written? |
|---|---|---|
| `id` | server-generated UUID | yes |
| `tenant_id`/`brand_id` | scope, never platform-wide (mirrors Grant, §8) | yes |
| `status` | `Generated`/`UnderReview`/`Approved`/`Rejected`/`Edited`/`Activated`/`Discarded` — explicit enum, W1's common contract | no (the one mutable read-model field, N3.2) |
| `originating_kind` | `rule` \| `model` \| `manual` — structurally distinguishes the source, never inferred | yes |
| `originating_rule_id` / `originating_model_version` / `originating_staff_actor_id` | the one populated per `originating_kind` | yes |
| `generated_at` | `clock_timestamp()` | yes |
| `proposed_config` | the inert, structured proposal (below) | yes — a review's *changes* are captured as a diff (N3.2), never an in-place edit of the original proposal |
| `reason` | free text and/or a structured signal summary — why this suggestion exists | yes |
| `reviewer_id` | who claimed/decided | set once claimed |
| `review_claimed_at` | `clock_timestamp()` | set once |
| `decision` | `Approved`/`Rejected`/`Edited` | set once per review round |
| `decided_at`/`decided_by` | when/who | set once per review round |
| `modifications` | for `Edited`: field-by-field diff, `{field: (proposed_value, reviewer_value)}` | append-only per edit round |
| `rejection_reason` | mandatory if `Rejected` (mirrors §10's reason-code discipline) | yes |
| `activated_at` | `clock_timestamp()` | set once |
| `resulting_reference` | the `campaign_id`/`offer_version_id`/`grant_id` or `bulk_grant_job_id` Activation produced | set once, bidirectional with that object's own `originating_suggestion_id` (W6) |
| `discarded_at`/`discard_reason` | terminal, no financial artifact | set once |

`proposed_config` carries exactly what the gate directive names, each
field a direct reference into an already-specified Wave 1 shape — a
suggestion invents no new configuration vocabulary:

- `proposed_bonus_type` — one of W3's canonical-mechanic codes (trigger/
  reward/completion), or a named catalogue item (§N4).
- `proposed_offer_reference` — nullable: an existing Offer to reuse, or
  null meaning "propose a new Campaign/Offer."
- `proposed_reward` — amount/percentage/cap/`reward_kind` (W2.6's shape).
- `proposed_wagering_requirement` — multiplier/contribution weights
  (W2.7's shape).
- `proposed_target_segment` — a `segment_id`+`segment_version_id`
  reference (W7's "by reference, never inlined" rule, unchanged).
- `proposed_campaign_reference` — nullable, existing Campaign to attach
  to vs. propose a new one.
- `proposed_timing` — proposed start/end window, or a trigger-condition
  description for a signal-driven proposal.
- `proposed_player_population` — reuses N2.2's exact `target` shape
  (single player / list / segment / segment set) — a suggestion's
  population is not a fifth targeting concept.
- `proposed_activation_strategy` — one of `grant_policy`'s existing
  values (W2.2: `auto_issue`/`manual_approval_required`/`code_redeemed`/
  `external_signal`/`manually_assigned`) — a suggestion never invents a
  sixth.

#### N3.2 Lifecycle, formalized as an append-only review trail

Mirroring `GrantActivation`'s own pattern one level down in stakes: the
`BonusSuggestion.status` column is a **read-model pointer**, not the
authoritative history. The authoritative record is an append-only
`SuggestionReviewEvent` per transition (`Generated` is the creation event
itself; each of `UnderReview`/`Approved`/`Rejected`/`Edited`/`Activated`/
`Discarded` is its own row, actor/timestamp/reason carried per §10's
audit discipline). This is not a new mechanism — it is the identical
"typed projection over an append-only trail" pattern W2.4 already
establishes, applied to a non-financial object, so a reviewer's full
edit/re-review history is never lost to an in-place status-column
overwrite.

- **Generated**: `proposed_config` frozen at this instant; never mutated
  in place thereafter.
- **UnderReview**: `reviewer_id` claims the queue entry; no RG/Risk/
  `AssetAuthorization` call happens here — there is nothing to gate, no
  money moves. A reviewer **may** use N2.4's `CheckOfferEligibility`
  query surface as an informational preview of the proposed population's
  likely eligibility; this is a UX aid, never a gate — the real gate
  runs, unconditionally, at Activation's ordinary Grant-issuance path,
  exactly as for every other targeting mode (T.4's "repeated live" rule
  applies here with no carve-out).
- **Approved**: `reviewer_id`/`decided_at` set; the suggestion may
  proceed to Activation with `proposed_config` unchanged.
- **Rejected**: terminal for this round; `rejection_reason` mandatory.
- **Edited**: the specific modified fields are diffed against
  `proposed_config` (never a silent overwrite — W6's own "a suggestion
  silently approved-with-private-edits never happens" rule, restated as
  a concrete diff structure) and the suggestion returns to an
  `UnderReview`-equivalent state carrying the modified proposal,
  requiring its **own** subsequent `Approved`/`Rejected` decision — an
  edit is never itself a terminal approval. This can iterate (edited
  again), each round its own `SuggestionReviewEvent`.
- **Activated**: reachable only from `Approved` (directly, or via one or
  more `Edited` rounds each themselves `Approved`). Activation means
  **exactly one thing**: the approved `proposed_config` is submitted
  through N2.4's **ordinary, unmodified** command surface (single Grant,
  or `BulkGrantJob`, per `proposed_player_population`'s shape) —
  architecturally, `BonusSuggestion.Activate()` **is**
  `BulkGrantJob.Create()`/`Grant.Issue()` called with an extra
  `originating_suggestion_id` parameter, not a second, parallel code
  path. The resulting object carries `originating_suggestion_id` back to
  this suggestion (W6); this suggestion's own `resulting_reference`
  points forward — bidirectional, so either record answers "why does
  this Grant/campaign exist" or "what did this suggestion produce."
- **Discarded**: terminal, no financial artifact, reason optional but
  recorded if given.

#### N3.3 The structural (not promised) non-financial guarantee

Three independent facts make "Suggestion != Grant" true by construction,
not by discipline, restated and strengthened from W6:

1. **No write path to ledger/wallet tables.** No code reachable from any
   `BonusSuggestion` state transition calls `internal/ledger`/
   `internal/wallet`. Only Activation, via N2.4's command surface, can
   ever reach the ledger — and that surface is the identical one every
   other targeting mode already reaches it through.
2. **No write path to `Grant`/`Progress` other than through Activation's
   call into the ordinary issuance pipeline.** There is no
   `BonusSuggestion`-specific Grant-creation function; Activation's
   *only* mechanism is "call the surface a human/API/CRM caller would
   otherwise call directly."
3. **RBAC-enforceable at the credential level, named as a `security`
   dependency, not designed here**: a suggestion-generator service
   identity (a rules engine today; a future scoring/ML model, explicitly
   out of scope per W6/N3.4) is scoped to `bonus_suggestion:create` only
   — it holds no credential capable of calling `bonus_grant:create` or
   `BulkGrantJob:create` directly. This is what makes the guarantee hold
   for a **future** AI/CRM/recommendation system too: such a system can
   only ever populate `Generated` rows; it structurally cannot move
   money, no matter how it is built, because it is never issued the
   credential that could.

#### N3.4 Still out of Wave 1/1.5 scope, restated

The generator that *populates* `Generated` rows (a scoring model, an LTV
heuristic, a CRM-triggered rule) remains explicitly out of scope, per
W6's own "building a generator ahead of a concrete first consumer" scope
test. This section specifies the object and lifecycle a future generator
would populate — never the recommendation logic itself.

### N4. Full bonus catalogue validation (gate §H)

Re-verified against W3's canonical-mechanics catalogue, category by
category, per the gate directive's full list. Every category composes
from W3's existing three-choice decomposition (Trigger × Reward ×
Completion) with **zero new financial mechanisms** — confirmed, not
merely asserted, in the table below. Two items needed correction (the
Affiliate split, N4.2) or a cross-reference addition (Retail, N4.3); the
rest are direct restatements of W3's own already-frozen mapping.

| Category | W3 row | Verdict |
|---|---|---|
| Welcome / First Deposit | "Welcome / first-deposit" | Confirmed |
| Multi-deposit | "Multi-deposit" | Confirmed |
| Reload | "Reload" | Confirmed |
| Cashback | "Cashback / loss-back" | Confirmed — see below |
| Loss-back | Same row as Cashback | Confirmed — "loss-back" and "cashback" are the identical R1-against-net-loss/C2 mechanic under two commercial names; no functional distinction exists in this platform's rule shape, and none needs to |
| No-deposit | "No-deposit" | Confirmed (C1-shaped variant only, in slice; C3-shaped variant deferred per Stage 4H-B0's own scope note, unchanged) |
| Free Spins / Free Rounds | "Free spins / free rounds" | Confirmed, deferred (fulfillment not built) |
| Free Bets | "Free bets" | Confirmed, deferred (blocked on `internal/sportsbook`) |
| Sportsbook bonus | "Sportsbook bonuses" | Confirmed, deferred (blocked on `internal/sportsbook` existing at all) |
| Casino bonus | "Casino bonuses" | Confirmed — composes from Deposit/Reload/Cashback/Wagering/Free-spin, no new mechanic |
| Wagering / Turnover bonus | "Wagering / turnover bonuses" | Confirmed |
| Coupon | "Promo codes / voucher / bonus codes" | Confirmed |
| Promo Code | Same row | Confirmed — naming variance only |
| Voucher | Same row | Confirmed, with one cross-reference: `docs/architecture/26-retail-operations-architecture.md`'s own vocabulary explicitly excludes "voucher" from its retail-instrument taxonomy (line 25) — a retail voucher and a Bonus Engine "voucher-named" coded bonus are **not the same object**; this section's "Voucher" row is the coded-bonus sense only, never the retail cash-instrument sense, and this document does not (and should not) resolve that naming collision — it is flagged so a future reader does not conflate them |
| Bonus Code | Same row as Coupon | Confirmed — naming variance only |
| Manual Bonus | "Manual bonuses" | Confirmed, C1-shaped variant only in slice; see N4.3 for one open item |
| Compensation / Goodwill | "Compensation / goodwill" | Confirmed — "same flag as manual bonuses" |
| VIP / Loyalty | "VIP / loyalty" | Confirmed — T3-shaped (manual VIP-desk grant) in-slice-composable; T4-shaped (points/loyalty redemption signal) deferred, blocked on Points/Gamification (doc 24) |
| Retention | "Retention / reactivation" | Confirmed — T3-shaped (manual) variant only; automated dormancy-trigger variant needs a new event producer, unbuilt, unchanged from W3 |
| Reactivation | Same row | Confirmed |
| Birthday / Anniversary | "Birthday / anniversary" | Confirmed — T3-shaped variant only |
| Referral | "Referral / affiliate" — **split, N4.2** | Corrected |
| Affiliate acquisition (NEW) | Same row, split — **N4.2** | Specified |
| Tournament / competition reward | "Tournament / competition rewards" | Confirmed, deferred (blocked on Gamification) |
| Mission / challenge reward | "Mission / challenge rewards" | Confirmed, deferred |
| Points / XP-triggered reward | "Points / XP-triggered rewards" | Confirmed, deferred (blocked on Points accounting, doc 24) |
| Provider-native bonuses | "Provider-native bonuses" | Confirmed — T5/C4, §3/§3.2 |

#### N4.2 Splitting Referral from Affiliate acquisition, and the Affiliate pipeline's interface boundary into Bonus Engine

W3's single "Referral / affiliate" row conflated two distinct producers
that happen to compose identically once a trigger reaches Bonus Engine.
**Split, additively, into two rows sharing one mechanic**:

- **Referral** (in-house "refer a friend"): `T4` (an internal
  referral-confirmation signal — doc 22's `RewardTriggerSignal`, W10 —
  or `T3` for a manual staff grant), `R2` reward, `C1`/`C3` completion,
  composing from the Wagering-bonus or Cash-reward rows exactly as W3
  already states. Unchanged.
- **Affiliate acquisition** (NEW — per the gate directive's own
  requirement to cross-reference `architect`'s parallel Affiliate
  architecture's expected pipeline: **Affiliate → Attribution →
  Segmentation → CRM → Bonus Offer → Bonus Grant**). Bonus Engine's
  position in that pipeline is the **last link only** — it never talks
  to Affiliate or Attribution directly. Concretely:

  > **Interface boundary (specified here, without assuming Affiliate's
  > internal design)**: Affiliate/Attribution's acquisition decision
  > reaches Bonus Engine exclusively through **CRM**, via the identical
  > two surfaces N2.4 already specifies for any CRM-driven assignment —
  > `CheckOfferEligibility` (a preview) and the command surface
  > (single-Grant or `BulkGrantJob`, `ActorService`-authenticated). If
  > CRM chooses to express "acquired via affiliate X" as Segmentation
  > membership (the pipeline's own stated order places Segmentation
  > before CRM), CRM passes a `segment_id`+`segment_version_id`
  > reference exactly as any segment-targeted assignment already does
  > (N2.2). If CRM instead resolves an explicit player list itself, it
  > passes that list. **Bonus Engine's own model requires no new field,
  > no new trigger mechanic, and no new object to receive this**: from
  > Bonus Engine's side, "CRM relaying an Affiliate-attributed
  > acquisition decision" and "CRM relaying any other campaign
  > decision" (N2.4) are the identical call, distinguished only by
  > `source_system`/metadata on the resulting audit record, never by
  > code path. This composes as `T3`/`T4` + `R2`/`R1` + `C1`/`C3` from
  > the already-existing Wagering-bonus/Manual-bonus/Cash-reward rows —
  > **zero new financial mechanism**, exactly as the gate directive asks
  > to be confirmed.

  Attribution-specific commercial terms (e.g., a different reward size
  per affiliate, a cost-attribution to the affiliate's own commission
  ledger rather than `bonus_expense`) are an Affiliate/CRM-domain
  question about **which** Offer/Campaign CRM selects and **how the
  affiliate's own commission accounting works** — not a Bonus Engine
  concern; Bonus Engine treats an affiliate-driven Grant exactly as
  operator-funded unless a `funding_source = provider:<id>` value (W2.6,
  ADR 0032 §6) is explicitly configured on the Offer, identical to any
  other provider-funded Grant question already named in Wave 1 (finding
  BF-1, task registry).

#### N4.3 Cross-reference — cashier-issued manual bonus (Retail)

`docs/architecture/26-retail-operations-architecture.md` line 827
already names an `OPEN DECISION` for `bonus-engine`: "whether a bonus
may ever be *issued by a cashier* (an obvious collusion vector)." This
is a genuine, standing open item under the "Manual Bonus" row above —
not resolved by this section, and not silently assumed either way. If
ever authorized, a cashier-issued Grant would be `actor_type = staff`
(N2.3), subject to whatever additional retail-hierarchy control
`docs/decisions/0036-retail-hierarchy-rbac-and-audit.md` and `security`
require on top of the ordinary staff-manual-grant path (W5's four-eyes
placeholder, §10) — named here as the item's current status, carried
forward, not newly invented.

#### N4.4 Confirmation

Every category the gate directive lists composes from W3's existing
Trigger × Reward × Completion decomposition, the segmentation/targeting
shapes in §N2, and (where relevant) the Suggestion object in §N3 as an
optional upstream proposal mechanism that itself produces nothing until
Activation. **No category requires a bespoke financial implementation,
a new ledger transaction type beyond `bonus_grant`/`bonus_conversion`/
`bonus_forfeiture`/`bonus_reversal` (§7.3), or a new bypass of RG/Risk/
`AssetAuthorization`/eligibility.** The two corrections in this section
(N4.2's split, N4.3's cross-reference) are naming/provenance
clarifications and a carried-forward open item, not new mechanism.

### N4.5 Round 2 consistency re-verification — catalogue, targeting, and Suggestion lifecycle against A/B/C above (gate directive item D)

A consistency check, not new design — confirming N2 (targeting), N3
(`BonusSuggestion`), and N4 (catalogue) still hold given this round's
holding-representation rewrite (A), `SEP-1` adoption (B), and EOI
adoption (C).

- **Catalogue (N4).** **Unaffected.** Every category in N4's table
  composes from W3's Trigger × Reward × Completion decomposition; nothing
  in A/B/C adds, removes, or reshapes a financial mechanism — A changes
  *where* a held WIN's value sits and *which account* represents it,
  never *which bonus type* exists; B and C change *who may authorize* a
  grant-causing or resolution act, never *what* that act produces. N4.2's
  Affiliate-acquisition interface boundary and N4.3's cashier cross-
  reference are likewise untouched — both route through N2.4's command
  surface, which now additionally requires an EOI parent (C), but neither
  section's own content changes because of that.
- **Targeting modes (N2.1–N2.3, N2.5–N2.6).** **Unaffected in mechanism,
  confirmed above (N2.5's new bullet, N2.6's rewritten closing
  paragraph) to now additionally require and enforce an EOI parent at
  every mode's shared `issued` pipeline** — this is a strengthening of
  the existing no-bypass guarantee, not a change to which modes exist or
  how each resolves its target population. N2.2's `segment_set` union
  resolution and N2.3's `ActorService`/`actor_type` correction are both
  untouched by A/B/C.
- **N2.4 (CRM interface boundary).** **Changed, precisely as this
  round's own N2.4a rewrite states** — the command surface now requires a
  resolvable `parent_operation_id`; the query surface
  (`CheckOfferEligibility`) is unaffected, since it creates nothing.
- **`BonusSuggestion` lifecycle (N3).** **Unaffected in shape, one
  precise consequence noted.** `Generated → UnderReview → {Approved →
  [Edited]* → Activated} | Rejected | Discarded` (N3.2) is untouched —
  Activation still means exactly "submit the approved `proposed_config`
  through N2.4's ordinary command surface" (N3.2's own "Activation **is**
  `BulkGrantJob.Create()`/`Grant.Issue()`" identity). The one precise
  consequence: since that command surface now requires an EOI parent
  (N2.4a), an Activation with no upstream-supplied parent is itself
  Activation's own qualifying human act — the reviewer's `Approved`
  decision (N3.2, itself reason-coded and, for the underlying Grant/job,
  already four-eyes-gated per §B1.2) **is** the reason-code+approval act
  that mints a fresh `bonus_manual_grant`/`bonus_bulk_grant` root at
  N2.4a's path (a), exactly as an ordinary staff action would — no new
  approval step, no new object, and N3's own "Suggestion != Grant by
  construction" guarantee (N3.3) is unaffected, since Activation still
  reaches the ledger through no path but the one N2.4a now gates. `SEP-1`
  (`REQ-SEP-BONUS-2`, N1.12) applies at Suggestion review/Activation
  exactly as N1.12's table states — orthogonal to, and unaffected by,
  this EOI consequence.

### N5. Summary of corrections to Wave 1, and cross-domain dependencies for the Orchestrator's reconciliation

**Corrections made to this specialist's own Wave 1 work, stated
plainly**:

1. W1's `actor_type` enum was wrong — checked against
   `internal/audit.ActorType` and corrected (N2.3). This also resolved
   two of the gate directive's ten targeting modes (API-driven, CRM-
   driven) using an existing primitive instead of a new one.
2. W5's `BulkGrantJob.target` shape did not support multi-segment
   targeting — corrected additively (N2.2).
3. §T.7's "rare fourth entry path" (`converted` racing an in-flight
   settlement) was asserted, not built — now covered by the same
   mechanism as expiry/cancellation/forfeiture (N1.4 Path A).
4. W3's "Referral / affiliate" row conflated two distinct producers —
   split, with the new Affiliate pipeline's interface boundary specified
   (N4.2).
5. Wave 1 did not check `docs/decisions/0038`'s "settlement win/loss
   happy paths" language against casino's actual three-callback-type
   protocol (`bet`/`win`/`rollback`, no loss-confirmation event) — doing
   so surfaced N1.7's genuine, disclosed residual gap rather than
   assuming symmetry between the two provider shapes.
6. **Stage 4H-B1 Wave 1.5 Fix Round 2.** This document's own N1.4.1 item
   2 two-branch account-type split, N1.4.1 item 4's `bonus_g2_hold:
   <correlation_id>` idempotency key, and the `HeldDispositionRecord`
   field list (old N1.9) are all retired and replaced by
   `ledger-finance`'s/`casino`'s finalized, binding contract (N1.3, N1.4,
   N1.9). This document's own N2.4/N2.3 text left the single-Grant
   staff-action-equivalent surface reachable by an `ActorService` caller
   with no volume-bounding precondition at all — corrected by N2.4a's
   `EconomicOperationIdentity` adoption.

**Cross-domain dependencies, named for the Orchestrator's reconciliation
(none blocking this section's own conclusions)**:

- **`casino`** (`4HB1W15-01`/Round 2) — **CLOSED this round** for the
  hold-capture posting (now confirmed unconditional), the widened
  `ResolveTerminalGrantCredit` seam, the still-held rollback transition,
  and — new this final closing pass — the `RecheckGrantExposure`
  Grant-status-finalization seam (N1.4.2, N1.10). Still open: `08
  §16.10.1`'s locked-shape recommendation for a future bonus-funded
  casino bet, `08 §16.10.3`'s flagged tension, and **LF-10** (rollback of
  an already-resolved disposition, N1.10) — all routed, none this
  document's to decide.
- **`ledger-finance`** — **CLOSED this round** for the account/schema
  decision, the idempotency key, the reconciliation stream, and the
  `ACTION_REFORFEIT`/`ACTION_HOLD_FOR_REVIEW` framing corrections (N1.10).
  Confirmed, not merely re-asked: N1.3's `AOE` reads need no query shape
  beyond §7.10's R1–R4 family.
- **`architect`** (`4HB1W15-03`, and doc 34 this round) — Segmentation
  Engine placement (N2.2's `segment_set` reuse, N2.4's CRM-via-
  `internal/segment` path), the CRM Engine's own architecture (N2.4/N4.2
  specify only the boundary Bonus Engine exposes, not CRM's internals),
  and the Affiliate Engine's pipeline (N4.2) — all still open, unaffected
  by this round. **New this round, closed on Bonus Engine's side**: doc
  34's `EconomicOperationIdentity` is adopted at N2.4a; the table-text
  inconsistency this document previously flagged between doc 34 §3.1 and
  `security` §W15.1.12 over `bonus_held_disposition_resolution`'s
  four-eyes threshold is, as of this final closing pass, **ratified, not
  merely flagged** — doc 34 §3.1's "tenant-configurable, above
  `CLAUDE.md`'s threshold, default 0" reading is the cited position at
  N1.12, `security` having self-corrected to match.
- **`security`** — **REQ-SEP-BONUS-1–4 specified this round** (N1.12);
  RBAC for the two command-surface callers (`ActorService`, N2.3/N2.4),
  the `BulkGrantJob` permission gate (N2.5), and the suggestion-generator
  credential scoping (N3.3) remain open, unaffected by this round's work.
  `bonus_held_disposition:resolve` is a new permission `security` mints
  and owns (N1.12).
- **`qa`** — N1.6's now-six scenarios and N1.11's now-three named
  adversarial tests (`G2-HOLD-1/2/3`) as the minimum test set for the
  terminal-state invariant; N2's ten targeting modes, now including
  N2.4a's EOI entry-check/consume behavior, as a test-matrix input; N3's
  full suggestion lifecycle (including multi-round Edit) as a
  completeness-trail test target, mirroring Progress-trail-completeness
  testing one level down in stakes.

Owner of this section: `bonus-engine`. Nothing in this section
authorizes writing `internal/bonus`, a migration, or a test. No Human
Decision Register item is selected.

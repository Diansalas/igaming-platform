# 0040 — Stage 4H-B1 Wave 3 — cross-domain composition rulings and recorded design constraints

Status: **Accepted** for the decisions marked `DECISION`; **Recorded,
not decided** for the items marked `CONSTRAINT` (they bind whoever
implements the named work later, but authorize no work now);
**Confirmation only** for the items marked `CONFIRMED`.

Owner: `architect`. Produced by Stage 4H-B1 Wave 3 Phase 10, the
cross-domain composition certification phase, after reading Wave 3's
entire diff (`9d857dc..8c6ab7a`) as one integrated change rather than
phase by phase. Every factual claim below was checked against the code
at that range, not taken from a prior phase's self-report.

This document exists because several Wave 3 findings are genuinely
cross-domain: they were explicitly routed to `architect` by `risk`
(Phase 4) and `security` (Phase 6) rather than decided inside one
domain, or they only become visible when the whole Wave is read at once.
Recording them here keeps them out of the individual domains' documents
(which their owners maintain) while making them binding and findable,
per `CLAUDE.md`'s "cross-cutting changes go through the architect and are
recorded in `docs/decisions/`" rule.

**What this document does not do.** It resolves no Human Decision
Register item (ADR 0039's three; `OpenBetSelfExclusionPolicy`; the
mixed/bonus-funded sportsbook cashout policy; FD-1). It authorizes no
stage transition. It makes no commercial, legal or licensing decision.

---

## D1 — `DECISION`: a root authorization for a value-creating operation type may never be unbounded

**Context.** `security`'s Phase 6 review (`security-architecture.md`
§W3P6.5 item 1) found, and routed here rather than changing
unilaterally: `economicop.ConsumeRootBudget` treats a nil
`recipient_ceiling` as "no recipient check at all" and a nil
`intended_aggregate_value` (on a non-null `asset_code` root) as "no value
check at all". Wave 3's new minting HTTP handler
(`newMintEconomicOperationHandler`) requires all three bounds, but
`bonus.MintRootOperation` — doc 34 §3.1's declared mint point for **both**
Bonus operation types, and the mechanism's own entry point — accepted nil
for all of them. A bound enforced in one transport is not a bound: any
future non-HTTP minting path (a job runner, a CRM adapter, a fixture
promoted to production code) silently reopens the SEC-W15-02
decomposition vector one layer below the control that exists to close it.

**Decision.** The bound is structural. `internal/economicop` now declares
`boundedRootOperationTypes` and exports
`ValidateRootAuthorizationBounds`, called by `MintRootOperation` before
the idempotency lookup (so an unbounded request is refused on its own
merits, never quietly resolved to a pre-existing bounded EOI). For a
declared type, a root must carry a non-empty `asset_code`, a positive
`intended_aggregate_value`, a positive `recipient_ceiling` and a non-zero
`expires_at`; `single_subject` additionally requires `subject_ref` and a
ceiling of exactly 1; `subject_scope = none` is refused outright.

**Why permit-by-enumeration rather than a blanket rule.** Doc 34 §2.2
states plainly that a null `asset_code` is legal and meaningful (a
non-monetary operation has no asset) and RK-W15P2-5 states such a root has
**no enforceable value budget at all**. A blanket rule would contradict the
document it enforces. The rule is therefore per-`operation_type`, over
exactly the types that create player-redeemable value and already carry a
declared consumption shape (`bonus_manual_grant`, `bonus_bulk_grant`,
`api_initiated_grant`).

**Consequence, binding on the next consumer.** A new value-creating
`operation_type` needs **two** declarations in the same change: a
`consumptionShapes` entry (so `ConsumeRootBudget` can count it) and a
`boundedRootOperationTypes` entry (so its roots cannot be unbounded). One
without the other is a half-built control. Recorded normatively in doc 34
§5.7.

**Status: `IMPLEMENTED`** (commit `5af05d2`), with regression coverage
proven to fail pre-fix: five of the six refused shapes minted a real,
approved, open root successfully before this change.

---

## D2 — `DECISION`: HR-21's pinned lock order extends to every `wallet_balance_projection` row, not only `player_bonus`

**Context — the finding.** `ledger-accounting-model.md` §6.6.16 records a
joint gap in its own words: *"the lock-acquisition order across HR-3's
`(tenant_id, correlation_id)` advisory lock, doc 10 §9/§T.7's
`(tenant_id, grant_id)` advisory lock, and HR-12's `FOR UPDATE` on the
`player_bonus` projection row… No cycle exists among the orders as
currently written, but no document states the order jointly, and 'no
cycle today' is not a property that survives a fourth participant."*

**Wave 3 added the fourth participant.** Tracing the whole Wave at once:

- `internal/casino`'s `postBet` takes `FOR UPDATE` on the **`player_cash`**
  `wallet_balance_projection` row (`lockCashBalance`, invariant #15),
  then posts (migration 0023's `AFTER INSERT` trigger takes a write lock
  on every projection row the posting touches), and only **then** —
  strictly after both — reaches `HasActiveWageringGrant` →
  `RecordCashFundedWageringContribution` →
  `RecordWageringContribution` → `AdvisoryLockGrant`. Order:
  **projection row, then grant**.
- Every Bonus-domain value-moving path does the exact reverse, by design:
  `ActivateGrant`, `TerminateGrant`, `CheckAndCompleteGrant`,
  `ConvertGrant` and `ResolveHeldDispositionAction` all take
  `AdvisoryLockGrant` **first**, then `ledger.Post`, whose trigger then
  takes the projection row locks. Order: **grant, then projection row**.

Both locks are of the **blocking** family (`pg_advisory_xact_lock`, not
the try-lock variant; `SELECT … FOR UPDATE`), so a genuine wait-for cycle
between two transactions is possible whenever the two orders meet on a
shared projection row. **This is a different question from `risk`'s F1
and is not covered by its closure**: F1 concerned two *scheduler*
namespaces using `pg_try_advisory_xact_lock`, which by construction never
waits and therefore can never participate in a cycle (`qa` Phase 9 closed
it empirically and correctly). The pair here blocks.

**Liveness, stated precisely rather than alarmingly.** The cycle is
**latent, not live**. The only Bonus paths that post to `player_cash` are
`ConvertGrant` and `ResolveHeldDispositionAction`'s `ACTION_ROUTE_TO_CASH`;
the only ones that post to `house_gaming` are `postWinLockedBonus` and
`postRollbackHeldWin`. `ConvertGrant` has **zero non-test callers** and
there is still no conversion route in `bonus_routes.go` (verified at
`5af05d2`), and the three G-2 paths remain unreachable because nothing
locks bonus-funded stakes (the reconnaissance's §1.1 finding, unchanged
by this Wave). The live Grant-locking paths Wave 3 *did* wire — the
deposit sweep, the cashback scheduler, the expiry sweep — post only to
`player_bonus`/`promo_liability`/`bonus_expense`, which `postBet` never
locks. No cycle is reachable today.

**Decision — HR-21 (extended).** The pinned order, binding on every
domain, is:

> `(tenant_id, correlation_id)` advisory lock → `(tenant_id, grant_id)`
> advisory lock → **any** `wallet_balance_projection` row lock (whether
> taken explicitly with `FOR UPDATE` or implicitly by migration 0023's
> trigger on an `INSERT INTO ledger_entries`).

The third element is no longer "the `player_bonus` projection row"
specifically. Any transaction that takes both a Grant advisory lock and
any projection row lock must take them in that order. Recorded
normatively in doc 34 §5.6; `ledger-finance` owns whether to mirror it
into `ledger-accounting-model.md`'s own HR catalogue (routed, not done
here — that document is theirs).

**Two consequences, both binding.**

1. **`DR-4HB1W3-ARCH-01` (open, routed to `casino` + `bonus-engine`).**
   `postBet`'s cash-funded wagering-contribution call site violates the
   extended order today. The fix is to resolve the qualifying Grant and
   take `AdvisoryLockGrant` **before** `lockCashBalance`, not after the
   posting. It is deliberately **not** made in Phase 10: it restructures
   a hot path across two domains (the Grant must be resolved earlier and
   threaded down, and the fail-closed multi-Grant branch hoisted with
   it), which is more than a certification phase should change this late
   in a Wave. **Gate: this fix lands before any of (a) a conversion
   trigger of any kind — HTTP route, auto-convert sweep or job — (b)
   `postBet`'s bonus-funded/locked-stake leg, or (c) any other path that
   makes `ConvertGrant`/`ACTION_ROUTE_TO_CASH`/`postWinLockedBonus`
   concurrently reachable with a bet.** Any one of those makes the cycle
   live with no further change to `postBet`.
2. **The constraint `risk` routed here by name (Phase 4), now stated in
   its general form.** When `postBet`'s bonus-funded/locked-stake leg is
   eventually built, its Grant advisory lock must be taken **before**
   `lockCashBalance`/`ledger.Post`, never after. `casino` (Phase 7)
   independently re-confirmed the factual claim about the current call
   ordering, and this document does not re-derive it. Additionally, and
   separately: **no conversion or held-disposition call site may become
   reachable for a Grant in `activated`/`in_progress`** — `ConvertGrant`
   requires `completed` and `TerminateGrant` structurally rejects
   anything else, and neither guard may be relaxed to accommodate a
   bonus-funded leg.

---

## D3 — `DECISION`: the `contribution_weight_table` fail-open question (`risk`'s F2)

**The question as routed.** `ResolveContributionWeightBP` fails **open**
to 100% contribution on a missing or unparseable
`contribution_weight_table`. `risk` (LOW, needs a ruling) flagged this as
"the value-favouring-the-player direction on a malformed Offer config,
the inverse of the T.5.1 value-reducing asymmetry the rest of the package
applies," and asked for an explicit ruling rather than an
implementation-local choice.

**The ruling: neither fail-open nor fail-closed silently. Fail loud, and
move the decision to where the error actually is.** The framing offers
two options that are both wrong in this specific case:

- *Fail closed to 0%* makes the Grant **uncompletable**. The player
  wagers against an advertised requirement that silently can never be
  met, the Grant eventually expires, and the platform keeps the value.
  That is a value-**increasing**-for-the-platform silent failure driven
  by a platform-side configuration defect — worse, not better, than the
  status quo, and exactly the shape a regulator treats as unfair terms.
- *Fail open to 100%* is not free either. `risk` is right that it is the
  inverse asymmetry, and the claim that "this affects progress, not
  money" is only half true: completion is the precondition for
  `ConvertGrant`, which moves real value from `player_bonus` to
  `player_cash`. What is true — and what bounds the exposure — is that no
  **new** value is created: the liability was already posted at
  activation, the amount is capped by `granted_amount`/`MaxCashoutAmount`,
  and conversion still runs the full T.1 gate chain. The defect
  accelerates a payout the platform already authorized; it does not
  invent one.

**Therefore, three rulings, in order of where the error belongs:**

1. **Authoring time — reject, loudly.** A `contribution_weight_table`
   that does not parse into `ContributionWeightTable`, or that carries a
   weight outside `[0, 10000]`, must be **rejected** at
   `CreateOfferVersion`/the HTTP create-version handler, with a
   validation error naming the offending key. A configuration error costs
   nothing to refuse at the moment it is authored and is invisible to the
   player. Today nothing validates it at any layer: the handler binds the
   raw JSON string through and Postgres only checks that it is *valid
   JSON*. **Routed to `bonus-engine`** (their write path); recorded in
   doc 13.
2. **Bet time, out-of-range weight — clamp, never propagate.**
   `IMPLEMENTED` this phase (`5af05d2`, `DR-4HB1W3-ARCH-03`). This was not
   a policy nicety: migration 0058's column `CHECK
   (contribution_weight_bp BETWEEN 0 AND 10000)` was the only enforcement
   anywhere and it fires at the **end** of the chain, so an out-of-range
   weight surfaced as a raw constraint error out of
   `CreateWageringProgress`, out of `RecordCashFundedWageringContribution`,
   out of `postBet` — **rolling back the player's own cash bet**. One
   mistyped Offer field was a live betting outage for every player
   holding a Grant against it. The clamp is the value-reducing direction
   only (an out-of-range weight can now reduce, never inflate, credited
   progress) and restores symmetry with the posture `risk` itself set one
   phase earlier in `DR-4HB1W3-RISK-02`: a dormant Grant-data defect must
   fail closed on the **authorization** decision without becoming a live
   betting outage.
3. **Bet time, unparseable table — keep the permissive default, but stop
   being silent.** The resolver must distinguish **absent** (`len == 0` —
   an Offer that never configured this axis; 100% is the correct,
   documented, intended default and warrants no signal) from **present
   but unparseable** (a platform-side data defect), and the latter must
   emit an audited signal naming the `offer_version_id` — the same shape
   `RecordCashFundedWageringContribution` already uses for its two other
   "refuse to guess" cases
   (`bonus_wagering_contribution.ambiguous_multi_grant_skipped`,
   `…unmeasurable_target_completion_skipped`). The player's live bet is
   not the place to punish either party for a platform data defect; an
   operator finding out is. **Routed to `bonus-engine`** (one audit call
   plus a branch); recorded in doc 13. Not made here because the resolver
   is currently a pure function with no `ctx`/`tx`, and threading those
   through is a signature change to their code, not a certification fix.

**What is routed to `product-owner-proxy`, and what is not.** The
structure above (reject at authoring; clamp out-of-range; signal the
unparseable case) is an engineering-safety ruling and is **not** open for
re-litigation on commercial grounds. The one genuinely commercial
question inside it — *when an Offer's table is unparseable at bet time,
is continuing at 100% the right default, or would the operator rather
the Grant stall and be reviewed?* — is a product call, and is routed to
`product-owner-proxy` for confirmation. `architect`'s recommendation is
100%-plus-signal, because the alternative silently withholds an
advertised benefit from a player who did nothing wrong. This is a
confirmation of a default within a fixed frame, not an open design
question.

---

## D4 — `DECISION`: `bonus_campaign_activation` is a declared EOI type with no mint point, and Wave 3 made its effecting writes live

**The finding (no single phase would have caught this).** Doc 34 §3.1
declares `bonus_campaign_activation` as an EOI `operation_type`, minted
at "a Bonus Campaign being activated", with consumption shape **standing
authorization** — *"an activated campaign causes many subsequent
grant-issuing effecting writes over its life, under the one activation
approval."* The Go enum carries the constant
(`OperationBonusCampaignActivation`). But:

- `ActivateCampaign` (Phase 3, `four_eyes_ops.go`) consumes a four-eyes
  `ChangeOperation` and performs the status write. It mints **no EOI**.
- `economicop.consumptionShapes` has **no entry** for this type, so even
  if one were minted, `ConsumeRootBudget` would refuse it fail-closed
  (`ErrUndeclaredConsumptionShape`).
- **Wave 3 built exactly the effecting writes doc 34 declared this type
  to bound**: the deposit sweep and the cashback scheduler issue Grants
  automatically, to any qualifying player, for as long as the Campaign
  stays active. Before this Wave those writes did not exist, so the
  missing EOI bounded nothing that was happening.

Consequently, a single four-eyes-approved campaign activation today
authorizes an **unbounded** count and value of automatic Grant issuance —
bounded only by Offer configuration and per-player idempotency, never by
a declared ceiling. That is precisely the R12 decomposition surface the
EOI exists for, at the one surface that is now automated.

**Decision.** This is not an emergency and is not fixed here: every
sweep-driven issuance currently **denies at the `AssetAuthorization`
gate**, because no per-player jurisdiction resolver exists anywhere on
the platform (`qa` Phase 9 §4 confirms no test in this Wave ever reaches
a successful `bonus_grant` posting through either sweep). The mechanism
is inert for a reason unrelated to this gap. **The gate is therefore tied
to the thing that makes it live: before automatic, campaign-driven
issuance can actually succeed — i.e. as part of whatever change closes
the jurisdiction-resolution gap for the sweeps — one of two things must
happen, and be recorded:** either (a) `campaign_activate` mints a
`bonus_campaign_activation` EOI root with declared bounds (which then
requires the matching `consumptionShapes` and
`boundedRootOperationTypes` entries per D1's consequence, and the sweeps
must relay it as `parent_operation_id`), or (b) an explicit recorded
determination, co-signed by `security`, states why campaign-driven
automatic issuance does not need a budget ceiling and what bounds it
instead. Silence is not the third option. Recorded in doc 34 §3.1's note
and in doc 13.

---

## D5 — `DECISION`: disposition of `security`'s remaining residual items (W3P6.5 items 2–4)

- **Item 2 — subject-set containment is `single_subject`-only.**
  **Accepted as adequate today; no escalation.** `security`'s own
  assessment is confirmed on inspection: `enumerated_set` roots are the
  only other shape any code mints, and the bulk surface's recipient set
  is independently bound twice over — by
  `BulkJobExecutePayloadMatch`'s recipient-set hash inside the four-eyes
  pinned payload (a payload swap mid-run is refused, doc 34 §5.4) and by
  SEP-1's own beneficiary resolution — plus `recipient_ceiling`, which
  D1 now makes mandatory. The EOI row itself does not carry
  `subject_set_hash`/`subject_definition_hash`, so the containment is
  enforced one layer up rather than on the authorization object. That is
  a real architectural asymmetry and is recorded as such, but it closes
  no live gap to fix it now, and doing so would require materializing
  those hashes on the EOI row — a schema change for a control that is
  already covered. **Binding condition:** the first `criteria_defined`
  root minted by anything reopens this item, because that shape has no
  equivalent second binding.
- **Item 3 — `ConsumeRootBudget` skips the subject check on `uuid.Nil`.**
  Confirmed latent (no caller passes it). **Folded into D1's direction of
  travel**: `ValidateRootAuthorizationBounds` now refuses a
  `single_subject` root with a nil `subject_ref` at mint, which removes
  half the shape. Refusing a nil *subject argument* at consume time under
  `single_subject` scope is the other half and is **routed to
  `bonus-engine`** as a one-line fail-closed addition when they next
  touch that function; recorded in doc 13.
- **Item 4 — the EOI-mint audit record is thin.** Confirmed, accepted as
  a real gap, **routed to `bonus-engine`** (the audit metadata map in
  `MintRootOperation` plus the handler's request context). Not fixed here
  because "who authorized this budget, from where" is an audit-content
  question with no architectural decision inside it. Recorded in doc 13.

---

## D6 — `DECISION`: two boundary statements that Wave 3 made wrong, corrected in doc 29

Both are cases of a document contradicting shipped code after this
Wave — the class of finding that is invisible phase-by-phase and only
appears on an end-to-end read.

**(a) BI-2's `casino → bonus` precondition was stale, and read literally
marked shipped code as a violation.** It conditioned the import on "ADR
0039 Decision 2 selecting `ACTION_ROUTE_TO_CASH`". Decision 2 was never
resolved that way — Wave 2 built all three actions as staff-resolvable
`bonus_held_dispositions` outcomes, so no code path selects any of them —
and Wave 3 then added a `casino → bonus` call site with nothing to do
with G-2 at all (`postBet`'s wagering-contribution trigger, which HR-10
requires to be in the bet's own transaction). **Corrected in doc 29
§8's BI-2 plus a note**: the import is permitted; BI-2's real content is
the **one-way** rule that `ledger`/`wallet`/`risk`/`rg`/`payments` never
depend on Bonus, which is unchanged and still holds. **ADR 0039 Decision
2 is untouched and stays open** — only the stated precondition was
corrected, not the decision.

**(b) The deposit sweep satisfied the transport design's letter by
taking an undeclared schema dependency instead.** `internal/bonus`
reads `deposit_intents` — an `internal/payments`-owned table — directly
via raw SQL, for `brand_id`, `player_account_id`, `wallet_id`,
`asset_code`, `amount` and `payment_method`. Avoiding a Go-level call
into `internal/payments` was the right instinct, and the read is
genuinely necessary (`ledger_transactions` carries no `payment_method`,
which the deposit-method eligibility axis needs). But the result is a
dependency the compiler cannot see: a payments migration renaming one of
those columns breaks a Bonus sweep at runtime, with nothing in either
domain's review able to notice. **Decision: accept the read, declare the
dependency.** New invariant **BI-16** (doc 29 §8) names the table and the
exact six columns, makes it mechanically checkable (the set of non-`bonus_*`
tables in `internal/bonus`'s SQL must equal the declared set), and binds
`payments` to treat those columns as a published interface. This is the
only peer-domain table read Wave 3 introduced — verified by diffing the
table set against `9d857dc`.

## `CONSTRAINT` RC-1 — `bonus_adjustment_write`, for whoever builds it

Not built in Wave 3 (`bonus-engine` stopped on it deliberately; the
reconnaissance's dependency map routes the posting shape to
`ledger-finance` first). These constraints bind that work; they do not
authorize it.

1. **The posting shape is `ledger-finance`'s to specify first**, exactly
   as they specified the `held_disposition_resolve` shapes. Nothing may
   be implemented against a guessed shape.
2. **Risk `Operation`: mint a new one; do not reuse `OperationBonusGrant`**
   (`risk`'s routed input, adopted). A manual adjustment is a distinct
   posting shape with a distinct limit-evaluation path, and ADR 0031 §12's
   additive discipline plus a `cumulativeSpec` entry is the declared way
   to add one. Reusing the grant operation would silently fold staff
   adjustments into whatever cumulative rules are scoped to ordinary
   bonus grants — a category error that under- or over-counts both.
   Note the vocabulary-disjointness rule (doc 34 §3.1): the Risk
   `Operation` and the EOI `operation_type`
   (`manual_balance_adjustment`, already declared) are separate
   vocabularies and neither may be derived from the other.
3. **The gate asymmetry must split by direction, not by operation.** A
   value-**increasing** adjustment (crediting `player_bonus`) runs the
   full T.1 chain — `AssetAuthorization` → RG → Risk — unchanged and
   unreordered, plus four-eyes above threshold, plus an EOI. A
   value-**reducing** adjustment follows `terminalWriteDown`'s existing
   precedent and is **not** gated by `AssetAuthorization`/RG/Risk (T.5.1's
   value-reducing asymmetry: a control that refuses to take value away is
   protecting nobody) — but it still requires the reason code, the
   four-eyes threshold and the audit record, which are about authority
   and accountability, not about value authorization.
4. **`ConsumeRootBudget` cannot count it until
   `manual_balance_adjustment` has a declared `consumptionShapes` entry**
   — and, per D1, a `boundedRootOperationTypes` entry in the same change
   if the adjustment can increase value.

## `CONSTRAINT` RC-2 — `grant_forced_conversion`, for whoever builds it

`risk`'s routed input is adopted in full and restated as binding:

1. **The full T.1 chain runs unchanged and unreordered**, with
   `Operation = bonus_conversion`. "Forced" describes which *Bonus-domain
   precondition* is overridden, never which *gate* is skipped. A forced
   conversion that skips `AssetAuthorization`/RG/Risk is not a forced
   conversion, it is an unauthorized payout.
2. **Four-eyes is added in series, never in substitution.** It is an
   additional conjunct on top of the gate chain and the EOI, not a
   replacement for any of them (doc 34 §5.3's composition rule).
3. **If forced conversion lets staff name a different amount, that amount
   is the one Risk evaluates** — never the Grant's own derived amount
   with a different figure posted afterwards.
4. **`architect` scope ruling on what "forced" may override.** It may
   override the **wagering-target** check (HR-12's `P_firm` re-derivation
   versus `WageringTargetScaled`) — that is a commercial/goodwill
   decision a staff member is entitled to make with four-eyes. It may
   **not** override the **AOE** check. `ConvertGrant` checks `AOE(G,t)`
   first and blocks with `open_exposure_outstanding` for a reason: paying
   cash against value that is still at risk in an unsettled round
   double-spends the same bonus value, and no approval count makes that
   arithmetic work. A forced conversion attempted against a non-empty AOE
   must be refused with the same reason code, not escalated past. This
   answers the open question the reconnaissance's dependency map named
   ("does 'forced' mean overriding the wagering-target check only, or
   also AOE?"): **wagering target only.**
5. `MarkGrantReversed`'s wiring (still caller-less) does **not** get
   folded into this operation's approval flow by default — that remains an
   open scope question, unchanged by this Wave.

## `CONSTRAINT` RC-3 — `grant_cancel_completed`

Unchanged and untouched by Wave 3. `TerminateGrant` structurally rejects
a `completed` Grant, so this is genuinely new business logic whose
posting shape (clawing back value that may already be converted and
spent) is `ledger-finance`'s to specify before `bonus-engine` implements.
Named here only so its absence stays deliberate.

## `CONSTRAINT` RC-4 — `StakedBonusAmount` is now "read from the posted debit" by call-site construction only

§6.6.4's rule is verbatim: *"`staked_bonus_amount` … **read from the
posted ledger entry**, never from the caller"*, generalized by §7.18.3.2
to the `player_cash` debit for the cash-funded mechanic.
`RecordCashFundedWageringContribution` passes `event.Amount`, the number
`postBet` itself just posted, in the same transaction. **That value is
provably the posted debit today** — `postBet` posts `event.Amount` and
then passes it, and the `findPostedBetTransaction` short-circuit
(strictly earlier in the function, under the bet-delivery advisory lock)
makes `postResult.AlreadyPosted` structurally unreachable at the call
site, so a redelivery carrying a different amount can never arrive here.

But the rule is now satisfied **by construction across two packages**
rather than by reading `ledger_entries`, and nothing tests that property.
Moving the contribution call above the short-circuit, or adding a second
caller of `RecordCashFundedWageringContribution` without postBet's
guarantees, would break §6.6.4 silently. **Binding invariant for `qa` and
`code-reviewer`:** any caller of `RecordCashFundedWageringContribution`
must supply a `StakeAmount` that is the posted funding-account debit on
`BetLedgerTransactionID` in the same transaction. **Recommended durable
fix, routed to `bonus-engine`/`ledger-finance`, not made here** (it would
break test fixtures that pass synthetic transaction ids, a wider blast
radius than a certification phase should take): have
`RecordWageringContribution` verify `StakedBonusAmount` against the
actual posted debit on `LockLedgerTransactionID`, making the rule
structurally true rather than conventionally true.

---

## Confirmations

- **`CONFIRMED` — LF-10 is untouched and remains orthogonal.** Re-checked
  against this Wave's diff, not inferred from the reconnaissance:
  `bonus_settlement.go`'s `postRollbackHeldWin` is unmodified in
  `9d857dc..8c6ab7a`, and no Wave 3 file touches
  `bonus_held_dispositions`. Status unchanged: a rollback naming a
  settlement whose disposition has already moved past `held` **fails
  closed** (`ErrHeldDispositionRollbackUnsupported`, posts nothing), the
  general case remains open and correctly routed to `ledger-finance` as a
  future dedicated dispatch, and it is doubly inert — the path is
  unreachable until `postBet`'s bonus-funded locking side exists.
- **`CONFIRMED` — the multi-account abuse signal is real and is now
  recorded in the architecture record, not only in a commit message.**
  `detectMultiAccountFirstDepositSignal` (`deposit_sweep.go`, Phase 5):
  for a `FirstDepositOnly` Offer match it resolves the depositing
  account's `person_id` and checks whether that Person already holds a
  `bonus_grants` row against the **same** Offer via a **different**
  `player_account_id`, recording
  `bonus_deposit_sweep.multi_account_signal_detected`. It is a **signal
  only** — never a denial, skip or alteration of the Grant attempt —
  which is what `security-architecture.md` §W15.1.5/REQ-SEP-BONUS-3
  requires and explicitly forbids turning into a block.
  Device/payment-fingerprint correlation remains out of scope and needs a
  fraud/device-signal source this platform does not have. Doc 13's R5
  mitigation row is updated accordingly.
- **`CONFIRMED` — F1 is closed, and its closure is recorded where it will
  be found.** `risk`'s Phase 4 concern (can the deposit sweep and the
  cashback scheduler deadlock each other across two advisory-lock
  namespaces?) was closed empirically by `qa` Phase 9
  (`TestDepositAndCashbackSchedulers_ConcurrentDifferentNamespaces_NoDeadlock`,
  8 iterations, asserting specifically on SQLSTATE `40P01`). The
  structural reason it can never recur: both jobs use
  `pg_try_advisory_xact_lock`, the **non-blocking** family, and a lock
  acquisition that never waits cannot participate in a wait-for cycle.
  **This reasoning does not transfer to D2's pair**, which blocks — the
  distinction is recorded in doc 34 §5.6 precisely so a future reader does
  not cite F1's closure as covering it.
- **`CONFIRMED` — the KYC-tier taxonomy gap is scheduled, not resolved.**
  `identity-compliance` (Phase 5) ruled it NOT NEEDED FOR WAVE 3 and
  `architect` agrees. It remains a genuine cross-domain design gap:
  `bonus_offer_versions.kyc_rg_level_required` is stored and never read,
  and `player_accounts.kyc_tier` has the right *name* but no operative
  *meaning* (nothing in the repository ever writes it; it is always 0).
  Reuse is therefore not available — defining what a tier means, what
  promotes a player between them and who owns writing it **is** the
  missing contract. Dispatch order, unchanged: `identity-compliance`
  (design) → `architect` (contract sign-off) → `bonus-engine`
  (enforcement call site). Recorded as a scheduled item in doc 13 so it
  survives the end of this Wave.

---

## Consequences

- Two shipped code changes (D1, D3 item 2), both regression-proven, both
  inside the existing mechanisms — no new control, no new schema, no new
  migration.
- Two normative additions to doc 34 (§5.6 lock ordering, §5.7 bounded
  roots), one note on §3.1, two new EOI invariants (EOI-19/EOI-20); one
  corrected invariant and one new one in doc 29 (BI-2's precondition,
  BI-16's declared peer-domain read).
- Seven items routed to other owners and recorded in doc 13's scheduled
  cross-domain table: the `postBet` lock-order fix (`DR-4HB1W3-ARCH-01`,
  gated), `contribution_weight_table` authoring validation, the
  unparseable-table audit signal, `ConsumeRootBudget`'s nil-subject
  refusal, the EOI-mint audit metadata, the `StakedBonusAmount`
  ledger-read hardening, and the `bonus_campaign_activation` EOI
  determination (gated).
- One question routed to `product-owner-proxy` (D3's commercial default),
  framed for confirmation inside a fixed structure.
- Nothing in this document authorizes a stage transition, and nothing in
  it overrides `ledger-finance` on a financial invariant or `security` on
  a security requirement — D1 and D5 implement what `security` asked for
  and route back what they did not.

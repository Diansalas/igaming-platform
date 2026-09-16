# ADR 0032 — Bonus, Reward and Promotional Accounting

Status: Accepted (Stage 4H-A, Bonus/Gamification/Reward Architecture
Freeze) — **architecture only, `NOT IMPLEMENTED`**. No migration, no Go
code, and no `transaction_type`/`account_type` value in this document
exists yet. Owner: `ledger-finance`. This ADR resolves the three bonus
`OPEN DECISION`s left standing by `ledger-accounting-model.md` §2 and
`financial-transaction-flows.md` Flows 12/14/15, and adds the accounting
treatment for provider-funded and externally-fulfilled rewards, which no
prior document covered at all.

Labeling convention is inherited from `financial-domain-model.md`:
`BLUEPRINT` = stated directly in the Blueprint; `ARCHITECTURAL DECISION` =
decided here or in a named prior ADR; `OPEN DECISION` = deliberately not
resolved, with the reason and the required decision-maker named;
`RECOMMENDATION` = a `ledger-finance` position that still needs a
business/finance/human sign-off before it binds.

## Context

Stage 3A/3B built the ledger (`internal/ledger`, ADR 0019/0020/0021) and
deliberately stopped short of bonus postings: `ledger_transactions.
transaction_type` carries no bonus values, and three questions were
recorded as open rather than guessed at.

1. `ledger-accounting-model.md` §2 — what `promo_liability` actually *is*,
   and what the debit side of a bonus grant is.
2. `financial-transaction-flows.md` §14 — whether bonus→cash conversion
   also discharges `promo_liability`, and against what.
3. `financial-transaction-flows.md` §15 — confirmed as a new economic
   fact, not a reversal, but with no expense/no-expense position stated.

Stage 4H-A adds three more, none of which any existing document addresses:
who bears the cost when a *provider* funds the promotion; what the ledger
does when a bonus is granted and settled entirely inside an *external*
provider's own bonus engine; and where the line sits between a reward that
is money and a reward that is not.

Leaving these open is no longer acceptable: the Bonus Engine, the
Gamification subsystem and the Reward Orchestrator are all being designed
in this stage, all three can cause value to move, and every one of them
would otherwise be free to invent its own answer. That is precisely the
"second financial truth system" failure this ADR exists to prevent.

## Decision

### 0. Position statement — there is exactly one financial truth system

`ARCHITECTURAL DECISION`, non-negotiable, and the frame for everything
below.

The Bonus Engine, the Gamification subsystem and the Reward Orchestrator
are **decision systems**. They decide *who*, *when*, *how much*, *under
what terms*, and *whether the terms were met*. They are not accounting
systems. Every monetary consequence of their decisions is recorded by
`internal/ledger`, through `ledger.Post`, under the same invariants as a
deposit, a casino bet or a withdrawal — append-only, double-entry,
balanced per asset, DB-enforced idempotency, compensating corrections
only, projection-never-authoritative, reconciled on a schedule.

Binding consequences, each of which is a blocking `ledger-finance` review
finding if violated:

- No bonus/gamification/reward table holds an authoritative monetary
  balance. Any monetary figure such a table carries is a denormalized read
  of the ledger and must be reconcilable to it, exactly as
  `wallet_balance_projection` is (ADR 0019).
- No bonus/gamification/reward code path `UPDATE`s a balance, mutates a
  historical entry, or posts a money movement without an idempotency key.
- Wagering progress remains a **derived read** over ledger entries that
  debited `player_bonus` (`financial-transaction-flows.md` §13,
  `06-wallet-ledger-architecture.md`) — not a counter maintained in a
  bonus side table that could drift from the entries it claims to
  summarize.
- `internal/ledger` is still the only package that writes
  `ledger_accounts`/`ledger_transactions`/`ledger_entries` (ADR 0001).
  The Bonus Engine supplies the instruction; `ledger-finance` owns the
  posting mechanism. That sentence already existed in
  `10-bonus-engine-architecture.md`; this ADR makes it binding on
  Gamification and the Reward Orchestrator too.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 1. Which rewards touch the ledger, and which never do

`ARCHITECTURAL DECISION`. The Blueprint does not enumerate reward types,
so the line is drawn here as a *test* rather than a list, because a list
will be incomplete the first week Gamification ships.

> **Monetary test.** A reward is monetary — and therefore goes through
> `internal/ledger` — if it has a defined redemption value denominated in
> a registered `Asset`, **or** if the platform would owe the player
> something denominated in an `Asset` should the player ask for it. If
> neither holds, it does not touch the ledger at all.

Applying the test:

| Reward | Ledger? | Treatment |
|---|---|---|
| Bonus funds / deposit match / cashback in funds | **Yes** | §3 below |
| Free spins / free bets creating a platform-side obligation in an asset | **Yes** | §3, §6 (funding source decides the cost side) |
| Tournament prize paid in wallet funds | **Yes** | Ordinary grant or cash credit per §3/§6 |
| Bonus winnings / converted bonus | **Yes** | §4 |
| Badge, level, XP, avatar, cosmetic, leaderboard rank | **No** | Gamification state only; audit record, no ledger entry |
| Non-transferable status tier (VIP level) | **No** | Gamification state only |
| Free tournament entry with no cash-value alternative | **No** | Gamification state only |
| Loyalty/VIP **points** | **No — separate ledger** | `docs/architecture/24-points-accounting-architecture.md` |
| Externally-granted-and-settled provider bonus never entering our wallet | **No** | §6(c) — recording a liability we do not owe is as wrong as omitting one we do |
| Physical prize (merchandise, travel) | `OPEN DECISION` | Real cost, not player wallet value; recognition timing and account are a finance decision, not resolved here |

Posting a ledger entry for a non-monetary reward is a defect: it inflates
`promo_liability` with an obligation that can never be discharged in an
asset, and it breaks the §3 mirror invariant. Conversely, *not* posting a
monetary reward because it was issued by a reward/gamification code path
rather than the Bonus Engine is the same defect in the other direction.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.** The physical-prize
row is **`OPEN DECISION`**, referred to finance/the Master Orchestrator.

### 2. `promo_liability` — definition, and the mirror invariant

`ledger-accounting-model.md` §2's `OPEN DECISION` offered two framings:
(1) `promo_liability` as the debit-side mirror of `player_bonus`, or (2)
`promo_liability` credit-normal with a new `bonus_expense` account. Both,
as written, were incomplete: framing 1 has nothing to discharge
`promo_liability` at conversion (it strands permanently, so its claimed
"mirrors outstanding `player_bonus`" property is false the first time a
player converts), and framing 2 cannot be posted at all, because one
grant of amount `X` cannot credit two credit-normal accounts without a
`2X` debit.

**Resolved: framing 1 is confirmed for `promo_liability`, and the missing
piece — the account that was genuinely absent — is added.**

- **`promo_liability`** (`BLUEPRINT`, house-level, per `(tenant_id,
  asset_code)`, `wallet_id IS NULL`): the platform's counter-side for
  bonus funds in circulation. Debited when bonus value enters a player's
  bonus balance, credited when it leaves. Its credit-positive
  `signed_balance` is therefore negative, which matches
  `ledger-accounting-model.md` §5's existing expectation (`promo_liability
  ≤ 0`) — **no change to that sign table is required**.
- **`bonus_expense`** (`ARCHITECTURAL DECISION`, **new — a twelfth account
  type**, house-level, per `(tenant_id, asset_code)`, `wallet_id IS NULL`,
  debit-normal): the operator's *recognized* promotional cost. This is the
  account `ledger-accounting-model.md` §2 and
  `financial-transaction-flows.md` §14 both identified as missing ("a
  realized-bonus-cost/P&L account does not exist yet") without naming it.

The invariant this buys, and the reason for choosing this shape over any
other, is that `promo_liability` becomes a *continuously checkable mirror*
rather than a running total nobody can verify:

> **Invariant B1 (bonus mirror).** For every `(tenant_id, asset_code)`:
> `signed(promo_liability) + Σ signed(bonus-denominated player accounts) == 0`
> at every instant, with no tolerance band.

As of this ADR the set of bonus-denominated player accounts is exactly
`{player_bonus}`. See §9 for the one change that would extend it, and
which is a **blocking precondition** for the flow that needs it.

The rule that makes B1 hold by construction:

> **Rule B2 (mirror rule).** Every `LedgerEntry` against a `player_bonus`
> account is accompanied, in the **same `LedgerTransaction`**, by an entry
> of the opposite direction and equal amount against `promo_liability` for
> the same `(tenant_id, asset_code)`.

For a grant and a forfeiture, the mirror leg *is* the transaction's other
leg, so the transaction has two entries. For every other bonus movement
(stake, win, conversion, jackpot carve-out, rollback) the real economic
pair is joined by a `promo_liability ↔ bonus_expense` pair, so the
transaction has four entries and still balances per asset.

Rule B2 admits **no exception by transaction type**. It binds
`manual_adjustment`, `bonus_reversal`, casino/sportsbook postings and any
future type identically: a `manual_adjustment` against `player_bonus` of
`X` is four entries (`player_bonus` ↔ `manual_adjustment` for the real
correction, `promo_liability` ↔ `bonus_expense` for the mirror), never
two. `internal/ledger` validates this unconditionally on every posting,
not per-caller. **Wave-2 ledger-finance review correction (P1-4)**: an
earlier draft left this unstated, and §7's manual-adjustment guidance
below named no account, which would have let a four-eyes-approved
correction post `Dr player_bonus / Cr manual_adjustment` with no mirror
leg — breaking B1 undetected until the next hourly sweep.

`RECOMMENDATION` (implementation shape, not an invariant): the mirror legs
are **generated by the ledger posting layer** from the `player_bonus`
entry, not hand-assembled by `internal/casino`, `internal/sportsbook` or
the bonus engine. Enforcement belongs where the invariant is owned, not in
each caller's discipline — the same reasoning CLAUDE.md applies to RLS
("not by discipline in application code"). A caller-supplied-and-validated
variant is acceptable if the validation is unconditional and in
`internal/ledger`; a variant where each domain remembers to add the legs
is not.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.** Requires a
follow-up edit to `ledger-accounting-model.md` §2 (add `bonus_expense`,
close the `OPEN DECISION`), §6 (add invariant B1) and an additive
migration adding the account type. Whether `bonus_expense` is presented as
a P&L expense line or as contra-revenue in statutory reporting is a
**finance/reporting decision, `OPEN DECISION`** — it does not change any
posting above.

### 3. Grant — when a bonus is a contingent liability

`BLUEPRINT` flow (Flow 12), directions unchanged:

| Event | Entries |
|---|---|
| Bonus grant `X` | Dr `promo_liability` `X` · Cr `player_bonus` `X` |

Recognition position, stated explicitly because the directive requires it:

> **A granted bonus is a contingent liability, not a recognized expense.**
> While the value sits in `player_bonus` it is restricted, non-withdrawable
> and may still evaporate through forfeiture. No `bonus_expense` is
> recognized at grant. The contingent exposure at any instant is exactly
> `Σ signed(player_bonus)` per tenant+asset, and — by invariant B1 — is
> equal and opposite to `signed(promo_liability)`, so it is a single-row
> read, not a report.
>
> **A bonus becomes a recognized expense at the instant bonus value leaves
> `player_bonus` for any reason other than forfeiture** — i.e. when it is
> converted to cash, wagered away to `house_gaming`, or carved into
> `jackpot_contribution`. That is the moment the operator has irreversibly
> given something up: either the player now holds withdrawable cash, or
> the house booked gaming revenue that was never funded by real money.

This is the economically correct recognition point and it produces the
right reported numbers without any period-end adjustment. Worked check
(EUR, minor units omitted for readability):

| Step | `player_bonus` | `promo_liability` | `house_gaming` | `bonus_expense` (Dr-positive) |
|---|---|---|---|---|
| Grant 20 | +20 | −20 | 0 | 0 |
| Bet 10 from bonus | +10 | −10 | +10 | +10 |
| Win 25 to bonus | +35 | −35 | −15 | −15 |
| Convert 35 to cash | 0 | 0 | −15 | +20 |

B1 holds at every row. Final `bonus_expense` = 20 = the granted amount,
recognized only once the player actually kept it. NGR (`house_gaming`
less `bonus_expense`) is `−15 − 20 = −35`, which equals the real cash the
operator handed over (`player_cash` +35, less the 0 real money the player
staked). A grant that is granted and fully forfeited leaves every column
at zero — correctly, nothing happened. A grant that is granted and fully
lost in play leaves `house_gaming +20` and `bonus_expense +20`, i.e.
NGR 0 — correctly, the "revenue" was the operator's own promotional money
round-tripping.

**Direct cash reward (no wagering requirement).** Where an Offer or reward
decision credits withdrawable funds directly — a `cash_credit` reward type
(`21-reward-orchestration-architecture.md`), a goodwill cash award, a
cashback paid as cash rather than bonus:

| Event | Entries |
|---|---|
| Cash reward `X` (operator-funded) | Dr `bonus_expense` `X` · Cr `player_cash` `X` |
| Cash reward `X` (provider-funded, §6(b)) | Dr `provider_payable` `X` · Cr `player_cash` `X` |

Two entries. `promo_liability` is **not** involved and invariant B1 is
unaffected, because no bonus-denominated balance is created: expense is
recognized immediately, which is correct — nothing is contingent and
nothing can be forfeited. `transaction_type = bonus_grant` with a
funding/`reward_kind` discriminator on the transaction metadata.

A cash reward is **never** modeled as a zero-wagering `player_bonus` grant
followed by an immediate conversion. `10-bonus-engine-architecture.md`
§2's "modeled as a Wagering axis with multiplier 0 / already satisfied" is
a **lifecycle-state statement only** (so the Progress trail still records
a `completed` transition) and must not produce two postings and a
transient restricted balance for value that was never restricted.
**Wave-2 ledger-finance review correction (P1-3)**: an earlier draft of
this ADR and doc 21 both referenced "cash credit" as a supported reward
type without ever defining its entries — `21-reward-orchestration-
architecture.md`'s `cash_credit` mechanism and `10-bonus-engine-
architecture.md` §2's "Cash reward (no wagering requirement at all)" row
both resolve to the table above.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 3.1 Lifecycle event → posting map (binding)

`ARCHITECTURAL DECISION`, added by Wave-2 ledger-finance review (P1-2).
An earlier draft left the mapping from Bonus Engine lifecycle events
(`10-bonus-engine-architecture.md` §6) to the postings above implicit,
with three concrete consequences that would have shipped as real bugs:
`granted` and `activated` were both emitted as lifecycle events with no
statement of which one triggers the §3 grant posting (a domain posting on
both would double-credit undetected, since the two events carry different
idempotency keys); a `converted` lifecycle event was never defined despite
§4 requiring one; and `cancelled` had no posting at all, though it is a
terminal state reachable after funds already sit in `player_bonus`. This
table is now the single binding answer:

| Bonus Engine lifecycle event (doc 10 §6) | Ledger effect | `transaction_type` |
|---|---|---|
| `granted` (Grant enters `issued`) | **None.** An `issued` Grant is a decision, not a movement; no value has entered a wallet. | — |
| `activated` (funds enter `player_bonus`) | Dr `promo_liability` · Cr `player_bonus` (§3) | `bonus_grant` |
| `completed` | **None.** Conversion-eligibility is a decision, not a movement. | — |
| `converted` | The single atomic four-entry transaction of §4 | `bonus_conversion` |
| `expired` | Dr `player_bonus` · Cr `promo_liability` on the **outstanding** balance (§5) | `bonus_forfeiture` |
| `cancelled` from `issued` (never activated) | **None** — no value ever posted | — |
| `cancelled` from `activated`/`in_progress` | Dr `player_bonus` · Cr `promo_liability` on the outstanding balance — identical shape to §5, **no `bonus_expense` recognized or reversed**; distinguished from expiry by `reason_code`, not by a separate account | `bonus_forfeiture` |
| `reversed` | §7's compensating transaction, `reverses_transaction_id` set | `bonus_reversal` |

A Grant produces **exactly one** `bonus_grant` posting, at `activated`,
never at `issued`. An Offer with no opt-in and no deposit trigger reaches
`activated` in the same operation as `issued`; that is one posting, not
two. `10-bonus-engine-architecture.md` §6's event list must include
`converted` alongside `granted, activated, completed, expired, cancelled,
reversed`, and §1.3's `completed → converted` transition is described as
emitting the `converted` lifecycle event (§6 item 2) — not, as an earlier
draft said, "the conversion split instruction (§6)", which conflates §6's
wagering-split-instruction item with its lifecycle-event item.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 4. Cash conversion on wagering completion — the exact treatment

This is the question the directive singles out, so it is answered without
hedging.

**Bonus funds are converted by a single atomic transfer within one wallet
and one asset: `Dr player_bonus X / Cr player_cash X`. They are not
"retired and re-granted as new cash".** The `promo_liability` mirror is
discharged in the same transaction against `bonus_expense`, resolving
Flow 14's `OPEN DECISION`:

| Event | Entries |
|---|---|
| Bonus conversion `X` | Dr `player_bonus` `X` · Cr `player_cash` `X` · Dr `bonus_expense` `X` · Cr `promo_liability` `X` |

Four entries, balanced per asset, one `LedgerTransaction`, one database
transaction. Why this and not the alternative:

1. **A retire-then-credit design has a window in which the player holds
   neither.** Two transactions can partially fail; `ledger-accounting-
   model.md` §1.2 exists specifically so there is no partial/pending
   ledger state. One transaction removes the window by construction.
2. **The cash must remain traceable to the bonus that produced it.**
   Max-cashout caps, bonus-abuse review and AML source-of-funds all need
   "this `player_cash` credit came from that grant". A transfer carries
   that natively through `correlation_id`/`causation_id` on one
   transaction; a disconnected "new cash credit" would require a side
   table to reconstruct it — exactly the reconstruct-it-later pattern the
   cash/bonus ledger split was adopted to avoid.
3. **"New credit while the old is retired" is the shape you need when the
   two sides are different assets.** They are not: `player_bonus` and
   `player_cash` are two account types of the *same wallet* in the *same
   asset*. Per-asset balance therefore holds trivially, and no conversion
   clearing account is involved. Introducing one here would wrongly imply
   a cross-asset movement and would collide with ADR 0021's genuinely
   cross-asset `ConversionOperation`, which this is not. A bonus
   conversion is **never** a `ConversionOperation`.
4. Max-cashout capping happens *before* posting: the Bonus Engine decides
   the convertible amount `X` (possibly less than the outstanding bonus
   balance); the remainder is forfeited by a separate §5 posting. The
   ledger never silently converts less than it was asked to.

Ledger-enforced check: a conversion's `player_bonus` debit may not exceed
that wallet's `player_bonus` balance, read `SELECT ... FOR UPDATE` on the
projection row inside the posting transaction (ADR 0020). An
over-conversion is rejected, not clamped — clamping is a business decision
and belongs to the Bonus Engine.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.** Closes
`financial-transaction-flows.md` §14's `OPEN DECISION`.

### 5. Expiry and forfeiture

| Event | Entries |
|---|---|
| Bonus forfeiture / expiry `X` | Dr `player_bonus` `X` · Cr `promo_liability` `X` |

- Two entries only. **No `bonus_expense` is recognized or reversed**,
  because none was ever recognized on the forfeited value — it never left
  `player_bonus` except to be extinguished. A forfeited bonus costs the
  operator nothing and must not appear in promotional cost.
- Forfeiture is **not** a `reverses_transaction_id` reversal of the grant.
  Confirmed from `financial-transaction-flows.md` §15, restated with the
  reasoning because it is the distinction most likely to be got wrong: a
  forfeiture is a *new economic fact* (terms were not met, the bonus
  expired, RG/self-exclusion intervened), it is frequently partial, and it
  frequently occurs long after the grant. `reverses_transaction_id` is
  reserved for "this transaction should never have been posted" (§7).
- The forfeited amount is the **currently outstanding** bonus attributable
  to the grant, never the original grant amount. Value already consumed
  keeps its recognized expense; it cannot be un-spent.
- Attribution across multiple concurrent grants (which grant's remaining
  balance is being forfeited — FIFO, last-in, per-grant lot tracking) is a
  **Bonus Engine business rule**, not a ledger concern. The ledger records
  the amount instructed and enforces one thing: the sum forfeited may
  never exceed the wallet's `player_bonus` balance, checked `FOR UPDATE`
  in-transaction. Grant attribution is carried on the transaction's
  correlation/causation metadata so the Progress trail
  (`10-bonus-engine-architecture.md`) and the ledger tell the same story.
- Expiry is a **posted transaction on the expiry date, not a filtered
  read**. A balance that silently stops counting at read time is not
  auditable, not reproducible as-of a past date, and unanswerable when a
  player disputes it. There will be such players every week.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 6. Who bears the cost — three genuinely different scenarios

The player-facing postings (`player_bonus`, `player_cash`) are **identical
in all three cases where value actually enters our wallet**: a player must
not be able to tell from their balance who funded the promotion. What
differs is the cost side.

The funding attribution is fixed on the grant record **at grant time and
is immutable thereafter**. Re-attributing cost after the fact would mean
editing history; a genuine change of attribution is itself a new
compensating transaction with a reason code, never an amendment.

#### (a) Operator-funded — the tenant absorbs the cost

The default, and everything in §2–§5 above. Recognition debits
`bonus_expense` for that tenant and asset. The cost lands in the tenant's
own P&L.

#### (b) Provider-funded — a provider absorbs the cost

A casino game provider funds a free-spin package as a marketing
contribution. The platform still owes the player the resulting value, so
grant/conversion/forfeiture post exactly as in §3–§5 — **with one
substitution**: at every point where the operator-funded case debits
`bonus_expense`, the provider-funded case debits **`provider_payable`**
instead.

| Event (provider-funded) | Entries |
|---|---|
| Grant `X` | Dr `promo_liability` `X` · Cr `player_bonus` `X` (identical to §3) |
| Recognition (conversion / consumption) `X` | Dr `provider_payable` `X` · Cr `promo_liability` `X` |
| Forfeiture `X` | Dr `player_bonus` `X` · Cr `promo_liability` `X` (identical to §5) |

Rationale for reusing `provider_payable` rather than adding a thirteenth
account type: `provider_payable` is the Blueprint's existing credit-normal
"amount owed to a provider" account, settled periodically through Flow 17.
A provider-funded promotion reduces what we owe that provider — in
practice these deals settle as an offset against the monthly GGR-share
invoice, which is exactly a debit to `provider_payable`. No new account is
needed, the provider settlement reconciliation stream
(`reconciliation-model.md` §2.5) picks it up unchanged, and the figure
appears where a finance reviewer already looks for provider balances.

Two things this requires and does not assume:

- The grant must carry `funding_source = provider` **and** the funding
  `provider_id`, resolved server-side. A provider-funded attribution that
  the platform cannot tie to a specific provider agreement is rejected,
  not defaulted to operator-funded and not defaulted to provider-funded.
- **`OPEN DECISION` (commercial, human sign-off required).** Whether
  provider-funded promotions in fact settle by invoice offset or by
  separate cash settlement, and whether the `house_gaming` revenue
  produced by a provider-funded free spin is the operator's revenue at
  all, are **contract terms**, not engineering choices. CLAUDE.md puts
  provider contracts squarely in "stop and ask". The postings above are
  correct for the offset model; if a provider contract says otherwise,
  this section is amended by a new decision, not by a code workaround.

#### (c) Externally-fulfilled — the provider's own bonus engine owns it end to end

A sportsbook (or casino) provider grants, tracks, wagers and settles a
bonus entirely inside its own system, using its own balance. The value
never enters a platform wallet.

**Treatment: the ledger posts nothing. Zero entries. No
`promo_liability`, no `player_bonus`, no `bonus_expense`.**

- The platform records the *fact* as a domain event plus an `audit.Record`
  (for reporting, RG/limit visibility, and player-support answerability),
  and reconciles it against the provider's own statement as a **memo
  stream** — counts and references, not balances (see §8).
- **The platform's ledger never mirrors a balance held in a provider's
  system.** This is the hard rule of this section. A mirrored balance is a
  second truth system by definition, it drifts the moment the provider
  settles something we did not see, and there is no correct resolution for
  that drift because we cannot correct their ledger. Recording a liability
  we do not owe is exactly as wrong as omitting one we do.
- The moment external value genuinely lands in our wallet — the provider
  settles a free-bet win as a real payout — that is an **ordinary provider
  settlement posting** into `player_cash`, keyed on `(tenant_id,
  provider_id, provider_tx_id)`, under the Flow 9 shape. It is cash from a
  provider, **not** a bonus grant, and it must not create bonus balance or
  wagering requirements on our side.
- If the provider bills us for the cost of its externally-fulfilled bonus,
  that is `provider_payable` and Flow 17. It is not bonus accounting, and
  it does not retroactively make the bonus ours.
- Consequence for the Reward Orchestrator and the External Reward Provider
  contract being designed in parallel: an external reward's contract must
  declare, per reward type, whether fulfilment is *into our wallet*
  (→ §6(a)/(b)) or *inside the provider* (→ this section). A reward type
  that does not declare it is rejected at configuration time rather than
  guessed at posting time.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`** for (a) and (c);
**(b) is RESOLVED structurally but `PROVIDER DEPENDENT`** on the actual
funding-deal terms, with the commercial `OPEN DECISION` above.

### 7. Reversal, cancellation and rollback — identical to the established pattern

`ARCHITECTURAL DECISION`, inherited wholesale. **Bonus reversal follows the
exact pattern already implemented for casino and payments — a new
`LedgerTransaction` whose `reverses_transaction_id` points at the specific
original, carrying the exact inverse entries. Historical entries are never
edited and never deleted.** See `internal/casino`'s `postRollback` and
`internal/payments`' deposit-reversal path; nothing here is new
mechanism.

- **Reversal vs. forfeiture is a real distinction, not a synonym.**
  Reversal = "this grant should never have been posted" (duplicate campaign
  fire, operator error, fraud, a rescinded award). Forfeiture (§5) = "the
  grant was valid, the terms were not met". Different transaction types,
  different accounting, both append-only.
- A reversal's entries mirror the original's **including the §2 mirror
  legs**, which fall out automatically because those legs live in the same
  transaction being reversed. A rollback of a bonus-funded casino bet
  therefore restores `player_bonus`, `house_gaming`, `promo_liability` and
  `bonus_expense` together, with no special-case code.
- **Double-reversal protection:** the original is selected `FOR UPDATE`
  and rejected if a transaction already reverses it — the same row-lock
  the casino rollback path uses after a specialist review empirically
  reproduced two concurrent rollbacks both posting a reversal. A row lock
  on an append-only table is not a mutation and does not trip the
  append-only trigger.
- **Reversal of a never-seen grant writes a tombstone** occupying the same
  idempotency slot (`(tenant_id, provider_id, provider_tx_id)` or
  `(tenant_id, idempotency_key)`), so a late-arriving original is rejected
  rather than posted after its own cancellation. CLAUDE.md's rollback rule,
  same mechanism as `postRollbackTombstone`.
- **A reversal must fail loudly if the granted value has already been
  partly consumed.** Value already wagered or converted cannot be
  un-granted; the ledger would have to create a negative bonus balance to
  do it. The correct instrument in that case is: forfeit the remaining
  balance (§5), and handle the consumed portion as a `manual_adjustment`
  **against `player_cash`** (recouping realized value from the player's
  withdrawable balance), with a mandatory reason code and four-eyes
  approval above the configured threshold (CLAUDE.md). It does **not**
  target `player_bonus`: the consumed value has already left that account
  and recreating it there would reopen a wagering obligation for value the
  player has already spent. If a correction genuinely must move
  `player_bonus`, it carries the §2 mirror legs like any other
  `player_bonus` entry. Silently reversing to a negative balance, or
  clamping the reversal to what remains and calling it a full reversal,
  are both rejected. **Wave-2 ledger-finance review correction (P1-4)**:
  an earlier draft named no target account for the manual adjustment,
  leaving `player_bonus` as the most likely implementer choice — exactly
  the account this correction must not touch.

Status: **RESOLVED (architecture, inherited) — `NOT IMPLEMENTED`.**

### 8. Idempotency — inherited from ADR 0020, not reinvented

Nothing in bonus accounting gets a bespoke idempotency mechanism. ADR 0020
applies verbatim:

- **Provider-originated bonus postings** (provider-issued free rounds,
  external reward provider callbacks): `UNIQUE (tenant_id, provider_id,
  provider_tx_id)` on `ledger_transactions`, with `provider_id` resolved
  from the **credential that verified the callback signature**, never from
  the payload (ADR 0019's actor matrix).
- **Internally-originated bonus postings** (grant, conversion,
  forfeiture, reversal, manual award): `UNIQUE (tenant_id,
  idempotency_key)`, where the key is the originating system's own stable
  per-business-fact `idempotency_key` (`22-canonical-activity-event-
  taxonomy.md`'s `idempotency_key` field — **never** its per-publish
  `event_id`, which may differ across redeliveries of the same fact) — the
  Bonus Engine's grant/conversion/forfeiture id, or the Reward
  Orchestrator's reward-issuance id — **generated once when the operation
  is first accepted and reused verbatim on every retry, never regenerated
  per delivery attempt.** A Reward Orchestrator that mints a fresh key per
  attempt has defeated the mechanism entirely; this is the single most
  likely way for this design to fail in practice, so it is called out
  here rather than assumed. (Wave-2 ledger-finance review, P2: an earlier
  draft called this key the originating system's "event id," the exact
  terminology doc 22 reserves for the per-publish field — corrected here
  since this is the authoritative money ADR.)
- Both keys are **tenant-scoped** (`ledger-accounting-model.md` §3).
- Exact retry returns the original result via the `SAVEPOINT` pattern in
  `db.IdempotentInsert`. Same key with a different payload is **rejected**
  (`ledger.ErrIdempotencyKeyReused`), never silently applied and never
  silently treated as a match. Concurrent duplicates are arbitrated by the
  database constraint, never by check-then-insert.
- Authorization: ADR 0019's actor matrix already assigns `bonus_grant`,
  `bonus_conversion` and `bonus_forfeiture` to an **internal service
  identity** (ADR 0014), and explicitly forbids a player session from
  originating any of them or any direct credit to `player_bonus`. A
  `bonus_reversal` type must be added to that matrix under the same
  service-identity row, and Gamification/Reward Orchestrator postings
  inherit it unchanged — they are internal services, not a new actor
  class.

Status: **RESOLVED (inherited) — `NOT IMPLEMENTED`.**

### 9. No floating point anywhere near a bonus calculation

`ARCHITECTURAL DECISION`, restated because bonus configuration is where
this rule is most likely to be broken. Match percentages, contribution
percentages, wagering multipliers and cashback rates are not money, but
they **multiply** money — so, exactly as ADR 0021 requires for
`exchange_rate`, they are `NUMERIC`, never `FLOAT`/`DOUBLE`/`REAL`. A
100%-match-capped-at-€X computed in floating point is a defect and a
blocking review finding, not a shortcut.

The bonus amount must be exactly recomputable from the stored inputs
(base amount, stored rate, stored cap, stored rounding rule) as a
reconciliation check. Rounding uses the **same** rule ADR 0021's open
rounding decision settles on — never a second, different convention for
bonuses — and until that decision is made, bonus amounts are computed by
one shared helper rather than per-campaign arithmetic.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.** Rounding
direction remains ADR 0021's `OPEN DECISION` (finance).

### 10. Known gap this ADR surfaces: `player_locked` loses stake origin

`OPEN DECISION` / **blocking precondition**, found while checking
invariant B1 against `financial-transaction-flows.md` Flow 8.

Flow 8 posts a sportsbook stake as `player_cash`/`player_bonus` →
`player_locked`, a **single** account type. That loses the origin of the
locked funds, with two consequences: settlement cannot know whether to
return the stake to cash or to bonus, and invariant B1 breaks the moment a
bonus-funded stake is locked (the value has left `player_bonus` but the
player may still get it back, so `promo_liability` has nothing correct to
mirror).

`RECOMMENDATION`: split into `player_locked_cash` and
`player_locked_bonus` (or carry an equally binding, indexable origin
dimension), and extend invariant B1's account set to include the
bonus-origin locked account. This does **not** block Stage 4H-A and does
**not** affect casino, which has no locked state. It **is** a blocking
precondition for implementing bonus-funded sportsbook stakes, and it needs
`architect` + `sportsbook` + `ledger-finance` agreement because it changes
a Blueprint-listed account type.

Status: **`OPEN DECISION` — not resolved here, deliberately.** Raised to
the Master Orchestrator.

## Consequences

- **Follow-up edits required to documents this ADR does not own the right
  to change in this stage** (all `NOT IMPLEMENTED` until made):
  `ledger-accounting-model.md` §2 (add `bonus_expense`, close the
  `promo_liability` `OPEN DECISION`, update the `player_bonus` row's
  allowed transaction types) and §6 (add invariant B1);
  `financial-transaction-flows.md` Flows 5/6/7/9/11/12/14/15/20, a new
  externally-fulfilled flow, and the summary table (four-entry
  conversion, mirror legs on every bonus-funded play flow, provider-funded
  variant, new Flow for externally-fulfilled = no posting) — **Wave-2
  ledger-finance review correction (P1-1)**: an earlier draft of this
  bullet named only Flows 12/14/15, omitting the bonus-funded play flows
  that carry the highest volume of `player_bonus` entries and would have
  shipped with no mirror-leg guidance at all; `reconciliation-model.md`
  (add the B1 stream and the externally-fulfilled memo stream); ADR
  0019's actor matrix (`bonus_reversal`).
- **Migrations required before any bonus posting** (additive, own stage):
  `bonus_expense` account type; `bonus_grant`, `bonus_conversion`,
  `bonus_forfeiture`, `bonus_reversal` transaction types added to the
  `CHECK` constraint on `ledger_transactions.transaction_type`. Until then
  bonus postings are `BLOCKED` by the existing constraint — which is the
  intended behaviour, not an obstacle to route around.
- **New reconciliation stream (B1), hourly, zero tolerance, P1 on drift** —
  the same class as ledger-vs-projection. It is cheap (two aggregate reads
  per tenant+asset) and it catches the entire family of bonus bugs that
  would otherwise surface as an unexplained `promo_liability` balance
  months later.
- **Write contention increases.** `promo_liability` and `bonus_expense` are
  single house-level rows per tenant+asset touched by *every* bonus-funded
  bet, materially widening the contention surface ADR 0019 already flagged
  for `house_gaming`. Sharding those projection rows or accepting
  serialized posting per tenant+asset is a throughput design point for
  `architect`; it is not a reason to weaken B1.
- **Testing floor** (CLAUDE.md's financial test list, non-negotiable, owned
  by `ledger-finance`): grant/convert/forfeit/reverse happy paths;
  duplicate and replayed grants; same-key-different-amount rejection;
  concurrent grants to one wallet; concurrent conversion vs. bonus-funded
  bet on one wallet; conversion exceeding balance; reversal of a partly
  consumed grant (must fail); double reversal race; reversal of a
  never-seen grant (tombstone, then late original rejected); rollback of a
  bonus-funded bet restoring all four accounts; provider-funded
  recognition landing in `provider_payable` and never in `bonus_expense`;
  an externally-fulfilled reward posting **zero** ledger entries; B1
  asserted after every one of the above. A happy-path grant test does not
  satisfy this.
- **`ledger-finance` sign-off is required** on the Bonus Engine,
  Gamification, Reward Orchestrator and External Reward Provider designs
  before any of them is marked complete, limited to their monetary
  aspects. Nothing in this ADR gives `ledger-finance` a view on bonus rule
  logic, campaign design, or gamification mechanics.

## Open decisions referred upward

1. `bonus_expense` presentation in statutory/partner reporting (expense
   line vs. contra-revenue) — finance.
2. Provider-funded settlement mechanics and whether provider-funded
   `house_gaming` revenue is the operator's — **commercial contract, human
   sign-off** (CLAUDE.md "stop and ask").
3. Physical/non-wallet prize cost recognition — finance.
4. `player_locked` origin split (§10) — `architect` + `sportsbook` +
   `ledger-finance`.
5. Rounding direction for bonus amount computation — inherits ADR 0021's
   open rounding decision; must not be answered separately for bonuses.
6. Pooled and guaranteed tournament prize-pool liability accounting
   (`18-tournament-architecture.md` §7/§14.3 — "an operator liability
   `ledger-finance` must own") — added by Wave-2 ledger-finance review
   (P2); not resolved here because Tournaments are not authorized for
   implementation this stage, but the liability-accounting question is
   `ledger-finance`'s to answer, not `architect`'s, when they are.
7. Rounding/remainder rules for split prizes and `pro_rata` partial mission
   rewards (`18-tournament-architecture.md` §14.2, `19-mission-
   architecture.md` §11) — added by Wave-2 ledger-finance review (P2);
   must resolve to the same shared rounding helper as item 5 above, not a
   second convention invented independently for prize-splitting.

## Owner

`ledger-finance`. Bonus rule logic is owned by `bonus-engine`, gamification
mechanics by `architect`, risk controls by `risk`; none of them may alter
the accounting treatments above without `ledger-finance` sign-off, and
`ledger-finance` does not alter their rules.

## Cross-references

`CLAUDE.md` ("Financial / ledger rules"); ADR 0001, 0007, 0014, 0019,
0020, 0021; `docs/architecture/06-wallet-ledger-architecture.md`,
`ledger-accounting-model.md`, `financial-transaction-flows.md`,
`reconciliation-model.md`, `10-bonus-engine-architecture.md`;
`docs/architecture/24-points-accounting-architecture.md` (points are a
separate ledger and are **not** covered by this ADR); `internal/ledger`,
`internal/casino` (`postRollback`), `internal/payments`.

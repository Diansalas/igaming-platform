# ADR 0038 — Sportsbook Accounting and Ledger Integration

Status: Proposed (Stage 4H-B0-R4, architecture only — **`NOT IMPLEMENTED`**).
No migration, no Go code, and no `transaction_type`/`account_type` value in
this document exists yet. Owner: `ledger-finance`. This ADR is the detailed
financial/ledger-integration companion `docs/architecture/09-sportsbook-
architecture.md` points to; `sportsbook` owns doc 09's product/build-vs-buy
framing, this ADR owns exactly how every sportsbook financial fact becomes
a balanced `LedgerTransaction`. Mirrors `docs/decisions/0032-bonus-
accounting.md`'s structure throughout, because it is the closest existing
precedent for a domain-specific accounting ADR and there is no reason to
invent a second shape for the same kind of document.

**Amended in Stage 4H-B0-R5** (final pre-implementation gate — closes the
two P1s `qa`'s Stage 4H-B0-R4 review left open): §14 is new and
**RESOLVES** P1-3 (the idempotency design needed a per-occurrence
distinguishing field before implementation) as an `ARCHITECTURAL DECISION`
— `ledger-finance`'s to make, since it is a mechanism-level tightening of
this ADR's own idempotency design, not a change to the human-approved
account-type schema. §15 is new and **PROPOSES, for independent
`architect` + `bonus-engine` + `sportsbook` review, NOT a unilateral
decision** — a resolution to P1-4 (the pre-existing `player_locked`
origin-split gap, §9). The full, platform-wide proposal lives in
`ledger-accounting-model.md` §6.3 (not sportsbook-specific, per that P1's
own instruction not to build a sportsbook-specific workaround); §15 here
is the sportsbook-specific instantiation and cross-reference. Neither
addition changes any entry already specified in §3–§13; both are
additive.

Labeling convention inherited from `financial-domain-model.md`: `BLUEPRINT`
= stated directly in the Blueprint; `ARCHITECTURAL DECISION` = decided here
or in a named prior ADR; `OPEN DECISION` = deliberately not resolved, with
the reason and the required decision-maker named; `RECOMMENDATION` = a
`ledger-finance` position that still needs a business/finance/human
sign-off before it binds.

## Context

`docs/architecture/09-sportsbook-architecture.md` (Stage 5 scope, being
substantially rewritten in parallel by `sportsbook` this same stage)
already establishes the framing this ADR builds on and does not
relitigate:

- An open sportsbook bet is a liability that can span days or months,
  unlike a casino round that settles in milliseconds.
- Stake moves to `player_locked` at placement.
- An open-bet record carries potential return.
- Settlement, void, partial settlement (bet-builder legs), and cashout are
  each **distinct ledger events**, never conflated into one "resolve bet"
  operation.
- Markets can be corrected after initial settlement, requiring
  re-settlement as its own event, never a silent balance edit.
- Sportsbook GGR is only known at settlement, so every sportsbook report
  needs an explicit open-liability line.
- The build/buy line runs through a widget/iframe provider first
  (RECOMMENDATION, per the Blueprint) with a feed/API in-house-UI shape
  deferred; **either shape produces the same financial facts** — a bet is
  placed, accepted or rejected, and later settled, voided, partially
  settled, cashed out, or corrected. This ADR's postings are written
  against those facts, not against either integration shape, so they hold
  unchanged whether the originating event arrives as a provider webhook
  (widget/iframe or feed/API) or as an internal in-house pricing/settlement
  engine's own decision. Where the two shapes genuinely differ — who
  computes an amount, and therefore who is responsible for rounding it —
  is called out explicitly in §7.

`player_locked` and its allowed transaction types already exist in
`ledger-accounting-model.md` §2's table (as forward references to this
ADR) and in `financial-transaction-flows.md` Flows 8–11 (as `ARCHITECTURAL
DECISION`/`BLUEPRINT`-labeled sketches). This ADR is the authoritative
detail behind those forward references: where they and this ADR conflict,
this ADR wins for sportsbook specifics, and a follow-up edit to both
documents is required (§10).

**What this ADR explicitly does not do.** It does not design the
sportsbook domain model (`OpenBet`, provider adapter, odds/trading logic —
`sportsbook`'s), it does not resolve the `player_locked` origin gap for
bonus-funded stakes (already an `OPEN DECISION` in `ledger-accounting-
model.md` §6.2 and ADR 0032 §10, referred to `architect` + `sportsbook` +
`ledger-finance` jointly — this ADR treats it as a still-open blocking
precondition, not something to re-decide unilaterally here), and it does
not invent a second rounding convention (§7 confirms reuse of ADR 0021's
resolved decision without exception).

## Decision

### 0. Position statement — one financial truth system, restated for sportsbook

`ARCHITECTURAL DECISION`, inherited verbatim from ADR 0032 §0 and binding
here without modification.

The sportsbook engine (whichever integration shape — widget/iframe
provider callback, feed/API adapter, or a future in-house pricing/trading
engine) is a **decision system**: it decides which bets are accepted, what
odds apply, when and how an event settles, whether a cashout offer is
honored, and when a market needs correcting. It is not an accounting
system. Every monetary consequence of those decisions is recorded by
`internal/ledger`, through `ledger.Post`, under the same invariants as a
deposit, a casino bet, or a bonus grant — append-only, double-entry,
balanced per asset, DB-enforced idempotency, compensating corrections
only, projection-never-authoritative, reconciled on a schedule.

Binding consequences, each a blocking `ledger-finance` review finding if
violated:

- No sportsbook table holds an authoritative monetary balance. An
  `OpenBet`'s `potential_return` field (§2) is a domain projection, not a
  balance, and must never be read as one.
- No sportsbook code path `UPDATE`s a balance, mutates a historical entry,
  or posts a money movement without an idempotency key.
- The platform's aggregate open-bet liability is a **derived read** over
  `player_locked` balances (§6), never a counter maintained by the
  sportsbook domain that could drift from the ledger entries it claims to
  summarize — the identical reasoning ADR 0032 §0 applies to wagering
  progress.
- `internal/ledger` remains the only package that writes
  `ledger_accounts`/`ledger_transactions`/`ledger_entries` (ADR 0001). The
  sportsbook engine or provider adapter supplies the instruction (which
  event happened, for which bet, for how much); `ledger-finance` owns the
  posting mechanism that turns it into balanced entries. Identical
  boundary to `10-bonus-engine-architecture.md` §6's "Bonus Engine
  computes *what* the split should be; it never issues the ledger
  posting itself" — the sportsbook engine never issues a
  `player_cash`/`player_bonus`/`player_locked`/`house_gaming` posting
  itself either.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 1. Account types — none new required

`ARCHITECTURAL DECISION`. Unlike bonus accounting (which needed a new
`bonus_expense` account type), sportsbook needs **zero new account types**.
Every account sportsbook touches already exists in `ledger-accounting-
model.md` §2:

| Account type | Role in sportsbook |
|---|---|
| `player_cash` | Stake source (cash-funded) and payout destination |
| `player_bonus` | Stake source (bonus-funded) and payout destination — **blocked** until the `player_locked` origin split is resolved (§2, §9) |
| `player_locked` | Holds the stake for the life of an open bet |
| `house_gaming` | Absorbs settled stakes as revenue and pays out wins/cashouts/partial settlements — the same account casino uses, per `ledger-accounting-model.md` §2's existing "Allowed transaction types" column, which already lists the sportsbook transaction types below against `house_gaming` |
| `promo_liability` / `bonus_expense` | Mirror legs on the bonus-funded portion of any sportsbook posting — **blocked** for the identical reason as `player_bonus` above (§2, §9) |
| `manual_adjustment` | Staff corrections that are not a market re-settlement (rare; §8's compensating-entry path is preferred whenever the correction traces to a specific bet) |

No `sportsbook_payable`/`sportsbook_liability`/`sportsbook_expense` or any
other new account type is introduced. `provider_payable` (existing) is
reused unchanged for periodic GGR-share/fee settlement with a sportsbook
odds provider, under Flow 17 — sportsbook does not need its own variant of
that flow.

Status: **RESOLVED — `NOT IMPLEMENTED`.**

### 2. Potential return — a domain fact, never a ledger fact

`ARCHITECTURAL DECISION`, answering doc 09's "an open-bet record carries
potential return" precisely, because leaving it ambiguous is exactly the
kind of gap that produces an invented ledger posting later.

**Potential return is a field on the sportsbook domain's `OpenBet` record
(`OpenBet.potential_return`), computed at acceptance from the locked stake
and the accepted odds (or, for a bet-builder/combo, the combined odds of
its legs). It is never posted as a `LedgerEntry` and creates no
ledger-visible liability of its own.** The only ledger-visible fact for an
open bet is the stake amount sitting in `player_locked` — an actual,
posted, reconciliable movement. `potential_return` is a contingent
projection: real money changes hands only when settlement (§4/§5), a
partial settlement (§8), or a cashout (§9) determines an actual amount to
pay, exactly as ADR 0032 §3 treats an unconverted bonus grant as a
"contingent liability, not a recognized cost" until the value actually
leaves `player_bonus`. Reusing that same distinction here, rather than
inventing a second one, keeps "contingent vs. realized" meaning the same
thing everywhere in the platform's accounting.

**Consequence for reporting (ties to doc 09's reporting-consequence
section, and to §6 below).** Two different "open sportsbook exposure"
numbers exist and must never be presented as one:

1. **Open-bet stake liability** — `Σ signed(player_locked)` per
   tenant+asset (§6). Ledger-derivable, reconciliable, authoritative. This
   is "what the platform would owe back if every open bet were voided
   right now."
2. **Potential payout exposure** — `Σ OpenBet.potential_return` for every
   open bet, per tenant+asset. **Not** ledger-derivable, because no money
   has moved for the winnings portion of any open bet. This is a trading/
   risk number ("what the platform would owe if every open bet won"),
   owned entirely by the sportsbook domain's own open-bet table, computed
   as its own derived read (never a maintained counter, for the identical
   drift-prevention reason `player_locked`'s sum must be a derived read
   and not a cached total) and out of `ledger-finance`'s scope to specify
   further. A report that shows number 2 as if it were number 1 (or vice
   versa) is a defect.

Status: **RESOLVED — `NOT IMPLEMENTED`.**

### 3. Bet placement (Flow 8 detail)

`BLUEPRINT`/`ARCHITECTURAL DECISION`, per `financial-transaction-flows.md`
Flow 8, restated with the precision this ADR owes it.

| Event | Entries |
|---|---|
| Bet placed, stake `S` (cash-funded) | Dr `player_cash` `S` · Cr `player_locked` `S` |
| Bet placed, stake `S` (bonus-funded) | **Blocked** — see §9. Once unblocked: Dr `player_bonus` `S` · Cr `player_locked_bonus` `S`, plus the ADR 0032 §2 mirror pair (Dr `bonus_expense` `S` · Cr `promo_liability` `S`) is **not** posted at placement, because no value has left `player_bonus` for a non-forfeiture reason yet — the stake is still contingent, exactly like an unconverted grant. The mirror pair only fires when the stake is actually consumed (§5's loss case) or returned unwagered (§6/§9's void/cashout-to-bonus case, which needs no mirror pair since nothing left `player_bonus` net) |

Two entries for the cash-funded case, matching Flow 8 exactly. Insufficient
funds is checked in the same database transaction that would post the
lock (invariant #15) — a rejected placement leaves no `LedgerTransaction`
row at all (§3, `ledger-accounting-model.md` §4), identical to casino's
insufficient-funds path.

**Idempotency key**: `(tenant_id, provider_id, provider_bet_reference)`,
where `provider_bet_reference` is the specific, immutable reference the
provider (or the in-house engine, using its own internally-generated
reference in the identical role) assigns to **this bet at acceptance** —
never the player's slip-submission attempt id, which may be retried before
acceptance. `transaction_type = 'sportsbook_bet'`.

**Correlation.** `correlation_id` is set, at this first transaction, to a
stable **internal bet id** minted by the sportsbook domain (or the ledger
posting layer, if the domain has none of its own yet) — not the provider's
reference. Every later transaction against this same bet (settlement,
void, partial settlement, cashout, resettlement) carries the **same**
`correlation_id`, so the bet's full financial history is reconstructable
by one query without depending on the provider ever correlating its own
references to each other. `causation_id` on this transaction is the
provider's acceptance callback's own delivery id (or, in-house, the
internal acceptance decision's id) — distinct from `provider_bet_reference`
per the platform's existing `provider_tx_id` vs. `causation_id` distinction
(`ledger-accounting-model.md` §1.2).

**Audit event**: `sportsbook_bet.locked`.

**Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #12, #15.

Status: **RESOLVED (cash-funded) — `NOT IMPLEMENTED`.** Bonus-funded case
**BLOCKED**, see §9.

### 4. Bet rejection — no ledger row, ever

`ARCHITECTURAL DECISION`, closing the gap doc 09/Flow 8 leave implicit.

**The ordinary case: no funds ever moved, so no `LedgerTransaction` is
ever created.** A bet rejected for odds-changed, market-suspended, stake
outside provider limits, or any other pre-acceptance reason is, by
`ledger-accounting-model.md` §4's own rule, invisible to the ledger by
construction — exactly like a declined casino bet or a declined deposit.
There is nothing to reverse because nothing was ever posted.

**The optimistic-hold case, named explicitly because it is the trap this
ADR must close.** Some provider/UI flows optimistically place a hold
(reserve funds client-side or session-side) before the provider's
acceptance confirms, to avoid a race where the player's balance changes
between slip submission and acceptance. **If, and only if, that
optimistic hold is itself backed by a posted `sportsbook_bet`
`LedgerTransaction`** (i.e., the platform chose to lock the stake before
knowing acceptance would succeed, rather than holding it in an
unposted, provider/session-side reservation), rejection **must** be posted
as its own explicit reversal:

| Event | Entries |
|---|---|
| Optimistic lock released on rejection, stake `S` | Dr `player_locked` `S` · Cr `player_cash`/`player_bonus` `S` — exact inverse of §3, `reverses_transaction_id` set to the lock transaction |

`transaction_type = 'sportsbook_void'` (reused — a rejected-after-
optimistic-lock bet is, from the ledger's point of view, indistinguishable
from a void-before-settlement: the full stake returns, the bet never
became a wagering fact). **This is never a silent no-op.** A rejection
that leaves a posted `player_locked` entry unreleased is a stuck lock —
exactly the failure mode this section exists to name and prevent — and
must alert as an integrity problem (a reconciliation break under §6) if it
is ever observed with no corresponding release within the provider's own
acceptance-timeout window.

**RECOMMENDATION**: the sportsbook domain should prefer never posting a
`sportsbook_bet` lock until acceptance is confirmed (holding the
optimistic reservation entirely outside the ledger, the same "pending
lives outside the ledger until it's a fact" pattern `ledger-accounting-
model.md` §4 already uses for deposits/withdrawals), which makes this
reversal path unnecessary in the common case. Where a specific provider's
protocol makes pre-acceptance locking unavoidable (e.g. the provider
requires the platform to reserve first and confirms asynchronously), the
reversal path above is mandatory, not optional.

**Idempotency key**: the rejection event's own provider reference, distinct
from the (now-void) acceptance reference — same pattern as Flow 4's
withdrawal pre-submission rejection.

**Audit event**: `sportsbook_bet.rejected` (no-lock case) /
`sportsbook_bet.rejected_lock_released` (optimistic-lock case).

**Invariants engaged**: none (no-lock case, `ledger-accounting-model.md`
§4's own no-op-by-absence rule) / #1, #2, #3, #4, #5, #10, #14 (optimistic-
lock case).

Status: **RESOLVED — `NOT IMPLEMENTED`.**

### 5. Settlement — win and loss (Flow 9 detail)

`BLUEPRINT`, per `financial-transaction-flows.md` Flow 9, restated with
the bonus-blocking status made explicit and the rounding boundary named.

| Event | Entries |
|---|---|
| Loss, stake `S` | Dr `player_locked` `S` · Cr `house_gaming` `S` |
| Win, stake `S`, full payout `S+W` | (1) Dr `player_locked` `S` · Cr `house_gaming` `S` — stake absorbed; (2) Dr `house_gaming` `S+W` · Cr `player_cash` `S+W` — full payout, **not** winnings `W` alone (Flow 9's own documented trap: pairing a winnings-only debit against an `S+W` credit does not balance) |

Two balanced pairs on a win, `S`=`S` and `(S+W)`=`(S+W)`, satisfying
invariant #1 as a whole. This is the exact shape Flow 9 already specifies;
this ADR adds nothing new to the entries themselves and instead pins down
what Flow 9 left generic.

**Where the payout amount `S+W` comes from, and who is responsible for it
being correct — the load-bearing distinction for rounding (§7).** In the
widget/iframe or feed/API provider-driven shape (the near-term reality per
doc 09's build/buy line), the provider computes `S+W` from its own
odds/settlement engine and states it in the settlement callback as an
already-rounded minor-unit integer. **The ledger posts exactly what the
callback states; it does not recompute, validate, or re-round the
odds/payout math.** This is the same posture Flow 11's existing `OPEN
DECISION` already states for cash-out margin and is restated here for
ordinary settlement so it is not assumed differently for the two flows.
Reconciliation for this posting is therefore against the **provider's own
settlement statement** (mirroring Flow 17's provider-settlement
reconciliation), not a recomputation from stored odds — the ledger simply
has no odds of its own to recompute from in this shape.

In a future in-house pricing/settlement engine (the feed/API "platform
computes its own odds" shape doc 09 explicitly defers), the platform
itself would compute `S+W = S × decimal_odds` (or a combined-odds product
for a bet-builder). That computation is a percentage/rate-of-money
calculation and **must** use ADR 0021's one shared rounding helper
(round-half-up, applied once at the final monetary boundary, full
`NUMERIC` precision until then — DS-1/DS-2/DS-3, unchanged, no second
convention). `decimal_odds` (and any per-leg odds in a combo) would be
stored `NUMERIC`, never `FLOAT`/`DOUBLE`, exactly as ADR 0021 already
requires for `exchange_rate`. This case does not exist yet — no in-house
pricing engine is authorized this stage — and is named here only so it is
not later "discovered" as a second rounding decision when it is built.

**Bonus-funded win — blocked.** Where the original stake locked from
`player_bonus`, this settlement is **blocked** by the same `player_locked`
origin gap as placement (§9) — settlement cannot know which account to
return the payout to until the origin split exists. Once unblocked, a
payout crediting `player_bonus` carries the identical ADR 0032 §2 mirror
pair (`promo_liability`/`bonus_expense`) Flow 6/9 already specify for
casino, generated by the same shared ledger posting layer — no
sportsbook-specific mirror logic is invented.

**Idempotency key**: `(tenant_id, provider_id, provider_settlement_reference)`
— the settlement event's own reference, distinct from
`provider_bet_reference` (§3). A settlement callback that references a bet
slip with no matching `player_locked` entry is rejected and escalated as
an integrity alert, identical to Flow 9's stated failure behavior.

**Correlation**: `correlation_id` = the same internal bet id §3 minted.

**Audit event**: `sportsbook_bet.settled`.

**Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #12, #13, B1 (once
bonus-funded stakes are unblocked).

Status: **RESOLVED (cash-funded) — `NOT IMPLEMENTED`.** Bonus-funded case
**BLOCKED**, see §9.

### 6. Exposure and liability accounting — derived read, no maintained counter

`ARCHITECTURAL DECISION`, directly answering doc 09's "every sportsbook
report needs an explicit open-liability line."

**The platform's aggregate open-bet stake liability, per `(tenant_id,
asset_code)`, is:**

```sql
SELECT SUM(le.amount) FILTER (WHERE le.direction = 'credit')
     - SUM(le.amount) FILTER (WHERE le.direction = 'debit') AS open_liability
FROM ledger_entries le
JOIN ledger_accounts la ON la.id = le.ledger_account_id
WHERE la.account_type = 'player_locked'
  AND la.tenant_id = $1
  AND la.asset_code = $2
  AND le.created_at <= $3;   -- or a join+filter on ledger_transactions.posted_at, per
                              -- ledger-accounting-model.md §5's note on the two timestamps
```

This is the **same formula shape** `ledger-accounting-model.md` §5 already
defines for any single account's balance, summed across every wallet's
`player_locked` account for the tenant+asset rather than one wallet's. It
is deliberately **not** a maintained counter (no `open_sportsbook_liability`
column anywhere), for the identical reason ADR 0032 §0 requires wagering
progress to be a derived read: a maintained counter is a second truth
system that can drift from the entries it claims to summarize, and the
underlying entries already make the number cheap to compute (a single
indexed aggregate per tenant+asset, no worse than the existing wallet
balance projection query pattern).

**Materialization, if read performance demands it**: exactly the
`reconciliation-model.md` §3 pattern — a **subordinate, rebuildable cache**
(e.g. a periodic materialized view refreshed on the platform's existing
projection cadence), never a second source of truth, and never read on any
bet/settlement path (CLAUDE.md: "Redis (or any cache) never holds an
authoritative balance and is never read on the bet/settlement path").

**Reporting consequence, stated explicitly per doc 09**: a sportsbook
GGR/NGR report for period `P` is incomplete without this figure as of
`P`'s end, alongside realized `house_gaming` activity for `P` — a report
that shows only settled revenue overstates a day with many still-open
bets and understates the day a large accumulator settles. This is a
`data-analytics` reporting-layer consumption of the query above, not a new
ledger write.

**Consequence for reconciliation**: the daily/hourly reconciliation stream
this figure feeds compares `Σ player_locked` against the sportsbook
domain's own count of currently-open bets' stakes (a cross-check that the
domain's own bet-state view and the ledger's locked-funds view agree) —
the identical "two independently derived views of the same fact must
match" pattern `ledger-accounting-model.md` §2's `player_withdrawal_hold`
row already uses ("every held amount must equal exactly one non-terminal
withdrawal request").

Status: **RESOLVED — `NOT IMPLEMENTED`.**

### 7. Multi-asset — confirmed, no hardcoded currency

`ARCHITECTURAL DECISION`, stated because the directive requires it to be
confirmed explicitly rather than assumed.

Every posting in this ADR resolves its asset from the **wallet's own
`asset_code`** — the same 1:1 stake-currency-to-wallet resolution Flow 5/8
already use for casino/sportsbook, with no cross-asset conversion mid-bet.
`amount` columns are `NUMERIC(38,0)` minor units per the asset's registered
`decimal_exponent` (ADR 0001/0007/0021), identically to every other ledger
posting — no code path in this ADR assumes 2 decimal places, a fiat asset,
or any specific asset at all. A sportsbook stake, payout, partial-
settlement amount, or cashout amount in BTC or an 18-exponent asset posts
through the identical entries and idempotency mechanism as one in EUR;
only the `asset_code` and its registry-looked-up exponent differ.

> **Stage 4H-B0-R4 Wave-2 review addition (citation only).** ADR 0037
> (`docs/decisions/0037-asset-currency-registry-and-fx-conversion-
> architecture.md`), authored by `architect` in parallel this same stage,
> formalizes the Asset Registry and confirms the `asset_code`/
> `decimal_exponent` mechanism this section relies on is unchanged. No
> content in this section is altered by that citation — it names the
> now-canonical source for the registry this ADR already assumed.

**Rounding — reuses ADR 0021's resolved decision, in full, without
exception.** This ADR does not open a second rounding decision. The two
places a sportsbook-specific computation could need rounding are named
precisely, per the directive:

1. **A proportional cashout or partial-settlement amount computed by the
   platform** (§8/§9's generalized `R`/`P` formula, when `R` or `P` is
   derived as a percentage/proportion of the stake by the platform itself
   rather than stated outright by the provider) — **must** use ADR 0021's
   one shared rounding helper (round-half-up, applied once at the final
   monetary boundary, full `NUMERIC` precision until then; the applied
   rule/version denormalized onto the `LedgerTransaction` row exactly as
   ADR 0021 specifies for every other rounding site).
2. **An in-house bet-builder/combo payout** (§5's future in-house case) —
   same helper, same reasoning, no exception for "it's odds, not a bonus
   percentage." A rate multiplied by an integer amount is a rate multiplied
   by an integer amount regardless of which domain produced the rate.

**Where no platform-side rounding occurs at all**: the ordinary
provider-driven case for every flow in this ADR (§5/§8/§9), where the
provider states `S+W`, `R`, `P`, or a combo payout as an already-rounded
minor-unit integer in its callback. The ledger posts that integer verbatim
and performs no arithmetic on it beyond the balancing entries this ADR
already specifies. This is the common case today and is expected to remain
so for as long as the widget/iframe or feed/API provider shape is in use;
§5's in-house case is named for completeness, not because it is scheduled.

Status: **RESOLVED — `NOT IMPLEMENTED`.** No change to ADR 0021's resolved
rounding decision is required or proposed; this section only identifies
where sportsbook-specific computations bind to it.

### 8. Void, partial settlement, and cashout — three distinct events, one generalized posting shape

`ARCHITECTURAL DECISION`, per Flows 10/11 and doc 09's explicit "never
conflated" requirement. All three release some or all of a bet's
`player_locked` stake before or instead of an ordinary full settlement.
They are kept as **three distinct `transaction_type` values** — never one
"resolve bet" type — because they mean different things to a reader of the
ledger, to reporting, and to a support agent explaining a player's history,
even where (as below) their entries share one generalized formula.

#### 8.1 Void (Flow 10)

The full stake is returned as if the bet never happened — a market
cancellation, push, or data error, never a market outcome.

| Event | Entries |
|---|---|
| Void before settlement, stake `S` | Dr `player_locked` `S` · Cr `player_cash`/`player_bonus` `S` |
| Void after settlement, stake `S` (+ any payout already posted) | Full reversal chain: a new `LedgerTransaction` with `reverses_transaction_id` pointing at the original settlement transaction (and, separately, at the original bet transaction if the settlement legs were posted against `player_locked` directly rather than via a prior lock reversal — see Flow 9's two-pair shape), exact inverse entries, landing the stake back in `player_cash`/`player_bonus` |

`transaction_type = 'sportsbook_void'` for both timing variants — the
economic meaning ("this bet is nullified, full stake returned, never
became house revenue or a real win") is identical regardless of when the
provider's void callback arrives; only the entries needed to reach that
end state differ mechanically. This distinguishes void from:

- **Loss/win settlement** — a market outcome occurred; void asserts no
  outcome is being recognized at all.
- **Player-initiated bet withdrawal** (§8.4) — void is always
  provider/event-initiated (a market fact external to the player's
  wishes), whereas a withdrawal, if it existed, would originate from the
  player's own unilateral decision instead.

**Idempotency key**: the void's own provider reference, distinct from both
`provider_bet_reference` and any settlement reference already posted.

**Audit event**: `sportsbook_bet.voided`.

**Invariants engaged**: #1, #2 (post-settlement case), #3, #4, #5, #10
(post-settlement case), #12, #13 (post-settlement case), #14
(post-settlement case).

#### 8.2 Partial settlement (bet-builder / multi-leg)

Some legs of a multi-leg (bet-builder, same-game-multi) wager resolve
before others, or a portion of the wager's stake is otherwise determined
while the remainder stays open. Let `R` = the portion of the locked stake
released by this event (attributable to the leg(s) now resolved), and `P`
= the amount paid out for that released portion (0 if the resolved leg(s)
lost, a computed win amount if they won). The remainder of the stake
(`original_stake − R`) stays in `player_locked` against the still-open
legs.

| Case | Entries |
|---|---|
| Always | Dr `player_locked` `R` · Cr `player_cash`/`player_bonus` `P` |
| `R > P` (resolved leg(s) lost, house keeps the released margin) | + Cr `house_gaming` `R − P` |
| `P > R` (resolved leg(s) won enough that the payout for this portion exceeds the released stake) | + Dr `house_gaming` `P − R` |

Identical generalized shape to Flow 11's existing formula, deliberately
reused rather than re-derived, because the underlying arithmetic
constraint (`R`/`P` may differ in either direction; a zero-amount entry is
forbidden so the `house_gaming` leg is omitted entirely when `R = P`) is
the same regardless of *why* a portion of the stake is being released.
What is new here, per the directive, is that this is now a **named,
separately-typed** event distinct from full settlement and from cashout:
`transaction_type = 'sportsbook_partial_settlement'`.

**Idempotency key**: `(tenant_id, provider_id, provider_partial_settlement_reference)`
— **distinct per partial-settlement occurrence**, since a single bet may
have multiple legs settle at different times, each independently
idempotent. The reference must identify *which* leg-settlement event this
is, not merely the bet — a provider that reuses the bet's own reference
across successive partial settlements would make the second event collide
with (and be silently dropped as a duplicate of) the first, which is
exactly the "stuck lock"-shaped failure this ADR treats as unacceptable.

**Audit event**: `sportsbook_bet.partially_settled`.

**Bonus-funded portion**: blocked, same as §5, until the `player_locked`
origin split (§9) exists; once unblocked, the `promo_liability`/
`bonus_expense` mirror pair applies to whichever of `R`/`P`/margin legs
credit `player_bonus`, exactly as ADR 0032 §2 already specifies for
casino/sportsbook full settlement.

**Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #12, #13, B1 (once
unblocked).

#### 8.3 Cashout (early settlement offer accepted)

A player accepts a provider-offered early buyout of some or all of a still
-open bet's remaining exposure, before the underlying event concludes.
Mechanically this uses the **same generalized `R`/`P` formula** as §8.2 —
`R` = the portion of the remaining locked stake being closed out, `P` =
the provider's cashout payout for that portion — but it is kept as its
**own, fourth `transaction_type`, `sportsbook_cashout`**, never a variant
of `sportsbook_partial_settlement`, for a reason that is about meaning, not
arithmetic:

- A **partial settlement** is triggered by a **market fact becoming known**
  (a leg's outcome resolves) — the provider is reporting something that
  happened.
- A **cashout** is triggered by a **player's voluntary commercial
  decision** to accept a provider-priced buyout **before** any new market
  fact exists — the underlying event has not concluded and no leg has
  necessarily resolved. The price `P` reflects the provider's own current
  odds/margin view at the moment of acceptance, not a settled outcome.

Conflating the two would make a report unable to answer "how much of our
sportsbook revenue came from bets actually finishing vs. players buying
out early" — a materially different commercial question a partner will
ask, and exactly the kind of loss-of-distinction doc 09's "never
conflated" rule exists to prevent, even where (as here) the ledger-entry
shape happens to coincide.

| Case | Entries |
|---|---|
| Always | Dr `player_locked` `R` · Cr `player_cash`/`player_bonus` `P` |
| `R > P` | + Cr `house_gaming` `R − P` |
| `P > R` (cashing out a bet currently in a winning position pays more than the released stake) | + Dr `house_gaming` `P − R` |

A **full cashout** is the special case `R = original remaining locked
stake` (the bet closes entirely). A **partial cashout** releases only part
of it, leaving the rest in `player_locked` — the same "may cash out more
than once" property Flow 11 already notes, each occurrence independently
idempotent.

**Idempotency key**: `(tenant_id, provider_id, provider_cashout_reference)`
— the specific cashout-offer-acceptance event's own reference, distinct
per occurrence for the identical multiple-partial-cashout reason as §8.2.

**Audit event**: `sportsbook_bet.cashed_out`.

**Rounding**: per §7 — the provider-driven case posts `P` as given; an
in-house cashout-pricing engine, if ever built, would compute `P` through
the one shared rounding helper.

**Bonus-funded portion**: blocked, same as §5/§8.2, until §9 resolves —
**and, unlike §5/§8.2, blocked by a second, independent open question
even after §9 resolves.** Stage 4H-B0-R5 round 2: because this event is
the player's own voluntary commercial decision rather than a market fact
becoming known (the "meaning, not arithmetic" distinction above), it is
**not** settled whether cashout proceeds on a mixed- or bonus-funded bet
split proportionally cash:bonus (mirroring the lock), pay entirely to
`player_cash` regardless of origin, or whether bonus-funded bets are
simply not cashout-eligible at all. Both posting candidates are
double-entry-balanced and invariant-B1-safe, so the ledger's invariants
do **not** select between them; the choice is a bonus-policy/RG/product
question. Worked entry tables for both candidates, and the reasons this
is referred upward rather than decided, are in
`ledger-accounting-model.md` §6.3.3.2 case C-cashout. Cash-funded
cashout — the only shape this ADR actually specifies above — is
unaffected.

**Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #12, #13, B1 (once
unblocked).

#### 8.4 Player-initiated bet withdrawal (pre-settlement) — out of scope

> **Stage 4H-B0-R4 Wave-2 review correction.** This subsection was
> originally labeled "Cancellation." That collided with an unrelated,
> already-settled use of the same word in `docs/architecture/09-sportsbook-
> architecture.md` §1.1/§3.5, where "Cancellation" names an Event/Market-
> level Trading Operation that cascades into a Void settlement for every
> affected open bet — a different concept entirely from the player-level
> capability this subsection discusses. Renamed to "player-initiated bet
> withdrawal" so this ADR stops reusing doc 09's term for a different
> meaning. Doc 09 itself is untouched — its "Cancellation" terminology is
> `sportsbook`'s to own, not `ledger-finance`'s to change. Nothing about
> this subsection's scope decision (still out of scope, still not built)
> changes.

`ARCHITECTURAL DECISION` (scope), answering the directive's explicit
question directly rather than by omission.

**Player-initiated bet withdrawal — a player unilaterally un-doing an
already-accepted bet, distinct from both void (provider/event-initiated)
and cashout (provider-priced, market-referenced) — is explicitly out of
architectural scope for this ADR.** Reasoning:

- Neither the Blueprint nor doc 09 describes a player-initiated,
  no-market-referenced bet withdrawal capability.
- It is not a standard capability of the widget/iframe integration shape
  doc 09 recommends starting with (Altenar/BetBy/Digitain render the
  betting experience themselves; a bet, once accepted, is closed to
  unilateral player withdrawal in that shape — only cashout, which is
  priced, and provider-initiated void exist).
- Per CLAUDE.md's "no uncontrolled scope expansion": this is not required
  by the Blueprint, the current stage's path, the future B2B architecture,
  or security/compliance, and building a bespoke "instant free withdrawal
  window" would be inventing a product capability no integration partner
  offers, ahead of any stated requirement.

**If a future jurisdiction mandates a cooling-off/withdrawal window**
(some regulators require a short post-acceptance withdrawal right,
distinct from the provider's own cashout mechanic), that is a genuinely
new requirement, not an extension of this ADR's existing flows, and would
be recorded as its own decision when a jurisdiction actually requires it —
architecturally it would most likely reuse the §8.1 void-before-settlement
shape (full stake returned, player-initiated rather than provider-
initiated, distinguished by `reason_code`/`causation_id` rather than a new
`transaction_type`), but that is a recommendation for a future decision,
not something this ADR builds now.

Status: **RESOLVED (out of scope) — deferred future consideration, not a
gap.**

Status (§8 overall): **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 9. Bonus-funded sportsbook wagering — confirmed BLOCKED, not re-decided here

`OPEN DECISION`, inherited and confirmed, not reopened. `ledger-
accounting-model.md` §6.2 and ADR 0032 §10 already establish that
`player_locked` is a **single** account type, so Flow 8 loses whether a
locked stake came from `player_cash` or `player_bonus`. Two consequences,
restated because every flow in this ADR depends on them:

1. **Settlement cannot know which account to return a stake or payout to**
   once locked — every settlement/void/partial-settlement/cashout flow in
   §5/§8 that would credit `player_bonus` is mechanically unable to
   determine that it should, for a stake that was locked before this gap
   is closed.
2. **Invariant B1 breaks the moment a bonus-funded stake is locked** — the
   value has left `player_bonus` (so `promo_liability`'s mirror is now
   wrong by that amount) while the player may still get it back, and
   nothing in the current account model records that the locked amount's
   *origin* was bonus rather than cash.

**This ADR does not resolve the gap.** Doing so requires splitting
`player_locked` into `player_locked_cash`/`player_locked_bonus` (or an
equally binding, indexable origin dimension) and extending invariant B1's
covered-account set — a change to a Blueprint-listed account type that ADR
0032 §10 already correctly scopes as needing `architect` + `sportsbook` +
`ledger-finance` joint sign-off, not a unilateral `ledger-finance` decision
inside a single domain's accounting ADR. **Every bonus-funded posting
sketched in §3/§5/§8 above is therefore BLOCKED, not merely deferred,
until that joint decision lands** — cash-funded sportsbook wagering is not
blocked by it and every flow in this ADR is fully specified for the
cash-funded case.

**What this ADR commits to now, so the eventual unblock is mechanical
rather than a redesign**: once the origin split exists, every bonus-funded
leg in §3/§5/§8 carries the identical ADR 0032 §2 `promo_liability`/
`bonus_expense` mirror pair already established for casino, generated by
the same shared ledger posting layer (`internal/ledger`), never hand-
assembled by a sportsbook package — the same "split instruction, ledger
posts it" boundary `10-bonus-engine-architecture.md` §6 already establishes
for casino's bonus-funded stakes. The sportsbook engine's own
responsibility, mirroring that same document's two-part boundary, is
exactly: (1) compute the cash/bonus split of a stake at placement
(from the Grant's current balance mix and the sportsbook-equivalent of a
wagering-contribution rule, if one applies) as a **split instruction**, and
(2) emit the settlement/void/partial/cashout lifecycle facts with enough
context (bet id, amounts, reason) for `internal/ledger` to post the
correct legs including the mirror pair — the sportsbook engine never
specifies account types or posting order itself, identically to bonus's
own boundary. No new mechanism is invented for this; it is the existing
one, extended to a second stake-locking product once the origin gap is
closed.

Status: **`OPEN DECISION` — not resolved here, deliberately, raised to the
Master Orchestrator as already flagged by ADR 0032 §10.** Referred-upward
item, not restated as new.

### 10. Reversal/correction — market corrected after initial settlement

`ARCHITECTURAL DECISION`, per doc 09's explicit requirement and CLAUDE.md's
standing compensating-entry rule.

A market correction after settlement (a scoring error, a data-feed
mistake, a result later overturned by the sport's own governing body) is
handled as a **two-transaction compensating sequence**, never a mutation
of the original settlement:

1. **Rollback of the original settlement.** A new `LedgerTransaction`,
   `transaction_type = 'sportsbook_rollback'`, `reverses_transaction_id`
   pointing at the original settlement/partial-settlement/cashout
   transaction being corrected, posting the **exact inverse** of whichever
   of §5/§8.2/§8.3 actually posted. Named `sportsbook_rollback` rather than
   reusing `sportsbook_void`, deliberately: a void asserts "this bet never
   happened"; a rollback here asserts "this bet's *settlement* was
   computed wrong and is being corrected" — the bet itself remains a real,
   resolved wagering fact, which is a different statement a reader of the
   ledger needs to be able to tell apart (and mirrors `financial-
   transaction-flows.md` Flow 7's own `casino_rollback` naming for the
   identical "reverse a specific posted settlement" role).
2. **Re-settlement.** If the corrected market produces a new determinate
   outcome, a **fresh** transaction of whichever type actually applies
   (`sportsbook_settlement`, `sportsbook_partial_settlement`, or
   `sportsbook_cashout` — cashout is unaffected by a market correction
   arriving later and would not normally need re-settlement, but a
   partial-settlement leg can) is posted, reflecting the corrected result,
   with its **own new** idempotency key (the correction event's own
   provider reference — never the original settlement's reference, which
   is already consumed and would collide). This is a **new economic fact**,
   posted forward, never an edit of history — identical in spirit to ADR
   0032 §5's "forfeiture is a new economic fact, not a reversal of the
   grant" distinction.

**Never-seen-original tombstone case**: if a correction/rollback event
arrives referencing a settlement `provider_tx_id` the ledger never posted
(lost callback, out-of-order delivery), the rollback handler writes a
tombstone occupying that reference's idempotency slot
(`(tenant_id, provider_id, provider_settlement_reference)`), per CLAUDE.md's
"a rollback for a transaction never seen writes a tombstone" — identical
mechanism to Flow 2/7's existing tombstone handling, no sportsbook-specific
variant.

**Double-reversal protection**: the original settlement transaction is
selected `FOR UPDATE` and a rollback is rejected if a rollback already
exists for it — the same row-lock pattern `internal/casino`'s
`postRollback` already uses, reused unchanged.

**Distinguishing this from void**: a void (§8.1) asserts the bet never
became a market fact at all; a rollback+re-settlement asserts the bet did
resolve, but the *recorded outcome* of that resolution was wrong and is
being corrected to a different (possibly still nonzero) outcome. A market
that is corrected to "no result stands" (rather than to a different
result) resolves as rollback-then-void, not rollback-then-nothing — the
`player_locked` stake must land somewhere, and "nowhere" is not a valid
end state.

**Bonus-funded portion**: the rollback's inverse entries automatically
include whatever mirror legs the original settlement posted (once §9
unblocks bonus-funded settlement) — no special-case rollback code, the
identical "falls out of reversing the same transaction" property ADR 0032
§7 already relies on for casino.

**Idempotency key**: rollback keyed by the correction event's own
provider reference (distinct from the settlement it reverses); re-
settlement keyed by its own new provider reference (§5/§8.2/§8.3, as
applicable).

**Audit event**: `sportsbook_bet.rolled_back` / (re-settlement's own event
name per whichever type it reuses, e.g. `sportsbook_bet.settled` again,
distinguishable from the first by `causation_id` pointing at the
correction event rather than the original outcome callback).

**Invariants engaged**: #1, #2, #3, #4, #5, #10, #13, #14, B1 (once
unblocked).

Status: **RESOLVED — `NOT IMPLEMENTED`.**

### 11. Idempotency — the precise per-lifecycle-event key strategy

`ARCHITECTURAL DECISION`, answering the directive's explicit question:
how a single Bet's multiple distinct financial events avoid colliding
under one idempotency mechanism, unlike a casino round's single bet/win
pair.

**Nothing bespoke is invented — ADR 0020's mechanism applies verbatim, per
`ledger-accounting-model.md` §3.** What sportsbook adds is a precise
statement of *which* value plays which role, because a bet's events are
more numerous than any other product's:

- **`(tenant_id, provider_id, provider_tx_id)` uniqueness is never
  collision-prone across a bet's lifecycle, because `provider_tx_id` is
  always the specific event's own reference, never the bet-slip/bet
  reference reused across events.** Each of placement, rejection-release,
  settlement, void, partial settlement (per occurrence), cashout (per
  occurrence), and rollback/re-settlement carries a **distinct** provider-
  issued (or, for an in-house engine, internally-minted in the identical
  role) reference. A provider adapter that reuses the bet-slip reference
  as `provider_tx_id` for more than one of these events is a defect — it
  would make the second event's insert collide with the first's unique
  constraint and be silently treated as an idempotent retry of the wrong
  fact. This is the single most likely way this design fails in practice,
  the identical class of failure ADR 0032 §8 already calls out for a
  Reward Orchestrator minting a fresh key per delivery attempt (there:
  too many keys for one fact; here: too few keys for many facts) — named
  explicitly for the same reason.

  > **Stage 4H-B0-R4 `qa` review finding (P1-3), resolved Stage 4H-B0-R5,
  > see §14.** The paragraph above states this as a **contract** the
  > provider adapter must honor. `qa` correctly flagged that a contract
  > alone has a residual gap: if a non-conformant adapter reuses a
  > reference across two genuinely distinct occurrences of the *same*
  > `transaction_type` whose payloads also happen to coincide (e.g. two
  > bet-builder legs that both settle for the same amount), a
  > payload-comparison idempotency check has nothing left to tell them
  > apart by, and the second, real occurrence is silently absorbed as a
  > duplicate of the first with no error raised. §14 closes this with a
  > required, adapter-supplied **occurrence discriminator**, not a
  > restatement of the contract. Nothing below in this section is
  > superseded by §14; §14 only adds the missing mechanism underneath it.
- **`correlation_id` is the internal bet id, stable across every
  transaction the bet ever produces** (§3) — this is the mechanism for
  "show me this bet's full financial history," and it is **not** part of
  the uniqueness constraint. Two transactions may share a `correlation_id`
  freely; they may never share a `(tenant_id, provider_id, provider_tx_id)`
  pair.
- **`transaction_type` is a second, independent discriminator**, not a
  substitute for a distinct `provider_tx_id` — even though `sportsbook_bet`
  and `sportsbook_settlement` could never collide on `provider_tx_id` alone
  if the provider follows its own protocol correctly, the type still
  matters for every other invariant/reconciliation query in this ADR that
  filters by it (§6, §8).
- **Per-occurrence events (partial settlement, cashout) need a reference
  per occurrence, not per bet** (§8.2/§8.3) — a provider or in-house engine
  that assigns one reference per bet-builder and reuses it for every leg's
  partial settlement would collide the second leg against the first
  exactly as above; this is called out a second time here because it is
  the specific case most likely to be overlooked when a bet-builder ships.
  **§14 makes this a required mechanism (`occurrence_ordinal`), not only a
  named risk** — see §14 for exactly how the discriminator is derived and
  supplied.
- **Internally-originated events with no natural provider reference** use
  `(tenant_id, idempotency_key)` — a stable, reproducible reference
  generated once at the originating decision and reused verbatim on
  retry — identical to Flow 16's `manual_adjustment` pattern. **Amended,
  Stage 4H-B0-R5 follow-up correction (§14.6):** this path is **not**
  rare for sportsbook overall — it is the **standard path for every
  in-house-sportsbook-engine-mode posting** (bet, settlement, void,
  partial settlement, cashout, rollback alike, whenever `provider_id` is
  `NULL` per ADR 0033 §2), and remains the **exception path for
  external-provider-mode postings**, where it continues to cover only the
  genuinely rare staff-initiated manual void distinct from a market
  correction. The original text above ("expected to be rare for
  sportsbook, since the widget/iframe/feed/API shapes make the provider
  the origin of nearly every event") is corrected by this amendment: it
  was accurate only for the external-provider-mode shapes it was written
  against, and did not yet account for in-house-engine mode. See §14.6 for
  the full routing rule, the exact field/constraint used in each mode, and
  a worked idempotency example.

**Exact retry / concurrent duplicate / same-key-different-payload
semantics**: unchanged, inherited from ADR 0020 (`SAVEPOINT`-based exact
retry, `ErrIdempotencyKeyReused` on a same-key-different-payload attempt,
database-constraint arbitration of concurrent duplicates — never
check-then-insert). Nothing in this section modifies that mechanism.

**Authorization**: ADR 0019's actor matrix already needs a row for each
`sportsbook_*` transaction type (a provider callback scoped to the tenant
resolved from the credential that verified the callback's signature, never
from a tenant/player identifier in the payload — the same rule Flow 8's
preamble already states platform-wide) — adding those rows is an additive
edit to that matrix (§12), not a new authorization mechanism.

Status: **RESOLVED — `NOT IMPLEMENTED`.**

### 12. Reconciliation — recomputing the posted amount from stored inputs

`ARCHITECTURAL DECISION`.

- **Stake amount (§3)**: the posted amount **is** the input — the accepted
  stake at placement, with no computation and therefore no rounding
  question. Reconciliation is a straight match against the provider's own
  bet-acceptance record.
- **Settlement/partial-settlement/cashout amounts, provider-driven case
  (the near-term default, §5/§7/§8)**: the ledger does not recompute these
  from odds — it has none to recompute from in this shape. Reconciliation
  is against the **provider's own settlement/cashout statement**,
  matched by `(provider_id, provider_tx_id)`, the same posture Flow 17
  already uses for provider-fee settlement and Flow 11's existing `OPEN
  DECISION` already states for cash-out margin specifically.
- **Settlement/partial-settlement/cashout amounts, in-house-computed case
  (future, not built this stage)**: exactly recomputable from stored
  inputs — stake, stored odds (`NUMERIC`, never `FLOAT`), the stored
  rounding-rule identifier denormalized onto the `LedgerTransaction`
  (ADR 0021's existing storage design, reused verbatim) — as `payout =
  round_half_up(stake × combined_odds, asset_exponent)`. This case does
  not exist yet; it is specified so that if/when it is built, the
  recomputation contract is already fixed rather than re-derived per
  implementer.
- **Open-bet liability (§6)**: reconciled as a derived read against the
  sportsbook domain's own open-bet-stakes view, per §6's own reconciliation
  paragraph — not a recomputation of a single amount but a cross-check
  between two independently derived totals.

Status: **RESOLVED — `NOT IMPLEMENTED`.**

### 13. Risk & Limits integration — reusing the established pattern, not a new mechanism

`ARCHITECTURAL DECISION`, per ADR 0031 §13–§18's already-established
"every domain declares its own Risk integration, in the same transaction
as its own posting, before commit" pattern.

**Current repository state, verified, not assumed**: `sportsbook_bet` is
**already** one of the six `Operation` values migration 0041's CHECK
constraint accepts and `internal/risk/types.go` already declares as a Go
constant (ADR 0031 §13's own table: "`Sportsbook` | `sportsbook_bet` |
`NOT IMPLEMENTED` — does not exist as a package yet"). **No new `Operation`
value, migration, HTTP-allowlist entry, or OpenAPI enum entry is required
for bet placement to be Risk-gated** — the extension point already exists
and is simply unwired, because `internal/sportsbook` does not exist yet.

**What this ADR specifies for `ledger-finance`'s side of that future
wiring**, exactly as §16 step 5 of ADR 0031 states the ledger-transaction-
type mapping is "`ledger-finance`'s to specify, never Risk's to invent":

- **`min_amount`/`max_amount` with `TimeWindow: transaction`** apply to
  `sportsbook_bet` unchanged the moment the call site exists — `Rule.
  breach()`'s comparison is already generic over `Operation` and needs no
  sportsbook-specific change.
- **`cumulative_amount`** ("no more than X in sportsbook stakes per rolling
  day") requires `operationLedgerTransactionTypes["sportsbook_bet"] =
  "sportsbook_bet"`, and its netting counterpart requires
  `operationLedgerRollbackTypes["sportsbook_bet"]` to net **only**
  `sportsbook_void` against it — a voided stake is treated as if the bet
  never happened at all (full stake returned, §8.1), so it must not
  permanently consume a player's cumulative stake capacity, the identical
  correctness property the existing `casino_bet`/`casino_rollback` netting
  already establishes. This is a one-operation-to-one-ledger-type mapping,
  identical in shape to the existing `casino_bet` → `casino_rollback`
  entry — no widening of `operationLedgerRollbackTypes`'s value type is
  needed for `sportsbook_bet`.

  > **Stage 4H-B0-R4 Wave-2 review correction.** An earlier version of this
  > bullet also netted `sportsbook_rollback` (in addition to
  > `sportsbook_void`) against `sportsbook_bet`'s cumulative stake usage,
  > and justified it by reasoning that a rolled-back stake "was never
  > actually put at risk (or the wager it represented was undone)." That
  > directly contradicted this ADR's own §10, which is explicit that a
  > rollback does **not** mean the wager was undone: "the bet itself
  > remains a real, resolved wagering fact" — a rollback corrects a wrongly
  > -recorded *settlement outcome*, it does not nullify the underlying
  > staked bet the way a void does. `sportsbook_rollback` is therefore
  > **never** netted against cumulative stake usage. Tracing it through:
  > player stakes `S`, the bet is wrongly settled as a win, the market is
  > corrected (a `sportsbook_rollback` reversing that settlement), then
  > re-settled as a loss — the player genuinely staked `S` throughout, and
  > netting the rollback would incorrectly erase that `S` from their daily
  > cumulative tally. Worse, in §10's own composite "no result stands"
  > case (rollback-then-void), netting *both* legs would double-subtract
  > the stake. The scenario the original text was evidently trying to
  > cover — that composite case — is already handled correctly without
  > netting rollback at all: rollback-then-void nets exactly once, via the
  > trailing `sportsbook_void` transaction alone (the rule stated above),
  > which is the only leg of that sequence that actually asserts "this
  > stake was never at risk." No new mechanism is needed to preserve that
  > property; removing `sportsbook_rollback` from the netting set is a
  > pure correction, not a gap.

  `sportsbook_settlement`/`sportsbook_partial_settlement`/
  `sportsbook_cashout` are **deliberately not netted** — the stake was
  genuinely wagered and resolved (whether won or lost), which is not
  "unconsuming" cumulative capacity, the same reasoning a casino win does
  not net against a casino bet's cumulative consumption either.

  > **Non-blocking mapping gap, recorded for later (Stage 4H-B0-R4 Wave-2
  > review addition).** `cumulative_amount` is not wired for sportsbook
  > settlement *payouts* today, and this ADR does not propose wiring it
  > this stage. If a future rule ever needs a cumulative check over
  > sportsbook settlement payouts specifically,
  > `operationLedgerTransactionTypes["sportsbook_settlement"]` will need to
  > widen from its current single value to a list that also includes
  > `sportsbook_partial_settlement` — the transaction-types-side mirror of
  > the one-to-many widening this section's earlier (now-corrected) text
  > mistakenly proposed on the rollback-types side. Recorded here only so
  > it is not rediscovered as a surprise when that wiring is eventually
  > attempted; it changes nothing about this ADR's current,
  > `sportsbook_bet`-only `cumulative_amount` specification.
- **Enforcement position**: bet placement calls `risk.Evaluate` in the
  same database transaction as the `sportsbook_bet` `LedgerTransaction`,
  before either commits — the identical positional contract `internal/
  casino`'s `postBet` already implements, with RG evaluated first
  (`rg.EvaluateEligibility`, short-circuiting on denial) and Risk second,
  never the reverse (ADR 0031 §1/§14). A non-nil error or a `DENY`/`REVIEW`
  outcome aborts the whole transaction before any `player_locked` entry
  exists — no ledger effect has happened yet at that point, mirroring
  casino's own fail-closed contract exactly.
- **Settlement, void, partial settlement, cashout, and rollback are
  provider-driven facts already accepted or already decided elsewhere**
  (the market resolved, the provider voided it, the player already
  accepted a cashout price) — they are not new exposure-creating decisions
  the platform is choosing to allow, so **none of them are additional Risk
  checkpoints**. This mirrors casino's own scope: Risk gates `casino_bet`
  (a new exposure-creating decision) but not `casino_win`/`casino_rollback`
  (facts about an exposure already accepted). The only sportsbook-specific
  decision point analogous to a *new* exposure being created after
  placement is a **cashout offer being generated and shown to the player**
  in the first place — that is a provider/trading decision outside the
  ledger's or Risk's boundary entirely (the platform does not price
  cashout offers in the provider-driven shape), so it requires no Risk
  integration point of its own.

**Cross-domain aggregate exposure** (a rule spanning bonus + casino +
sportsbook) remains the confirmed, unsolved `OPEN DECISION` ADR 0031 §17
already records — this ADR does not attempt to close it, and sportsbook's
own open-bet exposure (§2's "potential payout exposure," specifically) is
additional evidence for why that future `exposure` `LimitKind` would need
its own definition of what "exposure" means across three structurally
different things (a settled casino stake, an unsettled sportsbook
position, and an outstanding bonus liability) — not a reason to
approximate it here.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.** No `internal/
risk` code, migration, or schema change is proposed by this ADR; §13's
mapping specification is ready for whichever stage wires
`internal/sportsbook`'s enforcement call site.

### 14. Canonical idempotency contract — resolving the Stage 4H-B0-R4 deferred item (P1-3)

`ARCHITECTURAL DECISION`, Stage 4H-B0-R5. Resolves the gap `qa`'s Stage
4H-B0-R4 review named and §11 flagged forward: §11's `(tenant_id,
provider_id, provider_tx_id)` uniqueness is only collision-safe if every
distinct lifecycle event genuinely carries a distinct `provider_tx_id` —
that was stated as a **contract** the provider adapter must honor, and a
contract alone has a residual failure mode. If a non-conformant adapter
reuses a reference across two genuinely distinct occurrences of the
**same** `transaction_type`, and their payloads also happen to coincide
(two bet-builder legs that both settle for the same amount is the
canonical example, but the same shape recurs for two cashout occurrences
of the same size, or two market corrections on the same bet), a
payload-comparison-based idempotency check (ADR 0020's
same-key-different-payload rejection) sees an identical key and an
identical payload and correctly — by its own rules — treats the second
event as an exact retry of the first. **The second, real, distinct event
is silently absorbed, no error is raised, and that leg's settlement is
permanently lost.** This section closes that gap with a required
mechanism, not a restatement of the contract.

Nothing in §3–§13 is changed by this section. No new `LedgerTransaction`
or `LedgerEntry` column is introduced; no migration beyond what §3–§13
already require is needed. This section binds the **adapter's**
obligation more precisely and, where noted, extends what the ledger
posting layer validates.

#### 14.1 Three canonical identifiers

1. **Platform operation ID — `LedgerTransaction.id`.** The platform's own
   identity for one specific, already-posted financial operation. It does
   not exist before the row is inserted, so it cannot itself be an input
   to the idempotency decision for that same insert — its role is
   downstream: it is what `reverses_transaction_id` (§10) points at, what
   ties a correction/reversal chain together, and what audit/reconciliation
   join on once a transaction exists. No pre-generated-before-the-fact
   operation id pattern exists elsewhere in this codebase to reuse —
   `internal/casino`'s `RoundID` plays the role of §3's `correlation_id`
   (grouping a bet and its later win under one business operation), not a
   distinct pre-minted per-event operation id — so this ADR does not
   invent one for sportsbook either. `correlation_id` (§3, the internal bet
   id) remains the "find this bet's whole history" key; `LedgerTransaction.id`
   remains the "this one specific posted fact" key. They are not
   interchangeable and neither is part of the idempotency uniqueness
   constraint.
2. **Provider operation ID — `provider_tx_id`, unchanged from §3–§13.**
   The provider's (or in-house engine's) own reference for one specific
   lifecycle event, opaque, never a provider type or enum
   (`ledger-accounting-model.md` §1.2). This is the value the `UNIQUE
   (tenant_id, provider_id, provider_tx_id)` constraint keys on today and
   continues to key on after this section — no schema change.
3. **Provider occurrence/event ID — `occurrence_ordinal` — new, closes the
   gap.** A strictly increasing integer scoped to `(tenant_id,
   correlation_id, transaction_type)`, **required, not optional**, for
   every `transaction_type` where §8/§10 establish that more than one
   occurrence may legitimately exist against the same bet:
   `sportsbook_partial_settlement`, `sportsbook_cashout`, and
   `sportsbook_rollback`/its paired re-settlement (a market can, in
   principle, be corrected more than once). It is `NULL`/not applicable
   for the single-occurrence types (`sportsbook_bet`, `sportsbook_void`'s
   ordinary case), which need no discriminator because at most one such
   event is ever architecturally valid per bet.

   **How it is derived — adapter responsibility, never inferred from
   ledger state.** Per the directive's explicit constraint, the canonical
   layer never needs to understand a specific provider's own ID scheme,
   but the adapter is responsible for mapping whatever the provider gives
   it into this ordinal before the ledger ever sees the event:
   - Where the provider's own protocol already carries a per-occurrence
     signal — a leg index, a settlement sequence number, a cashout attempt
     counter — the adapter uses it directly. This is not a new concept:
     ADR 0033 §2's canonical `sportsbook_settlement` event already carries
     a nullable `leg_reference` for exactly this case; `occurrence_ordinal`
     is the ledger-layer's binding, required counterpart to that same
     signal, promoted from "nullable, illustrative" to "required, for the
     transaction types named above."
   - Where the provider's protocol gives no such signal at all (the
     non-conformant case this section exists for), the adapter derives it
     from its **own** inbound-delivery deduplication record — assigned
     once, the first time a specific raw delivery is processed, and reused
     verbatim if that exact delivery is retried. This is always available
     to the adapter regardless of how poor the provider's own reference
     scheme is, because it depends only on the adapter's own receipt of a
     distinct message, never on the provider's business-level payload.
   - **It is never computed as "count existing rows for this
     `(correlation_id, transaction_type)` and add one."** That
     reintroduces exactly the check-then-insert race CLAUDE.md forbids,
     and — more specifically — it is not reproducible by a legitimate
     retry: an attempt that crashed before its insert committed would, on
     retry, recompute a *different* "next" value than a fresh occurrence
     would, if anything else had posted in between. Deriving the ordinal
     from something intrinsic to the specific occurrence (the provider's
     signal, or the adapter's own delivery-dedup record) is what makes it
     reproducible identically on retry and different for a genuinely new
     occurrence.

   **How it is carried — no schema change.** The adapter composes the
   value it submits as `provider_tx_id` to already include this ordinal
   (e.g. `{provider's settlement reference}#{occurrence_ordinal}`).
   `provider_tx_id` remains the single opaque `TEXT` the existing `UNIQUE
   (tenant_id, provider_id, provider_tx_id)` constraint keys on — this is
   the same "opaque string, never a provider type or enum"
   (`ledger-accounting-model.md` §1.2) latitude already given to that
   column, extended to an adapter-composed value rather than only a
   provider-supplied one. No new column on `LedgerTransaction` is
   introduced, consistent with this ADR's Consequences section.

   **Secondary, independent check — `causation_id`.** §3 already sets
   `causation_id` to the specific provider callback's own delivery id,
   distinct from `provider_tx_id`. This section makes that field do double
   duty as a cross-check: two `LedgerTransaction` rows that end up with the
   *same* `provider_tx_id` (an adapter defect, since the ordinal
   composition above should prevent it) but genuinely different
   `causation_id` values is the exact signature of the failure mode this
   section closes, and is treated as an **integrity alert**, not a
   possible outcome — because with the ordinal composed into
   `provider_tx_id` as specified, it should be structurally impossible.

#### 14.2 The six cases, precisely

| # | Case | What distinguishes it | Resolution |
|---|---|---|---|
| 1 | Retry of the same occurrence | Identical `(tenant_id, provider_id, provider_tx_id)` **and** identical payload **and** identical `causation_id` | ADR 0020 `SAVEPOINT` exact-retry path — returns the original result, no new write |
| 2 | Duplicate delivery (concurrent or delayed) | Same as #1, arriving concurrently or after a delay | Same as #1 — the database unique constraint, not application locking, arbitrates the race |
| 3 | Legitimate new occurrence | Different `(tenant_id, provider_id, provider_tx_id)`, because the adapter composed a different `occurrence_ordinal` (or the event is a genuinely different bet/lifecycle-event type) | Always succeeds as a new, independently posted `LedgerTransaction` |
| 4 | Correction (a new occurrence that revises a previous one) | Its own new provider reference (§10) **and** `reverses_transaction_id` = the platform operation id of the transaction being corrected | New transaction, traceable via `reverses_transaction_id`, never an edit of the original |
| 5 | Reversal (undoing a specific prior transaction) | Same as #4 | Same as #4 — `reverses_transaction_id` is the one and only correction/reversal traceability mechanism (§10, `ledger-accounting-model.md` §1.2), generalized, not reinvented, for every case in this row and the one above |
| 6 | Settlement update (partial settlement, cashout — legitimately multiple events on one bet, same `transaction_type`) | Distinct `occurrence_ordinal` per event, composed into a distinct `provider_tx_id`, even when the rest of the payload coincides | Each occurrence posts independently; never collides with a sibling occurrence regardless of payload similarity |

#### 14.3 Deterministic outcomes, per the directive's explicit list

- **Repeated bet (placement retry).** Same `provider_bet_reference` and
  payload → exact retry (#1/#2), no new lock, original result returned.
- **Repeated win (settlement retry).** Same composed settlement
  `provider_tx_id` (including its `occurrence_ordinal`, `NULL`/single-slot
  for an ordinary full settlement) and payload → exact retry, no new
  payout posted.
- **Repeated rollback.** Same correction-event reference → exact retry,
  returns the original rollback's result; a **second, different**
  correction event against an original that already has a rollback is
  rejected by §10's `FOR UPDATE` double-reversal lock, never silently
  posted as a second rollback.
- **Repeated settlement (two genuinely distinct partial-settlement or
  cashout occurrences).** Distinguished by `occurrence_ordinal` per case 6
  above — both post, neither is absorbed into the other, regardless of
  payload similarity. This is the exact scenario §11/P1-3 named as
  unresolved; it is now mechanically prevented rather than merely
  discouraged by contract.
- **Correction.** New provider reference, `reverses_transaction_id` set to
  the platform operation id of the transaction being corrected (§10);
  same-key retries of the correction itself resolve as case #1.
- **Provider callback redelivery (general case).** Resolves via the
  `SAVEPOINT`-based idempotent-retry path keyed on the (now
  ordinal-inclusive, where applicable) `provider_tx_id`; a same-key,
  different-`causation_id` collision is impossible by construction and, if
  ever observed, is an integrity alert per §14.1, not a silently accepted
  duplicate.

#### 14.4 The core invariant

**Financially equivalent retries must be idempotent — never double-posted.
Genuinely different legitimate occurrences must never collapse into the
same idempotency key, even when their payloads happen to be identical.
Uniqueness is enforced by an identifier the platform controls or requires
the adapter to supply — `occurrence_ordinal`, composed into the DB-enforced
`provider_tx_id` — never inferred from payload comparison alone.** A
payload-comparison check remains necessary (it is what ADR 0020 uses to
reject a genuine same-key-different-payload defect) but is no longer
sufficient on its own to distinguish two same-payload occurrences — that
is exactly the job the occurrence discriminator does instead.

#### 14.5 Scope of this section

This is a tightening of §11's existing mechanism, not a new one. ADR
0020's exact-retry/concurrent-duplicate/same-key-different-payload
semantics are unchanged; `internal/ledger` remains the sole writer; no
account type, no new `LedgerTransaction`/`LedgerEntry` column, and no
weakening of any invariant #1–#15/B1 is introduced. The one binding change
is on the **adapter**: for `sportsbook_partial_settlement`,
`sportsbook_cashout`, and `sportsbook_rollback`/re-settlement, composing an
`occurrence_ordinal` into the submitted `provider_tx_id` is now
**required**, not merely good practice — a conformance-suite check for
whichever adapter is built first (mirroring §2.4's conformance-suite
citation in doc 09) must verify it before that adapter is marked complete.

#### 14.6 Stage 4H-B0-R5 follow-up correction — in-house-mode `provider_id`/idempotency routing (closes the `bonus-engine`-identified partial-index gap)

`ARCHITECTURAL DECISION`, `ledger-finance`'s to make (a mechanism-level
tightening of this ADR's own idempotency design, the same authority basis
as §14 itself — not a change to the human-approved account-type schema).
Triggered by an independent `bonus-engine` review of this ADR's
idempotency design, conducted this same stage.

**The gap.** `ledger-accounting-model.md` §1.2/§3 defines `UNIQUE
(tenant_id, provider_id, provider_tx_id) WHERE provider_id IS NOT NULL` —
a **partial** index. §11 (as originally written) said internally-
originated postings with "no natural provider reference" use `(tenant_id,
idempotency_key)` instead, but called that path "rare for sportsbook,"
which reads as: *ordinary* in-house-engine bet/settlement postings were
expected to go through the `(tenant_id, provider_id, provider_tx_id)`
path, "using [the in-house engine's] own internally-generated reference in
the identical role" as an external provider's. Neither this ADR nor doc 09
ever stated what `provider_id` itself equals on an in-house-originated
`ledger_transactions` row. If an implementer leaves it `NULL` — a
plausible default, since there is no external "provider" in in-house mode
— the partial index above never evaluates for that row at all, and a
duplicate in-house posting (e.g. a retried bet-placement request) would
insert twice with **no database-level rejection whatsoever**. That is a
live violation of CLAUDE.md's "every financial write is idempotent via a
unique constraint... enforced by the database" rule, for the entirety of
in-house sportsbook mode, and this section closes it.

**Decision: Option 2 — in-house-mode postings use `(tenant_id,
idempotency_key)` as their *standard* path, not the `(tenant_id,
provider_id, provider_tx_id)` path at all.** `provider_id` stays `NULL`
for every in-house-mode `LedgerTransaction`, exactly as it already is on
the canonical event (below); it is never populated with a reserved
pseudo-value. `provider_tx_id` is correspondingly left `NULL` too (never
half-populated while `provider_id` stays `NULL` — a row with `provider_id
IS NULL` and `provider_tx_id` populated would be meaningless, since the
partial index that column exists for never even looks at it in that
state). §11's bullet on "internally-originated events with no natural
provider reference" is amended in place (this stage) to say so.

**Why Option 2, not Option 1 (a reserved pseudo-`provider_id`) — two
independent pieces of existing platform precedent, not just a stylistic
preference:**

1. **This exact question was already decided, at the architecture layer,
   one section away.** `docs/decisions/0033-provider-interoperability-
   and-external-bonus-engines.md` §2 (Stage 4H-B0-R4 Wave-2, `architect`,
   independent review) states, for the identical in-house-vs-external
   sportsbook distinction at the canonical-event layer: *"`provider_id`
   ... is nullable. `NULL` means the event originated from the platform's
   own in-house sportsbook engine ... **never a reserved sentinel
   string**."* The canonical `sportsbook_bet`/`sportsbook_settlement`/
   `sportsbook_void_cancel` events this ADR's postings are written against
   (Context, above) are the same events ADR 0033 §2 describes. Populating
   `LedgerTransaction.provider_id` with a reserved pseudo-value for the
   identical in-house case would make the ledger posting layer disagree
   with the canonical event it was posted from about what `provider_id`
   means — the adapter would have to translate a `NULL` it received into a
   non-`NULL` sentinel it invents, a translation this ADR has no
   authority to require unilaterally (CLAUDE.md: "no specialist redesigns
   shared architecture unilaterally"; this would be exactly that, against
   an `architect`-authored decision, from a different specialist's ADR).
   Option 2 requires no such translation: `NULL` in, `NULL` stored,
   consistently.
2. **The codebase already has a working, precedented pattern for "no
   external identity" that is a discriminator plus an empty identifying
   field, never a sentinel stuffed into the identifying field.**
   `internal/audit/audit.go`'s `Entry`/`Record` (checked for this review,
   per the directive's own instruction to look for precedent):
   `ActorType` is the discriminator (`ActorPlayer` / `ActorStaff` /
   `ActorService` / `ActorSystem`), and `Record` **enforces**, in code,
   that `ActorID` is empty exactly when `ActorType == ActorSystem` and
   non-empty otherwise (`internal/audit/audit.go`, `Record`: `"audit:
   actor_id must be empty for actor_type 'system'"` /
   `"audit: actor_id is required for actor_type %q"`). No
   reserved-sentinel `ActorID` value for a system actor exists anywhere in
   that package. This ADR's own shape is the exact structural analogue —
   `provider_id` populated or `NULL` is the discriminator (mirroring
   `ActorType`), and the uniqueness mechanism keyed on the populated field
   (mirroring `ActorID`'s required-when-non-system rule) is exactly what
   §11/this section route by. Introducing a sentinel `provider_id` here
   would be the one inconsistent case in the codebase where "no external
   counterpart" is represented by a stuffed identifier rather than by
   `NULL` plus a discriminator — Option 2 keeps sportsbook consistent with
   both precedents instead of adding a second pattern for the same
   concept.

   A reserved-sentinel approach (Option 1) would also carry an ongoing
   registry burden this ADR has no mechanism to own (the sentinel must
   never collide with any current or future real provider's `provider_id`,
   forever, across every tenant and every future sportsbook vendor) and
   would corrupt §12's reconciliation-against-the-provider's-own-
   settlement-statement logic, which is specifically keyed on a real
   external provider's statement existing to reconcile against — a
   sentinel `provider_id` has no such statement, and a reconciliation job
   that doesn't know to special-case the sentinel would silently treat
   in-house postings as an unmatched/unreconciled external-provider
   stream. `NULL` avoids both problems: it is already the platform's
   existing "no external counterpart" value everywhere else, and §12's own
   logic already has to branch on provider-driven vs. in-house-driven
   settlement (§5) for the rounding/reconciliation-source distinction, so
   branching on `provider_id IS NULL` there costs nothing new.

**The routing rule, precisely, for every `sportsbook_*` transaction type
(bet, settlement, void, partial settlement, cashout, rollback alike) —
supersedes nothing in §3/§5/§8's entry tables, only clarifies which
uniqueness mechanism each mode's posting uses:**

| Mode | `provider_id` | `provider_tx_id` | Uniqueness mechanism (DB-enforced) | `idempotency_key` |
|---|---|---|---|---|
| External-provider (widget/iframe, feed/API) | the adapter's provider identifier (opaque, non-`NULL`) | the provider's own per-event reference, `occurrence_ordinal`-composed per §14.1–§14.4 where applicable | `UNIQUE (tenant_id, provider_id, provider_tx_id) WHERE provider_id IS NOT NULL` | populated (schema requires `NOT NULL`) but not the operative uniqueness check for this row — set to a platform-generated value never reused as an idempotency input elsewhere, identical to how any other provider-originated transaction already satisfies the `NOT NULL` constraint today |
| In-house sportsbook engine | `NULL` (ADR 0033 §2, unchanged, propagated verbatim by the adapter — never translated into a sentinel) | `NULL` (never populated when `provider_id` is `NULL` — nothing for the partial index to key on, so nothing is put there) | `UNIQUE (tenant_id, idempotency_key)` (unconditional — no `WHERE` clause, always evaluated) | the in-house engine's own per-event reference — **the same value, derived under the same rules**, that would have played `provider_bet_reference`/`provider_tx_id`'s role in external-provider mode (§3/§5/§8's "or, for an in-house engine, using its own internally-generated reference in the identical role" language, unchanged by this section) |

**The one thing that must not happen, named explicitly because it is the
same failure shape as the original gap one field over**: `idempotency_key`
for an in-house posting must **never** be a freshly-generated value minted
at insert time (e.g. a new random UUID per attempt). If it were, the
`UNIQUE (tenant_id, idempotency_key)` constraint would technically still
be "enforced by the database," but it would never actually *fire*, because
a retried attempt would submit a different key every time and never
collide with itself — satisfying the letter of CLAUDE.md's rule while
defeating its purpose exactly as the original `provider_id IS NULL` gap
did. `idempotency_key` must instead be derived exactly the way §14.1
already specifies for the analogous provider-side reference: from a
per-occurrence signal intrinsic to the specific event (the in-house
engine's own acceptance/settlement/void decision id) or from the adapter's
own inbound-delivery deduplication record, assigned once and reused
verbatim on retry — never recomputed as "the next in a sequence," for the
identical non-reproducibility reason §14.1 already rules that out for
`occurrence_ordinal`. This is not a new rule invented here; it is the
existing "admin action's own id, generated once at approval and reused
verbatim on retry" pattern §11 already cites from Flow 16's
`manual_adjustment`, generalized from "rare staff action" to "every
in-house-mode posting."

**Composition pattern for the three occurrence-ordinal types
(`sportsbook_partial_settlement`, `sportsbook_cashout`,
`sportsbook_rollback`/re-settlement) — confirms it generalizes cleanly.**
§14.1 states the pattern as "the adapter composes the value it submits as
`provider_tx_id` to already include this ordinal (e.g. `{provider's
settlement reference}#{occurrence_ordinal}`)" — worded, as written,
against the external-provider-mode field. **Yes, §14 as originally written
implicitly assumed a real external `provider_id`/`provider_tx_id` pair for
that composition** (the example is literally "provider's settlement
reference"), because §14 was scoped to tightening §11's existing
`provider_tx_id`-keyed mechanism and did not yet separately address the
in-house routing question this section resolves. **The composition pattern
itself is field-agnostic and works identically for the in-house case**:
for in-house-mode partial settlement/cashout/rollback, the adapter composes
`{in-house engine's own per-event reference}#{occurrence_ordinal}` and
submits it as `idempotency_key` (never `provider_tx_id`, which stays
`NULL`) instead. Nothing about `occurrence_ordinal`'s derivation rules
(never inferred from ledger state, never "count and add one," always
intrinsic to the specific occurrence — §14.1) changes; only the column the
composed string lands in changes, following the same `provider_id IS NULL`
⇒ `idempotency_key` routing this section establishes for every other
sportsbook transaction type.

**Worked example — two identical in-house `sportsbook_bet` placement
attempts, same bet, from a retried request (e.g. a client-side timeout
that retries an accepted-but-unacknowledged placement).**

1. The in-house engine accepts bet `B`, minting its own internal
   acceptance reference `iref-B-accept-7f3a` — derived, per the rule
   above, from the acceptance decision's own intrinsic id (never a fresh
   UUID minted per attempt), so a retry of the *same* acceptance
   reproduces this identical string.
2. First placement attempt: the adapter hands the ledger posting layer a
   `sportsbook_bet` instruction with `provider_id = NULL` (in-house mode,
   per ADR 0033 §2), `provider_tx_id = NULL`, `idempotency_key =
   "iref-B-accept-7f3a"`. `internal/ledger` inserts one `LedgerTransaction`
   row (`tenant_id = T`) with two balanced entries (Dr `player_cash` `S` ·
   Cr `player_locked` `S`, cash-funded case) and commits. Audit event
   `sportsbook_bet.locked` fires.
3. The client never receives the response (network timeout) and retries
   the identical placement. The in-house engine's acceptance-decision
   record is unchanged — it is the same accepted bet, not a new decision —
   so the adapter reproduces the identical `iref-B-accept-7f3a` reference.
4. Second attempt reaches `internal/ledger` with the same
   `(tenant_id = T, idempotency_key = "iref-B-accept-7f3a")`. Because
   `UNIQUE (tenant_id, idempotency_key)` has **no `WHERE` clause** — unlike
   mechanism 1, it is evaluated for every row regardless of what
   `provider_id` holds — the `INSERT` collides with the constraint at the
   database level, exactly as it would for any other internally-originated
   posting (a `manual_adjustment`, a bonus-engine-triggered posting) today.
5. ADR 0020's exact-retry semantics (§11, unchanged) take over from there:
   same key, same payload ⇒ `SAVEPOINT`-based exact-retry path, the
   original transaction's result is returned, and **no second
   `LedgerTransaction` row is created** — `S` is locked exactly once, not
   twice. Had the retried payload somehow differed (a defect elsewhere),
   ADR 0020's `ErrIdempotencyKeyReused` would fire instead of a silent
   double-post — still a database-arbitrated outcome, never a
   check-then-insert race.

Contrast with the gap as originally reachable: if `provider_id` had been
left `NULL` **and** the posting had still been (incorrectly) attempted
through the `provider_tx_id` path, or if `idempotency_key` had been a
fresh UUID per attempt, step 4 would have inserted a **second**,
independent `LedgerTransaction` — a second `S` locked against the player's
`player_cash`/`player_locked` accounts for one accepted bet, a real
double-debit. The routing rule and the reproducible-reference requirement
above are what jointly close that off, at the database constraint level,
not by adapter discipline.

**Requirement flagged for `sportsbook` (doc 09), not implemented by this
ADR.** `docs/architecture/09-sportsbook-architecture.md` should state, for
its in-house-engine mode, that: (a) `provider_id` is left `NULL` on every
posting instruction it hands to the ledger posting layer for in-house-
originated events (consistent with ADR 0033 §2's canonical-event
statement, which doc 09 already implicitly relies on); and (b) its adapter
mints one stable, intrinsic, per-event reference per lifecycle event
(acceptance, settlement, void, per-occurrence partial settlement/cashout/
rollback) and supplies it as `idempotency_key` — never as `provider_tx_id`,
and never freshly regenerated on retry of the same event — per this
section's routing rule and reproducibility requirement. This is named here
for the Orchestrator to route to `sportsbook`; this ADR does not edit doc
09 or ADR 0033 itself, and ADR 0033 §2 needs no edit (it already says the
right thing at the canonical-event layer; this section only makes the
ledger-posting-layer consequence of that same fact explicit).

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 15. `player_locked` origin-split — proposed resolution (Stage 4H-B0-R5, PROPOSAL ONLY, NOT IMPLEMENTED)

**This section is a cross-reference, not the proposal itself.** Per the
directive's own reasoning — the underlying gap is not sportsbook-specific
(`ledger-accounting-model.md` §6.2, ADR 0032 §10 both named it before
sportsbook existed, and any future bonus-funded casino feature with a
contingent/locked state would hit the identical gap) — the full proposal
is written where a platform-wide fix belongs: **`ledger-accounting-
model.md` §6.3**, a new section replacing that document's §6.2 "open item
so far" framing with an exact, reviewable proposal, in the same spirit and
rigor as ADR 0035 §1.3.1's formalization of the agent-float schema
proposal (worked SQL sketch, row-shape reasoning, explicit
`NOT IMPLEMENTED` status, explicit non-authority to decide it alone).

**What §6.3 proposes, in one paragraph**: split `player_locked` into two
`account_type` values, `player_locked_cash` and `player_locked_bonus`
(an additive `account_type` CHECK widening only — no new column, no new
owner family, no RLS change, because both new types are player-owned,
wallet-scoped, and fit the *existing* `UNIQUE (wallet_id, account_type,
asset_code)` constraint unchanged), and extend invariant B1's covered
"bonus-denominated player accounts" set from `{player_bonus}` to
`{player_bonus, player_locked_bonus}`. A second candidate shape — an
origin-tag column on `ledger_entries` instead of splitting the account —
was seriously considered and is written up and rejected in §6.3, with
reasons.

**Confirmed for the sportsbook-specific case, applying §6.3's proposed
mechanism to §3/§5/§8 above, once and if it is approved (this ADR does not
adopt it unilaterally):**

- **A. Cash-funded bet** — unaffected by whether this proposal is adopted;
  §3/§5/§8's cash-funded entries are correct as written today.
  **Recommendation carried from §6.3**: mint `player_locked_cash` (never
  bare `player_locked`) from the first cash-funded sportsbook posting
  onward, so that if/when this proposal is later approved, adding
  `player_locked_bonus` requires zero backfill of any already-posted
  `player_locked_cash` row.
- **B. Bonus-funded bet** — placement becomes `Dr player_bonus X · Cr
  player_locked_bonus X` (the §3 table's "once unblocked" row, now with
  the concrete account name). §6.3 shows this needs **no mirror leg at
  lock time** — the transfer stays entirely inside the extended
  bonus-denominated set, so B1 holds through the lock unchanged — and that
  wagering-progress attribution needs **no change at all**, because it is
  derived from the debit to `player_bonus` (`ledger-accounting-model.md`
  §6.1/ADR 0032 §0), which already posts at lock time regardless of where
  the credited leg lands.
- **C. Mixed cash+bonus bet** — supported, using the same
  split-instruction boundary §9 already commits to (sportsbook computes
  the split, `internal/ledger` posts it): `Dr player_cash C · Dr
  player_bonus B · Cr player_locked_cash C · Cr player_locked_bonus B`,
  four entries, balanced per asset, no new mechanism beyond crediting two
  accounts instead of one.
- **D. Rollback of a cash-funded bet** — `Dr player_locked_cash X · Cr
  player_cash X`, confirmed, unaffected by this proposal beyond the
  account name.
- **E. Rollback of a bonus-funded bet** — `Dr player_locked_bonus X · Cr
  player_bonus X`. This is the case that breaks today without the split:
  a rollback handler with only a single undifferentiated `player_locked`
  balance to release has no reliable, indexable way to know this
  particular bet's stake was bonus-origin, and either (i) credits
  `player_cash`, handing the player real, withdrawable money they were
  never entitled to, or (ii) defaults to `player_bonus` for a bet that was
  actually cash-funded, wrongly re-restricting real cash as non-withdrawable
  bonus funds. Both are real financial-integrity defects, not
  rounding-scale errors.
- **C-void / C-loss / C-win / C-partial / C-cashout — the mixed-funded
  *unlock* side** (Stage 4H-B0-R5 round 2, added at this specialist's
  request): case C above proves only the **lock**. The corresponding
  unlock-side cases for a mixed-funded bet — void (§8.1), total loss
  (§5), win (§5), partial settlement (§8.2) and cashout (§8.3) — are
  worked through with full entry tables and invariant checks in
  `ledger-accounting-model.md` **§6.3.3.2**, and the mechanism for
  recovering a bet's original `C`/`B` amounts at settlement time (a
  `correlation_id`-scoped query against the bet's `sportsbook_bet`
  transaction, joined to `ledger_accounts` for `account_type IN
  ('player_locked_cash','player_locked_bonus')`) is specified in
  **§6.3.3.1**. That recovery mechanism is required specifically because
  **this ADR's lock and settlement are separated in time**, unlike
  casino's atomic resolution — it was missing from round 1 and is not
  optional. Two statuses differ from the rest of §15 and must not be
  read as approved along with it: the **proportional payout split** for
  a mixed-funded win is **newly proposed and unreviewed**, and
  **mixed-funded/bonus-funded cashout is an explicit `OPEN QUESTION`**
  (§8.3's "meaning, not arithmetic" distinction is exactly why it cannot
  be extrapolated from partial settlement) and stays **BLOCKED**
  independently of whether Shape A is approved.
- **F. Win/settlement after a bonus-funded bet** — applies ADR 0032's
  existing casino precedent unchanged, generalized to the extended
  bonus-denominated set (§6.3): the payout returns to `player_bonus`
  (continuing wagering progress), never `player_cash`, and the
  `promo_liability`/`bonus_expense` mirror pair fires at **this** point —
  the moment value actually leaves `{player_bonus, player_locked_bonus}`
  for `house_gaming` — rather than at lock time, which is the deferred
  version of ADR 0032 §3's existing recognition-timing rule, not a new
  rule invented for sportsbook.
- **G. Loss after a bonus-funded bet** — the stake absorption itself
  (`Dr player_locked_{origin} X · Cr house_gaming X`) needs no origin
  distinction; the *only* origin-dependence is whether the
  `promo_liability`/`bonus_expense` mirror pair also fires alongside it
  (only for the bonus-origin case, never for cash), for the identical
  reason as F.

> **Stage 4H-B0-R5 round-2 factual correction (`architect` review).** An
> earlier draft of this section — and of the Consequences bullet below
> that mirrors it — described the split as using "the same kind of
> additive CHECK-widening ADR 0032 already used for `bonus_expense`,"
> phrased so as to imply the platform had **already executed** such a
> migration successfully. **That implication was false and is
> withdrawn.** ADR 0032 §2 *architecturally decided* to add
> `bonus_expense` as a twelfth account type; its own status line reads
> `RESOLVED (architecture) — NOT IMPLEMENTED` and it still lists "an
> additive migration adding the account type" as outstanding.
> `bonus_expense` appears in **zero** files under `migrations/`, and
> migration `0020_create_ledger_accounts.up.sql`'s `account_type` CHECK
> still lists exactly the original eleven values. Correct statement: ADR
> 0032 **decided** to use this shape; **nobody has executed it**. If this
> proposal is migrated before ADR 0032's own migration lands, it would be
> **the first `account_type` CHECK-widening migration ever executed
> against this schema** — which raises, not lowers, the operational bar
> (down-migration, the auto-generated constraint name
> `ledger_accounts_account_type_check`, and a rehearsal against an
> instance already holding rows). See `ledger-accounting-model.md`
> §6.3.1's matching correction and §6.3.2's verified-constraint-identity
> note.

**Status: `NOT IMPLEMENTED`. Proposal only, requiring `architect` +
`bonus-engine` + `sportsbook` review and, per CLAUDE.md, human approval
before any migration is written** — this changes a Blueprint-listed
account type's shape and is explicitly outside `ledger-finance`'s
unilateral authority (CLAUDE.md: "no specialist redesigns shared
architecture unilaterally"). §9's `OPEN DECISION` status is **not** closed
by this section; it is given a concrete, reviewable form, exactly as ADR
0035 §1.3.1 did for the agent-float schema question. Every bonus-funded
sportsbook posting in §3/§5/§8 remains **BLOCKED** until a human closes
this decision and the resulting migration lands.

## Consequences

- **Follow-up edits required** to documents this ADR does not own the
  right to change in this stage (all `NOT IMPLEMENTED` until made):
  `ledger-accounting-model.md` §2 (the `player_locked`/`player_cash`/
  `house_gaming` rows' "Allowed transaction types" columns already list
  most sportsbook types as forward references to this ADR — replace the
  forward reference with a citation to this ADR and add
  `sportsbook_partial_settlement`/`sportsbook_cashout`/`sportsbook_rollback`
  where missing); `financial-transaction-flows.md` Flows 8–11 (restate
  Flow 11 as two flows — partial settlement and cashout — per §8 above,
  and add the rejection/rollback/re-settlement flows §4/§10 introduce);
  ADR 0019's actor matrix (add a row per `sportsbook_*` transaction type,
  scoped to the provider-callback-verified tenant, per §11).
- **Follow-up edits required *if and when* the §15 / `ledger-accounting-
  model.md` §6.3 origin split is approved** — listed separately from the
  bullet above because they are conditional on a decision nobody has
  made yet, and `NOT IMPLEMENTED` regardless. The authoritative,
  exhaustive list (call sites, migrations, documents, tests) is
  `ledger-accounting-model.md` §6.3.4, not duplicated here. The two
  entries worth naming in this ADR because they are owned **outside**
  `ledger-finance` and would otherwise be discovered late:
  1. `internal/wallet/wallet.go` — `GetSummary`'s `account_type` switch
     (`ledger-finance` owns the fix; flagged here because it disproves
     the "purely additive, no code change" reading of §15).
  2. **`docs/decisions/0034-bonus-gamification-rg-kyc-identity-
     integration.md` §14.1** — found by `bonus-engine`'s Stage 4H-B0-R5
     review. That section still writes the **self-exclusion-triggered
     void** posting as an undifferentiated
     `Dr player_locked / Cr player_cash|player_bonus`, which under the
     split must become `Dr player_locked_cash · Cr player_cash` **or**
     `Dr player_locked_bonus · Cr player_bonus` (or both legs for a
     mixed-funded bet — the exact shape is
     `ledger-accounting-model.md` §6.3.3.2 case C-void, which posts all
     four entries and needs **no** `promo_liability` mirror, since a
     void's bonus leg is a transfer *within* the extended bonus set).
     **This edit is deliberately not made by `ledger-finance` and is not
     made in this round**: ADR 0034 is owned by
     `identity-compliance`/`architect`, and self-exclusion semantics are
     theirs. It is recorded here only so the edit is scheduled rather
     than discovered during implementation.
- **Migrations required before any sportsbook posting** (additive, own
  stage, no SQL written here, mirroring the "migration order, no SQL"
  format `docs/architecture/27-stage-4h-b0-scope-and-implementation-
  plan.md` §1.1 used for bonus):
  1. `ledger_transactions.transaction_type` CHECK widening to add
     `sportsbook_bet`, `sportsbook_settlement`, `sportsbook_void`,
     `sportsbook_partial_settlement`, `sportsbook_cashout`,
     `sportsbook_rollback` (six values; no new `account_type` values are
     needed at all, per §1).
  2. (Deferred, blocked by §9) once the `player_locked` origin split is
     jointly authorized: the `player_locked_cash`/`player_locked_bonus`
     split itself (or equivalent origin dimension), plus extending
     invariant B1's covered-account set — owned jointly per ADR 0032 §10,
     not sequenced further here.
  3. (Future, only if `internal/risk`'s `cumulative_amount` support for
     `sportsbook_bet` is wanted at the same time sportsbook ships) the
     `operationLedgerTransactionTypes`/`operationLedgerRollbackTypes`
     mapping entries described in §13 — no migration required for this
     one (it is a Go map, not a schema change), listed here only for
     sequencing visibility since it depends on step 1 existing first.
  Until step 1 lands, every sportsbook posting in this ADR is `BLOCKED` by
  the existing `CHECK` constraint — the intended behavior, not an obstacle
  to route around, identical to bonus's own pre-migration state.
- **No change to ADR 0021's resolved rounding decision.** This ADR reuses
  DS-1/DS-2/DS-3 verbatim (§7) and adds no new rounding site that isn't
  already covered by "round once, at the final monetary boundary,
  round-half-up, one shared helper." The only sportsbook-specific
  computations that could ever need rounding (an in-house cashout price or
  bet-builder combo payout) do not exist in the near-term provider-driven
  shape and are named, not built, in §5/§7/§8.3.
- **No change to ADR 0032's bonus accounting.** Bonus-funded sportsbook
  wagering remains `BLOCKED` on the pre-existing `player_locked` origin
  gap (§9); this ADR adds no new bonus-accounting mechanism and commits
  only to reusing ADR 0032 §2's mirror-leg pattern unchanged once that gap
  closes.
- **No change to the core ledger schema beyond the additive
  `transaction_type` CHECK widening in migration step 1 above.** No new
  `account_type`, no new column on `LedgerAccount`/`LedgerTransaction`/
  `LedgerEntry`, no new table. **§14 (Stage 4H-B0-R5) confirms this
  holds**: the occurrence discriminator it requires is carried inside the
  existing opaque `provider_tx_id` string, composed by the adapter, not a
  new column. **§15 (Stage 4H-B0-R5) is the one exception, and it is a
  proposal, not a decision**: if and when the `player_locked` origin
  split is approved (`ledger-accounting-model.md` §6.3), it adds two
  `account_type` values (`player_locked_cash`/`player_locked_bonus`) via
  an additive CHECK-widening — the same *shape* ADR 0032 **decided**
  (`RESOLVED (architecture) — NOT IMPLEMENTED`) to use for
  `bonus_expense`, **not** a shape this platform has already executed:
  `bonus_expense` has never been migrated, so this would be the first
  such widening ever executed against this schema (see §15's round-2
  factual correction). Still no new column, no new owner family, no new
  table, but a schema change this ADR does not have the authority to make
  unilaterally — **and, per `ledger-accounting-model.md` §6.3.4, not a
  code-free one either**: `internal/wallet/wallet.go`'s `GetSummary`
  `account_type` switch must change in the same slice or
  `Summary.LockedBalance` silently reports zero while real locked funds
  exist.
- **New reconciliation consumers, not new reconciliation streams.** §6's
  open-liability query and §12's provider-statement matching are read
  patterns against reconciliation machinery that already exists
  (`reconciliation-model.md`'s ledger-vs-projection and provider-statement
  streams) — no dedicated new stream is proposed, unlike ADR 0032's B1
  stream, because sportsbook introduces no new invariant class beyond
  existing ones (#1–#15) plus B1 once bonus-funded stakes unblock.
- **Testing floor** (CLAUDE.md's financial test list, non-negotiable, owned
  by `ledger-finance`, mirroring ADR 0032's format): placement happy path
  (cash-funded); duplicate/replayed placement callback; concurrent
  placement vs. insufficient funds; rejection with no lock ever posted;
  rejection with an optimistic lock correctly released; a stuck-lock
  integrity alert firing when a rejection's release never arrives within
  the provider's timeout; settlement win/loss happy paths; a settlement
  referencing an unknown bet (integrity alert, not a silent accept); void
  before settlement; void after settlement (full reversal chain);
  duplicate void; partial settlement of one leg of a multi-leg bet with
  the remainder correctly still locked; two independent partial-settlement
  events on the same bet each independently idempotent (regression-testing
  the exact key-collision failure §11 names); a partial settlement where
  `P > R` and where `R > P`; full cashout; partial cashout followed by a
  second cashout on the remainder; a cashout on a bet in a winning position
  (`P > R`); market correction after full settlement (rollback + correct
  re-settlement, net effect verified); market correction after a partial
  settlement; rollback of a never-seen settlement (tombstone, then a
  late-arriving original settlement rejected); double-rollback race
  (row-lock rejection); open-liability query correctness against a mix of
  locked/settled/voided/cashed-out bets across multiple wallets and
  assets; multi-asset (BTC or an 18-exponent asset) placement/settlement/
  void/partial/cashout all producing correctly-scaled `NUMERIC(38,0)`
  entries with no hardcoded exponent. A happy-path bet/settlement pair does
  not satisfy this list.
- **`ledger-finance` sign-off is required** on the sportsbook domain
  model, provider adapter, and (if ever built) in-house pricing/settlement
  engine before any of them is marked complete, limited to their monetary
  aspects — identical scope limitation to ADR 0032's own closing statement.
  Nothing in this ADR gives `ledger-finance` a view on odds/trading logic,
  provider selection, or UI/UX.

## Open decisions referred upward

1. **`player_locked` origin split for bonus-funded sportsbook wagering**
   (§9) — `architect` + `sportsbook` + `ledger-finance`, already flagged by
   `ledger-accounting-model.md` §6.2 and ADR 0032 §10, confirmed here as
   still blocking every bonus-funded flow in this ADR. **Stage 4H-B0-R5**
   formalizes a concrete, reviewable proposal (§15, full mechanism in
   `ledger-accounting-model.md` §6.3) — still `NOT IMPLEMENTED`, still not
   decided, now in a form the three named reviewers can review directly
   rather than re-deriving a shape from the open item's prose. **Round 2
   (also Stage 4H-B0-R5)**: all three reviewers approved the *shape*
   (Shape A) and raised specific gaps, now closed in
   `ledger-accounting-model.md` §6.3 — but that round **added** unreviewed
   content of its own (Rule B2 extended, the mixed-funded unlock cases,
   the split-recovery mechanism, the proportional-payout rule for a
   mixed-funded win) which needs the **same** review gate; see
   `ledger-accounting-model.md` §6.3.5.1 for the per-item review status,
   which is deliberately **not** uniform across §6.3.
   Human approval is still outstanding.
2. **`sportsbook_bet`'s Risk enforcement wiring** (§13) — not a
   `ledger-finance` open decision on its own (the `Operation` value and
   `min_amount`/`max_amount` support already exist; `operationLedgerRollbackTypes["sportsbook_bet"]`
   nets only `sportsbook_void`, a plain one-to-one mapping requiring no
   map-shape widening, per §13's Stage 4H-B0-R4 Wave-2 correction). The one
   recorded-but-non-blocking implementation item is
   `operationLedgerTransactionTypes["sportsbook_settlement"]`'s eventual
   widening to a list (§13), needed only if/when `cumulative_amount` is
   ever wired for sportsbook settlement payouts — a small joint `risk` +
   `ledger-finance` implementation item for whichever stage does that, not
   for the stage that wires `internal/sportsbook`'s bet-placement gating.
3. **Cross-domain aggregate player exposure spanning bonus + casino +
   sportsbook** — restated, not newly raised; ADR 0031 §17's existing open
   decision, for which sportsbook's own "potential payout exposure" (§2)
   is additional motivating evidence, not a new instance requiring its own
   answer.
4. **Whether a future in-house pricing/settlement engine is ever
   authorized** (§5/§7/§8.3's "if built" cases) — a product/build-vs-buy
   decision doc 09 already frames as deferred; this ADR takes no position
   on timing and only pre-specifies the rounding/reconciliation contract
   that would apply if and when it happens.
5. **A future jurisdiction-mandated bet-withdrawal window** (§8.4) — not
   a current requirement; recorded so a future reader does not have to
   re-derive why a player-initiated bet withdrawal capability is absent
   from this ADR's flow list.
6. **Mixed-/bonus-funded cashout proceeds split** (§8.3;
   `ledger-accounting-model.md` §6.3.3.2 case C-cashout) — **new in Stage
   4H-B0-R5 round 2**, raised by `sportsbook`'s review of the origin-split
   proposal. Whether a cashout on a bet with bonus-origin stake pays
   proportionally into `player_cash`/`player_bonus`, entirely into
   `player_cash`, or is disallowed upstream in `internal/sportsbook` so
   the posting question never arises. Requires `bonus-engine` +
   `sportsbook` + `product-owner-proxy`; **not** answered by resolving
   item 1, and not answerable by `ledger-finance` alone, because every
   candidate posting is balanced and invariant-B1-safe — no financial
   invariant discriminates between them, so the decision is
   bonus-policy/RG/product, not accounting. Bonus- and mixed-funded
   cashout stays **BLOCKED** until it is closed; cash-funded cashout is
   unaffected.

## Owner

`ledger-finance`. Sportsbook product/domain logic, odds/trading, and the
provider adapter are owned by `sportsbook`; neither may alter the
accounting treatments above without `ledger-finance` sign-off, and
`ledger-finance` does not alter sportsbook's rules, odds handling, or
provider selection.

## Cross-references

`CLAUDE.md` ("Financial / ledger rules"); ADR 0001, 0007, 0019, 0020, 0021,
0031 (§1–§18), 0032, 0033 (§2's `leg_reference` — the precedent §14
generalizes into `occurrence_ordinal`), 0035 (§1.3.1 — the tone/rigor
precedent §15/`ledger-accounting-model.md` §6.3 follow for a
`ledger-finance` schema *proposal*, not a decision), 0037 (Asset Registry
and FX-conversion architecture, §7's asset-exponent mechanism — cited per
Stage 4H-B0-R4 Wave-2 review); `docs/architecture/09-sportsbook-
architecture.md` (product/build-vs-buy framing, owned by `sportsbook`);
`ledger-accounting-model.md` (§6.2/§6.3 — the `player_locked` origin-split
proposal §15 cross-references), `financial-transaction-flows.md` (Flows
8–11, 17); `10-bonus-engine-architecture.md` §6 (the
split-instruction/lifecycle-event boundary this ADR reuses for the
eventual bonus-funded case); `reconciliation-model.md`;
`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` §1.1
(the migration-order format this ADR's Consequences section mirrors);
`internal/ledger`, `internal/casino` (`postRollback`, `postBet`'s
Risk-integration position, `RoundID`'s correlation role cited in §14.1).

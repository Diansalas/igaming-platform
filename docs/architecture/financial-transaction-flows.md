# Financial Transaction Flows

Status: Stage 3A (Financial Architecture Freeze) — architecture only,
`NOT IMPLEMENTED`. Source: Blueprint §4.2, §4.6, §4.7 (sportsbook), §4.5
(bonus), extending `ledger-accounting-model.md`. Owner: `ledger-finance`,
with `payments` (deposit/withdrawal), `casino`, `sportsbook`, and
`bonus-engine` co-owning the flows specific to their domain.

Every flow below states: initiating event, source/destination accounts,
asset, amount representation, transaction boundary, idempotency key,
external reference, expected balance effects, failure behavior, retry
behavior, compensation behavior, audit event, and which
`ledger-accounting-model.md` §6 invariants apply. All amounts are minor
units per the asset's registered exponent (ADR 0007). "Transaction
boundary" always means: one `LedgerTransaction` row, one or more balanced
`LedgerEntry` rows, written in one database transaction — see
`ledger-accounting-model.md` §1.2 for why there is no partial/pending
ledger state.

**Who may originate each flow (authorization boundary).** Each flow below
names an *initiating event*, but an initiating event is not an
authorization. Which actor class may cause which `transaction_type` is
specified once, canonically, in
`docs/decisions/0019-authoritative-ledger-and-balance-projection-architecture.md`
§"Who may originate which posting", and is enforced server-side at the
ledger posting API boundary — not by which HTTP handler happened to call
it, and never inferred from the UI. In particular: a player session can
never originate a `manual_adjustment`, a `bonus_grant`, or any direct
credit to `player_cash`/`player_bonus`/`house_gaming`; a provider callback
is scoped to the tenant resolved from the credential that *verified the
callback signature*, never to a tenant or player identifier carried in the
payload. This paragraph was added by the Stage 3A `security` review, which
found no single statement of that boundary anywhere in the Stage 3A set.

## 1. Deposit — `BLUEPRINT`

- **Initiating event**: PSP confirms a successful deposit (webhook/
  callback), or (crypto) custodian confirms sufficient on-chain
  confirmations.
- **Accounts**: debit `psp_clearing` (tenant), credit `player_cash`
  (wallet).
- **Asset**: the wallet's asset; PSP/custodian amount must already be in
  that asset (no implicit conversion — see `financial-domain-model.md` on
  cross-asset movement being an explicit `ConversionOperation`, not part
  of a deposit).
- **Idempotency key**: `(provider_id, provider_tx_id)` = (PSP identifier,
  PSP's transaction id) or (custodian identifier, on-chain tx hash +
  output index for UTXO chains / tx hash for account-model chains).
- **Correlation/causation**: `correlation_id` = the deposit request's own
  id (created when the player initiated the deposit, before the callback
  arrives); `causation_id` = the specific callback delivery id.
- **Balance effect**: `player_cash` +amount.
- **Failure behavior**: PSP reports failure/decline → no `LedgerTransaction`
  is ever created (§4 of the accounting model — failed attempts that never
  reached "money moved" leave no ledger row; the payment-orchestrator's own
  attempt log, not the ledger, records the decline attempt).
- **Retry behavior**: PSP redelivers the same success callback → unique
  constraint on `(provider_id, provider_tx_id)` makes the second insert a
  no-op; handler returns the already-posted result.
- **Compensation**: see Flow 2.
- **Audit event**: `deposit.posted` (actor = system/PSP-callback, amount,
  asset, wallet, provider reference).
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8.

## 2. Deposit reversal / failure (post-hoc) — `ARCHITECTURAL DECISION`

- **Initiating event**: a PSP later reverses a deposit already posted
  (chargeback, ACH return, confirmed-then-orphaned crypto reorg beyond the
  custodian's finality threshold).
- **Accounts**: debit `player_cash`, credit `psp_clearing` — the exact
  inverse of Flow 1's entries, as a **new** `LedgerTransaction` with
  `reverses_transaction_id` pointing at the original.
- **Balance effect**: `player_cash` −amount (may drive the wallet negative
  if already spent — see `OPEN DECISION` below).
- **Failure behavior / idempotency**: the reversal itself has its own
  `(provider_id, provider_tx_id)` (the chargeback/return's own reference),
  so a redelivered reversal callback is equally idempotent.
- **Tombstone case**: if the *original* deposit was never seen by the
  ledger (e.g. lost callback) and a reversal for it arrives, the reversal
  handler writes a tombstone row keyed by the original's
  `(provider_id, provider_tx_id)` so a late-arriving original deposit
  callback is rejected rather than posted — per CLAUDE.md's "a rollback
  for a transaction never seen writes a tombstone."
- **Audit event**: `deposit.reversed` (includes reason, original
  transaction id).
- **`OPEN DECISION`**: whether a deposit reversal that would drive
  `player_cash` negative is (a) permitted (wallet shows a negative
  balance, collections/offset process handled outside the ledger) or (b)
  blocked and escalated to a manual `manual_adjustment` case queue. The
  Blueprint does not address negative-cash-balance policy; this is a
  business/risk decision, not invented here.
- **Invariants engaged**: #1, #2, #3, #4, #5, #10, #14.

## 3. Withdrawal — `BLUEPRINT`

- **Initiating event**: player requests a withdrawal; full state machine in
  `withdrawal-state-machine.md`. This flow covers only the ledger-visible
  moments within that state machine.
- **Step A (request accepted)**: debit `player_cash`, credit
  `player_withdrawal_hold` (both wallet-scoped) — moves the amount out of
  spendable cash **immediately** on request acceptance, before any
  approval, so it cannot be spent twice (invariant #12) while a decision
  is pending.
- **Step B (approved + submitted to PSP/custodian and confirmed sent)**:
  debit `player_withdrawal_hold`, credit `psp_clearing` (or the
  custodian-facing account, per `crypto-custody-boundary.md`).
- **Asset**: wallet's asset.
- **Idempotency key**: Step A keyed by the withdrawal request's own id
  (`idempotency_key`, one request = one hold); Step B keyed by
  `(provider_id, provider_tx_id)` from the PSP/custodian send
  confirmation.
- **Balance effect**: net `player_cash` −amount once both steps post; in
  between, funds are visibly held, not vanished (queryable via
  `player_withdrawal_hold`).
- **Failure behavior**: request rejected at approval → Flow 4 (reversal of
  Step A only, Step B never happens). PSP/custodian send fails after
  submission → Flow 4's "post-submission failure" variant.
- **Audit event**: `withdrawal.requested`, `withdrawal.hold_placed`,
  `withdrawal.sent` — each a distinct audit event tied to the same
  `correlation_id`.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #11, #12, #15.

## 4. Withdrawal reversal / failure — `ARCHITECTURAL DECISION`

- **Initiating event**: withdrawal rejected during approval, or fails after
  submission (PSP decline, custodian rejects, on-chain broadcast fails).
- **Accounts (pre-submission rejection)**: debit `player_withdrawal_hold`,
  credit `player_cash` — reverses Step A only.
- **Accounts (post-submission failure)**: two separate compensating
  `LedgerTransaction`s (funds never left the platform's custody boundary,
  or the custodian confirms non-delivery), each with its own single
  `reverses_transaction_id` — `LedgerTransaction.reverses_transaction_id`
  is a single FK (`ledger-accounting-model.md` §1.2), so one transaction
  cannot reverse two originals: (1) reverses Step B — debit
  `psp_clearing`/custodian account, credit `player_withdrawal_hold`; (2)
  reverses Step A — debit `player_withdrawal_hold`, credit `player_cash`.
  Both share the withdrawal's `correlation_id` so the full chain (Step A →
  Step B → reversal of B → reversal of A) is traceable as one business
  operation; net effect is `player_cash` restored to its pre-withdrawal
  amount.
- **Idempotency**: keyed by the rejection/failure event's own provider
  reference, same as Flow 2.
- **Audit event**: `withdrawal.rejected` / `withdrawal.failed`.
- **Invariants engaged**: #1, #2, #3, #4, #5, #10, #11, #14.

## 5. Casino bet — `BLUEPRINT`

- **Initiating event**: casino provider's bet callback (Blueprint §4.3
  wallet-callback contract).
- **Accounts**: debit `player_cash` and/or `player_bonus` (split per
  active bonus-wagering rules — see the worked example in
  `06-wallet-ledger-architecture.md`), credit `house_gaming`. If the game
  is jackpot-enabled, an additional split credits `jackpot_contribution`
  (`OPEN DECISION` in `ledger-accounting-model.md` §2 on whether this is a
  liability or expense recognition).
- **Asset**: wallet's asset (the game's stake currency, resolved 1:1 to a
  wallet — no cross-asset conversion mid-bet).
- **Idempotency key**: `(provider_id, provider_tx_id)` = (casino provider
  id, provider's bet/round reference) — Blueprint explicitly frames
  provider callback retries as "the normal case."
- **Balance effect**: `player_cash`/`player_bonus` −amount,
  `house_gaming` +amount (net of any jackpot split).
- **Failure behavior**: insufficient funds → the transaction is rejected
  before posting (checked in the same DB transaction that would post it —
  invariant #15, read-then-write atomically, detailed in
  `docs/decisions/0020`); the provider callback returns a decline, no
  `LedgerTransaction` row is created.
- **Retry behavior**: exact retry (same `provider_tx_id`) → idempotent
  no-op returning the original result, per Blueprint's callback contract.
- **Compensation**: Flow 7 (rollback).
- **Bonus-funded portion — mirror legs required (ADR 0032 §2, Rule B2).**
  **Wave-2 ledger-finance review correction (P1-1)**: an earlier draft of
  this flow described only the two-entry cash-funded shape. Any entry in
  this flow against `player_bonus` is accompanied, in the **same
  `LedgerTransaction`**, by an equal and opposite entry against
  `promo_liability`, joined by an equal `bonus_expense` entry so the
  transaction still balances per asset. A bonus-funded stake of `X` is
  therefore four entries (Dr `player_bonus` `X` · Cr `house_gaming` `X` ·
  Dr `bonus_expense` `X` · Cr `promo_liability` `X`). A cash-funded stake
  is unchanged (two entries). A mixed cash+bonus stake is the cash portion
  as a two-entry pair plus the bonus portion as its own four-entry group
  in the same transaction. These legs are generated/validated in
  `internal/ledger`, never hand-assembled by `internal/casino`.
  Provider-funded grants substitute `provider_payable` for `bonus_expense`
  (ADR 0032 §6(b)). **Invariants engaged: add B1.**
- **Audit event**: `casino_bet.posted`.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #15, B1.

## 6. Casino win — `BLUEPRINT`

- **Initiating event**: casino provider's win callback for a specific
  round.
- **Accounts**: debit `house_gaming`, credit `player_cash` and/or
  `player_bonus` — win lands in whichever account funded the
  corresponding stake (Blueprint's worked example: a bonus-funded stake's
  win lands in `player_bonus`, not `player_cash`, so wagering requirements
  apply to winnings from bonus play).
- **Idempotency key**: `(provider_id, provider_tx_id)` = the win's own
  provider reference (distinct from the triggering bet's reference).
- **Balance effect**: inverse of Flow 5's debit side.
- **Failure behavior**: a win callback for a round with no matching prior
  bet transaction is rejected and escalated (integrity alert — a
  provider protocol violation, not a normal failure path).
- **Bonus-funded portion — mirror legs required (ADR 0032 §2, Rule B2).**
  A win credited to `player_bonus` is a four-entry transaction, inverted
  from Flow 5's stake shape: Dr `house_gaming` `Y` · Cr `player_bonus`
  `Y` · Dr `promo_liability` `Y` · Cr `bonus_expense` `Y`. A win credited
  to `player_cash` is unchanged (two entries). **Wave-2 ledger-finance
  review correction (P1-1)**: omitted in an earlier draft.
- **Audit event**: `casino_win.posted`.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #13, B1.

## 7. Casino rollback — `BLUEPRINT`

- **Initiating event**: provider requests a rollback of a specific
  previously-posted bet (and/or its associated win), e.g. round voided by
  the provider.
- **Accounts**: a new `LedgerTransaction` with `reverses_transaction_id`
  pointing at the original bet (and, if a win was also posted for the same
  round, a second reversal referencing the win transaction) — exact
  inverse entries of whichever of Flow 5 / Flow 6 actually posted.
- **Idempotency key**: the rollback's own `provider_tx_id` (distinct from
  the bet's/win's own references) — a rollback is itself a retriable,
  idempotent operation.
- **Failure behavior — never-seen original**: rollback references a
  `provider_tx_id` the ledger never posted a bet for → tombstone written
  (CLAUDE.md rollback rule) so a late-arriving original bet callback for
  that reference is rejected rather than posted after the fact.
- **Bonus-funded portion — mirror legs required (ADR 0032 §2/§7).** A
  rollback of a bonus-funded bet or win inverts every leg the original
  posted, including its `promo_liability`/`bonus_expense` mirror pair —
  this falls out automatically because those legs live in the same
  transaction being reversed, so no special-case rollback code is needed.
  **Wave-2 ledger-finance review correction (P1-1)**: omitted in an
  earlier draft, which described only the two-entry cash-funded case.
- **Audit event**: `casino_bet.rolled_back` / `casino_win.rolled_back`.
- **Invariants engaged**: #1, #2, #3, #4, #5, #10, #13, #14, B1.

## 8. Sportsbook bet / fund lock — `BLUEPRINT`

- **Initiating event**: sportsbook widget/feed places a bet slip.
- **Accounts**: debit `player_cash` → credit `player_locked_cash`, and/or
  debit `player_bonus` → credit `player_locked_bonus` (all wallet-scoped) —
  the locked account is chosen by the **origin** of the funds, so origin
  is recorded by the account itself (migration `0048`; bare `player_locked`
  is not an admitted account type — `ledger-accounting-model.md` §6.5,
  invariant L1). Funds leave spendable balance immediately but are
  not yet house revenue, because the outcome is unknown (unlike a casino
  bet, which resolves near-instantly).
- **Idempotency key**: `(provider_id, provider_tx_id)` = (sportsbook
  provider id, bet-slip reference).
- **Balance effect**: `player_cash`/`player_bonus` −stake,
  `player_locked_cash`/`player_locked_bonus` +stake, per origin.
- **Failure behavior**: insufficient funds → rejected pre-posting, same
  pattern as Flow 5.
- **Audit event**: `sportsbook_bet.locked`.
- **Stake origin — the former `OPEN DECISION`, resolved at the schema
  level.** This flow previously credited a *single* `player_locked`
  account and so did not record whether the locked funds came from
  `player_cash` or `player_bonus`, leaving settlement (Flows 9/10/11)
  unable to know which account to return the stake to and breaking ADR
  0032's invariant B1 on the first bonus-funded lock. Migration `0048`
  implements ADR 0032 §10's recommended resolution — the
  `player_locked_cash`/`player_locked_bonus` split — and
  `ledger-accounting-model.md` §6.1/§6.3.2 extends B1's account set to
  `{player_bonus, player_locked_bonus}` accordingly. **Still open, and
  narrower than before:** the *bonus-funded* half of this flow remains
  gated — no `sportsbook_*` transaction type exists yet, gates G-2/G-3 are
  undecided, and `internal/ledger`'s HR-9 guard fail-closed rejects any
  posting against `player_locked_bonus` until ADR 0032's `bonus_expense`
  account type and the Rule B2 (extended) mirror generator both exist.
  Mixed cash+bonus stakes are hard-rejected at placement (HR-2). Full
  statement: `ledger-accounting-model.md` §6.5.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #12, #15.

## 9. Sportsbook settlement — `BLUEPRINT`

- **Initiating event**: sportsbook provider confirms an event outcome and
  settles a bet slip (win or loss).
- **Accounts (loss)**: debit the locked account the stake was locked into
  (`player_locked_cash` or `player_locked_bonus`, per origin), credit
  `house_gaming` — the locked stake becomes house revenue.
- **Accounts (win)**: four entries as two balanced pairs (not a single net
  entry, so `house_gaming`'s activity shows both the stake absorbed and
  the full payout made, matching casino's bet/win symmetry and keeping GGR
  reporting, `12-audit-reporting-architecture.md`, consistent across
  products): (1) debit `player_locked_cash`/`player_locked_bonus` (per
  origin) for the stake amount (`S`), credit
  `house_gaming` for `S` — the stake is absorbed exactly as in the loss
  case above; (2) debit `house_gaming` for the **full payout**
  (stake-plus-winnings, `S+W`, not the winnings portion alone), credit
  `player_cash`/`player_bonus` for `S+W`. Each pair balances on its own
  (`S`=`S`, `S+W`=`S+W`), so the transaction as a whole satisfies invariant
  #1. Note the trap: pairing a winnings-only debit against an `S+W` credit
  does **not** balance — do not debit `house_gaming` for winnings alone in
  this leg.
- **Idempotency key**: `(provider_id, provider_tx_id)` = settlement's own
  reference, distinct from the original lock's reference.
- **Failure behavior**: settlement references a bet slip with no matching
  locked entry in either locked account (already settled, or never locked)
  → rejected, integrity alert. The lookup must name **both**
  `player_locked_cash` and `player_locked_bonus`; naming one silently
  misses stakes of the other origin (invariant L1, layer 4).
- **Bonus-funded portion — mirror legs required (ADR 0032 §2, Rule B2).**
  Where the original stake locked from `player_bonus` — i.e. sits in
  `player_locked_bonus`, which is precisely what makes the origin knowable
  at settlement time (migration `0048`); this flow remains unimplementable
  for a bonus-funded stake until the gates in Flow 8 clear — the payout
  leg crediting `player_bonus` carries the same
  `promo_liability`/`bonus_expense` mirror pair as Flow 6's win. **Wave-2
  ledger-finance review correction (P1-1)**: omitted in an earlier draft.
- **Audit event**: `sportsbook_bet.settled`.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #12, #13, B1.

## 10. Sportsbook void — `BLUEPRINT`

- **Initiating event**: provider voids a bet slip (event cancelled, pushed,
  data error) before or after settlement.
- **Accounts (void before settlement)**: debit `player_locked_cash` →
  credit `player_cash`, and/or debit `player_locked_bonus` → credit
  `player_bonus` — stake returned to the balance it came from, never
  became house revenue. The return destination is read off the locked
  account's own type, which is the whole point of migration `0048`'s
  split: an undifferentiated locked account would leave this flow either
  leaking real cash to the player or wrongly re-restricting cash as bonus
  funds.
- **Accounts (void after settlement)**: full reversal chain — reverse the
  settlement transaction (Flow 9's entries inverted, `reverses_
  transaction_id` set), landing the stake back in `player_cash`/
  `player_bonus`.
- **Idempotency key**: void's own provider reference.
- **Audit event**: `sportsbook_bet.voided`.
- **Invariants engaged**: #1, #2, #3, #4, #5, #10, #12, #13, #14.

## 11. Sportsbook partial settlement (cash-out / partial cash-out) — `ARCHITECTURAL DECISION`

- The Blueprint does not describe partial settlement mechanics in detail;
  this flow is modeled by analogy to full settlement since cash-out is a
  standard sportsbook-provider capability the platform must be able to
  accept a callback for.
- **Accounts**: let `R` = the released (settled) portion of the locked
  stake and `P` = the provider-stated cash-out payout. The remainder of the
  original stake stays in the locked account it was locked into
  (`player_locked_cash`/`player_locked_bonus`) against the still-open
  portion of the bet. `P` may be **less than or greater than** `R` — a cash-out on a
  bet in a winning position pays out more than the released stake — so the
  entries must be written generally, not as "debit `R`, credit margin +
  payout" (which only balances when `P < R` and silently breaks invariant
  #1 otherwise):
  - always: debit `player_locked_cash`/`player_locked_bonus` (per origin)
    `R`; credit `player_cash`/`player_bonus` (the matching origin) `P`;
  - if `R > P` (house keeps a margin): credit `house_gaming` `R − P`;
  - if `P > R` (payout exceeds the released stake): debit `house_gaming`
    `P − R`.

  In both cases total debits = total credits in the wallet's asset. The
  `house_gaming` entry is omitted entirely when `R = P` (zero-amount
  entries are forbidden: `CHECK (amount > 0)`,
  `ledger-accounting-model.md` §1.3).
- **Idempotency key**: the partial-settlement event's own provider
  reference — **distinct per partial event**, since a single bet slip may
  cash out more than once (partial cash-out then final settlement) and
  each must be independently idempotent.
- **`OPEN DECISION`**: exact house-margin computation on a partial cash-out
  is provider-determined and not specified by the Blueprint; the ledger
  records whatever the provider's callback states as the settlement
  amount, it does not recompute or validate the odds/margin math itself
  (that validation, if any, belongs to the sportsbook specialist's
  provider-adapter layer, not the ledger).
- **Bonus-funded portion — mirror legs required (ADR 0032 §2, Rule B2).**
  Where the payout `P` (or its `player_bonus`-attributable share) credits
  `player_bonus`, that portion carries the same `promo_liability`/
  `bonus_expense` mirror pair as Flow 6/9, added to whichever of the
  `R`/`P`/margin legs above already apply — the transaction remains
  balanced per asset with the mirror pair as an additional, separate
  balanced group. **Wave-2 ledger-finance review correction (P1-1)**:
  omitted in an earlier draft.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #12, #13, B1.

## 12. Bonus grant — `BLUEPRINT`

- **Initiating event**: bonus-engine awards a bonus (campaign trigger,
  manual grant, deposit-match, etc.).
- **Accounts**: debit `promo_liability` (tenant house-level), credit
  `player_bonus` (wallet). Exactly one debit and one credit of equal
  amount, in one asset.
- **Idempotency key**: bonus-engine's own grant id as `idempotency_key`
  (no external provider involved for most grants; provider-sourced free-
  round grants use `(provider_id, provider_tx_id)` instead).
- **Balance effect**: `player_bonus` credit +amount (the player-facing
  liability increases); `promo_liability` debit +amount, i.e. its
  credit-positive `signed_balance` moves **down** by the same amount. The
  two accounts cannot both "increase" in the same sense: two same-direction
  entries would breach invariant #1. Granting a bonus does create a
  cost/obligation, but that obligation is carried by
  `player_bonus`, and `promo_liability` is its balancing counter-side, so
  the magnitude of `promo_liability` mirrors the outstanding `player_bonus`
  total with the opposite sign. Whether that counter-side should instead be
  a distinct `bonus_expense` account, leaving `promo_liability`
  credit-normal, was the `OPEN DECISION` recorded in
  `ledger-accounting-model.md` §2.
- **`RESOLVED` (architecture, Stage 4H-A) — `NOT IMPLEMENTED`**, by
  `docs/decisions/0032-bonus-accounting.md` §2/§3: the directions above are
  confirmed unchanged (Dr `promo_liability` / Cr `player_bonus`, two
  entries). `promo_liability` is the debit-side mirror of `player_bonus`
  (invariant B1), and **no `bonus_expense` is recognized at grant** — a
  granted bonus is a contingent liability, not a recognized cost. ADR 0032
  also adds the provider-funded variant (§6(b)) and the
  externally-fulfilled case (§6(c), **zero ledger entries**). Full
  reasoning and entry tables live in the ADR, not here.
- **Audit event**: `bonus.granted`.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8.

## 13. Bonus wagering — `BLUEPRINT`

- Not a distinct ledger transaction type of its own — wagering progress is
  a **derived read** over Flow 5/Flow 8 entries that debited
  `player_bonus` (per `06-wallet-ledger-architecture.md`: "splitting the
  stake across cash/bonus at the ledger level... is what makes wagering
  progress... fall out of the data instead of being reconstructed later").
  Listed here for completeness; there is no separate posting logic beyond
  Flows 5/8/6/9 themselves.
- **`OPEN QUESTION` (Stage 4H-B0-R5 Wave 3, `bonus-engine` review, not
  decided here) — the derived read is not yet specified to net out a
  reversed/voided debit.** "A derived read over entries that debited
  `player_bonus`" is precise for casino (Flow 5/6), where a bet resolves
  atomically and a debit that posted is final. It is **not** precise once
  a debit can post at **lock** time and later be undone by a separate
  compensating transaction before the underlying bet ever settles — which
  is exactly Flow 8's shape once bonus-funded sportsbook wagering unblocks
  (`ledger-accounting-model.md` §6.3.3.2 case C-void; ADR 0034 §14's
  `VOID_ON_SELF_EXCLUSION` reuses the identical void shape as a second,
  platform-triggered cause). The append-only ledger never deletes the
  original lock-time debit, so a naive `SUM(debit amounts to
  player_bonus)` read counts it **permanently**, even for a bet that was
  fully voided and returned 100% of the stake with zero player risk. A
  correct derived read must exclude (or net against) any debit whose
  originating transaction was subsequently reversed via
  `reverses_transaction_id` — without double-subtracting a legitimate WIN
  credit, which is a different kind of credit to the same account and must
  keep counting the original debit as progress. This is a **wagering-
  requirement-integrity gap**, not merely a display nuance: as drafted,
  voiding a bonus-funded locked stake (for any reason, including one
  entirely outside the player's control) leaves the player with wagering
  credit for a stake they never actually risked. Not resolved here —
  `ledger-finance` + `bonus-engine` own the exact query definition before
  any bonus-funded sportsbook stake goes live (ADR 0038 §9's blocking
  precondition already covers this class of gap; this is filed as one of
  its specifics, not a new blocker).
- **Invariants engaged**: none beyond those already listed under Flows
  5/6/8/9 (no new ledger writes).

## 14. Bonus conversion / payout — `BLUEPRINT`

- **Initiating event**: bonus-engine determines wagering requirement met
  (or a manual/campaign-driven conversion) for some or all of a player's
  `player_bonus` balance.
- **Accounts (always posted)**: debit `player_bonus`, credit `player_cash`
  — this leg is unconditional and independently testable: converting
  amount `X` always moves `X` from `player_bonus` to `player_cash`.
- **Accounts (formerly `OPEN DECISION`, now `RESOLVED`)**: whether the same transaction *also*
  moves `promo_liability` is not fixed here, and depends on the
  `promo_liability` framing left open in `ledger-accounting-model.md` §2.
  Note the direction constraint: Flow 12 **debits** `promo_liability` on
  grant and Flow 15 **credits** it on forfeiture, so any discharge at
  conversion time would have to be a **credit** — and a credit needs a
  balancing debit, which under the current ten-account list has nowhere to
  land (a realized-bonus-cost/P&L account does not exist yet; same gap as
  §2's `promo_liability` `OPEN DECISION`). Under framing 1 of that open
  decision, nothing moves here at all (the cost was already recognized at
  grant and the player keeps it).
- **`RESOLVED` (architecture, Stage 4H-A) — `NOT IMPLEMENTED`**, by
  `docs/decisions/0032-bonus-accounting.md` §4. Conversion is a **single
  atomic transfer within one wallet and one asset — Dr `player_bonus` `X`
  / Cr `player_cash` `X` — never "retire and re-credit as new cash"**, and
  the `promo_liability` mirror *is* discharged in the same transaction
  against the newly added `bonus_expense` account: four entries
  (Dr `player_bonus` · Cr `player_cash` · Dr `bonus_expense` ·
  Cr `promo_liability`), one `LedgerTransaction`, one database transaction.
  A bonus conversion is **never** an ADR 0021 `ConversionOperation` (same
  asset, no FX clearing account). Max-cashout capping happens before
  posting; an over-conversion is rejected, not clamped. Rationale, worked
  figures and the balance-check rule are in the ADR.
- **Idempotency key**: bonus-engine's own conversion event id.
- **Audit event**: `bonus.converted`.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8.

## 15. Bonus forfeiture — `ARCHITECTURAL DECISION` (implied by `player_bonus`/`promo_liability` existing, not separately enumerated by the Blueprint's flow list but required for the account pair in `ledger-accounting-model.md` §2 to ever reach zero)

- **Initiating event**: bonus expires, or wagering-requirement terms are
  violated (e.g. self-exclusion, RG intervention, T&C breach).
- **Accounts**: debit `player_bonus`, credit `promo_liability` — the exact
  inverse of Flow 12, unwinding an unconverted grant: the player-facing
  obligation (`player_bonus`) is removed and `promo_liability` returns
  toward zero by the same magnitude. (The forfeiture is *not* modeled as a
  `reverses_transaction_id` reversal of the grant transaction: a
  forfeiture is a new economic fact, is often partial, and may occur long
  after grant; reversal is reserved for correcting a transaction that
  should not have been posted.)
- **`RESOLVED` (architecture, Stage 4H-A) — `NOT IMPLEMENTED`**, by
  `docs/decisions/0032-bonus-accounting.md` §5/§7. The two entries above
  are confirmed, and the expense position the flow never stated is now
  fixed: **no `bonus_expense` is recognized or reversed on forfeiture** —
  the value never left `player_bonus` except to be extinguished, so a
  forfeited bonus costs the operator nothing. The forfeited amount is the
  *currently outstanding* bonus, never the original grant amount; grant
  attribution (FIFO/lot) is a Bonus Engine rule, not a ledger concern. A
  separate `bonus_reversal` type covers the genuinely different "this grant
  should never have been posted" case, with tombstone and double-reversal
  protection per the established casino/payments pattern.
- **Idempotency key**: bonus-engine's own forfeiture event id.
- **Audit event**: `bonus.forfeited`.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8.

## 16. Manual adjustment — `BLUEPRINT`

- **Initiating event**: staff-initiated correction via the back-office
  admin flow, gated by four-eyes approval above a configurable threshold
  (CLAUDE.md) and, per ADR 0017, an `OPEN DECISION` on whether step-up
  auth is additionally required (see `withdrawal-state-machine.md` §6,
  "Relationship to ADR 0017", for the analogous withdrawal case — the same
  question applies here).
- **Accounts**: debit/credit `manual_adjustment` paired with whichever
  account is being corrected (most commonly `player_cash`).
- **Idempotency key**: the admin action's own id (`idempotency_key`) —
  generated once when the four-eyes request is first approved, not
  re-generated on retry, so a double-submit of the same approved action is
  a no-op.
- **Mandatory fields**: `reason_code` is `NOT NULL` for this transaction
  type specifically (DB constraint — CHECK on `transaction_type =
  'manual_adjustment' → reason_code IS NOT NULL`).
- **Audit event**: `manual_adjustment.posted`, always including both
  approvers' identities (four-eyes) per CLAUDE.md.
- **Invariants engaged**: #1, #2, #3, #5, #6, #7, #8, #10, #14.
- **`OPEN DECISION`**: as written, `manual_adjustment` can pair with *any*
  other account, including tenant/system-level accounts (`psp_reserve`,
  `psp_clearing`, `house_gaming`) that Flows 1/3/17/18/19 otherwise reserve
  for provider-driven, reconciliation-anchored movements. A staff member
  with manual-adjustment permission could otherwise move funds into/out of
  `psp_reserve` or `psp_clearing` outside those flows, with no requirement
  that it correspond to an actual PSP statement line. Whether
  system/house-level accounts should require a stricter approval tier,
  a restricted reason-code set, or be excluded from `manual_adjustment`
  entirely (corrections there going through Flow 19/18-shaped entries
  instead) is a finance/risk policy decision for `ledger-finance` and
  `security`, not resolved in this freeze.

## 17. Provider settlement / payable — `BLUEPRINT`

- **Initiating event**: periodic (e.g. monthly) provider settlement run —
  computing GGR-share/fees owed to a casino or sportsbook provider.
- **Accounts**: debit `house_gaming` (or a dedicated expense recognition —
  `OPEN DECISION`, see below), credit `provider_payable`. On actual
  payment to the provider (an outbound payment, itself routed through
  `payment-orchestration.md`): debit `provider_payable`, credit
  `psp_clearing`/bank rail.
- **Idempotency key**: `(provider_id, provider_tx_id)` = (provider id,
  settlement-period reference, e.g. "2026-08-provider-x").
- **`OPEN DECISION`**: whether the debit side of provider-fee recognition
  is `house_gaming` itself (netting the fee against gaming revenue) or a
  distinct expense account not in the Blueprint's ten-account list. Not
  resolved here — flagged for `ledger-finance`/finance-team sign-off in
  Stage 3B, since it affects GGR vs. NGR reporting math
  (`12-audit-reporting-architecture.md`).
- **Audit event**: `provider_settlement.posted`.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #13.

## 18. PSP clearing — `BLUEPRINT`

- Not a standalone player-facing flow; `psp_clearing` is touched as one
  leg of Flow 1 (deposit) and Flow 3 (withdrawal). This entry documents
  its own internal movements: **PSP settlement batching**, where the PSP
  moves cleared funds to the platform's bank account in a batch distinct
  from individual deposit confirmations.
- **Accounts**: **credit** `psp_clearing` (the receivable from the PSP is
  drawn down as the PSP actually pays out), debit a platform bank-rail
  representation. Direction matters here and is easy to invert:
  `psp_clearing` is debit-normal (`ledger-accounting-model.md` §2 — an
  asset/receivable that a deposit *debits* into existence), so a batch
  settling to the bank must credit it, otherwise the account grows without
  bound instead of netting toward zero as that section requires.
- **`OPEN DECISION`** (bank-rail account): the bank-rail side of this entry
  is currently described as "out of ledger scope — the ledger's own view
  ends at `psp_clearing`; the actual bank account is external and
  reconciled against, not modeled as a `LedgerAccount`"
  (`reconciliation-model.md` §2.6). That cannot stand as written: a
  balanced transaction needs a real `LedgerAccount` on both sides, and the
  Blueprint's ten account types contain no bank/treasury account. Either
  (a) an eleventh `bank_treasury` account type is added (tenant-scoped,
  debit-normal) so this flow balances inside the ledger, or (b) PSP batch
  settlement is declared entirely out of ledger scope and posts no ledger
  transaction at all, with the bank leg living only in the finance/ERP
  reconciliation layer. Option (b) means `psp_clearing` never nets to
  zero in the ledger, which conflicts with its stated "transient" purpose.
  This is a finance/treasury-accounting decision, not resolved here.
- **Idempotency key**: PSP's own batch/settlement reference.
- **Audit event**: `psp_clearing.batch_settled`.
- **Invariants engaged**: #1, #3, #4, #5, #13.

## 19. PSP reserve movement — `BLUEPRINT`

- **Initiating event**: PSP increases or releases a rolling reserve
  (chargeback buffer) held against the tenant's processing volume.
- **Accounts (reserve increase)**: **debit `psp_reserve`, credit
  `psp_clearing`** — value moves from the receivable the PSP is about to
  pay us into the portion it is withholding; both accounts are debit-normal
  assets (`ledger-accounting-model.md` §2), so the reserve growing is a
  debit. **(release)**: inverse (debit `psp_clearing`, credit
  `psp_reserve`). Inverting these directions would show the reserve as a
  credit balance — i.e. as something the platform owes the PSP rather than
  something the PSP owes the platform — which is why they are stated
  explicitly here.
- **Idempotency key**: PSP's own reserve-adjustment reference.
- **Audit event**: `psp_reserve.adjusted`.
- **Invariants engaged**: #1, #3, #4, #5, #13.

## 20. Jackpot contribution — `BLUEPRINT`

- Covered as a split within Flow 5 (casino bet) when the game is
  jackpot-enabled — not a standalone transaction type. Jackpot **payout**
  (a player wins the progressive) is its own transaction: debit
  `jackpot_contribution`, credit `player_cash`, keyed by the provider's
  jackpot-win reference, subject to the same `OPEN DECISION` on liability
  vs. expense framing noted in `ledger-accounting-model.md` §2.
- **Bonus-funded stake, jackpot carve-out.** If the stake being carved
  from was bonus-funded, the carve-out debits `player_bonus` (not
  `player_cash`) for its share, and that debit carries the same
  `promo_liability`/`bonus_expense` mirror pair as the rest of Flow 5's
  bonus-funded stake — the carve-out is a split of the same stake amount
  Flow 5 already mirrors, not a second, separately-mirrored movement.
  Jackpot **payout** always credits `player_cash`, never `player_bonus`
  — a jackpot win is new value the player did not stake, so it carries no
  wagering requirement regardless of how the contributing stake was
  funded. **Wave-2 ledger-finance review correction (P1-1)**: omitted in
  an earlier draft.
- **Invariants engaged**: #1, #3, #4, #5, #6, #7, #8, #13, B1 (for the
  payout transaction specifically; B1 only where the contributing stake
  was bonus-funded).

## 21. Externally-fulfilled bonus — no posting — `ARCHITECTURAL DECISION`

Added by Wave-2 ledger-finance review (P1-1): ADR 0032 §6(c)'s
"externally-fulfilled" treatment had no corresponding flow in this
document, though the Consequences section of that ADR asked for one.

- **Initiating event**: an external (provider-native) bonus engine
  grants, tracks, wagers and settles a bonus entirely inside its own
  system (`docs/decisions/0033-provider-interoperability-and-external-
  bonus-engines.md`).
- **Accounts**: **none.** The ledger posts zero entries — no
  `promo_liability`, no `player_bonus`, no `bonus_expense`. Recording a
  liability the platform does not owe is exactly as wrong as omitting one
  it does (ADR 0032 §6(c)).
- **What is recorded instead**: the fact of the grant/settlement as a
  domain event plus an `audit.Record`, reconciled against the provider's
  own statement as a memo stream — counts and references, not balances
  (`reconciliation-model.md` §2.10).
- **The Flow 9 boundary case**: the moment external value genuinely lands
  in the platform wallet (the provider settles a free-bet win as a real
  payout), that landing is an ordinary Flow 9-shaped provider-settlement
  posting into `player_cash`, keyed on `(tenant_id, provider_id,
  provider_tx_id)` — cash from a provider, **not** a bonus grant, and it
  creates no bonus balance or wagering requirement on the platform side.
- **Idempotency key**: n/a — no `LedgerTransaction` is posted for the
  externally-fulfilled fact itself; the memo-stream record is keyed by
  the provider's own reference for reconciliation purposes only.
- **Audit event**: a Bonus/Gamification domain event (not yet named a
  ledger `transaction_type`, because none is posted), plus `audit.Record`.
- **Invariants engaged**: none (no posting). Reconciled under
  `reconciliation-model.md` §2.10's memo stream, not B1.

## Summary table

Notation: `X → Y` reads "**debit** X, **credit** Y" for the amount moved.
The table is a navigation aid only; the authoritative entry directions are
in each flow above.

| # | Flow | Primary accounts | Reversible via |
|---|---|---|---|
| 1 | Deposit | `psp_clearing` → `player_cash` | Flow 2 |
| 2 | Deposit reversal | `player_cash` → `psp_clearing` | — (itself a reversal) |
| 3 | Withdrawal | `player_cash` → `player_withdrawal_hold` → `psp_clearing`/custodian | Flow 4 |
| 4 | Withdrawal reversal | reverse of Flow 3's applicable step | — |
| 5 | Casino bet | `player_cash`/`player_bonus` → `house_gaming` (+`jackpot_contribution` split) (+ `bonus_expense` → `promo_liability` mirror pair on the bonus-funded portion — ADR 0032 §2) | Flow 7 |
| 6 | Casino win | `house_gaming` → `player_cash`/`player_bonus` (+ `promo_liability` → `bonus_expense` mirror pair on the bonus-funded portion — ADR 0032 §2) | Flow 7 |
| 7 | Casino rollback | reverse of Flow 5/6, mirror legs included | — |
| 8 | Sportsbook lock | `player_cash` → `player_locked_cash` / `player_bonus` → `player_locked_bonus` (locked account chosen by fund origin — migration `0048`) | Flow 10 |
| 9 | Sportsbook settlement | loss: `player_locked_cash`/`player_locked_bonus` → `house_gaming`. Win: `player_locked_cash`/`player_locked_bonus` → `house_gaming` (stake) **and** `house_gaming` → `player_cash`/`player_bonus` (stake+winnings) (+ mirror pair on the bonus-funded portion — ADR 0032 §2) | Flow 10 |
| 10 | Sportsbook void | reverse of Flow 8/9 | — |
| 11 | Sportsbook partial settlement | `player_locked_cash`/`player_locked_bonus` (released portion `R`, per origin) → `player_cash`/`player_bonus` (payout `P`, matching origin), with `house_gaming` taking the difference on whichever side balances (`R>P` credit, `P>R` debit) (+ mirror pair on the bonus-funded portion of `P` — ADR 0032 §2) | Flow 10 (on the settled portion) |
| 12 | Bonus grant | `promo_liability` → `player_bonus` (two entries; no `bonus_expense` at grant — ADR 0032 §3) | Flow 15 (forfeiture) / `bonus_reversal` (erroneous grant) |
| 13 | Bonus wagering | (derived read, no posting) | n/a |
| 14 | Bonus conversion | **Single atomic four-entry transaction** (ADR 0032 §4, `RESOLVED`): `player_bonus` → `player_cash` **and** `bonus_expense` → `promo_liability`. Never retire-and-recredit; never a `ConversionOperation` | — |
| 15 | Bonus forfeiture | `player_bonus` → `promo_liability` (two entries; **no** `bonus_expense` recognized or reversed — ADR 0032 §5) | — |
| 16 | Manual adjustment | `manual_adjustment` ↔ target account | itself (a second manual adjustment) |
| 17 | Provider settlement | `house_gaming`/expense → `provider_payable` → `psp_clearing` | — |
| 18 | PSP clearing batch | bank rail → `psp_clearing` (i.e. credit `psp_clearing`); bank-rail account itself an `OPEN DECISION` | — |
| 19 | PSP reserve movement | increase: `psp_reserve` → `psp_clearing` (debit reserve). Release: inverse | itself (inverse movement) |
| 20 | Jackpot contribution / payout | contribution (within Flow 5): `player_cash`/`player_bonus` → `jackpot_contribution`, carved out of the stake rather than out of `house_gaming` (+ mirror pair if bonus-funded — ADR 0032 §2). Payout: `jackpot_contribution` → `player_cash` (always cash, never bonus) | — |
| 21 | Externally-fulfilled bonus | **none — zero ledger entries, ever** (ADR 0032 §6(c)); memo stream only | n/a |

**Not in this list — cross-asset conversion.** No flow above moves value
between two assets; per ADR 0007/0021 that is only ever an explicit
`ConversionOperation`, which is designed but not implemented and produces
no flow in Stage 3B. Any future flow that appears to change a wallet's
asset is a defect until a `ConversionOperation` flow is added here with
its own per-asset balanced entries and its counter-account `OPEN DECISION`
(`ledger-accounting-model.md` §2) resolved.

## Cross-references

- Account definitions, invariants: `ledger-accounting-model.md`.
- Bonus/reward/promotional accounting (resolves Flows 12/14/15's bonus
  `OPEN DECISION`s; adds `bonus_expense`, invariant B1, `bonus_reversal`,
  the mirror-leg requirement now reflected in Flows 5/6/7/9/11/20/21,
  provider-funded and externally-fulfilled treatments, the lifecycle
  event → posting map, and the direct-cash-reward treatment):
  `docs/decisions/0032-bonus-accounting.md`.
- Withdrawal state machine detail: `withdrawal-state-machine.md`.
- Payment/PSP routing: `payment-orchestration.md`.
- Crypto-specific deposit/withdrawal detail: `crypto-custody-boundary.md`.
- Reconciliation per flow: `reconciliation-model.md`.

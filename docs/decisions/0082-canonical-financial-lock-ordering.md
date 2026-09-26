# 0082 — Canonical Financial Lock Ordering (Stage 9.1, closes LOCK-1)

## Status

*Status note 2026-09-26: "design only" below is historical. §4 was implemented in Stage 9.1 (LOCK-1 closed; task S91-07: `internal/ledger/lockorder.go` and call sites — Go only, no migration), and Amendment A4 was applied in Stage 10 W1 with sportsbook settlement (migration `0091_sportsbook_settlement`). See `docs/progress.md` "Stage 9.1" and "Stage 10 — W1", `docs/governance/task-registry.md` S91-07, and `docs/governance/stage-10-completion-report.md`.*

Accepted — **design only**. No Go source file and no migration is changed
by this ADR's own dispatch. Implementation is delegated to
`ledger-finance` (with `bonus-engine` for Workstream C), who must
implement exactly what §4 specifies and nothing else. Every architectural
invariant in §2 is binding on `qa` and `code-reviewer`.

This ADR supersedes the Stage 9 proposal "sort the entries inside
`ledger.Post`" (proven insufficient — see §1.4) and closes finding
**LOCK-1**. It does not change any financial invariant: nothing about
what gets posted changes, only the sequence in which rows are locked.

**Amended 2026-09-25 — Amendment A4 (Stage 10 W1, ADR 0088 §5/§5.5).**
Sportsbook settlement, void and rollback are brought under the canonical
order (§1.5 row, §2.1 exception list, §2.2 R1/R3, new §5.1c E-4 /
INV-LOCK-E4, Amendment A2's prospective binding, Amendment A3/INV-LOCK-E3
wording, §4.4/§5.3 pointers); see "Amendment A4" at the end of this file.
Partial settlement and cashout remain `NOT IMPLEMENTED` and bound by
Amendment A2.

**Amended 2026-09-26 — Amendment A6 (Stage 10.3, ADR 0092).** L0.1 is
also taken by casino `postRollback`, keyed on the original reference.
There is no new class and no new exception. See "Amendment A6" at the end
of this file; the §2.1 L0.1 row is to be read with it.

## Context

### The reported defect

Stage 9's review (`docs/governance/task-registry.md`, S9-05/S9-12) found a
real ABBA deadlock between `internal/casino`'s `postBet` and
`postWinDirectCash` on the same two `wallet_balance_projection` rows. The
proposed fix — sorting the entries inside `ledger.Post` — was
investigated by `ledger-finance` and **proven not to close the cycle**,
because `postBet` takes its `player_cash` projection lock *outside and
before* `ledger.Post` (`internal/casino/orchestrator.go:663`
`lockCashBalance`, called at line 886; `ledger.Post` at line 933). A sort
that happens only inside `Post` cannot establish a total order over locks
taken before `Post` is ever entered. LOCK-1 was therefore deferred rather
than superficially closed, with the note that it "needs a cross-cutting
`ledger.LockProjectionsInOrder`-style discipline spanning ledger, casino,
sportsbook, and withdrawal — an architect-level decision".

This ADR is that decision. The inventory below shows the prior naming
guess was directionally right but the *scope* was understated: the same
class of cycle exists in `internal/payments` and `internal/withdrawal`
with no `internal/casino` involvement at all, and a fourth cycle crosses
from the projection rows into `internal/bonus`'s advisory locks.

### How row locks are actually acquired in this codebase

Three distinct mechanisms, only one of which is visible as
`SELECT ... FOR UPDATE` in the Go source. Any lock-ordering rule that
only covers the first is not a rule at all.

1. **Explicit `SELECT ... FOR UPDATE`** — three near-identical private
   helpers on `wallet_balance_projection`
   (`casino.lockCashBalance:663`, `sportsbook.lockCashBalance:465`,
   `withdrawal.lockCashBalanceForUpdate:255`), plus row locks on
   non-projection tables (`ledger_transactions`, `withdrawal_requests`,
   `bonus_grants`, `bonus_held_dispositions`, `economic_operations`).
2. **Implicit, via migration `0023`'s `AFTER INSERT` trigger on
   `ledger_entries`.** `ledger_entries_update_projection()` runs
   `INSERT INTO wallet_balance_projection ... ON CONFLICT
   (ledger_account_id) DO UPDATE`, which takes a row-level **exclusive**
   lock on the conflicting projection row. Therefore **the order of the
   `EntryInput` slice handed to `ledger.Post` *is* a projection-lock
   acquisition order** — one lock per entry, in slice order
   (`internal/ledger/ledger.go:335-343`). This is the mechanism that
   produced LOCK-1, and it is invisible at every call site.
3. **Unique-index insertion waits**, which are lock waits for deadlock
   purposes: `ledger.GetOrCreateAccount`'s `INSERT ... ON CONFLICT DO
   NOTHING` on `ledger_accounts`, `db.IdempotentInsert`'s insert into
   `ledger_transactions (tenant_id, idempotency_key)`, and the trigger's
   own insert of a *new* projection row. A transaction blocks on another
   transaction's uncommitted index entry for the same key, and that wait
   participates in deadlock cycles exactly like a row lock.

Postgres detects and breaks these cycles (`deadlock_timeout`, default
1s), so the production symptom is not a hang: it is a P1-adjacent
`40P01` error aborting a bet, a win, a deposit, or a withdrawal
transition, under exactly the concurrent load where it is least
acceptable. Nothing is mis-posted (the ledger's invariants hold through a
deadlock abort — the whole transaction rolls back), so this is a
**liveness/availability** defect, not a correctness one. That is why it
is fixable without touching a single financial invariant.

## 1. Inventory — every money-touching lock site

Line numbers are from HEAD at the time of writing and are a navigation
aid, not an identifier; the implementer must locate by function name.

### 1.1 `internal/ledger`

| Site | Locks taken | Order | Outside `Post`? |
| --- | --- | --- | --- |
| `Post` (`ledger.go:247`) | `ledger_transactions` idempotency-key index insert (`db.IdempotentInsert`, line 308), then one **implicit** projection lock per entry (line 335-343) | key insert **first**, then projections in `entriesToPost` slice order: caller entries first, then generated mirror legs (§7.4.2 fixed order) | n/a — this is `Post` |
| `applyBonusMirror` / `resolveEntryAccounts` (`bonus_mirror.go:159,230`) | none (unlocked `SELECT` on `ledger_accounts`) | — | before the locks |
| `checkReversalFundingMatches` (`bonus_mirror.go:437`) | none (unlocked read) | — | — |
| `GetOrCreateAccount` (`ledger.go:381`) | `ledger_accounts` unique-index insert wait | caller-determined, per call | yes — always called before `Post` |
| `GetProjectedBalance` (`ledger.go:452`) | none — plain read | — | — |
| `RebuildProjectionRow` (`ledger.go:503`) | projection row lock via `ON CONFLICT DO UPDATE` | one account per call | yes; no production caller today (tests/DR only) |

**Finding L-a:** `Post` takes the `ledger_transactions` key lock *before*
the projection locks. Two concurrent deliveries of the same
`(tenant_id, idempotency_key)` therefore serialize on the key, but a
transaction that holds a key entry and then waits on projections can be
the second edge of a cycle. The canonical order fixes this direction
(§2.2 rule R5).

### 1.2 `internal/wallet`

No lock sites. `wallet.GetSummary` (`wallet.go:155`) is an unlocked read
of `wallet_balance_projection`; `wallet.GetByID` reads `wallets`. Nothing
to change.

### 1.3 `internal/casino`

| Site | Locks, in the order taken | Outside `Post`? |
| --- | --- | --- |
| `postBet` (`orchestrator.go:691`) | (1) advisory `casino_bet_delivery:<tenant>:<provider>:<tx>` (:741); (2) RG advisory on the person (`rg.lockPerson`, via `evaluateAndAuditEligibility`); (3) Risk cumulative advisory (`risk/evaluator.go:338`, if a cumulative rule is scoped); (4) **`player_cash` projection `FOR UPDATE`** (`lockCashBalance`, :663, called :886); (5) `ledger_transactions` key insert; (6) implicit projections `player_cash` → `house_gaming` (:933); (7) **after `Post`**: `bonus.AdvisoryLockGrant` via `RecordCashFundedWageringContribution` → `RecordWageringContribution:538` / `CheckAndCompleteGrant:614` (:974-991) | **yes, twice**: step 4 before `Post`, step 7 *after* it |
| `postWin` (`orchestrator.go:1026`) | `ledger_transactions` rows of the round `ORDER BY id FOR UPDATE` (:1059) then a per-origin branch | yes |
| `postWinDirectCash` (`bonus_settlement.go:325`) | implicit projections **`house_gaming` → `player_cash`** (:335) | no |
| `postWinLockedCash` (`bonus_settlement.go:367`) | implicit projections `house_gaming` → `player_cash` → `player_locked_cash` (:381) | no |
| `postWinLockedBonus`, non-terminal (`bonus_settlement.go:422`) | `bonus.AdvisoryLockGrant` (:436); implicit projections `house_gaming` → `player_bonus` → `player_locked_bonus` → then generated mirror legs (`promo_liability`, `bonus_expense`/`provider_payable`) (:467) | advisory yes; projections no |
| `postWinLockedBonus`, terminal (`bonus_settlement.go:512`) | as above, with `player_bonus_held` in place of `player_bonus`; then `ResolveTerminalGrantCredit` | same |
| `postRollback` (`orchestrator.go:1117`) | (1) `ledger_transactions` original row `FOR UPDATE` (:1135); (2) `bonus.AdvisoryLockGrant` (:1257, bonus-funded bets only); (3) implicit projections in `loadEntries` order — **already sorted ascending by `ledger_account_id`** (LOCK-2 fix, :1366-1371) **but generated mirror legs are appended after them**, breaking the ascending order for bonus-touching rollbacks | yes |
| `postRollbackHeldWin` (`bonus_settlement.go:582`) | `AdvisoryLockGrant` (:594) then `bonus_held_dispositions` row `FOR UPDATE` (:597, HR-25 order); implicit projections `player_bonus_held` → `house_gaming` → generated (:684) | yes |
| `postRollbackTombstone` (`orchestrator.go:1320`) | `ledger_transactions` key insert only — no entries, no projection locks | — |

### 1.4 The proven LOCK-1 cycle, stated exactly

`postBet` acquires `player_cash` (explicit, line 886) → `house_gaming`
(implicit, line 933). `postWinDirectCash` acquires `house_gaming`
(implicit) → `player_cash` (implicit), because Flow 6's entry slice is
`[house Dr, cash Cr]`. Same two rows, opposite order. Sorting inside
`Post` reorders step 2 of `postBet` and both steps of
`postWinDirectCash`, but `postBet`'s `player_cash` lock is already held
before `Post` is called, so the cycle survives the sort whenever
`house_gaming.id < player_cash.id`. **Confirmed: the prior proposal does
not work.**

### 1.5 `internal/sportsbook`

| Site | Locks, in order | Outside `Post`? |
| --- | --- | --- |
| `PlaceBet` (`orchestrator.go`, lock helper at :465, used at :235) | RG/Risk advisory locks; **`player_cash` projection `FOR UPDATE`**; `ledger_transactions` key insert; implicit projections `player_cash` → `player_locked_cash` (:288) | yes |
| settlement / void / rollback *(row amended 2026-09-25, Amendment A4 (1), ADR 0088 §5.1)* | L1 `sportsbook_bets` row `FOR UPDATE` → (rollback/void-after-settlement) L2 settlement `ledger_transactions` `FOR UPDATE` → L3 `LockProjectionsForPostings` over every posting → L4 `Post` (one or two) → E-4 (§5.1c) | yes (L1/L2 and the E-4 history insert/status UPDATE) |
| partial settlement / cashout | **NOT IMPLEMENTED** — no `ledger.Post` call site exists (ADR 0038 §8.2/§8.3). *(Original row, before Amendment A4, read "settlement / void / partial settlement / cashout — NOT IMPLEMENTED — no `ledger.Post` call site exists (ADR 0038 §5/§8/§10; the transaction types are deliberately absent from migration `0078`)"; settlement/void/rollback types were added by migration `0091`.)* | n/a |

### 1.6 `internal/payments`

*(Row amended 2026-09-26, Amendment A5, Stage 10.1 PAY-REV-1, ADR 0090: the
deposit-reversal row now takes an explicit L2 lock before Post; the
original row text — "implicit projections only, no explicit locks" — is
struck through, not deleted, below.)*

| Site | Locks, in order | Outside `Post`? |
| --- | --- | --- |
| `postDepositSuccess` (`orchestrator.go:722`) | `ledger_transactions` key insert; implicit projections **`psp_clearing` → `player_cash`** | no explicit locks anywhere in this package |
| deposit reversal (`orchestrator.go:989`) | ~~implicit projections **`player_cash` → `psp_clearing`**~~ **L2** `ledger_transactions` row `FOR UPDATE` on the ORIGINAL deposit (`receiveDepositReversalCallback`'s S2 step) → implicit projections **`player_cash` → `psp_clearing`** | ~~no~~ **yes — the new S2 lock, taken before `GetOrCreateAccounts`/L3/`Post`** |
| `postDepositReversalTombstone` (`orchestrator.go:1027`) | no entries — no projection locks | — |

**Finding LOCK-1b (new, this inventory):** a deposit and a reversal of a
*different* deposit for the same wallet+asset, running concurrently, take
`(psp_clearing, player_cash)` in exactly opposite orders. This is the
same defect as LOCK-1 with no casino code involved, and it was not
previously recorded.

### 1.7 `internal/withdrawal`

| Site | Locks, in order | Outside `Post`? |
| --- | --- | --- |
| `RequestWithdrawal` (:314, `Post` at :372) | `withdrawal_requests` insert; **`player_cash` projection `FOR UPDATE`** (`lockCashBalanceForUpdate`:255); implicit projections **`player_cash` → `player_withdrawal_hold`** | yes |
| `Reject` (:739, `Post` at :797) | `withdrawal_requests` row `FOR UPDATE` (`lockRequestForUpdate`:231); `withdrawal_approvals` insert; implicit projections **`player_withdrawal_hold` → `player_cash`** | yes (request row) |
| `Complete` (:1051, `Post` at :1073) | request row `FOR UPDATE`; implicit projections `player_withdrawal_hold` → `psp_clearing` | yes |
| `Fail` (:1139, `Post` at :1169) | request row `FOR UPDATE`; implicit projections `player_withdrawal_hold` → `player_cash` | yes |
| `Cancel` (:1228, `Post` at :1246) | request row `FOR UPDATE`; implicit projections `player_withdrawal_hold` → `player_cash` | yes |
| `Approve` / `MarkSubmitted` / `LockApprovedForSubmission` / `LockSubmittedForResolution` | request row `FOR UPDATE` only, no posting | — |

**Finding LOCK-1c (new, this inventory):** `RequestWithdrawal` takes
`(player_cash, player_withdrawal_hold)`; `Reject`, `Fail` and `Cancel`
take the same pair in the opposite order. The `withdrawal_requests` row
lock does **not** serialize them, because a second withdrawal request is
a *different* request row on the same wallet.

### 1.8 `internal/bonus` (and `internal/economicop`)

| Site | Locks, in order | Outside `Post`? |
| --- | --- | --- |
| `AdvisoryLockGrant` (`lifecycle.go:35`) | `pg_advisory_xact_lock('bonus_grant:<tenant>:<grant>')` | the package's first lock everywhere |
| `applyGrantActivation` (`lifecycle.go:362`, `Post` at :383) | grant advisory (via `ActivateGrant`:280); gate chain (RG/Risk advisory); `PostGateHook` → `economicop.ConsumeRootBudget` → `economic_operations` root row `FOR UPDATE` (`enforce.go:285`); implicit projections `player_bonus` → generated `promo_liability` | yes |
| `terminalWriteDown` (`lifecycle.go:656`, `Post` at :678) | grant advisory (via `TerminateGrant`:722); implicit projections `player_bonus` → generated | yes |
| `CheckAndCompleteGrant` (`lifecycle.go:613`) / `RecordWageringContribution` (:537) | grant advisory (:614 / :538); no posting | — |
| `applyGrantConversion` (`conversion.go:183`, `Post` at :224) | grant advisory (`ConvertGrant`:98); implicit projections `player_bonus` → **`player_cash`** → generated | yes |
| `resolveHeldDispositionAction` (`held_disposition_ops.go`, `Post` at :497) | grant advisory (:302); `bonus_held_dispositions` row `FOR UPDATE` (`held_disposition.go:144`); gate chain; implicit projections `player_bonus_held` → (`player_cash` for ACTION_ROUTE_TO_CASH) → generated | yes |
| `LockGrantForUpdate` (`grant.go:210`) | `bonus_grants` row `FOR UPDATE` | — |

**Finding LOCK-1d (new, this inventory — the cross-class cycle):**
`applyGrantConversion` holds the **grant advisory lock** and then takes
the **`player_cash` projection lock** (inside `Post`).
`casino.postBet` does the reverse: it holds `player_cash` (and
`house_gaming`) from before `Post` and *then*, after `Post` returns,
calls `bonus.RecordCashFundedWageringContribution`, which acquires the
**grant advisory lock** (`orchestrator.go:974-991`). Same player, same
grant, opposite order: a genuine ABBA across a row lock and an advisory
lock. Sorting projection rows alone does not touch this cycle.

### 1.9 Existing partial ordering doctrine (to be generalized, not replaced)

The codebase already contains three correct but *local* ordering rules.
This ADR subsumes them; none is contradicted.

- `rg/rg.go:126-133`: "every caller takes this [RG person advisory] lock
  BEFORE any wallet-balance lock... never after."
- `docs/architecture/ledger-accounting-model.md` §7.7.2.9 (**HR-25**):
  grant advisory lock, then the `bonus_held_dispositions` row lock.
- `docs/decisions/0040` / doc 34 §5.3 rule 4 (**DR-4HB1W2-01**): Risk's
  advisory lock always before the EOI root-row lock.

## 2. Decision

### 2.1 Canonical lock classes (the total order)

Every transaction that writes to the ledger acquires locks in strictly
non-decreasing class order. Within a class, the stated key breaks ties.
**No exceptions except E-1 (§5.1), E-2 (§5.1a), E-3 (Amendment A3) and
E-4 (§5.1c, Amendment A4), each of which is named, bounded and guarded.**
*(E-3 was added by Amendment A3 in Stage 9.2; this sentence was updated by
the Stage 9.2 `ledger-finance` review — see the addendum at the end of
this file — so that the rule and its own exception list cannot drift
apart. E-4 added 2026-09-25 by Amendment A4 (3), ADR 0088 §5.3.)*

| Class | What | Within-class order |
| --- | --- | --- |
| **L0** | Advisory locks | by sub-class, below |
| L0.1 | Casino bet-delivery advisory (`casino_bet_delivery:...`) | one per callback; no ordering question |
| L0.2 | **Player bonus-scope advisory** (`bonus_player:<tenant>:<player>`) — **new**, §3.3 | one per player |
| L0.3 | Grant advisory (`bonus_grant:<tenant>:<grant>`) | ascending `grant_id` if more than one |
| L0.4 | RG person advisory (`rg.lockPerson`) | one per person |
| L0.5 | Risk cumulative advisory (`risk/evaluator.go:338`) | one per scope key |
| L0.6 | **Sportsbook event-exposure advisory** (`sb_exposure:<tenant>:<event>`) — **new**, Amendment A2, `docs/decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md` §6.2.5 | one per bet; keyed on the event id so no ordering question arises |
| **L1** | Domain state rows (`withdrawal_requests`, `bonus_grants`, `bonus_held_dispositions`, `economic_operations`, `casino_launch_sessions`, sportsbook bets) | ascending `id` within a table; tables in the order listed |
| **L2** | `ledger_transactions` rows locked for read (`FOR UPDATE` on an existing transaction) | ascending `id` (`ORDER BY id FOR UPDATE`) |
| **L3** | **`wallet_balance_projection` rows — ascending `ledger_accounts.id`** | strictly ascending UUID byte order (Postgres `uuid` comparison; in Go `bytes.Compare(a[:], b[:])`) |
| **L4** | `ledger_transactions` idempotency-key index insert, `ledger_entries` inserts and the projection updates their trigger performs | n/a — by L3, every projection lock is already held, so these acquire nothing new |

`ledger_accounts` creation (`GetOrCreateAccount`) happens **before L3**
and is ordered by its own canonical key (§3.2).

### 2.2 The rules, stated so that two implementers produce identical code

- **R1 — one pre-lock step.** Every operation that posts to the ledger
  acquires **all** of its L3 locks in **one** step, through
  `ledger.LockProjectionsForPosting` (§3.1), before it takes any L4 lock
  and before it reads any balance for a decision.
  *(Amended 2026-09-25, Amendment A4 (2), ADR 0088 §5.2: an operation
  posting more than one transaction pre-locks the union once via
  `ledger.LockProjectionsForPostings`; the pre-locked inputs are exactly
  those posted.)*
- **R2 — ascending `ledger_account_id`, always.** The L3 order is
  ascending `ledger_accounts.id`. It is arbitrary but *stable*, requires
  no domain knowledge, needs no central registry of account types, and
  is identical in Go and in SQL. It is the only key in this system that
  every call site can compute without knowing anything about the others.
- **R3 — no partial pre-locking, ever.** A caller may not lock a
  *subset* of the accounts it will touch. A subset is only safe if it is
  a *prefix* of the ascending order, which no caller can know. This is
  the precise generalisation of the LOCK-1 bug: `postBet` pre-locked the
  subset `{player_cash}` of `{player_cash, house_gaming}`.
  *(Amended 2026-09-25, Amendment A4 (2), ADR 0088 §5.2: for a
  multi-posting operation R3 reads "the set of inputs pre-locked is
  exactly the set subsequently posted, with identical `Entries` and
  `BonusCost`" — pre-locking one posting's accounts and then posting a
  second is a subset pre-lock and violates R3.)*
- **R4 — `wallet_balance_projection` is locked only by `internal/ledger`.**
  No `FOR UPDATE` on that table may appear in any other package. The
  three `lockCashBalance*` helpers are deleted, not kept as wrappers.
  Reads for display (`wallet.GetSummary`, `ledger.GetProjectedBalance`,
  `internal/reconciliation`) stay unlocked and are unaffected.
- **R5 — projections before the idempotency-key insert.** Inside `Post`,
  the L3 step runs **before** `db.IdempotentInsert`. (Today it runs
  after; see Finding L-a.) Two deliveries of the same key necessarily
  target the same projection set, so they serialize at L3 and never at
  L4 while holding L3 locks. Consequence, accepted: an idempotent replay
  now takes its projection locks before discovering it is a no-op.
- **R6 — every projection row is materialised before it is locked.** A
  ledger account with no entries yet has no projection row, and
  `SELECT ... FOR UPDATE` on an absent row locks nothing; the trigger
  then *creates* it at L4, out of canonical order, and two transactions
  creating two new rows in opposite orders deadlock on the index. The L3
  step therefore ensures the row exists (zero totals) and locks it, per
  account, in ascending order. This writes a row of zeros to
  `wallet_balance_projection` from `internal/ledger` — see §2.3 for why
  that does not violate the "projection is trigger-maintained" rule.
- **R7 — entry order inside `Post` is unchanged.** §7.4.2's "byte-identical
  entry rows" property (caller entries, then generated legs) is
  preserved. Once R1+R6 hold, entry order no longer has any locking
  meaning, so there is nothing to gain by disturbing it.
- **R8 — advisory locks strictly precede row locks.** L0 before L1/L2/L3,
  in every path, with the single named exception E-1 (§5.1). This
  generalises `rg.go`'s existing rule to the whole platform.

### 2.3 Why R6 does not weaken any financial invariant

Migration `0023` states the projection is "maintained EXCLUSIVELY by this
trigger... Application code never writes to this table directly", and
CLAUDE.md forbids ever `UPDATE`-ing a balance. R6 is consistent with
both:

- It only ever **inserts a row with `debit_total = 0, credit_total = 0`**,
  copying `tenant_id`/`wallet_id`/`player_account_id`/`asset_code`/
  `account_type` from `ledger_accounts` — the identical source and
  identical column values the trigger itself derives (migration `0022`'s
  `ledger_entries_populate_from_account` trigger populates those same
  columns from `ledger_accounts`, and `ledger.RebuildProjectionRow`
  already uses exactly this `INSERT ... SELECT ... FROM ledger_accounts`
  shape). It never touches a total. No balance is ever `UPDATE`d.
- It runs **inside `internal/ledger`**, the package that owns the
  projection — not in a domain package. R4 keeps every other package
  out.
- It is in the **same transaction** as the posting, so a rolled-back
  posting also rolls back the zero row. The only residue is from an
  operation that pre-locks and then legitimately declines (e.g.
  `postBet` with insufficient funds, which commits an audit record): a
  zero-valued row for an account that was about to be used anyway.
- **Reconciliation is unaffected.** `RunLedgerVsProjection`'s
  `MismatchKindMissingProjection` case ("no projection row, but
  `ledger_entries` show activity") is a *bypassed trigger* detector; a
  zero row with non-zero rebuilt totals still trips the second case
  (`MismatchKindBalanceMismatch`). No detection is lost. An account with
  a zero row and no entries reconciles as `0 == 0`, which is correct.
- **RLS is unaffected.** Every posting path already runs tenant-scoped
  with `app.player_account_id` unset — it must, or migration `0023`'s
  `tenant_staff_scope` `WITH CHECK` would already reject the trigger's
  own write. The ensure-and-lock step runs under the same policy as the
  trigger it front-runs, so it introduces no new RLS failure mode.

`ledger-finance` owns financial invariants and may reject R6. If it
does, the only compliant alternative is to make `Post` insert its
`ledger_entries` rows in ascending `ledger_account_id` order (which
closes the new-row case too, at the cost of §7.4.2's current entry
order). R6 is preferred because it leaves the posting shape untouched.
Either way, the choice must be recorded as an amendment to this ADR, not
made silently.

### 2.4 Alternatives considered and rejected

- **Sort inside `ledger.Post` only** — rejected, proven insufficient
  (§1.4). Retained as a *component* of the chosen design, subordinate to
  R1/R3.
- **A caller-supplied account-ID list
  (`LockProjectionsInOrder(ctx, tx, ids...)`)** — rejected as the
  primary API. Callers cannot enumerate the mirror/recognition legs
  `Post` generates (HR-17 forbids them from even constructing those
  legs), so any caller-supplied list is a *subset*, which R3 forbids. It
  survives only as the unexported primitive `ledger` uses internally.
- **Application-level mutex** — forbidden by CLAUDE.md and by the
  directive, and wrong on the merits: the platform runs multiple
  instances, and an in-process mutex serializes nothing across pods.
- **`SERIALIZABLE` isolation for financial transactions** — rejected for
  this stage. It converts deadlocks into serialization failures, which
  still need a retry path that does not exist yet, and it changes the
  semantics of every existing `FOR UPDATE`-based check-then-post proof in
  this codebase. Recorded as a possible future ADR, not a substitute.
- **Sorting `ledger_accounts` by `account_type`** (e.g. "always
  house-level first") — rejected: it needs a central, totally-ordered
  registry of account types that every future type must be added to, and
  a forgotten type silently reopens the cycle. Ascending `id` cannot be
  forgotten.

## 3. Exact new and changed surface (`internal/ledger`)

### 3.1 New: the L3 pre-lock API

New file `internal/ledger/lockorder.go`.

```go
// ErrAccountNotLocked is returned by LockedProjections.Balance for an
// account that was not part of the locked set - never a zero Balance
// (same fail-closed posture as wallet.GetSummary's erroring default arm).
var ErrAccountNotLocked = errors.New("ledger: ledger account was not part of the locked projection set")

// LockedProjections is the result of LockProjectionsForPosting: the
// complete set of wallet_balance_projection rows a posting will touch,
// all locked FOR UPDATE in the canonical order (ADR 0082), with the
// balances read under those locks.
type LockedProjections struct {
    // AccountIDs is the set actually locked, in canonical (ascending)
    // order. Exported for tests and observability only.
    AccountIDs []uuid.UUID
    balances   map[uuid.UUID]Balance
}

// Balance returns ledgerAccountID's balance as read under the lock this
// LockedProjections holds. The returned Balance.Found distinguishes "the
// row was materialised by this call with zero totals" (false) from "a row
// with real history" (true), exactly as GetProjectedBalance does.
func (l LockedProjections) Balance(ledgerAccountID uuid.UUID) (Balance, error)

// LockProjectionsForPosting acquires, in the canonical order (ADR 0082
// §2.1 class L3: ascending ledger_accounts.id), every
// wallet_balance_projection row the posting described by `in` will touch
// - INCLUDING the Rule B2 (extended) mirror and recognition legs Post
// itself generates, which a caller may not know about and may not
// construct (HR-17). Idempotent: calling it and then calling Post with
// the same TransactionInput re-acquires locks this transaction already
// holds, which is a no-op.
//
// Every call site that must read a balance before deciding whether to
// post (an insufficient-funds check) calls this FIRST and reads the
// balance from the result - never its own SELECT ... FOR UPDATE, which
// ADR 0082 R4 forbids outside this package.
func LockProjectionsForPosting(ctx context.Context, tx pgx.Tx, in TransactionInput) (LockedProjections, error)
```

Unexported primitives in the same file:

```go
// canonicalAccountOrder deduplicates ids and returns them sorted
// ascending by UUID byte order - identical to Postgres's own uuid
// ordering, so the Go-side order and any ORDER BY ledger_account_id
// agree exactly.
func canonicalAccountOrder(ids []uuid.UUID) []uuid.UUID

// ensureAndLockProjectionsInOrder is ADR 0082 R6: for each account, in
// canonical order, (a) INSERT the projection row with zero totals if
// absent (ON CONFLICT DO NOTHING, columns copied from ledger_accounts),
// then (b) SELECT ... FOR UPDATE it. Two statements per account, executed
// strictly in ascending account order. A single batched INSERT followed
// by a single batched SELECT ... FOR UPDATE is NOT equivalent and must
// not be substituted: the batched INSERT locks only the rows it creates,
// so a transaction can acquire a high-id new row in phase (a) and then a
// low-id existing row in phase (b) - descending, which is the very cycle
// this function exists to prevent. May be issued as one pgx.Batch (the
// server executes batch statements sequentially, in order, in the same
// transaction) to keep this to one round trip.
func ensureAndLockProjectionsInOrder(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, accountIDs []uuid.UUID) (map[uuid.UUID]Balance, error)

// prepareEntries is today's Post preamble, extracted verbatim so that
// Post and LockProjectionsForPosting compute the SAME final entry set
// from the same input: input validation, applyBonusMirror, and the
// §7.4.2 fixed-order concatenation. No behaviour change.
func prepareEntries(ctx context.Context, tx pgx.Tx, in TransactionInput) ([]EntryInput, error)
```

### 3.2 New: ordered account resolution

Closes the `ledger_accounts` unique-index wait (mechanism 3, §1.8's
`withdrawal` case: `RequestWithdrawal` resolves `cash` then `hold`,
`Reject`/`Fail`/`Cancel` resolve `hold` then `cash`).

```go
// AccountSpec names one ledger account to resolve. WalletID is nil for a
// house-level account.
type AccountSpec struct {
    WalletID    *uuid.UUID
    AccountType AccountType
    AssetCode   string
}

// GetOrCreateAccounts resolves every spec, creating any that do not exist
// yet, and returns their ids IN THE SAME ORDER AS specs. Creation is
// performed in canonical spec order - ascending by
// (wallet key, account_type, asset_code), where the wallet key is the
// empty string for a house-level account - NOT in the caller's argument
// order, so two call sites that resolve the same accounts in different
// argument orders can never block on each other's uncommitted
// ledger_accounts index entries in opposite orders.
//
// ledger_accounts.id is random, so it cannot order creation; the
// (wallet, type, asset) tuple is the only key knowable before the row
// exists, and it is exactly the tuple the partial unique indexes use.
func GetOrCreateAccounts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, specs ...AccountSpec) ([]uuid.UUID, error)
```

`GetOrCreateAccount` (singular) is kept, unchanged, for genuine
single-account call sites.

### 3.3 New: the player bonus-scope advisory lock (closes LOCK-1d)

In `internal/bonus/lifecycle.go`:

```go
// AdvisoryLockPlayerBonusScope takes ADR 0082 class L0.2: the
// (tenant_id, player_account_id) advisory lock that every operation
// touching BOTH this player's bonus state AND the ledger must hold
// before any grant-level advisory lock and before any
// wallet_balance_projection lock.
//
// It exists because casino.postBet takes its projection locks and THEN,
// after posting, acquires a grant advisory lock for the wagering
// contribution, while bonus.ConvertGrant holds a grant advisory lock and
// THEN takes the player_cash projection lock inside ledger.Post - an ABBA
// across a row lock and an advisory lock (finding LOCK-1d). Serializing
// both behind one player-scoped advisory lock, taken first, removes the
// cycle without moving the contribution out of the bet's own transaction
// (HR-10 requires it stay there).
//
// hashtextextended for the full 64-bit key space, tenant-scoped, exactly
// like AdvisoryLockGrant.
func AdvisoryLockPlayerBonusScope(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) error
```

`AdvisoryLockGrant` keeps its signature and gains this precondition
internally: it resolves `player_account_id` from `bonus_grants` (an
immutable column, so an unlocked read is sound), acquires L0.2, then
acquires L0.3 as today. Every existing `AdvisoryLockGrant` caller is
therefore correct with no change.

### 3.4 Changed: `ledger.Post`'s contract

`Post`'s signature does not change. Its documented contract gains:

1. It calls `prepareEntries` (as today, via `applyBonusMirror`), then
   `ensureAndLockProjectionsInOrder` over the **complete** final account
   set, **before** `db.IdempotentInsert` (R5). Re-locking rows a caller
   already locked via `LockProjectionsForPosting` is a no-op.
2. Callers are forbidden from taking any `wallet_balance_projection` lock
   themselves (R4), and from taking any L0/L1/L2 lock *after* calling
   `LockProjectionsForPosting` or `Post` (R8) — the one exception being
   E-1 (§5.1).
3. Entry insertion order is unchanged (R7).

## 4. Per-call-site change list

Fourteen production call sites; nine need no code change because `Post`'s
internal pre-lock covers them.

### 4.1 `internal/ledger/ledger.go`, `internal/ledger/lockorder.go` (new)

Implement §3.1, §3.2, §3.4. Extract `prepareEntries`; move the L3 step
ahead of `db.IdempotentInsert`. Update the `Post` doc comment and
`RebuildProjectionRow`'s doc comment (the latter: "callers iterating
multiple accounts must iterate in ascending `ledger_account_id` order —
ADR 0082 L3").

### 4.2 `internal/casino/orchestrator.go`

- **`lockCashBalance` (:657-675): DELETE.** No wrapper, no deprecation
  shim (R4).
- **`postBet`:** build the final `ledger.TransactionInput` (both entries,
  known amounts) *before* the balance check. Replace the
  `lockCashBalance` call at :886 with
  `locked, err := ledger.LockProjectionsForPosting(ctx, tx, betInput)`,
  then `bal, err := locked.Balance(cashAccountID)` and
  `available := bal.Signed()`. Pass **the same `betInput`** to
  `ledger.Post` at :933 — not a rebuilt one. Keep the call in exactly its
  current position (after RG and Risk, before `BindProviderRound`), so
  R8 and the existing "no ledger entry touched yet at decline time"
  property both still hold.
- **`postBet`, LOCK-1d:** immediately after the L0.1 delivery advisory
  lock (:741) and before RG/Risk, call
  `bonus.AdvisoryLockPlayerBonusScope(ctx, tx, tenantID, session.PlayerAccountID)`.
  (It needs the session, so place it directly after the session is
  resolved and validated, still before `evaluateAndAuditEligibility`.)
  The post-`Post` wagering-contribution block at :974-991 is then
  reentrant under a lock this transaction already holds. No other change
  there.
- **Account resolution (:872-879):** use
  `ledger.GetOrCreateAccounts(ctx, tx, tenantID, cashSpec, houseSpec)`.
- **`postWin` (:1059):** no change. Its `ORDER BY id FOR UPDATE` on
  `ledger_transactions` is already canonical (L2).
- **`postRollback`:** no change required; `Post`'s internal pre-lock now
  covers the case `loadEntries`' `ORDER BY` could not (the generated
  mirror legs appended after the caller's sorted entries). Keep
  `loadEntries`' `ORDER BY e.ledger_account_id` — it is now redundant for
  locking but remains correct and documents intent; update its comment to
  cite this ADR instead of claiming to be the lock-ordering mechanism.
- **`postRollbackTombstone`:** no change (no entries).

### 4.3 `internal/casino/bonus_settlement.go`

- `postWinDirectCash`, `postWinLockedCash`, `postWinLockedBonus` (both
  branches), `postRollbackHeldWin`: **no change required** — every one of
  them posts through `Post`, takes no projection lock of its own, and is
  fully ordered by `Post`'s internal L3 step.
- Account resolution in each: switch to `ledger.GetOrCreateAccounts`
  (§3.2). Mechanical.

### 4.4 `internal/sportsbook/orchestrator.go`

- **`lockCashBalance` (:462-477): DELETE.**
- **`PlaceBet`:** same transformation as `postBet` — build the final
  `TransactionInput`, call `ledger.LockProjectionsForPosting`, read the
  balance from the result, pass the same input to `Post`. Keep the call
  in its current position (:235, after RG/Risk, before
  `computePotentialReturn`).
- Account resolution: `ledger.GetOrCreateAccounts`.
- **Settlement / void / partial settlement / cashout:** nothing to change
  (NOT IMPLEMENTED). This ADR binds them prospectively: when built, they
  must use `LockProjectionsForPosting` and must not introduce a
  projection `FOR UPDATE` of their own.
  *(Amended 2026-09-25, Amendment A4 (7): settlement, void and rollback
  are no longer NOT IMPLEMENTED — see Amendment A4 and ADR 0088 §5 (they
  use `LockProjectionsForPostings` and take no projection `FOR UPDATE` of
  their own). Partial settlement and cashout remain NOT IMPLEMENTED and
  bound as stated.)*

### 4.5 `internal/payments/orchestrator.go`

- **No code change required (for LOCK-1b).** LOCK-1b is closed entirely by
  `Post`'s internal L3 step, because neither the deposit nor the reversal
  path takes any lock outside `Post`.
- Account resolution in `postDepositSuccess` and the reversal path:
  switch to `ledger.GetOrCreateAccounts` (both resolve `player_cash` and
  `psp_clearing`, currently in the same order — the change is
  defensive/uniform, not a fix).
- **Pointer (Amendment A5, 2026-09-26, Stage 10.1 PAY-REV-1, ADR 0090):**
  unrelated to LOCK-1b, `receiveDepositReversalCallback` was subsequently
  given its own explicit **L2** lock (a plain instance, no new exception)
  on the original deposit's `ledger_transactions` row, taken before
  account resolution/L3/`Post` — see §1.6's amended row and ADR 0090 for
  the full S0–S7 sequence and rationale (a check-then-insert race, not a
  lock-ordering defect this ADR's R1–R8 previously covered). R1–R8 are
  unchanged; no new class or exception was added.

### 4.6 `internal/withdrawal/withdrawal.go`

- **`lockCashBalanceForUpdate` (:255-269): DELETE.**
- **`RequestWithdrawal`:** build the final `TransactionInput` first; call
  `ledger.LockProjectionsForPosting`; read `player_cash` from the result
  for the `ErrInsufficientFunds` check; pass the same input to `Post`.
  Position unchanged.
- **`Reject`, `Complete`, `Fail`, `Cancel`:** **no change required** —
  LOCK-1c is closed by `Post`'s internal L3 step. Their
  `lockRequestForUpdate` call is L1 and already precedes L3.
- Account resolution in all five: `ledger.GetOrCreateAccounts`. This is
  the site where the ordered-resolution change is load-bearing, not
  cosmetic (§1.7 resolves `cash, hold` in `RequestWithdrawal` and
  `hold, cash` in the other four).

### 4.7 `internal/bonus`

- Implement `AdvisoryLockPlayerBonusScope` and fold it into
  `AdvisoryLockGrant` (§3.3).
- `applyGrantActivation`, `terminalWriteDown`, `applyGrantConversion`,
  `resolveHeldDispositionAction`: **no change required** for L3 — all
  four post through `Post` and take no projection lock. Their L0/L1
  ordering (grant advisory → `bonus_grants` / `bonus_held_dispositions` /
  `economic_operations` row → post) already matches §2.1.
- `applyGrantConversion`: account resolution → `ledger.GetOrCreateAccounts`.

### 4.8 `internal/reconciliation`

No change. `RunLedgerVsProjection` is an unlocked read and must stay one.
If a future repair mode is added, it must iterate accounts in ascending
`ledger_account_id` order and use `ledger`'s own primitives.

### 4.9 Suggested wave split (sequencing advice, not a requirement)

- **Workstream A (closes LOCK-1, LOCK-1b, LOCK-1c):** §4.1 + the
  projection parts of §4.2/§4.4/§4.6. Self-contained and independently
  correct.
- **Workstream B (closes the `ledger_accounts` creation-order residual):**
  `GetOrCreateAccounts` + the mechanical call-site swaps. Independently
  correct.
- **Workstream C (closes LOCK-1d):** §3.3 + `postBet`'s L0.2 call.
  Independently correct; needs `bonus-engine` review.

None of the three depends on either of the others being done first.
Partially landing them leaves the remaining cycles exactly as they are
today — no worse.

## 5. What this does NOT fully close, and why

Stated explicitly because the directive requires honesty over a clean
claim.

### 5.1 Exception E-1 — `ledger_transactions` row lock taken before a grant advisory lock

`casino.postRollback` locks the original `ledger_transactions` row
(`FOR UPDATE`, :1135) and only *then* resolves the grant and takes the
grant advisory lock (:1257); `postWinLockedBonus` reaches the grant
advisory lock (:436) after `postWin`'s L2 lock at :1059. That is L2
before L0 — an inversion of R8.

**Why it is not reordered this stage:** the advisory key *is derived
from the row being locked*. The grant id is looked up from
`grant_ledger_attributions` keyed on the original transaction id, which
is itself produced by the locked read. Reordering requires an unlocked
pre-read, an advisory lock on its result, then a re-read and re-
validation under the row lock — a restructuring of two of the most
safety-critical settlement paths in the platform, for a cycle that does
not currently exist.

**Why it is safe today, and the invariant that keeps it safe:** a cycle
needs a counterpart that holds a grant advisory lock and then waits on a
`ledger_transactions` row lock. No such path exists: every
`internal/bonus` path reads `ledger_transactions`/`ledger_entries`
**unlocked** (`RemainingBonusBalance`, `ComputeAOE`,
`checkReversalFundingMatches`). This becomes a **standing architectural
invariant**, verifiable by grep and owned by `code-reviewer` and `qa`:

> **INV-LOCK-E1:** no code path that holds a `bonus_grant:` advisory lock
> may take a `FOR UPDATE` on `ledger_transactions` or `ledger_entries`.

If that invariant ever needs to be broken, E-1 must be resolved first,
and this ADR amended.

### 5.1a Exception E-2 — `casino_provider_rounds` (L1) row lock taken after the L3 pre-lock

*Added during implementation, after `code-reviewer` found it unnamed.
Numbered `5.1a` rather than `5.2` so that existing §5.2 references (in
`internal/bonus/lifecycle.go` and elsewhere) keep pointing at the LOCK-1d
residual they were written against.*

`casino.postBet` calls `BindProviderRound` (`internal/casino/rounds.go`)
**after** `ledger.LockProjectionsForPosting`, not before it
(`internal/casino/orchestrator.go`: pre-lock, insufficient-funds
decision, then `BindProviderRound`, then `ledger.Post`).
`BindProviderRound` is a single
`INSERT ... ON CONFLICT (tenant_id, provider_id, provider_round_id) DO
UPDATE ... WHERE ...`, which takes a row-level exclusive lock on the
conflicting `casino_provider_rounds` row (and, on first observation, a
unique-index insertion wait). `casino_provider_rounds` is a domain state
row — **class L1** — so this is L1 acquired after L3: an inversion of
§2.1's order, exactly as E-1 is an inversion of R8.

**Why it is not reordered this stage:** moving `BindProviderRound` before
the L3 pre-lock would bind the round id for deliveries that are then
*declined*. The current position is itself a `qa` review finding
(recorded in `BindProviderRound`'s own call-site comment): all three
decline paths — RG eligibility, Risk, insufficient funds — return
`(OutcomeDeclined, nil)` and commit, so an earlier bind would let a
declined delivery durably claim a round id and pre-empt the legitimate
bet that later retries it. Reordering therefore means either (a) making
the bind conditional and re-entrant across a decline, or (b) splitting
the bind into a reserve/confirm pair — a restructuring of the bet path
for a cycle that does not currently exist. Same shape of trade-off as
E-1, and the same decision: name it, guard it, do not restructure the bet
path under schedule pressure.

**Why it is safe today, and the invariant that keeps it safe:** an ABBA
cycle needs a counterpart transaction that holds a
`casino_provider_rounds` row lock and then waits on a
`wallet_balance_projection` lock. No such counterpart exists, because
`postBet` is the **sole writer** of `casino_provider_rounds` in the whole
tree (verifiable by grep: `BindProviderRound` is the only `INSERT`/
`UPDATE` against that table, and `postBet` is its only caller; the other
two accessors, `LookupProviderRound` and
`LookupProviderRoundIDsBySession`, are unlocked reads). Two concurrent
`postBet` transactions acquire L3 then L1 in the *same* order as each
other, which is a consistent order and therefore deadlock-free even
though it is not the canonical one. This becomes a standing
architectural invariant alongside INV-LOCK-E1:

> **INV-LOCK-E2:** `casino_provider_rounds` has exactly one writer
> (`casino.postBet`, via `BindProviderRound`). Any second writer — or any
> path that locks a `casino_provider_rounds` row and then posts to the
> ledger — must resolve E-2 first.

**What would need to change to close it properly:** move the round
binding to before the L3 pre-lock while preserving the "a declined
delivery never claims a round id" property — i.e. option (a) or (b)
above — after which `BindProviderRound` sits at L1, ahead of L2/L3, and
this exception is deleted. Until then, a `code-reviewer`-owned grep for
new writers of `casino_provider_rounds` is the control.

### 5.1c Exception E-4 — sportsbook settlement history insert and status UPDATE after L4 (Amendment A4, 2026-09-25)

*(Added by Amendment A4 (4); text of ADR 0088 §5.3. There is no §5.1b —
E-3 is recorded in Amendment A3, not in §5.)*

History rows in `sportsbook_bet_settlements` cannot be inserted before
`Post`: `ledger_transaction_id` is `NOT NULL` with an FK, `Post` mints the
id internally, and the table is append-only (no reserve-then-update). So
the history insert and the `sportsbook_bets.status` UPDATE follow L4, as
`PlaceBet`'s `insertBet` does (E-3 precedent).

**Safety argument.** The status UPDATE is **not HOT**: `status` is in the
predicate of `idx_sportsbook_bets_open_exposure` (migration 0088:124), so
the UPDATE writes new entries into every index on `sportsbook_bets`,
including `UNIQUE (tenant_id, player_account_id, idempotency_key)`, and
another transaction's unique check on that key may **wait on this
in-progress updater**. That wait cannot close a cycle, for one reason:
**the E-4 writes begin only after the L3 step already holds every L3
lock this transaction will ever take**. From that point onward this
transaction acquires nothing a `PlaceBet` (or any L3-holding transaction)
could hold: the history unique-index entries and the FK `KEY SHARE` are
keyed by this bet (every other writer of this bet is queued behind the
same L1 lock before it reaches L3), the bet row is already locked by this
transaction, and `UNIQUE (ledger_transaction_id)` keys a row this
transaction just created. So anything waiting on the E-4 writes waits on
a transaction that will finish without waiting on it.

> **INV-LOCK-E4:** every INSERT into `sportsbook_bet_settlements` and every
> UPDATE of `sportsbook_bets.status` happens in a transaction that (i)
> took the bet's `FOR UPDATE` row lock at L1, before any L2/L3 lock, and
> (ii) performs the INSERT/UPDATE only **after** all of its L3 locks are
> held (after `LockProjectionsForPostings`). Sole writers: the named W1
> settlement functions in `internal/sportsbook`, verified by the static
> test `sportsbook_settlement_sole_writer_test.go` (ADR 0088 §14; owned by
> `code-reviewer` and `qa`).

**`sportsbook_bets` writers.** They are `insertBet` (E-3, from `PlaceBet`)
and the W1 settlement functions, which take L1 first (INV-LOCK-E4).
INV-LOCK-E3's forward condition ("a second writer … must take its row lock
at L1 before `LockProjectionsForPosting`") is met. `PlaceBet` inserts only
**new** rows and never locks an existing bet row; E-3 itself is not closed
by W1 (ADR 0088 §15, OI-4).

### 5.2 LOCK-1d residual — a grant that becomes active mid-bet

`postBet`'s L0.2 player-scope lock (§3.3) closes the cycle for every
grant that exists when the bet starts. Under `READ COMMITTED`, a grant
*activated by another transaction* after `postBet` took L0.2 would be
seen by the post-`Post` contribution read. That transaction cannot be
the other half of a cycle — it would have to hold L0.2 for the same
player, which `postBet` already holds — so the residual is closed by
L0.2 itself. Recorded here because the reasoning is non-obvious and a
future change to L0.2's scope (e.g. narrowing it back to grant level)
would silently reopen LOCK-1d. **Not deferred; documented.**

### 5.3 `internal/sportsbook` settlement, void, partial settlement and cashout

**NOT IMPLEMENTED** — cannot be brought under the canonical order this
stage because the code does not exist. §4.4 binds them prospectively.
This is a scope fact, not a deferral of known-broken code.

*(Amended 2026-09-25, Amendment A4 (7): settlement, void and rollback are
now brought under the canonical order by Amendment A4 (below) and ADR 0088
§5, with the named exception E-4 (§5.1c). Partial settlement and cashout
remain NOT IMPLEMENTED under this section.)*

### 5.4 What is explicitly out of scope

`SERIALIZABLE` isolation and a transaction-retry framework (§2.4); any
change to what is posted, to idempotency keys, to four-eyes controls, or
to the Rule B2 mirror generator's output; deadlock *retry* on `40P01`
(the fix here is prevention, and a retry layer for financial
transactions is its own ADR with its own idempotency analysis).

## 6. Concurrency test plan (for `qa` and `ledger-finance` to implement)

Scenarios by name and intent. The implementing specialist writes them;
this ADR does not. Every one must be **deterministic** — driven by a
blocker transaction holding a known lock plus `pg_stat_activity` lock-
wait polling, the pattern `internal/casino/stage9_concurrency_integration_test.go`
and `internal/jurisdiction/tenant_licence_admin_integration_test.go`
already use — never a bare goroutine race that passes by luck.

**A. Deadlock-freedom, one test per previously-proven cycle.** Each must
be written so that it **fails on today's HEAD** (stash the fix, re-run,
observe `40P01`) and passes after. A test that cannot be shown to fail
before the fix proves nothing.

1. `TestLockOrder_ConcurrentBetAndWinOnSameWallet_NoDeadlock` — LOCK-1:
   `postBet` and `postWinDirectCash` for the same wallet+asset, forced to
   interleave between their first and second projection locks.
2. `TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock` —
   LOCK-1b: `postDepositSuccess` for one intent against the reversal of a
   different, already-posted deposit on the same wallet.
3. `TestLockOrder_ConcurrentWithdrawalRequestAndRejection_NoDeadlock` —
   LOCK-1c: `RequestWithdrawal` for a new request against `Reject` (and,
   as sub-cases, `Fail` and `Cancel`) of a different request on the same
   wallet.
4. `TestLockOrder_ConcurrentBetAndGrantConversion_NoDeadlock` — LOCK-1d:
   `postBet` for a player holding an active wagering grant against
   `ConvertGrant` for that same grant.
5. `TestLockOrder_ConcurrentFirstPostingsCreateSameAccounts_NoDeadlock` —
   the `ledger_accounts`/new-projection-row case: two transactions whose
   *first ever* postings touch the same two brand-new accounts, driven
   from opposite call sites (e.g. `RequestWithdrawal` vs `Reject`).
6. `TestLockOrder_ConcurrentBonusMirrorPostings_NoDeadlock` — two
   bonus-touching postings whose caller entries are disjoint but whose
   **generated** `promo_liability`/`bonus_expense` legs collide (the case
   `loadEntries`' `ORDER BY` provably could not cover).

**B. The ordering property itself, not just the absence of a symptom.**

7. `TestLockOrder_PreLockCoversEveryEntryIncludingGeneratedLegs` — for a
   bonus posting, assert `LockedProjections.AccountIDs` is exactly the
   distinct account set of `Post`'s final entry list (caller + generated),
   and is strictly ascending.
8. `TestLockOrder_ProjectionRowIsMaterialisedAndLockedWhenAbsent` — R6:
   an account with no prior entries gets a zero-totals row created and
   locked by the pre-lock step; a concurrent reader blocks.
9. `TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage` — a **static
   source test** (walk `internal/`, flag any string literal naming
   `wallet_balance_projection` together with `FOR UPDATE` outside
   `internal/ledger`). This is the regression guard for R4 — the one
   test that prevents this ADR from decaying.
10. `TestLockOrder_NoLedgerTransactionForUpdateUnderGrantAdvisoryLock` —
    the INV-LOCK-E1 grep guard (§5.1).

**C. Nothing else moved.** These must pass unchanged, not be rewritten:

11. `TestLedger_BalancedUnderConcurrentLoad` — N concurrent mixed
    operations (bets, wins, deposits, withdrawals, bonus grants) against
    one tenant; afterwards `SUM(debits) == SUM(credits)` per asset, every
    `wallet_balance_projection` row equals `ledger.RebuildBalance`, and
    `reconciliation.RunLedgerVsProjection` reports zero mismatches.
12. The whole existing idempotency/duplicate-delivery suite — in
    particular the R5 reordering must not change replay behaviour:
    `AlreadyPosted` results, `ErrIdempotencyKeyReused`, tombstones, and
    `postBet`'s pre-`Post` idempotency short-circuit all behave exactly
    as before.
13. The existing four-eyes/authorization suites
    (`withdrawal_approvals`, `bonus_change_approvals`, SEP-1) — untouched
    by this change, and proven untouched.
14. `internal/casino/adversarial_lock_stress_test.go` and
    `stage9_concurrency_integration_test.go` — must still pass with no
    weakening of their assertions (notably that `postBet`'s
    insufficient-funds check is still decided under a held lock, now via
    `LockedProjections.Balance` rather than `lockCashBalance`).

## 7. Consequences

- One extra round trip per posting at most (the ensure-and-lock batch),
  and for pre-locking callers one extra `prepareEntries` pass (an indexed
  `ledger_accounts` read). Accepted.
- Lock hold times lengthen slightly: a posting now holds *all* of its
  projection locks from the pre-lock step rather than acquiring them
  progressively. This is the deliberate trade — broader, shorter-lived
  contention instead of narrow, deadlock-prone contention — and it also
  removes a class of partial-visibility read (a decision made against one
  account while another is still unlocked).
- `wallet_balance_projection` may now contain zero-valued rows for
  accounts whose posting was declined. Harmless; reconciled as `0 == 0`.
- Domain packages lose the ability to lock a balance directly. That is
  the point: the single mechanism that could reopen LOCK-1 is removed,
  and test 9 keeps it removed.
- Future money-touching domains (sportsbook settlement, crypto custody
  settlement, retail agent accounting) inherit correct ordering by using
  `ledger`'s API, with no knowledge of this ADR required.

## 8. Related

- `docs/decisions/0001` (ledger design), `0019` (authoritative ledger and
  balance projection), `0020` (financial idempotency and concurrency
  control), `0032` (bonus accounting / Rule B2), `0038` (sportsbook
  accounting), `0040` (Wave 3 cross-domain composition rulings,
  DR-4HB1W2-01).
- `docs/architecture/ledger-accounting-model.md` §7.4 (mirror generator),
  §7.7.2.9 (HR-25).
- `docs/architecture/06-wallet-ledger-architecture.md`,
  `08-casino-integration-architecture.md`,
  `09-sportsbook-architecture.md` — all three carry pointers to this ADR
  where they describe locking behaviour.
- Migrations `0022` (`ledger_entries` + populate trigger), `0023`
  (`wallet_balance_projection` + projection trigger). **No migration
  changes are required by this ADR.**

---

## Amendment A1 — 2026-09-21 — R6 ACCEPTED (`ledger-finance`)

§2.3 delegates one decision to `ledger-finance`, the owner of financial
invariants, and requires the choice be recorded here rather than made
silently. This is that record.

**Decision: R6 is ACCEPTED as written.** The L3 step materialises an
absent `wallet_balance_projection` row with zero totals from inside
`internal/ledger` and then locks it, per account, in ascending
`ledger_account_id` order. The alternative offered in §2.3 — re-ordering
`Post`'s `ledger_entries` insertion into ascending account order — is
REJECTED.

### Why R6 does not weaken any invariant I own

1. **No balance is ever `UPDATE`d, and no total is ever written.** The
   statement is `INSERT ... SELECT ... FROM ledger_accounts ... ON
   CONFLICT (ledger_account_id) DO NOTHING`, with literal constants `0, 0`
   for `debit_total`/`credit_total`. `DO NOTHING` means an existing row is
   not read, not rewritten and not touched at all. CLAUDE.md's "never
   `UPDATE` a balance" holds literally, not merely in spirit, and there is
   no expression in the statement that could later be made to compute a
   total.
2. **Migration 0023's "maintained EXCLUSIVELY by this trigger" is already,
   deliberately, a package-scoped rule rather than an absolute one.**
   `ledger.RebuildProjectionRow` — the exercised disaster-recovery path
   ADR 0019 requires exist — has always written this table directly from
   `internal/ledger`, using the identical `INSERT ... SELECT ... FROM
   ledger_accounts` shape. R6 adds a second writer inside the same owning
   package, and R4 plus
   `TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage` keep every
   other package out permanently.
3. **Reconciliation loses no detection, and gains one correction.** A
   bypassed or disabled trigger still trips `RunLedgerVsProjection`: a
   zero row against non-zero rebuilt totals is a
   `MismatchKindBalanceMismatch` instead of a
   `MismatchKindMissingProjection`, and both mark the run
   `mismatches_found`. Separately — and this was *not* anticipated by
   §2.3 — R6 **removes an existing false positive**: today an account with
   a `ledger_accounts` row, no entries and no projection row falls through
   `RunLedgerVsProjection`'s first arm (rebuilt totals are zero) into its
   second, where `rebuilt.AssetCode != projected.AssetCode` compares
   `"EUR"` against the empty string of an absent row and reports drift.
   R6's zero row copies asset/type from `ledger_accounts`, so such an
   account reconciles cleanly as `0 == 0`. No change was made to
   `internal/reconciliation` (§4.8); this is a consequence, recorded here
   so it is not later mistaken for a regression.
4. **RLS introduces no new failure mode.** Every `WithPlayerScope` call
   site in the tree was inspected and all are read-only; no posting path
   runs player-scoped. The ensure step therefore runs under exactly the
   policy the trigger it front-runs already requires.

### Why the alternative was rejected

The alternative fixes a **liveness** defect by changing the **shape of
committed financial history**: §7.4.2's "a given logical posting always
produces byte-identical entry rows" would no longer hold, because entry
row order would become a function of randomly-assigned account UUIDs
rather than of the posting's own logical structure. That is an
auditability property being traded away for a deadlock fix that does not
require it. As the owner of financial invariants I will not make that
trade while a liveness-only fix exists. R6 leaves the posting shape, the
entry order, the idempotency key composition and the mirror generator's
output all completely untouched (R7).

### Accepted consequence, restated

`wallet_balance_projection` may now contain zero-valued rows for accounts
whose posting was legitimately declined (e.g. `postBet` with insufficient
funds, which commits an audit record). This is the residue §2.3 already
named. It is harmless, it reconciles as `0 == 0`, and per point 3 above
the account in question already had a `ledger_accounts` row before this
change — the row of zeros is strictly *more* accurate than its absence.

### Related finding recorded while implementing §6 test 4

`ConvertGrant` requires a Grant in status `completed`, while `postBet`'s
wagering-contribution path only ever locks a Grant that is
`activated`/`in_progress`. The SAME Grant therefore cannot satisfy both
sides of LOCK-1d through their production entry points on today's code,
so §1.8's "same player, same grant" overstates how reachable LOCK-1d is
*via `ConvertGrant` specifically*. The lock ORDERING §1.8 describes is
nonetheless exactly what `applyGrantConversion` and a held-disposition
`ACTION_ROUTE_TO_CASH` resolution both perform, and it is reproduced
against the real primitives (`bonus.AdvisoryLockGrant` then a
`ledger.Post` of the conversion's exact two-leg shape) in
`TestLockOrder_ConcurrentBetAndGrantConversion_NoDeadlock`, which fails on
pre-fix HEAD with `40P01` and passes after. The L0.2 fix is retained
unchanged: it is correct, it is cheap, and it closes the cycle before any
future change makes the production pairing reachable.

Similarly, §6 test 6's "caller entries disjoint, generated legs collide"
arrangement **cannot** be made to deadlock on HEAD, because the Rule B2
generator emits its legs in one globally fixed order (all step-2
`promo_liability` legs, then all step-3 recognition legs, assets sorted)
identically for every posting — so two postings sharing only generated
legs share them in the same relative order and no cycle exists. That
sub-case is asserted for what it does prove; the deadlock half of test 6
uses two bonus-touching postings that share `house_gaming` and
`player_bonus_held` in opposite orders (internal/casino's
`postWinLockedBonus`-terminal and `postRollbackHeldWin` shapes), which is
a genuine ABBA and does fail on HEAD.

---

## Amendment A3 — Stage 9.2 — named exception E-3 (`sportsbook`, per `docs/decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md` §7.3)

`docs/decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md`
Part C found that `sportsbook.PlaceBet` already contains an ordering
inversion this ADR's own §1.5 classification implied but never named. This
amendment names it, alongside E-1 (§5.1) and E-2 (§5.1a), per §2's own
"no exceptions except E-1 and E-2, each of which is named, bounded and
guarded by a standing invariant" rule — E-3 is now a third such named,
bounded exception, not a new violation of that rule.

> **E-3 — `sportsbook_bets` (L1) insertion wait taken after the L3
> pre-lock.** `sportsbook.PlaceBet` calls `insertBet` *after*
> `ledger.Post`, so the `UNIQUE (tenant_id, player_account_id,
> idempotency_key)` index insertion wait — a class L1 acquisition — happens
> after L3/L4. §1.5 classified sportsbook bets as L1 but did not name this
> inversion.
>
> **Why it is not reordered:** `insertBet` must carry
> `ledger_transaction_id`, which does not exist until `Post` returns, and
> the cross-check immediately after it (the Stage 6.1 orphaned-posting
> guard) depends on comparing the two. Reordering means either splitting
> the insert into reserve/confirm or dropping that guard — the same shape
> of trade-off as E-2, and the same decision: name it, guard it, do not
> restructure the bet path under schedule pressure.
>
> **Why it is safe today:** a cycle needs a counterpart holding a
> `sportsbook_bets` row lock and then waiting on a
> `wallet_balance_projection` lock. None exists: `findBetByIdempotencyKey`,
> `GET /v1/me/sportsbook/bets` and the admin list are all unlocked reads,
> and settlement/void are NOT IMPLEMENTED (§5.3, unchanged by ADR 0083's
> own Part C/Part B — neither adds a settlement/void ledger-posting code
> path). *(Amended 2026-09-25: Stage 10 W1 adds settlement/void/rollback
> writers; they take the bet row lock at L1 before any L3 lock, so they
> are not such a counterpart — Amendment A4, §5.1c.)* Two concurrent `PlaceBet` transactions take L3 then L1 in the
> *same* order as each other, which is consistent and therefore
> deadlock-free even though it is not canonical.
>
> **INV-LOCK-E3:** `sportsbook_bets` ~~has exactly one writer
> (`sportsbook.insertBet`, from `PlaceBet`)~~ — **reworded 2026-09-25 by
> Amendment A4 (6):** writers are `insertBet` (E-3) and the W1 settlement
> functions, which take L1 first (INV-LOCK-E4). Any second writer — in
> particular a future settlement/void path — must resolve E-3 first, by
> taking its `sportsbook_bets` row lock at L1, *before*
> `LockProjectionsForPosting`. *(Met by the W1 settlement functions,
> §5.1c.)*

This amendment names a pre-existing inversion; it does not introduce one.
ADR 0083 Part C's own new jurisdiction gate (steps 5-7 of that ADR's §7.1)
takes no lock at all and is inserted before the existing L0.4 (RG) lock,
so it cannot violate R8 by construction and does not change this ADR's
lock-acquisition sequence in any other respect. Amendment A2 (new lock
class L0.6, ADR 0083 Part B / §6.2.5) is deliberately NOT made here — it
belongs to the separate wave implementing ADR 0083's cross-player exposure
gate, per that ADR's own §9.4 wave split.

*(Note, 2026-09-25, applying `ledger-finance` review finding P3-2 via
Amendment A4 (8): Amendment A2 was subsequently made — see the A2 block
below.)*

---

## Amendment A2 — Stage 9.2 Part B2 / Wave 3 — new lock class L0.6 (`sportsbook`, per `docs/decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md` §6.2.5/§7.3)

`docs/decisions/0083-sportsbook-jurisdiction-gating-and-cumulative-exposure.md`
Part B2 adds the cross-player, per-event/market/selection book-exposure
gate (`internal/sportsbook/exposure.go`'s `evaluateExposureLimits`), which
this Amendment gives its own advisory-lock class. §2.1's table (above) now
carries this row directly; it is restated here as the amendment record,
alongside Amendment A1 (R6) and Amendment A3 (E-3, Wave 2/Part C).

> **L0.6 — Sportsbook event-exposure advisory
> (`sb_exposure:<tenant>:<event>`).** Acquired by
> `sportsbook.evaluateExposureLimits`, ONLY when at least one
> `sb_exposure_limits` row is configured for the bet's `(tenant_id,
> asset_code)` (§6.2.4 step 1's "none configured ⇒ not armed ⇒ no lock"
> contract — zero rows is the shipped, unarmed default). Keyed on the
> **event id**, never market or selection, whatever mix of `scope_kind`s
> is configured for that tenant: any two bets whose exposure aggregates
> can interact at market or selection level necessarily share the same
> event (selection ⊂ market ⊂ event — §6.2.5's own argument), so one
> event-keyed lock closes every race at all three levels and removes the
> need to invent a within-class ordering rule for multiple L0.6 locks
> (exactly the class of rule R2 exists to avoid having to invent). One per
> bet — never more than one L0.6 acquisition in a single `PlaceBet` call.
>
> **Where it sits in the total order:** immediately **after L0.5** (the
> Risk cumulative advisory) and **before L3** — `evaluateExposureLimits`
> runs after `risk.Evaluate` (ADR 0083 §7.1 step 11 follows step 9) and
> before `ledger.GetOrCreateAccounts`/`LockProjectionsForPosting`. A
> single, consistent L0 sub-ordering (L0.1 → L0.2 → L0.3 → L0.4 → L0.5 →
> L0.6) is what keeps a future reader from reversing any two of them.
>
> **Mechanism:** `hashtextextended` for the full 64-bit key space,
> tenant-scoped, exactly like `AdvisoryLockGrant` and
> `AdvisoryLockPlayerBonusScope` (`internal/bonus/lifecycle.go`) — a
> single 32-bit `hashtext` component is an unacceptable collision risk for
> a shared advisory-lock namespace, and a platform-global (untenanted) key
> would make two tenants' unrelated events serialize against each other.
> Held for the remainder of the caller's transaction; Postgres releases it
> at COMMIT/ROLLBACK, there is no unlock call.
>
> **R8 compliance:** every L0.6 acquisition strictly precedes L3 in
> `PlaceBet`'s own call order (ADR 0083 §7.2's own compliance check) —
> satisfied by construction, since `evaluateExposureLimits` (step 11) runs
> before `ledger.GetOrCreateAccounts`/`LockProjectionsForPosting` (steps
> 12/14) and never itself locks or reads a `wallet_balance_projection`
> row.
>
> **Prospective binding:** sportsbook settlement, void, partial settlement
> and cashout, when built (NOT IMPLEMENTED today), must take L0.6 before
> any L1/L2/L3 lock on the same event, exactly like every other L0
> sub-class's own prospective binding.
>
> **Amended 2026-09-25 by Amendment A4 (5), ADR 0088 §5.4:** settlement,
> void and rollback do **not** take L0.6, because `sportsbook_bets`
> carries no `event_id` and deriving the key would read the catalogue
> before L1 (ADR 0088 §5.4); this must be revisited before any exposure
> limit is armed (HDR-SB-1). Partial settlement and cashout remain bound
> by A2 until their own ADR.

This amendment adds a new lock class; it does not alter any existing
class's position, and it does not touch Amendment A3/E-3, which is a
separate, pre-existing ordering inversion this wave does not revisit.

---

## `ledger-finance` review addendum — 2026-09-21 — Stage 9.2 Amendments A2 and A3

Scope: the financial-invariant and lock-ordering review of Stage 9.2's two
amendments to this ADR (A2, new class **L0.6**; A3, named exception
**E-3**) and of the `sportsbook.PlaceBet` changes they describe. Requested
as a review; not a redesign. Security review ran separately and is not
covered here.

**Verdict: ACCEPTED. No P0 and no P1. No financial invariant is
encroached on.** `SUM(DEBITS) == SUM(CREDITS)`, the append-only rule, the
"never `UPDATE` a balance" rule, the no-floating-point rule and the
database-enforced idempotency guarantee are all untouched by this stage.
Amendments A2 and A3 are internally consistent with R1–R8 and with E-1/E-2,
with the one drafting defect corrected below.

### What was verified, and how

Verified by direct reading of `internal/sportsbook/orchestrator.go`,
`exposure.go`, `jurisdiction.go` and `internal/risk/cumulative.go` at their
current state (not from the implementing agents' reports), and by running
`internal/sportsbook`, `internal/risk` and `internal/ledger` under
`-tags=integration -race -count=1` against a freshly migrated scratch
database (88 migrations): **`internal/ledger` 52/52 pass, 0 skip;
`internal/sportsbook` 56 pass, 0 fail, 5 skip (all five are the rung-2
operating-market tests, correctly skipped as BLOCKED on HDR-J-7);
`internal/risk` pass.** `TestLockOrder_SportsbookPlaceBetAcquiresLocksInCanonicalOrder`,
`TestLockOrder_ConcurrentSportsbookBetsOnSameEventNoDeadlock`,
`TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage` (R4 guard),
`TestLockOrder_PreLockCoversEveryEntryIncludingGeneratedLegs`,
`TestLockOrder_ProjectionRowIsMaterialisedAndLockedWhenAbsent` (R6) and
`TestLedger_BalancedUnderConcurrentLoad` all pass unmodified.

1. **Nothing posted changed.** `PlaceBet` still contains exactly one
   `ledger.Post` call site, and `git diff` shows the `betInput`
   construction, the derived `ledgerIdempotencyKey`, the two-entry
   `Dr player_cash / Cr player_locked_cash` shape,
   `ledger.GetOrCreateAccounts`, `ledger.LockProjectionsForPosting` and
   `ledger.Post` as literally unchanged lines. Neither new gate reads or
   writes a balance, and neither can reach `betInput` (both receive only a
   `catalogueScope` value copy plus scalars; the sole changed field on the
   whole path is `RiskRequest.JurisdictionCode`, which is not a ledger
   input). No new package writes `wallet_balance_projection`; no
   floating-point money was introduced.
2. **Idempotency untouched.** `findBetByIdempotencyKey` is still strictly
   first after structural validation and returns before
   `getSelectionWithContext` and before both new gates, so a retry
   re-evaluates neither jurisdiction, risk nor exposure and acquires no
   lock. (See P2-1 below on the test that is supposed to prove this.)
3. **Lock order confirmed line by line**, not from a report:
   idempotency/selection/status/odds and the whole jurisdiction gate take
   no lock; RG (**L0.4**) → risk (**L0.5**) → `computePotentialReturn` (no
   lock) → `evaluateExposureLimits` (**L0.6**, conditional) →
   `GetOrCreateAccounts` → `LockProjectionsForPosting` (**L3**) →
   `ledger.Post` (**L4**) → `insertBet` (**L1**, E-3). This is exactly ADR
   0083 §7.2. **R8 holds for L0.6 specifically**: the advisory `Exec` in
   `evaluateExposureLimits` strictly precedes every L3 acquisition, and the
   function takes no row lock of its own (both its reads —
   `sb_exposure_limits` and the `sportsbook_bets` aggregate — are plain
   unlocked `SELECT`s). L0.6's key, guard condition ("zero configured rows
   ⇒ no lock"), `hashtextextended` mechanism and one-acquisition-per-bet
   property all match Amendment A2's text exactly.
4. **`IgnoredAccountTypes: ["player_locked_cash"]` is correct**, and
   `TestSportsbookCumulative_TwoPlayerOwnedLegsAreNotNettedToZero` is a
   real proof rather than a tautology: it posts the genuine placement shape
   through `ledger.Post`, then asserts three distinct things — the shipped
   spec measures the full stake, an account-type-blind sum over *the same
   rows* nets to exactly zero (the ADR 0031 §32(a) defect signature,
   reproduced), and emptying `IgnoredAccountTypes` now fails closed with
   `ErrUnrecognizedCumulativeLeg` rather than under-counting.
5. **Exposure is correctly kept out of the ledger.** The aggregate reads
   only `sportsbook_bets.potential_return` (a domain projection, doc 09
   §1.7) and never `ledger_entries`, `ledger_accounts`,
   `wallet_balance_projection` or the `player_locked_cash` balance, so no
   second financial-truth system is created. `SUM(bigint)` returns
   `numeric` in PostgreSQL and is scanned as `pgtype.Numeric`;
   `numericToBigInt` scales a positive `Exp` up rather than truncating and
   refuses a negative one, and comparison is `*big.Int`. No int64 or float
   intermediate exists on the threshold path. This bet's own
   `potential_return` reaches `int64` only through `money.ToInt64`, which
   fails closed on overflow.
6. **R6 residue: better than the ADR claims.** A jurisdiction or exposure
   rejection returns before `ledger.GetOrCreateAccounts`, i.e. before L3
   *and* before the `ledger_accounts` creation step — so it leaves neither
   a zero-totals `wallet_balance_projection` row nor a `ledger_accounts`
   row. §7.2's "strictly better than the insufficient-funds decline" is
   correct and understated.
7. **The `computePotentialReturn` move is inert.** The call is
   byte-identical and its arguments (`params.StakeAmount`,
   `sel.OddsNumerator/Denominator`, `params.AssetCode`) are unmodified
   between the old and new positions, so the computed value cannot differ.
   `assetregistry.GetAsset` is a plain unlocked `SELECT`, so the move
   introduces no lock. Only the moment an asset-registry/rounding error
   surfaces changes, and both orders are fail-closed.

### Findings

- **P2-1 — the idempotency test does not prove what its own comment claims
  is "the decisive proof".** `TestSportsbookCumulativeAndExposure_IdempotentRetryReevaluatesNothing`
  disables the exposure limit between the two calls. With the limit
  disabled, a *full re-evaluation* would also pass the gate, also hit
  `ledger.Post`'s `AlreadyPosted` short-circuit, also take `insertBet`'s
  conflict path and also return the same bet id — so neither
  `retry.Bet.ID == first.Bet.ID` nor `countBets() == 1` distinguishes
  "short-circuited at step 2" from "re-evaluated and converged". The
  decisive form is to **leave the limit armed**: at ceiling 3 000 with a
  2 000 `potential_return` per bet, re-evaluation would compute
  2 000 (already open) + 2 000 (incremental) > 3 000 and return
  `RejectionExposureLimit`, so an accepted retry could then only come from
  the short-circuit. The underlying property is correct (verified by direct
  reading); the *test* is the gap. Owner: `sportsbook`/`qa`.
- **P3-1 — FIXED IN THIS EDIT.** §2.1's rule text still read "No
  exceptions except E-1 (§5.1) and E-2 (§5.1a)" after Amendment A3 added
  E-3. A rule whose own exception list is out of date is precisely the
  drift ADR 0083's Status section warns about. Updated in place.
- **P3-2 — Amendment A3's closing paragraph is now stale.** It states that
  "Amendment A2 ... is deliberately NOT made here — it belongs to the
  separate wave", which was true when Wave 2 landed but is no longer: A2
  sits immediately below it in this same file. (A3 also physically precedes
  A2, so the file order is A1, A3, A2.) Recommend a one-line
  "subsequently made — see the A2 block below" note rather than rewriting
  the amendment record. Not corrected here, to avoid editing another
  wave's amendment text under a review task.
- **P3-3 — the L0.6 arming transition is an unrecorded, transient
  under-count.** A bet that entered `evaluateExposureLimits` while zero
  limits were configured takes no L0.6 lock; if an operator arms and
  commits a limit before that bet commits, the next bet's aggregate misses
  the in-flight one. Bounded by the bets in flight at the instant of
  arming, self-healing on the next bet, and structurally incapable of
  affecting the ledger — but it should be one sentence in ADR 0083 §6.2.5
  and on the admin write surface, not folklore.
- **P3-4 — R6 no-residue is asserted by proxy.**
  `TestSportsbookExposure_BalanceInteraction` asserts
  `countLedgerTransactions() == 0`; it never queries
  `wallet_balance_projection` directly, so it would not catch a future
  reordering that materialised a zero-totals row while still posting
  nothing. One extra `SELECT count(*) FROM wallet_balance_projection ...`
  would make the assertion direct.

None of P2-1 or P3-1..4 blocks Stage 9.2. No part of ADR 0083 §6.2 is
rejected under ADR 0082 §7.4 / CLAUDE.md's financial-invariant authority.

---

## Amendment A4 — 2026-09-25 — Stage 10 W1 — sportsbook settlement, void and rollback (`ledger-finance`, per `docs/decisions/0088-sportsbook-settlement-implementation-contract.md` §5/§5.5)

Applied on acceptance of ADR 0088 (ACCEPTED 2026-09-25). Text of ADR 0088
§5.5, verbatim; each numbered item is also marked inline at its target
location in this file. The settlement driver this amendment orders is
in-house **`MOCK`** mode, driven by a non-production test-support staff
route (ADR 0088 §9) — not a provider integration.

> **Amendment A4 — Stage 10 W1 — sportsbook settlement, void and rollback
> (ADR 0088 §5).** (1) §1.5 row "settlement / void …" now reads: L1
> `sportsbook_bets` row `FOR UPDATE` → (rollback/void-after-settlement) L2
> settlement `ledger_transactions` `FOR UPDATE` → L3
> `LockProjectionsForPostings` over every posting → L4 `Post` (one or
> two) → E-4. (2) §2.2 R1/R3: an operation posting more than one
> transaction pre-locks the union once via `LockProjectionsForPostings`;
> the pre-locked inputs are exactly those posted. (3) §2.1 exception list
> becomes "E-1, E-2, E-3 and E-4". (4) New §5.1c **E-4** with
> INV-LOCK-E4 (text of ADR 0088 §5.3, including the "writes only after
> all L3 locks are held" clause). (5) Amendment A2's "Prospective
> binding" paragraph: settlement, void and rollback do **not** take L0.6,
> because `sportsbook_bets` carries no `event_id` and deriving the key
> would read the catalogue before L1 (ADR 0088 §5.4); this must be
> revisited before any exposure limit is armed (HDR-SB-1). Partial
> settlement and cashout remain bound by A2 until their own ADR. (6)
> Amendment A3's "`sportsbook_bets` has exactly one writer" and
> INV-LOCK-E3's text become: "writers are `insertBet` (E-3) and the W1
> settlement functions, which take L1 first (INV-LOCK-E4)". (7)
> §4.4/§5.3 "NOT IMPLEMENTED" updated to point here. (8) Also apply the
> pending P3-2 note ("A2 subsequently made — see below").

Where applied:

| Item | Location in this file |
|---|---|
| (1) | §1.5 — row split into "settlement / void / rollback" (new order) and "partial settlement / cashout" (still NOT IMPLEMENTED; original row text preserved inline) |
| (2) | §2.2 — inline notes under R1 and R3 |
| (3) | §2.1 — exception-list sentence |
| (4) | new §5.1c (between §5.1a and §5.2) |
| (5) | Amendment A2 — note appended to the "Prospective binding" paragraph |
| (6) | Amendment A3 — INV-LOCK-E3 reworded (original wording struck through, not deleted); ADR 0083 §7.3/§10 carry the same rewording |
| (7) | §4.4 and §5.3 — pointer notes |
| (8) | Amendment A3 — closing-paragraph note after "per that ADR's own §9.4 wave split" |

What this amendment does not do: it does not close E-3 in `PlaceBet`
(ADR 0088 §15 OI-4), does not answer HDR-SB-1, and does not change R2, R4,
R5, R6, R7 or R8. R4 is preserved — `LockProjectionsForPostings` lives in
`internal/ledger/lockorder.go`.

---

## Amendment A5 — 2026-09-26 — Stage 10.1 PAY-REV-1 (ADR 0090): inventory update only, no new class or exception

**Scope: inventory and pointer only, per the planning gate's architect
ruling (`docs/plans/stage-10.1-planning/05-review-architect.md` §(1)).
This amendment adds no new lock class and no new named exception; R1–R8
are unchanged.**

Stage 10.1 fixed PAY-REV-1 (a check-then-insert race in
`internal/payments`' deposit-reversal callback: two concurrent,
distinct-provider-reference reversal callbacks for the SAME original
deposit could both observe "not yet reversed" before either posting was
visible to the other, and both post, over-debiting `player_cash`) by
giving `receiveDepositReversalCallback` its own explicit lock:

```
SELECT transaction_type FROM ledger_transactions WHERE id = $orig AND tenant_id = $tenant FOR UPDATE
```

taken before `GetOrCreateAccounts`/L3/`Post`, then a fresh, post-lock
re-check of "already reversed?" (relying on READ COMMITTED to see a
concurrent winner's commit). This is why this ADR needs updating at all:
§1.6's "deposit reversal" row previously read "implicit projections only"
and §4.5 previously said "No code change required … neither path takes
any lock outside `Post`" — both became stale the moment this lock was
added, even though the fix itself needed no new rule.

**Why this is a PLAIN L2 instance, not a new exception:**
- It is a single row lock, taken on `ledger_transactions` — the same
  table and the same lock strength (`FOR UPDATE`) as `internal/casino`'s
  `postRollback` precedent (§1.3), which this fix mirrors exactly.
- It is taken strictly before this call's own `GetOrCreateAccounts`, L3
  pre-lock and `Post` (L4) — the canonical §2.1 order, satisfied with no
  reordering needed.
- No L0 (advisory) or L1 (domain state row) lock exists on this path
  before or after the change; `deposit_intents` is read, never locked or
  mutated, by a reversal.
- Deadlock analysis: a concurrent deposit success, or a withdrawal, on the
  same wallet takes no lock on the ORIGINAL deposit's `ledger_transactions`
  row, so no new cycle is introduced (payments' own analysis, confirmed by
  `ledger-finance`).

**What this amendment does:**
1. §1.6's "deposit reversal" row is updated (struck-through original text
   preserved inline, not deleted) to show the new **L2** step.
2. §4.5 gets a pointer note recording the change and why it needed no new
   rule.
3. This section states, explicitly: no rule/class change; no new named
   exception (there is no "E-5").

**What this amendment does not do:** it does not touch LOCK-1b or its own
closure (§1.6/§4.5's ORIGINAL content, both preserved above the
strikethrough), does not change R1–R8, and does not touch anything in
`internal/casino`, `internal/sportsbook`, `internal/bonus` or
`internal/withdrawal`. The full S0–S7 sequence, the migration 0092
backstop index, and the HTTP-layer denial handling are recorded in ADR
0090 → `docs/plans/stage-10.1-planning-gate-proposal.md` §E (ADR 0090
itself only references the sequence by name; §E of that report is where
it is actually spelled out step by step), not repeated here.

**S-4d (added 2026-09-26, Stage 10.1 post-implementation architecture/
security review):** the new `FOR UPDATE` this amendment records requires
the runtime database role to retain `UPDATE` privilege on
`ledger_transactions` — `SELECT ... FOR UPDATE` needs `UPDATE`, not merely
`SELECT`, on the target table/row, per PostgreSQL's own row-locking
privilege model. This codebase already REVOKEs `UPDATE` on
`sportsbook_bet_settlements` (Amendment A4's E-4 precedent) for a
different table, so the pattern of tightening privileges after a
migration is a live one here. If `UPDATE` on `ledger_transactions` is ever
REVOKEd from the runtime role for any reason, this lock — and
`internal/casino`'s `postRollback` precedent it mirrors, which takes the
identical `FOR UPDATE` shape on the same table — both fail CLOSED: every
deposit-reversal callback and every casino rollback would return a plain
500 (a permission-denied error surfacing through the generic error path),
never a silent bypass of the lock. Recorded here as a dependency to check
before any future privilege-tightening migration touches
`ledger_transactions`, not as an open action item.

## Amendment A6 — 2026-09-26 — Stage 10.3 CAS-CAP-ROLLBACK-1 (ADR 0092): L0.1 also taken by `postRollback`

### Amendment (Stage 10.3, ADR 0092)

**Owner: `ledger-finance`.** Recorded by `architect` from `ledger-finance`'s own design
(`docs/plans/stage-10.3-planning/02-casino-financial-analysis.md` §1.4 step 4). The contract it
serves is the ADR 0025 Stage 10.3 amendment.

**This amendment does not:**
- add a lock class;
- add a named exception (there is no "E-5");
- change R1–R8.

**What changes.**
- `postRollback` now takes the **L0.1** casino bet-delivery advisory lock as its **first** lock.
- The key is the **original** reference:
  `pg_advisory_xact_lock(hashtextextended('casino_bet_delivery:' || tenant || ':' || provider || ':' || original_provider_tx_id, 0))`.
  This is the same key string that `postBet` takes on its own reference
  (`internal/casino/orchestrator.go:914` at `c90e591`).
- L0.1 is taken before the L2 `ledger_transactions` original-row `FOR UPDATE`.
- It is therefore also taken before any L0.3 grant advisory that the bonus-funded rollback path
  takes afterwards under the existing E-1 (§5.1).

**The ordering rules still hold.**
- L0.1 is still taken **exactly once per transaction** and first.
- R8 (advisory locks precede row locks) holds.
- The §2.1 row for L0.1 now reads: "taken once per transaction by `postBet` (own reference) and
  `postRollback` (original reference)". It still raises no ordering question within the class.

**Why.**
- A concurrent late original bet and its rollback now serialize on the same key.
- Either the bet posts first and the rollback reverses it, or the tombstone is written first and
  the bet is rejected with the named `ErrOriginalTombstoned`.
- Today the loser of that race gets an untyped unique-violation error, which surfaces as a 500.

**Deadlock analysis.**
- Two transactions contend on L0.1 only for the same (tenant, provider, reference).
- Neither holds any other lock when it requests L0.1, so no cycle through L0.1 is possible.
- After L0.1, each follows the unchanged canonical order: L0.2/L0.4/L0.5 → L3 for `postBet`, and
  L2 → (E-1 L0.3) → L3 for `postRollback`.

**Permitted extension for W1c.** If `ledger-finance` rules that E10 needs the same serialization,
`postWin` may take L0.1 on its **own** reference as its first lock. E10 is a late win racing the
rollback of that win, which keys L0.1 on the win's reference. That shape is within this amendment:
one L0.1 per transaction, taken first. When implemented, it must be recorded in the §1.3 inventory.

**Tests** (`qa` binding plan, `04-review-qa.md` §4 W1c):
- The late-original-versus-rollback race runs 50 iterations. The test asserts where the waiter
  blocks (L0.1), not only the outcome.
- Concurrent identical rollback callbacks produce exactly one tombstone.

**Status.** `NOT IMPLEMENTED` at acceptance. Target: `IMPLEMENTED` in W1c.

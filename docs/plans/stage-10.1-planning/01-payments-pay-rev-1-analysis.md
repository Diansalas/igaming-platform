> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# PAY-REV-1 remediation — planning analysis

Repo at `20c72e4`, branch `claude/focused-wright-jw88w9`. Planning only — no
files changed by this analysis.

---

## 1. Exact root cause and every reachable code path

### 1.1 Root cause, file:line

`internal/payments/orchestrator.go`, `receiveDepositReversalCallback`
(function starts `:931`):

1. **Lock-free original load.** `:932` calls
   `loadDepositIntentByProviderRef` (`:383-396`), a plain `SELECT` with no
   `FOR UPDATE`, resolving `original` (the `deposit_intents` row) and, via
   `original.LedgerTransactionID`, the original ledger transaction. Nothing
   locks `ledger_transactions` row `original.LedgerTransactionID` at any
   point in this function.
2. **Unlocked "already reversed?" check.** `:986-992`:
   ```go
   tx.QueryRow(ctx,
       `SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE reverses_transaction_id = $1
                         AND (provider_id IS DISTINCT FROM $2 OR provider_tx_id IS DISTINCT FROM $3))`,
       original.LedgerTransactionID, providerID, event.ProviderReference,
   ).Scan(&alreadyReversed)
   ```
   Plain `SELECT`, no lock. Two concurrent callbacks under **distinct**
   `event.ProviderReference` values both evaluate this before either
   commits and both see `alreadyReversed = false`.
3. **Key derived from the reversal's own reference, not the original.**
   `:1009-1020`: `IdempotencyKey: providerID + ":" + reversalRef` where
   `reversalRef := event.ProviderReference` (`:1009`). Two distinct
   reversal references therefore produce two distinct idempotency keys, so
   `ledger.Post`'s own `(tenant_id, idempotency_key)` uniqueness (its only
   collision detector) never fires between them — each is a "new" posting
   as far as `Post` is concerned.
4. **Non-unique index.** `migrations/0021_create_ledger_transactions.up.sql:55`:
   `CREATE INDEX idx_ledger_transactions_reverses ON ledger_transactions
   (reverses_transaction_id) WHERE reverses_transaction_id IS NOT NULL;` —
   a plain (non-unique) partial index. It accelerates the `:987` `EXISTS`
   lookup but enforces nothing.

Net effect: this is textbook check-then-insert on a financial write, which
CLAUDE.md explicitly forbids ("enforced by the database, not check then
insert"). `ledger.Post`'s ADR 0082 L3 pre-lock (`ensureAndLockProjectionsInOrder`,
`internal/ledger/lockorder.go:259`) serializes the two callbacks only on
their **projection** rows (`player_cash`, `psp_clearing`) — it has no
concept of "at most one reversal per original transaction" and cannot
provide it. Both transactions pass the `:986` check before either reaches
L3/L4, both post distinct `deposit_reversal` transactions, and
`SUM(DEBITS)==SUM(CREDITS)` still holds (each individual posting is
balanced), so this is invisible to `ledger_vs_projection` reconciliation.
This is exactly what the W1 ledger-finance sign-off (§4) and the F-7 audit
(§7.4 item 5) both confirmed by a (deleted) empirical probe: 2–6 concurrent
distinct-reference reversals of one 1,000-unit deposit posted 2–6 times,
driving `player_cash` to −1,000 … −5,000.

Note what F-7 (`36616f1`) did and did not fix here: it excluded the
reversal's **own** reference from the `:986-989` `EXISTS` predicate (`IS
DISTINCT FROM`), so a *sequential* redelivery of the *same* reversal
reference is now correctly idempotent (falls through to `ledger.Post`,
which recognizes the identical key/payload and returns `AlreadyPosted`).
That change is orthogonal to PAY-REV-1: it does not add, remove, or narrow
any lock, and it does not close the distinct-reference race. The audit
explicitly flagged this as "New finding, not F-7 … own item" (§7.4 item
5).

### 1.2 Every code path that can create a deposit reversal

Grep-verified (`rg 'receiveDepositReversalCallback|CallbackEventDepositReversal|postDepositReversalTombstone'`):

| Path | Reachability | In/out of Stage 10.1 scope |
|---|---|---|
| `POST /v1/webhooks/payments/{tenantSlug}/{providerID}` → `newPaymentWebhookHandler` (`internal/httpserver/deposit_handlers.go:257-362`) → `Orchestrator.ReceiveCallback` (`orchestrator.go:829`) → `receiveDepositReversalCallback` when `event.EventType == CallbackEventDepositReversal` | **The only production/reachable path.** Requires a signature-verified callback (`provider.HandleCallback`) against a real tenant's active status. Today only `*payments.MockProvider` is registered (`cmd/platform-api/main.go`), so this is dev/mock-only, but the code path is exactly what a real PSP webhook would hit. | **IN scope.** This is the vulnerable path PAY-REV-1 names. |
| Player-facing simulation route `POST /v1/players/me/deposits/{id}/simulate-callback` → `newSimulateDepositCallbackHandler` (`internal/httpserver/payment_deposit_simulation_handlers.go:254`) | **Cannot emit a reversal.** It hardcodes `payments.CallbackEventDeposit` / `OutcomeSucceeded` only (`:296-297`); there is no reversal-simulation endpoint anywhere in `internal/httpserver`. Confirmed by grep: no handler constructs `CallbackEventDepositReversal`. | **OUT of scope** — no such path exists to fix. |
| Any admin/back-office path | **None exists.** No back-office handler calls `ReceiveCallback` or constructs a `deposit_reversal` ledger posting; the only writer of `TxDepositReversal` in the whole tree is `receiveDepositReversalCallback` (grep: `ledger.TxDepositReversal` appears exactly once, at `orchestrator.go:1013`). | **OUT of scope** — nothing to change. |
| Reconciliation | **Read-only.** `internal/reconciliation` never posts; it only detects drift after the fact, and (per the W1 sign-off, §4) it currently cannot detect this defect at all, because each individual reversal posting is internally balanced. | **OUT of scope for the fix itself**, but item 5's "reconciliation check" (§6 below) is an optional in-scope addition. |
| `postDepositReversalTombstone` (`orchestrator.go:1048-1066`) | Only reached when `!found || original.LedgerTransactionID == nil` (`:937`) — i.e. no real original exists yet. Its own idempotency key is deterministic per `(providerID, originalRef)` (`fmt.Sprintf("tombstone:%s:%s", ...)`, `:1058`), so concurrent tombstone attempts for the *same* never-seen original collapse to one row via `ledger.Post`'s own idempotency check — no lock-free race exists here because the key itself, unlike the reversal path, is derived from the **original's** reference, not the caller's own. | **OUT of scope** — already correct by construction; not part of PAY-REV-1. |

### 1.3 Other reversal-shaped postings — which share the pattern, which don't

| Type / site | Lock before "already reversed?" check | Key derivation | Shares PAY-REV-1's defect? | In/out of Stage 10.1 |
|---|---|---|---|---|
| `casino_rollback`, generic (`internal/casino/orchestrator.go`, `postRollback:1268`) | **Yes.** `:1285-1288`: `SELECT id, transaction_type FROM ledger_transactions WHERE tenant_id=$1 AND provider_id=$2 AND provider_tx_id=$3 FOR UPDATE` locks the **original** row (found by the *rollback's* `OriginalProviderTxID`, but the lock target is the original transaction row itself) before the `:1334-1343` already-reversed check runs. This is the ADR 0082 L2 lock, taken before L3/L4, exactly the shape this fix needs. | Key is `providerID:ProviderTxID` — the rollback's own reference (structurally the same shape as payments' `providerID:reversalRef`), **but** the L2 lock on the original closes the race the key alone cannot. | **No — already fixed.** The code comment at `:1276-1282` states this was itself found and fixed by a prior specialist review ("two distinct concurrent rollback references for one bet both succeeded, doubling the reversal credit"). This is the **precedent pattern** for PAY-REV-1's own fix. | Reference precedent only; not touched by 10.1. |
| `casino_rollback` of a held win (`postRollbackHeldWin`, `internal/casino/bonus_settlement.go:582`) | Inherits the same `postRollback` L2 lock (called from within `postRollback` after the lock is already held, `:1352-1355`) plus its own `bonus_held_dispositions` row `FOR UPDATE` (ADR 0082 HR-25 order). | Same as above. | **No — already fixed** (same L2 lock). | Not touched. |
| `sportsbook_rollback` (`internal/sportsbook/settlement.go`, `lockAndPost`, ADR 0088 §5.1) | **Yes**, and stronger: L1 `sportsbook_bets` row `FOR UPDATE` first, then L2 `SELECT … FOR UPDATE` on the settlement's own `ledger_transactions` row, then `LockProjectionsForPostings` (L3), all *before* any decision or `Post` call (ADR 0088 §4.3's decision table runs entirely under L1). Additionally backstopped by a partial-unique index on `sportsbook_bet_settlements` (append-only history table, migration 0091) and by `ledger.Post`'s own F-7 canonical-payload compare. | Deterministic, server-composed key `sportsbook_rollback:<bet_id>#<g>` — never derived from a caller/provider-supplied reference at all (this whole family is staff-driven test-support only per ADR 0088 §9; not provider-callback-driven). | **No — already fixed, and structurally cannot recur** (no provider-reference-keyed reversal exists in this family). | Not touched; cited only as the ADR 0082/0088 precedent for "L2 lock before L3, then L4". |
| `withdrawal_reversed` (`ledger.TxWithdrawalReversed`, `internal/ledger/ledger.go:114`) | N/A | N/A | **N/A — NOT IMPLEMENTED.** Grep for any production poster of `TxWithdrawalReversed` in `internal/withdrawal` returns nothing; the constant exists in the CHECK-constraint enum with no writer anywhere in the tree. There is nothing to audit or fix. | **OUT of scope** — no code exists. Flag as "declared but unimplemented; must be built to this ADR's lock-order rule from day one" if/when it lands (mirrors the F-7 audit's identical treatment of `manual_adjustment`). |
| `bonus_reversal` (`ledger.TxBonusReversal`, `internal/ledger/ledger.go:138`) | N/A | N/A | **N/A — NOT IMPLEMENTED as a caller-facing "reverse an already-posted bonus event" flow.** `TxBonusReversal` is declared in the CHECK-constraint enum (ADR 0032 §3.1 lists it as one of the four bonus posting shapes distinguished by `reason_code`) but grep (`rg 'TxBonusReversal' internal/`) finds it only in `ledger.go`'s own const block and doc comments — no production `Post` call site uses it (the F-7 audit's call-site inventory, §2/§3, lists only `TxBonusGrant`/`Conversion`/`Forfeiture` as posted; `TxBonusReversal` is absent from every one of the 21 call sites). | **OUT of scope** — no code exists. Same flag as `withdrawal_reversed`. |
| Deposit reversal **tombstone** (`postDepositReversalTombstone`) | N/A — no original to lock (that is the whole point: the original was never seen). | Deterministic key derived from the **original's** reference (`tombstone:%s:%s`, providerID + originalRef) | **No.** Already race-free by construction (see §1.2 above): concurrent tombstone attempts for one never-seen original collapse on `ledger.Post`'s own idempotency key, because — unlike the live-reversal path — the key is keyed on the *original's* identity, not the caller's own reference. | Not touched. |

**Conclusion for item 1:** exactly one code path is broken
(`receiveDepositReversalCallback`, reachable only via the real/mock PSP
webhook), and the fix pattern already exists in this codebase
(`casino.postRollback`'s L2 `FOR UPDATE`). No other reversal type needs a
code change; two declared types (`withdrawal_reversed`, `bonus_reversal`)
have no poster at all and should be recorded as "must follow this pattern
when built," not fixed now (no code to fix).

---

## 2. Database invariant

### 2.1 The invariant, precisely

> **INV-PAY-REV-1:** for a given `tenant_id`, at most one un-tombstoned
> `ledger_transactions` row may exist with a given non-null
> `reverses_transaction_id` **when `transaction_type = 'deposit_reversal'`**.

Stated as: "at most one `deposit_reversal` transaction reverses a given
original transaction." This is deliberately **narrower** than "at most one
reversing transaction of *any* type per original" — see below for why a
type-scoped index is required, not a bare `reverses_transaction_id` unique
index.

### 2.2 Must the index be global (any type) or per-type?

**Per-type (`WHERE transaction_type = 'deposit_reversal'`), not global.**
Evidence that a global index would be **incorrect** — i.e. would reject
data shapes the platform already legitimately produces or reasonably will:

1. **Sportsbook rollback-then-void composes two postings that both name
   the same settlement transaction via `reverses_transaction_id`.**
   ADR 0082 §5.1c / ADR 0088 §5.1: "void-after-settlement" posts a
   rollback (`sportsbook_rollback`, `ReversesTransactionID = <the
   settlement's tx id>`) **and then**, in the same DB transaction, a void.
   The void's own posting does not reverse the settlement directly (its
   entries are the before-settlement void shape), so this specific
   composition does not by itself create two rows with the *same*
   `reverses_transaction_id`. However, the **re-settlement** flow
   (ADR 0088 §4.3's decision table, "tombstone, then a late settle, then
   the next generation") can leave **two** rows whose
   `reverses_transaction_id` both point at the *same* original settlement
   transaction across generations if a bet is rolled back, re-settled, and
   rolled back again — each rollback names its own generation's settlement
   transaction id, which differs per generation, so in practice these do
   **not** collide on the same `reverses_transaction_id` value either.
   This case is more subtle than it first looks; treat it as **needs a
   data check before deciding**, not as a proven collision (see §2.4
   below — this is exactly why a pre-flight scan is mandatory, not
   optional).
2. **`casino_rollback` of a `casino_win` with bonus attribution
   (`postRollbackHeldWin`) plus a possible companion forfeiture.** These
   are two *different* transaction types (`casino_rollback` and
   `bonus_forfeiture`) that can both carry metadata related to one
   economic event, but `bonus_forfeiture`'s `reverses_transaction_id` is
   not set at all in the current schema/call sites (grep: no
   `bonus_forfeiture` posting sets `ReversesTransactionID`) — so no
   collision exists today, but a global index would forbid a *future*,
   legitimate design where a bonus forfeiture and a casino rollback both
   legitimately reference the same original via `reverses_transaction_id`
   for different reasons (they are different facts about different
   accounts). A **type-scoped** index leaves that design space open
   without deciding it now; a global index forecloses it silently.
3. **Partial refunds are explicitly NOT IMPLEMENTED** (payments) and
   Stage 3B/10 only ever reverse a **whole** deposit
   (`financial-transaction-flows.md` Flow 2's own text: "Stage 3B
   implements only whole-deposit reversal … never a partial-refund
   feature" — `orchestrator.go:960-963`). If/when partial refunds are
   built, a payments-scoped "at most one reversal of a given original"
   rule would need to become "the sum of reversal amounts for a given
   original ≤ the original's amount," which is a **different**,
   amount-aware invariant that a bare uniqueness index cannot express at
   all — recorded as a forward-compatibility note, not solved here.
4. **`sportsbook_void` does not use `reverses_transaction_id`** (it is a
   *composed*, independent posting, not a reversal of a specific
   transaction id — ADR 0088 §2.3's void-before/void-after shapes). No
   collision risk from that type.

**Decision: the migration must add
`CREATE UNIQUE INDEX ... ON ledger_transactions (reverses_transaction_id)
WHERE transaction_type = 'deposit_reversal' AND reverses_transaction_id IS
NOT NULL` (payments-scoped only)**, not a bare
`WHERE reverses_transaction_id IS NOT NULL` index. Extending the same
pattern to `casino_rollback` (already lock-protected, but currently has
**no** database backstop at all — the F-7 audit and the W1 sign-off both
flag this as worth evaluating in the same item) should be evaluated as a
**second, separate partial unique index**
(`WHERE transaction_type = 'casino_rollback'`) in the **same migration**,
because `casino_rollback`'s own values of `reverses_transaction_id` are
disjoint from `deposit_reversal`'s by construction (different original
transaction types), so the two indexes cannot conflict with each other —
but this is a scope decision for item 7 below (recommended, not forced):
`casino_rollback` is not reported broken today (the lock already prevents
the race), and CLAUDE.md's "no uncontrolled scope expansion" argues for
doing the payments index first and the casino index as an explicitly
separately-reviewed, low-risk addition in the same migration only if
`ledger-finance`/`code-reviewer` agree it is in-scope hardening rather than
scope creep. Sportsbook needs no such index — ADR 0088's own history-table
partial unique indexes already provide this backstop for that family
(confirmed by the W1 ledger-finance sign-off §1).

### 2.3 Does a unique index alone suffice?

**No — three residual gaps, all must be closed by the L2 lock and/or
caller logic, not by the index:**

1. **The index only prevents the *second write*, not the *business
   decision or the audit trail* for the loser.** Without an application-
   level check-then-insert control that runs under a lock, the loser's
   transaction still executes the entire pre-`Post` code path (amount/asset
   validation, `GetOrCreateAccounts`, building the `TransactionInput`) and
   only discovers the conflict at the `ledger_transactions` insert itself
   (a bare unique-constraint violation, not `db.IdempotentInsert`'s own
   `(tenant_id, idempotency_key)` conflict path, since the two reversals
   have *different* idempotency keys by construction). That raw constraint
   violation today has **no typed sentinel and no caller-side mapping**
   (unlike `ErrIdempotencyKeyReused`/`ErrIdempotencyPayloadMismatch`, which
   are keyed off a *different* index). It would surface as a bare
   `pgx`/`pgconn` unique-violation error wrapped only by `ledger: insert
   transaction: %w` (`ledger.go:362`), which the payments HTTP handler
   would currently map to a generic 500 (`payment_webhook_failed`,
   `deposit_handlers.go:350-353`), not the domain-correct 409 +
   `payment_webhook_integrity_alert_*` treatment every other
   integrity failure gets. **The L2 lock is what prevents this scenario
   from ever reaching the database at all** — the loser is turned away
   *before* it builds a `TransactionInput`, with a named Go sentinel
   (`ErrDepositAlreadyReversed`, which the code already has) rather than an
   opaque DB constraint error. The index is the backstop for a future
   writer that skips the lock (per ADR 0082's own philosophy — R4's
   `wallet_balance_projection` analogy), not the primary control.
2. **Audit correctness.** `receiveDepositReversalCallback` writes
   `deposit.reversed` (`:1033-1043`) only on the winning path today; if the
   loser instead fails on a raw unique-violation without a caller-level
   check, no audit record at all is written for the *rejected* attempt —
   violating CLAUDE.md's "every mutating administrative/financial action
   writes an audit record" for the denied case. The fix must ensure the
   rejected reversal is audited as a denial (mirroring
   `ErrAlreadyRolledBack`'s treatment in casino, which the F-7 audit
   confirms has no dedicated audit path either — this is a pre-existing
   gap worth closing in the same change, not a new one this fix
   introduces, but it becomes visible/testable once the lock exists).
3. **User/PSP-visible result for the second request.** Per item 4 below,
   the correct external contract for "distinct reference naming an
   already-reversed original" is a rejection (`ErrDepositAlreadyReversed`
   → HTTP 409), not a silent 200. A unique index by itself cannot produce
   that response shape; only application code that catches the lock/index
   outcome and maps it to the existing sentinel can.

**Conclusion:** the unique index is necessary (closes the gap for any
future writer that bypasses the lock, and is cheap insurance against a bug
in the lock's own scope) but not sufficient on its own; the L2 lock plus
existing caller logic (`ErrDepositAlreadyReversed`) is the primary
mechanism, exactly as the W1 sign-off's "Recommended fix (all three
parts)" already states.

### 2.4 Could existing data already violate the invariant?

**Must be checked, not assumed.** All data at HEAD is dev/synthetic
(CLAUDE.md environment-safety rule — no production data exists), but the
migration's pre-flight check (§5) must not assume that: CI databases,
local dev databases carrying test fixtures, and any staging database that
has run the mock PSP under load (the very probe described in the W1
sign-off and the F-7 audit was run against a live local database and then
"deleted" — its *rows*, not just its test file, must be considered
possibly still present in whatever database it ran against) could already
contain duplicate `deposit_reversal` rows for one original. The migration
must refuse to apply if any exist (§5), full stop, rather than assume
"this is Stage 10, so it can't have happened yet."

---

## 3. Transaction ordering / concurrency model (ADR 0082 alignment)

### 3.1 Where the new L2 lock goes

Per ADR 0082 §2.1's class table (L0 advisory → L1 domain rows → **L2
`ledger_transactions` FOR UPDATE, ascending id** → L3 projections → L4
idempotency-key insert + entries), and per the `casino.postRollback`
precedent (`orchestrator.go:1276-1288`, itself an ADR 0082-conformant L2
lock predating the ADR's own formal numbering but retroactively an
instance of the same class), the new lock must be:

```go
// New, immediately inside receiveDepositReversalCallback, AFTER the
// tombstone branch (:937-953, which has no original row to lock) and
// BEFORE the amount/asset validation (:964-971) and the already-reversed
// check (:986-995).
var lockedType ledger.TransactionType
err := tx.QueryRow(ctx,
    `SELECT transaction_type FROM ledger_transactions WHERE id = $1 FOR UPDATE`,
    original.LedgerTransactionID,
).Scan(&lockedType)
```

Placement relative to L3/L4:

- **Before L3 (`LockProjectionsForPosting`/`Post`'s internal
  `ensureAndLockProjectionsInOrder`).** This satisfies ADR 0082 R8
  ("advisory locks strictly precede row locks... L0 before L1/L2/L3") by
  extension: L2 must precede L3, and this new lock introduces no L0/L1
  interaction, so no new exception is needed. This is exactly
  `casino.postRollback`'s existing shape (L2 at `:1288`, then eventually
  `Post`'s internal L3 at whatever point `postRollback`'s own `Post` call
  is reached) and requires no new named exception (E-1..E-4) in ADR 0082 —
  it is a **new instance of the existing L2 class**, not a new mechanism.
- **Ascending id within class, tie-break rule.** Trivial here: exactly one
  `ledger_transactions` row is locked per call (the original), so there is
  no ordering-among-multiple-L2-locks question the way `postWin`'s
  `ORDER BY id FOR UPDATE` over a round's rows has. No change to the
  "ascending id" rule is needed; it degenerates to a single-row case.
- **Re-check after acquiring (READ COMMITTED semantics).** Postgres's
  default `READ COMMITTED` means the `:987-989` "already reversed?" query,
  if left unchanged, would — once re-run *after* the L2 lock — correctly
  see a concurrent committer's row (a blocked second transaction resumes
  after the first commits and its own subsequent `SELECT`s see the
  post-commit state under READ COMMITTED, because each statement takes a
  fresh snapshot). **The already-reversed check must therefore be
  re-executed (or executed for the first time) only after the L2 lock is
  held**, not before it — moving it from its current position (`:986`,
  currently before any lock exists at all) to immediately after the new L2
  `SELECT ... FOR UPDATE`. This is the "re-check after acquiring" pattern
  ADR 0020's own table already requires generically ("Double-spend →
  `SELECT ... FOR UPDATE` (or `SERIALIZABLE`) balance check inside the
  posting transaction... the check and the debit are atomic, not two
  steps").
- **No `SERIALIZABLE` needed.** ADR 0082 §2.4 and §5.4 explicitly reject
  `SERIALIZABLE` platform-wide for financial transactions (no retry
  framework exists); a single-row `FOR UPDATE` is the established idiom
  here (`casino.postRollback`, `withdrawal`'s five `lockRequestForUpdate`
  sites) and is sufficient because the invariant is entirely local to one
  original transaction's reversal state — there is no multi-row
  read/decide/write spanning two independently-locked rows the way a
  cross-wallet conversion would need.

### 3.2 RLS

No change needed. `receiveDepositReversalCallback` already runs inside
`tx`, which the caller (`newPaymentWebhookHandler`) opens via
`deps.DB.WithTenant(r.Context(), t.ID, ...)` where `t.ID` is resolved
server-side from the tenant **slug in the URL path**, verified against
`identity.GetTenantBySlug` and `t.Status == "active"`
(`deposit_handlers.go:267-296`) — never from the payload, consistent with
CLAUDE.md's "tenant_id is authoritative from server-side authenticated
context only." `ledger_transactions` carries `FORCE ROW LEVEL SECURITY`
(migration `0021_create_ledger_transactions.up.sql:57-58`) with a single
`tenant_isolation` policy scoped by `app.tenant_id`
(`:65-68`). The new `SELECT ... FOR UPDATE` runs under the same
already-tenant-scoped `tx`, so RLS transparently confines the lock to the
caller's own tenant's row — a cross-tenant `original.LedgerTransactionID`
cannot be locked (RLS would already have returned zero rows for
`loadDepositIntentByProviderRef` in that case, per its own doc comment
`:376-382`, before this new lock is ever reached). No RLS policy change is
required.

### 3.3 The loser's outcome

- **Same reference (redelivery of the identical reversal).** Both
  transactions serialize at the new L2 lock; the second one to acquire it
  re-checks "already reversed?" and finds a row whose `(provider_id,
  provider_tx_id)` **matches** its own event — this is the existing
  `IS DISTINCT FROM $2 OR IS DISTINCT FROM $3` exclusion (`:988`, F-7's own
  fix), so `alreadyReversed` is `false` for its own reference, and it falls
  through to `ledger.Post` with the **same** idempotency key the winner
  used. `Post`'s own `(tenant_id, idempotency_key)` conflict path fires,
  the canonical-payload compare (`replay.go`) finds an exact match, and
  the loser gets `PostResult{AlreadyPosted: true}` with the winner's
  transaction id — i.e. **idempotent success**, unchanged from today's
  (already-correct, F-7-fixed) behaviour for this case. The L2 lock does
  not change this path at all; it only removes the window in which a
  *different* reference could race in ahead of it.
- **Different reference (the actual PAY-REV-1 scenario).** The loser's
  re-check (now correctly serialized behind the L2 lock) finds a
  committed reversal under a *different* `(provider_id, provider_tx_id)`,
  so `alreadyReversed = true`, and it returns the existing
  `ErrDepositAlreadyReversed` (`:994`) **before ever calling
  `ledger.Post`** — no ledger row is attempted, so there is nothing for the
  new unique index to even need to catch in the normal (lock-respecting)
  path; the index exists purely as defense-in-depth for a hypothetical
  future writer that bypasses this function's own lock.
- **HTTP contract.** `newPaymentWebhookHandler` today has **no explicit
  mapping** for `ErrDepositAlreadyReversed` — it falls through to the
  generic `err != nil` branch (`:350-353`), which logs
  `payment_webhook_failed` and returns a **500** (`apierror.CodeInternal`).
  This is a genuine, pre-existing (not new) contract defect independent of
  PAY-REV-1's race: a *sequential*, correctly-detected second reversal
  attempt already gets a misleading 500 today. The Stage 10.1 fix should
  add an explicit `errors.Is(err, payments.ErrDepositAlreadyReversed)`
  branch mapping to `apierror.CodeConflict` (409), matching the sibling
  `ErrCallbackPayloadMismatch` handling immediately above it
  (`:340-349`) and the casino `ErrAlreadyRolledBack` precedent. This is a
  small, clearly in-scope caller-side fix (not a new architectural
  decision) needed so the concurrency fix's loser gets a correct, testable
  HTTP status rather than a spurious 500 that would make the new
  regression test's HTTP-level assertion (if any) awkward. The
  player-facing simulation route has its own `writeDepositCallbackError`
  (`payment_deposit_simulation_handlers.go:216-249`), which **also** lacks
  an `ErrDepositAlreadyReversed` branch, but as established in §1.2 that
  route can never emit a reversal event at all, so the gap there is inert
  and out of scope (may be noted as a latent inconsistency, not fixed).
- **Interaction with `ErrIdempotencyPayloadMismatch`.** Unreachable on the
  loser's path once the L2 lock and its re-check are in place: the loser
  never reaches `ledger.Post` with a *different* payload under the
  *same* key (that would require the same `providerID:reversalRef` string,
  which by hypothesis differs between the two concurrent reversals in the
  PAY-REV-1 scenario). `ErrIdempotencyPayloadMismatch` remains reachable
  only for its existing, narrower case (site #20 in the F-7 audit: a
  *sequential* redelivery of one reversal reference that names a
  *different* original than the first delivery of that same reference —
  a payload-level attack/bug on one key, not the concurrency race this
  item fixes). No change to that mapping is needed.

### 3.4 Deadlock analysis

- **Against concurrent deposit success on the same wallet (LOCK-1b,
  ADR 0082 §1.6).** `postDepositSuccess` takes **no** L1/L2 lock at all —
  confirmed by ADR 0082 §4.5 ("No code change required... neither the
  deposit nor the reversal path takes any lock outside `Post`") and by
  re-reading `postDepositSuccess` (`orchestrator.go:704-779`): it goes
  straight to `GetOrCreateAccounts` then `ledger.Post`. The new L2 lock in
  the reversal path is on the **original deposit's own**
  `ledger_transactions` row, which a concurrent *new* deposit (for a
  *different* `deposit_intents`/ledger transaction) never touches. Two
  transactions cannot deadlock over a resource only one of them ever
  acquires. If the concurrent deposit success is confirming the **same**
  original that is simultaneously being reversed (a genuinely pathological
  redelivery scenario — a "success" callback landing after a reversal is
  already in flight for the same reference), `postDepositSuccess`'s own
  short-circuit (`intent.Status == DepositIntentSucceeded` → no-op,
  `:705-707`) and `deposit_intents`' single-row nature mean there is at
  most one deposit-success writer per intent, and it does not lock
  `ledger_transactions` at all — so still no cycle.
- **Against withdrawal on the same wallet.** Withdrawal's own L1 locks
  (`withdrawal_requests` row `FOR UPDATE`, per ADR 0082 §1.7) are on a
  **different table** than `ledger_transactions`, and withdrawal never
  locks a `ledger_transactions` row (ADR 0082's own inventory, §1.7, lists
  no L2 site for withdrawal). No shared resource, no cycle.
- **Against a second, distinct-reference reversal of the *same* original
  (the case this fix targets).** Both acquire the L2 lock in the **same
  order** (both target the identical row — `original.LedgerTransactionID`
  — since both callbacks name the same `OriginalProviderReference`), so
  this degenerates to ordinary lock contention (one waits, the other
  proceeds), never a cycle — a single shared resource acquired by two
  transactions in the same order cannot deadlock by definition.
- **Against `casino_rollback`/other domains.** `deposit_reversal`'s new L2
  lock only ever targets a `deposit`-type original (payments never
  reverses a casino/sportsbook/bonus transaction and vice versa — the
  original is looked up via `deposit_intents.provider_reference`, a
  payments-only table), so there is no cross-domain row overlap to analyze
  a cycle against.
- **New unique-index insertion wait (mechanism 3 of ADR 0082's taxonomy).**
  The new partial unique index on `(reverses_transaction_id) WHERE
  transaction_type='deposit_reversal'` introduces a new insertion-wait
  point, but — given the L2 lock above already serializes the two
  concurrent reversal attempts on the *same* original before either
  reaches `ledger.Post` — the second transaction is turned away by the
  Go-level `alreadyReversed` check (an ordinary `SELECT`, not an insert)
  long before it would ever attempt the conflicting insert. The index's
  insertion-wait scenario is therefore only reachable by a hypothetical
  future writer that skips this function's own L2 lock, which by
  definition is not analyzed here as part of *this* transaction's
  behaviour (a new writer would need its own ADR 0082-conformant lock
  ordering review at the time it is added).

**Conclusion:** the fix introduces no new deadlock risk under ADR 0082's
model; it is a straightforward, single-new-lock-site addition of the same
class and shape as the existing, already-reviewed `casino.postRollback` L2
lock.

---

## 4. Idempotency model / F-7 re-audit of every reversal caller

Re-auditing per the F-7 audit's own method (site #20 and §6.2's
"observation"), against the fixed code:

| Scenario | Behaviour after the fix |
|---|---|
| Legitimate retry: same reversal reference, same payload (amount/asset unchanged) | **Idempotent success**, unchanged — falls through the (now correctly ordered) already-reversed check to `ledger.Post`, whose canonical-payload compare (`replay.go`) confirms an exact match and returns `AlreadyPosted: true`. Confirms the Stage 10 F-7 "same-reference redelivery is idempotent" change (`:980-984`'s comment) **remains correct** under the new lock — the lock only changes *when* the check runs, not what it decides for this case. |
| Same reference, different payload (amount/asset changed, or naming a different original) | **`ErrCallbackPayloadMismatch`** (wrapping `ledger.ErrIdempotencyPayloadMismatch`), per the existing `:1021-1028` mapping — untouched by this fix. If the amount/asset differs from the *original's own* recorded amount/asset, the earlier `:964-971` check (unchanged, now running after the L2 lock — see below) rejects it even before `Post` is reached, with `ErrCallbackProviderMismatch`. |
| Different reference naming an already-reversed original (sequential, no concurrency) | **Rejected, no posting** — `ErrDepositAlreadyReversed`, as today, now returned deterministically under the L2 lock rather than via an unlocked read (behaviourally identical for the sequential case; the lock changes nothing observable here, only removes a race window that sequential execution never exercises anyway). |
| Concurrent, different references, same original | **Exactly one posts.** This is the scenario the fix exists for: the L2 lock forces total ordering; the loser's post-lock re-check sees the winner's committed row and returns `ErrDepositAlreadyReversed`. **This is the core behavioural change.** |
| Concurrent, same reference | **One posts, the other replays** (`AlreadyPosted: true`), unchanged from today — see row 1. The L2 lock serializes them the same way, but both take the identical idempotent-success branch once serialized. |

**Where the amount/asset check (`:964-971`) must move.** Currently it runs
*before* the already-reversed check and *before* any lock. It should move
to run **after** the new L2 lock is acquired (or be left where it is,
immediately followed by the lock, then the re-check) — its own
correctness does not depend on the lock (it only compares the callback's
claimed amount/asset against `original.Amount`/`original.AssetCode`, both
already-immutable fields read once at `:932`), so this is a minor
placement/readability decision for the implementer, not a correctness
requirement. Recommendation: acquire the L2 lock **immediately** after the
tombstone branch, before *both* the amount/asset check and the
already-reversed check, so every subsequent read in this function is under
the lock — simplest to reason about and matches `casino.postRollback`'s
own shape (lock first, then every subsequent validation).

**Confirms:** the Stage 10 same-reference-redelivery change
(`IS DISTINCT FROM`, F-7 remediation) remains correct and is not
disturbed by this fix — it operates entirely within the "same reference"
branch, which the L2 lock does not alter.

---

## 5. Migration strategy

### 5.1 Numbering

Next free migration is **`0092`** (confirmed: `migrations/` tips at
`0091_sportsbook_settlement.{up,down}.sql`; no `0092` exists yet). Name
suggestion: `0092_deposit_reversal_uniqueness.up.sql` /
`.down.sql`.

### 5.2 Pre-flight duplicate detection

- **Must run inside the migration**, before the `CREATE UNIQUE INDEX`,
  and must **refuse loudly** (raise an exception that aborts the
  migration) if any duplicate is found — never silently skip or dedupe
  existing rows (CLAUDE.md: "Corrections are compensating entries, never
  edits or deletions of historical entries" — a migration must not delete
  or merge a duplicate `ledger_transactions` row under any
  circumstances; that decision requires a human-reviewed compensating
  entry, not a migration).
- **RLS-proof requirement.** `ledger_transactions` has `FORCE ROW LEVEL
  SECURITY` (`0021...up.sql:58`), which — per Postgres's documented
  semantics — means even the table owner is subject to the row policy
  **except** when the connection has `BYPASSRLS`, or when running as a
  literal superuser. Migrations in this codebase run via whichever role
  `db.Pool.MigrateUp` connects as; the Stage 10 W0 completion report
  states plainly that "the CI/dev-only `igaming_test_admin` role" was
  created with **`NOSUPERUSER`, `NOBYPASSRLS`** specifically so that
  application roles are never granted elevated bypass. **This is the
  single most important correctness risk for this migration:** a
  duplicate-detection query written as an ordinary `SELECT ...
  reverses_transaction_id, count(*) ... GROUP BY ... HAVING count(*) > 1`
  run under a tenant-scoped or non-bypassing role would only see **one
  tenant's** rows (or **zero**, if `app.tenant_id` is unset, since the
  policy's `USING` clause is `tenant_id =
  NULLIF(current_setting('app.tenant_id', true), '')::uuid` — an unset
  setting yields `NULL = tenant_id`, which is never true, so the policy
  hides **every** row from a connection with no tenant context set). A
  migration must therefore either: (a) run as a role that genuinely has
  `BYPASSRLS` (this codebase's actual production/migration role — check
  `deploy/init-app-role.sql`/whatever role `MigrateUp` uses in prod,
  which is a decision for the implementer to confirm against the real
  deploy role, not assumed here), or (b) explicitly loop over every
  tenant, setting `app.tenant_id` per iteration, to scan each tenant's
  rows in turn — before running the `CREATE UNIQUE INDEX`.
- **`CREATE UNIQUE INDEX` itself scans every row regardless of RLS
  policy**, per Postgres's own documented behaviour: index builds run as
  the table owner / with elevated internal access and are **not** subject
  to row security policies (RLS restricts DML and `SELECT`, not internal
  maintenance operations like index builds) — so the index-creation step
  itself is not at risk of silently skipping rows the way an ordinary
  `SELECT`-based pre-check under a non-bypassing role would be. This means
  the *danger* is specifically in the **pre-flight duplicate-detection
  query** the migration author writes to produce a friendly refusal
  message — if that query is RLS-scoped and silently sees zero tenants'
  worth of duplicates, the migration would proceed to `CREATE UNIQUE
  INDEX`, which would **then legitimately fail** with a generic
  Postgres unique-violation error (not the friendly, named refusal the
  task requires) if duplicates do exist. **Net requirement: the
  duplicate pre-check must not rely on RLS filtering at all** — either run
  it with an explicit `SET LOCAL row_security = off` (requires the
  connecting role to have the privilege to do so; another reason to
  confirm the actual migration-connection role's privileges) or iterate
  tenants explicitly. Recommend the implementer verify empirically, as
  part of implementation (not planning): connect as the actual migration
  role in a scratch database, seed a synthetic cross-tenant duplicate, and
  confirm the chosen pre-check query actually sees it.
- **`CONCURRENTLY` vs. inside the migration transaction.** `CREATE UNIQUE
  INDEX CONCURRENTLY` **cannot run inside a transaction block** at all —
  it is a hard Postgres restriction, and every migration file in this
  repository presumably runs inside the tool's own wrapping transaction
  (standard for a `.up.sql`/`.down.sql` pair using this codebase's
  migration runner). Given: (a) `ledger_transactions` at Stage 10 volumes
  is not large enough to make a brief exclusive lock operationally risky
  (dev/synthetic data only, per CLAUDE.md's environment-safety rule — no
  production data or production deploy is in scope for this stage), and
  (b) the existing precedent — migration `0091`'s own down-migration
  refusal pattern and every other migration in this tree runs
  transactionally — **the recommendation is a plain (non-`CONCURRENTLY`)
  `CREATE UNIQUE INDEX` inside the migration's normal transaction**,
  accepting a brief `ACCESS EXCLUSIVE`-adjacent lock (a plain unique index
  build takes `SHARE` lock on the table, blocking writes but not reads,
  for the build's duration) as acceptable for a table this size at this
  stage. If a future production deploy needs a hot-path migration on a
  large `ledger_transactions` table, that is a separate, later, explicitly
  human-authorized production-migration-strategy decision (per the Stage
  10 completion report's own note: "Migration 0091 has not been applied to
  staging, and does not need to be for Stage 10" — the same posture
  applies here).
- **Refusal message.** Model on migration `0091`'s own down-migration
  refusal ("refuses once any settlement evidence exists" — same repo
  convention). Suggested shape: a `DO $$ ... RAISE EXCEPTION 'migration
  0092: % duplicate deposit_reversal(s) found for reverses_transaction_id
  %; a deposit was reversed more than once (PAY-REV-1) — resolve via a
  reviewed compensating entry before re-running this migration', ... END
  $$;` block, naming the count and (if feasible without leaking excessive
  detail into a migration log) the affected transaction ids, run before
  the `CREATE UNIQUE INDEX` statement.

### 5.3 Down migration

Straightforward: `DROP INDEX IF EXISTS ...` for both the new unique index
and (if item 2's optional casino extension is taken) its sibling. No
`sportsbook_bet_settlements`-style "refuses once evidence exists" pattern
is needed here, because dropping a *uniqueness constraint* is always safe
to reverse (it only ever *prevented* rows from being written; removing the
constraint cannot corrupt existing data, unlike migration `0091`'s
down-migration, which would have to decide what to do with existing
settlement history rows).

### 5.4 Effect on migration-chain tip-pin tests

Grep confirms these integration test files reference or depend on `0091`
being the current tip (via `MigrateUp`/dedicated per-migration test
files, or a shared migration-count assumption):

- `internal/sportsbook/settlement_migration_0091_integration_test.go`
  (directly locates/depends on `0091_sportsbook_settlement.up.sql`
  by filename, `:39-40` — will be unaffected by a purely additive `0092`
  migration, since it names its own file explicitly rather than asserting
  "0091 is the last migration").
- `internal/sportsbook/settlement_db_constraints_integration_test.go`,
  `settlement_sql_branches_integration_test.go`,
  `settlement_readpath_integration_test.go`,
  `migration_0082_immutability_integration_test.go` — all found by the
  broad grep for "0091" but need per-file inspection (not done in this
  planning pass) to confirm whether any asserts a **total migration
  count** or **latest-migration-number** rather than just depending on
  0091's schema being present. A targeted grep specifically for
  `len(...)==91`, a hardcoded `"0091"` used as "the tip," or a
  `MigrateUp` call with no explicit target directory (implying "run every
  migration up to whatever the tip is") is recommended as an
  implementation-time check — **this planning pass did not find any such
  pattern** (no hits for `migrationTip`/`tipVersion`/`latestMigrationNumber`
  constants), suggesting these tests key off specific table/column
  existence (schema-shape assertions) rather than a numeric tip pin, which
  would make them robust to a new additive `0092`. This should be
  **verified, not assumed**, before implementation — flagged as a
  concrete pre-implementation task, not resolved here.
- `internal/risk/cumulative_sportsbook_test.go`,
  `internal/jurisdiction/migration_0075_integration_test.go`,
  `internal/jurisdiction/migration_0077_integration_test.go`,
  `internal/bonus/wave3_phase2_migrations_integration_test.go`,
  `internal/db/runtime_role_separation_test.go`,
  `internal/operatingmarket/qa_migration_rls_survives_failed_rollback_test.go`,
  `internal/operatingmarket/migration_0076_integration_test.go` — these
  reference "0091" only incidentally (e.g. a comment, or an unrelated
  numeric literal); each names its **own** migration number in its
  filename and is very unlikely to assert a global tip. Same
  recommendation: spot-check at implementation time rather than assume.

**Expectation:** a purely additive migration (`0092`, new indexes only, no
column/table changes) should not break any of these, since none of them
appears (from this grep pass) to assert "0091 is the last migration that
exists" as opposed to "0091's own schema is present." This must be
confirmed by actually running the full suite after adding `0092`, which is
an implementation-stage activity, not something this planning-only pass
can execute.

### 5.5 Locking cost

A plain `CREATE UNIQUE INDEX` on `ledger_transactions` takes a `SHARE`
lock for the duration of the build (blocking concurrent writers to the
table, not readers) — acceptable given dev/synthetic data volumes and
CLAUDE.md's explicit scoping of this stage to non-production environments.
Two new partial indexes (deposit-reversal-scoped, and optionally
casino-rollback-scoped) are both small relative to the full table (each
indexes only rows of one `transaction_type`), so the build itself is fast
even before considering volume.

---

## 6. Audit, API, RLS, rollback, tests

### 6.1 Audit

- **The winning path is already fully audited** (`deposit.reversed`,
  `:1033-1043`).
- **The rejected concurrent (or sequential) attempt is currently NOT
  audited** — `receiveDepositReversalCallback` returns
  `ErrDepositAlreadyReversed` (`:994`) with no `audit.Record` call at all.
  This is a **pre-existing gap**, not introduced by this fix, but it
  becomes directly testable/visible once the race is closed (today the
  race means the "rejected" path is rarely exercised at all in practice —
  it either wins or double-posts; once fixed, it will be exercised
  routinely under load). **Recommendation (in-scope, small): add an audit
  record for the denial** — `Action: "deposit.reversal_rejected_already_reversed"`,
  `Outcome: audit.OutcomeDenied`, carrying `provider_id`,
  `original_ledger_transaction_id`, and the rejected reversal's own
  `provider_reference` (never the *other*, already-posted reversal's
  reference, to avoid leaking one PSP delivery's identifier into a
  different one's audit trail without cause — though since this is all
  server-side/tenant-scoped audit, not player-facing, this is a minor
  consideration, included only for completeness). This mirrors the
  existing `deposit.declined`/`deposit.ambiguous` audit shape already in
  this file.
- **Alert.** Recommend an integrity-style alert log line (matching the
  existing `payment_webhook_integrity_alert_payload_mismatch` pattern) —
  something like `payment_webhook_reversal_duplicate_attempt_rejected` —
  logged from the HTTP handler once the `ErrDepositAlreadyReversed`
  mapping is added (§3.3), so operational monitoring can see this as a
  named class of event rather than only reaching it via generic audit-log
  querying. This is an incremental addition to existing observability
  conventions, not a new mechanism.

### 6.2 API impact

- New explicit branch in `newPaymentWebhookHandler`
  (`deposit_handlers.go`) for `errors.Is(err, payments.ErrDepositAlreadyReversed)`
  → `apierror.CodeConflict` (409), placed alongside the existing
  `ErrCallbackPayloadMismatch` branch (§3.3). This is a behaviour
  **correction** (today this case falls through to a 500), not a new
  contract — genuinely in-scope and low-risk.
- No change needed to `newSimulateDepositCallbackHandler`'s error mapping
  (§1.2 — that route cannot emit a reversal at all).
- No response-body shape change on the success path.

### 6.3 RLS / tenant isolation impact

None beyond what §3.2 already covers — the new lock and the new index
both operate entirely within the existing tenant-scoped transaction and
the existing `FORCE ROW LEVEL SECURITY` policy on `ledger_transactions`.
No new policy, no new RLS-bypassing code path.

### 6.4 Rollback strategy

- **Code revert with index kept is safe and recommended as the rollback
  posture**, mirroring the general principle that a uniqueness constraint
  never corrupts data by existing — if the Go-level lock/logic change
  needs to be reverted for any reason, the database index alone continues
  to prevent a *second* row from ever being written (the old, racy code
  would simply get an ordinary constraint-violation error on the loser,
  surfaced as an unhandled 500 rather than a clean 409 — a regression in
  polish, not in the financial invariant). This is explicitly the "defense
  in depth" framing the W1 sign-off itself uses for the index.
- **Index revert without code revert would reopen the race** and must
  not be done independently; if the migration must be rolled back (e.g. a
  duplicate is discovered post-hoc that the pre-flight check missed due to
  an RLS-scoping bug — see §5.2's own risk callout), the code-level lock
  still prevents *new* races even without the index, so a down-migration
  in isolation degrades the system back to "lock-protected, no DB
  backstop" rather than back to the original defect, provided the code fix
  ships and stays.

### 6.5 Test list

Following the Stage 10 W0 "deterministic concurrency, blocker-PID-scoped"
rule (`docs/governance/stage-10-completion-report.md` item 1: "lock-wait
polling is now scoped to each test's own blocker PID") and the probe shape
from the W1 ledger-finance sign-off (§4: "ran reversal A to completion
inside tx1 without committing; started tx2 with reversal B for the same
deposit; polled `pg_stat_activity` until tx2's backend was blocked on a
lock inside Post... then committed tx1"), adapted to block on the **new
L2 lock** rather than inside `Post`:

**Concurrency (deterministic, `internal/payments`, new file e.g.
`payrev1_concurrency_integration_test.go`):**

1. `TestPayRev1_ConcurrentDistinctReferenceReversals_ExactlyOnePosts` —
   the core regression test, in the probe's exact shape: reversal A (ref
   R1) begins in tx1 and is deliberately held just past its L2 lock
   acquisition (e.g. via a test-only hook, or by holding the row lock from
   a separate blocker transaction the way `loHoldProjectionRow` holds a
   projection row in the existing lock-order harness — reuse/extend that
   harness's pattern: a `loBlocker`-style helper that takes the L2 lock on
   the original transaction row and waits); reversal B (ref R2, same
   original) starts in tx2 and is polled via `pg_stat_activity` until
   blocked specifically on tx1's PID (blocker-PID-scoped, per the W0
   rule — never a bare `pg_locks` scan with no PID filter); tx1 commits;
   assert: exactly one `deposit_reversal` row exists for the original,
   `player_cash` reflects exactly one reversal, the loser returns
   `ErrDepositAlreadyReversed`, and `SUM(DEBITS)==SUM(CREDITS)` holds.
   **This test must be shown to fail on the pre-fix code** (per ADR 0082
   §6's own methodology: "a test that cannot be shown to fail before the
   fix proves nothing") — run it against a stashed pre-fix `orchestrator.go`
   to confirm it reproduces the −1,000/-2,000 pattern the sign-off's
   deleted probe found, then restore the fix and confirm it passes.
2. `TestPayRev1_ConcurrentIdenticalReferenceReversals_OnePostsOneReplays`
   — same harness, both racers use reference R1; assert both return
   success, the same `LedgerTransactionID`, and exactly one
   `deposit_reversal` row (proves the L2 lock does not break the existing,
   correct same-reference idempotent-replay behaviour from F-7).
3. `TestPayRev1_SequentialDistinctReferenceReversal_Rejected` — no
   concurrency; reversal A completes, reversal B (different reference,
   same original) is attempted afterward; assert `ErrDepositAlreadyReversed`,
   zero new ledger rows, and (if the audit addition from §6.1 is
   implemented) an audit record with the denial outcome.
4. `TestPayRev1_SequentialSameReferenceRedelivery_StillIdempotent` —
   regression guard for the exact F-7 same-reference fix
   (`:980-984`'s comment): confirms this fix does not reintroduce the
   pre-F-7 bug where a sequential same-reference redelivery was wrongly
   rejected.
5. `TestPayRev1_LockOrder_NoDeadlockAgainstConcurrentNewDeposit` — LOCK-1b
   sibling: the existing
   `TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock`
   (`lockorder_integration_test.go:106`) already covers "deposit of a
   *different* intent vs. reversal of *this* one" and must be re-run
   unmodified after the fix to confirm no new deadlock is introduced (per
   §3.4's analysis) — add to the regression list as "must still pass,
   unmodified," not necessarily a new test.
6. **Database-level:** a direct-SQL test proving the new partial unique
   index actually rejects a manually-constructed duplicate insert (bypass
   the Go layer entirely, insert two `deposit_reversal` rows with the same
   `reverses_transaction_id` directly via SQL under the test's own tenant
   scope) — the "backstop, not primary control" property from §2.3 must
   be independently verified at the schema level, not only inferred from
   the Go-level test passing.
7. **Migration test:** `TestMigration0092_RefusesWithExistingDuplicates` —
   seed a duplicate via direct SQL against a **pre-0092** schema, then run
   the `0092` migration and assert it fails with the named refusal
   message (mirrors migration `0091`'s own down-migration refusal test
   pattern, `settlement_migration_0091_integration_test.go`), plus a
   companion test confirming `0092` succeeds cleanly against a
   non-duplicate dataset and the down-migration cleanly drops the new
   index(es).
8. **HTTP-level:** extend the existing webhook handler test suite (or add
   one) asserting `ErrDepositAlreadyReversed` now maps to 409, not 500,
   confirming §3.3/§6.2's contract fix.
9. **Audit-level:** if §6.1's audit addition is implemented, a test
   asserting the denied-reversal audit record's shape (actor, tenant,
   entity, outcome=denied), per CLAUDE.md's own audit-testing expectation.

All concurrency tests must use the blocker-PID-scoped polling helper
already established by the W0 flake fix and the existing
`internal/payments/lockorder_harness_test.go` primitives
(`loStartRacer`, `loRunABBA`, `loAssertNoDeadlock`) — extending that
harness with a new `loHoldLedgerTransactionRow`-style blocker (analogous
to the existing `loHoldProjectionRow`) is the natural, low-risk
implementation path, reusing infrastructure this same package already
built and reviewed for exactly this class of test.

---

## 7. Items that need a HUMAN decision (strict test applied)

Applying CLAUDE.md's own bar — "gambling licence decisions, jurisdiction
selection, provider contracts, production credentials, commercial
pricing, major irreversible architecture decisions, legal interpretation,
production launch authorization" — and being strict that an *engineering*
choice within an already-approved stage is **not** a human decision even
if consequential:

1. **Authorization to start Stage 10.1 at all.** The Stage 10 completion
   report already states this explicitly ("New decision requested:
   authorize a stage to fix PAY-REV-1") and this is the correct, narrow
   ask — a human must authorize starting the next stage (CLAUDE.md's
   stage-gate rule is itself an absolute human-approval gate, independent
   of PAY-REV-1's own content). **This is the one clear, load-bearing human
   decision this plan surfaces**, and it is already flagged upstream; this
   analysis does not add a new one on this point, only confirms it stands.
2. **NOT a human decision — deferred to engineering, decided in §2.2
   above:** whether the unique index is global or per-type. This is a
   reversible, ordinary database-design choice within the already-approved
   "fix PAY-REV-1" scope; `ledger-finance` and `architect` review (already
   named as required reviewers in the completion report) is the correct
   control, not a human business decision.
3. **NOT a human decision — deferred to engineering, decided in §2.2/§7
   scope note:** whether to extend the same partial-unique-index pattern
   to `casino_rollback` in the same migration. This is a scope-boundary
   engineering call (in-scope hardening vs. scope creep), properly
   resolved by `ledger-finance`/`code-reviewer`/`architect` sign-off during
   implementation, not by the human authorizing the stage.
4. **NOT a human decision:** the `OPEN DECISION` already on record in
   `financial-transaction-flows.md` §2 ("whether a deposit reversal that
   would drive `player_cash` negative is permitted or blocked/escalated")
   is a **pre-existing, separately-tracked** business decision (the OB-1
   sibling for payments) that PAY-REV-1's fix does not need to resolve —
   preventing the *double* reversal does not require deciding whether a
   *single, correct* reversal may leave `player_cash` negative (Stage 10.1
   should not silently fold this in; if the implementer finds it
   unavoidable to touch this behaviour while fixing PAY-REV-1, that is the
   moment to stop and flag it, but nothing in the root-cause analysis
   above requires touching it). Recorded here only to confirm it is
   correctly **out of scope**, not to reopen it.
5. **NOT a human decision:** exact wording of the migration's refusal
   message, the new audit action-name string, or the new alert name — all
   ordinary engineering/naming choices consistent with this codebase's
   existing conventions (§6.1/§6.2), reversible, and low-risk.

**Summary: exactly one human decision is in scope for this item — the
stage-authorization gate itself, already correctly identified in the
Stage 10 completion report.** Everything else analyzed above (index
scope, casino extension, migration mechanics, test design, audit/alert
naming, HTTP status mapping) is an ordinary, reversible engineering
decision within that stage's bounded scope, to be made by `payments` with
`ledger-finance` and `architect` review, per the completion report's own
"Owner: payments. Reviews: ledger-finance and architect" assignment.

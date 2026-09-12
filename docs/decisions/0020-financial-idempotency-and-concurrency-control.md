# ADR 0020 — Financial Idempotency and Concurrency Control

Status: Accepted (Stage 3A, architecture only — `NOT IMPLEMENTED`),
derived from Blueprint §4.2's idempotency requirement and ADR 0001,
extended in Stage 3A to specify idempotency and concurrency control
explicitly.

## Context

Every financial operation in `financial-transaction-flows.md` can be
retried (client retry, provider redelivery, network partition recovery)
and can race against a concurrent operation on the same wallet (two bets
in flight, a deposit and a withdrawal at once, two provider callbacks for
related events arriving out of order). ADR 0001 established that
idempotency is DB-enforced, not "check then insert" — this ADR specifies
exactly how, and how concurrency is controlled so the same guarantee holds
under simultaneous requests, not just sequential retries.

## Decision

### Idempotency key scope

- **Provider-originated transactions** (deposit confirmation, withdrawal-
  sent confirmation, casino bet/win, sportsbook settlement, custodian
  events): `UNIQUE (tenant_id, provider_id, provider_tx_id)` on
  `ledger_transactions`. This is the Blueprint's own stated mechanism
  ("or equivalent", per CLAUDE.md) and is treated as the default for
  anything with an external counterparty. **Every key in this ADR is
  tenant-scoped**; the reasoning (cross-tenant collision and the RLS
  unique-violation oracle) is recorded once, in
  `ledger-accounting-model.md` §3, and not repeated per key here.
- **Internally-originated transactions** (manual adjustment, bonus grant/
  conversion/forfeiture with no external provider): `UNIQUE
  (tenant_id, idempotency_key)`, where the key is generated once by the originating
  service (bonus-engine, admin action) at the moment the operation is
  first accepted, and reused verbatim on any retry of that same logical
  operation — never regenerated per attempt.
- **Client-initiated requests before a provider reference exists**
  (`InitiateDeposit`, `WithdrawalRequest` creation): a client-supplied or
  session-derived `idempotency_key` at the orchestration/workflow layer
  (`payment-orchestration.md` §8, `withdrawal-state-machine.md` §4) —
  this is a *different* key from the eventual `provider_tx_id`, since the
  provider reference doesn't exist until the provider responds; both keys
  independently prevent duplication at their respective layers.

### Exact-retry behavior

A second `INSERT` with the same key hits the unique constraint, fails at
the database level, and the handling code catches that specific
constraint-violation error and returns the **original** transaction's
result (looked up by the same key) rather than propagating the error to
the caller. This is symmetric across every flow — there is one shared
"idempotent insert" helper pattern, not a bespoke retry handler per flow.

**Postgres/pgx implementation note (implementability defect fixed here):**
in Postgres, once any statement in a transaction errors — including a
`23505 unique_violation` from the idempotent `INSERT` — that transaction
is aborted and every subsequent statement is rejected
(`current transaction is aborted, commands ignored until end of
transaction block`) until `ROLLBACK`. The "catch the error and look up the
original result" step above is therefore **not achievable in the same
outer transaction** unless the `INSERT` is wrapped in a `SAVEPOINT` first:
`SAVEPOINT` before the insert, and on a unique-violation,
`ROLLBACK TO SAVEPOINT` (which pgx's `Tx.Begin(ctx)` issues automatically
when called on an already-open `pgx.Tx`, giving a nested-transaction API)
before running the compare/lookup query, so the outer transaction (and any
locks/GUC settings it already holds, e.g. `app.tenant_id`) survives.
Equivalently, the whole outer transaction may simply be rolled back and the
lookup performed in a fresh transaction/connection — slightly simpler, at
the cost of one extra round trip on the (rare) conflict path only; the
non-conflicting fast path is unaffected either way. The shared
"idempotent insert" helper (below) must implement one of these two
patterns explicitly — a naive "catch the pgx error, then run another query
on the same `pgx.Tx`" implementation will fail with
`current transaction is aborted` on every single retry/duplicate, which
would otherwise only surface under concurrency-race testing, not normal
sequential development.

### Same-key-different-payload behavior

If a retried request arrives with the same idempotency key but a
**different** payload (e.g. a different amount — a client bug or an
attempted replay/tamper), the handler must **reject** the request rather
than either (a) silently applying the new payload or (b) silently
returning the old result as if it matched. This is checked by comparing a
hash of the semantically-relevant request fields (asset, amount, source/
destination) stored alongside the idempotency key at first-insert time.
`OPEN DECISION`: the exact set of fields included in that comparison hash
per transaction type is a Stage 3B implementation detail, not fixed here,
but the requirement itself (detect and reject mismatched replays, never
silently prefer either payload) is not optional.

### Concurrent-duplicate behavior

Two concurrent requests carrying the *same* idempotency key racing each
other: the database's unique constraint itself is the arbiter — exactly
one `INSERT` succeeds, the other fails the constraint check and follows
the exact-retry path above (look up and return the winner's result;
subject to the same savepoint/fresh-transaction requirement noted above).
No "check if exists, then insert" application-level pattern is used
anywhere, because that pattern has a race window a database constraint
does not. Note that Postgres itself serializes this race: the losing
`INSERT` blocks on the conflicting index entry until the winner's
transaction commits or rolls back, then either fails with
`unique_violation` (winner committed) or proceeds normally (winner rolled
back) — the loser's subsequent lookup query, run under `READ COMMITTED`
(or after a fresh `SAVEPOINT`/transaction as above), is guaranteed to see
the winner's already-committed row.

### Callback deduplication

Provider callback delivery (webhooks) is deduplicated at two layers,
deliberately redundant (§8 of `payment-orchestration.md`): the adapter's
own short-lived recently-seen-reference cache (a performance optimization,
avoiding a wasted DB round-trip for an obviously-immediate redelivery) and
the ledger's own unique constraint (the actual correctness guarantee,
never bypassed even if the cache layer is disabled/cleared/misses).

### Concurrency controls

- **Every financial state change is one database transaction** — a
  `LedgerTransaction` and all its `LedgerEntry` rows commit atomically or
  not at all (already stated in `ledger-accounting-model.md` §1.2,
  restated here as the concurrency baseline).
- **Insufficient-balance checks** (casino bet, sportsbook lock,
  withdrawal request) read the current balance **inside** the same
  database transaction that would post the debit, using `SELECT ... FOR
  UPDATE` on the relevant `wallet_balance_projection` row (or an
  equivalent serializable-transaction approach — see next point) so a
  second concurrent debit against the same wallet cannot both read
  "sufficient funds" before either commits. This directly prevents
  double-spend and a negative available balance where prohibited
  (`ledger-accounting-model.md` §6 invariant #15).

  Reading the *projection* here is not a violation of "the projection is
  never the authoritative read" (`ledger-accounting-model.md` §5) or of
  CLAUDE.md's "a cache is never read on the bet/settlement path, and the
  authoritative balance read happens inside the same database transaction
  as the write". The projection is not a cache: it is a Postgres row in the
  same database, updated in the *same transaction* as every
  `ledger_entries` insert that changes it (ADR 0019), so inside a
  transaction holding `FOR UPDATE` on that row its value is equal by
  construction to `SUM(ledger_entries)` for that account. The row serves
  two purposes at once — the serialization point for concurrent debits, and
  the balance figure itself. If Stage 3B ever weakens the
  same-transaction projection update (e.g. to an async maintainer), this
  paragraph stops being true and the sufficiency check must revert to a
  direct `SUM` over `ledger_entries`; that coupling is deliberate and must
  be called out in any proposal to change ADR 0019's projection write path.
  `ledger-finance` owns the final call on this reading.
- **`SERIALIZABLE` isolation** is used (rather than row locking alone) for
  any operation that reads/writes more than one account within a single
  transaction where a lock-ordering deadlock risk exists (e.g. a
  conversion operation touching two wallets) — Postgres's serializable
  isolation detects and aborts (for client-side retry) the losing
  transaction rather than requiring the application to hand-order locks
  correctly across every code path.
- **Callback races** (two settlement events for the same bet arriving out
  of order, or simultaneously from redundant provider delivery) are
  resolved by the same idempotency key that prevents duplicate posting —
  a race is just a special case of "concurrent duplicate," handled
  identically, not a separate mechanism.
- **Redis locks are never the sole correctness mechanism** for any
  financial operation — an optional Redis-based lock may exist purely as
  a performance optimization (reducing contention/wasted transaction
  aborts under `SERIALIZABLE`), but correctness holds with that lock
  entirely absent, verified by testing the flow with the optimization
  disabled (Stage 3B test requirement).

## How specific failure modes are prevented

| Failure mode | Prevention |
|---|---|
| Double-spend | `SELECT ... FOR UPDATE` (or `SERIALIZABLE`) balance check inside the posting transaction |
| Negative available balance where prohibited | Same as above — the check and the debit are atomic, not two steps |
| Duplicate settlement | `(tenant_id, provider_id, provider_tx_id)` unique constraint |
| Late-arriving original after a rollback for a never-seen transaction | Tombstone row occupying the same `(tenant_id, provider_id, provider_tx_id)` key — `ledger-accounting-model.md` §1.4 |
| Duplicate withdrawal | `WithdrawalRequest` creation idempotency key (`withdrawal-state-machine.md` §4) + the hold-posting happening in the same transaction as the request row's creation |
| Race-condition balance corruption | One DB transaction per financial state change; row locking or serializable isolation for any multi-account operation |

## Consequences

- Every Stage 3B posting code path shares one idempotent-insert/locking
  helper rather than reimplementing this per flow — a deviation is a
  `code-reviewer`/`ledger-finance` blocking finding, not a style
  preference.
- Test coverage for every flow in `financial-transaction-flows.md` must
  include: exact retry, same-key-different-payload, concurrent-duplicate,
  and a genuine concurrency race (two goroutines/connections) — per
  CLAUDE.md's financial testing requirements, restated here as this ADR's
  own gate.

## Owner

`ledger-finance`, concurrency mechanics co-reviewed by `backend`.

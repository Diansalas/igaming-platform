# 06 — Wallet and Ledger Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.2 (the part of the system
that "must be right on day one, because it is the part you can never
retrofit"). Owned by `ledger-finance`; changes require their sign-off per
`.claude/agents/ledger-finance.md`.

## Core rule

Append-only double-entry accounting. Never `UPDATE` a balance. Every
movement of money writes balanced entries; the balance is a projection
recomputed from entries, never authoritative on its own.

## Account types

`player_cash`, `player_bonus`, `player_locked` (open sportsbook stakes),
`house_gaming`, `provider_payable`, `psp_clearing`, `psp_reserve`,
`jackpot_contribution`, `promo_liability`, `manual_adjustment`. Every entry
carries `tenant_id`, currency, and a transaction group id.

## Money representation

Integers in minor units with a per-currency exponent. Given Anjouan
economics, crypto is almost certainly in scope, so `BIGINT` cents is
insufficient: use `NUMERIC(38,0)` plus a per-asset exponent (8 or 18).
Floating point anywhere near a balance is treated as a defect, not a
shortcut, and blocks review.

## Idempotency

A unique constraint on `(provider_id, provider_tx_id)` is what stops a
retried callback from double-debiting a player. This is the whole
correctness story for the wallet-callback path: retries are the *normal*
case (providers retry aggressively on any timeout), so the database
constraint — not "check then insert" application logic, which loses the
race under concurrency — makes a repeated call a no-op returning the same
answer.

Rollbacks write compensating entries, never deletions. A rollback for a
transaction never seen writes a tombstone, so the original arriving late
is rejected rather than applied.

## Worked example (from the Blueprint)

Bet: €10 stake, part bonus-funded.

| Account | Debit | Credit |
|---|---|---|
| player_cash | 4.00 | |
| player_bonus | 6.00 | |
| house_gaming | | 10.00 |

Win: €25, same round, bonus-funded stake means the win lands in bonus.

| Account | Debit | Credit |
|---|---|---|
| house_gaming | 25.00 | |
| player_bonus | | 25.00 |

Splitting the stake across cash/bonus **at the ledger level** (not a side
table) is what makes wagering progress, max-cashout caps, and bonus-cost
reporting fall out of the data instead of being reconstructed later.

## Where platforms lose money (explicit anti-pattern)

Caching balances in Redis and reading them on the bet path. A cache read
racing an in-flight write produces negative balances, free spins that
never debit, and a reconciliation gap discovered weeks later in a
partner's revenue report. The authoritative balance read happens inside
the same database transaction as the write. Redis holds sessions and
configuration — never money.

## Reconciliation

Balance = recomputed from ledger entries, checked hourly against the
materialized projection. Any non-zero drift is a P1 incident (Blueprint
§6 NFR table).

## Stage mapping

Core ledger + wallet service is Stage 3, and gates every other financial
flow (payments, bonus, sportsbook, casino wallet-callbacks) — nothing
downstream can be trusted without it existing first.

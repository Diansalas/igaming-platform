# ADR 0007 — Multi-Wallet Per Player, Not a Single Balance+Currency Field

Status: Accepted (human decision), supersedes the implicit single-balance
assumption in the Stage 0 draft of ADR 0001.

## Context

Stage 0's ledger design (ADR 0001) established append-only double-entry
accounting with account types like `player_cash`/`player_bonus` but did
not explicitly model multiple simultaneous currencies/assets per player.
The approved product direction requires a player to hold several wallets
at once (e.g. EUR, USD, BTC) with no fixed limit on how many.

## Decision

- A player has a distinct `Wallet` per asset (fiat currency or crypto
  asset), not one generic balance row carrying a currency field.
- Each `Wallet` has its own identity, its own set of ledger account types
  (`player_cash`, `player_bonus`, `player_locked`, etc.), its own balance
  projection, and its own transaction history.
- An `Asset` registry defines, per asset, its decimal exponent (EUR=2,
  USD=2, BTC=8, etc.) — never assumed to be 2 anywhere in code.
- Moving value between two wallets of different assets is never a direct
  balance mutation; it is an explicit `ConversionOperation` producing its
  own balanced ledger entries (source/destination amounts, exchange rate,
  rate source, fees, spread, provider reference, idempotency key — see
  `docs/architecture/06-wallet-ledger-architecture.md`).
- No exchange engine is implemented in Stage 1; only the schema/interface
  shape is established so it can be built correctly later (Stage 3+).

## Consequences

- Ledger schema work must include the `Asset` registry and `Wallet`
  identity tables from the start. Stage 1 delivered the `Asset` registry
  only (migrations 0003, 0006); the `Wallet` identity table and all
  ledger/posting logic are Stage 3 — an earlier draft of this ADR implied
  Stage 1 also delivered wallet tables, which was incorrect and has been
  corrected.
- Every money-handling code path must look up an asset's exponent from the
  registry rather than assuming 2 decimals — this is now a `code-reviewer`
  and `ledger-finance` review checkpoint.
- Reconciliation (hourly recompute vs. projection) runs per wallet, not
  per player — a drift in one asset's wallet must not be masked by another
  asset's wallet balancing out.

## Owner

`ledger-finance`.

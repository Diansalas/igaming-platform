# 06 — Wallet and Ledger Architecture Proposal

Status: Accepted core invariants (Blueprint §4.2, "the part you can never
retrofit"), **updated** to the human-approved multi-wallet model (see
`docs/decisions/0007-multi-wallet-per-player-model.md`). Owned by
`ledger-finance`; changes require their sign-off per
`.claude/agents/ledger-finance.md`.

## Core rule (unchanged)

Append-only double-entry accounting. Never `UPDATE` a balance. Every
movement of money writes balanced entries; the balance is a projection
recomputed from entries, never authoritative on its own.
`SUM(DEBITS) == SUM(CREDITS)` always holds, per asset.

## Multi-wallet model (revised from Stage 0)

Stage 0 drafted a single generic balance-per-account-type-per-currency
model. That is **superseded**: a player holds an explicit, separate
**wallet per asset** (currency or crypto asset), not one balance row with
a currency field. This is a human-approved architectural decision, not
discretionary.

```
Player
  └── Wallet (EUR)   — wallet_id, tenant_id, player_id, asset_code=EUR
  └── Wallet (USD)   — wallet_id, tenant_id, player_id, asset_code=USD
  └── Wallet (BTC)   — wallet_id, tenant_id, player_id, asset_code=BTC
  └── Wallet (...)   — no fixed limit on number of wallets/assets
```

Each `Wallet` carries: wallet identity (`wallet_id`), player identity,
tenant/brand context, asset/currency (`asset_code` → `Asset` registry),
status (active/frozen/closed), and is the anchor for a per-wallet set of
sub-accounts (see below), transaction history, and any wallet-level
limits/policies.

### Asset registry

```
Asset
  code (e.g. "EUR", "USD", "BTC", "USDT-TRC20")
  asset_type ("fiat" | "crypto")
  decimal_exponent (e.g. EUR=2, USD=2, BTC=8)
  display_name, active
```

The decimal exponent is **configurable per asset**, not assumed to be 2,
8, or any other single value. `NUMERIC(38,0)` minor-unit integers are
scaled by the asset's own exponent — EUR/USD entries are `amount * 10^2`,
BTC entries `amount * 10^8`, USDT (a common stablecoin with a
non-obviously-round precision) is `amount * 10^6` on the network the
platform's registry row targets. No code path assumes a fixed number of
decimals or a fixed set of "typical" values; the exponent is always
looked up from the `Asset` registry, never hardcoded.

### Account types (per wallet, unchanged set, now wallet-scoped)

`player_cash`, `player_bonus`, `player_locked` (open sportsbook stakes),
`house_gaming`, `provider_payable`, `psp_clearing`, `psp_reserve`,
`jackpot_contribution`, `promo_liability`, `manual_adjustment`. Every
ledger account now carries a `wallet_id` (nullable only for
platform/house-level accounts that are not player-owned, e.g.
`house_gaming` aggregated per tenant+asset rather than per player wallet)
in addition to `tenant_id` and asset/currency (inherited from the wallet).
Every entry carries a transaction group id.

A bet or bonus grant always resolves to a specific wallet first (by
asset), then to the account type within that wallet — there is no
cross-wallet implicit conversion at the ledger-entry level (see Cross-
currency operations below).

## Money representation (unchanged principle, generalized)

Integer minor units, with the per-currency/per-asset exponent read from
the `Asset` registry. `NUMERIC(38,0)` plus a per-asset exponent (typically
2 for major fiat, 8 or 18 for crypto). Floating point anywhere near a
balance or an asset amount is treated as a defect, not a shortcut, and
blocks review.

## Idempotency (unchanged)

A unique constraint on `(provider_id, provider_tx_id)` stops a retried
callback from double-debiting a player. Retries are the *normal* case;
the database constraint — not "check then insert" application logic —
makes a repeated call a no-op returning the same answer.

Rollbacks write compensating entries, never deletions. A rollback for a
transaction never seen writes a tombstone.

## Cross-currency operations (new — architecture only, not implemented in Stage 1)

Per the human-approved decision, moving value between two wallets of
different assets is **never** a simple balance adjustment. It is an
explicit, auditable `ConversionOperation`:

```
ConversionOperation
  id, tenant_id, player_id
  source_wallet_id, destination_wallet_id
  source_amount, destination_amount (both minor units, own exponents)
  exchange_rate, rate_source, rate_timestamp
  fee_amount, fee_asset, spread
  provider_reference, idempotency_key
  resulting_ledger_entry_group_id
  created_at, audit metadata
```

A `ConversionOperation` produces its own balanced ledger entries (debit
source wallet's relevant account, credit destination wallet's relevant
account, plus fee/spread entries as needed) — never a direct field
mutation on either wallet. **No exchange engine is built in Stage 1 or
Stage 2**; this schema exists so the capability can be added correctly
(Stage 3+) without a redesign. Any interim manual conversion (e.g. support-
assisted) must still go through this model, not an ad hoc balance edit.

## Worked example (unchanged mechanics, now wallet-scoped)

Bet: €10 stake from the player's EUR wallet, part bonus-funded.

| Account (EUR wallet) | Debit | Credit |
|---|---|---|
| player_cash | 4.00 | |
| player_bonus | 6.00 | |
| house_gaming | | 10.00 |

Win: €25, same round, same EUR wallet, bonus-funded stake means the win
lands in bonus.

| Account (EUR wallet) | Debit | Credit |
|---|---|---|
| house_gaming | 25.00 | |
| player_bonus | | 25.00 |

Splitting the stake across cash/bonus **at the ledger level** (not a side
table) is what makes wagering progress, max-cashout caps, and bonus-cost
reporting fall out of the data instead of being reconstructed later. This
is unchanged by the multi-wallet model — it now simply happens within one
wallet's set of accounts.

## Where platforms lose money (explicit anti-pattern, unchanged)

Caching balances in Redis and reading them on the bet path. The
authoritative balance read happens inside the same database transaction as
the write. Redis holds sessions and configuration — never money, for any
wallet or asset.

## Reconciliation (unchanged, now per wallet+asset)

Balance = recomputed from ledger entries, checked hourly against the
materialized projection, per wallet. Any non-zero drift is a P1 incident
(Blueprint §6 NFR table).

## Custody boundary (crypto wallets — see ADR 0008)

For crypto assets, the `Wallet`/ledger model above is strictly the
platform's internal accounting of player entitlement. It is backed by,
but distinct from, actual on-chain custody, which is delegated to an
external custody provider behind an abstraction (see
`07-payments-architecture.md` and
`docs/decisions/0008-crypto-custody-provider-abstraction.md`). Private
keys and blockchain signing never enter this ledger's trust boundary.

## Stage mapping

Stage 1 establishes only the `Asset` registry schema (migrations 0003 and
0006) and the general tenant/RLS foundation (`tenants`,
`tenant_jurisdiction_configs`, `tenant_config`) that any future
tenant-owned table, wallets included, will sit on top of. **No `wallets`
or ledger table exists yet** — an earlier draft of this document
overstated Stage 1's scope here; corrected after Stage 1 specialist
review. The `Wallet` identity table, the full ledger schema, idempotent
postings, and reconciliation jobs remain Stage 3, and gate every other
financial flow (payments, bonus, sportsbook, casino wallet-callbacks) —
nothing downstream can be trusted without it existing first.

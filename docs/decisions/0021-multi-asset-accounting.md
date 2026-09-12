# ADR 0021 — Multi-Asset Accounting Representation

Status: Accepted (Stage 3A, architecture only — `NOT IMPLEMENTED`),
formalizing ADR 0007's multi-wallet decision down to the exact numeric
representation and cross-asset movement architecture, which Stage 3A
re-evaluates explicitly rather than assuming carried forward unchanged.

## Context

ADR 0007 decided a player holds a distinct wallet per asset. Stage 3A
requires this stage to explicitly re-evaluate whether `NUMERIC(38,0)` +
per-asset exponent (from ADR 0001/0007) remains correct now that the full
ledger/account model (`ledger-accounting-model.md`) is being fixed, and to
resolve the cross-asset conversion architecture that ADR 0007 scoped but
did not fully design.

## Decision

### Numeric representation — re-evaluated, unchanged

`NUMERIC(38,0)` (a fixed-point, arbitrary-precision integer type in
PostgreSQL) for every `amount` column in the ledger model, combined with
`assets.decimal_exponent` looked up per asset — **confirmed, not
changed**. 38 digits of precision comfortably covers the largest
realistic minor-unit amount for any fiat or crypto asset the platform is
likely to support (a 128-bit integer maxes out around 3.4×10^38 total
range; `NUMERIC(38,0)` gives 38 *decimal* digits, i.e. up to
99999999999999999999999999999999999999 minor units — no realistic
balance or single transaction approaches this even for an 18-decimal
asset at a large denomination). No change to ADR 0001/0007 is warranted.

### Asset identity vs. currency/asset code vs. precision vs. display — explicit distinction

- **Asset identity**: the `assets.code` primary key (e.g. `"EUR"`,
  `"BTC"`, `"USDT-TRC20"`) — the only thing any ledger row references.
- **Currency/asset code** is the same value as asset identity in this
  model — there is deliberately no separate "currency code" vs. "asset
  code" split, because doing so would require every code path to know
  which of two codes to use for a given operation. A crypto asset's
  network variant (e.g. USDT on TRC20 vs. ERC20) is a **distinct** `Asset`
  row with its own code, not a sub-field of one "USDT" asset — they are
  economically and operationally distinct (different deposit addresses,
  different confirmation rules, not fungible with each other at the
  ledger level even though a `ConversionOperation` could bridge them, same
  as EUR→BTC).
- **Decimal precision/exponent**: `assets.decimal_exponent`, always looked
  up, never hardcoded (ADR 0007, restated as a standing rule).
- **Display amount**: a presentation-layer concern — `amount / 10^exponent`
  computed at the API/UI boundary for human display, never stored, never
  used in comparisons or arithmetic server-side beyond that one
  presentation step.
- **Authoritative integer representation**: the `NUMERIC(38,0)` minor-unit
  value stored in `ledger_entries.amount` — the only value any financial
  logic operates on.

### Cross-asset operations — `ConversionOperation`, not an exchange engine

Restated and finalized from `financial-domain-model.md`/
`06-wallet-ledger-architecture.md`, since Stage 3A requires this be
explicitly designed rather than left as a forward reference:

```
ConversionOperation
  id, tenant_id, player_account_id
  source_wallet_id, destination_wallet_id     -- must belong to the same player_account_id
  source_asset_code, destination_asset_code
  source_amount, destination_amount            -- both minor units, own exponents
  exchange_rate, rate_source, rate_timestamp
  fee_amount, fee_asset_code, spread
  provider_reference    -- if the rate/execution comes from an external FX/liquidity provider
  idempotency_key       -- UNIQUE (tenant_id, idempotency_key), tenant-scoped as for every other financial key (`ledger-accounting-model.md` §3)
  ledger_transaction_id -- the single LedgerTransaction this produces
  created_at
```

**Column types (stated explicitly, because "no floating point for money"
must extend to the fields *next to* money):** `source_amount`,
`destination_amount` and `fee_amount` are `NUMERIC(38,0)` minor units like
every other amount in the model. `exchange_rate` and `spread` are
**`NUMERIC`, never `FLOAT`/`DOUBLE`/`REAL`** — a float rate multiplied by
an exact integer amount reintroduces exactly the rounding
non-determinism `NUMERIC(38,0)` exists to eliminate, and makes the stored
rate unable to reproduce the stored `destination_amount`. Stage 3B should
store the rate at a fixed declared scale (e.g. `NUMERIC(38,18)`) or as an
explicit numerator/denominator pair; either way `destination_amount` must
be recomputable from `source_amount`, the stored rate and the stored
rounding rule, exactly, as a reconciliation check.

This shape **supersedes** the earlier sketch in
`06-wallet-ledger-architecture.md` ("Cross-currency operations") in two
respects, both consequences of Stage 2's identity model (ADR 0012) and
this stage's ledger model: it is keyed on `player_account_id`, not the
pre-Stage-2 `player_id`, and it points at a single
`ledger_transaction_id` rather than a `resulting_ledger_entry_group_id`
(there is no entry-group concept in the Stage 3A ledger —
`LedgerTransaction` *is* the grouping). Scope: tenant + player + the two
assets, recorded in `financial-domain-model.md`'s scoping table; both
wallets must belong to the same `player_account_id`, so brand is
implicitly single-valued and no `brand_id` column is carried (see that
document's brand denormalization rule).

- A `ConversionOperation` produces exactly **one** `LedgerTransaction`
  whose entries balance **per asset** (`ledger-accounting-model.md`
  invariant #1 is explicitly per-asset, not platform-wide). That
  requirement has a consequence worth stating plainly, because it is easy
  to get wrong: a debit on the source wallet and a credit
  on the destination wallet are in *different* assets and therefore do
  **not** balance each other. Each asset side needs its own counter-entry:

  | Leg | Entries |
  |---|---|
  | Source asset A | debit source wallet's `player_cash` (A) `X_A`; credit a tenant-level conversion clearing account (A) `X_A` |
  | Destination asset B | debit the tenant-level conversion clearing account (B) `X_B`; credit destination wallet's `player_cash` (B) `X_B` |

  Fees/spread post as additional entries within whichever asset they are
  charged in. The FX result (the platform's spread, and any rate movement
  between the two clearing legs) lands in the clearing accounts, which is
  where an FX gain/loss position becomes visible and reportable.

  `OPEN DECISION`: the conversion clearing account type does not exist in
  the Blueprint's ten-account list and is not invented here — the same gap
  recorded in `ledger-accounting-model.md` §2's cross-asset `OPEN
  DECISION`. Resolving it (new account type vs. reusing an existing
  house-level account) is a finance decision required **before** any
  conversion is implemented, and is why this ADR designs but does not build
  `ConversionOperation`.
- **Same-asset transfer** (moving value between two wallets denominated in
  the *same* asset, e.g. consolidating balances — if ever needed) is
  explicitly **not** a `ConversionOperation` — it would be a simple
  debit/credit pair with no rate/fee/spread fields relevant, and is
  `OPEN DECISION`/out of scope for Stage 3A since no flow in
  `financial-transaction-flows.md` currently requires same-asset,
  cross-wallet transfer (wallets are per-player-per-asset, so a same-asset
  transfer only makes sense player-to-player, which is not a Blueprint-
  described feature and is not designed here).
- **Rounding**: destination_amount is computed from source_amount ×
  exchange_rate, rounded to the destination asset's own `decimal_exponent`
  using a fixed rounding rule (`OPEN DECISION`: round-half-up vs.
  round-half-even is not specified by the Blueprint — a business/finance
  decision, not invented here) with any rounding residue absorbed as part
  of the platform's spread, never left as an unbalanced fractional unit.
- **Fees and spread**: modeled as explicit fields and, where they
  represent platform revenue, post to a fee-revenue account — `OPEN
  DECISION` on whether that's a new account type or folded into
  `house_gaming`; not resolved here since no Blueprint text addresses FX
  fee accounting specifically.
- **No exchange engine is built** — a Stage 3A scope decision.
  `rate_source` is an external abstraction (a rate-feed provider behind
  its own interface, out of scope for this ADR) that the
  `ConversionOperation` records a snapshot from at execution time
  (`rate_timestamp`); the platform never runs its own market-making or
  order-matching logic.

## Consequences

- No code path may assume a fixed decimal count (2) anywhere — this
  remains a standing `code-reviewer` checkpoint from ADR 0007, now backed
  by the explicit asset-identity/precision/display-amount distinction
  above so reviewers have a concrete checklist rather than a general
  reminder.
- `ConversionOperation` is designed but **not implemented** in Stage 3B
  unless a specific product requirement calls for it — no flow in
  `financial-transaction-flows.md` currently produces one; it exists so
  that if/when cross-asset movement is needed (e.g. a future "convert my
  BTC winnings to EUR" feature), the schema and invariant story are
  already settled.
- Two open business decisions (rounding direction, fee/spread revenue
  account) are explicitly deferred, not resolved by engineering judgment
  alone, since they affect displayed amounts and reported revenue.

## Owner

`ledger-finance`.

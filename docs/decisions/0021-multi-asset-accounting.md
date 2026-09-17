# ADR 0021 — Multi-Asset Accounting Representation

Status: Accepted (Stage 3A, architecture only — `NOT IMPLEMENTED`),
formalizing ADR 0007's multi-wallet decision down to the exact numeric
representation and cross-asset movement architecture, which Stage 3A
re-evaluates explicitly rather than assuming carried forward unchanged.

> **Stage 4H-B0-R3 — rounding/precision decision RESOLVED.** The
> rounding/precision `OPEN DECISION` that Stage 4H-B0-R1 identified as a
> blocking precondition for bonus amount computation (deposit-match,
> reload, cashback) has been decided by the human and validated by
> `ledger-finance`, `bonus-engine`, `risk`, `architect`, `security`, and
> `qa` against the existing architecture. **DS-1 = round-half-up (ties
> away from zero); DS-2 = round once, at the final monetary boundary,
> full `NUMERIC` precision until then, via an explicit shared function,
> never an implicit database cast; DS-3 = one platform-wide rule by
> default, with room for a future per-asset/per-jurisdiction override if
> a genuine legal/business need arises.** See "Rounding and precision —
> RESOLVED (Stage 4H-B0-R3)" below for the full recorded decision,
> validation findings, and the small number of non-blocking
> implementation-time clarifications flagged during validation. This
> closes the rounding/precision item as a blocker; `docs/decisions/
> 0032-bonus-accounting.md` §9, `docs/decisions/0035-retail-agent-
> network-accounting.md` §9.3 (physical-cash rounding) and §5.2
> (commission rates), and FX conversion (this ADR) all inherit the same
> resolved decision — each remains independently gated by its own other
> open items (see doc 27 §22's dependency graph), which this resolution
> does not itself unblock.
>
> *(Historical, Stage 4H-B0-R1): One item in this ADR was flagged as
> **not** a background open item — the rounding/precision decision was a
> blocking precondition for any bonus amount computation, specifically
> for deposit-match, reload and cashback bonuses. That blocking status is
> superseded by the resolution above; retained here for the historical
> record.*

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
  using the platform's shared rounding rule (**RESOLVED, Stage
  4H-B0-R3** — round-half-up, applied once at the final monetary
  boundary; see the dedicated section below) with any rounding residue
  absorbed as part of the platform's spread, never left as an unbalanced
  fractional unit. **Stage 4H-B0-R1**: this decision was never scoped to
  FX alone and was never a two-way "half-up vs. half-even" question — it
  was stated in full, with the complete option set and its blocking
  scope, in the dedicated section below, and has since been resolved
  there. As noted in doc 27 §26.5, recording this rule does not by
  itself unblock FX conversion, which remains separately gated by the
  conversion-clearing-account `OPEN DECISION` immediately below.
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

### Rounding and precision — RESOLVED (Stage 4H-B0-R3)

**RESOLVED.** The three-question decision below was answered by the human
and validated against the existing architecture by six specialists
(`ledger-finance`, `bonus-engine`, `risk`, `architect`, `security`, `qa`)
in Stage 4H-B0-R3. The original Q1/Q2/Q3 option set, added in full by
**Stage 4H-B0-R1** (`ledger-finance`), is preserved below unmodified as
the rationale record — every option was presented neutrally, none was
selected by any specialist, and the human's answer is recorded separately
in the "DECISION RECORDED" subsection after the gate statement.

#### What the decision actually is — three separable questions

The decision-maker must answer all three. Answering only the first leaves
the computation non-deterministic.

**(Q1) Rounding direction.** Which way does an amount go when the exact
mathematical result is not representable in the target asset's minor unit —
including, specifically, the exact-half case? Options in the table below.

**(Q2) Rounding point and precision.** Two sub-questions:

- *Where* is the rounding applied — once, at the final step, to the target
  asset's `assets.decimal_exponent` (2 for EUR, 8 or 18 for a crypto
  asset), with all intermediate arithmetic carried at full `NUMERIC`
  precision? Or at each intermediate step (e.g. percentage applied, then
  cap applied, then contribution weighting applied)? These give different
  answers, and "the obvious one" is not obvious to an implementer under
  time pressure, which is exactly why it must be written down.
- *What happens to the sub-minor-unit residue* — is it absorbed by the
  counterparty side of the same transaction (the platform's spread for FX,
  `bonus_expense` for a bonus, `agent_commission_expense` for commission),
  discarded (impossible — that breaks `SUM(DEBITS) == SUM(CREDITS)` and is
  not an available option), or **carried forward** in an explicit
  remainder-tracking mechanism until it accumulates to a whole minor unit?

**(Q3) Uniformity and scope.** Is there exactly one platform-wide rule, or
may the rule vary per asset, per jurisdiction, or per operation class
(credit to a player vs. debit from a player)? A per-jurisdiction override
is a real possibility because some jurisdictions prescribe cash-rounding
conventions (ADR 0035 §9.3), and a per-direction rule ("always in the
player's favour") is a real commercial policy, not a bug — but both must
be chosen deliberately, because each one multiplies the number of stored
rule identifiers a past amount must be reproducible against.

#### Where the answer binds (the full dependent set)

| Site | Computation | Document |
|---|---|---|
| FX conversion | `source_amount × exchange_rate` → destination exponent | this ADR |
| **Deposit-match bonus** | `deposit_amount × match_%`, then cap | ADR 0032 §9 |
| **Reload bonus** | `deposit_amount × reload_%`, then cap | ADR 0032 §9 |
| **Cashback bonus** | `net_loss × cashback_%`, then cap | ADR 0032 §9 |
| Wagering contribution weighting | `stake × contribution_%` | ADR 0032 §9 |
| Wagering requirement target | `bonus_amount × multiplier` | ADR 0032 §9 |
| Agent commission | `base × rate`, tiered, then cap | ADR 0035 §5.2 |
| Physical-cash rounding | round to the smallest circulating denomination — a **different** rounding target (denomination, not minor unit) that nonetheless inherits Q1's direction rather than inventing a second one | ADR 0035 §9.3 |
| Split prizes / `pro_rata` mission rewards | share of a pool | ADR 0032 open items 6/7 |

The three bolded rows are the ones that make this a gate rather than a
background item: they are three of the Bonus Engine's five first-slice
bonus types.

#### (Q1) Rounding direction — the options, presented neutrally

`X` = the exact mathematical result; the target is the asset's minor unit.

| Option | What it means concretely | Trade-off |
|---|---|---|
| **A. Round-half-up** (ties away from zero) | `€10.005` → `€10.01`; a 10% cashback on `€12.34` net loss → `€1.23` (`1.234`), on `€12.35` → `€1.24` (`1.235` ties up) | Simplest to implement and by far the easiest for a support agent to explain to a player ("we always round the half up"). Over a large number of tie cases it is very slightly biased in the direction of whoever the amount is credited to — i.e. it favours the player on a credit (bonus grant, cashback, win) and favours the operator on a deduction (stake, fee, commission taken). The bias is only on exact ties, so its practical magnitude depends entirely on how often ties occur, which for `× percentage` arithmetic on 2-decimal assets is not rare. |
| **B. Round-half-even** ("banker's rounding"; ties to the nearest even minor unit) | `€10.005` → `€10.00`; `€10.015` → `€10.02` | Eliminates the systematic tie bias of A in aggregate, which is why several accounting and statistical stacks default to it. Materially harder to explain at a support desk ("why did mine round down and my friend's round up?"), and it is the option most likely to be silently mis-implemented, because several languages' default `round()` is half-up while several others' (and IEEE 754's default mode) is half-even — a platform that mixes both produces amounts that differ by one minor unit depending on which service computed them. |
| **C. Round-down / truncate toward zero** (floor for positive amounts) | `€1.239` → `€1.23`; a 100% match on `€10.005` → `€10.00` | Fully deterministic, never overstates an obligation, and never grants a minor unit that was not earned. Systematically favours the operator on every credit to a player (bonuses are always a hair smaller than advertised) and favours the player on every deduction. Over high volumes the accumulated difference is real money and is the shape most likely to attract a "you shorted me a cent" complaint pattern and, in some jurisdictions, regulatory interest in advertised-vs-paid bonus amounts. |
| **D. Round-up / ceiling** | `€1.231` → `€1.24` | The mirror of C: never understates a player credit, systematically costs the operator a fraction of a minor unit on every rounded bonus, cashback and win, and systematically over-charges on every rounded deduction unless deductions are excluded. Rarely chosen as a global rule; occasionally chosen for player-credit amounts only (which is really option E). |
| **E. Directional by beneficiary** ("always in the player's favour": round credits to a player up, deductions from a player down) | A cashback of `€1.231` → `€1.24`; a stake deduction of `€1.239` → `€1.23` | Commercially and reputationally the safest player-facing position, and trivially defensible to a regulator or a support desk. It is the most expensive option (the operator pays the residue on both sides), and it is the most complex to enforce, because "is this leg a credit to the player?" must be decided by the ledger layer for every posting shape — including four-entry bonus transactions and mirror legs — rather than being a property of one arithmetic helper. |
| **F. Truncate + carry the remainder forward** (a remainder accumulator per player/asset/context; a whole minor unit is released once the accumulated fraction reaches one) | Cashback of `€1.239` pays `€1.23` and stores `0.009`; three such events later the stored fraction exceeds `0.01` and the next payout includes the extra cent | The only option that is **exact in aggregate** — no party gains or loses a fraction over time — which is why it is used where fractions are economically significant (e.g. per-unit interest, high-precision crypto). The cost is a genuine new mechanism: a durable, tenant-scoped, concurrency-safe remainder store with its own idempotency, reconciliation and audit story, plus a policy for what happens to an unreleased remainder when a bonus expires, a wallet closes or a player self-excludes. It is not a variant of A–E; it is additional machinery, and `ledger-finance` would own it. |

**Observation, explicitly not a recommendation.** In consumer-facing
payments and gaming systems, half-up (A) is the most commonly encountered
convention, and "in the player's favour" (E) is a common overlay
specifically on promotional credits; half-even (B) is more common in
accounting/reporting stacks than in player-facing amount computation; and
remainder-carrying (F) is generally reserved for contexts where fractions
accumulate materially. This paragraph is offered because the human asked
what is typical, and is labelled an observation precisely so it cannot be
read as `ledger-finance` having made the choice. Any of A–F is
implementable within the existing `NUMERIC(38,0)` model; A–E require no new
storage, F does.

#### (Q2) Precision/remainder treatment — the options

| Option | What it means concretely | Trade-off |
|---|---|---|
| **P1. Round once, at the end, to the target asset's exponent** — every intermediate value carried at full `NUMERIC` precision; the residue is absorbed by the counterparty leg of the same transaction (FX spread, `bonus_expense`, `agent_commission_expense`) | A capped tiered commission is computed entirely in `NUMERIC`, rounded once when the posting amount is produced | Fewest rounding events, therefore the smallest accumulated deviation, and the residue always lands in an account a finance reviewer already reads. Requires every service in the chain to agree on where "the end" is, which must be written down per computation, not assumed. |
| **P2. Round at each intermediate step** | Percentage applied and rounded, then cap applied and rounded, then weighting applied and rounded | Easier to reason about step-by-step and to show in a player-facing breakdown; compounds rounding deviation across steps and makes the final amount depend on step order, so the step sequence becomes part of the stored rule. |
| **P3. Truncate to the minor unit and carry the remainder** (option F above) | As F | Exact in aggregate; needs the remainder-tracking mechanism described in F, including its expiry/closure policy. |

#### What is fixed regardless of the answer — `ledger-finance` asserts this now

These bind whichever of A–F and P1–P3 is chosen, and are **not** part of
the open decision:

1. All such arithmetic is `NUMERIC`, never `FLOAT`/`DOUBLE`/`REAL`
   (this ADR, ADR 0032 §9, ADR 0035 §9.1).
2. A posted amount must be **exactly recomputable** from the stored inputs
   (base amount, stored rate, stored cap, **stored rounding-rule
   identifier**) as a reconciliation check. That implies the chosen rule is
   **versioned and stored with, or resolvable as-of, the transaction** —
   changing the platform's rounding rule later must not retroactively make
   every historical amount fail its own recomputation check.
3. A rounding residue is **never** a silent truncation of a player balance
   and **never** an unbalanced fractional unit. It is either absorbed by an
   explicit counter-leg in the same `LedgerTransaction` or carried in an
   explicit remainder mechanism. `SUM(DEBITS) == SUM(CREDITS)` is not
   negotiable against any rounding convention.
4. There is exactly **one** shared rounding helper, in one place, used by
   FX, bonus, commission and cash-rounding alike. Per-campaign, per-domain
   or per-service arithmetic is a blocking review finding — including, and
   especially, in a POS/JavaScript client (ADR 0035 §9.2).
5. Until the decision is made, bonus amounts are computed through that one
   shared helper (ADR 0032 §9) so that exactly one line changes when the
   human answers, rather than a rule being re-derived in each campaign.

#### Gate statement — Stage 4H-B1 is READY FOR HUMAN AUTHORIZATION after `bonus_conversion` (Stage 4H-B0-R3 update)

**Stage 4H-B0-R1 correction (historical).** Stage 4H-B0's completion
report described Stage 4H-B1 (Bonus Engine implementation) as
independently authorizable, while the same stage's own risk register
(`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md`,
originating in ADR 0032 §9) recorded this ADR's unresolved rounding
decision as blocking precise computation for deposit, reload and
cashback bonuses. Both could not be true; Stage 4H-B0-R1 corrected the
gate to `CONDITIONALLY READY` pending three items.

**Stage 4H-B0-R3 update: item 1 of those three is now RESOLVED** (see
"DECISION RECORDED" above). Item 3 was independently re-confirmed with
no additional P0/P1 found (`architect`'s focused 12-area review, Stage
4H-B0-R2, re-confirmed unchanged in Stage 4H-B0-R3). The gate now reads:

> **Stage 4H-B1 is READY FOR HUMAN AUTHORIZATION after completion of
> `bonus_conversion`.** Design and scope are frozen and reviewed; the
> rounding/precision decision is resolved; no other P0/P1 financial
> dependency remains. **Production implementation of any bonus amount
> computation must not begin until:**
>
> 1. ~~This ADR's rounding/precision `OPEN DECISION` (Q1, Q2 and Q3
>    above) is explicitly resolved by the human decision-maker and
>    recorded here.~~ **RESOLVED, Stage 4H-B0-R3.**
> 2. The Risk dependency for the `bonus_conversion` `Operation` value is
>    **completed and reviewed** (ADR 0031's extension model, all steps in
>    one authorized change) — **still NOT STARTED, zero of six steps**,
>    re-verified by `risk` in Stage 4H-B0-R3 against current repository
>    state. See ADR 0031 §16a for the full checklist.
> 3. **No other P0/P1 financial dependency remains** open against the
>    first-slice bonus types — re-confirmed, none found.
>
> Work that does not compute a monetary amount from a percentage (lifecycle
> state machine, Offer/Grant modelling, eligibility, Progress trail) was
> never gated by item 1 and remains unblocked.

`ledger-finance` is **not authorized to, and does not, select a rounding
rule here.** Options A–F and P1–P3 are enumerated for a human to choose
from; the choice carries player-facing, commercial and (for cash rounding)
jurisdictional weight. *(Historical — decision recorded below.)*

#### DECISION RECORDED (Stage 4H-B0-R3)

**DS-1 (Q1, rounding direction) = Option A, round-half-up (ties away from
zero).**

**DS-2 (Q2, rounding point/precision) = Option P1, round once, at the
final monetary boundary, with every intermediate value carried at full
`NUMERIC` precision.** Explicit constraints from the human decision,
confirmed compatible with the architecture by validation: no implicit
rounding of intermediate calculations; a database numeric-to-integer cast
must never be relied on as the business rule; no truncate-and-carry
remainder mechanism (Option F/P3) unless the existing architecture
already requires it — validated finding: **it does not** (see the
cashback clarification below).

**DS-3 (Q3, uniformity/scope) = one platform-wide deterministic rounding
rule by default**, with the architecture able to support a future
per-asset/per-jurisdiction override if a genuine legal/business
requirement arises, without redesign — validated as fully compatible with
ADR 0035 §9.3's cash-rounding note (a future override is a legitimate
path, not an ungoverned second convention) and requiring no extra
implementation cost now, given item 2 below already requires a
versioned, stored rule identifier per transaction.

**Exact algorithm** (validated by `ledger-finance`, confirmed exponent-
agnostic by `qa`): round half away from zero — `result = sign(x) ×
floor(|x| + 0.5)` — applied via one named, shared function operating on
the exact pre-rounding `NUMERIC` value and the target asset's minor unit,
never via a bare `ROUND()` call, an implicit `NUMERIC(38,0)` column-scale
coercion, or a language/library default (`ledger-finance` confirmed
PostgreSQL's own implicit numeric-to-integer coercion happens to match
round-half-up for positive values today — this is a coincidence of
today's Postgres behavior, not a specification, and must never be relied
upon). The function's interface must be expressed in "minor unit" /
"base unit" terms, never "cents," and takes no hardcoded scale (`qa`).
**Negative-input contract, specified now though no current bonus call
site produces a negative input** (all `ledger_entries.amount` values are
strictly positive; reversals post the exact inverse of an already-posted
positive integer, never a stored negative — `ledger-finance` confirmed
negative amounts are not applicable to any current bonus posting): "ties
away from zero" means `-2.5 → -3`, not `-2.5 → -2`, so the same function
is unambiguous if DS-3's one shared rule is later reused for FX or
commission, where a signed delta is more plausible.

**Where the applied rule/version must be stored** (`security`,
confirmed compatible with DS-1/DS-2/DS-3): an immutable, append-only
`rounding_rules` reference table, one row per composite version encoding
both the Q1 direction and the Q2 rounding-point/residue-treatment
together (never two independently-versioned axes, which would create an
ambiguous cross-product) — new rule, new row, existing rows never
edited, mirroring this codebase's standing append-only convention. The
identifier actually applied must be **denormalized onto the
`LedgerTransaction` row itself** at post time (e.g.
`ledger_transactions.rounding_rule_id`), immutable thereafter under the
same no-mutation enforcement every other column on that table already
has — the same rationale already applied to `tenant_id`/`wallet_id`/
`player_account_id`/`asset_code` on `LedgerEntry` (denormalized so RLS
and reconciliation never depend on a join to a table that could evolve
independently). The upstream computation row (the Grant/Offer, the
future `ConversionOperation`) should also store the same identifier
alongside its own inputs (rate, cap, base amount) — that is where the
recomputation inputs live; the ledger-row copy is what makes the audit
trail self-sufficient even if the upstream row is later archived or
restructured. **Not implemented this stage** — this is the specified
target for whichever migration builds it.

**Per-bonus-type clarification** (`bonus-engine`, `ledger-finance`,
resolving an ambiguity flagged during validation rather than leaving it
open): for Deposit-match, Reload, and Cashback, the "final monetary
boundary" DS-2 rounds at is **after both the percentage multiply and the
cap comparison** — i.e., round `min(exact_amount × rate_%, cap)` once,
not the raw percentage result before the cap decision. This is the only
reading consistent with "the final boundary," since the cap is part of
computing what is actually granted. The **wagering-requirement target**
(`bonus_amount × multiplier`) is a comparison threshold that gates a
lifecycle-state transition and is never itself posted to the ledger
(ADR 0032 §3.1: "conversion-eligibility is a decision, not a movement")
— DS-2's "final monetary boundary" language does not apply to it in the
ledger-posting sense, though the same deterministic `NUMERIC` discipline
still applies so the same comparison always resolves the same way. **Per-
game contribution weighting** (`stake × contribution_%`), by contrast,
**is monetary** — it determines the actual cash/bonus split posted for a
wagering event (doc 10 §6) — and DS-2's final boundary for it is the
split-instruction computation, capped by the Grant's remaining bonus
balance, rounded once there. Generic Wagering bonus's and Coupon's flat/
pre-configured grant amounts are unaffected by DS-1/DS-2 entirely (no
percentage computation produces them); their wagering-requirement/
contribution tracking, once activated, follows the same rule as above.

**Cashback residual — confirmed consequence, not silently assumed.**
Repeated cashback events (e.g. a fixed weekly percentage of net loss)
leave a small fractional residue every time that never resolves to zero
on its own. **Validated finding: the existing architecture has no
remainder-accumulation mechanism anywhere** (no such account in
`ledger-accounting-model.md`, no such stream in `reconciliation-
model.md`, no such object in ADR 0032) — building one (Option F/P3)
would be new, unauthorized architecture requiring its own ADR and human
sign-off, not something DS-1+DS-2 imply or require. **What DS-1+DS-2 as
recorded actually mean for cashback is: each cashback calculation is
rounded independently and immediately, round-half-up, with no
accumulation.** The discarded sub-minor-unit fraction each period is not
owed to the player and never becomes a ledger fact; it is a bounded,
one-directional-only-on-exact-ties commercial cost already described in
Option A's own trade-off row above, not a new finding. This is recorded
explicitly here so it is not silently assumed later.

**Non-blocking implementation-time items flagged during validation, for
Stage 4H-B1 to resolve when building — none of these change or reopen
DS-1/DS-2/DS-3, and none blocks recording this decision:**
1. A bonus computation that rounds to exactly 0 minor units cannot be
   posted (`ledger_entries.amount > 0`). This is a Bonus Engine
   eligibility/configuration question (e.g. a minimum-deposit/minimum-
   bonus guard), not a rounding-rule question — `ledger-finance` flagged
   it, Stage 4H-B1 must close it before implementation.
2. Whether a conversion's max-cashout cap can ever be percentage-derived
   (rather than a flat configured figure) is unspecified by any existing
   document (`bonus-engine`) — if it ever is, that derivation needs its
   own single DS-2 rounding point, most naturally at Offer-configuration
   or Grant-issuance time, not recomputed per conversion.
3. Coupon's Reward-axis shape (a flat grant vs. a %-based Offer, e.g. a
   coupon-triggered deposit-match) is not pinned down by doc 10 today
   (`bonus-engine`) — an open scoping question for whoever authors the
   Coupon Offer templates in Stage 4H-B1, unrelated to the rounding
   decision itself.

Status: **RESOLVED — DS-1/DS-2/DS-3 recorded above, Stage 4H-B0-R3.**

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
- One open business decision (fee/spread revenue account) remains
  explicitly deferred, not resolved by engineering judgment alone, since
  it affects reported revenue. The rounding direction decision below is
  **RESOLVED** as of Stage 4H-B0-R3.
- **Stage 4H-B0-R1 correction, resolved Stage 4H-B0-R3**: the rounding
  decision was never a deferred FX-only item — it gates every
  percentage-of-money computation on the platform (deposit-match, reload
  and cashback bonuses per ADR 0032 §9; wagering contribution weighting;
  agent commission per ADR 0035 §5.2; physical-cash rounding per ADR 0035
  §9.3) and, before this stage, made Stage 4H-B1 `CONDITIONALLY READY`
  rather than independently ready. **Now resolved**: see "Rounding and
  precision — RESOLVED (Stage 4H-B0-R3)" for the recorded decision and
  the gate statement immediately below it, updated to "READY FOR HUMAN
  AUTHORIZATION after `bonus_conversion`." The fee/spread revenue account
  remains FX-scoped and was never a bonus gate.

## Owner

`ledger-finance`.

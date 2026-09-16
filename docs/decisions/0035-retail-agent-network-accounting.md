# ADR 0035 — Retail Agent Network Accounting

Status: Proposed (Stage 4H-B0, Bonus/Gamification/Retail Architecture &
Scope Freeze) — **architecture and accounting design only,
`NOT IMPLEMENTED`**. No migration, no Go code, no `account_type`,
`transaction_type` or `Operation` value in this document exists yet.
Owner: `ledger-finance`.

This ADR specifies **only the monetary/accounting treatment** of a retail
agent network. The hierarchy/role model, the node object itself, the
who-may-fund-whom authorization predicate, and the RBAC shape are owned by
`architect` (`docs/architecture/26-retail-operations-architecture.md`, in
progress in parallel) and `security`. Where this ADR needs one of those, it
names the requirement it places on them and does **not** design it — the
assumptions it makes are enumerated in §12 so the Master Orchestrator can
verify consistency once all Wave-1 documents are in.

Labeling convention is inherited from `financial-domain-model.md` and
ADR 0032: `BLUEPRINT` = stated directly in the Blueprint;
`ARCHITECTURAL DECISION` = decided here or in a named prior ADR;
`OPEN DECISION` = deliberately not resolved, with the reason and the
required decision-maker named; `RECOMMENDATION` = a `ledger-finance`
position that still needs business/finance/human sign-off before it binds.

**Nothing in this document is `BLUEPRINT`.** Retail operations are a new
confirmed business requirement; the Blueprint describes an online platform
and contains no agent network, no cashier, no till and no commission
model. Every account type, transaction type and invariant below is
therefore `ARCHITECTURAL DECISION` or weaker, and none may be presented
anywhere as a Blueprint requirement (CLAUDE.md, "Primary source of truth").
The scope-anchor question that follows from that — whether the retail
domain is authorized product scope at all — belongs to the Master
Orchestrator and `product-owner-proxy`, not to this ADR, which answers only
"if it is built, this is how the money is accounted for".

## Context

The platform must support retail iGaming operations: a physical agent
network (Operator → Partner → Super Agent → Agent → Player/Cashier, with
configurable depth and structure per tenant/licence/jurisdiction) as
another surface of the **same** platform, sharing the **same authoritative
ledger** as online.

That single sentence is the whole problem. Retail introduces, for the first
time in this platform's history, all four of the following at once:

1. **A financial counterparty that is neither a player nor a provider nor
   the house** — an agent node holds value, is funded, is owed money, and
   is the source and destination of player funds. `ledger_accounts` today
   supports exactly two owner families (a `Wallet`, or the tenant-as-house)
   and cannot express a third without a schema change (§1.3, §10.1).
2. **Money that moves physically, outside any system the platform can
   observe.** A cashier taking a €100 note is a real economic event with no
   API, no callback, no settlement file and no counterparty statement. The
   platform's ledger must record its *consequence* without ever pretending
   to have custody of the note (§3.1, §6.2).
3. **A new originating actor class** — a POS/cashier terminal. Casino and
   sportsbook are providers; PSPs are providers; the bonus engine is an
   internal service; staff are staff. A cashier is none of these, and the
   idempotency and authorization consequences are genuinely different
   (§8).
4. **A commercial revenue-share relationship with a non-provider
   counterparty** — commission down a configurable hierarchy (§5).

Every one of these is a place where a second financial truth system would
otherwise be invented: an "agent balance" table maintained by a retail
service, a "till balance" column updated on shift close, a "commission
owed" counter incremented per bet. This ADR exists to prevent all three
before any of them is written, exactly as ADR 0032 did for the Bonus
Engine.

## Decision

### 0. Position statement — there is exactly one financial truth system

`ARCHITECTURAL DECISION`, non-negotiable, inherited verbatim from ADR 0032
§0 and restated here because a new domain is the moment it gets forgotten.

The retail subsystem — the hierarchy, the terminals, the shift workflow,
the commission calculator, the agent back office — is a **decision and
workflow system**. It decides *who may fund whom*, *which cashier is on
shift*, *what the commission rate is*, *whether a payout is authorized*.
It is not an accounting system. Every monetary consequence of its
decisions is recorded by `internal/ledger`, through `ledger.Post`, under
the same invariants as a deposit, a casino bet or a bonus grant —
append-only, double-entry, balanced per asset, DB-enforced idempotency,
compensating corrections only, projection-never-authoritative, reconciled
on a schedule.

Binding consequences, each a blocking `ledger-finance` review finding if
violated:

- **No retail table holds an authoritative monetary balance.** There is no
  `agent_balance` column, no `till_balance` column, no
  `commission_accrued` counter. An agent's float balance is a
  `wallet_balance_projection` row over ledger entries and nothing else. Any
  monetary figure a retail table carries is a denormalized read of the
  ledger and must be reconcilable to it.
- **No retail code path `UPDATE`s a balance**, mutates a historical entry,
  or posts a money movement without a DB-enforced idempotency key.
- **`internal/ledger` remains the only package that writes
  `ledger_accounts`/`ledger_transactions`/`ledger_entries`** (ADR 0001).
  The retail subsystem supplies the instruction; `ledger-finance` owns the
  posting mechanism.
- **A cashier's till is not a ledger account** and the physical cash in it
  is not a platform balance (§6.2). This is the retail analogue of ADR 0032
  §6(c)'s "the platform's ledger never mirrors a balance held in a
  provider's system", and it is equally non-negotiable.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 1. Agent/node financial accounts — worked out, not guessed

#### 1.1 What an agent's float actually is

The directive asks whether an agent's float is a liability, an asset, or
something else *from the platform's perspective*. Working it out from the
cash movements rather than from the word "float":

Consider the prefunded (prepaid) model, which is the dominant retail shape
and the one this ADR designs for by default:

1. A Partner wires €10,000 of real money to the operator's bank/PSP rail.
   The operator now **holds €10,000 that belongs to the Partner** and has
   promised the Partner €10,000 of spendable capacity on the platform.
2. The Partner passes €6,000 of that capacity down to a Super Agent, who
   passes €2,000 to an Agent. No money moved externally; the operator still
   holds the same €10,000 and now owes it to three parties instead of one.
3. A player hands €100 in cash to that Agent's cashier. **The cash stays
   with the agent.** The platform receives nothing. What the platform does
   is reduce what it owes the Agent by €100 and increase what it owes the
   player by €100.

At every step the operator's obligation is "spendable value we owe a
counterparty, redeemable either as platform value or as real money on
request". That is precisely what `player_cash` is for a player. Therefore:

> **An agent node's float is a platform liability, credit-normal, and is
> the direct structural analogue of `player_cash` for a non-player
> counterparty.** It is not an asset of the platform; the platform has no
> claim on it. It is not an expense; nothing has been given up. It is not a
> receivable, *unless it goes negative* — see §1.4.

The single most important consequence, stated plainly because getting it
wrong is the most likely retail accounting bug:

> **A retail cash deposit is not a deposit into the platform. It is a
> transfer of an existing platform liability from an agent to a player.**
> No money crosses the platform's custody boundary, so no `psp_clearing`,
> `psp_reserve` or bank-rail account may appear in a retail deposit's
> entries. Posting a retail deposit in the Flow 1 shape (`Dr psp_clearing /
> Cr player_cash`) would record a receivable from a PSP that does not
> exist and will never settle, inflating `psp_clearing` permanently and
> breaking `reconciliation-model.md` §2.6's "drains toward zero"
> expectation with a balance no PSP statement can ever explain.

This is enforced structurally as invariant R1 (§3.4), not left to caller
discipline.

#### 1.2 New account types

`ARCHITECTURAL DECISION`. All new, all `NOT IMPLEMENTED`, all additive to
`ledger-accounting-model.md` §2's table.

| Account type | Purpose | Owner/scope | Normal balance | Directly manipulable? |
|---|---|---|---|---|
| `agent_float` | A hierarchy node's spendable operating capacity — the platform's liability to that node | **Node** + asset: one row per `(tenant_id, hierarchy_node_id, asset_code)` | **Credit** (liability owed to the node) | No — only via a `LedgerTransaction` |
| `agent_commission_payable` | Commission earned by a node and recognized, not yet discharged | **Node** + asset: one row per `(tenant_id, hierarchy_node_id, asset_code)` | **Credit** (liability owed to the node) | No |
| `agent_commission_expense` | The operator's recognized cost of the agent network | House-level, per `(tenant_id, asset_code)`, `wallet_id IS NULL` | **Debit** (expense) | No |

Two further account types are **proposed conditionally** and are not
adopted by default:

| Account type | Adopted? | Condition |
|---|---|---|
| `retail_cash_on_hand` | **No — `OPEN DECISION`** | Required **only** if the retail network is company-owned (the operator owns the cash in the till) rather than franchised (the agent owns it). See §6.2 and §11.1. Debit-normal asset, scoped per `(tenant_id, hierarchy_node_id, asset_code)` if adopted. |
| `cash_rounding_difference` | **`RECOMMENDATION`** | Needed only once an asset/jurisdiction requires physical-cash rounding (§9.3). House-level per `(tenant_id, asset_code)`, clearing-style (no normal balance, like `manual_adjustment`). The alternative — folding the residue into `house_gaming` — is a finance/reporting call, not an engineering one. |

`signed_balance` remains credit-positive for every account type without
exception (`ledger-accounting-model.md` §5). Expected signs for the new
types: `agent_float ≥ 0` (see §1.4), `agent_commission_payable ≥ 0`,
`agent_commission_expense ≤ 0`, `retail_cash_on_hand ≤ 0` if adopted. The
"normal balance" column is an operational-alert expectation, never a
read-time sign convention.

**Why `agent_commission_payable` is a separate account from `agent_float`,
and not netted into it.** Requirement #13 (no commingling) is one reason,
but not the load-bearing one. The load-bearing one is that float is
*gated* — an `agent_float` debit is refused if the balance is
insufficient (§3.4, invariant #15) — and unrecognized or disputed
commission must never silently raise the ceiling a cashier can fund
players against. Merging them makes an accounting dispute about last
month's commission into a live overdraft on today's deposits. Moving
commission into float is therefore an explicit posting (§5.3), never a
property of one account carrying two meanings.

**Why `agent_commission_payable` is not `provider_payable`.** ADR 0032
§6(b) reused `provider_payable` for provider-funded promotions and was
right to: a provider-funded bonus genuinely reduces what we owe a
provider, and it settles through the provider settlement stream
(`reconciliation-model.md` §2.5). An agent is not a provider: it has no
`ProviderCapability` row, no adapter, no callback credential, no
settlement statement, and is not reconciled against a vendor's books.
`provider_payable` is also a single house-level row per
`(tenant_id, asset_code)` with per-provider detail reconstructed from
transaction metadata, which is acceptable for a handful of providers and
unacceptable for a network of thousands of nodes whose individual
balances must be authoritative and lockable.

#### 1.3 The node-scoped owner family — a real conflict with ADR 0019

`OPEN DECISION` (`architect` + `ledger-finance` + `security`), and the
single most consequential finding in this ADR.

`ledger_accounts` today supports exactly two owner families
(`ledger-accounting-model.md` §1.1):

```
UNIQUE (wallet_id, account_type, asset_code) WHERE wallet_id IS NOT NULL
UNIQUE (tenant_id, account_type, asset_code) WHERE wallet_id IS NULL
```

A node-scoped account fits neither. With `wallet_id IS NULL` it collides:
two agents in the same tenant both needing `agent_float`/EUR violate the
house-level constraint, so a naive implementation would silently give the
whole tenant **one shared float account** — every agent spending every
other agent's capacity, undetectable until an agent overdraws someone
else's money. This must be resolved before any retail posting, not
discovered during it.

Three resolutions were considered:

- **(a) Give each node a `Wallet`.** Rejected. `wallets.player_account_id`
  is `NOT NULL` with a composite FK to `player_accounts`
  (`financial-domain-model.md`), and ADR 0019's player-scoped RLS policies
  key on `player_account_id`. Making a node a pseudo-player would either
  break that FK or create fake `player_accounts` rows that pollute player
  counts, KYC/RG state, GGR reporting and every player-scoped policy in
  the platform. A node is not a player and must not be represented as one.
- **(b) Add a third owner dimension to `ledger_accounts`** —
  `hierarchy_node_id UUID NULL`, a third partial unique index
  `UNIQUE (tenant_id, hierarchy_node_id, account_type, asset_code) WHERE
  hierarchy_node_id IS NOT NULL`, and a `CHECK (num_nonnulls(wallet_id,
  hierarchy_node_id) <= 1)` so an account has at most one non-house owner.
  `hierarchy_node_id` is denormalized onto `ledger_entries` and
  `wallet_balance_projection` by the same `BEFORE INSERT` trigger pattern
  ADR 0019 already uses for `player_account_id`/`wallet_id`, for the same
  reason (a policy needs the column on the row it protects).
  **`RECOMMENDATION`.** It is additive, changes no existing constraint,
  invalidates no existing row, and preserves every current invariant.
- **(c) A single house-level `agent_float` control account plus a
  per-node subsidiary ledger** maintained by the retail subsystem.
  Rejected outright: that subsidiary ledger *is* a second financial truth
  system with authoritative per-node balances, which §0 forbids, and the
  per-node balance is exactly the figure a posting must lock and check.

**This ADR does not adopt (b) unilaterally** — it changes a shared,
already-implemented table owned jointly with `architect` and `security`,
which CLAUDE.md's "no specialist redesigns shared architecture
unilaterally" puts outside `ledger-finance`'s authority to decide alone.
It is raised as an `OPEN DECISION` with (b) as the recommended shape and
(a)/(c) recorded as considered-and-rejected so the decision is not
re-derived.

Two consequences that fall out and must be decided with it:

- **RLS for node-scoped accounts.** ADR 0019 gives player-owned accounts a
  player-principal scope via `app.player_account_id` and house-level
  accounts a staff-RBAC-only read path. A node-scoped account needs a
  third answer, because an agent principal must be able to read *their own
  node's* float and their subtree's, and must not read a sibling's. Whether
  that is a new `app.hierarchy_node_id` GUC with a subtree-aware policy, or
  a staff-RBAC-only read path with subtree filtering in the handler, is
  `security` + `architect`'s call. `ledger-finance`'s only binding
  requirement: whatever is chosen must not repeat ADR 0019's
  projection-`UPDATE` trap — the posting path runs under a tenant/service
  scope with no agent principal, so `wallet_balance_projection`'s `SELECT`
  policies must remain OR'd permissive policies, never a single policy
  that ANDs an agent scope in, or every retail posting silently updates
  zero projection rows and produces P1 drift by construction.
- **Projection grain is already correct.** ADR 0019 chose one
  `wallet_balance_projection` row per `ledger_account_id` rather than per
  `(wallet_id, account_type)` precisely so that non-wallet accounts have a
  row to reconcile and a row to lock. Node-scoped accounts inherit that
  with **no change** — the prior decision already accommodates this case.

#### 1.4 Negative float — a credit line is a business decision, not an account shape

`agent_float` going negative means the node owes the operator: the same
account, read as a receivable. No second account type is needed, and none
is proposed.

`ARCHITECTURAL DECISION` (ledger default): **an `agent_float` debit may
not drive the balance below zero.** The check is a `SELECT ... FOR UPDATE`
on the projection row inside the posting transaction, identical in
mechanism to the `player_cash` sufficiency check (ADR 0020,
`ledger-accounting-model.md` §6 invariant #15). An overdraw is **rejected,
not clamped** — clamping is a business decision and does not belong in the
ledger.

`OPEN DECISION` (commercial + credit-risk + **legal**): whether post-pay
agents (a credit line permitting a negative float up to a configured
limit) are supported at all. Three reasons this is not `ledger-finance`'s
to decide:

1. It is an extension of commercial credit to a counterparty — a
   credit-risk and contractual decision.
2. Several target jurisdictions regulate or prohibit credit-funded
   gambling, and a retail credit line can be the mechanism by which a
   player plays on credit even if the player never sees a credit product.
   CLAUDE.md puts legal interpretation squarely in "stop and ask".
3. If adopted, the limit value is `internal/risk` configuration (§4), not
   a ledger constant — the ledger's floor becomes "not below the
   authorized limit for this node and asset", with the limit resolved by
   `risk.Evaluate` before the posting, and the ledger's own hard floor
   remaining zero for any node with no authorized line.

Status: **§1.1/§1.2/§1.4 RESOLVED (architecture) — `NOT IMPLEMENTED`.
§1.3 is an `OPEN DECISION` and a blocking precondition for any retail
posting.**

### 2. The four pools and the non-commingling invariant

Requirement #13 — player money, agent operational funds, commissions and
platform/house funds may never commingle — is met structurally, the same
way `player_cash`/`player_bonus`/`promo_liability`/`bonus_expense` are kept
distinct today: by account type plus owner scope, checked at posting time,
not by convention.

Every ledger account belongs to **exactly one pool**, determined solely by
its `account_type` and owner scope. No account is a member of two pools.

| Pool | Accounts | Owner scope |
|---|---|---|
| **P — player** | `player_cash`, `player_bonus`, `player_locked`, `player_withdrawal_hold` | Wallet (tenant+brand+player+asset) |
| **F — agent operational** | `agent_float` | Node (tenant+node+asset) |
| **C — commission owed** | `agent_commission_payable` | Node (tenant+node+asset) |
| **H — platform/house** | `house_gaming`, `psp_clearing`, `psp_reserve`, `provider_payable`, `jackpot_contribution`, `promo_liability`, `bonus_expense`, `manual_adjustment`, `agent_commission_expense`, and any bank/treasury account Flow 18 eventually adds | Tenant+asset |

> **Invariant R2 (pool separation).** Every `LedgerEntry` belongs to
> exactly one pool. A `LedgerTransaction`'s implied pool transitions must
> be drawn from the matrix below; `internal/ledger` rejects any transaction
> whose entries imply a transition not in it. The rejection is
> unconditional and per-posting, not per-caller — enforcement belongs where
> the invariant is owned, the same reasoning ADR 0032 §2 applies to the
> bonus mirror legs and CLAUDE.md applies to RLS.

| Transition | Permitted | `transaction_type` |
|---|---|---|
| H → F | Yes | `agent_funding` (real money received from the node) |
| F → H | Yes | `agent_settlement` (real money paid out to the node); `till_variance` (§6.4, if adopted); four-eyes `manual_adjustment` (§7.1) |
| F → F | Yes, **adjacent nodes only** | `agent_float_transfer` |
| F → P | Yes | `retail_deposit` |
| P → F | Yes | `retail_withdrawal_payout` |
| P → P | Yes | `retail_withdrawal_authorization` (into `player_withdrawal_hold`), plus every existing intra-player flow |
| H → C | Yes | `agent_commission_accrual` |
| C → F | Yes | `agent_commission_capitalization` |
| C → H | Yes | `agent_commission_payout` |
| P ↔ H | Yes | every existing flow (deposit, bet, win, bonus, withdrawal, …) — unchanged |
| **P ↔ C** | **Never** | A player's money is never commission, and commission is never credited to a player wallet. There is no transaction type and no reason code that makes this legal. |
| **C → C** | **Never** | Commission is never transferred between nodes. A node earns its own commission; an override earned by an upper node is its own accrual (§5.4), not a re-assignment of a lower node's. |

Two further structural rules, both checkable at posting time:

- **No transaction may contain entries against two different nodes'
  `agent_float` accounts except an `agent_float_transfer`**, which contains
  exactly two float entries and nothing else. This prevents a retail
  deposit from quietly moving float between nodes under cover of a player
  leg.
- **No transaction may contain both a player leg and more than one float
  leg.** A retail deposit or payout touches exactly one node.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 3. Retail deposit and withdrawal — the full posting chains

All amounts are minor units per the asset's registered exponent (§9). All
transactions are one `LedgerTransaction`, one database transaction.

#### 3.1 Upstream funding — how float comes into existence

| Event | Entries | Idempotency key |
|---|---|---|
| Node funds itself over an existing PSP rail (card/bank transfer through a `PaymentProvider`) | Dr `psp_clearing` `X` · Cr `agent_float(node)` `X` | `(tenant_id, provider_id, provider_tx_id)` — the PSP's own reference, `provider_id` resolved from the credential that verified the callback (ADR 0019) |
| Node funds itself by direct bank transfer, no PSP adapter | **`BLOCKED`** — see §11.2 | — |
| Node A funds child node B | Dr `agent_float(A)` `X` · Cr `agent_float(B)` `X` | `(tenant_id, idempotency_key)` — §8 |

The funding chain is **recursive and depth-agnostic by construction**:
every node that can hold value has exactly one `agent_float` per asset, and
every funding movement between adjacent nodes is the *same* two-entry
transaction. Hierarchy depth and structure are therefore data, never a code
path — which is the accounting expression of CLAUDE.md's "nothing
brand-specific may become a code path" and of this stage's own "configurable
depth/structure per tenant/licence/jurisdiction, not hardcoded" requirement.
A four-level network and a two-level network produce identical postings.

Whether a *cashier* is itself a funded node (with its own `agent_float`) or
operates directly against its parent Agent's float is a configuration
choice in `architect`'s hierarchy model. **The accounting is identical
either way**; the only difference is which node's float a terminal's
postings debit, and at which node the §6 till reconciliation is anchored.

#### 3.2 Retail deposit — cash in at a cashier

Physical: the player hands `X` in cash to the cashier; the cash enters the
till and **stays with the agent**. The platform never receives it.

| Event | Entries |
|---|---|
| `retail_deposit` `X` | Dr `agent_float(node)` `X` · Cr `player_cash(wallet)` `X` |

Two entries. `psp_clearing` is **not** involved (§1.1, invariant R1).

**What discharges the agent's float?** The debit *is* the discharge, and
nothing further is posted. The agent's compensation is the physical cash,
which lives outside the ledger entirely (§6.2). An implementation that
looks for a second, later "float settlement" leg for an individual deposit
has misunderstood the model: the agent was made whole at the counter, in
cash, at the instant of the transaction.

Preconditions checked **inside the posting transaction, before commit**:

1. `rg.EvaluateEligibility` for the **player** — a self-excluded or
   otherwise RG-ineligible player must not be funded at a physical counter
   any more than online. Retail is a new channel, not a new rulebook.
2. `risk.Evaluate` (§4).
3. `agent_float(node)` sufficiency, `SELECT ... FOR UPDATE` on the
   projection row (§1.4).

Order is RG first (short-circuiting), then Risk, then sufficiency —
the fixed order `internal/casino` already establishes (ADR 0031 §1).

#### 3.3 Retail withdrawal — cash out at a cashier

`ARCHITECTURAL DECISION`: a retail payout is **two steps**, reusing
`player_withdrawal_hold` exactly as designed. No new account type.

| Step | Event | Entries |
|---|---|---|
| A | `retail_withdrawal_authorization` `X` — terminal requests payout, platform authorizes | Dr `player_cash` `X` · Cr `player_withdrawal_hold` `X` |
| B | `retail_withdrawal_payout` `X` — cashier confirms the cash was physically handed over | Dr `player_withdrawal_hold` `X` · Cr `agent_float(node)` `X` |
| — | Expiry/cancellation (payout never happened) | Dr `player_withdrawal_hold` `X` · Cr `player_cash` `X` — Flow 4 shape, `reverses_transaction_id` set to Step A |

The one-step alternative (`Dr player_cash / Cr agent_float` at the moment
of payout) was considered and **rejected**, for a reason specific to
physical cash:

> There is an irreducible window between "the platform authorized this
> payout" and "the cashier physically handed over the notes". In a
> one-step design that window has no account holding the value, so an
> authorized-but-unpaid payout is indistinguishable in the ledger from a
> paid one — and the §6 till reconciliation could not tell "the cashier
> owes this player cash" apart from "the till is short". The two-step
> design makes the window a balance (`player_withdrawal_hold`), bounded,
> queryable and expirable. Its correctness is demonstrated in §3.5.

Consequences that must be honoured:

- **The hold expiry window must exceed the terminal's maximum
  offline/retry window**, or a slow Step B confirmation races its own
  expiry and produces a payout posted against a hold that no longer
  exists. `RECOMMENDATION`: the expiry is per-tenant configuration with a
  floor derived from the terminal retry policy, not a hardcoded constant.
- Step B references the specific authorization it discharges; a Step B for
  an expired, already-discharged or unknown authorization is **rejected
  with an integrity alert**, never posted "best effort". This is the same
  rule Flow 9 applies to a settlement with no matching `player_locked`
  entry.
- A retail payout **never** credits `psp_clearing` and is never a
  `withdrawal_completed`. The value lands in `agent_float` because the
  agent gave up their own cash and the platform now owes them that value.
- Four-eyes approval above a configured per-asset threshold applies to
  Step A exactly as it does to an online withdrawal (§7.2), reusing
  `withdrawal_policies`/`ResolveApprovalPolicy` rather than a second
  threshold mechanism.

The agent later converts accumulated float back to real money:

| Event | Entries |
|---|---|
| `agent_settlement` `X` | Dr `agent_float(node)` `X` · Cr `psp_clearing` / bank rail `X` |

— which closes the circle and is the mirror of §3.1. It inherits Flow 18's
unresolved bank-rail `OPEN DECISION` when the payout is not made over a
PSP rail (§11.2).

#### 3.4 Invariant R1 — retail money never touches an external rail

> **Invariant R1 (retail conservation).** For every `LedgerTransaction`
> whose `transaction_type` is `retail_deposit`,
> `retail_withdrawal_authorization`, `retail_withdrawal_payout` or
> `agent_float_transfer`, the sum of entries against **any** house-level
> external-rail account (`psp_clearing`, `psp_reserve`, and any bank/
> treasury account Flow 18 adds) is exactly **zero** — i.e. those
> transactions contain no such entries at all. Enforced unconditionally at
> posting time in `internal/ledger`, with **no tolerance and no exception
> by caller**.

R1 is cheap (a type check plus an account-type check on the entry set) and
it structurally eliminates the single most dangerous retail bug: a retail
cash movement posted in the Flow 1/Flow 3 shape, minting a PSP receivable
or payable that no settlement file will ever match and that
`reconciliation-model.md` §2.2/§2.6 would report as an unexplained
mismatch of unknown age.

The complementary rule: `agent_funding` and `agent_settlement` are the
**only** retail transaction types that may touch an external rail, because
they are the only ones where real money genuinely crosses the platform's
custody boundary.

#### 3.5 Worked example with running balances

EUR, minor units omitted for readability. `signed_balance` is
credit-positive throughout (`ledger-accounting-model.md` §5), so
debit-normal accounts show negative. Chain: Partner P → Super Agent S →
Agent A; terminal T belongs to A and posts against A's float.

| Step | `psp_clearing` | `float(P)` | `float(S)` | `float(A)` | `player_cash` | `hold` | `house_gaming` | `comm_payable(A)` | `comm_expense` |
|---|---|---|---|---|---|---|---|---|---|
| 0. start | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| 1. P funds 10,000 via PSP | −10,000 | +10,000 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| 2. P → S 6,000 | −10,000 | +4,000 | +6,000 | 0 | 0 | 0 | 0 | 0 | 0 |
| 3. S → A 2,000 | −10,000 | +4,000 | +4,000 | +2,000 | 0 | 0 | 0 | 0 | 0 |
| 4. Player deposits 100 cash at T | −10,000 | +4,000 | +4,000 | +1,900 | +100 | 0 | 0 | 0 | 0 |
| 5. Player bets 100, loses | −10,000 | +4,000 | +4,000 | +1,900 | 0 | 0 | +100 | 0 | 0 |
| 6. Commission run: A earns 20% of 100 GGR | −10,000 | +4,000 | +4,000 | +1,900 | 0 | 0 | +100 | +20 | −20 |
| 7. A capitalizes commission to float | −10,000 | +4,000 | +4,000 | +1,920 | 0 | 0 | +100 | 0 | −20 |

**Check.** Sum of every signed balance at step 7:
`−10,000 + 4,000 + 4,000 + 1,920 + 0 + 0 + 100 + 0 − 20 = 0`. Debits equal
credits, as they must, at every row.

**Economic read.** The platform holds €10,000 of real money (a receivable
at the PSP), owes €9,920 to the agent network, booked €100 of gaming
revenue and €20 of commission cost. The operator's net position is
`10,000 − 9,920 = 80 = 100 GGR − 20 commission` — NGR of 80, arrived at
from two independent directions. The physical €100 note the player handed
over never appears in this table, correctly: it is the agent's, and it is
exactly what compensated the agent for the 100 of float they gave up at
step 4.

**Withdrawal continuation.** Suppose the player instead held 350 and
withdraws 300 at the counter:

| Step | `float(A)` | `player_cash` | `hold` |
|---|---|---|---|
| a. start | +1,900 | +350 | 0 |
| b. Step A — authorize 300 | +1,900 | +50 | +300 |
| c. Step B — cashier pays 300 cash, confirms | +2,200 | +50 | 0 |

The agent handed over 300 of their own cash and received 300 of float. If
Step B never happens, the expiry posting returns the player to `+350`,
`hold` to 0, float unchanged at `+1,900` — and the till is not short,
because no cash left it. **That symmetry is the proof the two-step design
is correct**: the only ledger state that can exist between authorization
and payout is a hold, and the hold's resolution matches the physical
outcome in both directions.

The one genuinely bad case — the cashier paid the cash but the Step B
confirmation was permanently lost and the hold expired — is **detected, not
silently absorbed**: the §6 shift reconciliation sees declared cash out of
300 against ledger payouts of 0 and raises a P1. It is then corrected by
posting through the ordinary path (a fresh authorization plus payout, or a
four-eyes `manual_adjustment`), never by editing the expired hold's
entries.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.**

### 4. Limit checks gate postings — the integration point only

`ledger-finance` does **not** own the limit engine, the limit rules, the
thresholds, or their scoping. `internal/risk` does (ADR 0031), and the
`risk` specialist is extending that ADR in parallel this wave. What this
ADR owns is **whether and how a limit check gates a posting**, mirroring
ADR 0031 §13 and `internal/casino`'s bet path.

`ARCHITECTURAL DECISION`:

- Every retail operation capable of affecting exposure calls
  `risk.Evaluate` **in the same database transaction as the posting it
  gates, before that transaction commits.** A retail limit evaluated in a
  separate transaction is not a gate; it is advice. No cache, and no
  counter maintained by the retail subsystem, may stand in for that read.
- A non-nil error, a `deny`, or a `review` outcome aborts the whole
  transaction and posts nothing — ADR 0031 §6's fail-closed contract,
  unweakened for retail.
- The retail subsystem **must not build its own limit engine**. No
  threshold comparison, no per-node daily cap table, no "max payout per
  shift" counter in retail code. Those are `risk_rules` rows.
- RG composes with Risk in the fixed order RG-first-then-Risk on any
  operation with a player leg (§3.2). Operations with no player leg
  (`agent_float_transfer`, `agent_funding`, `agent_settlement`,
  commission) have no RG dimension — RG is a player-protection concept and
  a hierarchy node is not a player.

Proposed `Operation` values, **DOCUMENTED ONLY** per ADR 0031 §16's
extension model — no Go constant, no migration, no HTTP validation is
added by this ADR, and none may be created before all the steps in that
model are executed together in one authorized change:

| Proposed `Operation` | Gates | Why not an existing value |
|---|---|---|
| `retail_deposit` | Funding a player's wallet at a physical counter | Not a PSP `deposit`: different rail, different counterparty, different exposure (agent float, not PSP chargeback), and reusing `deposit` would silently rebind every existing deposit rule to retail |
| `retail_withdrawal` | Step A authorization of a counter payout | Same reasoning against reusing `withdrawal` |
| `agent_float_transfer` | Moving float between nodes | No player, no wallet, no existing operation resembles it |

The ledger-side mapping ADR 0031 §16 step 5 explicitly reserves for
`ledger-finance` to specify, supplied here so that `cumulative_amount`
rules can work for the two player-facing operations:

| Operation | `operationLedgerTransactionTypes` | `operationLedgerRollbackTypes` |
|---|---|---|
| `retail_deposit` | `retail_deposit` | `retail_deposit_reversal` |
| `retail_withdrawal` | `retail_withdrawal_authorization` | `retail_withdrawal_authorization_reversal` |
| `agent_float_transfer` | **not supportable today — see below** | — |

**A genuine gap, stated rather than designed around.** ADR 0031's
cumulative aggregation query nets `ledger_entries` filtered by
`le.player_account_id` and `asset_code`. An `agent_float_transfer` has no
`player_account_id` on either leg, so a `cumulative_amount` rule on that
operation would aggregate to **zero** and silently never fire — the exact
fail-open behaviour ADR 0031 §6 exists to prevent. Making it work requires
a node-keyed aggregation in `internal/risk`, which is Risk's to build, not
`ledger-finance`'s to design. Until it exists, `cumulative_amount` on
`agent_float_transfer` is **unsupported and must be treated as
unavailable**, not quietly assumed to work. Per-transaction
`min_amount`/`max_amount` rules on it work unchanged, because those compare
`req.Amount` directly and are generic over `Operation`.

Status: **RESOLVED (integration point) — `NOT IMPLEMENTED`.** The rules,
thresholds and the node-keyed aggregation are `risk`'s.

### 5. Commission and revenue share — accounting treatment only

#### 5.1 When commission is earned, and when it is recognized

`ARCHITECTURAL DECISION`, reasoned rather than asserted.

Commission is *earned* continuously, as the underlying commissionable
activity occurs. It is *recognized in the ledger* by a **periodic
commission run**, one posting per node per period per asset — not per
commissionable event.

The alternative (accrue a posting on every commissionable bet) was
considered and rejected for two independent reasons:

1. **It is not generally computable per event.** Real retail commercial
   terms are commonly tiered and net-based ("18% of monthly GGR up to X,
   22% above"). A tiered rate applied to a single bet is undefined until
   the period total is known; a per-event accrual would have to be
   continuously revised, which means either mutating history or a stream of
   corrections proportional to turnover.
2. **Write contention.** Every bet would additionally touch a house-level
   `agent_commission_expense` row and a node-level payable row, materially
   widening the contention surface ADR 0019 already flagged for
   `house_gaming` and ADR 0032 widened again for `promo_liability`.

Before the run posts, commission is a **computable figure, not a ledger
balance** — the same honest position ADR 0032 §3 takes for a granted bonus
("a contingent liability, not a recognized expense"). The retail subsystem
may display it; it must not treat it as an authoritative balance, and it
must never be spendable float before §5.3 posts.

This is deliberately the same mechanism as Flow 17 (provider settlement),
which is already a periodic, statement-shaped, idempotency-keyed run. The
cadence (daily, weekly, monthly) is a commercial choice, not an
architectural one — a shorter period is the same posting, more often.

| Event | Entries |
|---|---|
| `agent_commission_accrual` `X` for node N, period p | Dr `agent_commission_expense` `X` · Cr `agent_commission_payable(N)` `X` |

Idempotency: `UNIQUE (tenant_id, idempotency_key)` with the key shaped
`retail-commission:{node_id}:{period_id}` — Flow 17's own
`"2026-08-provider-x"` pattern. A re-run of a closed period is an exact
retry and returns the original result; a re-run with a **different**
computed amount is a same-key-different-payload case and is **rejected**
(ADR 0020), never silently re-posted. A genuine restatement of a closed
period is a separate, reason-coded compensating accrual, never an
amendment.

Period attribution for reporting is carried on the transaction metadata
(`correlation_id` plus the period identifier); `posted_at` is when the run
executed. Those two are legitimately different and the reporting layer must
use the period, not `posted_at` — stated explicitly because conflating them
misstates every month boundary.

#### 5.2 What the commission is computed on

**`OPEN DECISION` — commercial, human sign-off required (CLAUDE.md "stop
and ask").** Exactly the same class as ADR 0032 §6(b)'s provider-funding
terms. Not resolved here, and not guessable:

- The **base**: GGR, NGR, turnover/handle, net player loss, deposits, or a
  fixed fee per transaction. These produce materially different numbers and
  different incentive structures.
- The **rate**, and whether it is flat, tiered, volume-stepped or
  negotiated per node.
- Whether bonus-funded play, voided rounds, reversed deposits and
  chargebacks are included in or excluded from the base — each of which is
  a real dispute waiting to happen, and each of which changes the ledger
  query the run executes.
- Whether commission is charged against `house_gaming` as
  contra-revenue or presented as an operating expense in statutory and
  partner reporting (a finance/reporting question that changes no posting
  above, exactly like ADR 0032's `bonus_expense` presentation question).

What is fixed **regardless** of the answer, and is `ledger-finance`'s to
assert: the computed amount must be **exactly recomputable from stored
inputs** (the stored base figure, the stored rate, the stored cap, the
stored rounding rule, the stored period bounds) as a reconciliation check,
and the rate is `NUMERIC`, never `FLOAT`/`DOUBLE`/`REAL` (§9.1). A rate
row for a closed period is **append-only**: superseding a rate creates a
new versioned row with its own effective window, never an in-place edit,
or a past period's commission stops being reproducible.

#### 5.3 How commission is discharged — and the netting trap

| Event | Entries |
|---|---|
| `agent_commission_capitalization` `X` (commission becomes spendable float) | Dr `agent_commission_payable(N)` `X` · Cr `agent_float(N)` `X` |
| `agent_commission_payout` `X` (real money paid to the node) | Dr `agent_commission_payable(N)` `X` · Cr `psp_clearing` / bank rail `X` |

**Netting is not a third mechanism.** Where a node owes the operator on
float and is owed commission, the settlement is *capitalization followed by
a smaller settlement* — two balanced groups, in one transaction or two,
both of which pass through `agent_commission_payable`.

> The trap, stated because it is the shape an implementer will reach for:
> a "net settlement" posted as `Dr agent_float X · Cr psp_clearing X` with
> no `agent_commission_payable` entry balances perfectly and is **wrong** —
> it discharges the commission liability without ever recording that
> commission existed. `agent_commission_payable` would grow without bound,
> `agent_commission_expense` would understate the true cost of the network,
> and NGR would be overstated by exactly the netted amount, permanently.
> A settlement that nets commission **must** contain the payable leg.

#### 5.4 Hierarchical commission (overrides)

A single commissionable event commonly produces commission at several
levels (a Super Agent earns an override on its Agents' production). The
accounting shape is unchanged and needs no new mechanism: the period run
posts **one debit to `agent_commission_expense` for the total** and **one
credit per earning node to that node's `agent_commission_payable`** —
N+1 entries, one transaction, balanced per asset.

The cascade *rules* (who earns an override on whom, at what rate, to what
depth) are commercial terms and belong to the same `OPEN DECISION` as
§5.2. Note that C → C is forbidden by invariant R2: an upper node's
override is its **own accrual from expense**, never a re-assignment of a
lower node's payable. Modelling it as a transfer would make one node's
commission reducible by another's, and would break the property that a
node's payable balance is what the operator actually owes that node.

Status: **§5.1/§5.3/§5.4 RESOLVED (architecture) — `NOT IMPLEMENTED`.
§5.2 is an `OPEN DECISION` (commercial, human sign-off).**

### 6. Cashier shift and till reconciliation

#### 6.1 Two different streams, deliberately not merged

`ARCHITECTURAL DECISION`. Retail adds **two** reconciliation streams of
genuinely different kinds, and collapsing them into one would make a
software bug and a cash shortage indistinguishable:

- **R3a — retail ledger-internal consistency.** Purely ledger-side
  figures: for every node, the node's `agent_float` net movement equals the
  sum of the postings its own terminals and its funding relationships
  produced. Zero tolerance, hourly, P1 on drift — the same class as
  `reconciliation-model.md` §2.1 and §2.9, and buildable with no
  counterparty relationship whatsoever.
- **R3b — shift/till reconciliation.** Declared physical cash movement
  against ledger-recorded retail postings for the same terminal and shift.
  This is the stream the directive asks for and is specified below.

#### 6.2 The till is not a ledger account

`ARCHITECTURAL DECISION`, and the load-bearing rule of this section.

**In the franchised model this ADR designs for by default, the physical
cash in a cashier's till belongs to the agent, not to the platform. It is
therefore not a `LedgerAccount`, not a platform asset, and not a platform
balance.**

This is the direct analogue of ADR 0032 §6(c) ("the platform's ledger never
mirrors a balance held in a provider's system"), and it is rejected for the
same reasons: the platform cannot control the till, cannot audit it without
a human count, and cannot correct it. Modelling it as a ledger account would
make the ledger's balance depend on a periodic human observation — a second
truth system whose drift has no correct resolution, since the platform
cannot post entries into someone else's cash drawer.

What *is* modelled is the shift: a `retail_shift` **workflow table** (like
`withdrawal_requests` — tenant-scoped, `FORCE ROW LEVEL SECURITY`, mutable
workflow fields, **never** a ledger table) carrying the terminal, the
cashier principal, the open/close timestamps, and the declared opening
count, declared cash in, declared cash out and declared closing count, per
asset. Declared counts are **observations**, audited on every change, and
never a source of authoritative balance.

`OPEN DECISION` (§11.1): the company-owned variant — where the operator
owns the shop and the cash — is a genuinely different custody model and
*would* require a `retail_cash_on_hand` debit-normal asset account. Both
models may coexist within one tenant. This carries custody, insurance,
AML and audit weight and is explicitly not `ledger-finance`'s to decide
alone.

#### 6.3 Invariant R3 — the check, its cadence and its tolerance

> **Invariant R3 (shift reconciliation).** For every closed shift, for
> every asset:
>
> `Σ ledger retail_deposit(terminal, shift) − Σ ledger retail_withdrawal_payout(terminal, shift)`
> `== declared_cash_in − declared_cash_out`
>
> **Tolerance: zero. No band.** Severity: **P1** on any non-zero
> difference. Cadence: evaluated at shift close, and swept by the hourly
> reconciliation job for shifts that closed since the last sweep (and for
> shifts left open beyond a configured maximum, which is itself a finding).

Zero tolerance is correct here, and is not an over-claim, precisely
*because* §6.2 keeps the platform out of the cash-custody business. R3 does
not compare the ledger to a cash count; it compares the ledger to the
**declared movements** the same terminal reported. A non-zero difference
therefore means one of exactly two things, both integrity events and
neither a rounding artefact:

- **A posting exists with no corresponding declared cash movement** — e.g.
  a player was credited but no cash was taken (fraud, terminal
  misconfiguration, a replayed key that should have been rejected).
- **A declared cash movement exists with no posting** — e.g. cash was taken
  and the player was never credited (the §3.5 lost-confirmation case, an
  offline queue that never drained, or theft).

A genuine cash *shortage* — the notes in the drawer being fewer than the
agent's own records say — is an **agent-side loss in the franchised model
and does not appear in the platform's ledger at all**, because the platform
never had custody. Stating that explicitly is what lets R3 stay
zero-tolerance instead of acquiring a fudge factor that would hide the two
real failure modes above.

#### 6.4 Correction mechanism

Mirroring `reconciliation-model.md` §2.1's "the projection is rebuilt from
the ledger, never the reverse":

- **A shift variance is never resolved by adjusting the ledger to match the
  declaration**, and never by editing or deleting a posted retail
  transaction.
- A missing posting is resolved by **posting it through the ordinary
  idempotent retail path** — the same code path a live terminal would use,
  not a "backfill" path, so every invariant still applies uniformly. This
  is `reconciliation-model.md` §2.2's established pattern.
- A posting with no cash movement is resolved by a **compensating
  transaction** with `reverses_transaction_id` set and a mandatory reason
  code.
- Where the resolution is that the node owes the operator (or vice versa),
  it is settled **against that node's `agent_float`**, with a four-eyes
  `manual_adjustment` or a dedicated `till_variance` type (§7.1), and
  **never against a player's wallet**. A player's balance is not an
  adjustment surface for an operational discrepancy they had no part in.
- Every mismatch is a `ReconciliationMismatch` row, never a log line —
  auditable, assignable, trackable to resolution
  (`reconciliation-model.md` §5), and logged at `Error` level per ADR 0023.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.** Requires two new
streams in `reconciliation-model.md` (§2.11 R3a, §2.12 R3b) and R1/R2/R3
added to `ledger-accounting-model.md` §6.

### 7. Four-eyes and approval controls

`ARCHITECTURAL DECISION`, mirroring CLAUDE.md's manual-adjustment rule,
ADR 0032 §7, and `withdrawal-policy-configuration.md`'s
resolved-per-tenant/brand/asset threshold mechanism. Nothing here is a new
approval primitive.

#### 7.1 Where it applies

| Money movement | Control |
|---|---|
| **Manual adjustment to a node's `agent_float`** | Four-eyes above a configured per-asset threshold, mandatory `reason_code`, fully audited. See the stricter-tier recommendation below. |
| **Float transfer between nodes above a configured threshold** | Four-eyes, per-asset threshold, reason code. |
| **Commission rate override, or a manual commission adjustment** | Four-eyes, reason code, and the override is written as a **new versioned rate row**, never an in-place edit (§5.2). |
| **Agent settlement (real money out to a node)** | Approval gate required — see §11.3, this is an outbound money path with no existing state machine. |
| **Opening or changing a credit line** (if post-pay is ever authorized, §1.4) | Four-eyes, reason code, per-node and per-asset. |
| **Till-variance write-off above a configured threshold** (§6.4) | Four-eyes, reason code. |

Two retail-specific hardening rules:

- **Self-approval exclusion.** A principal belonging to (or in the
  management chain of) the node whose float is being adjusted may **never**
  be one of the required approvers on that movement. This is new: the
  existing four-eyes rule assumes two operator staff approving a player's
  withdrawal, where neither is the beneficiary. In retail the beneficiary
  has principals inside the system.
- **Distinct humans, not distinct logins.** The approver count dedupes by
  `staff_users.person_id` where linked, per ADR 0023 §1 — carried forward
  unchanged, and more load-bearing here because an agent network has many
  more principals than a back office does.

#### 7.2 Thresholds are per-asset, always

`withdrawal-policy-configuration.md` §1 already paid for this lesson: a
single process-wide minor-unit constant is silently asset-blind
(`100000` is EUR 1,000.00, or 0.001 BTC, or 1,000,000 USDT). Every retail
threshold — approval, transfer, variance, credit line — resolves from a
per-`(tenant, brand?, asset)` configuration row with a **fail-closed
default of zero** (i.e. everything requires full approval until a tenant
configures otherwise), reusing the `ResolveApprovalPolicy` shape rather
than inventing a second resolver.

#### 7.3 A recommendation into an existing open decision

`financial-transaction-flows.md` §16 records an `OPEN DECISION`: whether
`manual_adjustment` against house/system-level accounts should require a
stricter approval tier or be excluded entirely. Retail sharpens it,
because an `agent_float` adjustment **mints spendable capacity that becomes
player cash at the next counter transaction** — a shorter path from a
staff action to real player balance than any existing house account offers.

`RECOMMENDATION` (not a unilateral resolution; the open decision remains
`ledger-finance` + `security`): `agent_float` belongs in the stricter
tier — a restricted reason-code set, a higher approval count, and a
mandatory link to a `ReconciliationMismatch` or an incident reference where
the adjustment is a correction.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`**, except §7.3 which
is a `RECOMMENDATION` into an existing open decision.

### 8. Idempotency — which key shape a cashier terminal uses, and why

A POS/cashier terminal is a **new originating actor class**. ADR 0019's
actor matrix has four rows — player session, verified provider callback,
internal service, staff principal — and a terminal is none of them. Getting
the key shape right is therefore a real decision, not an inheritance.

#### 8.1 The decision

`ARCHITECTURAL DECISION`: **cashier-originated retail postings use
`UNIQUE (tenant_id, idempotency_key)`, never
`(tenant_id, provider_id, provider_tx_id)`.**

Why not the provider key:

- `provider_id` in this platform means "an external system behind a
  `PaymentProvider`/`CasinoProvider`/`CryptoCustodyProvider` adapter, with
  a `ProviderCapability` row, a credential, a callback signature and a
  settlement statement we reconcile against" (ADR 0019, ADR 0022). A
  terminal has none of those. It has no books of its own to reconcile
  against — which is exactly why §6 needed a *declaration* stream rather
  than a statement stream.
- ADR 0019's per-provider scoping refinement ("a callback authenticated
  with provider X's credential may originate only the `transaction_type`s
  consistent with X's own declared `ProviderCapability`") has no meaning
  for a terminal, so putting terminals in the provider namespace would
  either weaken that rule or require a fake capability row per terminal.
- Provider references are opaque strings from a counterparty. A terminal's
  reference is **minted by our own client software**, which is a different
  trust posture and needs the hardening in §8.2.

The key shape:

```
idempotency_key = 'retail:' || <terminal_id> || ':' || <terminal_operation_id>
```

- `<terminal_id>` is the **server-resolved** terminal identity, taken from
  the authenticated terminal credential/session — **never** from the
  request payload.
- `<terminal_operation_id>` is a client-minted, durable, monotonic or
  ULID/UUIDv4 identifier, generated **once** when the cashier commits the
  operation on the terminal, written to the terminal's local durable log,
  and **replayed verbatim on every retry**, never regenerated per delivery
  attempt. A terminal that mints a fresh id per attempt has defeated the
  mechanism entirely — the same failure ADR 0032 §8 calls out for the
  Reward Orchestrator, and the single most likely way this design fails in
  practice.
- The key is tenant-scoped like every other key in this platform
  (`ledger-accounting-model.md` §3).

For the two-step withdrawal (§3.3): **Step A** is keyed by the terminal's
own operation id as above. **Step B** is keyed by the server-side retail
withdrawal authorization's id plus a `:payout` discriminator — Flow 3's
own "Step A keyed by the withdrawal request's own id" pattern — so a
re-confirmation is idempotent regardless of which terminal or shift sends
it, and two terminals cannot both discharge one authorization.

#### 8.2 The namespace-claiming attack — genuinely new to this platform

Stated explicitly because it is the one security property a terminal has
that a provider does not. A provider namespace is 1:1 with a credential; a
tenant has **many** terminals, all writing into one
`(tenant_id, idempotency_key)` space.

If `<terminal_id>` were taken from the payload, terminal A could write into
terminal B's namespace, producing two distinct attacks:

1. **Replay/steal** — A submits a key already used by B and receives B's
   original transaction result via ADR 0020's exact-retry path, learning a
   transaction it has no right to see.
2. **Namespace squatting / denial** — A pre-inserts keys in B's future
   namespace (trivial where `<terminal_operation_id>` is a predictable
   sequence), so B's genuine transactions are later rejected as
   same-key-different-payload. A retail counter that cannot transact is an
   outage with a cash queue in front of it.

Both are closed by resolving `<terminal_id>` server-side from the
authenticated credential — the identical rule ADR 0019 already applies to
`provider_id` ("resolved from the credential that verified the callback
signature, never from the payload"). The `RECOMMENDATION` that follows:
`<terminal_operation_id>` should be a random ULID/UUIDv4 rather than a
predictable per-terminal counter, so squatting is infeasible even if the
server-side resolution were ever weakened.

#### 8.3 Inherited rules, restated because retail is where they bite

- **Same key, different payload → reject** (`ledger.ErrIdempotencyKeyReused`),
  never silently apply and never silently return the old result. This
  matters more here than anywhere else: a terminal replaying a stale local
  queue after a software update is a realistic way to re-submit an old key
  with a new amount.
- **Concurrent duplicates are arbitrated by the database constraint**,
  never by check-then-insert, and the exact-retry lookup uses ADR 0020's
  `SAVEPOINT` pattern.
- **A reversal of a never-seen retail transaction writes a tombstone**
  occupying the same `(tenant_id, idempotency_key)` slot, so a
  late-arriving original from a terminal that was offline is rejected
  rather than posted after its own cancellation.
- **Double-reversal protection**: the original is selected `FOR UPDATE`
  and rejected if a transaction already reverses it.

#### 8.4 Offline terminals — a blocking position, and an open decision

`ARCHITECTURAL DECISION` (position) + `OPEN DECISION` (business):

> **A retail posting is authorized online, inside the posting transaction,
> or it does not happen.** A terminal may queue an *intent* while offline;
> it must not promise a player a credit, or hand over cash against a
> balance, that the ledger has not accepted.

The reason is not conservatism. `ledger-accounting-model.md` §6 invariant
#15 requires the authoritative balance read to happen in the same database
transaction as the write. An offline terminal cannot read
`agent_float(node)` or `player_cash(wallet)` authoritatively, so an offline
deposit can overdraw a float that another terminal on the same node has
already spent, and an offline payout is a player double-spend by
construction. Neither is detectable until the queue drains, by which point
the cash has physically moved.

**`OPEN DECISION` (business + product, human sign-off):** genuinely
offline-capable retail is a common requirement in low-connectivity LATAM
and African markets and may well be a real commercial requirement here. If
it is, it needs its **own decision and its own design** — bounded
per-terminal pre-authorized capacity, a hard offline ceiling, an explicit
acceptance of the residual double-spend risk, and a settlement process for
the exceptions — and not a quiet relaxation of invariant #15 inside a
retail code path. Raised to the Master Orchestrator rather than
worked around.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`**, with §8.4's
offline capability an explicit `OPEN DECISION`.

### 9. No floating point, integer minor units, per-owner-per-asset

`ARCHITECTURAL DECISION`, binding for retail exactly as ADR 0032 §9 binds
it for bonuses. Retail is the first surface where money is physically
counted by a human, so this section names the three retail-specific ways it
will be broken.

#### 9.1 The rule, unchanged

Every amount is `NUMERIC(38,0)` minor units, scaled by the asset's
`decimal_exponent` looked up from the `Asset` registry, never hardcoded and
never assumed to be 2 (ADR 0007, ADR 0021). Commission rates, override
rates and any cap are `NUMERIC`, never `FLOAT`/`DOUBLE`/`REAL` — they are
not money but they *multiply* money, which is the same reasoning ADR 0021
applies to `exchange_rate` and ADR 0032 §9 to match percentages. A
commission computed in floating point is a defect and a blocking review
finding, not a shortcut.

#### 9.2 The POS client is a new floating-point risk surface

Stated because it is outside every existing review checkpoint. POS
applications are typically JavaScript/Android/embedded clients whose
default numeric type is a double. A cash amount must travel **end to end as
an integer minor-unit value plus an asset code** — never as a decimal
string parsed into a float, never as a "major units" number, and never
re-derived on the client from a displayed string. Denomination handling
(3 × €20 + 1 × €10) is an integer count times an integer minor-unit
denomination value, summed in integers. `code-reviewer` and
`ledger-finance` checkpoint on any retail client contract.

#### 9.3 Cash rounding — a retail-only problem with no online analogue

A cashier physically cannot pay 12.34 where the smallest circulating coin
is 0.05, and several target-market currencies have minor units that no
longer circulate at all. This has no equivalent in any existing flow.

`ARCHITECTURAL DECISION`: **the ledger posts exactly what moved.** Where a
jurisdiction/asset requires physical-cash rounding, the rounded difference
is an **explicit, separately-identified entry** against
`cash_rounding_difference` (or `house_gaming`, §1.2) within the same
transaction — never a silent truncation of the player's balance, and never
a payout posted for an amount different from the amount handed over. A
player's balance must never change by an amount nobody can account for.

The rounding *rule* per asset and jurisdiction is an `OPEN DECISION`
(finance + compliance) and **inherits ADR 0021's open rounding decision** —
it must not resolve to a second, different convention invented for retail,
for the same reason ADR 0032 §9 refuses to invent one for bonuses.

#### 9.4 Per-owner-per-asset, and the multi-currency counter

- A node holds **one `agent_float` per asset** (§1.2). Funding a node in
  EUR does not give it capacity to fund USD deposits. There is no generic
  "float balance with a currency field" — ADR 0007's rule applies to nodes
  as it does to players.
- A player wallet is credited **in the asset the cash was tendered in**.
  A counter that accepts USD cash and credits a EUR wallet is performing a
  cross-asset movement, which is only ever an explicit
  `ConversionOperation` (ADR 0007/0021) and never an implicit leg of a
  deposit.
- **Consequence, stated as a blocker rather than discovered later:**
  `ConversionOperation` is designed but `NOT IMPLEMENTED`, and its
  conversion clearing account remains an `OPEN DECISION`
  (`ledger-accounting-model.md` §2, ADR 0021). **Multi-currency retail
  counters are therefore `BLOCKED` on that open decision.** A
  single-currency counter per asset works with no conversion at all and is
  the supported shape until it is resolved.

Status: **RESOLVED (architecture) — `NOT IMPLEMENTED`.** §9.3's rounding
direction and §9.4's conversion clearing account are inherited `OPEN
DECISION`s, not new ones.

### 10. Conflict check against already-approved architecture

Performed explicitly, per the directive. Where retail genuinely needs
something that contradicts prior-approved architecture it is flagged for
Master Orchestrator sign-off, never silently redesigned.

| Prior decision | Conflict? | Disposition |
|---|---|---|
| **ADR 0019 / `ledger-accounting-model.md` §1.1** — two owner families (`wallet_id` or house) | **YES — real conflict** | §1.3. A node-scoped account fits neither unique constraint and would silently collapse into one shared tenant-wide float. `OPEN DECISION`, recommended shape (b), **blocking precondition** for any retail posting. |
| **ADR 0019 actor matrix** — four actor classes | **YES — gap, not contradiction** | A POS/cashier terminal is a fifth originating actor class and must be added, with its own permitted `transaction_type` set and the §8.2 credential-resolved-identity rule. `architect` + `security` own the matrix edit; §8 states the requirements it must satisfy. |
| **ADR 0019 RLS model** — tenant scope + player scope + staff RBAC | **YES — gap** | §1.3. Node-scoped accounts need a third read scope. `security` + `architect`'s call; `ledger-finance`'s binding constraint is the OR'd-permissive-policy requirement so retail postings do not silently update zero projection rows. |
| **ADR 0019 projection grain** (one row per `ledger_account_id`) | **No** | Already accommodates node-scoped accounts with no change. The earlier decision was right and is reused, not amended. |
| **ADR 0020** — idempotency and concurrency | **No** | §8 uses the existing `(tenant_id, idempotency_key)` mechanism and the existing `SAVEPOINT`/`FOR UPDATE` patterns verbatim. No new mechanism. |
| **ADR 0021** — asset/exponent/`NUMERIC(38,0)` model | **No** | Inherited unchanged (§9). Retail *depends on* ADR 0021's unresolved conversion clearing account for multi-currency counters (§9.4) and on its unresolved rounding direction (§9.3) — a dependency, not a contradiction. |
| **ADR 0007** — multi-wallet per player | **No** | Extended by analogy (one float per node per asset), explicitly **not** by reusing `Wallet` for nodes (§1.3(a), rejected). |
| **ADR 0032 / invariant B1** — bonus mirror | **No** | Retail posts no `player_bonus` entry, so B1 is untouched. A retail-funded player playing a bonus-funded round is ordinary Flow 5 and carries B1's mirror legs exactly as today. |
| **ADR 0031** — one risk engine, no per-domain limit engines | **No** | §4 consumes `risk.Evaluate` and builds nothing. Two honest gaps named: node-keyed cumulative aggregation, and the three DOCUMENTED-ONLY `Operation` values. |
| **`reconciliation-model.md` §2.1–§2.10** | **No** | Two new streams added (§6.1); no existing stream changes. §2.6's "`psp_clearing` drains toward zero" expectation is *protected* by invariant R1 rather than threatened by it. |
| **`financial-transaction-flows.md` §16** — `manual_adjustment` scope open decision | **Sharpened, not contradicted** | §7.3 adds a `RECOMMENDATION` into that still-open decision. |
| **Flow 18** — no bank/treasury account type exists | **Inherited blocker** | §11.2. Agent funding and settlement outside a PSP rail cannot balance inside the ledger until Flow 18's `OPEN DECISION` is resolved. |
| **CLAUDE.md** — never `UPDATE` a balance; balance is a projection | **No** | §0. There is no `agent_balance` or `till_balance` column anywhere in this design. |
| **Blueprint** | **n/a** | The Blueprint contains no retail model. Nothing here is labelled `BLUEPRINT`; see the header. |

### 11. Known gaps and blocked items this ADR surfaces

#### 11.1 Franchised vs. company-owned retail — a custody decision

`OPEN DECISION`, **business + legal + insurance**, referred upward per this
specialist's own limitation on custody decisions with legal/insurance
weight.

This ADR designs the **franchised** model (the agent owns the cash in the
till; the platform owes the agent float). A **company-owned** model (the
operator owns the shop and its cash) is a different custody position and
requires a `retail_cash_on_hand` debit-normal asset account per node per
asset, with the till then genuinely being a platform asset that must be
counted, insured, and reconciled as such — and §6.2's "the till is not a
ledger account" rule would not apply to those nodes. Both models can
coexist within one tenant, keyed per node.

Not decidable by engineering: it determines who bears cash-loss risk,
whether the cash is on the operator's balance sheet, what insurance is
required, and how AML source-of-funds obligations attach.

#### 11.2 Agent funding/settlement outside a PSP rail is `BLOCKED`

`financial-transaction-flows.md` §18's `OPEN DECISION` — there is no
bank/treasury account type in the Blueprint's ten, and the bank leg of a
PSP batch is currently described as "out of ledger scope" — means a direct
bank transfer from or to an agent **has no account to balance against**.

Disposition: agent funding and settlement **over an existing PSP rail**
work today against `psp_clearing` and need nothing new. Direct bank
transfers are `BLOCKED` on Flow 18's resolution (option (a), a
`bank_treasury` account type, would unblock both). Not resolved here —
it is a treasury-accounting decision that predates retail and should not
be settled as a side effect of it.

#### 11.3 Agent payouts have no approval state machine

`OPEN DECISION` (`payments` + `architect` + `ledger-finance`). An
`agent_settlement` or `agent_commission_payout` is an **outbound money
path to a non-player counterparty**, and no such workflow exists:
`withdrawal-state-machine.md` is player- and `WithdrawalRequest`-shaped
throughout.

`RECOMMENDATION`: reuse the withdrawal approval *policy* mechanism
(`ResolveApprovalPolicy`, per-tenant/brand/asset thresholds, four-eyes,
step-up boundary) rather than inventing a second approval concept. Whether
the *workflow object* is a `WithdrawalRequest` variant or a distinct
`AgentPayoutRequest` is `payments`/`architect`'s call. What is **not**
optional: an outbound money path without an approval gate is not
acceptable, and this one is larger and less frequently reviewed than a
typical player withdrawal.

#### 11.4 Anonymous / bearer retail play is not designed

`OPEN DECISION` (product + compliance). This ADR assumes every retail
player has a `PlayerAccount` and therefore a `Wallet` — that assumption is
what makes §3's postings possible at all.

Anonymous or bearer-instrument retail play (a paper ticket with no account,
common in LATAM retail) is a **fundamentally different accounting object**:
there is no wallet to credit, so the operator's obligation is a bearer
liability against an instrument rather than a balance against a player, and
it interacts directly with KYC/AML thresholds and jurisdictional rules on
anonymous gambling. It is flagged here, not designed. If it is in scope,
it needs its own decision, its own account type and its own
`identity-compliance` analysis.

#### 11.5 Retail deposit reversal inherits Flow 2's negative-balance question

A `retail_deposit_reversal` (a fraudulent or erroneous counter deposit,
found after the player has played) can drive `player_cash` negative,
exactly as Flow 2 can. `financial-transaction-flows.md` §2's `OPEN
DECISION` on negative-cash-balance policy applies unchanged and is not
re-answered here. Note the retail-specific twist worth carrying into that
decision: the offsetting recovery in retail has a natural target (the
originating node's `agent_float`) that an online chargeback does not — but
whether the agent bears that loss is a **contract term**, not an
engineering choice.

## Consequences

- **Follow-up edits required to documents this ADR does not own the right
  to change in this stage** (all `NOT IMPLEMENTED` until made):
  `ledger-accounting-model.md` §1.1 (the third owner family, §1.3), §2
  (the three new account types and their rows), §6 (invariants R1, R2,
  R3); `financial-transaction-flows.md` (new flows for retail deposit,
  retail withdrawal steps A/B, agent funding, agent float transfer, agent
  settlement, commission accrual/capitalization/payout, plus the summary
  table); `reconciliation-model.md` (§2.11 R3a, §2.12 R3b); ADR 0019's
  actor matrix (the terminal actor class) and RLS section (node scope);
  ADR 0031 §13's table (three retail rows) and §16 (three proposed
  `Operation` values, DOCUMENTED ONLY).
- **Migrations required before any retail posting** (additive, own stage):
  the `ledger_accounts` third owner dimension and its partial unique index
  (§1.3, pending that `OPEN DECISION`); the `agent_float`,
  `agent_commission_payable`, `agent_commission_expense` account types;
  the `retail_deposit`, `retail_deposit_reversal`,
  `retail_withdrawal_authorization`,
  `retail_withdrawal_authorization_reversal`, `retail_withdrawal_payout`,
  `agent_funding`, `agent_float_transfer`, `agent_settlement`,
  `agent_commission_accrual`, `agent_commission_capitalization`,
  `agent_commission_payout` transaction types added to the `CHECK`
  constraint on `ledger_transactions.transaction_type`. Until then every
  retail posting is `BLOCKED` by the existing constraint — which is the
  intended behaviour, not an obstacle to route around.
- **Two new reconciliation streams**, both internal and buildable with no
  vendor relationship: R3a (hourly, zero tolerance, P1) and R3b (at shift
  close plus hourly sweep, zero tolerance, P1).
- **Write contention is *reduced* relative to the bonus case, not
  increased.** `agent_float` and `agent_commission_payable` are per-node
  rows, so retail volume shards naturally across the network rather than
  concentrating on a single house row. `agent_commission_expense` is a
  single house row per `(tenant, asset)` but is touched only by periodic
  commission runs, not per transaction. This is a deliberate property of
  the per-node account design, not an accident.
- **Testing floor** (CLAUDE.md's financial test list, non-negotiable,
  owned by `ledger-finance`). A happy-path retail deposit test does not
  satisfy this:
  - Normal retail deposit; normal two-step retail withdrawal; agent
    funding; float transfer down a four-level chain; agent settlement.
  - Duplicate/replayed terminal key returns the original result; same key
    with a different amount is **rejected**; concurrent duplicates
    arbitrated by the DB constraint.
  - Two terminals cannot collide in each other's key namespace, and a
    payload-supplied `terminal_id` cannot influence the key (§8.2).
  - Concurrent retail deposits against **one** agent float: exactly the
    available float can be spent, never more; an overdraw is rejected, not
    clamped.
  - Concurrent retail payout and online withdrawal on **one** player
    wallet.
  - A retail transaction containing a `psp_clearing`/`psp_reserve` entry
    is **rejected** (invariant R1).
  - A transaction touching two nodes' floats that is not an
    `agent_float_transfer` is rejected; a transaction with a player leg and
    two float legs is rejected (invariant R2).
  - Payout Step B against an expired, unknown or already-discharged
    authorization is rejected; hold expiry restores `player_cash` exactly;
    Step B retried after success is idempotent.
  - Reversal of a retail deposit is a compensating transaction, never an
    edit; double-reversal race rejected; reversal of a never-seen retail
    transaction writes a tombstone and a late original is then rejected.
  - Commission run is idempotent per node per period; a re-run with a
    different computed amount is rejected; a netted settlement missing the
    `agent_commission_payable` leg is rejected (§5.3); a hierarchical run
    produces one expense debit and N payable credits that balance.
  - R3b detects a posting with no declared cash movement, and a declared
    cash movement with no posting, in both directions.
  - Four-eyes on an `agent_float` manual adjustment; self-approval by a
    principal of the affected node is refused; two logins of one person
    do not satisfy two approvals.
  - An RG-ineligible player is refused a retail deposit; a `risk.Evaluate`
    `deny`/`review`/error blocks before any entry is written.
  - R1, R2 and R3 asserted after every one of the above.
- **`ledger-finance` sign-off is required** on
  `docs/architecture/26-retail-operations-architecture.md` and on any
  retail implementation before either is marked complete, limited to their
  monetary aspects. Nothing in this ADR gives `ledger-finance` a view on
  the hierarchy model, the RBAC shape, terminal provisioning, or the agent
  back-office UX.

## Open decisions referred upward

1. **Third owner family on `ledger_accounts`** (§1.3) — `architect` +
   `security` + `ledger-finance`. **Blocking precondition** for any retail
   posting; recommended shape stated, not adopted unilaterally.
2. **Commission structure, base, rate and cascade terms** (§5.2) —
   **commercial contract, human sign-off** (CLAUDE.md "stop and ask"),
   exactly the class of ADR 0032's provider-funding decision.
3. **Post-pay agents / credit lines** (§1.4) — commercial + credit-risk +
   **legal** (credit-funded gambling is regulated or prohibited in several
   target jurisdictions).
4. **Franchised vs. company-owned retail cash custody** (§11.1) —
   business + legal + insurance. Determines whether `retail_cash_on_hand`
   exists at all.
5. **Genuinely offline-capable terminals** (§8.4) — business + product.
   Conflicts with invariant #15; needs its own design if required, never a
   quiet relaxation inside retail code.
6. **Agent payout approval workflow** (§11.3) — `payments` + `architect`.
   An outbound money path currently has no state machine.
7. **Anonymous / bearer retail play** (§11.4) — product + compliance.
8. **Bank/treasury account type** (§11.2) — inherits Flow 18's existing
   open decision; treasury accounting, not retail.
9. **Cash-rounding direction per asset/jurisdiction** (§9.3) — inherits
   ADR 0021's open rounding decision; must not be answered separately for
   retail.
10. **Conversion clearing account** (§9.4) — inherits
    `ledger-accounting-model.md` §2 / ADR 0021. Multi-currency retail
    counters are `BLOCKED` until resolved.
11. **`manual_adjustment` stricter tier for house/node accounts** (§7.3) —
    inherits `financial-transaction-flows.md` §16's open decision, now with
    a `ledger-finance` `RECOMMENDATION` attached.
12. **Node-keyed cumulative aggregation in `internal/risk`** (§4) — `risk`.
    Until it exists, `cumulative_amount` on `agent_float_transfer` is
    unsupported and must not be presented as working.
13. **Is retail authorized product scope at all** — Master Orchestrator +
    `product-owner-proxy`. Retail has no Blueprint anchor (see header);
    this ADR answers "how is it accounted for", never "should it be built".

## Assumptions about work in progress in parallel

Recorded so the Master Orchestrator can verify consistency once all Wave-1
documents land. If any assumption is wrong, the named section of this ADR
is the one to revisit.

**About `docs/architecture/26-retail-operations-architecture.md`
(`architect`):**

1. A hierarchy node is a **tenant-owned object** with a stable id,
   `tenant_id NOT NULL`, RLS, and a parent link — and a node never spans
   tenants. (§1.2, §10; cross-tenant funding is already impossible via the
   existing single-`tenant_id`-per-transaction rule and the entry-level
   composite FK, invariant #5.)
2. **Depth and structure are configuration, not code** — the four named
   levels are one tenant's configuration, not an enum. §3.1's postings are
   depth-agnostic and stay correct for two levels or six.
3. There exists a server-side authorization predicate answering "may this
   actor move float from node A to node B", and it is evaluated **before**
   any `agent_float_transfer` is posted. This ADR requires its existence
   and its position; it designs neither.
4. `agent_float` is scoped **tenant + node + asset**, deliberately **not**
   tenant + brand + node + asset — an agent's commercial relationship is
   with the licensed operator, so one agent may fund players across several
   brands of the same tenant. This mirrors `financial-domain-model.md`'s
   existing "house-level accounts are tenant-scoped, not brand-scoped"
   reasoning. **If doc 26 makes nodes brand-scoped, §1.2's scope key
   changes and this ADR must be revised.**
5. A cashier terminal has a **distinct, server-resolvable identity** with
   its own credential (§8.2 depends on this absolutely), and a shift links
   a terminal to a cashier principal and a node.
6. Whether a cashier is itself a funded node or posts against its parent's
   float is a **per-tenant configuration**; §3.1 states the accounting is
   identical either way.

**About the `risk` specialist's parallel ADR 0031 extension:**

7. The three `Operation` values in §4 are proposed by this ADR but
   **owned by `risk`** — if `risk` names them differently, `risk`'s naming
   wins and §4's table is corrected. What must not change is the *position*
   of the call: same transaction as the posting, before commit, fail-closed.
8. `risk` accepts the `operationLedgerTransactionTypes` /
   `operationLedgerRollbackTypes` mapping in §4 as the `ledger-finance`
   answer ADR 0031 §16 step 5 reserves for this specialist.
9. `risk` owns the node-keyed cumulative aggregation gap (§4, open decision
   12). This ADR states the gap and explicitly does not design around it.
10. Credit-line limits for post-pay agents, if §1.4's open decision is ever
    answered yes, are **`risk_rules` configuration**, never a ledger
    constant and never a retail-owned table.

## Owner

`ledger-finance`. The hierarchy/role model and the who-may-fund-whom
authorization shape are owned by `architect`; RBAC, the terminal actor
class and node-scoped RLS by `security`; limit rules by `risk`; agent
payout orchestration by `payments`. None of them may alter the accounting
treatments above without `ledger-finance` sign-off, and `ledger-finance`
does not alter their designs.

## Cross-references

`CLAUDE.md` ("Financial / ledger rules", "Multi-tenancy", "Security");
ADR 0001, 0007, 0012, 0019, 0020, 0021, 0022, 0023, 0031, 0032;
`docs/architecture/ledger-accounting-model.md`,
`financial-domain-model.md`, `financial-transaction-flows.md`,
`reconciliation-model.md`, `withdrawal-state-machine.md`,
`withdrawal-policy-configuration.md`,
`06-wallet-ledger-architecture.md`;
`docs/architecture/26-retail-operations-architecture.md` (in progress,
`architect` — hierarchy/role model and funding authorization);
`internal/ledger`, `internal/risk`, `internal/rg`, `internal/casino`
(`postBet`, `postRollback` — the established gate-then-post and
compensating-reversal patterns this ADR reuses).

# 24 — Points Accounting Architecture

Status: **`NOT IMPLEMENTED`** — architecture only, produced in Stage 4H-A
(Bonus/Gamification/Reward Architecture Freeze). No table, no migration
and no Go package described here exists. Owner: `ledger-finance`. The
business rules that *decide* point movements are owned by the Stage 4H-A
Gamification architecture (`architect`); this document owns only how those
decisions are durably and auditably **recorded**.

Source: CLAUDE.md ("Financial / ledger rules" — applied here by analogy,
see §1), ADR 0007 (the `Asset` registry precedent), ADR 0013 (append-only
enforcement and audit), ADR 0016 (RLS `UPDATE`-visibility), ADR 0019
(projection architecture), ADR 0020 (idempotency and concurrency), ADR
0032 (bonus accounting — where the money/non-money line is drawn).

Labeling convention as elsewhere in `docs/architecture`:
`ARCHITECTURAL DECISION` = decided here or in a named ADR;
`RECOMMENDATION` = a position that still needs sign-off; `OPEN DECISION` =
deliberately unresolved, with the decision-maker named.

## 1. Position: points are not money, and get their own ledger

`ARCHITECTURAL DECISION`.

Loyalty/VIP points are **not** a wallet balance and never appear in
`ledger_transactions`/`ledger_entries`, in `wallets`, in
`wallet_balance_projection`, or in any per-asset balance. ADR 0032 §1
draws the line: a reward is money if it has a defined redemption value
denominated in a registered `Asset`, or if the platform would owe the
player something denominated in an `Asset` on request. Points, as designed
here, satisfy neither — they are a **claim on a catalogue of redemptions**,
not a claim on an asset (see §7, which is what keeps that sentence true).

Why a separate table family rather than a `PointType` masquerading as an
`Asset`:

1. **The money ledger's central invariant is per-asset.**
   `SUM(debits) == SUM(credits)` is enforced per `asset_code` by a deferred
   constraint trigger. Injecting a non-asset "asset" into that registry
   either makes the invariant meaningless for that row or forces every
   money-side query, report and reconciliation stream to filter points
   out — a filter that will be forgotten exactly once, in exactly the
   place that matters.
2. **Every monetary reconciliation stream would have to learn about
   points.** PSP, custodian, casino-GGR and provider-payable
   reconciliation (`reconciliation-model.md` §2) all join on money
   accounts. None of them has a points counterparty; all of them would
   need a carve-out.
3. **Blast radius.** A gamification bug that mints a billion points must
   not be able to corrupt, lock, or even contend with a money account. A
   separate table family makes that structural rather than careful.
4. **Different regulatory and audit scope.** Points are not player funds,
   are not segregated, are not withdrawable, and do not enter AML
   source-of-funds analysis. Conflating the stores would drag points into
   a regime they do not belong in, and would give points the credibility
   of player funds, which they must not have.

What is **not** different: every *design principle*. Points are append-only,
double-entry, idempotent, concurrency-safe, projection-based and
reconciled — not because points are money, but because the properties that
make a money ledger trustworthy (reproducibility, dispute-answerability,
no silent mutation) are exactly the properties a player disputing a
vanished points balance needs, and because a contributor who learns one
posting discipline in this codebase should not have to learn a second,
weaker one.

## 2. Point type plurality — multiple named types, via a registry

`ARCHITECTURAL DECISION`: the platform supports **multiple named point
types**, not one global points currency. A `PointType` registry is
introduced, deliberately modeled on the `Asset` registry (ADR 0007/0021).

Conceptual shape (not DDL):

```
PointType
  tenant_id                 -- NULLABLE: NULL = platform-wide DEFINITION/TEMPLATE,
                             -- non-NULL = tenant-owned definition; see the scope
                             -- correction immediately below this block
  code                      -- UNIQUE (tenant_id, code)
  brand_id                  -- nullable = all brands OF THAT TENANT, never all tenants
  display_name
  exponent                  -- default 0 (whole points); see §3
  transferable              -- default false (player-to-player transfer is out of scope)
  default_expiry_policy     -- gamification interprets it; the ledger only records results
  active
```

**Specialist-review correction on scope (P1 fix)**: an earlier draft of
this section made `PointType.tenant_id` `NOT NULL` ("no platform-global
point type exists"), reasoning from the liability argument in point 1
below. Cross-document review found this conflates two genuinely different
things: the **registry row** (a *definition* — a name, an exponent, a
default expiry policy) versus the **balance** (a *liability* — an actual
player's accrued points in that type). The liability argument is correct
and binding for the balance side (`PointAccount`/`PointTransaction`/
`PointEntry` below remain `tenant_id NOT NULL`, always — a platform-wide
balance row would be incoherent, exactly as originally argued) but does
**not** extend to the definition side: a `PointType` *definition* is a
template, exactly like a platform-wide `risk_rules` row (ADR 0031) or a
platform-wide Bonus Campaign template
(`docs/architecture/10-bonus-engine-architecture.md` §8) — both of which
this codebase already allows to be `tenant_id IS NULL`. Disallowing a
platform-wide `PointType` *definition* would make it impossible for a
platform-wide Bonus Campaign template, Marketplace item template, or
Level programme template (all of which reference a `PointType.code` for
pricing/denomination — see `docs/architecture/17-gamification-engine-
architecture.md` §9.1, `docs/architecture/20-reward-marketplace-
architecture.md` §3) to ever exist, contradicting those documents. The
corrected model: **`PointType` (the definition) is dual-scope, nullable
`tenant_id`, mirroring `risk_rules`'s exact pattern — a tenant may
instantiate/reference a platform-wide `PointType` definition, but every
actual balance row for that type remains tenant-scoped and non-nullable.**
This also corrects §9's RLS section below, which previously justified the
`NOT NULL` choice with a claim about `db.WithoutTenant` connections that
does not match this codebase's actual RLS policy shape (a `WithoutTenant`
connection sees only `tenant_id IS NULL` rows under migration 0041's real
policy text, never another tenant's data — the same policy shape this
document's own dual-scope tables already rely on elsewhere).

Why plurality rather than assuming one currency — the directive explicitly
forbids assuming, so this is argued rather than asserted:

1. **Multi-tenancy makes a single global points BALANCE incoherent.** A
   platform-global points balance is a cross-tenant shared object, which
   contradicts CLAUDE.md's rule that every tenant-owned table carries
   `tenant_id` under RLS, and would make tenant A's points economically
   meaningful against tenant B's redemption catalogue. Points issued by
   one operator are a liability of *that* operator. (This argues the
   BALANCE must be tenant-scoped; it does not argue the type DEFINITION
   must be — see the scope correction above.)
2. **Brand differences must be configuration, not code paths.** "Loyalty
   Points", a VIP-tier currency, and a seasonal tournament currency are
   three configuration rows under this model and three code branches
   under a single-currency model. CLAUDE.md forbids the latter.
3. **Plurality is nearly free now and expensive later.** It costs one
   registry table and one foreign key today. Retrofitting it means
   re-identifying every historical points row in an append-only store that
   by construction cannot be rewritten — the "can never retrofit" class of
   mistake ADR 0001 exists to avoid, applied to the same shape of problem.
4. **The precedent is proven in this codebase.** `Asset` already solves
   "many named units with their own precision, looked up never
   hardcoded". Reusing that shape means reviewers already know what
   correct looks like, rather than evaluating a novel mechanism.

Binding consequences:

- **Point types are never fungible with each other.** There is no implicit
  conversion between point types, and no operation that reads a balance in
  one type and writes another. If cross-type conversion is ever required
  it is an explicit, audited operation analogous to ADR 0021's
  `ConversionOperation`, and it needs its own ADR — it is not designed
  here.
- No code path assumes a single point type, a single exponent, or that
  "points" means the tenant's default type — the same standing review
  checkpoint ADR 0007 established for asset exponents.
- A `brand_id IS NULL` point type means "all brands of that tenant" and
  never "all tenants", the same rule ADR 0019 applies to
  `provider_capabilities`.

## 3. Numeric representation

`ARCHITECTURAL DECISION`. Points amounts are integers in minor units with a
per-`PointType` `exponent`, stored as `NUMERIC(38,0)` — identical
treatment to money, for identical reasons.

- **Floating point is never used for a points amount, a points balance, or
  a points accrual rate.** Accrual rates ("0.1 points per €1 wagered"),
  multipliers and tier bonuses are `NUMERIC`, never `FLOAT`/`DOUBLE`/
  `REAL`, exactly as ADR 0021 requires for `exchange_rate`. This is a
  blocking review finding, not a style preference.
- `exponent` defaults to `0` (whole points) but is configurable precisely
  so fractional accrual schemes never force either a float or an
  undocumented rounding hack. The exponent is **always looked up from the
  registry, never hardcoded** — including not hardcoding `0`.
- A points amount must be exactly recomputable from its stored inputs
  (base amount, stored rate, stored cap, stored rounding rule). Rounding
  uses one shared helper with a declared rule, not per-rule arithmetic.

## 4. Conceptual model

A parallel, deliberately recognizable shape. Names are conceptual; the
owning migration stage fixes the final ones.

```
PointType                    -- §2
PointAccount                 -- (tenant_id, point_type_code, account_type,
                                player_account_id nullable for house-level)
PointTransaction             -- one atomic, idempotent, balanced points fact
PointEntry                   -- one leg: debit|credit, amount > 0, never a signed column
PointBalanceProjection       -- one row per PointAccount (§6)
```

Account types (`ARCHITECTURAL DECISION` — the Blueprint says nothing about
points, so these are decided here and labeled as such):

| Account type | Scope | Normal balance | Purpose |
|---|---|---|---|
| `player_points` | player + tenant + point type | Credit | The player's spendable points balance |
| `points_issued` | tenant + point type (house-level) | Debit | Counter-side of every earn |
| `points_redeemed` | tenant + point type (house-level) | Credit | Counter-side of every spend |
| `points_expired` | tenant + point type (house-level) | Credit | Counter-side of every expiry |
| `points_adjustment` | tenant + point type (house-level) | — | Counter-side of staff adjustments |

Double entry is retained rather than "it's just a counter" because it buys
the same thing it buys for money at essentially zero cost: *points issued*,
*points redeemed*, *points expired* and *points outstanding* become four
account balances rather than four ad-hoc aggregation queries over
transaction types, and the whole store self-checks.

As with the money ledger, `signed_balance` is defined **credit-positive for
every account type without exception**; the "normal balance" column is an
expectation for alerting and a presentation-layer negation instruction,
never a per-account sign convention applied at read time.

### 4.1 Operations

| Operation | Entries | Notes |
|---|---|---|
| **Earn** `X` | Dr `points_issued` · Cr `player_points` | Triggered by a gamification rule, mission, wager, deposit, tier event |
| **Spend** `X` | Dr `player_points` · Cr `points_redeemed` | Marketplace purchase, reward redemption, tournament buy-in |
| **Refund** `X` | Dr `points_redeemed` · Cr `player_points` | A *new economic fact* (order cancelled, item undeliverable) — not a reversal |
| **Expiry** `X` | Dr `player_points` · Cr `points_expired` | A posted transaction on the expiry date — see §4.3 |
| **Manual adjustment** `X` | `points_adjustment` ↔ `player_points` | Mandatory reason code, four-eyes above threshold — §9 |
| **Reversal** | New transaction, `reverses_transaction_id` → the specific original, exact inverse entries | "This should never have been posted" — §4.2 |

Invariant, trivially true under double entry and therefore worth asserting
as a reconciliation check rather than assuming:

> **Invariant P1.** For every `(tenant_id, point_type_code)`:
> `Σ signed(all point accounts) == 0`, and
> `outstanding points == Σ signed(player_points)`.

> **Invariant P2.** `signed(player_points)` is **never negative**, for any
> player, at any instant. Points cannot be overdrawn.

P2 is enforced by reading the balance `FOR UPDATE` inside the spending
transaction (§8). An over-debit — including a clawback larger than the
remaining balance — is **rejected by the ledger**, never clamped and never
allowed to go negative. What to do about a rejected clawback (clamp to
available, defer, escalate to review) is a Gamification business decision;
recording an impossible balance is not an option available to it.

### 4.2 Reversal vs. refund — the same distinction the money ledger makes

- **Refund** = the spend was valid, the thing bought did not happen. A new
  transaction, no `reverses_transaction_id`, its own idempotency key.
- **Reversal** = the transaction should never have been posted (duplicate
  rule fire, operator error, abuse clawback of a fraudulently earned
  award). A new transaction with `reverses_transaction_id` pointing at the
  **specific** original, carrying its exact inverse entries. Never an edit,
  never a delete.
- The original is selected `FOR UPDATE` and rejected if something already
  reverses it — the same concurrent-double-reversal defect
  `internal/casino`'s rollback path had to fix empirically.
- A reversal naming an original the points ledger never saw writes a
  **tombstone** occupying the same idempotency slot, so a late-arriving
  original is rejected rather than applied after its own cancellation.
  Identical mechanism to `postRollbackTombstone`.

### 4.3 Expiry is posted, not filtered

Expiry is a transaction dated on the expiry date, **not** a `WHERE
expires_at > now()` filter applied at read time. A filtered expiry is not
auditable, cannot be reproduced as-of a past date, and is unanswerable
when a player asks why their balance dropped — which they will.

If Gamification's expiry policy is lot-based (FIFO over earning batches)
rather than whole-balance, the attribution is Gamification's rule; the
points ledger records the resulting amount and carries the source/lot
reference on the entry so the two stories match. **Remaining-balance-per-lot
is derived by reading the append-only trail, never stored as a mutable
`remaining` column on a lot row** — that column would be a balance being
`UPDATE`d, which is the one thing this whole architecture exists to
prevent.

## 5. Append-only enforcement

`ARCHITECTURAL DECISION`, inherited mechanism — ADR 0013/0019 arrived at
this pair the hard way and it is not re-derived here.

`PointTransaction` and `PointEntry` get **no `UPDATE` and no `DELETE`
policy at all** under `FORCE ROW LEVEL SECURITY` (absence of a permissive
policy is a deny), **plus** a `BEFORE UPDATE OR DELETE` row-level trigger
and a `BEFORE TRUNCATE` statement-level trigger that unconditionally
raise. `REVOKE`-based protection is explicitly **not** sufficient: the
application's runtime role owns these tables and a table owner can
re-`GRANT` itself any privilege, and row-level triggers never fire on
`TRUNCATE`. Both triggers are required, and both must be tested **as the
application's own runtime role**, not as a lesser one.

## 6. Balance projection

`ARCHITECTURAL DECISION`, mirroring ADR 0019 deliberately and exactly.

- `PointBalanceProjection` is **one row per `PointAccount`** (the same
  grain choice ADR 0019 made per `ledger_account_id`, for the same reason:
  house-level accounts need a projection row to reconcile against and a
  lockable row to serialize on).
- It is maintained **in the same database transaction as the `PointEntry`
  insert that changes it** — never an async job, never a trigger with
  eventual consistency, never a cache. This is what makes it subordinate
  rather than a second source of truth.
- **No cache is ever authoritative for a points balance, and no cache is
  read on the spend path.** The authoritative read happens inside the same
  database transaction as the write. Same rule as money, same reason.
- It is **fully rebuildable from `PointEntry` alone** at any time, and that
  rebuild must be an exercised runbook, not a theoretical property.
- **The ADR 0016 `UPDATE`-visibility gotcha applies here identically.** The
  projection's `SELECT` policies must be multiple *permissive* policies
  OR'd together — a tenant-scope policy covering system/service paths AND
  a player-scope policy for player-facing reads — never a single policy
  that ANDs the player scope in. A gamification rule firing from a
  background job has no player session and therefore no
  `app.player_account_id`; if the player scope were ANDed, the projection
  `UPDATE` would affect zero rows, the entries would commit, and the
  balance would silently drift. The writing code must additionally check
  `RowsAffected()` and fail the whole transaction loudly on zero.

### 6.1 Reconciliation

A scheduled job recomputes the true `SUM` from `PointEntry` and diffs it
against the projection, per `(tenant_id, point_type_code, account_type,
player_account_id)`.

- **Cadence: hourly**, matching the money ledger, because the machinery and
  the failure mode are the same.
- **Tolerance: zero.** There is no rounding source between an integer entry
  sum and an integer projection; any non-zero difference is a bug.
- **Severity:** `RECOMMENDATION` — **P1 for any point type that is
  redeemable for anything of monetary value** (which, under §7's model, is
  every redeemable type, since a redemption issues a bonus grant), and
  **P2 for a purely cosmetic point type** if one ever exists. The
  correction is always "rebuild the projection from the entries, never the
  reverse", exactly as `reconciliation-model.md` §2.1 states for money.
- Drift is investigated per key and the results recorded in the same
  `ReconciliationRun`/`ReconciliationMismatch` shape the financial streams
  use, so the on-call runbook is one runbook.

## 7. Points → cash: recommended **not** allowed, and how redemption works instead

This is the decision that keeps §1's "points are not money" sentence true,
so it is stated plainly.

`RECOMMENDATION`, **needs Master Orchestrator / human sign-off to be
treated as settled**: the platform does **not** support converting points
directly into `player_cash`, and the `PointType` registry carries no
"convertible to asset" capability.

Reasons:

1. **A points balance convertible to cash is money in substance.** It would
   immediately and correctly attract the entire money regime — AML
   source-of-funds, withdrawal controls, RG limits, segregation, tax
   treatment — and at that point there is no reason for a separate points
   ledger to exist at all. Half-adopting the regime is the dangerous
   outcome.
2. **It is an unbacked issuance power.** Gamification rules would be able
   to mint withdrawable value without a deposit, a provider settlement, or
   any reserve behind it.
3. **It is a legal question, not an engineering one.** In several target
   jurisdictions a redeemable-for-cash loyalty balance is a payment
   instrument or e-money, with licensing consequences. CLAUDE.md puts
   legal interpretation squarely in "stop and ask".

**How a player gets value from points instead.** A redemption is two
transactions in two ledgers, not one cross-system transfer:

1. a **points spend** in this ledger (Dr `player_points` / Cr
   `points_redeemed`), and
2. the **reward it buys** — most commonly a bonus grant posted through
   `internal/ledger` under ADR 0032 §3, or a free-spin/free-bet award, or a
   non-monetary item that touches no ledger at all,

linked by a shared `correlation_id`. The points balance is never the source
of the money; the *decision to grant* is, and that grant is an ordinary
operator-funded promotion with its own `promo_liability` mirror. This is
what lets a redeemable points programme exist without points themselves
being money.

**Atomicity.** Both ledgers live in the same PostgreSQL database, so a
redemption whose reward is a ledger posting executes in **one database
transaction** — points spend and bonus grant commit together or not at
all. No saga, no distributed transaction, no compensation path is needed
or permitted for that case. A saga would only become necessary if the two
stores were ever separated, and separating them requires its own ADR
precisely because it would turn a free atomicity guarantee into a
correctness problem.

**Where the reward requires an external call** (free spins at a game
provider), atomicity is impossible, so: the points spend and a *pending
fulfilment* record are written atomically in one transaction; the provider
call is a separate, idempotent, retryable step; and terminal failure
triggers a **compensating points transaction** (a refund per §4.1),
never an edit and never a silently abandoned spend. The pending fulfilment
record is the thing that makes an abandoned spend impossible to miss.

If the platform ever does want points→cash, it is a **new ADR** with
`ledger-finance`, `risk`, `identity-compliance` and legal sign-off, and it
is not enabled by a configuration flag added to the registry later.

## 8. Idempotency and concurrency

`ARCHITECTURAL DECISION`, inherited from ADR 0020 rather than reinvented.
Every mechanism below is the same one the money ledger uses.

- **Internally-originated** points movements (a gamification rule firing, a
  mission completion, a marketplace order, a tier award): `UNIQUE
  (tenant_id, idempotency_key)`, where the key is the originating system's
  own **`idempotency_key`, generated once when the operation is first
  accepted and reused verbatim on every retry — never regenerated per
  attempt.** **Specialist-review correction**: an earlier draft of this
  bullet called the key the originating system's own "event id" — this
  must NOT be `docs/architecture/22-canonical-activity-event-taxonomy.md`'s
  `event_id` field, which is explicitly unique per PUBLISH (and can differ
  across redeliveries under at-least-once broker semantics); it must be
  that same taxonomy's separate `idempotency_key` field, unique per
  BUSINESS FACT, which is the field actually designed to be stable across
  redelivery. A Reward Orchestrator or gamification worker that mints a
  fresh key per delivery attempt — or that reuses the wrong one of these
  two taxonomy fields — has defeated the mechanism entirely; this is the
  most likely practical failure of this design and is called out rather
  than assumed.
- **Externally-originated** points movements (an External Reward Provider
  awarding points): `UNIQUE (tenant_id, provider_id, provider_tx_id)`, with
  `provider_id` resolved from the **credential that verified the callback
  signature**, never from the payload — ADR 0019's actor-matrix rule,
  applied unchanged.
- Both keys are **tenant-scoped**, for the reasons recorded once in
  `ledger-accounting-model.md` §3 (cross-tenant collision, and the RLS
  unique-violation oracle).
- **Exact retry** returns the original transaction's result via the same
  `SAVEPOINT` pattern `db.IdempotentInsert` implements — a naive
  catch-the-error-then-query on an aborted pgx transaction fails with
  `current transaction is aborted` on *every* duplicate, and only surfaces
  under concurrency testing.
- **Same key, different payload** is **rejected**, never silently applied
  and never silently treated as a match.
- **Concurrent duplicates** are arbitrated by the database constraint.
  There is no check-then-insert anywhere in this design.
- **Concurrency:** one database transaction per points state change;
  `SELECT ... FOR UPDATE` on the `PointBalanceProjection` row inside any
  transaction that debits `player_points`, so two concurrent spends cannot
  both observe a sufficient balance (invariant P2). Any operation touching
  more than one player's accounts uses `SERIALIZABLE` rather than
  hand-ordered locks. **A Redis lock is never the sole correctness
  mechanism** — correctness must hold with any such optimization entirely
  absent, and that must be tested with it disabled.

## 9. Tenancy, isolation and authorization

`ARCHITECTURAL DECISION`, the same dual-scope pattern as everything else in
this codebase — stated concretely because ADR 0019 found tables whose prose
claimed tenant-scoped RLS while carrying no `tenant_id` column.

- **`point_types` is dual-scope** (nullable `tenant_id`, mirroring
  `risk_rules`'s exact policy shape — see the scope correction in §2):
  `tenant_id IS NULL` rows are platform-wide definitions readable by every
  tenant's staff and only writable via a platform-scoped connection;
  `tenant_id`-set rows are readable and writable only by that tenant's
  staff. **`point_accounts`, `point_transactions`, `point_entries`,
  `point_balance_projection`, and any points reconciliation tables carry
  `tenant_id NOT NULL`** — the liability argument in §2 applies fully to
  every balance-bearing table, just not to the type-definition registry.
  All of the above carry `FORCE ROW LEVEL SECURITY` and a policy gated on
  the connection-level `app.tenant_id` setting.
- **`tenant_id` is never accepted from the client** on any write path —
  always derived server-side from authenticated context.
- Player-scoped rows carry **`player_account_id` denormalized on every row
  the policy protects**, never reachable only by joining another
  RLS-protected table. This is ADR 0019's explicit lesson: a policy written
  as a subquery into another protected table has deny behaviour that
  depends on that table's policy set. `brand_id` is carried where a point
  type is brand-scoped, under the same "NULL means all brands of that
  tenant, never all tenants" rule.
- The player scope GUC is `app.player_account_id`, set only by trusted
  server code from the authenticated player's own resolved account — never
  from a path, query or body parameter.
- House-level points accounts have **no** player scope and are readable
  only by staff/service principals with the appropriate RBAC permission.
- **No BALANCE-bearing points table is platform-scoped** —
  `point_accounts`/`point_transactions`/`point_entries`/the projection and
  any reconciliation table are always `tenant_id NOT NULL`, for the same
  reason ADR 0019 excluded the reconciliation tables from dual-scope: a
  balance is never a legitimate cross-tenant object. **Specialist-review
  correction**: an earlier draft of this bullet additionally claimed a
  dual-scope policy "would make a `WithoutTenant` connection a legitimate
  cross-tenant read path" — that claim is inaccurate against this
  codebase's actual RLS policy shape (migration 0041's real policy text:
  `tenant_id IS NULL OR tenant_id = current_setting('app.tenant_id')`,
  under which a `WithoutTenant` connection sees ONLY the `tenant_id IS
  NULL` rows, never another tenant's) — the true reason balance tables
  stay single-scope is the liability argument alone, not an RLS-safety
  argument. `point_types` (the definition registry, §2) IS dual-scope,
  using exactly this same, correctly-understood policy shape. Cross-tenant
  points *reporting* (aggregated, non-transactional) goes through the
  reporting layer (`12-audit-reporting-architecture.md`), not relaxed RLS
  on any balance table.
- Players never write to any points table directly; there is no
  player-scoped `INSERT`/`UPDATE`/`WITH CHECK` policy on any of them.

**Actor matrix** (extending ADR 0019's, same shape):

| Actor class | May originate |
|---|---|
| Player session | Nothing. A player action may *trigger* an earn or request a redemption, but the posting is made by a service identity. A player may never originate an earn, an adjustment, or any credit to `player_points`. |
| Internal service (Gamification, Reward Orchestrator, expiry job) | earn, spend, refund, expiry, reversal — under a service identity (ADR 0014) |
| Verified external reward provider callback | earn/spend types within that provider's declared capability, scoped to the tenant resolved from the authenticating credential, and referencing only awards the platform itself created for that same `provider_id` |
| Staff principal with explicit RBAC permission | `points_adjustment` only — mandatory reason code, four-eyes above a configurable threshold |

The four-eyes/reason-code requirement on manual points adjustment is
**deliberately the same as CLAUDE.md's manual balance adjustment rule**,
even though points are not money: a staff member who can mint redeemable
points can mint bonus grants by proxy (§7), so the abuse profile is the
money one.

## 10. Audit

Every points movement writes an `audit.Record` **in the same database
transaction** as the postings, exactly as `internal/casino` and
`internal/payments` do — actor, tenant, entity, outcome, reason code where
applicable, and the point type and amount in metadata.

Actions: `points.earned`, `points.spent`, `points.refunded`,
`points.expired`, `points.reversed`, `points.adjusted`,
`points.reversal_tombstoned`.

As with money, **the audit log is a human-readable trail, not a second
ledger.** Reconstructing a points balance from the audit store is not a
supported operation; reconciliation and rebuild always read `PointEntry`.

## 11. Boundary with Gamification

The one sentence this document exists to make unambiguous:

> **Gamification decides WHEN and HOW MUCH. This ledger defines HOW that
> decision is durably, safely and auditably recorded — and is the only
> place a points balance exists.**

Gamification owns: rules, triggers, missions, tiers, multipliers, caps,
eligibility, anti-abuse, expiry policy, the redemption catalogue, and
every question of whether a player *should* receive points. None of that
is `ledger-finance`'s business and nothing here constrains it.

This ledger owns, and Gamification may not do: holding an authoritative
points balance in its own tables; `UPDATE`ing a balance; computing a
balance from a cache; posting without an idempotency key; editing or
deleting history; correcting anything by any means other than a
compensating transaction. Any points figure a gamification table carries
is a denormalized read that must be reconcilable to this ledger — the same
subordinate relationship `wallet_balance_projection` has to
`ledger_entries`.

Relationship to the other Stage 4H-A documents: the Reward Orchestrator
routes a reward to *either* this ledger (points), *or* `internal/ledger`
(money, per ADR 0032), *or* neither (purely cosmetic rewards) — never to
two of them for the same value, and never to its own store. Risk and RG
controls consume points/reward events; they do not post them.

## 12. What must be decided before implementation

1. **Points→cash convertibility (§7)** — `RECOMMENDATION` is "not
   supported". Needs Master Orchestrator / human sign-off, with
   `identity-compliance` and `risk` input, before it is settled.
2. **Reconciliation severity (§6.1)** — P1 for redeemable point types is a
   `RECOMMENDATION`; confirm with the Master Orchestrator, since it sets an
   on-call paging expectation.
3. **Player-to-player point transfer** — `OPEN DECISION`, assumed **not**
   supported (`transferable` defaults false). Enabling it has abuse,
   AML-adjacent and jurisdictional implications and needs its own decision.
4. **Cross-point-type conversion (§2)** — `OPEN DECISION`, out of scope,
   needs its own ADR if ever required.
5. **Whether points balances are in scope for RG/self-exclusion
   enforcement** (e.g. can an excluded player still redeem points into a
   bonus?) — `OPEN DECISION` owned by `identity-compliance`, not resolved
   here. `ledger-finance`'s only position: whatever is decided must be
   enforced at the redemption posting boundary, server-side, not in the UI.

## Cross-references

`CLAUDE.md`; ADR 0007, 0013, 0014, 0016, 0019, 0020, 0021, 0032;
`docs/architecture/06-wallet-ledger-architecture.md`,
`ledger-accounting-model.md`, `reconciliation-model.md`,
`12-audit-reporting-architecture.md`, `10-bonus-engine-architecture.md`,
and the Stage 4H-A Gamification architecture and Reward Orchestrator
documents; `internal/ledger`, `internal/audit`, `internal/reconciliation`.

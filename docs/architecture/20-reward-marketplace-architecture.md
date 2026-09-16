# 20 — Reward Marketplace Architecture (Future)

Status: **`NOT IMPLEMENTED` — architecture freeze only (Stage 4H-A), and
the furthest-out of the four gamification documents.** No package, no
schema, no migration, no API. Owned by `architect` as a cross-domain
boundary document. This exists so the Gamification and Reward
Orchestration designs do not foreclose a marketplace, **not** because a
marketplace is scheduled. Nothing here authorizes implementation, and a
future stage that builds it should expect to revisit these choices with the
benefit of a real product specification.

## 1. What it is, and the hard boundary

A **Reward Marketplace** (sometimes "loyalty shop" / "reward store") is a
catalog of items a player can obtain by spending a balance — normally
gamification points — outside of any competition or mission.

> **The marketplace never directly manipulates a monetary balance or a
> points balance. It requests fulfilment through the Reward Orchestrator,
> exactly as the Bonus Engine and the Tournament domain do.**

It is a **storefront and an order state machine**. It owns the catalog,
the purchase intent, the order lifecycle, and the audit trail of what was
bought. It owns nothing that moves value:

| Concern | Owner |
|---|---|
| Debiting the points balance for a purchase | Points accounting (`ledger-finance`, doc 24), requested via the Reward Orchestrator |
| Debiting a monetary balance for a purchase | `internal/ledger`, requested via the Reward Orchestrator |
| Fulfilling a bonus / free spins / free bet item | Bonus Engine, routed by the Reward Orchestrator |
| Fulfilling a points-multiplier or tournament-entry item | Gamification, routed by the Reward Orchestrator |
| Fulfilling a physical or third-party-voucher item | An external fulfilment provider behind a provider interface, routed by the Reward Orchestrator |
| Refunds and reversals | The Reward Orchestrator's reversal path |
| The marketplace's own catalog, eligibility, limits wiring, and order state | **This domain** |

The marketplace domain must not import `internal/ledger`,
`internal/wallet`, or the Bonus Engine, and must have no code path capable
of crediting or debiting anything.

## 2. Conceptual model

```
MarketplaceCatalog        scope: platform-template | tenant | brand
  code, display_name, status, jurisdiction_scope[], licensing_mode_scope

MarketplaceItem           belongs to a catalog
  code, display_name, description, media_ref
  item_type                    (§4 — the reward KIND, not its mechanics)
  price_specification          (§3)
  inventory_policy             (§5)
  availability_window (start, end, timezone_basis)
  eligibility_ref, segment_ref
  purchase_limit_refs[]        (references to RISK rules — §6)
  fulfilment_specification     (a reward DECISION template, resolved by
                                the Reward Orchestrator — never mechanics)
  version, effective_from, effective_to, audit metadata

MarketplaceOrder          one player's purchase
  item_id, item_version (pinned), player_account_id, tenant_id, brand_id
  price_paid_specification (pinned at order creation — §3)
  state: created | reserved | charged | fulfilling | fulfilled
       | failed | refunding | refunded | cancelled | expired
  debit_request_ref, reward_decision_ref
  eligibility_decision_ref, risk_decision_ref
  idempotency_key
  created_at, state_history (append-only)
```

Item **versions are pinned on the order**. A price or specification change
must never alter what an already-placed order was for — the same
definition/instance discipline tournaments and missions use.

## 3. Price — be explicit

**Decision: an item's price is denominated in exactly one currency, named
explicitly, and that currency is normally a gamification point type.**

```
price_specification
  currency_kind: point_type | monetary_asset
  currency_code:  a PointType.code  OR  an assets.code
  amount:         integer minor units under that currency's own exponent
```

Non-negotiable properties:

- **Never floating point.** Integer minor units with the exponent looked up
  from the owning registry (the `Asset` registry for a monetary asset, the
  point-type registry for points), never assumed. This is CLAUDE.md's rule
  for money, and it applies to points for the same reason: rounding errors
  in a redemption price are indistinguishable from theft to the player who
  notices.
- **No mixed-currency price and no implicit conversion.** An item priced in
  points is bought with points. "Pay 500 points + €2" is a compound price
  and is **not** supported in this design; it would require an atomic
  two-domain debit with its own partial-failure semantics, and it is not
  worth that complexity absent a real requirement. If it is ever needed, it
  needs its own ADR.
- **A point type's scope constrains the item's scope.** An item priced in a
  tenant-scoped point type can only appear in that tenant's catalog. This
  falls out of doc 17 §3.2's scoping and must be enforced, not assumed.
- **`RECOMMENDATION`: the first implementation should support
  `point_type` pricing only.** A monetary price turns the marketplace into
  a payment surface, with PSP, AML, refund, chargeback, and tax
  implications far beyond a loyalty shop. If real-money purchase is ever
  required, it goes through the existing payments domain, not through a new
  path here.
- Price may be **tiered by player level or segment** (a VIP price), as
  additional price rows with a scope, not as logic. The price actually
  applied is pinned on the order.

## 4. Candidate item types

Listed to show the model is wide enough. **The fulfilment mechanics of each
are explicitly out of scope** — they belong to the Reward Orchestrator and
the domain it routes to (Bonus Engine, Gamification, Casino, an external
fulfilment provider). This document defines only that an item names a type
and its parameters.

| Item type | Fulfilled by | Notes |
|---|---|---|
| Bonus (funds / deposit match) | Bonus Engine | Carries wagering obligations the Bonus Engine owns |
| Free spins / free rounds | Bonus Engine → Casino | Per-provider normalization is Casino's, doc 10 |
| Free bet | Bonus Engine → Sportsbook | No sportsbook domain exists yet |
| Cashback | Bonus Engine | Window and calculation are Bonus's |
| Tournament entry | Gamification (doc 18 §4) | An entry, not a prize |
| Points multiplier | Gamification | A time-bounded modifier on points accrual rules |
| Mini-game attempt | *(placeholder)* | Blocked on doc 17 §11's unresolved legal question |
| Virtual reward (avatar, badge, cosmetic) | Gamification | No monetary value; the cheapest type to ship first |
| Physical reward (merchandise) | External fulfilment provider | Introduces shipping addresses, PII, logistics, tax, and returns. See §9 |
| Third-party voucher / gift card | External fulfilment provider | Introduces a cash-equivalent instrument with real AML exposure |

**`RECOMMENDATION`: if a marketplace is ever built, start with virtual
rewards and Bonus-Engine-fulfilled items only.** Physical rewards and
cash-equivalent vouchers each bring an entire compliance and operations
surface with them and should be separate, separately-authorized decisions.

## 5. Inventory

Inventory applies to some item types and not others; the model must say
which, explicitly, rather than treating unlimited as the absence of a
concept.

| Policy | Meaning | Concurrency requirement |
|---|---|---|
| `unlimited` | No stock constraint (typical for bonus/virtual items) | None |
| `limited_total` | N units ever | Atomic decrement; overselling is a defect |
| `limited_per_period` | N units per day/week/period | Atomic decrement within the period |
| `unique` | A single specific item (e.g. one named prize) | At most one order can ever succeed |

Where inventory is constrained, the decrement must be **atomic and
concurrency-safe at the database level** — a conditional update checked by
rows-affected, never a read-then-write, mirroring the single-use launch
token pattern `08-casino-integration-architecture.md` §5 already
establishes. Two concurrent purchases of the last unit must produce exactly
one success and one clean, distinguishable failure.

Reservation: for a limited item, stock is **reserved at order creation and
released on failure or expiry** — not decremented only at fulfilment
(which oversells) and not decremented optimistically with no release path
(which leaks stock). Reservations have a TTL.

## 6. Purchase limits — Risk owns them, not the marketplace

**Hard rule, identical to doc 17 §7.1: the marketplace has no limit engine,
no counter, no cap, and no velocity concept of its own.** Every purchase
limit is a rule in `internal/risk`, evaluated by `risk.Evaluate` with a
`marketplace_purchase` operation in the same transaction as the order's own
state change, before it commits, fail-closed on any error (ADR 0031 §6).

Limits a real marketplace will want, and what they require:

| Limit | Risk requirement |
|---|---|
| Max purchases of item X per player per period | A `count` limit kind — **does not exist today** (ADR 0031 §4) |
| Max total redemption value per player per period | `cumulative_amount`, which exists |
| Max purchases per period across the catalog | A `count` limit kind — does not exist today |
| Per-brand or per-jurisdiction redemption ceiling | Existing scope dimensions |

Two of those need a `LimitKind` ADR 0031 deliberately did not implement.
The correct response is ADR 0031 §12's five-step extension process, adding
`count` to `internal/risk` — **not** a counter inside the marketplace.
Until that exists, the corresponding limits do not exist and must be
disclosed as not existing, per CLAUDE.md's no-fake-completion rule.

`purchase_limit_refs[]` on an item is therefore a reference to risk rules,
not an embedded limit definition.

## 7. Eligibility and RG

Evaluated at purchase time, in this order, non-negotiably (ADR 0031 §1):

1. **RG** — `rg.EvaluateEligibility`. Denial short-circuits before Risk.
   **A self-excluded player cannot purchase or redeem anything in the
   marketplace**, including a purely virtual item. No second self-exclusion
   concept exists here.
2. **Risk** — `risk.Evaluate` (§6).
3. **Item-specific conditions** — segment membership, level/VIP tier,
   availability window open, jurisdiction/market permitted, brand
   membership, KYC-verification state where the item type requires it
   (a cash-equivalent voucher plausibly does; a virtual avatar does not),
   inventory available, sufficient balance.

The **balance-sufficiency check is not the marketplace's decision to
enforce**. The marketplace may display affordability, but the authoritative
check happens inside the debit transaction performed by the balance-owning
domain — exactly as the ledger's "authoritative balance read happens inside
the same database transaction as the write" rule requires for money, and
for the same reason. A marketplace that pre-checks a balance and then
requests a debit has a TOCTOU window; the debit itself must be the
gatekeeper.

## 8. Order lifecycle, failure, and refund

```
created --> reserved --> charged --> fulfilling --> fulfilled
   |           |            |            |
   |           |            |            +--> failed --> refunding --> refunded
   |           |            +--> failed --> refunding --> refunded
   |           +--> expired (reservation TTL, stock released)
   +--> cancelled (eligibility/risk denial — nothing reserved or charged)
```

Decisions:

- **Charge before fulfil, refund on failure.** The alternative (fulfil
  first, charge after) can deliver an item without payment; this order can
  charge without delivering, which is recoverable through a refund. Between
  two imperfect orderings, choose the recoverable one — and make the refund
  path a first-class, tested state, not an exception handler.
- **Every order is idempotent** on a client-supplied-then-server-validated
  idempotency key, enforced by a database uniqueness constraint. A
  double-submitted purchase produces one order, never two.
- **A refund is a reversal request through the Reward Orchestrator**, never
  a direct credit from the marketplace. It carries the original order's
  identity, and its own idempotency key, so a retried refund cannot
  double-credit.
- **Some fulfilments are economically irreversible** — a bonus already
  wagered, free spins already played, a voucher already redeemed by the
  player. A failure discovered after such a fulfilment cannot be undone by
  a refund and must surface as an explicit operational outcome requiring a
  human decision, never a silent success and never an automatic
  compensating credit. This is the same honesty tournaments' recalculation
  path requires (doc 18 §9).
- **A stuck order is an operational alert, not a hidden state.** Orders in
  `fulfilling` or `refunding` past a threshold must be visible in back
  office with the underlying reward-decision status.
- **The state history is append-only.** An order's state is never
  overwritten without a trail; "what happened to my order" must be
  answerable from the record.

## 9. What physical and voucher rewards drag in (recorded, not designed)

Flagged here so a future stage does not discover it late:

- **Shipping addresses are PII** subject to `16-privacy.md`'s rules, with
  their own retention, access-control, and erasure obligations. The
  platform does not collect shipping addresses today.
- **Physical fulfilment is a provider integration**, and per CLAUDE.md's
  provider rule it is a subsystem, not a connector: the platform owns the
  adapter, idempotency/retry, per-tenant credentials, the state machine,
  and reconciliation against orders.
- **Cash-equivalent vouchers have real AML exposure.** A points-to-voucher
  path is a value-transfer path. Whether it is permissible, and under which
  licences, is a legal question (§11).
- **Tax and consumer-law obligations** (delivery timelines, returns,
  reporting) vary by jurisdiction and are not engineering decisions.

None of this is designed here. All of it must be resolved before a physical
or voucher item type is enabled.

## 10. Multi-tenancy, scoping, and audit

Per doc 17 §9, without exception:

- **Catalogs and items** are dual-scope: `tenant_id NULL` is a
  platform-wide **template**; set means tenant-owned, optionally narrowed
  by `brand_id`. Tenant staff read both, write only their own. Staff-scope
  RLS policies carry the `app.player_account_id IS NULL` guard.
- **Orders** are `tenant_id NOT NULL`, RLS-enforced, with a SELECT-only
  `player_self_scope` policy so a player reads their own orders and no
  one else's.
- **A player's points from tenant A are never spendable in tenant B's
  catalog** — this falls out of the point type's own scope (doc 17 §3.2)
  and must be enforced at the data level, not by application discipline.
- Jurisdiction and licensing-mode scoping use ADR 0006 / ADR 0031 §9–§10's
  existing mechanism. An item unavailable in a market is a configuration
  row, never a code path.
- **Audit** (existing immutable `audit_log`): catalog/item
  created/versioned/price-changed/disabled, inventory adjusted (with reason
  code), order created/charged/fulfilled/failed/refunded/cancelled,
  eligibility and risk denials with their decision codes, and every
  administrative override or manual refund. A manual refund requires a
  reason code and, above a configurable threshold, four-eyes approval —
  CLAUDE.md's manual-adjustment rule applies to anything that hands a
  player value, not only to a wallet.

## 11. Open decisions

1. **Is a reward marketplace wanted at all, and when?** Nothing has
   scheduled it. This document exists to keep the option open.
2. **Are cash-equivalent vouchers or points-to-cash paths permissible**
   under the platform's licences and target jurisdictions? Legal.
   Determines whether points are a financial liability (doc 17 §14.3).
3. **Are physical rewards in scope?** (§9) They bring PII, logistics, tax,
   and a provider integration.
4. **Is a compound price (points + money) ever required?** (§3) Explicitly
   unsupported here; would need its own ADR.
5. **Does `internal/risk` get a `count` limit kind** so per-item purchase
   limits can exist at all? (§6) Required before limits are real.
6. **What is the player-visible policy when a fulfilment is irreversible
   and the order failed downstream?** (§8) Product plus support.

## 12. What is explicitly not designed here

Fulfilment mechanics for any item type; the points accounting model
(doc 24); the Reward Orchestrator's own routing, retry, and reversal
design; the player-facing storefront API and UI; pricing/merchandising
analytics; promotional discounting; gifting between players (which would be
a value-transfer path with its own AML questions); and any external
fulfilment provider integration.

## Cross-references

- Gamification Engine: `docs/architecture/17-gamification-engine-architecture.md`
- Tournaments: `docs/architecture/18-tournament-architecture.md`
- Missions: `docs/architecture/19-mission-architecture.md`
- Bonus Engine: `docs/architecture/10-bonus-engine-architecture.md`
- Risk & Limits: `docs/decisions/0031-risk-and-limits-engine.md`
- Privacy/PII obligations: `docs/architecture/16-privacy.md`
- Provider abstraction: `docs/decisions/0004-provider-abstraction-pattern.md`
- Domain boundaries: `docs/architecture/02-domain-and-service-boundaries.md`

# 34 — `EconomicOperationIdentity` — the canonical authorization boundary for value-creating operations

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** No Go code, no
schema, no migration, no route is authorized by this document. Produced
in Stage 4H-B1, Wave 1.5 **Fix Wave** (task `4HB1FW-03`), against the
Orchestrator's unifying technical contract for that wave, which names
this concept and constrains its shape:

> "`EconomicOperationIdentity`: **extend existing operation/transaction-
> identity mechanisms** (`correlation_id`, idempotency keys, campaign/
> offer/grant versioning) to carry parent-operation/batch-lineage/
> intended-aggregate-value, **rather than inventing a new domain**. Owned
> by `architect`, since it is the entity that must be consistently
> referenced by Bonus, CRM, and Affiliate alike."

Numbering: `docs/architecture/` was populated through `33`. This document
takes the next free number, **34**.

## 0. What this is, in one paragraph — and what it is not

An `EconomicOperationIdentity` (**EOI**) is a row that names **the real
logical economic operation a human or a policy actually authorized**, so
that every technical execution attempt caused by that authorization —
the first call, a retry, a resumed job, page 7 of 40, a re-sent batch
after a crash, a per-player item inside a bulk grant — resolves to the
**same** authorization boundary, and never mints a new one.

It is **not** a new domain, not a new service, not a workflow engine, not
a saga coordinator, not an event bus, not a second audit log, and not a
second idempotency mechanism. It is one shared table plus a small set of
rules about who mints and who inherits. Everything it carries either
already exists somewhere in this platform under a different name, or is
the minimal addition needed to make the existing things compose.

---

## 1. Why this exists — the concrete defect it closes

### 1.1 SEC-W15-02, restated as a general shape

`security`'s SEC-W15-02 (P0, Wave 1.5) found this in the CRM→Bonus
interface as originally designed:

> A CRM journey with an `offer_request` step running against a
> 100,000-member audience issues 100,000 individual grant calls, **none
> of which is a `BulkGrantJob`** — and therefore none of which touches
> the `bonus_bulk:execute` always-four-eyes control, which exists
> precisely because a bulk grant's blast radius is *recipients × value*,
> not value.

The specific fix is in doc 31 §7.2.3 (the control attaches to
`EngagementCampaign` activation, enforced in Bonus). But the *shape* of
the defect is general, and it recurs anywhere a controlled operation can
be executed as N smaller ones:

| The shape | Concrete instances in this platform |
|---|---|
| **Decomposition** — one authorized operation executed as N individually-sub-threshold operations, each of which passes every per-operation control | CRM journey → N grants (SEC-W15-02). A bulk grant "split for performance" into N jobs. A commission period settled as N per-affiliate instructions. A manual adjustment split below a four-eyes threshold |
| **Re-minting on retry** — a retry that mints a fresh authorization context, so the retry is governed as a new operation nobody approved | A lost response causes a settlement retry with a new `instruction_id` (doc 32 LF-6's double-settlement vector). A resumed `BulkGrantJob` that re-creates itself instead of resuming |
| **Pagination laundering** — a batch that exceeds an approved ceiling is sent as pages that individually do not | A 100k audience delivered as 100 × 1k jobs, each under a 5k ceiling |
| **Orphaned execution** — a call arriving with no traceable authorization at all, accepted because nothing required one | Any `ActorService` caller of a grant surface, today |

Every one of these is a control that is present, correct, and bypassed —
not by an attacker defeating it, but by the system's own supported
execution patterns routing around it.

### 1.2 Why the existing mechanisms do not already close it

This platform has four identity mechanisms that are each individually
correct and none of which answers the question:

| Mechanism | What it answers | Why it is not enough |
|---|---|---|
| **`idempotency_key`** (doc 22; `(provider_id, provider_tx_id)`; `(tenant_id, idempotency_key)`) | "Have I already executed *this exact call*?" | Scoped to one call. Two calls that are *parts of the same authorized operation* have different keys, legitimately. It prevents double-execution, never decomposition |
| **`correlation_id`** (doc 22 line 48) | "Which events came from one logical request?" | It is a **tracing** identifier: caller/request-scoped, threaded for observability and dispute reconstruction. It carries no approval state, no ceiling, no value, and nothing enforces it. Nothing rejects a call for having a fresh one |
| **`operation_ref`** (doc 22) | "Which source-of-truth row is this event about?" | Points *down* at a row, not *up* at an authorization |
| **Version pins** (`campaign_version_id`, `offer_version_id`, `segment_version_id`, `commission_rule_version_id`) | "Under which frozen rules?" | Answer *what* was authorized, never *how much of it* or *how many times* |

The EOI is the missing fifth: **"under which single human/policy
authorization, with what remaining budget, is this execution happening?"**

It is deliberately built *out of* the four above rather than beside them
(§4).

---

## 2. The model

### 2.1 The one binding sentence

> **An authorization mints exactly one `EconomicOperationIdentity`. Every
> execution it causes — directly or transitively, first attempt or
> thousandth — inherits that identity as its `parent_operation_id`. No
> executor ever mints.**

Mint-once, inherit-always. Every property in this document is a
consequence of that sentence.

### 2.2 Fields

The field set the Fix Wave directive names, each with its source and its
purpose. **Bold** = load-bearing for the decomposition fix; the rest are
context the authorization needs to be reconstructable.

| Group | Field | Source / rule |
|---|---|---|
| Identity | `operation_id` | Server-generated UUID. The EOI's own identity |
| | `tenant_id` | **`NOT NULL`**, from authenticated server context only, never client-supplied (`CLAUDE.md`). RLS-scoped like every other tenant-owned table |
| | `brand_id` | Nullable (an operation may be tenant-wide) |
| **Type** | **`operation_type`** | A closed, compiled-in enum (§3.1). Not operator-extensible; a new value goes through the same additive extension discipline ADR 0031 §12 established |
| Actor | `initiating_actor_type` / `initiating_actor_id` | `internal/audit.ActorType` verbatim — `{player, staff, service, system}` (doc 10 N2.3: there is no `provider` value, and no domain invents a parallel enum) |
| | `initiating_principal_id` | The authenticated principal, where one exists. Distinct from the actor *type* so that `security`'s actor≠subject invariant has a field to test |
| **Subject** | **`subject_scope`** | `single_subject` \| `enumerated_set` \| `criteria_defined` \| `none`. This is what makes "a grant to one player" and "a grant to an audience" the same kind of object with different scopes |
| | `subject_ref` | For `single_subject`: the beneficiary's `player_account_id` / `affiliate_node_id`. Null otherwise |
| | **`subject_set_hash`** | A content hash pinning the authorized subject set. `security`'s `CRM-BR-1`/§W15.1.2 require the set to be **resolved, materialized and hashed at approval**, with the row count pinned and membership tested against the pin — never a re-resolution at execution. `bonus-engine`'s doc 10 **W5** requires **live** resolution at run time so a since-excluded player is never included from a stale list. **The reconciliation this document adopts** (routed as doc 31 DEP-CRM-7, unconfirmed by either owner): the pin is a **ceiling**, and live resolution may only **shrink** it — no subject outside the pin is ever reachable, and a subject inside it may still be dropped by the live per-item gate. Both hashes are therefore carried: `subject_set_hash` over the **materialized** set (the ceiling) and `subject_definition_hash` over the **definition** (the segment-reference pins, so a definition swap is detectable). Until both owners confirm, the conservative composition applies: pin ∩ live re-resolve |
| | `subject_set_count` | The pinned row count disclosed at approval (`CRM-BR-1`). Re-verified at execution; a deviation aborts (doc 31 §7.2.3 item 4) |
| | `beneficiary_class` | `player` \| `affiliate` \| `staff` \| `platform`. The EOI carries the *class*; the **resolved beneficiary set** `B(O)` that `security`'s `SEP-1` (`security-architecture.md` §W15.1) tests against is supplied by each adopting domain's own resolver, in the same transaction as the authorizing write, and is deliberately **not** stored here — §2.3 |
| **Economic owner** | **`economic_owner`** | *Who bears the cost.* `tenant` (operator-funded), `provider:<provider_id>` (provider-funded), `affiliate:<node_id>`, `platform`. This is not decoration: doc 10 §3's `fulfillment_owner` split and `ledger-finance`'s BF-1 lot-attribution finding both turn on it, and a ceiling is meaningless without knowing whose money it bounds |
| **Value** | `asset_code` | Asset Registry code (ADR 0037). **Null is legal and meaningful**: a non-monetary operation (a pure send campaign) has no asset |
| | **`intended_aggregate_value`** | Integer minor units, decimal-string wire form, `NUMERIC(38,0)`-compatible — **never** `int64`, never floating point (`CLAUDE.md`; ADR 0021; doc 21's exponent-18 correction). The *authorized* aggregate, by value, at approval time |
| | **`recipient_ceiling`** | An integer **count**. The maximum number of subjects this authorization may ever reach. Mandatory for `criteria_defined` scope (doc 31 §7.2.3 item 3) |
| | **`per_window_ceiling`** + `window` | For a continuously-running authorization (a live journey): the maximum executions per declared window |
| | `value_measure_basis` | How `intended_aggregate_value` was computed and its `as_of` — EDR-R1's "copy the mutable with its as-of" (doc 30 §7.1) applied to a control input |
| **Lineage** | **`parent_operation_id`** | Nullable **only** for a root operation. Non-null for every derived execution |
| | `root_operation_id` | Denormalized for query sanity; equals `operation_id` for a root. A closure/recursive query is correct but makes the enforcement path a recursive CTE, which is the wrong thing to put on a write path |
| | **`lineage_kind`** | `root` \| `retry` \| `resume` \| `page` \| `item` \| `compensation`. §3.2 |
| | `batch_ordinal` / `batch_total` | For `page`/`item`: position and declared total. Reuses ADR 0038 §14's `occurrence_ordinal` discipline for repeated same-type operations |
| **Approval** | **`approval_state`** | `not_required` \| `pending` \| `approved` \| `rejected` \| `expired` \| `consumed` \| `revoked`. **`not_required` is never a default** — it is a recorded determination with the policy row and threshold that produced it |
| | `required_approvals` / `approvals_received` | Counts, with the fail-closed default of `2` / threshold `0` when no policy row matches (`internal/withdrawal/policy.go`'s `defaultApprovalPolicy`, `policy.go:136`) |
| | `approval_refs[]` | The approval rows. **Referenced here, CONSUMED at the enforcement point** — a reference is not a control (`security` SEC-W15-12; §5.3) |
| | `threshold_at_decision`, `required_approvals_at_decision` | By value, so a later policy change cannot make a historical decision unauditable |
| | `pinned_payload` | The content the approval is *for*: `engagement_campaign_version_id`, `journey_version_id`, every `(offer_id, offer_version_id)`, `subject_set_hash`, `commission_rule_version_id`, `agreement_version_id` — whichever apply. A hash of this is what the consumption function matches against (§5.3) |
| **Idempotency** | `idempotency_key` | The **minting** key: the deterministic key under which *this EOI itself* is created exactly once (§3.3). Distinct from the per-call `idempotency_key`s of the executions beneath it, which continue to work exactly as they do today |
| **Audit** | `correlation_id` | doc 22's existing field, **inherited from the root, never re-minted** (§4.2) |
| | `audit_record_id`, `created_at` (`clock_timestamp()`), `created_by` | The common object contract (doc 10 W1) |
| Lifecycle | `status` | `open` \| `exhausted` \| `completed` \| `aborted` \| `superseded` |
| | `expires_at` | An authorization that can be executed forever is not an authorization. Mandatory; a platform maximum bounds the configured value |

### 2.3 What is deliberately NOT a field

| Not carried | Why |
|---|---|
| A balance, an accrual or a liability | It is a control object, not a financial one. `SUM(DEBITS)==SUM(CREDITS)` lives in the ledger and nowhere else (`CLAUDE.md`) |
| The resolved **beneficiary** set `B(O)` | `SEP-1` (§W15.1.2) requires each adopting domain to supply a **total**, deterministic resolver evaluated in the same transaction as the authorizing write, with an empty result being a refusal. Caching it on the EOI would make a stale copy authoritative for a control — the same reason doc 31 §3's `excluded` may not be a cached RG answer |
| An eligibility, RG, Risk or KYC outcome | Those are re-evaluated live at every checkpoint (doc 10 T.11) and recorded in the Eligibility Decision Record (doc 30 §7). An EOI that cached one would be a fourth gate |
| A retry/backoff schedule, a step list, a state machine of execution | That is a workflow engine. The EOI says *what is authorized*; each domain runs its own execution the way it already does |
| Message content, PII, contact endpoints | doc 16; doc 22's identity-evidence rule |

---

## 3. The rules that make it work

### 3.1 Who mints — a closed list

An EOI is minted **only** by an authorization event, and the list is
closed. If an execution path is not downstream of one of these, it has no
EOI and (per §5.1) is rejected.

| `operation_type` | Minted at | Root authorization |
|---|---|---|
| `bonus_bulk_grant` | `BulkGrantJob` creation (doc 10 W5) | The job's own four-eyes approval |
| `bonus_manual_grant` | A staff single-Grant action (doc 10 §1.3) | The staff action, plus four-eyes above threshold |
| `bonus_campaign_activation` | A **Bonus** Campaign being activated | Its activation approval |
| `crm_engagement_campaign_activation` | `EngagementCampaignVersion` activation (doc 31 §7.2.3) | Always four-eyes if it contains an `offer_request` step; otherwise four-eyes above an audience-size threshold with a fail-closed default |
| `affiliate_commission_settlement` | A `CommissionSettlementInstruction` being approved (doc 32 §6.5/§7) | The three-conjunct four-eyes of doc 32 §6.5.1 |
| `affiliate_reattribution` | A `PlayerAttribution` supersede (doc 32 §5.3.1) | Same three conjuncts |
| `manual_balance_adjustment` | A staff adjustment (`CLAUDE.md`) | Reason code + four-eyes above threshold |
| `api_initiated_grant` | An authenticated `ActorService` call to a grant surface (doc 10 N2.3 mode 7) | The service credential's own authorization grant — which is exactly the case that has no human approval today, and therefore the case §5.1's rejection rule is most important for |

**Deliberately absent from this list**: an ordinary player-initiated bet,
deposit or withdrawal. Those are single-subject, self-authorized, already
fully controlled by RG/Risk/AssetAuthorization on the live path, and
wrapping them in an EOI would add a control object to the hottest path in
the platform for no gain. **The EOI governs operations one party
authorizes *on behalf of, or affecting, many others* — that is the
decomposition surface.** Stated explicitly so nobody generalizes this
into a universal transaction wrapper.

### 3.2 `lineage_kind` — the four ways a child comes to exist, and why each is a child

This is the heart of the decomposition fix. Each kind exists because
there is a real execution pattern that would otherwise mint a root.

| Kind | Arises when | Binding rule |
|---|---|---|
| **`retry`** | A call is repeated after a lost response, a timeout, a transient error, or an operator "try again" | **A retry is NEVER a new root.** It inherits `parent_operation_id` and consumes no additional budget beyond what its original attempt consumed. If the original attempt's effect is unknown (the classic lost-response case), the per-call `idempotency_key` — unchanged, the existing mechanism — determines whether the effect already happened; the EOI determines whether the *authorization* still covers it |
| **`resume`** | A crashed/restarted job continues (doc 10 W5's resumability; a period-close settlement run) | A resume re-attaches to the existing EOI. Budget already consumed stays consumed: `BulkGrantJobItem`'s existing `UNIQUE (tenant_id, bulk_grant_job_id, player_account_id)` row is the per-item record of what was already spent, and the EOI is the aggregate ceiling those items count against |
| **`page`** | A large operation is executed in chunks | **Every page inherits.** `batch_ordinal`/`batch_total` are declared, and the sum of pages is checked against `recipient_ceiling` at the EOI, not per page. This is what makes "100 × 1k pages under a 5k ceiling" fail instead of succeed |
| **`item`** | One subject inside a batch | The per-item execution carries the parent EOI. Doc 10 W5's guarantee is unchanged — "N individual `issued` transitions sharing one job correlation id, never a batch-level bypass" — and the EOI is the object that makes the *converse* also true: never an item-level bypass of a batch-level control |
| **`compensation`** | A reversal, clawback, or correcting entry (doc 32 §7.1 rule 8's `reverses_instruction_id`) | Inherits, so a reversal is attributable to the operation it reverses, and **releases** rather than consumes budget |

**A child never has a wider scope than its parent.** Concretely,
enforced at creation:

- `child.recipient_ceiling ≤ parent.remaining_recipient_budget`
- `child.intended_aggregate_value ≤ parent.remaining_value_budget`
- `child.subject_set ⊆ parent.subject_set` (by hash for an enumerated
  set; by segment-reference containment for a criteria-defined one)
- `child.asset_code == parent.asset_code` (or parent's is null)
- `child.expires_at ≤ parent.expires_at`
- `child.approval_state` is **inherited, never re-derived** — a child
  cannot be `not_required` under an `approved` parent, and cannot be
  `approved` under a `pending` one

A violation of any of these is a **rejected write**, not a warning. A
child that needs a wider scope is a new authorization, and must go
through §3.1's minting path with its own approval.

### 3.3 Mint-once — how a retry fails to create a fresh authorization

The mechanism is deliberately the platform's existing one, not a new one:
**an EOI is created under a DB-unique `idempotency_key` derived
deterministically from the authorization**, never from the attempt.

```
   bonus_bulk_grant                 : (tenant_id, bulk_grant_job_id)
   crm_engagement_campaign_activation:(tenant_id, engagement_campaign_version_id,
                                       activation_ordinal)
   affiliate_commission_settlement  : (tenant_id, affiliate_node_id, period_id,
                                       accrual_set_hash)
   affiliate_reattribution          : (tenant_id, supersedes_attribution_id)
   manual_balance_adjustment        : (tenant_id, adjustment_request_id)
```

`UNIQUE (tenant_id, operation_type, idempotency_key)`, enforced by the
database — never a check-then-insert read (`CLAUDE.md`: "enforced by the
database, not 'check then insert' application logic").

Two properties follow directly:

- **A retry of the authorization** (an operator clicking "settle" twice,
  a lost-response retry) hits the unique constraint and **resolves to the
  existing EOI**, with its already-consumed budget and its already-
  consumed approval. It does not mint.
- **A retried *execution*** never touches the mint path at all: it
  inherits `parent_operation_id` from the record of its first attempt.

Note the shape deliberately mirrors doc 32 §7.1 rule 9's settled-accrual
link table (`UNIQUE (tenant_id, accrual_id)`): **a unique key on the
thing consumed, not only on the operation consuming it.** That is the
same correction, one level up.

### 3.4 Budget accounting — a projection, never a counter

`remaining_recipient_budget` and `remaining_value_budget` are
**derived at read time, inside the enforcing transaction**, from the
append-only record of what each child consumed — not maintained as a
mutable column.

This is `CLAUDE.md`'s balance rule applied to a control quantity, and for
the same reason: *"Never `UPDATE` a balance. Balances are projections
recomputed from ledger entries."* A mutable `remaining` column is a
counter that drifts, and a drifted authorization ceiling fails **open**.

Concretely: the consumption record is the domain's own already-existing
per-item row (`BulkGrantJobItem`, the settled-accrual link row, the
`Communication` row), carrying `parent_operation_id`. The projection is a
`SUM`/`COUNT` over those rows, `FOR UPDATE`-serialized on the EOI row
itself so two concurrent executions cannot both read the same remaining
budget. **The EOI row is the serialization point; the child rows are the
truth.**

A periodic recompute-and-diff of any materialized view of this is
required for the same reason the ledger's is (hourly projection diff,
`CLAUDE.md`), and any non-zero drift is treated as a control failure, not
a reporting nuisance.

---

## 4. How it composes with what already exists

The directive's constraint — *extend, do not invent* — made concrete.

### 4.1 It sits between `correlation_id` and `idempotency_key`, replacing neither

```
   correlation_id        "these things came from one request"     TRACING
        │                 doc 22 line 48 — unchanged
        │
   operation_id (EOI)    "these things share one AUTHORIZATION"   CONTROL   ← new
        │                 approval state, ceilings, lineage
        │
   idempotency_key       "this exact call happened once"          EXECUTION
                          doc 22 line 52, (provider_id, provider_tx_id),
                          (tenant_id, idempotency_key) — unchanged
```

All three are carried together. None subsumes another:

- Two different authorizations can legitimately share a
  `correlation_id` (one operator request that activates two campaigns).
- One authorization spans thousands of `idempotency_key`s.
- A `correlation_id` is never rejected for being fresh; an EOI is.

### 4.2 `correlation_id` is inherited, never re-minted

doc 22's `correlation_id` contract and doc 31 §13's threading rule
(*"journey instance → communication → offer request → Grant → ledger
transaction → audit record as one value, never re-minted"*) are unchanged
and are now **enforceable**: a child EOI copies its root's
`correlation_id`. A path that re-mints one is detectable by comparing the
two, which is a mechanically checkable invariant rather than a
convention.

### 4.3 Version pins are absorbed, not duplicated

`pinned_payload` **references** the version ids that already exist —
`campaign_version_id`, `offer_version_id`, `segment_version_id`,
`journey_version_id`, `commission_rule_version_id`,
`agreement_version_id`, `attribution_model_version_id`. It copies none of
their content, because each referent is immutable (doc 30 §7.1's
**EDR-R1**: reference the immutable, copy the mutable with its `as_of`).

`BulkGrantJob`'s existing `campaign_id`/`offer_version_id` fields are
therefore **not** duplicated onto the EOI — the EOI's `pinned_payload`
names them, and `BulkGrantJob` remains their home.

### 4.4 It does not become a fourth gate

Binding, and this is the boundary that keeps the concept small:

> The EOI answers **"is this execution within an authorization that was
> approved?"** It never answers "may this player receive value?" That
> remains, unchanged and unreordered,
> `AssetAuthorization → RG → Risk`, live, in the same transaction as the
> effect, fail-closed (doc 10 §T.1; doc 29 §8 BI-7; doc 33 §3.1 item 2).

It is likewise **not** `SEP-1` and does not subsume it.
`security`'s `SEP-1` (§W15.1) asks *"is the acting or approving principal
a beneficiary of this operation?"*; the EOI asks *"is this execution
inside an approved operation at all?"* Both must hold; neither implies
the other. A self-dealing operator can hold a perfectly valid approved
EOI, and an operation with no beneficiary conflict whatsoever can still
have no authorization. They are enforced at the same row — the
authorizing write — as two independent conjuncts.

An EOI check is an **additional necessary condition**, evaluated at the
entry of the acting domain's surface, *before* the gate chain and never
inside it. It can only ever cause a **denial**; it can never cause, waive,
soften, reorder or substitute for an approval of anything. A passing EOI
check is worth exactly as much at the gate as a `true` from
`segment.Resolve` — i.e. nothing (doc 30 §8.1, SEG-3).

### 4.5 Package placement

**`internal/economicop`** — one flat Go package, per the platform-wide
convention (doc 29 §1.1, verified: no subpackage exists anywhere under
`internal/`). Capability-minimal, in the same spirit as `internal/segment`
(doc 30 §2): mint, resolve, check-and-consume-budget, and the lineage
validator. **No scheduler, no workflow engine, no retry policy, no state
machine beyond the `status` enum, no HTTP surface of its own.**

Deliberately *not* placed inside `internal/audit`: audit records what
happened; the EOI constrains what may happen. Merging them would make a
write-only append-only store into an enforcement dependency.

Deliberately *not* placed inside `internal/auth`: `auth` answers "may
this principal perform this kind of action?" (a type question); the EOI
answers "is this particular action within an already-approved
operation?" (an instance question). They compose; they are not the same
question.

---

## 5. Enforcement — where the check actually happens

### 5.1 The fail-closed entry rule

At the entry of every §3.1 surface:

| Condition | Result |
|---|---|
| No `parent_operation_id` supplied, on a surface that requires one | **Reject** |
| `parent_operation_id` does not resolve, or resolves in another tenant | **Reject** |
| Resolved EOI's `approval_state` is not `approved` (or `not_required` with a *recorded* determination) | **Reject** |
| `status` is not `open`, or `expires_at` has passed | **Reject** |
| The execution's scope exceeds remaining budget (§3.2's five containment checks) | **Reject** |
| Any error resolving any of the above | **Reject** — fail closed, ADR 0037 §C.1 / `internal/risk`'s error contract |

**Rejection, never degradation.** Not "proceed with a warning", not
"truncate to the ceiling", not "queue for later". Doc 31 §7.2.3 item 4's
*"aborts on deviation — not 'warns', not 'truncates'"* is this rule, one
level down.

### 5.2 Enforced by the domain that CREATES the value, never by the requester

`security`'s SEC-W15-02 is explicit that CRM must not be the enforcer of
its own budget. Generalized:

| Domain | Mints | Enforces |
|---|---|---|
| **CRM** | `crm_engagement_campaign_activation` at activation approval | Nothing about grant volume. It carries and passes the identity |
| **Bonus** | `bonus_bulk_grant`, `bonus_manual_grant`, `bonus_campaign_activation` | **Every** grant-causing call, whoever the caller is — its own staff surfaces, `ActorService` callers, and CRM alike. Bonus creates the value, so Bonus holds the ceiling |
| **Affiliate** | `affiliate_commission_settlement`, `affiliate_reattribution` | The re-attribution path (its own value). Settlement's monetary enforcement is `ledger-finance`'s at the posting, with Affiliate enforcing the accrual-link uniqueness (doc 32 §7.1 rule 9) |
| **Ledger** | Nothing | Rejects a settlement whose instruction carries no resolvable approved parent. It already re-derives the amount (doc 32 §7.1 rule 6); this is the authorization half of the same posture |

The rule stated plainly: **the domain whose tables change is the domain
that checks.** A requester-enforced control is removable by a refactor,
a bug, or a bought product's own engine.

### 5.3 Budget and approval are CONSUMED, atomically, in the effecting transaction

Both use the pattern this platform already implements and has already
reviewed — `asset_change_consume_approved_request` (migrations
`0044_asset_registry_failclosed_and_dual_control.up.sql:474`, hardened in
`0047_asset_registry_dual_control_hardening.up.sql:271`):

| Property | Why it is needed here |
|---|---|
| `SELECT … FOR UPDATE` on the EOI row | Two concurrent executions cannot both read the same remaining budget |
| The four-eyes predicate (`approver_principal_id <> requested_by_principal_id`) **inside the selection predicate**, not as a separate check | A caller that forgets to check cannot succeed — an unapproved operation is simply not selectable |
| Payload containment (`payload @> match`) | The execution must match the **pinned** payload (§2.2), not merely reference an approval that exists |
| State transition in the same transaction | The approval is **spent**; a replay finds nothing and raises |
| `RAISE EXCEPTION` on no match | Fail closed and loud, aborting the whole statement |

`security` referred to this as the `bonus_change_consume_approved_request`
pattern; the function implemented in this repository is the
`asset_change_*` one named above, and it is the concrete precedent. Whether
the EOI version is a generalization of it or a sibling is `security`'s and
`ledger-finance`'s call (doc 32 DEP-AFF-6, doc 31 DEP-CRM-5); the
properties are binding either way.

### 5.4 Worked example — the SEC-W15-02 attack, before and after

A 100,000-member audience, an `EngagementCampaign` with one
`offer_request` step, a €10 offer.

| Step | Before (Wave 1.5 as designed) | After |
|---|---|---|
| Operator authors the campaign | `crm_config:manage` | `crm_offer_request:configure` — a **distinct** authority (doc 31 §12.2) |
| Activation | An ordinary status change | Mints `crm_engagement_campaign_activation`. Contains an `offer_request` step ⇒ **four-eyes ALWAYS**, no threshold. Approval pins the campaign/journey/offer versions and the audience-definition hash; `recipient_ceiling` and `intended_aggregate_value` recorded |
| Journey runs | 100,000 individual grant calls | 100,000 executions, **each carrying `parent_operation_id`**, each `lineage_kind = item` |
| Bulk control | **Never triggered** — no `BulkGrantJob` exists | Irrelevant which surface is used: Bonus checks the parent EOI on **every** grant-causing call |
| Exceeding the ceiling | Nothing to exceed | Execution 100,001 finds zero remaining budget ⇒ **rejected**, loudly, with the operation id in the audit record |
| Splitting into 100 × 1k pages | 100 sub-threshold batches, all pass | Each page is a `page` child; `child.recipient_ceiling ≤ parent.remaining` ⇒ the sum cannot exceed the parent's ceiling. Page 6 fails |
| Retrying after a crash | A fresh, unapproved context | `resume` re-attaches to the same EOI; already-spent budget stays spent (the existing `BulkGrantJobItem` rows are the record) |
| Retrying the *activation* | A second activation | Hits `UNIQUE (tenant_id, operation_type, idempotency_key)` ⇒ resolves to the existing EOI with its already-**consumed** approval |
| A journey step calling the single-Grant surface instead | Same bypass, different door | Same rejection — the check is on the **surface**, not on the shape of the call |

---

## 6. Invariants (for `qa` and `code-reviewer`; each mechanically checkable)

| ID | Invariant | Check |
|---|---|---|
| **EOI-1** | Every §3.1 surface rejects a call with no resolvable, `approved`, `open`, unexpired `parent_operation_id`. Every failure mode of resolution denies | Fail-closed integration test per surface, mirroring `internal/risk/fail_closed_integration_test.go` |
| **EOI-2** | An EOI is created exactly once per authorization: `UNIQUE (tenant_id, operation_type, idempotency_key)`, DB-enforced, never check-then-insert | Concurrency test: N simultaneous mint attempts ⇒ exactly one row |
| **EOI-3** | A retry, a resume, a page and an item **all** resolve to the same `root_operation_id` as their first attempt; none mints a root | Interrupt/resume test on a bulk job and on a period settlement; assert one root, one approval consumption |
| **EOI-4** | A child's scope never exceeds its parent's: all five §3.2 containment checks are enforced as rejected writes | Property test over the five, plus the explicit "100 × 1k pages under a 5k ceiling" case |
| **EOI-5** | Remaining budget is **derived** from append-only child records inside the enforcing transaction; no mutable `remaining` column is the authority | Schema inspection + a recompute-and-diff test |
| **EOI-6** | Budget and approval are consumed atomically with the effect, via a `FOR UPDATE` + payload-containment + state-transition function; a replay raises | Replay test; concurrent-consume test |
| **EOI-7** | `approval_state = not_required` is never a default: every such row names the policy row and threshold that produced it. Absent policy ⇒ threshold `0`, `required_approvals` `2` | Schema constraint + a zero-config test asserting approval is required |
| **EOI-8** | `correlation_id` is inherited from the root and never re-minted along a lineage | Test comparing root and leaf |
| **EOI-9** | The EOI check is never inside the `AssetAuthorization → RG → Risk` chain and never alters its order or outcome; `internal/rg`, `internal/risk`, `internal/kyc`, `internal/assetregistry` never import `internal/economicop` | Import inspection (the technique doc 02 uses for the risk/rg separation) + code review of the call ordering |
| **EOI-10** | `economic_operations` carries `tenant_id NOT NULL`, `FORCE ROW LEVEL SECURITY`, the `app.player_account_id IS NULL` conjunct on its staff-scope policy, and **no player-facing or affiliate-facing read policy at all** | Schema inspection + cross-tenant/cross-player/cross-principal-class RLS tests (the Stage 4H-B0-R6 F2 gap shape) |
| **EOI-11** | No floating-point money: `intended_aggregate_value` is integer minor units in `NUMERIC(38,0)`-compatible form against the registry exponent, never `int64` | Type inspection + a multi-exponent test (0/2/6/8/18) |
| **EOI-12** | `internal/economicop` contains no scheduler, no retry policy, no workflow/saga state machine, no HTTP surface, and no eligibility/RG/Risk/KYC concept | Repo inspection + grep |
| **EOI-13** | Every EOI mint, approval, consumption, rejection and status change writes an `audit.Record` in the same transaction (actor, tenant, entity, before/after, reason code) | Count-matching test (`CLAUDE.md`; ADR 0013) |

---

## 7. What is NOT authorized by this document

- No implementation. `internal/economicop` does not exist and is not
  authorized to exist by this document.
- No migration, no table, no route.
- **No retrofitting onto the player-initiated bet/deposit/withdrawal
  path** (§3.1's deliberate absence). Proposing that is scope expansion
  and should be rejected.
- No workflow/saga engine, no scheduler, no outbox, no broker — the
  event-transport decision (doc 31 §6.2, OI-CRM-1, doc 22 open decision
  1) is untouched by this document and is not resolved by it.
- No new principal type, actor type, or permission taxonomy —
  `security` owns those (doc 31 §12.2, doc 32 §3.2).
- No human decision is selected: G-2, `OpenBetSelfExclusionPolicy`,
  cashout policy and FD-1 are untouched (ADR 0039).

---

## 8. Open items and routed dependencies

| ID | Item | Owner | Blocking? |
|---|---|---|---|
| **DEP-EOI-1** | **Bonus must enforce §5.1 at every grant-causing surface** — its own staff surfaces, `ActorService` callers, and CRM alike. `bonus-engine` owns doc 10 and is independently revising §N1–N2 this same Fix Wave; it has not seen this document | bonus-engine | **Yes** — doc 31 §7.2.3 items 1/3/4 are unenforceable without it |
| **DEP-EOI-2** | The consumption function (§5.3) — a generalization of, or sibling to, `asset_change_consume_approved_request`. Same dependency as doc 32 DEP-AFF-6 and doc 31 DEP-CRM-5; **one function should serve all three**, and deciding that is `security` + `ledger-finance`'s | security + ledger-finance | Yes, before implementation |
| **DEP-EOI-3** | **`security` has since PUBLISHED the invariant as `SEP-1`** (`security-architecture.md` §W15.1): unconditional, threshold-independent, refusing on any unresolvable or empty beneficiary set, with each adopting domain supplying a resolver and an enforcement point and **nothing else**. The EOI composes with it and does not duplicate it: `SEP-1` asks *"is the actor a beneficiary?"*; the EOI asks *"is this execution inside an approved operation?"* — both must hold, neither implies the other. Remaining dependency: the Person-linkage primitive underneath (`identity-compliance`, `4HB1FW-05`, not seen here) | identity-compliance (+ security) | Yes, before implementation |
| **DEP-EOI-4** | `risk` is independently reviewing this concept against Risk's own operation/eligibility model this round. If `risk` finds that `operation_type` should align with, or stay deliberately distinct from, ADR 0031's `Operation` enum, that is `risk`'s call — this document deliberately does **not** reuse Risk's enum (an EOI is not a risk-evaluated operation), and the reasoning should be confirmed or corrected by the owner | risk | No |
| **DEP-EOI-5** | **(new)** `security`'s `CRM-BR-1`/§W15.1.2 (pin the **materialized** subject set; never re-resolve at execution) and `bonus-engine`'s doc 10 **W5** (resolve **live** at run time) point in opposite directions. §2.2's `subject_set_hash` adopts the ceiling reconciliation — the pin bounds from above, live resolution may only shrink — but **neither owner has confirmed it**. Same item as doc 31 **DEP-CRM-7** | security + bonus-engine | Yes, before the first pinned-audience execution |
| **OI-EOI-1** | An ADR ratifying this concept is recommended. **Number deliberately not claimed** — parallel-dispatch ADR-number collision has already occurred once in this project (Stage 4H-B0-R6's `0049`) and twice now in this gate's numbering discussions | Orchestrator | No |
| **OI-EOI-2** | `economic_owner` (§2.2) intersects `ledger-finance`'s **BF-1** finding (concurrent operator-funded and provider-funded Grants share one fungible `player_bonus` balance with no lot-attribution mechanism), which is deferred to `architect` + a human decision. This document introduces the *field*, not a resolution, and Wave 2's operator-funded-only restriction is unaffected | architect + human | No |
| **OI-EOI-3** | Retention of `economic_operations` rows: an authorization record is audit-adjacent and inherits doc 16's unresolved retention-period question (a legal decision, not an engineering one) | identity-compliance → human | No |

## 9. Cross-references

- The defect this closes: `security`'s SEC-W15-02 (Wave 1.5 Phase 2); `docs/governance/task-registry.md`
- CRM's binding to it: `31-crm-engine-architecture.md` §7.2.3, §7.2.4, DEP-CRM-5
- Affiliate's binding to it: `32-affiliate-and-acquisition-architecture.md` §5.3.1, §7, AI-23, DEP-AFF-10
- Bonus's surfaces it governs: `10-bonus-engine-architecture.md` W5, §1.3, N2.2–N2.6
- Existing identity mechanisms it extends: `22-canonical-activity-event-taxonomy.md` (envelope: `correlation_id`, `operation_ref`, `idempotency_key`, `reverses_ref`)
- The consumption pattern it adopts: `migrations/0044_asset_registry_failclosed_and_dual_control.up.sql`, `migrations/0047_asset_registry_dual_control_hardening.up.sql`
- The fail-closed default it mirrors: `internal/withdrawal/policy.go` (`defaultApprovalPolicy`)
- Gate order it must not join: `10-bonus-engine-architecture.md` §T.1; `29-bonus-implementation-contract.md` §8 (BI-7); `33-cross-domain-commercial-flow-map.md` §3.1 item 2
- Evidence/reconstruction rules it follows: `30-segmentation-engine-architecture.md` §7.1 (EDR-R1/EDR-R2)
- Flow map and dependency graph: `33-cross-domain-commercial-flow-map.md`
- Unmade human decisions (none selected here): `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`

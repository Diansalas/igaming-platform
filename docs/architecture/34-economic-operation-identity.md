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

**Revision (Wave 1.5 Fix Round 2, `architect`)**: two independent Phase 2
reviews (`code-reviewer` finding NEW-2; `risk` findings RK-W15P2-2 through
RK-W15P2-5 and RK-W15P2-8 part B) proved the version below this line was
insufficient — as written, its own worked example (§5.5, formerly §5.4)
did not follow from its own enforcement rule (§3.4), and the design as a
whole did **not** close SEC-W15-02. This revision replaces §3.4, extends
§2.2/§3.1/§3.2/§5, and adds invariants EOI-14 through EOI-17. Every
change is mapped to the finding that required it inline. Still
**DESIGN/ARCHITECTURE ONLY** — nothing below authorizes implementation.

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
| | **`intended_aggregate_value`** | Integer minor units, decimal-string wire form, `NUMERIC(38,0)`-compatible — **never** `int64`, never floating point (`CLAUDE.md`; ADR 0021; doc 21's exponent-18 correction). The *authorized* aggregate, by value, at approval time. **A null `asset_code` EOI has no enforceable value budget** — an `intended_aggregate_value` on a non-monetary operation, if present at all, is informational only, and §5.4's consumption function must never be asked to enforce a value ceiling against a null asset (RK-W15P2-5, stated explicitly rather than left implicit in the asset_code row above) |
| | **`recipient_ceiling`** | An integer **count of distinct subjects reached**, not a row count. Mandatory for `criteria_defined` scope (doc 31 §7.2.3 item 3). **Monotone non-decreasing, never released by a compensating entry** (RK-W15P2-2, §3.2, §3.4): a recipient granted and later clawed back has still been *reached* — clawing back the value does not un-reach them. Consumption is `COUNT(DISTINCT subject)` over the append-only consumption rows in the whole lineage subtree, computed the same way as the value budget (§3.4) but netted differently: only the value budget nets under compensation, `recipient_ceiling` never does |
| | **`per_window_ceiling`** + `window` | For a continuously-running authorization (a live journey): the maximum executions per declared window |
| | `value_measure_basis` | How `intended_aggregate_value` was computed and its `as_of` — EDR-R1's "copy the mutable with its as-of" (doc 30 §7.1) applied to a control input |
| **Lineage** | **`parent_operation_id`** | Nullable **only** for a root operation. Non-null for every derived execution |
| | `root_operation_id` | Denormalized for query sanity; equals `operation_id` for a root. A closure/recursive query is correct but makes the enforcement path a recursive CTE, which is the wrong thing to put on a write path. **This is the field the whole enforcement mechanism turns on (§3.4): every budget is a subtree-wide aggregate keyed on `root_operation_id`, never on `parent_operation_id` alone** — code-reviewer's NEW-2 finding, closed below |
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
| `bonus_held_disposition_resolution` | A staff action clearing a `HeldDispositionRecord` (doc 10 §N1.4.1/5c: `ACTION_REFORFEIT` or `ACTION_ROUTE_TO_CASH`, introduced by this round's LF-2 fix) | The staff action's reason code, plus four-eyes above `CLAUDE.md`'s threshold — same shape as `manual_balance_adjustment`, but scoped to a specific Grant's holding representation rather than an arbitrary ledger posting. **Resolves RK-W15P2-8 part B**: doc 10's LF-2 fix introduces a new staff-authorized, value-creating operation with no `operation_type` in the version of this table Phase 2 reviewed. Added here via this document's own §2.2 extension discipline rather than left as a gap. Value is known exactly at this point (the `HeldDispositionRecord`'s captured amount) — this is not an instance of §3.4's value-unknown-at-execution case, which applies to the *issuing* Grant's own EOI (e.g. cashback at `issued`), not to this later, amount-known disposition step |

**Deliberately absent from this list**: an ordinary player-initiated bet,
deposit or withdrawal. Those are single-subject, self-authorized, already
fully controlled by RG/Risk/AssetAuthorization on the live path, and
wrapping them in an EOI would add a control object to the hottest path in
the platform for no gain. **The EOI governs operations one party
authorizes *on behalf of, or affecting, many others* — that is the
decomposition surface.** Stated explicitly so nobody generalizes this
into a universal transaction wrapper.

**`operation_type` is a fully separate, non-derived vocabulary from
`internal/risk`'s `Operation` enum (DEP-EOI-4, `risk`-confirmed) —
binding, not a suggestion.** An EOI answers "is this execution inside an
approved operation?"; a Risk `Operation` answers "which posting shape and
limit-evaluation path does this request take?" They classify different
things and will diverge over time for reasons specific to each (a new
Risk `Operation` needs a `cumulativeSpec`, doc 34's `operation_type` needs
a minting authority and a consumption-record declaration — §3.4). Binding
requirements for whoever implements either enum:

- **No derivation, ever.** No `operation_type` value may be computed from,
  aliased to, defaulted from, or kept in lockstep with a Risk `Operation`
  value by convention, naming pattern, or shared constant. Each domain
  owns and extends its own enum independently, through its own additive
  discipline (this document's, ADR 0031 §12 for Risk's).
- **Vocabulary-disjointness test, mandatory before either implementation
  merges.** A test asserting `operation_type`'s value set and
  `internal/risk.Operation`'s value set share no member and that no
  function in the codebase maps one to the other. This is the same
  discipline `internal/risk`'s own import-inspection technique (doc 34
  EOI-9, mirroring doc 02's risk/RG separation check) applies one level
  up: two vocabularies that happen to look similar today are exactly the
  ones a future refactor quietly merges unless a test forbids it.

### 3.2 `lineage_kind` — the four ways a child comes to exist, and why each is a child

This is the heart of the decomposition fix. Each kind exists because
there is a real execution pattern that would otherwise mint a root.

| Kind | Arises when | Binding rule |
|---|---|---|
| **`retry`** | A call is repeated after a lost response, a timeout, a transient error, or an operator "try again" | **A retry is NEVER a new root.** It inherits `parent_operation_id` and consumes no additional budget beyond what its original attempt consumed. If the original attempt's effect is unknown (the classic lost-response case), the per-call `idempotency_key` — unchanged, the existing mechanism — determines whether the effect already happened; the EOI determines whether the *authorization* still covers it |
| **`resume`** | A crashed/restarted job continues (doc 10 W5's resumability; a period-close settlement run) | A resume re-attaches to the existing EOI. Budget already consumed stays consumed: `BulkGrantJobItem`'s existing `UNIQUE (tenant_id, bulk_grant_job_id, player_account_id)` row is the per-item record of what was already spent, and the EOI is the aggregate ceiling those items count against |
| **`page`** | A large operation is executed in chunks | **Every page inherits.** `batch_ordinal`/`batch_total` are declared, and the sum of pages is checked against `recipient_ceiling` at the EOI, not per page. This is what makes "100 × 1k pages under a 5k ceiling" fail instead of succeed |
| **`item`** | One subject inside a batch | The per-item execution carries the parent EOI. Doc 10 W5's guarantee is unchanged — "N individual `issued` transitions sharing one job correlation id, never a batch-level bypass" — and the EOI is the object that makes the *converse* also true: never an item-level bypass of a batch-level control |
| **`compensation`** | A reversal, clawback, or correcting entry (doc 32 §7.1 rule 8's `reverses_instruction_id`) | Inherits, so a reversal is attributable to the operation it reverses, and **releases budget — but only the VALUE budget** (`remaining_value_budget`, §3.4). **`recipient_ceiling` is never released by a compensation** (RK-W15P2-2): a clawed-back recipient has still been reached, so `remaining_recipient_budget` does not grow back. This is a deliberate asymmetry between the two budgets, not an oversight — netting recipient count under compensation would let the exact SEC-W15-02 shape recur one level up (grant, clawback, re-grant, clawback, re-grant... each cycle "freeing" a recipient slot while the true audience reached keeps growing) |

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

**Creating a child does NOT reserve its declared ceiling up front**
(NEW-2, answering the question the prior version of this document left
ambiguous). The check above (`child.recipient_ceiling ≤
parent.remaining_recipient_budget`) is evaluated **at child-creation
time, non-locking, against the subtree's aggregate consumption as it
stands at that instant** — it is an early, advisory rejection of an
obviously-oversized child, not a hold. It is therefore possible, by
design, for many children's *declared* ceilings to sum to more than the
root's ceiling at the moment they are created (100 pages of
`recipient_ceiling=1000` each may all be created under a root ceiling of
5,000, since none of them has consumed anything yet). This is safe
**only because it is not the enforcement point** — §3.4 is. See §3.4 for
why this is correct rather than a loophole, and §5.5 for the corrected
worked example.

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

### 3.4 Budget accounting — a projection over the WHOLE lineage subtree, never a counter and never scoped to the direct parent

**This section replaces the version `code-reviewer` proved defeats the
whole mechanism (NEW-2).** The defect, restated precisely: the prior text
said the projection was "`SUM`/`COUNT` over [child rows], `FOR
UPDATE`-serialized on the EOI row" without saying *which* EOI row each
page's own check ran against. A literal reading has each page compute its
**own** remaining budget from rows where `parent_operation_id = <that
page>` — which is always zero before that page has executed anything,
since real consumption rows are written under the page, not under the
root. Meanwhile the root's own remaining budget, computed the same way,
never moves, because no consumption row ever carries `parent_operation_id
= root` directly. Result: a root ceiling of 5,000 constrains nothing,
because nothing is ever measured against it. This is the exact SEC-W15-02
shape recurring one level inside the fix that was supposed to close it.

**The fix: there is exactly one budget per authorization, and it is
always computed over the entire subtree rooted at `root_operation_id`,
never over a single generation of parentage.**

```sql
-- remaining_recipient_budget for root R (COUNT, never released by compensation):
SELECT R.recipient_ceiling - COUNT(DISTINCT c.subject_ref)
FROM   economic_operations R
JOIN   economic_operations eoi ON eoi.root_operation_id = R.operation_id
JOIN   <declared consumption row(s) for eoi.operation_type> c
       ON c.parent_operation_id = eoi.operation_id
WHERE  R.operation_id = $root_id
  AND  R.lineage_kind = 'root'
FOR UPDATE OF R;

-- remaining_value_budget for root R (SUM, nets under compensation):
SELECT R.intended_aggregate_value
       - COALESCE(SUM(c.value) FILTER (WHERE eoi.lineage_kind <> 'compensation'), 0)
       + COALESCE(SUM(c.value) FILTER (WHERE eoi.lineage_kind =  'compensation'), 0)
FROM   economic_operations R
JOIN   economic_operations eoi ON eoi.root_operation_id = R.operation_id
JOIN   <declared consumption row(s) for eoi.operation_type> c
       ON c.parent_operation_id = eoi.operation_id
WHERE  R.operation_id = $root_id
  AND  R.lineage_kind = 'root'
FOR UPDATE OF R;
```

`root_operation_id` (§2.2) exists **precisely** so this is a join against
an indexed column, not a recursive CTE walking the lineage tree on every
write — the prior version's own stated reason for denormalizing it, now
actually used for it.

**No reservation at child creation (answering NEW-2's explicit question,
stated once here as the canonical answer — §3.2 cross-refers to it):**
creating a `page`/`item`/child EOI does **not** consume or reserve any
budget. The only two places budget is ever measured are:

1. **§3.2's non-locking containment check**, at child creation, against
   the subtree aggregate **as it stands at that instant** — early,
   advisory, rejects an obviously-oversized child, but does not prevent
   many siblings' declared ceilings from summing to more than the root's
   ceiling (§3.2).
2. **§5.4's locking consume**, exactly once per actual effecting write,
   which is the real enforcement point.

This means 100 pages of `recipient_ceiling=1000` under a root ceiling of
5,000 **may all be created** — and that is correct, not a hole, because
none of them has executed anything yet. What cannot happen is more than
5,000 *actual* grants executing: the 5,001st execution, in whichever page
it falls, computes `remaining_recipient_budget` as the subtree-wide
`COUNT(DISTINCT subject_ref)` above, finds it exhausted, and is rejected
at §5.4's atomic consume — **regardless of which page or item lineage_kind
it carries**. See §5.5 for the corrected worked example (the previous
text's "page 6 fails" was wrong under these — the design's own — rules;
it only holds under reservation-at-creation semantics, which this section
explicitly rejects).

**`FOR UPDATE` is always taken on the ROOT row, never on an intermediate
page's row** (closing the ambiguity `risk`'s RK-W15P2-3 identified between
this section and §5.3 as originally written — see §5.3 for the full
canonical-ordering rule this requires). A page or item never has its own
independently-enforced budget; it has a *declared* ceiling checked
non-locking against the root's aggregate at creation (§3.2), and its
actual executions consume against the root's lock at effecting time.

**The two budgets net differently under compensation (RK-W15P2-2):**
`remaining_value_budget` nets — a clawback's `value` is added back with
the opposite sign, consistent with `CLAUDE.md`'s ledger netting.
`remaining_recipient_budget` does **not** net — it is monotone
non-decreasing over the append-only consumption rows regardless of
`lineage_kind = compensation`, because a clawed-back recipient has still
been *reached* (§2.2, §3.2).

**Consumption-record shape must be declared per `operation_type`, and an
undeclared shape is refused, not silently ignored (RK-W15P2-4).** The
`<declared consumption row(s)>` join target above is not implicit. This
platform has already had, and fixed, exactly this defect once:
`internal/risk/cumulative.go`'s `cumulativeSpec` requires every
`Operation` to declare its `MeasuredAccountTypes` and
`IgnoredAccountTypes` explicitly, and returns
`ErrUnrecognizedCumulativeLeg` — not a silent zero — the moment a query
observes a player-side ledger leg the spec didn't declare either way.
The EOI mechanism requires the identical discipline, one level up: each
`operation_type` in §3.1's table must declare **exactly which child-row
table(s) and column(s) constitute its consumption record** (for example:
`bonus_bulk_grant` → `BulkGrantJobItem` rows keyed by
`parent_operation_id`, counted by `player_account_id`, valued by
`granted_amount`). If a domain later writes a **second** kind of
consumption row for the same `operation_type` — concretely, the moment
this round's `HeldDispositionRecord` exists, a `bonus_held_disposition_resolution`
EOI (§3.1) could in principle be consumed against by more than one row
shape — the consumption function must **raise**, not under-count, the
moment it finds a `parent_operation_id` on a row shape the declaration
for that `operation_type` does not name. Whoever implements
`internal/economicop` names the equivalent of
`ErrUnrecognizedCumulativeLeg` for this domain; this document does not
pick the Go error name, but does bind the *behavior*: fail closed on an
undeclared shape, always.

**Value-unknown-at-execution-time operations consume the conservative
maximum, never zero or a placeholder (RK-W15P2-5).** Some grants —
cashback is the concrete first-slice case (doc 10 §2) — have a value not
known until a settlement job closes a window well after `issued`. Per
ADR 0031 §42(c)'s already-established rule for the identical shape inside
Risk (`ErrMissingAmount` on a zero-amount request; the two acceptable
resolutions are the Offer's declared ceiling as a conservative maximum,
or making the amount-known checkpoint the authoritative gate — never a
placeholder), the EOI mechanism adopts the same rule at the same layer:
**an `operation_type` whose value is not known at execution time consumes
the Offer's declared maximum possible value against `remaining_value_budget`
at that execution**, not zero, not a sentinel, not the campaign average.
This is conservative by construction — if the ceiling passes, the actual
(smaller-or-equal) realized value also passes — and it means a batch of
cashback issuances can exhaust `remaining_value_budget` before any of
them actually settles, which is correct: the authorization bounded the
*maximum exposure it created*, and that exposure exists from `issued`,
not from settlement. (Recall from §2.2: a **null** `asset_code` EOI has
no enforceable value budget at all — this rule applies only when
`asset_code` is non-null and a value is simply not yet *known*, a
different case from *not applicable*.)

**A periodic recompute-and-diff** of any materialized view of either
budget is required for the same reason the ledger's is (hourly projection
diff, `CLAUDE.md`), and any non-zero drift is treated as a control
failure, not a reporting nuisance.

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

### 5.3 Canonical lock ordering — one rule, resolving the §5.1/§5.4 ambiguity `risk` found (RK-W15P2-3)

The prior version of this document left the lock-acquisition point
ambiguous between "entry check, non-locking" (§5.1) and "effecting-
transaction consume" (what is now §5.4), and never stated its order
relative to Risk's own advisory lock (`internal/risk/evaluator.go`'s
`pg_advisory_xact_lock`, taken inside `Rule.breach()`,
`evaluator.go:338`). Left that way, a naive implementation could hold an
EOI row lock across the **entire** gate chain
(`AssetAuthorization → RG → Risk` + the ledger posting) for every one of
100,000 items in a job — turning an EOI throughput concern into Risk
fail-closed denials under load (Risk's own advisory lock contends with
whatever else is holding it), and risking an AB-BA deadlock if different
call paths ever acquired the two locks in different orders.

**The canonical rule, stated once, binding everywhere `internal/economicop`
is called from:**

1. **§5.1's entry check is non-locking.** It may only reject early
   (no resolvable/approved/open/unexpired parent, or an obviously
   oversized child per §3.2). It never takes `FOR UPDATE` and is not the
   enforcement point.
2. **The gate chain runs next, unchanged and unreordered** — exactly
   §4.4's `AssetAuthorization → RG → Risk`, including Risk's own
   `pg_advisory_xact_lock`. The EOI is not part of this chain and is
   never interleaved with it.
3. **The locking consume (§5.4) happens exactly once, strictly AFTER the
   gate chain completes and immediately before the effecting write**, in
   the same transaction as that write. It is always taken on the **ROOT**
   row identified by `root_operation_id` (§3.4) — never on an
   intermediate page's or item's own row, which has no independent lock
   to take.
4. **Ordering relative to Risk: Risk's advisory lock is always acquired
   BEFORE the EOI row lock, never after.** Because step 2 always precedes
   step 3, this is automatic as long as no call site is restructured to
   take the EOI lock earlier "for efficiency" — which step 1 makes
   unnecessary, since the cheap non-locking rejection already happened.
   This fixed ordering is what makes an AB-BA deadlock between the two
   locks structurally unreachable: every path takes Risk's lock, then the
   EOI lock, never the reverse.

This makes the EOI row lock's hold time exactly one effecting write, not
a whole gate chain — the throughput property the prior ambiguity put at
risk.

### 5.4 Budget and approval are CONSUMED, atomically, in the effecting transaction

Both use the pattern this platform already implements and has already
reviewed — `asset_change_consume_approved_request` (migrations
`0044_asset_registry_failclosed_and_dual_control.up.sql:474`, hardened in
`0047_asset_registry_dual_control_hardening.up.sql:271`):

| Property | Why it is needed here |
|---|---|
| `SELECT … FOR UPDATE` on the **root** EOI row (§5.3 rule 3, §3.4) | Two concurrent executions — anywhere in the subtree, whichever page or item they belong to — cannot both read the same remaining budget |
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

### 5.5 Worked example — the SEC-W15-02 attack, before and after

A 100,000-member audience, an `EngagementCampaign` with one
`offer_request` step, a €10 offer, `recipient_ceiling = 5,000` at
activation approval. **Corrected from the prior version, which described
"page 6 fails" — that only holds under reservation-at-creation semantics,
which §3.4 explicitly rejects. Under the rules this document actually
specifies, page creation is not the enforcement point; the 5,001st actual
grant execution is, wherever it falls.**

| Step | Before (Wave 1.5 as designed) | After |
|---|---|---|
| Operator authors the campaign | `crm_config:manage` | `crm_offer_request:configure` — a **distinct** authority (doc 31 §12.2) |
| Activation | An ordinary status change | Mints `crm_engagement_campaign_activation`. Contains an `offer_request` step ⇒ **four-eyes ALWAYS**, no threshold. Approval pins the campaign/journey/offer versions and the audience-definition hash; `recipient_ceiling = 5,000` and `intended_aggregate_value` recorded on the **root** |
| Journey runs | 100,000 individual grant calls | 100,000 executions, **each carrying `parent_operation_id`**, each `lineage_kind = item` |
| Bulk control | **Never triggered** — no `BulkGrantJob` exists | Irrelevant which surface is used: Bonus checks the parent EOI on **every** grant-causing call |
| Exceeding the ceiling | Nothing to exceed | Execution 100,001 finds zero remaining budget ⇒ **rejected**, loudly, with the operation id in the audit record |
| Splitting into 100 × 1k pages | 100 sub-threshold batches, all pass | All 100 pages **may be created** — page creation does not reserve budget (§3.2, §3.4). But actual item executions are consumed against the **root's** subtree-wide aggregate (§3.4), regardless of which page they run under: the **5,001st actual grant execution across the whole subtree** — which could be item 1 of page 6, or item 800 of page 5, depending on execution order — finds `remaining_recipient_budget = 0` at §5.4's locking consume and is **rejected**. The other 94,999 create-only pages never get to execute past that point either, for the same reason. The sum can never exceed 5,000 **executed** grants, which is the actual property that matters |
| Retrying after a crash | A fresh, unapproved context | `resume` re-attaches to the same EOI; already-spent budget stays spent (the existing `BulkGrantJobItem` rows are the record) |
| Retrying the *activation* | A second activation | Hits `UNIQUE (tenant_id, operation_type, idempotency_key)` ⇒ resolves to the existing EOI with its already-**consumed** approval |
| A journey step calling the single-Grant surface instead | Same bypass, different door | Same rejection — the check is on the **surface**, not on the shape of the call |
| A recipient granted, then clawed back, then targeted again by a later page | Not modeled | `recipient_ceiling` consumption is `COUNT(DISTINCT subject_ref)` and never releases on the clawback (RK-W15P2-2) — the recipient still counts once against the 5,000, permanently |

---

## 6. Invariants (for `qa` and `code-reviewer`; each mechanically checkable)

| ID | Invariant | Check |
|---|---|---|
| **EOI-1** | Every §3.1 surface rejects a call with no resolvable, `approved`, `open`, unexpired `parent_operation_id`. Every failure mode of resolution denies | Fail-closed integration test per surface, mirroring `internal/risk/fail_closed_integration_test.go` |
| **EOI-2** | An EOI is created exactly once per authorization: `UNIQUE (tenant_id, operation_type, idempotency_key)`, DB-enforced, never check-then-insert | Concurrency test: N simultaneous mint attempts ⇒ exactly one row |
| **EOI-3** | A retry, a resume, a page and an item **all** resolve to the same `root_operation_id` as their first attempt; none mints a root | Interrupt/resume test on a bulk job and on a period settlement; assert one root, one approval consumption |
| **EOI-4** | A child's scope never exceeds its parent's at creation (all five §3.2 containment checks, non-locking, are enforced as rejected writes); **and, separately, no more than `recipient_ceiling` actual executions ever complete across the ENTIRE lineage subtree**, aggregated by `root_operation_id`, never by direct `parent_operation_id` alone (§3.4, NEW-2) | Property test over the five containment checks; **the explicit "100 pages of `recipient_ceiling=1000` created under a 5,000 root ceiling, then all attempt to execute" case, asserting exactly 5,000 executions succeed and the 5,001st — in whichever page it falls — is rejected** (replacing the "page 6 fails" framing the prior version of this invariant's check implied) |
| **EOI-5** | Remaining budget is **derived** from append-only child records inside the enforcing transaction, aggregated over the whole subtree via `root_operation_id`; no mutable `remaining` column is the authority, and no per-page or per-item local aggregate is ever treated as authoritative | Schema inspection + a recompute-and-diff test + a test asserting a page's own `parent_operation_id`-scoped count is never queried as a budget source |
| **EOI-6** | Budget and approval are consumed atomically with the effect, via a `FOR UPDATE` (on the ROOT row, §5.3/§5.4) + payload-containment + state-transition function; a replay raises | Replay test; concurrent-consume test; **and a mandatory negative control** (`risk`'s requested pattern, copied from `internal/risk/cumulative_race_integration_test.go`): the identical concurrency test re-run with the `FOR UPDATE` statement replaced by a no-op, which must deterministically **overshoot** `recipient_ceiling` — proving the lock, not incidental scheduling luck, is what the positive test is actually verifying |
| **EOI-7** | `approval_state = not_required` is never a default: every such row names the policy row and threshold that produced it. Absent policy ⇒ threshold `0`, `required_approvals` `2` | Schema constraint + a zero-config test asserting approval is required |
| **EOI-8** | `correlation_id` is inherited from the root and never re-minted along a lineage | Test comparing root and leaf |
| **EOI-9** | The EOI check is never inside the `AssetAuthorization → RG → Risk` chain and never alters its order or outcome; `internal/rg`, `internal/risk`, `internal/kyc`, `internal/assetregistry` never import `internal/economicop` | Import inspection (the technique doc 02 uses for the risk/rg separation) + code review of the call ordering |
| **EOI-10** | `economic_operations` carries `tenant_id NOT NULL`, `FORCE ROW LEVEL SECURITY`, the `app.player_account_id IS NULL` conjunct on its staff-scope policy, and **no player-facing or affiliate-facing read policy at all** | Schema inspection + cross-tenant/cross-player/cross-principal-class RLS tests (the Stage 4H-B0-R6 F2 gap shape) |
| **EOI-11** | No floating-point money: `intended_aggregate_value` is integer minor units in `NUMERIC(38,0)`-compatible form against the registry exponent, never `int64` | Type inspection + a multi-exponent test (0/2/6/8/18) |
| **EOI-12** | `internal/economicop` contains no scheduler, no retry policy, no workflow/saga state machine, no HTTP surface, and no eligibility/RG/Risk/KYC concept | Repo inspection + grep |
| **EOI-13** | Every EOI mint, approval, consumption, rejection and status change writes an `audit.Record` in the same transaction (actor, tenant, entity, before/after, reason code) | Count-matching test (`CLAUDE.md`; ADR 0013) |
| **EOI-14** *(new, RK-W15P2-2)* | `recipient_ceiling` consumption is `COUNT(DISTINCT subject_ref)` over the subtree's append-only rows and is monotone non-decreasing; a `lineage_kind = compensation` row never decreases it, even though the same row DOES net against `remaining_value_budget` | Test: grant, clawback, re-grant same subject ⇒ `recipient_ceiling` consumption reads 1 throughout (not 0 after clawback, not 2 after re-grant), while `remaining_value_budget` correctly returns to its pre-grant level after the clawback |
| **EOI-15** *(new, RK-W15P2-4)* | Every `operation_type` declares, exhaustively, the child-row table(s)/column(s) that constitute its consumption record; a child row bearing a `parent_operation_id` whose shape is not named in that declaration causes the consumption function to **raise**, never to under-count silently | Test mirroring `internal/risk/cumulative.go`'s `ErrUnrecognizedCumulativeLeg` pattern: introduce an undeclared second consumption-row shape for one `operation_type` and assert the enforcing query fails closed rather than returning a smaller-than-true sum |
| **EOI-16** *(new, RK-W15P2-5)* | An `operation_type` whose value is not known at execution time consumes the Offer's declared maximum against `remaining_value_budget` at that execution, never zero or a placeholder; a null `asset_code` EOI has no enforceable value budget at all | Test: a cashback-shaped `issued` execution with value unknown consumes the declared ceiling, not 0; a separate test asserts an attempt to enforce a value budget against a null-`asset_code` EOI is rejected as a configuration error, not silently treated as unlimited or zero |
| **EOI-17** *(new, DEP-EOI-4)* | `operation_type` is never derived from, aliased to, or mapped to `internal/risk.Operation`; the two enums' value sets are disjoint and no function converts one into the other | The vocabulary-disjointness test (§3.1) |

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
| **DEP-EOI-4** | **RESOLVED this revision.** `risk` independently confirmed `operation_type` must stay a fully separate, non-derived vocabulary from ADR 0031's `Operation` enum — never reused, never aliased. §3.1 now states this as a binding requirement (no derivation, ever; a vocabulary-disjointness test), closing this item. No further owner action required unless a future reviewer disagrees | risk (confirmed) | No — closed |
| **DEP-EOI-5** | **(new)** `security`'s `CRM-BR-1`/§W15.1.2 (pin the **materialized** subject set; never re-resolve at execution) and `bonus-engine`'s doc 10 **W5** (resolve **live** at run time) point in opposite directions. §2.2's `subject_set_hash` adopts the ceiling reconciliation — the pin bounds from above, live resolution may only shrink — but **neither owner has confirmed it**. Same item as doc 31 **DEP-CRM-7** | security + bonus-engine | Yes, before the first pinned-audience execution |
| **DEP-EOI-6** *(new, RK-W15P2-4)* | The consumption-record-shape declaration (§3.4) must be authored per `operation_type` by the domain that owns that type's tables — `bonus-engine` for `bonus_bulk_grant`/`bonus_manual_grant`/`bonus_campaign_activation`/`bonus_held_disposition_resolution`, `crm` for `crm_engagement_campaign_activation` (which enforces nothing itself but should still declare its own shape for symmetry), `affiliate` for its two types. Not yet authored by any of them — this document specifies the *requirement and failure behavior*, not the per-type declarations themselves | bonus-engine, affiliate, crm | Yes, before implementation of any given `operation_type`'s enforcement |
| **DEP-EOI-7** *(new, RK-W15P2-3)* | §5.3's canonical lock ordering (Risk's advisory lock always before the EOI row lock) is a call-site discipline, not something the EOI schema alone enforces. Whoever implements the Bonus/Affiliate call sites that invoke both Risk and `internal/economicop` in the same transaction must follow §5.3's ordering; a code-review checklist item, not a database constraint, since no database mechanism can order two applications' independently-acquired advisory/row locks | bonus-engine, affiliate (implementers) + code-reviewer (gate) | Yes, before implementation |
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
- The self-defending undeclared-consumption-shape pattern it copies (RK-W15P2-4): `internal/risk/cumulative.go` (`ErrUnrecognizedCumulativeLeg`, `cumulativeSpec`)
- The value-unknown-at-execution-time rule it copies (RK-W15P2-5): ADR 0031 §42(c) (`ErrMissingAmount`, the conservative-maximum resolution)
- The canonical lock ordering it fixes relative to (RK-W15P2-3): `internal/risk/evaluator.go` (`Rule.breach()`'s `pg_advisory_xact_lock`, `evaluator.go:338`)
- The negative-control test pattern EOI-6 now requires: `internal/risk/cumulative_race_integration_test.go`
- The fail-closed default it mirrors: `internal/withdrawal/policy.go` (`defaultApprovalPolicy`)
- Gate order it must not join: `10-bonus-engine-architecture.md` §T.1; `29-bonus-implementation-contract.md` §8 (BI-7); `33-cross-domain-commercial-flow-map.md` §3.1 item 2
- Evidence/reconstruction rules it follows: `30-segmentation-engine-architecture.md` §7.1 (EDR-R1/EDR-R2)
- Flow map and dependency graph: `33-cross-domain-commercial-flow-map.md`
- Unmade human decisions (none selected here): `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`

# ADR 0036 — Retail Agent-Hierarchy: Authorization, Hierarchy-Scoped RLS, Reporting Permissions and Audit

Status: **`NOT IMPLEMENTED`.** Stage 4H-B0 architecture/scope freeze —
security design only. No code, no migration, no schema, no endpoint, no
permission constant, and no role is created by this document. Every
mechanism below is a specification a future authorized stage builds
against, exactly as ADR 0017 (staff MFA/step-up) and ADR 0031 §14–§18
(Bonus/Gamification risk contract) are specifications rather than
implementations.

Owner: `security` (authorization model, RLS strategy, audit requirements,
non-bypass guarantees). Depends on `architect` for the hierarchy data
model (`docs/architecture/26-retail-operations-architecture.md`, which
landed while this document was being drafted and against which every
assumption here has since been verified — see §13), on `data-analytics` for the reporting/BI architecture that
consumes §5's permission primitives, on `identity-compliance` for the
actual RG/KYC rule design §8 refuses to invent, and on `ledger-finance`
for every money-movement concern §8.1 routes back to the existing
wallet/ledger/withdrawal path.

---

## 0. What this document is, and what it is not — `ARCHITECTURAL DECISION`

### 0.1 In scope

1. The authorization model for a configurable N-level agent hierarchy, and
   precisely how it composes with the existing permission-based RBAC
   (`internal/auth/permission.go`) and tenant-scoped RLS
   (`internal/db.Pool.WithTenant`).
2. The row-level-security strategy for hierarchy-scoped tables.
3. The permission primitives for hierarchy-scoped reporting (the
   permission model only — not report contents, not the BI/warehouse
   architecture).
4. Four-eyes/approval controls for retail administrative actions.
5. Audit requirements for retail financial and administrative actions,
   and whether `internal/audit` survives them without a redesign.
6. Confirmation, at the authorization layer specifically, that no retail
   permission or role can be used to circumvent RG self-exclusion or KYC
   gating.
7. The authorization and isolation tests every domain specialist's `qa`
   coverage must include for this subsystem (§10).

### 0.2 Explicitly NOT in scope of this document

- **The hierarchy data model itself** — how nodes and parent/child
  relationships are stored is `architect`'s
  (`docs/architecture/26-retail-operations-architecture.md`). §3 states a
  hard requirement (REQ-H1) that model must satisfy for the RLS strategy
  to work. That dependency was carried as `BLOCKED` while doc 26 did not
  exist and is now **resolved** (§3.1, §13 A4).
- **Commission/settlement arithmetic, float/credit accounting, cash
  reconciliation** — `ledger-finance`. This document says only *who may
  invoke* those operations and *what must be audited*, never how the money
  is computed or posted.
- **RG/KYC rule design for retail** — `identity-compliance`. §8 covers the
  authorization layer's non-bypass property only.
- **Reporting/BI architecture, report contents, warehouse topology** —
  `data-analytics`. §5 defines the permission primitives their document
  consumes and the scope contract they must reproduce; it does not design
  a single report.
- **Terminal/POS hardware, cash-drawer integration, receipt printing** —
  out of scope entirely. §4.4 treats device binding conceptually and
  refuses to specify hardware.
- **Penetration testing or certification-grade audit.** This is
  design-level review appropriate to a development-stage platform. Nothing
  here constitutes a statement that a future implementation is secure;
  it states what an implementation must do to be reviewable at all.

### 0.3 Limits of the review this document represents

This is a *design* review of a subsystem that does not exist. It cannot
and does not certify:

- any future implementation's conformance to it (that is a separate,
  mandatory `security` review of the actual diff — retail touches auth,
  sessions, RBAC, PII and money, so §"Review responsibility" of the
  security specialist's charter applies to every retail change without
  exception);
- the correctness of `architect`'s hierarchy ERD, which had not been
  written when this was authored;
- the regulatory acceptability of an agent-hierarchy retail model in any
  jurisdiction — that is a legal/licensing question for the human
  (§12.6), and a retail network is in several target markets a *licensed
  activity in its own right*, not merely a software feature.

---

## 1. Threat model for the retail surface — `ARCHITECTURAL DECISION`

Stated first because every decision below is traceable to one of these.
The retail surface differs from every existing platform surface in three
ways that matter to security:

**T1 — The adversary is often an authorized insider.** Unlike the
brand frontend (adversary = anonymous internet) or the back office
(adversary = a small, vetted staff population), a retail network has
hundreds-to-thousands of low-trust, high-churn, commercially-motivated
principals (agents, sub-agents, cashiers) holding valid credentials.
"Valid login" is the *starting* position of the attacker, not the end
state. Every control below assumes the credential is legitimate and the
actor is hostile.

**T2 — Lateral movement within a tenant is the primary risk, not
cross-tenant.** The existing platform's isolation story is overwhelmingly
about tenant A vs tenant B. Retail introduces a second isolation
dimension *inside* one tenant: Partner P1's subtree vs Partner P2's
subtree. These are commercial competitors on the same operator. A
cross-subtree read of player lists, volumes, or commission terms is a
real commercial harm even though it is intra-tenant and therefore
invisible to every existing RLS policy in this codebase.

**T3 — Cash at the counter is an AML/RG bypass magnet.** A cashier who
can create accounts, take cash, and pay cash is, structurally, the
highest-risk actor on the platform. The classic abuses are: registering
accounts on other people's identity documents (farming/smurfing), paying
out to a self-excluded player, structuring cash transactions below
thresholds, and "adjusting" a player's balance in exchange for cash off
the books.

Derived attacker goals this document must make structurally impossible or
at minimum detectable-and-audited:

| # | Goal | Primary control |
|---|---|---|
| A1 | Read a sibling/other Partner's subtree data | §3 hierarchy RLS, fail-closed |
| A2 | Move a node under oneself to acquire its subtree | §6 `retail_node:reassign` separated + four-eyes + audit |
| A3 | Self-promote by editing one's own node or an ancestor | §2.6 strict-descendant write rule (`depth > 0`) |
| A4 | Mint a higher-privileged principal | §2.7 retail roles never hold `PermStaffManage`; restricted creation path |
| A5 | Forge or widen authorization scope via a request parameter | §2.3 scope is server-resolved from a DB binding, never client-supplied; §3.2 GUC set only by trusted server code |
| A6 | Bypass RG/KYC by transacting at the counter | §8 — retail has no money path of its own |
| A7 | Read the tenant-wide audit log from an agent console | §7.3 — `PermAuditRead` never granted to retail; audit RLS must be narrowed |
| A8 | Exfiltrate player PII through "reports" | §5.4 separate export permission, PII-bearing projections separately gated |
| A9 | Keep operating after suspension until token expiry | §2.4 authorization re-resolves per request; §4.5 session revocation on suspension |
| A10 | Approve one's own administrative action | §6.3 requester≠approver, person-dedup, approver outside affected subtree |

---

## 2. Authorization model for a configurable hierarchy — `ARCHITECTURAL DECISION`

### 2.1 The problem with each naive answer

The existing model is two orthogonal axes: **capability** (`Permission`,
bundled into a `Role`) and **tenant** (`app.tenant_id` GUC + RLS). A
configurable N-level hierarchy is a *third* axis and cannot be folded
into either of the first two:

- **"Give each hierarchy level its own `Role`"** (`RoleSuperAgent`,
  `RoleAgent`, …) is wrong twice over. First, the directive requires
  configurable depth and structure per tenant/licence/jurisdiction —
  a fixed role ladder in Go code is precisely the "brand/tenant
  difference became a code path" that CLAUDE.md's multi-tenancy rule
  forbids; a tenant with six tiers or with differently-named tiers would
  require a code change. Second and more fundamentally, **a role can
  never express the actual isolation boundary.** Two sibling Agents hold
  the *identical* role; the thing that separates them is *which node they
  are*, not *what kind of node they are*. A role-based check is
  structurally incapable of answering "may this Agent see that Agent's
  players," which is threat T2, the main risk.
- **"Check node relationships in application code"** (the
  `FOR UPDATE`-guarded, application-enforced pattern used elsewhere in
  this codebase) is rejected as the *authoritative* mechanism for the
  same reason ADR 0016 rejected leaving `sessions` isolation to
  `ListActiveSessions`' `WHERE` clause: CLAUDE.md requires isolation
  "enforced by PostgreSQL row-level security… not by discipline in
  application code." An application-only subtree filter is one forgotten
  `WHERE` clause away from a full-tenant data leak, and retail will have
  many more query sites than `sessions` ever had.

### 2.2 Decision: three orthogonal axes, all server-side, RLS-authoritative

Authorization for any retail request is the **conjunction** of three
independently-enforced axes. None may substitute for another.

| Axis | Question | Mechanism | New? |
|---|---|---|---|
| Capability | May this principal perform this *kind* of action at all? | `auth.Permission` + `RequirePermission` | No — unchanged |
| Tenant | Whose tenant's data? | `app.tenant_id` GUC + existing RLS | No — unchanged |
| **Hierarchy scope** | **Which subtree of nodes?** | **New `app.hierarchy_node_id` GUC + closure-predicate RLS** | **Yes** |

This is deliberately the same shape as the existing player-scope axis
(`app.player_account_id`, ADR 0019) and principal-scope axis
(`app.principal_id`, ADR 0016): a scalar GUC, set only by a dedicated
`internal/db` helper, from a value trusted server code has already
resolved, consumed by RLS policies that fail closed when it is unset.
**No new authorization primitive, idiom, or vocabulary is introduced** —
this is a third instance of a pattern this schema already uses twice.

### 2.3 Where a principal's scope comes from — reconciled with doc 26 §5.1

`architect`'s `docs/architecture/26-retail-operations-architecture.md`
§2.1/§5.1 (entity 7) models the human↔node relationship as
`hierarchy_node_staff_assignments`: **N:M with effective dating**, because
one cashier legitimately works two shops this week and a third next week,
and because putting a `node_id` column on `staff_users` would mutate an
Identity-owned, prior-approved table (their Conflict C). `security`
accepts that data model — the operational reality is real and the
table-ownership reasoning is right.

An earlier draft of this section required **exactly one active binding per
principal**, which contradicts it. That requirement is **withdrawn as a
data-model constraint and re-stated as a session/request constraint**,
which is where it actually belongs:

> **`ARCHITECTURAL DECISION` — one scope per session, never a union.** A
> principal may hold N concurrent active assignments. A *session* acts
> under **exactly one** of them at a time, selected explicitly and
> recorded, and every authorization decision and audit record names that
> one. The set of a principal's assignments is never unioned into an
> effective scope.

Why the distinction matters, and why the union is the dangerous option:
a union of two subtrees is (a) not expressible in a scalar GUC without
reintroducing §3.4's application-supplied node set, (b) invisible in
review — nothing in a request shows that the caller was acting across two
branches, and (c) unattributable in audit, which is the one thing a
retail network most needs (whose shop did this action belong to?).

**How the one scope is selected**, fail-closed in both cases:

- **Counter/terminal sessions** — selected *by the terminal*, not by the
  human. Doc 26 §2.2 binds each registered terminal to exactly one node;
  the session's scope is that terminal's node, **validated against the
  cashier's own active assignments** (the cashier must hold an active
  assignment that is ancestor-or-self of the terminal's node). A cashier
  with three assignments who logs into Shop 2's terminal acts at Shop 2,
  and cannot act at Shop 1 or 3 from it. This falls out of the
  two-principal model for free and needs no extra UI.
- **Console sessions (agent/partner back-office, no terminal)** — selected
  explicitly at login when the principal holds more than one active
  assignment. The choice is recorded on the session and on every audit
  record (`actor_node_id`, §7.1). A principal with exactly one active
  assignment skips the choice; a principal with zero active assignments
  gets **no retail scope at all** and can do nothing retail-related, which
  is doc 26 §2.1's own "a cashier with `retail_cashier` and no active
  assignment can do nothing", restated at the scope layer.

**Effective dating is an authorization input, not metadata.** An
assignment outside its `effective_from`/`effective_to` window is not an
assignment. The resolution in §2.4 evaluates the window at request time
using `clock_timestamp()`, not `now()` — the exact defect
`internal/rg.EvaluateEligibility` had to fix (a transaction-stable `now()`
silently ignoring a row that had already become effective while the
transaction waited on a lock). Retail will have the same shape of race
whenever an assignment is granted or revoked while a request is in
flight, and it must not re-learn it.

`assignment.principal_id` and `node_id` are written only by an authorized
`retail_binding:manage` action (§2.7) and read only by the server. Neither
is ever accepted from a client.

### 2.4 Scope resolution — `ARCHITECTURAL DECISION`, with one deliberate rejection

For every retail-scoped request, the server resolves a single
`scope_node_id`, in this exact order, failing closed:

1. If the request carries a **terminal principal** (doc 26 §2.2), the
   candidate scope is that terminal's bound node, and the cashier
   principal must hold an active assignment that is **ancestor-or-self**
   of it (§2.3). Fail → deny.
2. Else, if the principal has exactly one **active, in-window assignment**,
   the scope is that assignment's `node_id`. If the principal has more
   than one, the scope is the one **explicitly selected** for this session
   (§2.3) and must be one of their own active assignments — a selection
   that does not match is a denial, never a silent fallback to the first.
3. Else, if the principal's role is in an explicit **allowlist** of
   tenant-level roles (`RoleTenantAdmin`, the proposed
   `retail_network_manager`, and `RoleCompliance` for investigation —
   §4.6), the scope is **the root node of one explicitly selected
   `hierarchy_network`** (see the multi-network note below), recorded on
   the audit record as `scope_source: "network_root"` so "someone acted
   with whole-network visibility" is never invisible.
4. Else **deny** (403). There is no fourth branch, and in particular there
   is no "no assignment means no narrowing."

**Multi-network correction** (doc 26 §5.1 entity 4: *a tenant may run
several networks*, e.g. one per licence/jurisdiction/brand). An earlier
draft of this section assumed one tree per tenant and resolved
tenant-level roles to "the tenant's root node." That is wrong against
doc 26, and the fix matters because the tempting repair is the dangerous
one:

- **Rejected:** a "tenant-wide" branch in which the scope GUC is left
  unset and the RLS policy falls back to a plain tenant match. That
  reintroduces exactly the fail-open shape §3.3 exists to prevent —
  forgetting to set the scope would silently equal full-tenant
  visibility.
- **Chosen `ARCHITECTURAL DECISION`:** a session acts under **one
  network's root at a time**, selected explicitly and audited, exactly
  like the multi-assignment case in §2.3. A genuine cross-network
  back-office view is **composed by the application from N scoped
  queries** (one per network the caller is entitled to), never by a
  bypass branch. The honest cost: a tenant-wide retail dashboard over N
  networks issues N scoped queries instead of one. That is a real cost
  and it is accepted deliberately — N is the number of networks a single
  tenant operates (small, operator-configured), not a per-row or
  per-player factor, and the alternative is a policy branch whose failure
  mode is silent whole-tenant disclosure.

Then, before the scope is usable:

5. The scope node must itself be `active` and within its effective date
   range, **and every ancestor of it must be active.** `ARCHITECTURAL
   DECISION`, and a non-obvious one: suspending a Partner must suspend
   everything beneath it. A denormalized `effective_status` column would
   go stale on every reparent; the check must be computed from the
   reachability relation at authorization time:

   ```sql
   NOT EXISTS (
     SELECT 1 FROM hierarchy_node_closure c
     JOIN hierarchy_nodes n ON n.id = c.ancestor_node_id
     WHERE c.descendant_node_id = :scope_node_id
       AND c.tenant_id = :tenant_id
       AND (n.status <> 'active'
            OR clock_timestamp() <  n.effective_from
            OR (n.effective_to IS NOT NULL AND clock_timestamp() >= n.effective_to))
   )
   ```

   Without this, "suspend the Partner" is a control that suspends exactly
   one login and leaves its forty cashiers trading (threat A9).

   **Wave-2 review correction (F8, P1)**: this check runs during scope
   *resolution*, before `app.hierarchy_node_id` is established — so a
   plain per-request connection either has no node scope set yet, or (if
   run after `WithNodeScope`) is scoped to the candidate node itself, not
   its ancestors. Either way, §3.3's `closure_scope` policy — which only
   permits rows where `ancestor_node_id` equals the *caller's own* resolved
   scope — makes this query's `hierarchy_node_closure` read return zero
   rows regardless of the real data, so `NOT EXISTS` is always true and
   the check silently passes every time (fail-open, not fail-closed). The
   binding fix: this specific check runs through a dedicated
   `SECURITY DEFINER` accessor (e.g. `retail.node_and_ancestors_active(tenant_id,
   node_id) RETURNS boolean`) that reads `hierarchy_node_closure`/
   `hierarchy_nodes` without going through `closure_scope` at all,
   accepts the candidate node id as an explicit parameter (never session
   state), and is itself audited/reviewed as a scoped RLS-bypass
   primitive — exactly like the internal service-identity pattern ADR
   0014 already establishes for other privileged reads. `closure_scope`
   itself is **not** widened to permit this — doing so would reopen
   §3.3 Class 3's separate "no ancestor visibility" default this
   document already chose. Test §10.11 must assert this check fails
   closed against a real suspended ancestor, not just that the query
   runs.

**Deliberate rejection: the scope node id is NOT a JWT claim.**
`ARCHITECTURAL DECISION`. ADR 0011's claim set (`tenant_id`, `role`,
`principal_type`) is **unchanged by this document** — no `retail_node_id`
claim is added. Reasons:

- A binding revocation, a node suspension, or a reparent must take effect
  on the **next request**, not at access-token expiry. A claim-carried
  scope is authoritative for the token's whole lifetime, which converts
  every suspension into an advisory one.
- A claim is a value the server later reads back and trusts. Re-resolving
  from the database inside the same transaction as the action removes the
  question entirely, at the cost of one indexed lookup per request — a
  cost this codebase already pays for tenant/player resolution.
- It keeps the token shape stable, so ADR 0011/0017's claim architecture
  needs no amendment and no re-review.

A `retail_node_id` claim MAY later be added as an **observability hint
only** (log correlation), if and only if it is documented as
non-authoritative and no authorization path reads it. Given how reliably
"non-authoritative" fields become authoritative under deadline, the
recommendation is simply not to add it.

### 2.5 Request-path composition (illustrative, not a route table)

```
RequireAuth                      (existing)
  → RequireTenantScope           (existing, ADR 0011 — retail is never platform-scoped)
  → RequirePermission(PermRetailX)  (existing mechanism, new constants §2.7)
  → RequireRetailScope           (NEW: resolves §2.4, rejects 403 on failure,
                                  puts retail.ScopeContext{NodeID, NodeType,
                                  ScopeSource} in the request context)
  → handler
       → db.Pool.WithNodeScope(ctx, tenantID, scopeNodeID, fn)   (NEW helper)
            → all reads/writes inside fn are subtree-bounded by RLS (§3)
```

`RequireRetailScope` is the **first line of defence and the source of a
clean 403**; RLS (§3) is the **authoritative** boundary. This is exactly
the two-layer split ADR 0024 uses for withdrawal governance (Go-level
`ApproverEligibility` closure in front of an authoritative database
trigger), applied to a different invariant. Neither layer alone is the
design.

#### 2.5a Transaction phasing for a money-touching retail operation — binding, corrects an internal contradiction found by Wave-2 review (F1, P0)

An earlier draft of §2.5 said `WithNodeScope` wraps the **whole** handler
body, so "all reads/writes inside `fn` are subtree-bounded by RLS." §3.5
point 3 separately requires that the actual ledger posting (and the RG/
Risk/float-sufficiency reads inside the same DB transaction, per ADR 0035
§3.2) run "under a tenant/service scope with no agent principal" — i.e.
with `app.hierarchy_node_id` **unset**. Both cannot be true of the same
connection for the same statements: with the GUC set for the whole
transaction, the un-guarded posting-path policies on `ledger_entries`/
`wallet_balance_projection`/`ledger_accounts` (§3.5 point 3's "exactly as
today" tenant/service scope) don't apply, and adding the §3.5 point 4
guard to those same policies makes them invisible to a node-scoped
connection — the retail posting silently updates zero projection rows,
exactly the P1-drift-by-construction failure ADR 0035 §1.3 warned about
and this section exists to prevent. Symmetrically, the dual-scope
configuration reads (§3.3 Class 2 — node type/relation/capability
definitions) require the GUC **unset** to see a platform-wide template
row, so a capability check made while node-scoped would itself fail
closed.

**Binding resolution**: a money-touching retail operation is one database
transaction with **two phases on the connection's session state**, not
one:

1. **Authorization phase** — `app.hierarchy_node_id` is **set**
   (`WithNodeScope`). Reads only: resolve the caller's scope, read
   `hierarchy_node_types`/`_relations`/`_capabilities` (as platform-wide
   templates, which is why capability resolution must complete **before**
   the GUC is set, or via a dedicated accessor that runs unscoped —
   `architect` to confirm the exact call order in implementation), check
   node status and the ancestor-suspension predicate (§2.4 step 5, see
   also §3.3 Class 3's fix below), and — for a read-only report or an
   inquiry endpoint — stop here; this phase alone is sufficient and uses
   the §3.5 additional SELECT-only policy to read node-scoped ledger
   projections.
2. **Posting phase** — `app.hierarchy_node_id` is **cleared** (`SET
   LOCAL app.hierarchy_node_id TO DEFAULT`, or an equivalent
   session-reset primitive `internal/db` must expose and this document
   requires be added, reviewed as its own change since "clear an
   authorization GUC mid-transaction" is itself an escalation-adjacent
   primitive) before RG (`rg.EvaluateEligibility`), `risk.Evaluate`, the
   `SELECT ... FOR UPDATE` float-sufficiency read, and the ledger posting
   itself run — all under the **same tenant/service scope every other
   domain's posting path already uses** (ADR 0019's actor matrix), so the
   existing, unguarded posting policies apply unchanged and §3.5 point 3
   holds exactly as written.
3. **Audit phase** — the audit insert (§7.3) requires `actor_node_id`
   bound to the writer's own scope, so it is written **either** back
   under the authorization-phase's node scope (GUC re-set after the
   posting phase completes, same transaction) **or** via a
   `SECURITY DEFINER`-style accessor that accepts the resolved node id as
   an explicit parameter rather than reading it from session state — the
   second option avoids a second GUC flip per request and is
   `RECOMMENDATION`ed, left to implementation to choose.

Consequence for §3.5 point 4: the `app.hierarchy_node_id IS NULL` guard
applies **only** to the existing staff-scope read policies that a
node-scoped connection could otherwise also satisfy (the failure mode
§3.5 point 4 itself describes) — it must **not** be added to the
posting-path write policies the posting engine uses during phase 2,
because phase 2 never carries the GUC in the first place under this
phasing and adding the guard there would be a no-op at best and a source
of confusion at worst. This is a correction to §3.5 point 4's wording,
not to its intent.

A `node_id` supplied in a path, query or body is **never** the scope. It
is accepted only as a **narrowing filter within the already-resolved
scope**, and a value outside the caller's subtree returns **404, not
403** — this codebase's established "never confirm the existence of a
resource outside the caller's own authorization" rule (ADR 0031 §8's
disable-rule precedent, ADR 0029 §4a's KYC precedent). A 403 here would
turn the API into an org-chart enumeration oracle for a competitor
Partner (threat T2).

### 2.6 Read scope vs. write scope — `ARCHITECTURAL DECISION`

They are not the same set, and conflating them is a self-promotion
vulnerability (threat A3):

- **Read scope** = the scope node *and* all descendants (closure rows
  with `depth >= 0`). An agent sees themselves and everything under them.
- **Administrative write scope** = **strict descendants only** (closure
  rows with `depth > 0`). An actor may never administratively modify
  their own node (commission terms, credit limit, status, parent) or any
  ancestor. Changes to one's own node come from strictly above, through
  §6's approval flow.
- **Operational write scope** (recording a counter transaction at one's
  own node) is `depth >= 0` — a cashier obviously transacts at their own
  node. These are different permissions (§2.7) precisely so the two write
  scopes never collapse into one policy.

This imposes a concrete requirement on `architect`'s model: **the
reachability relation must carry depth** (or an equivalent way to
distinguish self from strict descendant), because `depth > 0` is a
load-bearing authorization predicate, not a reporting convenience.

### 2.7 Permission and role proposal — `RECOMMENDATION`, `NOT IMPLEMENTED`

Naming follows the existing convention exactly
(`<resource>:<verb>` and `<resource>_config:read`/`:manage`,
per `risk_config:read`/`risk_config:manage` and doc 25 §2). **No
`Permission` constant is added to code by this document.**

| Proposed permission | Gates | Separation rationale |
|---|---|---|
| `retail_node:read` | View nodes in own subtree | Baseline |
| `retail_node:manage` | Create/edit **strict descendant** nodes | Does **not** include reparent or suspend |
| `retail_node:reassign` | Change a node's parent | **Its own permission, never bundled.** Reparenting is *the* privilege-escalation primitive of a hierarchy (threat A2): attaching another Partner's subtree under yourself grants instant read access to its entire data set. Same reasoning that split `tournament:settle` from `tournament_config:manage` (doc 25 §1.6) and `withdrawal_policy:write` from `withdrawal:approve` (ADR 0024) |
| `retail_node:suspend` | Suspend/reinstate a descendant node | Separated so a support-shaped role can stop a suspected-compromised agent without being able to create or reparent anything |
| `retail_binding:manage` | Bind/unbind a principal to a **strict descendant** node | This is the privilege-**granting** primitive. Step-up gated (§4.3) |
| `retail_config:read` / `retail_config:manage` | The tenant's hierarchy template: node types, permitted depth, permitted child types per type | Tenant-level only. **Never granted to any in-tree actor** — an agent that can edit the template can grant its own tier new capability |
| `retail_policy:write` | Four-eyes thresholds and approval policy for retail actions (§6) | Must be **disjoint** from `retail_approval:decide` and from `retail_node:*` — exact `PermWithdrawalPolicyWrite` precedent (raise threshold → act alone → lower it back) |
| `retail_approval:decide` | Approve/reject a pending retail administrative action | Dedicated, `RoleFinance`-shaped: one role, one narrow authority |
| `retail_cash:record` | Record a counter cash-in at own node | Money-adjacent; audited, RG/Risk-gated (§8) |
| `retail_payout:execute` | Execute a counter payout at own node | Money-adjacent; step-up above threshold (§4.3) |
| `retail_float:transfer` | Move float/credit between nodes in own subtree | `ledger-finance` owns the accounting; this gates the action |
| `retail_report:read` | Operational reports, own subtree (§5) | |
| `retail_commission:read` | Commission/margin figures, own subtree (§5.3) | Separated from `retail_report:read`: an Agent seeing their Super Agent's margin is a commercial leak, not a permissions detail |
| `retail_report:export` | Bulk export/download (§5.4) | Separated from read: bulk export is a distinct exfiltration and data-protection event |
| `retail_audit:read` | **Subtree-scoped** audit read (§7.3) | Never `PermAuditRead`, which is tenant-wide (threat A7) |
| `retail_player:read` | Player detail for players attributed to own subtree | Never `PermPlayerRead`, which is tenant-wide |

Proposed roles — **capability profiles, deliberately position-independent**:

- **`retail_cashier`** — `retail_cash:record`, `retail_payout:execute`,
  `retail_report:read` (own node), `retail_player:read` (limited
  projection, §8.4).
- **`retail_agent_admin`** — `retail_node:read`, `retail_node:manage`,
  `retail_binding:manage`, `retail_report:read`,
  `retail_commission:read`, `retail_audit:read`, `retail_player:read`.
  **`retail_audit:read` cannot be usefully granted until §7.3's finding
  (C1: the existing `audit_log` RLS policy does not yet narrow by
  subtree) is resolved** — granting it today would give
  `retail_agent_admin` tenant-wide audit visibility, not subtree-scoped,
  defeating the permission's own purpose. Flagged here so implementation
  does not grant it before §7.3's migration lands (Wave-2 review, F14).
- **`retail_network_manager`** (tenant back-office, root-scoped) — the
  above plus `retail_node:reassign`, `retail_node:suspend`,
  `retail_config:read`/`retail_config:manage`.
- **`retail_approver`** — `retail_approval:decide` **and nothing else**,
  mirroring `RoleFinance`'s and `RoleRiskManager`'s "one role, one narrow
  authority" shape.
- `RoleTenantAdmin` gains `retail_config:read` and `retail_policy:write`
  only — never `retail_approval:decide`, never `retail_binding:manage`,
  never `retail_node:reassign`. This is the exact split ADR 0024 landed
  on for withdrawals (policy authority with tenant_admin, decision
  authority with a dedicated role) and it is reused rather than
  re-litigated.

**The key structural insight that makes configurable depth work without
N roles:** a Partner, a Super Agent and an Agent hold the *same*
capability profile (`retail_agent_admin`). What differs between them is
**their position in the tree**, which the hierarchy-scope axis already
expresses, and **their commercial configuration** (commission rate,
credit limit, which child types they may create), which is *configuration
rows* per CLAUDE.md's "brand differences are configuration rows, never
code paths." A tenant that wants "Agents may not create sub-agents" sets
`may_create_child_types = {}` for that node type in the hierarchy
template — server-validated against the platform-defined closed set —
rather than needing a new role. **Tenants may never mint new
permissions**: that would make the authorization language itself
tenant-authored, which is a whole new threat surface (a tenant-defined
permission that maps to nothing, or to too much) and is explicitly
deferred to a future partner-console custom-role feature (§12.8).

**Bundling hazard, recorded as a build-time requirement** (the same
finding doc 25 §2 recorded for its seven `manage`-class permissions): a
permission with no named grantee gets bundled into `RoleTenantAdmin` at
build time because that is the path of least resistance. Every permission
above has a named grantee in this section. Any implementation that adds a
retail permission without naming its grantee is non-conformant to this
ADR.

**Anti-escalation rule (threat A4), `ARCHITECTURAL DECISION`:** no retail
role ever holds `PermStaffManage`. Creating a retail principal is
`retail_binding:manage` plus a **restricted** staff-creation path that can
mint only `retail_cashier`/`retail_agent_admin` accounts bound to a
**strict descendant** node — mirroring exactly the
`newCreateStaffHandler` restriction that Stage 3D used to close the
"tenant_admin mints a finance account and approves through it" path (ADR
0024 residual findings). Without this rule, `retail_agent_admin` becomes
a full privilege-escalation primitive on day one.

### 2.8 Composition with doc 26's `hierarchy_node_capabilities` — `ARCHITECTURAL DECISION`

Doc 26 §1.5/§5.1 (entity 3) introduces a **second** authorization-shaped
layer this document must reconcile with, or the two will be implemented
as alternatives by whoever builds them: `hierarchy_node_capabilities`,
dual-scope configuration declaring what a *node type* may do
(`register_player`, `counter_deposit`, `counter_payout`, `fund_child`,
`settle_with_parent`, `earn_commission`), optionally narrowed by
`jurisdiction_code`.

They are not alternatives and neither subsumes the other. They answer
different questions:

| Layer | Question | Owner | Authority |
|---|---|---|---|
| `auth.Permission` (§2.7) | May **this principal** do this kind of thing? | `security` | Platform-defined, closed set, in code |
| `hierarchy_node_capabilities` | Is this kind of thing **available at this node type**, in this jurisdiction? | `architect` / tenant configuration | Tenant-configurable, from a platform-defined closed set of capability codes |

**The composition rule, stated so it cannot be implemented as an `OR`:**

> **A retail operation is authorized only if the principal holds the
> permission AND the node's type carries the capability AND the
> hierarchy scope covers the node. Capability configuration may only ever
> NARROW what a permission allows; it may never GRANT anything a
> permission does not already allow.**

Three consequences that must survive implementation:

1. **A capability is not a permission.** Adding a capability row must
   never make an action possible for a principal lacking the permission.
   If a tenant could grant capability to a node type and thereby enable an
   action, tenants would be authoring the authorization language — the
   thing §2.7 rules out.
2. **Capability codes are a platform-defined closed set**, validated
   server-side on write, exactly as doc 26 §1.5 implies by enumerating
   them. A free-text capability column would be a tenant-authored
   permission by another name.
3. **The jurisdiction narrowing is a one-way ratchet.** A
   jurisdiction-scoped capability row that removes `counter_payout` in
   jurisdiction X must not be overridable by a tenant-scoped row that adds
   it back — this is the same HARD_LIMIT-vs-CONFIGURABLE_LIMIT distinction
   ADR 0031 §5 already draws for risk rules, and retail needs it for the
   same reason (a legal constraint must not be beatable by a more specific
   commercial row). Doc 26 §H6 specifies most-specific-wins precedence for
   `hierarchy_node_type_relations`; applying that ordering *unmodified* to
   a jurisdiction-derived capability restriction would let tenant+specific
   beat a legal constraint. **Flagged to `architect` as a genuine gap
   in H6** (§9, C16), not resolved unilaterally here.

---

## 3. RLS strategy for hierarchy-scoped tables

### 3.1 Can PostgreSQL RLS express "see your subtree"? — `ARCHITECTURAL DECISION`

Yes — but only if the ancestor→descendant reachability relation is
queryable as **a single indexed predicate from inside a policy**, without
the application supplying the node set. Three candidate mechanisms:

| Mechanism | In-policy predicate | Verdict |
|---|---|---|
| **(a) Recursive CTE in the policy** | `node_id IN (WITH RECURSIVE …)` | **Rejected as the primary mechanism.** A policy expression is inlined into *every* statement against the table; a recursive walk then runs per statement (and, depending on plan shape, effectively per row) on tables that will carry the platform's highest-volume retail traffic. It is also un-indexable and its cost grows with subtree size — a root-scoped back-office query would traverse the whole network on every read. Correct, but a latency and lock-footprint hazard exactly where the system is busiest |
| **(b) Closure table read in the policy** | `EXISTS (SELECT 1 FROM hierarchy_node_closure c WHERE c.tenant_id = t.tenant_id AND c.ancestor_node_id = <scope GUC> AND c.descendant_node_id = t.node_id)` | **RECOMMENDED.** One indexed lookup per row against a composite index; plan is stable; the predicate is a plain `EXISTS` a reviewer can read at a glance |
| **(c) Materialized path (`ltree`)** | `t.path <@ <scope path>` | **Acceptable with a caveat.** GiST-indexable and fast, but the scope's *path* is not a value the caller's identity directly yields — it must be looked up from `hierarchy_nodes` (whose own RLS then applies, creating a regress) or passed in a second GUC by the application, which weakens the "database is authoritative" property back toward "application asserts its own scope". Usable, but strictly weaker than (b) |

**Recommendation: (b), a closure table, with a single scalar GUC
`app.hierarchy_node_id`.**

**Dependency status: RESOLVED, not assumed.** This recommendation was
written as a `BLOCKED` dependency on `architect`'s ERD choice while
`docs/architecture/26-retail-operations-architecture.md` did not yet
exist. That document has since landed and §1.3 chooses exactly this
shape, independently: **an authoritative adjacency list
(`hierarchy_nodes.parent_node_id`) with `hierarchy_node_closure`
(`ancestor_node_id`, `descendant_node_id`, `depth`, including depth-0
self rows) maintained as a derived projection in the same transaction as
any structural change**, path enumeration explicitly rejected, with
invariants H1–H5 (adjacency authoritative; closure written in the same
transaction; closure recomputed and diffed on a schedule; cycles
prevented by a database constraint; a cross-tenant/cross-network parent
edge a constraint violation). The dependency is therefore **satisfied**,
and the requirement below is recorded as the standing security constraint
on any future change to that model rather than as an open question:

> **REQ-H1.** The ancestor→descendant reachability relation, including
> depth (§2.6), must be expressible as a single non-recursive, indexable
> predicate evaluable from inside a PostgreSQL RLS policy, using only
> (i) a single scalar session variable set by trusted server code and
> (ii) columns on the protected row. Any model that requires the
> application to compute and supply the set of reachable node ids does
> not satisfy REQ-H1. **Doc 26 §1.3 satisfies REQ-H1.**

Two `security` observations on doc 26 §1.3's shape, which it did not
draw out and which matter here because the closure table is
authorization data (§3.3):

- **Its H3 (recompute-and-diff on a schedule) is a detection control, not
  a prevention control, and its severity classification must reflect
  what closure drift actually is.** Doc 26 correctly says a wrong closure
  row is "an authorization error, not merely a reporting error" — so
  non-zero closure drift is not merely a P1 the way balance drift is; it
  is a **live access-control incident**, potentially meaning either a
  subtree that has been readable by the wrong ancestor since the last
  diff, or an operator locked out. The runbook for closure drift must
  differ from the runbook for balance drift: revoke/re-scope first, then
  reconcile.
- **H1 ("no business logic reads the closure to decide structure; it
  reads it only to answer containment questions fast") needs one explicit
  carve-out**: RLS policies *do* read the closure to make authorization
  decisions (§3.3), which is a containment question but is emphatically
  not a "fast path optimisation" that could be skipped under load or
  replaced by an adjacency walk. The closure is on the critical
  authorization path, and H2's same-transaction maintenance is what makes
  that safe.

### 3.2 The GUC and its helper — `ARCHITECTURAL DECISION`

`app.hierarchy_node_id`, set per-transaction via `set_config(…, true)`,
exclusively by two new `internal/db` functions mirroring the existing
ones exactly:

- `Pool.WithNodeScope(ctx, tenantID, scopeNodeID, fn)` — sets
  `app.tenant_id` **and** `app.hierarchy_node_id`; the only sanctioned
  way to run a hierarchy-scoped query. Mirrors `WithPlayerScope`.
- `SetHierarchyNodeIDForCurrentTx(ctx, tx, nodeID)` — sets the GUC
  inside an already-open transaction, for the case where a retail action
  must be atomic with something already running under `WithTenant`.
  Mirrors `SetPrincipalIDForCurrentTx` / `SetSessionInternalOpID`, and
  carries the identical doc-comment warning: **it grants no authorization
  of its own**; the caller must already have established entitlement to
  that scope via §2.4.

No other code path may set this GUC. The value always comes from §2.4's
server-side resolution — never from a path, query, or body parameter
(threat A5). This is the same sentence `WithTenant` and `WithPlayerScope`
already carry, and it is load-bearing in the same way.

### 3.3 Policy shapes — `RECOMMENDATION` (conceptual SQL, no migration)

**Class 1 — hierarchy-scoped tenant-owned tables** (retail nodes, node
configuration, bindings, counter transactions, commission records,
retail-attributed player links):

```sql
-- Read
CREATE POLICY retail_subtree_read ON <table>
  FOR SELECT
  USING (
    tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    AND EXISTS (
      SELECT 1 FROM hierarchy_node_closure c
      WHERE c.tenant_id = <table>.tenant_id
        AND c.ancestor_node_id   = NULLIF(current_setting('app.hierarchy_node_id', true), '')::uuid
        AND c.descendant_node_id = <table>.node_id
    )
  );

-- Administrative write: strict descendants only (§2.6)
CREATE POLICY retail_subtree_admin_write ON <table>
  FOR ALL
  USING      (<same, plus c.depth > 0>)
  WITH CHECK (<same, plus c.depth > 0>);
```

**Fail-closed by construction** — the single most important property
here. When `app.hierarchy_node_id` is unset, `NULLIF(...)::uuid` is
`NULL`, `c.ancestor_node_id = NULL` evaluates to `NULL`, `EXISTS` is false,
and the row is invisible. There is **no** `OR <guc> IS NULL` escape
branch, and there must never be one: a policy of the form
"unset means no narrowing" makes *forgetting to set the scope* equal to
*full tenant visibility*, which is precisely the failure mode ADR 0016
had to retire from `sessions` (`FOR SELECT USING (true)`) and ADR 0031's
own P1 had on `risk_rules`. **Tenant-wide access is not a bypass branch;
it is simply the root node's subtree** (§2.4 step 3), which flows through
the identical predicate.

Note the `app.player_account_id IS NULL` conjunct is present **from day
one**, not added later after a review finds it missing. Its absence was
an independently-found P1 on `risk_rules` (ADR 0031 "Specialist review
findings"), and the guard costs nothing.

**Class 2 — dual-scope retail configuration** (platform-supplied default
hierarchy templates alongside tenant overrides, mirroring `risk_rules`'
and `PointType`'s nullable-`tenant_id` shape): ADR 0013's dual-scope
expression, unchanged, **plus** both the player and retail-scope guards,
because a template is tenant/platform-level configuration and must never
be readable or writable from an in-tree retail connection:

```sql
USING (
  (
    (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
  )
  AND NULLIF(current_setting('app.player_account_id',    true), '') IS NULL
  AND NULLIF(current_setting('app.hierarchy_node_id', true), '') IS NULL
)
```

**Class 3 — `hierarchy_nodes` themselves are Class 1** (a node is visible to
its ancestors, not to its siblings), with one addition: a node must also
be able to see its own ancestry *identifiers* for breadcrumb/reporting
purposes only if that is an explicit product requirement — by default it
must not, since ancestor metadata (other Partners' names, terms) is
exactly the T2 leak. Default: **no ancestor visibility.** `OPEN DECISION`
if a product requirement contradicts this.

**`hierarchy_node_closure` is itself authorization data** — `ARCHITECTURAL
DECISION`, and the subtlest point in this section. Two consequences:

1. **Its own RLS must be non-recursive**, or the Class 1 policies that
   subquery it deadlock logically (a closure read inside a policy is
   subject to closure's own policy). The self-consistent shape is:

   ```sql
   CREATE POLICY closure_scope ON hierarchy_node_closure
     FOR SELECT
     USING (
       tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
       AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
       AND ancestor_node_id = NULLIF(current_setting('app.hierarchy_node_id', true), '')::uuid
     );
   ```

   This grants a scope holder visibility of exactly their own descendant
   list — which is precisely what they are entitled to know — and nothing
   about siblings or ancestors. It is self-contained (no regress) and
   fail-closed when the GUC is unset.

2. **Anyone who can write a closure row can grant themselves scope.**
   Direct DML on this table is therefore a privilege-escalation
   primitive, and the write path must be closed at the database, not by
   convention: closure rows are maintained **exclusively** by a trigger on
   `hierarchy_nodes` (insert / parent change / status change), and a guard
   trigger on `hierarchy_node_closure` rejects any DML not originating from
   it (e.g. keyed on a transaction-local flag the maintenance function
   sets via `set_config(..., true)` and nothing else sets). This is the
   same philosophy as `audit_log`'s immutability trigger in ADR 0013:
   enforced regardless of which role runs the query, including the
   application role that owns the table, because `REVOKE` does not bind a
   table owner.

**Performance/complexity trade-offs, stated honestly:**

- Reads: one index probe per row against
  `hierarchy_node_closure (tenant_id, ancestor_node_id, descendant_node_id)`. Cheap and
  plan-stable. A second index on `(tenant_id, descendant_node_id)` supports the
  ancestor-status check in §2.4.
- Storage: closure is O(nodes × average depth). For a network of 10⁴
  nodes at depth ≤ 6, that is ~10⁵ rows — trivial.
- **Writes are where the cost lands.** A reparent rewrites the closure
  rows for the entire moved subtree (O(subtree × depth)). This is
  acceptable because reparenting is a rare, four-eyes-gated administrative
  action (§6), not a hot path — but it **must be transactional with the
  node write**, because a stale closure row is not a cache miss, it is an
  authorization bug (either a stale grant or a stale denial).
- **The real complexity cost is correctness of closure maintenance**, not
  query performance. That is why §10 requires an adversarial test that a
  reparent atomically revokes the old ancestor's access and grants the new
  one's, and a test that the maintenance trigger is the only writer.

### 3.4 Rejected: an application-computed node set in the GUC

Passing the resolved descendant ids as a delimited GUC string and using
`node_id = ANY(string_to_array(current_setting(...), ',')::uuid[])` was
considered and **rejected** as the primary mechanism:

- It breaks down for large subtrees — a root-scoped back-office session
  would set a multi-hundred-kilobyte GUC on every transaction.
- More importantly it makes the **application** the authority on the node
  set, with the database merely honouring whatever list it is handed.
  That is the "isolation by discipline in application code" CLAUDE.md's
  multi-tenancy rule exists to prevent, wearing an RLS costume. A bug in
  the set computation is a silent cross-subtree leak that no RLS test can
  catch, because RLS would be doing exactly what it was told.

It remains acceptable as an *additional narrowing filter* layered on top
of the closure predicate (e.g. a UI filter), never as the boundary.

### 3.5 Node-scoped ledger accounts — `security`'s answer to ADR 0035 §1.3

`docs/decisions/0035-retail-agent-network-accounting.md` §1.3 (authored in
parallel by `ledger-finance`) raises a node-scoped owner family on
`ledger_accounts` (`agent_float` per node) and explicitly defers the RLS
question to `security` + `architect`, naming two candidate answers: a new
subtree-aware GUC policy, or a staff-RBAC-only read path with subtree
filtering in the handler. This section answers it.

**Decision: reuse `app.hierarchy_node_id` (§3.2). Do not mint a second
hierarchy GUC, and do not put the subtree filter in the handler.**

- There is exactly **one** hierarchy scope concept on this platform and
  it gets exactly **one** session variable — `app.hierarchy_node_id`, the
  name `architect` proposes in doc 26 §8 Conflict A and this document
  adopts. A second, separately-named GUC for “the ledger's node scope”
  would create two session variables that must always agree with nothing
  enforcing that they do, and a divergence between them is a silent
  authorization gap rather than an error.
- **Handler-side subtree filtering is rejected** for the reason §2.1
  already gives: it is the "isolation by discipline in application code"
  CLAUDE.md's multi-tenancy rule exists to prevent, and here it would be
  applied to *balances*, where the failure mode is one agent spending
  another agent's float.

Concretely, on whichever columns ADR 0035's option (b) lands
(`ledger_accounts.hierarchy_node_id`, denormalized onto `ledger_entries`
and `wallet_balance_projection` by the same `BEFORE INSERT` trigger
pattern ADR 0019 already uses):

```sql
-- ADDITIONAL permissive SELECT policy, OR'd beside the existing ones
CREATE POLICY ledger_retail_subtree_read ON <table>
  FOR SELECT
  USING (
    tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    AND <table>.hierarchy_node_id IS NOT NULL
    AND EXISTS (
      SELECT 1 FROM hierarchy_node_closure c
      WHERE c.tenant_id     = <table>.tenant_id
        AND c.ancestor_node_id   = NULLIF(current_setting('app.hierarchy_node_id', true), '')::uuid
        AND c.descendant_node_id = <table>.hierarchy_node_id
    )
  );
```

Four properties this shape must have:

1. **It is an ADDITIONAL permissive policy, never a conjunct added to an
   existing one.** This is `ledger-finance`'s own binding requirement in
   ADR 0035 §1.3, and it is correct: the posting path runs under a
   tenant/service scope with no agent principal, so ANDing an agent-scope
   term into `wallet_balance_projection`'s existing `SELECT` policy would
   make every retail posting update zero projection rows and manufacture
   P1 drift by construction. It is also the Postgres behaviour ADR 0016
   documented — an `UPDATE` requires the row to be visible under *some*
   `SELECT` policy.
2. **`hierarchy_node_id IS NOT NULL` is load-bearing.** Without it, a
   retail-scoped connection could reach house-level (`wallet_id IS NULL`,
   `hierarchy_node_id IS NULL`) accounts whenever the closure `EXISTS`
   happened to be satisfiable — the "platform-wide row visible to a
   narrower scope" error ADR 0013's dual-scope discussion warns about.
3. **Read-only.** This policy grants `SELECT` only. A retail principal
   never writes `ledger_entries`, `ledger_transactions` or
   `wallet_balance_projection` directly under any scope — postings are
   made by the posting engine under a tenant/service scope, exactly as
   today. A retail-scoped `INSERT`/`UPDATE` policy on any ledger table
   would be a P0 and must not be created.
4. **The existing staff-scope *read* policies (the ones a node-scoped
   connection could otherwise also satisfy during the authorization
   phase, §2.5a) must additionally carry the `app.hierarchy_node_id IS
   NULL` guard**, for the same reason §3.3's Class 2 shape does and §7.3
   gives for `audit_log`: permissive policies OR together, so a narrow
   policy placed beside a broad one narrows nothing unless the broad one
   is guarded. **This guard is never added to the posting-engine's own
   write path** (§2.5a phase 2 runs with the GUC unset by construction,
   so the guard would be a no-op there) — Wave-2 review correction (F1).

This remains an `OPEN DECISION` in the sense ADR 0035 means it — the
`ledger_accounts` owner-family change (its option (b)) is `architect`'s
and `ledger-finance`'s to ratify jointly. This section commits only to
the access-control half, conditional on that shape being adopted.

---

## 4. Cashier as an actor — `ARCHITECTURAL DECISION`

### 4.1 Decision: a cashier is a `staff_users` row plus an active node assignment

Not a new actor type. Not a player. Concretely: `staff_users` (existing
table, existing dual-scope RLS, existing Argon2id password handling,
Postgres-backed lockout, rotating refresh tokens with reuse detection) +
a `hierarchy_node_staff_assignments` row (§2.3) + a retail role (§2.7). The
"hybrid" in the question is real but narrow: **staff for authentication
and audit identity, hierarchy-bound for authorization scope.**

Why not a player: a cashier operates the platform *on behalf of the
operator*; giving them a `player_accounts` row would put them under
player RLS (`WithPlayerScope` grants self-access to financial rows —
entirely the wrong shape), would make RG/self-exclusion semantics apply
to an operator employee (meaningless), and would blur `audit.ActorPlayer`
vs `ActorStaff` in exactly the records regulators read.

Why not a new actor type: a third principal type means a parallel
authentication stack — login, lockout, session issuance, refresh
rotation, reuse-chain revocation, MFA. That machinery took ADR 0016 and
ADR 0018 to get right once. Two implementations of it will diverge, and
the second one is always the weaker one. `audit.ActorType`,
`auth.PrincipalType`, and `sessions.principal_type` all stay as they are.

**Required additive schema change (flagged, not a redesign):**
`staff_users.role`'s `CHECK` constraint must be widened to accept the new
retail roles. Precedent: migration `0041` widened the identical
constraint additively for `risk_manager`. The companion `CHECK`
(`platform_admin ⟺ tenant_id IS NULL`) is already satisfied — every
retail role is tenant-scoped, never platform-scoped.

### 4.2 Session and token model

- Cashiers use the **existing** brand-independent staff session model.
  No new token type. The session/game-token separation of doc 05 is
  untouched — a cashier never receives a game launch token.
- **Shorter access-token TTL and shift-bounded sessions** are a
  `RECOMMENDATION`: a POS in a shop is a shared, physically-exposed
  device; a session that survives the end of a shift is a credential
  lying on a counter.
- Authorization re-resolves the binding per request (§2.4), so revocation
  is immediate for *authorization* purposes even before session
  revocation lands. Both are still required (§4.5).

### 4.3 MFA and step-up (ADR 0017) — `RECOMMENDATION` + `OPEN DECISION`

ADR 0017 models MFA as a per-account/per-role policy and step-up as a
per-operation freshness requirement. Applied to retail:

- **Agent-admin and above must have MFA**, and `retail_node:reassign`,
  `retail_binding:manage`, `retail_config:manage` and
  `retail_policy:write` must be `RequireStepUp`-gated. These are
  authorization-changing operations; they belong in ADR 0017's
  step-up-gated list alongside "granting a permission," which it already
  names.
- **`retail_payout:execute` above a configurable threshold must be
  step-up gated**, matching ADR 0017's own "withdrawal approval" and
  "manual financial adjustment" candidates. The threshold is business
  policy (§12).
- **Whether an ordinary cashier must hold a second factor at all is an
  `OPEN DECISION` for the human/compliance function**, not an engineering
  call: a shared POS with no personal device makes TOTP awkward, and
  designing around an unstated operational reality produces a control
  nobody uses. What `security` does assert, and will block on: **if a
  cashier is exempted from step-up for high-value actions, the exemption
  must be paid for with a compensating control — a second human approver
  at the node (four-eyes at the counter), a hard per-shift payout ceiling,
  or terminal binding (§4.4) — never with nothing.** "Cashiers are
  exempt" with no compensating control is a finding that blocks
  completion of the retail payout capability.

### 4.4 The terminal as a second principal — `security`'s answer to doc 26 §2.2

Doc 26 §2.2 goes further than an earlier draft of this section did, and
it is right to: **a money-touching retail operation requires TWO
authenticated principals presented together — the terminal (a
`service`-type principal bound at registration to exactly one node) and
the cashier (a `staff` principal).** `security` adopts that decision
without modification. It is the correct analogue of doc 05's
"two different token types, never conflated" invariant: the terminal
token answers *which registered device, at which node, in which tenant*;
the cashier token answers *which accountable human*; neither alone
authorizes anything, and every audit record carries both (§7.1).

An earlier draft of this section treated terminal binding as an optional,
per-tenant-configurable nicety with IP allowlisting as a cheaper
substitute. **That is withdrawn for money-touching operations**: with
two-principal authentication as the architecture, a counter deposit or
payout from an unregistered device must simply fail. IP/CIDR allowlisting
remains a useful *additional* control (and a cheap one), never a
substitute.

Doc 26 §2.2 explicitly assigns the terminal credential mechanism to
`security`. Answering it, at design level:

**`RECOMMENDATION` — a long-lived registration secret exchanged for
short-lived terminal access tokens, not a long-lived bearer token, and
not mTLS in the first slice.**

- **Rejected: a long-lived bearer token stored on the terminal.** It is a
  standing credential on a physically exposed device in a shop, with no
  rotation story and no way to detect theft short of its use.
- **Rejected for the first slice: mTLS client certificates.** Genuinely
  stronger (the credential can be non-exportable), but it pulls in
  certificate issuance, distribution, renewal and revocation (CRL/OCSP)
  infrastructure that does not exist anywhere in this platform today, and
  it does not compose with the existing `RequirePermission`/
  `RequireTenantScope` middleware without new plumbing. Recorded as the
  right target state for a high-value or high-risk deployment, not as the
  starting point.
- **Chosen shape**: registration mints a per-terminal secret, shown once,
  **stored only as an Argon2id hash** (`retail_terminals` holds a
  credential *reference*, never a secret value — doc 26 §5.1 entity 8
  already says this and it is exactly right). The terminal exchanges it
  for a short-lived, tenant-scoped, node-bound access token through the
  same issuance path service identities already use (ADR 0014 option 2),
  subject to the same Postgres-backed lockout as password login. This
  reuses `internal/auth` wholesale and introduces no new credential
  primitive.

Non-negotiable properties, whichever mechanism is ultimately chosen:

1. **One credential per terminal, never shared.** A shared credential
   makes "which terminal" unanswerable and makes revocation an outage for
   every terminal at once.
2. **Individually revocable**, with revocation taking effect on the next
   request — not at token expiry (the §2.4 re-resolution rule applies to
   the terminal principal too: its node binding and status are read from
   the database per request, never trusted from the token).
3. **Rotatable without re-registering the device**, or it will never be
   rotated in practice.
4. **The node binding is server-side data, never a client-supplied
   field** — doc 26 §2.2 already states this; it is restated because it
   is the single property that makes the terminal principal a control
   rather than a label.
5. **The secret never appears in an audit record, a log line, or an error
   message** (`audit.Entry.Metadata`'s standing prohibition on
   authentication material).
6. **A terminal principal holds no permissions of its own beyond
   "participate in a counter operation at its node."** It must never be
   able to act without a cashier session — otherwise a stolen terminal is
   a standing, unattributable counter credential, and the two-principal
   design has bought nothing.

`OPEN DECISION` remaining for the human/operator (§12): whether terminals
are tenant-managed or platform-managed, and the physical/operational
process for registering and decommissioning one (a decommissioned
terminal whose credential is never revoked is the most likely real-world
failure of this control, and that is a process problem, not a code one).

### 4.5 Suspension must terminate sessions

Suspending a node or revoking a binding must, in the same transaction,
revoke the active sessions of every affected principal
(`auth.RevokeAllSessionsForPrincipal` already exists and is used by the
password-reset flow). Authorization already fails on the next request via
§2.4, so this is defence in depth rather than the primary control — but
leaving a suspended agent's refresh-token chain alive means a suspension
that is reversed by a later mistaken reinstatement silently restores
sessions that should have been destroyed.

### 4.6 Which existing roles get a root scope

Only `RoleTenantAdmin`, the proposed `retail_network_manager`, and
`RoleCompliance` (investigation) resolve to the network root (§2.4 step 3),
and every such resolution is audited with `scope_source: "network_root"`.
`RolePlatformAdmin` is deliberately **excluded**: it holds a nil-tenant
token (ADR 0011), `RequireTenantScope` denies it before any retail
handler runs, and there is no code path by which it could resolve a
specific tenant's root node — granting it retail permissions would be the
"capability nothing can actually use" that `permission.go`'s own
`RolePlatformAdmin` comment warns against.

---

## 5. Reporting permission model — foundation only — `ARCHITECTURAL DECISION`

This section defines **permission primitives and a scope contract** for
`data-analytics` to consume. It designs no report.

### 5.1 The primitives

- **`retail_report:read`** — operational reporting (activity volume,
  transaction counts, cash position), **scoped to the caller's own
  subtree**, resolved per §2.4. Never parameterised by a client-supplied
  scope.
- **`retail_commission:read`** — commission, margin and settlement
  figures, same subtree scope. Separate from `retail_report:read` because
  the inter-tier commercial relationship is exactly the data a subordinate
  tier should not see about its superiors, and because a Partner's margin
  is competitively sensitive against sibling Partners (threat T2).
- **`retail_report:export`** — bulk export/download of any of the above.
  Separate from read because an export is a distinct exfiltration event
  and, where the rows carry personal data, a distinct data-protection
  event. Every export writes an audit record including the filter set and
  the row count (§7.4).
- **`retail_player:read`** — per-player detail for players attributed to
  the caller's subtree. Distinct from the existing tenant-wide
  `PermPlayerRead`, which no retail role holds.

### 5.2 The scope contract `data-analytics` must consume

> **REQ-R1.** Every retail reporting query — in the transactional
> database, in any read replica, and in any warehouse/BI layer — takes an
> authoritative, server-resolved `(tenant_id, scope_node_id)` pair and
> applies the same ancestor→descendant reachability predicate as §3.3.
> The pair is never derived from a client-supplied parameter; a
> client-supplied `node_id` is a narrowing filter validated against that
> scope, and an out-of-scope value returns 404 (§2.5).

> **REQ-R2.** No retail report may be computed on a connection that
> bypasses the scope predicate (a `WithoutTenant`, `BYPASSRLS`, or
> warehouse-service-account path) and then filtered in application or BI
> code. Aggregation is not a bypass: an aggregate over a subtree must be
> computed under the same scope as row-level access to that subtree. If
> the BI layer cannot reproduce the reachability predicate, it must not
> serve hierarchy-scoped reports at all.

> **REQ-R3.** Any warehouse/replica carrying retail data must carry the
> reachability relation (closure or equivalent) alongside it, and must
> reflect reparents and suspensions with a stated, bounded lag. An agent
> who was reparented away yesterday and still sees the old subtree in
> "yesterday's report" today is an authorization defect, not a freshness
> quirk. `data-analytics` owns choosing and documenting that lag bound.

### 5.3 PII boundary — `ARCHITECTURAL DECISION`

`retail_report:read` must not become a side channel around player-data
permissions. This is the same finding doc 25 §1.1 recorded for
`bonus:read` vs `verification:read`, applied here:

- The default retail report projection is **aggregate or pseudonymous** —
  counts, volumes, totals per node. It carries no player identity, no
  contact data, no document data, and **no KYC or RG-derived status**
  under any circumstances.
- Per-player detail requires `retail_player:read` *in addition*, remains
  subtree-bounded, and still excludes KYC/RG state (§8.4).
- A report must never expose a withheld/excluded player's *reason* — the
  same two-projection rule doc 25 §1.7 applied to leaderboards.

### 5.4 Known residual risk for `data-analytics` — P2

**Small-cell inference.** A node with one cashier or one player makes
"aggregate" reporting effectively player-level, and a sibling-invisible
aggregate at a parent can still leak a child's individual activity when
the child has a single player. Standard mitigations (minimum cell size
suppression, rounding) are `data-analytics`' to choose. Recorded here so
it is not discovered after the first regulator asks.

### 5.5 `security`'s answer to doc 26 Conflict B — do NOT put a node dimension on `player_accounts`

Doc 26 §8 Conflict B asks `identity-compliance` + `security` whether
`player_accounts`' RLS should gain a node dimension so a network manager
can read "players registered in my subtree." `architect`'s position is
no; **`security` agrees, and adds the condition that makes the
alternative safe.**

Agreed reasoning:

1. Adding a second permissive policy to `player_accounts` is the
   OR-widening hazard (a node-scoped connection would satisfy both the new
   narrow policy and, unless every existing policy is guarded, the broad
   tenant one) on the single most security-sensitive table in the identity
   domain. The guard would have to be added to `player_accounts`' existing
   policies — a change to Identity-owned, prior-approved, already-reviewed
   RLS, for a retail feature.
2. An online-only tenant's `player_accounts` RLS stays byte-identical,
   which means retail introduces zero regression risk for the B2C MVP
   brand that does not use it.

**The condition `security` attaches**, without which the alternative is
worse rather than better: joining from the node-scoped
`retail_player_origins` to a tenant-scoped `player_accounts` means the
subtree restriction is enforced **only on the retail side of the join**.
A malformed join predicate, a `LEFT JOIN` where an `INNER JOIN` was
meant, or a later "and also show unattributed players" tweak silently
returns tenant-wide player rows to an agent. Therefore:

> Every retail-scoped read of player data goes through **one
> purpose-built, audited accessor** that encapsulates the join — never a
> raw `SELECT ... FROM player_accounts` in a retail handler. This is
> exactly the control ADR 0015 already imposed for the analogous
> `persons` case ("if a future feature genuinely needs a tenant-scoped
> read of a person's data, it must go through a purpose-built, audited
> accessor… reviewed by `security` and `identity-compliance` together").
> The same review pairing applies here.

Plus the projection rule from §5.3: that accessor returns a **retail
projection** of a player — never the full row, never KYC/RG state, never
document metadata (§8.4).

---

## 6. Four-eyes and approval controls for retail administration — `ARCHITECTURAL DECISION`

### 6.1 Which actions

Above a per-tenant configurable threshold where an amount exists, and
unconditionally where the action is authorization-changing:

| Action | Four-eyes | Why |
|---|---|---|
| Create a node | Threshold/config-driven | Moderate risk; volume argues against unconditional |
| **Reassign a node's parent** | **Always** | The escalation primitive (threat A2). An entire subtree's data visibility and commercial attribution moves in one write |
| Suspend / reinstate a node | Reinstate: always. Suspend: no | Asymmetric on purpose: suspension is a safety action that must never be slowed down; *undoing* one is the abusable direction |
| Delete / close a node | Always | Destroys attribution and can orphan a subtree |
| Grant or revoke a principal binding | Always | The privilege-granting primitive |
| Change credit limit / commission terms | Above threshold | Direct financial effect |
| Manual balance adjustment reachable from retail | Always, per CLAUDE.md | Inherited rule, not a new one |

### 6.2 Mechanism — reuse, do not reinvent

`retail_admin_approvals`, shaped **exactly** like `withdrawal_approvals`
(`docs/architecture/withdrawal-state-machine.md` §5) rather than
inventing a second approval model:

```
retail_admin_approvals
  id                            UUID PK
  tenant_id                     UUID NOT NULL   -- denormalized; RLS needs it on the protected row
  retail_admin_request_id       UUID NOT NULL
  approver_principal_id         UUID NOT NULL   -- server-derived from the approver's session, never a body field
  decision                      TEXT NOT NULL   -- 'approve' | 'reject'
  reason_code                   TEXT NULL       -- required on 'reject' and on every threshold-crossing action
  threshold_at_decision         NUMERIC(38,0) NOT NULL
  value_at_decision             NUMERIC(38,0) NOT NULL
  decided_at                    TIMESTAMPTZ NOT NULL
  UNIQUE (retail_admin_request_id, approver_principal_id)
  FOREIGN KEY (retail_admin_request_id, tenant_id)
      REFERENCES retail_admin_requests (id, tenant_id)   -- an approval can never cross tenants
```

Append-only, protected by the same `BEFORE UPDATE OR DELETE` +
`BEFORE TRUNCATE` trigger pair `audit_log` uses (ADR 0013) — not by
`REVOKE`, because the application role owns the table.

### 6.3 The bypass paths that must be closed explicitly

`UNIQUE (request, approver)` alone does not deliver four-eyes. Each of
the following is a known bypass in this exact design and must be closed
at the database, mirroring the withdrawal precedent:

1. **Two logins, one human.** Distinct-approver counting dedupes on
   `COALESCE(staff_users.person_id, approver_principal_id)` — ADR 0023 §1's
   already-solved problem, reused verbatim.
2. **Approver must be attributable and active.** Non-automated decisions
   require the approver to resolve to a `staff_users` row with non-NULL
   `person_id` and `status = 'active'` — ADR 0024 §1, reused verbatim,
   enforced by a `BEFORE INSERT` trigger and not only in Go.
3. **NEW, retail-specific: the approver must be outside the affected
   subtree.** An approver whose scope node lies *within* the subtree being
   changed has a direct interest in the outcome. Concretely: reject the
   decision if `EXISTS (closure where ancestor = affected_node AND
   descendant = approver_scope_node)`. This is the retail analogue of
   "the approver is never the beneficiary," and without it an agent and
   their own sub-agent can approve each other's node changes all day.
4. **Threshold mutation is itself a bypass.** `retail_policy:write`
   (`RoleTenantAdmin`) and `retail_approval:decide` (`retail_approver`)
   are held by disjoint roles, so no principal can raise a threshold, act
   alone, and lower it back — the exact `PermWithdrawalPolicyWrite` /
   `PermWithdrawalApprove` split from ADR 0024. `threshold_at_decision`
   and `value_at_decision` record what was actually in force.
5. **Structuring below the threshold.** Splitting one large credit-limit
   increase into N sub-threshold increases must be considered; the hook is
   the same as withdrawals' — threshold evaluation over a rolling
   per-node total, not a single request's delta. `OPEN DECISION` on window
   and total (risk/AML policy, `identity-compliance`).
6. **No TOCTOU window.** The distinct-approver count and the threshold
   comparison run inside the **same transaction** as the state change they
   authorize, reading the request's own current values.
7. **Automated approval is a service identity** (ADR 0014) and can never
   count as one of two human approvals.

---

## 7. Audit requirements — `ARCHITECTURAL DECISION`

### 7.1 Does `internal/audit` survive? — Yes, additively. No package redesign.

The existing `audit.Entry` already carries actor type/id, tenant, action,
target type/id, outcome, IP, user agent, request id and structured
metadata, and `Record(ctx, tx, entry)` already guarantees the audit row
and the action commit or roll back together. Retail needs **one new
dimension**: the hierarchy position of the actor and of the target.

**Decision: add two nullable columns to `audit_log` —
`actor_node_id UUID` and `target_node_id UUID` — plus the two
corresponding optional fields on `audit.Entry`.** Additive migration,
additive struct fields handled exactly like `TenantID`'s existing
nil-to-NULL mapping. `internal/audit`'s API shape, its atomicity contract,
and its immutability triggers are unchanged.

**Why real columns and not `metadata` JSONB** (this was a genuine
choice, not a default): a subtree-scoped audit read (§7.3) needs the node
id as an **indexable, typed predicate inside an RLS policy**. A policy
joining a closure table to a JSONB-extracted, untyped value is fragile,
un-plannable, and silently fails open on a malformed value. This
platform's precedent is unambiguous — every scope key (`tenant_id`,
`principal_id`, `player_account_id`) is a column, never metadata.

### 7.2 What must be audited

**Every** retail administrative action: node created / updated /
reassigned / suspended / reinstated / closed; binding granted / revoked;
hierarchy template changed; retail policy/threshold changed; approval
requested / approved / rejected.

**Every** retail financial action: counter cash-in, counter payout,
float/credit transfer between nodes, commission accrual adjustment, any
manual adjustment reachable from a retail surface.

**Every** authorization denial on a retail surface, with
`Outcome = OutcomeDenied` — `retail.scope_denied`. This is not
bookkeeping: repeated cross-subtree probing is the single best detection
signal for a compromised or hostile agent credential (threat T1), and it
is invisible unless denials are recorded.

**Every** root-scope resolution (§2.4 step 3), with
`scope_source: "network_root"`, so "someone acted with whole-network
visibility" is never silent.

Proposed action namespace, following the existing `domain.event`
convention (`risk.rule_created`, `staff.mfa_enrolled`):
`retail.node_created`, `retail.node_reassigned`, `retail.node_suspended`,
`retail.node_reinstated`, `retail.binding_granted`,
`retail.binding_revoked`, `retail.cash_in_recorded`,
`retail.payout_executed`, `retail.float_transferred`,
`retail.commission_adjusted`, `retail.approval_requested`,
`retail.approval_decided`, `retail.report_exported`,
`retail.scope_denied`, `retail.template_changed`.

**Reason code mandatory** for: manual adjustment, node suspension,
reparent, credit-limit change, and the void/cancel of a counter
transaction. **Audit is not a substitute for the ledger and the ledger is
not a substitute for audit** — a counter cash-in produces both a ledger
posting (`ledger-finance`'s invariant set applies unchanged) and an audit
record; neither is optional because the other exists.

### 7.3 `audit_log`'s current RLS does not survive retail — **P1, flagged to `architect`**

`audit_log` today has exactly one policy: ADR 0013's dual-scope
`FOR ALL`, tenant-wide, with **no** player-scope guard and (obviously) no
hierarchy narrowing. Two consequences:

1. **Any retail principal granted `PermAuditRead` would read the entire
   tenant's audit log** — every other Partner's subtree, every staff
   action, every compliance decision (threat A7). Mitigation at the RBAC
   layer: no retail role ever holds `PermAuditRead`; retail audit reading
   is the separate `retail_audit:read`.
2. **RBAC alone is not sufficient, and adding a second, narrower policy
   does not narrow anything.** PostgreSQL ORs permissive policies
   together: a retail connection is by definition tenant-scoped, so it
   already satisfies the existing broad policy, and a new subtree-scoped
   policy added beside it would widen, not narrow. The narrowing must
   therefore come from a **distinguishing guard inside the existing
   policy**, exactly the `tenant_staff_scope` / `player_self_scope` shape
   ADR 0019 established.

Required change (a **tightening of already-approved ADR 0013
architecture — flagged, not silently made**):

- Amend the existing `dual_scope_isolation` policy's `USING` with
  `AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
  AND NULLIF(current_setting('app.hierarchy_node_id', true), '') IS NULL`
  — which also closes the pre-existing, unrelated missing player guard.
- Add `audit_retail_scope_read FOR SELECT`, requiring tenant match plus
  a closure `EXISTS` on `actor_node_id` **or** `target_node_id`.
- **Do not** apply the retail guard to the INSERT path. A cashier's action
  must write its audit record from inside the retail-scoped transaction
  that performs it (ADR 0013's atomicity contract). Insert must therefore
  remain permitted from retail scope, with `WITH CHECK` requiring
  `actor_node_id` to be **within the writing connection's own scope**, so
  a cashier cannot forge an audit record attributed to another node.

This is a schema change to a Stage-2 table protected by an immutability
trigger. It is additive and policy-level (no data rewrite), but it must
go through `architect` and a fresh `security` review of the actual
migration — it is named here as a required dependency, not performed
here.

### 7.4 Export auditing

`retail.report_exported` records the requesting principal, the scope node,
the filter set, the row count, and the output format. A bulk export of
player-adjacent data is the highest-volume PII egress this subsystem has,
and an export with no record of what left is not auditable after the
fact.

---

## 8. Risk / RG / KYC cannot be bypassed through retail — `ARCHITECTURAL DECISION`

Scope note: the RG/KYC *rules* are `identity-compliance`'s
(`docs/decisions/0026`, `0027`, `0028`, `0034`). This section establishes
only that **the authorization layer cannot be used as a bypass vector** —
which is the question actually asked of `security`.

**Load-bearing premise, stated explicitly (Wave-2 review correction,
F12, P2)**: this section's non-bypass guarantee presupposes every retail
player is an identified `PlayerAccount`. Whether anonymous/bearer-
instrument retail play is required in any target jurisdiction is an open
human/business/legal decision escalated by `risk` (ADR 0031 §22) and
`ledger-finance` (ADR 0035 §11.4) — if answered "yes" for any
jurisdiction, this section's guarantee does not cover that case and needs
its own design.

### 8.1 The structural guarantee: retail is a channel, not a second rulebook

**Retail is a channel, not a second financial truth system.** Counter
cash-in and counter cash-out post into the **one** append-only
double-entry ledger, under the same invariants as every other channel,
and — this is the security-relevant part — they run the **same
enforcement composition, in the same fixed order, inside the same
transaction as the posting, before it commits**:
`rg.EvaluateEligibility` first (short-circuiting), then `risk.Evaluate`,
then balance/sufficiency locking (ADR 0031 §1, ADR 0034 §1).

**Correction against a parallel document**
(`docs/decisions/0035-retail-agent-network-accounting.md`,
`ledger-finance`, authored in parallel with this one): an earlier draft
of this section asserted that a counter cash-in *is* a `deposit` and a
counter payout *is* a `withdrawal`, routed through the existing
`internal/payments`/`internal/withdrawal` packages verbatim. ADR 0035
§3.2/§3.3 shows that is too strong — retail postings have their own
transaction types (`retail_deposit`, `retail_withdrawal_authorization`,
`retail_withdrawal_payout`) and their own account family (`agent_float`),
precisely because retail money never touches an external PSP rail (its
invariant R1) and because the physical authorize-then-hand-over-the-cash
window needs a real hold balance. That is a ledger-modelling decision,
**not** a bypass: ADR 0035 §3.2/§3.3 state the identical
RG → Risk → sufficiency-lock precondition chain this section requires.
The security property is therefore **the enforcement composition and its
position inside the posting transaction**, not the reuse of any
particular Go package, and this section is corrected to say so rather
than to contradict a financial-domain decision that is not `security`'s
to make.

`security`'s binding requirement on any retail posting path, restated
for the avoidance of doubt: **no retail transaction type may exist whose
posting path does not call `rg.EvaluateEligibility` for the affected
player, in the same transaction, before commit.** A retail payout's
Step B (physical hand-over confirmation) discharging an already-gated
Step A authorization is the one acceptable exception shape — the same
reasoning that exempts `postWin`/`postRollback` today (ADR 0026 §11:
settling or reversing an already-legitimate prior action must not be
blocked by a later status change).

**The bypass, if it ever exists, will be a retail-specific money path
that skips that composition** — an "agent credits a player directly"
shortcut, a counter adjustment that posts without the gate, or a
per-node/per-cashier cash limit implemented as a counter inside a retail
package instead of as a `risk_rules` row. Any such design is rejected by
this ADR, and any proposal for one must come back through `security` +
`ledger-finance` + `identity-compliance` together. There is no retail
exception to CLAUDE.md's financial rules.

### 8.2 Why no permission can express "skip RG"

Structurally, not by policy:

- `rg.EvaluateEligibility(ctx, tx, params)` takes **no override
  parameter**, no actor, no role, and no permission. It cannot be told to
  skip. Its inputs are `(TenantID, BrandID, PlayerAccountID, WalletID)` —
  facts about the *player*, never about the caller.
- The check runs **inside the transaction that posts the effect**, before
  the effect commits. RBAC runs **before the handler**, and answers only
  "may this actor attempt this action at all."
- These are therefore two different questions asked at two different
  points, and no value of the first can change the answer to the second.
  **A permission that means "and skip RG" is not expressible in this
  architecture** — which is what "structurally impossible, not merely
  policy-forbidden" means here.

Concrete failure scenario, as asked: *a cashier holding
`retail_payout:execute` attempts a counter payout for a self-excluded
player.* The withdrawal path calls `rg.EvaluateEligibility`, which returns
`CodeSelfExcluded` from the platform-wide, `Person`-attached restriction,
and the transaction aborts. The cashier's permission set never enters
that evaluation. The denial is audited (`OutcomeDenied`, with
`actor_node_id`).

**Standing prohibitions**, which `security` will block on if proposed:
no `retail_rg:override`, no `retail_kyc:waive`, no "supervisor override"
permission, and no retail flag that suppresses an eligibility call.
Nothing may be added to `rg.EvaluateEligibility`'s parameters that
describes the *caller*.

### 8.3 A nuance this ADR will not decide by analogy

Several jurisdictions **require** returning a self-excluded player's
remaining balance. If that is true in a target market, "deny all
withdrawals to self-excluded players" is the wrong policy, and the right
one is a compliance-approved, four-eyes'd, reason-coded, audited
administrative return — **never an ordinary cashier counter operation**,
because "pay out to an excluded player" as a routine cashier capability is
the exact shape of the abuse it would otherwise prevent. Flagged to
`identity-compliance` and the human (§12.4); not resolved here, and
explicitly not assumed either way.

### 8.4 KYC at the counter is stricter, never looser

- **`PermVerificationReview` is never granted to any retail role.** A
  cashier collects documents; a cashier does not *decide* identity. The
  collected evidence enters the existing `kyc_documents` /
  `kyc_verifications` flow and is reviewed by `RoleCompliance`, unchanged.
- **`PermIdentityReviewManage` is never granted to any retail role.**
  Clearing `identity_review_required` is precisely the control that stops
  self-exclusion evasion (ADR 0027 §6/§8); putting it in the hands of the
  actor with the strongest commercial incentive to onboard is the
  single worst grant available in this subsystem.
- A cashier can never set `verified_at`, never alter `PlayerAccountStatus`
  toward a less-restricted value, and never see another player's KYC or RG
  state (§5.3) — a retail read permission must not become an inference
  channel for KYC status, the doc 25 §1.1 finding applied here.
- Cashier-assisted registration is the classic farming/smurfing vector
  (threat T3). Required mitigations: the created player account records
  the creating `actor_node_id`; identity resolution (ADR 0027) applies
  unchanged; and registration volume per node is a natural
  `RISK_SIGNAL`/detection input for a future stage (not built here, and
  not approximated — ADR 0031 §18's "a rule the engine cannot evaluate
  must never be configurable" applies).

### 8.5 Agent "ownership" of a player is not impersonation — `OPEN DECISION` with a warning

A node being *attributed* a player (for commission and reporting) must
never imply the node may **act as** that player. No retail permission may
authorize placing a bet, requesting a withdrawal, changing credentials,
or altering RG settings *on behalf of* a player through the player's own
authorization context.

Retail models in several LATAM markets do expect proxy/assisted play. If
that becomes a requirement it is a **separate, explicitly designed,
`security`+`identity-compliance`-reviewed delegation feature**, never an
implicit consequence of hierarchy scope. The non-negotiable properties if
it is ever built: the **player's** RG/KYC eligibility is evaluated (never
the agent's), the audit record names **both** principals, and the agent
never holds or presents the player's session token (doc 05's token
separation invariant). Recorded as an open decision (§12.5) rather than
assumed absent.

### 8.6 Risk engine integration

Retail operations are exposure-affecting and fall under ADR 0031 §13's
standing rule: they must call `risk.Evaluate` in the same transaction as
their effect. **Wave-2 review correction (F4, P1)**: an earlier draft of
this section endorsed doc 26 §4.3's now-superseded operation names
verbatim; the authoritative set, resolved in ADR 0031 §21 (which owns
naming per §16) and corrected in doc 26 §4.3, is **`retail_deposit`,
`retail_withdrawal`, `retail_funding`** rather than reusing
`deposit`/`withdrawal`, correctly routed through ADR 0031 §16's six-step
extension process and owned by `risk`. `security` supports separate
values for the same reason ADR 0031 §15e gave for `tournament_entry`:
reusing `deposit`/`withdrawal` would silently rebind every existing
online payment rule to counter operations, which is a change to
already-authored rules rather than an addition. Note the consequence,
stated honestly: **until those values and their enforcement call sites
exist, no risk rule can gate a counter operation at all** — cash
structuring in particular needs the `count`/`velocity` limit kinds ADR
0031 §4 deliberately did not implement, so it is not merely unconfigured,
it is currently inexpressible. Note the existing
honest status: `internal/payments` does **not** call `risk.Evaluate`
today (ADR 0031 §13's table, `NOT IMPLEMENTED`). Retail does not change
that and must not be described as covered by it. **Retail must not build
its own limit engine** — including per-cashier or per-node daily cash
limits, which are `risk_rules` scope dimensions or a future `count`/
`velocity` `LimitKind`, not a counter table in a retail package
(ADR 0031 §14, applied to this domain).

---

## 9. Conflicts and dependencies against already-approved architecture — flagged, not changed

| # | Approved artefact | Interaction | Action required |
|---|---|---|---|
| C1 | **ADR 0013** — `audit_log` dual-scope RLS | Tenant-wide policy cannot express subtree-scoped audit reads, and ORing a narrow policy beside it widens rather than narrows | §7.3's policy amendment. **Requires `architect` sign-off and a fresh `security` review of the migration.** Not performed here |
| C2 | **Migration 0011** — `staff_users.role` CHECK | Must be widened additively for retail roles | Additive migration; precedent 0041. No conflict, but it is a Stage-2 table change |
| C3 | **ADR 0011** — token claim set | **No change.** Scope is deliberately not a claim (§2.4) | None — recorded so a future implementer does not "helpfully" add one |
| C4 | **ADR 0019** — `WithPlayerScope` two-policy pattern | Every new retail table must carry the `app.player_account_id IS NULL` guard from day one | §3.3. This is the exact P1 found late on `risk_rules`; it must not recur |
| C5 | **ADR 0016** — GUC-scoping idiom | `app.hierarchy_node_id` is a third instance of the same idiom, not a new mechanism | None. Consistency is the point |
| C6 | **ADR 0024 / withdrawal-state-machine §5** | Retail approvals reuse the four-eyes shape and its seven closed bypass paths | §6. Reuse, not reinvention |
| C7 | **ADR 0031** | Retail must not build a limit engine; retail operations consult `risk.Evaluate` | §8.6 |
| C8 | **ADR 0026 / 0027 / 0028 / 0034** | RG remains sole authority; KYC review stays with Compliance | §8.2, §8.4 |
| C9 | **ADR 0002** — isolation-tightening roadmap (RLS → schema-per-tenant → db-per-tenant) | Hierarchy scope is an **intra-tenant** dimension, so it rides along under every step of that roadmap without redesign | None — verified compatible |
| C10 | **doc 25 §2** — bundling hazard | Same hazard applies to every retail `manage`-class permission | §2.7 names a grantee for every permission; build-time conformance requirement |
| C11 | **ADR 0017** — MFA/step-up | Retail adds new step-up-gated operations and an unresolved cashier-MFA question | §4.3; extends ADR 0017's own open-decision list rather than contradicting it |
| C12 | **ADR 0035 §1.3** — node-scoped `ledger_accounts` owner family (`ledger-finance`, parallel) | That ADR explicitly defers the RLS question for node-scoped ledger accounts to `security` + `architect` | **Answered in §3.5**: reuse `app.hierarchy_node_id`, additional read-only permissive policy, never a handler-side filter, existing staff policies guarded. The `ledger_accounts` owner-family change itself remains `architect` + `ledger-finance`'s to ratify |
| C13 | **ADR 0035 §3.2/§3.3** — retail-specific ledger transaction types | Contradicts an earlier draft of §8.1 that claimed retail reuses `internal/payments`/`internal/withdrawal` verbatim | **§8.1 corrected**, not ADR 0035. The security invariant (RG→Risk inside the posting transaction) is preserved and is what ADR 0035 already specifies |
| C14 | **doc 26 §1.3** — adjacency authoritative + closure projection | Satisfies REQ-H1; §3.1's `BLOCKED` dependency is **resolved** | None, beyond §3.1's two observations (closure drift is an access-control incident, not a reporting one; RLS is a closure consumer on the critical path) |
| C15 | **doc 26 §2.1/§5.1 entity 7** — N:M effective-dated staff assignments | Contradicted this document's earlier "exactly one active binding" rule | **§2.3 corrected**: architect's data model accepted; the one-scope rule moved from the data model to the session/request layer, where it belongs |
| C16 | **doc 26 §H6** — most-specific-wins precedence for hierarchy configuration | Applied unmodified to a **jurisdiction-derived capability restriction**, a tenant-specific row could beat a legal constraint | **Flagged to `architect`, not resolved here** (§2.8). ADR 0031 §5's HARD_LIMIT vs CONFIGURABLE_LIMIT distinction is the precedent that already solves this shape |
| C17 | **doc 26 §8 Conflict B** — node dimension on `player_accounts` | Routed to `security` + `identity-compliance` | **Answered in §5.5**: agree with architect (no node dimension), conditional on a single purpose-built audited accessor per ADR 0015's precedent |
| C18 | **doc 26 §2.2** — terminal as a second principal; credential mechanism assigned to `security` | Stronger than this document's earlier "optional terminal binding" | **§4.4 corrected and answered**: two-principal authentication adopted for money-touching operations; registration-secret-to-short-lived-token recommended, mTLS recorded as target state |

**No conflict is resolved unilaterally by this document.** C1 (amending
`audit_log`'s RLS) is the only item requiring a change to an
already-accepted decision, and it is flagged for `architect` rather than
made. C13, C15 and C18 are places where **this document was corrected to
match a parallel specialist's decision**, not the other way round — in
each case the parallel document was right and an earlier draft here was
wrong; the corrections are marked in place rather than silently applied,
so a reviewer can see what changed and why. C16 is the one place this
document flags a gap in `architect`'s own design without fixing it.

---

## 10. Mandatory authorization and isolation tests — `security`-specified, `qa`-owned

Per the security specialist's testing responsibility: the following are
**required** coverage for any retail implementation. They are stated as
properties, not as test names, and several must be proven by **adversarial
SQL against the database**, not only through HTTP handlers — an
HTTP-level test passes identically whether RLS or an application `WHERE`
clause is doing the work, which is exactly the gap ADR 0016's review
closed for `sessions`.

**Hierarchy isolation**

1. A sibling Agent's data is invisible: request for Agent B's node/players
   /transactions using Agent A's valid token returns 404 (never 403, never
   data).
2. An ancestor CAN read a descendant's data; a descendant CANNOT read an
   ancestor's.
3. Cross-Partner isolation proven by direct SQL under
   `WithNodeScope(tenantA, partner1Node)` against Partner 2's rows —
   zero rows, not filtered rows.
4. **Fail-closed proof**: with `app.hierarchy_node_id` unset, a
   tenant-scoped connection reads **zero** rows from every Class 1 retail
   table. (If this test passes trivially because a policy has an
   `OR guc IS NULL` branch, the implementation is non-conformant.)
5. GUCs do not leak across pooled transactions — mirroring
   `TestSessionRLS_ScopeGUCsDoNotLeakAcrossTransactions`.
6. A player-scoped connection (`WithPlayerScope`) reads and writes zero
   rows on every retail table — the `risk_rules` P1 regression, pre-empted.
7. Cross-tenant: tenant B's valid token with tenant A's node id → 404.

**Structure mutation**

8. Reparent: in one transaction, the old ancestor's access is revoked and
   the new ancestor's is granted; no window exists where both or neither
   can read the moved subtree.
9. A retail principal cannot INSERT/UPDATE/DELETE `hierarchy_node_closure`
   directly (escalation test) — denied by the maintenance guard, not by
   the absence of a Go call site.
10. A node cannot be reparented to a node inside its own subtree (cycle),
    and a cycle in closure is impossible by construction.
11. Suspending a Partner denies every principal in its subtree on the
    **next request**, including principals whose access token is still
    valid.

**Write-scope**

12. An actor cannot modify their own node's status, parent, credit limit
    or commission terms (`depth > 0` rule), via HTTP and via direct SQL.
13. An actor cannot bind a principal to their own node or an ancestor.
14. A retail role cannot create a staff user of any non-retail role, and
    cannot create one bound outside its strict-descendant set.

**Four-eyes**

15. Two logins resolving to the same `Person` cannot satisfy both
    approvals.
16. An approver whose scope node lies inside the affected subtree is
    rejected (at the database trigger, not only in Go).
17. An unlinked (`person_id IS NULL`) or suspended staff account cannot
    approve.
18. A single principal cannot raise a threshold, act, and lower it back
    (permission-disjointness test on the role map itself).

**RG / KYC non-bypass**

19. A cashier with every retail permission cannot complete a payout for a
    self-excluded player; the attempt is audited with `OutcomeDenied`.
20. No retail role holds `PermVerificationReview`, `PermIdentityReviewManage`,
    `PermRGRestrictionWrite`, `PermStaffManage`, `PermAuditRead`, or
    `PermPlayerRead` — asserted as a table-driven test over the role map,
    so a future grant fails the build rather than a review.
21. A retail read endpoint never discloses KYC/RG-derived status to a
    caller lacking the corresponding permission.

**Audit**

22. A retail actor cannot read the tenant-wide audit log; a subtree-scoped
    read returns only rows whose `actor_node_id`/`target_node_id` is in
    scope.
23. A cashier cannot write an audit record attributed to another node
    (`WITH CHECK` proof).
24. Every action in §7.2's list produces exactly one audit record, in the
    same transaction, and the action fails if the audit write fails.

**Reporting**

25. A `node_id` report filter outside the caller's subtree returns 404.
26. `retail_report:read` alone does not permit export; export is audited
    with row count.
27. The retail player-read accessor (§5.5) returns only players attributed
    to the caller's subtree — proven by seeding an unattributed player and
    a player attributed to a sibling subtree, and asserting neither
    appears. This is the test that catches the join-predicate failure mode
    §5.5 names.

**Two-principal and assignment model (added after reconciliation with doc 26)**

28. A counter operation with a valid cashier token and **no** terminal
    principal is denied; with a valid terminal principal and no cashier
    session, denied.
29. A cashier with an active assignment at Shop 1 cannot operate Shop 2's
    terminal, even within the same agent (doc 26 §4.2 check 6).
30. A principal with two active assignments acts under exactly one per
    session; no request ever sees the union of both subtrees, and the
    selected assignment appears on every audit record.
31. An assignment outside its `effective_from`/`effective_to` window grants
    nothing — including the `clock_timestamp()`-vs-`now()` race in §2.3
    (an assignment revoked while a request waits on a lock must not still
    authorize it).
32. A revoked terminal credential stops working on the **next** request,
    not at token expiry; revoking one terminal does not affect any other.
33. A terminal principal alone holds no standing authority: with no
    cashier session, it can perform no operation of any kind.

**Capability layer (§2.8)**

34. Removing a `hierarchy_node_capabilities` row denies the action even
    for a principal holding the permission (capability narrows).
35. Adding a capability row does **not** enable the action for a principal
    lacking the permission (capability never grants) — the test that
    proves the composition is an `AND`, not an `OR`.
36. A capability code outside the platform-defined closed set is rejected
    at write time.

---

## 11. Findings register (security-review framing)

Severities are assigned against the *design as it would be built without
this ADR's controls* — this is a design review, so every finding is
preventive rather than live.

**P0 — must be structurally closed before any retail code is marked
complete**

- **P0-1 `retail_node:reassign` bundled with `retail_node:manage`.**
  Failure scenario: Partner P1's `retail_agent_admin` reparents Partner
  P2's subtree under itself and immediately reads P2's entire player base,
  volumes and commission terms. Control: §2.7 separation + §6 unconditional
  four-eyes + §6.3(3) approver-outside-subtree + audit.
- **P0-2 Fail-open RLS via an "unset scope means no narrowing" branch.**
  Failure scenario: one handler forgets `WithNodeScope`, runs under
  plain `WithTenant`, and returns every node's data to an agent console
  with no error anywhere. Control: §3.3's no-escape-branch policy shape;
  test §10.4.
- **P0-3 Retail roles holding `PermStaffManage`.** Failure scenario: an
  agent mints a `tenant_admin`/`finance` account and escalates out of the
  hierarchy entirely — the Stage 3C escalation path, repeated. Control:
  §2.7 anti-escalation rule + restricted creation path; test §10.14.
- **P0-4 A retail-specific money path.** Failure scenario: a "counter
  balance"/"agent float to player" shortcut posts value without the
  RG→Risk composition, making self-exclusion unenforceable at the counter
  for the highest-risk channel on the platform. Control: §8.1.
- **P0-5 Writable closure/reachability data.** Failure scenario: any actor
  able to insert a closure row grants themselves ancestry over an
  arbitrary subtree — a total authorization bypass that leaves the RLS
  policies looking correct. Control: §3.3's trigger-only maintenance plus
  a DML guard trigger; test §10.9.

**P1**

- **P1-1 `audit_log`'s tenant-wide RLS (C1/§7.3).** A retail actor with
  any audit read would see the whole tenant's trail; and the narrowing
  cannot be added by a new policy alongside the existing one. Also closes
  a pre-existing missing `app.player_account_id` guard.
- **P1-2 Suspension that does not cascade to descendants (§2.4 step 5).**
  Suspending a Partner otherwise stops one login and leaves its whole
  network trading.
- **P1-3 Scope carried in the JWT (§2.4).** Makes every revocation,
  suspension and reparent advisory until token expiry.
- **P1-4 Missing `app.player_account_id` guard on new retail tables
  (C4).** The exact `risk_rules` P1, pre-empted rather than repeated.
- **P1-5 Commission/margin bundled into a general report permission
  (§5.1).** Cross-tier and cross-sibling commercial leak inside one
  tenant.
- **P1-6 Reporting computed on a scope-bypassing connection (REQ-R2).**
  Aggregation used as an isolation bypass, in a layer where RLS is not
  present.
- **P1-7 Cashier step-up exemption with no compensating control
  (§4.3).** Named explicitly as a completion blocker for the payout
  capability rather than a preference.
- **P1-8 A retail read surface leaking KYC/RG state (§5.3/§8.4).** Doc 25
  §1.1's `bonus:read`/`verification:read` finding, applied to a channel
  with far weaker actors.
- **P1-9 Capability configuration implemented as an OR with permissions
  (§2.8).** Failure scenario: a tenant adds a `counter_payout` capability
  row to a node type and thereby enables payouts for principals holding no
  payout permission — tenants authoring the authorization language. The
  composition must be an `AND` in which capability only ever narrows; test
  §10.35 is the one that proves it.
- **P1-10 Union-of-assignments scope (§2.3).** Failure scenario: a cashier
  holding assignments at three shops issues one request that silently
  spans all three, producing an action no single shop can be held
  accountable for and an audit record that names no node. The one-scope-
  per-session rule and test §10.30 close it.

**P2**

- **P2-1 Small-cell inference in subtree aggregates (§5.4).**
- **P2-2 Reparent cost and stale-closure risk under concurrency (§3.3).**
  Correctness is covered by test §10.8; the performance profile is
  acceptable but should be measured, not assumed.
- **P2-3 Enumeration via 403-vs-404 divergence.** Any retail endpoint that
  403s on an out-of-scope id becomes an org-chart oracle (§2.5).
- **P2-4 Warehouse/replica scope lag (REQ-R3).**
- **P2-5 Shared-POS session hygiene (§4.2)** — shift-bounded sessions and
  short TTLs.
- **P2-7 Jurisdiction-derived capability restrictions beatable by a more
  specific tenant row (§2.8, C16).** Doc 26 §H6's most-specific-wins
  precedence, applied unmodified to capabilities, lets a tenant-scoped row
  override a jurisdiction-scoped removal. ADR 0031 §5's HARD_LIMIT vs
  CONFIGURABLE_LIMIT distinction already solves this shape; flagged to
  `architect` rather than fixed here. Raised as P2 only because no
  capability row exists yet — it becomes P1 the moment one does.
- **P2-8 Decommissioned terminals with live credentials (§4.4, §12.8).**
- **P2-9 Cross-network query composition (§2.4)** — N scoped queries for a
  tenant-wide dashboard. Accepted cost; flagged so it is not "optimised"
  later into a bypass branch, which is the failure mode this trades
  against.
- **P2-6 No detection signal for cashier-assisted registration volume
  (§8.4)** — noted as a future `RISK_SIGNAL`, deliberately not
  approximated now.

---

## 12. Open decisions requiring a human (not engineering calls)

1. **Is retail in scope for the Anjouan B2C MVP at all**, or only for
   future B2B tenants? This changes sequencing materially and is a
   scope/commercial decision.
2. **Regulatory status of an agent network per target jurisdiction.** In
   several markets a retail agent is a licensed role in its own right, and
   agent onboarding is itself a regulated process. Legal, not engineering.
3. **Must cashiers hold a second factor?** (§4.3) — and if not, which
   compensating control is accepted: counter four-eyes, per-shift payout
   ceiling, terminal binding, or network allowlisting.
4. **Self-excluded player balance return at the counter** (§8.3) —
   compliance/legal, per jurisdiction.
5. **Is proxy/assisted play by an agent a requirement?** (§8.5) — if yes,
   it needs its own design, review and probably its own ADR.
6. **Do agents hold player funds or operate on credit/float?** This
   determines whether an agent-account/settlement ledger domain is needed
   at all, and it is `ledger-finance`'s to design once the human answers.
   Nothing in this document assumes either answer.
7. **Four-eyes thresholds and the structuring window** for retail
   administrative and financial actions (§6.3(5)).
8. **Terminal fleet ownership and lifecycle** (§4.4) — are terminals
   tenant-managed or platform-managed, and what is the operational process
   for registering and, critically, **decommissioning** one? A retired
   terminal whose credential is never revoked is the most likely
   real-world failure of the two-principal control, and it is a process
   problem rather than a code one.
9. **Deferred extensions** (recorded so they are not silently built):
   tenant-authored custom retail roles (§2.7), mTLS/device attestation for
   terminal credentials (§4.4 — recorded as target state, not first
   slice), ancestor-metadata visibility for breadcrumbs (§3.3 Class 3),
   and a genuine cross-network single-query view (§2.4 — deliberately
   composed from N scoped queries instead).

---

## 13. Assumptions about parallel work — stated so the Orchestrator can verify consistency

**About `architect`'s `docs/architecture/26-retail-operations-architecture.md`.**
This document was drafted before doc 26 existed and finalized after it
landed. The items below were written as `ASSUMPTION`s and have since been
**verified against doc 26**; the verdicts are recorded so the Orchestrator
can see which held and which did not, rather than a clean list that hides
the corrections:

- A1. **VERIFIED.** Nodes are tenant-owned (`tenant_id NOT NULL`, doc 26
  §1.4/§5.1 entity 5). No platform-wide node; definitions
  (`hierarchy_node_types`/`_relations`/`_capabilities`) are the dual-scope
  layer, instances are not — which is exactly §3.3's Class 1/Class 2
  split.
- A2. **WRONG, and corrected.** Doc 26 §5.1 entity 4 allows a tenant to
  run **several networks**, so "one tree per tenant with a single root"
  does not hold. §2.4 was rewritten: a session acts under one network's
  root at a time, explicitly selected and audited, and a cross-network
  view is composed from N scoped queries rather than a bypass branch. The
  tempting repair (a tenant-wide branch with the GUC unset) is explicitly
  rejected there.
- A3. **VERIFIED.** Single parent (adjacency list, self-FK), cycles
  prevented by a database constraint (doc 26 H4), cross-tenant/
  cross-network parent edges a constraint violation (H5).
- A4. **VERIFIED — this is what resolves §3.1's `BLOCKED` dependency.**
  `hierarchy_node_closure (ancestor_node_id, descendant_node_id, depth)`
  including depth-0 self rows, maintained in the same transaction as the
  adjacency change (H2). Depth is present, which §2.6's strict-descendant
  (`depth > 0`) write rule needs.
- A5. **VERIFIED and exceeded** — nodes also carry `network_id`,
  `jurisdiction_code`, `effective_from`/`effective_to` and `external_ref`.
  Effective dating is an authorization input, handled in §2.3/§2.4.
- A6. **VERIFIED.** Node types, permitted parent→child relations and
  capabilities are configuration rows. Doc 26 adds a capability layer this
  document had not anticipated — reconciled in **§2.8** (permission AND
  capability AND scope; capability may only narrow).
- A7. **VERIFIED.** `retail_player_origins` with `UNIQUE(player_account_id)`
  (doc 26 §5.1 entity 10) — attribution is an edge, not tree membership,
  and a player is explicitly **not** a node (§1.6). Access-control
  consequence answered in **§5.5**.
- A8. **PARTIALLY RESOLVED.** A `hierarchy_network` may optionally
  reference a `brand`; nodes carry `jurisdiction_code` rather than
  `brand_id`. Brand narrowing therefore composes at the network level.
  Unchanged position: brand scoping may only narrow, never widen.
- A9. **NEW, from doc 26 §2.2** — a terminal is an independent
  `service`-type principal bound to one node, and money-touching
  operations require the terminal *and* the cashier. Adopted in §2.4 and
  §4.4; the credential mechanism doc 26 assigned to `security` is answered
  in §4.4.

**About `data-analytics`' reporting/BI document** (not yet read at the
time of writing — genuinely still `ASSUMPTION`s):

- B1. It consumes §5.1's permission primitives rather than minting its own
  retail permissions.
- B2. It accepts REQ-R1/R2/R3 (server-resolved scope pair, no
  bypass-then-filter, reachability replicated with a stated lag bound).
- B3. It owns report contents, the warehouse topology, and the small-cell
  suppression policy (§5.4) — none of which this document specifies.
- B4. It does not introduce a reporting path that reads retail data under
  a service account exempt from the scope predicate. If it does, that is a
  P0 against REQ-R2 and this document's §5 does not cover it.

**About `identity-compliance` (parallel RG/KYC document):**

- C1. RG/KYC rule design for retail is theirs; this document constrains
  only the authorization layer (§8) and asserts no RG/KYC rule.
- C2. §8.3 (self-excluded balance return) and §8.5 (proxy play) are routed
  to them and the human, not answered here.

**About `ledger-finance` (`docs/decisions/0035-retail-agent-network-
accounting.md`, which appeared while this document was being written and
was read before finalization):**

- D1. Retail postings have their own transaction types and an
  `agent_float` account family (ADR 0035 §3.2/§3.3), **not** verbatim
  reuse of `internal/payments`/`internal/withdrawal`. §8.1 was corrected
  to match. The security invariant this ADR requires — RG then Risk,
  inside the posting transaction, before commit — is stated identically
  in ADR 0035, so the two documents agree on the control even though an
  earlier draft of §8.1 described the mechanism wrongly.
- D2. ADR 0035 §1.3's `ledger_accounts.hierarchy_node_id` option (b) is
  **assumed** as the shape §3.5's policy is written against. If
  `architect` + `ledger-finance` ratify a different shape, §3.5 must be
  re-derived (its principles hold; its SQL does not).
- D3. Agent float/credit, commission accrual, settlement and till
  reconciliation are ADR 0035's; this document's §2.7 permissions, §6
  four-eyes controls and §7 audit requirements apply to whatever they
  produce. Note ADR 0035 §7 also discusses four-eyes for retail — the two
  must be reconciled by the Orchestrator so there is one approval
  mechanism (§6.2's `retail_admin_approvals`, reusing the withdrawal
  shape), not two.

---

## 14. Status summary

Every deliverable in this document is **`NOT IMPLEMENTED`**. The single
dependency carried as `BLOCKED` during drafting (`architect`'s ERD
satisfying REQ-H1) is **resolved** by doc 26 §1.3:

| Item | Label | Status |
|---|---|---|
| Three-axis authorization model (§2) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| Scope-not-in-JWT (§2.4) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| Permission/role set (§2.7) | `RECOMMENDATION` | `NOT IMPLEMENTED` |
| Closure-predicate RLS (§3) | `RECOMMENDATION` | `NOT IMPLEMENTED` — **dependency RESOLVED**: doc 26 §1.3 satisfies REQ-H1 |
| Node-scoped ledger-account RLS (§3.5) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` — conditional on ADR 0035 §1.3 option (b) being ratified |
| Permission × capability composition (§2.8) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED`; one gap flagged to `architect` (C16) |
| One scope per session, N:M assignments (§2.3) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| Two-principal terminal + cashier (§4.4) | `ARCHITECTURAL DECISION` (adopted from doc 26 §2.2) | `NOT IMPLEMENTED` |
| Terminal credential mechanism (§4.4) | `RECOMMENDATION` | `NOT IMPLEMENTED` — answers doc 26's open question to `security` |
| No node dimension on `player_accounts` (§5.5) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` — answers doc 26 Conflict B, needs `identity-compliance` concurrence |
| `app.hierarchy_node_id` + `WithNodeScope` (§3.2) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| Cashier = staff + binding (§4.1) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| Cashier MFA/step-up policy (§4.3) | `OPEN DECISION` | `NOT IMPLEMENTED` |
| Reporting permission primitives (§5) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| Four-eyes for retail admin (§6) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| `audit_log` node columns (§7.1) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| `audit_log` RLS narrowing (§7.3) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` — requires `architect` sign-off (C1) |
| RG/KYC non-bypass guarantees (§8) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |
| Proxy/assisted play (§8.5) | `OPEN DECISION` | `NOT IMPLEMENTED` |
| Test specification (§10) | `ARCHITECTURAL DECISION` | `NOT IMPLEMENTED` |

Nothing in this document makes any retail functionality "secure," and
nothing here has been reviewed against an implementation, because none
exists. When retail is implemented, every change touching auth, sessions,
tokens, RBAC, secrets, PII or money-adjacent code requires its own
`security` review of the actual diff — this document is the standard that
review will be conducted against, not a substitute for it.

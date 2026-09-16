# 12 — Back Office, Partner Console, Audit and Reporting Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.8, §4.9.

## RBAC

Three-tier: platform administrator (us), partner administrator, brand
operator. Permissions are scoped by tenant and never inferred from the UI
— every mutating action is authorized server-side regardless of what the
requesting UI displayed.

## Audit

Every mutating action writes an audit record — actor, tenant, entity,
before state, after state, IP, reason code — to an append-only store with
5–7 year retention. Manual balance adjustments require a reason code and
four-eyes approval above a threshold. This is the first thing an auditor
asks to see; its absence has ended platform businesses (Blueprint §4.8) —
treated as core infrastructure, not defensive extra work.

## Back office and partner console

Two distinct surfaces (see `00-system-overview.md`), both API-first,
sharing the same RBAC and audit substrate but serving different users with
different usability bars — back office must be usable by a non-technical
retention manager; partner console serves us and licensees for
provisioning, credentials, revenue share, and compliance overview.

## Reporting and BI

Never run reports against the transactional ledger. Change data capture
(Debezium) feeds the event bus, which feeds ClickHouse — the operational
database keeps its latency budget, analysts get columnar speed (Blueprint
§4.9).

Daily reports: GGR/NGR by brand/game/provider/country/currency/day, bonus
cost, provider cost, PSP cost, first-time depositors, retention cohorts,
player LTV. Sportsbook reports must carry an explicit open-liability line
(see `09-sportsbook-architecture.md`).

Regulatory exports are per-jurisdiction, behind a pluggable interface —
same reasoning as the jurisdiction-pluggability design consequence in
`01-requirements-inventory.md` §5.

## Ownership and stage mapping

Back office/partner console/audit: `backoffice` (implementation),
`security` (RBAC/audit review). Reporting/BI: `data-analytics`, with
`ledger-finance` review on anything presented as a financial figure. Audit
foundation is Stage 2 (alongside identity/tenancy); full back office,
partner console, and reporting pipeline are Stage 6.

## Retail agent-hierarchy reporting (Stage 4H-B0 architecture freeze) — `NOT IMPLEMENTED`

Status: architecture-freeze only — no CDC config, ClickHouse schema, API
endpoint, or report exists from this section. Source: the confirmed
business requirement that the platform support a retail agent-hierarchy
network (Operator → Partner → Super Agent → Agent → Player/Cashier,
configurable), directive requirements #18–#21. This section **extends**
the "Reporting and BI" design above; it does not replace or duplicate it,
per this document's own ownership rule and CLAUDE.md's rule that
ClickHouse is never authoritative for money — the ledger in PostgreSQL is,
here as everywhere else in this pipeline.

**Dependencies on two parallel, not-yet-landed specialist documents** —
consumed, not designed, here:

- `security`, `docs/decisions/0036-retail-hierarchy-rbac-and-audit.md`
  (in progress at the time of writing): assumed to define a
  `retail_report:read`-shaped permission scoped to a hierarchy-node
  subtree. §2 below is written against that assumed shape; if the landed
  permission model scopes differently (e.g. single-node rather than
  subtree, or a different verb), §2 needs revisiting by `security`/
  `data-analytics` together.
- `architect`'s parallel hierarchy-storage decision (adjacency list vs.
  closure table for the hierarchy-node/edge model): **not settled at the
  time of writing.** §2.2 states this as a blocking `OPEN DECISION` for
  query efficiency, not for the dimension design itself.

### 1. Same reporting pipeline, one more dimension — `ARCHITECTURAL DECISION`

Directive #21 ("design for both online and retail transactions using the
same authoritative financial ledger") is a hard requirement already
consistent with how `06-wallet-ledger-architecture.md` is built: that
document's account-type list (`player_cash`, `house_gaming`, etc.) has no
channel concept baked in today, and none is needed — a retail
cashier-assisted deposit or bet posts through the exact same
`LedgerTransaction`/`LedgerEntry` shape, the same
`(tenant_id, provider_id, provider_tx_id)` idempotency discipline, and the
same invariants as an online one. Retail is a *channel* and an
*origination path* (cashier/agent-operated vs. player self-service), never
a second ledger, a second account-type set, or a second set of invariants.
Whatever field carries the originating hierarchy node on the ledger/wallet
write path is `ledger-finance`'s/`architect`'s scope (see
`docs/decisions/0035-retail-agent-network-accounting.md`) — this document
does not add columns to the ledger; it states what the reporting pipeline
does with the result.

The new dimension the reporting pipeline gains, additive alongside the
existing tenant/brand/game/provider/country/currency/day set already
named in "Reporting and BI" above:

- `hierarchy_node_id` — the retail node (Agent, SuperAgent, Partner) that
  originated or is accountable for the transaction; null for an online
  transaction with no agent involvement.
- `hierarchy_node_type` — Operator / Partner / SuperAgent / Agent /
  Cashier, denormalized at CDC time so a report can filter or group by
  level without a hierarchy-table join on every query.
- `channel` — `online` | `retail`, always populated (coarser than
  `hierarchy_node_id`, which is null for pure-online rows).

Mechanically: (a) the ledger/wallet write path gains whatever column
`architect`/`ledger-finance` settle on to carry the originating node id —
not this document's decision; (b) the existing Debezium CDC capture of
`ledger_transactions`/`ledger_entries` picks that column up the same way
it already picks up `tenant_id`/`brand_id` — no new connector, no new
topic, no new consumer; (c) the ClickHouse ingestion/materialized-view
layer adds `hierarchy_node_id`/`hierarchy_node_type`/`channel` as further
`GROUP BY`/filter columns on the existing GGR/NGR/bonus-cost/
provider-cost/PSP-cost fact tables. **This is explicitly not a parallel
reporting system for retail**: one fact-table set, one set of
materialized views, one freshness target (< 5 min, Blueprint §6). A query
scoped to `hierarchy_node_id IS NULL AND channel = 'online'` is
definitionally today's existing report, unchanged.

Money-figure discipline is unchanged by this addition: any retail report
presenting a monetary figure (agent commission earned, GGR attributable to
a node's subtree, till variance) is labeled as **derived from the ledger
via CDC, not authoritative**, subject to the same reconciliation posture
as every other figure this pipeline produces — see
`reconciliation-model.md` §2.1 for the mechanism, and §4 below for why a
retail-specific reconciliation view must never become a second
computation of the same fact.

### 2. Hierarchy-scoped report access model — `ARCHITECTURAL DECISION` + `OPEN DECISION`

**2.1 What each level needs.** No new report *types* are invented — only
new scope filters over the report set already named in "Reporting and BI"
above, plus the retail-specific commission/till views a hierarchy
structurally requires:

| Level | Read scope | Reports |
|---|---|---|
| Agent | itself only (a leaf in the directive's hierarchy) | own players' activity (GGR/NGR/FTD/retention at player level), own commission earned, own cashier till/settlement view (§4) |
| Super Agent | its subtree (its Agents + their players) | same report set, rolled up across its Agents, with per-Agent drill-down — not a single opaque aggregate |
| Partner | its whole subtree | same, rolled up to Partner level, drillable down through Super Agent/Agent |
| Operator | tenant-wide | the existing full daily report set from "Reporting and BI" above, with `hierarchy_node_id`/`channel` simply available as additional grouping dimensions — this is today's Back Office reporting user, unchanged |

A level's report is always "its own report, plus every report the levels
below it could produce, rolled up and drillable" — never a distinct report
definition per level. This falls directly out of §1: the report
definition is identical at every level; only the
`WHERE hierarchy_node_id IN (subtree of X)` filter changes, supplied by
the caller's authorized scope — never a client-asserted node id, the same
server-side-only-authorization rule CLAUDE.md states for `tenant_id`,
applied here to hierarchy node.

**2.2 Query pattern and its dependency on the hierarchy storage decision —
`OPEN DECISION`, blocking efficiency (not blocking the dimension design in
§1).** The underlying query pattern behind every row above except Agent's
is a **subtree aggregation**: "aggregate fact rows for the set of
hierarchy nodes that are descendants-or-self of node X." How efficiently
that descendant set is produced depends on `architect`'s hierarchy-storage
decision, not yet settled:

- **Closure table** (a materialized `(ancestor_node_id,
  descendant_node_id, depth)` table maintained transactionally alongside
  every node insert/move): the descendant set is a single indexed lookup.
  Critically for this pipeline, that table is exactly the shape CDC can
  replicate into ClickHouse directly as a small dimension table, making
  subtree-scoped reports a plain join — the recommended shape from a
  reporting-efficiency standpoint.
- **Adjacency list only** (`parent_node_id` per row, no precomputed
  closure): the descendant set needs a recursive walk. Postgres can do
  this operationally (`WITH RECURSIVE`), but ClickHouse has no first-class
  recursive-CTE equivalent suited to per-report-request traversal over a
  table that can hold thousands of nodes for a national retail network —
  recursive resolution at report-query time is not viable in the
  analytical store.

**Dependency, stated explicitly rather than assumed away**: whichever
model `architect` chooses operationally, the reporting layer needs a
resolved ancestor/descendant mapping available to CDC — either directly
(closure table exists operationally, CDC replicates it as-is) or via a
reporting-side materialized projection this pipeline would have to build
and maintain itself (recompute a closure/subtree-membership table from the
adjacency list on every node create/move/re-parent event, publish it to
ClickHouse as its own dimension table). The second path is buildable but
is additional pipeline surface not needed if the operational model is
already a closure table. This document does not require `architect` to
choose a closure table, but flags that choosing adjacency-list-only moves
the closure-computation problem here instead of eliminating it, and asks
that ownership of computing it be settled between `architect` and
`data-analytics` before Stage 4H-B implementation, to avoid building it
twice. Hierarchy nodes are expected to change rarely relative to
transaction volume, so eventual consistency on this projection (within the
same < 5 min freshness target) is acceptable — not a hot-path requirement.

### 3. "Retail frontend exposes BO-equivalent reporting" — architectural meaning — `RECOMMENDATION`

Recommendation: **one reporting API surface, not two.** The same report
endpoints that serve the Back Office serve a future retail/agent console,
parameterized entirely by the calling principal's own authenticated
hierarchy scope (§2) plus tenant — never by a client-supplied node id, and
never by which frontend is calling.

- Report definitions, the underlying ClickHouse queries/materialized
  views, and the API contract (request/response shape, pagination,
  freshness metadata) are identical regardless of caller.
- What differs per caller is exactly two things, both already
  server-side-enforced concerns this pipeline respects rather than
  invents: (a) the authorization check (`retail_report:read` on a
  subtree, vs. an existing BO permission) gating which
  `hierarchy_node_id` values a request may filter to; (b) which report
  *types* a given frontend chooses to surface in its own UI — a UI/UX
  decision, explicitly out of scope here (`ux-design`/`frontend`).
- Rejected alternative: dedicated retail-only endpoints duplicating BO
  report logic. That would mean two implementations of the same GGR/NGR
  computation able to silently drift from each other — the "second
  computation of the same fact" failure mode CLAUDE.md's ledger-authority
  rule exists to prevent one layer down (ledger vs. analytics), repeated
  one layer up (BO API vs. retail API) if allowed. One API surface means
  one place a reporting bug or metric-definition change is fixed.
- This is a recommendation about the API layer only. It says nothing about
  retail-console authentication mechanics (agent/cashier login, session
  model) beyond "it must resolve to the same permission-and-scope shape
  §2 assumes" — that is `security`/`identity-compliance` scope for
  agent-network identity, not this document's.

### 4. Reconciliation reporting for retail — `ARCHITECTURAL DECISION`

Cashier settlement/reconciliation (till float, cash collected vs.
remitted, agent-to-superagent cash settlement) has its accounting/
ledger-posting design owned by `ledger-finance` in
`docs/decisions/0035-retail-agent-network-accounting.md` (in progress, not
yet landed at the time of writing) — assumed to define its own
reconciliation stream analogous in shape to the existing streams in
`reconciliation-model.md` §2, most likely closest to §2.2's wallet↔PSP
pattern (a counterparty figure — the cashier's physically-counted cash —
reconciled against a ledger-derived expected figure), with its own
`ReconciliationRun`/`ReconciliationMismatch` rows per that document's §5.

This reporting layer's obligation is precisely bounded: **surface that
stream's already-computed result, never recompute it.**

- The retail cashier-settlement report an Agent/Super Agent sees is a read
  view over the CDC-replicated `reconciliation_runs`/
  `reconciliation_mismatches` rows for the relevant retail stream — the
  same precedent `reconciliation-model.md` §6 already states for
  cross-tenant/aggregate reconciliation reporting ("through the reporting
  layer," never relaxed access to the operational tables) — filtered by
  hierarchy scope per §2, joined to hierarchy-node identity for display.
- It must never independently re-sum ledger entries to produce its own
  "expected cash" figure and compare it against a reported "actual cash"
  figure. Doing so would let a bug or timing skew between that computation
  and `ledger-finance`'s reconciliation job produce two different answers
  to "does this cashier's till balance," with no way to tell which is
  right — the same second-source-of-truth failure this document's
  ledger-authority framing exists to prevent, one level removed from money
  itself but just as capable of destroying trust in the numbers.
- Presentation-level aggregation of the existing rows (e.g. "days since
  last clean settlement," a Partner's rolled-up count of subtree agents
  with an open mismatch today) is fine — it reads `status`/
  `investigation_status` off existing rows, it does not compute a new cash
  figure.
- Freshness follows this pipeline's existing < 5 min CDC target; how often
  cashier settlement itself runs (per shift, daily) is 0035's
  business-process design, not this document's.
- The exact reconciliation-key/report field list cannot be finalized until
  `docs/decisions/0035` lands — this section states the rule (surface,
  never recompute) and the mechanism (CDC replication of
  `ledger-finance`'s reconciliation tables, same as every existing
  stream), not a final field list, which remains `NOT IMPLEMENTED` pending
  that document.

### 5. Labels

- §1 (additive `hierarchy_node_id`/`hierarchy_node_type`/`channel`
  dimensions on the existing CDC/ClickHouse pipeline): `ARCHITECTURAL
  DECISION`, `NOT IMPLEMENTED`.
- §2.1 (per-level report-scope model): `ARCHITECTURAL DECISION`, `NOT
  IMPLEMENTED`.
- §2.2 (subtree-aggregation query pattern's dependency on hierarchy
  storage): `OPEN DECISION` (owner: `architect`, cross-check:
  `data-analytics`) — blocks efficient implementation, not the dimension
  design itself.
- §3 (one shared reporting API surface for BO and retail console):
  `RECOMMENDATION`, `NOT IMPLEMENTED`.
- §4 (retail reconciliation reporting as a surfaced view, never a
  re-derivation): `ARCHITECTURAL DECISION`, `NOT IMPLEMENTED`, pending
  `docs/decisions/0035`'s final shape.

No CDC pipeline change, ClickHouse schema change, API endpoint, or report
exists yet as a result of this section. Nothing here authorizes Stage
4H-B implementation, and nothing here is a substitute for the human/legal
review any regulatory export still separately requires.

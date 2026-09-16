# 26 — Retail Operations & Configurable Agent-Network Architecture

Status: **`NOT IMPLEMENTED`.** Stage 4H-B0 architecture/scope freeze only.
No `internal/retail` or `internal/agentnetwork` package exists, no schema,
no migration, no API, no test. Every entity, table, column, permission,
interface and flow in this document is a **design proposal**, not a
built artifact. Nothing in this document authorizes implementation.

Owner: `architect` (cross-domain architecture, per
`docs/governance/ownership.md`). This document deliberately stops at the
boundary of four other specialists' ownership and names the document each
open item belongs to — see §0.3.

---

## 0. Preamble — provenance, labels, and what this document does not decide

### 0.1 Blueprint anchor: there is none

`ARCHITECTURAL DECISION` (evidence-based finding, not an opinion):
**`iGaming-Platform-Blueprint.pdf` contains no retail, land-based, agent-
network, POS, terminal, shop, kiosk, or cash-counter requirement of any
kind.** This was verified by full-text extraction and search of all 20
pages: the tokens `retail`, `agent`, `POS`, `terminal`, `kiosk`,
`land-based`, `shop`, `outlet`, `branch` and `voucher` do not occur as
whole words anywhere in the document. The word "cashier" occurs three
times (§1 vendor table, §2 surfaces table, §3 system map) and in every
occurrence means the **online player-facing deposit/withdraw screen** in
the brand frontend — not a human cashier at a physical counter. The
Blueprint's own framing is explicitly and exclusively online
("multi-tenant platform that runs your own casino brand first, then
licenses the same core to partner operators"), with a system map whose
only three product surfaces are brand frontend, operator back office and
partner console.

Therefore, per `CLAUDE.md` ("Do not assume a requirement exists unless
the Blueprint supports it… Never present a recommendation as if it were a
Blueprint requirement"):

- **No claim in this document is labeled `BLUEPRINT` on the subject of
  retail.** Where a claim is labeled `BLUEPRINT`, it is a *pre-existing*
  platform invariant (ledger, tenancy, tokens) that retail must not
  violate — never a retail requirement.
- Retail enters this project as a **stated business requirement from the
  human via the Orchestrator** (Stage 4H-B0's directive: "the platform
  must support BOTH online and retail iGaming operations, as one
  platform"). That is a legitimate authority for scope — it is simply not
  the Blueprint, and must never be cited as such.
- This is structurally the **same situation `product-owner-proxy` flagged
  for the Gamification domain in Stage 4H-A** (five architecture documents
  with no Blueprint anchor). `RECOMMENDATION`: the Orchestrator records
  Retail in `docs/architecture/14-mvp-scope-and-roadmap.md` as
  human-directed scope with no Blueprint anchor, exactly as Gamification
  was recorded. `14-mvp-scope-and-roadmap.md` is not edited by this
  document because this stage's directive restricts the architect to two
  files.

### 0.2 Label convention used throughout

| Label | Meaning in this document |
|---|---|
| `BLUEPRINT` | Traceable to a specific Blueprint section. Used here only for pre-existing platform invariants retail must respect. |
| `ARCHITECTURAL DECISION` | A design choice this document makes, within the architect's own authority, consistent with prior approved architecture. Binding on any future implementation unless superseded by an ADR. |
| `RECOMMENDATION` | A proposal that needs another specialist's or the Orchestrator's sign-off before it binds. |
| `OPEN DECISION` | Genuinely unresolved. Either a business/legal question for the human, or a cross-domain question owned by another specialist. Never silently resolved here. |

Deliverable status label for the whole document: **`NOT IMPLEMENTED`**.

### 0.3 What this document deliberately does NOT contain

| Subject | Owner | Expected document |
|---|---|---|
| Ledger accounting treatment of every retail money movement (accounts, postings, double-entry, float/settlement/commission treatment, invariants) | `ledger-finance` | `docs/decisions/0035-*` (Wave 1, parallel) |
| Retail RBAC permission set, terminal credential issuance/rotation, cashier session security, four-eyes thresholds | `security` | security's Wave 1 RBAC document |
| KYC tier logic for a retail-originated player, RG enforcement at the counter, AML treatment of cash | `identity-compliance` | identity-compliance's Wave 1 identity impact analysis |
| Cash-in/cash-out as a payment method, PSP/settlement rails for agent remittance | `payments` | payments' Wave 1 document |
| POS hardware, device drivers, printers, receipt/ticket physical format, network topology of a shop | **out of scope entirely** | — |
| Test strategy for retail | `qa` | a future qa document |

This document specifies only the **hierarchy/network model, the actor and
API-client shape, the authorization shape of money movement, the
conceptual ERD, and the reuse/extend/new analysis against existing
domains.**

---

## 1. Configurable hierarchy / role / network model

### 1.1 The requirement, restated precisely

The business requirement is a configurable network that **can** express
`Operator → Partner → Super Agent → Agent → Player/Cashier`, with
**different depths and structures per tenant, licence and jurisdiction**,
and **without that chain being hard-coded**.

`ARCHITECTURAL DECISION`: the chain above is treated in this design as
**one example configuration, never a schema, enum, column, or code path.**
No table, constraint, enum value, Go type or permission name proposed
below contains the strings `operator`, `partner`, `super_agent`, `agent`
or `cashier` as a *structural* element. Those appear only as **seed data
rows** in a configuration table. This is the direct application of
`CLAUDE.md`'s "nothing brand-specific may become a code path" rule to the
network topology: **nothing network-shape-specific may become a code
path either.**

### 1.2 Three separated concerns (the core decision)

`ARCHITECTURAL DECISION`: the model separates three things most retail
platforms conflate into one "agent level" integer. Conflating them is
precisely what makes a hierarchy un-reconfigurable later.

1. **Node type** — *what kind of thing is this* (a definitional template:
   "super agent", "shop", "franchise"). Carries no position.
2. **Structure** — *what may sit under what* (which node types may parent
   which other node types, and how deep). A separate, per-network
   configuration, not an ordinal on the type.
3. **Capability** — *what may this kind of node actually do* (register
   players, take cash, pay out cash, fund a child, hold float, earn
   commission). Also separate, because two tenants can use a type named
   "agent" with genuinely different capabilities, and because a capability
   may be legally restricted in one jurisdiction and not another.

An integer `level` column is **explicitly rejected** (`ARCHITECTURAL
DECISION`): it hard-codes a single ladder, breaks the moment one tenant
wants a 3-deep network and another a 6-deep one, breaks again when a
tenant wants two different branch shapes in the same network (e.g. a
direct-to-shop branch alongside a super-agent branch), and silently
encodes commercial policy as a number nobody can validate.

### 1.3 Storage decision: adjacency list is authoritative, closure table is a derived projection

`ARCHITECTURAL DECISION`: **the authoritative structure is an adjacency
list (`hierarchy_nodes.parent_node_id`, self-referencing). A closure
table (`hierarchy_node_closure`) is maintained as a derived projection of
it, in the same transaction as any structural change, and is
reconcilable against the adjacency list.** Path enumeration
(materialized path / `ltree`) is rejected.

This is not a novel pattern for this platform — it is the *same* pattern
the ledger already uses and the reviewers already understand: **an
append-only/authoritative source of truth plus a derived projection
maintained in the same transaction and periodically diffed against a
recomputation** (`06-wallet-ledger-architecture.md`,
`docs/decisions/0019`). Choosing a third, unfamiliar shape here would
cost more in review and correctness than it saves.

Justification against this platform's specific constraints:

| Criterion | Adjacency only | **Adjacency + closure (chosen)** | Path enumeration |
|---|---|---|---|
| "Is node X inside node Y's subtree?" — the single most important predicate, needed **inside an RLS policy** | Recursive CTE per row. Unacceptable: an RLS policy is evaluated per candidate row, and a recursive CTE per row is both a performance cliff and a planner hazard. | Indexed equality lookup on `(ancestor_node_id, descendant_node_id)`. `EXISTS (…)` in a policy, index-backed. | String prefix `LIKE 'a.b.c%'`. Index-usable but prefix-matching in a security predicate is a well-known footgun (escaping, separator collisions, a node id that is a prefix of another). |
| Arbitrary, per-tenant depth | Yes | Yes | Yes, until the path column hits a length/precision limit nobody notices until production |
| Re-parenting a subtree (an agent moves to a different super agent — a real, routine retail operation) | Single row update | Single row update + bounded closure rewrite for the moved subtree, in the same transaction | Rewrites the path string of **every** descendant; a partial failure leaves a structurally corrupt tree |
| Auditability of a structural change | Good | Good — the closure delta is reconstructible | Poor — the change is diffused across many mutated strings |
| Reconciliation story | n/a | **The closure table can be fully recomputed from the adjacency list and diffed on a schedule, exactly like the balance projection.** Non-zero drift is an incident, not a mystery. | The path strings *are* the structure; there is nothing independent to diff them against |
| Consistency with existing platform patterns | — | **Matches the ledger's authoritative-source + derived-projection + hourly-diff discipline** | Introduces a pattern with no precedent here |

`ARCHITECTURAL DECISION` (invariants the future implementation must
satisfy, and that `qa`/`code-reviewer` verify against):

- **H1.** `hierarchy_nodes.parent_node_id` is the only authoritative
  statement of structure. No business logic reads the closure table to
  *decide* structure; it reads it only to *answer containment questions
  fast*.
- **H2.** Every closure-table row is written in the **same transaction**
  as the adjacency change that implies it. There is no asynchronous
  rebuild job on the write path.
- **H3.** The closure table is recomputed from scratch and diffed against
  the stored projection on a schedule. Any non-zero drift is treated with
  the same seriousness as ledger/projection drift (`CLAUDE.md`'s
  reconciliation rule applied to structure rather than money) — because a
  wrong closure row is, by §4, an **authorization** error, not merely a
  reporting error.
- **H4.** A cycle is impossible by construction: every `INSERT`/`UPDATE`
  of `parent_node_id` is rejected if the proposed parent already appears
  as a descendant of the node being moved (an indexed closure lookup, a
  database-enforced constraint or trigger — **not** application
  discipline, per `CLAUDE.md`'s "enforced by the database, not by
  discipline in application code" rule).
- **H5.** A node's parent must belong to the **same `tenant_id` and the
  same `network_id`**, enforced by composite foreign key — mirroring ADR
  0012's `(brand_id, tenant_id) REFERENCES brands(id, tenant_id)`
  precedent exactly. A cross-tenant parent edge must be a constraint
  violation, not a bug a future service could introduce.

### 1.4 Dual-scope: platform-wide template vs. tenant instance

`ARCHITECTURAL DECISION`: the design reuses this codebase's already-
reviewed **dual-scope definition / tenant-scoped instance** pattern rather
than inventing a second shape. The precedents are explicit and already
carry review sign-off: `risk_rules` (ADR 0031 §3 — `tenant_id` NULL =
platform-wide, non-NULL = tenant-owned), `player_restrictions` (migration
0037), and the `PointType` correction in
`24-points-accounting-architecture.md` §2 (the *definition* is dual-scope
and nullable; every *balance/instance* row is `tenant_id NOT NULL`).

Applied here:

| Layer | Scope | Rationale |
|---|---|---|
| `hierarchy_node_types` (definition) | **Dual-scope.** `tenant_id NULL` = platform-wide template; non-NULL = tenant-specific type | Lets the platform ship a curated starter set of node types a tenant can adopt, while a tenant with a genuinely different network vocabulary defines its own — exactly `PointType`'s reasoning ("a definition is a template"). |
| `hierarchy_node_type_relations` (allowed parent→child structure) | **Dual-scope**, but see H6 | A platform-wide *default* topology template is useful; a tenant/licence/jurisdiction-specific override is the requirement. |
| `hierarchy_node_capabilities` (what a type may do) | **Dual-scope**, narrowable by `jurisdiction_code` | A capability may be legally unavailable in one jurisdiction (e.g. cash payout above a threshold) — the jurisdiction dimension mirrors `risk_rules`' own optional `jurisdiction_code` narrowing. |
| `hierarchy_networks` (a network instance) | **Tenant-scoped, `tenant_id NOT NULL`** | A network is commercial reality, never a template. |
| `hierarchy_nodes` (a node instance) | **Tenant-scoped, `tenant_id NOT NULL`** | Same reasoning as `PointBalance`/`PointEntry`. |
| every assignment/terminal/policy row | **Tenant-scoped, `tenant_id NOT NULL`** | — |

- **H6** `ARCHITECTURAL DECISION`: a tenant-scoped `hierarchy_node_type`
  may **not** reference a platform-wide relation row as if it were its
  own, and a tenant-scoped relation row **always wins** over a
  platform-wide one for the same `(parent_type, child_type)` pair.
  Precedence follows `risk_rules`' established most-specific-wins ordering
  (ADR 0031 §5) rather than a new ordering: tenant+jurisdiction > tenant >
  platform+jurisdiction > platform. A tie at the same specificity is a
  **configuration error that fails closed**, exactly like
  `risk.ErrConflictingRules` — never silently resolved by insertion order.

- **H7** `RECOMMENDATION` (needs `security` sign-off): the dual-scope RLS
  policy shape for the definition tables is the one already used by
  `risk_rules`/`point_types` —
  `tenant_id IS NULL OR tenant_id = current_setting('app.tenant_id')::uuid`
  for `SELECT`, with `INSERT`/`UPDATE`/`DELETE` of `tenant_id IS NULL`
  rows reachable only from a platform-scoped connection. Note the
  **already-documented limitation this inherits** (ADR 0031 §8): *no role
  can create a genuinely platform-wide row via HTTP today*. Retail
  inherits that gap rather than introducing it; it is called out here so
  nobody plans a "platform ships a default network template via the API"
  feature on an assumption that is currently false.

### 1.5 How the example chain is expressed (configuration, not schema)

For illustration only — this is **seed data**, and a different tenant's
rows would be entirely different with zero code change:

```
hierarchy_node_types            (tenant_id NULL = platform template)
  code='operator'      display_name='Operator'
  code='partner'       display_name='Partner'
  code='super_agent'   display_name='Super Agent'
  code='agent'         display_name='Agent'
  code='shop'          display_name='Retail Location'
  code='cashier_desk'  display_name='Cashier Desk'

hierarchy_node_type_relations   (per tenant/network/jurisdiction)
  operator    -> partner        min_children=0 max_children=NULL
  partner     -> super_agent
  super_agent -> agent
  agent       -> shop
  shop        -> cashier_desk

hierarchy_node_capabilities     (per type, optionally per jurisdiction)
  super_agent : fund_child, settle_with_parent, earn_commission
  agent       : fund_child, settle_with_parent, earn_commission,
                register_player
  shop        : register_player, counter_deposit, counter_payout
  cashier_desk: counter_deposit, counter_payout
```

A tenant wanting `Operator → Agent → Player` deletes three relation rows.
A tenant wanting a 7-deep franchise structure adds rows. A jurisdiction
that forbids agents registering players removes one capability row for
that `jurisdiction_code`. **No migration, no deploy, no code path.**

### 1.6 Where the Player sits — an important boundary correction

`ARCHITECTURAL DECISION`: **a Player is NOT a node in the hierarchy, and
a Cashier is NOT a node in the hierarchy.** The requirement's phrasing
"… → Agent → Player/Cashier" describes the *commercial* chain, but
modelling either as a `hierarchy_nodes` row would be a serious error:

- A `PlayerAccount` already has an owner model (`tenant_id`, `brand_id`,
  `person_id` — ADR 0012, ADR 0027) and a platform-wide `Person` above it.
  Making a player a hierarchy node would create a **second, competing
  identity hierarchy**, and would put player rows under a structural
  authorization model that `identity-compliance` owns. It would also
  quietly re-open ADR 0027's cross-brand identity model.
  → A player's relationship to the network is an **attribution edge**
  (`retail_player_origins`, §3), not tree membership.
- A cashier is a **human staff member** with an assignment to a node, not
  a structural position. One human can be assigned to two shops this week
  and a third next week; one shop has several cashiers. That is an N:M
  assignment with effective dating (`hierarchy_node_staff_assignments`,
  §2), not a tree edge.

The tree therefore contains **only commercial/operational units that can
hold a position, a float, a commission arrangement, and a set of
children.** Everything else attaches to a node by reference. This keeps
the subtree-containment predicate (§1.3) meaningful: "is this thing inside
my subtree" always means "is this *organizational unit* inside my
subtree", never a mix of organizational units, humans and players in one
`parent_id` column.

---

## 2. Cashier, terminal and POS as API clients

`ARCHITECTURAL DECISION` (the governing principle): **a retail terminal
is an API client of the same platform, never a second financial system.**
There is no terminal-local balance, no terminal-local ledger, no
terminal-side authorization decision, and no separate retail wallet
system. A POS is architecturally in the same class as the brand frontend:
a presentation surface over server-enforced business rules. `CLAUDE.md`'s
"authorization is enforced server-side only… never inferred from the UI"
applies to a counter terminal identically to a browser.

### 2.1 Is a cashier a `staff_user`? — Yes.

`ARCHITECTURAL DECISION`: **a cashier is an `identity.StaffUser`, with a
retail-appropriate `StaffRole`. No new actor type, no new principal type,
no parallel user table.**

Reasoning, grounded in what already exists:

- `audit.ActorType` is already `player | staff | service | system`
  (`internal/audit/audit.go`), and `auth.PrincipalType` is already
  `player | staff | service` (`internal/auth/jwt.go`), deliberately
  mirroring each other. Introducing a fifth/fourth value (`cashier`)
  would fork **every** audit write, every session row
  (`sessions.principal_type`), every login-attempt row, and every token
  path — for an actor that is, in every respect that matters, a staff
  member performing tenant-scoped operations under RBAC.
- `staff_users` already carries the exact properties a cashier needs:
  `tenant_id` (migration 0011's CHECK already enforces "every
  non-`platform_admin` role belongs to exactly one tenant" — a cashier
  trivially satisfies this), `role`, `status`, and the optional
  `person_id` linkage that `internal/withdrawal`'s `BeneficiaryCheck`
  already uses to prevent self-approval. That last one is *directly*
  valuable in retail: a cashier who is also a player at the same brand is
  exactly the self-dealing case a retail network must prevent, and the
  mechanism already exists.
- The MFA/step-up machinery (ADR 0017) and the session/refresh model (ADR
  0016, ADR 0018) apply unchanged.

`RECOMMENDATION` (requires `identity-compliance` for the enum/CHECK and
`security` for the permission grants — **this document does not add
them**): two new `StaffRole` values, e.g. `retail_cashier` and
`retail_network_manager`, following `StaffRoleRiskManager`'s Stage-4G
precedent of a **dedicated role rather than bundling capability into an
existing broad role**. Retail counter permissions must specifically NOT be
implied by `tenant_admin`, for the same separation-of-duties reason
`PermRGRestrictionWrite` and `PermWithdrawalPolicyWrite` are not.
The concrete permission names, grants and thresholds are **security's
document**, not this one.

`ARCHITECTURAL DECISION` (a non-obvious but load-bearing point): a
cashier's authority is **not** a property of their `staff_users` row. It
is the intersection of (a) their role's permissions and (b) their
**active node assignment**. A cashier with `retail_cashier` and no active
assignment can do nothing. This is why the assignment is its own table
with effective dating (§5) and **not** a `node_id` column on
`staff_users` — which would additionally require a schema change to an
Identity-owned, prior-approved table (see §8, Conflict C).

### 2.2 The terminal is a second, independent principal

`ARCHITECTURAL DECISION`: **a money-touching retail operation requires
TWO authenticated principals presented together: the terminal and the
cashier.** Neither alone is sufficient.

- **Terminal principal** — a `service`-type principal, exactly ADR 0014's
  Decision option 2 ("a future tenant-scoped service caller… would
  authenticate the same way a human does — a token scoped to exactly one
  tenant, minted for that service, checked against the same
  `RequirePermission`/`RequireTenantScope` middleware humans go
  through"). Retail is the first concrete consumer that makes ADR 0014's
  option 2 real rather than hypothetical. A terminal principal is bound at
  registration time to **exactly one `hierarchy_node`**, and that binding
  is server-side data, never a client-supplied field.
- **Cashier principal** — a `staff` principal as above, authenticated at
  shift start.

Why both, rather than just the cashier: this mirrors the platform's
**already-accepted two-token discipline** (`05-identity-architecture.md`:
brand session token vs. game launch token are "two different token types,
never conflated"). The terminal token answers *"which registered device,
at which node, in which tenant"*; the cashier token answers *"which
accountable human"*. A stolen cashier credential used from an
unregistered device fails. A compromised terminal with no cashier session
fails. Every audit record carries both. This is the retail analogue of
`security`'s existing invariant that a leak at one integration must not
become a platform-wide compromise.

`OPEN DECISION` (for `security`): the terminal credential's issuance,
storage, rotation, revocation and binding mechanism (mTLS client cert vs.
a long-lived registration secret exchanged for short-lived tokens vs.
device-attestation) is **security's to specify**, not the architect's.
This document specifies only the *shape*: one credential per terminal,
tenant-scoped, node-bound, individually revocable without affecting any
other terminal, and never shared between terminals.

### 2.3 Session model

`ARCHITECTURAL DECISION` — three nested, independently revocable scopes:

| Scope | Lifetime | Revocation effect |
|---|---|---|
| **Terminal registration** | Long-lived, administrative | Revoking it takes one physical terminal offline permanently until re-registered |
| **Cashier shift session** | Bounded by a shift; short-lived access token + rotating refresh, reusing `internal/auth`'s existing session model unchanged | Revoking it ends that human's authority at that terminal; the terminal stays registered |
| **Counter operation** | A single request | Idempotency-keyed (§2.4) |

`RECOMMENDATION`: a **shift** (open/close, with an operator-declared
opening and closing cash position) is a real retail concept and the
natural reconciliation unit for physical cash. Its *existence* is
proposed here as the session boundary; its **accounting treatment —
whether a shift close produces ledger entries, and how a cash variance is
posted — is `ledger-finance`'s (ADR 0035), not this document's.**

### 2.4 Idempotency

`BLUEPRINT`-derived invariant (Blueprint §4.2 / `CLAUDE.md`'s financial
rules), applied to retail: **every balance-affecting retail operation is
idempotent via a database unique constraint, not "check then insert"
application logic.**

`ARCHITECTURAL DECISION`: the retail terminal is treated as **a provider
that happens to be ours**. The idempotency key shape mirrors the
already-proven casino-callback constraint
`(tenant_id, provider_id, provider_tx_id)` exactly, as
`(tenant_id, terminal_id, terminal_operation_id)`, where
`terminal_operation_id` is generated by the terminal and is stable across
retries. A retried counter deposit therefore returns the same answer and
posts nothing new, by the same database mechanism that already protects
the bet path.

`ARCHITECTURAL DECISION`: retries are the **normal** case at a counter,
not an exception — a cashier whose network drops mid-transaction will
press the button again, and the physical cash has already changed hands.
The API contract must therefore make "repeat the call with the same
`terminal_operation_id` until you get a definitive answer" the documented,
expected client behaviour, and must return the **original** result (not a
duplicate error) on a repeat, so the terminal can print the correct
receipt. Note this is a *stronger* requirement than the casino callback
path, where a duplicate can simply be a no-op; here the client genuinely
needs the original outcome back.

`RECOMMENDATION` (for `ledger-finance` and `backend`): the concurrency
protection should mirror `internal/casino.postBet`'s Stage-4G-FINAL
`pg_advisory_xact_lock` scoped to the idempotency tuple, acquired before
the idempotency check — because Stage 4G-FINAL proved empirically that
the idempotency short-circuit alone reliably serializes only *sequential*
redeliveries, and two truly concurrent deliveries could each re-evaluate
live state and diverge. A double-tap on a counter terminal is exactly
that race.

### 2.5 Offline / connectivity

`ARCHITECTURAL DECISION` for the design baseline: **the platform is
authoritative and online-required for every balance-affecting operation.
No store-and-forward acceptance of wagers, deposits or payouts.** A
terminal that cannot reach the platform may operate only in a read-only
degraded mode (reprint a receipt already issued and cached locally,
display a previously-returned result, display static content). It may
**never** accept a stake, credit a wallet, or authorize a payout on
locally-held state.

Justification: an offline-accepted wager requires the terminal to
authorize money against a balance it cannot verify, which directly
violates `CLAUDE.md`'s "the authoritative balance read happens inside the
same database transaction as the write" and "Redis (or any cache) never
holds an authoritative balance and is never read on the bet/settlement
path" — a terminal-local cache is just a cache that happens to be in a
shop. It also creates an unbounded late-arrival window in which RG
self-exclusion, risk limits and KYC blocks are unenforceable, which
`internal/rg`/`internal/risk`'s fail-closed contracts (ADR 0031 §6) are
explicitly designed to prevent.

`OPEN DECISION` (business + compliance, for the **human** via the
Orchestrator): **offline/store-and-forward retail betting is a genuinely
common commercial expectation in LATAM retail networks**, particularly
for sports betting where connectivity at a small outlet is unreliable. If
the business requires it, it is **not** a small extension: it needs its
own ADR, its own risk-acceptance from `ledger-finance` (a wager accepted
offline is a liability the ledger did not know about), its own
`identity-compliance` position (RG/self-exclusion cannot be enforced at
acceptance time), and almost certainly a hard per-terminal offline
exposure cap with a physical-ticket fallback. **This document does not
design it and recommends it stay out of any first retail scope.**

### 2.6 What is explicitly out of scope here

No POS hardware protocol, device driver, printer/ESC-POS command set,
ticket barcode symbology, peripheral integration, or shop-level network
topology is designed in this document, per the directive. The
platform-side contract is an ordinary REST+JWT API per
`04-api-architecture.md`, versioned, with the platform's standard error
shape and the idempotency semantics above. Whatever hardware or vendor
POS sits in front of it is an integration detail for a later stage and
would follow `ADR 0004`'s provider-abstraction pattern if a third-party
POS vendor is ever involved.

---

## 3. Player registration and account management through retail

### 3.1 No new identity model

`ARCHITECTURAL DECISION`: **a player registered at a retail counter is an
ordinary `Person` + `PlayerAccount` under a `(tenant_id, brand_id)` pair.
Nothing about the existing identity model changes.** Specifically:

- ADR 0027's resolution boundary applies **unchanged**: retail
  registration calls
  `identityresolution.RegisterPlayerWithResolution`, and the
  `Match` / `NoMatch` / `Uncertain` / `ErrResolverUnavailable` dispatch —
  including landing an ambiguous registration in
  `identity_review_required` — is the same code path, with no retail
  special case. A retail-registered player who is self-excluded at
  another brand must be caught by the **same** mechanism, or the
  cross-brand protection ADR 0027 exists to provide has a hole shaped
  exactly like a shop counter.
- ADR 0012's `(brand_id, tenant_id)` composite FK applies unchanged. A
  retail shop sells under a brand; it does not create a parallel
  "retail brand" concept.
- `05-identity-architecture.md`'s `Person` remains platform-wide and
  `PlayerAccount` remains tenant-scoped. Retail adds no cross-tenant read
  capability (the same statement ADR 0027 §10 makes).

### 3.2 The one genuinely new thing: origin attribution

A retail-originated account needs to record **which node, which terminal
and which cashier created it** — for commission attribution, for fraud
investigation, and because a network manager legitimately needs to see
"players my subtree registered".

`RECOMMENDATION` (this is the choice §8 Conflict B turns on): record this
in a **new, retail-owned table `retail_player_origins`** with a UNIQUE
constraint on `player_account_id`, rather than adding an
`origin_channel`/`origin_node_id` column to `player_accounts`.

Rationale:
- `player_accounts` is Identity-owned, prior-approved, and carries
  security-reviewed RLS. Adding columns to it is a cross-domain schema
  change requiring `identity-compliance` sign-off through
  `integration-protocol.md`'s dependency-request procedure — which is
  fine if justified, but here it is avoidable.
- Per `docs/governance/ownership.md` rule 3, a new table with a read-only
  FK **into** `player_accounts` is owned by the creating domain and does
  not require Identity to co-review, whereas a **new constraint on
  `player_accounts` itself** does.
- It keeps "retail exists" out of the core identity schema entirely, so
  an online-only tenant's `player_accounts` rows are byte-identical to
  today's.

`OPEN DECISION` (for `identity-compliance` to resolve in their Wave-1
document, **not** here): whether a first-class
`player_accounts.registration_channel` (`online` | `retail` | …) is
nonetheless warranted because **compliance rules may differ by
registration channel** and compliance logic should not have to join a
retail-owned table to know how a player was onboarded. There is a real
argument each way. If identity-compliance wants the column, that is their
call on their table, and this document defers to it — it simply must be
an explicit, recorded decision rather than an accident.

### 3.3 KYC touchpoint — flagged, deliberately not designed here

`ARCHITECTURAL DECISION`: **this document invents no KYC rule, no KYC
tier, no threshold, and no RG rule for retail.** Per `CLAUDE.md`, KYC/AML/
RG are one compliance subsystem owned by `identity-compliance`. The
touchpoints are named here so their document can resolve them:

1. **Retail registration is the first plausible real source of
   `VerifiedAttributes`.** ADR 0027 §8 and ADR 0028 §7 both record the
   same open gap: *every registration reachable over the live HTTP API
   resolves `NoMatch` today, because no trusted verification source
   exists.* A counter registration involves **physical presentation of a
   government ID to an accountable, authenticated staff member** — which
   is, in principle, a stronger evidence source than anything the online
   flow currently has. Whether that evidence is trustworthy enough to
   populate `VerifiedAttributes` (and under what controls, given the
   obvious agent-collusion risk) is squarely `identity-compliance`'s
   decision. **Flagged as a significant, positive cross-domain
   opportunity; not decided here.**
2. **Does a retail-originated account get a different KYC tier, limit set,
   or verification deadline?** `identity-compliance`'s call.
3. **Is cash-at-counter a higher AML risk category requiring different
   thresholds?** `identity-compliance` + the human (it is partly a legal
   question).
4. **RG at the counter**: `rg.EvaluateEligibility` must be called before
   any retail operation that lets a player gamble or moves their money —
   this is not a new rule, it is ADR 0027's own explicit *"Forward note
   for future gambling/money endpoints"*: any future endpoint that can
   move money or let a player gamble MUST call `EvaluateEligibility`, and
   must never be assumed covered by checking `PlayerAccount.Status` some
   other way. **Retail endpoints are exactly the endpoints that note was
   written for.** Recorded here as a binding architectural invariant on
   any future retail implementation; the RG *rules* remain
   identity-compliance's.
5. **Self-exclusion at a physical location** (a self-excluded person
   walking into a shop, where there is no login to block) has no analogue
   in the online model. `identity-compliance` + human.

### 3.4 Account management at the counter

`ARCHITECTURAL DECISION`: any account-management action a cashier can
perform (password reset assistance, contact update, document capture) is
the **same server-side operation, under the same RBAC and the same audit
requirements**, as the equivalent back-office action — reached through the
retail API surface rather than reimplemented. No retail-specific account
mutation logic exists. `CLAUDE.md`'s "every mutating administrative action
writes an audit record (actor, tenant, entity, before/after, IP, reason
code)" applies with the addition of node and terminal context (§8,
Conflict F).

---

## 4. Money movement between hierarchy nodes — authorization shape only

**Scope boundary, stated plainly:** this section describes **what moves,
between whom, and who is permitted to authorize it**. It contains **no
account types, no postings, no debit/credit treatment, no balance model,
and no invariant about sums.** All of that is `ledger-finance`'s
exclusive ownership per the precedent set in ADR 0032 and restated in ADR
0035 (expected, Wave 1). Where this document needs to refer to a monetary
effect, it refers to it by business name only.

### 4.1 What needs to move (business vocabulary, not accounting)

| Movement | Direction | Description |
|---|---|---|
| **Float advance** | ancestor → descendant | An upstream node makes spending capacity available to a downstream node so the downstream node can pay out winnings and credit players without holding its own reserves. |
| **Float return** | descendant → ancestor | The reverse: unused capacity returned upstream. |
| **Settlement / remittance** | descendant ↔ ancestor | Periodic squaring-up of the net position between two adjacent nodes over a period. |
| **Commission** | platform/ancestor → node | Consideration earned by a node, typically on turnover, GGR or NGR of activity attributed to its subtree. |
| **Counter deposit (cash-in)** | player-facing, at a node | A player hands cash to a cashier; the player's wallet gains spendable value. |
| **Counter payout (cash-out)** | player-facing, at a node | The reverse. |
| **Adjustment / correction** | any permitted pair | A corrective movement, always with reason code and (above a threshold) four-eyes — `CLAUDE.md`'s existing manual-adjustment rule applies unchanged. |

`OPEN DECISION` (business/legal, for the **human**, flagged as material):

- **Is a float advance a credit line (creating a receivable the agent
  owes) or strictly prefunded (agent must pay first)?** These are
  completely different businesses with completely different accounting,
  credit-risk and insolvency exposure. `ledger-finance` cannot design ADR
  0035's treatment without this answer, and the architect must not guess
  it.
- **Are player funds ever held by the agent rather than the platform?** If
  yes, that is client-money handling by a third party and is heavily
  regulated in most jurisdictions.
- **Is commission regulated or tax-withheld per jurisdiction?** In several
  LATAM markets agent commission is subject to withholding at source. If
  so, commission is a compliance-configured, jurisdiction-varying
  calculation, not a commercial setting — which changes where it belongs
  architecturally. **This is a compliance/legal question, not the
  architect's to answer.**

### 4.2 The authorization model (this document's actual deliverable here)

`ARCHITECTURAL DECISION` — a movement between node A and node B is
permitted only if **all** of the following hold. Every one of them is
evaluated server-side, inside the same transaction as the effect, and
fails closed on any error (the same contract as `risk.Evaluate`, ADR 0031
§6):

1. **Same tenant.** `A.tenant_id = B.tenant_id`, and that tenant matches
   the authenticated connection's tenant context. Never client-supplied.
2. **Same network.** `A.network_id = B.network_id`. Two networks under one
   tenant do not exchange value implicitly.
3. **Structural adjacency or ancestry**, per the movement kind's own
   policy — resolved against the **closure table** (§1.3), not a
   client-supplied parent claim:
   - *Default*: float advance/return and settlement are permitted only
     along a **direct parent-child edge** (depth = 1). Skipping a level
     (an operator funding a shop directly, bypassing the agent) is a real
     commercial request and a real fraud vector; it is permitted **only**
     by an explicit `hierarchy_funding_policy` row, never by default.
   - Commission attribution may reference any ancestor in the subtree
     (depth ≥ 1), since commission is earned on subtree activity.
   - **Sibling/lateral movement is forbidden by default** and requires an
     explicit policy row.
4. **Capability.** Both node types carry the relevant capability
   (§1.5), narrowed by the node's `jurisdiction_code` if a
   jurisdiction-scoped capability row applies.
5. **Node status.** Both nodes are active and within their effective date
   range. A suspended node can receive a settlement but cannot originate
   a float advance or a counter operation — `RECOMMENDATION`, exact
   semantics per movement kind to be fixed with `ledger-finance`.
6. **Actor authorization.** The authenticated principal holds the
   permission for that movement kind **and** has an active assignment
   giving them authority over the originating node — specifically, the
   principal's assigned node must be an **ancestor-or-self** of the
   originating node in the closure table. A cashier at Shop 1 can never
   originate a movement at Shop 2, even within the same agent.
7. **Risk.** The movement is evaluated by **`internal/risk.Evaluate`** —
   never a new limit engine. This is the direct application of the
   precedent `02-domain-and-service-boundaries.md` already records for
   Gamification ("it has no limit engine, no threshold, no cap, no
   counter, and no velocity concept of its own"). Retail float limits,
   per-cashier payout ceilings, daily counter-deposit caps and
   cash-structuring thresholds are **risk rules**, evaluated in the same
   transaction as the effect, fail-closed on error.
8. **RG/KYC.** For any **player-facing** movement (counter deposit/
   payout), `rg.EvaluateEligibility` runs **first**, then `risk.Evaluate`,
   in that fixed order with RG short-circuiting on denial — ADR 0031 §1's
   established ordering, not a new one.
9. **Four-eyes above a configurable threshold** for adjustments and
   (likely) for large float movements — reusing
   `internal/withdrawal`'s existing distinct-approver / non-beneficiary /
   Person-linkage machinery rather than inventing a second approval
   system. `RECOMMENDATION`; thresholds are `security`'s and the
   business's.
10. **Audit.** Every movement writes an audit record carrying actor,
    terminal, both node ids, tenant, amounts, reason code and IP
    (`CLAUDE.md`'s mutating-financial-action rule).

`ARCHITECTURAL DECISION`: checks 1–6 are **retail/agent-network's own
authorization logic**; checks 7–10 are **calls into existing domains that
retail must not reimplement**. A future `internal/retail` package that
contains its own limit counter, its own eligibility check, or its own
audit writer has violated this boundary.

### 4.3 Risk integration requires an extension (flagged, not made)

`RECOMMENDATION` requiring `risk` specialist sign-off via ADR 0031 §12's
documented extension process — **not** done here: `risk.Operation`
currently accepts `casino_launch | casino_bet | deposit | withdrawal |
sportsbook_bet | bonus_grant`. Retail needs new operation values (e.g.
`retail_counter_deposit`, `retail_counter_payout`,
`retail_float_advance`, `retail_settlement`). ADR 0031 §4 is explicit
that a rule the evaluator cannot evaluate must not be configurable in the
first place, so this is a deliberate, reviewed extension, never an ad hoc
enum addition. Retail may also want the `count`/`velocity` limit kinds
ADR 0031 §4 deliberately did **not** implement (cash structuring is a
count/velocity problem) — that is ADR 0031 §12's five-step extension
process, owned by `risk`.

### 4.4 Explicit non-deliverable

Nothing in §4 states, implies, or constrains how a float advance is
posted, whether an agent float is a wallet, what account types exist, or
whether `SUM(DEBITS) == SUM(CREDITS)` is maintained by a particular
entry shape. **ADR 0035 (`ledger-finance`) is authoritative on all of
it**, and if anything in this section is inconsistent with ADR 0035,
**ADR 0035 wins** and this document is corrected — the same precedence
ADR 0032 established for bonus accounting.

---

## 5. Conceptual ERD — entities, relationships, RLS scoping

All tables **`NOT IMPLEMENTED`**. Names are indicative. No migration is
proposed by this document. **No retail financial account/balance table
appears below — that is `ledger-finance`'s (ADR 0035).**

### 5.1 Entity list

| # | Entity | Purpose | Key relationships | RLS scoping dimension |
|---|---|---|---|---|
| 1 | `hierarchy_node_types` | Definitional template: what kinds of node exist | — | **Dual-scope** (`tenant_id` NULL = platform-wide template; non-NULL = tenant-specific). Mirrors `risk_rules` / `point_types`. |
| 2 | `hierarchy_node_type_relations` | Which type may parent which type; optional min/max children | 2→1 (parent_type), 2→1 (child_type) | **Dual-scope**, optionally narrowed by `network_id` and `jurisdiction_code`. Most-specific-wins per H6. |
| 3 | `hierarchy_node_capabilities` | What a node type may do (`register_player`, `counter_deposit`, `counter_payout`, `fund_child`, `settle_with_parent`, `earn_commission`) | 3→1 | **Dual-scope**, optionally narrowed by `jurisdiction_code`. |
| 4 | `hierarchy_networks` | A named network instance; a tenant may run several (e.g. one per licence/jurisdiction/brand) | 4→`tenants`, optional 4→`brands`, optional 4→`jurisdictions` | **Tenant-scoped** (`tenant_id NOT NULL`). |
| 5 | `hierarchy_nodes` | **The tree.** `parent_node_id` self-FK (NULL = root), `node_type_id`, `status`, `jurisdiction_code`, `effective_from/to`, `external_ref` | 5→4, 5→1, 5→5 (self) | **Tenant-scoped**, plus a **node-subtree dimension** for node-scoped readers (see §8 Conflict A). Composite FK on `(parent_node_id, tenant_id, network_id)` per H5. |
| 6 | `hierarchy_node_closure` | Derived projection: `(ancestor_node_id, descendant_node_id, depth)`, including depth-0 self rows | 6→5 ×2 | **Tenant-scoped.** Derived, never authoritative (H1–H3). |
| 7 | `hierarchy_node_staff_assignments` | N:M human↔node with effective dating and an assignment role | 7→5, 7→`staff_users` | **Tenant-scoped**, node-scoped for node-scoped readers. |
| 8 | `retail_terminals` | A registered terminal/POS bound to exactly one node; status, label, credential reference (**never a secret value**) | 8→5 | **Tenant-scoped** + node-scoped. |
| 9 | `retail_terminal_sessions` | Cashier shift session at a terminal: open/close, declared opening/closing cash position | 9→8, 9→`staff_users` | **Tenant-scoped** + node-scoped. |
| 10 | `retail_player_origins` | Attribution: which node/terminal/cashier registered a player account. `UNIQUE(player_account_id)` | 10→`player_accounts` (read-only FK), 10→5, 10→8 | **Tenant-scoped.** Node dimension used for subtree-scoped reads (see §8 Conflict B). |
| 11 | `hierarchy_funding_policies` | Which movement kinds are permitted between which node pairs/types, beyond the default parent-child rule; approval threshold reference | 11→1 or 11→5 | **Tenant-scoped**, optionally `jurisdiction_code`-narrowed. |
| 12 | `hierarchy_commission_plans` | Commercial terms attached to a node or node type (basis, rate schedule, effective dating) — **the agreement, not the money** | 12→5 / 12→1 | **Tenant-scoped.** *See the P1 risk in §9 about the boundary with ADR 0035.* |
| 13 | `retail_operation_requests` | The idempotency record for a counter operation: `(tenant_id, terminal_id, terminal_operation_id)` UNIQUE, plus the stored canonical response | 13→8 | **Tenant-scoped** + node-scoped. |

Entities **deliberately absent** because they belong to `ledger-finance`
(ADR 0035): any node float balance, node ledger account, settlement
statement with monetary totals, commission accrual/payable, or cash
drawer balance. If ADR 0035 concludes that entity 12 (`commission_plans`)
is also monetary and therefore theirs, **this document yields it** — see
§9, R-P1-3.

### 5.2 Relationship sketch

```
tenants ──┬─< hierarchy_networks ──< hierarchy_nodes ──< hierarchy_nodes (self, parent_node_id)
          │                              │  │  │
          │                              │  │  └─< hierarchy_node_closure (derived, H1–H3)
          │                              │  └──< hierarchy_node_staff_assignments >── staff_users
          │                              └─────< retail_terminals ──< retail_terminal_sessions >── staff_users
          │                                             └──< retail_operation_requests
          ├─< brands ──< player_accounts ──1:1── retail_player_origins >── hierarchy_nodes
          └─< (existing tenant-owned tables, unchanged)

hierarchy_node_types (dual-scope) ──< hierarchy_node_type_relations (dual-scope)
                                  └──< hierarchy_node_capabilities (dual-scope, jurisdiction-narrowable)

jurisdictions ──< hierarchy_nodes.jurisdiction_code   (see §8 Conflict G — a positive finding)
```

---

## 6. Shared online + retail domain model

`ARCHITECTURAL DECISION` (the single most important statement in this
document): **retail is a new set of actors and a new set of entry points
over the existing platform core. It is not a parallel platform.** The
default answer for every existing domain is REUSE AS-IS, and every
EXTENSION below has to justify itself.

| Existing domain | Verdict | Detail |
|---|---|---|
| **Ledger** (`internal/ledger`) | **REUSE AS-IS** for its invariants and posting engine; **`ledger-finance` determines** whether new account types/entities are needed | Retail introduces **zero** new money-movement mechanisms. It introduces new *reasons* to post. Append-only, double-entry, idempotent, projection-not-authoritative: all unchanged. Whether a node-held float is expressible in today's account-type set is **ADR 0035's call, not this document's.** |
| **Wallet** (`internal/wallet`, ADR 0007) | **EXTENSION — `ledger-finance` owned, flagged as this document's biggest open dependency** | ADR 0007's `Wallet` is explicitly *per player, per asset*. A hierarchy node that holds a float is a balance-bearing entity that is **not a player**. Whether that is a wallet with a nullable/polymorphic owner, a distinct construct, or a house-account pattern (the model already has non-player `house_gaming` accounts, so precedent exists) is **ADR 0035's decision.** Flagged, not decided. See §9 R-P0-1. |
| **Player accounts / Person** (`internal/identity`, ADR 0012, ADR 0027) | **REUSE AS-IS**, plus one new *retail-owned* attribution table | §3. No column added to `player_accounts` by this proposal; ADR 0027's resolution flow unchanged; `Person` stays platform-wide. |
| **Staff / RBAC** (`internal/identity.StaffUser`, `internal/auth`) | **EXTENSION** — new roles + new permissions + a new *retail-owned* assignment table | §2.1. The `staff_users` table shape itself does **not** change; only the role enum/CHECK (Identity-owned) and the permission map (security-owned). |
| **Auth / sessions / tokens** (`internal/auth`, ADR 0014, 0016, 0017, 0018) | **REUSE AS-IS** | Terminal = `service` principal per ADR 0014 option 2, which retail makes real for the first time. No new principal type, no new session table, no new token type. MFA/step-up applies unchanged. |
| **Risk** (`internal/risk`, ADR 0031) | **EXTENSION** — new `Operation` values, possibly new limit kinds | §4.3. Retail builds **no** limit engine. Extension via ADR 0031 §12's process, owned by `risk`. |
| **Responsible Gaming** (`internal/rg`, ADR 0026) | **REUSE AS-IS** for the mechanism; **`identity-compliance` decides** whether physical-presence self-exclusion needs new rules | `EvaluateEligibility` called at every retail player-facing operation, per ADR 0027's own forward note. The *rules* are identity-compliance's. |
| **KYC** (`internal/kyc`, ADR 0028/0029) | **REUSE AS-IS** for the mechanism; **`identity-compliance` decides** tiers and the counter-evidence question | §3.3. A significant opportunity (first real `VerifiedAttributes` source) flagged to them. |
| **Tenant / Brand / Jurisdiction** (ADR 0012, ADR 0006, doc 15) | **REUSE AS-IS** | §8 Conflict G: a node carries a `jurisdiction_code` resolving against the **existing** tenant-level `TenantJurisdictionConfig`. No change to doc 15 or ADR 0012 is required by this proposal. |
| **Payments** (`internal/payments`, ADR 0022) | **EXTENSION — `payments` owned** | Cash-at-counter is arguably a payment method with no PSP. Whether it is modeled as a `PaymentProvider` capability or as a distinct retail movement is **payments' + ledger-finance's** joint call, not this document's. Agent remittance rails likewise. |
| **Bonus Engine** (ADR 0032, doc 10) | **REUSE AS-IS** | A bonus granted to a retail-registered player is an ordinary grant. Retail introduces no bonus mechanism. `OPEN DECISION` for `bonus-engine`: whether a bonus may ever be *issued by a cashier* (an obvious collusion vector) — flagged, not designed. |
| **Gamification** (docs 17–20) | **REUSE AS-IS** (and itself `NOT IMPLEMENTED`) | Retail activity, once it emits canonical events (doc 22), is just activity. No retail-specific gamification concept is proposed. |
| **Reward Orchestrator** (doc 21) | **REUSE AS-IS** | A retail-fulfilled reward, if ever needed, is a fulfilment destination — doc 23's `ExternalRewardProvider` shape already covers the concept. Not designed here. |
| **Audit** (`internal/audit`, ADR 0013) | **EXTENSION** — needs node/terminal context | §8 Conflict F. Requires architect + security sign-off per ownership.md. |
| **Event taxonomy** (doc 22) | **EXTENSION** — new canonical activity events | Retail operations must emit canonical, provider-neutral events with doc 22's `event_id` vs. `idempotency_key` distinction respected. Nothing retail-specific leaks into consumers. |
| **Reporting** (doc 12) | **EXTENSION** — a node-subtree dimension | Every retail report is "…by node subtree". Requires the closure table to be available to the analytics path. `data-analytics`-owned. |
| **Hierarchy / agent network itself** | **GENUINELY NEW** | Entities 1–13 in §5. This is the only genuinely new domain retail introduces. |

**Summary**: of 16 existing domains, **8 are reused as-is, 7 need a
bounded extension owned by their own specialist, and exactly 1 new domain
is created.** That ratio is the test of whether "one platform, not two
disconnected products" is real. If a future implementation finds itself
building a second wallet, a second eligibility check, a second limit
engine or a second audit writer for retail, it has failed this document's
central decision.

---

## 7. Package / domain ownership recommendation

### 7.1 Package split

`RECOMMENDATION` — **two packages, not one**, split along the money
boundary this codebase already enforces everywhere else:

| Package | Contains | Contains no |
|---|---|---|
| `internal/agentnetwork` | The hierarchy primitive: nodes, types, relations, capabilities, closure maintenance and reconciliation, containment queries, structural authorization (§4.2 checks 1–6), node↔staff assignments | Money, ledger imports, POS concepts |
| `internal/retail` | Retail operational surface: terminal registration, shift sessions, counter-operation orchestration, idempotency records, player-origin attribution — composing `agentnetwork` + `identity` + `rg` + `risk` + `ledger` | Its own limit engine, its own eligibility logic, its own audit writer, its own balance |

Rationale for the split (and for the `agentnetwork` name): **the
hierarchy is not inherently a retail concept.** A B2B sub-operator tree,
an affiliate/sub-affiliate structure, and a white-label reseller chain are
all the same graph with different node types. Naming it `retail` would
guarantee that the second consumer either duplicates it or imports a
package whose name lies. This is the same reasoning that made `Brand`
distinct from `Tenant` (ADR 0012) and `PointType` a registry rather than a
hardcoded currency.

Counter-argument acknowledged (`product-owner-proxy`'s standing concern,
and a fair one): two packages for a domain with one consumer is
speculative. **Mitigation**: the split is a package boundary, not a
service boundary or a second deployable — ADR 0010's single-deployable
decision is untouched — and it costs one directory. If the Orchestrator
prefers a single `internal/retail` with an internal `hierarchy`
sub-package, that is an acceptable, reversible simplification and this
document does not object.

### 7.2 Ownership

`RECOMMENDATION` for the Master Orchestrator's governance update
(`docs/governance/ownership.md` is Orchestrator/architect territory;
**this document does not edit it**):

- **No existing specialist owns this domain.** Retail/agent-network maps
  onto no entry in the current ownership table.
- **Architecture ownership → `architect`**, per the **explicit precedent
  set in Stage 4H-A for Gamification**, already recorded in
  `02-domain-and-service-boundaries.md`'s services table as
  *"gamification (architecture by architect)"*. Retail is in exactly the
  same position: a new cross-cutting domain touching identity, ledger,
  risk, RG, audit and tenancy, with no domain specialist who could own it
  without needing everyone else's sign-off for routine changes.
- **Implementation ownership → `OPEN DECISION` for the Orchestrator.**
  Two viable options:
  1. Assign `backend` (consistent with how `tenant-config` and the
     back-office APIs are assigned in doc 02), with mandatory
     `ledger-finance` sign-off on anything monetary and `security`
     sign-off on the terminal/cashier auth model.
  2. Create a new `retail` specialist agent under `.claude/agents/`, on
     the same grounds `risk` and `bonus-engine` were created — the domain
     is large enough to have its own standing invariants.
  **`RECOMMENDATION`: option 2 if retail is ever actually authorized for
  implementation; option 1 is adequate while it remains architecture
  only.** Either way, the boundary rule is fixed: **`ledger-finance` owns
  every monetary table and every posting, unconditionally** (ADR 0032's
  precedent, restated in ADR 0035).
- **Migrations**: per ownership.md rule 3, `agentnetwork`/`retail` own
  their own tables' migrations; any migration adding a constraint to
  `player_accounts`, `staff_users` or `wallets` requires that domain's
  co-sign.

---

## 8. Critical conflict analysis — does prior-approved architecture need to change?

The directive requires an explicit check of Stages 0–4H-A's approved
architecture, and requires that any genuine conflict be **flagged for
Master Orchestrator + affected-specialist sign-off, never silently
redesigned.** Nine areas were examined. Findings below, each with its
required sign-off.

---

### Conflict A — RLS needs an intra-tenant subtree dimension `RECOMMENDATION` (architect + `security`)

**Finding.** Every tenant-owned table today is scoped by exactly one
dimension: `tenant_id`, via `app.tenant_id` (ADR 0002, doc 03). Retail
introduces a genuine second dimension **inside** a tenant: a super agent
must see its own subtree and **not** a sibling super agent's, even though
both are the same tenant. `tenant_id` alone cannot express this.

**This is not unprecedented.** `internal/db` already implements two
additional scoping GUCs beyond tenant: `app.principal_id`
(`WithPrincipalScope`, ADR 0016, for `sessions`) and
`app.player_account_id` (`WithPlayerScope`, ADR 0019, for wallet/ledger
self-service). The pattern is established and security-reviewed.

**Proposal.** A third scoping helper, `WithNodeScope(tenantID, nodeID)`,
setting `app.tenant_id` + `app.hierarchy_node_id`, with node-scoped
policies of the shape:

```
EXISTS (SELECT 1 FROM hierarchy_node_closure c
        WHERE c.descendant_node_id = <row>.node_id
          AND c.ancestor_node_id = current_setting('app.hierarchy_node_id', true)::uuid)
```

**The specific trap that must be avoided** — and the reason this is
flagged rather than assumed: ADR 0019 documents that **Postgres ORs every
applicable permissive policy together**, so a player-scoped connection
would *also* satisfy a plain tenant-match policy unless the tenant-staff
policy explicitly requires `app.player_account_id` to be **unset**. The
identical hazard applies here: a node-scoped connection must **not** also
satisfy the plain tenant policy, or the subtree isolation this exists to
provide is silently defeated. Every retail table's tenant-staff policy
must therefore be split on "is `app.hierarchy_node_id` unset", exactly as
Stage 3B's migrations did for `app.player_account_id`.

**`internal/db` is owned by architect + security** (ownership.md). This
proposal therefore **requires `security` sign-off before any
implementation**, and is explicitly not a unilateral architect change.

**Severity**: P1. It is additive (no existing policy changes), but
getting the permissive-policy OR-widening wrong is a silent cross-subtree
data leak.

---

### Conflict B — does `player_accounts`' RLS need a node dimension? `OPEN DECISION` (`identity-compliance` + `security`)

**Finding.** A network manager legitimately needs "players registered in
my subtree". The obvious implementation is a node-scoped permissive
policy on `player_accounts`. **`player_accounts` is Identity-owned,
prior-approved, and security-reviewed.**

**`RECOMMENDATION` (architect's position, offered for their decision, not
imposed): do NOT add a node dimension to `player_accounts`' RLS.**
Instead, retail-scoped reads join from the retail-owned
`retail_player_origins` (node-scoped) to `player_accounts` on a normal
tenant-scoped connection, with the subtree check applied to the
retail-owned side.

Reasons:
1. Adding a second permissive policy to `player_accounts` is exactly the
   OR-widening hazard in Conflict A, on the **single most
   security-sensitive table in the identity domain**.
2. It keeps retail out of the core identity schema entirely — an
   online-only tenant's `player_accounts` RLS stays byte-identical.
3. It preserves ADR 0027 §10's clean statement that identity resolution
   grants no new data-access capability.

**This is `identity-compliance`'s table and their call.** Flagged as an
OPEN DECISION requiring their sign-off either way. If they prefer the
column+policy approach, that decision should be recorded in their Wave-1
document and this document corrected.

**Severity**: P1 if resolved carelessly; P2 if resolved deliberately
either way.

---

### Conflict C — `staff_users` `RECOMMENDATION` (`identity-compliance`), no structural conflict

**Finding.** Migration 0011's CHECK ("every non-`platform_admin` role
belongs to exactly one tenant") is **satisfied** by retail roles, which
are always tenant-scoped. No structural change is needed.

Two bounded changes are needed and both belong to other owners:
(a) new `StaffRole` values → Identity's enum/CHECK constraint;
(b) new permissions and their role grants → `security`'s permission map.

**Deliberate design choice avoiding a third**: node assignment lives in a
**new retail-owned table**, not a `staff_users.node_id` column — so
`staff_users`' shape is unchanged, and a staff member's assignment history
is effective-dated and multi-valued rather than a single mutable column.

**Severity**: P2, routine cross-domain dependency request.

---

### Conflict D — the wallet model is genuinely player-centric `OPEN DECISION` (`ledger-finance`, ADR 0035)

**Finding — the most consequential item in this analysis.** ADR 0007 and
`06-wallet-ledger-architecture.md` define `Wallet` as **per player, per
asset** (`wallet_id, tenant_id, player_id, asset_code`), and `CLAUDE.md`
restates it as a permanent rule. A hierarchy node holding a float is a
**balance-bearing entity that is not a player**.

Precedent exists in both directions and this document deliberately
**takes no position**: the ledger model already supports non-player-owned
accounts (`house_gaming` is described as "aggregated per tenant+asset
rather than per player wallet", with a nullable `wallet_id`), so a
node-level balance may fit today's account model without any change to
ADR 0007 — **or** it may warrant a distinct construct.

**This is `ledger-finance`'s exclusive decision under ADR 0032's
precedent and ADR 0035's scope.** The architect's only contribution is to
flag it loudly: **if ADR 0035 concludes that ADR 0007 or
`06-wallet-ledger-architecture.md` needs amendment, that is an amendment
to a human-approved decision** (ADR 0007 is explicitly labeled "human
decision… not discretionary") and therefore needs Master Orchestrator +
human sign-off, not a specialist's unilateral edit.

**Severity**: **P0** — not because anything is wrong today, but because
this is the one place where retail could silently contradict a
human-approved architectural decision if nobody looks.

---

### Conflict E — `risk.Operation` enum `RECOMMENDATION` (`risk`)

Covered in §4.3. Additive, via ADR 0031 §12's documented extension
process. **Severity**: P2.

---

### Conflict F — audit records need node/terminal context `RECOMMENDATION` (architect + `security`)

**Finding.** `audit.Entry` carries `ActorType`, `ActorID`, `TenantID`,
`Action`, `TargetType`, `TargetID`, plus request/IP/user-agent metadata.
It has **no node or terminal dimension**. Every retail audit record needs
both — "which shop, which terminal" is the first question any retail
fraud investigation asks, and `CLAUDE.md` requires actor/tenant/entity/
before-after/IP/reason-code on every mutating financial action.

Two options: encode node/terminal in the existing metadata payload (no
schema change, but unqueryable as a first-class dimension), or add
first-class nullable columns. **`internal/audit` is a shared primitive
whose own code requires architect + security sign-off** (ownership.md).

`RECOMMENDATION`: first-class nullable columns, because retail
investigations and regulator requests are node-filtered by nature and a
JSON-payload filter on an append-only 5–7-year table is a reporting
liability. **Flagged for security's Wave-1 document; not decided here.**

**Severity**: P2 (cheap now, expensive after the table is large — ADR
0027's own note about `ACCESS EXCLUSIVE` locks at production table size
applies).

---

### Conflict G — jurisdiction: **no conflict, and a positive finding** `ARCHITECTURAL DECISION`

**Finding.** ADR 0012 deliberately kept `TenantJurisdictionConfig`
tenant-scoped rather than brand-scoped ("no current consumer… would be
speculative"). Retail is intrinsically geographic — a shop has a physical
address in exactly one jurisdiction — so this is a natural pressure point.

**It turns out not to be a conflict.** A `hierarchy_nodes.jurisdiction_code`
FK to `jurisdictions.code` resolves against the **existing** tenant-level
`TenantJurisdictionConfig` row for that jurisdiction. No change to doc 15,
ADR 0006 or ADR 0012 is required.

**And it closes a real, documented gap.** Doc 15 and ADR 0031 §9 both
record that the platform has **no per-player jurisdiction resolver**
(`TODO(jurisdiction)`), so `RiskRequest.JurisdictionCode` is populated
only where a caller happens to have resolved it, and "a real production
launch persists no jurisdiction today" (`docs/active-stage.md`, Stage
4G-FINAL Part C). **A retail node is the first authoritative,
non-guessed, non-geolocated source of jurisdiction this platform would
ever have** — a shop's jurisdiction is a registered fact, not an inference
from an IP address. Retail therefore *strengthens* the jurisdiction model
rather than straining it. Worth surfacing to `risk` and
`identity-compliance` as a genuine benefit.

**Severity**: none (positive finding).

---

### Conflict H — ADR 0010 single-deployable: **no conflict** `ARCHITECTURAL DECISION`

Retail adds packages, not deployables. ADR 0010's "one deployable,
organized internally by package, split only when a real scaling/ownership
reason exists" is untouched. A retail terminal calls the same
`platform-api`. §7.1's two-package proposal is explicitly a package
boundary, not a service boundary.

---

### Conflict I — Blueprint contradiction: **none, but an anchor gap** `OPEN DECISION` (Orchestrator)

Retail does not *contradict* the Blueprint; the Blueprint is simply
**silent** on it (§0.1). Nothing in this document reverses a Blueprint
statement. The gap is a **scope-anchor** gap identical to Gamification's,
and per `CLAUDE.md`'s scope-expansion rule it must be **recorded**, not
assumed. `RECOMMENDATION`: the Orchestrator records it in
`14-mvp-scope-and-roadmap.md`.

---

### Summary of required sign-offs

| Item | Owner(s) | Type | Severity |
|---|---|---|---|
| A — node-scoped RLS GUC + policy split | architect + security | RECOMMENDATION | P1 |
| B — `player_accounts` node dimension | identity-compliance + security | OPEN DECISION | P1 |
| C — new StaffRoles + permissions | identity-compliance + security | RECOMMENDATION | P2 |
| D — node float vs. ADR 0007 wallet model | **ledger-finance** (+ human if ADR 0007 is amended) | **OPEN DECISION** | **P0** |
| E — `risk.Operation` extension | risk | RECOMMENDATION | P2 |
| F — audit node/terminal dimension | architect + security | RECOMMENDATION | P2 |
| G — node jurisdiction_code | — | ARCHITECTURAL DECISION (no conflict; positive) | — |
| H — deployable shape | — | no conflict | — |
| I — Blueprint anchor gap | Orchestrator | OPEN DECISION | P2 |

**No previously-approved architecture is changed by this document.**
Every item above is flagged for its owner.

---

## 9. Risk register (architect's own)

### P0

- **R-P0-1 — the node-float/wallet boundary (Conflict D).** If ADR 0035
  and this document disagree about whether a node balance is a `Wallet`,
  the first retail implementation will build a second balance system. That
  is the single failure mode that would make "one platform, not two
  products" false. **Mitigation**: ADR 0035 is authoritative; the
  Orchestrator must verify §6's wallet row against ADR 0035's actual text
  once Wave 1 lands.
- **R-P0-2 — retail is almost certainly not covered by the Anjouan
  licence.** Anjouan is an online licence; land-based/retail gambling is
  licensed locally, country by country, in essentially every target
  market, frequently with local-entity, local-hosting and local-data
  requirements. **Software capability is not regulatory approval**
  (`CLAUDE.md`). Nothing in this document may be read as suggesting the
  platform is licensed to operate retail anywhere. **This is a
  legal/business question for the human**, and it is P0 because it can
  invalidate the commercial premise of the whole domain, not merely its
  design.
- **R-P0-3 — cash is the highest-AML-risk channel in gambling, and the
  platform has no real AML monitoring today** (ADR 0027/0028: no real KYC
  vendor, `VerifiedAttributes` never populated over the live API, resolver
  always returns `NoMatch`). Retail would put cash acceptance on top of a
  compliance subsystem that is honestly labeled `NOT IMPLEMENTED` for
  every real vendor. **This sequencing risk belongs to the human and
  `identity-compliance`.**

### P1

- **R-P1-1 — closure-table drift is an authorization bug, not a reporting
  bug.** Because subtree containment gates both RLS visibility (Conflict
  A) and money-movement authorization (§4.2), a stale closure row is a
  security defect. H2/H3 (same-transaction maintenance + scheduled
  recomputation and diff) are mandatory, and `qa` should treat closure
  drift with the same severity as ledger/projection drift.
- **R-P1-2 — agent collusion and self-dealing is the dominant retail fraud
  vector**, and it is structurally different from online fraud: the
  cashier controls registration, cash, payout and (potentially) KYC
  evidence for the same player. The existing
  `staff_users.person_id` ↔ `BeneficiaryCheck` mechanism covers only one
  narrow case. **`security` should treat this as a first-class threat
  model**, not a permissions detail.
- **R-P1-3 — commission-plan ownership ambiguity.** §5 entity 12
  (`hierarchy_commission_plans`) is proposed as the *agreement*
  (rate schedule, basis, effective dating) with all accrual/payable
  treatment left to ADR 0035. That line is thin and two documents could
  reasonably both claim it. **The Orchestrator should confirm the split
  explicitly once ADR 0035 lands.**
- **R-P1-4 — the offline question (§2.5) may be a hard commercial
  requirement.** If it is, a material part of this design changes and
  several fail-closed contracts need explicit exceptions. Better answered
  before implementation than during it.
- **R-P1-5 — scope.** This document describes 13 new entities and 7
  extensions to existing domains for a domain with **no Blueprint anchor
  and no implemented consumer**. That is a large surface. `product-owner-
  proxy` should review it against the same standard applied to
  Gamification in Stage 4H-A, and should feel free to conclude that a
  first retail scope needs far less than this (e.g. a fixed-depth network
  and counter deposit/payout only, with the configurable-topology tables
  deferred).

### P2

- **R-P2-1** — data residency: physical retail presence in a country
  frequently triggers local data-hosting requirements, which stresses ADR
  0002's isolation-tightening path earlier than planned (the path exists —
  shared+RLS → schema → database → cluster per tenant — but retail may
  force it sooner).
- **R-P2-2** — `hierarchy_node_capabilities` and `risk_rules` can express
  overlapping restrictions ("this node type may not pay out" vs. "max
  payout = 0"). This is the *same* observation doc 02 already records
  about `risk`'s generic `Rule` shape being able to express an RG-shaped
  prohibition. Recorded for consistency; the intended split is
  **capability = structural/what-kind-of-thing-this-is; risk =
  amount/frequency/threshold.**
- **R-P2-3** — re-parenting a node mid-period has real consequences for
  commission attribution and settlement periods that this document does
  not resolve (it resolves only the structural mechanics). Needs
  `ledger-finance` + business input.
- **R-P2-4** — physical cash reconciliation (drawer count vs. system
  position) is a reconciliation stream that does not exist in
  `reconciliation-model.md` today.
- **R-P2-5** — terminal credential compromise has a larger blast radius
  than a player session; revocation latency and per-terminal exposure caps
  are `security`'s to specify.

---

## 10. Human business/legal decisions surfaced by this analysis

Per `CLAUDE.md`'s "when to stop and ask" rule, these are **not the
architect's to answer** and are surfaced to the Orchestrator for the
human. None is answered anywhere in this document.

1. **Is retail licensed anywhere we intend to operate it?** (R-P0-2.)
   Which jurisdictions, under whose licence, and does it require a local
   legal entity?
2. **Are agents/shops independent legal entities, sub-licensees, or our
   own staff?** This determines whether the hierarchy models contracting
   counterparties or internal cost centres — and therefore the entire
   settlement and liability model.
3. **Is agent float a credit line (receivable) or strictly prefunded?**
   (§4.1.) `ledger-finance` is blocked on this.
4. **Is commission jurisdiction-regulated or tax-withheld at source?**
   (§4.1.) If yes, it is compliance configuration, not a commercial
   setting.
5. **Is anonymous / unregistered retail play permitted or required?**
   Retail sports betting commonly uses anonymous cash tickets. This
   platform's entire wallet/ledger/RG/KYC model assumes a `PlayerAccount`
   exists. **If anonymous play is required, that is a fundamental scope
   question affecting ledger, identity, RG and AML simultaneously** — it
   is not an incremental feature, and it should be answered before any
   retail implementation is authorized.
6. **Is offline/store-and-forward betting a hard requirement?** (§2.5.)
7. **Do agents ever hold player funds?** (Client-money regulation.)
8. **Is a third-party POS vendor in scope, or is the terminal ours?**
   (Determines whether ADR 0004's provider-abstraction pattern applies.)
9. **Data residency obligations in target retail markets?** (R-P2-1.)
10. **Cash-handling/AML obligations**: reporting thresholds, structuring
    detection, source-of-funds at the counter. (R-P0-3.)

---

## 11. Related documents

- `docs/architecture/02-domain-and-service-boundaries.md` — Retail /
  Agent Network domain boundary (updated by this stage).
- `docs/architecture/05-identity-architecture.md` — Person/PlayerAccount/
  StaffUser model retail reuses unchanged.
- `docs/architecture/06-wallet-ledger-architecture.md`,
  `docs/decisions/0007` — the wallet model Conflict D turns on.
- `docs/architecture/15-jurisdiction-and-licensing-model.md` — resolved
  against by `hierarchy_nodes.jurisdiction_code` (Conflict G).
- `docs/decisions/0002`, `0012`, `0027`, `0031`, `0014`, `0019`, `0032` —
  the prior decisions this document composes rather than changes.
- **Expected, produced in parallel this wave, not authored here**:
  `docs/decisions/0035-*` (`ledger-finance`, retail monetary accounting),
  `security`'s retail RBAC/terminal-authentication document,
  `identity-compliance`'s retail identity/KYC/RG impact analysis, and
  `payments`' cash/remittance document. **If any of those contradicts
  this document on their own domain, theirs wins and this document is
  corrected.**

# 04 — API Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §3, §7; API-first principle
from the master project rules.

## Principle

The platform is API-first. Brand frontend, back office, partner console,
and external B2B integrations all consume the same class of platform API
— no surface gets private, undocumented business logic. Core business
rules live in backend/domain services, never exclusively in frontend code.

## Shape

- REST + JWT, per the Blueprint's system map (§3), as the Stage-1 baseline.
- OpenAPI specs maintained per service under `docs/api/`, generated from
  or validated against the actual service code (not hand-written and
  left to drift).
- `RECOMMENDATION`: version every public API from the first commit (e.g.
  `/v1/...` or a version header) — B2B partners integrate against these
  contracts and cannot be broken silently once a partner is live.

## Authentication/authorization propagation

- Brand frontend holds a short-lived JWT identifying the player and tenant.
- Game/sportsbook launch mints a separate, single-use, opaque token scoped
  to `(player, provider, game/product, currency, mode)` with a short TTL —
  never the player's session JWT (Blueprint §4.1). Owned by `security` +
  the relevant domain specialist (`casino`/`sportsbook`).
- Back-office/partner-console APIs authenticate staff separately from
  players, with RBAC scoped by tenant and enforced server-side on every
  request — never inferred from which UI element is visible.
- Every internal service-to-service call carries the tenant context
  explicitly; nothing downstream re-derives or trusts a tenant id passed
  as a plain, unauthenticated parameter.

## Inbound provider callbacks

The highest-traffic, most correctness-critical API surface is the inbound
wallet callback path (game/sportsbook provider → platform). It is treated
as a first-class API with its own contract per provider-adapter, gated by
the idempotency constraint in the ledger (see `06-wallet-ledger-
architecture.md`), not a special case bolted onto the general API layer.

## Conventions (RECOMMENDATION, confirm at Stage 1 gate)

- Consistent error shape across all services (machine-readable error code
  + human message), so `frontend`/`backoffice` can build generic error
  handling once.
- Idempotency-key support on every mutating endpoint that can plausibly be
  retried by a client or a provider (not just the ledger's own internal
  mechanism).
- Pagination and filtering conventions fixed once and reused — back office
  screens depend on server-side filtering/pagination for large tables
  (Blueprint §7).

## Ownership

`architect` owns these conventions; each domain specialist implements
against them and raises a proposal to `architect` (not a unilateral
change) if a convention doesn't fit their domain.

---

## Stage 4H-B0 — Retail/POS and Agent Hierarchy API Surface (Architecture/Scope Freeze)

Status: Stage 4H-B0. **ARCHITECTURE/API-SURFACE DESIGN ONLY — `NOT
IMPLEMENTED`.** This section modifies no code, no route table, and no
file under `docs/api/openapi/`. Endpoint paths below are illustrative of
the convention, not a commitment to a specific route table. It creates no
new tenant-isolation mechanism, no new service boundary, and no RBAC
enforcement mechanism — those are `security`'s and `architect`'s parallel
Stage 4H-B0 deliverables, referenced here by name, not redesigned. Source:
new confirmed business requirement — retail iGaming operations (directive
requirement #22, "retail terminals/POS/cashier interfaces must be treated
as clients of the platform APIs, not as separate financial systems") plus
a configurable agent hierarchy (Operator → Partner → Super Agent → Agent →
Player/Cashier).

This section follows the exact conventions doc 25
(`25-bonus-gamification-api-architecture.md`) established for a
forward-looking contract sketch: tenant resolution always server-side,
the existing `internal/apierror` error shape, the `<resource>_config:read`
/ `<resource>_config:manage` separation-of-duties permission pattern, the
existing `/v1/` single-version-prefix convention, and idempotency via a
DB-enforced unique constraint rather than check-then-insert. It does not
restate those ground rules in full — see doc 25 §0 for the shared baseline
this section builds on.

**Explicit assumption (`ASSUMPTION`, to reconcile at implementation
time):** this document was authored in parallel with two sibling Stage
4H-B0 deliverables this specialist does not own: `security`'s hierarchy
RBAC/audit design (as of writing, expected at `docs/decisions/0036-
retail-hierarchy-rbac-and-audit.md`, not yet committed) and `architect`'s
hierarchy data-model design (expected at `docs/architecture/26-retail-
operations-architecture.md`, not yet committed). Where this document
needs to say something about either, it states the *shape* it assumes
(§0/§2 below) and flags it `OPEN DECISION — reconcile with [security|
architect]'s Stage 4H-B0 document` rather than inventing a mechanism.
`payments`' sibling deliverable **did** land during this document's
authorship — a new "Retail cash rail" section appended to
`07-payments-architecture.md` — and §1/§4 below are written directly
against its actual decisions (retail cash is explicitly **not** a
`PaymentProvider` adapter; deposit/withdrawal confirmation idempotency is
a state-transition guard on the request row, not a DB-uniqueness key),
superseding this document's own earlier draft assumption of an
ADR-0020-style `(provider_id, provider_tx_id)` shape for retail. Nothing
here should be read as pre-empting `security`'s or `architect`'s
still-unlanded documents.

### 0. Hierarchy as data — conceptual model (`ARCHITECTURAL DECISION`)

Before the endpoint groups: a `HierarchyNode` is an **ordinary
tenant-owned table**, RLS-scoped by `tenant_id` exactly like every other
tenant-owned table (CLAUDE.md's RLS rule) — this document introduces no
second isolation primitive alongside `tenant_id`. A node's position in the
hierarchy (`parent_node_id`, `node_type` ∈
`{partner, super_agent, agent, cashier_terminal}`) is ordinary data, and
"can this caller see/act on this subtree" is an **authorization-scope
question layered on top of tenant scope** — the same shape this codebase
already uses for brand-scoped withdrawal queues (`withdrawal-state-
machine.md` §2) — not a new row-level-security dimension. `security`'s
parallel design owns exactly how that scope is resolved and enforced
(claim on the session, a recursive scope query, etc.); this document only
assumes such a mechanism exists and is enforced server-side, never
inferred from a client-supplied node id.

**`ARCHITECTURAL DECISION`, flagged for `architect` confirmation:**
`Player` is deliberately **not** its own `HierarchyNode` row, despite
being the stated leaf of the directive's hierarchy list. Modeling every
player as a tree node would multiply the tree by player count and conflate
an organizational/staff hierarchy (who reports to whom, who is authorized
to act where) with a customer-attribution relationship (which agent
registered/services this player). Instead: `player_account` gains a
nullable `registered_at_node_id` reference (an ordinary FK to
`HierarchyNode`, used for commission/reporting attribution only — the
commission *value* itself is `ledger-finance`/risk's business-rule domain,
per the directive, not modeled here). `Cashier` *is* a real principal
(see §3) but is not itself a `HierarchyNode` row either — a cashier is a
staff-like principal **assigned to** an `agent`-type or
`cashier_terminal`-type node, the same way a back-office staff member is
assigned to a tenant without being a row in the tenant table.

### 1. Cashier/POS operations — API surface (conceptual endpoint groups)

Every endpoint in this section is called by a **cashier session**, not a
player session and not a bare terminal credential (see §3). Every
endpoint resolves `tenant_id` and the cashier's own `hierarchy_node_id`
server-side from the authenticated session — never from a request
parameter — exactly as `tenant_id`/`brand_id` are resolved today
(`withdrawal-state-machine.md` §7's "all resolved server-side" pattern,
applied to the additional node dimension).

| Group | Conceptual endpoint(s) | Actor / scope | Idempotency shape |
|---|---|---|---|
| Player registration-at-retail | `POST /v1/retail/players` | Cashier session, scoped to their own node — the created `player_account.registered_at_node_id` is set server-side to the caller's own node, never client-supplied | Caller-supplied `client_reference`, unique on `(tenant_id, hierarchy_node_id, client_reference)` — mirrors doc 25's admin-create precedent (mission/badge creation), namespaced by node rather than by player since no player identity exists yet at the point of the call |
| Deposit confirmation | `POST /v1/retail/deposit-requests/{id}/confirm` (naming matches `07-payments-architecture.md`'s landed "Retail cash rail" §2) | Cashier session, scoped to own node; acts on a `RetailDepositRequest` already created (at player-request time) in state `awaiting_cash_handover` for a specific `player_account` — never a call that creates and confirms in one step | Per `07-payments-architecture.md` §1/§2 (landed during this stage, not this document's own invention): **not** an ADR-0020-style `(provider_id, provider_tx_id)` uniqueness key — retail cash is explicitly not a `PaymentProvider` adapter, so there is no `provider_id`. Idempotency is a state-transition guard: `UPDATE ... WHERE state = 'awaiting_cash_handover'`, `RowsAffected()` checked before the ledger-posting call is made; a repeat/racing confirm against an already-resolved request is a no-op returning current state, never a second posting |
| Withdrawal confirmation/fulfillment | `POST /v1/retail/withdrawal-requests/{id}/complete` (naming matches `07-payments-architecture.md` §3) | Cashier session, scoped to own node; acts on a `WithdrawalRequest` already in `submitted` state, reached via the *existing, unmodified* `requested → pending_review → approved` review/approval chain — review/approval is centralized and unaffected by channel (see §4) | Matches `WithdrawalRequest`'s existing state-transition concurrency control (`withdrawal-state-machine.md` §4: optimistic concurrency, `UPDATE ... WHERE state = $expected`), applied here to the `submitted → completed` transition exactly as `07-payments-architecture.md` §3 specifies — a repeat call against an already-`completed` request is a no-op returning current state, never a second cash movement |
| Shift/till open/close | `POST /v1/retail/shifts` (open), `POST /v1/retail/shifts/{id}/close` | Cashier session, scoped to own node/terminal | Append-only; at most one open shift per `(tenant_id, hierarchy_node_id or terminal_id)` enforced by a partial unique constraint (`WHERE closed_at IS NULL`) — opening a second shift while one is open is rejected, not silently allowed to coexist |
| Balance/float inquiry | `GET /v1/retail/shifts/{id}/float`, `GET /v1/retail/players/{id}/balance` | Cashier session, own node/shift for the till float; a specific player's balance only in the presence of that player (see `OPEN DECISION` below) | Read-only, no idempotency concern — a pure projection read, same as `GET /v1/wallets` today; never itself an authoritative source for a subsequent write (a stale float display must not be trusted by a later confirm-deposit call, which re-derives its own state independently) |

**`OPEN DECISION`** (flagged for `security`/product): does a player-balance
lookup at a terminal require the player to authenticate at the terminal
too (a PIN/second factor), or is cashier authentication alone sufficient
to look up a player by identifier? Cashier-alone lookup is a real
fraud/privacy exposure (a cashier could browse arbitrary players' balances
within their own node's book of business) — this document does not decide
it, but flags it as a required decision before `GET
/v1/retail/players/{id}/balance` is implemented.

### 2. Hierarchy management — API surface (conceptual endpoint groups)

**`ARCHITECTURAL DECISION`, `RECOMMENDATION` per the directive's own
prompt**: these are the **same endpoints**, parameterized by the caller's
own tenant + hierarchy-node scope and permission set — not a separate
"partner/agent console" API family. This matches this platform's existing
single-API-surface-many-roles pattern (`GET /v1/admin/players/{id}` is
already called by both `RoleSupport` and `RoleCompliance` under different
permission gates and gets different-shaped responses per caller, not
different routes). Concretely: an internal back-office `RoleTenantAdmin`
managing the platform's own B2C brand's retail network, and an external
B2B tenant's own partner-console user managing their sub-agent tree, call
the identical `/v1/admin/hierarchy/...` routes; what differs is (a) the
caller's own tenant (already isolates B2B tenants from each other and
from the platform's own brand, per existing multi-tenancy rules) and (b)
the caller's own node-subtree scope, resolved server-side. Genuinely
separate surfaces would only be justified if a partner-console caller
needed a materially different response shape or a materially different
trust boundary than an internal back-office caller with equivalent
permissions — no such need is identified here.

**`OPEN DECISION`, flagged for `architect` + identity owner:** whether a
partner-console user is represented by the existing `staff_users`-shaped
principal (extended with a `hierarchy_node_id` column, the same way
`brand_id` already sits alongside `tenant_id` on relevant tables) or a
genuinely distinct principal type. This document assumes (does not
decide) the former, since it avoids a second principal/session model for
what is, functionally, staff authentication scoped one level narrower than
today's tenant-wide staff — but identity's principal model is not this
document's to redesign.

Endpoint groups (paths illustrative):

- `POST /v1/admin/hierarchy/nodes` — create a node (`node_type`,
  `parent_node_id`). Idempotent via caller-supplied `client_reference`,
  unique on `(tenant_id, client_reference)`, mirroring doc 25's
  admin-create precedent (append-only, no in-place field mutation beyond
  what's listed below).
- `GET /v1/admin/hierarchy/nodes/{id}` — view one node. Requires the
  target node to be within the caller's own subtree (or the caller's own
  node), enforced by `security`'s scope-resolution mechanism, not
  redesigned here.
- `GET /v1/admin/hierarchy/nodes/{id}/subtree` — view descendants,
  cursor-paginated per doc 25 §0's convention (a large partner's agent
  tree is exactly the kind of actively-growing, potentially-unbounded list
  that convention exists for).
- `POST /v1/admin/hierarchy/nodes/{id}/reassign` — move a node to a new
  parent. **`ARCHITECTURAL DECISION`**: modeled as an explicit, audited
  state transition (mirrors the withdrawal state machine's "explicit
  transitions, never field overwrites" convention, `withdrawal-state-
  machine.md` §1) rather than a `PATCH` on `parent_node_id`, because a
  reassignment changes commission/limit inheritance for the entire moved
  subtree — a materially bigger blast radius than creating a leaf node,
  and CLAUDE.md requires an audit record (actor, before/after state,
  reason code) for exactly this class of action.
- `GET`/`PUT /v1/admin/hierarchy/nodes/{id}/config` — node-level
  configuration: limits and a commission-plan **reference** (which plan/
  tier applies to this node), never the plan's numeric rate values
  themselves. This endpoint stores/returns only the reference id; the
  values it points at are `ledger-finance`/risk's business-rule domain
  (per the directive) and live in whatever configuration surface those
  specialists define — this API never inlines a rate.

**Permission naming** (mirrors doc 25 §2's table exactly):

| Proposed permission | Gates |
|---|---|
| `hierarchy_node:read` | View a node / subtree, scoped to caller's own subtree (scope enforced by `security`'s design, not this permission bit alone — the bit answers "can this principal read hierarchy nodes at all," the subtree-scope check answers "which ones," a two-part authorization identical in shape to how `PermPlayerRead` today is a tenant-wide bit with no further scoping, generalized here to also carry a node dimension) |
| `hierarchy_node:manage` | Create a node |
| `hierarchy_node:reassign` | **`ARCHITECTURAL DECISION`, mirrors doc 25's `tournament:settle`/`tournament_config:manage` split**: reassignment is its own permission, never bundled with `hierarchy_node:manage` by default — the actor who provisions leaf agents must not, by that grant alone, also be able to re-parent an entire subtree's commission/limit inheritance |
| `hierarchy_node_config:read` / `hierarchy_node_config:manage` | Node-level limits / commission-plan-reference configuration — separate from `hierarchy_node:manage` because authoring *what a node is* (create/reassign) and authoring *what rules apply to it* are separate authorities, exactly as `bonus_config:manage` is separate from ordinary campaign-visibility read in doc 25 |

Per doc 25's own precedent, this document names no role to hold the
`manage`/`reassign`/`config:manage` set and explicitly records that as a
build-time requirement, not resolved here: a dedicated role (analogous to
`RolePromotionsManager`) should hold this set, never defaulted into
`RoleTenantAdmin`.

### 3. Retail terminal as API client — session/auth model

**`ARCHITECTURAL DECISION`**: the primary authentication event is a
**cashier login on the terminal** — a human, staff-like principal
authenticating — not a terminal-only credential granting API access on
its own. This fits the existing JWT/session model
(`05-identity-architecture.md`, ADR 0011's platform-scoped identity
tokens) with no new token type: the cashier receives the same shape of
staff-session JWT issued today (subject = staff principal id, `tenant_id`
claim, permission set resolved server-side), plus one additive claim,
`hierarchy_node_id`, resolved server-side at login from the cashier's own
staff record — never client-supplied, exactly like `tenant_id` is never
client-supplied today. This is deliberately **not** a repeat of this same
document's single-use opaque game-launch token pattern (§"Authentication/
authorization propagation" above): a cashier session is a genuine
multi-request work session (a shift can span hours and many
transactions), not a single-use, single-purpose token.

A shift (§1) additionally bounds the session at the business-logic layer,
not the auth layer: a mutating retail action outside an open shift is
rejected fail-closed, the same enforcement shape RG already uses for
"no path when state disallows it," rather than being expressed as a
separate short-lived token per shift.

**`RECOMMENDATION`, `OPEN DECISION` on exact mechanism (flagged for
`security`)**: a secondary, device-level credential for the terminal
itself should additionally exist, layered onto (never substituting for)
cashier login. Reasoning: (a) an unattended or stolen terminal must not be
able to accept a valid cashier's credentials from an unregistered device
— this is a real fraud vector specific to a channel handling physical
cash, unlike a player's browser session; (b) device revocation (a
decommissioned or reported-stolen terminal) needs a lifecycle independent
of any individual cashier's own credential lifecycle — revoking a
terminal should not require rotating every cashier who ever used it, and
vice versa. Concretely this looks like a device-bound channel (e.g. mTLS
client certificate or a long-lived signed device token presented
alongside, never instead of, the cashier's own JWT) such that a cashier
JWT is only honored over a channel already authenticated as a registered
terminal. The exact mechanism is `security`'s to specify, consistent with
"security owns RBAC/auth enforcement design" — this document only records
the requirement and its rationale.

### 4. The "client, not a financial system" boundary (`ARCHITECTURAL DECISION`)

The POS/terminal **never computes a balance and never decides an
outcome** — not a deposit's acceptance, not a withdrawal's approval, not
a KYC/risk decision. It only (a) submits a request (register player,
confirm cash received, confirm cash handed over, open/close shift) and
(b) displays platform-returned state from that same response. No number a
terminal displays is client-computed, cached-then-trusted, or replayed
from an earlier response as if current.

This generalizes the existing casino-provider-callback precedent
(`08-casino-integration-architecture.md`; this document's own "Inbound
provider callbacks" section above): a third-party casino provider today
asserts *facts* ("this round happened, this is its outcome") and the
ledger — never the provider — decides the resulting balance. A POS
terminal is architecturally the **same shape of client**, just first-party
instead of third-party: it asserts facts ("cash received", "cash handed
over", "a shift opened/closed") and existing platform-owned machinery
(the ledger-posting API on the deposit side, the *unmodified* withdrawal
review/approval chain plus one new sibling fulfillment handler on the
withdrawal side — `07-payments-architecture.md`'s landed "Retail cash
rail" section, §§1–3) — never the terminal itself — decides and posts the
consequence. Concretely:

- Deposit confirmation is not a new balance-mutation code path.
  **Correction against `07-payments-architecture.md`'s landed "Retail cash
  rail" section (§1), which this document defers to**: retail cash is
  deliberately **not** modeled as a `PaymentProvider` adapter — there is
  no external vendor, no async settlement to be ambiguous about, and no
  cascade-on-decline candidate set, so forcing it into that interface
  would mean most of it is a meaningless no-op (CLAUDE.md's "no fake
  completion," applied to an interface implementation rather than a
  claimed integration). It is instead a structurally distinct
  **fulfillment channel**: the cashier's confirm call still ends in the
  *same* "call into `ledger-finance`'s wallet/ledger posting API, never a
  direct balance write" rule every other channel already follows — the
  balance-mutation boundary this section exists to confirm is identical
  either way — but the mechanism reaching that call is a dedicated retail
  confirmation handler, not a registered `provider_id` in the adapter
  registry, and never appears in `provider_capabilities`.
- Withdrawal fulfillment confirmation is the `submitted` → `completed`
  transition already defined in `withdrawal-state-machine.md` §1, reached
  via a **sibling handler variant** for this one fulfillment method at
  `approved → submitted` time (`07-payments-architecture.md` §3) rather
  than a modification of the existing PSP/custodian submit path — the same
  transition an automated PSP webhook triggers for other channels, never a
  new terminal-specific approval path. A cashier confirming fulfillment is
  not granted (and must never be granted, by this permission alone) any
  part of the KYC/risk/four-eyes review authority that already gated
  `approved`; that review remains centralized, unmodified, and
  channel-independent.
- No retail-specific ledger account and no retail-specific balance
  projection is introduced by this document (that is `ledger-finance`'s
  ADR 0035, not yet landed). The idempotency mechanism is retail-specific
  in shape (a state-transition guard on the request row, per §1's table —
  not the existing `(provider_id, provider_tx_id)` / withdrawal-
  `idempotency_key` shapes, since there is no provider and no
  client-supplied key here) but is not a new *class* of financial-write
  risk: it protects against the same "don't apply the same fact twice"
  concern every other idempotency mechanism in this codebase protects
  against, via the same "guarded UPDATE, check `RowsAffected()` before the
  side-effecting call" pattern `internal/withdrawal` already uses. Retail
  is a new **client and a new fulfillment channel**, never a new
  balance-mutation authority.

### 5. Bonus/Gamification API surface — no changes needed this stage

One-paragraph check, not a redesign, per the directive: nothing in this
retail/hierarchy surface touches doc 25's resource shapes, permissions, or
conventions. A retail registration/acquisition channel is, at most, a
future value the Bonus Engine's own eligibility axis (owned by
`bonus-engine`, not this document) might reference — that is a data-model
question inside the Bonus Engine, not an API-surface change here. Stage
4H-A Wave-2's finding that Gamification implementation may be deferred
behind the Bonus Engine's MVP-required core (`14-mvp-scope-and-roadmap.md`)
is purely a build-**order** question for the Orchestrator's synthesis
document; it does not invalidate or require any change to doc 25's frozen
API-surface design, which this document confirms still holds as written.

### 6. Ownership and labeling

Authored by `backend-platform` per Stage 4H-B0's assignment, mirroring doc
25's permission-naming conventions as instructed. Cross-domain inputs:
`payments` (retail deposit/withdrawal idempotency and fulfillment-channel
shape, §1/§4) landed during this document's authorship and has been
reconciled in place — see the endpoint naming and idempotency mechanism
in §1's table and the corrections in §4. Still outstanding, not yet
reconciled: `security` (RBAC/hierarchy-scope enforcement mechanism, §0/§2;
device-credential mechanism, §3 — expected at `docs/decisions/0036-
retail-hierarchy-rbac-and-audit.md`), `architect` (hierarchy data-model
design and confirmation of the Player-is-not-a-node modeling decision in
§0, and of the same-endpoints-many-scopes recommendation in §2 — expected
at `docs/architecture/26-retail-operations-architecture.md`),
`ledger-finance`/risk (commission-plan and limit values referenced but not
defined by §2's node config; the retail ledger-account/posting treatment
this document only assumes is callable from the confirm/complete handlers
— expected at ADR 0035). Every substantive
statement above is labeled `ARCHITECTURAL DECISION`, `RECOMMENDATION`, or
`OPEN DECISION` inline; everything in this section is `NOT IMPLEMENTED` —
no Go types, no route registrations, no OpenAPI YAML, no migration, no
RBAC role-to-permission wiring.

# 36 — Back Office and Partner Console Architecture

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** No Go code, no
frontend code, no route, no screen is authorized by this document. Produced
in Stage 4H-B1, Wave 1.5 Fix Round 2 ("Product Surface Roadmap Gate"),
authored by `backoffice`. Companion to `35-product-surfaces-roadmap.md`
(authored concurrently by `architect`, which owns the overall Product
Surfaces roadmap skeleton and cross-domain dependency graph across all
three surfaces — B2C Brand Frontend, Operator Back Office, Partner
Console). That document was not available to this one at authoring time;
its skeleton is expected to reference this document for the two surfaces
covered here, not the reverse. Wherever this document previously needed a
concrete stage number, it used a placeholder — `[Stage: Back Office MVP —
ID TBD]` and `[Stage: Partner Console MVP — ID TBD]` — rather than
inventing one; `architect`'s roadmap document (`35-product-surfaces-
roadmap.md`) is now published and is authoritative for actual sequencing
and numbering. Per that document's `OI-PSR-1`, the placeholders below are
mechanically replaced with `Stage 6A` (Back Office MVP) and `Stage 6B`
(Partner Console MVP) — a reference update only; this document's own scope
and sequencing analysis is unchanged, and doc 35's own caveat stands:
these IDs are `architect`'s recommendation, not yet ratified outside doc
35 (doc 35 §8, `OI-PSR-5`).

Numbering: next free after `34` (`35` is reserved for the concurrent
roadmap-skeleton document). This document takes `36`.

## 0. Scope anchor — stated honestly

`MASTER-BUILD-PROMPT.md`'s Stage 6 currently reads: *"B2C frontend + back
office + partner console, reporting/BI pipeline"* as a single undifferentiated
line with no information architecture, no MVP boundary, and no dependency
statement for any of the three surfaces. That is the gap this round closes
for two of them. The Blueprint (§2, per the `backoffice` agent definition
in `.claude/agents/backoffice.md`) names both surfaces explicitly and
distinctly: the **operator back office** (internal platform/tenant staff)
and the **partner console** (external B2B partner/tenant admins and
platform admins managing partners). This document does not invent that
distinction — it elaborates it.

This document builds no UI, selects no Human Decision Register item, and
does not override `security`, `identity-compliance`, `ledger-finance`,
`risk`, or `architect` on their respective invariants. Where an MVP scope
below needs something from one of those domains that does not exist today
(a new role, a new endpoint), it is named as a dependency, not designed
here.

## 1. What exists today vs. design-only (verified by inspection, not assumed)

Per `02-domain-and-service-boundaries.md` and `13-dependency-map-and-risk-
register.md`, and confirmed directly against `internal/` at HEAD for this
document:

| Backend domain | Package | Status |
|---|---|---|
| Identity (Person/PlayerAccount/StaffUser/Tenant/Brand) | `internal/identity`, `internal/identityresolution` | `IMPLEMENTED` |
| Auth / RBAC (JWT, tenant context, permission gating) | `internal/auth` | `IMPLEMENTED` (foundation-level; see §4.1's finding on partner-admin) |
| Wallet / Ledger | `internal/wallet`, `internal/ledger` | `IMPLEMENTED` |
| Withdrawal state machine (review/approve/reject/submit, four-eyes via `RequiredApprovals`) | `internal/withdrawal` | `IMPLEMENTED` |
| Payments orchestration (deposits) | `internal/payments` | `IMPLEMENTED` |
| Casino (mock provider gateway, launch, bet/win/rollback) | `internal/casino` | `IMPLEMENTED` (provider is a mock — `PROVIDER DEPENDENT` for anything beyond the sandbox) |
| KYC (verification/document review) | `internal/kyc` | `IMPLEMENTED` (foundation; vendor is `PROVIDER DEPENDENT`) |
| Responsible Gaming (self-exclusion, staff restrictions) | `internal/rg` | `IMPLEMENTED` |
| Risk (rule **configuration** + inline `Evaluate`) | `internal/risk` | `IMPLEMENTED` as configuration + enforcement; **no persisted case/alert/decision-log table exists** — see §5.1 finding |
| Asset registry + asset-change four-eyes (`asset_change_requests`/`asset_change_approvals`) | `internal/assetregistry` | `IMPLEMENTED` |
| Reconciliation (scheduled run, `ReconciliationRun`/`ReconciliationMismatch` persisted) | `internal/reconciliation` | `IMPLEMENTED` as a job + data model; **no admin read API exists yet** |
| Audit (append-only log) | `internal/audit` | `IMPLEMENTED` |
| Sportsbook | — | `NOT IMPLEMENTED` (Stage 5, not started) |
| Bonus Engine | — | `NOT IMPLEMENTED` (architecture frozen only, docs 10/27-29; Stage 4H-B1 core not authorized) |
| Segmentation | — | `NOT IMPLEMENTED` (architecture frozen, doc 30) |
| CRM | — | `NOT IMPLEMENTED` (architecture frozen, doc 31) |
| Affiliate | — | `NOT IMPLEMENTED` (architecture frozen, doc 32) |
| Gamification | — | `NOT IMPLEMENTED` (architecture frozen, docs 17-21) |
| Retail / Agent Network | — | `NOT IMPLEMENTED` (architecture frozen, doc 26) |
| Reporting/BI (CDC → ClickHouse) | — | `NOT IMPLEMENTED` (Stage 6/7 scope, no pipeline exists) |
| CMS | — | `NOT IMPLEMENTED` (no Blueprint-anchored requirement found beyond brand/theme config, which is `tenant-config`'s, not a separate CMS domain) |
| Partner-distinct RBAC (a `partner_admin` role/principal separate from `tenant_admin`) | `internal/auth` | **Explicitly deferred in-code** — `rolePermissions`'s own comment: *"Stage 2 does not make this database-driven/partner-configurable — that would be a Stage 6 partner-console feature (custom roles), premature before there's a real multi-tenant admin surface to configure it from."* This is the single most important finding of this document — see §4.1 and §7.4. |

## 2. Design principle: an operational control surface, not a generic admin dashboard

This is an explicit human requirement, restated here so it drives every
downstream decision: **the Back Office is not "a CRUD screen per
database table."** It is built around the actual jobs staff do — clear a
KYC review, approve or reject a withdrawal, investigate a risk-flagged
player, resolve a reconciliation mismatch, suspend an account — as
**queues with state, ownership, and an SLA-relevant "what's outstanding"
view**, not as a table browser bolted onto every domain's schema.

Concretely, this means:

- The **home surface per section is a queue/worklist filtered to "needs
  action by me/my role,"** not a paginated table of every row that ever
  existed. A generic list view still exists underneath (for search/audit/
  investigation), but it is not the primary interaction.
- **Every queue item's possible actions are exactly the actions the
  caller's role is permitted to take**, discovered from what the API
  returns as available (never a client-side "if role == X show button"
  branch with no matching server check — see §3).
- **Cross-domain composition happens in the UI only as read-side
  aggregation**, never as a business decision. E.g., a Player detail page
  may show wallet balance, KYC status, RG restrictions, and open risk
  rules side by side (reads from four different domains), but suspending
  that player, clearing a KYC flag, or removing an RG restriction are four
  distinct authorized actions against four distinct APIs — never one
  "player admin" endpoint that fans out server-side across domain
  boundaries the platform elsewhere keeps separate (`02-domain-and-
  service-boundaries.md`'s verified boundary table).
- **The navigation is deliberately over-provisioned relative to what's
  buildable today** (§5.2's "coming soon" column exists for exactly this
  reason): a nav restructuring is expensive and error-prone once staff
  have muscle memory for where things live, so the taxonomy is fixed now,
  before most of it has a backend, rather than grown ad hoc as each domain
  lands.

## 3. Non-negotiables (restated — unchanged by this document)

These are `CLAUDE.md` obligations, not new rules, restated here because
this document is the concrete design surface where they are most often
weakened by convenience:

1. **The UI never owns financial or business rules.** No eligibility
   check, threshold comparison, approval-count logic, or state-transition
   rule is duplicated in frontend code — even for a "just disable the
   button" UX nicety. The UI calls an authoritative API and renders its
   answer; if the API says an action is unavailable, the UI shows that,
   it does not compute its own version of why.
2. **Permissions are enforced server-side only, never inferred from the
   UI.** Every action a screen exposes maps to a route gated by
   `auth.RequirePermission` (or the future partner-scoped equivalent,
   §7.4). A hidden button is a UX convenience, never a security boundary
   — the same discipline the `backoffice` agent definition already states
   as a testing responsibility (§6.8, §8.7 below).
3. **No brand-specific forked backend logic, ever, for either surface.**
   A brand difference the Back Office or Partner Console needs to expose
   (theme, catalogue, payment methods, RG defaults, bonus templates,
   jurisdiction rules) is a configuration row read through the same
   tenant-config API every tenant uses — never a per-brand code path, an
   `if tenant.Slug == "..."` branch, or a brand-specific screen variant.

## 4. The two surfaces are separate products, not one app with a role switch

They share a design system and may share a component library (§6.7/§8.7),
but are **separately deployed, separately routed, and separately RBAC-
scoped** applications. This is not a UX preference — it follows directly
from the actor difference:

| | Operator Back Office | Partner Console |
|---|---|---|
| Actor | Platform staff, tenant-scoped operator staff (support, compliance, finance, risk manager, tenant admin, platform admin) — all `identity.StaffUser` rows under the platform's own operational hierarchy | External B2B partner/tenant admins (a distinct actor even when represented as a `StaffUser` row — see §4.1), plus platform staff acting *on behalf of* a partner relationship |
| Trust boundary | Internal, platform-operated | External counterparty — same category of "internal-principal-space-but-external-interest" risk `security` already flagged for affiliates (doc 32 R13/SEC-W15-01) |
| Primary unit of scoping | A tenant/brand the staff member is assigned to operate | The partner's own tenant, and only their own tenant — never cross-tenant, ever |
| Typical action | Approve a withdrawal, review a KYC document, suspend a player, configure a risk rule | Provision a brand, rotate a PSP credential, read a revenue-share statement, view own compliance posture |
| Money movement authority | Yes, gated (withdrawal approval, asset-change approval) | **No.** A partner console user never directly authorizes a ledger-affecting operation; it *requests* (e.g., a brand config change subject to platform review) or *reads* (a statement, an invoice) |

### 4.1 Finding: today's RBAC model cannot yet express "partner admin" safely

`internal/auth/permission.go`'s `rolePermissions` map has exactly one
tenant-scoped administrative role, `tenant_admin`, and it is designed for
**our own operator staff acting on a tenant**, not for an **external
partner acting on their own tenant**. It holds `PermStaffManage`,
`PermPlayerSuspend`, `PermProviderConfigWrite`, `PermCasinoConfigWrite`,
`PermWithdrawalPolicyWrite`, and (correctly, per Stage 3D/4D/4F/4G's own
documented separation-of-duties reasoning) does **not** hold withdrawal-
approval or RG/KYC-write authority. None of that permission set is what a
Partner Console should hand to an external partner: staff management,
casino config, and withdrawal-policy authority over *our own platform's*
operational levers are not partner-appropriate capabilities even when
scoped to "their own tenant," because a hybrid-licensing tenant that
brings its own licence is still operating on shared platform
infrastructure whose operational levers (risk thresholds, casino provider
config, withdrawal policy) are ours to govern, not theirs to self-serve.

**This is a real, named gap, not a design choice this document makes
silently**: Partner Console cannot ship on `tenant_admin` reused verbatim.
It needs either (a) a new `StaffRole` (e.g. `partner_admin`) with its own,
narrower, permit-by-enumeration permission set, or (b) a distinct
principal type outside `StaffRole` entirely — the same fork in the road
`security` already named for affiliates in doc 32 §3.2/DEP-AFF-1 ("a
distinct principal class with deny-by-enumeration" vs. "a `StaffUser` with
scoped roles"). This document does not resolve that choice — it is
`security` + `identity-compliance`'s call, exactly as doc 32 left it open
for affiliates — but flags it as the **first blocking dependency** for any
Partner Console implementation (§8.2).

---

# PART A — Operator Back Office

## 5. Information architecture

### 5.2 Navigation sections

Each row states the backing domain's status today, and — separately —
what that navigation section should actually be **in an MVP build**: a
real feature, a "coming soon" placeholder (visible, disabled, or a static
explainer — a placement decision for `ux-design`, not fixed here), or
omitted entirely from the nav. The taxonomy below is the full, stable set;
omitting a section from MVP means it is not *built*, not that its slot
disappears — nav restructuring is exactly the cost this table exists to
avoid paying twice.

| Section | Backend domain status | MVP treatment | Notes |
|---|---|---|---|
| Dashboard | No dedicated aggregate domain; composes reads from several | Real, but minimal — an operational status panel (open withdrawal-review count, open KYC-review count, open RG-restriction count, latest reconciliation run status), not a BI dashboard | Reporting/BI (doc 6's Stage 6/7 CDC→ClickHouse pipeline) does not exist; a "Dashboard" that promises trend charts, cohort analysis, or NGR would be fabricating capability that isn't there |
| Players | `internal/identity` — `IMPLEMENTED` | **Real feature** | List/search (`newListPlayersHandler`), detail (`newGetPlayerHandler`), suspend (`newSuspendPlayerHandler`), identity-review clearing (`newClearIdentityReviewHandler`) all exist today |
| Wallets | `internal/wallet`, `internal/ledger` — `IMPLEMENTED` | **Real feature**, read-only in MVP | Balance and ledger-entry views per wallet/asset are real; a manual-balance-adjustment **screen** is explicitly **out of MVP** — see §6.3 finding |
| Transactions | `internal/ledger` — `IMPLEMENTED` | **Real feature** | Ledger-entry search/filter by account, type, date, correlation id |
| KYC/AML | `internal/kyc` — `IMPLEMENTED` (foundation) | **Real feature** | Verification/document review queue (`newReviewVerificationHandler`, `newReviewDocumentHandler`) exists; vendor integration behind it is `PROVIDER DEPENDENT` (sandbox today) |
| RG | `internal/rg` — `IMPLEMENTED` | **Real feature** | Staff-restriction creation/listing (`newCreateStaffRestrictionHandler`, `newListRestrictionsForAccountHandler`) exists |
| Risk | `internal/risk` — `IMPLEMENTED` as **rule configuration + inline enforcement only** | **Real, but narrower than "queue" implies** | A risk-rule config screen (list/create/disable, `newListRiskRulesHandler` etc.) is real. A **case-management queue** ("here are the players a rule flagged, assign/investigate/resolve") does **not** exist — no persisted decision/alert table. MVP ships rule config + an audit-log-derived "recent risk denials" view (read of `internal/audit`, filtered), explicitly labeled as *not* a case queue |
| Bonuses | — `NOT IMPLEMENTED` | **Omitted** | No `internal/bonus` package; nothing to point a screen at |
| Segmentation | — `NOT IMPLEMENTED` | **Omitted** | Architecture frozen (doc 30) only |
| CRM | — `NOT IMPLEMENTED` | **Omitted** | Architecture frozen (doc 31) only; also gated on a consent model that doesn't exist (DEP-CRM-1) |
| Gamification | — `NOT IMPLEMENTED` | **Omitted** | Architecture frozen (docs 17-21) only |
| Affiliate | — `NOT IMPLEMENTED` | **Omitted** | Architecture frozen (doc 32) only |
| Casino | `internal/casino` — `IMPLEMENTED` (mock provider) | **Real feature**, scoped | Catalogue/config admin (`PermCasinoCatalogueManage`, `PermCasinoConfigWrite`) exists; "coming soon" badge on any live-provider-specific claim since the only provider today is a mock |
| Sportsbook | — `NOT IMPLEMENTED` | **Omitted** | Stage 5 not started |
| Payments | `internal/payments` — `IMPLEMENTED` | **Real feature** | Deposit visibility; provider-capability config (`newProviderCapabilityHandler`-family) |
| Withdrawals | `internal/withdrawal` — `IMPLEMENTED` | **Real feature — the flagship MVP workflow** | Review/approve/reject/submit queue against the real state machine, four-eyes via `RequiredApprovals` (default 2, `withdrawal/policy.go`) |
| Reconciliation | `internal/reconciliation` — `IMPLEMENTED` as job + data model, **no admin read API** | **Coming soon** (near-term, small gap) | `ReconciliationRun`/`ReconciliationMismatch` rows already exist; a thin read-only API is the only missing piece, but it is not authorized by this document, so MVP does not include it |
| Assets/Currencies | `internal/assetregistry` — `IMPLEMENTED` (incl. four-eyes `asset_change_requests`/`asset_change_approvals`) | **Real feature** | This is also the platform's best existing example of a four-eyes UI pattern — reuse its shape rather than reinventing one for manual balance adjustment later |
| Tenants/Brands | `internal/identity` via `admin_routes.go` — `IMPLEMENTED` | **Real feature**, `platform_admin`-only | `newCreateTenantHandler`/`newCreateBrandHandler`; per ADR 0011, tenant creation is platform-admin-only, brand creation follows `canActOnTenant` |
| Users/RBAC | `internal/identity`/`internal/auth` — `IMPLEMENTED` for creation only | **Partially real** | Staff creation (`newCreateStaffHandler`) and person-linking exist; **role update/deactivation of an existing staff account does not exist yet** — MVP ships creation + list, not a full lifecycle editor |
| Audit | `internal/audit` — `IMPLEMENTED` | **Real feature** | `newListAuditLogHandler` — search/filter the append-only log |
| Reporting/BI | — `NOT IMPLEMENTED` | **Omitted** | No CDC/ClickHouse pipeline exists (Stage 6/7 per `MASTER-BUILD-PROMPT.md`) |
| CMS | — `NOT IMPLEMENTED`, no Blueprint anchor found | **Omitted, pending scope check** | Brand theming/content is `tenant-config`'s job; a distinct "CMS" domain (e.g. promotional page builder) has no Blueprint requirement located — if a human wants one, it is a scope-expansion decision per `CLAUDE.md`, not assumed here |
| Integrations | `internal/payments`, `internal/casino` provider-capability config — `IMPLEMENTED` (config surface) | **Real feature**, folded into Casino/Payments config rather than a separate top-level section in MVP | Kept in the full taxonomy as its own slot for when there are enough provider types (sportsbook, KYC vendor, affiliate platform) to justify a dedicated cross-domain integrations registry screen |

### 5.3 Navigation stability rule

The 24-section taxonomy above is the **complete, fixed set** for this
surface. An MVP build implements a subset (§6.1); it never renders a
different top-level structure. When Bonus, Segmentation, CRM,
Gamification, Affiliate, Sportsbook, or Reporting/BI eventually land, they
occupy a **pre-existing nav slot** that flips from "omitted"/"coming soon"
to "real feature" — no parent-nav redesign, no re-training staff on where
things live. This is the concrete mechanism satisfying the task's
requirement that "the information architecture must not require a full
navigation redesign when a new domain eventually lands."

## 6. Back Office MVP implementation stage — `Stage 6A` (`35-product-surfaces-roadmap.md` §5, `OI-PSR-1`)

### 6.1 Scope

**In scope** (all backed by domains that are `IMPLEMENTED` today, per §1):

- Players (list/search/detail/suspend/identity-review clear)
- Wallets (read-only balance + ledger views)
- Transactions (ledger-entry search)
- KYC/AML review queue
- RG restriction management (staff-created restrictions, read of
  player-facing self-exclusion)
- Risk rule configuration + audit-derived recent-denial view (explicitly
  **not** a case-management queue — see §5.2)
- Casino catalogue/config administration
- Payments/provider-capability configuration
- Withdrawals: the full review → approve/reject → (system) submit queue,
  with visible four-eyes state (how many approvals obtained vs. required)
- Assets/Currencies administration, including the existing
  request/approve four-eyes flow
- Tenants/Brands provisioning (platform-admin scope)
- Users/RBAC: staff creation and listing only
- Audit log search
- Dashboard: minimal operational-status panel only

**Explicitly excluded from this stage** (per the task's directive and
§1's status table — none of these has backend work authorized, and
building UI against them would be exactly the "fake completion" `CLAUDE.md`
forbids):

- Anything under Bonuses, Segmentation, CRM, Gamification, Affiliate,
  Sportsbook, Reporting/BI, or CMS.
- Manual wallet-balance adjustment as a shipped workflow (the ledger
  primitive — `TxManualAdjustment`, reason-code-enforced — exists, but no
  admin HTTP endpoint exposes it yet, and no four-eyes consumption is
  wired to it; adding that endpoint is backend/`ledger-finance` work this
  stage does not authorize).
- Reconciliation admin views (data exists, no read API yet).
- Any Risk case-management workflow beyond rule configuration.
- Staff role editing/deactivation (creation only exists today).

### 6.2 Backend API dependencies

Every screen in §6.1 maps to routes that exist today in
`internal/httpserver` (`admin_routes.go`, `casino_admin_handlers.go`,
`kyc_admin_handlers.go`, `rg_handlers.go`, `risk_handlers.go`,
`withdrawal_handlers.go`, `withdrawal_policy_handlers.go`,
`asset_registry_handlers.go`, `provider_capability_handlers.go`,
`credential_handlers.go`), gated by the existing permission set
(`PermPlayerRead/Suspend`, `PermAuditRead`, `PermStaffManage`,
`PermWithdrawalReview/Approve/Reject/Submit`, `PermProviderConfigWrite`,
`PermWithdrawalPolicyWrite`, `PermCasinoConfigWrite/CatalogueManage`,
`PermRGRestrictionRead/Write`, `PermIdentityReviewManage`,
`PermVerificationRead/Review`, `PermRiskConfigRead/Manage`,
`PermAssetRegistryManage`, `PermAssetAuthorizationWrite`,
`PermTenantRead/Write`, `PermBrandRead/Write`). **No new backend
permission or endpoint is required for this stage's in-scope list** — this
is what makes it a genuine MVP rather than a UI shell waiting on backend
work. The two small, named exceptions (reconciliation read API, manual
adjustment endpoint) are called out precisely so they are not silently
assumed into scope.

### 6.3 Gaps discovered this round (recorded, not fixed)

1. **Manual balance adjustment has a domain primitive but no admin API.**
   `internal/ledger` defines `AccountManualAdjustment`/
   `TxManualAdjustment` and fails closed without a reason code, but
   nothing in `internal/httpserver` exposes it, and no four-eyes
   consumption function is wired to it (the platform's own four-eyes
   precedent is `asset_change_consume_approved_request` for asset
   changes). Building this is `ledger-finance` + `security` work, not
   authorized here, and must exist before any manual-adjustment UI ships.
2. **Risk has configuration and enforcement but no investigation queue.**
   If the Orchestrator wants a real "flagged players to investigate"
   workflow, that requires `risk` to persist evaluation
   decisions/denials as queryable records (or a `security`-reviewed
   decision to derive it entirely from `internal/audit`, which was
   designed for compliance/legal record-keeping, not operational
   triage UX). Recorded as an open item for `risk` + `architect`, not
   decided here.
3. **Reconciliation has data but no read API.** Small, low-risk addition
   (`backend`/`ledger-finance`); not authorized this round.
4. **No staff role-update/deactivation endpoint.** Onboarding is covered;
   offboarding and role changes are not. This is a real operational gap
   for a live back office (a departing employee's access must be
   revocable) and should be prioritized early in implementation even
   though it wasn't named in the task's explicit MVP list — flagging it
   here rather than silently building past it.

### 6.4 RBAC requirements

- Enforced exclusively server-side via `auth.RequirePermission`/
  `RoleHasPermission` — the UI reflects, never decides, what a given
  staff member may do. A screen renders an action's affordance only when
  the authenticated session's role includes the corresponding permission,
  but the route itself performs the real check regardless of what the
  client sends.
- No new role is required for the in-scope list (§6.1) — `platform_admin`,
  `tenant_admin`, `support`, `compliance`, `finance`, and `risk_manager`
  already cover it, with the existing separation-of-duties rules
  (`tenant_admin` excluded from withdrawal/RG-write/verification-review
  authority; `finance` and `risk_manager` each single-purpose) unchanged
  and un-widened by this UI.
- Tests must assert **both** that a role without a permission cannot see
  the UI affordance **and** that the underlying API call is rejected when
  attempted directly (curl-equivalent) — mirroring the `backoffice` agent
  definition's own standing testing responsibility.

### 6.5 Tenant/brand scoping

Identical discipline to the rest of the platform: `tenant_id` in every
Back Office request comes from the authenticated session's server-side
tenant context (`tenant.Context`), never from a client-supplied header,
query param, or route segment that isn't independently re-derived
server-side. RLS is the enforcement mechanism, not the UI's own filtering
— a screen filtering its own query by a tenant selector is a UX
convenience over an API that already refuses cross-tenant rows via
`WithTenant`, exactly as `canActOnTenant`'s doc comment states for
tenant/brand provisioning (ADR 0011). A `platform_admin` session (`tc.
TenantID == uuid.Nil`) may act across tenants where a route explicitly
allows it (tenant/brand provisioning); every other role is confined to
its assigned tenant by the same RLS/context mechanism used everywhere
else in the platform.

### 6.6 Audit requirements

Every mutating action in this stage's scope (player suspend, KYC/document
review decision, RG restriction create, risk rule create/disable, casino
catalogue/config change, withdrawal approve/reject, asset-change
approve, staff creation, tenant/brand creation) already writes through
`internal/audit`'s `Record` function per `02-domain-and-service-
boundaries.md`'s stated rule ("only `audit`'s shared library appends to
the audit store"). The Back Office's obligation is narrower than
building audit — it is **surfacing** it: the Audit nav section must let
staff search by actor, tenant, target type/id, and action, and every
other screen's action-result feedback should link to the resulting audit
entry so an action's provenance is one click away, not a separate hunt
through the Audit screen.

### 6.7 Design system / frontend technology

**Recommendation**: React, with a component library purpose-built for
dense, data-heavy operational UIs and a **real virtualized, server-side-
filtered data grid** — this is a named Blueprint requirement (§7, per the
`backoffice` agent definition), not optional polish, because queue views
here can run to tens of thousands of rows (players, ledger entries, audit
log) where client-side pagination or a non-virtualized table degrades
badly. Concretely: **Mantine or Ant Design** as the base component
library (both ship production-grade data-grid, form, and admin-shell
primitives out of the box; a from-scratch design system for an internal
tool is generally not worth the cost relative to shipping the operational
workflows this document scopes), paired with **TanStack Table** (headless,
virtualization-friendly) or the chosen library's own virtualized grid if
it's adequate, and **TanStack Query** for server-state/cache management
against the platform's REST APIs.

**This is a `backoffice`-scope recommendation for an internal admin tool,
not a claim about what `frontend`'s parallel B2C brand-frontend dispatch
will choose** — that dispatch is running concurrently and its design-
system decision has not been seen by this document's author. B2C brand
frontends generally warrant a different stack (Next.js, brand-themeable,
consumer-grade animation/interaction budget) than an internal admin tool
optimized for information density and keyboard-driven workflows, so a
mismatch here is expected and not itself a problem — **but reconciliation
is flagged as an explicit follow-up**: if both dispatches independently
pick incompatible React component libraries, at minimum shared primitives
(auth session handling, API client generation from the platform's OpenAPI
spec per `04-api-architecture.md`, design tokens for anything genuinely
shared like brand color previews) should be a common package rather than
duplicated. The Partner Console (Part B) should share Back Office's choice
here rather than pick a third one, since both are internal-operator-style
surfaces even though they serve different actors.

### 6.8 Testing approach

Per the `backoffice` agent definition's own standing testing
responsibility, plus `qa`'s general gate:

- **RBAC enforcement tests**: for every screen/action, a test asserting a
  role lacking the permission (a) does not see the affordance and (b)
  receives a rejection when the underlying API is called directly,
  bypassing the UI.
- **Four-eyes tests**: withdrawal approval and asset-change approval
  screens must be tested against the real `RequiredApprovals`/approval-
  count state, including the case where the acting user is not eligible
  to provide the second approval (self-approval, non-distinct approver).
- **Audit-linkage tests**: every mutating action's success path is
  followed by an assertion that a corresponding audit entry exists and is
  visible in the Audit screen with matching actor/target/action.
- **Tenant-scoping tests**: a tenant-scoped session cannot retrieve or act
  on another tenant's rows through any in-scope screen, exercised the
  same way the platform's existing RLS tests are (attempt the read/write,
  assert rejection or empty result, not merely "the UI didn't show a
  link").
- **Degradation tests**: an action the UI allowed to be attempted but the
  API rejects (e.g., a race where a withdrawal was approved by someone
  else first) must render the server's rejection reason, not a stale
  optimistic-UI success state.
- No end-to-end test against a live third-party provider (KYC vendor,
  PSP) — those remain sandbox/mock per `CLAUDE.md`'s environment-safety
  rule until a real vendor contract exists.

### 6.9 Release criteria

1. Every screen in §6.1's in-scope list is backed by a real API call with
   no client-side business logic duplicating a server decision.
2. RBAC, four-eyes, audit-linkage, and tenant-scoping test suites (§6.8)
   pass, independently reviewed by `security`.
3. `code-reviewer` sign-off on the frontend/API-integration code.
4. No screen references a domain from the "omitted" column of §5.2 as if
   it were live (no dead links, no fabricated data, no "demo mode" that
   could be mistaken for real capability — `CLAUDE.md`'s "no fake
   completion" rule applied to UI, not just backend claims).
5. The 24-section nav taxonomy (§5.2) is implemented in full for
   navigation structure, even where most sections show "coming soon" —
   this is what proves the stability property (§5.3) before a second
   domain lands.
6. Every deliverable in the stage completion report is labeled
   `IMPLEMENTED`, `PARTIALLY IMPLEMENTED`, `MOCK`, `STUB`, `PROVIDER
   DEPENDENT`, `NOT IMPLEMENTED`, or `BLOCKED` per `CLAUDE.md` — no
   section of this document's scope may be reported as done if it is a
   placeholder.

---

# PART B — Partner Console

## 7. Definition and separation from Back Office

### 7.1 Why this is a separate surface (restated concretely)

Per §4, this is a structurally distinct product: different actor (external
partner vs. internal staff), different trust boundary, different money
authority (read/request, never approve), and — per §4.1's finding —
**different RBAC primitives that do not yet exist**. Building Partner
Console as "Back Office with a partner flag" would put an external
counterparty in the same principal space as `tenant_admin`/`finance`/
`risk_manager`, which is exactly the self-dealing/privilege-boundary
pattern `security` already flagged as R13 for affiliates (doc 32) —
Partner Console must not recreate that mistake for the broader partner
relationship.

### 7.2 Actors and scope

- **Partner/tenant admin** (external, B2B): a human at an external
  operator who brought their own tenant onto the platform (hybrid
  licensing, ADR 0006) or who licenses under the platform's own licence
  as an external brand. Scoped strictly to their own tenant; never sees
  another tenant's data, ever, at any RLS or UI layer.
- **Platform staff acting on the partner relationship** (internal): e.g.
  a platform admin approving a partner's requested brand configuration
  change, or platform finance staff producing a revenue-share statement.
  This is Back Office territory, not Partner Console — Partner Console is
  the partner's own view; the platform-staff-facing counterpart to
  "manage partners" belongs in Back Office's Tenants/Brands section
  (already scoped in Part A), not duplicated here.

### 7.3 Information architecture

| Section | Backend domain status | MVP treatment | Notes |
|---|---|---|---|
| Brand overview/provisioning | `internal/identity` brand create/read — `IMPLEMENTED`, but **platform-admin-scoped today, not partner-self-service** | **Partially real, gated** | The underlying brand-create API exists; a partner-facing version requires the new partner-scoped role (§4.1) and a decision on which brand-config fields a partner may self-serve vs. request-and-platform-approves (a request/approval pattern like `asset_change_requests`, not direct write, is the safer default for anything touching risk/compliance-relevant config) |
| Provider/PSP credential management | `internal/payments`, `credential_handlers.go` — `IMPLEMENTED` as a platform-admin config surface | **Partially real, gated** | `PermProviderConfigWrite` exists but is not partner-scoped; a partner rotating their own PSP credential is a real, common need but requires the same new-role work as above, plus a security review of credential-write exposure to an external principal |
| Revenue-share statements | — `NOT IMPLEMENTED` | **Omitted** | No commission/revenue-share domain exists anywhere in the platform. Affiliate architecture (doc 32) explicitly declines to design the commission posting shape (`ledger-finance`'s open item, DEP-AFF-4); a tenant-level revenue-share concept (as opposed to affiliate commission) has no architecture at all yet |
| Cost pass-through | — `NOT IMPLEMENTED` | **Omitted** | Same gap as above — no cost-attribution/billing domain exists |
| Invoicing | — `NOT IMPLEMENTED` | **Omitted** | No invoicing domain; would also need a real payment-collection mechanism from the partner, out of scope for a control-surface document |
| Compliance view (scoped) | `internal/kyc`, `internal/rg`, `internal/risk` — `IMPLEMENTED`, but with **no partner-scoped read permission today** | **Coming soon, careful scope** | Even once buildable, this must expose only aggregate/summary compliance posture for the partner's own tenant (e.g. "KYC completion rate," "open RG restrictions count") — **never per-player PII or case detail**, which stays platform-staff-only. This needs an explicit `security` + `identity-compliance` sign-off on exactly what aggregate is safe to expose to an external partner before any screen is built, not just an RBAC permission |

### 7.4 The honest MVP finding: Partner Console is currently thin

Unlike Back Office, where six of nine in-scope areas are fully backed
today, **almost none of Partner Console's Blueprint-named capabilities
(brand provisioning, PSP credential management, revenue-share, cost
pass-through, invoicing, compliance view) has a partner-appropriate
backend today.** The closest things that exist (`internal/identity` brand
APIs, `internal/payments` credential config) are real, but built and
permissioned for **our own platform staff**, not for **self-service by an
external partner**. This is not a criticism of prior stages — Partner
Console was never previously scoped — but it means an honest MVP for this
surface is materially smaller than Back Office's, and this document says
so rather than padding the scope with placeholders dressed as features.

## 8. Partner Console MVP implementation stage — `Stage 6B` (`35-product-surfaces-roadmap.md` §5, `OI-PSR-1`)

### 8.1 Scope

**In scope**, once its one blocking dependency (§8.2 item 1) is resolved:

- Read-only brand/tenant configuration view (their own tenant only) —
  no write, to avoid needing the request/approval pattern's design work
  before this stage can ship anything at all.
- Read-only view of their own tenant's provider/PSP configuration
  (which providers are enabled, not raw credential values — credential
  *rotation* is a write action deferred to a later wave per §8.2 item 2).
- A minimal, aggregate-only compliance posture view, gated on
  `security`/`identity-compliance` sign-off on exactly which aggregates
  are safe (§7.3).

**Explicitly excluded / `BLOCKED`**:

- Revenue-share statements, cost pass-through, invoicing — no backend
  domain exists at all (§7.3); this is `NOT IMPLEMENTED` at the domain
  level, not a UI gap.
- Brand configuration writes, PSP credential writes/rotation — blocked on
  the new partner-scoped RBAC role (§4.1) and, for anything
  compliance/risk-relevant, a request/approval pattern rather than direct
  write.
- Per-player compliance detail — never exposed to a partner, by design,
  regardless of role.

Given this, `Stage 6A` (Back Office MVP) should very likely
precede `Stage 6B` (Partner Console MVP) in `architect`'s roadmap
skeleton — Back Office has six of nine ready-today sections and zero new
RBAC primitives required, while Partner Console needs a new RBAC concept
built and reviewed before its first screen can safely ship. `architect`'s
`35-product-surfaces-roadmap.md` §5 has since confirmed exactly this
ordering. This document states that dependency for `architect`'s sequencing
call; it does not
itself decide stage order.

### 8.2 Dependencies (blocking, in priority order)

1. **A partner-scoped role/principal** (§4.1) — `security` +
   `identity-compliance` decide new `StaffRole` vs. distinct principal
   type, with a permit-by-enumeration permission set containing none of
   `PermStaffManage`, `PermWithdrawalReview/Approve/Reject/Submit`,
   `PermCasinoConfigWrite`, `PermRiskConfigManage`, or
   `PermAssetRegistryManage`. **Nothing in Partner Console can be
   implemented safely before this exists.**
2. A decision on **write pattern** for partner-initiated config changes:
   direct write (only ever safe for genuinely cosmetic fields) vs.
   request-and-platform-approves (the `asset_change_requests` precedent) —
   `architect` + `security`.
3. `security` + `identity-compliance` sign-off on the exact aggregate
   compliance metrics safe to expose to a partner (§7.3).
4. For anything beyond the MVP scoped here (revenue-share, invoicing):
   `ledger-finance`'s commission/revenue-share posting-shape decision
   (parallel to the still-open DEP-AFF-4 for affiliate commission) plus a
   human commercial-policy decision on revenue-share terms — both
   explicitly out of this document's authority.

### 8.3 RBAC requirements

- Same absolute rule as Back Office (§6.4): server-side enforcement only.
- Additionally: **the partner-scoped role must be impossible to reach
  platform-operational permissions through, by construction** — a code-
  level test (mirroring the pattern already used for `tenant_admin`'s
  withdrawal-permission exclusion) asserting the new role's permission
  set has empty intersection with the money-movement and platform-
  operational permission set.
- A partner user must never be able to name another tenant's id anywhere
  in a request and get a non-empty/non-rejected result — this is RLS's
  job (§8.4), but the RBAC layer must not even present another tenant's
  id as selectable in any UI affordance.

### 8.4 Tenant/brand scoping

Identical RLS discipline to Back Office (§6.5) with one addition: because
the actor is external, there is **zero tolerance for a platform-admin-
style cross-tenant escape hatch** — no Partner Console route may ever
accept `tc.TenantID == uuid.Nil`-style platform-wide scope. Every route
this surface calls must be one that already requires (or is modified to
require) a non-nil tenant context, structurally preventing the
Back Office's platform-admin cross-tenant pattern from ever applying here.

### 8.5 Audit requirements

Identical obligation to Back Office (§6.6): every partner-initiated
mutating action (even a "request a change" write, not just a direct one)
writes an audit record with `ActorType: audit.ActorStaff` (or whatever
principal type §8.2 item 1 settles on) so a partner's actions are as
traceable as an internal staff member's. Given the external-counterparty
trust boundary, `security` should specifically review whether partner
audit records need any additional field (e.g., an explicit "external
principal" flag) beyond what internal-staff audit records carry — flagged
as a question for that review, not answered here.

### 8.6 Design system / frontend technology

Share Back Office's choice (§6.7) rather than introduce a third stack —
both are internal-operator-style, data-dense, form-and-table-heavy
surfaces, and Partner Console's actual near-term scope (§8.1) is smaller
than Back Office's, which argues for maximum reuse of components,
API-client generation, and auth-session handling rather than a bespoke
partner-facing design language. If a genuinely different visual identity
is wanted for the partner-facing surface (a reasonable ask — it is,
after all, external-facing to a paying customer), that is a theming
decision on top of the shared component library, not a different
library.

### 8.7 Testing approach

Everything in §6.8 applies, plus:

- **Cross-tenant isolation is the single highest-priority test class**
  for this surface specifically — an external partner account must be
  proven, not assumed, incapable of reading or writing another tenant's
  data through every in-scope screen, including edge cases (an id typed
  directly into a URL/API call, not just UI navigation).
- **Permission-boundary tests** asserting the partner role cannot reach
  any platform-operational permission (§8.3), run as part of the same
  suite that already tests `tenant_admin`'s withdrawal-permission
  exclusion, so a regression in either is caught by the same gate.

### 8.8 Release criteria

1. §8.2 item 1 (partner-scoped RBAC) is implemented, reviewed by
   `security`, and its permission-boundary test (§8.7) passes, **before**
   any Partner Console screen is built against it.
2. Every screen in §8.1's in-scope list is read-only or request-based per
   §8.2 item 2's decision — no direct write to anything compliance/
   risk/financially relevant ships in this first stage.
3. Cross-tenant isolation tests (§8.7) pass, independently reviewed by
   `security`.
4. No screen references revenue-share, cost pass-through, or invoicing as
   if they exist — those sections, if shown at all in nav for taxonomy-
   stability reasons (§5.3's principle applied here too), are unambiguously
   labeled not-yet-available, never a fabricated or "sample data" view.
5. Every deliverable labeled per `CLAUDE.md`'s completion-status taxonomy,
   with `BLOCKED` used honestly for the sections in §8.1 that are blocked
   on backend domains that don't exist rather than described as future
   "coming soon" UI work.

---

## 9. Summary table for `architect`'s roadmap skeleton

| Surface | MVP buildable today without new backend work | Blocking dependency before MVP can start | Recommended relative sequencing |
|---|---|---|---|
| Operator Back Office | Yes — 6 of 9 flagship workflow areas fully backed (§6.1/§6.2) | None blocking; two small near-term gaps noted (§6.3) don't block the stage | Should precede Partner Console |
| Partner Console | No — one new RBAC primitive required before any screen (§8.2 item 1) | A partner-scoped role/principal (`security` + `identity-compliance`); a write pattern decision (`architect` + `security`) | Should follow Back Office and its RBAC dependency's resolution |

## 10. Non-negotiables — final restatement

Both stages above, and every future wave adding a "coming soon" section to
real: the UI never owns financial/business rules; permissions are enforced
server-side only, never inferred from the UI; no brand-specific forked
backend logic ever becomes a code path in either surface.

## 11. PRH-2 K1 — scoped financial capability grants (ADR 0099): no UI yet

`internal/httpserver/capability_routes.go` exposes a full admin API for
requesting, approving/rejecting, cancelling and revoking a scoped financial
capability grant (`/v1/admin/tenants/{tenantID}/capability-grants/...`),
but **the Back Office has no screen for it yet** - this is a real,
disclosed gap, not an oversight to silently work around. When it is built,
per this document's own non-negotiables:

- **Grant request/approve/reject/revoke are server-enforced,** exactly as
  every other Back Office action - the UI only ever calls the existing API
  and reflects its response; it never locally decides who may approve a
  grant.
- **G-P1 (a platform-originated grant naming a tenant's own `finance`
  staff) has no route and must not get a UI control** - it is DEFERRED out
  of PRH-2 (ADR 0099 §4.1, architect ruling). A Back Office control for it
  would be a UI ahead of a backend capability that does not exist.
- **The four-eyes shape is visible, not just enforced.** A grant always
  shows both the requester and the approving `platform_admin` as distinct
  identities (never "self", by construction) - the UI surfaces this rather
  than only showing the current state, so an operator can see the
  four-eyes control actually held.
- **Revoke is the emergency stop** (ADR §8.1) and should be a prominent,
  single action in whichever screen lists a tenant's active grants -
  consistent with this codebase's existing kill-switch UI pattern
  (§6/§8.2's Back Office wave, mirrored for grants).

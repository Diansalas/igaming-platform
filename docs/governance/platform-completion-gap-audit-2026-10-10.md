# Platform completion gap audit (2026-10-10)

Status: **READ-ONLY audit. Documentation only. Nothing here is an approval, a legal/licensing statement or an authorisation.**
Branch `claude/focused-wright-jw88w9`, HEAD `81d3f42`. Migrations on disk: 0001..0127 (254 files). No tests were run for this audit;
every state below is read from source, migrations, route tables and the governance documents, and is LOCAL/MOCK evidence at best.

Goal: reconcile the repo against the Blueprint (`iGaming-Platform-Blueprint.pdf`, sections 4.1-4.9 and the vendor/own boundary) and find what
remains to reach **technical** production readiness with **no real external provider** (all providers MOCK), where legal/licensing approval
is a per-tenant/brand **manual launch authorisation control** (not a technical blocker) and **AWS deployment is not authorised** (IaC/runbooks only).

Label vocabulary (CLAUDE.md): IMPLEMENTED / PARTIALLY IMPLEMENTED / MOCK / NOT IMPLEMENTED. Gap tags:
`[PI-U]` PROVIDER-INDEPENDENT-UNBLOCKED (can be built now), `[HUMAN]` HUMAN-DECISION, `[LEGAL]`, `[PROV]` PROVIDER-DEPENDENT, `[AWS]` AWS-GATED.
Evidence citations are `file:line` (line numbers from HEAD `81d3f42`); where a claim is an absence it cites the place where the absence is recorded or
the search that shows it (grep over `internal/`, `cmd/`, `migrations/`, `backoffice/src`, `b2c/src`).

Correction of stale sources (do not rely on them): `docs/HANDOVER.md` says migrations 0001..0121 and HEAD `639a2f0`/`60f58ed`; the tree has 0001..0127
(0122 receipt attribution, 0123 payout instruments, 0124 hold resolution, 0125-0127 payout M4/binding). `docs/HANDOVER.md` section 5 still lists
"0001..0119". Registry and ADRs 0095/0110/0111 are authoritative.

---------------------------------------------------------------------

## 0. SPECIAL FOCUS - Brand / tenant launch authorisation

### 0.1 What exists (verified)

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| `tenants.status` column, CHECK active/suspended/closed | IMPLEMENTED (column only) | `migrations/0001_create_tenants.up.sql:14` | - |
| `brands.status` column, CHECK active/suspended/closed, **DEFAULT 'active'** | IMPLEMENTED (column only) | `migrations/0008_create_brands.up.sql:11` | - |
| Tenant created **immediately `active`** (hard-coded, no draft/pending state) | IMPLEMENTED as a defect for launch control | `internal/identity/tenant.go:225` | [PI-U] |
| Brand created **immediately `active`** (hard-coded) | IMPLEMENTED as a defect for launch control | `internal/identity/brand.go:134` | [PI-U] |
| Brand creation allowed to **`tenant_admin`** (PermBrandWrite) for its own tenant: a tenant-scoped role can mint an active brand with no platform approval | IMPLEMENTED; launch-control hole | `internal/auth/permission.go:818` (RoleTenantAdmin has PermBrandWrite), route `internal/httpserver/routes.go:85-86` | [PI-U] [HUMAN: who may create brands] |
| Tenant creation platform-admin only | IMPLEMENTED | `internal/httpserver/routes.go:65-66`, `internal/auth/permission.go:710` | - |
| `tenant.ChangeStatus` (active/suspended/closed), audited (before/after/reason_code/IP/request id), runs under `WithPlatformAdmin`, FOR UPDATE, 5 s lock_timeout; closure refused while open sportsbook bets (trigger `GP020`) | IMPLEMENTED but **no authz, no caller** | `internal/tenant/status.go:111-118` (the doc comment itself: "performs NO authorization check... No route calls it today"), `:120-175` | [PI-U] TENANT-STATUS-AUTHZ-1 |
| **No** function, route or UI to change `brands.status` (only INSERT; `grep "UPDATE brands"` over `internal cmd` = 0 hits) | NOT IMPLEMENTED | `internal/identity/brand.go:132-142`; `internal/tenant/brands_grant_pin_integration_test.go` (grant pin only) | [PI-U] |
| **No HTTP route** for tenant status change (route table has GET/POST tenants, brands, staff, licence only) | NOT IMPLEMENTED | `internal/httpserver/routes.go:65-95`; `docs/HANDOVER.md:202` ("no HTTP route exists") | [PI-U] |
| **No Back Office UI** for activation/suspension/reactivation: tenants pages are list/detail/create tenant/brand/staff only; status is a read-only Badge | NOT IMPLEMENTED | `backoffice/src/features/tenants/TenantDetailPage.tsx:46,61`, `BrandDetailPage.tsx:31`, `TenantListPage.tsx:23-25`, `backoffice/src/app/AppRoutes.tsx` (no status route) | [PI-U] |
| Money-initiation gate: HTTP deposit initiation, player withdrawal request, staff payout submit, cascade child fail closed unless tenant AND brand `active` (H-SEC-5/11, merged `3643cb0`) | IMPLEMENTED (MOCK) | `internal/tenant/payment_initiation_gate.go:38,66`; callers `internal/withdrawal/withdrawal.go:456`, `internal/payments/deposit_v2.go:141`, `payout.go:340`, `drive.go:164,181` | - |
| Gameplay gate: NEW casino/sportsbook postings refused when **tenant** non-active (brand NOT checked) | IMPLEMENTED for tenant; **brand missing** | `internal/tenant/gameplay_gate.go:37`; callers `internal/casino/orchestrator.go:960`, `internal/sportsbook/orchestrator.go:165`; migration 0118/0121 trigger `tenants_status_change_gate` | [PI-U] |
| Player register/login/refresh and staff login do **not** consult tenant or brand status (only player/staff account status) | NOT IMPLEMENTED | `internal/httpserver/auth_routes.go:259,362,371` (account/staff status only; no tenant/brand status read) | [PI-U] |
| Casino launch, sportsbook bet placement, bonus grant do **not** check brand status (only money initiation + tenant gameplay gate do) | NOT IMPLEMENTED | `grep "FROM brands" internal/` returns only `internal/identity/brand.go` and `internal/payments/sweeper_resolution_only.go` | [PI-U] |
| Kill switch (payments) as a *partial* suspension control: per-tenant, release is four-eyes, tenant-visible audit (ADR 0104) | IMPLEMENTED (MOCK) | routes `internal/httpserver/payments_kill_switch_handlers.go`, ADR 0104; it is a payments-only brake, not a launch/activation control | - |
| Licence registry: `jurisdictions`, `licences` (licensee platform/tenant, status active/suspended/expired, issued/expires, permitted_products/markets) | IMPLEMENTED | `migrations/0002_create_jurisdictions_and_licensing.up.sql:16-28` | - |
| Tenant licensing model `under_platform_licence` / `own_licence` + DB consistency (composite FK `tenants_licence_matches_model`, exclusive BYOL index `uq_tenants_exclusive_own_licence`) | IMPLEMENTED | `migrations/0001:13`, `0007`, `0017`, `0077`; `internal/jurisdiction/tenant_licence_admin.go:79-165` | - |
| Assign/unassign licence to tenant: platform-only (`PermTenantLicenceAssign`), `WithPlatformAdmin`, audited `jurisdiction_registry.tenant_licence_assigned` (platform-visible only; MKT-AUDIT-1 open) | IMPLEMENTED | `internal/jurisdiction/tenant_licence_admin.go:79,157`; route `internal/httpserver/jurisdiction_admin_routes.go:76`; perm `internal/auth/permission.go:364` | - |
| Create jurisdiction / licence (platform-only); **no licence status-transition op** (suspend/expire/renew) exists; the "status != active" branch is vacuous | PARTIALLY IMPLEMENTED | `internal/jurisdiction/registry_admin.go:124,311`; `tenant_licence_admin.go:96-101` comment | [PI-U] |
| Licence validity evaluator (as-of, expiry) | IMPLEMENTED but consumed only by the operating-market mechanism | `internal/jurisdiction/licence_validity.go:68`; consumer `internal/operatingmarket/resolve.go:259` | - |
| Operating-market / country policy (licence ceilings, per-country policy, per-brand policy, registration permitted?) | IMPLEMENTED as **mechanism only: zero HTTP routes, no production caller** (no importer of `internal/operatingmarket` outside the package and tests) | ADR 0045 header ("zero HTTP routes, zero production wiring"); `internal/operatingmarket/registration.go:46` (`IsRegistrationPermitted` unused by handlers) | [PI-U] wire; country content = [LEGAL] |
| `tenant_jurisdiction_configs` (per-tenant per-jurisdiction KYC/AML/RG/reporting ruleset ids, allowed currencies/payment methods, geo block list) | Table only; **no Go reader or writer** (only comments in `internal/withdrawal/policy.go:67`, `internal/payments/orchestrator.go:162`) | `migrations/0002...:49-63` | [PI-U] |
| Jurisdiction resolution + evidence collection activation per tenant (platform / tenant admin routes) | IMPLEMENTED (foundation); legal content is not supplied | routes `PUT /v1/admin/jurisdiction-resolution-active/{operationClass}`, `PUT /v1/admin/jurisdiction-evidence-collection/{evidenceType}`; `internal/jurisdiction/resolution_active.go`, `evidence_collection_active.go` | [LEGAL] content |
| Four-eyes machinery reusable for activation (capability grants K1, manual adjustment K2, force resolution K3, kill-switch release, hold resolution, provider credential change, bonus change requests) | IMPLEMENTED (pattern exists; not applied to status) | e.g. `internal/capability`, `internal/adjustment`, migration 0112/0113/0115/0124; signed actor proof ADR 0110 | - |

### 0.2 What is missing for the target control (activation / suspension / reactivation with manual approval)

| Required element | State today | Gap tag |
|---|---|---|
| Launch state machine on tenant AND brand (e.g. `draft/pending_launch` -> `active` -> `suspended` -> `active` / `closed`), **default non-active** on create | NOT IMPLEMENTED (both default `active`: `0001:14`, `0008:11`, `identity/tenant.go:225`, `identity/brand.go:134`) | [PI-U] (needs migration; number allocated by orchestrator only) |
| Manual-approval request + approver (actor + timestamp) persisted on the entity (or a launch-authorisation table) | NOT IMPLEMENTED (`ChangeStatus` writes status + audit only; no approver column, no request table; `tenants` has only `updated_at`) | [PI-U] |
| Four-eyes: requester != approver, DB-enforced, signed actor proof for the approve step | NOT IMPLEMENTED for status (pattern exists elsewhere, ADR 0099/0110) | [PI-U] + security review |
| Server-side authz on status change (new platform-only permission, never in tenant_admin; step-up/MFA per ADR 0017) | NOT IMPLEMENTED (TENANT-STATUS-AUTHZ-1 OPEN; `status.go:111-118`) | [PI-U] |
| HTTP routes (request / approve / reject / cancel / suspend / reactivate) + OpenAPI | NOT IMPLEMENTED (`docs/api/openapi/platform-api.yaml` has no such path) | [PI-U] |
| Back Office UI (launch checklist, request, approval queue, status history) | NOT IMPLEMENTED | [PI-U] |
| Conditions / notes / reason code captured and displayed | PARTIAL: `reason_code` required and audited (`status.go:134-136,165-171`); no free-text conditions/notes field, no PII-safe note policy (K3 note retention is a [LEGAL] open item) | [PI-U] + [LEGAL] retention |
| Jurisdiction config as a launch prerequisite (at least one jurisdiction + operating-market policy for the brand) | NOT IMPLEMENTED (resolution machinery exists but nothing reads it at launch) | [PI-U] |
| Licensing basis recorded per launch: `own licence` / `platform licence` / `manual-approved other` | PARTIAL: `licensing_model` has only two values (`0001:13`); no "manual-approved other / attested" basis; licence link optional (`tenants.licence_id` nullable, `0002:28`) | [HUMAN] (model change) + [PI-U] |
| Licensing status gate (licence active, not expired, covers the brand's products/markets) evaluated at activation and continuously | NOT IMPLEMENTED at the activation point (evaluator exists, `licence_validity.go:68`; no licence suspend/expire transition, no sweeper that auto-suspends on expiry) | [PI-U] |
| Responsible operator / accountable person recorded for the brand (named contact, approver of record) | NOT IMPLEMENTED (`grep` for responsible_operator/launch_approved/go_live in migrations and `internal/` = no hit outside payments kill-switch wording) | [PI-U] |
| Launch readiness checks (automated pre-activation checklist: provider capability rows present, credentials resolved, assets authorised, KYC/RG/withdrawal policy present, jurisdiction + licence valid, alert routing ready, kill switch state, reconciliation clean) | NOT IMPLEMENTED (only per-area readiness reports exist: `alert_routing_ready` `internal/httpserver/alert_routing_handlers.go:398`, `runbooks/production-configuration-checklist.md`) | [PI-U] |
| Tenant-visible audit of platform activation/suspension | PARTIAL: ADR 0104 covers the kill switch only (`GET /v1/admin/audit-log/platform-actions`); status change audit is platform-scope (`status.go:165`) | [PI-U] |
| Brand gate on every player-facing entry (register, login, launch, bet, bonus) and on gameplay postings | NOT IMPLEMENTED (see 0.1) | [PI-U] |
| Cascade-child + approved-withdrawal behaviour on suspended brand (owner decisions pending) | OPEN: HD-PRH2 H(8) (brand in resolution-only sweep) and "approved withdrawals have no release path" (registry `H-SEC-5-11-IMPLEMENTED-2026-10-08`, HANDOVER 31) | [HUMAN] |
| Legal/licensing approval itself | By design NOT code: becomes the manual approver's attestation recorded in the launch record | [LEGAL] (content), [HUMAN] (who approves) |

Conclusion for 0: the primitives (status columns, platform-admin scope, audit helper, licence registry, four-eyes pattern, money/gameplay gates) exist, but there
is **no launch-authorisation control at all**: every tenant and brand is born `active`, a `tenant_admin` can mint an `active` brand, `ChangeStatus` is unreachable
and unauthorised, brand status cannot be changed by any code path, and player entry points ignore tenant/brand status.

---------------------------------------------------------------------

## 1. Identity / tenancy / multi-brand / RBAC / audit / config / jurisdiction / assets / FX / events / observability

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Tenant isolation: RLS on tenant-owned tables, `tenant_id` from server context only | IMPLEMENTED | `internal/tenant/tenant.go:1-9`, `internal/db/tenant_rls.go:112`, ADR 0002/0013/0016 | - |
| Brand as distinct entity (ADR 0012), per-brand player accounts | IMPLEMENTED | `migrations/0008_create_brands.up.sql`, `internal/identity/brand.go` | - |
| Brand configuration editing (theme, locale, domains, languages, currencies, payment methods, RG defaults) from the partner console | PARTIALLY IMPLEMENTED: `brands.theme`/`default_locale` columns exist, **no update route**; no domain table; payment methods via `provider_capabilities` routes only; B2C brand is build-time | `migrations/0008:12-13`; route table (no PATCH brand); `docs/HANDOVER.md:203` | [PI-U] |
| Versioned configuration (CLAUDE.md "versioned, editable") | PARTIAL: versioning exists for bonus campaigns/offers, financial policy changes, withdrawal policies; not for brand config | `bonus_campaign_versions`, `financial_approval_policy_changes` migrations | [PI-U] |
| Staff RBAC (permission map, server-side) | IMPLEMENTED | `internal/auth/permission.go:710,817` | - |
| Staff lifecycle (audited suspend / role change / owner-only) | NOT IMPLEMENTED (STAFF-LIFECYCLE-1; H1/T3 identity-store hardening deferred) | HANDOVER 29, 31 | [PI-U] STAFF-LIFECYCLE-1; T3/H1 [HUMAN] |
| Staff MFA / step-up | IMPLEMENTED (ADR 0017/0018) per ADR set; verify per route before launch | `docs/decisions/0017*`, `0018*` | - |
| Scoped financial capability grants (K1) | IMPLEMENTED; G-P1 deferred | ADR 0099, migration 0112 | [HUMAN] G-P1 |
| Audit: append-only, dual-scope RLS, IP/actor/reason | IMPLEMENTED | ADR 0013, `internal/audit`; routes `GET /v1/admin/audit-log`, `/platform/audit-log` | - |
| Audit retention / export / WORM | NOT IMPLEMENTED beyond table (ALERT-RETENTION-1 for alerts) | HANDOVER 30 | [PI-U] design, [LEGAL] retention |
| Jurisdiction model (first-class, pluggable) | IMPLEMENTED foundation; legal content absent | ADR 0006/0043/0045/0046 | [LEGAL] content |
| Operating market wiring into registration/deposit/withdrawal/wagering | NOT IMPLEMENTED (mechanism only) | ADR 0045 header; no importer | [PI-U] |
| Geo-blocking (geolocation provider) | MOCK | `internal/geolocation/mock_provider.go` | [PROV] |
| Asset / currency registry, per-asset exponent, platform authorisation, operation eligibility | IMPLEMENTED | `internal/assetregistry/*`, routes `/v1/admin/assets*`, ADR 0021/0037 | - |
| Registry change-requests with approvals | IMPLEMENTED | routes `POST /v1/admin/assets/change-requests*` | - |
| FX / conversion service and `ConversionOperation` between wallets of different assets | NOT IMPLEMENTED (no rate source, no wallet-to-wallet conversion ledger path; only bonus conversion exists) | `internal/assetregistry/types.go:14`, `internal/ledger/ledger.go:136` (bonus only); no fx/rate table in migrations; HANDOVER 20 "FX / rate source NOT IMPLEMENTED", KYC-FX-AGG-1 | [PI-U] (mock rate source + conversion op); live rates [PROV] |
| Event bus | STUB: in-memory only, **no non-test importer** | `internal/eventbus/eventbus.go:60-90`; `grep eventbus. internal cmd` = no external use | [PI-U] (transactional outbox) |
| Observability: structured logs, Prometheus metrics, tracing hooks, health/readiness | PARTIALLY IMPLEMENTED (code present; alert rules not wired; no backend) | `internal/observability/*`, `internal/httpserver/health.go:26,37`; `docs/runbooks/observability-and-alerting.md` | [PI-U] rule files; backend [AWS] |
| Alert pipeline (durable alerts, ack/resolve routes, routing readiness) | IMPLEMENTED (log sink, MOCK); **delivers to no human** (ALERT-DELIVERY-1) | `internal/alerting`, routes `/v1/admin/alerts*`, ADR 0102 s18 | channel/recipient [HUMAN]; adapter interface [PI-U] |
| Secret store / provider credential model | IMPLEMENTED (devfile/memory; `awssm` exists, AWS off) | ADR 0093/0094, `internal/secretstore` | production backend [AWS] |

---------------------------------------------------------------------

## 2. Ledger / wallet / payments orchestration

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Append-only double-entry ledger, integer minor units, NUMERIC for crypto, tombstones | IMPLEMENTED | ADR 0001/0019/0020/0082, `internal/ledger`, `internal/money` | - |
| Multi-wallet per player per asset | IMPLEMENTED | `internal/wallet/wallet.go`, ADR 0007 | - |
| Balance projection + hourly recompute/diff (P1 on drift) | IMPLEMENTED (scheduler in platform-api) | `internal/reconciliation/scheduler.go`, `cmd/platform-api/main.go:493` | - |
| Reconciliation Back Office read API: **only the casino stream** has routes; ledger-vs-projection, payment_statement, sportsbook_settlement runs/mismatches have **no admin read route or UI** | PARTIALLY IMPLEMENTED | `internal/httpserver/casino_reconciliation_handlers.go:100,147`; `internal/reconciliation/read.go:58,93` (casino kinds only) | [PI-U] |
| Deposits (`InitiateDepositAttempt` sole path), INV-DEP-1 single authoritative success | IMPLEMENTED (MOCK PSP) | HANDOVER 10; migration 0107 | real PSP [PROV] |
| Withdrawals state machine, approvals, policies, holds | IMPLEMENTED | `internal/withdrawal`, routes `/v1/admin/withdrawals*`, `withdrawal-policies` | WD-RG-1 [HUMAN] |
| Payouts, payout instruments, destination binding (B13 / ADR 0111) | IMPLEMENTED (MOCK) | migrations 0123, 0125-0127, ADR 0111; routes `/v1/me/payout-instruments`, `/v1/admin/players/{id}/payout-instruments` | non-MOCK [PROV] |
| Payment sweeper, kill switch, force resolution (K3 M1/M2/M4), manual adjustments (K2), hold resolution | IMPLEMENTED (MOCK), four-eyes DB-enforced | migrations 0105, 0113, 0115, 0124; routes `/v1/admin/tenants/{tenantID}/payment-force-resolutions*`, `manual-adjustments*`, `withdrawal-hold-resolutions*` | - |
| Parked / disputed transactions: operator resolution paths | PARTIALLY IMPLEMENTED: open items PAY-PAYOUT-UNBOUND-RESOLVE-1, PAY-PAYOUT-CONTRADICTION-HOLD-1, GAP-AAM (amount_asset_mismatch park has no governed exit) | HANDOVER 30, 31; `docs/governance/open-owner-questions-2026-10-09.md` | [HUMAN] design + [PI-U] after ruling |
| Back Office UI for parked txns, force resolution, manual adjustment, kill switch, hold resolution | NOT IMPLEMENTED (backoffice has withdrawals queue/detail only; `grep` for kill-switch/force-resol/manual-adjust in `backoffice/src` hits only `auth/permissions*`) | `backoffice/src/features/*` listing; `docs/governance/operator-console-governed-flows-brief.md` | [PI-U] |
| Chargebacks / card disputes as a domain | NOT IMPLEMENTED (only payment "disputed" park state and alerts) | `internal/payments/alerts.go`; no chargeback table in migrations | [PROV] shape; internal case model [PI-U] |
| Statement/recon source for payments | MOCK source | `internal/reconciliation/payment_statement*.go` | real [PROV] |
| Crypto custody | NOT IMPLEMENTED (interface only) | ADR 0008 | [HUMAN]/[PROV] |
| Alert delivery to humans | NOT IMPLEMENTED | HANDOVER 20, 33 item 3 | [HUMAN] |
| TRIGGER-SEARCH-PATH-1 deployed-and-verified, PLAT-ROLESPLIT-1 | code done; verification not executed | HANDOVER 29, `runbooks/plat-rolesplit-staging-verification.md` | [AWS] verification; local-DB drill [PI-U] |
| Remaining PRH-2 items (R3 launch conditions, PAY-K3-FOLLOWUPS-1, MANUAL-ADJ-LINK-1, STMT-TABLE-INSERT-RLS-1, MA020-K2-VISIBILITY-1) | OPEN | HANDOVER 29-30 | mix [PI-U] / [HUMAN] |

---------------------------------------------------------------------

## 3. Casino

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Provider abstraction, catalogue, availability per tenant/brand, change-request governance | IMPLEMENTED (MOCK) | `internal/casino/*`, routes `PUT /v1/admin/casino/games`, `.../change-requests` | - |
| Launch-token bootstrap, wallet callbacks (wager/win/rollback), idempotency, tombstones | IMPLEMENTED against MOCK | ADR 0103, `internal/casino/bootstrap.go`, `orchestrator.go`; webhooks `/v1/webhooks/casino/...` | real [PROV] |
| Player play simulation (B2C trust boundary) | MOCK | ADR 0048, routes `/v1/me/casino/sessions/{id}/wager|win|rollback` | - |
| Round lifecycle (open round representable, round close signal) | NOT IMPLEMENTED (Q-GP-6) | HANDOVER 11 | [HUMAN] |
| CAS-GAME-KILL-BET-1, CAS-BET-REQUIRES-BOOTSTRAP-1, CAS-PLAYER-REF-1, CAS-WIN-ANOMALY-1, CAS-STMT-IO-1, CAS-WIN-IDEMP-1, Q-GP-2/3/4 | OPEN | HANDOVER 11, 31 | [PI-U] mostly; Q-GP [HUMAN] |
| Casino reconciliation (statement vs rounds) | IMPLEMENTED (MOCK statement); CAS-RECON-SCALE-1 open | `internal/reconciliation/casino_statement.go`, `casino_consistency.go` | scale [PI-U] |
| Jackpots / tournaments / live-casino specifics | NOT IMPLEMENTED (architecture only) | docs/architecture/18 | out of MVP scope [HUMAN] |

## 4. Sportsbook

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Catalogue model: sports/competitions/events/markets/selections | IMPLEMENTED (read + sync from provider) | `internal/sportsbook/types.go:85-190`, `catalogue.go`; tables `sb_*` | - |
| Sports-data provider abstraction | PARTIALLY IMPLEMENTED: `Provider` has **only `Catalogue(ctx)`**; no odds-update stream, results feed, or event-state feed | `internal/sportsbook/types.go:370-372` | [PI-U] interface; real [PROV] |
| Mock sportsbook (static events) | MOCK | `internal/sportsbook/mock.go:31-130` | - |
| Betslip / placement: singles only; exposure limits and jurisdiction restrictions enforced | IMPLEMENTED (singles); multiples/accumulators/system bets NOT IMPLEMENTED | `internal/sportsbook/orchestrator.go:165`, `bets.go`; ADR 0038/0083 | [PI-U] design (needs ruling on product scope [HUMAN]) |
| Settlement / void / rollback (re-settlement generations, tombstones) | IMPLEMENTED in-house MOCK; trigger is a **staff `simulate-settlement-event` route**, no results-feed consumer | `internal/sportsbook/settlement.go:327,622,726,789`; route `POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event` (`internal/httpserver/sportsbook_settlement_handlers.go:20-27`) | feed consumer [PI-U] against mock results; real [PROV] |
| Bonus/mixed-funded sportsbook wagering | BLOCKED/NOT IMPLEMENTED | ADR 0038 status note | [PI-U] after bonus rulings |
| Cashout, partial settlement, bet-builder, live in-play | NOT IMPLEMENTED | ADR 0038 status note (line 5) | [HUMAN] scope; [PI-U] after |
| Operator catalogue/market management UI (suspend market, void event) | NOT IMPLEMENTED (BO sportsbook page is bets list only) | `backoffice/src/features/sportsbook/SportsbookBetsPage.tsx` | [PI-U] |
| Sportsbook reconciliation vs settlement statement | IMPLEMENTED (MOCK source) | `internal/reconciliation/sportsbook_settlement.go`, `internal/sportsbook/mock.go:164-177`; no admin read route | [PI-U] |
| Go-live gate HDR-SB-1 / HDR-J-7 | OPEN | ADR 0083 s8.2 | [LEGAL] -> becomes launch-authorisation condition |

## 5. Bonus

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Campaigns, offers (versioned), suggestions, grants, wagering progress, conversion, expiry, cashback, bulk jobs, change-request four-eyes | IMPLEMENTED (waves 1-3, migrations 0050-0070) | `internal/bonus/*`; routes `/v1/admin/bonus/*`, `/v1/bonus/*` | - |
| Bonus ledger accounts (HR-9 guard rejects postings until the expense generator exists) | PARTIALLY IMPLEMENTED | HANDOVER 13 | [PI-U] |
| ADR 0042 G-2 configurability, self-exclusion auto-void consumer | NOT IMPLEMENTED (decided, not built) | HANDOVER 13, 34 | [PI-U] (authorised by ADR 0042); CAS-WIN-IDEMP-1 before bonus-funded stakes |
| Wave 4, gamification/tournaments/missions/rewards/segmentation/CRM/affiliate | NOT IMPLEMENTED (not authorised) | project-status; docs/architecture/17-25,30-32 | [HUMAN] |

## 6. KYC / AML

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| KYC provider interface, MOCK vendor, verification + documents, malware scan, storage | IMPLEMENTED/MOCK | `internal/kyc/provider.go`, `mock_provider.go`, `malware.go`, `storage.go`; routes `/v1/me/kyc/*`, `/v1/admin/kyc/*` | real [PROV] |
| Async submission outbox, dedicated worker identity, terminal-failure alert | IMPLEMENTED (MOCK) | migration 0114, `internal/kyc/outbox_worker.go` | KYC-OUTBOX-REQUEUE-1 [PI-U] |
| Enforcement (block play/withdrawal/deposit) with configurable policies and decision log | PARTIALLY IMPLEMENTED (KYC-ENFORCE-1); thresholds HD-KYC-1..8 undecided | `internal/kyc/enforcement.go`, routes `/v1/admin/kyc/enforcement-decisions` | thresholds [LEGAL]/[HUMAN]; remaining code [PI-U] |
| KYC webhook admission / auth | IMPLEMENTED | `/v1/webhooks/kyc/...`, ADR 0091/0097 | - |
| AML transaction monitoring, sanctions/PEP screening, SAR/STR case management, source-of-funds workflow | NOT IMPLEMENTED (no code or tables; `grep sanction|pep|watchlist` finds no domain code) | `internal/kyc/types.go` (no AML types beyond naming) | [PI-U] internal rules/case model with MOCK screening; screening list [PROV]; thresholds [LEGAL] |
| Back Office KYC queue/case review | IMPLEMENTED | `backoffice/src/features/kyc/*` | - |

## 7. Responsible gaming (RG)

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Self-exclusion (player self-service + staff restriction), open-bet policy, enumeration sweep | IMPLEMENTED | `internal/rg/rg.go:186,277`, `self_exclusion_policy.go`, `enumeration_sweep.go`; routes `/v1/me/rg/*`, `/v1/admin/rg/restrictions` | - |
| Restriction types: **only `self_exclusion`** exists (no cooling-off, time-out, deposit/loss/wager/session limits, reality checks) | NOT IMPLEMENTED | `internal/rg/rg.go:38-41` (enum has a single value; CHECK constraint admits no other) | [PI-U] mechanism; parameters per jurisdiction [LEGAL] |
| Player-set deposit/loss limits (cool-down on increase) | NOT IMPLEMENTED (risk engine has amount rules but they are operator-configured, not player-set) | `internal/risk/types.go:30-34` | [PI-U] |
| RG gate on withdrawal (WD-RG-1) | OPEN | HANDOVER 15, 31 | [HUMAN] |
| Jurisdiction-specific RG rules | mechanism via jurisdiction; content absent | ADR 0026, 0043 | [LEGAL] |
| BO RG queue | IMPLEMENTED (view + create restriction) | `backoffice/src/features/rg/*` | - |

## 8. Risk

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Risk and limits engine: hard/configurable/signal rules, fail-closed, exponent-aware, cumulative | IMPLEMENTED | `internal/risk/*`, routes `/v1/admin/risk/rules*`, ADR 0031 | - |
| Rule kinds: min/max/cumulative amount only (no velocity/count, device, multi-account, bonus-abuse, fraud scoring) | PARTIALLY IMPLEMENTED | `internal/risk/types.go:30-34`; comment "future non-monetary kind" | [PI-U] |
| Risk review queue (REVIEW outcomes -> case handling) | PARTIAL: outcomes exist, no BO queue | no `risk` feature in `backoffice/src/features` | [PI-U] |
| BO Risk rules UI | NOT IMPLEMENTED (API only) | `backoffice/src/features` listing | [PI-U] |
| Cumulative sportsbook exposure | IMPLEMENTED with named deferrals | ADR 0047/0083, `internal/sportsbook/exposure*.go` | - |

## 9. Retail / POS

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Operator / partner / super agent / agent / cashier / terminal / float (hierarchy, RBAC, float ledger, cash-in/out, ticket/voucher) | NOT IMPLEMENTED: no package, no tables, no routes | `docs/HANDOVER.md:256-258`; `docs/architecture/26-retail-operations-architecture.md`; ADR 0035 (Proposed), 0036 (not implemented); no `retail`/`agent` package under `internal/` | [HUMAN] (owner authorisation; deferred); design ready; no provider needed |

## 10. B2C app

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Register/login, account page, email verification, compliance status, deposit, withdrawal, payout instruments, wallet, casino lobby/launch, sportsbook, betslip, bets | IMPLEMENTED against MOCK | `b2c/src/auth/*`, `b2c/src/features/{account,deposit,withdrawal,casino,sportsbook,betslip,bets}`, `b2c/src/api/*` | - |
| Self-exclusion UI, KYC upload UI, responsible-gaming limits UI, bonus UI, residence declaration UI | PARTIAL/UNVERIFIED: API exists (`/v1/me/rg/self-exclusion`, `/v1/me/kyc/*`, `/v1/bonus/*`, `PUT /v1/me/residence`); `b2c/src/api/compliance.ts` and `ComplianceStatusCard.tsx` exist, no bonus feature dir | `b2c/src/features/` (no bonus/rg dir), `b2c/src/api/compliance.ts` | [PI-U] |
| Brand is build-time (one deployment per brand), no runtime brand/theme fetch | PARTIALLY IMPLEMENTED | `b2c/src/config/brand.ts`, HANDOVER 8 | [PI-U] |
| Handling of suspended/closed/pre-launch brand (maintenance/"not available" screen) | NOT IMPLEMENTED | no tenant/brand status in `b2c/src`; backend ignores status at login | [PI-U] |
| i18n / multi-language, geo-block UX | not verified; locale column exists | `migrations/0008:12` | [PI-U] |

## 11. B2B / partner console

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Tenant/brand/staff creation, tenant detail, my-tenant, provider/casino config forms, players, withdrawals, bonus, KYC, RG, sportsbook bets, casino rounds, audit log, platform audit log, casino catalogue | IMPLEMENTED (MVP) | `backoffice/src/layout/nav.ts:17-34`, `backoffice/src/app/AppRoutes.tsx` | - |
| Partner self-service onboarding (branding, domains, payment methods, currencies, languages, RG defaults, bonus templates as config) | PARTIAL (provider/casino config only) | `backoffice/src/features/configuration/*` | [PI-U] |
| Second-tenant dry run / B2B onboarding runbook | NOT DONE (Stage 7+ authorisation needed) | HANDOVER "B2B" | [HUMAN] authorise; then [PI-U] |
| Reporting / BI (CDC, ClickHouse) | NOT IMPLEMENTED (deferred) | Blueprint 4.9; HANDOVER 34 | [PI-U] basic reports; CDC infra [AWS] |
| Partner API keys / white-label domains | NOT IMPLEMENTED | no tables | [PI-U] |

## 12. Back Office UI coverage of governed workflows

| Workflow | API | UI | Gap tag |
|---|---|---|---|
| Withdrawals (queue, detail, approve/reject/submit/resolve) | IMPLEMENTED | IMPLEMENTED (`backoffice/src/features/withdrawals/*`) | - |
| Parked / disputed payments, force resolution (K3), hold resolution, M4 | IMPLEMENTED | NOT IMPLEMENTED | [PI-U] |
| Reconciliation runs/mismatches (ledger, payments, sportsbook) | casino only | NOT IMPLEMENTED (only casino rounds page) | [PI-U] |
| KYC | IMPLEMENTED | IMPLEMENTED | - |
| RG | IMPLEMENTED | IMPLEMENTED (queue) | - |
| Risk rules | IMPLEMENTED | NOT IMPLEMENTED | [PI-U] |
| Bonus (campaigns, change-requests) | IMPLEMENTED | PARTIAL (campaign list, change-request queue; no offers/suggestions/grants/bulk jobs UI) | [PI-U] |
| Tenant / brand management | create + read | create + read only; **no status/launch** | [PI-U] |
| Approvals inbox (cross-domain four-eyes: K1, K2, K3, kill-switch release, asset change, credential change, financial-policy change) | per-domain approve routes exist | NOT IMPLEMENTED (no unified inbox; `permissions.ts` only) | [PI-U] |
| Audit log (tenant + platform) | IMPLEMENTED | IMPLEMENTED | - |
| Alerts (list/ack/resolve, routing status) | IMPLEMENTED | NOT IMPLEMENTED | [PI-U] |
| Capability grants (K1), manual adjustments (K2), kill switch | IMPLEMENTED | NOT IMPLEMENTED | [PI-U] |
| Assets / jurisdictions / licences registry | IMPLEMENTED | NOT IMPLEMENTED | [PI-U] |
| Provider credential change requests | IMPLEMENTED | NOT IMPLEMENTED (config page covers capabilities only) | [PI-U] |

## 13. Infrastructure / operations

| Item | State | Evidence | Gap tag |
|---|---|---|---|
| Docker images (platform-api, SPA), local dev compose | IMPLEMENTED | `deploy/docker/*`, `deploy/docker-compose.dev.yml` | - |
| AWS IaC (Terraform modules: network, database, ecs, alb, edge, dns, ecr, iam, secrets, security, observability), deployer scripts, static policy tests | IMPLEMENTED as code; staging deployed once then torn down; AWS OFF | `deploy/aws/modules/*`, `deploy/aws/scripts/deploy.sh`, `deploy/aws/tests/run-static-checks.sh`, `docs/governance/staging-teardown-2026-09-26.md` | [AWS] execution |
| CI | workflows exist; GitHub CI blocked by billing (CI-BILLING-1); self-hosted prepared not active | `.github/workflows/ci.yml`, `ci-selfhosted.yml`, HANDOVER 27/46 | [HUMAN] |
| Health / readiness endpoints | IMPLEMENTED | `internal/httpserver/health.go:26,37` | - |
| Metrics / alert rules | PARTIAL: metrics emitted, rules documented ("not wired") | `docs/runbooks/observability-and-alerting.md:288` | rule files [PI-U]; backend [AWS] |
| Backup / restore / DR | **NOT MET**: no backup job, no restore ever exercised, no replica | `docs/runbooks/backup-and-disaster-recovery.md:1-60` | local backup/restore drill + scripts [PI-U]; managed PITR [AWS] |
| Migrations: up/down pairs, checksum verify, forward-only in deployed envs, `down` refused outside development/staging | IMPLEMENTED; rollback in prod = restore or forward-fix; CI reversibility covers last 4 steps only | HANDOVER 22, `cmd/migrate` | [PI-U] expand/contract runbook |
| Runtime role separation (`igaming_runtime`, TEMP revoked) | IMPLEMENTED in code; deployed verification not done | migration 0116, `docs/security/runtime-role-separation.md` | local verification [PI-U]; deployed [AWS] |
| Runbooks | operational-runbooks, payout-instrument-keys, staging lifecycle, production-configuration-checklist present; no launch/suspension runbook, no incident/on-call runbook | `docs/runbooks/*` | [PI-U] |
| Security: webhook edge/rate-limiting, secrets, dependency scanning (govulncheck in CI) | PARTIAL (WEBHOOK-EDGE-1 open) | `ci.yml:194`, HANDOVER 29 | edge [AWS] |
| Load / soak / chaos testing | NOT IMPLEMENTED | `docs/testing/testing-strategy.md` | [PI-U] |

---------------------------------------------------------------------

## Prioritised dependency-ordered list: top unblocked provider-independent tasks (max 15)

All are `[PI-U]` unless noted; all need a migration number allocated by the orchestrator only. Reviewers named per CLAUDE.md. Tasks 1-5 are the launch-authorisation spine and must run in order.

1. **LAUNCH-STATE-MODEL**: ADR + migration: tenant and brand launch state (`pending_launch|active|suspended|closed`), default non-active, `launch_authorisations` table (request, approver actor + timestamp, conditions/notes, licensing basis, licence ref, responsible operator, jurisdiction refs), append-only history; backfill existing rows to `active`. Resolve `[HUMAN]` items first (licensing basis values, who may create brands). Review: architect, security, ledger-finance (gameplay/money gate interaction), code-reviewer.
2. **TENANT-STATUS-AUTHZ-1**: platform-only permission, step-up/MFA, four-eyes + signed actor proof for activate/reactivate (suspend may be single-actor emergency), removing the unauthorised `ChangeStatus` entry point; brand status change function (none exists). Review: security, ledger-finance (closure/gameplay trigger), qa, code-reviewer.
3. **LAUNCH-ROUTES-API**: HTTP request/approve/reject/cancel/suspend/reactivate for tenant and brand + OpenAPI + tenant-visible audit (extend ADR 0104) + `tenant_admin` brand create made non-active. Review: security, architect, qa.
4. **BRAND-STATUS-GATES**: apply tenant+brand status gate to player register/login/refresh, casino launch/callbacks (new wagering), sportsbook placement, bonus grants; close HD-PRH2 H(8) once ruled. Review: security, ledger-finance, qa.
5. **LAUNCH-READINESS-CHECKS**: server-side readiness evaluator (provider capability + credential handle present, assets authorised, KYC/withdrawal/RG policy present, jurisdiction + licence valid via `EvaluateLicenceValidity`, operating-market policy, alert routing ready, kill switch, recon clean) returned by API and required to be green (or explicitly waived with reason) at approval. Review: architect, security, qa, product-owner-proxy.
6. **LICENCE-LIFECYCLE**: licence suspend/expire/renew transitions (audited, platform-only) plus continuous check that auto-flags or suspends a launched brand on licence expiry; wire `operatingmarket` (`IsRegistrationPermitted`, ceilings) into registration/deposit/withdrawal/wagering call sites; `tenant_jurisdiction_configs` reader/writer. Review: architect, security, code-reviewer.
7. **BACK-OFFICE-LAUNCH-UI**: launch checklist, request/approval queue, status history, suspend/reactivate dialogs (server remains authority) + B2C "not available/maintenance" state. Review: security, qa, product-owner-proxy.
8. **BACK-OFFICE-GOVERNED-FLOWS-UI**: parked/disputed payments, force resolution K3, hold resolution, manual adjustments K2, kill switch, capability grants K1, unified approvals inbox, alerts (per `operator-console-governed-flows-brief.md`). Review: security, ledger-finance, qa.
9. **RECONCILIATION-ADMIN-READ + UI**: read routes and UI for ledger_vs_projection, payment_statement, sportsbook_settlement runs/mismatches (casino exists). Review: ledger-finance, security, qa.
10. **RG-LIMITS-MECHANISM**: new restriction types (time-out/cool-off, player-set deposit/loss/wager/session limits with cool-down on increase) and enforcement hooks; values per jurisdiction left to `[LEGAL]` configuration. Review: security, ledger-finance (limit enforcement on money path), architect.
11. **AML-INTERNAL-FOUNDATION**: transaction-monitoring rules (velocity, structuring), case/SAR workflow, screening provider interface with MOCK; thresholds as configuration (HD-KYC via `[LEGAL]`). Review: security, architect, qa.
12. **TRANSACTIONAL-OUTBOX + EVENT BUS**: replace in-memory bus with outbox-based events (needed by audit/BI/alerts/sportsbook feeds); no external broker. Review: architect, ledger-finance, qa.
13. **SPORTSBOOK-FEED-ABSTRACTION**: extend `Provider` with odds-update and results/event-state feeds; mock feed driving settlement and market suspension (replacing staff simulate route as the trigger path); catalogue fetch timeout. Review: architect, ledger-finance, security, qa.
14. **FX-CONVERSION (MOCK RATE SOURCE)**: `ConversionOperation` wallet-to-wallet with quote, rate snapshot, two-sided ledger posting, idempotency, authorisation by asset eligibility; mock rate source only. Review: ledger-finance, architect, security, qa.
15. **OPS-READINESS-LOCAL**: local backup/restore drill (pg_basebackup/PITR script, restore into scratch DB, RLS + role verification, RPO/RTO evidence), alert-rule files, launch/suspension and incident runbooks, expand/contract migration runbook, load/soak harness. Review: qa, security, architect.

Not in this list because gated: alert delivery channel/recipients [HUMAN], sandbox PSP and any real provider [PROV]/[HUMAN], AWS staging/production and managed backups [AWS], retail/POS and Wave 4 [HUMAN], Q-GP-6 and WD-RG-1 [HUMAN], HD-KYC thresholds, jurisdiction content, HDR-SB-1 [LEGAL] (these become fields on the launch authorisation record, task 1).

# Licensing model and operational workflows: audit (core)

- **Type:** read-only audit by `architect`. No code, migration, ADR, governance or HANDOVER file was changed.
- **Base:** branch `claude/licensing-jurisdiction-completion` at `7ae5d1a`, worktree `licaudit1`.
- **Scope:** (1) the licensing model, (2) multi-brand launch governance (ADR 0112), (3) licence evidence, validity and
  expiry, (4) controlled licence and market lifecycle surfaces, (5) negative tests for authorisation and four-eyes.
- **Method:** I read the code, migrations and tests directly. Where a claim rests only on a document, it is marked
  **DOC-ONLY**. Earlier summaries (freeze reports, registry rows, closure reports) were not trusted as evidence.
- **Not a legal opinion.** Nothing here says whether any licence is legally sufficient. Software capability is not
  regulatory approval.

## 0. What was and was not run

| Run | Result |
|---|---|
| `go test -count=1 ./internal/launchgov/ ./internal/jurisdiction/ ./internal/operatingmarket/ ./internal/marketgate/` (unit tests only, no `integration` tag) | all four `ok` |
| Integration tests on a private database | **NOT RUN.** The harness command was refused by the session's permission classifier, so I stopped and did not try another way in. Every `*_integration_test.go` citation below is from reading the test, not from running it. |
| Back Office vitest | NOT RUN (the worktree has no `node_modules`). Back Office test citations are from reading the tests. |

Labels follow the brief: IMPLEMENTED-VERIFIED (code + migration + a test I read and can cite), IMPLEMENTED-UNTESTED,
DB-FIELD-ONLY, API-ONLY (no Back Office), UI-ONLY, MOCK, DOC-ONLY, NOT IMPLEMENTED. Because of the run limits above,
"VERIFIED" here means "I read the test", not "I ran it", for every integration test.

## 1. The licensing model as built

### 1.1 Entities and how they relate

| Entity | Where | Facts |
|---|---|---|
| `tenants.licensing_model` | `migrations/0001_create_tenants.up.sql:13` | `under_platform_licence` or `own_licence`, NOT NULL, set per tenant at creation. |
| `jurisdictions` | `migrations/0002...up.sql:6-13` | Registry of regulatory jurisdictions; `code` is its own code space, not ISO country (`internal/validation/country.go:15`). |
| `licences` | `migrations/0002...up.sql:15-27` | `jurisdiction_id`, `licensee` (`platform`/`tenant`), `licence_number`, `status` (`active`/`suspended`/`expired`), `issued_at`/`expires_at` (DATE, nullable), `permitted_products`, `permitted_markets` (deprecated, no readers). **No** regulator reference, document/evidence reference, review date, `updated_at`, or history table. |
| `tenants.licence_id` | `migrations/0002...up.sql:29-30` | **One** licence per tenant (a single nullable FK). |
| Model and licensee consistency | `migrations/0007...up.sql` (composite FK `tenants_licence_matches_model`), `0017` (NOT NULL backstop) | `under_platform_licence` may bind only a `platform` licence; `own_licence` only a `tenant` licence. |
| BYOL exclusivity | `migrations/0077...up.sql`, `uq_tenants_exclusive_own_licence` | A `tenant`-licensee licence binds to at most one tenant. A `platform` licence is shared on purpose. |
| `tenant_jurisdiction_configs` | `migrations/0002...up.sql:35-52`, RLS `0005` | Per tenant and jurisdiction: ruleset ids, currencies, payment methods, geo block list, effective dating. |
| `licence_country_ceilings`, `operating_country_policies` | `migrations/0076` | The licence's country ceiling, then tenant/brand/operation opt-ins. Append-only and effective-dated. |
| `tenant_licences` (named in the brief) | - | **Does not exist.** No table, Go symbol or migration with that name. The tenant-to-licence relation is the single column `tenants.licence_id`. |
| Brand-level licence binding | `migrations/0008_create_brands.up.sql` | **None.** Brands carry no licence and no jurisdiction. A brand's licence is always its tenant's one licence. |

### 1.2 Answers to the model questions

- **Own-company-licence tenant:** `licensing_model='under_platform_licence'`, bound to a shared `licensee='platform'`
  licence. IMPLEMENTED-VERIFIED (`registry_rls_integration_test.go:702`
  `TestTenantsRLS_PlatformLicenceMayStillBeSharedAcrossManyTenants`).
- **B2B tenant with its own licence:** `licensing_model='own_licence'`, bound exclusively to a `licensee='tenant'`
  licence. IMPLEMENTED-VERIFIED (`internal/jurisdiction/tenant_licence_admin_integration_test.go:232`,
  `registry_rls_integration_test.go:634`).
- **Hybrid platform:** both models exist side by side, chosen per tenant. IMPLEMENTED-VERIFIED.
- **Hybrid within one tenant** (for example brand A under our licence and brand B under the operator's own licence, or
  one operator holding two licences in two jurisdictions): **NOT REPRESENTABLE.** `tenants.licence_id` is one column,
  and H-2 (`internal/launchgov/hard.go:150-216`) requires the launch request's `licence_id` to equal
  `tenants.licence_id`. A brand cannot launch under a second licence. See gap G-3.
- **Changing a tenant's licensing model** (moving from our licence to BYOL): **NOT IMPLEMENTED.** No Go code updates
  `licensing_model`. The only writer is `identity.CreateTenant` (`internal/identity/tenant.go:230-233`). The
  `tenants_platform_admin_update` policy would allow it from a platform session, but no audited path exists.
- **Universal licence flag:** **none found.** I grepped migrations, `internal`, `cmd` and `backoffice/src` for
  `anjouan|KM-ANJ` and for universal or `is_platform_licen*`-style flags. Every hit is a comment, an example in a
  column comment, or a test fixture (`backoffice/src/features/launch/launchFixtures.ts:100`). Risk rules scope
  licensing explicitly through `risk_rules.licensing_mode` (`migrations/0042...up.sql:66`). Casino, sportsbook, bonus
  and withdrawal resolve the tenant's own `licensing_model` (`internal/casino/orchestrator.go:816-830`,
  `internal/bonus/eligibility.go:220`, `internal/withdrawalgate/standard/standard.go:54`). A missing licensing mode
  fails closed when a rule is scoped by it (`internal/risk/evaluator.go:105-128`).
- **Places that assume "our licence":** two places pick our licence by default, and neither is a runtime gate:
  - The Back Office tenant form preselects `under_platform_licence`
    (`backoffice/src/features/tenants/CreateTenantForm.tsx:23`). The server has no default and requires one of the
    two values (`internal/httpserver/admin_routes.go:67`). Low severity (G-14).
  - The staging seed creates its synthetic tenant as `under_platform_licence`
    (`internal/stagingseed/world.go:348`). This is staging-only fixture code.

### 1.3 Two "licensing" vocabularies

ADR 0112 adds a second vocabulary on purpose: the **recorded basis** of a launch decision
(`platform_licence | tenant_licence | other_manually_approved | not_applicable_recorded_determination`). It is
separate from `tenants.licensing_model` (ADR 0112 §4.1; `internal/launchgov/model.go:12`). H-2 enforces consistency
for the licence-backed bases (`hard.go:150-216`). There is one real inconsistency between the two:

- A brand launched on `other_manually_approved` or `not_applicable_recorded_determination` with no licence bound **can
  be activated, but serves nobody**. The operating-market resolver's step 1 needs `tenants.licence_id`. If it is NULL,
  the result is `OutcomeLicensingUnknown` (`internal/operatingmarket/resolve.go:247-252`). The market gate refuses
  anything that is not `Permitted()` (`internal/marketgate/marketgate.go:374-377`), and it runs on registration,
  deposit, casino launch and sportsbook placement (`internal/httpserver/auth_routes.go:127`,
  `deposit_handlers.go:195`, `casino_handlers.go:224`, `sportsbook_handlers.go:406`). The R-MARKET-CONFIG readiness
  check would show `fail`, but readiness only needs acknowledging, not passing. See G-7.

## 2. Brand and tenant launch authorisation (ADR 0112)

| Capability | Label | Evidence |
|---|---|---|
| Statuses `pending_launch/active/suspended/closed` on tenants and brands; new rows default to `pending_launch` | IMPLEMENTED-VERIFIED | `migrations/0128`; `internal/tenant/launch_governance_integration_test.go:249` |
| No status change without a same-transaction governed transition (`LA020`), from any SQL session including the platform scope | IMPLEMENTED-VERIFIED | `launch_governance_integration_test.go:304-418` (tenant-scoped brand UPDATE, platform tenant UPDATE, raw reactivation, stale replays, owner reopen of a closed brand all give `LA020`) |
| `closed` is terminal (reopen refused) | IMPLEMENTED-VERIFIED | same test, `:411-418` |
| Per-brand and per-tenant subjects decided separately | IMPLEMENTED-VERIFIED | ADR 0112 §3.1; `flow_integration_test.go:16`, `:83` |
| Brand gameplay gate (non-active brand refuses new wagering, fail-closed) | IMPLEMENTED-VERIFIED | `migrations/0129`; `internal/tenant/brand_gameplay_gate_integration_test.go:176` |
| Registration and login gate on brand and tenant status | IMPLEMENTED-VERIFIED | `internal/httpserver/launch_registration_gate_integration_test.go:64`, `:135` |
| Activate (four-eyes: brand = requester + 1 platform approver; tenant = 2; S14 = 2) | IMPLEMENTED-VERIFIED | `flow_integration_test.go:16`; `launch_authorisation_http_integration_test.go:144`; `launch_governance_integration_test.go:1116` |
| Suspend (single actor, immediate; supersedes pending requests) | IMPLEMENTED-VERIFIED | `launch_authorisation_http_integration_test.go:756`; ADR 0112 §6.3 |
| Reactivate (always four-eyes) | IMPLEMENTED-VERIFIED | `launch_authorisation_http_integration_test.go:756`; `flow_integration_test.go:83` |
| Close (2 approvals; `LA023` with hold-bearing withdrawals, `LA024` with open brand bets, `LA025` ledger hold arm) | IMPLEMENTED-VERIFIED | `closure_integration_test.go:97`; `closure_hold_balance_integration_test.go`; `launch_authorisation_http_integration_test.go:708` |
| Ratify a legacy-baseline subject | IMPLEMENTED-VERIFIED | `ratify_integration_test.go` |
| Reason codes (closed catalogue, required) | IMPLEMENTED-VERIFIED | ADR 0112 §4.2; `rules2_integration_test.go:19` |
| Recorded licensing basis, operator, jurisdictions, conditions note (PII-linted) | IMPLEMENTED-VERIFIED | `rules2_integration_test.go:19` |
| Readiness snapshot + hash, explicit acknowledgement of every non-pass, stale-readiness refusal | IMPLEMENTED-VERIFIED | `rules_integration_test.go:108` |
| Signed actor proof on request, approval and cancel (GUC impersonation refused) | IMPLEMENTED-VERIFIED | `proof_integration_test.go:117`, `:182`, `:322`, `:413` |
| Audit (one row per event, denied rows for refusals; `subject_tenant_id` projection) | IMPLEMENTED-VERIFIED | `rules_integration_test.go:96-98`; `launch_authorisation_audit_projection_integration_test.go` |
| HTTP API (11 routes) + OpenAPI + golden step-up table | IMPLEMENTED-VERIFIED | `launch_authorisation_routes.go:56-78`; `docs/api/openapi/platform-api.yaml` (every path present); `staff_route_golden_test.go:95,140,149-156` |
| Back Office (panel on tenant and brand, request dialog, approval queue, suspend dialog, history) | IMPLEMENTED-VERIFIED | `backoffice/src/features/launch/*`; `LaunchPanel.test.tsx`, `LaunchQueuePage.test.tsx`, `Wording.test.tsx` |
| Evidence attached to a launch decision | PARTIALLY IMPLEMENTED (by design) | Only `determination_reference` (text, 1..200) and `determination_by_role`. No document storage. ADR 0112 §4.2. |
| Notification of launch events | NOT IMPLEMENTED | Only `launch.suspension_exhausted` raises a P1 alert row, and nobody is paged (`internal/launchgov/alerts.go:12-20`, ALERT-DELIVERY-1 OPEN) |

ADR 0112's design choices that matter to the licensing question (verified in code and stated in ADR §5.2 l.294-296):

- **An expired, suspended, not-yet-issued or unbound licence never blocks activation.** R-LIC-VALID is informational.
  `licenceCheck` returns `fail` with `licence_<validity>` (`internal/launchgov/readiness.go:399-411`), and the
  approvers may acknowledge it. H-2 checks only that the licensee, model and binding agree (`hard.go:180-212`). It
  never checks `licences.status` or dates.
- **The recorded `licensing_status` is self-declared.** A request may record `in_force` while `licences.status =
  'expired'`. Nothing cross-checks the two (`hard.go:160-168` validates the vocabulary only).
- **A licence read error makes R-LIC-VALID `not_evaluable`, which can be acknowledged** (`readiness.go:404-406`). ADR
  0112 §8.5 (l.398) says "unreadable ... licence ... refuses". In practice H-2 reads the same row earlier and refuses
  on a read error (`hard.go:187-191`), so I don't think this can be exploited. It is still a gap between the code and
  the ADR wording (G-12).

## 3. Licence evidence, validity, expiry and history

| Capability | Label | Evidence |
|---|---|---|
| Validity predicate (status + half-open `[issued_at, expires_at)` date interval, fail-closed enum) | IMPLEMENTED-VERIFIED | `internal/jurisdiction/licence_validity.go:68-124`; `licence_validity_integration_test.go:29,214,272,314,343,394` |
| Runtime effect of an invalid licence: the operating-market resolver refuses (`not_permitted_by_licence` / `licensing_unknown`), so the market gate refuses registration, deposit, casino launch and sportsbook wager | IMPLEMENTED; **only partly tested end to end** | `operatingmarket/resolve.go:247-285`. Only the not-yet-issued arm is tested through the resolver (`resolve_integration_test.go:508`). **No test drives a `suspended`, `expired` (status) or date-expired licence, or an unbound licence, through `ResolveOperatingCountryPolicy` or the market gate.** I grepped every test for `CeilingReasonLicenceExpired`, `CeilingReasonLicenceSuspended` and `OutcomeLicensingUnknown`: zero hits. So the expired and suspended arms are IMPLEMENTED-UNTESTED at the resolver and gate level (G-6). |
| Licence evidence (document reference, regulator letter, certificate) | NOT IMPLEMENTED | No column on `licences` (`migrations/0002`), no table |
| Licence review date / renewal due date | NOT IMPLEMENTED | No column, no reader |
| `issued_at` / `expires_at` set via the application | NOT IMPLEMENTED (DB-FIELD-ONLY) | `CreateLicence` inserts neither (`registry_admin.go:345-350`). Every non-NULL value in the codebase comes from raw SQL in tests (`operatingmarket/resolve_integration_test.go:530`) |
| Licence status transitions (suspend / reinstate / expire / renew) | NOT IMPLEMENTED (DB-FIELD-ONLY) | `registry_admin.go:300-310` says so explicitly; registry `MKT-LICSTATUS-1` OPEN; grep for `UPDATE licences` in non-test Go: zero hits |
| Licence history (before/after) | PARTIALLY IMPLEMENTED | Creation is audited (`registry_admin.go:361-366`). There is no update path, so nothing else gets audited. There is no history table, and `licences` has no `updated_at`. |
| What happens at expiry | **Only an implicit per-request read** | Nothing changes `licences.status`. No scheduler, no alert kind (grep of `internal/alerting`, `internal/reconciliation`, `cmd` for licence: only `cmd/seed-market`). Brand and tenant stay `active`. The market gate starts refusing new player operations from the expiry date because the predicate reads `expires_at` against `AsOf=now`. Withdrawals are not market-gated, so players can still withdraw. **Nobody is notified.** The only trace is the market gate's per-refusal audit rows. |
| Who is notified | NOT IMPLEMENTED | No recipient, no alert kind; ALERT-DELIVERY-1 OPEN |
| Licence-assignment history visible to the affected tenant | NOT IMPLEMENTED | The audit row is platform-scoped (`tenant_licence_admin.go:164`); `MKT-AUDIT-1` OPEN |

## 4. Controlled lifecycle actions: matrix

Legend: Y = present, - = absent, n/a = not meaningful. "Four-eyes" means a second distinct human enforced by software.

| Action | DB | Service | API | OpenAPI | Back Office | Audit | Four-eyes | Step-up | Tests | Label |
|---|---|---|---|---|---|---|---|---|---|---|
| Create licence | Y (0002, RLS 0077) | Y `CreateLicence` `registry_admin.go:311` | Y `POST /v1/admin/licences` (`jurisdiction_admin_routes.go:41`), `PermJurisdictionRegistryManage` (platform_admin only, `permission.go:817`) | Y | **-** (read-only picker inside the launch request dialog) | Y platform-scope | **-** single actor | critical | `jurisdiction_admin_integration_test.go:29,75` | API-ONLY, single-actor |
| List licences | Y | Y | Y `GET` | Y | picker only | n/a | n/a | none | `:75` | API-ONLY |
| Update licence (number, products, dates) | DB-FIELD-ONLY | - | - | - | - | - | - | - | - | NOT IMPLEMENTED |
| Suspend / expire / reinstate / renew licence | DB-FIELD-ONLY | - | - | - | - | - | - | - | RLS denial only: `registry_rls_integration_test.go:335` | NOT IMPLEMENTED |
| Set issue / expiry / review dates | DB-FIELD-ONLY (no review column) | - | - | - | - | - | - | - | - | NOT IMPLEMENTED |
| Attach licence to tenant | Y (0007 FK, 0077 RLS + exclusivity) | Y `AssignTenantLicence` `tenant_licence_admin.go:79` (refuses a non-active licence `:114`, does not check dates) | Y `PUT /v1/admin/tenants/{tenantID}/licence`, `PermTenantLicenceAssign` (platform_admin only) | Y | **-** | Y platform-scope only | **-** single actor | critical | `httpserver/tenant_licence_admin_integration_test.go:94-436`; `jurisdiction/tenant_licence_admin_integration_test.go:148-767` | API-ONLY, single-actor |
| Detach (unassign) licence | same | same (`LicenceID=nil`) | same | Y | - | Y | - | critical | `:138`, `:411` | API-ONLY (fail-closed direction) |
| Approve / revoke licence (as a governed decision) | - | - | - | - | - | - | - | - | - | NOT IMPLEMENTED (no licence approval concept; licences are created `active`) |
| Create jurisdiction | Y | Y `CreateJurisdiction` `registry_admin.go:124` | Y `POST /v1/admin/jurisdictions` | Y | **-** | Y | **-** | critical | `jurisdiction_admin_integration_test.go:29,75` | API-ONLY, single-actor |
| Edit jurisdiction (`country_code`) | Y | Y `SetJurisdictionCountryCode` `:172` | **-** | - | - | Y | - | n/a | `country_code_integration_test.go` | IMPLEMENTED-UNTESTED at HTTP (service only, no route) |
| Edit jurisdiction name / regulator / notes; retire jurisdiction | Y (UPDATE policy) | - | - | - | - | - | - | - | - | NOT IMPLEMENTED |
| Configure `tenant_jurisdiction_configs` (ruleset ids, currencies, payment methods, geo block) | Y, **but** RLS `FOR ALL` on `tenant_id` only (`migrations/0005`), no audit trigger, no append-only guard | **-** (no Go writer anywhere: grep) | - | - | - | - | - | - | runtime role can write it from tenant scope (`internal/db/runtime_role_separation_test.go:187`) | DB-FIELD-ONLY, ungoverned |
| Licence country ceiling (enable / disable / withdraw) | Y (0076, append-only, `authorization_reference` required to enable) | Y `CreateLicenceCountryCeilingVersion` `operatingmarket/ceiling_admin.go:56` | **-** | - | - | Y platform-scope | **-** (`MKT-DUAL-1` OPEN; "two humans by process" in `docs/runbooks/market-blocking-configuration.md:44`, DOC-ONLY) | n/a | `operatingmarket/*_integration_test.go` | service + CLI `cmd/seed-market` (refuses production, `cmd/seed-market/main.go:15-19`); no production path |
| Operating country policy (tenant / brand / operation rungs) | Y | Y `CreateOperatingCountryPolicyVersion` `policy_admin.go:103` | **-** | - | - | Y | **-** | n/a | same | same as above |
| Jurisdiction evidence collection toggle / resolution-active toggle | Y | Y | Y (`jurisdiction_admin_routes.go:46-65`, tenant-scoped, compliance) | Y | **-** | Y | - | critical | `jurisdiction_evidence_collection_integration_test.go`, `jurisdiction_admin_integration_test.go:127,163` | API-ONLY |
| Create tenant (fixes `licensing_model`) | Y | Y | Y `POST /v1/admin/tenants` | Y | Y `CreateTenantForm.tsx` | Y | - | critical | `TenantForms.test.tsx` | IMPLEMENTED-VERIFIED (single actor; creates `pending_launch`) |
| Change `licensing_model` | DB allows from platform scope | - | - | - | - | - | - | - | - | NOT IMPLEMENTED |
| Brand / tenant activate, suspend, reactivate, close, ratify | Y | Y | Y | Y | Y | Y | Y (except suspend, by design) | approve: critical; create: by action class | see §2 | IMPLEMENTED-VERIFIED |

## 5. Negative tests: proven or absent

| Claim | Proven by | Status |
|---|---|---|
| A tenant role cannot approve or reject a launch | `launch_authorisation_http_integration_test.go:299` (`TestLaunchHTTP_AuthzMatrix`: tenant_admin, compliance, support, finance, player get 403; anonymous gets 401); service + DB: `rules_integration_test.go:20` (tenant principal gets `ErrForbidden`); DB `LA012`/`LA001` `launch_governance_integration_test.go:729` | PROVEN (read) |
| A platform-scope role without the permission (compliance/finance/support/empty) cannot approve | `rules_integration_test.go:54-60` | PROVEN (read) |
| An acting session (`platform_acting`) cannot approve | `rules_integration_test.go:51-53`; acting session sees none of the 3 tables `launch_governance_integration_test.go:790-797` | PROVEN (read) |
| The requester cannot approve their own request (same staff id) | `rules_integration_test.go:29-31`; HTTP `launch_authorisation_http_integration_test.go:154-156` (`LAUNCH_SELF_APPROVAL`) | PROVEN (read) |
| A second staff account of the same Person cannot approve | `rules_integration_test.go:33-45` | PROVEN (read) |
| The same Person cannot approve twice to satisfy a 2-approval rule | DB `launch_governance_integration_test.go:802-806` (`LA012`); unit `approvability_test.go:83,92` | PROVEN (read) |
| One approval of a 2-approval request does not execute | `launch_authorisation_http_integration_test.go:158-166` | PROVEN (read) |
| An approver with no `person_id` is refused | `launch_governance_integration_test.go:1198` | PROVEN (read) |
| Two final approvals racing execute once | `launch_governance_integration_test.go:927` | PROVEN (read) |
| GUC impersonation without a signed proof is refused | `proof_integration_test.go:117` (`AP001`) | PROVEN (read) |
| No proof issuer gives 500 and nothing recorded; service not wired gives 503 | `launch_authorisation_http_integration_test.go:530,559` | PROVEN (read) |
| A raw SQL status change from any session is refused | `launch_governance_integration_test.go:304` | PROVEN (read) |
| A tenant admin cannot suspend or activate the tenant itself, nor request on a tenant subject | `launch_authorisation_http_integration_test.go:385-397`; `rules_integration_test.go:86-89` | PROVEN (read) |
| Cross-tenant ids give 404 | `rules_integration_test.go:62-84`; `launch_authorisation_http_integration_test.go:416` | PROVEN (read) |
| Platform admin vs tenant admin on licence writes: tenant admin cannot create licences or jurisdictions, or assign a licence | `jurisdiction_admin_integration_test.go:29`; `tenant_licence_admin_integration_test.go:206,222,248` | PROVEN (read) |
| A tenant-scoped DB session cannot write `tenants.licence_id`, licences or jurisdictions | `registry_rls_integration_test.go:35,63,126,335,367,477,564` | PROVEN (read) |
| **One person cannot create a licence and bind it** | - | **ABSENT, and true by design today:** both actions are single-actor platform_admin. There is no rule to test (G-2). |
| **One person cannot enable a market** | - | **ABSENT:** single actor at the library level; the runbook asks for "two humans by process" (DOC-ONLY) (G-1) |
| **An expired or suspended licence cannot let a regulated operation through** | Validity predicate unit-level: `licence_validity_integration_test.go:29` | **PARTIAL:** no resolver or gate test for expired, suspended or unbound (G-6) |
| **An expired licence cannot be activated without being noticed** | Unacknowledged-check refusal generally: `rules_integration_test.go:108`; mapping unit `launch_authorisation_routes_test.go:44` | **ABSENT for licences specifically:** no test files an activation with an expired or suspended licence and asserts R-LIC-VALID `fail`. Only the pass case is tested (`rules_integration_test.go:377`) (G-8). |
| A failed or ambiguous market check refuses | `internal/marketgate/hardening_test.go`, `marketgate_integration_test.go`, `market_gate_http_*_integration_test.go` (unknown evidence refuses) | PROVEN (read; not run) |

## 6. Gap list (ranked)

Severity is the architectural and operational risk to a regulated launch, not effort. "HUMAN" marks gaps that need a
legal/compliance decision before the fix can be shaped. Every proposed fix is provider-independent.

| # | Sev | Gap | Evidence | Minimal fix | HUMAN? |
|---|---|---|---|---|---|
| G-1 | **High** | **Market enablement has no software four-eyes, and MKT-DUAL-1's own deadline has passed.** The registry says MKT-DUAL-1 "MUST be resolved before ... (b) any wiring of `ResolveOperatingCountryPolicy` into any consuming domain". The A8 market gate now wires it into registration, deposit, casino launch and sportsbook placement. Ceiling and policy writes are single-actor library calls. The only production-safe write path is none at all: `cmd/seed-market` refuses production. | `task-registry.md:1287-1310`; `marketgate.go` header; `ceiling_admin.go:56`; `policy_admin.go:103`; runbook `:44` | ADR + one generic `change_requests`/`change_approvals` pair (architect ruling MKT-DUAL-1). Signed actor proof per ADR 0110, covering every widening-capable write (`widening_capable` audit field). Then add the platform routes and a Back Office page. Until then, record that MKT-DUAL-1 trigger (b) has been crossed. | Partly: who may approve market opening (compliance vs platform admin) is HDR-M-1 |
| G-2 | **High** | **Licence creation and licence-to-tenant binding are single-actor.** One platform admin with step-up can create a licence record and bind it. Together with G-1 that one person controls the root of every market ceiling. | `jurisdiction_admin_routes.go:41,76`; `registry_admin.go:311`; `tenant_licence_admin.go:79` | Put licence create, bind, rebind and status change behind the same approval primitive as G-1. Unbind and suspend stay single-actor (fail-closed direction, same asymmetry as launch suspend). | Yes: which licence facts need a second reviewer, and of what role |
| G-3 | **High** | **One licence per tenant; no per-brand licence.** A B2B operator with licences in two jurisdictions, or a tenant with one brand under our licence and one under its own, cannot be modelled. H-2 forces `request.licence_id = tenants.licence_id`. The `tenant_licences` table named in the brief does not exist. | `migrations/0002:29`; `hard.go:180-212`; `migrations/0008` | Architect ADR before any second-licence tenant: a `tenant_licences` (or `brand_licences`) association with effective dating. Move the ceiling anchor from `tenants.licence_id` to that association. Update H-2 and the resolver's step 1. This is a decision, not a quick patch. | Yes: whether a tenant/brand may operate under several licences, and whether one brand may mix bases |
| G-4 | **High** | **No licence lifecycle: status, dates and renewal can't be written by the application, and there is no evidence or review date.** A regulator suspension can only be recorded by raw SQL. `issued_at`/`expires_at` are never written by app code. | `registry_admin.go:300-310,345-350`; `MKT-LICSTATUS-1`; `MKT-EXPIRY-1` | Add an audited, platform-only `licences` lifecycle: `SetLicenceStatus` and `SetLicenceValidityDates` with reason code and `evidence_reference` (text, like `determination_reference`). Add `review_due_at` and `updated_at` columns and a `licence_versions` append-only history (or audit before/after as the history). Four-eyes for widening changes (reinstate, extend expiry), single actor for narrowing (suspend). Add the API, OpenAPI and a Back Office "Licences" page. | Yes: the expiry boundary (MKT-EXPIRY-1), whether `issued_at` is mandatory, and what evidence is required |
| G-5 | **High** | **At expiry nothing happens except an implicit per-request refusal; nobody is notified.** Brand and tenant stay `active`. There is no alert, no scheduled check, and no pre-expiry warning. | §3 above; `alerts.go:12-20`; ALERT-DELIVERY-1 OPEN | A scheduled read-only sweep (the reconciliation scheduler pattern) that raises a P1 `licence.expired` alert and a P2 `licence.expiring` alert N days before `expires_at` or `review_due_at`. Do **not** auto-suspend brands without a human decision. If one is approved, use the existing single-actor suspend path with a system actor. | Yes: auto-suspend vs alert-only, the warning window, and recipients |
| G-6 | **Medium** | **The resolver and market gate are not tested for an expired, suspended or unbound licence.** The code path exists (`resolve.go:247-285`), but only the not-yet-issued arm is tested end to end. | grep: zero test hits for `CeilingReasonLicenceExpired`, `CeilingReasonLicenceSuspended`, `OutcomeLicensingUnknown` | Add integration tests: `status='suspended'`, `status='expired'`, `expires_at=today` (boundary) and `licence_id=NULL` each give `not_permitted_by_licence` / `licensing_unknown`. Also one HTTP market-gate test per arm (registration refused). | No |
| G-7 | **Medium** | **A licence-less launch basis can be approved but can never serve players.** `other_manually_approved` / `not_applicable_recorded_determination` with no bound licence passes H-2. The market resolver then returns `licensing_unknown` for every request. ADR 0112 §2.7 promises "every model is always available"; the runtime contradicts it. | `hard.go:170-178`; `resolve.go:247-252` | Either (a) make R-MARKET-CONFIG a hard precondition for `activate`/`reactivate` (smallest change, stays fail-closed), or (b) decide in an ADR how a licence-less determination anchors a market ceiling. | Yes for (b): the meaning of "not applicable" for market permission |
| G-8 | **Medium** | **Activation under an expired or suspended licence is allowed after acknowledgement, and the recorded `licensing_status` is not cross-checked against `licences.status`.** This is by design (ADR 0112 §5.2), but no test shows it. A recorded `in_force` next to an expired registry row is an untrue record. | `readiness.go:399-411`; `hard.go:160-168` | Minimal: add an H-2 consistency rule. When `licence_id` is set, the recorded `licensing_status` must not be `in_force` while `EvaluateLicenceValidity != valid`. Add tests for the expired and suspended acknowledgement paths. Whether an invalid licence should hard-block activation is a human call. | Yes: block vs acknowledge |
| G-9 | **Medium** | **`tenant_jurisdiction_configs` has no governed write path and is writable by any tenant-scoped runtime session.** RLS is `FOR ALL` on `tenant_id` only, with no audit and no append-only guard. It feeds H-3 (a hard precondition), R-RULESETS, R-CURRENCIES, R-PAY-METHODS, withdrawal policy and the AML policy jurisdiction check. No Go code writes it, so every real launch's H-3 needs a raw SQL INSERT. | `migrations/0005`; `runtime_role_separation_test.go:187`; grep: no Go writer | Narrow the policy: tenant SELECT, platform-admin INSERT/UPDATE, append-only with `effective_to` close (the 0076 pattern), and audit in the same transaction. Add `CreateTenantJurisdictionConfigVersion` + route + Back Office. Four-eyes if it widens currencies or payment methods. | No (engineering), but ruleset content is HDR-J |
| G-10 | **Medium** | **No Back Office for licences, jurisdictions, licence binding, operating markets or jurisdiction configs.** Every one of these is API-ONLY or service-only. Operators must call the API by hand for every licensing step. | `backoffice/src` grep: only `launch/*`, `tenants/*` show `licensing_model` | Platform "Licensing" section: jurisdictions list/create, licences list/create/lifecycle (after G-4), tenant licence binding on tenant detail, read-only operating-market view. Build after G-1/G-2 so the UI is born with maker-checker. | No |
| G-11 | **Low** | **The tenant cannot see its own licence-assignment history** (audit row is platform-scope). | `tenant_licence_admin.go:164`; `MKT-AUDIT-1` | Set `subject_tenant_id` on the platform-scope row (ADR 0104 projection, already used by launch audit). | Security disclosure call (owner: security) |
| G-12 | **Low** | **The code differs from ADR 0112 §8.5 on licence read errors.** R-LIC-VALID `read_error` is `not_evaluable` (acknowledgeable), but the ADR says unreadable licence refuses. Masked by H-2's earlier read. | `readiness.go:404-406`; ADR l.398 | Make a licence read error refuse in `licenceCheck` (return an error), or amend the ADR text. | No |
| G-13 | **Low** | **`licensing_model` cannot be changed** (for example a tenant moving from our licence to BYOL), and there is no governed path for it. | grep: no writer | Defer. Record it as a future decision; a migration path must also move credentials ownership (ADR 0006 consequence). | Yes (commercial/licensing) |
| G-14 | **Low** | **The Back Office preselects "Under platform licence"** on tenant creation. | `CreateTenantForm.tsx:23` | No preselection: a required, empty select. | No |
| G-15 | **Low** | **Jurisdiction edit and retire are missing** (name, regulator, notes; `SetJurisdictionCountryCode` has no route). | `registry_admin.go:172`; routes | Add when a consumer needs it, audited and platform-only. | No |
| G-16 | **Info** | **`licences.permitted_markets` / `permitted_products` are stored but unenforced** (MKT-PM-1). | `registry_admin.go:273-295` | Drop `permitted_markets` with the first real ceiling content, as MKT-PM-1 says. | Content: HDR-J-6 |

### 6.1 Gaps that need a HUMAN legal/compliance decision (for the orchestrator to route)

1. G-3: whether a tenant or brand may operate under more than one licence, and whether bases can mix within a tenant.
2. G-4 / MKT-EXPIRY-1: the expiry and issue date boundaries, whether `issued_at` is mandatory, and what evidence a
   licence record needs.
3. G-5: whether licence expiry auto-suspends brands or only alerts, the warning window, and recipients.
4. G-7(b): what "not applicable" means for market permission.
5. G-8: whether an invalid licence hard-blocks activation or remains an acknowledged input.
6. G-1 / G-2 (HDR-M-1): which roles may approve market opening and licence binding.
7. G-13: tenant licensing-model migration (commercial/licensing).
8. Existing and still OPEN: "Licence / jurisdictions beyond Anjouan" (`human-decision-register.md:94`);
   HDR-J-6/7/8/9, HDR-M-1/2 (`:89`); HD-CTF-9 (reopening a closed subject).

### 6.2 What is solid (no action needed)

- Hybrid licensing per tenant, enforced by the database (composite FK, BYOL exclusivity, RLS with no tenant-scoped
  write on `tenants`/`licences`/`jurisdictions`, deny-TRUNCATE).
- No universal licence flag and no hard-coded Anjouan path.
- ADR 0112 launch governance is complete for its own scope: per-brand and per-tenant subjects, a database-enforced
  status guard, four-eyes with distinct staff and distinct Person, signed proofs, reason codes, append-only history,
  audit, a fail-closed brand gameplay gate and registration gate, and an API, OpenAPI and Back Office with tests.
- At runtime the operating-market resolver fails closed on a missing or invalid licence (code read). G-6 is only
  about missing tests.

## 7. Things I could not verify

- Integration and Back Office test results: read, not run (see §0).
- Whether the deployed `igaming_runtime` role really holds INSERT/UPDATE/DELETE on `tenant_jurisdiction_configs`
  outside the test harness. The test harness grants it through default privileges, and
  `runtime_role_separation_test.go:187` relies on it. I did not read `deploy/init-app-role.sql`.
- Whether casino in-session bets (after launch) re-check the licence through `marketgate.BrandOpen`
  (`internal/casino/market_exposure.go:83`). I did not trace that path to the end.

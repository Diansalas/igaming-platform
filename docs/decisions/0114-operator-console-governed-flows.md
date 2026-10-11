# ADR 0114 - Operator console for the governed financial/operational flows

- **Status:** ACCEPTED - PENDING IMPLEMENTATION (2026-10-11; drafted by `architect` at `6bda8ac`; `security` design
  review APPROVE WITH CONDITIONS, conditions written into this text, section 9). Nothing in this ADR is implemented.
  Every UI and route below is `NOT IMPLEMENTED` until its slice lands; every governed flow it fronts is `MOCK` (no real
  provider). D-OPS-1 and D-OPS-2 are recorded as **orchestrator defaults, reversible by the owner** (section 5).
- **Trigger:** owner directive 2026-10-11: important governed financial/operational functionality must not stay
  API-only where a production operator would reasonably need an interface; UI is provider-independent and
  architecturally defined; **no workflow that has not been approved may be invented**.
- **Supersedes:** the "defer" recommendation of `docs/governance/operator-console-governed-flows-brief.md` (D-OPS-1).
- **Inputs:** that brief; `docs/governance/platform-completion-gap-audit-2026-10-10.md` sections 2 and 12;
  ADR 0095, 0099 (K1), 0100 (K2), 0101 (K3 M1/M2), 0102 (alerts), 0104 (tenant-visible audit), 0107, 0110 (signed
  actor proof), 0111 (B13 binding, M4, HSEC), 0112 (launch UI precedent), 0113; `internal/auth/permission.go`;
  `internal/httpserver/*_routes.go`; `docs/api/openapi/platform-api.yaml`; `backoffice/src/features/*`;
  `docs/governance/open-owner-questions-2026-10-09.md`.
- **Decision type:** cross-domain read-surface and console architecture. No change to any four-eyes rule, ledger
  account, state machine, RLS policy or approval threshold. One new permission is proposed (section 5, D-OPS-2).

## 1. Principles (binding on every slice)

1. **Server authoritative.** The console only fronts existing routes. `backoffice/src/auth/permissions.ts` stays a
   static hint (it already mirrors `manualAdjustments`, `paymentForceResolutions`, `withdrawalHoldResolutions`,
   `payoutInstruments`, `alertRouting`); every server refusal is rendered through a code-to-message map that never
   prints the server message (ADR 0112 slice 5 precedent).
2. **No invented workflow.** A screen exists only for a transition a route already performs. Where no governed exit
   exists (GAP-AAM, risk REVIEW case handling, RG limits, AML cases, chargebacks) the console shows state read-only
   and says "no governed action exists" - it never offers a workaround button.
3. **Four-eyes stays in the database.** The UI disables "approve" on the caller's own request (staff id compare) as a
   hint only; distinct-Person and proof checks (ADR 0110: the proof is issued server-side, the browser never signs)
   remain the authority. Approve/reject dialogs freeze and resend the `payload_hash` shown when the dialog opened.
4. **Tenant from context.** Tenant-scoped staff never choose a tenant: the path `tenantID` is filled from the token.
   Only platform roles get a tenant picker (platform_acting reads/writes, `canActOnTenant`).
5. **Money display.** Amounts arrive as decimal strings of minor units plus the asset exponent from the Asset
   registry; the UI formats with integer/BigInt arithmetic, never floating point.
6. **MOCK labelling.** Every governed-money screen carries a fixed banner: "Sandbox/MOCK provider - software
   capability only, not a regulatory approval" until the flow's non-MOCK gate is cleared.
7. **No staff-id leakage across scopes.** A tenant-scoped caller never sees a platform staff id or name: scope only
   (ADR 0112 slice 6 review conditions; ADR 0104 section 5 applies inside a scope). Free-text fields (`note`,
   reasons) carry the ADR 0112 "visible to tenant and platform staff" PII warning.
8. **Pagination.** New list routes use `internal/httpserver/pagination.go` (`limit` 1..100 default 50, `offset`,
   `total`) via `newPagedResponse`; filters are closed vocabularies validated at the wire (400 on unknown values).

## 2. Inventory of governed flows

Legend - State: **UI-ready** (route exists, build UI now) / **needs-read-route** (a small read-only route from
section 4 is needed first) / **needs-design** (workflow not defined; no UI action) / **blocked** (provider or human).
Roles: PA platform_admin, TA tenant_admin, CO compliance, FI finance, RM risk_manager, SU support, PM
promotions_manager, BX bonus_operations. "FI*" = also needs an active K1 grant in-tx (ADR 0099 section 6).
OAS = documented in `docs/api/openapi/platform-api.yaml`. BO = Back Office screen exists.

### 2.1 Withdrawals and payouts

| Flow | Backend route (handler) | OAS | BO | Roles see / act | Four-eyes / proof | State |
|---|---|---|---|---|---|---|
| Pending-review queue | `GET /v1/admin/withdrawals` (`financial_routes.go:69`) | yes | yes `WithdrawalQueuePage.tsx` | FI | n/a | IMPLEMENTED |
| Detail, history | `GET /v1/admin/withdrawals/{id}`, `/history` (`financial_routes.go:101-104`) | yes | yes `WithdrawalDetailPage.tsx` | FI | n/a | IMPLEMENTED (no attempt state) |
| Approve / reject | `POST .../{id}/approve`, `/reject` (`financial_routes.go:71-74`) | yes | yes | FI act | withdrawal policy approvals, DB-enforced | IMPLEMENTED |
| Submit / submitted queue / resolve | `POST .../{id}/submit`, `GET .../submitted`, `POST .../{id}/resolve` (`financial_routes.go:75-86`) | yes | yes | FI act | bound instrument rail (ADR 0111 2.4) | IMPLEMENTED; `api/withdrawals.ts:105` still documents a `payment_method` body (stale, harmless) |
| Payout attempt state + park reason on a withdrawal (`disputed`; `destination_mismatch`, `destination_integrity_failure`, `provider_reference_conflict`, `invalid_provider_reference[:*]`, `amount_asset_mismatch`, unbound) | none: `staffWithdrawalDetailResponse` carries only `payout_instrument` (`withdrawal_handlers.go:543-549`) | no | no | FI, CO, PA read | read only | needs-read-route (R1b) |
| Parked/disputed attempts queue (payouts; deposits parked `reversal_tombstone_precedes_success` for M1) | none (no admin attempt or deposit read) | no | no | FI, CO, PA read | read only | needs-read-route (R1) |
| K3 M1/M2 force-resolution request, list, detail, approve, reject, cancel | `/v1/admin/tenants/{tenantID}/payment-force-resolutions*` (`payment_force_resolution_routes.go:53-64`) | yes | no | request/approve FI*, PA (platform_acting); read CO | distinct Person, DB-enforced; signed actor proof; `payload_hash` on decide | UI-ready; pickers need R1 (attempt id) and R2 (finding code) |
| K3 M4 evidence paid / not-paid (`m4_evidence_*`, `evidence_line_id`, `evidence_ref_hash`) | same routes (request fields `payment_force_resolution_routes.go:202-213`) | yes | no | as M2 | as M2 plus import seal (ADR 0111 4.3) | UI-ready for MOCK after R1/R2b; **blocked** for non-MOCK (D-7, T10, S-3, `evidence_ref_hash` binding) |
| `amount_asset_mismatch` park exit | none (409 at `withdrawal_handlers.go:1353`; not in M2/M4/HSEC) | n/a | no | read only | n/a | needs-design (GAP-AAM, owner + ledger-finance) |
| `destination_integrity_failure` resume (Q-R32-3) | none beyond M4 not-paid (migration 0127) | n/a | no | read only | n/a | needs-design (non-blocking per owner matrix) |
| HSEC approved-hold release (`release_hold_to_player`) | `/v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions*` (`withdrawal_hold_resolution_routes.go:50-61`) | yes | no | PA only (request + approve by distinct platform Persons, platform_acting) | four-eyes, proof (`AP004`), `evidence_ref_hash` required | needs-read-route (R3: PA cannot read `/v1/admin/withdrawals`, it is `RequireTenantScope` + `withdrawal:review`); production **blocked** by Q-HSEC-1/2/3 (no policy row) |
| Payout instruments view (state, verification source/expiry, mask) | `GET /v1/admin/players/{playerID}/payout-instruments` (`payout_instrument_routes.go:74`; `payoutinstrument.View`, `ops.go:252-264`) | yes | no | FI (masks), CO, PA read | n/a | UI-ready |
| Payout instrument suspend | `POST /v1/admin/payout-instruments/{id}/suspend` (`payout_instrument_routes.go:76`) | yes | no | CO act | single actor + reason | UI-ready |
| Payout instrument verify / reinstate | none | n/a | no | - | - | needs-design (not approved; do not build) |
| Payment kill switch engage, list, detail, release request/approve/cancel | `/v1/admin/tenants/{tenantID}/payments/kill-switch*` (`payments_kill_switch_handlers.go:65-78`) | yes | no | TA, PA act | release four-eyes + platform lock (migration 0105) | UI-ready (pending release found via `release_request_id` on the switch) |

### 2.2 Ledger governance

| Flow | Backend route | OAS | BO | Roles | Four-eyes / proof | State |
|---|---|---|---|---|---|---|
| K2 manual adjustment submit, list, detail, approve, reject, cancel | `/v1/admin/tenants/{tenantID}/manual-adjustments*` (`manual_adjustment_routes.go:46-57`) | **no** | no | initiate/approve FI*, PA; read CO | reason code, threshold policy, distinct Person, proof | needs-read-route (R5: no staff wallet lookup; FI lacks `player:read`) |
| Financial policy (K2 thresholds) read; change propose/approve/reject/cancel | `GET /v1/admin/financial-policies`, `POST .../financial-policy-changes*` (`manual_adjustment_routes.go:63-74`) | **no** | no | PA author; TA tighten; CO, FI read | four-eyes on change | needs-read-route (R6: no list of pending changes to approve) |
| K1 capability grant request, list, approve, reject, cancel, revoke | `/v1/admin/tenants/{tenantID}/capability-grants*` (`capability_routes.go:42-55`) | **no** | no | PA all (sole approver, R-7); TA request/revoke/read; CO read; FI is grantee only | DB-enforced R-7, proof | needs-read-route (R4: grantee picker needs a staff directory) |

### 2.3 Reconciliation and alerts

| Flow | Backend route | OAS | BO | Roles | State |
|---|---|---|---|---|---|
| Casino runs / mismatches / callback rejections | `GET /v1/admin/casino/reconciliation/{runs,mismatches}`, `/casino/callback-rejections` (`casino_routes.go:56-61`) | yes | no (only rounds page) | TA, CO, FI read | UI-ready |
| `ledger_vs_projection` runs/mismatches (`missing_projection`, `balance_mismatch`, `ledger_unlinked_manual_adjustment`) | none (`reconciliation/read.go` admits casino streams only) | no | no | FI, CO, PA read | needs-read-route (R2) |
| `payment_statement` runs/findings (`pay_*` incl. `pay_captured_unposted`, `pay_declared_*`) | none | no | no | FI, CO, PA read | needs-read-route (R2) |
| `sportsbook_settlement` runs/mismatches (`sb_*`) | none | no | no | TA, CO, FI read | needs-read-route (R2) |
| Mismatch resolve/compensate | none (by design: corrections go through K2/K3) | n/a | no | - | not a flow; link to K2/K3 |
| Operational alerts list / ack / resolve | `GET /v1/admin/alerts`, `POST .../{alertID}/ack|resolve` (`alert_routing_handlers.go:57`, `alert_admin_handlers.go:53-54`) | yes | no | PA only (`alert:manage`) | UI-ready |
| Alert routing status / routes | `GET /v1/admin/alerting/status`, `GET|POST /v1/admin/alerting/routes` (`alert_routing_handlers.go:52-56`) | yes | no | PA (`alert:manage` / `alert:route_manage`) | UI-ready (status read now; route authoring waits on ALERT-DELIVERY-1 channel decision) |
| Tenant-visible alert view | none (alerts are platform-owned, ADR 0102) | n/a | no | - | needs-design (deferred) |

### 2.4 Other operator areas

| Flow | Backend route | OAS | BO | Roles | State |
|---|---|---|---|---|---|
| Tenant audit log; platform audit log | `GET /v1/admin/audit-log`, `/platform/audit-log` (`routes.go:140,151`) | yes | yes `features/audit/*` | TA, CO (tenant); PA (platform) | IMPLEMENTED |
| Tenant view of platform actions (ADR 0104) | `GET /v1/admin/audit-log/platform-actions` (`routes.go:161`) | **no** | no | TA, CO | UI-ready |
| KYC case queue, verification/document review | `/v1/admin/kyc/*` | yes | yes `features/kyc/*` | CO act; TA read | IMPLEMENTED |
| KYC enforcement decisions | `GET /v1/admin/kyc/enforcement-decisions` | **no** | no | CO, PA | UI-ready |
| AML monitoring / sanctions / SAR cases | none | n/a | no | - | needs-design (not built; gap audit section 6) |
| RG self-exclusion restrictions | `GET|POST /v1/admin/rg/restrictions` | yes | yes `features/rg/*` | CO act; TA read | IMPLEMENTED |
| RG limits (deposit/loss/session, cooling-off) | none (`rg.go:38-41` single type) | n/a | no | - | needs-design (backend NOT IMPLEMENTED; parameters LEGAL) |
| Risk rules list/create/disable | `/v1/admin/risk/rules*` | yes | no | RM act; TA read | UI-ready |
| Risk REVIEW outcome queue | none | n/a | no | - | needs-design |
| Bonus campaigns, change-requests, grants | `/v1/admin/bonus/*` | yes (suggestions, held-dispositions: **no**) | partial (campaigns, change-requests) | per security-architecture section B1.1 | UI-ready for existing GETs; offers/suggestions/bulk-jobs/held-dispositions lack list routes (needs-read-route, later) |
| Asset registry + change requests | `/v1/admin/assets*` | yes | no | PA | UI-ready; change-request list GET missing (needs-read-route, later) |
| Jurisdictions, licences, tenant licence | `/v1/admin/jurisdictions`, `/licences`, `PUT .../tenants/{id}/licence` | yes | no (consumed by launch panel) | PA | UI-ready |
| Tenant/brand provisioning, suspend, launch | `/v1/admin/tenants*`, launch routes (ADR 0112) | yes | yes (create/read; launch slice 5) | PA; TA brands/launch request | IMPLEMENTED |
| Provider credential requests / handles | `/v1/admin/tenants/{tenantID}/provider-credential*` | yes | no | PA act; TA read/revoke | UI-ready |
| Staff create, person-link, display name | `POST .../tenants/{tenantID}/staff`, `person-link`, `display-name` | partial (display-name, person-link: **no**) | create only | PA, TA (finance creation PA only, `admin_routes.go`) | needs-read-route (R4: no staff list) |

## 3. Role x screen matrix (server permissions only)

A = act, R = read, - = hidden (the server refuses anyway). Derived from `rolePermissions`
(`internal/auth/permission.go:730-1093`); the console never widens it.

| Screen | PA | TA | CO | FI | RM | SU | PM / BX |
|---|---|---|---|---|---|---|---|
| Withdrawal queue / detail / approve / reject / submit / resolve | - | - | - | A | - | - | - |
| Withdrawal detail: payout attempt panel (R1b) | - | - | - | R | - | - | - |
| Parked payments queue (R1) | R (tenant picker) | - | R | R | - | - | - |
| Force resolution M1/M2 (M4 MOCK) | A (platform_acting) | - | R | A (with K1 grant) | - | - | - |
| HSEC hold release + candidates (R3) | A | - | - | - | - | - | - |
| Manual adjustments K2 + wallet lookup (R5) | A | - | R | A (with K1 grant) | - | - | - |
| Financial policies (R6) | A (author) | A (tighten) | R | R | - | - | - |
| Capability grants K1 + staff picker (R4) | A (request/approve/revoke) | A (request/revoke) | R | - | - | - | - |
| Payment kill switches | A | A (own tenant) | - | - | - | - | - |
| Payout instruments | R | - | A (suspend) | R (masks) | - | - | - |
| Reconciliation: casino | - | R | R | R | - | - | - |
| Reconciliation: payment_statement (R2) | R | - | R | R | - | - | - |
| Reconciliation: ledger_vs_projection (R2) | R | - | R | R | - | - | - |
| Reconciliation: sportsbook_settlement (R2, new permission) | - | R | R | R | - | - | - |
| Alerts list / ack / resolve; routing status | A | - | - | - | - | - | - |
| Audit: tenant / platform-actions; platform | R (platform log) | R | R | - | - | - | - |
| KYC queue / enforcement decisions | R (enforcement) | R (verification) | A | - | - | - | - |
| RG restrictions | - | R | A | - | - | - | - |
| Risk rules | - | R | - | - | A | - | - |
| Bonus | - | R | R + suspend | - | suspend | R | per role |
| Registries (assets, jurisdictions, licences) | A | - | - | - | - | - | - |
| Tenants / brands / launch / staff | A | A (brands, staff, launch request) | R + launch request | - | - | - | - |

Notes: (a) FI has no `audit:read` and no `player:read`; this ADR does not add them. (b) CO and PA cannot open the
withdrawal detail (no `withdrawal:review`); they reach attempts through R1, which is the intended separation (Stage
3D: withdrawal governance stays with FI). (c) PA holds no `casino_reconciliation:read` and gets none here.

## 4. Minimal new read-only routes

All are GET, tenant-scoped through RLS (`db.WithTenant` for tenant callers; platform_acting with `canActOnTenant`
for PA on the `/tenants/{tenantID}/` family), return named envelope types covered by the OpenAPI drift test, never
return raw provider payloads, destination detail/ciphertext/fingerprints/seals, emails, or a platform staff id to a
tenant caller. Unknown enum values are mapped to a closed `other` bucket rather than passed through.

Unknown filter input is refused with 400; only the output side uses the `other` bucket.

| # | Route | Permission (existing unless noted) | Shape (fields) | Reviews |
|---|---|---|---|---|
| R1 | `GET /v1/admin/tenants/{tenantID}/payment-attempts?operation=payout|deposit&state=&terminal_reason=&withdrawal_request_id=&limit&offset` and `GET .../payment-attempts/{attemptID}` | `payment_force_resolve:read` **alone** (FI, CO, PA; `withdrawal:review` is not accepted) + `canActOnTenant` | `id, operation, state, terminal_reason, subject_ref {withdrawal_request_id or deposit_id}, amount_minor_units (string), asset_code, exponent, provider_id, has_provider_reference (bool), created_at, state_changed_at, open_resolution_id, admissible_kinds[]`. Never `provider_reference` text, player id or destination. A platform_acting read writes a tenant-visible platform-action audit row (ADR 0104) | security (exposure), ledger-finance (`admissible_kinds`), payments |
| R1b | add `payout_attempt` (latest attempt summary) to `GET /v1/admin/withdrawals/{id}` | unchanged (`withdrawal:review`, FI only) | `attempt_id, state, terminal_reason, open_resolution_id` or `null`; same exclusions as R1 | security, code-reviewer |
| R2 | `GET /v1/admin/tenants/{tenantID}/reconciliation/runs?stream=` and `.../reconciliation/mismatches?stream=&status=&kind=&subject_id=` | per stream, **checked before any query**: `payment_statement` -> `payment_force_resolve:read`; `ledger_vs_projection` -> `ledger_adjustment:read`; `sportsbook_settlement` -> **new** `sportsbook_reconciliation:read`; casino streams stay on the existing casino route. `stream` is required | run: `id, stream, period_start, period_end, run_at, status`; mismatch: `id, run_id, stream, kind, status, subject_ref, expected/actual minor units (string), asset_code, detected_at`. Unknown `stream`/`status`/`kind` input -> 400; `subject_ref` names an attempt, withdrawal, wallet or bet id, never a player identity | security, ledger-finance |
| R2b | `GET .../payment-attempts/{attemptID}/statement-evidence` (M4 line picker; MOCK only) | `payment_force_resolve:request` | `evidence_line_id, import_id, seal_verified (bool), verdict, line_date`; never the reference text while `evidence_ref_hash` is OPEN | security (T10), ledger-finance |
| R3 | `GET /v1/admin/tenants/{tenantID}/withdrawal-hold-candidates?limit&offset` | `withdrawal_hold_resolution:read` (PA only) | read through ONE narrow `SECURITY DEFINER` function that returns only `approved` withdrawals of the path tenant whose tenant or brand is non-active; **no platform-wide RLS policy** on `withdrawal_requests`. Fields `withdrawal_request_id, brand_id, amount_minor_units, asset_code, approved_at, tenant_status, brand_status, open_resolution_id`; no player id | security, ledger-finance |
| R4 | `GET /v1/admin/tenants/{tenantID}/staff?role=&limit&offset` | `RequireAnyPermission(staff:manage, capability_grant:request)` + `canActOnTenant` | tenant staff of the path tenant only; platform staff excluded **in SQL** (not by a Go filter); paginated; `role` filter is a closed vocabulary (400 otherwise). Fields `id, role, display_name, person_linked (bool), status, created_at`; no email, no person_id | security |
| R5 | `GET /v1/admin/players/{playerID}/wallets`; PA form `GET /v1/admin/tenants/{tenantID}/players/{playerID}/wallets` | `ledger_adjustment:read` + `RequireTenantScope` (tenant form). PA form additionally requires an **active K2 (ledger_adjustment) grant on that tenant** (ADR 0099), checked in-tx, and every PA read is recorded as a tenant-visible platform action (ADR 0104) | wallet fields only: `wallet_id, asset_code, exponent, projected_balance_minor_units (string), projection_as_of`, labelled "projection, not authoritative"; no player identity; foreign or unknown player -> 404; every read writes a read audit row. **No broad platform SELECT policy** on wallets or balance projections is added: the PA form runs platform_acting inside the tenant's RLS | ledger-finance, security |
| R6 | `GET /v1/admin/financial-policy-changes?status=&limit&offset` | `financial_policy:read` | cross-tenant rows only on an explicit platform-scope branch (`WithPlatformAdmin`); tenant callers via `db.WithTenant`, own tenant (+ platform baseline) only. Fields `id, scope, tenant_id, profile_code, proposed values, status, proposer_scope, proposer_id, created_at`; `proposer_id` blanked when proposer scope differs from caller scope | security, ledger-finance |
| H1 | **BLOCKER hardening (security live-confirmed):** platform actor ids are returned to tenant callers today | n/a | see section 4.1 | security |
| H2 | **BLOCKER hardening:** the M4 reference text leaks | n/a | see section 4.2 | security, ledger-finance |

`admissible_kinds` (R1) must be computed by calling the same database predicates the request path uses
(`payment_m2_admits`, `payment_m4_in_scope`, migrations 0115/0125/0127), not a Go re-implementation; it is a hint and
the request path re-checks.

### 4.1 H1 - actor-id redaction on every tenant-readable DTO (confirmed live by `security`)

Every `*_by` and `grantee_staff_id` field on every tenant-readable response is blanked (`null`, scope kept) when the
actor's scope differs from the caller's scope, through **one shared helper** modelled on `launchCall.dto`
(`internal/httpserver/launch_authorisation_routes.go:717`). Known fields (grep at `6bda8ac`, `json:"*_by"` /
`grantee_staff_id` in `internal/httpserver`):

| DTO | Fields |
|---|---|
| Kill switch / history (`payments_kill_switch_handlers.go:473`) | `changed_by` |
| Kill-switch release request (`payments_kill_switch_handlers.go:502,507`) | `requested_by`, `approved_by` |
| K1 grant request / grant (`capability_routes.go:247,257,263,287`) | `requested_by`, `grantee_staff_id` |
| K2 adjustment (`manual_adjustment_routes.go:247`) | `initiated_by` |
| K3 resolution (`payment_force_resolution_routes.go:231`) | `requested_by` |
| Provider credential handle (`provider_credential_handlers.go:178`) | `revoked_by` |
| KYC review (`kyc_admin_handlers.go:42`) | `reviewed_by` (same-scope today; routed through the helper anyway) |
| Already redacted / platform-only | launch DTOs (`launch_authorisation_routes.go:286,316,956`); HSEC `requested_by` and alert-route `superseded_by` (PA-only routes; still routed through the helper) |

A static test fails the build on any response struct field whose JSON name ends in `_by` or equals
`grantee_staff_id` that is not populated through the helper. The same rule applies to R6 `proposer_id`.

### 4.2 H2 - M4 evidence reference (blind entry while `evidence_ref_hash` is OPEN)

`resolutionDTO.evidence_reference` (`payment_force_resolution_routes.go:243`) returns the M4 reference R to every K3
reader, and R is written into tenant audit metadata (`internal/payments/manual_resolution.go:1253-1254`,
`md["evidence_reference"]`). That defeats blind entry. While `evidence_ref_hash` is OPEN: remove the reference text
from the K3 DTO and from the audit metadata **for every caller** (replace with the hash or `evidence_reference_present:
bool`). Invariant 7.2 applies to existing DTOs, not only to new routes.

## 5. Recorded decisions (orchestrator defaults, reversible by the owner)

**D-OPS-1 - build the console before a real provider: YES (default recorded 2026-10-11).** Build now, read-only
views first, then action screens; every governed-money screen labelled MOCK (principle 6). Building UI does not
change any non-MOCK gate: M4 non-MOCK stays blocked by D-7/T10/S-3 and the `evidence_ref_hash` binding; HSEC
production stays blocked by Q-HSEC-1/2/3; GAP-AAM stays read-only; alert human delivery stays ALERT-DELIVERY-1.
Nothing genuinely blocks the build.

**D-OPS-2 - who sees park reasons / attempt state / findings (default recorded 2026-10-11):**
- attempt state and park reason (R1): holders of `payment_force_resolve:read` only = FI, CO, PA. FI additionally sees
  the R1b summary on the withdrawal detail (`withdrawal:review`). Not TA, SU, RM (Stage 3D).
- `payment_statement` findings: `payment_force_resolve:read` (FI, CO, PA); `ledger_vs_projection`:
  `ledger_adjustment:read` (FI, CO, PA), since its correction path is K2; casino unchanged (TA, CO, FI).
- `sportsbook_settlement`: **new** `sportsbook_reconciliation:read` granted to **exactly** TA, CO, FI (Stage 10.3 W2b
  casino precedent). A role-matrix test asserts it is absent from PA, SU, RM, PM, BX. The only role-matrix change.
- HSEC: PA only; alerts: PA only (both unchanged).
If the owner reverses the new permission, sportsbook findings stay API-only and S5 drops that stream.

## 6. Build order (small slices)

Owners: `backend` (routes), `backoffice` (screens), `payments`/`ledger-finance` (domain review), `security`
(mandatory on every governed-money slice), `code-reviewer`, `qa` (gate), `product-owner-proxy` (wording/scope).
"Unblocked" = provider-independent and needs no further human decision beyond D-OPS-1/2. **No action screen is
enabled before the S0 H1/H2 tests pass; S1, S3 and S6-S9 do not ship before S0.**

| Slice | Content | Owner / reviewers | Gate |
|---|---|---|---|
| S0 | H1 shared helper + static test + fixes; H2 removal; section 3 generated matrix test; OpenAPI for K1, K2, financial-policy, platform-actions, enforcement-decisions, staff display-name/person-link | backend / security, ledger-finance (H2), code-reviewer | unblocked; first |
| S1 | Alerts console (PA): list with state/severity filters, ack, resolve, routing status read-only | backoffice / security, code-reviewer, PO-proxy | after S0 |
| S2 | R1 + R1b | backend, payments / security, ledger-finance | after S0 |
| S3 | Parked payments queue + attempt detail; withdrawal detail attempt panel; payout instruments view/suspend | backoffice / security, ledger-finance (wording), code-reviewer | after S0, S2 |
| S4 | R2 (payment_statement, ledger_vs_projection, sportsbook_settlement + new permission) | backend / security, ledger-finance | after S0 |
| S5 | Reconciliation views: runs + mismatches for all streams incl. existing casino routes; findings link to K3/K2 screens | backoffice / ledger-finance, code-reviewer | after S4 |
| S6 | Payment kill switch console (engage, release request/approve/cancel) | backoffice / security, payments | after S0 |
| S7 | R4 + K1 grants console (request, approve, reject, cancel, revoke, list) - prerequisite for FI to act in K2/K3 | backend + backoffice / security | after S0 |
| S8 | K3 M1/M2 console: request from an R1 attempt or R2 finding, closed reason/basis/context catalogues, approve/reject/cancel with frozen `payload_hash`, refusal map | backoffice / security, ledger-finance, payments, code-reviewer | after S0, S3, S5, S7; MOCK |
| S9 | R5 + R6 + K2 manual adjustments console + financial policy view and change approvals | backend + backoffice / ledger-finance, security | after S0, S7; MOCK |
| S10 | R3 + HSEC console (PA) | backend + backoffice / security, ledger-finance | after S0; MOCK only; production policy row gated by Q-HSEC-1/2/3 |
| S11 | R2b + M4 screens: blind entry of the reference (never pre-filled), evidence line picker with seal status | backend + backoffice / security, ledger-finance | after S0, S8; MOCK only; non-MOCK gated by D-7, T10, S-3, `evidence_ref_hash` |
| Later | risk rules UI, provider credentials UI, registries UI, platform-actions audit view, KYC enforcement view, bonus remainder (needs list routes), staff list screen, asset change-request list | domain owners | after S0; not day one |
| Not built | GAP-AAM exit, risk REVIEW queue, RG limits, AML cases, tenant alert view, instrument verify/reinstate | architect + owner | needs-design / owner |

Day-one operator set = S0-S8. S9-S11 follow. Each slice ends with qa evidence and a registry row; no slice claims more
than `IMPLEMENTED against MOCK`.

## 7. Architectural invariants (for `qa` and `code-reviewer`)

1. Every new route is GET-only, writes nothing except the read/platform-action audit rows required in section 4, and
   is refused (403) for every role not in section 3; a tenant caller with another tenant's `tenantID` gets 404/403
   and RLS returns zero foreign rows even if the handler check is removed (test both). The permission check runs
   before any query (R2 per stream).
2. No response field - new **or existing** - contains a platform staff id for a tenant caller (H1), an email,
   destination detail, ciphertext, fingerprint, seal value, raw provider payload, a player identity on R1/R2/R3/R5, or
   (while `evidence_ref_hash` is OPEN) a provider/evidence reference text (H2, any caller, DTO and audit metadata).
3. `admissible_kinds` agrees with the database predicates on a fixture of every terminal reason (one table test).
4. The console sends no client-chosen tenant id for tenant-scoped callers; it never computes approval counts or
   self-approval authoritatively; it formats money without floating point.
5. No console button exists for a transition no route performs (static test over the action maps).
6. A static test fails the build on any `*_by` / `grantee_staff_id` response field not populated through the H1 helper.
7. A generated test checks the section 3 matrix against `rolePermissions`; a role-matrix test pins
   `sportsbook_reconciliation:read` to exactly TA, CO, FI.
8. R6: a test proves tenant B's policy changes never appear for tenant A; cross-tenant rows only via `WithPlatformAdmin`.
9. The client never hides a refusal: every 403/409 is rendered through the code map, and no screen gates on
   `permissions.ts` alone (the server answer decides what is shown as done).

## 8. Consequences

- Operators get a provider-independent console for every approved governed flow; the API remains the only contract.
- Eight small read routes (R1-R6, R1b, R2b), two blocker hardenings (H1, H2) and one permission are added.
- ADR 0101/0111 header status lines are reconciled by a one-line STATUS note each (2026-10-11); bodies unchanged.
- Deferred (recorded, not built): tenant-visible alerts, unified cross-domain approvals inbox (gap audit section 12),
  risk REVIEW queue, AML case management.

## 9. Security design review (2026-10-11): APPROVE WITH CONDITIONS - applied

| # | Condition | Where applied |
|---|---|---|
| 1 | BLOCKER H1 wider than first listed (live-confirmed); one shared helper; static test; S1/S3/S6-S9 not before S0 | 4.1, 6, 7.6 |
| 2 | BLOCKER H2: `evidence_reference` in K3 DTO and tenant audit metadata; remove for every caller while `evidence_ref_hash` is OPEN | 4.2, 7.2 |
| 3 | R5: wallet fields only, 404 foreign/unknown, read audit; PA needs active K2 grant + ADR 0104 platform action; no broad platform SELECT policy | 4 (R5) |
| 4 | R3: narrow SECURITY DEFINER function, no platform-wide RLS policy, no player id | 4 (R3) |
| 5 | R1: `payment_force_resolve:read` alone; no reference text, player id or destination; platform_acting reads audited | 4 (R1), 5 |
| 6 | R6: `WithPlatformAdmin` branch only; tenant isolation test; `proposer_id` blank across scopes | 4 (R6), 7.8 |
| 7 | R4: staff:manage OR capability_grant:request; path tenant only; platform staff excluded in SQL; paginated; closed role filter | 4 (R4) |
| 8 | `sportsbook_reconciliation:read` exactly TA, CO, FI; absence test for PA, SU, RM, PM, BX | 5, 7.7 |
| 9 | R2: 400 on unknown input, `other` output only, no player identity in `subject_ref`, permission before query | 4 (R2), 7.1 |
| 10 | Sequencing: S6-S9 depend on S0; no action screen before H1 tests pass | 6 |
| 11 | Generated section 3 vs `rolePermissions` test | 7.7 |
| 12 | Client never hides a refusal; code map for every 403/409; no gating on `permissions.ts` alone | 1.1, 7.9 |

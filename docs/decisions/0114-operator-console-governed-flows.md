# ADR 0114 - Operator console for the governed financial/operational flows

- **Status:** PROPOSED - DESIGN ONLY (`architect`, 2026-10-11, repository at `6bda8ac`). Nothing in this ADR is
  implemented. Every UI and route below is `NOT IMPLEMENTED` until its slice lands; every governed flow it fronts is
  `MOCK` (no real provider).
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

| # | Route | Permission (existing unless noted) | Shape (fields) | Reviews |
|---|---|---|---|---|
| R1 | `GET /v1/admin/tenants/{tenantID}/payment-attempts?operation=payout|deposit&state=&terminal_reason=&withdrawal_request_id=&limit&offset` and `GET .../payment-attempts/{attemptID}` | `RequireAnyPermission(payment_force_resolve:read, withdrawal:review)` + `canActOnTenant` | `id, operation, state, terminal_reason, subject_ref {withdrawal_request_id or deposit_id}, amount_minor_units (string), asset_code, exponent, provider_id, has_provider_reference (bool), created_at, state_changed_at, open_resolution_id, admissible_kinds[]` | security (exposure), ledger-finance (`admissible_kinds`), payments |
| R1b | add `payout_attempt` (latest attempt summary, R1 field subset) to `GET /v1/admin/withdrawals/{id}` | unchanged (`withdrawal:review`) | `attempt_id, state, terminal_reason, open_resolution_id` or `null` | security, code-reviewer |
| R2 | `GET /v1/admin/tenants/{tenantID}/reconciliation/runs?stream=` and `.../reconciliation/mismatches?stream=&status=&kind=&subject_id=` | per stream: `payment_statement` -> `payment_force_resolve:read`; `ledger_vs_projection` -> `ledger_adjustment:read`; `sportsbook_settlement` -> **new** `sportsbook_reconciliation:read`; casino streams stay on the existing casino route | run: `id, stream, period_start, period_end, run_at, status`; mismatch: `id, run_id, stream, kind, status, subject_ref, expected/actual minor units (string), asset_code, detected_at` | security, ledger-finance |
| R2b | `GET .../payment-attempts/{attemptID}/statement-evidence` (M4 line picker; MOCK only) | `payment_force_resolve:request` | `evidence_line_id, import_id, seal_verified (bool), verdict, line_date` - never the reference text while `evidence_ref_hash` is OPEN | security (T10), ledger-finance |
| R3 | `GET /v1/admin/tenants/{tenantID}/withdrawal-hold-candidates?limit&offset` (approved withdrawals on a non-active tenant or brand) | `withdrawal_hold_resolution:read` (PA only) | `withdrawal_request_id, brand_id, amount_minor_units, asset_code, approved_at, tenant_status, brand_status, open_resolution_id` | security, ledger-finance |
| R4 | `GET /v1/admin/tenants/{tenantID}/staff?role=&limit&offset` | `RequireAnyPermission(staff:manage, capability_grant:request)` + `canActOnTenant` | `id, role, display_name, person_linked (bool), status, created_at` - no email, no person_id; platform staff never listed | security |
| R5 | `GET /v1/admin/players/{playerID}/wallets` | `ledger_adjustment:read` + `RequireTenantScope` (PA via `/tenants/{tenantID}/players/{playerID}/wallets` platform_acting) | `wallet_id, asset_code, exponent, projected_balance_minor_units (string), projection_as_of` labelled "projection, not authoritative" | ledger-finance, security |
| R6 | `GET /v1/admin/financial-policy-changes?status=&limit&offset` (PA: platform + all tenants; TA: own tenant) | `financial_policy:read` | `id, scope, tenant_id, profile_code, proposed values, status, proposer_scope, created_at` (proposer id only within the same scope) | security, ledger-finance |
| H1 | **Hardening, not a route:** verify and, where missing, redact to scope-only the platform actor id on tenant-readable governed DTOs: K3 `requested_by` (`payment_force_resolution_routes.go:231,253`), K1 `requested_by` (`capability_routes.go:263,274`), kill-switch `requested_by` / `approved_by` (`payments_kill_switch_handlers.go:502-523`) | n/a | `requested_by: null` + `requested_by_scope` when caller scope differs from actor scope | security |

`admissible_kinds` (R1) must be computed by calling the same database predicates the request path uses
(`payment_m2_admits`, `payment_m4_in_scope`, migrations 0115/0125/0127), not a Go re-implementation; it is a hint and
the request path re-checks. H1 is a suspected gap: a grep of those handlers found no redaction code; a test must
decide it before any tenant-facing governed screen ships.

## 5. Owner questions (only these two)

**D-OPS-1 - build the console before a real provider?** Recommended answer: **YES, build now** (the 2026-10-11
directive). Read-only views first, then action screens, every governed-money screen labelled MOCK (principle 6).
Building UI does not change any non-MOCK gate: M4 non-MOCK stays blocked by D-7/T10/S-3 and the `evidence_ref_hash`
binding; HSEC production stays blocked by Q-HSEC-1/2/3; GAP-AAM stays read-only; alert human delivery stays
ALERT-DELIVERY-1. Nothing here genuinely blocks the build.

**D-OPS-2 - who sees park reasons / attempt state / findings?** Recommended answer, using existing permissions only
except one addition:
- attempt state and park reason: FI (`withdrawal:review`, `payment_force_resolve:read`), CO and PA
  (`payment_force_resolve:read`). Not TA, SU, RM (Stage 3D: TA holds no withdrawal governance).
- `payment_statement` findings: same holders as above; `ledger_vs_projection`: `ledger_adjustment:read` (FI, CO, PA),
  since its correction path is K2; casino unchanged (TA, CO, FI).
- `sportsbook_settlement`: **new** `sportsbook_reconciliation:read` for TA, CO, FI - the exact Stage 10.3 W2b casino
  precedent. This is the only role-matrix change and needs `security` sign-off.
- HSEC: PA only; alerts: PA only (both unchanged).
If the owner prefers zero new permissions, sportsbook findings stay API-only and slice S5 drops that stream.

## 6. Build order (small slices)

Owners: `backend` (routes), `backoffice` (screens), `payments`/`ledger-finance` (domain review), `security`
(mandatory on every governed-money slice), `code-reviewer`, `qa` (gate), `product-owner-proxy` (wording/scope).
"Unblocked" = provider-independent and needs no further human decision beyond D-OPS-1/2.

| Slice | Content | Owner / reviewers | Gate |
|---|---|---|---|
| S0 | H1 redaction test + fix (DONE, section 9); OpenAPI for K1, K2, financial-policy, platform-actions, enforcement-decisions, staff display-name/person-link | backend / security, code-reviewer | unblocked |
| S1 | Alerts console (PA): list with state/severity filters, ack, resolve, routing status read-only | backoffice / security, code-reviewer, PO-proxy | unblocked |
| S2 | R1 + R1b | backend, payments / security, ledger-finance | unblocked (D-OPS-2 roles) |
| S3 | Parked payments queue + attempt detail; withdrawal detail attempt panel; payout instruments view/suspend (UI-ready) | backoffice / security, ledger-finance (wording), code-reviewer | after S2 |
| S4 | R2 (payment_statement, ledger_vs_projection; sportsbook if D-OPS-2 adds the permission) | backend / security, ledger-finance | unblocked |
| S5 | Reconciliation views: runs + mismatches for all streams incl. existing casino routes; findings link to the K3/K2 screens | backoffice / ledger-finance, code-reviewer | after S4 |
| S6 | Payment kill switch console (engage, release request/approve/cancel) | backoffice / security, payments | unblocked |
| S7 | R4 + K1 grants console (request, approve, reject, cancel, revoke, list) - prerequisite for FI to act in K2/K3 | backend + backoffice / security | after S0 |
| S8 | K3 M1/M2 console: request from an R1 attempt or R2 finding, closed reason/basis/context catalogues, approve/reject/cancel with frozen `payload_hash`, refusal map | backoffice / security, ledger-finance, payments, code-reviewer | after S3, S5, S7; MOCK |
| S9 | R5 + R6 + K2 manual adjustments console + financial policy view and change approvals | backend + backoffice / ledger-finance, security | after S7; MOCK |
| S10 | R3 + HSEC console (PA) | backend + backoffice / security, ledger-finance | MOCK only; production policy row gated by Q-HSEC-1/2/3 |
| S11 | R2b + M4 screens: blind entry of the reference (never pre-filled), evidence line picker with seal status | backend + backoffice / security, ledger-finance | MOCK only; non-MOCK gated by D-7, T10, S-3, `evidence_ref_hash` |
| Later | risk rules UI, provider credentials UI, registries UI, platform-actions audit view, KYC enforcement view, bonus remainder (needs list routes), staff list screen, asset change-request list | domain owners | unblocked, not day one |
| Not built | GAP-AAM exit, risk REVIEW queue, RG limits, AML cases, tenant alert view, instrument verify/reinstate | architect + owner | needs-design / owner |

Day-one operator set = S0-S8 (withdrawal queue already exists; parked payments, force-resolution approvals,
reconciliation, alerts, kill switch). S9-S11 follow. Each slice ends with qa evidence and a registry row; no slice
claims more than `IMPLEMENTED against MOCK`.

## 7. Architectural invariants (for `qa` and `code-reviewer`)

1. Every new route is GET-only, writes nothing except its own read audit if the domain already audits reads, and
   is refused (403) for every role not in section 3; a tenant caller with another tenant's `tenantID` gets 404/403
   and RLS returns zero foreign rows even if the handler check is removed (test both).
2. No new response field contains a platform staff id for a tenant caller, an email, destination detail, ciphertext,
   fingerprint, seal value, raw provider payload, or (while `evidence_ref_hash` is OPEN) a provider reference text.
3. `admissible_kinds` agrees with the database predicates on a fixture of every terminal reason (one table test).
4. The console sends no client-chosen tenant id for tenant-scoped callers; it never computes approval counts or
   self-approval authoritatively; it formats money without floating point.
5. No console button exists for a transition no route performs (static test over the action maps).

## 8. Consequences

- Operators get a provider-independent console for every approved governed flow; the API remains the only contract.
- Eight small read routes (R1-R6, R1b, R2b) and one redaction hardening (H1) are added; one permission is proposed.
- Inconsistency recorded, not fixed here: ADR 0101/0111 header status lines still read PROPOSED / NOT IMPLEMENTED while
  the gap audit (section 2) records K3/M4/HSEC as IMPLEMENTED (MOCK); the orchestrator should reconcile the headers.
- Deferred (recorded, not built): tenant-visible alerts, unified cross-domain approvals inbox (gap audit section 12),
  risk REVIEW queue, AML case management.

## 9. Slice S0 record (security hardening + OpenAPI) - IMPLEMENTED against the local DB / MOCK, reviews pending

Status of S0 only; the rest of this ADR stays PROPOSED. Labels: staff-id redaction IMPLEMENTED; evidence-reference
removal IMPLEMENTED; OpenAPI for the six undocumented families IMPLEMENTED (documents the routes as mounted, no new
behaviour). `security`, `ledger-finance` and `code-reviewer` reviews are pending.

### 9.1 The defect (security-confirmed live)

H1 in section 4 was a suspicion; it was real. A `platform_admin` engages a payment kill switch on tenant T; a
`tenant_admin` of T calls `GET .../kill-switches/{id}` and received `changed_by` = the platform admin's staff id
with `changed_by_scope = platform`. The same class existed on every tenant-readable governed DTO.

### 9.2 The fix: one helper, no way around it

`internal/httpserver/staff_identity_redaction.go`:

- `staffViewerOf(tenant.Context)` derives the viewer from the server-side authenticated context only (nil tenant id
  = platform principal, the `canActOnTenant` test).
- `staffViewer.ref(actorScope, id)` / `optRef(...)` return a `staffRef`, the only type a staff-id response field may
  have. A tenant viewer sees the id **only when the actor scope is exactly `tenant`**; any other value (including
  empty, unknown, and the `platform_acting` value the K2/K3 tables actually store - the OpenAPI had `platform` for
  the kill switch and `platform_acting` for K2/K3) is `null`. A zero `staffRef` marshals as `null`, so a DTO built
  without the helper fails closed. A platform viewer sees everything (unchanged).
- Rows with an actor id but no scope column (provider-credential `revoked_by`, the generic tenant
  `GET /v1/admin/audit-log` `actor_id`) use `tenantStaffIDs`: visible only if the id is a `staff_users` row of the
  caller's own tenant (explicit `tenant_id` filter, inside the tenant transaction; platform staff are RLS-hidden
  there too). No migration was needed.
- The wire change: the field stays present as `null` (it is `omitempty` only where it was before and the actor is
  absent, e.g. `approved_by` before approval). The sibling `*_scope` field is the scope-only marker.
- Static pin `staff_identity_static_test.go`: an AST walk over every non-test file of the package fails the build on
  any response field whose JSON name looks like a staff/actor/approver/principal id and which is neither a
  `staffRef` nor in a justified allowlist, on any hand-built visible `staffRef` outside the helper, and on a stale
  allowlist entry. A planted-offender test proves the scanner flags `closed_by string`, `approver_ids []string`,
  `grantee_staff_id *string` and a forged literal. `evidence_reference_static_test.go` pins the evidence-reference
  removal (9.4).

### 9.3 Inventory (every staff-id-shaped field in `internal/httpserver`)

| Field(s) | DTO / route family | Reader | S0 treatment |
|---|---|---|---|
| `changed_by` | kill switch (`killSwitchDTO`; list, get, engage, release-approve responses) | tenant_admin | `staffRef`, scope `changed_by_scope` |
| `requested_by`, `approved_by` | kill-switch release request | tenant_admin | `staffRef`; `*_scope` |
| `grantee_staff_id`, `requested_by` | K1 request DTO | tenant_admin, compliance | `staffRef`; `grantee_scope`, `requested_by_scope` |
| `grantee_staff_id` | K1 grant DTO | tenant_admin, compliance | `staffRef`; `grantee_scope` |
| `initiated_by` | K2 `adjustmentDTO` (submit, list, get, decision, cancel) | compliance, finance | `staffRef`; `initiated_by_scope` (`tenant` / `platform_acting`) |
| `requested_by` | K3 `resolutionDTO` (request, list, get, decision, cancel) | compliance, finance | `staffRef`; `requested_by_scope` |
| `requested_by` (+ new `requested_by_scope`) | HSEC `holdResolutionDTO` | platform_admin only today | `staffRef`, applied for defence in depth |
| `revoked_by` | provider-credential handle | tenant_admin | `staffRef` via tenant-staff membership (no scope column) |
| `actor_id` | generic tenant `GET /v1/admin/audit-log` (platform staff who acted inside the tenant, e.g. brand/staff creation) | tenant_admin, compliance | `staffRef` via tenant-staff membership; non-staff actors unchanged |
| `requested_by`, `decided_by`, `executed_by`, `approver_ids` | launch (ADR 0112) | tenant | already redacted inline by `launchCall`; allowlisted, existing tests unchanged |
| `actor.staff_id`, `approval_chain[].actor_staff_id` | `GET /v1/admin/audit-log/platform-actions` (ADR 0104) | tenant_admin, compliance | **not changed - see 9.5** |
| `requested_by_principal_id`, `approver_principal_id`, `applied_by_principal_id` | provider-credential requests | platform_admin only (`request`/`approve` permissions) | allowlisted: platform-only reader |
| `created_by_actor_id` | sportsbook jurisdiction restrictions | platform_admin only | allowlisted |
| `created_by_actor_id` | sportsbook exposure limits | risk_manager (tenant staff) | allowlisted: tenant-own actors |
| `reviewed_by` | KYC case queue | compliance (tenant staff) | allowlisted: tenant-own actors |
| `requested_by_principal_id`, `applied_by_principal_id` | bonus change requests (`RequireTenantScope`) | tenant staff | allowlisted: tenant-own actors |
| `superseded_by` | alert routing | platform_admin only | allowlisted: id of a routing row, not a staff id |
| `person_id` | staff create / person-link bodies and `staffResponse` | staff:manage | allowlisted: input, or the person link of the tenant staff just created |
| financial-policy changes and policies | `policyChangeDTO`, `policyDTO` | tenant_admin, compliance | no actor field exists; nothing to redact |
| KYC enforcement decisions | `enforcementDecisionResponse` | compliance | no actor field exists |

Audit **metadata** is not returned by the tenant audit routes (they select no metadata); the platform-actions route
projects only allowlisted keys.

### 9.4 Second defect: the M4 evidence reference text (security BLOCKER 2)

`resolutionDTO.evidence_reference` returned the PSP line reference R to every K3 reader, and R was written into the
tenant-visible audit metadata (`evidence_reference`). That defeats the blind-entry design under which each approver
types R independently. While `evidence_ref_hash` is OPEN:

- the K3 DTO no longer has `evidence_reference`; it has `evidence_reference_present: true` (M4 rows only);
- the audit metadata key is now `evidence_reference_present: true`; R is not written;
- **unchanged:** the column `payment_manual_resolutions.evidence_reference`, `EvaluateM4Evidence`, the equality
  checks in `m4EvidenceRefusal`, `postM4` (keys the completion `provider_id:R`), the reconciliation reader
  (`payment_statement_k3.go`) and every DB trigger/guard - the server still reads and compares R from the database.
  No M2/M4 verification was weakened.

Changed assertions (all listed in the commit): `payment_force_resolution_m4_api_integration_test.go` (was
`EvidenceReference == r`; now the key must be absent, R must not appear anywhere in the body, and
`evidence_reference_present` must be true) and `m4_resolve1_integration_test.go` (was audit `evidence_reference == r`;
now `evidence_reference_present == "true"` and R must not appear anywhere in that audit row's metadata).
Type-adaptation only: `backoffice_admin_integration_test.go` and `identity_flow_integration_test.go` read
`ActorID.String()` (own-tenant actor still visible, same expectation).

Residuals recorded for `ledger-finance` / `security` (not changed in S0):

1. After a **paid** M4 executes, R is, by design, the completion's provider transaction id (`provider_id:R`, LF F6) in
   the ledger and the `withdrawal.completed` audit row. That is post-execution, after every blind entry, and is the
   ledger key; it is not an approver-facing exposure.
2. `provider_reference_at_submission` (the attempt's own provider reference X, pinned for the not-paid and M2 paths) is
   still in audit metadata. For M4 not-paid the blind entry would be the portal's declined reference, which may equal
   X. Decision needed: hash it like R or leave it.

### 9.5 Tension recorded, not resolved here: ADR 0104 identifies the platform actor on purpose

`GET /v1/admin/audit-log/platform-actions` (ADR 0104 section 5.2) returns `actor.staff_id` and `display_name` of the
platform staff member for every platform action concerning the tenant, including kill-switch engage / release, with
an approval chain. That was an owner-approved, product-owner-proxy-reviewed decision. ADR 0114 principle 7 says the
opposite for every other surface. With S0 alone, a tenant that wants the platform actor's id can still read it from
that route, so the kill-switch DTO redaction is only as strong as that route. S0 did **not** change ADR 0104 (a
decision, not an implementation detail). Required decision (`architect` + owner + `security`): amend ADR 0104 to
scope-only (consistent with ADR 0112's launch rows, which already withhold) or record the exception as final.

### 9.6 OpenAPI (exactly as implemented)

Documented in `docs/api/openapi/platform-api.yaml`: K1 (7 operations), K2 (6), financial-policy changes and policies
(6), `GET /v1/admin/audit-log/platform-actions`, `GET /v1/admin/kyc/enforcement-decisions` (note: tenant-scope guard,
so a platform caller gets 403 even though `platform_admin` holds the permission), both staff display-name routes and
both person-link routes. Schemas changed: kill switch, release request, K3, HSEC, provider-credential handle and
`AuditEntry` (nullable staff ids, `evidence_reference_present`). DB-free drift tests
(`openapi_governed_routes_contract_test.go`): every mounted route is documented and every documented operation is
mounted; each DTO's JSON field set equals its schema's property set (reflection); request bodies refuse unknown
properties and carry no tenant field; the redaction contract and the absence of `evidence_reference` are pinned.

# ADR 0119 S4a - staff route chain and route registration

> **ROUTE-REGISTRATION FREEZE (effective from the S4a merge; announced 2026-10-13).**
>
> Every staff-authenticated route - every path under `/v1/admin/`, `/v1/staff/me/`, `/v1/staff/mfa/` and
> `POST /v1/staff/auth/step-up` - MUST be registered exactly like this:
>
> ```go
> mux.Handle("<METHOD> <literal path>", staffRoute(deps, <class>, <inner>))
> ```
>
> - `<class>` is one of the `routeClass` identifiers in `internal/httpserver/staff_route.go`: `stepUpNone`,
>   `stepUpFinancial`, `stepUpCritical`, or a named classifier (`stepUpByLaunchAction`,
>   `stepUpByFinancialPolicyRun`, `stepUpByAlertKind`, `stepUpByRGLimitDirection`). Pick it with ADR 0119
>   section 8.1 (reads are always `stepUpNone`). A new classifier needs an ADR 0119 amendment.
> - `<inner>` is the route's own gates and handler: `auth.RequirePermission(...)` /
>   `auth.RequireAnyPermission(...)`, `auth.RequireTenantScope(...)` where needed, then the handler. Do NOT add
>   `auth.Middleware` or `auth.RequireStaffPrincipal`: `staffRoute` already applies them, followed by the
>   per-request liveness check and `auth.RequireAAL2`.
> - Add the route to the golden table `staffRouteGolden` in
>   `internal/httpserver/staff_route_golden_test.go` with the same class, in the same change. If the route was
>   on the "classified on arrival" list (`staffRoutesClassifiedOnArrival`, ADR 0119 section 8.2), remove it
>   from there.
> - The build fails otherwise: `TestStaffRoutes_EveryStaffRouteRegisteredThroughHelper` (shape),
>   `TestStaffRoutes_GoldenClassification` (class and membership, both ways),
>   `TestStaffRoutes_EveryMutatingRouteClassified` (route counts),
>   `TestStaffRoutes_DBFree_ChainOnEveryRoute` (HTTP sweep of every golden route).
> - The two mixed-principal self-service routes (`GET /v1/me/sessions`, `DELETE /v1/me/sessions/{id}`) use
>   `liveIfStaff(deps, <handler>)` instead.
>
> **In-flight slices must rebase onto S4a before they merge:** ADR 0114 S1b, the ADR 0116 AML HTTP slice and
> ADR 0115 S5 (and any other branch that adds or edits a staff route). After the rebase, re-register each new
> route with `staffRoute` and a class, add it to the golden table, and rerun the four tests above. ADR 0115 S2's
> routes are already classified here (the staff RG limit route uses the `rgLimitChangeClass` classifier; the RG
> floor request and approval routes and the limit-config write are `critical`; reject and cancel are `none`).

Status: `IMPLEMENTED` on the S4a branch (not merged; gate G pending). Step-up enforcement is S4b and stays
`NOT IMPLEMENTED`. ADR record: `docs/decisions/0119-staff-mfa-stepup-and-lifecycle.md` section 25.

## 1. The chain

```text
auth.Middleware -> auth.RequireStaffPrincipal -> requireLiveStaffSession -> auth.RequireAAL2 -> <inner>
```

- `auth.Middleware` verifies the token (a staff token must carry every assurance claim at aal 2, and a role from
  the closed staff role set) and attaches the verified `auth.StaffSession` (sid, aal, amr, auth_time, mfa_at, sv).
- `auth.RequireStaffPrincipal` refuses player and service tokens (403) before any database work.
- `requireLiveStaffSession` runs ONE query in ONE read-only transaction scoped to the token's tenant (or platform)
  and principal, with `statement_timeout = 500ms`, sent as one pgx batch; the transaction commits and releases its
  connection before the handler runs. It refuses with one `401 session_revoked` when the session row is missing,
  revoked or past its absolute lifetime (database clock), the staff member is not `active`, the role differs from
  the token, the staff version differs from `sv` (every row is version 0 until migration 0139), or the staff
  member has no active, non-hard-locked second factor. A database fault is `503 service_unavailable`.
- `auth.RequireAAL2` refuses (401) a request without an aal2 staff session in its context (defence in depth: the
  token verifier already refuses any other aal).
- S4b appends `auth.RequireStepUp(class)` at the end of the chain. S4a records the class only.

Metric: `staff_liveness_decisions_total{outcome="allowed|refused|error", reason=...}`. Alert rules:
`StaffLivenessErrors` (page) and `StaffLivenessRefusals` (ticket) in `deploy/observability/prometheus-alert-rules.yml`
(not wired: ALERT-DELIVERY-1). Log events: `staff_liveness_refused` (request id, reason) and `staff_liveness_failed`
(request id, error).

## 2. Golden classification (178 routes: 108 mutating, 70 reads)

The authoritative table is `staffRouteGolden` (`internal/httpserver/staff_route_golden_test.go`). Differences from
ADR 0119 section 8.2 as written at `a3e56dd`, all from the S0 review fixes or later arrivals:

| Route | Class | Why |
|---|---|---|
| `POST /v1/admin/tenants/{tenantID}/staff` (legacy admin-chosen password) | critical | Review C7 |
| `POST /v1/admin/tenants/{tenantID}/payments/kill-switches/{killSwitchID}/release-requests`, `.../kill-switch-release-requests/{requestID}/approve` | critical | Review C7 (corrected paths) |
| `POST /v1/admin/tenants/{tenantID}/payments/kill-switch-release-requests/{requestID}/cancel`, `POST .../payments/kill-switches` (engage) | none | Protective |
| `POST /v1/admin/alerts/{alertID}/resolve` | classifier `alertResolveClass` | Review C7: critical for P1 / security kinds |
| `POST /v1/admin/tenants/{tenantID}/financial-policy-changes` | classifier `financialPolicyChangeClass` | Review C7: none for tighten-only callers |
| `POST /v1/admin/tenants/{tenantID}/launch-authorisations` | classifier `launchActionClass` | Section 8.2: none for suspend |
| `POST /v1/admin/players/{id}/rg/limits` | classifier `rgLimitChangeClass` | ADR 0115 S2 arrival: critical for an increase or removal, none for a decrease; staff increases stay 403 until S4b plus ADR 0115's classifier slice |
| `POST /v1/admin/platform/rg/limit-config-change-requests`, `.../{id}/approve`, `POST /v1/admin/rg/limit-configs` | critical | ADR 0115 S2 arrival (section 8.2 "classified on arrival") |
| `POST /v1/admin/platform/rg/limit-config-change-requests/{id}/reject`, `.../{id}/cancel` | none | Protective (rule 8.1) |
| `GET /v1/staff/me/mfa` | none | Read; now on the full staff chain (S3 D19 closed) |

Routes the ADR names that do not exist yet stay on the "classified on arrival" list (`staffRoutesClassifiedOnArrival`).

## 3. Not in S4a

Step-up prompts and enforcement, the three classifier functions, the signer backstop, the scoped `FOR SHARE`
re-assertion, the `app.actor_staff_id` self-select policy pair (S4b: S4a's liveness query runs in the token's own
scope and does not need it), lifecycle (S5/S6), UI (S7b/S8).

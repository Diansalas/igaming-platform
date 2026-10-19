# Licence lifecycle and expiry (licensing slice 1)

Scope: how a licence's status, validity dates, review date and evidence reference are recorded, what the platform does at
expiry, and what a regulator suspension looks like. Software capability only: nothing here says a licence is legally
sufficient or that any jurisdiction is approved. Related: `launch-and-suspension.md` (brand/tenant launch and suspension),
`market-blocking-configuration.md` (the operating-market gate), `observability-and-alerting.md` (the alerts).

## Who does what

| Step | Who | Control |
|---|---|---|
| Create a licence row (number, jurisdiction, licensee) | platform admin | `POST /v1/admin/licences` (existing), step-up critical, audited |
| Set status / issue and expiry dates / review date / evidence reference | platform admin ONLY (`jurisdiction_registry:manage`; tenant-scope and every other role are refused 403) | `PUT /v1/admin/licences/{id}/status`, `/validity-dates`, `/review-due`, `/evidence-reference`: step-up **critical** (a stale second factor is refused), reason code required, one audit row with before and after and the change kind, in the same transaction; a `licence_versions` row is written by trigger |
| Read the history | platform admin | `GET /v1/admin/licences/{id}/versions` (append-only; `UPDATE`, `DELETE` and `TRUNCATE` are refused, RLS is platform-admin read and insert only) |
| Bind a licence to a tenant | platform admin | `PUT /v1/admin/tenants/{tenantID}/licence` (existing; refuses a non-active licence) |
| Decide the business meaning (is the licence valid? what evidence?) | the business / compliance, outside the platform | the platform records what it is told; it never invents a date, a reference or a status |

Nothing in the platform fabricates evidence: `evidence_reference` is an opaque reference (1..200 characters, no control
characters, no "@") to a document held elsewhere; the document is never stored here.

## Interim rules (no human answer assumed)

1. **Widening needs two approvals; narrowing stays single actor plus step-up** (migration 0178; this replaces the slice 1 interim
   rule). The DATABASE classifies every change as `widening` (set `active` again, later `expires_at`, cleared expiry, earlier or
   cleared `issued_at`, postponed or cleared review date), `narrowing` (suspend, expire, earlier `expires_at`, a newly set expiry,
   later `issued_at`, a review date set or brought forward) or `neutral` (evidence reference). A widening PUT answers **202** with
   the licensing change request; two other platform admins (distinct Persons, neither the requester's) approve it through
   `POST /v1/admin/licensing-changes/{id}/approve` (see `licensing-change-approvals.md`) and only then does the licence change.
   A narrowing or neutral PUT executes at once (200) and the response carries `change_kind` and `change_request_id`. The
   request bodies are unchanged; `reason_code` must now be a code (1..64 characters of `[A-Za-z0-9_.:-]`). With fewer than three
   platform-admin Persons a widening change cannot be completed (HDR-M-1 interim).
2. **Narrowing stays single-actor by design.** A suspension must be recordable immediately and is the fail-closed direction.
3. **Status vocabulary is `active`, `suspended`, `expired` only.** A regulator **revocation** is recorded as `expired` with
   reason code `regulator_revocation` and the regulator reference as the evidence reference. A distinct `revoked` status is a pending
   human decision.
4. **Boundary.** The platform's existing predicate is used unchanged: a licence is valid from `issued_at` (inclusive) and NOT valid
   on its `expires_at` date (half-open). Confirming that convention with compliance is a pending human item.
5. **No auto-suspension.** The sweep alerts; it never suspends a brand, tenant or licence.
6. **Activation under an invalid licence stays acknowledgeable** (ADR 0112 5.2): the R-LIC-VALID failure must be explicitly
   acknowledged by the approvers. New in this slice: a launch request may not record `licensing_status = in_force` while the bound
   licence is not valid (H-2 `H-2:licensing_status_contradicts_licence_registry`); record the true status (`suspended`, `expired`,
   `applied_pending`, `conditional`) instead. A licence READ ERROR now refuses the evaluation (it used to be an acknowledgeable
   `not_evaluable`), matching ADR 0112 section 8.5.

## What a regulator suspension looks like in the platform

1. A platform admin (fresh second factor) calls `PUT /v1/admin/licences/{id}/status` with `{"status":"suspended","reason_code":"regulator_suspension"}`,
   then records the regulator's reference with `/evidence-reference`.
2. From the next request, for every tenant bound to that licence, the operating-market gate refuses registration, deposit, casino
   launch and sportsbook wagering (`403 MARKET_NOT_AVAILABLE`, one identical refusal, nothing created, a refusal audit row).
   Withdrawals are not market-gated: players can still withdraw.
3. Brands and tenants stay `active` in launch governance. If the business wants them suspended too (for example to stop
   registration pages and staff operations), suspend each through the launch-governance suspend action (`launch-and-suspension.md`).
4. Reinstatement is `{"status":"active"}` with a reason code. It is a widening change: it is filed (202) and needs two approvals (see interim rule 1).
5. The history of every step is in `GET /v1/admin/licences/{id}/versions` and the audit log (`jurisdiction_registry.licence_status_set`).

## Expiry and review sweep

Hourly, read-only. `licence.expired` (P1) when the status is `expired` or `expires_at` is reached; `licence.expiring` (P2) when
`expires_at` or `review_due_at` is within 30 days (an overdue review counts). The window is the constant
`jurisdiction.LicenceExpiryWarningWindowDays`: an INTERIM DEFAULT, not a legal notice period. The alert lists the affected tenant
and brand ids (every tenant and brand bound to the licence, whatever its launch status: a closed tenant can appear, because the
sweep deliberately does not read launch status). Nobody is paged until a human configures an alert route (`alert-delivery-channels.md`); until then the alerts are
durable rows only, visible at `GET /v1/admin/alerts`. See the runbook section in `observability-and-alerting.md`.

**Sweep self-monitoring (migration 0178).** `licence.sweep_failed` (P1) is raised when the tenant listing fails, a tenant read
fails or an alert write fails (phase `list_tenants`, `tenant_read`, `raise`): the safety net did not run completely, investigate
before relying on the absence of `licence.expired`. `licence.unmonitored` (P2) lists a licence bound to a tenant that records no
`expires_at` and has no more specific alert (a licence with no expiry is never in `licence.expiring`); record the expiry, or
accept the gap. A licence bound to NO tenant is not reported (residual: reading unbound licences needs a security decision).

## Read APIs (platform admin, `jurisdiction_registry:read`, step-up none)

`GET /v1/admin/jurisdictions` and `/{id}`; `GET /v1/admin/licences` and `/{id}` (each with `tenant_ids`, the tenants bound to it);
`GET /v1/admin/licences/{id}/versions`; `GET /v1/admin/licence-country-ceilings?licence_id=`; `GET
/v1/admin/operating-country-policies?tenant_id=`; `GET /v1/admin/tenants/{id}/jurisdiction-configs`. The three new lists are paginated
(`?limit=` default 50 max 200, `?offset=`). They show stored versions, not a market-permission decision.

## Pending human decisions (exact questions)

1. Boundary: "Is a licence valid on its own `expires_at` date (current: no) and on its own `issued_at` date (current: yes) in the
   Anjouan licence terms and in each other jurisdiction we onboard?"
2. Evidence: "Which evidence reference must be recorded before a licence may be bound to a tenant or a brand may be activated
   (none is required today)?"
3. Auto-suspend: "When a licence expires, should the platform suspend the dependent brands automatically, or only alert (current)?"
4. Window: "How many days before `expires_at` / `review_due_at` should the warning fire (current: 30)?"
5. Recipients: "Who receives `licence.expired` and `licence.expiring` (no route exists today)?"
6. Revocation: "Do we need a distinct `revoked` licence status, separate from `expired` (current: no)?"
7. Hard block: "Should activation under an invalid licence be refused instead of acknowledgeable (current: acknowledgeable)?"
8. Mandatory issue date: "Must `issued_at` be mandatory (a NULL `issued_at` is currently read as no issue date asserted)?"

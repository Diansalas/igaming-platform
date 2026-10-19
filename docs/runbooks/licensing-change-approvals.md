# Licensing and jurisdiction change approvals (maker-checker)

- **Scope:** the generic two-person approval primitive for licensing and jurisdiction configuration (migration 0177,
  `internal/licensingchange`; MKT-DUAL-1; licensing audit gaps G-1, G-2, G-9; triage L-10, L-24, L-25).
- **Status:** IMPLEMENTED (API only; no Back Office page yet, audit G-10). The approver roles and count are an INTERIM answer to
  HDR-M-1 (section 7).
- **Not a legal authorisation.** An executed change is a decision recorded by named staff. It does not decide whether a licence is
  valid or whether any jurisdiction permits operation. Never enter a value (country, licence number, ruleset id, authorisation
  reference) that a human with the authority has not supplied in the change ticket.

## 1. What is governed

| Kind | Writes | Widening (2 approvals) | Narrowing (single actor) |
|---|---|---|---|
| `licence_create` | a `licences` row (status `active`) | always | - |
| `tenant_licence_bind` | `tenants.licence_id` (bind or rebind) | always | - |
| `tenant_licence_unbind` | `tenants.licence_id = NULL` | - | always |
| `licence_ceiling_version` | a `licence_country_ceilings` version | `enabled` + `active` | `disabled`, or `withdrawn` |
| `operating_policy_version` | an `operating_country_policies` version | `enabled` + `active`; `withdrawn` at the brand / operation rung (absence there inherits) | `disabled` at any rung; `withdrawn` at the tenant rung |
| `licence_status_change` (0178) | `licences.status` | toward `active` (reinstate; also from an unrecognised status) | toward `suspended` / `expired` |
| `licence_validity_dates_change` (0178) | `licences.issued_at` / `expires_at` | a later or cleared `expires_at`; an earlier or cleared `issued_at` (any widening component makes the whole change widening) | a newly set or earlier `expires_at`; a later `issued_at` |
| `licence_review_due_change` (0178) | `licences.review_due_at` | postponed or cleared | set (from none) or brought forward |
| `licence_evidence_reference_change` (0178) | `licences.evidence_reference` | never | always (neutral, single actor) |
| `tenant_jurisdiction_config_version` | close + append a `tenant_jurisdiction_configs` version, or close it | a first version; any ruleset id change; an added currency or payment method; a removed geo block | only removing currencies / payment methods, adding geo blocks, same ruleset ids; or `close` |

The DATABASE classifies each change (`licensing_change_classify`); the caller cannot choose. A narrowing change filed by a
platform admin executes in the same call. A change filed by tenant staff (compliance: tenant-rung policy and jurisdiction
configuration; tenant admin: brand / operation-rung policy; own tenant only) needs one platform approval if narrowing and two if
widening. Tenant staff never approve.

Since migration 0178 the licence lifecycle writes (`PUT /v1/admin/licences/{id}/status | validity-dates | review-due |
evidence-reference`) are kinds of this primitive with unchanged request bodies: a widening request answers **202** with the change
request and needs two approvals; a narrowing or neutral request executes at once (**200**, single actor plus step-up). A request
that changes nothing (the licence already has that status / date / reference) is refused 400 and stores nothing. The database also
guards the `licences` table itself (`licences_update_guard`): the identity columns (jurisdiction, licensee, number, permitted
products / markets) can never change at runtime (`LC042`), and any other runtime UPDATE needs an executed request of the matching
kind in the same transaction (`LC040`), widening ones holding their approvals. A platform GUC alone authorises nothing. Licence
unbind stays single actor (fail-closed direction). An open request of the tenant class no longer blocks the identical request of
the platform class (`LICENSING_CHANGE_DUPLICATE_PENDING` is per requester class). A tenant-filed brand-rung policy naming a brand
that is not the tenant's own is refused at filing as not found.

## 2. Rules the system enforces

- Approvers: ACTIVE platform staff with role `platform_admin` and a Person (interim, HDR-M-1). The requester's staff account and
  any account of the requester's Person never approve or reject. One approval per Person per request: a second staff account of
  the same Person cannot supply the second approval.
- Freshness: at execution every counted approval is at most 24 h old (database cap; configurable lower). An open request expires
  72 h after filing.
- Content is immutable after filing (`LC012`); the payload hash binds the request, every approval and the execution.
- Every request, approval, reject, cancel and execution carries a signed actor proof (ADR 0110, operations
  `licensing_change:{request,approve,reject,cancel,execute}`). No proof issuer: the call fails with 500 and nothing is written.
- The governed tables accept a runtime write only inside the transaction that executed a matching request (`LC040`).
  `tenant_jurisdiction_configs` is append-only (`LC041`) with one open version per (tenant, jurisdiction); a tenant session can
  read it, never write it.
- Execution re-checks everything in one transaction: approvals (count, freshness, Person, role), expiry, the single-actor rule
  (requester, own transaction), and the classification against the state NOW (a change that would now widen is refused, `LC024`;
  file a new request). The domain writer then applies its own rules (licence ceiling, licensee vs licensing model, BYOL
  exclusivity) and writes its own before / after audit.
- Audit: `licensing_change.{requested,approved,rejected,cancelled,expired,executed}` per event (platform-scope rows are projected
  to the subject tenant, ADR 0104; a tenant sees the action, never the platform actor's id), refusals of a decision as
  `licensing_change.{approve,reject}_denied`, plus the domain writer's rows.

## 3. Procedure (API)

Step-up: filing and approving are `critical` (a fresh MFA step-up). Reject, cancel and reads need none.

1. **File:** `POST /v1/admin/licensing-changes` with `{"kind": ..., "payload": {...}, "reason_code": "<code>", "note": "<optional>"}`.
   Every payload key is required (null where it does not apply); unknown keys are refused. `reason_code` is a code
   (`[A-Za-z0-9_.:-]`, 1..64), never free text. Put the change ticket in `authorization_reference` (enabling rows) or `note`.
   The two existing routes file changes too, with unchanged bodies: `POST /v1/admin/licences` (202, `licence_create`) and
   `PUT /v1/admin/tenants/{tenantID}/licence` (a UUID: 202, `tenant_licence_bind`; `null`: 200, `tenant_licence_unbind`
   executed).
2. **Review:** `GET /v1/admin/licensing-changes?status=requested` and `GET /v1/admin/licensing-changes/{changeID}`. Approvers
   check the payload against the ticket, not against memory.
3. **Approve:** two different people (different Persons, neither the requester) each call
   `POST /v1/admin/licensing-changes/{changeID}/approve` with `{"reason_code": "reviewed_ok"}`. The second approval executes the
   change and returns `executed: true` with the ids written (`outcome`).
4. **Reject / cancel:** `.../reject` with a reason code (an approver), or `.../cancel` (the requester).
5. **Verify:** read the resulting row (ceilings / policies: `operatingmarket` explain; licences: `GET /v1/admin/licences`;
   configuration: launch readiness R-JUR-CONFIG).

Emergency stop (disable a country, unbind a licence, close a configuration): a platform admin files the narrowing change; it
executes in the same call. Do not wait for a second person.

## 4. Error codes

| Code | Meaning | Action |
|---|---|---|
| `LICENSING_CHANGE_SELF_APPROVAL` (409) | the requester's Person tried to decide | another Person approves |
| `LICENSING_CHANGE_ALREADY_DECIDED` (409) | this Person already approved | another Person approves |
| `LICENSING_CHANGE_NOT_PENDING` (409) | executed, rejected, cancelled or expired | file a new request if still needed |
| `LICENSING_CHANGE_DUPLICATE_PENDING` (409) | an identical request is open | approve or cancel that one |
| `LICENSING_CHANGE_APPROVALS_STALE` (409) | an approval is older than 24 h | cancel, file a new request |
| `LICENSING_CHANGE_REFUSED` (409) | the domain rules refused (ceiling, concurrency) | fix the precondition (for example the ceiling first) |
| `LICENSING_CHANGE_GUARD_REFUSED` (409) | a database guard (`LC0xx`), for example `LC024` (now widening) | read the code; usually file a new request |
| `LICENSING_CHANGE_PROOF_REFUSED` / `_NO_PERSON` (403) | proof refused / the account has no Person | security incident if unexpected; link the Person |
| 500 | no actor-proof issuer, or a step-up backstop failure | configuration (`ACTOR_PROOF_KEYS`); nothing was written |

## 5. Synthetic environments

`cmd/seed-market` and `cmd/seed-staging-world` still write ceilings and policies directly as the TABLE OWNER (the staging runner
connects as the owner; the governed-write guard admits the owner, the same documented path as ADR 0112 LF2). They keep refusing
production (`APP_ENV` and the `database_environment` anchor, unchanged). Never point them at production; production changes use
section 3.

## 6. Extension

(The licence lifecycle slice used this recipe in migration 0178.) A new kind needs: a `licensing_change_kinds` row, a
`licensing_change_classify` branch and a `licensing_change_row_matches` branch plus a governed-write trigger on the table it
writes (one migration), and a `kindSpec` registered in `internal/licensingchange` with `register()`. Until all exist the kind fails
closed. Narrowing kinds (suspend) stay single-actor.

## 7. Pending human decisions (interim behaviour in force)

- **HDR-M-1:** who approves licensing and market changes, and how many approvals. Interim: two approvals, ACTIVE `platform_admin`
  staff only, distinct Persons, platform scope; tenant staff request and read only. Changing it is one reviewed migration
  (`licensing_change_kinds.widening_approvals` / `approver_roles`).
- **Licence unbind / suspend second person:** interim single actor (fail-closed direction).
- **Tenant-requested narrowing:** interim needs one platform approval (tenant staff never execute).
- See `docs/plans/lic2-maker-checker-record.md` section 8 for the exact questions.

# Slice 5 record: Back Office licensing and jurisdiction section (frontend only)

- **Worktree / branch:** `.claude/worktrees/lic5`, branch `lic5-backoffice`, from the merged integration branch (migration head 0178).
- **Scope:** `/backoffice` only. No Go, no migration, no OpenAPI change (every read endpoint needed already exists).
- **Not a legal opinion.** Nothing in the UI approves a licence or a jurisdiction. Registry status is labelled "recorded", every
  licensing screen carries the server's disclaimer text, and the licence evidence reference is labelled as supplied by the operator
  and not verified by the platform.

## 1. Deliverables and labels

| Deliverable | Label | Where |
|---|---|---|
| API client for all licensing routes (reads, the four lifecycle PUTs, licence create, tenant bind/unbind, change list/get/approve/reject/cancel) | IMPLEMENTED | `backoffice/src/api/licensing.ts` |
| Jurisdictions list (local filter) and detail | IMPLEMENTED | `features/licensing/JurisdictionsPage.tsx` |
| Licences list (local filter), detail (lifecycle, evidence note, `tenant_ids`, `/versions` history, read-only country ceilings with `licence_id`, `current`, paging) | IMPLEMENTED | `LicencesPage.tsx`, `LicenceDetailPage.tsx` |
| Tenant licensing page: licence binding (derived from `tenant_ids`), jurisdiction configs and operating-country policies, both read-only and paged; bind / rebind / unbind filing | IMPLEMENTED | `TenantLicensingPage.tsx`, link from `TenantDetailPage` |
| Filing dialog: create licence, bind/unbind tenant, status, validity dates, review-due, evidence reference, through the EXISTING filing routes | IMPLEMENTED | `FileChangeDialog.tsx` |
| Licensing changes list (status/kind filters), detail with payload, approvals, approve / reject / cancel | IMPLEMENTED | `ChangesPage.tsx`, `ChangeDetailPage.tsx` |
| RBAC: `licensingRead/Request/Approve` flags (platform_admin only), nav entry, route guard | IMPLEMENTED (UI hint only; server authoritative) | `auth/permissions.ts`, `layout/nav.ts`, `app/AppRoutes.tsx` |
| Frontend tests (RBAC, flows, 202 vs 200, step-up, error mapping, contract) | IMPLEMENTED | `licensing.test.tsx`, `licensingSafety.test.ts`, `api/licensing.test.ts`, `layout/nav.test.ts` |
| Generic `POST /v1/admin/licensing-changes` filing (kinds `licence_ceiling_version`, `operating_policy_version`, `tenant_jurisdiction_config_version`) | NOT IMPLEMENTED | out of the brief ("existing change-filing endpoints"); those kinds are shown read-only. Listing/approving them already works in the changes screens (payload is rendered generically) |
| Jurisdiction create (`POST /v1/admin/jurisdictions`) | NOT IMPLEMENTED | not requested; list/detail are read-only |
| Browser run against a real API / visual review | NOT IMPLEMENTED | tests use MSW only (repo convention); no screenshot review was done |
| `npm run lint` | NOT IMPLEMENTED | the backoffice has no lint script; typecheck is `tsc -b` inside `npm run build` |

## 2. Behaviour worth knowing

- **202 is never success.** The filing routes answer 202 `{change_request, executed:false}` for widening and 200 for narrowing /
  neutral. `apiFetch` returns the body only, so `classifyFiling` tells them apart by shape: `change_request` -> "Pending approval.
  Nothing has changed yet ... needs N approvals"; `licence`+`change_kind` -> "Applied at once"; unbind assignment body -> unbound;
  anything else -> "not confirmed". Approve shows "executed" only when `executed:true` and status `executed`; otherwise
  "Approval recorded: x of y. Still pending approval".
- **Approval rules are on screen** (filing dialog, changes list, request detail): 2 distinct people for widening; requester (and
  their person) cannot approve/reject; approvals older than 24 h do not count (approvals older than 24 h are flagged, display only);
  requests expire 72 h after filing; narrowing/neutral by a platform admin executes at once.
- **Requester / already-approved hints.** Approve and Reject are disabled when `requested_by` equals the signed-in `sub`; Approve is
  also disabled when the user already approved. These are hints only: the server (distinct Persons, role) decides, and its refusals
  (`LICENSING_CHANGE_SELF_APPROVAL`, `_ALREADY_DECIDED`, ...) are rendered in fixed words (the server `message` is never shown).
- **Step-up / actor proof.** Exactly like the other critical actions: the `apiFetch` interceptor handles `403 step_up_required`
  (prompt, one re-send of the same body). Filing and approving are critical; reject and cancel need none. The signed actor proof
  (ADR 0110) is produced server-side; the UI never signs, hashes or sends a proof (pinned by a static test).
- **Cancel takes no reason code.** The brief listed "approve / reject / cancel with reason_code", but `POST .../cancel` has no request
  body in OpenAPI (and the API contract test would fail if one were sent). Approve and reject send `reason_code`; cancel sends none.
- **reason_code format** is validated client-side with the OpenAPI pattern `^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$` (1..64, first
  character alphanumeric). The server remains authoritative.
- **No defaults (G-14).** The kind, licence, tenant, jurisdiction, licensee and new status are all explicit choices (placeholder
  options, empty initial state; pinned by tests and a static test). The tenant's licensing model is displayed read-only and cannot
  be set from these screens. Lifecycle date/evidence fields are pre-loaded with the licence's CURRENT values so that a blank field
  is always a deliberate "clear".
- **LC0xx codes.** The API maps database guards to `LICENSING_CHANGE_GUARD_REFUSED` and does NOT put the LC0xx code on the wire
  (`licensing_change_routes.go` writes a fixed message, no `check_codes`). The UI therefore explains the guard family in words and
  would additionally list any `check_codes` matching `^LC0\d\d$` with a fixed description (test: `LC024`). If operators need the
  exact guard code, a backend change is required (add `check_codes`, as the launch routes do); this slice did not make it.
- **Lists.** Jurisdictions and licences are bare, unpaginated arrays on the API, so they are filtered locally. The change list takes
  `?limit` (max 200) and no offset, so the newest 200 matching are shown with a notice. Ceilings, policies and configs are paged
  server-side.
- **Tenant licence binding** is derived from `LicenceDetail.tenant_ids` (the tenant read has no `licence_id`).
- **Existing test changed intentionally:** `layout/nav.test.ts` platform_admin expectation gained `'Licensing'` (new nav entry); no
  assertion was weakened. A new test pins that no other role sees it.

## 3. Verification (all run in this worktree)

- `npm run build` (`tsc -b` typecheck + `vite build`): pass.
- `npx vitest run`: 61 files, 744 tests, all pass (the generic `api/openapiContract.test.ts` picks up `api/licensing.ts`
  automatically and passes; `api/licensing.test.ts` pins that the module contains exactly the 19 expected route/method pairs with
  their body fields and query keys, so that check is not vacuous).
- No backend was touched; no DB was used.

## 4. For the orchestrator

- Merge: fast-forward / merge branch `lic5-backoffice`. No migration, OpenAPI or route-freeze artefact changed.
- Proposed edit (not made; docs/runbooks are shared): in `docs/runbooks/licensing-change-approvals.md` section Status, replace
  "API only; no Back Office page yet, audit G-10" with "API and Back Office section `/licensing` (lic5); platform_admin only", and
  close audit gap G-10 in `docs/plans/licensing-audit-core.md` once reviewed.
- Security review is required for the new admin screens (new admin capability surface, no new permission level was introduced).
- Residuals: LC0xx not on the wire (above); cancel has no reason code; generic filing of ceiling/policy/config kinds not in the UI;
  no browser/visual run.

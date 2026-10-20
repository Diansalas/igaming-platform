# lic12: Back Office screens for operating bases (frontend only)

Branch `lic12-backoffice-basis` from `claude/licensing-jurisdiction-completion` da0af90 (migration head 0180). `/backoffice` only: no Go, no migration,
no OpenAPI change (every call already exists). Software capability only: nothing in the UI approves a jurisdiction or states that any operation is
lawful. A basis is shown as a recorded business decision; evidence and determination references are labelled operator-supplied and unverified.

## 1. Status labels

| Deliverable | Label |
|---|---|
| API client `api/operatingBases.ts` (list, get, generic filing via `POST /v1/admin/licensing-changes`, body-based 201 classification) | IMPLEMENTED |
| Tenant picker (explicit, no default) and per-tenant list (server paged, country / current filters, review overdue / due-soon / re-review indicators) | IMPLEMENTED |
| Detail: all fields, licence / jurisdiction links, decision record and approvers (from `decision` plus the linked change request), same-scope version history | IMPLEMENTED |
| File a basis: every choice explicit (brand scope, product scope, basis kind, licence / jurisdiction), non-dismissable no-licence notice, mandatory review metadata, client validation mirroring the server | IMPLEMENTED |
| Withdraw (single actor) and record a review (extend / flag re-review) with class hint | IMPLEMENTED |
| Links: tenant licensing page, change detail (readable summary of the three kinds, link to the resulting version) | IMPLEMENTED |
| Approve / reject / cancel | IMPLEMENTED by reuse of the lic5 change detail screen, unchanged; payload now also rendered in words |
| Readiness labels for R-OPERATING-BASIS | Already complete (title, note, ids for overdue and due-soon keys); note now points to the new screen |
| Real-API / browser run | NOT IMPLEMENTED here: the orchestrator runs real-API e2e separately |
| Tenant-side read of own bases, brand picker beyond 200 brands, operation vocabulary lookup | NOT IMPLEMENTED (no API; see residuals) |

## 2. Behaviour worth knowing

- Routes under `/licensing`: `operating-bases` (pick tenant), `operating-bases/tenants/:tenantId` (list), `operating-bases/:basisId` (detail). New tab
  "Operating bases". platform_admin only through the existing `licensingRead` guard (UI hint; the server decides every call).
- **201 is not success.** The generic filing route answers 201 for both outcomes (OpenAPI), so the status line says nothing. `classifyBasisFiling`
  reads the body: `executed:false` + request `requested` = "Pending approval, nothing has changed"; `executed:true` + request `executed` = "Applied at
  once"; any other combination or shape = "not confirmed". (The lic5 `classifyFiling` is for the licence-lifecycle 202/200 routes and is unchanged.)
- Step-up: filing is critical; the existing `apiFetch` interceptor prompts once and re-sends the same body once (tested). Nothing signs or sends an actor proof.
- No defaults: brand scope, product scope, basis kind, licence / jurisdiction, review outcome and review date all start empty. Licence options are filtered by
  licensee for the chosen kind (a convenience; the server re-checks).
- The no-licence notice is a fixed `role="note"` component with no controls; it shows on the filing dialog, the review dialog, the detail page and the change summary.
- Review date states are computed against the browser date (due date inclusive, as the resolver) and labelled as a hint; the resolver uses the database clock.
- Server text is never rendered; codes pass through `safeToken`; free text fields are rendered as React text (XSS test with `<img onerror>`).
- Guard code `LC043` text added to the LC map. The route still does not put LC codes on the wire (lic5 residual).

## 3. Honesty: what is mock-tested only

All tests are Vitest + MSW against hand-written responses that follow the OpenAPI shapes. They prove the UI's behaviour given those responses and that every call
(method, path, body keys, query keys) exists in `docs/api/openapi/platform-api.yaml`. They do NOT prove: that the real server accepts the payloads built here
(for example that the country code or operation exists, the `LintFreeText` rules beyond the mirrored subset, licensee matches), that the real 201 body has the
assumed `executed` / `status` combination for set, withdraw and review (taken from the OpenAPI text and `licensing_change_routes.go` reading, not run), that
`decision` / approver data is populated for real executed bases, or any visual rendering. Real-API e2e is for the orchestrator. The static contract check does not
validate payload sub-keys (only top-level body keys).

## 4. Tests (added)

`features/licensing/operatingBases.test.tsx` (RBAC for seven non-admin roles with zero requests, 403 degradation, explicit tenant, list params, indicators, detail,
decision / approvers, history scoping, XSS, closed-version actions, nothing preselected, notice persistence, exact payloads for set / withdraw / review, pending vs
applied vs unconfirmed, refusal wording, 403 on filing, step-up resend once with the same body, change-detail summary), `basisRules.test.ts`,
`api/operatingBases.test.ts` (exact route / method / body / query pins, kind enum, no signing, classification), extra source pins in `licensingSafety.test.ts`.
The generic `api/openapiContract.test.ts` picks the new module up automatically. No existing assertion was changed or weakened.

## 5. Residuals

- R1 The operation vocabulary (`platform_operations`) has no read API; the operation is a validated text field and the server refuses unknown codes.
- R2 The brand select lists the first 200 brands of the tenant.
- R3 Version history searches the newest 200 versions of the country; truncation is stated.
- R4 Review class hint cannot know the server's treatment of a previously null date; the server classifies.
- R5 Security review is required for the new admin surface (no new permission; reuses `operating_market_ceiling:manage` / `licensing_change:*` server side).

## 6. For the orchestrator

Merge the branch; no migration, OpenAPI or route-freeze artefact changed. Proposed text for `docs/plans/lic9-operating-basis-record.md` R7: "Back Office screens
implemented in lic12". Run real-API e2e for: file set (no-licence and licence kinds) -> two approvals -> detail decision; withdraw; review extend / flag.

## 7. Verification

`npm run build` pass; `npx vitest run`: 66 files, 797 tests pass. Re-verified after a transient disk-full window (all files present, suite re-run).
`backoffice/node_modules` is a symlink in the worktree (untracked, ignored or not committed).

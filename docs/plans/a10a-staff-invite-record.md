# A10a - staff invitation UI + UI-to-OpenAPI contract test (record)

Branch `a10a-staff-invite` from `1983863`. Scope: backoffice frontend, plus a documentation-only OpenAPI fix. No Go
code, no migration. Nothing here is a legal or regulatory claim; email delivery stays `MOCK` in development and
`PROVIDER DEPENDENT` in production (ADR 0119 OQ-1).

## What changed

| Area | Status | Where |
|---|---|---|
| Removed-route defect: `createStaff` POSTed `/v1/admin/tenants/{id}/staff` (GET-only since ADR 0119) and collected an admin-chosen password | IMPLEMENTED (fixed) | `backoffice/src/api/tenants.ts`, `features/tenants/InviteStaffForm.tsx` (replaces `CreateStaffForm.tsx`, deleted) |
| Invite form: email, role (platform admin: all 7; tenant admin: tenant_admin/support/compliance), Person ID (required client-side for tenant_admin/finance/compliance, UUID-checked), step-up through the shared `apiFetch` interceptor, fixed code-keyed messages for every documented refusal | IMPLEMENTED | `InviteStaffForm.tsx`, `staffErrors.ts` |
| Invitation-accept page outside the authenticated shell: `/accept-invitation`, token from the URL FRAGMENT only, scrubbed with `history.replaceState`, password + confirmation (min 12, client and server), continues into the existing `EnrolWizard` | IMPLEMENTED | `auth/InvitationAcceptPage.tsx`, `auth/enrolLink.ts`, `api/auth.ts` (`acceptStaffInvitation`), `auth/staffAuthErrors.ts` |
| Emailed link shape `/#/enrol?token=..&invite=1` (what `internal/stafflifecycle/service.go` sends) is routed to the invitation flow; `#/accept-invitation?token=` accepted as equivalent; invitation and enrolment tokens are kept apart (no cross-use) | IMPLEMENTED | `auth/enrolLink.ts` |
| Read-only staff directory with `status` invited/active/suspended/offboarded, authenticator, Person link, last sign-in, status change; shown to staff:read roles (platform_admin, tenant_admin, compliance) | IMPLEMENTED (there was no staff list UI before; the old form's text said "there is no staff list API") | `features/tenants/StaffDirectoryCard.tsx`, `auth/permissions.ts` (`staffRead`) |
| Error boundaries (app level and per-route inside the shell), fixed text, never renders or logs the thrown error | IMPLEMENTED | `components/ErrorBoundary.tsx`, `app/App.tsx`, `layout/AppLayout.tsx` |
| Generic UI-to-OpenAPI contract test over every `backoffice/src/api/*.ts` module (path, method, request-body field names, query keys); shared helper | IMPLEMENTED | `backoffice/src/test/openapiContract.ts`, `backoffice/src/api/openapiContract.test.ts` |
| Four routes the UI already called were missing from `platform-api.yaml` (PUT casino games, PUT casino provider capability, PUT game availability, PUT payment provider capability) | IMPLEMENTED (spec only, shapes copied from the handler request structs; routes already existed in Go) | `docs/api/openapi/platform-api.yaml` |

## Not done (explicit)

- Platform-level staff invitation (kind `platform_invite`) and the four-eyes request list/approve/reject/cancel UI
  (`/v1/admin/platform/staff-lifecycle-requests`): NOT IMPLEMENTED. It is not needed for the tenant invitation flow
  (the only invite the existing UI exposed), and approving needs a `payload_hash` review screen of its own. Follow-up S8.
- Suspend, reactivate, offboard, role change, MFA reset, invitation resend / revoke, lifecycle-events timeline: ADR 0119
  S8 (class B). The directory is read-only; the invite success banner says honestly that resend is not yet available
  when `email_delivery` is `failed`.
- b2c contract test: not wired. The helper runs unchanged against `b2c/src/api` (needs its own `yaml` devDependency and a
  copy or a cross-package import). A one-off probe found 10 findings in b2c (evidence, not fixed here): paths missing
  from the spec `GET /v1/me/casino/games`, `POST /v1/me/casino/games/{}/launch`, `GET /v1/me/rg/status`,
  `POST|GET /v1/me/deposits`, `GET /v1/me/deposits/{}`, `GET /v1/me/wallets`, `GET /v1/me/wallets/{}`; and body field
  `idempotency_key` not in the wager / win request schema. Follow-up (A10b or b2c owner).
- `yaml` was added as a backoffice devDependency (spec parser for the contract test); `package-lock.json` changed.

## Contract test: mismatches it found

Run against `1983863` + the helper, before any fix:

1. `api/tenants.ts:121 POST /v1/admin/tenants/{}/staff` - method not defined (spec has GET only). REAL DEFECT, fixed by
   replacing the call with `POST .../staff/invitations`.
2. `api/casinoAdmin.ts` `PUT /v1/admin/casino/providers/{}/capability`, `PUT /v1/admin/casino/games`,
   `PUT /v1/admin/casino/games/{}/availability` and `api/providerConfig.ts` `PUT /v1/admin/providers/{}/capability` -
   path not in OpenAPI. The routes exist (`casino_routes.go:82,101,105`, `financial_routes.go:108`) and the UI request
   bodies match the Go request structs; the DOCUMENT was incomplete. Fixed by adding the four operations and three request
   schemas. No allowlist entries were needed: `ALLOWLIST` in `openapiContract.test.ts` is empty, and a stale entry fails
   the suite.
3. After extractor hardening nothing else: all body fields and query keys the client sends exist in the schemas.

The test also has a self-test suite proving it fails for a bad path, a wrong method, an undocumented body field, an
undocumented query key and an unresolvable path, so a green run is not vacuous. It asserts it checks at least 60 calls,
25 bodies and 15 query sets. Limits: static analysis of source text (template literals, local `const x = (id) => ...`
helpers, thin wrappers such as `postAnonymous`); it does not check response shapes, header names, or bodies built by a
function call (e.g. `credentialBody(params)`), and `client.ts` (transport) is pinned by one dedicated assertion.

## Pre-existing flakiness observed (not caused by this work)

Under CPU load a full `npm test` run intermittently times out in `AlertsPage.test.tsx`, `KillSwitchPage.test.tsx`,
`LaunchPanel.test.tsx`, `Wording.test.tsx` (default 5 s test timeout; the set varies run to run). The alerts, killswitch
and launch folders pass when run alone with these changes, and `src/features/launch` passes alone at `1983863`. The new test files widen their own async waits (`asyncUtilTimeout`, per-file
`testTimeout`) and no existing assertion was touched.

## Proposed registry text (for the orchestrator to place; this agent did not edit the registry)

> A10a staff-invite UI + OpenAPI contract test: IMPLEMENTED (tenant invitation form with step-up and refusal mapping,
> invitation-accept page + link routing, read-only staff directory, error boundaries, generic UI-to-OpenAPI contract
> test for every backoffice api module, 4 missing PUT operations documented). NOT IMPLEMENTED: platform_invite +
> four-eyes request console and suspend/offboard/role/MFA-reset/resend/revoke (ADR 0119 S8), b2c contract test (10
> probe findings recorded). Email delivery MOCK / PROVIDER DEPENDENT. Security review: APPROVE (a10a-security-review.md); Low items fixed in the follow-up commit.

## Probes for the security review

1. Token handling: load `/#/enrol?token=T&invite=1` -> address bar becomes `/accept-invitation` with no fragment; `T` not in
   `location`, `history.state`, `document.referrer` of later navigations, `localStorage`, `sessionStorage`, console, DOM.
   `/accept-invitation?token=T` (query) is ignored. An enrolment-reset token never reaches the accept endpoint and vice
   versa (module separation in `enrolLink.ts`).
2. After a 401 the token is dropped and the form is gone; after 400/429/503/network the token is kept for a retry and both
   password fields are cleared; the request body is exactly `{token, password}` with no `Authorization`.
3. No oracle: unknown / expired / used / revoked / issuer-not-live / not-invited all produce the same 401 and the same
   fixed text; server `message` is never rendered (sentinel tests).
4. XSS: staff email / display name rendered as text (test with `<img onerror>` / `<script>`); `otpauth` link allow-list
   unchanged; no `dangerouslySetInnerHTML` added.
5. Step-up: invite returns `step_up_required` -> one prompt -> one re-send with the same frozen body and the stepped-up
   token; cancel sends nothing more; a second refusal fails visibly, no loop; the TOTP code is not retained outside the
   dialog (existing StepUpProvider).
6. Authorization is server-side: forcing a role the UI did not offer, or opening the page as `support`, still gets the
   server's 403 and the UI degrades with a fixed message; `staffRead` / `staffAdmin` are display flags only.
7. Double-submit: one invitation per click burst (`useOnceGuard`); the staff directory refetches after success.
8. Error boundary: a throwing child shows only the fixed text; the message/stack is not rendered and the component does not log it; React itself logs caught errors to console.error (SR-A10a-1), so no Error message may embed a secret.
9. Spec edit: confirm the four added PUT operations match the Go handlers and permissions (`casino_catalogue:manage`,
   `casino_config:write`, `provider_config:write`) and step-up classes (`stepUpNone`, `stepUpCritical`).

## Security-review follow-up (SR-A10a-1..6)

- SR-A10a-1 fixed (comment + test wording; `console.error` by React acknowledged). SR-A10a-2 fixed: invitation-accept
  and `cmd/seed-admin` floors count runes. No other staff password-setting entry point exists (`enrol/redeem` sets no
  password). Player flows (`auth_routes.go`, `credential_handlers.go`, floor 8) still count bytes: NOT changed here
  (player surface, out of this slice); follow-up. SR-A10a-3 fixed (spec only). SR-A10a-4 fixed (wording).
  SR-A10a-6 fixed: `validEmail` rejects Cc/Cf and invalid UTF-8; `<bdi>` in the directory and invite banner.
  Display names are not validated server-side (only wrapped in `<bdi>`).
- RECORDED, not changed: SR-A10a-5 - an unspent invitation link is a bearer credential like a password until accepted;
  S8 should consider notifying the inviter on acceptance. A query-string token reaches host access logs: the static host
  must not log query strings for `/enrol` and `/accept-invitation` (added to
  `docs/runbooks/production-configuration-checklist.md`).

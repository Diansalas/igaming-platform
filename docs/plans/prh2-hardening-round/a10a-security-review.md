# A10a security review: staff invitation UI, invitation-accept page, staff directory, OpenAPI additions

Reviewer: `security` specialist. Branch `a10a-staff-invite`, commit `ea49194` on base `1983863`.
Verdict: **APPROVE, no blocking findings.** Six Low/Info items are listed below. None blocks this slice or launch.
SR-A10a-2 (the server password floor counts bytes) should be fixed before a production launch.

## Scope

In scope (code reading plus executed probes):

- `backoffice/src/auth/InvitationAcceptPage.tsx`
- `auth/enrolLink.ts`, `auth/staffAuthErrors.ts`
- `api/auth.ts` (`acceptStaffInvitation`), `api/tenants.ts`
- `features/tenants/InviteStaffForm.tsx`, `StaffDirectoryCard.tsx`, `staffErrors.ts`
- `components/ErrorBoundary.tsx` and where it is mounted
- `auth/permissions.ts` (`staffRead`)
- the 4 PUT operations and 3 schemas added to `docs/api/openapi/platform-api.yaml`
- `backoffice/src/test/openapiContract.ts`
- the new `yaml` devDependency and the lockfile change

I also read these server files to check that the server enforces what the UI assumes. They are not changed by this branch:

- `internal/httpserver/staff_invitation_accept.go`, `staff_lifecycle_routes.go`, `staff_auth_handlers.go` (redeem purposes)
- `internal/stafflifecycle/service.go`, `actions.go`, `model.go`
- `internal/auth/permission.go`
- `casino_routes.go`, `casino_admin_handlers.go`, `financial_routes.go`, `provider_capability_handlers.go`, `json.go`

Not in scope:

- Real-browser behaviour. Global history, bfcache, password managers and extensions were reasoned about, not driven in a browser.
- The pre-existing `EnrolWizard`, `StepUpProvider` and `apiFetch` internals. I read the re-send path but did not re-review it.
- Server timing measurement.
- Email transport. It is MOCK / PROVIDER DEPENDENT.
- The b2c app.
- Penetration testing. That needs an external, human-run engagement.

No database was needed. The OpenAPI check was done by comparing the spec with the Go route registrations and request structs.

## Evidence executed

- `npx vitest run` on these 5 files: `InvitationAcceptPage.test.tsx`, `InviteStaffForm.test.tsx`, `openapiContract.test.ts`, `permissions.test.ts`, `TenantForms.test.tsx`. Result: 108/108 pass.
- A temporary probe file, deleted afterwards and not committed, checked three things:
  - **Link parsing:** 11 URL shapes run through `captureEnrolLinkToken`.
  - **Hostile text in the directory:** `StaffDirectoryCard` given `<img onerror>`, `<script>`, `<b>`, U+202E and a 20,000-character email.
  - **Error boundary:** `ErrorBoundary` with a child that throws a secret-bearing error. This confirmed SR-A10a-1.
- npm registry metadata for `yaml@2.9.1`.

## Probe results

**1. Token leakage: PASS.**
- The fragment link `#/enrol?token=T&invite=1` puts `T` in the invitation slot only.
- `replaceState(null, '', '/accept-invitation')` runs in `main.tsx` before the router renders. After it, the token is not in the address bar or in `history.state`.
- The query form is never kept:
  - `/accept-invitation?token=T` and `/enrol?token=T&invite=1` capture nothing and strip the URL.
  - For `/login?token=T7#/enrol?token=T8&invite=1`, only the fragment token `T8` is kept and the query is stripped.
- `index.html` sets `<meta name="referrer" content="no-referrer">`.
- The token lives in module memory and React state only. It is dropped on success, on a dead-token answer, on unmount and for a signed-in visitor.
- No storage writes, `console.*`, `dangerouslySetInnerHTML` or analytics were added (grep).
- Copying the URL after load yields `/accept-invitation` with no token.
- Reload or forward navigation loses the in-memory token and shows the dead-link text (SR-A10a-4, UX only).
- Global browser history residue: see SR-A10a-5.

**2. Cross-use of invitation and enrolment tokens: PASS.**
- The client keeps two separate slots and two endpoints.
- The server is authoritative:
  - accept resolves only `store.EnrolInvite`;
  - `/enrol/redeem` accepts only `EnrolMFAReset` and `EnrolBootstrap` (`staff_auth_handlers.go:602`);
  - each purpose has its own MAC domain (`staffmfa/keys.go`).
- If an invitation token is forced into `#/enrol?token=` without `invite=1`, the client sends it to redeem and the server refuses it with a 401.

**3. No oracle: PASS.**
- The server sends every token failure as one `401 invalid_enrolment_token`. That covers unknown, unusable, issuer not live, account status and parse failure.
- The client maps that code to the single `INVITATION_DEAD` text and never renders server `message` text.
- `validation_error` is decided before any token lookup, so it reveals nothing about the token.
- Argon2 hashing runs before the lookup. After the lookup, paths differ by a few database round-trips, but reaching them needs a MAC-valid, existing token, so the difference is not a useful client-side signal.

**4. Password handling: client PASS, server Low (SR-A10a-2).**
- Client side:
  - The UI enforces 12 UTF-16 units.
  - Both fields are cleared after every failure and after success.
  - Both fields use `autocomplete="new-password"`.
  - Nothing is logged, and the password lives only in component state.
- Server side: a minimum of 12 is enforced, but in bytes (SR-A10a-2).
- Body size is capped at 1 MiB (`maxRequestBodyBytes`).

**5. XSS: PASS.**
- Email, display name, role, status and timestamps render as React text.
- The probe found no `img`, `script` or `b` element and no execution.
- Bidi override characters render verbatim (SR-A10a-6).
- The invite success banner and error banners use fixed, code-keyed text plus the request id.

**6. Step-up: PASS.**
- `apiFetch` re-sends the same `options` (frozen body) exactly once with `_stepUpRetried`.
- Cancel throws `step_up_cancelled` and sends nothing more.
- A second refusal is `stepUpNotEffectiveError`, so there is no loop. Tests cover all three cases.
- `useOnceGuard` plus `isPending` blocks a double submit.
- A retry after a network error is safe: the server's partial unique index `staff_users_live_email (tenant_id, email) WHERE status <> 'offboarded'` turns it into a `409 conflict`.

**7. Role gating is UX only: PASS.**
- The server enforces `RequirePermission(PermStaffManage | PermStaffRead)`.
- `pathTenant` + `canActOnTenant` return 404 for a foreign tenant. `inTargetScope` returns `ErrNotFound` for a cross-tenant actor, and RLS sits underneath.
- `Grantable` returns `role_not_grantable` for finance, risk_manager, promotions_manager and bonus_operations unless the caller is a platform admin. `PersonRequired` returns `person_required`.
- The UI `staffRead` set (platform_admin, tenant_admin, compliance) matches `permission.go` lines 778, 891 and 989.
- `TENANT_GRANTABLE_STAFF_ROLES` and `PERSON_REQUIRED_STAFF_ROLES` match `model.go`.

**8. ErrorBoundary: rendering PASS, logging Low (SR-A10a-1).**

**9. OpenAPI PUT operations: request side PASS, responses Low (SR-A10a-3).**
- Paths, methods, permissions and step-up classes match the Go route registrations:
  - `casino_catalogue:manage`, none
  - `casino_config:write` + tenant scope, critical
  - `casino_config:write` + tenant scope, none
  - `provider_config:write` + tenant scope, critical
- The request field names match the Go request structs (`DisallowUnknownFields`).

**10. Supply chain: PASS.**
- `yaml@2.9.1`: sole maintainer and publisher `eemeli` (the long-standing author), zero dependencies.
- No `preinstall`, `install` or `postinstall` script, and no `hasInstallScript` in the lock entry.
- The lock integrity matches the registry `sha512-3NxN8+…`.
- devDependency only. Caret range, pinned by the lockfile. Published 2026-09-11 and currently the latest version.
- The contract test only `parse()`s a repo file, with no custom tags.
- The `package.json` reorder of `@tanstack/react-query` is cosmetic.

## Findings

### SR-A10a-1 (Low): the "never logged" claim for ErrorBoundary is not true; React logs the caught error

- **What happens:** React 18 logs a caught render error with `console.error(error)`, even when an error boundary catches it. This happens in development and in production: `react-dom.production.min.js` contains `function Li(a,b){try{console.error(b.value)}…}`.
- **Repro:** render `<ErrorBoundary><Boom/></ErrorBoundary>` where `Boom` throws `new Error('SECRET-TOKEN-XYZ')`. The text is not rendered, but `console.error` receives the message. The probe confirmed this.
- **What is overstated:** the class comment, the record's probe 8 ("neither rendered nor logged"), and the existing test, which checks the DOM only.
- **Impact:** today, no thrown error in these paths carries a secret. The only one is `'unexpected challenge'`, and `ApiError` messages are server text. A future throw that embeds response data would reach the browser console, extensions, or an error reporter added later.
- **Fix:**
  - Correct the comment and the record so they say the boundary does not render or log, but React itself logs to the console.
  - Keep secrets and response bodies out of `Error` messages, which is the real control.
  - If a real reporter is ever added, redact there.

### SR-A10a-2 (Low): the server password floor counts bytes, not characters

- **What happens:** `staff_invitation_accept.go` checks `len(req.Password.reveal()) < 12`, which counts bytes. The spec (`InvitationAcceptRequest.password minLength: 12`, JSON Schema characters) and the UI ("at least 12 characters") both promise characters.
- **Repro:** a direct API caller sends `password: "密码密码"`. That is 4 characters and 12 bytes. The UI rejects it, but the server accepts it.
- **Impact:** a non-browser caller can set a weaker-than-documented password for a staff account. MFA enrolment still follows, so this alone does not take over an account. The server code is pre-existing and not part of this diff.
- **Fix:** use `utf8.RuneCountInString` (or count grapheme-insensitive runes). Add a test with multi-byte input. Apply the same check anywhere else `minStaffPasswordLen` is used.

### SR-A10a-3 (Low): the added OpenAPI operations under-document the error responses

The request side is correct. The response side has gaps a client could rely on wrongly:

- **`PUT /v1/admin/casino/games`:**
  - Missing `409 conflict`. `writeCasinoGovernanceError` returns it when the change is refused as `ErrDualControlRequired`, meaning a blocklist removal or disabled→active without an approved change request. This is the outcome that matters for security.
  - Also missing `404` and `500`.
- **The two capability PUTs:**
  - Missing `404` (no adapter for `providerID`).
  - Their `403` is documented only as `StepUpRequired`. A plain `403 forbidden` (missing permission, or a platform-scoped token refused by `RequireTenantScope`) is not documented.
  - Missing `500`.
- **`PUT .../availability`:** the spec marks `enabled` as `required`, but Go decodes into a plain `bool`, so `{}` is accepted as `enabled:false`. This fails closed (it disables), but the spec is stricter than the server.
- **`WriteProviderCapabilityRequest.amount_limits.items`:** missing `additionalProperties: false`. Go rejects unknown nested fields. Amounts have no `format: int64`.
- **Fix:** add these responses and constraints. Optionally make `enabled` a `*bool` and reject a missing value in a later Go change.

### SR-A10a-4 (Info, UX): after a reload, the page says the link is dead when it may still be valid

- **What happens:** after the scrub, a reload or a back/forward reload shows "invalid, has expired or was already used…" even though the token is unspent. The admin may be asked to re-invite for no reason.
- **Why it is still right on security:** not persisting the token is the correct trade-off.
- **Fix:** add wording such as "If you reloaded this page, open the link from your email again."

### SR-A10a-5 (Info, residual design risk): the invitation link is a bearer credential until it is accepted

- **What happens:**
  - `replaceState` replaces only the current session-history entry. The original URL with its fragment can stay in the browser's global history and in synced history; the code says so honestly.
  - The emailed link also persists in the mailbox.
  - Accepting needs only `{token, password}`, with no email (ADR 0119 design). Anyone who obtains an unspent link can activate the account and enrol their own authenticator.
- **Existing mitigations:** single use, 72 h TTL, an issuer-liveness check, audit of every rejection and redemption, and revoke/resend (ADR 0119 S8). The directory now shows the `active` transition.
- **Not affected:** mail-scanner prefetch cannot spend the token, because it needs a password POST.
- **Recommendation:** no change in this slice. For S8, consider a notification to the inviter on acceptance.

### SR-A10a-6 (Low): bidi control characters in staff email or display name allow visual spoofing

- **What happens:** the server's `validEmail` rejects only whitespace, so U+202E and other Unicode format characters (category `Cf`) are accepted. They render verbatim in the directory and in the invite success banner. For example, `admin‮moc.live@x.com` displays reversed. This is not XSS.
- **Fix (server, preferred):** reject `Cf` and control characters in the email at invite time.
- **Fix (UI, defence in depth):** wrap staff-controlled strings in `<bdi>` or use `unicode-bidi: isolate; direction: ltr`.

### Info notes (no action needed)

- `#/enrolXYZ?token=…` is captured as an enrolment token because of the `startsWith` prefix match. This is harmless: the server rejects it and the URL is still scrubbed.
- A query-string token, as in `/accept-invitation?token=…`, has already reached the static host's access log by the time the client discards it. Emailed links never use this form. Keeping query strings out of the SPA host logs for `/enrol` and `/accept-invitation` is a deployment hardening note.
- The accept form has no username field, so password managers cannot associate the new password with an account. This is UX only.

## Authorization and tenant-isolation tests the `qa` coverage must include

The Go suites own these; the UI suites cannot prove them.

1. A tenant A `tenant_admin` token on `GET /v1/admin/tenants/{B}/staff` and `POST /v1/admin/tenants/{B}/staff/invitations` returns 404 and never data. A `compliance` token on POST returns 403.
2. A `tenant_admin` inviting `finance`, `risk_manager`, `promotions_manager` or `bonus_operations` returns `403 role_not_grantable`, and no `staff_users` row or token is written.
3. An invitation token on `/v1/staff/auth/enrol/redeem`, and an MFA-reset token on `/v1/staff/invitations/accept`, both return 401 `invalid_enrolment_token` and spend nothing.
4. Unknown, expired, used, revoked, issuer-suspended and account-not-invited each return byte-identical 401 bodies, apart from `request_id`.
5. A multi-byte password under 12 characters returns 400 (after SR-A10a-2 is fixed).

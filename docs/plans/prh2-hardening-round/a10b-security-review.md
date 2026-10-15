# A10b security review: operator consoles S3, S5, S7, S8 (ADR 0114)

- **Reviewer:** `security` (independent review). **Date:** 2026-10-15.
- **Subject:** commit `5ec4838` on `a10b-operator-consoles` (base `1983863`). Backoffice only: `api/{paymentAttempts,
  reconciliation,capabilityGrants,forceResolutions,holdResolutions,idempotency}.ts`, `features/{consoles,payments,
  reconciliation,capability,resolutions}`, `auth/permissions.ts`, `layout/nav.ts`, `app/AppRoutes.tsx`,
  `lib/minorUnits.ts`, `test/{openapiLite,contractCapture,consoleFixtures}.ts`, `docs/plans/a10b-consoles-record.md`.
- **Method:** I read every new UI source file and checked the record's claims against code. I did not take the
  record's word for them. I then read the server side: `internal/httpserver/{capability_routes,
  payment_force_resolution_routes,withdrawal_hold_resolution_routes,payment_attempts_read_routes,
  staff_reconciliation_routes,staff_read_common,casino_routes,casino_reconciliation_handlers,admin_routes}.go`,
  `internal/payments/{manual_resolution,withdrawal_hold_resolution}.go`, migrations 0112, 0115, 0124 and 0125
  (payload-hash triggers, immutability, one-pending indexes, `UNIQUE (resolution_id, decided_by)`),
  `staff_identity_redaction.go`, and `internal/reconciliation/read_parse.go`.
- **Tests run (LOCAL, not CI):**
  - vitest over all new and changed backoffice tests: 17 files, 125 tests. The first full run had 2 failures,
    both in `ForceResolutionsPage.test.tsx` and both `findByRole` timeouts under parallel load. Re-run on its
    own, that file passed 19/19 (see F8).
  - Server-side integration tests on a PRIVATE database (`sr10b_20303`), plus the harness's own scratch
    databases. All passed: `TestCapabilityAPI_*` (10), `TestHSECRoutes_*` (4), `TestForceResolutionAPI_*` (5),
    `TestStaffRead*` (20, covering the R1/R2 authz matrix, cross-tenant, RLS backstop, shape exclusions,
    injected tokens and platform-read audit).
  - `sr10b_20303` was dropped afterwards. No role, password or shared infrastructure was touched.
- **Out of scope:** penetration testing, assistive-technology review, hosted CI, production deployment
  (same-origin proxy, CSP, headers), and the code-reviewer and ledger-finance reviews.

## Verdict

**ACCEPT WITH CONDITIONS for merge as `PARTIALLY IMPLEMENTED` against MOCK.** Nothing in this review blocks the merge:

- Every new route is authorized by the server. The UI gates are hints only.
- The server enforces tenant scope from the path and the token. It never uses a body or query value for it.
- Four-eyes, the payload hash and step-up are enforced in the database and handlers, not in the browser.
- No server string is rendered as HTML.
- There is no new dependency, and the test helpers do not ship.

**Conditions before S7/S8 are labelled `IMPLEMENTED`:**

- **C-1:** fix F1 (the K1 approver cannot identify the grantee).
- **C-2:** fix F3 (the decision dialogs do not show all the content the hash binds).
- **C-3:** register F2 / G7 (no exponent on K3 and HSEC amounts) as a tracked item.

**Launch flag:** F2 (G7) should block **production launch**, and any use with a non-MOCK provider or a
crypto/18-exponent asset, until the K3 and HSEC DTOs carry `exponent`. It does not block this MOCK-stage merge.

## Probe results

| # | Probe | Result | Evidence |
|---|---|---|---|
| 1 | Server enforces the permission behind each UI gate | **Holds** | Each route is wrapped in `staffRoute(..., auth.RequirePermission(...))`. K3 uses `payment_force_resolve:{request,approve,read}`. HSEC uses `withdrawal_hold_resolution:*`. K1 uses `capability_grant:*`, and `:approve` is platform_admin only both in code and in the 0112 catalogue. R1 uses `payment_force_resolve:read`. For R2, any one of three permissions opens the route, and the handler then checks the permission for the specific stream before it runs any query. The casino routes use `RequireTenantScope` + `casino_reconciliation:read`. Integration tests `A1_IneligibleRoleRefusedOnEveryRoute`, `HSECRoutes_TenantRolesDeniedEverywhere`, `T12_Matrix`, `R1_AuthzMatrix` and `R2_PermissionMatrixPerStream` pass |
| 1 | Tenant staff hand-edits `?tenant=` | **Ignored client-side; refused server-side** | `useConsoleTenant` takes the token's `tenant_id` for tenant principals. On the server, `canActOnTenant` gives 403 (K1/K3/HSEC) or 404 (R1/R2). `A1_CrossTenantRefused`, `R1_CrossTenant` and `R2_CrossTenantAndPlatformAudit` pass |
| 1 | Platform admin with a non-UUID `?tenant=` | **Never reaches a path** | `isUuid` gate. The server also runs `uuid.Parse` and returns 400 |
| 1 | Direct API calls without the UI | **Refused** | Same as above. The server never reads a tenant from a body (`tenant_id` is "accepted and IGNORED") |
| 2 | Server refuses a mismatched `payload_hash` | **Holds** | Triggers MR031 and HR031 (approval hash must equal the resolution hash). The actor proof also binds `payload_hash`. Tests: `stale payload hash` in `T14`, and `wrong payload hash` in the HSEC end-to-end test |
| 2 | Does the hash bind what the UI shows? | **Yes for the fields shown, but the dialog shows only part of them (F3)** | K3 binds tenant, attempt_id, kind, target_state, finding/basis/context, evidence_ref_hash, amount, asset_code, reason_code, attempt_state_at_submission and terminal_reason_at_submission. HSEC binds tenant, withdrawal id, kind, amount, asset, reason_code, evidence_ref_hash, withdrawal state and tenant/brand status. The payload is immutable after insert (LF-10 / MR030), so a resolution id always names the same content. The `liveHash !== frozenHash` block is defence in depth and cannot normally fire |
| 2 | Confirmation by typing the first 8 hash characters | **Adequate as friction only (F4)** | The required 8 characters are printed in the label. This does not prove the operator reviewed anything, and it is not a security control. The binding is server-side |
| 3 | Self-approval | **Refused** | DB distinct-Person rule; tests `self-approval` (K3, HSEC) |
| 3 | Approval without a K1 grant, or by a revoked grantee | **Refused** | The approval insert trigger requires an in-force grant. Before executing, the grants are re-locked FOR SHARE and the approvals recounted (`lockResolutionStaffAndGrants`). Covered by `T12_Matrix` and the HSEC grant test |
| 3 | tenant_admin approves K1 (R-7) | **Refused** | No `capability_grant:approve` in the role, and the DB blocks it as well. `A1_TenantAdmin_RequestAllowed_ApproveRejectRefused` passes |
| 3 | Platform-session K1 request for a tenant grantee | **409** | `I1_PlatformSessionCannotRequestForTenantGrantee` passes. The UI maps it and shows a hint |
| 4 | Step-up | **Holds** | The server refuses before any write (`staffRoute` step-up middleware). `apiFetch` re-sends once with the same options object, so the body and the Idempotency-Key are the same. A second refusal throws `stepUpNotEffectiveError`, so there is no loop. If the operator cancels the prompt, nothing is sent. Both paths have UI tests |
| 4 | G10: replay or double-click with the key ignored by the server | **Harmless** | Every repeat hits a state refusal: K1 one-pending index (`tenant, grantee, capability`), R-12 overlap check, not-pending (CG010), already revoked. K3/HSEC have one-pending-per-attempt/withdrawal indexes, `UNIQUE (resolution_id, decided_by)` and not-pending. The UI also has a synchronous once-guard. The UI does not claim server idempotency. The wording after a replay can mislead (F5) |
| 5 | Money display (G7) | **F2** | See below |
| 6 | Redaction | **Holds** | `staffViewer.ref` nulls platform actor ids for tenant viewers. The UI shows scope plus an 8-character prefix, never a name or email. R1 returns no provider reference, player, wallet or destination (`R1_ShapeExclusions`). K3 returns only `evidence_reference_present`, never the reference text. The UI never hashes (static pin). The staff picker shows display_name and role, which `staff:read` already exposes, and no email |
| 6 | Casino mismatch free text shown to tenant staff | **Acceptable, pre-existing (F7)** | It contains provider-controlled `provider_tx_id`, round ids and amounts. It is rendered as React text |
| 7 | XSS / injection | **No sink** | No `dangerouslySetInnerHTML` or `innerHTML`, enums go through `safeCode`, and `ApiError.message` is read only by the closed map (static pins plus hostile-string tests) |
| 8 | Reconciliation read-only | **Holds** | `api/reconciliation.ts` contains only GETs. The page imports only the list functions. A static pin forbids `useMutation` and non-GET methods on the page. The server has no mutation route |
| 9 | Tokens or ids in URLs | **Holds** | Browser URLs carry only `tenant`, `attempt` and `resolution` UUIDs. There are no tokens, reason or note text, or evidence hashes. Filters stay in component state and are not stored in history |
| 10 | openapiLite / dependencies | **Holds** | `package.json`, lockfile, `go.mod` and `go.sum` are unchanged. `test/*` is imported only by `*.test.*` files. `openapiLite` reads the spec through `node:fs` at test time |

## Findings

### F1 (Medium): the K1 approver cannot identify who is being granted authority

`GrantDecisionDialog` (`features/capability/CapabilityGrantsPage.tsx`) shows the grantee only as
`actorText(grantee_staff_id, grantee_scope)`, for example "Tenant staff (1a2b3c4d)", which is an 8-character id
prefix. The platform_admin approver therefore cannot see the name or role of the person who will be allowed to
request or approve money-moving resolutions, or check that this is the person intended.

The DB still enforces distinct-Person, no self-grant, role eligibility (R-9) and overlap. So this is not a
bypass, but it reduces the human second check to approving blind.

This combines with a request-side risk. In `GrantRequestDialog`, a non-empty "Or enter a staff id" field
silently overrides the dropdown selection. A leftover typed id therefore files a request for a different person
than the one selected, and the approver cannot detect it.

- **Repro:** as tenant_admin, select staff A, also type the UUID of eligible staff B, and file the request. As
  platform_admin, the dialog shows "Tenant staff (bbbbbbbb)", with nothing to compare against A.
- **Fix:**
  - In the approve and reject dialogs, show the full grantee id and resolve `display_name` and `role` from
    `GET /staff` for tenant grantees. Platform_admin has staff:read for the tenant. For platform grantees, show
    the full id.
  - In the request dialog, make the two inputs mutually exclusive, or show the resolved grantee (name, role,
    full id) before the operator confirms.

### F2 (Medium, launch-relevant): K3 and HSEC amounts have no exponent (G7)

The K3 and HSEC DTOs carry no `exponent`, so the tables and decision dialogs render, for example,
`150000 minor units of EUR (exponent unknown)`.

The executed amount cannot be wrong because of this. It is copied by the database from the attempt or
withdrawal and bound into the hash, and the approval count is computed by the server. What it affects is the
approver's judgement of materiality. A EUR value can be misread by 100x. An 18-exponent asset
(`1500000000000000000 minor units of ETH`) cannot realistically be read at all. HSEC labels the field "Amount
returned to the player", which is exactly the value a platform_admin is approving.

The UI does not guess: it never applies a default exponent. That is correct.

**Recommendation:** add `exponent` additively to `PaymentForceResolution` and `WithdrawalHoldResolution`,
resolved from the asset registry server-side as R1 already does. This is required before production launch,
or before any non-2-exponent or crypto asset is reachable. It is not required for this MOCK merge.

An interim UI mitigation is possible for K3: fetch the attempt (`getPaymentAttempt`, which has `exponent`) in
the decision dialog and show the formatted amount next to the raw value. Show it only when `asset_code`
matches.

### F3 (Low-Medium): the decision dialogs show only part of the content the hash binds

The checkbox in `DecisionDialog` reads "I reviewed the content above and the hash shown is the one I am
deciding on". The content above, however, is a subset of what the hash binds:

- **K3 summary:** kind, amount, target state, requester, approvals and expiry. It omits attempt id, operation,
  finding, basis and context codes, `evidence_ref_hash` and `reason_code`. These are visible in the detail panel,
  which is dimmed behind the modal.
- **HSEC summary:** omits `reason_code`, `evidence_ref_hash` and the tenant/brand status at submission.
- **Both:** the server-snapshot fields (attempt or withdrawal state at submission, terminal reason) are hashed
  but not in the DTO.

Approving an M2 "declare paid" without seeing the attempt id or operation in the dialog weakens the four-eyes
review.

**Fix:**

- List every hashed DTO field in `summary`. For K3, include attempt id with a link, operation, codes,
  evidence_ref_hash and reason_code.
- State that the hash also covers server-recorded snapshot fields that are not shown.
- For a platform principal, also show the tenant (F6).

### F4 (Low): the typed confirmations are self-referential

- **Decision dialogs:** the label prints the 8 characters the operator must type.
- **Request dialogs:** the required prefix is derived from the operator's own attempt or withdrawal id input,
  so it cannot catch a wrong id.

With a crafted link (`/force-resolutions?tenant=X&attempt=Y`), the attempt id is pre-filled and the label shows
its prefix. Mitigations: the K3 dialog shows an attempt preview (operation, reason, formatted amount), and the
request still needs a kind, basis, ack and reason plus a different approver.

HSEC has no preview, because R3 is absent (G9).

**Fix:**

- Present the 8-character confirmation as a deliberate-action step only, in comments and the record, not as
  verification.
- When R3 exists, show a withdrawal preview before the operator confirms.

### F5 (Low): "Nothing was changed" after a replay can mislead (G10 wording)

Scenario: the first approve commits, but its response is lost. The UI shows the correct "Nothing was confirmed".
The operator retries from the same dialog. The repeat gets 409 `force_resolve_conflict` (if the first one
executed) or a refusal from `UNIQUE (resolution_id, decided_by)`. The 403/409 texts in `errors.ts` end with
"Nothing was changed." That is true of the repeat, but the operator can read it as "my approval did not count".
The list refresh corrects the state.

**Fix:** after a `network_error` in the same dialog, word later refusals as "this retry changed nothing; your
earlier attempt may have been recorded - check the state below".

### F6 (Low): platform decision dialogs do not show the tenant

For platform_admin, the tenant comes from `?tenant=`. A crafted or stale link switches the context, and the
picker can show only "Tenant xxxxxxxx". The decision acts on a resolution id that is tenant-bound and whose
content is shown, so this cannot cause a wrong-tenant action. It is still a review-clarity gap.

**Fix:** show the tenant name and id in every decision and request dialog for platform principals.

### F7 (Info): casino reconciliation free text is shown to tenant staff

`reconciliation_key`, `expected_value` and `actual_value` from `/v1/admin/casino/reconciliation/mismatches`
contain provider-controlled text (`provider_tx_id=`, round ids). It is rendered as plain text, so there is no
XSS. The exposure is pre-existing (W2b) and limited to tenant-scoped roles with `casino_reconciliation:read`.

It is acceptable for operational data. A provider could, however, inject misleading wording. Consider applying
the R2 `TrustedPrefix` treatment, or visually marking the provider-controlled part.

### F8 (Info, test robustness): flaky timeouts in `ForceResolutionsPage.test.tsx` under load

In the full parallel run, 2 tests failed on `findByRole('status')` with the default 1 s timeout. The file passes
19/19 on its own. This is not a security defect, but a flaky four-eyes test erodes trust in the gate. Raise the
timeout for those assertions or wait for the network call.

### F9 (Info)

- The K3 reason pattern counts UTF-16 code units, while the server checks 1-64 **bytes**. A non-ASCII reason
  can pass the UI and then get a 400. This is a correctness issue, not a security issue.
- After the integration runs, three `cg_api_*` scratch databases were present on the local cluster. I could not
  attribute them; they may belong to concurrent agents. I did not drop them. The orchestrator may want to check
  for them.

## Required tests (authorization / tenant isolation), status

The server-side tests exist and pass locally (listed above). The UI suite covers: no network call when a role
is gated out; a tenant principal's path always uses the token tenant; 403/404/409 are rendered through the
closed map; a stale hash is never sent; double-click sends once; step-up re-sends once and a cancel sends
nothing; hostile strings render as text.

No new server test is required by this change, because no server code changed. When F2's DTO change is made,
add an R1-style test that `exponent` comes from the asset registry and is null for an unknown asset.

## What this review does not establish

It does not establish that the consoles are "secure" in general. It covers code-level and design-level review
of this diff, plus the server checks those consoles depend on, at this commit. It does not cover deployment
configuration, browser hardening headers, real providers, or any regulatory approval.

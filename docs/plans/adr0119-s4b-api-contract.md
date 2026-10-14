# ADR 0119 S4b - step-up API contract (for the S7b back-office agent)

Status: contract for slice S4b (backend). The normative source is `docs/api/openapi/platform-api.yaml` (tag
`staff-auth`, the `StepUpRequired` response and the `bearerAuth` description); this page summarises it for the
back-office work (S7b). If the two disagree, the OpenAPI file wins and this page is a defect. S4b and S7b merge to
`main` together (ADR 0119 section 16). Nothing here is a regulatory claim.

## 1. What changes for the back office

1. Some staff routes now answer **`403 step_up_required`** when the caller's last second-factor verification is
   older than the route's class allows. Nothing was written: the refusal happens before the handler runs (or, for
   the withdrawal policy, before any insert, inside a transaction that rolls back).
2. A new endpoint, **`POST /v1/staff/auth/step-up`**, verifies a fresh TOTP code and returns a **new access token**.
3. A new endpoint, **`POST /v1/staff/mfa/recovery-codes/regenerate`** (itself step-up `critical`), replaces the
   caller's recovery codes and returns the new ones once.

## 2. `403 step_up_required`

Body (the platform error envelope plus `max_age`):

```json
{"code": "step_up_required", "message": "a fresh second-factor verification is required", "request_id": "...", "max_age": 300}
```

Header (RFC 9470 section 3):

```text
WWW-Authenticate: Bearer realm="platform-api", error="insufficient_user_authentication", error_description="a fresh second-factor verification is required", max_age="300"
```

`max_age` is in seconds:

| Source | `max_age` (default) | Bound |
|---|---|---|
| route class `critical` | 300 | configurable 60-300 (`STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS`) |
| route class `financial` | 600 | configurable 60-600 (`STAFF_STEP_UP_FINANCIAL_MAX_AGE_SECONDS`) |
| withdrawal policy `require_step_up` on `POST /v1/admin/withdrawals/{id}/approve` (multi-approval withdrawals) | 120 | fixed (ADR 0119 section 9.3) |

Use `max_age` only to explain the prompt; never compute freshness client-side (the server clock and the sealed
session row decide).

The classes of every route are pinned in `internal/httpserver/staff_route_golden_test.go` (`staffRouteGolden`). Five
routes are classified per request:

| Route | Rule |
|---|---|
| `POST /v1/admin/tenants/{tenantID}/launch-authorisations` | `none` when `action` is `suspend` (the route then refuses it with 400 anyway: suspensions use their own routes); `critical` otherwise |
| `POST /v1/admin/tenants/{tenantID}/financial-policy-changes` | `none` when the caller holds only `financial_policy:tighten`; `critical` when the caller holds `financial_policy:author` |
| `POST /v1/admin/alerts/{alertID}/resolve` | `critical` when the alert's kind is in the pinned security / P1 list (every `staff.*` kind, `alerting.raise_failed` and every `p1` severity alert) or the alert cannot be read; `none` otherwise and for a caller the handler refuses anyway (no `alert:manage`, or a tenant session) |
| `POST /v1/admin/alerting/routes` | `none` for a caller the handler refuses anyway (no `alert:route_manage`, or a tenant session: the handler's audited `403 forbidden`); `critical` for every caller it admits (platform admin) |
| `POST /v1/admin/players/{id}/rg/limits` | `critical` for a removal (`value: null`); `none` for a value. A staff **increase** stays refused by the RG domain with `403 RG_STEP_UP_REQUIRED` whatever the step-up state (ADR 0115: staff increases are NOT enabled by S4b) |

Ordering: the step-up gate runs after the bearer, staff-principal, liveness and aal2 checks and **before the route's
own permission gate**. A caller who lacks the permission may therefore be prompted for a step-up first and then
receive `403 forbidden`. The UI must not treat a completed step-up as a promise that the action is allowed.

## 3. `POST /v1/staff/auth/step-up`

- **Auth:** `Authorization: Bearer <staff access token>` through the full staff chain (liveness, aal2). Rate bucket
  `mfa` (30 per IP per minute, shared with the login second-factor steps). Responses are `no-store`.
- **Never auto-retry or double-submit this request** (no automatic retry on timeout or network error, no second
  submit while one is in flight, disable the submit button until the response). Each duplicate submission of a valid
  code is a replay and counts as a FAILURE on the factor's counter, which is shared with login; five failures lock the
  factor and revoke the session (security review of S4b, finding 2). On a transport error, close the modal and let the
  user start again with a new code.
- **Body:** `{"code": "123456"}` - a 6-digit TOTP code. **Recovery codes are not accepted** (a staff member who lost
  the device signs out and uses the recovery flow of the login).
- **200:**

```json
{
  "access_token": "...", "token_type": "Bearer", "expires_in": 900,
  "mfa_at": "2026-10-13T10:00:00Z",
  "step_up_expires_at": {"financial": "2026-10-13T10:10:00Z", "critical": "2026-10-13T10:05:00Z"}
}
```

  - Replace the access token in `tokenStore` with `access_token`. **The refresh token does not change** (no
    `refresh_token` in the body). A later refresh carries the new `mfa_at` forward.
  - Only the token returned here carries the fresh step-up. An access token minted BEFORE the step-up (an older copy
    of the same session) stays un-stepped-up: the server uses the older of the token's `mfa_at` and the session's.
- **Errors (branch on `code`):**

| HTTP | `code` | Meaning | UI |
|---|---|---|---|
| 400 | `validation_error` | no code | keep the modal open |
| 401 | `invalid_mfa_code` | wrong, malformed or already-used code; one failure was counted against the factor (shared with login) | "The code is not valid." Let the user retry |
| 401 | `session_revoked` | the session is no longer live, the access token is not the session's newest (refresh first), or **five failed step-ups on this session within 15 minutes revoked the session** | sign out, back to login |
| 401 | `unauthorized` | missing / invalid / expired bearer token | refresh once, else sign out |
| 403 | `mfa_locked` | the factor is soft-locked (5 failures in 15 min) or hard-locked; nothing was compared | "Too many incorrect codes." Close the modal; the original action fails visibly |
| 403 | `forbidden` | not a staff token | sign out |
| 429 | `rate_limited` | `mfa` bucket; `Retry-After` | wait |
| 503 | `service_unavailable` | fault; nothing committed | "Try again" |

## 4. `POST /v1/staff/mfa/recovery-codes/regenerate`

- **Auth:** bearer, full staff chain, step-up `critical` (so the UI may first get `403 step_up_required`, run the
  modal and re-send once). No body. `no-store`; the response is `x-sensitive`.
- **200:** `{"recovery_codes": ["XXXXX-XXXXX", ... 10 ...]}`. Every earlier unconsumed code is revoked in the same
  transaction. Show the codes once, exactly like the enrolment screen (copy, download `.txt`, the "I have stored these
  codes" tick); never keep them in state or `localStorage`.
- **Errors:** `401 session_revoked`, `401 unauthorized`, `403 step_up_required`, `403 forbidden`, `503`.

## 5. The modal (S7b; ADR 0119 section 15 item 3)

1. On `403 step_up_required` from any staff call: open the modal (TOTP field), call `POST /v1/staff/auth/step-up`
   exactly once per code entered (never automatically retried, see section 3).
2. On `200`: swap the access token, then **re-send the original request exactly once**, with the same frozen body
   and the same `Idempotency-Key` header if it had one. Never retry more than once; never re-send after a second
   `step_up_required` (show the failure).
3. On cancel: the original action fails visibly.
4. The re-send is safe: a `step_up_required` refusal happened before any write. A route that then answers its own
   state-based `409` on a duplicate is behaving correctly.
5. Governed dialogs (ADR 0114: frozen `payload_hash`) work unchanged because the re-send uses the frozen payload.

## 6. Other server-side behaviour the UI can observe (rare)

- A governed final approval or withdrawal approval whose actor was suspended between the request starting and the
  transaction locking its rows answers **`401 session_revoked`** and writes nothing (ADR 0119 section 9.5).
- A step-up-classified operation whose actor proof is signed without a fresh step-up (a server defect, never a user
  error) answers `500 internal_error` and writes nothing (the signer backstop, ADR 0119 section 9.1).

## 7. Not in S4b

Lifecycle (suspend, invite, MFA reset) routes (S5/S6), the staff directory (S8), WebAuthn.

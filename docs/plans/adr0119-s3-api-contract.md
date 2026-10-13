# ADR 0119 S3 - staff MFA API contract (for the S7a back-office agent)

Status: contract for slice S3 (backend). The normative source is `docs/api/openapi/platform-api.yaml` (tag
`staff-auth`); this page summarises it for the back-office work (S7a). If the two disagree, the OpenAPI file wins and
this page is a defect. S3 and S7a merge to `main` together (ADR 0119 section 16).

Every endpoint below is unauthenticated (it establishes authentication) except `GET /v1/staff/me/mfa`. Every success
response of the `/v1/staff/auth/*` endpoints carries `Cache-Control: no-store` and `Pragma: no-cache`.

## 1. Flows

### 1.1 Normal login (enrolled staff member)

1. `POST /v1/staff/auth/login` `{tenant_slug?, email, password}` (omit `tenant_slug` for platform staff).
   - `200 {"challenge": {"type": "mfa", "token", "expires_in": 300, "methods": ["totp", "recovery_code"]}}`.
     **No token pair** (breaking change from before S3).
2. `POST /v1/staff/auth/mfa/verify` `{challenge_token, code}` (6 digits).
   - `200 TokenPair` (`access_token`, `refresh_token`, `token_type`, `expires_in`). The access token is aal2.

A challenge allows 5 wrong codes and lives 5 minutes. After it is exhausted or expired, restart at step 1.

### 1.2 Lost device (recovery code)

1. Login as 1.1 step 1.
2. `POST /v1/staff/auth/mfa/recover` `{challenge_token, recovery_code}`.
   - `200 {"challenge": {"type": "enrol", "token", "expires_in": 300, "methods": ["totp"]}}` - **not a session**.
3. Enrolment 1.3 with the `enrol` challenge.

### 1.3 Enrolment (from 1.2, or from an enrolment token)

0. (Token path only) `POST /v1/staff/auth/enrol/redeem` `{token, tenant_slug?, email, password}`
   - `200 {"challenge": {"type": "enrol", ...}}`. The token comes from the `cmd/seed-admin` bootstrap job (S3) or an
     emailed MFA-reset link (S6, not yet built). The back office route `/#/enrol?token=...` reads it. **Only the fragment form is supported** (S7a security review F2): the S6 emails and the `cmd/seed-admin` output must emit `/#/enrol?token=` and never `/enrol?token=`; the back office discards a query-form token and shows a fixed invalid-link page. The back-office origin must also send `Referrer-Policy: no-referrer` (cutover runbook precondition 5).
1. `POST /v1/staff/auth/enrol/start` `{challenge_token}`
   - `200 {factor_id, otpauth_uri, secret_base32, expires_in: 600}` (x-sensitive). Render `otpauth_uri` as a QR code
     **locally** (bundled library; never a network QR service). Show `secret_base32` behind a reveal toggle. Calling
     start again supersedes the previous pending factor; the challenge is not consumed.
2. `POST /v1/staff/auth/enrol/confirm` `{challenge_token, factor_id, code}`
   - `200 {recovery_codes: [10 x "XXXXX-XXXXX"], token_pair: TokenPair}` (x-sensitive). Show the codes once with copy
     and download (`.txt`); require the "I have stored these codes" tick before continuing. Never write them to
     `localStorage` or keep them in state after leaving the page. Every other session of the user is revoked.

### 1.4 Refresh and logout (unchanged endpoints, new staff rules)

- `POST /v1/auth/refresh` `{refresh_token}` -> `200 TokenPair`. Staff refresh tokens are **idle-limited to 30 minutes**
  (configurable 5-60) and the session dies **12 hours** after the second-factor login (configurable 1-12), whatever
  the activity. Then refresh answers `401` and the user logs in again (full 1.1).
- `POST /v1/auth/logout` `{refresh_token}` -> `204`.

### 1.5 My security

- `GET /v1/staff/me/mfa` (bearer) -> `{enrolled, factor_type, activated_at, recovery_codes_remaining, hard_locked,
  locked_until}`. Warn at 3 or fewer recovery codes. Regenerating recovery codes needs step-up and arrives in S4b.

## 2. Error codes (branch on `code`, never on `message`)

| HTTP | `code` | Where | Fixed UI message (suggested) |
|---|---|---|---|
| 401 | `unauthorized` | login | "Email or password is incorrect." |
| 403 | `forbidden` | login, verify, recover, enrol/* | "This account cannot log in. Contact your administrator." |
| 403 | `mfa_enrolment_required` | login | "Your account has no authenticator yet. Ask your administrator for an enrolment link." |
| 403 | `mfa_locked` | login, verify, recover, enrol/confirm | "Too many incorrect codes. Try again in 15 minutes, or ask your administrator to reset your authenticator." |
| 401 | `invalid_challenge` | verify, recover, enrol/start, enrol/confirm | "Your sign-in session expired. Please sign in again." (restart at login / redeem) |
| 401 | `invalid_mfa_code` | verify, recover, enrol/confirm | "That code is not valid." (one attempt was used) |
| 401 | `invalid_enrolment_token` | enrol/redeem | "This enrolment link is invalid or expired, or the details do not match." (every redeem refusal: unknown, used or expired token; wrong or unknown `tenant_slug`; wrong email or password; inactive account; a bootstrap link for an account that already has an authenticator) |
| 409 | `mfa_enrolment_expired` | enrol/confirm | "Setup took too long. Start again." (call enrol/start again with the same challenge) |
| 404 | `not_found` | login only | "Unknown operator." (bad `tenant_slug` at login; redeem never answers 404, see `invalid_enrolment_token`) |
| 429 | `rate_limited` | all | "Too many attempts. Wait a minute." (`Retry-After` header from the per-IP limiter; the per-account password lockout at login and redeem answers 429 without it) |
| 503 | `service_unavailable` | all (login included) | "Sign-in is temporarily unavailable. Try again." (every server-side fault; `500` only for an unexpected panic) |
| 400 | `validation_error` | all | form validation |

Notes:

- `mfa_locked` is the same answer whatever code was typed. A hard lock (10 failures in 24 hours), or a lost device
  with no recovery codes left, needs an administrative MFA reset (S6). Until S6 the only path is the owner procedure
  of `docs/runbooks/staff-mfa-s3-cutover.md` section 5.1: the database owner revokes the factor and its recovery
  codes by SQL in the staff member's scope, then runs `cmd/seed-admin -issue-bootstrap -operator -reason`; the staff
  member's next password step answers `403 mfa_enrolment_required` (and raises the expected P1
  `staff.mfa_integrity`), and they enrol through the token path of 1.3. The UI needs nothing extra for this: the
  `mfa_enrolment_required` message already says to ask for an enrolment link.
- `invalid_challenge` never says why (expired, used, exhausted, wrong purpose). The UI restarts the flow.
- `503` is the same whether the code was right or wrong (ADR 0119 gate G4). Nothing was committed.

## 3. Token and secret handling in the browser

- Challenge tokens, enrolment tokens, the TOTP secret, recovery codes, access and refresh tokens are secrets: keep
  them in memory only (never `localStorage` / `sessionStorage`, never in a URL except the one-time `/#/enrol?token=`
  link, which the page must strip from the address bar after reading), never log them, never send them to a
  third-party service.
- Access tokens carry new claims `sid`, `aal` (always 2), `amr` (`["pwd","otp"]`), `auth_time`, `mfa_at`, `sv`. The UI
  must not rely on them for authorization (the server is authoritative).

## 4. Not in S3 (do not build UI against these yet)

| Endpoint | Slice |
|---|---|
| `POST /v1/staff/auth/step-up` and the `403 step_up_required` modal | S4b (S7b) |
| `POST /v1/staff/mfa/recovery-codes/regenerate` | S4b (needs step-up `critical`) |
| `POST /v1/staff/invitations/accept`, `POST /v1/staff/auth/password/redeem` | S6 |
| `401 session_revoked` (liveness) | S4a |
| Staff directory and lifecycle screens | S8 |

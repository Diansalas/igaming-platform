# ADR 0119 - Staff MFA, step-up authentication and staff lifecycle

- **Status:** ACCEPTED - PENDING IMPLEMENTATION (2026-10-13). Design only: no code, no migration and no endpoint exists
  yet. Every mechanism below is `NOT IMPLEMENTED` until its slice (section 16) lands and passes its review gate. No
  owner question in section 18 blocks the design or slices S1-S9. Each policy value marked **[ORCH-DEFAULT]** is a
  reversible orchestrator/security default. It is **not owner-approved**, and the owner may change it without a
  redesign.
- **Supersedes in part:** ADR 0017 (architecture foundation, `NOT IMPLEMENTED`). This ADR closes ADR 0017's four "open
  decisions" with reversible defaults. Two ADR 0017 choices change, and the reasons are given in place: recovery codes
  use a keyed HMAC, not Argon2id (section 6.4); MFA policy is code, not a data row (section 4.1).
- **Classification:** class A, before technical production readiness. The staff MFA and STAFF-LIFECYCLE-1 rows of
  `docs/plans/platform-reconciliation-2026-10-13.md` (lines 56-57, 215, 229, 239). ADR 0115 S2 staff RG increases stay
  403 until S4 of this ADR lands.
- **Inputs (verified at `a3e56dd`):**
  - `internal/auth/{jwt,session,middleware,permission,password,keys}.go`
  - `internal/httpserver/{staff_routes,auth_routes,admin_routes,routes,ratelimit,withdrawal_handlers}.go`
  - `internal/identity/{staff_user,login_attempt}.go`, `internal/withdrawal/withdrawal.go:795-860`
  - `internal/secretstore`, `internal/actorproof`, `internal/payoutinstrument/keys.go` (key-family precedent)
  - migrations 0011 (`staff_users`), 0012/0018 (`sessions` and its RLS), 0029/0034 (Person link)
  - ADR 0099, 0101, 0110, 0112, 0114, 0115
  - `docs/governance/human-decision-register.md` (H1/T3), `docs/architecture/01-requirements-inventory.md`
- **Owners:** `security` (mechanism; author); `architect` (claims shape, schema, ADR 0110 extension); `identity-compliance`
  (lifecycle policy); `qa` (gates); `ledger-finance` (S4 only: the withdrawal and K-flow integration).
- **Not a regulatory claim.** These are software controls only. No licence or regulator has approved them.

## 1. Context: what exists today (verified)

| Area | Fact | Where |
|---|---|---|
| Staff login | Password only (Argon2id), and a lockout of 5 failures per 15 min per `tenant:email`. On success it returns a `TokenPair` straight away. There is no second factor. | `staff_routes.go:31-218`, `login_attempt.go` |
| Claims | `sub`, `tenant_id`, `role`, `principal_type`, `iat`, `exp`, `jti`. There is no `sid`, `aal`, `amr`, `auth_time` or version claim. HS256 with process-managed keys. | `jwt.go:69-115` |
| Access token | 15 min TTL. It is stateless: nothing re-checks staff status or role on each request. | `config.go:614`, `middleware.go:22-57` |
| Refresh | 30-day TTL, the same for players and staff. It rotates with reuse detection. On refresh, staff `status` and `role` are re-read. | `session.go`, `auth_routes.go:333-417` |
| Staff status | `active` or `suspended` (CHECK). No route can change it. | migration 0011 |
| Staff creation | `POST /v1/admin/tenants/{t}/staff`. **The admin chooses the password.** finance, risk_manager, promotions_manager and bonus_operations are restricted to platform_admin. platform_admin can only be created by `cmd/seed-admin`. | `admin_routes.go:515-650` |
| Role change, suspend, offboard | No route exists. | gap audit section 1, STAFF-LIFECYCLE-1 |
| Step-up | Only a fail-closed placeholder: `withdrawal.ErrStepUpRequired` returns 403 when the withdrawal policy sets `require_step_up`. | `withdrawal.go:857-860`, `withdrawal_handlers.go:814` |
| Actor proof | An HMAC proof, verified in the DB, on 13 governed tables. It is signed for whoever the server authenticated. | ADR 0110 |
| Identity-store residual | T3 is OPEN: an attacker can mint or take over `staff_users` rows, or forge `sessions` rows, by SQL and then receive genuine proofs. Hardening is DEFERRED to an owner decision (H1). | ADR 0110 sections 8, 10, 10a; decision register line 47 |
| Secret store | `internal/secretstore` resolves provider-credential refs in the `/provider-creds/` namespace. Platform key families such as `ACTOR_PROOF_KEYS` and `PAYOUT_INSTRUMENT_KEYS` reach the process as `config.SecretValue` env values, which the deployment secret store injects. | `secretstore.go`, `config.go:357-389` |

Consequences:

- One stolen staff password gives full staff authority.
- A suspended staff member keeps a working access token for up to 15 minutes.
- No governed path can disable a leaving or compromised staff member.

## 2. Decision summary

1. **Factor.** The base factor is TOTP (RFC 6238) plus one-time recovery codes. It sits behind an internal `StaffFactor`
   interface, so WebAuthn can be added later as another implementation. The design uses no external IdP and no vendor.
2. **MFA is mandatory for every staff role** [ORCH-DEFAULT]. The policy is a code constant, not a row. A staff login
   never yields an access token without a verified second factor.
3. **Secrets.**
   - Factor secrets are encrypted with AES-256-GCM, with AAD bound to the staff member, the tenant and the factor.
   - The key is a new `STAFF_MFA_KEYS` family that the deployment secret store delivers.
   - Recovery codes, login challenges and enrolment tokens are stored only as keyed HMACs.
   - None of these values is ever logged, rendered or audited.
4. **Session assurance.**
   - Staff tokens gain `sid`, `aal`, `amr`, `auth_time`, `mfa_at` and `sv`.
   - The matching `sessions` columns are sealed with a keyed HMAC, and a refresh with a missing or bad seal is refused.
   - A staff session lasts at most 12 h, with a 30 min idle limit [ORCH-DEFAULT].
5. **Liveness on every staff request.** One indexed query checks that the session is not revoked, that the staff member
   is `active` and that the `role` and `staff_version` match the token. If the check fails, the request is refused.
   Suspending or offboarding a staff member therefore takes effect on their next request, not 15 minutes later.
6. **Step-up.**
   - `RequireStepUp(class)` is applied per route. Section 8 is the complete classified list.
   - Max-age is 5 min for `critical` and 10 min for `financial` [ORCH-DEFAULT].
   - A stale or missing step-up gets 403 `step_up_required` with an RFC 9470 `WWW-Authenticate` challenge.
   - As a backstop, the actor-proof signer refuses to sign a step-up-classified operation unless there is a fresh
     step-up assertion for the same actor.
7. **Replay protection.** A used TOTP time-step is burned by a monotonic conditional UPDATE. No step at or below the
   last accepted step is accepted again.
8. **Brute force.**
   - Each factor allows 5 failures per 15 min, then a 15 min lock.
   - 10 failures in 24 h hard-lock the factor, raise a durable alert and need an administrative MFA reset.
   - A per-IP limiter bucket also applies.
9. **Lifecycle.**
   - States: `invited -> active <-> suspended -> offboarded`. `offboarded` is terminal. All changes are audited with
     before and after values, the actor and a reason code.
   - Suspend and offboard revoke every session, and offboard destroys credentials.
   - Granting a platform-staff role (and reactivating platform staff or resetting a platform staff member's MFA) needs
     four-eyes by distinct Persons. It is bound by a DB guard and an actor proof.
   - The DB keeps at least one active platform_admin (last-admin protection).
   - Invitations replace admin-chosen passwords.
10. **Break-glass: none in the application** [ORCH-DEFAULT]. The only recovery when no platform_admin can authenticate
    is the existing owner-role bootstrap (`cmd/seed-admin`, operator-run with database owner credentials). It is
    audited and alerted (section 11).
11. **New SQLSTATE class `SF`.** It does not collide with any code used today (section 13.6). Migrations 0137 (MFA) and
    0138 (lifecycle).

## 3. Threat model (what this defends, and what it does not)

| # | Threat | Defence in this ADR | Residual |
|---|---|---|---|
| M1 | Stolen or guessed staff password (phishing, reuse, stuffing) | Mandatory TOTP. Without the factor a password yields only a single-use 5 min challenge. | Real-time phishing proxy (AitM) that relays the TOTP. TOTP is not phishing-resistant, so WebAuthn is the follow-up (OQ-3). |
| M2 | Stolen access or refresh token (XSS, device theft) | Step-up on every high-risk route. The session is limited to 12 h absolute and 30 min idle. The liveness check honours revocation immediately. | Inside the step-up window (5 or 10 min) a token thief who also controls the browser can act. Bounded and audited. |
| M3 | TOTP replay (shoulder-surf, log, proxy) | Monotonic `last_used_step`; a code is valid only once per factor. | none |
| M4 | Online TOTP brute force | Per-factor lockout with a `FOR UPDATE` counter. A hard lock after 10 failures in 24 h. The challenge allows 5 attempts. | About 3e-5 success per hard-lock cycle (3 valid codes in 10^6, 10 tries). After a hard lock an admin must reset the factor. |
| M5 | Arbitrary SQL as `igaming_runtime` (THREAT-MODEL-ARBITRARY-SQL-1): plant a factor, recovery code, challenge or enrolment token; forge a sessions row with `aal=2`; delete a factor to force re-enrolment | AEAD with AAD that binds the factor; keyed HMACs for codes, challenges and tokens; a keyed assurance seal on sessions; MFA policy held in code. A deleted factor causes denial of service, never a bypass, because enrolment needs a keyed enrolment token. | See section 12, R1-R3. **T3 is narrowed for staff, not closed.** A SQL `UPDATE staff_users.role` on a staff member's own row is still accepted, unless the owner authorizes the authz seal in OQ-2. |
| M6 | Insider tenant_admin escalation (shell accounts, self-grant) | Invitations (the inviter never learns the credentials). The existing platform-only role list is kept. Self-target is refused. Platform roles need four-eyes. | A tenant_admin can still invite a tenant_admin who is an accomplice. That is a legitimate authority, and it is audited. |
| M7 | A leaving or compromised staff member keeps access | Suspend or offboard revokes sessions and bumps `staff_version`. Liveness refuses the next request. Governed writes re-check liveness `FOR SHARE` inside the transaction. | A request already committed before the suspension stays committed and audited. That is correct. |
| M8 | Lock-out of governance (all admins suspended or lost factors) | Last-admin guard; four-eyes MFA reset; owner-path bootstrap. | If the owner credentials are also lost, recovery is an operational matter outside this design. |
| M9 | Application-host compromise | Out of scope: the host holds the JWT, actor-proof and MFA keys. | Same as ADR 0110 T6. |
| M10 | Secret leakage through logs, audit, errors or a third-party QR service | Secret types redact. Enrolment responses are `no-store`. The QR code is rendered locally in the browser. A static and runtime log-scan test. | none known |

## 4. Factor architecture

### 4.1 Policy (code, not data)

- `auth.StaffMFARequired = true` is a constant, not a configuration value and not a row. ADR 0017 suggested a policy
  source of truth. We drop that: under M5 a row that says "MFA not required" would be a one-UPDATE bypass. A later
  per-role relaxation, if the owner ever wants one, would need a code change and an ADR.
- There is no enforcement flag and no "dev bypass" mode. Tests obtain MFA-level tokens through an `integration`-tagged
  helper, `internal/auth/authtest` (S3). A static test pins that only test code imports it, the same pin as
  `prooftest`.
- There is no legacy-enrolment grace window. Production has never run. When S3 lands, every existing dev or staging
  staff account is re-seeded or re-invited, and every existing staff session is revoked (section 13.1 step 5).

### 4.2 `StaffFactor` interface (`internal/staffmfa`)

```go
// FactorType is a closed vocabulary mirrored by a DB CHECK; adding a value needs a migration and an ADR amendment.
type FactorType string // "totp" now; "webauthn" reserved

type StaffFactor interface {
    Type() FactorType
    // Begin creates a PENDING factor and returns what the user needs to enrol it, exactly once.
    Begin(ctx context.Context, tx pgx.Tx, staff StaffRef, now time.Time) (FactorID uuid.UUID, Enrolment EnrolmentMaterial, err error)
    // Confirm activates a PENDING factor after a valid assertion; it revokes any previously active factor of the same type.
    Confirm(ctx context.Context, tx pgx.Tx, staff StaffRef, id uuid.UUID, a Assertion, now time.Time) error
    // Verify checks an assertion against the staff member's ACTIVE factor and atomically burns it (replay protection),
    // updating failure counters on refusal. It locks the factor row FOR UPDATE.
    Verify(ctx context.Context, tx pgx.Tx, staff StaffRef, a Assertion, now time.Time) (Result, error)
}
```

- `EnrolmentMaterial` and `Assertion` are redacting types: `String`, `GoString`, `Format`, `LogValue` and
  `MarshalJSON` all return `[REDACTED]`, as `secretstore.Secret` does. Only the enrolment HTTP response encoder may read
  the raw value, through an unexported accessor.
- `StaffRef` carries `{StaffID, TenantID (uuid.Nil for platform)}`, taken from the verified challenge or token. It is
  never taken from the request body.
- `amr` values follow RFC 8176: `pwd` (password) and `otp` (TOTP). WebAuthn will add `hwk` or `swk` plus `user`.
  `mfa` is added whenever two distinct factor classes are present.

### 4.3 TOTP parameters

| Parameter | Value | Reason |
|---|---|---|
| Algorithm | HMAC-SHA1, 6 digits, 30 s step, T0 = 0 | RFC 6238 defaults. Every authenticator app supports them. SHA-1 is fine inside HMAC. |
| Secret | 20 random bytes (160 bit), from `crypto/rand` | RFC 4226 section 4 recommends at least 128 bits and 160 bits by preference |
| Accepted window | steps `T-1, T, T+1` (about plus or minus 30 s of skew) | Usability. Each extra step adds 1e-6 per guess. |
| Replay rule | accept step `s` only if `s > last_used_step` | Monotonic. A used code, or any older code, is refused even inside the window. |
| Comparison | constant time (`subtle.ConstantTimeCompare`) across all three candidate steps. The loop never exits early. | Timing |
| Implementation | in-repo, about 60 lines, in `internal/staffmfa/totp`, tested against the RFC 6238 Appendix B vectors (SHA-1 rows) | Provider-independent. The only dependency is the Go standard library. |
| `otpauth://` URI | `otpauth://totp/{issuer}:{email}?secret=...&issuer={issuer}&algorithm=SHA1&digits=6&period=30`. `{issuer}` is a platform configuration value (`STAFF_MFA_ISSUER`, default `iGaming Platform Back Office`), never a brand name. | The authenticator label |

## 5. Keys and storage of secrets ("encrypted at rest via the secret store")

### 5.1 Key family

- **Config.** `STAFF_MFA_KEYS` (`kid:base64(secret)`, comma-separated) and `STAFF_MFA_ACTIVE_KID`, as
  `config.SecretValue`. The deployment secret store delivers them exactly as it delivers `ACTOR_PROOF_KEYS` and
  `PAYOUT_INSTRUMENT_KEYS` (ADR 0110 section 6, ADR 0111 section 2.2).
- **`config.Load` refuses:**
  - malformed entries;
  - any key under 32 decoded bytes;
  - an unknown active kid;
  - a key equal to either JWT secret, any actor-proof key, any payout-instrument key, or the provider-credential
    fingerprint key;
  - outside an explicit development environment, the published `.env.example` placeholder.
- **Why not `internal/secretstore.Fetcher`.** That package resolves per-tenant provider-credential handles. Its refs
  must sit in the `/provider-creds/` namespace (`secretstore.go:113-152`), and its circuit breaker and negative cache
  are tuned for outbound provider calls. A platform-wide key that every staff login needs belongs with the other
  platform key families, loaded once at startup. "At rest via the secret store" means this: the key material exists
  only in the deployment secret store and in process memory. The database holds only ciphertext and keyed MACs.
- **Startup gate.** `staffmfa.VerifyStartup` runs from `cmd/platform-api/main.go`. In every environment it refuses to
  start without a valid family, because staff login cannot work without one. Unlike the actor proof, no environment
  "fails closed at the DB": with no key, there is no staff login.

### 5.2 Subkeys (HKDF-SHA256, the same construction as `payoutinstrument/keys.go:107-117`)

| Label | Use |
|---|---|
| `staff-mfa-factor-aead-v1` | AES-256-GCM key for factor secrets |
| `staff-mfa-recovery-v1` | HMAC key for recovery-code hashes |
| `staff-auth-challenge-v1` | HMAC key for login and enrolment challenge-token hashes |
| `staff-enrolment-token-v1` | HMAC key for invite, enrolment and MFA-reset token hashes |
| `staff-session-assurance-v1` | HMAC key for the sessions assurance seal |

### 5.3 Formats

- **Factor secret.** `secret_ct = GCM.Seal(nonce(12 random bytes), secret, AAD)` with
  `AAD = "sf-v1|" || factor_id || "|" || staff_user_id || "|" || coalesce(tenant_id,'platform') || "|" || factor_type`.
  The row stores `secret_kid`, `secret_nonce` and `secret_ct`. If ciphertext is moved to another row, or written by
  SQL, `Open` fails. That failure counts as a verification failure and also raises the durable alert
  `staff_mfa_integrity` (P1). The factor is never treated as "absent".
- **Keyed hashes.** `HMAC(subkey(kid), purpose || "|" || staff_user_id || "|" || value)`. The row stores the kid and
  the 32-byte MAC. Every lookup is by MAC, so a lookup of a planted row (any value, but no valid key) finds nothing.
- **Rotation.**
  1. Add the new kid to the family and make it active.
  2. Rewrap the AEAD rows with the job `staffmfa-rewrap`. It is idempotent, runs per row and is audited as
     `staff.mfa_rewrapped` with counts only.
  3. Challenges, enrolment tokens and seals have short lifetimes. They age out under the old kid, which stays
     verify-only for at most 24 h.
  4. Recovery codes are one-way, so their old kid stays verify-only until every code under it is consumed or
     regenerated. The runbook (S9) adds a regeneration campaign and a hard retirement date.
  5. A retired kid is refused.

### 5.4 Never logged

- Factor secrets, the `otpauth` URI, TOTP codes, recovery codes, challenge tokens and enrolment tokens are request or
  response values only.
- The request logger must not log bodies of `/v1/staff/auth/*`, `/v1/staff/invitations/*` or `/v1/staff/mfa/*`.
  Today it logs no bodies at all. A test pins that this stays true for these routes.
- Audit `Metadata` never holds them. A static test scans `audit.Record` call sites in `internal/staffmfa`,
  `internal/stafflifecycle` and the new handlers and rejects any metadata key in `{secret, code, token, uri,
  recovery}`.
- Each enrolment, recovery-code and token response carries `Cache-Control: no-store` and `Pragma: no-cache`.

## 6. Flows

### 6.1 Login (replaces the current single-step `POST /v1/staff/auth/login`)

1. **Password step** (`POST /v1/staff/auth/login`). The existing password and lockout logic stays as it is, including
   the dummy-hash timing equalisation, the scope check and the status check. A NULL `password_hash` (an `invited` or
   `offboarded` row, section 10) is verified against `DummyPasswordHash` and fails as `invalid_credentials`. On a
   correct password with status `active`:
   - If the staff member has an ACTIVE factor that is not hard-locked: insert a `staff_auth_challenges` row with
     `purpose = 'mfa'`, a 5 min TTL and `attempts_left = 5`, and return `200 {"challenge": {"type": "mfa", "token",
     "expires_in": 300, "methods": ["totp", "recovery_code"]}}`. **No `TokenPair`.**
   - If there is no active factor: return `403 mfa_enrolment_required`. The UI then explains that an enrolment link
     from an administrator is needed (section 6.3). A password alone **never** opens enrolment.
   - If the factor is hard-locked: return `403 mfa_locked`, which is also audited.
   - Audit: `staff.login_password_verified` (it replaces `staff.login_succeeded`, which now means a full aal2 login).
2. **Second-factor step** (`POST /v1/staff/auth/mfa/verify` with `{challenge_token, code}`):
   - **Lock the challenge.** Look up the challenge by keyed MAC and lock it `FOR UPDATE`. It must be unexpired,
     unconsumed, `purpose = 'mfa'` and `attempts_left > 0`.
   - **Re-check the staff member.** Re-read the staff row: `active`, and the same `staff_version` as when the challenge
     was issued.
   - **Verify.** Call `StaffFactor.Verify`.
   - **On success.** Consume the challenge, then call `IssueSession` with `aal = 2`, `amr = ["pwd","otp"]`,
     `auth_time = now`, `mfa_at = now`, `absolute_expires_at = now + 12h` and a seal. Return a `TokenPair` and audit
     `staff.login_succeeded` with `amr`.
   - **On failure.** Decrement `attempts_left` and update the factor counters (section 6.5). Audit
     `staff.mfa_verification_failed`. Return 401 `invalid_mfa_code`. When the challenge is exhausted it is consumed.
   - **Commit.** The failure writes commit through the same "record then return the outcome" pattern as
     `staff_routes.go:89-198`, so a refused attempt is never rolled back.
3. **Recovery-code path** (`POST /v1/staff/auth/mfa/recover` with `{challenge_token, recovery_code}`). The challenge
   rules are the same.
   - The recovery code is looked up by keyed MAC among the staff member's unconsumed codes and consumed with
     `UPDATE ... WHERE consumed_at IS NULL` (`RowsAffected = 1`).
   - On success the response is **not** a session. The current challenge is consumed, and a new challenge with
     `purpose = 'enrol'` is returned: a recovery code buys a re-enrolment, never an aal2 session.
   - The old active TOTP factor is revoked when the new one is confirmed, not before. A recovery attempt by an attacker
     who has already burned a code therefore does not leave the victim without a factor.
   - Every other session of the staff member is revoked at confirmation.
   - Audit `staff.mfa_recovery_used` and raise the durable alert `staff_mfa_recovery_used` (info severity, routed to the
     staff member's own scope admins and to platform security).

### 6.2 Enrolment (needs a `purpose = 'enrol'` challenge; reached only through 6.1.3 or 6.3)

- `POST /v1/staff/auth/enrol/start` with `{challenge_token}`:
  - creates a `pending` factor (TTL 10 min);
  - returns `{factor_id, otpauth_uri, secret_base32}` exactly once, `no-store`;
  - does **not** consume the challenge;
  - each call revokes any earlier `pending` factor of the same staff member.
- `POST /v1/staff/auth/enrol/confirm` with `{challenge_token, factor_id, code}`:
  - verifies the code against the pending factor, using the same replay rule and the same counters, which live on the
    pending row;
  - then, in one transaction:
    1. activate it, setting `last_used_step` to the step it accepted;
    2. revoke the previous active factor of the same type;
    3. revoke and replace all recovery codes with 10 new ones;
    4. consume the challenge;
    5. revoke all other sessions;
    6. issue an aal2 session;
  - returns `{recovery_codes: [10], token_pair}` once, `no-store`;
  - audit `staff.mfa_enrolled` (the metadata holds `factor_type`, the replaced factor id and `via ∈ {invite,
    reset_token, recovery_code, bootstrap}`).
- **Recovery code format.** 10 codes. Each is 10 characters of Crockford base32 (50 bits), displayed as
  `XXXXX-XXXXX`. Input is normalised: upper case, hyphens and spaces removed, I and L read as 1, O read as 0.
- **Regenerating codes.** `POST /v1/staff/mfa/recovery-codes/regenerate` (step-up `critical`) replaces all codes and
  returns the new ones once.

### 6.3 Enrolment tokens (invite, MFA reset, bootstrap)

- **Purposes.** A `staff_enrolment_tokens` row has `purpose ∈ {invite, mfa_reset, bootstrap}`. The token is 32 random
  bytes, base64url.
- **TTL** [ORCH-DEFAULT]: invite 72 h, mfa_reset 24 h, bootstrap 24 h.
- **One live token.** Issuing a new token for the same `(staff, purpose)` revokes the live one.
- **Delivery.**
  - invite and mfa_reset: by email to the staff member's address, through `internal/email.Provider`. MOCK today. A real
    adapter is PROVIDER DEPENDENT (OQ-1).
  - bootstrap: printed once by `cmd/seed-admin` on the operator's terminal.
  - **The token is never returned in an HTTP response to the inviter or the resetter.** This removes the "admin knows
    the shell account's credentials" weakness of today's creation route (ADR 0024 residual).
- **Redemption.**
  - `POST /v1/staff/invitations/accept` with `{token, password}` (for an invite: it sets the password and moves
    `invited -> active`).
  - `POST /v1/staff/auth/enrol/redeem` with `{token, tenant_slug?, email, password}` (for mfa_reset and bootstrap: it
    needs the current password as well).
  - Both consume the token and return a `purpose = 'enrol'` challenge.
  - Wrong-token attempts share the per-IP `credential` bucket and are audited.
  - A redemption for an account that is not `invited` (for an invite) or not `active` (for the others) is refused.

### 6.4 Why recovery codes use a keyed HMAC and not Argon2id (changes ADR 0017)

1. **Planting resistance under M5.** A SQL-written Argon2 hash of a code the attacker chose would work. A keyed HMAC
   needs a key that the runtime role never sees.
2. **No denial-of-service lever.** Verifying a code against 10 Argon2id (64 MiB) hashes per attempt would turn the
   recover endpoint into a memory-exhaustion lever, the same concern as `ratelimit.go:18-25`.
3. **Offline cracking needs the key.** With 50 bits per code, an offline attack requires the key. If the application
   host is compromised, the factor secrets go with it (M9) anyway.

### 6.5 Rate limiting and lockout

| Control | Value [ORCH-DEFAULT] | Mechanism |
|---|---|---|
| Challenge attempts | 5 per challenge | `attempts_left`, decremented under `FOR UPDATE` |
| Factor soft lock | 5 failures within 15 min sets `locked_until = now + 15 min` | `failed_count` and `failed_window_start` on the factor row, updated under `FOR UPDATE` (this serialises parallel guesses) |
| Factor hard lock | 10 failures within 24 h sets `hard_locked_at`. A reset is required (section 10.5). | `failed_24h_count` and `failed_24h_start`. Durable alert `staff_mfa_hard_locked` (P2). Its audit marks it as a compromise signal: the password was correct. |
| Recovery-code failures | count against the same factor counters | one budget for every second-factor guess |
| Step-up failures | count against the same factor counters, and 5 failures also revoke the current session | a token thief cannot grind TOTP from inside a session |
| Per-IP | new bucket `mfa`, 30 per IP per minute, over `/v1/staff/auth/mfa/*`, `/enrol/*` and `/step-up`. The existing `credential` bucket (20 per min) covers `/invitations/accept` and `/enrol/redeem`. | `ratelimit.go` (single process; the same caveats apply) |
| Password lockout | unchanged (5 per 15 min per identifier) | `login_attempts` |

A locked factor answers `403 mfa_locked` **before** any code comparison. The response is the same whether the code
would have been right or wrong.

### 6.6 Step-up (`POST /v1/staff/auth/step-up` with `{code}`)

- **Who may call.** It needs a bearer access token that passes the full staff chain (section 7.3) with `aal = 2`.
- **What it does.** It verifies the code against the caller's active factor (replay rule, counters). It updates the
  session row's `mfa_at` and seal, then mints a **new access token** with `mfa_at = now`. The refresh token does not
  change.
- **Audit.** `staff.step_up_verified`, or `staff.step_up_failed` on failure.
- **Recovery codes are not accepted for step-up.** A staff member who has lost the device recovers through 6.1.3.
- **The browser flow.** The back office shows a modal, calls step-up, swaps the access token and retries the original
  request once with the **same** `Idempotency-Key` (section 15.3).

## 7. Session assurance and liveness

### 7.1 Claims (staff tokens only; player tokens unchanged)

| Claim | Meaning |
|---|---|
| `sid` | the session id (a `sessions.id` UUID). This is new for staff. |
| `aal` | `2` for every staff access token after S3. Any other value is refused by `RequireAAL2`. |
| `amr` | RFC 8176 values, for example `["pwd","otp"]` |
| `auth_time` | when the second factor at login was verified (Unix seconds). It is carried unchanged through rotation. |
| `mfa_at` | the most recent second-factor verification (login or step-up), in Unix seconds |
| `sv` | `staff_users.staff_version` at issue time |

The claims are signed by the existing JWT key registry. `Verify` adds two checks: a staff token must carry every claim
in this table, and its `aal` must be in `{2}`.

### 7.2 `sessions` columns and seal (migration 0137)

- **New nullable columns:** `aal SMALLINT`, `amr TEXT[]`, `auth_time TIMESTAMPTZ`, `mfa_at TIMESTAMPTZ`,
  `absolute_expires_at TIMESTAMPTZ`, `assurance_kid TEXT`, `assurance_seal BYTEA`.
- **The seal.**
  `seal = HMAC(subkey, "ss-v1|" || id || "|" || principal_type || "|" || principal_id || "|" || coalesce(tenant_id,'platform') || "|" || aal || "|" || join(amr,',') || "|" || auth_time || "|" || mfa_at || "|" || absolute_expires_at)`.
  Times are microsecond UTC integers.
- **On refresh of a staff session, `RotateSession` must:**
  - **verify the seal**, refusing a missing or bad one as 401 `invalid or expired refresh token`, with an audit row
    `auth.session_assurance_invalid` and the durable alert `staff_session_integrity` (P1), because this shape only
    arises from SQL tampering;
  - **refuse** when `now > absolute_expires_at`;
  - **copy** `aal`, `amr`, `auth_time`, `mfa_at` and `absolute_expires_at` to the new row and **re-seal** it, because
    the session id changes;
  - **use the staff idle TTL** for `expires_at`: `STAFF_REFRESH_IDLE_TTL`, default 30 min [ORCH-DEFAULT], capped at
    `absolute_expires_at`.
- **Players** keep `RefreshTokenTTL` and no seal.
- **The trigger `sessions_staff_assurance_guard` (`SF040`).** For `principal_type = 'staff'`, an INSERT must carry
  non-NULL `aal`, `auth_time`, `absolute_expires_at` and `assurance_seal`. `auth_time` and `absolute_expires_at` are
  immutable on UPDATE. The trigger cannot check the HMAC (the DB holds no key), so the seal check stays in Go. The
  trigger catches application bugs; the seal defeats SQL forgery.

### 7.3 Staff route chain (one helper; no route bypasses it)

`staffRoute(perm, stepUp, h)` composes:

```text
auth.Middleware -> auth.RequireStaffPrincipal -> RequireLiveStaffSession -> auth.RequireAAL2 ->
auth.RequirePermission(perm) [-> auth.RequireTenantScope where the route needs it] -> auth.RequireStepUp(class) -> h
```

`RequireLiveStaffSession` (in `internal/httpserver`, because it needs the DB):

- **The query.** It runs one query in the token's own scope (`WithTenant(tid)` or `WithoutTenant`) with
  `app.principal_id = sub`, which reuses migration 0018's `session_select_own_principal` policy:
  `SELECT s.revoked_at, s.expires_at, s.absolute_expires_at, su.status, su.role, su.staff_version FROM sessions s JOIN
  staff_users su ON su.id = s.principal_id WHERE s.id = $sid AND s.principal_id = $sub`.
- **It refuses with 401 `session_revoked`** when:
  - no row is found;
  - the session is revoked, or past `absolute_expires_at`;
  - the staff member is not `active`;
  - `role` differs from the token's `role`, or `staff_version` differs from `sv`.
- It does **not** refuse on the idle `expires_at`. That governs refresh only; the 15 min access token is the idle unit.
- On a DB error it returns 503 (fail closed).
- **Cost:** one indexed lookup per staff request. Staff traffic is low (back office).

`RequireStepUp(class)` reads `mfa_at` from the claims. If `now - mfa_at > maxAge(class)`, or `mfa_at` is absent, it
returns `403` with `{"error": "step_up_required", "max_age": N}` and the header `WWW-Authenticate: Bearer
error="insufficient_user_authentication", max_age="N"` (RFC 9470 section 3). Otherwise it places
`auth.StepUpAssertion{Subject, SessionID, MFAAt}` in the context for the signer backstop (section 9.1). A negative age
beyond 5 s of clock skew is also refused.

**Static completeness test.**

- **Coverage.** Every route under `/v1/admin/`, plus `/v1/staff/mfa/*` and `/v1/staff/auth/step-up`, is registered
  through `staffRoute` (AST scan of `mux.Handle` calls, with a planted-offender non-vacuity case).
- **Classification.** Every mutating method (POST, PUT, PATCH, DELETE) carries an explicit `stepUp` value from the
  closed set `{none, financial, critical}`. A missing classification does not compile, because the argument is
  required.
- **Pinning.** A golden table of route to class (section 8) is pinned by a test, so changing a class is a reviewed diff.

### 7.4 Max-ages [ORCH-DEFAULT]

| Item | Default | Bound enforced in `config.Load` |
|---|---|---|
| Staff access token TTL | 15 min (unchanged) | 1-15 min |
| Staff refresh idle TTL | 30 min | 5-60 min |
| Staff session absolute (aal2 lifetime) | 12 h from `auth_time` | 1-12 h |
| Step-up `critical` | 5 min | 1-5 min |
| Step-up `financial` | 10 min | 1-10 min |
| Login challenge | 5 min, 5 attempts | fixed |
| Pending factor | 10 min | fixed |

The configuration can only tighten these values. A value above the bound refuses startup.

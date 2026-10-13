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

## 8. Which operations require step-up (enumerated against the permission matrix) [ORCH-DEFAULT]

### 8.1 Rule

A staff write requires step-up when it does one of these:

- **(a)** moves, releases or creates value: money, bonus value, or payout dispatch;
- **(b)** changes who can do what: staff, roles, MFA, capability grants;
- **(c)** creates, approves or rotates a secret or provider credential;
- **(d)** initiates, approves or executes a four-eyes or governed request;
- **(e)** loosens a financial, RG, risk, regulatory or availability control, or opens a brand, tenant or market;
- **(f)** silences or redirects security signals (alert routing).

Classes:

- `critical` (5 min) for (b), (c), (f) and the governance or policy parts of (d) and (e).
- `financial` (10 min) for (a) and money-decision four-eyes. These decisions come in queues, so the longer window
  reduces re-prompting without reaching the identity-changing operations.

**Not step-up (aal2 is enough):**

- **Protective, fail-safe actions:** engage a kill switch, suspend a brand or tenant, suspend a player, add an RG
  restriction, reject or cancel a governed request, reject a withdrawal. The justification:
  - Requiring a fresh factor during an incident slows the defenders.
  - These actions move the platform toward safety.
  - The reverse actions are themselves step-up and four-eyes.
  - Each one is audited and, where it applies, alerted.
- **Routine reads and low-impact configuration:** casino game availability, catalogue edits, brand creation (a brand
  is created `pending_launch` and cannot trade), display names, alert ack/resolve, KYC document review.

### 8.2 Route classification (the golden table pinned by the S4 test)

| Route (method path) | Permission | Class | Rule and justification |
|---|---|---|---|
| `POST /v1/admin/withdrawals/{id}/approve` | `withdrawal:approve` | financial | (a)(d): it releases player funds. This **supersedes** the `withdrawal.ErrStepUpRequired` placeholder (section 9.3). |
| `POST /v1/admin/withdrawals/{id}/submit` | `withdrawal:submit` | financial | (a): it dispatches a payout to the provider |
| `POST /v1/admin/withdrawals/{id}/resolve` | `withdrawal:submit` | financial | (a): manual outcome of a submitted payout |
| `POST /v1/admin/withdrawals/{id}/reject` | `withdrawal:reject` | none | protective |
| `POST .../manual-adjustments` | `ledger_adjustment:initiate` | financial | (a)(d): K2 initiation. Both four-eyes Persons must be strongly authenticated, or a stolen initiator session plus a rubber-stamp approval defeats four-eyes. |
| `POST .../manual-adjustments/{id}/approve` | `ledger_adjustment:approve` | financial | (a)(d): K2 approval and execution |
| `POST .../manual-adjustments/{id}/reject\|cancel` | | none | protective |
| `POST .../payment-force-resolutions` and `/{id}/approve` | `payment_force_resolve:*` | financial | (a)(d): K3 |
| `POST .../payment-force-resolutions/{id}/reject\|cancel` | | none | protective |
| `POST .../withdrawal-hold-resolutions` and `/{id}/approve` | `withdrawal_hold_resolution:*` | financial | (a)(d): HSEC release |
| `POST .../withdrawal-hold-resolutions/{id}/reject\|cancel` | | none | protective |
| `POST .../payments/kill-switches` (engage) | `payments_kill_switch:engage` | none | protective. It is deliberately fast for incident response. |
| `POST .../kill-switches/{id}/release-requests` and `.../release-requests/{id}/approve` | `payments_kill_switch:release` | critical | (d)(e): it re-opens payments. Four-eyes. |
| `POST .../release-requests/{id}/cancel` | | none | protective |
| `POST .../launch-authorisations` (activate, reactivate, ratify, close) | `launch_authorisation:request` | critical | (d)(e): it opens or changes market availability |
| `POST .../launch-authorisations` with `action = suspend`, `POST .../brands/{b}/suspend`, `POST .../tenants/{t}/suspend` | `launch_authorisation:suspend` | none | protective (ADR 0112 section 6.3 asymmetry). The handler classifies by `action`. The class is resolved from the validated body **before** the handler runs, through a declared per-route classifier function. That function is pinned by test and is not free-form. |
| `POST .../launch-authorisations/{id}/approve` | `launch_authorisation:approve` | critical | (d)(e) |
| `POST .../launch-authorisations/{id}/reject\|cancel` | | none | protective |
| `POST .../capability-grants/requests`, `/{id}/approve`, `/{id}/revoke` | `capability_grant:*` | critical | (b)(d): K1 grants financial authority. A revoke is protective, but it is critical because an attacker revoking defenders' grants during an incident is a real denial-of-service vector. Low volume. |
| `POST .../capability-grants/requests/{id}/reject\|cancel` | | none | protective |
| `POST /v1/admin/financial-policy-changes`, `.../tenants/{t}/financial-policy-changes`, `/{id}/approve` | `financial_policy:*` | critical | (d)(e): it changes the four-eyes thresholds themselves |
| `POST .../financial-policy-changes/{id}/reject\|cancel` | | none | protective |
| `POST /v1/admin/withdrawal-policies`, `DELETE /v1/admin/withdrawal-policies/{id}` | `withdrawal_policy:write` | critical | (e): thresholds and `require_step_up` (permission.go:122-127 bypass #3) |
| `POST .../provider-credential-requests`, `/{id}/decisions`, `/{id}/apply`, `POST .../provider-credentials/{h}/transitions` | `provider_credential:*` | critical | (c). A revoke transition is protective, but it is still critical: revoking production credentials is an outage vector. |
| `PUT /v1/admin/providers/{p}/capability`, `PUT /v1/admin/casino/providers/{p}/capability` | `provider_config:write`, `casino_config:write` | critical | (c)(e): provider enablement per tenant |
| `POST /v1/admin/alerting/routes` | `alert:route_manage` | critical | (f): an attacker could reroute or silence alerts |
| `POST /v1/admin/assets`, `PATCH .../assets/{code}`, `PUT .../assets/{code}/activation\|operation-eligibility\|platform-authorization`, `POST .../assets/change-requests` and `/{id}/decision` | `asset_registry:manage` | critical | (d)(e): what money can move. Dual control. |
| `PUT /v1/admin/asset-authorizations/*` | `asset_authorization:write` | critical | (e) |
| `POST /v1/admin/jurisdictions`, `POST /v1/admin/licences`, `PUT .../tenants/{t}/licence` | `jurisdiction_registry:manage`, `tenant_licence:assign` | critical | (e): regulatory basis |
| `PUT /v1/admin/jurisdiction-resolution-active/{c}`, `PUT /v1/admin/jurisdiction-evidence-collection/{e}`, jurisdiction evaluation-policy writes, operating-market ceiling/tenant/brand policy writes | `jurisdiction_*`, `operating_market_*` | critical | (e) |
| `POST /v1/admin/sportsbook/jurisdiction-restrictions` and `/{id}/withdraw` | `sportsbook_jurisdiction_restriction:manage` | critical | (e). A withdraw loosens the control. Creating one is protective, but it is grouped for legibility. |
| `POST /v1/admin/risk/rules` and `/{id}/disable`, `POST /v1/admin/sportsbook/exposure-limits` and `/{id}/disable` | `risk_config:manage`, `sportsbook_exposure_limit:manage` | financial | (e): a disable or a new looser rule exposes the book |
| `POST /v1/admin/bonus/grants`, `/manual-grants`, `/manual-grants/{id}/activate`, `/economic-operations`, `/bulk-jobs`, `/bulk-jobs/{id}/execute`, `/held-dispositions/{id}/resolve` | `bonus_grant:issue`, `bonus_adjustment:write`, `bonus_bulk:execute`, `bonus_held_disposition:resolve` | financial | (a): creates bonus value |
| `POST /v1/admin/bonus/change-requests` and `/{id}/decide` | bonus governance | critical | (d) |
| `POST /v1/admin/bonus/campaigns/{id}/activate`, `/offers/{id}/publish` | `bonus_campaign:activate`, `bonus_offer:manage` | financial | (a): releases value to many players |
| Campaign, offer and segment create or edit, suggestion claim or decide | bonus authoring | none | configuration. Activation or publishing is the gate. |
| `POST /v1/admin/casino/games/{g}/change-requests`, `/casino/change-requests/{id}/approvals` | `casino_catalogue:govern` | critical | (d) |
| `PUT /v1/admin/casino/games`, `PUT .../games/{g}/availability` | `casino_catalogue:manage`, `casino_config:write` | none | catalogue configuration under governance |
| `POST /v1/admin/players/{id}/suspend`, `POST /v1/admin/rg/restrictions` | `player:suspend`, `rg_restriction:write` | none | protective |
| `POST /v1/admin/players/{id}/reinstate`, `POST .../identity-review/clear` | `player:suspend`, `identity_review:manage` | critical | (e): lifts a protective control |
| Clearing an RG restriction (ADR 0026 section 12) | `rg_restriction:write` | critical | (e) |
| ADR 0115: `POST /v1/admin/players/{id}/rg/limits` that is an **increase or removal** | `PermRGLimitWriteOnPlayerRequest` | critical | (e). It **unblocks the ADR 0115 S2 403**. The player's own `confirm` is still required. A decrease is `none`. Classified by a pinned classifier, like launch. |
| ADR 0115: RG floor change requests and their approve, `limit-configs` writes | `PermRGConfigWrite` and the floor four-eyes | critical | (d)(e) |
| `POST /v1/admin/kyc/verifications/{id}/review`, `/documents/{id}/review` | `verification:review` | none [ORCH-DEFAULT, revisit OQ-4] | high volume. KYC approval does not by itself move money: withdrawal approval is step-up. |
| `POST /v1/admin/payout-instruments/{id}/suspend` | `payout_instrument:suspend` | none | protective |
| `POST /v1/admin/tenants`, `POST .../brands` | `tenant:write`, `brand:write` | critical (tenant), none (brand) | tenant creation is a platform-level identity boundary. A brand is born `pending_launch`. |
| All staff lifecycle routes (section 14.2) except `suspend` | `staff:manage` and the new governance permission | critical | (b) |
| `POST .../staff/{id}/suspend`, `POST /v1/admin/platform-staff/{id}/suspend` | `staff:manage` | critical | (b). Suspension is protective toward the target, but an attacker suspending defenders is a lock-out vector. The last-admin guard bounds it, and step-up makes it costly. |
| `POST .../staff/{id}/person-link`, `POST /v1/admin/platform-staff/{id}/person-link` | `staff:manage` | critical | (b): Person binding decides four-eyes distinctness |
| `PATCH .../staff/{id}/display-name`, `PATCH /v1/admin/staff/me/display-name` | | none | cosmetic. Audited (ADR 0104). |
| `POST /v1/staff/mfa/recovery-codes/regenerate` | own | critical | (b) |
| `POST /v1/admin/alerts/{id}/ack\|resolve` | `alert:manage` | none | operational |
| `POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event` | non-production only | none | test support, not mounted in production |

**Pending flows.** A route added later must take a class in the same change, and the golden test makes that visible.
ADR 0114 S1b, the ADR 0116 AML case actions and the ADR 0107 closed-tenant resolution pick their class from rule 8.1 at
their own review.

## 9. Integration with the governed flows, the actor proof and Person binding

### 9.1 Actor proof (ADR 0110): no format change, a signer backstop

- **Proof format unchanged.** The proof stays `v1`, and the DB verifier does not change for step-up. The reason:
  - Under M5 the attacker cannot forge a proof at all.
  - A proof is minted only by the application, which is where step-up is enforced.
  - Putting `mfa_at` into the proof would add nothing the DB could check independently, because the DB holds no factor
    material.
- **Signer backstop.** `actorproof.Issuer` gains a registry, `actorproof.StepUpOperations`, which maps each proof
  operation to a class. Its contents:
  - `ledger_adjustment:initiate|approve`
  - `payment_force_resolve:request|approve`
  - `withdrawal_hold_resolution:request|approve`
  - `capability_grant:request|approve|revoke`
  - `financial_policy_change:propose|approve`
  - `launch_authorisation:request|approve`
  - `staff_lifecycle:request|approve` (new, section 10.4)
  - `rg_floor_change:*` when ADR 0115 adds it
- **When a proof is signed** for one of these operations, the issuer requires `auth.StepUpAssertionFrom(ctx)`. The
  assertion must:
  - be present;
  - have `Subject == proof actor`;
  - have an `MFAAt` no older than the class max-age plus 30 s of in-flight grace.
- **Otherwise signing returns `ErrStepUpMissing`** and the handler answers 500 (ADR 0110 section 7: a signing failure
  is a defect, never a 403).
- **Why have a backstop.** It turns a missing `RequireStepUp` on a route into a loud failure instead of a silent bypass.
- **Exemptions.** `:cancel`, `:reject` and the launch `:suspend` are not in the registry.
- **Test support.** `prooftest` and `capability.TestSessionProofHook` (integration tag only) place a synthetic assertion
  in the context.

### 9.2 Flows without a proof (ADR 0110 T8): withdrawal approvals, kill-switch release, asset dual control, bonus and catalogue governance, provider credentials

- **Route-level only.** For these flows step-up is enforced at the route by `RequireStepUp`. The handler passes the
  assertion into the domain call where the domain owns a policy decision (9.3).
- **SQL is a separate threat.** These flows stay open to arbitrary SQL (T8, owner-deferred). Step-up protects them
  against M1 and M2, not against M5. This is stated, not hidden.

### 9.3 Withdrawal policy `require_step_up`

- **The new contract.** `withdrawal.Approve` gains a parameter `stepUp auth.StepUpAssertion` (zero value means none).
  The placeholder branch (`withdrawal.go:857-860`) becomes: if `requiresMultipleApprovals && policy.RequireStepUp`,
  then require `stepUp.MFAAt` to be within `StepUpPolicyMaxAge`, which is 2 min [ORCH-DEFAULT], stricter than the
  route's `financial` 10 min. Otherwise it returns `ErrStepUpRequired`, and the HTTP layer now maps that to 403
  `step_up_required` with `max_age = 120`.
- **The policy flag stays meaningful.** It tightens freshness for large withdrawals.
- **Still fail-closed.** An automated decision (`isAutomated`) never satisfies a step-up policy.

### 9.4 Person binding

- **The current state.** Four-eyes distinctness counts Persons through `staff_users.person_id`, which is append-only
  (migration 0034). A NULL Person refuses (ADR 0099 R-4).
- **What MFA proves.** It proves possession of the factor enrolled to **that staff account**. It does not prove the
  Person. Person binding stays an administrative fact.
- **New rules:**
  - An invitation for any role that holds an approve permission **must carry `person_id`**, enforced in the Go
    validation and by trigger `SF014`. Those roles are `platform_admin`, `finance`, `tenant_admin` (it holds the
    `financial_policy:tighten` and `capability_grant` request paths) and `compliance` (launch request).
    [ORCH-DEFAULT; other roles optional.]
  - A staff lifecycle approver must be a different staff member **and** a different Person from the requester and from
    the **target** (`SF032`). Nobody approves their own MFA reset or their own role grant through a second account.
  - An MFA reset never changes `person_id`. Linking a Person stays a separate `critical` route.

### 9.5 Liveness inside governed transactions (suspension race)

- **The re-assertion.** Every step-up-classified handler re-asserts the actor's liveness **inside its own business
  transaction** with `SELECT status, role, staff_version FROM staff_users WHERE id = $actor FOR SHARE`. On a mismatch
  it refuses with `ErrActorNotLive` (401 `session_revoked`).
- **Why it serialises.** A suspension's `UPDATE staff_users` takes a row lock that conflicts with `FOR SHARE`. Either
  the governed write commits first (and is audited as having happened before the suspension), or it sees `suspended`
  and refuses.
- **Lock order.** The re-assertion is taken at the existing ADR 0082 Amendment A8 position: L1 `staff_users FOR SHARE`,
  ascending id, **after** the flow's own L1 request-row `FOR UPDATE` and before L2/L3.
  - K2 and K3 already take this lock (A8), so for them the re-assertion adds only the `role` and `staff_version`
    comparison to an existing statement.
  - A flow with no L1 request row (withdrawal approve, kill-switch release, bonus, configuration routes) takes it as
    its first lock, after its existing withdrawal-row lock where it has one.
  - Taking it earlier than A8 is **forbidden**. A shared lock queued behind a waiting suspension `UPDATE` would form a
    deadlock cycle with a peer that holds the request row.
  - `ledger-finance` confirms the placement in S4.
- **The suspension transaction** locks only `staff_users` (UPDATE), `sessions` (revocation UPDATE) and the append-only
  lifecycle rows. It never takes a financial lock, so it cannot form a cycle with an L1-L3 holder.
- **Existing DB guards are kept.** The guards that already check `staff_users.status = 'active'` (for example
  migrations 0106 and 0112 `financial_actor_session`) remain unchanged and stay as defence in depth.

## 10. Staff lifecycle (STAFF-LIFECYCLE-1)

### 10.1 States and transitions (DB-enforced by `SF010`)

```text
invited --accept+enrol--> active --suspend--> suspended --reactivate--> active
   |                        |                    |
   +--revoke invite--> offboarded <--offboard----+  (offboarded is terminal)
```

| Transition | Who (server-side; tenant from the token, never the body) | Step-up | Four-eyes | Effects, all in one transaction |
|---|---|---|---|---|
| create invitation (`-> invited`) | tenant staff: `staff:manage` in its own tenant, for the roles it may create today (`admin_routes.go:550-585`: tenant_admin may create `tenant_admin`, `support` and `compliance`; platform_admin may create any tenant role). Platform staff: `platform_staff:govern` through a 10.4 request. | critical | platform staff only | `INSERT staff_users(status = 'invited', password_hash = NULL, invited_by)`; an enrolment token (`invite`) is emailed; a lifecycle event; audit `staff.invited` |
| accept (`invited -> active`) | the invitee, holding the token and choosing a password (minimum length as today) | n/a | no | sets `password_hash`; consumes the token; returns a `purpose = 'enrol'` challenge. The account is `active` but **cannot get a session until a factor is confirmed**. |
| revoke invitation (`invited -> offboarded`) | same authority as the invitation | critical | no | revokes the token; a lifecycle event |
| suspend (`active -> suspended`) | tenant staff: `staff:manage` in its own tenant, but tenant_admin only for roles it may create. Platform-only roles (`finance`, `risk_manager`, `promotions_manager`, `bonus_operations`) and platform staff: platform_admin. | critical | no (protective) | `staff_version++` (`SF012`). Revoke all of the target's sessions (`auth.RevokeAllSessionsForPrincipal`), live challenges and enrolment tokens. If the reason is `security_compromise`, also revoke the factors and recovery codes. Lifecycle event and audit. |
| reactivate (`suspended -> active`) | the authority that may **grant** the target's current role. Platform staff: a 10.4 four-eyes request. | critical | platform staff | `staff_version++`. If the factors were revoked (compromise), an `mfa_reset` token is issued and emailed. Lifecycle event and audit. |
| offboard (`active\|suspended -> offboarded`) | same as suspend | critical | no | `staff_version++`. `password_hash := NULL`. Revoke all factors, recovery codes, challenges, tokens and sessions. Set `offboarded_at`. `email` and `person_id` are kept, because audit identity must stay resolvable. |
| role change (`active -> active`, role A to B) | tenant: tenant_admin only where **both** A and B are roles it may create; platform_admin any tenant role. The tenant/platform boundary cannot be crossed (`tenant_id` is immutable, `SF011`; platform_admin is the only platform role). | critical | no for tenant roles (OQ-5); a platform grant **is** an invitation through 10.4 | `staff_version++`. Revoke all sessions. `SF014` (Person required for approve-capable roles). Lifecycle event with before/after role. |
| MFA reset | tenant staff: as suspend. Platform staff: a 10.4 four-eyes request. | critical | platform staff | revoke factors and recovery codes, revoke all sessions, issue an `mfa_reset` token by email. A hard-locked factor is cleared only this way. |
| Person link (existing route) | unchanged authority | critical (new) | no | unchanged (append-only, migration 0034) |

- **Reason codes are required on every transition.** The vocabulary is closed, a CHECK in the DB plus Go constants
  [ORCH-DEFAULT]:
  - suspend: `security_compromise`, `policy_violation`, `investigation`, `leave_of_absence`, `role_review`, `other`
  - offboard: `employment_ended`, `contract_ended`, `security_compromise`, `duplicate_account`, `other`
  - reactivate: `investigation_closed`, `returned_from_leave`, `error_correction`, `other`
  - role change: `role_change_approved`, `error_correction`, `other`
  - MFA reset: `device_lost`, `device_replaced`, `hard_locked`, `security_compromise`, `other`
  - invite revoke: `no_longer_required`, `wrong_details`, `other`
- **`other` needs a `note`** of 10-500 characters, with the ADR 0104 PII warning. The ADR 0104 display-name hygiene
  rules apply to the note.

### 10.2 Self-target and the scope fence

- **No self-target.** No actor may suspend, reactivate, offboard, change the role of or MFA-reset **their own**
  account. This is checked in Go by comparing the token subject with the path id. In the DB, `SF013` refuses a status
  or role change where `NEW.id = app.principal_id`. That GUC is forgeable by SQL, so `SF013` is defence in depth
  against an application bug.
- **Allowed self-service:** re-enrolment through recovery (6.1.3), regenerating one's own recovery codes, one's own
  display name and one's own sessions.
- **Scope fence.** A tenant-scoped caller can never reach a platform row: the dual-scope RLS of migration 0011 hides it,
  and the handler answers 404. A cross-tenant target is also 404 (`canActOnTenant`, then RLS).

### 10.3 Last-admin protection

- **`SF020` (platform).** A BEFORE UPDATE OF `status`, `role` trigger on `staff_users`, for rows with
  `tenant_id IS NULL`.
  - It fires when OLD is an active `platform_admin` and NEW is not.
  - It takes `pg_advisory_xact_lock(hashtext('sf:last_admin:platform'))`, then counts the **other** active platform
    admins that hold an active factor which is not hard-locked.
  - If there are none, it refuses.
  - The advisory lock serialises two admins suspending each other at the same moment. The second transaction re-counts
    after the first commits, because each statement under READ COMMITTED takes a fresh snapshot.
- **`SF021` (tenant).** The same rule per tenant, keyed by `sf:last_admin:<tenant_id>`, for `tenant_admin`.
  - It applies only when the acting session is tenant-scoped, that is when no `app.acting_platform_principal_id` or
    `app.platform_admin_principal_id` is set.
  - A platform admin may remove the last tenant_admin (tenant offboarding) with a reason code.
  - It is an accident guard. GUC forgery under M5 is out of its scope.
- **The console warns** when fewer than 2 platform admins are active and enrolled, because the 10.4 four-eyes then
  cannot complete without the owner path (section 11).

### 10.4 Four-eyes for platform-staff changes (`staff_lifecycle_requests` and `staff_lifecycle_approvals`)

- **Kinds:** `platform_invite` (the only way to grant `platform_admin` over HTTP), `platform_reactivate` and
  `platform_mfa_reset`. A platform suspend or offboard is single-actor, because it is protective and bounded by
  `SF020`.
- **Approvals.** One approver plus the requester, so two distinct Persons [ORCH-DEFAULT, OQ-5 may raise it to two
  approvers]. Approver ≠ requester and ≠ target, by staff id **and** by Person. A NULL Person refuses (`SF032`).
- **Lifetime.** `expires_at = now() + 72h`, forced by the DB.
- **Flow (the ADR 0112 S9 pattern):**
  - The requester INSERTs the request with an actor proof (`staff_lifecycle:request`, scope `platform`, target `new`,
    payload = a digest of kind, target, email, Person, reason code and note).
  - The approver locks the request `FOR UPDATE` and INSERTs the approval (proof `staff_lifecycle:approve`, target =
    request id, payload = the request's `payload_hash`).
  - It then moves `pending -> executing`, performs the staff change, moves `executing -> executed` and writes the
    lifecycle event and the audit row, all in one transaction.
- **`SF030` guard.** It refuses the privileged changes unless this txid has an `executing` request of the matching kind
  and target, or `current_user` owns the table (the owner path, section 11). The privileged changes are:
  - INSERT of a `platform_admin` row;
  - a platform row moving `suspended -> active`;
  - an INSERT of a `staff_enrolment_tokens` row with `purpose = 'mfa_reset'` for a platform staff member.
- **`SF033`.** A deferred check at commit: no request is left `executing`.
- **Actor proof (ADR 0110 extension).**
  - `actor_proof_require` and `internal/actorproof` admit the prefix `staff_lifecycle:` for scope `platform` with a
    NULL tenant, in lockstep. `platform_acting` and `tenant` are refused (`AP004`).
  - The governed-table list grows from 13 to 15.
  - `internal/stafflifecycle` joins the `TestStatic_OnlyFourEyesPackagesSign` allow-list. The security review of that
    change is mandatory.
- **What this closes.** Through `SF030`, a platform_admin INSERT by SQL alone is now refused: it needs a proof-bound
  approval that SQL cannot forge. ADR 0110 section 8 had deferred this "owner-only lifecycle for platform rows" as part
  of H1. **It is included here** because the task requires four-eyes for platform role grants, and a DB-enforced
  four-eyes is the platform's rule (CLAUDE.md). It is reversible (one trigger). OQ-2 asks the owner to confirm it.

### 10.5 MFA reset and hard lock

- **A hard-locked factor** (6.5) blocks login and step-up. Recovery codes still work, through 6.1.3, unless the codes
  are also exhausted.
- **Otherwise it needs an MFA reset** under 10.1 or 10.4. The reset revokes everything and emails an `mfa_reset`
  token. The staff member redeems it with their password (6.3), then enrols.
- **The resetter never sees the token** and cannot enrol a factor on the target's behalf.

### 10.6 Audit (before/after, actor, reason code)

Every lifecycle change and every MFA event writes, in the same transaction:

1. **A `staff_lifecycle_events` row (append-only, `SF099`).** Columns:
   - `id`, `tenant_id` (the target's), `staff_user_id`
   - `event` (closed vocabulary, below)
   - `actor_type` (`staff`, `system` or `owner`), `actor_staff_id`, `actor_person_id`
   - `before` JSONB and `after` JSONB: only `status`, `role`, `staff_version`, `person_id`, factor ids and states,
     never secrets
   - `reason_code`, `note`, `lifecycle_request_id`
   - `request_id`, `ip`, `user_agent`, `created_at`

   The `event` values are `invited`, `invite_revoked`, `invite_accepted`, `suspended`, `reactivated`, `offboarded`,
   `role_changed`, `person_linked`, `mfa_enrolled`, `mfa_reset`, `mfa_hard_locked`, `mfa_recovery_used`,
   `recovery_codes_regenerated` and `owner_provisioned`.
2. **An `audit.Record` row.** The action is `staff.<event>`. `Metadata` carries `{reason_code, before, after,
   lifecycle_request_id}`. A platform actor on a tenant target sets `SubjectTenantID`, so the tenant sees the action
   (ADR 0104), with the platform actor redacted (ADR 0114 section 4.1).

Authentication events go to `audit.Record` only, because they are high volume. They are `staff.login_password_verified`,
`staff.login_succeeded`, `staff.mfa_verification_failed`, `staff.step_up_verified`, `staff.step_up_failed`,
`staff.mfa_locked` and `auth.session_assurance_invalid`.

## 11. Break-glass [ORCH-DEFAULT]

- **None in the application.** There is no "MFA bypass" flag, no master recovery code, no support override and no
  environment switch.
- **The only escape is the existing owner path.** `cmd/seed-admin` is run by an operator with **database owner
  credentials**, which the runtime never holds: production startup refuses the owner role (ADR 0112 S11). In this
  mode it:
  1. refuses unless `-break-glass` is given **and** a `-reason` is supplied;
  2. creates a new `platform_admin` row through the owner path, which `SF030` admits because `current_user` owns the
     table;
  3. issues a `bootstrap` enrolment token (keyed; printed once);
  4. writes an `owner_provisioned` lifecycle event and a platform audit row;
  5. raises the durable alert `staff_break_glass_used` (P1) to every remaining platform admin.
- **Usage rule.** Two operators should be present. That is a runbook control (S9), not a software control, and it is
  stated as such.
- **Owner reversal.** OQ-6 asks whether the owner wants a governed in-app break-glass instead. The recommendation is
  no.

## 12. Residuals (stated honestly)

| # | Residual | Severity | Mitigation / owner |
|---|---|---|---|
| R1 | **M5 insider escalation.** An attacker who holds both a legitimate staff account (with its factor) and arbitrary SQL can `UPDATE` their own `role` or `status` (for example, un-suspend themselves or move from support to finance in the same tenant), then log in with their real MFA. `SF010` and `SF012` still admit a legal-looking transition. | High under M5 | **OQ-2:** an `authz_seal` column, `HMAC(staff-authz subkey, id, tenant, role, status, staff_version, person_id)`, written by the application on every lifecycle change and verified at login, refresh and liveness. This is part of H1, so it is not built until the owner authorizes it. Slice S-H1 is specified so it can follow immediately. |
| R2 | **T3 is narrowed, not closed.** For staff, MFA neutralises SQL-minted rows (no factor, no keyed token), taken-over passwords (factor AEAD) and forged sessions (seal). Player T4 and keyed refresh hashes for players are untouched. | Medium | H1/T4 owner items stay OPEN. The decision register is updated after S4 to say "staff part narrowed". |
| R3 | **Factor deletion by SQL** denies service to the victim (a lock-out). It is never a bypass. | Low | Durable alert on missing-factor logins for accounts that previously had one, using the lifecycle events. MFA reset. |
| R4 | **TOTP is phishable** by a real-time proxy (AitM). | Medium | WebAuthn for platform_admin is the next factor (OQ-3). `StaffFactor` already accommodates it. |
| R5 | **Token theft inside the step-up window** (5 or 10 min) by an attacker who controls the browser. | Low-medium | Short windows. Liveness revocation. Audit. Per-session step-up failure revocation. |
| R6 | **Pending governed requests initiated by a staff member who is later suspended or offboarded** stay approvable by a colleague. The K-flow guards check the acting principal's status, not the initiator's. | Medium | The console shows the initiator's status (S8). A follow-up ADR adds an "initiator still active" check to the K1-K3 and launch guards (it changes existing guards, so it is out of scope here). |
| R7 | **Active K1 capability grants** of an offboarded staff member stay in the table. They are inert, because `financial_actor_session()` requires `status = 'active'`. | Low | The offboarding screen lists them with a revoke link (critical step-up). |
| R8 | **Email delivery is MOCK.** Production invites and resets need a real email adapter (PROVIDER DEPENDENT). Until then, the only delivery is the owner-run CLI. | Launch dependency | OQ-1 |
| R9 | **The per-IP limiter is per process** (`ratelimit.go` caveats). | Low | The per-factor counters are in the DB and global, so they are the real control. |
| R10 | **Application-host compromise** (it holds the JWT, proof and MFA keys). | Inherent | ADR 0110 T6 |
| R11 | **A clock skew between the application and an authenticator** larger than ±30 s fails closed (the user cannot log in). | Low | The runbook covers NTP. |
| R12 | **Email retention.** `email` and `person_id` are kept after offboarding for audit identity. That PII retention needs a data-retention decision. | Compliance | It is recorded. A retention/erasure policy is a separate compliance decision and does not block this ADR. |

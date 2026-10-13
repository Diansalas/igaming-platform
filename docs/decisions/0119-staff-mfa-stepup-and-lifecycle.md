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

# ADR 0119 - Staff MFA, step-up authentication and staff lifecycle

- **Status:** ACCEPTED - PENDING IMPLEMENTATION (2026-10-13). Design only: no code, no migration and no endpoint exists
  yet. Every mechanism below is `NOT IMPLEMENTED` until its slice (section 16) lands and passes its review gate. No
  owner question in section 18 blocks the design or slices S1-S9. Each policy value marked **[ORCH-DEFAULT]** is a
  reversible orchestrator/security default. It is **not owner-approved**, and the owner may change it without a
  redesign.
- **S1 (2026-10-13):** the pure-Go library slice is `IMPLEMENTED` on an unmerged branch, pending gate G. Every other
  slice stays `NOT IMPLEMENTED`. See section 21.
- **Revision 2 (2026-10-13):** the S0 design review by `architect` and `ledger-finance` returned ACCEPT WITH
  CONDITIONS. Section 20 resolves every blocking condition (C1-C6) and accuracy fix (C7-C11) in the text, and folds
  in the cheap non-blocking items (N1-N15). The `security` rulings are recorded there: C2 adopts the scoped
  re-assertion; C3 rotates factors by re-enrolment only; C5 moves `SF030` to S-H1; C6 merges S5 with S6.
- **Supersedes in part:** ADR 0017 (architecture foundation, `NOT IMPLEMENTED`). This ADR closes ADR 0017's four "open
  decisions" with reversible defaults. Two ADR 0017 choices change, and the reasons are given in place: recovery codes
  use a keyed HMAC, not Argon2id (section 6.4); MFA policy is code, not a data row (section 4.1).
- **Classification:** class A, before technical production readiness. The staff MFA and STAFF-LIFECYCLE-1 rows of
  `docs/plans/platform-reconciliation-2026-10-13.md` (lines 56-57, 215, 229, 239). ADR 0115 S2 staff RG increases stay
  403 until S4b of this ADR plus ADR 0115's own classifier slice land.
- **Inputs (verified at `a3e56dd`):**
  - `internal/auth/{jwt,session,middleware,permission,password,keys}.go`
  - `internal/httpserver/{staff_routes,auth_routes,admin_routes,routes,ratelimit,withdrawal_handlers}.go`
  - `internal/identity/{staff_user,login_attempt}.go`, `internal/withdrawal/withdrawal.go:795-860`
  - `internal/secretstore`, `internal/actorproof`, `internal/payoutinstrument/keys.go` (key-family precedent)
  - migrations 0011 (`staff_users`), 0012/0018 (`sessions` and its RLS), 0029/0034 (Person link)
  - ADR 0099, 0101, 0110, 0112, 0114, 0115
  - `docs/governance/human-decision-register.md` (H1/T3), `docs/architecture/01-requirements-inventory.md`
- **Owners:** `security` (mechanism; author); `architect` (claims shape, schema, ADR 0110 extension); `identity-compliance`
  (lifecycle policy); `qa` (gates); `ledger-finance` (S0 re-check and S4b: section 9.5, ADR 0082 A9, the K-flow integration).
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
   - The keys are two new families that the deployment secret store delivers: `STAFF_MFA_FACTOR_KEYS` (AEAD) and
     `STAFF_MFA_MAC_KEYS` (all keyed MACs).
   - Recovery codes, login challenges and enrolment tokens are stored only as keyed HMACs.
   - None of these values is ever logged, rendered or audited.
4. **Session assurance.**
   - Staff tokens gain `sid`, `aal`, `amr`, `auth_time`, `mfa_at` and `sv`.
   - The matching `sessions` columns are sealed with a keyed HMAC, and a refresh with a missing or bad seal is refused.
   - A staff session lasts at most 12 h, with a 30 min idle limit [ORCH-DEFAULT].
5. **Liveness on every staff request.** One indexed query checks that the session is not revoked, that the staff member
   is `active` and that the `role` and `staff_version` match the token. If the check fails, the request is refused.
   Suspending or offboarding a staff member therefore takes effect on their next HTTP request, not 15 minutes
   later. Governed transactions already in flight are covered by section 9.5 and residual R13. Already-queued
   governed requests are covered by R6.
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
     four-eyes by distinct Persons, through proof-bound request and approval tables. The extra `staff_users` guard
     `SF030` is part of the owner-deferred H1 item, so it is built only in S-H1 (section 10.4, OQ-2).
   - The DB keeps at least one active platform_admin (last-admin protection).
   - Invitations replace admin-chosen passwords.
10. **Break-glass: none in the application** [ORCH-DEFAULT]. The only recovery when no platform_admin can authenticate
    is the existing `cmd/seed-admin` bootstrap. It runs as a job **inside the deployment environment**, with the same
    secret-store injection of the database URL and the MFA keys. It is audited and alerted (section 11).
11. **New SQLSTATE class `SF`.** It does not collide with any code used today (section 13.6). Three migrations:
    0137 (MFA tables, S2), 0138 (the staff-session cutover, merged with S3) and 0139 (lifecycle, merged with S6). The
    orchestrator re-allocates the numbers at dispatch.

## 3. Threat model (what this defends, and what it does not)

| # | Threat | Defence in this ADR | Residual |
|---|---|---|---|
| M1 | Stolen or guessed staff password (phishing, reuse, stuffing) | Mandatory TOTP. Without the factor a password yields only a single-use 5 min challenge. | Real-time phishing proxy (AitM) that relays the TOTP. TOTP is not phishing-resistant, so WebAuthn is the follow-up (OQ-3). |
| M2 | Stolen access or refresh token (XSS, device theft) | Step-up on every high-risk route. The session is limited to 12 h absolute and 30 min idle. The liveness check honours revocation immediately. | Inside the step-up window (5 or 10 min) a token thief who also controls the browser can act. Bounded and audited. |
| M3 | TOTP replay (shoulder-surf, log, proxy) | Monotonic `last_used_step`; a code is valid only once per factor. | none |
| M4 | Online TOTP brute force | Per-factor lockout with a `FOR UPDATE` counter. A hard lock after 10 failures in 24 h. The challenge allows 5 attempts. | About 3e-5 success per hard-lock cycle (3 valid codes in 10^6, 10 tries). After a hard lock an admin must reset the factor. |
| M5 | Arbitrary SQL as `igaming_runtime` (THREAT-MODEL-ARBITRARY-SQL-1): plant a factor, recovery code, challenge or enrolment token; forge a sessions row with `aal=2`; delete a factor to force re-enrolment | AEAD with AAD that binds the factor; keyed HMACs for codes, challenges and tokens; a keyed assurance seal on sessions; MFA policy held in code. A deleted factor causes denial of service, never a bypass, because enrolment needs a keyed enrolment token. | See section 12, R1-R3. **T3 is narrowed for staff, not closed.** A SQL `UPDATE staff_users.role` or `status` on a staff member's own row is still accepted, unless the owner authorizes S-H1 (OQ-2). `sessions.revoked_at` is not sealed (R2). |
| M6 | Insider tenant_admin escalation (shell accounts, self-grant) | Invitations: the API never returns the credential. The existing platform-only role list is kept. Self-target is refused. Platform roles need four-eyes. | The inviter still chooses the invitee's email address and Person (R14), so an inviter who controls that mailbox gets a working account. A tenant_admin can also invite an accomplice tenant_admin. That is a legitimate authority, and it is audited. |
| M7 | A leaving or compromised staff member keeps access | Suspend or offboard revokes sessions and bumps `staff_version`. Liveness refuses the next request. Governed writes that move value or change authority re-check liveness `FOR SHARE` inside the transaction (section 9.5). | A request already committed before the suspension stays committed and audited. That is correct. Other step-up routes have a window as long as the handler runs (R13). |
| M8 | Lock-out of governance (all admins suspended or lost factors) | Last-admin guard; four-eyes MFA reset; the `cmd/seed-admin` bootstrap job (section 11). | If the deployment environment itself is lost, recovery is an operational matter outside this design. |
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
  staff account is re-seeded or re-invited, and every existing staff session is revoked by migration 0138 (section 13.1a).

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

- **Config: two families, so factor keys and MAC keys can be retired independently** (review C3).
  - `STAFF_MFA_FACTOR_KEYS` and `STAFF_MFA_FACTOR_ACTIVE_KID`: the AES-256-GCM factor-secret key only.
  - `STAFF_MFA_MAC_KEYS` and `STAFF_MFA_MAC_ACTIVE_KID`: the four keyed-MAC subkeys of section 5.2.
  - Both use the `kid:base64(secret)`, comma-separated format and are `config.SecretValue`.
  - The deployment secret store delivers them exactly as it delivers `ACTOR_PROOF_KEYS` and `PAYOUT_INSTRUMENT_KEYS`
    (ADR 0110 section 6, ADR 0111 section 2.2).
- **`config.Load` refuses:**
  - malformed entries;
  - any key under 32 decoded bytes;
  - an unknown active kid;
  - outside an explicit development environment, the published `.env.example` placeholder.
- **One platform key-family registry** (review N7). S1 adds a single registry in `internal/config` of every platform
  key family, with **pairwise** distinctness checked across all of them: both JWT secrets, `ACTOR_PROOF_KEYS`,
  `PAYOUT_INSTRUMENT_KEYS`, `PAYOUT_INSTRUMENT_FP_KEYS`, `PROVIDER_CREDENTIAL_FINGERPRINT_KEY` and the two new
  families. A family added later then cannot forget the check.
- **Why not `internal/secretstore.Fetcher`.** That package resolves per-tenant provider-credential handles. Its refs
  must sit in the `/provider-creds/` namespace (`secretstore.go:113-152`), and its circuit breaker and negative cache
  are tuned for outbound provider calls. A platform-wide key that every staff login needs belongs with the other
  platform key families, loaded once at startup. "At rest via the secret store" means this: the key material exists
  only in the deployment secret store and in process memory. The database holds only ciphertext and keyed MACs.
- **Startup gate.** `staffmfa.VerifyStartup` runs from `cmd/platform-api/main.go` and `cmd/seed-admin`. In every
  environment it refuses to start without both valid families, because staff login cannot work without them. Unlike the actor proof, no environment
  "fails closed at the DB": with no key, there is no staff login.

### 5.2 Subkeys (HKDF-SHA256, the same construction as `payoutinstrument/keys.go:107-117`)

The HKDF `info` is `label || "|" || kid`, so a subkey can never be reused across kids.

| Family | Label | Use |
|---|---|---|
| FACTOR | `staff-mfa-factor-aead-v1` | AES-256-GCM key for factor secrets |
| MAC | `staff-mfa-recovery-v1` | HMAC key for recovery-code hashes |
| MAC | `staff-auth-challenge-v1` | HMAC key for login and enrolment challenge-token hashes |
| MAC | `staff-enrolment-token-v1` | HMAC key for invite, enrolment and MFA-reset token hashes |
| MAC | `staff-session-assurance-v1` | HMAC key for the sessions assurance seal |

### 5.3 Formats

- **Factor secret.** `secret_ct = GCM.Seal(nonce(12 random bytes), secret, AAD)` with
  `AAD = "sf-v1|" || factor_id || "|" || staff_user_id || "|" || coalesce(tenant_id,'platform') || "|" || factor_type`.
  The row stores `secret_kid`, `secret_nonce` and `secret_ct`. If ciphertext is moved to another row, or written by
  SQL, `Open` fails. That failure counts as a verification failure and also raises the durable alert
  `staff_mfa_integrity` (P1). The factor is never treated as "absent".
- **Keyed hashes.** `HMAC(subkey(kid), purpose || "|" || staff_user_id || "|" || value)`. The row stores the kid and
  the 32-byte MAC. Every lookup is by MAC, so a lookup of a planted row (any value, but no valid key) finds nothing.
- **Rotation of the FACTOR family: by re-enrolment only. No rewrap** (review C3, option ii).
  - The factor's `secret_*` columns stay immutable (`SF002`), and the runtime has no UPDATE grant on them. A rewrap job
    would need both owner database credentials and the factor key in one process, which is a worse exposure than the
    rotation it serves.
  - Procedure:
    1. Add the new kid and make it active. New enrolments use it.
    2. Run an MFA-reset campaign for every factor under the old kid. The S9 runbook names the query that counts them.
    3. Retire the old kid once no active or pending factor references it.
  - An emergency rotation after a suspected FACTOR key leak means every staff member re-enrols. That is acceptable at
    back-office scale, and it is the correct response anyway, because the secrets themselves are exposed.
- **Rotation of the MAC family.**
  - Challenges (5 min), enrolment tokens (at most 72 h) and session seals (12 h) age out under the old kid, which stays
    verify-only for at most 72 h.
  - Recovery codes are one-way. Their old kid stays verify-only until every code under it is consumed or regenerated,
    through a regeneration campaign with a hard retirement date (S9 runbook).
  - This coupling, recovery codes holding the MAC family's old kid, is **accepted explicitly**. It no longer affects
    the factor key.
- **A retired kid is refused** in both families.

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
   - If the factor is hard-locked: return `403 mfa_locked`, which is also audited. No challenge is issued, so neither
     TOTP nor a recovery code can be presented. A hard lock is cleared **only** by an MFA reset (section 10.5).
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
  - invite and mfa_reset: by email to the staff member's address, through `internal/email.Provider`.
    - `email.MockProvider` is a synthetic component, so `providerkind.RefuseSyntheticInProduction` already refuses to
      start a production binary with it (`cmd/platform-api/registrations.go:388`).
    - A real email adapter is therefore **already a launch prerequisite** (ADR 0030 player verification and reset use
      the same provider). Staff invite and reset are `PROVIDER DEPENDENT` on it. There is no production interim
      unless the owner chooses one (OQ-1).
  - bootstrap: printed once by `cmd/seed-admin`, which runs as a job inside the deployment environment (section 11).
  - **The token is never returned in an HTTP response to the inviter or the resetter.** The API no longer hands the
    credential to the admin, as today's creation route does. The inviter still chooses the invitee's email address and,
    for approve-capable roles, the Person. An inviter who controls that mailbox can still activate the account (R14).
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
- **Only the chain head.** The `sid` row must have `replaced_by_session_id IS NULL`. Otherwise step-up refuses with 401
  `session_revoked`, and the client refreshes first. This keeps it from writing `mfa_at` to a dead row (review N2).
- **Side effect.** `revokeChainFrom` (refresh-token reuse detection) now also kills every outstanding access token of
  that chain, through liveness.
- **Audit.** `staff.step_up_verified`, or `staff.step_up_failed` on failure.
- **Recovery codes are not accepted for step-up.** A staff member who has lost the device recovers through 6.1.3.
- **The browser flow.** The back office shows a modal, calls step-up, swaps the access token and re-sends the
  original request once with the same frozen body (section 15, item 3).
  - **Why the retry is safe.** A route-level `step_up_required` is returned by middleware before the handler runs, so
    no write has happened. The domain-level `ErrStepUpRequired` (section 9.3) returns before any insert, and its
    transaction rolls back.
  - The admin four-eyes routes carry no `Idempotency-Key`. They are idempotent by state, answering 409 on a repeat
    (`launch_authorisation_routes.go:27-31`). Bonus routes that read the header re-send their own key.

### 6.7 Lost password, lost device, lost both (review C9)

- **Lost device, password known:** recovery code (6.1.3). If no codes are left, or the factor is hard-locked: MFA reset
  (10.1, 10.5).
- **Lost password, device kept:** an administrator issues a `password_reset` enrolment token. Its authority and step-up
  are the same as MFA reset, and for platform staff it is a 10.4 four-eyes request of kind `platform_password_reset`.
  The token is emailed. Redemption (`POST /v1/staff/auth/password/redeem` with `{token, new_password, totp_code}`)
  requires a **valid TOTP from the active factor**. It sets the password, revokes every session and returns an `mfa`
  challenge. Neither the token alone nor the mailbox alone is enough.
- **Lost both:** offboard the account and re-invite under a new staff id. The lifecycle events link the two through
  `note`. The Person link carries over, because it is set at invitation. A self-service path is deliberately not
  provided.


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
  - **verify the seal**, refusing a missing or bad one as 401 `invalid or expired refresh token`.
    - **Cutover rule (review C1).** Migration 0138 records its own `now()` in a one-row owner table,
      `staff_session_cutover(at)`. A staff session with a **NULL** seal and `created_at < cutover.at` predates S3. Its
      refresh is a plain `ErrSessionNotFound`: no audit P1, no alert.
    - A **non-NULL bad** seal, or a NULL seal on a session created at or after the cutover, only arises from SQL
      tampering. It writes an audit row `auth.session_assurance_invalid` and raises the durable alert
      `staff_session_integrity` (P1).
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
- On a DB error or timeout it returns 503 (fail closed). That is correct, because every staff handler needs the DB
  anyway.
- **Implementation (review N1).**
  - The scope GUCs and the SELECT go in one pgx batch inside a read-only transaction, with `SET LOCAL
    statement_timeout = '500ms'`.
  - The liveness transaction **finishes and releases its connection before** the handler acquires its own: no nested
    pool acquisition, so no pool-exhaustion deadlock.
  - A metric and an alert track the liveness-refusal and liveness-error rates.
  - Non-staff tokens are refused by `RequireStaffPrincipal` before any DB work, so there is no DoS amplification.
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
  closed set `{none, financial, critical}`, or a named classifier (section 8.2). The argument is required, but `none`
  always compiles, so **the golden table is the control, not the compiler**.
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

### 8.2 Route classification (the golden table pinned by the S4b test)

Only **registered** routes appear here, with exact path strings: the golden test compares strings, and every
registration is a string literal. The inventory was taken at `a3e56dd`. A row with a **classifier** is a named,
pinned Go function. It reads only the already-validated body, or the target row, and the handler re-uses the same
parsed value, so the class cannot change between the classifier and the handler.

**Withdrawals and payouts**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/withdrawals/{id}/approve` | `withdrawal:approve` | financial (a)(d). Supersedes the `ErrStepUpRequired` placeholder (9.3). |
| `POST /v1/admin/withdrawals/{id}/submit` | `withdrawal:submit` | financial (a): dispatches a payout |
| `POST /v1/admin/withdrawals/{id}/resolve` | `withdrawal:submit` | financial (a) |
| `POST /v1/admin/withdrawals/{id}/reject` | `withdrawal:reject` | none (protective) |
| `POST /v1/admin/payout-instruments/{id}/suspend` | `payout_instrument:suspend` | none (protective) |

**K1-K3 and HSEC four-eyes**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/tenants/{tenantID}/manual-adjustments`, `POST /v1/admin/tenants/{tenantID}/manual-adjustments/{requestID}/approve` | `ledger_adjustment:initiate`, `:approve` | financial (a)(d). Both Persons must be strongly authenticated, or a stolen initiator session plus a rubber-stamp approval defeats four-eyes. |
| `POST /v1/admin/tenants/{tenantID}/payment-force-resolutions`, `POST /v1/admin/tenants/{tenantID}/payment-force-resolutions/{resolutionID}/approve` | `payment_force_resolve:*` | financial (a)(d) |
| `POST /v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions`, `POST /v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions/{resolutionID}/approve` | `withdrawal_hold_resolution:*` | financial (a)(d) |
| `POST /v1/admin/tenants/{tenantID}/capability-grants/requests`, `POST /v1/admin/tenants/{tenantID}/capability-grants/requests/{requestID}/approve`, `POST /v1/admin/tenants/{tenantID}/capability-grants/{grantID}/revoke` | `capability_grant:*` | critical (b)(d). A revoke is protective, but it is critical here: revoking defenders' grants during an incident is a denial-of-service vector, and the volume is low. A deliberate exception to the protective rule. |
| the `/reject` and `/cancel` routes of all four families above | | none (protective) |

**Policies and governance**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/financial-policy-changes`, `POST /v1/admin/financial-policy-changes/{changeID}/approve` | `financial_policy:author` | critical (d)(e): it changes the four-eyes thresholds themselves |
| `POST /v1/admin/tenants/{tenantID}/financial-policy-changes` | `financial_policy:author` or `:tighten` | **classifier `financialPolicyChangeClass`**: `none` when the caller holds only `financial_policy:tighten` (the domain already refuses anything but a tightening, so this is protective under rule 8.1); `critical` when the caller authors (platform_admin) |
| `POST /v1/admin/financial-policy-changes/{changeID}/reject\|cancel` | | none |
| `POST /v1/admin/withdrawal-policies`, `DELETE /v1/admin/withdrawal-policies/{id}` | `withdrawal_policy:write` | critical (e): thresholds and `require_step_up` (permission.go:122-127 bypass #3) |
| `POST /v1/admin/bonus/change-requests`, `POST /v1/admin/bonus/change-requests/{requestID}/decide` | bonus governance | critical (d) |
| `POST /v1/admin/casino/games/{gameID}/change-requests`, `POST /v1/admin/casino/change-requests/{requestID}/approvals` | `casino_catalogue:govern` | critical (d) |
| `POST /v1/admin/assets/change-requests`, `POST /v1/admin/assets/change-requests/{id}/decision` | `asset_registry:manage` | critical (d) |

**Payments kill switch**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/tenants/{tenantID}/payments/kill-switches` (engage) | `payments_kill_switch:engage` | none: protective, and deliberately fast for incident response |
| `POST /v1/admin/tenants/{tenantID}/payments/kill-switches/{killSwitchID}/release-requests`, `POST /v1/admin/tenants/{tenantID}/payments/kill-switch-release-requests/{requestID}/approve` | `payments_kill_switch:release` | critical (d)(e): re-opens payments, four-eyes |
| `POST /v1/admin/tenants/{tenantID}/payments/kill-switch-release-requests/{requestID}/cancel` | | none |

**Launch and market availability**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/tenants/{tenantID}/launch-authorisations` | `launch_authorisation:request` or `:suspend` | **classifier `launchActionClass`**: `none` for `action = suspend` (ADR 0112 section 6.3 asymmetry); `critical` for activate, reactivate, ratify and close |
| `POST /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/approve` | `launch_authorisation:approve` | critical (d)(e) |
| `POST /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/reject\|cancel`, `POST /v1/admin/tenants/{tenantID}/brands/{brandID}/suspend`, `POST /v1/admin/tenants/{tenantID}/suspend` | | none (protective) |
| `POST /v1/admin/tenants` | `tenant:write` | critical: a tenant is a platform-level identity boundary |
| `POST /v1/admin/tenants/{tenantID}/brands` | `brand:write` | none: a brand is born `pending_launch` |

**Secrets, providers and security signals**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/tenants/{tenantID}/provider-credential-requests`, `POST /v1/admin/tenants/{tenantID}/provider-credential-requests/{requestID}/decisions`, `POST /v1/admin/tenants/{tenantID}/provider-credential-requests/{requestID}/apply`, `POST /v1/admin/tenants/{tenantID}/provider-credentials/{handleID}/transitions` | `provider_credential:*` | critical (c). Revocation is protective, but revoking production credentials is an outage vector. |
| `PUT /v1/admin/providers/{providerID}/capability`, `PUT /v1/admin/casino/providers/{providerID}/capability` | `provider_config:write`, `casino_config:write` | critical (c)(e) |
| `POST /v1/admin/alerting/routes` | `alert:route_manage` | critical (f) |
| `POST /v1/admin/alerts/{alertID}/resolve` | `alert:manage` | **classifier `alertResolveClass`**: `critical` when the alert kind is P1 or security (at least `staff_session_integrity`, `staff_mfa_integrity`, `staff_break_glass_used`, `staff_mfa_hard_locked`, `staff_mfa_recovery_used`; the list is pinned), otherwise `none`. Resolving such an alert from a stolen session would hide the signal (f). |
| `POST /v1/admin/alerts/{alertID}/ack` | `alert:manage` | none: an ack does not close the alert |

**Regulatory, assets, risk**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/assets`, `PATCH /v1/admin/assets/{code}`, `PUT /v1/admin/assets/{code}/activation`, `PUT /v1/admin/assets/{code}/operation-eligibility`, `PUT /v1/admin/assets/{code}/platform-authorization` | `asset_registry:manage` | critical (e) |
| `PUT /v1/admin/asset-authorizations/{scopeKind}`, `PUT /v1/admin/asset-authorizations/operation-eligibility/{code}` | `asset_authorization:write` | critical (e) |
| `POST /v1/admin/jurisdictions`, `POST /v1/admin/licences`, `PUT /v1/admin/tenants/{tenantID}/licence` | `jurisdiction_registry:manage`, `tenant_licence:assign` | critical (e) |
| `PUT /v1/admin/jurisdiction-resolution-active/{operationClass}`, `PUT /v1/admin/jurisdiction-evidence-collection/{evidenceType}` | `jurisdiction_resolution_active:write`, `jurisdiction_evidence_collection:activate` | critical (e) |
| `POST /v1/admin/sportsbook/jurisdiction-restrictions`, `POST /v1/admin/sportsbook/jurisdiction-restrictions/{id}/withdraw` | `sportsbook_jurisdiction_restriction:manage` | critical (e). A withdraw loosens. Creation is protective but is grouped for legibility. |
| `POST /v1/admin/risk/rules`, `POST /v1/admin/risk/rules/{id}/disable`, `POST /v1/admin/sportsbook/exposure-limits`, `POST /v1/admin/sportsbook/exposure-limits/{id}/disable` | `risk_config:manage`, `sportsbook_exposure_limit:manage` | financial (e) |

**Bonus**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/bonus/grants`, `POST /v1/admin/bonus/manual-grants`, `POST /v1/admin/bonus/manual-grants/{grantID}/activate`, `POST /v1/admin/bonus/economic-operations`, `POST /v1/admin/bonus/bulk-jobs`, `POST /v1/admin/bonus/bulk-jobs/{jobID}/execute`, `POST /v1/admin/bonus/held-dispositions/{dispositionID}/resolve` | `bonus_grant:issue`, `bonus_adjustment:write`, `bonus_bulk:execute`, `bonus_held_disposition:resolve` | financial (a) |
| `POST /v1/admin/bonus/campaigns/{campaignID}/activate`, `POST /v1/admin/bonus/offers/{offerID}/publish` | `bonus_campaign:activate`, `bonus_offer:manage` | financial (a): releases value to many players |
| `POST /v1/admin/bonus/campaigns`, `POST /v1/admin/bonus/offers`, `POST /v1/admin/bonus/offers/{offerID}/versions`, `POST /v1/admin/bonus/suggestions/{suggestionID}/claim`, `POST /v1/admin/bonus/suggestions/{suggestionID}/decide` | bonus authoring | none: activation or publishing is the gate |

**Players, KYC, RG, catalogue**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/players/{id}/suspend`, `POST /v1/admin/rg/restrictions` | `player:suspend`, `rg_restriction:write` | none (protective) |
| `POST /v1/admin/players/{id}/reinstate`, `POST /v1/admin/players/{id}/identity-review/clear` | `player:suspend`, `identity_review:manage` | critical (e): lifts a protective control |
| `POST /v1/admin/kyc/verifications/{id}/review`, `POST /v1/admin/kyc/documents/{id}/review` | `verification:review` | none [ORCH-DEFAULT, OQ-4]. Ledger-finance verified that no production path calls `withdrawal.Approve` with `isAutomated = true`, so a KYC approval does not by itself release funds. |
| `PUT /v1/admin/casino/games`, `PUT /v1/admin/casino/games/{gameID}/availability` | `casino_catalogue:manage`, `casino_config:write` | none |

**Staff**

| Route | Permission | Class |
|---|---|---|
| `POST /v1/admin/tenants/{tenantID}/staff` (the legacy route where the admin chooses the password; it lives until S6) | `staff:manage` | critical (b) |
| `POST /v1/admin/tenants/{tenantID}/staff/{staffID}/person-link`, `POST /v1/admin/platform-staff/{staffID}/person-link` | `staff:manage` | critical (b): Person binding decides four-eyes distinctness |
| `PATCH /v1/admin/tenants/{tenantID}/staff/{staffID}/display-name`, `PATCH /v1/admin/staff/me/display-name` | | none: cosmetic, audited (ADR 0104) |
| the section 14.2 lifecycle routes, including suspend | `staff:manage`, `platform_staff:govern` | critical (b). Suspension is protective toward the target, but an attacker suspending defenders is a lock-out vector, bounded by the last-admin guard. The S6 slice adds these rows. |
| `POST /v1/staff/mfa/recovery-codes/regenerate` | own account | critical (b). The S3 slice adds this row. |

**Test support:** `POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event` is `none`. It is a test-support route
and is not mounted in production.

**Classified on arrival.** These routes do not exist yet. Each one takes its class in the slice that registers it, and
the golden test makes the change visible:

- ADR 0115 S2:
  - `POST /v1/admin/players/{id}/rg/limits`: classifier, `critical` for an increase or removal, `none` for a
    decrease. The 403 for staff increases stays until **S4b plus ADR 0115's own classifier slice**.
  - RG floor change requests, their approvals and their apply (`rg_limit_config:*`): `critical`.
  - `limit-configs` writes: `critical`.
- Clearing an RG restriction (ADR 0026 section 12): `critical`.
- Jurisdiction evaluation-policy writes and operating-market ceiling/tenant/brand policy writes. The permissions exist,
  but no HTTP route does yet. They take `critical`.
- ADR 0114 S1b, ADR 0116 AML case actions and ADR 0107 closed-tenant resolution: rule 8.1.

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
  - `rg_limit_config:request|approve|apply` (ADR 0115, already in `internal/actorproof` and migration 0135). `:apply`
    is the platform staff member's INSERT of the approved floor row (`created_by_actor_id`, migration 0135 line 829).
    That is a staff HTTP action executing a four-eyes decision, so it is `critical`, not exempt. If ADR 0115 ever moves
    `:apply` to a system worker, it must be removed from this registry in the same change. Otherwise the backstop
    turns it into a 500.
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

### 9.5 Liveness inside governed transactions (suspension race) - scoped (security ruling on review C2)

**Ruling.** `security` adopts the architect's scoped alternative. The universal re-assertion is withdrawn. Reasons:

- **It cannot work as written.** Under `WithTenant(target)` the dual-scope policy of migration 0011 hides a platform
  admin's own `staff_users` row, so every platform actor would be refused.
- **The A8 claim was overstated.** K2, K3/M4 and HSEC take the A8 staff `FOR SHARE` only on the final-approval
  execution path, and today they compare nothing.
- **The marginal gain is small.** Request-level liveness already refuses every request that starts after the
  suspension. What remains is the window of a handler that started before the suspension committed: milliseconds.
  The existing DB status guards (`financial_actor_session`, `financial_acting_session_valid`) still refuse a
  suspended actor at their own statements. That window is declared as residual R13, not as "closed".
- **This is not an escalation.** No universal check is required.

**(a) Scope.** The in-transaction re-assertion is required only where the transaction **moves value or changes
authority** and has a canonical L1 staff position. Every other step-up route relies on the request-level liveness check
and the existing DB status guards (R13).

| Flow | Where the re-assertion goes | Its L1 predecessor (ADR 0082 A7/A8) | Statement it extends |
|---|---|---|---|
| K2 manual adjustment, final approval and execution (`adjustment.DecideInTx` step 4) | the existing A8 staff `FOR SHARE` | `ledger_adjustment_requests FOR UPDATE` | add the acting approver's id to the `ANY($1)` id set; compare `status = 'active'`, `role` and `staff_version` for the actor row against the token |
| K3 / M4 force resolution, final approval and execution (`payments/manual_resolution.go:1126-1152`) | the existing A8 staff `FOR SHARE` | withdrawal or intent, then `payment_attempts`, then `payment_manual_resolutions FOR UPDATE` | the same |
| HSEC hold resolution, final approval and execution (`payments/withdrawal_hold_resolution.go:690-722`) | the existing A8 staff `FOR SHARE` | the withdrawal, then the attempts, then `withdrawal_hold_resolutions FOR UPDATE` | the same |
| Withdrawal `approve` | `approverEligibilityCheck` (`withdrawal_handlers.go:682`) becomes a `FOR SHARE` read | `withdrawal_requests FOR UPDATE` (`lockRequestForUpdate`) | that eligibility read, plus a `staff_version` and `role` comparison |
| Withdrawal `submit` and `resolve` | a new staff `FOR SHARE` | the flow's **last** preceding L1 row: withdrawal, then `payment_attempts` (A7). Never right after the withdrawal row. | new statement, at the A8 position |
| K1 capability grant approve and revoke | a new staff `FOR SHARE` | `staff_capability_grant_requests FOR UPDATE` (or the grant row on a revoke) | new statement. The grantee's row is read by the existing K1 logic. |
| Staff lifecycle execution (10.4) and the tenant lifecycle routes (10.1) | the actor's row `FOR SHARE`, ascending id with the target's row (which the change itself write-locks) | `staff_lifecycle_requests FOR UPDATE` where one exists | new statement |

- **Non-final approvals and initiations** of K2, K3 and HSEC do not move value. They get request-level liveness plus
  the DB guards (R13).
- **Flows that take an L0, L0.x advisory or R0 lock** (ADR 0082 N1) take the staff L1 lock **after** it, never as their
  first lock.

**(b) Visibility.** S4b adds one narrow permissive policy pair on `staff_users`:

```sql
CREATE POLICY staff_actor_self_select ON staff_users FOR SELECT
  USING (id = NULLIF(current_setting('app.actor_staff_id', true), '')::uuid);
CREATE POLICY staff_actor_self_lock ON staff_users FOR UPDATE
  USING (id = NULLIF(current_setting('app.actor_staff_id', true), '')::uuid) WITH CHECK (false);
```

- **Why the second policy.** `FOR SHARE` needs an UPDATE-policy-visible row. `WITH CHECK (false)` means it can never
  admit an actual UPDATE.
- **Who sets the GUC.** Only one `db` helper, `db.SetActorStaffIDForCurrentTx`, sets `app.actor_staff_id`, from the
  verified token subject. A static test pins it as the only setter.
- **Restrictive fences still apply.** The 0112 acting fence and the migration 0114 KYC-worker fence are not affected.
- **No new exposure under M5.** The runtime role can already read platform rows through `WithoutTenant`.

**(c) Amendments, landing in S4b with ledger-finance and architect sign-off:**

- **ADR 0099 A-19** (section 6.8 column discipline) admits `staff_version` in the A8 statements of K2, K3/M4 and HSEC.
  The A-19 static test is updated in the same change.
- **ADR 0082 Amendment A9** records:
  - the placement table above as canonical;
  - that the staff `FOR SHARE` stays at the A8 position (after every L1 row the flow takes, before L2/L3), and after
    any L0, L0.x or R0 lock;
  - that the actor's row joins the ascending-id A8 set;
  - that a suspension transaction locks exactly one `staff_users` row, plus `sessions` and append-only rows.

**Cycle freedom (corrected rationale).**

- The suspension transaction write-locks exactly **one** `staff_users` row and takes no L0-L3 lock. A single-row writer
  cannot form a cycle with share lockers that each follow one canonical order.
- PostgreSQL does not queue a new `FOR SHARE` behind a waiting updater when the tuple is already share-locked. The cost
  is **writer starvation**, which human traffic bounds, not deadlock. The earlier text's deadlock argument is
  withdrawn.
- The A8 position is kept for **canonical-order consistency**, not to avoid a cycle.

**Suspension phasing (two transactions).**

1. **T1** revokes all of the target's sessions, live challenges and enrolment tokens, and writes the audit row. It takes
   no `staff_users` lock and commits at once. From this point liveness refuses every new request from the target.
2. **T2** runs with `SET LOCAL lock_timeout = '5s'`. It updates `staff_users.status`, which bumps `staff_version`, and
   writes the lifecycle event. On a lock timeout it retries once. If the retry also times out, it answers 409
   `lifecycle_busy`. The sessions stay revoked (fail-safe), and the console offers the retry.

## 10. Staff lifecycle (STAFF-LIFECYCLE-1)

### 10.1 States and transitions (DB-enforced by `SF010`)

```text
invited --accept+enrol--> active --suspend--> suspended --reactivate--> active
   |                        |                    |
   +--revoke invite--> offboarded <--offboard----+  (offboarded is terminal)
```

| Transition | Who (server-side; tenant from the token, never the body) | Step-up | Four-eyes | Effects (one transaction, except suspend and offboard, which use the two-transaction phasing of 9.5) |
|---|---|---|---|---|
| create invitation (`-> invited`) | tenant staff: `staff:manage` in its own tenant, for the roles it may create today (`admin_routes.go:550-585`: tenant_admin may create `tenant_admin`, `support` and `compliance`; platform_admin may create any tenant role). Platform staff: `platform_staff:govern` through a 10.4 request. | critical | platform staff only | `INSERT staff_users(status = 'invited', password_hash = NULL, invited_by)`; an enrolment token (`invite`) is emailed; a lifecycle event; audit `staff.invited` |
| accept (`invited -> active`) | the invitee, holding the token and choosing a password (minimum length as today) | n/a | no | sets `password_hash`; consumes the token; returns a `purpose = 'enrol'` challenge. The account is `active` but **cannot get a session until a factor is confirmed**. |
| revoke invitation (`invited -> offboarded`) | same authority as the invitation | critical | no | revokes the token; a lifecycle event |
| suspend (`active -> suspended`) | tenant staff: `staff:manage` in its own tenant, but tenant_admin only for roles it may create. Platform-only roles (`finance`, `risk_manager`, `promotions_manager`, `bonus_operations`) and platform staff: platform_admin. | critical | no (protective) | `staff_version++` (`SF012`). Revoke all of the target's sessions (`auth.RevokeAllSessionsForPrincipal`), live challenges and enrolment tokens. If the reason is `security_compromise`, also revoke the factors and recovery codes. Lifecycle event and audit. |
| reactivate (`suspended -> active`) | the authority that may **grant** the target's current role. Platform staff: a 10.4 four-eyes request. | critical | platform staff | `staff_version++`. If the factors were revoked (compromise), an `mfa_reset` token is issued and emailed. Lifecycle event and audit. |
| offboard (`active\|suspended -> offboarded`) | same as suspend | critical | no | `staff_version++`. `password_hash := NULL`. Revoke all factors, recovery codes, challenges, tokens and sessions. Set `offboarded_at`. `email` and `person_id` are kept, because audit identity must stay resolvable. |
| role change (`active -> active`, role A to B) | tenant: tenant_admin only where **both** A and B are roles it may create; platform_admin any tenant role. The tenant/platform boundary cannot be crossed (`tenant_id` is immutable, `SF011`; platform_admin is the only platform role). | critical | no for tenant roles (OQ-5); a platform grant **is** an invitation through 10.4 | `staff_version++`. Revoke all sessions. `SF014` (Person required for approve-capable roles). Lifecycle event with before/after role. |
| MFA reset | tenant staff: as suspend. Platform staff: a 10.4 four-eyes request. | critical | platform staff | revoke factors and recovery codes, revoke all sessions, issue an `mfa_reset` token by email. A hard-locked factor is cleared only this way. |
| Person link (existing route) | unchanged authority | critical (new) | no | unchanged (append-only, migration 0034). `SF012` bumps `staff_version` on a `person_id` change, so the target is logged out on their next request. That is intended. |
| password reset (6.7) | as MFA reset. Platform staff: a 10.4 request of kind `platform_password_reset`. | critical | platform staff | issue a `password_reset` token by email. Redemption needs a valid TOTP. |

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
  `tenant_id IS NULL`. The trigger function is VOLATILE, so the count after the advisory lock takes a fresh snapshot.
  A catalog test pins this.
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
  cannot complete without the `cmd/seed-admin` bootstrap job (section 11).

### 10.4 Four-eyes for platform-staff changes (`staff_lifecycle_requests` and `staff_lifecycle_approvals`)

- **Kinds:** `platform_invite` (the only way to grant `platform_admin` over HTTP), `platform_reactivate`,
  `platform_mfa_reset` and `platform_password_reset`. A platform suspend or offboard is single-actor, because it is protective and bounded by
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
- **No `staff_users` guard in this ADR's core slices** (review C5). The `SF030` guard is part of the owner-deferred H1
  item, "owner-only staff lifecycle" (decision register H1/T3; ADR 0110 sections 8 and 10). It moves to **S-H1** and is
  built only if the owner answers OQ-2(a). When built, it refuses three changes unless this txid holds an `executing`
  request of the matching kind and target:
  - an INSERT of a `platform_admin` row;
  - a platform row moving `suspended -> active`;
  - an `mfa_reset` or `password_reset` token for platform staff.
- **What the core slices enforce.**
  - The four-eyes is enforced in the application, and in the DB on the governed rows themselves: the proof-bound
    `staff_lifecycle_requests` and `staff_lifecycle_approvals`, with `SF031`, `SF032` and `SF033`.
  - The `staff_users` change happens in the same transaction as the executing request.
  - A SQL-minted `platform_admin` row still cannot authenticate: it has no factor, and enrolment needs a keyed token.
- **`SF033`.** A deferred check at commit: no request is left `executing`.
- **Actor proof (ADR 0110 extension).**
  - `actor_proof_require` and `internal/actorproof` admit the prefix `staff_lifecycle:` for scope `platform` with a
    NULL tenant, in lockstep. `platform_acting` and `tenant` are refused (`AP004`).
  - The governed-table list grows by 2 (the catalog test counts).
  - `internal/stafflifecycle` joins the `TestStatic_OnlyFourEyesPackagesSign` allow-list. The security review of that
    change is mandatory.
- **What this closes, and what it does not.**
  - Closed: an HTTP actor cannot grant platform admin, reactivate platform staff, or reset a platform staff member's
    MFA or password without a second Person whose proof the DB verifies.
  - Not closed without S-H1: under M5, a SQL `UPDATE` that un-suspends a platform row, or edits the role of a tenant
    row, is accepted (R1).

### 10.5 MFA reset and hard lock

- **A hard-locked factor** (6.5) blocks login, recovery and step-up. A hard lock means the password was correct and
  10 second-factor guesses failed, which is a compromise signal. It is cleared **only** by an MFA reset under 10.1 or
  10.4. The reset revokes everything and emails an `mfa_reset`
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
- **The only escape is the existing `cmd/seed-admin`**, run as a job **inside the deployment environment**: an ECS
  task with the same secret-store injection as `platform-api`, meaning `DATABASE_URL` (the runtime role) and both
  `STAFF_MFA_*` families. It never runs on an operator workstation. In break-glass mode it:
  1. refuses unless `-break-glass` **and** `-reason` are given;
  2. refuses unless no active platform_admin with a usable factor exists, or `-force-additional` is also given;
  3. creates an `active` `platform_admin` row (with a Person, `SF014`);
  4. issues a `bootstrap` enrolment token (keyed), shown once in the task's protected output channel and never logged;
  5. writes an `owner_provisioned` lifecycle event and a platform audit row;
  6. raises the durable alert `staff_break_glass_used` (P1) to every remaining platform admin.
- **Who can launch it.** Only someone who can run tasks in the deployment environment. That is an infrastructure IAM
  control, not an application one, and it is stated as such. Under M5 it adds nothing an attacker does not already
  have.
- **Usage rule.** Two operators should be present. That is a runbook control (S9), not a software control, and it is
  stated as such.
- **Owner reversal.** OQ-6 asks whether the owner wants a governed in-app break-glass instead. The recommendation is
  no.

## 12. Residuals (stated honestly)

| # | Residual | Severity | Mitigation / owner |
|---|---|---|---|
| R1 | **M5 insider escalation.** An attacker who holds both a legitimate staff account (with its factor) and arbitrary SQL can `UPDATE` their own `role` or `status` (for example, un-suspend themselves or move from support to finance in the same tenant), then log in with their real MFA. `SF010` and `SF012` still admit a legal-looking transition. | High under M5 | **OQ-2:** an `authz_seal` column, `HMAC(staff-authz subkey, id, tenant, role, status, staff_version, person_id)`, written by the application on every lifecycle change and verified at login, refresh and liveness. This is part of H1, so it is not built until the owner authorizes it. Slice S-H1 is specified so it can follow immediately. |
| R2 | **T3 is narrowed, not closed.** For staff, MFA neutralises SQL-minted rows (no factor, no keyed token), taken-over passwords (factor AEAD) and forged sessions (seal). Player T4 and keyed refresh hashes for players are untouched. `sessions.revoked_at` is not sealed: under M5 a revoked session of an **active** staff member can be un-revoked by SQL. Using it still needs the raw refresh token, so the risk is low. | Medium | H1/T4 owner items stay OPEN. The decision register is updated in S9 to say "staff part narrowed". |
| R3 | **Factor deletion by SQL** denies service to the victim (a lock-out). It is never a bypass. | Low | Durable alert on missing-factor logins for accounts that previously had one, using the lifecycle events. MFA reset. |
| R4 | **TOTP is phishable** by a real-time proxy (AitM). | Medium | WebAuthn for platform_admin is the next factor (OQ-3). `StaffFactor` already accommodates it. |
| R5 | **Token theft inside the step-up window** (5 or 10 min) by an attacker who controls the browser. | Low-medium | Short windows. Liveness revocation. Audit. Per-session step-up failure revocation. |
| R6 | **Pending governed requests initiated by a staff member who is later suspended or offboarded** stay approvable by a colleague. The K-flow guards check the acting principal's status, not the initiator's. | Medium | The console shows the initiator's status (S8). A follow-up ADR adds an "initiator still active" check to the K1-K3 and launch guards (it changes existing guards, so it is out of scope here). |
| R7 | **Active K1 capability grants** of an offboarded staff member stay in the table. They are inert, because `financial_actor_session()` requires `status = 'active'`. | Low | The offboarding screen lists them with a revoke link (critical step-up). |
| R8 | **Email is a launch prerequisite.** `email.MockProvider` is synthetic and refused at production startup. Staff invite, MFA reset and password reset are `PROVIDER DEPENDENT` on a real adapter, which is the same dependency ADR 0030 already has. There is no production interim unless the owner picks one (OQ-1). | Launch dependency | OQ-1 |
| R9 | **The per-IP limiter is per process** (`ratelimit.go` caveats). | Low | The per-factor counters are in the DB and global, so they are the real control. |
| R10 | **Application-host compromise** (it holds the JWT, proof and MFA keys). | Inherent | ADR 0110 T6 |
| R11 | **A clock skew between the application and an authenticator** larger than ±30 s fails closed (the user cannot log in). | Low | The runbook covers NTP. |
| R13 | **In-flight window on non-scoped step-up routes.** For step-up routes outside the 9.5 placement table, a request that passed liveness just before a suspension committed can still commit. The window is the handler's run time (milliseconds). The existing DB status guards refuse at their own statements where they exist. | Low | Accepted (section 9.5 ruling) |
| R14 | **The inviter chooses the email address and the Person.** An inviter who controls the invitee mailbox gets a working account. If that account is bound to a colleague's Person, it can also defeat four-eyes distinctness for approve-capable roles. | Medium | The API no longer returns the credential. Invitations are critical step-up and audited. Platform roles need four-eyes. **Deferred consideration, not built:** a per-tenant invite email-domain allow-list as configuration. |
| R12 | **Email retention.** `email` and `person_id` are kept after offboarding for audit identity. That PII retention needs a data-retention decision. | Compliance | It is recorded. A retention/erasure policy is a separate compliance decision and does not block this ADR. |

## 13. Data model, RLS and migrations

The next free migration number is **0137**. No `migrations/0137*`, `0138*`, `0139*` or `docs/decisions/0119*` exists on
any branch (`git log --all`, checked 2026-10-13). The orchestrator allocates the final numbers, so the implementer
re-checks before S2. Every function pins `search_path` (ADR 0108). No function is `SECURITY DEFINER`, apart from the
existing ADR 0110 verifier, which is extended and not added.

### 13.1 Migration 0137 `staff_mfa` (slice S2)

1. **`staff_mfa_factors`**
   - Columns:
     - `id UUID PK DEFAULT gen_random_uuid()`
     - `tenant_id UUID NULL REFERENCES tenants(id)`
     - `staff_user_id UUID NOT NULL REFERENCES staff_users(id)`
     - `factor_type TEXT NOT NULL CHECK (factor_type IN ('totp'))`
     - `status TEXT NOT NULL CHECK (status IN ('pending','active','revoked'))`
     - `secret_kid TEXT NOT NULL`, `secret_nonce BYTEA NOT NULL CHECK (octet_length(secret_nonce) = 12)`,
       `secret_ct BYTEA NOT NULL`
     - `last_used_step BIGINT NOT NULL DEFAULT 0 CHECK (last_used_step >= 0)`
     - `failed_count INT NOT NULL DEFAULT 0`, `failed_window_start TIMESTAMPTZ`, `locked_until TIMESTAMPTZ`
     - `failed_24h_count INT NOT NULL DEFAULT 0`, `failed_24h_start TIMESTAMPTZ`, `hard_locked_at TIMESTAMPTZ`
     - `pending_expires_at TIMESTAMPTZ`
     - `created_at`, `activated_at`, `revoked_at`
     - `revoked_reason TEXT CHECK (revoked_reason IN ('replaced','reset','suspended_compromise','offboarded','pending_superseded','pending_expired'))`
     - `CHECK ((status = 'pending') = (pending_expires_at IS NOT NULL))`
   - Indexes:
     - `UNIQUE (staff_user_id, factor_type) WHERE status = 'active'`
     - `UNIQUE (staff_user_id, factor_type) WHERE status = 'pending'`
     - `(tenant_id)`
   - Triggers:
     - `sf_factor_tenant_forced` (`SF001`): BEFORE INSERT sets `NEW.tenant_id` from `staff_users` and refuses a
       supplied value that differs. A composite FK cannot cover the NULL platform arm, because MATCH SIMPLE skips rows
       with a NULL column.
     - `sf_factor_guard` (`SF002`): `id`, `tenant_id`, `staff_user_id`, `factor_type`, `secret_*` and `created_at`
       are immutable. Status may move only `pending -> active|revoked` or `active -> revoked`. `last_used_step` never
       decreases (`SF003`).
     - `sf_no_delete` (`SF099`) on DELETE and TRUNCATE.
2. **`staff_mfa_recovery_codes`**
   - Columns:
     - `id`, `tenant_id`, `staff_user_id`
     - `batch_id UUID NOT NULL`
     - `code_kid TEXT NOT NULL`, `code_mac BYTEA NOT NULL UNIQUE CHECK (octet_length(code_mac) = 32)`
     - `created_at`, `consumed_at`, `revoked_at`
   - Triggers: `SF001` and `SF099`. `sf_recovery_guard` (`SF004`) allows only `consumed_at` and `revoked_at` to move
     from NULL to a value, never both.
3. **`staff_auth_challenges`**
   - Columns:
     - `id`, `tenant_id`, `staff_user_id`
     - `purpose TEXT CHECK (purpose IN ('mfa','enrol'))`
     - `token_kid`, `token_mac BYTEA UNIQUE`
     - `staff_version_at_issue BIGINT NOT NULL`
     - `attempts_left SMALLINT NOT NULL CHECK (attempts_left BETWEEN 0 AND 5)`
     - `expires_at`, `consumed_at`, `created_at`
     - `via TEXT CHECK (via IN ('login','recovery_code','invite','mfa_reset','bootstrap'))`
   - Triggers: `SF001` and `SF099`. `sf_challenge_guard` (`SF005`) allows only an `attempts_left` decrement and
     `consumed_at` moving from NULL to a value.
4. **`staff_enrolment_tokens`**
   - Columns:
     - `id`, `tenant_id`, `staff_user_id`
     - `purpose TEXT CHECK (purpose IN ('invite','mfa_reset','password_reset','bootstrap'))`
     - `token_kid`, `token_mac BYTEA UNIQUE`
     - `expires_at`, `consumed_at`, `revoked_at`
     - `issued_by UUID NULL`, `lifecycle_request_id UUID NULL`, `created_at`
   - Index: `UNIQUE (staff_user_id, purpose) WHERE consumed_at IS NULL AND revoked_at IS NULL`.
   - Triggers: `SF001` and `SF099`. `sf_enrolment_token_guard` (`SF006`) applies the immutability rules.
     `SF007` (bootstrap tokens on the owner path only) is **reserved for S-H1**. A planted bootstrap row is useless
     anyway, because its MAC needs the key.
5. **`sessions` additions: only the nullable columns of section 7.2.** The `SF040` trigger and the staff-session
   revocation are **not** in 0137 (review C1). Today's `IssueSession` does not write those columns, so S2 must not
   break staff login.
6. **Pre-tenant lookup policies** (the ADR 0016 and migration 0040 pattern): `staff_challenge_select_by_mac`, `USING
   (token_mac = decode(NULLIF(current_setting('app.staff_challenge_lookup_mac', true), ''), 'hex'))`, and the same for
   `staff_enrolment_tokens` with `app.staff_enrolment_lookup_mac`. Two new `db.Pool` helpers follow
   `WithCredentialTokenLookup`. A row is visible only to a caller that already holds a value whose keyed MAC matches.
   The policy is SELECT-only. Any mutation, including the `FOR UPDATE` on a challenge, uses the ADR 0016 two-phase
   pattern: look up by MAC to learn the tenant, then operate in the resolved tenant or platform scope (review N5).

### 13.1a Migration 0138 `staff_session_cutover` (merges together with S3; review C1)

1. **The trigger `sessions_staff_assurance_guard` (`SF040`)**, as in section 7.2.
2. **The one-row owner table `staff_session_cutover(at TIMESTAMPTZ NOT NULL)`**, set to the migration's `now()`. The
   runtime role gets SELECT only.
3. **Revocation of every live staff session.** The migration runs as the migration role under the existing FORCE-RLS
   posture. It uses the per-row `app.session_internal_op_id` technique of ADR 0016 and asserts that the rows affected
   equal a pre-count.
4. **Deploy order (S9 runbook).** S3 code is deployed to every pod first, then 0138 runs. A pre-S3 pod that is still
   running after 0138 fails staff logins with `SF040`, which is fail-closed. A brief staff-auth maintenance window
   during the rollout is acceptable, because production has never run. The cutover rule of 7.2 keeps the
   pre-cutover sessions from raising a P1.

### 13.2 Migration 0139 `staff_lifecycle` (slice S5, merged together with S6; review C6)

1. **`staff_users`**
   - `status` CHECK widens to `('invited','active','suspended','offboarded')`.
   - `password_hash` drops NOT NULL and gains `CHECK (password_hash IS NOT NULL OR status IN ('invited','offboarded'))`
     and `CHECK (status <> 'offboarded' OR password_hash IS NULL)`.
   - New columns: `staff_version BIGINT NOT NULL DEFAULT 1`, `status_changed_at TIMESTAMPTZ`, `offboarded_at
     TIMESTAMPTZ`, `invited_by UUID NULL`.
   - **INSERT rule.** An INSERT may use `status IN ('invited','active')`. `invited` is used by the HTTP invitation;
     `active` by `cmd/seed-admin` and the test fixtures. The DB cannot tell those callers apart without the S-H1 owner
     path, so the Go layer is the control here.
   - **Merge unit (review C6).** 0139 and S5 merge **together with S6**, never earlier. S6 removes the legacy
     admin-chosen-password route (whose `person_id` is optional and whose INSERT is `active`), updates
     `cmd/seed-admin` to require a Person, and migrates every fixture that inserts approve-capable staff without one.
     The S5+S6 qa gate checks that fixture migration is complete.
   - Triggers, all BEFORE ROW, named so that they fire in this order:
     - `sf_10_staff_transition` (`SF010`): the 10.1 graph.
     - `sf_11_staff_immutable` (`SF011`): `id`, `tenant_id`, `email`, `created_at`.
     - `sf_12_staff_version` (`SF012`): sets `NEW.staff_version = OLD.staff_version + 1` when `role`, `status`,
       `password_hash` or `person_id` changes, and refuses any other change to `staff_version`.
     - `sf_13_staff_self_target` (`SF013`).
     - `sf_14_staff_person_required` (`SF014`): for an INSERT, or a role change, into
       `('platform_admin','tenant_admin','finance','compliance')`.
     - `sf_20_last_admin` (`SF020`, `SF021`).
     - `SF030` is **not** created here. It is reserved for S-H1 (section 10.4, OQ-2(a)).
   - The existing `staff_users_person_id_append_only` trigger (migration 0034) is kept.
2. **`staff_lifecycle_events`** (section 10.6). Dual-scope RLS for SELECT. INSERT only. `SF001` and `SF099`.
3. **`staff_lifecycle_requests`**
   - Columns:
     - `id`
     - `kind CHECK (kind IN ('platform_invite','platform_reactivate','platform_mfa_reset','platform_password_reset'))`
     - `target_staff_id UUID NULL` (NULL only for `platform_invite` until execution)
     - `invite_email TEXT NULL`, `invite_person_id UUID NULL`
     - `reason_code`, `note`
     - `requested_by`, `requested_by_person` (both forced)
     - `required_approvals SMALLINT` (forced to 1)
     - `payload_hash` (forced; a DB digest of the content)
     - `state CHECK (state IN ('pending','executing','executed','rejected','cancelled','expired','refused_at_execution'))`
     - `expires_at` (forced to `now() + 72h`), `created_at`, `decided_at`
   - RLS: platform session only, with no tenant arm.
   - Guards: `sf_31_lifecycle_request_guard` (`SF031`: content, forced columns and the state machine; a cancel only by
     the requester) and `zz_actor_proof_guard` (last).
4. **`staff_lifecycle_approvals`**
   - Columns: `id`, `request_id`, `decided_by`, `decided_by_person` (forced), `decision CHECK IN
     ('approve','reject')`, `reason_code`, `payload_hash`, `created_at`.
   - `UNIQUE (request_id, decided_by_person)`.
   - Guards: `sf_32_lifecycle_approval_guard` (`SF032`: the request row was locked `FOR UPDATE` in this txid; distinct
     staff and Person from the requester and the target; non-NULL Persons; the request is `pending` and not expired;
     the `payload_hash` matches). `zz_actor_proof_guard` (last). `SF099` makes the table append-only.
5. **`sf_33_lifecycle_no_executing_at_commit`** (`SF033`): a DEFERRABLE INITIALLY DEFERRED constraint trigger.
6. **ADR 0110 verifier extension.** The `staff_lifecycle:` prefix is admitted for the `platform` scope. The governed
   tables are the two above: **+2** on whatever count the catalog test finds at merge time (13 at `a3e56dd`; ADR 0115
   may add tables first). A catalog test pins `zz_actor_proof_guard` as the last BEFORE ROW trigger on
   both.

### 13.3 RLS summary

| Table | Tenant session | Platform session | Player / service / acting |
|---|---|---|---|
| `staff_mfa_factors`, `staff_mfa_recovery_codes`, `staff_auth_challenges`, `staff_enrolment_tokens`, `staff_lifecycle_events` | the dual-scope policy, identical to `staff_users` (migration 0011): own tenant's rows only | platform rows only (`tenant_id IS NULL`) when `app.tenant_id` is empty. Tenant rows are reached by `WithTenant(target)` or `WithPlatformActingInTenant`, as the existing staff handlers do. | none. Every restrictive fence that exists on `staff_users` today is replicated on each new staff table: the 0112 acting fence and the migration 0114 KYC-worker fence, plus any platform-service fence. A `platform_acting` session or a worker therefore cannot read or write a staff member's factors. A catalog test asserts that every restrictive policy on `staff_users` has a counterpart on each new staff table (review N3). |
| challenges, enrolment tokens (pre-tenant) | plus the single-row lookup-by-MAC SELECT policy | the same | n/a |
| `staff_lifecycle_requests`, `staff_lifecycle_approvals` | none | SELECT, INSERT, UPDATE (requests: state only) | none |

Every table gets `ENABLE` and `FORCE ROW LEVEL SECURITY`.

### 13.4 Grants to `igaming_runtime`

- **SELECT and INSERT** on every new table.
- **UPDATE is column-level only:**
  - factors: `status`, `last_used_step`, `failed_*`, `locked_until`, `hard_locked_at`, `activated_at`, `revoked_at`,
    `revoked_reason`
  - recovery codes: `consumed_at`, `revoked_at`
  - challenges: `attempts_left`, `consumed_at`
  - enrolment tokens: `consumed_at`, `revoked_at`
  - lifecycle requests: `state`, `decided_at`
  - `staff_users` (0139): `status`, `role`, `password_hash`, `status_changed_at`, `offboarded_at`. **Not**
    `staff_version`, which only the trigger sets.
  - `sessions` (0137): INSERT of the new columns; UPDATE of `mfa_at`, `assurance_kid` and `assurance_seal` only. No
    UPDATE of `aal`, `amr`, `auth_time` or `absolute_expires_at`, in addition to the `SF040` trigger.
- **No DELETE or TRUNCATE anywhere.** Expired rows are pruned by an owner-run runbook step, the same as actor-proof
  nonces.
- A test asserts the ACLs.

### 13.5 Pinned facts for the catalog tests

- The `SF` guards fire in the order of their names.
- `zz_actor_proof_guard` is last on the two governed tables.
- No new function is `SECURITY DEFINER`.
- `search_path` is pinned on every function.
- Every table has `FORCE ROW LEVEL SECURITY`.
- The `SF020`/`SF021` trigger function is VOLATILE.
- The restrictive-fence parity of section 13.3.
- Exactly one setter of `app.actor_staff_id` (section 9.5).
- `igaming_runtime` holds no DELETE or TRUNCATE privilege on these tables.

### 13.6 SQLSTATE class `SF` (verified free)

**Verification at `a3e56dd`:**

- Custom classes in `migrations/*.up.sql` (`ERRCODE = '…'`): AP, AR, BR, CG, GP, HR, LA, MA, MR, PC, PI, RL, SP, WD.
- Additional classes in Go string literals: HS.
- Classes in ADR texts: the same set.
- Standard PostgreSQL classes: 00-58, 72, F0, HV, P0, XX. There is no `SF`.
- `grep -rn 'SF[0-9][0-9][0-9]'` over `migrations`, `internal` and `docs` returns nothing.

**Codes (callers branch on the code only):**

| Code | Meaning |
|---|---|
| `SF001` | the tenant of a child row is not the staff member's tenant (forced or refused) |
| `SF002` | an immutable factor column changed, or an illegal factor status transition |
| `SF003` | `last_used_step` decreased |
| `SF004` | a recovery-code mutation other than consume or revoke |
| `SF005` | a challenge mutation other than an attempt decrement or consume |
| `SF006` | an enrolment-token mutation other than consume or revoke |
| `SF007` | **reserved for S-H1**: a `bootstrap` enrolment token outside the owner path |
| `SF010` | an illegal staff status transition, or leaving `offboarded` |
| `SF011` | an immutable `staff_users` column changed (`id`, `tenant_id`, `email`, `created_at`) |
| `SF012` | `staff_version` was edited directly |
| `SF013` | a self-targeted status or role change |
| `SF014` | an approve-capable role without `person_id` |
| `SF020` | the last active, enrolled platform admin would be removed |
| `SF021` | the last tenant_admin of a tenant would be removed by a tenant-scoped actor |
| `SF030` | **reserved for S-H1**: a privileged platform change without a same-txid `executing` lifecycle request |
| `SF031` | a lifecycle request content, forced-column or state violation |
| `SF032` | an invalid lifecycle approval (same staff or Person as the requester or target, NULL Person, a duplicate Person, not pending, expired, payload mismatch, request not locked in this txid) |
| `SF033` | a lifecycle request left `executing` at commit |
| `SF040` | a staff session without assurance columns, or an immutable assurance column changed |
| `SF099` | an append-only or no-delete violation |
| `AP001`-`AP005` | unchanged ADR 0110 codes on the two governed tables |

## 14. API, OpenAPI and permissions

### 14.1 Authentication and self-service (`internal/httpserver/staff_auth_routes.go`)

| Route | Auth | Rate bucket | Notes |
|---|---|---|---|
| `POST /v1/staff/auth/login` | none | `login` | **Changed response:** a `challenge`, never a `TokenPair` |
| `POST /v1/staff/auth/mfa/verify` | challenge (`mfa`) | `mfa` | returns a `TokenPair` (aal2) |
| `POST /v1/staff/auth/mfa/recover` | challenge (`mfa`) | `mfa` | returns a challenge (`enrol`) |
| `POST /v1/staff/auth/enrol/start` | challenge (`enrol`) | `mfa` | `no-store`. Returns `factor_id`, `otpauth_uri` and `secret_base32` once. |
| `POST /v1/staff/auth/enrol/confirm` | challenge (`enrol`) | `mfa` | `no-store`. Returns `recovery_codes[10]` and a `TokenPair`. |
| `POST /v1/staff/auth/enrol/redeem` | enrolment token + password | `credential` | returns a challenge (`enrol`) |
| `POST /v1/staff/invitations/accept` | invite token + new password | `credential` | returns a challenge (`enrol`) |
| `POST /v1/staff/auth/password/redeem` | `password_reset` token + new password + TOTP | `credential` | revokes all sessions and returns an `mfa` challenge (6.7) |
| `POST /v1/staff/auth/step-up` | bearer, staff chain | `mfa` | returns a new access token and `step_up_expires_at` per class |
| `GET /v1/staff/me/mfa` | bearer, staff chain | none | factor type and status, `activated_at`, recovery codes remaining, `hard_locked` |
| `POST /v1/staff/mfa/recovery-codes/regenerate` | bearer, step-up critical | none | `no-store` |
| `POST /v1/auth/refresh` | refresh token | `refresh` | staff rules of 7.2 |

### 14.2 Lifecycle administration

Tenant staff routes live under `/v1/admin/tenants/{tenantID}/staff`. Platform staff routes live under
`/v1/admin/platform-staff`. Every route is a `staffRoute` and needs a `reason_code` (plus a `note` for `other`) on
every mutation.

| Route | Permission | Step-up |
|---|---|---|
| `GET .../staff`, `GET .../staff/{id}`, `GET .../staff/{id}/lifecycle-events` | `staff:read` (new) | none |
| `POST .../staff/invitations`. This **replaces** `POST .../staff`, the admin-chosen-password route, which is removed in S6, and its OpenAPI entry with it. | `staff:manage` | critical |
| `POST .../staff/{id}/invitation/resend`, `POST .../staff/{id}/invitation/revoke` | `staff:manage` | critical |
| `POST .../staff/{id}/suspend`, `/reactivate`, `/offboard`, `/role`, `/mfa-reset`, `/password-reset` | `staff:manage`, plus the 10.1 role-authority rules | critical |
| `GET /v1/admin/platform-staff`, `GET /v1/admin/platform-staff/{id}` | `staff:read`, platform scope | none |
| `POST /v1/admin/platform-staff/{id}/suspend`, `/offboard` | `staff:manage`, platform scope | critical |
| `POST /v1/admin/platform/staff-lifecycle-requests` (kind: `platform_invite`, `platform_reactivate`, `platform_mfa_reset` or `platform_password_reset`) | `platform_staff:govern` (new) | critical |
| `GET /v1/admin/platform/staff-lifecycle-requests[/{id}]` | `platform_staff:govern` | none |
| `POST .../staff-lifecycle-requests/{id}/approve` | `platform_staff:govern` | critical |
| `POST .../staff-lifecycle-requests/{id}/reject`, `/cancel` | `platform_staff:govern` | none |

**Error codes.** These are new `apierror` codes, and the back office maps them to fixed messages (the ADR 0112 slice 5
precedent):

- `mfa_enrolment_required`, `mfa_locked`, `invalid_mfa_code`
- `invalid_challenge` (one code for unknown, expired, consumed or wrong-purpose; it never distinguishes them)
- `invalid_enrolment_token` (the same rule)
- `step_up_required` (with `max_age`), `session_revoked`
- `last_admin`, `self_target_forbidden`, `person_required`, `role_not_grantable`, `lifecycle_busy`
- `lifecycle_transition_invalid`, `four_eyes_violation`

### 14.3 Permissions

| Permission | Roles | Notes |
|---|---|---|
| `staff:read` (new) | platform_admin; tenant_admin and compliance (own tenant) | Directory and history. A tenant never sees platform staff (ADR 0114 principle 7). |
| `staff:manage` (existing) | platform_admin, tenant_admin | The 10.1 role-authority rules are enforced in the handler **and** by the existing platform-only creation list |
| `platform_staff:govern` (new) | platform_admin only | A static governance permission in the ADR 0099 section 3.2 style: not grantable through K1 and never in any tenant role. Pinned by the role-matrix test (the `adr0114_role_matrix_test.go` pattern). |

There is no permission for MFA self-service: every authenticated staff member has it over their own account only,
because the subject comes from the token.

### 14.4 OpenAPI (`docs/api/openapi/platform-api.yaml`)

- **New tags:** `staff-auth`, `staff-lifecycle`.
- **Schemas:** `StaffLoginChallenge`, `MfaVerifyRequest`, `MfaRecoverRequest`, `EnrolStartResponse` (marked
  `x-sensitive: true`), `EnrolConfirmResponse` (`x-sensitive: true`), `EnrolRedeemRequest`, `InvitationAcceptRequest`,
  `StepUpRequest`, `StepUpResponse`, `StaffMfaStatus`, `StaffDirectoryEntry` (`id`, `email`, `display_name`, `role`,
  `status`, `mfa: {enrolled, hard_locked}`, `person_linked: bool`, `last_login_at`. `last_login_at` has no column: it is derived at read time as the maximum `auth_time` over the staff member's `sessions` rows, so it is the last completed aal2 login, and NULL when no row remains.), `StaffLifecycleEvent`
  (redacted per ADR 0114 section 4.1 for tenant callers), `StaffLifecycleActionRequest` (`reason_code`, `note?`,
  `role?`), `PlatformStaffLifecycleRequest`, `PlatformStaffLifecycleDecision`.
- **Error responses.** Every staff route documents `401 session_revoked` and the step-up `403 step_up_required`
  response with its `WWW-Authenticate` header.
- **Tests.** The existing OpenAPI-vs-routes parity test, where present, covers the new routes. S6 adds them otherwise.
- **Audit semantics (review N9).** `staff.login_succeeded` keeps its meaning, "a session was issued". After S3, a
  session is only issued after the second factor. The new action `staff.login_password_verified` covers the first
  step. The ADR 0104 consumer list and any report that counts logins are told in S3.
- **Breaking change.** `POST /v1/staff/auth/login` changes its response. The back office is the only client and
  changes in S7a, which merges together with S3.

## 15. Back office screens (`backoffice/src`)

1. **Login, two steps** (`auth/LoginPage.tsx`): email, password and an optional tenant slug, then a TOTP field with a
   "use a recovery code" link. A specific message for `mfa_enrolment_required` ("ask your administrator for an
   enrolment link") and for `mfa_locked`.
2. **Enrolment** (`auth/EnrolPage.tsx`, reached from an invite or reset link `/#/enrol?token=` or from the recovery
   flow):
   - Set the password (invite only).
   - Show a QR code **rendered locally** from `otpauth_uri` (a bundled QR library; **no network QR service**), plus a
     manual secret behind a reveal toggle.
   - Confirm the code.
   - Show the recovery codes once, with copy and download (`.txt`). The user must tick "I have stored these codes"
     before continuing.
   - The codes are not kept in application state after leaving the page and are never written to `localStorage`.
3. **Step-up modal** (`auth/StepUpProvider.tsx`): a fetch-layer interceptor.
   - On `403 step_up_required` it opens a modal with a TOTP field and calls `/v1/staff/auth/step-up`.
   - It swaps the access token in `tokenStore`, then **re-sends the original request once with the same frozen
     body**. This is safe because a step-up refusal happens before any write (section 6.6). Bonus routes re-send their
     own `Idempotency-Key`.
   - It never retries more than once. On cancel, the original action fails visibly.
   - Every governed dialog (ADR 0114: approve or reject with a frozen `payload_hash`) works unchanged, because the
     retry resends the frozen payload.
4. **My security** (`features/account/SecurityPage.tsx`): factor status, recovery codes remaining (warn at 3 or fewer),
   regenerate (step-up), and own sessions (the existing `/v1/me/sessions`).
5. **Staff directory** (`features/staff/StaffDirectoryPage.tsx`):
   - Filters for status, role and MFA state (closed vocabularies).
   - Tenant callers see their own tenant only. Platform callers get a tenant picker, plus a "Platform staff" tab.
6. **Staff detail and lifecycle** (`features/staff/StaffDetailPage.tsx`):
   - Actions shown according to `permissions.ts` hints. The server is authoritative.
   - Reason-code dialogs. A confirmation that names the effects ("revokes all sessions").
   - The lifecycle history, and active K1 grants with a revoke link (R7).
   - The initiator's status on the target's pending governed requests (R6).
   - Disabled self-target actions (a hint only).
7. **Invite** (`features/staff/InviteStaffDialog.tsx`): email, role (only the roles this caller may grant), and Person
   (required for approve-capable roles). It states clearly that the invite is emailed and the inviter never sees a
   password or token.
8. **Platform staff four-eyes queue** (`features/staff/PlatformStaffRequestsPage.tsx`): the ADR 0112 launch-queue
   pattern. The caller's own requests have approve disabled (a hint). The `payload_hash` is frozen.

Every screen uses the ADR 0114 principles 1, 4 and 7, and the code-to-message map (server text never shown).

## 16. Slice plan (dependency order; one branch per slice; nothing merges without its gate)

**Gate G** applies to every slice. It needs:

- a **live** review by `security`, meaning a reviewer runs the tests against a private scratch database as
  `igaming_runtime`, not a paper review;
- a review by `code-reviewer`;
- a `qa` gate: race detector, integration tag, mutation evidence for the listed mutants, and the
  `docs/plans/.../adr0119-sN-mutation-kill.txt` evidence file.

Extra reviewers are named per slice.

| Slice | Content | Depends on | Extra review |
|---|---|---|---|
| **S0** | This ADR (revision 2). Pointers in ADR 0017 and ADR 0115. OQ-1 to OQ-6 entered in `docs/governance/human-decision-register.md`. Architect and ledger-finance re-check section 9.5 as rewritten. | - | architect, ledger-finance |
| **S1** | `internal/staffmfa`, pure Go with no DB: TOTP (RFC 6238 Appendix B vectors), the `StaffFactor` interface, the two key families with HKDF subkeys (`label \|\| kid`), AEAD with AAD, keyed MACs, recovery-code generation and normalisation, redacting types, `config.Load` validation of `STAFF_MFA_*` and the TTL bounds, the pairwise **platform key-family registry** (5.1), `VerifyStartup`. **The key-provisioning runbook** (staging needs keys before S3 deploys). | S0 | - |
| **S2** | Migration 0137 (13.1, **without** `SF040` and without the session revocation), the store layer, the two-phase lookup helpers in `internal/db`, the restrictive-fence replication and its parity test, and integration tests for every `SF00x` code, the RLS, the ACLs and the catalog pins. Staff login on `main` is unchanged. | S1 | - |
| **S3 + S7a** (one merge unit) | The authentication flows of section 6: login challenge, verify, recover, enrol start/confirm/redeem, step-up endpoint, password redeem. The new claims, the session seal, the staff refresh rules with the cutover rule, the staff TTLs and the `mfa` limiter bucket. The `internal/auth/authtest` helper, and every existing staff-token fixture migrated to it. `cmd/seed-admin` issues a `bootstrap` token and runs as a deployment job. **Migration 0138** (13.1a). **S7a:** back office two-step login, enrolment and My security. They merge to `main` **together**, because a `main` that cannot log in to the back office breaks every developer and every E2E test. | S2 | qa (fixture-migration completeness) |
| **S4a** | `staffRoute`, `RequireStaffPrincipal`, `RequireLiveStaffSession` (N1 implementation), `RequireAAL2`. Re-register every `/v1/admin` route through `staffRoute` with an explicit class. In S4a the class is only recorded; enforcement comes in S4b. The static coverage test. **Route-registration freeze:** announced to the ADR 0114 S1b, ADR 0115 S2 and ADR 0116 owners. Any route merged after S4a must use `staffRoute` with a class. | S3 + S7a | - |
| **S4b + S7b** (one merge unit) | `RequireStepUp` and the three classifiers. The golden table (8.2). The signer backstop with the corrected registry (9.1). The `withdrawal.Approve` step-up parameter (9.3). The scoped in-transaction re-assertion and the `staff_actor_self_*` policies (9.5), with **ADR 0082 Amendment A9** and the **ADR 0099 A-19 amendment** written in the same change. **S7b:** the back office step-up modal. | S4a | **ledger-finance (mandatory:** A9, the placement table, K-flows), architect (A-19, the RLS policy) |
| **S5 + S6** (one merge unit, review C6) | Migration 0139 (13.2, without `SF030`), `internal/stafflifecycle` (domain plus proof signing), the ADR 0110 verifier and `actorproof` lockstep extension (`staff_lifecycle:`, +2 governed tables), and the `TestStatic_OnlyFourEyesPackagesSign` allow-list. **S6:** the lifecycle HTTP API (14.2), invitation and reset email through `email.Provider`, removal of the legacy admin-chosen-password route, `cmd/seed-admin` requiring a Person, the fixture migration, the new permissions and the role-matrix test, OpenAPI (14.4), and `cmd/seed-admin -break-glass` (section 11). S5 can be developed in parallel after S2, but it merges only together with S6. | S4b | architect (ADR 0110 extension), identity-compliance (reason codes, policy) |
| **S8** | Back office: staff directory, detail and lifecycle, invite, and the platform four-eyes queue (15.5-15.8). It follows the ADR 0114 console conventions (principles 1, 4 and 7; the code-to-message map). | S5 + S6 | - |
| **S9** | Runbooks: FACTOR re-enrolment campaign, MAC rotation and recovery-code regeneration, lost device / password / both, break-glass as a deployment job, the 13.1a deploy order, NTP, pruning. The launch-gate checklist update. Decision-register and threat-register updates: T3 "staff part narrowed"; R1 to R14. | S8 | - |
| **S-H1** (conditional) | `SF030` and `SF007` (OQ-2(a)), and the `authz_seal` on `staff_users` (R1, OQ-2(b)). **Built only for the parts the owner authorizes.** | S5 + S6, and the owner | security, architect |

- **Critical path:** S0 → S1 → S2 → (S3+S7a) → S4a → (S4b+S7b) → (S5+S6) → S8 → S9.
- **ADR 0115:** staff RG increases stay 403 until **S4b plus ADR 0115's own classifier slice**.
- **Labels until each slice merges:** every item is `NOT IMPLEMENTED`. After merge, the label is `IMPLEMENTED` against
  the local DB with MOCK email. Real email delivery is `PROVIDER DEPENDENT` and gates production startup (R8, OQ-1).
  Production key provisioning is `PROVIDER DEPENDENT` on the operator, as for ADR 0110.

## 17. Test plan (`-race -tags integration`, as `igaming_runtime`, private scratch databases)

### 17.1 Authorization matrix

- **Every lifecycle route.** Each lifecycle route × every role (platform_admin, tenant_admin, support, compliance,
  finance, risk_manager, promotions_manager, bonus_operations, player, service) × target (own tenant, other tenant,
  platform row, self) gives the expected 2xx, 403 or 404. A cross-tenant target is **404, never data**. A tenant caller
  can never reach a platform row (404).
- **Role-grant authority.** tenant_admin cannot invite or role-change into `finance`, `risk_manager`,
  `promotions_manager` or `bonus_operations` (403 `role_not_grantable`), and cannot suspend those roles.
  platform_admin can.
- **Platform governance.** `platform_staff:govern` is held by platform_admin only (role-matrix golden).
- **Self-target.** Refused by both the HTTP layer and `SF013`, tested separately by bypassing Go with direct SQL that
  sets the GUC.
- **Directory leakage.** A tenant caller never sees platform staff ids, names or emails in any DTO (the ADR 0114 H1
  helper applies).
- **The full chain on every staff route.** The static completeness test plus a table-driven HTTP test check that each
  of these gets 401 or 403 and never reaches the handler: a player token, a service token, an aal-less staff token
  (minted directly by a test-only issuer), a revoked session, a stale `sv`, and a stale role.

### 17.2 Step-up

- **Golden table.** For every route classified in section 8: `mfa_at` absent gives 403 `step_up_required` with
  `max_age` and the RFC 9470 header; `mfa_at = now - (maxAge + 1 s)` gives 403; `mfa_at = now - (maxAge - 1 s)` passes
  the gate. A route classified `none` never asks.
- **Classifier routes.** Launch `action = suspend` needs no step-up, while `activate` needs one. An RG decrease needs
  none, while an increase needs one. A forged `action` change between the classifier and the handler is impossible,
  because the handler re-uses the classifier's parsed body. A test covers that.
- **Signer backstop.** A test route deliberately registered without `RequireStepUp` that signs `ledger_adjustment:
  approve` returns 500 `ErrStepUpMissing`, and no row is written. An assertion for a different subject is refused.
- **Withdrawal policy.** With `require_step_up` above the threshold: fresh at 3 min passes the route but fails the
  policy (403, `max_age = 120`); at 1 min it succeeds. `isAutomated` never satisfies it.
- **Modal retry safety** (BO test plus an API test, for a four-eyes approve route and a bonus route): the refused
  attempt leaves no state change, no proof nonce consumed and no audit success row. The single re-send after step-up
  produces exactly one effect. A second re-send gets the route's normal state-based 409.

### 17.3 Replay

- The same code twice within its step gives one success and one 401.
- A code from step T-1 after step T was accepted is refused (monotonic).
- Two concurrent verifies with the same valid code (goroutines with a barrier) give exactly one success, and
  `last_used_step` is set once.
- A challenge token reused after success is refused (`invalid_challenge`). A challenge for staff A presented with
  staff B's code fails. An `enrol` challenge used at `mfa/verify` is refused, and the reverse too.
- An enrolment token reused, expired, revoked, or for the wrong purpose is refused with the identical
  `invalid_enrolment_token`.
- Recovery codes: a code used twice gives one success. Concurrent use gives one success.

### 17.4 Brute force and lockout

- 5 wrong codes trigger the soft lock. The 6th attempt, even with the **correct** code, returns `mfa_locked` with no
  comparison (timing-independent body).
- After `locked_until` passes, the counter window resets.
- 10 failures in 24 h hard-lock the factor and raise the alert, and only an MFA reset clears it.
- **Parallel guessing.** 50 concurrent wrong verifies against one factor leave `failed_count` exact and never exceed 5
  comparisons before the lock. This proves the `FOR UPDATE` serialisation.
- Per-IP `mfa` bucket: the 31st request in a minute gets 429.
- Recovery-code and step-up failures share the factor budget. 5 step-up failures revoke the session.

### 17.5 Suspension and session races

- **Suspend vs active session.** Suspend B, then B's next request with a still-unexpired access token gets 401
  `session_revoked`. B's refresh gets 403. B's step-up gets 401.
- **Suspend during an in-flight governed write.** Approver B starts a K2 approval; the test pauses after the request
  `FOR UPDATE` using the existing lock-wait harness (`casino_lock_wait_harness_test.go` pattern). A suspends B and
  commits, or blocks on B's `FOR SHARE`. Both orders are tested:
  - B committed first: B's approval stands and is audited before the suspension.
  - The suspension committed first: B's re-assertion sees `suspended` and nothing is written.
  - There is no deadlock in either order.
  - The static lock-order suite asserts the A9 placement for every flow in the 9.5 table. A B-7-style contention test
    runs N concurrent approvers against one suspension. It asserts the suspension completes within its `lock_timeout`
    plus one retry, or answers `lifecycle_busy` with the sessions already revoked.
  - A platform admin acting under `WithTenant(target)` passes the re-assertion through `staff_actor_self_select`.
    Without the GUC, the row is invisible and the request is refused (the policy is pinned by test).
- **Role change mid-session.** The old token gets 401 (role/`sv` mismatch). A refresh issues the new role.
- **Last admin.** Two platform admins suspend each other concurrently: exactly one succeeds, and the other gets
  `SF020`. A single admin offboarding the only other enrolled admin is refused. Platform removal of a tenant's last
  tenant_admin succeeds, while tenant-scoped removal is refused (`SF021`).

### 17.6 Integrity under M5 (direct SQL as `igaming_runtime`)

Each of these attacks is attempted by direct SQL, and each must fail:

- **A planted factor.** A factor INSERT with an attacker-encrypted secret fails to verify: AEAD open fails, the alert is
  raised and the attempt counts as a failure.
- **Moved ciphertext.** Victim ciphertext copied onto another staff member's factor row fails.
- **Planted MAC rows.** A recovery code, challenge or enrolment token with an unkeyed or wrong-key MAC is never found.
- **A forged staff session** with `aal = 2` and a random seal: refresh gives 401, an audit row and the P1 alert.
- **A deleted factor.** Login gives `mfa_enrolment_required`: no bypass, and enrolment without a keyed token is
  impossible.
- **Platform admin INSERT, or un-suspend of a platform row**, is **accepted** without S-H1. Pinned as
  `..._Residual_R1_Platform_...`. It turns into an `SF030` refusal test if S-H1 is built.
- **A lifecycle approval INSERT without a proof** gives `AP001`. One with the same Person gives `SF032`.
- **Residual R1 is pinned.** A SQL `UPDATE` of the attacker's own tenant row from support to finance is **accepted**,
  and their next real-MFA login yields finance. The test is named `..._Residual_R1_...`, exactly like ADR 0110's
  residual test, so the gap is visible until S-H1.

### 17.7 Secrets hygiene

- A log-capture test runs the full enrol, verify, recover, step-up and invite flows with a `slog` handler that records
  everything. The secret, the base32 secret, the URI, every code and every token never appear.
- Audit-metadata scan (static plus runtime).
- `no-store` headers on the sensitive responses.
- `fmt` / `slog` / JSON redaction of `EnrolmentMaterial` and `Assertion`.
- The key-family config is refused when it equals the JWT, proof, payout or fingerprint keys.

### 17.8 Mutation plan (each mutant must be killed; evidence file per slice)

| Mutant | Killing test |
|---|---|
| `>` changed to `>=` in the replay rule | 17.3 same-code-twice |
| window widened to ±2 steps | TOTP window unit test (T±2 refused) |
| `FOR UPDATE` dropped from the factor read | 17.4 parallel guessing |
| AAD omits `staff_user_id` (or `tenant_id`) | 17.6 moved ciphertext |
| seal not verified on refresh | 17.6 forged session |
| `absolute_expires_at` not copied on rotation | refresh-chain beyond 12 h test |
| liveness skips the `sv` comparison | 17.5 role change mid-session |
| `RequireStepUp` uses the token `iat` instead of `mfa_at` | 17.2 stale `mfa_at` with a fresh token |
| class `critical` swapped to `financial` | golden table test |
| signer backstop removed | 17.2 backstop test |
| `FOR SHARE` re-assertion removed | 17.5 in-flight governed write |
| advisory lock removed from `SF020` | 17.5 concurrent mutual suspend |
| cutover rule raises P1 for pre-cutover NULL seals | the 0138 cutover test |
| `staff_actor_self_lock` allows UPDATE (`WITH CHECK` dropped) | an RLS test asserting UPDATE through the GUC is refused |
| hard lock still issues a challenge | 17.4 hard-lock test (no challenge, recovery unreachable) |
| `SF032` Person check removed | the same-Person approval test |
| recovery code grants a session instead of an enrol challenge | the recovery flow test |
| challenge not consumed on success | 17.3 challenge reuse |
| locked factor compares before refusing | 17.4 correct-code-while-locked test |
| the invite response includes the token | the API test asserting the response schema has no token field |

## 18. Owner questions (minimal; none blocks; each has a recommended answer and a default already applied)

| # | Question | Recommended answer | Default applied if unanswered |
|---|---|---|---|
| OQ-1 | A real email adapter is already a launch prerequisite: `email.MockProvider` is synthetic and refused at production startup. Staff invite, MFA reset and password reset depend on it. Does the owner want a **pre-email interim**? Option (a): a non-synthetic "email disabled" provider that makes invite and reset answer 503, listed in the launch-readiness gate, plus a scoped `cmd/staff-admin issue-token` job. That job runs inside the deployment environment with the runtime DB role and `STAFF_MFA_*`, **not** owner credentials. It only delivers a token for an invite or reset that an authorized, step-up-verified admin already created over HTTP, and the operator delivers the token out of band. Option (b): no interim. | (b): no interim. Contract the email provider before launch, which ADR 0030 needs anyway. | (b). Staff onboarding beyond the bootstrap admins is blocked in production until a real email adapter exists. Development and staging use the MOCK provider. |
| OQ-2 | H1 partial authorization: (a) build `SF030` and `SF007`, which make `platform_admin` creation, platform reactivation and platform reset four-eyes-only at the DB with an owner path for bootstrap; (b) build the `staff_users.authz_seal` that closes residual R1. | (a) Yes. (b) Yes. | **Neither is built** (review C5: an orchestrator default must not override the owner's H1 deferral). S-H1 waits. R1 stays OPEN in the threat register. |
| OQ-3 | Factor roadmap: TOTP now; WebAuthn (phishing-resistant) for platform_admin, and when? | TOTP at launch for all staff. WebAuthn mandatory for platform_admin within the first post-launch hardening cycle. | TOTP only. `StaffFactor` is ready for WebAuthn. |
| OQ-4 | Step-up scope and max-ages (section 8, 7.4), notably: KYC review without step-up, and `financial` at 10 min. | Accept as defaulted. Revisit after 30 days of real operator usage. | Section 8 as written. |
| OQ-5 | Four-eyes depth: one approver for platform-staff changes (two Persons in total); tenant-role changes single-actor with step-up. | Accept. Raise to two approvers only if the platform admin team grows beyond 5. | As written. |
| OQ-6 | Break-glass: is there no in-application break-glass, with the `cmd/seed-admin -break-glass` deployment job as the only escape? | Yes. | As written. |

**Status rationale.** No question blocks a slice:

- OQ-1 only affects production staff onboarding, which fails closed.
- OQ-2 gates only S-H1.
- The others confirm reversible defaults.

The ADR therefore stays `ACCEPTED - PENDING IMPLEMENTATION`. S0 enters the six questions in the decision register.

## 19. Consequences

- **Every staff login** needs a factor. Existing dev and staging staff must be re-seeded or re-invited when S3 lands,
  and migration 0138 revokes every staff session then.
- **Each staff request** costs one extra short read-only transaction (liveness, 500 ms timeout).
- **High-risk routes** re-prompt for a TOTP after 5 or 10 minutes.
- **Admin-chosen staff passwords disappear** in S5+S6.
- **`platform_admin` grants become four-eyes** over HTTP. They need at least two enrolled platform admins with
  distinct Persons. Otherwise only the `cmd/seed-admin` bootstrap job can add one.
- **The governed-table count** under ADR 0110 rises by 2.
- **New:** SQLSTATE class `SF`; key families `STAFF_MFA_FACTOR_KEYS` and `STAFF_MFA_MAC_KEYS`; one platform
  key-family registry.
- **Amendments:** ADR 0082 A9 and ADR 0099 A-19, both in S4b.
- **ADR 0017** is superseded in part, and its pointer line names this ADR.
- **ADR 0115** S2 staff increases unblock at S4b plus ADR 0115's own classifier slice. ADR 0115 carries a pointer.
- **What remains NOT closed** (must not be reported as closed):
  - T3 for players (T4);
  - residual R1, including its platform half, until S-H1;
  - T5 and T8;
  - R13 and R14;
  - the inherent residuals R10 and R11.

## 20. Review disposition (revision 2, 2026-10-13)

The source is the S0 design review by `architect` and `ledger-finance`: ACCEPT WITH CONDITIONS (orchestrator scratchpad
`reconciliation/adr0119-design-review.md`).

| Item | Disposition | Where |
|---|---|---|
| C1 S2 breaks staff login | `SF040`, the cutover marker and the staff-session revocation move to migration 0138, which merges with S3. The cutover rule: a NULL seal on a pre-cutover session is a plain not-found with no P1. The deploy order is in S9. | 7.2, 13.1 item 5, 13.1a, 16 |
| C2 universal `FOR SHARE` re-check | **Security ruling: scoped alternative adopted. Universal check withdrawn; no escalation.** The re-assertion applies only where value moves or authority changes. It is folded into the existing A8 statement for K2, K3/M4 and HSEC final approval, and covers withdrawals (`approverEligibilityCheck`; submit and resolve at the A8 position after attempts), K1 and lifecycle. A narrow `app.actor_staff_id` self-select and lock policy pair. ADR 0099 A-19 and ADR 0082 A9 amendments in S4b. Placement table. Corrected cycle rationale. Two-phase suspension with `lock_timeout`. R13 states the remaining window. | 9.5, 12, 16, 17.5 |
| C3 rewrap contradicts SF002 | No rewrap: the FACTOR family rotates by re-enrolment only. The AEAD key and the MACs are split into two families, so recovery codes no longer hold the factor key. HKDF `info` is `label \|\| kid`. | 5.1-5.3 |
| C4 OQ-1 interim unreachable | Restated: real email is a launch prerequisite (the mock is synthetic and refused at startup). The interim, if the owner wants one, is an explicit "email disabled" provider plus a scoped deployment-environment CLI under the runtime role, never owner credentials. Default: no interim. `cmd/seed-admin` runs as a deployment job holding `STAFF_MFA_*`. | 6.3, 11, 12 R8, 18 OQ-1 |
| C5 SF030 is owner-deferred H1 | `SF030` and `SF007` move to S-H1, built only on an OQ-2(a) answer. Section 10.4 "What this closes" is rewritten. The R1 platform-half test is pinned. | 10.4, 13.2, 13.6, 17.6, 18 |
| C6 S5 breaks legacy create, seeding and fixtures | S5 merges together with S6 (option i). The INSERT rule and the fixture-migration gate are stated. | 13.2, 16 |
| C7 golden table | Exact registered paths only. Legacy staff-create is `critical`. Kill-switch paths corrected. Non-existent routes are moved to "classified on arrival". Alert resolve uses a classifier (`critical` for P1 and security kinds). The tenant financial-policy route uses a classifier (`none` for tighten-only). The golden table, not the compiler, is the control. KYC stays `none`. | 7.3, 8.2 |
| C8 retry claim | The retry is safe because the refusal precedes any write. There is no `Idempotency-Key` claim for four-eyes routes. The test is rewritten. | 6.6, 15, 17.2 |
| C9 hard lock vs recovery | A hard lock is cleared only by an MFA reset; no challenge is issued. Lost password: a `password_reset` token that needs a valid TOTP. Lost both: offboard and re-invite. | 6.1, 6.7, 10.1, 10.5, 13.1, 14 |
| C10 registry names and counts | `rg_limit_config:request\|approve\|apply`. `:apply` is a staff execution, so it is `critical`, not exempt. "+2 governed tables". | 9.1, 13.2, 19 |
| C11 overclaims | The invitation claim is restated. R14 covers the inviter choosing the mailbox and Person, with an email-domain allow-list recorded as a deferred consideration. `sessions.revoked_at` unsealed is added under R2. Item 5 of section 2 is qualified. | 2, 3, 6.3, 12 |
| N1 liveness cost | Batched read-only transaction, 500 ms timeout, released before the handler's connection, 503 on error, metric and alert. | 7.3 |
| N2 step-up on a replaced row | Chain head only, and the reuse-detection side effect is stated. | 6.6 |
| N3 fences | Parity with every restrictive fence on `staff_users`, plus a catalog test. | 13.3 |
| N4 grants | `staff_users` and `sessions` column grants. | 13.4 |
| N5 two-phase lookup | Stated. | 13.1 item 6 |
| N6 VOLATILE | Pinned. | 10.3, 13.5 |
| N7 key distinctness | The pairwise registry includes `PAYOUT_INSTRUMENT_FP_KEYS`. `cmd/seed-admin` needs the keys and runs as a deployment job. | 5.1, 11 |
| N8 `last_login_at` | Derived from sessions. | 14.4 |
| N9 audit semantics | `staff.login_succeeded` keeps "session issued". The new `staff.login_password_verified` is announced to ADR 0104 consumers. | 14.4 |
| N10 person-link logout | Stated. | 10.1 |
| N11 ADR 0115 pointer | Added in this revision. | ADR 0115 |
| N12 owner questions in the register | In S0, done in this revision. | `human-decision-register.md` |
| N13 SQLSTATE and migration numbers | Confirmed. 0137, 0138 and 0139 are to be re-allocated at dispatch. | 2, 13 |
| N14 federated SSO | Deferred consideration (below). | 20 |
| N15 tenancy, jurisdiction, provider fit | No change needed. | - |

**Deferred considerations (recorded, not built).**

- Federated SSO (OIDC or SAML) for external B2B operator staff. `StaffFactor` and `amr` do not preclude it. It would
  need its own ADR.
- A per-tenant invite email-domain allow-list as configuration (R14).

## 21. S1 implementation record (2026-10-13)

Branch `worktree-agent-aa94adf845a9e85f3`, based on `b480ec6`. **NOT MERGED.** Gate G (live `security` review,
`code-reviewer`, `qa`) has not run. Nothing below is an approval.

### 21.1 Status labels

| Item | Label |
|---|---|
| S1 library: `internal/staffmfa` (TOTP in `internal/staffmfa/totp`, the two key families with HKDF subkeys, factor-secret AEAD with the section 5.3 AAD, keyed hashes, challenge/enrolment tokens, recovery-code generation and normalisation, the section 6.5 counters and policy constants as pure functions, redacting secret types, the `StaffFactor` interface, `VerifyStartup`), the `config.Load` validation of `STAFF_MFA_*`, the section 7.4 bounds and `STAFF_MFA_ISSUER`, the platform key-family registry, and the key-provisioning runbook `docs/runbooks/staff-mfa-keys.md` | `IMPLEMENTED` |
| A `StaffFactor` implementation (store-backed TOTP factor) | `NOT IMPLEMENTED` (S2/S3) |
| `VerifyStartup` wired into `cmd/platform-api` and `cmd/seed-admin` | `NOT IMPLEMENTED` (S3) |
| Session assurance seal (the `ss-v1` canonical form) | `NOT IMPLEMENTED` (S3; S1 only derives its subkey) |
| Migrations 0137/0138/0139, store layer, every HTTP route, login/step-up/lifecycle flows, back office, runbook campaigns of S9, S-H1 | `NOT IMPLEMENTED` |
| Production key provisioning | `NOT IMPLEMENTED` (`PROVIDER DEPENDENT` on the operator and the deployment secret store; AWS is OFF) |

Staff login is unchanged: nothing in the running platform reads the new keys or calls the library.

### 21.2 Decisions taken inside S1 (reversible; for the reviewer)

1. **Token wire format carries the staff id.** Section 5.3 binds `staff_user_id` into every keyed hash, but section
   13.1 item 6 looks challenges and enrolment tokens up by MAC before the staff member is known. To satisfy both, a
   token is `base64url(staff_user_id[16] || random[32])` (64 characters). The lookup computes the MAC with the
   embedded id; `VerifyTokenHash` also requires the found row's `staff_user_id` to equal it. The id is not secret
   from the holder (that staff member). Endpoints that accept more than one purpose (`/enrol/redeem`:
   `mfa_reset` or `bootstrap`) compute one lookup per purpose (S3).
2. **Environment variable names.** Section 7.4 names only `STAFF_REFRESH_IDLE_TTL`. S1 uses the repository's
   `_SECONDS` convention for all five bounds: `STAFF_ACCESS_TOKEN_TTL_SECONDS`, `STAFF_REFRESH_IDLE_TTL_SECONDS`,
   `STAFF_SESSION_ABSOLUTE_TTL_SECONDS`, `STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS`,
   `STAFF_STEP_UP_FINANCIAL_MAX_AGE_SECONDS`.
3. **Where "required" is enforced.** `config.Load` refuses missing keys only when `APP_ENV=production` (explicit).
   `VerifyStartup` refuses missing keys in every environment, as section 5.1 says, but it is wired in S3: wiring
   it now would stop every development and test environment while nothing uses the keys. Until S3 a deployment
   with `APP_ENV` missing loads without keys (residual S1-R1). The placeholder refusal uses `GuardEnvironment()`, so
   a missing `APP_ENV` is treated as production there.
4. **Registry scope.** The registry checks every pair of the eight families, in every form a key can be reused in
   (security review C3): a text key (`JWT_*`, `PROVIDER_CREDENTIAL_FINGERPRINT_KEY`) as its text AND as its base64
   decoding when it decodes; a kid-list entry as its decoded bytes AND as its encoded text. So
   `JWT_SIGNING_SECRET=X` with `STAFF_MFA_MAC_KEYS=m1:X` is refused. A value repeated inside one family is
   each family's own rule (the staff families refuse it; `ACTOR_PROOF_KEYS` still admits it, unchanged). **Behaviour
   change:** the two JWT secrets are separate registry families, so `JWT_PREVIOUS_SECRET == JWT_SIGNING_SECRET` is
   now refused at startup.
5. **`password_reset` token TTL.** Not stated by the ADR; S1 uses the MFA-reset lifetime (24 h), because section
   6.7 gives it the MFA-reset authority.
6. **Counter semantics (section 6.5, literal).** The windows are fixed (start + count, the columns of 13.1), not
   sliding. A success resets the 15-minute window only, never the 24-hour count. Setting a soft lock restarts the
   15-minute window. Inconsistent counters are `LockInvalid`, which refuses like a lock. A clock that goes
   backwards keeps a window open.
7. **Replay rule detail.** When two window candidates share a code (a 1e-6 event), the highest step is burned.
8. **Strict inputs.** A TOTP code is exactly 6 ASCII digits (the UI strips spaces). Key lists refuse empty entries
   (a trailing comma), keys over 1024 bytes and duplicate values. Recovery-code input is ASCII only, at most 64
   bytes before normalisation.
9. **Published placeholders.** `.env.example` now publishes commented-out development placeholders for both
   families; they, and the two published fingerprint placeholders, are refused in either staff family unless
   `APP_ENV` is explicitly `development`.
10. **Exception list.** `config.Config.Environment`'s list of environment-gated security controls gains item 9 for
    this rule.
11. **Secret material behind unexported closures (security review C2).** `FactorSecret`, `Assertion`,
    `EnrolmentMaterial`, `Token`, `RecoveryCode` and `Keys` hold their material only behind an unexported
    `func() data` field. fmt and slog cannot call methods on an unexported field of another struct and render it
    by reflection; a plain pointer is not enough, because fmt's bad-verb path (`%s`, `%d`, `%x` on a pointer)
    prints the pointee. A func value renders as an address under every verb. Tests cover structs that hold every
    type in unexported fields, nested, in slices, maps and interfaces.
12. **Degenerate keys.** A staff MFA key whose bytes are all equal (for example all zero) is refused.
13. **otpauth escaping.** Issuer and account are percent-encoded with a space as `%20` in both the label and the
    query; `&`, `=`, `+`, `@`, `/`, `?` and `#` are always encoded.
14. **Duration settings.** The `_SECONDS` values are range-checked as integers before they are multiplied into a
    `time.Duration` (`18446744134` would otherwise wrap to about 60 s and pass a bound).

### 21.3 Changed existing tests and assertions

No existing assertion was changed. Test setup changed so existing production-shaped tests keep testing what they
tested:

- `internal/config/config_test.go`: `clearEnv` clears the new variables; `TestLoad_EnvironmentExplicit_TrueWhenSet`,
  `TestLoad_ValidAppEnvValuesAccepted` and `TestLoad_ProductionWithTestSupportEndpointsDefaultSucceeds` call
  `setStaffMFAKeys(t)` before `Load`.
- `internal/config/fingerprint_key_test.go`: `baseEnv(t, "production")` also sets fresh staff MFA keys.
- `cmd/platform-api/synthetic_guard_ordering_integration_test.go` (integration tag): the `APP_ENV=production`
  subprocess gets fresh staff MFA keys and drops inherited `STAFF_MFA_*`, so it still fails on the synthetic guard
  and not on configuration.

### 21.4 Evidence

- Unit tests (no database): `internal/staffmfa/totp` (RFC 6238 Appendix B SHA-1 vectors at 8 digits and their
  6-digit low-order form, RFC 4226 Appendix D, window exactly plus or minus 1, replay strictly greater, a pinned
  adjacent-step collision for the highest-step rule, malformed-input fail-closed, a fuzz target) and
  `internal/staffmfa` (HKDF derivation pinned against an independent computation, subkey independence, master keys
  not retained, AEAD round trip, AAD tamper per field, ciphertext/nonce/kid tamper, planted ciphertext, FACTOR and
  MAC rotation and retirement, MAC format pinned per kind, kind separation, staff binding, recovery-code
  generation/normalisation, counters, lock-before-compare, redaction under every fmt verb, slog and JSON, error
  messages, static pins for no environment/clock reads and constant-time comparison), plus
  `internal/config/staff_mfa_keys_test.go` (required in production, partial refusal, placeholders, redaction,
  bounds, issuer, registry completeness and the 28-pair distinctness matrix, direct and through `Load`).
- Mutation pass: `docs/plans/prh2-hardening-round/adr0119-s1-mutation-kill.txt` (driver
  `adr0119-s1-mutation-driver.py`). Counts are in that file and in 21.6; the S1 rows of the section 17.8 table
  (replay `>` to `>=`, window +-2, AAD without staff or tenant, compare-before-lock in the pure core) are all
  KILLED.

### 21.5 Residuals (S1)

| # | Residual | Disposition |
|---|---|---|
| S1-R1 | Until S3 wires `VerifyStartup`, a production deployment with `APP_ENV` missing loads without staff MFA keys. Nothing uses them yet, so there is no bypass today; S3 must wire the gate before any login path reads them. | S3 entry gates 21.7 (a) and (c) |
| S1-R2 | Constant-time behaviour is pinned statically (comparison primitives), not measured. | Accepted |
| S1-R3 | Keys and secrets live in Go memory and are not zeroised (strings are immutable; the GC copies). | Inherent (M9, R10) |
| S1-R4 | Reuse of a key across environments is not machine-checkable. | Runbook control |
| S1-R5 | A token reveals the staff UUID to whoever holds it (the email channel included). | Accepted (21.2 item 1); reviewer to confirm |
| S1-R6 | Fixed (not sliding) 24 h window: around a window boundary up to 18 failures can occur before the hard lock instead of 10. The soft lock still limits guessing to 5 failures per soft-lock cycle (about 15 minutes). | Accepted by the 13.1 schema; reviewer to confirm |

### 21.6 Security review of S1 (2026-10-13): APPROVE WITH CONDITIONS - disposition

| Item | Disposition (all on the S1 branch) |
|---|---|
| C2 redaction of unexported nested fields | Applied: 21.2 item 11. Struct-wrapped cases added to `TestRedaction_NoSecretTypeEverRenders`; a pointer-based layout was tried first and FAILED that test (the bad-verb leak), which is why closures are used. Mutants V01 (token behind a plain pointer) and V02 (keys behind a plain pointer). |
| C3 registry across text and decoded forms | Applied: 21.2 item 4. `TestPlatformKeyFamilies_CrossForm_*` (JWT signing/previous, fingerprint, `ACTOR_PROOF_KEYS`, `PAYOUT_INSTRUMENT_FP_KEYS`, `PAYOUT_INSTRUMENT_KEYS`, both staff families; direct and through `Load`; a no-false-positive check). Mutants V03, V04. |
| C4 24 h count at the threshold without `hard_locked_at` | Applied: `Counters.valid()` requires `failed_24h_count < 10` when `hard_locked_at` is NULL, otherwise `LockInvalid`. **S2 obligation:** the `staff_mfa_factors` CHECK must express the same rule (`hard_locked_at IS NOT NULL OR failed_24h_count < 10`). Mutant V05. |
| C5 checklist | Applied: `production-configuration-checklist.md` states that `JWT_PREVIOUS_SECRET` must differ from `JWT_SIGNING_SECRET` (refused at startup) and that the signing secret is part of the cross-form registry. |
| Note: Open with an invalid AAD | `OpenFactorSecret` now returns an error matching BOTH `ErrFactorIntegrity` and `ErrInvalidInput`; the doc says no error may be read as "factor absent". Mutant V06. |
| Note: `_SECONDS` int64 wrap | Applied: 21.2 item 14; `18446744134` and the int64 extremes are in the bounds test. Mutant V09. |
| Note: otpauth `%20` | Applied: 21.2 item 13; the test parses the URI back and checks one issuer and no `+`. Mutants V10, V11. |
| Optional: degenerate key | Applied: 21.2 item 12. Mutants V07, V08. |

Mutation evidence after the conditions (`adr0119-s1-mutation-kill.txt`, passes 4 and 5): 85 mutants, 79 KILLED,
5 SURVIVED and justified as behaviour-equivalent (K19, S02, S07, R02, P09; S02 became equivalent because of C2:
without `Token.Format` the closure field still renders only as an address), 1 REMOVED (U01), 0 ERROR. Every
review mutant (V01-V11) is KILLED. `TestStaffMFA_ValidateBoundsDirectly` was added so the bounds check in
`validateStaffMFA` stays independently tested now that `loadStaffMFA` pre-checks the integer range (mutant C06).

### 21.7 S3 entry gates (recorded by the S1 security review; NOT built in S1)

S3 may not merge unless each of these holds and is tested:

- **(a)** `staffmfa.VerifyStartup` is wired into `cmd/platform-api` and `cmd/seed-admin` before any code reads the
  keys.
- **(b)** `*staffmfa.Keys` is built through exactly one constructor that calls `VerifyStartup`, with a static pin
  that `NewKeys` / `NewKeysFromConfig` are called nowhere else outside tests.
- **(c)** A subprocess test: with `APP_ENV` unset and no `STAFF_MFA_*` keys, the binary refuses to start (closes
  S1-R1).
- **(d)** `/v1/staff/auth/enrol/redeem` resolves `{tenant_slug, email, password}` to the SAME `staff_user_id` as the
  token and the token row. The tenant and the `StaffRef` come from the row, never from the body or the token.
- **(e)** A slow-rate MFA-failure alert: second-factor failures after a correct password on 3 or more distinct days
  within 7 days (S3 or S9).
- **(f)** The S3 log-capture test (section 17.7) includes struct-wrapped secrets (secret types held in unexported
  fields of request, response and domain structs).
- **(g)** The generated `otpauth://` URI is checked with at least one real authenticator app (manual evidence
  recorded with the S3 gate).

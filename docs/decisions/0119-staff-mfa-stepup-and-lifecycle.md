# ADR 0119 - Staff MFA, step-up authentication and staff lifecycle

- **Status:** ACCEPTED - PENDING IMPLEMENTATION (2026-10-13). Design only: no code, no migration and no endpoint exists
  yet. Every mechanism below is `NOT IMPLEMENTED` until its slice (section 16) lands and passes its review gate. No
  owner question in section 18 blocks the design or slices S1-S9. Each policy value marked **[ORCH-DEFAULT]** is a
  reversible orchestrator/security default. It is **not owner-approved**, and the owner may change it without a
  redesign.
- **S1 (2026-10-13):** the pure-Go library slice is `IMPLEMENTED` on an unmerged branch, pending gate G. Every other
  slice stays `NOT IMPLEMENTED`. See section 21.
- **S2 (2026-10-13):** migration 0137 (the four MFA tables) and the DB-backed storage layer
  (`internal/staffmfa/store`) are `IMPLEMENTED` on an unmerged branch, pending gate G. The `sessions` columns, `SF040`
  and the session revocation moved to 0138 (S3) as the slice plan says; the `staff_users` triggers (`SF010`-`SF014`,
  `SF020`, `SF021`) stay in 0139. Every other slice stays `NOT IMPLEMENTED`. See section 22.
- **S3 (2026-10-13):** migration 0138 (session assurance, seal, `SF040`, cutover marker, staff-session revocation,
  `SF008` lockout clock) and the two-step staff login, enrolment, recovery and staff refresh are `IMPLEMENTED` on an
  unmerged branch, pending gate G and the manual authenticator check (21.7 (g)). Step-up, liveness and lifecycle stay
  `NOT IMPLEMENTED`. The `security` (live) and `code-reviewer` / `qa` reviews returned APPROVE WITH CONDITIONS; the
  conditions are applied on the branch (section 23.8) and await re-review. S3 merges together with S7a. See section 23.
- **S4a (2026-10-13):** the staff route chain (`staffRoute`: bearer, staff principal, per-request liveness, aal2), the
  closed staff role set (S3-R12), the liveness and refresh re-check of the second factor (S3-R11), and the golden
  step-up classification of all 178 staff routes with its static completeness test are `IMPLEMENTED` on an unmerged
  branch, pending gate G. No migration. Step-up ENFORCEMENT stays `NOT IMPLEMENTED` (S4b): S4a records the class only.
  **Route-registration freeze** in force from the S4a merge (`docs/plans/adr0119-s4a-route-registration.md`). The
  `security` (live) and `code-reviewer` / `qa` reviews returned APPROVE WITH CONDITIONS; the conditions are applied on
  the branch (section 25.8) and await re-review. See section 25.
- **S4b (2026-10-13):** step-up ENFORCEMENT is `IMPLEMENTED` on an unmerged branch, pending gate G and the mandatory
  `ledger-finance` review: `requireStepUp` on every classified staff route (critical 5 min, financial 10 min; `403
  step_up_required` with the RFC 9470 header), the four classifier functions, `POST /v1/staff/auth/step-up` and
  `POST /v1/staff/mfa/recovery-codes/regenerate`, the S3-R10 seal v2 (refresh token hash and idle expiry bound), the
  S3-R13 / S4a-R10 rules (the sealed row, not the claims, decides freshness; incoherent claims refused), migration
  **0151** (the step-up column grants only), the actor-proof signer backstop, the
  withdrawal `require_step_up` 2 min rule, and the section 9.5 re-assertion for K2, K3/M4, HSEC and withdrawal approve
  (ADR 0082 Amendment A9, ADR 0099 A-19 amendment). Withdrawal submit / resolve and K1 re-assertion, the section
  9.5(b) GUC + `staff_actor_self_*` policy pair, lifecycle, the back-office modal (S7b) and staff RG increases stay
  `NOT IMPLEMENTED`. See section 26.
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

### 7.2 `sessions` columns and seal (migration 0138; moved out of 0137 at S2, section 22.2 D2)

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
- **S4b implementation note (section 26, D14).** The pair and the GUC are NOT built in S4b: every flow S4b re-asserts
  (K2, K3/M4, HSEC final approvals; withdrawal approve) already sees the actor's own row (tenant session: the
  dual-scope policy; acting session: `app.tenant_id` is empty, so the dual-scope policy admits platform rows and the
  0112 acting fence admits the acting principal). They land with the first flow whose session hides the actor (K1
  platform-scope approvals, the lifecycle; S4b-R3). Until then a static test pins that nothing sets `app.actor_staff_id`.

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
5. **`sessions` additions: none in 0137.** The nullable columns of section 7.2, the `SF040` trigger and the
   staff-session revocation are **not** in 0137 (review C1): all three are in 0138, which merges with S3. (Revision 2
   put the nullable columns in 0137; the S2 dispatch moved them to 0138, so that 0137 touches no existing table at
   all. Today's `IssueSession` does not write those columns, so S2 does not break staff login either way.)
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
| `staff_mfa_factors`, `staff_mfa_recovery_codes`, `staff_auth_challenges`, `staff_enrolment_tokens`, `staff_lifecycle_events` | the dual-scope policy, identical to `staff_users` (migration 0011): own tenant's rows only | platform rows only (`tenant_id IS NULL`) when `app.tenant_id` is empty. Tenant rows are reached by `WithTenant(target)` ONLY: an acting session (`WithPlatformActingInTenant`) is refused by the strict acting fence, so S6 MFA resets and every other write to a tenant staff member's MFA rows must use `WithTenant(target)` (S2 review). | none. Every restrictive fence that exists on `staff_users` today is replicated on each new staff table: the 0112 acting fence and the migration 0114 KYC-worker fence, plus any platform-service fence. S2 adds two further RESTRICTIVE fences (D14): `player_scope_fence` (`app.player_account_id` must be empty) and `platform_service_fence` (`app.platform_service_id` must be empty), because `staff_users`' own policy admits player and service sessions. A `platform_acting` session, a player session or any service session therefore cannot read or write a staff member's factors. A catalog test asserts that every restrictive policy on `staff_users` has a counterpart on each new staff table (review N3). |
| challenges, enrolment tokens (pre-tenant) | plus the single-row lookup-by-MAC SELECT policy | the same | n/a |
| `staff_lifecycle_requests`, `staff_lifecycle_approvals` | none | SELECT, INSERT, UPDATE (requests: state only) | none |

Every table gets `ENABLE` and `FORCE ROW LEVEL SECURITY`.

### 13.4 Grants to `igaming_runtime`

- **SELECT and INSERT** on every new table.
- **UPDATE is column-level only:**
  - factors: `status`, `last_used_step`, `failed_*`, `locked_until`, `hard_locked_at`, `activated_at`, `revoked_at`,
    `revoked_reason`, and `pending_expires_at` (S2: the `pending_expires_at` CHECK forces it to NULL when a factor
    leaves `pending`; `SF002` lets it only be cleared)
  - recovery codes: `consumed_at`, `revoked_at`
  - challenges: `attempts_left`, `consumed_at`
  - enrolment tokens: `consumed_at`, `revoked_at`
  - lifecycle requests: `state`, `decided_at`
  - `staff_users` (0139): `status`, `role`, `password_hash`, `status_changed_at`, `offboarded_at`. **Not**
    `staff_version`, which only the trigger sets.
  - `sessions` (0138, not 0137): INSERT of the new columns; UPDATE of `mfa_at`, `assurance_kid` and `assurance_seal` only. No
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
| `SF008` | (S3, gate G2) `failed_24h_count` decreased before its 24 h window (database clock) ended, or `failed_24h_start` moved backwards |
| `SF040` | a staff session without assurance columns, or an immutable assurance column changed |
| `SF041` | (S3) migration 0138 up pre-flight refused: `sessions`, the factor counters or the alert kinds drifted, or the staff-session revocation count disagreed with its pre-count |
| `SF042` | (S3) migration 0138 down refused: staff alerts or live sealed staff sessions exist |
| `SF097` | (S2) migration 0137 down refused: MFA data exists |
| `SF098` | (S2) migration 0137 up pre-flight refused: the `staff_users` RLS posture the new tables replicate has drifted |
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
| **S4b + S7b** (one merge unit) | `RequireStepUp` and the four classifiers (S4a added `rgLimitChangeClass` for the ADR 0115 S2 route; section 25.3 D7). The golden table (8.2). The signer backstop with the corrected registry (9.1). The `withdrawal.Approve` step-up parameter (9.3). The scoped in-transaction re-assertion and the `staff_actor_self_*` policies (9.5), with **ADR 0082 Amendment A9** and the **ADR 0099 A-19 amendment** written in the same change. **S7b:** the back office step-up modal. | S4a | **ledger-finance (mandatory:** A9, the placement table, K-flows), architect (A-19, the RLS policy) |
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

## 22. S2 implementation record (2026-10-13)

Branch `worktree-agent-a61c153a5162a0976`, based on `8d87554` (the merge of S1). **NOT MERGED.** Gate G (live
`security` review, `code-reviewer`, `qa`) has not run. Nothing below is an approval.

### 22.1 Status labels

| Item | Label |
|---|---|
| Migration `0137_staff_mfa` (up and down): `staff_mfa_factors`, `staff_mfa_recovery_codes`, `staff_auth_challenges`, `staff_enrolment_tokens`; SQLSTATE `SF001`-`SF006`, `SF097`, `SF098`, `SF099`; the C4 CHECK; `FORCE ROW LEVEL SECURITY` and the dual-scope policy on all four; the two pre-tenant lookup policies; all eight restrictive `staff_users` fences replicated on each table; column-level runtime grants (mirrored in `deploy/init-app-role.sql`) | `IMPLEMENTED` against the local database |
| `internal/staffmfa/store`: the database-backed `staffmfa.StaffFactor` for TOTP (`TOTPFactor`: `Begin`, `Confirm`, `Verify`, `VerifyDetailed`) with the replay-protected monotonic burn; `Store`: recovery codes (issue, consume, count), login challenges and enrolment tokens (issue, two-phase resolve, lock, decrement, consume) | `IMPLEMENTED` against the local database |
| `db.Pool.WithStaffChallengeLookup` / `WithStaffEnrolmentLookup` (the ADR 0016 two-phase lookup helpers) | `IMPLEMENTED` |
| `EnrolmentMaterial.RevealForEnrolment` (the one exported read, S1 package, additive) | `IMPLEMENTED` |
| `SF020`, `SF021`, `SF010`-`SF014` and every other `staff_users` change; `staff_version`; lifecycle tables | `NOT IMPLEMENTED` (0139, S5 + S6) |
| `sessions` assurance columns, the seal, `SF040`, the cutover table, the staff-session revocation | `NOT IMPLEMENTED` (0138, S3) |
| HTTP routes, login change, step-up, session change, `VerifyStartup` wiring, `cmd/seed-admin`, back office | `NOT IMPLEMENTED` (S3 and later). Staff login on `main` does not touch any of this |
| Production key provisioning, real email | `NOT IMPLEMENTED` (`PROVIDER DEPENDENT`, unchanged from section 21.1) |

### 22.2 Deviations from sections 13.1-13.6 and the slice text (for the reviewer)

| # | Deviation | Reason |
|---|---|---|
| D1 | **`SF020` / `SF021` (and their VOLATILE pin) are not in 0137.** The S2 dispatch listed "SF020 VOLATILE trigger function"; section 13.2 puts `SF020`/`SF021` on `staff_users` in 0139, and a trigger on `staff_users` would change existing staff behaviour (no platform admin could be suspended or demoted while no factor can exist before S3). The catalog test `TestCatalog_PoliciesFlagsTriggersFunctions` pins that no function raising `SF010`/`SF020`/`SF021`/`SF030`/`SF040`/`SF007` exists in 0137 and says to flip it into the VOLATILE pin when 0139 lands. | Scope: the ADR text over a summary line; the orchestrator can overrule |
| D2 | **No `sessions` change at all in 0137.** Revision 2 put the nullable columns in 0137; the S2 dispatch moved them to 0138. Sections 7.2, 13.1 item 5 and 13.4 are amended in place. | Dispatch |
| D3 | `GRANT UPDATE (pending_expires_at)` on factors, and `SF002` lets it only be cleared. | The CHECK `(status='pending') = (pending_expires_at IS NOT NULL)` forces it to NULL on activation or revocation, so the runtime role must be able to write it. Section 13.4 amended |
| D4 | Extra CHECKs beyond the ADR: the counters mirror `staffmfa.Counters.valid()` (`failed_count` 0-4, `failed_24h_count` 0-10, a window start exists iff its count is non-zero); the C4 CHECK as section 21.6 asks; the activation / revocation columns agree with `status`; non-empty `secret_ct` and kids; MAC length 32 on the challenge and token tables too (the ADR only gave `code_mac` one); `expires_at > created_at`; `staff_version_at_issue >= 0` | Make the database refuse what the library already treats as locked or invalid |
| D5 | Extra trigger behaviour beyond the ADR text: a revoked factor is frozen; `activated_at`, `hard_locked_at` and the revocation are write-once; a consumed challenge is frozen; `attempts_left` only decreases; consumed and revoked are never both | Same intent as `SF002`/`SF004`/`SF005`/`SF006`; makes the lockout irreversible by the runtime role |
| D6 | `issued_by` has a foreign key to `staff_users(id)` (NULL allowed). `lifecycle_request_id` has none (its table is created by 0139) | Audit identity integrity |
| D7 | New codes `SF097` (down refused, MFA data exists) and `SF098` (up pre-flight refused) | Repo convention (`RL099`, `BR098`); section 13.6 amended |
| D8 | The pre-flight (`SF098`) is a drift guard on the `staff_users` RLS posture the new tables replicate (FORCE RLS, `dual_scope_isolation`, exactly the eight restrictive fences, `financial_acting_gucs_present`). It reads no row and lifts no `FORCE` | 0137 migrates no data; the only pre-condition that can silently go wrong is the review-N3 fence parity |
| D9 | Restrictive fences use the **strict** form on the new tables: the acting fence admits no acting session at all (`staff_users` admits the acting principal's own row and the acted tenant's rows). Same policy names and commands, so the parity test matches by name | Section 13.3: an acting session or a worker "cannot read or write a staff member's factors" |
| D10 | The lookup policies add migration 0040's "and no tenant is set" conjunct, and compare with `decode(..., 'hex')` as the ADR says. The pool helpers accept only 64 lowercase hex characters, so a malformed value never reaches `decode` | ADR 0016 / 0040 pattern |
| D11 | The store package is `internal/staffmfa/store` (the ADR names no package for the store). `Verify`, `VerifyDetailed` and `ConsumeRecoveryCode` report a refusal through the result with a **nil error** (a caller that rolls back on any error cannot lose the failure counters); `Confirm`, which returns only an error by interface, returns `ErrRejected`/`ErrLocked`/`ErrHardLocked`/`ErrIntegrity` and `store.Refused(err)` tells the caller to commit | Section 6.1 "record then return the outcome" |
| D12 | `staffmfa.EnrolmentMaterial.RevealForEnrolment` (S1 package, additive): the S1 design left the raw read to an unexported accessor no other package can call | The store tests and the S3 enrolment response need it |
| D13 | `IssueChallenge` takes `staffVersion` (0 until 0139 adds `staff_users.staff_version`); `staff_version_at_issue` has no foreign key | 0139 |
| D14 | (security review F1) Two further RESTRICTIVE fences on all four tables: `player_scope_fence` and `platform_service_fence`, each `FOR ALL`, requiring `app.player_account_id` / `app.platform_service_id` to be empty. The catalog parity test is now a superset test (every `staff_users` fence plus exactly these two, pinned by name). The `kyc_worker_fence_*` count pin stays 52 (the new fences have other names); the per-table restrictive count is 8 + 2 = 10. Live tests refuse a player session and each of `alert_dispatcher`, `sportsbook_catalogue_sync`, `kyc_submission_worker` and an unknown service for read, FOR UPDATE, insert, revoke and hard-lock | Section 13.3 said these sessions get no access; `staff_users` is readable by them |
| D15 | (review F5) `staff_enrolment_tokens.issued_by` has a foreign key to `staff_users(id)` that is NOT tenant-scoped: it can name a staff member of another tenant. It is audit attribution only. S6 must treat the lifecycle event / audit row as the authoritative issuer, or validate `issued_by` in Go | Review F5 |

### 22.3 Decisions taken inside S2 (reversible)

1. **Serialisation point.** The staff member's ACTIVE factor row (`FOR UPDATE`) serialises `Verify`, recovery-code consumption,
   recovery-code regeneration and the failure counters; the PENDING factor row serialises `Confirm`. `Confirm` then locks the
   old active row through its `UPDATE`, inside a savepoint; `Verify` never locks a pending row, so no cycle exists between them.
2. **Two layers behind every single-use rule**: the row lock and a conditional `UPDATE` (`last_used_step < step`,
   `consumed_at IS NULL`, `revoked_at IS NULL`, `attempts_left > 0`). Each layer has its own test (the conditional alone,
   without the lock, is tested concurrently; the lock alone by a lock-wait test), so a mutant that drops one is killed by a test
   that does not depend on the other.
3. **A keyed MAC is verified against the kid the row names**, not only found by equality, and the purpose is inside the MAC as
   well as in the purpose column; both are tested independently.
4. **No tenant GUC is ever set by the store.** The caller opens the scope (`store.WithScope` picks tenant or platform from a
   `StaffRef` that a lookup returned).
5. **Hard lock write-once.** Once `hard_locked_at` is set the runtime role cannot clear it (`SF002`): only a new factor (the S6
   MFA reset) restores access.

### 22.4 Changed existing tests and assertions (every one)

| File | Change | Why |
|---|---|---|
| `internal/db/kyc_worker_identity_integration_test.go` | `fencedTables` (ADR 0106 section 3.3, "nine tables") gains `staff_mfa_factors`, `staff_mfa_recovery_codes`, `staff_auth_challenges`, `staff_enrolment_tokens` (thirteen) | The catalogue gate derives the fenced set from the policies; the four new tables carry the KYC worker fence. This also extends T-ID-2 (the worker reads zero rows) to them |
| `internal/db/kyc_worker_catalogue_gate_integration_test.go` | `expected 36 restrictive kyc_worker_fence_* policies` becomes 52 (9 tables x 4 + 4 tables x 4) | Same |
| `internal/db/null_arm_replay_static_test.go` | `a18FencedTables` gains the same four tables | A-18 skips tables whose `tenant_id IS NULL` arm is covered by the staff_users-class restrictive fence; the parity is proven by `TestCatalog_RestrictiveFenceParity` and `TestRLS_RestrictiveFences_NoActingNoWorkerAccess` |

No hard-coded migration-head pin exists in the Go tests (searched). `deploy/init-app-role.sql` gains one block (the four
tables' grants), mirrored by `TestStatic_InitAppRoleSQLMatchesMigration0137Grants`.

### 22.5 Evidence (private database harness only; sanctioned local CI credentials; no role, password or privilege change)

- `internal/staffmfa/store` (integration tag; `TEST_RUNTIME_DATABASE_URL` is the role every assertion runs as; the table owner
  only provisions tenants and staff and plants tamper rows that the triggers must stop):
  - **replay and burn:** the same code twice, an older step after a newer one, window T+1 accepted and T+3 refused, 24
    parallel verifiers of one valid code give exactly one success, `burnStep` alone (no lock) gives exactly one of 16;
  - **lockout:** 5 failures soft-lock, the 6th attempt with the CORRECT code is answered locked and writes nothing, the
    locked answer does not depend on the code, expiry, the 24 h window restart, the 10th failure sets the hard lock, a hard
    lock refuses after any delay and the runtime role cannot clear it, a reset factor is clean, 50 parallel wrong verifies
    give exactly 5 comparisons and 45 locked answers with exact counters, `Confirm` and recovery-code failures share the budget;
  - **integrity:** planted, flipped, moved (to a colleague, to platform staff) and unknown-kid ciphertext is
    `OutcomeIntegrity` (counted, never "absent", never accepted); a factor sealed under another key with the victim's exact
    AAD is never accepted;
  - **recovery codes:** stored only as keyed MACs, single use (16 concurrent consumers give one), bound to the staff member,
    normalisation, regeneration revokes the old batch, a locked factor refuses before any lookup;
  - **challenges and enrolment tokens:** keyed MAC only, two-phase resolve, purpose and kid bound in the MAC, expiry
    boundary, single use under concurrency, exhaustion consumes, the `FOR UPDATE` is held (a concurrent locker waits);
  - **triggers, CHECKs, indexes:** every `SF001`-`SF006` and `SF099` code by direct SQL for every table, the C4 CHECK, the
    counter CHECKs, the unique indexes, MAC uniqueness;
  - **RLS, fences, grants:** the dual-scope visibility matrix on every table, cross-tenant and cross-scope writes touch
    nothing, parity with `staff_users` across 12 GUC combinations, acting and KYC-worker sessions see and write nothing, the
    lookup policies (one row, SELECT only, only with no tenant, fences apply), the runtime privilege matrix (SELECT, INSERT,
    column UPDATE only), catalog pins (policies, FORCE, trigger list and timing, functions plain, pinned, volatile);
  - **adversarial (arbitrary SQL as `igaming_runtime`):** a planted factor, recovery code, challenge or enrolment token with an
    attacker key or an unkeyed hash is never accepted or found; a MAC rebuilt into a token does not resolve; the pinned
    residuals `..._Residual_ForgedTenantGUCReadsCiphertextOnly` and `..._Residual_R3_RevokingAFactorIsDenialOfServiceNotBypass`;
  - **migration (scratch databases):** up / down / up whole-schema snapshot (policies, trigger enablement and definitions,
    constraints, table and COLUMN ACLs, function flags and bodies, indexes) and the diff only adds objects of the four tables;
    `SF098` for six drifts (a KYC-worker fence missing, an acting fence missing, an unexpected extra fence, FORCE lifted,
    the dual-scope policy gone, the acting predicate function gone) with names only and nothing created; `SF097` for data in each of the four tables with counts only, nothing
    dropped, FORCE still on, and through the real migrate runner;
  - **static:** no logging, printing, environment, clock or formatted value in the store; grants mirrored in
    `deploy/init-app-role.sql`; migration scope (no sessions, no `staff_users`, no later-slice SQLSTATE, every function pins its
    `search_path`, no `SECURITY DEFINER`); SQLSTATE class `SF` only where assigned.
- Mutation pass: `docs/plans/prh2-hardening-round/adr0119-s2-mutation-kill.txt` (driver
  `adr0119-s2-mutation-driver.py`). 121 mutants (49 Go in the store and the lookup helpers, 72 SQL in migration 0137 up and
  down, each SQL mutant on a database REBUILT from the mutated migration; Q66-Q69 are the review's fence mutants): 117 KILLED, 4 SURVIVED and justified as
  behaviour-equivalent (R03, R07: second layers behind `consumeCode` and the keyed-MAC verification; Q09: `factor_type`
  has one admitted value and no UPDATE grant; Q19: unreachable behind the frozen-revoked branch), 0 ERROR. The pass found
  and closed real test gaps: the kid binding of a keyed MAC (C09, C12), the row lock of the challenge / token / pending
  rows (C13, E08, G02: a lock-wait test), the conditional burn independent of the lock (G03, G03b, G05: a stale-row
  test), the `Begin` staff predicate (G08), the tenant arm of the down refusal (Q63: a count blind to `FORCE ROW LEVEL
  SECURITY` missed tenant rows), the purpose column against the purpose inside the MAC (C10, C11, E06), the status graph
  message (Q15). The file records the discarded runs (a container restart; a connection-exhaustion run whose mass
  failures looked like kills) so that they cannot be mistaken for evidence.

### 22.6 Residuals (S2)

| # | Residual | Disposition |
|---|---|---|
| S2-R1 | **Lockout counters are runtime-writable columns** (the application has to write them). Under M5 (arbitrary SQL as `igaming_runtime`) plus the victim's password, the runtime role can reset `failed_count`, `failed_window_start`, `locked_until`, `failed_24h_count` and `failed_24h_start` at will, and so can PREVENT the hard lock from ever being set: the M4 bound (about 3e-5 per hard-lock cycle) does not hold under M5, and R9's claim that the DB counters are the real control is weakened to a control against non-SQL attackers only. Under M5 the only bound on online guessing is the per-process, per-IP limiter (R9) plus audit. What remains true: `hard_locked_at` is write-once once set, and `last_used_step` is monotonic (replay protection holds under SQL) | Open; S3 entry gate G2 decides (DB-clock rule, audited decreases + alert, or an owner-accepted residual) |
| S2-R2 | A runtime session can set `hard_locked_at`, `status = 'revoked'` or `last_used_step` upward on a factor it can see: a denial of service (R3), never a bypass | R3 |
| S2-R3 | Tenant isolation of the new tables is the tenant GUC like every other table (a runtime session that forges `app.tenant_id` reads that tenant's rows: ciphertext and MACs only) | Pinned by the `Residual` tests; M5 |
| S2-R4 | `Verify` reports `ErrNoActiveFactor` (absent) differently from every outcome. S3's login must collapse that into one response so it is not a factor-existence oracle | S3 gate |
| S2-R5 | The fixed 24 h window (S1-R6) is stored as the library writes it; the CHECKs do not tighten it | S1-R6 |
| S2-R6 | Expired rows are never deleted by the runtime role (no DELETE); pruning is an owner runbook step (S9), and the owner is also stopped by `SF099` unless the runbook disables the trigger | S9 must define the prune procedure |
| S2-R7 | `staff_users` rows referenced by these tables cannot be deleted (foreign keys, no cascade). Offboarding keeps the row anyway (section 10.1) | Intended |

### 22.7 What `security` should probe live (gate G)

1. Forge each GUC (`app.tenant_id`, `app.principal_id`, `app.platform_admin_principal_id`, `app.platform_service_id`,
   `app.acting_*`, `app.staff_challenge_lookup_mac`, `app.staff_enrolment_lookup_mac`, `app.session_lookup_hash`) in every
   combination against all four tables: read, `INSERT`, `UPDATE`, `SELECT ... FOR UPDATE`; try to learn a victim's row
   through the lookup policies without holding the MAC.
2. Try to defeat the triggers as `igaming_runtime`: `SET session_replication_role = replica`, `ALTER TABLE ... DISABLE TRIGGER`,
   `COPY`, `INSERT ... ON CONFLICT DO UPDATE` on the unique MAC columns, `UPDATE ... FROM` joins that change `tenant_id`,
   `TRUNCATE ... CASCADE`, writes through `staff_users` cascades.
3. Race the burn: many connections, `READ COMMITTED` and `REPEATABLE READ` / `SERIALIZABLE` transactions (the store expects
   `READ COMMITTED`; a `40001` must fail closed), two pods, a transaction cancelled after the `UPDATE` but before commit, a
   `Confirm` racing a `Verify` on the old factor, two enrolments begun at once.
4. Counter manipulation under M5 (S2-R1): which resets can the runtime role perform, and is that acceptable next to the
   per-IP bucket and the audit alert design for S3?
5. Planted rows: an active factor with attacker ciphertext (the victim then fails integrity on every login: confirm S3 raises
   `staff_mfa_integrity` and that a reset restores access), a planted pending factor that blocks enrolment through the unique
   index, a planted live enrolment token that blocks issuing a new one.
6. Whether `sf_force_tenant_from_staff` reading `staff_users` under the caller's own RLS (so a platform session cannot write a
   tenant staff member's rows without `WithTenant(target)`) is the behaviour S3 and S6 want.
7. Secret hygiene: that nothing in the store, its errors or the Postgres log can carry a code, token, secret or MAC (statement
   logging, `log_min_error_statement`: the failing statements carry parameters only as `$n`).

### 22.8 S3 entry gates recorded by the S2 security review (NOT built in S2)

S3 may not merge unless each holds and is tested:

- **G1** An HTTP-level test that 5 and 6 wrong codes through the REAL routes persist the soft lock and decrement `attempts_left`, plus a mutant that returns an error from the transaction closure (must be killed). Consider a store-owned transaction wrapper, or aligning `Confirm` with the result-plus-nil-error contract.
- **G2** Decide S2-R1: a DB-clock rule (`failed_24h_count` may decrease only if its window start is older than 24 h), or audited decreases with an alert, or an owner-accepted residual.
- **G3** The step burn and the session / challenge consumption happen in one transaction.
- **G4** The response to a `40001` or a commit failure must not depend on the verification outcome.
- **G5** A static caller allow-list for every `RevealFor*` method.
- **G6** The S9 runbook pins `log_parameter_max_length = 0` and keeps `pg_stat_statements` parameter-free.
- **G7** S2-R4: collapse "no factor" into the single login response and raise `staff_mfa_integrity`.
- **Note:** between S3 and S6 there is no MFA-reset path. The last platform admin's lockout then needs owner intervention through `cmd/seed-admin`, so S3 must not reach a persistent environment without the runbook.

## 23. S3 implementation record (2026-10-13)

Branch `worktree-agent-a8cb2de225b471305`, based on `d2cad31`. **NOT MERGED.** Gate G (live `security` review,
`code-reviewer`, `qa`) has not run, and entry gate 21.7 (g) (a real authenticator app) needs a human. Nothing below is
an approval. S3 must not reach a persistent environment without `docs/runbooks/staff-mfa-s3-cutover.md`.

### 23.1 Status labels

| Item | Label |
|---|---|
| Migration `0138_staff_session_cutover` (up and down): the seven `sessions` assurance columns and their CHECKs, `staff_session_cutover` (one row, append-only, fenced read policy), `SF040` guard, revocation of every live staff session, column-level `sessions` UPDATE grant, `SF008` lockout clock (gate G2), ten `staff.*` alert kinds, `SF041` pre-flight, `SF042` down refusal | `IMPLEMENTED` against the local database |
| Staff login: password step returns an `mfa` challenge (or one `403 mfa_enrolment_required` / `403 mfa_locked`); `POST /v1/staff/auth/mfa/verify` (TOTP), `POST /v1/staff/auth/mfa/recover` (recovery code to an `enrol` challenge) | `IMPLEMENTED` |
| Enrolment: `POST /v1/staff/auth/enrol/redeem`, `/enrol/start`, `/enrol/confirm` (10 recovery codes, aal2 session, all earlier sessions revoked) | `IMPLEMENTED` |
| `GET /v1/staff/me/mfa` | `IMPLEMENTED` (bearer + `RequireStaffPrincipal`; the S4a staff route chain does not exist yet) |
| Staff session: keyed seal, claims `sid`/`aal`/`amr`/`auth_time`/`mfa_at`/`sv`, refresh with seal check, 30 min idle, 12 h absolute (S1 config), C1 cutover rule, logout | `IMPLEMENTED` |
| Per-IP `mfa` rate bucket (30/min) | `IMPLEMENTED` (per process, R9) |
| Alerts `staff.mfa_integrity`, `staff.session_integrity`, `staff.mfa_hard_locked`, `staff.mfa_recovery_used`, `staff.mfa_failure_pattern` (+ `staff.platform_*` variants) | `IMPLEMENTED` (raised post-commit) |
| `staffmfa.LoadKeys` as the single key constructor, wired into `cmd/platform-api` and `cmd/seed-admin` | `IMPLEMENTED` |
| `cmd/seed-admin` bootstrap enrolment token (new admin, or `-issue-bootstrap [-tenant-slug]` for a staff member without an active factor) | `IMPLEMENTED` (owner break-glass path until S6) |
| OpenAPI (`docs/api/openapi/platform-api.yaml`) and UI contract `docs/plans/adr0119-s3-api-contract.md` | `IMPLEMENTED` (drift tests) |
| Cutover runbook `docs/runbooks/staff-mfa-s3-cutover.md` | `IMPLEMENTED` (document; not exercised in a persistent environment) |
| Step-up (S4), staff route chain / liveness / `sv` versioning (S4a, 0139), lifecycle and MFA reset (S5/S6), invitations, password redeem, recovery-code regeneration, back office (S7a, separate agent) | `NOT IMPLEMENTED` |
| Gate 21.7 (g): `otpauth://` URI checked with a real authenticator | `BLOCKED` on a human (manual evidence) |
| Production key provisioning, real email delivery of enrolment tokens | `PROVIDER DEPENDENT` / `NOT IMPLEMENTED` |

### 23.2 How each entry gate holds

| Gate | Mechanism | Test(s) |
|---|---|---|
| 21.7 (a) | `staffmfa.LoadKeys` (= `NewKeysFromConfig` + `VerifyStartup`) runs in `cmd/platform-api` right after `config.Load`, before the synthetic guard and any DB side effect; `cmd/seed-admin` calls it before connecting | `TestRun_GuardAndSchemeValidationPrecedeDBConnect` (AST order pin), `TestLoadKeys_IsTheStartupGate` |
| 21.7 (b) | `LoadKeys` is the only constructor outside tests | `TestStatic_StaffMFAKeysSingleConstructor` (qualified-call scan; `payoutinstrument.NewKeys` is not a false positive) |
| 21.7 (c) | config refuses in every environment when the key family is missing | `TestStaffMFAStartupGate_RefusesWithoutKeys_Subprocess` (APP_ENV unset, no keys: exit non-zero, named error) |
| 21.7 (d) | redeem: the token row is resolved first; tenant and `StaffRef` come from the row; the `{tenant_slug,email,password}` triple must resolve to the same staff id and tenant; platform tokens refuse a `tenant_slug`; every mismatch is one `401 invalid_enrolment_token`, counts as a failed password attempt and is audited | `TestStaffMFA_Redeem_GateD_TokenRowIsAuthoritative`, `TestStaffMFA_Redeem_Lockout` |
| 21.7 (e) | failures audited as `staff.mfa_verification_failed` on 3 distinct UTC days within 7 days raise `staff.mfa_failure_pattern` (tenant) / `staff.platform_mfa_failure_pattern` | `TestStaffMFA_GateE_SlowRateFailurePattern` (tenant and platform subtests, a 9-day-old row outside the window) |
| 21.7 (f) | every secret on the HTTP path is a `redacted[T]` (String/GoString/Format/LogValue/MarshalJSON print `[REDACTED]`); raw values reach the wire only through `wire()` inside `writeStaffSecretJSON` | `TestStaffMFA_SecretHygiene_LogCapture` (server log, slog/fmt of struct-wrapped requests, responses, domain types) |
| G1 | refusals return nil from the transaction closure so counters and `attempts_left` commit | `TestStaffMFA_G1_RefusalsPersistThroughRealRoutes` (5 and 6 wrong codes through the real routes; DB state read back); mutants M01-M03 |
| G2 | **DB-clock rule chosen** (no owner decision needed): trigger `sf_factor_lockout_clock` raises `SF008` when `failed_24h_count` decreases while `OLD.failed_24h_start > now() - 24h`, or when `failed_24h_start` moves backwards | `TestFactorLockoutClock_SF008` (as `igaming_runtime`); mutants Q04-Q06 |
| G3 | step burn, challenge consumption, session insert and audit happen in one `m.run` transaction | `TestStaffMFA_G3_BurnConsumeAndSessionInOneTransaction` (a commit fault after the burn leaves step, challenge and sessions untouched); M04 |
| G4 | every DB fault, `40001` and commit failure answers the same fixed `503 service_unavailable`, independent of the outcome computed before it | `TestStaffMFA_G4_FaultAnswerIndependentOfOutcome` (test-only `commitFault` seam returning `40001`: right code, wrong code and recovery code give the same status, code and message; nothing persists); M23 |
| G5 | `RevealFor*` static caller allow-list (three sites in `internal/httpserver/staff_auth_secrets.go`, one in `cmd/seed-admin`) | `TestStatic_RevealForCallersAllowlist` |
| G6 | the cutover runbook pins `log_parameter_max_length = 0`, `log_parameter_max_length_on_error = 0` and parameter-free `pg_stat_statements` as a precondition | runbook only (no DB setting is changed by code) |
| G7 | at the password step "never enrolled" and "factor vanished" give one `403 mfa_enrolment_required`; the latter (an activated factor row exists) also raises `staff.mfa_integrity`; at `mfa/verify` / `recover` no factor is `401 invalid_mfa_code` plus the integrity alert | `TestStaffMFA_G7_NoFactorIsOneAnswer_AndIntegrityAlerts`; M05, M29, M31 |

Other required tests: `TestStaffMFA_FullFlow_TenantAndPlatform`, `..._HardLock_NoChallenge_AlertRaised`, `..._Replay`,
`..._RecoveryFlow`, `..._SessionSealTamper`, `..._RefreshBounds`, `..._RateLimit_MFABucket`, `..._TenantIsolation`,
`..._SuspendedBetweenSteps`, `..._EnrolConfirm_Expiry_And_WrongCode`, `TestMigration0138_UpDownUp_WholeSchema`,
`..._PreFlight_Refusals`, `..._Cutover_RevocationAndC1Rule`, `..._RollingDeploy_FailsClosed`, `..._Down_Refusals`,
`TestSessions_SF040Matrix`, `TestGrants_SessionsAndCutover_RuntimeRole`, `TestOpenAPI_StaffAuth_*` (3),
`TestSealSession_*` (3), `TestIssueStaff_*`, `TestVerify_StaffTokenNeedsEveryAssuranceClaim`,
`TestIssue_RefusesStaffPrincipal`, `TestSeedAdmin_BootstrapToken_Subprocess`.

### 23.3 Deviations (for the reviewer; each reversible)

| # | Deviation | Reason |
|---|---|---|
| D1 | Alert kinds are dotted (`staff.mfa_integrity`, not `staff_mfa_integrity`) and each signal has a `staff.platform_*` variant | Repo kind convention; `alert_kinds_check1` requires a subject tenant for tenant kinds, and a platform staff member has none. The platform variants are subjectless and not tenant-raisable |
| D2 | 0138 lifts `FORCE ROW LEVEL SECURITY` on `sessions` inside its own transaction to revoke every staff session (with a pre-count assertion), then restores it | The migration owner otherwise sees only GUC-scoped rows and would revoke none |
| D3 | Deferred endpoints: step-up, recovery-code regeneration (needs step-up), invitations, password redeem, `401 session_revoked` | Slice plan (S4/S6); listed in the contract doc section 4 |
| D4 | New codes `SF008` (lockout clock), `SF041` (0138 pre-flight), `SF042` (0138 down refused) and API code `mfa_enrolment_expired` (409, a pending factor or `enrol` challenge expired during confirm) | Gate G2; repo migration convention; a distinct, non-secret UI state. Section 13.6 amended |
| D5 | Every fault on these routes is `503 service_unavailable` with one fixed message, not `500` | Gate G4: one answer independent of the outcome |
| D6 | `cmd/seed-admin -issue-bootstrap [-tenant-slug]` re-issues a bootstrap token to an existing active staff member WITHOUT an active factor (tenant staff too) | Tenant staff have no other enrolment path before S6 (invitations); refuses when a factor is active |
| D7 | `sv` is always `0` (`staffVersionUnversioned`) | `staff_users.staff_version` arrives in 0139 |
| D8 | G7 interpretation as in 23.2 (password step: one 403; second-factor endpoints: `invalid_mfa_code`) | A challenge is only issued to an enrolled account, so "no factor" at verify means it vanished between steps |
| D9 | Every MFA decision uses the transaction's DB clock (`SELECT now()`) | Agrees with `SF008` and the store CHECKs; immune to app-host clock skew |
| D10 | 0138 narrows the runtime role's `sessions` UPDATE to exactly the columns S3 writes (`revoked_at`, `last_used_at`, `replaced_by_session_id`), only when the role currently has table-level UPDATE; mirrored in `deploy/init-app-role.sql`. Section 13.4's `mfa_at` / `assurance_kid` / `assurance_seal` grants move to S4b, the slice that first writes them (code review F9) | Least privilege; defence in depth behind `SF040` |
| D11 | `SF040` freezes more than section 7.2 lists: on staff rows `id`, `principal_id`, `tenant_id`, `created_at`, `refresh_token_hash`, `expires_at`; `revoked_at` and `replaced_by_session_id` write-once (security review R1); `principal_type` on all rows; `mfa_at` monotonic; seal/kid never back to NULL. Binds every role, the owner included | Section 7.2 intent; each is pinned in `TestSessions_SF040Matrix` |
| D12 | The `staff_session_cutover` read policy is fenced (no `platform_service`, player or acting session) | A18 / KYC-worker / acting fence catalogue tests |
| D13 | Logout of an already-revoked session answers 204 (was 500) | Pre-existing defect found by the hygiene test |
| D14 | `auth.IssueSession` and `auth.Issue` refuse `PrincipalStaff`; staff use `IssueStaffSession` / `IssueStaff` | A staff token or session without assurance cannot be minted by mistake |
| D15 | Player refresh reads the staff assurance in a separate statement | Player refresh keeps working on a 0137 schema during the rolling deploy |
| D16 | Every integration-test staff login (`mustLoginStaff`) runs the real redeem/enrol/verify flow over HTTP | No bypass exists, so the fixtures cannot use one |
| D17 | Redeem honours the per-account password lockout and records each failed redeem as a failed staff password attempt | Redeem verifies the password, so it must not be an oracle that ignores the lockout |
| D18 | Refresh responses (player and staff) carry `Cache-Control: no-store` | Token hygiene |
| D19 | `GET /v1/staff/me/mfa` uses `auth.Middleware` + `RequireStaffPrincipal`, without liveness | The S4a staff chain does not exist; read-only, own account |
| D20 | (security review C1) `auth.Issue` and `auth.Verify` bind the role to the principal type: a staff principal holds a staff role (non-empty, not `player`); a player or service principal holds `player` only; no other principal type exists. A hand-signed token with a staff role on a non-staff principal is a 401 | Permissions are looked up by role alone; before the fix a player-principal token with `platform_admin` passed `/v1/admin/tenants` |
| D21 | (security review C2) `config.ValidateStaffMFAEnv` applies `config.Load`'s staff MFA rules (published placeholders, bounds, issuer, pairwise key-family distinctness) and `cmd/seed-admin` calls it before `staffmfa.LoadKeys` | `seed-admin` minted a bootstrap token under the published `.env.example` keys with `APP_ENV=production` |
| D22 | (security review R4) `cmd/seed-admin -issue-bootstrap` requires `-operator` and `-reason` and records both in `staff.bootstrap_token_issued` (optional, recorded when given, at account creation) | CLAUDE.md: every mutating administrative action records actor and reason |
| D23 | (security review R3) `/enrol/redeem` refuses a bootstrap token of a staff member with an active factor: the single `invalid_enrolment_token`, the token is not spent, audit `reason=active_factor`, P1 `staff.mfa_integrity` | Defence in depth behind `cmd/seed-admin`'s own refusal |
| D24 | Every login fault answers the fixed 503 (was 500 at the password step), and every redeem refusal (the lockout 429 included) pays one Argon2 verification | Code review F6; the review timing note |

### 23.4 Changed existing tests and assertions (every one)

| File | Change | Why |
|---|---|---|
| `cmd/platform-api/run_order_ast_test.go` | adds the pin "`staffmfa.LoadKeys` before `refuseSyntheticInProduction`" and requires it before DB connect | Gate (a) |
| `cmd/platform-api/synthetic_guard_ordering_integration_test.go` | the MissingAppEnv subprocess now gets staff keys | Otherwise it fails on the new key gate before reaching the guard it tests |
| `internal/adjustment/b3_rls_integration_test.go`, `internal/db/kyc_worker_identity_integration_test.go` (seed and the T-ID-2 insert probe), `internal/db/capability_grant_integration_test.go` | staff `sessions` fixture inserts carry the assurance columns | So the probes still test RLS, not `SF040` |
| `internal/alerting/alert_test.go` | the five platform staff kinds are allowed to be subjectless platform kinds (and asserted not tenant-raisable) | D1 |
| `internal/auth/jwt_test.go`, `middleware_test.go`, `require_player_principal_test.go`, `require_staff_principal_test.go` | staff tokens minted with `IssueStaff`; the subject `"admin-1"` became a uuid | D14 |
| `internal/auth/session_rls_integration_test.go` | staff sessions via `IssueStaffSession` with test keys | D14, `SF040` |
| `internal/httpserver/identity_flow_integration_test.go` | `TestStaffLogin_PlatformAdminOmitsTenantSlug` and `TestStaffLogin_TenantScopedRequiresTenantSlug` expect `403 mfa_enrolment_required` before enrolment and `200` (challenge) after; `mustLoginStaff` moved to the fixture file | The login response changed (section 14.1) |
| `internal/httpserver/staff_identity_static_test.go` | four allow-list entries (`Deps.StaffMFA`, the caller's own `FactorID` in three DTOs) | Not a staff identity of another principal |
| `internal/httpserver/*` (about 45 files, 51 `Deps` literals) | `StaffMFA: testStaffMFA()` added | Login needs the MFA subsystem |
| token helpers in `rg_limits_harness`, `provider_credential_api`, `alert_routing_http`, `capability_api`, `payments_kill_switch_api`, `payment_deposit_simulation`, `player_surface_principal`, `launch_authorisation_routes_test` | staff tokens via `authtest.StaffToken` / `IssueStaff` | D14 |
| `internal/staffmfa/store/world_integration_test.go` (`t0` = now-7d truncated), `binding_` and `lockwait_integration_test.go` (`created_at = t0`) | test clock moved into the past | A future `t0` conflicted with `SF008` (DB clock) |
| `internal/staffmfa/store/rls_grants_integration_test.go` | catalogue pins gain `sf_factor_lockout_clock` and `sf_cutover_immutable`; "no function raises SF040" became "exactly `sessions_staff_assurance_guard` raises SF040" | 0138 |
| `internal/staffmfa/store/static_test.go` | the SF class allow-list admits `SF008`, `SF040`-`SF042`, `SF099` in 0138 only | 0138 |
| `internal/staffmfa/store/stale_integration_test.go` | `activate` signature | `ConfirmDetailed` returns the replaced factor id |
| `internal/staffmfa/store/triggers_integration_test.go` | the "24h window without a count" CHECK probe runs before the count is raised | From a non-zero count it is a decrease, which `SF008` refuses first |
| `internal/db/null_arm_replay_static_test.go` | unchanged (an entry added during development was removed) | - |

Review round (security C1-C3, R1-R4; code review F1-F9), further changed assertions:

| File | Change | Why |
|---|---|---|
| `internal/auth/require_player_principal_test.go`, `require_staff_principal_test.go` | the `...DeniesServicePrincipal` tokens are minted with `RolePlayer` (was `RoleSupport`) | C1: `Issue` refuses a staff role on a service principal; the tests still prove the principal check (403) |
| `internal/httpserver/sportsbook_settlement_flow_integration_test.go` `TestSettlementSimulate_403ForServicePrincipal` | asserts `Issue` refuses `risk_manager` on a service principal, a hand-signed one is 401, and a legal service token is 403 | C1 |
| `internal/httpserver/staff_mfa_schema_integration_test.go` `TestGrants_SessionsAndCutover_RuntimeRole` | runtime UPDATE columns are `last_used_at`, `replaced_by_session_id`, `revoked_at` (was also `mfa_at`, `assurance_kid`, `assurance_seal`); the `deploy/init-app-role.sql` mirror string likewise; new pin of the permissive `sessions` policies | F9; C3 |
| `internal/httpserver/staff_mfa_schema_integration_test.go` `TestSessions_SF040Matrix` | the final block: the runtime role's `mfa_at` UPDATE is now 42501 and the owner moves `mfa_at` / rewrites the seal (was the runtime role); `expires_at` added to the immutable list; write-once cases added | F9; R1 |
| `internal/httpserver/staff_mfa_flow_integration_test.go` `TestStaffMFA_SessionSealTamper` | (c) the unsealed `mfa_at` edit is made by the owner (the runtime role has no grant); (d) `mfa_at`, the seal and `refresh_token_hash` added to the 42501 list; the seal-NULL / `mfa_at`-backwards SF040 probes run as the owner | F9 |
| `internal/httpserver/identity_flow_integration_test.go` (the two `TestStaffLogin_*` tests) | the 403 also asserts `code == "mfa_enrolment_required"` | F7 |
| `cmd/seed-admin/seed_admin_integration_test.go` | every `-issue-bootstrap` run passes `-operator` / `-reason` | R4 |

### 23.5 Evidence (private database harness only; no role, password or privilege change)

- Integration tests run as `igaming_runtime` against private databases (`-tags integration -p 1 -count=1`):
  `internal/staffmfa/...`, `internal/auth/...`, `internal/tenant`, `internal/db`, `cmd/...` (migrate, platform-api,
  seed-admin), `internal/config`, `internal/actorproof/...`, `internal/launchgov`, `internal/withdrawal`,
  `internal/alerting/...`, `internal/adjustment`: PASS. `internal/httpserver` S3 tests, OpenAPI drift, role matrix and
  staff identity pins: PASS. The full `internal/httpserver` run (`-timeout 60m`): PASS except
  `TestResolutionIsolation_*` latency thresholds (500 ms / 400 ms), which fail identically at the base `d2cad31` on
  this host (pre-existing, environmental). `go test ./... -count=1`, gofmt, `go vet` (plain and integration) and
  `golangci-lint --build-tags integration --new-from-rev=d2cad31`: clean.
- Review round (section 23.8): the affected packages above, the whole S3 httpserver subset and one full
  `internal/httpserver` run (`-timeout 60m`) were re-run at the final tree: PASS, except the same three
  `TestResolutionIsolation_*` latency thresholds (`NormalOperation`, `OneTenantStoreOutage`, `FinancialDuringOutage`),
  which fail identically at the base `d2cad31` on this host. The whole mutation pass was re-run: 78 mutants, 76
  KILLED, 2 SURVIVED (M13, M35, justified as before), 0 ERROR; every review-round mutant (X2, X3, X6, X7, C1a-f,
  C2a-c, R3a-b, R4a-b, Q12-Q16) is KILLED.
- Mutation pass (first round): `docs/plans/prh2-hardening-round/adr0119-s3-mutation-kill.txt` (driver
  `adr0119-s3-mutation-driver.py`). 56 mutants (45 Go, 11 SQL on a database rebuilt from the mutated 0138): 54 KILLED,
  2 SURVIVED and justified as behaviour-equivalent (M13: the Go absolute-expiry check sits behind the CHECK
  `expires_at <= absolute_expires_at`; M35: app clock vs DB clock on one host), 0 ERROR. The pass found and closed two
  test gaps: a bare `redacted[T]` passed to slog (the `LogValue` path, M19b) and redemption by a suspended staff
  member (M52).

### 23.6 Residuals (S3)

| # | Residual | Disposition |
|---|---|---|
| S3-R1 | No MFA-reset path until S6. The ONLY procedure for a hard-locked factor, or a lost device with no recovery codes left, is the owner procedure of `docs/runbooks/staff-mfa-s3-cutover.md` section 5.1 (also stated in the contract doc): the database owner revokes the factor and its recovery codes by SQL in the staff member's scope, then runs `cmd/seed-admin -issue-bootstrap -operator -reason`; the next password step answers `403 mfa_enrolment_required` and raises the EXPECTED P1 `staff.mfa_integrity` / `staff.platform_mfa_integrity`. The owner SQL writes no audit row of its own (the ticket and the seed-admin audit row are the record). Pinned by `TestStaffMFA_OwnerResetProcedure_S3toS6` | Runbook; S6 replaces it with the audited four-eyes reset |
| S3-R2 | No liveness until S4a: a suspended staff member's access token lives up to 15 min (refresh already re-checks status); `sv` is 0. Since the review round, a logged-out or rotated staff session can no longer be revived by SQL (`revoked_at` / `replaced_by_session_id` write-once, `SF040`, every role: security review R1) | S4a + 0139 |
| S3-R3 | Under M5 the runtime role can still reset the SOFT lock (`failed_count`, `locked_until`); with `SF008` the 24 h hard-lock bound (10 failures) now holds under M5. It can also move `failed_24h_start` forward (delays the window end, never decreases the count) | Accepted with G2; probe live |
| S3-R4 | Alerts are raised post-commit and best-effort: a failed raise is not retried | Matches the existing post-commit alert pattern |
| S3-R5 | The `mfa` bucket is per process and per IP (R9) | R9 |
| S3-R6 | Gate (g) manual authenticator evidence pending | Human |
| S3-R8 | Refresh-time expiry checks (`expires_at`, `absolute_expires_at`) compare against the application clock (pre-existing refresh code); the MFA decisions and `SF008` use the database clock. Pod/DB skew shifts refresh expiry by the skew only | Move refresh to the DB clock with S4a |
| S3-R7 | A runtime session that forges tenant GUCs can still read another tenant's `sessions` rows (hashes and seals only; no MAC key) as before 0138 | Existing M5 residual |
| S3-R9 | (security review R2) A planted bad-seal staff session naming a made-up PLATFORM principal is refused (401) and audited (`auth.session_assurance_invalid`), but its `staff.platform_session_integrity` alert fails the alerts RLS policy (the platform-admin runner's principal does not exist), so only `alerting.raise_failed` is recorded. The tenant variant raises normally | ACCEPTED: the refusal and the audit row stand; the forged principal cannot log in; `alerting.raise_failed` is itself a platform alert |
| S3-R10 | (security review C3) The keyed seal does not cover `refresh_token_hash` or `expires_at`. Both are immutable on staff rows under `SF040` for every role (and the runtime role holds no UPDATE on either; no permissive DELETE or ALL policy exists on `sessions`, pinned by `TestGrants_SessionsAndCutover_RuntimeRole`), so only a party that can disable triggers - the table owner or a superuser (`ALTER TABLE ... DISABLE TRIGGER`) - could rewrite them | S4b condition: bind both into the seal when S4b re-seals on step-up (a seal-format change was not cheap in S3) |
| S3-R11 | (security review R5) Staff refresh does not re-check the factor or the hard lock: an existing session survives a hard lock (or the owner revocation of 5.1) until its 30 min idle or 12 h absolute expiry. The owner procedure of S3-R1 therefore includes a guarded owner revocation of the account's sessions (`UPDATE sessions SET revoked_at = now() WHERE principal_type = 'staff' AND principal_id = ... AND revoked_at IS NULL`, with `app.principal_id` set; the unguarded form fails `SF040` once any session is revoked), and the re-enrolment (`/enrol/confirm`) revokes every session again | S4a (liveness re-check on refresh and on every staff route) |
| S3-R12 | (re-review F1) The C1 binding admits ANY non-empty non-`player` role on a staff principal (an unknown role string holds no permission, so this is not a bypass today) | **S4a condition:** bind staff principals to the closed set of staff roles (the `identity.StaffRole` values), refusing any other role at `Issue`/`IssueStaff`/`Verify` |
| S3-R13 | (re-review F2) `Verify` checks that `amr` is non-empty and that `auth_time` / `mfa_at` are present, but not that `amr` names a second-factor method, nor `auth_time <= mfa_at <= now + skew`, nor `auth_time` within the absolute session TTL. Tokens are only minted by `IssueStaff` from sealed rows, so this matters only with the signing key | **S4b condition (before step-up ships):** require a second-factor method in `amr` (`otp`, or the recovery path's method), `auth_time <= mfa_at <= now + skew`, and `auth_time` no older than the absolute TTL |
| S3-R14 | (re-review note) The write-once rules of `SF040` apply to STAFF rows only: on a player session `revoked_at` is not write-once and a NULL `replaced_by_session_id` can be set by the runtime role. Under M5 this is a denial of service (revoking or chaining a player's session), not a revival (refresh-token reuse detection and the token hash still apply) | Recorded; player sessions are outside ADR 0119 |

### 23.7 What `security` should probe live (gate G)

1. GUC forging against `staff_session_cutover` and `sessions` (read, insert, update of each assurance column, `SF040`
   bypass attempts, the column grant).
2. Seal forgery: a planted seal under another kid, a seal copied between sessions, an `amr`/`aal` change with seal
   recomputation impossible without the MAC key; the C1 rule for a backdated `created_at` (immutable under `SF040`).
3. Timing of `enrol/redeem` and of the password step (enrolled vs not, existing vs not).
4. Rate buckets: `login` shared with players, `mfa` 30/min, `credential` for redeem; header spoofing of the client IP.
5. The `commitFault` seam is test-only (unexported field, set only in `_test.go`) and cannot be reached in production.
6. `SF008` under concurrency (two pods failing and a 24 h roll-over at the same time).
7. Alert raise scope: a forged principal id in a platform alert, and the tenant subject of tenant alerts.
8. The logout change (D13): no session existence oracle.

### 23.8 Review round (2026-10-13): disposition of the review conditions

| Condition | Disposition | Tests |
|---|---|---|
| Security C1: a staff role on a non-staff principal passed admin routes | FIXED: role/principal binding in `Issue`, `IssueStaff` and `Verify` (D20) | `TestIssue_RefusesStaffRoleOnNonStaffPrincipal`, `TestVerify_RefusesHandSignedRolePrincipalMismatch` (hand-signed, 401 through the middleware), `TestRolePrincipalBinding_StaffRoleOnNonStaffPrincipal_401` (HTTP, hand-signed JWT on `/v1/admin/tenants`); C1a-f |
| Security C2 + code review F3: seed-admin skipped the placeholder and distinctness checks | FIXED: `config.ValidateStaffMFAEnv` before `LoadKeys` (D21) | `TestValidateStaffMFAEnv_*` (3), `TestSeedAdmin_BootstrapToken_Subprocess` steps 1b (APP_ENV=production and unset with the `.env.example` keys; a key shared with `JWT_SIGNING_SECRET`); C2a-c |
| Security C3: no runtime path to rewrite `refresh_token_hash` / `expires_at`; seal binding | PINNED: the permissive-policy set of `sessions` (no DELETE / ALL), the runtime UPDATE column set, `expires_at` immutable under `SF040`. Seal binding NOT done (not cheap): S4b condition S3-R10 | `TestGrants_SessionsAndCutover_RuntimeRole`, `TestSessions_SF040Matrix`; Q14 |
| Security R1: un-revoke / chain-link reset revived old refresh tokens | FIXED in 0138: `revoked_at`, `replaced_by_session_id` write-once on staff rows (`SF040`, every role); S3-R2 text updated | `TestStaffMFA_RevocationAndChainLinkWriteOnce` (HTTP: logout and rotation), `TestSessions_SF040Matrix`; Q12, Q13, Q15 |
| Security R2: platform session-integrity alert fails the alerts RLS under a forged principal | ACCEPTED and documented (S3-R9) | - |
| Security R3: redeem did not re-check "no active factor" for bootstrap tokens | FIXED (D23) | `TestStaffMFA_Redeem_BootstrapWithActiveFactorRefused` (tenant, platform); R3a-b |
| Security R4: seed-admin audit rows lacked operator / reason | FIXED: `-operator`, `-reason` (D22) | `TestSeedAdmin_BootstrapToken_Subprocess`; R4a-b |
| Security R5: refresh does not re-check factor or hard lock | DEFERRED to S4a (S3-R11) | - |
| Timing note: the redeem lockout 429 paid no dummy hash | FIXED: every redeem refusal pays one Argon2 verification (D24) | (not measurable in CI; reviewed) |
| Code review F1: liveness refusal on recover, enrol/start, enrol/confirm | TESTED | `TestStaffMFA_SuspendedBetweenSteps_EverySecondStep`; X2, X3, X6 |
| Code review F2: G4 for enrol/confirm | TESTED | `TestStaffMFA_G4_EnrolConfirm`; X7 |
| Code review F4: owner procedure between S3 and S6 | DOCUMENTED identically in the runbook (5.1), S3-R1 and the contract doc; executed by a test | `TestStaffMFA_OwnerResetProcedure_S3toS6` (tenant, platform) |
| Code review F5: S3 merges together with S7a | Orchestrator | - |
| Code review F6: contract doc (redeem never 404; login faults) | FIXED: the contract doc; login faults now answer the fixed 503 (D24) | - |
| Code review F7: assert the 403 code | FIXED | `TestStaffLogin_*` (2) |
| Code review F8: root binaries | FIXED: `.gitignore` gains `/seed-admin`, `/platform-api`, `/migrate` | - |
| Re-review F3: `-operator` / `-reason` character hygiene | FIXED: control, Cf (bidi overrides / isolates, zero-width, U+FEFF, U+061C, ...) and Zl / Zp (U+2028 / U+2029) refused; a superset of `staff_users_display_name_hygiene` | `TestOperatorContext_RefusesUnsafeCharacters`, `TestUnsafeAuditRune_CoversDisplayNameHygiene` (every rune the display-name class refuses); R4c-e |
| Re-review F4: owner session revocation; trigger-disable wording | FIXED: the guarded owner SQL in runbook 5.1 / 5.2 (executed by the procedure test, which also shows the unguarded form failing `SF040`); S3-R10 says "owner or superuser" | `TestStaffMFA_OwnerResetProcedure_S3toS6` |
| Re-review F1 / F2 / player-session note | RECORDED as S3-R12 (S4a), S3-R13 (S4b), S3-R14 | - |
| Code review F9: pre-granted `mfa_at` / seal columns | REMOVED from 0138 and `deploy/init-app-role.sql` (D10); S4b grants them | `TestGrants_SessionsAndCutover_RuntimeRole`, `TestStaffMFA_SessionSealTamper`; Q16 |

## 24. S7a implementation record (2026-10-13)

Branch: the S7a worktree branch on top of `ddd3ed6` (the S3 backend), `backoffice/` only. **NOT MERGED**; S3 and S7a merge
together (section 16). Gate G (live `security` review, `code-reviewer`, `qa`) has not run. Nothing below is an approval.
No backend Go, migration or OpenAPI file was touched; every call matches `docs/api/openapi/platform-api.yaml` (tag
`staff-auth`) and `docs/plans/adr0119-s3-api-contract.md`.

### 24.1 Status labels

| Item | Label |
|---|---|
| Login in two steps (`auth/LoginPage.tsx`): password, then TOTP; "use a recovery code"; challenge-expiry restart; fixed messages for every contract error code | `IMPLEMENTED` (against the S3 API contract; tested with MSW mocks of the API, not against a running backend) |
| Recovery-code flow ends in enrolment, never a session | `IMPLEMENTED` (MSW-tested) |
| Enrolment-link entry (`auth/EnrolPage.tsx`, `/#/enrol?token=` and `/enrol?token=`, token stripped from the URL before the router renders) and `EnrolWizard` (start, secret behind a reveal toggle, copy setup URI, confirm, recovery codes once with copy / download `.txt` / acknowledgement tick) | `IMPLEMENTED` (MSW-tested) |
| QR code rendering of `otpauth_uri` | `NOT IMPLEMENTED` (no QR library is a dependency and none was added; the setup key and a "Copy setup URI" button are the fallback; decision for the owner / architect below) |
| "My security" page (`features/account/SecurityPage.tsx`, `/account/security`, link in the top bar): status, type, activation time, locks, recovery codes remaining with a warning at 3 or fewer | `IMPLEMENTED` (MSW-tested) |
| Recovery-code regeneration on "My security" | `NOT IMPLEMENTED` (the endpoint is S4b and needs step-up; the page shows a labelled note and no action) |
| Own-sessions list on "My security" (ADR 15.4) | `NOT IMPLEMENTED` (out of the S7a request) |
| Step-up modal and `403 step_up_required` handling | `NOT IMPLEMENTED` (S7b) |
| Staff directory, lifecycle, invitations, password redeem (`401 session_revoked`) | `NOT IMPLEMENTED` (S8 / S6 / S4a) |
| End-to-end check against a running platform-api with a real authenticator app | `BLOCKED` on a human (same as entry gate 21.7 (g)); only mocked-API tests exist |

### 24.2 Decisions taken inside S7a (reversible; for the reviewer)

| # | Decision | Why |
|---|---|---|
| U1 | **Both tokens are memory only.** The refresh token moved from `sessionStorage` to a module variable (`auth/tokenStore.ts`). A page reload now ends the session and the user signs in again with a second factor. | Contract section 3: access and refresh tokens are secrets, never `localStorage` / `sessionStorage`. This changes pre-S7a behaviour (silent restore after reload). |
| U2 | **Fail-closed aal2 gate in `setSession`**: a token pair whose access token does not decode to `aal == 2` is refused, the session is cleared and `InsufficientAssuranceError` is thrown; the same function installs refreshed tokens, so a refresh that returns anything else signs the user out. | "Never show a logged-in state without a valid aal2 session." The decode is unverified and a UI convenience; the server stays authoritative. The UI makes no authorization decision from `aal`, `amr`, `sid`. |
| U3 | The recovery codes and token pair are held in `EnrolWizard` state; the session is installed only when the user ticks "I have stored these codes" and presses Continue. | The recovery codes must not vanish because an early session install redirected the page. |
| U4 | `apiFetch` gained an `anonymous` option for the staff-auth calls: no bearer, no refresh-on-401, no session clear on 401, `cache: 'no-store'`. `ApiError` gained `retryAfterSeconds` (from `Retry-After`). | A wrong code answers 401; the old behaviour would have tried a token refresh with a stale token. |
| U5 | The UI branches on `code` only and renders fixed strings (the contract's suggested wording); the server `message` is never shown on these screens. `unauthorized` outside the password step is treated as an expired challenge. Unknown codes collapse to one generic message. | No oracle through message text. |
| U6 | Client-side format checks (6 digits; `XXXXX-XXXXX`) run before any call so a typo does not spend one of the 5 challenge attempts. | The server remains authoritative; a malformed code is also a server `invalid_mfa_code`. |
| U7 | `index.html` gets `<meta name="referrer" content="no-referrer">`. | Defence in depth for the one-time token link and challenge material. |
| U8 | `Open in authenticator app` link is rendered only if the server URI matches `^otpauth://totp/`. | A hostile `javascript:` URI from a compromised response is never made clickable. |

### 24.3 Changed existing tests and assertions

| File | Change | Why |
|---|---|---|
| `src/auth/LoginPage.test.tsx` | rewritten | Login no longer returns a token pair |
| `src/test/handlers.ts` | login handler returns a challenge; handlers for verify, recover, redeem, enrol start / confirm and `GET /v1/staff/me/mfa` added | S3 API |
| `src/test/jwt.ts` | test JWTs carry `aal: 2`, `amr: ['pwd','otp']` | U2 |

### 24.4 Evidence

Run in `backoffice/`: `npx tsc -b`, `npm run build`, `npx vitest run` (29 files, 318 tests, all passing at the S7a commit after the security fixes).
Tests cover every flow and error path of the contract table, keyboard use (Enter submit, focus, tab order), double-submit,
challenge expiry (fake timers), fail-closed aal, no Web Storage writes and no console output containing any secret
sentinel, secrets absent from the DOM after unmount, a hostile otpauth URI, and "My security" warnings and degradation.

### 24.5 Contract gaps and residuals

| # | Item |
|---|---|
| S7a-G1 | The contract says the enrolment link is `/#/enrol?token=`; the back office uses `BrowserRouter`, so `captureEnrolLinkToken` (run in `main.tsx` before render) reads the fragment form, keeps the token in module memory and rewrites the URL to `/enrol` with `history.replaceState`. **Only the fragment form is accepted** (security review F2): a path or query form (`/enrol?token=`) never keeps the token, the URL is rewritten without the query, and the page shows the fixed invalid-link message. S6 emails and the `cmd/seed-admin` bootstrap output must emit `/#/enrol?token=` only. |
| S7a-G2 | No session-liveness signal exists before S4a: a suspended user keeps a working UI until the access token expires. The UI cannot detect it. |
| S7a-R1 | Clipboard and downloaded `.txt` files hold recovery codes outside the app's control; the UI warns but cannot clear them. |
| S7a-R2 | The `otpauth_uri` and setup key are visible in the DOM of the setup step (unavoidable without a QR canvas; a QR would carry the same data). |
| S7a-R3 | Memory-only tokens (U1) mean no silent session restore after a reload; JavaScript in the page origin can still read in-memory tokens (XSS), as before. |
| S7a-R4 | `replaceState` rewrites the current session-history entry only; the browser's global history (address-bar autocomplete, sync) may still hold the original URL containing the one-time token. The token is single-use, bound to the staff member and requires their current password, which limits the exposure. |
| S7a-R5 | **Deploy requirement (F2):** the back-office origin must send `Referrer-Policy: no-referrer` (cutover runbook precondition 5). The `<meta name="referrer">` is now the first element of `<head>` but is not a substitute. |
| S7a-O1 | Owner / architect: add a pinned, zero-dependency QR library (for example `qrcode-generator`) so the QR can be shown locally? Not added: the request said not to add dependencies unless unavoidable. |

### 24.6 Security review of S7a (2026-10-13): APPROVE WITH CONDITIONS - disposition

| # | Finding | Disposition |
|---|---|---|
| F1 | The login challenge-expiry timer kept running in the `enrol` phase and wiped the recovery codes / session after confirm | Fixed: one absolute deadline fixed when the `mfa` challenge is issued (not restarted on step switches), the timer runs only in the `mfa` and `recovery` phases (the server enforces the `enrol` challenge expiry), and a generation counter drops late responses after Cancel or a restart. Tests: 300 s of fake time with the recovery-codes screen shown keeps the codes; single deadline across step switches; late verify response after Cancel. |
| F2 | Query-form enrolment link kept the token; referrer meta not first | Fixed: fragment form only; `<meta name="referrer" content="no-referrer">` is the first element of `<head>`; deploy requirement S7a-R5 recorded in the cutover runbook (precondition 5) and the contract page. |
| F3 | ADR wording on URL stripping; token lifetime in memory | Wording corrected (S7a-R4). The token is cleared from module memory when `EnrolPage` unmounts. A failed redeem does not burn the token (OpenAPI: only success consumes it), so it is kept for a retry while the page is open. |
| F4 | Recovery codes lost on reload | Fixed: a `beforeunload` prompt is active while the codes are shown and not acknowledged (tested). |
| F5 | Cached server data survived a session end | Fixed: `AuthProvider` clears the React Query cache whenever the session is null (logout, refresh failure, aal reject; tested). |
| F6 | Dead bootstrap-refresh branch and stale comments | Removed; `tokenStore` removes the legacy `sessionStorage` key `backoffice.refresh_token` once on load. |
| F11 | Object URL revoked synchronously; clipboard advice; unmount cleanup claim | The object URL is revoked after 1 s; the setup-URI copy notice tells the user to clear the clipboard; unmount cleanup is described as best-effort (no scrubbing guarantee in JavaScript). |
| F12 | Accessibility | Error text is linked to inputs with `aria-describedby`; the setup key has a visible label instead of an `aria-label` on a `<p>`. |
| F13 | Recorded recommendation for S7a-O1 (not built, not blocking) | If a QR is added: vendor the Nayuki QR Code generator (MIT, a single TypeScript file with its licence header) rather than a new npm dependency; render from the module matrix as React `rect` elements or a canvas, never by injecting SVG or HTML strings; no network QR service. `NOT IMPLEMENTED` today. |

## 25. S4a implementation record (2026-10-13)

Branch `worktree-agent-a4112bbe45cc66a8e`, based on `e77900e` (S3 + S7a). **NOT MERGED.** Gate G (live `security`
review, `code-reviewer`, `qa`) has not run. Nothing below is an approval. **No migration** (0139 stays reserved for
S5/S6; 0140-0142 for the unmerged AML work). Route-registration freeze and the classification rationale:
`docs/plans/adr0119-s4a-route-registration.md`.

### 25.1 Status labels

| Item | Label |
|---|---|
| `staffRoute(deps, class, inner)` (`internal/httpserver/staff_route.go`): `auth.Middleware -> auth.RequireStaffPrincipal -> requireLiveStaffSession -> auth.RequireAAL2 -> inner`; every one of the 178 staff routes (`/v1/admin/...` and `GET /v1/staff/me/mfa`) re-registered through it with a class | `IMPLEMENTED` |
| `RequireLiveStaffSession` (N1): one read-only transaction in the token's own scope and principal, 500 ms statement timeout, connection released before the handler, `401 session_revoked` on refusal, `503` fail-closed on a fault, metric `staff_liveness_decisions_total{outcome,reason}` | `IMPLEMENTED` against the local database |
| Liveness rules: session not revoked and within its absolute lifetime (database clock), staff `active`, role equal to the token's, staff version equal to `sv`, an active second factor that is not hard-locked (S3-R11) | `IMPLEMENTED` (staff version: see D1) |
| `auth.RequireAAL2`; `auth.StaffSession` attached to the request context by `auth.Middleware` | `IMPLEMENTED` |
| Closed staff role set (S3-R12): `auth.StaffRoles` / `IsStaffRole`; `Issue`, `IssueStaff` and `Verify` refuse a staff principal with any other role | `IMPLEMENTED` |
| Staff refresh re-checks the second factor (S3-R11): a revoked or hard-locked factor refuses the refresh (403, like a suspended account) | `IMPLEMENTED` |
| Golden classification table (`staffRouteGolden`, 178 routes: 108 mutating, 70 reads) and the static completeness test, with planted-offender negative controls | `IMPLEMENTED` |
| `liveIfStaff` on `GET /v1/me/sessions` and `DELETE /v1/me/sessions/{id}` (a staff caller must be live; players unchanged) | `IMPLEMENTED` |
| Ending a staff session ends its whole rotation chain (security review C1): logout, `DELETE /v1/me/sessions/{id}` and refresh-token reuse revoke every predecessor and successor, so an access token minted before a refresh is refused too; `GET /v1/me/sessions` lists chain heads only (every principal) | `IMPLEMENTED` |
| `apierror.CodeSessionRevoked` (`session_revoked`, 401) and its OpenAPI documentation (bearer scheme, `/v1/staff/me/mfa`, `Error.code` enum) | `IMPLEMENTED` |
| Alert rules `StaffLivenessErrors` (page) and `StaffLivenessRefusals` (ticket) | `IMPLEMENTED` as rule text; `NOT IMPLEMENTED` as delivery (ALERT-DELIVERY-1, unchanged) |
| Step-up enforcement (`RequireStepUp`), the four classifier functions, the signer backstop, the scoped `FOR SHARE` re-assertion, `staff_actor_self_*` policies, ADR 0082 A9 / ADR 0099 A-19 | `NOT IMPLEMENTED` (S4b) |
| Lifecycle, invitations, MFA reset, `staff_version` column | `NOT IMPLEMENTED` (S5 + S6, migration 0139) |
| Back office handling of `401 session_revoked` | `NOT IMPLEMENTED` (S7b / S8; the S7a UI already treats any 401 on a bearer call as a session end) |

### 25.2 How the S3 review conditions hold

| Condition | Mechanism | Tests |
|---|---|---|
| S3-R11 (liveness re-checks factor / hard lock; refresh re-checks) | `usableFactorPredicate` (`f.status = 'active' AND f.hard_locked_at IS NULL`) in the liveness query and in the staff refresh | `TestStaffLiveness_RefusalMatrix` (hard lock, factor revoked by the S3-R1 owner procedure), `TestStaffRefresh_RechecksFactor`; mutants L06, L08, L09, L32 |
| S3-R12 (closed staff role set) | `rolePrincipalValid` uses `IsStaffRole`; parity with `identity.StaffRole` and the latest `staff_users_role_check` | `TestStaffRoles_ClosedSet_ParityWithIdentityAndDatabase`, `TestStaffPrincipal_RoleOutsideClosedSetRefused`, the DB-free sweep (`made_up_role` is 401 on every route); A01, A02, A09, A10 |
| S3-R13 (amr / auth_time / mfa_at checks) | Not in S4a: an S4b condition, before step-up ships | - |
| S3-R8 (refresh on the database clock) | Partly: liveness compares `absolute_expires_at` with the database clock; the refresh path still uses the application clock | L10 |
| S3-R2 (no liveness until S4a) | Closed: a suspended, offboarded or role-changed staff member's next request is refused, and so is every access token of a logged-out, deleted or reuse-revoked session chain. **Correction (security review C1):** the first S4a cut claimed "logged-out" here, but an access token minted before a refresh names the replaced predecessor row, which logout did not revoke; it was honoured (200 on a read, 204 on a write) until the chain revocation below | `TestStaffLiveness_RefusalMatrix`, `TestStaffLiveness_EveryGoldenRoute`, `TestStaffLiveness_SessionEndRevokesWholeChain`; C1a-C1f |
| S3 D19 (`GET /v1/staff/me/mfa` without liveness) | Closed: the route is a `staffRoute` | the sweep and the matrix |

### 25.3 Deviations (for the reviewer; each reversible)

| # | Deviation | Reason |
|---|---|---|
| D1 | `sv` is compared with the constant `staffVersionUnversioned` (0): the liveness query reads no `staff_version` column, and a token whose `sv` is not 0 is refused | `staff_users.staff_version` arrives with 0139. When it lands, the query selects the column and nothing else changes (`staffLivenessRow.staffVersion`). Until then a `person_id` or password change is NOT detected by liveness (S4a-R1) |
| D2 | The statement timeout is set in its own round trip before the batch (scope GUCs + SELECT stay one batch) | Found by the S4a fault test: pgx sends the SELECT's `Parse` before the batch's `set_config` runs, and `Parse` already takes the table locks, so a timeout queued in the same pipeline did not bound a query not yet cached on the connection (2 s instead of 500 ms). Mutant L18b reproduces the defect and is killed |
| D3 | Each route keeps its own inner order of `auth.RequireTenantScope` and `auth.RequirePermission` (section 7.3 shows permission first) | Behaviour of every existing route is unchanged behind the chain |
| D4 | Liveness also refuses when no active, non-hard-locked factor exists; a soft lock (`locked_until`) does not end sessions | S3-R11. A soft lock is a guessing throttle; making it end sessions would let a password holder log the victim out |
| D5 | One `401 session_revoked` for every refusal cause (and for a request with no verified staff session in its context); the cause goes to the metric and the `staff_liveness_refused` log line, never to the client. Refusals are not written to `audit_log` | No oracle; no write on the read path. See S4a-R3 |
| D6 | `GET /v1/me/sessions` and `DELETE /v1/me/sessions/{id}` use `liveIfStaff` | Section 7.3's list does not name them, but a revoked staff token must not list or touch sessions |
| D7 | ADR 0115 S2 routes that arrived before S4a are classified here: `POST /v1/admin/players/{id}/rg/limits` classifier `rgLimitChangeClass`; RG floor request / approve and `limit-configs` writes `critical`; floor reject / cancel `none` | Section 8.2 "classified on arrival" |
| D8 | A classifier route is recorded as `classifier:<name>` (ceiling `critical`); the four functions are S4b | Section 16 S4a: classes recorded only |
| D9 | OpenAPI documents `session_revoked` once on the `bearerAuth` scheme and on `/v1/staff/me/mfa`, not on every staff operation | Section 14.4's per-route documentation is an OpenAPI pass for S6 (S4a-R8) |
| D10 | `staff_actor_self_select` / `staff_actor_self_lock` are NOT added | The liveness query runs in the token's own scope, where the dual-scope policy already shows the caller's own row; the policies are needed only by S4b's in-transaction re-assertion under `WithTenant(target)` |
| D11 | The "alert" of N1 is a Prometheus rule on the metric, not a durable alert kind | A durable kind needs a migration; S4a has none. The rules are unwired like every rule in that file (ALERT-DELIVERY-1) |
| D12 | The staff refresh still rotates before the status / role / factor re-check (pre-existing order). A refused refresh leaves a rotated successor whose token was never delivered; a retry with the old refresh token is then reuse (chain revoked, audited) | Fail-safe; reordering the refresh is outside S4a (S4a-R6) |
| D13 | `authtest` gains `LiveStaffToken`, `LiveStaffSession`, `ClaimedStaffToken` and `EnsurePlaceholderFactor` (integration tag only). The placeholder factor has junk ciphertext and can never verify a code | Section 4.1 said S4a changes only this helper. Harness tokens are now claims-faithful: a live session when the staff row is visible in the claimed scope, a bare token otherwise |
| D14 | (security review C1) The chain revocation applies to STAFF sessions only (`revokeLinkedStaffSessions`, one recursive CTE over `replaced_by_session_id` in both directions, guarded `revoked_at IS NULL`); reuse detection for staff also revokes the predecessors; `ListActiveSessions` excludes replaced rows for every principal | Player access tokens are not checked by liveness, so a player chain needs no change; a replaced row is a past link, not a device |
| D15 | (code review CR 3) A liveness error while the request's own context is done is `reason="client_cancelled"`, logged at Info as `staff_liveness_cancelled`; only `db_error` pages (`StaffLivenessErrors`, minimum 3 in 5 min); `StaffLivenessRefusals` needs both a count over 25 and a 20 % share of all decisions | A client that goes away is not a database fault |
| D16 | (code review CR 4, security review R3(a)) Each liveness decision log line carries `staff_id`, `token_tenant` (a tenant id or `platform`), `sid`, `method`, `route` (the mux pattern) and `client_ip` (trusted-proxy aware); `request_id` and the logging `tenant_id` come once from the request logger | Investigation without the token |
| D17 | (code review CR 2) The static scan requires every pattern of the package to be exactly `<GET|POST|PUT|PATCH|DELETE> /<path>` with literal first two path segments, and a repository-wide scan finds any route registration (`http.NewServeMux`, `http.Handle(Func)`, `X.Handle(Func)("literal", ...)`) outside `internal/httpserver` | A tab, double space, host or leading wildcard could otherwise reach a staff path unclassified |

### 25.4 Changed existing tests and assertions (every one)

Assertions:

| File | Change | Why |
|---|---|---|
| `internal/httpserver/launch_authorisation_routes_test.go` `TestLaunchRoutes_PermissionPerRoute_Pinned` | the route regex reads `staffRoute(deps, <class>, auth.RequirePermission(<perm>)(`; the wrapper pin becomes "no `auth.Middleware(` in the file and exactly one `staffRoute` per route" | the chain moved into `staffRoute` (pinned by `TestStaffRoute_ChainComposition`) |
| same file, `TestLaunchRoutes_NilService_503_DBFree` | a staff token without a database is `503 service_unavailable` from liveness (was `503 LAUNCH_AUTHORISATION_UNAVAILABLE` from the handler); player 403 and anonymous 401 unchanged | liveness runs before the handler and fails closed without a database |
| `internal/httpserver/payment_attempts_gate_static_test.go` `TestPaymentAttemptRoutes_GatedOnForceResolveReadAlone` | the registration regex reads `staffRoute(deps, stepUpNone, ...)` | same; the gate assertion is unchanged |
| `internal/httpserver/alert_admin_layers_integration_test.go` `TestAlertAdmin_EachAuthorizationLayerIsPinnedOnItsOwn` | both layer tokens (platform compliance; platform admin under a tenant claim) are `401 session_revoked` over HTTP; each layer's 403 is asserted behind the chain (`behindChain`) | neither claim combination can belong to a live session |
| `internal/httpserver/alert_routing_audit_integration_test.go` `TestAlertAdmin_DeniedAuditNeverRecordsAHostilePathSegment` | the compliance-claim token is 401 over HTTP; its platform-scope denial and audit target are asserted behind the chain; the tenant token belongs to a real tenant admin row | same |
| `internal/httpserver/alert_routing_http_integration_test.go` `TestAlertRoutingAPI_Authorization_EachLayer_DeniedAuditedInCallerScope` | the two mismatched-claim tokens are 401 over HTTP for the mutation and the reads; the 403s and the caller-scope denied audit rows come from `behindChain`; the tenant admin is a real row | same; the audit-count assertions are unchanged |
| same file, `TestAlertAdmin_RefusedAckAndResolveAreAudited_AckTakesAReasonCode` | the permission and scope refusals are 401 over HTTP and 403 behind the chain | same; the audit counts (1 success, 4 denied, 1 tenant row) are unchanged |
| `internal/httpserver/rg_limits_config_integration_test.go` `TestRGLimitsS2_Floor_RejectCancelAndValidation` | a platform staff id under a tenant claim is `401 session_revoked` (was `403 forbidden`) | no live session matches it |
| `internal/httpserver/sportsbook_settlement_flow_integration_test.go` `TestSettlementSimulate_DeactivatedActor_403AndAuditRecorded` | over HTTP: 401 and no `settlement_rejected` row; behind the chain: 403 and exactly one `ACTOR_NOT_ACTIVE` row (security review P3-2's in-handler audit kept pinned) | the suspension ends the session on the next request |
| `internal/httpserver/stage3c_withdrawal_resolution_test.go` `TestWithdrawalResolve_SuspendedStaffAccountRejected` | `401 session_revoked` (was 403) | same |
| `internal/httpserver/stage3d_withdrawal_governance_test.go` `TestWithdrawalApprove_InactiveStaffAccountRejected` | `401 session_revoked` (was 403); zero approval rows unchanged | same |
| `internal/httpserver/staff_mfa_hygiene_integration_test.go` `TestStaffMFA_SecretHygiene_LogCapture` | after the refresh-token reuse, `GET /v1/staff/me/mfa` with the chain's access token is `401 session_revoked` (was 200); My security is exercised with a fresh login whose tokens and code join the secret scan | section 6.6 side effect: reuse detection revokes the chain, and liveness now enforces it |

Review round (no existing assertion changed; assertions ADDED to existing tests):

| File | Addition | Why |
|---|---|---|
| `internal/httpserver/stage3d_withdrawal_governance_test.go` `TestWithdrawalApprove_InactiveStaffAccountRejected` | behind the chain: 403 "this staff account is not active" (the domain `withdrawal.Approve` check, `ErrApproverInactive`) and zero `withdrawal_approvals` rows | code review CR 1 (mutant X1) |
| `internal/httpserver/stage3c_withdrawal_resolution_test.go` `TestWithdrawalResolve_SuspendedStaffAccountRejected` | behind the chain: 403 and the withdrawal stays `submitted` | CR 1 (mutant X2) |
| same file, NEW `TestWithdrawalSubmit_SuspendedStaffAccountRejected` | 401 over HTTP; behind the chain 403 and the withdrawal stays `approved` | CR 1 (mutant X2b) |
| `internal/httpserver/staff_liveness_integration_test.go` `TestStaffLiveness_RefusalMatrix` | the refusal log line carries staff id, tenant scope, sid, method, route, client IP; one `request_id`; never the token | CR 4 |

`deploy/observability/prometheus-alert-rules.yml`: the two S4a rule expressions changed (D15). `financial_flow_integration_test.go`
gains `financialTestDeps` (the same Deps as `newFinancialTestServer`), `withdrawalStateOf` and `suspendedFinanceContext`.

Setup only (no assertion changed):

- `internal/httpserver/staff_mfa_flow_integration_test.go` `TestStaffMFA_SessionSealTamper`, `TestStaffMFA_RefreshBounds`: the staff members whose sessions are planted get a placeholder factor (a staff refresh now needs a usable factor).
- Harness token helpers in `capability_api_`, `provider_credential_api_`, `alert_routing_http_`, `payments_kill_switch_api_` and `rg_limits_harness_integration_test.go`: `authtest.ClaimedStaffToken`.
- `internal/httpserver/staff_read_routes_integration_test.go` `newSRWorld`: the risk-manager, promotions-manager and bonus-operations tokens belong to real staff rows (were random subjects).
- `alert_routing_http_` / `alert_routing_audit_integration_test.go`: tenant-admin tokens for real rows (`arAPI.tenantStaff`); `arAPI` keeps its `Deps` for `behindChain`.

No other existing assertion changed: the role matrix (`TestADR0114_RoleMatrix_AgainstRolePermissions` and the permission tests), the OpenAPI drift tests (`TestOpenAPI_*`) and the staff identity pins pass unchanged.

### 25.5 Evidence (private databases; no role, password or privilege change)

All integration runs: `-tags integration -p 1 -count=1`, as `igaming_runtime` against private databases built from this
tree (`priv_db.sh` / `priv_test.sh`; databases `s4a_live_a4112`, `s4a_pkg_a4112`, `s4a_mut_a4112`, `s4a_base_a4112`).

| Command | Result |
|---|---|
| `go test ./... -count=1` | PASS |
| `gofmt -l .`; `go vet ./...`; `go vet -tags integration ./...` | clean |
| `golangci-lint run --build-tags integration --new-from-rev=e77900e ./...` (2.9.0) | 0 issues |
| integration: `./internal/auth/... ./internal/staffmfa/... ./internal/tenant/... ./internal/db/... ./cmd/... ./internal/config/... ./internal/observability/... ./internal/apierror/... ./deploy/observability/` | PASS |
| integration: `./internal/httpserver/` full run, `-timeout 60m` (final tree) | PASS except `TestResolutionIsolation_NormalOperation` and `TestResolutionIsolation_FinancialDuringOutage` (500 ms / 400 ms latency thresholds). The same subset at the base `e77900e` on this host, run twice: `NormalOperation` (both runs), `FinancialDuringOutage` and `ConnectionExhaustion` fail. Pre-existing and environmental, as recorded for S3 (23.5) |
| new S4a tests, OpenAPI drift (`TestOpenAPI_*`), role matrix and permission tests, staff identity pins | PASS |

New tests: `internal/auth/staff_chain_test.go` (closed role set and its parity with `identity` and the latest
`staff_users_role_check`; mint and verify refusal of a role outside the set; staff session context; `RequireAAL2`);
`internal/httpserver/staff_route_static_test.go` (coverage scan, golden comparison, mutating-route classification,
closed class set, ten planted offenders plus an unclassified-route mutant and a hand-registered route on the real
sources, chain composition pin, the DB-free sweep of all 178 routes x 7 token shapes, `liveIfStaff`, the
classified-on-arrival list); `staff_route_golden_test.go` (the table); `staff_liveness_unit_test.go` (verdict, query
shape, the middleware without a database); `openapi_staff_liveness_contract_test.go`; `staff_liveness_integration_test.go`
(refusal matrix for tenant and platform staff: logout, suspension, role change then refresh to the new role, `sv`
mismatch, hard lock, revoked factor, unknown / foreign / cross-tenant / cross-scope sid, absolute expiry; a refused write
writes nothing; fault 503 with a dead pool and with a blocked table on a cold and a warm connection, cut at 503 ms by
SQLSTATE 57014; a one-connection pool serves a read and a write; every golden route refuses a revoked session and no
live GET is refused; the refresh factor re-check; EXPLAIN; latency; the read-only helper).

Query plan (`EXPLAIN`, sequential scans disabled to prove index usability): an index scan on each table -
`sessions` (`idx_sessions_principal` on this small table; `sessions_pkey` is equally usable), `staff_users`
(`staff_users_id_tenant_unique`), `staff_mfa_factors` (`staff_mfa_factors_one_active`, `staff_user_id = su.id`) - and
no sequential scan; the default plan is the same. Latency of the liveness check through the helper on the private
database, 300 checks each: tenant staff p50 0.59 ms, p95 1.06 ms, p99 1.32 ms; platform staff p50 0.50 ms, p95 0.84 ms,
p99 1.16 ms (a shared development host; not a production figure).

Mutation pass: `docs/plans/prh2-hardening-round/adr0119-s4a-mutation-kill.txt` (driver
`adr0119-s4a-mutation-driver.py`, run in a copy of the tree against its own database). 44 mutants (34 in
`internal/httpserver` and `internal/db`, 10 in `internal/auth`): 42 KILLED, 2 SURVIVED and justified (L20
behaviour-equivalent through `errors.Is`; L20b not inducible), 0 ERROR. Section 17.8's S4a row ("liveness skips the `sv`
comparison", L05) is KILLED. The pass found one real defect (D2) and two test gaps (A07, L16), all closed.

**Review round (section 25.8), final tree.** `go test ./... -count=1`: PASS. `gofmt`, `go vet` (plain and
integration): clean. `golangci-lint --build-tags integration --new-from-rev=e77900e`: 0 issues. Integration:
`./internal/auth/... ./internal/staffmfa/... ./internal/tenant/... ./internal/db/... ./cmd/... ./internal/config/...
./internal/withdrawal/... ./internal/observability/... ./internal/apierror/... ./deploy/observability/`: PASS. Full
`./internal/httpserver/` (`-timeout 60m`, overlapping the mutation pass): PASS except `TestResolutionIsolation_*`
latency thresholds (`NormalOperation`, `OneTenantStoreOutage`, `MultipleTenantsOutage`), which use no staff route
(player and webhook routes only) and fail at the base too; every S4a, withdrawal, OpenAPI and staff-identity test
rerun on the final tree: PASS. Mutation pass over all 60 mutants (the 44 above plus C1a-C1f, X1, X2, X2b, X3, X3b, X4,
X4b, K1-K3): 58 KILLED, 2 SURVIVED (L20, L20b, justified), 0 ERROR (the first hand-back said 62 / 60: a counting error). The C1 regression test found a second real defect
on the way: the logout of an already-revoked staff head returned its "not found" from the transaction closure, which
rolled the chain revocation back (fixed; C1d).

### 25.6 Residuals (S4a)

| # | Residual | Disposition |
|---|---|---|
| S4a-R1 | `sv` is always 0 until 0139: a `person_id` or password change does not end sessions through liveness (a role or status change does) | 0139 (S5 + S6) bumps `staff_version` and the query reads it (D1) |
| S4a-R2 | In-handler authorization layers that only mismatched claims could reach (alert layers 1 and 2, the settlement `ACTOR_NOT_ACTIVE` audit, the withdrawal approve / submit / resolve suspended-account checks) are now unreachable over HTTP; they stay as defence in depth for the R13 window. Each is pinned behind the chain (`behindChain`), the withdrawal ones since the review round (code review CR 1; X1, X2, X2b) | Accepted |
| S4a-R3 | Liveness refusals are logged and counted, not written to `audit_log`; security review P3-2's durable audit row for a deactivated actor now arises only inside the R13 window | **ACCEPTED by `security`** on two conditions: (a) the refusal log line identifies the request (D16, done); (b) **launch condition L2:** ALERT-DELIVERY-1 must wire `StaffLivenessRefusals` and `StaffLivenessErrors` to a named on-call before launch |
| S4a-R4 | Step-up classes are recorded, not enforced; classifier routes are not evaluated | S4b |
| S4a-R5 | The alert rules are unwired | ALERT-DELIVERY-1 (unchanged) |
| S4a-R6 | The staff refresh rotates before its re-checks (D12; security review F2) | Pre-existing; S4b or S6 |
| S4a-R7 | Under M5 (arbitrary SQL as the runtime role) liveness reads runtime-writable state (`staff_users.status` / `role`; R1) and adds nothing against M5 | R1 / S-H1 (owner) |
| S4a-R8 | OpenAPI documents `session_revoked` globally, not per operation (D9) | S6 OpenAPI pass |
| S4a-R9 | One extra short transaction per staff request; under pool exhaustion the 2 s liveness deadline answers 503 | Accepted (section 19); metric and alert |
| S4a-R10 | (security review, recorded for S4b) S3-R13 is still open: S4b's `RequireStepUp` and the signer backstop must take `mfa_at` and `auth_time` from the SESSION ROW (sealed), not from the token claims, and check `auth_time <= mfa_at <= now + skew` | S4b condition |
| S4a-R11 | **Launch condition L2** (security review): the S4a alert rules are unwired (S4a-R5); delivery of `StaffLivenessRefusals` / `StaffLivenessErrors` to a named on-call is required before production launch | ALERT-DELIVERY-1 (owner / ops) |

### 25.7 What `security`, `code-reviewer` and `qa` should probe

1. Try to reach any staff handler without the chain: a route registered in another package, a `mux.Handle` built from a
   non-literal pattern, a pattern without a method, a route under a prefix the static test does not cover.
2. Forge every claim combination against the liveness query (sid of another principal, another tenant, a platform sid
   under a tenant claim, a tenant sid under a platform claim, a role outside the closed set, `sv` other than 0) with GUC
   forging on the same connection impossible (the liveness transaction sets its own GUCs and commits).
3. Statement-timeout and pool behaviour: cold vs warm connections, a starved pool (the 2 s deadline), `40001` / commit
   failure of the read-only transaction, cancellation mid-batch; confirm nothing ever passes on an error.
4. The `behindChain` pins (S4a-R2): is each in-handler layer still worth keeping, and is the pin faithful to the route's
   inner chain?
5. Classification review of the golden table against section 8.1, in particular the ADR 0115 S2 rows (D7) and the
   reads that are now on the chain.
6. The route-registration freeze: the in-flight branches (ADR 0114 S1b, ADR 0116 AML HTTP, ADR 0115 S5) must rebase and
   register through `staffRoute`.

### 25.8 Review round (2026-10-13): disposition of the review conditions

`security` (live) and `code-reviewer` / `qa`: APPROVE WITH CONDITIONS. Applied on the branch; the security re-review (25.9) added four more items.

| Condition | Disposition | Tests / mutants |
|---|---|---|
| Security C1 (merge condition): a pre-refresh access token survived logout / DELETE (200 on a read, 204 on a write) | FIXED: ending a staff session revokes its whole chain, both directions, on logout, DELETE and reuse; the list shows chain heads only (D14); the 25.2 claim corrected | `TestStaffLiveness_SessionEndRevokesWholeChain` (tenant, platform: logout, delete, delete a middle link, reuse, logout after the head was revoked); C1a-C1f |
| Code review CR 1 (blocking): no pins for the withdrawal approve / submit / resolve suspended-account checks | FIXED: behind-chain pins (403, zero approval rows, state unchanged); S4a-R2 and 25.4 corrected | the three withdrawal tests; X1, X2, X2b |
| CR 2: pattern shapes that would escape the scan; registrations in other packages | FIXED: strict pattern shape and literal first two segments for every pattern of the package; repository-wide registration scan with an allowlist (D17) | eight new planted offenders, `TestStaffRoutes_NoRouteRegistrationOutsideHTTPServer`; X3, X3b, X4, X4b |
| CR 3: a cancelled request counted as a database error | FIXED: `client_cancelled` at Info; alert thresholds with a minimum count and a ratio (D15) | `TestRequireLiveStaffSession_FaultLogFieldsAndClientCancellation`; K1 |
| CR 4 + security R3(a): the refusal log line did not identify the request; `request_id` twice | FIXED (D16) | same test, `TestStaffLiveness_RefusalMatrix`; K2, K3 |
| CR 5: documentation drift | FIXED: the plan doc states the timeout's own round trip (D2) and four classifier functions; the S4b slice row says four classifiers | - |
| Security L2 (launch condition) | RECORDED: S4a-R11; S4a-R3 ACCEPTED on (a) D16 and (b) L2 | - |
| Security note on S3-R13 | RECORDED: S4a-R10 (S4b must use the sealed session row, not the claims) | - |
| Security F2 (refresh rotates before its re-checks) | Stays S4a-R6 (S4b / S6) | - |

### 25.9 Security re-review (2026-10-13): APPROVE WITH CONDITIONS - disposition

| Item | Disposition | Tests / mutants |
|---|---|---|
| 1 (merge condition, reproduced live): ending an OLDER link while a refresh of the head runs left the new head live (one CTE statement walks one READ COMMITTED snapshot; the refresh's new head commits while the statement waits on the head's row lock) | FIXED by a re-snapshot loop (`revokeLinkedStaffSessions`): the CTE repeats until a pass revokes nothing, each pass a new snapshot; bounded at 50 passes, beyond which `ErrChainRevocationUnstable` rolls the whole transaction back (nothing partially revoked is committed; the logout / DELETE / refresh answers 5xx and is retried). Chosen over locking the head first because the head is only known from a snapshot that may already be stale, and the loop also covers refreshes of later heads; a refresh that has not yet locked the head cannot succeed after the revocation commits (its conditional UPDATE needs an unrevoked head) | `TestStaffLiveness_ChainRevocationRacesRefresh` (a held head-row lock; the refresh queued first, the session end second, detected through `pg_locks`; variants delete-predecessor, logout-old-refresh; controls logout-head, delete-head, reuse-old; the new head's access AND refresh token refused, every row revoked); C1g (single snapshot) KILLED |
| 2: DELETE of one's own already-revoked head answered 404 before the chain step, leaving older links live | FIXED: `RevokeSession` runs the staff chain revocation whatever the presented row's state; 204 when anything was revoked, 404 when nothing was left (logout keeps its idempotent 204). The logout-specific fallback is gone (the same code path now) | `TestStaffLiveness_DeleteRevokedHeadEndsOlderLinks`; C1a, C1d KILLED |
| 3: `StaffLivenessRefusals` could be masked by a high share of legitimate traffic | FIXED: `... or sum by (reason)(increase(staff_liveness_decisions_total{outcome="refused"}[10m])) > 200` | rules parity PASS |
| 5: `revokeChainFrom` (1000 hops) rolls back reuse revocation on a very long forward chain | Staff: FIXED, reuse detection on a staff chain uses the re-snapshot CTE (no hop bound, both directions). Players: recorded as S4a-R12 | C1e KILLED |
| Note: non-literal patterns outside `internal/httpserver` | Recorded as S4a-R13 | - |

| # | Residual | Disposition |
|---|---|---|
| S4a-R12 | Refresh-token reuse on a PLAYER chain longer than 1000 links (`revokeChainFrom`'s `maxChainHops`) errors and rolls the reuse revocation back (pre-existing; player access tokens are not liveness-checked, the stolen refresh token is still refused as reused) | Pre-existing; fix with the CTE approach if player chains ever need liveness |
| S4a-R13 | The repository-wide registration scan finds route registrations whose pattern is a string LITERAL (and every `http.NewServeMux` / `http.Handle(Func)`); a non-literal pattern passed to `X.Handle` outside `internal/httpserver` is not recognised as a route (inside the package every non-literal pattern fails the scan) | Known limit; no such call exists today |

Evidence (final tree): integration `./internal/auth/...`, `./internal/withdrawal/...`, `./deploy/observability/`: PASS;
the S4a `internal/httpserver` tests (chain, race, liveness, routes, withdrawal pins, OpenAPI, staff identity pins):
PASS; `go test ./... -count=1`, `gofmt`, `go vet` (plain and integration), `golangci-lint --new-from-rev=e77900e`:
clean. Mutation (new logic): C1a, C1b, C1c, C1d (re-anchored to the new code), C1e, C1f and the new C1g: all KILLED;
the mutant set is now 61 (L 34, A 10, C1 7, X 7, K 3).

## 26. S4b implementation record (2026-10-13)

Branch `worktree-agent-aa30e7cd9d960921a`, based on `3019e51` (head 142: S1-S4a + S7a merged). **NOT MERGED.** Gate G
(live `security` review, `code-reviewer`, `qa`) has not run; **`ledger-finance` review is mandatory** (section 9.5,
ADR 0082 Amendment A9, the K-flow integration) and `architect` sign-off is required (ADR 0099 A-19 amendment, the
decision NOT to build the section 9.5(b) policy pair in S4b, the `internal/stepup` package). S4b merges together with S7b (the back-office step-up modal, a separate agent;
contract `docs/plans/adr0119-s4b-api-contract.md`). Nothing below is an approval. **Migration 0151** (the number the
orchestrator allocated: 0139 stays reserved for S5 + S6, 0143-0150 for ADR 0116 AML and other in-flight slices).

### 26.1 Status labels

| Item | Label |
|---|---|
| `requireStepUp(class)` as the last link of `staffRoute` (critical 5 min, financial 10 min, configurable down to 1 min; `STAFF_STEP_UP_*_MAX_AGE_SECONDS` wired in `cmd/platform-api`), `403 step_up_required` with `max_age` and the RFC 9470 `WWW-Authenticate` header, enforced on every one of the 180 golden routes by class | `IMPLEMENTED` |
| The four classifier functions (`launchActionClass`, `financialPolicyChangeClass`, `alertResolveClass`, `rgLimitChangeClass`); the classifier reads the SAME body bytes the handler decodes, with the same decoder and type | `IMPLEMENTED` |
| `POST /v1/staff/auth/step-up` (TOTP only; replay rule and failure counters shared with login; writes only the chain-head row; new access token, refresh token unchanged; the failure that locks the factor revokes the session chain) | `IMPLEMENTED` |
| `POST /v1/staff/mfa/recovery-codes/regenerate` (step-up critical; the S3 contract left it for S4b, section 23.3 D3) | `IMPLEMENTED` |
| Conditions S3-R13 / S4a-R10: the freshness instant is the OLDER of the token's `mfa_at` and the sealed session row's; liveness verifies the row's seal and coherence (aal 2, `amr` names a second factor, `auth_time <= mfa_at <= now + 5 s`, `auth_time` within the configured absolute TTL) on the database clock; `auth.Verify` / `IssueStaff` refuse an `amr` without a second factor, `mfa_at < auth_time`, a future `auth_time` / `mfa_at` beyond 5 s, an `auth_time` older than 12 h | `IMPLEMENTED` |
| Condition S3-R10: seal format v2 binds `refresh_token_hash` and the idle `expires_at`; a genuine v1 (S3) seal is retired quietly (liveness `seal_legacy`, refresh a plain 401, no audit, no alert) | `IMPLEMENTED` |
| Migration 0151: runtime column UPDATE of `mfa_at`, `assurance_kid`, `assurance_seal` ONLY (mirrored in `deploy/init-app-role.sql`, granted there only once version 151 is applied); down migration | `IMPLEMENTED` against the local database |
| Section 9.5(b) `app.actor_staff_id` GUC and the `staff_actor_self_select` / `staff_actor_self_lock` policy pair | `NOT IMPLEMENTED` (not required: D14, S4b-R3) |
| Signer backstop: `actorproof.StepUpOperations` (16 operations, section 9.1 minus `staff_lifecycle:*`), applied in `Issuer.Attach` (the only production signing path, pinned) | `IMPLEMENTED` |
| `withdrawal.Approve` step-up parameter: `require_step_up` above the threshold needs the approver's own step-up within 2 min (database clock); automated decisions never satisfy it; HTTP `403 step_up_required`, `max_age = 120` | `IMPLEMENTED` |
| Section 9.5 scoped re-assertion folded into the existing A8 statement for K2, K3/M4 and HSEC final approvals; `approverEligibilityCheck` for withdrawal approve made `FOR SHARE` with a role comparison (`db.CheckActorStaffLive`; `db.ErrActorStaffNotLive` -> `401 session_revoked`); ADR 0082 Amendment A9 and the ADR 0099 A-19 amendment written | `IMPLEMENTED` (pending `ledger-finance` and `architect` sign-off) |
| Section 9.5 re-assertion for withdrawal `submit` / `resolve` and K1 grant approve / revoke | `NOT IMPLEMENTED` (S4b-R3) |
| `staff_lifecycle:*` in the backstop registry; the lifecycle flows; `staff_version` | `NOT IMPLEMENTED` (S5 + S6, migration 0139) |
| ADR 0115 staff RG increases | `NOT IMPLEMENTED` by design: they stay refused (`403 RG_STEP_UP_REQUIRED`) until ADR 0115's own classifier slice (S4b-R4) |
| Back-office step-up modal | `NOT IMPLEMENTED` (S7b, separate agent; contract written) |
| Production key provisioning; real email | `PROVIDER DEPENDENT` (unchanged) |

### 26.2 How the review conditions hold

| Condition | Mechanism | Tests / mutants |
|---|---|---|
| S3-R10 (seal binding) | Seal format v2 (`ss-v2|...|refresh_token_hash|expires_at`, `staffmfa.SessionAssurance`); every new or rotated staff session and every step-up re-seal is v2; `VerifySessionSeal` accepts v2 only; a genuine v1 seal is recognised (`IsLegacySessionSeal`) and retired without a P1 | `TestSealSession_*` (canonical v2 form pinned, every field bound, malformed hash/expiry refused, legacy recognised but never valid), `TestStepUp_SealBindsRefreshHashAndIdleExpiry` (owner rewrites either column with the SF040 trigger disabled: refresh refused + audited + P1; liveness `seal_invalid`), `TestStepUp_LegacyV1SealRetiredQuietly`; SE01-SE05 |
| S3-R13 (token claims) | `auth.Verify` / `IssueStaff`: `amr` must name a second factor (`otp`), `auth_time <= mfa_at`, neither more than 5 s in the future, `auth_time` no older than 12 h (the absolute-TTL bound; the configured, shorter, absolute expiry is enforced on the sealed row) | `TestVerify_StaffAssuranceCoherence_S3R13`, `TestIssueStaff_RefusesIncoherentAssurance_S3R13`, `TestStepUp_ForgedAndIncoherentClaims` (hand-signed tokens: 401); V01-V04 |
| S4a-R10 (row, not claims) | Liveness reads the row's sealed assurance with the database clock, verifies the v2 seal and the row's coherence (new liveness reasons `seal_invalid`, `seal_legacy`, `assurance_incoherent`) and hands the VERIFIED row to the gate; the gate uses the OLDER of the token claim and the row (a forged fresh claim over a stale row is refused; a token minted before a step-up stays stale) | `TestStaffAssuranceVerdict`, `TestStepUp_ForgedAndIncoherentClaims` (fresh claim, 11-minute-old row: 403), `TestStepUp_EndToEnd_TenantAndPlatform` (old token stays stale after the step-up); G01, G02, L01-L05 |
| Step-up writes only the chain head | `auth.StepUpStaffSession`: the caller's own row (`id`, `principal_id`, tenant) locked `FOR UPDATE`, refused unless not revoked, not replaced and within its absolute expiry, its v2 seal verified, then `UPDATE ... SET mfa_at, assurance_kid, assurance_seal WHERE ... revoked_at IS NULL AND replaced_by_session_id IS NULL` | `TestStepUp_EndToEnd_TenantAndPlatform` (a step-up through a replaced row: 401), `TestStepUp_RacesRevocation_BothOrders`, `TestStepUpStaffSession_DirectRefusals` (tampered seal, absolute expiry at and after the bound, revoked, another staff member's session; row untouched); H01, H02b, H03, P03 (H02a justified, 26.5) |
| 0151 grants only as needed | `GRANT UPDATE (mfa_at, assurance_kid, assurance_seal)`; nothing else; mirrored in `deploy/init-app-role.sql` (granted only once `schema_migrations` holds version 151) | `TestMigration0151_CatalogPins` (no `staff_actor_self_*` policy; the six runtime UPDATE columns), `TestMigration0151_UpDownUp_WholeSchema` (exactly the three column grants added, nothing else), `TestGrants_SessionsAndCutover_RuntimeRole` (changed, 26.4); P03 |
| Section 9.5 re-assertion | `db.ScanActorStaffRows` / `db.CheckActorStaffLive` over the rows the EXISTING A8 `FOR SHARE` statement already returns (K2 `lockStaffAndGrants`, K3/M4, HSEC; the acting approver's id added explicitly to the set) and over withdrawal approve's eligibility read (now `FOR SHARE`, after `lockRequestForUpdate`); status `active` and the token's role, else `db.ErrActorStaffNotLive` -> `401 session_revoked`, nothing written | `TestS4b_K2_ActorChangedBeforeA8_IsRefusedAndNothingWritten`, `TestS4b_K2_ApprovalHoldingA8_SuspensionOfTheActorWaits`, `TestS4b_K3_ActorSuspendedBeforeA8_IsRefusedAndNothingWritten`, `TestS4b_HSEC_PlatformActorReasserted_BothOrders`, `TestStepUp_WithdrawalApprove_EligibilityHoldsTheApproverRowForShare`, `TestStepUp_WithdrawalApprove_ReassertsRoleInTransaction`, `TestCheckActorStaffLive`; B-7 / Y07 / HSEC concurrency unchanged; R01, R03, R04, R06-R08 (R09 justified, 26.5) |

### 26.3 Deviations (for the reviewer; each reversible)

| # | Deviation | Reason |
|---|---|---|
| D1 | The step-up gate runs BEFORE the route's own permission gate (`staffRoute` wraps the route's `inner`, which carries `RequirePermission` / `RequireTenantScope`; S4a D3). A caller without the permission may be asked to step up first, then gets `403 forbidden` | Inserting the gate between a route's own gates needs every one of the 180 registrations rewritten. Nothing leaks: the class of every route is public (golden table, OpenAPI), and a refusal happens before any write either way. The classifier routes return `none` for a caller their handler refuses anyway, so those handlers' audited denials are unchanged |
| D2 | The freshness instant is `min(token mfa_at, sealed row mfa_at)`; "now" is the database clock of the liveness query | S4a-R10; one clock with `mfa_at` (written with `SELECT now()`); the claim is whole seconds, so the effective instant is at most 1 s older than the row's |
| D3 | Seal format v2; a v1 (S3) seal is never accepted: every staff session that exists when S4b deploys must log in again (liveness answers `session_revoked`, refresh 401), without an integrity alert | S3-R10; accepting v1 would keep the unbound hash / expiry for up to 12 h. Operational note for the cutover runbook |
| D4 | "5 step-up failures revoke the current session" (section 6.5) is implemented as: the step-up failure that leaves the factor LOCKED (soft or hard; the counters are the factor's, shared with login) revokes the calling session's whole chain | One budget for every second-factor guess (section 6.5); no per-session counter is needed, and a mix of login and step-up guesses cannot avoid it. A locked factor answers `mfa_locked` before any comparison and revokes nothing |
| D5 | `POST /v1/staff/auth/step-up` is a `staffRoute` (class none) with the `mfa` rate bucket INSIDE the chain | The freeze requires `staffRoute`; an unauthenticated request does not consume the bucket (the factor lockout is the real throttle) |
| D6 | `rgLimitChangeClass`: a removal (`value: null`) or an unparsable body is `critical`; any value is `none`. Whether a value is an increase is decided by the RG domain under its person lock, and the domain still refuses every staff increase (`RG_STEP_UP_REQUIRED`) | ADR 0115: staff increases are NOT enabled by S4b. Enabling them needs ADR 0115's classifier slice to pass the step-up assertion into the domain transaction (the route cannot know the current limit) |
| D7 | `alertResolveClass`: `critical` for every `staff.*` kind and every `p1` alert, and for an alert that does not exist or cannot be read; `none` otherwise and for a caller the handler refuses (no `alert:manage`, or a tenant session); a database fault is 503 | Section 8.2 (f) "at least" list, pinned as a prefix + severity rule |
| D8 | `financialPolicyChangeClass` decides by permission (`financial_policy:author` -> critical); the backstop exempts `financial_policy_change:propose` from a TENANT-scope session only (tighten-only callers; `author` is platform_admin only) | Otherwise the backstop would turn every tighten-only proposal into a 500 |
| D9 | The backstop runs in `Issuer.Attach` (the only production signing path; `Sign` has no context and is test-only, pinned); its max-age is the class's section 7.4 UPPER bound (5 / 10 min) plus 30 s grace, not the configured value | The route gate already applied the configured value; the backstop catches a missing or wrong class |
| D10 | Integration-test support: `actorproof.SetTestStepUpFallback` (build tag `integration`, set by `prooftest`'s `init`) lets a governed domain service called DIRECTLY by an integration test sign without an assertion. It never applies to an HTTP staff request (`auth.Middleware` marks it, `stepup.WithStaffRequest`) nor to a present-but-wrong assertion | Section 9.1 "Test support"; hundreds of domain tests call the services without the HTTP chain. Production binaries cannot contain the setter (pinned) |
| D11 | New leaf package `internal/stepup` (class, assertion, freshness); `internal/auth` re-exports the names (`auth.StepUpClass`, `auth.StepUpAssertion`, ...) | `internal/actorproof` and `internal/withdrawal` importing `internal/auth` creates test import cycles (`db` and `tenant` tests import `prooftest`) |
| D12 | Withdrawal approve (9.5): `approverEligibilityCheckLocked` reads the approver's own row `FOR SHARE` (tenant session: the row is visible) and compares the role; a status other than `active` keeps the pre-existing `ErrApproverInactive` 403 (S4a-R2 pin), a role mismatch is `401 session_revoked`. Reject, submit and resolve keep the unlocked `approverEligibilityCheck` | Section 9.5 table; S4b-R3 for submit / resolve |
| D13 | Section 9.5 implemented for K2, K3/M4, HSEC and withdrawal approve; NOT for withdrawal submit / resolve and K1 approve / revoke | Scope of this slice as dispatched (fold into the existing A8 statements + `approverEligibilityCheck`); the new statements of the other rows need their own lock-order review (S4b-R3) |
| D14 | Section 9.5(b)'s `app.actor_staff_id` GUC and `staff_actor_self_*` policy pair are NOT built (an earlier revision of this branch built them; removed). Every flow re-asserted here already sees the actor's own row: tenant sessions through the dual-scope policy; the acting session (K2 / K3 / HSEC for a platform principal) because `app.tenant_id` is empty there, so the dual-scope policy admits platform rows and the 0112 acting fence admits the acting principal's id. The mutation pass showed the GUC mutants equivalent (R02 / R05 / P02) | Section 9.5(b) is "only if required"; building unused RLS surface widens what `igaming_runtime` SQL can reach (M5). It lands with the first flow whose session hides the actor (K1 platform-scope approvals, lifecycle; S4b-R3). ADR 0082 A9 items 3-5 rewritten accordingly |
| D15 | The `staff.mfa_failure_pattern` count includes `staff.step_up_failed` | The same compromise signal (a correct credential, wrong factor codes) |
| D16 | Recovery-code regeneration takes no body, audits `staff.mfa_recovery_codes_regenerated`, and reuses the S3 enrolment response constructor (the G5 `RevealFor*` allow-list is unchanged) | Section 6.2; gate G5 |
| D17 | A step-up refused because the session is not the chain head still burns the code's TOTP step (the transaction commits) | A code is accepted at most once (17.3) |
| D18 | Withdrawal `require_step_up` is checked on the database clock inside the approval transaction; 2 min is a constant (`stepup.PolicyMaxAge`), not configuration | Section 9.3 [ORCH-DEFAULT] |
| D19 | The liveness refusal for `seal_invalid` / `assurance_incoherent` is logged at Error and counted, not audited or alerted (the refresh path audits and raises the P1 for the same row) | No writes on the read path (S4a D5) |

### 26.4 Changed existing tests and assertions (every one)

| File | Change | Why |
|---|---|---|
| `internal/staffmfa/session_test.go` `TestSealSession_CanonicalFormPinned` | the pinned canonical form is `ss-v2|...|<refresh_token_hash>|<expires_at>` (was `ss-v1|...` without the two fields); `sampleAssurance` gains the two fields | S3-R10 |
| same file, `TestSealSession_EveryFieldIsBound`, `TestSealSession_RefusesMalformedInput` | cases ADDED (the two new fields; empty / upper-case / short / `|` hash, no expiry) | S3-R10 |
| `internal/auth/staff_claims_test.go` `TestIssueStaff_RoundTripsTheAssuranceClaims`, `internal/auth/staff_chain_test.go` `TestMiddleware_AttachesStaffSessionForStaffTokensOnly` | the fixed 2027 `auth_time` / `mfa_at` became `now - 1 h` (+100 s / +60 s); the round-trip assertions compare against those | S3-R13: a future `mfa_at` is refused by `Verify` |
| `internal/httpserver/openapi_staff_auth_contract_test.go` `TestOpenAPI_StaffAuth_RoutesDocumentedBothWays` | 7 -> 9 mounted operations; the two bearer routes are checked for bearerAuth / no-store / 503 (+ the `mfa` bucket and 429 for step-up, the `StepUpRequired` response for regenerate) instead of the unauthenticated rules; the reverse scan covers `/v1/staff/mfa/` too | New routes |
| same file, `TestOpenAPI_StaffAuth_DTOsMatchSchemas` | DTO list +3 (`stepUpRequest`, `stepUpWire`, `recoveryCodesWire`); `x-sensitive` list +2 | New schemas |
| `internal/httpserver/staff_liveness_unit_test.go` `TestStaffLivenessSQL_Shape` | "liveness must not READ the idle `expires_at`" became "reads it only as `expires_at AS sealed_idle_expiry`, never compares it"; the sealed-assurance columns are required | Seal v2 binds `expires_at`; liveness still never refuses on the idle expiry |
| `internal/httpserver/staff_route_static_test.go` `TestStaffRoute_ChainComposition` | the pinned chain string ends `... auth.RequireAAL2(requireStepUp(deps, class)(inner))` | The gate |
| same file, `TestStaffRoutes_EveryMutatingRouteClassified` | 178 / 108 -> 180 / 110 | Two new routes |
| same file, `TestStaffRoutes_ClassifiedOnArrivalAreNotRegistered` | the "no concrete entry: vacuous" failure became a planted-list non-vacuity check | The last two concrete entries (step-up, regenerate) moved to the golden table |
| `internal/httpserver/staff_route_golden_test.go` | +2 golden rows (`POST /v1/staff/auth/step-up` none, `POST /v1/staff/mfa/recovery-codes/regenerate` critical); 2 arrival entries removed | Same |
| `internal/httpserver/staff_mfa_schema_integration_test.go` `TestGrants_SessionsAndCutover_RuntimeRole` | runtime UPDATE columns +`assurance_kid`, `assurance_seal`, `mfa_at`; the `deploy/init-app-role.sql` mirror must also contain the 0151 grant | Migration 0151 (review F9: granted by the slice that writes them) |
| same file, `TestSessions_SF040Matrix` | the runtime `mfa_at` forward UPDATE now succeeds (was 42501); a runtime backwards `mfa_at` is `SF040` (added) | Same |
| `internal/httpserver/staff_mfa_flow_integration_test.go` `TestStaffMFA_SessionSealTamper` | (c) the unsealed forward `mfa_at` edit is made by the RUNTIME role (was the owner) and the session's access token is also refused at liveness (`seal_invalid`, added); (d) `mfa_at = now()` and the seal-to-NULL removed from the runtime 42501 list; the runtime seal-to-NULL is `SF040` (added) | Same; S4a-R10 |
| same file, `TestStaffMFA_Redeem_GateD_TokenRowIsAuthoritative` | the "unknown token" is built so that it always differs from the real one (`"x" + tok[1:]` was the same token whenever the token started with `x`, 1 in 64) | Pre-existing flake, seen in this run |
| `internal/httpserver/staff_liveness_integration_test.go` `TestStaffLiveness_RefusalMatrix` | the absolute-expiry case plants a 1 h session that ended 1 h ago (was a 12 h session that ended 1 h ago, whose 13 h-old `auth_time` `Verify` now refuses); a token with a 13 h-old `auth_time` is asserted `401 unauthorized` (added) | S3-R13 |
| `internal/httpserver/staff_mfa_static_test.go` `TestStatic_StaffAuthAuditMetadataKeys` | the scanned files +2 (`staff_stepup_handlers.go`, `staff_stepup.go`) | New audit call sites |
| `internal/adjustment/static_test.go` (`staffAllowedCols`) | `staff_version` admitted | ADR 0099 A-19 amendment (section 9.5(c)) |
| `internal/withdrawal/*_test.go` (9 files), `internal/httpserver/payment_force_resolution_m2/m4_api_integration_test.go`, `staff_read_routes_integration_test.go`, `internal/payments/{k3_world,payout_dispatch,b13b_destination}_integration_test.go`, `internal/reconciliation/payout_live_integration_test.go` | every `withdrawal.Approve` call passes the zero assertion `auth.StepUpAssertion{}` (an alias of `stepup.Assertion`; setup only; no assertion changed). `TestApprove_RequireStepUpFailsClosedWhenAboveThreshold` keeps its assertions (no assertion: still `ErrStepUpRequired`) | New parameter (section 9.3) |
| `internal/httpserver/staff_mfa_world_integration_test.go` (`sealOK`, `plantSession`, `sessions`) | seal v2 (setup) | S3-R10 |
| `internal/auth/authtest` | `LiveStaffTokenAged` added (setup) | Stale step-up fixtures |
| `internal/actorproof/prooftest` | `AttachForSession` places a synthetic assertion; `WithStepUp`; the `init` enables the integration-only fallback (D10) | Section 9.1 test support |

Note for the reviewer: an earlier revision of THIS branch pinned the `staff_actor_self_*` policy pair in its own new
tests (`internal/db/actor_staff*_test.go`, `staff_stepup_migration_integration_test.go`); after the pair was removed
(D14) they pin its ABSENCE (`TestMigration0151_CatalogPins`, `TestStatic_NoActorStaffIDSetterYet`) and exactly three
column grants. These are branch-new tests, not existing assertions.

New test files (not changes): `internal/stepup/stepup_test.go`, `internal/auth/stepup_test.go`,
`internal/actorproof/stepup_test.go`, `internal/actorproof/testsupport_integration_test.go`,
`internal/db/actor_staff_test.go`, `internal/db/actor_staff_integration_test.go`, `internal/httpserver/staff_stepup_test.go`,
`internal/httpserver/staff_stepup_integration_test.go`, `internal/httpserver/staff_stepup_direct_integration_test.go`,
`internal/httpserver/staff_stepup_withdrawal_integration_test.go`, `internal/httpserver/staff_stepup_retry_integration_test.go`,
`internal/httpserver/staff_stepup_migration_integration_test.go`, `internal/adjustment/s4b_reassertion_integration_test.go`,
`internal/payments/s4b_reassertion_integration_test.go`, `internal/withdrawal/stepup_policy_integration_test.go`.

No other existing assertion changed: the role matrix, the S3 / S4a HTTP, OpenAPI and staff-identity pins, the K2 / K3
/ HSEC / B-7 / Y07 concurrency tests and every withdrawal test pass unchanged.

### 26.5 Evidence (local only; NOT GitHub CI)

Private databases only (`priv_db.sh` / `priv_test.sh`, sanctioned local CI credentials; no role or password change).
Final head of the code: `0c3eb88` plus this documentation commit.

| Command | Result |
|---|---|
| `go build ./...`, `go vet ./...`, `go vet -tags integration ./...`, `gofmt -l` | PASS (clean) |
| `go test ./... -count=1` (unit) | PASS (60 packages ok, 0 FAIL) |
| `golangci-lint --build-tags integration --new-from-rev=3019e51` (2.9.0) | PASS (0 issues) |
| `-tags integration -p 1`: `stepup`, `auth/...`, `staffmfa/...`, `actorproof/...`, `db`, `withdrawal`, `adjustment`, `apierror`, `observability`, `cmd/platform-api` (final head) | PASS |
| `-tags integration -p 1`: `launchgov`, `tenant`, `rg`, `alerting/...`, `reconciliation/...`, `config`, `cmd/migrate`, `cmd/seed-admin`, `deploy/observability` (before the 9.5(b) removal, which does not touch them) | PASS |
| `-tags integration -p 1 ./internal/aml/` | FAIL: `TestMigrations0140to0142_UpDownUp_WholeSchema` only - PRE-EXISTING: fails identically on a `git archive` of the base `3019e51` |
| full `./internal/httpserver/` (`-timeout 60m`, final head, 1008 s) | FAIL only `TestResolutionIsolation_NormalOperation` / `_OneTenantStoreOutage` / `_FinancialDuringOutage` (known flaky latency bound, ISO-LATENCY-1; the deposit-callback path is not touched by S4b). Re-run alone: the latter two PASS; `_NormalOperation` 2 of 3 PASS (worst 478 / 494 ms, one run 531 ms vs the 500 ms bound) |
| `TestStepUp_*`, `TestStepUpStaffSession_DirectRefusals`, `TestMigration0151_*`, `TestGrants_*`, `TestSessions_SF040Matrix` (final head) | PASS |
| `./internal/payments/` per-letter shards A-Z | PASS (26 shards, 0 FAIL). A first parallel run failed shards B-E en masse and F-Z at package level while other heavy runs shared the machine; every shard was re-run on the final code and passes (B-E, G-Z, A, F) |
| K2 / K3 / HSEC / B-7 / Y07 concurrency + `TestS4b_*` (adjustment, payments; final head) | PASS |
| Mutation pass (`docs/plans/prh2-hardening-round/adr0119-s4b-mutation-kill.txt`) | 59 mutants: 57 KILLED, 2 justified (H02a, R09) |
| `deploy/init-app-role.sql` 0151 block | executed in a rolled-back transaction against the private database: PASS |

### 26.6 Residuals (S4b)

| # | Residual | Disposition |
|---|---|---|
| S4b-R1 | Under M5 (arbitrary SQL as the runtime role) the 0151 grant lets SQL move a staff session's `mfa_at` forward (SF040 still refuses backwards / NULL). The edit cannot forge a step-up: the seal no longer verifies, liveness refuses the session (`seal_invalid`) and its refresh is refused, audited and alerted (P1). It is a denial of service against that session only | Accepted; pinned by `TestStaffMFA_SessionSealTamper` (c) |
| S4b-R2 | The step-up prompt precedes the route's permission check (D1) | Accepted (no write, public classes); revisit if the back office finds the prompt confusing |
| S4b-R3 | Section 9.5 re-assertion NOT built for withdrawal `submit` / `resolve` and K1 grant `approve` / `revoke`: they rely on the request-level liveness check and the existing DB status guards (R13 window: a handler that started before a suspension committed). The 9.5(b) GUC + `staff_actor_self_*` policy pair are not built either (D14) | Follow-up with `ledger-finance` (new statements at the A8 position after the flow's last L1 row) and `architect` (the policy pair, with the first flow that needs it) |
| S4b-R4 | ADR 0115 staff RG increases stay refused (`RG_STEP_UP_REQUIRED`); the route classifier marks only removals critical | ADR 0115's classifier slice (pass the assertion into the RG domain transaction) |
| S4b-R5 | Step-up is per SESSION: after a step-up, every access token minted for that session chain (the step-up response, and any later refresh, which copies `mfa_at`) is fresh; a thief who holds the refresh token benefits within the window | Section 3 M2 residual (bounded by the 5 / 10 min window, the 30 min idle and refresh-token reuse detection) |
| S4b-R6 | The signer backstop uses the class's UPPER bound (5 / 10 min) + 30 s, not the configured (possibly tighter) max-age | Accepted (D9): the route gate applies the configured value |
| S4b-R7 | Domain-level integration tests sign through the integration-only fallback (D10); only HTTP tests exercise the real backstop | Accepted; `TestStepUp_SignerBackstopOverHTTP`, `TestTestStepUpFallback_NeverAppliesToAStaffRequest` |
| S4b-R8 | Every staff session issued before S4b (seal v1) ends at deploy: staff log in again (no alert) | Add to the S9 runbook / deploy note |
| S4b-R9 | `sv` is still 0 (`staff_version` arrives with 0139): the 9.5 re-assertion compares `status` and `role` only | 0139 (S5 + S6) |
| S4b-R10 | A `seal_invalid` refusal at liveness is not itself audited or alerted (D19); the P1 arises on the session's next refresh | Accepted; metric + Error log line (`StaffLivenessRefusals` rule) |
| S4b-R11 | `staff_lifecycle:request|approve` are not in the backstop registry yet | S5 + S6 (with the operations) |

### 26.7 What `security`, `ledger-finance`, `code-reviewer` and `qa` should probe

`security` (live):
1. Forge every claim shape against a stale row (fresh `mfa_at` claim, future claims, `amr` without `otp`, an
   `auth_time` older than 12 h, a token for another `sid`) and try to obtain `200` on a critical route; try a token
   minted before a step-up after the step-up.
2. SQL as `igaming_runtime`: move `mfa_at` forward on your own session, rewrite the seal from another session, swap
   `refresh_token_hash` / `expires_at` (needs the trigger disabled: owner); confirm that 0151 added no policy and no
   column beyond the three, and that nothing sets `app.actor_staff_id`.
3. Step-up endpoint: replay, the shared lockout (login + step-up guesses), a recovery code, a revoked / replaced /
   expired session, step-up racing logout / refresh, the `mfa` bucket, response caching (`no-store`).
4. Classifier evasion: duplicate / case-folded JSON keys, an oversized body, a body that the classifier and the
   handler could parse differently, an alert whose kind changes between the classifier and the handler (the
   classifier reads the row once; the handler locks it later - the window is a resolve of a just-raised alert).
5. The backstop: a governed write reached through a route classified `none`; the integration-only fallback is not in
   the production binary (`go build` without the tag; the static pin).
6. The 403 `step_up_required` body / header for every class, and that no handler ran (no audit, no state change).

`ledger-finance` (mandatory): ADR 0082 Amendment A9 (placement table, position, the actor in the ascending-id set,
item 3's visibility argument per flow, item 5: no lock added or moved, recount unchanged), K2 / K3 / HSEC final approvals under a
concurrent suspension in both orders, the withdrawal approve `FOR SHARE` (`lockRequestForUpdate` first), and the
S4b-R3 decision (submit / resolve / K1).

`architect`: the decision not to build the 9.5(b) policy pair in S4b (D14, S4b-R3), the ADR 0099 A-19 amendment, the
`internal/stepup` package split (D11), migration number 0151 (grants only).

`code-reviewer` / `qa`: the golden-wide test's coverage pins (47 critical, 22 financial, 4 classifier, 37 none
mutating, 70 reads), the changed assertions of 26.4, the surviving mutants of 26.5, and the static pins (NO setter
of `app.actor_staff_id` yet, single production caller of `auth.ContextWithStepUpAssertion`, production signs only
through `Attach`, the integration-only fallback setter).

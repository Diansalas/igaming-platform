# ADR 0017 — Staff MFA and Step-Up Authentication (Architecture Foundation)

Status: Architecture foundation only - `NOT IMPLEMENTED`. No code, schema,
or endpoint in this pass; this documents the target design so Stage 3+
(or a dedicated future stage) builds against a session/claims shape that
already accommodates it, rather than retrofitting one later. Per the
security-hardening-pass instructions: business policy specifics (which
operations require step-up, exact max-age thresholds, rollout timeline)
are explicitly NOT decided here - those are human/compliance decisions
(see "Open decisions" below).

## Context

Stage 2's staff authentication is password + Postgres-backed lockout,
with no second factor. The Stage 2 security review flagged the absence
of MFA for `platform_admin`/`tenant_admin` as a pre-launch blocker for a
production iGaming back office - a single stolen or guessed password
today grants full staff-level access, with no additional barrier. Stage
3+ will also introduce genuinely high-stakes actions (withdrawal
approval, manual ledger adjustment) where even a legitimate, currently-
valid session shouldn't be sufficient authorization on its own - a
session opened hours ago for routine work is not the same assurance as
"this specific person, right now, approving this specific $50,000
withdrawal."

## Decision — two distinct, related concepts

1. **MFA (Multi-Factor Authentication)** - a persistent property of a
   staff account: has it enrolled a second factor, and does every login
   require presenting it.
2. **Step-up authentication** - a per-operation, session-scoped
   elevation: a specific high-risk request requires a *fresh* proof of a
   factor (re-entered TOTP code, or similar), independent of how long ago
   the session's original login happened.

Modeled separately because they answer different questions. An account
can have MFA enrolled generally, yet a specific action might still demand
it be *freshly* re-verified (a 6-hour-old session shouldn't silently
authorize a large manual adjustment). Conversely, during a phased
rollout some accounts might not have MFA enrolled yet while low-risk
actions still proceed normally - the two concepts must not be collapsed
into one boolean.

## Data model (future - not created by this pass)

- `staff_mfa_factors`: `id`, `staff_user_id`, `factor_type` (`'totp'`
  initially - the column is a string, not a hardcoded enum tied to one
  vendor, so a future factor type doesn't require a schema rewrite),
  `secret_encrypted` (never stored in the clear - envelope-encrypted with
  a key held outside application config, the same custody principle as
  the signing-key architecture in ADR 0018), `enrolled_at`,
  `last_used_at`, `status` (`'active'`/`'disabled'`).
- `staff_mfa_recovery_codes`: one-time-use codes, hashed at rest
  (Argon2id, same as passwords - never plaintext), consumed on use,
  remaining count exposed to the user so they know when to regenerate.
- A tenant/platform compliance-policy source of truth for
  `mfa_required` per staff account or role - deliberately not modeled as
  a self-service toggle a `tenant_admin` controls on their own account,
  since that would let the exact role MFA is meant to protect disable its
  own protection. Ownership of that policy surface is future
  `identity-compliance` work, not decided here.

## Session assurance level

JWT claims gain two new fields, following the standard OIDC `amr`
(Authentication Methods Reference) convention rather than inventing a
platform-specific shape:

- `amr`: array of methods used for this session, e.g. `["pwd"]` or
  `["pwd","totp"]`.
- `mfa_at`: nullable Unix timestamp of the most recent second-factor
  verification for this session (distinct from the session's original
  `iat`/login time - a step-up re-verification updates this without
  requiring a full new login).

A new `RequireStepUp(maxAge time.Duration)` middleware - mirroring
`RequireTenantScope`'s existing per-route (not blanket) application
pattern - checks that `mfa_at` is present and within `maxAge` of the
current request. Failing that check returns a distinct error code
(`step_up_required`, not the ordinary `unauthorized`) so a client can
prompt for re-verification in place, rather than forcing a full
re-login.

## Enrollment / verification / recovery flow (future endpoints)

- `POST /v1/staff/mfa/enroll` (requires an existing authenticated
  session) - generates a TOTP secret and a QR-encodable URI; returns
  recovery codes exactly once.
- `POST /v1/staff/mfa/verify` - confirms enrollment with a valid code,
  flips the factor to `active`, writes `staff.mfa_enrolled` to the audit
  log.
- `POST /v1/staff/auth/login` extended: after password verification, if
  the account requires MFA, the response is a short-lived, single-
  purpose "MFA challenge" token - explicitly NOT a real session/refresh
  pair - rather than `TokenPair`. A second call,
  `POST /v1/staff/auth/mfa-verify`, presents the challenge token plus a
  TOTP code; only on success is a real session issued, with `amr`
  reflecting both factors and `mfa_at` set.
- `POST /v1/staff/mfa/recover` consumes one recovery code (subject to
  the same Postgres-backed lockout pattern as password login) and starts
  a fresh enrollment flow; every use is audited
  (`staff.mfa_recovery_used`), since a recovery code is itself a
  security-sensitive bypass of the normal factor and warrants the same
  scrutiny as a password reset.
- An administrative MFA reset (a platform_admin unlocking a locked-out
  tenant_admin) is itself a step-up-gated, audited action - never a bare
  "disable MFA" flag flip performed casually.

## Step-up-gated operations (forward list, not final business policy)

Anticipated candidates, per the hardening-pass instructions: withdrawal
approval, manual financial/ledger adjustment, financial configuration
changes (e.g. enabling a payment method or currency), security
configuration changes (triggering key rotation, granting a permission),
and other privileged administrative operations as they're built. Each is
wired to `RequireStepUp` at its own specific route when it's built - not
a single session-wide policy - so the requirement is legible per-endpoint
rather than an invisible global rule. The exact list, and the `maxAge`
threshold per operation, are compliance/business decisions for a human to
set (see "Open decisions"), not invented here.

## Audit events

`staff.mfa_enrolled`, `staff.mfa_verification_failed`,
`staff.mfa_recovery_used`, `staff.step_up_verified`,
`staff.step_up_denied` - all written via the existing
`internal/audit.Record` pattern, atomically with the action they record,
exactly like every Stage 2 audit event.

## What this pass does NOT do

No migration, no new table, no new endpoint, no new middleware is created
in this hardening pass. This ADR exists so that whichever future stage
implements MFA does so against session-claims and audit conventions that
already fit the rest of this codebase, rather than inventing an
incompatible shape under time pressure later.

## Open decisions (human/compliance, not engineering)

1. Which specific operations require step-up at launch, and their
   `maxAge` thresholds.
2. Whether MFA is mandatory for all staff roles at launch or phased in
   (e.g. `platform_admin`/`tenant_admin` first).
3. TOTP specifically, vs. also supporting WebAuthn/hardware keys, for the
   initial factor type.
4. Who administers MFA policy (compliance function, platform ops) and
   the recovery/reset approval process for a locked-out account.

## Owner

`security` (mechanism design), `identity-compliance` (enrollment/recovery
policy), `architect` (schema and session-claims integration).

# ADR 0030 — Email Verification and Password Reset

Status: Accepted. Part of Stage 4F. Distinct from KYC/identity
verification (ADR 0028) - this ADR covers only the two credential-recovery
flows directive §12/§13 require: confirming a player controls their own
email address, and letting them reset a forgotten password.

## Context

Both flows share the exact same shape: issue a random, short-lived,
one-time-use token; deliver it out-of-band (email); accept it back once;
apply a side effect; never allow reuse or enumeration. Stage 2 already
solved this shape once for refresh tokens (`sessions.refresh_token_hash`,
migration 0012/0018, ADR 0016's "hash lookup with no tenant scope, then
resolve scope" pattern) - the right move is reusing that pattern, not
inventing a new one.

## Decisions

### 1. One shared table, purpose-discriminated

`player_credential_tokens` (migration 0040) holds both
`email_verification` and `password_reset` tokens, discriminated by a
`purpose` column, rather than two near-identical tables. Both are "a
random, hashed, short-lived, one-time-use credential proof tied to a
player_account" - the only difference is what they authorize, which
`ValidateAndConsumeCredentialToken`'s `wantPurpose` parameter enforces (a
password-reset token presented to the email-confirm endpoint is rejected
with the identical generic error as "not found").

### 2. Token lifecycle

- **Generate**: 32 random bytes (`crypto/rand`), base64url-encoded - never
  a short numeric "code" (directive's own wording allows either; a
  high-entropy token removes any brute-force concern span, unlike a
  6-digit code, which would additionally need per-account attempt
  throttling this design doesn't need).
- **Store**: only the SHA-256 hash (`token_hash`) - identical
  never-store-the-secret principle as `sessions.refresh_token_hash`. The
  raw token exists only in the outbound email and the confirm request
  body, never persisted, never logged (`internal/email`'s
  `MockProvider.Sent()` is an in-memory test inspection API, not a log).
- **TTL**: 24h for email verification, 1h for password reset - reset
  is deliberately shorter since it grants account takeover if
  intercepted, verification only unlocks account activation.
- **One live token per purpose**: `IssueCredentialToken` supersedes
  (marks consumed, without ever having been "used") every prior
  outstanding token of the same purpose for that account before issuing a
  new one - an old email link becomes inert the instant a new one is
  requested, giving "resend" a well-defined, safe meaning instead of
  silently accumulating live tokens.
- **Consume**: an atomic conditional `UPDATE ... WHERE consumed_at IS
  NULL AND expires_at > now()`, checked via `RowsAffected()` -
  closes the exact TOCTOU class ADR 0027 §6 already documented for Stage
  4E's identity-review-clear handler. Verified under real concurrency:
  `TestPasswordReset_ConcurrentDoubleConfirmAppliesExactlyOnce` fires 8
  simultaneous confirms against the same token and asserts exactly one
  succeeds and exactly one row is ever marked consumed.

### 3. Rate limiting and anti-enumeration

`CountRecentCredentialTokens` counts every issuance (superseded,
consumed, or still-live) in the last hour, capped at 3
(`credentialTokenRateMaxCount`) - a caller cannot bypass the limit by
exhausting tokens faster. Critically, a rate-limited request returns the
exact same `204 No Content` as a successful one
(`TestEmailVerification_ResendIsRateLimited`), and a password-reset
request against an email that doesn't exist returns the exact same `204`
as one that does (`TestPasswordReset_UnknownEmailGetsIdenticalResponse`)
- directive §12/§13's explicit "do not expose whether a specific email
belongs to an account... via enumeration." A brand slug, by contrast, is
already a public identifier (identical precedent to register/login), so
an unknown brand slug legitimately returns 404.

### 4. Session invalidation on password reset

A completed password reset calls `RevokeAllSessionsForPrincipal`,
revoking every session for that player account - the same protective
response the codebase already gives detected refresh-token reuse. This
required a real bug fix: `RevokeAllSessionsForPrincipal`'s UPDATE was
silently affecting zero rows because Postgres requires a row to be
SELECT-visible under some RLS policy before permitting any UPDATE against
it, and `sessions`' own `session_select_own_principal` policy
(migration 0018) requires the `app.principal_id` GUC, which only
`WithPrincipalScope` set - a plain `WithTenant` transaction never set it.
Fixed by adding `db.SetPrincipalIDForCurrentTx`, which sets that GUC on
the ALREADY-OPEN transaction (rather than reaching for
`WithPrincipalScope`, which would have required a second, separate
transaction and broken the atomicity requirement that the password
hash update, session revocation, and audit record must commit together
or not at all). Confirmed by
`TestPasswordReset_RequestConfirmChangesPasswordAndRevokesSessions`, which
asserts the OLD access token's underlying session is unusable
immediately after reset, and by independent security specialist review.

### 5. Password hashing happens only after the token is validated

A P1 finding from security specialist review: an earlier version of this
handler computed the new password's Argon2id hash BEFORE validating the
token, with an inverted rationale in its own comment. Since this route is
unauthenticated and carries no per-request rate limit of its own, that
made Argon2id's deliberately expensive hashing (CLAUDE.md's own
uncompromising stance on password hashing) trivially triggerable by
anyone posting garbage tokens - an uncosted DoS surface. Fixed: hashing
now happens INSIDE `ValidateAndConsumeCredentialToken`'s callback, which
only runs once a request has already proven it holds a genuinely valid,
unconsumed token.

### 6. Email delivery abstraction

`internal/email.Provider` (`Send(ctx, Message) error`) is the only
production dependency this stage introduces for these flows -
`MockProvider` (in-memory, `Sent()` inspection API) is the only
implementation shipped, matching directive §14's explicit "do not require
production SMTP/API credentials." A real provider (SES, SendGrid, ...)
slots in behind this same interface later; nothing above it changes.

### 7. Relationship to `player_accounts.verified_at`

`verified_at` (Stage 2's own reserved column) is repurposed by this
stage for EMAIL verification specifically - stamped by
`identity.SetPlayerAccountEmailVerified` when a verification token is
confirmed. This closes a previously-dormant Stage 2 gap as a side effect:
`PlayerStatusPendingVerification` existed since Stage 2 but nothing ever
transitioned it onward; email-verification-confirm now does so via the
already-existing `identity.SetPlayerAccountStatusIfCurrent
(pending_verification -> active)`, applied only if the account is STILL
in that exact state (never overriding a later state like `suspended` or
`identity_review_required`). This is an account-status fact, entirely
separate from KYC/identity verification (`kyc_verifications` - ADR 0028),
which never touches `player_accounts` at all.

## Specialist review findings and fixes

See `docs/progress.md`'s Stage 4F entry for the full itemized review pass
and every P0/P1 finding raised and fixed before this stage was considered
complete - including the concurrent-double-confirm P0 (§2 above) and the
password-hashing-order P1 (§5 above).

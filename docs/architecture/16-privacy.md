# 16 — Privacy Architecture

Status: Stage 2. Referenced from `internal/identity/player_account.go`.
Ownership: `identity-compliance` owns the data-minimization judgment calls
below; `security` reviews access controls; `architect` signs off on the
schema/retention design pattern. Neither this document nor any code in
Stage 2 invents a specific legal retention period — see "Retention" below.

## Scope

This document inventories what personal data Stage 2's identity/tenancy/
security foundation actually collects, why, who/what can read it, and
what is deliberately deferred. It is not a legal privacy policy and makes
no claim about GDPR/CCPA/LGPD or any specific jurisdiction's compliance
status — that is regulatory/legal work, out of scope for an engineering
document (see CLAUDE.md's Compliance section: "software capability and
legal/regulatory/licensing approval are different things").

## Data minimization: what Stage 2 deliberately does not collect

Per the Stage 2 instructions ("privacy-conscious modeling, no unnecessary
PII") and CLAUDE.md's Compliance section, `player_accounts` (Stage 2)
holds only what registration and authentication require: email and a
password hash. No name, date of birth, address, phone number, or
government ID is collected — that belongs to the Stage 4 KYC/AML
subsystem, behind its own vendor-agnostic interface (see
`11-kyc-aml-rg-architecture.md`), not this table. `persons` similarly
holds no PII directly — only a partial-unique `person_key_hash` column
that Stage 2 defines as a hook but does not populate (there is no KYC
document data yet to hash into it).

## Sensitive fields inventory

| Table | Field(s) | Sensitivity | Notes |
|---|---|---|---|
| `player_accounts` | `email` | PII (identifying) | Unique per `(brand_id, email)`. RLS-protected (tenant-scoped, no public read — see migration 0010). |
| `player_accounts` | `password_hash` | Credential material | Argon2id, never the raw password. Never returned by any API response (see `PlayerProfile`/`PlayerAccount` schemas in `docs/api/openapi/platform-api.yaml` — neither includes it). |
| `staff_users` | `email`, `password_hash` | PII / credential material | Same protections as above; dual-scope RLS (migration 0011). |
| `sessions` | `user_agent`, `ip_address` | PII (device/network identifier) | Collected for security purposes (session review, anomaly detection), not analytics. Public-read RLS policy on this table (`docs/decisions/0013`) is justified by *token possession* being the real access control, not row visibility — but this means `ip_address`/`user_agent` are technically world-readable by anyone who already holds a valid refresh token for that specific row (i.e., not a new exposure beyond what token possession already grants). |
| `login_attempts` | `identifier` (may embed email), `ip_address` | PII | Necessary for lockout enforcement, which is itself a player-protective security control. |
| `audit_log` | `ip_address`, `user_agent`, `metadata` (embeds the lockout identifier, which contains an email, on `*.login_blocked_lockout` events) | PII, by design | Required by CLAUDE.md's audit rule ("actor, tenant, entity, before/after state, IP, reason code") — the audit trail's entire purpose is to reconstruct who did what from where. Access is tenant-scoped RLS + `audit:read` permission (compliance/tenant_admin/platform_admin only — never `support`). Narrowed in Stage 2 security review: the `unknown_email` login-failure branch (an attempted email with no matching account) no longer embeds the raw email in `metadata` — logging a non-user's email address was a stricter privacy concern than logging one that's actively being rate-limited, since the latter is operationally necessary to identify which identifier a lockout applies to. |
| `audit_log` | secrets (passwords, tokens) | **Never stored** | No handler in `internal/httpserver` passes a raw password, password hash, access token, or refresh token into an `audit.Entry`'s `Metadata`. This is a standing invariant, not enforced by a type system — a future code-reviewer/security check should keep verifying it (see CLAUDE.md: "never store secrets in audit logs"). |

No table stores a raw PAN, card number, or private cryptographic key —
those never enter Stage 2's scope at all (payments/custody are Stage 3+,
and even then via hosted fields/custodian delegation, per CLAUDE.md's
Security section).

## Access controls

- **Row-level security** is the primary technical control: `player_accounts`
  and `staff_users` are tenant-scoped with `FORCE ROW LEVEL SECURITY`
  (never bypassable by the application's own database role — see
  `internal/db`'s `verifyNotPrivileged` check). A player's PII is
  invisible to every other tenant's connection, structurally, not by
  query discipline.
- **Permission-based authorization** gates which authenticated principal
  can read what: `player:read` for admin player listing/lookup,
  `audit:read` for the audit trail, neither granted to unauthenticated
  or under-scoped tokens (see `internal/auth/permission.go`'s
  `rolePermissions` table).
- **Self-service is self-scoped only**: `GET /v1/me`, `GET /v1/me/sessions`,
  and `DELETE /v1/me/sessions/{id}` are hard-scoped to the calling
  principal's own `principal_id` — never parameterized by another
  principal's id, and session revocation additionally checks ownership
  server-side (`internal/auth/session.go`'s `RevokeSession`) rather than
  relying on RLS tenant-scoping alone (two principals can share a
  tenant).
- **No API response leaks another principal's credential material**:
  password hashes are never serialized into any JSON response type in
  `internal/httpserver`.

## Audit requirements

Every mutating identity/security action — registration, login (success
and every failure branch), logout, refresh, refresh-reuse detection,
session revocation, tenant/brand/staff creation, player suspension —
writes an `audit.Entry` in the *same database transaction* as the action
it records (see `internal/audit/audit.go`'s `Record`, which takes the
caller's `pgx.Tx`). This is deliberate: audit logging must not depend on
a separate, mutable application log (CLAUDE.md's explicit requirement) or
on a best-effort side-channel write that could silently fail while the
underlying action succeeds. The `audit_log` table itself is
append-only — a `BEFORE UPDATE OR DELETE` trigger
(`audit_log_deny_mutation`) rejects any mutation attempt unconditionally,
even for the application's own database role (see
`docs/decisions/0013-audit-log-immutability-and-dual-scope-rls.md` and
`TestAuditLog_ImmutableEvenForOwningRole`).

## Retention

Stage 2 does not implement any automated deletion, expiry, or archival
job for `player_accounts`, `staff_users`, `sessions`, `login_attempts`,
or `audit_log`. This is deliberate, not an oversight: retention periods
for player PII, KYC records, and financial/audit trails are jurisdiction-
and regulator-dependent (Europe vs. LATAM markets have different legal
minimums), and CLAUDE.md explicitly forbids inventing a legal retention
period. Retention is modeled as **future configuration/business policy**,
not a hardcoded value:

- `docs/decisions/0006-hybrid-licensing-and-jurisdiction-model.md`
  already establishes that jurisdiction is a first-class, pluggable
  concept (`TenantJurisdictionConfig`); retention rules are a natural
  extension of that same per-jurisdiction configuration surface once a
  human/legal decision fixes the actual periods per market.
- Two operational exceptions already exist independent of any future
  retention policy, because they are security mechanisms, not PII
  archives: `sessions` rows past `expires_at`/`revoked_at` and
  `login_attempts` rows past the lockout window
  (`internal/identity/login_attempt.go`'s `lockoutWindow`) are
  functionally dead the moment they age out, even though nothing
  currently purges them from storage.
- `PROVIDER DEPENDENT`/deferred: no deletion or anonymization job exists
  yet for any table in this document. Building one is explicitly out of
  scope until retention periods are set by a human decision (see "When to
  stop and ask" in CLAUDE.md: legal interpretation is a stop-and-ask
  item, not an engineering judgment call).

## Future deletion/anonymization needs

When retention periods are set, the following will need engineering work
not yet built:

1. A player-initiated or regulator-mandated account deletion/
   anonymization flow for `player_accounts` — likely anonymization
   (replace `email` with a tombstone value, keep the row for ledger/audit
   referential integrity) rather than a hard delete, since Stage 3+
   financial records will foreign-key to `player_accounts.id` and
   CLAUDE.md forbids editing/deleting historical ledger entries.
2. A scheduled purge job for expired `sessions` and aged-out
   `login_attempts` rows (operationally safe to hard-delete; they carry
   no long-term regulatory retention requirement, only tempered by
   whatever short-term security-forensics value recent rows have).
3. `audit_log` retention is the most constrained case: audit trails
   typically have long, regulator-set minimums specifically *because*
   they are audit trails, and the append-only trigger means even a
   retention purge cannot be a bare `UPDATE`/`DELETE` — it would need to
   be a deliberate, reviewed, explicitly-audited exception path (a
   partition-drop by age or a `security`+`architect`-approved
   trigger-disable window), not something written casually alongside
   other work.

None of this is scheduled for a specific stage yet; it belongs wherever
the human resolves the underlying retention-period decision.

## Stage mapping

Stage 2 establishes the sensitive-field inventory, access-control
mechanisms (RLS + permissions), and audit-trail foundation above.
Retention/deletion/anonymization tooling (previous section) is deferred
pending a human/legal decision on actual periods. KYC document handling
and its privacy implications are Stage 4 (`11-kyc-aml-rg-architecture.md`)
and will need this document revisited when that data starts flowing
through vendor-agnostic KYC interfaces.

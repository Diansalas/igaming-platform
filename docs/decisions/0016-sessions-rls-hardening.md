# ADR 0016 — Sessions RLS Hardening: Closing the Public-Read Exposure

Status: Accepted (pre-Stage-3 security hardening gate, superseding the
"sessions is a deliberate exception" section added to ADR 0013 during
Stage 2 review)

## Context

Migration 0012 gave `sessions` a `FOR SELECT USING (true)` policy,
justified because `RotateSession`'s and `POST /v1/auth/logout`'s initial
lookup (`internal/auth.lookupSessionByToken`) presents only a raw refresh
token with no tenant hint - there is no `app.tenant_id` to scope a query
by before reading the row that would reveal the tenant. That justification
is real, but the policy as written applied to *every* read of the table,
not just that one lookup path. The consequence, flagged in the Stage 2
completion report as the most significant remaining security debt: any
tenant-scoped connection could run a bare `SELECT * FROM sessions` and see
every tenant's and every principal's session metadata (`principal_id`,
`tenant_id`, `ip_address`, `user_agent`) - isolation depended entirely on
`internal/auth.ListActiveSessions`/`RevokeSession` filtering by
`principal_id` in application code, not on the database, which is exactly
the failure mode `CLAUDE.md`'s multi-tenancy rule exists to prevent
("enforced by PostgreSQL row-level security... not by discipline in
application code").

## Approaches considered

1. **A `BYPASSRLS` role for a `SECURITY DEFINER` lookup function.** Viable
   in principle - a `NOLOGIN` role that nothing can ever authenticate as
   directly, owning a narrow function that takes a token hash and returns
   only the row matching it. Rejected for this pass because it requires
   `CREATE ROLE` privilege our migration-running application role does not
   have (Stage 1 deliberately avoided a superuser/elevated-privilege
   bootstrap path for the app's own connection), pushing role creation
   into a separate, currently-nonexistent DBA bootstrap step
   (`deploy/init-app-role.sql` equivalent) - real infrastructure work
   disproportionate to what the actual access pattern needs.
2. **Drop `FORCE ROW LEVEL SECURITY` and rely on table ownership to
   bypass RLS for the lookup.** Rejected outright: our application's
   runtime connection role IS the table owner (the same single-role model
   Stage 1 established), so removing `FORCE` would let the *ordinary*
   application connection bypass RLS entirely - reintroducing the exact
   Stage 1 superuser/`BYPASSRLS` vulnerability this platform's whole RLS
   design exists to prevent, just via a different mechanism.
3. **Narrow the existing policy using session-scoped Postgres GUCs set
   only by trusted server code, mirroring the `app.tenant_id` idiom
   already used everywhere in this schema.** Chosen. No new database
   role, no privilege escalation, no bootstrap dependency - implemented
   entirely with the same `set_config(..., true)` mechanism
   `internal/db.Pool.WithTenant` already uses, scoped per-transaction so
   nothing can leak across pooled connections.

## Decision

Migration `0018_harden_sessions_select_rls` replaces the single
`session_read_by_token` policy with two narrower ones (Postgres OR's
multiple permissive policies for the same command together):

```sql
-- Phase 1: exact-token-hash lookup only.
CREATE POLICY session_select_by_token_hash ON sessions
    FOR SELECT
    USING (refresh_token_hash = NULLIF(current_setting('app.session_lookup_hash', true), ''));

-- Phase 2: an authenticated principal's own sessions, in their own
-- tenant/platform scope.
CREATE POLICY session_select_own_principal ON sessions
    FOR SELECT
    USING (
        (
            (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
        )
        AND principal_id = NULLIF(current_setting('app.principal_id', true), '')::uuid
    );
```

Two new `internal/db.Pool` methods set the corresponding GUCs, exactly
mirroring `WithTenant`'s existing pattern:

- **`WithSessionLookup(ctx, hash, fn)`** — sets `app.session_lookup_hash`
  to the SHA-256 hex digest of the actual refresh token being looked up
  (never anything else). Used only by
  `internal/auth.lookupSessionByToken`. Visibility under this scope is at
  most the one row whose `refresh_token_hash` equals that exact value -
  possession of the unguessable token remains the real credential, but
  now the database itself enforces "at most one row," not "every row."
- **`WithPrincipalScope(ctx, tenantID, principalID, fn)`** — sets both
  `app.tenant_id` (if non-nil) and `app.principal_id`. Used by
  `GET /v1/me/sessions` and `DELETE /v1/me/sessions/{id}` (both wired to
  the caller's own verified `principalID` from their JWT, never a
  path/body parameter). Now the database, not just
  `ListActiveSessions`'s `WHERE principal_id = $2` clause, refuses to
  return another principal's row.

**INSERT/UPDATE policies (`session_scoped_insert`, `session_scoped_update`)
are unchanged** - tenant-scoped only, as before. They were never the
exposure this ADR addresses (a session's `principal_id` is set by trusted
server code at issuance, not attacker-controlled input), and adding a
principal-match requirement to them would break `RotateSession`'s and
`revokeChainFrom`'s legitimate system-level mutations, which act on a
session's own already-verified principal rather than an externally
asserted one.

One internal query changed shape without changing behavior:
`RotateSession`'s concurrency re-check (added during Stage 2 review to
close a rotation race) was `SELECT ... FOR UPDATE`, which is now denied
by the narrower SELECT policy in the exact context it runs in (a
tenant-only-scoped, not principal-scoped, transaction, since at that
point the code is acting as the *system* on the row's own already-known
principal, not on a caller-asserted one). Rewritten as
`UPDATE sessions SET last_used_at = now() ... RETURNING replaced_by_session_id, revoked_at`,
which acquires the identical row lock through the UPDATE policy (already
correctly tenant-scoped, unaffected by this change) instead of the
SELECT policy - same concurrency guarantee, no RLS policy conflict.

## What this does NOT yet do

No current endpoint lets a platform_admin browse another principal's
sessions for support/security investigation - none existed before this
change either. If that becomes a real requirement, it must be built as
its own explicitly-scoped, permission-gated, audited mechanism (e.g. a
`session:admin_read` permission paired with a narrow, logged accessor) -
never by loosening `session_select_own_principal`'s principal match back
toward a blanket "platform scope sees everything," which would
reintroduce this exact class of exposure at the platform-admin level
instead of the tenant level.

## Consequences

- A tenant-scoped connection can no longer read another tenant's, or
  another principal's, session metadata under any code path in this
  codebase - closing both the cross-tenant AND cross-principal exposure,
  enforced at the database layer per `CLAUDE.md`'s multi-tenancy rule,
  not merely by `ListActiveSessions`/`RevokeSession`'s existing
  application-level filtering (which remains, as defense in depth, not
  as the sole guarantee).
- This ADR supersedes the "`sessions` is a deliberate exception... the
  cost... is accepted rather than fixed for Stage 2" language added to
  `docs/decisions/0013-audit-log-immutability-and-dual-scope-rls.md`
  during Stage 2 review - that debt is now closed. ADR 0013 is annotated
  to point here rather than restating the fix.
- No new database role, no elevated privilege, no bootstrap dependency
  was introduced - the fix uses only mechanisms this schema already
  relies on everywhere else.

## Owner

`security` (design, review), `architect` (schema/RLS pattern
consistency).

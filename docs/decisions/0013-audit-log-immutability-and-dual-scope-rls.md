# ADR 0013 — Audit Log Immutability and the Dual-Scope RLS Pattern

Status: Accepted (Stage 2)

## Context

`CLAUDE.md` requires audit records to be append-only and immutable, and
Stage 2's instructions add: "do not make audit logging dependent on
mutable application logs" and "design the audit foundation so later
financial operations can integrate with it." Some audit-worthy events
(platform-admin actions, service-identity actions) have no tenant at all;
most do. The same "some rows are tenant-scoped, some are platform-wide"
shape also applies to `staff_users`, `sessions`, and `login_attempts`
introduced this stage.

## Decision

### Immutability

`audit_log` is protected by a `BEFORE UPDATE OR DELETE` trigger that
unconditionally raises an exception. This is enforced regardless of which
role runs the query - including the application's own runtime role, which
owns the table (ordinary `REVOKE`-based protection does not bind a table
owner; a trigger does). No code path in the platform ever issues an
`UPDATE` or `DELETE` against `audit_log`; the trigger exists so that
remains true even under a future bug or a compromised credential with
this role's access, not only by convention.

**Correction (Stage 2 security review):** the original `BEFORE UPDATE OR
DELETE FOR EACH ROW` trigger did not cover `TRUNCATE` - Postgres row-level
triggers never fire on it, and a `TRUNCATE` from the same non-superuser
application role that owns the table would have erased the entire trail
with no error, silently defeating the guarantee this section claims.
Migration `0016_audit_log_deny_truncate` adds a second, statement-level
`BEFORE TRUNCATE` trigger reusing the same deny function. "Enforced
regardless of which role runs the query" is accurate only as of that
migration.

### Dual-scope RLS pattern

For any table that can legitimately hold both tenant-scoped and
platform-wide rows (`audit_log`, `staff_users`, `sessions`,
`login_attempts`), the RLS policy is:

```sql
USING (
  (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
  OR
  (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
)
```

(with the same expression in `WITH CHECK` for mutable tables). This is
deliberately different from the single-expression pattern used for
purely tenant-owned tables (`tenant_config` in Stage 1, `player_accounts`
in Stage 2): a plain `tenant_id = NULLIF(...)::uuid` comparison is NULL
(denied) whenever `tenant_id` is NULL, in both the row and the session
setting - so it cannot express "this platform-wide row is visible to a
platform-scoped (no tenant context) connection" at all. The dual-scope
form handles both cases explicitly: a tenant-scoped connection sees only
its own tenant's rows; a connection with no tenant context (`WithoutTenant`)
sees only platform-wide (`tenant_id IS NULL`) rows. Neither can see the
other's rows.

**`sessions` is a deliberate exception, not an instance of this pattern -
correction (Stage 2 architect/security review):** migration `0012` gives
`sessions` a THIRD, distinct shape: `FOR SELECT USING (true)` (public
read) plus tenant-scoped `INSERT`/`UPDATE` policies, not the dual-scope
`FOR ALL` expression above. This was necessary, not accidental: a refresh
token carries no tenant hint, so `RotateSession`'s first phase
(`lookupSessionByToken`) has no `app.tenant_id` to scope a `SELECT` by -
finding the row *is* how the tenant is learned, and only then does a
second, properly-scoped transaction perform any mutation (see
`internal/auth/session.go`). The real access control on read is
possession of the exact, cryptographically-random, unguessable token, not
row visibility. The cost, flagged in review and accepted rather than
fixed for Stage 2, is that this also makes `sessions.ip_address` and
`user_agent` readable by any tenant-scoped connection for any row, not
just the caller's own - `internal/auth.ListActiveSessions` and the
ownership check in `RevokeSession` are therefore safe only because they
filter by `principal_id` in application code, which is exactly the kind
of isolation `CLAUDE.md` otherwise asks RLS, not discipline, to provide.
A future tightening (e.g. a `SECURITY DEFINER` lookup function, or
scoping `SELECT` to a session-token GUC set only by
`lookupSessionByToken`) is deferred, not forgotten - see the Stage 2
completion report's technical debt section.

## Consequences

- Every future table that mixes tenant-scoped and platform-wide rows uses
  this exact pattern rather than inventing a variant - `internal/db`'s
  test suite includes a reusable RLS-proof test shape for both the
  single-scope and dual-scope patterns.
- Audit-record writes always happen inside the same transaction as the
  action they record (`internal/audit.Record(ctx, tx, entry)` takes the
  caller's `pgx.Tx`), so an audit record can never be silently lost if the
  action's own transaction fails, and the action can never "succeed"
  without its audit record - they commit or roll back together. This is
  the hook `CLAUDE.md` and Stage 2's instructions ask for so Stage 3's
  financial operations can integrate the same way.
  **Correction (Stage 2 architect/backend review):** this claim was false
  for one call site - `newCreateTenantHandler` originally called
  `identity.CreateTenant` (which committed its own transaction) and then
  wrote the audit record in a *separate* `WithoutTenant` transaction,
  logging (not failing) on error. Fixed by giving `CreateTenant` the same
  `pgx.Tx`-based signature `CreateBrand`/`CreateStaffUser` already used,
  so tenant creation now follows this pattern exactly like every other
  admin handler - no exceptions remain as of Stage 2 completion.

## Owner

`security` (RLS pattern, trigger), `architect` (schema shape).

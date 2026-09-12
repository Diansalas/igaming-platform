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
correction (Stage 2 architect/security review), closed (pre-Stage-3
hardening pass):** migration `0012` originally gave `sessions` a THIRD,
distinct shape - `FOR SELECT USING (true)` (public read) plus
tenant-scoped `INSERT`/`UPDATE` policies, not the dual-scope `FOR ALL`
expression above - necessary because a refresh token carries no tenant
hint, so `RotateSession`'s first phase has no `app.tenant_id` to scope a
`SELECT` by. That blanket `SELECT` policy made `sessions.ip_address`,
`user_agent`, `principal_id`, and `tenant_id` readable by any
tenant-scoped connection for ANY row, not just the caller's own -
isolation depended entirely on `internal/auth.ListActiveSessions`/
`RevokeSession` filtering in application code, exactly the kind of
isolation `CLAUDE.md` asks RLS, not discipline, to provide. This was
flagged as the most significant remaining Stage 2 debt and is now closed:
see `docs/decisions/0016-sessions-rls-hardening.md` for the narrower
per-token-hash and per-principal `SELECT` policies (migration `0018`)
that replaced the blanket one, and why a new database role/`BYPASSRLS`
approach was considered and rejected in favor of the same
`set_config`-based GUC pattern this schema already uses everywhere.

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

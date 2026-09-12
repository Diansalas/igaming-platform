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
`session_read_by_token` policy with **three** narrower ones (Postgres
OR's multiple permissive policies for the same command together):

```sql
-- Phase 1: exact-token-hash lookup only (no tenant/principal context
-- exists yet - see Context above).
CREATE POLICY session_select_by_token_hash ON sessions
    FOR SELECT
    USING (refresh_token_hash = NULLIF(current_setting('app.session_lookup_hash', true), ''));

-- Phase 2: an authenticated principal's own sessions, in their own
-- tenant/platform scope (self-service listing/revocation).
CREATE POLICY session_select_own_principal ON sessions
    FOR SELECT
    USING (
        (
            (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
        )
        AND principal_id = NULLIF(current_setting('app.principal_id', true), '')::uuid
    );

-- Phase 3: internal system operations (refresh rotation, reuse-chain
-- revocation) that already resolved a specific row via phase 1 and need
-- to touch it in a tenant-only-scoped transaction, with no
-- caller-asserted principal_id available.
CREATE POLICY session_select_internal_op ON sessions
    FOR SELECT
    USING (
        id = NULLIF(current_setting('app.session_internal_op_id', true), '')::uuid
        AND (
            (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
        )
    );
```

Three new `internal/db` functions set the corresponding GUCs, mirroring
`WithTenant`'s existing pattern:

- **`Pool.WithSessionLookup(ctx, hash, fn)`** — sets `app.session_lookup_hash`
  to the SHA-256 hex digest of the actual refresh token being looked up
  (never anything else). Used only by
  `internal/auth.lookupSessionByToken`. Visibility under this scope is at
  most the one row whose `refresh_token_hash` equals that exact value -
  possession of the unguessable token remains the real credential, but
  now the database itself enforces "at most one row," not "every row."
- **`Pool.WithPrincipalScope(ctx, tenantID, principalID, fn)`** — sets both
  `app.tenant_id` (if non-nil) and `app.principal_id`. Used by
  `GET /v1/me/sessions` and `DELETE /v1/me/sessions/{id}` (both wired to
  the caller's own verified `principalID` from their JWT, never a
  path/body parameter). Now the database, not just
  `ListActiveSessions`'s `WHERE principal_id = $2` clause, refuses to
  return another principal's row.
- **`SetSessionInternalOpID(ctx, tx, sessionID)`** — sets
  `app.session_internal_op_id` within an ALREADY-OPEN transaction (it
  does not begin or commit one, unlike the two above). Called by
  `internal/auth.withResolvedScope` for every rotation/logout mutation
  (keyed to the row phase 1 already resolved), and re-called by
  `revokeChainFrom` on each hop of the reuse-detection chain walk, since
  that function touches a different row id every iteration and a single
  fixed value wouldn't cover every hop.

**INSERT/UPDATE policies (`session_scoped_insert`, `session_scoped_update`)
are unchanged** - tenant-scoped only, as before. They were never the
exposure this ADR addresses (a session's `principal_id` is set by trusted
server code at issuance, not attacker-controlled input), and adding a
principal-match requirement to them would break `RotateSession`'s and
`revokeChainFrom`'s legitimate system-level mutations, which act on a
session's own already-verified principal rather than an externally
asserted one.

**A Postgres behavior discovered while building this, load-bearing for
the whole design**: an `UPDATE`/`DELETE` against a row requires that row
to be visible under SOME `SELECT` policy, not merely to satisfy the
`UPDATE`/`DELETE` policy's own `USING` clause - and this applies whether
or not the statement uses `RETURNING`. Confirmed empirically (see
`internal/auth/session_rls_integration_test.go` and the commit history of
this fix). This is *why* phase 3 (`session_select_internal_op`) exists at
all: without it, `RotateSession`'s concurrency re-check and
`revokeChainFrom`'s chain walk - both plain `UPDATE`s against a
tenant-only-scoped transaction with no token hash and no caller-asserted
`principal_id` in scope - would silently affect zero rows regardless of
the row's actual eligibility, indistinguishable from "lost a race" or
"already revoked." `RotateSession`'s concurrency re-check itself remains
a plain conditional `UPDATE ... WHERE id = $1 AND replaced_by_session_id
IS NULL AND revoked_at IS NULL`, checked via `RowsAffected()` (no
`RETURNING`) - unchanged in shape from before this ADR, but it only works
because phase 3 now grants it the SELECT visibility this behavior
requires.

## Corrections made during this hardening pass's own review

An independent `security` and `code-reviewer` pass on this exact change
(before it was considered final) found and this fixed:

- **Blocking**: `revokeChainFrom`'s walk used `WHERE revoked_at IS NULL`
  to decide whether to continue, so a chain with an already-revoked
  MID-chain node (e.g. the session's own owner logged out of that one
  device before a reuse of an earlier link was ever detected) stopped
  the walk there, leaving every session further down the chain live
  despite a confirmed theft signal. Fixed: the `UPDATE` now uses
  `revoked_at = COALESCE(revoked_at, now())` with no `WHERE` filter on
  `revoked_at`, so an already-revoked hop is still touched (a no-op on
  that column) and still `RETURNING`s where the chain continues. A
  defensive `maxChainHops` cap was added against an unexpected cycle.
  Regression test:
  `TestRevokeChainFrom_ContinuesPastAlreadyRevokedMidChainNode`.
- **Should-fix**: the `UPDATE sessions SET replaced_by_session_id = $1
  ... WHERE id = $2` that links a rotated-out session to its successor
  discarded its result, so a 0-row outcome (which should be unreachable,
  but previously would have been silently possible had phase 3's SELECT
  visibility been missing or wrong) would mint a new session without
  ever marking its predecessor replaced - breaking reuse detection for
  that exact token with no error raised anywhere. Fixed: the write's
  `RowsAffected()` is now checked and a loud error returned if it's zero.
- **Should-fix**: the concurrency re-check's race-loser branch
  (`RowsAffected() == 0`, meaning a concurrent request won the race)
  returned `ErrSessionReused` with no audit record and no chain
  revocation - a real gap, since this branch is reachable by a genuine
  concurrent replay attempt, not only a benign client-side double-submit.
  Fixed by writing an `auth.refresh_rotation_race_lost` audit entry on
  this path. Deliberately NOT also revoking the chain here: unlike a
  confirmed already-rotated-token reuse (handled separately, chain
  revoked), this branch cannot distinguish a malicious replay from an
  ordinary retry, and the transaction's winner is - by construction - a
  legitimate rotation; revoking it too would be a needless, uninvestigated
  session termination for what is very often just a network retry. This
  is a considered trade-off, not an oversight left unaddressed - see
  "Remaining security debt" in the Security Hardening Completion Report.
- **Should-fix**: `session_select_internal_op`'s `USING` clause matched
  only `id = app.session_internal_op_id`, with no tenant conjunct -
  unexploitable today (every setter already resolved the row's own
  tenant via `withResolvedScope`/`revokeChainFrom`'s own transaction
  scope), but a future caller setting this GUC from any less-trusted
  input would get a single-row cross-tenant read with no isolation
  backstop. Fixed by ANDing in the same tenant/platform-scope check every
  other policy here uses, at zero cost to any current caller.
- **Should-fix**: the three GUC-setting call sites duplicated raw
  `set_config` SQL directly in `internal/auth/session.go` instead of
  going through `internal/db`, the package this schema's convention says
  owns these session-variable names. Fixed by adding
  `db.SetSessionInternalOpID`, used by both call sites.
- **Test gap, closed**: the original test suite proved cross-principal
  and cross-tenant *reads* were denied but had no test proving a
  cross-principal *write* (`UPDATE`) was denied at the database level -
  the one HTTP-level IDOR test alone would pass identically whether RLS
  or `RevokeSession`'s own `AND principal_id = $2` clause was doing the
  work. Closed with
  `TestSessionRLS_PrincipalCannotRevokeAnotherPrincipalsSessionDirectly`,
  plus `TestSessionRLS_InternalOpScopeSeesOnlyThatOneRow` and
  `TestSessionRLS_ScopeGUCsDoNotLeakAcrossTransactions` proving the two
  properties above by direct, adversarial SQL rather than by inference.

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

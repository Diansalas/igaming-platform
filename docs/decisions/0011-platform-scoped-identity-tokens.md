# ADR 0011 — Platform-Scoped Identities May Hold a Nil-Tenant Token

Status: Accepted (Stage 2)

## Context

Stage 1's JWT design required every token to carry a non-nil `tenant_id`,
because every principal was assumed to be a brand-scoped player. Stage 2
introduces staff users, including platform administrators who are not
scoped to any single tenant (they operate the platform itself, across
tenants). Forcing a platform administrator's token to carry a specific
tenant_id would either be meaningless (which tenant?) or would require
minting a fresh token per tenant they touch, which doesn't match how a
platform operator actually works (one session, many tenants over time,
each individual action still tenant-scoped).

## Decision

`tenant.Context.TenantID` may legitimately be `uuid.Nil`, representing a
platform-scoped principal (currently: `staff_users` rows with
`tenant_id IS NULL`, i.e. the `platform_admin` role). This is not a
weakening of the tenant-isolation rule — it moves *where* the rule is
enforced:

- **Token verification** (`internal/auth`) no longer rejects a nil
  tenant_id outright. It still requires every other claim (subject,
  expiry, audience, issuer, signature) to be valid.
- **Every tenant-scoped operation** (anything that calls
  `db.Pool.WithTenant`) requires a non-nil tenant_id at the point of use.
  A new middleware, `auth.RequireTenantScope`, enforces this for any route
  that operates on a specific tenant's data — it 403s a nil-tenant token
  before the handler ever runs.
- A nil-tenant token can only do two things: platform-wide operations
  gated by `platform_admin`-only permissions (e.g. provisioning a new
  tenant), or nothing at all on tenant-owned data. It is never treated as
  "matches every tenant" or given a bypass through RLS - `db.WithTenant`
  still requires an actual, specific tenant id argument; there is no code
  path that lets a nil-tenant caller pass `uuid.Nil` into `WithTenant` and
  have it mean "all tenants."

## Consequences

- A platform admin who needs to act on a specific tenant's data must go
  through an endpoint that resolves and validates that tenant id (e.g. a
  path parameter checked against the `tenants` table), and that handler
  applies `RequireTenantScope`-equivalent logic explicitly since the
  caller's own token doesn't carry it. This is more verbose per-endpoint
  than a single blanket rule, but it means every tenant-touching code path
  states its tenant explicitly and auditably rather than inheriting an
  ambient "admin bypass."
- A full "assume tenant context" flow (platform admin mints a
  tenant-scoped token for support purposes) is deferred - noted as
  deferred functionality in the Stage 2 completion report, not silently
  dropped. Until it exists, platform-admin actions on a specific tenant
  are per-endpoint and explicit, not session-wide impersonation.
- `docs/security/security-architecture.md` is updated to state this
  explicitly as a standing invariant: nil-tenant is a valid *identity*
  scope, never an authorization bypass.

## Owner

`security` (invariant enforcement), `architect` (schema/claims shape).

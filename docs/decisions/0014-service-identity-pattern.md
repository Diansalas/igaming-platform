# ADR 0014 — Service Identity: Direct Database Access for Platform Jobs, No Service Mesh

Status: Accepted (Stage 2)

## Context

Stage 2's instructions ask for a service-identity model distinct from
human users, with least privilege, but explicitly warn against building a
complex service-mesh identity system unless actually justified. The
platform's only real "service" so far is `cmd/migrate` (and the new
`cmd/seed-admin`), both of which already connect directly to PostgreSQL
with the same non-privileged application role used at runtime, outside
the HTTP/JWT layer entirely.

## Decision

Two distinct service-identity shapes, chosen per need rather than one
general mechanism:

1. **Platform jobs with no tenant scope** (migrations, admin bootstrap,
   and - in later stages - reporting CDC, reconciliation jobs): connect
   directly to PostgreSQL via `internal/db`, using the same
   non-superuser, non-`BYPASSRLS` role as the API service, with
   `db.WithoutTenant` for platform-level tables. No HTTP layer, no JWT, no
   new credential type. This is what already exists and needs no new
   code - it is documented here so it's recognized as the service-identity
   pattern rather than an ad hoc exception.
2. **A future tenant-scoped service caller** (e.g. a webhook receiver
   acting on behalf of a specific tenant, needed starting around Stage 3's
   provider integrations): would authenticate the same way a human does -
   a token scoped to exactly one tenant, minted for that service, checked
   against the same `RequirePermission`/`RequireTenantScope` middleware
   humans go through. `auth.RolePlatformAdmin` and friends already model
   "a role with a defined permission set"; a `service` principal type
   reuses that shape rather than inventing a parallel one. **Not
   implemented in Stage 2** - there is no consumer yet, and building the
   credential-issuance flow now (API keys, service secrets, rotation)
   with nothing to call it would be exactly the premature abstraction
   Stage 1's review flagged in `internal/eventbus`/`internal/validation`.

## Consequences

- No new service-credential type, API-key table, or service-auth
  middleware is added in Stage 2.
- When a real tenant-scoped service caller is needed, it is added as an
  extension of the existing JWT/permission model (option 2), not a
  separate system.
- Least privilege for platform jobs is already satisfied structurally:
  they use the same restricted role as everything else, never a superuser
  connection (`internal/db/db.go`'s `verifyNotPrivileged` applies to every
  caller equally, jobs included).

## Owner

`architect`, `security`.

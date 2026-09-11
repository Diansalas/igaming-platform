# ADR 0012 — Brand Is a Distinct Entity From Tenant

Status: Accepted (Stage 2), supersedes Stage 1's implicit 1:1 tenant↔brand
assumption (`tenant_config` keyed 1:1 by `tenant_id`).

## Context

Stage 1 modeled per-brand configuration (`tenant_config`) keyed directly
by `tenant_id`, implicitly assuming one tenant runs exactly one
consumer-facing brand. Real iGaming operators commonly run multiple
consumer brands ("skins") under one commercial/legal/licensing
relationship - one back-office team, one licence, several differently
themed, differently domained player-facing products. The human's Stage 2
instructions explicitly require Tenant and Brand not be collapsed.

## Decision

- **Tenant** is the commercial/legal/licensing relationship: licensing
  model, licence, jurisdiction permissions at the tenant level, partner
  console access. Unchanged in shape from Stage 1, plus a new `slug` for
  staff-login tenant resolution (see `internal/identity`).
- **Brand** (new, migration `0008_create_brands...`) is the
  consumer-facing product: name, public slug/domain, theme, default
  locale. A tenant has one or more brands. `tenant_config`'s columns moved
  onto `brands`; `tenant_config` itself is dropped.
- **Player accounts** belong to a `(tenant_id, brand_id)` pair, enforced
  by a composite foreign key `(brand_id, tenant_id) REFERENCES
  brands(id, tenant_id)` so a player account can never reference a brand
  belonging to a different tenant than its own `tenant_id` column claims -
  a database-level guarantee, not an application-trusted invariant.
- **Brand identity/theme is treated as public data** (`brands` gets an
  RLS `SELECT` policy of `USING (true)`), because it is exactly what an
  unauthenticated visitor's browser needs to render the site - the same
  class of data as a public website's HTML/theme. **Mutations remain
  tenant-scoped** (separate `INSERT`/`UPDATE`/`DELETE` policies requiring
  the connection's tenant context to match). This is a deliberate,
  documented deviation from the single-policy pattern used elsewhere
  (`tenant_config` in Stage 1, `player_accounts` in Stage 2) - those hold
  genuinely private data with no legitimate public-read case.

## Consequences

- `docs/architecture/15-jurisdiction-and-licensing-model.md`'s
  `TenantJurisdictionConfig` intentionally stays tenant-scoped, not
  brand-scoped, for now - a tenant's jurisdiction permissions are assumed
  to apply uniformly across its brands until a real commercial case
  requires per-brand jurisdiction rules. Recorded here as a deliberate
  scope decision, not an oversight, per `CLAUDE.md`'s scope-expansion
  test: moving it to brand-level has no current consumer and would be
  speculative.
- Registration and login now require a `brand_slug`, not a bare
  `tenant_id` - resolving "which brand is this request about" from a
  public identifier, before any authentication exists, is the correct
  place to determine tenant context for a pre-auth request (see
  `docs/security/security-architecture.md`'s pre-authentication tenant
  resolution section for why this does not conflict with "never trust a
  client-supplied tenant id").

## Owner

`architect`.

# 04 — API Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §3, §7; API-first principle
from the master project rules.

## Principle

The platform is API-first. Brand frontend, back office, partner console,
and external B2B integrations all consume the same class of platform API
— no surface gets private, undocumented business logic. Core business
rules live in backend/domain services, never exclusively in frontend code.

## Shape

- REST + JWT, per the Blueprint's system map (§3), as the Stage-1 baseline.
- OpenAPI specs maintained per service under `docs/api/`, generated from
  or validated against the actual service code (not hand-written and
  left to drift).
- `RECOMMENDATION`: version every public API from the first commit (e.g.
  `/v1/...` or a version header) — B2B partners integrate against these
  contracts and cannot be broken silently once a partner is live.

## Authentication/authorization propagation

- Brand frontend holds a short-lived JWT identifying the player and tenant.
- Game/sportsbook launch mints a separate, single-use, opaque token scoped
  to `(player, provider, game/product, currency, mode)` with a short TTL —
  never the player's session JWT (Blueprint §4.1). Owned by `security` +
  the relevant domain specialist (`casino`/`sportsbook`).
- Back-office/partner-console APIs authenticate staff separately from
  players, with RBAC scoped by tenant and enforced server-side on every
  request — never inferred from which UI element is visible.
- Every internal service-to-service call carries the tenant context
  explicitly; nothing downstream re-derives or trusts a tenant id passed
  as a plain, unauthenticated parameter.

## Inbound provider callbacks

The highest-traffic, most correctness-critical API surface is the inbound
wallet callback path (game/sportsbook provider → platform). It is treated
as a first-class API with its own contract per provider-adapter, gated by
the idempotency constraint in the ledger (see `06-wallet-ledger-
architecture.md`), not a special case bolted onto the general API layer.

## Conventions (RECOMMENDATION, confirm at Stage 1 gate)

- Consistent error shape across all services (machine-readable error code
  + human message), so `frontend`/`backoffice` can build generic error
  handling once.
- Idempotency-key support on every mutating endpoint that can plausibly be
  retried by a client or a provider (not just the ledger's own internal
  mechanism).
- Pagination and filtering conventions fixed once and reused — back office
  screens depend on server-side filtering/pagination for large tables
  (Blueprint §7).

## Ownership

`architect` owns these conventions; each domain specialist implements
against them and raises a proposal to `architect` (not a unilateral
change) if a convention doesn't fit their domain.

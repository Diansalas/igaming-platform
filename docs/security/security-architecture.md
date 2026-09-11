# Security Architecture Proposal

Status: Stage 0 draft. Owned by the `security` specialist going forward.
Source: Blueprint §4.1, §4.6, §4.7, §4.8, §7, plus `CLAUDE.md`.

## Authentication and session model

- Brand-frontend players: short-lived JWT per session, tenant-scoped.
- Game/product launch: separate single-use opaque token bound to
  `(player, provider, product, currency, mode)`, short TTL — never the
  session JWT. A leak in one of many provider integrations must not become
  account takeover platform-wide.
- Back office / partner console staff: separate authentication path from
  players, with RBAC scoped by tenant.

## Authorization

Three-tier RBAC (platform admin / partner admin / brand operator).
Enforced server-side on every request; never inferred from UI state;
`tenant_id` always derived from authenticated context, never accepted from
the client.

## Tenant isolation

PostgreSQL row-level security bound to connection-level tenant context is
the enforcement mechanism (see `docs/architecture/03-database-
architecture.md`). Every new endpoint requires a test proving a valid
token for tenant A cannot read or write tenant B's data.

## Secrets

Vault or a cloud KMS. Provider HMAC keys rotate; PSP credentials are
per-tenant. No secret ever lives in a config file committed to git, an
environment variable dumped to logs, or an API response.

## PCI scope

Hosted fields/redirect only for card data. No PAN ever reaches platform
infrastructure.

## Audit

Every mutating action (financial, administrative, compliance-relevant)
writes an immutable audit record: actor, tenant, entity, before/after
state, IP, reason code. 5–7 year retention, exportable. Manual balance
adjustments require reason code + four-eyes approval above a configurable
threshold.

## Threat model priorities for Stage 0/1

1. Cross-tenant data access (highest architectural risk given the
   multi-tenant model).
2. Session/token leakage into a third-party provider integration.
3. Idempotency-key forgery or replay on financial endpoints (owned jointly
   with `ledger-finance`).
4. Privilege escalation in back-office/partner-console RBAC.
5. Secrets exposure via logs, error messages, or client-visible responses.

## What this document does not cover

Formal penetration testing, GLI-19/ISO 27001 certification audits, and
legal/regulatory security requirements specific to a jurisdiction — those
require external, human-run engagements and are tracked as dependencies in
`docs/architecture/13-dependency-map-and-risk-register.md`, not
substituted for by this document.

## Review cadence

`security` reviews every change touching auth, sessions, tokens, RBAC,
secrets, or PII handling before it is marked `IMPLEMENTED`, per
`.claude/agents/security.md`.

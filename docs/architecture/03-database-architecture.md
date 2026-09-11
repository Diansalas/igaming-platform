# 03 — Database Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.2, §5, §7.

## System of record

PostgreSQL 16+, single authoritative store for money and identity.
Serializable isolation where correctness demands it (ledger writes,
balance recomputation). No eventual-consistency model for a balance —
ever.

## Multi-tenant isolation

Start: shared cluster, `tenant_id` on every tenant-owned table, enforced by
row-level security bound to a connection-level `SET LOCAL app.tenant_id`
(or equivalent) — never enforced only by `WHERE tenant_id = ?` discipline
in application code (Blueprint §5: "that discipline fails exactly once").

Escalation path (deployment decision, not a schema rewrite): shared
cluster + RLS → schema-per-tenant → database-per-tenant → cluster-per-
tenant. The data-access layer (an internal repository/query-builder layer)
must be written so which tier is active is a connection-routing concern,
not something every query call site needs to know about.

## Ledger schema (see `06-wallet-ledger-architecture.md` for full detail)

- Append-only `ledger_entries` table: never updated, never deleted.
- Account types: `player_cash`, `player_bonus`, `player_locked`,
  `house_gaming`, `provider_payable`, `psp_clearing`, `psp_reserve`,
  `jackpot_contribution`, `promo_liability`, `manual_adjustment`.
- Every entry: `tenant_id`, `account_type`, `currency`, `amount` (signed,
  `NUMERIC(38,0)` + exponent), `transaction_group_id`, `provider_id`,
  `provider_tx_id`, `created_at`.
- Unique constraint on `(provider_id, provider_tx_id)` is the idempotency
  mechanism — enforced at the database, not the application.
- Balances are a materialized/derived projection (a view, materialized
  view, or maintained summary table), recomputed and diffed hourly against
  raw entries; never the authoritative read.

## Partitioning

`RECOMMENDATION`: partition high-volume tables (`ledger_entries`,
`bonus_progress`, `audit_log`) by time (monthly/quarterly) and consider a
secondary partition or index strategy by `tenant_id` once tenant volume
justifies it. Decide at Stage 3 based on projected transaction volume, not
speculatively now.

## Audit store

Append-only, `tenant_id`-scoped, 5–7 year retention, exportable. Written by
a single shared library (see `02-domain-and-service-boundaries.md`) so
every service gets the same guarantees rather than reimplementing them.

## Migrations

`RECOMMENDATION`: one migration tool used platform-wide (not per service
ad hoc), forward-only migrations in version control, no manual schema
changes against any shared environment. Certification readiness (Blueprint
§8) depends on this discipline existing before it's needed.

## Analytics separation

Operational PostgreSQL is never queried directly for reports. Change data
capture (Debezium) feeds the event bus, which feeds ClickHouse (Blueprint
§4.9, §7) — see `12-audit-reporting-architecture.md`.

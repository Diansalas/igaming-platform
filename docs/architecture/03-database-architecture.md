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

**Superseded in detail by Stage 3A.** The sketch below is the Stage 0
proposal and is retained for history; the authoritative column/constraint
shape is now `docs/architecture/ledger-accounting-model.md` (with
`docs/decisions/0019` and `0021`). Three specific differences matter,
because the Stage 0 text below states them the other way round:
entries carry a `direction` ('debit'/'credit') plus a strictly positive
`amount`, **not** a signed amount; entries are grouped by
`ledger_transaction_id` (a first-class `LedgerTransaction` row), **not** a
bare `transaction_group_id`; and the idempotency constraints are
tenant-scoped, `(tenant_id, provider_id, provider_tx_id)` and
`(tenant_id, idempotency_key)`. The Stage 3A account-type list also adds
`player_withdrawal_hold`.

- Append-only `ledger_entries` table: never updated, never deleted.
- Account types: `player_cash`, `player_bonus`, `player_locked_cash`,
  `player_locked_bonus`, `house_gaming`, `provider_payable`,
  `psp_clearing`, `psp_reserve`, `jackpot_contribution`,
  `promo_liability`, `manual_adjustment` (plus `player_withdrawal_hold`,
  above). The Blueprint's single `player_locked` is carried by **two**
  types split by the origin of the locked value — migration `0048`;
  bare `player_locked` is not an admitted value. See
  `ledger-accounting-model.md` §2 and invariant L1 (§6.1/§6.5.4).
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

`IMPLEMENTED` (Stage 1): one small, dependency-light migration tool used
platform-wide (`internal/db/migrate.go` + `cmd/migrate`), not per-service
ad hoc, with no manual schema changes against any shared environment.
Every migration is written as a reversible `up`/`down` SQL pair — this
corrects an earlier draft of this document, which called for forward-only
migrations; reversibility was chosen instead so `down` can be exercised in
CI and local development as a genuine correctness check (a migration that
can't be described in reverse usually reveals a hidden assumption).
**Applied direction remains forward-only in staging/production**: `down`
is a development/CI verification tool, never run against an environment
holding real data without an explicit, recorded decision — a correction
there is a new forward migration (a compensating change), not a rollback,
for the same reason the ledger itself never edits history. Concurrent
migration runs are serialized with a Postgres advisory lock so two
processes can't apply the same migration set at once. Certification
readiness (Blueprint §8) depends on this discipline existing before it's
needed.

## Analytics separation

Operational PostgreSQL is never queried directly for reports. Change data
capture (Debezium) feeds the event bus, which feeds ClickHouse (Blueprint
§4.9, §7) — see `12-audit-reporting-architecture.md`.

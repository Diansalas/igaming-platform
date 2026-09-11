# ADR 0003 — Technology Stack Baseline

Status: **Approved as Stage 1 baseline by the human**, with the explicit
instruction to validate each element during Stage 1 rather than adopt the
full list mechanically — see "Stage 1 validation" below. Hosting row is
**superseded** by `docs/decisions/0009-hosting-hyperscale-cloud.md`.

## Context

The Blueprint recommends a specific stack (§7) based on tail-latency
requirements (wallet callback p99 < 150ms), operational simplicity
(limiting language count), and SEO needs (casino affiliate traffic).
Nothing has been implemented yet, so this stack is not yet load-bearing —
it can still be changed cheaply.

## Decision (proposed)

| Layer | Choice |
|---|---|
| System of record | PostgreSQL 16+ |
| Wallet service | Go |
| Other backend services | Go or Kotlin |
| Event bus | Kafka (NATS JetStream if ops capacity is the binding constraint) |
| Analytics | ClickHouse, fed by CDC (Debezium) |
| Cache/sessions | Redis (never balances) |
| Brand frontend | Next.js |
| Back office / partner console | React + a virtualized, server-filtered data grid |
| Hosting | ~~Hetzner/OVH/Leaseweb-class~~ — **superseded**: major hyperscale cloud, see ADR 0009 |
| Secrets | Vault or a cloud KMS |
| Observability | OpenTelemetry |

## Consequences

- Committing to Go for the wallet service means the team needs Go
  expertise from Stage 3 onward.
- Choosing Kafka over NATS JetStream trades lower ops simplicity for
  per-player event ordering guarantees the bonus engine depends on;
  revisit if ops capacity proves to be the actual binding constraint.
  **Stage 1 does not deploy either** — it defines the event-bus interface
  only (see `docs/architecture/02-domain-and-service-boundaries.md`),
  deferring the concrete broker choice until there's a concrete ordering/
  throughput need to validate against, per the human's explicit "do not
  create distributed systems complexity without a concrete reason"
  instruction.
- Hosting choice updated per ADR 0009; written AUP confirmation for the
  specific hyperscale provider is still outstanding and still blocks
  production deployment only, not Stage 1 development work.

## Stage 1 validation

Per the human's explicit instruction, this list is a baseline to validate,
not a mandate to build out fully. Stage 1 determines, per element: what's
required immediately (Postgres, Go, structured logging/OTel, basic CI),
what should be introduced later (Kafka/NATS, ClickHouse — no concrete need
yet), what can remain modular (event bus behind an interface, custody/PSP
behind interfaces), and where a simpler architecture is preferable (one
foundation service, not a microservice per domain, until real scaling/
ownership needs appear). See the Stage 1 completion report for what was
actually introduced vs. deferred.

## Owner

`architect`, with `devops` implementing.

# ADR 0003 — Technology Stack Baseline

Status: Proposed (RECOMMENDATION — confirm explicitly at the Stage 1 gate)

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
| Hosting | Hetzner/OVH/Leaseweb-class, contingent on written gambling-AUP confirmation |
| Secrets | Vault or a cloud KMS |
| Observability | OpenTelemetry |

## Consequences

- Committing to Go for the wallet service means the team needs Go
  expertise from Stage 3 onward.
- Choosing Kafka over NATS JetStream trades lower ops simplicity for
  per-player event ordering guarantees the bonus engine depends on;
  revisit if ops capacity proves to be the actual binding constraint.
- The hosting choice is gated on Blueprint §10 Q6 (written AUP
  confirmation) — see `0005-open-business-decisions.md`. No infrastructure
  is provisioned against a host that hasn't confirmed in writing.

## Status of confirmation

Not yet confirmed by the human. Treated as the working default for Stage 1
scaffolding; flag any deviation as a new ADR rather than silently drifting
from this baseline.

## Owner

`architect`, with `devops` implementing.

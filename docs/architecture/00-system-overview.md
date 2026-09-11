# 00 — System Overview

Status: Stage 0 draft. Source: `iGaming-Platform-Blueprint.pdf` §2, §3, §6, §7.

## The asset

The platform is the system of record for player identity and player money,
plus the commercial/operational surface around it (back office, partner
console, tenant config). Games, odds/trading, card acquiring, and RNG
certification are licensed, not built.

## Three product surfaces (Blueprint §2)

| Surface | Users | Responsibility |
|---|---|---|
| Brand frontend | Players | Lobby, game launch, sportsbook, cashier, account, RG controls. Rendered from tenant config — never forked per brand. |
| Operator back office | Partner staff | Player management, bonus campaigns, payment approval, risk queues, CMS, reporting. Must be usable by a non-technical retention manager. |
| Partner console | Us + licensees | Brand provisioning, provider/PSP credential management, revenue-share statements, cost pass-through, invoicing, platform-wide compliance view. |

**Rule of thumb:** if launching a new brand requires an engineer, this is a
consulting business, not a platform. It must require a partner-console form
and a DNS record.

## System map

```
BRAND SURFACES              PLATFORM CORE                    EXTERNAL PROVIDERS
(rendered from config)      (the owned asset)                (contracted, never built)

Brand website (Next.js) ─┐  ┌─ Identity & PAM ── Tenant config
Game client (iframe)    ─┤  │  Wallet & Ledger ── Risk & AML         launch/catalogue
Sportsbook widget       ─┼─▶│  Bonus engine    ── KYC orchestrator ─▶ Game aggregator
Operator back office    ─┤  │  Game gateway    ── Responsible gaming   (Hub88, SoftSwiss, Pariplay)
Partner console         ─┘  │  Sportsbook adapter ── Event bus       ▶ Sportsbook provider
                             │  Payment orchestrator ── Reporting&BI   (Altenar, BetBy, Digitain)
                             │  Audit log · RBAC · append-only history ▶ PSPs & crypto rails
                             │  PostgreSQL — one source of truth       ▶ KYC/AML vendor (SumSub, Veriff)
                             └──────────────────────────────────────▶ Affiliate platform
                                        ▲
                                        │ wallet callbacks (highest-traffic,
                                        │ lowest-latency, most correctness-
                                        │ critical surface in the system)
```

REST/JWT connects brand surfaces to the core. The one inbound arrow — the
game aggregator calling the wallet on every spin — is the surface most
likely to cause an incident and is the focus of `06-wallet-ledger-
architecture.md` and `08-casino-integration-architecture.md`.

## The nine core services (Blueprint §4)

1. Identity & PAM
2. Wallet & Ledger
3. Bonus engine
4. Game gateway
5. Sportsbook adapter
6. Payment orchestrator
7. Tenant config
8. Risk & AML / KYC orchestrator / Responsible gaming (one compliance
   subsystem, three concerns)
9. Event bus / Reporting & BI

Plus platform-wide cross-cutting concerns: audit log & RBAC (append-only),
and PostgreSQL as the single source of truth for money.

## Non-functional targets (Blueprint §6)

These are contractual/operational, not aspirational — aggregators suspend
integrations that miss them.

| Property | Target | Why |
|---|---|---|
| Wallet callback p99 | < 150 ms | Provider timeouts trigger retries → rollback storms → suspension. |
| Platform availability | ≥ 99.95% | ~4h/year. Aggregator contracts set availability floors. |
| Ledger RPO | 0 | Synchronous replica; committed financial transactions cannot be lost. |
| Ledger RTO | < 15 min | Automated, rehearsed failover. |
| Reconciliation drift | 0 | Projection vs. recomputed ledger, checked hourly. Non-zero pages someone. |
| Game launch p95 | < 800 ms | Directly tied to player drop-off. |
| Audit retention | 5–7 years | Immutable, exportable. |
| Report freshness | < 5 min | Retention teams act intraday. |

## Technology baseline (RECOMMENDATION — Blueprint §7)

| Layer | Choice | Reasoning |
|---|---|---|
| System of record | PostgreSQL 16+ | Strong consistency, RLS, partitioning. No eventual-consistency story works for a balance. |
| Wallet service | Go | Predictable tail latency (no GC pause spikes) — the one service with a contractual p99. |
| Other services | Go or Kotlin | Node/TS acceptable for back-office APIs. One hot-path language, one general-purpose language; three is a hiring problem. |
| Event bus | Kafka (NATS JetStream if ops capacity is the binding constraint) | Per-player ordering matters for the bonus engine. |
| Analytics | ClickHouse, fed by CDC (Debezium) | Keeps reporting load off the ledger. |
| Cache/sessions | Redis | Sessions, tenant config, rate limits. **Never balances.** |
| Brand frontend | Next.js | SSR matters — casino affiliate traffic is SEO-driven. |
| Back office | React + a real virtualized data grid | Operators live in tables of tens of thousands of rows. |
| Hosting | Hetzner/OVH/Leaseweb-class — **verify in writing first** | AWS/GCP acceptable-use policies vary by region on gambling; confirm before building on any host. |
| Secrets | Vault or cloud KMS | Per-tenant PSP credentials, rotating provider HMAC keys. |
| Observability | OpenTelemetry | Per-provider latency SLOs, per-tenant error budgets. |

Confirm this baseline at the Stage 1 gate; it is a recommendation, not yet
a locked decision (tracked in `docs/decisions/0003-technology-stack.md`).

## Related documents

`01-requirements-inventory.md`, `02-domain-and-service-boundaries.md`, and
the per-domain architecture docs in this directory.

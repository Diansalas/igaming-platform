# 02 — Domain and Service Boundary Proposal

Status: Stage 0 proposal, owned by `architect`, subject to revision at the
Stage 1 gate once scaffolding decisions are locked.

## Principle

Service boundaries follow the nine core services in Blueprint §4, grouped
by who is authorized to change them (see `.claude/agents/`). A boundary is
correct if a single specialist can own it without needing another
specialist's sign-off for routine changes, while cross-cutting invariants
(money, tenant isolation, audit) are enforced by shared, review-gated
components rather than duplicated per service.

## Proposed services (Stage 1+ scaffolding, not all built at once)

| Service | Owns | Primary specialist |
|---|---|---|
| `identity` | Person/player model, sessions, cross-brand resolution | identity-compliance |
| `tenant-config` | Brand/tenant configuration, versioning | backend / architect |
| `wallet` | Ledger, account balances, idempotent postings | ledger-finance |
| `game-gateway` | Aggregator/provider adapters, catalogue, launch tokens, wallet-callback endpoint | casino |
| `sportsbook-adapter` | Sportsbook provider integration, open-bet liability | sportsbook |
| `bonus-engine` | Campaign/Offer/Grant/Progress, rule evaluation | bonus-engine |
| `payment-orchestrator` | PSP/crypto routing, reserve accounting, withdrawal workflow | payments |
| `compliance` | KYC/AML orchestration, RG controls, case queue | identity-compliance |
| `backoffice-api` / `partner-console-api` | Admin operations, RBAC-gated | backend / backoffice |
| `audit` | Append-only audit log, shared library used by every mutating service | security (design), backend (implementation) |
| `event-bus` (Kafka/NATS) | Cross-service event distribution | integrations / architect |
| `reporting` | CDC → ClickHouse, report definitions | data-analytics |

`RECOMMENDATION`: start Stage 1 with fewer, coarser services (e.g.
`wallet`, `game-gateway`, `identity+compliance`, `backoffice-api` as one
deployable each) and split further only when a real scaling or
ownership need appears — per Blueprint's own bias toward not
over-building ahead of proven need (§13).

## Contract discipline

- Every cross-service call is a versioned, documented API (internal or
  external) — never a shared database table written by two services.
- Only `wallet` may write ledger tables. Every other service that needs to
  move money calls `wallet`'s API.
- Only `audit`'s shared library appends to the audit store; services call
  it, they don't implement their own audit writer.
- `tenant-config` is the single source of truth for what a brand looks
  like; no service caches tenant config longer than its documented TTL.

## What is explicitly NOT a separate service (yet)

Per `CLAUDE.md`'s scope-expansion test, the following stay inside an
existing service until a real need forces a split: affiliate tracking
in-house rebuild (Blueprint §1 table — third-party first, in-house is a
year-two consideration), CRM/campaign journey builder (buy first), and any
per-jurisdiction reporting engine beyond a pluggable export interface.

## Open question

Whether `identity` and `compliance` should be one service or two is
deferred to Stage 2 design, when the actual KYC vendor contract shape
(Blueprint §10 Q2 dependency) is known.

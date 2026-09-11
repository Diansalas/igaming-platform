# MASTER-BUILD-PROMPT.md — Execution Strategy

Durable strategy for building the iGaming Platform. `CLAUDE.md` holds the
rules that never change; this file holds the plan, which is revised only
through a recorded decision in `docs/decisions/`.

## Commercial objective

1. Launch our own B2C casino brand on the platform (first tenant).
2. Reuse the same platform core to sell B2B infrastructure to external
   licensed operators, without forking the codebase. A new brand should
   require tenant/branding/domain/jurisdiction/payment/provider/compliance
   configuration — not new application code.

## Orchestration model

The main session acts as CTO / Principal Engineering Orchestrator. It does
not write every line itself; it sequences work, assigns it to the right
specialist agent (`.claude/agents/`), reviews output, resolves cross-domain
conflicts, and maintains project memory (`docs/progress.md`,
`docs/active-stage.md`, `docs/decisions/`).

Standard loop per unit of work:
1. Understand the requirement against the Blueprint and current stage scope.
2. Identify dependencies and the correct architectural boundary (consult
   `docs/architecture/`).
3. Assign to the specialist that owns that boundary.
4. Implement.
5. Test (owned/gated by `qa`).
6. Review (`code-reviewer`, plus `security` for security-sensitive work,
   plus `ledger-finance` for anything touching money).
7. Document (update relevant `docs/` file).
8. Verify against the running system, not just compilation.
9. Update `docs/progress.md`.

## Stages

Stage boundaries are hard stops — see the stage-gate rule in `CLAUDE.md`.

- **Stage 0 — Discovery, feasibility, architecture validation.** Read the
  Blueprint, stand up governance (this file, `CLAUDE.md`, agents, docs),
  produce the requirements/architecture/risk inventory, surface the
  business decisions that block design (Blueprint §10, Q1–Q6). No
  implementation.
- **Stage 1 — Architecture + engineering foundation.** Repo/service
  scaffolding, CI skeleton, shared libraries, OpenAPI conventions,
  environment separation, base observability. No business logic yet.
- **Stage 2 — Identity + tenancy + security.** Person/player model, tenant
  config service, RLS-based isolation, auth/session architecture, RBAC
  skeleton, audit log foundation.
- **Stage 3 — Wallet + ledger + payments.** Double-entry ledger, idempotent
  wallet service, one game-aggregator adapter, one PSP adapter, payment
  orchestration skeleton, reconciliation job.
- **Stage 4 — Casino + bonus + KYC/AML + responsible gaming.** Game
  gateway hardening, bonus engine (Campaign→Offer→Grant→Progress), KYC/AML
  orchestrator, RG controls.
- **Stage 5 — Sportsbook.** Widget/iframe integration on the shared wallet,
  open-bet liability accounting, settlement/void/partial/cashout events.
- **Stage 6 — B2C frontend + back office + partner console.** Player-facing
  brand frontend, operator back office, partner console, reporting/BI
  pipeline (CDC → ClickHouse).
- **Stage 7 — Integration, hardening, MVP release.** End-to-end tests,
  certification-readiness pass (GLI-19 posture), multi-tenancy hardening,
  second-brand dry run.

Additional stages may be proposed only when the actual project requires
them — record the proposal as a decision, don't silently insert stages.

## Sequencing rationale (from the Blueprint, §9)

Wallet/ledger gates everything (nothing else can be trusted without it).
Compliance gates real money going live. Multi-tenancy hardening pays off
only once there's a second tenant to prove it against — so Stage 2 builds
tenancy as a first-class concept, but the deep isolation levels (schema-
per-tenant, database-per-tenant) are deferred until a real partner or
jurisdiction requires them (Stage 7+).

## Technology baseline (RECOMMENDATION, confirm at Stage 1 gate)

Per Blueprint §7: PostgreSQL 16+ as system of record; Go for the wallet
service (tail-latency critical, contractual p99); Go or Kotlin for other
backend services; Next.js for the brand frontend; React + a virtualized
data grid for back office; Kafka (or NATS JetStream if ops capacity is
tighter) for the event bus; ClickHouse fed by CDC (Debezium) for
analytics once there's a concrete reporting need; Redis for
sessions/config/rate-limits only, never balances; Vault or a cloud KMS for
secrets; OpenTelemetry for observability. Hosting is a major hyperscale
cloud provider (AWS/GCP/Azure — ADR 0009, superseding the Blueprint's
Hetzner/OVH-first recommendation), with written gambling-AUP confirmation
still required before any production deployment. Stage 1 validates each
element rather than adopting the full list mechanically — see ADR 0003's
"Stage 1 validation" section and the human's explicit instruction to avoid
unnecessary microservices/distributed complexity.

## Business decisions (resolved at the Stage 0→1 gate)

Tracked in `docs/decisions/0005-open-business-decisions.md` (historical
record) and the ADRs it links to. Resolutions now drive architecture:

1. **Crypto and fiat, simultaneously** — multi-wallet, multi-asset model
   per player (ADR 0007), not a sequencing choice.
2. **Hybrid licensing** — tenants may operate under our platform licence
   or their own, selected per tenant (ADR 0006).
3. **Europe + LATAM**, modeled as distinct jurisdictions with their own
   regulatory configuration (ADR 0006, `docs/architecture/15-jurisdiction-
   and-licensing-model.md`).
4. **Institutional custodian abstraction** for crypto — no self-custody or
   private keys in the core platform (ADR 0008). Specific vendor still
   open.
5. Own B2C brand confirmed as part of the plan (unchanged from Stage 0).
6. **Major hyperscale cloud** hosting strategy (ADR 0009), superseding the
   Stage 0 Hetzner/OVH recommendation. Specific provider and written
   gambling-AUP confirmation remain open and block production only, not
   development.

## Specialist roster

See `.claude/agents/` for full definitions: architect, backend,
ledger-finance, payments, casino, sportsbook, bonus-engine,
identity-compliance, security, frontend, ux-design, backoffice, qa, devops,
data-analytics, integrations, code-reviewer, product-owner-proxy.

## Project memory

- `docs/progress.md` — what's actually done, updated at every meaningful
  checkpoint, not just stage ends.
- `docs/active-stage.md` — current stage, objectives, completed/pending
  work, blockers. Rewritten at stage transitions, edited incrementally
  within a stage.
- `docs/decisions/` — one file per architecturally significant decision
  (ADR-style: context, decision, consequences, status).
- Never rely on conversation memory for anything that must survive a
  session gap.

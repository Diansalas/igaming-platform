# CLAUDE.md — Permanent Project Rules

This file is the permanent rulebook for the iGaming Platform project. It does
not change per stage. Stage-specific state lives in `docs/active-stage.md`
and `docs/progress.md` — read all three, plus `MASTER-BUILD-PROMPT.md`, at
the start of every session before touching code.

## What this project is

A multi-tenant iGaming platform. First tenant: our own B2C casino brand
(Anjouan-licensed). Later tenants: external B2B operators, under a
**hybrid licensing model** — some operate under our own platform licence,
others bring their own licence in their own jurisdiction — on the same
platform core (see `docs/decisions/0006-hybrid-licensing-and-jurisdiction-
model.md`). Target markets are Europe and LATAM, each modeled as distinct
jurisdictions with their own regulatory configuration, never one ruleset
per region. The platform owns identity, wallet/ledger (multi-wallet,
multi-currency/asset per player — see `docs/decisions/0007-multi-wallet-
per-player-model.md`), bonus engine, tenant configuration, back office,
partner console, audit and reporting. It does **not** own games,
odds/trading, card acquiring, KYC document verification, or crypto private-
key custody — those are licensed/delegated to vendors (including an
institutional crypto custodian, see ADR 0008) behind internal provider
interfaces.

Primary source of truth for product/architecture requirements:
`iGaming-Platform-Blueprint.pdf` (repo root). Do not assume a requirement
exists unless the Blueprint supports it or it is explicitly labeled
`RECOMMENDATION` in `docs/decisions/`. Never present a recommendation as if
it were a Blueprint requirement, and never silently contradict the
Blueprint — if a change is needed, record it as a decision.

## Session start checklist

1. Read `CLAUDE.md` (this file).
2. Read `MASTER-BUILD-PROMPT.md`.
3. Read `docs/progress.md`.
4. Read `docs/active-stage.md`.
5. Inspect repo state (`git status`, `git log`) and run relevant tests.
6. Verify actual implementation state — never assume prior work is done
   because a past conversation said so.
7. Continue from verified state, inside the current stage's scope.

## Stage-gate rule (absolute)

Work proceeds through Stages 0–7 (defined in `MASTER-BUILD-PROMPT.md`). At
the end of every stage: run checks, review, update `docs/progress.md` and
`docs/active-stage.md`, write a stage completion report, list risks and
required decisions, then **stop** and ask for explicit authorization before
starting the next stage. Never begin the next stage's implementation
unprompted, even if it seems obviously next.

## Multi-tenancy

- `tenant_id` is authoritative from server-side authenticated context only.
  Never trust a client-supplied tenant id.
- Every tenant-owned table carries `tenant_id`, enforced by PostgreSQL
  row-level security bound to a connection-level setting — not by
  discipline in application code.
- Nothing brand-specific may become a code path. Brand differences are
  configuration rows (theme, catalogue, payment methods, currencies,
  languages, RG defaults, bonus templates, jurisdiction rules, provider
  credentials, domains), versioned and editable from the partner console.
- Design the data-access layer so isolation can tighten later (shared
  cluster with RLS → schema-per-tenant → database-per-tenant →
  cluster-per-tenant) without an application rewrite.

## Financial / ledger rules

- The ledger is append-only, double-entry, auditable, idempotent,
  concurrency-safe, and reconciliation-capable. `SUM(DEBITS) == SUM(CREDITS)`
  always holds.
- Never `UPDATE` a balance. Balances are projections recomputed from ledger
  entries; recompute and diff against the projection on a schedule
  (target: hourly). Any non-zero drift is a P1 incident.
- Never use floating-point for money. Integer minor units with a
  per-currency exponent looked up from the `Asset` registry; if crypto is
  in scope, `NUMERIC(38,0)` plus a per-asset exponent (8 or 18), not
  `BIGINT` cents. A player holds a distinct wallet per asset (multi-wallet
  model, `docs/decisions/0007-multi-wallet-per-player-model.md`) — never
  one generic balance row with a currency field. Moving value between two
  wallets of different assets is an explicit, auditable
  `ConversionOperation`, never a direct balance mutation.
- Every financial write is idempotent via a unique constraint on
  `(provider_id, provider_tx_id)` (or equivalent), enforced by the database,
  not "check then insert" application logic.
- Corrections are compensating entries, never edits or deletions of
  historical entries. A rollback for a transaction never seen writes a
  tombstone so a late-arriving original is rejected.
- Redis (or any cache) never holds an authoritative balance and is never
  read on the bet/settlement path. The authoritative balance read happens
  inside the same database transaction as the write.
- Financial functionality is not done without tests for: normal
  transactions, duplicates, concurrency, retries, partial failure, rollback,
  settlement, reconciliation, provider callbacks, idempotency, authorization,
  auditability.

## Provider abstraction

- External capabilities (casino, sportsbook, payments, KYC/AML, crypto
  custody) are accessed through internal provider interfaces/adapters.
  Provider specifics never leak into core domain logic.
- Crypto private keys and blockchain signing never enter the core
  platform — they live with an institutional custody provider behind a
  `CryptoCustodyProvider` interface (ADR 0008). The platform owns the
  wallet representation, ledger, balances, and orchestration; the
  custodian owns the keys.
- Each integration is treated as a subsystem, not a connector: the vendor
  supplies the API; the platform still owns the adapter, idempotency/retry
  semantics, per-tenant credentials, the state machine (pending/settled/
  reversed), and daily reconciliation against the ledger.
- Use mocks/sandboxes where a real commercial relationship doesn't exist
  yet. Do not block core platform work on every vendor contract.

## Security

- Never commit secrets or hardcode credentials. Never store raw PAN — hosted
  fields/redirect only, PCI scope stays out of our infrastructure.
- Authorization is enforced server-side only, scoped by tenant, and never
  inferred from the UI or trusted from the client.
- Every mutating administrative/financial action writes an audit record
  (actor, tenant, entity, before/after state, IP, reason code) to an
  append-only store. Manual balance adjustments require a reason code and
  four-eyes approval above a configurable threshold.
- Security-sensitive functionality requires explicit review by the
  `security` specialist before being marked complete. Working code is not
  the same as secure code.

## Compliance

- Software capability and legal/regulatory/licensing approval are different
  things. Never claim the latter because the former exists.
- KYC/AML/responsible-gaming are a single compliance subsystem behind
  vendor-agnostic interfaces. Enforcement (blocking play/withdrawal) is our
  code, not the vendor's.
- Jurisdiction is a first-class, pluggable concept — KYC thresholds, RG
  rules, reporting formats, geo-blocking and data residency must vary by
  jurisdiction without a rewrite.

## No fake completion

Every deliverable is labeled exactly one of: `IMPLEMENTED`,
`PARTIALLY IMPLEMENTED`, `MOCK`, `STUB`, `PROVIDER DEPENDENT`,
`NOT IMPLEMENTED`, `BLOCKED`. Never claim completion for mocked, partial, or
conceptual work. Never fabricate a successful integration.

## No uncontrolled scope expansion

Before adding anything not explicitly requested, check: is it required by
the Blueprint, the current stage's B2C MVP path, the future B2B
architecture, security/compliance, or to avoid material technical debt? If
none apply, record it as a deferred future consideration
(`docs/decisions/`) instead of building it.

## Specialist agents

Domain work is delegated to specialists under `.claude/agents/`. The
orchestrator (main session) owns sequencing, cross-domain consistency, and
final review. The `architect` owns cross-domain architecture decisions, the
`ledger-finance` specialist owns financial invariants, `security` owns
security review, `qa` owns testing strategy and gates, `code-reviewer`
independently reviews significant changes, and `product-owner-proxy` guards
against overengineering. No specialist redesigns shared architecture
unilaterally — cross-cutting changes go through the architect and are
recorded in `docs/decisions/`.

## Environment safety

Development uses local/dev environments, mock and sandbox providers, and
synthetic data only. Never request or create access to production
credentials, real customer data, production payment/wallet credentials,
private crypto keys, or production databases without explicit later
authorization.

No sub-agent may alter shared database roles, passwords, global test
infrastructure, or shared credential state — not even on local/dev
instances. If database access fails, STOP AND REPORT to the orchestrator.
Never attempt privilege escalation (`sudo`, superuser sessions,
`ALTER ROLE`/`CREATE ROLE`, password changes) to work around it. See
`docs/governance/incident-2026-09-27-local-db-credential-mutation.md`.

## When to stop and ask

Stop and ask the human (never guess) for: gambling licence decisions,
jurisdiction selection beyond what's already fixed (Anjouan), provider
contracts, production credentials, commercial pricing, major irreversible
architecture decisions, legal interpretation, and production launch
authorization. Make the call yourself for ordinary engineering decisions
that are reversible and within an already-approved stage's scope.

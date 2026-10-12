# Load, soak and chaos test plan

Status: **PLAN, plus one SMALL local smoke that was actually run.** Load, soak and chaos testing as a discipline is
`NOT IMPLEMENTED` (gap audit section 13). The smoke below is not a capacity test, makes no capacity claim, and runs against
mock providers on a shared sandbox VM. Nothing here targets or authorises any non-local environment (AWS is OFF).

## 1. What was actually run (local smoke)

`deploy/loadtest/local-smoke.sh` builds `cmd/migrate` and `cmd/platform-api`, creates a **private scratch database**
(`drill_bk_smoke_*`, dropped on exit), migrates it, seeds synthetic data (`deploy/drill/seed.sql`), starts `platform-api` with
`APP_ENV=development` on `127.0.0.1:18080` and drives it with `deploy/loadtest/smoke` (standard library only, loopback targets
only, no new dependency). Scenarios: `GET /healthz` (no DB), `GET /readyz` (DB ping), `GET /v1/me` (JWT verification plus a
tenant-scoped RLS read) after a one-time player registration per worker. Server stopped with `kill <pid>`. **The server runs as the migration-owner role, not `igaming_runtime`** (dev mode allows it; production startup refuses an owner-privileged connection), so the smoke does not exercise the runtime role's grants. The JWT signing secret is a fresh random value per run; `internal/config` only requires 32+ characters and has no dev-literal denylist for JWT, which is why no literal secret is kept in the repository. Production additionally refuses the mock adapters at startup (MOCK-ADAPTER-PROD-1; an unset `APP_ENV` counts as production), so a dev-mode process cannot be mistaken for a production one.

```sh
export TEST_ADMIN_DATABASE_URL=... TEST_DATABASE_URL=...
deploy/loadtest/local-smoke.sh --dry-run
deploy/loadtest/local-smoke.sh -c 8 -d 10s
make smoke-local
```

Result, 2026-10-12, 8 workers, 10 s, Postgres 16.13 on the same VM, mock providers, owner-role connection (dev mode):

| Scenario | Requests | Errors | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|
| healthz | 24,754 | 0 | 0.45 ms | 1.47 ms | 2.94 ms | 10.7 ms |
| readyz | 24,754 | 0 | 0.65 ms | 1.98 ms | 3.55 ms | 12.7 ms |
| me | 24,754 | 0 | 1.49 ms | 3.66 ms | 5.98 ms | 22.7 ms |

What this shows: the dev-mode server starts on a freshly migrated scratch database, serves these three paths without error
under 8 concurrent clients, and `/v1/me` costs roughly 1-3 ms on this machine. What it does **not** show: any capacity
(client and server share the machine and the database with other work), anything about write paths, the money path, the
sweepers, connection-pool limits, the per-IP rate limiter (raised to avoid throttling the setup; see script), TLS, a load
balancer, real providers, real network or a production-sized database. Do not quote these numbers as a capability.

## 2. Principles for the real test (when an environment exists)

- Synthetic data and mock/sandbox providers only; never production data or credentials; a **dedicated** environment that is
  not shared with developers (the local VM is shared, so its numbers are noise by construction).
- Test the **runtime role** (`igaming_runtime`) connection, as production does; the dev smoke uses the owner.
- Observe through the same signals production will have: `deploy/observability/` rules once wired, plus Postgres statistics.
  Where the platform emits no signal (DB pool saturation, reconciliation staleness) the test must add a measurement, and
  the gap becomes an instrumentation ticket (`deploy/observability/pending-signals.yml`).
- Every run records: commit, migration head, dataset size, instance sizes, configuration (`DATABASE_MAX_CONNS`, sweep
  intervals, rate limits), warm-up, duration, and raw results. No result without its conditions.
- Cap load deliberately and ramp; abort on error-rate or latency guards. Correctness checks run **during** load, not after.

## 3. Scenarios

| # | Scenario | Load shape | Pass criteria (to be agreed; starting points) | Notes |
|---|---|---|---|---|
| L1 | Read mix: catalogue, `/v1/me`, balances | ramp to target RPS, hold 15 min | 5xx < 0.1%, p95/p99 within SLO draft, pool not saturated | SLOs are NOT defined yet: Blueprint section 7 latency/error budgets need numbers (human decision) |
| L2 | Auth: register / login / refresh | ramp; per-IP limit **on** | limiter refuses excess with 429, no 5xx, password-hashing cost does not starve other routes | exercises `AUTH_RATE_LIMIT_PER_MINUTE`, trusted-proxy handling |
| L3 | Deposit to settle (mock PSP) | steady and burst, duplicates and retries injected | zero duplicate ledger postings, `(provider_id, provider_tx_id)` idempotency holds, all attempts reach a terminal state, reconciliation clean | financial correctness under load is the point; duplicates, retries and callback reordering are part of the load |
| L4 | Casino bet/win/rollback callbacks (mock provider) | concurrent rounds per player and across players | no deadlock beyond handled retries, ledger balanced per tenant/asset, projection equals recompute afterwards | ADR 0082 lock ordering under contention |
| L5 | Sportsbook place/settle/void | concurrent placement and settlement batches | same as L4 | |
| L6 | Withdrawal request / approve / complete | concurrent, with holds | hold invariant (migration 0132 guards) never trips, no stranded hold | |
| L7 | Webhook flood | above the admission limits for one provider kind | bulkheads refuse with the documented reasons, other domains unaffected, `/readyz` stays up, **pool is not exhausted** (F-POOL-2) | verifies ADR 0097 bulkheads and `webhook_admission_*` metrics |
| L8 | Background loops under load | sweepers and reconciliation running during L3-L6 | sweeper last-pass advances, reconciliation cycle completes within its interval, no starvation of request traffic | pool sharing between loops and requests |
| S1 | Soak, 24-72 h at 30-50% of the L1-L6 mix | constant | no memory growth trend, no connection or goroutine leak, no table/index bloat beyond expectation, no log volume surprise | watch RSS, goroutines, `pg_stat_activity`, WAL volume, disk |
| S2 | Soak with periodic deploys (rolling, two versions on one schema) | rolling restarts every N minutes | zero 5xx beyond a bounded window, `/readyz` gating works | validates expand/contract coexistence |
| C1 | Chaos: database restart / failover | kill DB connections during L3 | platform fails closed, recovers, no lost or duplicated financial write, reconciliation clean | only on a dedicated environment; never shared dev |
| C2 | Chaos: provider latency / 5xx / hang (mock) | inject at the mock | timeouts bounded, cascade works, no pool exhaustion, no DB transaction held across provider I/O | ADR 0095 transaction-boundary rule |
| C3 | Chaos: duplicate / reordered / late callbacks | replay and shuffle | tombstones, idempotency, no double posting | |
| C4 | Restore under load | take a backup point, continue load, restore | RPO/RTO measured honestly; drill checks pass | needs a real backup mechanism (NOT MET) |

## 4. Tooling

The in-repo harness (`deploy/loadtest/smoke`) is deliberately tiny. For real load shapes use an established tool (k6, vegeta)
**only after a decision to add it**: it would be a new dependency and a new CI/ops surface, so it is not added here. The
financial scenarios (L3-L6) need a scripted scenario runner with assertions on the ledger, not just HTTP statistics.

## 5. Preconditions that are not met

- A dedicated, non-shared test environment (no AWS; local VM is shared).
- Agreed SLOs and error budgets (Blueprint section 7).
- A metrics backend and wired alert rules (ALERT-DELIVERY-1 open; OTel exporter supports stdout/none only).
- Instrumentation for DB pool saturation and reconciliation staleness.
- A backup mechanism for C4.
- Real provider behaviour: MOCK only, so provider-side latency and failure shapes are assumptions.

## 6. Reporting

Each run produces a short report: conditions (section 2), scenario results, correctness checks (ledger balanced, projection
equal, reconciliation clean), alerts that would have fired (against `deploy/observability/`), defects found with owners. No
claim of capacity or readiness is made from a smoke or from a single run.

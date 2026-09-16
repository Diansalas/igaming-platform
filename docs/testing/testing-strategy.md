# Testing Strategy

Status: Stage 0 draft. Owned by the `qa` specialist going forward. A
feature is not complete because it compiles, the server starts, or a
happy-path request works — per `CLAUDE.md`.

## Baseline requirement (every non-trivial change)

- Unit tests for business logic.
- Integration tests for API endpoints.
- Authorization tests (a request without the right role/scope must be
  rejected).
- Tenant-isolation tests (a valid token for tenant A must never read or
  write tenant B's data) for anything touching tenant-scoped data.

## Financial functionality (wallet, ledger, payments, bonus postings) — mandatory full matrix

Per `CLAUDE.md`, no financial code is done without tests covering:

1. Normal transactions.
2. Duplicate/replayed requests (idempotency).
3. Concurrent requests against the same account (race conditions must not
   corrupt the balance).
4. Retries (provider retries the same `provider_tx_id` — must be a no-op
   returning the same result).
5. Partial failures (network drop after write, before response).
6. Rollback, including rollback of a transaction never seen (tombstone
   behavior) and rollback of a transaction seen (compensating entry).
7. Settlement (including partial settlement for sportsbook).
8. Reconciliation (recomputed balance vs. projection; drift detection).
9. Provider callbacks (the actual inbound bet/win/rollback/balance path).
10. Idempotency under concurrency, not just sequentially.
11. Authorization on financial endpoints.
12. Auditability (every financial mutation produces a traceable audit
    record).

## Provider integrations

Adapter tests against sandbox/mock providers covering: happy path, timeout
+ retry, malformed/unexpected response shapes, and state-machine
transition correctness (pending → settled/reversed, never skipping or
reversing invalid transitions).

## End-to-end coverage

Critical flows get end-to-end tests once the relevant services exist:
registration → KYC tier gate → deposit → bet/spin → withdrawal;
self-exclusion → subsequent login/play attempt blocked across brands;
bonus grant → wagering progress → payout or forfeiture.

## Quality gates

- CI must fail the build on any failing test — `devops` owns verifying the
  pipeline actually enforces this rather than swallowing failures.
- No test is skipped, disabled, or quietly quarantined to unblock a
  merge; if that's ever genuinely necessary, it is a recorded decision via
  the orchestrator, not a silent QA call.
- `code-reviewer` checks that claimed test coverage actually exercises the
  failure modes it claims to, not just that tests exist.

## Test reporting standard (Stage 4G-FINAL)

Every completion report's test section states, per test suite/command
run, one of exactly five results — never a blanket "all clean" when any
suite actually flaked or failed:

- **PASS** — ran, every test passed, no flake observed.
- **FAIL** — ran, at least one test failed and the failure is real
  (reproducible, not investigated-and-dismissed as infrastructure noise).
  A stage is not complete with an unresolved FAIL on anything the stage
  touched.
- **FLAKE** — ran, failed at least once, but investigated and determined
  to be genuine non-determinism (not a correctness defect) — the report
  states the evidence for that conclusion (reproduction rate, isolated
  re-run results, root-cause mechanism if known), never just the label.
  A FLAKE the investigation cannot explain is a FAIL, not a FLAKE.
- **NOT RUN** — the suite exists but was not executed this stage (e.g. a
  suite gated on infrastructure unavailable in this environment) — the
  report states why.
- **BLOCKED** — the suite could not run due to an environment/dependency
  problem outside the change being validated (e.g. the database was
  unreachable) — distinct from NOT RUN in that it was ATTEMPTED and
  failed to execute at all, not skipped by choice.

For every suite, the report states: the exact command, the result, the
specific failure/flake if any, whether it blocks stage completion, and
the reason for that blocking determination. See any Stage 4G-FINAL-or-
later completion report's own test matrix for the applied format.

## What "done" requires

Every deliverable is labeled one of `IMPLEMENTED`, `PARTIALLY
IMPLEMENTED`, `MOCK`, `STUB`, `PROVIDER DEPENDENT`, `NOT IMPLEMENTED`,
`BLOCKED` — `qa` sign-off is what allows a label to move to `IMPLEMENTED`.

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

## What "done" requires

Every deliverable is labeled one of `IMPLEMENTED`, `PARTIALLY
IMPLEMENTED`, `MOCK`, `STUB`, `PROVIDER DEPENDENT`, `NOT IMPLEMENTED`,
`BLOCKED` — `qa` sign-off is what allows a label to move to `IMPLEMENTED`.

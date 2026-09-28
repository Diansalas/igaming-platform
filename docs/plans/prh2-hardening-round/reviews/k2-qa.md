# QA review — PRH-2 K2 (2026-09-28)

**Reviewer:** `qa`. The orchestrator recorded this review.

**Scope:** branch `prh2-k2-manual-adjustments` @ `18c357a`. The reviewer ran tests on a private DB (dropped afterwards):
- `internal/adjustment`: 39 test functions pass;
- `TestManualAdjustmentAPI_*` and `TestK2G*`: pass.

## Verdict: PASS

| Category | Tests | Adequate |
|---|---|---|
| Normal transactions | B-1, B-3 | Yes |
| Duplicates | B-4, plus the idempotency key via B-7 part 2 | Yes |
| Concurrency | B-7 parts 1, 1b, 2 and 3, with **real forced contention** (a `pg_stat_activity` lock-waiter barrier) | Yes |
| Retries | B-7 part 2, B-4 | Yes |
| Partial failure | B-15 | Yes |
| Rollback | B-15; the compensating-entry design | Yes |
| Settlement | N/A; execution in the final transaction is the analogue | Scope-appropriate |
| Reconciliation | B-18 (drift = 0), B-25 (the detector) | Yes |
| Provider callbacks | N/A; B-23 reuses the payment exposure state | Scope-appropriate |
| Idempotency | UNIQUE key plus B-7 part 2 | Yes |
| Authorization | B-3 (the K1-1 negatives inside the real acting executor), B-5, B-6, the HTTP A-1 matrix | Yes |
| Auditability | B-17, HTTP A-12 | Yes |

**Further checks, all passing:**
- All of B-1..B-26 exist and are non-vacuous.
- The invariant helper (per-transaction and tenant-wide SUM(D) = SUM(C), plus projection = recomputed) is called after every financially effective test.
- B-19 up/down/up compares a whole-schema snapshot, and the down refusals are tested.
- The mutation evidence has 68/68 killed; about 10 entries were spot-checked against the source and all matched. Process failures are honestly disclosed.

## Notes (non-blocking)

1. The two `internal/reconciliation` legacy-shape failures are a pre-existing G1 regression. **The orchestrator fixed it on main** in `9fcd08e`: `audit.Record` now names `subject_tenant_id` only when it is set.
2. HD-PRH2-8 is still open; interim (b) is implemented.
3. LEDGER-MANUAL-ADJ-LINK-1 is deferred and launch-blocking; the detective control (B-25) ships.

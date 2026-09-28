# Security review — PRH-2 A, CAS-REVOKE-CONSUMED-1 (2026-09-28)

Reviewed commit `eebd982` (branch `cas-revoke-consumed-1`, based on `cabca27`). Method: `git archive` into scratch, private databases (dropped afterwards), no role changes.

**Verdict: ACCEPT.** CAS-REVOKE-CONSUMED-1 can be **CLOSED once merged**. The separately registered CAS-REVOKE-BET-RACE-1 stays open.

**What was verified:**
- **Migration.** The function bodies were diffed mechanically:
  - down vs 0042: **IDENTICAL**;
  - up vs 0042: only the four spec lines (`0108…up.sql:52-55`), taken verbatim from the FH-7 "Required fix (a)".
  - 0042 was the latest definition of the function, and the trigger stays `BEFORE UPDATE … FOR EACH ROW`.
- **`RevokeLaunchSession`** (`launch.go:399-415`): `SELECT … FOR UPDATE`, then an UPDATE with `status IN ('active','consumed')`, returning the prior status. Its only caller is `launchFailed` (`orchestrator.go:544`), which audits `prior_status` in the same transaction (`:554`).
- **Tenant isolation.** A cross-tenant revoke returns `ErrLaunchSessionNotFound`, so there is no existence oracle and nothing is written. This is the correct semantics.
- **Audit.** No raw token appears in any audit, log or error.
- **Tests.** The full `./internal/casino/...` suite passes under `-race` with the integration tag. The targeted run gave 24 PASS, 0 FAIL, 0 SKIP. The inverted test parses the audit JSON, so the N-A fix holds.
- **Mutants re-killed by security:**
  - M1 (whole-row equality dropped): killed by the `consumed_at` change and `consumed_at` NULL cells.
  - M4 (compare-and-swap back to `active` only): killed by the inverted, replay and settlement tests.
  - M5, security's own (audited `prior_status` hard-coded): killed at `launch_two_phase_integration_test.go:792`.
- **ADR 0095 §15.1.5 amendment:** accurate.

**Findings (none blocking):**

| ID | Sev | Finding |
|---|---|---|
| F-1 | Info | The tenant-isolation test's comment says "zero rows, not an error", but the test asserts `ErrLaunchSessionNotFound`. Fix the wording. |
| F-2 | Low | The refused cells in the trigger matrix accept any error. Assert SQLSTATE `P0001` and the trigger message in a later test-hygiene pass. |
| F-3 | Low | No test pins `FOR UPDATE`. Correctness does not depend on it, because the UPDATE's `WHERE` is the atomic compare-and-swap. An optional two-revokes concurrency test would cover it. |
| F-4 | Info | CAS-REVOKE-BET-RACE-1 is unchanged and remains launch-blocking. |
| F-5 | Info | The evidence file gives the path as `internal/casino/migrations/0108…`; the real path is `migrations/0108…`. |

**Effect on other work:** workstream B may start after A merges. Condition C-103-3 applies: assert `revoked == true`.

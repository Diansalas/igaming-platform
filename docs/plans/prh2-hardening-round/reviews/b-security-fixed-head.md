# Security review (fixed head) — PRH-2 B (2026-09-28)

**Reviewer:** `security` (the hard gate). The orchestrator recorded this review.

**Scope:** `e720001..e810ece`, excluding the merged main content. Tests ran on private DBs, dropped afterwards.

## Verdict: ACCEPT WITH CONDITIONS

There is one condition, B-C3, and it is only a test pin. There is no open security defect in production code.

**`FOR NO KEY UPDATE`**
- (a) The scope is exactly `(token_hash, tenant_id)`.
- (b) At `-race -count=50`, 100/100 runs have exactly one winner.
- (c) The ABBA test reproduces 40P01 under `FOR UPDATE` and passes under the fix.
- (d) Replay and idempotency are unchanged. F-7 now fails closed on any foreign constraint conflict.

**Formal ADR 0103 §4 acknowledgement:** `FOR NO KEY UPDATE` is accepted.
- It still serialises concurrent bootstraps, and it still conflicts with the revoke's `FOR UPDATE`.
- It gives up only the FK `KEY SHARE` conflict, and the key columns involved are immutable.
- The acknowledgement is **provisional until B-C3 lands**, because a silent revert would be undetectable.

**Closed:**
- B-C1 (init-app-role grants after the backfill);
- B-C2 (the duplicate UNIQUE removed);
- F-2 (the uniform 401 and constant 403);
- the P2-2 pin;
- F-6 (the secrecy scan), with note L-1;
- the missing F-3 revoke-false test, accepted as unreachable under the row lock.

**Mutants re-killed:**
- CAS expiry: KILLED.
- Provider binding: KILLED.
- **Lock mode `FOR NO KEY UPDATE` → `FOR UPDATE` (`bootstrap.go:242`): SURVIVED.** This is B-C3.

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| B-C3 | Medium (test-only; blocks marking B complete) | The production lock clause is not pinned. The lock-order test uses its own Sprintf query, so reverting the production lock to `FOR UPDATE` passes every casino and httpserver bootstrap and lock-order test. | Have the fix subtest call the real `getLaunchSessionForBootstrap`/`BootstrapLaunch`, keeping the Sprintf `FOR UPDATE` case as the mutant control. Show the lock-mode mutant now fails. |
| L-1 | Low | The secrecy scan does not cover the 5xx path or the replay-success log. | Extend the scan to both. |
| L-2 | Low | The gate-denial path's revoke upgrades to `FOR UPDATE` while holding the RG advisory lock. No cycle was found. | Add the denial path to the lock-order harness. |

**Tests (local, not CI):**
- casino `-race`: ok.
- httpserver: everything passes except the two `TestResolutionIsolation_*` timing-lane tests, which failed under the reviewer's own concurrent DB rebuild and passed when re-run alone. B does not touch them.

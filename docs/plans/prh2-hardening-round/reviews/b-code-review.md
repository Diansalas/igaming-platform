# Code review — PRH-2 B (CAS-PLAY-BOOTSTRAP-1), 2026-09-28

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** `a7b2720..e810ece` (first-parent `e720001`..`2696dbb`), excluding the merged main content. Tests ran on a private DB (`cr_b_rv`), dropped afterwards.

## Verdict: READY WITH CONDITIONS

C-1 and C-2 must land before merge.

The production logic is correct against ADR 0103 §3.2–§3.4:
- the F-1 lock fix closes the ABBA deadlock (40P01 reproduced at `-count=5` under `FOR UPDATE`, never under the fix);
- concurrent consumes are correct under a forced, verified race;
- byte-identical replay is sound.

The gap is regression protection: four mutants (MF1, MC2, MD, M7) survive the whole suite.

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| C-1 | **High (pre-merge)** | Same as security's B-C3. The lock-order test's racer A is a Sprintf copy, never `getLaunchSessionForBootstrap`. **MF1** (bootstrap.go back to `FOR UPDATE`) survived the full casino suite. | Racer A calls the real `getLaunchSessionForBootstrap`, then `rg.EvaluateEligibility`. Keep the raw `FOR UPDATE` only as the positive control. Correct the doc comment. |
| C-2 | Medium (pre-merge) | The race between a different token and the same `request_id` (the `IdempotentInsert` conflict path) is untested. A forced harness (blocker transaction, 2 waiters confirmed in `pg_stat_activity`, 10/10 runs) gives 1 success on the real code. **MC2** (the conflict branch accepts) gives 2 successes and 2 consumed sessions under the harness, yet **survived** the existing suite. | Add the forced-contention test. |
| C-3 | Medium | SB-6 varies only `asset_code`. **MD** (drop `mode` from the digest) survived: a `mode:"demo"` request would replay a `real` bootstrap. | Table-drive SB-6 over `provider_game_id`, `asset_code` and `mode`. |
| C-4 | Low | "Revoke returns false → 5xx" can be forced in a scratch DB with a test-only `BEFORE UPDATE` trigger that returns NULL, with no production seam. **MR** (invariant check removed) survived. | Add the test, or have the architect record it as a residual. |
| C-5 | Low | The F-7 branch is unreachable through `BootstrapLaunch`. **M7** survived. | Accept it as defence in depth. Pin that `IdempotentInsert` returns the violated constraint's name. |
| C-6 | Low | B adds 14 new `errcheck` findings (`resp.Body.Close()` in two new test files) under `--build-tags=integration`: 666 → 680 vs main `9a65680`. The standard gate without tags: 0. | Use `defer func() { _ = resp.Body.Close() }()`. |
| C-7 | Low | Docs drift: (a) the `ResponseJSON` comment; (b) the `getLaunchSessionForBootstrap` comment says the revoke is a plain UPDATE (it is `FOR UPDATE`, `launch.go:428`; no cycle was found); (c) the evidence overclaims "12/12"; (d) the 401/403 bodies are identical apart from `request_id` and use the `apierror` envelope. | Fix the comments, the evidence and the ADR §3.3 wording. |
| C-8 | Info | `ResolveLaunchToken` is still exported, with test-only callers. | A static test forbidding non-test callers. Track the deletion (ADR §11 item 5). |

**Local runs (not CI):**
- `-race -tags integration`: casino, full httpserver (474 s) and `cmd/platform-api` all ok.

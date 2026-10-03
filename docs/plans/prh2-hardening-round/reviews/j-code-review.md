# Code review — PRH-2 J (PRH-I4-METRICS-1), 2026-09-28

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** `9c6c705..6d8beea` (6 files).

## Verdict: READY WITH CONDITIONS

The production change is small and correct:
- all 10 decision points record exactly once, after the decision;
- the gauge balances on the panic and error paths;
- `safelyRecord` isolates panics;
- the labels are closed;
- the `TestMain` reader showed no order-dependence (`-race -count=3 -shuffle=on` ok; the full integration suite shuffled under `-race` ok).

| ID | Sev | Finding | Required change |
|---|---|---|---|
| J-1 | Medium | The "failing exporter never affects admission" test is **vacuous**. The global OTel delegate binds to the first `SetMeterProvider` (`TestMain`), so swapping the provider has no effect; a probe recorded delta = 1 into `TestMain`'s reader. The observability failing-exporter and no-op tests also don't exercise the package instruments, and a cited file doesn't exist. | Add a test injection seam, and test nil, panicking and blocking/erroring instruments with identical admission outcomes. Delete or reword the vacuous tests. |
| J-2 | Medium | Coverage gaps. MJ2 (verified admitted double-counted) and MJ3 (`domain_bulkhead` recorded as `verified`) **SURVIVED**; MJ1 was killed. | Assert every reason at both stages: delta 1 at the exercised point, 0 elsewhere. |
| J-3 | Low | `admitted` is counted at both stages, which skews rejection-rate dashboards. This is security's J-L2. | Add a closed `stage` label, and document the semantics. |
| J-4 | Low | The ADR and runbook say "closed" while the reviews are pending. This is security's J-L1. | Reword until the orchestrator closes the item. |
| J-5 | Info | A double release would drive the gauge negative. | Optional `sync.Once`. |

# Code review — PRH-2 A, CAS-REVOKE-CONSUMED-1 (2026-09-28)

**Commit reviewed:** `eebd982`, based on `cabca27`. Reviewer: code-reviewer. Tests ran in a scratch copy against a private DB, which was dropped afterwards.

**Verdict: READY WITH CONDITIONS.** The change is correct:
- The 0108 up body is 0042 plus exactly the four spec lines. The down body is byte-identical to 0042. No other migration uses the number 0108.
- The single caller (`orchestrator.go:544`) is updated.
- No test was loosened.
- Build, vet, the pinned lint and the race suite are clean.
- The QA W1 checklist for A is met.

**Mutants:**
- **Killed:** Q1–Q4 (QA/author), X1b, X2, X3, X6, X7, X8.
- **Survived:** X4 (drop `FOR UPDATE`), X5 (app CAS includes `expired`), X9 (whole-row equality ignores `id`).

| # | Sev | Finding | Required change |
|---|---|---|---|
| A1 | Med (docs / no-fake-completion) | The ADR 0095 §15.1.5 amendment says "C4 is now closed" before the registry and security gates agree. | Align the wording: "IMPLEMENTED; security ACCEPT (`reviews/a-security.md`); CLOSED on merge". The orchestrator updates the registry at merge. |
| A2 | Low | Nothing tests the contract "`revoked=false` means already `expired` or `revoked`". X5 survives, and `expired` is reachable in production (lazy expiry at `launch.go:314`). Under X5 the phase-C transaction would roll back and lose the `casino.launch_failed` audit. | Test `RevokeLaunchSession` on an `expired` and on an already-`revoked` session: each returns `(prior, false, nil)` and leaves the row unchanged. Ideally also a phase-C case asserting the audit row exists. |
| A3 | Low | The doc comment misstates why the CAS can fail. What `FOR UPDATE` actually guarantees is an exact `prior_status`. X4 survives. | Correct the comment. Optionally add a two-connection test that kills X4. |
| A4 | Low | Whole-row equality is exercised only through `consumed_at`; `id` is untested. X9 survives. | Add a matrix cell: consumed→revoked together with a changed `id` is refused. |
| A5 | Low | Refused cells in the matrix assert only `err != nil`. | Assert SQLSTATE P0001, or the trigger's message. |
| A6 | Info | Behaviour change: a missing row now returns `ErrLaunchSessionNotFound` instead of `(false, nil)`. In practice this is unreachable. | Mention it in the ADR amendment. |
| A7 | Info | The evidence file gives the wrong path, `internal/casino/migrations/…`. | Fix the path. |
| A8 | Info | A leftover scratch DB `cas0108_70c7357cf98b448e` remains, with 0 active connections, and its owner is unknown. | The orchestrator asks the author before anyone drops it. |

# PRH-2 K2 — Orchestrator independent mutant re-kill (309f33b)

Local run, not CI. Done on 2026-10-03 against a `git archive` of `prh2-k2-manual-adjustments` at
`309f33b`, using the private DB `orch_k2rk` (since dropped). Each mutant was applied to
`migrations/0113_governed_manual_adjustments.up.sql`, the DB was rebuilt from the mutated file, and
then `-tags integration -count=1 -p 1 -run 'TestK2C1_|TestK2C2_|TestK2C3_|TestK2R1_' ./internal/adjustment/`
was run. The file was restored and `cmp`-verified after the run.

| Mutant | Change | Result | Failing tests |
|---|---|---|---|
| BASE | none | pass (rc=0) | — |
| F2 | drop `r.state = 'executing'` from `ledger_governed_fence_allows` | **KILLED** | `TestK2C2_FenceStateArmAfterExit/governed_posting_after_the_refused_exit` |
| F7 | `→ executing` recount condition replaced by `IF false` | **KILLED** | `TestK2C3_DBRecountOnExecuting`: both subtests (1 of 2 approvals; initiator grant revoked) |
| C1ii | per-entry closed-shape `RAISE` (CG030) removed | **KILLED** | `TestK2C1_…`: probe (over-credit, then refused), wrong wallet, wrong direction |
| C1i | non-executed-exit MA040 `RAISE` removed | **KILLED** | `TestK2C1_…`: correct shape, then refused; wrong wallet. `TestK2R1_…`: refused_insufficient_funds, refused_at_execution |

This confirms the implementer's evidence for F2, F7 and C1ii, and shows that C1i and C1ii are
independently pinned.

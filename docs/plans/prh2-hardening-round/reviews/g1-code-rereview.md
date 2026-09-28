# Code re-review — PRH-2 G1 (2026-09-28)

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** `ac073d8..aa2596f`, covering fix commits `1fa5b77` and `23c642d`. Private DB `cr_g1_rr`, dropped afterwards.

## Verdict: READY WITH CONDITIONS

Every NOT READY item is fixed and verified. The two remaining conditions are not correctness or isolation defects.

| ID | Status | Verification |
|---|---|---|
| F-1 / G1-C1 | FIXED | The reproduction (two tenants, a named staff member in the second) now refuses. After clearing the name, the down completes and `app.tenant_id` is reset. |
| F-2 | FIXED; M1 killed | Cases 5c, 5d and 5e each fail with "expected P0001, got <nil>" under the mutant. |
| F-3 | FIXED; M2 killed | 4 tests fail under the mutant. |
| F-4 | Mostly fixed (C-2) | The audit-log API, noeffect and casino tests call the real code and are not vacuous. The kyc test copies the SQL text, so it is vacuous as a regression guard. |
| F-5 | FIXED (see C-1) | The query uses the same `WithTenant` session with a two-arm filter. The new mutants are killed: M4 (subject rows only) and M5 (page keys only). |
| F-6 to F-9 | FIXED | — |
| G1-C2 / G1-C3 | Reviewed | OK. |

## Conditions

| ID | Sev | Finding | Required change |
|---|---|---|---|
| C-1 | Medium (performance) | `platformActionsChainSourceQuery` has no LIMIT and no key restriction. The tenant arm reads the tenant's whole audit history through `idx_audit_log_tenant_time` and applies the `action LIKE` filter only afterwards, in the heap. It then JSON-decodes every kill-switch row on every page view. | Restrict the query to the chain keys on the current page, e.g. `AND (metadata->>'kill_switch_id' = ANY($2) OR target_id = ANY($2))`. Re-run `ChainSpansPages`. |
| C-2 | Low (test strength) | The kyc "readers unchanged" test copies the SQL text, so it cannot detect a regression in `provider.go`. | Drive the real path: seed a matching subject row in the held-for-review dedup test (`kyc/enforcement_integration_test.go:614-667`) and assert the tenant's held row is still written. Or extract the query into a named helper and test that. |

**Local runs (not CI):**
- `-race -tags integration` for audit, identity, kyc, casino, full httpserver (418 s) and `cmd/platform-api`: all ok.
- lint: 0 issues.

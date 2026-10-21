# lic30 record: AML monitor hardening (ledger-finance M-1 / M-2 / M-3 / LOW-2..4 / I-1, security L-1 / L-2 / L-3 / L-4)

Branch `lic30-aml-monitor-hardening` from integ-c `dcfebdc` (migration head 0189). Software capability only; the only screening provider is the MOCK; no regulatory approval is claimed.

## 0. Which migration

Migrations **0181 and 0189 were edited IN PLACE** (both unreleased); no new file (a 0190 would only have split one function across two files). Any database that applied either earlier must be REBUILT, never rolled back or patched (runbook updated).

## 1. Items

| Item | Change | Label |
|---|---|---|
| M-1 | `aml_screening_parked_overdue` and `aml_screening_gate_overdue` no longer return zeros when no policy gates withdrawals: a person with an unresolved `needs_officer` request is still denied by the always-on hold, so the batch is still scanned. The alert threshold is that of the LAST gating trigger version, else an **engineering fallback of 86400 s (24 h; NOT a legal value; documented in the SQL, the runbook and here)**. | IMPLEMENTED |
| M-2 | A tenant with no in-memory cursor starts its parked-screening cycle at a RANDOM person (first leg: random point to the end), then wraps to the beginning (cursor nil) for the following passes, so restarts more frequent than a cycle no longer starve high person ids. The cursor is NOT persisted (decision: the simplest sound change; a table adds a write on every pass). | IMPLEMENTED (random start); persistence NOT IMPLEMENTED |
| M-3 | Engineering floor on the screening policy re-screen interval, in the route (400), `AuthorScreeningPolicy` (Go) and `aml_screening_policy_author` (0189): **>= 3600 s** (`rescreen_interval_below_floor`) and, if the jurisdiction has a gating trigger version, **>= 2 x its `gate_alert_after_seconds`** (`rescreen_interval_below_alert_threshold`, database only: Go cannot see the trigger policy). Justification: below an hour every clear expires within the hour (all withdrawals freeze behind the gate) and `aml_screening_enqueue_due` floods; below twice the alert threshold, clears expire before the trapped-funds alert can fire. The 1-year cap stays. A SAFETY GUARD, not a legal value. The unrouted service setter keeps accepting any interval (existing tests use seconds). | IMPLEMENTED |
| LOW-2 / L-2 (effective time) | `effective_from` of the screening policy author is bounded to [now - 2 min, now + 30 d] in Go and in 0189 (`policy_effective_from_out_of_range`), like the trigger policy. | IMPLEMENTED |
| LOW-2 (strictness) | The tighten-only comparison now checks the new version against BOTH the latest version (in force or pending) and the version IN FORCE: lists, interval and grace must be at least as strict as each; the effective time must not be earlier than the version in force (when none is in force yet, not earlier than the latest). A version at least as strict as both may therefore take effect earlier than a pending, future-dated one. | IMPLEMENTED |
| L-1 | `aml_screening_policy_author` takes `pg_advisory_xact_lock(hashtextextended(tenant || jurisdiction, 0))` before reading the previous version, so two concurrent tightenings cannot each compare with the same predecessor. Tested deterministically: a test holds the same lock in another transaction and the author blocks until it is released. | IMPLEMENTED |
| L-3 | DEFERRED (residual below). | NOT IMPLEMENTED |
| LOW-3 / L-4 | In-place edits recorded in the runbook (rebuild any DB that applied 0181 / 0189 earlier). | IMPLEMENTED (documentation) |
| LOW-4 | `aml.ScreeningParkedOverdue` (loops every batch, unbounded) moved from the production file to `export_test.go`: it is test-only. A `failed_terminal` request now counts in the officer bucket (it needs an officer). | IMPLEMENTED |
| I-1 | The alert value is documented as a PER-BATCH count (SQL header, runbook, this record). | IMPLEMENTED (documentation) |
| Security tests | Tenant-scope officer: 403 with `platform_officer_required` and nothing written (integration: policy count unchanged is asserted in `TestLic29_PolicyAuthor_FirstVersionAuthorityAndProof` for the tenant-scope world; HTTP: status and code in `TestLic30_PolicyRoute_TenantScopeOfficerIs403WithTheClosedCode`). An officer of tenant B posting tenant A's id: 404 on both policy routes, service never reached. `policy_loosening_refused` for each of the four dimensions (`TestLic29_PolicyAuthor_TightensButNeverLoosens`, retained). No refusal code outside the AML layer (a static test: only `aml_routes.go` and `internal/aml` read them). | IMPLEMENTED |

## 2. Pin and test changes (declared; nothing weakened)

- `TestLic29_PolicyAuthor_TightensButNeverLoosens`: the "earlier effective time" case used `now - 3 h`, which the new bound refuses with another code; it now uses `now - 90 s` against a version that took effect 30 s ago (added to the test), so the case still proves the dimension. The three other dimensions and every other assertion are unchanged.
- `TestLic29_ParkedScan_NoGatingPolicyAndBadArguments`: expected `Scanned == 0` without a gating policy; the batch is now scanned (M-1), so it expects `Scanned == 1` and every bucket 0.
- `TestLic29_SweeperWalksTheKeysetCycleAndAlertsApart`: expected the first read to start at nil; it now expects the injected random start (M-2) and, after a completed cycle, nil. The seam is `testParkedStart`.
- `policyMux` / the lic30 route tests give the handler a logger (an error path logs).
- New category of test files: `internal/aml/screening_lic30_integration_test.go`, `internal/httpserver/aml_screening_policy_lic30_test.go`; `export_test.go` gains `ScreeningParkedOverdue` and `AuthorScreeningPolicySkippingGoChecks` (so the database bounds are proven without the Go pre-checks).

## 3. Mutation checks

Each applied to the live function or the source, killed by the named test, then restored: the M-1 fallback in `aml_screening_parked_overdue` and in `aml_screening_gate_overdue` (`TestLic30_M1_...`), the failed_terminal bucket, the 3600 floor, the 2 x threshold, the effective-time bound, the advisory lock (`TestLic30_L1_...`), the in-force comparison and the latest comparison (`TestLic30_Low2_...`), and the random start in the sweeper.

## 4. Backlog needing an owner / architect decision BEFORE go-live

**A four-eyes loosening / revert path for the screening policy.** The HTTP route is tighten-only (lic29). Combined with the new floor this still leaves a gap: a value that is valid but too SHORT (for example exactly 3600 s where 86400 s was meant) cannot be reverted over HTTP, and a legitimate loosening (a list removed on a regulator's instruction, a longer interval) has no governed route at all; today both are database-operator actions through the unrouted setter. Before go-live the owner and the architect must decide: a propose / approve (two Persons) path for loosening and reverting, its step-up class (it belongs in the signer's step-up registry as critical when built), and whether the unrouted setter stays reachable by any operator procedure.

## 5. Residuals

1. **L-3 (proof operation reuse), DEFERRED.** `aml_screening_policy_author` (0189) and the unrouted `aml_screening_policy_set` (0154) verify the same proof operation `aml_screening_policy:set` bound to the same arguments, so a proof signed for the routed author could in principle be spent on the setter, which can loosen. Separating them needs a new operation name and a replacement of the proof verifier's closed operation set (migration of the released verifier). The attack needs the runtime credential AND the proof issuer key (the proof is signed server-side and is single-use, 60-second, actor- and argument-bound), i.e. it is not available to a holder of an ordinary session. Decision recorded: defer to the loosening path work above, which needs its own operation anyway.
2. The sweeper cursor is not persisted (M-2): after a restart only the random start protects coverage; coverage of a given person is probabilistic across restarts, deterministic within one uninterrupted cycle.
3. The 24 h fallback threshold (M-1) is an engineering value.
4. A dead worker AND a dead sweeper remains invisible; `ALERT-DELIVERY-1` stays open.
5. The `rescreen_interval_below_alert_threshold` rule uses the LATEST gating trigger version of the jurisdiction (in force or pending); a later, longer alert threshold does not re-check the policy.

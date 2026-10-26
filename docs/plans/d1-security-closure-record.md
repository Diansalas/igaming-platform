# d1 record: security closure before merge (D7 break-glass setter, D4 trigger-policy loosening, D10 freshness and list binding)

Branch `d1-security-closure` from `9c50402`. Migration head was 0190; this slice adds **0191** (D7 + D4) and **0192** (D10). Software capability only; the only screening
provider is the MOCK; no regulatory approval is claimed. Screening loosening stays DISABLED and INERT: no capability row ships, no gate-enabling trigger policy was written, nothing is
production-enabled. Review status: **no `security` or `ledger-finance` review has been done; this record is not a review and the slice is not claimed review-complete.**

## 0. Labels

| Deliverable | Label |
|---|---|
| D7: runtime `EXECUTE` on `aml_screening_policy_set` revoked (migration 0191), init script re-asserts it, fixtures moved to the owner path | IMPLEMENTED (software); owner decision on the operator procedure for the break-glass: OPEN |
| D7(c): catalogue test pinning every runtime-executable writer of `aml_screening_policies` (and of `aml_screening_trigger_policies`) | IMPLEMENTED |
| D7(d): a runtime actor cannot remove or weaken the sanctions list by any path | IMPLEMENTED (tested per path, section 3) |
| D4: trigger-policy route tighten-only in the DATABASE (`aml_screening_trigger_policy_author`), 0181 setter owner-only | IMPLEMENTED |
| D4: governed kind `aml_screening_trigger_policy_loosen` (0190 pattern), same inert capability row | IMPLEMENTED, INERT (capability not configured anywhere) |
| D4: Back Office FILING form for the new kind | NOT IMPLEMENTED (API only, like the other two kinds); generic list/detail pages show it; the trigger form states it only tightens |
| D10: provenance on every clear, `LEAST(recorded, current)` freshness, list superset + no-gap rule, legacy provenance untrusted, scheduler uses the same predicate | IMPLEMENTED |
| D10: a provider attestation of the lists actually screened | PROVIDER DEPENDENT (MOCK only); residual in section 6 |
| Any cooling period, interval, grace, list rule, threshold, sanctions false-positive policy or withdrawal release policy | NOT CHOSEN (owner decisions, section 7) |
| Real KYC/AML vendor, real PSP | NOT USED (all providers MOCK) |

## 1. Commits

| SHA | Content |
|---|---|
| `de54493` | D7: migration 0191 part 1, init script, owner-path fixtures, tests, runbook |
| `49bc3d2` | D4: migration 0191 part 2, Go kind, closed codes, OpenAPI, Back Office wording, tests, runbooks |
| `3201446` | D7: executor refuses a sanctions-list removal even for a forged executed request (test) |
| `1ce4435` | D10: migration 0192, worker policy argument, tests, runbook |
| (next) | e2e spec 10 (g) calls the 13-argument finish |
| (last) | this record |

## 2. What was built

### D7 (migration 0191, part 1)

`REVOKE ALL ON FUNCTION aml_screening_policy_set(...) FROM PUBLIC, igaming_runtime`. The function body is untouched: an owner call still runs `aml_authorize` (in-force officer grant, owner-scope
quorum, signed single-use proof of operation `aml_screening_policy:set`), so the break-glass keeps its officer controls; only the database role that may CALL it changed. `deploy/init-app-role.sql` no
longer grants it and re-asserts the revoke on every run. The proof-operation reuse residual of lic30 (L-3: a proof signed for the routed author could be spent on the setter) is closed as a consequence.

**Caller inventory (every caller found by grep over Go, TS, SQL, shell and docs):**

| Caller | Kind | Change |
|---|---|---|
| `aml.Service.SetScreeningPolicy` | the only Go method; no production caller; not in `AMLOfficerAPI` (pin `TestSetScreeningPolicy_IsNotRoutedYet_SR7_3` untouched and green) | kept as the owner-path method; doc updated |
| `backoffice/e2e/tools/aml-fixture/main.go` (runtime DB role) | e2e fixture tool | now calls `AuthorScreeningPolicy`, the SAME tighten-only method the HTTP route uses (it only ever writes a first version); no new runtime path |
| 20 call sites in `internal/aml/*_test.go` (adversarial, sanctions_potential, screening_integration x9, lic23, lic29 x2, lic30, trigger x2) | integration tests over the RUNTIME pool | actor changed to the owner pool through `amlfix.OwnerService` / `ownerSvc(t)`; assertions unchanged |
| `internal/launchgov/readiness_aml_integration_test.go` | integration test | owner pool service |
| `migration_0154/0155_integration_test.go` (3 sites) | scratch databases over the OWNER pool already | unchanged |
| `internal/aml/catalog_integration_test.go` | catalogue test that listed the setter as runtime-executable | moved to `definerNoRuntime` (intentional pin change) |
| `internal/aml/screening_test.go` (validation unit test over a fake pool) | unit | unchanged |
| HTTP layer, cmd, other packages | none | none |

`amlfix.OwnerService(t, ownerPool)` refuses a pool whose session user is the runtime role, so a fixture cannot quietly fall back to a runtime path.

### D4 (migration 0191, part 2)

Loosening of the trigger policy, from the schema of `aml_screening_trigger_policies`, against the latest version AND the version in force:

| Dimension | Looser | Stricter |
|---|---|---|
| `events` | a previous event is missing | an event is added |
| `gate_withdrawal` | true to false (turning an existing gate OFF) | false to true (guarded by heartbeat + environment anchor, unchanged) |
| `gate_alert_after_seconds`, both versions gate | longer | shorter |
| `effective_from` | earlier than the version in force (route only; same rule as 0189) | |

`legal_review_reference` and `reason_code` are provenance. One definition: `screening_trigger_cmp()` (truth-table test).

1. `aml_screening_trigger_policy_author` (new, runtime-executable): the 0181 setter body (same proof operation and digest, same quorum and gate guards) + advisory lock + the rule above; a loosening is `AT003` with HINT `trigger_policy_loosening_refused`, nothing written. The Go method `SetScreeningTriggerPolicy` (the route) calls it.
2. `aml_screening_trigger_policy_set` (0181): OWNER-ONLY. Go: `OwnerSetScreeningTriggerPolicy`, not in `AMLOfficerAPI`, pinned never referenced by production code outside `internal/aml` (`TestOwnerSetScreeningTriggerPolicy_IsNeverRouted_D1`).
3. Kind `aml_screening_trigger_policy_loosen`: classifier wrapper widened in place (the 0190 wrapper body + one branch; the 0180 base untouched), `screening_trigger_loosen_assert` (validator), `screening_trigger_loosen_apply` (executor), table guard on `aml_screening_trigger_policies`
   (`change_request_id`, scope `platform` coupled to it by CHECK, unique index; an INSERT is admitted only with an executed request of THIS transaction whose payload equals the row, reason code fixed `governed_loosening`, effective time bounded; and never `gate_withdrawal = true` unless the previous latest version already gated),
   approval-trigger extended to the new kind, `screening_loosening_reconcile()` extended. Rules: requester != approvers, 2 distinct Persons, officer grant + owner-scope quorum for requester / counted approvers / executor, signed proofs, approvals <= 24 h old, request lifetime 72 h, advisory locks (the 0190 capability key and a per-jurisdiction key shared with the route author), FORCE RLS tables untouched, audit rows `licensing_change.*` + `aml.screening_trigger_policy_loosened` with previous and in-force version.
   Gated by the SAME capability row (`aml_screening_loosening_settings`): none exists, so `loosening_not_configured`. Own-licence tenants stay out of scope (`loosening_not_supported_for_tenant_licensing_model`).
4. A loosening can never enable a gate: `trigger_gate_enable_refused` in the validator and again in the table guard. A version that KEEPS a gate (longer threshold) is refused where the environment anchor is production or unrecorded (`gate_environment_refused`); switching a gate OFF only releases and works everywhere.
   A longer threshold must leave the screening policy interval at least twice the threshold (`rescreen_interval_below_alert_threshold`, the lic30 M-3 rule, which previously was not re-checked when the threshold was lengthened).
5. Closed codes added (Go table, OpenAPI enum, Back Office wording, drift pins): `trigger_policy_loosening_refused`, `no_trigger_policy_to_loosen`, `trigger_gate_enable_refused`. OpenAPI kind enum and payload docs, trigger-policy route description (tighten-only), runbooks.
6. The production guard of the gate is not redefined (`aml_screening_trigger_policy_set`, `screening_worker_available`, `aml_screening_gate_environment_ok` bodies untouched; a static test pins it).

### D10 (migration 0192)

The defect, reproduced BEFORE the fix on the 0191 schema and code (`go test -tags integration ./internal/aml -run 'TestD10_...'`, section 3): lengthening the interval revived an expired clear and released a held withdrawal; a pep-only clear still cleared once sanctions was in force; a list removed and re-added left old clears valid; an expired auto-reviewed re-flag / lifted officer match was revived.

1. Provenance columns: `aml_screening_requests.screened_policy_id / screened_lists / screened_interval_seconds` (all-or-none CHECK, FK to the policy version, a NOT VALID CHECK that every request ending `done` or `needs_officer` carries it), `aml_screening_results.screened_policy_id / screened_interval_seconds` (pair CHECK, FK; `lists` already existed).
2. `aml_screening_request_finish` takes the policy version as its LAST argument; the lists and the interval are read from THAT policy row (tenant, the request's jurisdiction, in effect), never from the caller. The 12-argument function is dropped. `aml_screening_record` stamps the policy in force (the request's jurisdiction if the key resolves a request, else the tenant policy with the shortest interval) and its interval; `lists` stay the officer's.
3. `aml_screening_covers(at, recorded interval, recorded lists, current interval, current lists, gap since)`: provenance present AND recorded lists superset of the current lists AND no version since the screening lacked a currently required list AND `at > now - LEAST(recorded, current)`. Owner-only.
4. `aml_screening_gate_denies` applies it to the four routes (worker clear, auto-reviewed re-flag, officer clear, officer match with a lifted hold). `aml_screening_enqueue_due` applies the same predicate (persons not covered are DUE) and its key now names the policy version.
5. Time basis: the existing database clock (`completed_at` for requests, `recorded_at` for results), not the provider-supplied `screened_at` (forgeable by the caller); this is a deliberate departure from the literal wording `screened_at + ...` in the brief.
6. Unchanged: unresolved matches, sanctions holds, officer holds, kill switch, closed accounts, the backfill grace and first-gating baseline (0190), `aml_screening_gate_overdue` / `parked_overdue` bodies. No release path added (every conjunct is stricter than the old test).

**Effect on existing dev data (stated plainly).** Every clear that exists before 0192 has no provenance and stops counting. Under a gating trigger policy those persons are denied at the gate until the scheduler queues a re-screen and the worker records a clear with provenance
(`EnqueueBatch` persons per pass). Nothing is rewritten or deleted. No production database has a gate (the environment anchor forbids enabling it), so there is no production effect. `TestD10_Upgrade_LegacyClearsAreNotTrusted_...` proves it on a database upgraded from 0191 with a legacy clear.

## 3. Verification (private databases only: `d1sec`, `d1mut`, `d1base`; scratch databases by the tests)

Environment: PostgreSQL 16 local, runtime role `igaming_runtime` (non-superuser, non-BYPASSRLS, asserted by the test worlds), private databases only (`d1sec` for the integration tests, `d1mut` for mutations, `d1base` built from migrations <= 0190 for the upgrade check, `d1e2e` for Playwright; all dropped at the end), scratch databases created by the tests themselves. Commands use the `priv_db.sh` / `priv_test.sh` helpers (rebuild from the worktree's migrations; `-tags integration -count=1 -p 1`).

| Command | Result |
|---|---|
| `go test -count=1 ./...` (unit, no integration tag) | 79 packages ok, exit 0 |
| `go vet ./...`, `go vet -tags integration ./...`; `gofmt -l` on all Go files | clean, no output |
| `internal/aml` (full package, integration) at the final tree | ok, 270.1 s |
| `internal/licensingchange` (full package, integration; includes migration 0190 and 0191 up/down/up byte for byte and the `LC095` refusal) | ok, 110.5 s |
| `internal/httpserver -run 'Screening\|AML\|Licensing\|Staff\|StepUp\|OpenAPI\|Route\|Mutation\|D1\|C1\|C2\|Owner'` | ok, 430.1 s (includes the new route test, the governed-kind HTTP test, the OpenAPI contract tests and the pins) |
| `cmd/migrate`, `internal/db` | ok 1.7 s, ok 36.6 s |
| `internal/payments -timeout 50m -run 'Lic26\|Lic29\|AML\|Aml\|Hold\|Screening\|Gate\|Sweep\|0115\|Parked'` (the tests that touch the screening gate, the sweeper and the parked monitor) | ok, 725 s, 193 top-level tests passed. The WHOLE payments package exceeds the default 10 minute test timeout (1215 tests; it timed out in the unrelated `TestEchoConformance_MockSupported`), so the full package was NOT run to completion. |
| `internal/withdrawal`, `internal/withdrawalpolicy`, `internal/withdrawalgate` (no test files), `internal/alerting` (migration 0181 round trip), `internal/launchgov` | ok 52.8 s, ok 1.1 s, -, ok 232.7 s, ok 59.2 s |
| `cd backoffice && npx vitest run` (all 82 files) / `npx vitest run src/features/licensing src/features/aml` / `npx tsc --noEmit` | 1084 passed / 16 files, 280 passed / clean |
| `cd b2c && npx vitest run src/api/openapiContract.test.ts`; `python3 yaml.safe_load` of `docs/api/openapi/platform-api.yaml` | 21 passed; ok |
| Playwright `E2E_SPEC=12-screening-policy` via `backoffice/e2e/run.sh`, fresh private database, real `cmd/platform-api` | 7 passed (4.9 min) |
| Playwright `E2E_SPEC=10-aml-screening`, fresh private database | first run 7 passed, 1 FAILED ((g): the spec called the dropped 12-argument finish function, `42883`); spec fixed (pin 19); second run on a fresh database: **8 passed (12.1 min)** |
| Playwright specs 01-09, 11, 13 | NOT re-run (they do not touch the changed functions; the kind filter gained one option, covered by vitest) |
| Migrations: clean setup (`priv_db.sh` from scratch, repeatedly); upgrade from a database at 0190 (`d1base` -> `migrate up` applied [191 192]); `migrate down -steps 2` then `up` | schema fingerprint (all function definitions with ACL and comment, constraints, indexes, columns, kind registry; 24 000+ lines) of "upgrade from 0190" == "clean setup" == "up again after down 2"; "down 2" == the fingerprint of a database built from migrations <= 0190, byte for byte |
| Migration tests in Go: `TestMigration0191_UpDownUp_Snapshot_DownRefusedWithData`, `TestMigration0192_UpDownUp_WholeSchema_DownRefusedOnceProvenanceExists`, `TestD10_Upgrade_LegacyClearsAreNotTrusted_...` (database at 0191 with a legacy clear, upgraded to 0192) | passed inside the package runs above |

**Failing-before proof (D10), run on the 0191 schema and code before the fix was written:**
```
TestD10_LengtheningTheInterval_...  D10 DEFECT: lengthening the re-screen interval revived an EXPIRED clear and released a held withdrawal without any new screening
TestD10_ListSuperset_...            D10 DEFECT: clean: a pep-only clear still clears the person once sanctions is in force
TestD10_ListRemovedAndReAdded_...   D10: a list removed and re-added invalidates the clear taken before the removal, whatever lists that clear recorded
TestD10_Reflag_...                  D10: lengthening the interval revived an expired auto-reviewed re-flag / lifted officer match
(TestD10_ShorteningTheInterval_StillTightensAtOnce passed before too: shortening already tightened; kept as a regression)
```
All pass after the fix (listed in the aml package run). Held-withdrawal proxy: `aml_hold_active` (the only hold read of the payout/withdrawal gate); the payments sweeper and claim tests above are unchanged and green.

**D7 proofs on a clean migrated database (`TestD7_*`, `TestCatalog_*`):** (a) `has_function_privilege('igaming_runtime', setter, 'EXECUTE')` false, `public` false, no ACL entry for the runtime role or PUBLIC, the owner can execute; (b) the setter as the runtime role is `42501` in raw SQL and through the Go service over the runtime pool, nothing written; (c) the live catalogue enumeration of every function (any schema, transitive over call chains, dynamic SQL included) that can write `aml_screening_policies` and is executable by the runtime role or PUBLIC is exactly `{aml_screening_policy_author, screening_policy_loosen_apply}`, and for `aml_screening_trigger_policies` exactly `{aml_screening_trigger_policy_author, screening_trigger_loosen_apply}` (the detector is shown non-vacuous: the owner-only setter IS a writer); (d) for the sanctions list: the tighten route refuses removal (`policy_loosening_refused`), the setter is `42501`, direct INSERT/UPDATE/DELETE/SELECT on the table are `42501`, the executor refuses a request that was not executed in this transaction, the governed kind refuses removal at filing (existing `TestC4_SanctionsListRemoval_...`) and at execution even for a forged executed request with forged approvals (`TestD7_SanctionsRemoval_IsRefusedByTheExecutor...`); the init script does not re-grant (run twice on a throwaway database).

## 4. Intentional pin changes (nothing weakened; each was necessary because behaviour changed on purpose)

**D7**
1. `internal/aml/catalog_integration_test.go` `TestCatalog_AMLFunctions_DefinerSearchPathAndPrivileges`: `aml_screening_policy_set` moves from `apiRuntime` (SECURITY DEFINER AND runtime-executable) to `definerNoRuntime` (SECURITY DEFINER and NOT runtime-executable). The test body is unchanged; the pin is stricter.
2. Fixtures that call the setter now run as the table owner (`amlfix.OwnerService`): 20 call sites in `internal/aml/*_test.go` and one in `internal/launchgov/readiness_aml_integration_test.go`. Only the ACTOR changed; every assertion is the same.
3. `backoffice/e2e/tools/aml-fixture/main.go` uses `AuthorScreeningPolicy` instead of `SetScreeningPolicy` (the tool only writes a first version; the route's floors apply and the fixture values satisfy them).
4. `deploy/init-app-role.sql`: the setter leaves the runtime grant list and an explicit REVOKE is added. New test `TestD7_InitAppRoleSQL_DoesNotRegrantTheSetter` pins it.

**D4**
5. `internal/licensingchange/kinds_test.go` `TestKinds_MatchTheMigrationRegistry`: also reads 0191; 15 -> 16 kinds and 16 seeds of `2, ARRAY['platform_admin']`.
6. `internal/aml/refusal_test.go` `TestLic29_RefusalCodes_MatchTheMigrations`: also reads 0191 (the three new codes are raised by literal HINTs there). Body unchanged.
7. `internal/aml/static_test.go` `TestStatic_AMClassOnlyInAMLMigrations`: 0191 and 0192 added to the AML migration list (the author raises `AM002`; the re-issued record function carries the AM codes).
8. `internal/aml/catalog_integration_test.go`: `aml_screening_trigger_policy_set` moves to `definerNoRuntime`; `aml_screening_trigger_policy_author` is added to `apiRuntime`.
9. Fixtures that LOOSEN the trigger policy use the owner path (`setTriggerOwner`, `OwnerSetScreeningTriggerPolicy`): `TestLic18_UnresolvedMatchHoldsEvenAfterTheGateIsSwitchedOff`, `TestLic23_N1_GateOffVersionAllowedAfterTheJurisdictionLeavesTheTenantsScope`,
   `TestLic26_F1_...`, `TestLic30_M1_...`, `TestTriggerPolicy_Authoring_IsGovernedLikeTheScreeningPolicy`. Assertions are kept; in the last two ONE assertion was ADDED each (the route refuses the same version with the closed code). In `TestLic23_N1_...` the "gate-OFF version allowed" claim is now asserted for the owner path (N-1 relief lives in the setter and in the governed kind) and the route's refusal is asserted first.
10. Closed refusal codes grow by three (`trigger_policy_loosening_refused`, `no_trigger_policy_to_loosen`, `trigger_gate_enable_refused`): Go table, OpenAPI enum `AMLScreeningRefusalCode`, `screeningErrors.ts` wording, and the drift pins (`screeningRefusal.test.ts`, `refusal_test.go`) now cover them; the pins themselves are unchanged. OpenAPI kind enum +1 (`aml_screening_trigger_policy_loosen`).
11. Back Office: new label `Screening trigger policy loosening (AML)`, and a new note `Tighten only` in the trigger policy dialog (`TRIGGER_TIGHTEN_ONLY_NOTE`); new vitest assertions for both. UI-visible wording changed, so Playwright specs 10 and 12 were re-run (section 3).
12. `scratchWorld` in `screening_loosening_integration_test.go` gained a `through` parameter via `scratchWorldThrough` (the old name keeps 190). No behaviour change.
13. New pin `TestOwnerSetScreeningTriggerPolicy_IsNeverRouted_D1` (HTTP package) next to the untouched SR7-3 pin.

**D10**
14. Two tests call the finish function in raw SQL; they now pass the policy version (`TestLic18_...` provider-reference case in `screening_lic18_integration_test.go`, `TestQueue_ClaimTokenCAS_...` in `screening_trigger_integration_test.go`). Assertions unchanged.
15. `TestLic27b_S4_ConfirmedRequestMustNameTheSanctionsList`: its fixture UPDATE moves a request to `needs_officer`; since 0192 such a request must carry provenance (new CHECK), so the fixture states it. Without that the PEP-only leg would have failed on the NEW check and stopped proving the sanctions rule. Both assertions are unchanged.
16. `deploy/init-app-role.sql`: the finish function signature (13 arguments).
17. The scheduled idempotency key is now `sched:<person>:<bucket>:<policy id>` (was `sched:<person>:<bucket>`); no test asserted the shape (only the reserved prefix `sched:` is pinned).
18. Behavioural change on purpose: a clear without provenance (every row written before 0192) no longer counts (section 2, "Effect on existing dev data").
19. `backoffice/e2e/specs/10-aml-screening.spec.ts` (g): its two raw SQL calls of `aml_screening_request_finish` as the runtime role gain the 13th argument (`NULL`); the assertions (cross-tenant `AT002`, no claim token applies nothing) are unchanged. Found by the first Playwright run (the 12-argument function no longer exists: `42883`), fixed, and the spec re-run in full.

## 5. Mutation checks (each applied to the migration text, DB rebuilt, named tests run, file restored; killed = at least one named test failed)

**D4 (migration 0191, `internal/aml` and `internal/licensingchange` tests)** - 19 mutations, all killed. (The first run of the table-guard mutation printed `SURVIVED` because the database rebuild failed with a leftover connection ("permission denied to terminate process" on the private `d1mut` database); it was re-run on a rebuilt database and is KILLED by `TestD4_ExecutorAndGuard_NeedAnExecutedRequest_AndNeverEnableAGate`.)

| Mutation | Result | Killed by |
|---|---|---|
| author: strictness check removed | KILLED | `TestD4_TriggerAuthor_RefusesEveryLooseningDimension_WithTheClosedCode_AndWritesNothing` |
| author: effective-time dimension removed | KILLED | `TestD4_TriggerAuthor_RefusesEveryLooseningDimension_WithTheClosedCode_AndWritesNothing` |
| cmp: gate on->off not looser | KILLED | `TestD4_TriggerAuthor_RefusesEveryLooseningDimension_WithTheClosedCode_AndWritesNothing`, `TestD4_StrictnessComparison_TruthTable` |
| cmp: longer threshold not looser | KILLED | `TestD4_TriggerAuthor_RefusesEveryLooseningDimension_WithTheClosedCode_AndWritesNothing`, `TestD4_StrictnessComparison_TruthTable` |
| cmp: event removal not looser | KILLED | `TestD4_TriggerAuthor_RefusesEveryLooseningDimension_WithTheClosedCode_AndWritesNothing`, `TestD4_StrictnessComparison_TruthTable` |
| setter stays runtime-executable (trigger) | KILLED | `TestCatalog_AMLFunctions_DefinerSearchPathAndPrivileges`, `TestD4_TriggerSetter_IsOwnerOnly_RuntimeIs42501_OwnerPathStillWorks`, `TestD4_OnlyAllowListedRuntimeFunctionsWriteTheTriggerPolicies` |
| validator: capability configured check removed | KILLED | `TestD4_Unconfigured_AndDisabled_AreRefused_AndNothingIsWritten` |
| validator: never-enable-a-gate check removed | KILLED | `TestD4_Dimensions_AndTheClosedRefusals`, `TestD4_ProductionAnchor_GateOffAllowedEverywhere_KeepingAGateNeedsAnAllowedEnvironment` |
| validator: environment guard removed | KILLED | `TestD4_ProductionAnchor_GateOffAllowedEverywhere_KeepingAGateNeedsAnAllowedEnvironment`, `TestD4_Migration_DoesNotRedefineTheGateOrItsProductionGuard_AndTheAuthorKeepsIt` |
| validator: interval>=2*threshold removed | KILLED | `TestD4_Dimensions_AndTheClosedRefusals` |
| validator: not_a_loosening check removed | KILLED | `TestD4_Dimensions_AndTheClosedRefusals` |
| validator: mixes-tightening check removed | KILLED | `TestD4_Dimensions_AndTheClosedRefusals` |
| validator: cooling period check removed | KILLED | `TestD4_CoolingPeriod_IsEnforcedFromFiling_AndDelaysTheEffectiveTime` |
| table guard: never-enable-a-gate removed | KILLED | `TestD4_ExecutorAndGuard_NeedAnExecutedRequest_AndNeverEnableAGate` |
| approval trigger: new kind not officer-checked | KILLED | `TestD4_OfficerControls_PlainPlatformAdminsCannotLoosenTheTriggerPolicy` |
| executor: approvers_assert removed | KILLED | `TestD4_OfficerControls_ApproverLosingStandingBeforeExecutionDoesNotCount` |
| classifier: new kind not widening | KILLED | `TestD4_GateOff_EndToEnd_TwoPersons_SameTransaction_Audited`, `TestD4_Dimensions_AndTheClosedRefusals` |
| reconcile: trigger orphan loop removed | KILLED | `TestD4_Reconciliation_IncludesTheTriggerKind` |
| table guard: executed-request check removed (re-run on a rebuilt database) | KILLED | `TestD4_ExecutorAndGuard_NeedAnExecutedRequest_AndNeverEnableAGate` |

**D10 (migration 0192) and D7 (0190 sanctions check)** - three first-run survivors, all analysed:

| Mutation | Result | Killed by |
|---|---|---|
| covers: current interval only (recorded interval ignored) | KILLED | `TestD10_LengtheningTheInterval_NeverRevivesAnExpiredClear_NorReleasesAHeldWithdrawal`, `TestD10_CoveragePredicate_Matrix` |
| covers: recorded interval only (shortening ignored) | KILLED | `TestD10_ShorteningTheInterval_StillTightensAtOnce`, `TestD10_CoveragePredicate_Matrix` |
| covers: lists superset check removed | KILLED | `TestD10_CoveragePredicate_Matrix`, `TestD10_ListSuperset_PepOnlyClearNoLongerClearsOnceSanctionsIsInForce`, `TestD10_OfficerResults_RecordProvenance_AndCoverOnlyTheListsInForce` |
| covers: gap rule removed | KILLED | `TestD10_CoveragePredicate_Matrix`, `TestD10_ListRemovedAndReAdded_InvalidatesOldClears` |
| covers: missing provenance trusted | KILLED | `TestD10_CoveragePredicate_Matrix`, `TestD10_Upgrade_LegacyClearsAreNotTrusted_SubjectsBecomeDue_AndNothingIsRewritten` |
| covers: missing provenance trusted (interval defaults to current) | SURVIVED |  |
| scheduler: old due rule (last screening older than the interval) | KILLED | `TestD10_LengtheningTheInterval_NeverRevivesAnExpiredClear_NorReleasesAHeldWithdrawal`, `TestD10_ListSuperset_PepOnlyClearNoLongerClearsOnceSanctionsIsInForce`, `TestD10_ListRemovedAndReAdded_InvalidatesOldClears` |
| scheduler: key without the policy version | SURVIVED |  |
| finish: policy not validated against tenant/jurisdiction | KILLED | `TestD10_Finish_RecordsProvenanceFromThePolicyRow_RefusesBadPolicies_IdempotentAndConcurrent` |
| finish: policy id may be omitted | SURVIVED |  |
| record: no provenance stamped | KILLED | `TestD10_OfficerResults_RecordProvenance_AndCoverOnlyTheListsInForce` |
| gate: auto-reviewed re-flag route not bound | KILLED | `TestD10_Reflag_AutoReviewedReflagAndLiftedMatch_AreBoundLikeAClear` |
| gate: lifted-hold route not bound | KILLED | `TestD10_OfficerResults_RecordProvenance_AndCoverOnlyTheListsInForce` |
| scheduler: key without the policy version (existing person, bucketed key) | KILLED | `TestD10_ListSuperset_PepOnlyClearNoLongerClearsOnceSanctionsIsInForce` |
| covers: recorded-interval NOT NULL conjunct removed (legacy RESULTS trusted) | KILLED | `TestD10_CoveragePredicate_Matrix` |
| 0190 validator: sanctions removal check removed | KILLED | `TestD7_SanctionsRemoval_IsRefusedByTheExecutorEvenForAForgedExecutedRequest`, `TestC4_SanctionsListRemoval_IsRefusedEvenWithEverythingElseInOrder` |

Analysis of the survivors: (a) `covers: missing provenance trusted (interval defaults to current)` is an EQUIVALENT mutant (the `IS NOT NULL` conjuncts it left in place still reject a missing provenance); the valid variant (removing the interval conjunct) is the `recorded-interval NOT NULL conjunct removed` row, KILLED. (b) `scheduler: key without the policy version` first survived because the test's persons were screened by registration-keyed requests, so no scheduled key existed in the bucket; `TestD10_ListSuperset_...` was strengthened with an EXISTING person (screened by the bucketed scheduled backfill) and the mutation is now KILLED (second row of that name). (c) `finish: policy id may be omitted` is EQUIVALENT: with the explicit NULL check removed, `WHERE sp.id = NULL` finds no row and the same `AT001` is raised.

## 6. Residual risks (honest)

1. **Strictness of the TRIGGER policy is enforced in the author function, not by the table guard for `platform_acting` rows.** The owner-only `_set` must be able to write a loosening (the documented break-glass), so the table cannot refuse it. A future migration that re-grants `aml_screening_trigger_policy_set` (or adds a runtime definer that writes the table) would reopen the hole; the catalogue tests `TestD4_OnlyAllowListedRuntimeFunctionsWriteTheTriggerPolicies` and `TestD7_OnlyAllowListedRuntimeFunctionsWriteTheScreeningPolicies` (live catalogue, transitive over call chains and dynamic SQL) and the init-script test are the controls.
2. **The owner path is real.** Whoever holds the table-owner role can call both setters (with officer grant, quorum and a fresh signed proof) and can obviously do anything else. It is documented as an owner-recorded decision with a reconciliation query; nothing technical stops an owner. The 0191 down re-grants both setters to the runtime role (dev/staging only; it is the pre-0191 state).
3. **Provider attestation.** For a worker clear the database records the lists and interval of the policy version the worker NAMES (validated: this tenant, the request's jurisdiction, in effect) but cannot prove the provider was called with those lists. The worker API is runtime-callable (as the clear itself always was); the heartbeat and the environment anchor remain the controls. With a real provider this needs a signed vendor attestation of the lists actually screened (PROVIDER DEPENDENT). The MOCK answers per list it is given.
4. **Officer results.** `lists` are the officer's statement. The policy and interval are stamped by the database: the request's jurisdiction when the key resolves a request, else the tenant's policy with the SHORTEST interval (conservative, never looser); no policy in force at record time means no stamp and the result never counts as a fresh clear. A result recorded under a key that resolves nothing is not tied to a jurisdiction.
5. **The no-gap rule is conservative.** Any version with `effective_from <= now` that lacked a currently required list invalidates clears completed before it, including a version later superseded by a later-created one. The cost is at most one extra re-screen; the direction is fail closed.
6. **Time basis.** Freshness is measured from the database clock at completion/recording, not from the provider/officer-supplied `screened_at` (which a caller controls). A provider answer that is old when it arrives is therefore treated as fresh from arrival.
7. **A burst on tightening.** Adding a list makes EVERY covered person stale at once: withdrawals wait and the trapped-funds monitor counts them from the first gating version (old) as soon as they are denied, until the scheduler (`EnqueueBatch` per pass) and the worker catch up. That is the correct, visible behaviour; capacity planning for a large tenant is an operations item.
8. **The route refuses back-dated tightenings.** The `effective_from` dimension (earlier than the version in force) can refuse a legitimate tightening submitted within the 2-minute clock tolerance right after another version; it mirrors the screening policy route (0189).
9. **Own-licence tenants, the filing form and tenant visibility** stay as in c4 (out of scope, API only, none). The trigger kind inherits D5 (approver population = platform_admin x 2) and D8.
10. **`NOT VALID` constraint.** `aml_screening_requests_provenance_required` is not validated against legacy rows by design (they are final and untrusted). A dump/restore keeps it NOT VALID.
11. **Disk/time cost of the gate.** `aml_screening_covers` is called per candidate row inside the gate and the scheduler (the gate is evaluated per person per payout claim and per monitor batch of 500). It is a pure expression; no new index was added. A load test with a large tenant has NOT been run.
12. **Not done:** `security` and `ledger-finance` review; Back Office filing form for the three kinds; ADR/registry text applied (proposals below).

## 7. Owner decisions

**Blocks MERGE (engineering sign-off, not product decisions)**

| Id | Item | Who |
|---|---|---|
| M1 | `security` review of 0191 (new SECURITY DEFINER functions, a table guard, a wrapped classifier, two functions made owner-only, proof-operation sharing) and 0192 (re-issued gate, scheduler, finish, record; a function signature change). BLOCKING per `CLAUDE.md`. | security |
| M2 | `ledger-finance` review of 0192: it changes when a clear expires, i.e. who is held at the withdrawal/payout gate, and drops the 12-argument finish function. Confirm the claim "only ever stricter" (every conjunct is added; the legacy path fails closed). | ledger-finance |
| M3 | Acknowledge the effect on EXISTING dev/staging data: every pre-0192 clear stops counting and those persons are re-screened (section 2). Recommendation: accept; rebuild or let the worker catch up. | owner (ack) |
| M4 | Re-run of the orchestrator's own verification (this record is not acceptance). | orchestrator |

**Blocks ACTIVATION (nothing here is enabled; each needs a recorded decision)**

| Id | Decision | Options | RECOMMENDATION |
|---|---|---|---|
| A1 | Who may use the owner path of the two setters, under what recorded decision, and how it is reconciled (D7 residual) | (a) never in production; (b) a named DBA procedure with a recorded decision per use, reconciliation query afterwards | (b), with the decision record cited in `reason_code`/legal reference, and the reconciliation run after each use |
| A2 | D1-D3 of c4 (is governed loosening wanted, cooling period, which dimensions may ever be loosened; sanctions closure stays) now ALSO decide trigger-policy loosening, because the new kind shares the capability row | as c4 | Leave OFF. If wanted, per tenant, with a recorded compliance instruction |
| A3 | D10 follow-up the owner raised in c4: pin the policy id on payout decisions (apply the policy in force at REQUEST time) | build later; accept the current rule (the stricter of recorded and current) | Accept the current rule; the pinning needs ledger-finance design and is not required for safety any more |
| A4 | A real provider's attestation of the screened lists (section 6.3) | require it in the provider contract; accept the worker's statement | Require it before any non-MOCK provider |
| A5 | D5 (approver population; own-licence tenants out of scope), D6 (jurisdiction floors replace the 3600 s engineering floor), D8 (tenant notification), D9 (filing form) of c4 | as c4 | Unchanged from the c4 record |
| A6 | Capacity: the `EnqueueBatch`/pass size and monitor batch for a large tenant after a list is added (section 6.7) | measure; set per tenant configuration | Measure before any non-MOCK activation |
| A7 | Whether the `effective_from` dimension should count as loosening on the trigger route (section 6.8) | keep (mirrors 0189); drop it for the trigger route | Keep |

## 8. Proposed governance text (for the orchestrator; nothing is decided here)

**ADR 0116 s6 (AML routes), proposed amendment.** "Both screening configuration routes are tighten-only in the DATABASE: `aml_screening_policy_author` (0189) and `aml_screening_trigger_policy_author` (0191). The unrouted setters `aml_screening_policy_set` (0154) and `aml_screening_trigger_policy_set` (0181) are OWNER-ONLY since 0191: neither the runtime role nor PUBLIC can execute them. Loosening a screening policy or a trigger policy is available only as the governed two-Person licensing changes `aml_screening_policy_loosen` and `aml_screening_trigger_policy_loosen` (ADR 0127), inert until a per-tenant capability is configured."

**ADR 0127 (PROPOSED, governed loosening), proposed addition.** "A third kind, `aml_screening_trigger_policy_loosen`, loosens the screening TRIGGER policy under the same primitive and the same capability row: loosening of the trigger policy is an event removed, a withdrawal gate switched off, a longer gate alert threshold, or an earlier effective time (route only). It is always widening, can never enable a withdrawal gate (checked in the validator and the table guard), keeps a gate only where the environment anchor allows one, and requires the alert threshold to stay at most half the screening interval. Closed codes: `trigger_policy_loosening_refused`, `no_trigger_policy_to_loosen`, `trigger_gate_enable_refused`."

**ADR 0116 s4.3 (screening gate), proposed addition (D10).** "A screening satisfies the withdrawal/payout gate only while it COVERS the person: its provenance (policy version, lists screened, interval in force when produced) is recorded; its lists are a superset of the lists now in force; no policy version since lacked a list now in force; and now is before its completion time plus the SHORTER of the recorded and the current re-screen interval. A longer interval never revives an expired screening; a shorter one tightens at once. A screening with no recorded provenance is not trusted and the person is due for re-screening. Freshness is measured on the database clock. The scheduler uses the same predicate."

**ADR 0119 s9.1 (step-up registry).** No change: the trigger policy author uses the same operation (`aml_screening_trigger_policy:set`, critical) as the route did; the owner-only setter shares the proof operation but is not runtime-callable. The three licensing change kinds use the existing `licensing_change:*` operations.

**Runbook text (applied in this branch):** `docs/runbooks/aml-screening-triggers.md` "d1 additions" (owner path of the setters, the loosening table, the governed kind, the D10 rules and the effect on existing data); `docs/runbooks/licensing-change-approvals.md` section 6a.

**Registry rows (`docs/governance/task-registry.md`), proposed.**

| ID | Owner | Text |
|---|---|---|
| D1-D7-BREAK-GLASS-OWNER-ONLY | identity-compliance | Runtime `EXECUTE` on `aml_screening_policy_set` revoked (migration 0191); fixtures moved to `amlfix.OwnerService`; init script re-asserts; catalogue allow-list of runtime writers of `aml_screening_policies`. **IMPLEMENTED; security review PENDING.** Owner procedure for the break-glass: OPEN (A1). |
| D1-D4-TRIGGER-LOOSENING-GOVERNED | identity-compliance | Trigger policy route tighten-only in the database; 0181 setter owner-only; governed kind `aml_screening_trigger_policy_loosen` under the c4 capability (INERT, no row). **IMPLEMENTED, INERT; security review PENDING.** Supersedes the OPEN state of C4-D4-TRIGGER-POLICY-LOOSENING for the software part; the capability decision (D1-D3) stays OPEN. |
| D1-D10-FRESHNESS-LIST-BINDING | identity-compliance + ledger-finance | Provenance on every clear; `LEAST(recorded, current)`; list superset and no-gap rule; legacy clears untrusted; scheduler uses the same predicate (migration 0192). **IMPLEMENTED (MOCK provider); ledger-finance and security review PENDING; provider attestation of the lists PROVIDER DEPENDENT.** Supersedes the OPEN state of C4-D10-RETRO-RELEASE for the software part (pinning the policy id on payout decisions remains an optional follow-up, A3). |
| D1-REVIEW-0191-0192 | security, ledger-finance | OPEN, BLOCKING. See M1, M2 of `docs/plans/d1-security-closure-record.md`. |

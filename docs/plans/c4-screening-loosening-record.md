# c4 record: governed loosening of the AML screening policy

Branch `c4-screening-loosening` (from 4974b0d; migration head was 0189, this slice adds **0190**, the only migration of the line). Software capability only; the only
screening provider is the MOCK; no regulatory approval is claimed. Design and options: `docs/plans/c4-screening-loosening-design.md`. Runbook:
`docs/runbooks/aml-screening-triggers.md` ("c4 additions").

## 0. Labels

| Deliverable | Label |
|---|---|
| Design note (what counts as loosening, options, safe default, decisions) | IMPLEMENTED (document) |
| Governed loosening mechanism: two licensing change kinds, per-tenant capability, executor, guards, audit, closed refusal codes | IMPLEMENTED, INERT by default (no capability row, no value ships) |
| Safe default "loosening refused unless configured, fully approved and past its cooling period" | IMPLEMENTED (database-enforced) |
| Back Office: list, detail, approve, reject, cancel and fixed refusal wording for the two kinds (generic pages) | IMPLEMENTED |
| Back Office: a form to FILE a loosening or capability request | NOT IMPLEMENTED (API only: `POST /v1/admin/licensing-changes`) |
| Loosening of the TRIGGER policy (fewer events, gate off) under two Persons | NOT IMPLEMENTED (owner decision D4; today the single-officer critical route) |
| Removal of the runtime `EXECUTE` on the unrouted `aml_screening_policy_set` (break-glass) | NOT IMPLEMENTED (owner decision D7) |
| Any cooling period, interval, grace, list rule or threshold value | NOT CHOSEN (all are OWNER DECISIONS, section 6) |
| Enabling the withdrawal screening gate in production | NOT DONE, NOT POSSIBLE through this slice; the production guard is untouched |
| `security` review (BLOCKING) and `ledger-finance` review of migration 0190 | REQUESTED, NOT YET DONE (section 5) |

## 0a. Review round (security: acceptable after fixes; ledger-finance: approved with conditions) - fixes applied IN PLACE to migration 0190

Migration 0190 is unreleased on this branch and was edited in place; any database that applied the first version must be REBUILT (the 0190 down was also extended and is
re-verified byte for byte). Merged `integ-c` at 889bdff first (clean; its `licensing_change_routes.go` was taken, and the c4 `check_codes` plumbing coexists with c1's `ClosedRefusals`).

| Finding | Fix | Test (fails before, passes after; mutation proof) |
|---|---|---|
| SEC HIGH-1 officer controls bypassed | The requester, every counted approver and the executor of a loosening, and of an enabling or shortening configure, hold an in-force PLATFORM `aml_officer` grant on the tenant (live-eligible) and the owner-scope officer quorum (2 Persons) holds: `screening_loosening_officer_ok`, `_officers_assert`, `_approvers_assert`, a BEFORE INSERT trigger on `licensing_change_approvals` for the two kinds, calls in the validator, the configure classifier and both executors. Narrowing configure (disable, longer cooling) stays open to any platform_admin. Codes `officer_grant_required`, `officer_quorum_not_met`. | `TestC4_OfficerControls_PlainPlatformAdminsCannotEnableOrLoosen` (three plain admins; one officer is no quorum; a non-officer approval is not recorded), `..._ApproverLosingStandingBeforeExecutionDoesNotCount`. Mutations killed: validator check, configure check, approval trigger, counted-approver check, quorum, grant check. |
| LF H-1 sanctions list | `sanctions_list_removal_refused`: never removed through this path (safe default, not a business policy). | `TestC4_SanctionsListRemoval_...`, `TestC4_NotALoosening_...`; mutation killed. |
| LF M-2 mixes | Every dimension looser or equal: `loosening_mixes_tightening` (a list added, a shorter interval or grace). | `TestC4_NotALoosening_...`; mutation killed. |
| SEC MEDIUM-1 counter-authoring window | `effective_from` = execution + the LATER of the payload offset and the tenant cooling period. | `TestC4_EffectiveTime_IsAtLeastTheCoolingPeriodAfterExecution`; mutation killed. |
| SEC LOW-1 capability lock | `screening_loosening:<tenant>` advisory lock in the validator and both executors before the capability is read. | `TestC4_CapabilityLock_AFinalApprovalWaitingBehindADisableObservesIt` (a disable queued first makes the final approval observe `loosening_disabled`); the mutation (lock removed from both functions) is killed. A first version of the test hung when the assertion failed: it now always releases the lock holder. |
| SEC LOW-2 policy guard | The platform branch also requires `effective_from` in [executed_at, executed_at + 30 d + 1 min] and `reason_code = 'governed_loosening'`. | `TestC4_PolicyGuard_BoundsTheEffectiveTimeAndTheReasonCode` (owner-forged executed request, control row admitted); two mutations killed. |
| LF M-1 retroactivity | Documented (runbook "Retroactive effect of a loosening", design section 7). Safe guard: a longer `backfill_grace_seconds` is refused (`grace_extension_after_expiry_refused`) once the previous grace has run out while a gating trigger policy exists. The retro-release by a longer interval is NOT prevented: OWNER DECISION D10. Policy ids are NOT pinned on payout decisions (out of scope, follow-up). | `TestC4_GraceExtensionAfterExpiry_IsRefused` (staging scratch DB); mutation killed. |
| LF L-2 reconciliation | Query added to the runbook (executed requests vs `change_request_id` rows, and the capability). | documentation |

Kept as LAUNCH BLOCKERS, not changed: D4 (trigger-policy loosening is a single officer chain) and D7 (the unrouted `aml_screening_policy_set` break-glass stays reachable).

## 1. What was built

Migration `0190_c4_screening_policy_governed_loosening.{up,down}.sql`, minimal by construction: the 0180 classifier body is NOT copied; `licensing_change_classify`
is renamed `licensing_change_classify_base` and wrapped. Contents:

- `aml_screening_loosening_settings` (append-only per-tenant capability versions: `enabled`, `cooling_seconds` 0..255600, `evidence_reference`, `reason_code`,
  `change_request_id` unique FK, author columns). ENABLE + FORCE RLS, the single `aml_owner_session` policy, no privilege for the runtime role, `aml_deny_mutation` triggers,
  insert guard (`LC040`).
- `aml_screening_policies`: `change_request_id` column (unique, FK), `created_by_scope` also `platform` (CHECK-coupled to the request id), `aml_screening_policies_guard`
  extended for `platform` rows (an executed `aml_screening_policy_loosen` request of the same transaction, executor and payload). The `platform_acting` branch is the 0154 check unchanged.
- Kind rows `aml_screening_policy_loosen` and `aml_screening_loosening_configure` (2 approvals, `platform_admin`), classifier branches in the wrapper.
- `screening_policy_loosen_assert`, `screening_loosening_state`, `screening_loosening_settings_apply`, `screening_policy_loosen_apply` (grants mirrored in `deploy/init-app-role.sql`).
- Go: `internal/licensingchange/screening_loosening_kinds.go`; `GuardError.Hint` (LC050 only); `licensingChangeCheckCodes` in the licensing change routes (closed table membership,
  anything else dropped); five new closed codes in `aml.RefusalCodes` (`loosening_not_configured`, `loosening_disabled`, `no_screening_policy_to_loosen`, `not_a_loosening`,
  `loosening_cooling_period`); OpenAPI (`AMLScreeningRefusalCode` enum and descriptions, kind enum and payload documentation, tighten-only route text); Back Office labels, fixed wording
  and the licensing error view; runbooks.

Rules (all database-enforced; Go only decodes early): configured and enabled for the tenant; tenant configured for the jurisdiction; a previous version exists; the request is a
loosening (list removed, longer interval or grace versus the latest version); interval at least 3600 s and at least twice the latest gating `gate_alert_after_seconds`, interval and grace at
most 31536000 s; `effective_from` = execution time + 0..2592000 s; the cooling period counted from filing, checked again at execution; the production environment guard (a gated
jurisdiction in a non-development/staging/test anchor refuses); two distinct platform_admin Persons, requester and the requester's Person never decide, approvals at most 24 h old,
request lifetime 72 h, signed proof on request, approve and execute (existing step-up class `critical`); execution writes the policy row, the audit rows
(`licensing_change.requested|approved|executed` and `aml.screening_policy_loosened` with before and after) in one transaction.

The tighten path is unchanged: `POST .../screening/policy` (0189) is untouched and still refuses loosening (`policy_loosening_refused`, its description now points at the governed request).

## 2. Verification (commands actually run after the review fixes; private database `c4scr`, rebuilt from the final migration set)

Environment: PostgreSQL 16 local, runtime role `igaming_runtime` (non-superuser, non-BYPASSRLS, asserted by the test world), private database only. Disk filled once during the
work (ENOSPC): `go clean -cache` was run, as the brief allows.

| Command | Result |
|---|---|
| `PRIV_DB=c4scr PRIV_SRC=<worktree> bash priv_test.sh -tags integration -count=1 -p 1 ./internal/licensingchange` (all 0177-0183 and 0190 tests; includes migration 0190 up/down/up byte for byte and the `LC095` down refusal) | ok (101.6 s) |
| same, `./internal/aml` (full package: unchanged 0189 / lic29 / lic30 tests, the catalogue and static pins) | ok (255.3 s) |
| same, `./internal/httpserver -run 'Licensing\|Screening\|AML\|Staff\|StepUp\|OpenAPI\|Route\|Mutation\|C1\|C2'` | ok (529.1 s). A first run of the same subset had ONE failure, my own HTTP test, which needed officer grants for the three fixture operators after fix 1; fixed and the whole subset re-run |
| same, `./cmd/migrate ./internal/db` | ok (2.5 s, 55.6 s) |
| `go test -count=1 ./...` (unit, no integration tag) | 79 packages ok, none failed |
| `go vet ./...` | clean |
| `gofmt -l` on every touched or new Go file | no output |
| backoffice `npx vitest run src/features/licensing src/features/aml` | 16 files, 275 tests passed; `npx tsc --noEmit` clean |
| `python3 yaml.safe_load` of `docs/api/openapi/platform-api.yaml` | ok |

New tests: `internal/licensingchange/screening_loosening_integration_test.go` (21 tests after the review round, adding officer controls x2, the sanctions closure, the capability lock race, the effective-time window, the policy guard bounds and the grace guard; originally 14: unconfigured and disabled refusals, per-tenant isolation of the capability, end to end with audit
and before/after, self approval, the requester's other account, single approver, duplicate approvals by account and by Person, `not_a_loosening` and `no_screening_policy_to_loosen`
and `tenant_not_in_jurisdiction`, the interval floor and the alert-threshold floor, the one-year caps and the offset window with the SQL checked directly, cooling period with a real wait,
longer versus shorter cooling classified narrowing versus widening, a stale (> 24 h) approval refused by Go and by the database, an expired request, three concurrent approvers and two
accounts of one Person racing, tenant staff (filing, reading, approving, direct SQL, the executors, RLS from another tenant), the executors and the table guards without an executed
request of the transaction, a replayed executor, the wrong kind, tampered payload, forged and replayed proofs, strict decode, migration 0190 up/down/up byte for byte plus the `LC095`
refusals, three production-anchor scenarios on throwaway databases (`production`, unrecorded, `staging`), and a static pin that 0190 never mentions the gate functions),
`internal/httpserver/licensing_change_screening_loosening_http_integration_test.go` (real HTTP stack as `igaming_runtime`: inert 409 with the closed code, tenant staff 403, two-Person
flow, the tenant cannot read the request, before/after outcome, `not_a_loosening` code), `internal/httpserver/licensing_change_check_codes_test.go` (closed table only),
`backoffice/src/features/licensing/screeningLoosening.test.ts`.

Mutation checks (each applied to the live function or the migration, killed by the named test, then restored; the database was rebuilt from the file afterwards):
cooling check removed (`TestC4_CoolingPeriod_...`); interval floor removed, alert-threshold floor removed (`TestC4_Loosening_NeverGoesBelow...`); configured check removed, enabled
check removed (`TestC4_Unconfigured_...`); not-a-loosening check removed, tenant-in-jurisdiction check removed (`TestC4_NotALoosening_...`); configure classified never-widening,
loosen classified narrowing (`TestC4_ConfigureThenLoosen_...`, cooling, unconfigured); executor accepting an earlier transaction (first attempt SURVIVED because the replay used another
actor: the test now replays with the executing actor and the mutation is killed), executor skipping the cooling check, executor ignoring the offset; policy guard open, settings guard open
(`TestC4_ExecutorsAndGuards_...`, the settings forgery assertion was added after the first mutation run showed no test forged a capability row); state readable by a tenant session
(`TestC4_TenantStaff_...`); the production environment guard removed from 0190 (`TestC4_ProductionAnchor_...`).

## 3. Intentional pin changes (nothing weakened)

Review round: NO new pin change. The refusal-code drift pins (Go table, OpenAPI enum, Back Office wording, migration HINT literals) grow by five codes
(`officer_grant_required`, `officer_quorum_not_met`, `sanctions_list_removal_refused`, `loosening_mixes_tightening`, `grace_extension_after_expiry_refused`); the pins themselves are unchanged.
Tests of this slice were adapted, not weakened: fixtures now grant the officers the new rules require, and the earlier "dropping a list" case drops `pep` (the sanctions list is closed).
The six changes below are from the first round and stand.

1. `internal/licensingchange/kinds_test.go` `TestKinds_MatchTheMigrationRegistry`: reads 0190 too; expects 15 kinds (13 + 2) and 15 seeds of `2, ARRAY['platform_admin']`.
2. `internal/aml/refusal_test.go` `TestLic29_RefusalCodes_MatchTheMigrations`: also reads 0190, so every new code must be raised by a literal `HINT` there. The test body is unchanged.
3. `internal/aml/adversarial_integration_test.go`: `amlTables` gains `aml_screening_loosening_settings` (the catalogue test says a new AML table needs a catalogue decision: this record is it). In
   `TestAppendOnly_BindsTheOwner` the table is skipped in the UPDATE/DELETE loop ONLY, because a row exists only after an executed two-person licensing change which that package cannot run;
   the UPDATE, DELETE and TRUNCATE refusals WITH a row are asserted in `licensingchange.TestC4_ExecutorsAndGuards_...`, and the TRUNCATE leg of the aml test still applies to it.
4. `internal/aml/static_test.go` `TestStatic_AMClassOnlyInAMLMigrations`: 0190 added to the AML migration list (it re-issues the 0154 policy guard, which carries `AM001`).
5. `internal/httpserver/aml_screening_policy_lic30_test.go` `TestLic30_RefusalCodesAreNotUsedOutsideTheAMLRoutes`: ONE named exception, `licensing_change_routes.go`, which may only test
   `aml.RefusalCodes[hint]` membership (the file must not call `RefusalCode(`, and must mention the table exactly once). Every other file is still refused.
6. Wording (not assertions): the `policy_loosening_refused` text in `aml.RefusalCodes`, OpenAPI, `screeningErrors.ts`, `ScreeningLabels.ts`, `ScreeningPolicyTab.tsx` and the e2e mirror
   `backoffice/e2e/lib/sp_ui.ts` no longer says "Loosening has no approval path"; they say that this form only tightens and that loosening is a separate two-person request, refused unless
   switched on. The Playwright e2e was NOT re-run (only the mirrored string changed; the vitest wording pins pass). `TestSetScreeningPolicy_IsNotRoutedYet_SR7_3` and the golden route tables
   are UNCHANGED: no route was added.

## 4. Gaps and residual risks (honest)

1. **Trigger policy loosening is not two-Person** (D4). `POST .../screening/trigger-policy` can still reduce events or switch the gate off with one officer chain (quorum, critical step-up, proof).
2. **Break-glass remains**: `aml_screening_policy_set` is still runtime-executable (D7, LIC30 L-3). The new path is therefore not the ONLY way to loosen until that grant is revoked.
3. **Loosening is measured against the latest version only.** The tighten-only route also compares with the version in force; the governed path does not repeat that (the effective time is
   always execution time or later, so it cannot rewrite history).
4. **Cooling period semantics are bounded by the primitive**: counted from filing, at most 71 h, executed by the final approver's call (an early final approval is refused and rolled back;
   the first approver's approval then stays valid for 24 h only). A longer period needs a primitive change (ADR).
5. **Rename-and-wrap of `licensing_change_classify`**: a later migration that does `CREATE OR REPLACE FUNCTION licensing_change_classify` with a full body silently drops the two kinds; the new
   kinds then fail closed (`LC010`) and the 0190 tests fail. Any such migration must wrap or re-add the branches.
6. **The 0190 down loses nothing silently** (refused while data exists) but a database that applied 0190 and then loosened a policy cannot be rolled back; rebuild instead.
7. **No tenant visibility** of a loosening of its own policy (D8): the request has no subject tenant, and its audit rows are platform-scoped.
8. **Mock provider**: nothing here screens anyone; a longer interval changes only when a (mock) clear expires.
9. The scratch-database scenarios run the service over the owner pool (the governed functions and guards are identical for the runtime role; the runtime-role behaviour is covered on the shared
   private database).
10. `security` (BLOCKING: new SECURITY DEFINER functions, a new guard on a compliance table, a wrapped classifier) and `ledger-finance` (a longer interval or grace changes when withdrawal-gate
    clears expire) have NOT reviewed this slice. It must not be called complete until they have.

## 5. Reviews requested

- `security`: migration 0190 as a whole (definer functions run as the table owner with the session GUC swapped to the subject tenant around single statements and restored; the
  guards read the request table with the tenant setting cleared, the 0177 technique; the wrapped classifier; `LC050` hint plumbing to `check_codes`).
- `ledger-finance`: effect of a governed loosening on the payout gate (`aml_screening_gate_denies` reads the policy in force); the claim that nothing here can enable the gate.
- `qa`: the five pin changes in section 3, and whether the Playwright spec 12 should be re-run for the mirrored wording.

## 6. OWNER DECISIONS (none taken here; each has options and a RECOMMENDATION)

| Id | Decision | Options | RECOMMENDATION |
|---|---|---|---|
| D1 | Is governed loosening wanted, and for which tenants? | (a) never (tighten-only forever + documented break-glass); (b) per tenant, enabled by a two-person decision; (c) platform-wide | Leave OFF (the shipped state). If wanted, (b), tenant by tenant, only with a recorded compliance instruction. |
| D2 | Cooling period: whether one exists, its length, and what it counts from. Since the review round the cooling period is also the minimum delay between the DECISION and the new version taking effect (a window to author a stricter counter-version). | none (0 is a stated value); a period from filing (built, 0..71 h); a period from the first approval (needs a change); longer than 71 h (needs a primitive change) | A non-zero period; the value is the owner's. No default is shipped. |
| D3 | Which dimensions may ever be loosened | interval and grace (built) with the sanctions list CLOSED by default (`sanctions_list_removal_refused`); the pep list removable (built); relaxing the sanctions closure | Keep the sanctions closure. Relaxing it is an owner + compliance decision and first needs (a) the screened lists recorded on every clear and (b) a gate subset check (a clear counts only if it covered the lists now required); neither exists. |
| D4 | Must loosening the trigger policy (fewer events, gate off) also need two Persons? | leave on the critical officer route; add a governed kind; make the route tighten-only | Add a governed kind with the same primitive (separate slice) before the first production tenant. |
| D5 | Who approves, how many | platform_admin x 2 (interim HDR-M-1, built); a separate compliance approver role; 3 for list removal | Keep the interim until HDR-M-1 is answered. |
| D6 | Jurisdiction-specific floors instead of the 3600 s engineering floor | keep; per-jurisdiction floor table | Keep until legal provides values. Never invent them. |
| D7 | Fate of the unrouted `aml_screening_policy_set` | keep (break-glass by an officer quorum); revoke the runtime `EXECUTE` (one migration) | Revoke once D1 is decided and (b) is in use; until then document the break-glass. |
| D8 | Tell the tenant when its policy is loosened | no (built); read-only notice | Decide with compliance; tipping-off rules differ by jurisdiction. |
| D10 | Retroactive release: a longer re-screen interval makes expired clears of waiting withdrawals fresh again at their next gate claim, without re-screening (the longer grace case is guarded) | accept and document (built); require a re-screen of persons whose clear would be revived; pin the policy id on the payout decision and apply the policy in force at request time (follow-up) | Do not enable the capability for a tenant with waiting withdrawals until decided; prefer pinning the policy id (needs ledger-finance design). |
| D9 | A Back Office filing form | none (API only, built); a dialog on the generic pages | Defer until D1 is decided. |

Approval decisions needed before the capability is switched on for a first tenant: D1 to D3 and D10 by the owner; security review of 0190 (blocking); ledger-finance sign-off on the payout-gate effect;
the evidence reference naming the decision record. The production gate stays off regardless.

## 7. Proposed ADR 0127 text (for the orchestrator; nothing is decided)

**ADR 0127: Governed loosening of the AML screening policy** (status PROPOSED, depends on 0116, 0123, 0126).

*Context.* The screening policy route is tighten-only (0189) so a regulator-instructed loosening, or the revert of a value that was valid but wrong, had no governed path.

*Decision (proposed).* A screening policy version that is less strict than the latest version of its tenant and jurisdiction (a list removed, a longer re-screen interval, a longer backfill
grace) is authored only by the licensing change kind `aml_screening_policy_loosen`: two distinct platform_admin Persons, signed proofs (ADR 0110), approvals at most 24 h old, a 72 h request lifetime,
the engineering floors of 0189, the AML officer controls (the requester, each counted approver and the executor hold an in-force platform `aml_officer` grant and the owner-scope quorum holds), a version that is looser or equal in every dimension, never removing the sanctions list, never extending an expired backfill grace, an effective time of execution plus the later of 0..30 days and the cooling period, and execution in the transaction of the final approval. The capability is a per-tenant, append-only setting
(`aml_screening_loosening_settings`) changed by the kind `aml_screening_loosening_configure` (widening when it enables loosening or shortens the cooling period). With no setting the
capability is off and every loosening is refused. The cooling period is configuration, counted from filing, never defaulted. The route stays tighten-only. The withdrawal gate and its production
guard (non-production environment anchor, worker heartbeat) are not touched; a loosening never writes a trigger policy and is refused where a gate exists in a database whose anchor is not
development, staging or test.

*Consequences.* Loosening has one audited, two-person path once enabled; it is inert until the owner decides D1 to D3. The trigger policy and the unrouted setter remain open (D4, D7). A later
migration touching `licensing_change_classify` must preserve the wrapper. Enabling the capability for any tenant requires a recorded owner decision and the `security` and `ledger-finance` reviews.

*Retroactivity.* A loosening applies to withdrawals already waiting at their next gate claim; a longer interval revives expired clears (owner decision D10, not prevented), a longer expired grace is refused.

*Not decided here.* The cooling period, whether the sanctions closure may be relaxed, jurisdiction floors, tenant notification, retro-release (D1 to D10).

**ADR 0119 s9.1 (step-up registry), proposed addition.** "The loosening kinds use the existing `licensing_change:request|approve|execute` operations (class critical); no new operation is added.
`aml_screening_policy:set` stays out of the signer backstop registry; if the unrouted setter is ever routed, it moves in as critical in the same change."

**ADR 0116 s6, proposed amendment of the lic29 sentence.** "Loosening a screening policy over HTTP is available only as the governed two-person licensing change of ADR 0127, inert until configured."

## 8. Proposed register rows (for `docs/governance/task-registry.md`)

| ID | Owner | Text |
|---|---|---|
| C4-SCREENING-LOOSENING-MECHANISM-2026-10-23 | identity-compliance | Governed loosening of the AML screening policy (migration 0190, branch `c4-screening-loosening`): two licensing change kinds, per-tenant capability, executor and guards in the database, closed refusal codes, audit, generic Back Office pages. **IMPLEMENTED, INERT by default; PROVIDER DEPENDENT screening unchanged (MOCK); security and ledger-finance review PENDING.** Filing form NOT IMPLEMENTED. Details `docs/plans/c4-screening-loosening-record.md`. |
| C4-D1-LOOSENING-WANTED | owner | OPEN. Whether governed loosening is wanted at all and for which tenants. RECOMMENDATION: leave off. |
| C4-D2-COOLING-PERIOD | owner | OPEN. Whether a cooling period exists, its length and start (built: from filing, 0..71 h). No default shipped. |
| C4-D3-LOOSENABLE-DIMENSIONS | owner + compliance | OPEN. Which of list removal, longer interval, longer grace may ever be loosened; whether the sanctions list may be removed. |
| C4-D4-TRIGGER-POLICY-LOOSENING | owner + security | OPEN, LAUNCH BLOCKER. Trigger policy (fewer events, gate off) is a single-officer critical route, not two Persons. RECOMMENDATION: governed kind before the first production tenant. |
| C4-D5-D9-REMAINING | owner | OPEN. Approver population (D5), jurisdiction floors (D6), fate of the unrouted `aml_screening_policy_set` (D7, LAUNCH BLOCKER together with D4), tenant notification (D8), Back Office filing form (D9). |
| C4-D10-RETRO-RELEASE | owner + ledger-finance | OPEN. A longer re-screen interval revives expired clears of waiting withdrawals at their next gate claim. Follow-up: pin the policy id on payout decisions. |
| C4-SECURITY-REVIEW-0190 | security | OPEN, BLOCKING. New SECURITY DEFINER functions, a new guard on `aml_screening_policies`, wrapped `licensing_change_classify`. |
| C4-LEDGER-REVIEW-0190 | ledger-finance | OPEN. Effect of a loosened interval or grace on the payout gate; confirm nothing here can enable the gate. |

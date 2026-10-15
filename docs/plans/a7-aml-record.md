# A7 AML operability: implementation record and proposed registry text

Status: IMPLEMENTATION RECORD of branch `a7-aml-operability` (base `f9a31e7`), `identity-compliance`, 2026-10-15. **Not merged, not pushed.
`security` review (mandatory, live) PENDING.** Software capability only: nothing here is a regulatory approval, a licence statement or a
filing capability. Every provider is a MOCK. This file proposes registry text; it does not edit the task registry, HANDOVER, progress,
active-stage or the human decision register (the orchestrator applies what it accepts).

Parent: `docs/plans/platform-completion-audit-2026-10-14.md` item A7. Design: `docs/decisions/0116-aml-internal-foundation.md` (section 13
records this work).

## 1. Deliverables and labels

| # | Deliverable | Label | Where |
|---|---|---|---|
| 1 | AML permission constants `aml_case:read`, `aml_case:work`, `aml_case:close_approve`, `aml_hold:manage` (static route gate; `compliance` and `platform_admin` only; `aml_officer` stays a capability, not a role) | IMPLEMENTED | `internal/auth/permission.go`, `aml_permission_test.go` |
| 1 | 11 officer routes (list/read holds of a person, place hold, lift hold, list/read cases, open case, transition, note, SAR flag, propose, approve) through `staffRoute`, golden table, step-up registry, OpenAPI | IMPLEMENTED | `internal/httpserver/aml_routes.go`, `docs/api/openapi/platform-api.yaml` |
| 1 | Reject a four-eyes proposal | NOT IMPLEMENTED | the service has no reject; a later event withdraws a proposal |
| 1 | Tenant-wide hold list, alert raise route, rule/screening/retention config routes, SAR export | NOT IMPLEMENTED | out of A7 scope (S4, S5, config) |
| 2 | Back Office AML queue: cases list, holds by person, case detail, permitted actions, step-up through the existing interceptor, permission-gated nav | IMPLEMENTED (minimal) | `backoffice/src/features/aml`, `backoffice/src/api/aml.ts` |
| 2 | UI-to-OpenAPI contract test for the AML paths and request-body fields | IMPLEMENTED | `backoffice/src/api/aml.test.ts` (the repo had no such harness; the generic every-`apiFetch` harness is still A10) |
| 3 | Launch readiness items `R-AML-QUORUM`, `R-AML-SCREENING`, `R-AML-MONITORING` (monitoring always fails, explicitly) | IMPLEMENTED | `internal/launchgov/readiness.go`, migration 0154 `aml_readiness` |
| 3 | The launch criterion that BLOCKS activation (ADR 0116 Q1) | NOT IMPLEMENTED | readiness is informational; the existing acknowledgement rule makes the items impossible to ignore silently, nothing blocks |
| 4 | `ScreeningProvider` interface, closed outcome vocabulary, pseudonymous subject reference | IMPLEMENTED | `internal/aml/screening.go` |
| 4 | Deterministic screening provider | MOCK | `internal/aml/screening_mock.go` (`providerkind.Synthetic`, production-refused, registered `aml/screening_provider`) |
| 4 | Real screening vendor | PROVIDER DEPENDENT | |
| 4 | Fail-closed orchestrator + audit trail (`aml_screening_results`) + per-tenant/jurisdiction policy (`aml_screening_policies`) + conformance suite | IMPLEMENTED (officer-initiated) | `screening_orchestrator.go`, `screening_service.go`, `screeningtest/`, migration 0154 |
| 4 | Automatic triggers (registration, KYC approval, scheduler), outbox + worker, system actor, enforcement-gate wiring | NOT IMPLEMENTED | follow-ups F1-F3 below (need decisions) |
| 5 | D-AML-TIP-1 option (c) | PARTIAL: documented duty + unchanged cover; see section 6 | `docs/runbooks/aml-adjustment-approver-confidentiality.md` |

## 2. Migration 0154 (up and down; `0155` intentionally unused; `0152` / `0153` are not mine)

`migrations/0154_aml_screening_and_readiness.{up,down}.sql`. Pre-flight AM098; down refuses AM099 while any screening row exists.

- Tables `aml_screening_policies`, `aml_screening_results`: ENABLE + FORCE RLS, one owner-only policy bound to the session tenant, no
  privilege for `igaming_runtime`, FKs `ON DELETE RESTRICT`, append-only triggers (UPDATE/DELETE/TRUNCATE refused), BEFORE INSERT
  guards that re-check the owner scope, the person/account binding, the hold binding and the proven session actor.
- Result-table CHECKs make fail-closed structural: a clear carries no match/error/action; an error names its closed code and is never
  a clear; a match names its list and can never be recorded as no action; a sanctions match or `confirmed` gates ALL value movement;
  every hold decision names its hold.
- Functions (SECURITY DEFINER, `search_path` pinned, granted to the runtime role, mirrored in `deploy/init-app-role.sql`):
  `aml_screening_policy_set` (platform officer only, quorum, proof), `aml_screening_policy_effective` (proof-less, session-tenant
  bound), `aml_screening_record` (proof, idempotent per `(tenant, key)`, places the required hold in the SAME transaction),
  `aml_screening_list` (proof), `aml_readiness` (proof-less, session-tenant bound, boolean quorum).
- No value is seeded; no default exists for any policy field; the legal review reference is required.

## 3. Routes, permissions, step-up (ADR 0119 8.1)

All under `/v1/admin/tenants/{tenantID}/aml/`; the tenant is the path value only (a foreign tenant is 404), a platform token acts as
`platform_acting` in that tenant (the database refuses without an in-force grant). Bodies are strict (unknown fields refused), contain no
tenant/actor. Responses are `Cache-Control: no-store`; error bodies are fixed sentences plus one closed check code
(`quorum_not_met`, `hold_refused`, `four_eyes_refused`, `stale`, `illegal_transition`, `guard_refused`, `note_refused`).

| Route | Permission | Step-up | Why |
|---|---|---|---|
| GET `aml/cases`, GET `aml/cases/{caseID}`, GET `aml/persons/{personID}/holds` | `aml_case:read` | none | reads |
| POST `aml/holds` | `aml_hold:manage` | none | protective (it only restricts) |
| POST `aml/holds/{holdID}/lift` | `aml_hold:manage` | critical | loosens a control (8.1 (e)) |
| POST `aml/cases`, `.../transition`, `.../notes`, `.../sar-flag` | `aml_case:work` | none | record or restrict only |
| POST `.../proposals` | `aml_case:work` | critical | initiates a four-eyes action (8.1 (d)) |
| POST `.../approvals` | `aml_case:close_approve` | critical | approves a four-eyes action (8.1 (d)) |

The signer backstop `actorproof.StepUpOperations` gains exactly `aml_hold:lift`, `aml_case:propose`, `aml_case:approve` (critical); a unit
test pins each route's class against the registry.

## 4. Design points to review (also ADR 0116 section 13)

1. Outcome vocabulary `clear | potential_match | confirmed | error` (ADR text: `unavailable`); `confirmed` is vendor-reported, we hold, we do not determine.
2. Sanctions-list holds (even for a `potential_match`) use `reason_class = confirmed_sanctions` so they gate all value movement and lift only by the proof-bound four-eyes approval; PEP-only and unavailable-result holds are `manual` / withdrawal+payout.
3. Screening-mandated holds do not wait for the officer quorum (ADR 0116 s6); manual holds still do.
4. A match acts with or without a policy; the policy governs lists and the "no completed result" rule (C1).
5. Ambiguity resolves to the stricter reading (a match naming no list is a match on every requested list; any inconsistent, unknown, panicking or timed-out provider answer is an `error`, never a clear).
6. `aml_readiness` counts grants under a placeholder `app.principal_id` set and restored around one count (FORCE-RLS grant tables admit only principal-bound sessions; launch readiness is a plain tenant read run for a tenant by platform staff). Boolean output only.
7. `RecordScreening` is OFFICER-ATTESTED: the database records what the officer's proof-bound call says; it does not verify that a provider was called. No gate consumes a result today, so a recorded `clear` has no enforcement effect.
8. The mock subject-key resolver is a fixed synthetic key; the real resolver (per-tenant secret-store key) is PROVIDER DEPENDENT / deploy.

## 5. Follow-ups (each needs a decision or another owner; none built)

- **F1 Automatic screening triggers + system actor:** registration, KYC approval, attribute change and the scheduled re-screen need an outbox + worker (ADR 0106 pattern: new `PlatformService` identity + fence catalogue) and a system actor for hold placement (today a hold's `placed_by` must be a proven officer). Needs `security` + `architect`.
- **F2 Gate wiring:** a gate-side predicate "screening required, no completed result" at deposit/withdrawal/payout (ADR 0116 s4.3) is not built; the five gates stay as they are. Needs `ledger-finance`, `payments`, `casino`, `sportsbook`, `product-owner-proxy`.
- **F3 Policy/officer-facing config:** HTTP route + Back Office screen for screening policy (read-only to tenants, platform-authored), rule and retention config (S4/S7 tables), and the Q1 launch criterion that BLOCKS.
- **F4 Reject-proposal operation** (needs a new event type in 0142's state machine) and a hold-placement rate alert (`aml.hold_placement_rate`).
- **F5 Alert routing to officers** (ALERT-DELIVERY-1) and S4 monitoring / S5 SAR (class B).
- **F6 Q6 privacy review** before any identity attribute reaches a vendor.
- **F7 In-product adjustment-approver reminder** (D-AML-TIP-1 (c)) needs `security` + `ux-design` wording sign-off.

## 6. D-AML-TIP-1

The brief says to implement the "owner-approved option (c) as recorded". What is recorded: ADR 0116 12.4 and the decision register row
D-AML-TIP-1 say **OPEN**, "(b) as built is a reversible orchestrator default, NOT owner-approved. Recommended: (c) with a documented AML
confidentiality duty for adjustment approvers, plus (a) later". The audit (D-AML-TIP) repeats the recommendation. **No owner approval of
(c) is recorded in the repository.** QUESTION for the orchestrator/owner: is (c) approved, by whom and when, so the register can say so?

Most conservative reading implemented: option (c) is the acceptance of the visible signal plus a documented duty; it does not require
removing the generic `conflict` cover (and the brief says the cover remains). So: (b)'s code is unchanged (and its tests still pass), the
duty is documented as DRAFT operating text pending owner/legal wording (`docs/runbooks/aml-adjustment-approver-confidentiality.md`), and the
acknowledgement recording and the in-product reminder are recorded as NOT IMPLEMENTED follow-ups. Proposed register row text is in section 9.

## 7. Verification

See the final report of the run; the exact commands and results are copied into section 10 at the end of this file.

## 8. Changed existing assertions (none weakened or deleted; each is an addition or an updated count/list)

- `internal/actorproof/stepup_test.go` `TestStepUpOperations_RegistryPinned`: the pinned map gains the three AML operations (18 to 21).
- `internal/httpserver/staff_route_static_test.go`: route pin 204/125 to 215/133 (11 routes, 8 mutating); `staff_route_golden_test.go`: +11 rows, the "classified on arrival" AML entry replaced by a pointer.
- `internal/httpserver/staff_stepup_integration_test.go` `TestStepUp_GoldenTable_EveryClassifiedRoute`: coverage pin critical 59 to 62, none-mutating 39 to 44, reads 79 to 82 (the 11 AML routes).
- `internal/httpserver/staff_identity_static_test.go`: ADR 0114 H1 allowlist gains four entries (`amlCaseDTO.PersonID`, `amlQueueItemDTO.PersonID` and the two request-body `PersonID` fields: a Person is not a staff identity), each justified; the actor / author ids go through `staffViewer.ref`.
- `internal/aml/static_test.go`: SQLSTATE class `AM` also allowed in `0154_*` (an AML migration).
- `internal/aml/adversarial_integration_test.go`: `amlTables` gains the two screening tables (every catalogue, privilege, policy, trigger and FK check now also covers them).
- `internal/aml/catalog_integration_test.go`: `apiRuntime` +5 functions; `noProof` +2 (the two session-tenant-bound proof-less readers).
- `internal/launchgov/ratify_integration_test.go`: ADDED an assertion (on a pre-0154 scratch schema the AML items are `not_evaluable`/`read_error` and the readiness still commits).
- `backoffice/src/layout/nav.test.ts`: two expected arrays gain `'AML'` (platform_admin, compliance).
- Code constant `launchgov.SnapshotVersion` 1 to 2 (the check set changed; in-flight launch requests filed against the old hash go stale by design).

## 9. Proposed registry text (the orchestrator applies; not edited here)

**Task registry / progress rows**

- A7 AML operability: PARTIALLY IMPLEMENTED on branch `a7-aml-operability` (not merged, security review pending). IMPLEMENTED: S6 minimal (11 routes, permissions, OpenAPI, step-up classes, minimal Back Office queue, UI-to-OpenAPI contract test), S7 readiness items, S2 foundation (interface, MOCK, fail-closed officer-initiated orchestrator, audit trail, policy config, conformance suite, migration 0154). NOT IMPLEMENTED: S4 monitoring, S5 SAR, S2 automatic triggers/system actor/gate wiring, reject-proposal, policy HTTP route/screen, the blocking launch criterion.
- New open rows: `AML-SCREEN-TRIGGERS-1` (F1), `AML-SCREEN-GATES-1` (F2), `AML-CONFIG-ROUTES-1` (F3), `AML-REJECT-PROPOSAL-1` (F4), `AML-LAUNCH-CRITERION-1` (Q1 blocking), `AML-TIP-REMINDER-1` (F7).
- Migrations: 0154 used; 0155 unused (add to the reserved list with 0152/0153 if those are also unused).

**human-decision-register D-AML-TIP-1:** keep OPEN until the owner confirms. Proposed text: "Engineering has implemented option (c)'s documented duty
(`docs/runbooks/aml-adjustment-approver-confidentiality.md`, DRAFT wording) and kept the generic cover (b) unchanged. Owner/legal still to: approve (c), approve the wording, decide where the
acknowledgement is recorded, and decide whether to add (a) later. Launch-blocking for `under_platform_licence` tenants whose finance staff approve adjustments."

**HD-AML-1 (launch gating):** engineering now shows R-AML-QUORUM / R-AML-SCREENING / R-AML-MONITORING in the readiness panel; monitoring is a standing failing item; enforcement of Q1 (blocking) is not built.

## 10. Verification log (private database `a7_*`, rebuilt from this worktree's migrations; local CI role grants; all `-tags integration -count=1 -p 1`)

| Command | Result |
|---|---|
| `gofmt -l cmd internal` | clean |
| `go vet ./...` and `go vet -tags integration ./...` | PASS |
| `golangci-lint run ./...` (2.9.0) | 0 issues. `--build-tags integration` shows 667 PRE-EXISTING issues (integration-tagged files are not linted in CI); none in a file of this branch |
| `go test ./... -count=1` (unit) | PASS: 62 packages ok, 1525 test functions PASS, 0 FAIL, 5 SKIP (the 5 skips are the pre-existing ones) |
| `internal/aml/...` integration | PASS (also with `-race`, 176 s) |
| `internal/launchgov`, `internal/kyc`, `internal/tenant`, `internal/auth/...`, `internal/config`, `internal/alerting/...`, `internal/db`, `internal/actorproof/...`, `internal/adjustment`, `cmd/...` integration | all PASS |
| `internal/httpserver -run 'StaffRoute\|Golden\|OpenAPI\|AML\|Aml\|Launch\|StaffIdentity'`, `-run 'TestStepUp\|StaffLiveness\|StaffRoute\|Golden'`, `-run 'F4\|ManualAdjustment'` | PASS (the full httpserver / payments packages were NOT run, per the brief: hosted CI shards them) |
| `backoffice`: `npx vitest run` / `npm run build` | 37 files, 431 tests PASS / build OK |
| migration 0154 up / down / up whole-schema snapshot, pre-flight AM098, down refusal AM099 | PASS (`internal/aml/migration_0154_integration_test.go`) |
| `go run ./cmd/migrate verify` on the migrated private DB | FAILS only with `GAP 152, 153` (those numbers are other work's; `0155` is unused). Add them to `reservedUnusedVersions` at merge as appropriate |

Environment-dependent, not caused by this branch: `TestEligibility_PlatformOfficers_SuspendedOrDemotedAreNoOfficersAndNoQuorum` and `TestEligibility_SanctionsLiftNeedsTwoLiveOfficers` fail on a FRESH database with `SF020` (the last enrolled platform admin cannot be suspended) and pass once any other enrolled platform admin exists (hosted CI's shared database has them; any httpserver staff-token test creates one). Observed here by running them alone, then after `TestAMLHTTP_OfficerFlow`.

### Mutation pass (kill or justify; harness files in the session scratchpad, not committed)

- **Go enforcement / fail-closed logic: 53 mutants, 53 KILLED.** Decide boundaries and rules (9), provider-answer normalisation, timeout, panic, replay and case rules in the orchestrator (13), record validation mirroring the CHECKs (5), the mock provider (2), route tenant scoping / platform scoping / no-store / fail-closed availability / error table / step-up class / permission-per-route / limit bound (12), readiness checks (9), step-up registry (3). Two harness defects were found and fixed along the way (a mutant that did not compile; a Go build-cache race with another session: the run was repeated with a private `GOCACHE`).
- **SQL functions and guards: 29 mutants, 27 KILLED, 2 SURVIVORS, both EQUIVALENT:** `aml_screening_record` "hold decision without a reason code accepted" and `aml_screening_policy_set` "list count unchecked". In both the guard is a second check behind the Go validation AND a table constraint (`aml_holds.reason_code` NOT NULL + regex; `aml_screening_policies.lists` NOT NULL + CHECK), so the write is refused either way; only the SQLSTATE differs. Not killable through the officer API without bypassing the Go validation.
- **26 table CHECK constraints (results + policies): every one dropped in turn, 26 of 26 KILLED** after tests were added for the survivors (confirmed-with-PEP-scope, decision vocabulary, list vocabulary and count, recording-scope vocabulary, every policy-table CHECK).
- Not mutation-testable in a single session (recorded, not claimed): the per-person advisory lock in `aml_screening_record` (a concurrency property), the sub-millisecond timing parity, and the browser rendering of the Back Office beyond the vitest cases.

## 11. What `security` should probe (live)

1. **Route layer:** a `compliance` token WITHOUT an `aml_officer` grant on every route (expect 403, nothing written); a tenant-A token naming tenant B in the path (404 everywhere, including the platform-owned case ids); a platform token with no acting grant for the named tenant; a token with `role` altered or `tenant_id` forged; a body or query carrying `tenant_id` / `actor_id` (must be 400); replay of the same propose / approve (CAS and single-use proof); `?tenant=` on the Back Office case link for a tenant officer (ignored: claims win).
2. **Step-up:** lift, propose, approve with a 6-minute-old second factor (403 `step_up_required`, nothing written); place hold / transition / note / flag with the same token (allowed by design: protective). Probe whether a hold placement should be critical (a compromised officer session can freeze players; no `aml.hold_placement_rate` alert exists).
3. **`aml_readiness`:** the placeholder `app.principal_id` set/restore; call it from a plain tenant session with a forged `app.tenant_id`, an acting session and a read-only transaction; try to make it return another tenant's data or the officer count; check the restore on the error path.
4. **`aml_screening_record`:** no quorum by design (confirm this reading of ADR 0116 s6); an officer screening a person of ANOTHER owner scope / tenant; idempotency-key reuse across tenants and with changed content; a recorded `clear` for a person never sent to a provider (results are OFFICER-ATTESTED today; nothing consumes them, but F1/F2 would); the `confirmed_sanctions` class on a `potential_match`; hold placed with `player_account_id` NULL for a person with several accounts.
5. **Tipping-off:** error bodies and logs (no person, case, hold or match text); `Cache-Control`; no `audit_log` row (a test pins this); staff-identity redaction of actors and note authors for tenant viewers; the readiness coarsening of provider-derived details; the generic `conflict` cover (D-AML-TIP-1) is unchanged but its deterministic signal and timing difference remain.
6. **Definer-only posture:** the catalogue tests cover the two new tables and five functions (runtime has no table privilege; one owner-only policy; append-only triggers; search_path pinned). Confirm `deploy/init-app-role.sql` re-assertion and that no GUC grants anything (the policy-table read is proof-less by design: tenants may read their policy, never a result).
7. **Mock labelling:** `MockScreeningProvider` / `MockSubjectKeys` are `Synthetic`; the former is registered under `aml/screening_provider` so production startup is refused while it is wired; `MockSubjectKeys` is wired nowhere (the orchestrator is unwired); confirm no production code path constructs the orchestrator.
8. **Back Office:** note bodies rendered as text; no server message rendered; the proposal an approver sees is the server's last event with its server content hash (never typed); an approver cannot approve their own proposal in the UI (the server also refuses).

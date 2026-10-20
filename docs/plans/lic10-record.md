# lic10 record: AML trigger wiring (G1), residence maker-checker (G8 / Q21), KYC enforcement policy authoring (L-20)

Branch `lic10-aml-residence-kyc` (from `claude/licensing-jurisdiction-completion` e2ade1e, migration head 0179). Migration **0181** (0180 belongs to the parallel `operating bases` work). Software capability only: nothing here is a regulatory approval, a licence statement or a legal value. **No shipped threshold, mode, event list or policy row anywhere.** No real provider. No secrets.

**VERIFICATION STATE (read first).** The authoring session's database harness command was refused by the permission system, so **no integration test and no SQL of migration 0181 has been executed**. What was run: `gofmt`, `go vet` (plain and `-tags integration`), `go build ./...`, `go test ./...` (all unit and static tests green after the pin updates listed in section 6), `go test ./internal/db`. Every integration test named below is written but NOT RUN. The owner runs them (section 7) before anything is merged. Treat migration 0181 as unreviewed SQL: `security` review is mandatory and BLOCKING.

## 1. Verified current state (from code, not docs)

### Item 1, AML triggers (audit G1)

Existed before this work (ADR 0116 A7, officer-initiated only): `ScreeningProvider` interface + closed outcome vocabulary + pseudonymous subject reference (`internal/aml/screening.go`), the MOCK provider (`screening_mock.go`, Synthetic, production-refused), the fail-closed orchestrator (`screening_orchestrator.go`), per-tenant/jurisdiction screening policy versions (`aml_screening_policies`: lists, re-screen interval, backfill grace, legal review reference; authored by a platform_acting officer with quorum and a signed proof), results with hold placement in one transaction (`aml_screening_record`, officer-proved, officer quorum), holds with place/lift (sanctions lift needs four-eyes), cases, the SAR flag, readiness items R-AML-QUORUM / R-AML-SCREENING / R-AML-MONITORING (monitoring always fails).

NOT wired before this work (grep: zero production callers of `Screen`/`NewOrchestrator`): any automatic trigger (registration, KYC approval, scheduled re-screen), any queue/worker, any use of `rescreen_interval` (it was stored and never read), any gate predicate "screening required, no completed result" (a recorded `clear` had no enforcement effect), transaction monitoring, first-deposit and withdrawal-request hooks.

### Item 2, residence determinations (audit G8, Q21)

`kyc.ReviewVerification` writes `verified_residence_country/_source/_set_by/_set_at` on one reviewer's call; `kyc.ReadMarketResidenceEvidence` (the ONLY evidence consumer; `GetVerifiedResidence` has no production caller) made it evidence immediately; the F5(b) control is an after-the-fact audit row plus the `kyc.residence_into_open_market` alert, no block, no maker-checker.

### Item 3, KYC enforcement policies (L-20)

`internal/kyc/enforcement_admin.go` (Create draft / Activate / Withdraw / Supersede) was library-only; migrations 0100/0103 enforce draft-only insert, four-eyes creator-vs-activator by `app.platform_admin_principal_id`, no activation of `edd_amount`/`registration_tier` (not evaluator-wired), no withdrawal of an ACTIVE row without a successor. There was no route.

## 2. Deliverables and labels

| # | Deliverable | Label |
|---|---|---|
| 3 | Routes `GET/POST /v1/admin/kyc/enforcement-policies`, `POST .../{policyID}/activate | withdraw | supersede` (platform_admin, step-up critical on every write, reason code, IP/UA/request id on audit), golden table, OpenAPI, static pins, unit + integration tests, runbook | **IMPLEMENTED** (integration tests not run) |
| 3 | Back Office pages | NOT IMPLEMENTED |
| 3 | `edd_amount`, `registration_tier` authoring | NOT IMPLEMENTED (evaluator does not consult them; route refuses them) |
| 2 | Per-tenant policy `none | into_open_market | all`, governed (stricter = one actor, active at once; less strict = pending, two approvals by two distinct Persons, requester excluded; CHECK-enforced), signed actor proofs, DB guards, runtime has no write privilege | **IMPLEMENTED** (SQL and integration tests not run) |
| 2 | Pending determinations are not market evidence (fail closed), snapshot of the mode on each determination, second-Person approval bound to the exact determination, pending list, routes, OpenAPI, runbook | **IMPLEMENTED** (not run) |
| 2 | Per-jurisdiction override of the mode | NOT IMPLEMENTED (per tenant only) |
| 2 | Launch-readiness INFO "mode is none" | NOT IMPLEMENTED: needs `launchgov.SnapshotVersion` (forbidden here); proposed text below |
| 2 | Platform-level floor / re-check sweep of existing determinations after tightening / Back Office | NOT IMPLEMENTED |
| 1 | Governed trigger policy (events registration, kyc_approval, scheduled; gate_withdrawal flag), authored exactly like the screening policy | **IMPLEMENTED** (not run); service/library only, no HTTP route, no Back Office |
| 1 | Durable queue `aml_screening_requests` (DB unique key on (tenant, idempotency_key), state-machine trigger, claim lease + token CAS, retry with backoff, terminal failure, alert `aml.screening_attention`) | **IMPLEMENTED** (not run) |
| 1 | Enqueue triggers on `player_accounts` (registration) and `kyc_verifications` (approval), scheduled enqueue (periodic re-screen + backfill) | **IMPLEMENTED** (not run) |
| 1 | Worker over the provider interface | **MOCK** (started only outside production, MOCK provider and MOCK subject key resolver); real vendor PROVIDER DEPENDENT |
| 1 | Withdrawal/payout gate through the existing gate point (`aml_hold_active`): held without a fresh completed clear, with a system-found unresolved match, or after terminal failure; backfill grace for existing persons | **IMPLEMENTED** (not run) |
| 1 | Hold placement by the system | NOT IMPLEMENTED by design (needs a system actor, ADR 0116 F1; a match waits for an officer who records under the request's key) |
| 1 | First-deposit and withdrawal-request TRIGGERS | NOT IMPLEMENTED (reasons in the runbook) |
| 1 | Transaction monitoring and rules | **NOT IMPLEMENTED**. `R-AML-MONITORING` semantics unchanged and truthful: it still always fails and must still be acknowledged. Nothing here is, or may be described as, monitoring. |
| 1 | HTTP routes/Back Office for the trigger policy and the request list | NOT IMPLEMENTED |

## 3. Decisions made (interim, reversible; flagged for the owner)

1. **Residence policy does not use the `licensing_change` primitive.** The registry replaces `licensing_change_classify` and `licensing_change_row_matches` as WHOLE functions in each migration that adds a kind; the parallel migration 0180 replaces the same two functions. A blind second replacement in 0181 would drop their branches. Chosen instead: a self-contained governance with the same rule set (stricter = one actor, less strict = two approvals by distinct Persons, requester excluded, DB-enforced, ADR 0110 proofs, same `financial_actor_session`/`actor_proof_require` machinery). Revisit once 0180 is merged if the owner prefers a registry kind.
2. **Default is today's behaviour.** No policy row = mode `none`. **Behaviour change on deploy: none.** An existing tenant changes only when its `compliance` staff configure a mode.
3. **Snapshot semantics.** The mode in force is recorded on each determination when made; tightening applies to future determinations only. Reason: a retroactive requirement would instantly block every verified player on a tightening. A re-check sweep is a residual.
4. **`into_open_market` depends on the application's market evaluation** (transaction-local setting `app.residence_into_open_market`); with the mode in force and the setting missing the DB REFUSES the determination. The DB cannot evaluate the operating-market ladder. `all` has no such dependence and is the stricter assurance. A determination into a market that opens later is not re-approved under `into_open_market`.
5. **Tenant scope only for the residence policy and approvals** (compliance; tenant_admin reads). No `platform_acting` path: it would widen the A-16 acting-setter allow-list (`internal/db/acting_setter_static_test.go`). Platform staff operate a platform-licensed tenant through that tenant's compliance accounts.
6. **"Pending is not evidence" is enforced in the single evidence read** (`kyc.ReadMarketResidenceEvidence`, pinned by a static test) and the DB enforces everything about the check row: creation by an AFTER trigger on `kyc_verifications` (cannot be skipped by a writer), immutability, approval by a different staff AND Person, signed proof, the CHECK-level governance of the policy. It is not a table constraint on `kyc_verifications` itself.
7. **AML worker has no system actor and never places a hold.** Matches wait for an officer (`needs_officer`), the withdrawal gate holds meanwhile, and the officer's `RecordScreening` under the request's idempotency key places the real hold with quorum and proof. This avoids editing `actor_proof_require` (also edited by 0180's lineage) and `aml_holds`' proven-actor guards.
8. **Gate scope: withdrawal and payout only**, inside `aml_hold_active`. Deposit/stakes unchanged. The public rendering stays the indistinguishable `account_review`.
9. **KYC policy routes accept only `cumulative_deposit` and `play`**, and expose no "deactivate" for an active policy: the database refuses it by design (0103). "Deactivate" = supersede with an activated successor (three distinct administrators across create/activate/supersede cases). A never-activated draft can be withdrawn.
10. **New permissions:** `kyc_enforcement_policy:manage|read` (platform_admin only), `residence_approval_policy:manage` (compliance), `:read` (compliance, tenant_admin). Approving a determination reuses `verification:review` (compliance).

## 4. Security and review requests

- `security` (BLOCKING): migration 0181 in full (new definer functions, guard triggers, the `aml_hold_active` replacement, the `kyc_verifications` triggers); the worker API being proof-less and session-tenant bound (a runtime-credential holder with a tenant session could call `aml_screening_request_finish`, as with the KYC outbox; system-actor attestation is F1 and NOT built); the transaction-local market declaration setting; the acting-setter and signer allow-lists.
- `ledger-finance`: `aml_hold_active` now answers true for `withdrawal`/`payout` in more cases (only when a tenant has configured a gating trigger policy). It is the same gate point and the same fail-closed error path (`EvalFailed` on any error). No ledger code changed.
- `architect`: decision 1 (not a licensing_change kind) and the ADR text below.
- `qa`: the integration tests in section 7 have never been executed.

## 5. Proposed ADR / registry text (for the orchestrator; no ADR or registry was edited)

**ADR 0096 amendment (KYC enforcement policy authoring).** KYC enforcement policies are authored through platform_admin routes (`/v1/admin/kyc/enforcement-policies`), each write step-up critical with a reason code and request evidence on the audit row. The database remains the authority on four-eyes (creator versus activator), on the unwired trigger types and on supersession; the routes add no rule. The platform ships no policy and no threshold. L-20 (threshold VALUES, HD-KYC-1..8) stays an owner/legal input.

**ADR 0116 amendment (automatic screening triggers, partial).** A governed trigger policy per tenant and licensing jurisdiction selects which events (registration, kyc_approval, scheduled) enqueue an automatic screening and whether withdrawal/payout is held while a required screening is outstanding. The queue is the durable record; the unique key (tenant, idempotency_key) is the idempotency control. The worker never clears a match and never places a hold; a match waits for an officer. F1 (system actor), F2 beyond withdrawal/payout, S4 (monitoring) and S5 stay NOT IMPLEMENTED. Row for the registry: "A7-F1/F2 triggers + gate (withdrawal/payout): PARTIALLY IMPLEMENTED, MOCK provider; monitoring NOT IMPLEMENTED".

**New ADR candidate (residence determination maker-checker, answers Q21 mechanism, not value).** A tenant mode `none | into_open_market | all` decides whether a reviewer's residence determination needs a second distinct Person before it is market evidence. Default `none`. Changes to a stricter mode are one actor; to a less strict mode need two approvals by distinct Persons. The mode is snapshotted per determination. Pending determinations are not evidence. Owner decision still needed: the mode to configure per tenant (Q21 recommends (b) into_open_market).

**Registry rows.** G1: PARTIALLY IMPLEMENTED (triggers/queue/worker/gate on MOCK; monitoring and system actor open). G8: IMPLEMENTED mechanism, mode is owner configuration. L-20: IMPLEMENTED routes, thresholds remain an owner input.

**Proposed launch-readiness INFO items (NOT added; they change `launchgov.SnapshotVersion`):** `R-RESIDENCE-MAKER-CHECKER`: INFO, acknowledgeable, detail `mode_none` when a tenant's effective mode is `none` ("a single reviewer's residence determination is market evidence"), passing otherwise; and an addition to `R-AML-SCREENING` detail: `trigger_policy_absent` when a screening policy exists with no trigger policy ("screening is officer-initiated only"). Reader functions to extend: `residence_policy_mode(tenant)` and `aml_screening_trigger_policy_effective(tenant, jurisdiction)`.

## 6. Existing assertions changed (none weakened or deleted; every change is an addition or an updated count)

- `internal/httpserver/staff_route_golden_test.go`: +10 rows (5 KYC policy, 5 residence). `staff_route_static_test.go`: route pin 239/143 to 249/150. `staff_stepup_integration_test.go`: coverage pin critical 70 to 76, none-mutating 43 to 44, read 96 to 99.
- `internal/actorproof/stepup_test.go`: registry pin +2 critical (`residence_policy:approve`, `residence_determination:approve`); protective list +`residence_policy:set`. `actorproof_test.go` signer allow-list +`internal/residenceapproval`.
- `internal/aml/static_test.go`: SQLSTATE class AM also allowed in `0181_*` (it re-issues `aml_hold_active`).
- `internal/aml/adversarial_integration_test.go`: `amlTables` +2, new `amlStateMachineTables` exception (the queue is a guarded state machine: DELETE/TRUNCATE still AM005, UPDATE goes through its guard). `catalog_integration_test.go`: `apiRuntime` +6, `noProof` +4, state-machine branch for the trigger assertion.
- `internal/tenant/status_readers_static_test.go` + `docs/security/launch-status-readers.md`: the worker is a classified `A` reader of `tenants.status`.
- `deploy/init-app-role.sql`: grants for the new API functions and the re-asserted table revokes.
- Unchanged by design: `launchgov.SnapshotVersion`, `operatingmarket`, licensing change kinds, ADRs, governance files.
- **Merge note with lic9 (operating bases, 0180):** expect trivial textual conflicts in the same pin counts (golden table, `staff_route_static_test.go`, `staff_stepup_integration_test.go`), `internal/httpserver/server.go` (Deps + route registration), `cmd/platform-api/main.go`, and the migrations directory; re-add the counts after merging. 0181 must run after 0180 and touches neither of its functions.

## 7. Commands for the owner (nothing here has been run)

```
cd /home/user/igaming-platform/.claude/worktrees/lic10
gofmt -l cmd internal; go vet ./... && go vet -tags integration ./internal/... ./cmd/...
go test ./...                                   # unit and static; green at the tip
export PRIV_DB=lic10$(date -u +%H%M%S)p$$x01 PRIV_SRC=$PWD
bash /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/priv_db.sh
bash /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/priv_test.sh -tags integration -count=1 -p 1 \
  ./internal/db ./internal/alerting ./internal/aml ./internal/kyc ./internal/httpserver ./internal/marketgate ./internal/launchgov \
  -run 'Migration0181|ResidenceMakerChecker|ResidenceApprovalPolicyAPI|KYCEnforcementPolicyAPI|TriggerPolicy|RegistrationTrigger|Worker_|Gate_|Queue_|Catalog|Runtime_NoAccess|AppendOnly|StepUp_GoldenTable|KYCResidence|TestMigration'
# then the full suites of ./internal/aml ./internal/kyc ./internal/alerting ./internal/httpserver without -run, and `go test ./internal/db`.
```
Expected first-run risks (SQL never executed): plpgsql syntax or type slips in 0181; the audit insert in `residenceapproval` (tenant principal session); `financial_actor_session()` shapes in the guards; the `aml_hold_active` replacement versus the catalog pins; the `TestKindParity` registry parity for `aml.screening_attention`; `scratchThroughVersion(…, 181)` with the 0180 gap in this worktree.

## 8. Residuals

1. All integration tests unrun (above). 2. Launch-readiness INFO items (section 5). 3. No HTTP/Back Office for the AML trigger policy and the request list (service only). 4. System-actor attestation for the worker API and system hold placement (ADR 0116 F1). 5. First-deposit and withdrawal-request triggers; transaction monitoring and rules. 6. Residence: per-jurisdiction override, platform floor, re-check sweep after tightening, Back Office, `into_open_market` re-approval when a market opens later. 7. KYC policies: Back Office; `edd_amount`/`registration_tier` wiring; per-asset aggregation. 8. Real screening vendor and a per-tenant subject-key resolver (PROVIDER DEPENDENT); alert delivery to a person (ALERT-DELIVERY-1 open: `aml.screening_attention` pages nobody by default). 9. Enabling `gate_withdrawal` for a tenant with no working provider holds every withdrawal after the grace: do not enable it for a production tenant before a contracted provider exists.

# A13b: operational runbook completeness audit and the AWS staging deployment plan

Date: 2026-10-16. Author: `devops`. Branch `a13-runbooks` (base `origin/claude/focused-wright-jw88w9`, head `5317641`). Docs only, plus one tiny command fix
(`deploy/aws/scripts/deploy.sh seed-admin`, section 3). Not pushed, not merged. The task registry, HANDOVER, progress, active-stage, the decision register and the
ADRs were NOT edited; proposed text is in section 5. Method: every route, permission, env var, migration refusal code, script flag and file reference named in a
runbook was checked against the code at this commit by reading it (no build or test was run; no database, AWS or Terraform command was run; `terraform` is not
installed here). Statements that depend on a real provider or an owner decision are labelled in the runbooks themselves.

Finish line used: completion audit `docs/plans/platform-completion-audit-2026-10-14.md` section 6 (technically production-ready and provider-integration-ready),
item 4 (runbooks for alert response, reconciliation drift, kill switch, backup/restore, staff MFA; restore drill with data), plus the new items A5, A6, A8, A9, A11, A12.

## 1. Required runbooks: status

Status legend: OK = existed and matches current code; updated = existed, stale parts corrected; written = did not exist; depends-on-owner = complete as far as software
allows, remaining steps need an owner decision or a real provider (labelled inside the runbook).

| # | Required runbook | Status | Location | Verification notes |
|---|---|---|---|---|
| 1 | Deploy / rollback | updated | `operational-runbooks.md` sections 1 and 2; `stage-9-4-staging-lifecycle-runbook.md`; `aws-staging-deploy-plan.md` | `cmd/migrate` flags and the `down` guard checked (`up`, `down`, `status`, `verify`; `-dir`, `-steps`). Added `migrate verify` after `up` and the rule that new required variables precede the rollout. Staging lifecycle runbook got a banner: its teardown text predates A12 and the stack misses required variables (plan section G). Production deployment procedure itself remains generic (hosting is not chosen; ADR 0009) |
| 2 | Migrations / expand-contract | updated | `expand-contract-migrations.md`; `migration-0101-payment-attempts-remediation.md` | Added the reserved-version tolerance of `verify` (`reservedUnusedVersions` 143-150, 159, 161, read from `internal/db/migrate.go`) and the data-bearing down refusals of 0154/0158/0162/0163 (`AM099`, `RL099`, `WD097`, `MR099`, each confirmed in the down files). Policy is still reviewer-enforced (no linter) |
| 3 | Secrets / credential rotation | written (index) + OK (details) | `secrets-and-credential-rotation.md`; `operational-runbooks.md` section 15; `staff-mfa-keys.md`; `payout-instrument-keys.md`; `production-configuration-checklist.md` | The rotation knowledge was scattered over five documents; the index lists every secret family with its env var, overlap rule and procedure. Config rules verified in `internal/config` (min 32 bytes, family distinctness, placeholder refusal, `ValidateSecretBackendScheme`). Production-checklist gap fixed (section 2 below). Real provider/webhook secret rotation: PROVIDER DEPENDENT. Rotation owner and calendar: depends-on-owner |
| 4 | Incident response and on-call | depends-on-owner | `incident-response-and-oncall.md` | A template with every contact `<NOT ASSIGNED>` (HD-PRH2-4-OPS, ALERT-DELIVERY-1). Symptom table extended with the A5, A6, A8, A11, staff and secrets runbooks. Cannot be completed by software: the rota, the pager and the contacts are owner inputs |
| 5 | Backup / restore / PITR | OK | `backup-restore-dr.md` (A12), `backup-and-disaster-recovery.md`, `restore-from-backup.md` | Script names in `deploy/backup/` exist; the staging steps reference only existing files. Local measurements only; managed RDS PITR is the staging deploy's job. The local drill was last recorded at migration head 157 (schema is now 163): re-run is plan precondition B4. `baseline.sql` hashes every public table generically, so the new tables are covered without a code change |
| 6 | Disaster recovery | OK + depends-on-owner | `backup-restore-dr.md` section 4, `backup-and-disaster-recovery.md` | Full loss, operator error (PITR) and single tenant are covered. No replica, no cross-region copy, no scheduled backup, RPO 0 not met: all labelled NOT IMPLEMENTED / PROVIDER DEPENDENT in those files. Targets (RPO/RTO) are an owner statement |
| 7 | Kill switch | written | `kill-switch-and-containment.md`; `payment-kill-switch-release-recovery.md` | Only the wedged-release recovery existed; engage, release and the other "stop now" controls had no runbook. Verified: routes, permissions (`payments_kill_switch:engage|release|read`), step-up classes (engage none, release critical), body fields and the `deposit|payout|*` validation in `payments_kill_switch_handlers.go`; casino game routes and permissions in `casino_routes.go`; A3 game-kill behaviour from `docs/plans/a3-casino-record.md` |
| 8 | Alert delivery and routing | OK | `alert-delivery-channels.md`, `observability-and-alerting.md` | Every `ALERT_*`/`METRICS_*` variable checked in `internal/config/observability.go`. Honest status already stated (email MOCK, no route seeded, ALERT-DELIVERY-1 open). One code finding: `R-ALERTS` now passes on vocabulary (section 4, F1) |
| 9 | Reconciliation failure response | updated | `operational-runbooks.md` section 3, `observability-and-alerting.md` section 4 (`sweep_failed`, `run_failed`, A11 phases, stuck cursor) | Corrected "tenant listing failure is log-only" (A9 made it a durable P1) and added the read routes. A11 stuck-cursor runbook and `RECONCILIATION_PAYMENT_STATEMENT_MAX_LINES` checked against the code and the A11 record. The stuck-cursor SQL needs the tenant scope set (RLS); the runbook says read-only, no edits |
| 10 | RG enforcement activation (A6) | written | `rg-enforcement-activation.md` | Routes, permissions (`rg_config:write` = compliance only), critical step-up, 409 precondition, one-way switch, migration 0158 down refusal and the OpenAPI text checked. Floor VALUES: depends-on-owner/compliance (OQ-2). Session-duration limit: NOT IMPLEMENTED |
| 11 | AML officer operations | updated | `aml-officer-bootstrap.md` | Header said "no route"; since A7 there are 11 routes and a Back Office queue (the "Working the queue" section already existed). Header and limits corrected. Real screening vendor, monitoring (S4), SAR (S5): NOT IMPLEMENTED / PROVIDER DEPENDENT. Legal decisions HD-AML-*: depends-on-owner |
| 12 | Staff lifecycle and MFA recovery | written + updated | `staff-lifecycle-and-mfa-recovery.md`; `staff-mfa-s3-cutover.md`; `staff-mfa-keys.md` | The S3 cutover runbook still said "no MFA reset until S6"; S5/S6 are merged. New runbook built from `staff_lifecycle_routes.go`, `staff_auth_routes.go`, `cmd/seed-admin/main.go` flags and the ADR 0119 S5/S6 contract. Fixed the seed-admin command lines (`-operator`/`-reason` are required in every mode). Production onboarding needs a real email provider (PROVIDER DEPENDENT); no staff password-reset route (NOT IMPLEMENTED) |
| 13 | Market blocking configuration (A8) | written | `market-blocking-configuration.md` | Gate behaviour from `internal/marketgate` and the A8 record; there is NO route or console to write market rows (verified: no `operatingmarket` handler) so the runbook describes the reviewed Go call, the authorisation-reference rule and the emergency disable. Staging needs `MOCK_GEOLOCATION_COUNTRY` and rows (not in the Terraform). Country content and MKT-DUAL-1: depends-on-owner/compliance. Real locator: PROVIDER DEPENDENT |
| 14 | Launch / brand activation (four-eyes) | updated | `launch-and-suspension.md` | Routes match `launch_authorisation_routes.go`. Added `R-AML-QUORUM`, `R-AML-SCREENING`, `R-AML-MONITORING`; rewrote `R-ALERTS` (finding F1). Legal evidence, licence validity, attestation: depends-on-owner (class E) |
| 15 | Scratch-DB / test hygiene (TEST-HYGIENE-1) | written (policy) | `test-database-hygiene.md` | No runbook existed; the analysis lived in a not-executed deletion plan. Policy and read-only inspection written from `internal/testsupport/scratchdb`. The durable reaper is NOT IMPLEMENTED (code change, proposed in the runbook); cleanup of existing databases is owner-authorised |
| 16 | Observability / dashboards | OK + depends-on-owner | `observability-and-alerting.md`, `deploy/observability/` | Rules, metrics and local scrape example exist and are honestly labelled (NOT DEPLOYED, log-event rules not wired). **Dashboards do not exist** (no dashboard file in the repository); a metrics backend is a hosting decision (PROVIDER DEPENDENT / owner) |
| 17 | Payout reversal and amount dispute (A5) | written | `payout-reversal-and-amount-dispute.md` | Route, body, quorum, outcome values, refusal mapping from `payout_reversal_handlers.go` and `payments/payout_reversal.go`; M2 basis rule from `manual_resolution.go`; negative-balance rule from the A5 record. Stale "WITHDRAWAL-REVERSAL-1 is required first / PAYOUT-AMOUNT-DISPUTE-1 open" lines in `operational-runbooks.md` corrected. Signed-actor-proof gap R1 and partial returns: open (security/owner) |
| 18 | Statement-source registry / stuck cursor (A11) | OK | `observability-and-alerting.md` section 4; `operational-runbooks.md` sections 14 and 16 | Registry, phases (`no_statement_source`, `source_not_eligible`, `scope`, `cursor_anomaly`, `truncated`), bound variable and gauge names match the A11 record. Real statement sources: PROVIDER DEPENDENT. Pointer added to the incident table |
| 19 | AWS staging deployment plan | updated (finished) | `aws-staging-deploy-plan.md` | See section 3. PREPARED, NOT AUTHORISED, NOT EXECUTED |

Also reviewed and left unchanged: `payout-instrument-keys.md`, `staff-mfa-keys.md`, `production-startup-database-config.md`, `plat-rolesplit-staging-verification.md`,
`load-and-soak-test-plan.md`, `self-hosted-runner.md` (the completion audit's D-CI recommends dropping the Mac-runner plan; if the owner accepted that, the orchestrator should retire it), `developer-handover.md`
(contains older migration counts; it is a hand-over, not an operations runbook).

## 2. Documentation defects found and fixed (docs only)

| Where | Defect | Fix |
|---|---|---|
| `production-configuration-checklist.md` | Missing the variables `PAYOUT_INSTRUMENT_*` (production-required), `PROVIDER_CREDENTIAL_FINGERPRINT_KEY`, `SECRETSTORE_BACKENDS`, `AWS_SECRETSMANAGER_REGION`, `SECRETSTORE_DEVFILE_ROOT`, `WEBHOOK_ADMISSION_*`, `WEBHOOK_RL_PER_IP_*`, `RECONCILIATION_PAYMENT_STATEMENT_MAX_LINES` and a pointer to the A9 variables (found by diffing every `os.Getenv` name in `internal/config` against the checklist) | new section with required/default/secret facts read from the code |
| `operational-runbooks.md` | five stale statements: STAFF-LIFECYCLE-1 "not implemented" (twice), K3 "not implemented" in the sweeper section, WITHDRAWAL-REVERSAL-1 "required first" (twice), PAYOUT-AMOUNT-DISPUTE-1 "open", tenant listing failure "log-only" | corrected with references |
| `staff-mfa-s3-cutover.md` | "no MFA reset / break-glass software controls until S6"; seed-admin command lines missing the required `-operator` and `-reason` | banner and corrected commands |
| `aml-officer-bootstrap.md` | "no HTTP route" | corrected (A7) |
| `launch-and-suspension.md` | `R-AML-*` rows missing; `R-ALERTS` described as always failing | corrected, see F1 |
| `stage-9-4-staging-lifecycle-runbook.md` | teardown text ("no final snapshot") predates A12 | banner |

## 3. The code or script fix made

`deploy/aws/scripts/deploy.sh`, function `cmd_seed_admin`: the container command override `["/app/seed-admin","-email",$email,"-create-person"]` omitted
`-operator` and `-reason`, which `cmd/seed-admin` requires in every mode (ADR 0119 S5+S6 review SR-1; `operatorContext.validate(true)`), so `deploy.sh seed-admin <email>`
would have failed at the first staging run. Added `"-operator","deploy.sh","-reason","staging-bootstrap"`. `bash -n` passes; not executed against AWS. No test pins that line
(searched `deploy/aws/tests`).

## 4. Findings the orchestrator should act on (not fixed here: code or infrastructure scope)

| # | Finding | Evidence | Suggested owner / action |
|---|---|---|---|
| F1 | **`R-ALERTS` overstates.** Since A9 added the `email` and `webhook` kinds, `alertsCheck()` passes because `knownHumanChannelKinds` contains them, with no route, no real email provider and nobody paged. The runbook said "always fails today"; it no longer does, and a launch approver could read a pass as "alerts reach a person" | `internal/launchgov/readiness.go` `alertsCheck`, `internal/alerting/delivery_state.go` `knownHumanChannelKinds` | `alerting`/`launchgov` + `security`: derive the check from an enabled, approved route whose sink is `HumanNotification()` in this process. Until then the runbook warns readers |
| F2 | **Staging stack cannot start the current binary.** `deploy/aws/modules/ecs` sets no `STAFF_MFA_*` (required in every environment), `PAYOUT_INSTRUMENT_*`, `ACTOR_PROOF_*`, `MOCK_GEOLOCATION_COUNTRY`, `METRICS_ADDR`, `ALERT_*`; the seed-admin task lacks the MFA keys too; no task inserts the `actor_proof_keys` row or the operating-market rows (no route exists); no task runs `migrate verify` | `deploy/aws/modules/ecs/main.tf` task definitions vs `internal/staffmfa/startup.go`, `internal/config`, A8 record | `devops` + `security`: plan section G (STAGING-PREP-1) before the SHA is pinned |
| F3 | `role-init` task runs `psql -v ON_ERROR_STOP=1 -f` without `--single-transaction`, although `operational-runbooks.md` section 15 requires it ("Terraform not changed by this branch") | `deploy/aws/modules/ecs/main.tf` role-init command | `devops`: add the flag in STAGING-PREP-1, or accept (script is idempotent; the plan says re-run on error) |
| F4 | The local backup drill's last recorded run was at migration head 157; head is 163 | `docs/plans/a12-backup-drill-record.md`; `migrations/` | re-run `deploy/backup/drill.sh` on the final commit (plan precondition B4). `baseline.sql` is generic so the new tables are hashed; the drill's explicit coverage list does not name `rg_enforcement_activations` or the 0162/0163 objects (optional addition) |
| F5 | Terraform for A12 and every STAGING-PREP-1 change are unvalidated (`terraform`, `shellcheck` absent in this environment) | A12 record | first step of the owner-authorised run, plan 0.2 / B7 |
| F6 | ADR 0122 section 10 (exclude `actor_proof_keys.secret` from logical dumps) is still an open architect decision | ADR 0122 | `architect` |
| F7 | A5 residual R1: `reverse` decisions are not covered by the ADR 0110 signed actor proof (security HIGH before real money) | `docs/plans/a5-payout-reversal-record.md` | `security`/`ledger-finance` before any real PSP |

## 5. Proposed text for the single writer (nothing below was applied)

### 5.1 Proposed ADR 0123 (name only; the architect numbers it): "The single AWS staging deployment: authorisation, preconditions and scope"

Context: owner decision 8 (2026-10-15) authorises exactly one named-commit AWS staging deployment after the A-class software work. Decision: the deployment is
governed by `docs/runbooks/aws-staging-deploy-plan.md`. (1) Nothing is executed until the owner completes the authorisation record (section A of the plan) in their own
message; an agent message never authorises it. (2) Staging only (`APP_ENV=staging`), the single account pinned in Terraform, `eu-central-1`, synthetic data, all providers MOCK,
no production credential or customer datum. (3) Preconditions B1 to B8 (named SHA, hosted CI green on that SHA, final local verification recorded, local drill at the current head, security
review of the infrastructure delta, static Terraform checks, inputs supplied). (4) STAGING-PREP-1 (plan section G: staff MFA, payout instrument and actor proof keys, market configuration, migrate verify,
backup runner and alarms, metrics/alert webhook) is merged and reviewed before the SHA is pinned, or each exclusion is signed by the owner. (5) The run verifies roles, `search_path`, secrets, permissions, migrations,
PITR to a timestamp, logical backup/restore on RDS, observability and alert delivery, captures evidence in `docs/plans/`, and ends with teardown including deletion of the final snapshot and retained backups.
(6) The result does not establish production readiness, legal readiness, provider readiness or brand activation. Consequences: any STOP condition ends the run and goes to the orchestrator; a second deployment needs a new decision.

### 5.2 Proposed registry rows

| Item | Label | Note |
|---|---|---|
| A13-RUNBOOKS-1 | `IMPLEMENTED` (documents) | runbooks listed in section 1; index `docs/runbooks/README.md`; incident rota and dashboards remain depends-on-owner / NOT IMPLEMENTED |
| STAGING-PREP-1 | `NOT IMPLEMENTED` (blocking the staging deploy) | plan section G, gaps G1 to G5, G7, G8; F2 and F3 above |
| R-ALERTS-OVERSTATES-1 | `OPEN` (finding F1) | readiness check passes on vocabulary, not on a configured human route |
| TEST-HYGIENE-1 | `PARTIALLY IMPLEMENTED` | policy and procedure written (`test-database-hygiene.md`); reaper NOT IMPLEMENTED; deletion of the ~740 databases needs the owner's written authorisation |
| DEPLOY-SH-SEED-ADMIN-1 | `IMPLEMENTED` (fix) | `-operator`/`-reason` added; unexercised against AWS |

## 6. What is owner-dependent or provider-dependent (consolidated)

Owner: authorisation record, AWS account/credential/region/CIDRs, alert recipients and webhook receiver, KMS choice, backup runner placement, backup signing key and fingerprint, ADR 0122 section 10,
market country content and the authorisation reference, RG floor values (with compliance), incident rota and contacts, rotation calendar, RPO/RTO statements, cleanup of scratch databases, teardown consent.
Provider: real email adapter (staff onboarding and alert email), real geolocation locator, real PSP/casino/sportsbook statement sources and credential rotation, AML screening vendor, metrics backend and pager.
Compliance/legal (class E, never blocks technical readiness): jurisdiction floors, AML decisions HD-AML-*, licence evidence and brand attestations.

## 7. Not done, by instruction

No `terraform apply`/`plan`/`init`, no AWS command, no database command, no test run. The staging plan is written so the owner can authorise it; nothing in it was exercised.

## 8. Security review of STAGING-PREP-1 and R-ALERTS

Date: 2026-10-16. Reviewer: `security` (independent of the author). Branch `staging-prep-1`, commits `b65f158` (deploy/aws), `7679706` (launchgov R-ALERTS),
`3cd9d97` (docs), on top of `4f4164d`. **Verdict: APPROVE for merge, with conditions.** No Critical or High finding. SRSP-1 and SRSP-2 must be closed, or
signed as exclusions by the owner, before the staging run is executed (plan precondition B6 and the go/no-go list). They do not block the merge. SRSP-3 to SRSP-8
are recorded residuals.

### 8.1 What was checked, and how

- Read the full diff of `deploy/aws/modules/{secrets,ecs,iam}` and `environments/staging`, plus `deploy/aws/sql/init-runtime-role.rds.sql`,
  `deploy/aws/bootstrap` (permissions boundary `StagingAppSecrets` = `igaming-staging/*`), `deploy/aws/scripts/deploy.sh` (role-init then migrate on every
  `up`/`migrate`) and `modules/database` (logging parameters).
- Key formats compared against the parsers: `internal/staffmfa/keys.go` (`ParseKeySet`, strict std/url base64, >= 32 bytes, HKDF to 32-byte subkeys),
  `internal/actorproof.ParseKeySet`, `internal/config` (`validatePayoutInstrumentKeys`, `validateActorProofKeys`, `validatePlatformKeyFamilies`) and
  `internal/payoutinstrument.NewKeys` (HKDF; the 32-byte AES check applies to the derived subkeys). Checked empirically too: a throwaway test, injected with
  `go test -overlay` so the worktree was not changed, ran `config.Load` 200 times with `APP_ENV=staging` and freshly generated `stg-v1:<64 alphanumeric>` values for all
  five families, `METRICS_ADDR=127.0.0.1:9464` and `ALERT_RETENTION_DAYS=30`. All loaded. Each value decodes to 48 bytes, and the staff MFA and actor-proof
  constructors accept them.
- `deploy/aws/tests/run-static-checks.sh` re-run offline (terraform 1.11.4, local provider mirror, mock providers, no credentials). All checks passed, including
  `secrets` 8/8 (`secret_string == null` for every key-family version). Nothing was planned or applied, and no AWS command was run.
- Go: `go vet` and the unit tests of `launchgov`, `alerting/...`, `config`, `staffmfa`, `actorproof`, `payoutinstrument` and `cmd/platform-api` pass. Integration
  tests ran on a private scratch database that was dropped afterwards: `launchgov` `TestReadiness_Alerts_*` (3/3 PASS, confirmed with `-v`) and `alerting/...` pass.
- Grep of the diff and of `deploy/aws` for secret values (key-shaped literals, `AKIA`, private-key headers, `stg-vN:<body>`) found none. The only literal is the
  pinned staging account id in a tftest ARN, which is not a secret.

### 8.2 Answers to the review questions

1. **Secrets kept out of state and logs: yes.** All six key bodies are `ephemeral "random_password"` values written only through `secret_string_wo`, and tftest
   asserts `secret_string == null`. The outputs carry ARNs, the kid and the `awssm://` reference (ARN plus versionId), which are not secret. The task definitions
   inject the keys with `valueFrom`, so a value never appears in the task definition. The kid is the only non-secret part. `key_family_version` is separate from
   `secret_version`, so a DB or JWT rotation does not orphan MFA factors or sealed instruments.
2. **Key formats are valid.** The kid `stg-v<N>` matches every kid regex. The kid variables all come from the same `module.secrets.key_family_kid` output that is
   embedded in the secret values, so they cannot diverge. The five bodies are independent ephemeral values, so the cross-family distinctness rules hold.
   The webhook HMAC (64 raw bytes) is at or above `webhookMinSecretBytes`, and the `awssm://` reference matches `secretstore.awssmRef`.
3. **IAM is acceptable for staging.** The service execution role gains three named ARNs and the one-off role gains only `staff-mfa-keys`, which seed-admin needs.
   The one-off role still cannot read the JWT secret, and the service role still cannot read the RDS master secret. Execution-role credentials are never exposed
   to containers. The task-role grant is a single `GetSecretValue` on one exact ARN, created only when `alert_webhook_url` is set, and capped by the boundary.
   SRSP-3 covers the shared-role residual.
4. **Migrate and role-init.** `--single-transaction` is correct: everything in the script is transactional, and the `\o /dev/null` password-suppression still
   works. The migrate chain has a new ordering gap, SRSP-2. A drift that predates this change and is not part of it is SRSP-1.
5. **Metrics are not exposed.** `METRICS_ADDR=127.0.0.1:9464` binds loopback inside the awsvpc task. `portMappings` lists only `container_port`, and there is
   no target group, security-group rule or `METRICS_ALLOW_NON_LOOPBACK`. Config refuses a non-loopback bind outside development unless that flag is set, and a
   precondition refuses a shared port.
6. **Staging safety holds.** `APP_ENV=staging` is set on platform-api and on seed-admin. Seed-admin has no staging relaxation: placeholders are refused
   everywhere outside development. All providers are MOCK. `PROVIDER_CREDENTIAL_FINGERPRINT_KEY` is not provisioned, so `providercred.New` returns no subsystem,
   which leaves no provider-credential path even though `SECRETSTORE_BACKENDS=awssm` is set. The webhook URL is checked by `egressguard.ValidateURL` and the dial
   control: loopback, private, link-local and so 169.254.170.2, the ECS credential endpoint, are refused outside development. The transport has no proxy, no
   redirects and TLS >= 1.2. Terraform also requires `https://`.
7. **R-ALERTS fails closed.** A missing source gives `no_routing_source`. A never-evaluated dispatcher gives `routing_not_evaluated`, and a stale one gives
   `routing_evaluation_stale`: both are not_evaluable and never pass. A missing severity is a fail. It passes only when p1 **and** p2 each have a latest step-0
   `scope='platform'` route that is enabled, has a recipient and has the DB human flag, and whose sink is wired and reports `HumanNotification()`.
   `EmailSink.HumanNotification()` is `!synthetic`, so email over MOCK cannot pass (integration-tested). `WebhookSink` is false in development posture. Enabling
   a human route needs SR-7 four-eyes (`AR001`, tested). A newer disabled version or an enabled log route at step 0 can only take a pass away.
8. **No tenant-controlled input and no isolation issue.** `Config.AlertRouting` is a process function and takes no `EvalInput`. Routes are read under the
   dispatcher platform-service identity with `scope='platform'`. `alert_routes` is written only under the platform-admin RLS policy, which requires
   `app.tenant_id` to be empty, and only through the 0110/0156 guard triggers. A tenant principal therefore cannot create, enable or influence the routing that
   R-ALERTS reads. The result exposes closed detail codes only, never a recipient.
9. **The SnapshotVersion bump 4 to 5 is right.** The semantics of a check changed, so the version must change. `Hash()` covers the version and every check, so
   any request bound to a v4 hash goes stale. A changed R-ALERTS result also changes the hash (integration-tested). The only pin is
   `readiness_aml_test.go`, now 5. No migration or DB constraint stores the version.

### 8.3 Findings

| # | Severity | Finding and concrete failure scenario | Required action |
|---|---|---|---|
| SRSP-1 | **Medium** (pre-existing, not introduced by this change; defence in depth) | `init-runtime-role.rds.sql` runs `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES ... TO igaming_runtime` and then re-narrows only `schema_migrations` and `actor_proof_*`. The dev script `deploy/init-app-role.sql` also re-narrows `withdrawal_requests`, `sportsbook_bet_settlements`, every `aml_*` table and the three `provider_credential_*` tables. The RDS script has none of them, and neither script re-narrows the other ~26 tables that migrations `REVOKE` from the runtime role (for example `alert_routes`, `ledger_adjustment_*`, `staff_capability_grants`, `financial_approval_policies`). `deploy.sh up`/`migrate` run role-init BEFORE migrate on every invocation, so any re-run against a migrated database widens the runtime role. Re-runs include the documented secret-rotation path and "re-run on error" after migrations. Scenario: after a rotation, the internet-facing runtime role holds UPDATE/DELETE on append-only and four-eyes tables. Today the guard triggers and FORCE RLS still refuse those writes (checked for `alert_routes`, `withdrawal_requests` and the AML tables), so this is a lost independent layer, not a bypass. It does break the runtime-role-separation section 3 model that step 3 of the plan claims to verify. | Before the staging run: either make the script snapshot-and-restore the runtime grants (or re-assert every migration REVOKE), with a test that re-runs **the RDS script** after `migrate up` and diffs `information_schema.role_table_grants` against the post-migration baseline; or add a plan step that runs that diff after every role-init re-run and STOPs on any difference |
| SRSP-2 | **Low** (introduced by this change) | The migrate command is now `up && verify && status && REVOKE`. If `migrate verify` fails after `up` has created `schema_migrations`, which on a first deploy holds DML for the runtime role through the default privileges, the authoritative REVOKE never runs. Before this change only `up` sat in front of the REVOKE. Scenario: a first-deploy checksum or gap failure leaves the runtime role able to write `schema_migrations` until a later successful run. ECS services created by `terraform apply` may already be connecting as that role. | Move the REVOKE directly after `up` (`up && REVOKE && verify && status`), or run it unconditionally while keeping the failing exit code. Update the tftest assertion and plan 6.1 |
| SRSP-3 | Low (accepted for staging) | The task role is shared by all six task definitions, so b2c, backoffice and the one-off tasks can also read the alert-webhook HMAC once `alert_webhook_url` is set. Scenario: code execution in the static-site container yields the HMAC, and with it the ability to forge signed alerts to the owner's receiver. That is integrity of alerts only, with no data access. | Before production: a dedicated platform-api task role, with b2c, backoffice and the one-off tasks on an empty task role |
| SRSP-4 | Low | `alert_webhook_url` is a plain, non-sensitive variable that ends up in tfvars, state, plan output and the task definition, where `DescribeTaskDefinition` shows it. Many receiver products (chat or incident "incoming webhook" URLs) put a bearer credential in the URL path. | Document in the variable and in plan I that the URL must not embed a credential, because authentication is the HMAC. Alternatively mark it `sensitive` and move it to Secrets Manager |
| SRSP-5 | Info | Key-family rotation has no overlap (a `key_family_version` bump invalidates enrolled MFA factors and sealed payout instruments). This is acceptable for disposable staging and stated in the module. The pattern must not be reused for production, where `staff-mfa-keys.md` and `payout-instrument-keys.md` apply. | None for staging |
| SRSP-6 | Info | The `modules/iam/main.tf` file header still says the task role is "intentionally empty". The role description and the new block are correct. | Update the header in the next touch |
| SRSP-7 | Info (residual of R-ALERTS) | A pass means only that a four-eyes-approved platform route for p1 and p2 points at a wired webhook, or a non-MOCK email channel, from this replica's latest evaluation. It does not show that the receiver pages anyone (ALERT-DELIVERY-1 stays OPEN), and routing is platform-wide, not per tenant. Each platform-api replica evaluates independently, so a decision and its execution on different replicas can disagree, but only towards staleness, never towards a pass. | Keep the `launch-and-suspension.md` wording, and judge the staging delivery by receipt (plan 10.2) |
| SRSP-8 | Info | Owner actions B (`actor_proof_keys` row) and C (operating-market rows) handle key material and production-adjacent writes by hand, from a VPC session that does not exist yet (G1). Not building them was correct. Any tool written for them later needs its own security review. | As stated in plan G3 |

### 8.4 Not in scope of this review

Real AWS behaviour (IAM simulation of the deployer and boundary, RDS `\getenv`/psql version, whether Secrets Manager JSON-key injection works on Fargate), the
A12 Terraform beyond what the static checks cover, the backup runner (G1), the alarms (G2), and penetration testing. This review does not declare the staging
stack "secure". It covers the configuration and code paths listed in 8.1 at commit `3cd9d97`.

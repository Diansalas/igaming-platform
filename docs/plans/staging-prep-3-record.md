# STAGING-PREP-3 record: making the one staging deployment executable end to end

Date: 2026-10-16. Author: devops. Branch: `staging-prep-3` (from `claude/focused-wright-jw88w9`). Not pushed, not merged.
Scope: close gap G1 (backup runner) and owner actions B (actor-proof row) and C (operating-market rows) of
`docs/runbooks/aws-staging-deploy-plan.md` with reviewed tooling, inside the owner's approval of ONE staging deployment
(eu-central-1, pinned account, staging only, mock providers, synthetic data).

**Nothing in this change touched AWS.** No `terraform apply/plan`, no `deploy.sh`, no `aws` command was run. No credential,
key or secret was used or written to a file. All evidence below is offline (Terraform with a provider mirror and mock providers) or
against a throwaway local PostgreSQL 16 cluster on a drill port.

## 1. Status labels (CLAUDE.md "no fake completion")

| Deliverable | Label | Evidence / caveat |
|---|---|---|
| `staging-runner` image definition (`deploy/docker/staging-runner.Dockerfile`) | PARTIALLY IMPLEMENTED | `docker build --check` is clean; the image was NEVER built: the Alpine CDN is not reachable from the sandbox, so the package list (bash, curl, jq, coreutils, grep, sed, gawk, findutils, diffutils, openssl, age, postgresql16-client, aws-cli, procps-ng with a `procps` fallback) is unverified against Alpine 3.20. The Go helpers it builds compile on the host. |
| Terraform: ECR repo, 2 task definitions, log group, task role, boundary statement, outputs | IMPLEMENTED (offline) | `terraform fmt`, `validate`, `terraform test` (staging root 13, ecs 22, iam 8, ecr 2 runs; all pass). Never planned or applied. |
| `deploy.sh runner <mode> [KEY=VALUE...]`, `runner-image`, image build in `up` | PARTIALLY IMPLEMENTED | Parameter handling is tested offline (`deploy/aws/tests/runner-overrides-test.sh`, 20 assertions) and shellcheck-clean. `run-task`, polling and CloudWatch log retrieval have never run. |
| Mode `verify-db` | IMPLEMENTED (local) | Run against a throwaway cluster; 38 RESULT records + 3 grant SNAPSHOT lines. Not run on RDS. |
| Mode `seed-actor-proof` | IMPLEMENTED (local) | Idempotent, conflict-refusing, secret-free log, tested locally; not run with the real secret. |
| `cmd/seed-market` + mode `seed-market` | IMPLEMENTED (local + 8 integration tests) | Library calls only; tested on a private scratch DB and the throwaway cluster. |
| Mode `backup-restore-drill` | PARTIALLY IMPLEMENTED | Every step ran locally, including the two-phase restore, `verify.sh`, the drop and the PITR baseline. IAM authentication, TLS `verify-full`, `rds_iam`, `BYPASSRLS` creation on RDS, the `pg_catalog` revoke on RDS and `pgaudit` are PROVIDER DEPENDENT unknowns that only the real run answers. The restore is EXPECTED to be BLOCKED on RDS (section 5). |
| Mode `pitr-verify` | PARTIALLY IMPLEMENTED | Run locally against a physical copy of the cluster on another port; the RDS-clone specifics (master secret following the clone, credential reset) are unverified. |
| Mode `probe-backup-role` | IMPLEMENTED (local) | 15 + 8 probes, classification verified including the un-hardened and "RDS refused" paths. |
| IAM delta | BLOCKED until applied by the bootstrap credential | Section 6. |

## 2. What exists

| Path | What |
|---|---|
| `cmd/seed-market/` | `main.go`, `settings_test.go` (6 unit tests), `seed_market_integration_test.go` (8 integration tests, tag `integration`) |
| `deploy/staging-runner/entrypoint.sh`, `lib.sh`, `lib-probes.sh`, `modes/*.sh` (6), `sql/*.sql` (5) | the runner |
| `deploy/staging-runner/runner_static_test.go` | 10 static guards in `go test ./deploy/...` (no secret exposure patterns, read-only verification pack, bound actor-proof secret, 15 + 8 probes, drill follows the plan, Dockerfile bakes no key, exactly one `rds-db:connect` statement) |
| `deploy/staging-runner/tests/local-harness.sh`, `run-local-modes.sh`, `fixture.sql` | throwaway-cluster harness and the end-to-end local suite (81 assertions) |
| `deploy/docker/staging-runner.Dockerfile` | the image |
| `deploy/aws/modules/ecr` | `staging-runner` repository (immutable tags, scan on push, same pattern) |
| `deploy/aws/modules/ecs` | log group `/ecs/igaming-staging/staging-runner`; task definitions `igaming-staging-staging-runner` and `igaming-staging-staging-runner-actor-proof` |
| `deploy/aws/modules/iam` | `igaming-staging-ecs-runner-task-role` (`rds-db:connect` as `igaming_backup` on this DbiResourceId only) |
| `deploy/aws/bootstrap/main.tf` | boundary statement `StagingRunnerBackupRoleIamAuth` |
| `deploy/aws/environments/staging` | wiring, outputs `ecs_staging_runner_task_definition_arn`, `ecs_staging_runner_actor_proof_task_definition_arn`, `runner_task_role_arn`, `db_resource_id`, `key_family_kid`, tftests |
| `deploy/aws/scripts/deploy.sh` | `runner`, `runner-image`, runner image in `up` phase 2, sourceable for tests |
| `deploy/aws/tests/run-static-checks.sh`, `runner-overrides-test.sh` | the runner scripts are syntax-checked, shellchecked and the parameter handling is tested |
| `docs/runbooks/aws-staging-deploy-plan.md` | G1, G3, 0.4, 0.7, 3.x, 4.2, 5.x, 6.1/6.5, 7.2/7.4, 8.x, section 12 reference the runner; the authorisation record (section A) is untouched |

## 3. Design

### 3.1 Execution model

The RDS instance is private, so every in-VPC action is a ONE-OFF ECS task of the runner image, started by `deploy.sh runner <mode>`:
`aws ecs run-task` with `containerOverrides = [{name: staging-runner, command: [<mode>], environment: [<non-secret parameters>]}]`; the script polls until the task
stops (its own loop, default 3600 s, because `aws ecs wait` gives up after about ten minutes), prints the task's CloudWatch stream, waits (bounded) for the final `RUNNER|END` line and exits
non-zero on exit codes 1, 2 and 3. The image `ENTRYPOINT` is the dispatcher; its default `CMD` is `usage` (prints the modes, does nothing else).

| Exit | Meaning |
|---|---|
| 0 | no `FAIL` (read the `FINDING` / `BLOCKED` lines: they are recorded unknowns, not failures) |
| 1 | at least one `RESULT|...|FAIL` |
| 2 | refused: bad parameter, `APP_ENV` not development/staging, loopback target outside the local harness, missing prerequisite |
| 3 | STOP condition of plan section 11 (RDS refused `BYPASSRLS`, IAM auth unusable, a conflicting actor-proof row, ...) |

Output contract: `RESULT|name|PASS|FAIL|INFO|FINDING|BLOCKED|detail`, `TIMING|name|seconds`, `PITR|...`, `KEY|...`, `SNAPSHOT|...`, `SUMMARY|...`, `RUNNER|START|STOP|REFUSED|END`.
Every detail is one line, bounded, and scrubbed of the registered secret values.

### 3.2 Task definitions and identities

| | `staging-runner` | `staging-runner-actor-proof` |
|---|---|---|
| Modes | verify-db, seed-market, backup-restore-drill, pitr-verify, probe-backup-role | seed-actor-proof |
| Execution role | one-off execution role (ECR pull of the runner repo, secrets, runner log group) | same |
| Task role | `igaming-staging-ecs-runner-task-role`: `rds-db:connect` as `igaming_backup` on this DbiResourceId, nothing else | NONE (no AWS credential in the container) |
| Secrets injected | `DB_MASTER_PASSWORD` (RDS-managed master secret), `DB_RUNTIME_PASSWORD` | `DB_MASTER_PASSWORD`, `ACTOR_PROOF_KEYS` |
| Hardening | uid 10001, all capabilities dropped, init process, 1 vCPU / 2 GiB, 20 GiB ephemeral storage | uid 10001, caps dropped, 0.25 vCPU / 0.5 GiB |

Passwords arrive as `DB_*_PASSWORD`, never `PGPASSWORD`: the runner copies them into a 0600 libpq passfile on tmpfs and `unset`s them before any child process starts. No connection string
carries a password; psql, `cmd/migrate` and `cmd/seed-market` read the passfile.

### 3.3 Modes

| Mode | Parameters (all non-secret) | Writes |
|---|---|---|
| `verify-db` | `EXPECT_BACKUP_ROLE` present/absent/any, `GRANT_LISTING` 0/1 | nothing (plain SELECTs; `migrate verify` is read-only) |
| `seed-actor-proof` | none | one `actor_proof_keys` row, only when the kid is absent |
| `seed-market` | `TENANT_SLUG`, `BRAND_SLUG`, `COUNTRY`, `AUTH_REF`, `OPERATOR`, `REASON`, optional `ACTOR_ID`, `DRY_RUN` | one licence-ceiling version, one tenant policy version, and the audit rows the library writes |
| `backup-restore-drill` | `DRILL_RESTORE_DB`, `SKIP_RESTORE`, `PRINT_EXPECTED` | see 4 |
| `pitr-verify` | `PITR_ENDPOINT`, `EXPECTED_DIGESTS`, `EXPECT_ANCHOR` | only on the disposable clone, see 4.3 |
| `probe-backup-role` | `EXPECT_HARDENING` applied/refused/unknown | nothing (every probe is `BEGIN READ WRITE; ...; ROLLBACK`) |

`deploy.sh` accepts only the parameter names listed for the mode, values from `[A-Za-z0-9._:/@#=,;+ -]`, refuses names containing `PASS`, `SECRET`, `TOKEN`, `PRIVATE`, `CREDENTIAL` or `KEYS`, and
caps the total size (container overrides are limited to 8192 characters). The runner re-validates every parameter it uses with its own strict regex.

### 3.4 What each mode cannot do

None of them can: write a ledger, balance, player or staff row; read a secret other than the ones injected; change a role other than as listed in 4; connect to anything but the pinned RDS endpoint
(host must be an `eu-central-1` RDS endpoint on port 5432; `pitr-verify` accepts only `igaming-staging-*` endpoints other than the primary); run when `APP_ENV` is not `staging`.
Loopback targets exist only behind `RUNNER_ALLOW_LOCAL=local-test-harness` and ports 5497-5499; `deploy.sh` cannot set that variable and the task definition does not.

## 4. The backup/restore drill in detail

### 4.1 Steps (each is a `RESULT` record)

1. Preflight: OpenSSL >= 3, tool versions, RDS CA bundle downloaded from `truststore.pki.rds.amazonaws.com` (SHA-256 and certificate count logged, `sslmode=verify-full` everywhere), master login, environment anchor.
2. Provision `igaming_backup` as the master: `init-backup-role.sql` with `BACKUP_ROLE_IAM_ONLY=1`, `GRANT rds_iam`, `pgaudit.log` role setting when pgaudit is loaded. **If RDS refuses the role: FINDING + STOP (plan 5.1).** First IAM login with a token is checked (`backup_role_login`); failure is a STOP.
3. `harden-backup-surface.sql` as the master; if RDS refuses the `pg_catalog` revoke: FINDING, the two `REVOKE CONNECT` statements are retried alone, and the abuse probes that then succeed are FINDINGs (residuals).
4. The attribute check and the 23 probes (section 8).
5. Ephemeral Ed25519 signing key and age keypair in `${scratch}/keys`; fingerprint pinned from this run's log.
6. `backup.sh` with `BACKUP_IAM_TOKEN_COMMAND='aws rds generate-db-auth-token ...'`, `verify-full`, age recipient and signing key; checks: manifest `"encryption": "age"`, signature file, no plaintext artefact; prints `SHA256SUMS` hash.
7. Restore, two-phase (`RESTORE_CONFIRM`, `RESTORE_EXPECT_SOURCE_ENVIRONMENT`, `RESTORE_TARGET_ENVIRONMENT`, `RESTORE_EXPECT_SET_SHA256`, pinned public key): `--phase provision` as the master, a disposable `igaming_restore_loader` login (NOSUPERUSER, member of the owner role, SCRAM verifier computed in the task, 2-hour validity), `--phase load` as that login, `verify.sh` (token passfile for the backup role), `DROP DATABASE igaming_restore_check` (also on any exit path), loader role dropped.
8. PITR baseline: `baseline.sql` as the backup role inside ONE `REPEATABLE READ READ ONLY` transaction; `T` = that transaction's `now()` (the snapshot follows microseconds later; keep the system quiet); 13 per-kind digests (sha256 prefix of the sorted lines) and the environment anchor are printed; `PRINT_EXPECTED=1` also prints every line as `EXPECTED|...`.

### 4.2 Documented staging deviations (each is deliberate and visible in the log)

1. **Ephemeral keys.** The signing key and age keypair live in the task's storage and die with it; the fingerprint is pinned from the same run's log. This proves sign, encrypt, verify and decrypt; it does NOT prove out-of-band pinning (ADR 0122 section 5), and the backup set is destroyed with the task (no durable backup is produced). The key directory is separate from the backup directory (lib-sign.sh enforces it).
2. **Restore target environment.** `restore.sh` requires target environment = source environment, so the drill passes the source's own anchor (`unrecorded` unless 6.2 recorded `staging`), not the plan's literal `staging`.
3. **PITR baseline by digest, not by `expected.txt`.** A task cannot carry the baseline file (several thousand lines) to a later task through an 8192-character override. `pitr-verify` receives per-kind digests, compares them with the clone's own `baseline.sql` output, and gives `verify.sh` the clone's own baseline for the structural gates (invariants, `deploy/drill/verify.sql` as owner, `migrate verify`, `restorecheck`); the line-by-line baseline gates are therefore skipped there (they would be trivially equal). Sequence values are not compared with the source. The full `expected.txt` is available as `EXPECTED|` records for a human diff.
4. **Clone credential reset.** The runner's task role may mint tokens only for the primary's DbiResourceId. On the disposable PITR clone the mode revokes `rds_iam` from the clone's copy of `igaming_backup` and sets a 2-hour password credential (client-side SCRAM verifier). The primary is never touched.
5. **Master/owner.** The same master credentials are used on the clone; if the clone does not accept them (plan 7.3: "unverified: whether the managed master secret follows") the mode STOPs and records it.

### 4.3 Mutations by the drill (complete list)

On the primary: create/alter `igaming_backup` (+ `rds_iam` membership, `pgaudit.log` setting); `harden-backup-surface.sql` (REVOKE EXECUTE FROM PUBLIC on advisory-lock / WAL-message / large-object / NOTIFY functions in `pg_catalog`, GRANT EXECUTE back to `igaming` and `igaming_runtime`; REVOKE CONNECT on `postgres` and `template1` FROM PUBLIC); `GRANT CONNECT ON DATABASE postgres TO <master>`; create/drop `igaming_restore_loader`; create/drop database `igaming_restore_check`. Reads: the whole database through the BYPASSRLS backup role (synthetic data; it lands encrypted in the task's ephemeral storage and is destroyed with the task). The probes change nothing (rolled back; session locks end with the connection; a non-transactional `pg_logical_emit_message` would write one tiny WAL record only if hardening were absent).

## 5. Expected RDS outcomes the architect should be ready for (PROVIDER DEPENDENT; none verified)

1. `CREATE ROLE ... BYPASSRLS` as the RDS master: unknown. A refusal is a STOP (plan 5.1).
2. Revoke on `pg_catalog` functions: unknown; recorded as a FINDING with the abuse probes becoming residuals.
3. **The logical restore is expected to be BLOCKED.** `restore.sh` refuses (a) an owner role that holds CREATEROLE or is a member of any `rds_*` / `pg_*` role (check "the owner role must be unprivileged", applies to every phase) and (b) a data-load identity that is a member of such a role. On RDS the owner `igaming` IS the master: CREATEROLE and `rds_superuser`, so (a) refuses even at `--phase provision`, and any role created `IN ROLE igaming` is transitively privileged for (b). The drill records `restore_provision|BLOCKED` (or `restore_load|BLOCKED`) with the exact error, then runs `verify.sh` with the backup's baseline against the LIVE source as `INFO` records (the source may have changed since the snapshot). This needs an architect decision before production (for example an unprivileged application-database owner distinct from the RDS master, or an ADR 0122 amendment); it is not worked around here.
4. `restorecheck` (gate 4 of `verify.sh`) scopes to the first tenant it lists and requires it to hold ledger rows (`runtime_role_scoped_sees_only_own_tenant`); `deploy/drill/verify.sql` requires a non-empty ledger. A staging database whose first listed tenant has no ledger entries FAILS that check by design. Make sure every synthetic tenant has ledger activity (6.6) before the drill, or treat that single check as data-dependent.

## 6. IAM delta (NOT applied; listed, policy files NOT edited)

No action was found missing from `deploy/aws/iam/staging-deployer-infra-policy.json` for the runner: ECR repository create/describe/push on `repository/igaming-staging/*`, `ecs:RegisterTaskDefinition`, `RunTask`, `DescribeTasks`, `StopTask`, `logs:CreateLogGroup`, `PutRetentionPolicy`, `GetLogEvents`, `DescribeLogStreams` on `/ecs/igaming-staging/*`, `rds:Describe*` (for `resource_id`) are all present.
Two deltas ARE needed:

1. **Deployer policy `staging-deployer-edge-iam-state-policy.json`** (the three IAM statements name the roles exactly): add `arn:aws:iam::765578795051:role/igaming-staging-ecs-runner-task-role` to the `Resource` list of
   - `IamCreateStagingRolesOnlyWithBoundary`: `iam:CreateRole`, `iam:PutRolePolicy`, `iam:DeleteRolePolicy` (condition `iam:PermissionsBoundary` unchanged);
   - `IamManageStagingRoles`: `iam:GetRole`, `iam:DeleteRole`, `iam:UpdateRole`, `iam:TagRole`, `iam:UntagRole`, `iam:GetRolePolicy`, `iam:ListRolePolicies`, `iam:ListAttachedRolePolicies`, `iam:ListInstanceProfilesForRole`;
   - `IamPassStagingRolesToEcsOnly`: `iam:PassRole` (condition `iam:PassedToService = ecs-tasks.amazonaws.com`; `ecs:RunTask` needs it for the task role).
   Extend `deploy/aws/tests/simulate-deployer-policies.py` with the corresponding allow cases (and a deny case for creating the role without the boundary).
2. **Permissions boundary** `igaming-staging-ecs-role-boundary` (managed in `deploy/aws/bootstrap`, applied by the BOOTSTRAP credential only): the new statement `StagingRunnerBackupRoleIamAuth` allows `rds-db:connect` on `arn:aws:rds-db:eu-central-1:765578795051:dbuser:db-*/igaming_backup` (the DbiResourceId does not exist when the boundary is applied; the role's inline policy pins the exact instance). Without it the task role's permission is capped to nothing and the IAM login fails with an authentication error. **This one is in the repository's bootstrap Terraform, not in a policy JSON; it must be re-applied with the bootstrap credential before the first runner task.**

Also relevant to plan 7.3, but pre-existing and unchanged: the deployer cannot `rds:RestoreDBInstanceToPointInTime` (the simulator asserts the deny). The clone must be created by a credential that may (the bootstrap/owner credential), or the deployer policy needs `RestoreDBInstanceToPointInTime` (and `AddTagsToResource`) on `db:igaming-staging-db`, `db:igaming-staging-db-pitr-*`, the subnet/parameter/option groups: not added here.

## 7. Security-relevant decisions and residuals for the security review

| Id | Decision / residual |
|---|---|
| SRSP-9 | `seed-actor-proof`: the key is in PID 1's environment (every ECS-injected secret is) and in one tmpfs file during the run; the database log is protected by the runbook's pre-check (STOP when statement parameters could be logged), not by masking. The mode runs in a task definition with no task role. |
| SRSP-10 | The one-off execution role can now also pull the runner image, write the runner log group and READ the actor-proof secret (needed to inject it). The staging deployer can already read every `igaming-staging/*` secret; the service execution role gains nothing. A separate execution role for the actor-proof definition would narrow it; not built (it would add a third deployer-policy role). |
| SRSP-11 | A dedicated task role (not the shared `ecs-task-role`) carries `rds-db:connect`, so platform-api, b2c, backoffice and the one-off tasks cannot mint backup-role tokens. SRSP-3 (shared task role and the webhook secret) is unchanged. |
| SRSP-12 | Boundary wildcard `db-*` on the DB resource id, pinned by the role's own policy; any role carrying this boundary could at most connect as `igaming_backup`. |
| SRSP-13 | The drill dumps the whole synthetic database into task storage encrypted with a key that sits in the same task: it proves mechanics, not confidentiality against a compromised task. Fargate ephemeral storage is encrypted by the platform and destroyed with the task. |
| SRSP-14 | `bash -c "$BACKUP_IAM_TOKEN_COMMAND"` is how `backup.sh` obtains tokens (existing, operator-trusted configuration). The runner builds that string only from the validated RDS host, a constant user and the region constant (`printf %q`). |
| SRSP-15 | Container root filesystem is writable (scripts need `/tmp`); the process runs as uid 10001 with all capabilities dropped. A read-only root filesystem with explicit tmp volumes would tighten this. |
| SRSP-16 | Logs: only `RESULT`/`TIMING`/`PITR`/`KEY`/`SNAPSHOT`/`SUMMARY` records, bounded scripts' lines and the first line of an error. Static guards forbid tracing, `PGPASSWORD`, passwords in URLs or `-c`/`-v`, and printing of the secret variables; a local run asserts the key text never appears in any output. `PRINT_EXPECTED=1` prints row hashes (not row data). |
| SRSP-17 | Restoring and dumping with a master credential that is also the application database owner is how the platform is shaped on RDS; section 5 item 3 is the architectural consequence. |
| MKT-DUAL-1 | Still OPEN: `seed-market` is a single-operator tool; two humans review the parameters and the authorisation reference by process. |

## 8. Probe list (for the security review)

Each runs as `igaming_backup` in its own connection inside `BEGIN READ WRITE; <statement>; ROLLBACK`; PASS = denied by a privilege error.

Write / escalation (15, must be denied): `INSERT INTO tenants ...`; `UPDATE ledger_entries SET amount = amount + 1`; `DELETE FROM ledger_entries`; `TRUNCATE ledger_entries`; `CREATE TABLE zz_backup_probe (a int)`; `CREATE TEMP TABLE zz_probe (a int)`;
`DROP TABLE ledger_entries`; `ALTER TABLE ledger_entries DISABLE TRIGGER ALL`; `SET ROLE igaming`; `SET session_replication_role = replica`; `CREATE ROLE zz_probe`; `ALTER ROLE igaming_backup SUPERUSER`; `COPY (SELECT 1) TO PROGRAM 'true'`;
`SELECT pg_read_file('/etc/passwd')`; `SELECT pg_reload_conf()`.

Abuse primitives (8, denied once `harden-backup-surface.sql` took effect; FINDING when RDS refused it): `pg_advisory_lock(42)`; `pg_try_advisory_lock(42)`; `pg_advisory_xact_lock(42)`; `pg_logical_emit_message(false, 'x', 'y')`;
`lo_create(0)`; `pg_notify('c', 'p')`; connect to `postgres`; connect to `template1`.

Plus the role's own attribute check (`rolsuper=0, bypassrls=1, createdb=0, createrole=0, replication=0, connlimit=2`, member of `rds_iam` or nothing).

## 9. Suggested order of operations for the orchestrator (all after the IAM delta and the authorisation record)

1. Apply the bootstrap boundary and the deployer-policy delta; run the simulator; run `deploy/aws/tests/run-static-checks.sh` on the named commit.
2. `deploy.sh up` (builds platform-api and staging-runner images under the commit SHA; role-init and migrate run inside it). Then `deploy.sh runner verify-db EXPECT_BACKUP_ROLE=absent`: record the SNAPSHOT digests (3.1, 3.5).
3. `deploy.sh seed-admin <email>`; create the synthetic tenant (with licence) and brand in the Back Office; `deploy.sh runner seed-actor-proof`; `deploy.sh runner seed-market ... DRY_RUN=1`, then without `DRY_RUN`; acceptance flows (6.6) so every tenant has ledger rows.
4. `deploy.sh runner backup-restore-drill` (nothing else writing): record RDS findings, probes, timings, `PITR|T_utc`, `PITR|expected_digests`, `PITR|anchor`.
5. Restore the PITR clone at T with a credential allowed to (section 6); wait for it; `deploy.sh runner pitr-verify PITR_ENDPOINT=... EXPECTED_DIGESTS=... EXPECT_ANCHOR=...`; delete the clone (7.5).
6. `deploy.sh runner verify-db EXPECT_BACKUP_ROLE=present` and `deploy.sh runner probe-backup-role` for the final evidence; teardown (section 13). The runner image, task definitions, role and log group are destroyed with the stack (the ECR repository is `force_delete`).

## 10. Verification performed

| Check | Result |
|---|---|
| `terraform fmt -recursive -check deploy/aws`, init/validate of every root and module, `terraform test` (offline mirror, terraform 1.11.4) via `deploy/aws/tests/run-static-checks.sh` | pass (staging root 13, ecs 22, iam 8, ecr 2 runs; all other modules unchanged) |
| `deploy/aws/tests/runner-overrides-test.sh` (via the static checks) | 20 assertions pass |
| shellcheck (`-S warning`) on `deploy/staging-runner/**/*.sh` and `deploy.sh` | clean |
| `gofmt`, `go vet` (plain and `-tags integration`) on `./cmd/... ./deploy/...` | clean |
| `go test ./deploy/... ./cmd/...` | see final report |
| `cmd/seed-market` integration tests on a private scratch DB | 8 pass |
| `deploy/staging-runner/tests/run-local-modes.sh` against a throwaway cluster (drill port 5497-5499) with a synthetic world loaded through the platform services | 81 assertions held, 0 failed (SEED_WORLD=1: 2 tenants, 3 players each, loaded through the platform's own services) |
| `docker build --check` of the Dockerfile | no warnings |

### Could NOT be tested locally

The image build (Alpine package availability, apk, the runner as uid 10001 in a container); anything on ECS (task start, execution-role secret injection, awslogs, `deploy.sh runner`'s `run-task` / polling / log retrieval); IAM database authentication and `rds_iam`; `sslmode=verify-full` against the RDS CA bundle and the truststore download; the real master role (locally the "master" is the cluster superuser and the owner is `igaming`; on RDS both are `igaming`); `BYPASSRLS` creation, `pg_catalog` revoke, `pgaudit`, role-membership consequences on RDS (section 5); a PITR clone's credentials; timings on real data; Terraform plan/apply.

## 11. Security review (security specialist, 2026-10-16)

Scope: the diff `12edec0..ad408ba` (5 commits) against `origin/claude/focused-wright-jw88w9`: `cmd/seed-market`, `deploy/staging-runner/**`,
`deploy/docker/staging-runner.Dockerfile`, Terraform (`modules/ecs`, `iam`, `ecr`, `environments/staging`, `bootstrap/main.tf`), `deploy.sh runner` /
`runner-image`, the runbook diff, compared with `deploy/aws/iam/*.json`, ADR 0122 and ADR 0110. Code- and design-level only. NOT in scope and NOT done:
any AWS call, `terraform plan/apply`, an image build, a run on RDS, a run of `run-local-modes.sh` (throwaway cluster), penetration testing.
Nothing here declares the runner "secure" on RDS: every PROVIDER DEPENDENT item of section 5 is still unverified.

Checks run: `go test ./cmd/seed-market/... ./deploy/...` pass; shellcheck `-S warning` on every runner script, `deploy.sh` and the overrides test is clean;
`deploy/aws/tests/run-static-checks.sh` (offline mirror) passes all checks; the 8 `cmd/seed-market` integration tests pass on a private scratch DB,
plus a throwaway reviewer probe (not committed) that showed SRP3-1 and SRP3-3. The scratch DB was then dropped.

### 11.1 Findings

| Id | Severity | Finding and failure scenario | Required fix |
|---|---|---|---|
| SRP3-1 | **MEDIUM (blocks marking `seed-market` complete)** | `cmd/seed-market` decides "synthetic environment" from `APP_ENV` alone. The platform's binding control is the `database_environment` anchor (migration 0135: "NO ROW = PRODUCTION"; the `APP_ENV`/GUC side is forgeable and "can only NARROW"), and seed-market never reads it. The refusal of `production` works for every spelling (`TrimSpace` plus an exact allow-list, so `Production`, ` production ` and `PRODUCTION` are refused), but the check is self-attested. Failure scenario: an operator holding a real database's owner credential runs the binary with `APP_ENV=staging`. It then opens a licence ceiling and a tenant opt-in for any country, because `MOCK_GEOLOCATION_COUNTRY` is unset outside mock environments and the country rule then does not apply. Reproduced: on a database with no anchor row (production by the platform's definition), `apply` wrote both versions and reported `OK`. | Before any write (also in `-dry-run`), read the anchor with `db.Pool.ReadEnvironmentAnchor` and refuse unless it is `staging`, `development` or `test` (absent counts as production). Add an integration test for an absent and a `production` anchor. Runbook: 6.2 must record `staging` before 6.5 (today the anchor may be left `unrecorded`). |
| SRP3-2 | **MEDIUM (blocks treating the probe output as evidence)** | The probe verdicts fail open. (a) A WRITE probe that fails for a reason other than a privilege error is a `FINDING` (exit 0). PostgreSQL checks privileges BEFORE triggers and constraints, so such an error (`ledger_entries` append-only trigger, NOT NULL, a CHECK) proves the role HOLDS the write privilege: that is a FAIL. (b) Connection failures are `BLOCKED` for write probes and `FINDING` for abuse probes, and the attribute check is `BLOCKED`. So `probe-backup-role` against an unreachable role or an expired token ends with 0 PASS and exit 0, and `deploy.sh` prints "exit 0 (no FAIL)". (c) No `statement_timeout`: `pg_advisory_lock(42)` blocks forever if any session holds key 42 (only possible if hardening was refused). | Write class: any non-privilege error is FAIL. Abuse class: a connection error is BLOCKED. The mode exits 3 if any probe is BLOCKED or the PASS count is not 15 + 8 (15 + the abuse FINDINGs when `EXPECT_HARDENING=refused`). Prefix each probe with `SET LOCAL statement_timeout = '10s'`, and use the two-key advisory-lock form with a reserved key. |
| SRP3-3 | LOW | `ACTOR_ID` / `-actor-id` is only checked for UUID syntax. Any UUID is recorded as the actor of both audit rows: one with no `staff_users` row, a tenant staff member or a disabled admin (reproduced with a random UUID: success). This produces a misattributed audit trail for a market enablement while MKT-DUAL-1 is open. Also: a suspended or closed tenant is not refused (`tenantStatus` is read but not used), and check-then-insert runs at READ COMMITTED (a concurrent closed version inserted between the check and the library's close-open would be closed and re-enabled; single-operator tool, informational). | Require the actor to be an active `platform_admin` with `tenant_id IS NULL`, and refuse a non-active tenant. Optionally take `SELECT ... FOR UPDATE` on the licence/tenant row before the check. |
| SRP3-4 | LOW (process, wrong instruction) | Sections 6 and 9 and runbook B-checklist say the BOOTSTRAP credential applies both the boundary statement and the three deployer-policy statements. `staging-bootstrap-policy.json` can only version `policy/igaming-staging-ecs-role-boundary`, and the deployer has an explicit `Deny iam:CreatePolicyVersion` on `*`. The deployer-policy change therefore needs an IAM-administrative principal (the account owner), not the bootstrap credential. The simulator extension has also not been written yet. | Correct the wording (see 11.3). Extend `simulate-deployer-policies.py` (allow cases for the runner role, a deny case without the boundary) before the change is applied. |
| SRP3-5 | LOW | The actor-proof log pre-check misses settings that also log bound parameters: `log_transaction_sample_rate` > 0 (every statement of a sampled transaction, with parameters up to `log_parameter_max_length`), `log_min_duration_sample` with `log_statement_sample_rate`, `auto_explain` (PG16 `auto_explain.log_parameter_max_length`, when loaded), and pgaudit OBJECT auditing (`pgaudit.role`) with `pgaudit.log_parameter=on`. The current parameter group (`pgaudit` only, RDS defaults) enables none of them, so this is not exploitable today, but a later parameter-group change would pass the gate silently. | Add the four checks (STOP when any could log). |
| SRP3-6 | LOW | Secret hygiene claims are stronger than the code. (a) Record 3.2 says the passwords are unset "before any child process starts". In fact `rn_init` (`mktemp`, `hostname`, `date`) and, in `seed-actor-proof`, `psql --version`, `base64`, `od`, `wc` run while `DB_MASTER_PASSWORD` is still in the environment, and `psql --version` runs while `ACTOR_PROOF_KEYS` is too. (b) `unset` does not change `/proc/<pid>/environ` of the bash process (nor of PID 1), so any uid-10001 process in the container can still read the values. (c) Tracing can be switched on from outside: an ECS environment override `SHELLOPTS=xtrace` (verified: bash imports it at start-up) or `BASH_ENV` traces `rn_pgpass_line` with the password into CloudWatch. The static guards only cover tracing written in the scripts. `deploy.sh` cannot set these names, but a raw `ecs run-task` by the deployer can. | (a)/(b): correct the wording (the boundary is the container, same trust as SRSP-9). (c): the entrypoint refuses to start when `SHELLOPTS` contains `xtrace`/`verbose` or `BASH_ENV`/`ENV`/`PS4` is set, and runs `set +xv` first. |
| SRP3-7 | LOW | Network placement: with the default `ecs_public_ip_mode = true` (ADR 0086), the runner, which holds the master credential and dumps the whole BYPASSRLS database into task storage, runs in a PUBLIC subnet with a public IP. Inbound traffic is blocked: the task SG only admits the ALB on 8080, and nothing listens there. Egress is `0.0.0.0/0`, as for every task. It is NOT in a private subnet, contrary to the review brief's assumption. | Record this as accepted for staging, or run the drill with `ecs_public_ip_mode = false`. For production, use private subnets with VPC endpoints (ECR, Secrets Manager, Logs, STS) and no general egress. |
| SRP3-8 | LOW | Supply chain: the base images are pinned by tag, not digest (`golang:1.26.9-alpine`, `alpine:3.20`). apk package versions are not pinned, and the `procps` fallback makes the package set non-deterministic. No artefact is downloaded during the build, so no checksum is missing there (apk verifies signed indexes). At run time the RDS CA bundle is fetched over HTTPS and trusted on first use: its SHA-256 is logged but not compared with a pinned value. ECR has immutable tags and scan on push. The images are built on the operator's machine. This is the same pattern as `platform-api.Dockerfile`. | Pin base-image digests, and pin the CA bundle SHA-256 (or bake a reviewed bundle into the image), for production. Accepted residual for this staging run. |
| SRP3-9 | LOW | Abnormal termination leaves credentials and objects behind. `igaming_restore_loader` is a password login IN ROLE `igaming` (on RDS transitively `rds_superuser`) and is dropped only by the EXIT trap, as is `igaming_restore_check`. ECS `StopTask` sends SIGTERM, but bash runs the trap only after the foreground child (for example a long `pg_restore`) returns, and SIGKILL follows after 30 s. An OOM kill skips the trap altogether. Mitigations: `VALID UNTIL` 2 h, `CONNECTION LIMIT 2`, the password only in task tmpfs, and on RDS provisioning is expected to be BLOCKED before the loader exists. Similarly, `pitr-verify` gives the clone's BYPASSRLS `igaming_backup` a password credential (2 h). That deviates from ADR 0122 M8 on the clone and is acceptable only because the clone is disposable. | Add a post-run runbook check (`pg_roles` has no `igaming_restore_loader`, `pg_database` has no `igaming_restore_check`). Make the deletion of the clone (7.5) a mandatory, evidenced step. Consider `stopTimeout = 120`. |
| SRP3-10 | INFO | `pitr-verify` only guards against the primary by `PITR_ENDPOINT != RDS_HOST`. Its regex accepts any `igaming-staging-*` instance (a replica, or an unrelated instance), and step 1 (revoke `rds_iam`, set a password) runs before anything server-side proves the target is a clone. | Restrict the regex to `^igaming-staging-db-pitr-` (the plan's naming), matching the deployer resource pattern. |
| SRP3-11 | INFO | The `deploy.sh` allow-list is a safety rail, not a security boundary: whoever holds `ecs:RunTask` + `iam:PassRole` can override any environment variable, `taskRoleArn` and `executionRoleArn`. That principal (the deployer) can already read every `igaming-staging/*` secret, so there is no escalation. Functional mismatch: the overrides test accepts `REASON=staging bootstrap`, which the runner's `REASON` regex (no spaces) refuses with exit 2. | Align the regexes or the test example. |
| SRP3-12 | INFO (confirms SRSP-12) | The boundary is shared by all four staging roles. After this change the deployer could add `rds-db:connect ... /igaming_backup` to the SHARED `ecs-task-role` (platform-api) and it would take effect. This is not an escalation: the deployer is already more privileged (master secret). | Production: a dedicated boundary for the runner role. |

### 11.2 Verified as correct (within the scope above)

- `seed-market`: every SQL statement is parameter-bound; the brand lookup is scoped to the resolved tenant (`WithTenant` plus `tenant_id = $2`), and a foreign brand is refused (test). Free-text fields refuse control, format and line-separator characters. `-authorization-reference` is mandatory. It is idempotent with no duplicate version or duplicate audit row, and refuses a closed (disabled/withdrawn) open version. After the writes it re-resolves the gate and fails unless all four operations are permitted. The `production` refusal holds for every case and whitespace variant. The runner passes only `PATH`, `HOME`, `APP_ENV`, a password-free `DATABASE_URL`, `PGPASSFILE` and `MOCK_GEOLOCATION_COUNTRY` (`env -i`).
- `seed-actor-proof`: kid binding (exactly one entry for `ACTOR_PROOF_ACTIVE_KID`, else STOP); key ≥ 32 bytes; the hex is read by psql from a 0600 tmpfs file and sent as a `\bind` parameter, never in argv, `-v`, a URL or statement text. It never overwrites; a conflict STOPs. Output is kid/status/octets only. It runs in a definition with NO task role.
- Probes: each runs on its own connection as `BEGIN READ WRITE; <stmt>; ROLLBACK`, and a session that dies mid-way is aborted by the server. `INSERT/UPDATE/DELETE/TRUNCATE/CREATE/DROP/ALTER TABLE`, `CREATE ROLE`, `ALTER ROLE`, `SET ROLE`, `SET session_replication_role`, `lo_create` and `pg_notify` (delivered only on commit) are all transactional, so nothing is left behind. Session advisory locks end with the connection. Non-transactional only if NOT denied: `pg_logical_emit_message(false, ...)` (one WAL record, documented), `COPY ... TO PROGRAM` (runs `true` at once) and `pg_reload_conf()` (SIGHUP at once). The last two would already be a FAIL.
- Terraform: `igaming-staging-ecs-runner-task-role` has exactly one statement, `rds-db:connect` on `dbuser:<DbiResourceId>/igaming_backup`. Both the resource id and the user are validated (no wildcard; the user is fixed). The actor-proof definition has no `task_role_arn`. Both definitions run as uid 10001 with all capabilities dropped and init enabled. The task role's trust policy is `ecs-tasks` only, and it carries the boundary. The ECR repo is IMMUTABLE with scan on push. The image tag is the full commit SHA from a clean tree. `rds_ca_bundle_url` is restricted to the AWS truststore.
- Execution role gains read of the actor-proof secret (SRSP-10): ACCEPTABLE. Execution-role credentials are used by the ECS agent and never reach the container. ECS `containerOverrides` cannot add `secrets`, so only a re-registered task definition (deployer) could inject it elsewhere, and the deployer can already read it.
- `deploy.sh runner`: allow-listed names per mode, a credential-name deny-list, a value character set without quote, `$`, backtick or pipe, overrides built with `jq --arg` (no shell or JSON injection), no secret on any command line, the entrypoint override impossible (`command` only) and a single mode argument.
- 2-hour credentials: the passwords are 32 random alphanumerics from `/dev/urandom`. Only a client-side SCRAM verifier reaches the server (`\getenv`, `:'...'` quoting). The clear password is only in a 0600 tmpfs passfile and registered for scrubbing. `VALID UNTIL` is 2 h. Disposal: loader dropped at exit, clone deleted by the operator (see SRP3-9).

### 11.3 IAM delta: exactly what must be applied, and by whom

Confirmed minimal; nothing broader is needed (ECR, ECS RunTask/RegisterTaskDefinition/StopTask/DescribeTasks, Logs GetLogEvents/DescribeLogStreams, `rds:Describe*`
and the explicit-Deny statements are already sufficient, and the Deny on boundary escape stays `*`).

1. **BOOTSTRAP credential** (`staging-bootstrap-policy.json`, which allows `iam:CreatePolicyVersion`/`DeletePolicyVersion` on `policy/igaming-staging-ecs-role-boundary` only): `terraform apply` in
   `deploy/aws/bootstrap`, which adds exactly ONE statement to the boundary:
   `StagingRunnerBackupRoleIamAuth`: `Allow rds-db:connect` on `arn:aws:rds-db:eu-central-1:765578795051:dbuser:db-*/igaming_backup`. Nothing else in the bootstrap diff.
2. **IAM-administrative principal (account owner), NOT the bootstrap credential and NOT the deployer**: a new version of the customer-managed deployer policy built from
   `staging-deployer-edge-iam-state-policy.json`, with `arn:aws:iam::765578795051:role/igaming-staging-ecs-runner-task-role` added to the `Resource` list of exactly
   `IamCreateStagingRolesOnlyWithBoundary` (condition unchanged), `IamManageStagingRoles` and `IamPassStagingRolesToEcsOnly` (condition `iam:PassedToService = ecs-tasks.amazonaws.com` unchanged).
   The JSON file in the repository must be edited and simulator-tested first; it has not been edited in this change.
3. Not needed for this change. As before, the PITR clone (7.3) needs `rds:RestoreDBInstanceToPointInTime`, which the deployer does not have: run it with the owner credential.

### 11.4 Verdict

**NOT APPROVED as complete.** SRP3-1 and SRP3-2 must be fixed and re-reviewed. Both are small, local changes, and both concern the very controls this change claims: the refusal of a real environment, and probe evidence for a BYPASSRLS role.
SRP3-4 (wording on who applies the IAM delta) and SRP3-6(a)/(b) (overstated claims) must be corrected in this record and the runbook.
SRP3-3, -5, -6(c), -7, -8, -9 are LOW: fix them, or have the owner accept them in writing as staging residuals. SRP3-10..12 are informational.
None of these findings needs a schedule change for the infrastructure parts (Terraform, the task-role design, the actor-proof task definition), which are approved as designed, subject to the PROVIDER DEPENDENT items of section 5.
**Launch-relevant (production):** SRP3-1 (any environment-sensitive tool must bind to the database anchor), SRP3-7/-8/-12 (production network, digest pinning, a dedicated boundary), and section 5 item 3 (the logical restore on RDS) must be resolved before any production use of this tooling.

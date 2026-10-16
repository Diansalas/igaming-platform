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

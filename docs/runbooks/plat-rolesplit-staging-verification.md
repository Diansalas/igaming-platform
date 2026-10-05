# PLAT-ROLESPLIT-1 / TRIGGER-SEARCH-PATH-1-0116: staging deployment and verification procedure

Owner: `devops` (with `security`). Written 2026-10-05, READ-ONLY analysis: **nothing in this document has been run against
AWS or any database.** AWS is OFF (`docs/governance/staging-teardown-2026-09-26.md`). Every step marked **OPERATOR** needs a
human with AWS access and the separate authorizations in section 6. This runbook proposes no Terraform, IAM or code change
unless labeled PROPOSAL, and it deploys nothing by itself.

Owner decision 2026-10-05 (invariant to prove): PUBLIC cannot CREATE TEMP; `igaming_runtime` cannot CREATE TEMP; the database
OWNER keeps TEMP where required; a fake TEMP-shadow approval fails; a genuine two-person approval succeeds; **the
application service runs as `igaming_runtime`, never the database owner (PLAT-ROLESPLIT-1).**

Related: ADR 0108 (`docs/decisions/0108-revoke-temp-from-runtime-role.md`), migration
`migrations/0116_revoke_temp_from_runtime.up.sql`, `docs/runbooks/operational-runbooks.md` section 7 step 5,
`docs/security/runtime-role-separation.md`, `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md`.

## 1. Verdict: is the existing approved staging architecture sufficient?

**SUFFICIENT for the invariant "the staging API connects as `igaming_runtime`, never the owner/master".** No Terraform or IAM
change is needed to make it true. Evidence (file:line):

| Question | Answer | Evidence |
|---|---|---|
| Is there a non-owner runtime role? | Yes, `igaming_runtime` (LOGIN, NOSUPERUSER, NOCREATEDB, NOCREATEROLE, NOBYPASSRLS), created by the role-init task | `deploy/aws/sql/init-runtime-role.rds.sql:83-90` |
| Which secret holds its password? | Secrets Manager `igaming-staging/db-runtime` (JSON `{username,password}`; `${name_prefix}/db-runtime`), written write-only, never in state | `deploy/aws/modules/secrets/main.tf:54-69`; default username `igaming_runtime`: `deploy/aws/modules/secrets/variables.tf:5-8` |
| Env the platform-api task gets | `APP_ENV=staging`, `DATABASE_URL=postgres://igaming_runtime@<rds-host>:5432/<db>?sslmode=require` (no password), `PGPASSWORD` from `<db-runtime ARN>:password::`, `JWT_SIGNING_SECRET` from the JWT secret. No master reference. | `deploy/aws/modules/ecs/main.tf:70` (URL), `:117-134` (env and secrets); `deploy/aws/environments/staging/main.tf:190,207-214` |
| Static test that the API gets only runtime+JWT | Yes (`terraform test`) | `deploy/aws/modules/ecs/tests/ecs.tftest.hcl:70-75` |
| Which tasks run as owner/master | `role-init`, `migrate`, `seed-admin`: `DATABASE_URL` user = master `igaming`, `PGPASSWORD` from the RDS-managed master secret | `deploy/aws/modules/ecs/main.tf:71` (URL), `:368-404` (migrate), `:414-451` (role-init), `:462-499` (seed-admin) |
| Could the API task ever receive master credentials? | Not by configuration. The service execution role can read only the runtime and JWT secrets; only the one-off role can read the master secret. The task role is empty. | `deploy/aws/modules/iam/main.tf:41-58`, `:138-145`; `deploy/aws/environments/staging/main.tf:118-121` (service) vs `:125-129` (one-off); ADR 0086 section 17 |
| Residual path to the master credential | An operator who can `run-task` with a command override, or re-create a staging role with a self-trusting policy, can read it. Accepted for synthetic staging; Access Analyzer is the detection. Not reachable from the API container. There is no ECS Exec (`enable_execute_command` appears nowhere in `deploy/`), so there is no shell into the API task. | ADR 0086 section 18 "Residual" (lines 362-379) |
| Who owns the database | The RDS master user `igaming`: RDS creates `db_name` (`igaming_platform_staging`) at instance creation owned by `username = master_username` (default `igaming`). This IS the migration-owner role. | `deploy/aws/modules/database/main.tf:78-80`, `variables.tf:34-38`; `init-runtime-role.rds.sql:4-9`. **Live owner is UNVERIFIED until deployed; step V4 proves it, and migration 0116 itself RAISES `TEMP-REVOKE-1` if the owner premise is false (`migrations/0116_revoke_temp_from_runtime.up.sql:52,58`, ADR 0108 section 3.3).** |
| Does 0116's "migration role must own the database" hold? | Holds by construction: migrate runs as the master, who owns the database (above). | ADR 0108 section 5 first bullet |
| Is role-init before migrate enforced? | By `deploy.sh` (`run_migrations`, `deploy/aws/scripts/deploy.sh:156-161`), not by Terraform. The 0116 revoke is in BOTH role-init (`init-runtime-role.rds.sql:113-118`) and migrate (0116). | `ecs/main.tf:8-19` header |
| Does the production startup gate cover staging? | **No.** It runs only when `GuardEnvironment()` is `production`; staging sets `APP_ENV=staging`. | `internal/db/production_safety.go:112-115`, `cmd/platform-api/main.go:145`, ADR 0108 section 5 "Production gate scope" |

What the architecture does NOT give you (not a blocker, affects only how easily the proof is produced):

1. The API never logs which database role it connected as. It logs only `"starting platform-api"` (environment) and
   `"database connected"` (`cmd/platform-api/main.go:97,128`). No authenticated endpoint exposes `current_user`
   (non-test code references `current_user` only in `internal/db/db.go:69` and `internal/db/production_safety.go`).
2. `application_name` is not set (`internal/db/db.go:27-53`; no `application_name` in `DATABASE_URL`, `ecs/main.tf:70`), so
   `pg_stat_activity.application_name` is empty for the API. Identify the API's backends by `usename` and `client_addr`.
3. No ECS Exec, so no `psql` inside the API task. Evidence comes from `pg_stat_activity` (via a one-off task) plus the task
   definition.
4. `deploy.sh up` creates the services in phase 3, before role-init (phase 5) (`deploy.sh:219-246`). API tasks start before
   `igaming_runtime` exists, fail DB authentication, and are replaced by the phase-5 forced deployment. This is benign for
   this invariant: there are no runtime sessions before the revoke, so no pre-0116 TEMP shadow can exist. A staging database
   is also always freshly created (one commit per environment lifetime, `deploy.sh:195-199`), so the "restore loses the ACL"
   path of ADR 0108 section 5 does not arise there.

## 2. Preconditions (all must hold; none is satisfied by this document)

1. Section 6 authorizations are recorded in writing by the owner. **Without them, stop. Nothing below may be run.**
2. Approved commit checked out clean (`git status --porcelain` empty); the image tag is the full SHA (`deploy.sh:59-64`).
   It contains migrations 0001..0117, gap-free, including `0116_revoke_temp_from_runtime`.
3. Offline checks already green, run by engineering (not run here): `deploy/aws/tests/run-static-checks.sh`
   (includes `terraform test` for the ecs module's secret wiring).
4. `aws sts get-caller-identity` shows account `765578795051` and the authorized principal (section 6), region
   `eu-central-1`. `deploy/aws/environments/staging/terraform.tfvars` exists with `staging_access_cidrs` including the
   operator IP.
5. Last AWS-deployed commit was `9190d5d01da076141a1f70d7e5897a573d3b18f4`. The current code (migrations to 0117, newer
   startup wiring) has never run on AWS. Expect and record unrelated first-deploy failures separately from this verification.

## 3. Required order

1. `terraform apply` creates RDS (master owns the database), secrets (runtime secret value written), IAM, ECS.
2. **role-init** (master): create or update `igaming_runtime`, `REVOKE TEMPORARY ... FROM PUBLIC`, `REVOKE TEMPORARY ... FROM
   igaming_runtime`, grants, default privileges.
3. **migrate** (master): `/app/migrate up` (applies 0116, which re-revokes and ASSERTS), then the chained
   `REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime`.
4. Force a new deployment of platform-api (and the other services): the runtime sessions start only now, after 0116.
5. Smoke and the checklist in section 4.

This is exactly what `deploy/aws/scripts/deploy.sh up` (phase 5) and `deploy.sh migrate` do. **OPERATOR** runs them,
interactively, after authorization; this document does not add an alternative deploy path. If 0116 or role-init is ever applied
to a database that already has runtime sessions, recycle them (force a new deployment of the platform-api service) or run
`pg_terminate_backend` as in `operational-runbooks.md` section 7 step 5.

## 4. Verification checklist (all steps OPERATOR; all read-only against AWS and the database)

Naming (names only, from the Terraform): cluster `igaming-staging-cluster`; services `igaming-staging-platform-api` etc.;
task definition families `igaming-staging-platform-api` and `igaming-staging-role-init`; secrets `igaming-staging/db-runtime`,
`igaming-staging/jwt-signing-secret`, `igaming-staging/seed-admin-password`, plus the RDS-managed master secret; log groups
`/ecs/igaming-staging/{platform-api,migrate,role-init,seed-admin}`. Region `eu-central-1`.

### 4.1 Running SQL without ECS Exec (helper, OPERATOR)

There is no `psql` access to the private RDS from an operator laptop and no ECS Exec. Reuse the existing `role-init` task
definition as a read-only one-off with a command override (the same mechanism `deploy.sh seed-admin` uses,
`deploy.sh:277-290`). That task already receives `DATABASE_URL` (user = master), `PGPASSWORD` (master) and
`IGAMING_RUNTIME_PASSWORD` (the runtime secret's password) (`ecs/main.tf:432-438`). Run from
`deploy/aws/environments/staging` with the authorized credentials:

```bash
# OPERATOR. Read-only SQL; prints no secret. The override script must never echo $PGPASSWORD or $IGAMING_RUNTIME_PASSWORD.
rs_run() {   # $1 = shell script to run inside the role-init container
  local cluster subnets sg assign net ovr arn
  cluster="$(terraform output -raw ecs_cluster_name)"
  subnets="$(terraform output -json ecs_task_subnet_ids)"
  sg="$(terraform output -raw ecs_security_group_id)"
  assign="$(terraform output -raw ecs_assign_public_ip)"
  net="$(jq -n --argjson s "$subnets" --arg g "$sg" --arg a "$assign" '{awsvpcConfiguration:{subnets:$s,securityGroups:[$g],assignPublicIp:$a}}')"
  ovr="$(jq -n --arg s "$1" '{containerOverrides:[{name:"role-init",command:["sh","-c",$s]}]}')"
  arn="$(aws ecs run-task --region eu-central-1 --cluster "$cluster" \
        --task-definition "$(terraform output -raw ecs_role_init_task_definition_arn)" --launch-type FARGATE \
        --network-configuration "$net" --overrides "$ovr" --query 'tasks[0].taskArn' --output text)"
  aws ecs wait tasks-stopped --region eu-central-1 --cluster "$cluster" --tasks "$arn"
  aws ecs describe-tasks --region eu-central-1 --cluster "$cluster" --tasks "$arn" \
      --query 'tasks[0].containers[0].exitCode' --output text
  echo "log stream: /ecs/igaming-staging/role-init  role-init/role-init/${arn##*/}"
}
# Read output with: aws logs get-log-events --region eu-central-1 --log-group-name /ecs/igaming-staging/role-init \
#                     --log-stream-name role-init/role-init/<task-id> --query 'events[].message' --output text
```

Define two script snippets used below. `AS_OWNER` connects as the master; `AS_RUNTIME` rewrites only the user in the
non-secret URL and takes the password from the injected runtime secret:

```bash
AS_OWNER='psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1'
AS_RUNTIME='PGPASSWORD="$IGAMING_RUNTIME_PASSWORD" psql "$(printf %s "$DATABASE_URL" | sed "s#//[^@]*@#//igaming_runtime@#")" -X'
```

Usage pattern for a SQL block held in a shell variable (the here-document is part of the script string passed to
`rs_run`):

```bash
rs_run "$AS_OWNER <<'SQL'
$SQL_V4
SQL"
```

Caveat: the output lands in CloudWatch Logs. The SQL below returns role names, counts and booleans only. Do not add
`\conninfo`-style or `SHOW` commands, and never `get-secret-value`, that could print credentials.

### V1. Task definition wiring (OPERATOR, read-only AWS)

```bash
aws ecs describe-task-definition --region eu-central-1 --task-definition igaming-staging-platform-api \
  --query 'taskDefinition.containerDefinitions[0].{env:environment,secretNames:secrets[].name,secretFrom:secrets[].valueFrom}'
```
PASS: `DATABASE_URL` is `postgres://igaming_runtime@...?sslmode=require` (no password); `APP_ENV=staging`;
`PGPASSWORD` valueFrom is the `igaming-staging/db-runtime` ARN ending `:password::`; **no** `rds!db-...` (master) ARN anywhere.
Capture: task definition revision. FAIL: stop (section 5).

### V2. Secret names exist (OPERATOR)

```bash
aws secretsmanager describe-secret --region eu-central-1 --secret-id igaming-staging/db-runtime --query '{Name:Name,ARN:ARN}'
```
`describe-secret` returns no value. Never run `get-secret-value` for this verification.

### V3. Deployment ordering evidence (OPERATOR)

From the `deploy.sh up` or `migrate` transcript: `role-init completed (exit 0)` precedes `migrate completed (exit 0)`, which
precedes `forcing a new deployment`. In CloudWatch: the `role-init` stream's start time < the `migrate` stream's < the first
`"database connected"` event in the `platform-api` group that is not followed by an auth failure. Migrate log must show 0116
applied (and no `TEMP-REVOKE-1`).

### V4. Roles, ownership, no other TEMP holders (OPERATOR, SQL as owner)

```bash
read -r -d '' SQL_V4 <<'EOF'
\set VERBOSITY verbose
SELECT current_database(), current_user, session_user;
SELECT rolname, rolsuper, rolbypassrls, rolcreaterole, rolcreatedb, rolcanlogin
  FROM pg_roles WHERE rolname IN ('igaming','igaming_runtime') ORDER BY 1;
SELECT d.datname, pg_get_userbyid(d.datdba) AS db_owner FROM pg_database d WHERE d.datname = current_database();
SELECT tableowner, count(*) FROM pg_tables WHERE schemaname = 'public' GROUP BY 1;
SELECT n.nspname, pg_get_userbyid(n.nspowner) AS schema_owner,
       has_schema_privilege('igaming_runtime', n.nspname, 'CREATE') AS runtime_can_create
  FROM pg_namespace n WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema';
SELECT r.rolname, r.rolsuper,
       has_database_privilege(r.oid, current_database(), 'TEMP')   AS temp,
       has_database_privilege(r.oid, current_database(), 'CREATE') AS db_create,
       has_table_privilege(r.oid, 'public.ledger_adjustment_requests', 'INSERT') AS k2_insert
  FROM pg_roles r WHERE r.rolcanlogin AND r.rolname !~ '^pg_' ORDER BY 1;
EOF
rs_run "$AS_OWNER <<'SQL'
$SQL_V4
SQL"
```
PASS:
- `db_owner` = `igaming` (the master). This proves 0116's "migration role must own the database" premise on RDS.
- `igaming_runtime`: `rolsuper f, rolbypassrls f, rolcreaterole f, rolcreatedb f, rolcanlogin t`. Record the master's
  `rolbypassrls` as observed. `db.Connect` (`internal/db/db.go:66-84`) refuses a superuser or BYPASSRLS role in every
  environment; whether the RDS master trips that is UNVERIFIED, and either result is useful evidence.
- `public` tables owned only by `igaming`; none owned by `igaming_runtime`.
- `runtime_can_create` = `f` for every schema.
- Last query (ADR 0108 C3): `igaming_runtime` has `temp f, db_create f`. Every OTHER login role with `temp t` or `k2_insert t`
  must be reviewed and justified: the expected ones are the owner `igaming` and RDS-internal roles such as `rdsadmin`. An
  unexplained row means STOP (C3 not satisfied).

### V5. TEMP privilege, positive and negative (OPERATOR). Reuses `operational-runbooks.md` section 7 step 5 verbatim

As the runtime role:

```bash
read -r -d '' SQL_V5 <<'EOF'
\set VERBOSITY verbose
SELECT current_database(), current_user, session_user,
       has_database_privilege(current_database(),'TEMP')                          AS temp,          -- must be f
       (SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user) AS privileged,    -- must be f
       (SELECT count(*) FROM pg_auth_members
         WHERE member=(SELECT oid FROM pg_roles WHERE rolname=current_user))      AS memberships,   -- must be 0
       (SELECT count(*) FROM pg_proc WHERE prosecdef)                             AS secdef_fns;    -- 0, or each reviewed (RDS rds_* caveat)
BEGIN; CREATE TEMP TABLE tsp1_probe(a int); ROLLBACK;   -- NEGATIVE TEST: must fail with SQLSTATE 42501
EOF
rs_run "$AS_RUNTIME <<'SQL'
$SQL_V5
SQL"
```
PASS: `current_user = session_user = igaming_runtime`, `temp = f`, `privileged = f`, `memberships = 0`, and the `CREATE TEMP TABLE`
prints `ERROR:  42501: permission denied to create temporary tables in database "igaming_platform_staging"`. If it
SUCCEEDS, that is a FAIL (the table is rolled back, but the invariant is broken): STOP (section 5). Read `secdef_fns` per the
caveat in the operational runbook.

As the owner, with the PUBLIC ACL check and the live-shadow check (both must be as stated):

```sql
-- as the owner (AS_OWNER). PUBLIC must NOT hold TEMPORARY:
SELECT count(*) FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
 WHERE d.datname = current_database() AND a.grantee = 0 AND a.privilege_type = 'TEMPORARY';   -- must be 0
-- owner keeps TEMP (ADR 0108 section 3, "owner retains TEMP where required"):
SELECT has_database_privilege(current_user, current_database(), 'TEMP');                      -- t
-- no live runtime TEMP shadow (must return NO rows):
SELECT n.nspname||'.'||c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname ~ '^pg_temp_[0-9]+$' AND c.relowner = 'igaming_runtime'::regrole;
```

Mapping to the owner invariant: PUBLIC cannot CREATE TEMP (count 0); runtime cannot (`temp f` plus 42501); owner retains
(`t`); a fake TEMP-shadow approval fails because the shadow tables cannot be created (the 42501 above is the structural proof;
the end-to-end attack replay is `TestTempRevoke_K2ShadowAttack*` and `TestTempRevoke_RuntimeRoleCannotCreateAnyTempObject_AllForms`
in `internal/db/temp_revoke_integration_test.go`, LOCAL EVIDENCE ONLY, not re-run on AWS because the image has no Go
toolchain). **Genuine two-person approval succeeds**: not provable from SQL alone. Prove it through the Back Office by
submitting a manual adjustment as staff A and approving it as staff B (second distinct platform staff; `deploy.sh seed-admin`
creates only the first). If the acceptance run does not include this, label that half **NOT VERIFIED ON STAGING (local tests
only)**.

### V6. Live proof that the API's connections are `igaming_runtime` (OPERATOR)

Get the API task private IPs (read-only AWS):

```bash
aws ecs list-tasks --region eu-central-1 --cluster igaming-staging-cluster --service-name igaming-staging-platform-api \
  --query 'taskArns' --output text | xargs aws ecs describe-tasks --region eu-central-1 --cluster igaming-staging-cluster --tasks \
  --query 'tasks[].attachments[].details[?name==`privateIPv4Address`].value[]' --output text
```
Then, as the owner (master), list sessions (rows only; no credentials):

```sql
SELECT usename, application_name, client_addr, backend_type, state, count(*)
  FROM pg_stat_activity
 WHERE datname = current_database() AND backend_type = 'client backend'
 GROUP BY 1,2,3,4,5 ORDER BY usename, client_addr;
```
PASS: every `client_addr` equal to a platform-api task IP has `usename = igaming_runtime`; **no** row with `usename = igaming`
and a platform-api task IP (the only `igaming` row should be this one-off task's own connection, `client_addr` = its IP);
`application_name` is empty (expected, section 1 item 2). Also record `SELECT count(*) FROM pg_stat_activity WHERE usename =
'igaming_runtime'` > 0 (the API is connected). On RDS the master should be able to see other sessions' rows (`rds_superuser`
membership); if `usename`/`client_addr` come back NULL for other sessions, record "insufficient visibility" and rely on V1, V7
and V8 instead (UNVERIFIED which of these applies on RDS).

### V7. `current_user` / `session_user` for the runtime credential (OPERATOR)

There is no authenticated API diagnostic that returns `current_user` (section 1 item 1), and no ECS Exec. The `AS_RUNTIME` run in
V5 already prints `current_user, session_user` for the exact credential the API uses (same Secrets Manager secret, same user
name, V1). Together with V1 (API definition references only that secret), V6 (live backends) and the IAM fact that the API
task cannot read the master secret (section 1), this is the evidence. No code change is needed to close the item (see
section 7 for an optional log line).

### V8. API startup log lines (OPERATOR, read-only AWS)

```bash
aws logs filter-log-events --region eu-central-1 --log-group-name /ecs/igaming-staging/platform-api \
  --filter-pattern '"starting platform-api"' --query 'events[].message' --output text
```
Expected JSON (slog, stdout) lines in order: `"starting platform-api"` with `"environment":"staging"`, then
`"database connected"`, then `"http server listening"` (`cmd/platform-api/main.go:97,128,584`). Absent: any
`platform-api: fatal:`. Note that in `APP_ENV=staging` the TEMP/ownership/membership gate does NOT run, so there is no
"gate passed" line and none should be expected. A successful `/readyz` through the edge confirms the pool is live:
`curl -fsS "$(terraform output -raw api_url)/readyz"` (from an allowlisted IP; non-allowlisted IPs get 403).

Failure behavior if the production gate trips (production or unset `APP_ENV` only): `run()` returns the error,
`main` prints `platform-api: fatal: db: refusing to start in production - ...` to stderr and exits 1
(`cmd/platform-api/main.go:37-42,145-147`); ECS marks the container stopped, the health check never passes, and the service
replaces the task repeatedly (visible in `/ecs/igaming-staging/platform-api` and service events). The messages name the cause:
"owns tables ... point DATABASE_URL at the runtime role's credential", "holds the TEMPORARY privilege ... run the REVOKE
statements of migration 0116", or "is a member of another role". That is the intended fail-closed outcome, not a bug to bypass.

### V9. Migration state (OPERATOR, SQL as owner)

```sql
SELECT max(version), count(*) FILTER (WHERE version = 116) AS has_0116 FROM schema_migrations;  -- expect 117 and 1
SELECT has_table_privilege('igaming_runtime','public.schema_migrations','INSERT');             -- must be f (chained revoke)
```
(`schema_migrations.version` is BIGINT, `internal/db/migrate.go:130-135`.)

### V10. Rotation/recycle drill (OPTIONAL, OPERATOR, only if time within the authorized session)

`deploy.sh migrate` re-runs role-init (idempotent, includes the revoke) and migrate, then force-redeploys; afterwards repeat V5
and V6. This proves the revoke survives a re-run and that old sessions are replaced.

## 5. Stop and rollback conditions

STOP, take no corrective action in the database, and report to the orchestrator if any of the following is observed:

1. V1 shows the master ARN or a user other than `igaming_runtime` on the API task, or V6 shows `igaming` from an API IP.
2. V5 negative test succeeds (a TEMP table can be created by `igaming_runtime`), or `PUBLIC` count > 0, or the owner no longer
   holds TEMP.
3. 0116 fails with `TEMP-REVOKE-1` (database not owned by the migrating role) or role-init/migrate exits non-zero.
4. V4 shows an unexplained login role with TEMP or K2 insert rights, or `runtime_can_create = t`.
5. A runtime TEMP shadow exists (V5 owner query returns rows).

Do not `ALTER ROLE`, `GRANT`, change passwords, or escalate privileges to "make the check pass" (CLAUDE.md "Environment
safety", `docs/governance/incident-2026-09-27-local-db-credential-mutation.md`). The rollback for this ephemeral,
synthetic-data environment is `deploy.sh down` (interactive; then `verify-teardown.sh`), not `migrate down`. Never run
`cmd/migrate down` for 0116: its down file re-grants TEMP to PUBLIC. Idempotent, authorized re-runs are acceptable only as
`deploy.sh migrate` (role-init is idempotent and re-applies the revoke); re-verify V5/V6 afterwards. If the live runtime
sessions pre-date the revoke, force a new deployment of the platform-api service
(`aws ecs update-service --force-new-deployment`) so the pool is rebuilt, then re-run the shadow query.

## 6. What must be authorized separately (nothing here is an approval)

Staging deployment is OFF and requires explicit, separate owner authorization
(`docs/governance/staging-teardown-2026-09-26.md`, "Next staging deployment": one deliberate governed deployment from the
final approved commit, "needs separate human authorization"). Concretely:

| # | Authorization / action | Who | Source |
|---|---|---|---|
| 1 | Explicit owner go-ahead for the next staging deployment, naming the commit; decide whether this verification IS the "one governed deployment" (then bundle full acceptance) or a narrower role-split-only session | owner | teardown record "Next staging deployment"; planning gate P-W4 (`docs/plans/next-real-provider-integration-planning-gate.md:416`) |
| 2 | AWS access: account `765578795051`, region `eu-central-1`, IAM user `claude-staging-deployer` (retained, per teardown record) or another authorized deployment principal. The runbook states the deployment credential was "Not yet requested or authorized" for a new session (`stage-9-4-staging-lifecycle-runbook.md` section 0). Never root; the `claude-staging-readonly` user cannot run tasks. Confirm the four deployer policies are attached and re-run `deploy/aws/tests/simulate-deployer-policies.py` (section 12.1) | owner / AWS account admin | lifecycle runbook sections 0, 1.1, 12 |
| 3 | **ACCESS-ANALYZER-CHECK-1**: IAM Access Analyzer check (both regions) by the account admin, listed as "before the next staging deployment"; the deployer credential is denied `access-analyzer:ListAnalyzers` by design | human (AWS account admin) | `task-registry.md` row ACCESS-ANALYZER-CHECK-1; planning gate line 390 |
| 4 | **HD-10.3-2** (IAM/KMS for `awssm`, endpoint vs proxy, IRSA, account pinning) and **DEPLOY-FPKEY-1** (AWS delivery of `PROVIDER_CREDENTIAL_FINGERPRINT_KEY`): registered as human decisions before the future staging deployment. NOT technically required for this verification: `deploy/` is unchanged by HD-10.3-2, the ECS task sets no `SECRETSTORE_BACKENDS`, and an absent fingerprint key is valid (`internal/config/config.go:424-430`). The owner must nonetheless decide or explicitly defer both for a role-split-only session; if deferred, anything needing `awssm` or the fingerprint key stays STAGING REQUIRED | owner (+ security) | `task-registry.md` DEPLOY-FPKEY-1; ADR 0092/0093 (HD-10.3-2); planning gate lines 93-94, 416 |
| 5 | Cost acknowledgement: lifecycle runbook section 7 estimates about USD 0.10/hour running, about USD 0.40-0.55 for a 4-hour session plus about 0.05-0.10 per create/destroy cycle; an account budget exists. No separate cost approval is recorded in the repository | owner | lifecycle runbook section 7 |
| 6 | Operator network: public IPv4 in `staging_access_cidrs` (git-ignored tfvars) | operator | lifecycle runbook section 0 |
| 7 | After the session: `deploy.sh down` + `verify-teardown.sh` (VERIFY-TEARDOWN-ECS-1 may still exit 1 for ECS tag-index lag) and the Access Analyzer check again | operator | lifecycle runbook section 6 |

Not an AWS gate but relevant to "approved commit" evidence: CI-BILLING-1 (no GitHub CI evidence; local evidence only).

## 7. What remains to close PLAT-ROLESPLIT-1 (ordered by who must act)

Honest scope: staging verification can produce VERIFIED-IN-STAGING evidence for the invariant. It cannot by itself close
PLAT-ROLESPLIT-1 or lower TRIGGER-SEARCH-PATH-1-0116 for real money, because no production environment exists and the
registry rows say the same invariants must hold "in every environment that serves real money". Only `security` may lower the
rating (registry row TRIGGER-SEARCH-PATH-1-0116). (Condition numbering differs: ADR 0108 section 4 lists C1 runtime role, C2
end state verified, C3 no other TEMP/CREATE principal; the registry row lists C1..C4 including gate active, ordering and `down`
guard. Both sets are covered by section 4 above.)

**Engineering (can be done now, recommended, none built here):**

1. RECOMMENDED, small: extend the startup gate to `staging`. Change the first lines of `VerifyRuntimeRoleInProduction`
   (`internal/db/production_safety.go:112-115`) so staging is gated too (for example `environment != "production" &&
   environment != "staging"`), pass `cfg.GuardEnvironment()` as today, and update the tests that assert staging is never gated
   (`internal/db/production_safety_test.go:34-47,97-100,113`) and the ADR 0108 section 5 bullet. Why: it costs nothing where
   the Terraform already connects as `igaming_runtime` (section 1), it turns a future staging misconfiguration (owner credential,
   TEMP restored, role membership) into a visible crash instead of a silent hole, and it is continuous, whereas section 4 is a
   one-time check. Why not / risks: it changes the scope of an established gate that ADR 0108 deliberately left at production
   and defers to `security`; `make run APP_ENV=staging` against the owner `igaming` (Makefile line 26 permits it) would start
   failing; and a first-deploy quirk on RDS (for example an unexpected `pg_auth_members` row for `igaming_runtime`) would
   crash-loop the staging API, which is acceptable because it is exactly the finding to catch, but should be expected.
   Decision belongs to `security` and the owner. It is optional for production closure and should be taken before the staging
   deployment only if they want the gate itself exercised on AWS (then add V11: expect `fatal` lines absent and `/readyz` 200).
2. OPTIONAL, tiny, log-only: log the connecting role once after `db.Connect` (`logger.Info("database role", "user", <current_user>)`
   via `SELECT current_user`), so CloudWatch alone proves the identity. Not a new endpoint. A `/v1/admin` diagnostic returning
   `current_user` is NOT recommended: it adds an authenticated surface for information V6 already provides.
3. OPTIONAL: add `application_name=platform-api` to `database_url_runtime` in `deploy/aws/modules/ecs/main.tf:70` so
   `pg_stat_activity` labels the API's sessions (PROPOSAL only; a Terraform change needs its own review and the ecs
   `terraform test` update). V6 works without it.
4. OPTIONAL, dev only: `make run` and `.env.example` (`DATABASE_URL=postgres://igaming:...`, `.env.example:7`; `Makefile:4,30`)
   run the app as the owner by design (migration-mechanics tests need DDL). Development is explicitly not covered by the 0116
   closure (ADR 0108 section 4 C1). A `make run-runtime` target using the existing `RUNTIME_DATABASE_URL`
   (`Makefile:11`) would let developers exercise the non-owner path; not required.
5. Defence in depth, unchanged: pinning the 0026..0113 functions remains NOT IMPLEMENTED (separate follow-up).

**Operator (needs AWS access and section 6 authorizations):**

6. Run the deployment (`deploy.sh up`) and section 4 V1-V9; capture the evidence below; `deploy.sh down` afterwards.

**Owner / security:**

7. Record the section 6 authorizations; decide the staging-gate question (item 1); after evidence review, `security` decides
   whether registry rows PLAT-ROLESPLIT-1 and TRIGGER-SEARCH-PATH-1-0116 move to "VERIFIED IN STAGING, production cutover
   pending". Final closure needs the same verification on whichever environment first serves real money (production infra
   does not exist; `CLAUDE.md` forbids building it prematurely), including after any restore/recreate (ADR 0108 C2).

## 8. Evidence to capture (names only, no secret values)

- Commit SHA / `terraform output -raw image_tag`; operator principal ARN from `aws sts get-caller-identity` (account and user
  name only); UTC timestamps; task definition revision numbers for `igaming-staging-platform-api` and `-role-init`.
- V1 output (env names, `DATABASE_URL` without password, secret NAMES/ARNs); V2 output (secret name and ARN only).
- V3: transcript lines showing role-init, then migrate, then forced deployment, with exit 0; log stream names.
- V4, V5, V6, V9 SQL output exactly as printed (role names, booleans, counts, `client_addr` of tasks), including the 42501 error
  line, and the platform-api task IP list used in V6.
- V8 log lines (`starting platform-api`, `database connected`, `http server listening`); `/readyz` result.
- Which items were NOT VERIFIED (genuine two-person approval on staging; RDS-specific visibility; master `rolbypassrls`) and
  why. Redact nothing sensitive in because none should be present; if a password appears in any log, STOP and report it.
- Label per CLAUDE.md "No fake completion": result of this procedure is `IMPLEMENTED` (code, local evidence) /
  `PARTIALLY IMPLEMENTED` (until staging evidence exists) / `BLOCKED` on the section 6 authorizations. It is never a claim of
  production readiness or regulatory approval.

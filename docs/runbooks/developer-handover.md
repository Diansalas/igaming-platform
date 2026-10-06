# Developer handover runbook (hands-on procedure)

Companion to [`../HANDOVER.md`](../HANDOVER.md) (read that first). Snapshot 2026-10-06, code HEAD `d149a64`. **Every command below carries its source in `[src: path]`.** Commands whose exact flags I could not find in the repository are marked **unverified**. Nothing here was executed while writing it (a race sweep was running and databases were not touched).

Placeholders in `<angle brackets>` are for you to fill. Database URLs in the repo are synthetic dev/CI placeholders, valid nowhere else.

## 0. Absolute safety rules (read before any command)

1. **Never** use `ALTER ROLE`, `CREATE ROLE`, `sudo` or password changes to fix database access for yourself or an agent. If DB access fails: STOP and report to the owner/orchestrator. [src: `CLAUDE.md` "Environment safety"; `docs/governance/incident-2026-09-27-local-db-credential-mutation.md`]
2. The Makefile targets `dev-db-init-roles` / `dev-db-init-test-admin` use `sudo -u postgres`. They are **for the human owner of the machine, once**; automation and agents never run them. [src: `Makefile`; `docs/runbooks/README.md` step 2b]
3. **Container restarts kill Postgres and background jobs.** Recovery: start the cluster (`service postgresql start`, run it in the background so your shell is not blocked), then wait until `pg_isready` succeeds, then re-check your roles by simply connecting; do not repair roles. [src: `Makefile` target `dev-db-up`; orchestrator facts (handover_facts.md); `pg_isready` is the standard PostgreSQL client tool, unverified in repo]
4. **Disk pressure:** each scratch database and each `-race` binary is large; the 2026-10-05 cleanup moved free disk from 1.9 GB to 14.6 GB after dropping 748 scratch DBs. Check `df -h` before long sweeps; do not run two heavy sweeps at once. A `go test` that hits its default 10-minute timeout leaks scratch databases, so always pass `-timeout 60m` for long packages. [src: task registry rows `CLEANUP-2026-10-05`; `docs/plans/prh2-hardening-round/analysis/cleanup-manifest-2026-10-05.md`]
5. **Use private scratch databases** for your own test runs (section 4). Never run destructive tests against a shared database that someone else's sweep uses. Test code creates and drops its own per-test databases through `internal/testsupport/scratchdb` using the dev/CI-only `igaming_test_admin` role. [src: `docs/testing/testing-strategy.md` "Scratch databases"]
6. **Never delete an unknown database or worktree.** 44 UNKNOWN literal-named databases and 52 agent worktrees are preserved by owner decision. Keep `igaming_orch_local` and `igaming_platform_ci_local`. Classify first (who created it, what references it, is it listed in a manifest). [src: registry `CLEANUP-2026-10-05`; orchestrator facts]
7. Do not touch GitHub billing, Terraform/IAM, or AWS without written owner authorization. [src: HANDOVER section 48]

## 1. Prerequisites

| Tool | Version / note | Source |
|---|---|---|
| Go | `go 1.26.0` in `go.mod` (the repo targets Go 1.26.x; CI uses `go-version-file: go.mod`) | `go.mod`; `.github/workflows/ci.yml` step "Set up Go" |
| PostgreSQL | 16 (CI image `postgres:16`; compose uses `postgres:16`) | `.github/workflows/ci.yml`; `deploy/docker-compose.dev.yml` |
| golangci-lint | **v2.9.0**, pinned in CI. The linter binary must itself be built with Go >= 1.26 or it refuses to run ("the Go language version (go1.25) used to build golangci-lint is lower than the targeted Go version"); v2.9.0 is the first release whose binary is built with go1.26. Config `.golangci.yml` (`version: "2"`) | `.github/workflows/ci.yml` step "golangci-lint" comments; `.golangci.yml` |
| Node.js | 22 (frontends and deploy static checks) | `.github/workflows/ci.yml` |
| govulncheck | `golang.org/x/vuln/cmd/govulncheck@v1.8.0` | `.github/workflows/ci.yml` |
| Docker | optional (compose dev DB). The sandbox this was written in had no Docker daemon | `deploy/docker/README.md` |

Check: `go version` (must say go1.26.x), `psql --version`, `golangci-lint version` (must show v2.9.0 **and** "built with go1.26"). [src: standard tool flags; first two unverified in repo]

## 2. Clone, branch, state

```sh
git clone https://github.com/Diansalas/igaming-platform.git     # remote name 'origin' [src: orchestrator facts]
cd igaming-platform
git fetch --all
git checkout claude/focused-wright-jw88w9     # code HEAD d149a64 at snapshot; docs handover on branch prh2-handover-docs
git status
git log --oneline -20
git branch -vv
git worktree list                              # 52+ agent worktrees under .claude/worktrees/ - inspect only, never remove
```
[src: standard git; branch names and counts from orchestrator facts and `git branch -a`]

Inspect the two **unmerged, ready, merge-pending** branches without checking them out:
```sh
git log --oneline d149a64..prh2-r5-signed-actor-proof     # 8863e31 tip, migration 0120, ADR 0110
git log --oneline d149a64..prh2-r5-stake-return-closure   # 35cd2fb tip, migration 0121
git show prh2-r5-signed-actor-proof:docs/decisions/0110-signed-actor-proof.md | less
```
[src: standard git; SHAs verified with `git log` at snapshot]

## 3. Local development

First-time setup is in [`README.md`](README.md) "Local development setup" [src: `docs/runbooks/README.md`]:

```sh
make dev-db-up            # service postgresql start        [src: Makefile]
# alternative with Docker:
docker compose -f deploy/docker-compose.dev.yml up -d        [src: docs/runbooks/README.md; deploy/docker-compose.dev.yml]
# role/database creation is a HUMAN step: deploy/init-app-role.sql, deploy/init-test-admin-role.dev.sql
#   (make dev-db-init-roles, make dev-db-init-test-admin: use sudo -u postgres; see rule 2 above) [src: Makefile]
make migrate-up           # DATABASE_URL=... go run ./cmd/migrate up  [src: Makefile]
cp .env.example .env      # placeholders only; export the variables   [src: docs/runbooks/README.md; .env.example]
make run                  # APP_ENV=development DATABASE_URL=... go run ./cmd/platform-api [src: Makefile]
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz                                 [src: docs/runbooks/README.md]
```
`make run` defaults `APP_ENV=development`; an unset `APP_ENV` is treated as production and the mock-adapter startup guard refuses to start. `.env.example` placeholders are refused outside development. Dev `make run` connects as the database **owner** (known PLAT-ROLESPLIT-1 dev gap). First admin bootstrap: `SEED_ADMIN_PASSWORD=... go run ./cmd/seed-admin` (the password only via env, never a flag; flags other than the password were not inspected: **unverified**) [src: `cmd/seed-admin/main.go` header comment].

Frontends (each of `b2c`, `backoffice`): `cd <app> && npm ci && npm run build && npm test` [src: `.github/workflows/ci.yml` job `frontend`; `b2c/package.json`, `backoffice/package.json`]; dev server `npm run dev`.

Build and static checks:
```sh
go build ./...                      # make build        [src: Makefile]
go vet ./...                        # make vet          [src: Makefile]
go vet -tags integration ./...      # integration-tag vet (registry row PRH-2-R3-FINAL-VERIFICATION says "vet both tag sets"; the flag spelling is standard go, unverified in repo)
gofmt -l .                          # must print nothing; make fmt-check  [src: Makefile]
golangci-lint run ./...             # make lint; v2.9.0 [src: Makefile; ci.yml]
golangci-lint run --build-tags integration ./...   # UNVERIFIED flag spelling: registry says integration-tagged files are linted on new code, and testing-strategy.md "Linter" says integration-tagged files are NOT linted by default [src: docs/testing/testing-strategy.md "Linter"]
govulncheck ./...                   # [src: ci.yml step govulncheck]
make ci                             # fmt-check vet lint build test  [src: Makefile]
```

## 4. Migrations

```sh
export DATABASE_URL='postgres://igaming:<password>@127.0.0.1:5432/<scratch-or-dev-db>?sslmode=disable'
go run ./cmd/migrate up         # apply all pending                 [src: cmd/migrate/main.go; Makefile migrate-up]
go run ./cmd/migrate status     #                                   [src: cmd/migrate/main.go; Makefile migrate-status]
go run ./cmd/migrate verify     # checksum drift + version gaps ONLY [src: cmd/migrate/main.go; ci.yml step "Verify migrations"]
APP_ENV=development go run ./cmd/migrate -steps=1 down   # dev/staging only [src: Makefile migrate-down; cmd/migrate/main.go checkDownAllowed]
```
- Expected on HEAD `d149a64`: migrations `0001..0119`, 119 pairs (238 files), gap-free. After merging both ready branches: `0001..0121`. [src: `ls migrations`; orchestrator facts]
- `down` runs only when `APP_ENV` is **explicitly** `development` or `staging`; unset or `production` (or any other value) is refused before any DB connection. Flags go **before** the command (`-steps=N down`). [src: `cmd/migrate/main.go` lines 26-40, 120-135]
- `verify` is not a proof of reversibility; CI reversibility covers only the last 4 steps (fresh DB `igaming_reversibility`, `up`, `-steps=4 down`, `up`, `verify`). [src: ci.yml step "Migration reversibility"]
- Forward-only in deployed environments. A restored/new database needs the TEMP revoke re-applied manually (0116): see `docs/runbooks/operational-runbooks.md` section 7 step 5.
- Migration numbers are allocated only by the orchestrator.

### Private scratch database (for your own experiments)
Preferred: let the tests make their own (`scratchdb.New`) by setting `TEST_DATABASE_URL` and `TEST_ADMIN_DATABASE_URL` (section 5); the `igaming_test_admin` role is meant for that helper only (CREATE/DROP of scratch databases), so do not widen its use. If you genuinely need a hand-made private database, a human with legitimate access creates it owned by the `igaming` role (CI does this for its reversibility check: `psql "$TEST_ADMIN_DATABASE_URL" -c "CREATE DATABASE igaming_reversibility OWNER igaming"` [src: ci.yml step "Migration reversibility"]); an agent whose access fails stops and reports. Drop only databases you created: `DROP DATABASE <name>` (no `FORCE`) [src: registry `CLEANUP-2026-10-05` used plain DROP]. Name them with a clear owner prefix so nobody has to classify them later.

## 5. Tests

Environment variables (synthetic values, from CI; locally use your own dev equivalents) [src: `.github/workflows/ci.yml` env block; `Makefile`]:

| Variable | Used for |
|---|---|
| `DATABASE_URL` | app / `cmd/migrate` connection (role `igaming`, the owner) |
| `TEST_DATABASE_URL` | integration tests; the role must be non-superuser, non-BYPASSRLS |
| `TEST_RUNTIME_DATABASE_URL` | runtime-role (`igaming_runtime`) adversarial probes; tests skip cleanly when unset |
| `TEST_ADMIN_DATABASE_URL` | `igaming_test_admin`; used only by `internal/testsupport/scratchdb` to CREATE/DROP scratch DBs; migration/RLS tests skip when unset |
| `JWT_SIGNING_SECRET`, `PROVIDER_CREDENTIAL_FINGERPRINT_KEY` | CI sets published placeholders; fingerprint key is refused unless `APP_ENV=development` |

Commands:
```sh
go test ./...                                   # make test                          [src: Makefile]
go test -race ./...                             # CI "Unit tests (race detector)"    [src: ci.yml]
make test-integration                           # -tags=integration -v ./... (no -race, no -p 1) [src: Makefile]
make test-integration-runtime-role              # only ./internal/db/... with TEST_RUNTIME_DATABASE_URL [src: Makefile]
```
CI integration step (timing lane skipped by name, then run alone) [src: `.github/workflows/ci.yml` steps "Integration tests" and "Timing-sensitive security tests, run alone"]:
```sh
TIMING_LANE_TESTS='TestStoreOutage_DoesNotPinPool|TestStoreOutage_DoesNotPinPool_ProductionPoolSize|TestResolutionIsolation_NormalOperation|TestResolutionIsolation_OneTenantStoreOutage|TestResolutionIsolation_MultipleTenantsOutage|TestResolutionIsolation_SimultaneousOnset_Bounded|TestResolutionIsolation_ConnectionExhaustion|TestResolutionIsolation_FinancialDuringOutage'
go test -race -tags=integration -v -skip "^(${TIMING_LANE_TESTS})\$" ./...
go test -race -tags=integration -v -count=1 -run '^(TestStoreOutage_DoesNotPinPool|TestStoreOutage_DoesNotPinPool_ProductionPoolSize)$' ./internal/providercred/
go test -race -tags=integration -v -count=1 -run '^(TestResolutionIsolation_NormalOperation|TestResolutionIsolation_OneTenantStoreOutage|TestResolutionIsolation_MultipleTenantsOutage|TestResolutionIsolation_SimultaneousOnset_Bounded|TestResolutionIsolation_ConnectionExhaustion|TestResolutionIsolation_FinancialDuringOutage)$' ./internal/httpserver/
```
**Local per-package sweep (what the project's race sweep used):** one package at a time so a container restart loses only one package: `go test -race -tags integration -count=1 -p 1 -timeout 60m -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' <package>` [src: the running sweep's command line, orchestrator; registry rows `PRH-2-R3-FINAL-VERIFICATION`, `CLASSB-COMBINED-TREE-VERIFY-2026-10-06`: `-race -tags integration -count=1 -p 1 -timeout 90m` on payments/reconciliation/adjustment]. `internal/payments` takes about 20-30 minutes under `-race`. The sweep result is **LOCAL evidence**, never CI. The timing lane is run alone, never skipped from reports: its bound (500 ms) is unchanged and it is environment-dependent (ADR 0094; `docs/testing/testing-strategy.md` "Mutation evidence convention and timing lane").

Mutation-kill evidence is recorded per workstream under `docs/plans/payment-readiness/evidence/` and `docs/plans/prh2-hardening-round/` (convention: throwaway copy, control run, restore-and-compare, SUMMARY line, every survivor classified) [src: `docs/testing/testing-strategy.md`].

Static checks for AWS code: `deploy/aws/tests/run-static-checks.sh` (needs Terraform and Node 22) [src: `ci.yml` job `infrastructure`]. Does not touch AWS.

## 6. After the two ready branches merge

1. Check the sweep first (HANDOVER section 36 task 1). 2. `git merge` each branch in turn on a working branch; resolve nothing silently. 3. `go run ./cmd/migrate up` on a fresh scratch DB, then `verify`: must show `0001..0121` gap-free. 4. Re-run: `./internal/actorproof/...`, `./internal/adjustment/...`, `./internal/capability/...`, `./internal/payments/`, `./internal/db/...`, `./internal/httpserver/`, `./internal/casino/`, `./internal/sportsbook/`, `./internal/tenant/...`, `./internal/reconciliation/...` with the sweep command. 5. gofmt, vet, golangci-lint v2.9.0 (Go 1.26-built). 6. Update registry (append-only), `docs/active-stage.md`, `docs/progress.md`, this handover. [src: derived from registry rows and ADR 0110; the package list is my judgement from the branch diff, not a repository-mandated list]

For production-like configuration after 0120: `ACTOR_PROOF_KEYS` and `ACTOR_PROOF_ACTIVE_KID` come from the secret store; see ADR 0110 section 6 and `docs/runbooks/operational-runbooks.md` section 15 (both exist only on branch `prh2-r5-signed-actor-proof` until merged).

## 7. AWS staging procedure (pointers only; OFF and not authorized)

Do not run anything without the owner's written authorization naming the commit. Order and checklists: [`plat-rolesplit-staging-verification.md`](plat-rolesplit-staging-verification.md) (preconditions section 2, order section 3, checks section 4, authorizations section 6) with [`stage-9-4-staging-lifecycle-runbook.md`](stage-9-4-staging-lifecycle-runbook.md). Pre-flight (read-only) [src: lifecycle runbook section 2]:
```sh
aws sts get-caller-identity           # must be the authorized principal, account 765578795051, region eu-central-1
git status --porcelain                # must be empty; images are built from a clean commit
deploy/aws/scripts/verify-teardown.sh # read-only
```
Deploy/teardown scripts: `deploy/aws/scripts/deploy.sh up|migrate|down` (interactive; never `-auto-approve`) [src: lifecycle runbook sections 3 and 6]. Open items first: ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1, HD-10.3-2. AWS is OFF; retained resources are listed in [`../governance/staging-teardown-2026-09-26.md`](../governance/staging-teardown-2026-09-26.md).

## 8. CI and the self-hosted runner

GitHub Actions (`.github/workflows/ci.yml`) is blocked by billing (CI-BILLING-1); do not modify billing. Reproduce its steps locally with the commands above and label the result LOCAL. The Mac self-hosted runner is prepared, not installed or registered: follow [`self-hosted-runner.md`](self-hosted-runner.md) only after the owner separately authorizes it; its workflow `.github/workflows/ci-selfhosted.yml` runs by `workflow_dispatch` only. [src: both files]

## 9. Troubleshooting quick reference

| Symptom | Do | Never |
|---|---|---|
| `password authentication failed` / role missing | STOP, report to owner/orchestrator with the exact error | `ALTER ROLE`, `sudo`, password reset |
| Postgres not running after a container restart | `service postgresql start` (backgrounded), wait for `pg_isready`, re-run the one failed package | recreate roles or drop databases |
| Integration tests all skip | `TEST_DATABASE_URL` / `TEST_ADMIN_DATABASE_URL` / `TEST_RUNTIME_DATABASE_URL` unset | run against a shared DB to "make them run" |
| Timing-lane test fails on a slow host | record the host/load, do not change the bound; see ADR 0094 and the characterization evidence | widen 500 ms, skip silently |
| `golangci-lint` says built with older Go | build the v2.9.0 binary with Go 1.26.x | lower `go.mod` |
| `migrate down` refused | set `APP_ENV=development` explicitly on a dev DB | use it in production |
| Disk nearly full | stop sweeps, check which scratch DBs are yours, drop only those | delete unknown DBs/worktrees |
| Test run left databases behind | classify by creator; plain `DROP DATABASE` of your own only | `DROP DATABASE ... FORCE` on others' |

# Self-hosted CI runner (Mac) — PREPARED, NOT ACTIVE

> **STOP. No setup happens until the repository owner separately and explicitly
> authorizes it.** Nothing in this document has been executed. No agent, no
> script and no workflow in this repository installs, creates or changes
> anything on the owner's Mac or on GitHub (runner registration, billing, plan,
> settings, secrets). Every command in sections 3 to 8 is for the OWNER to run,
> later, by hand, after authorizing it.

## 1. Purpose and status

| Item | State |
|---|---|
| Why | GitHub-hosted Actions are blocked by account billing, registry row **CI-BILLING-1** (runs #365 onward never started). Owner decision 2026-10-05: billing is NOT modified and the plan is NOT upgraded. A controlled self-hosted runner is the already-planned alternative source of authoritative CI evidence (referenced by `docs/decisions/0094-secret-resolution-resource-isolation.md`, "Environment qualification for LOCAL evidence"). |
| Repository preparation | `PREPARED`: this runbook and `.github/workflows/ci-selfhosted.yml` (manual `workflow_dispatch` only). |
| Runner | `NOT IMPLEMENTED`. No runner is registered. A dispatched run would only queue. |
| `.github/workflows/ci.yml` | Unchanged. It keeps targeting GitHub-hosted runners and stays blocked by CI-BILLING-1. |
| CI-BILLING-1 | Stays OPEN. A self-hosted result does not close it and is never described as GitHub-hosted CI. |

Evidence vocabulary (mandatory in every report that quotes a result):

- **LOCAL**: run by an agent or the owner in a dev shell or sandbox.
- **SELF-HOSTED CI EVIDENCE**: a run of `ci-selfhosted.yml` on the registered Mac runner, with the environment lines described in section 3 (timing lane).
- **GITHUB-HOSTED CI**: a run of `ci.yml` on `ubuntu-latest`. None exists since #364.

These three are never merged or relabeled. In particular F-POOL-1 K1 / PRH-FPOOL1
("the next GitHub CI run") are not satisfied by SELF-HOSTED CI EVIDENCE unless the
owner/architect/security explicitly rule so and record it; this runbook does not
decide that.

## 2. Runner labels and machine assumptions

The repository contains NO documented facts about the owner's Mac (model, chip,
macOS version, RAM, disk). **ASSUMPTIONS, to be confirmed by the owner before setup:**

- A1. Apple Silicon (arm64). If the Mac is Intel, replace `ARM64` with `X64` in the three `runs-on` lines of `ci-selfhosted.yml` and in the download file name below.
- A2. A currently supported macOS with Xcode Command Line Tools (`git`, `clang` for cgo, needed by `go test -race`).
- A3. Homebrew is installed under an ADMIN account (Apple Silicon default prefix `/opt/homebrew`; Intel `/usr/local`).
- A4. At least 16 GB free disk and 8 GB RAM. The timing lane's 4-vCPU assumption (ADR 0094) is not known to hold; section 9 records the real CPU count.
- A5. The repository `Diansalas/igaming-platform` is PRIVATE (see section 6). The owner must verify.

Label set (exact). `config.sh` adds `self-hosted`, `macOS` and the architecture
automatically; only `igaming-ci` is passed:

```
self-hosted, macOS, ARM64, igaming-ci
```

`runs-on: [self-hosted, macOS, ARM64, igaming-ci]` in the workflow requires ALL four.

## 3. Required workflow behavior (what `ci-selfhosted.yml` does)

Jobs mirrored from `ci.yml`: `build-test-lint` (Go: guards, gofmt, vet, golangci-lint v2.9.0, `go mod tidy` drift, govulncheck v1.8.0, build, migrations up/verify, unit tests with `-race`, integration tests with `-race -tags=integration` and the same `-skip` list, the isolated timing lane, the evidence assertions, migration reversibility), `frontend` (b2c, backoffice: `npm ci`, `npm run build`, `npm test`, Node 22) and `infrastructure` (`deploy/aws/tests/run-static-checks.sh`, Terraform 1.16.3, Node 22, no AWS credentials). Not mirrored: `frontend-image` (needs a Docker daemon).

Exact integration environment (names only; values are the synthetic CI values already in `ci.yml`):

- Database `igaming_platform_test`. Bootstrap superuser `postgres` (never used by the app; superusers bypass RLS).
- Roles created by the same SQL as `ci.yml`: `igaming` (NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS, owns the database and `public`), `igaming_runtime` (non-owning runtime role, PLAT-ROLESPLIT-1, mirrors `deploy/init-app-role.sql`, plus the post-migration REVOKEs on `schema_migrations` and `sportsbook_bet_settlements`), `igaming_test_admin` (CREATEDB, member of `igaming`, mirrors `deploy/init-test-admin-role.dev.sql`; used only by `internal/testsupport/scratchdb`).
- Env names: `DATABASE_URL`, `TEST_DATABASE_URL`, `TEST_RUNTIME_DATABASE_URL`, `TEST_ADMIN_DATABASE_URL`, `JWT_SIGNING_SECRET`, `PROVIDER_CREDENTIAL_FINGERPRINT_KEY`. The Makefile equivalents (`dev-db-up`, `dev-db-init-roles`, `dev-db-init-test-admin`, `test-integration`, `test-integration-runtime-role`) use `service postgresql start` and `sudo -u postgres`, which are Linux-specific and are NOT used on the Mac; the workflow runs the same SQL files' semantics directly. (Note `TEST_ADMIN_DATABASE_URL`, as in `ci.yml` and the Makefile, is the name in use; the task text's "TEST_ADMIN" refers to it.)
- Deviation from `ci.yml`, deliberate: PostgreSQL listens on `127.0.0.1:54329` (not 5432) so it cannot collide with another Postgres on the Mac. A fresh cluster is created per run in `$RUNNER_TEMP` and deleted in an `always()` step; the workflow refuses to start if the port is already in use.
- GitHub `services:` containers do NOT exist on macOS runners. Postgres therefore comes from Homebrew `postgresql@16` (section 5; the only method wired into the workflow). Docker alternative (NOT wired; would need workflow changes and owner review): Colima, `brew install colima docker` then `colima start --cpu 4 --memory 8` then `docker run -d --name igaming-ci-pg -p 127.0.0.1:54329:5432 -e POSTGRES_PASSWORD=igaming_ci_superuser_password -e POSTGRES_DB=igaming_platform_test postgres:16`. Always bind `127.0.0.1`. Docker Desktop has licensing terms for some organizations; owner to check.

Timing lane (ADR 0094 section 9 and the 2026-10-05 environment qualification amendment): the 8 timing tests are excluded from the main integration step and run in their own step, alone, with `-race -tags=integration -count=1`, one `-run` per package, each test required to PASS by name, no retry, bound 500 ms UNCHANGED. The workflow additionally: (a) makes `frontend` and `infrastructure` wait (`needs`) so nothing overlaps it, and `concurrency: ci-selfhosted` allows one run at a time; (b) waits up to 180 s for the 1-minute load average to be below 2 before each repetition and prints `QUALIFIED` or `UNQUALIFIED` (waiting never alters a bound; an UNQUALIFIED failure is reported FAILING-UNQUALIFIED, never green, never a reason to change a bound); (c) records CPU model, logical CPUs, macOS, Go and PostgreSQL versions, load, per repetition; (d) repeats the lane `timing_repetitions` times (default 5, the ADR minimum). Worst-case latency margin is read from the uploaded `timing-rep-*.log` files; the workflow does not compute it. **For a qualifying run the owner must not use the Mac for anything else while the run is in progress** (no builds, no video calls, no Spotlight indexing bursts, display sleep off, charger connected).

## 4. Security isolation requirements (all mandatory)

1. Repository visibility. Self-hosted runners must be used only if the repository is PRIVATE, or (if it is ever public) with fork pull-request workflows disabled and "Require approval for all outside collaborators" set. Settings > Actions > General: verify BOTH before registering. Executing untrusted PR code on the owner's hardware is remote code execution on that machine.
2. Never run untrusted code. `ci-selfhosted.yml` triggers only on manual `workflow_dispatch` by someone with write access; do not add `pull_request`/`pull_request_target`/`push` triggers to it. Review any workflow change before it can reach the runner.
3. Dedicated unprivileged macOS user (`ghrunner`, STANDARD account, NOT admin, not in `admin` or `_developer` groups), whose home holds only the runner. That user has no access to the owner's Keychain, `~/.ssh`, `~/.aws`, browser profiles, documents or Time Machine. Owner permissions on their own home stay at 700 (verify: `ls -ld /Users/<owner>`). The runner never runs as the owner. Never log into `ghrunner` with iCloud/Apple ID or add credentials to its keychain.
4. No secrets. The workflow uses no `secrets.*` and no cloud credentials; `permissions: contents: read`. Do not add repository/organization secrets that this repository's runner could read. The database passwords in the repo are synthetic CI/dev placeholders (already published in `ci.yml`, `deploy/init-app-role.sql`) and never real credentials. No AWS, no production data, no real provider credentials ever touch this machine (CLAUDE.md "Environment safety").
5. Runner group / scope. Register the runner at repository level (Settings > Actions > Runners of this repository only). If an organization is ever used, restrict its runner group to this repository only.
6. Registration token handling. The registration token (Settings > Actions > Runners > New self-hosted runner) is short-lived (about 1 hour) and is typed or pasted by the OWNER only, directly into the terminal of `ghrunner`. It is never given to an agent, never written to a file, a chat, a commit, shell history shared elsewhere or a log. After registration the runner stores its own credentials in `~ghrunner/actions-runner/.credentials*`, readable by `ghrunner` only; do not copy that directory.
7. Ephemeral vs persistent. `--ephemeral` makes the runner accept ONE job and unregister, which is the strongest isolation, but this workflow has four-plus jobs (`build-test-lint`, `frontend` x2, `infrastructure`), so a single ephemeral registration would run only the first job. DECISION FOR THE OWNER (default proposed: persistent runner, started on demand with `./run.sh`, stopped after the verification session, deregistered when not needed). Use `--ephemeral` only if the owner accepts re-registering with a fresh token for each job, or after the workflow is reduced to one job (that would be a separate reviewed change). JIT configuration (REST API) is not described because it needs an API token, which this runbook does not introduce.
8. Network. Required outbound HTTPS only: `github.com`, `api.github.com`, `*.actions.githubusercontent.com`, `*.githubusercontent.com`, `objects.githubusercontent.com`, `pipelines.actions.githubusercontent.com`, the Go module proxy `proxy.golang.org`, `sum.golang.org`, `go.dev`/`dl.google.com` (toolchain), `registry.npmjs.org`, `releases.hashicorp.com` and the Terraform provider registry `registry.terraform.io`, plus `vuln.go.dev` (govulncheck). The list is an unverified starting point; confirm against real run logs. No inbound ports. PostgreSQL listens on `127.0.0.1` only.
9. Disk and cleanup. Go build/module caches (`~ghrunner/go`, `~ghrunner/Library/Caches/go-build`), `_work/`, and npm caches grow; the Terraform provider cache is per run. After sessions: `rm -rf ~ghrunner/actions-runner/_work/*` and `go clean -cache` (as `ghrunner`). The Postgres data directory is removed by the workflow's final step.
10. Updates. Keep the runner on a current release (the `golangci-lint-action@v9` needs the node24 Actions runtime); macOS and Homebrew updates are the owner's.

## 5. Exact setup commands (OWNER, LATER, only after separate authorization)

Placeholders: `<OWNER_ADMIN>` is the owner's admin account; `<RUNNER_VERSION>` and `<SHA256>` come from the "New self-hosted runner" page on GitHub (use macOS / ARM64 there); `<REGISTRATION_TOKEN>` is the one-hour token, typed by the owner only.

```sh
# 5.1 As <OWNER_ADMIN>: create the dedicated standard (non-admin) user.
#     Omitting -admin creates a standard user. You are prompted for the password.
sudo sysadminctl -addUser ghrunner -fullName "GitHub CI Runner" -password -
#     Verify it is NOT an admin (must print nothing for the admin group check):
dscl . -read /Groups/admin GroupMembership | tr ' ' '\n' | grep -x ghrunner || echo "ghrunner is not admin: OK"

# 5.2 As <OWNER_ADMIN>: Xcode CLT (skip if already installed) and PostgreSQL 16.
xcode-select --install          # interactive; skip if `xcode-select -p` already prints a path
brew install postgresql@16      # installs binaries only; do NOT run `brew services start postgresql@16`
#     The workflow starts and deletes its own throwaway cluster. Confirm binaries:
ls /opt/homebrew/opt/postgresql@16/bin/initdb      # Intel: /usr/local/opt/postgresql@16/bin/initdb
#     Make the binaries readable/executable by ghrunner (Homebrew prefixes normally are).

# 5.3 Go, Node, Terraform: NO manual install is needed. The workflow's
#     actions/setup-go (go-version-file go.mod: go 1.26.0, toolchain go1.26.8),
#     actions/setup-node (Node 22; there is no .nvmrc and package.json has no
#     "engines"; CI uses 22) and hashicorp/setup-terraform (1.16.3) download them
#     per run, and golangci-lint v2.9.0 comes from golangci/golangci-lint-action@v9.
#     Optional manual pins for offline debugging only:
#       brew install go node@22 ; golangci-lint v2.9.0 via its release binary.

# 5.4 As ghrunner (e.g. `sudo -u ghrunner -i`, or a fast-user-switch login): download the runner.
cd ~ && mkdir -p actions-runner && cd actions-runner
curl -o actions-runner-osx-arm64-<RUNNER_VERSION>.tar.gz -L \
  https://github.com/actions/runner/releases/download/v<RUNNER_VERSION>/actions-runner-osx-arm64-<RUNNER_VERSION>.tar.gz
echo "<SHA256>  actions-runner-osx-arm64-<RUNNER_VERSION>.tar.gz" | shasum -a 256 -c
tar xzf ./actions-runner-osx-arm64-<RUNNER_VERSION>.tar.gz

# 5.5 As ghrunner: register. The owner types the token; nothing else holds it.
./config.sh --url https://github.com/Diansalas/igaming-platform \
  --token <REGISTRATION_TOKEN> \
  --name igaming-mac-ci-1 \
  --labels igaming-ci \
  --work _work
#     Add --ephemeral only if you chose that in section 4 item 7 (one job per registration).
#     If prompted for the runner group, keep Default (repository-level runner).

# 5.6 Start it. DEFAULT (on demand, in the foreground, stop with Ctrl-C):
./run.sh
#     OPTIONAL always-on service, only if the owner decides so later (macOS
#     LaunchAgent for ghrunner; it runs only while ghrunner has a login session,
#     which is an assumption to verify):
#       ./svc.sh install ghrunner     # then:  ./svc.sh start   |  ./svc.sh status
```

## 6. Verification steps (after the owner has run section 5)

1. GitHub: Settings > Actions > Runners shows `igaming-mac-ci-1` with status **Idle** and labels `self-hosted, macOS, ARM64, igaming-ci`.
2. Terminal of `./run.sh` shows `Listening for Jobs`.
3. Confirm repo visibility / fork settings (section 4 item 1) and that no `secrets` exist that the runner could read.
4. Free the Mac of other work, then Actions > "CI (self-hosted, manual)" > Run workflow on the intended branch/commit (`timing_repetitions` 5). The orchestrator may start the dispatch only if the owner authorizes it.
5. Expected log lines: `Runner name: 'igaming-mac-ci-1'`; step "Record runner environment" prints `SELF-HOSTED CI EVIDENCE`, `cpu model: ...`, `go: go version go1.26.8 darwin/arm64`; "Start throwaway PostgreSQL" prints a `PostgreSQL 16.x` version; `packages ok:` / `tests failed: 0` in the assertion step; five lines `timing lane repetition N/5: all 8 PASS by name (QUALIFIED)`.
6. Failure semantics are the same as `ci.yml` (a failing test fails the job). First-run failures that are pure environment mismatches (macOS vs Linux tooling, e.g. BSD `grep`/`sed` in `deploy/aws/tests/run-static-checks.sh`, Terraform provider download blocked by egress) are expected possibilities (UNVERIFIED assumption that the scripts run unmodified on macOS). Report them as BLOCKED/PARTIALLY IMPLEMENTED and do not weaken any gate.

## 7. Handoff procedure

Orchestrator hands the owner: this runbook, the commit SHA containing `ci-selfhosted.yml`, and the question list in section 2 (A1 to A5) with the owner's answers needed back.

Owner must: (1) explicitly authorize the Mac setup in writing (separate from this task); (2) answer A1 to A5; (3) decide persistent vs ephemeral (section 4 item 7); (4) run section 5 personally; (5) confirm Idle; (6) dispatch (or authorize dispatching) the workflow with the Mac otherwise idle.

Evidence back to the orchestrator: run URL and run ID, head SHA, per-job conclusions, the artifact `selfhosted-ci-evidence` (`selfhosted-evidence.txt`, `integration-test.log`, `timing-tests.log`, `timing-rep-*.log`), the CPU model, load and QUALIFIED/UNQUALIFIED lines, and the worst observed latency of the timing tests with its margin to 500 ms. The orchestrator records the outcome in the task registry labeled exactly **SELF-HOSTED CI EVIDENCE** (never "CI", never "LOCAL", never "GITHUB-HOSTED CI") and states that CI-BILLING-1 remains open. Whether this evidence satisfies F-POOL-1 K1 / PRH-FPOOL1 is a ruling for architect + security + owner, not assumed here.

## 8. Teardown and removal (OWNER)

```sh
# As ghrunner, stop the runner (Ctrl-C of ./run.sh, or if a service was installed):
cd ~/actions-runner
./svc.sh stop ; ./svc.sh uninstall           # only if the service was installed
# Deregister. A removal token comes from Settings > Actions > Runners > the runner > Remove;
# alternatively remove the runner there in the GitHub UI.
./config.sh remove --token <REMOVAL_TOKEN>
cd ~ && rm -rf ~/actions-runner ~/go ~/Library/Caches/go-build
# Any leftover throwaway Postgres (normally already removed by the workflow):
pgrep -fl 'postgres.*54329' ; pkill -f 'igaming-pgdata' || true
# As <OWNER_ADMIN>: optionally remove the user and the Homebrew package.
sudo sysadminctl -deleteUser ghrunner
brew uninstall postgresql@16
```

After removal, verify in GitHub that no runner remains listed.

## 9. Open assumptions and limits (summary)

Unverified: Mac model/arch/OS (A1 to A5); that `run-static-checks.sh` and the guard steps run on macOS BSD tools; the egress host list; that `golangci-lint-action@v9` and `setup-*` actions behave on the macOS self-hosted image; that `svc.sh` LaunchAgent fits an unattended Mac. Timing-lane margins on this host are unknown until measured. No gate has been weakened or changed: every `ci.yml` command, `-skip` list, name guard and the 500 ms bound is retained.

**STOP: no setup, registration, installation or dispatch happens until the owner separately authorizes it.**

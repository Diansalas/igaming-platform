# Staging acceptance suite (black-box, mock providers, synthetic data)

Status: `IMPLEMENTED` as a test tool and run against a local `platform-api` (see
`docs/plans/staging-acceptance-suite-record.md` for the exact local results, the platform gaps it found, and what it
cannot prove). It has **not** been run against the AWS staging stack: that is the operator's step.

The suite is a single Go program in this directory. It talks to the **public API URL only** (the CloudFront `api_url`
from an allow-listed machine), uses MOCK providers and synthetic players, never touches AWS, never calls a real
provider, and never prints a secret. It implements the acceptance flows of `docs/runbooks/aws-staging-deploy-plan.md`
sections 6.3 to 6.6, 9.4, 9.5 and 10 and of `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md` section 4.

It is built with the build tag `stagingacceptance` on every file, so it is never compiled or run by `go build ./...`,
`go vet ./...`, `go test ./...` or CI (`go list ./...` does not even list the package).

## 1. Run it

```sh
# from the repository root, on a machine inside staging_access_cidrs
export BASE_URL="$(cd deploy/aws/environments/staging && terraform output -raw api_url)"   # https://<id>.cloudfront.net
export TENANT=<synthetic tenant slug>  BRAND=<synthetic brand slug>
export ADMIN_EMAIL=<first platform admin>  ADMIN_PASSWORD_FILE=/secure/admin.pw          # 0600
export ADMIN_TOTP_SECRET_FILE=/secure/admin.totp                                          # 0600, see section 3
export RESULTS_FILE=./staging-acceptance-results.txt
go run -tags stagingacceptance ./deploy/aws/acceptance
# or: go build -tags stagingacceptance -o staging-acceptance ./deploy/aws/acceptance && ./staging-acceptance -help
```

Exit codes: `0` no FAIL (skips allowed); `1` at least one FAIL; `2` invalid configuration or unwritable results file;
`3` only with `-strict`: no FAIL but a prerequisite SKIP remains. The run takes under a minute when nothing waits;
`-prove-stale-stepup` adds about 5.5 minutes, `RECON_WAIT_SECONDS` adds up to the value you give.

Unit tests of the harness itself (TOTP against the RFC 6238 vectors, scrubber, config validation, secret-file mode,
enrolment, "a failed prerequisite never turns into a PASS"):

```sh
go test -tags stagingacceptance -count=1 ./deploy/aws/acceptance
```

## 2. Inputs

Secrets are **file paths** (mode 0600; a group/other-readable file is refused). Nothing secret is ever an argument or a
value in the environment, and the program prints no secret or token. Flags override the environment; the names below
are the environment form.

| Variable | Required | Meaning |
|---|---|---|
| `BASE_URL` | yes | public API URL (CloudFront `api_url`); no credentials, query or fragment |
| `TENANT`, `BRAND` | yes | slugs of the synthetic tenant and brand under test. They must already exist and be `active` (plan 6.5). `TENANT` is also the tenant slug tenant staff sign in with |
| `ADMIN_EMAIL`, `ADMIN_PASSWORD_FILE`, `ADMIN_TOTP_SECRET_FILE` | yes | first platform admin (`deploy.sh seed-admin`). The TOTP file is an existing base32 secret, or the path the new secret is written to on first enrolment |
| `ADMIN_BOOTSTRAP_TOKEN_FILE` | first run only | single-use enrolment token that `cmd/seed-admin` printed once (copy it from the one-off task log into a 0600 file) |
| `ADMIN2_*` | for 6 checks | second platform admin (four-eyes kill-switch release, stale step-up probe). Created by `seed-admin -break-glass -force-additional` or by an approved `platform_invite` |
| `TENANT_ADMIN_*` | for configuration | tenant staff, role `tenant_admin` of `TENANT`: applies provider capabilities and the casino opt-in; reads RG enforcement state |
| `FINANCE1_*`, `FINANCE2_*` | withdrawal flow | two **distinct** tenant `finance` staff, each linked to a distinct Person |
| `COMPLIANCE_*` | KYC | tenant `compliance` staff: the mock KYC vendor never approves by itself, a reviewer must (and it is what gates withdrawals) |
| `RISK_MANAGER_*` | optional | tenant `risk_manager`: sole holder of `sportsbook_settlement:simulate` |
| `<ROLE>_EMAIL`, `_PASSWORD_FILE`, `_TOTP_SECRET_FILE`, `_BOOTSTRAP_TOKEN_FILE` | | the four variables every actor takes (`<ROLE>` is `ADMIN`, `ADMIN2`, `TENANT_ADMIN`, `FINANCE1`, `FINANCE2`, `COMPLIANCE`, `RISK_MANAGER`) |
| `RESULTS_FILE` | no | default `./staging-acceptance-results.txt` (created 0600) |
| `EXPECT_APP_ENV` | no | default `staging`; compared with the environment the running process reports in launch-readiness |
| `CORS_ORIGIN` | no | an origin that `CORS_ALLOWED_ORIGINS` allows; without it the CORS check SKIPs |
| `EDGE_HEADERS` | no | `auto` (required when `BASE_URL` is https), `require`, `skip` |
| `MARKET_EXPECT` | no | `open` (default) or `closed`, see section 5 |
| `PLAYER_RESIDENCE_COUNTRY`, `KYC_VERIFIED_RESIDENCE_COUNTRY` | no | residence evidence parameters, see section 5 |
| `ASSET_CODE` (`EUR`), `SECOND_ASSET` (`USD`, empty disables), `DEPOSIT_AMOUNT` (10000), `WITHDRAW_AMOUNT` (2500), `STAKE_AMOUNT` (1000) | no | synthetic amounts in minor units |
| `CONFIGURE` | no | default `true`: idempotent PUTs of the mock-payments and mock-casino capabilities, one catalogue game, and the tenant opt-in. `false` expects them to be pre-configured |
| `KILL_SWITCH_DRILL` | no | default `true`: engage a deposit kill switch (the sanctioned synthetic alert), prove it blocks deposits, release it through four-eyes. Needs `ADMIN` and `ADMIN2` |
| `SECOND_TENANT` | no | default `acceptance-tenant-b`: isolation tenant created (pending, idempotent by slug) through the platform admin API; empty disables |
| `SECOND_ACTIVE_TENANT`, `SECOND_ACTIVE_BRAND` | no | an owner-provisioned ACTIVE second tenant with open market rows, enabling end-to-end cross-tenant player checks |
| `PROVE_STALE_STEPUP` / `-prove-stale-stepup` | no | waits past the critical step-up window and proves a stale second factor is refused |
| `RECON_WAIT_SECONDS` | no | wait for the first hourly reconciliation run before giving up (SKIP) |
| `RUN_ID`, `-timeout`, `-only <regexp>`, `-strict`, `-debug` | no | `-debug` prints scrubbed request/response lines to stderr; use it locally, not for evidence |

An actor that is not configured is not an error: every check that needs it reports `SKIP [prerequisite]` naming the
missing variable. The first platform admin is the only mandatory identity.

## 3. Staff MFA handling (first platform admin and the others)

- `cmd/seed-admin` creates the first `platform_admin` and prints a single-use bootstrap enrolment token **once**.
  Put it in `ADMIN_BOOTSTRAP_TOKEN_FILE` (0600) and set `ADMIN_TOTP_SECRET_FILE` to a path that does not exist yet.
- On the first run the suite redeems the token, starts a TOTP enrolment, **writes the TOTP secret to
  `ADMIN_TOTP_SECRET_FILE` (0600) before confirming the factor** (a crash can never leave an enrolled account whose
  secret was lost; it refuses to enrol when it has nowhere to keep the secret), confirms with a code it computes
  itself, and writes the ten recovery codes to `<totp file>.recovery-codes` (0600). Neither is printed or logged; the
  scrubber also replaces them if they ever reach an output line.
- Later runs read the secret from the file and log in with a computed code. Because the server rejects a TOTP step it
  has already accepted, the suite never reuses a step, records the last step used in `<totp file>.laststep`, and waits
  for the next 30 second step only when a window is used up.
- **Keep the secret and recovery-code files.** Losing the secret of the only platform admin means an MFA reset or
  break-glass (`docs/runbooks/staff-lifecycle-and-mfa-recovery.md` sections 4 and 6).
- Step-up: the suite refreshes the second factor (`POST /v1/staff/auth/step-up`) right before each financial or
  critical action; `auth.stepup_critical_route` proves the gate with the critical `POST /v1/admin/tenants` route and an
  empty body (400 after the gate, nothing created).

## 4. Prerequisites the suite does not create (and the check that reports each)

| Prerequisite | Who | Reported by |
|---|---|---|
| Stack deployed, access CIDR allow-listed for the machine running the suite | operator | `health.*` |
| `APP_ENV=staging`, `TEST_SUPPORT_ENDPOINTS_ENABLED=true`, `MOCK_GEOLOCATION_COUNTRY` set (plan G3, owner action A) | Terraform | `ledger.deposit_settle_via_mock_callback`, `player.activation_seam` SKIP when the routes are absent |
| Synthetic tenant and brand **active** (launch governance) | owner / two platform admins | `setup.resolve_tenant_brand` |
| Operating-market rows for the mock country, licence ceiling and tenant opt-in (plan 6.5, owner action C; there is no route) | owner | `platform.readiness_market_config`, `player.register` |
| `actor_proof_keys` row (owner action B) | owner | not needed by this suite's flows; governed writes (launch decisions, K1/K2/K3, RG floors) answer `AP002` without it |
| Tenant staff accounts: `tenant_admin`, two `finance`, `compliance` (and optionally `risk_manager`) with Persons, each with a bootstrap token | owner | `setup.signin_*`, then the dependent flows SKIP |
| Alert route authored and approved by two platform admins (plan 10.2) | platform admins | `platform.readiness_alerts_item` (SKIP until then) |

**Tenant staff cannot be created through any supported staging path today** (finding SA-1 in the record): invitations
are delivered by the MOCK e-mail provider, which keeps the token in process memory, and `cmd/seed-admin` creates
platform admins only. Until a reviewed owner procedure or tool exists, the withdrawal four-eyes, KYC approval and
capability-configuration flows cannot run in staging and report `SKIP [prerequisite]`. That is a platform gap, not a
suite limitation, and it is the largest reason for a non-green staging acceptance.

## 5. Market evidence dependency (KYC-residence evidence)

The market gate refuses registration, deposit initiation and casino launch (and sportsbook placement) unless evidence
of an open country exists (`docs/runbooks/market-blocking-configuration.md`). The suite makes the evidence setup a
parameter and does not assume the not-yet-merged `market-evidence-kyc-residence` branch:

- `MARKET_EXPECT=open` (default): the market is expected open. The mock country comes from the stack's
  `MOCK_GEOLOCATION_COUNTRY` plus the owner's market rows; `platform.readiness_market_config` must show
  `R-MARKET-CONFIG=pass`, and registration, deposit, launch and bets must succeed.
- `MARKET_EXPECT=closed`: run against a brand **without** market rows (or with the country closed). `player.register`
  must be refused `403 MARKET_NOT_AVAILABLE` (fail closed, one fixed body with no country or location), readiness must
  not pass, and every money flow reports `SKIP [prerequisite]` because it is unreachable by design. Run it once per
  evidence configuration change to prove the fail-closed default.
- `PLAYER_RESIDENCE_COUNTRY=XX`: after activation the synthetic player declares this residence
  (`PUT /v1/me/residence`, self-declared, today informational).
- `KYC_VERIFIED_RESIDENCE_COUNTRY=XX`: the compliance reviewer attaches this verified-residence determination when
  approving the mock KYC (`verified_residence_country`; the API refuses it unless jurisdiction evidence collection is
  active for the tenant).

**Dependency to resolve when that branch merges:** if the staging configuration then requires verified-residence
evidence *before* registration or deposit, the suite's order (register, activate, deposit, KYC later) will see
`MARKET_NOT_AVAILABLE` at the first money step and must be re-ordered or given the evidence earlier. Today the evidence
the gate consumes is the geolocation locator result only. Supply whatever evidence the staging config needs through
these parameters; do not edit the suite to hide a refusal.

## 6. Reading the results

`RESULT|<check name>|PASS|FAIL|SKIP|<detail>` lines are written to `RESULTS_FILE` (0600) and to stdout, followed by
`META|`, `SUMMARY|` and `EVIDENCE|` lines. `#` lines are comments.

- `FAIL` is a violated expectation; the detail names the request id (`acc-<run>-NNNN`) to look up in CloudWatch.
- `SKIP` always carries a class in square brackets: `[prerequisite]` an input or owner action is missing (supply it);
  `[blocked-by-fail]` an earlier FAIL made the check impossible (fix that first); `[operator-evidence]` the property is
  not observable through the API and the operator proves it (section 7); `[platform-gap]` the platform offers no
  black-box path (reported in the record). A skip is never a pass; `-strict` turns remaining prerequisite skips into
  exit 3.
- `SUMMARY|` gives the counts (`pass`, `fail`, `skip` and the four skip classes), `http_requests`, `duration_seconds`
  and the five slowest checks. `EVIDENCE|request|...` lists, for every check, its first request id and any 5xx, which is
  enough to find the structured log lines.
- Check names group by the plan section they evidence: `health.*` (6.3), `auth.*` and `setup.*` (6.4), `ledger.*`,
  `casino.*`, `sportsbook.*`, `kyc.*`, `withdrawal.*`, `rg.*`, `market.*`, `iso.*`, `recon.*` (6.6, 9.5),
  `alerting.*`, `killswitch.*` (9.4, 10), `observability.*` (9.4), `platform.*` (6.1, 6.2 as far as the API shows them).

Expected side effects that are not defects: `player.token_refresh_rotation` replays a rotated refresh token on purpose, so
each run leaves one `auth.session_reuse_detected` audit row; a wrong-password player login and an unknown-identity staff
login leave failed-login audit rows; `staff.platform_break_glass_used` (p1) is raised only if you created ADMIN2 that
way. HTTP 429 from the per-IP rate limits (login 60/min, register and credential 20/min, MFA 30/min) is retried up to
three times with a back-off; the count is `META|retries_429`.

The suite writes synthetic rows that cannot be deleted (append-only ledger, audit): players `acc-<run>-*@acceptance.invalid`,
deposits, a casino round, a sportsbook bet, withdrawals, a payment kill switch engaged and released, an isolation tenant
with a pending brand (reused by slug on later runs), one catalogue game, provider capability rows. That is the point of
plan 6.6 (the database gets ledger, audit and alert rows). It never switches RG enforcement on (one-way) and never
releases a kill switch it did not engage (`setup.no_stale_containment` fails a run that starts while one is engaged).

## 7. Operator-only evidence (what the suite reports as SKIP)

1. **Non-allow-listed networks get 403** (`health.edge_allowlist_403`): from a network outside `staging_access_cidrs`
   (for example a phone on mobile data) every `api_url`, `staging_url` and `backoffice_url` path returns 403 from the
   CloudFront viewer function; record it once.
2. **Build/version and migration head** (`platform.version_evidence`, `platform.migration_head`): no HTTP surface
   reports them. Evidence: the `migrate` task log (`migrate verify: all applied migrations verified clean`, the
   `status` line, plan 6.1) and the `starting platform-api environment=staging` line below. The suite does show the
   environment the process reports (`platform.environment_anchor`, via launch-readiness).
3. **Alert delivery** (`alerting.delivery_to_receiver`, plan 10.1/10.2): `killswitch.engage_raises_alert` raises the
   durable p2 alert `payment.kill_switch_engaged`; the operator checks the HMAC-signed webhook arrived at the receiver
   and that nobody is e-mailed (the e-mail channel is MOCK). `R-ALERTS` passes only once p1 and p2 routes are approved.
4. **Metrics** (`observability.metrics`): there is no scrape path out of the task (plan G4); `health.metrics_not_public`
   proves `/metrics` is not served on the public URL.

### CloudWatch Logs Insights queries

Log group `/ecs/igaming-staging/platform-api` (JSON lines; `request_id` is the `X-Request-Id` the suite sent, or the id
the server generated):

```
# every request of one suite run, in order
fields @timestamp, request_id, method, path, status, duration_ms
| filter msg = "http_request" and request_id like /^acc-<RUN_ID>-/
| sort @timestamp asc

# the first request of a failing check (request id from the FAIL detail)
fields @timestamp, level, msg, request_id, path, status, error
| filter request_id = "acc-<RUN_ID>-0123"
| sort @timestamp asc

# 5xx during the run window
fields @timestamp, request_id, method, path, status
| filter msg = "http_request" and status >= 500
| sort @timestamp desc | limit 50

# startup evidence: environment and listener of each task start
fields @timestamp, msg, environment, addr
| filter msg in ["starting platform-api", "database connected", "http server listening"]
| sort @timestamp desc | limit 20

# kill-switch drill and break-glass alerts as logged
fields @timestamp, level, msg, request_id, provider_scope, operation_scope
| filter msg like /kill_switch|break_glass/
| sort @timestamp desc | limit 20

# auth refusals during the run (no secrets are logged; bodies are never logged)
fields @timestamp, request_id, path, status
| filter msg = "http_request" and request_id like /^acc-<RUN_ID>-/ and status in [401, 403]
```

The structured `http_request` line carries `request_id`, `method`, `path`, `status`, `duration_ms` and `tenant_id`.

## 8. What the suite cannot prove

See `docs/plans/staging-acceptance-suite-record.md` section 6 for the full list. In short: it cannot prove a mock
payout completes (no black-box settlement exists), anything about the database or AWS (roles, backups, PITR,
migrations, alarms), real PSP/KYC/geolocation behaviour (all MOCK), legal or regulatory approval, alert delivery to a
person, non-allow-listed edge behaviour, metrics, a signed provider callback with a valid signature (the secrets are
in-process), concurrency under load, or multi-replica behaviour (`deploy.sh scale 2` is a separate step: re-run the
suite against the scaled stack).

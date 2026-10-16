# Staging acceptance suite: record (QA)

Branch `staging-acceptance-suite` (from `claude/focused-wright-jw88w9`). Deliverable: `deploy/aws/acceptance/` (Go,
build tag `stagingacceptance`) and `deploy/aws/acceptance/README.md` (how to run, inputs, CloudWatch queries, limits).
Scope: the operator's black-box acceptance of the deployed AWS staging stack (plan `docs/runbooks/aws-staging-deploy-plan.md`
sections 6.3 to 6.6, 9, 10, 12; `stage-9-4-staging-lifecycle-runbook.md` section 4). MOCK providers and synthetic data
only. No AWS call, no real provider call, no secret in any file, no core behaviour changed.

## 1. Labels (CLAUDE.md vocabulary)

| Item | Label | Basis |
|---|---|---|
| The suite as a tool (harness, 107 named checks, scrubber, TOTP/MFA automation, results/evidence format) | `IMPLEMENTED` | built, vetted (plain and tagged), linted, 13 tagged unit tests + 1 untagged guard test pass; run end to end against a local `platform-api` (section 3) |
| Evidence that the AWS staging stack passes the suite | `NOT IMPLEMENTED` | not run against AWS (out of scope for this task); the operator's step |
| Health, headers, CORS, request id, auth/MFA/step-up, registration/activation/login, deposit and callback idempotency, wallet per asset, casino mock bet/win/rollback, sportsbook mock bet/settlement, RG default-off and time-out, withdrawal four-eyes and decline path, tenant/ownership isolation, reconciliation views, alerts API, kill-switch drill | `IMPLEMENTED` (against a local stack, MOCK providers) | section 3 |
| Edge security headers, `/metrics` not public | `IMPLEMENTED` for the check; the https/CloudFront header path is covered by a unit test against a TLS stub, not a real edge | `suite_test.go` |
| "Mock payout completes -> ledger balanced" | `BLOCKED` (platform gap SA-2) | no black-box way to settle a mock payout; the suite proves the in-flight state, the hold and the decline-releases-hold path instead |
| Withdrawal four-eyes, KYC approval, capability configuration in a real staging stack | `BLOCKED` until SA-1 is resolved | tenant staff cannot be created through any supported staging path |
| Version / migration head / database anchor evidence | `STUB` (reported SKIP, operator evidence) | no HTTP surface exposes them (SA-3); environment is shown via launch-readiness |
| Alert delivery to a receiver, non-allow-listed 403, metrics scrape, CloudWatch log reading | `PROVIDER DEPENDENT` / operator evidence | README section 7 |

## 2. What was built

Single program, standard library plus the module only (no new dependency; no import from `internal/`, the TOTP is an
independent RFC 6238 implementation that also proves the server against the RFC). Files: `main.go` (run order, exit
codes), `config.go` (env/flags, validation), `staff.go` + `totp.go` (staff login, first-time enrolment, MFA, step-up),
`world.go` + `report.go` + `httpc.go` (check runner, scrubber, `RESULT|name|PASS/FAIL/SKIP|detail`, evidence summary),
`checks_*.go` (flows), `suite_test.go`, `tag_guard_test.go`.

Properties, each pinned by a unit test or observed in the local runs:

- Secrets are 0600 file paths only (a group/other-readable file is refused); the program never prints them. The TOTP
  secret of a first-time enrolment is written to the operator-designated 0600 file **before** the factor is confirmed
  (it refuses to enrol with nowhere to keep it); recovery codes go to a sibling 0600 file. A scrubber (literal secrets
  plus JWT/Bearer/otpauth/opaque-token/`password|token|secret` shapes, six-digit codes) filters every line that leaves the
  process. Leak scan of all local outputs against all 209 secret values used in the runs: 0 hits; no JWT or otpauth text
  in any output.
- TOTP: the server rejects a replayed step, so the suite never presents a step twice, persists the last step
  (`<file>.laststep`) across runs and waits for the clock only when its window is used up.
- A failed prerequisite never becomes a PASS: dependents report `SKIP [blocked-by-fail]`; a check that reaches no verdict
  FAILs; SKIP classes are `prerequisite`, `blocked-by-fail`, `operator-evidence`, `platform-gap`; `-strict` makes remaining
  prerequisite skips exit 3. Exit codes: 0, 1 (FAIL), 2 (invalid config), 3 (`-strict`).
- Every request carries `X-Request-Id: acc-<run>-NNNN`; the results list the first request id of each check and every 5xx.
- The suite never enables RG enforcement (one-way), never releases a kill switch it did not engage (it FAILs early if one
  is already engaged: `setup.no_stale_containment`), and always releases the one it engages (`killswitch.cleanup_not_left_engaged`).
- Market evidence is a parameter (`MARKET_EXPECT`, `PLAYER_RESIDENCE_COUNTRY`, `KYC_VERIFIED_RESIDENCE_COUNTRY`); the
  `market-evidence-kyc-residence` branch is not assumed (README section 5 names the ordering dependency).

## 3. Local verification (exact results)

Environment: `cmd/platform-api` built from this tree, `APP_ENV=development`, `TEST_SUPPORT_ENDPOINTS_ENABLED=true`,
`MOCK_GEOLOCATION_COUNTRY=MT`, the Terraform variable set (`STAFF_MFA_*`, `PAYOUT_INSTRUMENT_*`, `ACTOR_PROOF_*`,
`METRICS_ADDR`, `ALERT_RETENTION_DAYS`, `RECONCILIATION_INTERVAL_SECONDS=20` so reconciliation runs in the window), runtime
database role on a private scratch database (`PRIV_DB=acc_suite`, own port 18089, stopped by pid). Provisioning that has
no API (as in staging) was done by an untracked local fixture test (active tenant+brand, licence ceiling and tenant opt-in
for MT, tenant staff with Persons) and `cmd/seed-admin` (two platform admins, bootstrap tokens). Plain HTTP, so the
CloudFront header check SKIPs locally.

| Run | Setup | Checks | PASS | FAIL | SKIP (prereq / op-evidence / gap / blocked) | HTTP requests | Duration | Exit |
|---|---|---|---|---|---|---|---|---|
| A. first run, all six actors enrol in-run (earlier build, 104 checks) | 2 platform admins, 4 tenant staff, tenant active with market rows | 104 | 92 | 0 | 12 (5 / 6 / 1 / 0) | n/a | 29.9 s | 0 |
| B. final build, full, second active tenant for cross-tenant player checks | same + `SECOND_ACTIVE_TENANT` | 107 | 95 | 0 | 12 (5 / 6 / 1 / 0) | 229 | 9.3 s | 0 |
| C. repeat of B (stored TOTP secrets, replay-window waits) | same | 107 | 95 | 0 | 12 (5 / 6 / 1 / 0) | 229 | 34.8 s | 0 |
| D. `MARKET_EXPECT=closed` on a brand with no market rows | fail-closed | 107 | 44 | 0 | 63 (57 / 6 / 0 / 0) | 76 | 51.9 s | 0 |
| E. ADMIN only (no second admin, no tenant staff, no CORS origin) | missing inputs | 107 | 58 | 0 | 49 (43 / 6 / 0 / 0) | 134 | 65.0 s | 0 |
| F. `-strict` with a prerequisite SKIP (`-only health`) | | | | | | | | 3 |
| G. empty environment | | | | | | | | 2 (every missing input named, no stack trace) |
| H. `-prove-stale-stepup` (a second factor older than the critical window is refused) | full + second active tenant | 107 | 96 | 0 | 11 (4 / 6 / 1 / 0) | 232 | 320.3 s | 0 |

Run B is the reference result for the final build: 95 PASS, 0 FAIL, 12 SKIP. Counts for B/C/D/E/H are for the final
code; the one FAIL seen during development (a flaky synthetic IBAN, below) was fixed before them.

Bugs found in the suite while testing and fixed: scrubber rewrote check names and URL paths as tokens; payout instrument
reuse across runs (the platform refuses one destination shared by two players, now a checksum-valid unique GB IBAN per
run); **the synthetic IBAN's 14-digit account run was Luhn-valid in about 1 run in 10 and the platform then refuses it as a
possible card number (`pan_refused`)**, a flaky FAIL seen once in run H's first attempt, now prevented by construction
and pinned by `TestSyntheticIBANIsValidAndNeverLooksLikeACardNumber` (3000 run ids); wrong mock payments provider id
(`mock-payments`) and rail (`bank_transfer`, the only payout rail the mock adapter declares); hold arithmetic (a hold
moves the amount out of cash into `held_for_withdrawal`); alert list envelope key (`alerts`); a leftover engaged kill
switch from an aborted run broke the next run (now `setup.no_stale_containment`); a withdrawal flow without a
compliance reviewer FAILed instead of reporting a prerequisite SKIP; HTTP 429 now retried with a back-off.

The single `SKIP [platform-gap]` of B, C and H is `withdrawal.payout_completion` (SA-2). The `[prerequisite]` skips of B
and C are: `health.security_headers` (plain http locally), `platform.readiness_alerts_item` (no approved p1/p2 route
yet), `player.residence_declared` (parameter not given), `market.refusal_is_oracle_free` (needs an ACTIVE brand with no
rows; proven by run D instead), `auth.stepup_stale_refused` (not requested; run H proves it). The `[operator-evidence]`
skips are the six in README section 7. Run D's 57 prerequisite skips are the money flows that are unreachable by design
while the market is closed.

Not run: the suite against AWS; a real CloudFront `https` run (the header check is covered by a unit test against a TLS
stub only); a multi-replica run.

## 4. Findings (platform; none fixed here)

| Id | Finding | Impact on staging acceptance | Suggested owner / next step |
|---|---|---|---|
| SA-1 | **No supported way to create tenant staff in deployed staging.** `POST .../staff/invitations` delivers the token by the MOCK e-mail provider (in-process memory, unreadable over HTTP); `cmd/seed-admin` creates `platform_admin` only (`-issue-bootstrap` needs an existing active tenant staff row). Withdrawal four-eyes needs two tenant `finance` staff with distinct Persons; KYC approval needs `compliance`; capability and catalogue opt-in need `tenant_admin`; `platform_admin` is excluded from tenant-scoped routes by design. | Plan 6.6 flows (withdrawal four-eyes, KYC, capability configuration) cannot run in staging without an owner SQL procedure; the suite SKIPs them with the missing variable named | architect + security: a reviewed owner tool/procedure (for example `seed-admin` creating tenant staff with Person), or a test-support token route; needs a decision, not built |
| SA-2 | **A mock payout cannot be completed through the HTTP API.** `MockProvider.Withdraw` answers `pending` for every ordinary amount; only deposits have `simulate-callback`; the payout webhook needs a signature whose secret is in-process; staff `resolve`/sweeper just re-poll the mock (still pending). Only the sentinel amount 111 gives a deterministic (declined) outcome. | "withdrawal -> mock payout completes -> ledger balanced" is unprovable black-box; the hold stays in `held_for_withdrawal` and the request stays `submitted` | ledger-finance / architect: a test-support payout settlement route gated exactly like `simulate-callback`, or a sentinel success amount |
| SA-3 | No HTTP surface reports build/version, applied migration head or the `database_environment` anchor (launch-readiness shows only `APP_ENV`). | Reported as operator evidence (migrate task logs, startup log line) | optional: an admin status route; not required by the Blueprint |
| SA-4 | The structured log line `payments_kill_switch_engaged_alert` (and likely other staff-scoped lines) carries `tenant_id` **twice** (first the nil UUID from the request context, then the real value): duplicate JSON keys make Logs Insights extraction ambiguous. Observed in the local `api.log`. | Weakens the CloudWatch correlation by tenant | observability owner: use a distinct attribute name or drop the context attribute |
| SA-5 | `PUT /v1/admin/providers/{id}/capability` silently narrows the request to the adapter's declared manifest (a `sepa` payment method sent to the mock was dropped, response 200, stored `{card}`); the mock declares only `card` and `bank_transfer`. | Configuration trap: a payout then fails with "no payment provider available" instead of a 400 at configuration time | payments owner: reject values outside the manifest |
| SA-6 | Informational: with a deposit kill switch engaged, deposit initiation answers `201` with `status: declined` (not an error status); list envelopes differ (`items` vs `alerts` vs bare arrays). | none | contract hygiene |

## 5. CI guard and `go test ./...`

`go list ./...` does not list `deploy/aws/acceptance` (every non-test file and the harness tests are behind
`//go:build stagingacceptance`), so `go build ./...`, `go vet ./...` and `go test ./...` compile and run nothing from the
suite (`go test ./...` on the whole module was run on this tree: every package `ok`, no failure; an earlier attempt was
disturbed by another session cleaning the shared Go build cache and was repeated). One untagged guard test, `tag_guard_test.go` (external test package, no suite code), runs in plain
`go test ./...` and fails if any file in the directory loses the tag. The CI `Guard` steps (TEST_ADMIN URL, AWS SDK
confinement, memstore) do not match the directory; the integration-tag guard lists specific `testsupport` directories and
is unaffected. `gofmt -l` is clean; `go vet` clean plain and with `-tags stagingacceptance`; golangci-lint 2.9.0: 0 issues
plain and tagged.

## 6. Residuals: what the suite cannot prove

- That the AWS stack passes (not run). Anything about the database, IAM, RDS roles, backups, PITR, migrations, alarms
  (operator sections 3 to 9 of the plan).
- A mock payout completing (SA-2); a signed provider callback with a valid signature (secrets are in-process); the
  casino "rollback before the original" tombstone path (the play simulation only rolls back the caller's own round
  transactions).
- Real PSP, KYC, geolocation, e-mail, sportsbook or custody behaviour (all MOCK); legal or regulatory approval; alert
  delivery to a person or receiver; the non-allow-listed 403; metrics; log content in CloudWatch.
- Cross-tenant PLAYER reads between two active tenants unless the owner provisions a second active tenant with market
  rows (`SECOND_ACTIVE_TENANT`); without it the suite proves staff, login, ownership and token-authority isolation and
  the not-launched tenant's unreachability.
- Behaviour under load or concurrency beyond the six-way concurrent callback probe; multi-replica behaviour (re-run after
  `deploy.sh scale 2`).
- The market-evidence ordering once `market-evidence-kyc-residence` merges (README section 5).
- The staging stack's real configuration of `ALERT_*`, `METRICS_ADDR`, `mock_geolocation_country`, actor-proof key row and
  operating-market rows (owner actions A, B, C): the suite reports their absence, it does not create them.

## 7. Operator quick reference

1. Provision per plan 6.4/6.5 (first admin, owner actions A to C) and the tenant staff of SA-1.
2. Export the variables of README section 2; first run enrols the actors (keep the TOTP and recovery files).
3. `go run -tags stagingacceptance ./deploy/aws/acceptance`; keep `staging-acceptance-results.txt` as the evidence
   attachment for plan section 12 rows 6.3/6.6/9.x/10.x (it contains no secrets); do the README section 7 steps.
4. Re-run once with `MARKET_EXPECT=closed` against a brand without market rows if the fail-closed default is part of the
   acceptance; re-run after each evidence-configuration change.

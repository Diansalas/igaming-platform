# Final whole-platform production-readiness audit (provider-independent scope)

- Auditor: `security` specialist, independent, READ-ONLY on the audited tree.
- Audited: branch `claude/focused-wright-jw88w9`, HEAD `75c548c`, migration head `0170`.
- Date: 2026-10-17.
- Scope: provider-independent platform only. All providers are MOCK, AWS is not applied, and no brand is activated.
- This is a code- and design-level review. It is not a penetration test, a certification audit, a legal or regulatory approval, or a launch authorisation.

## Evidence I produced myself
- `go build ./...` and `go vet ./...` both clean. `gofmt -l` over cmd/internal/deploy found nothing. `go test ./...` (no build tags) passed: 75 packages `ok`, 0 FAIL.
- Hosted CI: GitHub Actions run #877 on `75c548c` completed `success`, with 27/27 check runs green (read-only API query). Runs #873-#876 on the four commits before it failed. `75c548c` is the fix for the 0101 guard test.
- **Live DB checks were SKIPPED.** The documented dev credentials (Makefile `DATABASE_URL`) got "password authentication failed". As instructed, I made no other attempt and changed no role. Because of this, the RLS/tenant checks below are static analysis of `migrations/*.up.sql`, plus the integration shards inside hosted run #877. I ran no integration test or drill locally.
- Secrets: I ran pattern scans over the tracked tree and over the added lines of the last 300 commits (`e5f29c2..75c548c`). No secret values were printed.

## Verdict: NOT FREEZE-READY (close)
At code level the platform meets the freeze bar for the MOCK scope. I found no CRITICAL or HIGH finding. The freeze finish line in `docs/plans/platform-freeze-work-list-2026-10-16.md` is still unmet on these items:
1. **B-1 Status docs contradict the code.** The finish line requires "runbooks and status docs match the code". The contradictions are listed in section 6.
2. **B-2 No backup/restore drill at the final migration head 0170.** The last recorded full drill is from F1: 138 PASS, at a head no later than 0166. Migrations 0167-0170 came after it.
3. **B-3 No named freeze commit has been recorded.** Hosted CI on `75c548c` is green but is not recorded in any status doc or registry row.
4. **B-4 (recommended before freeze, unregistered, MEDIUM) U-1.** `JWT_SIGNING_SECRET` placeholders published in this repository are not refused outside development (section 2).

The acceptance-suite SA-3 wiring is already registered for "at the freeze" (F5-F6 row). It is not counted again here.

## 1. Production safety of mocks: PASS
- `cmd/platform-api/main.go:92-104`: `mockProviderWiring` → `buildProviderBundle` → `refuseSyntheticInProduction` → `requireMarketEvidenceInProduction`. These run immediately after `config.Load()` and before the logger, tracing and `db.Connect`.
- `internal/providerkind/guard.go`: this is an **allow-list**. A component must implement `ProductionEligible` and must not be `Synthetic`. It keys on `cfg.GuardEnvironment()`, so an unset `APP_ENV` counts as production (`internal/config/config.go:1073`).
- `cmd/platform-api/registrations.go:399-465` registers every bundle component: payments, casino, sportsbook, statement sources, person resolver, document storage, malware scanner, email, payout verifier, geolocation locator, AML screening, every mock webhook/outbound resolver, secret backends and webhook schemes. The sportsbook, email, AML, statement-source and person-resolver mocks are always constructed. Production therefore cannot start until real adapters exist, which is the honest fail-closed state.
- `internal/providerkind/completeness_scan.go`: an AST scan that fails on any exported `Mock|Fake|Stub|InMemory` type without a `SyntheticComponent` marker. `cmd/platform-api/main_construction_ast_test.go` confines provider construction to `registrations.go`.
- Config refusals: `TEST_SUPPORT_ENDPOINTS_ENABLED` with production (`config.go:919`); `MOCK_GEOLOCATION_COUNTRY` with production (`config.go:928`); devfile secret backend outside explicit dev (`config.go:1111-1130`). Published MFA and fingerprint placeholders are refused outside explicit development (`config.go:597`, `staff_mfa.go:125`).
- Market evidence in production must be the bound KYC residence locator (`registrations.go:509-537`, `main.go:200-207`). Payment cursor sealing is required in production (`main.go:267`).
- Auxiliary binaries: `cmd/seed-market` and `cmd/seed-staging-world` refuse a production `APP_ENV` or anchor. `cmd/migrate down` refuses production or an unset `APP_ENV` (`cmd/migrate/main.go:124-138`).
- Checklist vs code: all 55 env-var names that appear as literals in `internal/config` are present in `docs/runbooks/production-configuration-checklist.md`. I saw no contradiction in the rows I checked.
- RISK LOW, already documented in ADR 0085 (around line 190) and not in the registry: the player simulation/test-support route flags (`main.go:518-535`) use `cfg.TestSupportRoutesEnabled()`, which reads an unset `APP_ENV` as development. This cannot be exploited today because the always-registered mocks make the synthetic guard refuse startup first. It will matter once the last mock is replaced. Fix: AND with `GuardEnvironment() != "production"`, as `wiring.go:96` already does.

## 2. Secrets: PASS with one MEDIUM finding
- Tracked tree: high-signal patterns (AWS `AKIA/ASIA`, PEM private keys, `ghp_`/`github_pat_`, Slack, Stripe live, Google API keys, JWTs) hit only deliberate test fixtures. These are `deploy/backup/backup_static_test.go:359-427`, which holds the scanner's own regexes and dummy canaries, and `deploy/aws/acceptance/suite_test.go:75`, a dummy JWT. No `.pem`/`.key`/`.tfstate`/`.env` file is tracked; only `*.example` files are.
- History (last 300 commits): the same patterns hit only `7e16ad0` (the fixture file above). Long password/secret literals hit only synthetic seed/acceptance fixtures (`cmd/seed-staging-world/main_test.go`, `deploy/aws/tests/fetch-seed-world-test.sh`, `deploy/aws/acceptance/checks_auth.go`).
- Dev-only DB passwords in `Makefile` and `docs/runbooks/README.md` are documented local placeholders. Staging pulls `JWT_SIGNING_SECRET` from Secrets Manager (`deploy/aws/modules/ecs/main.tf:164`).
- **U-1 (MEDIUM, unregistered).** `.env.example:13` publishes a `JWT_SIGNING_SECRET` of 40 characters or more. The CI workflows publish another (`.github/workflows/ci.yml:57,366,464`; `ci-selfhosted.yml:74`). `config.Load` checks only presence and length (`internal/config/config.go:950-954`). `publishedPlaceholderKeys` (`internal/config/staff_mfa.go:28`), documented as "every key value this repository publishes", does not include either JWT value. A production or staging deployment that copied `.env.example` would start, and anyone could then forge player access tokens (HS256) for any tenant. Staff routes are partly protected by the live-session check. The only control today is procedural: the checklist says never to use the `.env.example` value. Fix: add both JWT placeholders to the published-placeholder refusal outside explicit development, plus a unit test. Provider-independent and bounded, so A-class.

## 3. Tenant isolation: PASS (static; live DB not checked)
- A static parse of all 160 up-migrations found 158 surviving tables. Every table with `tenant_id` has ENABLE + FORCE RLS. Ten tables in 0141, 0142 and 0154 at first looked unprotected, but they are covered by `FOREACH ... EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY')` loops (`0141:234-236`, `0142:175-177`, `0154:138-140`). The new 0169 table `withdrawal_gate_decisions` is FORCE RLS and append-only (`0169:139,145`).
- Tables without `tenant_id` are platform/catalogue tables, and most have RLS. Three have no RLS: `actorproof_private.actor_proof_keys` (owner-only schema; RLS enabled with no policy according to the 0166 header), `database_environment` (anchor) and `payout_instrument_verification_max_age`. None is tenant-owned.
- Routes: every `r.PathValue("tenantID")` is followed by `canActOnTenant` (`internal/httpserver/admin_routes.go:27`), or by the shared `staff_read_common.go:61-71`, or by the idempotency wrapper (`staff_idempotency.go:111-112`). JSON `tenant_id` fields are response DTOs. Registration takes `brand_slug`, and the tenant is derived server-side (`auth_routes.go:97-137`).
- Freeze tests exist: `internal/httpserver/staff_route_golden_test.go` (223 routes with a golden step-up class) and `staff_route_static_test.go` (`EveryStaffRouteRegisteredThroughHelper`, `GoldenClassification`, `EveryMutatingRouteClassified`, `DBFree_ChainOnEveryRoute`, `NoRouteRegistrationOutsideHTTPServer`).
- Not verified: live `pg_class.relforcerowsecurity` and runtime-role grants at 0170, because the DB was unavailable. Hosted run #877 integration shards are the only live evidence.

## 4. Money: PASS
- No `float32`/`float64` in non-test Go code is used for money. The remaining uses are rate limiter, sampler, timing and config RPS (`internal/marketgate/sampler.go`, `internal/config/webhook_admission.go`). `internal/rg/limits_config.go:614` uses `UseNumber`. No `REAL`/`DOUBLE PRECISION`/`FLOAT` columns exist in migrations.
- Ledger append-only triggers: `ledger_transactions_immutable` and `_no_truncate` (`0021:82,86`); `ledger_entries_immutable` and `_no_truncate`, with a deferred `ledger_entries_balanced` constraint trigger (`0022:99,134,138`); `ledger_accounts_immutable_fields` (`0082:303`). No later migration drops them.
- The projection is written only by the `ledger_entries_project_balance` trigger (`0023:59`). Direct writes are fenced by `wallet_balance_projection_acting_fence` (CG031, `0113:1525-1537`). No Go code issues `UPDATE wallet_balance*`.
- Idempotency: `UNIQUE (tenant_id, idempotency_key)` plus a unique index on `(tenant_id, provider_id, provider_tx_id)` (`0021:42-46`).
- LOW, unregistered, crypto-deferred (U-5): `b2c/src/lib/money.ts:35` and `backoffice/src/lib/money.ts:22` format by float division. `backoffice/src/features/configuration/PaymentCapabilityForm.tsx:63-64` sends `Number(min_amount|max_amount)`. This is exact for fiat. For 18-exponent assets it is lossy above about 0.009 units. Fix it when crypto is scheduled.
- LOW, known named gap: `internal/bonus/types.go:38` uses a fixed `placeholderRoundingRuleID`. No `rounding_rules` table exists (registry line 419 assigned it to `0052`, which was never used for that purpose).

## 5. Staff/admin controls and the F1-F6 claims: PASS
- Step-up: all 223 staff routes carry a golden class. The mutating routes classed `none` are protective/tightening or reject/cancel actions, as ADR 0119 §8 rules. `POST /v1/admin/kyc/documents/{id}/review` is `none` per ADR 0119 line 659 (ORCH-DEFAULT, OQ-4). Recording a verified residence is `critical` (`kycResidenceReviewClass`). Four-eyes is enforced in the DB with signed actor proofs (K2/K3, T8 reverse rows, SR-7 alert routes, AML proposals/approvals).
- Audit: no single static test asserts "every mutating route writes audit". A heuristic call-graph scan of 142 mutating staff routes showed audit writes in the domain layer or DB functions for every route I sampled (rg, risk, sportsbook exposure, jurisdiction, assets, bonus suggestion review events). RISK LOW: audit coverage is enforced by convention and per-domain tests, not by a route-level gate.
- F1 / 0166 matches the code: owner-only `actorproof_private` schema with refusals APM01 and an ACL assertion (`0166:40-131`); `backup.sh:136-144,238,269-270` preflight, `--exclude-schema` and TOC assertion; runtime revokes in `deploy/init-app-role.sql:190-197` and `deploy/aws/sql/init-runtime-role.rds.sql:203-212`.
- F2/F3 (0167/0168) match the code: `reconciliation.VerifyPaymentCursorSealing` (`internal/reconciliation/payment_cursor_seal.go:146`) is wired at `main.go:267` with GuardEnvironment. The vocabulary `pay_payout_reversal_tombstone_but_paid` is in OpenAPI, the backoffice and the runbook.
- F4 / 0169 matches the code and the runbook `docs/runbooks/withdrawal-dispatch-gate.md`: WD052-WD055 in the up migration and WD095 in the down migration; alert kind `withdrawal.payout_gate_denied` (`internal/alerting/kind.go:175`); caller pin `internal/withdrawal/dispatchgate_callers_static_test.go` (commit `60753e3`).
- F5/F6 (0170, platform status) match the code: alert kind present (`internal/alerting/kind.go`, `internal/marketgate/open.go`); `GET /v1/admin/platform/status` (`internal/httpserver/platform_status_routes.go:38`); commit validation in `internal/buildinfo` and the Dockerfile (`deploy/docker/platform-api.Dockerfile:63-68`).

## 6. Docs truth: FAIL (blocking B-1)
These contradictions appear in the newest entries, and in the indexed runbook statements:
- `docs/active-stage.md:5` and `docs/progress.md:5` say "code head `0dc2680` (later commits are documentation only)". In fact the code changed in F1-F6 (migrations 0165-0170 and Go changes). HEAD is `75c548c`.
- Same lines say "migration head **0164**". It is **0170** (160 up files; 0143-0150, 0159 and 0161 are reserved, which matches `internal/db/migrate.go:111`).
- Same lines say "hosted CI on `0dc2680`: 27/27". The current evidence is run #877 on `75c548c`, 27/27.
- `active-stage.md:6` and `progress.md:6` say the drill was "118 PASS / 0 FAIL at head 164". F1 recorded 138 PASS later, and there is no drill at 0170.
- `active-stage.md:8` and `progress.md:8` say "Core platform development is STOPPED. Next authorised-only steps: the ONE named-commit AWS staging deployment". The registry row `OWNER-PRIORITY-CHANGE-AWS-LAST-2026-10-16` superseded this: F1-F6 were implemented, and AWS comes last against a frozen commit.
- `docs/HANDOVER.md:7` says "code head `0dc2680` ... migration head 0164 ... core platform development is stopped". Same contradictions as above.
- `docs/HANDOVER.md:9` says "What was built since 2026-10-15" without mentioning 0165 (T8), 0166 (F1), 0167/0168 (F2/F3), 0169 (F4) or 0170 (F5).
- `docs/HANDOVER.md:11` says "unit 68 packages ... drill 118/0 at head 164". My run shows 75 packages ok, and the drill head is stale.
- `docs/runbooks/README.md:46-49` says the `staff-mfa-s3-cutover.md` break-glass path applies "until the S6 MFA reset exists". S6 is implemented: `staff-lifecycle-and-mfa-recovery.md` is indexed at line 30.
- `docs/runbooks/README.md:50-52` says the self-hosted runner is for "CI-BILLING-1". Hosted CI is not billing-blocked (HANDOVER 2026-10-15 "CI state").
- `docs/runbooks/README.md:23` says the incident-response runbook has "no rota or recipient exists (ALERT-DELIVERY-1 open)". A9 added email and webhook channels with the owner as interim recipient (line 39-40 of the same file). The line needs qualifying: the channel exists, a rota does not.
- `docs/runbooks/README.md` does not index six runbooks: `aml-adjustment-approver-confidentiality.md`, `aml-officer-bootstrap.md`, `developer-handover.md`, `payout-instrument-keys.md`, `plat-rolesplit-staging-verification.md`, `production-startup-database-config.md`.
- `deploy/aws/acceptance/checks_auth.go:327,330` skip messages say "platform-api exposes no build/version endpoint" and "no HTTP surface reports the applied migration head". F6 added both. This is already registered (F5-F6 row: "wire to the new route at the freeze").
- Registry row `F4-WITHDRAWAL-GATE-BACKSTOP-MERGED-2026-10-17` lists SF4-2 as open, but commit `60753e3` implemented the caller pin. Row `F5-F6-...` says "Migrations 0167-0169 reserved in migrate.go", but no reservation remains. Both need a superseding append-only row.

TODO/FIXME/placeholders in non-test Go code that matter:
- `TODO(jurisdiction)` at `internal/payments/orchestrator.go:161,175`, `internal/payments/types.go:380`, `internal/casino/launch.go:185`, `internal/casino/orchestrator.go:1979`, `internal/risk/evaluator.go:98`: payment-method, launch and risk decisions are not filtered by a per-player jurisdiction. This is an accepted design gap tied to jurisdiction resolution (SEC-4I/DR-4I lineage) and to the market gate. It is not an A-class blocker for the MOCK freeze, but it must be closed before a second jurisdiction goes live.
- `internal/bonus/types.go:38` `placeholderRoundingRuleID`: see section 4.
- `internal/httpserver/rg_limits_routes.go:222`: a staff RG limit increase is refused as "step-up ... not implemented". This fails closed (ADR 0017 not implemented). Correct behaviour, not a defect.
- No `FIXME`/`XXX`/`HACK` exists in non-test Go code. The `authtest`, `testsupport` and `alertingtest` helper packages are not in the dependency graph of `cmd/platform-api`, `cmd/seed-admin` or `cmd/migrate`.

## 7. Open items vs registry; unregistered items
The registered open items I confirmed in code or docs (not repeated as new): SF4-1/R1, F4-R2-NEGBAL-SQL, R3, SF4-5, SRF2R-1/2/3, SRF2-6, SRME-4/5/6, SRME-1 maker-checker, SRF5-3, about 26 duplicate `tenant_id` log sites, acceptance SA-3 wiring, the T8 residuals, BRAND-DELETE-REHOME-1, PERSON-RELINK-PIN-1, NULL-ARM-WRITE-1, HD-TENANT-CLOSURE-PLAYER-FUNDS-1, ALERT-DELIVERY-1 (rota).

**Not registered:**
| Id | Severity | Class | Item |
|---|---|---|---|
| U-1 | MEDIUM | **A** (security config guard, provider-independent, bounded) | The published `JWT_SIGNING_SECRET` values (`.env.example`, CI workflows) are not refused outside explicit development; `publishedPlaceholderKeys` omits them (section 2). |
| U-2 | — (freeze finish-line) | **A** | Status docs (active-stage, HANDOVER, progress) and the runbooks index contradict the code (section 6). Needs a superseding registry row for SF4-2 closed and for the reservation text. |
| U-3 | — (freeze finish-line) | **A** | No backup/restore drill evidence at migration head 0170. |
| U-4 | LOW | B | Hosted CI run #877 on `75c548c` (27/27) is not recorded anywhere. Record it with the named freeze commit. |
| U-5 | LOW | F (crypto-deferred) | Frontend float handling of minor units (`b2c/src/lib/money.ts:35`, `backoffice/src/lib/money.ts:22`, `PaymentCapabilityForm.tsx:63-64`). |
| U-6 | LOW | B | Test-support route flags do not consult `GuardEnvironment()` (ADR 0085 notes it; no registry row). |
| U-7 | LOW | B | No route-level static gate asserts that every mutating staff route writes an audit record; coverage is by convention and domain tests. |

## Out of scope / not verified
Live DB state (RLS flags and grants at 0170), local integration suites, the backup drill, anything on AWS/RDS, real providers, frontend test runs, load, and legal, regulatory or licensing questions. Passing this review does not make the platform "secure" in an absolute sense. It records what was checked at `75c548c`.

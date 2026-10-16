# A6 record: ADR 0115 S4-min enforcement, WD-RG-1 withdrawal gate, minimal RG UI

Status: implementation record for completion audit item A6. Proposed registry text only: this file does not edit
the task registry, HANDOVER, progress, active-stage or the human-decision-register. Owner decisions of 2026-10-15
applied: player RG limits are MANDATORY for the first brand; the RG/risk gate is added to withdrawals; ALL
providers are MOCK. Software capability only: no regulatory approval or certification is claimed.

## 1. Proposed registry text

| Item | Label | Note |
|---|---|---|
| RG-ENFORCE-1 (deposit / loss / wager limit enforcement, same DB transaction as the posting) | `IMPLEMENTED` (software), DEFAULT OFF | Per tenant/brand switch `rg_enforcement_activations` (migration 0158). OQ-2 jurisdiction VALUES remain `BLOCKED` on compliance; with the switch ON and no floor in force every action fails closed `rg_config_missing`. |
| RG-ENFORCE-2 time-out enforcement | `IMPLEMENTED` | Not behind the switch (same as self-exclusion). Blocks launch, bet, deposit, bonus eligibility. Never blocks settlement, win, rollback, void. |
| RG-ENFORCE-3 session-duration limit, reality check | `NOT IMPLEMENTED` (PROVIDER DEPENDENT / F) | Enabled `session_duration` floor fails the readiness check `session_limit_not_enforced`; Person-scope chain aggregation fails closed. |
| WD-RG-1 withdrawal RG/risk gate | `IMPLEMENTED` (RG = real, risk = real engine over MOCK-fed rules) | Codes WD050 (RG) and WD051 (risk). Application gate only: there is NO database backstop (see residuals). |
| R-RG-CONFIG launch-readiness check | `IMPLEMENTED` | Readiness `SnapshotVersion` 2 (hash changes). |
| Player RG page (b2c) | `IMPLEMENTED` (UI), MOCK API in tests | `/account/responsible-gaming`. |
| PERSON-RELINK-PIN-1 | stays OPEN | No Go relink writer exists; migration 0158 adds a DB trigger that takes both Persons' L0.4 locks on any `player_accounts.person_id` change. |
| pending_intent_ttl / TTL sweep | `NOT IMPLEMENTED` | Rejected at the API; a stuck pending deposit intent keeps counting toward the deposit limit until it resolves. Owner acceptance needed. |
| Rollup for heavy players | `NOT IMPLEMENTED`, decision in section 5 | |
| `rg_deposit_limit_exceeded_post_facto` alert | `NOT IMPLEMENTED` | Delivery is ALERT-DELIVERY-1. |

## 2. What was built

- Migration 0158 (up and down): `rg_enforcement_activations` (append-only, FORCE RLS, SELECT and INSERT only for the runtime role,
  actor columns set by trigger from `rg_actor_session`, unique per `(tenant_id, brand_id)` NULLS NOT DISTINCT) and the
  `player_accounts_person_relink_lock` trigger. 0159 is unused and registered reserved, as are 153-157 (held by other work).
  The down migration refuses (RL099) once an activation exists: an activation is a recorded compliance decision.
- `rg.EvaluateAction` (isolation + scope guard, transaction-local lock/statement timeouts restored afterwards,
  eligibility first, then per kind: config, count_bonus refusal, mandatory, MIN across account and tenant_person scopes
  and the ceiling, measurement, refusal). Denial carries a stable code and the S3 figures (usage, limit, limit_source,
  window start, as-of, config version ids, audit inputs) into the audit row. Any error is `ErrEvaluationUnavailable`: never zero usage.
- `rg.EvaluateWithdrawalEligibility` (restrictions do not block; the one in force is reported).
- Wired: casino real launch and `postBet`; sportsbook placement; payments deposit T1 (`deposit_v2.go`) and T2 (`drive.go`);
  bonus eligibility through `EvaluateEligibility` (time-out now included). Not wired, by design: casino win, rollback,
  settlement, sportsbook settle/void/rollback, demo launch (eligibility only).
- Withdrawal gate `internal/withdrawalgate` (leaf contract) with `standard` implementation; `withdrawal.RequestParams.Gate`
  is REQUIRED (nil refuses). Runs after the tenant gate and before the payout-instrument gate (lock order, ADR 0082).
- Routes: `GET /v1/me/rg/options`, `GET|POST /v1/admin/rg/enforcement`; OpenAPI also gains the already-existing
  `/v1/me/rg/self-exclusion` and `/v1/me/rg/status` that were missing.
- B2C: `b2c/src/api/rg.ts`, `features/rg/*`, route `/account/responsible-gaming`, link from the account page, vitest tests and the
  OpenAPI contract test `rg.contract.test.ts`.

## 3. Decisions and interpretations (each reversible, each needs owner or compliance confirmation)

1. Withdrawals during a time-out or self-exclusion are ALLOWED (ADR 0115, WD-RG-1 default). The restriction in force is
   recorded in the `withdrawal.requested` audit as `rg_restriction_in_force`. Reversing this is a one-line change in the standard gate.
2. A numeric ceiling is a hard cap on every period the kind allows, whether or not the player set a value. An attested-none ceiling bounds nothing.
   A mandatory kind with a numeric ceiling does not demand a player value (the ceiling bounds it).
3. Removing a mandatory limit is an increase (cooling-off applies); once realized the kind is mandatory-without-value again and refuses.
4. Withdrawal gate: risk outcomes DENY and REVIEW both refuse (REVIEW has no queue on the withdrawal path yet). The account must be `active` and the wallet not frozen.
5. A rejected withdrawal gate returns 403 `forbidden` with a generic message; the reason code is only in the audit row.
6. A casino path with `count_bonus_funded=true` fails closed (S3 condition (a)).
7. The enforcement switch is per brand OR tenant-wide (a tenant-wide row covers all brands). Activation is staff-initiated, audited and cannot be reverted by the application.

## 4. ADR 0115 S3 hard conditions: disposition

| Condition | Disposition |
|---|---|
| (a) count_bonus_funded on casino | Satisfied: refused up front, fail closed. Test `CountBonusFundedOnCasino_FailsClosed`. |
| (b) heavy-player measurement, statement_timeout, rollup decision | statement_timeout satisfied (default 4 s, fail closed, tested at 1 ms and 5 ms). Measurement and rollup decision: section 5. |
| (c) session aggregation at tenant_person | Satisfied: fail closed in every action; also a readiness failure. Test `SessionAggregationAtPersonScope_FailsClosed`. |
| (d) relink lock | Satisfied by DB trigger (both Persons, lower hash first) plus a post-lock re-read of the Person in `evaluateEligibility`. Tests `RelinkTakesBothPersonLocks_...`. PERSON-RELINK-PIN-1 itself stays open. |
| (e) denial audit figures | Satisfied: `Decision.AuditMetadata`; asserted on casino, sportsbook, deposit and withdrawal-adjacent paths. |
| (f) pending_intent_ttl not presented as effective | Satisfied: OpenAPI and UI do not offer it; documented as ineffective. |
| (g) e2e extended to enforcement call sites | Satisfied: casino, sportsbook, payments e2e files named `rg_enforcement_s4_integration_test.go`. |
| (h) reader guards | Satisfied: `usageGuard` + `session != tenantID` check, tests for REPEATABLE READ, SERIALIZABLE, player-scoped session. |
| (i) deposit re-measure | Satisfied for the request path: see evidence test `TestRGEnforceS4_Evidence_*` (log lines). Plan re-measure with deposit rows is in the S3 evidence file; unchanged query. |
| Ceiling clamp (S2 hard condition) | Satisfied at read time; test `Ceiling_IsAHardCap_AndAClampOnAConfirm`. |
| Legacy-scope MIN | Satisfied: account and tenant_person values are both considered and the minimum binds; test `ScopeMIN_AccountAndTenantPerson`. |

## 5. Heavy-player evidence and rollup decision

Test: `TestRGEnforceS4/HeavyPlayerMeasurement` (`internal/rg/limits_s4_heavy_integration_test.go`, gated by `RG_S4_HEAVY=1`; a skip is not
evidence). One casino player, 150,000 bet transactions + 150,000 win transactions spread over 29 days (300,000 `ledger_entries` rows in the
30-day window), real triggers, `ANALYZE` run, windows rolling_24h + rolling_30d + calendar_month (wager) and rolling_30d (loss), account scope,
measured through `EvaluateAction(casino_wager)` with a 60 s statement timeout so the figure is the cost of the scan and not a timeout artefact.
Seven runs on a shared dev box at load average 20-26 (other agents running): 3.76 s, 4.08 s, 3.91 s, 3.46 s, 3.34 s, 3.43 s, 3.34 s;
p50 3.46 s, max 4.08 s (about 11.5 microseconds per row, consistent with the S3 estimate of 13). A first run with the production timeout failed
CLOSED on its third evaluation at 4.04 s (`57014`, surfaced as `ErrEvaluationUnavailable`), which is the designed behaviour. A 5 ms timeout also fails closed.

Finding: at 300,000 rows the default 4 s `DefaultActionStatementTimeout` is AT the edge: such a player is refused (fail closed) on a fraction of
bets. The S3 hard condition (b) is therefore satisfied for the statement timeout (present, fail closed, tested) and for the measurement
(done), but NOT satisfied as "the 30-day and calendar-month windows are safe for every player".

Decision proposed (needs owner or architect confirmation): (1) do NOT raise the timeout: the evaluation runs under the L0.4 Person lock, so
the timeout is also the longest a single bet of that player can hold up that player's other bets and the lock timeout of the same Person; (2) do NOT
build the rollup in A6 (new projection, reconciliation job, drift handling: its own ledger-finance-owned slice); (3) record the limit: a
player with more than roughly 250,000 ledger rows in 30 days is refused when a 30-day or calendar-month wager or loss window is configured;
(4) before launch EITHER configure the first brand with rolling_24h / rolling_7d windows only for wager and loss, OR schedule the
rollup slice (ADR 0115 section 1.4). That 250,000 figure is rows (bets + wins), roughly 4,000 bets per day sustained for 30 days: far
beyond a recreational player, but a bot or a bonus-abuse pattern reaches it, and those players are the ones refused. `NOT IMPLEMENTED`: rollup.
Statement of risk: availability of play for very high volume players, never an unsafe allow.


## 6. Mutation pass

Driver `docs/plans/a6-mutation-driver.py`, results `docs/plans/a6-mutation-results.tsv`. 30 hand-written single-line mutants of the enforcement
logic and its call sites, each run against the named integration suites (rg `TestRGEnforceS4`, casino/sportsbook/payments `TestRGEnforceS4_*`,
withdrawal `TestWDRG1*`). 28 KILLED, 2 SURVIVED (justified below), 0 build errors in the final run. Two mutants first needed a
test: `min_becomes_max` SURVIVED the first pass (every scenario put the ceiling and the player value on different scope keys, so the MIN between
the two sources was never exercised) and `wd_risk_review_allowed` SURVIVED (the REVIEW outcome still refused, through the unavailable path with a 503). Tests
added: `Ceiling_SameScopeKey_MinOfPlayerValueAndCeiling` and `TestWDRG1_Risk_ReviewOutcomeRefuses_WithTheRiskGateCode`; both mutants are now killed. An
earlier pass was discarded because the shared Postgres cluster crashed mid-run on a full disk (not caused by this work); the complete pass was re-run
from the start. `timeouts_not_applied` makes the lock-timeout test hang, so it is KILLED by the 5 minute test timeout.

Survivors, justified as equivalent:
- `usage_guard_removed` (the explicit `usageGuard` call at the top of `EvaluateAction`): the same guard runs inside `ReadUsageConfig`,
  `MeasureStakeUsage`, `MeasureDepositUsage` and the session reader on every path that reads usage, and `activationOn` already refuses a session whose
  tenant differs. Removing the early call changes only WHEN the refusal happens, not whether. Kept as defence in depth.
- `timeout_end_inclusive` (`ends_at > clock_timestamp()` to `>=`): the two differ only when `clock_timestamp()` equals `ends_at` to the microsecond. No test can
  pin that instant without controlling the database clock; the boundary test proves "before ends_at blocks, after ends_at allows" with the database clock.


## 7. Assertions changed (none weakened)

- `internal/rg/limits_s1_static_test.go`: the expected set of tables granted by `deploy/init-app-role.sql` gained `rg_enforcement_activations` (SELECT, INSERT).
- `internal/httpserver/staff_route_static_test.go`: route-count pin 204/125 to 206/126 (two new staff routes).
- `internal/httpserver/staff_route_golden_test.go`: two new classification entries.
- `internal/launchgov/closure_withdrawal_race_integration_test.go`: fixture now activates the player account (WD-RG-1 requires an active account). Assertions unchanged.
- Sixteen existing withdrawal test/handler call sites now pass `Gate: standard.New()`; `wallet_adversarial_test.go` uses a local allow-all gate. No assertion changed.
- OpenAPI prose: the time-out description no longer says that nothing blocks; the S2 comment says limits are enforced when the switch is on.
- Readiness snapshot version 1 to 2 (an added check changes the hash).

## 7b. Merge with origin/claude/focused-wright-jw88w9 (tip ce37d15) and verification

Merged into a6-rg-enforcement. Conflict resolutions: readiness check sets and labels keep both (A7 R-AML-*, A8 R-MARKET-CONFIG, A6 R-RG-CONFIG); `SnapshotVersion` is now 4 (the A7/A8 test `readiness_aml_test.go` pinned 3 and was updated to 4: a CHANGED ASSERTION); route pins 217/134 and step-up counts none-mutating 43, read 83 (+2 A6 routes on top of upstream 215/133, 42/82); test helper `wantCheck` in the A6 readiness test renamed `wantRGCheck` (name clash). A8 market guards required `MarketGuard`/`WithUngatedMarketOpenForTests` in the A6 casino, sportsbook and payments tests (fixture change). Migration 0158 follows upstream 0157; the reserved-gap entries I had added for 153-157 were removed again.

Commands (private scratch DB, `-p 1`): integration `-tags integration` PASS for rg, casino, sportsbook, bonus, withdrawal, risk, launchgov, tenant, kyc, db, wallet, reconciliation, identity, withdrawalgate, httpserver subset (Withdraw|Deposit|RG|Golden|OpenAPI|Staff|Route|HSEC|KYC|Submit|Stage3C|Financial|Launch|StepUp); `-race` PASS for rg (RGEnforceS4|RGLimits|Eligib), casino (RGEnforceS4), withdrawal (whole). payments: the full suite did NOT complete in this environment (two attempts hit the 40 minute and 90 minute limits inside the slow K3 scratch-database suites, no failure seen); a 436-test subset (`RGEnforce|Deposit|HSEC|B13B|Payout|R31|K3_World|K3_[A-W]|Resolution|Webhook|Kill`, K3_X skipped) PASSED, so payments is `PARTIALLY VERIFIED`. `go test ./...` PASS; `go vet` plain and `-tags integration` PASS; gofmt clean; golangci-lint 2.9.0 0 issues; b2c `npm run build` and `npm test` (127 tests) PASS; backoffice `tsc --noEmit` clean. The mutation pass and the heavy run were executed before the merge on the same enforcement files (unchanged by the merge).

`TestResolutionIsolation_FinancialDuringOutage` (INV-POOL, 400 ms bound on the oldest pooled transaction) failed twice and passed twice on a machine at load average 20+, and passed in the post-merge subset run; it measures wall-clock transaction age and is load-sensitive. Not proven independent of the A6 change on an idle box: ledger-finance may re-run it alone.

## 8. Residuals and merge friction

1. Session-duration and reality-check enforcement not built (PROVIDER DEPENDENT / F).
2. `pending_intent_ttl` has no effect; a stuck pending intent counts toward the deposit limit. Owner acceptance needed.
3. No rollup; see section 5.
4. Payout dispatch T1p does not re-run the withdrawal gate and WD050/WD051 have no database backstop: a code path that bypasses `RequestWithdrawal` is not gated.
5. Late-success deposit credit after a limit decline: the `rg_deposit_limit_exceeded_post_facto` alert is not built.
6. PERSON-RELINK-PIN-1 open: no Go relink writer exists.
7. Staff changes to a player's limits are not notified to the player.
8. Other agents' withdrawal tests that call `RequestWithdrawal` without `Gate` now fail closed. Pass `standard.New()` or a test gate.
9. Shared-line merge conflicts likely: `reservedUnusedVersions` in `internal/db/migrate.go`, the staff-route pin, `deploy/init-app-role.sql`.
10. A risk rule scoped by payment method fails closed on withdrawals (no method at request time).
11. The launch-readiness hash changes with every brand (SnapshotVersion 2); stored acknowledgements must be redone.
12. Enforcement default OFF: until staff activate it per brand, limits remain recorded-only. The readiness check R-RG-CONFIG blocks activation of a brand while it is off.

## 9. Review probe lists

Security: (1) `usageGuard` and the `session != tenantID` check: can a platform-acting or player-scoped session reach `EvaluateAction`? (2) the 403 generic body on gate denial (no reason leakage) and the 503 on unavailable; (3) `/v1/admin/rg/enforcement` RBAC and step-up classification (`stepUpNone`: is that right for a switch that can only turn enforcement ON?); (4) RLS and grants of `rg_enforcement_activations`; (5) the relink trigger as `SECURITY DEFINER` or not, search_path pin, and its lock order against `rg.lockPerson`; (6) tenant isolation on the withdrawal gate input (foreign account refused); (7) no session or game token reaches a provider on the launch path (unchanged, test retained); (8) the b2c page never stores or logs limit values outside component state.

Ledger-finance: (1) the L0.4 lock is taken before any ledger lock on casino bet, sportsbook placement, deposit T1/T2 and withdrawal request, and the usage reads run after it under READ COMMITTED; (2) the deposit candidate intent counted exactly once (ExcludeIntentID) and the T2 double-count conservatism; (3) loss usage netting (wins offset losses; `loss_may_go_negative`); (4) boundary semantics `usage + amount > limit` refuses, equality allowed; (5) rollback, win and settlement never evaluated; (6) timeouts are transaction-local and restored so the posting keeps its own; (7) withdrawal gate placement before the instrument FOR SHARE lock and the hold; replay of an idempotent request is not re-evaluated; (8) the held-funds interaction: a withdrawal request is not a stake and is not counted in loss/wager usage; (9) the heavy-player cost under the L0.4 hold time.

## 10. Security review (security specialist, independent, 2026-10-16, branch tip 15a30c8)

Scope: the A6 diff against origin/claude/focused-wright-jw88w9 (enforcement, activation switch and route, relink trigger, withdrawal
gate WD050/WD051, `GET /v1/me/rg/options`, the b2c RG page). Method: code review, plus adversarial SQL probes as `igaming_runtime` and the
A6 integration suites, run on a private scratch database (`sec_a6_review`, sanctioned harness, synthetic data only). Not in scope: penetration
testing, the ledger-finance probe list (section 9), the payments full suite, the heavy-player run, and code outside the diff except where a
diff path calls it. Passing this review does not make the subsystem "secure". It means no further finding was found within this scope.

**Verdict: NOT APPROVED. One blocking finding (SR6-1), and the fix is small.** All other probe items pass. SR6-2 to SR6-5 are Low and do not block.

### 10.1 What was verified

| Probe | Result | Evidence |
|---|---|---|
| Time-out / self-exclusion predicate | PASS | One query in `evaluateEligibility` (self-exclusion ranked first), database clock, exclusive `ends_at`. It reaches casino real and demo launch, `postBet`, sportsbook `PlaceBet`, deposit T1 and T2 (all through `EvaluateAction`, which falls through to `EvaluateEligibility` when the switch is off) and bonus eligibility (`bonus/eligibility.go`, direct `EvaluateEligibility`). A grep of every non-test call site shows that postWin, postRollback, sportsbook settle/void/rollback and payout never call it. Withdrawals use `EvaluateWithdrawalEligibility` (restriction reported, not blocking: an owner decision). |
| Fail-closed | PASS | `EvaluateAction` wraps every error after validation in `ErrEvaluationUnavailable`. The switch read refuses a session for another tenant (`ErrUsageScope`) instead of answering "off". All five call sites return the error, so the transaction aborts. Withdrawal: a nil gate, a gate error or an unknown gate code or risk outcome gives `ErrGateUnavailable` and a 503. Tests: `RepeatableRead_IsRefused`, `PlayerScopedSession_IsRefused`, `StatementTimeout_FailsClosed_NeverZeroUsage`, `LockTimeout_FailsClosed`, `TestWDRG1_NoGate_And_FailingGate_FailClosed_NothingWritten`, `..._GateCannotDecide_Is503`. |
| 0158 activation table: RLS, grants, append-only | PASS | Catalogue: RLS on, FORCE RLS on, runtime grants are exactly SELECT and INSERT, and every function is non-SECURITY DEFINER with a pinned search_path. The runtime-role probes were all refused: a player-scoped insert, a platform insert, an acting-session insert, a tenant-B staff member claiming tenant A, a tenant-A staff member inserting for tenant B (RL001), a tenant-B brand (FK), a suspended staff member (RL001), UPDATE, DELETE, TRUNCATE, `ON CONFLICT DO UPDATE` (permission denied), `ALTER TABLE ... DISABLE TRIGGER` (must be owner), and a duplicate scope (23505). Caller-supplied id, actor and `created_at` are overwritten by the trigger. Reads: tenant B sees 0 rows, a session with no tenant sees 0, a platform session sees 0, tenant A sees its row. |
| The runtime role cannot turn enforcement off | PASS | No UPDATE or DELETE grant or policy, the immutability trigger, and no TRUNCATE grant. The down migration refuses with RL099 while a row exists. There is no Go or SQL disable path. |
| `/v1/admin/rg/enforcement` RBAC | PASS | GET needs `rg_config:read`. POST needs `rg_config:write` (compliance only), `RequireTenantScope`, and the tenant and actor from the token (an unknown `tenant_id` body field gives 400). Tested: support 403, tenant_admin 403, platform 403, player 403, no token 401, another tenant's brand 400, and tenant B does not inherit. Success and denial are both audited. |
| `/v1/admin/rg/enforcement` stepUpNone | **FAIL, SR6-1** | See below. |
| Relink trigger | PASS | Not SECURITY DEFINER. `search_path = pg_catalog, public, pg_temp`, and every call is schema-qualified. It uses the same key as `rg.lockPerson` (`hashtext('player_restrictions'), hashtext(person_id::text)`). It takes the lower hash first, so two relinks cannot invert the order. A relink takes the tuple lock and then both person locks. An evaluation takes the person lock, then re-reads the Person and follows a move. No Go code takes a row lock on `player_accounts`. `person_id` is in no unique index, so a relink takes FOR NO KEY UPDATE, which does not conflict with the FK KEY SHARE taken by ledger and withdrawal inserts, so there is no ABBA cycle. Tests `RelinkWaitsForAnInFlightEvaluation`, `EvaluationFollowsAMoveCommittedBeforeItsLock`. |
| WD050/WD051 | PASS (see SR6-2, SR6-3, SR6-4) | The 403 body is one generic sentence for both gates. Code and reason stay in the audit row and the server log. The gate returns a 503 when it cannot decide. A nil gate refuses. The denial audit holds gate_code, reason_code, brand_id, wallet_id, asset_code, amount and risk_outcome, with no PII (no email, name, instrument detail or fingerprint). `rg_restriction_in_force` on the success audit holds only the restriction type. |
| Platform or player-scoped session reaching `EvaluateAction` across tenants | PASS | Platform session: `app.tenant_id` is empty, so `ErrUsageScope` gives unavailable. Acting or other-tenant session: the tenant mismatch is refused. A player-scoped session of the same tenant: with the switch on, `usageGuard` refuses (`app.player_account_id` set). With the switch off, it runs eligibility only, under RLS, so another player's account is not found and the call errors. Another tenant's account id under the right tenant session: RLS hides it, so the call errors and fails closed. |
| Usage reads, tenant isolation | PASS | `accts` CTE, `ledger_entries`, `ledger_transactions`, `ledger_accounts` and `deposit_intents` are all filtered by `tenant_id = $1`, the joins match on tenant, RLS applies, and `usageGuard` runs on each reader. |
| b2c RG page | PASS (see SR6-5) | Form values are held only in `useState`. Server responses are in the in-memory react-query cache. No localStorage, sessionStorage, cookie, console, beacon or analytics call in `features/rg`, `api/rg.ts` or `lib/money.ts`. The contract test pins every `apiFetch` path and method of `rg.ts` to the OpenAPI spec and includes a self-check of the extractor. Limitation: it checks paths and methods only, not request or response shapes (the Go-side OpenAPI DTO tests cover the server half). |
| Test run (this review) | PASS | Run with `-tags integration -count=1 -p 1` on the private DB: rg, withdrawal, httpserver (RG enforcement, WDRG1, OpenAPI RG, staff route/golden), casino, sportsbook, payments with the A6 test filters. 69 PASS, 1 SKIP (the heavy run, gated by `RG_S4_HEAVY`). |

### 10.2 Findings

**SR6-1 (Medium, BLOCKS completion of the staff route; fix before the item is marked IMPLEMENTED): `POST /v1/admin/rg/enforcement` is classified `stepUpNone`.**
The "protective" label of ADR 0119 rule 8.1 does not fit this write. With the switch on and no floor in force, every deposit, real launch and wager of the
brand, or of the whole tenant when `brand_id` is omitted, fails closed with `rg_config_missing` (section 1). The write cannot be undone: no software
path reverses it, and the 0158 down migration refuses (RL099). Recovery needs a hand-written owner migration.
Failure scenario: an attacker with a stolen aal2 compliance session (token theft, an unlocked workstation) sends one POST with no `brand_id`. This
irreversibly halts all deposits and gameplay of the tenant until engineering ships a migration. The handler has no precondition that a floor is configured.
ADR 0119 already treats protective writes that are outage vectors as `critical`: capability-grant revoke, provider-credential revoke, staff
suspension. This case is stronger, because it is also irreversible. Required: classify the POST `critical`. That means the route
registration, `staffRouteGolden`, and the step-up count pins in `staff_route_static_test.go`. GET stays `none`. Recommended, as a product or compliance decision and not
blocking: refuse activation unless R-RG-CONFIG passes for the scope, so an honest mistake cannot halt a brand either.

**SR6-2 (Low): the gate's tenant-isolation tests do not prove what they say.** `TestWDRG1_TenantIsolation_ForeignAccountIsRefused` asserts only
`err != nil`. The foreign account is refused by the earlier tenant/status gate before the WD-RG-1 gate runs, so the test does not exercise the gate's
own isolation. The HTTP suspended-account case also accepts 401. Add an assertion on the error class, and add a direct test of
`standard.Gate.Evaluate` under a tenant-B session with tenant-A ids. It must return an error and never `Allowed`.

**SR6-3 (Low): the WD-RG-1 denial audit records `ActorSystem` with no actor id, IP, user agent or request id.** The request is player-initiated, and the
success audit uses `ActorPlayer`. CLAUDE.md requires an actor and an IP on audit records. Record `ActorPlayer` with the account id and the request meta.

**SR6-4 (Low): the handler checks `gateDenied` before `err`.** If the commit of the denial-audit transaction fails, the player still gets a 403 while the
denial audit is lost. Nothing else is written, so this is not unsafe. Check the WithTenant error first and answer 503.

**SR6-5 (Low, pre-existing pattern that A6 extends): b2c `logout` does not clear the react-query cache.** RG restrictions (self-exclusion status) and
limits stay in memory for the next sign-in in the same tab until they refetch. Call `queryClient.clear()` on logout. This applies to every player-data query, not only RG.

**Informational and residual (no action in A6, flagged for launch):**
- I-1: the database checks that `app.principal_id` is an active staff member of the tenant. It cannot prove that the request came from that staff member, so any code running as the runtime role can activate the switch. This is the accepted trust model of 0135.
- I-2 (launch-relevant): WD050/WD051 have no database backstop, and payout dispatch does not re-run the gate (residual 4 in section 8). A code path that bypasses `RequestWithdrawal` is ungated. This must stay on the launch risk list.
- I-3: by probing amounts, a player can locate a risk-rule threshold (403 against 201). This is inherent to any amount limit and is not reduced by a generic body.
- I-4: `ActivateEnforcement`'s idempotent 23505 branch is race-safe (savepoint, then read the stored row). A repeat writes no second audit row. Tested.

### 10.3 Tests the specialists' QA coverage must keep

- Another tenant's token or session against `/v1/admin/rg/enforcement`, `/v1/me/rg/options` and `/v1/me/withdrawals` returns 403 or 404 and never data or an allow (present). Add the direct gate case from SR6-2.
- A platform session, an acting session and a player-scoped session reaching `EvaluateAction` or `EnforcementEnabled` give an error and never "off" or "allowed" (the player-scoped case is present. Add platform and acting.).
- The runtime role cannot UPDATE, DELETE or TRUNCATE `rg_enforcement_activations`, and cannot insert as a platform, player, acting or suspended principal, or for another tenant (the catalogue pins are present. The behavioural probes above were run manually: worth a test).
- After SR6-1: the POST without a fresh step-up returns the step-up-required answer.

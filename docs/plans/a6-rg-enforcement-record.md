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

## 11. Ledger-finance review (ledger-finance specialist, independent, 2026-10-16, code at 15a30c8)

Scope: the ledger-finance probe list of section 9, plus the heavy-player decision (section 5), the pending-intent TTL residual and a re-run of
`TestResolutionIsolation_FinancialDuringOutage`. The code reviewed is the code at 15a30c8. Commit 4a300d8 (section 10) changed only this file.
Method: code reading of every call site and of the usage SQL, the A6 integration suites, one extra mutant, and an independent heavy-player
measurement. Everything ran on a private scratch database (`lf_a6_review`, sanctioned harness, synthetic data only, `-p 1`).

**Verdict: NOT APPROVED as-is. One blocking finding (LF-A6-1), which needs a test only and no code change.** Every other probe passes. LF-A6-2 and
LF-A6-4 are conditions on ACTIVATING the switch for a brand. They do not block the merge.

### 11.1 What was verified

| Probe | Result | Evidence |
|---|---|---|
| L0.4 before any ledger lock: casino bet | PASS | `postBet`: L0.1 delivery, tenant/brand status gates (SHARE), capability and game reads (no lock), L0.2 `AdvisoryLockPlayerBonusScope`, then `EvaluateAction` (L0.4), then risk, KYC, the projection locks and `ledger.Post`. The call replaces the earlier `EvaluateEligibility` in the same place, so the ADR 0082 order is unchanged. The bet is cash-only (`Dr player_cash / Cr house_gaming`), so the candidate `event.Amount` is exactly the leg that usage measures. |
| L0.4 before any ledger lock: sportsbook placement | PASS | Idempotency short-circuit (read), tenant/brand gates, catalogue and jurisdiction reads (no lock), then `EvaluateAction` (L0.4), then risk, KYC, L0.6 exposure, the projection locks and `ledger.Post`. |
| L0.4 before any ledger lock: deposit T1 / T2 | PASS | T1: market gate, replay lookup, tenant gate (SHARE), intent INSERT (a row no other transaction can see, so no wait edge), then `EvaluateAction`, then KYC and the attempt insert. There is no ledger lock in T1. T2: `EvaluateAction` is the first statement, before the `deposit_intents FOR UPDATE` parent lock (RV-0095 N1). |
| L0.4 before any ledger lock: withdrawal request | PASS | Replay lookup (read), `RequireActiveForPaymentInitiation`, then the WD-RG-1 gate (L0.4 in `EvaluateWithdrawalEligibility`, then risk), then the payout-instrument FOR SHARE lock, KYC, the insert and the hold projections. |
| Usage reads after the lock, READ COMMITTED | PASS | `usageGuard` (isolation and scope) runs before the lock. Every usage statement runs after `lockPerson` and is a new statement, so its snapshot is newer than the lock. `TestRGEnforceS4/RepeatableRead_IsRefused`. The concurrency tests (stakes, two brands of one Person, deposits) post exactly the allowed number. |
| Candidate intent counted once | PASS | `inflight` excludes `di.id = $9` and `Amount` is added once. T1 inserts the candidate before the evaluation, so the exclusion is load-bearing. Mutant `exclude_candidate_intent_removed` is killed. `DepositLimit_UsageWindowsAndFigures` asserts 600 + 300 (candidate) == 900 allowed, 401 refused. |
| T2 double count is conservative only | PASS (LF-A6-6) | Only possible when the candidate already has a ledger credit. The in-flight side cannot double-count it (`ledger_transaction_id IS NULL`, one statement, one snapshot). |
| Loss netting of wins | PASS | `sg` is debit-positive over `player_cash` for the closed loss set, so a win credit reduces loss. `TestRGEnforceS4_Casino_LossLimitIsNetOfWins` (real postings) and `WagerAndLossLimits` (stake 300, win 250, loss 50). |
| `loss_may_go_negative` | **GAP, LF-A6-1** | The plumbing is correct (`req.LossMayGoNegative = deref(cfg.LossMayGoNegative)`, a nil compose gives false, `floorLoss`). No S4 test exercises the `false` path. |
| Boundary `usage + amount > limit` refuses, equality allowed | PASS | `checkDeposit` and `checkStake` use `Cmp(...) > 0`. Pinned at equality and at +1 for deposit (600+400 / 401), wager (300+200 / 201), loss (50+50 / 51), the ceiling (400 / 401) and mandatory-with-ceiling (700 / 701). Mutants `deposit_boundary_ge` and `stake_boundary_ge` are killed. |
| Win, rollback, settlement, void never evaluated | PASS | The only non-test callers of `EvaluateAction` are casino launch (real), `postBet`, `PlaceBet`, deposit T1 and T2. Tests `..._NeverBlocksSettlement`, `..._NeverBlocksSettlementOrVoid` and the time-out-not-settlement test post settlements while a limit or time-out refuses stakes. |
| Transaction-local timeouts restored | PASS | `set_config(..., true)` with the values read before the change. Restore runs on the allow and deny returns. On an error the caller aborts (all five call sites return the error, also confirmed in section 10), so the timeouts cannot reach a posting. `TimeoutsAreRestored`, and mutants `timeouts_not_restored` and `timeouts_not_applied` are killed. |
| Withdrawal gate placement and replay | PASS | Before the instrument FOR SHARE lock and the hold (above). A replay returns before the gate (`TestWDRG1_Replay_IsNotReEvaluated`). A denial commits only the audit, and the key is not consumed. |
| A withdrawal is not a stake | PASS by code (LF-A6-7) | The `withdrawal_*` transaction types are in neither `RGLossTransactionTypes` nor `RGStakeTransactionTypes`. The deposit read counts only `deposit` credits, so a released hold neither consumes nor restores deposit capacity. No test pins it. |
| No new ledger write path, no money change | PASS | No change under `internal/ledger` or `internal/wallet` (except one test), and migration 0158 touches no ledger table. Amounts stay `int64` minor units, limits and usage are `big.Int` or decimal strings, and the RG UI uses string arithmetic (`majorToMinorString`, `minorGreater`). The only `float64` in the Go diff is JSON decoding in tests. The only direct ledger INSERTs are the heavy-test seed (test-only, real triggers). |
| A6 suites (this review) | PASS | rg, casino, sportsbook, payments, withdrawal, launchgov, httpserver filtered to `RGEnforceS4|WDRG1|RGLimits|Eligib|Migration0158|RGConfig|WithdrawalRGGate|LockOrder`: all `ok`. |
| `TestResolutionIsolation_FinancialDuringOutage` re-run alone | PASS 5 of 5 | Idle box (load average 0.2 to 1.0). The oldest pooled transaction was 43.8, 40.4, 37.3, 43.2 and 42.8 ms against the 400 ms bound. With the switch off, A6 adds only the activation read to this path. The earlier failures (load average above 20) are a load artefact and not an A6 regression. |

### 11.2 Findings

**LF-A6-1 (Medium, BLOCKING; a test only): the `loss_may_go_negative = false` path is not tested through enforcement, and the mutant in the unsafe
direction survives.** `rgfix.PermissiveFloor` sets `loss_may_go_negative: true` for loss, and `EnforcedFloor` inherits it, so every S4 test runs with net
winnings giving headroom. I applied the mutant `req.LossMayGoNegative = true` (`internal/rg/enforcement.go`, `checkStake`) and ran
`TestRGEnforceS4*` in rg, casino and sportsbook. It **SURVIVED** (all `ok`; reverted). In production the effect would be that a player with net
winnings in the window could stake beyond the loss limit by the amount won, while the configuration says the floor of 0 applies. The current code is
correct, but nothing pins it. Required before merge: (a) a test with a floor override `loss_may_go_negative: false`, a player with stake 100 and
win 300 in the window (raw -200), and a loss limit of 100. A stake of 100 must be allowed and 101 refused, with figures `usage` = "0" and
`loss_raw_usage` = "-200". (b) The same set-up with `true`: 300 allowed, 301 refused. (c) Add the mutant to `a6-mutation-driver.py` as
`loss_may_go_negative_forced_true`. Recommended: make the engineering floor (false) the default of `EnforcedFloor`, so that tests default to the
protective reading.

**LF-A6-2 (Medium, ACTIVATION condition; not a merge blocker): the heavy-player cost. The proposal of section 5 is right in direction but too narrow as
worded.** My independent measurement used the same profile (150,000 bets + 150,000 wins in 29 days, 20 deposits, `ANALYZE`), an idle box, five runs
each, and a 60 s timeout so the figures are the scan cost:

| Evaluation | p50 | max |
|---|---|---|
| casino_wager, wager 24h + 7d, loss 7d | 0.64 s | 0.69 s |
| casino_wager, plus wager and loss 30d and wager calendar_month | 2.94 s | 3.40 s |
| deposit, 24h + 7d | 0.40 s | 0.41 s |
| deposit, 24h + 7d + 30d | 1.64 s | 1.70 s |

The 30-day stake figure uses 74% (p50) to 85% (max) of the 4 s budget on an idle box. The author's 3.46 s / 4.08 s at load 20+ confirms that it fails
closed under load. Three precisions the proposal needs:
1. **It is enforced through `allowed_periods`, not through what players choose.** `kindPlan.build` applies a numeric ceiling to EVERY allowed period, and a
   player may set a limit for any allowed period. So "windows of 7 days or less" means that the floors of wager AND loss list only `rolling_24h`,
   `rolling_7d`, `calendar_day` and `calendar_week`.
2. **The deposit read also scans every ledger row of the player in the window,** not only the deposits (the `ent` CTE filters by type after the join).
   It is cheaper per row: 1.64 s at 300,000 rows leaves about 2.4 times headroom, so a 30-day or calendar-month DEPOSIT limit is acceptable for the
   first brand at this profile. It fails closed at roughly 600,000 or more rows in 30 days on an idle box, and fewer under load. Monthly deposit limits are the
   ones regulators most often require, so this matters.
3. The L0.4 hold equals the evaluation time, so concurrent actions of one heavy Person serialise (LF-A6-3).

**Recommendation:** accept points (1) to (3) of section 5: do not raise the timeout, do not build the rollup in A6, and record the limit. Adopt (4) in
this form. For the first brand, the wager and loss floors use `allowed_periods` of at most 7 days. The deposit floor may include `rolling_30d` and
`calendar_month`. Schedule the ledger-derived rollup (a ledger-finance slice: projection, hourly drift job, P1 on drift) BEFORE any brand allows a
30-day or calendar-month wager or loss period. If compliance says that monthly wager or loss limits are legally required for Anjouan or for the
first brand, the rollup becomes a launch prerequisite. That is a legal question for compliance, not a ledger-finance call. Recommended, not
blocking: an R-RG-CONFIG readiness failure when a wager or loss floor allows `rolling_30d` or `calendar_month` while the rollup is `NOT IMPLEMENTED`,
so the constraint is enforced and not only documented.

**LF-A6-3 (Low; availability only): heavy-Person concurrency.** The L0.4 lock is held from the evaluation to commit. A second concurrent action of the
same Person waits. In casino the wait happens first on L0.2, under the session `lock_timeout`. On sportsbook and deposit it happens on L0.4, under
the 2 s `DefaultActionLockTimeout`, and fails closed when exceeded. This is never an unsafe allow. With LF-A6-2 applied (about 0.6 s), it is negligible.

**LF-A6-4 (Medium, OWNER ACCEPTANCE required before activation): `pending_intent_ttl` has no effect. The risk in figures:**
- What counts: every intent of the Person's measured accounts in status `pending` or `ambiguous` with no ledger posting, at its REQUESTED amount, in
  every deposit window that contains its `created_at`.
- Nothing releases it. The sweeper escalates a stale submitted deposit (T16 alert, `escalateDepositIfDue`) but does not fail it. K3 M1 only records
  evidence on a disputed deposit. No operator path moves a stuck deposit attempt to failed. Only a provider outcome (webhook or poll) or ageing out of
  the window releases it.
- Duration: up to the longest allowed deposit window. That is 24 h for `rolling_24h`, 7 days for `rolling_7d` and 30 days for `rolling_30d`. A
  calendar window holds it until the period ends, so an intent stuck on the 1st of a month blocks for up to 31 days.
- Amount: each intent was admitted at T1 with usage + amount <= limit, so the stuck total is at most the smallest deposit limit. The worst case is
  one stuck intent equal to the remaining headroom, which blocks every deposit of the Person for the whole window.
- Direction: never an unsafe allow and never a double count (one snapshot: once `ledger_transaction_id` is set the in-flight side drops it). A
  stuck intent that later succeeds counts at its posting time. That is correct (the money arrived then), but it can lift usage above the limit
  after the fact, and the `rg_deposit_limit_exceeded_post_facto` alert is not built (residual 5).
- Frequency: unknown. It is `PROVIDER DEPENDENT` (all providers are MOCK, and there is no rate data).

Owner and compliance must accept, in writing: (i) a player can be refused deposits for up to the longest allowed deposit window (31 days with
`calendar_month`) because of a payment stuck on the provider side that the player did not cause, with no operator override. (ii) The player sees
`deposit_limit_reached`, not "a payment is still pending". The alternative to acceptance is a payments slice: a governed staff resolution that
moves a stuck deposit attempt to failed through the attempt state machine, which releases the usage automatically. It needs ledger-finance review.
If the first brand uses only 24 h deposit windows, the exposure is at most 24 h. With a monthly window it is up to 31 days.

**LF-A6-5 (Low / informational; PERSON-RELINK-PIN-1): a possible deadlock between a re-link and an evaluation.** `evaluateEligibility` locks the
Person read before the lock (A) and, if the account moved, then the new Person (B), not in hash order. Two re-links of the same Person pair
in quick succession, with an evaluation in between, can form an A/B cycle. PostgreSQL's deadlock detector aborts one side: the evaluation fails
closed, or the re-link is retried. No Go re-link writer exists. When it lands, its tests must include an evaluation running concurrently with two
re-links of one Person pair.

**LF-A6-6 (Informational): the T2 double count is conservative and also desirable.** It occurs only when an earlier attempt of the same intent has
already credited (a late success) while a cascade attempt is still `created`. Refusing that cascade prevents a second charge for a deposit that has
already arrived.

**LF-A6-7 (Low, not blocking): "a withdrawal is not a stake" is proven by code only.** Add a test. Request a withdrawal (hold), then reject it or let it
fail (release). Wager, loss and deposit usage must be unchanged by both postings.

### 11.3 Conditions

- Merge: LF-A6-1 (the test, with the mutant killed). SR6-1 from section 10 also blocks, independently.
- Activation of the switch for any brand: the LF-A6-2 floor configuration (wager and loss at most 7 days) or the rollup slice merged, and the
  LF-A6-4 owner and compliance acceptance recorded in the human-decision register.
- Follow-ups, not blocking: LF-A6-7, the LF-A6-2 readiness check, LF-A6-5 with the re-link writer.

## 12. Fix round after the reviews (SR6-1..5, LF-A6-1..7)

| Finding | Disposition |
|---|---|
| SR6-1 | DONE. `POST /v1/admin/rg/enforcement` is step-up `critical` (route registration, `staffRouteGolden`, step-up count pins: critical 63, none-mutating 42, read 83; OpenAPI text). GET stays `none`. Guard (the recommended one, implemented): activation is refused with 409 `conflict` and the closed R-RG-CONFIG detail (`no_jurisdiction_referenced`, `no_currency`, `floor_missing_deposit|loss|wager`, `session_limit_not_enforced`, `session_aggregation_unsupported`) unless the tenant's LICENCE jurisdiction (the one enforcement reads; refused as `no_licence_jurisdiction` when the tenant has none) AND every jurisdiction with a configuration in effect have a floor for deposit, loss and wager in every allowed currency (SR6R-1 fix, section 15). A refusal is audited as `rg.enforcement.activation_denied` and writes nothing. A repeat for an already switched-on scope is not re-checked (idempotent 200). Function: `launchgov.RGActivationReadiness` (floor part of R-RG-CONFIG, `rgFloorChecks`). Tests: stale session gets 403 `step_up_required`; no configuration and no floors get 409; fixed after floors exist. The back office does not call this route (checked), so there is no BO UI change. |
| LF-A6-1 | DONE. `s4_LossMayGoNegative`: floor `loss_may_go_negative=false`, stake 100 and win 300 (raw -200), loss limit 100: 100 allowed, 101 refused with `usage` 0 and `loss_raw_usage` -200. True: 300 allowed, 301 refused (usage -200). Mutant `loss_may_go_negative_forced_true` added to `a6-mutation-driver.py`; kill result in section 12.1. The engineering default of `EnforcedFloor` is NOT changed (it would alter every S4 test's headroom); the false path now has its own test. |
| SR6-2 | DONE. `TestWDRG1_StandardGate_TenantIsolation_DirectEvaluate`: the gate called under a tenant-B session with tenant-A ids (and tenant-B ids with tenant A's account) returns an error and never Allowed. |
| SR6-3 | DONE. `RequestParams` gained optional IPAddress, UserAgent, RequestID; the denial audit records `ActorPlayer` with the account id and that metadata; the handler fills them. Test `TestWDRG1_DenialAudit_RecordsPlayerActorAndRequestMetadata`. |
| SR6-4 | DONE in the handler (`gateDenied != nil && err != nil` gives 503 before the 403). NOT unit-tested: forcing a commit failure of the denial-audit transaction through the HTTP stack needs fault injection that the harness does not offer. Residual, low. |
| SR6-5 | DONE. `queryClient.clear()` on b2c logout (AuthProvider uses `useQueryClient`); test in `AuthContext.test.tsx`; `renderWithProviders` now also returns the `queryClient`. |
| LF-A6-7 | DONE. `TestWDRG1_WithdrawalHoldAndRelease_DoNotChangeStakeOrDepositUsage`: wager, raw loss, credited and in-flight deposit usage are identical before the request, after the hold, and after the release (Cancel). |
| LF-A6-2 | RECORDED, ACTIVATION CONDITION. For the first brand the wager and loss floors' `allowed_periods` must be limited to `rolling_24h`, `rolling_7d`, `calendar_day`, `calendar_week` (7 days or less), because a numeric ceiling applies to every allowed period and a player may set any allowed period. The deposit floor may include `rolling_30d` and `calendar_month` (1.64 s at 300,000 rows idle; fails closed around 600,000 rows). The ledger-derived rollup must be scheduled BEFORE any brand allows a 30-day or calendar-month wager or loss period; if compliance says monthly wager or loss limits are legally required, the rollup is a launch prerequisite. Not built: the recommended readiness failure for a wager or loss floor that allows `rolling_30d` or `calendar_month` (follow-up). |
| LF-A6-3 | Accepted (availability only, never an unsafe allow). |
| LF-A6-4 | RECORDED, OWNER ACCEPTANCE REQUIRED BEFORE ACTIVATION. A deposit intent stuck pending or ambiguous with no ledger posting counts at its requested amount in every deposit window containing its `created_at` until it ages out of the window or a provider outcome resolves it: up to 24 h (`rolling_24h`), 7 days (`rolling_7d`), 30 days (`rolling_30d`) or until period end (a stuck intent on the 1st of `calendar_month` blocks up to 31 days). The player sees `deposit_limit_reached`, not "a payment is pending", and no operator can override. The alternative to written acceptance is a payments slice: a governed staff resolve-to-failed path through the attempt state machine, reviewed by ledger-finance. |
| LF-A6-5 | RECORDED (PERSON-RELINK-PIN-1). An evaluation locks the Person read before the lock and, if the account moved, the new Person, not in hash order; two re-links of one Person pair in quick succession around an evaluation can form an A/B cycle. PostgreSQL's deadlock detector aborts one side (the evaluation fails closed or the re-link is retried). No Go re-link writer exists. When it lands, its tests must run an evaluation concurrently with two re-links of one Person pair. |
| LF-A6-6 | Informational: the T2 double count is conservative and desirable. |

### 12.1 Counts and commands (after the fix round)

Private scratch DB, `-tags integration -count=1 -p 1 -v`; counts are PASS lines (tests and subtests), after merging origin/claude/focused-wright-jw88w9 (30f77f7 already contained).
- rg whole package: 187 PASS, 0 FAIL, 1 SKIP (the gated heavy run). `-race` rg (`RGEnforceS4|RGLimits|Eligib`): 50 PASS, 0 FAIL, 1 SKIP.
- withdrawal whole package: 164 PASS, 0 FAIL; `-race` whole package: 164 PASS, 0 FAIL.
- launchgov whole package: 123 PASS, 0 FAIL. tenant: 64 PASS, 0 FAIL. db (migration pins): 310 PASS, 0 FAIL, 1 SKIP.
- httpserver `RGEnforcement|RGOptions|WDRG1|WithdrawalRGGate|OpenAPI|StepUp|Golden|Staff|Withdraw`: 353 PASS, 0 FAIL.
- casino, sportsbook, payments `RGEnforceS4`: 8, 4 and 4 PASS, 0 FAIL. The full payments suite is still not completed in this environment (see 7b).
- Not re-run in this round: bonus, risk, kyc, wallet, reconciliation, identity (untouched by the fix round; passed in 7b).
- `go test ./...` PASS; `go vet` plain and `-tags integration` PASS; gofmt clean; golangci-lint 2.9.0 0 issues; b2c build PASS and `npm test` 128 tests PASS (127 + the logout cache test).
- Mutation: `loss_may_go_negative_forced_true` KILLED (it SURVIVED before the LF-A6-1 test); `min_becomes_max` and `stake_boundary_ge` re-run KILLED. 31 mutants in `a6-mutation-results.tsv`.
- Test stability change: `FailClosed/StatementTimeout_FailsClosed_NeverZeroUsage` failed once on a fast idle box with a bare 1 ms budget (the read finished inside it). It now holds an ACCESS EXCLUSIVE lock on `ledger_entries` so the read cannot finish; 3 of 3 repeat runs PASS. The assertion (fail closed, never zero usage) is unchanged.
- Changed assertions this round: step-up golden counts (critical 62 to 63, none-mutating 43 to 42); `TestWDRG1_TenantIsolation_ForeignAccountIsRefused` left as is (SR6-2 is covered by the new direct gate test); HTTP enforcement test now creates a jurisdiction configuration and floors before the first activation (the new SR6-1 guard) and asserts the 409 before that.
- Not unit-tested: SR6-4 (commit failure of the denial-audit transaction answers 503).

## 13. Paste-ready text for the human-decision register (proposal; the register is not edited here)

**HDR-A6-1: RG enforcement interpretations awaiting confirmation (owner and compliance).** The first brand's RG limits are mandatory (owner decision 2026-10-15). The software ships DEFAULT OFF behind a one-way per-brand or tenant switch (step-up critical, refused unless compliance floors are in force in the tenant's licence jurisdiction and in every jurisdiction its market configuration names). Four interpretations were made by the implementer and need written confirmation: (1) a numeric ceiling in the jurisdiction floor is a HARD CAP on every allowed period, whether or not the player set a value, and a mandatory kind with a numeric ceiling does not require a player value; (2) removing a mandatory limit is treated as an increase (it waits out the cooling-off period, the old value binds meanwhile) and once realized the kind is mandatory-without-value again and refuses; (3) withdrawals are ALLOWED during a time-out or self-exclusion (the player's funds stay reachable; the restriction in force is audited), and a risk REVIEW outcome refuses a withdrawal like a DENY because no review queue exists on that path; (4) enforcement cannot be switched off by the application (append-only, one-way); switching off is a recorded decision plus a migration. Decision needed: confirm or reverse each.

**HDR-A6-2: monthly windows and the rollup (LF-A6-2).** A 30-day or calendar-month wager or loss window costs 2.9 s (p50) to 3.4 s (max) of the 4 s fail-closed budget for a player with 300,000 ledger rows in 30 days; heavier players are refused. Decision needed: for the first brand, restrict the wager and loss floors to periods of 7 days or less (default, no rollup needed; deposit floors may keep 30-day and calendar-month), OR schedule the ledger-derived rollup before launch. If compliance rules that monthly wager or loss limits are legally required for Anjouan or the first brand, the rollup becomes a launch prerequisite (a legal question for compliance).

**HDR-A6-3: stuck deposit intents block deposits (LF-A6-4).** `pending_intent_ttl` has no effect. A deposit stuck on the provider side counts toward the player's deposit limit until it resolves or ages out: up to 24 h, 7 days, 30 days or 31 days with a calendar month, with no operator override, and the player sees `deposit_limit_reached`. Decision needed before activation: accept this in writing (compliance and owner), or fund a payments slice with a governed staff resolve-to-failed path (ledger-finance review).

**HDR-A6-4: PERSON-RELINK-PIN-1 and the re-link deadlock window (LF-A6-5).** No re-link writer exists. When one is built it must take the Person locks in hash order, and its tests must race an evaluation against two re-links of one pair. Open until then.

## 14. Security re-check of the fix round (security specialist, 2026-10-16, tip 881642b)

Scope: the A6 commits 8df678d, 2907dbe, bebd6d8 and 7347eba (SR6-1 to SR6-5, LF-A6-1). Not re-reviewed: the merged A8, A11 and A12 content.
Method: code review, plus a scratch probe test (not committed) and a hand-applied mutant (reverted), run on the private scratch DB
`sec_a6_review`, rebuilt at this tip. Suites re-run with `-tags integration -count=1 -p 1`: rg, withdrawal, httpserver (RG enforcement, RG options, WDRG1,
OpenAPI RG, step-up golden table, staff route), casino, sportsbook, payments with the A6 test filters. Result: 96 PASS, 1 SKIP (the gated heavy run). b2c
`AuthContext.test.tsx`: 4 PASS.

**Verdict: NOT APPROVED. One blocking finding (SR6R-1).** The step-up part of SR6-1 and SR6-2 to SR6-5 are closed. The LF-A6-1 test is sound.

| Item | Result |
|---|---|
| SR6-1 step-up | CLOSED. `stepUpCritical` on the POST, golden entry `critical`, counts critical 63 and none-mutating 42. Test: a stale session gets 403 `step_up_required` before the handler runs. GET stays `none`. |
| SR6-1 guard: audit and leak | PASS. The 409 refusal is audited in its own transaction as `rg.enforcement.activation_denied` with `code=activation_not_ready`, `readiness_result`, `readiness_detail`, actor, IP, user agent and request id (seen in the probe). The 409 message carries only the closed detail vocabulary (`no_jurisdiction_referenced`, `no_currency`, `floor_missing_<kind>`, `session_*`, `read_error`) to a compliance caller who holds `rg_config:read` anyway: no jurisdiction code, currency or value is disclosed. A refused POST writes no activation row. |
| SR6-1 guard: bypass | **FAIL, SR6R-1** (below). |
| SR6-2 | CLOSED. `TestWDRG1_StandardGate_TenantIsolation_DirectEvaluate` calls `standard.Gate.Evaluate` directly under a tenant-B session with tenant-A ids (and with tenant A's account). It asserts an error and never `Allowed`. The HTTP suspended-account case still accepts 401. Left as is: the direct gate test now carries the isolation claim. |
| SR6-3 | CLOSED. The denial audit records `ActorPlayer` with the account id, IP, user agent and request id. The handler fills them in, and the test asserts all five. |
| SR6-4 | CLOSED by code (`gateDenied != nil && err != nil` returns 503 before the 403). There is no test. Residual risk is low and was disclosed by the author. |
| SR6-5 | CLOSED for explicit logout (`queryClient.clear()`, with a test). Residual, Low (SR6R-2): the session-expiry paths (`api/client.ts` lines 103 and 111, `AuthContext.tsx` line 49) call `clearSession()` without clearing the cache. |
| LF-A6-1 test | SOUND. It is an evaluation-only check after one stake of 100 and one win of 300, with only a loss limit of 100 set. It pins both directions: false gives usage 0, 100 allowed, 101 refused; true gives usage -200, 300 allowed, 301 refused. It also checks `loss_raw_usage` -200. I applied the unsafe mutant `req.LossMayGoNegative = true` myself: `false_floors_usage_at_zero` FAILS (101 allowed), so the mutant is killed. Reverted, and the tree is clean. The deterministic statement-timeout test (an ACCESS EXCLUSIVE lock held by the owner) keeps the assertion and is valid. |

**SR6R-1 (Medium, BLOCKING): the activation guard checks a different jurisdiction from the one enforcement uses, so it can pass while every deposit and wager of the scope is refused.**
- **What the guard checks:** `launchgov.RGActivationReadiness` checks the jurisdictions of the tenant's in-effect `tenant_jurisdiction_configs`.
- **What enforcement reads:** `rg.EvaluateAction` reads its floors from `tenantJurisdiction`, which is `tenants.licence_id -> licences.jurisdiction_id`. That is the licence jurisdiction, and it is NULL when the tenant has no licence.
- **Reproduced (scratch probe, not committed):**
  1. Tenant A is licensed in jurisdiction J1, which has no floors. Its in-effect configuration names J2, which has deposit, loss and wager floors for EUR.
  2. POST `/v1/admin/rg/enforcement` answers 201.
  3. `EvaluateAction(deposit, EUR)` then answers `rg_config_missing`, so every deposit and wager of the tenant is refused, irreversibly.
- **Same outcome without a licence:** the same happens for any tenant whose `licence_id` is NULL (`jur == nil` gives "not found" for every kind).
- **Why this is realistic:** the hybrid licensing model (ADR 0006) makes a gap between the licence jurisdiction and the market configuration likely.
- **Severity:** the critical step-up already closes the stolen-session path. What remains is the honest-mistake halt that the guard exists to prevent. The guard is documented as a guarantee in the OpenAPI text, in section 12 and in the paste-ready HDR-A6-1 ("refused unless compliance floors are in force"), and that guarantee does not hold.
- **Required:** include the jurisdiction `tenantJurisdiction` returns in the guard's set, and refuse (`no_licence_jurisdiction` or similar) when it is NULL. Add a test for the mismatch case and the no-licence case. If the guard cannot be fixed now, remove the guarantee from the OpenAPI text, section 12 and HDR-A6-1 instead.
- **Related, for the architect (not blocking A6):** the launch readiness check R-RG-CONFIG checks the request's `JurisdictionIDs`, so it has the same gap. More broadly, which jurisdiction governs RG under hybrid licensing (the licence or the market) is a cross-domain decision. Today enforcement uses only the licence jurisdiction.

**SR6R-2 (Low, non-blocking):** session expiry and a 401 do not clear the react-query cache (see the SR6-5 row). Clear it at the same point as `clearSession()`, or subscribe the query client to session changes.

## 15. Fix round 2: SR6R-1 and SR6R-2

| Finding | Disposition |
|---|---|
| SR6R-1 | DONE. `launchgov.RGActivationReadiness` now reads the LICENCE jurisdiction (`tenants.licence_id -> licences.jurisdiction_id`, what `rg.tenantJurisdiction` reads) and adds it to the set; a tenant with no licence jurisdiction is refused (`no_licence_jurisdiction`). The rule is deliberately at least as strict as enforcement: floors are required in the licence jurisdiction AND in every jurisdiction with a market configuration in effect, in every allowed currency of those configurations. Tests (`rg_enforcement_licence_guard_integration_test.go`): licensed in J1 without floors, configuration naming J2 with floors gives 409 `floor_missing_deposit` and nothing is written; once J1 has floors, 201; a tenant with no licence gives 409 `no_licence_jurisdiction`. The OpenAPI text, section 12 and HDR-A6-1 now state the guarantee only in that form. |
| Launch readiness R-RG-CONFIG | SAME GAP, FIXED (cheap, and the existing tests only needed a licence in their fixtures). `rgConfigChecks` adds the licence jurisdiction to the request's `JurisdictionIDs` and fails `no_licence_jurisdiction` when there is none. Fixture change: the two existing R-RG-CONFIG tests now bind a licence (`w.licence`) to the tenant (their assertions are unchanged). New test `TestReadiness_RGConfig_CoversTheLicenceJurisdiction`: no licence fails, licence jurisdiction without floors fails although the requested jurisdiction is configured, both configured passes. |
| Architect question (open, cross-domain) | Under the hybrid licensing model (ADR 0006), which jurisdiction governs RG limits: the licence jurisdiction (today: the only one enforcement reads) or the market jurisdiction of the player or brand? A6 takes the strict reading (floors needed in both), which can make activation harder than necessary but never lets a halt through. The resolution belongs in a decision record, not in A6. |
| SR6R-2 | DONE. The AuthProvider subscribes to the token store and clears the react-query cache on every signed-in to signed-out transition (explicit logout, session expiry, refused refresh, 401 in `api/client.ts` and the bootstrap effect all end in `clearSession()`). Test: clearing the session directly empties the cache. |

Paste-ready addendum for HDR-A6-1 (proposal): the activation guard and the launch-readiness check R-RG-CONFIG both require compliance floors in the tenant's licence jurisdiction and in every market-configured jurisdiction; whether RG should be governed by the licence jurisdiction or the market jurisdiction under hybrid licensing is an open architect question.

### 15.1 Counts

Private scratch DB, `-tags integration -count=1 -p 1 -v`, PASS lines (tests and subtests), 0 FAIL in every row:
- launchgov whole package: 124 PASS (was 123; +1 new licence-jurisdiction test).
- httpserver `RGEnforcement|RGOptions|WDRG1|WithdrawalRGGate|OpenAPI|StepUp|Golden|Staff|Withdraw`: 354 PASS (was 353; +1 licence-guard test).
- withdrawal whole package 164 PASS; rg `RGEnforceS4|Migration0158` 32 PASS, 1 SKIP; casino 8, sportsbook 4, payments 4 (`RGEnforceS4`).
- `go test ./...` PASS; `go vet` plain and `-tags integration` PASS; gofmt clean; golangci-lint 2.9.0 0 issues; b2c build PASS, `npm test` 129 tests PASS (128 + the session-end cache test).
- Not re-run (unchanged this round): `-race` rg and withdrawal, tenant, db pins, bonus, risk, kyc, wallet, reconciliation, identity, full payments (still not completed here, see 7b).

## 16. Security final check of SR6R-1 and SR6R-2 (security specialist, 2026-10-16, tip 1a727a1)

Scope: commits 0ff5096 and 1a727a1. Private scratch DB `sec_a6_review`, rebuilt at this tip. Re-run with `-tags integration -count=1 -p 1`: httpserver (RG enforcement, RG options, step-up golden), launchgov (RG config readiness), rg (`TestRGEnforceS4`), withdrawal (WDRG1). Result: 66 PASS, 1 SKIP (the gated heavy run). b2c `src/auth`: 13 PASS.

**Verdict: APPROVED for merge from a security standpoint.** SR6-1 to SR6-5 and SR6R-1 and SR6R-2 are closed, and no blocking finding remains. This approval does not cover activating the switch for a brand: that still depends on the ledger-finance conditions LF-A6-2 and LF-A6-4 and on the HDR decisions in section 13.

| Question | Result |
|---|---|
| Is the guard at least as strict as enforcement? | YES. The guard reads the same jurisdiction as enforcement. Enforcement (`tenantJurisdiction`) uses `tenants.licence_id -> licences.jurisdiction_id -> jurisdictions.code`, with inner joins. The guard and `rgConfigChecks` read the same `licence_id -> jurisdiction_id`, then the same `jurisdictions` row by id (`rgFloorChecks` requires the counts to match). A NULL licence, or a licence row hidden by RLS, gives `no_licence_jurisdiction`, so the guard refuses. Floor presence comes from the same `rg_limit_config_compose(...).found` value that `readUsageConfigFor` uses. That value exists only when a platform floor (`tenant_id IS NULL`) is in force for the jurisdiction, kind and asset. Floors have no `effective_to`, so `found` is brand-independent, and a later floor version never turns it false. |
| Expired or inactive licence | No mismatch. `licences.status` and `expires_at` are filtered by neither `tenantJurisdiction` nor the guard, so both read the same jurisdiction. Enforcement keeps using an expired licence's floors, which is protective. Whether play should continue on an expired licence is a separate launch-governance matter and outside this item. |
| RLS on `licences` | The same row is visible in both sessions. The guard runs under a tenant principal scope with no player. `tenantJurisdiction` clears the player scope before the read. `licences_read` admits both. |
| Does R-RG-CONFIG (`rgConfigChecks`) include the licence jurisdiction? | YES. It is appended to the request's `JurisdictionIDs`, and NULL fails with `no_licence_jurisdiction`. Test `TestReadiness_RGConfig_CoversTheLicenceJurisdiction` covers no licence, a licence jurisdiction without floors, and floors in both jurisdictions. The HTTP test `..._ActivationGuard_UsesTheLicenceJurisdiction` reproduces my SR6R-1 probe: it now answers 409 `floor_missing_deposit`, and 201 once the licence jurisdiction has floors. A tenant with no licence gets 409 `no_licence_jurisdiction`. |
| b2c: is the cache cleared on every transition from signed-in to signed-out? | YES. `AuthProvider` subscribes to the token store and clears the cache when the access token goes from non-null to null. Every sign-out path ends in `clearSession()`, which emits to subscribers: explicit logout, a bootstrap that fails to refresh, the refresh refusal and the 401 in `api/client.ts`. A token refresh keeps the token non-null, so it does not clear. The store is in-memory and per tab, so there is no cross-tab path. Tests cover both the explicit logout and a direct `clearSession()`. |

Informational. No action needed in A6; these are for the launch risk list.
- I-5: the guard checks floors for the union of `allowed_currencies` in the in-effect configurations. An action in an asset outside that set (for example a wallet asset no configuration allows) is refused with `rg_config_missing` once the switch is on. This is a partial halt confined to assets that are not configured, and it fails closed.
- I-6: a change made after activation (a platform admin repointing `tenants.licence_id` to a jurisdiction without floors) can still halt the tenant. R-RG-CONFIG now detects this at readiness time. Licence changes are platform-admin, critical-step-up operations.
- Carried over: I-2 (WD050/WD051 have no database backstop, and payout dispatch does not re-run the gate) stays on the launch risk list.

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

# Ledger-finance review record: licensing slice 3 (lic3) and 0176 / 0177, financial-invariant view

- **Reviewer:** `ledger-finance` specialist, independent review (report only; no code changed).
- **Worktree / branch:** `.claude/worktrees/lfrev3`, branch `lfrev3`, from `claude/licensing-jurisdiction-completion` at `d600295`.
- **Scope:** `docs/plans/lic3-enforcement-record.md` and the code merged with it (`git diff af4e18d 7d414cc`), plus the
  money-path effect of 0176 (licence lifecycle) and 0177 (governed licensing writers).
- **Method:** code and SQL reading only. The private-database harness was NOT run (subagents may not run it); every DB
  fact below comes from migrations and Go source and is marked "not run" where it would need a live probe. No
  concurrency/load test was run.
- **Not a legal opinion.** Nothing here says whether any licence, market or payout behaviour is legally required.

## 1. Verdict

**APPROVE WITH CONDITIONS.**

lic3 adds no ledger write, no balance mutation, no floating point and no new money path. The new launch/bootstrap
status gate sits only on paths that mint sessions (launch handler, `LaunchGame`, bootstrap); it never reaches
`postWin`, `postRollback`, the stake-return path, sportsbook settlement, deposits, withdrawals or payouts. No lock
cycle was found. The chain-helper refactor changes no money-path outcome for a valid tenant session. Licence
expiry/suspension does not gate withdrawals, payout claims or payout-instrument verification.

Conditions (none blocks merge of lic3 itself; C1 and C2 must be closed before the first real-money tenant):

- **C1 (from F-1).** Detection for legacy stored risk rules that refuse every evaluation: a readiness item or a
  platform report listing active `risk_rules` with `jurisdiction_code IS NOT NULL` (any operation) or
  `operation = 'deposit'`. A legacy jurisdiction-scoped `withdrawal` rule refuses every withdrawal request and
  every dispatch today (player funds stuck); lic3 prevents new ones but does not find old ones.
- **C2 (from F-2).** The owner must decide, and record, what happens to player funds when a tenant or brand is
  SUSPENDED (not the licence): today new withdrawal requests AND payout claims are refused (H-SEC-11). Licence
  status does not change tenant/brand status automatically, so the "withdrawals keep working on licence
  suspension" position holds only as long as no one suspends the tenant/brand in response.
- **C3 (from F-3).** Record the bootstrap lock sequence in ADR 0082 (the orchestrator places the text; proposed
  below), including the invariant that keeps it cycle-free.

## 2. Findings (severity-ranked)

### F-1 MEDIUM (pre-existing, not introduced): legacy jurisdiction-scoped risk rules still strand withdrawals

- **Where:** `internal/risk/evaluator.go:233` (`ErrMissingJurisdiction` for any jurisdiction-scoped rule when the
  request supplies no `jurisdiction_code`); the withdrawal gate supplies none (lic3 record G6, and
  `internal/risk/policy_service.go:150-160`). Refusal added at `internal/risk/policy_service.go:177-184`.
- **What lic3 does:** `CreateRule` refuses new deposit and jurisdiction-scoped rules. Stored rows are untouched and
  evaluate exactly as before (no evaluator change in the diff). `DisableRule`
  (`internal/risk/policy_service.go:294-298`) has no new check, so an operator CAN still disable a legacy rule: the
  refusal does not strand an enabled rule. No production code calls `CreateRule` except the HTTP handler
  (`internal/httpserver/risk_handlers.go:243`), so no onboarding/seed flow breaks. A deposit rule was never
  evaluated (no `risk.Evaluate` caller uses `OperationDeposit`), so refusing it changes no deposit flow.
- **Residual:** an existing active jurisdiction-scoped `withdrawal` rule refuses every withdrawal request and every
  payout dispatch (`EvaluateDispatchGate`, `internal/payments/payout.go:449`). Nothing finds it. Condition C1.
- **Defence in depth:** the refusal is Go-only; a raw INSERT by the runtime credential can still create such a row
  (also security F-9). Accept with C1's detection.
- **Note (capability, not safety):** casino, sportsbook and bonus callers do pass a `jurisdiction_code` in some
  paths, so the blanket refusal is stricter than strictly needed for those operations. That is a narrowing of
  capability, not a financial risk; it lifts with G3 as the lic3 record says.

### F-2 MEDIUM (pre-existing, human decision): tenant/brand suspension stops withdrawals and payouts

- **Where:** `internal/withdrawal/withdrawal.go:485` (new request refused), `internal/payments/payout.go:440,467`
  (payout claim `approved -> submitted` refused; request stays `approved`, funds stay in the hold), DB backstop
  `migrations/0131_withdrawal_requests_subject_active_guard.up.sql` (WD010/WD011).
- **Why it matters here:** the owner's position is "withdrawals/payouts keep working while a licence is suspended or
  expired". Verified: licence lifecycle writes (`internal/jurisdiction/licence_lifecycle.go:231,258,281,308`) and
  the sweep (`internal/jurisdiction/licence_sweep.go`) touch only `licences` / `licence_versions` / alerts; nothing
  sets `tenants.status` or `brands.status` from a licence state. So licence status alone never stops a withdrawal.
  But the natural operational response to a suspended licence is a governed tenant/brand suspension, and that DOES
  stop new withdrawals and payout claims. Player money is then held, recoverable only through the governed hold
  release (0124), not payable.
- **Decision needed (human):** whether a suspended (as opposed to closed) tenant/brand must still allow withdrawal
  requests and payout claims. Interim safe behaviour: unchanged (refused, funds held, nothing lost; balances and
  holds remain ledger-backed and reconcilable).

### F-3 LOW: bootstrap takes the status advisory keys after a row lock (letter of ADR 0082 R8), no cycle

- **Where:** `internal/casino/bootstrap.go:250-252` (session row `FOR NO KEY UPDATE`), then
  `bootstrapGateDenialReason` `internal/casino/bootstrap.go:322-335` (tenant key SHARED, then brand key SHARED),
  then RG person lock L0.4 (`evaluateAndAuditEligibility`, :365).
- **Rule:** ADR 0082 R8 "advisory locks strictly precede row locks"; ADR 0082 LF1 row: status keys before L0.2,
  tenant key before brand key.
- **Analysis (why no deadlock):**
  1. Status keys are only ever taken EXCLUSIVE by a status change: `tenants_status_change_gate`
     (`migrations/0118...up.sql:50-63`) and the brand branch of `launch_subject_status_guard`
     (`migrations/0129...up.sql:88-92`). Neither function, nor any Go caller of a governed status change, locks or
     updates a `casino_launch_sessions` row (all writers: `internal/casino/launch.go:344,349,404,494,503`,
     `internal/casino/bootstrap.go:279`; none is on a status-change path). So no transaction holds a status key
     EXCLUSIVE while waiting for the session row.
  2. Shared/shared on the keys never conflicts. `postBet` takes the keys SHARED (L0.1, tenant key at
     `orchestrator.go:1814`, brand key at :1896) and touches the session row only through FK `FOR KEY SHARE`, which
     is compatible with `FOR NO KEY UPDATE` (the documented reason bootstrap uses NKU, `bootstrap.go:201-240`).
  3. Queue fairness case: bootstrap B holds the session row and queues SHARED behind a waiting EXCLUSIVE writer W;
     W waits for a SHARED holder T (postBet/LaunchGame). T would have to wait for B's session row to close a cycle.
     T's only interaction with that row is FK KEY SHARE (compatible) or, for `LaunchGame`'s phase-C
     `RevokeLaunchSession` (`orchestrator.go:603`), a separate transaction that holds no status key. No cycle.
  4. Tenant key before brand key in all three new call sites, same as postBet. Brand status writers lock the
     `brands` row then the brand key; no new path locks a `brands` row (plain reads only,
     `internal/tenant/gameplay_gate.go:121`).
- **Residual:** the argument depends on invariant (1). It is not pinned by a test. Not load-tested (not run).
  Condition C3 records it.

### F-4 LOW: in-flight round resumption after a brand/tenant suspension now depends on the vendor

- **Where:** `internal/casino/orchestrator.go:332` and `internal/httpserver/casino_handlers.go:227` (launch),
  `internal/casino/bootstrap.go:322-335` (bootstrap).
- **Ledger effect:** none. Settlement of already-placed bets is unaffected by the new gate (see section 3). But a
  player can no longer relaunch to resume an interrupted round, so completion relies on the vendor's own
  auto-complete / rollback callbacks. For a brand suspension the platform accepts both a win and a rollback; for a
  tenant suspension a new win is refused (ADR 0095 s40.5, owner decision) and only a stake return is accepted, which
  is the open Q-GP-6. lic3 did not change that decision; it removes the player-side resume route that might
  otherwise have prompted the vendor to send the win. Interim: keep the gate (minting sessions for a suspended
  brand is worse); attach this to Q-GP-6 for the human decision.

### F-5 LOW: two-transaction window between the handler gate and `LaunchGame`

- **Where:** `internal/httpserver/casino_handlers.go:219-251` (gate, then `wallet.GetOrCreate`, commit), then
  `LaunchGame`'s own transaction (`orchestrator.go:329-338`).
- A brand suspended between the two commits leaves a newly created, empty wallet and a denied launch. No ledger
  entry, no projection change, idempotent get-or-create, nothing to reconcile. Within one transaction the order is
  correct (gate before get-or-create; denial commits only the audit row). Accept.

### F-6 LOW: licence validity is read without serialisation against a licence status change

- **Where:** `internal/operatingmarket/resolve.go:261` (`EvaluateLicenceValidity`, plain read) vs
  `internal/jurisdiction/licence_lifecycle.go:192,231` (`FOR UPDATE` then `UPDATE licences`).
- Unlike tenant/brand status (advisory-key serialised), a deposit initiation, launch, sportsbook bet or new casino
  bet can commit using the pre-suspension snapshot while a licence suspension commits. One transaction of exposure,
  nothing stranded, nothing to correct in the ledger. Accept or record as a known window.

### F-7 LOW: `rg.tenantJurisdiction` now errors on `licence_invalid`

- **Where:** `internal/rg/limits_service.go:281-305`.
- **No-licence tenant:** unchanged (`nil`, "no configuration"). So the fix does NOT newly block deposits, bets or
  withdrawals for a tenant without a licence. For an own-licence/platform tenant with no licence bound and RG
  enforcement ON, the outcome is whatever `nil` produced before (per the code comment `rg_config_missing`, fail
  closed); unchanged by lic3, not run.
- **`licence_invalid`:** tenant row not visible, or licence/jurisdiction row not readable. The FK
  (`migrations/0002...up.sql:29`) rules out a dangling licence; in the sessions that reach this function
  (`usageGuard` requires `app.tenant_id` = tenant; the function clears the player GUC first) `tenants_read` is
  `USING (true)` and `licences_read` admits the session's own licence (`migrations/0077...up.sql:104-117`). So the
  error is reachable only through a mis-scoped session or a broken database, where failing closed is correct.
- **Money paths reached:** deposits and wagers only, and only through `evaluateLimits` when RG enforcement is ON
  (`internal/rg/enforcement.go:162-207,281`). Withdrawals do NOT reach it (`EvaluateWithdrawalEligibility` ->
  `evaluateEligibility`, `internal/rg/rg.go:637-643`, reads no jurisdiction). Also reached by player
  self-protection writes (`limits_timeout.go:96`, `limits_service.go:507`, `options.go:72`) where an error refuses a
  protective action; acceptable only because the state is unreachable in a correctly scoped session. Flag to the
  `rg` owner, no change requested.

### F-8 INFO

- `payoutinstrument.maxAgeFor` (`internal/payoutinstrument/service.go:535-555`): only caller is `Verify`, which runs
  under `WithTenant` (no player GUC), so the licence is visible; no-licence still returns `nil`. The one theoretical
  change (dangling licence: was `nil`, now error) is ruled out by the FK. The helper does not judge licence status,
  so a suspended/expired licence does not block payout-instrument verification.
- KYC (`internal/kyc/enforcement.go:460-478`): withdrawal operations never call the chain
  (`evaluateEnforcementReads`, :380-381); deposit/play outcomes unchanged. The dormant signal changes no decision.
- `operatingmarket` step 1: unchanged outcomes (`licensing_unknown` for every non-resolved state).
- The test seam `WithUngatedMarketOpenForTests` now also skips the bootstrap status gate (`bootstrap.go:322`).
  Production use is pinned by a static test; the overloading means a bootstrap test built with that seam cannot
  observe the status gate. Info only.
- Demo-mode decision: agreed. Demo creates no ledger effect, but it creates a wallet (handler) and a wallet-bound
  session; gating it is consistent with "nothing is minted for a non-active brand".
- 0177 governed writers: the narrowed tables (`licence_country_ceilings`, `operating_country_policies`,
  `tenant_jurisdiction_configs`) are read, never locked, by money paths; `tenant_jurisdiction_configs` is read only
  by `launchgov` (readiness / hard checks), not by any runtime deposit, wager, withdrawal or payout path. A
  narrowing cannot strand funds; a ceiling/policy narrowing affects only the market gate (below).

## 3. In-flight rounds when a brand or tenant is suspended (ADR 0095 s40.5 / s40.6)

- The new gate is called from exactly three places: `httpserver/casino_handlers.go:227`, `casino/orchestrator.go:332`
  (`LaunchGame`), `casino/bootstrap.go:322-335`. None is on a callback posting path.
- `postWin` (`orchestrator.go:2364`): tenant gate only (`:2462`), no brand gate, no session lookup (accounts come
  from the original's ledger entries). Brand suspended: win posts. Tenant suspended: refused (pre-existing s40.5).
- `postRollback` (`orchestrator.go:2557`): stake return of a posted bet goes through `requireStakeReturnAllowed`
  (`stake_return.go:52`, `GameplayStatus`), allowed on a suspended/closed tenant (s40.6); no brand gate.
- A bootstrap denial revokes only an `active` (never consumed) session; `postBet` accepts only `consumed`
  sessions (`orchestrator.go:1858`), so a revoked-at-bootstrap session never carries a bet. Consumed sessions are
  not touched by the gate.
- Sportsbook settlement/void: unchanged (`internal/sportsbook/settlement.go:969-974`).

## 4. Financial flows when a LICENCE is suspended or expired (tenant and brand still `active`)

Licence state reaches money paths only through the operating-market resolver (`OutcomeNotPermittedByLicence`,
`internal/operatingmarket/resolve.go:266-283`) via the market gate (`marketgate` ops: registration, deposit,
launch, sportsbook wagering, casino wagering brand-level check).

**Stop (refused before any ledger write):**

1. New deposit initiation (`payments/deposit_v2.go`, `OpDeposit`).
2. Casino launch / session mint (`OpLaunch`), and launch-token bootstrap (`brandMarketOpen`, `bootstrap.go:358`).
3. New casino bet (`postBet` brand-level market check, `orchestrator.go:1915`).
4. New sportsbook bet placement (`sportsbook/orchestrator.go:169`).
5. Player registration (not a money flow; listed for completeness).

**Keep running:**

1. Withdrawal requests (no market gate, no licence read; `withdrawal.go:485` checks tenant/brand status only).
2. Withdrawal approval, payout claim and dispatch (`payments/payout.go`, status gate only).
3. Payout-instrument registration and verification (`maxAgeFor` does not judge licence status).
4. Deposit completion of already-initiated attempts (the sweeper cascade does not consult the market gate,
   `payments/orchestrator.go:371`), deposit reversals and their tombstones.
5. Casino win of an already-placed bet, casino rollback (stake return), rollback tombstones.
6. Sportsbook settlement, void and resettlement of open bets.
7. Bonus lifecycle postings (no licence-validity read found in `internal/bonus`).
8. Governed manual adjustments, withdrawal hold release, reconciliation jobs.

**If, in addition, the TENANT is suspended (a separate governed action, not automatic):** new deposits, launches,
bets, casino wins (s40.5) and sportsbook settlement postings stop; stake returns continue (s40.6); new withdrawal
requests and payout claims stop (F-2). **If only the BRAND is suspended:** new stakes, launches, bootstraps,
withdrawal requests and payout claims for that brand stop; wins and stake returns continue.

## 5. Proposed text for the orchestrator to place

1. ADR 0082, LF1 row addendum: "Launch-token bootstrap (`BootstrapLaunch`): session row `FOR NO KEY UPDATE`, then
   tenant status key SHARED, then brand status key SHARED, then L0.4. This deviates from R8 (row before advisory)
   and is admissible only while no transaction that holds a status key EXCLUSIVE (tenant or brand status change)
   locks or updates a `casino_launch_sessions` row. Any change that makes a status change touch launch sessions
   must first move bootstrap's status check before the session lock."
2. Registry: C1 (legacy risk-rule detection, launch-blocking for the first real-money tenant); F-2 decision
   (withdrawals/payouts under tenant/brand suspension); F-4 attached to Q-GP-6.

## 6. Not done / not run

- No harness database, no integration or race tests run. Section 2 F-3's no-cycle argument is by code reading.
- No change made to code, ADRs or governance files.

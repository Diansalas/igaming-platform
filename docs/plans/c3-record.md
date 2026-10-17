# C3 record: negative-balance detection (ledger-finance review condition C3, F4-R2-NEGBAL-SQL detection half)

Worktree `.claude/worktrees/c3`, branch `c3-negbal-detect`, from the integration tip `afc5c3a`
(`claude/focused-wright-jw88w9`). Migration **0174** (allocated; 0173 belongs to the bonus work item). Author: `ledger-finance`
specialist. Status labels per CLAUDE.md in section 8.

Inputs: `closure-ledger-finance-review-record.md` (lfrev worktree) section 1.3 and condition C3; `f4r1-record.md` section 5.

## 1. What I found

- C3 is real: before this change nothing reported a negative player wallet or a payout dispatched while the wallet was negative.
  The only controls were preventive (the WD050 gate at request, T1p, T2, T12; wager / launch / bonus gates).
- The application gate's definition (`internal/rg/rg.go` `walletBalanceNegative`, owner decision 13): a
  `wallet_balance_projection` row of the wallet with `account_type IN ('player_cash', 'player_locked_cash')` and
  `credit_total < debit_total`. It does **not** net withdrawal holds; it reads no `player_withdrawal_hold`.
- The withdrawal hold lives on `player_withdrawal_hold`, and `withdrawal.RequestWithdrawal` takes it only when
  `player_cash >= amount` under the L3 projection lock (`withdrawal.go`, `ErrInsufficientFunds`). The only writer of an
  UNFUNDED hold is the test fixture `wdpath.Plant` (it skips the funds check by design). So in production a hold never makes
  `player_cash` negative; "hold-only negative" is a fixture-only state.

## 2. Exact decisions

### 2.1 Predicate (a): `player_wallet_negative_balance` = the gate's predicate, holds NOT netted

The orchestrator brief suggested "net of open withdrawal holds ... as the withdrawal gate computes it". The gate does not net
holds, and netting is financially wrong, so I did not do it. The F4 agent showed the same thing in `f4r1-record.md` section 5:
deposit 100, request 100 (cash 0, hold 100), chargeback 100 gives cash -100 and hold +100. The net is 0, but paying that
withdrawal pays out money the platform no longer has. That is exactly the R1 exposure C3 exists for, and netting would hide
it. The detector therefore uses the gate's predicate exactly: same two account types, same sign rule, no netting. It evaluates
the predicate on BOTH the ledger rebuilt from `ledger_entries` (authoritative) and `wallet_balance_projection` (what the gate
reads). Either one negative is a finding, so the detector reports a superset of what the gate refuses. The two sources can only
differ under projection drift, which the same run reports as `balance_mismatch`.

Consequence for "hold-only negative (legit) -> no finding": the legitimate hold state is the FUNDED hold (cash goes to exactly
0, never below). It produces no finding, as proven by `TestC3_FundedHold_NoFinding` (two funded holds bring cash to 0, then
dispatch and completion, all clean). An UNFUNDED hold is reported. The gate refuses that state too, so the detector and the
gate agree, as proven by `TestC3_OpenHoldDoesNotNetAChargebackAway`. The pre-C3 suites that plant unfunded payouts are handled
in section 3.2.

### 2.2 Predicate (b): `payout_dispatched_while_wallet_negative`

A request in state `submitted` or `completed` (`failed`, `reversed`, `rejected` and `cancelled` did not pay out, or no longer
do) produces this finding when both of these hold:
- its wallet is negative now (finding (a));
- the negative account's balance, rebuilt from the entries with `created_at <= dispatch instant`, is already < 0.

The dispatch instant is `GREATEST(newest allowing withdrawal_gate_decisions.decided_at, attempt.first_submitted_at,
attempt.last_sent_at)`. `decided_at` is `clock_timestamp()` inside the claim, after the gate's read, so it is the latest
dispatch evidence available. The request's `updated_at` is used only when none of these exist (legacy rows). The
`withdrawal_gate_decisions` term is added only when the table exists, so the statement still runs on scratch schemas pinned below
0169. Key: the request id. One row per request (`DISTINCT ON`).

Recovery semantics, (a) and (b): both are standing and unwindowed and re-reported on every run. Both stop on the first run
after the balance is >= 0 (the receivable is collected). The open alert gets no further occurrences and staff resolve it, as for
every other standing finding. (b) is deliberately tied to current negativity, which is what "recovery clears" requires. Past runs'
rows remain as the evidence.

### 2.3 Framework placement, alerting, idempotency, RLS

- Appended to `RunLedgerVsProjection` like check D and K2 (`internal/reconciliation/negative_balance.go`), so it runs hourly
  through the existing sweep for every tenant, active or observed. There is no scheduler change and no new stream.
- ONE SQL statement (one READ COMMITTED snapshot). It is tenant-scoped by explicit `tenant_id = $1` predicates on top of RLS, and
  it FAILS CLOSED (`ErrNegativeBalanceCheckBlind`: the run errors, rolls back, `reconciliation.run_failed`) when the session is not
  exactly the run tenant's scope, or when no player-account entry is visible while projection rows show postings.
- Alerts: the existing Kind `reconciliation.ledger_projection_drift` (p1), with no alert_kinds change. There is one stable
  discriminator per finding kind and tenant: `stream:ledger_wallet_negative` and `stream:ledger_payout_while_negative`. The run id
  is an attribute only, so a persisting condition is ONE open alert with growing occurrences, never a flood.
  `raiseLedgerRunAlerts` became table-driven over the five discriminators (same order, same calls).
- Read-only: the file issues no `Exec`. The statement contains no write or DDL keyword (`TestC3_DetectorIsReadOnly`), and the
  ledger, projection, request and attempt footprint is byte-identical before and after three sweeps (`TestC3_ChargebackNegative_...`).
- Staff view: both kinds join the `ledger_vs_projection` staff vocabulary (`LedgerVsProjectionMismatchKinds`, OpenAPI
  `StaffReconciliationKind`, backoffice `KINDS_BY_STREAM`). The wallet finding's subject is `ledger_account`. The payout finding
  has no subject, because the staff contract has no `withdrawal_request` subject type (same as check D). Amounts are not exposed.

### 2.4 Migration 0174

`reconciliation_mismatches_mismatch_kind_check` = 0167's list byte for byte plus the two kinds. It is a strict superset, pinned by
`TestMigration0174_...` (every 0167 kind kept, exactly two added). The constraint name is kept. The down restores 0167's CHECK and
refuses with WD099 while a row of either kind exists (the 0167 pattern, including NO FORCE / FORCE RLS inside the transaction).
No data or other object is touched.

## 3. Changes to existing tests (explicit)

### 3.1 Fixture funded: `TestADR0113D_MultiAssetWallets_AndTheWalletKeyPath`

The test calls its two wallets "healthy", but the GBP wallet was never funded: 130 GBP planted on a 0 balance, so cash was -130.
The C3 check reported that wallet and broke the test's "reconcile clean" assertion. I funded the GBP wallet with a 130 GBP
deposit (`negbal.Deposit`) before the plants. No assertion changed. Check D's findings in that test are unaffected because they
concern hold accounts only.

### 3.2 Precise fixture filter in three pre-C3 invariant helpers

`d2AssertBalanced` (prh2_d2_merchant_crosscheck), the "LF (c) 6" helper (prh2_d2_parked_capture, used by the D2 and R8 suites)
and `bcInvariants` (prh2_r9_bound_clear, used by R9 and R16) assert "RunLedgerVsProjection reports 0 mismatches". Their worlds
plant payouts of 5000 / 900 / 700 on the fixture's 1000 deposit (unfunded holds). Following the check-D precedent
(`withoutPlantedUnlinkedHoldLegs`), I added `withoutUnfundedHoldFixtureNegatives`. It drops a C3 finding ONLY for an account whose
EVERY debit is a `withdrawal_requested` hold leg. Such an account can be negative only through an unfunded hold, which only
`wdpath.Plant` can build. A C3 finding on any account with another debit (chargeback, wager, completion) is kept, and so is every
other kind. Each helper's `len(drift) == 0` therefore still proves that every dropped finding was a fixture artifact. The filter
itself is pinned by `TestC3_FixtureFilter_KeepsRealNegatives` (a chargeback negative keeps both findings, an unfunded plant drops
both, and other kinds are kept). Every pre-existing kind is checked exactly as before, so no pre-existing assertion was weakened.
The alternative was funding `wdpath.Plant` or those worlds. That changes the ledger content those payment suites assert, and it is
the payments-recon workstream's call (as the F4 record says), so I did not do it.

## 4. Tests (tests-first note)

I wrote the implementation and the tests in the same pass, not strictly red-first. To prove the tests are non-vacuous, I ran a
mutation check: with the detector's findings not appended (`_ = negatives`), these tests FAIL:
`ChargebackNegative`, `OpenHoldDoesNotNet`, `PayoutDispatchedAfter`, `PayoutDispatchedBefore`, `RecoveryClears`,
`TenantIsolation`, `ConcurrentSweeps`, `FixtureFilter` and `Migration0174`. The two negative tests (`FundedHold`,
`ConcurrentLivePostings`) pass in both cases by design. Afterwards the file was restored byte for byte.

New (`internal/reconciliation`):
- `c3_negative_balance_integration_test.go`:
  - chargeback negative gives exactly one standing finding and one P1, and repeats add rows per run plus occurrences on the same
    alert, with the ledger untouched;
  - funded hold: no finding;
  - an open hold does not net a chargeback away, and an unfunded plant is reported like the gate;
  - payout dispatched after the negative-making posting: the payout finding, which persists after completion;
  - payout dispatched before the chargeback: only the wallet finding;
  - recovery clears (a partial recovery clears nothing, recovery to exactly 0 clears, and no new occurrence is added);
  - tenant isolation, plus fail-closed under a foreign scope;
  - concurrent sweeps: one alert, occurrences equal the recorded runs;
  - concurrent live postings that stay >= 0: no false finding over 4 s of races;
  - fixture-filter exactness.
- `c3_negative_balance_test.go` (unit): read-only statement; predicate fragments equal the gate's (and a tripwire on `rg.go`'s
  predicate); split routing; staff vocabulary; staff subject.
- `migration_0174_integration_test.go`: strict superset, up/down/up whole-schema snapshot, the check runs and stores on the
  schema, and the down refuses WD099 with a finding.
- backoffice `src/api/reconciliation.test.ts`: a new case (the C3 kinds are `ledger_vs_projection`-only and in the OpenAPI enum).
  The existing vocab-equality case covers the rest.

## 5. Commands and results

Private database `c3neg155633p7620x01` (priv_db.sh, migrations through 0174). All integration runs used `priv_test.sh -tags
integration -count=1 -p 1`.

- `go test ./internal/reconciliation/` (unit): ok. `go test ./internal/db/`: ok. `go test ./internal/alerting/`: ok.
  `go test ./internal/httpserver/ -run 'OpenAPI|ADR0114|StaffRoute|Staff'` (unit, including the OpenAPI kind-enum drift test):
  ok.
- `./internal/reconciliation/...` FULL integration, first run (before the fixture handling in section 3): 12 FAIL, all the
  unfunded-fixture shape (section 3). After the section 3 changes: **ok, 376 top-level tests + 203 subtests PASS, 0 FAIL, 0 SKIP**
  (210 s); `reconciliation/statement` 11 PASS.
- The new tests alone (`-run 'TestC3_|TestMigration0174'`): all PASS. Mutation check (section 4): the 9 positive tests FAIL with
  the detector disabled.
- External packages that run reconciliation (every test whose body or helper calls `reconciliation.Run*`, found by a scan):
  - `internal/payments` `-run '^TestK3_|^TestR3_|^(TestF2fr_WithKeysOnlyASealedDepositReversalClears|TestF3_R9_...|TestRR4_...|TestRR5_...)$'`:
    ok, 110 top-level PASS (668 s);
  - `internal/httpserver` `-run '^(TestAdmission_T6|TestCasCapRollback1_C5_ReconciliationSweepClean|TestCasinoReconciliationAdmin_|TestDrillSeedWorld|TestStaffRead|TestA10c_R2_|TestOpenAPI_ADR0114)'`:
    ok, 34 PASS;
  - `internal/adjustment` (B19 0113 up/down/up, B25, LinkFence0153): ok, 3 PASS;
  - `internal/bonus` `^TestLFCert_`: ok, 5 PASS;
  - `internal/ledger` `^TestLockOrder_`: ok, 9 PASS;
  - `internal/sportsbook` BrandGate: ok, 2 PASS;
  - `internal/wallet` `^TestReconciliationSweep_`: ok, 2 PASS.
- `./internal/db/` + `./cmd/migrate/` integration: ok (153 + 4 PASS, 1 pre-existing SKIP
  `TestVerifyEnvironmentAnchor_RealDatabase_FourStates`). `./internal/alerting/` `-run 'TestMigration0117|Static'`
  integration: ok, 34 PASS.
- Backoffice (node_modules symlinked from the main tree, not copied): `npx vitest run src/api/reconciliation.test.ts
  src/features/reconciliation`: 2 files, 16 tests PASS. `npx tsc --noEmit -p .`: clean.
- `gofmt -l`: clean. `go vet` plain and `-tags integration` on `internal/reconciliation`, `internal/httpserver`,
  `internal/db`: clean.
- Migrate on private databases with the CLI:
  - on the test database (holding C3 findings), `APP_ENV=development migrate -steps=1 down` is refused with WD099 ("0174 down
    refused: negative balance findings exist") and the version stays 174;
  - on a fresh private database `c3mig162500p30718x01`, down 1 gives version 172 and a CHECK hash different from 0174's, and up
    gives version 174 with the CHECK hash identical to the first up;
  - `migrate verify` reports only `GAP missing migration version 173`, which is expected on this branch until the bonus 0173
    merges.
  - Both private databases were dropped (as `igaming_test_admin`). No role, password, shared database or global infrastructure
    was touched.

## 6. Residuals

- R-C3-1 (attribution window): (b) attributes a chargeback by its posting transaction's START (`ledger_entries.created_at` is
  `now()` of the posting transaction). A chargeback whose transaction started after the dispatch instant, but committed before the
  claim commit or the provider send, is reported by (a) only, not by (b). The wallet is still flagged, so detection is not lost;
  only the "this payout raced the gate" attribution is. Closing it needs a commit-ordered sequence on ledger entries
  (schema change, not proposed).
- R-C3-2 (severity): both findings page as P1 through the drift Kind (an existing Kind). A chargeback-driven negative wallet
  that is already blocked by the gate may deserve P2. That would need a new alert Kind, which is an `alert_kinds` migration, and I
  did not add one here. I recommend that ledger-finance and the owner revisit this once alert routing exists (ALERT-DELIVERY-1).
- R-C3-3 (ack masking): the same caveat as the other per-tenant ledger discriminators. A NEW negative wallet in a tenant whose
  alert is already open or acked adds an occurrence and does not page again. Operators must read the latest run's rows (runbook
  4.1).
- R-C3-4: the preventive SQL clause (F4-R2-NEGBAL-SQL proper, WD052 at dispatch) stays the tracked follow-up (owner
  `payments-recon`). It still needs `wdpath.Plant` to be funded. This work is detection only.
- On this branch alone, migration 0173 is absent, so the chain has a version gap at 173. Tests that assert no unexpected migration
  gap (`ma020_parity_integration_test.go`, `alerting/migration_0117_integration_test.go`) are expected to report it until the
  bonus work item's 0173 is merged. Observed: `migrate verify` on this branch reports exactly that gap. The two Go
  gap-assertion tests passed here because they run on their own pinned scratch chains.

## 7. Proposed registry text (for the orchestrator)

- F4-R2-NEGBAL-SQL / C3: "Detection half IMPLEMENTED (c3-negbal-detect, migration 0174): standing read-only
  ledger_vs_projection findings `player_wallet_negative_balance` (the WD050 predicate exactly, holds not netted) and
  `payout_dispatched_while_wallet_negative`, P1 via `reconciliation.ledger_projection_drift`, discriminators
  `stream:ledger_wallet_negative` / `stream:ledger_payout_while_negative`. Runbook payout-reversal-and-amount-dispute.md 4.1.
  Residuals R-C3-1..4 (docs/plans/c3-record.md). The preventive SQL clause remains OPEN (owner payments-recon), needs funded
  wdpath.Plant fixtures."
- Closure review C3: "satisfied by c3-negbal-detect (ledger-finance); security review of the new read statement not claimed."

## 8. Labels

- Negative-wallet detection (a): `IMPLEMENTED`.
- Payout-while-negative detection (b): `IMPLEMENTED`, with residual R-C3-1 (attribution by transaction start).
- Migration 0174: `IMPLEMENTED`.
- Runbook / observability docs: `IMPLEMENTED`.
- Preventive SQL backstop (F4-R2-NEGBAL-SQL clause): `NOT IMPLEMENTED` (unchanged, tracked).
- `security` review of this change: not performed and not claimed.

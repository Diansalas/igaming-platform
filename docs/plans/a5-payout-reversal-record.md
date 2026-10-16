# A5 record: governed payout reversal, payout amount/asset dispute exit, negative-balance block

Author: `ledger-finance` (implementation). Branch `a5-payout-reversal`, base `1adce1c`
(`claude/focused-wright-jw88w9`, includes A6, A8 r3, A11, A12). Owner decisions 2026-10-15 (scope items 1-3; decision
13 = negative-balance policy "block until cleared"). Providers stay MOCK. Evidence is local only (private scratch
databases `a5_rev`, `a5_rev2`, `a5_mut` on the local PostgreSQL; never CI, never staging). Reviews by `security`,
`ledger-finance` (independent), `code-reviewer` and `qa` are still required before merge. Registry, HANDOVER,
progress, active-stage and the decision register are NOT edited (the orchestrator is the single writer).

## 1. Deliverables and labels

| # | Deliverable | Label |
|---|---|---|
| 1 | WITHDRAWAL-REVERSAL-1: governed payout reversal (compensating `withdrawal_reversed` posting, E11 `completed -> reversed`) | **IMPLEMENTED** against the local database / **MOCK** providers |
| 1a | Idempotency enforced by the database (three UNIQUEs, the state machine) and idempotent replay of the executing call | IMPLEMENTED |
| 1b | Tombstone rule for a reversal seen before the completion (tombstone + park + hold release) | IMPLEMENTED (MOCK) |
| 1c | Four-eyes (distinct Persons, payload digest) + policy step-up + critical route step-up | IMPLEMENTED; NOT bound to the ADR 0110 signed actor proof (residual R1) |
| 1d | Audit rows (decision, execution, tombstone, HTTP row with IP/UA/request id) | IMPLEMENTED |
| 1e | Reconciliation visibility (check D: reversed arm WD033/WD035, orphan `withdrawal_reversed` WD044) | IMPLEMENTED |
| 1f | HTTP route `POST /v1/admin/withdrawals/{id}/payout-reversal` + OpenAPI | IMPLEMENTED; back-office screen NOT IMPLEMENTED |
| 1g | Provider-initiated reversal callback (a PSP "payout returned" webhook) | NOT IMPLEMENTED (no provider event type exists; staff record the return; PROVIDER DEPENDENT) |
| 1h | Partial or different-asset return | NOT IMPLEMENTED (refused, 409; needs the partial-payout design) |
| 2 | PAYOUT-AMOUNT-DISPUTE-1: park + audit + P1 on every amount-carrying evidence path (callback, QueryStatus) | IMPLEMENTED before A5; **tested end to end in A5** (amount and asset, both paths) |
| 2a | Governed staff exit: M2 admits `amount_asset_mismatch` / `callback_amount_asset_mismatch` with basis `provider_confirmed_out_of_band` only (migration 0163) | IMPLEMENTED (MOCK) |
| 2b | Exit when the provider returned the whole payout: the A5 reversal tombstone branch | IMPLEMENTED (MOCK) |
| 2c | Exit for a partial or different-asset payout | NOT IMPLEMENTED (stays parked with hold, P1, audit; owner design) |
| 3 | Negative-balance block (owner decision 13) in the RG eligibility seam | IMPLEMENTED |
| 3a | "Cleared by a governed compensating action" | IMPLEMENTED as a derived block (lifts when the balance is >= 0: a K2 credit, tested; or a player deposit, see Q2) |

## 2. What was built

### 2.1 Migration 0162 (`migrations/0162_withdrawal_payout_reversal.{up,down}.sql`)

Next free number (0159 and 0161 stay reserved and unused; `internal/db/migrate.go` unchanged).
- `withdrawal_requests.reversal_ledger_transaction_id` (FK to `ledger_transactions`); NOT added to the shared Go column
  list (migration tests drive the Go writers on databases pinned below 0162); read by `withdrawal.ReversalLink`.
- `withdrawal_approvals`: decision `reverse`; CHECK `withdrawal_approvals_reverse_reason_shape` (`reverse` iff
  `reason_code ~ '^payout_reversal:[0-9a-f]{64}$'`); the 0026 UNIQUE (request, approver) is split into two partial
  UNIQUEs (review decisions approve/reject keep the old rule exactly; one `reverse` vote per approver).
- `ledger_transactions_one_withdrawal_reversal`: UNIQUE (tenant, reverses_transaction_id) WHERE type =
  `withdrawal_reversed` (Go sentinel `ledger.ErrWithdrawalReversalAlreadyExists`).
- Replaced in place (0132 / 0034 bodies plus marked A5 lines; down restores them byte for byte, snapshot-tested):
  - state guard: E11 `completed -> reversed`; the release link must stay the completion; the reversal link is set
    exactly once on E11 and nowhere else (WD022); **WD026**: E11 needs >= 2 distinct non-NULL Persons (linked staff,
    human decisions) on ONE `reason_code` (payload digest).
  - check A: `reversed` keeps the completion as release (WD033) and links exactly one `withdrawal_reversed`
    of the pinned shape (key `<id>:reversed`, correlation = request, reverses = the completion, provider = the
    request's, Dr `psp_clearing` / Cr the wallet's `player_cash`, amount and asset = the request's) (**WD035**);
    every other state carries no reversal link.
  - WD043 function: `withdrawal_reversed` carries NO hold leg and must, at COMMIT, be the reversal link of a
    `reversed` request that passes check A (**WD044**); the four other types keep WD043 unchanged. B-i (0132) is
    unchanged and still refuses any hold leg of `withdrawal_reversed` (WD040).
  - 0034 governance trigger: the beneficiary exclusion covers `reverse`; a `reverse` row is never automated.
- Down refuses (**WD097**) while any reversed request, `withdrawal_reversed` transaction or `reverse` decision exists.

### 2.2 Go

- `internal/withdrawal/payout_reversal.go`: `PayoutReversal` (payload, `Validate`, length-prefixed SHA-256 `Digest`),
  `RecordPayoutReversalDecision` (lock; state `completed` or `submitted`; payload matched against the request and its
  completion; eligibility (linked, active); beneficiary; policy `required = max(2, RequiredApprovals)`; policy
  `require_step_up` at ANY amount (2 min); insert the `reverse` row; count distinct Persons on the digest; audit),
  `ReverseCompletedPayout` (E11: L2 lock on the completion, the compensating posting, the transition, audit),
  `ReversalLink`, idempotent replay (`isExecutedReversalReplay`: this approver's vote on this digest, the digest at
  the floor, and the ledger carries its execution).
- `internal/payments/payout_reversal.go`: `DecidePayoutReversal` (one `alerting.InTx` transaction): the decision, and
  at the quorum either E11 (completed) or the tombstone branch (submitted: park the open attempt T10
  `reversal_tombstone_precedes_success`, E10 `withdrawal.Fail` reason `payout_returned_before_completion`, a
  `tombstone` on (provider_id, original reference) correlated to the request, audit, P1 raise LAST). Refuses an
  attempt in flight (`created`/`submitting`) and inconsistent attempt sets; requires the original reference to be
  one the platform holds (request or attempt).
- `internal/httpserver/payout_reversal_handlers.go` + route in `financial_routes.go`: tenant scope,
  `withdrawal:approve`, step-up **critical** (golden table), strict body, amount as a decimal string of minor units.
- `internal/ledger/ledger.go`: the 0162 constraint mapped to `ErrWithdrawalReversalAlreadyExists`.
- `internal/reconciliation/withdrawal_hold.go` (check D, ONE statement): the reversed arm (parity with check A) and
  `unlinked_rev_mm` (WD044, keyed `ledger_transaction:<id>`).
- `internal/rg`: `CodeWalletNegativeBalance` in `evaluateEligibility` (projection read of the wallet's `player_cash`
  / `player_locked_cash`, no lock, before L1+); applies to `EvaluateEligibility` (launch, bootstrap, bonus),
  `EvaluateWithdrawalEligibility` (WD-RG-1 gate, WD050) and `EvaluateAction` for every action except deposit.
- Migration 0163 + `internal/payments/manual_resolution.go`: M2 allow-list (`M2ResolvableDispute`,
  `PayoutDisputeReasons`, `AmountDisputeReason`), the execution re-check of the basis rule (Go mirror; the database
  is the authority, MR010).
- `internal/testsupport/negbal` (integration only): a real Flow 2 chargeback posting (Dr cash / Cr clearing) and a
  deposit, used to drive a wallet negative the way production does.

### 2.3 Migration 0163 (`migrations/0163_payout_amount_dispute_m2_admission.{up,down}.sql`)

`payment_m2_admits` (0115 body) and `payment_manual_resolutions_guard` (0125 body), each verbatim plus the two
reasons in the closed allow-list and, in the guard at submission and at `-> executing`, the A5 basis rule (MR010).
Down restores both bodies byte for byte (snapshot-tested) and refuses (**MR099**) while a pending or executed M2 of a
mismatch park exists.

## 3. Interpretations and open questions (each reversible; owner / compliance / security confirmation needed)

- **Q1 (security, HIGH before real money): signed actor proof.** `reverse` decisions live in `withdrawal_approvals`,
  which the ADR 0110 signed-actor-proof backstop does not cover (same class as ADR 0113 residual 9.3). See R1.
- **Q2 (owner): what clears the block.** Implemented as a DERIVED block: a wallet is blocked while its
  `player_cash` or `player_locked_cash` balance is negative and unblocked as soon as it is not. A governed K2 credit
  clears it (tested). A player DEPOSIT also clears it, because deposits are never blocked (decision 13 blocks
  "withdrawal, bet and deposit-less play"). If the owner wants the block to stay until a governed action even after
  a deposit, a sticky flag (table + governed clear) is needed: not built (would be a new framework).
- **Q3 (owner): scope of "play".** Blocked: casino and sportsbook wagers, casino launch and bootstrap, bonus
  eligibility. Never blocked: deposits, wins, rollbacks, settlements, voids (unchanged A6 rule).
- **Q4 (ledger-finance, decided here):** a governed reversal needs max(2, policy approvals) distinct Persons at any
  amount (ADR 0100 interim (b)); the database floor is 2.
- **Q5 (ledger-finance, decided here): M2 on mismatch parks** requires `provider_confirmed_out_of_band` for both kinds
  (supersedes the F9 "refused" classification for these two reasons only; reconciliation_exhausted is not enough
  against contradicting evidence).
- **Q6 (owner/payments): provider return events.** No PSP callback type for "payout returned" exists; staff record
  the return through the governed route. A future adapter event would feed the same decision path.

## 4. Assertions changed (none weakened)

| Test | Before | After | Why |
|---|---|---|---|
| `internal/withdrawal/adr0113_state_machine_integration_test.go` `TestADR0113_GraphMatrix_TenantSession` | `completed -> reversed` bare UPDATE: WD020 | WD022 | E11 is legal since 0162, but only with the reversal link + two-Person decisions; the bare UPDATE is refused by column discipline first |
| same file `TestADR0113_ReversedIsTerminal` | check A on a planted `reversed` (no link): WD033 | WD035 | the completion is now the legal release of `reversed`; the missing reversal link is what is wrong |
| `internal/reconciliation/adr0113_check_d_integration_test.go` (comment only) | "reversed: no shape exists (WD033)" | WD035 comment | the assertion is parity-computed, unchanged |
| `internal/httpserver/openapi_adr0114_read_routes_contract_test.go` `TestAdmissibleKinds_Table` | `amount_asset_mismatch` -> none (GAP-AAM) | M2 both (with ref) / not-paid only (no ref) | migration 0163 |
| `internal/httpserver/openapi_resolution_withdrawal_contract_test.go` | resolve doc says "No automatic, M2, M4 or hold-release path" | doc names the out-of-band M2 exit and the reversal; route count 15 -> 16 | A5 |
| `internal/httpserver/staff_route_static_test.go` | 217 routes / 134 mutating | 218 / 135 | the new route (golden class critical) |
| `internal/alerting/static_wiring_test.go` | B12 raise-site table | + `payout_reversal.go:DecidePayoutReversal` (1) | the tombstone park raises LAST |
| `internal/tenant/status_readers_static_test.go` + `docs/security/launch-status-readers.md` | allow-list | + the 0163 verbatim 0125 `'closed'` line (LF7) | verbatim restatement |
| `internal/ledger/withdrawal_types_wd043_static_test.go` | `withdrawal_reversed`: refused end to end | no hold leg; linked at commit (WD044) | the WD043 decision for the type |
| `internal/payments/manual_resolution.go` `PayoutDisputeReasons` | both mismatch reasons `false` | `true` (out-of-band basis only) | migration 0163; `TestK3_C5_C33` now iterates the remaining refused reasons |
| `internal/httpserver/staff_stepup_integration_test.go` `TestStepUp_GoldenTable_EveryClassifiedRoute` | critical 63 | critical 64 | the new critical route |
| `internal/bonus/cashback_scheduler_integration_test.go` `seedCasinoBetForCashback` (fixture, no assertion changed) | a bet posted from an unfunded wallet (cash -1000) | the stake is funded by a deposit first | a production bet never overdraws cash; a negative wallet is now blocked for bonus play (decision 13). The deposit does not enter the net-loss query |

## 5. Tests (local private databases; exact commands; counts in section 6)

```
gofmt -l . ; go vet ./... ; go vet -tags integration ./...
golangci-lint run ./...                         # 2.9.0
golangci-lint run --build-tags integration ./...
go test ./...
PRIV_DB=a5_rev  PRIV_SRC=<worktree> bash priv_test.sh -tags integration -count=1 -p 1 -timeout 3h \
  ./internal/withdrawal/... ./internal/ledger/... ./internal/reconciliation/... ./internal/rg/... ./internal/adjustment/... \
  ./internal/casino/... ./internal/sportsbook/... ./internal/launchgov/... ./internal/risk/... ./internal/wallet/... \
  ./internal/db/... ./internal/tenant/... ./internal/withdrawalgate/... ./internal/payoutinstrument/... \
  ./internal/alerting/... ./internal/bonus/... ./internal/aml/... ./internal/identity/...
PRIV_DB=a5_rev2 PRIV_SRC=<worktree> bash priv_test.sh -tags integration -count=1 -p 1 -timeout 4h \
  ./internal/payments/... ./internal/httpserver/...
PRIV_DB=a5_rev  PRIV_SRC=<worktree> bash priv_test.sh -tags integration -race -count=1 -p 1 -run 'TestA5' \
  ./internal/payments/ ./internal/withdrawal/ ./internal/reconciliation/ ./internal/rg/ ./internal/casino/ \
  ./internal/adjustment/ ./internal/httpserver/
```

A5 tests (all `//go:build integration` unless noted):
- `internal/payments/a5_payout_reversal_integration_test.go`: normal (four-eyes, exact posting shape, audit, attempt
  stays succeeded, balanced, projection = rebuild); duplicates/replays/payload binding; concurrency (8 Persons x 3
  rounds, exactly one posting); partial failure (failure after the posting rolls everything back; retry executes);
  authorization (unlinked, unknown, suspended, beneficiary, payload mismatch x6, another tenant, one Person two
  logins, missing checks); step-up policy (missing, stale, fresh; `max(2, 3)`); floor of two under a one-approval
  policy; tombstone branch (park, E10, tombstone, P1, late success posts nothing, the provider UNIQUE refuses a late
  completion on the reference); amount dispute then return (and refusal when the reference was never held); attempt
  in flight.
- `internal/payments/a5_amount_dispute_integration_test.go`: callback/poll x amount/asset park, no money moved, one
  audit, one P1; M2 x {both reasons} x {both kinds}: MR010 with `reconciliation_exhausted`, executes with
  out-of-band; 0163 down refused MR099.
- `internal/withdrawal/a5_payout_reversal_db_integration_test.go`: every E11 weakening (WD026 x4, WD022 x2, WD020,
  WD035, WD044/WD035 x3, WD044, WD040), terminal `reversed`, one reversal per completion, WD021 into `reversed`,
  WD022 reversal link on a non-terminal row, decision-row guards (23514 x2, P0001 x2, 23505), digest binding.
- `internal/withdrawal/a5_migration_0162_integration_test.go`: 0162 and 0163 up/down/up whole-schema snapshots; 0162
  down refused WD097.
- `internal/withdrawal/a5_negative_balance_integration_test.go`, `internal/casino/a5_negative_balance_integration_test.go`,
  `internal/rg/a5_negative_balance_integration_test.go`, `internal/adjustment/a5_negative_balance_cure_integration_test.go`.
- `internal/reconciliation/a5_check_d_reversal_integration_test.go`: governed reversal clean; orphan WD044 + P1;
  reversed-arm parity (WD033 x2, WD035).
- `internal/httpserver/a5_payout_reversal_http_integration_test.go`: support 403, stale step-up 403, other tenant 404,
  body actor 400, fractional amount 400, quorum over HTTP, duplicate 409, HTTP audit rows.

## 6. Counts (local private databases; never CI)

| Check | Result |
|---|---|
| `gofmt -l .` (Go) | clean |
| `go vet ./...` and `go vet -tags integration ./...` | clean |
| `golangci-lint run ./...` (2.9.0) | 0 issues |
| `golangci-lint run --build-tags integration ./...` | 682 findings (errcheck/staticcheck/unused in older test files; the baseline count at `1adce1c` was not re-measured); **0 in any file A5 added or changed** (filtered by file) |
| `go test ./...` (unit) | 67 packages ok, 0 FAIL, 8 without tests |
| Integration batch 1 (`a5_rev`, 18 package trees, `-p 1`) | 17 packages ok, 3 FAIL on the first run (withdrawal: the two changed ADR 0113 assertions; adjustment: `TestB19_Migration0113UpDownUp`, check D read the new column on a schema pinned at 0113, fixed with the `to_jsonb` read; bonus: two cashback fixtures overdrew cash, fixed by funding the stake), then withdrawal, adjustment, bonus and reconciliation re-run: 5/5 ok; 4 trees without tests |
| Integration batch 2 (`a5_rev2`) | payments ok (2738 s); httpserver 1 FAIL on the first run (`TestStepUp_GoldenTable_EveryClassifiedRoute`, the critical-route count pin 63 -> 64), fixed |
| Final batch (`a5_rev2`, after every fix) | httpserver ok (757 s); payments `-run 'Recon|recon|TestA5|K3_C4|K3_C5'` ok (170 s) |
| A5 set under `-race -json` (`a5_mut`, 7 packages) | 27 top-level PASS, 31 subtests PASS, 0 FAIL, 0 SKIP, 0 DATA RACE |
| Migrations 0162 / 0163 | up/down/up whole-schema snapshots PASS; 0162 down refused WD097, 0163 down refused MR099 (tests) |

No test was skipped by the A5 tests (the owner-plant tests skip only on the persistent shared databases; the
private databases are not on that list).

## 7. Mutation pass (ledger-critical code; kill or justify)

Drivers: `sqlmut.py` (SQL: replaces ONE function body / drops ONE constraint on the private `a5_mut` database, or for
the 0163 guard mutates the migration file the scratch M2 world migrates from; runs the targeted tests; restores) and
`gomut.py` (Go: edits ONE source line, runs the targeted tests on `a5_mut`, restores). Both live in the session
scratchpad (not committed). Killed = the targeted tests FAIL with the mutant in place. Result: **42 mutants, 42
killed** (5 initially survived; each was killed by an added test, listed). 4 further mutants are justified
equivalent (not run).

| # | Mutant | Targeted test | Result |
|---|---|---|---|
| M01 | guard: WD026 block removed | `TestA5DB_E11_GovernanceFloorAndShape` | KILLED |
| M02 | guard: WD026 counts logins, not Persons | same | KILLED |
| M03 | guard: WD026 ignores the payload digest | same | KILLED |
| M04 | guard: E11 also from `submitted` | `TestA5DB_ReversedIsTerminal_OneReversalPerCompletion` | KILLED |
| M05 | guard: reversal link free outside E11 | same (WD022 on a submitted row, added) | KILLED |
| M06 | guard: E11 may move the release link | `TestA5DB_E11...` | KILLED |
| M07 | guard: terminal exception widened to any completed update | E11 + terminal tests | KILLED |
| M08 | check A: reversal entry shape not checked | `TestA5DB_E11...` | KILLED |
| M09 | check A: reversal key not pinned | same | KILLED |
| M10 | check A: `reversed` release accepts any type | `TestA5D_ReversedArm_ParityWithCheckA` (forged completion-shaped release, added) | KILLED (survived first) |
| M11 | check A: reversal need not reverse the completion | `TestA5DB_E11...` ("reverses another transaction", added) | KILLED (survived first) |
| M12 | WD044: link-at-commit check removed | `TestA5DB_E11...` | KILLED |
| M13 | WD044: check A call removed | same | KILLED |
| M14 | WD044: `withdrawal_reversed` falls back to the hold-leg rule | same | KILLED |
| M15 | 0034 governance: beneficiary rule excludes `reverse` | `TestA5DB_ReverseDecisionRow_Guards` | KILLED |
| M16 | 0034 governance: automated `reverse` admitted | same | KILLED |
| M17 | 0163 M2 guard: mismatch basis rule removed at submission | `TestA5_AmountDispute_M2_OnlyWithOutOfBandBasis` | KILLED |
| M18 | index `ledger_transactions_one_withdrawal_reversal` dropped | `TestA5DB_ReversedIsTerminal...` | KILLED |
| M19 | CHECK `withdrawal_approvals_reverse_reason_shape` dropped | `TestA5DB_ReverseDecisionRow_Guards` | KILLED (re-run on a fresh database) |
| M20 | index `withdrawal_approvals_one_reverse_decision_per_approver` dropped | same | KILLED (re-run on a fresh database) |
| G01 | floor of two dropped (policy count only) | `TestA5_PayoutReversal_FloorOfTwoEvenWhenThePolicyAsksForOne` (added) | KILLED |
| G02 | policy step-up not enforced | `TestA5_PayoutReversal_StepUpPolicy` | KILLED |
| G03 | beneficiary check skipped (DB still refuses, wrong error) | `TestA5_PayoutReversal_AuthorizationRefusals` | KILLED |
| G04 | count ignores the payload digest | `TestA5_PayoutReversal_DuplicatesReplaysAndPayloadBinding` | KILLED |
| G05 | payload amount not matched | authorization refusals | KILLED |
| G06 | eligibility (linked) not enforced | authorization refusals | KILLED |
| G07 | replay ignores the quorum | duplicates test (evidence-only payload, added) | KILLED (survived first) |
| G08 | reversal direction swapped | normal test | KILLED |
| G09 | execution does not re-match the completion reference | `TestA5DB_RecordDecision...` (wrong original at execution, added) | KILLED (survived first) |
| G10 | tombstone branch: attempt not parked | `TestA5_PayoutReversal_BeforeCompletion_TombstoneParkRelease` | KILLED |
| G11 | tombstone on the return reference instead of the original | same | KILLED |
| G12 | tombstone branch: reference-known check dropped | `TestA5_PayoutReversal_AmountDisputeThenReturn` | KILLED |
| G13 | `submitting` attempt not refused | `TestA5_PayoutReversal_AttemptInFlight_Refused` | KILLED |
| G14 | P1 not raised on the park | tombstone test | KILLED |
| G15 | rg: deposits blocked too | `TestA5_NegativeBalance_...` | KILLED |
| G16 | rg: locked cash ignored | `TestA5_NegativeLockedCash_Blocks` | KILLED |
| G17 | rg: withdrawal not blocked | `TestA5_NegativeBalance_...` | KILLED |
| G18 | rg: zero balance treated as negative | same | KILLED |
| G19 | D: reversed release arm dropped | `TestA5D_*` | KILLED |
| G20 | D: orphan reversal check dropped | `TestA5D_*` | KILLED |
| G21 | D: reversal entry rule dropped | `TestA5D_ReversedArm_ParityWithCheckA` (bad entries, added) | KILLED (survived first) |
| G22 | M2 Go mirror: mismatch reasons not admitted | M2 amount-dispute test | KILLED |

Justified (equivalent under every reachable state; not run):
- 0163 basis rule at `-> executing` (MR010): `basis_code` is part of the resolution's immutable payload (0115
  guard), so a resolution admitted at submission always passes it; it is defence in depth.
- Go `AmountDisputeReason` execution re-check: same reason (the Go mirror of the immutable basis).
- WD044 "no hold leg" branch: B-i (0132, unchanged) refuses any hold leg of `withdrawal_reversed` first (WD040,
  tested); the branch matters only with B-i switched off.
- check A "only a reversed request carries a reversal link": the guard refuses first (WD022/WD020, M05 kills that);
  the branch matters only after an owner trigger bypass (residual R2).

## 8. Residuals

- **R1 (HIGH before real money; security).** `reverse` decisions are not bound to the ADR 0110 signed actor proof.
  A holder of the runtime credential can insert two well-shaped decisions naming two real, active, linked staff of
  the tenant and post a reversal (same class as ADR 0113 9.3 for WD023). Bounded: the payout must be `completed`,
  the amount is exactly the payout amount in its asset to the payout's own wallet, one reversal per completion, and
  a forged reversal is NOT detected by any reconciliation today (statement reconciliation does not model returns).
  Fix options: bind `reverse` rows to the 0120 proof (ADR 0110 amendment), or route reversals through K2's
  proof-bound tables.
- R2. The owner can `DISABLE TRIGGER` (ADR 0110 9.4); check D detects malformed reversed rows and orphan
  `withdrawal_reversed` transactions, not a well-shaped forged one (R1).
- R3. Partial / different-asset payouts and returns: no exit (park stays, hold kept, P1 standing).
- R4. The negative-balance gate reads the projection (runtime-writable outside the acting fence: ADR 0113 slice 2
  security C1); a forged projection is the hourly ledger_vs_projection P1. Posting paths keep their own under-lock
  funds checks.
- R5. The gate is a pre-lock read: a negative-making posting committing between the gate and the bet is not seen
  by that bet; a cash-funded bet is still refused by the under-lock funds check; a bonus-funded stake in that window
  is the residual.
- R6. A payout reversal posted for a PSP that later also reports the payout in a statement: statement
  reconciliation of returns is NOT IMPLEMENTED (PROVIDER DEPENDENT).
- R8. The database floor (WD026) proves two Persons agreed on ONE payload digest, not that the posting equals that
  payload (the evidence hash is not on the ledger); the binding of the posting to the voted payload is Go-level
  (`matchReversalPayload` at decision time, the re-check under the L2 lock at execution, G09). Same trust class as R1.
- R7. A provider return reference reused for a second reversal (or colliding with another posting) is refused by
  the (tenant, provider, provider_tx_id) UNIQUE but surfaces as a 500, not a typed 409.

## 9. Review probe lists

### 9.1 Security

1. Forge path R1: as `igaming_runtime` in a tenant session, insert two `reverse` rows (two real staff) + the posting
   + E11 in one transaction; confirm it commits; decide whether to require the signed actor proof before real money.
2. Route: `POST /v1/admin/withdrawals/{id}/payout-reversal` refuses support (403), platform_admin token (tenant
   scope), stale step-up (critical), another tenant (404), any body actor field (strict decode, 400).
3. The decider is the token subject only; `approverEligibilityCheckLocked` re-asserts role and staff_version FOR
   SHARE (ADR 0119 9.5).
4. Beneficiary: Go `ErrSelfApproval` (durable denied audit) and the 0162 trigger (`reverse` covered).
5. Two logins of one Person never satisfy WD026 or the Go count.
6. The payload digest binds every field; a different payload never counts with another.
7. The tombstone branch never credits twice: E10 releases the hold once; a later success, M2 or M4 meets a terminal
   withdrawal and the tombstone UNIQUE.
8. Audit rows carry no PII (references and the evidence hash only; no free text).
9. 0163: M2 on a mismatch park requires `provider_confirmed_out_of_band` at submission AND at `-> executing`.
10. Negative-balance gate: no new lock, no new SECURITY DEFINER, tenant-scoped read only.

### 9.2 Ledger-finance (independent)

1. Posting shape: Dr `psp_clearing` (house) / Cr `player_cash` (the request's wallet), amount = request amount, asset =
   request asset; balanced; projection = rebuild after every case.
2. Never an edit: the completion row and its entries are untouched (test asserts release link unchanged; ledger is
   append-only).
3. Idempotency: key `<id>:reversed` UNIQUE; (tenant, provider, return ref) UNIQUE; one reversal per completion
   UNIQUE; state machine; Go replay only for the executed digest.
4. Lock order: request (L1) -> attempts (L1, ascending id, submitted only) -> staff FOR SHARE -> completion (L2) ->
   L3/L4; tombstone branch posts Fail (L3/L4) before the tombstone (L4 only).
5. Invariant #12: reversal has no hold leg; WD034 terminal sum = 0 unchanged; check D aggregate unchanged.
6. The tombstone branch's accounting: the PSP paid and got the money back, so the platform never debited
   `psp_clearing`; the hold returns to cash (E10). Reconciliation reason `reversal_tombstone_precedes_success` is
   `reasonExcluded` (net zero at the PSP).
7. M2 on mismatch parks: declare paid posts the full requested amount; acceptable only on out-of-band confirmation.
8. Negative balance definition (player_cash or player_locked_cash < 0 on the wallet), deposits never blocked.
9. Mutation table (section 7): every survivor justified.

## 10. Independent ledger-finance review (2026-10-16, tip `84ac47f`, diff vs `1adce1c`)

Reviewer: `ledger-finance` (independent of the implementation). Evidence: code and migration reading (0162, 0163,
`internal/withdrawal/payout_reversal.go`, `internal/payments/payout_reversal.go`, check D, `internal/rg`, statement
reconciliation, `receipt.go`), the A5 integration set re-run on a private scratch database (`lf_a5_rev`, dropped
after): `go test -tags integration -count=1 -p 1 -run 'TestA5|TestADR0113'` over payments, withdrawal,
reconciliation, rg, casino, adjustment, httpserver: 7/7 packages ok (rg, casino, adjustment, httpserver re-run with
`-v`: every A5 test PASS, none skipped). Four scratch probe tests (NOT committed, file removed) are reported below
as P-R, P-S, P-M, P-L.

**Verdict: APPROVE for merge into the MOCK-only line, with LF-A5-1 and LF-A5-2 recorded as open rows that MUST
close before any real money (owner decision 17). Not approved for real money.**

### 10.1 Verified (no finding)

- Posting shape: `withdrawal_reversed` Dr `psp_clearing` (house, `wallet_id IS NULL`) / Cr `player_cash` (the
  request's wallet), request amount, request asset, exactly two entries, balanced; the exact mirror of the
  completion (Dr hold / Cr `psp_clearing`). Check A (WD035) pins type, key `<id>:reversed`, correlation, reversed
  completion, provider, wallet, asset, amount and entry count. The completion row and entries are never touched; the
  release link must stay the completion (WD022 on E11). Integer minor units only (`int64` in Go, decimal-string
  amount at HTTP, `NUMERIC(38,0)` in the database); no float anywhere.
- Idempotency is database-enforced: `(tenant_id, idempotency_key)` UNIQUE on `<id>:reversed`;
  `idx_ledger_transactions_tenant_provider_tx` on the return reference; `ledger_transactions_one_withdrawal_reversal`
  (one reversal per completion); the state machine (terminal `reversed`, WD020/WD021). None is check-then-insert.
- Tombstone branch accounting: the hold goes back to cash once (E10 `withdrawal.Fail`, key `<id>:failed`, WD034
  terminal sum 0); `psp_clearing` is never touched (net zero at the PSP); the tombstone sits on (provider, ORIGINAL
  reference), so a late `withdrawal_completed` is refused twice (WD020 on the failed request, and the provider
  UNIQUE, tested). M2 / M4 require `submitted` and are refused after it. The attempt is parked (T10) and the P1 is
  raised LAST.
- Commit-time checks: WD026 is in the BEFORE UPDATE guard (cannot be skipped by statement order; votes are
  append-only, `withdrawal_approvals_immutable`); WD035 / WD044 are `DEFERRABLE INITIALLY DEFERRED` constraint
  triggers (`SET CONSTRAINTS ... IMMEDIATE` only fires them earlier, which refuses the reversal INSERT; disabling
  needs the table owner or superuser: ADR 0110 9.4, R2). No SECURITY DEFINER; `search_path` pinned.
- Lock order against ADR 0082 (A7/A8/A9): L1 request -> L1 payout attempts (after the parent, ascending id) ->
  staff FOR SHARE -> L2 completion FOR UPDATE -> L3/L4 in `ledger.Post`; the tombstone `Post` has no entries (no L3)
  and comes after Fail's L3/L4; nothing takes an L0-L2 lock after a Post. The payout callback/poll paths lock the
  withdrawal first, so there is no cycle with the reversal.
- Concurrency: 8 Persons x 3 rounds (implementation test) and probe **P-R** (12 rounds: the FINAL vote races a
  success callback on a submitted payout): both interleavings occurred (11 tombstone, 1 complete-then-E11), cash
  credited exactly once in every round, ledger balanced, projection = rebuild. Probe **P-M**: a vote cast while
  `submitted` plus a vote after the completion executes E11 on the same digest correctly.
- Check D: the reversed arm is a parity copy of check A (WD033/WD035, wallet/asset/amount/entry count) and the
  orphan `withdrawal_reversed` arm (WD044) is keyed per transaction; the governed-reversal test is clean (no false
  positive on healthy data); the tombstone (no entries, no hold leg) cannot reach `unlinked_mm` or WD034.
- Down migrations: 0162 refuses (WD097) on any reversed request, reversal link, `withdrawal_reversed` or `reverse`
  row, so the restored `(request, approver)` UNIQUE cannot fail; bodies restored byte for byte (snapshot test).
  0163 refuses (MR099) on pending/executed M2 of a mismatch park. Honest.
- M2 exit (0163): admitted for both mismatch reasons only with basis `provider_confirmed_out_of_band`, at
  submission and at `-> executing` (database authority, Go mirror). The M2 itself stays the K2/K3 proof-bound,
  payload-hashed, four-eyes framework. "Declare paid" posts the request amount through `Complete` (Dr hold / Cr
  `psp_clearing`); "declare not paid" posts `Fail` (Dr hold / Cr cash). One executed resolution per attempt, the
  request must be `submitted`, so the two kinds and the A5 tombstone branch are mutually exclusive: no double count.
  Note (not a finding): "declare paid" on a mismatch park is an attestation that the provider paid the REQUESTED
  amount; a different amount has no exit (R3) and must not be declared paid.
- Negative-balance block: sign is right (`credit_total < debit_total` on the credit-normal `player_cash` /
  `player_locked_cash`); every gated path passes the wallet id (casino launch/bootstrap/wager, sportsbook wager, bonus
  `GateCheckpoint`, the WD-RG-1 withdrawal gate); `ActionDeposit` is the one exemption; it lifts at `>= 0` (tested);
  no new lock.

### 10.2 Findings

| ID | Severity | Finding | Required action |
|---|---|---|---|
| LF-A5-1 | **HIGH before real money** (MEDIUM while MOCK) | The tombstone branch releases the hold on a staff attestation that the PSP returned the payout, but nothing afterwards detects that the attestation was wrong (PSP paid, never returned = double payout). Probe **P-L**: after the tombstone branch, a late SUCCESS callback for the original reference writes **0 alert rows and 0 audit rows** (the attempt is already `disputed`, the B12 discriminator dedups). Statement reconciliation classifies `reversal_tombstone_precedes_success` as `reasonExcluded` and a succeeded payout line on a `disputed` attempt falls into the silent "already a P1" case (`payment_statement.go` `matchPayment`). The functionally equivalent M2 "declare not paid" has the standing kind `pay_declared_not_paid_but_paid`; the A5 branch has none. | Before real money: (a) a late success / succeeded statement line on a payout whose request the A5 tombstone branch failed raises a distinct P1 and writes an audit row; (b) a payout-scoped standing finding (in `payoutDisputeReasonClasses` or a K3-style standing kind keyed on the tombstone) that clears only on PSP return evidence for the original reference or a K2 recovery debit (causation = the `withdrawal_failed`). Recommended as the first A5 follow-up. |
| LF-A5-2 | **HIGH before real money** (= R1, judged) | `reverse` rows are not bound to the ADR 0110 signed actor proof. A5 makes `withdrawal_approvals` authorise a CREDIT of house money to a player (E11) and a hold release on a submitted payout (tombstone branch, a second "declare not paid" exit with weaker governance than M2: `withdrawal:approve` + critical step-up instead of the proof-bound K3 approvals and `payment_force_resolve` grant). The precedent is HSEC-APPROVED-HOLD-RELEASE-1 (0124), which proof-bound the hold-release tables for exactly this class. | **Recommendation: bind, do not route through K2, accept as an open row for the MOCK merge only.** Bind the `reverse` decisions to the 0120 proof (ADR 0110 amendment: a `withdrawal_payout_reversal:*` operation, `zz_actor_proof_guard` on `withdrawal_approvals` for `decision = 'reverse'` or a dedicated proof-bound decision table), ideally together with ADR 0113 9.3 / ADR 0110 T8 for `approve`. Routing through the K2 tables is rejected: K2 posts `manual_adjustment` / `compensating_entry` under the K2 fence and cannot drive E11 or produce the check-A-pinned `withdrawal_reversed` shape without duplicating the state machine. Must close before real money; owner authorization per ADR 0110 T8. |
| LF-A5-3 | LOW | A vote stays counted after its approver is suspended (probe **P-S**: first voter suspended, second vote EXECUTED the reversal, `counted=2`). Neither the Go count nor WD026 re-checks `status = 'active'` at execution. Same pre-existing class as `withdrawal.Approve`; K2 re-validates at execution. | Count only `active` staff in the Go quorum and in WD026 (with LF-A5-2). |
| LF-A5-4 | LOW (owner question) | The negative-balance block is per wallet (per asset), not per Person: a player negative in one fiat wallet can still bet or withdraw from another asset wallet of the same player. Decision 13 does not say which. | Owner to confirm scope (Q2b). Per Person is a one-query change in `walletBalanceNegative`. |
| LF-A5-5 | LOW | Check D has no detective twin of WD026: a well-shaped `reversed` row with no `reverse` decisions (owner `DISABLE TRIGGER`, R2) passes check D. | Add "reversed needs >= 2 distinct-Person `reverse` decisions on one digest" to check D's reversed arm. |
| LF-A5-6 | INFO | The return reference is staff-typed in the provider's `provider_tx_id` namespace. A typo equal to a future real reference of the same PSP would make that later posting fail (R7, surfaces as 500). Keeping the real return reference there is right (a future PSP return event dedups on it). | Map the provider-UNIQUE conflict to a typed 409 (R7) and log it; no namespace change. |
| LF-A5-7 | INFO | `isExecutedReversalReplay` checks the floor (2), not `max(2, policy)`. A replay posts nothing, so this is harmless. 0163 down takes no `LOCK TABLE` (0162 down does); DEV-only. | Optional tidy-up. |

### 10.3 Owner question Q2 (a deposit lifts the block): ruling

Accept as implemented (derived block, a deposit cures it). On the ledger the debt is netted on the same wallet: the
deposit credit simply brings `player_cash` back to >= 0, so no separate recovery posting is needed and nothing can
drift from the ledger. A partial deposit keeps the block, and a new negative (another chargeback) re-blocks
automatically. Decision 13 says "block until cleared", and a debt repaid in full is cleared. A sticky flag would
add a second source of truth next to the ledger for no accounting gain. If compliance/AML wants a human review of
accounts that went negative through a chargeback (a fraud signal), that belongs in the account/risk restriction
(existing governed status), not in the balance gate. Owner to confirm, together with LF-A5-4 (scope).

### 10.4 Merge conditions

None blocking for the MOCK-only merge. Before real money: LF-A5-1 and LF-A5-2 closed and re-reviewed by
`ledger-finance` and `security`; R6 (statement reconciliation of returns) remains PROVIDER DEPENDENT.

## 10. Security review (SR5, `security`, 2026-10-16, independent)

Scope: diff `1adce1c..84ac47f` (migrations 0162 / 0163, `internal/withdrawal/payout_reversal.go`,
`internal/payments/payout_reversal.go`, `internal/payments/manual_resolution.go`, `internal/httpserver/payout_reversal_handlers.go`
+ route, OpenAPI, `internal/rg` negative-balance gate), against ADR 0110, 0113, 0119 and the 9.1 probe list. Evidence:
private scratch database `sr5_a5` (local, dropped after the run; never CI). Probes were throwaway test files run once and
deleted (not committed). Re-ran: `-run 'TestA5|TestStaffRoutes|TestStepUp_GoldenTable'` over httpserver, withdrawal, rg,
casino, reconciliation, adjustment (35 PASS, 0 SKIP) and `-run TestA5 ./internal/payments/` (13 PASS). NOT in scope:
penetration testing, the back-office screen (not built), a real PSP, the identity-store threats T3/T4 of ADR 0110.

### 10.1 Verified (no finding)

- Route `POST /v1/admin/withdrawals/{id}/payout-reversal`: `staffRoute(stepUpCritical, RequireTenantScope(RequirePermission(
  PermWithdrawalApprove)))`; golden table `critical`; route freeze pins 218 / 135 and critical 64 updated; OpenAPI entry present
  (strict body `additionalProperties: false`, amount as a decimal string, evidence hash pattern). The decider is the token
  subject only; `approverEligibilityCheckLocked` re-asserts role and `staff_version` FOR SHARE (ADR 0119 9.5). No
  Idempotency-Key header: idempotency is database-enforced (three UNIQUEs + the state machine) and the executing call's retry
  is `already_executed`; acceptable for a staff decision route (a lost response on a non-executing vote returns 409, UX only).
- Four-eyes: distinct non-NULL Persons on ONE digest in Go and in WD026; the digest is length-prefixed and binds request id,
  provider, both references, amount, asset and evidence hash; `withdrawal_approvals` is append-only (`ledger_deny_mutation`
  on UPDATE/DELETE/TRUNCATE), so a vote cannot be re-pointed to another payload. The approve quorum counts
  `decision = 'approve'` only (a `reverse` vote never counts toward a payout approval).
- S-L1 analogue: staff whose Person is the withdrawing player's Person cannot decide a reversal: Go `ErrSelfApproval`
  (403 + durable Denied audit row, SR11-4 pattern) and the 0162 governance trigger (`reverse` covered).
  `player_accounts.person_id` is NOT NULL, so the comparison never degrades to "unknown".
- RLS: `withdrawal_approvals` and `withdrawal_requests` FORCE RLS, tenant-bound policies unchanged; the new column and
  indexes inherit them; runtime grants unchanged (no DELETE on `withdrawal_requests`). No new SECURITY DEFINER; the three
  0132-derived functions and both 0163 functions pin `search_path = pg_catalog, public, pg_temp` (see SR5-6 for the fourth).
- Audit: decision, execution, tombstone and HTTP rows carry ids, provider references, amount, asset, digest and evidence
  hash only; the HTTP row adds IP / user agent / request id as CLAUDE.md requires. No names, e-mails, free text or secrets.
- M2 mismatch exit (0163): admitted only with basis `provider_confirmed_out_of_band` at submission and at `-> executing`
  (MR010, Go mirror); `basis_code` is in the immutable payload; M2 keeps its ADR 0110 proof, capability grants, four-eyes
  and statement-source rule; `evidence_ref_hash` is NOT NULL for M2 (check4). The basis cannot be swapped after approval.
- Negative-balance gate: a tenant-RLS read of the wallet's own projection rows, no lock, no new SECURITY DEFINER; it cannot
  affect another tenant or another player (derived from the gated wallet only). Deposits are never blocked, so a player can
  always self-cure; making a wallet negative on purpose needs a chargeback (own deposit) or a governed K2 debit.
- Tombstone abuse from outside: no unauthenticated or player path writes this tombstone. The tombstone is written only on
  a reference the platform already holds for THIS payout (probe: a park with no bound reference is refused with
  `ErrReversalPayloadMismatch`), and the provider UNIQUE is per tenant, so no cross-tenant blocking.

### 10.2 Findings

| # | Severity | Finding |
|---|---|---|
| SR5-1 | **MEDIUM** (must close before real money) | **The tombstone branch is a weaker-governed "declare not paid".** Probe: a normal `pending` payout (provider accepted, reference bound, no dispute); two finance staff with `withdrawal:approve` record the same payload -> `tombstoned_hold_released`, player cash +500 (hold released); the genuine late success is then parked `disputed` (P1). If the PSP actually paid, the player is paid twice. M2 "declare not paid" for the same situation requires the ADR 0110 signed proof, the `payment_force_resolve` capability grants, basis `provider_confirmed_out_of_band` (L-3) and a statement source; this branch requires none of them (only `withdrawal:approve` x2, critical step-up and a staff-asserted evidence hash). The same applies to an amount/asset-mismatch park (record 2b), which now has a stronger exit (M2, 0163) and a weaker one (this branch). Fix (any one): require the `payment_force_resolve:approve` eligible grant for each decider of the submitted branch (Go + database), or route the submitted case through K3 M2 "declare not paid" (basis out-of-band) plus the tombstone. Mitigations today: four-eyes by Persons, critical step-up, audit, P1 on the park, MOCK providers only. |
| SR5-2 | **HIGH before real money / not a merge blocker** (= R1) | **Reverse decisions are not bound to the ADR 0110 signed actor proof.** Confirmed as `igaming_runtime` in a tenant session: two well-shaped `reverse` rows naming two real linked staff + `ReverseCompletedPayout` commit (state `reversed`, cash credited); no step-up, no HTTP layer, no decision or HTTP audit row is written by the forger. This adds a money-crediting flow to ADR 0110 threat-register T8 (withdrawal approvals 0026/0034, DEFERRED-REQUIRES-OWNER-AUTHORIZATION) and is undetectable by reconciliation today (R1/R6). Recommendation: do NOT route reversals through the K2 adjustment tables (K2 is wallet-adjustment semantics: it would duplicate the governance, lose the E11 / reversal-link discipline and need an architect decision). Instead, register an open row now and close it before any real-money reversal by an ADR 0110 amendment (owner authorization, as T8 requires) that adds `withdrawal_approvals` INSERT to the proof-bound set: operation `withdrawal_payout_reversal:decide`, target = request id, payload hash = the existing payout digest (already the `reason_code`), with WD026 counting only proof-bound rows; close ADR 0113 9.3 (`approve`) in the same amendment. Until closed: MOCK providers only, and the route must stay off any real-money deployment. |
| SR5-3 | LOW | **Return-reference poisoning.** `return_reference` is staff-typed and lands in the (tenant, provider_id, provider_tx_id) UNIQUE shared with every posting of that provider. Probe: reverse payout A naming payout B's provider reference as the return reference; B's genuine success then cannot post and parks `provider_reference_conflict` (hold kept, no M2 path; ops needed). A deceived second approver cannot see the collision. Needs two decisions on the same payload; no money is lost (hold kept, P1). Fix: refuse a return reference the platform already holds for any payment attempt / withdrawal / deposit of the same (tenant, provider), and map the residual UNIQUE collision to a typed 409 (closes R7 too). |
| SR5-4 | LOW | **A vote never goes stale.** Probe: staff 1 votes, is then suspended; staff 2 votes -> the reversal executes counting the suspended Person (WD026 and the Go count check Person linkage, not status). Votes also have no expiry. Fix: count only `status = 'active'` linked staff at execution (Go and WD026) and/or a vote lifetime. |
| SR5-5 | LOW (owner question) | **Negative-balance block is per wallet.** A player with a negative EUR wallet (e.g. after a chargeback) is not blocked from withdrawing or playing from another asset wallet. Owner decision 13 says "block until cleared"; confirm per wallet vs per player (Q2/Q3). Also, the gate is skipped when a caller passes `WalletID = uuid.Nil` (pre-existing contract); every current gated caller passes the wallet (verified: casino launch/bet, sportsbook bet, bonus eligibility, withdrawal gate). |
| SR5-6 | LOW / INFO | **`withdrawal_approvals_enforce_governance` has no pinned `search_path`** (the 0034 body, replaced by 0162, which states "every function pins search_path"). It is SECURITY INVOKER and resolves `staff_users` / `player_accounts` unqualified; a session able to create a temp table could shadow them (pg_temp is searched first when not listed). `igaming_runtime` has no TEMP privilege on the scratch database; production TEMP grants are not verified here. Fix: add `SET search_path = pg_catalog, public, pg_temp` to the 0162 body (down keeps the 0034 body). |
| SR5-7 | INFO (test gap) | The HTTP test does not cover a `platform_admin` token (tenant scope) nor the beneficiary 403 + Denied audit row over HTTP (both are covered by the generic scope middleware and by the Go-level test respectively). Add both to `a5_payout_reversal_http_integration_test.go`. |

### 10.3 Authorization / tenant-isolation tests the `qa` coverage must keep (security specification)

1. Another tenant's valid finance token on this route returns 404 and records nothing (present).
2. A `platform_admin` token returns 403 (tenant scope) and records nothing (SR5-7, add).
3. The paid player's own Person (staff login) returns 403 and writes one Denied audit row (add the HTTP variant).
4. Two logins of one Person never reach the quorum (present, Go and WD026).
5. After SR5-1: a decider without the force-resolve capability cannot take the submitted (tombstone) branch.
6. After SR5-2: a `reverse` row inserted without a valid proof is refused by the database (AP class).

### 10.4 Verdict

**APPROVE for merge at the current development stage (MOCK providers), with conditions**; NOT approved for any
real-money use. Conditions: SR5-1 and SR5-2 registered as open rows that MUST close before real-money payout reversals
(SR5-2 needs owner authorization per ADR 0110 T8); SR5-3, SR5-4, SR5-6 fixed or registered; SR5-5 put to the owner with
Q2/Q3. Launch-blocking for real money: SR5-1, SR5-2. This review does not declare the feature "secure": it covered the
listed code and migrations at `84ac47f` only.

## 11. Fix round (2026-10-16, after the `ledger-finance` and `security` reviews)

Author: `ledger-finance` (implementation). Base: `13b1db5` (the two review commits `1c6c0fa` and `13b1db5` are on
the branch). Code commits `178b646`, `53009bb`, `caa2fba`; this section is in the commit after them. Evidence is local only
(private scratch databases `a5_fix` and `a5_mut`, dropped after the run; never CI). Providers stay MOCK. The
section 1 labels are unchanged, except row 1c (see 11.1, item 1) and R7 (closed).

### 11.1 Items

| # | Review ID | Change | Label |
|---|---|---|---|
| 1 | SR5-1 / LF-A5-1 (governance) | The tombstone branch (request `submitted`) now needs each decider to hold an in-force `payment_force_resolve:approve` grant (`ledger_adjustment_eligible_grant`, the M2 rule: active staff, linked Person equal to the grant's request-time Person, eligible role). Go: refused at the decision (`ErrReversalCapabilityRequired`, HTTP 403); the quorum count re-checks the grant of every counted vote at execution (a vote whose grant was revoked stops counting). Database: new **WD027**, an immediate AFTER INSERT trigger on `ledger_transactions` (`ledger_transactions_payout_tombstone_guard`, pinned `search_path`, no SECURITY DEFINER): a `tombstone` whose `correlation_id` names a withdrawal request of the tenant commits only with two distinct ACTIVE Persons holding that grant on one `reverse` digest; every other tombstone (deposit, casino, sportsbook: fresh or unrelated correlation ids) exits at the correlation test, and every other transaction type exits at the first test. The decision now runs in the deciding principal's scope (`alerting.NewPrincipalRunner`, `app.principal_id`), the only tenant session in which `staff_capability_grants` is visible (0112 `tenant_scope_select`); without a principal both checks fail closed. The completed-payout reversal (E11) keeps `withdrawal:approve` x2 (it moves house money the PSP already returned; its money proof is the completion itself). | IMPLEMENTED (MOCK). Signed actor proof still NOT bound (item 7) |
| 1b | LF-A5-1 (a), late success | A success for the original reference of a payout whose request the tombstone branch failed (the attempt is `disputed` and a tombstone correlated to the request exists for that provider) now writes one audit row `payments.payout_success_after_reversal_tombstone` (OutcomeDenied, ids + reported reference only) per new observation and raises a DISTINCT P1 (`reason:success_after_payout_reversal_tombstone`, dedup per attempt + reason, LAST statement) instead of falling silent. Wired in all three success sources that meet a disputed payout attempt: the verified callback (`receipt.go`, audit once per new receipt; a redelivery only re-raises), the status poll (`applyPayoutStatusEvidenceInTx`) and the lost-CAS late-evidence router (`applyPayoutLateEvidence`). Posts nothing; the request stays `failed`; the ledger also refuses the reference (tombstone UNIQUE, now the typed `ledger.ErrProviderReferenceTaken`). | IMPLEMENTED (MOCK) |
| 1c | LF-A5-1 (b), standing finding | **NOT IMPLEMENTED, deliberately.** Check D findings are invariant P1s that must be zero on healthy data. A standing "tombstone branch awaiting PSP return evidence" finding has no platform signal that can clear it today (statement reconciliation does not model returns, R6), so it would be a permanent P1 on every legitimate tombstone branch and train operators to ignore D. It is not "cheap and consistent with check D". The detective signal is item 1b (a late success raises a distinct P1). A succeeded STATEMENT line for the tombstoned reference still lands in `payment_statement.go`'s "already a P1" case (`reversal_tombstone_precedes_success` is `reasonExcluded`): residual R9, to close with R6. | NOT IMPLEMENTED (R9) |
| 2 | SR5-3 (closes R7, LF-A5-6) | A return reference the platform already holds for the provider (a payment attempt's `provider_reference`, a withdrawal's `provider_reference` or a ledger `provider_tx_id` of the same tenant + provider) is refused before any vote is recorded: `ErrReversalReturnReferenceInUse`, HTTP 409 `conflict`. The residual race (a posting taking the reference between the vote and the execution) surfaces as the typed `ledger.ErrProviderReferenceTaken` (new: `ledger.Post` routes a 23505 on `idx_ledger_transactions_tenant_provider_tx` with no idempotency-key match to it, instead of an untyped error), mapped to the same 409. | IMPLEMENTED |
| 3 | SR5-4 / LF-A5-3 | Only `status = 'active'` staff count, in the Go quorum and in WD026 (0162 guard body). Check D's WD026 twin (item 5) deliberately does NOT re-check status (a voter suspended after a legitimate reversal must not raise a false P1). Vote expiry: not added (no Blueprint or ADR basis; the active-status and grant re-checks cover the stale-vote risk the reviews describe). | IMPLEMENTED |
| 4 | SR5-6 | `withdrawal_approvals_enforce_governance` (0162 replacement body) now ends `SET search_path = pg_catalog, public, pg_temp`; the down migration keeps the 0034 body byte for byte (snapshot test). Catalog pin: `TestA5DB_FunctionsPinSearchPath` asserts `proconfig` of the five 0162 functions. | IMPLEMENTED |
| 5 | LF-A5-5 | Check D's link arm reports **WD026** for a `reversed` request with fewer than two distinct linked Persons (non-automated `reverse` decisions) on one digest. One statement, unchanged shape; healthy data stays clean (tested with a later-suspended voter). | IMPLEMENTED |
| 6 | SR5-7 | HTTP: a `platform_admin` token gets 403 and records nothing; staff whose Person is the paid player's Person gets 403 and exactly one `denied` row of `withdrawal.payout_reversal.http`; plus a held return reference gets 409 `conflict` (item 2). | IMPLEMENTED (tests) |
| 7 | R1 / SR5-2 / LF-A5-2 | **OPEN.** Proposal in 11.3; not implemented. | NOT IMPLEMENTED (OPEN, before real money) |
| 8 | LF-A5-4 / SR5-5, Q2 | Owner questions with a recommendation in 11.4. | owner decision |

Why the grant route and not M2 routing (item 1): the grants model was practical. `ledger_adjustment_eligible_grant`
is the exact function M2 itself uses (0115) and is callable in a principal-scoped tenant session; it carries the
active-staff, Person-linkage and eligible-role checks, so the branch now needs the same capability as M2 "declare
not paid" without duplicating the K3 tables or the state machine. Routing through M2 would have needed a second
resolution kind writing a tombstone and an E10 release through the K3 executor (a cross-domain change for the
architect). What the branch still lacks compared with M2 is the ADR 0110 signed proof (item 7) and the
`provider_confirmed_out_of_band` basis (the staff-asserted evidence hash plays that role); both are recorded.

### 11.2 Changed assertions (none weakened)

| Test | Before | After | Why |
|---|---|---|---|
| `internal/payments/payout_alerts_test.go` | 6 payout signal reasons | 7 (+ `success_after_payout_reversal_tombstone`, name pinned) | item 1b |
| `internal/alerting/static_wiring_test.go`, `static_derived_raise_set_test.go` | raise-site table | + `payout_reversal.go:payoutReversalTombstoneCell:raisePayoutDisputeAlert` (1), derived set + the cell | item 1b: raise LAST |
| `internal/payments/a5_payout_reversal_integration_test.go` `TestA5_PayoutReversal_BeforeCompletion_TombstoneParkRelease`, `TestA5_PayoutReversal_AmountDisputeThenReturn` | deciders: plain finance staff | K3 world grant holders (`f1`, `f2`); `financeNoGrant` refused with `ErrReversalCapabilityRequired`; late success now asserts 1 audit row per new callback receipt (redelivery: none), +1 per poll, +1 per lost CAS, and a distinct P1 (was: no assertion, probe P-L showed 0/0) | items 1, 1b |
| `internal/httpserver/a5_payout_reversal_http_integration_test.go` | success audit count over `withdrawal.payout_reversal.http` | filtered `outcome = 'success'` | the new beneficiary `denied` row shares the action |

### 11.3 R1 / SR5-2 / LF-A5-2: proposal (OPEN, NOT IMPLEMENTED; needs owner authorization per ADR 0110 T8)

1. ADR 0110 amendment adding `withdrawal_approvals` INSERT for `decision = 'reverse'` to the proof-bound set.
   Operation name **`withdrawal_payout_reversal:decide`**; scope `tenant`; target = the withdrawal request id (text).
2. Payload hash = the existing payout digest that is already the decision's `reason_code` (`payout_reversal:<sha256>`
   over the length-prefixed request id, provider, original reference, return reference, amount, asset and evidence
   hash; `withdrawal.PayoutReversal.Digest`). No new payload format: the proof signs exactly what WD026 groups by.
3. A `zz_actor_proof_guard`-style BEFORE INSERT trigger on `withdrawal_approvals` for `decision = 'reverse'` calling
   `actor_proof_require(NEW.approver_principal_id, 'tenant', NEW.tenant_id, 'withdrawal_payout_reversal:decide',
   NEW.withdrawal_request_id::text, <digest>)` and recording the consumed proof id on the row (new nullable column,
   NOT NULL for `reverse` rows written after the amendment; earlier rows stay as historical evidence).
4. WD026 (E11) and WD027 (tombstone) count only proof-bound rows; check D's WD026 twin counts the same.
5. Go: `DecidePayoutReversal` attaches the proof in the decision transaction (as K3 does for
   `payment_force_resolve:approve`); the HTTP route already carries the step-up the proof issuance needs.
6. Close ADR 0113 9.3 (`decision = 'approve'`, WD023) in the same amendment.
Until closed: MOCK providers only; the route stays off any real-money deployment.

### 11.4 Owner questions (recommendation from `ledger-finance`; the owner decides)

- **LF-A5-4 / SR5-5 (block scope).** Recommendation: **accept per wallet (per asset).** The debt is on that wallet's
  own ledger accounts and is cured on the same wallet; a per-Person block would couple unrelated asset wallets and
  their RG/AML treatment without an accounting reason. If compliance wants a player-level restriction after a
  chargeback (a fraud signal), use the existing governed account restriction, not the balance gate. Per Person stays
  a one-query change in `walletBalanceNegative` if the owner rules otherwise.
- **Q2 (a deposit lifts the block).** Recommendation: **accept that a deposit clears the block** (section 10.3
  ruling): the deposit credit nets the debt on the same wallet, a partial deposit keeps the block, a new negative
  re-blocks, and no second source of truth sits next to the ledger.

### 11.5 Residuals after the fix round

- R1: OPEN (11.3). R7: CLOSED (item 2). R3, R4, R5, R6, R8: unchanged. R2: unchanged, now detected for a reversed
  request without decisions (item 5).
- **R9 (new, before real money).** LF-A5-1 (b): no standing finding for a tombstone branch, and a succeeded
  statement line for the tombstoned reference is still silent in statement reconciliation; closes with R6
  (statement returns) as a `payoutDisputeReasonClasses` row or a K3-style standing kind that clears on PSP return
  evidence or a K2 recovery debit (causation = the `withdrawal_failed`).
- R10 (LOW). WD027 restates the Go grant rule at the database, but the grant check reads
  `staff_capability_grants` through RLS: a session without `app.principal_id` sees no grants and fails closed
  (refuses), never open.

### 11.6 Tests and counts (private scratch databases `a5_fix` / `a5_mut`, local PostgreSQL; never CI)

Harness: `PRIV_DB=<db> PRIV_SRC=<worktree> bash priv_db.sh` (fresh database, all migrations), then
`bash priv_test.sh <go test args>`. `internal/payments` integration tests run on per-test scratch databases
migrated from the source tree (`depositV2ScratchPool`).

| Run | Command | Result |
|---|---|---|
| Static | `gofmt -l`; `go vet ./...`; `go vet -tags integration ./...` | clean |
| Lint | `golangci-lint run ./...` | 0 issues |
| Lint (integration tag) | `golangci-lint run --build-tags integration ./...` | 0 issues in any A5 / fix-round file (the 682 reported are the pre-existing errcheck / staticcheck / unused backlog in other test files) |
| Unit | `go test -count=1 ./...` | 67 packages ok, 0 FAIL |
| A5 set | `go test -tags integration -count=1 -p 1 -run 'TestA5\|TestMigration0162\|TestMigration0163'` over payments, withdrawal, reconciliation, rg, casino, adjustment, httpserver | 7/7 packages ok; 66 PASS (34 top-level), 0 FAIL, 0 SKIP |
| A5 set, race | the same with `-race` | 7/7 ok; 66 PASS (34 top-level), 0 FAIL, 0 SKIP, 0 DATA RACE |
| Batch | `go test -tags integration -count=1 -p 1 -timeout 60m` over withdrawal, reconciliation, ledger, alerting, httpserver, payments, rg, adjustment, db (production code as at `53009bb`; later commits add tests only) | 9/9 packages ok; 5376 PASS (3167 top-level), 0 FAIL, 2 SKIP (pre-existing environment skips: `TestRGEnforceS4/HeavyPlayerMeasurement`, `TestVerifyEnvironmentAnchor_RealDatabase_FourStates`) |

Two fix-round failures were found and fixed on the way. They are recorded here, not hidden. (1) The grant check
returned "no grant" for real grant holders, because the decision ran in a tenant session without
`app.principal_id`, where 0112 RLS hides the grants. Fixed by running the decision in the deciding principal's
scope (item 1; mutant G46 pins this). (2) The new check D plant could not run `ALTER TABLE ... ENABLE TRIGGER` while
deferred trigger events were pending. The plant now fires them first with `SET CONSTRAINTS ALL IMMEDIATE` (test
fixture only).

New or extended tests: `TestA5_PayoutReversal_BeforeCompletion_TombstoneParkRelease` (grant holders; non-holder
refused; late success via callback x2, status poll and lost CAS: audit rows 1/2/3, distinct P1, nothing posted;
typed `ErrProviderReferenceTaken`), `TestA5_PayoutReversal_AmountDisputeThenReturn` (grant holders),
`TestA5_PayoutReversal_TombstoneVoterGrantRevoked_NoLongerCounts`, `TestA5_PayoutReversal_WD027_OneGrantHolderIsNotAQuorum`,
`TestA5_PayoutReversal_ReturnReferenceAlreadyHeld_Refused` (references held only by an attempt, only by the ledger,
and by a withdrawal + attempt), `TestA5_PayoutReversal_SuspendedVoterNoLongerCounts`, `TestA5DB_E11` (+ "one suspended
after voting" -> WD026), `TestA5DB_WD027_PayoutTombstoneNeedsForceResolveGrantHolders`, `TestA5DB_FunctionsPinSearchPath`,
`TestA5D_ReversedWithoutDecisions_IsWD026`, `TestA5HTTP_PayoutReversal_AuthorizationAndQuorum` (+ platform_admin 403,
beneficiary 403 + one Denied row, held reference 409).

### 11.7 Mutation pass (fix-round code only; kill or justify)

Driver: one edit per mutant, targeted tests on `a5_mut`, then restore. SQL mutants replace the live function body,
or, for tests in `internal/payments`, the migration source the scratch databases are built from, and then restore
it.

| Mutant | Result |
|---|---|
| S01 WD027 disabled | KILLED (`TestA5DB_WD027`) |
| S02 WD027 grant condition dropped | KILLED (`TestA5DB_WD027`) |
| S03 WD027 active-staff filter dropped | SURVIVED, **equivalent**: `ledger_adjustment_eligible_grant` itself requires `s.status = 'active'`; the filter is kept as an explicit restatement |
| S04 WD027 fires on every tombstone | KILLED (the deposit-style tombstone in `TestA5DB_WD027`) |
| S05 WD027 threshold 1 | KILLED (`TestA5_PayoutReversal_WD027_OneGrantHolderIsNotAQuorum`) |
| S06 WD026 active-staff filter dropped | KILLED (`TestA5DB_E11`) |
| S07 governance function search_path pin removed | KILLED (`TestA5DB_FunctionsPinSearchPath`) |
| G30 Go quorum counts suspended voters | KILLED |
| G31 Go decider grant check skipped | KILLED |
| G32 Go quorum ignores grant revocation | KILLED |
| G33 return-reference check disabled | KILLED |
| G34 return reference: payment_attempts leg dropped | KILLED |
| G35 return reference: ledger leg dropped | KILLED |
| G36 ledger: provider-reference conflict untyped | KILLED |
| G37 / G38 / G39 callback / status-poll / lost-CAS hook removed | KILLED (each) |
| G40 late-success audit not written | KILLED |
| G41 late-success audit on every redelivery | KILLED |
| G42 late success raises the park reason (not distinct) | KILLED |
| G43 tombstone lookup ignores the provider | SURVIVED, **equivalent in practice**: only the A5 branch writes a tombstone whose correlation id is a withdrawal request, and always with that request's provider (WD027 + the Go path); the provider filter is a narrowing restatement |
| G44 check D WD026 arm disabled | KILLED |
| G45 check D WD026 re-checks staff status (false P1) | KILLED |
| G46 decision without the principal scope | KILLED |
| G47 HTTP 409 mapping removed | KILLED |

Not mutated: the `withdrawal_requests` leg of the return-reference check. Every withdrawal `provider_reference` is
first bound on its payout attempt, so the attempt leg already covers it; the leg is kept as a restatement.

### 11.8 Review probe additions

- Security: (a) as `igaming_runtime` in a principal-scoped tenant session, insert two `reverse` rows for two grant
  holders + a tombstone correlated to the request: expected to commit (R1, the proof is still not bound; not probed
  in this round); without the grants
  -> WD027; without `app.principal_id` -> WD027 (fails closed). (b) The 409 refusal leaks only that the reference is
  held for that provider, to an authenticated finance user of the same tenant. (c) The HTTP route now opens a
  principal-scoped session for the token subject; check that nothing else in the decision depends on the absence of
  `app.principal_id`.
- Ledger-finance: (a) the WD027 trigger fast-exits on every non-tombstone and every tombstone whose correlation id is
  not a withdrawal request of the tenant (casino, sportsbook and deposit tombstones are unaffected; the full batch is
  green). (b) `ErrProviderReferenceTaken` is returned only when the key lookup finds no row (a true replay still
  returns `AlreadyPosted`). (c) The late-success cell posts nothing and changes no state.

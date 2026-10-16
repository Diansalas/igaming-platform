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

## 6. Counts

(filled in below after the runs)

## 7. Mutation pass (ledger-critical code; kill or justify)

(filled in below)

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

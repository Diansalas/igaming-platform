# Ledger-finance independent review: final provider-independent closure (76187cc..2daf352)

Reviewer: `ledger-finance` (independent; reviewed only, fixed nothing). Date 2026-10-17. Worktree `.claude/worktrees/lfrev`,
branch `lfrev-closure`, based on the integration tip `2daf352` (claude/focused-wright-jw88w9). Migration head reviewed: 0172
(0171 F4, 0172 SRF2R). All providers MOCK. Software capability only; no regulatory or licensing position is claimed.

Scope: financial invariants of every commit in `76187cc..2daf352` (append-only double-entry, no balance UPDATE, idempotency by
database constraint, no double credit / double grant / double dispatch, reconciliation integrity, compensating entries only),
with the six questions of the review brief.

## 0. Verdict

**APPROVE WITH CONDITIONS** for the provider-independent freeze.

No commit in the range adds a ledger posting path, mutates a historical ledger row, updates a balance, introduces floating point
on a server money path, or weakens an idempotency constraint. One latent defect was found and proven (section 4, NA-4
FirstDepositOnly double grant); it has no monetary effect today (every deposit-bonus grant is cancelled at the jurisdiction
gate, HDR-J-1/HDR-J-3), so it does not block the freeze, but it is a binding condition before any deposit bonus can activate.

Conditions (binding, in this order):

| # | Condition | Binding before |
|---|---|---|
| C1 | NA-4 FirstDepositOnly double grant (section 4): apply the smallest fix in 4.3 (application check) and, in the next migration window, the DB backstop | any FirstDepositOnly offer can produce an ACTIVE grant (i.e. before HDR-J-1/HDR-J-3 unblock deposit-bonus activation), and in any case before production launch |
| C2 | MA020 statement-only reversal (section 2.1): at PSP onboarding, prove the PSP can re-deliver (or let us re-query through the verified receipt path) a deposit-reversal notification after the fact; if it cannot, a governed tombstone path must be designed (ADR, migration, four-eyes, sealed-statement-bound) | first real PSP go-live (PROVIDER DEPENDENT) |
| C3 | F4-R2-NEGBAL-SQL detection gap (section 1.3): the record's compensating controls do not include a negative-balance detective control; add a scheduled read-only detection (negative `player_cash`/`player_locked_cash` wallets, and payouts submitted after a negative-making posting) to the existing tracked follow-up | production launch (follow-up owner `payments-recon`, review `ledger-finance`) |

Registry note for the orchestrator: C1 is a new defect row (proposed id `BONUS-FIRST-DEPOSIT-DOUBLE-GRANT-1`, severity MEDIUM
latent, `NOT IMPLEMENTED`). C2 and C3 extend existing rows (SRF2R-3 accepted cost; F4-R2-NEGBAL-SQL).

## 1. F4 signed dispatch decision (f4r1-record.md, migration 0171)

Read: `migrations/0171_*.up.sql` (WD053 `withdrawal_dispatch_decision_ok`, the proof guard), `internal/withdrawal/dispatchgate.go`,
`dispatchproof.go`, `internal/payments/payout.go` (T1p `ClaimForDispatch`), `payout_sweep.go` (T2 `reclaimPayoutCreated`, T12
`resubmitPayoutAmbiguous`, `dispatchPayoutAttempt`), `payout_withdrawal_gate.go`.

### 1.1 Double dispatch: none introduced

- The proof is signed and consumed INSIDE the claim transaction (nonce UNIQUE, consumed in the same transaction, rolled back with
  it). It adds a refusal; it adds no transition, no retry loop and no provider call.
- T1p: one `approved -> submitted` per request is still the state machine's own guarantee (WD020-WD025); a rolled-back claim
  leaves the request `approved` with no attempt. T2: `ClaimCreatedForSubmission` CAS with a fresh claim token and lease, attempt
  `created` = never sent (INV-IO-9 CHECK). T12: poll first, IdempotentSubmission manifest and `max_resubmits` CAS unchanged.
- The provider call sites remain the two pinned ones (`b13b_static_test.go`); both run only after the application's own committed
  claim. A replayed proof is single-use (AP005) and only for the exact row it names (target, attempt, stage, outcome, figures).

### 1.2 Dispatch without the gate / hold released wrongly: none

- WD053 now requires an allowing row WITH `actor_proof_nonce` recorded in the same transaction and no denying row; historic
  0169 rows (NOT VALID CHECK) are irrelevant because WD053 is restricted to `decided_xact = pg_current_xact_id()`.
- Every new failure path is fail-closed and leaves the hold in place: `ErrDispatchProofUnavailable` wraps `ErrGateUnavailable`;
  T1p maps it to `ErrPayoutWithdrawalGateUnavailable` (whole claim rolled back, request `approved`, hold untouched); T2/T12 roll
  the tick back (attempt stays `created` / `ambiguous`, nothing sent, nothing released). A denial at T2/T12 escalates (T16: no
  resend, no release), unchanged. Any other `Attach` error is returned and also rolls the claim back.
- The SF4-5 alert is raised detached after the rollback; the process-local counter has no financial effect.
- Liveness note (not a money defect): a missing signing key stalls every payout claim (503 at T1p, silent retry at T2/T12 with the
  p2 alert after 3 ticks). Production start already refuses without `ACTOR_PROOF_KEYS` (`VerifyConfiguredInProduction`).

### 1.3 Accepting F4-R2-NEGBAL-SQL as subsumed: financially sound, with condition C3

Window: `rg.walletBalanceNegative` is read by the gate evaluation BEFORE the L1 lock, without a lock (ADR 0082 R8), READ
COMMITTED. A reversal/chargeback posting that commits between that read and the claim commit is not seen; the claim transaction
contains no provider I/O, so the window is the claim's own duration (milliseconds, bounded by lock waits).

Exposure per occurrence: at most `min(withdrawal amount, negative shortfall)`. The paid amount is the player's own HELD funds
(`player_locked_cash`, posted and balanced at request time); the loss is that the platform pays out while it holds a receivable
on `player_cash`. It is not a double payment and not a ledger imbalance (the ledger stays balanced; the receivable is a negative
projection, visible in the ledger).

Why the SQL clause would not materially change this: the same exposure exists, unclosable by any dispatch-time check, between the
claim commit and the post-commit provider `Withdraw`, and between the send and the PSP's settlement. The SQL clause would shorten
only the in-claim part (milliseconds) of a window that is otherwise seconds to days long. With the signed allow, the application's
negative-balance refusal can no longer be bypassed by a forged row, which was the real reason for the SQL backstop.

Compensating controls (exact, as they exist today):
- Next gated action refuses: T2/T12 re-dispatch, new withdrawal requests (WD050), wagers, casino launch, bonus eligibility
  (runbook `payout-reversal-and-amount-dispute.md` section 4).
- Reversal postings are verified-callback ledger postings (append-only, keyed) and are reconciled against the PSP statement by
  the payment-statement stream; the hourly ledger-vs-projection drift job keeps the projection the gate reads honest.
- NOT existing (the record should not be read as claiming them): there is no reconciliation finding, alert Kind or report for a
  negative player wallet, and no finding for "payout dispatched while the wallet was negative" (grep of `internal/` and the alert
  Kind list). Force-resolution (M2) is a payout state resolution, not a clawback. Recovery of the receivable is a K2 governed
  credit/deposit or an operational collection process. Hence condition C3 (detection only, read-only, no ledger effect).

Verdict for question (1): signed allow plus WD052/WD053 leave no path to double dispatch, ungated dispatch or wrong hold release
that I could find. Accepting F4-R2-NEGBAL-SQL as subsumed: ACCEPTED (financially sound) with C3.

## 2. SRF2R (srf2r-record.md, migration 0172)

### 2.1 `payment_ref_cleared` tombstone-only: correct direction; operability trap exists, remedy insufficient alone (C2)

- Before 0172 a runtime-writable, SQL-unverifiable statement line could open MA020 and admit a manual credit while the captured
  deposit could still post: a double-credit path. Removing it is required by the "no double credit" invariant. 0172 changes a
  STABLE read function only; no ledger table, posting path or historical row is touched. APPROVED.
- What MA020 blocks: manual CREDITS (K2 adjustments) to a player with a `disputed` deposit attempt in the R-MA-1 reason set whose
  reference is not tombstoned. Debits, deposits, withdrawals, wagers and wins are not blocked; the K2 compensation of an executed
  M2 declare-paid stays exempt. So no player balance is frozen and no money is stranded inside the ledger.
- The trap: the ONLY tombstone writer for a deposit reversal is the verified callback (`receiveCallbackViaReceiptPath ->
  ApplyReceiptEvidence -> applyReversalReceiptEvidence -> postDepositReversalTombstone`); M1 never changes a disputed deposit and
  M2 is payout-only. If the reversal callback is lost and the PSP cannot re-deliver it, every manual credit to that player
  (including compensation owed for an unrelated incident, and a K2 credit meant to lift a negative-balance block) is refused
  forever. The runbook remedy (ask the PSP to re-deliver) depends on a PSP capability that is not guaranteed and is often
  time-limited. Fail-safe (no double credit) but not always recoverable: hence C2. Not freeze-blocking: no real PSP exists, the
  capability is PROVIDER DEPENDENT.
- Visibility: after 0172 the Go detective finding can clear while SQL stays open (SQL-cleared is a subset of Go-cleared, pinned).
  The operator learns of the block only at the MA020 refusal (explicit code plus runbook paragraph). Acceptable.

### 2.2 Cursor budget / `cursor_flood`: cannot silently skip statement lines

- The cursor only moves to the `coverage_end` of the newest run whose RUN seal and IMPORT seal both verify. Budget exhaustion
  returns `ErrPaymentCursorScanBudgetExceeded` before any cursor is trusted: phase 0 fetches nothing; the in-match re-read rolls
  the match back (no run committed, cursor unchanged). There is no partial or advanced cursor on any budget path; the budget check
  is after a full batch, so a short final batch ends the read normally with whatever verified cursor (or none = zero = re-cover
  from the start, which re-matches more, never fewer, lines).
- Statement lines skipped would require the cursor to advance past unmatched lines; no path in the diff can do that. A standing
  flood stops reconciliation of that pair LOUDLY (P1 `reconciliation.run_failed` phase `cursor_flood` every run) until the
  owner-authorised exclusion procedure runs. APPROVED.
- `ServerCursor`: rows of each batch are fully read and closed before `runImportVerifies` runs on the same transaction; the cursor
  is closed on every exit (defer plus explicit close). The cursor name is a fixed identifier (regexp), not input.

### 2.3 `match_stalled` new semantics: cannot hide a real stall (production)

- An orphan import is excluded only if a run of THIS source with a VERIFYING run seal committed after the orphan's `fetched_at`.
  A real stall (no match commits) produces no such run, so every orphan fetched after the last genuine progress run is counted
  and raises after 30 min. An orphan excluded by a later progress run does not lose lines: the progress run proves the stream
  advances from the trusted cursor, and later imports re-fetch from that cursor.
- Budget hit in the progress read: nothing counts as progress, the stall is raised (fail closed). A continuously held advisory
  lock still trips after the 15 s wait (`lock_still_held=true`); the check never takes the lock.
- Residual (non-production only): without keys a runtime-SQL actor can plant a run row backed by an import of the source and
  suppress the stall (A11 rule). Production cannot run without keys (`VerifyPaymentCursorSealing`); SRF2-6 (staging keys) stays.

### 2.4 M2 gate refusal reasons

`evidence_scan_budget_exceeded` is a closed reason under the existing 409 `force_resolve_precondition_failed`, logged at Error,
refused at both submission and execution. Narrowing the Request pre-read to the attempt's provider can only refuse (a provider not
read is never current). Decide still reads all providers, each bounded. A flood can deny M2 (availability), never admit it.
APPROVED.

## 3. NA-2 deposit mismatch P1 (raise only)

`raiseDepositSignalAlert` is the last statement of the two mismatched-success cells of an already-terminal deposit; the cell
returns the same `ResolutionAnomalyOther` disposition as before; no state change, no posting, no audit change (the audit row is
still once per new receipt). Closed reason set, provider_id-only attributes. Side effect to note: a TRANSIENT alert failure now
rolls the receipt delivery back (redelivery retries), same as payout R-5/R-6; when the cell runs inside a transaction that also
posts (deferred receipts), that posting rolls back atomically and is retried idempotently: no partial posting, no double posting.
No ledger effect. APPROVED.

## 4. NA-4 bonus deposit sweep bounded re-scan

### 4.1 Double grant under overlapping sweeps: impossible for one deposit and offer version

- Unique key `(tenant_id, campaign_id, offer_version_id, player_account_id, trigger_reference)` (migration 0057) is DB-enforced.
- The re-scan excludes, in SQL, any deposit that already has a grant for ANY offer (`NOT EXISTS ... trigger_reference`), which
  also covers an offer re-versioned between two sequential ticks.
- Production ticks are serialised per tenant by `pg_try_advisory_xact_lock` (`schedulers.go`); the sweep is the only caller of
  `IssueAndActivateDepositBonus`. The only theoretical one-deposit double grant is two UNLOCKED concurrent sweeps that see two
  different offer versions of the same offer; the advisory lock excludes it. Each tick is one transaction (partial failure rolls
  back grants and watermark together).

### 4.2 Late earlier deposit also qualifying as first deposit: DEFECT (latent), proven

Mechanism: `isFirstDeposit(D)` = no committed deposit of the player with `(posted_at, id)` below D's. Deposit B (later
`posted_at`) commits first and is swept as "first" (A is invisible); A (earlier `posted_at`, committed late) is then picked up by
the re-scan, and nothing below A's key exists, so A is also "first". Different `trigger_reference`, so the unique key admits both.
Result: two FirstDepositOnly grants for one player and one offer. Before NA-4 A was silently skipped (the player got one grant,
on the "wrong" deposit), so NA-4 converts a fairness defect into a double-grant defect.

Proof (temporary probe test, run once on a private database, then deleted, not committed): FirstDepositOnly offer, a player with
no prior deposit, A started first and held open, B committed, sweep, A committed, sweep. Result: `grants for player = 2 (A=1,
B=1)`, both for the same offer. Today both rows are `cancelled` (the jurisdiction gate denies every deposit-bonus activation,
HDR-J-1/HDR-J-3), so the monetary exposure today is zero. Once activation is unblocked the exposure is one extra first-deposit
bonus per occurrence (bounded by the offer's `cap_amount` and `MaxQualifying`; bonus funds are wagering-locked, so cash-out
exposure is the post-wagering conversion). Player influence: low but non-zero (two concurrent deposits on two rails, timing
the slow one across a sweep tick); load makes it more likely without player intent.

Verdict: DEFECT, severity MEDIUM latent; not freeze-blocking because it cannot credit value today; binding condition C1.

### 4.3 Smallest safe fix (not implemented)

1. Application (no migration, sufficient for the single serialised writer): in `matchesDepositEligibility`, when
   `ov.FirstDepositOnly`, also return false if the player already holds any `bonus_grants` row for the same `offer_id` (any
   version, any status including cancelled):
   `SELECT EXISTS (SELECT 1 FROM bonus_grants WHERE tenant_id = $1 AND player_account_id = $2 AND offer_id = $3)`.
   Runs under the per-tenant advisory lock in the same transaction as the insert, so it is race-free for the sweep. Test: the
   probe scenario above must yield exactly one grant, plus the existing NA-4 tests unchanged.
   (Including cancelled rows is deliberate: a first-deposit offer is evaluated once per player; a later re-evaluation after the
   jurisdiction gate opens must not grant on the second deposit. If product wants re-evaluation of cancelled grants, that is a
   bonus-engine rule decision, not a ledger one.)
2. DB backstop (next migration window; CLAUDE.md requires idempotency enforced by the database): a first-deposit marker on the
   grant plus a partial unique index `(tenant_id, offer_id, player_account_id) WHERE first_deposit_only`. Not addable now (only
   the F4 agent may add a migration in this closure).

Other NA-4 residuals (accepted as recorded): a deposit transaction open longer than 15 min is outside the guarantee; ineligible
deposits are re-evaluated each tick for 15 min (repeat signal audit rows; retroactive grant if an offer turns active in that
window, same retroactivity as the backlog path).

## 5. U-5 frontend exact money vs int64 JSON-number fields

- Responses: `parseJsonExact` converts any integer literal outside +/-(2^53-1) to an exact digit string before `JSON.parse`;
  the fast path (`/\d{16}/`) is correct (2^53 has 16 digits). Display is BigInt-exact. `amount.ts` is byte-identical in both apps
  (checked with `cmp`).
- Requests: `toWireInteger` refuses (null) beyond 2^53-1, never rounds. Server-side enforcement is unchanged (all balance and
  limit checks are server-side).
- Fiat cannot reach 2^53 in practice: the seeded fiat assets (EUR, USD, GBP, BRL, MXN) all have exponent 2, so 2^53-1 minor units
  = 90 071 992 547 409.91 currency units (about 90 trillion) per single request. Even a high-nominal currency added later as a
  configuration row (for example ARS, COP, CLP exponent 0, IRR) stays above hundreds of millions of USD equivalent per request,
  orders of magnitude above any deposit, stake or withdrawal limit. CONFIRMED.
- Crypto residual (deferred with crypto, exactly): BTC (exponent 8) is safe (2^53 sat is about 90 million BTC, above supply);
  USDT (exponent 6) is safe up to about 9 billion USDT. The residual is exponent-18 assets (ETH / ERC-20 style): 2^53 wei is about
  0.009 units, so such amounts cannot be sent from the UI. The same assets also overflow the Go `int64` DTO and ledger-input
  fields at about 9.22 units, i.e. the binding fix is a backend one (decimal-string DTOs and `NUMERIC(38,0)` end to end, as
  CLAUDE.md already requires when crypto is in scope), not a frontend one. Label: `NOT IMPLEMENTED` (deferred with crypto).

## 6. NA-3 casino fail-closed: no ledger effect

`HealthStatus` error now fails the LAUNCH closed (`health_unavailable`); the jurisdiction denial writes one audit row in the
launch transaction. Launch precedes any wager; no wallet call, no posting, no balance read on a money path is added; the bet,
win and rollback paths are untouched. APPROVED.

## 7. Other commits in range (financial scan)

NA-1 (panic values never logged), NA-5/NA-6 (environment anchor / acceptance wiring), U-6/U-7 (test-support route gate, staff
mutation audit proof, bonus suggestion claim audit), jurisdiction TODO closure (comments, pins, config-only tests), regscan:
no ledger, wallet, balance or idempotency change. The bonus suggestion claim gains an audit write only.

## 8. Accepted residuals (exposure, detection, mitigation)

| # | Residual | Exposure | Detection | Mitigation |
|---|---|---|---|---|
| R1 | F4-R2-NEGBAL-SQL: negative-making posting between gate read and claim commit | per occurrence at most min(withdrawal amount, negative shortfall); ms window, same order as the unclosable commit-to-send window | today none dedicated (C3); visible in the ledger projection; next gated action refuses | C3 detection job; SQL clause stays the tracked follow-up once `Plant` fixtures are funded |
| R2 | ADR 0110 T6: holder of the application signing key can sign an allow | any approved request could be dispatched without a real evaluation | AP001..AP005 in the DB log only for forgeries; none for a key holder | out of scope by ADR 0110; key in secret store, production start check |
| R3 | Approval rows not proof-bound (ADR 0113 s9.3) | a runtime-SQL actor can forge `approved`; still cannot call the provider | none dedicated | ADR 0113 s9.3 (open) |
| R4 | MA020 statement-only reversal | no money moves; manual credits to that player blocked until a tombstone exists | MA020 refusal code; Go standing finding clears | C2 at PSP onboarding |
| R5 | `cursor_flood` stops a pair until owner-authorised exclusion | reconciliation of that pair paused (no false clearing) | P1 every run | runbook exclusion procedure (6.11) |
| R6 | Non-production `match_stalled` forgeable without keys | none in production | n/a | production requires keys; SRF2-6 staging keys |
| R7 | NA-4: deposit transaction open > 15 min not re-scanned | one missed bonus grant (player-unfavourable, no over-credit) | none | deposit transactions are short |
| R8 | U-5: exponent-18 crypto amounts not representable | none today (no 18-exponent asset seeded) | n/a | backend string DTOs + NUMERIC(38,0) when crypto is scheduled |

## 9. Commands

- Worktree: `git worktree add .claude/worktrees/lfrev -b lfrev-closure claude/focused-wright-jw88w9` (tip 2daf352).
- Private database `lfrev154222p27506x01` (`priv_db.sh`, migrations through 0172), then
  `priv_test.sh -tags integration -count=1 -p 1 ./internal/bonus/ -run 'TestLFRevProbe|TestRunDepositSweepForTenant_LateCommitting' -v`:
  ok; existing NA-4 test PASS; probe logged `grants for player2 = 2 (A=1, B=1)`, both `cancelled`, same offer. Probe file deleted,
  not committed. Database dropped (as `igaming_test_admin`). No role, password, shared database or global infrastructure touched.
- No other test run (the orchestrator's full verification run was using the database concurrently). No code changed.

## 10. Labels

- This review: `IMPLEMENTED` (review record only).
- Fix for C1: `NOT IMPLEMENTED` (proposed in 4.3). C2: `PROVIDER DEPENDENT`. C3: `NOT IMPLEMENTED` (tracked with F4-R2-NEGBAL-SQL).
- `security` sign-off on 0171/0172 is separate and not claimed here.

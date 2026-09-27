# RV-PRH-I3 — Ledger-finance code review of the ADR 0096 KYC enforcement implementation

**Reviewer:** `ledger-finance`. **Date:** 2026-09-27. **Branch:** `claude/focused-wright-jw88w9`,
`HEAD d4a0e08`. **Commits reviewed:** `69f603f`, `9dc362d`, `2858ac2`, `b3e1e85`.

**Measured against:**
- ADR 0096 §12.2 (C1–C7);
- `rv-0096-ledger-reverify.md` (N1–N3, plus N4/N6/N8);
- ADR 0082 §2.1/§2.2 (R8 plus Amendment A7 rule N1).

This is a static review. The code was not edited.

## Verdict: **SIGN-OFF WITH CONDITIONS** (no veto)

No veto applies to the code as written:
- no floating point (the threshold uses `big.Int`, and `SUM(amount)::text` over `NUMERIC(38,0)`);
- no historical ledger mutation;
- no direct balance `UPDATE`;
- every new posting carries an idempotency key (`requestID:kyc_denied`).

The financial logic of `RequestWithdrawal` and `DenyForCompliance` is correct by inspection.

The sign-off does **not** extend to the ADR 0096 §15 label "IMPLEMENTED" for
`withdrawal.DenyForCompliance` or for the withdrawal-request deny path. The C7 test suite that
ledger-finance required is almost entirely absent (LF-I3-1). Until LF-I3-1 and LF-I3-2 are
closed, those two items are **PARTIALLY IMPLEMENTED**. `DenyForCompliance` also has no caller
(PRH-I1 owns T1p), so it is not yet a live money path. That is why this is conditional and not
a REJECT.

**The tests were NOT run by this reviewer.** The environment's permission policy blocked both
of these:
- obtaining the local Postgres credentials (the Makefile default password is rejected);
- the subsequent `go build` / `go test -race` invocation.

No pass/fail result is claimed here. The §15.3 CI-timing and pass claims are the implementer's
own and were not reproduced. See LF-I3-6.

## Condition-by-condition

| Item | Status | Basis (code) |
|---|---|---|
| **C1 / N2**: lookup → mismatch → gate → insert | **CLOSED (code); test gap** | `withdrawal.go` `RequestWithdrawal`: a read-only `getByTenantPlayerIdempotencyKey` runs first. A hit goes through the wallet/asset/amount mismatch check (`ErrIdempotencyKeyReused`) and never returns the row blindly. A non-`ErrNotFound` error fails. Then `EvaluateEnforcement`, then `IdempotentInsert`. The post-insert conflict branch is unchanged, with the same mismatch check, so the concurrent same-key race still falls to the DB unique constraint. On deny, nothing is inserted or posted. On allow, `RecordDecision` runs in the same tx as the hold. A denied key leaves no row, so a retry after a pass creates a fresh request, which is correct because nothing was posted. |
| **C1**: "handler commits the deny" | **DEVIATION — see LF-I3-3** | The implementation returns `*KYCDeniedError`. That **rolls back** the request tx, and the handler then writes the decision in a **fresh** `WithTenant` tx. The domain effect is zero either way: the rolled-back tx held only reads (`GetPlayerAccountByID`, `wallet.GetByPlayerAndAsset`), the gate's plain SELECTs and the pre-lookup. So there is no financial harm. However, it is not the signed shape, and the deviation is not listed in §15.2. |
| **"No `requested` row without a hold"** | **HOLDS** | The row INSERT, `LockProjectionsForPosting`, the sufficiency check, `Post` and `hold_ledger_transaction_id` are all in one tx. Any error, including `ErrInsufficientFunds`, rolls everything back. The gate runs before the INSERT. |
| **C2 / N3**: `DenyForCompliance` shape | **CLOSED (code); untested** | `lockRequestForUpdate` (L1) → `state == approved` guard → `GetOrCreateAccounts(hold, cash)`. The posting debits `player_withdrawal_hold` and credits `player_cash` for `wr.Amount`, with `TxWithdrawalRejected`, `ReversesTransactionID = wr.HoldLedgerTransactionID`, `CorrelationID = requestID` and key `requestID:kyc_denied`. `Post` takes its own L3 after L1. Then a single conditional `UPDATE … SET state='rejected', release_ledger_transaction_id=$2 … WHERE id=$3 AND state='approved'`, where `RowsAffected()==0` → `ErrStateConflict`, which rolls back the posting. There is no `ReasonCode` on the ledger tx. The audit is `ActorSystem` / `withdrawal.rejected_kyc` with outcome, `policy_version`, hold and release tx ids. There is no `withdrawal_approvals` row. The decision row is in the same tx (the N1 carve-out is honoured). |
| **C2**: exactly-once under the row lock | **HOLDS by construction** | Only `DenyForCompliance` and `LockApprovedForSubmission` leave `approved`. `Reject` is from `pending_review`, `Cancel` from `requested`, `Fail` from `submitted`. Under the L1 `FOR UPDATE`, the second arrival reads a non-`approved` state and returns `ErrStateConflict` **before** posting. A repeated deny is also blocked by the ledger key. A concurrent `Reject` vs `DenyForCompliance` on one request cannot both release, because they have disjoint source states. **No test demonstrates any of this** (LF-I3-1). |
| **C2**: state-machine amendment | **OPEN** (LF-I3-2) | `docs/architecture/withdrawal-state-machine.md` still has no `approved → rejected` (KYC) edge. |
| **C3**: no pay-out on known failed status | **CLOSED** | `evaluateWithdrawalStructuralRule` requires the latest per-account row to be passed and unexpired, with no history exemption. It adds the deny-only cross-account `rejected` overlay per Person. A DB error returns `unavailable`, never `passed`. |
| **C4**: TOCTOU bounded | **CLOSED** | The gate is computed inside the gated tx, and no decision is accepted from the caller. The pre-lookup replay does not re-gate. That is deliberate and correct: the hold already exists, and a later revocation is caught at payout. |
| **C5**: ADR 0095 fit | **CLOSED for this scope** | `DenyForCompliance` is legal only from `approved`, and the doc comment routes T2/T12 denials to ADR 0095 M3. The T1p call site is deferred to PRH-I1 and disclosed. |
| **C6**: cumulative deposit arithmetic | **CLOSED** | `sumSettledDeposits` sums `ledger_entries.amount` (`NUMERIC(38,0)`) as `::text` into `big.Int`. It counts credit entries to `player_cash` on `transaction_type='deposit'` (matching `payments/orchestrator.go:776`, which credits `player_cash`), restricted to `a.asset_code = $3` and the player's wallets. There is no `deposit_intents.amount` and no cross-asset sum. When no policy row matches the asset, the result is `unavailable` (fail closed). The comparison `cumulative + amount >= threshold` uses `big.Int.Cmp`. Gross (un-netted) and excluding in-flight deposits is the conservative direction and is disclosed under HD-KYC-1. Note: no deposit call site exists yet (PRH-I1), and only the dormant case is tested (LF-I3-1). |
| **Play gates**: placement | **CLOSED** | In casino `postBet` and sportsbook `PlaceBet` the order is RG → Risk → KYC, before `GetOrCreateAccounts` / `LockProjectionsForPosting`. A deny returns the existing declined/rejected shape with a nil error, so the decision row commits with no posting. |
| **A7-N1**: does the KYC read take an L0.x advisory? | **NO — compliant** | `EvaluateEnforcement` issues only plain SELECTs: `tenants.licence_id`, `kyc_enforcement_policies`, `kyc_verifications`, and the ledger sum. There are no `FOR UPDATE`/`FOR SHARE` clauses and no `pg_advisory*` calls. Under A7-N1 a lock-free gate may run after L1, so the position after RG (L0.4) and Risk (L0.5) is fine. The only lock-relevant write is `RecordDecision`'s INSERT on a deny or a non-dormant allow. Its FK (`player_accounts (id, tenant_id)`) takes `FOR KEY SHARE`, the same as every existing `audit`/domain insert. No path takes `FOR UPDATE` on `player_accounts`, so no new wait edge is introduced. |
| **C7**: tests | **OPEN** (LF-I3-1) | See below. |

## Conditions (must close before PRH-I3 is marked IMPLEMENTED without qualification)

**LF-I3-1 (blocking the label): the C7 test suite is missing.** No test in the repo references
any of these: `DenyForCompliance`, `KYCDeniedError`, `ErrKYCUnavailable`,
`EnforcementCasinoPlay`, `EnforcementSportsbookPlay` or `RejectionKYCDenied`. The existing
`TestRequestWithdrawal_IsIdempotentOnRetry` seeds an approved verification, so it would still
pass if the pre-lookup were deleted. It does not prove "replay does not re-gate". The following
are required, as integration tests under `-race`:
1. **Withdrawal-request deny, through the handler.** No `withdrawal_requests` row. Zero
   `ledger_transactions` for the correlation. The `kyc_enforcement_decisions` row and the
   `kyc.enforcement_denied` audit are committed. The same key retried after the verification
   passes creates a fresh request and hold.
2. **Replay after a revocation.** Hold succeeds, then a `rejected` verification is inserted, then
   the same key is replayed. The test asserts the original row is returned and that no new
   decision row is written (this proves the pre-lookup ran before the gate). Also assert that a
   mismatched amount on a replay returns `ErrIdempotencyKeyReused`.
3. **`DenyForCompliance` happy path**, called in the same tx as `LockApprovedForSubmission`.
   Assert:
   - exactly one `withdrawal_rejected` with `reverses_transaction_id = hold` and key
     `…:kyc_denied`;
   - `state='rejected'`;
   - `release_ledger_transaction_id` equals that tx;
   - the audit and decision rows are present;
   - `player_cash` is restored and the hold is 0;
   - **`SUM(debits)==SUM(credits)`** tenant-wide;
   - **projection == recomputed-from-ledger**, with zero drift from `internal/reconciliation`.
4. **Exactly-once under concurrency.** N goroutines run `DenyForCompliance` on one `approved`
   request, and `DenyForCompliance` races `LockApprovedForSubmission` → `MarkSubmitted`. Exactly
   one release (or zero, if submit wins). The losers get `ErrStateConflict` with no posting. Run
   the same invariants as in item 3.
5. **Wrong-state refusal.** `DenyForCompliance` from `requested`, `pending_review`, `submitted`
   and `rejected` returns `ErrStateConflict` and posts nothing. This is the C5 "never release
   after possible dispatch" guard.
6. **Cumulative-deposit threshold with an active policy.** Cover below, at and above the
   threshold. A deposit in another asset must not count. A `deposit_intents` row with no
   posting must not count. A value above `int64` range must be exact. An asset with no active
   policy row returns `unavailable`.
7. **Casino and sportsbook play-deny with an active `play` policy.** No ledger transaction. The
   balance and projection are unchanged. The decision row is committed. The ADR 0082 lock-order
   harness passes unmodified with the gate active, not dormant.

**LF-I3-2: `withdrawal-state-machine.md` amendment.** Add the `approved → rejected` edge
(system actor, `DenyForCompliance`, releases the hold via `withdrawal_rejected`, legal only
pre-dispatch). This was an implementation gate under C2.

**LF-I3-3: the C1 commit-shape deviation must be disclosed and bounded.** The rollback-then-
fresh-tx shape is financially acceptable, because the rolled-back tx provably holds no writes.
It still differs from the signed C1 text and it has two consequences:
- (a) It is correct only while every statement before `RequestWithdrawal` in the handler closure
  is a read. Add a comment at the call site, or a test, that fails if a write is ever added
  there.
- (b) If the second tx fails, the player still gets a 409 "denied" while the decision and audit
  rows are lost, with only a log line to show for it. Whether that is acceptable belongs to
  `security` (condition 5 durability). Returning a 5xx on `recErr` would keep "no denial
  response without a durable record".

Record the deviation in ADR 0096 §15.2.

**LF-I3-4 (non-blocking, recommended, carried from C2): DB backstop.** Add a partial unique
index `(tenant_id, reverses_transaction_id) WHERE transaction_type IN
('withdrawal_rejected','withdrawal_failed')`, mirroring migration 0092. It is not added, and the
register entry should be confirmed.

**LF-I3-5 (informational).**
- For casino and sportsbook, an `unavailable` outcome caused by a real SQL error leaves the
  Postgres tx aborted. The subsequent `RecordDecision` then fails, and the callback or bet
  returns an error rather than a committed decline. This is still fail-closed (nothing is
  posted), but the decision row is not recorded. The withdrawal path is unaffected because it
  records in a fresh tx.
- N8 still stands: `EnforcementParams.Amount int64` cannot represent crypto amounts above about
  9.22 units at exponent 18.

**LF-I3-6: evidence.** The implementer or `qa` must attach an `-race` integration run of
`internal/withdrawal`, `internal/kyc`, `internal/casino`, `internal/sportsbook` and
`internal/httpserver` that includes the LF-I3-1 tests, under
`docs/plans/payment-readiness/evidence/`. This reviewer could not execute it (see the verdict).

## Labels

- `EvaluateEnforcement`, the withdrawal-request gate and the play gates: financially correct by
  inspection, **PARTIALLY IMPLEMENTED** until LF-I3-1 items 1, 2, 6 and 7 pass.
- `DenyForCompliance`: financially correct by inspection, **PARTIALLY IMPLEMENTED** (untested, no
  caller) until LF-I3-1 items 3–5 and LF-I3-2 are done.
- The deposit gate and the payout T1p call site: **NOT IMPLEMENTED** (PRH-I1), as §15 already
  says.

# Ledger-finance review — PRH-2 plan (2026-09-28)

Reviewer: `ledger-finance`, read-only, plan at `a3547f5`. Every file:line below was read at that commit; items marked "inferred" come from reading, not execution.

**Verdict: APPROVE WITH CONDITIONS.** The overall shape is sound:
- K1 → K2 → K3 ordering.
- `ledger.Post` is the only way into the ledger.
- Idempotency is enforced by the database.
- Policies are effective-dated and append-only, with no seeded threshold; an absent policy means the operation is disabled.
- The payments lane is strictly serial.
- 0114's down migration restores the 0107 guard.

Several plan statements are factually wrong, however, and four money paths are under-specified. The **HIGH** findings are binding: for ADR 0100/0101 **before K2/K3 code starts**, and for C, D and I-wire **inside those workstreams**.

| ID | Sev | Plan § | Evidence | Required change |
|---|---|---|---|---|
| LF-1 | HIGH | §4 0114, §5-K3 | No `failed` state exists (0101:94-97). There is no transition out of `disputed` (0107:171-193). `→succeeded` and payout `→declined` require `last_evidence_kind ∈ {sync, callback, query_status}` (0107:259-264). | **Ruling.** (a) **Deposits never leave `disputed`.** M1 is a `payment_manual_resolutions` row on a still-disputed attempt; there is no `resolved_no_action` state and no state-CHECK change; the deposit branches of the guard and both 0107 indexes stay byte-identical. (b) **Payouts only:** `{ambiguous, disputed}` → `succeeded` ("declare paid") or `→ declined` ("declare not paid"), with `last_evidence_kind='operator'`. Admitted only with an approved resolution for that attempt and that target state in the same transaction (0105 `decided_txid = txid_current()` pattern, 0105:243-247,324). UNIQUE per attempt; every other operator-evidence terminal stays refused. |
| LF-2 | HIGH | §5-K3 M1 | The 0107 ledger backstop covers only `transaction_type='deposit'` (0107:84-86). A K2 `manual_adjustment` crediting `player_cash` for a `multiple_success_for_intent` capture would pass every backstop and double-credit. | **Ruling:** no M1 credit of any type for a financially resolved intent (HD-LEDGER-UNALLOC-1, not reopened). The funds leave only via a PSP refund (reversal → tombstone) or LEDGER-SUSPENSE-B-1 later. M1 is evidence-only in PRH-2. An M1 credit for an unresolved intent is out of scope; if ever built, it goes through `postDepositSuccess` as a `deposit` posting. The K2 reason-code catalogue has no deposit-allocation code, and the K3 executor refuses to link a K2 transaction to a deposit resolution. |
| LF-3 | HIGH | §4 0114 | `pay_captured_unposted` clears only on a reversal, a tombstone or an allocation (§28.9). | An M1 resolution on `multiple_success_for_intent` must not clear or suppress it; it may only annotate the finding as acknowledged, which keeps reporting and ageing. Test required. |
| LF-4 | HIGH | §5-D | `sweeper.go:513` posts using `res.ProviderReference` as `provider_tx_id` and idempotency key. The tombstone check is skipped when the echo is empty. `ApplySuccess` keeps the bound reference (`attempt.go:615-627`). `ledger.Post` accepts an empty `ProviderTxID` (`lockorder.go:346`). Inferred: an empty echo posts under key `"<provider>:"`, skips the tombstone check, and collides tenant-wide. | D: (1) the tombstone check and posting key always derive from `*attempt.ProviderReference`; (2) a differing non-empty echo → T10 `poll_reference_mismatch`; (3) an empty echo is allowed, using the bound reference. Defence in depth: `ledger.Post` rejects an empty `ProviderTxID` with `ErrInvalidEntry`, after confirming by grep that no caller relies on `""`. |
| LF-5 | HIGH | §5-C (sync half) | `DepositResult` has no `Amount` or `AssetCode` (`types.go:471-495`). A sync success (`drive.go:260-264`) posts `attempt.Amount` with no provider evidence. `postDepositSuccess`'s intent comparison (`orchestrator.go:1108`) is tautological; the poll path is the same. | C adds the echoed `Amount` and `AssetCode` to the sync-success contract. Missing → `ErrorClassAmbiguous` (the poll decides); mismatch → T10 `sync_amount_mismatch`, no posting. C and D share one comparison helper. |
| LF-6 | MEDIUM | §5-C binding | The reference index is per tenant (0101:131), and RLS hides other tenants. For a same-tenant bound reference, posting happens before `ApplySuccess`, so a collision surfaces as `ErrCallbackPayloadMismatch` (`orchestrator.go:1175-1179`) or a unique violation: an error loop, not T10. | Add an in-tx pre-check under the intent lock, before posting, that routes to T10 `provider_reference_conflict`; it covers deposit and payout attempts in the same tenant. Drop the cross-tenant clause (no cross-tenant read). |
| LF-7 | HIGH | §5-I I-wire, §1-I | `RecordDepositMultipleSuccessRefusal`'s only caller is `InitiateDepositAudited` (`orchestrator.go:684`), which E2 deletes, so the `:1509` site dies. The live P1 is `auditMultipleSuccessForIntent` (`:989-1024`), inside the T10/T13d evidence transaction, which must commit with its receipt (LF95-C3, §28.4). The inventory also misses the drift P1 (`reconciliation/scheduler.go:333-341`) and `payments_deposit_intent_index_backstop_fired` (`:1021`). | (1) `alerting.Raise` inside a financial transaction runs under a savepoint; a failure is logged and swallowed and never aborts T10, T13d or a posting. (2) A detached `Raise` (fresh transaction) is used for failure-path P1s whose business transaction rolls back (§28.8); "a rolled-back event raises no alert" applies to event alerts only. (3) Add the drift and backstop sites to I-wire. (4) Correct the plan's stale premise. |
| LF-8 | MEDIUM | §5-H | The advisory xact lock lasts one transaction, while a pass spans A, B (no transaction) and C. The row lease uses a constant owner, `SweeperBatchLeaseOwner` (`sweeper.go:74,169-208`); `RunSchedulerLoop` uses no advisory lock. | Exactly-once rests on the row lease plus SKIP LOCKED, the claim-token CAS, ledger idempotency and the 0107 indexes; the advisory lock is only an efficiency hint. Tests: lease expiry mid-pass → one posting and one payout dispatch; a mutant with the advisory lock removed still passes. |
| LF-9 | HIGH | §5-K2 | "HR-9 still applies" is false: HR-9 was removed (`bonus_mirror.go:26-33`, `ledger.go:26-29`). A BONUS_SET posting auto-generates mirror legs, and HR-17 forbids hand-built ones. | ADR 0100 defines a closed posting-shape catalogue, enforced in the executor and by a CHECK on the request table. PRH-2 allows only the counter-account `manual_adjustment` and, on the player side, `player_cash`. Refused: `player_withdrawal_hold`, `player_locked_*`, `player_bonus*`, `psp_*`, `provider_payable`, `promo_liability`, `bonus_expense`, `jackpot_contribution`. Bonus corrections go through bonus-engine. |
| LF-10 | HIGH | §4 0112, §5-K2 | Execution is bound only to "a same-tx `ledger_transaction_id`". `ledger.Post` inserts under a savepoint, so an `xmin` check would compare a subtransaction xid. | The payload (wallet, account type, asset, direction, amount, reason code) is immutable after submission, and each approval pins a payload hash (the 0063 `p_payload_match` precedent). The executor posts exactly that payload. A trigger verifies the linked transaction is a `manual_adjustment` in the same tenant (composite FK `ledger_transactions_id_tenant_key`, 0021:51), UNIQUE, with entries equal to the payload. "Same transaction" uses `decided_txid`, never `xmin`. |
| LF-11 | MEDIUM | §5-K2 | ADR 0098 §2 requires an "independent authorized actor"; two principals for one Person are not independent. | Remove `distinct_principal`. A distinct, non-NULL `person_id` (0029:18) is a non-configurable floor whenever four-eyes applies. |
| LF-12 | MEDIUM | §5-K2 | Minor units are per-asset. `EntryInput.Amount` is int64 (`ledger.go:259`); the column is NUMERIC(38,0) (0022:18). | CHECK: `above_threshold` requires a non-null `asset_code`. Refuse amounts above int64 max at submission, so an approved request can always execute. |
| LF-13 | MEDIUM | §5-K2 | `ledger.Post` has no generic sufficiency check; the sanctioned pattern is `LockProjectionsForPosting` (`lockorder.go:103-124`). | **Ruling:** a debit adjustment may not drive a player-owned account negative. Check with `LockProjectionsForPosting` in the execution transaction; if insufficient, the request ends terminal with nothing posted. Execute in the final approval's transaction, so there is no approved-but-unexecuted window. Add an ADR 0082 amendment placing the request/resolution row lock before L3. |
| LF-14 | MEDIUM | §5-K2 | When an approval stops counting is undefined. | An approval counts only if the approver's grant is in force at execution (checked by timestamp then, not by a sweeper). `idempotency_key = 'manual_adjustment:'||request_id` (UNIQUE `(tenant_id, idempotency_key)`, 0021:43); `correlation_id` = the request id; `causation_id` = the compensated transaction, if any. The reason code comes from a closed catalogue and is copied to `ledger_transactions.reason_code` (0051:72, `lockorder.go:355`). |
| LF-15 | MEDIUM | §5-K3 M2 | `withdrawal.Complete` requires a non-empty `providerTxID` (`withdrawal.go:1408-1411`). `Complete` and `Fail` require the `submitted` state (`:1417`, `:1508`). | ADR 0101: (1) "declare paid" uses a reserved provider-tx namespace that `providerref.Validate` refuses for real references, so it cannot collide; (2) M2 only when the withdrawal is `submitted` (T14 disputes excluded); (3) record the double-payout risk of "declare not paid" followed by a late real success (→ T14 P1). |
| LF-16 | LOW | §1-E2 | There are 20 legacy call sites in 10 files, including `migration_0107_integration_test.go:1867`; the plan says 19 in 9. The legacy path is the only way `ErrDepositAlreadyPostedForIntent` is reached. | Mutant parity keeps that sentinel killed through a direct `ledger.Post` fixture. Delete `RecordDepositMultipleSuccessRefusal` with the chain. Keep `RecordDepositReversalRejection`, which has a live caller (`deposit_handlers.go:484`). |
| LF-17 | LOW | §1-K | `lockorder.go:355` is the `reason_code` validation, not a lock-order rule. | Fix the citation. |
| LF-18 | LOW | §5-K3 | The 0107 project rule: guard tests run on a HEAD-migrated scratch DB (§28.8(3)). | Apply it to 0114; keep 0114's down refusing while resolutions exist. |
| LF-19 | INFO | §3, §4 | 0108–0114 are gap-free in merge order, and the payments lane order is consistent. | — |
| LF-20 | INFO | §5-F | The savepoint approach for the KYC outage path. | Accepted, provided the `unavailable` path commits only the decision and audit rows: no `withdrawal_requests` row, no ledger posting (LF-I3-3). |
| LF-21 | INFO | Principle | No jurisdiction rule is hard-coded. The jurisdiction floor is precedence mechanics; LF-11 and LF-13 are ledger-correctness and human-decided floors. | — |

## Required tests and mutants

**C**
- Sync success with a missing amount → ambiguous, no posting.
- Sync success with a mismatched amount or asset → T10, no posting.
- A same-tenant reference already bound to another deposit or payout attempt → T10, not an error loop.
- An empty reference on sync success → the unchanged ambiguous path, not parked.
- MUT: drop the sync amount compare; drop the binding pre-check.
- The ledger stays balanced, and no T10 produces a ledger transaction.

**D**
- Empty echo → posted under the attempt's reference, with the tombstone checked on the attempt's reference.
- Differing echo → T10 `poll_reference_mismatch`.
- Amount higher, amount lower, or asset different → T10.
- Tombstone on the bound reference with an empty echo → T10.
- Poll mismatch racing a matching callback, `-count=50` → exactly one terminal state and at most one deposit posting per intent.
- MUT: post with `res.ProviderReference`; skip the tombstone on empty; drop each compare.

**E2**
- Parity for every INV-DEP-1, A7 and reconciliation mutant, including `ErrDepositAlreadyPostedForIntent` via a direct ledger fixture.
- The static no-provider-call-in-tx test.

**H**
- Two loops against one DB.
- Lease expiry mid-pass → one posting, one payout dispatch.
- Crash between B and C → recovered.
- Kill switch engaged → no dispatch.
- Per-tenant failure isolation.
- Full `-race -tags integration`.
- MUT: removing the advisory lock still leaves exactly-once; removing the claim-token CAS must fail the tests.

**I**
- Alert insert failure injected inside T10/T13d → the dispute and receipt commit, and the response is still a uniform 200.
- A failure-path P1 whose business transaction rolls back → the detached alert persists.
- Dedup under N concurrent raisers.
- Drift P1 wired.
- MUT: `Raise` without a savepoint → a test fails.

**K2**
- Payload changed after approval → refused.
- Self-approval, same Person under two principals, and an unlinked Person → refused.
- Approver revoked between approval and execution → the approval no longer counts.
- Policy tightened in between → an extra approval is required.
- Expired → refused.
- Execute twice or concurrently → one ledger transaction.
- Debit beyond balance → refused, nothing posted.
- BONUS_SET, hold or locked account shapes → refused.
- Reason code outside the catalogue → refused.
- `above_threshold` without an asset → CHECK refusal.
- No in-force policy → disabled.
- Property: SUM(D) = SUM(C), and projection = recomputed.
- MUT: drop the payload hash, the distinct-Person check, the in-tx grant read, the executed↔transaction match, or the sufficiency check.

**K3**
- Deposit `disputed` → any state is refused, even with an approved resolution.
- M1 on `multiple_success_for_intent` leaves `pay_captured_unposted` reporting and posts nothing.
- A K2 credit linked to a deposit resolution → refused.
- Payout declare-paid and declare-not-paid, only when the withdrawal is `submitted`.
- A resolution for X used on Y, a target-state mismatch, or reuse → refused.
- Late real success after "declare not paid" → T14 P1.
- Late callback after "declare paid" → no second Step B.
- Guard tests at HEAD; 0114 up/down/up; down refuses while resolutions exist.
- MUT: drop `decided_txid`; drop the attempt binding; allow a deposit out of `disputed`.

## Corrections to the human-decision list

- **HD-PRH2-1:**
  - Also ask whether `above_threshold` has a ceiling; without one, (b) is (a) in disguise.
  - Distinct-Person is not a human question; ADR 0098 §2 already decided it (LF-11).
- **HD-PRH2-3:** agree that no value is needed before implementation.
  - `above_threshold` rows are per-asset.
  - Fixtures use synthetic, test-only values; no migration seeds a policy row.
- **Not a new decision:** an M1 credit on a resolved intent. LF rules no (LF-2), within HD-LEDGER-UNALLOC-1.
- **HD-PRH2-6** gates whether platform grants may post into a tenant ledger operating under its own licence. K2/K3 fail closed (platform scope refused on tenant ledgers) until it is answered.

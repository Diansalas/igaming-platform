# PAY-DOUBLE-CREDIT-1 — reconciliation and fix plan (read-only analysis)

Status: **OPEN, HIGH (financial integrity)**. No fix implemented. Implementation is
halted pending human approval of this plan (orchestrator STOP, 2026-09-27).

Analysis base: `origin/claude/focused-wright-jw88w9` @ `0eea950` (read-only code
inspection by the orchestrator). The reproducer is the payout/A7 agent's test
`TestA7_1a_SweeperClaimVsCallbackPhaseC_SameDepositIntent` (kept outside Git, in the session scratchpad).

## 1. Mechanism (exact)

| Question | Answer (code) |
|---|---|
| Intent creation | `InitiateDepositAttempt` (`deposit_v2.go`) inserts the `deposit_intents` row and attempt 1 (T1+T2, `InsertSubmittingAttempt`). |
| Original attempt | A `payment_attempts` row (`operation='deposit'`, `attempt_no=1`, own `provider_id`, `provider_reference`, `external_idempotency_key`, `merchant_reference`). |
| Decline | T8 `ApplyDecline` (attempt → `declined`) plus `finalizeDeclined` (intent projection → `declined` unless it is already `succeeded`). |
| Fallback trigger | `cascadeEligible` → `insertCascadeAttemptIfEligible` (`cascade.go`) inserts attempt N+1 in `created`. It is triggered from the callback path (`receipt.go`), phase C (`drive.go`) and the sweeper (`sweeper.go`). |
| Fallback attempt | A new `payment_attempts` row on the same `deposit_intent_id`, a different provider, and its own references and idempotency key. It is claimed by T2 (`ClaimCreatedForSubmission`) and dispatched by the sweeper. |
| Linking | Only through `deposit_intent_id`. The index `payment_attempts_one_live_per_intent` allows at most one live attempt (`created/submitting/pending/ambiguous`) per intent. |
| Provider references / idempotency | Per attempt: unique `(tenant, provider_id, provider_reference)`. The ledger idempotency key is `providerID:providerReference`, plus the unique `(tenant, provider_id, provider_tx_id)` on ledger transactions. **Nothing is unique per intent.** |
| Late callbacks | `ApplyReceiptEvidence` → `applyResolvedReceiptEvidence` resolves the **attempt** by `(verified provider, provider_reference)`. It does not resolve the intent. |
| Where the credit happens | `postDepositSuccess` (`orchestrator.go:841`): `ledger.Post` debits `psp_clearing` and credits `player_cash`. It is reached from `receipt.go` (T7 at line 588 and T13 at line 560), `drive.go:268` (phase C sync success), `sweeper.go:381` (poll success) and the legacy `orchestrator.go:757`. |
| Transition permitting the 2nd credit | **T13** (`declined → succeeded`, deposit only). It posts even when a sibling attempt already succeeded. This is by design: ADR 0095 §4.3 T13 plus the ledger-finance **LF-Q1** ruling (§21.2) treat it as a "second capture", post it to `player_cash` and raise P1 `multiple_success_for_intent`. **T7** (`submitting/pending/ambiguous → succeeded`) also posts without checking whether the intent is already `succeeded`; `postDepositSuccess` then takes its "T13 second capture" branch and writes `deposit.second_capture_posted`. |

### Reproduction orders

All of these are sequential. **No concurrency is needed.**

- **Fallback first, original late:**
  1. A1 declines, and A2 is created and claimed.
  2. A2 succeeds (T7, credit #1; intent → `succeeded`).
  3. A1's late success arrives: T13 on a declined attempt, so credit #2.
- **Original late while the fallback is in flight:**
  1. A1 declines, and A2 is claimed and becomes `pending`.
  2. A1's late success arrives (T13, credit #1). `rejectCreatedSiblings` only rejects `created` siblings, so A2 survives.
  3. A2 succeeds (T7, credit #2). This is the A7 #1a reproduction, which produced 2 distinct `ledger_transaction_id` for one intent.
- **Timeout variant:** A1 times out and becomes `ambiguous`. The sweeper then declines it on an authoritative not-found (T6→…) and the cascade runs. After that, either of the two orders above.

### Other facts

- **Mock provider:** yes, the bug is reachable with the mock. The mock emits no unsolicited late success, but any signed callback or poll result drives the path, and the integration tests do exactly that. It is not reachable in production today, because no real provider is connected.
- **Concurrency:** not required. Concurrent success paths are serialised by `deposit_intents ... FOR UPDATE` (`receipt.go:443`, `drive.go:119/170`) and by the one-live-attempt index. The defect is a state-machine rule, not a race. A concurrent variant yields the same result, because both transactions serialise and each passes the per-attempt checks.
- **Duplicate callbacks:** they do not make it worse. The receipt dedupe, the ledger idempotency key and the attempt CAS (`succeeded` is terminal for T7) collapse exact redeliveries. Only different provider references (different attempts) produce a second credit.
- **Reconciliation:** it detects the result **after the fact**. `checkPlatformDuplicates` (`internal/reconciliation/payment_statement.go:977`) reports `pay_duplicate` when an intent has more than one succeeded attempt. It never prevents the credit, and it never alters balances.
- **Withdrawals / refunds / reversals:**
  - Payouts have one attempt per withdrawal (`payment_attempts_one_per_withdrawal`), and payout cascade is not implemented.
  - A success after a decline on a payout is **T14 → `disputed`**, never a second settle.
  - The T12 resend is gated by `IdempotentSubmission` plus `max_resubmits`.
  - Reversals: the PAY-REV-1 second-distinct-reversal check holds, and a reversal resolves the attempt's **own** posting (LF95-C6(b)).
  - Refunds are not implemented.
  - Residual in the same class: a self-contradicting payout provider (T14 residual, ADR 0095 §20) is detected but not prevented. The M1/M2 remediation is BLOCKED on HD-0095-1.

## 2. F-POOL-2 relationship

**Caused by the F-POOL-2 design (ADR 0095). It is not a pre-existing defect.**

At Stage 10.3 (`103b033`), `receiveDepositCallback` treated any callback on a terminal intent (`succeeded/declined/failed`) as a no-op. The intent stored only the latest cascade reference, so a late success for an earlier provider's reference found no intent (`ErrDepositIntentNotFound`, 404). The legacy code under-credited a real late capture, leaving it for reconciliation to find. It never double-credited.

F-POOL-2 introduced two things:
- per-attempt resolution, which lets a late success reach its own attempt;
- T13 "second capture posts" (LF-Q1).

The previous F-POOL-2 plan is therefore **not** sufficient: T13, the T7 guard, LF-Q1 and §20's accepted residual ("T13 with a sibling already `submitting` may capture a third time") must be amended.

## 3. Invariant (INV-DEP-1)

> For every `deposit_intent`, at most one `payment_attempt` with `operation='deposit'` is ever
> `succeeded`, and at most one `ledger_transactions` row of type `deposit` (non-reversal) is ever
> posted with `correlation_id = intent.id`. A verified provider success never, by itself, grants
> permission to post when the intent is already financially resolved.

Enforcement (defence in depth):

1. **Single choke point.** `postDepositSuccess` is the only deposit-crediting function. Every caller (T7 or T13, from the receipt path, phase C, the sweeper, T17 re-drive, or the legacy path) checks the intent's financial resolution under the existing `deposit_intents FOR UPDATE` lock before posting. If it is already `succeeded`, or another attempt of the intent is `succeeded`, it takes the no-post branch described in §4.
2. **Database backstop.** A new migration adds a partial unique index `payment_attempts (tenant_id, deposit_intent_id) WHERE operation='deposit' AND state='succeeded'`. It has a pre-flight that refuses to apply while existing data violates it, with no bypass, per the 0101 runbook pattern. Ledger-finance to rule on whether an additional ledger-level unique index `ledger_transactions (tenant_id, correlation_id) WHERE transaction_type='deposit'` is also required; its reversal and tombstone interplay needs checking.
3. **Reconciliation.** `pay_duplicate` remains as the detector. A new or extended kind flags "provider captured, platform disputed (not posted)", so the unposted real capture is surfaced for refund.

## 4. Required fix design

- **New T13 (declined → succeeded), only if the intent is not already financially resolved.** Otherwise the attempt goes **declined → `disputed`** (a new T13d, sibling of T13t):
  - terminal reason `multiple_success_for_intent`;
  - **no posting**;
  - P1;
  - an audit record;
  - an anomaly receipt, answered with the uniform 200.
- **T7 guard (submitting/pending/ambiguous → succeeded).** If the intent is already resolved, the attempt goes to **`disputed`** through T10 instead (same reason, no posting, P1).
- **In-flight siblings.** At the T13 or T7 success that resolves the intent, `created` siblings are rejected (unchanged). `submitting/pending/ambiguous` siblings are not force-transitioned, because the provider may still capture. Their later success hits the T7 guard and goes to `disputed`.
- **The second, real capture is not credited.** It is held as a disputed attempt pending refund or reversal. The M1/M2 resolution and the refund remain **BLOCKED on HD-0095-1 / LEDGER-MANUAL-ADJ-4EYES-1**, which are already recorded human decisions.
- **Ledger-finance must formally supersede LF-Q1 for this case.** The alternative, a suspense or unallocated posting that records the money without crediting the player, is ledger-finance's call. It is not the default, because its release posting is also BLOCKED.
- **T17 / `pay_status_mismatch` re-drive** (ADR 0095 §… line ~1592) must go through the same choke point, so reconciliation-triggered re-drive can never post a second credit.
- **Bonus:** unchanged, because only the first posting is linked to the intent.
- **Tenant:** the choke point runs inside `WithTenant`; cross-tenant evidence is already refused at binding (INV-IO-14).

### Tests that must change

These tests encode "the second capture posts" and must be **inverted**, not deleted:
- `receipt_integration_test.go` (17 references);
- `rvlf_i1_regression_integration_test.go` (12);
- `migration_0101_integration_test.go` (9);
- `payment_statement_integration_test.go` (4; the `pay_duplicate` fixtures are built from real posting paths, so these need a different fixture that doesn't rely on the now-forbidden second posting);
- a few `httpserver` tests.

## 5. Test matrix (every scenario ends with loAssertBalanced + projection rebuild, audit asserted, at most one posting per intent)

| # | Scenario | Expected |
|---|---|---|
| A | Original succeeds first | 1 credit; the original is `succeeded`; `created` fallback siblings are `rejected`. |
| B | Fallback succeeds first | 1 credit (fallback). |
| C | Original succeeds after fallback succeeded | The original goes `declined → disputed` (T13d). No 2nd credit; P1; audit. |
| D | Original and fallback succeed concurrently (`-race`, ≥50 reps) | Exactly 1 credit; the other attempt ends `disputed`. |
| E | Duplicate original success callback | No-op: 1 receipt effect, 1 credit. |
| F | Duplicate fallback success callback | No-op. |
| G | Same provider reference repeated | Deduplicated by the receipt and the ledger key. |
| H | Different provider references for one intent | At most 1 credit; the extras are `disputed`. |
| I | Original times out → fallback succeeds → original late success | 1 credit; the original is `disputed`. |
| J | Callback while the attempt is `ambiguous` and the intent is already resolved | T10 → `disputed`, no post. |
| K | Concurrent delivery of C/D/I under the race detector | As above. |
| L | Cross-tenant callback | Refused at binding; no effect in either tenant. |
| M | Reconciliation | `pay_duplicate` never fires after the fix. A new or extended kind flags a provider capture held as `disputed` (not posted). Reconciliation writes nothing. |
| N | Mutation | Removing the choke-point check fails B/C/D/H/I; removing the DB index fails the backstop test. |
| O | T17 / re-drive of a disputed second capture | No post. |

## 6. Ownership (no self-assignment, no self-review)

| Role | Assignment |
|---|---|
| Architect | Amend ADR 0095 §4.3 (T13/T13d/T7 guard), §4.4 matrix, §20 residual and §21.2; confirm A7 lock order unchanged. |
| Ledger-finance | Supersede LF-Q1 for this case; rule on the ledger-level index and on no-post vs suspense; sign off. |
| Payments | Owns the state-machine semantics and the reconciliation kind; reviews. |
| Backend | Implements (the callback agent, **only after** the architect and ledger-finance designs are recorded). |
| QA | Owns the test matrix §5 and the inverted tests; independent run. |
| Security | Tenant binding, audit, no disclosure in the uniform 200. |
| Code reviewer | Independent review with mutations. |

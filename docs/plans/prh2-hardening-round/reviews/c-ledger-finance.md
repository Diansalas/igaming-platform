_Reviewer: `ledger-finance`. Recorded verbatim by the orchestrator._

# Ledger-finance review — PRH-2 C (PAY-DEP-REF-VALIDATE-1 + INVDEP1-BACKSTOP-BRANCH-TEST-1)

**Scope:** `prh2-c-dep-ref-validate` @ `fc0d18b` (base `c458bb8`; no migration), reviewed via `git archive`.

**Method:**
- Private DB `lf_c_20261003`, under `pipefail`, with every log grepped for FAIL.
- **Dropped** (no `lf_%` DB remains), and the scratch export and my throwaway probe deleted.
- No repo, role or credential change. DB access worked throughout.

## Verdict: ACCEPT WITH CONDITIONS

Pre-merge items are marked **[PRE-MERGE]**. No T10 posts, and C-1 is closed. One probe found a real liveness gap on the new missing-amount path (F-C1).

## Test runs (local, not CI)
- `-race -tags integration -count=1 -p 1 ./internal/payments/...`: `ok` (477.7s), FAIL count 0.
- `./internal/reconciliation/...`: `ok`. The partial-migration fixture failure I reported in K2 (C-K2-2) no longer reproduces at this base.
- **Mutants I re-ran** (exact-anchor replace; reverted byte-identical, confirmed with `cmp` against `git show fc0d18b`):

| Mutant | Result |
|---|---|
| **M2** (E2 C-1): backstop branch disabled (`if false && errors.Is(err, ErrDepositIntentAlreadyResolved)`) | **KILLED**: `TestINVDEP1_BackstopBranch_PreCheckPassesLedgerIndexFires_DisputesAndCommits` ("racer B … got error … ledger backstop") |
| **C-AMT-2**: phase C amount comparison dropped | **KILLED**: `TestDepSyncAmount_MismatchDisputesNoPosting` (the attempt became `succeeded`) |
| **C-BIND-1**: binding verdict dropped | **KILLED**: `TestDepRefConflict_…ParksEveryOutcome` (the unique-violation error loop reappears, SQLSTATE 23505) |

- **Probes (throwaway):**
  - **Drift:** after a `sync_amount_mismatch` park, an invalid-reference park and one real post, `RunLedgerVsProjection` reports **0 mismatches** and the ledger is balanced. **Drift stays zero.**
  - **Missing-amount pollability:** **FAILS** (F-C1).

## Focus items

**Every path that posts.**
- The code order in `applyDepositCallResult` is:
  1. an invalid reference parks (`invalid_provider_reference:<reason>`);
  2. the **LF-6 binding pre-check** for every outcome that carries a reference (`provider_reference_conflict`, which covers deposit **and payout** attempts plus other intents);
  3. then the outcome switch. For `Succeeded`: the **amount** check (`sync_amount_mismatch`), then the tombstone check, then `postDepositSuccessOrDispute` (INV-DEP-1), then the post.
- The binding check therefore runs *before* the amount check, not after as your note said. That matches ADR 0095 §34.3. Both are T10s with no posting, so the order has no financial effect.
- **No T10 produces a ledger transaction:** `parkDepositAttempt` and the tombstone branch only call `ApplyDisputeFromNonTerminal` plus audit; every C test asserts zero transactions and a balanced ledger; my drift probe agrees.
- **I1** (INV-DEP-1) holds; M2 is killed by a real race. **I3** holds (no new key path; binding conflicts become T10s, not unique-violation loops). **I4** holds (same-tenant predicate, no cross-tenant read; FORCE RLS, test pinned). **I6** holds (no allocation; a mismatched capture stays disputed and uncredited).

**Missing amount goes to ambiguous, and `MarkAmbiguousFromSubmitting` does not bind the reference: this is a liveness defect (F-C1).**
- Probe, with a sync success, a valid reference and no amount echo:
  - after phase C: `state=ambiguous`, `attempt.provider_reference=<nil>`, while the intent carries the reference;
  - after making it due and running two sweeps: still `ambiguous`, reference nil, `poll_count=2`.
- `processViaQueryStatus` reschedules any attempt without a reference without polling (`sweeper.go`). So "the poll decides" (ADR §34.2) **never happens**.
- The only remaining resolution is a callback the receipt path can match by merchant reference, or `pay_unresolved` after the 24-hour horizon. Money the PSP reports as captured sits unresolved.

**Intent projection recompute (implementer note 4): ruling is UNIFY.** The phase C tombstone T10 (`reversal_tombstone_precedes_success`) calls `ApplyDisputeFromNonTerminal` directly. It writes **no `payment.attempt_disputed` audit and no recompute**, while the three new parks write both. Every phase C dispute must go through `parkDepositAttempt`, so the audit record and the intent projection are uniform (F-C2).

**The C-1 test (`invdep1_backstop_branch_integration_test.go`): meets every requirement.** It is a real race (the X5 construction; no production seam) and asserts:
- disputed with `multiple_success_for_intent`;
- exactly one `payment.attempt_disputed`;
- `payments_deposit_intent_index_backstop_fired` logged exactly once (and the ordinary alert once);
- exactly one deposit posting for the intent, none keyed on `refB`;
- racer B commits with no error;
- a balanced ledger.

M2 re-killed. **INVDEP1-BACKSTOP-BRANCH-TEST-1 can be closed.**

## QA F2 ruling (reconciliation impact of the new dispute reasons)

**(a) Should the `payment_statement` stream flag these reasons? Yes. Excluding them is not correct.**

The `:950` exclusion ("disputed: already a payments P1") rests on the premise that every disputed attempt was already alerted when it was disputed. **That premise is false for C's reasons.** ADR §34.7 says they emit **no** P1 line, and the claim there that "a disputed attempt is already reported through reconciliation" is **incorrect** for them. Per reason:

- **`sync_amount_mismatch`: flag as `pay_captured_unposted`**, the same kind and semantics as ADR 0095 §28.9 (the provider says it captured, the platform disputed, nothing posted).
  - Extend the `capturedUnposted` cases at both `:941` (a matched line, status `succeeded`) and `:1000` (standing, unwindowed, no line this run) from `terminal_reason = 'multiple_success_for_intent'` to `IN ('multiple_success_for_intent', 'sync_amount_mismatch')`.
  - Clearing is unchanged: a reversal line, or a tombstone on the attempt's bound reference.
  - **Prerequisite:** the validated reference must be **bound on the attempt** at park time. It passed the LF-6 pre-check, so binding cannot conflict. Today `ApplyDisputeFromNonTerminal` leaves it NULL, so `byRef` matching and the tombstone clear cannot work.
  - `pay_amount_mismatch` also fires in-window. Both are correct.
- **`provider_reference_conflict`:**
  - When the reference is bound to **another deposit** attempt, the statement line resolves to that holder, and a second `succeeded` line for the same reference raises `pay_duplicate` (`check=duplicate_line`). That is adequate as an in-window signal.
  - When it is bound to a **payout** attempt, or when a deposit line resolves to the parked attempt **by merchant reference** with status `succeeded`, flag it in-run as `pay_captured_unposted`. The parked attempt cannot hold the reference (unique index), so standing coverage needs persisted-line evidence, the same approach as ruling 5(c)(d). That part can follow in D.
- **`invalid_provider_reference:*`:**
  - A statement line carrying the invalid reference is refused at import (`validatePaymentLine`), which fails the run (`run_failed`, P1). That is a loud signal.
  - A line that resolves to the parked attempt by merchant reference with status `succeeded` must be flagged in-run as `pay_captured_unposted`.
- **Security C-1** (parked attempts still return the PSP redirect) makes a real capture on a parked attempt *more* likely. So the in-run rule must apply whatever outcome the adapter originally reported.

**(b) Pre-merge for C, or D?**
- **[PRE-MERGE C]:** bind the validated reference on the attempt for `sync_amount_mismatch` parks and on the missing-amount ambiguous path (F-C1; the same code change); record the adapter outcome in the park audit (F-C3); unify the tombstone T10 (F-C2); correct §34.7.
- **D (binding before D merges, and before any real PSP is enabled; the MOCK statement source only until then):** the `payment_statement.go` matcher changes in (a) and the QA F5 test in (c). Acceptable because C's reasons are reachable only against the MOCK adapter and the MOCK statement source.
- **K2 follow-up (register):** extend `player_open_payment_exposure` (MA020) to `sync_amount_mismatch`, cleared by a tombstone on the now-bound reference. Otherwise a goodwill credit could hand-pay a mismatched capture, the F4 logic again. Before the first real-money tenant.

**(c) What the test must assert** (`payment_statement` stream, MOCK source, one tenant):
1. A `sync_amount_mismatch` park, plus a statement line (matched by the bound reference) with status `succeeded` and the provider's amount: the run reports **`pay_captured_unposted`** for that attempt (and `pay_amount_mismatch`). Not silently excluded.
2. **Standing:** a later run whose coverage excludes that line still reports `pay_captured_unposted`.
3. **Clearing:** it clears only after a tombstone on the bound reference or a reversal line. An M1 or `investigation_status` change does not clear it.
4. A `provider_reference_conflict` bound to a **payout** attempt, plus a deposit line resolving by merchant reference to the parked attempt with status `succeeded`: flagged. A conflict bound to another deposit attempt, plus a second `succeeded` line with the same reference: `pay_duplicate`.
5. An `invalid_provider_reference` park, plus a line carrying the invalid reference: the import is refused and the run failure is audited (P1). A merchant-resolved `succeeded` line: flagged.
6. In every case: zero ledger transactions for the parked attempt, SUM(D) = SUM(C), `RunLedgerVsProjection` reports 0 mismatches, and the reconciliation run posts nothing.
7. Mutant: revert the predicate to `multiple_success_for_intent` only; tests 1 and 2 must fail.

## Findings and conditions

| ID | Sev | Finding | Condition | When |
|---|---|---|---|---|
| **F-C1** | **MEDIUM-HIGH** | A missing-amount sync success leaves the validated reference unbound on the attempt (`MarkAmbiguousFromSubmitting` does not bind it). The sweeper claims the attempt and reschedules it without ever polling. **Probe:** `poll_count=2`, reference nil, still `ambiguous`. A reported capture is never auto-resolved. | Bind the validated, LF-6-checked reference on T6 when it is non-empty (`provider_reference = COALESCE(provider_reference, $ref)`). Apply the same to a plain `Ambiguous` outcome that carries a reference (same root cause, pre-existing). Do the same at the `sync_amount_mismatch` park, which is the F2 prerequisite. Test: missing-amount, then one sweep → `QueryStatus` is called with the reference, and success posts exactly once. | **[PRE-MERGE]** |
| **F-C2** | LOW | The tombstone T10 in phase C writes no `payment.attempt_disputed` audit and does not recompute the intent, unlike the new parks. | Route it through `parkDepositAttempt` (ruling: unify). | **[PRE-MERGE]** |
| **F-C3** | LOW | Park audits do not record the adapter-reported outcome, so it cannot be determined later whether a park may hide a capture. | Add `adapter_outcome` to `parkDepositAttempt`'s metadata. | **[PRE-MERGE]** |
| **F-C4** | LOW | `foreignReferenceBinding` checks `payment_attempts` and `deposit_intents` but not `ledger_transactions`. A sync success whose `(provider, reference)` equals an existing non-tombstone posting key (for example a payout Step B `withdrawal_completed` settlement reference at the same PSP) would reach `ledger.Post` and loop on `ErrIdempotencyPayloadMismatch`. | Extend the pre-check to `ledger_transactions (tenant_id, provider_id, provider_tx_id)` for types other than `tombstone` (the tombstone has its own T10), then T10 `provider_reference_conflict`. | D, or a C follow-up |
| **F-C5** | LOW | ADR 0095 §34.7 says the new reasons are "already reported through reconciliation"; that is false (QA F2). | Correct the text, and route the new T10s through the I-wire alert core as P1, with the other `payment.attempt_disputed` sites. | **[PRE-MERGE]** for the text; I-wire for the alert |
| F2 | see the ruling | The recon matcher and test (a)/(c); the MA020 extension | As ruled | D; K2 follow-up |

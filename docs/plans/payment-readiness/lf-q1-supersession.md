# LF-Q1 supersession: multiple verified successes for one deposit intent (PAY-DOUBLE-CREDIT-1)

- **Author and authority:** `ledger-finance`. This ruling covers financial invariants, the ledger
  schema and the accounting treatment. It is binding on `payments`, `backend` and `qa` for the
  parts marked RULED.
- **Trigger:** the human's binding invariant (Financial Hardening workstream). One deposit intent
  gives at most ONE succeeded deposit attempt and at most ONE authoritative ledger posting. A
  verified provider success must not, by itself, authorize another player credit once the intent
  is financially resolved.
- **Inputs:**
  - `docs/plans/payment-readiness/double-credit-reconciliation.md` (a60b08c);
  - ADR 0095 §4.1, §4.3 (T7/T13/T13t), §4.4, §5.1, §5.4, §12.5, §20, §21.2;
  - `docs/governance/task-registry.md` (HD-0095-1, LEDGER-MANUAL-ADJ-4EYES-1,
    PAY-DOUBLE-CREDIT-1);
  - code read at `a60b08c`: `postDepositSuccess`, `receipt.go`, `drive.go`, `sweeper.go`,
    `ledger.Post`, `internal/reconciliation/payment_statement.go`,
    `internal/bonus/deposit_sweep.go`, `internal/kyc/enforcement.go`.
- **Scope:** a design ruling only. No code, migration or ADR was changed. ADR 0095 §21.2 is left
  verbatim; the `architect` adds the amendment that points here.

## 1. Supersession of LF-Q1 for the multiple-success case — RULED

**Old ruling (ADR 0095 §21.2, unchanged text):** "T13 posts to `player_cash`, not a suspense
account." A verified, amount- and asset-matching success means the platform received the player's
money, so `psp_clearing` is debited and `player_cash` credited. T13 therefore posts even when a
sibling attempt of the same intent has already succeeded (a "second capture"), and raises P1
`multiple_success_for_intent`. §20 accepts a further residual: a sibling already `submitting` may
capture a third time.

**Why it is unsafe:**

1. **It credits one purchase intent more than once.** The player asked to deposit X once and
   receives 2X (or 3X) of spendable, withdrawable `player_cash`. The P1 comes after the fact.
   By then the money may already be wagered or withdrawn, and reversing it needs a manual
   adjustment that is BLOCKED (LEDGER-MANUAL-ADJ-4EYES-1). In practice, "detect and correct"
   means "detect and hope".
2. **It turns a provider-side defect into a player-side liability automatically.** A second
   capture is almost always a PSP or cascade malfunction, or a player paying twice by mistake. It
   is not an instruction to fund the wallet. Treating verified evidence of money movement as
   authority to credit confuses **evidence** with **authorization**.
3. **It is reachable with no concurrency.** The fallback-then-late-original sequence is purely
   sequential (double-credit-reconciliation.md §1). It is therefore a design rule, not a race
   that locks could fix.
4. **Its premise was the stranding risk, and that risk is avoidable differently.** LF-Q1 rejected
   suspense because releasing it needed a BLOCKED manual posting. Crediting the player to avoid
   stranded funds trades a recoverable accounting hold for an unrecoverable over-credit. That is
   the wrong direction for a fail-closed ledger.

**New ruling (supersedes §21.2 for this case only):**

- **INV-DEP-1.** Per deposit intent there is at most one `succeeded` deposit attempt and at most
  one `ledger_transactions` row with `transaction_type='deposit'` (`correlation_id = intent.id`).
  A reversal (`deposit_reversal`) or tombstone does not reopen the intent. Once an intent has a
  succeeded attempt, it is **financially resolved for ever**, even if that posting is later
  reversed.
- **A matching verified success for an attempt whose intent is already financially resolved never
  credits the player:**
  - from `declined` it is the new **T13d**: `declined → disputed`, terminal reason
    `multiple_success_for_intent`;
  - from `submitting/pending/ambiguous` it is a T7 guard → **T10** `disputed` (same reason);
  - either way: P1, an audit record, an anomaly receipt with the uniform 200, and **no error and
    no rollback**, so there is no 5xx loop. Its accounting treatment is §2.
- **T13 stays valid only as the first success.** A `declined → succeeded` is allowed when no
  other attempt of the intent is `succeeded`. That covers the late original after a decline when
  the fallback has not captured.
- **What still stands from §21.2:**
  - The LF95-C6 constraints (the attempt's own ledger link; the tombstone precedes success).
  - The statement that a matching success is evidence that real money moved. What changes is the
    consequence: it is evidence to be **accounted for** (§2), not a credit.
- **§20 residuals that are withdrawn:** "T13 with a sibling already `submitting` may capture a
  third time" and "second capture posts" are no longer accepted residuals. A third real capture
  follows the same no-credit path as the second.
- **Enforcement point.** The single choke point is `postDepositSuccess`. It is the only
  production `TxDeposit` poster: I checked every `ledger.TxDeposit` use outside tests. Every
  caller reaches it under `deposit_intents FOR UPDATE`, and it must refuse (typed sentinel) when
  the intent already has a succeeded attempt or a deposit posting. This applies to the receipt
  path, phase C, the sweeper, T17 re-drive and the legacy `InitiateDeposit`. The callers map
  that refusal to T13d or T10. The DB backstops are in §3.

## 2. Accounting treatment of the second REAL capture

### 2.1 Does an existing human decision already determine it? No. **Human decision required.**

I checked every related register item precisely.

| Item | What it decides | Does it decide (A) vs (B)? |
|---|---|---|
| HD-0095-1 (human, open) | **Who** may force-resolve a disputed attempt or unresolvable payout (M1/M2), and above what threshold | **No.** It governs the *exit* from `disputed`, not whether the capture is recognised in the ledger while disputed. It is also itself undecided. |
| LEDGER-MANUAL-ADJ-4EYES-1 (registered, blocks go-live) | The four-eyes manual adjustment and mismatch-resolution API | **No.** It is the release/correction *mechanism* both options eventually need. It is not a recognition policy, and it is not decided either. |
| PAY-DOUBLE-CREDIT-1 (open) | Records the defect and the plan. The plan explicitly leaves "no-post vs suspense" to ledger-finance and calls suspense "not the default". | **No.** That is an orchestrator plan, not a recorded human decision. |
| The human's invariant (this workstream) | At most ONE authoritative ledger posting per intent; no automatic player credit | **Only partly.** It excludes crediting the player, so both (A) and (B) comply. It does not say whether an unallocated suspense posting (a different transaction type, no player credit) counts as a second "authoritative ledger posting". A reading either way is an interpretation of the human's words, and I will not make it on their behalf. |
| ADR 0095 §4.1 (`disputed`: "automation must not resolve"; T10/T13t/T14/T15 post nothing) | Architecture precedent: disputed means no posting | **No.** It is an ADR, not a human decision. It supports (A) as the default. It does not bind the accounting-policy choice. |
| Existing chart of accounts (`internal/ledger/ledger.go`) | No suspense or unallocated account type exists | Not a decision. It means (B) needs a new account type and transaction type (a ledger schema migration). |

The choice is whether player-attributable money that the platform holds, but has not allocated,
is recognised in the ledger. That has safeguarding and audit weight, which is exactly the class
this specialist must not decide alone. So this is exactly **one** human decision,
**HD-LEDGER-UNALLOC-1**: "Is a verified second real capture on an already-resolved deposit intent
(A) held off-ledger as a disputed attempt, or (B) recognised on-ledger in an unallocated
(suspense) liability account, pending refund or manual allocation?"

### 2.2 Options and consequences

| Dimension | (A) Attempt `disputed`, no posting | (B) Unallocated/suspense posting (Dr `psp_clearing`, Cr `unallocated_receipts`), no player credit |
|---|---|---|
| Player credit | None | None |
| INV-DEP-1 | Holds trivially | Holds only if (B) uses a **distinct** `transaction_type` (for example `deposit_unallocated`) and account type; it must never be `deposit`. |
| Ledger completeness | **Incomplete.** Real money the PSP holds for us, and owes back to the payer, is absent from the ledger. The only record is the attempt row, the receipt, the audit and the P1. | **Complete.** The receipt and the obligation are both on the ledger, and the balance sheet shows the liability. |
| `psp_clearing` vs PSP settlement | A permanent, explainable difference per disputed capture until refunded. Reconciliation must explain it with a new kind (§4). | `psp_clearing` matches what the PSP will settle; the open item is on the unallocated account. |
| Refund path (PSP-initiated refund or chargeback of the 2nd capture) | **Already works.** The `deposit_reversal` for that reference resolves the disputed attempt, which has `LedgerTransactionID == nil`, so it takes the existing **tombstone** branch: no ledger effect, net zero, reconciliation clean. It also blocks any later M1 credit of that reference. | **Needs code.** Today the reversal posting always debits `player_cash` (`applyReversalReceiptEvidence`). For a (B) posting it must mirror the original's entries (Dr unallocated, Cr `psp_clearing`). Otherwise a refund would debit the player: a new wrong-debit defect. |
| Platform-initiated refund | NOT IMPLEMENTED (ADR 0095 §5.5) under both options | Same |
| Allocation to the player (the human decides the second payment was intended) | M1 (BLOCKED, HD-0095-1) plus a credit via LEDGER-MANUAL-ADJ-4EYES-1 (BLOCKED) | A transfer from unallocated to `player_cash` via LEDGER-MANUAL-ADJ-4EYES-1 (BLOCKED) |
| KYC cumulative deposits (`sumSettledDeposits` counts `transaction_type='deposit'`) | Not counted (correct: not the player's deposit) | Not counted, provided the transaction type is distinct |
| Bonus deposit sweep (joins `deposit_intents.ledger_transaction_id`) | Unaffected | Unaffected |
| New schema | None | New account type, new transaction type, CHECK updates, reversal-mirroring change. All ledger-finance-owned and buildable. |
| What is BLOCKED | Final resolution of the dispute (HD-0095-1) and any allocation (LEDGER-MANUAL-ADJ-4EYES-1); outbound refund (§5.5) | Same three, plus release of the unallocated balance other than through a PSP-initiated reversal |

**My recommendation, as a recommendation not a decision, for the human:** (B) as the **target**
end state, implemented only once the reversal-mirroring change is reviewed.

- A player-funds ledger should recognise every unit of money received and owed onward.
- `psp_clearing` should reconcile to PSP settlement without off-ledger explanations.
- (B) keeps the liability visible to finance and safeguarding reporting.

(A) remains an acceptable permanent policy if the human prefers no new account class. Its cost
is a standing reconciliation exception per case.

### 2.3 Interim implementation, so the technical work isn't blocked — RULED: **(A)**

(A) is the safe interim:

- **It never credits the player.**
- **It writes no ledger rows at all,** so there is nothing to reverse or compensate if the human
  picks (B).
- **It composes with the existing reversal and tombstone machinery today.**
- **It is reversible toward (B).** If (B) is chosen, a one-time forward-only backfill can post
  exactly one `deposit_unallocated` entry per `disputed`/`multiple_success_for_intent` attempt
  that has not been refunded:
  - idempotency key `unallocated:<provider_id>:<provider_reference>`, so it is replay-safe;
  - no historical entry is edited;
  - the backfill is gated on the (B) schema migration.
  - Going from (B) interim to (A) would instead require compensating entries, which is why (B) is
    not the interim.
- **Conditions on the interim:**
  - the disputed attempt stores the matched amount, asset and provider reference (it already
    does);
  - the P1 alert names the intent and attempt, with no amounts in log lines (security S-5);
  - the new reconciliation kind in §4 ships with it, so no held capture is ever invisible.

## 3. DB backstop — RULED: both indexes are required

**(i) `payment_attempts (tenant_id, deposit_intent_id) WHERE operation='deposit' AND
state='succeeded'` (partial unique): required, but not sufficient on its own.**

- *Why it is required:* it enforces the state-machine half of INV-DEP-1 across every transition
  source. That includes future M1/M2 manual transitions: a `disputed → succeeded` M1 on an
  already-resolved intent is refused by the index.
- *Why it is not sufficient:*
  - `postDepositSuccess` calls `ledger.Post` **before** `ApplySuccess`, and nothing structural
    ties the two together.
  - The legacy `InitiateDeposit` path posts through `postDepositSuccess` with **no attempt row**,
    so the attempt index cannot see it. That path is still compiled, exported and used by the
    test bridge.
  - Any future code that posts without, or before, an attempt transition bypasses it.
  - The invariant is about money. Only the ledger is authoritative for money.
- Pre-flight: the migration refuses to apply while any intent has more than one succeeded
  deposit attempt. No bypass, per the 0101/0092 runbook pattern. Dev and scratch data produced by
  the current T13 tests will trip it; that is expected and correct.

**(ii) `ledger_transactions (tenant_id, correlation_id) WHERE transaction_type='deposit'`
(partial unique): REQUIRED.** Ledger schema change owned by ledger-finance. Name it
`ledger_transactions_one_deposit_per_intent`.

- **Contract amendment that goes with it (binding):** for `transaction_type='deposit'`,
  `correlation_id` IS the `deposit_intents.id`. This is already true of the only production
  poster (`postDepositSuccess`: `CorrelationID: intent.ID`).
  - A future deposit vehicle not driven by an intent must use a distinct `transaction_type`, or
    mint a real intent first. An example is unsolicited crypto deposits to a custodial address
    (ADR 0008). Flag to the `architect`.
- **Replay safety.** `ledger.Post` already classifies conflicts by looking up the idempotency key
  **first**, regardless of which constraint Postgres names (the P2-A fix). The new index must
  follow the 0092 pattern exactly:
  - a new typed sentinel `ErrDepositAlreadyPostedForIntent` is returned **only** when this index
    fired **and** no row exists for the request's idempotency key;
  - an exact redelivery (same `provider_id:provider_reference`) still returns `AlreadyPosted`;
  - callers map the sentinel to T13d/T10 (no 5xx). If it ever fires, the choke point was
    bypassed, which is a P1 defect signal.
- **Reversal interplay:**
  - `deposit_reversal` rows are a different type, so they are outside the index.
  - A reversed deposit still occupies the slot, so a late sibling success after a reversal is
    T13d/T10, not a fresh credit. That matches INV-DEP-1: a reversal does not reopen the intent.
  - PAY-REV-1 (0092, one reversal per original) is unaffected.
  - With a single posting per intent, `intent.ledger_transaction_id` equals the succeeded
    attempt's own link. LF95-C6(b) (reverse the attempt's own posting) therefore becomes
    defence-in-depth, and must keep being asserted (§5).
- **Tombstone interplay:**
  - Tombstones are type `tombstone` with a random correlation id, so they are outside the index.
  - A success whose reference a tombstone holds is decided before posting (T10/T13t). If it
    reached `ledger.Post`, the existing `(tenant, provider_id, provider_tx_id)` index would fire
    first and give an untyped error, exactly as today.
  - Under (A), a PSP refund of a held second capture writes a tombstone on its reference. That is
    correct and needs no posting.
- **Option (B) interplay:** the unallocated posting must use its own `transaction_type`, so it
  never collides with this index. Its own at-most-once key is the idempotency key
  `unallocated:<provider_id>:<provider_reference>`.
- **Existing data shapes (checked):**
  - Every production deposit posting uses `correlation_id = intent.id`.
  - Test fixtures that post `TxDeposit` directly mostly use `uuid.New()` or `gen_random_uuid()`
    (bonus, migration_0101, wallet, ledger, statement fixtures), so they are unaffected.
  - **One fixture breaks, and must be changed, not the index:**
    `internal/idempotency/integration_test.go`
    (`TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse`, lines 320-321: two
    distinct occurrence keys sharing one `corr`) posts two `TxDeposit` rows with the same correlation id. Give each occurrence its
    own correlation id. What that test pins is idempotency-key composition, not correlation
    sharing.
  - Pre-flight for the migration: refuse if
    `SELECT tenant_id, correlation_id FROM ledger_transactions WHERE transaction_type='deposit'
    GROUP BY 1,2 HAVING count(*) > 1` returns any row. No bypass.

## 4. T17 / `pay_status_mismatch` re-drive, reconciliation kinds, bonus — RULED

**T17 and the §12.5 re-drive job:**

- **Evidence only.** Re-drive goes QueryStatus → the §4.4 matrix → the `postDepositSuccess`
  choke point. It can never post for an already-resolved intent: the result is T13d/T10, then no
  posting.
- **Terminal attempts are never state-changed by T17.** That covers `disputed` (including
  `multiple_success_for_intent`), `succeeded` and `rejected`. The CHECK already forbids
  `next_action_at` on terminal rows. T17 against a terminal attempt is a read-only re-verify
  whose result is recorded as evidence for the M1 queue.
- **`declined` is the one terminal state T17 may move**, via T13 (first success only) or T13d.
- The re-drive job must **not** act on the new kind below.

**Reconciliation kinds.** **Yes, a new kind is required: `pay_captured_unposted`**
("provider captured, platform disputed, not posted").

- *Why it is needed:* `matchPayment` currently **skips** every `disputed` attempt ("already a
  payments P1"). Under (A), an unrefunded real capture would therefore never appear in
  reconciliation at all. The only trace would be a one-shot P1 alert.
- *Condition:*
  - the attempt is `disputed` with `terminal_reason='multiple_success_for_intent'`;
  - the statement line (if any) is `succeeded`;
  - there is no `deposit_reversal` line and no ledger tombstone under that reference.
  - `reversal_tombstone_precedes_success` disputes are excluded: they are net zero at the PSP.
- *Remediation:* escalate. Never auto-resolve. Never trigger T17 re-drive. It clears when a
  reversal or tombstone appears (refunded), or when M1/allocation happens (BLOCKED).
- *Ageing:* report it on every run until it clears, with the amount, so the open exposure is
  always visible.
- *Under (B):* the kind becomes "unallocated receipt still open". The ledger join (LF95-C13)
  must include `deposit_unallocated` postings, mapped to their disputed attempt.
- **`pay_duplicate` / `checkPlatformDuplicates` stays as a detector.** After §3 it is
  structurally unreachable for new data. Any occurrence means an index was dropped, or the data
  predates the migration: a P1 integrity alert.

**Bonus impact: none.**

- `internal/bonus/deposit_sweep.go` qualifies deposits by joining `deposit_intents.ledger_transaction_id`,
  which is the first and now the only posting.
- Removing the second posting cannot change which deposit triggers a bonus.
- Under (B), the unallocated posting has a distinct type and no intent link, so bonus never
  sees it.
- Adjacent consumer: KYC `sumSettledDeposits` stops over-counting second captures under both
  options (a correction, not a regression).

## 5. Tests and fixtures that encode "second capture posts" (invert, never delete)

I checked the repository at `a60b08c`. Each item must keep its original protective purpose and
assert the new behaviour.

| # | Location | What it encodes today | Inverted assertion |
|---|---|---|---|
| 1 | `internal/payments/receipt_integration_test.go` `TestReceipt_T13SecondCapture_ThroughApplyReceiptEvidence_LedgerBalanced` (L78–L200) | The child succeeds, then the declined parent's late success **posts** its own distinct ledger transaction | Same set-up: the parent's late success returns **no error** (keeps the PRH-I5 "no 500 loop" property) and gives disposition `anomaly`. The parent goes `declined → disputed` (T13d, `multiple_success_for_intent`). Exactly **one** deposit posting for the intent; balance = one amount; intent link unchanged; P1 audit present; balanced; projection rebuild. |
| 2 | `internal/payments/rvlf_i1_regression_integration_test.go` `TestRVLF_P6_ReversalOfSecondCaptureReversesItsOwnTransaction` (L298–L362; asserts `cashBalance == 2*amt` at L331) | A second capture is posted, and its reversal reverses it | The parent's late success is disputed and unposted (balance = `amt`). A reversal naming the **parent's** reference takes the **tombstone** branch (no ledger debit; balance unchanged). A reversal naming the child's reference reverses the one posting (`reverses_transaction_id` = the child attempt's own link; keeps the LF95-C6(b) assertion). A distinct second reversal of the child is still `ErrDepositAlreadyReversed` (PAY-REV-1). Final balance 0; balanced. |
| 3 | `internal/reconciliation/payment_statement_integration_test.go` `TestPaymentStatement_Kind_DuplicatePlatformSuccess` (L529–L563) | Builds `pay_duplicate` from a real T13 second posting | Split into two. (a) The real path: the same sequence now yields **`pay_captured_unposted`** and **no** `pay_duplicate`. (b) The detector: build two succeeded attempts on a scratch DB migrated to just **before** the §3 migration (legacy-data shape), assert `pay_duplicate`, then assert the §3 migration's pre-flight **refuses** on that data. |
| 4 | `internal/idempotency/integration_test.go` `TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse` (L320–L321) | Two distinct-key `TxDeposit` postings share one correlation id | Not a "second capture" test, but it will violate §3(ii). Give each occurrence its own correlation id; the idempotency-composition assertions are unchanged. |
| 5 | ADR 0095 §16.2 test list ("a reversal of a T13 second capture reverses the second capture, not the first") and the evidence file `evidence/prh-i1-mutation-kill.txt` (PRH-I5 T13 mutant) | A specification and evidence that assume the second capture posts | Documentation, not tests: `architect` (ADR) and `payments` (evidence) must re-state them against item 1/2's inverted form. Not deleted. |

**Checked and NOT inverted (they stay valid):**

- `TestRVLF_P3_T13RejectsCreatedSibling`: the T13 there is the **first** success.
- `TestRVLF_F3_*`: no second posting.
- `TestMigration0101_T13t_*` and `TestMigration0101_T12_RefusedWhenSiblingSucceeded`: one
  succeeded attempt each.
- The `internal/httpserver` "T13" matches (`TestAdmission_T13_DomainIndependence`, and the QA-plan
  "T13" labels in the tenant-binding tests) are QA test-plan numbering, unrelated to deposit T13.
  **No httpserver test encodes a second capture.** This corrects the plan's "a few httpserver
  tests".
- No test asserts the `deposit.second_capture_posted` audit action. After the fix, that
  production audit branch in `postDepositSuccess` becomes dead code and must be removed together
  with the second-capture branch.

**New tests required (in addition to the plan's §5 matrix A–O):**

- the `ErrDepositAlreadyPostedForIntent` backstop via direct `ledger.Post` with a fresh key and
  a reused intent correlation;
- an exact-redelivery replay still `AlreadyPosted` with the new index present;
- a reversal-then-late-sibling-success → T13d/T10, no posting;
- `pay_captured_unposted` appears, then clears after a tombstone;
- a mutation kill for each of: the choke-point check; the attempt index; the ledger index; the
  T17-on-terminal guard.

## Labels

- This ruling: IMPLEMENTED as a design ruling.
- INV-DEP-1 enforcement, the §3 indexes, the reconciliation kind and the inverted tests:
  NOT IMPLEMENTED.
- The final accounting treatment: BLOCKED on human decision HD-LEDGER-UNALLOC-1. The interim (A)
  is RULED and may be built now.

---

## Confirmation of ADR 0095 revision 4, §28 AM-2 / §29 (`8d04848`) — `ledger-finance`, 2026-09-27

All six implementation choices are **CONFIRMED**. None needs correcting. There are two binding
notes, on choices 2 and 6.

1. **Migration 0107: guard and kind CHECK. CONFIRMED.** Without the `CREATE OR REPLACE
   payment_attempts_guard()`, T13d would be refused by the 0101 trigger. Keeping `declined →
   disputed` restricted to exactly `{reversal_tombstone_precedes_success,
   multiple_success_for_intent}` is correct. The kind CHECK must be a strict superset of 0102's.
2. **Pre-flight = the index build in `DO … EXCEPTION` (0092 pattern). CONFIRMED, and this
   corrects my §3.** Under `FORCE ROW LEVEL SECURITY`, with no `app.tenant_id` set, my
   `GROUP BY` pre-flight would read zero rows and pass duplicates. It is demoted to the
   operator's diagnostic, run as a role that sees every tenant.
   - **Operational note (binding for production):** a non-`CONCURRENTLY` build inside a
     transaction blocks writes to `ledger_transactions` for the build's duration. That is
     acceptable at current synthetic scale. Before any production-size ledger, `devops` must
     plan the window, or a separate concurrent-build procedure with the same fail-closed
     outcome.
3. **"Resolved" = resolved by ANOTHER attempt or ANOTHER ledger idempotency key. CONFIRMED.**
   - An exact redelivery (same attempt, same `provider_id:provider_reference`) stays
     `AlreadyPosted` or a no-op.
   - On the legacy path, A = NULL makes every succeeded attempt count, which is correct.
4. **Tombstone check before INV-DEP-1. CONFIRMED.** It matches my ruling: a PSP-refunded
   capture nets to zero and must be `reversal_tombstone_precedes_success`, not
   `pay_captured_unposted`.
5. **Legacy `InitiateDeposit`. CONFIRMED.**
   - It returns the sentinel and posts nothing.
   - P1 and `deposit.multiple_success_refused` go in a **separate** tx, because the caller's tx
     rolls back on the error.
   - The path stays scheduled for removal.
6. **Poll-path T13d: audit plus P1, no receipt. CONFIRMED** (polls are not receipted, §5.3).
   - **Binding note:** because no receipt exists, the T13d audit record must carry the matched
     `provider_id`, `provider_reference`, amount and asset, and the evidence kind
     (`query_status`).
   - Security S-5 still applies: amounts go in the audit metadata, not in log lines.
   - `pay_captured_unposted` covers it anyway, because the kind is keyed on the attempt, not on
     a receipt.

**Other checks.**
- The §28.8 down-migration (fail closed if `pay_captured_unposted` rows exist; no row deletion)
  is correct.
- The attempt-index violation, which aborts the tx with a 5xx, is acceptable. It is reachable
  only if both the choke point and the ledger index were bypassed.
- The §29.1 durable-state map and §29.2 derived predicate are consistent with INV-DEP-1.

`docs/architecture/reconciliation-model.md` §2.2 (new amendment block) and §2.6 (explained
difference) are updated for `pay_captured_unposted` in the same commit.

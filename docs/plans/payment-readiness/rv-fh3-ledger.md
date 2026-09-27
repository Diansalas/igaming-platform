# RV-FH3: ledger-finance review of PAY-DOUBLE-CREDIT-1 (INV-DEP-1, ADR 0095 §28 AM-2)

- **Reviewer:** `ledger-finance`, main financial sign-off. I am not the implementer.
- **Branch / commit:** `worktree-agent-adce273a3a5339f77` @ `a927aed`.
  - FH-3 itself is `8ce538c`.
  - Follow-ups: `109ef04`, `bdb9085`, `f43025c`, `5541b23`, `9e107ed`, `a927aed`.
- **Baseline:**
  - ADR 0095 §28 (AM-2) and §29;
  - `docs/plans/payment-readiness/lf-q1-supersession.md` (ruling `17e5ffc` and confirmation `4c85e5e`);
  - ADR 0082 A7 §(7) and `rv-a7-tests.md`;
  - QA's `qa-fh3-adjudication.md`.
- **Environment:**
  - A detached worktree, and two private DBs via `priv_db.sh`: `rv_lf_fh3_*` for the suites and
    `rv_lf_fh3m_*` for the manual migration round trip. Both are dropped and the worktree is
    removed.
  - **No `sudo`, `ALTER ROLE` or password change.** DB access worked throughout.

## Verdict: CHANGES REQUIRED (two small items). Every money path is approved.

**No money-movement defect was found.** INV-DEP-1 held in every scenario I ran:

- QA's matrix A–O and the adjudicated D/K, green under `-race`;
- my 25-repetition concurrent storm (see Probes);
- the reversal-of-a-disputed-capture probes;
- direct attacks on the ledger backstop.

**Result:** at most one succeeded deposit attempt and at most one `deposit` posting per intent,
the ledger balanced, and projection = rebuild, every time.

**Blocking sign-off:**

- **C1, guard weakening.** Migration 0107 weakens a DB guard. The fix is a one-line migration
  change.
- **C2, reconciliation ageing.** `pay_captured_unposted` does not meet the "reported on every
  run until it clears" rule. Under HD-LEDGER-UNALLOC-1 (A), that report is the **only** standing
  record of real money held off-ledger, so it is part of financial correctness, not polish.

Everything else is either an approval or a non-blocking condition.

## Item-by-item

| Item | Result | Evidence |
|---|---|---|
| **Choke point** in `postDepositSuccess` and the check order | **APPROVED** | Pre-check `postDepositSuccessOrDispute` (§28.3 rule 1.4), then re-check inside `postDepositSuccess` (rule 2), then the ledger index (rule 3). All three production sites use the wrapper: receipt (`applyDepositSuccessAndPost`), phase C (`drive.go`), and the sweeper/T17 (`applyStatusEvidence`). `resolvedForOtherDeposit` is exactly §28.2 (another succeeded attempt, `id IS DISTINCT FROM A`; or a deposit posting under a different idempotency key). The dead "second capture posted" branch is removed, and the audit action is retired. |
| Check order: mismatch → tombstone → INV-DEP-1 → post | **APPROVED** | Receipt matrix, phase C and sweeper all do mismatch, then `tombstoneExists`, then the wrapper. Probe Q1a and QA's `TestINVDEP1_Mutation6_*` confirm a refunded capture gets `reversal_tombstone_precedes_success`, not `multiple_success_for_intent`. |
| T7 guard, T13 first-success-only, T13d | **APPROVED** | `applyMultipleSuccessDispute`: live state → T10, declined → T13d (`ApplyMultipleSuccessForIntent`). No posting, no error, uniform 200 on the callback path. Matrix C, H, I, J and the inverted T13 tests pass. |
| Exact redelivery stays `AlreadyPosted` | **APPROVED** | Probe Q3 (a direct `ledger.Post` replay → `AlreadyPosted`); matrix E, F, G. Mutant IDK (drop the `idempotency_key <> K` clause) is killed. |
| Poll-path audit fields (my confirmation, binding note 6) | **PARTIAL (condition C3)** | `auditMultipleSuccessForIntent` records `provider_id`, `provider_reference`, `last_evidence_kind`, `deposit_intent_id` and the terminal reason. **Amount and asset are missing.** They are recoverable from the attempt row, because a T13d/T10 fires only on matching evidence, but the note required them in the audit. |
| No posting on a second capture | **APPROVED** | Matrix C, D, H, I, K; probes Q1a, Q1b, Q2; A7 #1a. |
| Legacy `InitiateDeposit` path | **PARTIAL (condition C4)** | `resolveAmbiguous` → `postDepositSuccess(attemptID=nil)` returns `ErrDepositIntentAlreadyResolved` and posts nothing. That part is correct. But **`RecordDepositMultipleSuccessRefusal` has no caller**: the P1 and `deposit.multiple_success_refused` audit row in a separate tx (§28.3 rule 3, my confirmation item 5) is never written, and nothing tests it. The path has no production caller (scheduled for removal, security P2-L2), so this is non-blocking. Label: PARTIALLY IMPLEMENTED. |
| `ledger.ErrDepositAlreadyPostedForIntent` | **APPROVED** | Returned only when the 0107 ledger index fired **and** no row exists under the request's idempotency key (the P2-A lookup-first rule). Probe Q3: a new key with the same intent → sentinel; the same key with a different amount or correlation → `ErrIdempotencyPayloadMismatch`; an exact replay → `AlreadyPosted`. Mutant LSEN (sentinel mapping removed) is killed by Q3 and `TestMigration0107_LedgerBackstop_*`. |
| Migration 0107: both indexes, DO/EXCEPTION pre-flights | **APPROVED** | **Manual round trip:** up → down → up → down → up, with indexes, CHECK and guard present or absent each time as expected. **Refusal:** I seeded two `deposit` rows with one `correlation_id` at 0106. `migrate up` refused with the runbook message and stayed at 0106. A plain `SELECT` as the owner saw **0** of those rows under FORCE RLS, which demonstrates why the GROUP BY pre-flight had to be demoted (confirmation item 2). Mutants LIDX and AIDX (index predicate neutralised) are both killed. |
| Migration 0107: the kind CHECK | **APPROVED** | A strict superset of 0102's list, plus `pay_captured_unposted`. |
| Migration 0107: guard trigger | **REJECTED (C1)** | See F1. |
| Migration 0107: down file | **APPROVED with Low L1** | It restores 0101's guard body **verbatim** (I diffed the extracted function bodies) and 0102's CHECK, and drops both indexes. L1: the CHECK restore is not wrapped. If `pay_captured_unposted` rows exist it fails closed, but with a generic check-violation message instead of the ADR's runbook text. |
| `pay_captured_unposted` | **CHANGES REQUIRED (C2)** | See F2. The clearing conditions (reversal line or tombstone) are **untested**: mutant RTMB (clearing conditions replaced by `true`) survives the full `internal/payments` and `internal/reconciliation` suites. Condition C5. |
| `pay_duplicate` | **APPROVED** | Kept as a detector. `TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape` builds the legacy shape on a 0106 scratch DB, asserts `pay_duplicate`, then asserts the 0107 pre-flight refuses that data. |
| T17 / re-drive through the choke point | **APPROVED** | T17 is the sweeper's `applyStatusEvidence` path, which calls the wrapper. `TestINVDEP1_O_*` and `TestINVDEP1_O2_*` (real `multiple_success_for_intent` reason) pass. The GRD mutant kills O2. |
| QA matrix, including the adjudicated D/K | **GREEN** | `-race -p 1` over `TestINVDEP1_*`, `TestMigration0107*`, `TestA7_1a*`, `TestRVLF_C2*` and `TestRVLF_C3*`: payments ok (50 s), reconciliation ok. Full baseline without `-race`: payments 313 s, reconciliation 46 s, ledger 17 s, idempotency ok. No data race reported. I reviewed QA's D/K adjudication: the arithmetic correction is right, the per-intent checks were strengthened, and the tests are not weakened. |

## Findings

**F1 (Medium, blocks sign-off): the 0107 guard admits a deposit `declined → disputed` with a NULL `terminal_reason`.**

- 0101 said `AND NEW.terminal_reason IS DISTINCT FROM 'reversal_tombstone_precedes_success'`,
  which refuses NULL.
- 0107 says `AND NEW.terminal_reason NOT IN ('reversal_tombstone_precedes_success',
  'multiple_success_for_intent')`. With NULL, that expression evaluates to NULL, so the IF is
  false and NULL passes.
- **Probe Q4 (confirmed):** a direct `UPDATE … SET state='disputed', terminal_reason=NULL` on a
  declined deposit attempt is **accepted**.
- **Impact:** application code always sets a reason, so this is a defence-in-depth loss. But a
  reason-less disputed deposit attempt would be invisible to both the M1 queue and
  `pay_captured_unposted`, which is keyed on the reason.
- `security` reached the same finding independently (`81dd4b7`, F-M1).
- **Fix:** `AND (NEW.terminal_reason IS NULL OR NEW.terminal_reason NOT IN (…))`, or
  `COALESCE(NEW.terminal_reason, '') NOT IN (…)`. Add a test for the NULL case.

**F2 (Medium, blocks sign-off): `pay_captured_unposted` is only raised when a statement line matches the attempt.**

- The kind is emitted only inside `matchPayment`, when the attempt is matched by a statement line
  with `succeeded`/`reversed` status. `checkUnmatchedAttempts` has no case for a disputed
  `multiple_success_for_intent` attempt.
- Statements are period-scoped: a capture appears in the statement for its own settlement period.
- **Scenario:** the second capture is reported once, in its period's run. In the next run's
  statement it has no line, so the exposure silently **disappears** from reconciliation while the
  money is still unrefunded.
- That contradicts ADR 0095 §28.9 / reconciliation-model §2.2 ("its statement line (if any)";
  "re-reported on every run until it clears").
- `loadPlatform` already loads every attempt of the provider, unwindowed, so the fix is local:
  also emit `pay_captured_unposted` from `checkUnmatchedAttempts` (or a dedicated pass) for any
  such attempt with no reversal line and no tombstone, marked "no statement line".

**F3 (Medium, non-blocking, pre-existing): the phase C and sweeper evidence paths use a pre-lock attempt snapshot.**

- `applyStatusEvidence` (sweeper) and `applyDepositCallResult` (phase C) act on the attempt read
  **before** taking `deposit_intents FOR UPDATE`, and never re-read it.
- In my 25-repetition storm, a concurrent callback had already moved the attempt, and the sweeper
  then failed with `T7/T13 ->succeeded` or `T10 ->disputed` **CAS conflict** errors. Its tx rolls
  back and it converges on the next tick.
- Money-safe: CAS plus INV-DEP-1, and I saw no double posting in any repetition. But it
  contradicts ADR 0095 §6.4 ("the late sync result is then passed to `applyEvidence` against the
  new state"), produces error and alert noise, and phase C can return an error to the player for a
  deposit that did succeed.
- `applyMultipleSuccessDispute` also picks T10 or T13d from the stale state.
- **Fix:** re-read the attempt under the intent lock at both sites, as `ApplyReceiptEvidence`
  already does.

**F4 (Low): no test distinguishes the application choke point from the ledger backstop.**

- Mutants **PRE**, **RECK** and **BOTH** (both `if resolved` checks disabled) **survive** the full
  payments and reconciliation suites. The 0107 ledger index catches the posting and maps it to the
  same dispute.
- That is defence in depth working, and the implementer disclosed it. But a regression that
  silently disabled the application checks would surface only as
  `payments_deposit_intent_index_backstop_fired` P1s in production.
- **Condition C6:** one test asserting that a normal T13d/T10 does **not** take the backstop path.
  For example, assert the audit and flag shape, or that a `backstopFired` signal is false.

**Low L1:** the down file's CHECK restore should be wrapped with the runbook message (see the
migration row above).

## Probes (scratch, not committed; source kept in the session scratchpad as `fh3-ledger-probe_integration_test.go.txt`)

| Probe | Result |
|---|---|
| **Q1a**, reversal of a **disputed** second capture | PASS. The reversal naming the disputed attempt's reference takes the tombstone branch: no debit, cash stays 5000, 1 tombstone. Reversing the posted capture debits once (→ 0). A redelivered late success after the tombstone gives no error and no credit. 1 deposit posting; balanced; projection = rebuild. |
| **Q1b**, the only posting reversed first, then the sibling's success | PASS. The sibling goes to `disputed`/`multiple_success_for_intent`, and cash stays 0. **A reversal does not reopen the intent.** |
| **Q2**, concurrent storm, 25 reps, `-race` | PASS. Each rep races two successes each for A1 and A2, a sweeper poll where both PSPs report success, and a reversal naming A1. Every rep has exactly one succeeded attempt and one deposit posting. Cash = (deposits − reversals) × 5000 (25/8 and 25/9 in two runs). Balanced; projection = rebuild. The only errors were F3's sweeper CAS conflicts; the callbacks had none. |
| **Q3**, ledger backstop semantics | PASS (see the table above). |
| **Q4**, guard NULL `terminal_reason` | **FAIL**: this is F1. |

## Mutants (my own, each applied with an anchor check and restored with `git checkout`; each kill confirmed by two isolated re-runs)

| Mutant | Result |
|---|---|
| IDK: the `idempotency_key <> K` clause neutralised | killed (`TestINVDEP1_G_*`, `TestINVDEP1_Mutation1_*`) |
| AID: the `id IS DISTINCT FROM A` clause neutralised | killed (matrix A–G, A7 #1a, many statement tests) |
| LSEN: ledger sentinel mapping removed | killed (Q3, `TestMigration0107_LedgerBackstop_*`) |
| GRD: T13d reason removed from the 0107 guard | killed (C, D, H, I, K, O2, inverted tests, Q1a, Q1b) |
| LIDX: ledger index neutralised | killed |
| AIDX: attempts index neutralised | killed |
| H1R3: raw-outcome fingerprint (my FH-5 C1) | killed (`TestRVLF_SecGapA3_ReversalFingerprintUsesRawWireOutcome`). **FH-5 C1 is now closed.** |
| **PRE / RECK / BOTH**: application choke-point checks disabled | **survive** (backstop-equivalent): F4, C6 |
| **RTMB**: `pay_captured_unposted` clearing conditions replaced by `true` | **survives** the full payments and reconciliation suites: C5 |

## A7 #1a (`a7_1a_integration_test.go`) against §(7): **MEETS §(7), with one strengthening condition**

§(7) requires three things, and the test meets each:

1. **The race.** The sweeper lease plus per-item claim (racer A) and a late callback (racer B)
   contend on one deposit intent. A holder keeps `deposit_intents` locked while both queue. The
   sweeper's own phase C for the cascade child runs after the release.
2. **Where the waiter blocks.** It asserts that both racers are blocked by the holder's pid
   (`a7WaitAnyLockWaiter` plus `loBlockingPIDs`, then `loWaitBlocked`).
3. **Outcome and invariants.** It asserts the outcome (exactly one attempt succeeded, the other
   disputed) and ends with `assertLedgerBalanced`, `loAssertBalanced` and
   `loAssertProjectionMatchesRebuild`.

**Condition C7:**

- The "exactly one posting" check (a) runs only inside `if finalChild.ProviderReference != nil`,
  and (b) counts distinct `payment_attempts.ledger_transaction_id`, not ledger rows. An
  unlinked second posting would therefore escape it.
- Replace it with an unconditional `count(*) FROM ledger_transactions WHERE
  transaction_type='deposit' AND correlation_id = intent` (the matrix's `ledgerDepositTxCount`).
- The test also relies on PostgreSQL's FIFO lock queue (racer A queued first) to route the cascade
  child to the second provider. Document that assumption in the test.

**#5c ("receipt insert after an L1 lock", tombstone branch).** I do **not** accept
"blocked-on-design".

- The ordering is observable without a second call site: assert **where** the second of two
  identical deliveries blocks.
- FH-6 already did exactly this for the main receipt path (A7-C1, `loBackendQuery` contains
  `INSERT INTO payment_provider_events`).
- For the tombstone branch, use two identical reversal deliveries naming a
  **resolved-but-never-posted** original, which is the branch that takes the intent lock.
  - With the correct order, the second delivery waits on the R0 insert.
  - With the mutant, it waits on `SELECT … FROM deposit_intents … FOR UPDATE`.
- That kills the mutant. It stays **required** for A7 closure (condition C8).

## Ruling: deposit-side lock order now that FH-3 has landed (ADR 0082 A7 gate review, deposit half)

**The as-built deposit sequence is RULED CONFORMANT to A7:**

- **Callback, deposit success:** R0 (receipt insert, first write) → L1 `deposit_intents FOR
  UPDATE` → attempt CAS (row lock) → choke-point reads → L3 projections (`ledger.Post` pre-lock) →
  L4 ledger insert, including the 0107 ledger-index insertion wait.
- **Callback, reversal:** R0 → L1 → L2 original `ledger_transactions FOR UPDATE` → L3 → L4 (A7-TOMB-1 as fixed).
- **Phase C / sweeper evidence:** L1 → attempt CAS → L3 → L4.
- **Deposit T2 claim:** L0.4 RG advisory → KYC reads → L1 → attempt CAS (A7-5a pinned).

Why the new 0107 index adds no cycle:

- The only transaction that can contend for the same `(tenant, correlation_id)` key is one
  posting for the **same** intent.
- Every such transaction already holds that intent's L1 lock, so the two serialize on L1 before
  either reaches the index.
- The legacy path inserts its intent row in the same transaction, so no other transaction can see
  the key until commit.

Deviations recorded (none is an ordering violation):

- F3: phase C and the sweeper act on a pre-lock attempt snapshot.
- The FH-5 L-e oversize-reason audit row no longer precedes R0 on redelivery (`TestRVLF_Le_*`).

**A7 status:** still `PARTIALLY IMPLEMENTED`. #1a is now present (C7 is a strengthening, not a
blocker). The only remaining §(7) item is **#5c (C8)**. The deposit-half gate review is
**complete** with this ruling, and the payout half was reviewed earlier. When #5c lands with a
killed mutant, A7 may be marked `IMPLEMENTED`.

## Conditions

| # | Item | Blocking? |
|---|---|---|
| C1 | F1: fix the guard's NULL handling in 0107 (or a new migration if 0107 has already shipped anywhere), and add a NULL test | **yes** |
| C2 | F2: `pay_captured_unposted` on every run until cleared, including with no statement line, with a test covering a run with no line | **yes** |
| C3 | Add amount and asset to the `multiple_success_for_intent` audit metadata | no (before the stage gate) |
| C4 | Wire `RecordDepositMultipleSuccessRefusal` on the legacy path (or delete the path), with a test | no (before the stage gate) |
| C5 | A test that kills RTMB: the kind clears after a reversal line, and after a tombstone | no (before the stage gate) |
| C6 | A test that distinguishes the application choke point from the ledger backstop (F4) | no (before the stage gate) |
| C7 | A7 #1a: count ledger rows by correlation id unconditionally; document the lock-queue assumption | no |
| C8 | A7 #5c by waiter-query assertion on the tombstone branch | no for FH-3; yes for A7 `IMPLEMENTED` |
| F3 | Re-read the attempt under the intent lock in phase C and the sweeper | no (before the stage gate) |
| L1 | Wrap the down file's CHECK restore with the runbook message | no |

Once C1 and C2 land with tests, `ledger-finance` signs off FH-3 financially, and I can confirm
that from the diff alone.

## Labels

- INV-DEP-1 enforcement (choke point, T10/T13d, both DB backstops, re-drive): **IMPLEMENTED**.
- Migration 0107: **IMPLEMENTED, with F1 to fix**.
- `pay_captured_unposted`: **PARTIALLY IMPLEMENTED** (F2).
- Legacy-path refusal audit: **PARTIALLY IMPLEMENTED** (C4).
- A7: **PARTIALLY IMPLEMENTED** (#5c).
- HD-LEDGER-UNALLOC-1 (B): **NOT IMPLEMENTED**, by decision (A now).

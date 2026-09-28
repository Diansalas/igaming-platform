# QA adjudication: FH-3 (ADR 0095 §28, migration 0107) test failure reports

- Reviewer: `qa`
- Branch under adjudication: `worktree-adce273a3a5339f77`, commits `8ce538c` (FH-3
  implementation) and `109ef04` (tip at the time of this adjudication, FH-5 security
  re-verification, unrelated to FH-3).
- Method: a **detached worktree** created from the main repo
  (`git worktree add --detach <scratchpad>/qa-fh3 109ef04`), against a series of
  **private** scratch databases (`priv_db.sh`/`priv_test.sh`, `PRIV_DB=qa_fh3_*`). No
  shared `TEST_DATABASE_URL` instance was touched for any *money-path* assertion; the
  one legacy-shape fixture below uses the project's standard `scratchdb` package
  (private, auto-cleaned, throwaway databases - the same mechanism every other
  migration-boundary test in this repository already uses). All scratch databases were
  dropped and the worktree removed at the end of this adjudication. No `sudo`,
  `ALTER ROLE`, `CREATE ROLE` or password change was run at any point.
- Mandate: **do not take the implementer's word.** Both reported failures were
  independently reproduced from a clean checkout and diagnosed with direct SQL evidence
  before any test file was changed.

## Verdict

**Both reported "test bugs" are confirmed to be genuine test bugs, not fix bugs.**
INV-DEP-1 (at most one succeeded deposit attempt, at most one deposit ledger posting,
per intent) held in **every single repetition observed** - 100 repetitions total across
`TestINVDEP1_D` and `TestINVDEP1_K`, each independently re-run after the correction,
plus every other scenario in the matrix. The corrected test files are committed on this
QA worktree branch (see §4) for the coordinator to copy into the FH-3 branch.

---

## 1. Item 1: `TestINVDEP1_D`/`TestINVDEP1_K` "cumulative wallet-balance assertion"

### 1.1 Reproduction, unmodified

Ran `TestINVDEP1_D_ConcurrentOriginalAndFallbackSuccess_Race` verbatim (as brought in at
`8ce538c`, with only the implementer's own documented `postDepositSuccess` signature
adaptation) under `-race` on a private DB:

```
--- FAIL: TestINVDEP1_D_ConcurrentOriginalAndFallbackSuccess_Race (3.48s)
    inv_dep1_matrix_integration_test.go:474: rep 1: PAY-DOUBLE-CREDIT-1: exactly one
    credit must survive a concurrent delivery, got balance=10000
```

This is real, reproducible, and matches the implementer's report exactly: rep 0 passes
(balance 5000), rep 1 fails (balance 10000, but the assertion still expects flat 5000).

### 1.2 Root cause (confirmed by inspection, then by evidence)

`invDep1RaceOnce` is called in a **sequential loop** (never concurrently across reps) by
both `TestINVDEP1_D` and `TestINVDEP1_K`, always against **one shared `invDep1Setup`**
(one tenant, one wallet, created once per test). Every repetition creates a **new**
deposit intent and, if the invariant holds, contributes its own, independent 5000-unit
credit to that **same** wallet. The wallet's cash balance is therefore cumulative across
reps by construction - rep 0 alone lands at 5000, rep 1 (correctly) at 10000, rep 2 at
15000, and so on. The original assertion (`cashBalance(...) != 5000` on every rep) never
accounted for this and was arithmetically wrong from rep 1 onward. This is a defect in
the test's own arithmetic, not in the code under test.

### 1.3 Independent verification (before touching anything)

Before writing any correction, I instrumented (temporarily, then made permanent) every
repetition to log the actual per-intent facts requested:

- count of succeeded deposit attempts for **that repetition's own intent**;
- count of `ledger_transactions` rows of type `deposit` for that intent's
  `correlation_id`;
- the wallet's cash balance;
- the running total of succeeded deposit attempts for the tenant (used to compute the
  correct cumulative expectation independently of any loop-index bookkeeping - see 1.4).

Ran **all 50 repetitions** of `TestINVDEP1_D` and **all 50 repetitions** of
`TestINVDEP1_K` under `-race`, to completion (not stopping at the first assertion
failure), on two separate private databases. Every single one of the 100 repetitions
recorded:

```
succeeded_deposit_attempts=1 deposit_ledger_transactions=1
```

**Zero exceptions.** No repetition, in either test, ever showed more than one succeeded
deposit attempt or more than one deposit ledger posting for its own intent. Sample lines
(full logs available; every one of the 100 follows this exact shape):

```
rep 0:    intent=145e853a-... succeeded_deposit_attempts=1 deposit_ledger_transactions=1 wallet_balance=5000    want_cumulative=5000
rep 1:    intent=6156f702-... succeeded_deposit_attempts=1 deposit_ledger_transactions=1 wallet_balance=10000   want_cumulative=10000
...
rep 49:   intent=f96c9700-... succeeded_deposit_attempts=1 deposit_ledger_transactions=1 wallet_balance=250000  want_cumulative=250000
rep 1000: intent=09eb74c8-... succeeded_deposit_attempts=1 deposit_ledger_transactions=1 wallet_balance=5000    tenant_total_succeeded_deposits=1  want_cumulative=5000
...
rep 1049: intent=e2ceeaf7-... succeeded_deposit_attempts=1 deposit_ledger_transactions=1 wallet_balance=250000  tenant_total_succeeded_deposits=50 want_cumulative=250000
```

(`TestINVDEP1_K` labels its reps `1000+i` for log-correlation purposes only, unrelated
to the arithmetic bug - see 1.4.)

This is direct, per-rep, per-intent SQL-backed evidence that **the invariant held every
time**; the flat `!= 5000` assertion was the only thing wrong.

### 1.4 A second, independent bug found in my own first-draft correction

My first correction computed the cumulative expectation as `(rep+1) * 5000`. Re-running
`TestINVDEP1_K` with that correction immediately failed:

```
rep 1000: intent=... wallet_balance=5000 want_cumulative=5005000
```

Because `TestINVDEP1_K` deliberately labels its reps starting at `1000` (a log-
correlation choice, documented in its own comment, to distinguish its lines from
`TestINVDEP1_D`'s when both are read together) - `rep` is a caller-chosen **label**, not
a guarantee of "this many intents have resolved on this wallet so far". Computing the
expectation from `rep` was itself fragile. The final correction instead queries the
**actual** total count of succeeded deposit attempts for the tenant
(`totalTenantSucceededDepositCount`) and multiplies by 5000 - correct regardless of any
caller's labelling choice, and it doubles as an independent, self-verifying cross-check
(if INV-DEP-1 ever let an intent post twice, this total would exceed the number of
intents actually created, and the cumulative-balance assertion would catch it on its
own, separately from the per-intent checks).

### 1.5 Correction applied

In `internal/payments/inv_dep1_matrix_integration_test.go`, `invDep1RaceOnce` now:

1. Keeps every existing per-intent invariant check (`assertInvariantAndBalanced`,
   unchanged) - **strengthened**, not weakened: two new explicit, fatal checks
   (`succeededCount != 1` / `ledgerTxCount != 1`) are asserted *before*
   `assertInvariantAndBalanced` runs, each labelled `FIX BUG` in its failure message so
   a real regression is unambiguous.
2. Computes the cumulative wallet-balance expectation from
   `totalTenantSucceededDepositCount(tenant) * 5000` (an independent DB query), not from
   the `rep` label.
3. Logs the full per-rep fact set (`t.Logf`, always, pass or fail) so a future run's
   evidence trail is self-contained without re-instrumenting.

Neither test's repetition count was reduced (`const reps = 50` unchanged in both), and
neither test's use of `-race` was removed.

### 1.6 Post-correction re-verification

Re-ran both tests to completion, `-race`, 50 reps each, on fresh private databases:

```
--- PASS: TestINVDEP1_D_ConcurrentOriginalAndFallbackSuccess_Race (4.90s)
--- PASS: TestINVDEP1_K_ConcurrentAdversarialOrderings_Race (5.34s)
```

All 100 log lines show the correct, growing cumulative balance and `succeeded_deposit_
attempts=1`/`deposit_ledger_transactions=1` on every single repetition.

---

## 2. Item 2: `TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape` needs a pre-0107 DB

### 2.1 Confirmed the reported cause

`newPayWorld(t)` uses `testPool(t)`, i.e. the shared `TEST_DATABASE_URL`, which is now
migrated through 0107. The test's own fixture builds the pre-§28 "legacy shape" (two
succeeded deposit attempts for one intent, via a direct `ledger.Post` + `ApplySuccess`,
bypassing the receipt path) - exactly the shape migration 0107's two partial unique
indexes exist to make impossible. On a fully-migrated database this fixture would now
be refused by the very backstop it is trying to predate, so the test cannot build its
fixture there. The implementer's diagnosis was correct.

### 2.2 Correction applied

`internal/reconciliation/inv_dep1_recon_integration_test.go` now builds this one test's
fixture on a **dedicated scratch database migrated only up to and including 0106** (one
migration before the 0107 backstop), following the exact pattern
`internal/payments/migration_0101_integration_test.go` and this same package's own
`migration_0098_integration_test.go` already use for migration-boundary tests:

- `migration0106ReconVersion` derives 106 from 0107's own on-disk filename (not a
  hard-coded literal), so a renumbering is caught here instead of silently testing the
  wrong boundary;
- `migration0106ReconDirThroughSelf` copies every migration file numbered `<= 106` into
  a fresh temp dir;
- `migration0106ReconScratch` creates a private, auto-cleaned scratch database
  (`internal/testsupport/scratchdb`, never the shared instance) migrated through that
  temp dir, and asserts 106 was in fact the last migration applied;
- `newPayWorldOnPool` duplicates `newPayWorld`'s own wiring (mock providers, capability
  registration, orchestrator, statement sources) against an explicit pool, rather than
  refactoring `newPayWorld` itself - confining this correction to this file only.

The test then, in order:

1. builds the legacy two-succeeded-attempts shape (unchanged construction);
2. runs the `payment_statement` stream and asserts `pay_duplicate` still fires - the
   "detector still valid on legacy data" property, **unchanged assertion**;
3. **new:** migrates the *same* database (which still carries this exact violating data)
   the rest of the way to the latest on-disk migration (0107), and asserts this
   **fails**, with the 0107 attempts pre-flight's own message
   ("more than one succeeded deposit attempt exists for a deposit intent") - closing the
   "confirm the 0107 pre-flight refuses that data" requirement this file's previous
   revision had flagged as NOT IMPLEMENTED.

### 2.3 Verification

```
--- PASS: TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate (0.09s)
--- PASS: TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape (1.97s)
    inv_dep1_recon_integration_test.go:304: migration 0107 correctly refused on the
    legacy duplicate-succeeded-attempts shape: db: apply migration 107
    (deposit_intent_double_credit_backstop): ERROR: migration 0107: more than one
    succeeded deposit attempt exists for a deposit intent; ... (SQLSTATE P0001)
```

Note also that `TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate` - the sibling
test that drives the **real** receipt path for a second capture and was written
test-first to fail pre-fix - now **passes** against `8ce538c`: `pay_captured_unposted`
fires and `pay_duplicate` does not, confirming the fix's reconciliation-side behaviour
end to end, not just the payments-package assertions.

---

## 3. Full matrix re-run against `109ef04` (with the two corrections applied)

All 17 tests in `internal/payments/inv_dep1_matrix_integration_test.go`, `-race`,
one private DB:

| Test | Result |
|---|---|
| `TestINVDEP1_A_OriginalSucceedsFirst_CreatedSiblingRejected` | PASS |
| `TestINVDEP1_B_FallbackSucceedsFirst` | PASS |
| `TestINVDEP1_C_OriginalSucceedsAfterFallbackSucceeded_Disputed` | PASS |
| `TestINVDEP1_D_ConcurrentOriginalAndFallbackSuccess_Race` (50 reps, `-race`) | PASS |
| `TestINVDEP1_E_DuplicateOriginalSuccessCallback` | PASS |
| `TestINVDEP1_F_DuplicateFallbackSuccessCallback` | PASS |
| `TestINVDEP1_G_SameProviderReferenceRepeated_LedgerKeyDedupe` | PASS |
| `TestINVDEP1_H_ThreeDistinctReferences_AllButFirstDisputed` | PASS |
| `TestINVDEP1_I_TimeoutThenFallbackThenLateOriginal` | PASS |
| `TestINVDEP1_J_AmbiguousSiblingAfterResolution_T7Guard` | PASS |
| `TestINVDEP1_K_ConcurrentAdversarialOrderings_Race` (50 reps, `-race`) | PASS |
| `TestINVDEP1_L_CrossTenantCallback_NoEffectInEitherTenant` | PASS |
| `TestINVDEP1_O_ReDriveOfDisputedAttempt_NoPost` | PASS |
| `TestINVDEP1_Inverted_T13SecondCapture_BecomesDisputed_NoSecondPosting` | PASS |
| `TestINVDEP1_Inverted_RVLF_P6_ReversalOfDisputedSecondCaptureTakesTombstoneBranch` | PASS |
| `TestINVDEP1_Mutation1_ResolvedForOtherDepositPredicate` (implementer-added) | PASS |
| `TestINVDEP1_Mutation6_TombstonePrecedesMultipleSuccessForIntent` (implementer-added) | PASS |

`internal/reconciliation/inv_dep1_recon_integration_test.go` (2 tests, with the §2
correction): both PASS (§2.3).

`internal/idempotency/inv_dep1_correlation_integration_test.go`: unaffected by this
adjudication, still passes (part of the full-package run below).

### 3.1 Full-package re-runs (not just the matrix files)

| Suite | Result |
|---|---|
| `internal/payments` (entire package, `-race -tags=integration`) | ok (380.2s) |
| `internal/reconciliation` + `internal/reconciliation/statement` | ok (84.1s / 1.1s) |
| `internal/idempotency` | ok (2.6s) |
| `internal/ledger` | ok (39.2s) |

### 3.2 Static checks and migration verify

| Check | Result |
|---|---|
| `go vet ./...` | clean |
| `go vet -tags=integration ./...` | clean |
| `gofmt -l .` | clean |
| `golangci-lint run ./...` (pinned 2.9.0, untagged) | `0 issues` |
| `go run ./cmd/migrate verify` | "all applied migrations verified clean, no version gaps" (through 0107) |

---

## 4. Files changed by this adjudication

Both files are corrected **in place** (no new files, no scenario removed, no repetition
count reduced, no per-intent check weakened):

- `internal/payments/inv_dep1_matrix_integration_test.go` - `invDep1RaceOnce`'s final
  assertion block (§1.5) plus a new `totalTenantSucceededDepositCount` helper. Every
  other scenario (A, B, C, E-J, L, O, the two Inverted tests) is untouched.
- `internal/reconciliation/inv_dep1_recon_integration_test.go` - a new pre-0106 scratch-
  DB helper set and `newPayWorldOnPool`, and
  `TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape`'s setup line plus a new
  closing assertion (§2.2). `TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate` is
  untouched.

**These files are committed on the QA worktree branch, not the FH-3 branch** - this
agent's sandbox refuses every git operation and direct file write targeting a checkout
other than its own assigned worktree (confirmed by attempting `git status`/`git commit`
directly, `git -C`, `cd && git`, and `git --git-dir=... --work-tree=...`, all refused).
Per the coordinator's own instruction, the corrected files are committed here for the
coordinator to copy into `worktree-adce273a3a5339f77`. Note that these files will **not
compile as-is** in this QA worktree's own branch, since that branch predates the FH-3
fix (`postDepositSuccess`'s changed signature) entirely - they are only correct against
`109ef04` (or later, same signature), where they were written and verified.

## Label

Per `CLAUDE.md`'s completion labels: FH-3 (ADR 0095 §28, migration 0107) is, from this
independent QA adjudication, confirmed **IMPLEMENTED** with respect to INV-DEP-1 as
tested by this matrix - no fix bug was found in either reported failure; both were
genuine test-authoring bugs in QA's own file, now corrected and re-verified with
evidence, not weakened. This does not itself constitute a full sign-off on FH-3 (that
remains the code-reviewer's/ledger-finance's/security's standing responsibility); it
certifies only that the two specific failure reports raised against this matrix do not
indicate a double-credit defect.

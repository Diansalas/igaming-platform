# QA FH-7 Final Gate — Financial Hardening / Payment Readiness (PRH)

Reviewer: `qa`. Date: 2026-09-28. Worktree: `.claude/worktrees/agent-a49d13dcf2ff1dc33`,
branch reset to `origin/claude/focused-wright-jw88w9`, HEAD `649f3ba` (descendant of `7c071c3`,
confirmed via `git merge-base --is-ancestor 7c071c3 HEAD`). Working tree clean throughout.

**Overall verdict: GATE PASS, except item O (CI-BILLING-1), which is BLOCKED, not PASS, per the
human's explicit instruction. No product code was changed by this review; two orphaned duplicate
`go test` processes from a tooling mistake of my own were killed and the private DB was rebuilt
before the authoritative run (see §3 note).**

## 0. Setup

- `git fetch origin claude/focused-wright-jw88w9` — up to date.
- `git reset --hard origin/claude/focused-wright-jw88w9` → HEAD `649f3ba32d8fbf6458fc4ddf3ebaaa434f17d4b3`
  ("docs(evidence): FH-7 idle-machine timing lane 40/40 (TEST-RESISO-RACE-1 local half)").
- `git merge-base --is-ancestor 7c071c3 HEAD` → true.
- Private DB `fh7_qa_gate_1790570507` created via `priv_db.sh`, migrated to head, used for every
  run below, dropped at the end via `DROP DATABASE ... WITH (FORCE)` through the test-admin
  connection (same credential the sanctioned scripts use). No shared DB touched, no role/password
  changed.

## 1. Static checks

| Check | Result |
|---|---|
| `go build ./...` | Clean, exit 0 |
| `go vet ./...` | Clean, no output |
| `go vet -tags integration ./...` | Clean, no output |
| `gofmt -l .` | 0 files listed |
| Pinned `golangci-lint run --allow-parallel-runners ./...` (2.9.0, untagged) | `0 issues.` |

## 2. Migration verify and 0107 reversibility

- `go run ./cmd/migrate verify` on the fresh private DB (migrated 1→0107): `migrate verify: all
  applied migrations verified clean, no version gaps`. Full list runs OK through
  `0107_deposit_intent_double_credit_backstop`.
- 0107 reversibility tests, all in `internal/payments/migration_0107_integration_test.go`
  (run as part of the full suite in §3, package `ok`):
  - `TestMigration0107_UpDownUpRoundTrip_CleanDB`
  - `TestMigration0107_Down_RefusesWithRunbookMessageWhenCapturedUnpostedRowExists`
  - `TestMigration0107_Up_ExistingValidData_Succeeds`
  - `TestMigration0107_Up_DuplicateSucceededAttempts_RefusedByAttemptsPreflight`
  - `TestMigration0107_Up_DuplicateLedgerPostings_RefusedByLedgerPreflight`
  - `TestMigration0107_LedgerBackstop_ErrDepositAlreadyPostedForIntent`
  - `TestMigration0107_T13tT13d_TerminalReasonTrigger_HEAD`

## 3. Full integration suite (`-race -tags integration -count=1 -p 1`, timing lane skipped)

**Note on a tooling mistake I made and corrected:** my first background launch of this run used
`&` inside a command that was *also* passed `run_in_background: true`, which detached an orphan
`go test` process that kept running (writing to the same log file) after I believed it had exited.
I discovered two live `go test` PIDs writing to the same file and hitting the same private DB
concurrently, `kill -9`'d both, rebuilt the private DB from scratch, and reran once, cleanly, with
a single tracked background process. The result reported below is from that single, clean run
only; the earlier contaminated attempt's output was discarded and is not used as evidence anywhere
in this report.

Command: `go test -race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./...`

Result: **exit 0. Every package `ok`. Zero `FAIL`, zero `panic:`, zero `DATA RACE`** (grepped for
all three; none found).

| Package | Result | Time |
|---|---|---|
| cmd/platform-api | ok | 3.5s |
| internal/admission | ok | 1.1s |
| internal/apierror | ok | 1.0s |
| internal/assetregistry | ok | 3.0s |
| internal/audit | ok | 1.1s |
| internal/auth | ok | 3.0s |
| internal/bonus | ok | 13.3s |
| internal/casino | ok | 20.2s |
| internal/config | ok | 1.1s |
| internal/db | ok | 3.8s |
| internal/economicop | ok | 1.2s |
| internal/email | ok | 1.0s |
| internal/eventbus | ok | 1.0s |
| internal/geolocation | ok | 1.0s |
| internal/httpserver | ok | 231.8s |
| internal/idempotency | ok | 1.7s |
| internal/identity | ok | 2.0s |
| internal/identityresolution | ok | 1.4s |
| internal/jurisdiction | ok | 16.7s |
| internal/kyc | ok | 10.7s |
| internal/ledger | ok | 15.5s |
| internal/money | ok | 1.0s |
| internal/operatingmarket | ok | 13.3s |
| internal/payments | ok | 307.8s |
| internal/providercred | ok | 27.8s |
| internal/providerkind | ok | 1.8s |
| internal/providerref | ok | 1.0s |
| internal/providers | ok | 1.0s |
| internal/providers/httpclient | ok | 1.3s |
| internal/reconciliation | ok | 46.6s |
| internal/reconciliation/statement | ok | 1.0s |
| internal/rg | ok | 15.8s |
| internal/risk | ok | 5.3s |
| internal/secretstore | ok | 4.9s |
| internal/secretstore/awssm | ok | 6.8s |
| internal/secretstore/devfile | ok | 1.0s |
| internal/sportsbook | ok | 37.5s |
| internal/tenant | ok | 1.0s |
| internal/testsupport/credentialscan | ok | 1.0s |
| internal/txscope | ok | 1.2s |
| internal/validation | ok | 1.0s |
| internal/wallet | ok | 1.7s |
| internal/webhookauth | ok | 1.8s |
| internal/webhookauth/webhookauthtest | ok | 1.4s |
| internal/withdrawal | ok | 7.9s |
| 9 other packages (cmd/migrate, cmd/seed-admin, internal/observability, providercred/providercredtest, providers/httpclient/conformance, secretstore/memstore, testsupport/noeffect, testsupport/phasecapture, testsupport/scratchdb) | `[no test files]` | — |

## 4. A–O adversarial matrix (`double-credit-reconciliation.md` §5) — mapped, exists, and green

All tests below are in the `ok` `internal/payments`, `internal/reconciliation` or
`internal/idempotency` package runs from §3 (no test was run in isolation for this section beyond
what §3 already exercised, except D/K, re-run separately for repetitions in §5).

| Row | Scenario | Test(s) | Status |
|---|---|---|---|
| A | Original succeeds first | `TestINVDEP1_A_OriginalSucceedsFirst_CreatedSiblingRejected` | PASS, substantive (asserts 1 credit, sibling rejected) |
| B | Fallback succeeds first | `TestINVDEP1_B_FallbackSucceedsFirst` | PASS |
| C | Original succeeds after fallback | `TestINVDEP1_C_OriginalSucceedsAfterFallbackSucceeded_Disputed` | PASS (asserts T13d, no 2nd credit, P1, audit) |
| D | Concurrent original+fallback, race ≥50 reps | `TestINVDEP1_D_ConcurrentOriginalAndFallbackSuccess_Race` (50 internal reps) | PASS — see §5, re-run standalone `-race -count=50` (2500 total iterations), ok 200.1s |
| E | Duplicate original callback | `TestINVDEP1_E_DuplicateOriginalSuccessCallback` | PASS |
| F | Duplicate fallback callback | `TestINVDEP1_F_DuplicateFallbackSuccessCallback` | PASS |
| G | Same provider reference repeated | `TestINVDEP1_G_SameProviderReferenceRepeated_LedgerKeyDedupe` | PASS |
| H | 3 distinct references | `TestINVDEP1_H_ThreeDistinctReferences_AllButFirstDisputed` | PASS |
| I | Timeout → fallback → late original | `TestINVDEP1_I_TimeoutThenFallbackThenLateOriginal` | PASS |
| J | Ambiguous sibling after resolution | `TestINVDEP1_J_AmbiguousSiblingAfterResolution_T7Guard` | PASS |
| K | Concurrent adversarial orderings, race | `TestINVDEP1_K_ConcurrentAdversarialOrderings_Race` (50 internal reps) | PASS — see §5, re-run standalone `-race -count=50` (2500 total iterations), ok 211.6s |
| L | Cross-tenant callback | `TestINVDEP1_L_CrossTenantCallback_NoEffectInEitherTenant` | PASS |
| M | Reconciliation | `TestINVDEP1_Recon_M_CapturedUnposted_ReplacesDuplicate`, `TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape` (`internal/reconciliation/inv_dep1_recon_integration_test.go`) | PASS. Assertions read: confirms `pay_duplicate` no longer fires after the fix and the new/extended `pay_captured_unposted` kind fires instead; not vacuous. |
| N | Mutation | App choke point: `TestINVDEP1_Mutation1_ResolvedForOtherDepositPredicate`, `TestINVDEP1_Mutation6_TombstonePrecedesMultipleSuccessForIntent`, `TestINVDEP1_FL1_ApplicationChokePointCatchesItBeforeTheDBBackstop` (all `migration_0107_integration_test.go`); DB backstop bypassing the app layer: `TestMigration0107_LedgerBackstop_ErrDepositAlreadyPostedForIntent`, `TestMigration0107_Up_DuplicateSucceededAttempts_RefusedByAttemptsPreflight`, `TestMigration0107_Up_DuplicateLedgerPostings_RefusedByLedgerPreflight` | PASS. I read the assertions: the DB-backstop tests call `ledger.Post`/raw inserts directly, bypassing application code, and assert the typed sentinel `ledger.ErrDepositAlreadyPostedForIntent` / the unique-index violation — this is a real "remove/bypass the choke point" probe of the index itself, not a restatement of the app-level check. |
| O | T17/re-drive of a disputed second capture | `TestINVDEP1_O_ReDriveOfDisputedAttempt_NoPost`, `TestINVDEP1_O2_ReDriveOfDisputedAttempt_RealMultipleSuccessReason_NoPost` (migration_0107 file) | PASS |

Every row in A–O has at least one concrete, non-vacuous test, and all are in the green §3 run.
No row is missing a test.

Supporting rows also verified present and green: the A7 lock-order race tests
(`a7_1a_integration_test.go`, `a7_5c_integration_test.go`, `a7_lockorder_integration_test.go`) and
the F3b sweeper-evidence tests (`f3b_integration_test.go`) named in the task brief — see §7 item I.

## 5. Race repetitions (≥50, per §5's concurrency rows D and K)

Run standalone against a freshly rebuilt private DB (to avoid any residual contention from the
full-suite run):

```
go test -race -tags integration -run '^TestINVDEP1_D_ConcurrentOriginalAndFallbackSuccess_Race$' -count=50 ./internal/payments/...
ok   internal/payments 200.088s   EXIT 0

go test -race -tags integration -run '^TestINVDEP1_K_ConcurrentAdversarialOrderings_Race$' -count=50 ./internal/payments/...
ok   internal/payments 211.553s   EXIT 0
```

Each test itself loops 50 internal repetitions of the race scenario per invocation (`const reps =
50` in the test body), so `-count=50` on top of that ran 2,500 total adversarial race iterations
per test, all passing, 0 failures, 0 data races reported by `-race`.

## 6. TEST-T11A-FLIP-1 (commit `498559a`)

```
go test -race -tags integration -run '^TestWebhook_BadSignature_NoWriteBeforeVerification$' -count=100 ./internal/payments/...
ok   internal/payments 4.377s   EXIT 0
```
100/100 passes, 0 data races.

## 7. Final-gate items A–R

| # | Item | Verdict | Evidence |
|---|---|---|---|
| A | PAY-DOUBLE-CREDIT-1 closed | **PASS (substance), registry stale** | `ledger-finance` gave a final APPROVED sign-off (`rv-fh3-ledger.md` line 253: "Verdict: APPROVED. `ledger-finance` financial sign-off for FH-3 (PAY-DOUBLE-CREDIT-1) is GIVEN"), `security` APPROVE (`rv-fh3-security.md`), `code-reviewer` READY WITH CONDITIONS (only a Low, non-blocking residual) (`rv-fh3-code-review.md`). Human decision HD-LEDGER-UNALLOC-1 ("A now, B later") was made and is implemented per §28.13 of ADR 0095. The full A–O matrix is green (§4). **However**, `docs/governance/task-registry.md`'s `PAY-DOUBLE-CREDIT-1` row (line 4009) still reads "OPEN — HIGH ... implementation HALTED pending human approval (2026-09-27)", and ADR 0095's own status table (lines 4, 9) still says "§28 INV-DEP-1 NOT IMPLEMENTED". Both are stale relative to code/tests/reviews and should be corrected by their owners (architect/orchestrator) before this is presented as unconditionally closed. |
| B | ADR 0095 amended | **PASS** | §28 (AM-2, INV-DEP-1) and §29 (F-POOL-2 durable states) exist with full transition tables, choke-point design, migration 0107 design, reconciliation kind, HD-LEDGER-UNALLOC-1 record (§28.13). |
| C | LF-Q1 superseded appropriately | **PASS** | `lf-q1-supersession.md` records the ledger-finance ruling; ADR 0095's own table (§19.2 LF-Q1 row, and §21.2 T13 row) is marked `[SUPERSEDED for the multiple-success case by §28.7]` in place, not silently rewritten. |
| D | F-POOL-2 updated and implemented | **PASS** | §29 durable-state table maps every human-named state onto existing schema; INV-DEP-1 enforcement built on top of it (choke point in `postDepositSuccess`, migration 0107). |
| E | A–O adversarial matrix green | **PASS** | §4 above — every row has a real, non-vacuous test, all green. |
| F | PAY-SEC-S-H1 closed | **PASS (code/tests), registry stale** | `ApplyDeferredReceiptsForAttempt` now derives its `event_type` filter from `attempt.Operation` (`internal/payments/receipt.go` ~line 1514-1530, read directly); `TestRVLF_N3_DeferredApplyNeverReplaysADepositDeclineAsPayoutEvidence` kills the reverted-filter mutant. `rv-prh-i1-payout-security.md` verdict: "CLOSED". Registry row `PAY-SEC-S-H1` (task-registry.md line 4011) still says "OPEN — HIGH"; stale. |
| G | PAY-SEC-S-M1 closed | **PASS (code/tests), registry stale** | `TestRVLF_SM10_PayoutSuccessProviderReferenceMismatchDisputes` (`internal/payments/rvlf_i1_regression_integration_test.go:1739`) asserts a payout success with a different provider reference goes to `disputed` (T10 `provider_reference_mismatch`), never settles, matching QueryStatus's N6 rule; mutation-killed per `prh-i1-mutation-kill.txt`. `rv-prh-i1-payout-security.md`/`rv-prh-i1-callback-ledger.md` verdicts: "CLOSED". Registry row `PAY-SEC-S-M1` (line 4012) still says "OPEN — MEDIUM"; stale. |
| H | Payout security findings completed or explicitly escalated | **PASS** | `rv-prh-i1-payout-security.md` verdict: "APPROVED WITH CONDITIONS ... S-H1 and S-M1 closed. The remaining conditions are test pinning ... No finding blocks it." The three residuals (MA, MM1b, MRF unpinned) are explicitly recorded as LOW, non-blocking, with a concrete pinning task each — not silently dropped. |
| I | A7 lock-order tests complete | **PASS** | `rv-a7-tests.md`, final section ("A7 closure review (FH-3c, `92f5889`), by `ledger-finance`"): "A7 status ruling: **IMPLEMENTED**. All eight §(7) required tests are present and substantive ... #1a, #1b, #2 (with A7-C1), #3, #4, #5a, #5b, #5c." Tests found at HEAD: `TestA7_1a_SweeperClaimVsCallbackPhaseC_SameDepositIntent`, `TestA7_1b_SweeperClaimVsCallbackPhaseC_SameWithdrawal`, `TestA7_3_DeferredReceiptAppliedVsFreshCallback_SameAttempt`, `TestA7_4_N1_SweeperDepositReclaimVsSelfExclusion_SamePerson`, `TestA7_5a_SweeperDepositClaim_RGGateBlocksBeforeParentLock`, `TestA7_5b_ClaimBatch_SkipLockedNeverWaitsOnALockedAttemptRow`, `TestA7_5c_TombstoneBranch_SecondIdenticalReversalWaitsOnReceiptInsert` — all in the green `internal/payments` run (§3). Note: ADR 0095's own §(7)/status text for A7 was not independently re-checked word-for-word against "IMPLEMENTED" in this pass beyond the ledger-finance ruling; that edit is recorded as the ADR editor's remaining action in `rv-a7-tests.md` itself. |
| J | Kill-switch phase 2 reviewed and safely integrated | **PASS** | `qa-killswitch-phase2-verification.md`: full suite green, static checks clean, named mutant killed, verdict "IMPLEMENTED from a testing-verification standpoint". `killswitch-phase2-adr-notes.md`: independent code review + security review both confirm the branch touches no success/credit/decline-posting/cascade path, and record the merge-ordering rationale (phase 2 before PAY-DOUBLE-CREDIT-1) with explicit conditions for whichever lands second. |
| K | KYC/rate-limit/provider-reference work synchronized with roadmap, not lost | **PASS** | Task registry carries live, owned, non-orphaned rows for `PROVIDER-REF-BOUND-1`/PRH-REF (IMPLEMENTED, code+tests, gate review pending — tracked), `PROV-OUTBOUND-CRED-1` (PARTIALLY IMPLEMENTED, explicit remaining scope), `PROV-OUTBOUND-CRED-1-LEGACY-PATH` (registered, owner and trigger condition stated), `PRH-I4`/webhook rate limiting (PARTIALLY IMPLEMENTED, explicit residual `PRH-I4-METRICS-1`), `KYC-REVIEWREQ-FORWARD-1` (ruled and implemented, tests named), `RL-F1` (CLOSED). Nothing found abandoned without a registry row and owner. |
| L | Governance incident documented | **PASS** | `docs/governance/incident-2026-09-27-local-db-credential-mutation.md` exists, records actors, actions, recovery, root cause; status RESOLVED. |
| M | CLAUDE.md safety rule preserved | **PASS** | `CLAUDE.md` § Environment safety, "No sub-agent may alter shared database roles..." present verbatim (confirmed by direct read of this worktree's `CLAUDE.md`, line ~175). |
| N | All specialist reviews complete | **PASS, with the two stale-registry notes above (F/G) as the only gap** | Ledger-finance, security, code-reviewer, and product-owner-proxy review records all present and dated 2026-09-27/28 for every workstream examined (PAY-DOUBLE-CREDIT-1, kill-switch phase 2, payout security, A7, KYC). `rv-fh7-product-owner-proxy.md` verdict: APPROVE WITH CONDITIONS (its own conditions are exactly the F/G registry-staleness items already noted above, plus keeping KS-AUDIT-TENANT-1 on the pre-launch list, which it already is). |
| O | CI green once CI-BILLING-1 is resolved | **BLOCKED** | Per the task's explicit instruction, GitHub Actions is billing-blocked (every run fails in ~3s without starting). I did not attempt to invoke GitHub Actions and am not claiming a CI-green result. This item reports BLOCKED, never PASS, until CI-BILLING-1 is resolved. All the checks CI would run were reproduced locally in §1–§6 above and are green. |
| P | Working tree clean | **PASS** | `git status --short` empty before and after this review; no product code was modified. |
| Q | HEAD equals origin | **PASS** | HEAD `649f3ba32d8fbf6458fc4ddf3ebaaa434f17d4b3` == `origin/claude/focused-wright-jw88w9` at fetch time (confirmed by the `git reset --hard` itself pointing there; no new commits landed on origin during this review that I am aware of — this report's own commit will move this worktree one ahead of that origin ref, which is expected and is this deliverable). |
| R | No AWS changes | **PASS** | `git diff --stat 1560ad0^..HEAD -- deploy/aws '*.tf' '*.tfvars'` produced no output (empty diff). |

## 8. Summary

- Build/vet/fmt/lint: all clean.
- Migrations: verified clean through 0107, no gaps; 0107 up/down/up and preflight-refusal tests
  pass.
- Full integration suite (`-race -tags integration -count=1 -p 1`, timing lane excluded): **every
  package `ok`, 0 failures, 0 panics, 0 data races** (single clean run, after I corrected my own
  orphaned-process mistake — see §3).
- A–O adversarial matrix: all 15 rows mapped to real, substantive, passing tests; none vacuous.
- Race reps: D and K each passed `-race -count=50` (2,500 total adversarial iterations per test).
- TEST-T11A-FLIP-1: 100/100 under `-race`.
- Timing lane: not re-run, per instruction; evidence file `evidence/fh7-timing-lane-idle.txt`
  confirmed present, dated, and reporting 40/40 on `7c071c3` (this worktree's HEAD's direct
  ancestor).
- Final-gate items A–R: all PASS except **O, which is BLOCKED** (CI billing), and **A/F/G, which
  are PASS in substance (code, tests, and every specialist's own review record agree the work is
  done) but flag a real, outstanding hygiene gap**: `docs/governance/task-registry.md` still shows
  `PAY-DOUBLE-CREDIT-1`, `PAY-SEC-S-H1` and `PAY-SEC-S-M1` as OPEN/HALTED, which contradicts the
  ledger-finance/security/code-review sign-offs and the green test evidence. This is exactly the
  kind of gap QA is required to report rather than silently wave through: the orchestrator (not
  QA) should update these three registry rows to reflect their true, evidenced status before this
  block is presented externally as fully closed.
- No test was skipped, disabled, or quarantined by me. No product code was changed by me.

**Verdict: GATE PASS, except item O (CI-BILLING-1), which is BLOCKED, not PASS.** The three
stale-registry findings (A/F/G) do not block the gate — the underlying work is done and verified —
but must be corrected in the registry as a documentation/governance follow-up, not a re-opened
implementation task.

Branch: `claude/focused-wright-jw88w9`. Commit under review: `649f3ba32d8fbf6458fc4ddf3ebaaa434f17d4b3`.

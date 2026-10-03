# PRH-2 D2 — QA review

_Reviewer: `qa`. Branch `prh2-d2-recon-parked-capture` @ `0005aa7`. Recorded verbatim by the orchestrator._

D2 QA GATE VERDICT: PASS WITH CONDITIONS (prh2-d2-recon-parked-capture @ 0005aa7, base 564c515). It closes my C finding F2. No pre-merge blockers.

Commands run (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad; PRIV_DB=qa_d2_gate PRIV_SRC=$S/qa_d2 from `git archive 0005aa7`; pipefail; DB dropped at the end)
1. `git diff --stat 564c515 0005aa7 -- '*_test.go'`: only the new prh2_d2_parked_capture_integration_test.go (+686). No existing test file was touched, so the implementer's "no test changed" claim is confirmed.
2. `priv_test.sh -race -tags integration -count=1 -timeout 40m ./internal/reconciliation/...`
   - Result: `ok reconciliation 60.8s`, `ok statement`, exit=0, zero FAIL, panic or DATA RACE.
   - The existing INVDEP1_C5 and PaymentStatement tests stay green with the widened predicate.
3. `priv_test.sh -race -tags integration -count=10 -timeout 40m -run 'TestD2_' ./internal/reconciliation/`
   - Result: `ok 46.7s`, no FAIL, panic or DATA RACE.
4. My own mutant (c)7, in the scratch copy only: I reverted both `capturedUnpostedBoundReasons[a.terminalReason]` sites to `== "multiple_success_for_intent"` (2 replacements) and ran `-run TestD2_`.
   - Result: TestD2_1, TestD2_2 and TestD2_3 FAIL. That is killed, and (c)7 is satisfied.
   - The file was restored and verified byte-identical.

Do the tests meet LF (c) 1-7? Yes.
- (c)1: TestD2_1 covers sync_amount_mismatch, plus the D1 poll_amount_mismatch and poll_reference_mismatch reasons.
  - A bound-reference succeeded line gives exactly the expected kinds, using exact-set assertions (`d2Expect`).
  - That is pay_captured_unposted, plus pay_amount_mismatch where the amounts differ.
  - Control cases: pending, declined and reversed lines are not flagged.
- (c)2: TestD2_2 checks standing coverage with an empty wide window and with a past window that excludes the attempt. The finding is the no-line form keyed by the bound reference.
- (c)3: TestD2_3 covers clearing.
  - It clears on a reversal line naming the bound reference, or on a tombstone on that reference.
  - It does not clear on a reversal or tombstone on a different reference, or when investigation_status is set to resolved through ResolveMismatch.
  - M1 is NOT IMPLEMENTED (ADR 0101), so it is covered only structurally: the emission predicate reads no prior finding or resolution. That follows the ruling's own limit.
- (c)4: TestD2_4 covers the cases below.
  - Payout-bound conflict plus a merchant-resolved succeeded line is flagged, and the payout attempt is untouched. A pending line is not flagged, and a reversal line clears it.
  - A conflict bound to another deposit plus a second succeeded line gives exactly one pay_duplicate (check=duplicate_line), and exactly one provider deposit posting.
  - A defence-in-depth case: an unbound reason on an attempt that holds a reference is still flagged.
- (c)5: TestD2_5 covers both halves.
  - An invalid-reference line is refused with ErrPaymentStatementInvalid. Nothing is stored, no run is recorded, there is one audited P1 `sweep_run_failed`, and a P1 log line is emitted.
  - A merchant-resolved succeeded line is flagged, and the raw reference never reaches a mismatch row.
- (c)6: `d2AssertNoMoney` runs in every scenario.
  - Zero ledger transactions for the attempt, matched by intent correlation or by provider reference.
  - SUM(debits)==SUM(credits).
  - `RunLedgerVsProjection` returns 0 mismatches. This is an explicit check (`len(drift)==0`), not just a balanced-ledger check.
  - `d2Run` calls `assertOnlyRunWritten` around each match, proving the run writes only its run and mismatch rows (no ledger, projection, attempt or intent change).
- (c)7: satisfied (mutant above, plus the implementer's 17-of-17 kill record, which I read but did not rerun except for the main mutant).
- TestD2_6 additionally pins that the unchanged reasons (tombstone-precedes-success and unknown reasons) never produce pay_captured_unposted, in-run or standing.

Determinism (T-1/T-2): acceptable.
- There are no sleeps, goroutines or wall-clock assertions.
- `time.Now()` is used only to build statement coverage windows and the drift window, never in an assertion.
- Each subtest builds its own world with its own tenant, so there is no shared state between subtests, and map iteration order does not matter.
- It was stable across 10 iterations under -race.

Findings (all non-blocking)
- D2-F1 (LOW): the poll_amount_mismatch and poll_reference_mismatch cases use fixture T10s (`ApplyDisputeFromNonTerminal` with the plan's reason strings), because D1's code is not on this branch.
  - When D1 merges, add or confirm one end-to-end test where a real poll produces the park and recon reports it. If D1 spells the reason strings differently, TestD2 would stay green while production recon misses it, because recon matches on string literals.
  - Pin the strings with a shared-constant check, or a test in D1 that imports both.
- D2-F2 (LOW): the drift window is `now-1h..now`. The assertion is explicit, but with parked-only fixtures there is little to drift. The pay_duplicate case, which has a real posting, gives the only non-trivial drift coverage. Acceptable, since parks write no ledger rows (asserted).
- D2-F3 (LOW): the (c)3 M1 case is structural, not behavioural, because M1 does not exist yet. Re-test when ADR 0101 M1 lands.

My view on the deferral of standing coverage for unbound parks (PAY-RECON-PARKED-CAPTURE-STANDING-1): acceptable now, but it must carry a hard gate.
- The gap is real. For provider_reference_conflict and invalid_provider_reference, the capture is reported only in a run whose statement window contains a succeeded line resolving to the attempt by merchant reference.
- If the next run's window does not include that line, the finding silently disappears with no resolution. That is the "captured, not posted" exposure going quiet, the same class of problem as F2.
- This is acceptable only because these reasons are reachable solely against the MOCK adapter and the MOCK statement source. Standing coverage needs persisted-line evidence, which is a design item and not a matcher tweak.
- Conditions:
  - Register PAY-RECON-PARKED-CAPTURE-STANDING-1 with an owner (ledger-finance).
  - Make it a hard prerequisite to enabling any real PSP adapter or statement source, and to the first real-money tenant. Put it in HANDOVER next to the K2/MA020 follow-up.
  - Keep the "standing is NOT IMPLEMENTED (§35.4)" wording in the ADR and in the `payment_statement.go` disclosed-limits comment. The code already has both.
  - Keep the test that asserts the gap, `d2NoCU(..."an unbound park with no line this run")`. It will fail, correctly, when standing coverage is implemented, and must then be flipped.
- Label for this feature: bound reasons IMPLEMENTED; unbound-park standing coverage NOT IMPLEMENTED.

Relevant paths: /home/user/igaming-platform (untouched; branch prh2-d2-recon-parked-capture). The reviewed archive is $S/qa_d2. The files are internal/reconciliation/prh2_d2_parked_capture_integration_test.go, internal/reconciliation/payment_statement.go and docs/plans/payment-readiness/evidence/prh2-d2-mutation-kill.txt.

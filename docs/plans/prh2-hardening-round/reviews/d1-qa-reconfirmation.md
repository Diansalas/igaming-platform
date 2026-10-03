_Reviewer: `qa`. Branch `prh2-d1-poll-amount` @ `dc8130d`. Recorded verbatim by the orchestrator._

> **Orchestrator decision (2026-10-03):** the one low condition (strengthen `TestPollDeclinedAttempt_ContradictionAuditedMatchingPostsT13/matching_success_posts_under_the_bound_reference` so it proves the attempt was DECLINED when the poll reached phase C; QA's mutant B survives there) is made **pre-merge**, consistent with QA F1 and CR-5 (same weakness class, and it is the T13 second-capture subject). It is test-only.

D1 FORCED-TEST RE-CONFIRMATION at dc8130d: PASS WITH ONE LOW CONDITION. The fix is sound. You were right that my 788b873 claim was wrong. A hook-off mutant leaves every assertion true for "poll alone", so that test could pass without the interleaving. Please keep my 788b873 sentence "cannot pass vacuously" corrected as you did. My re-check of that reasoning at dc8130d follows.

Delta 788b873..dc8130d: only poll_amount_integration_test.go and the evidence file (`git diff --stat`). The scratch archive is $S/qa_d1c (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad).

1. Does the test now fail under "poll alone"? Yes.
   - Mutant A, hook off (`if true { return }` at the top of the onQuery closure): TestPollSuccess_CallbackCommitsDuringTheProviderCall_PollIsADuplicateNoOp FAILS ITSELF at line 507, "the forced callback never fired". The Cleanup guard also reports it at line 98.
   - Mutant B, the stronger variant that matters: the hook fires and closes `fired` but the callback is NOT applied (rvCallback skipped). The forced test fails at line 513, `last_evidence_kind="query_status", want "callback"`.
   - This shows the assertions on last_evidence_kind and on the one applied receipt discriminate "callback first" from "poll alone". Under the poll alone, the evidence kind would be query_status and there would be no receipt. The `fired` channel only proves the hook ran, and the last_evidence_kind check proves the callback applied before phase C.
   - Files were restored and verified byte-identical after each mutant.
2. Can the Cleanup guard give a false failure? I found no such case.
   - There are 8 call sites, in F3SM (x2), the declined tests (x2), the noise test (a 4-case loop, including the transport-failure case), the raw-echo test (x2 subtests) and the declined-Missing test. In each, a sweeper poll calls QueryStatus and the hook runs before the result or error is returned, so it always fires.
   - The guard fails only if the hook never fires, which is exactly the broken-precondition case. It fires in all the green runs below.
3. Stability:
   - `-race -tags integration -count=10 -timeout 40m -p 1 -run 'TestPollSuccess_|TestF3SM|TestSweepCASNoise|TestPollDeclinedAttempt|TestPollAmount|TestDeferredReceipt|TestDepRef' ./internal/payments/`: `ok 57.432s`, no FAIL, panic or DATA RACE.
   - Full `-race -tags integration -count=1 -timeout 40m -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`: `ok 453.505s`, exit=0, zero FAIL, panic or DATA RACE.
4. Same weakness elsewhere: one more instance. I ran mutant B (the hook fires but the callback is not applied) across all hook-based tests.
   - Killed: the forced test, both F3SM tests, the raw-echo test (both subtests), the declined contradicting subtest, the declined-Missing test, and all 4 noise cases.
   - SURVIVOR (LOW): TestPollDeclinedAttempt_ContradictionAuditedMatchingPostsT13/matching_success_posts_under_the_bound_reference (poll_amount_integration_test.go:~679-691, `deliverCallbackOnce(Declined)` then `pollSuccess("", 5000, "EUR")`).
     - If the declined callback is not applied, the poll alone succeeds and posts under the bound reference. The assertions (state succeeded, bound reference, 1 posting, balance 5000) hold for that path too.
     - So it does not prove the attempt was declined when the poll reached phase C, which is the T13 subject.
     - The Cleanup guard catches hook-off but not "fired, nothing applied". The gap needs a callback that returns nil but does nothing, which is unlikely today (the `t.Errorf("interleaved callback")` branch catches an error).
     - Fix (small, optional but recommended): in that subtest, assert the callback's effect, for example that exactly one `payment_provider_events` row exists for the reference with `disposition_at_receipt='applied'` plus a declined-at-callback marker, and/or capture `deliverCallbackOnce`'s `fired` channel and assert that attempt.State was declined before the poll. Alternatively read the declined state inside the hook, after the callback returns.
   - The two phase-B tests in C (TestDepRef_SyncSuccess_CallbackDuringPhaseB and phaseBCallbackThenAmbiguous) assert `deferred_unresolved` on the callback disposition, so they are not subject to this weakness.

Condition (non-blocking): strengthen the T13 matching subtest as above, or record the residual. Nothing else in D1 or C shows this pattern.

Relevant paths: /home/user/igaming-platform (untouched; branch prh2-d1-poll-amount). The reviewed archive is $S/qa_d1c, file internal/payments/poll_amount_integration_test.go (`deliverCallbackOnce` at line 90; forced test at 493).

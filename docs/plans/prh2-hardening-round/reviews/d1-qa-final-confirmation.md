_Reviewer: `qa`. Branch `prh2-d1-poll-amount` @ `54a55e3`. Recorded verbatim by the orchestrator._

D1 FINAL CONFIRMATION at 54a55e3: PASS. The last condition from my dc8130d review is closed. I have no remaining conditions on D1. I did not re-run the full mutation battery; I re-ran only mutant B, as you asked.

Delta dc8130d..54a55e3 (`git diff --stat`): poll_amount_integration_test.go (+15 lines) and the evidence file only. The scratch archive is $S/qa_d1d (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad).

Mutant B re-applied (the hook fires and closes `fired`, but rvCallback is skipped), run with `-run 'TestPollSuccess_Callback|TestF3SM|TestSweepCASNoise|TestPollDeclinedAttempt'`:
- The T13 matching subtest TestPollDeclinedAttempt_ContradictionAuditedMatchingPostsT13/matching_success_posts_under_the_bound_reference now FAILS ITSELF, at line 697: `applied declined-callback receipts = 0, want exactly 1 (the attempt must have been declined when the poll reached phase C)`. This is the survivor from my last review; it is now killed.
- Every other hook-based test also FAILS. That is all 7 test functions, with nothing surviving:
  - the forced test;
  - both F3SM tests plus the raw-echo test (invalid_echo and valid_different_echo);
  - the declined contradicting subtest and the declined-Missing test;
  - all 4 noise cases.
- The file was restored and verified byte-identical.

New in-hook state read, deadlock and false-failure check:
- The hook is called synchronously from QueryStatus on the test goroutine, because the sweeper in RunOnce spawns no goroutines. The `depScan` read opens its own fresh tenant tx, which does not wait on the poll. At that point the poll holds no transaction (phase B), so there is nothing to deadlock against.
- `depScan` calling `t.Fatalf` is therefore also on the test goroutine.
- The query is keyed on tenant plus reference. It is single-row (an ambiguous bound attempt), so it is not a false-failure risk.
- Stability: `-race -tags integration -count=10 -timeout 40m -p 1 -run 'TestPollSuccess_|TestF3SM|TestSweepCASNoise|TestPollDeclinedAttempt|TestPollAmount|TestDeferredReceipt|TestDepRef' ./internal/payments/`: `ok 58.319s`, no FAIL, panic or DATA RACE.
- Full `-race -tags integration -count=1 -timeout 40m -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`: `ok 468.893s`, exit=0, zero FAIL, panic or DATA RACE.

Remaining "assertions hold for the alternative path too" weakness: none that I can find.
- `deliverCallbackOnce` is the only user of `onQuery` among the poll tests. It now has three guards: the Cleanup that fails if the hook never fires, an in-hook state assertion, and the `fired` channel checked by the forced test.
- The C phase-B tests use a callback inside the Deposit script and assert the callback's `deferred_unresolved` disposition and the deferred-receipt resolution. Those assertions hold only if the callback actually ran.
- The goroutine race test is an explicit smoke test and is labelled as one.

Relevant paths: /home/user/igaming-platform (untouched; branch prh2-d1-poll-amount). The reviewed archive is $S/qa_d1d, file internal/payments/poll_amount_integration_test.go (`deliverCallbackOnce` at line 90, T13 subtest at ~690).

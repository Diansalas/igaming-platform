# QA hand-back (verbatim summary): D2 completion at 682fd5b, F-pay gate at dfc5df2

Source: qa subagent final report (model output, not user input).

## D2 at 682fd5b: PASS
- `-race -tags integration -count=1 -p 1 -run TestD2_ ./internal/reconciliation/`: ok 8.5s.
- D2F1 mutant (captureClass reverted to `c == reasonBoundIfReferenced`): TestD2_P1_RuntimeRule + TestD2_15 (3 subtests) FAIL (killed).
- P1-table mutant (reversal_tombstone_precedes_success -> reasonBound): TestD2_P1_Every..., TestD2_7/r1, TestD2_6 FAIL (killed).
- TestD2_15 setup probes (callback not applied; attempt holding stored reference): both fail in setup, so no vacuous pass.
- Carried: PAY-RECON-PARKED-CAPTURE-STANDING-1 remains a hard prerequisite before any real PSP (not a gate for this branch).

## F-pay at dfc5df2: PASS WITH CONDITIONS (no pre-merge blockers)
- payments ok 412s, withdrawal ok, httpserver KYC/Withdraw/Payout ok, -count=10 new set ok; 6/6 mutants killed (MU-6b, MU-6, MU-2, MU-9, MB1, MB6).
- Outage tests are forced via LOCK TABLE + lock_timeout, no sleeps; no vacuity; no existing test modified.
- F1 LOW: assert exactly-one attempt after HTTP retry + double-submit-after-outage test. F2 LOW: T12 outage test should assert next_action_at moved. F3 LOW/info: one `unavailable` decision row per evaluation per tick during sustained outage, no volume bound; add retention/rate note (tracked in PAY-FPAY-HARDENING-1). F4 INFO: allow-path row covered by MU-7.

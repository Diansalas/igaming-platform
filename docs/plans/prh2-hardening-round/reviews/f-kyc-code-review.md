# Code review — PRH-2 F-kyc (2026-09-28)

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** commit `df73606` (`prh2-f-kyc-outage-testpins`, based on `cabca27`). Security's
`f-kyc-security.md` was read, and its C-F1, F-2 and F-3 are not repeated here.

## Verdict: READY WITH CONDITIONS

- The savepoint fix is correct.
- The fault-injection tests use a real lock handshake, with no sleeps.
- No test was loosened: across 7 files the diff adds 713 lines and removes 1, a comment.
- MB1, MB6 and MPLAYREC (casino and sportsbook) are killed again, and so are 5 of the reviewer's 6 extra mutants.

## Findings

| # | Sev | Finding | Required / recommended |
|---|---|---|---|
| FK-1 | Low (condition) | ADR 0096 §16.2 and §23.1 mark **LF-I3-5 "CLOSED"**, but no committed test covers the play path. LF-I3-5 names two cases: `EvaluateEnforcement` **or** `RecordDecision` returning an error. The savepoint fixes only the first. A `RecordDecision` failure inside `postBet`/`PlaceBet` still rolls back the whole bet transaction. A scratch probe (`LOCK TABLE kyc_verifications` + `lock_timeout` on the casino callback) shows the evaluate-error half works at this commit, and mutants X1/X2 turn it red, but nothing in the repo pins it. | Commit a play-path outage test for casino and sportsbook. Reword to "evaluate-error half CLOSED; RecordDecision-error half remains". |
| FK-2 | Low (condition) | §23.1 points to "§23.4's fault-injection tests" twice; they are in §23.5. | Fix the cross-references. |
| FK-3 | Low | `TestRequestWithdrawalHandler_KYCStoreOutageReturns503` seeds all its fixtures through the `lock_timeout=300ms` pool. In parallel CI on one DB, a fixture statement could wait more than 300 ms behind another package's TRUNCATE and fail with 55P03. | Seed with the normal pool, and use the lock-timeout pool only for `newFinancialTestServer`. |
| FK-4 | Info | The fault-injection tests assert `Outcome=unavailable` but not the code. | Also assert `Code == "kyc_unavailable:verification_lookup_failed"`. |
| FK-5 | Info | Mutant X5 survives: the housekeeping-failure branch was changed to `return decision, nil`, which fails open. No fault injection can reach that branch. | Optional: a unit test with a fake `pgx.Tx` whose `Rollback` fails. |
| FK-6 | Info | In the B6 and N5 tests, the "no domain effect" post-checks always hold whenever the call errors (the transaction rolls back); the kill comes entirely from `errors.Is`. | No change needed. |

## Mutants

| Mutant | Result | Killed by |
|---|---|---|
| MB1 (the 503 branch off) | KILLED | `TestRequestWithdrawalHandler_KYCStoreOutageReturns503` |
| MB6 (the Allowed guard off) | KILLED | `TestDenyForCompliance_RefusesAllowedDecision` |
| MPLAYREC casino | KILLED | `TestReceiveCallback_BetDeniedByKYCPlayPolicy_NoLedgerEffect` |
| MPLAYREC sportsbook | KILLED | `TestPlaceBet_DeniedByKYCPlayPolicy_NoLedgerEffect` |
| X1 (savepoint bypassed) | KILLED | both outage tests |
| X2 (RELEASE instead of ROLLBACK) | KILLED | both outage tests |
| X3 (the N5 guard off) | KILLED | `TestDenyForCompliance_RefusesUnavailableOutcome` |
| X4 (the B4 filter removed from the play half) | KILLED | `ExcludesNonActiveTenants/{suspended,closed}` |
| X6 (the filter changed to `status <> 'closed'`) | KILLED | `ExcludesNonActiveTenants/suspended` |
| X5 (housekeeping failure fails open) | **SURVIVED** | FK-5 |

## Verification (local, private DB `cr_fkyc_0928`, dropped)

- build, vet (both tag sets), gofmt, and golangci-lint 2.9.0 with 0 issues;
- `-race -tags integration`: kyc, withdrawal, casino, sportsbook and db all ok;
- httpserver `-run 'KYC|Kyc|Withdrawal'` with `-race -p 1`: ok;
- the two `LOCK TABLE` tests alone, `-p 1`, 3 runs each: all PASS.

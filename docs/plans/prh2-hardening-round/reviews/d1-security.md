_Reviewer: `security`. Branch `prh2-d1-poll-amount` @ `07aa48d`. Recorded verbatim by the orchestrator._

> **Orchestrator note (2026-10-03):** this reviewer stopped before the mutant re-kills because free disk fell below the 500M floor (root at 99%, shared Go build cache about 14G). Their re-kill is OPEN and pre-merge, or must be recorded as an explicit exception by the user. The implementer's 29/31 mutant evidence is not independent verification. No shared cache or directory was cleaned (cleanup is not authorized).

Security review — PRH-2 D1 (prh2-d1-poll-amount @ 07aa48d; diff 564c515..07aa48d)

VERDICT: ACCEPT WITH CONDITIONS. There are 2 pre-merge findings, both proven by execution.

STOP NOTICE (per your disk rule): free disk space fell to 454M after the full payments run, below your 500M floor. I STOPPED before the mutant re-kills and did not retry. I then dropped my private DB (sec_d1_rv_20261003; verified 0 left) and deleted my export and logs. Free space after cleanup is 507M. The space was consumed by the go build/test cache during the runs; I did not touch any shared cache or directory.

The three requested mutants (echo-as-posting-key, tombstone-uses-echo, invalid-echo-leaked-into-audit) are therefore NOT RE-KILLED by me. I could not verify those three claims in prh2-d1-mutation-kill.txt independently.

What did run, with `-p 1` and one build, against the fresh private DB:
- go build and go vet (payments, ledger): clean.
- internal/ledger, txscope (TestPCG1, INV-IO-1(c)) and providerref: ok.
- full internal/payments `-count=1 -tags integration -p 1`: ok (402s). That includes the A7 lock-order tests, the D1 poll tests and the fault-injection tests.
- Not run under `-race`, because of the disk constraint.
- 6 execution probes (scratch-only file, deleted).
- No role or credential changes, no sudo.

------------------------------------------------------------------
PRE-MERGE

D1-F1 (Medium, proven): the poll DECLINE branch still binds the raw, unvalidated poll echo, onto the intent and (via COALESCE) onto the attempt. That is exactly the defect D1 fixed in the Pending branch.
- Where: sweeper.go, `case ErrorClassDefiniteDecline` (~567-591). `refPtr = &res.ProviderReference` is passed to finalizeDeclined, which calls setIntentAttempt (orchestrator.go:469). That function OVERWRITES deposit_intents.provider_reference unless the intent has succeeded. It also passes refPtr to ApplyDecline, which COALESCEs it onto the attempt.
- Probes (a live ambiguous attempt bound to reference R; the poll returns Declined with an echo E different from R):
  - E is a fresh valid string: the attempt is declined, and the intent's provider_reference is silently repointed from R to E. The "which provider holds this player's money" record is corrupted by a provider-supplied value that no LF-6 check ever saw.
  - E is another intent's bound reference: 23505 unique violation; the sweep errors and rolls back; the attempt stays ambiguous. It recurs on every pass: an error loop.
  - E is invalid (a control character): the deposit_intents CHECK fails, with the same error loop.
- Fix: as in D1's Pending branch, use the attempt's BOUND reference whenever one exists (`boundRef := *attempt.ProviderReference`), for both finalizeDeclined and DeclineEvidence.ProviderRef. Treat a non-empty, different echo as audit-only, recording the echo only if providerref.Validate passes, otherwise its length and hash prefix.
- Tests: add the three probe cases (fresh, foreign, invalid echo on Decline), asserting no sweep error, the intent reference unchanged, and the attempt declined.

D1-F2 (Medium, proven): a raw, unvalidated poll echo is written to audit, contradicting D1's own stated rule ("audited only when it is itself a valid reference").
- Where: sweeper.go, `case AttemptSucceeded` inside ErrorClassSucceeded (~490-498):
  `auditTerminalAmountAssetMismatch(..., ReceiptEvidence{ProviderReference: res.ProviderReference, ...})`.
  That stores metadata.provider_reference = the raw echo.
- Probe: a succeeded attempt, then a mismatched poll (4999) whose echo is "bad\x01" plus 300×"Z". Result: the audit row `payments.callback_amount_asset_mismatch_terminal` holds provider_reference with length 304, including the control character.
- Fix: pass the bound reference as provider_reference, and add an `echoed_provider_reference` only when providerref.Validate passes. Otherwise add echo_ref_reason, echo_ref_len and echo_ref_sha256_prefix, exactly as poll_evidence.go step 2 does.
- Test: extend TestF3SM_MismatchedPollAgainstAnAlreadySucceededAttempt with an invalid echo, asserting the raw value is absent from audit_log.

------------------------------------------------------------------
SHOULD FIX (Low, before real-provider use; not pre-merge)

D1-F3: raw, unbounded asset echo in audit.
- Probe: a live attempt polled with AssetCode = "EUR\x01" plus 4000×"A". This is a Mismatch (correct, it parks), but metadata.provider_asset_code is stored as 4004 bytes including the control character.
- The same raw field appears in poll_evidence.go (the poll_amount_mismatch extra and the poll_amount_unconfirmed metadata `provider_asset_code`), in auditTerminalAmountAssetMismatch (`echoed_asset_code`), and in C's sync_amount_mismatch park.
- jsonb escapes it safely, so this is not injection, but it is unbounded audit growth driven by provider data.
- Fix: record the asset echo only if it matches the asset-code format (e.g. at most 16 bytes, [A-Z0-9_-]); otherwise record its length and hash prefix.

------------------------------------------------------------------
Focus items
1. The success path uses the BOUND reference everywhere (checkPollSuccessEvidence, foreignReferenceBinding, tombstoneExists, postDepositSuccessOrDispute, ApplySuccess). A reference-less attempt returns an error rather than deriving a key from the echo, and the echo is never the posting or tombstone key on that path, by code reading. The decline path is the exception (D1-F1). The echo is not validated at the QueryStatus adapter closure (sweeper.go ~352-366). That is acceptable only if every consumer avoids using it raw, which D1-F1 and D1-F2 show is not yet true.
2. MISSING-ON-POLL (§36.2): ACCEPTED as money-safe. It never posts, never disputes, and the attempt stays live with backoff.
   - Abuse surface: a provider that never echoes an amount costs up to about 48 audit rows per attempt per day, with no terminal escalation. That is an availability and audit-volume concern, not a money one.
   - Condition before real-provider use: PAY-DEPOSIT-ESCALATION-1 (T16), or a cap or alert after N unconfirmed polls. The cap would bound audit growth and surface a never-confirming provider.
3. foreignReferenceBinding's new ledger_transactions check: SAFE. It has an explicit tenant_id predicate under WithTenant with FORCE RLS, so there is no cross-tenant read. It excludes tombstones, which keep their own T10. It parks instead of looping. There is no oracle, since the reference is provider-generated.
4. ledger.Post with an empty ProviderTxID: SAFE and fail-closed. prepareEntries returns the typed ErrInvalidEntry before any write. The empty_provider_tx_id_integration_test passed in the ledger suite.
5. Fault injection: by code reading, holdTableShare and pollVictim use LOCK TABLE plus SET lock_timeout on the test's own pool connection. There is no SET ROLE, superuser, ALTER ROLE or privileged escalation. LOCK TABLE needs only table privileges the owner role already has, and the tests passed.
6. Deferred-receipt draining at sync success (drive.go ~602-613) and poll success (sweeper.go ~560-566): it runs in the attempt's own WithTenant transaction under the held intent lock, keyed by the attempt's bound (provider_id, reference). There is no wrong-tenant path. The no-second-posting and resolution tests (TestDeferredReceipt_*) pass.
7. Behaviour changes:
   - Pending no longer writes the echo over a bound reference: correct.
   - Declined-attempt contradictions are audit-only: accepted (§4.4).
   - The poll tombstone T10 goes through parkDepositAttempt: accepted, giving the same audit and intent recompute as the other parks.
   - The new early return for non-success polls on terminal attempts (sweeper.go ~426) is fine.
8. C's pending-only redirect rule: NOT affected. D1 adds no player-facing return path; the sweeper never returns a redirect or token.
9. TestPCG1 and INV-IO-1(c) (txscope) are green. The A7 lock-order tests are green inside the full payments run (non-race only).

Outstanding because of the disk stop: re-run the three requested mutants (echo-as-posting-key, tombstone-uses-echo, invalid-echo-leaked-into-audit) and a `-race` pass, once disk space is restored. Re-killing the invalid-echo mutant is less important now that D1-F2 shows a separate raw-echo audit path that no test covers.

Relevant paths (commit 07aa48d):
- internal/payments/sweeper.go (Decline branch ~567-591; succeeded-terminal audit ~490-498; QueryStatus closure ~352-366)
- internal/payments/poll_evidence.go (steps 1-2 audit metadata)
- internal/payments/orchestrator.go (setIntentAttempt, line 469)
- internal/payments/attempt.go (ApplyDecline COALESCE, around line 694)
- internal/payments/drive.go (foreignReferenceBinding ledger check; deferred drain)
- internal/ledger/lockorder.go (empty ProviderTxID guard, around line 352)

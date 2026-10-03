# PRH-2 C — Security review

_Reviewer: `security`. Branch `prh2-c-dep-ref-validate` @ `fc0d18b`. Recorded verbatim by the orchestrator._

Security review — PRH-2 C (PAY-DEP-REF-VALIDATE-1, INVDEP1-BACKSTOP-BRANCH-TEST-1, KS-CAS-DISCRIM-TEST-1); branch prh2-c-dep-ref-validate @ fc0d18b, base c458bb8, no migration

VERDICT: ACCEPT WITH CONDITIONS. There is 1 pre-merge finding (C-1, High, proven by execution). The rest of the design holds.

Method:
- git archive of fc0d18b. build and vet clean.
- Fresh private DB sec_c_rv_20261003; DB access worked. Dropped WITH (FORCE); verified 0 left. No role or credential changes, no sudo, pipefail on, grepped for FAIL.
- `-race -count=1 -tags integration -p 1` on internal/payments (494s), internal/providerref and internal/txscope: all ok. That includes TestPCG1 (payment_provider_call_guard_static_test.go), INV-IO-1(c) (no_provider_call_in_tx_closure_static_test.go) and the A7 lock-order suites (a7_*).
- 1 execution probe and 2 Go mutants (scratch copy only, reverted and cmp-verified). Scratch copy removed.

------------------------------------------------------------------
PRE-MERGE

C-1 (High, proven): a deposit parked for a provider-reference CONFLICT (LF-6), or for a sync amount mismatch (LF-5), still hands the provider's redirect URL or hosted-field token to the player.
- Only the invalid-reference park is scrubbed, and that happens inside depositAdapterCall (drive.go:288 returns DepositResult{Outcome}).
- The conflict and mismatch parks are decided later, in phase C (drive.go:438-446 and :478-486). By then the caller has already copied the redirect and token out of the unscrubbed result:
  - deposit_v2.go:304: `redirectURL, hostedFieldToken := gr.Value.RedirectURL, gr.Value.HostedFieldToken`, returned unconditionally at :353;
  - drive.go:208, returned at :244 (the cascade/sweeper driver).
- deposit_handlers.go:88-89 writes them to the HTTP response.
- Probe (same tenant and provider; intent 1 binds reference R; for intent 2 the adapter returns Pending with R plus a redirect):
  - second attempt: disputed / provider_reference_conflict, correctly parked;
  - InitiateDepositAttemptResult.RedirectURL = "https://mock-psp.invalid/pay/x", which is returned to the player.
- Failure scenario: the player is sent to pay into a PSP session whose reference the platform has bound to a DIFFERENT attempt or intent, possibly another player's deposit in the same tenant. The provider's later callback for R is applied to the attempt that owns R. Player B's money is captured against player A's deposit, while B's own attempt sits disputed. That is exactly the cross-binding LF-6 exists to prevent, re-opened through the response.
- For sync_amount_mismatch the exposure is smaller (a sync success rarely carries a redirect), but the same rule applies: nothing from a parked response goes to the player.
- Fix:
  - have applyDepositCallResult return a "parked" signal, or check the post-phase-C attempt state;
  - in both InitiateDepositAttempt (deposit_v2.go:304/353) and driveCreatedAttempt (drive.go:208/244), return an empty redirect and token when the attempt was parked (disputed);
  - better: build the player-facing redirect only for an attempt left in the accepted/pending state.
- Tests: add `res.RedirectURL == "" && res.HostedFieldToken == ""` to assertParkedNoMoney and to every TestDepRefConflict_* and TestDepSyncAmount_Mismatch* case. Include a pending-with-redirect case for conflict, and a cascade-driven case through driveCreatedAttempt.
- The existing "NoRedirect" assertion covers only the invalid-reference park, which is why the 19-mutant set did not catch this.

------------------------------------------------------------------
Confirmed (no finding)
- C1 reference validation: the reference is validated before the outcome switch. providerref.Validate applies on Pending, so an empty reference parks (S-9); ValidateOptional applies on every other outcome. Any failure means ErrorClassProviderRefInvalid, then a T10 park via parkDepositAttempt.
- Scrubbing on that path:
  - The result is reduced to {Outcome}.
  - providerref.Error's string and LogAttrs carry only field, reason, length and a sha256 prefix, never the value (providerref.go:76-86).
  - The audit metadata holds ref_field, ref_reason, ref_len and ref_sha256_prefix only.
  - The reference is not bound to the attempt (ApplyDisputeFromNonTerminal does not set provider_reference).
  - My mutant (scrub removed: `return res, ErrorClassProviderRefInvalid, verr`) is KILLED by TestDepRef_InvalidReferenceOnAnyOutcomeParks_NoPersistenceNoRedirect, in 4 subtests.
- The conflict and mismatch parks record the VALIDATED provider_reference, plus the provider's amount and asset for a mismatch, in the audit row. Acceptable: these are validated values the platform stores routinely on bound attempts, and staff need them to triage the dispute.
- LF-6 foreignReferenceBinding (drive.go ~368-402):
  - No cross-tenant read. It has an explicit tenant_id predicate and runs under WithTenant with FORCE RLS on payment_attempts and deposit_intents.
  - My mutant (tenant predicates replaced with a no-op) passes TestDepRefConflict_CrossTenantSameStringHasNoEffectOnTheOtherTenant. That makes it equivalent under RLS, so the explicit predicate is defence in depth: keep it.
  - No oracle. References are provider-generated, not player-chosen, and the player-visible outcome of a conflict park is the same "ambiguous" status as any other ambiguous deposit, once C-1 removes the redirect difference.
  - No error loop. A conflict parks, returns nil, and the transaction commits. Only a genuine DB error propagates.
  - It runs under the intent lock and before any binding write.
- Every new T10 (invalid_provider_reference:*, sync_amount_mismatch, provider_reference_conflict) produces no ledger transaction. assertParkedNoMoney checks a ledger tx count of 0, a balance of 0, a balanced ledger, an ambiguous intent, no cascade and no next_action_at; the conflict tests check the same.
- LF-5 CompareProviderAmount: a missing echo means Ambiguous (poll decides, no posting, no dispute); a mismatch, including a negative amount, means T10 with no posting. The check runs before the tombstone/posting branch. The mock now echoes on every outcome.
- TestPCG1, INV-IO-1(c) and the A7 lock-order tests are intact and pass.

------------------------------------------------------------------
Implementer-disclosed risks, ruled
- New T10 reasons write an audit row but no P1 log line: ACCEPT FOR MERGE, CONDITION BEFORE REAL-PROVIDER USE (Medium). sync_amount_mismatch means the provider says it captured money in a different amount; provider_reference_conflict means a possibly captured payment is unbound. Both need durable P1 visibility (an ADR 0102 alert, or at least the same P1 log line the existing T10s emit), not just an audit row. Track it with the I-wire P1 routing.
- A decline carrying a conflicting reference now parks: ACCEPTED. Fail-safe, since a decline claiming another attempt's reference is anomalous. The intent shows ambiguous instead of declined, and nothing posts.
- The callback-vs-sync race test is probabilistic (5 iterations): ACCEPTED. Exactly-once posting is guaranteed structurally by the ledger idempotency key and the attempt CAS under the intent lock, not by the test. The test is supporting evidence only.

Scope: code- and DB-level review of the C delta; no penetration test. The ADR 0095 §34 text was read for consistency only. The mutation-evidence count (19) was not re-verified beyond my two mutants.

Relevant paths (commit fc0d18b):
- internal/payments/drive.go (depositAdapterCall ~271-305; parkDepositAttempt; foreignReferenceBinding; applyDepositCallResult ~420-487; lines 208 and 244)
- internal/payments/deposit_v2.go (lines 304 and 353)
- internal/httpserver/deposit_handlers.go (lines 88-89)
- internal/payments/dep_ref_validate_integration_test.go (assertParkedNoMoney ~147; TestDepRefConflict_* ~401-583)
- internal/providerref/providerref.go (lines 76-86)

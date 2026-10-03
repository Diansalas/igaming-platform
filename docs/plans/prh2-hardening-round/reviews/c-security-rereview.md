_Reviewer: `security`. Recorded verbatim by the orchestrator._

Security re-review — PRH-2 C (prh2-c-dep-ref-validate @ c2d1fc1; fixes bee5d5b and 34779b3, tests 33f61d3 and c2d1fc1, main merge e8f56e0; earlier review reviews/c-security.md)

VERDICT: C-1 CLOSED. ACCEPT. Nothing new is pre-merge.

One carried condition stays open, unchanged: P1 visibility (an ADR 0102 alert or the P1 log line) for the sync_amount_mismatch and provider_reference_conflict T10 parks, before any real-provider use.

Method:
- git archive of c2d1fc1. build and vet clean.
- Fresh private DB sec_c2_rv_20261003. DB access worked. Dropped WITH (FORCE); verified 0 left. No role or credential changes, no sudo, pipefail on, grepped for FAIL.
- Tests run:
  - full internal/payments package `-count=1 -tags integration`: ok (477s);
  - the C, A7, INVDEP1, KS, RVLF and probe subset under `-race`: ok (189s);
  - internal/providerref and internal/txscope (TestPCG1, INV-IO-1(c)) under `-race`: ok.
- Disclosure: my first attempt at the whole payments package under `-race` was stopped by the tool's 600s background limit before it printed any result. That is why I split it into the full non-race run plus the race subset above. No failure was observed in any run.
- 4 probes and 2 Go mutants, in a scratch copy only (removed).

------------------------------------------------------------------
Probes at c2d1fc1
- C-1 original (Pending plus a redirect, with a reference already bound to another intent of the same tenant): disputed / provider_reference_conflict, no reference bound, RedirectURL="" and Token="". CLOSED.
- Error path (a): adapter returns an error with Pending, a fresh valid reference, a redirect and a token. Result: ambiguous; the reference is bound to this attempt (T6); RedirectURL="" and Token="".
- Error path (b): the same, but the reference is already bound to another intent. Result: disputed / provider_reference_conflict; the reference is NOT bound; no redirect or token. The LF-6 pre-check runs before T6.
- Error path (c): the same, with an invalid reference (control character). Result: disputed / invalid_provider_reference:control_char; nothing bound; nothing returned.

Mutants (both killed, reverted and cmp-identical):
- Mx, the playerFacingRedirect state gate removed: KILLED by 8 tests (all conflict and mismatch parks, cascade-driven park, error path with a valid reference, late callback after a mismatch park, and others).
- My, the LF-6 pre-check skipped for the Ambiguous class (so T6 would bind an unchecked reference): KILLED by TestDepRefConflict_ReferenceBoundToAnotherDepositAttempt_ParksEveryOutcome (its ambiguous case) and by my error-path (b) probe.

------------------------------------------------------------------
Rulings

1. C-1: CLOSED. playerFacingRedirect (drive.go) returns the redirect URL and token only when the attempt that phase C committed is 'pending'. It is applied after phase C in both InitiateDepositAttempt (deposit_v2.go, after getAttemptInTenant) and driveCreatedAttempt (drive.go, on `final`), so the cascade loop inherits it. Gating on committed state rather than on the branch that ran is the right design: any future park reason is covered automatically. If a callback lands between the commit and the read, the gate only ever suppresses a redirect, which is fail-safe.

2. Orchestrator rule "no redirect or token on an ambiguous result, even with a valid reference": CONFIRMED.
- When the adapter reports an error, the platform does not know whether the PSP session is genuine or usable. Sending the player into it risks a capture the platform can only resolve by polling.
- The player can retry through a fresh, fully accepted attempt instead.
- It also follows directly from the pending-only gate, so there is no separate code path to drift.

3. Code review F1: CORRECT. The reference is validated before the `err != nil` return, using ValidateOptional when err is non-nil.
- An invalid reference parks, scrubbed to {Outcome}: probe (c).
- A valid reference is classified Ambiguous, passes through the LF-6 pre-check, then is bound by T6: probes (a) and (b).
- No raw reference reaches the binding query, the intent write or a log line. providerref errors carry only the field, reason, length and hash prefix.

4. LF F-C1 binding: SAFE.
- No cross-tenant binding. Both binding writes (T6 MarkAmbiguousFromSubmittingBindingRef, and parkDepositAttempt's bindRef UPDATE for sync_amount_mismatch) are `WHERE id = <this attempt>` with a state predicate, inside the attempt's own WithTenant session under FORCE RLS. The unique index payment_attempts_tenant_provider_ref is per tenant.
- No cross-attempt binding.
  - COALESCE never overwrites an already-bound reference.
  - The LF-6 pre-check in the same transaction, under the intent lock, refuses a reference bound to any other attempt (deposit or payout) or intent.
  - In a race with a concurrent, still-uncommitted binder on a different intent, the unique index raises 23505 and the transaction rolls back. On the next pass the pre-check sees the committed binding and parks, so there is no permanent loop.
- No LF-6 bypass. Both binding sites come after the pre-check, which now runs for every class except NotSent, including the error-path Ambiguous. Mutant My shows that skipping it is caught.
- Binding the reference on a sync_amount_mismatch DISPUTED attempt is safe. receipt.go's applyResolvedReceiptEvidence treats any success evidence on a disputed attempt as a no-op (`case AttemptDisputed: return false, ResolutionAnomalyOther, nil`, around receipt.go:751), so a later callback is recorded and routed to the right attempt but never posts. TestDepSyncAmount_LateCallbackAfterMismatchPark_RecordedOnlyNoPosting pins this.

Also checked:
- The unified tombstone park (LF F-C2) now goes through parkDepositAttempt (audit plus intent recompute, no posting, nothing bound).
- `adapter_outcome` in the audit metadata is a closed enum value. Harmless.

------------------------------------------------------------------
Still open (unchanged, not pre-merge): P1 log line or durable alert for the new T10 reasons (sync_amount_mismatch, provider_reference_conflict, invalid_provider_reference) before any real-provider use. Track with the I-wire P1 routing.

Scope: code- and DB-level review of the C fix delta; no penetration test. The merged main content in e8f56e0 (K2/0113 and others) was reviewed separately and not re-reviewed here.

Relevant paths (commit c2d1fc1):
- internal/payments/drive.go (playerFacingRedirect; depositAdapterCall's validate-before-err; parkDepositAttempt bindRef; the pre-check `if res.ProviderReference != "" && gr.Class != ErrorClassNotSent` around line 469)
- internal/payments/deposit_v2.go (playerFacingRedirect after phase C)
- internal/payments/attempt.go (MarkAmbiguousFromSubmittingBindingRef, around line 532)
- internal/payments/receipt.go (disputed no-op, around line 751)
- internal/payments/dep_ref_validate_integration_test.go

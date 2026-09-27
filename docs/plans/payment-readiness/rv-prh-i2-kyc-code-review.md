# RV-PRH-I2 (KYC) — Independent code review: ADR 0095 §15.2/§15.3 KYC create/submit split

Reviewer: `code-reviewer` (independent of the implementer). Date: 2026-09-27.
Scope: commit `8a52969` (merged at `dffdab3`) on branch `claude/focused-wright-jw88w9`,
read side by side with the pre-split code at `8a52969^`. I did not rely on the §15.3.1
summary. Items in the `security` and `identity-compliance` domains are listed in §4 for
those owners to decide. I have not ruled on them.

## Verdict: **NOT READY** (one blocking rework item, R1; the rest are follow-ups)

Most of the split is well built. Both phase-B provider calls run with no transaction and
no pooled connection held, and the one-connection-pool tests prove it directly. Both
phase-C transactions run on `context.WithoutCancel` with a 5 s bound, and both ctx-cancel
proxy tests fail if that is removed (I checked this with mutants MA/MF below). IC
condition 2 holds in both shapes. The 404→503 change is implemented as specified, and
the two pre-existing tests were strengthened, not weakened. The cmd wiring is correct.

The blocker: `SubmitVerification`'s phase C is **not** CAS-guarded and **not**
terminal-guarded, even though the commit message, the code comments and ADR 0095
§15.3/§15.3.1 all say it is. A staff rejection, or a callback, that commits while the
provider call is in flight gets silently overwritten. I reproduced this: a staff
**rejected** decision became **approved** (R1).

## 1. Verification performed

- `go vet -tags=integration` for `internal/kyc`, `internal/httpserver` and
  `cmd/platform-api`: clean.
- Integration tests on a **private** database I created for this review
  (`rv_i2kyc_cr_32156`, migrated to 104, not the shared CI database), run from a clean
  scratch worktree at `dffdab3`:
  - `go test -race -tags=integration ./internal/kyc/ ./cmd/platform-api/`: pass.
  - `go test -tags=integration ./internal/httpserver/ -run 'KYC|Kyc'`: pass.
- Scratch probes (in-package temporary test files, run, then deleted, never committed):
  - **P1.** During `SubmitVerification`'s provider call, staff `ReviewVerification`
    sets the row to `rejected` and commits. The provider then returns `approved`.
    Final status: **`approved`**.
  - **P2.** During the provider call, a verified callback `approved` is applied through
    `applyCallbackOutcome` and commits. The provider then returns `review_required`.
    Final status: **`review_required`**. The approval is demoted, which breaks the
    forward-only rank rule (J11).
  - **P3.** A player has a staff-approved verification, so withdrawal enforcement
    (`readLatestVerificationByPlayerAccount` → `effectiveOutcome`) returns `passed`.
    Then `CreateVerification` runs and the vendor fails in phase B. Enforcement now
    returns **`failed`**.
- Extra mutants I ran myself, in a scratch worktree against the private DB, over the
  full `internal/kyc` and `internal/httpserver` KYC/verification/document suites:

  | Id | Mutation | Result |
  |---|---|---|
  | MA | `SubmitVerification` phase C uses `ctx`, not `WithoutCancel(ctx)` | killed |
  | MF | `CreateVerification` phase C uses `ctx`, not `WithoutCancel(ctx)` | killed |
  | MB | `SubmitVerification` credential-binding check → `if false` | **survived** |
  | MC | `applyCreateVerificationResult` CAS predicate (`provider_reference IS NULL AND status='unverified'`) removed | **survived** |
  | MD | `submissionIdempotencyKey` drops `sort.Strings(ids)` | **survived** |
  | ME | `SubmitVerification` nil-outbound-resolver guard → `if false` | **survived** |

## 2. Findings (most severe first)

### R1 — BLOCKING: `SubmitVerification` phase C overwrites a concurrent terminal/forward status (no CAS, no terminal guard)

`internal/kyc/document_service.go` `applySubmissionResult` →
`internal/kyc/verification_service.go` `updateVerificationStatus`:

```go
`UPDATE kyc_verifications SET status = $1, reason = NULLIF($2, ''), updated_at = now() WHERE id = $3`
```

The only terminal check runs in phase A (`gatherSubmissionDocuments`), in a transaction
that has already committed. Phase C then writes with no predicate on the current
status. This contradicts:
- the commit message ("phase C applies the result under CAS");
- the doc comment on `applySubmissionResult` ("the existing terminal-guarded
  updateVerificationStatus");
- ADR 0095 §15.3 and §15.3.1, which say the same thing;
- ADR 0095 D1 ("applies the evidence … under a compare-and-set (CAS) guard").

`updateVerificationStatus` has never been terminal-guarded. Its only other callers
check rank themselves first.

- **Failure scenario (P1, reproduced).** A compliance officer rejects a verification
  (for example, for suspected document fraud) while the player's latest upload is being
  submitted. The vendor's automated check returns `approved`. Phase C writes
  `approved` over the staff `rejected`. ADR 0096 enforcement now returns `passed`, and
  the player can withdraw. The staff decision is lost, with only a
  `kyc.verification_submitted_to_provider` success audit row to show for it.
- **Failure scenario (P2, reproduced).** A vendor callback moves the row to `approved`
  mid-submission. The submit's own `review_required` result demotes it, and the player's
  withdrawals are blocked until a fresh vendor decision arrives.
- **Reachability.** With today's synchronous mock, the window is only phase A → phase C,
  and the mock's default outcome is `review_required`. So today this can turn a staff
  `rejected` back into `review_required`, but not into an allow. With any real adapter,
  the window is the whole vendor round-trip, and an `approved` result is ordinary.
- **Pre-existing, but this was the place to fix it.** The pre-split code had the same
  unguarded UPDATE inside one READ COMMITTED transaction with no `FOR UPDATE`, so the
  race is not new. However, §15.3 made this phase C's job and documented it as guarded.
  The implementation record now asserts a guarantee the code does not provide.
- **Fix (small).** Make phase C a CAS on the status phase A read, applied under the
  same forward-only rank rule the callback path uses. For example, route the definitive
  outcome through `applyCallbackOutcome`'s rank/CAS loop (or a shared helper), with a
  distinct audit action. At minimum, use
  `UPDATE … WHERE id=$ AND tenant_id=$ AND status=$phaseAStatus`, and on a miss re-read
  and no-op if the row is terminal or at/above the new rank. Add P1 and P2 as regression
  tests, and correct the "terminal-guarded" wording in §15.3/§15.3.1 and in the doc
  comment.

### F1 — "The orphan row has no enforcement effect" is false (deny direction; a new failure mode)

The doc comments on `insertOrphanVerification` and `CreateVerification`, the test
`TestCreateVerification_NilOutboundResolverLeavesHarmlessOrphanRow` and ADR §15.2 all
say the orphan has no enforcement effect because enforcement "only allows on passed".
But ADR 0096 enforcement reads the account's **latest** row
(`ORDER BY created_at DESC, id DESC LIMIT 1`). An orphan `unverified` row is the newest
row, so it maps to `failed`.

- **Failure scenario (P3, reproduced).** An approved player starts a re-verification
  (for example, a renewal), and the vendor is down. Before the split, the transaction
  rolled back, so the approval stayed the latest row and withdrawals kept working. Now
  the orphan commits, and **withdrawals (and play, where a jurisdiction requires passed)
  are denied** until a later retry succeeds. The success path already superseded the
  approval with a `pending` row before this change. What is new is that a vendor outage
  alone now does it.
- This fails closed, so it is not a security hole. But the documented claim is wrong,
  and the test named "Harmless" asserts nothing about enforcement.
- **Follow-up.** Either correct the claim (and let `identity-compliance` accept the
  deny-on-outage behaviour), or exclude `provider_reference IS NULL AND
  status='unverified'` rows from the enforcement "latest" read. The second option
  touches `enforcement*.go`, which is out of this task's scope, so it needs the owner's
  decision. See also §4 item 1.

### F2 — Player retries against a non-idempotent vendor: duplicate rows, plus a callback that 503s forever

The PROVIDER DEPENDENT label for `kv:` idempotency is correct and honestly documented.
Two consequences are not documented:

- **(a) Unbounded orphans.** Every retry after a phase-B failure commits one more orphan
  row and one more `kyc.verification_requested` audit row. Before the split, a failure
  wrote nothing.
- **(b) Permanent 503s.** Suppose phase C fails after the vendor accepted (for example,
  a DB error inside the 5 s bound). The vendor-side verification then exists, but its
  reference is never stored. Every callback for it returns
  `ErrVerificationReferenceUnknown` → **503, permanently**. There is no terminal state
  for a reference that will never become known. The vendor redelivers until its own
  retry policy gives up, and the vendor's decision is never linked to any platform row.
  The 503 branch in `kyc_admin_handlers.go` also writes no log line, so an operator
  cannot see a stuck reference.
- **Follow-up.** Add an allow-listed log line on the 503 branch (tenant and provider,
  without the reference or body). Record (b) under KYC-SUBMIT-OUTBOX-1 so the real-vendor
  intake decides how an orphaned reference is reconciled.

### F3 — Test/mutation evidence is accurate for what it lists, but "11/11" overstates coverage of the new branches

I found nothing false in the evidence file. Each listed mutant maps to a test that
exercises it. M8 is killed through the mock's "unknown reference" error rather than the
test's `called` assertion, but it is still killed. However, the file and §15.3.2 present
"the credential-binding check" and "the idempotency-key shape" as covered, and four
equivalent mutants survive:

- **MB.** `SubmitVerification` has its own copy of the binding check, and no test
  covers it. A resolver returning another tenant's credential would be used for the
  submit call without any test failing.
- **MC.** `CreateVerification`'s phase-C CAS predicate, the one §15.2 specifies, is
  untested. `TestCreateVerification_*` never exercises a row that has already left
  `unverified`.
- **MD.** The `ks:` key's ordering and content are untested.
  `TestSubmitVerification_IdempotencyKeyStableForSameDocumentSet` only submits an
  **empty** set, and `emptyDocSetSHA256()` derives its expected value by calling
  `submissionIdempotencyKey` itself, so the "pins the actual documented shape" claim is
  partly tautological. A multi-document test with an independently computed hash is
  needed.
- **ME.** `SubmitVerification` with a nil outbound resolver is untested (it would
  panic).

Also, the evidence file says it ran against the shared `igaming_platform_ci_local`
database. That is not wrong, but it differs from the private-DB convention. The
follow-up is to add the four tests and re-state the count.

### F4 — Wiring: correct, but the "mirrors casino exactly" claim is missing casino's wiring test

`kycOutboundCredentials()` correctly converts the nil concrete pointer to a true nil
interface, and `NewOutboundKindSplitResolver` returns nil when both resolvers are nil.
`KYCOutboundResolver` follows `testSupport`, the same flag as `KYCWebhookEnabled`, so a
KYC mock provider and its outbound mock cannot diverge. The mock is registered with the
synthetic-component guard (`buildRegistrations`).

The `allOnWiring` change is **required, not a weakening**. Without it, the new
`providerBundle.KYCOutboundResolver` field would stay nil in the completeness scan, and
that scan would be vacuous for it. With it, `TestSyntheticGuard_RegistrationCompletenessScan`
covers the new field, and it passes.

Missing: a KYC twin of `TestCasinoOutboundResolver_FollowsWiring`. It would check that
the resolver is nil when wiring is off, that it resolves `mock` with `Domain=="kyc"`
when on, and that it fails closed for an unregistered id. Low severity; follow-up.

### F5 — The upload handler now commits a document that may never be submitted (disclosed, accepted; note only)

`newUploadMyDocumentHandler` now commits the upload, then calls `SubmitVerification` on
`r.Context()`, and only logs any failure. Before the split, a submission failure rolled
back the upload and returned 500. Now the player gets 201 even when submission failed,
and the verification waits for the next upload. That is exactly the accepted
KYC-SUBMIT-OUTBOX-1 deferral. It is recorded here because a client disconnect right
after the upload commits is now enough to strand a submission, even with the mock.
No action beyond the existing hard precondition.

## 3. Items checked and found correct

- **CreateVerification phases.**
  - Phase A commits the row and its audit record before any credential resolution or
    provider call.
  - Nil provider or nil pool is rejected before phase A. A nil resolver is rejected
    after phase A, as documented.
  - Phase C's CAS (`id AND tenant_id AND provider_reference IS NULL AND
    status='unverified'`) fails closed on `RowsAffected != 1`.
  - `tenant_id` always comes from the server-resolved `tc.TenantID`.
- **SubmitVerification phase A.**
  - The read is tenant-scoped, and a cross-tenant id yields `ErrNotFound` (tested).
  - A terminal verification is a no-op with no provider call (tested).
- **IC condition 2, both shapes.**
  - Transport error: no phase C, status unchanged, `ErrProviderUnavailable`.
  - `ProviderError` outcome: failure audit row, early return before `statusForOutcome`.
  - Each shape has its own test, and each test is killed by its own mutant (M6, M7).
- **404→503.**
  - `ErrVerificationReferenceUnknown` does not wrap `ErrNotFound`, and a test asserts
    both directions.
  - The HTTP branch is ordered before the `ErrNotFound` branch.
  - Both updated pre-existing tests were **tightened**: they add the negative
    `errors.Is(ErrNotFound)` assertion and change 404 → 503. The test name
    `TestKYCWebhook_VerifiedUnknownReference_NotFound` is now misleading (cosmetic).
- **Handler restructure.**
  - Identity and provider selection stay in a short read transaction. The provider call
    happens outside it.
  - `ErrProviderUnavailable` → 503.
  - Upload ownership (`verification.PlayerAccountID == account.ID`) is still checked
    inside the upload transaction before any submission.
- **Other.**
  - There are no raw error strings in audit metadata. Phase-B/C errors go to slog only.
  - `CallContext` redaction is tested with a negative control (M11).
- **Labels.** "IMPLEMENTED (code + tests); not yet gate-reviewed" is accurate as a
  status label (MOCK provider only; KYC-SUBMIT-OUTBOX-1 recorded as a hard
  precondition). The **content** claims under it that are wrong are "terminal-guarded"
  and "under CAS" (R1) and "no enforcement effect" (F1). Both must be corrected with the
  R1 rework.

## 4. Surfaced to other owners (not adjudicated here)

1. **`security` / `identity-compliance` — pre-existing overlay weakness.**
   `crossAccountRejectedOverlay` (withdrawal) only looks at each other account's
   **latest** row. Any newer row on the rejected account B masks B's rejection, and
   account A's approved withdrawal is then allowed. Before this change, a successful
   `CreateVerification` on B already did this (a new `pending` row). A phase-B failure
   now does it too (an orphan `unverified` row). No new capability is added, but the
   mask is now reachable without the vendor accepting anything. For the owner to rate.
2. **`identity-compliance` — accept or reject F1's deny-on-vendor-outage behaviour**
   for approved players who start a re-verification.
3. **`security` — R1's staff-decision overwrite** is an integrity issue for compliance
   decisions (four-eyes and audit intent). Severity for a real adapter is the owner's
   call. I rate it blocking from a correctness standpoint only.

## 5. Required before READY

- R1 fixed, with P1 and P2 as regression tests, and the §15.3/§15.3.1/doc-comment
  wording corrected.
- F1's claim corrected (or the owner decision recorded).

F2–F5 are follow-ups and may be tracked in `docs/governance/task-registry.md`.

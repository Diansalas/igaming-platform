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

---

# Re-review: fix round `492cb20` (merged at `b9db031`)

Reviewer: `code-reviewer` (independent). Date: 2026-09-27. I read the diff `492cb20^..492cb20`
directly, then ran the suites and my own mutants in a detached scratch worktree at `b9db031`,
against a private database (`rr_i2kyc_cr_28463`, migrated to 104, runtime grants applied,
dropped afterwards).

## Verdict: **READY once N1 is corrected** (R1 and F1 are fixed; N1 is a small, required correction to a test and to the evidence file)

R1 is fixed properly, and so is F1. The fixes for C2–C5 and F2–F4 are present. However, C4
quietly removed the only test that killed mutant M8, and the evidence file still claims M8
is "re-verified unchanged, KILLED". That claim is false today (N1). It takes one line in a
test and one edit to the evidence file. Once both are done, this change is READY from a
code-review standpoint. The security and identity-compliance sign-offs are still their
owners' decisions.

## 1. Verification

- `go vet -tags=integration` for `internal/kyc`, `internal/httpserver` and
  `cmd/platform-api`: clean. `gofmt -l`: clean.
- `go test -race -tags=integration ./internal/kyc/ ./cmd/platform-api/`: pass.
- `go test -tags=integration ./internal/httpserver/ -run 'KYC|Kyc'`: pass.
- `go test -tags=integration ./internal/withdrawal/`: pass.
- Unrelated observation: `TestResolutionIsolation_*` failed on the fresh private DB with no
  mutation applied. This change does not touch those tests, and I did not investigate.
- Mutants (each run over the full `internal/kyc` suite unless noted; tree restored with
  `git checkout` after each):

  | Id | Mutation | Result |
  |---|---|---|
  | R1a | `applyForwardOnlyStatus` UPDATE loses `AND status = $5` (blind write) | killed (P1 + P2) |
  | R1b | forward-only check `<=` → `<` (same-rank terminal overwrite) | killed (P1) |
  | R1c | `maxAttempts` 3 → 1 (no re-read after a lost CAS) | killed (P1 + P2) |
  | R1e | `status_applied` audit flag hard-wired `true` | **survived** (N4) |
  | MA | `SubmitVerification` phase C uses `ctx`, not `WithoutCancel` | killed |
  | MF | `CreateVerification` phase C uses `ctx`, not `WithoutCancel` | killed |
  | MB | `SubmitVerification` credential-binding check → `if false` | killed, for the right reason ("got \<nil\>") |
  | MC | `applyCreateVerificationResult` CAS predicate removed | killed |
  | MD | `sort.Strings` dropped: multi-document integration test only | **survived** (as the implementer disclosed) |
  | MD | `sort.Strings` dropped: full suite | killed (`TestSubmissionIdempotencyKey_SortsDocumentIDs`) |
  | ME | `SubmitVerification` nil-outbound guard → `if false` | killed (panic) |
  | C4 | empty-set skip → `if false && …` | killed |
  | **M8** | `gatherSubmissionDocuments` terminal guard → `if false && …` | **survived** (N1) |
  | M8 + C4 off | same, with the empty-set skip also disabled | killed, which proves C4 is what masks it |
  | F1a | orphan predicate removed from `readLatestVerificationByPlayerAccount` | killed |
  | F1b | orphan predicate removed from `crossAccountRejectedOverlay` | killed |
  | F1c | predicate loosened to `NOT (status='unverified')` | survived. Equivalent in practice: `statusForOutcome` never produces `unverified`, so no decided `unverified` row exists. |
  | C2 | kind-split `target := s.mock` unconditionally | killed |
  | C5 | `RedactedProviderErrorDetail` default returns `err.Error()` | **survived** in `kyc` and `httpserver -run 'KYC\|Kyc\|Verification\|Document\|Redact'` (N3) |

- **Implementer's methodology disclosures: both confirmed.**
  - **MB.** I removed the `provider.created[ref] = true` registration from the MB test and
    re-applied the MB mutant. The test then **passes** under the mutant, because the mock's
    "unknown reference" error produces `ErrProviderUnavailable` by coincidence. The
    corrected test, which reuses the registered instance, fails with "got \<nil\>". So the
    correction is real and necessary.
  - **MD.** `gatherSubmissionDocuments` reads `ORDER BY id`. PostgreSQL compares `uuid`
    values bytewise, and that order matches lowercase-hex string order. So the integration
    test always receives pre-sorted input and cannot kill MD on its own. I confirmed it
    survives. The DB-free unit test is what kills MD.

## 2. Prior findings: status

| Prior | Status | Evidence |
|---|---|---|
| **R1** (blocking) | **FIXED** | Phase C now goes through `applyForwardOnlyStatus`: a CAS on phase A's status, then on a miss it re-reads and applies the rank rule. The blind `updateVerificationStatus` is deleted, and grep finds no other status writer without a predicate. P1 and P2 exist as regression tests and exercise the lost-CAS path: phase A reads `pending`, and a concurrent commit moves the row off `pending`. The R1a/b/c mutants each kill them. Wording in the doc comment and ADR 0095 §15.3/§15.3.1 is corrected. The remaining "terminal-guarded" at ADR line ~2092 refers to phase A, where it is accurate. |
| **F1** | **FIXED** | `orphanRowExclusionSQL` is applied to the primary read and, separately, to the overlay's inner subquery. Four tests cover it. F1a and F1b are each killed by their own test. Every enforcement "latest" read goes through these two functions (I grepped), so no other reader was missed. ADR 0096 §19 records the change. |
| **F2** | **ADDRESSED** | The allow-listed `kyc_webhook_reference_unknown` line is added, with Retry-After set through `writeAdmissionRejection`. An allow-list test checks that the reference is not leaked. The stuck-reference problem is recorded under KYC-SUBMIT-OUTBOX-1. |
| **F3** (MB/MC/MD/ME) | **FIXED** | All four are killed now (table above). The tautological `emptyDocSetSHA256` is removed. The key is now computed independently. |
| **F4** | **FIXED** | `TestKYCOutboundResolver_FollowsWiring` covers off → true nil, on → resolves `mock`/`Domain=="kyc"`, and fail-closed for an unregistered id. It passes. |
| **F5** | Note only, unchanged | n/a |
| **C1** (security) | Same as R1 | |
| **C2** (security) | **FIXED** | Five kind-split unit tests. The C2 mutant is killed. |
| **C3** (security) | **FIXED** | Same change as F2. |
| **C4** (security) | **FIXED, but it introduced N1** | The skip is implemented and `TestSubmitVerification_EmptyDocumentSetIsANoOp` kills its removal. |
| **C5** (security) | **Implemented, untested** (N3) | Both handler log lines now go through `RedactedProviderErrorDetail`. Whether C5 needs a test is for `security` to decide. |

## 3. New findings (most severe first)

### N1: REQUIRED. C4 made `TestSubmitVerification_TerminalVerificationIsANoOp` vacuous, and the evidence file's M8 claim is now false

The fix round added `seedDocument` to every pre-existing `SubmitVerification` test that
needed the provider call to happen, **except** `TestSubmitVerification_TerminalVerificationIsANoOp`
(`internal/kyc/kyc_two_phase_integration_test.go` ~L436). That test seeds no document and
asserts `called == false`.

- **Why it no longer tests anything.** If the terminal guard in
  `gatherSubmissionDocuments` is removed, execution now falls through to the new
  `len(submitted) == 0` skip and still makes no provider call. `applyForwardOnlyStatus`
  also keeps the status `approved`. So the test passes whether or not the guard exists.
- **Confirmed by mutants.** M8, run exactly as the evidence file specifies (`-run
  TestSubmitVerification_TerminalVerificationIsANoOp`), **survives**, and it also survives
  the full `internal/kyc` suite. With C4's skip also disabled, the same test kills it.
- **Why it matters.** `docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt`
  §1 says the original 11 mutants were "re-verified unchanged this round". It still lists M8
  as KILLED, quoting a first-failure message ("unknown provider reference") that cannot
  occur any more. So "17/17 killed" is not true as of `492cb20`.
- **Failure scenario.** A future refactor drops the terminal guard. An approved (or
  staff-rejected) verification whose player uploads another document then gets resubmitted
  to the vendor. That is an unnecessary vendor call, plus a `kyc.verification_submitted_to_provider`
  audit row against a closed case. The status stays correct only because R1's rank rule
  happens to backstop it. No test would fail.
- **Fix.**
  1. Add `seedDocument(t, pool, f, verificationID, DocumentPassport, "p.png")` to that test
     before the review step (while the verification is still non-terminal).
  2. Re-run M8.
  3. Correct the evidence file's M8 entry and the count.

  None of the other pre-existing tests was weakened. I checked every `SubmitVerification(`
  call site in tests:
  - Cross-tenant fails in phase A, before the skip.
  - `reason_platform_normalize` gained a document.
  - The four two-phase tests that need a provider call gained one.
  - The idempotency test was replaced with a stronger one.

### N2: Low. A misleading comment in the multi-document idempotency test

`TestSubmitVerification_IdempotencyKeyStableForMultiDocumentSet` (~L566–569) says that if
`sort.Strings` were dropped, the independently computed expectation "would no longer
match". That is false for the reason the implementer's own MD disclosure gives (`ORDER BY
id`), and I confirmed it: MD survives that test. In addition, the doc comments for
`independentSubmissionIdempotencyKey` and `TestSubmissionIdempotencyKey_SortsDocumentIDs`
have been merged into one block above the unit test. Fix: rewrite the inline comment so it
says the integration test only proves stability across calls and `document_count=3`, and
that the unit test is what pins the sort.

### N3: Low (for security to decide). C5 redaction has no test

Changing `RedactedProviderErrorDetail`'s default branch back to `err.Error()` survives
every KYC-related test. The helper is 15 lines and easy to read as correct. But a later
"improvement" that logs `%v` again would pass CI. A table unit test covering timeout,
canceled, `ErrProviderUnavailable`, and an arbitrary error containing a sentinel string
would close this. It is security's call whether C5 requires it.

### N4: Low. `status_applied` audit flag is untested

This is the only audit signal that distinguishes a superseded submission from one that
actually changed the row. Hard-wiring it to `true` survives. P1 and P2 could assert
`status_applied=false` on the submission's audit row at almost no cost.

### N5: Low, pre-existing from the original split (not introduced by this round). Submitting against an orphan calls the vendor with an empty reference

The upload handler takes `verification_id` from the client and only checks that the player
owns the row. So a player can upload to their own orphan row (`unverified`,
`provider_reference NULL`). `SubmitVerification` then calls
`provider.SubmitVerification(ctx, "", …)`.
- With the mock, the call fails closed (unknown reference → `ErrProviderUnavailable`).
- With a real vendor, the call is malformed, and its behaviour is PROVIDER DEPENDENT.

Suggested follow-up: a no-op or fail-closed guard in `SubmitVerification` when
`v.ProviderReference == ""`. It fits naturally next to the C4 skip.

### N6: Simplification / accuracy. `applyForwardOnlyStatus` is a second copy of the loop, not a shared one

The commit message says the new function is "shared", and the doc comment in
`verification_service.go` says both the callback step "(via applyCallbackOutcome)" and
phase C use it. In fact, `applyCallbackOutcome` still has its own identical rank/CAS loop
(`provider.go` ~L564–606) and does not call `applyForwardOnlyStatus`. This is correct
today, but it means the two loops can drift apart. Either make `applyCallbackOutcome` call
`applyForwardOnlyStatus` (its audit row is written only when `applied` is true), or fix the
comment. Also cosmetic: `insertOrphanVerification` and `CreateVerification`
(`verification_service.go` ~L100, ~L191) still justify "no enforcement effect" with "only
allows on passed". The claim is now true, but for a different reason (the orphan
exclusion).

## 4. Surfaced to other owners (not adjudicated)

1. **`identity-compliance` / `security`, pre-existing: the mirror image of R1.**
   `ReviewVerification` reads the row, checks it is not terminal, then runs a blind
   `UPDATE … WHERE id`. If `SubmitVerification`'s phase C or a callback commits `approved`
   between the staff read and the staff write, a staff `review_required` overwrites a
   terminal `approved`, moving it backward. "Staff wins last" may be the intended policy for
   `approved`/`rejected`. It is doubtful for `review_required` over a terminal status. This
   round did not change `ReviewVerification`. The same `WHERE status = $current` CAS would
   close it.
2. **`identity-compliance`: accepted behaviour change.** Phase C no longer lowers a
   status. For example, if a row is `review_required` (staff asked for more documents) and a
   re-submission's vendor result is `pending`, the row now stays `review_required` with the
   staff reason, where it used to drop to `pending`. This is consistent with J11. Recorded so
   the owner can see it.

## 5. Required before READY

- **N1:** add `seedDocument` to `TestSubmitVerification_TerminalVerificationIsANoOp`,
  re-run M8, and correct the M8 entry and the "17/17" count in the evidence file.

N2–N6 are follow-ups.

---

# Re-review 2: KYC fix round 2 `ed6d3e8` (merged at `9324189`)

Reviewer: `code-reviewer` (independent). Date: 2026-09-27. I read the diff `ed6d3e8^..ed6d3e8`
directly. I ran the suites, the pinned linter and my own mutants in a detached scratch
worktree at `9324189`, against a private database (`rr2_i2kyc_cr_13934`, migrated to 105,
runtime grants applied, dropped afterwards). While I was working, HEAD moved to `c80103b`
(docs only: the identity-compliance and security review files). No code changed.

## Verdict: **READY for N1–N6 and the ReviewVerification mirror race. The N-1 overlay fix is NOT complete (N-1b).**

N1–N6 and the mirror-race CAS are correctly fixed, and each fix is pinned by a test that I
confirmed kills a mutant. The N-1 narrowing still lets a later vendor `expired` row on the
rejected account lift the rejection. I reproduced this myself through the real
Create → verified-callback path. Security (N-1b) and identity-compliance (Ruling 1) reached
the same result independently, so I am confirming their finding, not ruling on it. The
fix and its severity belong to those owners and the orchestrator (ADR 0096 §20). From a
code-correctness standpoint, the N-1 item is not closed until `finalStatusesSQL` drops
`expired`, which is the `('approved','rejected')` shape, and a test kills that mutant.

## 1. Verification

- `gofmt -l`: clean.
- `go vet -tags=integration` for `kyc`, `httpserver`, `cmd/platform-api` and `withdrawal`:
  clean.
- Pinned `golangci-lint` 2.9.0 (go1.26.0 build), `run ./...` with
  `--allow-parallel-runners`: **0 issues**. The commit message says lint could not run.
  With the pinned binary it now runs and is clean.
- `go test -race -tags=integration`: pass for these packages:
  - `internal/kyc`
  - `cmd/platform-api`
  - `internal/withdrawal`
  - `internal/httpserver -run 'KYC|Kyc|Verification|Document'`
- Mutants. Each was run over the full `internal/kyc` suite unless noted, and the tree was
  restored with `git checkout` after each:

  | Id | Mutation | Result |
  |---|---|---|
  | **M8** | terminal guard in `gatherSubmissionDocuments` → `if false && …`, run as the evidence file specifies (`-run TestSubmitVerification_TerminalVerificationIsANoOp`) | **killed** |
  | M8 (full suite) | same | killed (N1 closed) |
  | N4 | `status_applied` hard-wired `applied \|\| true` | killed (the `false` subtest) |
  | N5-upload | `UploadDocument` orphan guard removed | killed |
  | N5-submit | `SubmitVerification` orphan guard removed | killed |
  | MR1 | `ReviewVerification` CAS predicate `AND status = $6` removed | killed |
  | MR2 | `ErrNoRows` → `ErrVerificationStatusConflict` mapping disabled | killed |
  | N6a | `applyCallbackOutcome` writes its audit row even when `!applied` | killed (replay, 8-concurrent and after-staff-decision webhook tests) |
  | C5 | `RedactedProviderErrorDetail` default branch returns `err.Error()` | killed (N3 closed) |
  | C5-http-create | create handler logs `err.Error()` | killed (`TestKYC_CreateVerificationProviderFailureLog_NeverLeaksRawErrorText`) |
  | **C5-http-upload** | upload handler's `submit_verification_failed` logs `submitErr.Error()` | **survived** (R2-2) |
  | NM1a | overlay reverted to F1's orphan-only predicate (the pre-N-1 state) | killed (pending and review_required tests) |
  | NM1b | `'approved'` dropped from `finalStatusesSQL` | killed (`LaterFinalApprovedLiftsRejection`) |
  | **NM1c** | `'expired'` dropped from `finalStatusesSQL` | **survived**. This mutant is the correct fix; see R2-1. |
  | **409-review** | handler branch `ErrVerificationStatusConflict` → 409 disabled | **survived** (R2-3) |
  | **409-upload** | handler branch `ErrVerificationNotSubmitted` → 409 disabled | **survived** (R2-3) |

- **Scratch probe** (a temporary in-package test, run, then deleted, never committed):
  1. Account A is approved and account B (same Person) is rejected. A withdrawal from A is
     denied.
  2. `CreateVerification` runs on B. The row goes `pending` with a live reference.
  3. A verified callback reports `expired` for that reference. The rank rule applies it.
  4. A withdrawal from A is now **allowed** (`outcome=passed`).

## 2. Round-1 findings: status

| Item | Status | Evidence |
|---|---|---|
| **N1** (required) | **FIXED** | `seedDocument` is added to the terminal test before the review step. M8 is killed both targeted and over the full suite. |
| **N2** | **FIXED** | The false inline claim is replaced with an accurate NOTE. The merged doc comments are separated again. |
| **N3** | **Mostly FIXED** | The helper has a table and sentinel unit test, and there is an end-to-end create-handler log test. Both are killed by their mutants. The upload-path log line is still unpinned (R2-2). |
| **N4** | **FIXED** | `TestSubmitVerification_AuditRecordsStatusAppliedFlag` asserts both `true` and `false`, through the P1 race shape. |
| **N5** | **FIXED** | There is a guard in both `UploadDocument` (before the scan or any storage write) and `SubmitVerification`, with a typed `ErrVerificationNotSubmitted`. Orphans are seeded through the real nil-resolver failure mode, not raw SQL. Each guard is killed by its own test. The HTTP 409 mapping is untested (R2-3). |
| **N6** | **FIXED (code); cosmetic leftovers** | `applyCallbackOutcome` now calls `applyForwardOnlyStatus`, and its audit row is written only when `applied` is true. N6a is killed by three existing webhook tests. The only behaviour change is an extra re-read on a no-op, which is harmless. The two stale "only allows on 'passed'" comments (`verification_service.go` ~L117, ~L207) are still there (R2-4). |
| **Mirror race** (§4 item 1) | **FIXED** | The CAS is `AND kyc_verifications.status = $6` (the status this call read). A miss maps to `ErrVerificationStatusConflict` → 409. It never retries or silently does nothing, which is the right choice for a deliberate staff action. The regression test commits a real `SubmitVerification` approval inside the read/write window through the test hook, and asserts both the conflict and that `approved` survives. MR1 and MR2 are killed. The hook is a nil package var and no `internal/kyc` test uses `t.Parallel`, so there is no `-race` hazard today. |
| **N-1** (security; orchestrator decision, ADR 0096 §20) | **Incomplete (N-1b)** | See R2-1. |
| **Evidence file "23/23"** | **Substantively honest; arithmetic slightly off** | See R2-5. |

## 3. New findings (most severe first)

### R2-1: HIGH (owned by security / identity-compliance; confirming N-1b). `expired` in `finalStatusesSQL` re-opens the N-1 bypass

`crossAccountRejectedOverlay`'s inner subquery now selects each other account's latest row
with status in `('approved','rejected','expired')`. The outer query matches only when that
row is `rejected`. So any **newer `expired`** row on B hides B's rejection.

- **Where `expired` comes from.** Only `statusForOutcome(ProviderExpired)` writes it; no
  platform job writes it. `applyForwardOnlyStatus` lets it land only on a non-terminal row,
  because `approved → expired` has the same rank and is a no-op. So an `expired` row always
  means "an attempt lapsed without a decision". It never means "an approval ran out".
- **Failure scenario (reproduced).** The player starts a new verification on the rejected
  account B and abandons it. The vendor sends `expired`. Withdrawals from the approved
  account A are then allowed. Whether the vendor emits `expired` on abandonment is
  PROVIDER DEPENDENT. It is not reachable with the mock today.
- **Why the tests miss it.** NM1c (dropping `expired`) survives, so no test pins the
  `expired` behaviour either way. The `crossAccountRejectedOverlay` doc comment and ADR
  0096 §20 both describe `expired` as a "re-decision". That is inaccurate.
- **Fix.** Use `('approved','rejected')`: only a later approval lifts a rejection. Add the
  test `rejected(B) → Create + expired callback on B → A still denied`. NM1c must then be
  killed by that test.

### R2-2: Low. The upload handler's `submit_verification_failed` redaction is unpinned

Changing that log line back to `submitErr.Error()` survives every KYC, Verification and
Document test in httpserver. The create-path twin is pinned. A matching capture test on the
upload path would close this.

### R2-3: Low. Neither new 409 mapping has an HTTP test

If the `ErrVerificationStatusConflict` branch in `newReviewVerificationHandler` or the
`ErrVerificationNotSubmitted` branch in `newUploadMyDocumentHandler` is removed, both
errors fall through to 500 with `review_verification_failed` / generic logging. No test
fails. The domain behaviour is pinned; the API contract (409 and message) is not. Each
needs one handler test.

### R2-4: Cosmetic. Stale orphan rationale

`insertOrphanVerification` and `CreateVerification` still say orphans have no enforcement
effect because "EvaluateEnforcement only allows on 'passed'". The actual reason is now
`orphanRowExclusionSQL`, and in the overlay, the final-status filter.

### R2-5: Low. The evidence-file count double-counts M8

The file says 23 = 11 (§1, "re-verified unchanged") + 6 (round 1) + 6 (round 2). It also
says the §1 M8 entry is SUPERSEDED and re-counted in §2.5 as M8-reverify. That makes §1
contribute 10 valid mutants, so the total is 22 distinct mutants. Every listed live mutant
I re-ran (M8, N3/C5, N4, N5, mirror race, N-1 revert) is genuinely killed. Separately,
§2.5 and §3.5 describe the N-1 mutant as killed without noting that the `expired`
membership is unpinned (R2-1). The evidence file should record that as a surviving mutant,
or as open under N-1b, until R2-1 lands.

### Note: pre-existing, low. Create-side ProviderError / empty reference

`applyCreateVerificationResult` maps an unrecognised outcome (including `ProviderError`
returned without a Go error) to `pending`, and stores `NULLIF(reference,'')`. A real
adapter that returns `ProviderError` with no reference would produce a `pending` row with
a NULL reference. That row counts as "decided" for F1: it would supersede an existing
approval in the primary read. Under N5 it is also a permanent upload and submit dead end
(409). The mock never does this, so this is PROVIDER DEPENDENT. IC condition 2's
"no state change on ambiguity" rule could be applied to create as well (treat it like a
phase-B failure and leave the orphan). Suggested for real-adapter intake.

## 4. Required

- **R2-1 / N-1b.** Owned by security, identity-compliance and the orchestrator. It must
  land before N-1 is marked closed. It does not block the N1–N6 or mirror-race items.

R2-2 to R2-5 and the note are follow-ups.

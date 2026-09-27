# RV-PRH-I2 (KYC) — Security review of the KYC CreateVerification/SubmitVerification split

- Reviewer: `security` specialist
- Date: 2026-09-27
- Subject: ADR 0095 §15.2 / §15.3 and the §15.3.1 / §15.3.2 implementation records; commit `8a52969` (merged at `dffdab3`); reviewed at HEAD `dffdab3`
- Verdict: **APPROVE WITH CONDITIONS.** PRH-I2 (KYC part) **must not be marked complete** until C1 is closed. C1 is HIGH: a staff rejection can be silently overwritten to `approved`, and `approved` is what KYC enforcement treats as passed. C2 and C3 must be closed before a real KYC adapter is registered. C4 and C5 are low-severity hygiene items.

## Scope

In scope:
- `internal/kyc/verification_service.go`: `CreateVerification`, `insertOrphanVerification`, `applyCreateVerificationResult`.
- `internal/kyc/document_service.go`: `UploadDocument`, `SubmitVerification`, `gatherSubmissionDocuments`, `submissionIdempotencyKey`, `applySubmissionResult`.
- `internal/kyc/callcontext.go`.
- `internal/kyc/provider.go`: the `ErrVerificationReferenceUnknown` path.
- `internal/httpserver/kyc_handlers.go` and the `kyc_admin_handlers.go` webhook dispatch.
- `cmd/platform-api` wiring (`registrations.go`, `wiring.go`, `main.go`).

Out of scope:
- `internal/kyc/enforcement*.go`, except to confirm how `approved` is interpreted.
- Payments (PRH-I1) and casino (already reviewed in `rv-prh-i2-casino-security.md`).
- A real KYC adapter (none exists).
- Penetration testing.

This review does not declare the KYC flow "secure" in general. It covers what this diff changes.

## Findings

### C1 — HIGH (blocks completion): phase C of `SubmitVerification` is not CAS or terminal-guarded, so a concurrent staff decision is overwritten

ADR 0095 §15.3 and §15.3.1 say phase C applies the result through "the terminal-guarded `updateVerificationStatus`". The function is not terminal-guarded. It runs `UPDATE kyc_verifications SET status=$1, reason=… WHERE id=$3` with no status predicate (`verification_service.go:436`). The only terminal check is in phase A (`gatherSubmissionDocuments`), and by the time phase C runs that read is stale.

The split puts the whole provider call between that check and the write. A real adapter may take up to `defaultProviderCallTimeout` (10s). Any change that commits in that window is overwritten:
- a staff `ReviewVerification`
- a verified callback's forward-only CAS transition

The overwrite can also move status backward, for example `review_required → pending`. That breaks the rank rule the callback path enforces.

**Failure scenario (reproduced on a private DB with a scratch probe test, not committed):**
1. A player uploads a document.
2. During phase B, a compliance officer rejects the verification (fraud).
3. The provider returns `approved`.
4. Final `kyc_verifications.status` is `approved`, with the staff member's `reviewed_by` still stamped on the row.

Probe output: `final status after staff REJECT during phase B + provider passed: "approved"`. `enforcement.go:295` maps `StatusApproved` to `OutcomePassed`, so a player a human rejected is now allowed through KYC-gated play and withdrawal. The same race existed with a much smaller window before the split, because the single transaction never locked the row. The split makes it wide, and the ADR/commit describe it as guarded when it is not.

**Required fix:**
- Make `applySubmissionResult`'s status write a CAS. Reuse the callback path's forward-only rank CAS with the expected status = `v.Status` from phase A, or at minimum add `AND status = $expected` and treat 0 rows affected as a no-op (still write the submission audit row with an outcome showing it was superseded).
- Add an integration test in which the provider stub commits a `ReviewVerification` (reject, and separately approve) inside `onSubmitVerification`, and assert the staff decision survives.
- Add a second test covering `review_required → pending` regression.
- Correct the ADR §15.3 / §15.3.1 wording.

### C2 — MEDIUM (before a real KYC adapter): `kyc.OutboundKindSplitResolver` has no tests

Spot-check mutant C, described below, made the KYC resolver serve the mock resolver to every adapter regardless of kind. It **survived** `go test -tags integration ./internal/kyc/ ./cmd/platform-api/`. Casino has `internal/casino/outbound_kindsplit_test.go`; KYC has no equivalent.

Production exposure today is nil. `MockOutboundResolver` is registered with `RefuseSyntheticInProduction` (`registrations.go`, `Domain:"kyc", Name:"outbound_resolver"`) and is only wired under `testSupport`, so a production process refuses to boot with it.

The kind split is still the control that stops a synthetic credential being sent to a real vendor in a mixed test-support deployment, and it is unpinned. Port casino's kind-split tests to cover:
- synthetic adapter → mock resolver
- real adapter → real resolver
- unregistered id fails closed
- mock nil with a synthetic adapter fails closed, with no fallback to real
- both nil gives a true-nil interface

### C3 — MEDIUM (before a real KYC adapter): an unknown `provider_reference` returns 503 indefinitely, with no log line and no Retry-After

The 503 mapping is correct in principle (see Verified item 6). But:
1. **Stuck decision.** If `CreateVerification`'s phase C fails permanently after the vendor accepted the request, the row stays `unverified` with `provider_reference NULL`, and every vendor callback for that reference gets 503 until the vendor gives up. This fails closed for enforcement, but the decision is lost and nothing signals it.
2. **No log or metric.** The branch writes no log line, so an operator cannot tell a stuck decision or a retry storm from normal traffic.
3. **No Retry-After.** Other webhook 503s (admission, DB gate) set `Retry-After`; this one does not.

**Required fix:**
- Add one allow-listed `kyc_webhook_reference_unknown` log line (tenant_id, provider_id, request_id; never the body or the reference text beyond a hash).
- Add a `Retry-After` header.
- Record this stuck-decision case alongside KYC-SUBMIT-OUTBOX-1 as a real-adapter precondition: create-side reconciliation, or recording the `kv:` key or reference durably before phase C.

### C4 — LOW: doc/code mismatch on an empty document set

`SubmitVerification`'s doc comment says a verification "with no current non-rejected documents to submit at all" is a no-op. The code has no `len(submitted)==0` check and calls the provider with an empty set. The mock returns `review_required`, which then goes through the unguarded write in C1. Either implement the no-op or correct the comment. Once C1 is fixed this has no security impact.

### C5 — LOW: raw provider and resolver error text in operator logs

`create_verification_provider_unavailable` and `submit_verification_failed` log `err`, which wraps the adapter's or resolver's error text with `%v`. For example, the mock includes the provider reference. This goes to logs only, never to audit or the HTTP body, and the real `providercred` resolver returns bare sentinels. Before a real adapter lands, confirm its errors carry no PII or response-body content, or log `errors.Is` class names instead.

## Verified (no finding)

1. **No transaction held across provider calls.** Phase A of both functions commits via `pool.WithTenant` before `outbound.Resolve` and the provider call. `TestCreateVerification_NoConnectionHeldAcrossProviderCall` and `TestSubmitVerification_NoConnectionHeldAcrossProviderCall` pin this with a one-connection pool. The handler restructure resolves identity and provider selection (`GetPlayerAccountByID`, `SelectProvider`, and the verification-ownership check `verification.PlayerAccountID != account.ID`) in their own short transaction, then calls the pool-based functions.
   - The tenant is always `tc.TenantID` from the authenticated context.
   - The player id comes from the session.
   - The upload handler calls `SubmitVerification` only after the ownership-checked upload transaction committed without error.
   - Note: that transaction is `WithTenant`, not `WithTenantReadOnly`. This is informational only.
2. **Phase C runs on `context.WithoutCancel` with a 5s timeout, in both functions.** Mutant B below confirms this is test-pinned for `SubmitVerification`.
3. **`CreateVerification` phase C is a real CAS.** `WHERE id AND tenant_id AND provider_reference IS NULL AND status='unverified'`, and a result other than 1 row fails closed. A vendor-supplied reference that collides with an existing row hits migration 0040's `(tenant_id, provider_id, provider_reference)` unique index and fails. That index is tenant-scoped, so there is no cross-tenant collision. The orphan `unverified` row maps to `OutcomeFailed` in enforcement, so it fails closed.
4. **CallContext redaction.** `String`, `GoString`, `Format` (all verbs), `LogValue` and `MarshalJSON` all render only `Credential.String()`, which is the credential's own redacted form. `Call` is an exported field of `CreateVerificationInput`, so `%+v` and JSON on the input struct also go through the redactor. The test covers value and pointer forms of both types and includes a negative control. Mutant A below confirms a mutation that prints the credential secret is killed.
5. **Credential binding and fail-closed behavior.**
   - After `Resolve`, both functions check `cred.TenantID/ProviderID/Domain=="kyc"`.
   - A nil pool, provider or resolver fails with `ErrProviderUnavailable` and no panic (tested).
   - The mock resolver carries the `SyntheticComponent` marker and is guarded by `RefuseSyntheticInProduction`.
   - `kycOutboundCredentials()` wires the mock only when `b.KYC` (the mock adapter) exists.
   - An unregistered provider id fails closed. (See C2 for the test gap.)
6. **The 503 for an unknown reference is not an oracle.** It is reachable only after `VerifyCallback` succeeds and the credential is redeemed. Every pre-verification failure is still the uniform 401 "callback rejected". The lookup has an explicit `tenant_id = route tenant AND provider_id` predicate plus RLS, so a sender holding tenant A's credential gets the same 503 for tenant B's references as for references that do not exist. There is no cross-tenant signal. The body is a fixed string with no reference echo and no error text. Its distinctness from the 404 and 400 bodies is visible only to an authenticated sender. The domain transaction rolls back on this error, so no partial write or audit row is left, and redelivery is not blocked by a consumed redeem.
7. **No raw error text in audit.**
   - Phase B/C failures are logged with `slog` using ids only (phase C adds `err.Error()` of a DB error, operator logs only). No audit row is written for them.
   - Audit metadata carries `provider_id`, `document_count`, the enum `provider_outcome` and the platform-normalized `reason` (the `normalizeProviderResult` bound, unchanged).
   - No Go error text reaches `audit_log`.
8. **Idempotency keys.**
   - `kv:<verification uuid>` and `ks:<verification uuid>:<sha256 over sorted doc uuids, NUL-separated>`.
   - The verification id is a random v4 UUID unique across the platform, so keys cannot collide across tenants, or across verifications within a tenant.
   - The sha256 encodes only document ids that are already sent to the same vendor in the same call. There is no PII and no tenant data beyond what the vendor already receives under that tenant's own credential.
   - The NUL separator plus fixed-length UUIDs rule out ambiguous concatenation.
   - Whether the vendor honors the key is correctly labeled PROVIDER DEPENDENT.
9. **Cross-tenant.**
   - Every transaction is `WithTenant(tenantID)` with a server-resolved tenant.
   - `SubmitVerification` with the wrong tenant resolves nothing under RLS and fails with `ErrNotFound` (tested).
   - Phase C of Create additionally predicates on `tenant_id`.
10. **IC condition 2.** A transport error, and a `ProviderError` outcome, both leave status unchanged. Both are tested.

## Mutation spot-check (private DB)

A private database `sec_kyc_i2` was created via `TEST_ADMIN_DATABASE_URL` (owner `igaming`), migrated to 104, and `deploy/init-app-role.sql` grants were applied (with the DB name adapted). Each mutant was a single in-place edit, reverted with `git checkout`, and `git diff` was clean afterwards.

| Mutant | Edit | Test | Result |
|---|---|---|---|
| A | `callContextRedacted` renders `string(c.Credential.Secret())` instead of `c.Credential.String()` | `TestCallContext_NeverRendersSecret` | **KILLED** ("CallContext rendered the secret") |
| B | `SubmitVerification` phase C uses `context.WithTimeout(ctx, …)` (drops `WithoutCancel`) | `TestSubmitVerification_ContextCancelledBeforePhaseC_StillAppliesResult` | **KILLED** ("db: begin tx: context canceled") |
| C (extra) | `OutboundKindSplitResolver.Resolve` always prefers the mock resolver | full `./internal/kyc/` + `./cmd/platform-api/` | **SURVIVED**, see C2 |

The baseline `go test -tags integration ./internal/kyc/` passed on the private DB. The C1 probe was run separately and is not committed.

## Launch-blocking flags

- **C1** blocks marking PRH-I2 (KYC) complete, and would block production launch if left open.
- **C2**, **C3** and KYC-SUBMIT-OUTBOX-1 block registering any real KYC adapter.

---

## Re-verification: fix round `492cb20` (merged at `b9db031`)

- Reviewer: `security` specialist
- Date: 2026-09-27
- Reviewed at: `b9db031`, in a detached worktree. HEAD later moved to `f7a0da0`, but the only changes in between are docs, payments and migration 0105; no KYC code changed.
- Verdict: **C1 CLOSED. PRH-I2 (KYC part) is no longer blocked by this review.**
  - C2, C3 and C4 are closed. C5 is closed in code but has no test (N-2).
  - The F1 enforcement change is sound. It never turns a real deny into an allow.
  - A new finding, **N-1 (HIGH, pre-existing, ADR 0096)**, is not caused by this diff and does not block PRH-I2. It must be tracked, and it blocks production launch until resolved.

### Method

- Worktree: detached at `b9db031` under the session scratchpad.
- Database: private DB `sec_rv_kyc_i2_r2`, created via `TEST_ADMIN_DATABASE_URL` with owner `igaming`, migrated to 104. `deploy/init-app-role.sql` was applied, minus its `CREATE ROLE igaming` statement and with the DB name adapted.
- Baseline: `go test -tags integration` passed for `./internal/kyc/`, `./cmd/platform-api/`, `./internal/withdrawal/` and `./internal/httpserver/` (full package, run clean). `-race ./internal/kyc/` also passed.
- Mutants: each was one scripted edit, tested against the kyc, platform-api and withdrawal packages plus `httpserver -run 'KYC|Kyc'`, then reverted with `git checkout`. The tree was clean afterwards.
- The worktree and the DB were removed afterwards.

| Mutant | Edit | Result |
|---|---|---|
| R1-blind | `applyForwardOnlyStatus`: rank guard disabled and `AND status = $5` removed (the old blind write) | **KILLED**: `..._StaffRejectDuringProviderCall_Survives`, `..._CallbackApprovalDuringProviderCall_NotDemoted` |
| R1-norank | Rank guard disabled, CAS kept (a lost race overwrites whatever it re-reads) | **KILLED** by the same two tests |
| R1-lt | `newRank <= rank` changed to `<` (a same-rank result overwrites a staff decision) | **KILLED**: `..._StaffRejectDuringProviderCall_Survives` |
| F1a | Primary-read predicate replaced with `TRUE` | **KILLED**: `TestEvaluateEnforcement_OrphanAfterApproval_StillPassed` |
| F1b | Overlay inner predicate removed | **KILLED**: `..._OrphanOnAnotherAccount_DoesNotMaskRejection` |
| F1c | Both predicates widened to `NOT (status='unverified')` | **SURVIVED, equivalent** (see F1 below) |
| F1d | Both predicates narrowed to `provider_reference IS NOT NULL`, which hides decided rows that have a NULL reference | **KILLED**: 8+ `TestEvaluateEnforcement_*` |
| F1e | Overlay only narrowed to `provider_reference IS NOT NULL` | **KILLED**: `..._WithdrawalCrossAccountRejectedOverlayDenies` |
| C5-raw | `RedactedProviderErrorDetail` default case returns `err.Error()` | **SURVIVED** (N-2) |
| C4 | `len(submitted)==0` skip disabled | **KILLED**: `..._EmptyDocumentSetIsANoOp` |
| C3-log | `kyc_webhook_reference_unknown` log line removed | **KILLED**: `TestKYCWebhook_ReferenceUnknownLogging_AllowListOnly` |
| C3-retry | `writeAdmissionRejection` replaced with plain `apierror.Write` (no `Retry-After`) | **KILLED**: `TestKYC_WebhookCallbackAuthentication` |
| C2 (my original mutant C) | Kind-split resolver always prefers mock | **KILLED**: `..._NonSyntheticAdapterUsesRealOnly`, `..._SyntheticAdapterWithNilMockFailsClosed` |

When several packages ran in parallel against the one DB, `TestResolutionIsolation_*` in httpserver failed once. The same tests pass when run alone and in a clean full-package run. This is load flakiness and has nothing to do with the KYC change.

### Finding status

**C1 / R1 (HIGH): CLOSED.**
- `applyForwardOnlyStatus` is a CAS on phase A's status. On a lost race it re-reads, applies the same rank rule as the callback path, and allows 3 attempts before failing closed.
- The blind `updateVerificationStatus` is deleted. Grep finds no other status writer without a predicate. The remaining writers are Create phase C (CAS), the callback path (CAS), and `ReviewVerification` (staff, intentionally authoritative).
- The regression tests commit the concurrent staff decision and the concurrent callback *inside* the provider call, so they exercise the real lost-CAS path.
- A superseded submission still writes its audit row, with `status_applied=false`.
- Residual (informational, pre-existing, same as the callback path): staff `review_required` has rank 2, so a later provider or callback `approved` (rank 3) still moves the row forward to `approved`. Whether a staff escalation should be sticky against a vendor auto-approval is a policy question for `identity-compliance`. It is not a regression.

**F1 (enforcement orphan exclusion): VERIFIED SOUND.** Adversarial analysis:
1. **Only never-decided rows match the predicate `status='unverified' AND provider_reference IS NULL`.**
   - Only `insertOrphanVerification` ever writes `unverified`.
   - Create phase C writes a `statusForOutcome` status, or `pending`. It never writes `unverified`.
   - Callbacks can only reach rows that have a reference.
   - `ReviewVerification` only writes approved, rejected or review_required.
   - `statusRank` has no backward move to rank 0.
   - So once a row receives any decision, it leaves the predicate permanently, and no decided row can be hidden. A staff-reviewed orphan or a decided row with a NULL reference stays visible; F1d/F1e prove this is test-pinned.
2. **Can a player or attacker manufacture a matching row to hide a rejection?**
   - Players can create orphans. Only the player's own session can call `POST /me/verifications`, and a failure in phase B (including a client disconnect cancelling the request context during a real adapter's I/O) leaves an orphan.
   - After the fix, an orphan is invisible to both reads, so it cannot mask anything.
   - **Before the fix**, a newer orphan on the rejected account B *did* mask B's rejection in the overlay, which was a deny→allow. This fix closes it. ADR 0096 §19.1's statement that the pre-fix defect "never lets a real deny through as an allow" is therefore inaccurate for the overlay half (doc correction, LOW). §19 also cites a "§4 item 1 note" in this review. No such section exists; the overlay point was code review F1.
3. **Can the predicate turn a deny into an allow?** Only in one case: an approved row followed by a newer orphan. Before the fix this was `unverified→failed`; now it is `passed`, based on a still-valid approval whose `expires_at` is still checked. This is the intended fix, and the orphan carries no decision that could justify a deny. No staff or system path creates an `unverified` row to force re-verification. The only caller of `CreateVerification` is the player handler. An all-orphan account still evaluates as `found=false`, so it is denied.
4. **Consumers.** All three consumers share the single primary read `readLatestVerificationByPlayerAccount`: withdrawal hold/payout (with the overlay), the deposit threshold path and play triggers. No other enforcement "latest" read exists (verified by grep). Deposit and play have no overlay, so the only F1 effect there is point 3.
5. **The F1c equivalence is by construction.** No writer produces `unverified` with a non-NULL reference. The narrower predicate that was implemented is the fail-closed choice: if a future writer did produce such a row, it would stay visible and deny. Keep the narrow form.
6. **Minor.** The overlay repeats the predicate inline instead of using `orphanRowExclusionSQL`, which is a drift risk. F1b/F1e pin it.

**C2: CLOSED.** The KYC kind-split tests plus the wiring twin kill the always-mock mutant.

**C3: CLOSED.**
- The log line contains only request_id, tenant_id and provider_id. No reference and no body.
- `Retry-After` is set. Both halves are test-pinned.
- The permanently stuck reference is recorded under KYC-SUBMIT-OUTBOX-1, which still blocks any real adapter.

**C4: CLOSED.** Implemented and test-pinned.
- Code re-review N1 (`rv-prh-i2-kyc-code-review.md`) found that this change makes `TestSubmitVerification_TerminalVerificationIsANoOp` vacuous, so evidence M8 is no longer killed. I agree. That is a test and evidence correction, with no security impact.

**C5: CLOSED in code, untested (N-2).**
- The two named log sites now use `kyc.RedactedProviderErrorDetail`, whose output is allow-listed and never includes raw text.
- In the phase B/C slog lines inside `internal/kyc`, B logs ids only. Phase C adds DB `err.Error()`.
- `create_verification_failed` still logs raw `err` for non-provider errors. Those errors come from DB or phase C. The one realistic content is a unique-violation detail that echoes a vendor `provider_reference`. That is an opaque vendor id, not PII or a secret (informational).

**F3 (mutation evidence):** 17/17 is not accurate as stated. Code re-review N1 showed M8 surviving, and C5 has no mutant or test at all. Neither affects the security verdict.

### New findings

**N-1: HIGH, pre-existing (ADR 0096 overlay), not introduced by this diff. Does not block PRH-I2. Blocks production launch until resolved or explicitly risk-accepted by the human.**

A player can neutralise the cross-account rejection overlay with one ordinary API call.

Reproduced on the private DB with an uncommitted probe:
1. Person P has account A (approved) and account B (rejected).
2. Overlay precondition confirmed: a withdrawal from A is denied.
3. P calls the normal `CreateVerification` path on account B. The mock provider returns `pending` with a reference set.
4. A withdrawal from A is now **allowed**: `outcome=passed`, `code=kyc_withdrawal_hold:passed`.

The cause is that the overlay only looks at each other account's *latest decided* row, and a fresh `pending` counts as decided. The same thing happened before this fix round, and before the PRH-I2 split: a single-transaction create also produced a newest `pending` row.

ADR 0096 §19.4 explicitly calls this "correct, not a residual gap". I disagree. The overlay exists (security N1) so that a rejection on one account of a Person denies withdrawals on the others. A rejected player choosing to start a new verification is not new evidence and must not lift that deny. Only a later **terminal** decision on B (for example `approved`) should.

Suggested direction, to be decided by `identity-compliance` and the `architect`: base the overlay on each other account's latest *terminal* row, or on a rejection not superseded by a later terminal row, instead of its latest decided row.

Required test: `rejected(B) → fresh CreateVerification on B (pending) → withdrawal from approved A still denies`.

**N-2: LOW. `RedactedProviderErrorDetail` has no test.** The C5-raw mutant survives the whole suite. Add a unit test covering:
- a wrapped raw error with sentinel text never appears in the output
- `DeadlineExceeded`, `Canceled` and `ErrProviderUnavailable` each map to their fixed strings
- a handler-level log capture for `create_verification_provider_unavailable`

Required before a real KYC adapter is registered, alongside C2/C3.

### Launch-blocking flags (updated)

- C1 no longer blocks.
- **N-1** blocks production launch until resolved or explicitly risk-accepted by the human.
- **KYC-SUBMIT-OUTBOX-1** and **N-2** block registering any real KYC adapter.

### Scope of this re-verification

In scope: the `492cb20` diff to `internal/kyc/{provider,document_service,verification_service,enforcement,callcontext}.go` and to `internal/httpserver/kyc_{handlers,admin_handlers}.go`, plus their tests.

Not re-reviewed:
- the rest of ADR 0096's enforcement (`enforcement_admin`, `decisions_read`, `dormancy`)
- payments and casino
- the ADR 0095 prose, beyond the §15.3 wording fix

No penetration testing was done.

---

## Re-verification 2: KYC fix round 2 (`ed6d3e8`, merged at `9324189`)

- Reviewer: `security` specialist
- Date: 2026-09-27
- Reviewed at: `9324189`, in a detached worktree.
- Verdict: **N-1 PARTIALLY CLOSED. The launch block stands.**
  - The pending, review_required and orphan variants are fixed and test-pinned.
  - A new sub-finding, **N-1b**, remains: a later `expired` row on the rejected account lifts the rejection. ADR 0096 §20 says it "keeps the deny in force", so this is not what the rule intended.
- N-2/C5 is **CLOSED**.
- The ReviewVerification mirror-race CAS and the N5 orphan refusal are **VERIFIED**.
- PRH-I2 (KYC part) completion is still not blocked by this review. N-1/N-1b is a pre-existing ADR 0096 issue.

### Method

- Database: private DB `sec_rv_kyc_i2_r3`, created via `TEST_ADMIN_DATABASE_URL`, migrated to 105, with `deploy/init-app-role.sql` grants applied (minus `CREATE ROLE igaming`, DB name adapted).
- Baseline passed: kyc, platform-api and withdrawal, plus `httpserver -run 'KYC|Kyc'`.
- Probe: an uncommitted probe test. Each case starts with account A approved and account B (same Person) rejected, and confirms a withdrawal from A is denied before the variant runs. B's new rows were created through the real `CreateVerification` path and then driven by a verified callback through `receiveCallbackInTx`, not seeded directly, except where noted as seeded.
- The worktree and DB were removed afterwards, and the main tree is clean.

### N-1 probe results (withdrawal from A after the variant on B)

| Variant on B, after the rejection | B's new row | Withdrawal from A | Correct? |
|---|---|---|---|
| Fresh `CreateVerification` (the original N-1) | `pending` | **denied** | yes (fixed) |
| Create, then `review_required` callback | `review_required` | **denied** | yes (fixed) |
| Never-decided orphan | `unverified`/NULL ref | **denied** | yes |
| Create, then `rejected` callback | `rejected` | **denied** | yes |
| Create, then `approved` callback (control) | `approved` | allowed | yes (intended lift) |
| **Create, then `expired` callback** | `expired` | **ALLOWED** | **no (N-1b)** |
| `expired` row seeded directly | `expired` | **ALLOWED** | **no (N-1b)** |
| `approved` row whose `expires_at` has lapsed (seeded) | `approved` | allowed | acceptable. A genuine later approval superseded the rejection. A's own expiry is still checked on the primary read. |

### Mutants (each a scripted single edit, reverted afterwards)

| Mutant | Result |
|---|---|
| Overlay reverted to the round-1 "latest decided row" predicate | **KILLED**: `..._N1_FreshPendingVerificationDoesNotLiftRejection`, `..._N1_ReviewRequiredDoesNotLiftRejection` |
| `pending` added to `finalStatusesSQL` | **KILLED** |
| `expired` removed from `finalStatusesSQL`, i.e. `('approved','rejected')` | **SURVIVED**. The `expired` behaviour is untested. This is also the recommended fix (below). |
| C5: `RedactedProviderErrorDetail` default returns `err.Error()` | **KILLED**: `TestRedactedProviderErrorDetail_UnclassifiedErrorNeverEchoesRawText` |
| Handler `create_verification_provider_unavailable` logs `err.Error()` | **KILLED**: `TestKYC_CreateVerificationProviderFailureLog_NeverLeaksRawErrorText` |
| ReviewVerification CAS predicate `status = $6` neutralised | **KILLED**: `TestReviewVerification_ConcurrentSubmissionDuringReview_ReturnsConflict` |
| N5 upload orphan guard disabled | **KILLED**: `TestUploadDocument_OrphanVerificationFailsClosed` |
| N5 submit orphan guard disabled | **KILLED**: `TestSubmitVerification_OrphanVerificationFailsClosed` |

### N-1b: HIGH (latent, PROVIDER DEPENDENT reachability), blocks production launch with N-1

The cause is the overlay's inner subquery: `AND v2.status IN ('approved','rejected','expired')`. The outer query only matches when B's latest final row *is* the rejected row. So any newer `expired` row on B makes the overlay return false, and the rejection is lifted.

ADR 0096 §20 and the `crossAccountRejectedOverlay` comment both claim the opposite: that a later `expired` "simply keeps the deny in force under a fresh terminal row". It does not. An `expired` status means an attempt lapsed. That is not evidence that clears a rejection.

**Reachability:**
- A player can start a new verification on B at will.
- Whether that player can then cause the vendor to emit `expired`, for example by abandoning the applicant flow, is PROVIDER DEPENDENT. Many KYC vendors do emit such a status.
- The mock adapter cannot be driven to `expired` by player action, so this is not reachable in dev today. It becomes reachable as soon as a real adapter maps an abandonment or lapse outcome to `ProviderExpired`.

**Required fix:**
- Lift a rejection only with a later final `approved`. Use `('approved','rejected')` in the overlay's subquery, or equivalently "a rejected row on another account with no later `approved` row on that account".
- Add the test `rejected(B) → Create + expired callback on B → withdrawal from approved A still denies`. The dropped-`expired` mutant must then be killed by that test, not survive.
- Correct the ADR 0096 §20 and code-comment wording.

### Other items

- **Consumer claim: VERIFIED.** `EvaluateEnforcement` dispatches by operation.
  - `isWithdrawalOperation` covers `withdrawal_hold` (`withdrawal.go:374`) and `withdrawal_payout` (`payments/payout.go`). It goes to `evaluateWithdrawalStructuralRule`, the only caller of `crossAccountRejectedOverlay`.
  - Deposit (`payments/kycgate.go`) goes to `evaluateDepositThreshold`. Casino and sportsbook play go to `evaluatePlayTrigger`. Neither consults the overlay; both use only the unchanged per-account primary read.
  - The overlay only runs after the primary outcome is `passed`, so it can only turn an allow into a deny, never the reverse.
- **N-2/C5: CLOSED.** There is now a unit test and an HTTP-level log-capture test, and both mutants are killed.
- **ReviewVerification mirror race: VERIFIED.** The UPDATE predicates on the status read at the start of the review. A lost race returns `ErrVerificationStatusConflict`, which maps to 409, so there is no silent overwrite of a concurrent provider transition. The callback path now shares `applyForwardOnlyStatus`, with the same semantics as before.
- **N5: VERIFIED.** Upload and submit both refuse a verification with no `provider_reference`. The check runs inside the tenant-scoped transaction, after the handler's ownership check, and fails closed with 409.
- **Informational (LOW, pre-existing):**
  - Both reads order rows by `created_at`, not by decision time. A rejection decided *after* a newer-created row's approval on the same account is ordered as older. This matters only with overlapping in-flight attempts, and the N5 and single-flight behaviour narrow it. Recorded for identity-compliance; not blocking.
  - The staff `review_required` → provider `approved` forward move remains a recorded policy item.

### Launch-blocking flags (updated)

- **N-1b** blocks production launch; the rest of N-1 is closed.
- **KYC-SUBMIT-OUTBOX-1** still blocks registering a real KYC adapter.
- N-2 no longer blocks anything.

### Scope

In scope: the `ed6d3e8` diff to `internal/kyc/{enforcement,provider,document_service,verification_service}.go` and to `internal/httpserver/kyc_{handlers,admin_handlers}.go`, plus their tests, and ADR 0096 §20 as far as it describes the overlay.

Not re-reviewed:
- the rest of ADR 0096/0095
- payments, casino and sportsbook, beyond confirming their `EvaluateEnforcement` operations

No penetration testing was done.

---

## Re-verification 3: KYC fix round 3 (`3671e0e`, merged at `24a9dad`)

- Reviewer: `security` specialist
- Date: 2026-09-27
- Reviewed at: `24a9dad`, in a detached worktree.
- Verdict: **N-1 and N-1b are CLOSED.** The full probe matrix is correct and the tests pin it.
  - A new finding, **N-3 (MEDIUM, introduced by this round)**: the new staff-sticky `review_required` gate also discards a vendor **rejection**, silently. It must be narrowed before the sticky-gate change (KYC-REVIEWREQ-FORWARD-1) is marked complete.
  - N-3 is also a launch blocker until fixed.
  - The rest of this round (the create-side no-reference guard, the 409s, the R2-2 redaction test) is verified.

### Method

- Database: private DB `sec_rv_kyc_i2_r4`, migrated to 105, with `deploy/init-app-role.sql` grants applied (minus `CREATE ROLE igaming`, DB name adapted).
- Baseline passed: kyc, platform-api and withdrawal, plus `httpserver -run 'KYC|Kyc'`.
- Probe: an uncommitted probe test.
  - Matrix cases start with account A approved. Account B (same Person) is rejected via the real `CreateVerification` path plus a verified `rejected` callback.
  - Each variant row is then created with `CreateVerification` and driven through `receiveCallbackInTx`, except where noted as seeded.
- The worktree and DB were removed afterwards, and the main tree is clean.

### N-1 / N-1b probe matrix (withdrawal from A)

| Sequence on B | Withdrawal from A | Correct? |
|---|---|---|
| rejected → fresh create (`pending`) | denied | yes |
| rejected → `review_required` callback | denied | yes |
| rejected → orphan | denied | yes |
| rejected → `expired` callback | denied | yes (N-1b fixed) |
| rejected → `expired` seeded directly | denied | yes (N-1b fixed) |
| rejected → `approved` callback (control) | allowed | yes |
| rejected → `expired` → `approved` | allowed | yes (the later approval lifts it) |
| rejected → `approved` → `expired` | allowed | yes (`expired` is ignored; the approval stands) |
| rejected → `approved` → `rejected` | denied | yes |

### Mutants (each a scripted single edit, reverted afterwards)

| Mutant | Result |
|---|---|
| `expired` put back into `finalStatusesSQL` | **KILLED**: `..._N1b_ExpiredOnRejectedAccountDoesNotLiftRejection_ViaCallback`, `..._SeededDirectly` |
| Sticky gate removed (SQL predicate plus re-read short-circuit) | **KILLED**: `TestApplyForwardOnlyStatus_StaffSetReviewRequiredIsStickyAgainstProviderApproval` |
| Sticky gate narrowed to `newStatus = approved` only, so a rejection still applies | **SURVIVED**. The rejection-blocking behaviour is not pinned by any test. This is also the N-3 fix. |
| Create-side "unrecognized outcome with no reference" guard disabled | **KILLED**: `TestCreateVerification_UnrecognizedOutcomeWithNoReferenceLeavesOrphanUntouched` |

### Sticky-gate abuse analysis

**Can a player influence `reviewed_by`? No.**
- The only writer is `ReviewVerification`. The only route to it is the back-office endpoint behind `auth.RequirePermission(auth.PermVerificationReview)` plus `RequireTenantScope`.
- No player route, callback path, `applyForwardOnlyStatus`, or create/submit path writes it.
- A player also cannot set `review_required` with a non-NULL `reviewed_by`.

**Can a staff `review_required` block a legitimate REJECTION? Yes. Finding N-3.**

Reproduced:
1. A is approved. B has a new verification with a reference, and a compliance officer sets it to `review_required`.
2. The vendor then sends a verified `rejected` callback for B.
3. Result:
   - B stays `review_required`.
   - **Zero** `kyc.provider_callback` audit rows are written, because `applyCallbackOutcome` writes no audit row when `applied=false`.
   - The webhook is acknowledged as a no-op, so the vendor does not redeliver.
   - **Withdrawal from A is ALLOWED.**
4. Control: the same sequence with a *provider*-set `review_required` ends with B `rejected`, 2 audit rows, and withdrawal from A **denied**.

### N-3: MEDIUM, introduced by `3671e0e`; blocks completing KYC-REVIEWREQ-FORWARD-1, and blocks production launch until fixed

The gate added to `applyForwardOnlyStatus` is `AND NOT (status='review_required' AND reviewed_by IS NOT NULL)`, plus a matching re-read no-op. It blocks *every* provider-driven forward move, including `rejected` and `expired`.

Identity-compliance Ruling 2's title and rationale concern only automated moves to **`approved`**, which is a vendor overriding a human escalation. Blocking a vendor **rejection** weakens enforcement instead:

1. **Cross-account deny lost.** A vendor rejection of the Person is discarded, so the N-1 overlay never sees a `rejected` row on B. Withdrawals from the Person's other approved accounts stay allowed for as long as the case sits in review. Account B itself stays denied, because `review_required` maps to pending.
2. **Silent evidence loss.** No audit row records the discarded rejection. The vendor stops redelivering, and nothing surfaces the rejection to the officer. The officer can later `approve` the case without ever knowing the vendor rejected it.
3. **Insider vector.** One officer with `PermVerificationReview` can escalate a case to `review_required` just before an expected vendor rejection. That suppresses the rejection with no trace of it. The escalation itself is audited; the discarded rejection is not.

**Required fix:**
- Apply the sticky rule only when `newStatus = approved`. Let `rejected` (and `expired`, which the overlay now ignores anyway) apply through the normal forward-only CAS.
- Add a test: staff `review_required` → vendor `rejected` callback → row is `rejected`, and a withdrawal from the Person's other approved account is denied. That test must kill the approved-only mutant's inverse.
- Separately, even for the blocked-approval case, write an audit row (for example `kyc.provider_callback` with `status_applied=false` and the provider outcome) whenever a provider result is discarded on a staff-sticky row, so the officer sees the vendor's view.

This is a correction to the implementation's scope, not to Ruling 2.

### Other items

- **Create-side no-reference guard: VERIFIED.** An unrecognized outcome with no reference returns `ErrProviderUnavailable` inside the phase-C transaction, which rolls back and leaves the orphan untouched. It is test-pinned.
  - LOW, informational: `kyc_create_verification_phase_c_failed` logs `err.Error()`, which now embeds the adapter-supplied outcome string via `%q`. That value is unbounded vendor-controlled text in operator logs. Bound or classify it before a real adapter lands, as with C5.
- **HTTP 409s and the R2-2 redaction test:** present and passing in the baseline. I did not mutation-test them individually this round.
- **Primary-account read:** unchanged. The overlay can still only turn an allow into a deny, and only for withdrawal hold and payout. That dispatch is the same as in re-verification 2.

### Launch-blocking flags (updated)

- **N-3** blocks production launch.
- **N-1 and N-1b** are closed.
- **KYC-SUBMIT-OUTBOX-1** still blocks registering a real KYC adapter.

### Scope

In scope: the `3671e0e` diff to `internal/kyc/{enforcement,provider,verification_service}.go` and its tests, plus Ruling 2 as it relates to the gate.

Not re-reviewed:
- ADR 0096 §21 prose, beyond the overlay semantics
- `kyc_round3_http_test.go`, beyond confirming it passes

No penetration testing was done.

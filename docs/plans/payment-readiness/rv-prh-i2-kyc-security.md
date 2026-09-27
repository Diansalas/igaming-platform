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

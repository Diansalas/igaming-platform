# Gate 10.3-W1 — identity-compliance review: KYC-REASON-BOUND-1 (W1d) and the KYC parts of W1a

Reviewer: `identity-compliance`. Repo `/home/user/igaming-platform`, branch `claude/focused-wright-jw88w9`, HEAD `ee2192f` (plus one unrelated unstaged modification to `internal/ledger/migration_0092_integration_test.go`, outside this review's scope). Read-only review: ran `go vet ./internal/kyc/... ./internal/httpserver/...` (clean) and `go test ./internal/kyc/... ./internal/httpserver/...` (both `ok`; integration tests intentionally not run, per instructions). No code, migration, or doc other than this file was written or committed.

## VERDICT: APPROVE WITH CONDITIONS

## Scope

- Diff `4a2a978..HEAD` for `internal/kyc`, `internal/httpserver/kyc_*`, `migrations/0095_*`, and the KYC OpenAPI schemas.
- ADR 0028's Stage 10.3 amendment ("Amendment (Stage 10.3, ADR 0092) — provider reason bound").
- HD-10.3-3 (players see STATUS ONLY; the bounded provider reason is available only to authorised compliance/back-office workflows), §22 of `docs/plans/stage-10.3-planning-gate-proposal.md`.
- My own design paper, `docs/plans/stage-10.3-planning/03-kyc-reason-bound-analysis.md`.
- Gate log `05-gate-log.md`, items 5 and 6.

## Concurrence with the narrowed ADR 0028 amendment

I concur with the architect's amendment. It correctly supersedes my paper's player-facing `reason_code` enum/column per HD-10.3-3 — that enum's exact members and copy is a product/legal decision (jurisdiction-specific disclosure rules; possible AML tip-off concerns), not an engineering one, and "status only" is the correct safe default until that decision is made. The implementation at HEAD matches the amendment's decision items 1–6 in every respect except one disclosed gap (see below).

## Verification against the seven checks

### 1. Players see status only on every player surface

`playerVerificationResponse` (`internal/httpserver/kyc_handlers.go:74-92`) has no `reason`/`reason_code` field at all, and is the only response type used by the two player-facing verification routes, `newCreateMyVerificationHandler` and `newListMyVerificationsHandler` (`kyc_handlers.go:125-209`). The OpenAPI `PlayerVerification` schema (`docs/api/openapi/platform-api.yaml`) drops `reason` entirely and its description records HD-10.3-3 explicitly. `TestKYCHandlers_PlayerResponse_NeverContainsRawReason` (`internal/httpserver/kyc_reason_authorization_integration_test.go`) checks the **raw JSON wire body**, not just the typed struct, for both POST and GET across every `ProviderOutcome` fixture (approved/rejected/review_required/expired), and additionally asserts the raw provider-reason marker string never appears anywhere in the body. No other player route returns a `kyc.Verification`-shaped body. **Pass.**

### 2. Staff see only the bounded, sanitised reason, tenant-scoped, permission-gated

The staff shapes (`verificationResponse`, `kycCaseResponse`) carry the bounded `reason`. Every `/v1/admin/kyc/*` route (`internal/httpserver/kyc_routes.go:27-40`) is wrapped in `auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead | auth.PermVerificationReview)(...))`, and `kyc_verifications` carries `FORCE ROW LEVEL SECURITY` (migration 0040) — tenant scoping is RLS's own, never an application WHERE clause. `TestKYCVerifications_TenantIsolation_Reason` proves: tenant B's staff see zero rows and never the reason substring via both the per-account list route and the tenant-wide case queue; a cross-tenant review attempt 404s (not 403, consistent with the codebase's existing "never confirm existence" discipline) and its body never carries the reason; and a same-tenant positive control proves the isolation is genuinely tenant-scoped rather than "the field never leaves the server" trivially passing. **Pass.**

### 3. The staff review path validates input, never a 500

`ReviewVerification` (`internal/kyc/verification_service.go`) now calls `NormalizeReason` on the staff-supplied `Reason` before any DB write:

```go
params.Reason, _ = NormalizeReason(params.Reason)
```

So an oversized or control-character-laden staff reason is normalized (bounded/cleaned), not rejected and not passed through raw to the database, meaning migration 0095's `CHECK` constraint can never surface as an opaque 500 from this path. `TestReviewVerification_OversizedReason_NormalizedNeverReachesDBAsError` proves this against a 4KB+, control/bidi-laden fixture, asserting both the returned and the **persisted** value are bounded and clean. `TestReviewVerification_CleanShortReason_StoredUnchanged` is the correct no-false-positive-mutation control, proving an already-clean short reason survives unchanged. **Pass.**

### 4. Migration 0095 behaves safely per tenant under FORCE RLS

The up-migration's pre-flight correctly avoids the migration-0048 trap (a blind `UPDATE ... WHERE <predicate>` under `FORCE ROW LEVEL SECURITY` with no `app.tenant_id` set silently affecting zero rows, not failing loudly): it loops over every row of the RLS-exempt `tenants` table, and for each one sets `app.tenant_id` to that tenant's own id (the same GUC ordinary application code uses via `db.Pool.WithTenant`) before touching that tenant's own `kyc_verifications` rows — `FORCE ROW LEVEL SECURITY` is never toggled off at any point. `TestMigration0095_PreflightNormalizesOversizedRowsAcrossTenantsBeforeConstraint` (`internal/kyc/migration_0095_integration_test.go`) seeds oversized/control-laden rows in **two separate tenants**, applies the migration, and proves **both** tenants' rows were normalized (not just the first one reached), then proves the `CHECK` constraint is live afterward via a fresh 513-byte insert failing with SQLSTATE `23514`. `TestMigration0095_UpDownUpRoundTrip` proves a clean, symmetric down/up/down cycle with no data-transformation concern on the way down (consistent with the down-migration file's own comment). **Pass.**

### 5. The KYC conformance reason case

`internal/kyc/conformance_test.go` adds a mandatory, fail-not-skip case (matching the existing tenant-binding-case precedent, F-9 already resolved for KYC): any adapter that is not `*MockKYCProvider` fails outright with an explicit message naming the `KYC-REASON-BOUND-1` obligation, rather than silently skipping — this correctly forecloses a future real adapter shipping unbounded or unclean reasons undetected. The mock's own fixture is exercised through the real `HandleCallback` path with a deliberately oversized, control/bidi-laden payload. `TestReasonBoundConformanceSelfTest_DetectsNonConformingReason` is the required self-test proving the assertion helper (`reasonConformanceViolation`) actually detects non-conformance on a deliberately dirty fixture and passes on an already-normalized one — not a check that trivially always succeeds. **Pass.**

### 6. No enforcement change to KYC/RG blocking

No diff to `internal/rg` or `internal/selfexclusion` anywhere in the reviewed range. The only enforcement-adjacent changes in `4a2a978..HEAD` are W1c's casino capability/rollback changes (`CAS-CAP-ROLLBACK-1`) and W1a's webhook-scheme refactor — both out of this review's remit, and neither touches KYC/RG gating logic. `kyc_verifications.status` still does not gate a withdrawal (unchanged since Stage 10.2, consistent with ADR 0028 §1's deliberate non-collapse of KYC status into `PlayerAccountStatus`). **Pass.**

### 7. W1a's KYC verify path (webhook_verify.go) preserves Stage 10.2 forward-only status and no-audit-on-replay behaviour

The refactor moves the pre-verification steps (adapter/scheme lookup, header/MAC extraction, credential resolution, `scheme.Verify`) out of `Orchestrator.ReceiveCallback` and into `Orchestrator.verifyCallback` (`internal/kyc/webhook_verify.go`), which is explicitly documented and structured to issue **no database statement at all** ("strict I1") — preserving the property that a callback failing verification never reaches the DB or the audit trail. `applyCallbackOutcome` (`internal/kyc/provider.go:351-440`), which owns the forward-only `statusRank` comparison and the `applied` (written vs. no-op) determination gating the single allow-listed `kyc_webhook_noop` info log line with **no** audit row for a replay/no-op, is **unchanged** by this diff — the refactor only relocated *where* verification happens (orchestrator-enforced, per adapter `WebhookScheme()`, Stage 10.3 W1a `WH-VENDOR-SCHEME-1`), not the replay/idempotency logic itself. **Pass.**

## One disclosed gap — condition for full closure

ADR 0028's Stage 10.3 amendment, Decision item 1, fourth bullet, is explicit and binding:

> [`NormalizeReason`] truncates to at most **512 bytes** without splitting a rune; records any truncation as a flag in the audit metadata.

At HEAD, `NormalizeReason(raw string) (bounded string, truncated bool)` (`internal/kyc/reason_normalize.go`) does return the `truncated` signal, but **every call site discards it**:

- `internal/kyc/mock_provider.go` (4 call sites: `CreateVerification`, `GetVerification`, `SubmitVerification` x2, `HandleCallback`) — all `reason, _ := NormalizeReason(...)`.
- `internal/kyc/verification_service.go`'s `ReviewVerification` — `params.Reason, _ = NormalizeReason(params.Reason)`.

The `audit_log.metadata` entries written in `applyCallbackOutcome` (`internal/kyc/provider.go:398,438`) carry only `"provider_id"`, `"provider_outcome"`, and `"reason"` — never a truncation flag. I grepped the whole tree for `truncated`/`reason_truncated` outside `reason_normalize.go` itself and its own unit test; the signal is computed and then dropped everywhere it is produced.

This is a small, low-risk, mechanical fix (thread the existing `truncated` return value into the existing `Metadata` map at the two audit-write sites in `provider.go`, and into whatever audit entry `ReviewVerification`'s own staff-reason write already produces, if any), but it is a specific, named commitment in a binding architect ruling that the code does not currently meet. Per CLAUDE.md's "No fake completion" and this agent's own charter ("any exception requires an explicit, recorded decision, not a quiet code change"), this should not be silently folded into an `IMPLEMENTED` label — it needs either the fix or an explicit, dated recorded exception.

**Secondary, non-blocking note.** Amendment item 5 also states: "Staff UIs must HTML-escape [the bounded reason], and a test must prove they do." `backoffice/src/features/kyc/KycCaseDetail.tsx` renders `kyc.reason` via plain JSX text interpolation, and a repo-wide grep found no `dangerouslySetInnerHTML` anywhere under `backoffice/src` — so React's default escaping already covers this by construction. There is no backoffice diff in this wave, and no new test explicitly proving the escaping. I do not consider this blocking (the underlying mechanism is sound and pre-existing, not a regression), but it is worth a small follow-up test to close the letter of the amendment.

## Conditions for APPROVE

1. Wire `NormalizeReason`'s `truncated` return value into `audit_log.metadata` (e.g. a `"reason_truncated": bool` key) at every write site currently discarding it (`internal/kyc/provider.go:394-398,434-438`, `internal/kyc/mock_provider.go`'s four call sites feeding those, and `ReviewVerification`'s own audit write for the staff reason path, if one records `reason`) — **or** record an explicit, dated exception against `KYC-REASON-BOUND-1` in the task registry / ADR if this is deliberately deferred.
2. Track (non-blocking to this gate) a small backoffice test proving the staff KYC case-detail view renders the reason as escaped text, to close the letter of amendment item 5.

## Everything else checked and found sound

- Identity/status-only split for players, correctly implemented and tested at the raw-JSON level, not merely the typed-struct level.
- Staff-only bounded reason with correct tenant RLS and permission scoping (`PermVerificationRead`/`PermVerificationReview`), proven with a positive same-tenant control.
- Staff input at the review boundary is normalized, never a raw 500 from the new `CHECK` constraint.
- Migration 0095's per-tenant pre-flight is FORCE-RLS-safe (no toggle of the FORCE flag, no migration-0048-style silent zero-row update), proven with a genuine two-tenant fixture, plus a clean up/down/up round trip.
- The KYC conformance suite's new reason-bound case is mandatory (fails, not skips) for any non-mock adapter, with a self-test proving the check itself is meaningful.
- No KYC/RG enforcement path was touched by this wave.
- W1a's orchestrator-enforced webhook verification refactor preserves the Stage 10.2 forward-only rank transition and no-audit-on-replay/no-op behaviour unchanged.
- `KYC-DOC-REJECTION-BOUND-1` (gate log item 6 — `kyc_documents.rejection_reason`, staff-entered, unbounded, and returned on player document routes) is correctly disclosed as out of this task's scope and is registered separately in the task registry, not silently dropped or conflated with this wave's fix.

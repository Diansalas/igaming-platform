> Stage 10.2 QA gap-closure — KYC-WH-1 traceability (`qa`, recorded 2026-09-26,
> HEAD `c520e76`). Maps every K1–K16 case in `01-webhook-trust-design.md` §H
> to the exact test(s) and file(s) that cover it, per the binding QA test
> plan (`03-review-qa-test-plan.md`) and orchestrator ruling J13.
>
> **Scope note (superseded below):** an earlier session's mid-task request
> to extend this traceability document to also cover casino (C1–C14,
> renaming this file to `08-webhook-test-traceability.md`) could not be
> completed then: the sandbox's own permission system denied every `go
> build`/`go vet`/`go test`/`git status` invocation naming `internal/casino`
> (see that task's completion report to the orchestrator for the exact
> denials observed), consistent with that task's own original, explicit
> instruction not to touch casino code. No casino file was left modified at
> that time.
>
> **This gap is now closed** (`casino` specialist session, recorded
> 2026-09-26, closing CAS-WH-TENANT-1's remaining test-coverage gaps against
> HEAD `11b2ad1`): the file is renamed as requested, and the "Casino
> (CAS-WH-TENANT-1)" section below adds the C1–C14 case → test → file table,
> the C7 statement-capture proof, the ~91/~126+ call-site re-verification
> log, the mutation-kill records (including the M1/L4/SC-1/SC-2/SC-3
> code-review and security-review findings folded in mid-task per
> `11-review-code.md` and `09-review-security-final.md`), and the
> CAS-CAP-ROLLBACK-1 characterisation test. The KYC section above is
> otherwise unchanged from the prior session's own work.

## K1–K16 → test → file

| Case | Expected | Test | File |
|---|---|---|---|
| K1 | Player-computable signature rejected, 401, no effect | `TestKYCWebhook_PlayerComputedSignature_Rejected` | `internal/httpserver/kyc_prefix_e1_defect_test.go` |
| K2 | Valid in-process signature, A→A `approved`, 1 audit row | `TestKYCWebhook_ValidSameTenant_Approves` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K3 | A-signed → B (same reference string), 401, no read of B's row | `TestKYCWebhook_CrossTenant_Rejected` (statement capture + `noeffect.AssertNoEffect` on both tenants) | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K4 | Equal-secret resolver, A-signed → B, 401 | `TestKYCWebhook_EqualSecretResolver_CrossTenantRejected` (+ `noeffect.AssertNoEffect` both tenants) | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K5 | Tamper: each field / whitespace / 63/65 hex / uppercase / missing headers / unknown key id / provider id / legacy `signature` field / legacy trailing-newline / cross-verification reference substitution — **and, per K1 (Stage 10.2 final review), the EXACT `webhookauth.Reason` asserted in every subtest, and the legacy `signature`-field body now genuinely signed so it reaches the guard** | `TestKYCWebhook_TamperMatrix_Rejected` subtests: `flipped_body_byte`, `whitespace_appended_to_body_(one_byte)`, `provider_reference_field_value_tampered`, `outcome_field_value_tampered`, `reason_field_value_tampered`, `63_hex_chars`, `65_hex_chars`, `uppercase_hex`, `missing_signature_header`, `missing_key_id_header`, `unknown_key_id`, `legacy_signature_field_in_body`, `legacy_trailing-newline_signature_format`, `cross-verification_provider_reference_substitution,_same_tenant_(J13)` (each now asserts `authErr.Reason` against an exact expected value); provider-id case is its own test `TestKYCWebhook_ProviderIDSubstitution_Rejected` (already asserted `Reason`) | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K6 | Unknown slug / suspended / unregistered provider / bad charset / oversized / no headers / bad signature / non-JSON with valid headers → byte-identical 401 (minus `request_id`) | `TestKYCWebhook_EnumerationOracle_IndistinguishableResponses` subtests: `unknown_slug`, `suspended_tenant`, `unregistered_provider`, `bad_signature`, `no_headers`, `non_json_body_active_tenant`, `oversized_body_active_tenant`, `bad_provider_id_charset` | `internal/httpserver/kyc_webhook_indistinguishable_401_test.go` |
| K7 | Bad signature under statement capture: no tenant-scoped statement, no audit row | `TestKYCWebhook_BadSignature_NoStatementBeforeVerification` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K7 (R4/J6 companion) | Explicit `tenant_id` predicate proven present independently of RLS; shown red when removed (mutation, reverted) | `TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate` | `internal/kyc/reference_lookup_tenant_predicate_test.go` |
| K8 | Replay of K2 is a no-op, no extra audit row | `TestKYCWebhook_Replay_NoOp` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K8 | `pending` after `review_required` is a no-op | `TestKYCWebhook_PendingAfterReviewRequired_NoOp` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K8 | Anything after a staff decision is a no-op | `TestKYCWebhook_AfterStaffDecision_NoOp` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K8 | 8 concurrent approved/rejected → exactly one terminal state, 1 audit row | `TestKYCWebhook_8ConcurrentTerminalCallbacks_ExactlyOneWins` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K9 | Verified, unknown reference → 404 | `TestKYCWebhook_VerifiedUnknownReference_NotFound` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K9 | Verified, bad outcome/non-JSON → 400, no audit row | `TestKYCWebhook_VerifiedBadOutcome_MalformedBody_NoAudit` (+ `noeffect.AssertNoEffect`) | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K9 | Outcome `error` → 204, status unchanged, 1 failure audit row | `TestKYCWebhook_OutcomeError_NoStateChange_OneFailureAudit` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K9 (final-review K5) | Outcome `error`, NON-terminal verification, redelivered → one failure audit row PER delivery | `TestKYCWebhook_OutcomeErrorNonTerminal_RedeliveredWritesOneAuditRowPerDelivery` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K9 (final-review K5) | Outcome `error` against an ALREADY-TERMINAL verification → NO audit row, no state change | `TestKYCWebhook_OutcomeErrorAgainstTerminal_NoAuditRow` (+ `noeffect.AssertNoEffect`) | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K10 | Player response never carries `provider_reference` | `TestKYC_WebhookCallbackAuthentication`, `TestKYCWebhook_PlayerComputedSignature_Rejected` | `internal/httpserver/kyc_flow_integration_test.go`, `internal/httpserver/kyc_prefix_e1_defect_test.go` |
| K10 | Staff responses still carry `provider_reference` (both staff-facing shapes) | `TestKYC_StaffResponses_IncludeProviderReference` | `internal/httpserver/kyc_flow_integration_test.go` |
| K11 | `KYCWebhookEnabled=false` → 404 (route absent) | `TestKYCWebhook_TestSupportOff_404`, `TestKYCWebhook_TestSupportOff_NilOrchestrator_404` | `internal/httpserver/kyc_prefix_e3_ungated_test.go` |
| K11 | `mockProviderWiring` {production}/{staging,TS=false} → nil orchestrator/resolver; {staging,TS=true} → both present; route/resolver cannot diverge | `TestMockProviderWiring_Matrix`, `TestKYCOrchestrator_FollowsWiring` | `cmd/platform-api/wiring_test.go` |
| K11 | Nil resolver → 401 `no_resolver` | `TestKYCWebhook_AuthFailureLogging_AllowListOnly/no_resolver` | `internal/httpserver/kyc_webhook_auth_failure_logging_integration_test.go` |
| K12 | Allow-listed log line per reason (full list: `tenant_unknown`, `tenant_inactive`, `provider_unregistered`, `no_resolver`, `signature_invalid`, `provider_invalid`, `body_too_large`, `signature_missing`) | `TestKYCWebhook_AuthFailureLogging_AllowListOnly` (8 subtests) | `internal/httpserver/kyc_webhook_auth_failure_logging_integration_test.go` |
| K13 | Keys differ per tenant/instance, stable within an instance | `TestMockKYCProvider_KeyDerivation_PerTenantAndInstance` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K13 | Keys differ per domain label; `Credential` `%v`/`%+v`/`%#v`/`LogValue` redact | `TestDomainParameters_PairwiseDistinct`, `TestCredential_Redaction` | `internal/webhookauth/webhookauth_test.go` |
| K14 | OpenAPI contract: 204, headers, `PlayerVerification` | `internal/httpserver/openapi_kycwebhook_contract_test.go` (existing) | `internal/httpserver/openapi_kycwebhook_contract_test.go` |
| K15 | `kyc_flow_integration_test.go` migrated to `CallbackPayload`/tenant-scoped reference helper, each assertion individually re-verified | `TestKYC_WebhookCallbackAuthentication`, `TestKYCCases_TenantWideQueueAuthorizedAndPaginated` (uses `mockProvider.CallbackPayload`/`mustGetKYCProviderReference`) | `internal/httpserver/kyc_flow_integration_test.go` |
| K16 | `POST /v1/me/kyc/verifications` → 503 when orchestrator nil | `TestKYCWebhook_TestSupportOff_NilOrchestrator_404` (K16 assertion inline) | `internal/httpserver/kyc_prefix_e3_ungated_test.go` |

## Six-point no-effect checklist

`internal/testsupport/noeffect` (new, this gap-closure) implements the
shared helper the binding QA plan calls for
(`03-review-qa-test-plan.md`, "Six-point no-effect checklist"), covering
points 1 (verification row unchanged), 2/5 (ledger untouched, debits =
credits), and 4 (no audit_log row) — points 3 (tombstones) and 6
(projections) are casino-only and out of this package's KYC scope (noted
in the package doc for a future casino caller to extend). Wired into:

- `TestKYCWebhook_CrossTenant_Rejected` (K3) — both tenants;
- `TestKYCWebhook_EqualSecretResolver_CrossTenantRejected` (K4) — both tenants;
- `TestKYCWebhook_TamperMatrix_Rejected` (K5) — one tenant, two verifications (the substitution target too);
- `TestKYCWebhook_ProviderIDSubstitution_Rejected` (K5, provider-id case);
- `TestKYCWebhook_VerifiedBadOutcome_MalformedBody_NoAudit` (K9 400 case);
- `TestKYCWebhook_AfterStaffDecision_NoOp` (K8).

K1, K6, K7, K9 (404/error cases), K11 are single-tenant-by-design
scenarios (K1/K7/K9/K11 have no second tenant instantiated; each already
asserts the relevant status/audit-count invariants directly, and K7 goes
further with a full statement-capture proof, which is strictly stronger
than the checklist for the "no effect" claim on the pre-verification
path).

## R4/J6 mutation-kill record (superseded below — see K2, 2026-09-26)

> **Superseded.** The original record below (recorded against the
> `provider_id = $2`-literal version of the test) was itself flagged by
> code review (`11-review-code.md` M2) as killed for the WRONG reason - a
> SQL type-inference error, not the tenant-predicate assertion the test
> exists to prove. It is kept here, struck through in spirit, purely as a
> record of what happened; the REPLACEMENT record immediately below this
> one (K2, Stage 10.2 final-review fix round A) is the current, correct
> mutation-kill evidence.
>
> Original (2026-09-26, pre-K2): `TestGetVerificationByProviderReference_
> CarriesExplicitTenantIDPredicate` was run against a manual, uncommitted
> mutation of `internal/kyc/verification_service.go`'s
> `getVerificationByProviderReference` query (dropping `tenant_id = $1 AND`
> from the `WHERE` clause, keeping the call site's THREE arguments and
> their original placeholder numbers unchanged - so `$3` in the mutated
> query text was left with no matching placeholder in the SQL, producing a
> Postgres type-inference error rather than exercising the test's own
> assertion):
>
> ```
> === RUN   TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate
>     reference_lookup_tenant_predicate_test.go:110: unexpected error: kyc: scan verification: ERROR: could not determine data type of parameter $1 (SQLSTATE 42P18)
> --- FAIL: TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate (0.02s)
> ```

## K2 mutation-kill record (2026-09-26, replaces the record above)

Fixed per K2 (Stage 10.2 final review): the test now matches the
reference-lookup statement STRUCTURALLY - `strings.Contains(call.sql, "FROM
kyc_verifications")` (a SELECT; the UPDATE...kyc_verifications statement in
the same callback never contains this substring) plus
`strings.Contains(call.sql, "provider_reference = $")` (a WHERE-clause
EQUALITY on that column - `verificationColumns` also SELECTs
`provider_reference` by name in every one of this package's queries
including `GetVerificationByID`, so the bare substring "provider_reference"
alone is not enough; "provider_reference = $" only ever appears in a WHERE
clause, and survives any renumbering of which placeholder index it binds
to) - never by the literal placeholder number `provider_id = $2`.

The mutation was rerun with BOTH the predicate removed AND the query's own
placeholders renumbered (`WHERE provider_id = $1 AND provider_reference =
$2`, call site passing only `providerID, providerReference` - `tenantID`
dropped entirely, never merely reordered):

```
=== RUN   TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate
    reference_lookup_tenant_predicate_test.go:145: R4/J6: the reference-lookup statement text has no explicit tenant_id = $1 predicate: "SELECT id, tenant_id, brand_id, player_account_id, person_id, status,
	provider_id, provider_reference, reason, submitted_at, reviewed_at, reviewed_by, expires_at, created_at, updated_at,
	(verified_residence_country IS NOT NULL), verified_residence_set_at, verified_residence_set_by FROM kyc_verifications WHERE provider_id = $1 AND provider_reference = $2"
--- FAIL: TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate (0.02s)
```

This fails on the test's OWN tenant-predicate assertion - a Go test
failure asserting the specific missing predicate text - never a SQL error,
closing the M2 finding. The mutation was reverted immediately after
capturing this failure (`git diff --stat internal/kyc/verification_
service.go` empty afterward; `go test -tags=integration
./internal/kyc/... -run TestGetVerificationByProviderReference_
CarriesExplicitTenantIDPredicate` green again, both before applying the
mutation and after reverting it). This demonstrates the test is killed by
removing the very predicate it exists to prove, independent of whatever
RLS would or would not have enforced behaviourally for the same mutation,
and independent of which placeholder numbers the query happens to use.

## Mutation-kill record: K5/K6/K7 guards

Every `TestKYCWebhook_TamperMatrix_Rejected` subtest, the K6 enumeration-
oracle probes, and K7's statement-capture assertion were exercised as
originally written (all pass); their "must go red if the guard is
removed" property is the same one already established for the underlying
`webhookauth.Scheme.Verify`/`CheckPreamble` code by
`internal/webhookauth/webhookauth_test.go`'s own
`TestScheme_Verify_TamperRejected`/`TestScheme_CheckPreamble` (shared
code, one proof) — this gap-closure adds the KYC-specific field/case
coverage on top (per-field tamper, provider-id substitution, legacy
trailing-newline, cross-verification substitution), all newly
demonstrated failing-then-passing during authoring (each new subtest was
run individually against the unmodified fixed code and confirmed to
reject; the provider-id-substitution and after-staff-decision tests were
each iterated once against an initially-wrong test harness that produced
the WRONG failure reason, `credential_unavailable` and a foreign-key
error respectively, before being corrected — see the two `go test -v`
transcripts in this task's completion report).

## Stage 10.2 final-review fix round A (KYC), 2026-09-26

Closes `01-webhook-trust-design.md` §K rulings K1 (KYC half), K2, K3, K4,
K5, K9 (`kyc.ErrUnknownProvider` only), K11 (KYC/wiring parts), K16.

### K1 mutation-kill record (legacy `signature`-field guard)

`internal/kyc/mock_provider.go`'s post-verification rejection of a legacy
top-level `"signature"` field (`HandleCallback`, the
`if _, present := generic["signature"]; present` guard) was temporarily
removed:

```
=== RUN   TestKYCWebhook_TamperMatrix_Rejected/legacy_signature_field_in_body
    orchestrator_webhook_integration_test.go:367: legacy signature field in body: expected a CallbackAuthError, got <nil>
=== NAME  TestKYCWebhook_TamperMatrix_Rejected
    orchestrator_webhook_integration_test.go:375: expected the verification untouched by every tampered attempt, got approved
--- FAIL: TestKYCWebhook_TamperMatrix_Rejected (0.03s)
    --- FAIL: TestKYCWebhook_TamperMatrix_Rejected/legacy_signature_field_in_body (0.00s)
```

The guard was restored immediately after capturing this failure (`git diff
internal/kyc/mock_provider.go` empty afterward). This is the fix for M1:
the subtest's body is now signed with the REAL derived key
(`provider.deriveKey`/`webhookauth.KYCScheme().Sign`), so it passes
`Scheme.Verify` and genuinely reaches this guard - before the fix, the
subtest mutated the body AFTER using the original signature, so it was
rejected by `Scheme.Verify` itself and never exercised the guard at all
(the mutation above would previously have left the subtest green).

### K4 mutation-kill record (`kyc_webhook_noop` allow-listed line)

The handler's `if !applied { logger.Info("kyc_webhook_noop", ...) }` branch
in `internal/httpserver/kyc_admin_handlers.go` was temporarily replaced
with `_ = applied` (no logging call at all):

```
=== RUN   TestKYCWebhook_NoopLogging_ReplayLogsAllowListedLine_AppliedDoesNot
    kyc_webhook_noop_logging_integration_test.go:111: expected exactly 1 kyc_webhook_noop line for the replay, got 0: []
--- FAIL: TestKYCWebhook_NoopLogging_ReplayLogsAllowListedLine_AppliedDoesNot (0.22s)
```

Restored immediately after capturing this failure (`git diff
internal/httpserver/kyc_admin_handlers.go` empty afterward). Closes M4:
`kyc.Orchestrator.ReceiveCallback` now returns `(Verification, applied
bool, error)`; `applyCallbackOutcome` reports `applied` for every branch
(a forward transition or a non-terminal `outcome=error` audit write is
`true`; every no-op, including the new K5 terminal-`error` case, is
`false`). The handler's `_ = result` is gone - the verification value is
discarded (never echoed, per design §G) but `applied` drives the new log
line. `TestKYCWebhook_NoopLogging_ReplayLogsAllowListedLine_AppliedDoesNot`
(`internal/httpserver/kyc_webhook_noop_logging_integration_test.go`)
asserts BOTH halves: the FIRST, genuinely-applied delivery logs no
`kyc_webhook_noop` line at all, and a REPLAY of that same delivery logs
exactly one, carrying only `request_id`/`tenant_id`/`provider_id`.

### K5 mutation-kill record (no audit row for `error` against a terminal verification)

`applyCallbackOutcome`'s new `if isTerminal(v.Status) { return v, false,
nil }` branch (checked before writing the failure audit row for
`outcome=error`) was temporarily removed:

```
=== RUN   TestKYCWebhook_OutcomeErrorAgainstTerminal_NoAuditRow
    orchestrator_webhook_integration_test.go:790: expected STILL exactly 1 audit row (no new one for outcome=error against a terminal verification), got 2
--- FAIL: TestKYCWebhook_OutcomeErrorAgainstTerminal_NoAuditRow (0.04s)
```

Restored immediately after capturing this failure (`git diff
internal/kyc/provider.go` empty afterward, confirmed by rerunning the full
`TestKYCWebhook_OutcomeError*` group green). Closes L2/F-5:
`TestKYCWebhook_OutcomeErrorAgainstTerminal_NoAuditRow` proves the fix
(reach terminal via a normal approval, then a later `outcome=error`
callback writes no second audit row and never disturbs the terminal
status); `TestKYCWebhook_OutcomeErrorNonTerminal_RedeliveredWritesOneAuditRowPerDelivery`
proves the narrower, still-disclosed exception survives unchanged (a
NON-terminal verification still gets one failure audit row per delivery,
including an exact replay - both in `internal/kyc/orchestrator_webhook_
integration_test.go`).

### K3: KYC conformance suite

`internal/kyc/conformance_test.go` (new) adds
`RunProviderConformanceSuite`/`TestMockKYCProvider_ConformsToKYCProvider`,
mirroring `internal/casino/conformance_test.go`'s tenant-binding
conformance case exactly, including its MAC-level sub-case (a credential
re-bound to claim tenant B's identity while still carrying tenant A's own
derived secret, forcing the assertion past `webhookauth.Scheme.Verify`'s
early `TenantID` metadata check and into the actual HMAC comparison). Per
K3/F-9, the case `t.Fatalf`s (never `t.Skip`s) for any provider that is not
`*MockKYCProvider` - closes ADR 0022 §3's "mandatory, skip-to-fail" claim,
which previously had no KYC suite to back it at all.

### K9: dead code removed

`kyc.ErrUnknownProvider` (unreferenced anywhere in the codebase) and its
doc comment were deleted from `internal/kyc/provider.go`.

### K11: KYC mock-wiring duplication/staleness removed

- `kyc.NewMockWebhookCredentials` now returns `webhookauth.MockResolver`
  directly (design §B2) instead of a KYC-local `MockWebhookCredentials`
  wrapper type that duplicated `MockResolver`'s own `Resolve` logic for no
  reason. No caller needed to change (all of them already only use the
  return value as a `webhookauth.Resolver`).
- `cmd/platform-api/main.go` now constructs `mockKYCProvider` ONLY when
  `wiring.KYCWebhookEnabled` (ADR 0085 amendment: "absent" in
  production/test-support-off, not merely constructed-but-unwired) -
  `kycOrchestrator`/`kycWebhookResolver` already guarded against a nil
  provider, so this is a real behavioural tightening, not just a comment
  fix.
- Removed the stale `mockWiring` doc comment ("Later Stage 10.2 steps
  extend this struct...") and the `kycOrchestrator` "for other purposes"
  justification, both no longer true (`cmd/platform-api/wiring.go`).

### K16: hard-coded fixture key replaced

`internal/kyc/orchestrator_webhook_integration_test.go`'s
`TestKYCWebhook_EqualSecretResolver_CrossTenantRejected` (K4/§H) now
derives its equal-secret fixture via `webhookauth.NewMockMaster()` (a
fresh per-process `crypto/rand` value) instead of the literal
`[]byte("equal-secret-shared-by-every-tenant-32bytes!!")`.

### Test run for this fix round (2026-09-26, branch `claude/focused-wright-jw88w9`, HEAD `eb91060` at task start)

```
gofmt -l internal/kyc internal/httpserver cmd/platform-api                          (empty)
go vet ./...                                                                        (clean)
go vet -tags=integration ./...                                                      (clean)
golangci-lint run ./...                                                             0 issues
go test ./...                                                                       ok (all packages)
go test -tags=integration ./internal/kyc/...        ok  1.481s
go test -tags=integration ./internal/httpserver/...  ok  63.589s
go test -tags=integration ./cmd/platform-api/...     ok  0.009s
```

## Full test run (2026-09-26, HEAD `c520e76`)

```
go test -tags=integration ./internal/kyc/...        ok  1.158s
go test -tags=integration ./internal/httpserver/...  ok  48.484s
go test -tags=integration ./cmd/platform-api/...     ok  0.010s
gofmt -l internal/kyc internal/httpserver internal/testsupport internal/webhookauth cmd/platform-api   (empty)
go vet -tags=integration ./internal/kyc/... ./internal/httpserver/... ./cmd/platform-api/... ./internal/testsupport/... ./internal/webhookauth/...   (clean)
```

## Casino (CAS-WH-TENANT-1)

> Recorded by the `casino` specialist, 2026-09-26, against HEAD `11b2ad1`
> (branch `claude/focused-wright-jw88w9`), closing the remaining
> test-coverage gaps for CAS-WH-TENANT-1 (Stage 10.2) per the binding QA
> plan (`03-review-qa-test-plan.md`), design §H/§C, and the code-review
> (`11-review-code.md`, findings M1/L4) and security-review
> (`09-review-security-final.md`, findings SC-1/SC-2/SC-3) items folded in
> mid-task per the orchestrator's course corrections.

### C1–C14 → test → file

| Case | Expected | Test | File |
|---|---|---|---|
| C1 | E4 re-run: rollback of a never-seen tx, signed for A, delivered to B → 401, no tombstone in B, full no-effect checklist | `TestCasinoWebhook_CrossTenantRollback_Rejected` | `internal/httpserver/casino_prefix_e4_defect_test.go` |
| C2 | A→A bet/win/rollback → 200 | `TestCasinoWebhook_ProviderRoundOwnershipConflict_Returns409WithoutLeakingIdentity` (bet leg, `respA` 200, via the real webhook route); `TestReceiveCallback_RollbackReversesFlow5BetExactly` and neighbouring package-level bet/win/rollback happy-path tests; `TestCasinoPlay_WagerWinRollbackHappyPath` (same `ReceiveCallback` code path via the play-simulation route) | `internal/httpserver/casino_flow_integration_test.go`; `internal/casino/orchestrator_integration_test.go`; `internal/httpserver/casino_play_flow_integration_test.go` |
| C3 | A-signed bet → B (same-id session genuinely exists under B) → 401 | `TestCasinoWebhook_CrossTenantBet_Rejected` | `internal/httpserver/casino_webhook_tenant_binding_test.go` |
| C4 | Equal-secret resolver, A-signed → B → 401 (tenant id is in the MAC, not merely which secret was looked up) | `TestCasinoWebhook_EqualSecretResolver_StillTenantBound` (rewritten per code-review L4: now wires a genuine `equalSecretCasinoResolver`, not a copy of C3; includes a same-tenant sanity-check `resp2` 200) | `internal/httpserver/casino_webhook_tenant_binding_test.go` |
| C5 | Tamper: amount / signature header / missing key id / unknown key id / legacy `signature` field → 401 each | `TestCasinoWebhook_TamperMatrix_Rejected` (HTTP, uniform 401 per code-review M1 fix: the legacy case now signs the legacy-shaped body with the real derived key via `mock.SignRawBody`, so it genuinely reaches `hasLegacySignatureField`); `TestCasinoWebhook_TamperMatrix_ExactReason` (package-level companion, asserts the EXACT `webhookauth.Reason` per case, which the uniform-401 HTTP layer cannot itself observe without contradicting C6) | `internal/httpserver/casino_webhook_tenant_binding_test.go`; `internal/casino/tamper_matrix_reason_integration_test.go` |
| C6 | Unknown slug / suspended / unregistered provider / bad signature / non-JSON / oversized / unconfigured-tenant-gets-503-not-401 / bad provider-id charset → byte-identical 401 (except the 503 case, which is deliberately distinct) | `TestCasinoWebhook_EnumerationOracle_IndistinguishableResponses` | `internal/httpserver/casino_webhook_tenant_binding_test.go` |
| C7 | Bad signature / missing credential / foreign credential / nil resolver under statement capture → **zero** tenant-scoped statements; verified callback → `LoadCapability` is the FIRST statement | `TestCasinoWebhook_BadSignature_NoStatementBeforeVerification`, `TestCasinoWebhook_MissingCredential_NoStatementBeforeVerification`, `TestCasinoWebhook_ForeignCredential_NoStatementBeforeVerification`, `TestCasinoWebhook_NilResolver_NoStatementBeforeVerification`, `TestCasinoWebhook_VerifiedCallback_LoadCapabilityIsFirstStatement` | `internal/casino/receive_callback_statement_capture_integration_test.go` (harness: `internal/casino/recording_tx_integration_test.go`) |
| C8 | Same `provider_tx_id` replay idempotent; F-7 mismatch → 409; double rollback → 409; rollback then late original → rejected by tombstone; 8 concurrent duplicates → one posting | `TestReceiveCallback_RedeliveredBetWinRollbackAreIdempotent`, `TestReceiveCallback_SecondDistinctRollbackOfSameBetRejected`, `TestReceiveCallback_RollbackOfNeverSeenOriginalWritesTombstone`, `TestReceiveCallback_ConcurrentDistinctRollbacksOnlyOneSucceeds`, `TestReceiveCallback_ConcurrentDuplicateBetsOnlyOneEffect`; `TestF7Casino_LegitimateRedeliveriesStillResolveToOriginal`, `TestF7Casino_ConcurrentTombstoneRedeliveryIsIdempotent`, `TestF7Casino_BetReplayWithDifferentPayloadRejected`, `TestF7Casino_WinReplayWithDifferentAmountRejected`, `TestF7Casino_RollbackRefReusedForDifferentOriginalRejected`; `TestConcurrentStress_ManyDuplicateBetDeliveries` | `internal/casino/orchestrator_integration_test.go`; `internal/casino/replay_f7_integration_test.go`; `internal/casino/adversarial_lock_stress_test.go` |
| C9 | Disabled (or never-configured) capability + valid signature → 503, no posting | `TestCasinoWebhook_DisabledCapability_ValidSignatureGets503` | `internal/httpserver/casino_webhook_tenant_binding_test.go` |
| C10 | Nil resolver (production wiring) → all 401; play routes absent | `TestMockProviderWiring_Matrix` (+ the wiring-divergence `*_FollowsWiring` tests) for the wiring gate itself; `TestCasinoWebhook_NilResolver_NoStatementBeforeVerification` for the webhook-route "all 401" half (`ReasonNoResolver`, zero statements) | `cmd/platform-api/wiring_test.go`; `internal/casino/receive_callback_statement_capture_integration_test.go` |
| C11 | Play routes with TS-on → green; broken resolver → 503 misconfigured, no signature material in the response | `TestCasinoPlay_WagerWinRollbackHappyPath` (TS-on green); `TestCasinoPlay_BrokenResolver_Returns503Misconfigured` | `internal/httpserver/casino_play_flow_integration_test.go`; `internal/httpserver/casino_webhook_tenant_binding_test.go` |
| C12 | Tenant-binding conformance case, mandatory for the first real adapter (skip-to-fail) | `"a credential resolved for one tenant is rejected for another (conformance)"` subtest, now with an added sub-case (code-review item 3, 2026-09-26) that re-binds a credential to claim tenant B's identity while still carrying tenant A's own derived secret — forcing the assertion past `webhookauth.Scheme.Verify`'s early `cred.TenantID != in.TenantID` metadata check and into the actual HMAC comparison itself | `internal/casino/conformance_test.go` |
| C13 | Existing status-code assertions (400/404 → 401) re-verified individually, not by blind replace | `TestCasinoWebhook_UnknownAndSuspendedTenantIdenticalNotFound`, `TestCasinoWebhook_UnsignedPayloadRejected`, `TestCasinoWebhook_ProviderRoundOwnershipConflict_Returns409WithoutLeakingIdentity` — see the call-site log below | `internal/httpserver/casino_flow_integration_test.go` |
| C14 | OpenAPI contract passes | `TestOpenAPI_CasinoWebhook_ContractMatchesHandler` | `internal/httpserver/openapi_casinowebhook_contract_test.go` |

**Beyond the C1–C14 list (SC-3, security review):**

| Item | Expected | Test | File |
|---|---|---|---|
| Casino auth-failure logging (K12-equivalent) | Allow-listed log line per reason (`tenant_unknown`, `tenant_inactive`, `provider_unregistered`, `no_resolver`, `signature_invalid`, `provider_invalid`, `body_too_large`, `signature_missing`); never body/header/signature/slug/err | `TestCasinoWebhook_AuthFailureLogging_AllowListOnly` (8 subtests) | `internal/httpserver/casino_webhook_auth_failure_logging_integration_test.go` |
| CAS-CAP-ROLLBACK-1 (J16 follow-up) | **Characterisation, not a requirement:** a verified rollback of an already-posted bet, against a since-disabled capability, returns 503 today; the original bet's ledger effect is left standing, un-reversed | `TestCasinoWebhook_CAS_CAP_ROLLBACK_1_DisabledCapabilityBlocksRollbackOfAlreadyPostedBet` | `internal/httpserver/casino_webhook_tenant_binding_test.go` |

### Six-point no-effect checklist (casino)

`internal/testsupport/noeffect` extended this task (SC-2, security review)
with `CasinoSnapshot`/`CaptureCasino`/`AssertNoCasinoEffect`, covering all
six points over casino's own tables (never touching the pre-existing KYC
`Snapshot`/`Capture`/`AssertNoEffect` types or their callers, which stay
green unchanged):

1. no new/changed `ledger_transactions` rows;
2. no `ledger_entries` changes;
3. no tombstones — `ledger_transactions` rows whose `idempotency_key` has
   the `tombstone:%` shape, counted separately per tenant;
4. no `audit_log` rows;
5. `SUM(debits) == SUM(credits)` unchanged;
6. projections unchanged — `wallet_balance_projection` debit/credit totals
   AND (added per SC-2's "casino_* rows" point) `casino_provider_rounds`
   row count and a `casino_launch_sessions` (id, status) fingerprint, so a
   same-row status transition (e.g. `active` → `consumed`) is caught even
   though it is not a row-count change.

Wired into (both tenants where a cross-tenant scenario exists, one tenant
otherwise, always a fresh `pool.WithTenant` transaction, never the
transaction the rejected callback ran in):

- `TestCasinoWebhook_CrossTenantRollback_Rejected` (C1) — both tenants;
- `TestCasinoWebhook_CrossTenantBet_Rejected` (C3) — both tenants;
- `TestCasinoWebhook_EqualSecretResolver_StillTenantBound` (C4) — both
  tenants;
- `TestCasinoWebhook_TamperMatrix_Rejected` (C5) — one tenant, re-captured
  per subtest;
- `TestCasinoWebhook_TamperMatrix_ExactReason` (C5 package-level) — one
  tenant;
- `TestCasinoWebhook_DisabledCapability_ValidSignatureGets503` (C9) — one
  tenant.

C6/C8/C10/C11/C12/C13/C14 are covered by their own, already-targeted
assertions (byte-identity, ledger-row idempotency counts, wiring-matrix
booleans, response-body scanning, HMAC-failure assertions, OpenAPI schema
matching respectively) — the design's own six-point list names C1,
C3–C7, C9, C10 as the checklist's scope; C7's statement-capture proof is
strictly stronger than the checklist for its own "no effect on the
pre-verification path" claim, per the same reasoning the KYC section above
gives for K1/K6/K7/K9/K11.

### Call-site re-verification log (item 2 of this task)

Per the binding QA plan's re-verification rule: about a 10% sample of the
`casino.NewOrchestrator` and `.CallbackPayload` call sites, plus 100% of
the sites whose expected status code changed, each checked individually
(right tenant, right resolver, assertion not weakened) and logged here.

**Current counts** (re-grepped 2026-09-26, HEAD `11b2ad1` plus this
session's own additions): `NewOrchestrator` — 91 (90 test call sites in
`internal/casino` + the 1 definition in `orchestrator.go`), unchanged from
J14's figure; `.CallbackPayload(` call sites bound to a `CallbackEventBet/
Win/Rollback` value (i.e. genuinely `casino.CallbackPayload`, filtering out
`payments.CallbackPayload`/`kyc.CallbackPayload` call sites that also match
the bare `.CallbackPayload(` grep) — 126 on a single-line match (more,
counting the several multi-line call sites the grep does not itself
resolve), up from J14's 119 figure as this and the prior sessions added new
C1–C9 tests.

**`NewOrchestrator` sample (9 of 90, every 10th site in file order):**

| # | Site | Check | Result |
|---|---|---|---|
| 1 | `internal/casino/risk_enforcement_integration_test.go:119` | tenant/resolver | `NewMockWebhookCredentials(provider)` — genuine per-tenant mock resolver, not weakened |
| 2 | `internal/casino/jurisdiction_blocklist_integration_test.go:146` | tenant/resolver | same pattern, correct |
| 3 | `internal/casino/bonus_settlement_integration_test.go:478` | tenant/resolver | same pattern, correct |
| 4 | `internal/casino/wagering_contribution_integration_test.go:165` | tenant/resolver | same pattern, correct |
| 5 | `internal/casino/failure_mode_matrix_integration_test.go:537` | tenant/resolver | same pattern, correct |
| 6 | `internal/casino/rg_enforcement_integration_test.go:133` | tenant/resolver | same pattern, correct |
| 7 | `internal/casino/orchestrator_integration_test.go:523` | resolver `nil` | test only calls `LaunchGame` (`TestLaunchGame_InvalidGameNotFound`), never `ReceiveCallback` — a `nil` resolver here is correct, not a weakened callback-auth test |
| 8 | `internal/casino/orchestrator_integration_test.go:831` | tenant/resolver | genuine mock resolver, correct |
| 9 | `internal/casino/orchestrator_integration_test.go:1340` | tenant/resolver | genuine mock resolver, correct |

A tenth site, `internal/casino/jurisdiction_concurrency_integration_test.go:51`,
was inspected alongside #7 specifically because it is the ONLY OTHER
`nil`-resolver site in the whole 90: `TestJurisdictionResolve_
ConcurrentCasinoLaunchAndBonusActivate_NoDeadlockIndependentDenial` also
only calls `LaunchGame`, never `ReceiveCallback` — same conclusion, correct.

**`.CallbackPayload` sample (13 of ~126, every 10th site):** all 13 sampled
sites pass a tenant identifier as the FIRST argument that is genuinely the
fixture's own tenant (`f.tenantID`, `e.f.tenantID`, `tenantA.ID`, `tenant.ID`
— never a foreign, hard-coded, or cross-tenant value), and no sampled site
weakens an assertion — the migration only ever prepends the tenant
parameter, never changes argument order or count elsewhere. Full list
inspected: `internal/casino/risk_enforcement_integration_test.go:126`,
`lockorder_integration_test.go:312`, `stage9_concurrency_integration_test.go:214`,
`bonus_settlement_integration_test.go:743,1141`,
`failure_mode_matrix_integration_test.go:350,810`,
`replay_f7_integration_test.go:67`,
`orchestrator_integration_test.go:738,944,1307`,
`adversarial_jurisdiction_isolation_test.go:119`,
`internal/httpserver/casino_webhook_tenant_binding_test.go:90`.

**100% of the status-code-changed sites (C13), individually re-run and
diffed against `3398c88^` (pre-migration):**

| Site (current line) | Old assertion | New assertion | Individually re-run | Verdict |
|---|---|---|---|---|
| `casino_flow_integration_test.go` `TestCasinoWebhook_UnknownAndSuspendedTenantIdenticalNotFound`, unknown-slug case | `StatusCode != http.StatusNotFound` (404) | `StatusCode != http.StatusUnauthorized` (401) | `go test -run '^TestCasinoWebhook_UnknownAndSuspendedTenantIdenticalNotFound$' -v` → PASS | Correct: matches design §C6's uniform 401 |
| same test, suspended-tenant case | 404 | 401 | same run | Correct, same reasoning |
| `TestCasinoWebhook_UnsignedPayloadRejected` | `StatusCode < 400 \|\| StatusCode >= 500` (loose 4xx) | `StatusCode != http.StatusUnauthorized` (strict 401) | `go test -run '^TestCasinoWebhook_UnsignedPayloadRejected$' -v` → PASS | STRENGTHENED, not weakened — a loose 4xx bound became an exact status check |
| `TestCasinoWebhook_ProviderRoundOwnershipConflict_Returns409WithoutLeakingIdentity` (design's own 387–395 range) | `rawPostJSON` + unsigned/pre-tenant `CallbackPayload(...)` (4-arg, no tenant param); `respA.StatusCode != http.StatusOK` unchanged | `rawPostCasinoCallback` + `mock.CallbackPayload(tenant.ID, ...)` (tenant param added); `respA`/`respB` status assertions (200 / 409) themselves UNCHANGED | `go test -run '^TestCasinoWebhook_ProviderRoundOwnershipConflict_Returns409WithoutLeakingIdentity$' -v` → PASS | Correct: this site's own status-code assertions did not change value, only the call shape (mechanical, verified against `3398c88^`); included in the 100% set per design's line-range grouping since it sits inside the same range as the three genuine status-code changes above |

### Mutation-kill records (casino)

All four mutations below were applied by direct `Edit`, confirmed red by
running the named test(s), then reverted; `git diff --stat` on the mutated
file was confirmed empty immediately after each revert.

**C7 (I1 statement-capture, this task).** `internal/casino/orchestrator.go`'s
`ReceiveCallback`: moved the `LoadCapability` call from its normal
post-verification position ((d) in the function's own doc comment) to run
BEFORE `provider.HandleCallback` ((c)). Result:

```
=== RUN   TestCasinoWebhook_BadSignature_NoStatementBeforeVerification
    receive_callback_statement_capture_integration_test.go:115: strict I1: expected ZERO statements before verification succeeds, got ["SELECT id, tenant_id, brand_id, provider_id,\n\t\t\tsupports_catalogue, supports_launch, supports_balance, supports_bet, supports_win, supports_rollback,\n\t\t\tsupported_assets, supported_game_types, callback_capabilities, priority, status\n\t\t FROM casino_provider_capabilities\n\t\t WHERE tenant_id = $1 AND provider_id = $2 AND (brand_id = $3 OR brand_id IS NULL)\n\t\t ORDER BY (brand_id IS NULL) ASC\n\t\t LIMIT 1"]
--- FAIL: TestCasinoWebhook_BadSignature_NoStatementBeforeVerification (0.02s)
=== RUN   TestCasinoWebhook_MissingCredential_NoStatementBeforeVerification
--- PASS: TestCasinoWebhook_MissingCredential_NoStatementBeforeVerification (0.02s)
=== RUN   TestCasinoWebhook_ForeignCredential_NoStatementBeforeVerification
--- PASS: TestCasinoWebhook_ForeignCredential_NoStatementBeforeVerification (0.02s)
=== RUN   TestCasinoWebhook_NilResolver_NoStatementBeforeVerification
--- PASS: TestCasinoWebhook_NilResolver_NoStatementBeforeVerification (0.02s)
FAIL
```

`TestCasinoWebhook_BadSignature_NoStatementBeforeVerification` is the only
one of the four that reaches the moved code (the other three fail earlier,
at step (a)/(b), so the mutation is invisible to them by construction —
expected, and consistent with strict I1 being about the ORDER relative to
(c) specifically). Reverted; `git diff --stat internal/casino/orchestrator.go`
empty afterward; the full C7 suite re-ran green.

**L4 (equal-secret resolver, code-review finding, this task).**
`internal/webhookauth/webhookauth.go`'s `Scheme.SigningInput`: dropped
`tenant_id` from the signing input entirely. Result:

```
=== RUN   TestCasinoWebhook_EqualSecretResolver_StillTenantBound
time=... level=ERROR msg=casino_webhook_missing_session_binding ...
    casino_webhook_tenant_binding_test.go:147: expected 401 under an equal-secret resolver (the tenant id is bound into the MAC), got 400
--- FAIL: TestCasinoWebhook_EqualSecretResolver_StillTenantBound (0.50s)
```

(The rewritten payload now verifies for tenant B under the shared secret,
so it clears verification and is rejected only later, post-verification,
by session binding — 400, not the expected 401.) Reverted; `git diff
--stat internal/webhookauth/webhookauth.go` empty afterward; re-ran green.
This mutation is shared code (payments/KYC/casino all use `SigningInput`);
the KYC and payments suites were re-run green after the revert too.

**M1 (legacy-signature-field guard, code-review finding, this task).**
`internal/casino/mock.go`'s `HandleCallback`: short-circuited
`hasLegacySignatureField(generic)` to always-false. Result:

```
=== RUN   TestCasinoWebhook_TamperMatrix_Rejected/legacy_signature_field_in_body
time=... level=ERROR msg=casino_webhook_missing_session_binding ...
    casino_webhook_tenant_binding_test.go:242: expected 401, got 400
--- FAIL: TestCasinoWebhook_TamperMatrix_Rejected (0.37s)
    --- FAIL: TestCasinoWebhook_TamperMatrix_Rejected/legacy_signature_field_in_body (0.01s)
```

Reverted; `git diff --stat internal/casino/mock.go` empty afterward; re-ran
green (both the HTTP-level `TestCasinoWebhook_TamperMatrix_Rejected` and
the package-level `TestCasinoWebhook_TamperMatrix_ExactReason`, which
asserts this specific case's `Reason == webhookauth.ReasonSignatureInvalid`
on top of the HTTP layer's necessarily-uniform "callback rejected" body).

**SC-3 (allow-listed logging, security-review finding, this task).**
`internal/httpserver/payment_callback_errors.go`'s
`callbackAuthFailureAllowlistFields` (shared by all three domains through
`logWebhookAuthFailure`): appended one non-allow-listed field,
`"debug_extra_field"`. Result: all 8
`TestCasinoWebhook_AuthFailureLogging_AllowListOnly` subtests went red
(`casino_webhook_auth_failed carries a NON-allow-listed field
"debug_extra_field"`) simultaneously. Reverted; `git diff --stat
internal/httpserver/payment_callback_errors.go` empty afterward; re-ran
green, alongside the pre-existing KYC and payments allow-list logging
tests (also green, confirming the shared function's contract is unchanged
for either domain).

### Casino test run (2026-09-26, HEAD `11b2ad1` plus this session's changes)

```
go build -tags=integration ./...                     (clean)
go vet ./...                                          (clean)
go vet -tags=integration ./...                        (clean)
golangci-lint run ./...                               0 issues
go test -tags=integration ./internal/casino/...       ok  8.845s
go test -tags=integration ./internal/httpserver/...   ok  48.173s
go test -tags=integration ./cmd/platform-api/...      ok
go test -tags=integration ./internal/kyc/...          ok   (unaffected regression check)
go test -tags=integration ./internal/payments/...     ok   (unaffected regression check, mutated-then-reverted shared code)
go test -tags=integration ./internal/webhookauth/...  ok
gofmt -l internal/casino internal/httpserver internal/testsupport internal/webhookauth cmd/platform-api   (empty)
```

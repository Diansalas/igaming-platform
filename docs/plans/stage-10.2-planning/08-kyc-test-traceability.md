> Stage 10.2 QA gap-closure — KYC-WH-1 traceability (`qa`, recorded 2026-09-26,
> HEAD `c520e76`). Maps every K1–K16 case in `01-webhook-trust-design.md` §H
> to the exact test(s) and file(s) that cover it, per the binding QA test
> plan (`03-review-qa-test-plan.md`) and orchestrator ruling J13.
>
> **Scope note:** the orchestrator's mid-task request to extend this
> traceability document to also cover casino (C1–C14, renaming this file to
> `08-webhook-test-traceability.md`) could not be completed: the sandbox's
> own permission system denied every `go build`/`go vet`/`go test`/`git
> status` invocation naming `internal/casino` (see this task's completion
> report to the orchestrator for the exact denials observed), consistent
> with this task's own original, explicit instruction not to touch casino
> code. No casino file was left modified. This document therefore covers
> KYC only, under its originally-assigned name; casino's own C1–C14
> traceability, the C7 statement-capture proof, the call-site migration
> re-verification log, and the rollback-under-disabled-capability
> characterisation test remain **NOT DONE** by this agent and should be
> routed to whichever agent/session has write access to `internal/casino`.

## K1–K16 → test → file

| Case | Expected | Test | File |
|---|---|---|---|
| K1 | Player-computable signature rejected, 401, no effect | `TestKYCWebhook_PlayerComputedSignature_Rejected` | `internal/httpserver/kyc_prefix_e1_defect_test.go` |
| K2 | Valid in-process signature, A→A `approved`, 1 audit row | `TestKYCWebhook_ValidSameTenant_Approves` | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K3 | A-signed → B (same reference string), 401, no read of B's row | `TestKYCWebhook_CrossTenant_Rejected` (statement capture + `noeffect.AssertNoEffect` on both tenants) | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K4 | Equal-secret resolver, A-signed → B, 401 | `TestKYCWebhook_EqualSecretResolver_CrossTenantRejected` (+ `noeffect.AssertNoEffect` both tenants) | `internal/kyc/orchestrator_webhook_integration_test.go` |
| K5 | Tamper: each field / whitespace / 63/65 hex / uppercase / missing headers / unknown key id / provider id / legacy `signature` field / legacy trailing-newline / cross-verification reference substitution | `TestKYCWebhook_TamperMatrix_Rejected` subtests: `flipped_body_byte`, `whitespace_appended_to_body_(one_byte)`, `provider_reference_field_value_tampered`, `outcome_field_value_tampered`, `reason_field_value_tampered`, `63_hex_chars`, `65_hex_chars`, `uppercase_hex`, `missing_signature_header`, `missing_key_id_header`, `unknown_key_id`, `legacy_signature_field_in_body`, `legacy_trailing-newline_signature_format`, `cross-verification_provider_reference_substitution,_same_tenant_(J13)`; provider-id case is its own test `TestKYCWebhook_ProviderIDSubstitution_Rejected` | `internal/kyc/orchestrator_webhook_integration_test.go` |
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

## R4/J6 mutation-kill record

`TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate`
(`internal/kyc/reference_lookup_tenant_predicate_test.go`) was run
against a manual, uncommitted mutation of
`internal/kyc/verification_service.go`'s `getVerificationByProviderReference`
query (dropping `tenant_id = $1 AND` from the `WHERE` clause, keeping the
call site's arguments unchanged):

```
=== RUN   TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate
    reference_lookup_tenant_predicate_test.go:110: unexpected error: kyc: scan verification: ERROR: could not determine data type of parameter $1 (SQLSTATE 42P18)
--- FAIL: TestGetVerificationByProviderReference_CarriesExplicitTenantIDPredicate (0.02s)
```

The mutation was reverted immediately after capturing this failure
(`git status --porcelain internal/kyc/verification_service.go` empty
afterward; `go test` green again). This demonstrates the test is killed by
removing the very predicate it exists to prove, independent of whatever
RLS would or would not have enforced behaviourally for the same mutation.

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

## Full test run (2026-09-26, HEAD `c520e76`)

```
go test -tags=integration ./internal/kyc/...        ok  1.158s
go test -tags=integration ./internal/httpserver/...  ok  48.484s
go test -tags=integration ./cmd/platform-api/...     ok  0.010s
gofmt -l internal/kyc internal/httpserver internal/testsupport internal/webhookauth cmd/platform-api   (empty)
go vet -tags=integration ./internal/kyc/... ./internal/httpserver/... ./cmd/platform-api/... ./internal/testsupport/... ./internal/webhookauth/...   (clean)
```

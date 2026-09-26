> Stage 10.1 PAY-WH-TENANT-1 — specialist working paper (verbatim, 2026-09-26). The Orchestrator rulings in `11-pay-wh-tenant-1-design.md` §"Review record and Orchestrator rulings" govern.

# PAY-WH-TENANT-1 — binding QA test plan (design review, no code yet)

Source: ADR 0090 item 3; `11-pay-wh-tenant-1-design.md` T1-T15 (verified at HEAD `9fb1aa2`).
Repo check: no OpenAPI lint/schema-validation test exists (`docs/api/openapi/platform-api.yaml`
has no referencing test/CI step) — a contract test must be added, not assumed present.

## 0. Pre-fix evidence requirement (BLOCKING, before any fix is accepted)

- `TestPayWH_S6_CrossTenant_PreFix_AttackSucceeds` (package `internal/payments`, integration,
  build-tagged or a `t.Skip` removed only for the one-time evidence run) — run against
  **current/unfixed** code. Must assert the attack **currently succeeds**: A-signed callback
  posted to B's slug returns 200 (or whatever non-401 the current code gives) and produces a
  ledger/tombstone effect in B. This run's output (pass = attack succeeds) is the required
  evidence artifact attached to the PR before the fix lands. After the fix, this same scenario is
  asserted 401 in T2/T3/T5 — the pre-fix test itself is then deleted or inverted, not left
  green-both-ways.

## 1. Test inventory

All integration tests: package `internal/payments` (orchestrator/adapter level) and
`internal/httpserver` (HTTP handler level), run as the **NOBYPASSRLS** runtime role per the
design's §7 mandate. Unit tests: package `internal/payments`.

| Test name | Pkg | Type | Assertion |
|---|---|---|---|
| `TestWebhook_SameTenant_AcceptedAndCredited` (T1) | httpserver | integration | A-signed→A: 200, exactly one ledger credit, one `deposit_intents` row settled |
| `TestWebhook_CrossTenant_SameRefCollision_Rejected` (T2) | httpserver | integration | A-signed→B (B has intent w/ same `provider_reference`): 401 `{"error":{"code":"unauthorized","message":"callback rejected"}}`; **no financial effect in B** (see §2) |
| `TestWebhook_CrossTenant_ReversalOfUnseenRef_NoTombstone` (T3, S-6 regression) | httpserver | integration | A-signed reversal, ref unseen in B, posted to B: 401; **zero tombstone rows in B**, zero ledger rows |
| `TestWebhook_SameTenant_B_AcceptedAndCredited` (T4) | httpserver | integration | B-signed→B: 200, credited once |
| `TestWebhook_SharedSecretAcrossTenants_TenantStillBound` (T5) | payments | integration | test resolver: A and B share one secret; A-signed→B: 401 — proves tenant_id is in signing_input, not just secret possession |
| `TestWebhook_PayloadTamper_Rejected` (T6) | payments | unit+table | for each of {amount, outcome, provider_reference, one whitespace byte in raw body} under original signature: 401, `hmac.Equal` fails |
| `TestWebhook_BindingTamper_Rejected` (T7) | payments | unit+table | 401 for each: sig computed for B posted to A; wrong provider_id in signing_input; unknown key_id; missing `X-Payments-Signature` or `X-Payments-Key-Id`; 63-hex sig; 65-hex sig; body still carries legacy `signature` field |
| `TestWebhook_Replay_SameSuccess_NoSecondCredit` (T8a) | payments | integration | A→A success replayed: 200 both times, exactly one ledger credit total |
| `TestWebhook_Replay_ReversalReplayed_Conflict` (T8b) | payments | integration | replayed reversal: second call 409, no second compensating entry |
| `TestWebhook_ConcurrentDuplicates_ExactlyOnePosting` (T8c) | payments | integration+race | 8 goroutines, identical A→A callback, `go test -race`: exactly one ledger posting, no duplicate-key errors surfaced as 500 |
| `TestWebhook_EnumerationOracle_IndistinguishableResponses` (T9) | httpserver | integration | unknown slug / suspended tenant / unregistered provider / not-configured provider / `polling_only` capability / bad provider_id charset / bad signature / key-material body: all byte-identical 401 status+body (excluding `request_id`) |
| `TestWebhook_DisabledCapability_StillAccepted` (T10) | payments | integration | `status='disabled'` capability row, valid signature: 200 (pins §3.1(b) — disabled is routing-only, not callback-blocking) |
| `TestWebhook_BadSignature_NoWriteBeforeVerification` (T11a) | payments | integration | statement-capturing tx wrapper: bad-signature call executes zero INSERT/UPDATE/`SELECT ... FOR UPDATE` statements |
| `TestWebhook_ProviderAcceptsWebhook_RLSScoped` (T11b) | payments | integration, RLS | under B's `app.tenant_id` GUC, `ProviderAcceptsWebhook` query returns false for a capability row that only exists for A (NOBYPASSRLS role) |
| `TestWebhook_AuthFailureLogging_AllowListOnly` (T12) | payments | unit/integration | for each `ErrCallbackAuthFailed` reason: captured log line contains only allow-listed keys (`request_id, reason, tenant_id, provider_id, key_id, credential_fingerprint, client_ip, body_len`); asserts absence of body/header values/signature/raw slug/`err` text; asserts **zero new `audit_log` rows** for every 401 case |
| `TestWebhook_SimulationRoute_TenantFromJWTOnly` (T13) | httpserver | integration | authenticated player simulates own deposit: 200; signer invoked with `tc.TenantID` (not any request field); response body has no `signature`/raw callback bytes |
| `TestWebhook_NoResolver_FailsClosed` (T14) | payments | unit | adapter registered with no `WebhookCredentialResolver` entry: every callback → 401 `no_resolver`, never falls back to unauthenticated verification |
| `TestMockWebhookCredentials_KeyDerivation` (T15) | payments | unit | `deriveKey(A,provider) != deriveKey(B,provider)`; stable across repeated calls within one process; differs across two `mock.New()` instances (new `masterSecret`); `fmt.Sprintf("%+v", cred)` and `cred.String()`/`LogValue()` never contain the raw secret bytes |

## 2. "No financial effect" assertion (T2, T3, and every 401 case) — exact checks, fresh tx

Run under B's tenant GUC in a **new** transaction/connection (not the request's, which rolled
back) via NOBYPASSRLS role:
1. `SELECT count(*) FROM ledger_transactions WHERE tenant_id = B` — unchanged from pre-call baseline (delta = 0).
2. `SELECT count(*) FROM deposit_intents WHERE tenant_id = B AND provider_reference = <ref>` — unchanged row count and unchanged `status` column.
3. Tombstone check: `SELECT count(*) FROM <tombstone table/key store> WHERE tenant_id = B AND provider = ... AND provider_reference = <ref>` — 0.
4. `SELECT count(*) FROM audit_log WHERE tenant_id = B` — delta = 0 for the request.
5. `SUM(debit) = SUM(credit)` invariant holds for B unchanged (global ledger balance check, not just row count — guards against an unbalanced partial write).
6. Balance/projection for B's affected wallet unchanged (re-read via the same projection recompute path used elsewhere in the suite).
All six must be asserted together; a test that checks only (1) is insufficient — this is exactly the "manual click worked" trap this design closes.

## 3. Mock-provider and simulation-route tests

- `mock.go` unit tests: `sign`/verify roundtrip removed in favor of `deriveKey` + HMAC over the new `signing_input` (NUL-joined tenant_id/provider_id/key_id/body) — assert NUL-byte framing makes body an unambiguous tail (adversarial test: inject 0x00 into a string field pre-hashing, confirm no field-shift collision, closing the `mock.go:207-213` weakness noted in the design).
- `CallbackPayload(tenantID, ...)` call-site regression: assert it never accepts/embeds an externally supplied tenant (only route/JWT-resolved).
- Simulation route (T13 above) plus: `TestSimulationRoute_CannotNameOtherTenant` — attempt to pass a tenant field in the simulation request body; assert it is ignored/rejected and `tc.TenantID` from JWT is what's signed.

## 4. OpenAPI contract test (new — none exists today)

No spectral/schema-validation test currently references `platform-api.yaml`. Required new test:
`TestOpenAPI_PaymentsWebhook_ContractMatchesHandler` (suggest package `internal/httpserver` or a
new `docs/api/openapi` verification script wired into `go test`/CI):
- Parse the YAML, assert the new `POST /v1/webhooks/payments/{tenantSlug}/{providerID}` entry
  exists with `security: []`, required headers `X-Payments-Signature` (pattern `^v1=[0-9a-f]{64}$`)
  and `X-Payments-Key-Id`, and response codes {200,400,401,404,409,500,503} documented.
- A live-request contract check: fire T1/T2/T9 requests/responses through the schema and assert
  conformance (status + body shape), so the doc cannot silently drift from the handler.
- If CI has no generic OpenAPI linter, `qa` flags this as a gap in `docs/testing/testing-strategy.md`
  rather than silently skipping — devops/architect decide whether to adopt one platform-wide.

## 5. Existing tests requiring migration (regressions to fix, not delete)

- ~38 `CallbackPayload` call sites in `internal/payments/*_test.go` (`adversarial_test.go`,
  `mock_test.go`, `orchestrator_integration_test.go`, `stage9_concurrency_integration_test.go`)
  and ~5 in `internal/httpserver/*_test.go` (`financial_flow_integration_test.go`,
  `payment_deposit_simulation_test.go`, `casino_flow_integration_test.go` if it shares helpers,
  `stage6_b2c_sportsbook_acceptance_test.go`, `stage7_b2c_casino_acceptance_test.go`) must be
  updated to the new `InboundCallback{TenantID, ProviderID, Header, Body}` shape and header-based
  signing — not the old body-embedded `signature` field.
- Every existing assertion expecting **404** (unknown slug) or **400** (bad signature) on the
  webhook route must be updated to **401** with the fixed body, per §3.2 of the design. Grep for
  `.Code(404)`/`.Code(400)` or equivalent status assertions in the webhook test files above; each
  must be enumerated and re-verified, not bulk find/replaced blind (some 400s legitimately survive
  verification, e.g. malformed body post-auth — see §3.2 table).
- `adversarial_test.go` in particular must be re-audited: any case it currently drives via the
  old single-secret HMAC needs a per-tenant credential fixture.

## 6. Sign-off gate

QA will not mark PAY-WH-TENANT-1 `IMPLEMENTED` unless: all tests above exist and pass under
`go test -race`, the pre-fix evidence artifact (§0) is attached, the migrated existing-test list
(§5) shows zero remaining 404/400 webhook-status assertions, the OpenAPI contract test (§4) passes
or the gap is explicitly recorded as a decision, and `security` has signed the post-implementation
diff review per the design's header requirement.

# Stage 10.2 — Webhook trust design: KYC-WH-1 + CAS-WH-TENANT-1 (DESIGN ONLY)

Author: `security`. Verified at HEAD `d76bdd3`. Binding scope: ADR 0091. Reference contract: ADR 0022
§3 amendment points 1–7 (Stage 10.1). No code changed here. Reviewers before implementation:
`architect` (+ database/RLS), `identity-compliance` (KYC), `casino` + `ledger-finance` (casino),
`backend`, `qa`. A post-implementation `security` diff review is mandatory before IMPLEMENTED.

## 0. Current state (verified)

**KYC-WH-1 (High)** — confirms Stage 10.1 paper 12.
- **Committed secret.** Compile-time constant at `cmd/platform-api/main.go:38-43`, passed to
  `kyc.NewMockKYCProvider` at `:271-273` (value not reproduced; §F).
- **Ungated.** The mock and the webhook route are registered unconditionally (`main.go:271-273`,
  `kyc_routes.go:42`).
- **No tenant binding.** The HMAC covers the body only (`internal/kyc/mock_provider.go:138-142`), and
  the signature trails the body after a newline (`:158-176,199-210`). The tenant comes from the slug
  only (`kyc_admin_handlers.go:424-441`), and `provider.go:153-167` passes the adapter no tenant.
- **Reference exposed.** The player sees `provider_reference` (`kyc_handlers.go:43,49-52`, used at
  `:139,:172`).
- **Unvalidated outcome, audited early.** `outcome` is not validated (`mock_provider.go:171`). The
  `kyc.provider_callback` audit row is written before the terminal check and before outcome
  validation (`provider.go:176-186`), so every replay appends a row.
- **Not monotonic.** A non-terminal status can move backwards, e.g. `pending` after
  `review_required` (`:192-202`).
- **Oracle.** Unknown slug, suspended tenant or unknown provider give 404; a bad signature or
  oversized body gives 400.
- **Leaky success response.** Success is 200 with the full verification, including
  `player_account_id`, sent to the external caller (`:479`). OpenAPI (`platform-api.yaml:2582-2612`)
  documents 204.
- **Exploit** (any deployment, staging included):
  1. The player creates a verification.
  2. The player reads its reference.
  3. The player HMACs `{"provider_reference":…,"outcome":"approved"}` with the public constant.
  4. The player POSTs to their own slug.
  5. The verification becomes `approved`.

**CAS-WH-TENANT-1 (Medium).**
- **Tenant from slug only** (`casino_handlers.go:300-350`).
- **Global key, no tenant in the MAC.** One per-process `crypto/rand` key serves all tenants
  (`internal/casino/mock.go:49-82`). It MACs NUL-joined fields without tenant or provider
  (`:306-313`), with the signature in the body (`:293`).
- **Parse before verify.** `HandleCallback` parses JSON **before** verifying (`:364-381`), a point-7
  violation: a non-JSON body gives a distinguishable error.
- **Oracle.** A bad signature gives 400 (`:351-357`); unknown tenant or provider gives 404.
- **Cross-tenant tombstone.** An A-signed callback verifies at B's slug. A rollback of an original
  unseen in B writes a **tombstone in B** (`orchestrator.go:1285-1304`, key at `:1478`).
- **Mock signing path not production-reachable today.** The mock is registered unconditionally
  (`main.go:164-166`), but its key never leaves the process. The only non-test minting sites are the
  play-simulation handlers (`casino_play_handlers.go:337,430,570`). They are gated by
  `CasinoPlaySimulationEnabled = cfg.TestSupportRoutesEnabled()` (`main.go:237`,
  `casino_routes.go:40`) and return results, never payloads.
- **The weakness is structural.** Any captured callback is valid for every tenant, on the real
  provider-facing route.
- **Undocumented route.** The casino webhook has no OpenAPI entry.

## A. Shared verification: EXTRACT `internal/webhookauth`

**Decision: extract, do not reuse in place.**
- KYC and casino importing `internal/payments` would invert the domain dependency direction.
- ADR 0091 forbids a second, subtly different implementation.

The package holds only the existing contract code. It has no generic orchestrator, registry or
vendor plug-in model.

**`internal/webhookauth/webhookauth.go`** (code moved, not rewritten):

| Symbol | Moved from |
|---|---|
| `Credential` + `String/GoString/LogValue` redaction; `Fingerprint=hex(sha256)[:16]` | `payments/types.go:317-353` |
| `Resolver` interface, `Resolve(ctx, tenantID, providerID, keyID)` | `types.go:355-365` |
| `Inbound{TenantID, ProviderID, Header, Body}` | `types.go:367-381` |
| `Reason` + its 11 constants; `AuthError{Reason,KeyID,CredentialFingerprint}`; `ErrAuthFailed`, `ErrCredentialUnavailable`, `ErrSignatureInvalid` | `types.go:119,199-263` |
| `ProviderIDPattern`, `ValidProviderID` | `webhook_auth.go:26-35,74-77` |
| `Scheme{Prefix, SignatureHeader, KeyIDHeader}`; `(Scheme) ParseHeaders` | new; `webhook_auth.go:79-106` |
| `(Scheme) SigningInput/Sign`: `Prefix‖0x00‖tenant_id‖0x00‖provider_id‖0x00‖key_id‖0x00‖raw body` | `payments/mock.go:312-331` |
| `(Scheme) Verify(cred, in) error`: header key id must equal `cred.KeyID`; rebuilds the input from `in.TenantID/ProviderID`; `hmac.Equal` over raw bytes; returns only `ErrSignatureInvalid` | `payments/mock.go:508-513` |
| `NewMockMaster()` (32 B `crypto/rand`, panics, never falls back); `DeriveMockKey(master,label,tenant,provider)`; `MockResolver{Master,Label,ProviderID}` resolves only `mock-v1` for its own provider. All **MOCK**. | `payments/mock.go:61-138,163-180` |

**Per-domain schemes.** Distinct prefixes give domain separation: a payments signature never
verifies as KYC or casino, even under an equal key. The signature header is `v1=<64 lowercase
hex>`; the key id matches `^[a-z0-9-]{1,32}$`.

| Domain | Prefix | Headers | Mock label |
|---|---|---|---|
| payments (unchanged) | `igaming.payments.webhook.v1` | `X-Payments-Signature`, `X-Payments-Key-Id` | `igaming/payments-mock-webhook/v1` |
| KYC | `igaming.kyc.webhook.v1` | `X-KYC-Signature`, `X-KYC-Key-Id` | `igaming/kyc-mock-webhook/v1` |
| casino | `igaming.casino.webhook.v1` | `X-Casino-Signature`, `X-Casino-Key-Id` | `igaming/casino-mock-webhook/v1` |

**Domain-specific, not shared:**
- the step order;
- the payments-only `ProviderAcceptsWebhook`;
- post-verification parsing;
- sentinels and HTTP mapping.

**Shared HTTP preamble.** New `internal/httpserver/webhook_preamble.go` extracts the payments handler
steps 1–5 (`deposit_handlers.go:269-338`):
1. provider charset check;
2. body limit, folded into 401 before any tenant work;
3. `Scheme.ParseHeaders`;
4. `GetTenantBySlug`;
5. `status=='active'`.

- The signature is `webhookPreamble(w, r, deps, scheme, maxBody, logEvent) (tenant, providerID,
  body, ok)`.
- `logWebhookAuthFailure(logger, event, …)` generalises `payment_callback_errors.go:108-138`, with
  the **same allow-list**: `request_id`, `reason`, `tenant_id` (if resolved), `provider_id`/`key_id`
  (if charset-valid), `credential_fingerprint` (for `signature_invalid` only), `client_ip`,
  `body_len`. Never the body, header values, signature, raw slug or err text.
- Events: `payment_webhook_auth_failed` (unchanged), `kyc_webhook_auth_failed`,
  `casino_webhook_auth_failed`.

**Payments migration, behaviour-preserving** (its own first commit; the payments tests stay green
with no edits beyond imports):
- **Type aliases:** `WebhookCredential`, `WebhookCredentialResolver`, `InboundCallback`,
  `CallbackAuthReason`, `CallbackAuthError` become `= webhookauth.X`; `ReasonX = webhookauth.ReasonX`.
- **Sentinel variables:**
  - `ErrCallbackAuthFailed = webhookauth.ErrAuthFailed`
  - `ErrWebhookCredentialUnavailable = webhookauth.ErrCredentialUnavailable`
  - `ErrCallbackSignatureInvalid = webhookauth.ErrSignatureInvalid`

  `errors.Is/As` is unchanged. No test asserts error text (checked by grep).
- **Scheme values.** `SigningInputPrefix`, `HeaderSignature` and `HeaderKeyID` keep their values in
  a `paymentsScheme`.
- **Mock.** `deriveKey` delegates to `DeriveMockKey(masterSecret, "igaming/payments-mock-webhook/v1",
  …)`, which is byte-identical. `MockWebhookCredentials` becomes a thin wrapper that keeps its API.
- **Stays in payments:**
  - `MultiWebhookCredentialResolver` (MOCK/test wiring, PW-6);
  - the key-material and legacy-field checks (`mock.go:446-605`, ADR 0022 §4.1).
- **Handler.** `newPaymentWebhookHandler` adopts `webhookPreamble` **only if** the T9 byte-identity
  tests and `openapi_paymentswebhook_contract_test.go` stay green unchanged. Otherwise it is left as
  is; the extraction must not force it.

## B. KYC-WH-1

**B1. Remove the committed constant.**
- Delete `main.go:38-43`. `kyc.NewMockKYCProvider()` takes **no argument**: remove the parameter and
  the `webhookSecret` field (`mock_provider.go:32-47`). A caller then structurally cannot inject a
  literal.
- The provider holds `master := webhookauth.NewMockMaster()`.
- Delete `sign`, `MockSignedCallbackBody` and `splitSignedPayload`.
- The test literals disappear with the parameter (`internal/kyc/mock_test.go:59,74,83,96`,
  `kyc_integration_test.go:92`, `kyc_flow_integration_test.go:39`).

**B2. Mock credential (MOCK).**
- `kyc.NewMockWebhookCredentials(p) webhookauth.MockResolver` uses the KYC label, `ProviderID "mock"`
  and `KeyID "mock-v1"`.
- The in-process and test signer is `(*MockKYCProvider) CallbackPayload(tenantID, ref, outcome,
  reason) webhookauth.Inbound`: headers plus a body with no signature field.
- No secret exists in the repo, config, environment, tests or docs; the key is unrecoverable outside
  the process.

**B3. Environment gate (ADR 0085).**
- **Wiring.** A new `cmd/platform-api/wiring.go` `mockProviderWiring(cfg config.Config)` builds the
  KYC mock, together with `kyc.NewOrchestrator(map{"mock": m}, kyc.NewMockWebhookCredentials(m))`,
  **only if `cfg.TestSupportRoutesEnabled()`**. Otherwise `KYCOrchestrator` is nil. Being a function
  makes it unit-testable.
- **Flag.** New `Deps.KYCWebhookEnabled`, whose doc mirrors `server.go:89-105`, is set to
  `cfg.TestSupportRoutesEnabled()`.
- **Route.** `kyc_routes.go:42` registers the webhook only if `deps.KYCWebhookEnabled &&
  deps.KYCOrchestrator != nil`. Absent means the mux returns **404**.
- **Nil resolver fails closed.** An orchestrator built with a nil resolver returns 401 `no_resolver`
  for every callback. That is the default for a future real vendor until a real resolver exists.
- **Consequence (decided by ADR 0091, disclosed).** In production or with test support off:
  - `POST /v1/me/kyc/verifications` returns 503 (`kyc_handlers.go:97-106`);
  - document upload needs a verification (`:204-260`), so player self-service KYC is unavailable;
  - staff KYC routes remain;
  - no real vendor exists (PROVIDER DEPENDENT).

**B4. No simulate route; decision: do not add one.**
- None exists and none is needed. Acceptance reaches `approved` through the existing staff route
  `POST /v1/admin/kyc/verifications/{id}/review`: `RequireTenantScope` + `PermVerificationReview`
  (`kyc_routes.go:32-33`), tenant from the JWT, never player-invocable.
- **Sole-grantee analysis.** `RoleCompliance` is the only grantee of `PermVerificationReview`
  (`auth/permission.go:688-690`); `tenant_admin` gets `PermVerificationRead` only (`:611-613`).
- **If a simulate route is ever needed** (ADR 0028 amendment first), it must:
  - be gated by `TestSupportRoutesEnabled`;
  - require that same permission;
  - take the tenant from `tc.TenantID`;
  - sign in-process;
  - never return the signed bytes.

**B5. Player responses.** Split `toVerificationResponse` (`kyc_handlers.go:39-58`):
- `playerVerificationResponse` has **no** `provider_reference` key. It is used at `:139,:172`.
- The staff shape keeps it (`kyc_admin_handlers.go:175,250`, `toKYCCaseResponse :47-60`). That is
  safe because the reference is no longer a capability.

**B6. Order.** The new `kyc.Orchestrator.ReceiveCallback(ctx, tx, tenantID, providerID, in
webhookauth.Inbound)` replaces `provider.go:153-203`. The handler (`kyc_admin_handlers.go:407-481`)
runs `webhookPreamble` (KYC scheme, 256 KiB `:483`), then `WithTenant(t.ID)`:
- **(a) Adapter registered**, else `provider_unregistered`. The former 404 becomes the uniform 401.
- **(b) Single credential.** A nil resolver gives `no_resolver`; a resolver error gives
  `credential_unavailable`. Then `cred.TenantID==tenantID && cred.ProviderID==providerID`, else
  `credential_unavailable` (I3).
- **(c) Verify.** `KYCProvider.HandleCallback(ctx, in, cred)` (a `provider.go:106` signature change)
  runs `kycScheme.Verify` over the raw bytes **first**. A failure is `ErrSignatureInvalid` →
  `AuthError{signature_invalid}` (point 7).
- **(d) Then parse.** `provider_reference` is required, and `outcome` must be in the closed enum
  `approved|rejected|pending|review_required|expired|error`.
  - A failure is `kyc.ErrCallbackMalformedBody` → 400 `callback rejected`, with no audit row.
  - A legacy signature field → `ErrSignatureInvalid` → 401.
- **(e) Load the verification** by reference with an **explicit `tenant_id = $3` predicate** on top
  of RLS (`verification_service.go:256-262`). Not found → 404, which is verified-caller-only.
- **(f) Monotonic transition and audit** (B7).

Before (c) succeeds: no tenant-scoped read, lock, write or audit row (I1). Every pre-verification
failure gets the identical 401 `{"error":{"code":"unauthorized","message":"callback rejected"}}`.

**B7. Replay and idempotency.**
- **Idempotency key.** The mock payload has no event id, and none is added. Idempotency is by the
  existing UNIQUE `(tenant_id, provider_id, provider_reference)` (`migrations/0040…up.sql:84`) plus a
  **monotonic** provider-driven rank: `unverified 0 < pending 1 < review_required 2 <
  approved|rejected|expired 3 (terminal)`.
- **Forward** (`rank(new) > rank(cur)`): `updateVerificationStatus` runs as a compare-and-set,
  `UPDATE … WHERE id=$1 AND status=$cur`. A lost race re-reads and re-evaluates. One success audit
  row is written.
- **Equal or backward** (every replay, anything after terminal, anything after a staff decision): no
  state change and **no audit row**. The response is 204, plus an allow-listed `kyc_webhook_noop`
  info line.
- **`error`:** no state change, one failure audit row (verified sender only, as today).
- **Staff decisions.** `ReviewVerification` (`:310`) is unaffected.
- **Concurrence.** `identity-compliance` must concur on the rank.

**B8 (cross-tenant).** A-signed bytes and headers posted to B: the signing input is rebuilt with B's
id and checked under B's derived key → 401 at step (c), before any B-scoped read.

**B9. Real vendor.**
- None exists. The interface is ready.
- The real resolver (FORCE-RLS handle table + secret store) is **NOT IMPLEMENTED**. It is blocked on
  the secret-store ADR and human-authorized provisioning, as for payments. Real vendors must meet
  point 3: per-merchant keys or a signed account id, and a timestamp tolerance.
- KYC-WH-1 closes **for the MOCK only**. A real vendor stays launch-blocking until it has a resolver
  and conformance tests.

## C. CAS-WH-TENANT-1

**C1. Interfaces.**
- `CasinoProvider.HandleCallback(ctx, in webhookauth.Inbound, cred webhookauth.Credential)` replaces
  `types.go:643`.
- `casino.NewOrchestrator(providers, resolver webhookauth.Resolver)` replaces `orchestrator.go:41`,
  for parity with payments. There are 91 test sites, all mechanical. A nil resolver fails closed,
  which forces explicit wiring.
- `ReceiveCallback(ctx, tx, tenantID, providerID, in webhookauth.Inbound)` replaces `:561`.
- `casino.ErrCallbackSignatureInvalid = webhookauth.ErrSignatureInvalid` (`types.go:174`).

**C2. Mock** (`casino/mock.go`, MOCK):
- `signingSecret` (`:49-55`) becomes `master` from `NewMockMaster()`.
- `NewMockWebhookCredentials(p)` uses the casino label and `mock-v1`.
- `CallbackPayload(tenantID, …) webhookauth.Inbound` (`:342-354`) returns headers plus a body with no
  signature. It covers about 140 test sites plus the 3 play-handler sites.
- Delete `sign`, the body `Signature` field and the NUL-joined MAC framing (`:289-313`). The raw-byte
  MAC removes field shifting.
- **`HandleCallback`:**
  1. `casinoScheme.Verify` over the raw bytes.
  2. Only then `json.Unmarshal`, the NUL-byte check (kept as validation) and the enum/UUID checks →
     `casino.ErrCallbackMalformedBody` → 400.
  3. A legacy `signature` field → `ErrSignatureInvalid`.

**C3. Order.**
- (a) Registered, else `provider_unregistered`.
- (b) Resolve plus the equality check.
- (c) `HandleCallback`.
- Then, **unchanged and post-verification**: `LoadCapability`/`status==active` (`:574-583`) → 503.

The casino capability stays a deliberate money-path kill switch, which differs from payments I4 by
the existing casino design, and only verified callers observe it. The casino path has **no**
pre-verification tenant-scoped read.

**C4. Replay and idempotency are unchanged:**
- the ledger `(tenant, provider_id, provider_tx_id)` key;
- round correlation (`:536-538`);
- F-7 → 409 (`mapReplayPayloadMismatch`);
- `ErrAlreadyRolledBack` → 409;
- the per-tenant tombstone key `tombstone:<provider>:<orig>` (`:1471-1485`).

Keys use the route `provider_id` the credential verified (point 6).

**C5. No financial effect on rejection.**
- Every auth failure returns before `postBet/postWin/postRollback`, and the transaction rolls back.
- A cross-tenant rollback gets 401 before the original lookup. Result: **no tombstone in the wrong
  tenant**, no ledger, projection, round or audit row.

**C6. Handler** (`casino_handlers.go:300-440`):
- `webhookPreamble` (1 MiB `:25`, `casino_webhook_auth_failed`).
- `AuthError` → uniform 401. This replaces the 404s at `:318-333,359-362` and the 400s at
  `:336-343,351-357`.
- The post-verification mappings (`:363-430`) and the success body `{outcome, tombstoned,
  ledger_transaction_id?, decline_reason?}` are unchanged.

**C7. Gating.**
- The casino webhook is the real provider-facing route, so it **stays registered**.
- `mockProviderWiring` supplies `casino.NewMockWebhookCredentials(mockCasino)` **only if
  `cfg.TestSupportRoutesEnabled()`**; otherwise the resolver is nil. Every production casino callback
  then gets 401 `no_resolver`, and no casino money moves. That is correct: no real aggregator exists.
- The mock adapter stays registered for catalogue and launch. That is pre-existing and harmless once
  nothing verifies, and it is noted for `architect`.

**C8. Play simulation (ADR 0085/0048).**
- The routes stay gated (`casino_routes.go:40-44`) and use `mock.CallbackPayload(tc.TenantID, …)`
  plus `ReceiveCallback(…, tc.TenantID, …)`. The tenant comes from the JWT only, and the signed bytes
  never leave the process.
- `writeCasinoCallbackError` (`casino_play_handlers.go:222`) maps `AuthError` → 503 "simulated play
  is misconfigured", mirroring the route-kind rule at `payment_callback_errors.go:36-44`.
- One flag gates both the resolver and the routes, so they cannot diverge.

## D. Environment gating matrix

Environments:
- `prod`: `APP_ENV=production`; `Load()` refuses test support there.
- `TS-on`: non-production with `TEST_SUPPORT_ENDPOINTS_ENABLED=true`, as in staging Terraform.
- `TS-off`: non-production with the flag unset or false.

| Surface | prod | TS-on | TS-off |
|---|---|---|---|
| KYC webhook | **absent → 404** | present, MOCK resolver | **absent → 404** |
| KYC mock / `POST /v1/me/kyc/verifications` | absent → 503 | present | absent → 503 |
| Staff KYC routes | present | present | present |
| Casino webhook | present, nil resolver → **all 401** | present, MOCK | present, nil → all 401 |
| Casino play-simulation routes | absent | present | absent |
| Payments webhook | present, MOCK resolver (unchanged; key unreachable) | present | present |
| Payments simulate-callback | absent | present | absent |

**Payments asymmetry.**
- The payments MOCK resolver is still wired unconditionally (`main.go:148-151`). It is not
  externally usable, so it is not required for 10.2.
- **Recommendation (orchestrator's call; small, reversible):** gate it identically in a separate
  commit after the extraction, for a uniform matrix. Tests wire the resolver explicitly.
- If declined, register it as `PAYWH-GATE-1`, deferred.

## E. PAYWH-BRAND-1 / RL-1 / TS-1: none is required, so defer all three

The two findings are **forgery** and **cross-tenant effect**. The tenant-bound, per-tenant credential
closes both.

- **BRAND-1** (brand scoping in `ProviderAcceptsWebhook`) is payments brand routing. KYC has no
  per-brand provider config, and the casino check is tenant-wide and post-verification. It has no
  bearing on either finding.
- **RL-1** (rate limiting) is an availability concern. The pre-verification cost is one platform
  tenant lookup plus one HMAC, and it writes nothing. Same profile as payments.
- **TS-1** (timestamp window). Replaying a captured callback has no effect in either domain: KYC has
  the monotonic rank, terminal no-op and no audit append; casino has `provider_tx_id` idempotency,
  F-7 409 and tombstones. Point 3 already requires timestamps for real adapters.

No human gate is triggered.

## F. Git history

- **Compromised.** The removed constant remains in history (Stage 4F through the fix commit,
  including deployed `9190d5d`). It **must be treated as compromised** and never used as, or derived
  into, any real credential.
- **No history rewrite.** The branch is shared, and after the fix the value authenticates nothing.
- **Redact the docs copy.** The value is quoted in
  `docs/plans/stage-10.1-planning/11-pay-wh-tenant-1-design.md` §8. Redact that working-tree copy to
  `<redacted: historical KYC mock constant, compromised; see Stage 10.2 §F>`.
- **G7 check.** `git grep` for the value must return nothing in the tree. The check script reads the
  value at run time from `git show <pre-fix>:cmd/platform-api/main.go` and never stores it.
- **Runbook.** Update `docs/runbooks/production-configuration-checklist.md:100-106`: mock credentials
  are per-process derived, and the KYC mock and webhook are gated.
- **Staging** runs the forgeable path until a human-authorized refresh. Staging KYC `approved` rows
  are untrusted synthetic data. This is a completion-report statement, not a decision.

## G. DB/RLS, OpenAPI, audit

**DB/RLS: no migration and no policy change.**
- KYC uses the existing unique index plus an explicit tenant predicate; the compare-and-set runs
  under the existing FORCE RLS.
- Casino is unchanged.
- The KYC and casino paths run **no** tenant-scoped statement before verification, apart from
  `WithTenant`'s `set_config`.

**OpenAPI** (`platform-api.yaml`):
- **KYC webhook** (`:2582-2612`, rewrite):
  - `security: []`; `providerID` pattern; required headers `X-KYC-Signature` (`^v1=[0-9a-f]{64}$`)
    and `X-KYC-Key-Id`.
  - The description quotes the signing input exactly, says the tenant is never read from the body,
    and states the route exists only with test support.
  - `KYCMockCallback` schema: `provider_reference` required, `outcome` enum, `reason`, at most 256
    KiB.
  - Responses:
    - **204** — fixes the mismatch by changing the handler from `writeJSON(200, verification)` to
      `WriteHeader(204)`, so no identifiers are echoed externally;
    - 400 — malformed body after verification;
    - 401 — `callback rejected`;
    - 404 — unknown reference, verified callers only;
    - 500.
- **Player vs staff schema.** New `PlayerVerification` without `provider_reference`, used by
  `/v1/me/kyc/verifications` (`:1932-1972`). `Verification` (`:4612`) stays for staff.
- **Casino webhook (new entry):**
  - `POST /v1/webhooks/casino/{tenantSlug}/{providerID}`; `X-Casino-*` headers.
  - `CasinoMockCallback` schema: `event_type` `bet|win|rollback`, `provider_tx_id`,
    `original_provider_tx_id`, `round_id`, `provider_game_id`, `amount` int64 minor units,
    `asset_code`, `outcome`, `decline_reason`, `player_account_id`, `session_id`; at most 1 MiB.
  - Responses: 200 `{outcome, tombstoned, ledger_transaction_id, decline_reason}`, 400, 401, 409, 503
    (verified callers only), 500.
- **Contract tests.** New `openapi_kycwebhook_contract_test.go` and
  `openapi_casinowebhook_contract_test.go`, modelled on the payments one.

**Audit and alerting.**
- No `audit_log` row for any unauthenticated failure.
- Alerts are the allow-listed events in §A; `reason=signature_invalid` is the forgery signal.
- Existing post-verification audits are unchanged, except the B7 no-op rule.

**Decision records (`architect`):**
- ADR 0022 §3 Status: KYC and casino conform (MOCK only) via `internal/webhookauth`.
- ADR 0028 amendment: gate, no simulate route, rank.
- ADR 0025 amendment: verify-before-parse, post-verification capability.
- ADR 0019 row 271 already covers casino.

## H. Tests (`qa`; integration tests run as the NOBYPASSRLS runtime role)

**No-effect checklist.** Every rejection asserts, from a fresh transaction, that both tenants have:
- no new or changed `kyc_verifications` rows;
- no `ledger_transactions`, `ledger_entries` or tombstones;
- no `casino_*` rows;
- no `audit_log` rows;
- debits = credits, with projections unchanged.

**Pre-fix evidence.** Run against `d76bdd3` before fixing. Output goes to `stage-10.2-planning/evidence/`
with no secret value; the tests are then retired or inverted.

| # | Test | Expected pre-fix |
|---|---|---|
| E1 | `TestKYCWH1_PreFix_PlayerSelfApprovalForgeSucceeds`: the player creates a verification, reads its own `provider_reference`, and signs with attacker-side `crypto/hmac` using the provider's static secret. The secret is a runtime-random value modelling the public constant; no literal is used. The player posts to their own slug. | 200, `approved`, audit row written |
| E2 | `TestKYCWH1_PreFix_SecretIsCompileTimeConstant` (`cmd/platform-api`, go/ast): the argument to `NewMockKYCProvider` is a string `const`. It asserts the kind only and never prints the value. | Passes. **Inverts post-fix** into a G7 guard: 0 arguments, no const string feeds a mock credential. |
| E3 | `TestKYCWH1_PreFix_UngatedWithTestSupportOff`: the webhook is reachable with test support off. | non-404 |
| E4 | `TestCasWH_PreFix_CrossTenantCallbackSucceeds`: A and B both have `mock-casino`; a rollback of an unseen tx is posted to B's slug. | 200, `tombstoned:true`, **tombstone in B** |

**KYC:**

| # | Case | Expected |
|---|---|---|
| K1 | E1 with any signature a player can compute | 401, no effect |
| K2 | Valid in-process signature, A→A `approved` | 204, `approved`, 1 audit row |
| K3 | A-signed → B, where B has the same reference string (direct insert) | 401; statement capture shows no read of B's row |
| K4 | Equal-secret test resolver; A-signed → B | 401 (the tenant is in the MAC) |
| K5 | Tamper: each field, one whitespace byte, headers (63/65 hex, uppercase, missing, unknown key id), provider id, legacy `signature` field, legacy trailing-newline format | 401 each |
| K6 | Unknown slug / suspended / unregistered provider / bad charset / oversized / no headers / bad signature / non-JSON with valid headers | byte-identical 401 (minus `request_id`) |
| K7 | Bad signature under statement capture | no tenant-scoped statement; no audit row |
| K8 | Replay of K2 | 204, no change, **no extra audit row** |
| K8 | `pending` after `review_required` | no-op |
| K8 | Anything after a staff decision | no-op |
| K8 | 8 concurrent `approved`/`rejected` | exactly one terminal state, 1 audit row |
| K9 | Verified, unknown reference | 404 |
| K9 | Verified, bad outcome or non-JSON | 400, no audit row |
| K9 | Outcome `error` | 204, status unchanged, failure audit row |
| K10 | Player POST/GET `/v1/me/kyc/verifications` | no `provider_reference` key |
| K10 | Staff routes | reference still present |
| K11 | `KYCWebhookEnabled=false` | 404 |
| K11 | `mockProviderWiring` with {production} and {staging, TS=false} | nil KYC orchestrator, nil casino resolver |
| K11 | `mockProviderWiring` with {staging, TS=true} | both present |
| K11 | Nil resolver | 401 `no_resolver` |
| K12 | Log line per reason | allow-listed keys only; never body, header values, signature or slug |
| K13 | Unit | keys differ per tenant, instance and domain label; stable within an instance; `%v/%+v/%#v` redact |
| K14 | OpenAPI contract | 204, headers, `PlayerVerification` |
| K15 | Rewrite `kyc_flow_integration_test.go:706-760,815-835` to use `CallbackPayload(tenant.ID,…)` and a tenant-scoped DB helper for the reference | each assertion re-verified individually |

**Casino:**

| # | Case | Expected |
|---|---|---|
| C1 | E4 re-run | **401, no tombstone in B**, full no-effect checklist |
| C2 | A→A bet/win/rollback | 200 |
| C3 | A-signed bet → B, where B has a same-id session | 401 |
| C4 | Equal-secret resolver | 401 |
| C5 | Tamper amount / tx id / round / session / player / whitespace / headers / legacy `signature` | 401 |
| C6 | K6 matrix, including the non-JSON body that gives 400 today | byte-identical 401 |
| C7 | Bad signature under statement capture | no tenant-scoped statement; `LoadCapability` not run |
| C8 | Same `provider_tx_id` replay | idempotent 200 |
| C8 | F-7 mismatch | 409 |
| C8 | Double rollback | 409 |
| C8 | Rollback, then late original | rejected by the tombstone |
| C8 | 8 concurrent duplicates | one posting |
| C9 | Disabled capability, valid signature | 503, no posting |
| C10 | Nil resolver (production wiring) | all 401; play routes absent |
| C11 | Play routes with TS-on | green |
| C11 | Play routes, broken resolver | 503 misconfigured; the response carries no body or signature |
| C12 | `casino/conformance_test.go` tenant-binding case | mandatory for the first real adapter; skip-to-fail rule as in payments C4 |
| C13 | Existing assertions at `casino_flow_integration_test.go:291,315,338,387-395` (400/404 → 401) | re-verified individually, not by blind replace |
| C14 | OpenAPI contract | new entry passes |

**Shared and payments:**
- **P1.** The full payments suite, the 17-test plan (paper 16) and the payments OpenAPI contract test
  are green **unchanged** after the extraction commit.
- **P2.** `webhookauth` unit tests:
  - framing;
  - `ParseHeaders` per scheme;
  - a cross-scheme signature is rejected under an equal key;
  - `MockResolver` rejects a foreign provider or key id;
  - redaction.
- **P3.** Full regression (G4).

## I. Human decisions: none required (checked strictly)

These are already decided or already behind a human gate:
- **KYC mock out of production.** Gating it out, and losing production self-service KYC as a result,
  is decided by ADR 0091.
- **Secret store and real resolvers** are future work behind the existing gate (ADR 0022 amendment).
- **Staging refresh** is reserved to the human by ADR 0091.

The rest is reversible engineering in scope:
- the extraction;
- header names;
- 204;
- the rank (needs `identity-compliance` concurrence);
- the casino post-verification capability check;
- the optional payments gate.

**Completion-report disclosures:**
- staging KYC data is untrusted until the refresh;
- both closures are MOCK-only;
- the mock casino adapter is still registered in production for catalogue and launch.

**Review scope.**
- **Covered:** the KYC and casino webhook handlers, orchestrators, mocks and wiring; the payments
  contract, for extraction; the ADR 0085 gate; KYC permission grantees; OpenAPI; migration 0040's
  index.
- **Not covered:** code (none yet); real vendor schemes; secret-store design; volumetric DoS;
  CI-FLAKE-281; the AWS/staging state (not inspected).

## J. Review record and Orchestrator rulings (binding for implementation)

Reviews recorded verbatim in this folder: `03-review-qa-test-plan.md` (qa),
`04-review-backend.md` (backend), `05-review-identity-compliance.md`
(identity-compliance), `06-review-casino.md` (casino), `07-review-architect-db.md`
(architect + DB/RLS). `02-ci-flake-281-investigation.md` is the CI-FLAKE-281 paper
(devops). No reviewer raised a blocking objection or a human decision.

| # | Source | Ruling |
|---|---|---|
| J1 | qa | The QA test plan (name map, mutation checks, payments regression guarantee, mechanical no-print check for E2) is adopted as binding for G1/G2/G4. Every guard claimed as "required" must be shown to go red when the guard is removed. |
| J2 | backend | Required: an `errors.Is`/`errors.As` regression test proving the `internal/payments` aliases and sentinels still match values produced by `internal/webhookauth`. |
| J3 | architect R1 | `webhookauth.Scheme` is the platform-defined MOCK wire scheme only; the package doc says so. Real adapters verify with the vendor scheme and still obey contract points 1–7. |
| J4 | architect R2 | Payments MUST adopt the shared preamble (not optional). If byte-identity tests fail, fix the shared code, never fork. Only if byte-identity is provably impossible: register `WH-PREAMBLE-1` and disclose. |
| J5 | architect R3 | `ReceiveCallback(tenantID, providerID, in)` overwrites `in.TenantID`/`in.ProviderID` from its parameters first, as payments does. One tenant id from route → RLS → resolver → signing input → credential check → writes. |
| J6 | architect R4 | KYC reference lookup is `tenant_id AND provider_id AND provider_reference`. Status change is a compare-and-set including `tenant_id` and the current status; at most 3 re-reads on a lost race, then error; audit row in the same transaction. |
| J7 | architect R5 | KYC route registration and the KYC resolver come from one `mockProviderWiring` result; a test (K11) proves they cannot diverge. |
| J8 | architect §2 | Strict I1 for KYC and casino: before verification only `GetTenantBySlug` (platform-wide) and `WithTenant`'s `set_config` run. K7/C7 statement capture is G3 evidence. |
| J9 | backend + architect | **PAYWH-GATE-1 is INCLUDED in 10.2**, as a separate commit after the extraction, inside `mockProviderWiring`. Condition: payments tests pass without edits other than wiring tests; otherwise the commit is dropped and PAYWH-GATE-1 registered as deferred. |
| J10 | architect §4 | PAYWH-BRAND-1, PAYWH-RL-1, PAYWH-TS-1 stay deferred (all reviewers agree). |
| J11 | identity-compliance | Forward-only rank approved. Added to B6/B7: a callback never resurrects a terminal verification; only a new `CreateVerification` row starts a new attempt. |
| J12 | identity-compliance | Forward note for B9: a future hosted-KYC vendor must issue its own short-lived session token for player redirect and must not repurpose `provider_reference`. |
| J13 | identity-compliance | Add K16: `POST /v1/me/kyc/verifications` returns 503 (not 404) when the KYC orchestrator is nil (production / test support off). K5 names the cross-verification `provider_reference` substitution case explicitly. |
| J14 | casino | Approved without change. Call-site counts corrected: `CallbackPayload` = 119, `NewOrchestrator` = 91; re-grep at implementation. |
| J15 | architect §5 | ADR amendments A (0022 §3, points 8–9), B (0028), C (0025), D (0019, needs `ledger-finance` concurrence), E (0085 §1), and updates to `docs/architecture/08-casino-integration-architecture.md` (trust-model steps) and a pointer in `payment-orchestration.md` §10, using the architect's draft text. ADR 0091 is not amended. |
| J16 | architect §3/§4 | New registry items (not 10.2 scope): **MOCK-ADAPTER-PROD-1** (mock payments/casino adapters remain registered in production for initiation, catalogue and launch; pre-launch checklist) and **CAS-CAP-ROLLBACK-1** (a disabled casino capability 503s a verified rollback; follow-up for `casino` + `ledger-finance`). |
| J17 | architect §6 | Completion report must disclose: staging stays forgeable until the human-authorised refresh and its KYC rows are untrusted synthetic data; both fixes are MOCK-only; MOCK-ADAPTER-PROD-1; CAS-CAP-ROLLBACK-1. |

**Disagreements.** None substantive. The only divergence was PAYWH-GATE-1 (the design
left it optional); backend and architect both recommended inclusion, ruled in J9.

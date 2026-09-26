# PAY-WH-TENANT-1 + API-DOC-PAYWH — design (Stage 10.1, DESIGN ONLY)

Author: `security`. Verified at HEAD `9fb1aa2`. Scope: ADR 0090 amendment item 3; finding S-6
(`04-review-security.md`). No code changes here. Reviewers before implementation: `payments`,
`backend`, `architect`, database/RLS, `qa`. A post-implementation `security` diff review is
mandatory before anything is labelled IMPLEMENTED.

## 0. Current state (verified)

- **Tenant from slug only.** `internal/httpserver/deposit_handlers.go:267-295` takes the tenant
  from `{tenantSlug}` alone (`GetTenantBySlug`). `:308-312` then opens `WithTenant(t.ID)` and
  calls `ReceiveCallback` with the body only; headers are not passed.
- **The adapter never sees the tenant.** `internal/payments/orchestrator.go:829-852` calls
  `HandleCallback(ctx, raw)` (`types.go:419`) with no tenant.
- **One key for all tenants, tenant not signed.** `internal/payments/mock.go:72,105-110` holds
  one per-process `crypto/rand` key. Nothing in `internal/config` sets it, and every tenant
  shares it. `mock.go:207-213` MACs event_type, references, outcome, amount, asset,
  decline_reason and cascadable. It does **not** MAC tenant or provider_id. The signature sits
  in the body (`:323`). Fields are NUL-joined, but the payments mock has no NUL-byte guard (the
  casino mock has one).
- **S-6.** A callback signed for tenant A verifies at `/v1/webhooks/payments/{B-slug}/…`. A
  reversal of an original that is not visible in B writes a **tombstone in B**
  (`orchestrator.go:932-952`).
- **No credential storage.** There is no per-tenant provider credential table or secret-handle
  mechanism. `provider_capabilities` (`migrations/0024:25-49`) has `status` and
  `callback_capabilities`, and no secret field by design (ADR 0022 §2.2).
- **Replay protection is idempotency only.** There is no timestamp or nonce. Protection comes
  from:
  - the ledger `(tenant, provider, provider_tx_id)` key;
  - `deposit_intents (tenant_id, provider_id, provider_reference)`;
  - the terminal-state no-op (`orchestrator.go:899-902`);
  - tombstones.
- **Enumeration oracle.** An unknown or inactive slug gets 404, as does an unknown provider. A
  bad signature gets 400. So "valid slug + garbage body ⇒ 400" reveals which tenants exist,
  despite the claim at `:276-280`.

## 1. Decision authority

- **ADR 0022 §3 is not a human decision.** Its `OPEN DECISION` is owned by
  **`security`/`payments`** as a Stage 3B design item. This design decides it:
  - **candidate 1**: the per-tenant URL selects **exactly one** candidate credential, which
    must then verify;
  - there is never trial verification across tenants;
  - no payload field asserts the tenant.
  `architect` records this as an ADR 0022 §3 amendment. No HDR is reopened.
- **Open HUMAN decision touched, not decided.** Real PSP secret material needs a store (ADR 0003:
  "Vault or a cloud KMS"). That choice depends on the hosting decision in **ADR 0009 (OPEN)**, and
  provisioning it is an AWS change behind the Stage 10.1 stop point.
- **Fallback.** 10.1 builds only the resolver interface plus a `MOCK` resolver. The real resolver
  is `NOT IMPLEMENTED`, and a provider without a resolver **fails closed** (every callback 401).
  S-6 remains **launch-blocking for any real PSP**.

## 2. Canonical tenant binding

### 2.1 Per-(tenant, provider) credential

New in `internal/payments/types.go`:

```go
type WebhookCredential struct {
    TenantID          uuid.UUID
    ProviderID        string
    KeyID             string // selects exactly one key; rotation overlap = two key ids, same tenant
    Secret            []byte // never logged/errored/audited; String()/LogValue() redact
    Fingerprint       string // hex(sha256(Secret))[:16] - only loggable form
    MerchantAccountID string // optional; real adapters compare vendor-signed account id
}
type WebhookCredentialResolver interface {
    Resolve(ctx context.Context, tenantID uuid.UUID, providerID, keyID string) (WebhookCredential, error)
}
```

- **Orchestrator wiring.** `NewOrchestrator` (`orchestrator.go:46`) gains
  `map[providerID]WebhookCredentialResolver`. A provider with no resolver gets an auth failure.
  There is no fallback to verification without a tenant.
- **Equality check.** The orchestrator re-checks `cred.TenantID == tenantID` and
  `cred.ProviderID == providerID`. A mismatch fails closed.
- **Real resolver (future, NOT IMPLEMENTED).** It will read a tenant-scoped, FORCE-RLS handle
  table plus the secret store, under the §2.2 rules of ADR 0022 and §10 of
  payment-orchestration.md:
  - handle and fingerprint only in the table;
  - excluded from CDC;
  - audited, per-tenant rotation.
  **No migration in 10.1 (0094 is not used).** No reader for that table exists yet.
- **MOCK resolver** (`mock.go`, labelled `MOCK`):
  - `key = HMAC-SHA256(master, "igaming/payments-mock-webhook/v1"‖0x00‖tenant_id‖0x00‖provider_id)`,
    with `KeyID="mock-v1"`.
  - `master` is the existing per-process `crypto/rand` 32-byte secret, renamed `masterSecret`.
  - **No config change.** Nothing goes in env, config or the repo, and the key stays
    unrecoverable from outside the process.
  - Deriving keys from one master is acceptable **only** for the mock.

### 2.2 Signed content and headers

```
signing_input = "igaming.payments.webhook.v1" 0x00 tenant_id 0x00 provider_id 0x00 key_id 0x00 <raw body bytes>
header X-Payments-Signature: v1=<hex(HMAC-SHA256(cred.Secret, signing_input)), 64 lowercase hex>
header X-Payments-Key-Id:    <key_id, ^[a-z0-9-]{1,32}$>
```

- **Where the prefix values come from.**
  - `tenant_id` is the **route-resolved** `tenants.id`, as a canonical lowercase UUID. It never
    comes from the body.
  - `provider_id` is the path value, checked against `^[a-z0-9][a-z0-9-]{0,62}$`.
- **Framing is unambiguous.** None of the prefix values can contain 0x00, so the body is an
  unambiguous tail. This also removes the NUL field-shift weakness in `mock.go:207-213`.
- **Rebuilt, not trusted.** The verifier rebuilds `signing_input` with the tenant it resolved.
  So an A-signature fails for B **even if A and B shared a key**.
- **Comparison.** The signature must hex-decode to exactly 32 bytes, and is compared with
  `hmac.Equal`.
- **Signature location.** The body `signature` field (`mock.go:323`) is removed. A body that
  still carries one is rejected as malformed (after verification).
- **Replay protection is unchanged (§4).** No timestamp exists today, and closing S-6 does not
  need one.
- **Deferred: PAYWH-TS-1.** Real adapters MUST enforce the vendor's signed-timestamp tolerance.
  The mock may add `X-Payments-Timestamp` to the signing input later.

### 2.3 Interface change

- **`HandleCallback`** (`types.go:419`) becomes
  `HandleCallback(ctx, req InboundCallback, cred WebhookCredential) (CallbackEvent, error)`,
  with `InboundCallback{TenantID uuid.UUID; ProviderID string; Header http.Header; Body []byte}`.
- **`ReceiveCallback`** (`orchestrator.go:829`) takes `InboundCallback` instead of `rawPayload`.
- **No cross-tenant key access.** An adapter only ever receives the one credential resolved for
  the verified request tenant.

## 3. Verification order, consistency, errors, alerting

### 3.1 Order

1. **HTTP handler** (`deposit_handlers.go:257`):
   1. Check the `providerID` charset.
   2. `GetTenantBySlug` (`:274`).
   3. Require `status=='active'` (`:288`).
   4. Read the body under the 1 MiB limit (`:297-305`).
   5. Require both headers, well-formed.
   The slug is only a lookup hint.
2. **Inside `WithTenant(t.ID)`**, `ReceiveCallback` runs these steps before any write:
   - **(a)** The adapter must be registered.
   - **(b)** The provider must be configured for the tenant. New
     `payments.ProviderAcceptsWebhook(ctx, tx, tenantID, providerID)` in `capability.go` runs
     `SELECT EXISTS(SELECT 1 FROM provider_capabilities WHERE tenant_id=$1 AND provider_id=$2
     AND callback_capabilities IN ('webhook','both'))`. That is an explicit tenant predicate on
     top of RLS; a row for any brand counts.
     - `status` is **not** checked. `disabled` is a routing flag (`capability.go:52-54`), and
       rejecting callbacks for a disabled provider would strand money already in flight.
     - A compromised provider is revoked by removing its credential from the resolver.
     - `payments`/`architect` must confirm this ruling.
   - **(c)** `Resolve(tenantID, providerID, keyID)`, then the equality re-check.
   - **(d)** Key-material scan, still before verification (ADR 0022 §4.1).
   - **(e)** HMAC verification over the tenant-bound input.
   - **(f)** Parse and dispatch. PAY-REV-1's L2 lock and the tombstone stay after (e) (S-4c).
3. **Nothing happens before verification except one read.** Steps (a)–(e) run only the
   read-only `EXISTS`: no lock, no write. Any failure rolls back.
4. **Financial writes use the verified tenant.** They run under `t.ID`, which is both the RLS
   GUC and the MAC input.

### 3.2 Errors

New `payments.ErrCallbackAuthFailed` wraps a closed reason enum:

- `tenant_unknown`
- `tenant_inactive`
- `provider_invalid`
- `provider_unregistered`
- `provider_not_configured`
- `no_resolver`
- `credential_unavailable`
- `signature_missing`
- `signature_invalid`
- `key_material`

**Every reason gets the same response:** HTTP **401**,
`{"error":{"code":"unauthorized","message":"callback rejected"}}`, with identical headers. This
replaces the 404/400 split at `:274-295` and `:313-335`.

Responses after verification are unchanged and visible only to a verified caller:

| Case | Response |
|---|---|
| Intent not found | 404 `no matching deposit for this reference` |
| F-7 payload mismatch | 409 `callback rejected` |
| PAY-REV-1 already reversed | 409 `callback rejected` |
| Other failure | 500 |

KYC and casino webhooks are out of scope (§8).

**Residual (Low, accepted):** timing still differs, because an unknown slug skips the DB round
trip and the HMAC. Tenant slugs appear in brand URLs, so they are not secrets.

### 3.3 Alerting and audit

- **Log line.** Each auth failure emits one `payment_webhook_auth_failed` warn line. The
  **allow-list** of fields is:
  - `request_id`
  - `reason`
  - `tenant_id` (only if resolved)
  - `provider_id` (only if it passes the charset check)
  - `key_id` (only if it passes the charset check)
  - `credential_fingerprint` (only for `signature_invalid`)
  - `client_ip` (`clientIP`, `json.go:32`)
  - `body_len`
- **Never logged:** body, header values, signature, raw slug, `err` text.
- **Metric.** Emit a `reason`-labelled counter if the metrics facility exists.
- **No `audit_log` row for unauthenticated failures.** Unauthenticated input must not be able to
  append to a tenant's append-only store, which cannot be redacted. This is a storage-DoS and
  log-poisoning vector, and the same principle as S-4c.
- **Verified-but-rejected outcomes keep their audit rows:** PAY-REV-1 S-3, and the tombstone
  audit (`orchestrator.go:942`).
- **Rate limiting is not in scope.** The webhook routes have no limiter today. Deferred item
  **PAYWH-RL-1**: a per-IP limiter tuned to PSP egress IPs.

## 4. Idempotency and replay

- **Keys are unchanged:** the ledger key, `deposit_intents` uniqueness, and
  `tombstone:<provider>:<ref>` per tenant (`orchestrator.go:1058`).
- **Same-tenant replay** re-verifies, then is absorbed by:
  - the postDepositSuccess short-circuit;
  - the terminal-state no-op;
  - F-7 → 409;
  - PAY-REV-1 → 409.
- **Cross-tenant replay** of A's bytes and headers to B's slug: `signing_input` is rebuilt with
  B's id and checked under B's key → 401 at step (e). This happens **before** any B-scoped
  `deposit_intents` read, so B gets no ledger write, no tombstone and no audit row. The write
  effect of S-6 is closed.

## 5. RLS

- **No policy changes, no migration.**
- **One pre-verification statement.** Under `app.tenant_id`, the only statement before
  verification is the RLS-scoped, read-only `EXISTS`, with no `FOR UPDATE`.
- **Financial writes stay where they are.** They happen after (e), in the same
  `WithTenant(t.ID)` transaction as today.

## 6. Mock, simulation route, config, migration, OpenAPI

- **`mock.go` changes:**
  - `masterSecret` replaces `signingSecret`, and `deriveKey(tenantID, providerID)` is added.
  - New `MockWebhookCredentials{m}` resolves only `KeyID=="mock-v1"` for `m.providerID`.
  - `CallbackPayload(tenantID uuid.UUID, eventType, …) InboundCallback` returns the body without
    a `signature` field, plus both headers.
  - `sign(body)` is deleted.
  - `HandleCallback` = (d) + (e) + parse.
  - Doc comments at `:53-70`, `:176-182` and `:339-353` are updated. The `MOCK` label is kept.
- **`cmd/platform-api/main.go:140-142`.** Bind the mock to a variable and pass
  `{"mock-payments": payments.MockWebhookCredentials(mock)}`.
- **Simulation route** (`payment_deposit_simulation_handlers.go:296-298`):
  - The tenant comes from the authenticated JWT context only (`tc.TenantID`); no request field
    can name one. The route calls `mock.CallbackPayload(tc.TenantID, …)`, then
    `ReceiveCallback(ctx, tx, tc.TenantID, providerID, inbound)`.
  - The signed bytes never leave the process. The response carries status only (`:319-323`), so
    the route cannot mint a callback that can be replayed at another tenant.
  - `writeDepositCallbackError` (`:216`) maps `ErrCallbackAuthFailed` to a generic 503, because
    that error means the mock is misconfigured.
- **Config:** none. **Migration:** none.
- **Doc corrections** (`architect` records the ADR 0022 §3 amendment). Update:
  - `payment-orchestration.md` around lines 88-100;
  - `07-payments-architecture.md:105-108`;
  - the code comments at `deposit_handlers.go:244-256` and `orchestrator.go:798-828`.

  New wording: "tenant resolved from the path, bound by a per-(tenant, provider) credential and
  a tenant-bound signature (MOCK resolver only; real resolver NOT IMPLEMENTED)".
- **OpenAPI (API-DOC-PAYWH).** In `docs/api/openapi/platform-api.yaml`, add a new entry before
  `:2455` for `POST /v1/webhooks/payments/{tenantSlug}/{providerID}`:
  - `security: []`.
  - Path parameters: `tenantSlug` (string) and `providerID` (with the pattern above).
  - Required headers: `X-Payments-Signature` (`^v1=[0-9a-f]{64}$`) and `X-Payments-Key-Id`.
  - Description:
    - quotes the exact `signing_input`;
    - states the tenant is never read from the body;
    - states the mock wire shape is platform-invented, and real adapters map to it (§9).
  - Body schema `PaymentsMockCallback`:
    - `event_type` enum `deposit|deposit_reversal`;
    - `provider_reference` (required);
    - `original_provider_reference`;
    - `outcome` enum `succeeded|declined|ambiguous|pending`;
    - `amount` int64 minor units;
    - `asset_code`;
    - `decline_reason`;
    - `cascadable`;
    - body at most 1 MiB.
  - Responses:
    - 200 `{deposit_intent_id,status,tombstoned}`;
    - 400 malformed body after verification, or body too large;
    - 401 `callback rejected`;
    - 404 (verified callers only);
    - 409;
    - 500;
    - 503 webhooks disabled.

## 7. Tests (`qa`; integration tests run as the NOBYPASSRLS runtime role)

Setup: tenants A and B, both with a `mock-payments` capability row. Every rejection also asserts,
from a **fresh transaction**, that:
- B's `ledger_transactions`, `deposit_intents` and `audit_log` have no new rows;
- debits = credits;
- projections are unchanged.

| # | Test | Expected |
|---|---|---|
| T1 | A-signed deposit to A | 200, credited once |
| T2 | A-signed deposit to B, where B has an intent with the same `provider_reference` string | 401, no effect in A or B |
| T3 | A-signed **reversal** of a reference unseen in B, posted to B (S-6 regression) | 401, **no tombstone in B** |
| T4 | B-signed to B | 200 |
| T5 | Test resolver gives A and B the **same** secret; A-signed posted to B | 401 (proves the tenant is in the MAC input) |
| T6 | Payload modified (amount, outcome, reference, or one whitespace byte) under the original signature | 401 |
| T7 | Binding modified | 401 each |
| T8 | Replay of a success A→A | 200, no second credit |
| T8 | Replayed reversal | 409 |
| T8 | 8 concurrent duplicates | exactly one posting |
| T9 | Unknown-provider/tenant indistinguishability | byte-identical 401 (status and body minus request_id) |
| T10 | `status='disabled'` capability, valid signature | 200 (pins the §3.1(b) ruling) |
| T11 | Valid tenant, bad signature | a statement-capturing tx wrapper shows no INSERT/UPDATE/`FOR UPDATE` |
| T11 | `ProviderAcceptsWebhook` under B's GUC | does not see A's rows |
| T12 | Each reason's log line | only allow-listed keys; no body, signature, header values or raw slug; no `audit_log` row for any 401 |
| T13 | Simulation route: an A player's own intent | 200; signer receives `tc.TenantID`; response exposes no body or signature |
| T14 | Adapter with no resolver (conformance) | fails closed with 401 |
| T15 | Unit tests | derived keys differ for A and B, are stable within a process and differ across instances; `%+v` of a credential never shows the secret |

T7 covers:
- signature computed for tenant B but posted to A;
- wrong provider_id in the signing input;
- unknown key_id;
- missing header;
- 63 or 65 hex characters;
- a body that still carries `signature`.

T9 compares these cases:
- unknown slug;
- suspended tenant;
- unregistered provider;
- provider not configured for the tenant;
- `polling_only`;
- bad provider_id charset;
- bad signature;
- key-material body.

**Existing tests to update (not delete):**
- ~38 `CallbackPayload` call sites in `internal/payments/*_test.go` and ~5 in
  `internal/httpserver/*_test.go`;
- webhook assertions that expect 404/400, which become 401.

## 8. KYC webhook comparison: finding only, scope NOT expanded

**KYC-WH-1 (High, pre-existing, out of scope, launch-blocking).**

The KYC webhook has the same flaw as S-6, and it is worse:
- `POST /v1/webhooks/kyc/{tenantSlug}/{providerID}` (`kyc_admin_handlers.go:407-479`) takes the
  tenant from the slug only.
- `internal/kyc/mock_provider.go:138-141` uses one HMAC over the body, with no tenant.
- The secret is a **constant committed to the repo**
  (`cmd/platform-api/main.go:43`, `"dev-mock-kyc-webhook-secret-not-for-production"`).
- The mock and the route are registered **unconditionally** (`main.go:261-263`,
  `kyc_routes.go:42`). No environment gate exists; only the runbook
  (`production-configuration-checklist.md:103`) warns.
- The player is given `provider_reference` (`kyc_handlers.go:52`).

**Failure scenario** on any deployment of this binary, staging included:
1. A player creates a KYC verification.
2. The player reads its `provider_reference`.
3. The player HMACs `{"provider_reference":…,"outcome":"approved"}` with the public constant.
4. The player POSTs it to their own tenant's webhook.
5. The verification becomes `approved` (`kyc/provider.go:193-196`): KYC self-approval.

Forging cross-tenant works the same way. The same 404/400 oracle exists. The OpenAPI entry
(`platform-api.yaml:2480`) documents 204, but the handler returns 200 with JSON.

**Recommended follow-up:** apply this contract, gate the mock off in production, and route-gate
the endpoint. This needs a human scope ruling and is not done here.

**CAS-WH-TENANT-1 (Medium, pre-existing, out of scope).** The casino webhook has the same slug-only
tenant and per-process global key, with no tenant in the MAC (`internal/casino/mock.go:306-313`).
Rollbacks of unseen transactions tombstone (`casino/orchestrator.go:1260`). The key is not
public, so an attacker needs a captured callback.

## 9. Real PSP statement and adapter readiness

**No real PSP is integrated.** Everything here is `MOCK` or contract. Each future real adapter
must meet four requirements:

1. **Resolver.** It needs a resolver backed by the handle table and the secret store. That
   resolver is NOT IMPLEMENTED and is blocked on ADR 0009 and the secret-store choice (human).
2. **Signature mapping.** `HandleCallback` maps the vendor's native scheme onto this contract:
   - Verify with the **single** credential resolved for the **route** tenant and the vendor key
     id.
   - If the vendor signs a merchant/account id, require it to equal `cred.MerchantAccountID`.
     That is the tenant binding when our tenant id cannot be in the signed content.
   - If the vendor has neither per-merchant keys nor a signed account id, the adapter is **not
     acceptable** without an ADR (for example per-tenant mTLS or a per-tenant URL secret).
   - Enforce PAYWH-TS-1 (the vendor's timestamp tolerance).
3. **No cross-tenant trial.** Never try keys across tenants.
4. **Errors.** Authentication failures return only `ErrCallbackAuthFailed` or
   `ErrCallbackSignatureInvalid`.

**Launch position.** Once this lands and passes review, S-6 is closed **for the MOCK** only. It
stays **launch-blocking for any real PSP** until requirements 1 and 2 exist for that PSP.

## 10. Scope of this review

**Covered:**
- the payments webhook handler, `ReceiveCallback` and the tombstone path;
- mock signing and the simulation route;
- `provider_capabilities`, config and `main.go` wiring;
- ADR 0022 §2.2/§3, ADR 0019 (actor row), ADR 0014, ADR 0003 and payment-orchestration §10;
- KYC and casino webhooks, only as needed for §8.

ADR 0014 option 2 (a JWT service principal) does not apply: a PSP cannot carry our JWT, so it
falls under ADR 0019's "verified provider callback" row instead.

**Not covered:** code (none yet), volumetric DoS, real vendor schemes, secret-store design, and
the casino/KYC fixes.

---

## Review record and Orchestrator rulings (2026-09-26)

| Reviewer | Verdict | Paper |
|---|---|---|
| `payments` | APPROVE with 3 changes | `13-pay-wh-review-payments.md` |
| `backend` | APPROVE with 4 required changes | `14-pay-wh-review-backend.md` |
| `architect` (+ database/RLS) | APPROVED WITH CHANGES C1–C7 | `15-pay-wh-review-architect-db.md` |
| `qa` | Binding 17-test plan | `16-pay-wh-review-qa-test-plan.md` |
| `identity-compliance` | KYC-WH-1 CONFIRMED (read-only) | `12-kyc-wh-1-verification.md` |

**Binding rulings for implementation** (these supersede conflicting text above):

1. **ADR 0022 §3 is closed by this design** (engineering item owned by security/payments; not in the Human Decision Register). The mechanism is candidate 1: the URL tenant only *selects* the single per-(tenant, provider) credential, which must verify a signature over content that includes the tenant. Candidate 2 (provider account id selecting the tenant) is not adopted. The architect's amendment text (paper 15) is applied to ADR 0022, and ADR 0019's matrix wording is corrected (with `ledger-finance` concurrence recorded).
2. **C1:** ADR 0009 is Accepted, with its pre-production confirmations still open. Choosing the real secret store is a future engineering ADR, and provisioning it needs human authorization. Stage 10.1 ships a **MOCK resolver only**; the real resolver is NOT IMPLEMENTED, and S-6 remains launch-blocking for any real PSP until it exists.
3. **C2/C3:** a single injected `WebhookCredentialResolver`, not a per-provider map. No `MerchantAccountID` field in 10.1.
4. **Merge order (payments):** PAY-REV-1 lands first. PAY-WH-TENANT-1 is implemented on top of it, and the "verify, then lock" ordering is re-verified against the merged code.
5. **Backend changes:**
   - one shared error mapper with a route-kind flag: an authentication failure is 401 `callback rejected` on the public webhook and 503 on the simulate route;
   - the `ErrUnknownProvider` → 404 branch is folded into the uniform 401;
   - header format validation (`X-Payments-Signature`, `X-Payments-Key-Id`) happens in the HTTP handler, before any tenant or database work;
   - a 404 remains only for the post-verification `ErrDepositIntentNotFound`.
6. **Invariants I1–I3 (architect) are binding for `qa`/`code-reviewer`:**
   - before verification, only the platform tenant lookup and read-only tenant-scoped config/credential reads run: no ledger, intent, wallet or projection reads, no locks, no writes, no audit rows;
   - credential lookup is scoped to the route tenant, with no cross-tenant key attempts;
   - after verification, one tenant id is used for RLS, the signature input and the credential, and any mismatch fails closed.
7. **I4:** `status='disabled'` capability rows gate routing only, and callbacks for them are still accepted (payments co-signed). Cutting off callbacks is done by revoking the credential.
8. **C5:** a key-material rejection before verification still raises the ADR 0022 §4.1 security alert (allow-listed fields).
9. **Tests (QA plan, paper 16):**
   - pre-fix evidence `TestPayWH_S6_CrossTenant_PreFix_AttackSucceeds`, run against the unfixed code and saved under `evidence/`, then retired;
   - the six-point "no financial effect" checklist;
   - tamper tests corrupt the signature headers (payments change 1);
   - existing 404/400 webhook assertions re-verified individually, not by blind replace;
   - an OpenAPI contract test for the payments webhook entry.
10. **C4:** the tenant-binding tests are written as ADR 0022 §6 conformance tests, so every future real adapter must pass them; T5 stays mock-only.
11. **Registered, not built:**
    - **PAYWH-BRAND-1:** no brand scoping in `ProviderAcceptsWebhook`.
    - **PAYWH-RL-1:** webhook rate limiting.
    - **PAYWH-TS-1:** signed-timestamp replay window.
    - **KYC-WH-1** (High) and **CAS-WH-TENANT-1** (Medium): registered in the task registry. KYC-WH-1 is raised to the human for a scope ruling. The shared callback contract is recorded once, in the ADR 0022 amendment, with no shared Go package in 10.1.
12. **Doc updates (C7):**
    - `07-payments-architecture.md`;
    - `payment-orchestration.md` §5 and §10;
    - stale comments in `deposit_handlers.go` and `orchestrator.go`;
    - the payments webhook entry in OpenAPI (API-DOC-PAYWH).

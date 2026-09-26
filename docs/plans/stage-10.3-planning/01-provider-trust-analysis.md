# Stage 10.3 planning: provider-trust analysis (DESIGN ANALYSIS ONLY)

- **Author:** `architect`
- **Date:** 2026-09-26
- **Repository state:** HEAD `957a3e8`
- **Scope of this paper:**
  - No code is changed, nothing under `deploy/` is touched, and no AWS or Terraform command is run. An AWS staging teardown is running in parallel.
  - Nothing is committed.
  - This paper does not authorize a stage. Stage 10.3 scope still needs the human's authorization through the orchestrator (CLAUDE.md stage-gate rule).
- **Labels:** every deliverable below is `NOT IMPLEMENTED` today unless stated otherwise.
- **Recommendations:**
  - Items marked `RECOMMENDATION` are the architect's engineering proposals.
  - They are not Blueprint requirements.
  - They become binding only once they are recorded as ADR amendments.

## 0. Verified current state (read from code, not from prior reports)

| Fact | Evidence |
|---|---|
| **One shared wire scheme.** Every inbound callback's header format is the platform MOCK `webhookauth.Scheme`: fixed header names, `v1=<64 lowercase hex>`, and key ids matching `^[a-z0-9-]{1,32}$`. It is enforced twice: once in the shared HTTP preamble, before the tenant lookup, and again in each orchestrator before resolution. | `internal/webhookauth/webhookauth.go` (`ParseHeaders`, `CheckPreamble`); `internal/httpserver/webhook_preamble.go:56`; `internal/payments/orchestrator.go:864`; `internal/kyc/provider.go:243`; `internal/casino/orchestrator.go:610` |
| **Verification happens only inside the adapter.** Each adapter verifies inside its own `HandleCallback`. The orchestrator resolves the credential but never verifies anything itself. | the three `ReceiveCallback`s, steps (c)/(d) |
| **The resolver takes no transaction.** The interface is `Resolve(ctx, tenantID, providerID, keyID)`. It receives no DB handle and no key-id-less mode. | `webhookauth.go` `Resolver` |
| **Only MOCK resolvers exist.** Every credential is derived from a per-process random master. They are wired only when `TestSupportRoutesEnabled()` is true; otherwise the resolver is nil and every callback gets 401 `no_resolver`. | `internal/webhookauth/mock.go`; `cmd/platform-api/wiring.go` |
| **Mock adapters are always registered.** The following are registered in every environment, production included: <br>- `mock-payments` and `mock-casino`; <br>- the sportsbook mock, whose catalogue is synced at startup; <br>- `MockSettlementStatementSource`, `MockPersonResolver`, `MockDocumentStorageProvider`, `MockMalwareScanner` and the email mock. <br>Only the KYC mock is gated. | `cmd/platform-api/main.go:150, 190, 222, 289, 308-309, 314, 339` |
| **Outbound calls carry no tenant.** The canonical outbound requests (`DepositRequest`, `WithdrawRequest`, `QueryStatus(ctx, ref)`, casino `LaunchRequest`, `Catalogue(ctx)`) carry neither a tenant nor a credential. <br>The outbound HTTP client authenticates with one static header value per client instance. That value is sourced from a process-global env var (`ProviderConfig.APIKeyEnvVar`). | `internal/payments/types.go:426-487`; `internal/casino/types.go:496, 645-652`; `internal/providers/httpclient/client.go:205-206`; `internal/providers/config.go:114-120`; ADR 0080 Decision 4 |
| **No credential or secret table exists.** Migrations end at `0093`. | `migrations/` |
| **Secrets reach the process only at task start.** The platform uses AWS Secrets Manager only through ECS `secrets`/`valueFrom` env injection at task start, by the execution role. The ECS task role is deliberately empty: "platform-api … make[s] no AWS API calls today". There is no AWS SDK in `go.mod`. | `deploy/aws/modules/secrets/main.tf`; `deploy/aws/modules/ecs/main.tf:131-133`; `deploy/aws/modules/iam/main.tf:113-117, 138-145` (read only) |
| **Conformance cases still skip for non-mock adapters.** Only the tenant-binding case fails for a non-mock adapter. The decline/ambiguous, duplicate-callback, credential and failure-fixture cases still `t.Skip`, so a real adapter would pass ADR 0022 §6 (a)/(b) by skipping. | `internal/payments/conformance_test.go:85, 114, 221`; `internal/casino/conformance_test.go:164, 187, 270, 292` |
| **The player KYC create path hard-codes the mock provider.** It calls `Provider("mock")`. | `internal/httpserver/kyc_handlers.go:131` |

## 1. WH-VENDOR-SCHEME-1: provider-specific verification contracts

**Why it exists.**
- Stage 10.2 final review L8/K12 (`11-review-code.md`; design §K12) found the problem.
- It is recorded in ADR 0022 §3 (Stage 10.2 amendment, "Known constraint on the first real adapter").
- It is registered as `WH-VENDOR-SCHEME-1`.
- ADR 0022 §3 point 8 says `Scheme` is *not* a vendor wire format. Even so, the preamble and all three orchestrators require it before the adapter runs. A real vendor's headers are rejected with 401 before its own verifier is reached.

**Does it block real-provider integration?** **Yes.** It blocks the first real adapter in every domain (payments, KYC, casino).

**Dependencies.** None upstream. The real resolver (§2) and the conformance suite depend on it.

### 1.1 Design (`RECOMMENDATION`, architect-owned)

Header parsing and verification move from a shared, hard-coded step into a **per-adapter `VerificationScheme` capability**. The platform keeps ownership of *ordering, tenant resolution, credential selection and the uniform failure response*. That is ADR 0022 §3 points 1–9, unchanged in substance.

```go
// package webhookauth (provider-neutral; imports no domain package)

// AuthMaterial is what a scheme extracted and format-checked from the
// request. It is NOT verified. KeyID selects the credential; the rest is
// scheme-private (signature bytes, signed timestamp, signed account id).
type AuthMaterial struct {
    KeyID   string // "" only for schemes declaring KeySelection == KeyImplicit
    private any    // opaque to the platform, consumed only by Verify
}

type VerificationScheme interface {
    // Name is a stable, loggable identifier, e.g. "platform-mock-payments-v1".
    Name() string
    // Extract parses and format-validates the auth headers only. It is pure:
    // no DB, no secret, no body parsing, no clock. On failure it returns
    // ReasonSignatureMissing or ReasonSignatureInvalid, never any other reason.
    Extract(in Inbound) (AuthMaterial, Reason, bool)
    // Verify checks in against creds over the RAW body, in constant time.
    // Conditions: cred.TenantID/ProviderID == in.TenantID/ProviderID; the
    // signed account id (if any) == cred.BoundAccountID; the signed timestamp
    // is within Properties().MaxSkew of now. The only error is ErrSignatureInvalid.
    Verify(creds CredentialSet, in Inbound, m AuthMaterial, now time.Time) error
    // Properties declares what the conformance suite must prove.
    Properties() SchemeProperties
}

type SchemeProperties struct {
    Binding         TenantBinding // SignedTenant | PerMerchantKey | PerMerchantKeySignedAccount
    KeySelection    KeySelection  // KeyFromHeader | KeyImplicit (vendor sends no key id)
    SignedTimestamp bool
    MaxSkew         time.Duration // required > 0 when SignedTimestamp
}

// CredentialSet is the single (tenant, provider) credential, plus at most
// one overlap predecessor for KeyImplicit schemes during a bounded rotation
// window (see the point-2 clarification below).
type CredentialSet struct{ Active Credential; Previous *Credential }
```

**How it is used.**
- **Adapters.** Each domain's provider interface gains `WebhookScheme() webhookauth.VerificationScheme`.
- **Mock schemes.** The existing MOCK `Scheme` implements `VerificationScheme` through an adapter method set:
  - `Binding = SignedTenant`, `KeySelection = KeyFromHeader`, `SignedTimestamp = false`, disclosed below.
  - Its bytes, prefixes, headers and labels are **unchanged**. `TestPaymentsParameters_ByteIdentical` stays the guard.
- **Preamble.** `webhookRoute.scheme` becomes `schemeFor func(providerID) (VerificationScheme, bool)`, supplied by the domain orchestrator from its process-global adapter registry. No tenant input is involved.
- **Preamble order becomes:**
  1. provider-id charset;
  2. bounded body read;
  3. scheme lookup (`provider_unregistered`);
  4. `scheme.Extract`;
  5. platform `GetTenantBySlug`;
  6. tenant active check.

  `provider_unregistered` therefore moves *before* the tenant lookup. The response is still the identical 401, and only the log reason ordering changes. It also narrows F-4's timing residual for unknown providers.
- **Orchestrator verification (the key change).** Each `ReceiveCallback` calls `scheme.Verify` **itself**, after resolution and before `HandleCallback`.
  - Today verify-before-parse and verify-before-state depend on every adapter remembering to verify first. After this change the platform enforces them structurally.
  - `HandleCallback` keeps its signature and still receives the credential, so re-verifying is permitted as defence in depth. It then only parses verified bytes.
  - Point 7's error contract is unchanged: pre-verification failures are the auth sentinels, and verified-but-malformed input is `ErrCallbackMalformedBody` → 400.
- **Key-id charset.** The charset stays a *platform* constraint on what may be logged. The scheme may map a vendor key id into the platform key-id space. The DB key-id charset is widened in §2 (bounded length, printable, no separators); the logging rule "log key id only after the charset check" is unchanged.
- **Out of scope until a vendor exists (`PROVIDER DEPENDENT`, deliberately not designed now):**
  - URL- or method-signing schemes. `Inbound` is a struct and can gain `Method`/`Path`/`RawQuery` additively.
  - Vendor-specific acknowledgement or error-body rendering, such as seamless-wallet casino responses.
  - Inbound mTLS and vendor IP allow-lists.

  None of these is invented here.

**ADR 0022 §3 amendments this requires (architect records; `security` concurrence required because points 2 and 7 are security requirements):**
- **Point 2 clarification.**
  - "Exactly one credential" means exactly one (tenant, provider) credential binding.
  - For `KeyImplicit` vendors only, the active key and at most one `verify_only` predecessor *of the same tenant and provider* may be tried, inside a bounded `not_after` window.
  - A trial across tenants or across providers stays forbidden.
  - If `security` does not concur, `KeyImplicit` vendors need a hard cut-over rotation instead. Both options are recorded here; the decision belongs to `security`, not the human.
- **Point 7/8 addition.** The orchestrator, not the adapter, is the mandatory verifier.
- **New point 10.** Every non-mock scheme must declare `SignedTimestamp = true` with `MaxSkew > 0`. This absorbs PAYWH-TS-1; see §4. A vendor without signed timestamps is not integrable without a further ADR, mirroring point 3's existing "neither" clause.

### 1.2 Conformance suite (the gate every real adapter must pass)

The suite is the non-`_test` package `internal/webhookauth/webhookauthtest`, entry point `RunSchemeConformance(t, Fixture)`.

**What the fixture supplies:**
- the scheme;
- a `Sign(creds, in, now)` function built from the vendor's documented algorithm or recorded sandbox fixtures (never invented);
- a per-tenant credential generator;
- a clock.

| Case | Assertion |
|---|---|
| SC1 | A genuine signature verifies. |
| SC2 | Any single-byte change to the body, a signature header or the timestamp is rejected with exactly `ErrSignatureInvalid`. |
| SC3 | **Tenant binding.** A credential for A never verifies a request routed as B, including under an equal key when `Binding = SignedTenant`. For `PerMerchantKey*`, A-signed traffic at B's route is rejected. |
| SC4 | Provider binding: the same checks across two provider ids. |
| SC5 | `Extract` rejects missing headers with `signature_missing` and malformed ones with `signature_invalid`, and is fuzzed for panics. |
| SC6 | A key-id mismatch between the header and the credential is rejected. |
| SC7 | **Replay window.** A stale signature (now − skew − 1s) and a future one (now + skew + 1s) are rejected; one inside the window is accepted. **Mandatory, fail-not-skip, for every non-mock scheme.** |
| SC8 | A signed account id that differs from `cred.BoundAccountID` is rejected (required when `Binding = PerMerchantKeySignedAccount`). |
| SC9 | Rotation overlap: `Previous` is accepted before `not_after` and rejected after it. `KeyImplicit` only. |
| SC10 | An empty or short secret, or an empty `CredentialSet`, is rejected. It never panics. |
| SC11 | The error value is `== ErrSignatureInvalid`, and its text contains no body bytes, secret or fingerprint. |
| SC12 | Duplicate auth headers behave deterministically (F-6/K13). |
| SC13 | MOCK schemes only: the cross-domain check (point 8, equal key, different domain, rejected). |

**Rules for the suite itself:**
- **Self-test.** The suite is run against deliberately broken reference schemes (ignores the tenant, ignores the timestamp, non-constant compare replaced by one that accepts a prefix). Each must go red. This is required by ruling J1 ("every guard … shown to go red").
- **SC7 for the mock.** The MOCK is exempt from SC7 only because its bytes are frozen and replay is inert by idempotency (§4). The SC7 path is proven with a test-only timestamped reference scheme in the harness's own tests, never in production code.
- **Existing skips become failures.** The domain conformance suites gain a `CallbackFixture` hook that uses the same `Sign`. The skips listed in §0 (payments 85/114/221, casino 164/187/270/292) become **fail-not-skip for non-mock adapters**, exactly as K3 already did for tenant binding. Without this, ADR 0022 §6's "passes identically" is not enforced.

### 1.3 Impact matrix

| Aspect | Impact |
|---|---|
| DB / migrations | None. |
| API / OpenAPI | The webhook paths' header parameters are re-described as "provider-defined; the platform MOCK scheme uses `X-{Domain}-Signature`/`X-{Domain}-Key-Id`". Status codes and uniform-401 semantics are unchanged. `TestOpenAPI_PaymentsWebhook_ContractMatchesHandler` and the casino/KYC contract tests need small updates. |
| Security | Positive: verify-first is enforced by the platform, not by each adapter. New surface: vendor parsers in `Extract`, which must be pure and fuzzed (SC5). `security` review is mandatory before completion (CLAUDE.md). |
| Financial | None on ledger mechanics. The invariant is I1: no posting or tombstone before `Verify` succeeds. The existing no-effect tests (C1/E4, T-series) must stay green unedited. |
| RLS | None. The strict I1 of point 9 is preserved; see §2 for the one handle-read allowance. |
| Tests | SC1–SC13 plus the self-test; payments byte-identity; the reason-order change covered by the existing allow-list log tests (K12/SC-3 style); the three domains' fixture-hook conversions; statement-capture tests (K7/C7) unchanged. |
| Rollback | Code-only. Revert the commits. The MOCK bytes are unchanged, so no in-flight callback or staging flow is affected. |
| Ownership | `architect` (contract, ADR text); `backend` (preamble, orchestrators); `payments`/`casino`/`identity-compliance` (their adapters and fixture hooks); `security` (review, concurrence on point 2/10); `qa` (suite and mutation proofs); `code-reviewer`. |
| Human decision? | **None.** This is pure engineering inside an already-recorded constraint. Choosing a vendor is not needed for it. |

## 2. Real signing-key and credential resolution

**Why it exists.**
- ADR 0022 §2.2 requires handle-not-material, FORCE RLS, CDC exclusion, audit of handle plus fingerprint, and per-tenant rotation with overlap.
- ADR 0022 §3 amendments (10.1 status: "real resolver … NOT IMPLEMENTED … blocked on the secret-store ADR"; 10.2 status: KYC and casino blocked the same way).
- `payment-orchestration.md` §10.
- ADR 0025's consequences ("per-tenant provider signing keys … precondition for any real provider").
- Registry rows PAY-WH-TENANT-1 ("launch-blocking for any real PSP"), KYC-WH-1 and CAS-WH-TENANT-1.

**Does it block real-provider integration?** **Yes**, for all three domains. The nil resolver fails closed, so no real callback can verify.

**Dependencies.**
- WH-VENDOR-SCHEME-1 (`CredentialSet`, `BoundAccountID`).
- A small ADR ("provider credential model and secret store"). This is the "secret-store ADR" the 10.1 amendment names.
- AWS Secrets Manager is already the platform's accepted secret store (ADR 0084/0086; `docs/security/security-architecture.md` "Vault or a cloud KMS"). Recording it for provider credentials is therefore an engineering decision by `architect` and `security`, **not** a human decision. What stays human is provisioning real credentials and any AWS action.

### 2.1 Handle table (`RECOMMENDATION`; migration `0094`, additive)

`provider_credential_handles`, tenant-owned configuration. It holds **no secret material**.

| Column | Notes |
|---|---|
| `id uuid PK` | |
| `tenant_id uuid NOT NULL REFERENCES tenants(id)` | A direct FK, unlike `provider_capabilities`' known gap. |
| `domain text NOT NULL CHECK (domain IN ('payments','kyc','casino'))` | Provider ids are not guaranteed globally unique across domains (the KYC mock id is `mock`). The domain enum widens additively (e.g. sportsbook). |
| `provider_id text NOT NULL` | Same charset as `ValidProviderID`. |
| `purpose text NOT NULL CHECK (purpose IN ('webhook_verify','outbound_api'))` | Least privilege: inbound and outbound never share a handle (ADR 0022 §4.2). |
| `key_id text NOT NULL CHECK (key_id ~ '^[A-Za-z0-9._-]{1,64}$')` | A vendor key id or a platform label (for `KeyImplicit` schemes). |
| `secret_ref text NOT NULL CHECK (length(secret_ref) <= 512 AND secret_ref ~ '^(awssm|devfile|memory)://')` | A backend-prefixed reference that **pins a version** (e.g. `awssm://<name>?versionId=<id>#<jsonKey>`). |
| `fingerprint text NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{16}$')` | `webhookauth.Fingerprint`, checked on every fetch. |
| `vendor_account_id text NULL` | The account or merchant id the vendor signs (point 3). Feeds `Credential.BoundAccountID`. |
| `status text NOT NULL CHECK (status IN ('active','verify_only','revoked'))` | |
| `not_before timestamptz NOT NULL DEFAULT now()`, `not_after timestamptz NULL` | `CHECK (status <> 'verify_only' OR not_after IS NOT NULL)`. `CHECK (purpose = 'webhook_verify' OR status <> 'verify_only')`. |
| `created_at/created_by`, `revoked_at/revoked_by/revoke_reason` | Audit carriers. The `audit_log` row itself carries handle plus fingerprint only. |

**Constraints:**
- `UNIQUE (tenant_id, domain, provider_id, purpose, key_id)`.
- Partial `UNIQUE (tenant_id, domain, provider_id, purpose) WHERE status = 'active'`, so there is exactly one active key.
- A trigger allows only forward transitions (`active→verify_only→revoked`, `active→revoked`), allows UPDATE of the status/revocation columns only, and forbids DELETE. History is evidence.

**RLS:**
- `ENABLE` + `FORCE ROW LEVEL SECURITY`, with an `app.tenant_id` policy.
- **No** dual-scope or platform policy (ADR 0022 §2.2: a `WithoutTenant` connection must not become a cross-tenant read path).
- Runtime-role grants are `SELECT, INSERT, UPDATE(status, not_after, revoked_*)`, with no DELETE.
- The table is named in ADR 0019's enumerated table list and excluded from CDC.
- A schema test asserts that no column name matches `secret|key_material|private|password` other than `secret_ref`.

**Deliberately omitted:**
- `brand_id`: see PAYWH-BRAND-1, §4. A nullable column plus the composite FK is an additive later migration.
- An mTLS purpose: `PROVIDER DEPENDENT`.

### 2.2 Resolver, store, cache

**Resolver.**
- Signature change: `Resolve(ctx, tx pgx.Tx, tenantID, providerID, keyID string) (CredentialSet, error)`, with the domain fixed at construction.
- The handle read runs **in the caller's tenant-scoped transaction**. There is one tenant id, no second pool connection and no pool-starvation deadlock under load, which a resolver holding its own `*db.Pool` would risk while the outer transaction holds a connection.
- Mocks ignore `tx`.
- This is a mechanical change across the 3 orchestrators and about 16 call sites plus tests.

**Point 9 conflict (must be amended; architect decision, `security` concurrence).**
- Point 9 forbids *any* tenant-scoped statement before KYC or casino verification. A DB-backed handle lookup is exactly such a statement.
- Point 4 already permits "credential-handle lookups" pre-verification, so the two points disagree for a real resolver.
- **Amendment:** before verification, KYC and casino may run exactly one additional statement: the resolver's read-only, explicitly tenant-predicated `SELECT` on `provider_credential_handles`, and nothing else.
- The K7/C7 statement-capture tests are updated to allow precisely that statement shape. Any other pre-verification statement must still fail them.

**Lookup:**

```sql
SELECT ... FROM provider_credential_handles
 WHERE tenant_id = $1 AND domain = $2 AND provider_id = $3
   AND purpose = 'webhook_verify'
   AND (key_id = $4 OR ($4 = '' AND status IN ('active','verify_only')))
   AND status IN ('active','verify_only')
   AND not_before <= now()
   AND (not_after IS NULL OR not_after > now())
```

The explicit predicate is in addition to RLS (ruling R4 precedent).
- At most 1 row for `KeyFromHeader`.
- At most 2 rows (active plus one predecessor) for `KeyImplicit`.
- Any other count fails closed.

**Secret store.** `secretstore.Store` has `Get(ctx, ref) ([]byte, error)`. Backends:
- `memory://` for tests;
- `devfile://` for local development only: it reads from a git-ignored directory and is refused unless `APP_ENV=development`;
- `awssm://` using `GetSecretValue` with a pinned `VersionId`, through `aws-sdk-go-v2/service/secretsmanager`. This is a new dependency and needs `security` dependency review.

The allowed backends per environment form a closed set: production allows `awssm` only. Both registration and the resolver enforce it.

**Cache.**
- The in-process cache holds material keyed by `(secret_ref)`. The ref pins an immutable version, so cached material can never be "stale". Revocation is enforced by the **per-request handle read**, not by cache expiry, so it is immediate.
- The cache is a bounded LRU with a TTL (for example 15 min, for memory hygiene) and `singleflight` per ref.
- It serves the cached value when the store errors, up to a max-stale bound. This is safe because the content is immutable.
- Every store call has a 2 s context timeout.
- Only refs that exist as handle rows ever reach the store. An unauthenticated caller cycling key ids costs one indexed DB miss, **never** a Secrets Manager call. This closes the cost- and throttle-amplification vector a naive resolver would open (relevant to PAYWH-RL-1).

**Integrity.** If the fetched material's fingerprint differs from the row's fingerprint, the request fails closed, raises a P1 security alert and does not serve the material. This catches wrong-ref wiring and store tampering.

**Rotation (inbound):**
1. Provision a new secret version out of band.
2. Register the handle `k2 active`, which moves `k1` to `verify_only` with `not_after = now + vendor overlap`.
3. The vendor switches to `k2`.
4. `k1` stops verifying at `not_after` through the predicate, with no job needed, and is later marked `revoked`.

The mock's `mock-v1` doc comment already anticipates coexisting ids.

**Rotation (outbound).** Register a new active handle; the old one is revoked atomically in the same transaction.

**Revocation disclosure (`ledger-finance` to confirm).** Revoking a casino credential mid-round rejects the verified wins and rollbacks of open rounds. That is the same stranded-stake class as CAS-CAP-ROLLBACK-1. The runbook must say: revoke only on compromise, and rotate with overlap otherwise.

**Fail-closed matrix.** The HTTP response is always the uniform 401; only the log reason differs.

| Condition | Reason |
|---|---|
| no resolver | `no_resolver` (existing) |
| no or expired handle | `credential_unavailable` (existing) |
| store error with no cached value | **new** `credential_store_unavailable`, plus an alarm |
| fingerprint mismatch | **new** `credential_integrity`, P1 |
| disallowed backend | `credential_unavailable` |

- A 503 was rejected: it would tell an unauthenticated caller that a handle exists for that key id, which is an oracle.
- **Financial consequence:** a vendor that does not retry on 4xx loses callbacks during a store outage. The backstop is `QueryStatus` polling and daily reconciliation (ADR 0022 §5.6). `ledger-finance` should confirm that this is acceptable and that reconciliation covers it.

**Wiring.**
- The real resolver is wired in every environment.
- The MOCK resolver is wired only under `TestSupportRoutesEnabled()`, as today.
- Composition is a **two-way split selected by the registered adapter's kind**: synthetic adapters go to the mock resolver, all others to the real resolver (see §3). This is not a per-vendor map, so it is consistent with PW-6.
- The mock providers need no handle rows, so staging flows are unchanged.

**Admin API (new).**
- Routes:
  - `GET /v1/admin/providers/{domain}/{providerID}/credentials`, which returns handles and fingerprints only, never material or refs' secrets;
  - `POST .../credentials` to register;
  - `POST .../credentials/{keyID}/transition` to move to `verify_only` or `revoked`.
- Tenant comes from the staff JWT.
- A new permission is required.
- Every write produces an `audit_log` row with handle, fingerprint and reason code.
- Registration fetches the ref once and rejects the request if the fingerprint does not match what the operator supplied, so the material is never trusted on first use.
- **Deferred:** partner-console *self-service writing of secret material* into the store (task-role `PutSecretValue`). This needs product and `security` scoping. For now provisioning is out-of-band by an operator.
- Four-eyes approval on credential changes is `security`'s call and a recommended condition for production.

**Outbound half (see §5 O1).** The same table, `purpose = 'outbound_api'`, and the same store. The orchestrator resolves the credential and passes it to the adapter call. The adapter never resolves.

### 2.3 Local versus staging

| Can be built and fully tested locally (synthetic PostgreSQL, NOBYPASSRLS runtime role) | STAGING REQUIRED |
|---|---|
| Migration 0094, RLS isolation (A cannot read or write B's handles, `WithoutTenant` sees zero), transition trigger, grants, schema no-material test | The task-role IAM policy: `secretsmanager:GetSecretValue` scoped to `<prefix>/provider-creds/*` and pinned to the environment's account and region, plus `kms:Decrypt` if a customer-managed key is used. This is a `deploy/` change and needs a human-authorized apply. |
| Resolver over the `memory://` and `devfile://` backends: exact-one, overlap, expiry, revocation-immediacy, fail-closed for every row of the matrix above, fingerprint integrity | Network path to Secrets Manager: staging public-IP mode, NAT, or a VPC interface endpoint (recommended for production). |
| `awssm://` backend against an **interface fake** of the SDK client: pinned `VersionId`, JSON-key extraction, timeout, error classification | Real `GetSecretValue` latency and throttling at 2 replicas; cache warm-up and cold-start behaviour. |
| Cache: TTL, LRU bound, `singleflight` (concurrency test), serve-cached-on-error, no store call for an unknown key id | An end-to-end rotation drill and a store-outage drill with a **synthetic** secret (never a vendor credential), plus the CloudWatch alarm on `credential_store_unavailable` and `credential_integrity`. |
| Point 9 amended statement capture; the three domains' end-to-end webhook tests using real-resolver wiring with `memory://` | Confirming no secret value appears in container logs, ECS task metadata or Terraform state (ADR 0086 write-only pattern). |
| Admin API, OpenAPI, audit rows (handle and fingerprint only), permission checks | |

LocalStack is optional and is **not** recommended as a new dev dependency; the SDK-interface fake is sufficient locally.

### 2.4 Impact matrix

| Aspect | Impact |
|---|---|
| DB | Migration `0094` (plus its down migration), additive. |
| API | New admin endpoints and OpenAPI. Webhook responses are unchanged. |
| Security | This is the first runtime AWS API call from the platform: task-role privilege and a new dependency. A single task role can read all tenants' provider secrets, so tenant isolation of *material* rests on RLS over handles plus fingerprint integrity, not on IAM. Per-tenant IAM/ABAC or a per-tenant KMS key for bring-your-own-licence tenants (ADR 0006) is a later tightening path, consistent with CLAUDE.md's isolation ladder. For low-entropy vendor secrets, an unkeyed truncated SHA-256 fingerprint could aid offline guessing; `security` should decide whether to key it (HMAC with a platform pepper). `security` review is mandatory. |
| Financial | No ledger change. The fail-closed behaviour defers callbacks, and reconciliation is the backstop; `ledger-finance` confirms. Revocation strands open casino rounds (disclosed above). |
| RLS | New FORCE-RLS table, no platform policy. The point 9 amendment allows exactly one pre-verification tenant-scoped read. |
| Tests | Listed in §2.3, plus a mutation proof for each guard: remove the tenant predicate, remove the fingerprint check, remove the `not_after` predicate, give the table a `WithoutTenant` policy. Each must go red. |
| Rollback | The resolver is wired behind a config switch. Switching it off gives a nil real resolver, so every real-provider callback returns 401 — the safe direction. The down migration drops the table; it holds handles only, and secrets in the store are untouched. The IAM policy revert is a human-authorized `deploy/` change. |
| Ownership | `architect` (model, ADRs); `security` (store, IAM, fingerprint, dependency, four-eyes); `backend` (resolver, store, cache, admin API); `payments`/`casino`/`identity-compliance` (wiring); `devops` (deploy module, alarms); `qa`; `ledger-finance` (fail-closed and revocation concurrence). |
| Human decision? | **Only these:** (a) authorizing the `deploy/` IAM/KMS/network change and its staging apply (an AWS action); (b) later, provisioning any *real* vendor or production credential. The table, resolver, cache, backends, amendments and dependency are engineering decisions. |

## 3. MOCK-ADAPTER-PROD-1: production fail-closed protection against synthetic adapters

**Why it exists.**
- Stage 10.2 architect review §3 and ruling J16.
- Registry row `MOCK-ADAPTER-PROD-1`.
- Completion report §"risks".

The inventory is wider than the registry row: **eight** synthetic components are registered unconditionally (the KYC mock is the only gated one) in `main.go` (see §0). Two of them go beyond the row's text:
- the sportsbook mock catalogue is *written to the database at startup* in every environment;
- `MockMalwareScanner` would accept unscanned KYC documents.

**Does it block real-provider integration?** **No** for sandbox or staging integration of a first adapter. **Yes** for production launch: it is a pre-launch gate.

**Dependencies.** None. It can run in parallel with §1. It must land before any production deployment.

**Design (`RECOMMENDATION`):**
- A leaf package, `internal/providerkind`, declares a marker interface: `Synthetic interface{ SyntheticComponent() }`. Every mock type implements it.
- `cmd/platform-api` gets a pure function `syntheticGuard(cfg, regs []Registration) error`. A `Registration` is `{Domain, Name, Component any}`. The function returns a named error listing every synthetic component when `cfg.Environment == "production"`.
- `run()` calls the guard **immediately after `config.Load()`**, before tracing, before `db.Connect` and before `SyncCatalogue`, then exits 1. This is refusal to start, not silent omission. Silent omission would leave a production binary serving a half-configured product with no signal.
- The platform already refuses unsafe production configuration loudly: `config.Load` rejects production plus test support, and `VerifyRuntimeRoleInProduction` exists.
- **ADR 0085 §1 amendment.** It names this as a new, reviewed `Environment` exception, closed set, alongside `VerifyRuntimeRoleInProduction` and `TestSupportRoutesEnabled`.
- **Consequence (disclosed).** A production binary cannot start until every registered domain has a real implementation *or* is explicitly not registered. No environment runs `APP_ENV=production` today, so nothing breaks.

**Tests:**
1. A guard matrix over `{production, staging, development}` × `{synthetic, non-synthetic test double}`.
2. A **completeness test** that scans `internal/**` with `go/ast` for `type Mock\w+` implementing any provider, scanner, storage, resolver or statement-source interface, and requires `SyntheticComponent()`. This proves no future mock escapes the guard, and a mock with the marker deleted must go red.
3. A registration-completeness test: `buildRegistrations(cfg)` enumerates everything `main` wires, and a deliberately added unregistered mock fails the test.
4. An ordering test: running the binary as a subprocess with `APP_ENV=production` and an unreachable `DATABASE_URL` fails with the guard's error, not a DB error. This proves the guard runs before any side effect.

| Aspect | Impact |
|---|---|
| DB / API / RLS | None. |
| Security | Positive. It removes production reachability of self-signed and synthetic money, compliance and scanning paths. |
| Financial | Positive. No mock payments, casino or sportsbook adapter or statement source can exist in a production process. |
| Rollback | Revert. It only affects `APP_ENV=production`. |
| Ownership | `architect` (rule, ADR 0085 amendment); `devops`/`backend` (implementation); `payments`, `casino`, `sportsbook`, `identity-compliance` (marker on their mocks); `security` review; `qa`. |
| Human decision? | **None** to build it. At production launch, *which verticals launch* when no real provider exists is a product and commercial human decision. The guard only makes that decision explicit instead of letting a mock launch by default. |

## 4. Re-assessment of PAYWH-BRAND-1, PAYWH-RL-1 and PAYWH-TS-1 against real-provider readiness

| Item | Source | Blocks the first real adapter? | Assessment and recommendation |
|---|---|---|---|
| **PAYWH-TS-1** (signed-timestamp replay window) | Registry 10.1 row; ADR 0022 §3 point 3; design §E; architect review §4; security F-5 | **No, as a separate platform item.** **Yes, as a per-adapter obligation**, already mandatory under point 3. | **Replay has no financial effect today:** <br>- payments: `(provider_id, provider_tx_id)` idempotency; <br>- casino: idempotency, F-7 409 and tombstones; <br>- KYC: forward-only rank. <br>**The one residual:** a replayed non-terminal KYC `error` callback appends an audit row on every delivery (F-5). A bounded window caps it. <br>**Why no shared window:** a platform-level nonce store would need vendor timestamps the platform cannot parse generically. <br>**Recommendation:** close TS-1 as **superseded by WH-VENDOR-SCHEME-1 SC7 and new point 10**, since vendor tolerance lives in the scheme. The orchestrator records the registry change. No DB, API or RLS impact. Owners: `security` and `architect`. Human: none. |
| **PAYWH-BRAND-1** (webhook capability check has no brand scope) | Registry 10.1 row; design §E; ADR 0022 §3 (credentials per `(tenant_id, brand_id)`) | **No.** | A callback route carries a tenant, not a brand. The brand is derived post-verification from the platform's own `deposit_intents` row. <br>**When it becomes real:** when one tenant holds *separate merchant accounts per brand at the same provider*. Then a per-brand credential needs a post-verification check `intent.brand_id == cred.BrandID`. That check is additive: a nullable `brand_id` plus the composite FK `(brand_id, tenant_id) → brands` on the handle table, and one check in `receiveDepositCallback`. <br>**Recommendation:** stay deferred, with the **trigger condition recorded**: the first tenant with more than one brand holding separate accounts at one provider. Owners: `payments` and `security`. Human: none. The commercial setup that triggers it is a human decision, but the fix is not. |
| **PAYWH-RL-1** (webhook rate limiting) | Registry 10.1 row; design §E; security F-4 | **No** for integration. **Yes** for production launch (availability). | With the §2 design, pre-verification cost stays at a platform lookup, one indexed handle read and at most one cached HMAC. There is no amplification to the secret store (cost and throttle), so the design constraint that made RL-1 matter for a naive resolver is closed by design. <br>**Per-IP limits are the wrong tool here:** vendors send from few IPs, and throttling them drops legitimate callbacks. <br>**Recommendation (pre-launch wave):** an edge rate-based rule on `/v1/webhooks/*`, plus vendor IP allow-lists where vendors publish ranges (`PROVIDER DEPENDENT`), plus a generous app-level per-(route, IP) cap returning 429. Document 429 in OpenAPI. Revisit F-4 timing there. <br>Owners: `security`, `devops` and `backend`. Human: only the authorization for any `deploy/` or edge change. |

## 5. Other provider-readiness blockers found in the repository

Only items evidenced in code or ADRs are listed. Nothing here is invented.

| ID | Finding | Blocks first real adapter? | Owner / notes |
|---|---|---|---|
| **O1 PROV-OUTBOUND-CRED-1** (new, proposed) | The canonical outbound calls carry no tenant id or credential (`DepositRequest`, `WithdrawRequest`, `QueryStatus(ctx, ref)`, casino `Launch`/`Catalogue`). The HTTP client authenticates with one static header per instance, sourced from a process-global env var. A shared adapter therefore cannot use per-tenant credentials, contradicting ADR 0022 §3 ("independent credentials per tenant") and §5 item 3. Rotation would need a redeploy. **Outbound request signing** is the same gap: vendors that HMAC-sign outbound requests need a per-call signer, not a static header. | **Yes** (architecturally). A single-tenant env-key shortcut would work mechanically but would violate ADR 0022 and would be debt at the first B2B tenant. | **Design:** <br>- the orchestrator resolves the `purpose = 'outbound_api'` handle (§2) and passes `OutboundCredential` in the request structs; <br>- `QueryStatus` gains a tenant or credential parameter; <br>- the HTTP client's `Request` gains a per-call `Authenticator` hook (static header, or HMAC signer supplied by the adapter), with redaction extended to per-call values. <br>This is a mechanical interface change across payments, casino and KYC plus their mocks. Owners: `architect`, `payments`, `casino`, `identity-compliance`, `backend`. Human: none. |
| **O2** (point 9 vs. real resolver) | Covered in §2.2. Point 9 as written forbids the handle read a real KYC or casino resolver needs. | Yes (for KYC and casino) | ADR 0022 amendment, `architect` with `security` concurrence. |
| **O3** (conformance skips) | Covered in §1.2. §6 (a)/(b) cases still skip for non-mock adapters. | Yes (ADR 0022 §6) | Part of WH-VENDOR-SCHEME-1's fixture hook; owner `qa`. |
| **O4** (KYC provider selection) | `kyc_handlers.go:131` hard-codes `Provider("mock")` for player self-service creation. A real KYC vendor cannot be selected per tenant. | Yes (KYC only) | `identity-compliance`. Select from tenant/jurisdiction configuration (ADR 0028), not code. It is small, but it is a real blocker for KYC. |
| **O5** CAS-CAP-ROLLBACK-1 (existing) | This is a hard pre-condition (ledger-finance) for wiring any real casino resolver. | Yes (casino only) | Already registered; `casino` and `ledger-finance`. Listed so the casino path is not scheduled without it. |
| **O6** KYC-REASON-BOUND-1 (existing) | Unbounded vendor `reason` text. | Yes (KYC only, per its own row) | Analysed separately in `03-kyc-reason-bound-analysis.md`. |
| **O7** (raw payload retention) | ADR 0022 §4.1 point 4 records an `OPEN DECISION`: whether, and how long, raw provider payloads are retained. | No for integration; needed before production disputes and chargebacks. | A retention period is a compliance/legal matter. **Human input may be required** for the regulatory retention period; the technical store design is `security`/`devops`. Flag it; do not build it. |
| **O8** (`PROVIDER DEPENDENT`, not designed) | URL- or method-signed schemes, vendor-specific acknowledgement or error bodies (seamless-wallet casino), inbound mTLS, vendor IP allow-lists. | Only if the chosen vendor requires them | Extension points exist (`Inbound` is extensible; the scheme is per-adapter). Design each when a vendor contract is known. |

## 6. Diagrams

### 6.1 Webhook trust boundary (after §1 and §2)

```
 Vendor ──HTTPS──> Edge (allowlist/WAF rate rule: PAYWH-RL-1, pre-launch)
                     │
                     ▼
 ┌──────────────────── platform-api : shared preamble (NO tenant/DB work) ─────────────────┐
 │ 1 provider_id charset  2 bounded raw-body read  3 scheme := registry[domain][providerID] │
 │ 4 m := scheme.Extract(headers)   (pure; vendor-specific; fuzzed)                          │
 │    any failure ─────────────────────────────────────────────> uniform 401 + allow-list log│
 └───────────────────────────────┬──────────────────────────────────────────────────────────┘
                                 ▼  platform-scoped GetTenantBySlug + active check (else 401)
 ┌──────────── WithTenant(t.ID) tx : RLS app.tenant_id = t.ID ────────────────────────────────┐
 │ 5 in.TenantID,in.ProviderID := route values (single tenant-id source, point 9/R3)           │
 │ 6 [payments only] ProviderAcceptsWebhook (read-only config, point 4)                        │
 │ 7 creds := Resolver.Resolve(tx, t.ID, providerID, m.KeyID)                                  │
 │      └─ ONLY pre-verification tenant-scoped statement allowed for KYC/casino (amended pt 9) │
 │      └─ secret material fetched via cache/store — never logged, only fingerprint            │
 │ 8 scheme.Verify(creds, in, m, now)   ◄── PLATFORM-ENFORCED (new), constant-time, raw bytes,  │
 │      tenant/provider/account binding, timestamp window (SC7)                                 │
 │    any failure in 5–8 ─────────────────────────────────────> uniform 401, no write, no audit │
 │ ══════════════════════ TRUST BOUNDARY: sender authenticated for (t.ID, providerID) ═══════ │
 │ 9 adapter.HandleCallback(in, cred) → parse verified bytes (malformed → 400)                  │
 │10 domain state/ledger reads, locks, writes, audit — all under t.ID and route providerID     │
 └─────────────────────────────────────────────────────────────────────────────────────────────┘
```

### 6.2 Provider credential model

```
             (out-of-band, human-authorized for real creds)
 Operator ──► AWS Secrets Manager  <prefix>/provider-creds/<tenant>/<domain>/<provider>/<key>
                │  immutable versions (VersionId pinned by the handle)
                │  task role: GetSecretValue on <prefix>/provider-creds/* only (STAGING REQUIRED)
                ▼
 Admin API (staff JWT tenant, RBAC, audit: handle+fingerprint only)
   register(key_id, purpose, secret_ref, fingerprint, vendor_account_id, not_before/after)
   └─ fetch once, fingerprint must match, else reject
                ▼
 provider_credential_handles  (tenant-owned, FORCE RLS, no platform policy, CDC-excluded,
   no secret column, forward-only status trigger, no DELETE)
   (tenant, domain, provider, purpose, key_id) → secret_ref@version, fingerprint, status, window
        │ per-request read in caller tx (revocation immediate)
        ▼
 Resolver[domain] ──► Cache (LRU+TTL, singleflight, serve-cached-on-store-error; keyed by
        │              pinned ref ⇒ immutable) ──► Store backend: awssm | devfile(dev) | memory(test)
        │   fingerprint(material) == row.fingerprint, else fail closed (credential_integrity, P1)
        ├──► inbound:  CredentialSet{Active, Previous≤1 (KeyImplicit, within not_after)}
        │              → VerificationScheme.Verify
        └──► outbound: exactly one 'active' outbound_api credential
                       → orchestrator → adapter request → httpclient per-call Authenticator
 Synthetic (mock) adapters: MOCK resolver, per-process random master, only under
 TestSupportRoutesEnabled(); refused entirely when APP_ENV=production (§3).
```

## 7. Implementation waves and dependency graph

```
 W0 (paper, architect+security)   ADR: provider credential model + secret store;
                                  ADR 0022 §3 amendments (pt 2 overlap, orchestrator-verify,
                                  pt 9 handle-read allowance, new pt 10 timestamps);
                                  ADR 0085 §1 (synthetic guard); ADR 0019 table list;
                                  registry: TS-1 superseded, O1 registered, BRAND-1 trigger
        │
        ├──────────────► W1a WH-VENDOR-SCHEME-1 (interface, mock adapter, preamble,
        │                    orchestrator-enforced Verify, webhookauthtest SC1–SC13 +
        │                    self-test, fixture hooks: skip→fail)           [local]
        │
        ├──────────────► W1b MOCK-ADAPTER-PROD-1 (independent)               [local]
        │
        ▼
 W2 (needs W1a)  migration 0094 + RLS tests; Resolver(tx) signature; secretstore memory/devfile;
                 real resolver + cache + fingerprint + rotation + fail-closed; synthetic/real
                 split wiring; admin handle API + OpenAPI + audit; pt-9 capture tests;
                 O1 outbound credential + httpclient Authenticator; O4 KYC provider selection  [local]
        │
        ▼
 W3 (needs W2)   awssm backend + SDK-interface fake [local]  →  deploy/ task-role IAM, KMS,
                 egress, alarms [code local; APPLY = HUMAN-AUTHORIZED]  →  staging drills with
                 synthetic secret: resolve, rotate, outage, integrity  [STAGING REQUIRED]
        │
        ▼
 W4 pre-production-launch (not needed for a first sandbox adapter): PAYWH-RL-1 edge/app limits;
                 O7 retention decision; partner-console self-service writes (if scoped);
                 four-eyes on credential changes; PAYWH-BRAND-1 when its trigger fires
        │
        ▼
 First real adapter per domain — HUMAN: vendor contract + sandbox credentials
   payments: W1a+W2(+W3 for staging)   KYC: + O4, KYC-REASON-BOUND-1   casino: + CAS-CAP-ROLLBACK-1
```

- **Critical path to a first real adapter:** W0 → W1a → W2 → W3.
- **Parallel work:** W1b runs alongside W1a. O1 can start in W1 as interface work if staffing allows.

## 8. Human decisions: strict list

**Required (human, through the orchestrator):**
1. **Stage 10.3 scope authorization.** Which of W0–W3 enter the stage (stage-gate rule).
2. **The AWS action.** Authorizing the W3 `deploy/` change (task-role IAM, KMS, network path) and its staging apply. After this paper's parallel teardown, any staging bring-up is itself human-authorized.
3. **Vendor selection and contracts, sandbox and production credentials.** Needed only for the first real adapter, which is out of these waves.
4. **At production launch:** which verticals launch without a real provider (made explicit by §3), and the regulatory raw-payload retention period (O7).

**Not human. These are engineering decisions, owned as noted:**
- the `VerificationScheme` shape;
- the orchestrator-enforced verification;
- the conformance suite;
- the handle table and RLS;
- choosing AWS Secrets Manager, which is already the accepted platform store;
- cache and TTLs;
- the backends;
- the new reason codes;
- the ADR 0022 point 2, 9 and 10 amendments (`architect` with `security` concurrence);
- the ADR 0085 amendment;
- the SDK dependency (`security` review);
- fingerprint keying and four-eyes on credential changes (`security`);
- revocation and fail-closed callback loss (`ledger-finance` concurrence);
- the TS-1, BRAND-1 and RL-1 dispositions;
- wave ordering.

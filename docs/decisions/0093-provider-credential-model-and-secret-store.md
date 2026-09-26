# ADR 0093 — Provider Credential Model and Secret Store

- **Status:** ACCEPTED 2026-09-26 (Stage 10.3 W0, ADR 0092). Binding for W2a (handles, resolver,
  admin API, outbound credentials) and W3b (the `awssm` backend code).
- **Decision type:** architecture. This is the "secret-store ADR" named by the ADR 0022 §3
  Stage 10.1 and Stage 10.2 amendments. Choosing AWS Secrets Manager is an engineering decision:
  it is already the accepted platform store (ADRs 0084 and 0086).
- **Owner:** `architect`. `security` concurrence is recorded in
  `docs/plans/stage-10.3-planning/04-review-security.md`, conditions C1, C2 and C4–C8, C12, C13
  and C15, adopted by rulings R1, R2, R4, R5, R7 and R8.
- **Sources:** paper `01-provider-trust-analysis.md` §2, as corrected by security C1–C8 and R15.

> Numbering note: migration `0093_sportsbook_settlement_causation_xact_status` is unrelated.
> The handle migration is provisionally `0096`.

## Context

Today every callback credential is a MOCK credential derived from a per-process random master
(`internal/webhookauth/mock.go`). The mock resolvers are wired only when
`TestSupportRoutesEnabled()` is true; otherwise the resolver is nil and every callback returns
401 with reason `no_resolver`. That wiring lives in `cmd/platform-api/wiring.go`.

The current code has these limits:
- `webhookauth.Resolver.Resolve(ctx, tenantID, providerID, keyID)` takes no transaction.
- `webhookauth.Fingerprint` is an unkeyed, truncated SHA-256 (16 hex characters).
- Outbound provider calls use one static, process-wide key read from an environment variable
  (`internal/providers/httpclient`, `ProviderConfig.APIKeyEnvVar`).
- No credential table exists.

ADR 0022 §2.2 already requires three things: a handle and never material, FORCE RLS with no
platform policy, and exclusion from CDC.

## Decision

### 1. Handle table `provider_credential_handles` (migration 0096, W2a)

This is tenant-owned configuration. **No secret value is ever stored in it.**

| Column | Rule |
|---|---|
| `id` | UUID primary key. |
| `tenant_id` | `NOT NULL`, `REFERENCES tenants(id)` directly. |
| `domain` | `CHECK IN ('payments','kyc','casino')`. May be widened additively. Provider ids are not unique across domains. |
| `provider_id` | Same charset as `ValidProviderID`. |
| `purpose` | `CHECK IN ('webhook_verify','outbound_api')` (R15). Inbound and outbound never share a handle (ADR 0022 §4.2). |
| `key_id` | `CHECK (key_id ~ '^[A-Za-z0-9._-]{1,64}$')`. A vendor key id, or a platform label for `KeyImplicit` schemes. |
| `secret_ref` | Backend-prefixed and **tenant-namespaced** (see below). Pins an immutable version. At most 512 bytes. |
| `fingerprint` | `CHECK (fingerprint ~ '^fp1:[0-9a-f]{64}$')`. See §2. |
| `vendor_account_id` | Nullable. The merchant or account id the vendor signs. Feeds `Credential.BoundAccountID` (ADR 0022 §3 point 3). |
| `status` | `CHECK IN ('active','verify_only','revoked')`. `verify_only` is allowed only when `purpose = 'webhook_verify'`. |
| `status_changed_at` | Set by the trigger. |
| `not_before` | `NOT NULL`. |
| `not_after` | `NULL` means unbounded. `NOT NULL` is required when `status = 'verify_only'`. |
| audit carriers | `created_at` and `created_by`; `revoked_at`, `revoked_by`, `revoke_reason`. |

**`secret_ref` namespace (R2 / C1).**
- The form is `<backend>://…/provider-creds/<tenant_id>/<domain>/<provider_id>/<name>`.
- For `awssm`, the ref ends in `?versionId=<id>[#<jsonKey>]`. A ref without `versionId`, or with
  a stage label such as `AWSCURRENT`, is refused in staging and production.
- The admin API rejects any ref whose tenant, domain or provider segments differ from the row's
  own values.
- The resolver independently refuses such a row. It fails closed with reason
  `credential_integrity` and raises a P1 alert.

**Constraints:**
- `UNIQUE (tenant_id, domain, provider_id, purpose, key_id)`.
- Partial unique index: at most one `active` row per `(tenant_id, domain, provider_id, purpose)`.
- Partial unique index: **at most one `verify_only`** row per the same key (R4).
- **Global** `UNIQUE (domain, provider_id, purpose, fingerprint)`, across all tenants and all
  statuses (R2).
  - Constraints are not subject to RLS. This key stops one secret from being bound to two
    tenants, and stops a revoked secret version from being registered again.
  - The fingerprint HMAC therefore must not include the tenant id.

**Transition trigger (R4 / C4):**
- Only these status transitions are allowed: `active→verify_only`, `active→revoked` and
  `verify_only→revoked`. Reactivation is impossible.
- Only the status, `not_after` and revocation columns may be updated. `DELETE` and `TRUNCATE` are
  refused, because the history is evidence.
- **`not_after` may only shrink.** The new value must be non-NULL and no later than the old
  value, where NULL counts as +∞. Extending it or clearing it is refused.
- **The overlap is capped.** For a `verify_only` row, `not_after − status_changed_at` must be at
  most **7 days**. This is a platform constant; changing it needs an amendment to this ADR.

**RLS and grants:**
- `ENABLE` and `FORCE ROW LEVEL SECURITY`, with a tenant policy on `app.tenant_id`.
- **No** platform or dual-scope policy.
- Runtime role grants: `SELECT, INSERT, UPDATE(status, status_changed_at, not_after, revoked_at,
  revoked_by, revoke_reason)`. No `DELETE`.
- The table is excluded from CDC.
- W2a must add the table to ADR 0019's enumerated table list, in the same way ADR 0022 §2.2 added
  `provider_capabilities`.
- A schema test asserts that no column name matches `secret|key_material|private|password`,
  other than `secret_ref`.

**Down migration.** It refuses when rows exist, using a validated `CHECK (false)` guard as in the
0091 down migration. It never uses `count(*)` under FORCE RLS.

### 2. Fingerprint (R5 / C6)

- The fingerprint is `fp1:` followed by the hex of HMAC-SHA256, computed under a **platform
  fingerprint key** over a domain-separation label (`igaming/provider-credential-fingerprint/v1`),
  a `0x00` byte and the secret value.
- The key is a platform secret delivered the same way as the other platform secrets (ADR 0086).
  In development and CI it comes from local configuration.
- Operators supply a confirmation value at registration. It is compared in constant time and is
  never persisted or logged.
- `webhookauth.Fingerprint`, the unkeyed form, remains only for the MOCK credentials.

### 3. Four-eyes on activation, single actor on disabling (R1 / C2)

**Rule: enabling takes two people; disabling takes one.**

| Action | Control |
|---|---|
| Register any handle that is or will become `active` (both purposes, including the new key in a rotation) | **Four-eyes, enforced in the DB.** A different platform administrator gives a content-bound approval that expires. |
| `active→verify_only`, shortening `not_after`, `→revoked` | **Single actor**, with a mandatory reason code. Revocation never waits for a second approver. |
| Reactivation, or re-registering a revoked fingerprint | Impossible, because of the trigger and the global unique key. |

**The DB pattern it follows.**
- The pattern is platform-scoped change-request and approval tables whose triggers consume the
  approval.
- Precedents:
  - `asset_change_requests` / `asset_change_approvals`: migration `0044`, **hardened by `0047`**;
  - `casino_catalogue_change_requests` / `casino_catalogue_change_approvals`: migration `0086`,
    **hardened by `0089`**.
- W2a copies the **hardened** shape, not the original 0044/0086 shape. Migrations 0047 and 0089
  exist because the original person check was inert (SEC-S92-1).

**What W2a keeps from that pattern:**
- A request is append-only apart from its own terminal transition.
- Approvals are immutable (`ledger_deny_mutation`).
- `UNIQUE (request_id, approver_principal_id)`.
- The requester and approver must each be an **active, platform-scoped staff principal with a
  linked Person**. The approver must be a distinct principal **and** a distinct Person.
- Any rejection blocks the request.
- The request is consumed and marked `applied` in the same statement that performs the insert.

**What W2a adds:**
1. **Content binding.**
   - The request carries the target `tenant_id` and every handle column: domain, provider,
     purpose, `key_id`, `secret_ref` with its version, fingerprint, `vendor_account_id`,
     `not_before` and `not_after`.
   - It also carries the disposition of the predecessor: `verify_only` with its `not_after`, or
     `revoked`.
   - The consuming trigger compares each column of the inserted row with `IS NOT DISTINCT FROM`,
     as 0086/0089 do for their payload.
2. **Expiry.** An approval can be consumed only within **24 hours** of its `decided_at`. This is
   a platform constant. The 0044/0086 pattern has no expiry.
3. **The scope bridge.**
   - The precedent tables are platform-scoped: their `platform_admin_scope` policy requires
     `app.tenant_id` to be empty. Principal eligibility is checked against `staff_users`, which
     under its `dual_scope_isolation` policy (migration 0011) cannot see platform rows from a
     tenant-scoped transaction.
   - The handle insert, however, must run under `WithTenant(target)`, because the handle table has
     no platform policy.
   - The codebase avoids `SECURITY DEFINER` for these lookups (the comments in migrations 0029
     and 0076).
   - Decision:
     - Filing and approving run in platform scope, so the 0047/0089 principal checks work
       unchanged.
     - The consume step runs in the tenant-scoped insert, through a **narrow additional policy on
       the governance tables only**. That policy allows `SELECT` and the terminal `UPDATE` to
       `applied` on rows where `target_tenant_id = app.tenant_id`.
     - No policy is ever added to `provider_credential_handles` or `staff_users`.
   - W2a design review may replace this bridge with an equivalent mechanism, subject to `security`
     concurrence. It may not replace it with one that weakens any rule above.

The promotion of the old key to `verify_only`, or its revocation for outbound credentials,
happens in the same transaction as the approved insert. Each row change writes its own audit
row.

**Permissions (four, never bundled into `PermTenantWrite` or `PermCasinoConfigWrite`):**

| Permission | Allows | Granted to |
|---|---|---|
| `provider_credential:read` | List handles: `key_id`, status, window, fingerprint, `secret_ref`. Never the secret value | `RoleTenantAdmin`, `RolePlatformAdmin` |
| `provider_credential:request` | File a registration | `RolePlatformAdmin` |
| `provider_credential:approve` | Decide on a registration | `RolePlatformAdmin`; the DB enforces a different Person |
| `provider_credential:revoke` | Move to `verify_only`, shorten the window, revoke | `RoleTenantAdmin`, `RolePlatformAdmin` |

**API rules:**
- The target tenant always comes from the authenticated scope, never from the request body.
- Every registration error returns one generic 400/409. The specific error class is written to
  the log only.
- Every write records an audit row with: actor, target tenant, handle, before and after status
  and window, fingerprint, IP and reason code. The secret value and the ref's secret content are
  never recorded.
- API scope is create/rotate/revoke, approval and audit only (R13).

### 4. Resolver (R5 / C5 / C7)

**Signature:**

```go
Resolve(ctx, tx pgx.Tx, tenantID uuid.UUID, providerID, keyID string, sel KeySelection) (CredentialSet, error)
```

- The domain is fixed when the resolver is constructed.
- `sel` always comes from the scheme's `Properties().KeySelection`. It never comes from the
  request (ADR 0022 §3, Stage 10.3 amendment).
- Mock resolvers ignore `tx`.

**Per-request handle read inside the caller's transaction.**
- There is exactly one read: a plain, lock-free `SELECT` with an explicit `tenant_id = $1` in
  addition to RLS.
- Its SQL is pinned by the statement-capture tests.
- It is the one pre-verification statement that ADR 0022 §3 point 9, as amended, allows.
- Revocation therefore takes effect immediately.
- The only in-flight exposure is a transaction that read the row before the revocation
  committed.

**Row counts:**
- `KeyFromHeader`: exactly one row.
- `KeyImplicit`: the `active` row plus at most one `verify_only` row, within `not_after`.
- Any other count fails closed.

**Store and cache:**
- The cache is in-process and keyed on **(tenant_id, secret_ref, fingerprint)**.
- The **fingerprint is compared on every resolve**, cache hits included. A mismatch fails closed
  with `credential_integrity`, raises a P1 alert and does not serve the value.
- Each store call runs under `singleflight` with a 2-second context timeout.
- A **circuit breaker / negative cache** makes calls fail fast (uniform 401,
  `credential_store_unavailable`) during a cool-down after a store error. A store outage
  therefore never holds pooled DB connections behind a 2-second wait.
- When the TTL expires the entry is refreshed, not evicted. A cached pinned version may be served
  up to a bounded max-stale while the store is failing.
- Only refs that exist as handle rows ever reach the store. A caller cycling through key ids
  costs one indexed DB miss and no store call.

**Fail closed.** The HTTP response is always the uniform 401. New closed `Reason` values:
- `credential_store_unavailable`;
- `credential_integrity`.

A DB error on the handle read also gives the uniform 401. The chosen log reason must not depend
on whether a handle exists.

**Wiring:**
- The real resolver serves non-synthetic adapters.
- The MOCK resolver serves synthetic adapters, and only when `TestSupportRoutesEnabled()` is
  true.
- The split is a two-way choice by adapter kind (ADR 0085 §1, Stage 10.3 amendment). It is never
  a map keyed by vendor.
- No real casino resolver is wired anywhere until revocation has been built and its immediacy
  tested (R9 / C14).

**Secret length (added at gate 10.3-W1; security review `06-gate-w1-review-security.md` §3).**
- `webhookauth.MinSecretBytes = 16` (128 bits) is the platform **floor**. The platform checks it
  before any scheme runs (`credentialUsable`, `internal/webhookauth/scheme.go`), and every scheme
  must check it on its own (conformance SC10). It is accepted as a floor because the platform does
  not choose most vendor secrets and a length check cannot measure entropy.
- **A secret the platform itself generates or negotiates must be at least 32 random bytes.** This
  covers outbound and webhook secrets created in W2a/W3b. W2a must enforce it where those
  secrets are generated. The MOCK already derives 32-byte keys.
- Disclosed residual: a vendor secret delivered as hex or base64 text and used as raw bytes
  carries about 4–6 bits per byte. A 16-byte one may be only 64–96 bits strong. The §2
  fingerprint is keyed (C6) because vendor secrets may be low-entropy. No further action now.
- Raising the floor is a constant change in a reviewed commit. No schema depends on it.

**`KeyImplicit` (gate 10.3-W1).** The `KeyImplicit` row-count rule above is `NOT IMPLEMENTED`.
Until W2a builds it, the Stage 10.3 W1 fix round refuses a `KeyImplicit` scheme at registration
(ADR 0022 §3, Stage 10.3 amendment).

### 5. Outbound credentials (PROV-OUTBOUND-CRED-1, R5 / C8)

- The orchestrator reads the `outbound_api` handle row **on every call**, inside the caller's
  transaction.
- It passes an `OutboundCredential` to the adapter, and the HTTP client authenticates through a
  per-call `Authenticator`.
- The credential is **never cached** in the adapter, the `Authenticator`, a long-lived HTTP
  client or a vendor SDK session.
- The only material that persists across calls is the resolver's cache from §4. It cannot outlive
  a revocation, because every call reads the handle row first.
- Required tests:
  - revoke, then the next outbound call fails closed;
  - tenant A's call never carries B's credential.
- The process-wide static key sourced from `APIKeyEnvVar` is removed.

### 6. Secret-store backends (R7 / R8 / C12 / C13)

| Backend | Allowed in | Rules |
|---|---|---|
| `memory://` | **Tests only.** It can be constructed only from test code, and CI enforces this in the same way as the existing "only test support reads X" checks. | — |
| `devfile://` | **Development only.** Requires `APP_ENV` to be **explicitly** `development`; a missing `APP_ENV` is treated as production. | Reads from a git-ignored directory. |
| `awssm://` | staging, production | See below. |

`awssm` rules:
- It uses **task-role credentials only**. Startup is refused when static credentials are present:
  `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_PROFILE` or a shared
  config file.
- Endpoint overrides and custom CA bundles are refused.
- The region comes explicitly from configuration.
- It calls `GetSecretValue` with a **pinned `VersionId`** only.
- **SDK logging is off** (`ClientLogMode` zero). SDK errors are classified into a closed set
  before they are logged.
- The client is constructed after `config.Load()` and after the synthetic guard, and makes no
  network call at init.
- The SDK modules allowed are the core, credentials/config and `service/secretsmanager` only, at
  pinned versions.
- They are **confined to one package**, `internal/secretstore/awssm`. An import-boundary test
  fails if any other package imports `github.com/aws/aws-sdk-go-v2/...`.
- `govulncheck` is in CI.
- W3b tests run only against an SDK-interface fake, with a test proving that no real AWS endpoint
  is ever dialled.

Production allows only `awssm`. Both registration and the resolver enforce the per-environment
allow-list.

### 7. Where the secret value may never appear

The secret value never reaches the database, logs, error strings, audit rows, OpenAPI examples,
the frontend, git, or Terraform state.

Every secret-bearing type carries the same redaction as `webhookauth.Credential`:
- `String`, `GoString` and `LogValue`;
- **plus a redacting `MarshalJSON`**, which `Credential` lacks today (C15).

This applies to `CredentialSet`, `OutboundCredential` and store results. Refs and fingerprints
are not secret, but they are staff-only.

### 8. Local versus STAGING REQUIRED

Everything in §1–§7 is built and tested locally: synthetic PostgreSQL, the NOBYPASSRLS runtime
role, the `memory` and `devfile` backends, and the SDK fake.

The following items are **STAGING REQUIRED**. They are deferred to the single governed staging
deployment and require separate human authorization:
- a task-role `secretsmanager:GetSecretValue` on `<prefix>/provider-creds/*`, plus
  `kms:Decrypt` constrained by `kms:ViaService` if a customer-managed key is used;
- the network path to Secrets Manager;
- delivering the platform fingerprint key to the task. This is a new platform secret plus an
  execution-role secret ARN, which is a `deploy/` change;
- real `GetSecretValue` latency and throttling;
- cold-cache behaviour;
- rotation and store-outage drills, using a **synthetic** secret;
- alarms on `credential_store_unavailable` and `credential_integrity`;
- confirming that no secret appears in container logs, task metadata or Terraform state.

### 9. HD-10.3-2: IAM excluded

The human excluded AWS IAM code changes from Stage 10.3. In this stage, therefore:
- no IAM, KMS or egress code is written under `deploy/`;
- the "empty/minimal task role" statement stands unchanged. It is in ADR 0084 and in the
  task-role `description` in `deploy/aws/modules/iam/main.tf`. ADR 0086 §17's execution-role
  separation also stands unchanged.

Consequences:
- The `awssm` backend cannot run on AWS until a **future human decision** authorizes the IAM
  architecture and its apply.
- Security's IAM conditions (C12 item 10) are recorded for that decision.
- The same change must amend the task-role statement it overrides.

## Consequences

- Real resolvers become possible for all three domains without an application rewrite.
- One task role would read every tenant's provider secrets. Isolation of the secret material
  rests on RLS over the handles, the tenant-namespaced refs and fingerprint integrity; it does not
  rest on IAM.
- Per-tenant IAM ABAC or KMS keys are the next rung on the isolation ladder. The trigger is the
  first B2B or bring-your-own-licence tenant whose contract or regulator requires it. That
  disclosure is carried to the human.
- A store outage defers callbacks: vendors get a 401, and reconciliation and `QueryStatus` are
  the backstop. Revoking a casino credential strands open exposure. Both effects are accepted
  (R9).
- PROV-REVOKE-ALL-1 (a cross-tenant revoke for one provider) is registered and not built.

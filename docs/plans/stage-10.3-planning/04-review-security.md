# Stage 10.3 planning gate — security review (`security`)

- **Reviewed:** `docs/plans/stage-10.3-planning-gate-proposal.md` and the papers `00`–`03` in this folder,
  especially `01-provider-trust-analysis.md` (§1–§3, §5–§8) and `02-casino-financial-analysis.md` §1.7
  (plus §1.3, §1.10 and §2.6/§2.11 where they bear on security).
- **Also checked:** CLAUDE.md; ADR 0022 §2.2, §3 and its Stage 10.1/10.2 amendments, and §4.2; ADR 0085;
  ADR 0086 §4 and its IAM notes; `docs/plans/stage-10.2-planning/09-review-security-final.md`;
  `docs/security/security-architecture.md` (four-eyes asymmetry).
- **Code facts read to check the plan:** `internal/config/config.go` (`resolveAppEnv`,
  `TestSupportRoutesEnabled`), `internal/webhookauth/webhookauth.go` (`Credential` redaction,
  `Fingerprint`), `internal/auth/permission.go` (dual-control and kill-switch precedents),
  `internal/httpserver/casino_admin_handlers.go`, `cmd/` (the three binaries), `go.mod`,
  `.github/workflows/ci.yml`.
- **Repository state:** branch `claude/focused-wright-jw88w9`, HEAD `ff30d87`.
- **Type of review:** planning only. No code was edited, nothing under `deploy/` was touched, no AWS or
  Terraform command was run, and nothing was committed.

## Verdict: **APPROVE WITH CONDITIONS**

The trust-boundary design is sound. It is also a real improvement: verify-first becomes enforced by the
platform instead of by adapter discipline, and replay tolerance becomes a mandatory, tested property.

The credential model is correct in its main choices:
- no secret material in the database;
- a pinned immutable version plus a fingerprint;
- revocation enforced by a per-request read of the handle row, not by cache expiry;
- fail-closed behaviour with a uniform 401;
- no platform or dual-scope RLS policy.

**Conditions:**
- Two **High** conditions must be written into ADR 0093 at W0 and implemented in W2a:
  - C1: tenant namespacing of `secret_ref`;
  - C2: DB-enforced four-eyes on credential activation.
- The remaining conditions are Medium or Low. Each is a small, explicit addition to a wave the plan
  already contains.

**Nothing blocks Stage 10.3 from starting.** No finding requires a new human decision. §7 lists the items
that are human decisions or that should be disclosed to the human.

---

## 1. Webhook trust boundary

### 1.1 Orchestrator-enforced `Verify` — **SOUND** (concur)

Today, verification happens inside `HandleCallback`. Each domain's I1 therefore depends on every adapter
author remembering to verify before parsing. Moving `scheme.Verify` into each `ReceiveCallback`, after
resolution and before `HandleCallback`, makes that ordering a property of platform code. That is the right
direction, and it matches ADR 0022 §3 points 4–7.

Other parts I concur with:
- `provider_unregistered` moves ahead of the tenant lookup. The 401 is identical, and this slightly
  narrows the F-4 timing residual.
- Adapters may re-verify as defence in depth.

**Residual risk.** Enforcement still lives in three hand-written orchestrator call sites. The plan's
mutation-kill column covers this only if the mutation is specifically "delete the `Verify` call in domain
X". This is condition **C9(a)**.

**Recommendation (Info, R-1).** Make the ordering a compile-time property. `Verify` would return a
`webhookauth.Verified` value that only `webhookauth` can construct (unexported fields), and
`HandleCallback`/`Parse` would require it as an argument. This is not a condition.

### 1.2 `VerificationScheme` interface — **SOUND, three fixes needed**

Parts that are correct:
- `Extract` is pure (no DB, secret, clock or body parsing) and restricted to the two missing/invalid
  reasons.
- The orchestrator supplies `now`.
- `CredentialSet` holds at most one `Previous`.
- Properties are declared.

Three defects need fixing:

- **(a) Medium — key-selection mode must come from the scheme, never from the request.** The proposed
  lookup SQL switches to multi-row trial when `$4 = ''`. If the orchestrator or resolver infers
  `KeyImplicit` from an empty `KeyID`, then a `KeyFromHeader` vendor whose `Extract` tolerates an absent
  key-id header lets an attacker turn the "exactly one credential" rule into a two-key trial just by
  omitting the header.

  *Failure scenario:* during a rotation window the old key is compromised and sits in `verify_only`.
  Traffic signed with the old key and sent without a key-id header now verifies, even though the vendor's
  own scheme would have pinned the new key id. → **C3**.

- **(b) Low — the error contract contradicts the new log reason.**
  - `Verify` is specified to return only `ErrSignatureInvalid`.
  - The proposal (§13) and new point 10 introduce a log reason `timestamp_out_of_window`.

  Recommended resolution:
  - `Verify` returns a closed `Reason` (or a typed `*AuthError`) alongside the single sentinel.
  - `timestamp_out_of_window` is reported only when the MAC over the same bytes is otherwise valid.
    Otherwise it is `signature_invalid`. This makes the reason a real replay signal rather than noise from
    unauthenticated callers.
  - The HTTP response remains the uniform 401.
  - The reason is added to the closed enum and to the allow-list log tests.

  → **C10**.

- **(c) Low — self-declared `Properties()` must also be validated when an adapter registers, not only in
  the conformance suite.** A non-synthetic scheme declaring `SignedTimestamp=false`, `MaxSkew<=0`,
  `MaxSkew` above the cap (C11), or an unknown `Binding` must make the process refuse to start. The suite
  proves that the declaration is truthful; startup validation proves that the declaration is permitted.
  → **C11**.

### 1.3 Conformance suite and its broken-scheme self-test — **SOUND in shape, not yet sufficient as a gate**

SC1–SC13, the fail-not-skip SC7, the conversion of the §0 skips to failures (which closes my 10.2 finding
F-9), and the self-test against broken reference schemes are all the right mechanisms. Four gaps would
leave the suite passable by a wrong real adapter:

1. **Sign and Verify can share a bug.** The adapter author supplies `Sign`. If the author's understanding
   of the vendor algorithm is wrong in the same way in both `Sign` and `Verify` (for example, both omit
   the timestamp or the account id from the signed input), SC1–SC3 pass. For every real scheme, the suite
   must include **at least one known-answer vector taken from the vendor's own documentation or a
   recorded sandbox delivery**. The vector is committed with its provenance, contains no real credential,
   and uses a vendor test key or a sandbox key rotated after capture.
2. **Each mandatory case must be shown to be load-bearing.** The self-test must include at least one
   broken reference scheme per mandatory property that only that case kills:
   - ignores tenant → SC3;
   - ignores provider → SC4;
   - ignores timestamp → SC7;
   - prefix compare → SC2;
   - ignores bound account → SC8;
   - honours `Previous` after `not_after` → SC9;
   - accepts an empty secret → SC10;
   - secret in error text → SC11;
   - panics on a malformed header → SC5;
   - multi-key trial on an absent key id under `KeyFromHeader` → new case, see C3.

   The plan's three examples are not enough to prove the suite.
3. **Tampering must be generated by the suite, not by the fixture.** SC2 must mutate every byte of the
   body and of every header the fixture declares as authentication material. The fixture declares the
   header set; the suite does the flipping.
4. **The suite must actually run for every real scheme.** Nothing in the plan guarantees that a newly
   registered non-synthetic adapter has a `RunSchemeConformance` invocation. A test must iterate the
   adapter registrations (the same `buildRegistrations` used by the synthetic guard, §4) and fail for any
   non-synthetic scheme with no registered fixture.

**Constant time.** No black-box suite can prove constant-time comparison. The "accepts a prefix" broken
scheme proves correctness, not timing. Constant time must come from a lint or AST rule: in scheme
packages, signature and MAC comparison uses `hmac.Equal` or `subtle.ConstantTimeCompare`, never `==` or
`bytes.Equal`. It must also be a named `code-reviewer` checklist item.

→ **C9**.

### 1.4 ADR 0022 §3 amendments

#### Point 2 — key-id overlap for `KeyImplicit` vendors: **CONCUR, with conditions**

`security` concurs. The proposed clarification is: at most the `active` key plus one `verify_only`
predecessor, same tenant/domain/provider/purpose, inside `not_after`, never across tenants or providers.
It keeps the property point 2 protects, which is that no other tenant's key material is reachable from
this route. A hard cut-over rotation is operationally worse (callbacks are lost during the vendor switch)
and no more secure against the realistic threat.

Conditions (**C4**):
- The trial mode is selected by `Properties().KeySelection` only (C3). A `KeyFromHeader` scheme never
  receives a `Previous`.
- **Bounded overlap:**
  - `not_after - (transition time)` is at most a platform maximum (proposed 7 days). It is enforced by
    the admin API and by the transition trigger.
  - `not_after` can only be **shortened**, never extended or cleared. The proposed grant
    `UPDATE(not_after)` currently allows extension, and extension is a widening.
  - The trigger must refuse it.
- **At most one `verify_only` per (tenant, domain, provider, purpose) for `KeyImplicit`.** It is enforced
  by a partial unique index, or, if the orchestrator prefers, by the resolver's existing "any other row
  count fails closed" rule plus a test. The DB constraint is preferred.
- **Verification evaluates both credentials without short-circuiting**, then accepts if either matched.
  The log records which `key_id` verified, so lingering use of the old key after the vendor switch is
  observable.

#### Point 9 — read-only handle lookup before verification: **CONCUR, with conditions**

Points 4 and 9 genuinely conflict for a DB-backed resolver, and the amendment resolves it at the smallest
possible surface. The single statement discloses nothing to the caller: the response stays the uniform
401 and no row data is returned.

Conditions (**C5**):
- Exactly one statement, a plain `SELECT`: no `FOR UPDATE`/`SHARE`, no advisory lock, no write, no
  audit row.
- Explicit `tenant_id = $1` in addition to RLS.
- The K7/C7 statement capture pins the **exact** SQL text/shape, not "any SELECT on the table".
- A DB error during that read yields a response that does not depend on whether a handle exists (the
  uniform 401 with a new log reason, or the existing non-attacker-controllable 500, consistently).
- The payments path keeps its existing point 4 allowance **plus** this read, and no more.
- **Accepted residual (Low, R-2).** A cold-cache store fetch makes latency differ between an existing and
  a non-existing (tenant, provider, key id) handle. This is the same class as F-4. Key ids are not
  secrets. Revisit with PAYWH-RL-1.

#### New point 10 — mandatory signed-timestamp window: **CONCUR, with conditions**

This correctly absorbs PAYWH-TS-1. I concur with closing TS-1 as superseded, **once** point 10 is recorded
and SC7 is fail-not-skip.

Conditions (**C11**):
- **Platform cap on `MaxSkew`:** proposed 10 minutes. A larger vendor tolerance needs a recorded `security`
  sign-off in that adapter's review.
- The timestamp must be inside the signed input. SC2 already mutates it.
- The platform clock is the only `now`.
- **The MOCK exemption is acceptable only because MOCK can never run in production** (§4 of this review).
- **Carry F-5 (Low).** A replay within the window of a KYC `error` callback still appends one audit row.
  The recommendation stands to dedupe on (verification, provider event id) with the first real KYC
  adapter.

**Disclosure for the human, not a decision now.** Point 3 (per-merchant keys or a signed account id),
point 10 (signed timestamps) and ADR 0022 §4.2 (separately scoped credentials) are now hard
**vendor-selection constraints**. A vendor lacking any of them is not integrable without a further ADR.
That should be known before contracting.

---

## 2. Credential model

| Aspect | Ruling |
|---|---|
| No secret values in the DB | **Sound.** The schema test (no column matching `secret|key_material|private|password` other than `secret_ref`), CDC exclusion, the ADR 0019 listing and audit of handle plus fingerprint only are all correct. One addition (Low, C15): new secret-bearing types (`CredentialSet`, `OutboundCredential`, store material) must carry the same redaction as `webhookauth.Credential` (`String`/`GoString`/`LogValue`) **and** a redacting `MarshalJSON`. `Credential` has no `MarshalJSON` today, so `json.Marshal(cred)` would emit `Secret` as base64. |
| Pinned version plus fingerprint | **Sound in principle, two fixes needed.** (1) **Tenant namespacing of `secret_ref` is missing (High, C1).** See below. (2) **Fingerprint keying (Medium, C6).** A 64-bit unkeyed truncated SHA-256 of a possibly low-entropy vendor secret appears in logs (`signature_invalid`), audit rows and the admin GET, so it is an offline-guessing oracle. **Ruling: key it.** Use HMAC-SHA256 with a platform fingerprint key delivered like the other platform secrets (ADR 0086), a domain-separated label and a version prefix (`fp1:`); the column CHECK widens accordingly. Any operator-supplied confirmation value at registration is compared in constant time and is never persisted or logged. `awssm` refs without an explicit `versionId` are refused in staging and production; no stage labels such as `AWSCURRENT`. |
| Cache semantics during an outage | **Sound for integrity; one availability defect (Medium, C7).** Keying by a pinned immutable ref makes stale-serving safe, and revocation does not depend on the cache. The defect: the store call runs inside the caller's tenant transaction, which holds a pooled DB connection. During a store outage with an expired or cold cache, every callback for a valid handle holds a connection for the full 2 s timeout, with bounded retries on top. A burst of legitimate vendor retries, or an attacker replaying any valid key id, can exhaust the **shared** pool and degrade player traffic. Required: (a) a circuit breaker / negative cache on store errors, so that after a failure, requests for that ref fail fast (uniform 401 `credential_store_unavailable`) for a short cool-down, and are never serialized behind a 2 s wait; (b) a concurrency test proving an outage does not pin N connections; (c) TTL triggers refresh, not eviction, so max-stale can actually be served; (d) the cache key includes tenant id, ref **and** fingerprint, and the fingerprint is compared on **every** resolve, including cache hits, not only on fetch. |
| Revocation latency | **Inbound: immediate. Sound.** The per-request handle read happens in the caller's transaction; the only in-flight exposure is a transaction that read the row before the revoke committed. **Outbound: not yet guaranteed (Medium, C8).** The outbound credential must be resolved per call from the handle row. It must not be cached in the adapter, the `Authenticator`, a long-lived HTTP client or a vendor SDK session. Required test: revoke, then the next outbound call fails closed. |
| Rotation overlap | Concur. See point 2 (C4). Also: the automatic `active → verify_only` transition performed when a new active key is registered writes its own audit row in the same transaction, and outbound rotation (new active, old revoked atomically) is audited per row. |
| Admin handle API permissions | **Ruling below** (C2). |
| Four-eyes on credential changes | **Ruling below** (C2). |

### 2.1 C1 (High): `secret_ref` must be bound to the owning tenant

**The gap.**
- One ECS task role can read every tenant's provider secrets (paper 01 §2.4).
- The handle table stores a free-form `secret_ref`.
- Nothing in the design stops tenant B's handle from naming tenant A's secret, whether through an API
  write, a mistake or SQL injection on a runtime-role path. The runtime role holds `INSERT` on the table.

**Failure scenario (outbound, confused deputy).** A tenant-B principal who can register a handle points
B's `outbound_api` handle at A's `awssm://<prefix>/provider-creds/<A>/payments/psp-x/...` ref. The
platform then signs B's withdrawals, status queries or game launches **as A's merchant account** at the
vendor. The fingerprint check does not stop this, because the material is genuine.

**Inbound variant.** Binding A's `webhook_verify` secret to B makes A-signed traffic verify at B's route,
which defeats the point 3 tenant binding for `PerMerchantKey` vendors.

**Required:**
1. The canonical secret path embeds the tenant id, the domain and the provider:
   `<prefix>/provider-creds/<tenant_id>/<domain>/<provider_id>/...`.
2. The admin API rejects any ref outside the caller's target tenant namespace.
3. The resolver independently refuses (fail closed, `credential_integrity`, P1) any row whose ref is
   outside its own tenant namespace. This is defence in depth against rows inserted by other paths.
4. A **global** unique index on `(domain, provider_id, purpose, fingerprint)` across all tenants and all
   statuses. Constraints are not subject to RLS. It prevents the same secret from being bound to two
   tenants, and it prevents re-registering a previously revoked secret version, which would otherwise
   silently undo a revocation (see C2).
5. Per-tenant IAM ABAC or a per-tenant KMS key remains a later tightening step on the isolation ladder;
   it is not required now. Trigger: the first B2B (especially bring-your-own-licence) tenant whose
   contract or regulator requires isolation of credential material. See §7.

### 2.2 Ruling: admin handle API permissions and four-eyes (C2, High)

**Precedent applied.** The platform's established asymmetry is:
- the widening direction is dual-controlled and enforced by the database:
  - `asset_change_requests` (migration 0044);
  - `casino_catalogue_change_requests` (migration 0086);
  - the bonus four-eyes set;
- the fail-closed direction is single-actor ("turning something off must never require a second
  approver", `security-architecture.md` B1.2; `PermBonusCampaignSuspend`).

A credential handle decides which key can move money into and out of a tenant, so it falls under that
rule.

**Four-eyes ruling:**
- **Required (four-eyes):**
  - registering any handle that can become `active`, for both `webhook_verify` and `outbound_api`, which
    includes the new key in a rotation;
  - reactivating anything. This is already impossible, because the trigger is forward-only and C1(4)
    forbids re-registering a revoked fingerprint.
- **Not four-eyes, single actor, reason code mandatory:** `active → verify_only`, any shortening of
  `not_after`, and `→ revoked`. Revocation is the emergency stop (§5) and must never wait for a second
  approver.
- **Enforcement:**
  - enforced **by the database**, using the 0044/0086 trigger pattern;
  - the approval must come from a different principal, and self-approval is refused by the trigger;
  - the approval binds the exact row content (ref with version, fingerprint, `key_id`, purpose,
    `vendor_account_id`, `not_before`/`not_after`) and expires (proposed 24 h).

  Enforcing it only in the application is insufficient because the runtime role holds `INSERT` on the
  table.
- **Timing:** it ships in **W2a together with the API**, so the API never exists in single-actor form.
- **The only acceptable fallback, if the orchestrator or `product-owner-proxy` defers it:**
  - the register route is not mounted unless `APP_ENV` is **explicitly** `development`;
  - four-eyes is registered as a blocker before any staging drill and before any non-synthetic
    credential is registered anywhere.
- **No threshold.** CLAUDE.md's "above a configurable threshold" applies to monetary adjustments; a
  credential change has no amount.

**Permission ruling.** Four new permissions. None may be bundled into `PermTenantWrite` or
`PermCasinoConfigWrite`: the role that configures routing or capabilities must not also be able to rebind
credentials.

| Permission | Grants | Grantees |
|---|---|---|
| `provider_credential:read` | List handles: `key_id`, status, window, fingerprint, `secret_ref`. Never material. | `RoleTenantAdmin`, `RolePlatformAdmin` |
| `provider_credential:request` | File a registration | `RolePlatformAdmin` only |
| `provider_credential:approve` | Decide a registration | `RolePlatformAdmin` only; a different principal, enforced by the DB |
| `provider_credential:revoke` | `verify_only`, shorten `not_after`, revoke | `RoleTenantAdmin` **and** `RolePlatformAdmin` |

- **Why request is platform-only.** Secrets are provisioned out of band into the platform's own store,
  and tenants cannot write it (partner-console self-service is deferred). Registering a handle is
  therefore inherently a platform-operator act.
- **Why revoke is also granted to the tenant.** A tenant, particularly a bring-your-own-licence tenant,
  that learns its vendor key is compromised must be able to stop it without waiting for the platform.
- **The target tenant is never taken from the body.** It comes from the authenticated scope. For a
  platform principal acting on a named tenant, the handler runs `WithTenant(target)`, never a platform or
  `WithoutTenant` policy on this table, and the audit row records the actor and the target tenant. The
  mechanism belongs to `architect`; these are its security constraints.
- **Registration errors** (ref not found, access denied, fingerprint mismatch, out-of-namespace) are
  returned as a single generic 400/409 to the caller. The specific class goes to the log only.
- **Every write** records an audit row with actor, tenant, handle, before and after status/window,
  fingerprint, IP and reason code (CLAUDE.md).

---

## 3. AWS SDK dependency — **ACCEPTABLE, under conditions (C12, Medium)**

**Why it is acceptable.**
- AWS Secrets Manager is already the platform's accepted secret store (ADR 0084/0086).
- The SDK is first-party, widely deployed and Apache-2.0 licensed.
- The alternative, injecting provider secrets as ECS environment variables, is worse. Rotation would need
  a redeploy, every task would hold every tenant's secret for its lifetime, and per-request revocation
  would be impossible.

**Conditions:**
1. **Minimal modules.** Only the SDK core, `config` (or the credentials and ECS container-credentials
   providers) and `service/secretsmanager`, plus their transitive smithy dependencies. Versions pinned in
   `go.mod`/`go.sum`.
2. **Import boundary.** An import-boundary test fails if any package other than
   `internal/secretstore/awssm` imports `github.com/aws/aws-sdk-go-v2/...`. This stops the dependency from
   quietly becoming the path for other runtime AWS calls.
3. **Vulnerability scanning.** `govulncheck` (or equivalent) is added to CI no later than the wave that
   introduces the SDK. CI has no dependency-vulnerability step today.
4. **Credentials are task-role only in staging and production.** Startup refuses static credentials
   (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_PROFILE`, a shared config
   file) when `APP_ENV` is staging or production. The region comes explicitly from config.
5. **No endpoint override in production.** `BaseEndpoint`, `AWS_ENDPOINT_URL`,
   `AWS_ENDPOINT_URL_SECRETS_MANAGER` and a custom CA bundle are refused. The fingerprint check (C6)
   remains the backstop against a redirected store.
6. **No logging of secret content.**
   - SDK client logging (`ClientLogMode`) is off.
   - No request or response body is logged anywhere.
   - SDK errors are classified into a closed set before logging.
   - `SecretString`/`SecretBinary` never reaches a logger or an error string.
7. **Pinned reads only.** `GetSecretValue` is called only with `VersionId` (see §2).
8. **Bounded calls.** Retries fit inside the 2 s context, with the circuit breaker from C7.
9. **No network activity before the guard.** The SDK client is constructed after `config.Load()` and
   after the synthetic guard, and it performs no network call at init.
10. **For HD-10.3-2 (recorded here, not applied).** These conditions apply if the human authorizes the
    IAM change:
    - the task-role policy allows `secretsmanager:GetSecretValue` on `<prefix>/provider-creds/*` only;
    - no `ListSecrets`, `DescribeSecret`, `PutSecretValue` or wildcard permission;
    - `kms:Decrypt` only with `kms:ViaService = secretsmanager.<region>.amazonaws.com`, if a
      customer-managed key is used;
    - ADR 0086's statement that "the task role is deliberately empty" is amended in the same change.

---

## 4. MOCK-ADAPTER-PROD-1 guard — **fail-closed on its declared input; not yet fail-closed overall (C13, Medium)**

**What is right.**
- The guard is a pure function.
- It runs immediately after `config.Load()`, before any database access or `SyncCatalogue`.
- Its outcome is refusal to start, not silent omission.
- The subprocess ordering test and the matrix test are both included.

**Can a flag bypass it?** Not as designed. The proposal (§14) states it can be bypassed only by removing
the component. Condition: a test runs the guard with **every** boolean in `Config` set to true (and to
false) and still gets a refusal in production. That proves the guard has no config input other than the
environment.

**Three gaps nonetheless leave it fail-open.**

1. **Its trigger is the ADR 0085 fail-open-on-absence input.**
   - `resolveAppEnv()` returns `"development"` when `APP_ENV` is **absent**. ADR 0085's Consequences
     already disclose that "unset" and "deliberately development" are indistinguishable.
   - A real production task that is missing `APP_ENV`, or that has `APP_ENV=staging` because of a
     copy-paste error, starts with all eight synthetic components.
   - The same applies to the `devfile://` backend, which is allowed "only if `APP_ENV=development`".

   **Required:** for the synthetic guard and the secret-store backend allow-set only, an **absent**
   `APP_ENV` is treated like production. Synthetic components and `devfile://` require `APP_ENV`
   **explicitly present** and in {`development`, `staging`}. `Config` records whether the value was
   explicit. Local tooling and CI set it; unit tests construct `Config` directly.

2. **The deny-list depends on naming.**
   - The AST completeness test finds `type Mock\w+`.
   - A synthetic double named `Fake…`, `Stub…`, `InMemory…` or `Dev…` without the marker would pass.
   - Embedding also matters: a real type embedding a mock inherits `SyntheticComponent()`, which is the
     safe direction, but a mock embedding a real type would inherit any positive marker.

   **Required, preferred option:** production requires every registered component to implement a
   positive `ProductionEligible` marker **and not** `Synthetic`. An unmarked new component is then refused
   by default. Today no component is production-eligible, so this costs nothing now.

   **Acceptable alternative:** keep the deny-list, but widen the AST scan to every non-`_test` type that
   implements any provider, scanner, storage, resolver, statement-source, email or person-resolver
   interface, regardless of its name.

3. **Coverage must be all-of-`main`, and all binaries.**
   - The registration-completeness test must fail when a component is wired in `main` without passing
     through `buildRegistrations`.
   - `cmd/seed-admin` and `cmd/migrate` must be confirmed to wire no provider component, or they must
     call the same guard.
   - `memory://` must be constructible only from test code, following the existing CI pattern "only test
     support reads X".

**Out of scope for the guard (Info, for the first real adapter).** A real adapter must not contain a
built-in "mock/sandbox mode" switch. A vendor sandbox base URL in production is a production-configuration
checklist item, not a type-level property.

---

## 5. Casino kill-switch change — rulings

**Change reviewed.** A disabled capability now gates new bets only; wins, rollbacks, replays and tombstones
settle regardless (02 §1.3, §1.7).

### 5.1 Is credential revocation an acceptable emergency stop? — **YES, with conditions (C14, Medium)**

- **Capability status is the wrong kind of control for a compromised key.** It is product and routing
  configuration. A forged win signed with a stolen key is an **authentication** failure, and the correct
  stop is to withdraw trust in the key. Revocation does exactly that: it is immediate per request (§2),
  and it stops every event type from that key, bets included.
- **The trade-off is accepted.** Revocation strands open exposure, as disclosed. That is the documented
  "security > settlement" choice in 02 §1.3, and `security` accepts it. Stranding is recoverable through
  reconciliation and compensation. Forged payouts that have already been withdrawn are not.
- **A capability that also blocked settlement would itself be harmful.** It would strand stakes for
  routine commercial disables, while giving no protection that revocation does not give better.

**Conditions:**
1. **No real casino resolver or adapter is wired** in any environment until W2a's per-tenant revocation
   is implemented and its immediacy is tested, including the revoke-versus-in-flight concurrency test.
   W1c may land before W2a only because casino is MOCK-only until then. State this in ADR 0025's
   amendment.
2. **Revocation is single-actor, reason-coded and available to tenant and platform admins** (C2).
   Requiring four-eyes to revoke is forbidden.
3. **Runbook wording confirmed and extended:**
   - "disable capability = stop new bets";
   - "revoke credential = stop all callbacks from that key, strands open exposure";
   - revocation for compromise triggers a mandatory `casino_consistency` run, plus a `casino_statement`
     run once a real source exists, over the compromise window **before** a replacement key is
     activated;
   - identified forged wins are corrected only by compensating entries.

   **Disclosure:** that correction path depends on LEDGER-MANUAL-ADJ-4EYES-1, which is not implemented,
   so incident recovery is a go-live blocker. It is already listed in the proposal §16. It must also be
   listed as a dependency of the casino compromise runbook.
4. **Deferred, with a trigger (Low).** Revoking a provider across tenants during a vendor-side breach
   currently means one tenant-scoped revocation per tenant, or a redeploy that removes the adapter. That
   is acceptable with one tenant. Before a second tenant shares a real casino (or payments) provider, a
   platform-operator "revoke all handles for provider P" operation is needed. It iterates tenants under
   `WithTenant`, audits per tenant and is single-actor. Register it with that trigger.
5. **Recommended deferred item (Medium, before real-money casino go-live, not 10.3).** Paper 02 §1.7
   notes that the amount of a win is unbounded. That is true with or without this change: it is the
   stolen-key blast radius, not a consequence of the kill-switch change. The recommended item is a
   detective control, a win-anomaly signal in `casino_consistency` (win/stake ratio or absolute size above
   a tenant-configured threshold raises a P1), so that a compromise is noticed in minutes rather than at
   statement time. It is detection only; it never rejects a verified win, which would re-create
   stranding. Register it as `CAS-WIN-ANOMALY-1`. This is not a condition of 10.3.

### 5.2 Is a four-eyes "settlement freeze" wanted? — **NO. Do not build it.**

**Why not:**
- **No new capability.** The only legitimate reason to refuse a verified settlement is that the
  verification itself is no longer trusted, and revocation covers that case.
- **Any other freeze harms players.** It would re-create CAS-CAP-ROLLBACK-1's stranding of stakes and
  withholding of wins by configuration, which is the defect this wave removes.
- **Four-eyes on a stop control is inverted.** It contradicts the platform's own rule that turning
  something off must never need a second approver, and it would slow the response in exactly the
  incident it exists for.
- **It is not needed for holding events either.** "Accept and record, but do not post" already exists in
  the proper form: the verified-only rejection record and reconciliation (§2.6).

This matches `ledger-finance`'s recommendation. If a future requirement for a freeze appears (for
example, a regulator order), it comes back as a decision with its own reconciliation obligation. It is not
added silently.

### 5.3 Related items in the casino paper

- **E3 decline audit and the rejection record (§2.6/§2.11): acceptable.**
  - Writes are verified-only, so I1 holds.
  - The rejection record uses a fresh `WithTenant(t.ID)` with the same verified tenant id.
  - It is written only for an explicit allow-list of financial rejection error classes, never for
    `AuthError` or a malformed body.
  - It holds no raw body or signature, and it is bounded by its unique key.
  - A test must prove that an unverified caller cannot create a row.
- **HD-10.3-4 (suspended tenant): no security objection to either answer.** If the human chooses to let
  suspended tenants settle, the tenant-status gate moves after verification and applies to bets only.
  Verification, the uniform 401 and I1 stay unchanged.

---

## 6. Other findings

**KYC-REASON-BOUND-1 (Low, C16).** In addition to the 512-byte bound and control-character stripping:
- reject or replace invalid UTF-8;
- strip Unicode bidi and format controls (U+202A–U+202E, U+2066–U+2069, U+200B–U+200F), because the raw
  text is shown to staff;
- staff UIs must HTML-escape the text, and a test must prove it.

The player-facing `reason_code` taken from a closed enum is correct. HD-10.3-3's default (no provider
reason at all) is the safe default.

**The "secrets scan" gate (Low, C17).** The proposal's per-wave gates (§15) list a "secrets scan". CI has
no secret-scanning step (`.github/workflows/ci.yml`). The gate must either name the procedure (the G7-style
history scan used in the 10.2 review) or add a CI scanner. It must not be listed as if it already ran.

**Inconsistent handle-table summary (Info).** The proposal's migration table (§6) omits the `purpose`
column that paper 01 §2.1 requires (inbound and outbound never share a handle, ADR 0022 §4.2). ADR 0093
must carry `purpose`.

### 6.1 Tenant-isolation and authorization tests the waves must include

These are required in `qa` coverage for W1a and W2a, and run as the NOBYPASSRLS runtime role.

| # | Test |
|---|---|
| T1 | A tenant-A staff token cannot list, register, approve or transition tenant B's handles, whatever the path or body. The response is 403/404 with no data, and no row changes in either tenant. |
| T2 | `WithTenant(A)` sees zero rows of B. `WithoutTenant` sees zero rows. An INSERT with `tenant_id = B` under A's context fails. |
| T3 | A ref outside the caller's tenant namespace is rejected by the API. A row inserted directly with a foreign-namespace ref is refused by the resolver (`credential_integrity`). |
| T4 | Real resolver with `memory://`: an A-signed callback at B's route gives 401 in all three domains, and the full no-effect checklist holds in both tenants. |
| T5 | Revoked, expired or `not_before`-future handles give 401 on the next request. Revocation racing an in-flight request is deterministic. The outbound call after a revoke fails closed. |
| T6 | Re-registering a revoked fingerprint is refused. The same fingerprint cannot be registered for a second tenant. |
| T7 | The trigger refuses a backward status transition, an extended or cleared `not_after`, a DELETE, and an UPDATE of any other column. |
| T8 | A `KeyFromHeader` scheme with an absent key id gives `signature_missing`, and the resolver is never called in multi-row mode. |
| T9 | Statement capture: exactly the pinned handle `SELECT` runs before verification (KYC and casino). Any other statement goes red. |
| T10 | Four-eyes: self-approval is refused by the DB. An approval binds the exact content, and a changed field invalidates it. An expired approval is refused. Revocation needs no approval. |
| T11 | Request capture proves tenant A's outbound call never carries B's credential. |
| T12 | No cross-tenant or cross-fingerprint cache hit. The fingerprint is compared on a cache hit. |
| T13 | A casino handle never resolves for payments or KYC under the same `provider_id`. An `outbound_api` handle never verifies an inbound callback. |
| T14 | Synthetic guard matrix, including an absent `APP_ENV` and all other config booleans set. |

---

## 7. Human decisions versus engineering

**Human decisions (correctly identified by the proposal):**
- HD-10.3-1 (scope);
- HD-10.3-2 (`deploy/` IAM/KMS/egress, and any apply);
- HD-10.3-3 (player-facing reason wording);
- HD-10.3-4 (suspended-tenant settlement);
- O7 (raw-payload retention period);
- vendor contracts and credentials;
- production launch.

**Engineering decisions, owned by `security` and ruled in this review:**
- the point 2, 9 and 10 concurrence;
- fingerprint keying;
- four-eyes and the admin API permissions;
- the SDK dependency;
- the settlement-freeze question;
- revocation as the emergency stop.

**Disclose to the human; not decisions now:**
1. Point 3, point 10 and ADR 0022 §4.2 are now **vendor-selection constraints** (§1.4). They should
   inform contracting.
2. **One task role reads every tenant's provider secrets.** This is acceptable for the B2C tenant. For
   the first B2B tenant, especially bring-your-own-licence, the tenant's contract or regulator may require
   per-tenant isolation of credential material (IAM ABAC or a per-tenant KMS key). That becomes a
   commercial or regulatory input at onboarding, and the engineering path exists.
3. **Casino compromise recovery (forged-win correction) depends on LEDGER-MANUAL-ADJ-4EYES-1,** which is
   already a go-live blocker.

**No new human decision is required to start Stage 10.3.**

---

## 8. Conditions (numbered, with severity and binding point)

| # | Sev | Condition | Binds at |
|---|---|---|---|
| C1 | **High** | `secret_ref` is tenant-namespaced. The API rejects foreign refs and the resolver refuses them. Global unique `(domain, provider_id, purpose, fingerprint)` across tenants and statuses. | ADR 0093 (W0); W2a |
| C2 | **High** | Four-eyes on activating registration, DB-enforced, distinct principal, binding exact content, expiring. Verify-only, shortening and revocation are single-actor. Four separate permissions per §2.2. Ships in W2a (or the §2.2 fallback). | ADR 0093 (W0); W2a |
| C3 | Medium | Key-selection mode comes from `Properties().KeySelection`, never from an empty `KeyID`. `KeyFromHeader` plus absent key id gives `signature_missing`. A conformance case and a broken-scheme case cover it. | ADR 0022 pt 2 (W0); W1a/W2a |
| C4 | Medium | Overlap bounded (maximum window). `not_after` can only shrink. At most one `verify_only` per `KeyImplicit` binding. Both keys evaluated, and the matched `key_id` logged. | W0; W2a |
| C5 | Medium | Point 9: exactly one pinned, lock-free, read-only `SELECT` with an explicit tenant predicate. The capture test pins its shape. The DB-error response is independent of handle existence. | W0; W2a |
| C6 | Medium | Fingerprint keyed (HMAC with a platform key, labelled, versioned). The operator confirmation value is never persisted or logged. `versionId` is mandatory for `awssm` in staging and production. | ADR 0093 (W0); W2a/W3b |
| C7 | Medium | Store outage cannot exhaust the shared DB pool: circuit breaker or negative cache, a concurrency test, refresh-not-evict, cache key (tenant, ref, fingerprint), fingerprint compared on every resolve. | W2a |
| C8 | Medium | Outbound credentials are resolved per call and never cached in the adapter, `Authenticator` or client. A revoke-then-call test. | W2a |
| C9 | Medium | Conformance: a vendor known-answer vector per real scheme; one killing broken scheme per mandatory case; suite-generated tampering over declared auth headers; per-domain "delete the `Verify` call" mutation; a registry-driven test that every non-synthetic scheme runs the suite; a constant-time lint or AST rule. | W1a |
| C10 | Low | `Verify` returns a closed reason. `timestamp_out_of_window` only when the MAC is otherwise valid. Enum and allow-list tests updated. The response stays the uniform 401. | W0; W1a |
| C11 | Low | Point 10 with a `MaxSkew` cap (10 min unless `security` signs off). Registration-time validation of non-synthetic `Properties()` refuses startup. | W0; W1a |
| C12 | Medium | AWS SDK conditions 1–10 in §3. | W3b (and HD-10.3-2 if approved) |
| C13 | Medium | Synthetic guard: absent `APP_ENV` is treated as production for the guard and the backend allow-set; a positive `ProductionEligible` marker (or a name-independent AST scan); an all-config-flags test; the other binaries covered; `memory://` test-only. | ADR 0085 amendment (W0); W1b |
| C14 | Medium | Casino: no real casino resolver before W2a revocation is tested; revocation single-actor for tenant and platform; the runbook text of §5.1(3), including the LEDGER-MANUAL-ADJ-4EYES-1 dependency; register the cross-tenant provider revoke (trigger: second tenant on a shared provider) and `CAS-WIN-ANOMALY-1` (trigger: before real-money casino go-live). | ADR 0025 amendment (W0); W1c; registry |
| C15 | Low | Redaction, including `MarshalJSON`, on every new secret-bearing type, with a test. | W2a |
| C16 | Low | KYC reason: UTF-8 validity, bidi and format-character stripping, staff-UI escaping test. | W1d |
| C17 | Low | The "secrets scan" gate names a real procedure or adds a CI step. | Per wave |

**Accepted residuals and informational items (not conditions):**
- R-1 (Info): the compile-time `Verified` token is recommended.
- R-2 (Low): the handle-existence timing residual, same class as F-4; revisit with PAYWH-RL-1.
- F-5 (Low) is carried: KYC `error` audit dedupe with the first real KYC adapter.
- Info: the proposal §6 table must include `purpose`.
- Info: no vendor sandbox mode inside real adapters; sandbox URLs go on the production-config checklist.

**Settlement freeze:** rejected (§5.2).

### Launch-blocking items to flag to the human (via the orchestrator)

These do not block Stage 10.3. They block production launch or real-vendor go-live:
- C1 and C2 must be implemented before any non-synthetic credential is registered anywhere.
- MOCK-ADAPTER-PROD-1, including C13, before any `APP_ENV=production` deployment.
- LEDGER-MANUAL-ADJ-4EYES-1: required for recovery from a casino compromise and for reconciliation
  compensation.
- PAYWH-RL-1.
- The STAGING REQUIRED Secrets Manager drills.
- `CAS-WIN-ANOMALY-1`, recommended before real-money casino go-live.

## 9. Scope of this review

**Covered:**
- the Stage 10.3 proposal and papers 00–03 as design;
- the ADR 0022 §3 amendments proposed for W0;
- the credential, handle and resolver design;
- the SDK dependency posture;
- the synthetic-guard design;
- the casino kill-switch change and the rejection record;
- spot-checks of the current code facts cited above.

**Not covered:**
- any implementation (none exists);
- real vendor schemes (none selected);
- `deploy/` and AWS state (deliberately not touched; a teardown is in progress);
- volumetric DoS beyond C7;
- the sportsbook and bonus paths;
- a re-audit of unchanged post-verification money paths.

This is a design-level review for a development-stage platform. It is not a penetration test or a
certification audit. Approving the plan does not make any later implementation secure: each wave still
needs its own `security` review before it is marked complete (CLAUDE.md).

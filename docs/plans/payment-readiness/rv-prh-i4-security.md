# RV-PRH-I4 — Security code review of the ADR 0097 implementation (webhook admission / rate limiting)

- **Reviewer:** `security` (the ADR 0097 author; this is the §15 implementation review).
- **Subject:** PRH-I4, commits `9df81a3..3cfb9e5` (merged at `f68a37c`), checked against ADR 0097
  §3–§9 and §21 (the implementation record and condition map), and against
  `docs/plans/payment-readiness/evidence/prh-i4-mutation-kill.txt`.
- **Verdict:** **APPROVE WITH CONDITIONS.** The core mechanism is sound, and I verified it in the code:
  - bounded keys;
  - an overflow fold that never resets;
  - B1/B2 run strictly between verification and `WithTenant`;
  - B2 is held through the follow-up writes and released exactly once;
  - production cannot disable admission.

  Four conditions (C1–C4 below) must be closed before PAYWH-RL-1 is marked closed. Until then,
  ADR 0097 stays `ACCEPTED – IMPLEMENTED WITH CONDITIONS`, not `IMPLEMENTED`. C1 and C4 also
  **block registering any non-MOCK webhook adapter**, and therefore block real-money launch.
- **Scope of this review:**
  - The admission code paths: `internal/admission/**`, `internal/httpserver/webhook_admission*.go`,
    `webhook_preamble.go`, `webhook_tenant_directory.go`, the three webhook handlers,
    `middleware.go` (RL-F4), `health.go`, `server.go` wiring, `internal/config/webhook_admission.go`,
    `cmd/platform-api/{main.go,webhook_admission.go}` (including HTTP-TIMEOUTS-1),
    `internal/webhookauth/retry_semantics.go`, and the OpenAPI 429/503 contract.
  - **Not in scope:** the PROVIDER-REF-BOUND-1, migration 0099 and ADR 0095 commits that happen
    to fall inside the same range (they have their own reviews). No penetration test and no load
    test were run.
- **Environment:** a private scratch database `sec_prh_i4_rv`, created through
  `TEST_ADMIN_DATABASE_URL`, fully migrated (100 migrations), then dropped. The targeted suites
  (`TestAdmission_*`, `TestGatedReader*`, `TestRLF4*`, `TestWebhookTenantDirectory*`,
  `TestOpenAPI_WebhookRateLimit*`) pass with `-tags=integration -race`, and so do the
  `internal/admission`, `internal/config` and `internal/webhookauth` unit suites with `-race`.

## 1. Verified against the code (holds)

| Item | Result |
|---|---|
| ORD-1/ORD-2: admission runs before any DB work or body read | **Holds in the code** for all three handlers. `admitPreAuth` is the first call after the orchestrator nil check, and before `webhookPreamble`, which is the only place the body is read or the slug lookup made. The A5 `Content-Length` pre-check comes before `CheckInboundPreamble`. **No test pins this ordering**: my mutation S1 survived (§3, C3). |
| Verification before B1/B2 | Holds. `admitVerified` runs only under `if err == nil` after `VerifyCallback`, and strictly before `deps.DB.WithTenant`. |
| B1/B2 keyed only from the verified identity | Holds, with a note. Keys are `t.ID`/`providerID`, the same values `VerifyCallback` just bound into `in.TenantID`/`in.ProviderID` and verified the signature over, so they are equivalent to the `VerifiedCallback` identity. Recommendation (not a condition): read them from the `VerifiedCallback` accessors so the binding is structural and not positional. |
| Deviation: ordered explicit calls instead of a route middleware | **Equivalent for today's three routes**, and I checked each one line by line. It is **not future-proof**: nothing stops a fourth webhook route from skipping `admitPreAuth`, and RL-F4 redaction silently depends on that same call. There is no guard and no test. See C3. |
| Bounded key cardinality | Holds. `preAuthKeys` admits only a directory slug (≤ 128 bytes) × a registered provider id, and anything else collapses to one `(domain,_unknown,_unknown)` key. The unknown limiter's `maxKeys` is 1, known is `DirectoryCap*8+8`, and verified is `VerifiedMaxKeys`. `lastSeen` is a subset of the `tat` keys (a rejection implies the key is present). Bulkhead per-key counters are deleted at 0. |
| Overflow fold, no reset | Holds (`gcra.go` folds into `overflowOn` and never clears the map). Gap: `OverflowOccurred()` has no caller, so the §4.3/§7 one-time `error` log is never emitted (L2). |
| `gatedReader` reentrancy refusal | Holds. It refuses on `txscope.Held(ctx)`, then on its own context mark, before `Acquire`. The gate wraps exactly one `WithTenantReadOnly` and never the secret-store Fetcher (INV-POOL is intact). The M6 evidence is consistent with the code. |
| Per-tenant transaction cap (B2) | Holds. It is acquired before `WithTenant` with no connection held while waiting. `releaseDomainTx` is deferred inside the handler, so it is held through the payments reversal-rejection audit transaction and the casino rejection record, and released once (`sync.Once`). No admission 429/503 is written after `WithTenant`: the only later 503 is casino's pre-existing `ErrProviderUnavailable` kill-switch mapping, which is a domain rollback, not admission. The B2 global cap of 2^20 is effectively unbounded, as disclosed in §21.2. Acceptable, because the pool is the real ceiling. |
| Directory is non-authoritative | Holds. The directory is unexported and exposes only `Contains`. It is only a keying input: `GetTenantBySlug`, the active check and verification still decide. The refresh is not request-driven, and a failed refresh keeps the last snapshot. |
| Fail-safe: limiter panic → 503 | Holds for panics inside `admitPreAuth`/`admitVerified`, which have their own `recover` and write a 503. A panic inside the A4b gate path is not covered and would reach `recoverMiddleware` as a 500 (L5). |
| Config validation / production cannot disable | Holds. `Validate` is called from `config.Load()` with `GuardEnvironment()`, so a missing `APP_ENV` counts as production, and `Enabled=false` is refused there. Every §9.3 rule is enforced, and the `main.go` mapping copies every field, including `Enabled`. Minor: disabling is allowed in *any* explicit non-production environment (for example staging), whereas the ADR says "only when test-support routes are enabled" (L4). |
| 429/503 + Retry-After by adapter semantics | B1 correctly switches to 503 when the adapter declares `Retries429=false`. My mutation S3 was killed by T14. Registration fails closed for a non-synthetic adapter that has no declaration. **Gaps:** a declaration of "retries neither 429 nor 503" is accepted (C4), and A4b 503s carry no `Retry-After` (C2). |
| RL-F4 (redacting the webhook path from access and panic logs) | Holds for the matched routes. `admitPreAuth` sets `RequestState.LogPath = r.Pattern` through the shared pointer, which survives `otelhttp`'s `r.WithContext` clone, and both the access line and the panic line use `logPathFor`. Residual (L3): the path is still logged raw on the orchestrator-nil 503 branch, which returns before `admitPreAuth`, and on unmatched `/v1/webhooks/...` paths (404/405). Both are bounded by `MaxHeaderBytes`. |
| HTTP-TIMEOUTS-1 | `ReadHeaderTimeout` 5 s, `ReadTimeout` 15 s (≥ `BodyReadTimeout` 10 s), `WriteTimeout` 60 s (well above `DomainWait` 2 s plus processing), `IdleTimeout` 120 s. Acceptable. |
| No secrets, bodies or signatures in logs | Holds. `webhook_admission_rejected` carries only allow-listed fields. `tenant_key` is a directory slug or `_unknown`, never a raw unknown path value. `provider_key` is a registered id or `_unknown`. There are no headers, body, signature or query string. The suppressor's key space is bounded like the limiter's. |
| OpenAPI generic bodies | Holds: 429/503 are documented on all three paths with generic bodies (T18). The documented `Retry-After` for 503 is not honoured on the A4b path (C2). |

## 2. Findings

### C1 — High (blocks non-MOCK adapters and launch): an A4b gate rejection inside credential resolution becomes a uniform **401**, not a 503

Code path:
- `gatedReader.WithTenantReadOnly` returns `errDBGateUnavailable`.
- The real resolver, `providercred.Resolver.Resolve` (`internal/providercred/resolver.go`, around line 141), replaces any reader error with `webhookauth.ErrCredentialUnavailable`.
- `webhookauth.ResolveCredentials` then folds that into `&AuthError{Reason: credential_unavailable}`, which is the uniform 401.

Consequences:
- The handlers' `errors.Is(err, errDBGateUnavailable)` check only fires for payments' first read (`ProviderAcceptsWebhook`).
- For casino and KYC it never fires at all.

**Failure scenario:** an attacker floods tenant B's own valid URL (residual R1), or several tenants are flooded at once (R3). B's A4b share (2) or the global gate (3) is saturated. A correctly signed callback from B's real provider passes A3/A4a, waits the 100 ms `DBGateWait` on the credential-handle read, and gets **401 "callback rejected"**. ADR 0097 §6.1 forbids 401 for limiting because vendors may treat 4xx as terminal, so the event (a deposit, a win, a rollback) is silently dropped from the push channel. This is the LF-C1 loss class the ADR exists to prevent.

Not caught today because every test uses MOCK resolvers, which ignore the reader.

**Required:**
- Propagate gate unavailability as a distinct, non-`AuthError` outcome through `Resolve` → `ResolveCredentials` → `VerifyCallback`. For example, add a `webhookauth.ErrReaderCapacity` sentinel and return it unwrapped from the domain `VerifyCallback`.
- Map it to 503 + `Retry-After` in all three handlers.
- Add a test with the real `providercred.Resolver` and a saturated gate that asserts 503, not 401, for casino and KYC as well as payments.
- The response must still be independent of signature validity (T15), which it naturally is, because resolution happens before verification.

### C2 — Medium: A4b 503s have no `Retry-After` and no admission log line

`webhook_preamble.go` (slug lookup) and the three handlers answer `errDBGateUnavailable` with a bare `apierror.Write(... CodeUnavailable ...)`.

Gaps against the ADR and the published contract:
- There is no `Retry-After: 1`, contrary to §6.1 and to the OpenAPI note that says the header is present for admission causes.
- There is no `webhook_admission_rejected` line with `tier=db_gate`, because `gatedReader.onRejected` is never set.
- The tier is therefore invisible to operators.

**Required:** route every A4b rejection through `writeAdmissionRejection` and `logRejected`. Add an HTTP-level assertion of the header.

### C3 — Medium: ORD-1/ORD-2 ordering is unpinned, and nothing forces future webhook routes through admission

- My mutation **S1** moved `admitPreAuth` after `webhookPreamble` in the payments handler, so the body is read and the slug looked up before admission. It **SURVIVED** the full admission suite plus the webhook suites.
- The implementer's explicit-call deviation is equivalent today, but it has no structural guard. A new webhook route that forgets `admitPreAuth` would bypass A2/A3/A4a and A4b-by-key (it would pass `deps.DB` rather than a `gatedReader`), and it would also lose RL-F4.

**Required:**
1. T10 (below) kills S1 for all three domains.
2. A route-completeness guard. My preference is a runtime fail-closed check: `admitPreAuth` stamps the request context, and `webhookPreamble` answers 503 if admission is enabled and the stamp is missing. That catches a new route at its first test. An AST/registration test works as well: every `mux.HandleFunc` pattern under `/v1/webhooks/` must use a handler whose body calls `admitPreAuth` before `webhookPreamble`.

Either option needs its own mutation (delete the call in one handler → the guard test fails).

### C4 — Medium (blocks non-MOCK adapters): `RequireRetrySemantics` accepts an adapter that retries neither 429 nor 503

ADR §6.3 says such an adapter must not be registered without LF-C1 option (b), meaning reconciliation with a P1 alert. Today `{Retries429:false, Retries503:false}` with `declared=true` passes. The admission layer would then answer B1 with 503 and A4a/A4b/B2 with 503, and that adapter treats all of them as terminal, so events are lost.

**Required:**
- Reject `!Retries503` at registration unless an explicit, audited LF-C1(b) attestation is present. The simplest version is an unconditional rejection until PRH-I5 reconciliation exists.
- Add a unit test for it.
- Related design note from the ADR author, to fix alongside: A3 always answers 429. For a provider declared `Retries429=false`, A3 should use the same adapter-declared status. That is keyed on the URL provider id, which is public, and is not a signature oracle.

### Low / Info (non-blocking; track)

- **L1 — T9 test is vacuous and the docs misstate the behaviour.**
  - `main.go`, `LoadDirectory`'s doc comment and the `Contains` doc comment all say "every webhook route answers 503" when the directory is not loaded. The code does not do that: it keys everything as `_unknown`, which is bounded, but legitimate traffic from all tenants then shares one 2/s bucket per domain.
  - `TestAdmission_T9_DirectoryNeverLoaded_FailsClosed` asserts only `!= 200`, and its payload names no intent, so it would pass whatever admission does.
  - Fix: either implement the §7 503 when `!Loaded()` (preferred; it takes one line in `admitPreAuth`) or record the deviation in the ADR. In both cases, assert the exact status.
  - Rated Low only because the fallback stays bounded and `/readyz` keeps the instance out of rotation.
- **L2 — no overflow log.** The GCRA overflow `error` log (§4.3/§7) is never emitted. §8 metrics are disclosed as NOT IMPLEMENTED (PRH-I4-METRICS-1). Both must land before the §8 alerts are relied on.
- **L3 — RL-F4 residuals.** The raw path is still logged on the orchestrator-nil branch (set `LogPath` before that check) and on unmatched `/v1/webhooks/*` paths.
- **L4 — disabling outside production.** `WEBHOOK_ADMISSION_ENABLED=false` is accepted in explicit staging, but §7 says "only when test-support routes are enabled". Tie it to `TestSupportRoutesEnabled()`.
- **L5 — panic in the A4b path.** A panic inside the A4b gate path (`gatedTenantLookup`/`gatedReader`) is a 500 from `recoverMiddleware`, not an admission 503.
- **I1 — A4a per-key share.** The share is keyed per `(tenant, provider)` rather than per tenantKey as §3/§9.1 say, so a tenant with P providers can hold P×16 slots, still under the 64 global cap. Acceptable; align the ADR text or the key.
- **I2 — operator override on `_unknown`.** An override with `provider_id: "_unknown"` passes the charset check and raises the unknown bucket. This is operator-only config. Reject reserved ids in `Validate`.
- **I3 — dead code.** `newWebhookAdmission` line 76 (`orElse(...)`) is overwritten on line 86.
- **I4 — `Bulkhead.Acquire` ignores context cancellation.** It is bounded by 100 ms / 2 s, so this is acceptable.

## 3. Mutation spot-checks (security-run; each reverted, `git diff` clean afterwards)

| # | Mutation | Tests run | Result |
|---|---|---|---|
| S1 | `deposit_handlers.go`: move the `admitPreAuth` block to after `webhookPreamble` (ORD-1/ORD-2 violated) | `-run 'TestAdmission_\|TestRLF4\|TestGatedReader\|TestPointNine\|Webhook'` (integration) | **SURVIVED.** Pins required: C3, T10. |
| S2 | `gatedTenantLookup`: call `identity.GetTenantBySlug` ungated (A4b removed from the slug lookup) | same, plus `internal/admission` | **SURVIVED.** Expected while T4 is open; T4 must kill it. |
| S3 | `admitVerified`: disable the 429→503 switch for `Retries429=false` adapters | `-run TestAdmission_` | **KILLED** by T14 ("a no-429-retry adapter's exhausted B1 bucket must answer 503, not 429"). |

The implementer's M1–M6 evidence is consistent with the code as it stands. The disclosed M3/T2 gap and the T2b fix are acknowledged.

## 4. Open items the implementer is closing now: what `security` requires

- **T4 (pool.Stat).**
  - Use a real pool with N = 10 and a barrier inside the gated section.
  - Send ≥ 200 concurrent unauthenticated requests with a valid slug.
  - Assert that `pool.Stat().AcquiredConns` attributable to webhooks is ≤ `W_db`, that an unrelated `pool.Acquire` succeeds without waiting on the barrier, and that requests over the cap get 503 and execute zero statements.
  - It must kill **S2** and also a `gatedReader` bypass (passing `deps.DB` to `VerifyCallback`).
- **T10 (no DB work and no body read before admission).**
  - Use statement capture (a pool tracer or counter) and an instrumented body reader.
  - Requests rejected at A2, A3 and A4a must produce zero statements and zero body bytes, for **each** of the three domains.
  - The point-9 statement set for admitted requests must be unchanged.
  - It must kill **S1** in each handler.
- **T6 financial matrix (ledger-finance C2, payments 1/3).**
  - Scenarios: T6a deposit, T6b casino win, T6c bet → rollback reordering (tombstone), plus reversal/refund and win-before-bet.
  - Each scenario is driven through **both** a verified-tier (B1/B2) limit and a pre-auth (A3) limit.
  - Required assertions:
    - the limited attempt writes zero ledger, tombstone, audit, rejection-record or intent-transition rows;
    - after advancing the fake clock past `Retry-After`, the redelivery gets 200 with exactly one posting;
    - a third delivery is an idempotent duplicate;
    - `SUM(debits)=SUM(credits)`, and the projection equals the rebuild.
  - It must kill a "B1/B2 moved inside `WithTenant`" mutation directly, not only through T2b.
- **T11 (virtual clock).** A virtualized read-deadline seam is preferred. If a real `ResponseController` deadline is used, this is my ruling: it is allowed in the **main lane** only if the test asserts that the A4a slot is released and the handler returns (upper bound ≥ 100× the test deadline) and makes no lower-bound or latency claim. Anything stricter needs the isolated timing lane and a separate ruling.
- **C1/C4 regression tests** as specified in §2. Closing C1–C4 requires a re-verification by `security`, scoped to those diffs only.

## 5. Launch-relevance summary

- C1 and C4 must be closed before any non-MOCK webhook adapter is registered.
- WEBHOOK-EDGE-1 (R1) is still required before real-money launch.
- Nothing in this review asserts that the control is "secure" beyond the scope listed above.

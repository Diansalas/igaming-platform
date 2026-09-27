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

## 6. Re-verification (round 3, scoped to C1–C4, Lows, T4/T6/T10/T11)

- **Reviewer:** `security`. **Subject:** round 3 as merged at `aefad6f`: `f3308cd`, `923f205`,
  `bb0a375`, `82253a1`, `304a925`, `7fc1daf`, `55b719b`, `46e08be`, `ef473e6`. ADR 0097 §21.7/§21.8,
  evidence M7–M11, registry PRH-I4-SECREVIEW-1. The admission code is byte-identical at the current
  branch head (`5d900c0`): `git diff aefad6f HEAD -- internal/httpserver internal/webhookauth
  internal/providercred` is empty.
- **Environment:** a private scratch DB `sec_prh_i4_rv3`, created through `TEST_ADMIN_DATABASE_URL`
  and owned by `igaming`, with all 102 migrations and the `deploy/init-app-role.sql` runtime grants.
  It was dropped afterwards. I ran the mutations in a private `git worktree` at `aefad6f`, so they
  could not collide with the concurrent merges into the shared tree. Every mutation was reverted,
  `git diff` was clean after each one, and the worktree was removed.
- **Not in scope:** anything outside the round-3 diffs listed above. No load test and no
  penetration test were run.

### 6.1 Baseline

The targeted suites were run at `aefad6f` with `-tags=integration`: `TestAdmission_*`,
`TestWebhookRouteGuard*`, `TestGatedReader*`, `TestRLF4*`, `TestWebhookTenantDirectory*` and
`TestOpenAPI_WebhookRateLimit*`, plus the unit suites for `providercred`, `webhookauth`,
`admission` and `config`. **Two tests are RED on the merged branch:**
`TestAdmission_T6c_CasinoBetLimitedThenRollbackReorder` and `TestAdmission_T6d_CasinoWinBeforeBet`.
Both fail in setup with "expected 201 launching the game, got 503", and the log shows
`casino_launch_failed reason=credential_unavailable`.

Both pass at the round-3 commit `ef473e6` itself. This is a semantic merge break with the PRH-I2
casino launch-path rework, not a defect in the admission code. However:
- T6c/T6d currently prove nothing;
- M11, whose kill was recorded by T6c, cannot be reproduced on the merged branch, because T6c now
  fails before reaching the assertion that M11 is supposed to trip.

### 6.2 Mutation results

| # | Mutation | Result |
|---|---|---|
| S1 (payments, re-run) | `admitPreAuth` block moved after `webhookPreamble` in `deposit_handlers.go` | **KILLED** by T10/payments ("body was read before admission rejected…") and also by T11 |
| S1 (kyc, new domain) | same mutation in `kyc_admin_handlers.go` | **KILLED** by T10/kyc |
| S2 (re-run) | gate removed from `gatedGetTenantBySlug` | **KILLED** by `TestAdmission_T4_GatedGetTenantBySlug_RespectsSaturation` |
| N1 | `newGatedReader` not used in the payments **and** casino handlers, so `deps.DB` is passed to `VerifyCallback` (a gatedReader bypass; §4's T4 said it "must kill") | **SURVIVED** the whole admission suite, including the real-pool T4 test. That test drives `gatedReader` directly, never through the HTTP handlers. |
| N2 | a fourth webhook route (`POST /v1/webhooks/sportsbook/…`) whose handler calls `webhookPreamble` with no `admitPreAuth` | **SURVIVED** `TestWebhookRouteGuard_*`. The guard checks only three hard-coded constructor names and never enumerates route registrations. |
| N3 | the casino handler's `authErr.Reason == ReasonAdmissionUnavailable` → 503 branch deleted (the C1 fix at the handler hop) | **SURVIVED** everything: the admission suite, including all three `TestAdmission_C1_*`, plus the `providercred` and `webhookauth` unit tests |
| N4 | the `ErrTenantReaderUnavailable` case deleted from `webhookauth.reasonForResolveError`, so the error falls into `default → ReasonCredentialUnavailable` (**this re-creates the C1 uniform-401 defect exactly**) | **SURVIVED** everything: the admission suite and the unit suites for `webhookauth`, `providercred`, `payments`, `casino` and `kyc` |
| N5 | the A3 adapter-declared 503 switch disabled (`false && declared && !allows`) | **SURVIVED**. There is no A3 test for a `Retries429=false` provider; T14 covers B1 only. |

N3 and N4 ran alongside the pre-existing T6c/T6d red. No other test failed, which is what I counted
as "survived".

### 6.3 Per-condition ruling

- **C1: OPEN.** The fix is correct, verified by reading the code along the whole path:
  - `gatedReader` returns `webhookauth.ErrTenantReaderUnavailable` (`errDBGateUnavailable` is now the
    same value);
  - `providercred.Resolver.Resolve` checks for it before the fold and returns it unwrapped;
  - `reasonForResolveError` maps it to `ReasonAdmissionUnavailable`;
  - `ResolveAndSeal`, `resolveAndVerify` and each domain's `VerifyCallback` pass the `*AuthError`
    through unchanged;
  - each handler's `AuthError` branch answers 503 with `Retry-After` before the uniform 401;
  - payments' first read hits the bare `errors.Is` branch;
  - B1/B2 and `WithTenant` are skipped because `err != nil`;
  - `recordCasinoCallbackRejection` does nothing for an `AuthError`.

  **Ruling on the implementer's disclosure:** `resolver_gate_unavailable_test.go` on its own is
  **not sufficient**. It pins one of four hops (Resolve), whereas N3 and N4 show that the other two
  code hops can each be deleted in one line with every test green. N4 in particular silently
  restores the exact High-severity 401. §2 C1 required a test with the real resolver and a
  saturated gate, per domain, asserting 503 rather than 401. The existing `TestAdmission_C1_*`
  tests never reach the resolver, so that requirement is unmet.

  **Required to close:**
  - (a) An **isolating HTTP-level test per domain** (payments, casino, kyc). It needs a test seam on
    the A4b gate, for example an acquire hook or counter that admits the first *k* acquisitions for
    a key and refuses the next one: k=1 for casino/kyc (the slug lookup), k=2 for payments (slug
    lookup plus `ProviderAcceptsWebhook`). The test must use the real `providercred.Resolver`, and
    must assert:
    - 503, not 401;
    - a `Retry-After` header;
    - one `webhook_admission_rejected tier=db_gate` line;
    - zero rows.

    It must kill N3 (in each domain) and N4.
  - (b) A table-driven unit test on `reasonForResolveError` / `ResolveCredentials`, in both key
    selections, that also kills N4 cheaply.
  - The same gate seam closes T4's N1 gap (below).

- **C2: CLOSED for the code.** Every A4b 503 goes through `writeDBGateUnavailable`, which writes
  `Retry-After: 1` and a `db_gate` line. The two sites are the slug lookup in `webhook_preamble.go`
  and the three handlers' `errors.Is` and `ReasonAdmissionUnavailable` branches. The HTTP-level
  `Retry-After` assertion exists only for the slug-lookup site. The credential-resolution site
  becomes pinned by C1(a).
  - Residual (Low, L7): no test asserts the `db_gate` log line.
  - Info: on this tier the log line has `tenant_key=""`, so suppression is per
    (domain, provider), not per tenant. It is bounded, and `provider_key` is always a registered id
    here, because the preamble has already refused unregistered providers.

- **C3: OPEN, narrowed.**
  - Part 1 (T10 kills S1) is **met**: S1 was killed in payments and kyc, and M10 covers the
    remaining domain the same way.
  - Part 2 (route completeness) is **not met**. The AST guard scans the three named constructors,
    so it pins "these three handlers keep calling `admitPreAuth`", but it does not force a *new*
    webhook route through admission, which was the point of the condition. N2 survives.

  **Required:** make the guard enumerate route registrations. Scan the package's non-test files for
  `HandleFunc`/`Handle` calls whose pattern literal contains `/v1/webhooks/`. Require the handler
  argument to be a call to a constructor that is itself checked for `admitPreAuth` and
  `markWebhookRouteForLogging`, and fail on any pattern it cannot resolve. Record a mutation that
  adds an unguarded fourth route. The runtime stamp alternative from §2 also closes this.

- **C4: CLOSED.**
  - `RequireRetrySemantics` refuses `{Retries429:false, Retries503:false}`, with a unit test.
  - All three domains' orchestrator constructors call `MustRequireRetrySemantics`, and I found no
    registration path after construction.
  - The ADR §6.1/§6.3 amendments match the code: A3 uses the adapter-declared status, keyed on
    `providerKey`, which is a registered id or `_unknown`. The URL provider id is public, so this is
    not a signature oracle.
  - New Low **L6**: the A3 half has no test (N5 survived). Add a T14-style A3 test that kills N5.
    This must land **before any non-MOCK adapter declaring `Retries429=false` is registered**.

- **Lows:**
  - **L1: CLOSED.** There is an explicit `!Loaded()` → 503 gate in `admitPreAuth`, and T9 asserts
    503.
  - **L2: CLOSED for the log line.** `webhook_admission_limiter_overflow` is emitted and
    `TestAdmission_OverflowLogged` covers it. PRH-I4-METRICS-1 is still open.
  - **L3: CLOSED for the orchestrator-nil branch.** `markWebhookRouteForLogging` is the first
    statement in each handler, and `TestRLF4_OrchestratorDisabled_StillRedactsPath` covers it. The
    unmatched `/v1/webhooks/*` 404/405 residual is **still open**, as Low and non-blocking, bounded
    by `MaxHeaderBytes`. §21.8's "both leftovers fixed" overstates this.
  - **L4, L5, I1–I4:** not addressed. They were never blocking, but they are **not registered** in
    the task registry either. Register them, or record them in the ADR, so they are tracked rather
    than dropped.

- **T4: OPEN.**
  - What is met: the real 10-connection pool, 200 concurrent callers, the barrier, the concurrent
    holders never exceeding the cap, an unrelated `Acquire` that doesn't wait, zero statements for
    rejected callers, and the S2 kill.
  - What is not met: the test drives `gatedReader` directly instead of HTTP requests with a valid
    slug. It therefore does not kill a gatedReader bypass (N1 survived), which §4 required
    explicitly.
  - **Required:** the C1(a) gate seam, plus an assertion that a request reaching `VerifyCallback`
    makes at least one A4b acquisition on its key after the slug lookup (or an equivalent). It must
    kill N1 in each domain.

- **T10: CLOSED, with an accepted deviation.** Statement capture and A2/A4a variants were not
  implemented. I accept the zero-body-byte canary as sufficient because:
  - all three tiers run inside the single `admitPreAuth` call, so ordering is a property of the call
    site, not of the tier;
  - the only pre-admission DB path, the slug lookup, sits inside `webhookPreamble` after the body
    read.

  S1 is killed per domain.

- **T6: OPEN.**
  - The matrix exists (T6a–f) and asserts zero rows on a limited attempt, exactly one posting on
    redelivery, idempotent duplicates, `SUM(debits)=SUM(credits)`, and a clean
    `RunLedgerVsProjection`.
  - However, T6c/T6d are **red on the merged branch** (§6.1), so the casino half of the matrix and
    M11 are currently unproven.
  - The §4 requirement to kill a "B1/B2 moved inside `WithTenant`" mutation directly has no
    evidence entry in M7–M11. It is still covered only indirectly, through T2b.

  **Required:**
  - fix the T6c/T6d harness for the PRH-I2 launch path, then re-run M11 on the merged branch;
  - record the B1/B2-inside-`WithTenant` mutation against the T6 matrix.

- **T11: CLOSED.** The virtualized deadline seam (`armBodyReadDeadline`) is the preferred option
  under my §4 ruling. It asserts that the A4a slot is held while the read is pending and released
  at the deadline. It makes no latency claim, so it stays in the main lane.

### 6.4 Final verdict

**APPROVE WITH CONDITIONS: still not closed.**

- **Closed:** C2, C4, L1, L2, L3 (orchestrator-nil), T10, T11.
- **Open:**
  - **C1** (isolating test; N3/N4 survive);
  - **C3** (route-completeness guard; N2 survives);
  - **T4** (gatedReader-bypass kill; N1 survives);
  - **T6** (T6c/T6d red on the merged branch; the B1/B2-inside-`WithTenant` mutation is not
    evidenced).
- **New:**
  - **L6** gates any `Retries429=false` adapter;
  - **L7**: add a `db_gate` log assertion;
  - register L3's residual, L4, L5 and I1–I4.

Consequences:
- PAYWH-RL-1 and PRH-I4-SECREVIEW-1 stay open.
- ADR 0097 stays `ACCEPTED – IMPLEMENTED WITH CONDITIONS`.
- The registry's PRH-I4 status "all C1–C4 conditions and Lows closed" is inaccurate and should be
  corrected to reflect this section.
- **C1 still blocks registering any non-MOCK webhook adapter, and therefore real-money launch.**
  The production code is correct today, but a one-line regression back to the High-severity 401 is
  undetected. WEBHOOK-EDGE-1 (R1) is still required before real-money launch.

The next re-verification can be scoped to:
- the C1(a)/(b) tests and their shared gate seam (which also closes T4);
- the C3 guard extension;
- the T6c/T6d fix.

I will re-run N1–N5 and M11 against them.

## 7. Re-verification #2 (round 4)

- **Reviewer:** `security`. **Subject:** round 4, commit `0f6310c`, as merged at `68f1470`
  (`claude/focused-wright-jw88w9`). ADR 0097 §21.9, evidence M12–M17.
- **Environment:** a private scratch DB `sec_prh_i4_rv4`, created through
  `TEST_ADMIN_DATABASE_URL` and owned by `igaming`, with all 104 migrations and the
  `deploy/init-app-role.sql` grants. The roles already existed, so the script's `CREATE ROLE`
  was refused as expected. The DB was dropped afterwards. I ran the mutations in a private detached
  `git worktree` at `68f1470`. Every mutation was reverted with `git checkout -- .` (plus removal of
  the one added file), `git status --short` was empty after each one, and the worktree was removed.
  I did not use the shared test DB.
- **Not in scope:** anything outside the round-4 diff. That includes the unrelated KYC and
  withdrawal changes merged between `0f6310c` and `68f1470`. I did read the KYC webhook handler
  diff and confirmed it does not touch admission. No load test and no penetration test were run.

### 7.1 Production path (item 1)

The only non-test change in round 4 is in `internal/httpserver/webhook_admission.go`: 16
insertions and 3 deletions. It changes the type of `webhookAdmissionRuntime.dbGate`, of
`gatedReader.gate` and of `gatedGetTenantBySlug`'s `gate` parameter from `*admission.Bulkhead` to
the unexported interface `dbGateAcquirer`. The interface has the identical `Acquire` signature.

- **Production wiring is unchanged.** The only non-test assignment is
  `dbGate: admission.NewBulkhead(settings.DBGateGlobal)` in `newWebhookAdmission`, and
  `NewBulkhead` never returns nil.
- **Nothing outside the package can substitute the gate.** The field and the interface are both
  unexported, and the exported `WebhookAdmissionRuntime` wraps `rt` in an unexported field. Its
  methods (`LoadDirectory`, `RunDirectoryRefresh`, `DirectoryReady`) expose neither the gate nor a
  setter.
- **The only writes to `rt.dbGate` outside the constructor are in tests.** All three are in
  `webhook_admission_c1_isolating_integration_test.go`, which has the `integration` build tag.
- **No behavioural change.** The call sites and the semantics are the same; dispatching through
  the interface adds no path that skips the gate.

**Verdict: no bypass introduced.** The baseline suite is green at `68f1470`: 40 PASS and 0 SKIP
across the admission, route-guard, gatedReader, RLF4 and directory tests, plus the unit suites for
`webhookauth`, `providercred`, `admission` and `config`. T6c and T6d now pass, so the §6.1 red is
fixed.

### 7.2 Mutation results (item 2), all security-run

| # | Mutation | Result |
|---|---|---|
| N1-payments | `newGatedReader` replaced by bare `deps.DB` in `deposit_handlers.go` | **KILLED** by `TestAdmission_C1a_Payments…` (401 instead of 503) |
| N1-casino | same, in `casino_handlers.go` | **KILLED** by `TestAdmission_C1a_Casino…` |
| N1-kyc (new) | same, in `kyc_admin_handlers.go` | **KILLED** by `TestAdmission_C1a_KYC…` |
| N3-payments (new) | the handler's `ReasonAdmissionUnavailable` branch disabled | **KILLED** by `C1a_Payments`. This confirms that its 503 comes from the credential-resolution hop (call 3), not from an earlier gated read. |
| N3-casino | same, in the casino handler | **KILLED** by `C1a_Casino` |
| N3-kyc (new) | same, in the KYC handler | **KILLED** by `C1a_KYC` |
| N4 | the `ErrTenantReaderUnavailable` case removed from `reasonForResolveError` | **KILLED** by `TestReasonForResolveError_AdmissionUnavailable` and `TestResolveCredentials_AdmissionUnavailable_BothKeySelections` (both key selections) |
| N5 | `false && declared && !allows` in the A3 branch | **KILLED** by `TestAdmission_T14b_A3AdapterDeclaredStatus_No429Retry` |
| N2 | a fourth route with a literal pattern (`POST /v1/webhooks/sportsbook/…`) whose handler calls `webhookPreamble` without `admitPreAuth` | **KILLED** by `TestWebhookRouteGuard_…` (both required calls reported missing) |
| **N2b** (new) | the same fourth route, but its pattern is a package `const` (`mux.HandleFunc(secSportsbookPattern, newSecSportsbookHandler(deps))`) | **SURVIVED.** The guard only considers a first argument that is an `*ast.BasicLit` and silently skips anything else. |
| **N2c** (new) | the same fourth route, registered through a helper `secRoute(mux, "POST /v1/webhooks/sportsbook/…", h)` that calls `m.HandleFunc(p, h)` | **SURVIVED**, for the same reason: the helper's inner `HandleFunc` has a non-literal pattern. |
| B1-payments (M12 re-run) | `admitVerified` moved inside the `deps.DB.WithTenant` closure in `deposit_handlers.go` | **KILLED**, but only by T6g ("delta (3) was not strictly less than … (3)") |
| **B1-casino** (new) | the same move in `casino_handlers.go` | **SURVIVED** the whole `TestAdmission_*` suite, including T6c/T6d and T2b |
| **B1-kyc** (new) | the same move in `kyc_admin_handlers.go` | **SURVIVED** `TestAdmission_*` plus every KYC-named integration test in the package |

### 7.3 Route-discovery evasion (item 3)

The rewritten guard does discover routes structurally, and it fails closed on an unresolvable
**handler**, whether that handler is a variable, a method value, an `http.HandlerFunc(...)`
conversion or a wrapper from another package. It does **not** fail closed on an unresolvable
**pattern**, which is what my §6.3 requirement said: "fail on any pattern it cannot resolve". N2b and
N2c show that a const or a registration helper is enough to evade it. Both are ordinary Go style,
not contrived.

Fixing this costs nothing today. All 152 `Handle`/`HandleFunc` calls in the package's non-test files
use a string-literal first argument. The fix:
- fail the guard on **any** `Handle`/`HandleFunc` call in the package whose first argument is not a
  `BasicLit`. That kills both N2b and N2c.
- Optionally, also flag any other function that takes an `*http.ServeMux` together with a pattern
  parameter.

The package-local scope is acceptable: the mux is not exposed outside `httpserver`.

### 7.4 T6g soundness (item 4)

- **Not flaky.** The test passed 60 times in a row (`-count=30`, twice) on the private pool. That
  pool is `phasecapture.Pool10`, private to the test. Nothing in the test starts
  `RunDirectoryRefresh`, and the pgxpool health check (60 s period) does not run within the
  test's window.
- **Not vacuous today.** At baseline the admitted delta is 3 and the limited delta is 2. The
  B1-payments mutation raises the limited delta to 3, and the test kills it.
- **Info I5, fragile.** The test asserts only `limited < admitted`, and the margin is exactly one
  acquisition. If a future change adds an acquisition to the admitted path only (for example a
  post-commit read), the margin grows and the mutation survives silently. Pin exact expected
  deltas, or assert `limited == admitted - 1`, so that drift fails loudly.
- **Scope gap, which is the T6 finding below.** T6g covers payments only. The same ordering
  regression in casino or KYC is undetected (§7.2).

### 7.5 Per-condition ruling

- **C1: CLOSED.**
  - C1(a) has isolating HTTP tests for all three domains. They use the real
    `providercred.Resolver` and a call-counting gate, and assert 503, `Retry-After`, a `db_gate`
    log line and zero ledger rows.
  - C1(b) has the unit test in both key selections.
  - N3 (all three domains) and N4 are killed.
  - **C1 no longer blocks registering a non-MOCK webhook adapter.**
  - Info I6: `countingGate` ignores the key. It would not catch a regression that gates credential
    resolution on the *wrong* A4b key.
  - Info I7: for KYC, the zero-ledger-rows assertion is vacuous, because KYC never writes ledger
    rows. The status and log assertions carry that test.
  - Neither I6 nor I7 blocks closure.
- **C2 / L7: CLOSED.** The `db_gate` log line is now asserted, in `assertC1Isolated`.
- **T4: CLOSED.** N1 is killed in all three domains through the HTTP handlers.
- **L6 / N5: CLOSED.** Killed by T14b.
- **C3: OPEN, narrowed to Low (L8).** N2 is killed. The N2b/N2c evasions survive, contrary to the
  explicit fail-closed-on-unresolvable-pattern requirement. The one-line fix is in §7.3.
- **T6: OPEN, narrowed.** T6c/T6d are green, and M11 is valid again. The B1-inside-`WithTenant`
  mutation is killed for payments only; casino and KYC survive. My §4 T6 requirement named the
  casino scenarios explicitly.
  - Impact of the uncaught regression: a limited *verified* callback would take a pool connection
    and open (then roll back) a domain transaction before its 429/503. That defeats ORD-3/ORD-4 and
    B1's purpose of protecting the pool from a provider retry storm.
  - It needs valid provider signatures, so it is not an unauthenticated attack path.
  - Severity: **Medium** as a regression-detection gap. No defect exists today; I verified by
    reading all three handlers that `admitVerified` precedes `WithTenant`.
  - **Required:** extend T6g (table-driven by domain) to casino and KYC, and kill the B1-casino and
    B1-kyc mutations.
- **Unchanged from §6:**
  - L3 residual (unmatched `/v1/webhooks/*` 404/405 path) stays Low, now registered;
  - L4, L5 and I1–I4 are registered, as §6 asked;
  - PRH-I4-METRICS-1 is still open.

### 7.6 Verdict

**APPROVE WITH CONDITIONS.** The High finding C1 is closed with real evidence, and the production
code is correct and unchanged in behaviour. Two test-completeness conditions remain. Neither is a
present defect. They must be closed before PRH-I4 / PRH-I4-SECREVIEW-1 are marked closed, and before
ADR 0097 moves off `IMPLEMENTED WITH CONDITIONS`:

1. **T6 (Medium):** a per-domain T6g covering casino and KYC, killing B1-casino and B1-kyc.
2. **C3 / L8 (Low):** the route guard must fail on any non-literal `Handle`/`HandleFunc` pattern,
   killing N2b and N2c.

Info items I5–I7 are recommended but not required.

Launch relevance:
- C1 no longer blocks non-MOCK webhook adapters.
- Conditions 1 and 2 do not block launch on their own, but they must be closed before real-money
  launch so that a regression in these controls is detectable.
- WEBHOOK-EDGE-1 (R1) is still required before real-money launch.
- The claim in ADR §21.9 and the registry that T6 and C3 are "closed" is inaccurate to the extent
  described above, and should be corrected.

The next re-verification can be limited to those two test diffs. I will re-run B1-casino, B1-kyc,
N2b and N2c.

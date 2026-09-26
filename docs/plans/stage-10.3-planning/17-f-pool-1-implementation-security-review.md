# 17 — F-POOL-1 implementation: security review (ADR 0094, conditions C1–C11)

- **Reviewer:** `security`. **Date:** 2026-09-26.
- **Branch / HEAD:** `claude/focused-wright-jw88w9` at `0cbb574`.
- **Diff reviewed:** `git diff bc23231..0cbb574 -- ':!docs'`, restricted to the F-POOL-1 commits:
  `7773649`, `4779958`, `8d973ef`, `18a57b8`, `2354475`, `1c5fb7e` and `f85c0b8` (the ADR record).
  The CODE-HYGIENE merge `4ae9bb4` was reviewed only for hygiene F-2 (§5 below).
- **Method:**
  - I checked each condition against the code, not against the ADR's implementation record.
  - I ran the timing lane alone and the new main-lane tests (§3).
  - I ran 3 throwaway mutations of my own. Each was reverted, and `git status` was clean afterwards.
  - I made no code changes. I committed nothing.

## Verdict: F-POOL-1 **CLOSED WITH CONDITIONS**

The root cause is fixed. No production path holds a pooled connection across a secret-store call
or across any wait for one:
- Every store-reaching resolver method now takes a `TenantReader` instead of a `pgx.Tx`.
- The handle read commits in its own `READ ONLY` transaction.
- The domain transaction opens only after verification.
- The `txscope` guard is present at every entry point.

The pool-10 variant of the kept test passes alone, and so do the 6 HTTP timing tests. In the outage
scenarios the oldest pooled transaction was at most 60 ms, against the 400 ms `longSlack`.

I found **no path by which an unverified callback can write**, and **no cross-tenant leak**.

None of the findings below can be reached from request input, and none re-opens the
connection-pinning mechanism. The remaining gaps are in test coverage, in one internal API
shape, and in the ADR text.

**Conditions on the closure.** F-POOL-1 re-opens if condition K1 fails.
- **K1 (blocking for the closure to stand).** The first GitHub CI run of the new timing-lane step
  must pass all 8 tests by name. The workflow has never run on CI. Evidence so far is local only
  (the implementer's runs plus mine).
  - If any timing-lane test fails on CI, handle it under ruling B: investigate, and never rerun
    until green. F-POOL-1 goes back to OPEN pending that investigation.
- **K2 (before Stage 10.3 is closed).** Fix findings S-1, S-2 and S-3 below.
- **K3 (before Stage 10.3 is closed; ADR text only).** Fix findings S-5 and S-6 below.

**Launch.** Once K1 holds, F-POOL-1 is **no longer launch-blocking**. These items remain
launch-blocking and are not part of this closure:
- **F-POOL-2** (registered; blocks each domain's first non-MOCK adapter);
- **PAYWH-RL-1** (no rate limiting on the webhook routes; now the main way one tenant's traffic can
  take another tenant's share of the pool).

## 1. Conditions C1–C11, checked against the code

| Cond. | Status | Evidence in code |
|---|---|---|
| C1 single use | **MET** | `webhookauth.VerifiedCallback` is handed out as a pointer, and `consumed` is a shared `*atomic.Bool`. So a struct copy is single-use too. `Redeem` runs `CompareAndSwap` **first**, so a failed mismatch also burns the token. A nil pointer or a zero value is rejected. Tests: `TestVerifiedCallback_SingleUse`, casino "reuse (C1)". Mutation M11 was recorded. |
| C2 age bound | **MET** | `verifiedAt` is `time.Now()`, so it carries a monotonic reading. `sinceVerified(v.verifiedAt)` rejects an age below 0 or above 30 s. It is independent of the `now` that `VerifyAndSeal` receives. Mutation M21 was recorded. |
| C3 copy, no `Inbound` in phase 2, redaction | **MET in code; the test covers payments only (S-1)** | All three domain `VerifyCallback`s call `CloneInbound` before anything else. `ReceiveVerifiedCallback` takes no `Inbound`. `Redeem` returns the sealed copy. `String`, `GoString`, `Format`, `LogValue` and `MarshalJSON` all redact, and there is no exported serialization. The unit test `TestVerifiedCallback_Redacted` covers fmt verbs, the pointer and the value, JSON, and slog JSON. |
| C4 handle of the credential that verified | **MET** | See the table after this one. |
| C5 tests | **MET** | The predecessor-revoked test is `TestRecheck_KeyImplicitPredecessorRevokedBetweenPhases`. The #10 variants cover reuse, age (unit test), tenant mismatch, provider mismatch and the WithTenant(B) case (casino). The body-mutation test covers payments. The redaction test is a unit test. |
| C6 marking coverage | **MET** | Every `With*` passes `txscope.Mark(ctx)`. `WithTenant`, `WithTenantSnapshot` and `WithTenantReadOnly` do it through the shared `withTenantTx`. `TestWithScopes_MarkTxscope` finds the methods by reflection, requires at least 10, and fails on an unknown parameter type. `TestPoolRaw_NoNonTestCallersOutsideDB` finds no non-test `.Raw()` outside `internal/db`. The `txscope` key is unexported and there is no unmark function. Mutation M13 was recorded. |
| C7 capture tests primary; advisory-lock caveat | **MET in ADR 0022; not carried into ADR 0094 §2 item 2 (S-5)** | The ADR 0022 §3 point 9 amendment states both points. |
| C8 cold start, P = 2 | **MET** | `MaxConcurrentStoreCallsPerTenant = 2`. `SlotWait` is still 250 ms. The C8 subtest of #1 (4 cold refs at 150 ms latency, 0 rejections) passes. §6 is updated to "≥ 2 healthy slots". |
| C9 rate bound and multi-tenant warning | **MET, but the stated bound is optimistic under concurrency (S-6)** | `TestFetcher_GlobalOutageRateBound` exists. The warning `secret_store_multi_tenant_degraded` fires at ≥ 3 degraded tenants per scheme, at most once per minute. It is emitted only on counting failures. |
| C10 mutations | **MET for M1–M13** | Evidence file: 28 run, 27 killed, 1 survivor (M14), declared equivalent. My own spot-checks are in §3. Two new survivors are recorded as S-1 and S-2. |
| C11 F-POOL-2 registered | **MET** | `docs/governance/task-registry.md` records the wider scope, the launch-blocking status for each domain, and the dual-write referral to `ledger-finance`. |

**C4 in detail:**
- `credentialFor` sets `HandleID: h.id` for each row, so the Active and Previous credentials each
  carry their own row id.
- `VerifyInbound` returns `*creds.Previous` when the predecessor's key id matched.
- `Redeem` rechecks `v.cred`, which is the credential that was *returned*.
- A real `Recheck` fails closed on any of the following:
  - a zero `HandleID`;
  - `c.TenantID` different from the tenant;
  - an invalid provider or fingerprint;
  - zero rows;
  - any DB error.
- `KindSplitResolver.Recheck` fails closed for an unregistered provider id.
- `MockRecheck` rejects any credential with a non-zero `HandleID`.
- Mutation M12 was recorded.

### Handle re-check (`HandleRecheckSQL`), including the narrower filter

- **Binds:** `tenant_id`, `id`, `fingerprint`, `domain`, `provider_id`, `purpose`, status
  (active or verify_only), and `not_before`/`not_after` on the database clock.
- **The extra predicates** (domain, provider, purpose) are all values that the handle read already
  bound, so they can only narrow the result. They cannot reject a legitimate callback:
  - `domain` is the resolver's own domain;
  - `provider_id` comes from the handle row;
  - `purpose` is `webhook_verify`.
- **Tenant binding:** the statement runs under the domain transaction's RLS **and** an explicit
  `tenant_id = $1`. A token for tenant A inside `WithTenant(B)` therefore gets 0 rows. The casino
  test covers this.
- **KeyImplicit predecessor:** it is accepted while its status is verify_only and
  `not_after > now()`. A revoke or an expiry between the phases rejects it.
  `TestRecheck_KeyImplicitPredecessorRevokedBetweenPhases` covers this.
- **Revocation:** revoking mid-window is accepted as in the design. There is no `FOR SHARE`, and the
  window is [re-check, commit].

### Fetcher: per-(scheme, tenant) breaker, P = 2, D = 2, fail-fast

- **Breaker:** keyed by `breakerKey{scheme, tenant}`. Only that tenant's flights record to it and
  only that tenant can probe it. An entry that is closed with 0 failures is pruned after a store
  call.
- **Admission:**
  - the global limit S = 4, the per-tenant limit P = 2, and the degraded budget D = 2 are checked
    atomically under `mu`;
  - the `degraded` flag is captured at admission and released with the same value;
  - a healthy owner waits at most `slotWait`;
  - a degraded owner never waits;
  - a degraded follower never waits (`wait = -1`);
  - a refused probe is aborted and not counted;
  - an admission loss writes no negative-cache entry.
- **Invariant:** `init` panics unless 0 < P ≤ D < S. With 2 ≤ 2 < 4, it holds.
- **Guard:** `Fetch` refuses a marked context **before** any cache, breaker or flight access, and
  returns the non-counting `ClassStoreConfig`, which is not negative-cached. `GetDirect` is guarded
  the same way.
- **My mutation** (degraded ignores `consecutive > 0`) was killed by 3 tests (§3).

### Every error path fails closed

I found no success-shaped fallthrough. I followed every new branch:
- the guard in each entry point: `Fetch`, `GetDirect`, `Resolver.Resolve`,
  `OutboundResolver.Resolve`, and the three domain `VerifyCallback`s;
- the nil-reader and nil-tenant checks;
- the read-only transaction's DB errors. Payments EXISTS errors still return a 500 with no detail;
  handle-read errors give `credential_unavailable`;
- admission loss, and an open breaker;
- the `Redeem` failures: nil or zero token, reuse, domain, tenant or provider mismatch, age, a nil
  resolver or transaction, and `Recheck` errors. All give `credential_unavailable`, the domain
  transaction rolls back, and casino records no rejection because the error is an `AuthError`, not a
  `CallbackRejectedError`;
- `ReceiveVerifiedCallback`'s lookup of the provider that was just redeemed.

### Can an unverified callback write?

**Not from request input.**
- `ReceiveVerifiedCallback` is reachable only through a successful `Redeem`.
- `Redeem` needs a sealed token.
- The only constructor is `VerifyAndSeal`, which runs `VerifyInbound`.
- `HandleCallback` also re-verifies with `cred.Secret`.
- Every production `VerifyCallback` call site (5 handlers) runs **outside** any `With*` closure. I
  checked each one.

The constructor can be abused only by code inside the process (S-3).

### Test-only bridge and test seam

- **`receiveCallbackInTx` / `txReader` / `phaseOneContext`:**
  - they exist only in `receive_bridge_test.go` in each domain package. `casino` and `kyc` are
    build-tagged `integration`; `payments` is not, but the file is `_test.go`;
  - a grep of non-test `.go` files finds no reference;
  - no test file that uses the bridge constructs a real `providercred` resolver.
  - **Confirmed unreachable from production.**
- **`simulationBetweenPhasesHook`:**
  - it is an unexported package variable, so it cannot be set from outside `httpserver`;
  - the only assignments are in `resolution_isolation_integration_test.go` (set, then deferred reset
    to nil);
  - there is no non-test assignment, so it is nil in production.
  - **Accepted** as a disclosed seam. A build-tag split would be stronger, but it is not required.

### `txscope` bypasses (reviewed; residuals accepted as disclosed)

| Bypass | Status |
|---|---|
| (a) `context.Background()` or a detached goroutine inside `fn` | Disclosed and accepted. |
| (a′) A closure inside `With*` that uses the **captured outer** `r.Context()` instead of the callback's `ctx` | Not caught by the guard. No such call exists today (checked above). This is the most likely future mistake, so it is noted here. |
| (b) A new `With*` that forgets to mark | Closed by the reflection test. |
| (c) `Raw()` | Closed by the source guard. |
| (d) A foreign `TenantReader` | Only `*db.Pool` in production; the bridge is test-only. |

Two more observations:
- `context.WithoutCancel` keeps values, so it stays marked.
- The API shape is still the primary control: nothing that reaches the store takes a `pgx.Tx`.

### CI lane (ruling (6))

- The lane runs exactly these 8 tests:
  - `TestStoreOutage_DoesNotPinPool`;
  - `…_ProductionPoolSize`;
  - #1 `NormalOperation`;
  - #2 `OneTenantStoreOutage`;
  - #3 `MultipleTenantsOutage`;
  - #3b `SimultaneousOnset_Bounded`;
  - #5 `ConnectionExhaustion`;
  - #7 `FinancialDuringOutage`.
- **That matches my ruling exactly.**
- The main lane skips exactly those names (`-skip "^(…)$"`).
- Lane properties:
  - it is blocking, with `-count=1` and no retry;
  - each test has its own `grep "--- PASS: <name> "` guard. The trailing space stops
    `TestStoreOutage_DoesNotPinPool` from matching `…_ProductionPoolSize`;
  - no lane test calls `t.Parallel()`.
- #4, #6, #8–#12, `…ResolveInsideTenantTxRefused` and the Fetcher unit tests are in the main lane,
  as ruled.
- The reviewed literals are unchanged: 500 ms, `longSlack` = 400 ms, ≤ 4.
- The kept test keeps its three original assertions with the same values, adds criteria (4) and
  (5), and stays at pool 20.

### `NormalOperation` load profile (≤ 10 in flight per tenant): **ACCEPTED**

**The §5 claim is not tested by this test.** §5 claims that an unrelated query stays under 500 ms
during one tenant's store outage. It is still tested at full strength by:
- the kept test and its pool-10 variant (50 concurrent callers);
- #2 (52 concurrent A callbacks at pool 10, with an unrelated query);
- #5 (500 A callbacks from 100 goroutines, and B's worst latency under 500 ms).

**What #1 measures** is healthy throughput latency, and the change does not affect the admission
property it was admitted to the lane for:
- 30 callbacks are in flight at once across the 3 tenants, each chain on its own wallet;
- the 500 ms bound is unchanged.

**Two disclosures follow from it:**
- At pool 10 under `-race`, about 60 simultaneous healthy callback chains exceed 500 ms p100. That is
  ordinary capacity, and it belongs with PAYWH-RL-1 and capacity planning.
- My alone-run measured 356 ms, against 226–287 ms in the record. That leaves 144 ms of headroom.
  Watch this test for flakes on CI. The bound must not be widened.

## 2. Findings

| ID | Severity | Finding | Concrete failure scenario | Required fix |
|---|---|---|---|---|
| **S-1** | Low | **C3's per-domain clone is tested only for payments.** Removing `CloneInbound` from `casino.VerifyCallback` **survived** every casino, `webhookauth` and httpserver test (my mutation A). The KYC code has the same shape. `TestVerifiedCallback_BytesArePrivateCopy` passes an already-cloned inbound into `VerifyAndSeal`, so it does not test the domains' own clone. | A future casino/KYC refactor drops the clone. A caller such as a retry wrapper or a pooled body buffer then reuses or mutates `body` between the phases. Phase 2's `HandleCallback` parses bytes that never verified, which breaks C3's "impossible by construction". Idempotency limits the financial effect, but the verification guarantee is lost. | Move the copy **into `VerifyAndSeal`**, so there is one chokepoint and the domains cannot forget it. Add body-mutation tests for casino and KYC. Either change must kill mutation A. |
| **S-2** | Low | **The `VerifyCallback` `txscope` guard is tested only for payments.** Disabling the casino guard **survived** (my mutation B). KYC has the same shape. | With a real resolver, INV-POOL still holds, because `providercred.Resolve` and `Fetch` have their own guards. That makes the survivor partly equivalent. With a MOCK resolver, a casino/KYC `VerifyCallback` inside a transaction would run silently, and the wiring bug that §4.1 is meant to surface by an error log would go unreported. The condition text ("each domain's `VerifyCallback`") is only one-third tested. | Add `TestVerifyCallback_InsideTxRefused` for casino and KYC: `credential_unavailable`, 0 store calls, one `secret_fetch_with_tx_held` line with the domain's `entry_point`. |
| **S-3** | Low | **`webhookauth.VerifyAndSeal` is exported and seals whatever `CredentialSet` its caller passes.** `Recheck` binds the handle id and fingerprint but never checks that `c.Secret` is the secret that fingerprint pins. The domain tag is a runtime string, not a separate type. | Code in the process (not request input) reads a live handle's `id` and `fingerprint` from the database. It builds a `Credential` with those values and a secret of its own choice, signs a body with that secret, and calls `VerifyAndSeal("payments", …)`. `Redeem` and `Recheck` pass, `HandleCallback`'s re-verify uses the same forged secret, and a deposit posts without any provider signature. The design's "only `VerifyCallback` constructs one" is weaker in the implementation. | Pick one: (a) make sealing reachable only from the resolution path, for example a `webhookauth` function that takes the `Resolver` and `TenantReader` and resolves the credentials itself; or (b) have `Redeem` recompute the keyed fingerprint of `cred.Secret` and compare it in constant time with `cred.Fingerprint`, which `Recheck` binds to the row. Option (b) needs the fingerprinter to be injectable into `Redeem`/`Recheck`, so (a) is preferred. The **runtime** domain tag is **acceptable** on its own: it fails closed, it is tested ("other domain"), and `HandleRecheckSQL` also binds `domain`. |
| **S-4** | Low (hygiene F-2 residual) | **`derivedTokenBytes` redacts through `fmt` but not through `slog` or `encoding/json`.** I confirmed this with a scratch program using a type with the same method set. `slog.TextHandler` special-cases any `[]byte`-kind value and prints it **in plaintext**. `slog.JSONHandler` and `json.Marshal` print it base64-encoded. `derivedEntry` itself is safe: fmt uses its `Format`, and JSON of its unexported fields is `{}`. | Code in the package logs `slog.Any("token", e.token)` while debugging an eviction bug. The derived OAuth/bearer token is written to the logs in plaintext. | Add `LogValue() slog.Value` (a `LogValuer` is resolved before the handler's `[]byte` special case) and `MarshalJSON` to `derivedTokenBytes`, and ideally to `derivedEntry`. Extend `outbound_cache_test.go` to check slog text, slog JSON and `json.Marshal`. |
| **S-5** | Info (doc) | **ADR 0094 §2 item 2** still says `READ ONLY` "is stronger than the statement-capture discipline alone". It does not say that the capture tests stay the **primary** I1 control, or that `READ ONLY` does not stop advisory locks. C7 required both. ADR 0022's amendment is correct. | A later reader treats `READ ONLY` as sufficient and drops or weakens `TestPointNineCapture_*`. That would allow a pre-verification `pg_advisory_xact_lock`, which is a cross-tenant contention lever. | Reword §2 item 2 to match the ADR 0022 amendment. |
| **S-6** | Low (doc and test) | **The C9 rate bound is exact only for sequential arrival.** `consecutive` can pass `BreakerTripThreshold` while flights are still in progress. With P = 2, one more counting call can finish after the third, so the first trip costs up to (3 + P − 1) × 2 = 8 attempts per tenant, not 6. The 120 s bound is then ≤ 14 N, not 12 N (700, not 600, for N = 50). The rate test calls sequentially, so it cannot see this. Concurrency is still bounded by S and D. | This does not endanger the store: it is still linear in N and still concurrency-capped. But §6 says the test "measured exactly 600 … none more", which overstates what is proven. | State the bound as N × (3 + P − 1) × (1 + `StoreMaxRetries`) + probes in §6. Optionally add a concurrent variant of the rate test. |
| **S-7** | Info | **An admission loss does not prune the breaker entry.** `own` returns before `pruneBreakerLocked`, so a closed, zero-failure entry can stay behind. | No security effect. The map is bounded by the number of tenants that have ever reached `Fetch`, which requires a matching handle row for an active, route-resolved tenant. `warnMultiTenantDegradedLocked` iterates the map, which is O(tenants). | Optional: prune in the admission-loss branch too. |
| **S-8** | Info | **Simulation handlers:** in step 3, the session or intent re-validation reads run **before** `Redeem`/`Recheck`, so `HandleRecheckSQL` is not the first statement in those domain transactions. | None. They are plain reads with no lock (`GetLaunchSessionByID`, `GetDepositIntentByID`) of the authenticated player's own rows, on a MOCK-only route. A failed `Redeem` rolls back. | None. Note this as a scoped exception in the ADR 0022 amendment, whose "first statement" wording is about the public webhook handlers. |
| **S-9** | Info | **The §9.3 post-condition `AcquiredConns() ≤ 10` is sampled once, at the end,** not "throughout". | None. The `pg_stat_activity` transaction-age sampler measures the real property, a connection held across the store wait, on the handler path, and does so continuously. | None required. |

**No Critical, High or Medium findings.**

## 3. What I ran

### Timing lane, alone

Commands identical to the CI step: `-race -tags=integration -count=1`, a 4-vCPU dev box, local
PostgreSQL. **8/8 PASS.**

| Test | Result |
|---|---|
| `TestStoreOutage_DoesNotPinPool` | 2.30 s |
| `TestStoreOutage_DoesNotPinPool_ProductionPoolSize` | 2.22 s |
| `TestResolutionIsolation_NormalOperation` | worst callback 356 ms |
| `TestResolutionIsolation_OneTenantStoreOutage` | oldest transaction 31.6 ms |
| `TestResolutionIsolation_MultipleTenantsOutage` | oldest transaction 27.6 ms |
| `TestResolutionIsolation_SimultaneousOnset_Bounded` | 4.12 s |
| `TestResolutionIsolation_ConnectionExhaustion` | oldest transaction 59.5 ms; B's worst latency 64 ms |
| `TestResolutionIsolation_FinancialDuringOutage` | oldest transaction 27.4 ms |

### Main lane, new tests

Run under `-race -tags=integration` on these packages: `httpserver`, `casino`, `payments`, `kyc`,
`providercred`, `db`, `secretstore/...`, `webhookauth/...` and `txscope`.

- **Covered:** #4, #6, #11, #8 per domain, the re-check DB-error test, the #10 and C5 variants, the
  body-mutation test, the `VerifyCallback` guard test, `TestRecheck_*`, the #12 outbound test,
  `…ResolveInsideTenantTxRefused`, C6 (reflection and `Raw` guard), `WithTenantReadOnly` refusing
  writes, the 9.2 Fetcher unit tests plus C8 and C9, `TestPointNineCapture_*`, and the
  `VerifiedCallback` unit tests.
- **Result:** **52 top-level PASS, 0 FAIL.**

### My mutations

Each one was applied, run, and reverted with `git checkout`. `git status` was clean afterwards.

| # | Mutation | Result |
|---|---|---|
| A | `casino.VerifyCallback` no longer calls `CloneInbound` | **SURVIVED** (casino, `webhookauth`, httpserver). Recorded as S-1. |
| B | `casino.VerifyCallback` `txscope` guard disabled | **SURVIVED** (casino, `providercred`, `webhookauth`, httpserver main-lane tests). Recorded as S-2. Partly equivalent for INV-POOL, because the downstream guards remain. |
| C | Fetcher `degradedLocked` ignores `consecutive > 0` (a tenant counts as degraded only once its breaker is open) | **KILLED** by `TestFetcher_DegradedBudget_HealthyReserve`, `TestFetcher_DegradedCallersFailFast` and `TestResolutionIsolation_MultipleTenantsOutage` (the healthy cold bet got a 401). |

## 4. Scope and limits of this review

**In scope:**
- the ADR 0094 code in `internal/{txscope,db,secretstore,providercred,webhookauth,payments,kyc,casino,httpserver}`;
- the CI workflow change;
- the new and migrated tests, sampled. I did not read all ~40 migrated domain test files line by
  line. I checked that none of them uses the bridge with a real resolver.

**Not in scope:**
- GitHub CI execution (K1);
- the AWS Secrets Manager adapter;
- F-POOL-2 and PAYWH-RL-1;
- penetration testing;
- behaviour under production load or on production hardware.

Passing this review does not make the webhook resolution path "secure" in general. It means that
F-POOL-1's mechanism is removed, and that ADR 0094's conditions are met, with the gaps listed above.

## 5. Hygiene F-2 ruling (`4ae9bb4`, `internal/providercred/outbound.go`)

- **Cache-local `derivedTokenBytes`: ACCEPTED as the right shape, and INCOMPLETE** (S-4).
  - It owns its own copy (`newDerivedTokenBytes`).
  - `bytes()` hands out copies.
  - `zero()` runs on eviction and on replacement.
  - `derivedEntry` has its own `String`/`GoString`/`Format`, which correctly closes the fmt
    reflection dump of the unexported `[]byte` field.
  - What is missing is `LogValue` and `MarshalJSON`, as described in S-4.
- **`secretstore.Secret.Wipe()`: NOT needed, and not recommended as a general method.**
  - A `Secret` is a value type. Copies share the backing array: the Fetcher's positive cache returns
    `e.secret` to every caller.
  - A `Wipe()` on any copy would therefore zero the cached bytes for every other holder. The next
    hit's fingerprint check would then raise a false `credential_integrity` P1, or a concurrent
    verification would fail.
  - Callers never alias the cache anyway, because `Bytes()` returns a copy.
  - Zeroing heap memory in Go is best effort, not a security control. Per-request copies
    (`Credential.Secret`, HMAC state) are never zeroed either.
  - If zeroing cached secrets is ever wanted, it belongs **inside the Fetcher's eviction path**,
    where the cache is the only owner, through an unexported helper. It does not belong on the
    public `Secret` API.
- **Hygiene F-2 status: PARTIALLY RESOLVED.** The residual is S-4 (Low).

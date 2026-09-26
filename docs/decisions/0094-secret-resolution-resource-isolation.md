# ADR 0094 — Secret Resolution Resource Isolation (F-POOL-1 fix)

- **Status:** ACCEPTED — IMPLEMENTED 2026-09-26.
  - Design co-signed by `security` with conditions C1–C11 and confirmed by `qa` with changes 1–8
    (sections below).
  - All conditions and changes are applied. Every test in §9 passes, and the mutations are
    recorded (see "Implementation record" at the end).
  - **F-POOL-1 stays OPEN until `security` reviews the implementing diff**, as the co-signature
    requires. This status records that the implementation exists and passes its tests; it is
    not a security sign-off.
- **Decision type:** architecture (cross-domain: `db`, `secretstore`, `providercred`,
  `webhookauth`, `payments`, `kyc`, `casino`, `httpserver`).
- **Owner:** `architect`. **Co-owner:** `security`.
- **Trigger:** finding F-POOL-1 (`docs/plans/stage-10.3-planning/15-ci-342-security-ruling.md`
  §2). The human requires that it is **fixed, not accepted as a risk**. Constraints:
  - Do not raise the global pool size.
  - Do not weaken or remove `TestStoreOutage_DoesNotPinPool`.
  - No per-tenant bypass.
  - One tenant must not consume resources that other tenants need.
- **Amends:**
  - ADR 0093 §4: "Per-request handle read inside the caller's transaction", and "Store and
    cache".
  - ADR 0093 §5: the first bullet.
  - ADR 0093 A4: the table rows "Concurrent store calls" and "Breaker scope".
  - ADR 0022 §3 point 9 allowance: the handle read moves into its own read-only transaction.
  - Security review §5: the claim that the semaphore "bounds how many pooled DB connections can
    be held waiting on the store".

## 1. Problem

Today, inbound credential resolution runs inside the domain's `WithTenant` transaction:
- The handler opens `deps.DB.WithTenant`.
- It calls `Orchestrator.ReceiveCallback(ctx, tx, …)`, then `verifyCallback`, then
  `webhookauth.ResolveCredentials(ctx, tx, …)`, then `providercred.Resolver.Resolve(ctx, tx, …)`,
  then `Fetcher.Fetch`.

So every caller waiting on the secret store holds one pooled connection:
- Up to **4 slot holders** wait up to `StoreCallTimeout` (2 s).
- **Every other caller** waits up to `SlotWait` (250 ms). This covers slot-race losers in
  `acquireSlot` and flight followers in `await`.
- A slot-wait loss writes no negative-cache entry, so each new callback waits the full 250 ms
  again.

At the production pool (`config.DatabaseMaxConns = 10`), one tenant's outage plus 50 callbacks
made an unrelated tenant's query wait about 1.42 s (measured).

There is a second cross-tenant coupling that the finding did not grade. It fails the human's
"one tenant must not consume resources needed by other tenants" rule, so it is fixed here too:
- The breaker is **per backend scheme**. Three counting failures on tenant A's refs open it for
  **every** tenant on that backend.
- The half-open probe goes to whoever asks first. During A's callback burst that is usually A,
  so the probe fails again and the cooldown keeps doubling.
- Result: healthy tenants' cold fetches fail, and their cached entries are served only while
  still inside max-stale, for as long as A stays down.
- The 4 store slots are shared the same way. A can hold all of them.

Paths verified, with their status:

| Path | Holds a pool connection while waiting on the store? |
|---|---|
| Inbound webhooks: payments, KYC, casino (`webhook_verify.go`, then `ResolveCredentials(ctx, tx, …)`) | **Yes: F-POOL-1** |
| Simulation handlers (`payment_deposit_simulation_handlers.go:310`, `casino_play_handlers.go:377/474/618`) | No store call, because the MOCK resolver serves synthetic adapters. They still use the in-transaction API, which this ADR removes. |
| Outbound `OutboundResolver.Resolve` (`providercred/outbound.go`) | No. The handle read commits in its own `WithTenant` before `secretFor`. No production caller yet. **Latent risk:** a future caller inside a `WithTenant` callback would nest a second acquisition under a held connection. |
| Admin registration and apply (`service.go:299` `fileRequest`, `service.go:505` `applyRequest`, both `GetDirect`) | No. Both calls happen between transactions. They bypass the Fetcher's slots, but they are staff-initiated and low-volume. |

## 2. Invariants preserved (verified by §9)

1. **Tenant isolation / RLS.**
   - Every DB statement still runs through `db.Pool.With*`, with `app.tenant_id` set from the
     route-resolved tenant.
   - The handle read keeps its explicit `tenant_id = $1` predicate, and the new re-check has one
     too.
   - The cache key stays (tenant, ref, fingerprint).
   - The breaker and slot accounting become per (scheme, tenant). This is stricter than today.
2. **Webhook I1 (ADR 0022 §3 point 9).** Before verification, the only tenant-scoped statements
   are the ones already allowed:
   - payments: `ProviderAcceptsWebhook` EXISTS plus `HandleReadSQL`;
   - KYC and casino: `HandleReadSQL` only.

   They now run in a **`READ ONLY`** transaction, so the database itself refuses any write
   before verification. That is stronger than the statement-capture discipline alone. No ledger
   read, lock, write, tombstone or audit row happens before verification.
3. **Financial correctness.**
   - The domain transaction and everything in it are unchanged: ledger posting, idempotency on
     `(provider_id, provider_tx_id)`, tombstones and audit.
   - The only addition is one post-verification, lock-free, primary-key read (§5).
   - A callback that fails the re-check rolls back and writes nothing.
4. **Revocation.** Revocation is at least as immediate as today (§5). A handle read happens on
   every request, in every breaker state, before any `Fetch`.
5. **Secret-store fail-closed behaviour.** Every new branch ends in one of the existing closed
   reasons and the uniform 401 (outbound: `ErrOutboundCredentialUnavailable`). There is:
   - no fallback credential;
   - no unauthenticated path;
   - no stale value beyond `CacheMaxStale`;
   - no weakening of the fingerprint check on every hit.
6. **Database safety.**
   - No migration.
   - No new lock.
   - No nested pool acquisition. It is now structurally refused (§4.1).
   - No change to pool size or configuration.

**New invariant INV-POOL (the fix).** No pooled DB connection is held across any secret-store
call or any wait for one: slot wait, flight wait, or store I/O. It is enforced by all three of:
- **API shape:** no resolver method that can reach the store takes a `pgx.Tx`.
- **A runtime guard:** §4.1.
- **Tests:** §9.

## 3. Options considered

| Option | Verdict |
|---|---|
| (ii) Minimum pool size or burst assumption in config validation | **Rejected.** It sizes the pool around the defect. The human forbids "just raise the pool". Any bound would still be exceeded by a bigger burst. |
| Pool-relative admission cap on callers waiting for the store (ruling §2 (i), first example) | **Rejected as the primary fix.** It only shrinks the defect: connections are still held for up to 2 s, and some fraction of the pool is still given to one tenant's outage. **Not needed after the root-cause fix** (§6: nothing waiting on the store holds a connection). |
| In-transaction callers get cache hits only; a miss fails and triggers an async fetch | **Rejected.** It turns every healthy cold fetch (first callback after start or rotation) into a 401. It also hurts healthy synchronous casino bet callbacks. |
| **(1) Split resolution + (2) per-tenant fairness in the Fetcher + structural guard** | **Chosen.** It removes the mechanism instead of bounding it. |

## 4. Chosen design

### 4.1 Split resolution: never hold a connection while touching the store

**`internal/db`** (new, small):
- `WithTenantReadOnly(ctx, tenantID, fn)` behaves like `WithTenant`, but uses `BeginTx(AccessMode:
  ReadOnly)`. `set_config(..., true)` is permitted in a read-only transaction.
- Every `With*` scope function (`WithTenant`, `WithTenantReadOnly`, `WithTenantSnapshot`,
  `WithoutTenant`, `WithPlatformAdmin`, `WithPlatformService`, `WithSessionLookup`,
  `WithPrincipalScope`, `WithPlayerScope`, `WithCredentialTokenLookup`) passes
  `txscope.Mark(ctx)` to `fn`.

**New leaf package `internal/txscope`** (no imports):
- `Mark(ctx) context.Context` and `Held(ctx) bool`.
- It is a leaf so that `secretstore` does not have to import `db`.

**Runtime guard.** These entry points fail closed without touching any cache or breaker state,
without a store call, and with one `error` log line `secret_fetch_with_tx_held` (tenant id and
entry point only):
- `secretstore.Fetcher.Fetch` returns `ClassStoreConfig`. It is non-counting and is not
  negative-cached.
- `secretstore.Router.GetDirect` returns `ClassStoreConfig`.
- `providercred.Resolver.Resolve` returns `webhookauth.ErrCredentialUnavailable`.
- `providercred.OutboundResolver.Resolve` returns `ErrOutboundCredentialUnavailable`.
- Each domain's `VerifyCallback` returns an `AuthError` with `credential_unavailable`.

A caller that drops the context inside a `With*` callback (`context.Background()`) escapes the
guard. The API shape still prevents that caller from reaching a tx-taking resolver. The guard is
defence in depth, not the only control.

**`webhookauth.Resolver`** (changed contract; this interface is internal):

```go
type TenantReader interface {
    WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn func(context.Context, pgx.Tx) error) error
}
type Resolver interface {
    // Runs the single HandleReadSQL in its OWN read-only tx, which COMMITS before any
    // secret fetch; then fetches with no transaction held. Mock resolvers ignore r.
    Resolve(ctx context.Context, r TenantReader, tenantID uuid.UUID, providerID, keyID string, sel KeySelection) (CredentialSet, error)
    // Post-verification re-check inside the DOMAIN tx (§5). Mock: nil for a synthetic binding.
    Recheck(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, c Credential) error
}
```

- `Credential` gains `HandleID uuid.UUID`. It is zero for the MOCK, and a real `Recheck` with a
  zero `HandleID` fails closed.
- `KindSplitResolver` sends both methods to the same target, chosen by the adapter kind of the
  provider id.
- `ResolveCredentials(ctx, r TenantReader, scheme, resolver, in, m)` no longer takes a `tx`.
- `ResolveSingleKey` and the mock resolvers adapt without behaviour change.

**Domain orchestrators** (payments, KYC, casino) split into two phases:
1. `VerifyCallback(ctx, r TenantReader, tenantID, providerID, in) (VerifiedCallback, error)`. It
   must be called with **no** transaction held (guarded), and runs:
   1. It overwrites `in.TenantID` and `in.ProviderID` from its parameters.
   2. It checks registration and the scheme, then runs `ExtractInbound` (no DB).
   3. Payments only: `ProviderAcceptsWebhook` in a `WithTenantReadOnly` transaction.
   4. `ResolveCredentials`: a read-only transaction, commit, then the Fetcher with no connection.
   5. `VerifyInbound`.
   6. `LogVerifiedKey`.
   7. It returns an opaque `VerifiedCallback`. Its fields are unexported, and only this function
      constructs one. A zero value is rejected. It carries the tenant, provider, adapter,
      verified `Credential` (including `HandleID`), and the inbound bytes.
2. `ReceiveVerifiedCallback(ctx, tx, tenantID, providerID, v VerifiedCallback)` runs inside
   `WithTenant`:
   1. `v` must be non-zero and match `tenantID`/`providerID`. Otherwise it fails closed with an
      `AuthError`, reason `credential_unavailable`.
   2. It calls `resolver.Recheck(ctx, tx, tenantID, v.cred)`. Any failure, including a DB error,
      is an `AuthError` with `credential_unavailable`. For casino this is not a
      `CallbackRejectedError`, so `recordCasinoCallbackRejection` stays a no-op.
   3. Then it runs the existing post-verification body unchanged: `HandleCallback`, the
      key-material scan, and the domain flow.

- The old `ReceiveCallback(ctx, tx, …)` and `verifyCallback(ctx, tx, …)` are **removed**, not
  kept as aliases, so no in-transaction path remains.
- The 40 test files that call `ReceiveCallback` migrate through a per-package test helper that
  runs both phases.

**HTTP handlers.** The public webhook handlers (`deposit_handlers.go`, `casino_handlers.go`,
`kyc_admin_handlers.go`) call `VerifyCallback(r.Context(), deps.DB, …)`. On an `AuthError` they
return the same uniform 401 and log line as today. Otherwise they run `deps.DB.WithTenant(…,
ReceiveVerifiedCallback)`.

The simulation handlers are restructured into three steps:
1. A `WithTenantReadOnly` transaction reads the session and builds the payload.
2. `VerifyCallback` runs.
3. A `WithTenant` transaction re-validates the session (`resolvePlayerOwnedSession`,
   `requireRealMode`, `requireActiveUnexpiredSession`), runs `ReceiveVerifiedCallback`, and
   writes the simulation audit record.

Step 3 has to re-validate: without it, a session ending between steps 1 and 3 would be a TOCTOU
gap.

**Outbound and admin.** No functional change: they already commit before fetching. They gain the
guard. ADR 0093 §5's text "inside the caller's transaction" is corrected to what is implemented:
"its own short transaction, committed before the fetch".

### 4.2 Per-tenant fairness in the Fetcher

Accounting is per **(scheme, tenant)**, under the existing `Fetcher.mu`.

| Constant | Value | Status |
|---|---|---|
| `MaxConcurrentStoreCalls` (S) | 4 | unchanged |
| `MaxConcurrentStoreCallsPerTenant` (P) | **2** (security C8: P = 1 failed the cold-start test; P = 2 was pre-approved) | new |
| `MaxDegradedStoreCalls` (D) | **2** | new |
| Healthy reserve H = S − D | 2 | derived; the `secretstore` package `init` panics unless 0 < P ≤ D < S |

1. **Breaker scope** changes from per scheme to **per (scheme, tenant)**. Thresholds are
   unchanged: trip at 3 consecutive counting failures, cooldown 15 s doubling to a 60 s cap, one
   half-open probe.
   - Only that tenant's own calls feed it and only that tenant's calls can probe it. One tenant's
     outage therefore never opens another tenant's breaker.
   - An entry that is closed with 0 failures is deleted, so the map is bounded by the number of
     tenants with recent failures.
   - There is no backend-wide breaker any more. §6 shows that store load in a global outage is
     already bounded by S and D.
   - `BreakerState(scheme)` becomes `BreakerState(scheme, tenant)`.
2. **Degraded tenant.** A tenant counts as degraded when its (scheme, tenant) breaker is not
   closed, or its consecutive counting failures are ≥ 1.
3. **Admission for a store call.** The owner of a flight needs:
   - (a) a per-tenant token, with at most P in flight for the tenant;
   - (b) a global slot out of S;
   - (c) if the tenant is degraded, also a degraded token, with at most D in flight across all
     degraded tenants.

   The rules for waiting and failing:
   - **Healthy owners** wait up to `SlotWait` (250 ms) for (a) and (b). This is only goroutine
     time, because no connection is held.
   - **Degraded owners never wait.** If (a), (b) or (c) is unavailable they fail fast
     immediately.
   - A half-open probe that is refused admission is aborted (`abortProbe`) and not counted.
   - Every failure path is the existing one: serve stale within max-stale, otherwise
     `ClassUnavailable`. Nothing is written to the negative cache on an admission loss. That
     would turn contention into denial, and after §4.1 a retry costs no connection.
4. **Followers of a degraded tenant's flight** do not wait. They fail fast with stale or
   `ClassUnavailable`. Healthy followers keep the `SlotWait` bound.
5. Unchanged: single-flight per (tenant, ref, fingerprint), the positive cache (TTL, max-stale,
   and the fingerprint on every hit), the per-ref negative cache, `StoreCallTimeout`, retries,
   and the integrity alert.

`GetDirect` (admin) stays outside S, P and D. It is staff-initiated, runs under four-eyes, is
bounded by `StoreCallTimeout`, and now holds no connection by construction. This is a disclosed
design choice, not a gap: routing it through the slots would let a tenant outage block a
platform admin from registering a replacement credential.

### 4.3 Admission bound relative to pool size

**None is needed, and none is added.** After §4.1:
- The connection time of a webhook request no longer depends on the store's health.
- A failing tenant's callbacks cost what any rejected request costs: one read-only transaction
  of about 1–3 ms.

General request-volume overload is a rate-limiting concern shared by every endpoint. It is not
part of F-POOL-1.

## 5. TOCTOU and revocation analysis

- **Today.** The handle read runs at t0 inside the domain transaction (READ COMMITTED,
  lock-free). A revoke that commits after t0 is not seen before the domain commit. The exposure
  window is [t0, commit], and it includes the store fetch (≤ 2 s) and verification. ADR 0093 §4
  accepts this: "the only in-flight exposure is a transaction that read the row before the
  revocation committed".
- **After.** The handle read (t0) and the domain transaction are separate. The domain
  transaction's first statement after `set_config` is:

  ```sql
  -- HandleRecheckSQL (pinned by the capture tests; lock-free, read-only, tenant-predicated)
  SELECT 1 FROM provider_credential_handles
  WHERE tenant_id = $1 AND id = $2 AND fingerprint = $3
    AND domain = $4 AND provider_id = $5 AND purpose = $6
    AND status IN ('active', 'verify_only')
    AND not_before <= now() AND (not_after IS NULL OR not_after > now())
  ```

  (As implemented: the domain, provider and purpose predicates were added to the design's
  text. They are strictly narrower.)

  Zero rows means `credential_unavailable`, and the transaction rolls back, so nothing is
  written.

  The effect on each case:
  - **Exposure window.** It becomes [t1, commit], where t1 is the re-check inside the domain
    transaction. That is **strictly narrower** than today's window: fetch and verify are
    outside it.
  - **Revocation between the read and the re-check.** The callback is rejected.
  - **Rotation between them (active to verify_only).** The callback is still accepted, which is
    consistent with the key's verify window.
  - **`not_after` passing between them.** The callback is rejected by the database clock.

- **`FOR SHARE` on the re-check** would make revoke-versus-callback linearizable. It is
  **rejected**: it would make the revoke `UPDATE` wait on in-flight financial transactions and
  add lock contention to the bet path, for a window that is already smaller than the accepted
  one.
- **Cache staleness.** The positive cache still governs only the *availability* of bytes. It
  never governs authorization, which is always the per-request handle read plus the re-check.
  So TTL and max-stale semantics are unchanged.
- **Signature timestamp.** `VerifyInbound` uses `now` at verification time. The domain
  transaction starts later only by the time needed to acquire a connection. The replay
  protection (skew ≤ 10 min, plus idempotency) is unaffected.
- **Outbound.** Unchanged. The in-flight exposure stays one call, as in ADR 0093 A4.

## 6. Resource-allocation rationale

**Pool connections** (N = `DatabaseMaxConns`, default 10, unchanged).

| | Before | After |
|---|---|---|
| Connection time of a failing tenant's callback | up to 2 s (4 holders) or 250 ms (others) | one read-only transaction (BEGIN, `set_config`, 1–2 reads, COMMIT), about 1–3 ms; **independent of store state** |
| Domain transaction | opened for every callback | opened **only after verification succeeds**, so a failing tenant opens none |
| Tenant A burst of 50 during A's outage, N = 10 | about 1.42 s wait for B | about 50 × 2 ms / 10 ≈ 10 ms of queueing for B |

The share of the pool that one tenant's outage can take is therefore no longer a function of
store latency. It is the same as that tenant's ordinary request rate, and it needs no per-tenant
connection quota. Connection starvation from pure request volume is generic overload (§4.3).

**Store slots** (a shared, process-wide resource).

| Constant | Reason for the value |
|---|---|
| S = 4 | Security constant. It protects the store and the process, and no evidence justifies changing it. |
| P = 2 | A tenant's working set is a handful of refs (3 domains × few providers × ≤ 2 keys). Single-flight already collapses duplicates. The design proposed P = 1. Security condition C8's test (4 distinct cold refs of one tenant, 150 ms store latency, 0 rejections) **fails at P = 1**: the third ref waits 300 ms, which is more than `SlotWait` (evidence `f-pool-1-mutation-kill.txt`, C8). It **passes at P = 2**: refs 3 and 4 get tokens at 150 ms. P = 2 was pre-approved by security. `SlotWait` was not widened. Result: a single tenant's outage holds **at most 2 of 4** slots. |
| D = 2 | Any number of degraded tenants together hold at most 2 slots. That leaves **H = 2 always reserved for healthy tenants** once the failures have been observed. D = 1 would slow recovery across many degraded tenants (probes serialize). D = 3 would leave only 1 healthy slot. |
| Healthy capacity | H = 2 slots at about 100 ms each gives about 20 cold fetches per second. Demand is about (number of refs) / 10 min of refresh, plus cold starts. The margin is several orders of magnitude. |

Guarantees for healthy tenants:

| Situation | Healthy slots available |
|---|---|
| One tenant's outage, including its onset | ≥ S − P = **2** |
| Any number of degraded tenants, after onset | ≥ **2** |
| k tenants whose failures begin at the same moment and are not yet observed | ≥ S − min(k·P, S) for at most `StoreCallTimeout` (2 s), then ≥ 2 |

**Disclosed onset residual (with P = 2):** the table's last row reaches 0 when k ≥ 2 tenants'
outages start at the same instant and each has at least 2 distinct cold refs in flight.
`TestResolutionIsolation_SimultaneousOnset_Bounded` pins this with k = 5. Even then:
- It affects healthy **cold** fetches only. Cache hits take no slot.
- Those fetches fail fast with a retryable uniform 401 or with stale data.
- It lasts ≤ 2 s.
- It involves **no** DB connection.

It cannot be closed without pre-empting in-flight calls. It is equivalent to a backend-wide
outage.

**Global outage** (no backend-wide breaker any more). This was corrected per security condition
C9. The earlier claim "lower than today's 4" was misleading.
- **Concurrency** is bounded: ≤ S = 4 at onset and ≤ D = 2 after it. The process and the store
  are protected against a stampede.
- **Rate** is **linear in the number of tenants N**. That is higher than today, where one
  backend breaker brings the load down to about one probe per 15–60 s.
  - To trip every tenant's breaker costs at most N × 3 × (1 + `StoreMaxRetries`) attempts.
  - After that, each tenant is allowed one probe per cooldown, and each probe is at most
    1 + `StoreMaxRetries` attempts.
  - Over 120 s the cooldown schedule is 15 s, 30 s and 60 s, which allows 3 probes.
  - The bound over 120 s is therefore N × 3 × 2 + N × 3 × 2 = 12 N attempts. That is 600 for
    N = 50.
  - In steady state this is about N / 60 probe calls per second at the 60 s cooldown cap: about
    17–33 req/s for N = 1000.
  - `TestFetcher_GlobalOutageRateBound` pins it. It measured exactly 600 attempts against the
    computed bound of 600: every allowed attempt was made, and none more.
- **Visibility:** when at least 3 distinct tenants on one backend are degraded at the same time,
  one rate-limited `warn` line, `secret_store_multi_tenant_degraded`, is logged at most once
  a minute per backend. A real backend-wide outage is therefore visible as one event.

**Goroutines and HTTP.** Owners wait ≤ 2 s, healthy followers and losers ≤ 250 ms, degraded
callers 0. This is the same or less than today, and none of it holds a connection.

## 7. Fail-closed analysis

| New branch | Outcome |
|---|---|
| Fetch, `GetDirect`, a resolver or `VerifyCallback` called with a transaction held | fail closed (§4.1), 0 store calls, no state change, error log |
| Read-only transaction DB error (EXISTS or handle read) | as today: payments' EXISTS error is a 500 without detail; handle read gives `credential_unavailable`, uniform 401, and a reason that does not depend on whether a handle exists (C5) |
| Admission refused (per-tenant, global or degraded) | stale within max-stale, otherwise `credential_store_unavailable`, uniform 401 |
| Per-tenant breaker open | same, with 0 store calls. Only that tenant is affected. |
| Re-check has 0 rows, a DB error, `VerifiedCallback` mismatch or a zero value | `credential_unavailable`, uniform 401, domain transaction rolled back, nothing written (no casino rejection record) |
| A write attempted before verification | refused by PostgreSQL (`READ ONLY` transaction), giving a 500 and a failed test |

Never: a fallback credential, a cross-tenant cache hit (the key includes the tenant), an
unauthenticated call, a stale value beyond 60 min, or a cached value whose fingerprint does not
match.

## 8. What changes, where (no migration)

| File(s) | Change |
|---|---|
| `internal/txscope/txscope.go` (new) | `Mark`, `Held` |
| `internal/db/tenant_rls.go`, `tenant_snapshot.go`, `platform_service.go` | `WithTenantReadOnly`; every `With*` marks the context |
| `internal/secretstore/fetcher.go` | per-(scheme, tenant) breaker; P/D admission; degraded fail-fast; guard; `BreakerState(scheme, tenant)`; new constants |
| `internal/secretstore/router.go` | guard in `GetDirect` |
| `internal/secretstore/memstore` (test-only) | `BlockRef(ref)` / `UnblockRef(ref)`; `OnCallCtx(func(ctx, ref))`; `MaxConcurrentFor(prefix)` |
| `internal/webhookauth/resolver.go`, `scheme.go`, `mock.go` | `TenantReader`; new `Resolver` contract; `Credential.HandleID`; `ResolveCredentials` without a transaction; `KindSplitResolver.Recheck` |
| `internal/providercred/resolver.go` | `Resolve` opens its own read-only transaction, commits, then fetches; `Recheck` plus the pinned `HandleRecheckSQL`; guard |
| `internal/providercred/outbound.go` | guard only |
| `internal/{payments,kyc,casino}/webhook_verify.go`, `orchestrator.go` / `provider.go` | `VerifyCallback` and `VerifiedCallback`; `ReceiveVerifiedCallback`; remove `ReceiveCallback` and `verifyCallback` |
| `internal/httpserver/{deposit,casino,kyc_admin,payment_deposit_simulation,casino_play}_handlers.go` | two-phase flow (§4.1) |
| `internal/config/config.go` | export `DefaultDatabaseMaxConns = 10` and use it in `Load`; no behaviour change |
| `.github/workflows/ci.yml` | the isolated timing lane runs exactly the tests marked [T] in §9, with a `grep` guard for each |
| Documentation (in the implementing commit) | ADR 0093 §4, §5, A4; ADR 0022 §3 point 9 allowance ("in its own `READ ONLY` transaction; plus the post-verification `HandleRecheckSQL`"); a pointer from security review §5 to this ADR |

Migration: **none.** `HandleRecheckSQL` uses existing columns and the primary key. No
configuration keys are added and the pool configuration is unchanged.

## 9. Test plan

"Pool 10" means `db.Connect(…, 10, …)`, and each such test asserts
`config.DefaultDatabaseMaxConns == 10`, so a change to the default forces this ADR to be
revisited. `config.DefaultDatabaseMaxConns` is a new exported constant that `config.Load` uses
in place of today's inline `10` (`config.go:426`); behaviour is unchanged.

**Timing bounds.** These literals are reviewed values and must never be widened:
- the unrelated query < 500 ms;
- `longSlack` = 400 ms, around `SlotWait`;
- ≤ 4 store calls.

Tests marked [T] run in the isolated CI lane under ruling B's conditions (blocking, no retry,
`grep` guard). This ADR is the test-specific ruling that condition 3 requires, subject to
`security` co-signature.

### 9.1 The kept test and its production-size variant (`internal/providercred`)

The kept test's call shape changes to the production shape: each of the 50 goroutines calls
`Resolver("casino").Resolve(ctx, timingReader(f.rt), …)` with no outer transaction. That is
exactly what handlers now do. This is not a weakening:
- all three original assertions are kept with their literal values;
- two stricter assertions are added.

| Test | Setup | Pass criterion |
|---|---|---|
| `TestStoreOutage_DoesNotPinPool` [T] (kept; pool 20, the shared fixture) | Global `Block()`; 50 concurrent callers over 8 tenants and refs; unrelated query at +400 ms | (1) `MaxConcurrent() ≤ 4`. (2) ≤ 4 resolves longer than `longSlack`. (3) The unrelated tenant's `WithTenant` plus `SELECT 1` takes < 500 ms. (4) **New:** 0 read-only transactions (measured by the `timingReader` wrapper around each `fn`) last longer than `longSlack`. (5) **New:** every store `Get` saw `txscope.Held(ctx) == false` (`OnCallCtx`). |
| `TestStoreOutage_DoesNotPinPool_ProductionPoolSize` [T] | Identical, at **pool 10** | The same five criteria. This is the property that failed 3/3 at HEAD `2876fa5`. |
| `TestStoreOutage_ResolveInsideTenantTxRefused` | The old shape: `Resolve` called inside `f.rt.WithTenant` | Every call returns `ErrCredentialUnavailable` in < 5 ms; 0 store calls; one `secret_fetch_with_tx_held` log line per call |

### 9.2 Fetcher unit tests (`internal/secretstore`, fake clock, no DB)

| Test | Pass criterion |
|---|---|
| `TestFetcher_PerTenantCap` | Two distinct blocked refs of tenant A: at most 1 concurrent store call for A. The second fails with `ClassUnavailable` within `SlotWait` + 50 ms. |
| `TestFetcher_DegradedBudget_HealthyReserve` | Tenants A, B and C each degraded (1 counting failure) and then blocked: degraded store concurrency ≤ 2. Healthy tenant H's cold fetch succeeds in < 50 ms while all degraded flights are blocked. |
| `TestFetcher_TenantBreakerIsolated` | 3 counting failures for A: `BreakerState(memory, A) = open` and `BreakerState(memory, B) = closed`. B's cold fetch makes 1 store call and succeeds. |
| `TestFetcher_DegradedProbeNeverStarvesHealthy` | A's breaker is half-open during a burst of A: exactly 1 probe for A; B's fetch never waits on A's probe. |
| `TestFetcher_DegradedCallersFailFast` | Degraded owners and followers return in < 5 ms with no store call (stale if present). |
| `TestFetcher_AdmissionLossNotNegativeCached` | After a slot-loss failure, the next fetch of the same key once capacity is free makes a store call. |
| `TestFetcher_FetchRefusedWithTxHeld`, `TestRouter_GetDirectRefusedWithTxHeld` | `ClassStoreConfig`, 0 store calls, breaker, negative cache and positive cache unchanged |
| Existing `TestStoreBreaker_*` and `TestStoreCache_*` | Pass unchanged in substance, restated per tenant. `tripStoreBreaker` trips the breaker of the tenant under test. `TestResolver_RevokeImmediateWhileBreakerOpen` opens **that tenant's** breaker. |

### 9.3 Adversarial and concurrency suite (the human's list)

Location: `internal/httpserver`, integration, **pool 10**. It drives real HTTP handlers with the
real resolver over memstore. A fake clock is injected into the Fetcher. Tenants B and C are
healthy.

Every test also asserts these **global post-conditions**:
- `SUM(debits) == SUM(credits)`;
- the recomputed balance equals the projection for each wallet;
- 0 ledger, intent, audit or tombstone rows for any rejected callback;
- `Stat().AcquiredConns() ≤ 10` throughout.

| # | Test | Scenario | Pass criterion |
|---|---|---|---|
| 1 | `TestResolutionIsolation_NormalOperation` [T] | 3 tenants × 30 concurrent signed callbacks (payments deposit and casino bet/win), both cold and warm | All verified and posted exactly once. p100 latency < 500 ms. At most 1 store call per (tenant, ref). |
| 2 | `TestResolutionIsolation_OneTenantStoreOutage` [T] | `BlockRef` on A's refs; 50 A callbacks; concurrently B (cold) and C (warm) callbacks; unrelated query | Every A callback gets the uniform 401 with `credential_store_unavailable`. Store concurrency for A ≤ 1. Every B and C callback succeeds, each in < 500 ms. The unrelated query takes < 500 ms. 0 read-only transactions longer than `longSlack`. `BreakerState(B) = closed` throughout. |
| 3 | `TestResolutionIsolation_MultipleTenantsOutage` [T] | A1–A5 each degraded (1 counting failure), then `BlockRef`, then a burst of 50 each; H makes a cold fetch | Degraded store concurrency ≤ 2. H's cold callback succeeds in < 500 ms. The unrelated query takes < 500 ms. |
| 3b | `TestResolutionIsolation_SimultaneousOnset_Bounded` [T] | A1–A5 blocked at the same instant, never observed before; H cold at +50 ms and at +2.3 s | At +50 ms, H either succeeds or fails fast (≤ `SlotWait` + 150 ms) with `credential_store_unavailable`, and holds 0 connections. At +2.3 s, H succeeds. A warm H callback always succeeds. This pins the §6 onset residual. |
| 4 | `TestResolutionIsolation_Recovery` | Test 2's outage, then `UnblockRef`, then advance the fake clock past the cooldown | A's single probe succeeds and A's breaker closes. The next A callback verifies and posts once. A redelivery of a callback that failed during the outage posts exactly once. B is unaffected at every phase. |
| 5 | `TestResolutionIsolation_ConnectionExhaustion` [T] | During A's outage: 500 A callbacks from 100 goroutines, plus B traffic | B's p100 < 500 ms. 0 transactions longer than `longSlack`. No `pgxpool` acquire error for B. Goroutines return to baseline ± 5 after the burst (no leak). |
| 6 | `TestResolutionIsolation_CrossTenant` | During A's outage: (a) A's route with a body signed with B's warm key; (b) B's route signed with A's key; (c) A and B share a provider id and key id with different secrets | (a) and (b) get the uniform 401 and 0 writes. (c) each verifies only under its own tenant. The cache and breaker for A are never read or updated by B's requests (per-tenant counters). |
| 7 | `TestResolutionIsolation_FinancialDuringOutage` [T] | During A's outage: B casino bet then win (cold, then warm), a B payment deposit callback, idempotent redelivery of each, and a B player wallet read | Each posts exactly once with correct amounts; each redelivery posts nothing new; every B request takes < 500 ms; global post-conditions hold; A has 0 ledger or audit rows. |
| 8 | `TestReceiveVerified_RevokedBetweenVerifyAndDomainTx` (each domain) | A test seam between the two phases revokes the verified handle | `credential_unavailable`, uniform 401, 0 rows written. A variant that transitions it to verify_only inside its window is accepted and posts once. A variant where `not_after` has passed is rejected. |
| 9 | `TestPointNineCapture_{Payments,KYC,Casino}_AllowsExactlyOneHandleRead` (updated) | Statement recorder | The pre-verification transaction is `READ ONLY` and contains exactly `set_config` + `HandleReadSQL`; payments has one more `READ ONLY` transaction with `set_config` + EXISTS. The domain transaction's first statement after `set_config` is `HandleRecheckSQL`. Nothing else runs before verification. |
| 10 | `TestVerifiedCallback_ZeroOrMismatchRejected` | Zero `VerifiedCallback`; tenant or provider mismatch | `credential_unavailable`, 0 statements after `set_config` |
| 11 | `TestSimulationHandlers_SessionRevalidatedInDomainTx` | The session is closed between phase 1 and phase 3 | Rejected, 0 ledger rows |
| 12 | `TestOutboundResolve_InsideTenantTxRefused` | `OutboundResolver.Resolve` called inside `WithTenant` | `ErrOutboundCredentialUnavailable`, 0 store calls, 0 nested acquisitions |

### 9.4 Mutation checks

Each check is run by hand. Record the result in the implementing review, then revert the change.

| # | Mutation | Must fail |
|---|---|---|
| M1 | `Resolve` fetches inside its read-only `fn` (the root cause reintroduced) | 9.1 production pool size (criteria 3 and 4), test 2, test 5 |
| M2 | Remove the `txscope` guard | `…ResolveInsideTenantTxRefused`, `…FetchRefusedWithTxHeld`, test 12 |
| M3 | P = 4 | `TestFetcher_PerTenantCap`, test 2 (A's concurrency ≤ 1) |
| M4 | D = S (no degraded budget) | `…DegradedBudget_HealthyReserve`, test 3 |
| M5 | Breaker keyed by scheme only | `…TenantBreakerIsolated`, test 2 (`BreakerState(B)`) |
| M6 | Degraded callers wait `SlotWait` | `…DegradedCallersFailFast` |
| M7 | Remove the re-check | test 8 |
| M8 | Open the pre-verification transaction with `WithTenant` instead of read-only | test 9 |
| M9 | `SlotWait` 350 ms, or `MaxConcurrentStoreCalls` 8 | 9.1 kept test, as measured in ruling §1 (c) and (b) |
| M10 | A slot loss writes a 5 s negative entry | `…AdmissionLossNotNegativeCached` |

## 10. Consequences

- F-POOL-1 is closed at the root: the store-outage connection hold becomes 0. The per-backend
  breaker coupling between tenants is closed too.
- The §5 property now holds at the production pool size **and** at any pool size, with no
  config assumption.
- Webhook handling costs one extra connection acquisition (the read-only transaction; payments
  needs two) plus one primary-key read. Expected cost: about 1–3 ms.
- The internal API changes in `webhookauth` and in 3 orchestrators. About 40 test files migrate
  mechanically.
- Related, **not** part of F-POOL-1, recorded here so it is not lost:
  - **F-POOL-2 (open, Medium, not yet reachable).**
    - `payments.InitiateDeposit` calls the adapter's synchronous `Deposit` inside the tenant
      transaction.
    - That is harmless with the in-process MOCK. With a real HTTP PSP adapter, a tenant's PSP
      outage would pin connections in the same way.
    - The §4.1 guard means that wiring real outbound credentials into that transaction
      **fails closed**, so the problem cannot ship silently.
    - It must be resolved, by the same "no connection across external I/O" rule, before any
      real payment adapter is wired. That is a Stage 10.3-W3b or later decision for the
      orchestrator.

## 11. QA test-plan review

**Reviewer:** `qa`. **Scope:** §9 (test plan) and §9.4 (mutation checks) only, against
the human's adversarial/concurrency list (normal operation, one tenant's outage, multiple
affected tenants, recovery, connection exhaustion, cross-tenant isolation, financial requests
while a tenant is degraded, `TestStoreOutage_DoesNotPinPool` not weakened) and CLAUDE.md's
financial test list, for the paths this ADR touches (payments/casino callback money paths).
No implementation code reviewed; none exists yet (label `NOT IMPLEMENTED` stands).

**Verdict: CONFIRMED WITH CHANGES.**

Mapping of the human's list to named tests, all present with a measurable pass criterion:

| Requirement | Test(s) |
|---|---|
| Normal operation | `TestResolutionIsolation_NormalOperation` (9.3 #1) |
| One tenant's outage | `TestResolutionIsolation_OneTenantStoreOutage` (#2) |
| Multiple affected tenants | `TestResolutionIsolation_MultipleTenantsOutage` (#3), `..._SimultaneousOnset_Bounded` (#3b) |
| Recovery | `TestResolutionIsolation_Recovery` (#4) |
| Connection exhaustion | `TestResolutionIsolation_ConnectionExhaustion` (#5) |
| Cross-tenant isolation | `TestResolutionIsolation_CrossTenant` (#6), plus `TestFetcher_TenantBreakerIsolated` (9.2) |
| Financial requests while a tenant is degraded | `TestResolutionIsolation_FinancialDuringOutage` (#7) |
| Starvation test not weakened | §9.1 kept test: same three literal assertions retained, two added, pool reverted to 20 per the `security` ruling §3 replacement. **Confirmed not weakened** — the call-shape change (no outer transaction) is the fix itself, not a relaxation of what is measured. |

`TestStoreOutage_DoesNotPinPool_ProductionPoolSize` (pool 10) and §9.3's shared pool-10 fixture
together give the pool-10 coverage the `security` ruling's F-POOL-1 follow-up asked for.

CLAUDE.md's financial list, for the callback money paths this ADR touches: duplicates and
idempotency are covered (redelivery in #4 and #7); concurrency is covered (nearly every test);
retries are covered (redelivery = retries here); authorization is covered (#6, test 8, test 10);
auditability is covered for the negative case (global post-condition: 0 audit/ledger/tombstone
rows for a rejected callback) — the positive case (audit row written on a successful post) is
unchanged domain logic and is assumed to already be covered by the existing per-domain financial
suites, not re-tested here; that assumption should be stated, not implicit. Rollback is covered
by test 8's revoked-between-verify-and-domain-tx case. **Partial failure and reconciliation are
under-specified** — see items 5 and 6 below.

### Required changes

1. **Pool-size self-check for §9.3.** §9.1 requires each pool-10 test to assert
   `config.DefaultDatabaseMaxConns == 10`, so a change to the default forces this ADR to be
   revisited. §9.3's shared fixture states pool 10 once in prose but does not carry the same
   assertion. Add it to the §9.3 fixture, run once per test via the shared setup, so the same
   forcing-function applies to the adversarial suite.

2. **State `-race` explicitly for §9.3.** §9.1's suite is known to run under `-race` (per the
   `security` ruling's measurements). §9.3 introduces new concurrent access to the per-(scheme,
   tenant) breaker map and admission counters under real goroutine concurrency, and the plan
   never states the race detector is on for it. Add `-race` as an explicit requirement for the
   whole §9.3 suite (both [T] and non-[T] rows), not just an assumed inheritance from the package.

3. **CI time budget for `internal/httpserver`.** §9.3 adds 12 test names to `internal/httpserver`
   integration (6 are [T] and run in the isolated blocking step; the remaining ones — #4, #6, the
   3 domain variants of #8, the 3 domain variants of #9, #10, #11, #12, 11 test functions total —
   land in the main integration step, which is already ~300 s against a 10-minute `go test`
   timeout). The plan states no estimated added cost and no budget. Required: (a) state a target
   budget for what these 11 tests may add to the main step, measured in the implementing PR; (b)
   if the measured total risks the 10-minute ceiling, move the resolution-isolation suite into
   its own test file (e.g. `internal/httpserver/resolution_isolation_test.go`) that can be timed
   and, if needed, split into its own CI step — separate from the existing named isolated lane,
   per `security` ruling B condition 3 ("no other test may be added to this... lane... without a
   ruling specific to that test"). Do not fold new tests into the existing isolated lane to solve
   a budget problem without that ruling.

4. **Missing mutation proving the financial-during-outage regression.** §9.4's ten mutations map
   to specific tests, but none is uniquely tied to `TestResolutionIsolation_FinancialDuringOutage`
   (#7) — the human's explicit "financial requests while the degraded tenant is failing"
   requirement. Add a mutation (e.g., bypass the `(provider_id, provider_tx_id)` idempotency
   constraint on redelivery, or let `ReceiveVerifiedCallback` post before the re-check completes)
   that only #7 (or #7 combined with the global post-conditions) is shown to kill.

5. **Mutations for tests 3b, 4, 6, 11.** §9.4 has no mutation uniquely killed by
   `..._SimultaneousOnset_Bounded` (#3b), `..._Recovery` (#4), `..._CrossTenant` (#6), or
   `TestSimulationHandlers_SessionRevalidatedInDomainTx` (#11). Each is a distinct, named
   regression surface (onset bound, breaker-close-after-cooldown, per-tenant cache/breaker
   isolation, and the TOCTOU re-validation window respectively). Add one mutation per test (e.g.,
   for #6: key the positive cache without the tenant in the key; for #11: skip the phase-3
   re-validation) so the review's "would this test actually catch the regression it claims to
   catch" question has evidence for all named tests, not a subset.

6. **Partial failure at the re-check, distinct from a clean miss.** §7's table lists "a DB error"
   at the re-check as a branch with the same fail-closed outcome as a 0-row miss, but §9's test 8
   only exercises the 0-row (revoked) case. CLAUDE.md requires "partial failure" coverage for
   financial code. Add a named test (e.g.
   `TestReceiveVerified_RecheckDBErrorRollsBack`) that injects a DB error on `HandleRecheckSQL`
   (not a clean miss) and asserts the domain transaction rolls back with 0 rows written, same as
   the revoked case, so the DB-error branch in §7 has its own proof rather than sharing test 8's
   coverage by inference.

7. **State the reconciliation scope explicitly.** CLAUDE.md's "reconciliation" item is currently
   satisfied only implicitly, via the §9.3 global post-condition (`SUM(debits) == SUM(credits)`
   and balance == projection, checked after each scenario). That is adequate for this ADR's
   money-path change, but the ADR should say so explicitly: this covers point-in-time
   reconciliation after each concurrency scenario, not the scheduled hourly drift job, which is
   out of this ADR's scope and already owned by the ledger-finance test suite. Otherwise this can
   later be read as "no reconciliation test" or, conversely, as if this ADR were asserting
   coverage of the scheduled job.

8. **Confirm the pool/fixture is shared across the domain-parameterized rows.** Tests 8 and 9 are
   written "(each domain)" / "`_{Payments,KYC,Casino}`", i.e., three test functions each. Confirm
   in the implementing PR that all three per test share the same pool-10 fixture and its
   self-check assertion (item 1), rather than each domain variant standing up its own fixture at
   a different, undocumented pool size.

None of these changes touch the chosen design (§3–§8) or the invariants (§2); they are additions
to the test plan and CI wiring. Once items 1–8 are applied, this reviewer's verdict is
`CONFIRMED`. `security` co-signature on the constants and the isolated-lane test list (§11) is
still required independently of this review.

## 12. Human decisions

**None required.** Every choice here is a reversible engineering decision within the approved
Stage 10.3 scope. The following are not human decisions but still have to happen:
- `security` co-signature on the constants P = 1 and D = 2, on the breaker scope change, and on
  adding the [T] tests to the isolated CI lane;
- `qa` confirmation of §9;
- the orchestrator's sequencing of F-POOL-2.

## Security design review

Reviewer: `security`. Date: 2026-09-26. Reviewed against the ADR text as committed in `55dde0f`,
with the §11 QA review from `2720b5f`, and against the code at HEAD `4be9ce2`, which does not
change the cited code since `662aaae`. Code read: `internal/secretstore/fetcher.go`; `internal/providercred/resolver.go`
(including `service.go` `GetDirect` call sites); `internal/webhookauth/{scheme,resolver}.go`;
`internal/{payments,casino}/webhook_verify.go`; `ReceiveCallback` in all three orchestrators;
`internal/db/tenant_rls.go`; the three public webhook handlers; and the `payments`/`casino`/`kyc`
outbound adapter call sites. This is a design review. I ran no code and changed none.

### Verdict: CO-SIGN WITH CONDITIONS

The design fixes F-POOL-1 at its root: no pooled connection is held across secret-store I/O. It
also removes the tenant coupling through the backend-wide breaker. The chosen option is correct.
The rejected options were rightly rejected. With the conditions below, the design meets every
constraint the human set.

**F-POOL-1 stays OPEN and launch-blocking** until three things are done:
- the implementation lands;
- every §9 test passes, including the additions below, with M1–M13 recorded;
- `security` has reviewed the implementing diff.

This co-signature approves the design only. It does not close the finding.

### (1) Split verification vs webhook I1: preserved and strengthened

- Before verification, the statements that run are unchanged: payments EXISTS plus
  `HandleReadSQL`; KYC and casino `HandleReadSQL` only.
- They now run in a `READ ONLY` transaction, so PostgreSQL itself refuses writes, `nextval`, and
  `SELECT … FOR UPDATE/SHARE`.
- The domain transaction, including its ledger locks, is opened only after verification
  succeeds. Today it is open during verification. That is a real strengthening: an
  unauthenticated caller can no longer cause a read-write transaction to be opened.
- `READ ONLY` does **not** refuse advisory locks (`pg_advisory_xact_lock`) or read-only function
  calls. So the statement-capture tests (test 9) remain the **primary** I1 control, and
  `READ ONLY` is defence in depth. That is how the ADR words it in §2.2; it must stay worded that
  way.
- Accepted residual: payments' `ProviderAcceptsWebhook` is not re-checked in the domain
  transaction. Its time-of-check to time-of-use window grows by at most the fetch and verify time
  (≤ 2 s).
  - This is acceptable because I4 already defines revocation as removing the *credential*, which
    the re-check covers, and not disabling the capability.
  - Today's check is also only READ COMMITTED, so it is not linearizable either.
- Unauthenticated cost rises from one transaction to two per payments callback (two read-only
  transactions), and from one to one for KYC/casino. Both are short and independent of the store.
  This is generic request overload and belongs to the already-registered `PAYWH-RL-1` (webhook
  rate limiting). See the residuals below.

### (2) Revocation re-check: accepted; the `FOR SHARE` rejection is accepted

- The exposure window moves from [handle read, commit] to [re-check, commit]. The new window is a
  subset of the old one, and fetch and verify are outside it. The stated semantics of ADR 0093 §4
  are kept.
- The re-check binds `fingerprint`, so it pins the exact secret version that verified the
  callback. It also re-evaluates `not_after` on the database clock at a strictly later time, so it
  is at least as strict as the verify-time check.
- Rejecting `FOR SHARE` is **accepted**. A lock would make the revoke `UPDATE` queue behind
  in-flight bet and settlement transactions. It would also add lock traffic to the bet path. All
  of that would close a window that ADR 0093 already accepts and that is now narrower.
- Useful side effect: `Recheck` runs inside a transaction marked by `txscope`. The §4.1 guard
  therefore makes it **structurally unable** to reach the store.
- Useful side effect: running under the transaction's RLS with `tenant_id = $1` means a
  `VerifiedCallback` for tenant A presented inside a `WithTenant(B)` transaction gets 0 rows. So
  the re-check also binds the token to the transaction's tenant, not only to the parameter.
- **Gap (condition C4):** under `KeyImplicit` the credential that verified may be the
  verify_only *predecessor*. The re-check must use the handle of the credential that
  `VerifyInbound` **returned**, not `CredentialSet.Active`.

### (3) Opaque `VerifiedCallback`: sound in shape, not yet replay-tight

Forgery from outside the package is prevented:
- The fields are unexported.
- The zero value is rejected.
- Each domain has its own type, so a casino token cannot be passed to payments.

Tenant and provider mismatch is rejected. It is also caught by the RLS-bound re-check (see (2)).

Remaining gaps, fixed by conditions C1–C3:
- **Reuse / late use.** It is a value type, so a copy can be passed to `ReceiveVerifiedCallback`
  any number of times and at any later time. After the split, the signed-timestamp window is
  checked in phase 1 only. A retained token (retry loop, queue, a future async path) would be
  processed after `MaxSkew` without re-verification. Idempotency on `provider_tx_id` limits the
  financial effect, but the replay property of ADR 0022 §3 point 10 would silently stop holding.
- **Byte aliasing.** If the token keeps the handler's `body` slice, which is also used after the
  call, then any mutation between the phases makes phase 2 parse bytes that were never verified.
  No code does this today. It must be impossible by construction.
- **Secret lifetime.** The token carries `Credential.Secret`, which `HandleCallback` needs for its
  defence-in-depth re-verify. It must therefore redact in every formatting path, as `Credential`
  does (C15).

### (4) `txscope` marking and fail-closed guard: accepted as defence in depth

The guard can be bypassed in these ways:
- (a) A `context.Background()`/`TODO()` or a detached goroutine inside a `With*` callback. The ADR
  discloses this.
- (b) A new `With*` scope function that forgets `Mark`.
- (c) A transaction or connection obtained through `db.Pool.Raw()`. It has no non-test callers
  today.
- (d) A `TenantReader` implementation that runs `fn` on an *outer* transaction and hands back an
  unmarked context. Only `*db.Pool` does this in production.

Why this is acceptable:
- None of these can be reached from request input.
- The guard fails in the safe direction: a false positive gives a 401. A marked context cannot be
  unmarked.
- The API shape (no store-reaching method takes a `pgx.Tx`) and the kept test's criteria (4)–(5)
  are the primary controls.

Condition C6 closes (b) and (c) mechanically.

### (5) Fetcher fairness

- **Per-(scheme, tenant) breaker: ACCEPTED.**
  - The breaker map is reachable only after a handle row matched for a route-resolved active
    tenant. An unauthenticated caller therefore cannot create entries for arbitrary tenants.
  - An attacker can now degrade only the targeted tenant, not every tenant on the backend. That is
    strictly better.
- **Protection of the store itself is weakened in *rate*, not in *concurrency*.**
  - The ADR's §6 claim that global-outage load is "lower than today's 4" is **misleading**. Today
    one backend breaker drives load to about one probe per 15–60 s once it trips.
  - Afterwards, a global outage costs about N × 3 counting calls (× 2 attempts with the retry) to
    trip every tenant's breaker, plus about N probes per cooldown. That is linear in the number of
    tenants.
  - Concurrency stays ≤ S = 4 at onset and ≤ D = 2 afterwards, so the process and the store are
    still protected against a stampede.
  - Against a fast-failing store with N = 1000 tenants, the steady probe rate is about 17–33 req/s
    at the 60 s cooldown. That is acceptable for a managed secret store.
  - A backend-wide breaker gated on a quorum of distinct failing tenants is **not required**. It
    would bring back some cross-tenant coupling, and the rate is already bounded. The claim must
    be corrected and the bound pinned (C9).
- **P = 1: CONDITIONALLY ACCEPTED.**
  - A tenant whose three or more distinct refs are cold at the same time (after a deploy or a
    rotation) serializes them.
  - With about 150 ms store latency, the third ref waits more than `SlotWait` and gets a 401 with
    no stale fallback.
  - A synchronous casino bet then fails in a way the player sees, under **healthy** conditions.
  - C8 settles this by test, with P = 2 pre-approved as the fallback.
- **D = 2, degraded fail-fast, followers fail-fast, aborted probe not counted: ACCEPTED.**
- **No negative-cache entry on admission loss: ACCEPTED.** After the split a retry costs no
  connection, and a negative entry would turn contention into denial.
- **Onset residual (k ≥ 4 simultaneous unobserved outages leave 0 healthy slots for ≤ 2 s):
  ACCEPTED as disclosed.**
  - It affects cold fetches only and holds no connection.
  - Closing it would require pre-empting in-flight calls.
- **`GetDirect` outside S/P/D: ACCEPTED.**
  - It is used only by `PermProviderCredentialRequest`, which is platform-admin-only
    (`auth/permission.go:540`).
  - It runs under four-eyes and holds no connection once the guard is in place.

### (6) Test plan and isolated CI lane: per-test ruling (ruling B, condition 3)

Ruling B's conditions 1, 4 and 5 apply to every test admitted to the lane:
- blocking;
- no retry or rerun and no `-count` with any-pass logic;
- one `grep -- '--- PASS: <name>'` guard per test;
- a single failure is investigated, never rerun until green.

In addition, no [T] test may call `t.Parallel()`.

| Test | Ruling |
|---|---|
| `TestStoreOutage_DoesNotPinPool` | **Lane: stays.** The new shape is not a weakening. It is valid only together with M1, which must fail it at pool 10 (criteria 3–4), and with `…ResolveInsideTenantTxRefused`, which covers the old shape. Pool stays 20 (ruling A). |
| `…_ProductionPoolSize` | **Lane: ACCEPTED.** Same geometry and the same wall-clock proxy. |
| `…ResolveInsideTenantTxRefused` | **Main lane, not [T].** Replace "< 5 ms" with structural assertions (0 store calls, no slot or flight taken) plus a wall-clock bound of < 100 ms. A 5 ms bound under `-race` in a parallel `./...` run is a flake generator. |
| 9.2 Fetcher unit tests | **Main lane.** Same rule: assert outcome, store-call counts and "no wait started". Every wall-clock bound must be ≥ 2× the gap it discriminates, e.g. "fail-fast" means < `SlotWait`/2, not < 5 ms. `…PerTenantCap` may keep "≤ `SlotWait` + 50 ms" only as an upper bound, with a lower bound ≥ `SlotWait` − 25 ms, so that a healthy waiter is shown to have waited. |
| #1 `NormalOperation` | **Lane: ACCEPTED**, because of its p100 bound. |
| #2 `OneTenantStoreOutage` | **Lane: ACCEPTED.** |
| #3 `MultipleTenantsOutage` | **Lane: ACCEPTED.** |
| #3b `SimultaneousOnset_Bounded` | **Lane: ACCEPTED.** Its real 2.3 s wait is part of what it pins. |
| #4 `Recovery` | **Main lane.** Fake clock; no wall-clock bound allowed. |
| #5 `ConnectionExhaustion` | **Lane: ACCEPTED.** |
| #6 `CrossTenant` | **Main lane.** Must not be moved into the lane: its assertions are not timing-based. |
| #7 `FinancialDuringOutage` | **Lane: ACCEPTED.** It must also assert the ledger post-conditions, which it does. |
| #8–#12 | **Main lane.** |
| New tests from C1–C9 | **Main lane**, except C9's rate test, which uses a fake clock and so also runs in the main lane. |

This ruling is consistent with QA §11 item 3. That item may create a *separate* timed step
for the main-lane resolution-isolation file to manage the time budget. It must not use the
isolated lane for that purpose. `security` endorses QA items 4–6, in particular the re-check
DB-error test.

The literals 500 ms, `longSlack` 400 ms and ≤ 4 store calls are reviewed values and must never be
widened. The "< 500 ms" bounds in tests #2, #3, #5 and #7 are admitted to the lane **only** because
they measure pool admission. Adding any other test to the lane still needs its own ruling.

### (7) F-POOL-2: severity Medium; the scope is wider than stated; hard sequencing gate

- **Severity: Medium.** It has the same mechanism and the same cross-tenant impact as F-POOL-1.
  It is not reachable today because every adapter is an in-process MOCK.
- **The scope is a pattern, not one call.** The same "external I/O inside the tenant transaction"
  pattern exists at:
  - `payments.attemptDeposit` (`provider.Deposit`, plus `QueryStatus` in `resolveAmbiguous`, and
    repeated up to `MaxCascadeDepth`);
  - `casino` launch (`provider.Launch` after `CreateLaunchSession(ctx, tx, …)`,
    `casino/orchestrator.go:320–333`);
  - `kyc.CreateVerification` and `SubmitVerification` (both take `tx`);
  - `HealthStatus` in payments and casino.
- **The ADR's claim that the guard makes F-POOL-2 "fail closed … cannot ship silently" is
  conditional.** It holds only if a real adapter resolves its outbound credential per call through
  `OutboundResolver` *inside* that transaction. An adapter that resolves before the transaction,
  or that keeps a client with credentials already loaded, bypasses it completely.
- **There is also a financial-correctness aspect, for `ledger-finance`.**
  - If the domain transaction rolls back after the PSP accepted `Deposit`, the PSP holds a request
    whose `MerchantReference` intent row was never committed.
  - This is a dual-write hazard, separate from pool pinning.
- **Sequencing (binding):**
  - F-POOL-2 must be designed (an ADR under the same INV-POOL rule) and fixed **per domain before
    that domain's first non-MOCK adapter is wired**.
  - It is a launch-blocking entry in the next real-provider integration gate.
  - It does not need to ship in the same change as this ADR.
  - Register it in `docs/governance/task-registry.md` with this wider scope.

### Conditions

- **C1.** Make `VerifiedCallback` a pointer to an unexported struct that is **single-use**. The
  first `ReceiveVerifiedCallback` consumes it atomically, and every later use fails closed with
  `credential_unavailable`.
- **C2.** Make `VerifiedCallback` **age-bounded**. It records a monotonic `verifiedAt`, and
  `ReceiveVerifiedCallback` rejects it after 30 s. That keeps the phase-2 start well inside
  `MaxSkew`, so the replay window of ADR 0022 §3 point 10 still holds end to end.
- **C3.** On entry, `VerifyCallback` copies the body and clones the headers. It verifies the
  copy, stores the copy, and passes exactly that copy to `HandleCallback`.
  `ReceiveVerifiedCallback` takes no `Inbound` parameter. `VerifiedCallback` implements redacting
  `String`, `GoString`, `Format`, `LogValue` and `MarshalJSON`, and has no exported serialization.
- **C4.** `HandleID` is set per handle row in `credentialFor`. `Recheck` uses the handle of the
  credential that `VerifyInbound` **returned** (Active or Previous).
  - A real `Recheck` fails closed on a zero `HandleID`.
  - `KindSplitResolver.Recheck` fails closed for an unregistered provider id.
  - The MOCK `Recheck` is reachable only through the synthetic branch.
- **C5.** Add the following tests:
  - test 8 variant: a `KeyImplicit` callback verified by the verify_only predecessor, whose handle
    is then revoked between the phases, is rejected with 0 rows;
  - test 10 variants: reuse (C1), age > 30 s (C2), and a `WithTenant(B)` transaction given a valid
    `VerifiedCallback` for A with `tenantID = A` (0 rows, 0 statements after the re-check);
  - a body-mutation-between-phases test (C3);
  - a formatting and redaction test (C3).
- **C6.** Add a table-driven test that runs every `db.Pool` method whose name starts with `With`,
  found by reflection so that a new scope function is covered automatically, and asserts
  `txscope.Held(ctx)` inside `fn`. Add a source guard test that fails on any non-test use of
  `(*db.Pool).Raw()` outside `internal/db`. `txscope` exposes no unmark function, and its context
  key type is unexported.
- **C7.** §2.2 and the ADR 0022 §3 point 9 amendment keep the capture tests as the primary I1
  control. They must state that `READ ONLY` does not stop advisory locks.
- **C8.** Extend test #1 with a tenant that has 4 distinct cold refs requested concurrently, with
  150 ms injected store latency. The pass criterion is 0 rejections.
  - If P = 1 cannot pass it, **P = 2 is pre-approved** without re-review, provided that
    P ≤ D = 2 < S = 4 still holds.
  - In that case §6 must be updated to "one tenant's outage onset leaves ≥ 2 healthy slots" and M3
    must be re-targeted.
  - Widening `SlotWait` is **not** permitted as an alternative.
- **C9.** Correct §6 "Global outage" to state the rate bound (see (5)) instead of "lower than
  today". Add a fake-clock unit test: N = 50 tenants and a store that fails fast, run over 120 s,
  with total store attempts ≤ N × 3 × (1 + `StoreMaxRetries`) + (probes allowed by each tenant's
  cooldown schedule). Add a metric or `warn` log when ≥ 3 distinct tenants on one scheme are
  degraded at once, so that a real backend outage is visible as one event.
- **C10.** Record mutations M1–M10 and these new ones in the implementing review:
  - **M11:** allow `VerifiedCallback` reuse → the C1 test must fail;
  - **M12:** make `Recheck` use `Active` → the C4/C5 predecessor test must fail;
  - **M13:** drop `Mark` from one `With*` → the C6 test must fail.

  M1 failing the kept test's production-size variant is **mandatory evidence** that the new call
  shape is not vacuous.
- **C11.** Register F-POOL-2 with the wider scope from (7), as launch-blocking for any non-MOCK
  adapter in payments, casino or KYC. Refer the dual-write aspect to `ledger-finance`.

### Residuals and scope

**Residuals (not F-POOL-1; recorded so they are not lost):**
- Public webhook routes have no rate limiting (`PAYWH-RL-1`, registered and deferred). After this
  fix it is the dominant remaining way for one tenant's traffic to affect another's pool share.
  It should be scheduled no later than the first real provider.
- The disclosed onset residual in §6.

**Not in scope:**
- the AWS Secrets Manager adapter and its failure classification;
- whether the pool-level behaviour of the MOCK-backed simulation handlers matches production;
- runtime verification (nothing was executed);
- implementation code, which does not exist yet.

Co-signing this design does not make the resolver path "secure" in general. The implementing diff
needs its own `security` review against C1–C11.

## Implementation record (2026-09-26)

**Label: IMPLEMENTED.**
- All security conditions C1–C11 and QA changes 1–8 are applied. Every §9 test passes locally
  under `-race`.
- **F-POOL-1 remains OPEN until `security` reviews the implementing diff.** This record is not a
  security sign-off.
- It has not been run on GitHub CI. The workflow change was validated only locally (YAML parse and
  the equivalent local commands).

**Commits** (branch `claude/focused-wright-jw88w9`):

| Commit | Content |
|---|---|
| `7773649` | db: txscope marking and `WithTenantReadOnly` |
| `4779958` | the fix: two-phase webhooks, `VerifiedCallback`, `Recheck`, Fetcher fairness, guards, handlers, test migration |
| `8d973ef` | §9.3 suite and re-check tests |
| `18a57b8` | CI lanes |
| `2354475` | NormalOperation load profile and the `VerifyCallback` guard test |
| `1c5fb7e` | tightened #3b |
| the docs commit | this record, the ADR amendments, the registry and the evidence |

**Where the implementation differs from the design text** (for the security implementation
review):
1. **P = 2, not 1.** Condition C8 required it: P = 1 fails the cold-start test (evidence C8-P1).
   §4.2 and §6 are updated. With P = 2, a single tenant's outage leaves at least 2 healthy slots,
   not 3. The simultaneous-onset residual starts at k ≥ 2 tenants.
2. **`HandleRecheckSQL` also binds `domain`, `provider_id` and `purpose`.** This is narrower than
   the design (§5).
3. **One token type for all domains.** `VerifiedCallback` is a single type,
   `webhookauth.VerifiedCallback`. Each domain exposes it as a type alias and seals it with its
   domain tag (`WebhookDomain`). `Redeem` fails closed on a domain mismatch, and
   `TestVerifiedCallback_ZeroOrMismatchRejected` covers "other domain".
   - Security (3) described "each domain has its own type" as a compile-time property. Here it is a
     runtime check. Forgery from outside is still impossible: the fields are unexported, and the
     only constructor, `VerifyAndSeal`, runs `VerifyInbound`.
   - **Flagged for the security implementation review.**
4. **The constant check runs at package init.** It panics unless 0 < P ≤ D < S. The design said
   `NewFetcher` would panic.
5. **Test-only bridge.** `receiveCallbackInTx` (in `receive_bridge_test.go`, in each domain
   package) runs both phases on the caller's transaction.
   - About 40 existing domain test files that assert domain behaviour (ledger, idempotency, locks,
     audit) use it, with MOCK resolvers only.
   - It deliberately does what production must not do, and it can reach no store.
   - Every real-resolver test and every test that pins an ADR 0094 property uses the real
     two-phase shape.
6. **`simulationBetweenPhasesHook`.** This is an unexported package variable in
   `internal/httpserver/casino_play_handlers.go`. It is nil in production and set only by
   `TestSimulationHandlers_SessionRevalidatedInDomainTx`. It is a disclosed test seam.
7. **How the HTTP suite measures "no transaction held longer than `longSlack`".**
   - The design assumed a wrapper around the reader. The handlers use `*db.Pool` directly, so no
     wrapper can be inserted on that path.
   - Instead, `phasecapture.XactAgeSampler` samples `pg_stat_activity` for the oldest open
     transaction of the pool under test (a unique `application_name`) on a separate connection.
     This is the F-POOL-1 symptom measured on the real handler path.
   - The kept `providercred` test still uses the reader wrapper, as designed.
8. **Test 1 load profile.** `NormalOperation` still sends 30 callbacks per tenant. At most 10 are in
   flight per tenant, and each (tenant, i) uses its own seeded player and wallet.
   - With all 60 chains in flight at once, `-race` p100 was 400–590 ms at pool 10, and 3 of 5
     alone runs failed the unchanged 500 ms bound. That is pool throughput (about 4 short
     transactions per callback), not a resolution effect.
   - After the change, p100 is 231–300 ms. The 500 ms bound is unchanged.
   - Flagged for the security implementation review, because test 1 was admitted to the lane for
     its p100 bound.
9. **#3b is stricter than first written.** The healthy onset callback must finish within
   `SlotWait` + 150 ms whatever the outcome. This was prompted by the survivor M15.
10. **#6 is stronger.** It opens A's own breaker, and B makes a cold fetch while A's breaker is open.
    The evidence mutation for #6 is M5. QA's suggested mutation, M17, is equivalent for #6 (see the
    evidence notes).

**§9 tests to files and lanes.** All run under `-race` (QA item 2). Every §9.3 test uses the
pool-10 fixture (`phasecapture.Pool10`, which asserts `config.DefaultDatabaseMaxConns == 10`). That
includes the test-8 and test-9 domain variants, which share the fixture (QA items 1 and 8).

| Tests | File | Lane |
|---|---|---|
| `TestStoreOutage_DoesNotPinPool` (pool 20), `_ProductionPoolSize` (pool 10), `_ResolveInsideTenantTxRefused` | `internal/providercred/resolver_integration_test.go` | the first two in the timing lane |
| 9.2 unit tests, plus C8 `TestFetcher_ColdStartFourRefs_NoRejection` and C9 `TestFetcher_GlobalOutageRateBound` | `internal/secretstore/fetcher_fairness_test.go` | main |
| #1–#7, #11 | `internal/httpserver/resolution_isolation_integration_test.go` | #1, #2, #3, #3b, #5, #7 in the timing lane; #4, #6, #11 main |
| #8 per domain, QA-6 `TestReceiveVerified_RecheckDBErrorRollsBack`, #10 casino plus C5 variants, C3 body mutation, `VerifyCallback` guard | `internal/{casino,payments,kyc}/resolution_recheck_integration_test.go` | main |
| #9 `TestPointNineCapture_*` | the existing domain capture files, rewritten to the two-phase shape | main |
| #10 unit plus C1/C2/C3/C4 | `internal/webhookauth/verified_test.go` | main |
| C4/C5 predecessor, #12 outbound | `internal/providercred/recheck_integration_test.go` | main |
| C6 | `internal/db/txscope_marking_integration_test.go`, `internal/db/raw_guard_test.go` | main |

**Results.**
- `go test -race ./...`: pass.
- `go test -race -tags=integration` on the main lane (everything except the 8 timing-lane tests):
  pass, 334 s wall-clock for the whole repository on 4 vCPU.
- Final re-run on the final code:
  - The main lane of the touched packages passes. That is `httpserver`, `payments`, `casino`,
    `kyc`, `providercred`, `db`, `secretstore/...`, `webhookauth/...` and `reconciliation`, in
    295 s; `internal/httpserver` took 293 s of it.
  - **Each of the 8 timing-lane tests was run alone 5 times: 40/40 PASS.**

  | Timing-lane measurement over those runs | Range |
  |---|---|
  | `NormalOperation` worst callback latency | 226–287 ms |
  | Oldest pooled transaction in the outage scenarios | at most about 86 ms, against the 400 ms `longSlack` |
  | Kept test and its pool-10 variant, duration | 2.16–2.26 s |

- The earlier 5× run on the pre-fix `NormalOperation` was 2/5, which led to item 8 above.
- `gofmt`, `go vet ./...` and `go vet -tags=integration ./...` are clean.
- `golangci-lint run ./...` (v2.9.0) reports 0 issues.
- With `--build-tags integration`, `golangci-lint` still reports the pre-existing repository
  findings; none of them are in files this change touches.

**CI time budget (QA item 3).**

| Item | Value |
|---|---|
| Budget for the main-lane additions | ≤ 15 s |
| Measured main-lane additions | #4 0.9 s, #6 about 3.5 s, #11 0.7 s, test-8/-10/-12, C-series about 6 s total; `internal/httpserver` went from about 300 s (the same machine at `103b033`) to 310 s |
| `internal/httpserver` against the 10-minute `go test` timeout | about 52% |
| New timing-lane step | about 20 s (`httpserver`) plus about 5 s (`providercred`) |

The suite is in its own file, as QA asked. No separate main-lane step is needed.

**Reconciliation scope (QA item 7).** The §9.3 global post-conditions are point-in-time checks
after each scenario:
- `SUM(debits) == SUM(credits)` per tenant;
- every ledger account's projection equals its rebuild from `ledger_entries`.

They are **not** the scheduled hourly drift job, which the ledger-finance suite owns. The positive
audit case (an audit row written on a successful posting) is unchanged domain logic. The existing
per-domain financial suites cover it, and they still run through the test bridge.

**Mutations (C10, QA items 4–5).** See `docs/plans/stage-10.3-planning/evidence/f-pool-1-mutation-kill.txt`.
- 28 mutations were run. 27 were killed by at least one named test.
- **M1**, the root cause re-introduced, fails `TestStoreOutage_DoesNotPinPool_ProductionPoolSize`:
  the unrelated query took 1.17 s. That is the mandatory evidence.
- **M14** (casino idempotency key) is a recorded survivor. It is equivalent on the redelivery path
  because redelivery short-circuits earlier. QA item 4's financial mutation is **M14b** instead:
  verification moved back into the domain transaction at the casino handler. #7 kills it.
- **M17** is equivalent for #6. #6's evidence mutation is M5.

**Not satisfied / open:**
- The `security` implementation review is still owed, so F-POOL-1 stays open.
- The workflow has not been executed on GitHub CI.
- F-POOL-2 is registered but not started.
- PAYWH-RL-1 is not started.


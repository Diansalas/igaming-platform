# 18 - Code review: F-POOL-1 implementation (ADR 0094) + CODE-HYGIENE-10.3-1 fix round

- **Reviewer:** `code-reviewer` (independent of the implementer).
- **Scope:** commits `7773649`, `4779958`, `8d973ef`, `18a57b8`, `2354475`, `1c5fb7e`, `f85c0b8`
  (F-POOL-1 / ADR 0094), and `4ae9bb4` (the fix round for `16-code-hygiene-review.md`). Reviewed
  at HEAD `0cbb574` (the merge commit).
- **Not in scope:** financial correctness is ruled on by `ledger-finance` and security properties
  by `security`. Where I found concerns in those areas I pass them on (marked "surface to") and do
  not rule on them.

## Verdict: READY WITH FOLLOW-UPS

- I found no confirmed correctness bug on the money paths.
- The two-phase split is complete. Every production webhook and simulation entry point uses it,
  and no production path resolves a credential inside a transaction.
- The domain logic after `Redeem` is byte-for-byte what `ReceiveCallback` used to run, so
  idempotency, duplicate and replay behaviour on casino bet/win/rollback, payment deposit/reversal
  and KYC status is unchanged. The payments domain has no withdrawal webhook callback, so there
  was nothing to migrate there.
- The follow-ups are one likely CI-flake source, two tight or undecayed fairness details, test
  gaps, and some things to pass on to other reviewers. None of them blocks F-POOL-1 going to the
  `security` implementation review.

What I ran:
- `go test -race -count=1` on these unit packages, all passing: `secretstore/...`,
  `webhookauth/...`, `txscope`, `providercred`, `db`, `payments`, `casino`, `kyc`.
- `go test -race -count=10 -run TestFetcher_` on `internal/secretstore`: 10/10 pass.
- `go vet -tags=integration` on the touched packages: clean.
- I did no long integration runs, because the DB is shared.

**Working tree note:** while I was reviewing, `internal/secretstore/fetcher.go` gained an
uncommitted edit marked `// MUTATION` in `degradedLocked`. It came from another reviewer's
mutation run, not from me. I did not touch it. It must not be committed.

---

## Findings (most severe first)

### R-1 (Low-Medium; CI reliability): `TestResolutionIsolation_CrossTenant` asserts wall-clock bounds in the contended main lane

`internal/httpserver/resolution_isolation_integration_test.go`, `TestResolutionIsolation_CrossTenant`:
- The test calls `w.wantOK(...)` 12 times, and `wantOK` fails if `r.latency >= isoBound`
  (500 ms).
- The test runs in the main integration step. That step runs every package's `-race` binary
  concurrently, which is exactly the CPU contention that caused CI #347 and led to CI-342 and
  security ruling (6).
- ADR 0094 §9.3 test 6 has **no latency criterion** (ADR line 484). Its criteria are 401 with 0
  writes, per-tenant verification, and no change to A's breaker or store namespace.
- So the test imposes a timing bound the ADR never asked for, outside the lane that security
  admitted timing bounds to.

**Failure scenario:** on a loaded runner, one B bet in CrossTenant takes 520 ms. It is scheduler
delay, not a resolution effect. The blocking main integration step goes red, and the lane
separation meant to prevent that does not apply to this test.

**Fix:**
- Use a status-only helper in CrossTenant (and in any other main-lane test that uses `wantOK`).
  The only other main-lane test in that file, Recovery, already checks status only.
- Alternatively, get a security ruling admitting CrossTenant to the timing lane. The status-only
  helper is simpler.

### R-2 (Low; test reliability, contradicts its own header): `TestFetcher_PerTenantCap` upper bound is +50 ms

`internal/secretstore/fetcher_fairness_test.go:78`:
- The test asserts `elapsed <= SlotWait+50ms` in the unit lane (`go test -race ./...`, all
  packages in parallel).
- The file header says "every bound is >= 2x the gap it discriminates".
- The upper bound here separates "waited SlotWait" from "waited much longer", for example
  `StoreCallTimeout`, which is 2 s. A 50 ms margin is not 2x anything.
- It passed 10/10 locally, but it has the same sensitivity to scheduler delay as CI #347.

**Fix:** widen the upper bound to something like `SlotWait + 500ms`. That still kills a
"waits StoreCallTimeout" mutant (M15's shape). Keep the lower bound, which is the one that
matters: it proves a healthy caller does not fail instantly.

### R-3 (Low; fairness semantics): "degraded" has no time decay

`degradedLocked` treats a tenant as degraded while `b.consecutive > 0`. Only that tenant's next
completed store call resets `consecutive`: a success, or a non-counting error. The value does not
decay with time.

**Failure scenario:**
1. Tenant T has one transient store timeout. `consecutive` becomes 1 and the breaker stays
   closed.
2. T's cache stays warm, so T makes no store call for hours.
3. After a rotation or cache TTL expiry, T's next cold fetch happens while S is busy with healthy
   traffic.
4. T is still classified as degraded, so it never waits for admission and only competes for the
   D budget. It gets an immediate `credential_store_unavailable` 401 on a callback that would
   have succeeded after a short wait.
5. The same stale entries are also counted by `warnMultiTenantDegradedLocked`. Three tenants,
   each with one old blip, trigger `secret_store_multi_tenant_degraded` even though nothing is
   degraded now.

The impact is small. It needs a busy S plus a stale single failure, and the result is a retryable
401 with no connection held.

**Fix (any one):**
- record the time of the last counting failure and treat `consecutive > 0` as degraded only
  within, say, `NegativeTTLCounting` or `BreakerInitialCooldown` of it; or
- prune a closed breaker with `consecutive < BreakerTripThreshold` after a quiet period.

This is a fairness constant/semantics change, so it needs `security`'s agreement (ADR 0093 A4).

### R-4 (Low; test gaps behind stated claims)

- **Payments simulation re-validation is untested.** Commit `4779958` says "the casino/payments
  simulation handlers re-validate their session/intent in the domain transaction". Only casino
  has a test: `TestSimulationHandlers_SessionRevalidatedInDomainTx`, which also has a mutation
  (M18). The payments step-3 checks in `payment_deposit_simulation_handlers.go`
  (`requireDepositAwaitingCallback` plus the provider-id/reference comparison) have no seam, no
  test and no mutation. The mutation may be equivalent for money, because
  `receiveDepositCallback`'s own state checks and the unique constraint stand behind it. The
  claim is still untested.
- **The casino and KYC `VerifyCallback` txscope guards are untested.** M2d and
  `TestVerifyCallback_InsideTxRefused` cover only payments.
  - On the real-resolver path this is covered one layer down by `Resolver.Resolve`'s own guard
    (M2b).
  - With a MOCK resolver, removing the casino or KYC domain guard survives every test.
  - Add the same test for both domains (about 20 lines each), or say explicitly that the resolver
    guard is the tested control.

### R-5 (Low; surface to `ledger-finance` and `security`, not ruled here): transient failures after verification become a non-retry-looking 401 on money paths

- `Resolver.Recheck` folds every error into `ErrCredentialUnavailable`, which becomes the uniform
  401. That includes a transient DB error, not only `pgx.ErrNoRows`.
- Admission loss and a follower's `SlotWait` timeout also become a 401
  (`credential_store_unavailable`).
- ADR 0094's failure table documents this (lines 390-393), so it is not a hidden deviation.

Two points for the domain owners:
- C5's uniformity rationale (no oracle for whether a handle exists) protects
  **unauthenticated** callers. `Recheck` runs only after the sender proved it holds the secret. A
  non-`ErrNoRows` error there could be a 5xx without leaking anything.
- Whether a real PSP or aggregator redelivers after a 401 is PROVIDER DEPENDENT.
  - For a payment deposit callback, a DB blip during `Recheck` then loses the callback until
    daily reconciliation.
  - For a casino bet it is a player-visible failure.
  - ADR 0094 made contention-driven 401s more likely (P = 2, `SlotWait` = 250 ms, followers fail
    fast), although healthy-latency tests show 0 rejections.
- `ledger-finance` should decide whether the reconciliation-backstop assumption holds per domain.
  `security` should decide whether post-verification errors may leave the uniform 401.

### R-6 (Informational; test evidence strength): the INV-POOL end-to-end control has no in-test positive control, and one "control" cannot fire

- `phasecapture.XactAgeSampler` filters on `application_name`. If that parameter were ever
  silently not applied (a key=value DSN instead of a URL, a pooler, a driver change), the query
  would return 0 rows and every `assertNoLongTx` would pass vacuously.
- Today the sampler's effectiveness is shown only by the recorded M14b run, which observed
  29.96 s. That is good evidence, but it is not re-checked on each CI run.
- Suggested fix: add a small positive-control subtest in the file that holds a transaction on the
  named pool for about 500 ms and asserts `MaxAge >= 400ms`.
- `isoWorld.storeCallsWithTx` counts store calls whose ctx is txscope-marked. `Fetcher.Fetch` and
  `Router.GetDirect` refuse a marked ctx *before* calling the store, so this counter cannot be
  non-zero unless the guard is also removed.
  - It is a restatement of the guard, not independent evidence. It still has value if it catches
    a future store path that bypasses both entry points.
  - It should not be counted as a separate INV-POOL control in the evidence narrative.

### R-7 (Informational; comment accuracy)

- The doc comments on `ReceiveVerifiedCallback` (casino, KYC and payments) say "no statement of
  any kind ran in THIS transaction before it" / "Redeem is the FIRST thing in this domain
  transaction". That holds for the public webhook routes.
- It does not hold for the simulation callers. `runCasinoPlaySimulation` step 3 and the deposit
  simulation step 3 run their session/intent reads before `Redeem`.
- This is not a regression. Those routes are authenticated player routes on MOCK adapters, and
  they read in the same order as before. Only the comment overclaims. Qualify it as "on the
  webhook route".

### R-8 (Informational; robustness, pre-existing pattern)

These Fetcher code paths call `f.fingerprint(...)` (`matches`) and `f.logger.Warn` while
holding `f.mu`, with no deferred unlock:
- the positive-cache hit;
- the result switch in `own`;
- `warnMultiTenantDegradedLocked`.

A panic there would leave `f.mu` locked. In `own` it would also self-deadlock, because the
deferred `release()` calls `f.mu.Lock()` on a mutex this goroutine already holds. `net/http`
recovers the handler panic, so the process would keep serving while every later `Fetch` blocked
forever.

The platform fingerprint is HMAC-SHA256 and cannot realistically panic, so this is not a live
bug. If this code is touched again, consider `defer f.mu.Unlock()`-style sections or a
`recover` in `own`.

### R-9 (Informational; surface to `security`): payments capability EXISTS is no longer evaluated inside the domain transaction

- `ProviderAcceptsWebhook` used to run as the first statement of the domain transaction. It now
  runs only in phase 1's READ ONLY transaction.
- The window between the check and the posting has grown from about 0 to the phase-1 duration
  (bounded by `StoreCallTimeout`, 2 s).
- Under I4 the capability is not a revocation mechanism (revocation is by credential, and
  `Recheck` covers that), so I consider this negligible. I note it only because ADR 0094's
  "differs from design" list does not mention it.

### R-10 (Informational): a cancelled owner fails its followers

`Fetcher.own` passes the **owner's** request ctx to `admit`. If the owner's client disconnects
while waiting for a slot, every follower joined to that flight gets `ClassUnavailable`, even
though the followers' own contexts are live. They would have waited at most `SlotWait` anyway,
so the impact is small.

To fix it, `admit` could use a context detached from the owner and bounded by `SlotWait`, as the
store call already does with `WithoutCancel`.

---

## Verified correct (no finding)

- **Every handler migrated.**
  - Production callers of `VerifyCallback`/`ReceiveVerifiedCallback` (grep, non-test):
    - casino webhook (`casino_handlers.go`);
    - payments webhook (`deposit_handlers.go`);
    - KYC webhook (`kyc_admin_handlers.go`);
    - casino wager/win/rollback simulation (`runCasinoPlaySimulation`);
    - deposit simulation.
  - Each one runs phase 1 on `r.Context()`/`deps.DB`, outside any `With*` callback. The only
    `webhookPreamble` users are the three webhook routes.
  - `ReceiveCallback` survives only in `//go:build integration` `receive_bridge_test.go` files.
  - `OutboundResolver` has no production caller.
  - `providercred` service `GetDirect` calls happen outside `With*` callbacks.
- **Resolution never happens inside a transaction.** The real `Resolver.Resolve` reads handles in
  its own `WithTenantReadOnly`, which commits before `credentialFor` fetches.
  `Fetcher.Fetch`, `Router.GetDirect`, both resolvers and all three `VerifyCallback`s fail closed
  on a marked ctx.
- **txscope marking.** Every `db.Pool.With*` passes `txscope.Mark(ctx)`: `withTenantTx` (and so
  `WithTenant`, `WithTenantSnapshot` and `WithTenantReadOnly`), `WithoutTenant`,
  `WithPlatformAdmin`, `WithPlatformService`, `WithSessionLookup`, `WithPrincipalScope`,
  `WithPlayerScope` and `WithCredentialTokenLookup`. The reflection test covers new scopes
  automatically and asserts at least 10. There are no non-test `Begin`/`Acquire`/`pgxpool`
  uses outside `internal/db`, and the `Raw()` source guard is sound.
- **Redeem.** It is consumed atomically first (CAS) and bound to domain, tenant and provider.
  The age check is monotonic. `Recheck` runs under the domain transaction's RLS plus an explicit
  tenant predicate, and binds handle id, fingerprint, domain, provider and purpose.
- **Rollback leaves zero rows.** Any `Redeem` failure is an `AuthError` returned from the
  `WithTenant` callback, so the deferred Rollback runs. Re-check revoke, expire, DB error and RLS
  mismatch are tested with ledger rows, balance and debit/credit sums asserted, and M7/M19 are
  killed.
- **No transaction retry wrapper exists** anywhere in `db` or `httpserver`, so the single-use
  token cannot be burned by an automatic retry of the domain transaction.
- **Error mapping.** Phase-1 errors are `AuthError`s, except payments' EXISTS DB error, which is
  a 500 as before. So `recordCasinoCallbackRejection` still no-ops on everything before
  verification.
- **Fetcher accounting.**
  - `admit` records the degraded flag at admission and `release` uses that same flag, so
    `degradedInFlight` cannot drift.
  - `release` is deferred immediately after a successful `admit`. Store panics are recovered in
    `callStore`, and the store call runs detached from caller cancellation but bounded at 2 s.
    Slots are therefore always released (with the R-8 caveat).
  - `inFlightTenant` deletes an entry when it reaches 0.
  - Admission loss aborts a probe without counting it and writes no negative entry.
  - After a store call completes, the breaker is looked up again (`breakerLocked`), and a breaker
    is never pruned while its probe is in flight.
- **Per-tenant maps are bounded.**
  - `breakers` and `inFlightTenant` are keyed by (scheme, tenant) and reachable only after a
    handle row matched for a route-resolved tenant. They are bounded by real tenants × schemes,
    and closed/zero breakers are pruned.
  - `lastWarn` is keyed per scheme.
  - Both LRUs are capped at 1024.
- **Breaker transitions per tenant:**
  - closed to open at 3 consecutive counting failures;
  - open to half-open after the cooldown, with a single probe;
  - a probe failure doubles the cooldown, capped at 60 s;
  - a probe success resets to closed with the initial cooldown;
  - a non-probe result in half-open does not close the breaker.
  - These are tested by `TestFetcher_TenantBreakerIsolated` and
    `TestResolutionIsolation_Recovery`, and M5/M16 are killed.
- **CI lanes.**
  - The `-skip "^(…)$"` list and the timing-lane `-run` lists contain exactly the same 8 names.
  - The per-name `--- PASS: <name> ` guard (with its trailing space) cannot be satisfied by a
    prefix-named test.
  - The timing lane uses `-count=1` with no retry. Go 1.26 supports `-skip`.
- **Test quality.**
  - The §9.3 tests assert what their doc comments claim: statuses, posting counts, balances,
    debit/credit sums, breaker states and store-call counts.
  - The mutation evidence is consistent with the tests I read.
  - The survivors M14 and M17 are honestly recorded, with sound equivalence arguments.
  - `TestResolutionIsolation_CrossTenant` (a) is **not** vacuous. A's secret is served warm from
    the positive cache before the breaker is consulted, so the 401 there does come from the key
    mismatch.
- **Docs and labels.**
  - ADR 0094 is marked "ACCEPTED — IMPLEMENTED", and it states explicitly that F-POOL-1 stays
    OPEN pending the `security` implementation review. That is accurate and not a sign-off
    claim.
  - The "differs from design" list is candid: one token type (a runtime domain check instead of
    a compile-time one), P = 2, the NormalOperation load profile, and the sampler instead of a
    reader wrapper.
  - The F-POOL-1 registry row is OPEN, and the F-POOL-2 row is NOT IMPLEMENTED with the scope
    security C11 requires. Both are accurate.
  - "Workflow not executed on GitHub CI" is disclosed.

## CODE-HYGIENE-10.3-1 fix round (`4ae9bb4`) against `16-code-hygiene-review.md`

| Finding | Status | Evidence |
|---|---|---|
| F-1 (Medium) | **Closed** | `stagedMigrations0075` holds back every migration above 0098 (option (a), the 0092 pattern). The three 0075 tests migrate the staged directory, so `wantDown` and `wantDirty` stay valid when a 0099 lands. Comments and the registry row are corrected. |
| F-2 (Low) | **Closed as surfaced** | `derivedTokenBytes` plus entry-level `String`/`GoString`/`Format` redaction, with `%v`/`%+v`/`%#v` tests on value and pointer forms. The long-term `Secret.Wipe()` question is explicitly left with `security` in the registry row. F-POOL-1 did not add `Wipe`, which is consistent with that. |
| F-3 (Low) | **Closed** | `BoundEvictionZeroesTokenBytes` and `RotationZeroesEvictedTokenBytes` capture the entry *before* the evicting `Put` and assert zeroed bytes plus `order.Len() == len(entries)`. `ConcurrentPutGetRotate` exists. |
| F-4 (Low) | **Closed** (by correcting the wording) | The error text is left as-is, the `tenant_snapshot.go` comment is fixed, and the registry no longer claims "same error text". |
| F-5 (Info) | **Closed** | The parity-test comment now states exactly what the test checks and that the shared helper is the real control. |

The registry row "Closed except F-2 (surfaced to `security`)" is accurate.

## Required / recommended follow-ups

1. **R-1:** remove the wall-clock assertions from main-lane `CrossTenant`, or have them admitted
   to the timing lane. Do this before the workflow first runs on GitHub CI.
2. **R-2:** widen `TestFetcher_PerTenantCap`'s upper bound in line with the file's own 2x rule.
3. **R-4:** add the payments-simulation re-validation test and the casino/KYC `VerifyCallback`
   guard tests, or correct the claims.
4. **R-3, R-6 to R-10:** optional hardening and wording fixes. They can be tracked on the
   F-POOL-1 row.
5. **R-5 and R-9:** passed to `ledger-finance` and `security` for their rulings.

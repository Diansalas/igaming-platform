# A8 operating-market blocking: independent security review (live)

Reviewer: `security` specialist. Branch `a8-market-blocking`, commit `d148013`, base `9b5a25f`. Reviewed on
2026-10-15 against a private database (`sr8_*`, built from this worktree's migrations, dropped afterwards). No
roles or passwords were changed. I wrote no fixes. The probe tests were temporary and have been deleted; their
source is kept outside the repo.

## Verdict

**APPROVED for merge.** I found no unresolved security finding in the gate's own logic. The design fails closed
on every error path I could reach. The country cannot be influenced by the client. All refusals look identical
to the player, and the audit rows contain no country and no IP.

**Launch-blocking, not merge-blocking:** SR8-1 (sportsbook placement is not gated) and SR8-2 (closing a market
does not end exposure that is already running). Neither is made worse by A8. Both mean the gate is not yet a
complete operating-market control. Production launch must not rely on it while they stay open. That is a human
decision, made through the orchestrator.

## Scope

**In scope:** `internal/marketgate`, `internal/geolocation/locator.go`, the wiring in `auth_routes.go`,
`deposit_handlers.go`, `deposit_v2.go` (phase A), `casino_handlers.go` and `casino/orchestrator.go`
(`LaunchGame` phase A), `apierror`, the `R-MARKET-CONFIG` readiness check, the
`config`/`mockProviderWiring`/synthetic-guard path for `MOCK_GEOLOCATION_COUNTRY`, and the b2c
`marketRefusal` mapping.

**Not in scope:**
- `internal/operatingmarket` resolver internals. These were reviewed in Stage 4I and are unchanged here. I only
  checked how A8 calls them.
- Changes merged on the integration branch since `9b5a25f` (A3/A4/A7/A9/A10a).
- Any real geolocation vendor. None exists.
- Penetration testing.

Passing this review does not make the feature "secure". It means no merge-blocking defect was found in the
scope above.

## What was run

- **Existing A8 tests, all PASS** (integration, `-count=1 -p 1`, private DB):
  - `internal/marketgate`, `internal/geolocation`, `internal/config`, `cmd/platform-api`
  - `launchgov` / `casino` `-run Market`
  - `payments` `-run MarketGuard`
  - `httpserver` `-run Market`
- **Temporary probe file** in `internal/httpserver`, using the real `marketgate.Gate` over a recording Locator
  that wraps `StaticLocator`. Results are under each probe below.
- **Live process starts** of the built `platform-api` binary under different env combinations (probe 6).

## Probe results

### 1. Client influence on the country: none

Each of register, deposit and launch was sent with all of these at once:
- headers: `X-Forwarded-For`, `X-Real-IP`, `Forwarded`, `True-Client-IP`, `X-Client-IP`, `CF-IPCountry`,
  `CloudFront-Viewer-Country`, `X-Country`, `X-Geo-Country`, `Accept-Language: mt-MT`
- a `country=MT` cookie
- `?country=MT&cc=MT` in the query

The open market was `MT`. The server-side Locator was set to `""` (unknown) and then to `DE`.

- All 6 requests got 403 `MARKET_NOT_AVAILABLE`.
- The Locator was called 6 times and always saw `IPAddress=127.0.0.1` (from `RemoteAddr`). No header value
  reached it.
- Adding `country`/`country_code` to the JSON body gets 400 `invalid request body` at all three points, because
  `decodeJSON` rejects unknown fields. So the body cannot even carry a country.
- `Evidence` has only unexported fields and its zero value is "unknown". Outside the package, the only way to get
  "known" evidence is `Gate.Locate`.

Note for a future vendor: see SR8-8.

### 2. Every error path refuses

I checked each path in the code; the tests cover them:

| Path | Result |
|---|---|
| No Locator; Locator returns a Go error; any outcome other than resolved | Unknown, so refused |
| Country lowercase (`mt`), unassigned (`ZZ`), 3-letter, empty | Unknown (`IsISO3166Alpha2` is a strict registry lookup, no case folding), so refused |
| Locator panics | Recovered, unknown, refused |
| Typed-nil `*StaticLocator` | Nil deref panic, recovered, refused |
| Typed-nil `*Gate` held in `Deps` | `Locate` returns unknown; package-level `Check` refuses |
| Timeout | Unknown, refused. Only works if the Locator honours its context; see SR8-4 |
| `Deps.MarketGate == nil` | 503 at all three points, nothing written (pinned statically and over HTTP) |
| Error from `clock_timestamp`, the policy read, `ErrTransactionScope` (player-scoped tx) or the audit write | `Check` returns an error, the caller's tx rolls back, and the handler answers 500. It never allows. Launch-handler error mapping: a gate error cannot match `identity.ErrNotFound` or the FK-violation branch, so it falls through to 500 |
| Configuration changes mid-request | The gate runs twice (handler tx, then orchestrator phase A). A change committed between the two is caught (pinned by `TestMarketGate_HTTP_OrchestratorEvaluationRefusesWhenConfigurationFlipsMidRequest`). Residual window: under READ COMMITTED, a disable committed after phase A's first statement does not stop that phase A; phase B (the PSP or vendor call) runs after commit. Milliseconds long; accepted (SR8-9) |
| `nil` `MarketGuard` on the orchestrator params | Ungated. Assessed as SR8-3 |

### 3. Entry points without a gate

There is exactly one non-test caller each of `InitiateDepositAttempt`, `LaunchGame`, `CreateLaunchSession` (via
`LaunchGame`) and `RegisterPlayerWithResolution`, and each is the gated handler (`grep`, verified).

| Entry point | Gated? | New exposure from an excluded market? |
|---|---|---|
| `POST /v1/me/sportsbook/bets` | No | **YES, confirmed live.** With the tenant rung disabled and unknown evidence, deposit returned 403 and a sportsbook bet returned **201 accepted**. This is SR8-1 |
| Casino bet callbacks (`postBet`) on a `consumed` session | No | **YES.** A consumed session has no time limit, and nothing revokes it when the market closes. SR8-2 |
| `BootstrapLaunch` (consuming the launch token) | No | Limited: the token was minted under an allow, and it has a 2-minute TTL. Part of SR8-2 |
| Demo launch | Yes | Same gate as real launch |
| Casino play-simulation routes; `simulate-callback` | No | Test-support only (`CasinoPlaySimulationEnabled` / test-support flag). Needs a session minted by a gated launch. Info |
| Deposit credit (PSP callback) and the sweeper cascade (`driveCreatedAttempt`) | No | Not new initiation: they continue an intent that was gated when it was created. Gating credit would strand funds the PSP has already captured. Correct, and it is the ledger-finance question MKT-WIRE-CREDIT-1 |
| Withdrawal | No | Correct. A player must always be able to cash out |
| Bonus coupon redeem | No | Adds bonus funds. Casino use of them is gated (launch); sportsbook use is not (folds into SR8-1). Low |
| Catalogue listing; login | No | No exposure. Info |

### 4. Oracle and timing

- **Response bodies and headers:** across both causes (unknown, resolved-not-permitted) and all three points,
  there was 1 distinct body (`request_id` removed) and 1 distinct header set (`Date` and `X-Request-Id` removed;
  `Content-Length: 141`).
- **Registration** shows the same refusal before the email-uniqueness check, so an excluded region cannot
  enumerate emails.
- **Timing** (loopback, medians of 60, two rounds):

| Point | Unknown evidence | Not permitted |
|---|---|---|
| Deposit | 3.43 / 3.34 ms | 4.28 / 3.69 ms |
| Launch | 3.03 / 3.54 ms | 4.38 / 3.53 ms |

The difference ranges from 0 to 1.4 ms, its size is not stable, and it is within noise. Registration's Argon2 cost
hides it completely. Separating the two causes would need many samples, and it only reveals "my location was
undetermined" versus "my location was resolved and is excluded". **Accepted (SR8-6).** When a real vendor exists,
the vendor call dominates both paths.

### 5. Audit rows

Live rows for the three operations:
- `ip_address` and `user_agent` are NULL.
- `metadata` contains only `{operation, reason_code, evidence: resolved|unknown, brand_id[, market_outcome,
  policy_version]}`.
- There is no country and no IP anywhere in the row.
- `actor_type` is `player` with the player id for deposit and launch, and `system` with no actor for
  registration.

`market_outcome` (for example `not_permitted_by_licence`) is visible to tenant staff who can read `audit_log`.
This is acceptable: the oracle rule protects against players, not against the tenant's own staff, and it only
describes the tenant's own configuration and licence ceiling (SR8-7). Flooding: SR8-5.

### 6. `MOCK_GEOLOCATION_COUNTRY` (live process starts)

| Environment | Result |
|---|---|
| `APP_ENV=production` + `MOCK_GEOLOCATION_COUNTRY=MT` | Refused at config load: `MOCK_GEOLOCATION_COUNTRY must not be set when APP_ENV=production` |
| `APP_ENV` unset + `MOCK_GEOLOCATION_COUNTRY=MT` | Config load accepts it (Environment defaults to development). The synthetic guard then refuses startup and names **`geolocation/locator (*geolocation.StaticLocator)`** among the non-production-eligible components. `mockProviderWiring` would also drop the value (`GuardEnvironment()` is production) |
| `APP_ENV=staging` + `MOCK_GEOLOCATION_COUNTRY=mt` | Refused: must be uppercase ISO alpha-2 |
| `APP_ENV=staging` + `MT` (with or without `TEST_SUPPORT_ENDPOINTS_ENABLED`) | Starts. The value is honoured only when test support is on (unit-pinned by `TestMockProviderWiring_GeolocationCountryOnlyWhenMocksAreEnabled`) |

Both production layers work.

### 7. `R-MARKET-CONFIG`

- It calls the real `ResolveOperatingCountryPolicy` for registration, deposit and casino wagering. It does not
  re-implement the decision.
- Empty configuration gives `no_market_configured`, and only enabled, active tenant-rung rows count as
  candidates. A withdrawn or disabled row, or one past its `effective_to`, is not current
  (`ListCurrentOperatingCountryPolicies` filters on `effective_from <= asOf < effective_to`), so the check fails.
- Detail codes are closed strings, and no player evidence is involved.
- One difference from the gate: the readiness check uses the application clock (`s.now()`), while the gate uses
  the database clock. Clock skew could only shift readiness by the skew, and the gate enforces independently
  (SR8-10).
- Licence validity is a separate readiness item (`R-LIC-VALID`).

### 8. Tenant and brand isolation of the policy read

- `Check` runs in the caller's tenant-scoped transaction. The resolver asserts the transaction's tenant scope
  (`ErrTransactionScope`) and RLS applies.
- The tenant id comes from the authenticated context (deposit, launch) or the brand row (registration). The brand
  id comes from the player account or the brand row. Neither is ever taken from the client.
- Covered by `TestCheck_TenantIsolation` and `TestMarketGate_HTTP_UnconfiguredBrandIsRefusedAndTenantsAreIsolated`
  (both PASS).

### 9. Idempotency key after a deposit refusal

Tested live:
1. A deposit with key K was refused (`DE`) and created nothing.
2. The same K from `MT` returned 201 and created the intent. The key was not consumed by the refusal.
3. A replay of K with a different amount from `DE` returned **403**, not the original intent and not a 409. A
   replay from an excluded market learns nothing about earlier intents.

There is no double-create risk, because the refusal writes no intent row. The DB unique constraint is still the
idempotency authority.

## Findings

### SR8-1: HIGH (launch-blocking; not merge-blocking; existing gap, recorded as A8 deviation 3 / MKT-WIRE-SB-1)

**Problem:** sportsbook bet placement is not market-gated.

**Repro:** in a market world, disable the tenant rung (or set the Locator to unknown), fund the wallet, then
`POST /v1/me/sportsbook/bets`. Result: **201 accepted**, while a deposit in the same state gets 403. A bonus
coupon redeemed in an excluded market can also be wagered here.

**Impact:** a player whose evidence places them outside the operating market gets new wagering exposure.

**Fix:** add `OpSportsbookWager` with the compile-time `("wagering","sportsbook")` mapping, and call the guard
inside the placement transaction before the bet insert. It is blocked on the HDR-J-7 evidence decision, as
recorded. Until then, either disable sportsbook for any brand that relies on market blocking, or have the owner
explicitly accept the gap.

### SR8-2: MEDIUM (launch-blocking; existing gap that market blocking makes material)

**Problem:** closing a market, or a player travelling into an excluded country, does not end exposure that has
already started.

- A `consumed` casino session has no time limit (CAS-SESSION-EXPIRY-1). Bet callbacks are accepted for as long as
  it stays consumed.
- No code path revokes sessions when the tenant or brand market rung is disabled.
- A launch token minted just before a disable can still be bootstrapped within its 2-minute TTL.

**Repro (by code):** `casino/orchestrator.go`, around line 1716: only `status == consumed` is checked; no expiry,
no market check. The non-test callers of `RevokeLaunchSession` are only the bootstrap and launch-failure paths.

**Fix:**
- (a) A maximum lifetime for consumed sessions, which forces a gated re-launch.
- (b) When a tenant or brand market rung is disabled, revoke that scope's active and consumed sessions in the
  same transaction (country-level revocation is impossible without per-request evidence on server-to-server
  calls, so (a) is what bounds travel).

Needs architect and ledger-finance input on in-play rounds (settlement of bets already placed is unaffected,
because postWin and postRollback resolve from the ledger).

### SR8-3: LOW (should fix before any new caller is added)

**Problem:** a `nil` `MarketGuard` means "ungated" on `InitiateDepositParams` and `LaunchGameParams`. That is a
fail-open default inside a security control (deviation 8).

**Current state:** there is one production caller each, and both set the guard. However:
- The static pin `setsCompositeKey(..., "MarketGuard")` would also pass for `MarketGuard: nil`. The HTTP
  flip-mid-request test kills that mutant today.
- Nothing pins that no *other* non-test caller exists.

**Fix (either):**
- Fail closed on `nil` and give tests and library callers an explicit `marketgate.Ungated` sentinel guard. The
  ~100 test call sites become a mechanical edit.
- Or add a static test that enumerates every non-`_test.go` call of `InitiateDepositAttempt` and `LaunchGame` in
  the module and asserts each one sets a non-nil-literal `MarketGuard`.

### SR8-4: LOW (vendor-time)

**Problem:** the `Locate` timeout only works if the Locator cooperates. `context.WithTimeout` signals, but
`GetSignal` is called synchronously. A vendor adapter that ignores its context blocks the player request until the
adapter returns. The outcome is still a refusal (`lctx.Err()` is non-nil), but it costs availability.

**Fix:** before wiring a real vendor, run `GetSignal` in a goroutine with `select` on `lctx.Done()`, or add a
contract test that requires every Locator to honour cancellation.

### SR8-5: LOW

**Problem 1: audit flooding.** Every refusal writes one `audit_log` row, and there is no refusal-specific rate
limit.
- Registration is limited to 20/min per IP per instance (the in-memory auth limiter), so a distributed
  anonymous client scales with its IP count.
- Deposit and launch need an authenticated player and have no per-player limit.
- In every deployment that does not set a country (staging today), every request is refused, so every request
  writes a row.
- Each row is about 0.5 KB with no PII, and `audit_log` is append-only, so growth cannot be cleaned up.

**Problem 2: Argon2 cost on refused registrations.** Registration computes the Argon2id hash (~64 MiB) *before*
the gate, so a refused registration still costs a full hash. That is a CPU and memory amplification available to
an excluded-market client (it already existed for registration as a whole, and the limiter bounds it).

**Fix:**
- (a) For audit volume: dedup or aggregate refusals per (tenant, brand, operation, reason_code, minute) into a
  counter, keeping one sampled row; or add a per-player refusal limiter on deposit and launch.
- (b) For Argon2: run a gate pre-check transaction before hashing (refuse early), keeping the authoritative
  in-transaction check. The refusal is already disclosed by the response, so a faster refusal adds no oracle.

### SR8-6: INFO (accepted)

Timing difference between unknown evidence and not permitted is within noise (figures in probe 4). Re-measure
once a real vendor exists.

### SR8-7: INFO (accepted)

`market_outcome` is readable by tenant staff in `audit_log`. Not player-facing.

### SR8-8: INFO (vendor-time; LOCATOR-VENDOR-1)

`clientIP(r)` uses `RemoteAddr` only. That is correct and unspoofable today, but behind a load balancer a real
Locator would geolocate the load balancer.

**Fix:** a real Locator must receive the IP from the same `TrustedProxyCount`-aware, rightmost-untrusted-hop
extraction that `ratelimit.go` uses. Never the leftmost `X-Forwarded-For` entry, and never `CF-IPCountry`-style
headers unless the edge strips client-supplied copies. Also budget for vendor quota: `Locate` runs on every
deposit and launch request, before any authorisation beyond the bearer token.

### SR8-9: INFO (accepted)

The READ COMMITTED window inside phase A, and phase B continuing after a disable, both last milliseconds.

### SR8-10: INFO

`R-MARKET-CONFIG` uses the application clock while the gate uses the database clock. Readiness is advisory; the
gate enforces.

## Tests every domain specialist's `qa` coverage must keep (authorization and isolation)

- **Each newly gated operation** (sportsbook next) must have:
  - a refusal for unknown evidence and for resolved-not-permitted, with an identical body;
  - nothing created and one audit row with no country or IP;
  - client headers, cookies, query and body unable to move the decision;
  - tenant B refused even though tenant A has the country open.
- **A static or AST test** that every non-test caller of a guarded orchestrator entry point passes a non-nil guard
  (closes SR8-3).
- **When SR8-2 is fixed:** a consumed session past its maximum lifetime is refused on `postBet`, and a brand-rung
  disable revokes that brand's sessions in the same transaction.

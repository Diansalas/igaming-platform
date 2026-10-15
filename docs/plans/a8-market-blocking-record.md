# A8 - operating-market blocking wired into registration, deposit initiation and casino launch

Branch `a8-market-blocking` (base `9b5a25f`, "Record A3 merged"). Not pushed, not merged. This file is the
record the task asked for; the task registry, HANDOVER, progress, active-stage and the human-decision register
are deliberately NOT edited here (proposed text is at the end).

## 1. What was built

One server-side gate, `internal/marketgate`, over the EXISTING operating-market model
(`internal/operatingmarket`: licence ceiling -> tenant -> brand -> operation rungs; versioned, append-only
configuration rows maintained by staff). No migration, no table, no new route, no new policy source.

| Piece | Where |
|---|---|
| Gate: `Locate` (evidence, outside any tx), `Check`/`Guard` (policy read + refusal audit, inside the tx) | `internal/marketgate/marketgate.go` |
| Evidence seam: narrow `geolocation.Locator` interface + `StaticLocator` (Synthetic MOCK, UNKNOWN by default) | `internal/geolocation/locator.go` |
| Registration | `internal/httpserver/auth_routes.go`: gate is the first statement of the account-creation tx; refusal commits only its audit row |
| Deposit initiation | `internal/httpserver/deposit_handlers.go` (before the wallet get-or-create) AND `internal/payments/deposit_v2.go` (first statement of phase A, replay included) |
| Casino launch + launch-token mint | `internal/httpserver/casino_handlers.go` (before the wallet get-or-create) AND `internal/casino/orchestrator.go` `LaunchGame` (first statement of phase A, before any catalogue read and before `CreateLaunchSession`) |
| Refusal | `403 MARKET_NOT_AVAILABLE`, ONE body for every cause (`apierror.CodeMarketNotAvailable`); audit action `market.access_refused` |
| Readiness | `R-MARKET-CONFIG` ("operating market configured") in `internal/launchgov/readiness.go` (config check set: activate/reactivate/ratify) |
| Startup guard | `geolocation/locator` registered in `cmd/platform-api/registrations.go`; `MOCK_GEOLOCATION_COUNTRY` refused under `APP_ENV=production` |
| B2C | `b2c/src/lib/marketRefusal.ts` used by RegisterPage, DepositPage, CasinoLobbyPage |

### Decision points implemented (ADR text -> code)

* ADR 0045 section 5.3/5.4: a tenant must opt in (absence at the tenant rung is terminal); every non-`permitted`
  outcome of the eleven-valued `Outcome` is a DENY; `not_configured` is a DENY -> a brand with no market
  configuration is refused at all three points (test: `TestCheck_NoMarketConfigurationFailsClosedAtEveryPoint`).
* ADR 0045 section 6 (oracle rule): ONE status / ONE code / ONE message for every cause. Registration goes through
  `IsRegistrationPermitted`, the one sanctioned anonymous-path projection (`(bool, error)`); deposit/launch use
  `ResolveOperatingCountryPolicy` and read only `Permitted()`/`Outcome()` (never a blocking-scope accessor - there is
  none). The outcome string goes to the staff-only audit row, never to the client.
* ADR 0045 INV-M-1: `operatingmarket` still imports neither `geolocation` nor `identity`; the composition lives in the
  new package `marketgate`. `TestOperatingMarket_ImportGraphInvariant` unchanged and green.
* ADR 0045 INV-M-3: the resolver is handed `AsOf` explicitly. The gate takes it from the DATABASE clock
  (`clock_timestamp()` in the same tx), the clock that stamped every `effective_from`, so app/DB skew cannot make a
  just-committed version "not yet effective".
* ADR 0045 section 12 ("no resolver wiring authorised by this ADR"): this task is the later phase that section anticipates;
  see the open prerequisites in section 4.
* Stage-4I exit register call-site table: registration / deposit / wagering are "fail-closed: deny"; deposit/registration
  need `CountryCode (from evidence, not client-declared)`.
* `geolocation/provider.go` binding requirements: country grain only, no IP stored, no persistence; the audit row carries
  `{operation, reason_code, evidence: resolved|unknown, brand_id[, market_outcome, policy_version]}` and never a country
  or an IP (tested by key allow-list).
* Casino: demo launches are gated too (the 4I K3-4 stance: catalogue/offer availability does not depend on stake).

## 2. Evidence rule (the part the ADRs leave to a human)

The owner directive lists three possible signals: declared country at registration, the player's resolved
jurisdiction, and a `Locator` returning "unknown" by default. The ADRs constrain them:

* "Declared country at registration": the registration request has no country field, and the exit-register call-site table
  and ADR 0083 5.3.3 both say the market-policy country must come "from evidence, not client-declared" / "never from a
  client". The directive's own item (1) says "never trusting client-supplied country". Most conservative reading:
  a client-declared country is NEVER the policy input. No field was added.
* "Player's resolved jurisdiction": `jurisdiction.Resolve` yields a jurisdiction CODE (e.g. `KM-ANJ`, sub-national, a
  different code space) not an ISO country, ADR 0045 INC-6 fences `jurisdictions.country_code` as "administrative
  metadata, never a country->jurisdiction resolver", and `RequiredPurposes` returns `ErrPurposeMappingUndetermined`
  for every class (HDR-J-7). It cannot yield a country today. Not used.
* The `Locator` result is therefore the only evidence. Resolved + valid ISO-3166-1 alpha-2 -> country; ANYTHING else (no
  Locator, Go error, non-resolved outcome, lowercase/unassigned/empty code, panic, timeout) is UNKNOWN and REFUSES.

Consequence, stated plainly: with no geolocation vendor the gate refuses every request unless a development/staging
process sets `MOCK_GEOLOCATION_COUNTRY` (honoured only with mock components on, refused under `APP_ENV=production`).
Production cannot start at all on the MOCK Locator (synthetic guard) - the same state every other mock already puts it in.
That is the fail-closed outcome the ADRs prescribe while PC-GAP-1 (may an operation proceed on unusable location
evidence?) and HDR-J-7/J-8 are open. The "does this policy REQUIRE evidence" question has no configuration home today
(`jurisdiction_precedence_configs.location_requirement` is keyed by player-jurisdiction operation class, has zero rows, and
unset = fail closed by design), so evidence is required unconditionally. If the owner later decides that declared residence
may suffice for some operation, that is a one-function change in `marketgate.Check` plus a decision record - not a rewrite.

## 3. Deliberate scope decisions and deviations from the directive

1. **Deposit "credit" is not gated.** The directive says "deposit initiation/credit". Credit happens from a PSP callback/
   sweeper where (a) there is no request IP, so evidence would always be unknown and the gate would always refuse, and
   (b) refusing to credit a deposit the PSP already captured would strand player money. That is a ledger-finance decision
   (a refund/hold path), not mine. Initiation is the gate; the cascade drive (`driveCreatedAttempt`) and
   callbacks are untouched. **Open question for the owner/ledger-finance.**
2. **Bet callbacks (`postBet`), launch-token consumption (`BootstrapLaunch`), withdrawal, bonus, catalogue listing: not gated**
   (outside the three named points; vendor server-to-server calls carry no player request evidence). The launch-token MINT
   (`CreateLaunchSession`) is gated; a session can no longer be created for a refused request.
3. **Sportsbook placement: not wired, no stub.** ADR 0083 5.3.3 specifies `evaluateOperatingMarket` for sportsbook and says
   it is blocked on HDR-J-7 and that no stub ships; the directive names only three points. `marketgate` is ready for it: a
   fourth `Operation` plus the compile-time `("wagering","sportsbook")` mapping. Risk recorded: sportsbook remains more
   permissive than casino launch.
4. **Wallet side effect.** Both handlers evaluate the gate before `wallet.GetOrCreate`, so a refused request creates no wallet;
   the gate runs a second time inside the orchestrator's phase A (the enforcement point the orchestrator owns), so a
   configuration change between the two is still caught. Cost: two cheap policy reads on the allowed path.
5. **`nil` guard on the orchestrator params = ungated library path** (needed by ~100 existing test call sites). Production
   entry points cannot be ungated: the three handlers return 503 when `Deps.MarketGate` is nil, and static tests pin both the
   nil-check and that the handlers hand the guard to the orchestrator. Residual: a NEW future caller of
   `InitiateDepositAttempt`/`LaunchGame` that forgets the guard is ungated; the pin tests only cover today's handlers.
6. **No `Locator` duplicate of `LocationProvider`.** `geolocation.Locator` is the one-method subset (`GetSignal`); every
   `LocationProvider` is a `Locator`. `MockLocationProvider` errors by default (also unknown -> refuse).
7. **No ADR amendment was written for 0045.** It is "Implemented as designed" history; this record + the proposed registry rows
   carry the supersession of section 12's "no wiring" for these three points.

## 4. Prerequisites the ADRs set for resolver wiring that are STILL OPEN (flagged, not decided)

ADR 0045 section 7.1 and the exit register say resolver wiring must wait for `MKT-DUAL-1` (four-eyes on market enablement),
`HDR-M-1/M-2`, `HDR-J-6/7/8/9`, `MKT-LICSTATUS-1`, `MKT-EXPIRY-1`. The owner directive was taken as the instruction to wire;
none of those decisions was made here. Practical effect today:

* There is NO HTTP route or console to write operating-market rows (ADR 0045: zero routes; "preserve the existing manual/
  configured market model"). Production configuration is therefore a direct Go/SQL operation by authorised staff, and the
  first real country row is exactly what `MKT-DUAL-1` says needs dual control first. Nothing in A8 weakens that: the write
  APIs and their `authorization_reference` CHECKs are unchanged.
* HDR-J-6 (country content) is unanswered: no country rows exist anywhere, so a freshly migrated database refuses
  everything. This is the designed state, but every environment that exercises registration/deposit/launch end to end now
  needs (1) `MOCK_GEOLOCATION_COUNTRY` and (2) a licence ceiling + tenant opt-in row for the brand. Known affected: the
  staging stack (`deploy/aws/environments/staging` sets `TEST_SUPPORT_ENDPOINTS_ENABLED` but not the new variable - NOT
  changed here, deploy is out of A8's scope), `deploy/loadtest/smoke`, and any manual local run.

## 5. Tests (all new unless listed under "changed")

* `internal/marketgate`: locate matrix (valid/nil/error/each non-resolved outcome/lowercase/unassigned/3-letter/empty/panic/
  timeout/default static/mock unconfigured); integration: allowed passes at all three operations; unconfigured brand refused;
  disabled tenant/not-opted-in/no-ceiling country refused; operation- and brand-rung narrowing; unknown evidence refused
  even when the market is open; configuration change effective on the next request and versioned (3 history rows);
  tenant isolation (B refused despite A open; B's tx checking A's request -> `ErrTransactionScope`); player-scoped tx
  fails closed; invalid requests are errors; audit carries ids/codes only (key allow-list, no country); registration audit
  has no player; refusal audit commits with the surrounding tx; concurrent config change vs requests (race, `-race`);
  16 concurrent refusals each audited once.
* `internal/httpserver` (real gate, real rows): allowed at all three points; three causes x three points = one identical
  body, no player/intent/session/wallet created, one audit row per refusal; config change immediate (disable/re-enable);
  unconfigured brand + tenant isolation; client-supplied body/headers cannot move the country; nil gate -> 503 x3 and no
  rows; static pins (nil-check, guard handed to orchestrator, denial mapping, 403 status); OpenAPI contract pins.
* `internal/payments`: guard first in phase A, refusal creates nothing / no provider call / key not consumed / replay
  refused / error fails closed / nil = library path.
* `internal/casino`: refusal is a Denied result with the generic code and no session; refusal precedes catalogue checks
  (unknown game id gets the same refusal); error fails closed; allow mints exactly one session after the guard saw none.
* `internal/launchgov`: `R-MARKET-CONFIG` fail/no config, pass, orphaned opt-in (ceiling disabled), operation-rung narrowing,
  second country satisfies, tenant isolation, disabled tenant row, present only in the config check set.
* `internal/config`, `cmd/platform-api`: `MOCK_GEOLOCATION_COUNTRY` default/accepted/refused in production/malformed;
  Locator registered and refused in production by name; mock wiring matrix; main wires `marketgate.New` into `Deps`.
* `b2c`: helper tests + one test per screen (register, deposit, launch) that the UI-owned sentence is shown and nothing
  else happens.

### Changed existing assertions

None weakened or deleted. Harness-only edits to existing test files:
* ~39 `internal/httpserver/*_test.go` files: every `Deps{... PersonResolver: identityresolution.NewMockPersonResolver() ...}`
  gained `MarketGate: allowAllMarketGate{}` (sed; gofmt realigned) because a nil gate now 503s the three routes.
  `allowAllMarketGate` is a `_test.go`-only type (precedent: `AllowAllDepositKYCGate`).
* `internal/config/config_test.go`: `MOCK_GEOLOCATION_COUNTRY` added to `clearEnv`'s list.

## 6. Labels (CLAUDE.md)

| Deliverable | Label |
|---|---|
| Gate, wiring at the three points, refusal contract, audit, readiness item, B2C mapping, startup guard | IMPLEMENTED |
| Geolocation evidence (real country from a network/vendor) | MOCK (StaticLocator) / PROVIDER DEPENDENT (vendor) |
| Production operability of the three flows | BLOCKED on a production-eligible Locator (or an owner decision on other evidence) AND on market configuration rows |
| Deposit credit gating | NOT IMPLEMENTED (deliberate, section 3.1) |
| Sportsbook placement / bet callbacks / withdrawal / bonus / catalogue | NOT IMPLEMENTED (out of scope, section 3) |
| Staff UI/route to configure operating markets | NOT IMPLEMENTED (ADR 0045 zero-route posture; MKT-DUAL-1) |

## 7. Migrations, routes

No migration (0160 and every number stay unused). No route added; `POST /v1/auth/register` gained a 403, and
`POST /v1/me/deposits` and `POST /v1/me/casino/games/{gameID}/launch` (previously undocumented routes) are now documented in
`docs/api/openapi/platform-api.yaml` with the 403.

## 7a. Verification summary

All on a private database, runtime role, `-tags integration -count=1 -p 1` unless stated:
`go build ./...`, `gofmt -l` clean, `go vet ./...` and `go vet -tags integration ./...` clean, `golangci-lint run ./...`
(v2.9.0) 0 issues, `go test ./...` (unit) all pass; integration PASS for operatingmarket, geolocation, marketgate, jurisdiction,
identity, tenant, config, `./cmd/...`, apierror, launchgov (whole package), casino (whole package), kyc, db, payments (the
207 tests in files that call `InitiateDepositAttempt`, ~36 min) and 150 httpserver tests (register/deposit/launch/launch-
authorisation/casino/stage acceptance + the new market files); `-race` PASS for operatingmarket + identity + marketgate;
b2c `vitest` (97 tests) + `npm run build` PASS; backoffice `tsc -b` + launch vitest PASS (one 5 s timeout of
`Wording.test.tsx` under CPU load while Go suites ran, green alone). The WHOLE httpserver and WHOLE payments packages were
not run (explicitly not required; hosted CI shards them).

Mutation pass on the gate and its call sites (24 distinct mutants; `a8_mut.sh`-style one-at-a-time, file always restored):
23 killed, 1 survived. Survivor: removing `result.AssertScope(...)` in `marketgate.Check` - an equivalent mutant (the result's
tenant/brand are the query's by construction and the transaction's tenant scope is already asserted inside the resolver);
kept as defence in depth. One mutant ("the orchestrator-level result is ignored by the deposit handler") initially survived
because the handler's own earlier evaluation refuses first; killed by adding
`TestMarketGate_HTTP_OrchestratorEvaluationRefusesWhenConfigurationFlipsMidRequest` (a gate that allows the first evaluation
and refuses the second, for deposit and launch). Two first-draft mutants that failed to compile were redone.

Environment note: during the payments run the shared Postgres server went into crash recovery (SQLSTATE 57P03) for about
three minutes and the payments run was repeated in full once it was back; I did not touch roles, passwords or the server. A
`-race` run also failed once with `encoding/xml is not in std` from the Go toolchain cache and passed on retry. Neither
relates to this change.

## 8. Proposed registry text (for the orchestrator to place; nothing was edited)

* **A8 / MKT-WIRE-1 (new)** - DONE (IMPLEMENTED) on `a8-market-blocking`: operating-market gate wired into registration, deposit
  initiation and casino launch (+ launch-token mint); `R-MARKET-CONFIG` readiness; B2C mapping. Residual rows below.
* **MKT-WIRE-CREDIT-1 (new, ledger-finance + architect)** - deposit credit / callback / sweeper cascade are not market-gated;
  decide whether a captured deposit from an excluded market is credited, held or refunded. Until then initiation is the control.
* **MKT-WIRE-SB-1 (new; = SB-JUR-RUNG2-1)** - sportsbook placement not market-gated; `marketgate` is ready; blocked on HDR-J-7
  evidence decision. Sportsbook is more permissive than casino launch until done.
* **MKT-WIRE-OTHER-1 (new)** - bet callback (`wagering` per bet), `BootstrapLaunch`, withdrawal, bonus, catalogue listing not gated.
* **MKT-EVIDENCE-1 (new, architect + identity-compliance + legal)** - which evidence may satisfy the gate (declared residence
  for deposit/launch? verified KYC residence? vendor geolocation?) and whether evidence is required per operation. Today:
  Locator-only, unconditional, unknown refuses. Subsumes PC-GAP-1 for these three operations.
* **MKT-OPS-1 (new)** - no staff route/console to configure operating markets; production rows are a manual Go/SQL operation
  and the first real row needs MKT-DUAL-1 (four-eyes) first.
* **MKT-ENV-1 (new, devops)** - staging terraform needs `MOCK_GEOLOCATION_COUNTRY` + market rows or its registration/deposit/launch
  flows are refused; `deploy/loadtest/smoke` likewise.
* **LOCATOR-VENDOR-1 (provider dependent)** - production-eligible `geolocation.Locator`; trusted-proxy-correct client IP
  (`clientIP(r)` is `RemoteAddr` only today); vendor selection + security review; the wiring is the only change needed.
* Close/annotate: ADR 0045 section 12 "no resolver wiring" - superseded for registration/deposit-initiation/launch by A8;
  HDR-M-1/J-6/J-7/J-8 and MKT-DUAL-1 remain OPEN and gate the first real country row and any further wiring.

## 9. What `security` should probe

1. Client influence on the country: body fields, `X-Forwarded-For`, `CF-IPCountry`, `Accept-Language`, query - tested for
   registration only; probe deposit/launch too, and `clientIP`'s trust of `RemoteAddr` behind a proxy once a real Locator exists.
2. Fail-closed on every error path: a `Check` error must never allow. Probe: DB failure mid-gate, `clock_timestamp` failure,
   audit write failure (returns error -> request fails), `ErrTransactionScope` from a player-scoped tx.
3. Ungated entry points: every caller of `InitiateDepositAttempt`, `LaunchGame`, `CreateLaunchSession`, `RegisterPlayer*`
   (grep shows one production caller each; confirm), the sweeper cascade, `payment_deposit_simulation` (test support), the
   casino play-simulation routes (mock play after launch - they need an existing session, which can no longer be minted).
4. Oracle: byte-identical bodies (request_id aside) across causes - also compare status, headers, TIMING (the unknown-evidence
   path skips the policy read and the DB-clock read, so it is measurably faster; the audit row is written on both).
5. Refusal-audit flood: an excluded-market client can generate one `audit_log` row per request with no rate limit specific to
   it (registration has the auth limiter; deposit/launch rely on authenticated rate limits). Decide whether refusals need
   sampling or a counter instead of a row.
6. Audit content: ids and closed codes only; confirm `market_outcome` (staff-only) is acceptable in `audit_log` readable by tenant
   staff (it names `disabled_by_brand` etc.), since the oracle rule protects players, not tenant staff.
7. Registration-time enumeration: the gate runs before the email-uniqueness check, so an excluded region cannot probe emails;
   an allowed region still can (pre-existing).
8. Config-write path unchanged (RLS own-tenant-only, append-only, `authorization_reference`); confirm no new read path to
   `operating_country_policies` was opened: `ListCurrentOperatingCountryPolicies` is now called from the readiness evaluator
   (tenant-scoped read-only tx) - confirm that the readiness response exposes only the closed detail code.
9. `MOCK_GEOLOCATION_COUNTRY`: production refusal at config load and at the synthetic guard (two layers), and that a staging
   operator cannot be tricked into a permissive static country by the missing-`APP_ENV` path (`GuardEnvironment` is
   production there; wiring ignores the value).
10. Concurrency: config change vs in-flight request (tested at the gate); a long transaction that read the policy before a
    disable commits is allowed by READ COMMITTED - the window is the transaction itself (for deposit: phase A).

## 10. Round 2 (security review `docs/plans/prh2-hardening-round/a8-security-review.md`, SR8-1..10)

Merged the integration tip first (A3, A4, A7, A9, A10a, A10b). Conflicts resolved keeping both sides:
`cmd/platform-api/registrations.go` (Geolocation AND AMLScreening bundle fields/registrations), `docs/api/openapi/platform-api.yaml`
(LaunchCheck code list now names R-MARKET-CONFIG next to the A7 R-AML-* codes; `MarketNotAvailable` response next to the
`Aml*` responses), `internal/launchgov/readiness.go` (both `marketConfigCheck` and `amlChecks` appended). **`SnapshotVersion`
2 -> 3** (deliberate: the merged check set differs from A7's v2; `readiness_aml_test.go`'s pin changed from 2 to 3).

| Finding | Done |
|---|---|
| SR8-1 sportsbook placement | `marketgate.OpSportsbookWager` = ("wagering","sportsbook"). `POST /v1/me/sportsbook/bets`: handler evaluates the gate inside the placement tx before the wallet get-or-create; `sportsbook.PlaceBet` evaluates `PlaceBetParams.MarketGuard` as its first statement after validation (before the idempotency lookup: replays refused). Refusal = `PlaceBetResult.MarketDenied` -> `403 MARKET_NOT_AVAILABLE`, nothing locked or written. Same contract, audit, OpenAPI (403 added, route text), b2c mapping (`BetSlip`). Nil gate -> 503. **ADR 0083 5.3.3 deviation recorded as an in-ADR amendment** (UNKNOWN evidence refuses; HDR-J-7 stays open). |
| SR8-2 (a) | `CASINO_SESSION_MAX_LIFETIME` (default 4h, bounded 1m..72h, fails startup otherwise) -> `casino.Orchestrator.SetSessionMaxLifetime`. `postBet` refuses a NEW bet on a consumed session older than that (DB clock, `created_at`), `ErrLaunchSessionRequired`, nothing written. |
| SR8-2 (b) | `marketgate.BrandOpen`/`AnyCountryPermits` (the R-MARKET-CONFIG resolver logic, now shared; DB clock; no player evidence). `postBet` DECLINES a NEW bet (`market_unavailable`, audit `casino_bet.denied_market_closed`, no ledger write) when no country permits casino wagering for the brand; an error to decide fails closed. Sportsbook needs no separate brand-level check: its per-request gate is strictly stronger (a closed market has no country that passes). |
| SR8-2 (c) | `BootstrapLaunch` step 4 gains `market_closed` (revoke + audit + commit, the existing definitive-denial path; HTTP stays the constant 403). |
| SR8-2 settlement | `postWin`/`postRollback`/replay/tombstone never reach either check (tested: two open bets, market closes, win posts and rollback posts, new bet declined, ledger balanced; win after lifetime posts). |
| SR8-2 concurrency | `TestPostBet_DisableRacingBets...`: 12 bets race a disable; every outcome is accepted-and-posted or declined-and-unposted, ledger balanced, balance equals accepted bets, next bet declined. |
| SR8-3 | Empty `MarketGuard` refuses (`marketgate.ErrGuardRequired`) in `LaunchGame`, `InitiateDepositAttempt`, `PlaceBet`. Tests pass the explicit named `marketgate.UngatedForTests` (per-package `testUngatedGuard`); casino orchestrator tests use `Orchestrator.WithUngatedMarketOpenForTests()` for the new brand-level check (default and production = ENFORCED, nothing to wire). Code scans (`internal/marketgate/callers_scan_test.go`): every production call of the three entry points passes a non-nil `MarketGuard` literal; no production file outside marketgate/casino-definition references either ungated sentinel. |
| SR8-4 | `Gate.Locate` runs the lookup in its own goroutine with a hard deadline; a late answer is discarded. Abandoned lookups are bounded by a 128-slot semaphore (a request that cannot get a slot is UNKNOWN at once), so a hung vendor can park at most 128 goroutines. Test with a Locator that ignores cancellation. |
| SR8-5 | (a) Registration runs the gate as a pre-check BEFORE the Argon2id hash (AST test pins the order); the authoritative in-transaction check stays. (b) Audit flood: refusal audit rows are sampled per (tenant, brand, operation, reason, player) token bucket - burst 5, then 1/minute - and the next written row carries `suppressed_since_last_row`. Registration has no player, so all anonymous refusals of a brand share one bucket; no IP or hash is stored. The refusal itself is never sampled. Process-local (N instances = N x the cap), map bounded at 10k keys. Chosen as the simplest sound option over a DB counter table (needs a migration) or per-IP keys (needs an identifier we deliberately never store). |
| SR8-6, 8 | Recorded only. A real Locator must receive the `TrustedProxyCount`-aware rightmost-untrusted-hop IP (`ratelimit.go`'s extraction), never the leftmost `X-Forwarded-For` nor a client-suppliable country header; budget vendor quota (`Locate` runs on every gated request). Timing difference unknown-vs-not-permitted is within noise; re-measure with a vendor. |
| SR8-7, 9, 10 | Recorded only. `market_outcome` is staff-readable in `audit_log` (accepted); the READ COMMITTED window inside phase A and phase B continuing after a disable last milliseconds (accepted); `R-MARKET-CONFIG` uses the application clock, the gate and `BrandOpen` the database clock (readiness advisory, gate enforces). |

Residual (PROVIDER DEPENDENT): a player who moves from an allowed country to an excluded one INSIDE a market that stays open is
not detected after launch; the maximum session lifetime is the only bound (a casino bet is a vendor callback with no player
request). Real detection needs a per-bet vendor geolocation signal.

### Round-2 changed assertions

* `internal/launchgov/readiness_aml_test.go`: `SnapshotVersion != 2` -> `!= 3` (check set grew by `R-MARKET-CONFIG`).
* `TestInitiateDepositAttempt_NilMarketGuardIsTheUngatedLibraryPath` replaced by `..._EmptyMarketGuardRefuses` + `..._NamedUngatedSentinelProceeds`;
  `TestLaunchGame_NilMarketGuardIsTheUngatedLibraryPath` replaced by `..._EmptyMarketGuardRefuses` + `..._NamedUngatedSentinelProceeds`
  (my own round-1 tests: nil used to mean ungated, now it refuses).
* Mechanical harness edits (no assertion touched): `MarketGuard: testUngatedGuard` added to every `LaunchGameParams{}`,
  `InitiateDepositParams{}` and `PlaceBetParams{}` literal in existing tests (casino, payments, httpserver, sportsbook,
  reconciliation, risk), and `.WithUngatedMarketOpenForTests()` appended to every `casino.NewOrchestrator(...)` in existing tests
  (these tests are about bet mechanics, not market configuration).

### Round-2 verification

gofmt clean; `go vet ./...` and `go vet -tags integration ./...` clean; golangci-lint 0 issues; `go test ./...` PASS. Integration (private DB,
`-tags integration -count=1 -p 1`): marketgate, operatingmarket, geolocation, config, `./cmd/...`, apierror, launchgov, sportsbook, casino,
reconciliation, risk, kyc PASS (whole packages); httpserver (the register/deposit/launch/sportsbook/market/launch-authorisation/casino/
stage-acceptance set, 300+ tests, plus all new market files) PASS after one load flake of `TestResolutionIsolation_NormalOperation`
(failed while b2c/backoffice vitest ran concurrently on the same CPUs, passes alone, whole `TestResolutionIsolation*` group re-run green);
payments: the 207 tests in files that call `InitiateDepositAttempt` PASS; `-race` PASS for marketgate, operatingmarket, identity.
b2c vitest (98 tests) + build PASS; backoffice `tsc -b` + vitest PASS (a different single test timed out once under the same
load, green alone). Mutation pass round 2: 14 mutants, 14 killed (two build-failure "kills" redone).

## 11. Round 3 (security re-review `a8-security-rereview.md`: SR8R-1..7)

Merged the tip again first (A7r3, caller-scan skip, payments pin): clean, no conflicts.

| Item | Done |
|---|---|
| SR8R-1 | `AnyCountryPermits` no longer resolves every configured country per call: (1) ONE set-based query drops candidates with no in-force, active, enabled licence ceiling (the resolver denies those at its step 1, so the verdict cannot change; on a query error the full list is kept and the resolver decides), (2) the country that last permitted the same (tenant, brand, operation set) is tried first (advisory, process-local, <= 20k entries; a stale hint costs one extra resolve and never changes a verdict). Fail-closed semantics unchanged (error = closed). Test with 100 configured countries, worst case (only the LAST sorts open, 99 closed at the brand rung): cold 159 ms, warm 7 ms measured; DOCUMENTED BOUNDS asserted: cold < 5 s, warm < 500 ms; equivalence with a brute-force resolver pass (also after closing the hinted country: stale hint safe) and with `R-MARKET-CONFIG` (it calls the same function; launchgov suite green). |
| SR8R-2 | Sampler rewritten: per-TENANT tables (cap 500) under a global cap (10k); eviction drops idle buckets first (full tokens, nothing suppressed), then the least recently used of the SAME tenant (global pressure: of the tenant holding most buckets) - never a quiet tenant's. A bucket evicted while holding suppressed refusals adds its tally to the tenant's pending `suppressed_evicted` count, carried by the next audit row written for that tenant (reported once), increments the `marketgate.EvictedSuppressed` counter and logs one cumulative Warn per tenant per minute. Every refusal is ALSO logged by the calling handler (structured Warn), so sampling never hides a refusal from logs. Tests: 12k-key cycling stays within both caps; another tenant's pending count survives two 12k floods; evicted tallies are counted and reported exactly once; idle-first eviction. |
| SR8R-3 | `TestMarketGate_HTTP_RegistrationInTransactionCheckRefusesWhenConfigurationFlipsAfterThePreCheck` (allow on the pre-check, refuse in the transaction: 403, no account, no wallet); the static test now also requires a `marketGuard` call before `RegisterPlayerWithResolution` inside the SAME transaction function literal. |
| SR8R-7 | Verified: a casino bet refused for session lifetime used to return `ErrLaunchSessionRequired` (rollback; only an HTTP log line, no durable row; `rejections.go` states session errors are not financial rejections). Now a DECLINE (`session_expired`) with audit row `casino_bet.denied_session_lifetime` (ids and closed code `SESSION_MAX_LIFETIME`), committed with the decline; no ledger row. Changed assertion: `TestPostBet_ConsumedSessionPastMaxLifetimeRefusesNewBetButSettles` expected an error, now expects the decline plus the audit row. |
| SR8R-4, 5 | Recorded only. SR8R-5 needs a PRODUCT ACKNOWLEDGEMENT: a replay of an idempotency key for an EXISTING bet from refused evidence answers 403 (the gate precedes the idempotency lookup by design), so a client retrying a timed-out request after the player moved out of the market sees a refusal, not the original bet; the bet itself stands and is visible in bet history. |
| SR8R-6 | Recorded; cheap metric added: `marketgate.LocateSlotsExhausted` counts lookups refused because the 128 abandoned-lookup slots were all held. Size the cap against vendor latency x gated request rate when a vendor exists. |
| Note | `R-MARKET-CONFIG` evaluates registration, deposit and casino wagering only; it does not evaluate sportsbook wagering (a brand that only offers sportsbook is judged on the casino operation). Recorded, not changed. |

# Market evidence from KYC-verified residence (owner decision 3, MKT-EVIDENCE-1)

Branch `market-evidence-kyc-residence` (base `12edec0`). Not pushed, not merged. Task registry, HANDOVER, progress, active-stage, the
decision register and ADRs are deliberately NOT edited. Builds on `a8-market-blocking-record.md` (read it first); this record supersedes its
section 2 ("evidence rule") and its MKT-EVIDENCE-1 row for the four gated operations.

## 1. Owner decision implemented

KYC-verified residence is the CURRENT platform market-evidence source. Vendor geolocation can be added later; **no geolocation provider was
invented**. Registration, deposit, launch (and sportsbook placement, which already called the gate) stay **fail-closed** when the required
market evidence is unavailable. The manual / configured operating-market model is unchanged (no importer for market rows was built).

## 2. Design

### 2.1 Evidence source for deposit, launch, sportsbook placement

| Piece | Where |
|---|---|
| Read: `kyc.ReadMarketResidenceEvidence(ctx, q, tenantID, playerAccountID) (country, ok, err)` | `internal/kyc/market_evidence.go` (no migration, one SQL statement) |
| Locator: `marketgate.KYCResidenceLocator` (implements `geolocation.Locator`, `providerkind.ProductionEligible`, NOT Synthetic) | `internal/marketgate/kycresidence.go` |
| Consumer: unchanged `Gate.Locate` / `Check` (2 s deadline, 128-slot abandoned-lookup bound, ISO-3166-1 validation, panic -> unknown) | `internal/marketgate/marketgate.go` |
| Wiring: default bundle Locator is the KYC source; the Synthetic static MOCK only when an explicit dev/staging `MOCK_GEOLOCATION_COUNTRY` is set; the pool is bound right after `db.Connect` | `cmd/platform-api/registrations.go`, `main.go` |

A country is evidence only when **all** hold (one statement, database clock, same tenant):

1. the player account's **latest decided** verification (the row KYC enforcement itself uses: never-decided orphan intent rows excluded,
   `created_at DESC, id DESC`) is `approved` and not expired (`expires_at` NULL or in the future);
2. an `approved`, unexpired verification of the same account carries a reviewer-recorded `verified_residence_country`; the most recent
   determination (`verified_residence_set_at`) wins.

Everything else is **no evidence -> refused with the existing closed code**: no record, `unverified`, `pending`, `review_required`, `rejected`,
`expired`, approved-but-expired, approved without a determination, a determination only on a non-approved row, a newer decided non-approved
row (a re-verification in progress withdraws the evidence on the next request, exactly as it withdraws the deposit clearance), an unbound
locator, a missing player account, a database error, a timeout, a cancelled context. A resolved country in a closed / unlicensed / not
opted-in market is refused by the policy as before (`MKT_NOT_PERMITTED`); no evidence is `MKT_EVIDENCE_UNAVAILABLE`. Player-facing: one
`403 MARKET_NOT_AVAILABLE` body for every cause (unchanged; no leak). The audit row still carries ids and closed codes only, never a country.

**Tenant scoping and never from the client.** `tenantID` and `playerAccountID` come from the authenticated token in the handlers. The read runs
in `Pool.WithTenantReadOnly` (the existing tenant session; RLS on `kyc_verifications`), asserts `app.tenant_id == tenantID` (mismatch is an
error, never another tenant's row) and repeats `tenant_id` in the predicate. The request IP is ignored and never read. Headers / body cannot
supply evidence (tested through HTTP).

**No migration.** `kyc_verifications` already has the residence columns, RLS and the `(player_account_id)` index (migration 0074 / 0040).
Numbers 0165+ are untouched (T8 can keep 0165).

### 2.2 Registration (product decision PD-MKT-REG-1, the owner can reverse it)

Registration happens before any KYC can exist, so there is **no player evidence** to require, and a client-declared country is not evidence
(the request has no country field and ADR 0083 5.3.3 / the exit register forbid one). Options considered:

| Option | Verdict |
|---|---|
| A. Refuse registration entirely until a pre-registration evidence source exists | Safest, but breaks the B2C flow completely (no player could ever reach KYC). Rejected as the default; reachable by configuration (below). |
| B. Require a client-declared country at registration, re-checked against KYC at first deposit/launch | Adds a client-controlled input and an API/UI change; a liar picks an allowed country, so it buys no security, only courtesy filtering. The re-check against KYC is already provided by the deposit/launch gates. Not built. |
| **C. Brand-level rule (chosen)** | Registration is permitted only while the brand's operating-market configuration permits the `registration` operation for **at least one country** (licence ceiling -> tenant -> brand -> operation rungs; same resolver, `AnyCountryPermits`). No country, no IP, no client value is used. An unconfigured, closed, narrowed-to-nothing or undecidable brand (DB error) refuses. |

Why C is safe: money and games never rely on registration. The registered account has no verified residence, so **deposit, launch and
sportsbook placement refuse it until KYC verifies a residence in an open market** (tested: a freshly registered account is refused at
deposit and launch). A wrong or hostile registration therefore cannot reach money or games. Residual exposure: a person located in an
excluded market can still create an unverified account (an email and a password hash are stored) because registration cannot know where
the visitor is. The stricter reversal is a configuration change (disable the `registration` operation for every country of the brand at the
operation rung: a disable needs no authorisation reference) or, if the owner wants a declared-country check at sign-up, a one-function change
in `marketgate.Check` plus an API field and a decision record (option B). **OWNER decision to confirm or reverse.**

The registration handler no longer calls `Locate` at all (no wasted lookup, no IP use); it passes the zero `Evidence{}` and `Check`
ignores evidence for the registration operation.

### 2.3 Production behaviour

* `KYCResidenceLocator` implements `MarkProductionEligible` and not `SyntheticComponent`, so the existing `providerkind.RefuseSyntheticInProduction`
  **passes** for `geolocation/locator` when the KYC source is wired and **refuses** the static MOCK Locator by name (unchanged guard,
  new assertions).
* The guard silently skips a nil component, so a new pure startup check `requireMarketEvidenceInProduction` (run from `main` right after the
  guard) refuses to start in production unless exactly one `geolocation/locator` registration carries a component (none, nil or two refuse).
  `config.Load` still hard-fails `MOCK_GEOLOCATION_COUNTRY` under `APP_ENV=production` and `mockProviderWiring` still ignores it there.
* The pool is bound after `db.Connect` (`bindMarketEvidence`; a nil pool fails startup); an unbound locator reports no evidence.

## 3. Labels (CLAUDE.md)

| Deliverable | Label |
|---|---|
| KYC-verified-residence evidence source, wiring, production guard acceptance, startup requirement | IMPLEMENTED |
| Brand-level registration rule (PD-MKT-REG-1) | IMPLEMENTED (a product decision awaiting owner confirmation) |
| Fail-closed behaviour at registration, deposit, launch, placement | IMPLEMENTED |
| Production operability of deposit / launch / placement | **PROVIDER DEPENDENT / BLOCKED**: verification rows are created through the KYC orchestrator, which is deliberately absent in production without a KYC vendor (`kycOrchestrator`); until a real KYC provider (or another approved onboarding path) exists, no production player has residence evidence, so these refuse (fail closed). Registration works once market rows exist. |
| Physical-location evidence (VPN, travel, residence != location) | NOT IMPLEMENTED (PROVIDER DEPENDENT: vendor geolocation, not invented) |
| Importer / staff UI for market rows | NOT IMPLEMENTED (unchanged; ADR 0045 zero routes, MKT-DUAL-1) |
| `StaticLocator` / `MOCK_GEOLOCATION_COUNTRY` | MOCK (non-production only; now optional) |

## 4. Tests

New:
* `internal/marketgate/kycresidence_integration_test.go`: verified residence in an allowed market passes (deposit, launch, sportsbook);
  closed / not-opted-in / unlicensed markets refused with no country in the audit; 16 no-evidence cases (no record, unverified, pending,
  review_required, rejected, expired status, approved-but-expired, approved without determination, residence only on a rejected / pending row,
  newer pending / rejected / expired / expired-approved supersession, expired residence row) each also asserted at the locator
  (inconclusive, no country); selection rules (orphan intent ignored, newest determination wins, fresh approval carries the last
  determination); tenant isolation (tenant B never reads A's player; scope-mismatch is an error); database error and closed pool and cancelled
  context fail closed; registration (open brand with zero / unrelated evidence allowed, unconfigured, brand-disabled, operation-disabled
  refused, one open country among closed suffices, change effective next request, other tenant's market does not open it, aborted
  transaction is an error never an allow); concurrency (40 concurrent gated requests allowed, then 40 refused after a rejection commits);
  latency (200 sequential lookups < 10 s, 64-way burst within the 2 s lookup deadline).
* `internal/marketgate/kycresidence_test.go` (unit): production-eligible and not synthetic, guard accepts it and refuses the static MOCK,
  unbound / nil / no-player / no-tenant reports no evidence and never calls the reader, nil pool bind fails, reader error is an error and unknown.
* `internal/httpserver/market_gate_kyc_residence_integration_test.go` (REAL gate over the REAL KYC locator, no static country): no KYC refuses
  deposit, launch and bet with one identical body and creates nothing; verified MT allows all three; a newer pending re-verification refuses
  all three on the next request; verified DE refused; spoofed country headers cannot refuse a verified player nor admit an unverified one;
  registration brand-level (open 201, closed 403); a fresh registration cannot reach money or games; another tenant's verified residence is
  never read.
* `cmd/platform-api/market_gate_wiring_test.go`: default bundle Locator is the KYC source, explicit country selects the static MOCK,
  production wiring can never select the static one; the production guard pair passes with the KYC source and refuses none / nil / two /
  synthetic / wrong-name; staging and development do not require a Locator; `bindMarketEvidence` binds only the KYC source and rejects a nil
  pool; `main.go` calls it.

Existing tests that still pass unchanged and are part of the proof: `internal/marketgate` latency / scale (`open_scale_integration_test.go`:
cold < 5 s, warm < 500 ms bounds untouched), `callers_scan_test.go` (static scan: every production caller passes a non-nil `MarketGuard`; no
production use of the ungated sentinels), `TestRegisterHandler_MarketPreCheckPrecedesThePasswordHash`, the in-transaction registration flip
test, the second-evaluation tests, the sportsbook gate tests, the OpenAPI market contract tests.

### Changed existing assertions (none weakened; each is a deliberate semantic change)

1. `internal/marketgate/marketgate_integration_test.go` `TestCheck_UnknownEvidenceFailsClosedEvenWhenMarketIsOpen`: the operation list
   `allOps` (registration, deposit, launch) became deposit, launch, sportsbook. **Registration no longer requires evidence** (PD-MKT-REG-1), so
   "unknown evidence refuses registration" is no longer true by design. Sportsbook was added (it was untested there). The registration
   behaviour is asserted instead by `TestCheck_RegistrationIsBrandLevelAndNeedsNoEvidence`.
2. `internal/httpserver/market_gate_http_cases_integration_test.go` `TestMarketGate_HTTP_EveryCauseIsOneIdenticalRefusalAndCreatesNothing`:
   registration is exercised only for the configuration cause (country outside the market / unknown evidence now apply to deposit and launch
   only). The identical-body, no-side-effect and audit-count assertions are unchanged for every point that remains.
3. Same file, `TestMarketGate_HTTP_ClientSuppliedCountryIsIgnored`: it previously sent extra body fields (`country`, `country_code`) and asserted
   "not 201"; the strict request decoder rejects unknown fields with a 400, so that passed for the wrong reason. It now closes the brand's market, sends
   only spoofed headers, and asserts exactly `403`. (Stricter, not weaker.)
4. `internal/httpserver/market_gate_audit_flood_integration_test.go` (two tests): the registration refusal flood is now produced by disabling the
   brand's market (`setTenant(Disabled)`) instead of `locator.SetCountry("DE")`; the bounded-audit-rows and no-starvation assertions are unchanged.
5. `cmd/platform-api/market_gate_wiring_test.go`: `TestBuildRegistrations_RegistersTheGeolocationLocator` asserted the registered Locator is always
   Synthetic; it is now table-driven (default = KYC source, not Synthetic; explicit country = Synthetic static). `TestRefuseSyntheticInProduction_RefusesTheMockLocatorByName`
   keeps its assertions but builds the bundle with an explicit mock country (the default bundle is no longer the MOCK).

Doc/text-only changes: `docs/runbooks/market-blocking-configuration.md`, `aws-staging-deploy-plan.md`, `production-configuration-checklist.md`,
and the description strings of `mock_geolocation_country` in `deploy/aws/modules/ecs/variables.tf` and `deploy/aws/environments/staging/variables.tf`
(no Terraform logic or default changed; the variable was already optional and unset by default, so staging still starts).

## 5. Mutation pass (new evidence logic)

19 mutants applied one at a time, file restored each time (driver in the session scratchpad): the SQL (latest-status check, latest expiry,
residence-row expiry, residence-row status, orphan exclusion, both orderings, tenant-scope assertion, tenant predicate), the locator
(not-found, nil player), the registration rule (always allow, wrong operation, needs evidence), the wiring (`n != 1`, disabled production
check, inverted selector, no-op bind, marker swapped to Synthetic). Result: **18 killed, 1 survived and is justified**: removing the
`tenant_id = $1` predicate from the residence sub-select is an equivalent mutant because RLS on `kyc_verifications` still filters by the
transaction's tenant; the predicate and the `app.tenant_id` assertion (whose removal IS killed) are defence in depth. Three first-draft results exposed
real test gaps or non-compiling mutants and were redone: M4 (residence sub-select without the `approved` check) survived until a test with a
rejected / pending row carrying a residence beneath a newer approval was added; M10 (locator ignores not-found) survived until the locator itself
was asserted inconclusive for every no-evidence case (the gate's ISO check had masked it); M10 and M18 first failed to compile and were
rewritten as compiling mutants.

## 6. Verification

See section 9.

## 7. Residuals and honest limits

1. **Residence is not location.** A verified resident of an open country can place bets from anywhere; a VPN or travel is not detected. A
   vendor geolocation Locator can be wired later behind the same interface (`PROVIDER DEPENDENT`, not built, not invented).
2. **Production KYC operability** (see section 3): no KYC vendor, so no production verification rows, so deposit / launch / placement refuse. The
   gate is fail-closed, not broken.
3. **Residence is reviewer-entered** (HDR-J-3c/3h: never derived from a document). It can be stale: an approved re-verification without a new
   determination carries the previous determination forward (the rule is deliberate so routine re-verification does not lock players out). A
   relocated player is only re-checked when a reviewer records a new determination.
4. The tenant's `verified_residence` evidence collection must be active, or reviewers cannot record residences and every player is refused.
5. **Registration is permitted for visitors in excluded markets** (PD-MKT-REG-1), who then cannot do anything with the account. Owner can reverse.
6. Casino bet callbacks remain brand-level (`BrandOpen`), not per-player (vendor callbacks have no request evidence); deposit credit is not gated.
   Unchanged from A8 (MKT-WIRE-CREDIT-1, SR8-2).
7. Each gated deposit / launch / placement now does one extra read-only tenant transaction (one indexed query) before the gate's own transaction.
8. Market rows remain a manual operator action; no importer was built (instruction preserved).

## 8. Probe lists

**For `security`**
1. Any path that makes `Locate` take a country from the client (headers, body, query, a token claim) - none exists; try `X-Forwarded-For`, `CF-IPCountry`
   on deposit, launch, bet.
2. Cross-tenant: a tenant B token or B tenant id with A's player id (locator and `ReadMarketResidenceEvidence`), and a mismatched `app.tenant_id`.
3. Residence write path: who can record `verified_residence_country` (compliance review, collection-active switch, step-up) - this is now money-gating
   data; confirm `PermPlayerResidenceRead` / review permissions, audit rows, and that a reviewer cannot self-approve their own account.
4. Fail-closed on every error: pool closed, statement timeout, aborted transaction, panic in the reader, 2 s deadline, slot exhaustion.
5. Evidence flapping: a newer `pending` self-service verification (`POST /v1/me/kyc/verifications`) withdraws a verified player's access on demand -
   confirm this is acceptable (same as the deposit KYC clearance) and cannot be abused to lock another player (player-scoped only).
6. Registration oracle: the 403 vs 201 now reveals only brand-level openness (already public by brand); confirm there is no per-country signal.
7. Timing: unknown-evidence path now includes a DB read; compare with allowed / not-permitted paths.
8. Audit flood from authenticated players without KYC (one row per refusal, sampled per player bucket) - volume for a population with no residence.
9. Production startup: `requireMarketEvidenceInProduction` and the synthetic guard; confirm nothing can register a Synthetic Locator under another name.
10. Logs: no country, no IP in any new log line (`kyc` error strings contain none).

**For `ledger-finance`** (nothing in this change touches the ledger, wallets, balances or idempotency; probes are for decision, not defects)
1. Deposit initiation is now refused for a player without verified residence: confirm that is acceptable for players who deposited before the change
   (existing wallets keep their balances; withdrawals are NOT gated here).
2. Deposit credit / PSP callbacks / sweeper are still NOT market-gated (MKT-WIRE-CREDIT-1): a deposit initiated by a verified player whose residence is later
   withdrawn is still credited; decide hold / refund policy.
3. A refused deposit creates no wallet, intent or ledger row (tested through HTTP); confirm no `wallet.GetOrCreate` happens before the gate on
   any other deposit entry point.
4. Re-verification (pending) blocks deposits and launches but not withdrawals or settlements; confirm the intended asymmetry (settlement / rollback of
   existing exposure is never blocked).
5. Casino and sportsbook bets already accepted stay accepted and settle when the evidence is later withdrawn.

## 9. Verification runs

Private database `me20261016150610p5345x01` (dropped afterwards), runtime role, `-tags integration -count=1 -p 1`. Counts are top-level
`--- PASS` / `--- FAIL` / `--- SKIP` lines (subtests not counted; the marketgate package has 94 including subtests).

| Run | PASS | FAIL | SKIP |
|---|---|---|---|
| `./internal/marketgate/` | 54 | 0 | 0 |
| `-race ./internal/marketgate/` | 54 | 0 | 0 |
| `./internal/kyc/` | 307 | 0 | 0 |
| `./internal/launchgov/` | 103 | 0 | 0 |
| `./internal/tenant/` | 62 | 0 | 0 |
| `./cmd/...` (migrate, platform-api, seed-admin) | 90 | 0 | 0 |
| `./internal/sportsbook/` | 215 | 0 | 5 (pre-existing) |
| `./internal/casino/` | 341 | 0 | 1 (pre-existing) |
| `./internal/httpserver/` subset: every test in the market, register, deposit, launch, casino-flow, financial-flow and sportsbook test files (159 named tests) | 159 | 0 | 0 |
| `./internal/payments/` subset: every test in the 31 files that call `InitiateDepositAttempt` (277 named tests, ~25 min) | 277 | 0 | 0 |

Also: `gofmt -l internal cmd` clean; `go vet ./...` and `go vet -tags integration ./...` clean; `golangci-lint run ./...` (v2.9.0) 0 issues;
`go test ./...` (unit, whole module) all pass, including the static callers scan. The whole `httpserver` and `payments` packages were not run
(only the named subsets above).

## 10. Security review (independent, `security` specialist, 2026-10-16)

Scope: the branch diff against merge-base `12edec0` (`internal/kyc/market_evidence.go`, `internal/marketgate/{kycresidence,marketgate}.go`,
`cmd/platform-api/{main,registrations,wiring}.go`, `internal/httpserver/auth_routes.go`, the new and changed tests), plus the call sites in
`deposit_handlers.go`, `casino_handlers.go`, `sportsbook_handlers.go` and the residence write path (`kyc.ReviewVerification`,
`POST /v1/admin/kyc/verifications/{id}/review`). Code-level and design-level review only; not a penetration test or certification audit.
Not in scope: the whole `httpserver` / `payments` packages, the KYC vendor (none exists), and legal interpretation.

Runs (private DB `sm20261016161334p6650x01`, dropped afterwards, `-tags integration -count=1 -p 1`): `./internal/marketgate/` ok,
`./cmd/platform-api/` ok, `./internal/httpserver/ -run 'MarketGate|Register'` ok, `./internal/kyc/ -run 'Residence|Evidence|Review'` ok,
`-race ./internal/marketgate/ -run 'KYCResidence|Registration'` ok. Three throwaway probe tests (not committed) confirmed SRME-2 and SRME-3.

**Verdict: APPROVE for merge and staging. Not a production-launch approval**: SRME-1, SRME-2, SRME-4, SRME-5 and SRME-6 need fixes or owner /
legal decisions before real-money production. No Critical or High finding.

Verified as correct: the evidence rule (latest decided row approved and unexpired; residence only from an approved, unexpired row; orphan
intent rows excluded; database clock); tenant scope (`WithTenantReadOnly`, `app.tenant_id` assertion, repeated `tenant_id` predicate, RLS);
tenant and player only from the authenticated token; IP ignored; no client field can supply a country (the strict decoder rejects body
fields, headers are ignored; tested); fail-closed on unbound locator, nil ids, DB error, closed pool, cancelled context, timeout, panic, slot
exhaustion; one identical `403 MARKET_NOT_AVAILABLE` body for every cause; audit metadata carries closed codes (`unknown` / `resolved` /
`brand_level`) and ids only, never a country; refusal sampler unchanged and still bounded; `KYCResidenceLocator` is ProductionEligible and
not Synthetic; `requireMarketEvidenceInProduction` refuses none / nil / two / wrong-name; `MOCK_GEOLOCATION_COUNTRY` is rejected by
`config.Load` under `APP_ENV=production` and ignored by `mockProviderWiring` whenever the guard environment is production (including a
missing `APP_ENV`), and the static Locator is refused by the synthetic guard, so there is no production bypass; `bindMarketEvidence` binds the
same instance the gate holds and a nil pool fails startup. The five changed assertions are deliberate semantic changes, not weakenings
(#3 is strictly stronger: exact `403` instead of "not 201", which previously passed on a 400).

| Id | Severity | Finding / concrete failure scenario | Recommendation | Blocks |
|---|---|---|---|---|
| SRME-1 | Medium | The residence determination is now money- and game-gating data but is written by ONE staff member with `PermVerificationReview`, `stepUpNone`, no four-eyes and no maker-checker (`kyc_routes.go` review route). A compromised or colluding reviewer sets `verified_residence_country = MT` for a player resident in an excluded market; the gate then admits deposit, launch and placement. The change is audited (no country in metadata) but nothing prevents it. Pre-existing write path whose impact this change raises. | Before production: step-up on any review that records or changes a residence, and either four-eyes on residence determinations or a periodic second-reviewer sample with alerting on residence changes into open markets. | Production launch (not staging, not sandbox provider work) |
| SRME-2 | Low-Medium | Carry-forward crosses an intervening rejection or expiry. Probe: approved(MT) -> rejected -> approved (no new determination) yields evidence MT. A verification rejected for false address / identity fraud and later re-approved without a fresh determination silently re-uses the earlier residence. The record documents carry-forward across routine re-verification, not across a rejection. | Only carry a determination forward when no non-approved decided row (rejected / expired / review_required) lies between it and the latest approved row, or require a fresh determination after any rejection. Add a test. | Production launch |
| SRME-3 | Low | TOCTOU between evidence read and action. Evidence is read once in its own read-only transaction; the in-transaction `Guard` re-checks the market POLICY but reuses the stale evidence. Probe: Locate (MT, approved) -> a rejection commits -> `Check(OpLaunch)` in a new transaction ALLOWS. Window = milliseconds between the read and the action transactions; not attacker-controllable (it needs a concurrent reviewer decision). Deposit phase A also re-runs KYC enforcement in-transaction, which narrows it for deposits when thresholds require KYC; launch and placement have no such recheck. | Re-read the evidence inside `Check`'s transaction for player operations (the reader already accepts a `RowQuerier`), keeping the outside read only as a cheap pre-filter. | Nothing (fix opportunistically) |
| SRME-4 | Medium (compliance) | Residence is not location: a verified resident of an open market can deposit and play from an excluded jurisdiction (travel, VPN); the platform has no physical-location evidence. Whether the Anjouan licence or any later B2B jurisdiction requires location (not residence) blocking is a legal question this review cannot answer. | Owner + legal confirmation that residence-only evidence is acceptable for each licensed jurisdiction before production; otherwise a vendor geolocation Locator (PROVIDER DEPENDENT) is a launch prerequisite. | Production launch decision (human) |
| SRME-5 | Medium (residual, pre-existing MKT-WIRE-CREDIT-1 / SR8-2) | Not market-gated: deposit credit callbacks / sweeper and casino bet callbacks (brand-level only). Scenario: a player's evidence is withdrawn (rejection, re-verification) after launch; the open game session keeps accepting bets until the session ends, and an in-flight deposit is credited. Exposure is bounded by session lifetime and by deposits initiated while evidence existed. | Decide before real-money production: revoke / refuse game sessions when evidence is withdrawn (or bound session lifetime), and a hold policy for credits whose player lost evidence. | Production launch; not staging; not sandbox provider integration |
| SRME-6 | Low | PD-MKT-REG-1. Safe in the engineering sense: no country input, no IP, no per-country oracle (201 vs 403 reveals only whether the brand is open for registration somewhere, which is public); a registered unverified account cannot deposit, launch or bet (tested through HTTP). What it allows: anyone, including an excluded-market resident, can create an unverified account (email + password hash stored) and reach KYC submission. Abuse: account-creation flood is bounded by the existing per-IP register rate limiter (distributed attackers could already do this from an allowed country before the change); each new account gets its own refusal-sampler bucket, so audit rows from deliberate deposit attempts grow with the number of accounts (5 per new bucket, then 1/min), bounded by the per-tenant and global bucket caps and the registration rate. | Accept for staging. For production, accept only if the owner / legal confirm that permitting account opening (not play) by excluded-market residents is compatible with the licence terms (some licensors treat account opening itself as "accepting" a restricted-country player); otherwise choose option B (declared country + KYC recheck) or disable registration per brand. | Owner decision before production |
| SRME-7 | Info | Stale comment at `cmd/platform-api/main.go` (`MarketGate: marketgate.New(providers.Geolocation)`) still says the gate runs "over the bundle's MOCK static Locator" and "refuses every request" without `MOCK_GEOLOCATION_COUNTRY`. Misleading to the next reader of the production wiring. | Update the comment. | Nothing |
| SRME-8 | Info | `requireMarketEvidenceInProduction` counts a non-nil component; it does not check the type or `Bound()`. A typed-nil or unbound locator would pass the count but fails closed at runtime (inconclusive -> refuse), and `bindMarketEvidence` fails startup on a nil pool. Acceptable; noted so nobody reads the check as "evidence is operational". | Optional: also assert `Bound()` after `bindMarketEvidence` in production. | Nothing |

Probe answers (section 8): (1) none; (2) isolated, mismatch errors; (3) see SRME-1 (staff and player identities are separate tables; a
reviewer approving "their own" player account was not tested and is not in scope); (4) fail closed everywhere checked; (5) a player can
withdraw only their own evidence via `POST /v1/me/kyc/verifications` (self-lockout only), acceptable and consistent with deposit KYC
clearance; (6) no per-country signal; (7) the unknown, resolved and not-permitted player paths all perform the same evidence read, and the
player already knows their own KYC status, so timing reveals nothing new; (8) see SRME-6; (9) a Synthetic Locator under another name is
refused by the synthetic guard (any registration), and a missing `geolocation/locator` by the new check; (10) no country or IP in new log
lines or error strings.

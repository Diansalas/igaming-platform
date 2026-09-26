# ADR 0085 — APP_ENV Fail-Closed Validation and Stateless Activation Seam

Status: Accepted. Owner: `backend`, reviewed by `security` and `architect`.

## Context

Stage 9.3's real end-to-end acceptance work against a local
staging-equivalent stack surfaced two related, genuine correctness gaps in
the three non-production "test-support" HTTP routes introduced across
Stages 7 and 9.3 (`POST /v1/me/casino/sessions/{id}/{wager,win,rollback}`
— ADR 0048; `POST /v1/me/deposits/{id}/simulate-callback`; `GET
/v1/me/email-verification/dev-token`, since redesigned — see below). Both
gaps were explicitly deferred out of Stage 9.3's scope as needing a
dedicated config/architecture decision rather than a unilateral fix, and
are closed here under Stage 9.4:

1. **APP_ENV fail-open.** `internal/config.Load()` computed `Environment`
   from an unvalidated, open string (`os.Getenv("APP_ENV")`, defaulting to
   `"development"` if unset via a `getEnvDefault`-style helper), and every
   caller gated its own simulation flag purely on
   `cfg.Environment != "production"`. Two independent Stage 9.3 security
   reviews demonstrated this fails OPEN: `"Production"` (wrong case),
   `"prod"` (a plausible alias), `"production "` (trailing whitespace), or
   a simply-unset `APP_ENV` on a real deployment all silently registered
   test-support routes — including one that mints ledger-affecting
   financial transactions — on what the operator believed was a
   production deployment. A wrong string comparison is not a security
   control.

2. **Activation-seam token store was per-process, in-memory.** Stage 9.3
   added `GET /v1/me/email-verification/dev-token` so a real HTTP client
   could complete account activation without a real email provider
   (`AccountActivationTestSupportEnabled`). Its first implementation held
   issued tokens in an in-memory map on the `httpserver.Deps` that handled
   the `request`/`resend` call. Stage 9.4's own directive requires this
   work correctly when request 1 (issue) reaches ECS/Fargate replica A and
   request 2 (retrieve) reaches replica B — the exact shape
   `desired_count=2` (ADR 0084) exists to exercise. An in-memory,
   per-process store cannot do that: the token would appear to not exist
   on any replica that didn't happen to receive the issuing request.

## Decision

### 1. Two-layer, fail-closed gate for all non-production test-support routes

Replace the single `Environment != "production"` check with two
independent, ANDed conditions, both required, computed in exactly one
place (`internal/config.Config.TestSupportRoutesEnabled()`) rather than
duplicated per call site:

- **Layer 1 — closed-set `APP_ENV` validation.** `Load()` now validates
  `Environment` against the exact closed set `{"development", "staging",
  "production"}` whenever `APP_ENV` is explicitly set to *any* value,
  including an explicit empty string (`APP_ENV=`) — only a variable that
  is genuinely absent from the process environment (`os.LookupEnv`'s
  `ok == false`) resolves to the `"development"` default. Any other value
  — a typo, wrong case, stray whitespace, or an explicit empty string —
  fails `Load()` outright at startup, rather than silently resolving to
  "not production". (The explicit-empty-string case was found
  independently, after the initial two-layer implementation, by both the
  `security` and `architect` review passes on this exact change — see
  `internal/config.resolveAppEnv`'s doc comment for why a naive
  `getEnvDefault`-style helper, correct for every other setting in this
  file, is specifically wrong for `APP_ENV`.)
- **Layer 2 — a second, independent, explicit opt-in.** A new field,
  `TestSupportEndpointsEnabled` (`TEST_SUPPORT_ENDPOINTS_ENABLED`),
  defaults to `false`. Both layers must independently be satisfied before
  any of the flags this ADR gates register (see the count and full list
  below, updated at Stage 10.2 final review K6):
  `cfg.Environment != "production" && cfg.TestSupportEndpointsEnabled`.
  A wrong/typoed `Environment` value alone can no longer register
  anything (Layer 1 already rejects it at startup), and a deployment that
  simply forgets to set `TestSupportEndpointsEnabled` gets it disabled by
  default, regardless of `Environment`.
- **Contradictory configuration is a hard startup failure, not a silent
  correction.** `Environment == "production" && TestSupportEndpointsEnabled
  == true` fails `Load()` outright with a named error. An operator setting
  both is directly attempting something unsafe; the correct response is a
  loud failure, not an override.

`cmd/platform-api/main.go` computes all **four** `Deps` flags
(`CasinoPlaySimulationEnabled`, `PaymentsMockSettlementEnabled`,
`AccountActivationTestSupportEnabled`, and Stage 10 W1's
`SportsbookSettlementSimulationEnabled`) directly from
`cfg.TestSupportRoutesEnabled()` — confirmed by grep, during security
review (and re-confirmed at Stage 10.2 final review, K6/M5), to be the
only place any of the four is computed from `Environment` at all, so
there is exactly one place this logic can be gotten wrong. Stage 10.2
(ADR 0091) adds a fifth consumer that is not itself a `Deps` field:
`cmd/platform-api/wiring.go`'s `mockProviderWiring(cfg)` derives its own
`testSupport` bool from the same `cfg.TestSupportRoutesEnabled()` call
and uses it to decide whether to wire the KYC mock/orchestrator and the
payments/casino mock webhook resolvers, including setting
`Deps.KYCWebhookEnabled` — so `TestSupportRoutesEnabled()` gates five
things in total (four direct `Deps` flags plus `mockProviderWiring`), all
still computed from the one function.

`Environment` itself remains informational elsewhere (used in
logs/traces) and must never gate a security control outside the named,
reviewed exceptions listed in §1 (the four `Deps` flags plus
`mockProviderWiring`) — see `internal/config.Config`'s own doc
comment on the `Environment` field for the exhaustive list.

*Amended 2026-09-26 (Stage 10.2, ADR 0091; PAYWH-GATE-1, KYC-WH-1,
CAS-WH-TENANT-1):* Stage 10.2 also derives mock-provider **wiring** from
`cfg.TestSupportRoutesEnabled()`. It does this in one place,
`cmd/platform-api/wiring.go` `mockProviderWiring(cfg)`. That function is
pure (no I/O, no globals) and is unit-tested for {production,
non-production} × {flag on, flag off}. It covers the KYC mock provider,
its resolver and its webhook route, and the casino and payments mock
webhook resolvers. Route registration and resolver wiring for the same
domain come from the same result, so they cannot diverge. When the gate
is off, payments and casino get a nil resolver (every callback 401
`no_resolver`) and the KYC mock and route are absent. This introduces no
new gate and no new use of `Environment`. It is one more consumer of the
same two-layer gate. See docs/decisions/0022 §3 (Stage 10.2 amendment)
and docs/decisions/0028 (Stage 10.2 amendment).

#### Amendment (Stage 10.3, ADR 0092) — synthetic-component production guard

*Amended 2026-09-26 (Stage 10.3, ADR 0092; MOCK-ADAPTER-PROD-1, wave W1b).
Recorded by `architect`. Sources: paper
`docs/plans/stage-10.3-planning/01-provider-trust-analysis.md` §3 and
security condition C13, adopted by ruling R8.*

**The gap.** Verified at `c90e591`. `cmd/platform-api/main.go` registers
these synthetic components in every environment:

- `mock-payments` and `mock-casino`;
- the sportsbook mock, whose catalogue is synced into the database at
  startup;
- `MockPersonResolver`;
- `MockDocumentStorageProvider`;
- `MockMalwareScanner`;
- the email mock;
- `sportsbook.MockSettlementStatementSource`, for the reconciliation
  scheduler.

Only the KYC mock is gated.

**The guard.** This is a new reviewed exception to the rule that
`Environment` never gates a security control. It belongs to a closed set,
together with `TestSupportRoutesEnabled()`, `mockProviderWiring` and
`VerifyRuntimeRoleInProduction`.

1. **Markers.** A leaf package declares two markers:
   - `Synthetic` (`SyntheticComponent()`), implemented by every mock or
     fake;
   - a positive `ProductionEligible` marker.
2. **Production eligibility.** In production, every registered component
   must implement `ProductionEligible` **and must not** implement
   `Synthetic`. A new component with no marker is therefore refused by
   default. Today no component is production-eligible, so this costs
   nothing now.
3. **The guard function.** `syntheticGuard(cfg, regs)` is pure. It runs
   **immediately after `config.Load()`**, before tracing, `db.Connect` and
   `SyncCatalogue`. If any registered component fails the rule, it returns
   a named error listing those components, and the process **refuses to
   start** (exit 1). Components are never silently omitted.
4. **When the guard applies.**
   - The guard applies when `APP_ENV=production` **or when `APP_ENV` is
     missing** (R8).
   - For this guard, and for the secret-store backend allow-list in
     ADR 0093 §6, a missing `APP_ENV` is treated as production.
   - Synthetic components and `devfile://` require `APP_ENV` to be
     **explicitly present** and in {`development`, `staging`}.
   - `Config` records whether the value was set explicitly.
   - Layer 1's handling of explicit values is unchanged. The general
     `"development"` default for an absent `APP_ENV` also remains for
     every other consumer.
5. **No flag bypass.** The guard's only configuration input is the
   environment. A test runs it with every boolean in `Config` set to true,
   and again with every one set to false, and it still refuses in
   production. The only way past it is to remove the synthetic
   component.
6. **Coverage.** *(Corrected at gate 10.3-W1, code review #2 and #7 and
   security F4. The text at acceptance said `buildRegistrations`
   enumerates everything `main` wires and that the AST scan is not based
   on type names. Neither was true at HEAD `bc72fe4`.)*
   - **What `buildRegistrations` covers.** `buildRegistrations(cfg, b)`
     (`cmd/platform-api/registrations.go`) enumerates the
     `providerBundle`, which `buildProviderBundle` builds once before
     `db.Connect`. The completeness test proves that every bundle field is
     registered. It does not prove that `main` and `wiring.go` construct
     nothing outside the bundle.
   - **The gap at HEAD.** The three MOCK webhook credential resolvers are
     built by helpers in `wiring.go`, called from `main.go`, outside the
     bundle. They are never registered.
     They are gated by `TestSupportRoutesEnabled()`, which reads
     `Environment`, not `GuardEnvironment()`. This is not exploitable
     today, because the MOCK adapters they are bound to are always
     registered, so the guard refuses production startup first.
   - **Stage 10.3 W1 fix round.** The mock webhook credential resolvers
     move into the provider bundle and are registered. Each adapter's
     `WebhookScheme()` is also registered in `buildRegistrations`. Both
     completeness tests use one shared helper.
   - **Still open.** An AST test that no non-test file in
     `cmd/platform-api` other than `registrations.go` constructs a
     provider component (security S-4 point 2). It binds before any type
     implements `ProductionEligible`.
   - **The real control is the positive marker, and it does not use
     names.** In production, `RefuseSyntheticInProduction`
     (`internal/providerkind/guard.go`) refuses every registered component
     that does not implement `ProductionEligible`, or that implements
     `Synthetic`. An unmarked component is refused whatever its name.
   - **The AST scan is a secondary hygiene check, and it does use a name
     heuristic.** `ScanForUnmarkedMocks`
     (`internal/providerkind/completeness_scan.go`) flags a type only if
     its name matches `(?i)(Mock|Fake|Stub|InMemory)` and it has no
     `SyntheticComponent` method. It skips unexported types. It keeps the
     `Synthetic` marker consistently applied. It is not what makes the
     guard safe.
   - `cmd/seed-admin` and `cmd/migrate` wire no mock or provider
     component, and a test keeps it that way. Verified at `c90e591`:
     their only internal imports are `audit`, `auth`, `db` and `identity`,
     and `db` respectively.
7. **`memory://` is test-only.** The `memory://` secret-store backend can
   be constructed only from test code (ADR 0093 §6).
8. **Tests.**
   - An ordering test in a subprocess: with `APP_ENV=production` and an
     unreachable `DATABASE_URL`, the process fails with the guard's error,
     not with a DB error. It is tagged `//go:build integration`.
   - A matrix covering {production, missing, staging, development} ×
     {synthetic, eligible, unmarked}.
   - A negative control proving that the marker scan goes red.

**Consequence (disclosed).** A production binary cannot start until every
registered domain has a real, production-eligible implementation or is not
registered at all. Which verticals launch without a real provider is a
human launch decision. The guard only makes that decision explicit.

**Disclosed at gate 10.3-W1.**
- **Today's binary refuses to start in production.** Every component in
  the provider bundle is a MOCK. So a binary started with
  `APP_ENV=production`, or with `APP_ENV` missing, exits at the guard
  before `db.Connect`. This is the intended fail-closed result, not a
  defect.
- **`make run` sets `APP_ENV=development`.** Because a missing `APP_ENV`
  is now treated as production by the guard, `make run` without it would
  refuse to start. The Stage 10.3 W1 fix round makes the `run` target
  default `APP_ENV=development`, overridable from the environment. At HEAD
  `bc72fe4` the target does not set it (code review #8).

**Status.** `NOT IMPLEMENTED` at acceptance. Target: `IMPLEMENTED` in W1b.

**Status at gate 10.3-W1.** The markers, the pure guard, its placement
right after `config.Load()`, the `GuardEnvironment()` rule and the matrix
tests are implemented at HEAD `bc72fe4`. The coverage corrections in point
6 (resolvers and schemes registered, shared completeness helper) are in
progress in the Stage 10.3 W1 fix round. Security S-4 point 2 binds before
any type implements `ProductionEligible`. No component is
`ProductionEligible` today.

### 2. Stateless-by-construction activation seam (no shared infrastructure)

Rather than replacing the in-memory map with a shared store (Redis, a new
Postgres table, or similar), `GET /v1/me/email-verification/dev-token` is
removed entirely. `POST /v1/me/email-verification/request` (and
`/resend`) now, when `AccountActivationTestSupportEnabled` is true, returns
the raw verification token directly in that same request's `200` response
body (`{"token": "..."}`) at the exact point the handler already holds it
in memory — immediately before handing it to `EmailProvider.Send`.
Flag-off/production behavior is byte-for-byte unchanged: `204 No Content`,
no body, exactly as before.

This is deliberately **not** "add a shared cache/store" — it needed no new
infrastructure because the value already existed, correctly, inside the
one request/response that creates it; only the previous design's choice to
stash it in a second, separate, stateful, per-process retrieval path was
the actual defect. The general precedent this establishes: **a
same-request-response reveal is preferred over any cross-request seam
state; cross-request state that must be visible to every replica always
belongs in Postgres (the platform's one shared, replica-independent source
of truth), never in an in-process, per-replica data structure.** This
mirrors, and is consistent with, how every other credential/token flow in
this codebase already works (`internal/auth.IssueCredentialToken` persists
to `player_credential_tokens`, not an in-memory map) — the deleted store
was the one exception, introduced under Stage 9.3's own time pressure, and
is now brought into line rather than justified as a special case.

Security review confirmed this reveal changes no forgery-resistance
property: `AccountActivationTestSupportEnabled` already implies the
caller is a real, authenticated, rate-limited request for that player's
own account (the same authorization boundary `internal/auth
.credential_token.go`'s token issuance already enforces, unaffected by
this change); this only removes an unnecessary second retrieval hop, it
does not weaken who may obtain the token.

Multi-replica correctness is proven directly, not merely asserted:
`internal/httpserver/email_verification_dev_token_test.go`'s
`TestAccountActivationDevToken_MultiReplica_RequestOnReplicaA_ConfirmOnReplicaB`
constructs two genuinely independent `httptest.NewServer` instances (each
its own freshly-constructed `Deps`, sharing only the same `*db.Pool` — the
real cross-replica boundary, Postgres), issues the request on replica A,
confirms on replica B, and asserts both replicas independently observe the
account transition to `active`.

## What this decision does not do

- It does not change any production-path authentication, session, or
  credential-issuance logic. `internal/auth/credential_token.go` (hashing,
  TTL, single-use consumption) is untouched.
- It does not add Redis, a cache, or any new shared-state infrastructure —
  the explicit instruction this ADR follows was "prefer an existing
  platform component... do not introduce unnecessary infrastructure",
  and the smallest correct fix needed none.
- It does not close the pre-existing, NOT-9.4-caused race in
  `IssueCredentialToken` (concurrent `POST .../request` calls for the same
  player can produce two simultaneously-live, independently-confirmable
  tokens under READ COMMITTED's check-then-act semantics) — security
  review confirmed this is identical behavior with the flag off and
  predates this change; it affects `PurposeEmailVerification` and
  `PurposePasswordReset` symmetrically and is recorded as a separate,
  narrower deferred item (a partial unique index or per-account lock),
  not folded into this ADR's scope.
- It does not touch `docs/decisions/0009-hosting-hyperscale-cloud.md`'s
  open AUP/legal confirmation or any jurisdiction/licensing decision.

## Related fix in the same stage: mock provider reference generation

While reviewing this change, `architect` independently found the same
multi-replica correctness bug *shape* elsewhere on the exact financial
simulation path this ADR's `desired_count=2` staging target exists to
exercise: `internal/payments.MockProvider.nextReference()` and
`internal/casino.MockCasinoProvider.nextReference()` each minted
`provider_reference`/`provider_tx_id` values from a bare per-process
`seq int` counter, so two replicas could each independently mint
`"mock-payments-1"` for their own first deposit — colliding on
`deposit_intents`' `(tenant_id, provider_id, provider_reference)`
uniqueness constraint and aliasing onto the same ledger idempotency key
(`provider_id, provider_tx_id`). This is not a Layer 1/Layer 2 gate
question (both mocks are already correctly gated by
`TestSupportRoutesEnabled()`/the mock-provider registration in
`cmd/platform-api/main.go`) — it is a separate, genuine multi-replica
determinism bug in reference generation, fixed in the same stage because
it directly blocks this ADR's own multi-replica correctness goal for the
real deposit-simulation flow. Both `nextReference()` implementations now
append a `uuid.NewString()` suffix (already a `go.mod` dependency, used
throughout both packages) to the existing per-process sequence number,
which remains only as a human-readable ordering hint in logs. This does
not change either mock's forgery-resistance property, which was already
established (per each type's own doc comment) via mandatory HMAC
signature verification in `HandleCallback`, never via reference
unpredictability.

## Consequences

- Any future real deployment (staging or production) must set `APP_ENV`
  explicitly; relying on the unset-default `"development"` fallback in a
  real environment is now an operator error Layer 1 cannot detect by
  itself, precisely because "unset" and "deliberately development" remain
  indistinguishable once resolved — documented in
  `docs/runbooks/production-configuration-checklist.md`.
- `deploy/aws/modules/ecs/variables.tf`'s new
  `test_support_endpoints_enabled` variable defaults to `false` at the
  module level (not merely per-environment), so any future environment
  reusing this module without an explicit override stays closed by
  default; `deploy/aws/environments/staging/main.tf` sets it to `true`
  explicitly, with the reasoning recorded inline.
- `docs/decisions/0048-casino-play-simulation-trust-boundary.md`'s
  mitigation list has been amended in place to point to this ADR rather
  than describe the now-superseded single-condition gate.

# Production Configuration Checklist

Owner: `devops`. Goes through every field of `internal/config.Config`
(`internal/config/config.go`), field by field, stating: what it is, its
env var, whether it's required, its dev default (if any), and the
production-specific action an operator must take. There is no config
FILE support by design (`config.go`'s own package doc comment) — every
value below comes from the process environment, or, once a secrets
manager is wired in front of it, from whatever injects environment
variables into the container/process (Vault agent, cloud KMS-backed
secret store, etc.) — never a checked-in file. See `docs/architecture/
38-deployment-architecture.md` for the deployment shape this checklist
assumes.

**Never plaintext.** Nothing in the "secret" column below may be
committed to git, placed in a Kubernetes ConfigMap (as opposed to a
Secret), or logged. `internal/config` never logs its own field values
(only `cfg.Environment` is logged, deliberately — see
`cmd/platform-api/main.go`'s startup log line — because every other field
is either a secret or noise).

## Database

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `DatabaseURL` | `DATABASE_URL` | Yes — startup fails without it | none | **Secret.** Must point at the non-owning `igaming_runtime` credential (or your production naming equivalent), never the migration-owner role — see `docs/security/runtime-role-separation.md`. `db.VerifyRuntimeRoleInProduction` refuses to start if this is wrong AND `APP_ENV=production` (see below), but get it right regardless of that safety net. Include `sslmode=require` (or stricter) in production — the dev/CI convention's `sslmode=disable` is a local-only shortcut. |
| `DatabaseMaxConns` | `DATABASE_MAX_CONNS` | No | `10` | Tune to the real database's `max_connections` divided by expected replica count, with headroom for the migration step's own separate connection and any operational tooling. Not a secret. |
| `DatabaseConnTimeout` | *(not env-configurable — hardcoded `5s` in `Load()`)* | n/a | `5s` | No action today; if this needs to be tunable in production, that's a small, additive config change, not something to work around by editing the constant per-environment. |

## Environment identity

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `Environment` | `APP_ENV` | No (defaults, but treat as required in practice) | `development` | **Set to exactly `production`** (case-sensitive, exact string — `config.go`'s own comment and `db.VerifyRuntimeRoleInProduction`'s doc comment both call this out) in the real production environment, and ONLY there. This field gates every deliberate, reviewed exception to "environments are otherwise identically configured" (`config.go`'s own doc comment on `Environment`), and there are now four: (1) the fail-closed runtime-role-ownership check (`db.VerifyRuntimeRoleInProduction`); (2) `CasinoPlaySimulationEnabled` (Stage 7); (3) `PaymentsMockSettlementEnabled` (Stage 9.3); (4) `AccountActivationTestSupportEnabled` (Stage 9.3). **(2), (3), and (4) fail OPEN on a typo**: `cmd/platform-api/main.go` computes them as `cfg.Environment != "production"`, and `APP_ENV` is neither validated against a closed set nor required, so `Production`, `prod`, `production ` (trailing space) or an unset variable all register the mock-provider simulation routes — `POST /v1/casino/.../simulate-*`, `POST /v1/me/deposits/{id}/simulate-callback`, and `GET /v1/me/email-verification/dev-token` — which let any authenticated player settle their own deposits, self-sign their own casino wins, and retrieve their own pending email-verification token, all without any real money, real provider, or real inbox being involved. Leaving it unset in real production therefore does far more than "silently disable a safety check": it exposes a money-minting surface and a credential-retrieval surface. Setting it incorrectly in a non-production environment forces that environment to provision a non-owning role it may not need yet and disables the staging simulation seams the acceptance-test flows depend on. Not a secret, but the single most load-bearing string in this table — verify it from the running process's own startup log (`starting platform-api environment=...`), never from the deployment template alone. |

## HTTP

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `HTTPAddr` | `HTTP_ADDR` | No | `:8080` | Usually left as-is; the orchestrator/load balancer routes to whatever port the container exposes. Not a secret. |

## JWT / authentication

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `JWTActiveKID` | `JWT_ACTIVE_KID` | No | `k1` | Any stable identifier; used only to select the active signing key. Not a secret. |
| `JWTSigningSecret` | `JWT_SIGNING_SECRET` | Yes — startup fails without it, and fails if under 32 characters | none | **Secret.** Generate with a real CSPRNG (e.g. `openssl rand -base64 48`), at least 32 characters, per environment — never reused between dev/staging/production, and never the value in `.env.example` or any doc. This is the Stage 2 foundation mechanism (HMAC shared secret); `internal/config`'s own doc comment marks production authentication as PROVIDER DEPENDENT — evaluate moving to asymmetric signing backed by a KMS-managed key or a real identity provider before go-live (`docs/security/security-architecture.md`'s production authentication evaluation) rather than treating this as the permanent production mechanism. |
| `JWTPreviousKID` | `JWT_PREVIOUS_KID` | No | `k0` | Only meaningful once `JWT_PREVIOUS_SECRET` is also set (key rotation in progress). Not a secret. |
| `JWTPreviousSecret` | `JWT_PREVIOUS_SECRET` | No, but same 32-char minimum if set, and must differ from `JWTActiveKID`'s KID | none | **Secret when set.** Set this to the OUTGOING signing secret during a rotation window (so already-issued, not-yet-expired tokens still verify), then remove it once the longest-lived outstanding token from before the rotation has expired (`RefreshTokenTTL`, below, bounds how long that window needs to be kept open). |
| `JWTIssuer` | `JWT_ISSUER` | No | `igaming-platform` | Not a secret. Deliberately independent of `OTelServiceName` (see `config.go`'s own comment) — do not repoint it at a telemetry setting. |
| `JWTAudience` | `JWT_AUDIENCE` | No | `platform-api` | Not a secret. |

## Session lifetime

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `AccessTokenTTL` | `ACCESS_TOKEN_TTL_SECONDS` | No | `900` (15 min) | Not a secret. Shorter limits a leaked access token's blast radius (nothing to revoke — it just expires); shortening further trades off more frequent refresh traffic. |
| `RefreshTokenTTL` | `REFRESH_TOKEN_TTL_SECONDS` | No | `2592000` (30 days) | Not a secret. This is the effective upper bound on how long a `JWTPreviousSecret` rotation window must stay open (above) — the longest-lived outstanding refresh token issued under the old secret must expire, or be rotated out, before removing it. |

## Observability

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `OTelServiceName` | `OTEL_SERVICE_NAME` | No | `platform-api` | Not a secret. Keep distinct per logical service if this binary is ever split (see `docs/architecture/38-deployment-architecture.md` §4's note on scheduler loops potentially moving to a dedicated worker process later). |
| `OTelExporter` | `OTEL_EXPORTER` | No | `stdout` | Not a secret. `stdout` is the Stage 1 foundation choice (human-readable, no external collector) — production should point this at a real OTLP exporter once a concrete observability backend exists; introducing that backend and its own endpoint/credential is a separate operational decision, not covered by this checklist since none is contracted yet (PROVIDER DEPENDENT, same category as the KYC/casino/payment mock providers). |

## Scheduled jobs

All four cadence groups below default to values already matching the
Blueprint's own targets (`docs/architecture/00-system-overview.md`) or
each job's own documented consequence-of-lateness reasoning
(`internal/config/config.go`'s field comments spell out why each default
was chosen) — production should normally leave every one of these at its
default rather than tuning them, and only override for a specific,
diagnosed reason (e.g. temporarily tightening a sweep interval while
investigating an incident).

| Field | Env var | Dev default | Notes |
|---|---|---|---|
| `ReconciliationInterval` | `RECONCILIATION_INTERVAL_SECONDS` | `3600` (1h) | Ledger-vs-projection drift sweep. Must be positive if set. Not a secret. |
| `RGEnumerationSweepInterval` | `RG_ENUMERATION_SWEEP_INTERVAL_SECONDS` | `900` (15 min) | Self-exclusion enumeration-run reconciliation. Deliberately tighter than the reconciliation sweep — a dropped run is a compliance-enforcement gap, not just a financial-drift one. Must be positive if set. Not a secret. |
| `RGEnumerationStalledThreshold` | `RG_ENUMERATION_STALLED_THRESHOLD_SECONDS` | `900` (15 min) | How old an enumeration run may be before it's reported stalled. Must be positive if set. Not a secret. |
| `BonusDepositSweepInterval` | `BONUS_DEPOSIT_SWEEP_INTERVAL_SECONDS` | `300` (5 min) | Must be positive if set. Not a secret. |
| `BonusCashbackSweepInterval` | `BONUS_CASHBACK_SWEEP_INTERVAL_SECONDS` | `3600` (1h) | Must be positive if set. Not a secret. |
| `BonusExpirySweepInterval` | `BONUS_EXPIRY_SWEEP_INTERVAL_SECONDS` | `3600` (1h) | Must be positive if set. Not a secret. |

## Rate limiting (S9.1-LAUNCH-1/S9.1-LAUNCH-2, closed Stage 9.1)

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `AuthRateLimitPerMinute` | `AUTH_RATE_LIMIT_PER_MINUTE` | No | `0` (per-bucket defaults in `internal/httpserver/ratelimit.go`) | Not a secret. `0` uses the built-in per-bucket defaults (the sane production-appropriate starting point); a positive value overrides EVERY bucket; a negative value disables the limiter entirely (do not do this in production without an equivalent control at the edge). Normally leave at the default. |
| `TrustedProxyCount` | `TRUSTED_PROXY_COUNT` | No — but see action | `0` (X-Forwarded-For never trusted) | Not a secret, but load-bearing and easy to get subtly wrong. **Must be set to the EXACT number of this deployment's own trusted reverse-proxy hops** the moment any load balancer/ingress/reverse proxy sits in front of `platform-api` — leaving it at `0` behind a real proxy collapses every distinct client onto the proxy's own address (the rate limiter becomes "per service," not "per client"); setting it too HIGH lets a client's own injected `X-Forwarded-For` entry be mistaken for the trusted one. See `internal/httpserver/server.go`'s `Deps.TrustedProxyCount` doc comment and `docs/architecture/38-deployment-architecture.md` §2/§4 for the exact trust model. |

## Cross-origin access (Stage 9.3)

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `CORSAllowedOrigins` | `CORS_ALLOWED_ORIGINS` | No | empty (no CORS headers at all — same-origin only) | Not a secret. Comma-separated exact-match browser origins (e.g. `https://staging.example.com,https://admin-staging.example.com`); a bare `*` is rejected at startup. Only needed when the frontend(s) and the API are deployed on different origins — leave unset for a same-origin (reverse-proxied) deployment. This is a browser convenience, never an authorization boundary: see `internal/httpserver/cors.go`'s doc comment. |

## Fields intentionally NOT in `internal/config.Config` — do not add them here casually

- **Mock provider credentials/webhook secrets** (`kycMockWebhookSecret`
  in `cmd/platform-api/main.go`, the mock casino/payment/email/KYC
  provider registrations) are hardcoded dev/test-only values, clearly
  labeled as such, and are never read from the environment. They must
  never be used in production — a real provider integration would add
  its own per-tenant credential fields (see `docs/decisions/` for the
  relevant provider-integration decisions), not repurpose these mock
  constants.
- **CI-only values** (`JWT_SIGNING_SECRET`'s CI value, the
  `TEST_DATABASE_URL`/`TEST_RUNTIME_DATABASE_URL` pair used only by the
  integration test suite — see `docs/security/runtime-role-
  separation.md`) are test fixtures, not production configuration, and
  must never be copied into a production environment.

## Pre-launch verification checklist

Before pointing real traffic at a `platform-api` instance in an
environment with `APP_ENV=production`:

1. `DATABASE_URL` points at `igaming_runtime` (or equivalent non-owning
   role) — confirm by letting the process start: `db.
   VerifyRuntimeRoleInProduction` fails closed with a named error if this
   is wrong (see `docs/security/runtime-role-separation.md`).
2. `JWT_SIGNING_SECRET` (and `JWT_PREVIOUS_SECRET`, if rotating) are
   freshly generated, environment-specific secrets pulled from a secrets
   manager, never a value that also appears in `.env.example`, this
   document, or any committed file.
3. Every secret above is confirmed absent from build logs, application
   logs, and error responses (`internal/config` never logs field values;
   confirm nothing downstream does either).
4. `/healthz` and `/readyz` both return `200` before the load balancer
   is told this instance is in service.
5. The migration step (`cmd/migrate up`) has completed successfully
   against this exact environment's database before this step is
   reached — see `docs/architecture/38-deployment-architecture.md` §2.

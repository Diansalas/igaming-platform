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
| `DatabaseIdleInTxTimeout` | `DATABASE_IDLE_IN_TRANSACTION_TIMEOUT_SECONDS` | No | `30` (seconds, 1..3600; never set to 0 by the application) | Sent as `idle_in_transaction_session_timeout` in the startup packet of every connection of the application pool (per connection, not `ALTER ROLE`; ADR 0112 security C4). Keep the default unless a reviewed reason exists; verify with `SHOW idle_in_transaction_session_timeout` through the application's own connection path (a pooler that drops startup parameters silently removes it). See `docs/runbooks/production-startup-database-config.md`. Not a secret. |
| `DatabaseConnTimeout` | *(not env-configurable — hardcoded `5s` in `Load()`)* | n/a | `5s` | No action today; if this needs to be tunable in production, that's a small, additive config change, not something to work around by editing the constant per-environment. |

## Environment identity

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `Environment` | `APP_ENV` | No (defaults, but treat as required in practice) | `development` | **Set to exactly `production`** (case-sensitive, exact string — `config.go`'s own comment and `db.VerifyRuntimeRoleInProduction`'s doc comment both call this out) in the real production environment, and ONLY there. This field gates every deliberate, reviewed exception to "environments are otherwise identically configured" (`config.go`'s own doc comment on `Environment`), and there are now four: (1) the fail-closed runtime-role-ownership check (`db.VerifyRuntimeRoleInProduction`); (2) `CasinoPlaySimulationEnabled` (Stage 7); (3) `PaymentsMockSettlementEnabled` (Stage 9.3); (4) `AccountActivationTestSupportEnabled` (Stage 9.3/9.4). **Stage 9.4 closed the "(2)/(3)/(4) fail OPEN on a typo" gap two independent Stage 9.3 security reviews flagged** (previously `cmd/platform-api/main.go` computed each as bare `cfg.Environment != "production"`, and `APP_ENV` was neither validated nor required, so `Production`, `prod`, `production ` (trailing space), or an unset variable all silently registered the three test-support routes). `Load()` now: (a) rejects any EXPLICITLY set `APP_ENV` value outside the exact closed set `development`/`staging`/`production` — a typo now hard-fails startup instead of silently defaulting to "not production" (leaving it UNSET is unchanged and still defaults to `development`, for local/CI convenience only — see the `TEST_SUPPORT_ENDPOINTS_ENABLED` row below for why an unset/wrong `APP_ENV` alone can no longer register a test-support route either way); (b) fails startup outright if `APP_ENV=production` AND `TEST_SUPPORT_ENDPOINTS_ENABLED=true` are set together — a loud, hard failure rather than a silent correction, since that combination is an operator directly attempting something unsafe. Not a secret, but the single most load-bearing string in this table — verify it from the running process's own startup log (`starting platform-api environment=...`), never from the deployment template alone. |
| `TestSupportEndpointsEnabled` | `TEST_SUPPORT_ENDPOINTS_ENABLED` | No | `false` | **Stage 9.4's second, independent gate.** `cmd/platform-api/main.go` now requires BOTH `cfg.Environment != "production"` AND this field before any of `CasinoPlaySimulationEnabled`/`PaymentsMockSettlementEnabled`/`AccountActivationTestSupportEnabled` register their routes (`POST /v1/casino/.../simulate-*`, `POST /v1/me/deposits/{id}/simulate-callback`, and the token-in-response-body behavior of `POST /v1/me/email-verification/request` — see that route's own OpenAPI description; the old separate `GET /v1/me/email-verification/dev-token` route no longer exists at all). Two independent mistakes — a wrong `APP_ENV` value AND this flag set true — are now required before any of these player-triggerable mock-money/self-signed-outcome/credential-adjacent surfaces can ever reach a real deployment. Not a secret. **Leave unset (`false`) in production, always** — `Load()` hard-fails startup if this is `true` while `APP_ENV=production` (see above), so getting this wrong in real production is caught at container start, not silently. **Set to `true` in staging only** (`deploy/aws/environments/staging`'s Terraform sets this explicitly) — the acceptance-test flows staging runs depend on these routes existing. |

| `MockGeolocationCountry` | `MOCK_GEOLOCATION_COUNTRY` | No | unset (location evidence UNKNOWN) | **A8 operating-market gate.** An uppercase ISO-3166-1 alpha-2 code the MOCK static Locator reports for every request so a development/staging stack can pass `internal/marketgate` without a geolocation vendor. Honoured only when mock components are enabled (`TEST_SUPPORT_ENDPOINTS_ENABLED=true`, non-production); `Load()` hard-fails startup if it is set under `APP_ENV=production`. **Leave unset in production, always.** With it unset the gate has no location evidence and REFUSES every registration, deposit and casino launch (fail closed) - a staging stack that relies on those flows must set it AND must have operating-market configuration rows for its brand (a brand with none is refused). The static Locator is also registered with the synthetic-component startup guard (`geolocation/locator`), so a production process cannot start on it: production needs a real, production-eligible Locator (PROVIDER DEPENDENT, not built). Not a secret. |
| `CasinoSessionMaxLifetime` | `CASINO_SESSION_MAX_LIFETIME` | No | `4h` | **A8 SR8-2.** Absolute maximum age (a Go duration, bounded 1m..72h; anything else fails startup) of a CONSUMED casino launch session that may still take NEW bets. Past it a bet is refused and the player must pass the evidence-gated launch again; this is the only bound on "the player travelled after launch" until a per-bet vendor geolocation signal exists (PROVIDER DEPENDENT). Wins, rollbacks and stake returns of exposure that already exists are NEVER blocked by it. Do not raise it past the longest legitimate play session you are willing to leave un-re-checked. Not a secret. |

### Database environment anchor and `app.environment` (ADR 0115 S1, migration 0135)

- Every connection of the `platform-api` pool now carries `app.environment = cfg.GuardEnvironment()` in its startup packet
  (`config.DatabaseRuntimeParams`; an unset or non-explicit `APP_ENV` is `production`; a `DATABASE_URL` value of
  `app.environment` is discarded, an `options` value naming it is refused). No `ALTER ROLE`, no role change.
- The setting is NOT a security control on its own: any session can `set_config()` it. SYNTHETIC-NOT-LEGAL RG configuration
  is admitted only when the owner-written `database_environment` anchor ALSO says `development` / `test`. **No anchor row =
  production.** **REQUIRED provisioning step (security review of ADR 0115 S1):** immediately after `migrate up` reaches
  0135 on a production database, as the migration-owner role, run once
  `INSERT INTO database_environment (environment) VALUES ('production');` - the row is write-once (the runtime role can
  never write it; the owner cannot change it while the guard trigger is enabled), so no later mistake can turn the
  database into a "test" one. Verify: `SELECT environment, recorded_at, recorded_by FROM database_environment;`.
- Startup check (launch gate, `IMPLEMENTED`): `platform-api` with `GuardEnvironment() = production` REFUSES to start when
  the anchor says `development` / `test` (`db.ErrEnvironmentAnchorMismatch`), when NO anchor row is recorded
  (`db.ErrEnvironmentAnchorUnrecorded`; the error text names the provisioning INSERT above), and when the anchor cannot be
  read (fail closed). A recorded `production` starts. Non-production guards (development, staging, test) check nothing, so
  dev / CI / staging need no anchor row. A refused start means the provisioning step below was skipped: run it, do not
  work around the check.
- **Where it fits in the deploy checklist:** first production deploy and every new production database (including a
  restore into a fresh database), strictly between the `cmd/migrate up` step (Pre-launch verification item 5) and the first
  `platform-api` start (item 1 onward): (a) `migrate up` reaches 0135; (b) the migration-owner role (the existing
  migration credential, no new role, no password change) runs the one statement above; (c) verify with the SELECT above
  (`environment = production`); (d) only then start `platform-api`. The statement is idempotent-safe to repeat only in the
  sense that a second INSERT fails on the primary key: the row is write-once, so there is nothing to undo or retry.
- A database restored from a test / development backup carries that backup's anchor row: never restore one into production
  (`restore-from-backup.md`).

## HTTP

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `HTTPAddr` | `HTTP_ADDR` | No | `:8080` | Usually left as-is; the orchestrator/load balancer routes to whatever port the container exposes. Not a secret. |

## JWT / authentication

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `JWTActiveKID` | `JWT_ACTIVE_KID` | No | `k1` | Any stable identifier; used only to select the active signing key. Not a secret. |
| `JWTSigningSecret` | `JWT_SIGNING_SECRET` | Yes — startup fails without it, and fails if under 32 characters | none | **Secret.** Startup refuses it when it equals (as text, or as its base64 decoding) any key of another platform key family (ADR 0119 S1 registry). Generate with a real CSPRNG (e.g. `openssl rand -base64 48`), at least 32 characters, per environment — never reused between dev/staging/production, and never the value in `.env.example` or any doc. This is the Stage 2 foundation mechanism (HMAC shared secret); `internal/config`'s own doc comment marks production authentication as PROVIDER DEPENDENT — evaluate moving to asymmetric signing backed by a KMS-managed key or a real identity provider before go-live (`docs/security/security-architecture.md`'s production authentication evaluation) rather than treating this as the permanent production mechanism. |
| `JWTPreviousKID` | `JWT_PREVIOUS_KID` | No | `k0` | Only meaningful once `JWT_PREVIOUS_SECRET` is also set (key rotation in progress). Not a secret. |
| `JWTPreviousSecret` | `JWT_PREVIOUS_SECRET` | No, but same 32-char minimum if set; its KID must differ from `JWTActiveKID`, and the secret itself must differ from `JWT_SIGNING_SECRET` (refused at startup since ADR 0119 S1: the two JWT secrets are separate families of the platform key-family registry) | none | **Secret when set.** Set this to the OUTGOING signing secret during a rotation window (so already-issued, not-yet-expired tokens still verify), then remove it once the longest-lived outstanding token from before the rotation has expired (`RefreshTokenTTL`, below, bounds how long that window needs to be kept open). |
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

### Metrics, alert channels and alert retention (A9; `alert-delivery-channels.md`)

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `MetricsAddr` | `METRICS_ADDR` | No | unset (no listener) | Not a secret. Prometheus text on a DEDICATED listener, no authentication. Production: bind `127.0.0.1:<port>` (agent/sidecar scrape) or a private interface restricted to the collector at the network layer; a non-loopback bind is refused in every environment except an explicit `APP_ENV=development` (staging included) unless `METRICS_ALLOW_NON_LOOPBACK=true`. Must differ from `HTTP_ADDR`'s port; never behind the public load balancer. |
| `MetricsAllowNonLoopback` | `METRICS_ALLOW_NON_LOOPBACK` | No | `false` | Set `true` only after the security group / network policy limits the metrics port to the collector. |
| `AlertEmailRecipients` | `ALERT_EMAIL_RECIPIENTS` | No | none | Bare addresses, at most 10. Production refuses the MOCK email provider (MOCK-ADAPTER-PROD-1), so this works in production only once a real email provider adapter exists (PROVIDER DEPENDENT). Interim owner recipient: `alert-delivery-channels.md` section 1. |
| `AlertWebhookURL` | `ALERT_WEBHOOK_URL` | No | none | `https` only, public destination only (SSRF guard). Takes effect only with an enabled route (SR-7 four-eyes). The payload carries pseudonymous staff/withdrawal/attempt identifiers: a DPA with the receiver's vendor is required before production use (`alert-delivery-channels.md` section 4). |
| `AlertWebhookAllowHosts` | `ALERT_WEBHOOK_ALLOW_HOSTS` | No | none | Comma-separated single-label host names the webhook URL may use (for example an in-cluster service). Leave unset: a dot-less host such as `metadata` is refused. |
| `AlertWebhookAllowNAT64` | `ALERT_WEBHOOK_ALLOW_NAT64` | No | `false` | `true` only on an IPv6-only network whose webhook receiver is addressed through `64:ff9b::/96`; the embedded IPv4 must still be public. |
| `AlertWebhookSecretRef` | `ALERT_WEBHOOK_SECRET_REF` | With the URL | none | A secret-store REFERENCE (`awssm://...`; `devfile://` is development-only), never the secret; the secret is at least 32 bytes, provisioned by a human in the secret store. |
| `AlertRetention` | `ALERT_RETENTION_DAYS` | No | unset (no purge) | Minimum 7. Set only after the per-jurisdiction audit-retention horizon is decided (legal); unset leaves ALERT-RETENTION-1 open and logs a startup warning. |
| `AlertRetentionInterval` | `ALERT_RETENTION_INTERVAL_HOURS` | No | `24` | Not a secret. |

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
| `PaymentsSweepInterval` | `PAYMENTS_SWEEP_INTERVAL_SECONDS` | `15` | Payments sweeper tick (deposit poll, payout dispatch/resend/resolve; ADR 0095 §7.3 RECOMMENDATION, §37). Must be positive if set. Not a secret. Non-active tenants are resolution-only. No alert delivery exists (ALERT-DELIVERY-1). |
| `KYCOutboxInterval` | `KYC_OUTBOX_INTERVAL_SECONDS` | `5` | KYC submission outbox worker tick (ADR 0106, ADR 0095 section 38; engineering default, not a regulatory deadline: any jurisdiction KYC deadline is HQ-E1-3, an OPEN question). Must be positive if set. Not a secret. The worker starts only when a KYC orchestrator and a KYC outbound credential resolver are both configured; otherwise nothing is ever sent (logged once at startup). No alert delivery exists (ALERT-DELIVERY-1). `PROVIDER DEPENDENT`: MOCK KYC provider only. |
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

- **Mock provider webhook credentials** (payments/KYC/casino) are Stage
  10.2 (ADR 0091) per-process `crypto/rand` values
  (`internal/webhookauth.NewMockMaster`), never a compile-time constant,
  never config, and never recoverable from outside the process that
  generated them. `cmd/platform-api/wiring.go`'s `mockProviderWiring`
  (derived solely from `cfg.TestSupportRoutesEnabled()`, ADR 0085) decides
  whether each mock provider's resolver — and, for KYC specifically, the
  webhook ROUTE and the Orchestrator itself — is ever wired into anything
  reachable. In production (or any deployment with test support off): the
  payments/casino webhook routes stay registered but every callback fails
  closed 401 `no_resolver` (no real vendor exists); the KYC webhook route
  is entirely ABSENT (404), and `POST /v1/me/kyc/verifications` is 503,
  since there is no real KYC vendor to fall back to. A real provider
  integration would add its own per-tenant credential fields (see
  `docs/decisions/` for the relevant provider-integration decisions), not
  repurpose these mock values — see KYC-WH-1 and CAS-WH-TENANT-1
  (`docs/plans/stage-10.2-planning/01-webhook-trust-design.md`).
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
   **Then the database environment anchor is recorded** (owner-side
   `INSERT INTO database_environment (environment) VALUES ('production');`,
   see "Database environment anchor" above). `platform-api` refuses to start in
   production without it (`db.ErrEnvironmentAnchorUnrecorded`).
6. `TEST_SUPPORT_ENDPOINTS_ENABLED` is unset (or explicitly `false`) —
   confirm the process actually started at all: `Load()` refuses to
   start if this is `true` at the same time `APP_ENV=production`, so a
   running production instance having reached this checklist step is
   itself partial evidence, but verify the deployment template directly
   too (never assume the fail-closed check will always catch it before
   `APP_ENV` itself is ever fixed to be correct).
7. **PROV-OUTBOUND-CRED-1 payments non-production behaviour change**
   (RV-PRH-I1 kill-switch phase 2 code review C4, before this branch:
   `paymentsOutboundCredentials()` always returned the MOCK outbound
   resolver, since the only registered payments adapter was always
   `mock-payments`). It now returns the MOCK resolver only when
   `TestSupportEndpointsEnabled` is also true (the same kind-split
   pattern casino and KYC already use). This is only reachable in a
   non-production deployment (`TEST_SUPPORT_ENDPOINTS_ENABLED=false`,
   `APP_ENV!=production`, e.g. a staging-like environment with the flag
   turned off) where a synthetic `mock-payments` adapter is still the
   only one registered:
   - If the real `providercred` subsystem is not configured either,
     deposits/withdrawal submit/poll all return `503` ("not enabled") -
     no behaviour change from before.
   - If the real subsystem IS configured, every payments call against
     `mock-payments` now becomes `NotSent` (T5) instead of succeeding:
     deposit intents/attempts commit and then revert to
     `created`/`pending`, and a payout claim commits
     `approved`→`submitted` and parks in `created`. `cmd/platform-api`
     constructs no `Sweeper` today, so nothing currently retries these
     rows automatically.
   - No currently-deployed environment is affected: staging sets
     `test_support_endpoints_enabled = true`
     (`deploy/aws/environments/staging`), and production never wires the
     synthetic adapter kind-split's MOCK half at all
     (`MOCK-ADAPTER-PROD-1`). Record this explicitly if a future
     environment sets `TEST_SUPPORT_ENDPOINTS_ENABLED=false` while still
     relying on the MOCK payments adapter for anything.
8. **Zero unlinked approve-capable staff accounts** (ADR 0119 S5 + S6 security re-review NR-2; residual S56-R2).
   Migration 0139 refuses CREATING an approve-capable account without a Person (SF014) but does not backfill legacy
   rows, and a person link by an actor without a Person is refused (NR-2). Before launch, as the table owner, this
   query must return **0**; otherwise link (through a Person-linked platform admin) or offboard each listed account
   first, and re-run:

   ```sql
   SELECT id, tenant_id, email, role, status FROM staff_users
    WHERE person_id IS NULL
      AND role IN ('platform_admin', 'tenant_admin', 'finance', 'compliance')
      AND status <> 'offboarded';
   ```
9. **Backup, restore and PITR are real, not assumed** (A12, ADR 0122; section "Backup, restore and point-in-time recovery" below).
   Before real money: automated backups with PITR are ON with a recorded retention, `latest_restorable_time` is recent, a PITR
   restore to a timestamp and a logical restore have each been rehearsed **on the managed topology** with `deploy/backup/verify.sh`
   PASS, the dedicated `igaming_backup` role exists with the attributes of ADR 0122 section 3, and a restore-time
   `database_environment` anchor step is in the restore runbook. None of this is proven on AWS yet
   (`docs/runbooks/aws-staging-deploy-plan.md`).

## Backup, restore and point-in-time recovery (A12, ADR 0122)

Operational detail: `docs/runbooks/backup-restore-dr.md`. Configuration items an operator must set or confirm:

| Item | Where | Production action |
|---|---|---|
| Automated backups + PITR | RDS `backup_retention_period` (Terraform `db_backup_retention_days`) | Set to the regulatory retention (staging: 14 days). `0` disables PITR and is rejected by the module. Confirm `latest_restorable_time` is within minutes |
| Deletion protection, final snapshot | `db_deletion_protection=true`, `db_skip_final_snapshot=false`, unique `db_final_snapshot_identifier` | Required in production; teardown is deliberate and multi-step |
| Storage / snapshot encryption | `db_kms_key_arn` (reference to an existing CMK) or the AWS-managed key | Production: a CMK with an explicit key policy (not designed yet) |
| Audit logging | parameter group `log_connections`, `log_disconnections`, pgaudit; `db_log_exports` | On; logs exported to CloudWatch with retention; alert on `igaming_backup` use outside the runner's schedule |
| `igaming_backup` role | `deploy/backup/init-backup-role.sql`, run once by the master user | `NOSUPERUSER BYPASSRLS`, SELECT-only, `CONNECTION LIMIT 2`, `VALID UNTIL` (time-boxed), no role membership. **Never** reuse `igaming` or `igaming_runtime` for dumps. Unverified on RDS: that the master may create a `BYPASSRLS` role |
| Backup credential | `backup_runner_secret_arn` (reference only) | The secret holds the `igaming_backup` password; Terraform never sees it; the runner materialises a 0600 passfile (`BACKUP_PGPASSFILE`), never an env password or URL |
| Backup artefact encryption | `BACKUP_ENCRYPT_AGE_RECIPIENT` / `BACKUP_ENCRYPT_GPG_RECIPIENT` (public references) | Required (the scripts refuse otherwise); private key in the restorer's secret store/KMS; no key material in git |
| Target guard | `BACKUP_PGHOST`/`BACKUP_PGPORT`, `BACKUP_ALLOW_REMOTE_TARGET=<host>` | Scripts refuse the shared 127.0.0.1:5432 cluster and any remote host not acknowledged per invocation |
| Restore anchor | `INSERT INTO database_environment (environment) VALUES ('production')` | A restore into a NEW database is **unrecorded** (or carries the source's anchor): re-run the anchor step before starting `platform-api` (see "Database environment anchor" above) |
| Not in any database backup | JWT secrets, staff MFA keys, payout-instrument keys, provider credentials in the secret store, role passwords | Restored from the secret store, never from a dump; `actor_proof_keys` signing keys ARE in the database and therefore in every dump (encrypt it) |

## Webhook body-read deadline (ADR 0097 A5, `BodyReadTimeout`)

`WebhookAdmission.BodyReadTimeout` (default 10s, not env-configurable today) is A5's per-request read
deadline. Since PRH-2 I-wire (`statusRecorder.Unwrap`) it is actually enforced, and on webhook routes the
same deadline cancels the request context that bounds the handler's database work. Any change to it must
keep it comfortably above the worst-case webhook processing time (verification, domain transaction and
the post-response alert work are measured separately, but a slow database counts); validation only
requires it to be positive. If it fires, the callback rolls back and the provider redelivers.

## Signed actor proof (PRH-2 R5, ADR 0110)

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `ActorProofKeys` | `ACTOR_PROOF_KEYS` | **Yes in production** - `cmd/platform-api` refuses to start without it (`actorproof.VerifyConfiguredInProduction`) | none (an absent key leaves K2/K3 governed writes failing closed at the database) | **Secret.** `kid:base64(secret)`, comma-separated, each secret at least 32 bytes decoded, generated from a real secret store; list two keys only during a rotation. The SAME secret must be provisioned by the migration/owner role into `actor_proof_keys` (never by `igaming_runtime`). Must differ from the JWT secrets. Never rendered by `config`. Runbook: `docs/runbooks/operational-runbooks.md` section 15. |
| `ActorProofActiveKID` | `ACTOR_PROOF_ACTIVE_KID` | **Yes in production** (with the keys) | none | The kid the application signs with. At startup in production the database must hold this kid as an `active` key or the process refuses to start. Not a secret. |

## Staff MFA key families and staff session bounds (ADR 0119 S1)

Status: the library and the configuration validation are `IMPLEMENTED` (ADR 0119 slice S1). Since slice S3 (staff
login with MFA) `cmd/platform-api` and `cmd/seed-admin` build the keys through `staffmfa.LoadKeys`, which runs
`staffmfa.VerifyStartup`: **both refuse to start without the two families in every environment** (APP_ENV unset
included). Runbooks: `docs/runbooks/staff-mfa-keys.md` (keys) and `docs/runbooks/staff-mfa-s3-cutover.md` (the S3
deploy order, the staff-session cutover and the break-glass path until S6).

| Field | Env var | Required | Dev default | Production action |
|---|---|---|---|---|
| `StaffMFAFactorKeys` | `STAFF_MFA_FACTOR_KEYS` | **Yes, in every environment** - `config.Load` refuses `APP_ENV=production` without it, and since S3 `staffmfa.LoadKeys` / `VerifyStartup` refuses to start without it everywhere. | none | **Secret.** `kid:base64(secret)`, comma-separated, each at least 32 bytes decoded. The AES-256-GCM factor-secret key. Rotates by re-enrolment only. Must differ from every other platform key family (the registry checks every pair). Never in the database, never rendered. |
| `StaffMFAFactorActiveKID` | `STAFF_MFA_FACTOR_ACTIVE_KID` | **Yes in production** (with the keys) | none | Not a secret. Must name a kid of `STAFF_MFA_FACTOR_KEYS`. |
| `StaffMFAMACKeys` | `STAFF_MFA_MAC_KEYS` | **Yes in production** | none | **Secret.** Same format. The recovery-code, challenge, enrolment-token and session-assurance HMAC keys. A retired kid stays listed (verify-only) until its rows age out (section 5.3). |
| `StaffMFAMACActiveKID` | `STAFF_MFA_MAC_ACTIVE_KID` | **Yes in production** (with the keys) | none | Not a secret. |
| `StaffMFAIssuer` | `STAFF_MFA_ISSUER` | No | `iGaming Platform Back Office` | Platform value shown in authenticator apps; never a brand name. No `:`. |
| `StaffAccessTokenTTL` | `STAFF_ACCESS_TOKEN_TTL_SECONDS` | No | 900 | Bound 60-900. Can only tighten. |
| `StaffRefreshIdleTTL` | `STAFF_REFRESH_IDLE_TTL_SECONDS` | No | 1800 | Bound 300-3600. |
| `StaffSessionAbsoluteTTL` | `STAFF_SESSION_ABSOLUTE_TTL_SECONDS` | No | 43200 | Bound 3600-43200. |
| `StaffStepUpCriticalMaxAge` | `STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS` | No | 300 | Bound 60-300. |
| `StaffStepUpFinancialMaxAge` | `STAFF_STEP_UP_FINANCIAL_MAX_AGE_SECONDS` | No | 600 | Bound 60-600. |

The published `.env.example` values of the two families are development placeholders and are refused unless
`APP_ENV` is explicitly `development`.

## Back-office static host: do not log query strings (A10a, SR-A10a-5)

Emailed staff links carry the one-time token in the URL FRAGMENT (`/#/enrol?token=...&invite=1`), which never reaches
the server. The SPA discards any token in a query string (`/accept-invitation?token=...`), but by then the request has
already reached the host. Deployment checklist item: the static host / CDN / reverse proxy serving the back office
must NOT write query strings to access logs (or must redact `token=`) for `/enrol` and `/accept-invitation`, and must
send no `Referer` containing them. Status: deployment configuration, `NOT IMPLEMENTED` in this repository.

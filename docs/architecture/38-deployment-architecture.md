# 38 — Deployment Architecture

Status: Stage 9 (Production Readiness, Security, Resilience & Launch
Hardening) foundation. Owner: `devops`. Describes the deployment shape
this codebase actually supports **today**, and the minimum additional
shape a first production deployment needs — not the full Blueprint §7
technology baseline (`00-system-overview.md`'s "Technology baseline"
table), which is an aspirational RECOMMENDATION for a scaled platform,
not a Stage 9 requirement. Per `CLAUDE.md`'s prohibition on premature
infrastructure, this document does not introduce Kubernetes, a service
mesh, Kafka, or multi-region topology — none of that has a credible
dev/staging workflow to build on yet. It describes the smallest
production-shaped deployment that closes real, named gaps.

## 1. What exists today

A single Go binary, `platform-api` (`cmd/platform-api/main.go`), stateless
except for its PostgreSQL connection pool:

- Reads all configuration from environment variables
  (`internal/config/config.go`) — no config file, ever (see
  `docs/runbooks/production-configuration-checklist.md` for every field).
- Connects to one PostgreSQL 16 database as the migration-owner role
  (`igaming` in dev/CI convention) in every environment **except**
  production, where `db.VerifyRuntimeRoleInProduction` requires the
  connecting role to be a genuinely non-owning runtime role instead
  (`igaming_runtime` — see `docs/security/runtime-role-separation.md`).
- Serves HTTP on `cfg.HTTPAddr` (`:8080` default) via `net/http`'s stdlib
  mux (`internal/httpserver`), with `/healthz` (liveness — process is up,
  never touches the database) and `/readyz` (readiness — database
  reachable, `internal/db.Pool.HealthCheck`) already implemented and
  wired (`internal/httpserver/health.go`, `server.go`). Neither leaks
  internal error detail: `/readyz`'s failure body is exactly
  `{"status":"unavailable","reason":"database unreachable"}` regardless
  of the underlying database error.
- Emits structured JSON logs (`internal/observability.NewLogger`) and
  OpenTelemetry traces/metrics (`internal/observability.InitTracing`/
  `InitMetrics`), defaulting to a stdout exporter — no external collector
  required yet (`OTEL_EXPORTER=stdout`), an OTLP exporter is a config
  value away (`OTEL_EXPORTER`) once there is a concrete backend to send
  to.
- Runs three in-process scheduler loops (reconciliation, RG enumeration
  sweep, three bonus-engine sweeps — see `cmd/platform-api/main.go`'s
  remainder past the section shown in §2 below) inside the SAME process
  as the HTTP server. There is no separate worker process today. This is
  a genuine current limitation (§4) — every replica currently runs every
  scheduler independently.
- Two separate frontend SPAs (`b2c/`, `backoffice/`), each a static Vite
  build (`npm run build` → `dist/`) with no server-side rendering and no
  runtime dependency on `platform-api` beyond its REST/JWT API — they can
  be deployed as static assets behind any CDN/static host, entirely
  independent of the Go binary's own deployment cadence.
- Schema migrations (`cmd/migrate`) are a separate, deliberately distinct
  deploy-time step, run once per deploy by the migration-owner role
  BEFORE the application version depending on that schema starts
  receiving traffic (§3).

## 2. Minimum production deployment shape

Nothing below requires new infrastructure beyond what a first production
deployment needs regardless of this platform's specifics; it is the
"what does 'deployed' mean" checklist, not new architecture.

1. **Process supervision / orchestration.** `platform-api` must run under
   something that restarts it on crash and routes traffic only to
   instances passing `/readyz` (any of: a container orchestrator's own
   liveness/readiness probes, a process supervisor plus a load balancer
   health check, or a PaaS's native equivalent — this document does not
   mandate a specific one, since none is provisioned yet). Liveness must
   probe `/healthz`, not `/readyz` — probing `/readyz` for liveness would
   make an orchestrator kill and restart a perfectly healthy process
   during a transient database blip it cannot fix by restarting (see
   `health.go`'s own doc comments, already written with this distinction
   in mind).
2. **At least two running replicas** behind a load balancer, for the
   ordinary reason (a single replica is a single point of failure) and
   the specific one this codebase's own rate limiter names
   (`internal/httpserver/ratelimit.go`'s doc comment): with N replicas,
   each enforces its own independent per-IP window, so whoever
   provisions the load balancer must account for `limit × replicas` as
   the effective platform-wide throughput, and MUST set
   `TRUSTED_PROXY_COUNT` to the exact number of its own proxy hops (see
   §4) — leaving it at its safe default of 0 behind a real load balancer
   collapses every client to the balancer's own address.
3. **Migration step runs once, before the new version receives traffic,
   as the migration-owner role.** `go run ./cmd/migrate up` (or the
   compiled equivalent) — never run by the application's own runtime
   credential (`igaming_runtime` cannot perform DDL at all by design; see
   `docs/security/runtime-role-separation.md`). This must be a distinct
   deploy-pipeline step, not something `platform-api` does on its own
   startup — `cmd/platform-api/main.go` never calls `cmd/migrate`, and
   should not, since that would require the runtime credential to hold
   DDL privileges, defeating the entire role split.
4. **The database itself needs a synchronous standby and a rehearsed
   failover procedure** to meet the Blueprint's own ledger RPO/RTO
   targets (`00-system-overview.md`: RPO 0, RTO < 15 min). Nothing in
   this codebase implements or assumes replica topology — this is a
   database-operations decision (which managed Postgres provider, or
   which native streaming-replication setup) outside this document's
   scope to make, but the requirement is real and should not be silently
   dropped once a production database is provisioned.
5. **Secrets (`DATABASE_URL`, `JWT_SIGNING_SECRET`, `JWT_PREVIOUS_SECRET`
   during rotation) come from a secrets manager or cloud KMS-backed
   injection mechanism, never a checked-in file.** `internal/config`
   already only reads environment variables — the remaining action is
   operational (how those environment variables are populated in the
   target environment), not a code change. See
   `docs/runbooks/production-configuration-checklist.md` for the full
   field-by-field treatment.
6. **`APP_ENV=production` must be set precisely and exclusively in the
   real production environment**, because `db.VerifyRuntimeRoleInProduction`
   gates its fail-closed check on that exact string. Setting it
   anywhere else (a staging environment that is not actually production)
   would incorrectly require a runtime-only credential there too; leaving
   it unset in real production would silently skip the safety check
   entirely. This is a one-line but load-bearing operational detail.

## 3. Rolling-deployment coexistence

Because there is one shared PostgreSQL schema and, during any rolling
deploy, two versions of `platform-api` briefly serve traffic
simultaneously against that same schema, every schema change must be
compatible with BOTH the old and the new application version for the
duration of the rollout. This codebase already has real, lived experience
with this exact hazard — not a hypothetical:

- `docs/progress.md` records a concrete case: a ledger idempotency-key
  change was found, on review, to permit a double stake-lock during a
  mixed-version rolling deploy, and was closed with an explicit
  cross-check inside the same transaction rather than by assuming
  instantaneous cutover.
- `docs/governance/task-registry.md`'s `MKT-MIG76-1` note (referenced
  throughout `docs/governance/stage-4i-exit-register.md`) documents this
  codebase's own permitted practice of amending an uncommitted migration
  in place during active development — explicitly NOT a practice that
  extends past a migration actually being deployed anywhere real.
  `PLAT-MIGDRIFT-1` (same document, §9) recorded the known gap that
  `cmd/migrate` tracked only version numbers, not content, so an
  environment that had a pre-amendment version applied would not
  self-detect the drift — **closed in Stage 9.1** by
  `schema_migrations.checksum` (migration `0083`) and the `migrate verify`
  subcommand; see "Migration checksum verification (`migrate verify`) —
  exact scope" below for precisely what it does and does not catch. This
  gap materialized for real, not just hypothetically: `igaming_platform_
  dev`'s `sportsbook_bets` table carried a stale `provider_bet_ref` column
  (predating migration `0081`'s in-place rename to
  `provider_bet_reference`) undetected by `migrate status` until Stage
  9.1's audit found it by direct schema inspection and rebuilt the
  database from the migration chain from scratch (the drift predated
  checksum tracking, so `migrate verify` could not have retroactively
  detected that specific instance either — see the scope section).

### Migration checksum verification (`migrate verify`) — exact scope

`schema_migrations` carries a `checksum` column (SHA-256 hex of the
up-file's raw bytes, recorded at the moment a migration is actually
applied — `internal/db.Pool.MigrateUp`). `go run ./cmd/migrate verify`
(wired into `.github/workflows/ci.yml` immediately after the migration
step) uses it to check two, and only two, things:

1. **Content drift on already-applied migrations.** For every row in
   `schema_migrations`, it recomputes the SHA-256 of the CURRENT on-disk
   `<version>_<name>.up.sql` file and compares it to the checksum recorded
   when that migration was applied. A mismatch means the file was edited
   after this database already applied the earlier content — exactly the
   `provider_bet_ref`/`provider_bet_reference` defect class.
2. **Version-sequence gaps.** The full on-disk migration set (not just
   applied ones) is checked for missing version numbers between the
   lowest and highest present. `internal/db.LoadMigrations` itself
   (called by every `migrate` subcommand, not just `verify`) separately
   rejects two different files declaring the same version number outright
   — true duplicates cannot reach `verify` at all.

**What it explicitly does NOT check** (no fake completion — read this
before treating a clean `migrate verify` as a stronger guarantee than it
is):

- **The corresponding `.down.sql` file.** Only the up-file that was
  actually executed is hashed. An edited down-file is invisible to this
  tool entirely.
- **The live database schema itself.** A checksum match proves the FILE
  is byte-identical to what was applied; it does NOT re-derive or compare
  the actual live schema (columns, constraints, indexes) against what
  that SQL would produce. A schema hand-altered outside the migration
  chain (e.g. a manual `ALTER TABLE` run directly against the database)
  is invisible to `verify` even though the migration file itself is
  untouched.
- **Any migration applied before checksum tracking existed, for any edit
  that happened before its checksum was backfilled.** `MigrateUp`
  automatically backfills a NULL checksum for legacy rows using
  whatever the file looks like AT BACKFILL TIME (there is no earlier
  recorded value to compare against) — this establishes a real baseline
  for every FUTURE edit, but cannot retroactively prove anything about
  edits that already happened before the backfill ran. This is exactly
  why `igaming_platform_dev`'s specific historical drift required a full
  rebuild from the migration chain (Stage 9.1), not a `migrate verify`
  finding — the drift predated the column that would have caught it.
- **Anything about migrations recorded with no on-disk file at all**
  (a deleted migration file) is reported as `missing_file`, a genuine
  failure — but `verify` cannot say what that file used to contain.

**Standing rule for every future migration, restated here as an
enforceable checklist rather than left implicit:**

1. **Additive first, destructive later, as separate migrations.** Adding
   a column, table, or index is safe to run before the new code that
   uses it ships. Dropping a column/table, renaming anything, or changing
   a column's type/nullability in a way the OLD version depends on is
   NOT safe to run in the same deploy as the code that stops using it —
   split into "add the new shape" (deploy N), "migrate code to use it
   exclusively" (deploy N+1, after every old replica has drained), "drop
   the old shape" (deploy N+2 at the earliest).
2. **A new NOT NULL column on an existing table needs a default or a
   backfill step**, not a bare `ADD COLUMN ... NOT NULL` — the OLD
   application version's INSERTs, still running during the rollout,
   don't know the new column exists.
3. **RLS policy changes are schema changes too** — see the actual
   defect this codebase found and fixed (`docs/security/runtime-role-
   separation.md` and the exit register's §2 note on `tenants_read`'s
   drift): a policy amendment applied to a live database without
   verifying it matches the committed migration file is exactly the
   `PLAT-MIGDRIFT-1` risk materializing for real, not a theoretical
   concern.
4. **The migration step (§2 point 3) always runs to completion before
   any new-version replica starts serving traffic**, and old-version
   replicas must keep running, unaffected, against the post-migration
   schema until they are drained — this is the property point 1's
   "additive first" rule exists to protect.

## 4. Known, named current limitations (not fixed by this document)

Recorded here so a future deployment doesn't rediscover them from an
incident instead of a document:

- **Scheduler loops run inside every `platform-api` replica
  independently** (§1) — reconciliation, RG enumeration sweep, and the
  three bonus-engine sweeps all currently run redundantly on every
  replica rather than via a leader-election or a dedicated worker
  process. Each sweep's own implementation is idempotent (this is a
  correctness requirement already met, per `CLAUDE.md`'s ledger rules),
  so redundant execution is wasteful, not unsafe — but it is real,
  unbounded-by-replica-count load against the same database every sweep
  interval. Splitting scheduler loops into a dedicated worker process (or
  adding leader election) is a genuine next step once replica count
  grows past a handful, not before.
- **The per-IP rate limiter (`internal/httpserver/ratelimit.go`) trusts
  `RemoteAddr` by default (`TrustedProxyCount`/`TRUSTED_PROXY_COUNT` = 0)
  and reads `X-Forwarded-For` ONLY when explicitly configured** — closed
  in Stage 9.1 (was `S9.1-LAUNCH-1`). Whoever introduces a load balancer/
  reverse proxy in front of `platform-api` MUST set `TRUSTED_PROXY_COUNT`
  to the EXACT number of proxy hops it controls (never guess high — an
  over-count lets a client's own injected `X-Forwarded-For` entry be
  mistaken for the trusted one); leaving it at the default 0 behind a real
  proxy reproduces the original "per-service, not per-client" degradation
  this bullet used to describe as unconditional. See
  `internal/httpserver/server.go`'s `Deps.TrustedProxyCount` doc comment
  and `docs/security/security-architecture.md`'s Stage 9 section for the
  full trust model. `AUTH_RATE_LIMIT_PER_MINUTE` is likewise now plumbed
  end to end through `internal/config` (was `S9.1-LAUNCH-2`) — see
  `docs/runbooks/production-configuration-checklist.md`.
- **No distributed rate limiting.** Each replica's limiter is
  independent, in-process, in-memory state — restarting a replica resets
  its own counters, and the effective platform-wide limit scales with
  replica count (§2 point 2). This is an accepted, documented tradeoff
  for a first production deployment, not an oversight; moving to a
  shared store (Redis) is future work if abuse patterns actually require
  it. `TRUSTED_PROXY_COUNT`/`AUTH_RATE_LIMIT_PER_MINUTE` (above) make the
  per-replica limit correctly keyed and operator-tunable; they do not
  change this — it is a separate, still-accepted limitation.
- **`PLAT-MIGDRIFT-1`** (§3) — closed in Stage 9.1 by
  `schema_migrations.checksum` and `migrate verify` — see "Migration
  checksum verification (`migrate verify`) — exact scope" in §3 for
  precisely what is (and is not) covered; it is a real, bounded
  improvement, not a claim that every possible drift class is now
  detectable.
- **No multi-region, no read replicas for query offloading, no message
  bus.** All explicitly out of scope per `CLAUDE.md`'s "no premature
  optimization" rule until there is a concrete load or availability
  requirement driving them — the Blueprint §7 technology baseline in
  `00-system-overview.md` names these as a later-stage RECOMMENDATION,
  not a Stage 9 requirement.

## 5. Related documents

`docs/runbooks/production-configuration-checklist.md` (every
`internal/config.Config` field, field by field), `docs/security/
runtime-role-separation.md` (the migration-owner/runtime-role split this
document's §2 point 6 depends on), `docs/governance/stage-4i-exit-
register.md` (`PLAT-ROLESPLIT-1`, `PLAT-MIGDRIFT-1`), `00-system-
overview.md` (the longer-term Blueprint §7 technology baseline this
document deliberately does not build toward yet).

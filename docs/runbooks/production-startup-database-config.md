# Production startup: database session configuration (ADR 0112 slice 4)

Status: `IMPLEMENTED` in `cmd/platform-api` (wiring, config, tests). The deployment side (setting the variable in the
task definition) is a deployment obligation, not done by this repository change.

## What is configured, and where

`platform-api` opens ONE application pool (requests, background loops, the launch executor) with
`db.ConnectWithRuntimeParams(ctx, DATABASE_URL, maxConns, connTimeout, cfg.DatabaseRuntimeParams())`.
`cfg.DatabaseRuntimeParams()` returns the per-CONNECTION session settings sent in the startup packet of every
connection the pool opens:

| Session setting | Source | Default | Bounds | Why |
|---|---|---|---|---|
| `idle_in_transaction_session_timeout` | `DATABASE_IDLE_IN_TRANSACTION_TIMEOUT_SECONDS` (`Config.DatabaseIdleInTxTimeout`) | `30` seconds | 1..3600; `0`, negative, non-integer and over-large values are refused at startup (the application never sets it off). A `DATABASE_URL` value of 0 or above 1 h is refused too (`db.ConnectWithRuntimeParams`); a value inside 1 ms..1 h is honoured | ADR 0112 security C4. A transaction left idle while it holds a tenant or brand status gate key or a subject row lock must not starve a suspension (the fail-closed lever) indefinitely; the server terminates such a session. |

It is a per-connection setting of the application's own pool. Nothing here uses `ALTER ROLE`, `ALTER DATABASE`, a
role change, a password change or any privileged session; the database role is exactly the one in `DATABASE_URL`.
The value is an engineering default, not a policy value. A value already present in `DATABASE_URL` as a runtime
parameter wins (`db.ConnectWithRuntimeParams` does not overwrite it).

Only a closed set of names is accepted by `db.ConnectWithRuntimeParams` (`idle_in_transaction_session_timeout`,
`statement_timeout`, `lock_timeout`); the platform sets only the first. `lock_timeout` is set per transaction by the
launch executor (`SET LOCAL`, default 5 s), not per connection.

## Checklist before a production start

1. `DATABASE_URL` is the non-owning runtime credential (`docs/security/runtime-role-separation.md`);
   `db.VerifyRuntimeRoleInProduction` refuses to start otherwise (ADR 0112 S11).
1a. The `database_environment` anchor is recorded as `production` (owner-side, once, after `migrate up`; see
   `production-configuration-checklist.md`, "Database environment anchor"). `platform-api` refuses to start in production
   when it is unrecorded or says development / test (`db.VerifyEnvironmentAnchor`).
2. `DATABASE_IDLE_IN_TRANSACTION_TIMEOUT_SECONDS` is unset (30 s) or deliberately set; verify on a running task with
   `SHOW idle_in_transaction_session_timeout` from the application's own connection (not a superuser session).
3. A proxy or pooler in front of PostgreSQL must forward startup parameters. One that rejects them breaks startup
   loudly; one that is configured to ignore them (for example PgBouncer's `ignore_startup_parameters`) silently drops
   the setting. Verify with step 2 through the same path the application uses.
4. The actor-proof signing key is configured and its kid is active in the database
   (`actorproof.VerifyConfiguredInProduction`); without an issuer every launch write fails closed with a 500
   (`internal_error`) and writes nothing.
5. The launch routes answer `503 LAUNCH_AUTHORISATION_UNAVAILABLE` only if the service was not wired; `platform-api`
   always wires it (`cmd/platform-api/main.go`), so a 503 there is a build defect.
6. Alert delivery for the P1 alert `launch.suspension_exhausted` is `NOT IMPLEMENTED` (ALERT-DELIVERY-1, OPEN): a row
   is raised, nobody is paged. Operators must watch the `launch_authorisation.suspend_denied` audit rows until a real
   recipient exists.

## What this does NOT cover

- **A session can still disable it.** `idle_in_transaction_session_timeout` is a user-settable GUC: any code or
  person holding a session of the runtime role can `SET idle_in_transaction_session_timeout = 0` for that session.
  The startup parameter is a default for honest code, not a control against a hostile session. Recommended,
  HUMAN-applied only (no agent or script of this repository applies it): (1) a role-level default so a pooler that
  drops startup parameters is still covered, for example `ALTER ROLE igaming_runtime SET
  idle_in_transaction_session_timeout = '30s'` run by the database administrator through the normal change process;
  (2) an alert on lock waits longer than the suspension's `lock_timeout` (pg_stat_activity `wait_event_type = 'Lock'`,
  or the `launch_authorisation.suspend_denied` audit rows with `code = LAUNCH_RETRYABLE_LOCK_TIMEOUT`), so a stuck
  holder is seen by a person.

- The `cmd/migrate` and `cmd/seed-admin` binaries are separate owner-role tools with their own short-lived pools; they
  deliberately do not use the setting.
- It does not bound a long-running statement (`statement_timeout` is not set) or a transaction that is busy rather
  than idle.
- A launch decision recorded through the API is a recorded human decision, not a legal or regulatory authorisation.

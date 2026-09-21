# Operational Runbooks — Stage 9

Concise, actionable procedures for the incidents this platform can
actually have today. Each one names the real code/tooling to use — no
invented tooling, no invented SLAs. Where a step is not yet possible
(no metrics backend, no backup mechanism), that gap is stated rather than
glossed over; see `docs/runbooks/observability-and-alerting.md` and
`docs/runbooks/backup-and-disaster-recovery.md` for the full evidence.

## 1. Deployment

1. Merge to the deploy branch only after CI is green (build, vet, lint,
   unit + integration tests, both frontend test suites).
2. Apply pending migrations first, separately from the app rollout:
   `DATABASE_URL=<prod> go run ./cmd/migrate up`. Never let app startup
   apply migrations implicitly.
3. Roll the new `platform-api` build using the deployment's normal
   rolling-update mechanism (see `docs/architecture/38-deployment-
   architecture.md`). `VerifyRuntimeRoleInProduction` runs on every
   instance's startup and fails closed if the connecting role owns tables
   — a bad role config stops the rollout instead of serving traffic.
4. Watch `/readyz` on new instances before shifting traffic; watch error
   rate/latency on the affected endpoints for at least one full request
   cycle of each mutating flow (deposit, bet, withdrawal) before declaring
   the deploy complete.
5. Roll out both frontends (`b2c`, `backoffice`) as static builds behind
   their own CDN/hosting target; these are independent of the API rollout
   and can be staged before or after it since the API is versioned
   additively (expand → migrate → contract, see migration safety rules in
   `docs/architecture/38-deployment-architecture.md`).

## 2. Rollback

1. **App rollback**: redeploy the previous known-good build. Safe at any
   time because schema changes follow expand→migrate→contract — the
   previous binary must still be able to read/write the current schema.
   If it can't, the migration violated the rule; fix forward instead of
   rolling back schema under live traffic.
2. **Migration rollback**: only for a migration that has not yet been
   relied upon by committed data in a way its `.down.sql` can't undo.
   `DATABASE_URL=<prod> go run ./cmd/migrate down` (steps as needed). Never
   hand-edit `schema_migrations`.
3. After any rollback, re-run the runtime-role adversarial suite and the
   reconciliation scheduler's next cycle before declaring the system
   stable again.

## 3. Financial incident (ledger imbalance / reconciliation mismatch)

1. Reconciliation raises a `reconciliation_mismatches` row — this is a P1
   incident per CLAUDE.md ("any non-zero drift is a P1 incident"), not a
   routine alert.
2. **Do not** manually `UPDATE` any balance or ledger row. Ever.
3. Identify the wallet/tenant from the mismatch record. Recompute its
   projection from source of truth: `internal/wallet`'s projection-rebuild
   path (`RebuildProjectionRow`) reads `ledger_entries` and rewrites only
   the projection, never historical entries.
4. If the ledger entries themselves look wrong (not just the projection),
   this is not a rebuild situation — escalate to `ledger-finance` review
   before taking any action. A correction is always a new compensating
   entry, never an edit or deletion of history.
5. Root-cause before closing: a lock-ordering gap, a missed idempotency
   key collision, or a provider double-callback are the known historical
   causes in this codebase (see `LOCK-1`..`LOCK-4` in the Stage 9 report).
6. Write the incident and its resolution to `reconciliation_mismatches`'
   investigation columns (mutable by design — see migration 0082) and to
   the audit log if any compensating entry was posted.

## 4. Provider outage (casino / sportsbook / KYC)

1. Provider adapters are isolated behind their internal interfaces
   (`CasinoProvider`, `SportsbookProvider`, KYC provider interface) — an
   outage in one vendor must not take down unrelated domains. Confirm this
   isolation held; if it didn't, that's a separate P1 architecture defect.
2. For casino/sportsbook: new launches/bets to the affected provider should
   fail closed (denied, not silently accepted) while existing open
   rounds/bets remain queryable.
3. For KYC: verification requests queue or fail closed; this must never
   silently downgrade to "treat as verified."
4. On provider recovery, the adapter's own reconciliation
   (provider-specific, per `docs/decisions/0080-...`) must run before
   trusting its callbacks again — a provider that was down may replay or
   duplicate callbacks on recovery, which the existing idempotency
   constraint on `(provider_id, provider_tx_id)` must absorb.

## 5. Payment outage (PSP down / cascade exhausted)

1. `internal/payments`' cascade-on-decline should already route around a
   single unhealthy PSP. An outage runbook is only needed when *all*
   configured PSPs for a tenant/currency are down.
2. Confirm via `deposit_intents`/`withdrawal_requests` state: a spike in
   `declined`/`pending` with no `settled` transitions is the signal.
3. Do not manually force a `deposit_intent` to `settled` — there is no
   ledger entry backing that without an actual provider confirmation, and
   doing so would violate "every financial write idempotent via a DB
   constraint tied to a real provider transaction."
4. If a genuine stranded hold results (player charged, no confirmation
   received), follow the existing withdrawal/stranded-hold recovery path
   from Stage 3C rather than improvising a new one.

## 6. Authentication / session outage

1. Symptoms: login/register/refresh endpoints erroring or the rate limiter
   (`internal/httpserver/ratelimit.go`) over-triggering.
2. Check whether the cause is the rate limiter's IP-keying degrading
   behind a load balancer (known launch gate `S9.1-LAUNCH-1` — it keys on
   `RemoteAddr`, not a trusted `X-Forwarded-For`). If so, this presents as
   many distinct users all being throttled together; the fix is
   operational (trust XFF at the edge) not a code rollback.
3. If the JWT signing key or key registry is implicated, do not rotate
   keys ad hoc mid-incident without checking `docs/decisions/` for the
   production signing architecture ADR — a bad rotation can invalidate
   every live session simultaneously.
4. Session/refresh-token state lives in Postgres (`sessions` table), not a
   cache — a Postgres outage is a database outage (§7), not a separate
   auth-specific failure mode.

## 7. Database outage

1. `platform-api` fails closed on DB loss (`/readyz` goes unready); this is
   correct behavior, not a bug to route around.
2. If this is a full primary loss: there is currently **no tested restore
   path** (see `docs/runbooks/backup-and-disaster-recovery.md` — this is a
   known, documented production blocker, not an oversight).
3. If this is connection exhaustion / pool starvation rather than data
   loss: check `internal/db`'s pool configuration and the reconciliation
   scheduler's own connection usage before assuming primary failure.
4. On recovery of any kind, always re-run
   `internal/db/runtime_role_separation_test.go`'s adversarial probes
   against the recovered instance before trusting it — a restored or
   failed-over database is not safe to serve traffic on until role
   separation and RLS are independently reconfirmed, not merely assumed
   carried over.

## 8. Restore verification (once a backup mechanism exists)

See `docs/runbooks/backup-and-disaster-recovery.md`'s "Minimum action
plan" — this step does not exist yet because no backup mechanism has been
built. Do not attempt an improvised restore against production without
that groundwork; an untested restore procedure executed for the first time
during a real incident is itself a major risk.

## 9. Security incident (suspected compromise / breach)

1. Every mutating admin/financial action is already audited
   (`audit_log`, append-only, immutability-triggered) — start there:
   actor, tenant, entity, before/after state, IP, reason code.
2. Revoke sessions for the affected principal(s) via the existing session
   store rather than a blanket key rotation, unless the signing key itself
   is suspected compromised.
3. If a runtime-role escalation is suspected (the exact class
   `PLAT-ROLESPLIT-1` closes structurally): re-run the adversarial probe
   suite immediately; a passing suite is strong evidence the DB-level
   privilege boundary held even if the application layer was compromised.
4. Any confirmed incident touching player PII or funds needs a
   `security`-specialist-led review before the incident is closed, per
   CLAUDE.md's "security-sensitive functionality requires explicit review
   by the `security` specialist."
5. Legal/regulatory notification obligations are a human/legal decision,
   not something to determine or execute autonomously — escalate rather
   than guess at breach-notification requirements.

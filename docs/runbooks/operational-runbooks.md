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
   Since PRH-2 R2 (ADR 0108) `cmd/migrate down` REFUSES to run unless `APP_ENV`
   is explicitly `development` or `staging` (production and an unset `APP_ENV`
   are refused), because a down migration can silently undo a security
   invariant (0116 down re-grants PUBLIC TEMP). In production, fix forward or
   restore; do not work around the guard. Never hand-edit `schema_migrations`.
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
   behind a load balancer because `TRUSTED_PROXY_COUNT` is misconfigured
   (left at its default `0`, or set to the wrong hop count) — closed as a
   code gap in Stage 9.1 (`S9.1-LAUNCH-1`), but still a real operational
   misconfiguration risk. If so, this presents as many distinct users all
   being throttled together; the fix is operational (set
   `TRUSTED_PROXY_COUNT` to the exact number of trusted proxy hops in
   front of this deployment — see `docs/runbooks/production-configuration-
   checklist.md`) not a code rollback.
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
5. Also confirm `TEMPORARY` is still revoked (ADR 0108, migration 0116). **A restore or `CREATE DATABASE` does not
   carry the database ACL, but `schema_migrations` still lists 0116, so `cmd/migrate up` will NOT re-apply it.**
   Run the 0116 up SQL (`migrations/0116_revoke_temp_from_runtime.up.sql`) or the environment's role-provisioning
   script (`deploy/aws/sql/init-runtime-role.rds.sql` on AWS, `deploy/init-app-role.sql` for the dev bootstrap)
   directly as the database OWNER with psql. Then recycle or terminate the `igaming_runtime` backends, because a
   session that created a TEMP table before the revoke keeps it. Terminating needs a role with `pg_signal_backend`
   (on RDS the master user, an rds_superuser); a plain non-superuser owner is DENIED
   (`permission denied to terminate process`) and the shadow stays alive. Either run, as that role,
   `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename='igaming_runtime' AND pid <> pg_backend_pid()`
   or, preferably in ECS, **force a new deployment of the platform-api service** so every runtime task and its
   pooled sessions are replaced. Then confirm no live shadow remains (as the owner; must return NO rows):
   ```sql
   SELECT n.nspname||'.'||c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname ~ '^pg_temp_[0-9]+$' AND c.relowner = 'igaming_runtime'::regrole;
   ``` In production the platform refuses to
   start while the connecting role holds TEMP. Verify:
   ```sql
   -- as igaming_runtime, connected to the app DB:
   SELECT current_database(), current_user,
          has_database_privilege(current_database(),'TEMP')                    AS temp,          -- must be f
          (SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user) AS privileged, -- must be f
          (SELECT count(*) FROM pg_auth_members
            WHERE member=(SELECT oid FROM pg_roles WHERE rolname=current_user)) AS memberships,   -- must be 0
          (SELECT count(*) FROM pg_proc WHERE prosecdef)                        AS secdef_fns;    -- must be 0 (see caveat)
   BEGIN; CREATE TEMP TABLE tsp1_probe(a int); ROLLBACK;                      -- must fail 42501
   -- as the owner:
   SELECT count(*) FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
    WHERE d.datname = current_database() AND a.grantee = 0 AND a.privilege_type = 'TEMPORARY'; -- must be 0
   ```
   Caveat on `secdef_fns`: the count covers all of `pg_proc`, including `pg_catalog`; on RDS the `rds_*` helper
   objects may add SECURITY DEFINER functions. Read the condition as "0, or each one reviewed as not callable by
   `igaming_runtime`". `memberships` must be 0 (production startup also refuses a runtime role that is a member of any role).
   When applying 0116 to a live environment for the first time, apply it before runtime traffic (the AWS deploy
   flow does) or recycle the runtime pools straight afterwards. `cmd/migrate down` is refused in production.

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

## 10. Scoped financial capability grants (ADR 0099, PRH-2 K1)

Applies to the `staff_capability_grant_requests` / `_approvals` /
`_grants` tables and the "platform acting in tenant X" session shape.

**Emergency revoke a grant:**
1. `POST /v1/admin/tenants/{tenantID}/capability-grants/{grantID}/revoke`
   with a `reason_code`, as a `tenant_admin` of that tenant or any
   `platform_admin` — this is the single emergency-stop action (ADR §8.1).
   It is one-way: a revoked grant can never be un-revoked or re-revoked
   with a different reason (both are refused, `CG012`) — a fresh grant
   must be requested and re-approved if access is needed again.
2. Confirm via `GET /v1/admin/tenants/{tenantID}/capability-grants` that
   the grant now shows `revoked_at` set.
3. Check `audit_log` for `capability_grant.revoked` (action name, exact)
   for the actor, reason code and before/after state — every revoke is
   audited by construction (`internal/capability.RevokeGrant`'s own
   caller always writes it in the same transaction).

**A suspicious grant approval (suspected sock-puppet / self-approval):**
1. The DB itself refuses same-Person approval-of-own-request
   structurally (R-1..R-8) — if one nonetheless exists, it means either a
   Person record was manually mis-linked, or a DB-level control was
   bypassed (table-owner/superuser access) — treat as a security incident
   (§9 above), not a routine revoke.
2. Revoke the grant immediately (above), then investigate how the
   approval was recorded — check `staff_capability_grant_approvals`'
   `decided_by`/`decided_by_person_id` against the requester's own
   `person_id`.

**A grantee's status/role changed after a grant was approved:** there is
no automatic revoke yet (STAFF-LIFECYCLE-1 is not implemented — ADR §8.4,
security ruling S-b). Revoke the grant manually as part of any
suspend/role-change/off-boarding procedure until that automation exists.

**G-P1 is not available:** a platform-originated grant request naming a
tenant's own `finance` staff always fails closed (`CG010`, HTTP 409) — this
is by design (ADR §4.1, architect ruling, deferred out of PRH-2), not an
outage. Use G-T instead (the tenant's own `tenant_admin` requests, a
`platform_admin` co-approves).

## 11. Manual adjustment (ADR 0100, PRH-2 K2)

Applies to `ledger_adjustment_requests` / `_approvals`, the financial
approval policy tables, and the `ledger_unlinked_manual_adjustment`
reconciliation kind. A manual adjustment is the ONLY way to correct a
player's cash balance by hand, and it has exactly one shape: the tenant's
`manual_adjustment` house account <-> the player's `player_cash`, one asset,
one amount. Bonus corrections go through the bonus engine; cross-asset moves
are a `ConversionOperation`.

**Nothing is enabled until the platform authors a baseline.** No policy row
is seeded (HD-PRH2-3). With no in-force `platform`-level row for
`ledger_adjustment` (asset-scoped or all-assets) every submission is refused
`MA014` ("manual adjustments are not enabled for this tenant"). To enable:
1. A `platform_admin` proposes `POST /v1/admin/financial-policy-changes`
   (`change_kind=policy`, `operation_kind=ledger_adjustment`, `level=platform`,
   `base_required_approvals` >= 1, optional per-asset threshold). The values
   are a legal/compliance decision - LEGAL / COMPLIANCE REVIEW REQUIRED;
   record the reference in `legal_review_reference`.
2. A DIFFERENT `platform_admin` (different Person) approves
   `POST /v1/admin/financial-policy-changes/{id}/approve` with the
   `content_hash` it reviewed. The policy row is written in that same
   transaction and is effective from approval time (never back-dated).
3. Under the HD-PRH2-8 interim every adjustment needs at least one
   independent approver at every amount (`base_required_approvals` < 1 is
   refused, `MA013`).

**Submitting and approving (four-eyes, always):**
1. The initiator (`finance` staff of the tenant with an in-force
   `ledger_adjustment:initiate` grant, or a `platform_admin` with a G-P2 grant
   for the tenant) submits `POST /v1/admin/tenants/{tid}/manual-adjustments`
   with `wallet_id`, `asset_code`, `direction` (`credit_player`/`debit_player`),
   `amount_minor_units` (integer string), `reason_code`
   (`operational_error_correction`, `compensating_entry`, `goodwill_credit`,
   `external_instruction`), `causation_transaction_id` where required,
   `evidence_ref_hash` (hex SHA-256 of the external evidence reference - never
   the reference itself or any PII) where required, and a note (1-1000 bytes).
2. A different Person with an in-force `ledger_adjustment:approve` grant
   reviews the request and approves with the request's `payload_hash`
   (`POST .../{id}/approve`). The approval that brings the count to the
   required number EXECUTES the posting in the same transaction - there is
   no "approved but not yet posted" state.
3. One reject ends the request. Only the initiator can cancel. Requests
   expire after 24 hours (technical default).
4. Refused by design: the initiator approving; the same Person under another
   principal; the player's own Person as initiator or approver (`MA032`);
   anyone who authored or approved a contributing policy (`MA011`); a stale
   or foreign payload hash (`MA031`).

**`refused_insufficient_funds`:** a debit larger than the player's cash
balance, read under the posting's own projection lock, ends the request
committed and posts nothing (LF-13, no negative balance). Submit a new
request for the correct amount if the correction is still due.

**`open_payment_exposure` (`MA020`, or `refused_at_execution` with that
code):** every CREDIT to a player who has a deposit attempt in
`disputed / multiple_success_for_intent` without a refund tombstone is
refused, whatever the reason code. **There is no override**, deliberately: a
hand credit could pay a captured-but-unposted deposit twice (INV-DEP-1).
Resolve the payment side first (the PSP refund produces the tombstone that
clears it). Debits are unaffected.

**Other `refused_at_execution` codes:** `asset_suspended` (goodwill in an
asset that is not active, not platform-authorized, or not authorized for the
tenant), `tenant_not_active` (goodwill for a suspended/closed tenant),
`compensation_cap_exceeded` (cumulative compensations would exceed the
causation's `player_cash` leg), `policy_disabled`.

**Tightening / loosening policy:**
- A `tenant_admin` may only TIGHTEN its tenant's (or brand's) rows:
  `POST /v1/admin/tenants/{tid}/financial-policy-changes` (`level=tenant` or
  `brand`), approved by a different Person (another `tenant_admin`, or a
  platform principal). A loosening from a tenant principal is refused
  (`MA010`).
- **Unblocking an over-tightened tenant (ADR 0100 §6.7):** a tenant can make
  its own adjustments slow by raising its own requirement. The platform
  remedy is a non-tightening tenant row with a platform REQUESTER **and** a
  platform APPROVER (`POST /v1/admin/tenants/{tid}/financial-policy-changes`
  by one `platform_admin`, approved by another). For `payment_force_resolve`
  on a non-active tenant, tenant/brand rows are ignored automatically (K2-1).
- Every policy change is four-eyes, audited (`financial_policy.change_*`),
  append-only and effective-dated.

**Emergency stop:** revoke the actor's `ledger_adjustment:*` grant (§10).
The next in-transaction read refuses the authority; an execution already
holding `FOR SHARE` on the grant completes first, then the revoke applies.
There is still no staff suspend API (STAFF-LIFECYCLE-1).

**`ledger_unlinked_manual_adjustment` (P1 reconciliation finding):** a
`manual_adjustment` ledger transaction exists after the K2 cutover with no
executed request linked to it - someone posted outside the governed path.
Treat it as a financial incident (§3): identify the writer (application logs,
`audit_log`, DB access logs), stop further access, and correct any economic
effect ONLY with a governed `compensating_entry` request whose causation is
the unlinked transaction. Never edit or delete ledger rows.

**What this is not:** software four-eyes is not a legal or licensing
approval. Real threshold values and the HD-PRH2-8 below-threshold question
are human decisions.

## 12. Payments sweeper stalled, stuck or disabled (ADR 0095 §37, PRH-2 H)

The payments sweeper (`payments.RunSweeperLoop`, started by `platform-api`) polls pending deposits and
dispatches, resends and resolves payouts. If it stops, nothing else does that work: pending deposits stay
pending and in-flight payouts stay `submitting`/`pending`/`ambiguous`. **Nothing pages anyone**
(ALERT-DELIVERY-1 is OPEN): a human must be watching the signals below.

**Signals.** Gauge `payments_sweeper_last_pass_unix_seconds` stops advancing (stalled if older than ~3x
`PAYMENTS_SWEEP_INTERVAL_SECONDS`); counter `payments_sweeper_passes_total` flat; log lines `payments sweeper:
refusing to start`, `... failed to list tenants`, `... claim batch failed`, `... item failed`, `... recovered
from panic`; `payments_sweeper_items_total{result="error"|"panic"}` and
`payments_sweeper_tenant_failures_total` rising; `payments_sweeper_resolution_only_blocks_total` rising means
dispatch is being withheld for a non-active tenant (expected, see step 5).
`payments_sweeper_passes_total` keeps incrementing on passes that could not list tenants (a database outage), so "passes_total flat" stays quiet then: use `payments_sweeper_last_pass_unix_seconds` (advances only after a successful listing) and `payments_sweeper_tenant_failures_total{phase="list"}`.

1. **Not running at all.** Look for `payments sweeper: refusing to start` (a wiring defect: missing payout KYC
   gate or dependency; `platform-api` also refuses to start on `payments sweeper wiring`). Fix the deployment;
   do not work around it by hand-driving attempts.
2. **Running but every item errors.** Read the item error. A DB outage: see §7. Provider calls refused with
   `no outbound credential resolver configured` / credential errors: the per-tenant credential is missing or
   revoked; every call fails closed as NotSent and nothing resolves until it is fixed.
3. **Panics.** A recovered panic leaves that attempt leased; it reappears when its lease expires. A repeating
   panic on one tenant is isolated (other tenants still sweep) but needs an engineering fix, not a restart loop.
4. **Do not** force an attempt to a terminal state or edit `payment_attempts` rows to unstick it. Recovery is
   the sweeper itself (lease expiry, then `QueryStatus`); if a human decision is needed use the governed
   force-resolution path (ADR 0101, K3, not yet implemented) or the reconciliation findings (ADR 0095 §12).
5. **A suspended/closed tenant's `created` deposit or payout is not being sent.** This is by design
   (resolution-only, ADR 0095 §37.3): its pending attempts still resolve by poll, and new dispatch resumes when a
   SUSPENDED tenant is reactivated (after the poll backoff, up to 30 min). An idempotent-manifest `ambiguous` payout of a
   suspended tenant is not resent: it stays `ambiguous` (funds held) until a poll resolves it or the tenant is
   reactivated. **A CLOSED tenant never resumes**: a never-sent `created` payout keeps its withdrawal hold and the
   player's funds stay held with no release path today. That is an OPEN BUSINESS/COMPLIANCE DECISION (§37.5), not
   something to fix by editing rows. A tenant whose new money you want paused should use the kill switch (§5).
   Each deferral bumps `poll_count`, so deferred attempts wait up to the 30 min backoff cap after reactivation.
6. **Disabling deliberately.** There is no off switch for the loop; the control that stops NEW money moving is
   the kill switch (INV-IO-15, ADR 0095 §10): it withholds every new dispatch while polls continue.
7. **Escalated payouts** (`payments.payout_resend_escalated`, `payments.payout_reclaim_denied_by_kyc` audit
   rows; `escalated_at` set): the sweeper never resends a non-idempotent or exhausted attempt. Resolve through the
   staff payout-resolution path (poll / `/resolve`). Releasing the hold of a never-sent attempt (M3,
   `RejectCreated` plus hold release) has NO caller today and the governed force-resolution path (ADR 0101, K3) is
   not implemented, so there is currently no supported release path; escalate to engineering and the business
   owner. Never resend or edit rows by hand.

`PROVIDER DEPENDENT`: all of this is exercised against MOCK adapters only.

---

## 13. KYC outbox stuck (ADR 0106, ADR 0095 section 38, PRH-2 E1)

The KYC submission worker (`kyc.RunOutboxWorkerLoop`, started by `platform-api` only when a KYC orchestrator
and a KYC outbound credential resolver are both configured) is the ONLY caller of the KYC vendor. HTTP create
and upload record a durable `kyc_submission_outbox` row and return; the worker sends later. If the worker
stops, verifications sit `unverified` (the orphan shape) and submitted documents are never sent. Enforcement
is unaffected: it never reads the outbox, and an unsent verification is simply not verified.

**Nothing pages anyone.** Alert notification for `kyc.submission_failed_terminal` is `NOT IMPLEMENTED` (no
route, channel or recipient; ALERT-DELIVERY-1 is OPEN). The alert row is durable but unrouted: a human has to be
watching the signals below. The staff `submission_state` on the compliance case list/verification reads
(`none`, `queued`, `sent`, `failed`, `cancelled`) is the only place a stranded submission becomes visible.

**Signals.** Gauge `kyc_outbox_last_pass_unix_seconds` stops advancing (stalled if older than ~3x
`KYC_OUTBOX_INTERVAL_SECONDS`); the oldest-due and oldest-deferred age gauges (observed at claim time, not a
table scan, so they only move while the worker is claiming) growing (the deferred-age gauge is sampled per pass and reads 0 between deferral re-claims, so alert on it with
`max_over_time`, not on an instantaneous value); counter `kyc_outbox_items_total` by
closed `result` label; log lines `kyc_outbox_*`; open `kyc.submission_failed_terminal` alerts;
`submission_state = failed` in the console; players reporting "stuck in unverified". Metrics carry no tenant,
provider or row labels.

1. **Not running at all.** `platform-api` logs once that the KYC outbox worker is not configured and does NOT
   fail startup when the orchestrator or the outbound credential resolver is nil: nothing is ever sent. Fix the
   deployment; there is no off switch and no manual send.
2. **Running but rows are not due or not claimed.** A retry waits for `next_attempt_at` (exponential backoff,
   computed by the database clock). A `claimed` row whose lease has not expired is not re-claimed; after it
   expires the next pass re-claims it and counts one failed attempt (`lease_expired`). Repeated `lease_expired`
   means phase C keeps failing (database health, section 7).
3. **`not_sent` / `ambiguous` retries.** The vendor or credential is unavailable or the result was ambiguous.
   The verification is never changed by an ambiguous result. After the retry budget the row ends
   `failed_terminal` and raises the alert. Fix the cause; pending rows resume on their own, but a row that has
   already ended `failed_terminal` does NOT resume (item 10). A permanent callback 503 for an unbound vendor
   reference is a known residual (ADR 0095 section 38.3).
4. **`provider_deconfigured` cancels.** The pinned provider is no longer configured for the tenant (a
   credential rotation without overlap, ADR 0093). The row is cancelled, not retried. Rotate with overlap.
   `PROVIDER DEPENDENT`: the rotation-overlap behaviour is not exercised against a real vendor.
5. **`deferred_tenant_inactive`.** The tenant is not active; the row is deferred without consuming a failed
   attempt (`claims` grows, `failed_attempts` does not) and is never cancelled. Whether a suspended or closed
   tenant's submissions should ever be sent or auto-cancelled is HQ-E1-1 (OPEN human/legal question).
6. **`credential_binding_mismatch`.** The resolved credential did not bind to the row's tenant, provider and
   kyc domain. This is an INTEGRITY signal, not an outage: it ends `failed_terminal` at once. Escalate to
   security; do not retry by hand.
7. **`apply_conflict`.** The vendor accepted the call but a deterministic database conflict (SQLSTATE class 22
   or 23) stopped the result being recorded. The row ends `failed_terminal`; the audit row names that a
   vendor-side artefact is not reflected on the platform (`vendor_reference_unbound` for a create,
   `vendor_state_unreflected` for a submit; never the reference value). Escalate to identity-compliance.
8. **`decided_concurrently` cancels.** Staff decided the orphan verification while its create was in flight.
   The staff decision is never overwritten. The vendor may hold a verification bound to no platform row
   (audit `vendor_reference_unbound=true`).
9. **Unbound vendor references** (ambiguous create that ended `failed_terminal`, `decided_concurrently`,
   `apply_conflict` on a create). Whether the vendor-side verification can be found and closed is `PROVIDER
   DEPENDENT` (ADR 0095 section 38.3). Record the case; never bind a reference by hand.
10. **`failed_terminal` is never requeued by hand.** The player re-uploads (the same content may be enqueued
    again) or starts a new verification; documents stranded on a failed orphan stay stored (retention
    HQ-E1-4). A staff requeue does not exist (KYC-OUTBOX-REQUEUE-1, `NOT IMPLEMENTED`; when built it must be
    an audited staff action with a reason code that never writes a verification status). Resolve alerts with
    `alert:manage` and a reason code.

**Never:** lift RLS or a worker fence, disable the outbox trigger, DELETE outbox rows (the table has no DELETE
path and the down migration refuses on any row), or set a verification status to clear a row.

`MOCK` / `PROVIDER DEPENDENT`: all of this is exercised against the MOCK KYC provider only.

## 14. Payment force-resolution M1/M2 (ADR 0101, PRH-2 K3)

Applies to `payment_manual_resolutions` / `_approvals` and the routes under
`/v1/admin/tenants/{tid}/payment-force-resolutions`. It is the ONLY governed way to move a
`disputed` or `ambiguous` payment attempt by hand. `IMPLEMENTED` against the MOCK provider;
behaviour against a real PSP is `PROVIDER DEPENDENT`. A resolution is never a ledger edit: M2
posts through `withdrawal.Complete` / `withdrawal.Fail` and M1 posts nothing.

**Nothing is enabled until the platform authors a baseline.** Like K2 (§11) there is no seeded policy
and no seeded grant. With no in-force `payment_force_resolve` policy a request is refused
`force_resolve_disabled`. The required approvals (the four-eyes number) come from the financial
policy tables and are recounted by the database at `pending -> executing`. The values are a
LEGAL / COMPLIANCE decision; nothing in the code invents them.

**Who:** `finance` staff with an in-force `payment_force_resolve:request` / `:approve` grant for the
tenant, or a `platform_admin` acting under a G-P2 grant for exactly that tenant. `compliance` may read.
`tenant_admin` has no force-resolution permission. The requester and approver must be different
Persons; the beneficiary (the player's own Person) can be neither. A route permission is only the
first gate: a missing or revoked grant is refused by the database.

**Before you request anything (in this order):**
1. **Re-verify with the provider first (T17).** Run the staff `/resolve` (a status poll) for the
   attempt and note the outcome. M2 is for the case where the provider cannot or will not answer.
   Record the hash of your evidence reference (`evidence_ref_hash`, hex SHA-256 of the external
   reference; never the reference itself and never PII). It is mandatory for M2.
2. **A statement source must be registered for the provider.** The standing reconciliation kinds
   below can only fire if a `payment_statement` stream for that provider is running. The service
   REFUSES BOTH `m2_declare_paid` and `m2_declare_not_paid` (`force_resolve_precondition_failed`, at submission
   and again at execution as `refused_at_execution` / `no_statement_source`) when no statement source is
   registered for THAT attempt's provider (ADR 0101 §12.3 and §28; PAY-K3-STATEMENT-SOURCE-WIRING-1). The
   registry is filled at startup from the SAME list the reconciliation scheduler runs, so registered means
   scheduled. Today that list holds only the MOCK source for the `mock-payments` provider: the MOCK source
   unlocks M2 for mock-payments attempts only, and every real provider stays refused until its own real
   statement source is scheduled. A refusal is the correct outcome for an unmonitored provider; escalate to
   engineering, do not look for a workaround. Known residuals: standing findings exist only while the
   provider's stream stays scheduled (removing a source silently ends them); tenants that are not
   `active` are not swept (OPEN HUMAN DECISION H-W1), so for a closed tenant treat M2 as unmonitored;
   deposit-park coverage depends on the PSP statement carrying `merchant_reference` (PROVIDER DEPENDENT).
3. **Check the reason is resolvable.** M2 admits an `ambiguous` payout, or a `disputed` payout with
   `provider_reference_mismatch` / `success_for_never_sent_attempt`, whose withdrawal is still
   `submitted`. NOT resolvable by M1/M2 (the request is refused `force_resolve_reason_not_resolvable`):
   `amount_asset_mismatch`, `callback_amount_asset_mismatch` (PAYOUT-AMOUNT-DISPUTE-1, open),
   `invalid_provider_reference*`, `late_*`, a tombstone. Those go to engineering and the business
   owner; do not try to edit rows.
4. **"Declare paid" needs a provider reference on the attempt.** A reference-less ambiguous payout
   (a timeout before the provider acknowledged) can only be declared NOT paid, with the out-of-band
   basis.

**Kinds:**
- `m1_deposit_evidence`: a `disputed` DEPOSIT. Evidence only. The attempt stays `disputed`, nothing is
  posted, nothing is linked. Funds leave only via a PSP refund. **M1 never clears
  `pay_captured_unposted`**, including a standing `poll_reference_mismatch` finding; it only
  annotates the finding as acknowledged.
- `m2_declare_paid`: the attempt becomes `succeeded` and the withdrawal `Complete` under the reserved
  reference `platform-operator-declared:<resolution-id>`. That namespace is refused at every ingress
  (provider replies, polls, callbacks, statements) and by CHECK constraints; only an executing M2 may
  write it.
- `m2_declare_not_paid`: the attempt becomes `declined` and the withdrawal `Fail` (hold released).
  This is the risky direction: if the provider did pay, the player is credited back AND paid.

**Flow (same shape as K2):** `POST .../payment-force-resolutions` (attempt id, kind, finding code,
basis / context code, `evidence_ref_hash`, reason code, note), then a different Person approves with
`POST .../{id}/approve` using the request's payload hash. The approval that reaches the required
count executes in the same transaction. One reject ends it; only the requester cancels; requests
expire after 24 hours (copied from K2, a technical default). Error bodies are one of the closed
tokens `force_resolve_disabled | _not_permitted | _precondition_failed | _reason_not_resolvable |
_conflict | _expired | _not_found`. Every refusal writes a `payment.manual_resolution_denied` audit
row. If the attempt moved between request and approval, the approval is refused (state and reason
are pinned in the payload) and the resolution ends `refused_at_execution` (committed, audited, nothing posted); submit a new one only after re-verifying.

**After an M2: the three standing kinds** (reconciliation `payment_statement`, surfaced like every
other payment mismatch through `reconciliation.payment_statement_mismatch`):
- `pay_declared_paid_unconfirmed`: declared paid but no confirming statement line in any eligible persisted
  import (payout, succeeded, resolved to the attempt by provider reference, or by merchant reference when no
  other payout holds the line's reference, amount AND asset equal). An import is eligible if it is non-MOCK, or
  if no non-MOCK import exists for the provider. Chase the provider statement.
- `pay_declared_not_paid_but_paid`: declared not paid, but a statement shows the provider paid. The
  player holds the returned funds AND was paid. This is the T14 double-payout risk. Compensate through
  a K2 manual adjustment (a debit of the player; reason `compensating_entry`). The debit's causation MUST be
  that resolution's `withdrawal_failed` transaction, otherwise it does not clear the finding.
- `pay_declared_paid_compensated_but_paid`: a compensation was posted for a declared-paid payout, and
  the provider's statement later shows it paid after all. Same remedy direction; escalate to finance.

One finding per exposure is kept; it persists until the evidence closes it. **Alerts for payout
disputes and T14 are NOT IMPLEMENTED** (PAY-PAYOUT-DISPUTE-ALERT-1 and ALERT-DELIVERY-1 are open).
Nothing pages anyone: staff must read the findings list.

**psp_clearing residual.** An executed declare-paid credits `psp_clearing` by the withdrawn amount at
execution, without a PSP confirmation. Each open `pay_declared_paid_unconfirmed` finding itemises that credit
as an explained difference. If the PSP never paid, the amount stays in `psp_clearing` until a
WITHDRAWAL-REVERSAL-1 posting or a governed correction; it is never netted away. Do not fix it by hand.

**Stranding by MA020.** After a declared-paid M2 the player's cash can be below what an in-flight K2
compensation expects; MA020 (open payment exposure) is a K2 payload refusal; it never applies to the Step B
`withdrawal.Complete` posting itself. The only exemption is a K2 `compensating_entry` CREDIT whose causation is the Step B of an executed declare-paid. Any other K2 adjustment of a player with an
open payment exposure is still refused (`open_payment_exposure_at_execution` is recorded in the audit
row of every executed adjustment).

**Non-active tenants.** For a CLOSED tenant the approver must be a platform acting principal (the
closed-tenant actor scope, ADR 0101 R-5); a tenant principal cannot approve. The hold-release
path for the funds of a closed tenant is OPEN (ADR 0107, design only, NOT IMPLEMENTED). Do not set a
tenant to `closed` while it has withdrawals in a hold-bearing state.

**The free-text `note` (up to 1000 bytes) is stored in the append-only audit log.** Do not put personal data in it (player names, account numbers, document
references): use the closed codes (basis, context, finding, reason) and the `evidence_ref_hash`. A personal-data note cannot be erased from an append-only store (PII retention risk).

**Never:** hand-edit `payment_attempts`, `withdrawal_requests` or ledger rows; resend a payout; use a
`platform-operator-declared:` reference anywhere else; claim a payment was delivered or an alert sent.

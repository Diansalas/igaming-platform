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

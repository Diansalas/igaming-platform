# Automatic AML screening triggers (lic10)

- **Scope:** migration 0181 part B, `internal/aml` (`screening_trigger_service.go`, `screening_worker.go`), audit gap G1.
- **Status:** PARTIALLY IMPLEMENTED. Trigger plumbing, a durable queue, a worker and a withdrawal/payout gate exist; the ONLY provider is the MOCK; a real screening vendor is PROVIDER DEPENDENT. Integration tests are for the owner to run.
- **This is NOT transaction monitoring.** Nothing here evaluates transactions against rules. `R-AML-MONITORING` stays failing and must keep being acknowledged. No threshold or rule is shipped.
- **Not a regulatory approval.** Which events screen, whether withdrawals are gated and the re-screen period are the operator's configuration, with a legal review reference.

## 1. What exists, precisely

| Piece | State |
|---|---|
| Screening provider interface, MOCK provider, orchestrator, `aml_screening_policies` (lists, re-screen interval, backfill grace), results, holds, cases, SAR flag | existed before (ADR 0116 A7), officer-initiated only |
| Governed trigger policy `aml_screening_trigger_policies` (which events trigger; whether withdrawal/payout is gated) | IMPLEMENTED (this work) |
| Events: `registration`, `kyc_approval`, `scheduled` (periodic re-screen and backfill) | IMPLEMENTED |
| Events `first_deposit`, `withdrawal_request` | NOT IMPLEMENTED (a denied withdrawal rolls its own request back, so a request-time enqueue cannot persist; the gate plus the scheduler cover the need; a first-deposit trigger needs the payments transaction) |
| Durable queue `aml_screening_requests` with a database unique key `(tenant_id, idempotency_key)`, retry, terminal failure, alert | IMPLEMENTED |
| Worker over the provider interface | IMPLEMENTED for the MOCK; started by `cmd/platform-api` only outside production |
| Gate: withdrawal and payout held while a required screening is outstanding | IMPLEMENTED (inside `aml_hold_active`, the existing AML gate point of every money boundary) |
| Hold placement by the system | NOT IMPLEMENTED by design: a match waits for an officer (section 4) |
| Transaction monitoring, rules, thresholds | NOT IMPLEMENTED |

## 2. Configuring (platform officer, no code release)

Order matters: licence bound to the tenant, tenant configured for the jurisdiction, **screening policy** (lists, re-screen interval, backfill grace; the routed tighten-only `aml_screening_policy_author`, first version or tightening), then the **trigger policy** (`SetScreeningTriggerPolicy`, platform_acting officer, officer quorum of the owner scope, signed proof, legal review reference). The trigger policy must be for the jurisdiction of the tenant's licence and needs the screening policy to exist. There is no HTTP route or Back Office screen yet (library/service only).

- `events`: which of `registration`, `kyc_approval`, `scheduled` trigger a request. Empty means "nothing triggers" (an explicit version).
- `gate_withdrawal`: when true, withdrawal and payout are HELD (the existing indistinguishable `account_review`) unless the person has a **fresh completed clear** (inside the screening policy's re-screen interval) or is inside the backfill grace (a person registered before the policy took effect). A pending, retrying, failed, error or stale result is not a clear. A system-found match no officer has recorded also holds. A gate with no events is refused (nothing would ever produce a result).
- Deposits, casino and sportsbook stakes are NOT gated by this predicate (their gates are unchanged).
- Warning: enabling `gate_withdrawal` in an environment with no working provider holds every withdrawal after the grace. The worker is not started in production and no real provider exists, so do not enable the gate for a production tenant until a real, contracted provider is wired.

## 3. The worker

Per active tenant and pass: scheduled enqueue (one request per person per re-screen interval), claim (SKIP LOCKED, lease), provider call with no transaction held and the existing fail-closed normalisation (an error, timeout, panic, unknown or inconsistent answer is an `error`, never a clear), then finish with a claim-token compare-and-set. Outcomes: clear is `done`; a match is `needs_officer`; an error is retried with a bounded backoff, then `failed_terminal`. The retry budget and backoff are engineering placeholders, not compliance deadlines.

Alert kind `aml.screening_attention` (p2, platform scope, raised in the same transaction): reason `match_review` or `failed_terminal`; attributes are the request id, the reason and the trigger only. Alert delivery to a person is ALERT-DELIVERY-1 (open); nobody is paged by default.

## 4. A match: what an officer does

1. List the person's requests (`Service.ListScreeningRequests`, proof-bound): a `needs_officer` request shows its idempotency key and the matched list names.
2. Record the result with `Service.RecordScreening` using **that key** (this is the existing officer-attested record: it places the hold with the officer quorum and proof, exactly as for a manual screening). A record under a different key does not resolve the request.
3. Until the record exists, withdrawal and payout stay held (when `gate_withdrawal` is on).

## 5. Limits recorded for security review

- Like the KYC submission outbox, the worker API (`aml_screening_request_claim/finish`) is session-tenant bound and proof-less: a holder of the runtime credential and a tenant session could mark a request done. A system-actor attestation for the worker (ADR 0116 F1) is NOT built.
- The tables are owner-only; the runtime role has EXECUTE on the API functions only.
- The queue is not tenant-visible and no tenant audit row is written for screening results (no tipping off).
- `launchgov.SnapshotVersion` is untouched: `R-AML-SCREENING` still counts only screening policies. A readiness detail for the trigger policy is a residual (proposed text in `docs/plans/lic10-record.md`).

## lic26 additions

- Replacing a stale request (`aml_screening_request_replace`) is refused while a worker's lease on it is still live; wait for the lease to expire. The replacement keeps the ORIGINAL age of the request, so the `aml.screening_gate_trapped` clock does not restart, and it records the officer, the officer's Person and the reason code on the new row.
- The payments sweeper reports persons parked at the payout gate only for an outstanding screening (log line `payouts parked for an outstanding AML screening`, counts only) and raises the platform-routed `aml.screening_gate_trapped` alert (attribute `persons` only) once they have been parked longer than the gating policy's `gate_alert_after_seconds`. If that alert fires and the screening worker logs no `aml screening worker: pass` lines, the worker is down.
- An officer-reviewed match carries forward to an identical re-flag only for four re-screen intervals (capped at 180 days) and only for the same outcome and lists; an upgraded match (for example potential to confirmed) always needs an officer.
- Databases that applied migration 0181 before the lic26 edit must be rebuilt (see `docs/plans/lic26-record.md`).

## lic29 additions

- **Screening policy over HTTP** (`GET` / `POST /v1/admin/tenants/{tenantID}/screening/policy`): the re-screen interval and the backfill grace the withdrawal gate counts from. The platform ships no value for either; a read returns `configured=false` until a platform officer, with the officer quorum, a signed critical-class proof and a legal review reference, authors a version. The route is TIGHTEN-ONLY (`policy_loosening_refused`, 409): a version may not remove a list, lengthen the interval or the grace, or take effect earlier than the latest version. Loosening a screening policy therefore cannot be done over HTTP (it has no four-eyes path); it is an owner decision recorded in `docs/plans/lic29-record.md`.
- **Closed refusal codes**: a refused screening call answers with the same status as before plus a stable code in `check_codes` (for example `screening_worker_heartbeat_missing`, `gate_environment_refused`, `sanctions_clear_refused`, `request_lease_live`, `policy_loosening_refused`). The list is the `AMLScreeningRefusalCode` schema of the OpenAPI document and `aml.RefusalCodes`; the Back Office maps each code to its wording.
- **Trapped-funds monitor** (payments sweeper): each pass reads ONE keyset batch of persons (500, in person id order) per tenant and continues from a cursor on the next pass, so every person is observed once per cycle whatever the tenant size. Persons stuck for want of a screening result (dead-worker signal) and persons waiting for an officer are counted and alerted apart: `aml.screening_gate_trapped` with discriminator `sweeper:tenant:<id>` (screening) or `sweeper_officer:tenant:<id>` (officer queue). The cursor and the failure counter live in the sweeper process: a restart starts a new cycle.
- **Monitor blind**: if the sweeper's read fails three consecutive passes for a tenant it raises `aml.screening_attention` with reason `monitor_failing` (discriminator `monitor:tenant:<id>`): treat it as "the trapped-funds alert cannot see this tenant", check the database and the sweeper log (`screening_read_failures`).

## lic30 additions

- **REBUILD, do not patch.** Migrations 0181 and 0189 were edited IN PLACE again (they are unreleased): any development or staging database that applied them before this change must be REBUILT from the migration set, never rolled back with the down and never patched by hand (`cmd/migrate` verifies checksums and fails loudly).
- **The alert value is a per-batch count.** `aml.screening_gate_trapped` raised by the sweeper carries the number of overdue persons found in the ONE batch (500 persons) read in that pass, not the tenant total; the following passes add theirs (the discriminator is the same, so occurrences accumulate on one alert). Read the alert as "at least this many", and use the sweeper log (`screening_scanned`, `screening_partial`) for the cycle position. Persons whose request failed terminally count with the officer queue (`sweeper_officer:`), because they need an officer.
- **No gating policy does not mean no monitoring.** A person with an unresolved `needs_officer` request stays denied whatever the trigger policy says, so the monitor keeps scanning for them when no policy gates withdrawals. It uses the alert threshold of the LAST gating version, else an engineering fallback of 24 h (not a legal value).
- **Restarts.** The sweeper's cursor is in memory. After a restart the first batch of each tenant starts at a RANDOM person and the cycle then wraps to the beginning, so a sweeper that restarts more often than a cycle still reaches every person with probability growing with each restart.
- **Re-screen interval floor.** Over HTTP the interval must be at least 3600 s (`rescreen_interval_below_floor`) and, where a withdrawal gate exists for the jurisdiction, at least twice its `gate_alert_after_seconds` (`rescreen_interval_below_alert_threshold`). It is a safety guard against a fat-fingered value (a 60 s interval would expire every clear and freeze all withdrawals), not a legal value. Because the route is tighten-only, a value that is too SHORT cannot be undone over HTTP (see the backlog item in `docs/plans/lic30-record.md`): check the number before posting.
- **Effective time.** A screening policy version takes effect between now (2 minutes tolerance) and 30 days ahead (`policy_effective_from_out_of_range`).

## c4 additions (migration 0190): governed loosening of the screening policy

**State: IMPLEMENTED as a mechanism, INERT by default.** No capability row, no cooling period and no value ships. Until a platform
administrator configures the capability for a tenant through the two-person flow below, every loosening request is refused with the
closed code `loosening_not_configured`, exactly as loosening was impossible before. Which cooling period, which loosenings and which
tenants are OWNER DECISIONS (`docs/plans/c4-screening-loosening-record.md`); nothing here is a regulatory position.

- **What counts as loosening** (the classifier of `aml_screening_policy_author`, 0189, read as written): against the LATEST version of the
  tenant and jurisdiction, a list removed, a longer `rescreen_interval_seconds`, or a longer `backfill_grace_seconds`. The effective time is
  not a loosening dimension of the governed path (it is `execution time + effective_offset_seconds`, 0..30 days, always inside the route's
  window). The trigger policy (which events screen, whether a withdrawal gate exists) is NOT covered: see the record, section "Gap".
- **Configure** (kind `aml_screening_loosening_configure`, payload `{tenant_id, enabled, cooling_seconds, evidence_reference}`): enabling or
  shortening the cooling period is WIDENING (two distinct platform_admin Persons, requester never decides, approvals at most 24 h old,
  request expires after 72 h); disabling or lengthening is NARROWING (one platform_admin, executes at once). `cooling_seconds` is 0..255600
  (a request lives 72 h, so a longer period could never be met); 0 is a stated value, not a default. Versions are append-only in
  `aml_screening_loosening_settings` (owner-only, FORCE RLS).
- **Loosen** (kind `aml_screening_policy_loosen`, payload `{tenant_id, jurisdiction_id, lists, rescreen_interval_seconds,
  backfill_grace_seconds, effective_offset_seconds, legal_review_reference, evidence_reference}` with the reason code on the request): two
  Persons. At EVERY step the database re-checks: configured and enabled, tenant configured for the jurisdiction, a previous version exists,
  it IS a loosening (otherwise use the screening policy route), the engineering floors of 0189 (interval at least 3600 s and at least twice
  the gate alert threshold; one-year caps), and at execution the cooling period counted from the FILING time. The new
  `aml_screening_policies` row (scope `platform`, `change_request_id` set) is written in the transaction of the final approval; the audit log
  gets `aml.screening_policy_loosened` with before and after next to the three `licensing_change.*` rows. A refusal at execution rolls the
  whole approval back (approve again after the cooling period; the first approver's approval stays valid for 24 h).
- **Production.** Migration 0190 does not touch the withdrawal gate, the worker heartbeat or `aml_screening_gate_environment_ok()`; a loosening
  never writes a trigger policy, and it is refused (`gate_environment_refused`) where the jurisdiction's latest trigger policy gates
  withdrawals in a database whose environment anchor is not development / staging / test. Do not enable `gate_withdrawal` for a production
  tenant; that stays the rule of the section above.
- **Own-licence tenants are OUT OF SCOPE** (`loosening_not_supported_for_tenant_licensing_model`): the quorum read cannot count the platform grants for them. Nothing was widened; supporting them needs D5 and the quorum read designed first.
- **Tenant staff** never file, approve, see or read these requests (no tenant projection: they are AML configuration).
- **Break-glass**: the unrouted `aml_screening_policy_set` (0154) can still loosen without this flow, but since migration 0191 (d1, D7) it is OWNER-ONLY
  (see "d1 additions" at the end of this runbook); the runtime role can no longer execute it.

### c4 review fixes (same migration 0190, edited in place; rebuild any database that applied the first version)

- **AML officer controls apply.** The requester, every COUNTED approver and the executor of a loosening (and of a configure request that enables loosening
  or shortens its cooling period) must hold an in-force PLATFORM `aml_officer` grant on the tenant (live-eligible: active `platform_admin`, same Person as
  the grant request), and the owner-scope officer quorum (two distinct Persons) must hold. Refusals: `officer_grant_required`, `officer_quorum_not_met`.
  An approval by a non-officer is refused when recorded; an approval by someone who lost standing before execution does not count (the request then cannot
  be completed: cancel and refile). Disabling the capability or lengthening the cooling period stays open to any `platform_admin` (fail-closed direction).
- **The sanctions list is never removed** through this path (`sanctions_list_removal_refused`): a SAFE DEFAULT, not a business policy. Relaxing it is an owner and
  compliance decision (D3) that first needs the screened lists to be recorded per clear and a gate subset check.
- **Every dimension looser or equal** (`loosening_mixes_tightening`): to tighten one setting and loosen another, tighten first on the screening policy form.
- **Effective time**: the later of the payload offset and the tenant's cooling period after EXECUTION, so a stricter counter-version can be authored in between.
- **Capability lock**: validation and execution take the `screening_loosening:<tenant>` advisory lock before reading the capability; a final approval waiting behind
  a disable sees it disabled.

#### Retroactive effect of a loosening (ledger-finance M-1) - READ BEFORE APPROVING ONE

The payout and withdrawal gate evaluates the policy in force at the moment a withdrawal is claimed, not the policy in force when it was requested. Therefore a
loosening that takes effect applies to withdrawals ALREADY WAITING at their next gate claim:
- a LONGER re-screen interval makes clears that had expired fresh again WITHOUT any new screening (persons held only because their clear was stale are released);
- a LONGER backfill grace re-opens a grace that had run out, for persons registered before the gate.
Safe guard built: while the jurisdiction has a gating trigger policy, a longer backfill grace is refused when the previous grace has run out BY THE TIME THE NEW VERSION TAKES EFFECT (at the effective time, i.e. execution + the later of the offset and the cooling period; the table guard repeats the rule)
(`grace_extension_after_expiry_refused`). The retro-release by a longer interval is NOT prevented: it is owner decision D10 (`docs/plans/c4-screening-loosening-record.md`).
Payout decisions do not record the policy id they were made under; recording it is a recorded follow-up, not part of c4.

#### Reconciliation (executed loosen / configure requests against the rows they must have written)

Run as a `platform_admin` session (no owner access and no `NO FORCE` needed; the function reads the RLS tables tenant by tenant and returns ids and kinds only):

```sql
SELECT o_problem, o_kind, o_request_id, o_tenant_id FROM screening_loosening_reconcile();
```

Expected result: NO rows. `executed_request_without_exactly_one_row` = an executed request whose policy / settings row is missing or duplicated;
`policy_row_without_executed_request` and `settings_row_without_executed_request` = a platform-authored row that no executed request of its kind explains.
Any row is an incident for `security` and `ledger-finance`. Any other session (tenant principal, no principal) is refused (`LC030` / `LC001`).

## d1 additions (migrations 0191 and 0192): security closure before merge

### D7: the break-glass setter is OWNER-ONLY (migration 0191)

`aml_screening_policy_set` (0154) authors a screening policy version with NO tighten-only rule and NO second person: it can remove a list (including
`sanctions`), lengthen the re-screen interval or the backfill grace. Since 0191 neither `igaming_runtime` nor PUBLIC can execute it (a call is `42501`);
`deploy/init-app-role.sql` no longer grants it and re-asserts the revoke on every run. The runtime paths to the table are exactly two:
`aml_screening_policy_author` (tighten-only, the HTTP route) and `screening_policy_loosen_apply` (the governed two-Person executor, refused without an executed
request of the transaction). `TestD7_OnlyAllowListedRuntimeFunctionsWriteTheScreeningPolicies` enumerates, from the catalogue, every runtime-executable function that
writes `aml_screening_policies` (directly, dynamically or through a call chain) and pins that list.

**Owner path (documented, not a runtime path).** A database operator holding the table-owner role (the migration role, never the runtime credential) may call the
setter. It still runs `aml_authorize`: the acting staff member needs an in-force platform `aml_officer` grant, the owner-scope officer quorum must hold, and a
signed actor proof (`aml_screening_policy:set`, 60 s, single use) must be attached in the same transaction; the audit and the `created_by` columns record the officer.
A loosening by this path is a recorded owner decision: file the decision record first, then run it, then verify with the reconciliation query above that no
unexplained platform-authored row exists. In tests and in the end-to-end harness the fixture changes ACTOR (`amlfix.OwnerService` over the owner pool), never the
assertion; the end-to-end `aml-fixture` tool uses the routed tighten-only `AuthorScreeningPolicy` (it only writes a first version).

### D4: loosening the TRIGGER policy is a governed two-Person change (migration 0191)

Until 0191, `POST /v1/admin/tenants/{id}/screening/trigger-policy` let ONE officer chain (quorum, critical step-up, signed proof, legal reference) remove events,
switch `gate_withdrawal` OFF or lengthen `gate_alert_after_seconds`. What counts as LOOSENING is defined from the schema of `aml_screening_trigger_policies`,
against the latest version AND the version in force:

| Dimension | Looser | Stricter |
|---|---|---|
| `events` | a previous event is missing | an event is added |
| `gate_withdrawal` | true to false (turning an existing gate OFF) | false to true (enabling a gate: guarded by the heartbeat and the environment anchor, unchanged) |
| `gate_alert_after_seconds` (both versions gate) | longer | shorter |
| `effective_from` | earlier than the version in force (route only) | |

`legal_review_reference` and `reason_code` are provenance, not strictness.

- **The route is tighten-only, enforced by the database.** It calls `aml_screening_trigger_policy_author` (0191): a loosening is refused with 409 and the closed code
  `trigger_policy_loosening_refused`, nothing is written. The 0181 setter `aml_screening_trigger_policy_set` is OWNER-ONLY (runtime and PUBLIC lose EXECUTE; `42501`).
  Owner path: same as D7 above (officer grant, quorum and signed proof are still checked; the Go method is `OwnerSetScreeningTriggerPolicy`, owner pool only).
- **Governed loosening.** Licensing change kind `aml_screening_trigger_policy_loosen` under the SAME inert capability row as `aml_screening_policy_loosen`
  (`aml_screening_loosening_settings`): no row = `loosening_not_configured`. It is always widening (two distinct `platform_admin` Persons; requester and the requester's
  Person never decide; approvals at most 24 h old; request lifetime 72 h; signed proofs on request, approve, execute). The requester, every counted approver and the
  executor hold an in-force platform `aml_officer` grant and the owner-scope officer quorum holds. It must be a loosening in at least one dimension and looser or equal in
  all (`not_a_loosening`, `loosening_mixes_tightening`); a longer alert threshold must leave the screening policy interval at least twice the threshold
  (`rescreen_interval_below_alert_threshold`). **It can never enable a gate** (`trigger_gate_enable_refused`, repeated by the table guard); a version that KEEPS a gate
  (a longer threshold) is refused where the environment anchor is production or unrecorded (`gate_environment_refused`); switching a gate OFF only releases and works in
  every environment. The new version is written by `screening_trigger_loosen_apply` in the transaction of the final approval, `effective_from` = execution + the later of the
  payload offset and the cooling period, scope `platform`, `reason_code` fixed to `governed_loosening`; the audit log gets `aml.screening_trigger_policy_loosened` with
  the previous and the in-force version next to the three `licensing_change.*` rows. `screening_loosening_reconcile()` also covers the new kind.
- **Turning a gate off does not release a held match.** An unresolved system-found match, a sanctions hold, an officer hold, the kill switch and closed accounts hold
  whatever the trigger policy says (`aml_screening_gate_denies` is unchanged in this respect).

### D10: a clear is only as fresh as when it was taken, and only for the lists it covered (migration 0192)

**The defect that was closed.** The gate used to judge a clear by the re-screen interval of the policy in force when it was EVALUATED, and never looked at the lists the clear
covered. A governed loosening (or the break-glass setter) that LENGTHENED the interval therefore made every expired clear fresh again and released withdrawals that were
waiting behind the gate with no new screening, and a clear taken against `pep` only kept clearing a person once `sanctions` had been added to the policy.

**Rules now (database-enforced, only ever stricter).**
- Every clear records its provenance. `aml_screening_requests` (worker clears, matches, auto-reviewed re-flags) gets `screened_policy_id`, `screened_lists`,
  `screened_interval_seconds`, written by `aml_screening_request_finish`, whose last argument is the policy version the worker screened under; the lists and the interval are READ
  FROM THAT POLICY ROW (it must be a version of this tenant and of the request's jurisdiction that is in effect), never taken from the caller. `aml_screening_results`
  (officer results) gets `screened_policy_id` and `screened_interval_seconds` stamped by `aml_screening_record` (the policy in force for the jurisdiction of the request the key resolves,
  else the tenant policy with the SHORTEST interval: conservative) next to the `lists` the officer states. Rows stay append-only; a terminal request stays final.
- `aml_screening_covers()`: a screening covers the person only if (1) its provenance is recorded, (2) its recorded lists are a superset of the lists now in force, (3) no policy version
  since it lacked a list now in force (a list removed and re-added invalidates it), and (4) `now < screening time + LEAST(recorded interval, interval now in force)`. A longer interval
  can never revive an expired clear; a shorter one tightens at once. The gate applies it to all four routes (worker clear, auto-reviewed re-flag, officer clear, officer match whose hold
  was lifted); `aml_screening_enqueue_due` applies the same predicate, so persons whose clear no longer covers them are queued (its idempotency key now also names the policy version, so a
  tightened policy can queue them again inside the same bucket).
- Unchanged: unresolved matches, sanctions holds, officer holds, the kill switch and closed accounts hold as before; the backfill grace and the first-gating baseline are as in 0190; no
  release path was added and no gate is enabled. A withdrawal already held for a stale clear stays held until the person is re-screened. No sanctions false-positive policy and no
  withdrawal release policy is defined here.
- The trapped-funds monitor (`aml_screening_gate_overdue`, `aml_screening_parked_overdue`) reads the gate: a newly stale person is counted from the oldest unresolved request, else from the first
  gating version (so a dead worker shows at once), and leaves the count when the re-screen completes (`TestD10_NewlyStalePersons_...`).

**Effect on existing (dev / staging) data.** Every clear written before 0192 has NO provenance and is not trusted. Under a gating trigger policy every person who relied on such a clear is denied
at the gate (withdrawals wait) until the next scheduled enqueue pass queues a re-screen (`EnqueueBatch` persons per pass) and the worker records a clear with provenance. Nothing is deleted or rewritten
(`TestD10_Upgrade_...` runs this on a database upgraded from 0191 with a legacy clear). No production database has a gate, so there is no production effect. The 0192 down is refused (`AT095`) once any
provenance has been recorded.

**Residual.** The worker names the policy version it screened under and the database validates that it is a version in effect for the request's jurisdiction; it cannot prove the provider was called with
those lists (the worker API is runtime-callable, as the clear itself always was; the heartbeat and the environment anchor are the existing controls). A real provider needs a signed vendor attestation of the
screened lists (PROVIDER DEPENDENT).

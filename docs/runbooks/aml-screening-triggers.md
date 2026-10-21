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

Order matters: licence bound to the tenant, tenant configured for the jurisdiction, **screening policy** (lists, re-screen interval, backfill grace; `aml_screening_policy_set`), then the **trigger policy** (`SetScreeningTriggerPolicy`, platform_acting officer, officer quorum of the owner scope, signed proof, legal review reference). The trigger policy must be for the jurisdiction of the tenant's licence and needs the screening policy to exist. There is no HTTP route or Back Office screen yet (library/service only).

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

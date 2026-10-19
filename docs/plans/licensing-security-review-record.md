# Security review record: licensing / jurisdiction slices 1-3 (0176, 0177, lic3)

- **Reviewer:** `security` specialist, independent review (report only; nothing was fixed).
- **Worktree / branch:** `.claude/worktrees/secrev3`, branch `secrev3`, from `claude/licensing-jurisdiction-completion` at `7d414cc`.
- **Scope:** everything merged since `7ae5d1a` (`git log 7ae5d1a..HEAD --merges`): lic1 (migration 0176, licence lifecycle,
  `licence_versions`, `licence.*` alert kinds, expiry sweep), lic2 (migration 0177, licensing change requests / approvals,
  `licensing_change:*` proof family, governed-write guard LC040, `tenant_jurisdiction_configs` RLS narrowing), lic3 (launch /
  bootstrap status gate, risk authoring refusals, `ResolveLicensingChain`). Records read first: `docs/plans/lic1-record.md`,
  `lic2-maker-checker-record.md`, `lic3-enforcement-record.md`.
- **Method:** code and SQL reading, plus live probes against a private harness database built from this tree (head 0177),
  using the `igaming_runtime` role (adversary A) and the owner role (synthetic fixtures only: two jurisdictions `SRV3A` /
  `SRV3B`, one licence, one tenant; dropped at the end). Targeted test runs (section 6).
- **Not a legal opinion.** Nothing here says whether any licence or market is legally permitted.

## 1. Verdict

**APPROVE WITH CONDITIONS** for merging into the provider-independent line.

Reasons: the new primitive (0177) holds against every adversary-A and adversary-B attack I tried on the tables it governs. Requests,
approvals and the governed writes cannot be forged, replayed, widened after approval, executed twice or moved across tenants
without a valid signed proof, and tenant isolation of the new tables is correct. The two real defects (F-1, F-2) are on the
`licences` **UPDATE** path. That path was single-actor and GUC-authorised before this branch (0077 policy). Merging does not make it
worse. The one exception is `licence_versions` (F-2), which is new, and its defect is availability and audit integrity, not a
widening. Both defects mean the claim "licensing configuration is maker-checker governed" is **not yet true for licences**. That
claim must not be made, and neither G-1 / G-2 nor MKT-DUAL-1 may be labelled IMPLEMENTED for licences, until F-1 and F-2 are fixed.

**Conditions**
1. **C1 (before merge):** the orchestrator's registry / ADR text says that 0177 governs `licences` **INSERT only**. Licence
   UPDATE (status, dates, and the identity columns `jurisdiction_id`, `licence_number`, `permitted_products`, `permitted_markets`)
   stays ungoverned and is tracked as F-1, open. The proposed lic2 text already says "licences (INSERT)". Keep it that way, and do
   not drop the qualifier anywhere.
2. **C2 (before production launch, LAUNCH BLOCKER):** fix F-1. Licence UPDATE needs a signed proof, the identity columns must be
   immutable or governed, and every widening must be governed by a `licensing_change` kind (lic2 section 6 hook).
3. **C3 (before production launch):** fix F-2 (`licence_versions` writable by any platform-GUC session; integer overflow poisoning).
4. **C4:** the tests in section 5 are added by `qa` before F-1 / F-2 are called fixed.

## 2. Findings (severity-ranked)

### F-1 HIGH: `licences` UPDATE is ungoverned. One GUC, no proof, every column (verified live)

- **Where:** `migrations/0077_tenant_licence_registry_rls.up.sql` (`licences_platform_admin_update`: only checks that
  `app.platform_admin_principal_id` is non-null, that `app.tenant_id` is null and that there is no player). There is no staff-row or
  proof check. `migrations/0177_licensing_change_approvals.up.sql:948` attaches the LC040 guard to `licences` **BEFORE INSERT**
  only. `igaming_runtime` holds column UPDATE on every licences column (verified: `jurisdiction_id`, `licensee`, `licence_number`,
  `status`, `issued_at`, `expires_at`, `permitted_*`, `review_due_at`, `evidence_reference`).
- **Exploit (adversary A, stolen runtime credential, reproduced):**
  `BEGIN; SET LOCAL app.platform_admin_principal_id = '<any uuid, need not exist>'; UPDATE licences SET status='active',
  expires_at='2099-12-31', jurisdiction_id='<other jurisdiction>', licence_number='…', permitted_markets='["ZZ"]' WHERE id=…;`
  The UPDATE succeeds. The trigger records a `licence_versions` row with `changed_by` = the made-up uuid.
  - Effect 1: a suspended or expired licence is reinstated and its expiry extended. The market gate then reopens every country its
    governed ceilings already enable, for every tenant bound to it.
  - Effect 2: an existing licence can be **re-pointed** to another jurisdiction, number or market set. That gives the same result
    as `licence_create` plus `tenant_licence_bind` (both need two approvals) with zero approvals. Every bound tenant's licence
    jurisdiction moves: RG floors (`rg.tenantJurisdiction`), KYC policy, payout window, and the `R-RG-CONFIG` readiness inputs.
  - `licensee` is protected for **bound** licences by the composite FK `tenants_licence_matches_model` (verified). For an
    unbound licence it is not protected.
- **Exploit (adversary B, staff insider, by design today):** one `platform_admin` with a fresh second factor can reinstate a
  licence or extend its `expires_at` through `PUT /v1/admin/licences/{id}/status|validity-dates`. That is single actor. Creating
  the same licence needs three Persons. This is recorded as residual G-1 / G-2 in lic1 section 4. I rate it HIGH, not accepted,
  because it makes the 0177 asymmetry incoherent.
- **Fix:**
  - In one migration, add a `BEFORE UPDATE` trigger on `licences` with three parts:
    - (a) refuse any change to `jurisdiction_id`, `licensee`, `licence_number`, `permitted_products`, `permitted_markets` for
      non-owner roles. If such changes are ever needed, make them a `licensing_change` kind. No Go writer updates these columns
      today (`internal/jurisdiction/licence_lifecycle.go:231,258,281,308` only touch status, dates, review and evidence).
    - (b) classify the change in SQL, mirroring `ClassifyStatusChange` / `ClassifyValidityDatesChange`. A widening change
      (toward `active`, `expires_at` later or cleared, `issued_at` earlier or cleared, `review_due_at` postponed or cleared) needs
      an executed `licence_lifecycle_change` request in this txid (new kind row, classify branch, `row_matches` branch for
      `licences` UPDATE).
    - (c) a narrowing or neutral change still needs a signed proof (new operation, for example `licence_lifecycle:write`), so the
      GUC alone is never enough.
  - Route the four lifecycle PUTs through `internal/licensingchange` for widening (202), and keep them single actor plus proof
    for narrowing.
- **Detection today:** a `licence_versions` row per change (but see F-2: forgeable attribution), and the `licence.updated_at`
  touch. There is no alert on a widening licence change.

### F-2 MEDIUM: `licence_versions` is forgeable, and one forged row blocks every later licence UPDATE (verified live)

- **Where:** `migrations/0176_licence_lifecycle_and_expiry_alerts.up.sql:76-81`. The INSERT policy admits any session with a
  platform-admin GUC. `deploy/init-app-role.sql:99` blanket-grants INSERT to `igaming_runtime`, and 0176 does not revoke it. The
  writer at `:100-123` is SECURITY INVOKER, takes `changed_by` from the unverified GUC, and numbers versions with
  `max(version_no)+1` (`:110`).
- **Exploit (A):**
  1. With the GUC set, `INSERT INTO licence_versions (…, version_no, operation, changed_by, status, …)` accepts any content. That
     includes `operation='baseline'`, any `changed_by` (or NULL) and any status or dates, so an auditor-facing history entry can
     be fabricated.
  2. **Poisoning:** insert `version_no = 2147483647` for a licence. Every later UPDATE of that licence then fails with
     `integer out of range`, because the trigger computes `max+1`. That includes the narrowing **suspend / expire** lever
     (reproduced in a savepoint). The row is append-only (`LV001` on UPDATE / DELETE, TRUNCATE denied), so even after the
     intruder is evicted, a status change on that licence needs an owner-level intervention that disables triggers.
     Enforcement by date still works (`EvaluateLicenceValidity`). Recording a suspension does not.
  3. The "gapless" property claimed in lic1 section 5 is broken by any forged `version_no`.
- **Fix:**
  - Revoke INSERT on `licence_versions` from `igaming_runtime` (both in the migration and in `deploy/init-app-role.sql`, like the
    0177 tables).
  - Make `licences_write_version()` `SECURITY DEFINER` (owner, pinned `search_path`). Alternatively keep INSERT but require
    `pg_trigger_depth() > 0` in a guard trigger.
  - Validate `changed_by` against an ACTIVE platform `staff_users` row (or record NULL plus the proof actor once F-1 adds proofs).
  - Use a `bigint` counter, or take a row lock on the licence before `max+1`.
- **Detection:** none automatic. A `version_no` gap or a `baseline` row dated after 0176 is the forensic marker.

### F-3 LOW: the expiry / review sweep fails silently, and default licences are never monitored

- **Where:** `internal/jurisdiction/licence_sweep.go:127` (tenant-list failure is returned and only logged by the loop),
  `:159` (per-tenant read failure: `TenantErrors++` plus a log line), `:190` (alert write failure: log only). The governed creation
  shape pins `issued_at` / `expires_at` NULL (`0177:553`), and the sweep raises nothing for NULL dates.
- **Failure scenarios:**
  - If RLS or the connection fails during the sweep, nobody learns that the safety net did not run. The `reconciliation.sweep_failed`
    kind is deliberately not reused.
  - A licence created through the governed path has no expiry until a single actor sets one, and until then it is never in
    `licence.expiring`.
  - A licence bound to no tenant is never alerted.
- **Impact:** detection only. Runtime enforcement (`EvaluateLicenceValidity` in the market gate, G-12 read-error refusal) is
  unaffected and fails closed.
- **Fix:** add a `licence.sweep_failed` kind (or a closed phase on an existing one) raised when `TenantErrors > 0`, the list fails
  or a raise fails. Also add a readiness or launch check, "licence bound to an active tenant has no `expires_at` recorded" (owner
  decision Q8).

### F-4 LOW: tenant-filed brand / operation policies are not checked for brand ownership at filing

- **Where:** `0177:441-450` and `internal/licensingchange/kinds.go:450-475`. `brand_id` is shape-checked only. The composite FK
  `operating_country_policies_brand_id_tenant_id_fkey` refuses a foreign brand only at **execution**, after platform approvals.
- **Scenario:** a tenant (or a mistaken platform requester) files a change carrying another tenant's brand id. Approvers spend
  their four-eyes effort on it, and the final approval rolls back with `DomainError`. No data leaks: the tenant cannot observe the
  refusal reason of a request it cannot execute. The cost is wasted approver effort and a confusing queue.
- **Fix:** in `precheck` (and the SQL classify branch), require `EXISTS (SELECT 1 FROM brands WHERE id = brand_id AND
  tenant_id = subject)` under the requester's RLS. If it does not exist, answer as not found.

### F-5 LOW: an open identical request can block a platform admin's urgent narrowing change

- **Where:** `0177:635` (`uq_licensing_change_requests_open_payload` on `payload_hash`, which is kind, payload and reason code).
- **Scenario:** a tenant compliance user files the same narrowing change with the same reason code. A platform admin's single-actor
  narrowing (for example disabling a country now) then answers `LICENSING_CHANGE_DUPLICATE_PENDING`. Workarounds exist: approve
  the tenant's request, or use a different reason code. Even so, during an incident this is friction on the fail-closed lever.
- **Fix:** scope the uniqueness by requester scope (or let a platform narrowing supersede, by cancelling the tenant's identical
  open request in the same transaction). At minimum, cover this case in the runbook.

### F-6 LOW: `licence.*` alerts can be forged by any session that sets the dispatcher GUC (by policy reading, not exercised)

- **Where:** `0176:167-211`. Raise is authorised by `app.platform_service_id = 'alert_dispatcher'`, a client-settable GUC. This is
  the same pattern as 0156 (pre-existing class).
- **Scenario (A):** an attacker pre-raises `licence.expiring` with misleading attributes. Attributes are frozen at first raise
  (lic1 residual), so the real sweep's later occurrences inherit the forged attribute set. The result is noise or misdirection,
  not suppression of enforcement.
- **Fix:** platform-wide item (service identities bound to something stronger than a GUC). Not specific to this branch.

### F-7 INFO: single-actor narrowing is an availability lever (documented, accepted interim)

One `platform_admin` can execute `tenant_licence_unbind` (every market of the tenant closes), a ceiling withdrawal, or a
`tenant_jurisdiction_config_version` close (H-3 then refuses activation). This is the fail-closed direction and each change is
proof-bound, step-up critical and audited. It stays open as an availability and insider-DoS question (lic2 section 8 question 3).

### F-8 INFO: launch status gate lock order (no cycle found; not load-tested)

`casino.CheckLaunchStatusGate` (`internal/casino/orchestrator.go`) and bootstrap (`internal/casino/bootstrap.go`, after the session
row `FOR NO KEY UPDATE`) take `tenant_status_gate_key` then `brand_status_gate_key`, SHARED. The exclusive holders are the status
triggers. Each takes exactly one key (`0118:57`, `0121:205` tenant; `0129:91`, `0130:363`, `0133:75` brand), and none of them
locks `casino_launch_sessions` afterwards. So there is no wait-for cycle with bootstrap. A tenant-then-brand cascade has the same
order as the gate. A future path that takes brand X then tenant X would deadlock against the gate. PostgreSQL would detect it and
the launch would fail closed (500). The `ledger-finance` lock-order review requested by lic3 is still owed.

### F-9 INFO: risk authoring refusals are Go-only

`internal/risk/policy_service.go:177-184` refuses deposit and jurisdiction-scoped rules in `CreateRule`. The database still accepts
such rows, for example from a SQL actor with a tenant GUC (pre-existing: such a row bricks that tenant's withdrawals, which is
fail-closed DoS). Existing rows of either type are not surfaced (lic3 residual). `TestOnlyBypassAuthoringRefusals` is set only by
tests (verified: the only production constructor is `internal/httpserver/risk_handlers.go:243`, which does not set it; the static
pin exists).

## 3. What was checked and held (adversary by adversary)

**A: SQL-only actor with the `igaming_runtime` credential (no actor-proof key).** All verified live unless marked (code).
- Direct writes to `tenants.licence_id` (bind / unbind), `tenant_jurisdiction_configs` (platform or tenant scope), `licences`
  INSERT, and `licences` `INSERT … ON CONFLICT DO UPDATE` all fail with LC040.
- `COPY … FROM` fails ("COPY FROM not supported with row-level security").
- DELETE on the governed tables affects 0 rows (no DELETE policy).
- A forged `licensing_change_requests` INSERT with a non-existent principal fails with LC001. With a real principal it needs a
  `licensing_change:request` proof (AP001 / AP002, covered by `TestSQL_MissingOrForgedProof_Refused`).
- `actorproof_private` is unreadable ("permission denied for schema").
- The runtime role has no CREATE on `public`, no TEMP, no superuser or BYPASSRLS, and no role memberships.
- `SET session_replication_role = replica` is refused.
- The owner bypass is `launch_is_table_owner()` = `current_user = owner(tenants)`, so it cannot be spoofed by the runtime role.
- Only two `SECURITY DEFINER` functions are executable by the runtime role and mention governed tables:
  `actor_proof_require` and `aml_screening_policy_set`, which only *reads* `tenant_jurisdiction_configs`. Neither writes a
  governed table.
- **Overloading attack considered:** `licensing_change_row_matches` is called with `TG_TABLE_NAME` of type `name` against a `text`
  parameter. An exact-type overload in `public` would win, but the runtime role cannot create one (no CREATE on `public`).
  Keep it that way.
- Widening after approval is impossible. Content is immutable (`0177:728-734`; column grants limited to the eight state columns),
  and approvals bind `payload_hash` (`TestSQL_PayloadTamperedAfterApproval_Refused`).
- Double execution is impossible: executed to anything else is refused (code).
- A narrowing change that turns into a widening is re-classified at execution (LC024; only `tenant_jurisdiction_config_version`
  is state-dependent, and ceiling / policy classification is payload-only).
- Execution is bound to `executed_txid = txid_current()`, which is 64-bit and epoch-extended. A rolled-back savepoint hides the
  executed state from the guard.
- The verifier delta against 0175 is exactly the `licensing_change:*` block plus one platform-scope list entry (diffed). No other
  operation or scope was loosened, and proofs bind operation, target (request id) and payload hash, so one cannot stand in for
  another.
- The append-only rules of `operating_country_policies` / `licence_country_ceilings` reduce the loose UPDATE match in
  `row_matches` (it checks only OLD keys) to "close a version". The close of an inheriting rung is covered by the deferred
  successor trigger.

**B: staff insider.**
- One person cannot be both requester and approver through two accounts of the same Person: blocked at approval (LC032), at
  rejection (LC022) and at counting time (`licensing_change_fresh_approvals` takes distinct Persons and excludes the requester's
  Person and account).
- Person linkage is append-only (`staff_users_person_id_append_only`). A sock-puppet *new* Person depends on ADR 0119 platform
  staff lifecycle four-eyes (residual R-4).
- Tenant staff never approve, reject or execute. This is enforced by scope in the DB guard, by the proof verifier (approve /
  reject / execute are platform-scope only) and by the static permission.
- A tenant cannot see platform staff ids: `toLicensingChangeJSON` maps every platform actor to a null ref, and the audit
  projection identifies only `payments_kill_switch.*` actors. Metadata of `licensing_change.*` rows has no `TenantPresentation`
  entry, so only base columns are shown.
- Approvals older than 24 h do not count (DB `clock_timestamp()`; Go mirror), and an approver who is no longer active stops
  counting.
- A proof cannot be replayed across tenants or requests, because it binds tenant, target and hash and its nonce is single-use.
- Step-up: request and approve are `critical`, which also covers execute, since only the final approver or a single-actor
  requester executes. The two legacy routes are critical too.
- **Not held:** the licence lifecycle widening (F-1, part B).

**C: availability and fail-open.**
- Approvals that cannot be read fail closed (`TestApprovalsUnreadable_FailsClosed`).
- A licence read error refuses (G-12).
- Every state of `ResolveLicensingChain` except `resolved` fails closed in `operatingmarket` and `kyc` (`unavailable`). `rg` now
  errors on `licence_invalid` (a fail-open was fixed).
- Alert raise failures do not affect enforcement (F-3 covers detection).
- Sweep edge cases: NULL dates mean "nothing raised" (F-3). Far-future dates are fine. `expires_at = today` counts as expired
  (half-open boundary, consistent with `EvaluateLicenceValidity`). An unknown status is classified as widening in the writer.
- The launch gate fails closed on any read error. See F-8 on deadlock.
- Risk refusals do not touch existing rules (F-9).
- Lock waits are bounded (`lock_timeout` 5 s) and surface as a retryable 409.

**D: tenant isolation.**
- New tables use FORCE RLS: `licensing_change_*`, `licence_versions`, and `tenant_jurisdiction_configs` (kept).
- The tenant projection of requests is bound to `subject_tenant_id`. The approvals projection goes through an RLS-filtered
  request subquery.
- Requests with no subject tenant (`licence_create`, ceilings) are invisible to tenants, which is conservative.
- The classify existence checks run under the requester's RLS, so a foreign tenant id gives LC010 "unknown tenant". There is no
  existence oracle.
- `licence_versions` is platform-only for reads.
- The ADR 0099 acting fences are present on all three new tables.
- Cross-tenant reads are covered for the service (`TestAuthorization_PlatformAdminVsTenantStaff`) and HTTP
  (`TestLicensingChangeHTTP_TwoPersonFlow_AuthzAndRedaction`). SQL-level tests are still owed (section 5).

**Model and text checks.**
- No universal licence flag. The hybrid model is preserved: `licence_create` takes `licensee` `platform|tenant`, the composite FK
  `tenants_licence_matches_model` holds (verified: flipping `licensing_model` or `licensee` on a bound pair is refused), and own
  licences are exclusive (`uq_tenants_exclusive_own_licence`).
- No legal assertion in data or UI text. The API disclaimer
  (`internal/licensingchange/licensingchange.go:87`) and the runbooks state software capability only.
- No licence, jurisdiction or country is seeded by 0176 or 0177.

## 4. Residuals (exposure, detection)

| ID | Residual | Exposure | Detection |
|---|---|---|---|
| R-1 | F-1 licence UPDATE ungoverned (SQL GUC; single-actor HTTP widening) | stolen runtime credential, or one platform_admin with step-up | `licence_versions` row (attribution forgeable, F-2); audit row for the HTTP path; no alert |
| R-2 | F-2 `licence_versions` forgery and overflow poisoning | stolen runtime credential | version gaps or post-0176 `baseline` rows; failing lifecycle writes (`integer out of range`) |
| R-3 | Owner credential bypasses LC040 (lic2 R1, accepted) | the owner / migration credential | none in DB; credential custody |
| R-4 | Sock-puppet Person: four-eyes rests on Person integrity (ADR 0119 staff lifecycle) | collusion or compromise of the platform staff onboarding approvers | staff lifecycle audit |
| R-5 | Actor-proof key lives in the app process (`ACTOR_PROOF_KEYS`). Runtime credential plus key = full forgery (ADR 0110 residual) | app host compromise | nonce table, audit |
| R-6 | Sweep silent failure; NULL-expiry licences unmonitored; unbound licences not alerted (F-3) | operational | logs only |
| R-7 | Single-actor narrowing as an insider DoS lever (F-7) | one platform_admin | audit `licensing_change.executed`, events |
| R-8 | `licence.*` alert forgery through the dispatcher GUC (F-6) | stolen runtime credential | alert rows without a matching sweep log line |
| R-9 | Launch gate lock order not load-tested; ledger-finance review owed (F-8) | availability only (fails closed) | 40P01 / 55P03 logs |
| R-10 | Existing deposit or jurisdiction-scoped risk rules not surfaced; DB accepts raw rows (F-9) | tenant-scoped SQL actor; legacy rows | none |
| R-11 | Alert delivery to a person not built (ALERT-DELIVERY-1) | operational | durable rows only |
| R-12 | HDR-M-1 interim (platform_admin only, two approvals): with fewer than 3 platform_admin Persons, no market can be opened | availability | n/a |

## 5. Tests `qa` must add (authorization / isolation)

1. Runtime role with a platform GUC (no proof): `UPDATE licences SET jurisdiction_id | licence_number | permitted_markets |
   permitted_products` is refused. `status` toward `active`, a later `expires_at`, or a cleared `expires_at` is refused without an
   executed widening request in the txid. Suspend without a proof is refused. Today all of these succeed (F-1).
2. Runtime role with a platform GUC: `INSERT INTO licence_versions` is refused, and a licence UPDATE after a row with
   `version_no = 2147483647` exists is impossible to set up (F-2).
3. SQL level, tenant session of tenant A (real staff with Person): an INSERT of a request whose payload subject is tenant B is
   refused (LC002 or RLS). `SELECT` of B's requests and approvals returns 0 rows. An UPDATE (cancel / expire) of B's request
   affects 0 rows.
4. A tenant-filed brand-rung policy naming another tenant's brand is refused at filing (F-4).
5. A failure of the licence sweep's tenant listing raises a durable alert (F-3, once the kind exists).

## 6. Verification run (private DB `secrev3…`, dropped)

- Live probes (section 2 F-1 / F-2, section 3 A) as `igaming_runtime`, inside rolled-back transactions. The owner-seeded synthetic
  fixture was removed with the database.
- `-tags integration -count=1 -p 1`: `internal/licensingchange` (18 tests, all PASS, none skipped), `internal/jurisdiction`,
  `internal/actorproof`, `internal/alerting -run 'Migration0176|Licence'`,
  `internal/casino -run 'StatusGate|LaunchGame_Brand|Bootstrap'`, `internal/launchgov -run 'Licence|Validity'`: all ok.
- Unit: `go test ./internal/db ./internal/risk ./internal/auth`: ok.
- Not run: full `internal/httpserver`, `internal/payments`, the full repository integration suite, or any load or concurrency
  test of the launch gate.

## 7. Out of scope of this review

Penetration testing, production key management, the provider-dependent paths, Back Office UI, legal sufficiency of any licence
or jurisdiction, and the pre-existing GUC-as-authority model in general (only its consequences for these slices are rated).
Passing this review does not make the subsystem "secure". It means no unrecorded blocker was found in the reviewed diff, given the
residuals above.

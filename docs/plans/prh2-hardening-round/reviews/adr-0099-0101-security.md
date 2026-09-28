# Security review — ADRs 0099–0101, plus the ADR 0102 follow-up ruling (2026-09-28)

Reviewed at `10e0471`, read-only. Design review only: no tests or DB probes.

## Part 1: `alerting.raise_failed` as a third dispatcher meta-Kind (LF F3) — ACCEPTED

Security revises its earlier Q3(b) note: the swallowed classes are deterministic, so a durable marker is required.

Conditions:
- **Scope.** The meta list is exactly `('alerting.unrouted','alerting.delivery_dead','alerting.raise_failed')`, with `tenant_id IS NULL AND subject_tenant_id IS NULL`. The SR-2 restriction applies on occurrences too. No UPDATE or DELETE.
- **Attributes, enforced by trigger.** The attributes are exactly `{kind, sqlstate_class}`:
  - `kind` must be an existing Kind;
  - `sqlstate_class` must match `^[0-9A-Z]{2}$`, or be `go_validation`;
  - the discriminator is forced to `'kind:'||kind`.

  So there is no free-text channel into a P1.
- **No subject tenant**, which would re-open SR-1. The Error log line may carry `tenant_id`; the metric has no tenant label.
- **Who may use the dispatcher identity.** Only `internal/alerting`, meaning the dispatcher and the `Flush` fallback (static test). The fallback can construct only `raise_failed`. Detached re-raises still reopen the originating scope.
- **If the fallback fails:** Error log plus `phase="fallback"`; no recursion.
- **Placement.** Within LF F5's post-response budget.
- **Tests:**
  - a persistent P0001 persists `raise_failed` and the 200 is unchanged;
  - a malformed dispatcher insert is refused;
  - a mutant with the fallback removed is killed.

**On the LF F6 vs SR-5 resolution** (post-commit detached raise at REPEATABLE READ sites, stable keys): no objection. F7(a) is not adopted.
- Residual: a crash between commit and the raise loses that alert. This is acceptable only because those sources are re-detected every run.
- Revisit if an event-type stream is ever added at REPEATABLE READ.
- The RR-site detached raise also gets the terminal fallback.

## Part 2: ADRs 0099–0101

| ADR | Verdict |
|---|---|
| 0099 (K1) | **ACCEPT WITH CONDITIONS.** C-99-1 and C-99-2 go into the ADR **before K1 code**. |
| 0100 (K2) | **ACCEPT WITH CONDITIONS** |
| 0101 (K3) | **ACCEPT WITH CONDITIONS.** C-101-1 goes into the ADR **before K3 code**. |

S-1..S-13 and HD-PRH2-2(c), -6 and -7 are otherwise carried correctly.

**Verified in code:**
- **Staff creation.** The allowlist at `admin_routes.go:491` excludes `platform_admin`. `finance`, `risk_manager`, `promotions_manager` and `bonus_operations` are platform-only (`:504-533`). A **tenant can create `compliance` accounts with a password and `person_id` it chooses**.
- **No staff lifecycle API.** The only staff UPDATE is the person link (`staff_user.go:164`). No API changes role or status, resets passwords, or suspends staff.
- **`platform_admin`** accounts come only from `cmd/seed-admin`.
- **Policies with a `tenant_id IS NULL` arm:**
  - `staff_users` (0011:39-47);
  - `sessions` (0012:56-67);
  - `login_attempts` (0013:26);
  - `audit_log` (0014:32-41);
  - `player_restrictions` (0037:122-131);
  - `risk_rules` (0041:161-180);
  - `open_bet_self_exclusion_policies` (0043:244-281).

  Security did not check whether later migrations narrowed the last three.

| ID | Sev | Finding | Required change |
|---|---|---|---|
| **K1-1** | **Med** | "No existing policy matches an acting session" is false. Acting sessions leave `app.tenant_id` unset, so they satisfy every NULL arm above. That lets them read platform staff rows including `password_hash`, **insert a `platform_admin` or a platform `sessions` row**, and read or write platform audit rows (SA-3). | C-99-1 |
| **K1-2** | **Med** | `compliance` as an eligible grantee: tenant admins mint these accounts with passwords they choose, and no reset or first-login-change flow exists. This is S-1 one role over. | C-99-2 |
| K1-3 | Low-Med | There is no suspend or role-change API, so the S-4 re-checks and the re-attestation view are only reachable by a direct DB change. Grant revoke is the real emergency stop. | Register STAFF-LIFECYCLE-1; the runbook names revoke as the emergency stop. |
| K1-4 | Low | Acting audit rows carry the actor only by convention. | An `audit_log` trigger for acting sessions (actor = acting principal, tenant = acting tenant); the tenant presentation marks them `platform_acting`. |
| K1-5 | Low | INV-CAP-6 holds for the HTTP API only. `seed-admin` is an out-of-band trust root, and the NULL arm lets any unset-tenant path insert a `platform_admin`. | Reword it as "no HTTP API". Add a static test that only `cmd/seed-admin` inserts `platform_admin`. Recommended: a DB guard requiring a bootstrap GUC. Document `seed-admin` as a two-person, audited procedure. |
| K1-6 | Low | Acting reads return whole rows. | A column-discipline static test (open item 2). |
| K2-1 | Low | Unbounded tenant "tightening" could make M2 unsatisfiable on in-flight payouts, even for a suspended tenant. | **Architect chooses:** (a) a technical CHECK bound on approval counts, or (b) for `payment_force_resolve` on non-active tenants, evaluate only the platform, jurisdiction and profile rows. |
| K2-2 | Low | No DB rule links a `manual_adjustment` to its request. | Register a follow-up (migrate the fixtures, then add the DB rule); the static test is mandatory in K2. |
| K2-3 | Info | The free-text note. | Bound its length. |
| **K3-1** | **Med** | The guard still admits `ambiguous → succeeded/declined` with provider evidence kinds. So an acting session with UPDATE on `payment_attempts` could forge evidence and terminate a payout without a resolution. | C-101-1 |
| K3-2 | Low | `provider_confirmed_out_of_band` relies on off-platform evidence. | `evidence_ref_hash` is NOT NULL for M2. |
| K3-3 | Low | The reserved-prefix refusal "at every ingress". | Tests at sync, callback, poll **and statement import**. |

**Rulings on the open items:**
1. **Eligible grantees:** `finance` (platform-minted only) and `platform_admin` via G-P2 only. **`compliance` is not eligible in PRH-2.** `tenant_admin` is never eligible. The residual on `finance` (its creator chooses the initial password) goes to STAFF-LIFECYCLE-1.
2. **Acting-session reads:** no row narrowing now. Close the NULL arm (C-99-1), and add a column-discipline static test:
   - `staff_users(id, tenant_id, role, status, person_id)`;
   - `player_accounts(id, tenant_id, person_id, status)`;
   - never `password_hash`, email or other PII.
3. **Lifetimes:**
   - G-P2 (acting) grants need a NOT NULL `valid_until` with a maximum-lifetime CHECK. The value is a technical security default recorded in K1's implementation record.
   - G-T and G-P1 grants: `valid_until` stays optional.
   - Re-attestation: no auto-void; an entry older than a configurable technical default raises a p2 alert.
4. **S-2(iii) extends to approvers: YES.** An approver who authored or approved a contributing policy does not count. Enforced by trigger and at execution.
5. **M1/M2 for non-active tenants: available.** Tenant status is read in the transaction and recorded. For K2 on non-active tenants, `goodwill_credit` is refused.
6. **Re-attestation: either form is acceptable.** Deferring the table and view to STAFF-LIFECYCLE-1, with a `grant.reattested` audit action meanwhile, is fine. If the view is built now, it uses a typed table.
7. **INV-CAP-6:** holds for the HTTP API only (K1-5).

**Implementation conditions:**
- **0099 (K1)**
  - **C-99-1 (in the ADR before code):** `AS RESTRICTIVE` policies deny any session with `app.acting_*` set, on every table with a NULL arm. Exceptions: the §6.4 acting policies, and reading one's own `staff_users` row. Extend A-4 accordingly, and correct §6.2, TM-3 and TM-6.
  - **C-99-2 (in the ADR before code):** eligible grantees are `finance` and G-P2 `platform_admin` only.
  - **C-99-3:** the K1-4 trigger and the `platform_acting` marker.
  - **C-99-4:** the K1-5 static test, the INV-CAP-6 wording, and the `seed-admin` runbook procedure.
  - **C-99-5:** the column-discipline static test.
  - **C-99-6:** G-P2 `valid_until` NOT NULL with a lifetime CHECK.
  - **C-99-7:** STAFF-LIFECYCLE-1 registered; the runbook names grant revoke as the emergency stop.
  - **C-99-8:** `WithPlatformActingInTenant` takes P only from the verified token subject and X only from the route-validated target; the session-open call is audited; raw `set_config('app.acting_…')` is forbidden by static test.
- **0100 (K2)**
  - **C-100-1:** approvers excluded under S-2(iii).
  - **C-100-2:** K2-1 resolved by the architect.
  - **C-100-3:** no `goodwill_credit` on non-active tenants.
  - **C-100-4:** the static test that only `internal/adjustment` posts `manual_adjustment`, plus the K2-2 follow-up.
  - **C-100-5:** a length bound on the note.
  - **C-100-6:** B-3 includes a K1-1 negative case.
- **0101 (K3)**
  - **C-101-1 (in the ADR before code):** acting UPDATE policies on `payment_attempts`, `withdrawal_requests` and every table `Complete`/`Fail` touch require an M2 resolution in `executing` with `executed_txid = txid_current()`. On `payment_attempts`, the WITH CHECK also requires `last_evidence_kind='operator'`. Tests: forged `callback` evidence, and an update without an executing resolution, are both refused.
  - **C-101-2:** `evidence_ref_hash` NOT NULL for M2.
  - **C-101-3:** reserved-prefix ingress tests, including statement import.
  - **C-101-4:** tenant status recorded on the resolution.

**Launch flags:**
- The TM-7 residual (two colluding platform admins, plus the `seed-admin` trust root) needs human risk acceptance before real-money K operations.
- TM-10: a LEGAL/COMPLIANCE REVIEW before any own-licence tenant receives a G-P2 grant.
- HD-PRH2-8 must be answered.

**Not covered:** later narrowing of the 0037/0041/0043 NULL arms; the `permission.go` role sets; the ledger fence predicate; login refusal for suspended tenants; `persons` RLS (0015); and the `ledger.Post`/`Complete`/`Fail` call enumeration.

**Orchestrator dispositions:**
- `raise_failed` is adopted into the ADR 0102 revision.
- The G-P2 maximum-lifetime and re-attestation defaults are treated as **technical security defaults**, not policy. They are configurable values recorded in K1's implementation record, and are listed for the human in the final report.
- Product-owner-proxy's re-attestation simplification is adopted (a `grant.reattested` audit action now; the table and view in STAFF-LIFECYCLE-1).

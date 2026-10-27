# Screening activation gate: design v2 (GATE-B, architect)

- **Status: DESIGN ONLY. NOT IMPLEMENTED.** No migration, no code, no database access. Nothing here is a legal opinion, a
  compliance approval, production readiness or a provider validation. All providers stay MOCK.
- **Base:** branch `gate-design-v2` from the accepted integration tip `01ea296` (migration head **0194**).
- **Replaces:** the first proposal of a database-enforced activation gate (D1 completion report s5 item 1 and s6,
  `d1-security-closure-record.md` s9.5 item 1 and s10.4, "CAPACITY-GATE-SWITCH"). That proposal left legacy gates in force
  and only flagged them. This version adds an explicit treatment for them, a full lifecycle, an honest trust-boundary analysis
  (including table owner and superuser), and a governance and migration plan.
- **Who decides what:** sections 12 (owner decisions) and 13 (engineering choices made here) are kept separate. Owner decisions are
  stored as configuration with **no shipped value**. When the configuration is absent, the system refuses.

Reading order: s1 (verified facts) and s2 (invariants) first. Then s3 to s7 cover the five owner-mandated items in order. s8
onwards hold the threat model, codes, audit, reconciliation, decisions, acceptance criteria and the list of points reviewers should attack.

---

## 1. Verified current state (file:line at `01ea296`)

Every statement below was checked by reading the cited file. Nothing was run against a database.

| # | Fact | Evidence |
|---|---|---|
| F1 | `database_environment` is the environment anchor. It holds one row (`singleton`), RLS is ENABLED but not FORCED, and the one read policy is `USING (true)`. A guard trigger refuses UPDATE, DELETE and TRUNCATE, and accepts an INSERT only from a member of the table owner. **No row means production** (`rg_database_environment()`). The runtime role has SELECT only. Like every trigger, the guard binds the owner only "while enabled". | `migrations/0135_rg_limits_schema_and_index.up.sql:172-211`, `:189-192`, `:210`, `:1371-1377` |
| F2 | `screening_worker_heartbeat(text, boolean)` is SECURITY DEFINER and proof-less. It upserts `screening_worker_heartbeats` and is **runtime-executable**. `screening_worker_available()` is true when any row was seen in the last 10 minutes (an engineering bound). The migration's own comment calls it "a configuration guard, not an attestation". | `0181_...up.sql:691-731`, grant `:1884`, `deploy/init-app-role.sql:240` |
| F3 | `aml_screening_trigger_policies` is append-only (`aml_deny_mutation` on UPDATE, DELETE and TRUNCATE) and FORCE RLS. Its policy is `aml_owner_session` on `aml_session_tenant()`, and the runtime role has no table privilege. A BEFORE INSERT guard (`_10_guard`) binds the author columns to the proven session actor. | `0181:734-755`, `:819-839`, `:842-858` |
| F4 | The policy **in force** for (tenant, jurisdiction) is the latest by `(created_at, id)` among rows with `effective_from <= now()`. The gate loops over every jurisdiction whose in-force row has `gate_withdrawal`. The grace baseline `v_first` is the earliest `effective_from` of any gating row. | `0181:1128-1133`, `:1507-1517`; redefined with the same selection in `0192:120-`, `0193:12-`, `0194:32-` |
| F5 | `aml_screening_gate_environment_ok()` is true when the anchor is `development`, `staging` or `test`. | `0181:912-915` |
| F6 | `aml_screening_trigger_policy_set` (the 0181 setter) refuses a gate without a fresh heartbeat (`screening_worker_heartbeat_missing`) or with a production or unrecorded anchor (`gate_environment_refused`). Since 0191 it is **owner-only**. | `0181:1060-1075`; `0191:236-243` |
| F7 | `aml_screening_trigger_policy_author` is the routed author and is TIGHTEN-ONLY against both the latest and the in-force version. It repeats the heartbeat and anchor checks for a gating version and takes the lock `hashtextextended('screening_trigger:'\|\|tenant\|\|jur, 191)`. It is runtime-executable. **It still lets one officer chain (quorum + proof) enable a gate in development, staging and test.** | `0191:129-233`, `:173`, `:176`, `:185`; `deploy/init-app-role.sql:254` |
| F8 | `aml_screening_trigger_policy_loosen` (two Persons, LC040 admission) writes through `screening_trigger_loosen_apply`. It takes `screening_loosening:`\|\|tenant/190, then the trigger key /191. It never enables a gate (`trigger_gate_enable_refused`, also in the table guard) and is gated by the **inert** capability `aml_screening_loosening_settings` (no row ships). | `0191:75-127`, `:248`, `:450-`, `:481`, `:484`; `0190:16-19`, `:49-` |
| F9 | Officer helpers check the requester, the counted approvers (24 h fresh, distinct Persons, never the requester) and the executor, plus the owner-scope quorum (2 or more Persons). Own-licence tenants are refused (`loosening_not_supported_for_tenant_licensing_model`). | `0190:387-464`, `:426` |
| F10 | The **gate predicate does not read** the anchor, the heartbeat or any authorisation. It reads trigger and screening policies, requests, results and holds. Once a gate row is in force, it keeps holding whatever happens to the heartbeat or the environment. | `0181:1477-1577`, `0194:32-` |
| F11 | The screening worker runs **inside the API process**, under the runtime credential, unless `cfg.GuardEnvironment()=="production"` or no provider is set. The heartbeat is sent over the runtime pool (`WithoutTenant`). | `cmd/platform-api/main.go:799-810`; `internal/aml/screening_worker.go:187-198` |
| F12 | The Go writer refuses a gate when `cfg.Environment=="production"` (second refusal) and calls `aml_screening_trigger_policy_author`. The owner-pool method `OwnerSetScreeningTriggerPolicy` calls the owner-only setter. | `internal/aml/screening_trigger_service.go:136-181`, `:144`, `:158-160` |
| F13 | Route: `POST /v1/admin/tenants/{tenantID}/screening/trigger-policy` (critical step-up, `PermAMLHoldManage`). | `internal/httpserver/aml_screening_routes.go:34-35` |
| F14 | Closed refusal codes come from `aml.RefusalCodes`. A database HINT outside the table is **ignored** (never copied into a response), and SQLSTATE drives the HTTP status. | `internal/aml/refusal.go:5-8`, `:9-` |
| F15 | A catalogue test pins the runtime-executable writers of `aml_screening_trigger_policies` to exactly `aml_screening_trigger_policy_author` and `screening_trigger_loosen_apply`. The detector covers direct DML, dynamic SQL and transitive callers. | `internal/aml/screening_d4_integration_test.go:135-136`; `internal/aml/screening_d7_integration_test.go:101-152` |
| F16 | Roles: owner `igaming` is NOSUPERUSER, NOCREATEDB, NOCREATEROLE and NOBYPASSRLS, and owns the database and the `public` schema. `igaming_runtime` is a non-owning runtime role. In production the server refuses to start if the runtime role owns tables or has memberships. | `deploy/init-app-role.sql:48-70`; `internal/db/production_safety.go:64-75`, `:115-`, `:150-167` |
| F17 | Prior art for catalogue fingerprints exists: trigger `tgenabled` plus definition md5, and function and ACL lines. The backup tooling statically bans `session_replication_role`, `DISABLE TRIGGER` and `NO FORCE ROW LEVEL SECURITY`. | `deploy/backup/baseline.sql:109`, `:145`, `:162`, `:208`; `deploy/drill/verify.sql:202`; `deploy/backup/backup_static_test.go:84` |
| F18 | The licensing-change primitive: an append-only request with a closed state machine, `executed_txid`, approvals on the payload hash that are at most 24 h old by distinct Persons, and actor-proof operations `licensing_change:{request,approve,reject,cancel,execute}`. A narrowing request with 0 approvals executes in the filing transaction. The final approval executes in its own transaction. | `0177_...up.sql:604-673`; `internal/licensingchange/flow.go:25-27`, `:118-119`; `internal/actorproof/actorproof.go:158-162`, `:315-322` |
| F19 | The capacity rule "no gate until capacity is accepted" is a **process control only**. | `docs/runbooks/aml-screening-triggers.md:230-234`; `d1-security-closure-record.md` s9.5 item 1 |
| F20 | Open owner rows: C4-LOOSEN-1 and D1-CLOSURE-1 (CAPACITY-GATE-SWITCH, withdrawal-release policy, sanctions false-positive policy, provider attestation, A1 break-glass procedure). | `docs/governance/human-decision-register.md:151`, `:154` |
| F21 | Sandbox server files are PostgreSQL 16 (`/usr/share/postgresql/16`). `system_functions.sql` there holds no REVOKE for `pg_control_system()` or `pg_postmaster_start_time()`, so they keep the default PUBLIC EXECUTE. **The implementation must confirm this on the target, and on RDS.** `CREATE EVENT TRIGGER` needs superuser in PG16, so the migration role `igaming` **cannot** create event triggers (F16). | file inspection only; PostgreSQL 16 documentation |

**Gaps this design closes (all confirmed above):**
- G1: in development, staging and test, one officer chain can enable a gate. The only preconditions are a heartbeat that any runtime session can forge (F2, F7) and the anchor.
- G2: no record shows that anyone authorised a gate, for which instance, until when, or on what capacity evidence.
- G3: existing gates cannot be classified as authorised or not.
- G4: an owner or superuser change to the guard (DISABLE TRIGGER, CREATE OR REPLACE) would leave no trace that the platform checks.

---

## 2. Design invariants (verified by `qa` and `code-reviewer`)

| ID | Invariant | How it is pinned |
|---|---|---|
| I1 | **The gate predicate never consults authorisation state.** `aml_screening_gate_denies`, `aml_hold_active`, `aml_screening_enqueue_due`, `aml_screening_gate_overdue` and `aml_screening_parked_*` read no `screening_gate_*` object. Authorisation affects only whether a new **enabling** row may be inserted. | Static catalogue test: the `prosrc` of those functions references no `screening_gate_` identifier. Mutation: making the gate read the classification must fail the test. |
| I2 | **Nothing in this design writes a trigger-policy row that switches a gate off**, and nothing deletes or edits one. Lapse, revocation, mismatch, attestation staleness, freeze and break-glass never switch a gate off. Switching off stays a governed loosening (F8) or the documented owner setter path (A1, F6). | Catalogue writer pin (F15) unchanged: the new executors are not writers of `aml_screening_trigger_policies`. |
| I3 | **No held withdrawal is released** by any new function. No new function touches `aml_holds`, `aml_hold_lifts`, `aml_screening_requests` states, `aml_screening_results`, payouts or the ledger. | Writer pins for those tables are unchanged. |
| I4 | **Classification is derived, never stored** in a column any role writes at runtime. | No `status`/`state` column exists on the evidence tables. Classification comes only from `screening_gate_classify()`. |
| I5 | Every evidence table is owner-owned, FORCE RLS and append-only (UPDATE, DELETE and TRUNCATE are denied by triggers set `ENABLE ALWAYS`). The runtime role has **zero** table privileges. Rows are written only by admitted executors. | Catalogue tests on ACLs, `relforcerowsecurity`, `tgenabled='A'`, and the writer allow-list per table. |
| I6 | **Applying a migration or starting the service never creates** a settings row, an authorisation, an attestation, a freeze or a gating row. | Test A-17 (s15). |
| I7 | **When configuration is absent, new enabling is refused.** No value ships for any owner parameter. | Tests A-3 and A-4. |
| I8 | A gate can still not be enabled where the anchor says **production or unrecorded**. `aml_screening_gate_environment_ok()` is unchanged, and this design adds no production path (s12 OD-G2). | The existing tests stay, plus a CHECK on the settings table. |
| I9 | Triggers are **not** claimed to stop the table owner or a superuser. Against them the controls are detection and operations (s5.5 to s5.7). | Wording review, plus the threat model (s8). |
| I10 | Sanctions, PEP, hold and needs_officer behaviour is untouched. | As I3. |
| I11 | The guard and the executors run under READ COMMITTED only. Any other isolation level is refused (the RL024 precedent: `0135:1169-1170`). | Test A-12. |
| I12 | The licensing and operating-basis model is untouched. The jurisdictions a gate may cover are still `aml_screening_jurisdictions(tenant)`. No country, jurisdiction or market value is hard-coded. | Code review, plus a grep test for jurisdiction literals in the new migrations. |

---

## 3. Item 1: existing unauthorised gates, restored databases and mismatched authorisations

### 3.1 Vocabulary (precise, from the F4 selection rule)

For one (tenant, jurisdiction), order rows by `(created_at, id)`:
- **gating row**: a row with `gate_withdrawal = true`.
- **gate run**: a maximal sequence of consecutive gating rows in that order. A non-gating row ends a run.
- **enabling row**: the first row of a run. **continuation row**: any later row of the same run.
- **New enabling** is the insertion of a gating row `NEW` such that NOT (the latest row by `(created_at, id)` is gating AND the
  row in force now is gating). This covers: the first gate ever; re-enabling after a gate-off; enabling while a governed gate-off is
  pending (the latest row is non-gating); and enabling while the only gating row is a pending, future-dated one. All of these
  need a current authorisation. Anything else (both the latest and the in-force rows gate) is a **continuation**. The routed author's
  tighten-only rule (F7) already forces a continuation to be equal or stricter. The governed loosening (F8) may also write a
  continuation (a longer alert threshold, the gate staying on).
- **Enabling rows are immediate** (engineering choice E-6): `effective_from <= clock_timestamp()`. A pending enabling row can
  therefore never take effect after its authorisation was revoked (see s4.3). Continuations may still be future-dated (up to 30
  days, as today).

### 3.2 Cut-over inventory (no data rewrite)

The guard migration (s7, step 0197) records the existing state **in the same transaction** that installs the guard. It takes
`LOCK TABLE aml_screening_trigger_policies IN SHARE ROW EXCLUSIVE MODE` first, so no row can slip in between the snapshot and the
guard. It writes **no policy row** and changes **no existing row**.

```sql
-- one row per database; written only by migration 0197
CREATE TABLE screening_gate_cutover (
    singleton          boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    applied_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    system_identifier  numeric(20,0) NOT NULL,      -- (pg_control_system()).system_identifier at cut-over
    database_oid       oid NOT NULL,                -- (SELECT oid FROM pg_database WHERE datname = current_database())
    environment        text NOT NULL,               -- rg_database_environment() at cut-over ('production' if unrecorded)
    inventory_rows     integer NOT NULL,
    inventory_sha256   text NOT NULL CHECK (inventory_sha256 ~ '^[0-9a-f]{64}$')
);
-- every trigger-policy row that existed at cut-over (gating or not): the ONLY rows that can ever be classified legacy
CREATE TABLE screening_gate_legacy_inventory (
    policy_id                 uuid PRIMARY KEY,
    tenant_id                 uuid NOT NULL,
    licensing_jurisdiction_id uuid NOT NULL,
    gate_withdrawal           boolean NOT NULL,
    created_at                timestamptz NOT NULL,
    row_sha256                text NOT NULL,       -- sha256 of the canonical row text at cut-over
    FOREIGN KEY (tenant_id, policy_id) REFERENCES aml_screening_trigger_policies (tenant_id, id) ON DELETE RESTRICT
);
```

Both tables are owner-owned, FORCE RLS (`screening_gate_legacy_inventory` uses the `aml_owner_session` tenant policy, as in
F3), and append-only (UPDATE, DELETE and TRUNCATE are denied by `ENABLE ALWAYS` triggers). The runtime role has no privilege. Their
only writer is the migration. Their insert guard refuses any insert once `screening_gate_cutover` has a row, so the inventory is
write-once.

Why an inventory and not a `created_at < cutover` test: `created_at` is a DEFAULT, not forced (F3 guard), so a later insert
could backdate itself into "legacy". From 0197 on, the guard forces `NEW.created_at := clock_timestamp()`, and legacy status comes
from membership in the inventory, never from a timestamp.

### 3.3 Classification (derived, read-only)

```sql
CREATE FUNCTION screening_gate_classify(p_tenant uuid DEFAULT NULL)
RETURNS TABLE (
    o_tenant_id uuid, o_jurisdiction_id uuid,
    o_state text,                    -- closed: see table below
    o_in_force_policy_id uuid, o_enabling_policy_id uuid, o_pending_policy_id uuid,
    o_authorisation_id uuid,         -- the authorisation the state was derived from (NULL when none)
    o_valid_until timestamptz, o_severity text)   -- 'p1' | 'p2' | 'p3' | NULL
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp;
-- p_tenant NULL: platform_admin session only (licensing_change_actor() scope platform); otherwise p_tenant must equal the
-- session tenant (AT002) or the caller is a platform session. Reads tenant by tenant under app.tenant_id (RLS), like 0190:494.
```

States are evaluated for every (tenant, jurisdiction) that has any trigger-policy row. When several apply, the first row of the
table below wins:

| Precedence | State | Definition (on the run that holds the in-force gating row, or failing that the pending gating row) | Severity (staging/production anchor; development/test) |
|---|---|---|---|
| 1 | `unexplained` | A gating row that is **not** in the legacy inventory and whose `gate_authorisation_id` is NULL. Or a stamp that points at an authorisation of another (tenant, jurisdiction), or one not valid at the row's `created_at`. Or an enabling row not in the inventory whose stamp is not an `activation` authorisation. **This can only happen if the guard was bypassed** (owner DISABLE TRIGGER, superuser, or a modified guard). | p1; p1 |
| 2 | `environment_mismatch` | The authorisation's recorded `environment` differs from `rg_database_environment()` now. | p1; p1 |
| 3 | `identity_mismatch` | The authorisation's `system_identifier` or `database_oid` differs from the live values (a logical restore into another cluster or database, s4.6). | p2; p2 |
| 4 | `revoked` | The authorisation (or adoption) is covered by a revocation. | p2; p3 |
| 5 | `lapsed` | `valid_until <= now()`, not revoked. | p2; p3 |
| 6 | `legacy_unauthorised` | The enabling row is in the legacy inventory, and no current `adoption` authorisation covers it. | **p1**; p2 |
| 7 | `authorised` | An `activation` authorisation stamped on the enabling row, or a current `adoption` naming it. Not revoked, not lapsed, identity and environment matching. | none |
| 8 | `pending_enabling` | Only a future-dated gating row exists, which is impossible for rows created after 0197 (E-6). Classified like the run it belongs to, so a legacy pending gate is `legacy_unauthorised`. | as its run |
| 9 | `none` | Neither the in-force row nor the latest row gates. | none |

A continuation row inherits the classification of its run's **enabling row**. The guard copies the enabling row's
`gate_authorisation_id` onto it (or NULL for a legacy run). This is forced, never taken from the caller.

A gate row is always **stricter than no row**. No state, however bad, is ever fixed by removing the gate automatically. The only
consequence of a bad state is a refusal of **new** enabling, an alert, and a reconciliation line.

### 3.4 Containment: what remains, what is refused

"Contained" means four things. The gate keeps holding exactly as today (I1, I3). It cannot spread. It cannot be quietly
legitimised. It is reported until a human resolves it through a governed path.

| Operation | `authorised` | `lapsed` / `revoked` | `identity_mismatch` | `legacy_unauthorised` | `environment_mismatch` / `unexplained` |
|---|---|---|---|---|---|
| The gate keeps holding; held withdrawals stay held | yes | yes | yes | yes | yes |
| Continuation that tightens (more events, shorter alert) via the routed author | allowed | allowed | allowed | allowed | allowed (stricter never harms) |
| Continuation that loosens while staying on (longer alert) via governed loosening | allowed | allowed | allowed | allowed | **refused** `gate_integrity_unresolved` (do not tune a suspect gate; switch it off or investigate) |
| Governed gate-off (F8) | allowed | allowed | allowed | allowed | allowed (the remedy; two Persons) |
| Owner setter gate-off (A1 break-glass, owner pool) | as today | as today | as today | as today | as today (detected, s5.5) |
| Re-enable after off, or enable while a gate-off is pending (= new enabling) | needs a current activation authorisation | same | refused until re-attested and re-authorised | needs an activation authorisation **and** no other legacy gate of the tenant (row below) | refused |
| New enabling of **another jurisdiction of the same tenant** | normal rules | normal rules | normal rules | **refused** `gate_tenant_has_unauthorised_gate` | refused |
| New enabling of **any tenant** on this instance | normal rules | normal rules | normal rules | normal rules | **refused instance-wide** `gate_integrity_unresolved` |
| Adoption (`screening_gate_adopt`, s3.6) | n/a | allowed (re-authorise the existing run) | allowed after the instance re-attestation | allowed | **refused** `gate_adoption_target_unexplained` / `gate_authorisation_environment_mismatch` |
| Replacement authorisation (new activation for the same tenant and jurisdiction) | allowed | allowed | allowed | allowed but does **not** cover the legacy run (adopt instead) | refused |

The two **refused** rows are the containment boundary. A legacy or suspect gate does not justify a new one. The instance-wide
refusal is limited to integrity states (`unexplained`, `environment_mismatch`, a broken evidence chain, a guard not intact), where
the instance's controls themselves are in doubt.

### 3.5 Alerts

New alert kinds use the 0181 seeding pattern (`0181:672-689`: a temporary single-kind INSERT policy, created and dropped in one block):

| Kind | Severity | Scope | Allowed keys | Raised by |
|---|---|---|---|---|
| `aml.screening_gate_integrity` | p1 | platform | `problem`, `count` | monitor loop (s11) for any p1 problem |
| `aml.screening_gate_authorisation` | p2 | platform | `problem`, `count` | monitor loop for any p2 problem |
| `aml.screening_gate_attention` | p3 | platform | `problem`, `count` | monitor loop for any p3 problem, plus "expiring within the configured warning window" (OD-G1) |

The monitor loop must run **in every environment, production included**. The screening worker does not run in production
(F11), but a legacy gate can exist there (restored data, pre-lic23 rows), so this loop is separate from the worker (s11). Alert
payloads carry codes and counts only, never tenant names or person data (the same rule as `aml.screening_gate_trapped`). Ids
come from the platform-admin reconcile (s11).

### 3.6 Adoption: bringing an existing gate under governance

Adoption is a governed **authorisation** that names an existing enabling row. It never writes a policy row. It changes
classification only.

- Kind `screening_gate_adopt` (s6): payload `{tenant_id, jurisdiction_id, policy_id, policy_row_sha256, validity_seconds,
  capacity_report_reference, capacity_report_sha256, capacity_accepted_persons_max, compliance_reference,
  legal_review_reference, evidence_reference, environment, system_identifier}`.
- Request phase and execute phase (both re-validated): `policy_id` is the **enabling row** of the run that is in force now
  (`gate_adoption_target_invalid` otherwise). `policy_row_sha256` equals the sha256 of the canonical row now, which binds the approvers to
  the exact row (`gate_adoption_digest_mismatch`). The run is not `unexplained` (`gate_adoption_target_unexplained`).
  `environment` and `system_identifier` equal the live values. A current instance attestation exists. Settings exist. All other
  activation preconditions apply (s4.1), **except the worker-attestation and heartbeat checks**. Those protect *new* enabling
  (a gate without a worker traps funds), and the gate already exists. The absence of a worker for an existing gate is reported as
  `gate_without_attested_worker` (p2), not used to block adoption. This exception is listed for review (s17).
- Executor `screening_gate_adopt_apply(p_request uuid) RETURNS jsonb` inserts one `screening_gate_authorisations` row with
  `mode = 'adoption'` and `adopts_policy_id = policy_id`. The run's continuation rows are covered through their enabling row.
- An adoption covers **only that run**. If the gate is later switched off and re-enabled, that is new enabling and needs an activation
  authorisation.

### 3.7 Restored or cloned databases: how every gate is classified

| Case | Inventory | Authorised runs | Legacy runs | Unexplained runs | New enabling |
|---|---|---|---|---|---|
| Physical replica promoted or PITR in the same cluster lineage (same `system_identifier`, same db oid, new timeline) | unchanged | `authorised` (identity matches) | `legacy_unauthorised` | `unexplained` | refused until re-attestation (`gate_instance_attestation_missing`, the timeline or postmaster start differs, s4.7) |
| Physical clone (storage snapshot started elsewhere, same everything) | unchanged | `authorised` (**residual R-3**, s4.7) | legacy | unexplained | refused until re-attestation (postmaster start differs) |
| Logical dump restored into a **new cluster** | restored as data | `identity_mismatch` | legacy | unexplained | refused (no current attestation, identity mismatch) |
| Logical dump restored into a **new database of the same cluster** | restored | `identity_mismatch` (db oid differs) | legacy | unexplained | refused |
| Restore of a pre-0197 dump, then `migrate up` | built at that 0197 | n/a | all existing gating runs | none | after configuration and attestation only |
| Anchor of the restore target differs (a pre-provisioned anchor row kept, the restored one rejected by the singleton PK) | unchanged | `environment_mismatch` (p1) | legacy | unexplained | refused instance-wide |

Restore procedure addition (runbook, s7.7): after **every** restore, run `screening_gate_reconcile()`. Re-attest the instance
(attestor role). Adopt or switch off every gate through the governed kinds. Never insert evidence rows by hand.

Known hole to close operationally: if a production database **never recorded** its anchor (unrecorded counts as production, F1),
a logical restore of it into a cluster where an operator then records `staging` makes that copy a staging database that holds
production data and gates. Recommendation (engineering and operations, s13 E-12): the provisioning runbook **must** record
`production` explicitly on every production database. Reconcile reports `anchor_unrecorded` (p2) wherever the anchor is
missing.

---

## 4. Item 2: authorisation lifecycle (database-enforced)

### 4.0 Evidence tables (DDL-level sketch; names are the proposal)

Every table below: owner `igaming`, `ENABLE` + `FORCE ROW LEVEL SECURITY`, `REVOKE ALL ... FROM PUBLIC, igaming_runtime`,
append-only (`BEFORE UPDATE OR DELETE` row trigger and `BEFORE TRUNCATE` statement trigger calling `aml_deny_mutation()`, both
`ALTER TABLE ... ENABLE ALWAYS TRIGGER`, s5.5), an `AFTER INSERT` chain trigger (s5.6), and a `BEFORE INSERT` admission guard.
Tenant tables use the `aml_owner_session` policy (`tenant_id = aml_session_tenant()`, F3). Platform-level tables use one policy
`TO <owner> USING (true) WITH CHECK (true)`, so no other role sees anything even if a privilege is granted by mistake.

```sql
-- OWNER DECISIONS as configuration (s12). Append-only versions per anchored environment; the latest row wins; NO ROW SHIPS.
CREATE TABLE screening_gate_settings (
    id                                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    environment                          text NOT NULL CHECK (environment IN ('development', 'staging', 'test')),  -- NOT production (I8, OD-G2)
    max_validity_seconds                 bigint NOT NULL CHECK (max_validity_seconds > 0),                -- OD-G1 (no default)
    expiry_warning_seconds               bigint NOT NULL CHECK (expiry_warning_seconds >= 0),             -- OD-G1
    min_approvals                        integer NOT NULL CHECK (min_approvals BETWEEN 2 AND 5),          -- OD-G4 (>= the kind's)
    capacity_report_required             boolean NOT NULL,                                                -- OD-G3
    capacity_persons_cap_required        boolean NOT NULL,                                                -- OD-G3
    compliance_reference_required        boolean NOT NULL,                                                -- OD-G4
    instance_attestation_max_age_seconds bigint NOT NULL CHECK (instance_attestation_max_age_seconds > 0), -- OD-G8
    worker_attestation_max_age_seconds   bigint NOT NULL CHECK (worker_attestation_max_age_seconds > 0),   -- OD-G9
    evidence_reference                   text NOT NULL CHECK (evidence_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$'), -- the owner decision id
    change_request_id                    uuid NOT NULL UNIQUE REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    created_by uuid NOT NULL, created_by_person_id uuid NOT NULL,
    created_at                           timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Owner-attested identity of THIS running instance (s4.7). Written only through screening_gate_instance_attest() by a member
-- of the NOLOGIN attestor role, never by the migration role or the runtime role.
CREATE TABLE screening_gate_instance_attestations (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    environment            text NOT NULL,          -- must equal rg_database_environment() at insert
    system_identifier      numeric(20,0) NOT NULL, -- all of the following are CAPTURED by the function, never passed in
    timeline_id            integer NOT NULL,       -- (pg_control_checkpoint()).timeline_id
    database_oid           oid NOT NULL,
    database_name          name NOT NULL,
    postmaster_start_time  timestamptz NOT NULL,   -- pg_postmaster_start_time()
    server_addr            inet NULL,              -- inet_server_addr() (NULL over a unix socket)
    operator_reference     text NOT NULL,          -- change / incident ticket
    valid_until            timestamptz NOT NULL,   -- attested_at + p_valid_seconds, capped by settings
    attested_session_user  name NOT NULL,          -- session_user (the named operator login), never a shared account
    attested_at            timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Owner-attested statement that a screening worker of provider P is deployed for this instance (s5.3, option A).
CREATE TABLE screening_worker_attestations (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id            text NOT NULL CHECK (provider_id ~ '^[a-z0-9_.-]{1,64}$'),
    synthetic              boolean NOT NULL,       -- must equal the heartbeat's flag; true only where the anchor is not production
    deployment_reference   text NOT NULL,          -- release / deployment id of the worker
    instance_attestation_id uuid NOT NULL REFERENCES screening_gate_instance_attestations (id) ON DELETE RESTRICT,
    valid_until            timestamptz NOT NULL,
    attested_session_user  name NOT NULL,
    attested_at            timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The authorisation itself (activation or adoption). Written only by the executors of the governed kinds (s6).
CREATE TABLE screening_gate_authorisations (
    id                          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                   uuid NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    licensing_jurisdiction_id   uuid NOT NULL REFERENCES jurisdictions (id) ON DELETE RESTRICT,
    mode                        text NOT NULL CHECK (mode IN ('activation', 'adoption')),
    adopts_policy_id            uuid NULL,
    environment                 text NOT NULL,           -- anchor value at execution (== payload, == live)
    system_identifier           numeric(20,0) NOT NULL,  -- live value at execution (== payload)
    database_oid                oid NOT NULL,
    instance_attestation_id     uuid NOT NULL REFERENCES screening_gate_instance_attestations (id) ON DELETE RESTRICT,
    worker_attestation_id       uuid NULL REFERENCES screening_worker_attestations (id) ON DELETE RESTRICT, -- NULL only for adoption
    settings_id                 uuid NOT NULL REFERENCES screening_gate_settings (id) ON DELETE RESTRICT,
    capacity_report_reference   text NULL,  capacity_report_sha256 text NULL CHECK (capacity_report_sha256 ~ '^[0-9a-f]{64}$'),
    capacity_accepted_persons_max bigint NULL CHECK (capacity_accepted_persons_max > 0),
    compliance_reference        text NULL,
    legal_review_reference      text NOT NULL,
    evidence_reference          text NOT NULL,
    valid_from                  timestamptz NOT NULL,    -- = executed_at
    valid_until                 timestamptz NOT NULL,    -- = valid_from + payload validity (<= settings max)
    change_request_id           uuid NOT NULL UNIQUE REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    requested_by uuid NOT NULL, requested_by_person_id uuid NOT NULL,
    approver_person_ids         uuid[] NOT NULL CHECK (cardinality(approver_person_ids) >= 1), -- the COUNTED approvers, snapshot
    executed_by uuid NOT NULL,  executed_by_person_id uuid NOT NULL,
    created_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, id),
    CHECK ((mode = 'adoption') = (adopts_policy_id IS NOT NULL)),
    CHECK (mode = 'adoption' OR worker_attestation_id IS NOT NULL),
    CHECK (valid_until > valid_from),
    FOREIGN KEY (tenant_id, adopts_policy_id) REFERENCES aml_screening_trigger_policies (tenant_id, id) ON DELETE RESTRICT
);

-- Revocations (single actor, fail-safe direction). Either one authorisation, or every authorisation of (tenant, jurisdiction)
-- created before the revocation.
CREATE TABLE screening_gate_revocations (
    id                        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 uuid NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    licensing_jurisdiction_id uuid NOT NULL,
    authorisation_id          uuid NULL,                 -- NULL = all of (tenant, jurisdiction) up to created_at
    source                    text NOT NULL CHECK (source IN ('governed', 'break_glass')),
    change_request_id         uuid NULL UNIQUE REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    incident_reference        text NULL,
    reason_code               text NOT NULL CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    revoked_by uuid NULL, revoked_by_person_id uuid NULL, revoked_session_user name NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((source = 'governed') = (change_request_id IS NOT NULL AND revoked_by IS NOT NULL)),
    CHECK ((source = 'break_glass') = (incident_reference IS NOT NULL)),
    FOREIGN KEY (tenant_id, authorisation_id) REFERENCES screening_gate_authorisations (tenant_id, id) ON DELETE RESTRICT
);

-- Break-glass activation freezes (instance-wide; fail-safe; time-boxed; cannot be lifted early, s4.10).
CREATE TABLE screening_gate_activation_freezes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    frozen_until timestamptz NOT NULL, incident_reference text NOT NULL, reason_code text NOT NULL,
    frozen_session_user name NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (frozen_until > created_at)
);

-- New column on the policy table (ADD COLUMN NULL: no rewrite; legacy rows stay NULL; FORCED by the guard, never the caller).
ALTER TABLE aml_screening_trigger_policies ADD COLUMN gate_authorisation_id uuid NULL;
ALTER TABLE aml_screening_trigger_policies ADD CONSTRAINT aml_screening_trigger_policies_gate_authorisation
    FOREIGN KEY (tenant_id, gate_authorisation_id) REFERENCES screening_gate_authorisations (tenant_id, id) ON DELETE RESTRICT;
```

"Current authorisation" for (T, J) at time t, written C(T,J,t), is the latest `activation` row of (T, J) by `(created_at, id)` that
meets all of: `valid_from <= t < valid_until`; not covered by a revocation (`authorisation_id = id`, or `authorisation_id IS NULL`
with a revocation `created_at >= row.created_at`); `environment = rg_database_environment()`; and `system_identifier` and
`database_oid` equal to the live values.

### 4.1 The activation guard (the core change)

```sql
CREATE FUNCTION aml_screening_trigger_policies_activation_guard() RETURNS trigger
    LANGUAGE plpgsql VOLATILE            -- NOT SECURITY DEFINER: it runs as the inserting definer function (owner) or the owner
    SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER aml_screening_trigger_policies_20_activation_guard   -- fires after _10_guard (name order), BEFORE INSERT
    BEFORE INSERT ON aml_screening_trigger_policies FOR EACH ROW
    EXECUTE FUNCTION aml_screening_trigger_policies_activation_guard();
ALTER TABLE aml_screening_trigger_policies ENABLE ALWAYS TRIGGER aml_screening_trigger_policies_20_activation_guard;
```

Algorithm (every refusal is `AT004` with a closed HINT, so an old application maps it to its existing 400, F14):
1. `NEW.created_at := clock_timestamp()`. If the caller supplied a `gate_authorisation_id`, refuse
   (`gate_authorisation_stamp_forbidden`). It is always computed here.
2. If `NOT NEW.gate_withdrawal`, go to step 9 (non-gating rows are never refused by this guard; switching off stays governed by F7 and F8).
3. If `current_setting('transaction_isolation') <> 'read committed'`, refuse `gate_isolation_refused` (I11).
4. Take `pg_advisory_xact_lock_shared(K3)`, then `pg_advisory_xact_lock_shared(K4(T,J))` (s4.9). Every later read sees all
   revocations, freezes and settings committed before the locks were granted.
5. Find the latest row L (by `(created_at, id)`) and the in-force row F of (T, J). If `L.gate_withdrawal AND F.gate_withdrawal`, this is a
   **continuation**: set `NEW.gate_authorisation_id := <enabling row of F's run>.gate_authorisation_id`. If F's run is
   `environment_mismatch` or `unexplained`, and NEW is a loosening (`screening_trigger_cmp` against F says looser), refuse
   `gate_integrity_unresolved`. Otherwise go to step 9.
6. **New enabling.** Refuse unless all of the following hold, in this order (first failure wins, one closed code each):
   - `NEW.effective_from <= clock_timestamp()` → otherwise `gate_enabling_must_be_immediate` (E-6)
   - `aml_screening_gate_environment_ok()` → `gate_environment_refused` (existing code, I8)
   - a settings row for the anchored environment exists → `gate_activation_not_configured`
   - no freeze row with `frozen_until > now()` → `gate_activation_frozen`
   - no instance integrity problem (the p1 set of s11: `unexplained`, `environment_mismatch`, chain broken, guard not intact) → `gate_integrity_unresolved`
   - no other run of tenant T classified `legacy_unauthorised` → `gate_tenant_has_unauthorised_gate`
   - a **current instance attestation**: the latest attestation whose captured `system_identifier`, `timeline_id`,
     `database_oid` and `postmaster_start_time` all equal the live values, `environment` = the anchor, and `valid_until > now()` →
     `gate_instance_attestation_missing`
   - C(T,J,now) exists → `gate_authorisation_required`. If none is current but an expired, revoked or mismatched one exists, the code is
     sharper: `gate_authorisation_lapsed` / `gate_authorisation_revoked` / `gate_authorisation_identity_mismatch` /
     `gate_authorisation_environment_mismatch`
   - the authorisation's `worker_attestation_id` row has `valid_until > now()`, and a heartbeat of that provider is younger than 10
     minutes with the same `synthetic` flag → `gate_worker_attestation_missing` (the heartbeat alone is no longer enough, s5.3)
   - when the settings demand a persons cap: `count(DISTINCT person_id) FROM player_accounts WHERE tenant_id = T` <=
     `capacity_accepted_persons_max` → `gate_capacity_exceeded`
7. `NEW.gate_authorisation_id := C.id`.
8. (Defence in depth) If `NEW.created_by_scope = 'platform'` (a governed loosening), new enabling is refused anyway
   (`trigger_gate_enable_refused`, the existing code, F8).
9. `RETURN NEW`. The chain trigger (AFTER INSERT) records the row (s5.6).

The guard binds **the owner** too, while it is enabled. It refuses the owner-only setter (F6), fixtures over the owner pool,
and a hand-written `INSERT` by the owner. Only disabling or replacing it (owner DDL, detected, s5.5) or a superuser bypasses it.

### 4.2 Lifecycle table: effect on NEW activation and on an ALREADY-ACTIVE gate

| Event | Database behaviour | NEW enabling | ALREADY-ACTIVE gate | Alert / reconcile |
|---|---|---|---|---|
| Initial authorisation executed | one `activation` row; `valid_until = executed_at + validity` | allowed (s4.1 step 6) within validity | n/a | `aml.screening_gate_authorised` audit |
| Activation (officer, routed author, gate on) | guard stamps `gate_authorisation_id` | consumed? **No**: the authorisation is a permission for (T, J) during validity, not a single-use token. Re-enabling within validity is allowed (E-7) | n/a | audit `aml.screening_gate_enabled` |
| Expiry (`valid_until` passes) | nothing written | refused `gate_authorisation_lapsed` | **unchanged: keeps holding, nothing released** | `lapsed` p2 (staging) / p3 (dev, test); `expiring` p3 before the warning window |
| Revocation (single actor) | one revocation row | refused `gate_authorisation_revoked` | **unchanged**. Switching off needs governed loosening | `revoked` p2/p3 |
| Replacement (new activation for the same T, J) | new row. The latest current one wins for new enabling. Old rows stay (append-only) | allowed under the new one | runs keep their stamp. A run stamped with an old lapsed authorisation is `lapsed` until adopted (an adoption can name it) | `replacement` audit; reconcile lists runs on a superseded stamp |
| Anchor differs from recorded | nothing written | refused `gate_authorisation_environment_mismatch` + instance-wide `gate_integrity_unresolved` | unchanged | `environment_mismatch` p1 |
| Logical restore (new `system_identifier` or db oid) | nothing written | refused (`gate_authorisation_identity_mismatch`; no current attestation) | unchanged | `identity_mismatch` p2; `instance_attestation_stale` |
| Physical clone or failover (same identity) | nothing written | refused until re-attestation (postmaster start or timeline differs) | unchanged; classification stays `authorised` (residual R-3) | `instance_attestation_stale` p2 when any current authorisation exists |
| Approval racing a revocation | serialised by K4 (s4.9) | the later one wins: revoke-then-execute is refused `gate_authorisation_revoked_before_execution` | n/a | both audited |
| Worker down | heartbeat ages out | refused `gate_worker_attestation_missing` | unchanged (fail-closed: persons stay held; existing `aml.screening_gate_trapped` and the parked monitors fire) | `gate_without_attested_worker` p2 |
| Worker recovered | heartbeat fresh again | allowed again if the attestation is still valid | queue drains; releases happen only through the normal clear path (no bulk release) | alert clears on the next monitor pass |
| Break-glass revoke / freeze | revocation or freeze row (`source = 'break_glass'`) | refused | unchanged | p2 + audit chain row |

The rule in every row: **a lapsed, revoked, mismatched or frozen state never turns an active gate off and never releases a held
withdrawal. It blocks new enabling, raises an alert, and switching off needs the governed loosening.**

### 4.3 Expiry and the open transaction

The guard checks `valid_until > clock_timestamp()` at INSERT. An authorisation that lapses between INSERT and COMMIT (seconds) produces
a gate that is `lapsed` from its first second. That is accepted: it is visible and alerted. E-6 (enabling rows are immediate) prevents
the much larger window of a future-dated enabling row taking effect weeks after expiry or revocation.

### 4.4 Revocation

- Governed kind `screening_gate_revoke` is **narrowing**: 0 approvals, executes in the filing transaction (F18). Any platform_admin
  may file it, with no officer grant needed, because it is the fail-safe direction (the same rule as disabling the loosening capability,
  `0190:595-596`, `0191:435-436`). Payload `{tenant_id, jurisdiction_id, authorisation_id | null, reason_code, evidence_reference}`.
- Executor `screening_gate_revoke_apply(p_request uuid) RETURNS jsonb`: K3 shared, K4 exclusive, then insert one revocation row.
  Refuses `gate_authorisation_not_found` (wrong T, J or id) and `gate_authorisation_already_revoked` (idempotent refusal, nothing
  written).
- Revoking an authorisation never writes a policy row (I2).

### 4.5 Replacement

A replacement is a new `screening_gate_authorise` request for the same (T, J). It is two-person again: extending validity is a
widening decision. The executor does not revoke the old row. The predicate C(T,J,t) picks the latest current one. Shortening is done
by revoking and re-authorising.

### 4.6 Environment mismatch, logical restore

Covered by s3.7 and s4.2. Recovery path: (1) reconcile; (2) attestor re-attests the instance; (3) a governed `screening_gate_adopt`
per gate run that should stay classified `authorised`, or a governed gate-off; (4) a new `screening_gate_authorise` before any new
enabling. The database never re-binds an authorisation to a new identity by itself.

### 4.7 Physical clone (same `system_identifier`): residual and compensating control

A physical copy has the same `system_identifier`, database oid, anchor, authorisations and attestations. **The database cannot tell
a clone from the original.** Compensating controls:
1. **Instance attestation bound to the instance's life.** An attestation is current only while `pg_postmaster_start_time()` and
   `(pg_control_checkpoint()).timeline_id` equal the captured values. A clone must be started, which gives a new postmaster start
   time. PITR or promotion gives a new timeline. So every clone, restore, restart and failover makes new enabling impossible until a
   named operator holding the attestor role re-attests. Plain restarts also need re-attestation. This is accepted because it blocks only
   *new* enabling (a rare operation) and never touches active gates.
2. **Attestation expiry** (`instance_attestation_max_age_seconds`, OD-G8), so a forgotten attestation does not live for ever.
3. **External inventory** (operational, s5.7): each attestation is also recorded outside the database (ticket plus log shipping). Two
   live attestations of the same `system_identifier` from different `server_addr`, or an attestation for a host not in the
   inventory, is a P1 operations alert.
4. **Residual R-3:** gates that were already authorised in the source stay `authorised` in the clone. An operator who re-attests a
   clone without checking defeats control 1. Both are stated and accepted only with owner sign-off (OD-G8).

### 4.8 Worker failure and recovery

There is no new path. The existing fail-closed behaviour stays (F10: persons stay held). New enabling depends on worker attestation
plus a fresh heartbeat. Recovery never triggers bulk release: a person is released only by a fresh clear through the existing D10
rules (0192 to 0194). The monitor loop (s11) runs outside the worker.

### 4.9 Concurrency: lock keys and ordering

| Key | Expression | Taken by (mode) |
|---|---|---|
| K1 (existing) | `hashtextextended('screening_loosening:'\|\|tenant, 190)` | loosening validators and executors (excl.) |
| K2 (existing) | `hashtextextended('screening_trigger:'\|\|tenant\|\|jur, 191)` | routed author, loosen apply (excl.) |
| K3 (new) | `hashtextextended('screening_gate:global', 195)` | guard, authorise, adopt, revoke executors (**shared**); settings apply, instance and worker attestation, freeze, break-glass revoke-all (**exclusive**) |
| K4 (new) | `hashtextextended('screening_gate:'\|\|tenant\|\|':'\|\|jur, 195)` | guard (**shared**); authorise, adopt, revoke, break-glass revoke (**exclusive**) |
| K5 (new) | `hashtextextended('screening_gate:chain', 195)` | every chain append (**exclusive**, always last) |

**Global order: K1 → K2 → K3 → K4 → K5.** The guard runs inside the author (holding K2) or loosen apply (holding K1, K2) and adds K3s,
K4s, K5. The executors take K3, K4, K5 after the licensing-change flow's request row lock. No path takes K2 after K3 or K4. No path
takes K3 after K4. No path takes a request row lock while holding K3, K4 or K5. So the advisory locks cannot form a cycle. The
licensing-change request row lock comes before K3 on the executor paths and is never taken by the guard.

Serialisation results (all under READ COMMITTED, I11):
- **Author (enable) against revoke.** If the author holds K4 shared until COMMIT, revoke waits and then records a revocation after a committed
  enabling: the gate is active and `revoked` (fail-safe, alerted). If revoke commits first, the guard, after getting K4, sees it and
  refuses.
- **Approve/execute against revoke.** Both take K4 exclusive. If revoke runs first, the executor re-validates and refuses
  `gate_authorisation_revoked_before_execution` (a revocation with `authorisation_id IS NULL` created after the request was filed
  covers it).
- **Settings or attestation change against enabling.** K3 exclusive against K3 shared: a narrowing of settings or a new freeze is seen
  by every later enabling.
- **Chain.** K5 gives the chain a total order (low volume: configuration events only).

### 4.10 Break-glass (separate NOLOGIN role; never disables the trigger)

- Role `igaming_gate_breakglass` (NOLOGIN, NOINHERIT on grantees, no table privileges). It is **provisioned out of band** by a
  role administrator (s7.5). Migrations cannot create roles (F16), and no agent may (CLAUDE.md, environment safety). A named operator login is
  made a member for a time box (`VALID UNTIL` on that login, or membership granted and revoked by the documented procedure, OD-G7).
- Functions: SECURITY DEFINER, owned by the owner, EXECUTE granted **only** to `igaming_gate_breakglass`. Each checks
  `pg_has_role(session_user, 'igaming_gate_breakglass', 'MEMBER') AND NOT pg_has_role(session_user, '<owner>', 'MEMBER')`, so the
  owner cannot use break-glass as cover. Each requires `incident_reference` and `reason_code`, writes the row plus a chain entry,
  and returns the row id:
  - `screening_gate_breakglass_revoke(p_authorisation uuid, p_incident text, p_reason text)`: one authorisation; K3s, K4x.
  - `screening_gate_breakglass_revoke_all(p_incident text, p_reason text)`: one `authorisation_id IS NULL` revocation per (T, J)
    that has any authorisation; K3x.
  - `screening_gate_breakglass_freeze(p_until timestamptz, p_incident text, p_reason text)`: no new enabling anywhere until
    `p_until`; K3x. A freeze **cannot be lifted early** (lifting would be the widening direction). It lapses at `p_until`.
- **Not available through break-glass:** inserting or extending an authorisation, changing settings, enabling a gate, switching a gate
  off, releasing anything, disabling or replacing any trigger. Switching off a gate in an emergency stays the governed loosening or the
  existing owner setter path A1 (OD-G7 asks whether that is acceptable).

---

## 5. Item 3: trust boundaries

### 5.1 Every path that could enable a gate, and why it cannot without authorisation

The "today" column is verified at `01ea296`. The "after" column is this design.

| # | Path | Actor | Today | After this design |
|---|---|---|---|---|
| P1 | `POST .../screening/trigger-policy`, then `SetScreeningTriggerPolicy`, then `aml_screening_trigger_policy_author` | officer (runtime role) | enables a gate in development, staging and test with a quorum, a proof and a heartbeat (F7) | the guard (s4.1) refuses new enabling without C(T,J), a current instance attestation, a worker attestation, settings, no freeze and integrity |
| P2 | Direct `INSERT INTO aml_screening_trigger_policies` | runtime | `42501`: no table privilege (`0181:829-831`, re-asserted by `deploy/init-app-role.sql`) | unchanged; the guard would apply too |
| P3 | `aml_screening_trigger_policy_set` (owner setter) | runtime | `42501` since `0191:236-243` | unchanged |
| P4 | `aml_screening_trigger_policy_set` or hand-written INSERT | **owner** | allowed in development, staging and test (F6) | **refused by the guard** (fires for the owner). Bypass needs DDL (s5.5) |
| P5 | `screening_trigger_loosen_apply` | runtime, two Persons | never enables (F8) | unchanged, plus guard step 8 |
| P6 | New executors (`screening_gate_*_apply`) | runtime, governed | n/a | they write only evidence tables and **never** `aml_screening_trigger_policies` (writer pin, A-9) |
| P7 | Forge heartbeat (`screening_worker_heartbeat`) | runtime, proof-less (F2) | satisfies the heartbeat precondition | not enough: new enabling also needs an attestor-written `screening_worker_attestations` row (s5.3) |
| P8 | Forge `app.environment` GUC | any session | irrelevant: the anchor is used (F1) | unchanged |
| P9 | Write `database_environment` | runtime | no (SELECT only, `0135:1371-1377`) | unchanged |
| P10 | Write evidence tables directly | runtime | n/a | no privilege; FORCE RLS; admission guards refuse anything not written by an executor of an executed request **of this transaction** (LC040 pattern, `0191:75-127`) |
| P11 | Call an executor with no executed request | runtime | n/a | LC040 `licensing change: no executed ... request of this transaction and actor` |
| P12 | Mint two-Person approvals | **compromised API process** | the process holds the actor-proof signing keys (`cmd/platform-api/main.go:250`, ADR 0110) and can sign as any staff | **same residual** (R-5): the two-Person rule does not resist full compromise of the API process. Compensation: every authorisation raises `aml.screening_gate_attention` (`authorisation_created`, p3) to an out-of-band channel; audit with IP and session; external log shipping |
| P13 | Insert an instance or worker attestation | runtime | n/a | no EXECUTE on the attest functions; they require `session_user` to be a member of `igaming_gate_attestor` and **not** of the owner role |
| P14 | Disable, replace or drop the guard; `NO FORCE` RLS; TRUNCATE after disabling the deny trigger | **owner** | possible (owner of the objects) | **still possible.** Detected, not prevented (s5.5 to s5.7) |
| P15 | `session_replication_role = replica` | owner | not settable: superuser-only parameter (PGC_SUSET), unless `GRANT SET ON PARAMETER` (PG15+). The manifest pins that `pg_parameter_acl` grants nothing (s5.6 D4) | the guard and append-only triggers are `ENABLE ALWAYS` and fire in replica mode too |
| P16 | Anything | **superuser** | everything | everything. Detection only by logs the database cannot edit (D5) |

### 5.2 SECURITY DEFINER functions the runtime role can execute that write the policy or evidence tables (catalogue test)

The `tableWriters` detector (`internal/aml/screening_d7_integration_test.go:101-152`: direct DML, dynamic EXECUTE, transitive
callers) is extended with one pinned allow-list per table. A new runtime-executable writer then fails CI until a recorded
decision changes the pin:

| Table | Exact runtime-executable writers after the design |
|---|---|
| `aml_screening_trigger_policies` | `aml_screening_trigger_policy_author`, `screening_trigger_loosen_apply` (**unchanged**, F15) |
| `aml_screening_policies` | `aml_screening_policy_author`, `screening_policy_loosen_apply` (unchanged) |
| `screening_gate_settings` | `screening_gate_settings_apply` |
| `screening_gate_authorisations` | `screening_gate_authorise_apply`, `screening_gate_adopt_apply` |
| `screening_gate_revocations` | `screening_gate_revoke_apply` |
| `screening_gate_instance_attestations`, `screening_worker_attestations`, `screening_gate_activation_freezes`, `screening_gate_cutover`, `screening_gate_legacy_inventory` | **none** |
| `screening_gate_evidence_chain` | **none directly** (written by chain trigger functions, which have no runtime EXECUTE). The detector's transitive closure must list exactly the apply functions above plus the two policy writers. The test asserts that set |
| `screening_worker_heartbeats` | `screening_worker_heartbeat` (unchanged; phase 2 moves it, s5.3) |

Further catalogue assertions (A-9): each new table has `relforcerowsecurity`, no ACL entry for `igaming_runtime` or PUBLIC, and its deny
and guard triggers present with `tgenabled = 'A'`. Every new function has `proconfig` containing `search_path=pg_catalog, public,
pg_temp`. The break-glass and attest functions grant EXECUTE only to their NOLOGIN role (or to nobody if the role does not exist).

### 5.3 Worker health cannot be spoofed into an activation prerequisite

The problem (F2, F11): the heartbeat is runtime-executable and proof-less, and the worker runs in the API process under the same
credential. **Any** secret the worker could use to sign a heartbeat is also in the API process.

| Option | What it proves | Feasible now? | Decision |
|---|---|---|---|
| A. Owner/attestor-recorded **worker attestation** (`screening_worker_attestations`) required for new enabling, **plus** a fresh heartbeat of the same provider and synthetic flag | that a named operator stated "a worker of provider P, deployment D, serves this attested instance until T". The heartbeat proves liveness only | yes, without a provider | **Phase 1 (merge-blocking part of this design)** |
| B. Separate worker deployment with its own LOGIN role (`igaming_screening_worker`). `screening_worker_heartbeat`, claim and finish move to that role; the runtime role loses EXECUTE on the heartbeat | that the heartbeat came from the worker credential, not the API credential | needs a process split (`main.go:799-810`) and role provisioning (out of band) | **Phase 2: activation-blocking for any staging activation that relies on real data; required before production** (OD-G9) |
| C. HMAC-signed heartbeat with a key in the API process | nothing beyond A (same trust domain) | yes | **Rejected**: false assurance |
| D. Provider-signed liveness (vendor signs a platform nonce) | that the vendor is reachable with the tenant's credential | no: PROVIDER DEPENDENT | future; recorded with the provider attestation design (D1-CLOSURE-1) |

What spoofing still achieves under A: a runtime attacker who keeps the heartbeat fresh while the attested worker is dead can satisfy
liveness for **new enabling only within an existing worker attestation**. The consequence is a gate without a working worker.
That fails closed (persons held) and is detected by `aml.screening_gate_trapped` and by the parked monitors that run outside the
worker (`0181:1625-1628`). It does not weaken AML. It harms availability and is alerted.

### 5.4 Evidence cannot be altered by runtime roles

There are no table privileges (I5). FORCE RLS with an owner-only policy. Append-only triggers (`ENABLE ALWAYS`). Insert admission
only through executors bound to an executed licensing-change request of the same transaction, or to the attestor and break-glass
role checks. Every row is hash-chained (s5.6). Executor outputs are audited in the same transaction (s10). Reconcile cross-checks rows
against requests (s11).

### 5.5 Table owner and superuser: an honest account

**Triggers, RLS and append-only rules do not bind a determined owner or superuser.** What each can do against this design:

| Capability | Owner `igaming` (NOSUPERUSER, F16) | Superuser |
|---|---|---|
| `ALTER TABLE ... DISABLE TRIGGER` / `DROP TRIGGER` on its tables | **yes** | yes |
| `CREATE OR REPLACE FUNCTION` (replace the guard or an executor) | **yes** | yes |
| `ALTER TABLE ... NO FORCE ROW LEVEL SECURITY` / `DISABLE ROW LEVEL SECURITY` | **yes** | yes |
| `TRUNCATE` / `DELETE` evidence (after disabling the deny trigger) | **yes** | yes |
| Insert a fake authorisation or attestation (after disabling the admission guard) | **yes** | yes |
| Rebuild the hash chain consistently after tampering | **yes** (owns the chain table) | yes |
| Read actor-proof keys (`actorproof_private`, `0166`) and forge proofs | **yes** (owner of the schema) | yes |
| `SET session_replication_role` | no (superuser parameter; manifest pins no `GRANT SET ON PARAMETER`) | yes |
| Edit `pg_catalog` directly; `ALTER SYSTEM`; load extensions; `COPY ... PROGRAM` | no | yes |
| Create, alter or disable an **event trigger** | no (`CREATE EVENT TRIGGER` and ownership need superuser in PG16) | yes |
| Change roles and memberships | no (NOCREATEROLE) | yes |
| Avoid server logs shipped elsewhere | no | can change logging settings (itself logged before it takes effect, if shipping is synchronous); cannot edit what was already shipped |

So the in-database controls protect against **runtime roles and accidents**. Against the owner they provide **detection**, and
against the superuser **detection outside the database**.

### 5.6 Independent detection controls

| ID | Control | Defeats | Defeated by | Status in this design |
|---|---|---|---|---|
| D1 | **DDL event trigger** (`ddl_command_end`, `sql_drop`, `table_rewrite`) writing to schema `ddl_audit`, owned by NOLOGIN `igaming_ddl_audit_owner`. Function SECURITY DEFINER of that role; the owner has no privilege on the schema. It records `txid_current()`, `session_user`, `current_user`, `inet_client_addr()`, `application_name`, command tag, `object_identity`. Reconcile flags any event on a guarded object whose txid did not also record a `schema_migrations` row (`ddl_outside_migration`, p1). | owner DDL (DISABLE TRIGGER, CREATE OR REPLACE, NO FORCE RLS, DROP, GRANT/REVOKE) | superuser (can disable it); does **not** capture TRUNCATE, DML, role changes (global objects), SET or ALTER SYSTEM | **must be provisioned by a superuser out of band** (F21). Script `deploy/security/ddl-audit.sql` is a follow-up deliverable. Its absence is reported as `ddl_audit_missing` (p1 in staging/production, p3 in development/test). Not creatable by migrations |
| D2 | `ENABLE ALWAYS` on the guard, the admission guards and the deny triggers | `session_replication_role = replica` | owner `DISABLE TRIGGER` (caught by D1) | in the migrations. Data-only restores of these tables are unsupported. The backup tooling already bans `--disable-triggers` (`deploy/backup/backup_static_test.go:84`). Full `pg_dump` restores load data before creating triggers, so they are unaffected |
| D3 | **Hash chain** `screening_gate_evidence_chain(seq bigint GENERATED ALWAYS AS IDENTITY, at, source_table, source_id, txid, session_user name, current_user name, row_sha256, prev_hash, chain_hash)`. `chain_hash = sha256(prev_hash \|\| seq \|\| source_table \|\| source_id \|\| row_sha256 \|\| at)`. Appended by AFTER INSERT triggers on every evidence table **and on `aml_screening_trigger_policies`**. Genesis row written by migration 0195. `screening_gate_chain_verify() RETURNS bigint` (the first broken seq, or NULL) | silent edit or deletion of history | owner rebuilds the whole chain; superuser | in the migrations. **External anchoring of the head** (D3b): the monitor job copies `(max(seq), chain_hash)` to an external append-only store on every run; a head the store saw that no longer verifies is p1 |
| D4 | **Catalogue manifest**: the release ships `deploy/manifest/screening-gate.manifest`, generated from a clean migrate. Lines: `FN\|regprocedure\|md5(prosrc)\|prosecdef\|proconfig\|owner\|proacl`, `TRG\|table\|name\|tgenabled\|md5(pg_get_triggerdef)`, `RLS\|table\|relrowsecurity\|relforcerowsecurity`, `ACL\|table\|relacl`, `POL\|...`, `EVT\|evtname\|evtenabled\|md5(function)`, `PARAMACL\|session_replication_role\|<acl or none>`, using the same formulas as `deploy/backup/baseline.sql:145-162`. An **external** job computes the same lines with inline SQL (never through a database function the owner could replace), using a read-only monitor credential, and diffs them | owner function or trigger replacement; ACL drift | superuser who also feeds the job false data (needs the job's host) | tooling is a follow-up deliverable. An in-database **tripwire** `screening_gate_catalogue_check()` compares against `screening_gate_catalogue_baseline` (written by the migration). It catches accidents only and makes `screening_gate_authorise_apply` refuse `gate_guard_not_intact` |
| D5 | **pgaudit + log shipping** (`log_connections`, `log_disconnections`, `pgaudit.log = 'ddl, role, write'` for the owner role, `misc_set` for `session_replication_role`) to a store the database owner and the cloud database admin cannot edit (separate IAM, WORM retention) | owner and superuser actions, including event-trigger removal | compromise of the logging account | **operational requirement. NOT available or verified in this sandbox.** Activation-blocking for production (s16) |
| D6 | **Role separation**: `igaming` (migration only, credential reachable only from the migration pipeline), `igaming_runtime` (app), `igaming_backup` (BYPASSRLS read), `igaming_gate_attestor` and `igaming_gate_breakglass` (NOLOGIN, membership time-boxed to named operator logins), `igaming_ddl_audit_owner` (NOLOGIN), cloud superuser (break-glass of the cloud provider, MFA) | one credential doing everything | collusion | role provisioning is human-run (incident 2026-09-27 rule). Absent attestor role means activation is impossible (fails closed) |
| D7 | **Alert on owner login** outside a declared migration window (from D5 logs). PG17's `login` event trigger could add an in-database signal later; the sandbox is PG16 | silent owner sessions | log tampering (D5 covers) | operational requirement |
| D8 | **Key custody**: actor-proof keys are in the API config (`main.go:250`) and in `actorproof_private` (owner). Move signing to a KMS or HSM with per-Person keys (an ADR 0110 follow-up) | proof forgery by owner or API compromise (R-5) | KMS admin compromise | out of scope here; recorded residual and owner decision OD-G12 |

---

## 6. Item 4: governance

### 6.1 Decision: reuse the licensing-change primitive with new kinds

**Decision (engineering, s13 E-1): reuse** `licensing_change_requests` / `licensing_change_approvals`, with four new kinds,
following exactly the 0190 and 0191 recipe (a kind row, the classifier wrapper branch, the approvals officer trigger, an
`*_apply` executor admitted by LC040, a Go `register()`).

Reasons:
- It already has the closed state machine, `executed_txid` admission, approvals on the **payload hash** that are at most 24 h old by
  **distinct Persons** (never the requester), a 72 h expiry, and signed proofs on request, approve and execute through the
  existing `licensing_change:*` operations (F18). No new actor-proof operation or verifier branch is needed.
- The officer requirements (in-force `aml_officer` grant, owner-scope quorum, own-licence refusal) exist as reusable functions
  (`screening_loosening_officers_assert`, `screening_loosening_approvers_assert`, `0190:413-464`).
- The Back Office list, approve and reject screens and the audit trail of licensing changes already exist. Only the filing form is new.
- A table-owned workflow would duplicate about 600 lines of state machine and proof binding (more review surface, the same guarantees).

Costs, accepted: `licensing_change_classify` gets one more wrapper branch (the rename-and-wrap rule of `0190` stands). Kinds are
platform-only. The kind row's `widening_approvals` is static, so the owner's approval count is enforced additionally by the
executor (`>= settings.min_approvals`).

### 6.2 The kinds

| Kind | Classification | Approvals | Officer grants | Executor | Notes |
|---|---|---|---|---|---|
| `screening_gate_authorise` | always **widening** | kind row `widening_approvals = 2`, `approver_roles = {platform_admin}`; executor requires `>= settings.min_approvals` fresh counted officer approvals | requester, every counted approver and the executor: in-force platform `aml_officer` grant on T + owner-scope quorum (reuse `0190:413-464`) | `screening_gate_authorise_apply(uuid) RETURNS jsonb` | payload `{tenant_id, jurisdiction_id, validity_seconds, capacity_report_reference, capacity_report_sha256, capacity_accepted_persons_max, compliance_reference, legal_review_reference, evidence_reference, environment, system_identifier}`. All keys required (null where the settings allow). Approvers sign over the environment and instance identity |
| `screening_gate_adopt` | always widening | as above | as above | `screening_gate_adopt_apply(uuid)` | payload as above plus `policy_id`, `policy_row_sha256` (s3.6) |
| `screening_gate_revoke` | always **narrowing** | 0: executes when filed | none (any platform_admin; fail-safe) | `screening_gate_revoke_apply(uuid)` | s4.4 |
| `screening_gate_settings_configure` | widening iff it is the first row for the environment, or any parameter becomes looser (longer validity or ages, fewer approvals, a requirement switched off); otherwise narrowing | widening: 2 Persons, officer grants **not tenant-bound**: `platform_admin` holding any in-force `aml_officer` grant (OD-G4 may require more) | | `screening_gate_settings_apply(uuid)` | payload = every settings column + `evidence_reference` (the owner decision id); no default for any field |

Execute-phase validation (re-run under K3 and K4, never trusting the request phase):
- tenant and jurisdiction exist; J is in `aml_screening_jurisdictions(T)`; the tenant is configured for J (`tenant_jurisdiction_configs`, as `0191:200-212`)
- the tenant's owner scope is `platform`; otherwise `gate_authorisation_not_supported_for_tenant_licensing_model` (own-licence tenants are out of scope until OD-G5; the licensing model is not changed)
- the anchor is not production or unrecorded (`gate_authorisation_environment_refused`), and payload `environment` = anchor and `system_identifier` = live (`gate_authorisation_environment_mismatch` / `gate_authorisation_identity_mismatch`)
- a settings row for the environment exists (`gate_authorisation_not_configured`), and `validity_seconds <= max_validity_seconds` (`gate_authorisation_duration_exceeds_configured_maximum`)
- capacity and compliance fields are present when required (`gate_capacity_report_required`, `gate_compliance_reference_required`), and the current persons count is `<= capacity_accepted_persons_max` when a cap is required (`gate_capacity_exceeded`)
- a current instance attestation exists (`gate_instance_attestation_missing`); for activation, a current worker attestation (`gate_worker_attestation_missing`)
- no revocation of (T, J) with `authorisation_id IS NULL` was created after the request (`gate_authorisation_revoked_before_execution`)
- no instance integrity problem (`gate_integrity_unresolved`), and the tripwire passes (`gate_guard_not_intact`)
- the approvals: fresh, distinct Persons, officers, `>= min_approvals` (`officer_grant_required`, `officer_quorum_not_met`, reusing the existing codes; `gate_approvals_below_configured_minimum`)

### 6.3 Owner parameters (no shipped value; absent means refuse)

Stored in `screening_gate_settings` and written only by `screening_gate_settings_configure`. The values and options are listed
in s12. Nothing else in the design carries a business or legal value. The remaining numbers are structural or engineering
bounds already in the codebase (10-minute heartbeat age `0181:730`, 24 h approval freshness and 72 h request expiry `0177`,
30-day effective window `0191:163`), each named as such.

### 6.4 Audit and signed evidence

Every executor writes its audit row in the same transaction (the `auditLoosening` pattern, `screening_loosening_kinds.go`, last function)
and a chain row (D3). The proofs of request, approval and execution are the licensing-change proofs (bound to the payload hash),
so the approvers' signatures cover the instance identity and the capacity report digest.

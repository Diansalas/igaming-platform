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

# Screening activation gate: design v2.1 (GATE-B, architect)

- **Status: DESIGN ONLY. NOT IMPLEMENTED.** No migration, no code, no database access. Nothing here is a legal opinion, a
  compliance approval, production readiness or a provider validation. All providers stay MOCK.
- **Base:** branch `gate-design-v2` from the accepted integration tip `01ea296` (migration head **0194**).
- **v2.1 (2026-10-27):** revised in place after five independent reviews (security, DB architecture, ledger-finance, QA,
  operations). All five were ACCEPTABLE AFTER CHANGES, with no veto. Every finding has a disposition in **s18 (Review dispositions)**,
  which also says where the text changed. The biggest changes:
  - Advisory locks are replaced by row locks on an owner-only lock table (s4.9).
  - Evidence moves to platform-level tables, so a forced-RLS read can no longer fail open (s4.0).
  - Cancelling a pending gate-off is now a continuation, and a new column points at the enabling row (s3.1).
  - Triggers are split between `ENABLE ALWAYS` and ORIGIN, with an explicit stance on logical replication (s5.5).
  - Monitor and external verification independence is stated honestly (s5.6, s11).
  - The approval count is pinned to 2 (s6).
  - A tiered activation proposal is given as an owner decision (s19).
  - A full test and mutation plan is added (s15).
- **Replaces:** the first proposal of a database-enforced activation gate (D1 completion report s5 item 1 and s6,
  `d1-security-closure-record.md` s9.5 item 1 and s10.4, "CAPACITY-GATE-SWITCH"). That proposal left legacy gates in force
  and only flagged them.
- **Who decides what:** s12 (owner decisions, split into merge-blocking and activation-blocking), s13 (engineering choices) and s19
  (the tier definition, an owner decision) are kept separate. Owner parameters are stored as configuration with **no shipped
  value**. When a parameter is absent, the system refuses.

Reading order: s1 and s2, then s3 to s7 (the five owner-mandated items), then s8 onwards. s18 is the index of review dispositions.

---

## 1. Verified current state (file:line at `01ea296`)

Every statement below was checked by reading the cited file. Nothing was run against a database. Citations F2, F3, F12, F16
and F21 were corrected in v2.1 (review: security citation findings).

| # | Fact | Evidence |
|---|---|---|
| F1 | `database_environment` is the environment anchor. It holds one row (`singleton`). RLS is ENABLED but not FORCED, and the one read policy is `USING (true)`. A guard trigger refuses UPDATE, DELETE and TRUNCATE, and accepts an INSERT only from a member of the table owner. **No row means production** (`rg_database_environment()`). The runtime role has SELECT only. The guard binds the owner only while it is enabled, and it is ORIGIN-enabled (`tgenabled = 'O'`). | `migrations/0135_rg_limits_schema_and_index.up.sql:172-211`, `:189-192`, `:198-203`, `:210`, `:1371-1377` |
| F2 | Heartbeat table `screening_worker_heartbeats`: `0181:696-709`. Function `screening_worker_heartbeat(text, boolean)`: `0181:711-723`, SECURITY DEFINER, proof-less, upsert. `screening_worker_available()`: `0181:725-731`, a 10-minute engineering bound. The migration's own comment calls it a "configuration guard, not an attestation" (`0181:691-695`). It is **runtime-executable**: `0181:1882-1894` (the list entry at `:1884`), `deploy/init-app-role.sql:240`. The worker calls it at `internal/aml/screening_worker.go:187-198`. | as cited |
| F3 | `aml_screening_trigger_policies` (`0181:734-755`) is FORCE RLS with policy `aml_owner_session` on `aml_session_tenant()`, and the runtime role has no table privilege (`0181:819-832`). It is append-only: `aml_deny_mutation` on UPDATE and DELETE (`..._append_only`) and on TRUNCATE (`..._no_truncate`) (`0181:834-835`), both ORIGIN-enabled. The BEFORE INSERT `_10_guard` was created at `0181:857-858`. **Its function was replaced in `0191:75-127`**: platform-scope rows need an executed `aml_screening_trigger_policy_loosen` of the same transaction (LC040), and a governed loosening never enables a gate (`trigger_gate_enable_refused`, LC050). Any other row must carry the proven session actor (AT001). `0191:111-125` adds the `change_request_id` column and the scope constraint. | as cited (v2 cited only the stale `0181:842-858` body) |
| F4 | The policy **in force** for (tenant, jurisdiction) is the latest by `(created_at, id)` among rows with `effective_from <= now()`. The gate loops over every jurisdiction whose in-force row gates. The grace baseline `v_first` is the earliest `effective_from` of any gating row. | `0181:1128-1133`, `:1507-1517`; the same selection in `0192:120-`, `0193:12-`, `0194:32-` |
| F5 | `aml_screening_gate_environment_ok()` is true when the anchor is `development`, `staging` or `test`. | `0181:912-915` |
| F6 | `aml_screening_trigger_policy_set` (the 0181 setter) refuses a gate without a fresh heartbeat or under a production or unrecorded anchor. It is **owner-only** since 0191. | `0181:1060-1075`; `0191:236-243` |
| F7 | `aml_screening_trigger_policy_author` is TIGHTEN-ONLY against both the latest and the in-force version. It checks heartbeat and anchor for a gating version, takes the advisory lock `hashtextextended('screening_trigger:'\|\|tenant\|\|jur, 191)`, and is runtime-executable. **One officer chain can still enable a gate in development, staging and test.** | `0191:129-233`, `:173`, `:176`, `:185`; `deploy/init-app-role.sql:254` |
| F8 | `aml_screening_trigger_policy_loosen` (two Persons, LC040) runs through `screening_trigger_loosen_apply`. It takes the advisory key `screening_loosening:`\|\|tenant/190, then the trigger key/191. It never enables a gate, and it is gated by the **inert** capability `aml_screening_loosening_settings`. | `0191:248`, `:450-`, `:481`, `:484`; `0190:16-19`, `:49-` |
| F9 | Officer helpers (requester, counted approvers fresh within 24 h, distinct Persons, executor, owner-scope quorum of at least 2) refuse own-licence tenants. The narrowing direction stays open to any platform_admin. | `0190:387-464`, `:426`; `0190:595-596`, `0191:435-436` |
| F10 | The **gate predicate does not read** the anchor, the heartbeat or any authorisation. | `0181:1477-1577`, `0194:32-` |
| F11 | The screening worker runs **inside the API process**, under the runtime credential, unless `cfg.GuardEnvironment()=="production"` or no provider is set. | `cmd/platform-api/main.go:799-810` |
| F12 | `SetScreeningTriggerPolicy` (`internal/aml/screening_trigger_service.go:136-138`) calls the tighten-only author. `OwnerSetScreeningTriggerPolicy` (`:144-146`) calls the owner-only setter. `writeTriggerPolicy` is at `:148-181`, and its production refusal is at `:155-157`. | as cited |
| F13 | Route `POST /v1/admin/tenants/{tenantID}/screening/trigger-policy` (critical step-up, `PermAMLHoldManage`). | `internal/httpserver/aml_screening_routes.go:34-35` |
| F14 | Closed refusal codes come from `aml.RefusalCodes`, and an unknown HINT is ignored. **In the licensing-change path only `LC*` and `AP*` SQLSTATEs become a `GuardError`, and only `LC050` keeps its HINT** (`internal/licensingchange/service.go:255-262`). Anything else, AT004 included, falls through to `CodeInternal` (500) in `licensingChangeErrorCode` (`internal/httpserver/licensing_change_routes.go:110-142`). | as cited |
| F15 | A catalogue test pins the runtime-executable writers of `aml_screening_trigger_policies`. The detector does **not** follow triggers (a writer of table X whose trigger writes Y is not counted as a writer of Y). | `internal/aml/screening_d4_integration_test.go:135-136`; `internal/aml/screening_d7_integration_test.go:101-152` |
| F16 | Roles in development: owner `igaming` is NOSUPERUSER, NOCREATEDB, NOCREATEROLE, NOBYPASSRLS and owns the database and `public`, and `igaming_runtime` is non-owning (`deploy/init-app-role.sql:48-70`). **In staging the owner defaults to the RDS master user** (`DB_OWNER_USER="${DB_OWNER_USER:-$DB_MASTER_USER}"`, `deploy/staging-runner/lib.sh:219`), which on RDS has CREATEROLE and `rds_superuser` membership. The runtime-role safety checks (owns no tables, no memberships) run **only for production** (`internal/db/production_safety.go:115-167`). So the owner/runtime separation this design relies on is enforced in development and production, but **not in staging** today. | as cited |
| F17 | Catalogue-fingerprint prior art, and a static ban on `session_replication_role`, `DISABLE TRIGGER` and `NO FORCE` in the backup tooling. | `deploy/backup/baseline.sql:109`, `:145`, `:162`, `:208`; `deploy/drill/verify.sql:202`; `deploy/backup/backup_static_test.go:84` |
| F18 | The licensing-change primitive: request state machine, `executed_txid`, approvals on the payload hash (24 h, distinct Persons). **`required_approvals` is stamped from the kind row at filing**, and the final approval executes in its own transaction. Proof operations `licensing_change:*`. | `0177:604-673`; `internal/licensingchange/flow.go:25-27`, `:118-119`; `internal/actorproof/actorproof.go:158-162` |
| F19 | "No gate until capacity is accepted" is a **process control only**. | `docs/runbooks/aml-screening-triggers.md:230-234` |
| F20 | Open owner rows C4-LOOSEN-1 and D1-CLOSURE-1. ALERT-DELIVERY-1 is still OPEN (the email channel is MOCK; alert routing is by severity, not by kind or environment: operations review H1). | `docs/governance/human-decision-register.md:151`, `:154`; operations review |
| F21 | The sandbox server files are PostgreSQL 16. Its `system_functions.sql` contains no REVOKE for `pg_control_system()`, `pg_control_checkpoint()`, `pg_postmaster_start_time()` or `inet_server_addr()`, so they keep the default PUBLIC EXECUTE. **This must be confirmed on the target and on RDS.** `CREATE EVENT TRIGGER` needs superuser in PG16, so the migration role cannot create event triggers. PG16 has no time-boxed role membership and no `login` event trigger (PG17 adds the latter). | file inspection only; PostgreSQL 16 documentation |
| F22 | The migration runner applies each file in one transaction and then inserts `schema_migrations (version, description, checksum)`. **No txid is recorded.** | `internal/db/migrate.go:236-258` |
| F23 | Existing test fixtures disable `aml_screening_trigger_policies_10_guard` (and in one case `aml_screening_policies_10_guard`) to insert rows directly, then re-enable with plain `ENABLE TRIGGER`, which sets ORIGIN. | `internal/licensingchange/screening_trigger_loosening_integration_test.go:263`, `:613`; `internal/licensingchange/screening_loosening_integration_test.go:112`, `:250-262`; `internal/httpserver/licensing_change_screening_trigger_loosening_http_integration_test.go:30` |

**Gaps this design closes:** G1, one officer chain can enable a gate where the heartbeat is forgeable (F2, F7). G2, there is no
authorisation record. G3, existing gates have no classification. G4, owner and superuser changes leave no trace that the platform
checks. G5 (new in v2.1), the staging owner/runtime separation is not enforced (F16).

---

## 2. Design invariants (to be verified by `qa` and `code-reviewer` on the implementation; not yet verified)

| ID | Invariant | How it is pinned (acceptance criteria in s15) |
|---|---|---|
| I1 | **The gate predicate never consults authorisation state.** `aml_screening_gate_denies`, `aml_hold_active`, `aml_screening_enqueue_due`, `aml_screening_gate_overdue` and `aml_screening_parked_*` read no `screening_gate_*` object. | A-10 (static pin plus mutant) |
| I2 | **Nothing in this design writes a trigger-policy row that switches a gate off**, and nothing edits or deletes one. Switching off stays a governed loosening (F8) or the owner setter path A1 (F6). **Every gate-off path succeeds in every bad state** (frozen, unconfigured, unattested, unexplained, chain broken, tripwire failing, non-READ-COMMITTED). The guard's non-gating branch runs no check and takes only the policy-pair lock, and the chain append runs no check (ledger M-2). | A-21, A-9 |
| I3 | **No held withdrawal is released** by any new function. | A-9 writer pins on holds, lifts, requests, results, payouts and ledger |
| I4 | **Classification is derived, never stored** in a column any role writes at runtime. | A-9 |
| I5 | Evidence tables are owner-owned, FORCE RLS with an owner-only policy, append-only, and give the runtime role **zero** privileges. Deny triggers are `ENABLE ALWAYS`; insert guards and chain triggers are ORIGIN (s5.5). | A-9 |
| I6 | **Applying a migration or starting the service never creates** a settings row, an authorisation, an attestation, a freeze, a revocation or a gating row. The only rows migrations write are the chain genesis (0195), the cut-over row and the inventory (0198), and the lock-table seed rows. | A-17 |
| I7 | **When configuration is absent, new enabling is refused.** | A-3, A-4 |
| I8 | Production and unrecorded anchors still refuse gate enabling (`aml_screening_gate_environment_ok()` unchanged). | existing tests + A-4 |
| I9 | Triggers are not claimed to stop the table owner or a superuser. | s5.5 wording review |
| I10 | Sanctions, PEP, hold and needs_officer behaviour is untouched. | A-9 |
| I11 | The guard's gating branch and the executors run under READ COMMITTED only. | A-8 |
| I12 | The licensing and operating-basis model is untouched, and no jurisdiction or country literal appears in a migration. | A-19 grep pin |
| I13 (new) | **No runtime-callable session can block a governed gate-off.** Every serialisation uses row locks on the owner-only `screening_gate_locks` table, which the runtime role cannot lock directly. Advisory keys are no longer used (s4.9). | A-7, A-22 |
| I14 (new) | **No cross-tenant check may fail open.** A read over FORCE-RLS tenant tables is made tenant by tenant with `app.tenant_id` saved and restored, and is asserted non-vacuous against a platform-level count. A failed assertion refuses or reports p1 (s4.0). | A-23 |

---

## 3. Item 1: existing unauthorised gates, restored databases and mismatched authorisations

### 3.1 Vocabulary (v2.1: continuation follows the gate in force; security M1, ledger M-1)

For one (tenant T, jurisdiction J), L is the latest row by `(created_at, id)` and F is the row in force now (F4).
- **gating row**: `gate_withdrawal = true`.
- **New enabling**: inserting a gating row while F is **not** gating (or there is no F). This covers the first gate, re-enabling after a
  gate-off took effect, and enabling while the only gating row is pending.
- **Continuation**: inserting a gating row while F **is** gating, whatever L is. A gating row that **cancels a pending governed
  gate-off** (L non-gating, F gating) is therefore a continuation. It is allowed in every classification state (keeping a gate on
  never weakens screening), and it inherits F's run.
- **Run identity is an explicit pointer, not a position.** New column `gate_enabling_policy_id uuid NULL`, forced by the guard. On an
  enabling row it holds the row's own id. On a continuation it holds `F.gate_enabling_policy_id`, or for a legacy F (pointer NULL)
  the legacy enabling row resolved from the inventory (s3.2). Legacy rows keep NULL. The enabling row of any post-cut-over gating row
  is one indexed lookup.
- **Enabling rows are immediate** (E-6): `effective_from <= clock_timestamp()`. Continuations may be future-dated (up to 30 days, as
  today). This changes the UI and E2E for gate-on (s7.6, A-24).
- **Monotonic `created_at` per (T, J)** (operations M5): the guard sets
  `NEW.created_at := GREATEST(clock_timestamp(), max(created_at of (T,J)) + interval '1 microsecond')`. "Latest" therefore stays
  consistent across a host clock step backwards. It never refuses: a gate-off must not fail because of the clock.

A shared helper `screening_trigger_policy_pair(p_tenant uuid, p_jur uuid, OUT latest ..., OUT in_force ...)` (owner-only, STABLE,
DB L2) replaces the duplicated "latest / in force" queries in the author, the loosen validator and apply, the guard and classify.
0197 rewrites the existing functions to call it (s7.1).

### 3.2 Cut-over inventory (no data rewrite)

Migration 0198 (the guard migration, s7.1) takes `LOCK TABLE aml_screening_trigger_policies IN SHARE ROW EXCLUSIVE MODE` under
`SET LOCAL lock_timeout = '5s'` (DB M9), then writes:

```sql
CREATE TABLE screening_gate_cutover (                     -- platform-level; one row; written only by 0198
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    applied_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    cutover_nonce uuid NOT NULL DEFAULT gen_random_uuid(),  -- signed into every authorisation payload (security M5)
    system_identifier bigint NOT NULL,                      -- bigint, as pg_control_system() returns it (DB M6)
    database_oid oid NOT NULL,
    environment text NOT NULL,                              -- rg_database_environment() at cut-over
    inventory_rows integer NOT NULL,
    inventory_sha256 text NOT NULL CHECK (inventory_sha256 ~ '^[0-9a-f]{64}$'),
    legacy_pending_rows integer NOT NULL                    -- legacy gating rows with effective_from > applied_at (security M2)
);
CREATE TABLE screening_gate_legacy_inventory (            -- platform-level (no tenant RLS policy: owner-only USING (true))
    policy_id uuid PRIMARY KEY, tenant_id uuid NOT NULL, licensing_jurisdiction_id uuid NOT NULL,
    gate_withdrawal boolean NOT NULL, effective_from timestamptz NOT NULL, created_at timestamptz NOT NULL,
    legacy_enabling_policy_id uuid NULL,                    -- for a gating row: the enabling row of its run, by position, computed ONCE here
    row_sha256 text NOT NULL,                               -- canonical encoding s4.0
    FOREIGN KEY (tenant_id, policy_id) REFERENCES aml_screening_trigger_policies (tenant_id, id) ON DELETE RESTRICT
);
```

- The inventory is read from the FORCE-RLS policy table **tenant by tenant** (set_config save and restore, the `0190:494`
  pattern). Its total is then asserted equal to a count taken with RLS not in the way (the migration runs as owner, and the count is
  taken per tenant and summed against `SELECT count(DISTINCT tenant_id)` over the inventory itself). A mismatch aborts the migration
  with `GA098` (I14).
- **Legacy pending gating rows** (security M2): pre-cut-over gating rows with `effective_from > now()` would take effect later
  with no authorisation. The pre-flight lists them (ids). By default 0198 **refuses to apply** while any exist (`GA098`,
  `legacy_pending_enabling_present`), unless the owner decision OD-G13 chose "apply and classify". With that option they are
  classified `legacy_pending_enabling` (p1 in staging and production). Remedy per the runbook: wait until they take effect (at most 30 days)
  and adopt or switch off, or neutralise them with a governed loosening before applying.
- Legacy status comes from **membership in the inventory**, never from a timestamp. `created_at` was a caller-suppliable default
  (F3) until 0198 forces it.
- Write-once: the insert guard on both tables refuses any insert once `screening_gate_cutover` has a row.
  `inventory_digest_mismatch` (p1) is reported if the recomputed digest differs.

### 3.3 Classification (derived, read-only; v2.1: on the run's enabling row)

`screening_gate_classify(p_tenant uuid DEFAULT NULL)`: SECURITY DEFINER, STABLE, `search_path` pinned. With `p_tenant` NULL it is
platform_admin only. Otherwise the caller must be in the session tenant (AT002) or a platform session. It reads tenant by tenant
with save and restore, and asserts the rows read per tenant equal the platform-level inventory and stamp counts (I14). For each (T, J),
it takes the run R of F if F gates, else of the latest pending gating row, and resolves R's **enabling row** E (from
`gate_enabling_policy_id`, or for legacy runs from `legacy_enabling_policy_id`). The state is evaluated **on E** (security M3).
The first matching row of this table wins:

| Precedence | State | Definition | Severity (staging/production; development/test: see s11) |
|---|---|---|---|
| 1 | `unexplained` | E is not in the inventory, **and** E is not stamped with an `activation` authorisation of the same (T, J) that was valid at `E.created_at`. Or a post-cut-over gating row's `gate_enabling_policy_id` is NULL or points at another (T, J) | p1 |
| 2 | `environment_mismatch` | the authorisation's `environment` is not equal to the anchor now (an **unrecorded anchor after a restore reads as production** and lands here first, operations M3) | p1 |
| 3 | `identity_mismatch` | the authorisation's `system_identifier` or `database_oid` is not equal to the live value | p2 |
| 4 | `revoked` | the authorisation (or adoption) is covered by a revocation (by sequence, s4.4) | p2 |
| 5 | `lapsed` | `valid_until <= now`, not revoked | p2 |
| 6 | `legacy_pending_enabling` | E is in the inventory with `effective_from > cutover.applied_at` and has no current adoption | p1 |
| 7 | `legacy_unauthorised` | E is in the inventory, with no current adoption naming E | p1 |
| 8 | `authorised` | an activation authorisation stamped on E, or a current adoption naming E; not revoked, not lapsed, identity and environment matching | none |
| 9 | `none` | neither F nor any pending row gates | none |

**Honesty (security M3).** `unexplained` catches a guard bypass only where the bypass left a visible inconsistency. An owner who
disables the admission guards and inserts a matching authorisation **and** a correctly stamped gating row produces a gate that
classifies `authorised`. In-database classification cannot detect that. Detection relies on the **external** controls (s5.6):
D1 (DDL log, `DISABLE TRIGGER` outside a declared window), D3b (the external chain head shows evidence inserted out of band), and
D5 (log shipping).

A gate row is always stricter than no row. No state is ever fixed by removing a gate automatically.

### 3.4 Containment: what remains, what is refused

| Operation | `authorised` | `lapsed` / `revoked` | `identity_mismatch` | `legacy_unauthorised` / `legacy_pending_enabling` | `environment_mismatch` / `unexplained` |
|---|---|---|---|---|---|
| Gate keeps holding; held withdrawals stay held | yes | yes | yes | yes | yes |
| Tightening continuation (routed author) | allowed | allowed | allowed | allowed | allowed |
| Cancel a pending governed gate-off (continuation, M1) | allowed | allowed | allowed | allowed | allowed |
| Loosening continuation that keeps the gate on (governed longer alert threshold) | allowed | allowed | allowed | allowed | **refused** `gate_integrity_unresolved` (LC050 on that path, s4.1) |
| Governed gate-off (F8) or owner setter gate-off (A1) | allowed in every state (I2). **A gate-off is a RELEASE decision** that needs the withdrawal-release policy OD-G10 (ledger L-1). The remedy the runbook and alerts point to is **adopt or replace**, not switch off | ← | ← | ← | ← |
| New enabling of this (T, J) | needs a current activation authorisation | same | refused until re-attested and re-authorised | needs activation, **and** no other legacy run of T | refused |
| New enabling of another J of the same T | normal | normal | normal | **refused** `gate_tenant_has_unauthorised_gate` | refused |
| New enabling of any tenant | normal | normal | normal | normal | **refused instance-wide** `gate_integrity_unresolved` |
| Adoption | n/a | allowed | allowed after re-attestation | allowed (a pending legacy row only once it is in force) | refused |

The instance-wide integrity set **checked inside the guard** (security L2) contains only states a runtime role cannot cause: `unexplained`,
`environment_mismatch`, `guard_not_intact`. States a runtime actor can influence (capacity growth through registrations, lapses,
chain verification results written by the in-process monitor) are reported by the monitor but never block other tenants.

### 3.5 Alerts

Three alert kinds (the 0181 seeding pattern, `0181:672-689`): `aml.screening_gate_integrity` (p1),
`aml.screening_gate_authorisation` (p2), `aml.screening_gate_attention` (p3). Allowed keys: `problem`, `count`, `runbook`.
**The discriminator is the problem code: one open alert per problem code** (operations H1). Payloads carry codes, counts and the
runbook anchor only. Severity per anchored environment is s11. **In development and test every problem is capped at p3** (no
paging: tests tamper on purpose). Delivery limits (ALERT-DELIVERY-1 OPEN, F20) are part of the tier proposal (s19).

### 3.6 Adoption

Kind `screening_gate_adopt`. Its payload is that of `screening_gate_authorise` (s6.2) plus `policy_id` (must be the **enabling row E**
of the run in force) and `policy_row_sha256` (canonical encoding, s4.0). The executor `screening_gate_adopt_apply(uuid)` re-validates
everything and inserts one `mode = 'adoption'` authorisation. It never writes a policy row. Refusals: `gate_adoption_target_invalid`
(not the enabling row of the run in force; a legacy pending row before it takes effect), `gate_adoption_digest_mismatch`,
`gate_adoption_target_unexplained`. Adoption needs no worker attestation (E-13). An existing gate with no worker is reported
`gate_without_attested_worker` (p2) instead. This exception stays on the reviewer attack list.

### 3.7 Restored or cloned databases

| Case | Authorised runs | Legacy runs | New enabling |
|---|---|---|---|
| PITR or promotion in the same lineage (same `system_identifier`, same db oid, new timeline) | `authorised` | legacy | refused until re-attestation (timeline or postmaster start differs) |
| Physical clone | `authorised` (**residual R-3**) | legacy | refused until re-attestation |
| Logical restore, new cluster or new database, **anchor restored** | `identity_mismatch` | legacy | refused |
| Logical restore with the **anchor left unrecorded** (operations M3) | `environment_mismatch` (an unrecorded anchor reads as production), p1 | legacy | refused instance-wide. Procedure order: **record the anchor first (owner), then attest, then adopt** |
| Restore of a pre-0198 dump, then `up` | n/a | all gating runs legacy | after configuration and attestation |
| Restore target with a pre-provisioned, different anchor | `environment_mismatch` | legacy | refused instance-wide |
| Blue/green major-version upgrade over logical replication (RDS) | the green instance has a new `system_identifier`: `identity_mismatch` after switchover | legacy | refused until re-attested; adoption per gate (s5.5 replication stance) |

After every restore, the drill and runbook run `deploy/drill/verify.sql` additions (chain verify, classify, reconcile; operations M4).
The external chain head may be **ahead** of a restored database after PITR. That is reported `chain_behind_external_head` (p2) and needs
an incident record stating the RPO gap (operations M4, the RPO/head-ahead rule).

Production databases **must** record `production` explicitly (E-12). `anchor_unrecorded` is p2. Correcting an anchor is impossible in
place (it is write-once): the procedure is a new database restored with the correct anchor recorded **before** any data load (operations I2).
A test pins that a production-configured `platform-api` refuses to start against a non-production anchor (A-25).

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
- tenant and jurisdiction exist; J is in `aml_screening_jurisdictions(T)`; the tenant is configured for J (`tenant_jurisdiction_configs`, as `0191:206-224`)
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

---

## 7. Item 5: migration strategy (0195+)

### 7.1 Sequence (expand, then contract)

| Step | Content | Effect on behaviour |
|---|---|---|
| **R1 (application release first)** | `aml.RefusalCodes` and the licensing-change refusal table gain every new code (s9). OpenAPI enums and Back Office texts too. Go registers the four kinds and **feature-detects** them (`to_regprocedure('public.screening_gate_authorise_apply(uuid)')`). If absent, filing answers `gate_authorisation_unavailable` (409) and the monitor loop is a no-op. Fixtures (`internal/testsupport/amlfix`) and E2E spec 10 learn to create settings, attestations and authorisations **through the real paths** | none |
| **0195_screening_gate_evidence** | pre-flight `GA098` (refuses if any object exists or 0194 is not applied). Creates settings, instance attestation, worker attestation, authorisation, revocation, freeze, chain (plus genesis row) and catalogue-baseline tables with their guards, deny triggers (`ENABLE ALWAYS`) and chain triggers. Creates attest and break-glass functions (EXECUTE granted only if the NOLOGIN roles exist, else to nobody). Creates `screening_gate_chain_verify`. Seeds the three alert kinds. `ADD COLUMN gate_authorisation_id` (NULL, no rewrite) plus FK | **inert**: no row except the chain genesis; the policy table is not guarded yet |
| **0196_screening_gate_governed_kinds** | inserts the four kinds into `licensing_change_kinds` (NO FORCE / FORCE dance as `0191:247-250`). Wraps `licensing_change_classify` (keeps every existing branch, the 0190 rule). Extends `licensing_change_approvals_screening_officer` to the widening kinds. Creates the four executors and the executed-request reconcile part | inert: every widening request refuses `gate_authorisation_not_configured` (no settings) |
| **0197_screening_gate_activation_guard (contract)** | `LOCK TABLE aml_screening_trigger_policies IN SHARE ROW EXCLUSIVE MODE`. Writes `screening_gate_cutover` and `screening_gate_legacy_inventory` (snapshot, digest). Installs `_20_activation_guard` (`ENABLE ALWAYS`) and the chain trigger on the policy table. Creates `screening_gate_classify`, `screening_gate_reconcile`, `screening_gate_monitor_summary`, `screening_gate_catalogue_check` and the baseline rows | **new enabling is refused everywhere** until configured, attested and authorised. Existing gates are unchanged (classified, alerted) |
| R2 (application) | monitor loop on; read and reconcile routes; Back Office classification badge and filing forms | alerts start |
| Later slice (phase 2, not this one) | `igaming_screening_worker` role and worker process split; move heartbeat, claim and finish EXECUTE | needs human role provisioning |

**Classification is derived; no data is rewritten.** No existing row of `aml_screening_trigger_policies` (or any other table) is
updated. Legacy rows keep `gate_authorisation_id = NULL`. The inventory is a new table.

### 7.2 Rollback (down): exactly when it is refused

| Down | Refused (`GA095`) when | Otherwise |
|---|---|---|
| 0197 | the anchor is `production`, `staging` or unrecorded (removing the guard weakens security); OR any row exists in settings, authorisations, revocations, attestations or freezes; OR any policy row has `gate_authorisation_id IS NOT NULL`; OR any policy row was created after `screening_gate_cutover.applied_at` | drops the guard, the policy-table chain trigger, classify, reconcile, monitor, the catalogue check and baseline, cutover and inventory. Restores the 0194 state byte for byte |
| 0196 | anchor not `development`/`test`; OR any `licensing_change_requests` row of the four kinds exists (decision records would be lost, the `0191` down precedent) | restores the 0195 classifier wrapper and approvals trigger; drops kinds and executors |
| 0195 | anchor not `development`/`test`; OR any chain row beyond genesis; OR any evidence row; OR any policy row with a non-NULL stamp | drops everything 0195 created, including the column |

This is stricter than `0191` down (development **and staging**): here staging is refused, because staging is where authorisations
will be real decisions.

### 7.3 Mixed application versions

| Combination | Behaviour | Acceptable? |
|---|---|---|
| Old app (pre-R1), schema 0197 | gating author call: `AT004` + unknown HINT, so 400 without a check code (F14 ignores unknown hints); nothing written. Licensing-change kinds unknown to Go: 400. Monitor absent: **no alerts** | yes for safety (fails closed). **No** for alerting, so R1 must precede 0197 (the order above) |
| R1, schema 0194 | features detected absent: `gate_authorisation_unavailable`; the gate behaves as today (**G1 still open**) | transitional only |
| R1/R2, schema 0197 | target state | yes |
| Rolling deploy of the worker | no worker signature changes in 0195 to 0197 | yes |

### 7.4 No activation by migration or service start (the pin)

Test A-17 (integration, `cmd/migrate` and `internal/aml`):
1. Clean `migrate up` to head, then: `count(*)` of settings, authorisations, revocations, instance attestations, worker
   attestations and freezes is 0. The chain holds the genesis row only (plus one row per pre-existing policy row in an upgrade). There is
   no gating policy row.
2. Upgrade from a 0194 fixture holding N gating runs: after `up`, the gating rows are byte-identical to before (`row_sha256` equal),
   their count is unchanged, there is no stamp, and every run classifies as `legacy_unauthorised`. A fixed decision table of
   `aml_screening_gate_denies(t, p, 'withdrawal')` over the fixture population is **identical before and after** (I1, I3).
3. Static test: migration files `0195+` contain no `INSERT INTO` any settings, authorisation, attestation, freeze or revocation table,
   and no call to an `_apply`, attest or break-glass function. `cmd/` and `internal/` production files reference the `_apply` functions
   only from `internal/licensingchange` and never call the attest or break-glass functions.
4. Start-up: the E2E harness boots `cmd/platform-api` on a fresh database; after start, counts are as in step 1. The monitor
   loop may have raised alerts, but it wrote no evidence.

### 7.5 `deploy/init-app-role.sql`

- Add to the runtime EXECUTE list: `screening_gate_authorise_apply(uuid)`, `screening_gate_adopt_apply(uuid)`,
  `screening_gate_revoke_apply(uuid)`, `screening_gate_settings_apply(uuid)`, `screening_gate_classify(uuid)`,
  `screening_gate_reconcile()`, `screening_gate_monitor_summary()`.
- Re-assert on every run (the pattern of `deploy/init-app-role.sql:261-269`, `:890-895`): `REVOKE ALL` from `igaming_runtime` and
  PUBLIC on every new table and on the attest, break-glass, guard, chain and catalogue-check functions.
- **No role is created in this file for production.** Optional `deploy/security/init-gate-roles.sql` (attestor, break-glass,
  ddl-audit owner, all NOLOGIN) is for a role administrator. Agents never run it (CLAUDE.md environment safety, incident
  2026-09-27). In CI it is applied by the CI database definitions under human-approved change.

### 7.6 OpenAPI and UI

- `AMLScreeningRefusalCode` gains the guard codes (s9). The licensing-change refusal schema (the list around
  `docs/api/openapi/platform-api.yaml:13060`, `13101`) gains the kind codes. Descriptions containing `: ` are quoted, the full YAML
  is validated, and the backoffice and b2c vitest OpenAPI contract tests are run.
- New read routes (platform session; `PermAMLCaseRead`; no person data):
  `GET /v1/admin/tenants/{tenantID}/screening/gate-authorisations?jurisdiction_id=` (classification plus a summary of the current
  authorisation) and `GET /v1/admin/platform/screening/gate-reconcile` (platform_admin; ids and codes).
- Back Office: a classification badge on the trigger-policy panel, and filing forms for the four kinds (activation-blocking, not
  merge-blocking: the licensing-change API can file them).

### 7.7 Runbook (`docs/runbooks/aml-screening-triggers.md`, a new section)

Covers: the order (owner decisions, then settings via governed kind, then roles provisioned, then instance attestation, then worker
attestation, then `screening_gate_authorise`, then officer enables); after-restore and after-restart procedure (s3.7, s4.7);
legacy gate handling (adopt or governed off; never by hand); break-glass procedure; reading the reconcile output; and the operational
requirements D1, D3b, D4, D5, D7 with their owners.

### 7.8 Verification matrix

| ID | Scenario | Expected |
|---|---|---|
| V1 | clean setup + `verify` | OK; A-17 step 1 |
| V2 | upgrade from a database at 0194 with legacy gates | A-17 step 2; inventory = all rows; digest recorded |
| V3 | fingerprints (functions, ACLs, comments, constraints, indexes, columns, kinds, triggers with `tgenabled`) | upgraded-from-0194 == clean setup; `down 3` == the 0194 database byte for byte (development anchor, empty evidence); `up` again == first up |
| V4 | down refusals | `GA095` under production, staging and unrecorded anchors; `GA095` with any evidence row; `GA095` with a stamped row |
| V5 | restored database: (a) `pg_dump` into a new database of the same cluster; (b) a pre-0197 dump restored then `up` | (a) `identity_mismatch` for authorised runs, legacy stays legacy, new enabling refused; (b) all gating runs legacy |
| V6 | concurrent author vs revoke, approve vs revoke (barrier-driven, both orders, 100 iterations) | serialised as s4.9; no deadlock; no enabling after a committed revocation |
| V7 | old app vs new schema | 400, no write, no check code |
| V8 | new app vs old schema | `gate_authorisation_unavailable`, gate unchanged |
| V9 | non-READ-COMMITTED enabling | `gate_isolation_refused` |
| V10 | owner tamper drills (test cluster only): disable guard then insert gating row; edit an authorisation after disabling its deny trigger; replace the guard body | reconcile: `gate_unexplained`, `evidence_chain_broken`, `guard_not_intact` (tripwire); with D1 provisioned: `ddl_outside_migration` |
| V11 | Playwright spec 10 updated (authorisation through the API by two officers, attestation by the harness's attestor) | green, no fixme |
| V12 | guard cost at 10 000 persons (persons-cap count) | measured and recorded; it is not on the withdrawal path (enabling only) |

---

## 8. Threat model

| # | Asset | Attacker | Path | Control | Residual |
|---|---|---|---|---|---|
| T1 | player withdrawal availability (funds not trapped by a premature gate) | compromised or careless officer; runtime credential holder | routed author (P1), owner setter (P4) | guard + two-Person authorisation + instance and worker attestation + capacity cap + settings | R-5: full API-process compromise forges approvals (P12) |
| T2 | AML/sanctions coverage (a gate silently off) | owner; insider | owner setter gate-off; DDL | I2; governed loosening; chain (D3); DDL log (D1); manifest (D4) | owner path A1 remains a single-officer loosening (accepted earlier, D1 report s5 item 5) |
| T3 | authorisation evidence integrity | owner | disable deny trigger, edit or delete; rebuild chain | D1, D3 + D3b external head, D4, D5 | superuser; external anchoring only as good as its operation |
| T4 | environment separation (production data or gates in staging) | operator restore | restore into another environment | anchor + identity binding + attestation + `anchor_unrecorded` | a production database without a recorded anchor (E-12) |
| T5 | instance identity | operator clone | storage snapshot | postmaster-start/timeline-bound attestation, expiry, external inventory | R-3: already-authorised gates stay `authorised` in a clone |
| T6 | activation prerequisites | runtime | heartbeat spoof | worker attestation (phase 1), worker role (phase 2) | spoofed liveness within a valid attestation; availability only, alerted |
| T7 | activation availability | break-glass holder | freeze or revoke all | fail-safe only; audited; time-boxed membership; separate role | activation denial of service until `frozen_until` |
| T8 | two-Person rule | colluding platform admins | grant each other `aml_officer`, then authorise | officer grants, quorum, audit, `authorisation_created` p3 alert | existing collusion residual (D1 report s5 item 4); OD-G4 |
| T9 | governance of legacy gates | insider | adopt an unexplained (tampered) gate | digest binding; `unexplained` refused | none beyond T3 |
| T10 | consistency | concurrent actors | approve vs revoke; author vs revoke | K3/K4 + READ COMMITTED | a lapse between INSERT and COMMIT (seconds, alerted) |
| T11 | tenant isolation | tenant staff | classify or read other tenants | session-tenant bound (AT002); platform-only for all-tenant reads | none new |
| T12 | platform metadata | runtime | `screening_gate_monitor_summary` | counts per problem code only, no ids | aggregate counts visible to the runtime role |
| T13 | deploy window | operator | old app with new schema | guard is in the database; fail-closed | no alerts until R2 (ordering rule) |
| T14 | security posture | operator | down migrations | `GA095` outside development/test or with evidence | none |
| T15 | everything | superuser | catalog edits, disable event triggers, `session_replication_role` | D5 log shipping to a store it cannot edit; cloud MFA | **cannot be prevented in the database** |

## 9. New closed refusal codes

| Code (HINT) | SQLSTATE | Raised by | HTTP |
|---|---|---|---|
| `gate_authorisation_required` | AT004 | activation guard | 400 (existing AT004 map) |
| `gate_authorisation_lapsed` | AT004 | guard | 400 |
| `gate_authorisation_revoked` | AT004 | guard | 400 |
| `gate_authorisation_identity_mismatch` | AT004 / LC050 | guard / executors | 400 / 409 |
| `gate_authorisation_environment_mismatch` | AT004 / LC050 | guard / executors | 400 / 409 |
| `gate_instance_attestation_missing` | AT004 / LC050 | guard / executors | 400 / 409 |
| `gate_worker_attestation_missing` | AT004 / LC050 | guard / authorise executor | 400 / 409 |
| `gate_activation_not_configured` | AT004 | guard | 400 |
| `gate_activation_frozen` | AT004 | guard | 400 |
| `gate_integrity_unresolved` | AT004 / LC050 | guard / executors | 400 / 409 |
| `gate_tenant_has_unauthorised_gate` | AT004 | guard | 400 |
| `gate_enabling_must_be_immediate` | AT004 | guard | 400 |
| `gate_isolation_refused` | AT004 | guard, executors | 400 |
| `gate_capacity_exceeded` | AT004 / LC050 | guard / executors | 400 / 409 |
| `gate_authorisation_stamp_forbidden` | AT004 | guard | 400 |
| `gate_authorisation_not_configured` | LC050 | executors | 409 (existing LC050 map) |
| `gate_authorisation_not_supported_for_tenant_licensing_model` | LC050 | executors | 409 |
| `gate_authorisation_environment_refused` | LC050 | executors | 409 |
| `gate_authorisation_duration_exceeds_configured_maximum` | LC050 | executors | 409 |
| `gate_capacity_report_required` | LC050 | executors | 409 |
| `gate_compliance_reference_required` | LC050 | executors | 409 |
| `gate_approvals_below_configured_minimum` | LC050 | executors | 409 |
| `gate_authorisation_revoked_before_execution` | LC050 | authorise and adopt executors | 409 |
| `gate_adoption_target_invalid` | LC050 | adopt | 409 |
| `gate_adoption_target_unexplained` | LC050 | adopt | 409 |
| `gate_adoption_digest_mismatch` | LC050 | adopt | 409 |
| `gate_guard_not_intact` | LC050 | executors (tripwire) | 409 |
| `gate_authorisation_not_found` | LC050 | revoke | 409 |
| `gate_authorisation_already_revoked` | LC050 | revoke | 409 |
| `gate_settings_environment_refused` | LC050 | settings | 409 |
| `gate_authorisation_unavailable` | n/a (Go) | R1 feature detection | 409 |
| `attestor_role_required`, `breakglass_role_required`, `breakglass_incident_reference_required`, `attestation_validity_out_of_range` | GA001-GA004 | owner-side functions (no HTTP) | n/a |
| down refused / pre-flight | GA095 / GA098 | migrations | n/a |

Reused unchanged: `gate_environment_refused`, `trigger_gate_enable_refused`, `officer_grant_required`, `officer_quorum_not_met`.
The exact AT004 and LC050 HTTP mapping must be confirmed against `internal/aml` and `internal/licensingchange/refusal.go` during
implementation. The table above assumes the existing maps (AT004 gives 400, LC050 gives 409) and changes neither.

## 10. Audit events

| Event (action) | Written by | Where | Content |
|---|---|---|---|
| `aml.screening_gate_authorised` / `aml.screening_gate_adopted` / `aml.screening_gate_authorisation_revoked` / `aml.screening_gate_settings_configured` | Go kind executors (`auditLoosening` pattern) | `audit` (same transaction) + chain | change_request_id, payload_hash, the executor's jsonb outcome (ids, validity, identity), reason code, IP, request id |
| licensing-change request, approve, reject, cancel, execute, expire | existing flow | existing | unchanged |
| `aml.screening_gate_enabled` | trigger-policy route (existing staff mutation audit) | audit | adds `gate_authorisation_id` to metadata |
| `aml.screening_gate_enable_refused` | route denied audit | audit | the closed code |
| instance attested, worker attested, break-glass revoke, revoke all, freeze | database functions (no staff session) | evidence row + chain row (`session_user`, `inet_client_addr()`, `application_name`) + D5 logs | ticket or incident reference, reason |
| reconcile and monitor runs | monitor loop | alerts (s3.5); external head export (D3b) | problem codes and counts |

## 11. Reconciliation contract

```sql
-- Detailed (platform_admin session only; LC030 otherwise). Read-only. Ids and codes only, no person data.
CREATE FUNCTION screening_gate_reconcile(
    OUT o_severity text, OUT o_problem text, OUT o_tenant_id uuid, OUT o_jurisdiction_id uuid,
    OUT o_policy_id uuid, OUT o_authorisation_id uuid, OUT o_request_id uuid, OUT o_chain_seq bigint)
RETURNS SETOF record LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp;

-- Summary for the monitor loop (runtime-executable, proof-less, no ids).
CREATE FUNCTION screening_gate_monitor_summary(OUT o_problem text, OUT o_severity text, OUT o_count integer)
RETURNS SETOF record LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp;
```

Closed problem vocabulary (severity column: staging/production anchor ; development/test anchor):

| Problem | Severity | Meaning |
|---|---|---|
| `gate_unexplained` | p1 ; p1 | s3.3 state 1 |
| `gate_environment_mismatch` | p1 ; p1 | state 2 |
| `gate_legacy_unauthorised` | p1 ; p2 | state 6 |
| `evidence_chain_broken` | p1 ; p1 | `screening_gate_chain_verify()` not NULL |
| `guard_not_intact` | p1 ; p1 | tripwire (guard or deny trigger missing, not `A`, body digest differs) |
| `inventory_digest_mismatch` | p1 ; p1 | inventory rows or digest differ from `screening_gate_cutover` |
| `evidence_row_without_request` / `executed_request_without_row` | p1 ; p1 | an authorisation, revocation or settings row without an executed request of its kind, or the reverse (the `0190:494` pattern) |
| `ddl_outside_migration` | p1 ; p2 | D1 event on a guarded object outside a migration transaction (only when D1 is provisioned) |
| `ddl_audit_missing` | p1 ; p3 | D1 event trigger absent or disabled |
| `gate_identity_mismatch` | p2 ; p2 | state 3 |
| `gate_authorisation_lapsed` / `gate_authorisation_revoked` | p2 ; p3 | states 5 and 4, with the gate active |
| `gate_without_attested_worker` | p2 ; p3 | a gate active and no current worker attestation or heartbeat |
| `gate_capacity_exceeded` | p2 ; p3 | the tenant's persons count is above the cap of the run's authorisation (never switches the gate off) |
| `instance_attestation_stale` | p2 ; p3 | no current attestation while a current authorisation exists |
| `anchor_unrecorded` | p2 ; n/a | no `database_environment` row |
| `gate_authorisation_expiring` | p3 ; p3 | within `expiry_warning_seconds` |
| `authorisation_created` / `adoption_created` / `revocation_created` / `freeze_active` | p3 ; p3 | informational, out-of-band visibility (R-5) |

Monitor loop: `aml.RunScreeningGateMonitor` is started in `cmd/platform-api/main.go` **outside** the worker branch
(`main.go:799`), in every environment including production. Its interval is an engineering configuration value, not a
compliance value. It raises the s3.5 alert kinds from the summary, and it exports the chain head (D3b) when the external store is
configured. It never writes evidence or policy rows.

---

## 12. Decisions requiring OWNER approval (not engineering)

None of these has a shipped value. Absent means refuse.

| ID | Decision | Options | RECOMMENDATION |
|---|---|---|---|
| OD-G1 | Maximum authorisation validity and expiry warning window, per environment | (a) 30 days; (b) 90 days; (c) 180 days; no-expiry is not offered | (b) 90 days for staging, a warning 14 days before. Development and test as the owner prefers. Short enough that capacity and compliance evidence is revisited |
| OD-G2 | Production activation policy | (a) no production activation until a real provider (PROVIDER DEPENDENT), the phase 2 worker split, D1, D3b, D4 and D5 in production, a withdrawal-release policy, and a separate future migration lifting I8; (b) per-tenant production authorisation with an owner signature in addition; (c) never | **(a)**. This design adds no production path |
| OD-G3 | Capacity acceptance: is a report required, is a persons cap required, which figures | (a) report + cap required in staging and production; (b) report only; (c) neither in development/test | (a) for staging and production, with figures from the CAPACITY plan load test (keyset batching, priority lanes) once built. The figures themselves are the owner's |
| OD-G4 | Compliance requirements and approver population | min approvals 2 or 3; `aml_officer` only, or an MLRO-type role; compliance reference required or not | 2 counted officer approvals besides the requester, compliance reference required, approvers from the platform officer population (unchanged trust model) |
| OD-G5 | Own-licence tenants | (a) keep refused; (b) design tenant-scope officer approval | (a) until a separate design (as C4-LOOSEN-1 D5) |
| OD-G6 | Treatment deadline for legacy gates | (a) adopt or governed-off within an owner-set period, alert until then; (b) tolerate indefinitely with p2 | (a). **Never** an automatic switch-off |
| OD-G7 | Break-glass: holders, time box, scope | (a) revoke, revoke-all and freeze only; (b) also an emergency gate-off | (a). Emergency gate-off stays the governed loosening or the documented owner setter A1 |
| OD-G8 | Instance attestation: maximum age, attestor population, acceptance of residual R-3 | 7, 30 or 90 days; 1 or 2 attestors | 30 days, two named operators, R-3 accepted in writing |
| OD-G9 | When the phase 2 worker separation is required | before staging with real persons; before production only | before any production activation and before staging with non-synthetic persons |
| OD-G10 | Withdrawal-release policy when a gate is switched off (already open, D1-CLOSURE-1) | not invented here | unchanged: decide before activation |
| OD-G11 | Alert routing; legacy severity in development/test | | p1 pages in staging/production; development/test to a channel |
| OD-G12 | External log and chain-head store (D3b, D5); key custody (D8) | cloud WORM log, separate account; KMS signing | WORM store in a separate account; KMS follow-up ADR |

## 13. Engineering choices made here (reversible; for reviewers)

E-1 reuse licensing change (s6.1). E-2 classification derived, never stored. E-3 legacy by inventory membership, not timestamp. E-4
guard codes on `AT004` and kind codes on `LC050` for old-app compatibility; new class `GA` only for non-HTTP functions. E-5 `ENABLE
ALWAYS` on guards and deny triggers. E-6 enabling rows immediate. E-7 an authorisation is reusable within validity (not a single-use
token). E-8 continuation means both the latest and the in-force rows gate. E-9 keys K3 to K5 and the global order. E-10 attestor and
break-glass are NOLOGIN roles distinct from the owner; the owner is explicitly excluded. E-11 attestation bound to postmaster start
and timeline. E-12 explicit production anchor in provisioning plus `anchor_unrecorded`. E-13 adoption exempt from worker attestation.
E-14 monitor loop in every environment. E-15 the guard is SECURITY INVOKER (runs as the definer function or the owner). E-16
containment is per tenant; instance-wide only for integrity states. E-17 the guard is a separate `_20_` trigger; the `_10_` guard is
not copied or changed.

## 14. Open questions

1. Are `pg_control_system()`, `pg_control_checkpoint()` and `pg_postmaster_start_time()` executable by the owner on the target
   (and on RDS)? If not, the attest function refuses (fail closed) and the design needs another instance signal.
2. Are event triggers available to the managed-service admin role (RDS `rds_superuser`)? To be verified, not assumed.
3. The migration runner must expose the txid of each applied file (for example a `txid` column on `schema_migrations`) for D1
   correlation. Today it does not, as far as read here.
4. How does the E2E harness obtain an attestor? It needs a human-approved role in the E2E cluster definition.
5. Persons count for the capacity cap: all persons with an account, or active only? An owner and capacity-plan question.
6. When a gate is re-enabled after off, the backfill grace is **not** re-granted (`v_first` is the earliest gating version, F4).
   This is unchanged, but compliance should know it before authorising a re-enable.
7. Should tenant-scope staff see the classification of their own tenant's gate? Platform-only in this design.

## 15. Acceptance criteria for the implementation (all merge-blocking)

- **A-1** The guard refuses new enabling without a current authorisation on each of: the routed author, the owner setter over the owner
  pool, and a raw owner INSERT. Each refusal has its closed code and writes nothing.
- **A-2** A legacy run: a tightening continuation is accepted. New enabling of another jurisdiction of the same tenant is refused
  `gate_tenant_has_unauthorised_gate`. Adoption flips the classification to `authorised` and changes no gate decision.
- **A-3** With no settings, enabling is refused `gate_activation_not_configured` and authorising `gate_authorisation_not_configured`.
- **A-4** Each owner parameter, when missing or violated, produces its code (duration, capacity report, cap, compliance reference,
  min approvals).
- **A-5** Lapse: the decision table of `aml_screening_gate_denies` over a fixed population is identical before and after expiry. New
  enabling is refused `gate_authorisation_lapsed`. Reconcile reports it.
- **A-6** Revocation: as A-5 with `gate_authorisation_revoked`. Single actor, executes when filed.
- **A-7** Concurrency (V6): both orders of author vs revoke and approve vs revoke, deterministic, no deadlock in 100 iterations.
- **A-8** Isolation refusal (V9).
- **A-9** Catalogue pins of s5.2 (writers, FORCE RLS, ACLs, `tgenabled = 'A'`, search_path, EXECUTE grants), each with a
  non-vacuity premise as in `screening_d7_integration_test.go:171-175`.
- **A-10** I1 static pin: the gate and monitor functions reference no `screening_gate_` object (mutation-killed).
- **A-11** A forged heartbeat with no worker attestation is refused `gate_worker_attestation_missing`.
- **A-12** Instance attestation currency is tested through a pure helper that takes live values as an argument (a restart cannot be
  simulated in CI), plus one end-to-end positive path.
- **A-13** Restore (V5a) into a private database of the same cluster gives `identity_mismatch`.
- **A-14** Adoption binds `policy_row_sha256`. Adopting an `unexplained` run is refused.
- **A-15** Reconcile and monitor return every problem code of s11 from a fixture that produces it. Tamper fixtures run as the owner on a
  private test database only (V10).
- **A-16** `screening_gate_chain_verify()` detects an edited, deleted and inserted row.
- **A-17** No activation by migration or start (s7.4, all four steps).
- **A-18** Migration matrix V1 to V5, V7, V8, including fingerprints and every down refusal.
- **A-19** Parity: every HINT raised in 0195 to 0197 is in `aml.RefusalCodes` or the licensing-change refusal table, and in the OpenAPI
  enum (a static test in the style of the existing lic29 parity).
- **A-20** Independent `security` review (blocking), `ledger-finance` sign-off that I1 to I3 hold, `qa` gate. Full local integration
  suite, hosted CI and Playwright (spec 10 updated) all green. No assertion weakened. Intentional pin changes listed (the
  trigger-policy writer pin must stay **unchanged**).

## 16. What blocks MERGING vs what blocks ACTIVATING

**Blocks merging the implementation:** A-1 to A-20, the runbook section, the init-app-role changes, the OpenAPI and RefusalCodes
parity, and the reviews. Merge does **not** need the new roles, D1, D5 or any owner decision. Without them the mechanism is
inert and fails closed: no settings means no authorisation, and no attestor role means no attestation, so no new gate anywhere. That
is safe to merge because it is strictly more restrictive than `01ea296`.

**Blocks ACTIVATING (per environment):** owner decisions OD-G1, G3, G4, G6, G7, G8, G11 recorded and entered through
`screening_gate_settings_configure`. Roles provisioned by a role administrator (attestor, break-glass; ddl-audit owner for
staging). D1 event trigger, D4 external manifest job and D3b external head in staging. Every legacy gate of the tenant adopted or
governed-off. The capacity work and accepted figures (CAPACITY plan). Back Office filing forms. **For production, additionally:**
OD-G2, OD-G9 (phase 2 worker separation), OD-G10, OD-G12, D5 log shipping, a real provider and its attestation (PROVIDER
DEPENDENT), the sanctions false-positive policy, and a separate future migration that lifts I8. None of these is authorised by this
document.

## 17. Design choices independent reviewers should attack

1. **E-6 (enabling rows immediate)** changes the routed author's behaviour for gate-on: no scheduled activation. Is any compliance
   need lost? Does it break the backfill-grace expectations of operators?
2. **E-8 continuation definition.** Can any sequence of future-dated continuations, governed loosenings and re-tightenings
   produce a gating in-force row in a run that never had an authorised enabling row, without the guard seeing "new enabling"?
3. **Adoption exempt from worker attestation (E-13).** Is a governed adoption of a gate with no worker a disguised approval of trapped
   funds?
4. **Containment scope (E-16).** Should a legacy gate freeze the whole instance, not just the tenant? Or is per-tenant already too
   much for development databases full of test gates?
5. **The guard is SECURITY INVOKER** and reads the authorisations under the inserting role. Verify that every inserting path (author,
   setter, loosen apply, raw owner) can read the evidence tables under FORCE RLS with the tenant GUC as set at that point. A read
   returning zero rows is a refusal (fail closed), but could produce a wrong code.
6. **Lock design.** Is K3 shared/exclusive enough to make settings narrowing and freezes visible to an enabling already past step 4?
   (It is designed so an enabling that holds K3 shared commits first and the freeze follows. Is that acceptable?)
7. **Postmaster-start binding** forces re-attestation after every restart. Will operators re-attest blindly (making R-3 worse) or
   find it too costly? Is timeline plus start time the right signal?
8. **Persons-cap count** in the guard and executors: a `count(DISTINCT person_id)` over `player_accounts` per enabling. Cost is fine
   for a rare operation, but is the right population counted (OQ5)?
9. **R-5:** the two-Person rule is only as strong as actor-proof key custody, and the API process holds the keys. Is the p3
   `authorisation_created` alert to an out-of-band channel enough until KMS (OD-G12)?
10. **Down migrations refused in staging.** Does that block legitimate staging rollbacks in a way that tempts operators toward manual
    DDL (which D1 would then flag)?
11. **Reuse of the `screening_loosening_*` officer helpers** for authorisation: their names and their own-licence refusal code are
    loosening-specific. Is reusing them a source of wrong refusal codes, and do they need generic wrappers?
12. **Monitor summary** is runtime-executable and cross-tenant (counts only). Is that an acceptable disclosure surface?

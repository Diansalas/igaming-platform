# Screening activation gate: design v2.3 (GATE-B, architect)

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
- **v2.2 (2026-10-27):** revised after five confirmation reviews of v2.1. All five were ACCEPTABLE AFTER CHANGES with no veto, and the earlier Highs are fixed. The second disposition table is s18.2. The main changes:
  - No lock row is taken before actor-proof or LC040 admission, and a privileged watchdog bounds lock holding (s4.9).
  - `inserted_at` is separated from the ordering `created_at` (s3.1).
  - A replica-mode refusal trigger is added (s5.5).
  - Role separation is extended and made consistent with the tier options (s5.5, s19).
  - Authorisations are bound to the tier (s19).
  - The lock-key CHECK defect is fixed (s4.0).
  - Fingerprint and decision-table pins are added for 0197 (s7.1).
  - Guard-level wiring tests and break-glass criteria are added (s15).
  - A relative implementation-size estimate is added (s21).
- **v2.3 (2026-10-27):** folds in the final checks of security (N8, N9) and DB architecture (V1-V8). Dispositions are in s18.3. The main changes:
  - The pre-existing configure executor re-classifies under its lock.
  - Filing-time validation and under-lock re-validation are one function per kind.
  - The watchdog detection is specified.
  - `inserted_at` is a NULL column with no CHECK (v2.3 final: the NOT VALID CHECK was dropped, see s3.1).
  - `last_created_at` is initialised lazily.
  - The replica-refusal triggers are renamed to sort first.
  - The 0198 pause mechanism is specified.
  - The 0197 down rule is relaxed.
  - The gate-off wait wording is bounded.
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
| I2 | **Nothing in this design writes a trigger-policy row that switches a gate off**, and nothing edits or deletes one. Switching off stays a governed loosening (F8) or the owner setter path A1 (F6). **No gate-off is refused because of a bad state** (frozen, unconfigured, unattested, unexplained, chain broken, tripwire failing, non-READ-COMMITTED). The guard's non-gating branch runs no check and takes only the policy-pair lock, and the chain append runs no check (ledger M-2). A gate-off **can** still wait for, or fail with retryable `55P03`/`40001` on, a lock held by an admitted concurrent writer. That wait is bounded by `lock_timeout` and by the watchdog (s4.9); it is not a refusal. | A-21, A-9 |
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
| I13 (v2.2 wording) | **A runtime session cannot take a lock that a governed gate-off needs without first passing actor-proof or LC040 admission**, and any lock held by a runtime backend is **bounded by the watchdog** (s4.9). Serialisation in this slice uses row locks on the owner-only `screening_gate_locks` table, which the runtime role cannot lock directly, and filing-time validators take no lock. Advisory keys are removed **from the functions of this slice** only. The platform-wide advisory-lock squatting class (21 migrations, e.g. `residence_policy:` at `0181:373`, `:406`) is a **separate review item** (SEC-N1-PLATFORM). | A-7, A-21, A-22 |
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
- **Ordering time vs wall time** (v2.2, security N2; operations M5; ledger N-2):
  - `created_at` is **for ordering only**. The guard sets `NEW.created_at := GREATEST(clock_timestamp(), COALESCE(last_created_at, max(created_at) of the pair) + 1 µs)` (v2.3, DB V2: lazy initialisation for pairs whose lock row is new while rows already exist; the max is read **under** the pair lock; the 0198 pre-flight bounds existing rows to at most 2 minutes ahead), where
    `last_created_at` is kept **on the `policy:<T>:<J>` lock row**. The guard updates it under `FOR UPDATE`, so a concurrent commit makes a
    REPEATABLE READ or SERIALIZABLE gate-off fail with retryable `40001` instead of silently landing below a gating row (ledger N-2).
  - A new column `inserted_at timestamptz NULL` (**no CHECK**; v2.3 final, DB V1), forced to `clock_timestamp()` by the guard, is the **wall time**. Legacy rows are **not** backfilled: the append-only trigger forbids UPDATE, and a NOT NULL column with a volatile default would rewrite the table.
    - **Why the CHECK was dropped:** a NOT VALID CHECK is still enforced on every new row, including replica apply. A logical-replication subscriber's initial tablesync, or a data-only restore, would therefore fail on legacy rows whose `inserted_at` is NULL.
    - **Why not the alternative:** a legacy-exemption marker on the row would need a column on legacy rows (UPDATE forbidden), or would be backdatable. Inventory membership already **is** the non-backdatable legacy marker, but a CHECK cannot reference another table.
    - **What enforces it instead:** the guard forces the value. Reconcile reports any post-cut-over row (not in the inventory) with NULL `inserted_at` as `unexplained` (p1). **Every validity test uses
    `inserted_at`** (classification "valid at `E.inserted_at`", stamps, lapse), never `created_at`. So one clamped row far in the future
    cannot make later enabling rows classify `unexplained`.
  - Engaging the clamp (`created_at > clock_timestamp()`) leaves a trace: the chain row carries `clamped = true`, and reconcile reports
    `clock_regression_clamped` (p2) and `created_at_ahead_of_clock` (p2) while any row's `created_at` is more than 2 minutes ahead of the
    clock (an engineering tolerance).
  - The 0198 pre-flight refuses (`GA098`) when any existing row has `created_at > now() + 2 minutes`.
  - It never refuses a gate-off because of the clock.

A shared helper `screening_trigger_policy_pair(p_tenant uuid, p_jur uuid, OUT latest ..., OUT in_force ...)` (owner-only, STABLE,
DB L2) replaces the duplicated "latest / in force" queries in the author, the loosen validator and apply, the guard and classify.
0197 rewrites the existing functions to call it (s7.1).

### 3.2 Cut-over inventory (no data rewrite)

Migration 0198 (the guard migration, s7.1) takes `LOCK TABLE aml_screening_trigger_policies IN ACCESS EXCLUSIVE MODE` (v2.2; was SHARE ROW EXCLUSIVE, see s7.1) as its first statement, under
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
  pattern). For non-vacuity, the migration also takes an **unfiltered** count inside the same transaction: `ALTER TABLE ... NO FORCE
  ROW LEVEL SECURITY`, then `count(*)`, then `FORCE` again. This follows the precedent of the `0191` down migration, and the owner does
  not bypass a FORCE policy otherwise. The per-tenant total must equal it; otherwise the migration aborts with `GA098` (I14). The
  temporary NO FORCE happens under the ACCESS EXCLUSIVE lock, inside the migration transaction, and is recorded by D1 as migration DDL.
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
| 1 | `unexplained` | E is not in the inventory, **and** E is not stamped with an `activation` authorisation of the same (T, J) that was valid at `E.inserted_at` (wall time; v2.2, security N2). Or a post-cut-over gating row's `gate_enabling_policy_id` is NULL or points at another (T, J) | p1 |
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

**Non-vacuity source of truth after cut-over (DB N2).** The platform-level counts that tenant-by-tenant reads are checked against are:
the policy-stream chain rows per (T, J) (platform-level, one per post-cut-over policy row) plus the inventory rows (pre-cut-over).
A mismatch gives `gate_integrity_read_inconsistent` (fail closed in the guard; p1 in reconcile). The cost is O(tenants) per evaluation,
which is acceptable for enabling and for the monitor pass. `app.tenant_id` is restored **exactly**, including the empty string.
**Caveat (v2.3, DB):** a policy row inserted with the chain trigger disabled makes the read look inconsistent. It then blocks new enabling
and reports p1 `gate_integrity_read_inconsistent` instead of `unexplained`. That is fail-closed and accepted.

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

### 4.0 Evidence tables, lock table, canonical encoding (v2.1)

**Common rules for every evidence table:**
- Owner `igaming`, `ENABLE` + `FORCE ROW LEVEL SECURITY`, **one policy `TO <owner> USING (true) WITH CHECK (true)`**.
- **Platform-level, no tenant RLS predicate** (security H2, DB H2/H3). The runtime role has no privilege at all, so a tenant
  predicate added no protection and made instance-wide reads silently return zero rows (fail open). Tenant isolation for reads is
  enforced in the read functions (AT002).
- `REVOKE ALL ... FROM PUBLIC, igaming_runtime`.
- Append-only: deny triggers on UPDATE, DELETE and TRUNCATE, set `ENABLE ALWAYS`.
- A BEFORE INSERT admission guard (ORIGIN).
- An AFTER INSERT chain trigger (ORIGIN; it does no checking, s5.6).
- `SET LOCAL lock_timeout = '5s'` in every migration that creates them (DB M9).

**Canonical encoding** (security M4, DB M7), used by `row_sha256`, the inventory, adoption digests and the chain:
- An **explicit column list** per table, never `row::text`. **The lists are versioned (v2.3, DB V1).** The **legacy inventory digest** (`row_sha256` at cut-over) covers the **original 0181/0191 columns only**. The **post-cut-over chain** encoding of policy rows also includes `gate_authorisation_id`, `gate_enabling_policy_id` and `inserted_at`. Each encoding carries its list version in the type tag (`policy.v1` / `policy.v2`), so the two are never confused.
- Each field is encoded as `<octet_length>:<value>;`, so the encoding is length-prefixed and unambiguous.
- Timestamps are `to_char(v AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`. Integers use `::text`. Booleans are `t`/`f`.
  Arrays are sorted and each element is length-prefixed. NULL is encoded as `-1:;`.
- Every hashing function pins `SET TimeZone = 'UTC'`, `SET DateStyle = 'ISO, YMD'`, `SET IntervalStyle = 'iso_8601'` and
  `SET extra_float_digits = 3` in `proconfig`. No float column is hashed.

```sql
-- Owner decisions as configuration. Append-only versions per anchored environment; the latest wins; NO ROW SHIPS.
CREATE TABLE screening_gate_settings (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq bigint NOT NULL,                                            -- from screening_gate_event_seq (below)
    environment text NOT NULL CHECK (environment IN ('development', 'staging', 'test')),   -- never production (I8)
    tier text NOT NULL CHECK (tier IN ('S', 'N')),                  -- s19, OD-G0 (owner decision)
    role_separation text NOT NULL CHECK (role_separation IN ('enforced', 'waived_tier_s')),  -- s5.5/s19 option S-b; CHECK (tier = 'S' OR role_separation = 'enforced')
    s_to_n_verifier_record_sha256 text NULL,                        -- s19: external verifier countersignature (self-asserted to the DB)
    max_validity_seconds bigint NOT NULL CHECK (max_validity_seconds >= 3600),  -- OD-G1; 3600 = engineering floor (QA H-4)
    expiry_warning_seconds bigint NOT NULL CHECK (expiry_warning_seconds >= 0),
    capacity_report_required boolean NOT NULL, capacity_persons_cap_required boolean NOT NULL,  -- OD-G3
    compliance_reference_required boolean NOT NULL,                 -- OD-G4
    instance_attestation_max_age_seconds bigint NOT NULL CHECK (instance_attestation_max_age_seconds > 0),  -- OD-G8
    instance_attestation_confirmations integer NOT NULL CHECK (instance_attestation_confirmations BETWEEN 1 AND 2), -- OD-G8
    worker_attestation_max_age_seconds bigint NOT NULL CHECK (worker_attestation_max_age_seconds > 0),       -- OD-G9
    freeze_max_seconds bigint NULL CHECK (freeze_max_seconds BETWEEN 60 AND 259200), -- OD-G7; NULL = structural cap 72 h
    evidence_reference text NOT NULL CHECK (evidence_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$'),
    change_request_id uuid NOT NULL UNIQUE REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    created_by uuid NOT NULL, created_by_person_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
-- NOTE (DB H1): no min_approvals column in v2.1. The count is PINNED to 2 (the kind row); see s6.1.

CREATE SEQUENCE screening_gate_event_seq;  -- owner-only; nextval called only AFTER the relevant lock row is held (s4.9),
                                           -- so the order per lock scope equals the commit order (DB M3)

CREATE TABLE screening_gate_instance_attestations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), seq bigint NOT NULL,
    environment text NOT NULL,
    system_identifier bigint NOT NULL,          -- CAPTURED: (pg_control_system()).system_identifier
    timeline_id integer NOT NULL,               -- CAPTURED: (pg_control_checkpoint()).timeline_id (primary only; s4.7)
    database_oid oid NOT NULL, database_name name NOT NULL,
    postmaster_start_time timestamptz NOT NULL, -- CAPTURED
    server_addr inet NULL,                      -- CAPTURED; informational only (unreliable behind proxies; operations M3)
    instance_label text NOT NULL,               -- operator-supplied instance id from the external inventory (operations M3)
    checklist_reference text NOT NULL,          -- the completed no-clone checklist (operations M3)
    operator_reference text NOT NULL,
    valid_until timestamptz NOT NULL,
    attested_session_user name NOT NULL,
    attested_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE screening_gate_instance_attestation_confirmations (   -- the second attestor (staging; OD-G8)
    attestation_id uuid NOT NULL REFERENCES screening_gate_instance_attestations (id) ON DELETE RESTRICT,
    confirmed_session_user name NOT NULL, confirmed_at timestamptz NOT NULL DEFAULT clock_timestamp(), seq bigint NOT NULL,
    PRIMARY KEY (attestation_id, confirmed_session_user)
);                                         -- the guard refuses a confirmation by the attesting session_user

CREATE TABLE screening_worker_attestations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), seq bigint NOT NULL,
    provider_id text NOT NULL CHECK (provider_id ~ '^[a-z0-9_.-]{1,64}$'),
    synthetic boolean NOT NULL, deployment_reference text NOT NULL,
    instance_attestation_id uuid NOT NULL REFERENCES screening_gate_instance_attestations (id) ON DELETE RESTRICT,
    valid_until timestamptz NOT NULL, attested_session_user name NOT NULL,
    attested_at timestamptz NOT NULL DEFAULT clock_timestamp()
);   -- current ONLY while its instance_attestation_id IS the current instance attestation (DB M5)

CREATE TABLE screening_gate_authorisations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), seq bigint NOT NULL UNIQUE,
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    licensing_jurisdiction_id uuid NOT NULL REFERENCES jurisdictions (id) ON DELETE RESTRICT,
    mode text NOT NULL CHECK (mode IN ('activation', 'adoption')),
    tier text NOT NULL CHECK (tier IN ('S', 'N')),     -- v2.2 (security N5): must equal the current settings tier to be current
    adopts_policy_id uuid NULL,
    environment text NOT NULL, system_identifier bigint NOT NULL, database_oid oid NOT NULL,     -- == payload == live (security M5)
    cutover_nonce uuid NOT NULL,                                                                  -- == payload == screening_gate_cutover
    instance_attestation_id uuid NOT NULL REFERENCES screening_gate_instance_attestations (id),  -- == payload == current
    worker_attestation_id uuid NULL REFERENCES screening_worker_attestations (id),
    screening_policy_id uuid NOT NULL,          -- the aml_screening_policies row in force at execution (security M8)
    settings_id uuid NOT NULL REFERENCES screening_gate_settings (id),
    capacity_report_reference text NULL, capacity_report_sha256 text NULL CHECK (capacity_report_sha256 ~ '^[0-9a-f]{64}$'),
    capacity_accepted_persons_max bigint NULL CHECK (capacity_accepted_persons_max > 0),
    persons_at_execution bigint NOT NULL,       -- exact count under app.tenant_id = tenant (I14)
    persons_held_on_enable bigint NULL,         -- activation only: persons who would be held at once (ledger L-5; s6.2)
    compliance_reference text NULL, legal_review_reference text NOT NULL, evidence_reference text NOT NULL,
    valid_from timestamptz NOT NULL, valid_until timestamptz NOT NULL,
    change_request_id uuid NOT NULL UNIQUE REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    requested_by uuid NOT NULL, requested_by_person_id uuid NOT NULL,
    approver_person_ids uuid[] NOT NULL CHECK (cardinality(approver_person_ids) = 2),
    executed_by uuid NOT NULL, executed_by_person_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, id),
    CHECK ((mode = 'adoption') = (adopts_policy_id IS NOT NULL)),
    CHECK (mode = 'adoption' OR (worker_attestation_id IS NOT NULL AND persons_held_on_enable IS NOT NULL)),
    CHECK (valid_until > valid_from),
    FOREIGN KEY (tenant_id, adopts_policy_id) REFERENCES aml_screening_trigger_policies (tenant_id, id) ON DELETE RESTRICT
);
CREATE INDEX screening_gate_authorisations_tj ON screening_gate_authorisations (tenant_id, licensing_jurisdiction_id, seq DESC);

CREATE TABLE screening_gate_revocations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), seq bigint NOT NULL UNIQUE,
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    licensing_jurisdiction_id uuid NOT NULL REFERENCES jurisdictions (id) ON DELETE RESTRICT,  -- FK added (DB M10)
    authorisation_id uuid NULL,              -- NULL = every authorisation of (T, J) with seq < this.seq (DB M3)
    source text NOT NULL CHECK (source IN ('governed', 'break_glass')),
    change_request_id uuid NULL UNIQUE REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    incident_reference text NULL, reason_code text NOT NULL CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    revoked_by uuid NULL, revoked_by_person_id uuid NULL, revoked_session_user name NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE NULLS NOT DISTINCT (source, incident_reference, tenant_id, licensing_jurisdiction_id, authorisation_id),  -- idempotency incl. NULL authorisation_id (PG15+; security N6, ledger N-4)
    CHECK ((source = 'governed') = (change_request_id IS NOT NULL AND revoked_by IS NOT NULL)),
    CHECK ((source = 'break_glass') = (incident_reference IS NOT NULL)),
    FOREIGN KEY (tenant_id, authorisation_id) REFERENCES screening_gate_authorisations (tenant_id, id) ON DELETE RESTRICT
);
CREATE INDEX screening_gate_revocations_tj ON screening_gate_revocations (tenant_id, licensing_jurisdiction_id, seq DESC);

CREATE TABLE screening_gate_activation_freezes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), seq bigint NOT NULL,
    frozen_until timestamptz NOT NULL, incident_reference text NOT NULL UNIQUE,     -- idempotency (ledger L-3)
    reason_code text NOT NULL, frozen_session_user name NOT NULL, drill boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (frozen_until > created_at)
);
CREATE TABLE screening_gate_freeze_lifts (      -- the two-Person governed early lift (security M7)
    freeze_id uuid PRIMARY KEY REFERENCES screening_gate_activation_freezes (id),
    change_request_id uuid NOT NULL UNIQUE REFERENCES licensing_change_requests (id), seq bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The lock table (s4.9). Owner-only, FORCE RLS, NO runtime privilege: no runtime session can lock a row directly.
CREATE TABLE screening_gate_locks (
    lock_key text PRIMARY KEY CHECK (lock_key ~ '^[a-z_]+(:(global|evidence|screening_gate|[0-9a-f-]{36})){1,2}$'),  -- v2.2 fix (DB N1): the v2.1 pattern rejected the seed keys
    last_created_at timestamptz NULL           -- policy:<T>:<J> rows only: the ordering clock (s3.1)
);
-- A static test inserts EVERY key form s4.9 generates (seeds and per-(T,J) forms) into a scratch copy of the CHECK (A-26).

-- New policy-table columns (ADD COLUMN NULL, no rewrite; FORCED by the guard; legacy rows stay NULL).
ALTER TABLE aml_screening_trigger_policies ADD COLUMN gate_authorisation_id uuid NULL;
ALTER TABLE aml_screening_trigger_policies ADD COLUMN gate_enabling_policy_id uuid NULL;   -- s3.1
ALTER TABLE aml_screening_trigger_policies ADD COLUMN inserted_at timestamptz NULL;          -- wall time, forced (v2.2); NULL only on legacy rows
-- (v2.3 final) NO CHECK on inserted_at: enforcement is the guard plus reconcile (s3.1), so that tablesync and data-only restore of legacy rows work.
-- FKs: (tenant_id, gate_authorisation_id) -> screening_gate_authorisations (tenant_id, id); (tenant_id, gate_enabling_policy_id) -> self.
CREATE INDEX aml_screening_trigger_policies_enabling ON aml_screening_trigger_policies (gate_enabling_policy_id) WHERE gate_enabling_policy_id IS NOT NULL;
```

**Chain table (DB N8):** `screening_gate_evidence_chain` has the indexes `(stream, seq)` UNIQUE and `(source_table, source_id)` UNIQUE, the latter so that completeness is checkable (s5.6). Chain trigger functions are declared VOLATILE.
**Canonical encoder (DB N8):** sorted text arrays use `COLLATE "C"`, and every encoding starts with a per-table type tag (`<len>:<table>;`), so two tables can never produce colliding encodings.

**Retention (DB L3):** every evidence table is retained for the life of the database (low volume: configuration events only). The
policy table's existing index covers the pair lookups.

**Clock seam (QA H-4).** One pure helper decides currency. The guard and the executors call it with `clock_timestamp()` and the live
values; tests call it with an explicit `p_now`:

```sql
CREATE FUNCTION screening_gate_authorisation_current(p_auth uuid, p_now timestamptz, p_env text, p_sysid bigint, p_dboid oid,
                                                     OUT o_current boolean, OUT o_reason text)  -- reason: closed code or NULL
    LANGUAGE plpgsql STABLE SET search_path = pg_catalog, public, pg_temp;   -- owner-only
CREATE FUNCTION screening_gate_instance_attestation_current(p_now timestamptz, p_env text, p_sysid bigint, p_timeline integer,
                                                            p_dboid oid, p_pm_start timestamptz, OUT o_id uuid, OUT o_reason text)
    LANGUAGE plpgsql STABLE SET search_path = pg_catalog, public, pg_temp;   -- owner-only
```

C(T,J,t) is the latest **activation** row of (T, J) by `seq` for which `screening_gate_authorisation_current(..., t, live...)` is
current. Coverage: not revoked by a row with `authorisation_id = id`, nor by a row with `authorisation_id IS NULL AND
revocation.seq > authorisation.seq`; `valid_from <= t < valid_until`; environment, `system_identifier`, `database_oid` and
`cutover_nonce` all equal live.

### 4.1 The activation guard

```sql
CREATE FUNCTION aml_screening_trigger_policies_activation_guard() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, public, pg_temp;     -- SECURITY INVOKER (E-15)
CREATE TRIGGER aml_screening_trigger_policies_99_activation_guard               -- 99: the LAST BEFORE INSERT trigger (security L5)
    BEFORE INSERT ON aml_screening_trigger_policies FOR EACH ROW EXECUTE FUNCTION aml_screening_trigger_policies_activation_guard();
-- ORIGIN-enabled (s5.5 replication stance). A catalogue test pins that no BEFORE INSERT trigger name sorts after it.
```

**Refusal SQLSTATE** (DB H4): when `NEW.created_by_scope = 'platform'` (the row comes from a governed executor, F3), every refusal is
**`LC050` + HINT**, so the licensing-change service keeps the code (F14). Otherwise it is **`AT004` + HINT** (routed author and owner
setter; old applications map it to 400). Every HINT is registered in `aml.RefusalCodes` **and** the licensing-change refusal table
(A-19).

Algorithm:
1. `PERFORM set_config('lock_timeout', '5s', true)` (saved and restored at exit). Take the **policy-pair lock row**
   `policy:<T>:<J>` **exclusively** (`SELECT ... FOR UPDATE`, inserted first with `ON CONFLICT DO NOTHING` if missing; DB M2,
   security L1). Re-taking it when the author or loosen apply already holds it is a no-op. `55P03` or `40P01` propagate and are mapped to a
   retryable `gate_retry_later` (DB M8; s9).
2. Force `NEW.inserted_at := clock_timestamp()` and `NEW.created_at` monotonic from the lock row's `last_created_at`, then update
   `last_created_at` (s3.1). If the caller supplied `gate_authorisation_id`, `gate_enabling_policy_id` or `inserted_at`, refuse
   `gate_authorisation_stamp_forbidden`.
3. **If `NOT NEW.gate_withdrawal`: `RETURN NEW` now.** No other lock, no isolation check, no settings, freeze, integrity, chain or
   attestation read (I2, ledger M-2). The gate-off path takes only the pair row lock (plus K1-equivalent `loosening:<T>` on the governed
   path, s4.9). Its chain append (AFTER INSERT) takes only the same pair row (the per-stream chain, s5.6).
4. Isolation must be READ COMMITTED (`gate_isolation_refused`).
5. Compute L and F with `screening_trigger_policy_pair`. **If F is gating: continuation.** Set
   `NEW.gate_enabling_policy_id` to F's enabling row (s3.1) and `NEW.gate_authorisation_id` to that row's stamp (NULL for legacy). If
   the run is `environment_mismatch` or `unexplained` and NEW is looser than F (`screening_trigger_cmp`), refuse
   `gate_integrity_unresolved`. `RETURN NEW`.
6. **New enabling.** Take `gate:global` **shared** (`FOR SHARE`), then `gate:<T>:<J>` **shared**. Then refuse unless all of the
   following hold, in this fixed precedence (one closed code each; QA error-precedence tests use the HINT as discriminator):
   1. `NEW.effective_from <= clock_timestamp()`, else `gate_enabling_must_be_immediate`
   2. `aml_screening_gate_environment_ok()`, else `gate_environment_refused`
   3. a settings row exists for the anchor, else `gate_activation_not_configured`
   4. role separation (s5.5), else `gate_role_separation_violated`
   5. no active freeze (`frozen_until > now` and not lifted), else `gate_activation_frozen`
   6. no instance integrity state (`unexplained`, `environment_mismatch`, `guard_not_intact`; evaluated tenant by tenant with
      non-vacuity, I14), else `gate_integrity_unresolved`. If the per-tenant read disagrees with the platform count: `gate_integrity_read_inconsistent`
   7. no other legacy run of T, else `gate_tenant_has_unauthorised_gate`
   8. a current instance attestation, with its confirmations when the settings require two, else `gate_instance_attestation_missing`
   9. C(T,J,now) exists, else `gate_authorisation_required`, or sharper `_lapsed` / `_revoked` / `_identity_mismatch` / `_environment_mismatch`.
      v2.2: C must also have `tier` = the current settings tier, else `gate_authorisation_tier_mismatch` (moving S to N invalidates every S authorisation; security N5)
   9b. **Tier S only:** the worker attestation and the heartbeat are both `synthetic = true`, and the provider is the MOCK, else `gate_tier_s_requires_synthetic`. This is the only database-enforced part of "synthetic only". **That no real person data is present is a process control** (s19)
   10. the in-force screening policy is the authorisation's `screening_policy_id`, else `gate_screening_policy_changed`
   11. the worker attestation of C is current and bound to the current instance attestation, and a heartbeat of that provider with
       the same `synthetic` flag is younger than 10 minutes, else `gate_worker_attestation_missing`
   12. when a cap is required: with `app.tenant_id` **set to T** (the previous value saved and restored **exactly**, including the empty
       string; then the read-back `current_setting('app.tenant_id') = T` is asserted, DB N8; I14), `count(DISTINCT person_id)` of
       `player_accounts` is at most `capacity_accepted_persons_max`, else `gate_capacity_exceeded`
7. `NEW.gate_authorisation_id := C.id`; `NEW.gate_enabling_policy_id := NEW.id`.
8. If `NEW.created_by_scope = 'platform'`, refuse `trigger_gate_enable_refused` (existing). This is defence in depth and is an
   **equivalent mutant** when the earlier checks are intact (the `_10_guard` and the loosen validator refuse first). It is tested by disabling
   the `_10_guard` admission on a private DB (QA).
9. `RETURN NEW`.

Notes on interaction (DB L6):
- BEFORE INSERT triggers fire before an `ON CONFLICT` decision, so the pair lock is taken even if no row results (harmless).
- `COPY` fires the guard for every row. Data-only restores of this table are unsupported (the backup tooling bans `--disable-triggers`, F17).
- Any refusal rolls back the whole statement.

### 4.2 Lifecycle: effect on NEW activation and on an ALREADY-ACTIVE gate

| Event | Database behaviour | NEW enabling | ALREADY-ACTIVE gate | Alert / reconcile |
|---|---|---|---|---|
| Authorisation executed | one activation row (seq); records `persons_at_execution`, `persons_held_on_enable`, `screening_policy_id` | allowed within validity | n/a | `authorisation_created` p3 (out of band, R-5) |
| Activation | guard stamps it | allowed again within validity (E-7, multi-use) | n/a | audit `aml.screening_gate_enabled` |
| Expiry | nothing written | refused `_lapsed` | **unchanged, nothing released** | `gate_authorisation_lapsed`; `expiring` p3 |
| Revocation | revocation row | refused `_revoked` | **unchanged** | `gate_authorisation_revoked` |
| Replacement | new activation row; latest current wins | allowed | runs keep their stamp | runs on a superseded stamp listed |
| Screening policy version changes | nothing written | refused `gate_screening_policy_changed` until re-authorised | unchanged | `gate_screening_policy_changed_since_authorisation` p3 |
| Anchor differs | nothing | refused instance-wide | unchanged | `gate_environment_mismatch` p1 |
| Logical restore or blue/green | nothing | refused | unchanged | `gate_identity_mismatch` p2; `instance_attestation_stale` |
| Clone, restart or failover | nothing | refused until re-attestation | unchanged (R-3) | `instance_attestation_stale` p2 if any current authorisation exists |
| Approve races revoke | serialised on the `gate:<T>:<J>` row | later one wins; `gate_authorisation_revoked_before_execution` | n/a | both audited |
| Worker down or recovered | heartbeat ages out or returns | refused or allowed again | unchanged; release only by fresh clears | `gate_without_attested_worker` p2 |
| Break-glass revoke or freeze | row (idempotent per incident) | refused | unchanged | p2 + chain |
| Freeze lifted (governed, two Persons) or expired | lift row, or time passes | allowed again | unchanged | `freeze_active` clears |
| A legacy pending gating row takes effect | nothing written by the database | n/a | the gate starts holding (pre-cut-over decision) | `legacy_pending_enabling` p1 (only if OD-G13 allowed applying) |

**A lapsed, revoked, mismatched or frozen state never turns an active gate off and never releases a held withdrawal.**

### 4.3 Expiry and the open transaction

Unchanged from v2: the INSERT-to-COMMIT window (seconds) is accepted, visible and alerted. E-6 removes the long pending window.

### 4.4 Revocation (v2.1: by sequence)

- `screening_gate_revoke` (narrowing, 0 approvals, any platform_admin, executes on filing). Payload
  `{tenant_id, jurisdiction_id, authorisation_id | null, reason_code, evidence_reference}`.
- `screening_gate_revoke_apply`: takes `gate:global` shared, then `gate:<T>:<J>` exclusive, then `nextval` (so seq order equals
  serialisation order), then inserts. Refusals: `gate_authorisation_not_found`, `gate_authorisation_already_revoked` (nothing written).
- **"Revoked before execution"** (an authorisation request filed before a `NULL`-scope revocation): the authorise and adopt executors,
  holding `gate:<T>:<J>` exclusive, refuse when a `NULL`-scope revocation of (T, J) exists whose `created_at >= request.created_at`.
  Both are database `clock_timestamp()` values. This one comparison is by clock, because the licensing-change request has no event seq
  at filing. **Residual:** a backwards step of the database host clock between filing and revocation can misorder them. Mitigation:
  revocations also record `max(seq)` at their time, and the runbook says "after any clock incident, revoke again".

### 4.5 Replacement

Unchanged: a new two-Person authorisation; extending validity is a widening; shortening is revoke plus re-authorise.

### 4.6 Environment mismatch, logical restore

The procedure order is fixed: (1) record the anchor if it is unrecorded (owner); (2) reconcile; (3) attest (two attestors in staging);
(4) adopt or governed gate-off per run (gate-off needs OD-G10); (5) a new authorisation before new enabling. The database never
re-binds an authorisation to a new identity. **After an actor-proof key rotation (`deploy/backup/rekey-actor-proof.sh`), open
licensing-change requests whose proofs no longer verify must be re-filed** (operations M3).

### 4.7 Instance attestation (v2.1: burden, two-person, semantics)

- An attestation is **current** only while every one of these holds:
  - the live `system_identifier`, `timeline_id`, `database_oid` and `pg_postmaster_start_time()` equal the captured values;
  - `environment` equals the anchor;
  - `valid_until > now`;
  - when the settings say 2, a confirmation by a **different** attestor login exists.
- Captured on the **primary** only: the attest function refuses `pg_is_in_recovery()`.
- `timeline_id` increments on promotion and on PITR. A promoted replica therefore needs re-attestation.
- `server_addr` is informational, because it is unreliable behind proxies (operations M3). The operator-supplied `instance_label` from
  the external inventory is what the external check compares.
- `checklist_reference` points at the completed no-clone checklist: hostname and instance id match the inventory, no other live instance
  carries this `system_identifier` (external query), and the restore or restart was planned.
- **Burden, stated honestly:** every restart blocks new enabling until re-attested. That is acceptable only because enabling is rare.
  The risk of blind re-attestation is mitigated by the checklist, two-person confirmation in staging (OD-G8), and the external
  inventory cross-check. Residual R-3 (an already-authorised gate in a clone stays `authorised`) remains an owner acceptance (OD-G8).

### 4.8 Worker failure and recovery

Unchanged (fail closed; release only by fresh clears). **When funds are trapped and the loosening capability is inert** (operations
H4), on-call follows this order:
1. Restore the worker (the normal path).
2. If the gate itself must go off, that is a release decision under OD-G10. It is executed either through the governed loosening (which needs
   the capability configured) or through the owner setter path A1 (an owner credential plus one officer chain, recorded decision, D1
   declared window).
3. Never disable a trigger, and never touch holds.

### 4.9 Concurrency: lock rows replace advisory locks (security H1, DB M2)

**Problem (security H1):** advisory keys are computable from published code, and `pg_advisory_lock` is PUBLIC-executable. Any
session (the runtime role included) can hold `hashtextextended('screening_trigger:'||T||J, 191)` or the 190 key for as long as it
likes. That blocks the author, **the governed gate-off** and, in v2, K3/K4/K5. The owner cannot revoke EXECUTE on `pg_catalog`
functions (that needs superuser).

**v2.1:** all serialisation uses `SELECT ... FOR UPDATE` / `FOR SHARE` on rows of `screening_gate_locks`.
- The table is owner-only with no runtime privilege, so only owner-executed (SECURITY DEFINER) code can lock rows.
- Rows are created on first use with `INSERT ... ON CONFLICT DO NOTHING`. `gate:global` is seeded by 0195.
- Every locker first does `set_config('lock_timeout', '5s', true)`, so a waiter fails with retryable `55P03` instead of hanging.
- One owner-only helper does all of this: `screening_gate_lock(p_key text, p_exclusive boolean)`.
- **v2.2 admission rule (security N1, High).** **No runtime-callable function takes any lock row before actor-proof consumption or LC040
  admission.**
  - These validate **without locking**: the filing-time asserts (`screening_trigger_loosen_assert`, `screening_policy_loosen_assert`,
    the classifier branches at `0190:585` and `0191:425`, every `screening_gate_*_assert`) and `screening_gate_enable_preview`.
  - These lock only **after** admission: the executors (LC040: an executed request of this transaction and actor), the routed authors
    (after `aml_authorize` consumed the proof) and the guard (inside an admitted writer).
  - Each executor **re-validates under its locks**. A narrowing request that turns out to be widening at execution is refused
    `classification_changed_refile`.
  - **v2.3 (security N8): pre-existing executors must re-classify too.** Removing the lock from the 0190 configure classifier
    (`0190:585`, repeated in `0191:425`; its own comment says the lock exists "so widening is decided against the version a concurrent
    change leaves") would otherwise leave `screening_loosening_settings_apply` (`0190:611-650`) with **no** re-classification. That function
    only checks approvers when `required_approvals > 0` (`0190:636-638`).
    - **The race this closes:** the capability is enabled with a 7-day cooling period. Admin A files "disable" (narrowing). Admin B
      concurrently files "enabled, 30 days" (also narrowing against the 7-day state). A commits. B's apply then inserts enabled/30 days. One
      platform_admin has re-enabled loosening with no approvals and no officer check, bypassing the c4 HIGH-1 fix.
    - **Fix, as a NAMED EXCEPTION to the 0197 fingerprint pin (s7.1):** `screening_loosening_settings_apply` gains, immediately after its
      (swapped) `loosening:<T>` lock and **before** the approver check:
      1. call `screening_loosening_configure_classify(r.payload, true)` (the single classification function, below);
      2. refuse `classification_changed_refile` (LC050) if `o_unchanged`;
      3. refuse `classification_changed_refile` if `o_widening AND NOT r.widening` (filed narrowing, now widening);
      4. if `o_widening`, call `screening_loosening_officers_assert(v_tenant, a.actor, a.person_id)`. This is redundant once step 3 refuses,
         but kept as defence in depth.

      A widening request that has become narrowing at execution is allowed (stricter).
    - **Checked and unchanged apart from the lock line:** `screening_policy_loosen_apply` (`0190:654-`) and `screening_trigger_loosen_apply`
      (`0191:450-`). Both kinds are **always widening** (their classification cannot change), and both already call their full assert
      **under** the locks (`0190:689-690`, `0191:484-485`). The 0191 configure branch has **no** apply of its own: it executes through
      `screening_loosening_settings_apply`. No other pre-existing kind loses a classifier lock in 0197.
    - **One function, so the two phases cannot drift (v2.3, DB V5).** The classification expression lives in exactly one new function,
      `screening_loosening_configure_classify(p jsonb, p_locked boolean, OUT o_widening boolean, OUT o_unchanged boolean)`.
      - It is **runtime-executable SECURITY DEFINER and read-only** (v2.3 final, DB C2). The filing-time classifier is an INVOKER function
        running as the runtime role and would get 42501 on an owner-only callee. The 0191 branch already calls the runtime-executable
        `screening_loosening_state(uuid)` (`deploy/init-app-role.sql:248-257`).
      - It is listed in the exposed-function pin and in the read-filter pin (A-30): a foreign tenant gives AT002, and the all-tenant form is
        platform-only.
      - The classifier branch calls it with `p_locked = false`; it reads only and never locks. `screening_loosening_settings_apply` calls it
        with `p_locked = true` after taking `loosening:<T>`.
      - **Self-check, keyed by lock key (v2.3 final, DB C1):** `screening_gate_lock` records each key it took in a **transaction-local**
        custom setting (`set_config('app.screening_gate_locks_held', <keys>, true)`). Being transaction-local, it rolls back with savepoints
        exactly as the row locks do. With `p_locked = true`, the function requires the **specific** key `loosening:<T>` to be listed. There is
        no `pg_locks` probe (it would prove only that some lock row is held, and scans the whole view).
      - **The marker is an internal consistency guard, not a security control**: any session can write a custom setting. A runtime caller
        passing `p_locked = true` without the lock simply fails the check, and gains nothing by faking the marker, because the function only
        reads.
      - `screening_gate_settings_classify(p, p_locked)` has the same properties (gate settings are platform-level, so it is platform-session-only).
    - **General rule (v2.3):** executors take their locks **before** calling an assert, and **no assert ever takes a lock itself**. The existing
      `screening_policy_loosen_assert` (`0190:302`) and `screening_trigger_loosen_assert` (`0191:316`) each **lose** their advisory-lock
      line in 0197. They are already called under the executors' locks (`0190:685-690`, `0191:481-485`). Their existing `p_filed_at IS NULL`
      vs NOT NULL phase parameter plays the role of `p_locked`, so filing and execution use the same body.
    - **Exactly which bodies change in 0197 beyond the lock line (the complete named-exception list for the fingerprint pin):**

      | Function | Change |
      |---|---|
      | `screening_policy_loosen_assert` (0190) | lock line **removed** |
      | `screening_trigger_loosen_assert` (0191) | lock line **removed** |
      | `licensing_change_classify` (the 0191 wrapper), configure branch | lock line removed; the inline widening expression replaced by a call to `screening_loosening_configure_classify(p, false)` |
      | `screening_loosening_settings_apply` (0190) | lock swapped; the call `screening_loosening_configure_classify(r.payload, true)`; refuse `classification_changed_refile` if unchanged, or if widening while filed narrowing; officer assert if widening (its **only** row in this table) |
      | `screening_loosening_configure_classify` | **new** |
      | `aml_screening_trigger_policy_author`, `screening_trigger_loosen_apply` (0191) | lock swapped + pair helper |
      | `aml_screening_policy_author` (0189), `screening_policy_loosen_apply` (0190) | lock swapped |

      Tests proving it: A-47 (race), the existing `internal/licensingchange/screening_loosening_integration_test.go` (configure) and
      `screening_trigger_loosening_integration_test.go` run unchanged, and the decision-table rows below.
    - **Configure decision-table rows (v2.3):**
      - disabled → enable (widening, 2 Persons);
      - enabled 7 d → shorten to 3 d (widening);
      - enabled → disable (narrowing, executes on filing);
      - 7 d → 30 d (narrowing);
      - an unchanged payload (LC010 at filing, `classification_changed_refile` at execution);
      - the concurrent case: A disable commits, then B's "30 d" execution is refused `classification_changed_refile`.

      Outcomes are identical to 0196 except the concurrent row.
    - **New kinds:** `screening_gate_settings_apply` performs the same steps against `screening_gate_settings`, through one
      `screening_gate_settings_classify(p, p_locked)`.
      `screening_gate_revoke_apply` is always narrowing and cannot become widening. `screening_gate_freeze_lift_apply` is always widening.
  - The `screening_gate_revoke` classifier branch validates payload shape only (DB N8).
  - A catalogue/static test parses every runtime-executable function and fails if a lock call precedes the admission call (A-27).
- **v2.2 watchdog (security N1, operations N2).** `idle_in_transaction_session_timeout` is **USERSET**: the runtime role can override it
  for its own session, so it is **not a bound**. `statement_timeout` is user-settable too, and PG16 has no `transaction_timeout`. The bound
  is therefore a **privileged watchdog**:
  - An external job under role `igaming_gate_watchdog` (LOGIN, `pg_signal_backend`, plus `pg_read_all_stats` to see other roles' activity;
    human-provisioned) terminates any **runtime-role** backend that holds a `screening_gate_locks` row lock longer than N seconds (N is an
    engineering setting; default proposal 30 s).
  - **Detection (v2.3, security N9).** Held row locks do **not** appear as tuple locks in `pg_locks`; only waiters do. A `FOR UPDATE` or
    `FOR SHARE` holder holds `RowShareLock` on the **relation** `screening_gate_locks` for the whole transaction. The watchdog therefore
    selects backends with a granted `RowShareLock` (or stronger) on that relation's oid, `usename` = the runtime role, and
    `now() - xact_start > N`.
    - It cross-checks with `pg_blocking_pids()` of current waiters on that relation, and logs whether the holder was actually blocking.
    - **Which signal is reliable (v2.3, DB V4):** the **termination trigger** is a **blocker of a waiter**. The watchdog samples backends
      **waiting** on a lock of `screening_gate_locks` (a granted=false row in `pg_locks` for the relation or a tuple of it). For each such
      waiter it takes `pg_blocking_pids(waiter)`, and terminates a blocker only if the blocker is the runtime role and its `xact_start` is
      older than N. A holder with no waiter is harmless and is only reported (`gate_lock_long_holder`, p3). The RowShareLock-on-relation scan
      is the **reporting** signal; the blocker-of-waiter rule is the **acting** signal, since it is exact about harm.
    - **Sampling (v2.3 final, DB V4 + security):** the watchdog samples every **≤ 1 s**, which is shorter than the 5 s `lock_timeout`. Because a
      gate-off waiter gives up after 5 s and retries, a blocker may be visible only between retries. The watchdog therefore **remembers any
      blocker seen in the last M minutes** (engineering default 10) and treats it as actionable while it still holds a lock on the relation and
      is older than N. In addition, **a runtime-role RowShareLock holder on `screening_gate_locks` older than N is actionable on its own**:
      runtime code has no legitimate reason to hold one for N seconds. That holder is reported and terminated, and logged as
      `gate_lock_long_holder`.
    - **N must exceed the worst-case commit latency.** `pg_terminate_backend` during a synchronous-replication commit wait leaves the
      transaction **committed locally** while the client sees a dropped connection. Every executor's request-state checks (an executed request
      cannot execute twice; LC040 binds the txid) make the client's retry safe. The routed author's retry produces at most a second,
      equal-or-stricter version (tighten-only), which is harmless. This is stated, not hidden.
    - It terminates **only** backends whose `usename` is the runtime role. It never touches the owner, the attestor, the break-glass role or
      superusers.
    - It **never reads or stores `query` text**: `pg_read_all_stats` exposes SQL literals. It reads only pid, usename, application_name,
      xact_start and state.
    - Its credential is held in a custody separate from both the owner and the runtime credentials.
    - The monitor raises `watchdog_absent` (p1 in Tier N, p2 in Tier S) when the watchdog's own heartbeat (an external metric, not a database
      row the runtime could write) is stale.
    - **Stated plainly:** a compromised watchdog credential can terminate any non-superuser backend. That is an availability risk, accepted
      only because it cannot read data, write data or escalate.
  - Each termination is logged and reported as `gate_lock_holder_terminated` (p2).
  - The watchdog is **activation-blocking in Tier N**. In Tier S it is required **unless** the owner chooses the operations alternative (an
    idle-in-transaction reaper, or a written owner acceptance of the residual), OD-G0/s19. Security's position is that it is required in both tiers. Until it exists, the residual is stated: an admitted runtime caller can hold
    a lock row for as long as its connection lives.

| Row key | Replaces | Taken by (mode) |
|---|---|---|
| `loosening:<T>` | advisory `screening_loosening:`\|\|T /190 | loosening validators and executors, capability configure (exclusive) |
| `screening_policy:<T>:<J>` | advisory `T\|\|J`/0 (0189, 0190) | screening policy author and loosen apply (exclusive) |
| `policy:<T>:<J>` | advisory `screening_trigger:`\|\|T\|\|J /191 | trigger author, loosen apply, **the guard** (exclusive); the policy-stream chain append (already held); **the adopt executor (exclusive, taken BEFORE `gate:global`, DB N8)** |
| `gate:global` | K3 | guard new-enabling branch, authorise, adopt, revoke (shared); settings apply, attestations, freeze, freeze lift, break-glass revoke-all (exclusive) |
| `gate:<T>:<J>` | K4 | guard new-enabling branch (shared); authorise, adopt, revoke, break-glass revoke (exclusive) |
| `chain:evidence` | K5 (evidence stream only) | evidence-table chain appends (exclusive, last) |

- **Global order:** `loosening:<T>` → `screening_policy:<T>:<J>` → `policy:<T>:<J>` → `gate:global` → `gate:<T>:<J>` → `chain:evidence`.
- The licensing-change request row lock (taken by the flow) precedes `gate:*` on executor paths and is never taken by the guard.
- **Gate-off path:** `[loosening:<T>] → policy:<T>:<J>` only. It never **takes** `gate:*` or `chain:evidence`. It **can wait transitively**: the adopt executor holds `policy:<T>:<J>` while waiting for `gate:global`. Every such wait is **bounded** (`lock_timeout`, watchdog) and **retryable** (v2.3, DB V8; I2, I13).
- Migration 0197 replaces the three advisory calls in the existing functions (0189 `aml_screening_policy_author`, 0190
  `screening_policy_loosen_assert` / `_apply` / configure classifier, 0191 `aml_screening_trigger_policy_author` /
  `screening_trigger_loosen_assert` / `_apply` / classify branch). Each is a `CREATE OR REPLACE` with an unchanged signature, and the
  only body change is the lock line plus the pair helper. The fingerprint diff between 0196 and 0197 must contain **only** those lines
  (A-18).
- **Residual (v2.2 wording):** an admitted runtime caller can hold a lock row while its transaction stays open. The bound is the watchdog,
  not `idle_in_transaction_session_timeout` (USERSET; it remains a recommended default but is not relied on).
  - `lock_timeout` turns every wait into a retryable failure.
  - `gate_lock_contention` (p2) reports waits longer than 30 s with the holder's `usename` and `application_name`. A non-superuser owner can
    only read that with `pg_read_all_stats` or `pg_monitor`; that grant is a role-administrator step, unverified on RDS (DEFERRED with AWS,
    operations N9).
  - Runbook for `gate_retry_later`: read `gate_lock_contention`; the role administrator (or the watchdog) terminates the holder; retry
    (ledger N-3).
- **Fairness (DB N8):** a steady stream of `FOR SHARE` lockers can starve a `FOR UPDATE` waiter. The 5 s `lock_timeout` turns this into a
  retryable failure, and the monitor counts retries.
- **0197 swap timing (DB N8):** the advisory-to-row swap runs inside the write-quiet window. While old and new code coexist, mutual
  exclusion is split between advisory and row locks for seconds. The swap of the screening-policy key is **atomic in one migration
  for both `0189:67` and `0190:689`** (security N6).

Serialisation results (READ COMMITTED):
- **Author vs revoke:** the enabling holds `gate:<T>:<J>` shared until COMMIT. Revoke then follows, so the gate becomes active and `revoked`. If revoke commits first, the guard refuses.
- **Approve vs revoke:** both exclusive on `gate:<T>:<J>`. If revoke goes first, the executor refuses `_revoked_before_execution`.
- **Settings narrowing or freeze vs enabling:** `gate:global` exclusive against shared. An enabling already past step 6 holds the shared lock and commits first. The narrowing or freeze applies to every later enabling. This is accepted and stated.

### 4.10 Break-glass (v2.1: cap, governed lift, idempotency, membership)

- Role `igaming_gate_breakglass` (NOLOGIN), provisioned out of band (s7.5).
- **Membership time-boxing** (operations H4, security M7): PG16 has no time-boxed membership. The procedure is: a role administrator runs
  `GRANT igaming_gate_breakglass TO <named login>` and `REVOKE` again within the incident, both under a ticket. Reconcile reports
  `breakglass_membership_active` (p2) **whenever any login is a member**, and `attestor_membership_stale` (p3) for every attestor member
  (PG16 records no grant time, so staleness is asserted by the external ticket review).
- Functions: SECURITY DEFINER, EXECUTE only to the role. Each checks `session_user` is a member of the role and **not** of the owner,
  and refuses no-ops (security H2: `revoke_all` when authorisations exist but zero rows would be written gives
  `breakglass_noop_refused`). Each is idempotent by `incident_reference` (a repeat returns the existing row id).
  - `screening_gate_breakglass_revoke(p_authorisation uuid, p_incident text, p_reason text)`
  - `screening_gate_breakglass_revoke_all(p_incident text, p_reason text)`: platform-level table, so no tenant loop is needed (DB M10)
  - `screening_gate_breakglass_freeze(p_until timestamptz, p_incident text, p_reason text, p_drill boolean)`: `p_until` must be at most
    `now + COALESCE(settings.freeze_max_seconds, 259200)`, else `gate_freeze_exceeds_cap` (ledger L-2, security M7). A drill freeze
    (`p_drill`) is limited to 15 minutes.
- **Early lift:** governed kind `screening_gate_freeze_lift` (widening, two Persons, officers). A freeze otherwise lapses at `frozen_until`.
- Break-glass can never: insert or extend an authorisation, change settings, enable or disable a gate, release anything, or touch any trigger.
- **Drill** (operations H4, v2.2 cadence N11): at each staging deploy, and on a throwaway cluster in CI with the real NOLOGIN roles. A drill freeze of a few minutes, a break-glass revoke on a synthetic tenant, and an
  evidence template (incident id, logins granted and revoked with times, functions called, chain seq range, reconcile output before and
  after).

---

## 5. Item 3: trust boundaries

### 5.1 Every path that could enable a gate (or block a gate-off)

| # | Path | Actor | Today | After v2.1 |
|---|---|---|---|---|
| P1 | trigger-policy route → `aml_screening_trigger_policy_author` | officer (runtime role) | enables in development, staging and test (F7) | the guard requires settings, attestation, worker attestation, C(T,J), no freeze, integrity, the same screening policy, and the cap |
| P2 | direct INSERT | runtime | `42501` | unchanged |
| P3 | owner setter via runtime | runtime | `42501` | unchanged |
| P4 | owner setter or raw INSERT | **owner** | allowed (F6) | refused by the guard while it is enabled; bypass needs DDL (detected externally, s5.6) |
| P5 | `screening_trigger_loosen_apply` | runtime, two Persons | never enables | unchanged; refusals are LC050 (s4.1) |
| P6 | new executors | runtime, governed | n/a | never write the policy table (A-9) |
| P7 | forged heartbeat | runtime | satisfies the precondition | insufficient: a worker attestation bound to the current instance attestation is required |
| P8, P9 | `app.environment` GUC; anchor writes | any / runtime | irrelevant / no | unchanged |
| P10, P11 | evidence writes; executor without an executed request | runtime | n/a | no privilege; LC040 admission |
| P12 | minting two-Person approvals | **compromised API process** (it holds the actor-proof keys, `main.go:250`) | possible | **same residual R-5**; out-of-band `authorisation_created` alert; KMS follow-up (OD-G12) |
| P13 | attestations | runtime; owner | n/a | attestor-role membership required, owner excluded, primary only |
| P14 | DISABLE, replace or drop guards; `NO FORCE`; TRUNCATE after disable | **owner** | possible | still possible; detected only by external controls (s5.6) |
| P15 | `session_replication_role = replica` | owner / superuser | owner: no (superuser-only parameter; the manifest pins `pg_parameter_acl`) | superuser: deny triggers still fire (`ALWAYS`); insert guards do not (ORIGIN, s5.5); detected by D5 `misc_set` and by reconcile `gate_unexplained` |
| P16 | anything | superuser | everything | detection outside the database only |
| P17 (new) | **staging owner = RDS master** (F16), which has CREATEROLE and `rds_superuser`; it can grant itself attestor or break-glass membership | owner in staging | possible | refused by the role-separation check (s5.5); Tier S/N decision (s19) |
| P18 | squatting on advisory keys, or (v2.1 gap, security N1) calling a proof-less assert that took a lock row and then sleeping | any session / runtime | possible | v2.2: lock rows only, never before admission (s4.9); an admitted holder is bounded by the **watchdog**. The platform-wide advisory class outside this slice is a separate item |

### 5.2 Catalogue pins (writers, exposure, prefix loop)

- **Trigger-aware writer detector** (security L4, F15). `tableWriters` is extended so that a function writing table X also counts as a
  writer of every table Y written by a trigger function on X (closure over `pg_trigger`). Without this, the chain trigger on the policy
  table, which writes `screening_gate_evidence_chain`, would be invisible.
- Pinned runtime-executable writers (exact sets):

| Table | Writers |
|---|---|
| `aml_screening_trigger_policies` | `aml_screening_trigger_policy_author`, `screening_trigger_loosen_apply` (**unchanged**) |
| `aml_screening_policies` | `aml_screening_policy_author`, `screening_policy_loosen_apply` (unchanged) |
| `screening_gate_settings` | `screening_gate_settings_apply` |
| `screening_gate_authorisations` | `screening_gate_authorise_apply`, `screening_gate_adopt_apply` |
| `screening_gate_revocations` | `screening_gate_revoke_apply` |
| `screening_gate_freeze_lifts` | `screening_gate_freeze_lift_apply` |
| `screening_gate_evidence_chain` | the union of the rows above plus the two policy writers (through triggers) |
| `screening_gate_locks` | every runtime-executable function that locks (through `screening_gate_lock`): the pinned list is generated from the s4.9 table and asserted exactly |
| attestations, confirmations, freezes, cut-over, inventory | **none** |

- **Exposed-function pin** (DB M4). SECURITY DEFINER assert helpers called from the classifier branches at filing time
  (`screening_gate_authorise_assert(jsonb, timestamptz)`, `screening_gate_adopt_assert`, `screening_gate_settings_assert`,
  `screening_gate_freeze_lift_assert`, following the 0191 `screening_trigger_loosen_assert` shape) are runtime-executable and listed
  in this pin and in `init-app-role.sql`. They raise LC050 and write nothing (the writer pin proves it).
- **Prefix revoke loop** (security L3). Every migration ends with a loop over `pg_proc` where the name matches `screening\_gate\_%` or
  `screening\_worker\_attest%`. It runs `REVOKE ALL ... FROM PUBLIC` and from the runtime role, and then grants back only the
  explicit allow-list. A new function is therefore closed by default. A catalogue test asserts the runtime-executable set equals
  the allow-list.
- **v2.2 pins:**
  - **Read filtering (security N7):** every runtime-executable function that reads any `screening_gate_*` table either filters by the session
    tenant (AT002) or is platform-session-only. This is a catalogue test with a list, and each listed function has a negative test.
  - **The closing prefix REVOKE covers sequences** (`screening_gate_event_seq`), because the default privileges in `init-app-role.sql`
    would otherwise give the runtime USAGE (security L3).
  - **`screening_gate_enable_preview` pin:** session-tenant bound (AT002), platform-session check, `LIMIT` and a local `statement_timeout`,
    no lock.
  - **No lock before admission** (A-27).
- Other pins: FORCE RLS; no ACL entry for runtime or PUBLIC on any new table; `tgenabled` per the s5.5 table; `proconfig` holds the
  pinned `search_path` (and the hashing GUCs on hashing functions); the guard is the last BEFORE INSERT trigger.
- **Post-suite check** (QA H-1): a test that runs last asserts every trigger in the s5.5 table has its specified `tgenabled`. It catches
  a fixture that re-enabled with plain `ENABLE TRIGGER` and left a trigger ORIGIN where ALWAYS is required.

### 5.3 Worker health

Unchanged from v2 (option A, owner/attestor-written worker attestation, now **bound to the current instance attestation**: DB M5;
option B, the worker role split, is phase 2; C rejected; D PROVIDER DEPENDENT).

### 5.4 Evidence cannot be altered by runtime roles

No privilege, FORCE RLS, append-only, admission-guarded, chained, audited, reconciled.

### 5.5 Owner and superuser; role separation; ALWAYS vs ORIGIN; logical replication

**Honest capability table:** unchanged from v2. The owner can disable or replace triggers and functions, turn off FORCE RLS, rebuild the
chain, and read the actor-proof keys. The superuser can do everything. In-database controls therefore protect against runtime roles
and accidents only.

**Role separation (security H3, N4; G5).** Enforced wherever the settings say `role_separation = 'enforced'` (mandatory for Tier N,
**and the default for Tier S**). The only alternative is the explicit Tier S option S-b (`waived_tier_s`, a two-Person settings decision),
whose acceptance is spelled out in s19. Under S-b, every check below reports `role_separation_waived` (p2) instead of refusing. When
enforced:
- The owner role is NOSUPERUSER, NOCREATEROLE and **not** a member of `rds_superuser`. **Staging must stop defaulting
  `DB_OWNER_USER` to the RDS master** (F16): a separate owner role is created by the role administrator.
- The attest functions and every governed executor refuse with `gate_role_separation_violated` when any of these holds:
  - the owner role has `rolcreaterole`, `rolsuper` or `rolbypassrls`;
  - the owner role is a member of `rds_superuser` (checked only if that role exists);
  - `session_user` (the API login, for executors) is a member of the owner role;
  - an attestor or break-glass member is also an owner member;
  - (v2.2) the **API login itself** is SUPERUSER, CREATEROLE or BYPASSRLS, is a member of `rds_superuser`, or has any membership at all;
  - (v2.2) any role other than the role administrator holds **ADMIN OPTION** on the attestor or break-glass roles
    (`pg_auth_members.admin_option`), or any CREATEROLE holder other than the role administrator exists. **The role administrator is
    identified by a fixed role name, membership in the NOLOGIN group `igaming_role_admin`** (v2.3, security). Neither the owner nor the
    runtime role can write it: they are NOCREATEROLE, and only a CREATEROLE holder or a superuser can grant it. It is never read from a table
    the owner can write. **v2.3 final (security):**
    - reconcile reports its member list, with a p3 `role_admin_membership_changed` on any change (D5 role audit in Tier N);
    - it refuses (`gate_role_separation_violated`) if a member is the owner, the API login, or an attestor or break-glass member;
    - internal managed-service superusers (e.g. `rdsadmin`) are excluded explicitly by name list, and that list is in the signed manifest;
  - (v2.2) the attestor or break-glass `session_user` **is** the API login.

  These are catalogue reads (`pg_roles`, `pg_auth_members`).
- `internal/db/production_safety.go` `VerifyRuntimeRoleInProduction` (`:115-167`) is extended into
  `VerifyRuntimeRoleSeparation`. v2.2: it runs at startup **whenever the anchor is staging or production** (not only when settings exist), and
  the monitor repeats the catalogue checks every pass (`role_separation_violated` p1, or `role_separation_waived` p2 under S-b). It refuses to
  start if the runtime role owns tables or has memberships.

**`ENABLE ALWAYS` vs ORIGIN (resolves security H3 against DB H5).** PostgreSQL's logical-replication apply worker runs with
`session_replication_role = replica`. ALWAYS triggers fire there; ORIGIN triggers do not. RDS blue/green major upgrades use logical
replication, so this matters.

| Trigger | Mode | Why |
|---|---|---|
| deny triggers (UPDATE, DELETE, TRUNCATE) on every evidence table; `aml_screening_trigger_policies_append_only`, `_no_truncate`; `database_environment_no_truncate`; **v2.2: a new `database_environment_deny` (UPDATE, DELETE) split out of `database_environment_guard`** | **ALWAYS** | the publisher's own deny triggers prevent UPDATE, DELETE and TRUNCATE on these tables, so none are ever published; ALWAYS therefore never blocks a legitimate apply, and it stops a replica-mode bypass |
| **v2.2 (security N3): `screening_replica_mode_refuse` on every guarded table** (v2.3, DB V3: the trigger is named **`<table>_00_replica_refuse`** so it sorts **first**, before `_10_guard`; the pin of s4.1 is now "no BEFORE INSERT trigger sorts after `_99_activation_guard` except none; `_00_replica_refuse` is the only one before `_10_`". The function first does the cheap `current_setting('session_replication_role') = 'replica'` comparison and only then reads the backend type with `pg_stat_get_activity(pg_backend_pid())`, not the view. PG16 also has `logical replication parallel apply worker`; Q11 stays open) (the policy table, every evidence table, `database_environment`), BEFORE INSERT OR UPDATE OR DELETE | **ALWAYS** | refuses (`GA010`) when `session_replication_role = 'replica'` unless the backend is a logical-replication apply or tablesync worker (`pg_stat_activity.backend_type` of `pg_backend_pid()`; the exact strings on PG16 and RDS are an open verification, Q11; **PG17 renamed them**, so the allow-list must be verified in the staging drill **before any logical-replication upgrade**; `pg_stat_activity` is read **only after** `current_setting('session_replication_role') = 'replica'` is true, so the normal path pays nothing). This closes the plain `SET session_replication_role` bypass. A superuser can still alter the trigger itself, which D1 and D5 detect |
| insert guards: `aml_screening_trigger_policies_10_guard`, `_99_activation_guard`, every evidence admission guard, `database_environment_guard` (it guards INSERT, UPDATE and DELETE in one function) | **ORIGIN** (unchanged default) | on a subscriber the apply worker has no session actor and no executed request, so ALWAYS insert guards would break replication. Replica mode is superuser-only (P15); a bypass is detected by `gate_unexplained` and by D5 |
| chain triggers (AFTER INSERT) | **ORIGIN** | a subscriber receives the chain rows as data; firing locally would double-append |

**Stance on logical replication of these tables:** allowed only as **whole-database replication for an upgrade or migration**,
never as partial publication. The subscriber is a new instance, so it needs re-attestation, and identity-bound authorisations
classify `identity_mismatch` after switchover and need adoption (s3.7). A catalogue test pins the modes above.
`database_environment_guard` keeps its INSERT half ORIGIN. Its UPDATE/DELETE half is split into the ALWAYS deny trigger above (v2.2).
Startup options can set `session_replication_role` at connection time for a superuser. That gap is recorded, and Tier N requires
pgaudit `misc_set` plus `log_connections` to see it.

### 5.6 Detection controls: independence stated honestly (security M6, operations H1/H2/H3/M6)

| ID | Control | Independent of the owner? | Independent of the API process? |
|---|---|---|---|
| in-DB | tripwire, chain verify, reconcile, monitor summary, run by the in-process monitor | **no** (the owner can replace them) | **no** (the monitor runs in the API process, F11) |
| D1 | DDL event trigger (superuser-provisioned, owned by `igaming_ddl_audit_owner`) | yes against the owner; no against the superuser | yes (fires in the database, not in the API process) |
| D3b | **external verifier job** (separate host and credential, client-side recomputation) | yes, as long as its external store is outside the owner's reach | yes |

**D3b.** A separate host or job runs under a dedicated read-only `igaming_gate_verifier` role (BYPASSRLS, SELECT only, like
`igaming_backup`, F17, so FORCE RLS cannot hide rows). It:
- reads the raw chain and evidence rows and **recomputes every hash client-side** (its own code, never `screening_gate_chain_verify()`);
- compares against the heads it stored previously in an external append-only store;
- computes the catalogue manifest lines (D4) with inline SQL.

**Completeness (security N3).** The external verifier checks a **one-to-one** correspondence: every evidence row and every post-cut-over
policy row has exactly one chain entry, and every chain entry points to an existing row. Missing entries (rows inserted with chain triggers
disabled) and orphan entries are p1. The in-database reconcile repeats the check as a tripwire.

The chain is **two streams**: a per-(T,J) policy stream serialised by `policy:<T>:<J>`, and `chain:evidence` for configuration events.
Each row carries `prev_hash` of its own stream. Ordering is by `seq` with **gaps allowed**: verification follows `prev_hash` links
and never assumes contiguous seq (security M4). The in-process monitor verifies **incrementally** from a watermark (operations H3).
The watermark is advisory, because the runtime role could reset it. The external job always verifies in full.

**D1.**
- Correlation with migrations: 0195 adds `schema_migrations.txid bigint DEFAULT txid_current()` (no Go change needed: the runner's
  INSERT runs inside the migration transaction, F22; a Go test pins that the recorded txid equals the transaction's).
- Correlation is decided **outside the database**: the verifier compares DDL events against the deployment pipeline's own records
  (the release id, migration versions and time window from the CI/CD log). A matching `schema_migrations` row alone can be forged by the owner.
- **Declared windows** for routine non-migration DDL (operations H2): `init-app-role.sql` re-asserting REVOKE/GRANT, post-restore
  grants, rekey. The pipeline records `{window_id, start, end, purpose}` externally before running. Events inside a declared window
  are p3 (logged). Outside one, they are p1 `ddl_outside_migration`.
- `ddl_audit_missing` is p3 until event triggers are proven feasible on RDS (operations H2); Tier N makes it p1 (s19; security disagrees with p3 for staging).

**D4 manifest (operations M6):**
- Owned by `security`. Generated in CI from a clean migrate of the release commit and signed with the release signing key.
- Regenerated only by a release.
- Scope: every `screening_gate_*` object, the policy-table triggers, the **gate predicate functions (I1 set) and the monitor functions**,
  ACLs, `pg_parameter_acl` (**must be empty**), **no `pg_db_role_setting` entry setting `session_replication_role`** for any role or
  database (security N3), and the event triggers.

**D5 (log shipping and pgaudit), D6 (role separation, above), D7 (owner-login alerts), D8 (key custody):** as v2. D5 is not
available in this sandbox.

**Alert delivery is not yet a control (F20).** ALERT-DELIVERY-1 is OPEN and the email channel is MOCK. A p1 raised today reaches no
human. The monitor has these failure modes and detections (operations H1):
- **Availability** is decided by `max(version) >= 0198` in `schema_migrations`, not by function presence. If the functions are absent
  at that version: `monitor_unavailable` (p1).
- Each check runs in its own sub-transaction (savepoint). An exception becomes `monitor_check_failed:<problem>` (p2) and does not stop
  the other checks.
- Single-flight: the monitor takes `monitor:screening_gate` with `FOR UPDATE NOWAIT` and skips the pass if it is held.
  `statement_timeout = 30s` per check. v2.2 (security N6, operations N5): the single-flight row is reachable through the runtime-executable
  summary, so a hostile holder could suppress passes. **A skipped pass also skips the export**, so the external dead-man's switch sees it as
  stale. The `monitor_skipped` metric counts skips.
- **Dead-man's switch:** every pass exports `last_pass_at` and the chain head to the external store. The external job raises if no pass
  arrives within 3 intervals.

---

## 6. Item 4: governance

### 6.1 Reuse the licensing-change primitive; approval count pinned to 2 (DB H1)

The reasons for reuse are unchanged from v2. **v2.1 correction:** `required_approvals` is stamped from the kind row at filing, and
execution happens in the final-approval transaction (F18). A setting asking for 3 approvals is therefore **unreachable**: the
request executes at the second approval, and an executor-side "≥ 3" refusal would only roll that approval back for ever. v2.1 **pins
2 counted approvals** (the kind row: `widening_approvals = 2`; `approver_person_ids` cardinality = 2). The `min_approvals` setting is
removed. Higher counts become a **future primitive change**: a per-request `required_approvals` derived at filing from the settings by
the classifier. That change is listed (OD-G4), not designed here.

### 6.2 The kinds (v2.1)

| Kind | Class | Approvals | Officers | Executor; filing-time assert |
|---|---|---|---|---|
| `screening_gate_authorise` | widening | 2 (pinned) | requester, both approvers, executor: in-force `aml_officer` on T + owner-scope quorum | `screening_gate_authorise_apply`; `screening_gate_authorise_assert` |
| `screening_gate_adopt` | widening | 2 | same | `screening_gate_adopt_apply`; `screening_gate_adopt_assert` |
| `screening_gate_revoke` | narrowing | 0 | none (any platform_admin) | `screening_gate_revoke_apply` |
| `screening_gate_settings_configure` | widening if first or looser, else narrowing | 2 / 0 | platform_admin holding any in-force `aml_officer` grant | `screening_gate_settings_apply`; `screening_gate_settings_assert` |
| `screening_gate_freeze_lift` (new) | widening | 2 | same as settings | `screening_gate_freeze_lift_apply`; `screening_gate_freeze_lift_assert` |

**Authorise payload (security M5, M8; ledger L-5):** `{tenant_id, jurisdiction_id, validity_seconds, capacity_report_reference,
capacity_report_sha256, capacity_accepted_persons_max, compliance_reference, legal_review_reference, evidence_reference,
environment, system_identifier, database_oid, instance_attestation_id, cutover_nonce, screening_policy_id,
persons_held_on_enable_preview}`.
- Approvers' proofs bind all of these through the payload hash.
- `persons_held_on_enable_preview` is the number of persons who would be held as soon as the gate is enabled. The filer reads it from a
  read-only preview `screening_gate_enable_preview(T, J)`, which counts persons not covered by the in-force screening policy, ignoring the
  backfill grace. The executor **recomputes** it and stores the computed value in `persons_held_on_enable`. A difference is shown to
  approvers and audited; it is not a refusal (no invented threshold).
- **Capacity acceptance is a head-count bound only.** It does not prove throughput (operations H3). Throughput evidence is the
  CAPACITY plan's load test, referenced by `capacity_report_sha256`.

**Execute-phase validation:** v2's list, with these changes:
- The executor sets `app.tenant_id = T` before the persons count and asserts it (security H2).
- `validity_seconds >= 3600` (the engineering floor) and `<= max_validity_seconds`.
- Instance identity, `cutover_nonce`, `instance_attestation_id` (current) and `screening_policy_id` (in force) must all equal live.
- Role separation (s5.5).
- `gate_authorisation_revoked_before_execution` (s4.4).
- Counted approvals: exactly the 2 fresh distinct officer Persons, with the existing codes.
- `0196` down restores the 0191 classifier wrapper and the approvals-officer trigger **byte for byte** (DB M4).
- **v2.2:** the filing-time asserts and the classifier branches validate **without any lock** (s4.9 admission rule). `screening_gate_enable_preview`
  is bound to the session tenant (AT002), refuses non-platform sessions, and runs with `LIMIT` and a local `statement_timeout`. Its count is
  advisory only; the executor recomputes it.
- **Accepted risk (DB L7):** any platform_admin can file a narrowing revoke. It is fail-safe, but it is also an availability lever (it stops new
  enabling). It is audited and alerted (`revocation_created` p3).

### 6.3 Owner parameters

These are stored in `screening_gate_settings`, with no value shipped. The `tier` column records the owner's tier decision (s19). The
structural and engineering bounds are named as such: the 3600 s minimum validity, the 72 h freeze cap fallback, the 15-minute drill
freeze, the 10-minute heartbeat age, and the 5 s `lock_timeout`.

### 6.4 Audit and signed evidence

Unchanged, plus audit events for freeze lifts and confirmations (s10).

---

## 7. Item 5: migration strategy (0195+) (v2.1)

### 7.1 Revised sequence

The **capacity work stays separate.** Keyset batching, the enqueue-failure alert, priority lanes and the load test are their own slice and
migrations, reviewed by ledger-finance for performance. The activation gate does not depend on them. Its only link is the owner's
capacity evidence (OD-G3: report digest and head-count cap). Mixing the two would make one slice's rollback hold the other hostage.

| Step | Content | Behaviour |
|---|---|---|
| **R1** (application, must reach **100 % of instances** before 0198) | codes in `aml.RefusalCodes` and the licensing-change refusal table; OpenAPI; Back Office texts; kinds registered; the **version probe** (the app reads `max(version)` of `schema_migrations`, refreshed every 60 s, and enables the features at `>= 0196` / `>= 0198`) **plus the per-instance probe deliverable** (operations N10): each instance reports its build version through a version endpoint or heartbeat row that **the pipeline** reads before applying 0198 (the opposite direction; the DB-side probe cannot prove 100 % rollout); **a read-only legacy-pending pre-flight check** shipped in R1 and CI (the same query as 0198's pre-flight, so operators learn before the deploy window whether 0198 would refuse, and can neutralise through governed loosening; DB N7); the retryable mapping of `55P03`/`40P01` in the aml routes (DB M8); fixtures and E2E on the sanctioned path (s15.3); `VerifyRuntimeRoleSeparation` (s5.5) | none |
| **0195_screening_gate_evidence** | `SET LOCAL lock_timeout = '5s'`; pre-flight `GA098`; event sequence; lock table (+ `gate:global`, `chain:evidence`, `monitor:screening_gate` seed rows); settings, attestation, confirmation, worker attestation, authorisation, revocation, freeze, freeze-lift, chain tables (genesis row), catalogue-baseline table; admission and deny triggers (modes per s5.5); chain triggers on the evidence tables; helpers (`screening_gate_lock`, the clock-seam functions, canonical encoders); attest and break-glass functions; alert kinds; `ALTER TABLE schema_migrations ADD COLUMN txid bigint NULL` **then** `ALTER COLUMN txid SET DEFAULT txid_current()` (existing rows stay NULL; no rewrite; operations N4, DB N3; the txid is meaningless after a logical restore, operations N11); prefix revoke loop **including sequences** | inert (genesis row and lock seed rows only) |
| **0196_screening_gate_governed_kinds** | five kinds; classifier wrapper branch; filing-time assert helpers; approvals-officer trigger extension; executors; executed-request reconcile part; prefix loop | inert: no settings, so everything refuses |
| **0197_screening_locks_and_pair_helper** | `screening_trigger_policy_pair`; `CREATE OR REPLACE` of the 0189, 0190 and 0191 functions, swapping advisory locks for lock rows (s4.9) and the duplicated pair queries for the helper; **no other body change** (fingerprint-diff pin), **except the named exception in `screening_loosening_settings_apply`** (re-classification under the lock; s4.9, security N8). **v2.2 pins (ledger N-1, DB N5):** (i) the bodies of `aml_screening_gate_denies`, `aml_hold_active`, `aml_screening_enqueue_due`, `aml_screening_since`, `aml_screening_covers`, `aml_screening_gate_overdue` and `aml_screening_parked_*` are **md5-identical from 0194 through 0198**; (ii) the 0189/0190 **screening-policy** functions differ **only** in the lock line and do **not** adopt the trigger-policy pair helper; (iii) because the pair helper replaces the latest/in-force queries (`0191:187-193`, loosen validator), the helper mutants M60-M62 (in-force by `now()` instead of `clock_timestamp()`, latest filtered by `effective_from`, ordering without `id`) must be killed by the **existing, unchanged** suites `internal/aml/screening_d4_integration_test.go`, `internal/httpserver/aml_trigger_policy_tighten_only_integration_test.go` and `internal/licensingchange/screening_trigger_loosening_integration_test.go`; (iv) before/after **decision tables** over a fixed population, including the returned jsonb of `screening_trigger_loosen_apply` and `screening_policy_loosen_apply`, are identical. **The decision table also covers `screening_loosening_settings_apply`** for every (current state × payload) pair: identical outcomes, except that the N8 race rows now refuse `classification_changed_refile` where 0196 inserted a row (an intentional, listed difference). Runs in the write-quiet window | the same decisions; no runtime-reachable advisory keys **in this slice** |
| **0198_screening_gate_activation_guard** (contract) | write-quiet window (operations M1); **v2.2 (operations N3, DB N4):** pause the gate monitors and the worker. **Mechanism (v2.3, DB V6):** this is a **WHOLE-PLATFORM maintenance window** that the owner and operations must sign off (new owner decision **OD-G15**, activation-blocking for applying 0198 to staging or production). The worker and the monitor run **inside every API instance** (`main.go:799-810`), so the write-quiet window **scales the API instances to zero** (maintenance page), runs 0198, and scales back. A configuration maintenance flag would need a restart anyway and adds a new runtime switch, so scaling is preferred. Runbook entry: s7.7. Then the **first statement** is `LOCK TABLE aml_screening_trigger_policies IN ACCESS EXCLUSIVE MODE` (one lock request: ADD COLUMN needs it anyway; no upgrade from a weaker lock). It is held for the minimum time under `lock_timeout = '5s'`. **Stated cost:** while it is held, the withdrawal-path reads of the trigger policies stall (bounded by that timeout; the payout path retries safely as today, parked rather than rejected: **to be confirmed by ledger-finance before it goes into a runbook**; with the instances scaled to zero, in-platform readers are absent and the stall concerns only out-of-band clients); `created_at`-ahead pre-flight (s3.1); legacy-pending pre-flight (OD-G13); cut-over row + nonce; inventory (tenant by tenant + non-vacuity); columns `gate_authorisation_id`, `gate_enabling_policy_id`, **`inserted_at`** (v2.3; no CHECK) + FKs + index; the `_99_activation_guard`; the policy-stream chain trigger (**fresh start: the existing rows are covered by the inventory digest, not chained individually**; DB M7); `ENABLE ALWAYS` on `_append_only`, `_no_truncate`, `database_environment_no_truncate`; classify, reconcile, monitor summary, `screening_gate_settings_present`, catalogue check and baseline | new enabling refused everywhere until configured; existing gates unchanged |
| R2 | monitor loop; read and reconcile routes; Back Office badge and filing forms | alerts start |
| phase 2 (separate) | worker role split | needs role provisioning |

### 7.2 Rollback (down; v2.1 narrows the refusal conditions, DB L4)

| Down | Refused (`GA095`) when | Otherwise |
|---|---|---|
| 0198 | the anchor is `production`, `staging` or unrecorded; OR any settings, authorisation, revocation, attestation, freeze or lift row exists; OR any policy row has a non-NULL `gate_authorisation_id` or is a **gating** row created after cut-over | drops the guard, columns, cut-over, inventory, the policy-stream chain trigger and its chain rows, classify, monitor; restores the original trigger modes; byte-equal to 0197. **Note:** in development and test this deletes the policy-stream chain rows of non-gating post-cut-over rows (evidence loss accepted only under those anchors) |
| 0197 | (v2.3, DB V7) refused **only if any 0198 object exists**. Lock rows are mutexes, not evidence: the down deletes non-seed lock rows. `last_created_at` is used only by the 0198 guard. Allowed in development, test **and staging** otherwise | restores **the 0191-era bodies as last redefined before 0197** (advisory locks) byte for byte |
| 0196 | anchor not development/test; OR any request of the five kinds exists | restores the 0191 classifier wrapper and approvals trigger **byte for byte** |
| 0195 | anchor not development/test; OR any chain row beyond genesis; OR any evidence row | drops everything, including the `txid` column |

Down tests record the anchor **before** running (DB L4). Fingerprints compare columns by **name and type, not `attnum`**: ADD then
DROP COLUMN leaves an attnum hole.

**Sanctioned guard maintenance** (operations M2): a guard change is always a **forward migration** inside a declared window.
`DISABLE TRIGGER` is never a maintenance step. It is p1 by design, and the backup tooling bans it (F17). In practice, rollback in
staging and production is forward-fix only.

### 7.3 Mixed versions

| Combination | Behaviour |
|---|---|
| old app (pre-R1) + 0198 | gating author call gives 400 without a code; the governed loosening path, when the guard refuses with LC050, gives 409 with a guard code (no 500, DB H4); no monitor. **Not allowed**: R1 must be at 100 % first (the pipeline checks the version probe of every instance) |
| R1 + 0194 | features off (the probe); the gate as today |
| R1/R2 + 0198 | target |

### 7.4 No activation by migration or service start (A-17, corrected)

1. After a clean `up`: zero rows in settings, attestations, confirmations, worker attestations, authorisations, revocations, freezes
   and lifts. The chain holds **exactly** the 0195 genesis row plus the 0198 cut-over row. No policy row exists. Lock-table rows are
   exactly the seeds. This resolves the v2 contradiction with I6, which counted per-row chain backfill.
2. Upgrade from a 0194 fixture with N gating runs:
   - the canonical digests of all policy rows are byte-identical before and after;
   - every run is `legacy_unauthorised`;
   - the `aml_screening_gate_denies` decision table over the fixture population is identical before and after.
3. A static pin over the migrations and Go production files (as v2).
4. Boot `cmd/platform-api` (E2E harness): the counts are as in step 1.

### 7.5 `deploy/init-app-role.sql` (v2.1)

- Grants: the five `_apply` executors, the five filing-time `_assert` helpers, `screening_gate_classify`, `screening_gate_reconcile`,
  `screening_gate_monitor_summary`, `screening_gate_settings_present`, `screening_gate_enable_preview`.
- **Ordering** (DB L5): every grant runs before a closing prefix-based `REVOKE ALL ... FROM PUBLIC, igaming_runtime` for
  everything **not** on the allow-list, so a re-run can never leave an extra grant.
- No role creation for production. `deploy/security/init-gate-roles.sql` (attestor, break-glass, verifier, ddl-audit owner) is for a
  role administrator only.

### 7.6 OpenAPI, UI and E2E changes (QA H-2)

- **OpenAPI:** the s9 codes go into `AMLScreeningRefusalCode` and the licensing-change refusal schema (`platform-api.yaml:13060`,
  `:13101`). New read routes as v2. Full YAML validated; vitest OpenAPI contract tests run.
- **Immediate enabling breaks these, and each must change:**

| Place | Change |
|---|---|
| `internal/httpserver/aml_trigger_policy_tighten_only_integration_test.go:37`, `:60` | future-dated gating bodies become immediate. A new assertion: a future-dated enabling gives `gate_enabling_must_be_immediate` |
| `backoffice/e2e/lib/wd_aml.ts:195-199` (next full minute), `:234-241` (wait until effective) | gate-on submits "now" and asserts immediate effect; the wait stays for continuations only |
| `backoffice/src/features/aml/ScreeningPage.tsx:207` | when `gate_withdrawal` turns a gate **on** (new enabling), the date picker is hidden and "now" is sent; continuations keep the picker |
| Back Office vitest pins of that form | updated with an explicit "intentional pin change" entry |

- **Test (A-24):** future-dated enabling is refused for every writer (routed author, owner setter, raw owner INSERT). The governed loosening
  never enables (existing).

### 7.7 Runbook completeness (operations M7)

The runbook section must contain:
- **the activation order** (v2.2, operations; s4.6 holds only the restore order), as **one ordered checklist with an owner per step**:
  1. owner decisions recorded (OD-G0 and the s12.2 list), including OD-G10 with after-hours criteria;
  2. roles provisioned with named logins (role administrator);
  3. watchdog running (operations);
  4. separate owner role, or the S-b waiver recorded (s19);
  5. settings via the governed kind (two officers);
  6. instance attestation plus confirmation (two attestors);
  7. worker attestation (attestor);
  8. capacity report and preview reviewed (approvers);
  9. `screening_gate_authorise` (requester plus two approvers);
  10. the officer enables;
  11. the manual chain-head anchor is recorded in the change ticket (Tier S) or by the external verifier (Tier N);
  12. the named daily reviewer is assigned for the activation period.
- **OD-G10 after-hours:** the criteria under which on-call may execute a gate-off (a release decision), the named approver **role**, and how
  the emergency owner credential and the declared window are obtained (secrets-manager break-glass entry, ticket, second person);
- an **acknowledged-until** mechanism for `lapsed`/`revoked` p2 problems: the monitor suppresses re-raising an acknowledged problem code
  until a recorded date, which itself expires (operations; this is alert-noise control, not a state change);
- `head_export_unconfigured` (p3) while the D3b store is not configured;
- `gate_capacity_exceeded` remediation: re-authorise with new capacity evidence, or a governed gate-off under OD-G10; never automatic;
- a shared environment must **never** be anchored development or test (operations);
- the `gate_retry_later` procedure (s4.9);
- the **0198 write-quiet window** (v2.3): announce; scale API instances to zero; run 0198; verify; scale up; reconcile. The payout-path claim is pending ledger-finance confirmation;
- **a per-problem triage table** (every s11 code: meaning, the first query to run, the remedy, who acts);
- tamper decision trees for `gate_unexplained`, `evidence_chain_broken`, `guard_not_intact` and `ddl_outside_migration` (preserve evidence, stop
  enabling with a freeze, escalate to security, never "fix" rows);
- escalation contacts (roles, not names);
- the break-glass procedure and evidence template;
- alert-flood handling (one alert per problem code; how to acknowledge);
- the p3 ticket route;
- the trapped-funds path (s4.8);
- the after-restore and after-restart procedure;
- the D1 declared-window procedure.

Every alert payload carries the runbook anchor.

### 7.8 Verification matrix (v2.1 additions)

As v2, plus:
- **V3b (v2.3 final):** each function changed by 0197 is compared against a **checked-in EXPECTED DIFF**, `migrations/testdata/0197.expected.diff`, made of the exact hunks of normalised `prosrc` (whitespace and comments normalised) between the 0196 and 0197 bodies. Every other function must be byte-identical. The s4.9 table is the prose summary; the expected-diff file is the pin.
- **V5c:** restore through `deploy/backup/restore.sh` under the drill's rules (backup by the BYPASSRLS `igaming_backup`, restore without
  weakening RLS). This is a feasibility proof for classification after restore (QA M).
- **V13 (concrete deliverables, v2.2):** `deploy/drill/verify.sql` lines for chain verify, completeness, classify and reconcile; `deploy/backup/pitr-drill.sh` asserts `instance_attestation_stale` after PITR; `deploy/drill/tamper.sql` adds one guard-disable step whose detection is the negative control; a break-glass drill script on a throwaway cluster with the real NOLOGIN roles; the written RPO/head-ahead rule. The earlier wording follows. Drill additions to `deploy/drill/verify.sql` (operations M4): chain verify, classify and reconcile after restore; a PITR restore shows
  `instance_attestation_stale`; a tamper negative control (`deploy/drill/tamper.sql` gains one guard-disable step that must surface in
  reconcile); the D3b head-ahead rule; a real NOLOGIN-roles drill.
- **V12b:** guard, executor, monitor and verify cost measured at 10,000 persons per tenant, recorded against the CAPACITY plan's figures
  (operations H3).

---

## 8. Threat model (v2 rows T1-T15 kept; T16-T21 added in v2.1)

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
| T10 | consistency | concurrent actors | approve vs revoke; author vs revoke | lock rows `gate:*` (v2.1; was K3/K4) + READ COMMITTED | a lapse between INSERT and COMMIT (seconds, alerted) |
| T11 | tenant isolation | tenant staff | classify or read other tenants | session-tenant bound (AT002); platform-only for all-tenant reads | none new |
| T12 | platform metadata | runtime | `screening_gate_monitor_summary` | counts per problem code only, no ids | aggregate counts visible to the runtime role |
| T13 | deploy window | operator | old app with new schema | guard is in the database; fail-closed | no alerts until R2 (ordering rule) |
| T14 | security posture | operator | down migrations | `GA095` outside development/test or with evidence | none |
| T15 | everything | superuser | catalog edits, disable event triggers, `session_replication_role` | D5 log shipping to a store it cannot edit; cloud MFA | **cannot be prevented in the database** |
| T16 | governed gate-off and activation availability | any session | lock squatting (P18) | lock rows only after admission; `lock_timeout`; watchdog; contention report | an admitted holder until the watchdog fires (N s); none if the watchdog is absent (activation-blocking) |
| T17 | correctness of instance-wide checks | design defect | FORCE RLS empty reads (fail open) | platform-level evidence; tenant loop + non-vacuity (I14) | a defect in a new cross-tenant read without the assertion (A-23 pins the known ones) |
| T18 | role separation | staging operator | owner = RDS master (P17) | separate owner role; `gate_role_separation_violated`; startup check | if the owner decision for Tier S waives it (s19) |
| T19 | logical replication | upgrade operator | ALWAYS insert guards breaking apply, or replica-mode bypass | the s5.5 split | superuser replica-mode bypass of insert guards (detected) |
| T20 | detection itself | owner / API compromise | replacing in-DB verify or monitor; silencing alerts | external verifier, client-side hashes, dead-man's switch | **alert delivery is OPEN (F20)**: until ALERT-DELIVERY-1 is closed, a p1 may reach nobody |
| T21 | evidence semantics | owner forging a consistent authorisation and stamp | admission guards disabled | external only (D1, D3b, D5) | in-database classification shows `authorised` (s3.3 honesty) |

## 9. Closed refusal codes (revised)

- **Guard** (AT004, or LC050 when the row comes from a governed executor; s4.1):
  `gate_authorisation_required`, `gate_authorisation_lapsed`, `gate_authorisation_revoked`,
  `gate_authorisation_identity_mismatch`, `gate_authorisation_environment_mismatch`, `gate_instance_attestation_missing`,
  `gate_worker_attestation_missing`, `gate_activation_not_configured`, `gate_activation_frozen`, `gate_integrity_unresolved`,
  `gate_integrity_read_inconsistent` (new), `gate_tenant_has_unauthorised_gate`, `gate_enabling_must_be_immediate`,
  `gate_isolation_refused`, `gate_capacity_exceeded`, `gate_authorisation_stamp_forbidden`, `gate_screening_policy_changed` (new),
  `gate_role_separation_violated` (new).
- **Executors and assert helpers** (LC050): `gate_authorisation_not_configured`,
  `gate_authorisation_not_supported_for_tenant_licensing_model`, `gate_authorisation_environment_refused`,
  `gate_authorisation_duration_exceeds_configured_maximum`, `gate_authorisation_duration_below_minimum` (new; 3600 s),
  `gate_capacity_report_required`, `gate_compliance_reference_required`, `gate_authorisation_revoked_before_execution`,
  `gate_adoption_target_invalid`, `gate_adoption_target_unexplained`, `gate_adoption_digest_mismatch`, `gate_guard_not_intact`,
  `gate_authorisation_not_found`, `gate_authorisation_already_revoked`, `gate_settings_environment_refused`,
  `gate_authorisation_binding_mismatch` (new: `cutover_nonce`, `database_oid` or `instance_attestation_id` not equal to live),
  `gate_freeze_not_active` (new, for a lift), plus the reused `officer_grant_required`, `officer_quorum_not_met`,
  `gate_role_separation_violated`, `gate_integrity_read_inconsistent`.
  **Removed in v2.1:** `gate_approvals_below_configured_minimum` (approvals are pinned to 2).
- **Retryable** (mapped in Go, not a HINT): `gate_retry_later` for `55P03` / `40P01` (aml routes 409 with Retry-After; licensing change
  keeps its existing `ErrRetryableLockTimeout`).
- **Go:** `gate_authorisation_unavailable` (the probe says features are off).
- **Owner-side functions** (GA, no HTTP): `GA001 attestor_role_required`, `GA002 breakglass_role_required`,
  `GA003 breakglass_incident_reference_required`, `GA004 attestation_validity_out_of_range`, `GA005 gate_freeze_exceeds_cap`,
  `GA006 breakglass_noop_refused`, `GA007 attestation_not_on_primary`, `GA008 attestation_confirmation_by_same_login`,
  `GA009 gate_role_separation_violated`. Migrations: `GA095` (down refused), `GA098` (pre-flight, including
  `legacy_pending_enabling_present` as its detail).
- Reused unchanged: `gate_environment_refused`, `trigger_gate_enable_refused`.
- **v2.2 additions:**
  - guard: `gate_authorisation_tier_mismatch` and `gate_tier_s_requires_synthetic` (AT004/LC050 by scope);
  - executors: `classification_changed_refile` (LC050; v2.3: also raised by `screening_loosening_settings_apply`);
  - owner side: `GA010 replica_mode_refused` (the replica-mode refusal trigger) and `GA098` details `created_at_ahead_of_clock_present`
    and `legacy_pending_enabling_present`;
  - retryable: `40001` is mapped to `gate_retry_later` as well (ledger N-2).
- **Parity rule (A-19):** every HINT raised anywhere in 0195-0198 is in `aml.RefusalCodes`, in the licensing-change refusal table and in the
  OpenAPI enum. A static test extracts the HINT literals from the migration files.

## 10. Audit events

| Event (action) | Written by | Where | Content |
|---|---|---|---|
| `aml.screening_gate_authorised` / `aml.screening_gate_adopted` / `aml.screening_gate_authorisation_revoked` / `aml.screening_gate_settings_configured` | Go kind executors (`auditLoosening` pattern) | `audit` (same transaction) + chain | change_request_id, payload_hash, the executor's jsonb outcome (ids, validity, identity), reason code, IP, request id |
| licensing-change request, approve, reject, cancel, execute, expire | existing flow | existing | unchanged |
| `aml.screening_gate_enabled` | trigger-policy route (existing staff mutation audit) | audit | adds `gate_authorisation_id` to metadata |
| `aml.screening_gate_enable_refused` | route denied audit | audit | the closed code |
| instance attested, worker attested, break-glass revoke, revoke all, freeze | database functions (no staff session) | evidence row + chain row (`session_user`, `inet_client_addr()`, `application_name`) + D5 logs | ticket or incident reference, reason |
| reconcile and monitor runs | monitor loop | alerts (s3.5); external head export (D3b) | problem codes and counts |

**v2.1 additions:** These are added to the v2 list: `aml.screening_gate_freeze_lifted` (governed); `instance_attestation_confirmed` and
`breakglass_freeze` with its drill flag (database: evidence row + chain + D5). Every governed audit carries `persons_at_execution`,
`persons_held_on_enable` and `screening_policy_id`.

## 11. Reconciliation and monitor contract (v2.1)

**v2 contract (kept; its severity column is SUPERSEDED by the per-environment mapping below, and the monitor-loop paragraph by s5.6):**

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
| `gate_legacy_unauthorised` | p1 ; p2 | state 7 (v2.2 fix: precedence numbering of s3.3) |
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

**v2.1 changes:**

- Signatures are as v2.
- The reconcile and summary functions loop over tenants with `set_config` save and restore. Each per-tenant count is asserted against the
  platform-level count. A mismatch is reported as `reconcile_read_inconsistent` (p1). It is never silently empty.

**Severity per anchored environment** (operations H1). In development and test **every** problem is p3, so tests that tamper on
purpose never page. In staging and production:

| p1 | p2 | p3 |
|---|---|---|
| `gate_unexplained`, `gate_environment_mismatch`, `gate_legacy_unauthorised`, `legacy_pending_enabling`, `evidence_chain_broken`, `guard_not_intact`, `inventory_digest_mismatch`, `evidence_row_without_request`, `executed_request_without_row`, `ddl_outside_migration`, `reconcile_read_inconsistent`, `monitor_unavailable`; `ddl_audit_missing` (**Tier N only**) | `gate_identity_mismatch`, `gate_authorisation_lapsed`, `gate_authorisation_revoked`, `gate_without_attested_worker`, `gate_capacity_exceeded`, `instance_attestation_stale`, `anchor_unrecorded`, `gate_lock_contention`, `breakglass_membership_active`, `chain_behind_external_head`, `monitor_check_failed:<problem>` | `gate_authorisation_expiring`, `gate_screening_policy_changed_since_authorisation`, `authorisation_created`, `adoption_created`, `revocation_created`, `freeze_active`, `attestor_membership_stale`, `ddl_in_declared_window`; `ddl_audit_missing` (Tier S) |

**Cost bounds** (operations H3):
- `gate_capacity_exceeded` in the monitor uses an estimate: `pg_class.reltuples` scaled by the tenant's share, refreshed by an exact count at
  most hourly. The guard and the executors always count exactly; it is a rare operation (rejected for the guard: s18).
- Chain verify in the monitor is incremental from a watermark.
- The monitor is single-flight with `statement_timeout`.
- V12b measures all of it.

**Metrics (operations, v2.2):** loop duration; last successful pass; a problem gauge by code; attestation age; authorisation expiry gauge;
guard refusals by code; `monitor_skipped`; watchdog terminations.

**v2.2 problem additions:**
- p1 in staging and production: `role_separation_violated`, `chain_entry_missing`, `chain_entry_orphan` (completeness);
- p2: `role_separation_waived` (S-b), `gate_lock_holder_terminated`, `clock_regression_clamped`, `created_at_ahead_of_clock`;
- p3: `head_export_unconfigured`;
- (v2.3) `watchdog_absent`: p1 in Tier N, p2 in Tier S (p3 in development and test);
- (v2.3) `gate_lock_long_holder`: p3.

Development and test stay capped at p3.

**Discriminator:** one open alert per problem code. **Routing:** severity is the only routing key the dispatcher supports today (F20).
Per-environment behaviour is therefore achieved by the severity mapping above, not by routing. OD-G11 is rewritten accordingly.

---

## 12. Owner decisions (v2.1: split by what they block)

### 12.1 Block MERGING the implementation

| ID | Decision | Options | RECOMMENDATION |
|---|---|---|---|
| OD-M1 | Authorise a **role administrator** to provision, in the CI database definitions, the E2E harness cluster and agents' private scratch databases (v2.2 full list, QA): the NOLOGIN attestor role with **two distinct attestor logins**; the NOLOGIN break-glass role with one member login; **a login that is a member of the owner role** (to prove owner exclusion); **an owner configuration with CREATEROLE** (and, where emulable, an `rds_superuser`-like membership) to exercise `gate_role_separation_violated`, otherwise listed as untestable with the substitute in s15.5; the BYPASSRLS verifier role (a superuser step); the watchdog role with `pg_signal_backend`. This is needed because every money-path test that enables a gate needs a real attestation (ledger H-1, QA C-1). No test-only bypass exists, and `t.Skip` on a missing role is banned. | (a) provision in CI and scratch via a human-approved change to the CI definitions and the scratch-db script; (b) block the implementation | **(a)**. Under CLAUDE.md, agents never create roles: the human or orchestrator applies it |
| OD-M2 | Accept the merge with alert delivery still OPEN (ALERT-DELIVERY-1). Merge is strictly more restrictive than today. | accept; or block on delivery | accept for merge; delivery blocks activation in Tier N (s19) |

### 12.2 Block ACTIVATION (per environment)

| ID | Decision | Options | RECOMMENDATION |
|---|---|---|---|
| **OD-G0** | **Tier definition and per-tier controls (s19)** | Tier S/N as proposed; a single tier; other | owner decides. The two reviewer positions are presented in s19 without a decision |
| OD-G1 | maximum validity, warning window | 30 / 90 / 180 days (≥ 3600 s floor) | 90 days, 14-day warning |
| OD-G2 | production activation | (a) none until the Tier N prerequisites, a real provider and a separate migration lifting I8; (b) per-tenant owner signature; (c) never | (a) |
| OD-G3 | capacity evidence and cap | report + cap required / report only / neither | report + cap for staging and production. It is head-count only; throughput is in the report |
| OD-G4 | approver population; compliance reference; **future**: more than 2 approvals (needs a primitive change, s6.1) | | 2 officers (pinned), compliance reference required, primitive change deferred |
| OD-G5 | own-licence tenants | keep refused / design | keep refused |
| OD-G6 | deadline for legacy gates | | adopt or replace within an owner period; a gate-off is a release decision under OD-G10 |
| OD-G7 | break-glass holders; freeze cap (≤ 72 h structural); early lift (two Persons); emergency gate-off excluded | | revoke, revoke-all and freeze only; 24 h cap; quarterly drill |
| OD-G8 | attestation maximum age; **confirmations (1 or 2)**; attestor population; acceptance of R-3 | | 30 days; 2 in staging and production; R-3 accepted in writing |
| OD-G9 | phase 2 worker split | | before Tier N |
| OD-G10 | withdrawal-release policy (a gate-off releases screening-only holds), **including after-hours criteria, the named approver role and how the emergency owner credential and declared window are obtained** | not invented | decide before activation and before any gate-off outside development (ledger non-negotiable) |
| OD-G11 | alert severity mapping as s11 (routing by severity only) + ALERT-DELIVERY-1 | | as s11; close ALERT-DELIVERY-1 for Tier N |
| OD-G12 | external store (D3b, D5); KMS signing (R-5) | | WORM store in a separate account; KMS ADR |
| OD-G13 | legacy **pending** gating rows at cut-over | (a) refuse to apply 0198 while any exist; (b) apply and classify p1 | (a) in staging and production; (b) in development and test |
| OD-G15 | (v2.3 final) the whole-platform maintenance window for applying 0198 (API scaled to zero) | approve a window / defer 0198 | approve per environment, with operations sign-off |
| OD-G14 | staging owner role: a separate owner, not the RDS master (P17) | S-a enforced in Tier S (default) / S-b waived in Tier S with the exact acceptance of s19 | part of OD-G0. The design is consistent under either option: enforcement is the setting `role_separation` |

## 13. Engineering choices

E-1 reuse licensing change (s6.1). E-2 classification derived, never stored. E-3 legacy by inventory membership, not timestamp. E-4
guard codes on `AT004` and kind codes on `LC050` for old-app compatibility; new class `GA` only for non-HTTP functions. E-5 `ENABLE
ALWAYS` on guards and deny triggers. E-6 enabling rows immediate. E-7 an authorisation is reusable within validity (not a single-use
token). E-8 continuation means both the latest and the in-force rows gate. E-9 keys K3 to K5 and the global order. E-10 attestor and
break-glass are NOLOGIN roles distinct from the owner; the owner is explicitly excluded. E-11 attestation bound to postmaster start
and timeline. E-12 explicit production anchor in provisioning plus `anchor_unrecorded`. E-13 adoption exempt from worker attestation.
E-14 monitor loop in every environment. E-15 the guard is SECURITY INVOKER (runs as the definer function or the owner). E-16
containment is per tenant; instance-wide only for integrity states. E-17 the guard is a separate trigger (named `_99_` since v2.1, superseding `_20_`); the `_10_` guard is
not copied or changed.

Superseded in v2.1: E-5 by E-23 (ALWAYS only for deny triggers), E-8 by the s3.1 continuation rule, E-9 by E-18 (lock rows). **v2.1 additions:** E-18 lock rows, not advisory keys. E-19 platform-level evidence tables. E-20 run identity by pointer column. E-21 forced
monotonic `created_at` instead of refusing on clock regression. E-22 two chain streams; gaps allowed. E-23 deny triggers ALWAYS,
insert guards and chain triggers ORIGIN. E-24 LC050 from the guard on governed paths. E-25 approvals pinned to 2. E-26 the clock-seam
helpers. E-27 a fresh chain at cut-over with the inventory digest as anchor. E-28 the version probe instead of function presence. E-29
the capacity work in a separate slice.

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

**v2.1:** v2's questions 1, 2, 4, 5 and 7 remain. Question 3 is answered by the `txid DEFAULT txid_current()` column. New questions:
- Q8: does RDS blue/green carry these tables' triggers in the same modes on green? To be verified in the staging drill.
- Q9: does `pg_is_in_recovery()`-gated attestation work for a managed failover where the new primary's timeline is visible immediately?
- Q10: can the version probe be read by every instance before 0198 runs (pipeline integration)? (v2.2: superseded by the per-instance probe deliverable.)
- Q11 (v2.2): the exact `pg_stat_activity.backend_type` strings of logical-replication apply and tablesync workers on PG16 and RDS (PG17 renamed them).
- Q12 (v2.2): the timeline read: `(pg_control_checkpoint()).timeline_id` lags a promotion until the next checkpoint. v2.2 captures **both** that and the timeline parsed from `pg_walfile_name(pg_current_wal_lsn())` (first 8 hex digits), and requires both to match (DB N8).
- Q13 (v2.2): whether `pg_read_all_stats` can be granted to the owner or monitor role on RDS for holder identity (DEFERRED with AWS).

---

## 15. Acceptance criteria (v2.1; all merge-blocking)

### 15.1 Criteria

Every criterion names its test layer, the mutants it must kill (s15.2), and its non-vacuity premise. **QA writes or reviews these tests
before implementation starts. The mutation run is performed by someone other than the implementer, and the survivor list is
evidence.**

| ID | Criterion | Layer | Kills | Non-vacuity |
|---|---|---|---|---|
| A-1 | Enabling without C(T,J) is refused on the routed author, the owner setter (owner pool) and a raw owner INSERT, with codes. "Zero rows written" is measured as unchanged row counts of the policy table, every evidence table, the chain, `audit` (except the denied-audit row the route writes by design) and alerts | integration | M01 M02 M08 | the same call succeeds after `AuthoriseGate` |
| A-2a | Legacy run: a tightening continuation is accepted with the pointer inherited | integration | M05 | the fixture has ≥1 legacy run |
| A-2b | Pending governed gate-off cancelled by a gating row: continuation, stamp inherited, allowed in **every** state | integration | M03 | a pending gate-off exists in force order |
| A-2c | Another J of the same T is refused `gate_tenant_has_unauthorised_gate` | integration | M28 | the same J of another tenant is allowed |
| A-2d | Adoption flips the classification; the gate decision table is unchanged | integration | M05 | ≥1 held person before and after |
| A-3 | No settings: `gate_activation_not_configured` and `gate_authorisation_not_configured` | integration | M06 | succeeds once configured |
| A-4 | Each owner parameter violated gives its code; validity floor 3600 | integration | M19 M24 | boundary values pass |
| A-5 | Lapse via the clock seam (explicit `p_now`) plus one end-to-end lapse with a 3600 s authorisation and an owner-forged `valid_until` on a private DB **through the guard**; gate decisions unchanged | unit-SQL + integration | M09 M10 M41 | before lapse: allowed |
| A-6 | Revocation by id and by `NULL` scope (seq coverage); gate decisions unchanged | integration | M12 M13 M42 | an unrevoked sibling stays current |
| A-7 | Deterministic interleavings (s15.4); no deadlock; lock-order matrix | integration | M14 M15 M16 | each case observes a waiter in `pg_locks` before release |
| A-8 | Non-READ-COMMITTED enabling refused; **gate-off under REPEATABLE READ succeeds** | integration | M17 | READ COMMITTED passes |
| A-9 | Catalogue pins s5.2 (trigger-aware writers, exposure, prefix loop, RLS, ACL, modes, search_path, last trigger) + post-suite mode check | integration-catalogue | M37 M38 | detector premise: the owner setter is detected as a writer |
| A-10 | I1: gate and monitor functions reference no `screening_gate_` object | static-catalogue | M40 | detector finds the guard's own reference |
| A-11 | Forged heartbeat without a worker attestation, or with one bound to a stale instance attestation: refused | integration | M11 | an attested worker passes |
| A-12 | Instance attestation currency through the pure helper (all six bound values, confirmations, primary-only) | unit-SQL | M21 M22 | the live tuple passes |
| A-13 | Restore (V5a, V5c) gives `identity_mismatch`; unrecorded anchor gives `environment_mismatch` | integration (private DB) | M23 | the source DB classifies `authorised` |
| A-14 | Adoption digest binding (canonical encoding with pinned GUCs; varied TimeZone/DateStyle in-session gives the same digest); `unexplained` refused | integration | M25 M26 | the correct digest passes |
| A-15 | Reconcile and monitor return every s11 code that is **runnable here**: all except the D1 codes (`ddl_outside_migration`, `ddl_in_declared_window`, `ddl_audit_missing` present-case), which are drill evidence. Also per-check isolation (`monitor_check_failed`) and `monitor_unavailable` by version | integration | M29 M30 | a clean fixture returns none |
| A-16 | Chain verify (in-DB and the external client-side reference implementation) detects edit, delete, insert, reorder and a forged `prev_hash`; tolerates seq gaps | integration + unit (Go verifier) | M31 M32 M33 M34 | an untampered chain with gaps verifies |
| A-17 | No activation by migration or start (s7.4) | integration + static + E2E | M35 M36 | the fixture upgrade has N > 0 gating runs |
| A-18 | Migration matrix (V1-V5, V3b, V7, V8), down refusals, fingerprints by column name. (V6 is covered by A-7, V9 by A-8; V12b is a performance record attached to the merge evidence, not pass/fail; V13 is drill evidence and activation-blocking) | integration (`cmd/migrate`) | M39 M59 | down-then-up equals the first up |
| A-19 | Parity of HINTs, RefusalCodes, licensing-change refusal table and OpenAPI; no jurisdiction literals | static | n/a | the extractor finds ≥ 40 hints |
| A-20 | Reviews: security (blocking), ledger-finance, qa; full integration suite, hosted CI, Playwright (spec 10 updated) green; the affected-test list (s15.3) all pass with their stated assertions; the trigger-policy writer pin **unchanged** | process | — | — |
| A-21 | **Gate-off succeeds** through the owner setter **and** `screening_trigger_loosen_apply` under each bad state: frozen, unconfigured, unattested, unexplained, chain broken, tripwire failing, non-READ-COMMITTED; and while `gate:global` and `chain:evidence` are held exclusively by another session (no wait). And (v2.3) while an adopt executor holds `policy:<T>:<J>` waiting on `gate:global`: a **bounded, retryable** wait, then success | integration | M07 M18 M55 | the same state refuses enabling |
| A-22 | No runtime-role session can lock any `screening_gate_locks` row (42501), and no advisory key remains in any function body | integration + static | M14 | the owner can lock |
| A-23 | Every cross-tenant read (guard step 6.6, classify, reconcile, monitor, inventory) raises or reports `_read_inconsistent` when its per-tenant read disagrees (fault injection: an RLS policy mutant) | integration | M20 | the consistent case reports nothing |
| A-24 | Future-dated enabling refused for every writer, **including a raw owner INSERT**; UI and E2E as in s7.6 | integration + vitest + E2E | M04 | a future-dated continuation is accepted |
| A-25 | A production-configured `platform-api` refuses to start against a non-production anchor; `VerifyRuntimeRoleSeparation` refuses where settings exist | Go integration | M38 | a matching configuration starts |

### 15.2 Mutation plan (QA H-3)

**Harness:** each mutant is a `CREATE OR REPLACE` of one function body (or one trigger-mode or ACL statement) applied as the owner in a
**private scratch database**, using the patching pattern the fixtures already use for controlled schema changes
(`internal/licensingchange/screening_loosening_integration_test.go:250-262`). Then the named tests run, and the mutant must be killed. The runner
is not the implementer. Survivors are listed as evidence with a disposition.

| Mutant | Change | Killed by |
|---|---|---|
| M01 | drop the guard's new-enabling branch | A-1 |
| M02 | guard ignores `created_by_scope`/owner path | A-1 |
| M03 | continuation test `F gating` becomes `L AND F gating` (the v2 rule) | A-2 (pending-off cancel) |
| M04 | drop the `effective_from <= now` check | A-24 |
| M05 | continuation does not copy stamp or pointer | A-2, A-15 (`unexplained` would appear) |
| M06 | skip the settings check | A-3 |
| M07 | non-gating early exit moved after the checks | A-21 |
| M08 | caller-supplied stamp accepted | A-1 |
| M09 | `<` becomes `<=` at `valid_until` | A-5 |
| M10 | max(valid_until) instead of the latest current by seq | A-5 |
| M11 | worker attestation not bound to the current instance attestation | A-11 |
| M12 | `NULL`-scope revocation ignored | A-6 |
| M13 | revocation coverage by `created_at` instead of seq | A-6 with an **owner-forged revocation on a private DB whose seq order differs from its clock order** |
| M14a | `gate:<T>:<J>` shared/exclusive swapped | A-7 |
| M14b | an advisory lock reintroduced in a slice function | A-22 |
| M15 | lock order inverted (`gate:*` before `policy:*`) | A-7 lock-order matrix |
| M16 | executor does not re-check revocation under the lock | A-7 |
| M17 | isolation check removed | A-8 |
| M18 | the chain trigger verifies or checks freeze | A-21 |
| M19 | validity cap not enforced | A-4 |
| M20 | per-tenant loop without non-vacuity | A-23 |
| M21 | stored identity compared with itself instead of live | A-12 |
| M22 | environment ignored | A-12, A-13 |
| M23 | `database_oid` not compared | A-13 |
| M24 | requester counted as approver; approvals older than 24 h counted | A-4 |
| M25 | digest computed from `row::text` / without pinned GUCs | A-14 |
| M26 | adoption of `unexplained` allowed | A-14 |
| M27 | skip the freeze check | A-39 (explicit) |
| M28 | skip the tenant legacy containment | A-2 |
| M29 | classification precedence swapped (`authorised` before `revoked`) | A-15 (cross-product oracle) |
| M30 | inventory membership replaced by the `created_at < cutover` test | A-15: a tamper fixture on a private DB inserts, after cut-over with the guard disabled, a gating row with a caller-supplied `created_at` before the cut-over time. It must classify `unexplained`, not legacy |
| M31 | chain ignores `prev_hash` | A-16 |
| M32 | row hash omits a column | A-16 |
| M33 | verify checks only the head | A-16 |
| M34 | verify requires contiguous seq | A-16 (gaps fixture) |
| M35 | 0198 seeds a settings row | A-17 |
| M36 | inventory writable after cut-over | A-17, A-9 |
| M37 | attestor check does not exclude the owner | A-9 |
| M38 | role-separation check removed | A-25, A-41 (with the OD-M1 owner configuration; otherwise substitute evidence, s15.5) |
| M39 | down not refused in staging | A-18 |
| M40 | the gate predicate reads `screening_gate_*` | A-10 |
| M41 | an authorisation treated as single-use (consumed at enabling) | A-5 (re-enable within validity) |
| M42 | lapse or revocation writes a policy row or touches `aml_holds` (I2, I3) | A-6 with **row-count and digest snapshots of policy, holds, lifts, payouts and ledger before and after** lapse and revocation (plus the A-9 writer pins) |

### 15.3 Fixtures (QA H-1, ledger H-1): one sanctioned path, no bypass

- **`amlfix.AuthoriseGate(t, w, tenant, jur) uuid`** is the only sanctioned helper. It:
  - ensures the settings row (the governed configure with the fixture's officer Persons, idempotent per DB);
  - **attests the instance through a real attestor login** (re-attesting when not current, so it survives restarts and fresh scratch DBs;
    `t.Fatal` if the role is missing, **never `t.Skip`**);
  - attests the MOCK worker;
  - files and approves `screening_gate_authorise` with two officer Persons;
  - returns the authorisation id.

  There is **no test-only guard bypass**.
- **Call sites to migrate** (QA census): the 44 `setTrigger(..., gate=true)` callers; the `tw.gate()` users in the lic27b, lic29, lic30
  and d10 suites; `triggerInput` in `internal/aml/screening_trigger_integration_test.go:109-130`; lic18 `:282-286`; lic23 `:116-120`;
  adversarial `:269`; d10 `:358`.
- **Trapdoor fixtures** (F23; four sites, not three). They disable only `_10_guard` to insert actor-less rows. With v2.1 the
  `_99_activation_guard` stays enabled, so a gating row they insert needs `AuthoriseGate` first (their backdated `effective_from` is
  allowed: it is `<= now`). They re-enable with the mode from the s5.5 table through `amlfix.RestoreTriggerMode`. The post-suite check
  (A-9) catches a wrong mode.
- A-20 lists every affected test file with the assertion it must keep unchanged (for example "held person stays held",
  "loosening never enables"). The implementation record must show each one passing with the same assertion text.

### 15.4 Deterministic interleavings (QA H-5)

The test session holds the lock row (`SELECT ... FOR UPDATE` as the owner). It starts the operation in a goroutine, **polls
`pg_locks` and `pg_stat_activity`** until that pid waits on the row, then releases. There are no sleeps.

Cases:
- author vs revoke (both orders);
- approve/execute vs revoke;
- break-glass revoke-all vs enabling;
- settings narrowing vs an enabling past step 6;
- freeze vs enabling;
- two enablings of the same (T, J);
- two executors of the same request;
- authorise vs settings change;
- gate-off while `gate:global` and `chain:evidence` are held (must not wait);
- **(v2.3, security N8)** configure race: hold `loosening:<T>`; file A "disable" and B "enabled, longer cooling" (both classified narrowing); release so that A commits first; B must be refused `classification_changed_refile`, and the capability stays disabled.

A **lock-order matrix test** parses each function's sequence of `screening_gate_lock` calls from `prosrc` and asserts it follows the
s4.9 order.

### 15.5 Other test obligations (QA M items)

- **Classification cross-product oracle:** a Go oracle over every combination of the s3.3 inputs (about 256 combinations) times 4 anchors,
  compared with `screening_gate_classify`.
- **Error-precedence tests:** for each pair of failing preconditions, the HINT of the higher-precedence one wins.
- **Restore simulation feasibility:** V5c.
- **Existing pins that change** (each listed as an intentional pin change):
  - `internal/licensingchange/kinds_test.go` kind counts: **16 → 21** in v2.1 (QA counted 20 with four kinds; v2.1 adds `screening_gate_freeze_lift`);
  - the staff step-up read count;
  - the golden route table (two read routes);
  - the staff mutation-audit static test;
  - the `schemaSnapshot` extension;
  - backup baselines and drill fingerprints (new tables and trigger modes).
- **Cannot be tested here** (substitute evidence): event triggers (needs superuser; substitute: a staging drill record), pgaudit and log
  shipping (operations evidence), a physical clone (substitute: the pure-helper tests A-12 + a drill), ALWAYS triggers firing under
  `session_replication_role = replica` (superuser; substitute: catalogue mode pin + a CI-superuser test only if the human approves),
  RDS function privileges (staging probe), real two-Person independence (process audit).
- The s2 claim "verified by qa and code-reviewer" was replaced by "to be verified".

### 15.6 v2.2 additions (criteria, mutants, tests)

| ID | Criterion | Layer | Kills | Non-vacuity |
|---|---|---|---|---|
| A-26 | Lock-key CHECK accepts every key form of s4.9 (seeds and per-(T,J)) and rejects malformed keys | static + SQL | M76 | a malformed key is rejected |
| A-27 | **No lock before admission**: catalogue parse (callees expanded transitively); behavioural: a session sets `app.platform_admin_principal_id`, calls each assert and preview, and holds the transaction; a concurrent governed gate-off completes without waiting. **Plus (v2.3):** a second session holds `policy:<T>:<J>` through an **admitted** routed author with its transaction left open; the governed gate-off waits and fails with retryable `55P03`, and once the watchdog (A-43) terminates the holder, the retried gate-off completes | catalogue + integration | M64 | the same assert run as the executor does lock |
| A-28 | **Guard-level wiring** (QA High): for each bound value (`system_identifier`, `timeline_id` from both sources, `postmaster_start_time`, `database_oid`, `environment`, `cutover_nonce`, `tier`), an owner-forged attestation or authorisation on a private DB with that **single value** changed is refused **by the real guard** with the matching HINT | integration (private DB, owner) | M21 M22 M23 M45 M65 | the unforged row passes |
| A-29 | **Break-glass**: owner exclusion; non-member refusal; idempotency by `incident_reference`; `breakglass_noop_refused`; freeze cap and drill cap; governed two-Person lift; membership reporting; no break-glass function writes the policy table, settings or authorisations (writer pin) | integration (OD-M1 roles) | M68-M75 | a member login succeeds |
| A-30 | Tenant isolation of reads: every function in the s5.2 read pin gives AT002 for a foreign tenant; `p_tenant` NULL is platform-only | integration | M77 | own tenant reads succeed |
| A-31 | Two-stream routing (a policy row lands only in its pair stream, under the pair lock); a **differential test of Go vs SQL hashes** over random rows; a rollback-gap fixture verifies | integration + Go unit | M51 M31-M34 | equal hashes on the sample |
| A-32 | 0198 behaviour: `GA098` abort on a non-vacuity mismatch; `relforcerowsecurity` restored afterwards; a concurrent writer blocked during the lock; OD-G13 both branches; a **Go oracle for `legacy_enabling_policy_id`** over randomised legacy histories | integration (`cmd/migrate`) | M56 M57 M58 M54 | a clean history passes |
| A-33 | Ordering vs wall time: clamp cases (clock behind; equal; ahead), `inserted_at` forced, the clamp trace (chain mark + `clock_regression_clamped`), `created_at_ahead_of_clock`, and the REPEATABLE READ gate-off race giving `40001` (ledger N-2) | integration | M52 M66 | a normal-clock insert is not marked |
| A-34 | `gate_enabling_policy_id` cases: caller-supplied refused; enabling = own id; continuation = F's enabling; legacy continuation = the inventory enabling row | integration | M53 M54 | — |
| A-35 | Severity mapping on scratch DBs anchored `staging` and `production` (and p3-capping in development and test) | integration | M78 | the development DB shows p3 |
| A-36 | Stale version probe: features stay off until the refresh after `up` | Go integration | M79 | after refresh, features are on |
| A-37 | Property test: random sequences of enabling, continuation, gate-off, pending-off and cancel vs a Go oracle of s3.1 | Go + integration | M03 M05 M53 | the oracle and the database agree on a known sequence |
| A-38 | 0197 pins: md5-identical predicate bodies 0194 to 0198; screening-policy functions change only in the lock line; decision tables (incl. returned jsonb) identical; helper mutants M60-M62 killed by the **unchanged** existing suites | catalogue + integration | M60 M61 M62 | pre-0197 tables are non-empty |
| A-39 | Freeze: new enabling refused `gate_activation_frozen`; a gate-off while frozen succeeds | integration | M27 | after expiry, enabling succeeds |
| A-40 | Replica-mode refusal: an owner session with a superuser-granted test `SET session_replication_role` (only if the human approves a CI superuser step) is refused `GA010`; otherwise substitute: catalogue mode pin + drill | integration or substitute | M67 | — |
| A-41 | Extended role-separation checks (API login attributes, ADMIN OPTION, attestor is not the API login), with the OD-M1 configurations | integration (or substitute) | M38 | the separated configuration passes |
| A-42 | Tier binding: changing settings S→N makes S authorisations non-current (`gate_authorisation_tier_mismatch`); Tier S needs synthetic worker and heartbeat (`gate_tier_s_requires_synthetic`) | integration | M65 M80 | same-tier passes |
| A-47 | (v2.3, security N8) The configure race of s15.4 is refused `classification_changed_refile`; a filed-widening request that became narrowing executes; the screening-loosen and trigger-loosen executors re-validate under the lock (existing suites) | integration (deterministic interleaving) | M82 | the same B request filed **after** A commits is classified widening and needs two Persons |
| A-43 | The watchdog terminates a runtime backend holding a lock row longer than N s, detected by its granted RowShareLock on `screening_gate_locks` + `xact_start` (never by query text); it never terminates a non-runtime role; `watchdog_absent` is raised when its heartbeat is stale. **v2.3 final:** a squatter visible **only between gate-off retries** is still terminated (remembered blocker, or a runtime RowShareLock holder older than N) | drill (role `pg_signal_backend`) | — | the holder is visible beforehand |
| A-44 | **Every alert problem code has an existing runbook anchor** (merge criterion, operations N10) | static | — | the extractor finds every code |
| A-45 | Revocation idempotency with `NULL` `authorisation_id` (NULLS NOT DISTINCT) | integration | M81 | distinct incidents are both recorded |
| A-46 | The `screening_gate_enable_preview` pin (AT002, platform-only, bounded, no lock) | integration | M64 M77 | own-tenant preview works |

**Additional mutants (QA):**
- **M43** guard skips 6.6 (instance integrity); **M44** skips 6.10 (screening-policy changed); **M45** nonce or payload binding ignored;
- **M46** persons count without `app.tenant_id = T` (fails open); **M47** cap skipped; **M48** `valid_from` lower boundary off by one;
- **M49** instance confirmation by the same login accepted; **M50** heartbeat synthetic-flag mismatch ignored; **M51** policy row routed to
  the evidence stream, or pair lock skipped in the chain append; **M52** `GREATEST` dropped;
- **M53** caller-supplied `gate_enabling_policy_id` accepted; **M54** legacy pointer wrong; **M55** gate-off taking `gate:*` or `chain:evidence` (A-21);
- **M56** 0198 lock dropped; **M57** 0198 unfiltered-count non-vacuity removed; **M58** OD-G13 refusal removed;
- **M59a/b/c** down refusal on evidence row / stamped row / post-cut-over gating row removed;
- **M60-M62** pair-helper mutants (s7.1); **M63** SQLSTATE by scope swapped (behavioural: governed path must return 409 with the code, not 500);
- **M64** lock before admission in an assert; **M65** tier or binding equality ignored; **M66** validity by `created_at` instead of `inserted_at`;
- **M67** replica-refusal allows any backend; **M68-M75** break-glass (owner exclusion, membership, idempotency, no-op, freeze cap,
  single-person lift, a break-glass writer of the policy table, membership reporting);
- **M76** lock-key CHECK; **M77** read without tenant filter; **M78** severity mapping ignores the anchor; **M79** probe never refreshes;
- **M80** Tier S synthetic check removed; **M81** idempotency key without `NULLS NOT DISTINCT`;
- **M82** (v2.3) `screening_loosening_settings_apply` without the re-classification lines. Killed by A-47.

Kill mapping: M43 → A-28/A-15 integrity fixture; M44 → A-42/A-5 variant; M46 → A-23; M47 → A-4; M48 → A-5; M49 → A-12; M50 → A-11;
M55 → A-21; M63 → A-21 (governed path).

**Test logistics (QA):**
- `AuthoriseGate` retries on `gate_retry_later` (bounded).
- Choke points to change first: `trigWorld.setTrigger`, `triggerInput`, `gate()` and `amlfix`. Extra files: `screening_lic26` (9 calls), d1r2, d1r3, d1r4.
- The lock-order parse expands callees transitively and treats the gate-off path as a prefix of the order.
- Interleaving polls are bounded by 4 s, under the 5 s `lock_timeout`.
- The `schemaSnapshot` extension covers: new tables and columns by name and type, trigger modes, function md5s, ACLs, the lock-key CHECK text.
- The E2E harness gets an attestor login (OD-M1).

**Cannot be tested here (substitute evidence), v2.2 additions:**
- role-separation branches needing `rds_superuser` (substitute: catalogue unit tests over fabricated `pg_roles` rows through a pure helper);
- primary-only attestation (substitute: the helper takes `pg_is_in_recovery()` as an argument);
- the BYPASSRLS verifier (substitute: a drill);
- a real restart, PITR or blue/green (substitute: the drill and helper tests);
- the `idle_in_transaction` bound and the watchdog (drill);
- alert delivery and the D3b store (operations evidence).

## 16. What blocks MERGING vs ACTIVATING (v2.2)

**MERGING:** A-1 to A-47 except the drill-only A-43 (written or reviewed by qa first; mutation run by a non-implementer); OD-M1 (the full role list of s12.1,
human-applied in CI, the E2E cluster and scratch DBs); OD-M2; the runbook section; the init-app-role changes; parity. Merge does not need D1, D3b, D4, D5, delivery or any
OD-G decision. Without them the mechanism is inert and fails closed.

**ACTIVATING:** OD-G0 (tier) and the controls its tier requires (s19); OD-G1, G3, G4, G6, G7, G8, G10, G11, G13, G14; settings
configured through the governed kind; roles provisioned; legacy gates of the tenant resolved; the capacity slice done and its figures
accepted; Back Office forms; the **watchdog** (or the owner-accepted Tier S alternative, s19); A-43 and the V13 drills; the Tier-agnostic non-negotiables (s19). **Production additionally:** OD-G2, G9, G12,
a real provider (PROVIDER DEPENDENT), the sanctions false-positive policy, and a migration lifting I8.

## 17. Choices reviewers should attack (v2.1)

1. Platform-level evidence tables (E-19): is giving up tenant RLS on owner-only tables really free?
2. ORIGIN insert guards (E-23): a superuser in replica mode bypasses the activation guard. Detected, not prevented. Acceptable?
3. The single clock comparison left in "revoked before execution" (s4.4).
4. The lock-row residual (open-transaction holding) and its dependence on an operations timeout.
5. The two-stream chain (E-22): can a policy row be appended to the wrong stream or skip one under concurrency?
6. The `persons_held_on_enable` preview ignores the backfill grace: is it misleading to approvers?
7. The approval count pinned to 2 (E-25) until a primitive change.
8. Tier S/N (s19) is not decided here. Does either position leave a gap neither reviewer named?
9. v2 items 1 to 12 still stand, except 2 (resolved by M1), 6 (lock rows) and 10 (down rules narrowed).

## 18. Review dispositions

Status: **A** = accepted as proposed, **M** = accepted with modification, **R** = rejected, **O** = owner decision, **N** = not
itemised in the relayed findings (generic hardening applied; the reviewer must confirm).

| Finding | Status | Where fixed / reason |
|---|---|---|
| SEC-H1 advisory-lock DoS (K3/K4/K5 and existing K1/K2) | M | s4.9: lock rows on owner-only `screening_gate_locks`; K1, K2 and the 0189 key also replaced (0197); `lock_timeout`; reconcile reports **lock contention with holder identity** rather than "non-owner holders" (non-owner sessions can no longer hold rows directly); residual open-transaction holding stated; A-22 |
| SEC-H2 FORCE RLS fail-open | A | s4.0 platform-level evidence; I14; tenant loop + non-vacuity; persons count under `app.tenant_id = T`; break-glass no-op refusal; A-23 |
| SEC-H3 staging owner/runtime boundary; ENABLE ALWAYS on existing triggers | M | F16; P17; s5.5 role separation + `VerifyRuntimeRoleSeparation` for any anchor with settings; ALWAYS applied to deny triggers only, because of DB-H5 (resolution in s5.5); whether Tier S requires the separate staging owner is OD-G14 / OD-G0 |
| SEC-M1 pending gate-off cancel is a continuation | A | s3.1, s3.4, s4.1 step 5; M03 |
| SEC-M2 legacy future-dated gating rows | A + O | s3.2 pre-flight + `legacy_pending_enabling` p1; OD-G13 |
| SEC-M3 unexplained on the enabling row; honesty | A | s3.3 + honesty paragraph; T21 |
| SEC-M4 canonical hashing; gaps in chain | A | s4.0 canonical encoding; s5.6 gaps allowed; A-14, A-16 |
| SEC-M5 payload binding (db oid, attestation id, nonce) | A | s3.2 nonce; s6.2 payload; `gate_authorisation_binding_mismatch` |
| SEC-M6 compensating controls not independent | A | s5.6 independence table; external client-side verifier with a BYPASSRLS read role; D1 correlated with pipeline records; dead-man's switch; T20 |
| SEC-M7 freeze cap, governed lift, membership time-boxing | A | s4.10; kind `screening_gate_freeze_lift` |
| SEC-M8 bind to the screening-policy version | A | s4.0 `screening_policy_id`; guard step 6.10; reconcile p3 |
| SEC-L1 guard takes the pair lock itself | A | s4.1 step 1 |
| SEC-L2 exclude runtime-inducible states from the guard integrity set | A | s3.4 last paragraph |
| SEC-L3 prefix revoke loop + REVOKE FROM PUBLIC | A | s5.2, s7.5 |
| SEC-L4 trigger-aware writer detector | A | s5.2 |
| SEC-L5 guard last BEFORE INSERT trigger | A | `_99_` name + pin, s4.1 |
| SEC-L6, SEC-L7 | N | text not in the relay; covered generally by s5.2 pins; security to confirm |
| SEC citations F2, F3, F12, F16, F21 | A | s1 rows rewritten |
| SEC A-17 step 1 vs I6 / s7.1 chain backfill | A | fresh chain at cut-over (E-27); s7.4 step 1 |
| DB-H1 min_approvals > 2 unreachable | A | s6.1 pinned to 2; column removed; OD-G4 future primitive change; code removed from s9 |
| DB-H2 / DB-H3 FORCE RLS | A | as SEC-H2 |
| DB-H4 AT004 in the governed path becomes 500 | A | F14; s4.1 LC050 on governed paths; parity A-19 |
| DB-H5 ALWAYS fires on a logical subscriber | A | s5.5 split + replication stance; T19 |
| DB-M2 guard takes the policy-pair lock exclusively first | A | s4.1 step 1 |
| DB-M3 revocation by monotonic sequence | M | coverage by seq (s4.0, s4.4); "revoked before execution" keeps one clock comparison (the request has no seq at filing): residual stated |
| DB-M4 definer assert helpers, pins, grants, byte-for-byte 0196 down | A | s5.2, s6.2, s7.2, s7.5 |
| DB-M5 worker attestation bound to the current instance attestation | A | s4.0, guard 6.11, A-11 |
| DB-M6 bigint; timeline semantics | A | s4.0, s4.7 |
| DB-M7 chain backfill vs fresh; explicit column list | A | fresh start (E-27); canonical encoding |
| DB-M8 lock_timeout + retryable 55P03/40P01 | A | s4.1 step 1, s9 `gate_retry_later`, R1 |
| DB-M9 SET LOCAL lock_timeout in 0195-0198 | A | s7.1 |
| DB-M10 revoke_all GUC; FK on revocation jurisdiction | A | platform-level table; FK added |
| DB-L2 one pair helper | A | s3.1, 0197 |
| DB-L3 indexes, retention | A | s4.0 |
| DB-L4 down/fingerprint details | A | s7.2 |
| DB-L5 init-app-role ordering | A | s7.5 |
| DB-L6 ON CONFLICT / COPY notes | A | s4.1 notes |
| LF-H-1 attestor role in CI; no Skip; affected-test list; no bypass | A + O | OD-M1; s15.3; A-20 |
| LF-M-1 | A | as SEC-M1 |
| LF-M-2 chain append does no checks; gate-off under every bad state | A | I2; s4.1 step 3; per-stream chain; A-21 |
| LF-L-1 gate-off is a release decision (OD-G10) | A | s3.4, runbook s7.7, OD-G10 |
| LF-L-2 freeze cap | A | s4.10 |
| LF-L-3 break-glass idempotency | A | unique `incident_reference` (s4.0, s4.10) |
| LF-L-4 lock_timeout | A | s4.9 |
| LF-L-5 persons held on enable | M | recorded (preview + recomputed), shown to approvers, **not** a refusal (no invented threshold) |
| QA-C-1 attestor role in the test cluster, scratch DBs, restarts | A + O | OD-M1; `AuthoriseGate` re-attests |
| QA-H-1 fixture migration | M | s15.3. Four trapdoor sites (not three). With insert guards kept ORIGIN (DB-H5), "must use ENABLE ALWAYS" applies only to deny triggers, handled by `RestoreTriggerMode` + the post-suite check |
| QA-H-2 immediate enabling breaks tests and UI | A | s7.6, A-24 |
| QA-H-3 mutation plan | A | s15.2 (M01-M42) |
| QA-H-4 clock seam; owner-forge test; minimum validity | A | s4.0 helpers; A-5; 3600 s floor |
| QA-H-5 deterministic interleavings | A | s15.4 |
| QA-M (oracle, restore feasibility, changed pins, error precedence, untestable list, tests before implementation, non-implementer mutation, s2 claim) | A | s15.1, s15.5, s2 heading. Item numbering M-1..M-10 was not relayed individually; each named item is covered |
| OPS-H1 monitor failure modes, discriminator, delivery, per-environment severity | A + O | s3.5, s5.6, s11; OD-G11 rewritten (routing is by severity only); delivery in OD-M2 / s19 |
| OPS-H2 D1 noise; declared windows; txid column; ddl_audit_missing p3 | M | s5.6; p3 in Tier S, p1 in Tier N (security disagrees: s19) |
| OPS-H3 cost | M | s11 single-flight, timeout, incremental verify, V12b, head-count honesty. **Rejected:** a cached persons count **in the guard** (an exact count is cheap for a rare operation and a cache could be stale or runtime-influenced); accepted for the monitor |
| OPS-H4 break-glass drill and trapped-funds path | A | s4.8, s4.10 |
| OPS-H5 chain-trigger dependency | A | as LF-M-2 |
| OPS-M1 migration locks, write-quiet window, R1 at 100 %, version probe | A | s7.1, s7.3 |
| OPS-M2 sanctioned guard maintenance | A | s7.2 |
| OPS-M3 attestation burden etc. | A | s4.7; s3.7 unrecorded-anchor row and ordering; rekey re-filing (s4.6) |
| OPS-M4 drill additions; RPO/head-ahead | A | s3.7, V13 |
| OPS-M5 clock monotonicity in the guard | M | forced monotonic `created_at` (E-21) instead of refusal, so a gate-off never fails on the clock (LF-M-2) |
| OPS-M6 manifest ownership, signing, scope, monitor credential | A | s5.6 D4; verifier role |
| OPS-M7 runbook completeness | A | s7.7 |
| OPS-L1..L6 | N | text not in the relay; operations to confirm |
| OPS-I2 anchor correction; production-anchor startup test | A | s3.7; A-25 |
| OPS-SCOPING vs SECURITY | O | s19 (both positions; OD-G0) |

**Rejected in full:** none. **Rejected in part:** OPS-H3 (a cached count in the guard). **Modified:** SEC-H1, SEC-H3, DB-M3,
LF-L-5, QA-H-1, OPS-H2, OPS-M5 (reasons above).

**New or remaining disagreements between reviewers:**
1. Security (H3, ALWAYS everywhere) vs DB architecture (H5, logical replication). The split resolves it; security should confirm that ORIGIN
   insert guards plus detection are acceptable.
2. Operations (scoping, `ddl_audit_missing` p3) vs security (staging is where activation is real). This is owner decision s19.
3. Operations M5 (refuse on clock regression) vs ledger M-2 (gate-off must always succeed). Resolved by forcing monotonic `created_at`;
   both should confirm.
4. QA H-1 ("use ENABLE ALWAYS" in the trapdoors) is superseded by the DB-H5 resolution; QA should confirm.

### 18.2 Review dispositions, round 2 (confirmation reviews of v2.1)

| Finding | Status | Where fixed / reason |
|---|---|---|
| SEC-N1 (High) proof-less asserts and classifier branches take locks | A | s4.9 admission rule (no lock before actor proof or LC040; filing-time validation without locks; execution re-validates under the locks; `classification_changed_refile`); watchdog; I2, I13, P18, T16 reworded; A-21 case (gate-off while a session sits in an assert, via A-27); A-22 scoped to this slice |
| SEC-N1-PLATFORM advisory-lock squatting across 21 platform migrations (e.g. `0181:373`, `:406`) | O (new item) | recorded as a separate platform review item for the orchestrator; out of this slice's scope |
| SEC-N2 clamp pushes `created_at` ahead; validity at `created_at` | A | s3.1: `inserted_at` (wall time) for every validity test; clamp trace; `created_at_ahead_of_clock`; 0198 pre-flight; A-33 |
| SEC-N3 replica mode | A | s5.5 `screening_replica_mode_refuse` (ALWAYS, backend_type allow-list; Q11 open); anchor UPDATE/DELETE deny split and ALWAYS; completeness check in the external verifier; manifest pins `pg_db_role_setting` and an empty `pg_parameter_acl`; Tier N pgaudit `misc_set` + `log_connections`; startup-options gap recorded; A-40 |
| SEC-N4 role separation extended; s5.5 vs s19 contradiction | A | s5.5 API-login attributes, ADMIN OPTION, attestor is not the API login; `VerifyRuntimeRoleSeparation` at startup for staging and production plus every monitor pass; the contradiction removed: the setting `role_separation` (`enforced` / `waived_tier_s`), with the exact Tier S acceptance in s19 |
| SEC-N5 tier binding | A | `tier` on authorisations; currency requires equality; Tier S synthetic check (database part) vs process part stated; S→N needs the verifier countersignature (self-asserted to the database); `screening_gate_tier_n_prerequisites()` reports only what the database can observe and labels the rest self-asserted (s19) |
| SEC-N6 revocation UNIQUE with NULL; dead column; preview binding; monitor single-flight; sequences | A | NULLS NOT DISTINCT (PG15+; RDS must be ≥ 15); `lifted_by_request_id` removed; preview AT002, platform-only, bounded; skipped pass skips the export (`monitor_skipped`); closing REVOKE covers sequences |
| SEC-N7 read-filter pin; atomic swap of `0189:67` and `0190:689` | A | s5.2; s4.9 |
| LF-N-1 0197 cannot prove tighten-only unchanged | A | s7.1 (i)-(iv); helper mutants M60-M62 killed by unchanged suites; A-38 |
| LF-N-2 REPEATABLE READ gate-off after a clock step-back | A | `last_created_at` on the pair lock row under FOR UPDATE (`40001` retryable); A-33 |
| LF-N-3 I13 wording; `gate_retry_later` runbook | A | s2; s4.9; s7.7 |
| LF-N-4 | A | as SEC-N6 |
| LF non-negotiables | A | s19 "Tier-agnostic non-negotiables" |
| QA guard-level wiring (High) | A | A-28 |
| QA break-glass criterion and mutants | A | A-29, M68-M75 |
| QA OD-M1 full role list; E2E attestor | A | s12.1 OD-M1 |
| QA missing mutants; weak mappings (M13, M14 split, M27, M30, M38, M42); equivalent mutant at step 8 | A | s15.2 rows rewritten; s15.6 M43-M81; s4.1 step 8 note |
| QA added tests (tenant isolation, streams + differential hashes + gaps, 0198 behaviour + Go oracle, clamp cases, pointer cases, severity mapping, stale probe, property test, A-1 zero-rows definition, A-2 split, A-15 runnable subset, A-24 raw owner INSERT) | A | s15.1 rows, s15.6 |
| QA logistics (AuthoriseGate retry, choke points and extra files, transitive lock parse, polling bound, schemaSnapshot content, preview pin) | A | s15.6 |
| QA N-12 inconsistencies (E-5/E-8/E-9/E-17; s11 "state 6"; A-18 mapping) | A | s13 note; s11 fixed; A-18 |
| QA untestable additions | A | s15.6 |
| OPS Tier S minimum (metrics, daily reviewer, manual head anchor, one-time D1/D3b/D4 exercise, reaper or acceptance, activation order, OD-G10 after-hours) | A + O | s19 table; s7.7; s11 metrics; OD-G10 |
| OPS not-fixed items (acknowledged-until, `head_export_unconfigured`, capacity remediation, one ordered checklist, concrete drill deliverables) | A | s7.7; V13 |
| OPS-N2 idle timeout is USERSET; doc inconsistency | A | s4.9 (not a bound; watchdog); s16 consistent |
| OPS-N3 / DB-N4 0198 lock | A | s7.1: ACCESS EXCLUSIVE as the first statement, monitors and worker paused, stated read stall |
| OPS-N4 / DB-N3 txid column | A | s7.1: ADD COLUMN NULL, then SET DEFAULT |
| OPS-N9 holder identity needs `pg_read_all_stats` | A (DEFERRED AWS) | s4.9; Q13 |
| OPS-N10 runbook anchors exist; per-instance R1 probe | A | A-44; s7.1 |
| OPS-N11 txid after a logical restore; drill cadence; shared environment anchoring | A | s7.1; s4.10; s7.7 |
| DB-N1 lock-key CHECK rejects the seeds (defect) | A | s4.0 fixed pattern; A-26 |
| DB-N2 non-vacuity source of truth; O(tenants); exact GUC restore | A | s3.3 paragraph; s4.1 step 12 |
| DB-N5 0197 decision tables | A | s7.1 (iv) |
| DB-N6 0197 down conditions; wording | A | s7.2 |
| DB-N7 legacy-pending pre-flight in R1/CI; governed neutralisation | A | s7.1 R1 |
| DB-N8 (a) classifier branches take `gate:global` exclusive **at filing** before the request row lock | **R** | conflicts with SEC-N1 (High): no lock before admission. Filing-time branches now take **no** lock, so the ordering concern disappears; the executors take `gate:global` after the request row lock (s4.9) |
| DB-N8 (b) adopt takes `policy:<T>:<J>` before `gate:global`; fairness; swap timing; revoke branch shape-only; read-back assertion; timeline from WAL file name too; COLLATE "C" + type tag; VOLATILE chain triggers; chain indexes; probe refresh interval | A | s4.9 table and notes; s6.2; s4.1 step 12; Q12; s4.0 |
| DB-L7 any platform_admin can file a revoke | A (accepted risk) | s6.2 |
| DB wording on the publisher's deny triggers | A | s5.5 |

**Rejected:** DB-N8 (a), for the reason above. **Owner items:** SEC-N1-PLATFORM; the watchdog alternative in Tier S; OD-G14 / S-b.

**New disagreements between reviewers (round 2):**
1. **DB-N8 (a) vs SEC-N1:** whether filing-time branches should take `gate:global`. Resolved in favour of SEC-N1 (no lock before admission);
   DB architecture should confirm that execution-time re-validation is sufficient.
2. **Watchdog in Tier S:** security makes it activation-blocking in both tiers; operations accepts a reaper or a written owner acceptance.
   Owner decision (s19).
3. **Role separation in Tier S:** security wants it enforced; operations accepts the S-b waiver. Owner decision OD-G14, now internally
   consistent through the setting.

### 18.3 Review dispositions, round 3 (security and DB-architecture final checks of v2.2)

| Finding | Status | Where fixed |
|---|---|---|
| SEC-N8 (Medium, introduced by v2.2) configure race after removing the classifier lock | A | s4.9 (named re-classification lines, pre-existing and new executors checked); s7.1 and V3b named exception to the fingerprint pin; decision table extended; s15.4 interleaving; A-47; M82 |
| SEC-N9 (Low-Medium) watchdog detection | A | s4.9 (RowShareLock on the relation + `xact_start`, `pg_blocking_pids` cross-check, runtime-role only, never query text, separate custody, `watchdog_absent`, availability risk stated); A-43 |
| SEC suggestion: A-27 with an admitted holder freed by the watchdog | A | A-27 |
| SEC note: PG17 backend_type rename; read `pg_stat_activity` only in replica mode | A | s5.5 replica-refusal row; Q11 |
| SEC note: identify the role administrator from something the owner cannot write | A | s5.5 (`igaming_role_admin` membership) |

| DB-V1 `inserted_at` NULL/NOT NULL inconsistency | A | s3.1, s4.0 (NULL + CHECK NOT VALID, no backfill), 0198 list, versioned canonical column lists |
| DB-V2 lazy `last_created_at` | A | s3.1 |
| DB-V3 replica-refusal trigger order and cost | A | s5.5 (`<table>_00_replica_refuse`, cheap check first, `pg_stat_get_activity`), Q11 |
| DB-V4 watchdog observable; N vs commit latency | A | s4.9 (blocker-of-waiter acts, RowShareLock scan reports; sync-commit caveat; retry safety) |
| DB-V5 0197 pin admits re-validation; exact list; one function per kind | A | s4.9 table of changed bodies; `screening_loosening_configure_classify(p, p_locked)`; asserts never lock; configure decision-table rows |
| DB-V6 0198 pause mechanism; ledger confirmation of the payout claim | A + pending | s7.1 (scale to zero), s7.7; claim marked "to be confirmed by ledger-finance" |
| DB-V7 0197 down too strict | A | s7.2 |
| DB-V8 transitive gate-off wait | A | s4.9, A-21 wording |
| DB-V9 | — | no change requested |
| DB N2 caveat (chain trigger disabled gives read-inconsistent) | A (recorded) | s3.3 |

| DB-V1 final: NOT VALID CHECK breaks tablesync and data-only restore | A | CHECK dropped (reasons in s3.1); guard + reconcile (`unexplained`) |
| DB-C1 lock self-check | A | a transaction-local key marker; no `pg_locks` probe; stated as a consistency guard, not a control |
| DB-C2 classify functions callable from the INVOKER classifier | A | runtime-executable SECURITY DEFINER read-only; exposed-function and read-filter pins |
| DB-V4 final: sampling interval below `lock_timeout` | A | s4.9 (≤ 1 s) |
| DB-V6 final: whole-platform window | A + O | OD-G15 |
| SEC sampling gap; `igaming_role_admin` membership reporting and exclusions; V3b expected diff; text nits | A | s4.9, s5.5, V3b, s4.9 table and steps |

Nothing was rejected in round 3. New problem codes: `watchdog_absent` and `gate_lock_long_holder` (p3).

## 19. Tiered activation proposal (OWNER DECISION OD-G0; not decided here; v2.2)

**Tier S:** synthetic persons only, the MOCK provider, a development or staging anchor.
**Tier N:** any non-synthetic person data, any real provider, or production.

**Tier-agnostic non-negotiables** (ledger; required in both tiers, not up for the tier decision):
- I1 to I3 and I13, with the predicate md5 pin (A-38);
- no gate-off refused for a bad state (A-21);
- screening-only holds park (never `DenyForCompliance`; lic18 behaviour unchanged);
- the payments sweeper monitor runs independently of the worker;
- worker attestation plus heartbeat before new enabling;
- OD-G10 decided before activation and before any gate-off outside development;
- in Tier S, synthetic-only **enforced** as far as the database can (worker and heartbeat `synthetic = true`, MOCK provider; s4.1 step 9b).
  That no real person data is present is a **process control**, checked by the named daily reviewer.

| Control | Operations (Tier S minimum) | Security (Tier S) | Tier N |
|---|---|---|---|
| In-database controls; roles provisioned with **named logins**; **two-person attestation**; runbook with the activation order | required | required | required |
| Break-glass drill on a throwaway cluster with real roles | required | required | required |
| Metrics (s11) and a **named daily reviewer** during activation | required | required | required |
| **Manual chain-head anchor** in the change ticket at every authorisation, attestation and revocation | required | acceptable only alongside D3b | replaced by D3b |
| One-time exercise of D1, D3b and D4 on the evidence environment | required | not enough: continuous | continuous |
| Watchdog | **reaper, or a written owner acceptance** | **required** | required |
| Role separation (OD-G14) | S-b waiver acceptable | S-a enforced | enforced (no waiver possible: CHECK) |
| Alert delivery (ALERT-DELIVERY-1) | dashboard plus the daily reviewer | required for p1 | required |
| D1 / D3b / D4 continuous | not required | required | required |
| D5 pgaudit (`misc_set`, `ddl`, `role`) + `log_connections` | not required | desirable | required |
| Phase 2 worker split | not required | not required | required before production |

**What Tier S accepts under S-b (role separation waived), stated exactly.** The staging owner is the RDS master. It can grant itself attestor
or break-glass membership. It can create logins. It can disable or replace any guard and forge consistent evidence. Only the
manual chain-head anchors in tickets and the daily reviewer would notice. The exposure is limited to synthetic persons and the MOCK
provider. **An S-b database can never move to Tier N**: the S→N settings change refuses unless `role_separation = 'enforced'`.

**S→N transition.** A two-Person settings change (`tier = 'N'`), which:
- invalidates every Tier S authorisation (tier equality, s4.1 step 9);
- requires `s_to_n_verifier_record_sha256`, the digest of a **signed freshness record produced by the external verifier** (head verified,
  completeness verified, manifest compare clean, timestamp). The database cannot verify that signature; the **approvers** verify it out of
  band. To the database it is self-asserted.

`screening_gate_tier_n_prerequisites()` reports only what the database can observe: the event trigger is present and enabled,
`pg_parameter_acl` is empty, role separation is enforced, the watchdog role exists, and the external head export is recent (from the
monitor's own export timestamps, which are themselves runtime-influenced). It labels every external control
`self_asserted:<control>`. It is **partial** and never claims to know the external controls (operations N1).

## 20. Top remaining design risks (v2.2)

1. **Alert delivery is OPEN.** Tier S under the operations position relies on a human daily reviewer.
2. **Owner-forged consistent evidence** stays invisible in the database. In Tier S with S-b, only ticket anchors and the reviewer remain.
3. **R-5:** the API process holds the actor-proof keys.
4. **The watchdog** is a new privileged component (`pg_signal_backend`). It is a target, and it does not exist yet. Without it, an admitted
   holder can stall a governed gate-off for as long as its connection lives.
5. **The platform-wide advisory-lock squatting class** outside this slice (SEC-N1-PLATFORM) remains.
6. **The 0197 rewrite** of reviewed functions: the pins (md5, decision tables, helper mutants on unchanged suites) are the only guard.
7. **0198 takes ACCESS EXCLUSIVE** on a table read on the withdrawal path. The stall is bounded but real; deploy discipline matters.
8. **Feasibility unknowns:** backend_type strings (Q11), timeline reads (Q12), `pg_read_all_stats` on RDS (Q13), event triggers and
   `pg_control_*` on RDS, NULLS NOT DISTINCT on the RDS version.
9. **Test migration scale:** about 60 call sites, 4 trapdoors, plus 21 new criteria. There is a risk of weakened assertions; the guard is A-20's same-text rule.
10. **"Revoked before execution"** keeps one clock comparison.

## 21. What the implementation estimate looks like (relative size; no dates)

Size units: S = 1, M = 2, L = 3, XL = 5 (relative effort, including tests and review rounds).

| Work package | Size | Notes |
|---|---|---|
| WP0 human role provisioning (OD-M1) and the watchdog role | S (human) | blocks merge-ready testing; not agent work |
| WP1 R1 application (codes, OpenAPI, probes, retry mapping, `VerifyRuntimeRoleSeparation`, legacy pre-flight check) | M | |
| WP2 migration 0195 (evidence tables, lock table, chain, encoders, attest and break-glass functions, replica refusal, txid) | L | the largest SQL surface |
| WP3 migration 0196 (five kinds, asserts, executors) + Go kind registration | L | reuses the 0190/0191 recipe |
| WP4 migration 0197 (lock swap + pair helper) with pins and decision tables | M | small diff, high risk; ledger review heavy |
| WP5 migration 0198 (guard, cut-over, inventory, trigger modes, classify, reconcile, monitor summary) | L | the deploy plan included |
| WP6 R2 (monitor loop, metrics, read and reconcile routes, Back Office badge and forms, UI change for immediate enabling) | L | |
| WP7 fixture migration (`AuthoriseGate`, about 60 call sites, trapdoors, pins 16→21 etc.) | L | mechanical but assertion-sensitive |
| WP8 new tests A-1 to A-46 + mutation harness + M01-M81 run by a non-implementer | XL | the dominant cost |
| WP9 external verifier job (client-side hashes, completeness, manifest compare, head store, dead-man's switch) | M | needed for Tier N, or as security's Tier S position |
| WP10 watchdog job | S | |
| WP11 runbook, drills (V13), break-glass drill, evidence templates | M | |
| WP12 independent reviews (security, ledger-finance, DB, QA, operations) over WP2-WP8 | L | |

Total is about 30 units. WP8 alone is about one sixth. WP2, WP3, WP5 and WP7 are on the critical path for merge. WP9 to WP11 are activation-blocking only.
The capacity slice is **not** included (separate).

# F1: actor-proof key out of the backup role's reach (SRBX-3 / R1; owner decision 5) - record

Branch `f1-actor-proof-key-isolation` (from `origin/claude/focused-wright-jw88w9`). Work-list item F1 of `docs/plans/platform-freeze-work-list-2026-10-16.md`.
Nothing here edits an ADR, the decision register, the task registry, HANDOVER, progress or active-stage; the ADR text to adopt is in section 9 (ready to paste).

## 1. Problem (from the SRBX-3 review, `docs/plans/backup-exclude-actor-secret-record.md` section 10)

`igaming_backup` kept table-level `SELECT` on `public.actor_proof_keys`, because `pg_dump` takes `LOCK TABLE ... ACCESS SHARE` on every table it dumps (also on a
table whose data is excluded) and a column-level grant broke the dump. The dump artefact no longer carried the key, but a stolen backup CREDENTIAL (an IAM token,
15 minutes) could still `SELECT secret`. With a stolen runtime credential as well, that defeats the database-enforced four-eyes rule (forged ADR 0110 proofs for any
actor; T1/T8 context).

## 2. Design (option a of the review, the architect-recommended fix)

| Choice | Mechanism | Why |
|---|---|---|
| Where the key lives | Migration `0166_actor_proof_key_isolation` creates the schema `actorproof_private` (owner: the migration owner; `REVOKE ALL ... FROM PUBLIC`) and **moves** the table with `ALTER TABLE public.actor_proof_keys SET SCHEMA actorproof_private` (rows, constraints, index, RLS flag and ACL move with it; nothing is copied). Then every non-owner privilege on the table is revoked (`igaming_backup`'s blanket SELECT, and re-asserted for `igaming_runtime`) | No USAGE on the schema means no non-owner role can even name the table; the init scripts' blanket `ALL TABLES IN SCHEMA public` grants can never re-grant it |
| How it is reached | Only through the two existing `SECURITY DEFINER` functions: `actor_proof_require` (0139's body verbatim except the one key lookup, now `actorproof_private.actor_proof_keys`) and `actor_proof_key_active` (0120's body, same change). `CREATE OR REPLACE` keeps owner, EXECUTE grants (owner + `igaming_runtime`), definer flag and the pinned `search_path = pg_catalog, public, pg_temp` (the private schema is NOT on it; the reference is schema-qualified) | No new reachable surface: no new function, view or grant |
| `public.actor_proof_keys` | Removed (no non-secret view or table left behind) | A view in `public` would be re-granted to `igaming_runtime` by the blanket grants and would make the logical restore depend on the excluded table; nothing needs it: the backup baseline stops listing key ids and `verify.sh` reads the key table's state as the OWNER |
| Logical backup | `backup.sh`: `pg_dump --exclude-schema=actorproof_private` (pg_dump neither locks nor reads it); a PREFLIGHT refuses the backup if migration 0166 is not applied or if the backup role holds USAGE/CREATE on the schema or any table/column privilege in it (read by OID); the stream TOC check now fails the backup on any `actor_proof_key*` data entry in ANY schema AND on any TOC entry of `actorproof_private`, and requires the `actor_proof_require` verifier in the dump | The guarantee rests on the role's privileges, the flag and the verified stream, independently |
| Baseline | `baseline.sql` (run as the backup role) reads only the CATALOG of the private schema: `FP\|actorproof_private_md5` (structure: owner, ACLs by name, relations, columns, defaults, constraints, indexes, triggers, policies, functions, types) and `INV\|actor_proof_key_reachable_by_non_owner` (direct grants to anyone but the owner incl. PUBLIC and column grants, plus effective USAGE/CREATE/SELECT of any other non-superuser role that is not a member of the owner; must be 0). No key row line and no key id any more (a legacy database below 0166 still gets the old non-secret lines) | The backup role sees no key metadata at all |
| Restore | `restore.sh` filters every TOC entry of `actorproof_private` and every `actor_proof_key*` data entry (any schema; warnings for a legacy set and for a set not made by `backup.sh`). After `pg_restore` it decides from the RESTORED catalog: at migration >= 0166 (`version 166` recorded, no public key table) it recreates the schema with an EMPTY key table from the reviewed DDL `deploy/backup/actorproof-private-schema.sql` (as the owner role, `psql -1`); a legacy database (< 0166) keeps its EMPTY `public.actor_proof_keys` (`migrate up` moves it); anything else fails the restore (database dropped) | Fail closed; the DDL equals the migrations (FP line compared by `verify.sh`, and an integration test) |
| Verify | `verify.sh` reads the key table's count and `kid:status` as the OWNER (`ap_owner_state`; never the secret): `unprovisioned` = 0 rows, `provisioned` = exactly the new kid; after physical recovery at least one key row must have survived (with `--actor-proof-kid`, that kid active; a legacy baseline's count is compared too). The FP and INV lines are compared/checked like all others | The backup role can no longer see the table |
| Re-key / seeding | `rekey-actor-proof.sh`, staging-runner `seed-actor-proof` (+ `verify-db.sql`), runbook section 15, `internal/actorproof/prooftest` (locates the table with `to_regclass` first, so a caller's transaction is never aborted; works on scratch databases below 0166 too) | Same procedures, new location |
| Probe packs | `drill.sh` phase 2 `backup_role_cannot_read_actor_proof_key`; staging runner `RN_KEY_PROBES` (3 probes judged like the write probes: only SQLSTATE 42501 passes) | The denial is proven where the credential is used |
| Down migration | Refuses (`APM02`, nothing changed) while the previous state cannot be restored faithfully: an object in the schema that 0166 did not create, a grant on the schema/table/columns to anyone but the owner, a dependent view/rule/trigger/policy, or a `public.actor_proof_keys` that exists again. Otherwise moves the table back, restores 0139's/0120's function bodies verbatim, drops the schema and re-grants `igaming_backup` SELECT on the key table exactly when it holds SELECT on `actor_proof_nonces` (the up refuses (`APM01`) unless the two were alike, so the reconstruction is faithful) | Repo down-migration convention (byte-for-byte snapshot test; refuse rather than lose) |

Up refusals (`APM01`, nothing changed): the schema already exists; the key table is not owned by the migrating role; it carries a grant to anyone but its owner and
`igaming_backup`'s SELECT, any column grant, or any dependent view/rule/trigger/policy (an unexpected path to the secret: investigate first); `igaming_backup`'s SELECT
on the key table differs from its SELECT on `actor_proof_nonces`.

Rejected alternatives: (b) pgaudit column auditing (detects, does not prevent); (c) PostgreSQL 17 `MAINTAIN` (unavailable on 16, unverified, wider privilege);
a non-secret view or a SECURITY DEFINER inventory function for the backup role (first implemented, then removed: new reachable surface for no security gain, and the
view variant breaks restore ordering and is re-granted by the blanket grants).

## 3. Labels

| Deliverable | Label |
|---|---|
| Migration 0166 (up/down, refusals) | IMPLEMENTED (integration-tested on scratch databases; drilled on a backup-granted 0165 database) |
| Backup role cannot read/lock/dump the key (preflight, exclusion, TOC check, init-backup-role re-assertion) | IMPLEMENTED (drilled locally, throwaway clusters) |
| Runtime role cannot read/alter/create in the schema | IMPLEMENTED (integration tests, SQLSTATE 42501 for 20 probes) |
| Restore recreates the empty key schema; legacy and foreign sets never restore key rows | IMPLEMENTED (drilled) |
| verify.sh / baseline.sql / restorecheck | IMPLEMENTED (restorecheck unchanged: it uses only the definer entry points) |
| Re-key script, staging-runner seed/verify/probe pack, runbook section 15 | IMPLEMENTED locally; NOT run against RDS (PROVIDER DEPENDENT) |
| AP001..AP005 semantics unchanged | IMPLEMENTED and tested at the SQL verifier (11 cases) plus the existing K1/K2/K3/launch/RG/T8/staff-lifecycle suites |
| Physical backups / PITR / snapshots carrying the key | unchanged by design (documented; they copy data files) |
| Owner/migration role and superuser can read the key | unchanged residual (ADR 0110 T7) |

## 4. Assertions added and changed

**Changed (never weakened; each is stricter or moved to where the fact now lives):**
- `deploy/backup/actor_proof_exclusion_test.go`: the exclusion pin is now `EXCL=(--exclude-schema=actorproof_private)` (was `--exclude-table-data=public.actor_proof_keys`, which is now forbidden as the mechanism); the "structure must be in the dump" pin (`TABLE public actor_proof_keys`) became "the verifier must be in the dump" plus "no entry of the key schema"; new pins for the preflight messages, the OID privilege check and the fault gate (`... || [ "$BK_TARGET_LOCAL" -ne 1 ]`); restore pins add the schema filter, the DDL step (`psql -1` as the owner), the state decision and the refusal; verify pins add `ap_owner_state`/owner credentials; restorecheck must not mention `actorproof_private`; the secret-reference scan covers `actorproof-private-schema.sql`; the baseline may not read a relation of the key schema and must carry the FP/INV lines; the DDL may not INSERT/COPY/UPDATE/GRANT. `TestBackupRoleKeepsTableSelectResidualIsRecorded` (it pinned residual R1) is replaced by `TestBackupRoleHasNoAccessToTheActorProofKeySchema` (the denial is re-asserted, no GRANT on the schema/tables). `TestRekeyScriptProperties` pins the private table.
- `deploy/backup/backup_static_test.go`: the drill-owned DROP DATABASE allow-list gains the drill's own throwaway copies `legacy_src` and `legacy165_db`.
- `internal/adjustment/actor_proof_integration_test.go` (`TestActorProof_RuntimeRoleCannotReadKeysNoncesOrMint`): the probes name `actorproof_private.actor_proof_keys` (still SQLSTATE 42501), and the privilege check also covers USAGE/CREATE on the schema.
- `internal/db/k2_gates_integration_test.go`: `actor_proof_keys` left the allow-list of owner-only public tables (it is no longer in `public`; narrower).
- `deploy/staging-runner`: probe pack 15 write + 9 abuse + **3 key** probes (27; summary 29 passes); `runner_static_test.go`, `probe-verdict-test.sh` (3 new verdict cases: key probe denied = PASS, missing relation = FAIL, success = FAIL) and `run-local-modes.sh` (counts; a mis-granted backup role FAILS the key probe) follow.
- `drill.sh`: `backup_role_dump_without_exclusion_would_carry_the_secret` (the R1 positive control) is replaced by `backup_role_cannot_read_actor_proof_key`; `backup_role_dump_complete` dumps with `--exclude-schema`; `logical_dump_excludes_actor_proof_secret` now asserts 0 entries of the key schema, the verifier present, NO key row line and NO key id in the backup-role baseline, `INV|actor_proof_key_reachable_by_non_owner|0` and a non-empty `FP|actorproof_private_md5`; positive control: the superuser's dump of the schema; `backup_with_actor_proof_rows_in_stream_is_refused` now runs on a deliberately mis-granted role (otherwise the role cannot dump the schema at all); the old "legacy" control is now a FOREIGN set (superuser dump carrying the whole schema with rows); `restored_database_has_no_actor_proof_secret_and_fails_closed` also requires the DDL step and `equal_FP`.

**Added:**
- `drill.sh`: `backup_role_cannot_read_actor_proof_key` (6 SQL probes, 42501 only; search_path detour; pg_dump of the schema, the table, `--schema-only` and the whole database without the exclusion all fail with `permission denied for schema actorproof_private` and leak nothing; no privilege by OID; no executable function reaching the schema; superuser control), `backup_role_cannot_dump_actor_proof_schema_even_without_exclusion`, `backup_refused_when_backup_role_can_read_actor_proof_key`, `migration_0166_up_down_up_on_a_backup_granted_database` (incl. the APM01 mismatch refusal and the byte-faithful re-grant on down), `true_legacy_set_is_restored_without_rows_and_migrates`.
- `internal/adjustment/key_isolation_integration_test.go` (beside the K2 actor-proof tests: importing `prooftest` switches on the step-up fallback at init, which `internal/actorproof`'s own unit tests must not see): `TestKeyIsolation_Catalog`, `TestKeyIsolation_RuntimeRoleDenied` (20 probes), `TestKeyIsolation_ProofRulesHoldAgainstTheMovedKey` (AP001..AP005, 11 cases), `TestKeyIsolation_StartupGate`, `TestMigration0166_UpDownUp_LegacyRows_Refusals` (5 up refusals, 10 down refusals, each with an unchanged catalog; the baseline INV statement flags all 7 grant plants), `TestRestoreDDL_MatchesTheMigratedKeySchema` (incl. the FP is not blind: six drifts change it, and it tells SELECT/INSERT/PUBLIC grants apart).
- `deploy/staging-runner/sql/verify-db.sql`: `backup_role_has_no_actor_proof_key_access`, `actor_proof_key_isolated`; the runtime check now covers the schema.

## 5. Results

All local, synthetic data only; the shared 5432 cluster was used only through the private scratch database `f120261016220530p8792x01` (rebuilt by
`priv_db.sh`) and the scratch databases the tests create; drills on throwaway clusters under `/tmp/pgdrill-*` (ports 5497-5499 checked free in
`/proc/net/tcp` first; `runuser -u postgres`; no pkill; 5432 never contacted).

| Check | Command | Result |
|---|---|---|
| Full drill (incl. PITR), commit `a894cfc` | `deploy/backup/drill.sh` | **135 PASS, 0 FAIL, `A12 DRILL RESULT: PASS`** (112,732 synthetic ledger entries in 56,334 transactions, 286,555 rows); incl. the five new F1 results and the PITR verifies (now asserting the key rows survived, read as the owner) |
| Small drill, final commit | `deploy/backup/drill.sh --small --skip-pitr` | **125 PASS, 0 FAIL, `A12 DRILL RESULT: PASS`** (commit `e572f2f`; the only changes after the full drill are test code and `prooftest` statement texts) |
| Small drill, earlier commit | same | 125 PASS, 0 FAIL (the first small run FAILED `migration_0166_up_down_up_on_a_backup_granted_database`: `cmd/migrate down` needs `APP_ENV=development`; fixed in the drill, then green) |
| Staging-runner modes on a throwaway cluster | `deploy/staging-runner/tests/run-local-modes.sh` | **207 ok, 0 FAIL**, exit 0 (incl. "3 actor-proof key probes denied", a mis-granted backup role FAILS the key probe, the seed writes `actorproof_private.actor_proof_keys`) |
| New integration tests | `priv_test.sh -tags integration -count=1 -p 1 -run 'KeyIsolation\|Migration0166\|RestoreDDL' ./internal/adjustment/` | 6/6 PASS |
| Integration packages | `priv_test.sh -tags integration -count=1 -p 1 <pkgs>` | ok: `internal/actorproof/...`, `internal/adjustment` (K1/K2 + F1), `internal/withdrawal` (T8, 0162/0165 migrations), `internal/launchgov`, `internal/stafflifecycle`, `internal/aml`, `internal/kyc`, `cmd/...` (5 packages); `internal/payments -run 'T8\|A5\|ActorProof\|K3'` ok (1430 s); `internal/httpserver -run 'StepUp\|StaffRoutes\|PayoutReversal\|A5HTTP\|ActorProof'` ok, `internal/rg` ok (366 s), `internal/db` ok |
| Unit tests | `go test ./...` | 74 packages ok, 8 without tests, 0 FAIL |
| Static (deploy) | `go test -count=1 ./deploy/...` | all ok |
| gofmt / vet | `gofmt -l .`; `go vet ./...`; `go vet -tags integration ./...`; `go vet -tags 'integration drillseed' ./internal/httpserver/` | clean |
| Lint | `golangci-lint run` on the touched packages, plain and `--build-tags integration` | 0 issues |
| shellcheck | `shellcheck -S warning -e SC1091` on every touched script | clean except 5 pre-existing SC2034/SC2155 in `drill.sh` (identical count on the base commit) |

Intermediate failures, fixed (not hidden): (1) the up migration's unexpected-grant check let a grant through when no `igaming_backup` role exists
(NULL in `NOT (grantee = NULL ...)`) - caught by the new up-refusal test, fixed with `COALESCE`; (2) `prooftest` first used a "try the new table,
fall back on 42P01" insert, which aborts a caller's transaction on a pre-0166 scratch database (two withdrawal migration tests failed) - replaced by a
`to_regclass` lookup first; (3) a `fmt.Sprintf` table name in `prooftest` tripped the launchgov status-writer scan - replaced by literal statements;
(4) importing `prooftest` into `internal/actorproof`'s own test binary switched on its step-up fallback for the unit tests there - the F1 tests moved
to `internal/adjustment`; (5) one batch hit a concurrently cleaned Go build cache (`could not import ... no such file`) - re-run green; (6) one
small-drill attempt hit a toolchain directory being rewritten by another process - re-run green.

## 6. Mutation pass on the permission model (kill or justify)

Each mutant was applied to a private copy of the tree, a private scratch database rebuilt from it, and the six key-isolation integration tests run
(`scratchpad/f1/mutate.sh`); the paths that need a real `igaming_backup` role (absent from the shared cluster, and roles must not be changed there)
were run on a throwaway cluster with the drill's own checks (`scratchpad/f1/mutrepro.sh`, port 5497). 24 mutants run: **24 killed** (two of them only after a test was strengthened or a throwaway-cluster check was added, see below); 1 equivalent mutant justified, not run.

| Mutant | Killed by |
|---|---|
| up: keep the backup role's SELECT on the moved table | throwaway cluster (drill check): backup keeps the privilege, and the down then refuses |
| up: GRANT USAGE on the schema to the runtime role / GRANT SELECT to PUBLIC | `TestKeyIsolation_Catalog` (non-owner grants) |
| up: copy into a new table instead of moving (public table left behind) | `TestKeyIsolation_Catalog` (public table must be gone) |
| up: verifier `search_path` includes the private schema / verifier still reads `public` / probe not SECURITY DEFINER | `TestKeyIsolation_Catalog` (pinned definer set, config and return types) |
| up: drop the unexpected-grant check / reintroduce the NULL bug / drop the column-grant check / drop the dependent-object check | `TestMigration0166_...` (APM01 expected) |
| up: drop the backup-SELECT mismatch refusal | throwaway cluster (drill check: mismatch not refused) |
| down: drop the grant check / the column check / the object check | `TestMigration0166_...` (APM02 expected) |
| down: keep the schema / alter the restored probe body | `TestMigration0166_...` (0165 snapshot byte for byte) |
| down: drop the backup SELECT reconstruction | throwaway cluster (ACL differs from the 0165 one) |
| restore DDL: no RLS / a PUBLIC grant / a weaker CHECK | `TestRestoreDDL_MatchesTheMigratedKeySchema` |
| baseline INV: drop the direct-grant term | `TestMigration0166_...` (a write grant must be flagged) |
| baseline INV: drop the effective USAGE/CREATE term | not killed by the Go tests (every Go plant is a direct grant, also counted by the direct term); **killed on the throwaway cluster**: with `igaming_runtime` a member of `pg_write_all_data` (USAGE on every schema, no SELECT, no direct grant) the original counts 1, the mutant 0 |
| baseline FP: drop grantee names / privilege types from the relation ACL | first SURVIVED (the FP still counted grants); the DDL test now also requires different FPs for SELECT vs INSERT vs PUBLIC grants: KILLED |

Justified (not a mutant worth killing): `REVOKE ALL ON SCHEMA actorproof_private FROM PUBLIC` in the up and the DDL - `CREATE SCHEMA` grants nothing to
PUBLIC, so removing it is an equivalent mutant (kept as an explicit belt). Static-only guards (exclusion flag, TOC regexes, preflight, probe pack, init
scripts) are pinned by `deploy/backup/*_test.go` and `deploy/staging-runner/runner_static_test.go` and exercised end to end by the drill's negative
controls (`backup_role_cannot_dump_actor_proof_schema_even_without_exclusion`, `backup_refused_when_backup_role_can_read_actor_proof_key`,
`backup_with_actor_proof_rows_in_stream_is_refused`, the FOREIGN and LEGACY set restores); they were not mutated one by one.

## 7. Residuals

- **Physical artefacts** (base backups, WAL archive, snapshots, PITR copies) contain the key table's data files and so the key; their protection is the AWS boundary (KMS, IAM). Unchanged, documented (`backup-restore-dr.md` sections 1, 4.2, 5a).
- **Owner/migration role and superusers** (and members of the owner role, e.g. the restore loader role while it exists) can read the key (ADR 0110 section 9.4, T7). The baseline invariant deliberately does not count members of the owner.
- **Before migration 0166** the backup credential could read the key (R1). A backup credential possibly exposed while a database was below 0166 still means re-keying (`backup-restore-dr.md` 5a step 1, corrected).
- **Legacy sets** carry key rows (unchanged: filtered on restore, warned; retire them). A legacy baseline compared by the new `verify.sh` after a restore of a legacy set differs in the new FP/INV lines (tool-version mismatch; legacy sets are to be retired anyway).
- **Mis-grant detection is at backup time** (preflight, INV) and in the probe packs, not continuous; a grant made and revoked between backups is not seen (pgaudit/log review on RDS: not proven).
- **`restore.sh` legacy branch** trusts `schema_migrations` of the restored archive to choose the layout; a hostile signed archive could claim < 0166 to avoid the DDL step, but then the database has no private schema, every proof fails closed and `verify.sh`'s FP comparison fails against a >= 0166 baseline (fail closed, not a leak).
- **Not run:** RDS (rds_superuser owner, IAM token, parameter groups), Terraform, the full HTTP K2/K3 flow on a re-keyed restored database (SQL-level verifier + startup gate only, as before).

## 8. Probe lists

**Security review:**
1. Any non-owner path to `actorproof_private.actor_proof_keys`: grants, default privileges (global and schema-scoped), role memberships (`pg_read_all_data`, rds roles), `SECURITY DEFINER` functions other than the two (catalog test pins exactly two), views/rules/triggers/policies on the table (up/down refuse; catalog test), extensions or event triggers.
2. The definer functions: `search_path` pinned without the private schema; the schema-qualified reference; `CREATE OR REPLACE` kept ACLs; no output path for the secret (void/boolean return; errors carry no key material).
3. `backup.sh`: the preflight cannot be skipped off a throwaway target (fault gate is `fault AND local`); `--exclude-schema` cannot be emptied except by the gated fault; the TOC regexes (`TABLE DATA <schema> actor_proof_key*`, any `actorproof_private` token) on the stream; a schema RENAME (the preflight looks the schema up by name: a renamed private schema would be dumped by a role that has no USAGE -> pg_dump fails closed; a role WITH usage on a renamed schema is not caught by the name-based TOC check but the data-entry regex still is).
4. `restore.sh`: filter completeness (ACL/COMMENT/DEFAULT ACL entries of the schema carry the token); the state decision is made from the restored catalog; DDL as the owner in one transaction; failure drops the database.
5. `rekey-actor-proof.sh`, `seed-actor-proof.sql`: unchanged secret handling, new location only.
6. Down migration: can it be used to re-expose the key? (dev/staging only by the migrate CLI policy; it re-grants SELECT to the backup role only when the nonce grant shows the blanket regime.)
7. The baseline `INV` excludes superusers, `pg_*` roles and members of the owner: confirm that is the intended trust boundary.

**Ledger-finance review:**
1. Proof semantics unchanged: AP001..AP005 order and conditions (0139 body verbatim except the key lookup; diff in section 2); nonce consumption unchanged (`public.actor_proof_nonces`).
2. Every proof-bound governed flow still verifies (K1, K2, K3, launch, RG floors, staff lifecycle, T8 payout reversal) - suites in section 5.
3. The migration moves rows, never copies; no financial table, trigger or projection is touched; `SUM(debits) = SUM(credits)` invariants untouched (baseline INV lines unchanged).
4. After a logical restore the platform fails closed on every governed write until re-keyed (unchanged behaviour, new mechanics).

## 9. READY-TO-PASTE amendments (the ADRs are NOT edited here)

**ADR 0110, new amendment section:**
```
## Amendment 2026-10-16 - the verification key lives in an owner-only schema (F1, migration 0166; SRBX-3 / R1; owner decision 5)

The verification copy of the actor-proof keys moved from `public.actor_proof_keys` to `actorproof_private.actor_proof_keys`. The schema `actorproof_private` is owned
by the migration owner and grants nothing to anyone else (no USAGE, no CREATE, no table or column privilege for PUBLIC, `igaming_runtime` or `igaming_backup`); the
table keeps its columns, constraints, RLS flag (enabled, not forced, no policy) and owner-only ACL. It is reachable only through the two existing SECURITY DEFINER
functions, `actor_proof_require` and `actor_proof_key_active`, whose bodies changed only in the schema-qualified table reference; their `search_path` stays
`pg_catalog, public, pg_temp` and their EXECUTE grants are unchanged. No other function, view or grant reaches the schema. Proof format and the AP001..AP005 rules
are unchanged. Section 6 ("Where they must NOT live") now also excludes the backup role: it cannot read the key, so a stolen backup credential no longer yields it
(T7 unchanged: the owner/migration role and a superuser still can). Evidence: docs/plans/f1-key-isolation-record.md.
```

**ADR 0122, amendment to section 10 (replaces item 5 of the 2026-10-16 amendment):**
```
5. (F1, migration 0166) The actor-proof key table lives in the owner-only schema `actorproof_private`. `backup.sh` dumps with `--exclude-schema=actorproof_private`
   (pg_dump neither locks nor reads it), refuses to run if migration 0166 is not applied or if the backup role holds any privilege on that schema or its relations,
   and fails the backup if the stream carries any entry of that schema or any actor_proof_key* data entry in any schema. The backup role holds no privilege on the
   key at all (residual R1 is closed for databases at migration >= 0166; init-backup-role.sql re-asserts the denial). The baseline carries only the key schema's
   structure fingerprint and a privilege invariant (no key ids); `verify.sh` reads the key table's state as the owner. `restore.sh` recreates the schema with an EMPTY
   key table from the reviewed DDL `deploy/backup/actorproof-private-schema.sql` (compared with the source's structure by verify.sh). Physical backups, snapshots
   and PITR still contain the key (inside the AWS boundary). A backup credential that may have been exposed while a database was below 0166 still requires a re-key.
```

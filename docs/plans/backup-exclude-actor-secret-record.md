# Owner decision 5: logical dumps exclude `actor_proof_keys.secret`; restore re-keys (record)

Branch `backup-exclude-actor-secret` (from `origin/claude/focused-wright-jw88w9`). Specialist: devops. ADR 0122 section 10 was the open question; the owner has decided option 2.
Nothing here edits an ADR, the decision register, the task registry, HANDOVER, progress or active-stage; the ADR text to adopt is in section 7 (ready to paste).

## 1. Decision implemented

- A logical dump never contains the rows of `actor_proof_keys` (the HMAC secrets of the signed actor proof, ADR 0110). The table STRUCTURE is dumped; a restored database starts with an
  EMPTY table, so no proof can verify (`AP002`) and a production `platform-api` refuses to start until a NEW key is provisioned through the reviewed secure procedure
  (`operational-runbooks.md` section 15 step 4, now scripted). The source's key id is retired by absence and is never reused. No key is ever copied through a backup.
- Physical base backups, snapshots and PITR are unchanged and DO contain the live key; they stay inside the AWS trust boundary. This is documented in `backup-restore-dr.md` (sections 3, 4.2, 5a, 5b)
  and the staging plan; `verify.sh --anchor-mode preserved` now asserts the key rows survived physical recovery.

## 2. Design (the mechanism chosen, and why)

| Choice | Mechanism | Why this, not the alternative |
|---|---|---|
| Exclusion | `pg_dump --exclude-table-data=public.actor_proof_keys` | Keeps the table definition (and its ACL, RLS flag, the `actor_proof_require` functions that reference it) so the restore is structurally identical and `migrate verify` / the catalog fingerprints still pass. A column-level exclusion does not exist in pg_dump; `--exclude-table` would drop the table and force the restore to recreate it. |
| Verified on the real bytes | `backup.sh` tees the dump stream into `pg_restore --list` while it is encrypted and FAILS the whole backup (run directory removed, nothing kept) if a `TABLE DATA public actor_proof_keys` entry exists or the `TABLE public actor_proof_keys` definition is missing; the manifest records the derived fact `"actor_proof_keys_data": "excluded"` (signed, like the `encryption` field) | The guarantee does not rest on a flag staying in the command line. |
| Restore | `restore.sh` also filters `TABLE DATA public actor_proof_keys` out of the TOC it feeds `pg_restore -L` (same mechanism as the environment anchor, M4) and warns about a LEGACY set | A validly signed set made before the decision still restores WITHOUT the key; the warning tells the operator to retire that set. |
| Baseline / verify | `baseline.sql` hashes the key table over `kid, status, created_at, retired_at` only (never the secret, so no md5 of it enters `baseline.txt`) and adds `META|actor_proof_kids|kid:status,...`; `verify.sh` drops the table from the logical comparison and asserts instead: `--expect-actor-proof unprovisioned` (default after a logical restore) or `provisioned --actor-proof-kid K --retired-kid O...`; after physical recovery (`--anchor-mode preserved`) the table is compared and `actor_proof_keys_retained_by_physical_recovery` is asserted | A restored table that is empty MUST differ from the baseline; the assertion replaces the comparison instead of weakening it. |
| Fail-closed proof | `restorecheck -actor-proof unprovisioned|provisioned|skip`: runs the platform's own `actorproof.VerifyConfiguredInProduction` (the production startup gate) with a throwaway issuer, and presents a well-formed unsigned proof to `actor_proof_require` as `igaming_runtime` expecting `AP002` | Uses the real startup gate and the real SQL verifier; the binary holds, reads and signs with no real key (static test pins it). |
| Re-key | `deploy/backup/rekey-actor-proof.sh` (owner role; `REKEY_*` target through the same target guard; psql >= 16 `\bind`; the secret is read by psql from a 0600 file, bound as a parameter, never in argv/statement text/stdout) | It is section 15 step 4 with its preconditions made mandatory instead of advisory. It does not generate, store or print a secret and never reads one from a database. |

## 3. Labels

| Deliverable | Label |
|---|---|
| Exclusion in `backup.sh` + TOC assertion + manifest field | IMPLEMENTED (drilled locally, throwaway clusters, synthetic data) |
| `restore.sh` never restoring the rows (incl. legacy sets) | IMPLEMENTED (drilled with a re-sealed legacy set) |
| `baseline.sql` / `verify.sh` / `restorecheck` adjustments | IMPLEMENTED |
| `rekey-actor-proof.sh` | IMPLEMENTED locally; NOT run against RDS (PROVIDER DEPENDENT: parameter-group logging, pgaudit, `rds_superuser` owner, tmpfs on the runner) |
| "After re-keying the platform's proof-bound operations work" | PARTIALLY IMPLEMENTED: asserted at SQL level as `igaming_runtime` (the application's role) with a perfectly formed proof signed with a throwaway key (accepted; replay refused `AP005`; old keys and relabelled keys refused `AP002`) plus the platform's production startup gate (admits the new kid, refuses the old ones). The full HTTP K2/K3 flow was not run against the restored database (the seeder builds its own world and key; running a governed flow on the restored copy needs a second Go integration harness, judged too heavy) |
| Backup role unable to read the secret column | NOT IMPLEMENTED / BLOCKED by pg_dump: see residual R1 (tried in the drill: a column-level grant makes `pg_dump` fail with `permission denied for table actor_proof_keys` because it takes `LOCK TABLE ... ACCESS SHARE` on every dumped table) |
| Physical backup / PITR carrying the key | documented, unchanged (by design) |
| ADR 0122 section 10 text | provided below (section 7); the ADR itself is NOT edited |

## 4. Assertions: added and changed

New `RESULT` lines in `deploy/backup/drill.sh` (all must PASS):
`source_holds_actor_proof_secrets_to_hunt` (a planted canary key `CANARY-ACTOR-PROOF-<random>` next to the seed's real platform key; 5 search patterns: ASCII/hex/base64 of the canary, hex/base64 of the platform key),
`backup_role_dump_without_exclusion_would_carry_the_secret` (positive control, residual R1),
`logical_dump_excludes_actor_proof_secret` (TOC has the definition and no data entry; manifest field; 0 occurrences in the dump as SQL text and raw bytes, in globals/baseline/manifest/SHA256SUMS, in the backup run output and in the source server log; a superuser dump of the same table has the leaks, so the hunt is not blind),
`encrypted_dump_excludes_actor_proof_secret` (the age set, decrypted),
`backup_with_actor_proof_rows_in_stream_is_refused` (negative control: drill-only fault `BACKUP_TEST_FAULT=include_actor_proof_keys` removes the exclusion; the stream TOC check refuses and keeps nothing),
`restored_database_has_no_actor_proof_secret_and_fails_closed` (0 rows; a pg_dump of the WHOLE restored database finds 0 occurrences; a perfect proof signed with the canary key and with the platform key is `AP002`; control on the source: the same proof is accepted and its replay is `AP005`; the `verify.sh` unprovisioned gates PASS),
`restore_output_names_the_rekey_step_and_leaks_nothing`,
`legacy_set_with_actor_proof_rows_is_restored_without_them` (a validly signed set whose archive carries the rows restores with a warning and without them),
`rekey_script_refuses_unsafe_use` (reused kid, 0644 secret file, too-short secret, non-owner role, `log_statement=all`; table stays empty),
`rekey_script_provisions_new_key` (incl. 0 occurrences of the new secret in the server log and script output; a second run is refused),
`proof_verification_works_only_with_the_new_key_after_rekey`,
`rekeyed_restore_verified` (`verify.sh --expect-actor-proof provisioned`),
`backup_of_rekeyed_database_excludes_the_new_secret`.

Changed: `backup_role_dump_complete` dumps with the exclusion (as production does); `logical_restore_verified` and `full_loss_restore_verified` now include the unprovisioned gates (`actor_proof_keys_empty_after_logical_restore`, `actor_proof_startup_gate_refuses_<kid>`, `actor_proof_verifier_fails_closed`); `equal_TABLE` no longer compares `actor_proof_keys` after a logical restore (asserted instead); PITR verification (`--anchor-mode preserved`) adds `actor_proof_keys_retained_by_physical_recovery`.

Static tests (`go test ./deploy/backup/`): `actor_proof_exclusion_test.go` (exclusion unconditional except the gated fault; TOC assertion; restore filter; verify/restorecheck assertions; restorecheck neither signs nor reads keys; no tool reads `actor_proof_keys.secret`; baseline row text is the non-secret columns; the residual is recorded beside the role grants; re-key script properties and forbidden constructs) and the secret scan extended with four actor-proof patterns (a `kid:base64` key pair in `ACTOR_PROOF_KEYS`, `decode('<64+ hex>','hex')`, a bytea hex literal into the key table, a hex secret assignment), each with a planted-secret case and false-positive cases for the runbook's placeholders.

## 5. Results

- **Full drill** `deploy/backup/drill.sh` (2026-10-16, throwaway clusters under `/tmp/pgdrill-*` on ports 5499/5498/5497 as `runuser -u postgres`, no pkill, the shared 5432 cluster never contacted; 113,014 synthetic ledger entries in 56,475 transactions, 287,259 rows): **131 PASS, 0 FAIL**, `A12 DRILL RESULT: PASS`, including the PITR phase (the recovered copies keep the key rows: `actor_proof_keys_retained_by_physical_recovery` inside `VERIFY RESULT: PASS`).
- Smoke drill `drill.sh --small --skip-pitr`: 121 PASS, 0 FAIL. (The first smoke run FAILED on `backup_role_dump_complete` and the backup itself: the column-level grant experiment, residual R1; reverted, re-run green.)
- `go test ./deploy/...`: all packages ok (static guards incl. the new `actor_proof_exclusion_test.go`); `go vet ./deploy/...` clean; `gofmt -l deploy` empty; `shellcheck -S warning -e SC1091,SC2034,SC2155` clean on the scripts this branch touched (`backup.sh`, `restore.sh`, `verify.sh`, `rekey-actor-proof.sh`, `drill.sh`). `lib-cluster.sh`, `lib-provision.sh` and others carry pre-existing SC2034/SC2155 warnings that were left alone (SRBX-9) (shellcheck was already installed; no sudo used).
- Environment note: during the full run another agent's cluster occupied port 5497 (a different tool's `/tmp/pgdrill-*` root). It was left alone; it was gone before the PITR phase started. `go clean -cache` was run once for disk space (3.4 GB), as allowed.
- NOT run: RDS, Terraform, the HTTP K2/K3 flow on a re-keyed restored database.

## 6. Residuals and limits

- **R1 (security review)**: `igaming_backup` keeps table-level `SELECT` on `actor_proof_keys`, so a stolen backup CREDENTIAL can still read the signing secret (and everything else: the role is BYPASSRLS by design). Excluding the secret from the dump protects the ARTEFACT, not the credential. Mitigations unchanged: time-boxed or IAM-only credential, three-party minting rules, production dumps from a clone (ADR 0122 section 5c). A real fix would move the secret into a table the backup role has no privilege on and exclude that whole table (a migration plus changes to the four SECURITY DEFINER verifiers: an architect decision, not done here).
- **R2**: legacy sets (made before this change) carry the key. `restore.sh` skips and warns; they must be retired and, if exposed, the key family rotated (`backup-restore-dr.md` 5a).
- **R3**: physical artefacts (base backups, WAL archive, snapshots, PITR copies) carry the key; their protection is the AWS boundary (KMS, IAM), which is NOT PROVEN here (no RDS run).
- **R4**: `rekey-actor-proof.sh` checks `log_statement`, `log_min_duration_statement` and `log_parameter_max_length_on_error`; (SRBX-4 fix) it now also refuses `log_transaction_sample_rate` > 0, sampled statement logging, `auto_explain` parameter logging and `pgaudit` with `pgaudit.log_parameter` on, reading them with `current_setting(name, true)` (so pgaudit IS visible when its library is loaded; NULL otherwise). The earlier claim that it cannot see pgaudit was wrong. Still unproven on RDS.
- **R5**: the new kid may legally equal an old one only if the operator omits `--retired-kid` (the script cannot know the source's ids; `baseline.txt` records them as `META|actor_proof_kids|`). Reusing a kid with a NEW secret is still cryptographically safe (old proofs fail the MAC) but muddies the audit trail; `verify.sh --retired-kid` asserts absence.
- **R6**: until re-keyed the platform is intentionally unable to perform governed writes (K1/K2/K3, launch, RG floors, staff lifecycle) and a production API does not start. That adds a step to RTO (secret-store entry, one script run, one deploy); not measured.
- **R7**: proofs minted before the restore are dead by design (60 s lifetime anyway); pending approvals in the restored data need fresh proofs from their approvers.
- **R8**: the TOC check matches `TABLE DATA public actor_proof_keys`. A future rename/move of the table or a second key table would need the pattern (and the static test) updated; a drill failure (the canary hunt) would show it.
- The drill uses throwaway clusters and synthetic data only; the SQL-level proof check is not the HTTP flow (section 3).

## 7. READY-TO-PASTE amendment for ADR 0122 section 10 (the ADR is not edited here)

```
## 10. Decision: logical dumps exclude `actor_proof_keys.secret`; restore re-keys (owner decision 5, 2026-10-16; implemented and drilled locally)

Status: DECIDED. This section was an open question for the architect (option 1 keep, option 2 exclude); the owner chose option 2.

1. A logical dump never contains the rows of `public.actor_proof_keys`. `backup.sh` runs `pg_dump --exclude-table-data=public.actor_proof_keys` (structure kept) and verifies the
   exclusion on the encrypted stream itself by reading its table of contents; a data entry for the table fails the whole backup and keeps nothing. The signed manifest records
   `"actor_proof_keys_data": "excluded"`. The baseline hashes the table's non-secret columns only and `verify.sh` asserts the restored table instead of comparing it.
2. A restored database has no actor-proof key. `restore.sh` never restores the table's rows (a legacy set that carries them is restored without them, with a warning). Until a new key
   is provisioned every governed write fails closed (`AP002`) and a production `platform-api` refuses to start (`actorproof.VerifyConfiguredInProduction`).
3. Re-key is a documented, scripted step: `deploy/backup/rekey-actor-proof.sh` is `operational-runbooks.md` section 15 step 4 with mandatory preconditions (owner role, secret from a
   0600 tmpfs file as a bound parameter, statement logging checked, no existing key, a kid different from the source's). The key and kid come from the secret store; nothing is
   copied through a backup. The source's kid is retired by absence.
4. Physical base backups, snapshots and PITR contain the live key. They stay inside the AWS trust boundary and are what recovers the live key after a total loss; they are never
   copied out of it.
5. Residual: the backup role retains table-level SELECT on `actor_proof_keys` (pg_dump takes LOCK TABLE on every dumped table), so a stolen backup credential can read the key; the
   dump artefact cannot. Moving the secret to a table the backup role cannot read is a possible later change (a migration; architect).
6. Not proven: the re-key on RDS (pgaudit, parameter group, owner login), the HTTP K2/K3 flow on a re-keyed restored database (the drill proves the SQL verifier path and the startup gate).
Evidence: docs/plans/backup-exclude-actor-secret-record.md.
```

## 8. Probe list for the security review

1. Hunt for any other path that carries the secret into an artefact: a view/trigger/function default/COMMENT/`pg_dump` large object, `audit_log` rows from key provisioning, the sequences, `globals.sql`, the baseline `META|actor_proof_kids|` line, the manifest, `backup.sh` stdout/stderr, the server log of the source (drill: 0 hits; the control proves the hunt sees it).
2. Bypass the stream TOC assertion: a table renamed/moved, a second schema's `actor_proof_keys`, `pg_dump` emitting the data under a different TOC description, `pg_restore --list` failing silently (handled: `toc.rc` must be 0), the process-substitution ordering race (waits up to 20 s for `toc.rc`).
3. `BACKUP_TEST_FAULT=include_actor_proof_keys` on a non-local target (must be refused by the existing gate) and via environment injection in the runner.
4. `restore.sh` with a signed legacy set (filtered; the secret still sits decrypted in the private 0700 work dir for the duration, as every dump content does); with a set whose manifest lacks the field (accepted with the filter, by design).
5. `rekey-actor-proof.sh`: secret exposure through argv, environment (only the FILE path is exported), psql history (non-interactive, `-X`), `ps` during the `cat` backtick, core dumps, `/proc`, shell xtrace, file TOCTOU between the mode/owner check and psql's read, symlink swap (refused), a non-tmpfs path on a remote target, a partially written hex file, a hex file with a trailing newline (stripped), server-side capture by `pgaudit` / `auto_explain` / `log_error_verbosity` / `log_min_error_statement` with parameters (R4), a second concurrent run (the unique `kid` and the "no existing key" precondition race: two runs with different kids could both pass the count check; the table owner is the only writer, accepted).
6. Can an operator re-key with a key that is also the source's? (omitting `--retired-kid`: R5.) Can the new secret equal the old? (the script cannot know it.)
7. `restorecheck`: it presents a well-formed unsigned token inside a rolled-back transaction as `igaming_runtime`; confirm it cannot consume a nonce (AP002 is raised before the nonce insert) and never holds a real key.
8. `verify.sh --expect-actor-proof skip` after a logical restore (an operator-supplied `skip` hides the check): is that acceptable? (default is `unprovisioned`; `skip` is for physical recovery.)
9. The backup role residual R1 and whether the column-level approach plus a different dump strategy (e.g. `--exclude-table` plus recreating the empty table at restore) is worth the migration/verifier change.
10. Interaction with ADR 0110 rotation: the "no existing key" refusal means rotating a live key still goes through section 15 by hand; confirm nobody uses the re-key script for rotation.

## 9. Files changed

`deploy/backup/{backup.sh,restore.sh,verify.sh,baseline.sql,drill.sh,README.md,rekey-actor-proof.sh,restorecheck/main.go,backup_static_test.go,backup_fixround_test.go,actor_proof_exclusion_test.go,init-backup-role.sql (comment only)}`,
`docs/runbooks/{backup-restore-dr.md,aws-staging-deploy-plan.md,operational-runbooks.md,restore-from-backup.md,production-configuration-checklist.md}`, this record.

## 10. Independent security review (security specialist, 2026-10-16)

Scope: the diff `12edec0..52f8c4f` (five commits), read from the code, not from this record: `backup.sh`, `restore.sh`, `verify.sh`, `baseline.sql`,
`init-backup-role.sql`, `rekey-actor-proof.sh`, `restorecheck/main.go`, the drill canary/hunt assertions, the secret-scan additions, the runbook changes, and
migrations 0120/0124/0130/0135/0139 (where the secret lives and how it is read). Run: `go test -count=1 ./deploy/backup/` ok, `go test ./deploy/...` ok,
`shellcheck -S warning` on the four changed scripts and the new one clean. NOT run: the drill (not needed: the hunt and its positive controls were read in code),
RDS, pgaudit, the HTTP K2/K3 flow. This review does not make the feature "secure"; it covers the listed files at this commit only.

**Verdict: NOT APPROVED as is.** The mechanism (exclusion, stream check, restore filter, baseline, re-key script) is sound. Merge is blocked by SRBX-1 and SRBX-2.
Both are document fixes. Once they are fixed and re-checked, this can be approved for staging. SRBX-3 (R1) blocks production unless the owner/architect decide otherwise.

### Verified (no finding)

- **Exclusion and stream check.** `--exclude-table-data=public.actor_proof_keys` is unconditional. The only way to drop it is `BACKUP_TEST_FAULT`, which is refused unless `BK_TARGET_LOCAL=1`
  (a loopback drill port; 5432 is refused; a remote host is never local). `PGOPTIONS`/`PGSERVICE` are unset by `lib-common.sh`. The pattern has no wildcard (`_` is literal in pg_dump patterns).
  The table is not partitioned and has no children. The secret exists only in `public.actor_proof_keys`: no audit trigger copies it, and the four `SECURITY DEFINER` verifiers read it
  into a local variable only. Views and matviews carry no data in a dump (a matview is REFRESHed at restore against the empty table).
  The check reads the TOC of the custom-format stream, which is written before any data block. A data block can only exist for a TOC data entry, so absence of the entry means
  absence of the rows. It fails closed: a missing or non-zero `toc.rc`, a missing definition, or a data entry each cause `bk_die`, and `cleanup` then removes `RUN_DIR`
  (including the already-moved encrypted dump) and `TMPW`. `trap '' PIPE` is inherited by `tee`, so `pg_restore --list` exiting early cannot kill the pipeline
  (GNU tee drops an EPIPE output in its default mode). `actor_proof_keys_data` in the manifest is only set after the check passes and is covered by the signature.
- **Restore.** The TOC filter runs on `toc.txt` of the decrypted private copy. The private-copy flow (SR12R-1 / SR12R2-1) is unchanged, so there is no regression. A genuine legacy set restores without the rows and with a warning.
- **Baseline/verify/restorecheck.** The baseline hashes only `kid, status, created_at, retired_at`, and the sequence probe does not touch this table, so no hash or digest of the secret is written.
  `META|actor_proof_kids|` holds key ids only. `restorecheck` uses a random in-memory key for the startup gate (the gate checks only that the kid is active) and a random MAC.
  It runs inside a rolled-back transaction, and `AP002` is raised before the nonce insert. It never holds or reads a real key.
- **Re-key script, main path.** The secret is in a psql variable read by psql's own backtick. Only the file PATH is in the environment and argv. It is bound with `\bind` (psql >= 16), and
  `-X` skips psqlrc. `KID` is regex-validated before it is interpolated into the precondition SQL. The owner check is `relowner = current_user`, which refuses a superuser that is not the owner. A second run is refused.
  The "Failing row contains" error DETAIL, which would print the secret, cannot be reached: the length, hex format and kid are pre-validated, and a unique violation prints only the kid.
- **Drill hunt.** It is not blind. The canary is searched in ASCII, hex and base64, and the seeded key in hex and base64. The dump is searched both decompressed (`pg_restore -f -`) and raw. A superuser dump and a backup-role dump without the exclusion are the positive controls.
  The canary-signed proof is accepted on the source (and its replay gets `AP005`) and gets `AP002` on the restore.
- **Secret-scan patterns.** Each new pattern has a planted positive case and the runbook placeholders as negatives. Sound.

### Findings

| ID | Severity | Finding | Required action |
|---|---|---|---|
| SRBX-1 | **HIGH (merge-blocking; governance and audit integrity)** | Commit `0aad201` DELETES the `2026-10-16 owner decisions` row from `docs/governance/human-decision-register.md` and the `OWNER-DECISIONS-2026-10-16-PROVIDER-PHASE` row from `docs/governance/task-registry.md`. Both rows were added at the merge-base `12edec0`. `git diff 12edec0..HEAD -- docs/governance/` shows the two deletions. Merging would erase the owner's written authorisation, including decision 5, which this branch implements, and the staging authorisation. That contradicts this record's own statement that it "edits nothing in the decision register / task registry". | Restore both files to their `12edec0` content (`git checkout 12edec0 -- docs/governance/human-decision-register.md docs/governance/task-registry.md`) and confirm with `git diff 12edec0..HEAD -- docs/governance/` that the diff is empty. |
| SRBX-2 | **MEDIUM (merge-blocking; false security claim leading to an incident-response gap)** | `docs/runbooks/backup-restore-dr.md` line 32 (role table: "not on `actor_proof_keys.secret`: only the non-secret columns") and lines 94-96 ("The backup credential itself cannot read the secret column (`init-backup-role.sql` grants only `kid, status, created_at, retired_at` ...; `backup.sh` warns when the role can still read the column)") are false. `init-backup-role.sql` keeps table-level SELECT (R1), and `backup.sh` has no such warning (it does not exist). This text is left over from the abandoned column-grant experiment. Section 5a step 1 then tells the operator to rotate the actor-proof key only for a legacy set or a physical copy ("a current logical set cannot contain them"). But 5a also covers a leaked **backup credential**, and that credential can read the key. Failure scenario: the backup credential is used outside the runner; the operator follows 5a, does not rotate the actor-proof key, and the attacker keeps a working key. | Correct lines 32 and 94-96 to say the role keeps table-level SELECT and the residual is R1. Rewrite 5a step 1 so that it says: rotate the actor-proof key family after any suspected exposure of a legacy set, any physical copy, **or the backup credential**. |
| SRBX-3 | **MEDIUM: acceptable for staging; production-blocking unless the owner or architect decide** | Residual R1, assessed below. | See the R1 assessment below. |
| SRBX-4 | LOW (fix before the RDS re-key; non-blocking for merge) | `rekey-actor-proof.sh` checks `log_statement`, `log_min_duration_statement` and `log_parameter_max_length_on_error`. It does not check other server settings that also log bound parameters: `log_min_duration_sample` with `log_statement_sample_rate`, `log_transaction_sample_rate` (logs every statement of a sampled transaction, parameters included), `auto_explain.log_min_duration` with `auto_explain.log_parameter_max_length` (default -1, meaning no limit), and `pgaudit.log` / `pgaudit.log_parameter`. On RDS the server log is exported to CloudWatch, which a wider group can read than the database. Also, the claim that "the script cannot see pgaudit" (staging plan 8.3, R4) is wrong: `current_setting('pgaudit.log_parameter', true)` (and the auto_explain settings) can be read from the session when the library is loaded, and return NULL otherwise. | Add these settings to the precondition query using `current_setting(name, true)` and refuse unless each is off or NULL. Correct R4 and plan step 8.3. |
| SRBX-5 | LOW | Re-key script robustness. (a) TOCTOU: the secret file is validated (mode, owner, symlink) and then read three times by path (`tr` twice, `cat` once). If its directory is writable by someone else and not sticky, the file can be swapped after validation for an attacker-chosen key. That would provision a key known to the attacker, the same outcome as R1. `/dev/shm` is sticky, so the documented path is safe. (b) `--retired-kid` is optional, so kid reuse (R5) depends on the operator. (c) The "no existing key" check and the INSERT are separate sessions, so two concurrent runs with different kids can both pass. (d) `tr ... \| grep -qE` under `pipefail` inside an `if` can mask a non-hex match through SIGPIPE; decode() would still fail closed. | (a) Refuse when the parent directory is group- or other-writable and not sticky, or copy the file once into a `mktemp -d` under `/dev/shm` and validate and read only that copy. (b) Require `--retired-kid` or an explicit `--no-source-kid`. (c) Use `BEGIN; LOCK TABLE actor_proof_keys IN SHARE ROW EXCLUSIVE MODE; INSERT ... SELECT ... WHERE NOT EXISTS (SELECT 1 FROM actor_proof_keys); COMMIT` and check the row count. (d) Use `grep -qE ... <<<"$(tr -d '\n' <file)"` or a bash regex. |
| SRBX-6 | LOW | `verify.sh --anchor-mode reset --expect-actor-proof skip` is accepted. After a logical restore the table is then neither compared nor asserted, so an operator flag silently removes the only check that no key travelled. | Refuse `skip` with `--anchor-mode reset`. |
| SRBX-7 | INFO / hardening | (a) The stream and restore filters match only `public.actor_proof_keys`. Only the table owner could create a copy (for example `audit.actor_proof_keys_copy`), and the verifiers read only `public`, but a cheap backstop would be to fail on any `TABLE DATA` entry whose tag contains `actor_proof_key`. (b) A hostile set signed with the real signing key can carry arbitrary SQL in pre-data TOC entries (for example an `INSERT INTO actor_proof_keys` inside a COMMENT or ACL `defn`), and the TOC filter cannot stop that. This is not a regression: trusting a validly signed set is the existing ADR 0122 model, and compromise of the signing key is already handled in 5a step 4. The backstop is `verify.sh`'s zero-row assertion, so verify must stay mandatory before traffic (it is: post-restore checklist). | Optional for (a). Keep the verify gate mandatory. |
| SRBX-8 | INFO | `restorecheck`'s `actor_proof_verifier_fails_closed` cannot tell the two states apart: a random MAC gets `AP002` whether or not a key exists. The evidence that no key exists is the zero-row assertion, the startup-gate refusal and the drill's canary-signed probe. The record's wording in section 2 ("Fail-closed proof") overstates it slightly. | Reword (for example "the verifier refuses an unsigned proof"). No code change needed. |
| SRBX-9 | INFO | Section 5 says `shellcheck -S warning` is clean on every `deploy/backup/*.sh`. It is not: `drill.sh` (4), `lib-cluster.sh` (1) and `lib-provision.sh` (1) give SC2034/SC2155 warnings. They are pre-existing (base `12edec0` had more) and not security-relevant. The changed scripts and `rekey-actor-proof.sh` are clean. | Correct the statement. |
| SRBX-10 | INFO | Physical backups and PITR: documented adequately (backup-restore-dr §1 / 4.2 / 5a, staging plan, ADR text item 4, R3), once SRBX-2 is fixed. Legacy baselines hold an md5 over row text that includes the secret. It cannot be inverted in practice (at least 32 random bytes), but those baselines should be retired with their legacy sets. | None beyond SRBX-2. |

### R1 assessment (SRBX-3)

- **Who can read the key.** `igaming_backup` has BYPASSRLS and table-level SELECT on every table, so it reads `actor_proof_keys.secret` with one `SELECT`. pg_dump with `--exclude-table-data` never SELECTs
  this table; it only takes `LOCK ... ACCESS SHARE`. So the role needs the privilege only for the lock. The mitigations (IAM token valid for 15 minutes, `CONNECTION LIMIT 2`, `VALID UNTIL`, log_connections, three-party
  minting, dumping from a clone) make the theft harder and detectable. They do not stop a holder of a valid token from reading the key within that window. This is **not a regression**: before this change the role
  had the same SELECT and every dump also carried the key. The change strictly reduces exposure.
- **What the key gives an attacker.** The key lets the attacker forge an ADR 0110 actor proof for ANY staff actor, scope, tenant, operation and target. Proofs have no use without a write path, but ADR 0110 exists because the
  owner put "arbitrary SQL with a stolen `igaming_runtime` credential" INSIDE the threat model (THREAT-MODEL-ARBITRARY-SQL-1 = YES). With runtime credential + key, the attacker defeats the database-enforced four-eyes rule on
  K1/K2/K3 manual ledger adjustments, payout-related approvals (after T8), launch authorisation, RG floors and staff lifecycle. In other words, they can forge an approver and move money. R1 turns this into
  "two credential thefts" (runtime and backup) instead of "runtime credential plus the application's secret store". Both credentials live outside the application, so a single compromised runner or operator
  workstation holding both is the realistic path. (Remote code execution in `platform-api` already holds the key in `ACTOR_PROOF_KEYS`, so R1 adds nothing in that scenario.) Separately, the backup credential
  alone already discloses all PII, password hashes and session MACs. That is the larger confidentiality impact, and it is unchanged.
- **Recommendation.**
  1. **Staging: ACCEPT.** Data is synthetic, providers are mocks and there is no real money; the staging key is staging-only and rotatable.
  2. **Production: BLOCK until one of the following is decided and implemented (owner and architect, recorded in `docs/decisions/`):**
     (a) the real fix: a migration that moves the key material into its own schema (for example `actor_proof_private.keys`). The backup role has no privilege on that schema, `backup.sh` uses
     `--exclude-schema` (pg_dump neither locks nor reads excluded tables), the four verifiers point at the new table, and the re-key/restore step creates the schema from a reviewed DDL file
     that `migrate verify` checks. Or
     (b) a cheaper interim for the owner to accept in writing: **pgaudit column-level object auditing**. Grant the `pgaudit.role` audit role `SELECT (secret) ON actor_proof_keys` only. pgaudit then logs exactly the statements
     that read the secret column (including `SELECT *` and row-to-text forms) and nothing that backup.sh or baseline.sql do. Add a P1 alert on any such log line from `igaming_backup`, plus the
     corrected 5a rule (SRBX-2: rotate the actor-proof key on any backup-credential exposure). This detects the theft but does not prevent it. Or
     (c) after an upgrade to PostgreSQL 17: the `MAINTAIN` privilege allows `LOCK TABLE` without SELECT, so table-level SELECT could be replaced with MAINTAIN plus column SELECT on the four
     non-secret columns. This is UNVERIFIED: it needs a drill. MAINTAIN also allows VACUUM/CLUSTER/REINDEX on the table, which is a small availability risk on a tiny table.
  3. Do not count R1 as closed by the dump exclusion. The exclusion protects the ARTEFACT; the CREDENTIAL still reads the key.

## 11. Fix round after the security review (SRBX-1..9)

- **SRBX-1**: `origin/claude/focused-wright-jw88w9` merged into the branch; `docs/governance/human-decision-register.md` and `docs/governance/task-registry.md` restored to the integration tip (`git diff origin/claude/focused-wright-jw88w9 --stat -- docs/governance` is empty).
- **SRBX-2**: `backup-restore-dr.md` (role table, section 3, section 5a step 1) now says the backup role keeps table-level SELECT (R1), there is no `backup.sh` warning, and a leaked backup credential means the actor-proof key MUST be re-keyed.
- **SRBX-4**: re-key script refuses sampled-transaction/statement logging, auto_explain parameter logging and pgaudit parameter logging; the "cannot see pgaudit" claim is corrected (plan step 8.3, R4).
- **SRBX-5**: the secret file is opened once (descriptor), checked with `stat -L` on the open file, read once into a variable and passed to psql through the environment (no re-reads by path); `--retired-kid` is mandatory unless `--first-key`; the insert is one statement under `LOCK TABLE ... SHARE ROW EXCLUSIVE` with `WHERE NOT EXISTS`, and the row is confirmed afterwards; the hex check is a bash regex (no pipefail quirk).
- **SRBX-6**: `verify.sh` refuses `--expect-actor-proof skip` unless `--anchor-mode preserved`; drilled (inside `rekey_script_refuses_unsafe_use`).
- **SRBX-7a**: the stream check and the restore filters match the table name in ANY schema (`TABLE DATA <schema> actor_proof_key*`). **SRBX-9**: shellcheck claim corrected in section 5. SRBX-8 wording: "fail-closed proof" means the verifier refuses an unsigned proof; the evidence that no key exists is the zero-row assertion plus the startup gate.
- **SRBX-3 is NOT implemented** (registered by the orchestrator as an open row "before production"). The options for the owner/architect, from the review:
  (a) a migration moving the key material into its own schema (for example `actor_proof_private.keys`); the backup role has no privilege on it, `backup.sh` uses `--exclude-schema`, the four verifiers point at the new table, and the restore/re-key creates the schema from reviewed DDL that `migrate verify` checks;
  (b) interim, accepted in writing: pgaudit column-level object auditing (grant the pgaudit audit role `SELECT (secret)` on `actor_proof_keys` only) with a P1 alert on any such read by `igaming_backup`, plus the corrected 5a rule; detects but does not prevent;
  (c) after PostgreSQL 17: the `MAINTAIN` privilege allows `LOCK TABLE` without SELECT, so table-level SELECT can be replaced by MAINTAIN plus column SELECT on the non-secret columns; UNVERIFIED, needs a drill, and MAINTAIN also permits VACUUM/CLUSTER/REINDEX on the table.

- **Re-run after the fix round** (ports 5497-5499 checked free via /proc/net/tcp first; throwaway clusters, no pkill, never 5432): full `deploy/backup/drill.sh` **131 PASS, 0 FAIL**, `A12 DRILL RESULT: PASS` (113,080 synthetic ledger entries, PITR included). `go test ./deploy/...` ok, `go vet` clean, `gofmt -l deploy` empty, shellcheck `-S warning -e SC1091,SC2034,SC2155` clean on backup.sh, restore.sh, verify.sh, rekey-actor-proof.sh, drill.sh. The 131 count is unchanged because the new refusals (txn/statement sampling, missing --retired-kid, verify skip) are folded into `rekey_script_refuses_unsafe_use`.

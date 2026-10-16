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

COUNTS_PLACEHOLDER

## 6. Residuals and limits

- **R1 (security review)**: `igaming_backup` keeps table-level `SELECT` on `actor_proof_keys`, so a stolen backup CREDENTIAL can still read the signing secret (and everything else: the role is BYPASSRLS by design). Excluding the secret from the dump protects the ARTEFACT, not the credential. Mitigations unchanged: time-boxed or IAM-only credential, three-party minting rules, production dumps from a clone (ADR 0122 section 5c). A real fix would move the secret into a table the backup role has no privilege on and exclude that whole table (a migration plus changes to the four SECURITY DEFINER verifiers: an architect decision, not done here).
- **R2**: legacy sets (made before this change) carry the key. `restore.sh` skips and warns; they must be retired and, if exposed, the key family rotated (`backup-restore-dr.md` 5a).
- **R3**: physical artefacts (base backups, WAL archive, snapshots, PITR copies) carry the key; their protection is the AWS boundary (KMS, IAM), which is NOT PROVEN here (no RDS run).
- **R4**: `rekey-actor-proof.sh` checks `log_statement`, `log_min_duration_statement` and `log_parameter_max_length_on_error`; it cannot see `pgaudit.log` / `pgaudit.log_parameter` (RDS). The staging plan step 8.3 makes the operator check them.
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

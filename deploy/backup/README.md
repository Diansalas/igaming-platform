# deploy/backup (A12, ADR 0122)

Backup, restore and point-in-time-recovery tooling and the drill that exercises it. Runbook: `docs/runbooks/backup-restore-dr.md`.
Design: `docs/decisions/0122-backup-role-and-pitr.md`. AWS plan (prepared, not applied): `docs/runbooks/aws-staging-deploy-plan.md`.

| File | Purpose |
|---|---|
| `backup.sh` | logical backup as the dedicated `igaming_backup` role: one exported snapshot for `pg_dump -Fc` and the baseline, globals, SHA-256 manifest signed with Ed25519 (REQUIRED), encryption validated BEFORE the dump and STREAMED (age/gpg recipient by reference; no plaintext on disk), keepalive for the snapshot holder, signal-safe cleanup |
| `restore.sh` | restore into a NEW database only; **signature against a pinned key first**, then checksums; two phases (`provision` as the bootstrap superuser: roles + empty DB; `load` as the NON-superuser owner); single transaction; owner-role allow-list; the environment anchor is NOT restored; allow-list validation of replayed role statements; post-restore database ACLs |
| `verify.sh` | fail-closed post-restore validation against the backup's baseline (content hashes, ledger invariants, drift 0, roles/grants/triggers/RLS, `migrate verify`, platform reconciliation, startup anchor guard) |
| `rekey-actor-proof.sh` | owner decision 5 / F1 (migration 0166): logical dumps EXCLUDE the owner-only key schema `actorproof_private` (`backup.sh` refuses a backup role that can reach it and verifies the exclusion on the stream); `restore.sh` recreates it EMPTY from `actorproof-private-schema.sql`; a restored database has no actor-proof key and fails closed (`AP002`) until this script provisions a NEW one (owner role, secret as a bound parameter from a 0600 tmpfs file; refuses a reused kid, unsafe logging, an existing key) |
| `pitr-drill.sh` | physical base backup + WAL archive, recovery of a second instance to a restore point / timestamp, WAL-gap detection (throwaway cluster only) |
| `drill.sh` | end-to-end drill on throwaway clusters under `/tmp/pgdrill-*` (never the shared 5432 cluster); `make drill-a12` / `make drill-a12-small` |
| `seed.sh` | loads synthetic data through the platform's own services (`internal/httpserver/drillseed_integration_test.go`) |
| `init-backup-role.sql` | the `igaming_backup` role definition (NOSUPERUSER, BYPASSRLS, SELECT-only, time-boxed <= 48 h; takes a SCRAM verifier, never a password) |
| `scramhash/` | Go: computes the SCRAM-SHA-256 verifier on the client so the password never appears in statement text |
| `harden-backup-surface.sql` | REVOKEs advisory-lock / `pg_logical_emit_message` / large-object / NOTIFY functions from PUBLIC and CONNECT on postgres/template1 (throwaway clusters; never applied to a shared one) |
| `baseline.sql`, `post-restore-grants.sql` | read-only baseline/verification queries; database-level ACLs a dump cannot carry |
| `restorecheck/` | Go: platform reconciliation + RLS behaviour + `VerifyEnvironmentAnchor` against a restored database as the runtime role |
| `lib-*.sh` | target guard (host canonicalisation) and credential handling; Ed25519 set signing/verification (`lib-sign.sh`); throwaway-cluster management; drill role provisioning; role-statement allow-list |

Rules baked into the scripts (and pinned by `go test ./deploy/backup/`): no secret in the repository, passwords only in 0600 passfiles, the shared
`127.0.0.1:5432` cluster is always refused, a remote host needs a per-invocation acknowledgement, RLS/triggers are never weakened outside the throwaway
drill, no sudo/pkill, no cloud calls.

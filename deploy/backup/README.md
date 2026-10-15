# deploy/backup (A12, ADR 0122)

Backup, restore and point-in-time-recovery tooling and the drill that exercises it. Runbook: `docs/runbooks/backup-restore-dr.md`.
Design: `docs/decisions/0122-backup-role-and-pitr.md`. AWS plan (prepared, not applied): `docs/runbooks/aws-staging-deploy-plan.md`.

| File | Purpose |
|---|---|
| `backup.sh` | logical backup as the dedicated `igaming_backup` role: one exported snapshot for `pg_dump -Fc` and the baseline, globals, SHA-256 manifest, encryption hook (age/gpg recipient by reference) |
| `restore.sh` | restore into a NEW database only; checksums first; single transaction; allow-list validation of replayed role statements; post-restore database ACLs |
| `verify.sh` | fail-closed post-restore validation against the backup's baseline (content hashes, ledger invariants, drift 0, roles/grants/triggers/RLS, `migrate verify`, platform reconciliation, startup anchor guard) |
| `pitr-drill.sh` | physical base backup + WAL archive, recovery of a second instance to a restore point / timestamp, WAL-gap detection (throwaway cluster only) |
| `drill.sh` | end-to-end drill on throwaway clusters under `/tmp/pgdrill-*` (never the shared 5432 cluster); `make drill-a12` / `make drill-a12-small` |
| `seed.sh` | loads synthetic data through the platform's own services (`internal/httpserver/drillseed_integration_test.go`) |
| `init-backup-role.sql` | the `igaming_backup` role definition (NOSUPERUSER, BYPASSRLS, SELECT-only, time-boxed) |
| `baseline.sql`, `post-restore-grants.sql` | read-only baseline/verification queries; database-level ACLs a dump cannot carry |
| `restorecheck/` | Go: platform reconciliation + RLS behaviour + `VerifyEnvironmentAnchor` against a restored database as the runtime role |
| `lib-*.sh` | target guard and credential handling; throwaway-cluster management; drill role provisioning; role-statement allow-list |

Rules baked into the scripts (and pinned by `go test ./deploy/backup/`): no secret in the repository, passwords only in 0600 passfiles, the shared
`127.0.0.1:5432` cluster is always refused, a remote host needs a per-invocation acknowledgement, RLS/triggers are never weakened outside the throwaway
drill, no sudo/pkill, no cloud calls.

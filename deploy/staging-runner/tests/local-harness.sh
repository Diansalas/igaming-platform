#!/usr/bin/env bash
# LOCAL test harness for the staging runner modes (STAGING-PREP-3). DEVELOPMENT / REVIEW ONLY: it runs the runner's shell
# modes against a THROWAWAY PostgreSQL 16 cluster under /tmp/pgdrill-XXXXXX on one of the drill ports 5497-5499 (never the
# shared 5432 cluster, no sudo, no pkill; the cluster runs as the OS user `postgres` through `runuser`, exactly like
# deploy/backup/drill.sh, whose libraries it reuses). Nothing here touches AWS.
#
#   deploy/staging-runner/tests/local-harness.sh up                 build the cluster, roles, migrations, binaries; print the state file
#   deploy/staging-runner/tests/local-harness.sh run <mode> [K=V..] run a mode exactly as the ECS task would, against it
#   deploy/staging-runner/tests/local-harness.sh psql [args..]      psql as the cluster superuser over its private socket
#   deploy/staging-runner/tests/local-harness.sh clone              copy the cluster to a second drill port (stands in for a PITR clone)
#   deploy/staging-runner/tests/local-harness.sh down               stop the cluster(s) and remove the root
#
# What differs from the ECS task (documented in docs/plans/staging-prep-3-record.md "What could not be tested locally"):
#   * the "master" is the cluster superuser `postgres` (a stock cluster cannot grant BYPASSRLS to a non-superuser), the OWNER is
#     `igaming` (DB_OWNER_USER); on RDS both are the master `igaming`;
#   * the backup login uses a client-side SCRAM verifier and a password file instead of IAM authentication (RUNNER_BACKUP_PASSWORD_FILE),
#     the code path of backup.sh's BACKUP_IAM_TOKEN_COMMAND is the same;
#   * no TLS, no RDS CA bundle, no rds_iam / rds_superuser roles, no pgaudit.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
BK="${REPO_ROOT}/deploy/backup"
STATE_DIR="${RUNNER_HARNESS_STATE_DIR:-${TMPDIR:-/tmp}}"
STATE="${RUNNER_HARNESS_STATE:-${STATE_DIR}/staging-runner-harness.state}"

# shellcheck source=deploy/backup/lib-common.sh
. "${BK}/lib-common.sh"
# shellcheck source=deploy/backup/lib-cluster.sh
. "${BK}/lib-cluster.sh"
# shellcheck source=deploy/backup/lib-provision.sh
. "${BK}/lib-provision.sh"

port_in_use() { awk -v p="$(printf '%04X' "$1")" 'NR > 1 && $4 == "0A" { split($2, a, ":"); if (a[2] == p) f = 1 } END { exit f ? 0 : 1 }' /proc/net/tcp /proc/net/tcp6 2>/dev/null; }

cmd_up() {
  command -v go >/dev/null || bk_die "go is required to build the helper binaries"
  local port="" p
  for p in 5497 5498 5499; do port_in_use "$p" || { port="$p"; break; }; done
  [ -n "$port" ] || bk_die "no free throwaway port among 5497-5499"
  cl_new_root
  DRILL_CLUSTERS=()
  cl_init src "$port" ""
  cl_start src "$port"
  mkdir -p "${DRILL_ROOT}/bin" "${DRILL_ROOT}/scratch"
  chmod 700 "${DRILL_ROOT}/bin" "${DRILL_ROOT}/scratch"
  ( cd "$REPO_ROOT" && for c in cmd/migrate cmd/seed-market cmd/seed-staging-world cmd/seed-admin deploy/backup/restorecheck deploy/backup/scramhash; do go build -o "${DRILL_ROOT}/bin/$(basename "$c")" "./$c"; done )
  export SCRAMHASH_BIN="${DRILL_ROOT}/bin/scramhash"
  prov_roles src "$port"
  local ipw spw
  ipw="$(awk -F: -v u=igaming '$4==u {print $5}' "${DRILL_ROOT}/secrets/pgpass")"
  DATABASE_URL="postgres://igaming:${ipw}@127.0.0.1:${port}/${SRC_DB_NAME}?sslmode=disable" "${DRILL_ROOT}/bin/migrate" -dir "${REPO_ROOT}/migrations" up >"${DRILL_ROOT}/work/migrate.log" 2>&1
  prov_reapply_app_roles src "$port"
  spw="$(cl_random_secret)"
  IGPW="$spw" cl_super src "$port" -d postgres <<'SQL' >/dev/null
\getenv pw IGPW
ALTER ROLE postgres WITH PASSWORD :'pw';
SQL
  printf '%s' "$spw" >"${DRILL_ROOT}/secrets/super.pw"; chmod 600 "${DRILL_ROOT}/secrets/super.pw"
  # throwaway staff MFA key families for the seed-world mode (two distinct random families, 48 bytes each)
  printf 'stg-v1:%s' "$(head -c 48 /dev/urandom | base64 -w0)" >"${DRILL_ROOT}/secrets/mfa-factor.key"
  printf 'stg-v1:%s' "$(head -c 48 /dev/urandom | base64 -w0)" >"${DRILL_ROOT}/secrets/mfa-mac.key"
  chmod 600 "${DRILL_ROOT}/secrets/mfa-factor.key" "${DRILL_ROOT}/secrets/mfa-mac.key"
  {
    printf 'HARNESS_ROOT=%q\n' "$DRILL_ROOT"
    printf 'HARNESS_PORT=%q\n' "$port"
  } >"$STATE"
  chmod 600 "$STATE"
  echo "harness: cluster on 127.0.0.1:${port}, root ${DRILL_ROOT}, state ${STATE}"
}

load_state() {
  [ -f "$STATE" ] || bk_die "no harness state (${STATE}); run 'up' first"
  # shellcheck source=/dev/null
  . "$STATE"
  DRILL_ROOT="$HARNESS_ROOT"
}

pw_of() { awk -F: -v u="$1" '$4==u {print $5; exit}' "${HARNESS_ROOT}/secrets/pgpass"; }

cmd_run() {
  load_state
  local mode="${1:?mode}"; shift
  local -a kv=("$@")
  local scratch="${HARNESS_ROOT}/scratch"
  # HARNESS_ENTRY=entrypoint.sh runs the entrypoint directly (without boot.sh) to test its own hardening; the default is the image's ENTRYPOINT, boot.sh under sh
  local entry="${HARNESS_ENTRY:-boot.sh}" runner=(sh) entry_path
  [ "$(basename "$entry")" = boot.sh ] || runner=(bash)
  case "$entry" in /*) entry_path="$entry" ;; *) entry_path="${REPO_ROOT}/deploy/staging-runner/${entry}" ;; esac
  # a clean environment: only what the ECS task definition would provide, plus the explicit local-harness switches
  env -i PATH="$PATH" HOME="${HOME:-/root}" \
    APP_ENV="${HARNESS_APP_ENV:-development}" RUNNER_ALLOW_LOCAL=local-test-harness RN_ROOT="$REPO_ROOT" RN_BIN="${HARNESS_ROOT}/bin" \
    RUNNER_SCRATCH_BASE="$scratch" RUNNER_BACKUP_PASSWORD_FILE="${scratch}/backup.pw" \
    RDS_HOST=127.0.0.1 RDS_PORT="$HARNESS_PORT" DB_NAME="$SRC_DB_NAME" DB_MASTER_USER=postgres DB_OWNER_USER=igaming DB_RUNTIME_USER=igaming_runtime \
    DB_MASTER_PASSWORD="$(cat "${HARNESS_ROOT}/secrets/super.pw")" DB_OWNER_PASSWORD="$(pw_of igaming)" DB_RUNTIME_PASSWORD="$(pw_of igaming_runtime)" \
    ACTOR_PROOF_ACTIVE_KID="${ACTOR_PROOF_ACTIVE_KID:-stg-v1}" \
    STAFF_MFA_FACTOR_ACTIVE_KID=stg-v1 STAFF_MFA_MAC_ACTIVE_KID=stg-v1 \
    STAFF_MFA_FACTOR_KEYS="$(cat "${HARNESS_ROOT}/secrets/mfa-factor.key")" STAFF_MFA_MAC_KEYS="$(cat "${HARNESS_ROOT}/secrets/mfa-mac.key")" \
    ${ACTOR_PROOF_KEYS:+ACTOR_PROOF_KEYS="$ACTOR_PROOF_KEYS"} ${MOCK_GEOLOCATION_COUNTRY:+MOCK_GEOLOCATION_COUNTRY="$MOCK_GEOLOCATION_COUNTRY"} \
    ${kv[@]+"${kv[@]}"} \
    "${runner[@]}" "$entry_path" "$mode"
}

cmd_psql() {
  load_state
  psql -X -q -At -v ON_ERROR_STOP=1 -h "$(cl_sock src)" -p "$HARNESS_PORT" -U postgres "$@"
}

# clone: an exact physical copy of the source cluster on another drill port (stands in for a PITR-restored RDS instance)
cmd_clone() {
  load_state
  local cport="" p cdir sdir
  for p in 5497 5498 5499; do [ "$p" = "$HARNESS_PORT" ] || port_in_use "$p" || { cport="$p"; break; }; done
  [ -n "$cport" ] || bk_die "no free throwaway port for the clone"
  sdir="$(cl_dir src)"; cdir="$(cl_dir clone)"
  # shellcheck disable=SC2034  # read by lib-cluster.sh
  DRILL_CLUSTERS=(src)
  cl_stop src
  pgas cp -a "$sdir" "$cdir"
  rm -f "${cdir}/data/postmaster.pid"
  sed -i -e "s/^port = .*/port = ${cport}/" -e "s#^unix_socket_directories = .*#unix_socket_directories = '${cdir}/sock'#" "${cdir}/data/postgresql.conf"
  cl_start src "$HARNESS_PORT"
  cl_start clone "$cport"
  printf 'HARNESS_CLONE_PORT=%q\n' "$cport" >>"$STATE"
  echo "harness: clone on 127.0.0.1:${cport}"
}

cmd_down() {
  load_state
  # shellcheck disable=SC2034  # read by lib-cluster.sh
  DRILL_CLUSTERS=(src)
  cl_stop src
  [ ! -d "$(cl_dir clone)" ] || cl_stop clone
  case "$HARNESS_ROOT" in /tmp/pgdrill-??????) rm -rf "$HARNESS_ROOT" ;; *) echo "harness: refusing to remove unexpected root ${HARNESS_ROOT}" >&2 ;; esac
  rm -f "$STATE"
  echo "harness: removed"
}

case "${1:-}" in
  up) cmd_up ;;
  run) shift; cmd_run "$@" ;;
  psql) shift; cmd_psql "$@" ;;
  clone) cmd_clone ;;
  down) cmd_down ;;
  *) sed -n '2,/^set -euo/p' "$0" | sed -e 's/^# \{0,1\}//' -e '/^set -euo/d'; exit 2 ;;
esac

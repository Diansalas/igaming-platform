#!/usr/bin/env bash
# Shared helpers of the staging runner (STAGING-PREP-3). SOURCED by entrypoint.sh and the modes, never executed.
#
# The runner is a ONE-OFF in-VPC task of deploy/docker/staging-runner.Dockerfile. It exists because the staging
# database is private: every operation that is not migrate / role-init / seed-admin runs here, selected by ONE command
# argument (a MODE) with NON-secret parameters as container-environment overrides (docs/plans/staging-prep-3-record.md).
#
# Output contract (machine readable, one record per line, stable prefixes):
#   RESULT|<name>|PASS|FAIL|INFO|FINDING|BLOCKED|<detail>   one verdict or observation
#   TIMING|<name>|<seconds>                                  measured durations
#   PITR|T_utc=...                                           values later modes need (digests, fingerprints; no secret)
#   SUMMARY|<mode>|pass=N|fail=N|finding=N|blocked=N|info=N  last summary line
#   RUNNER|STOP|<reason>                                     a STOP-level condition (exit 3)
#   RUNNER|END|exit=<rc>|mode=<mode>                         ALWAYS the last line (deploy.sh waits for it)
# Exit status: 0 = no FAIL; 1 = at least one FAIL; 2 = refused / bad usage / preflight; 3 = STOP.
# FINDING = RDS (or the platform) answered something the plan flagged "unverified": recorded, not a script failure.
# BLOCKED = a step that cannot run because of a recorded finding; the run continues with what is still possible.
#
# SECRET HANDLING (hard rules, tested statically in deploy/staging-runner/runner_static_test.go):
#   * passwords arrive ONLY as environment variables injected by the ECS execution role (DB_MASTER_PASSWORD,
#     DB_RUNTIME_PASSWORD, ACTOR_PROOF_KEYS, STAFF_MFA_*). entrypoint.sh moves them, before it runs any other program, into
#     NON-EXPORTED shell variables (RN_S_*) and unsets the environment copies, so no child process (mktemp, psql, ...) inherits them;
#     they are then written to 0600 libpq passfiles / tmpfs files. Honest limit: the kernel's /proc/<pid>/environ of PID 1 / the
#     shell keeps the initial environment block, readable by any process of the same uid in the container (same trust as every ECS
#     secret; there is no ECS Exec);
#   * no password, token or key is ever placed on a command line, in a URL, in a -v variable or in a log line; every
#     message passes through rn_scrub, which masks the registered secret values;
#   * no `set -x`, no `bash -x`, no PGPASSWORD, no echo of a secret-bearing variable;
#   * every file with key material lives in a 0700 directory created by this process and is removed on exit (the task's
#     ephemeral storage is destroyed with the task in any case).

# shellcheck shell=bash

RN_ROOT="${RN_ROOT:-/opt/igaming}"
# shellcheck disable=SC2034  # used by the sourced modes
RN_DIR="${RN_ROOT}/deploy/staging-runner"
RN_BACKUP_DIR="${RN_ROOT}/deploy/backup"
RN_BIN="${RN_BIN:-${RN_ROOT}/bin}"
RN_BACKUP_USER=igaming_backup
# Shared parameter formats of the seeding modes. REASON may hold spaces (SRP3-11: deploy.sh's override value pattern allows one), so a
# human sentence works; the Go side (stagingseed.CheckField) applies its own bounds again.
# shellcheck disable=SC2034  # used by the sourced modes
RN_RE_AUTHREF='^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,118}$'
# shellcheck disable=SC2034
RN_RE_OPERATOR='^[A-Za-z0-9][A-Za-z0-9._@-]{0,78}$'
# shellcheck disable=SC2034
RN_RE_REASON='^[A-Za-z0-9][A-Za-z0-9._:/# -]{0,118}$'
RN_MODE=""
RN_SCRATCH=""
RN_SHM=""
RN_RESULTS=""
RN_CA=""
RN_SSLMODE=verify-full
RN_HOST=""
RN_LOCAL=0
RN_T0=0
RN_SECRETS=()
RN_BACKUP_TOKEN_AT=0
RN_EXIT_HOOKS=()

rn_now() { date +%s.%N; }
rn_elapsed() { awk -v a="$1" -v b="${2:-$(date +%s.%N)}" 'BEGIN { printf "%.2f", b - a }'; }

# ---------------------------------------------------------------- output
rn_register_secret() { [ -n "${1:-}" ] && [ "${#1}" -ge 6 ] && RN_SECRETS+=("$1"); return 0; }

rn_scrub() { # text -> text with registered secret values masked
  local s="$1" x
  for x in ${RN_SECRETS[@]+"${RN_SECRETS[@]}"}; do s="${s//"$x"/***}"; done
  printf '%s' "$s"
}

rn_oneline() { # text -> one line, no field separator, bounded, scrubbed
  local s
  s="$(rn_scrub "$1")"
  s="${s//$'\n'/ }"; s="${s//$'\r'/ }"; s="${s//$'\t'/ }"; s="${s//|/¦}"
  printf '%s' "${s:0:${RN_MAX_DETAIL:-420}}"
}

rn_wire() { # an already field-separated record: scrub and bound it, keep its field separators
  local s
  s="$(rn_scrub "$1")"
  s="${s//$'\n'/ }"; s="${s//$'\r'/ }"; s="${s//$'\t'/ }"
  printf '%s' "${s:0:${RN_MAX_WIRE:-1200}}"
}

rn_log() { printf 'runner: %s\n' "$(rn_oneline "$*")"; }

rn_result() { # name status detail
  local st="$2"
  case "$st" in PASS|FAIL|INFO|FINDING|BLOCKED) ;; *) st=FAIL ;; esac
  printf 'RESULT|%s|%s|%s\n' "$1" "$st" "$(rn_oneline "${3:-}")"
  [ -z "$RN_RESULTS" ] || printf '%s\n' "$st" >>"$RN_RESULTS"
}

rn_kv() { printf '%s|%s|%s\n' "$1" "$2" "$(rn_oneline "$3")"; } # PITR|key|value, TIMING|name|secs ...

rn_die() { # exit 2: refused / bad usage / preflight
  printf 'RUNNER|REFUSED|%s\n' "$(rn_oneline "$*")" >&2   # stderr: rn_die is also called inside $(...) (rn_param), whose stdout is captured
  exit 2
}

rn_stop() { # exit 3: a STOP-level condition (plan section 11): nothing is worked around
  rn_result "stop_condition" FAIL "$*"
  printf 'RUNNER|STOP|%s\n' "$(rn_oneline "$*")"
  exit 3
}

rn_first_line() { # file -> first non-empty line, scrubbed and bounded (never the whole stderr)
  local l
  l="$(grep -m1 -v '^[[:space:]]*$' "$1" 2>/dev/null || true)"
  rn_oneline "${l:-no output}"
}

rn_need_tools() { local t; for t in "$@"; do command -v "$t" >/dev/null 2>&1 || rn_die "required tool not found in the image: $t"; done; }

# rn_param NAME REGEX [DEFAULT] -> prints the validated value; refuses (exit 2) a malformed one
rn_param() {
  local name="$1" re="$2" def="${3-__required__}" v
  v="${!name-}"
  if [ -z "$v" ]; then
    [ "$def" != "__required__" ] || rn_die "parameter ${name} is required"
    v="$def"
  fi
  [[ "$v" =~ $re ]] || rn_die "parameter ${name} has an invalid format"
  printf '%s' "$v"
}

# ---------------------------------------------------------------- lifecycle
# rn_on_exit <function> : run it (best effort) when the runner exits, before the scratch directories are removed (drop a restore-check database, a loader role)
rn_on_exit() { RN_EXIT_HOOKS+=("$1"); }

rn_cleanup() {
  local rc=$? h
  set +e
  for h in ${RN_EXIT_HOOKS[@]+"${RN_EXIT_HOOKS[@]}"}; do "$h" || true; done
  [ -n "$RN_SHM" ] && [ -d "$RN_SHM" ] && rm -rf "$RN_SHM"
  [ -n "$RN_SCRATCH" ] && [ -d "$RN_SCRATCH" ] && rm -rf "$RN_SCRATCH"
  printf 'RUNNER|END|exit=%s|mode=%s\n' "$rc" "$RN_MODE"
  exit "$rc"
}

# rn_init MODE : environment gate, private scratch, traps. The ONLY entry to a mode.
rn_init() {
  RN_MODE="$1"
  umask 077
  trap rn_cleanup EXIT   # first: even a refusal below ends with the RUNNER|END line deploy.sh waits for
  case "${APP_ENV:-}" in
    development|staging) ;;
    production) rn_die "APP_ENV=production: the staging runner never runs against production" ;;
    *) rn_die "APP_ENV must be staging (or development for the local test harness); got '${APP_ENV:-}'" ;;
  esac
  local base="${RUNNER_SCRATCH_BASE:-/home/runner/scratch}"
  [ -d "$base" ] || (mkdir -p "$base" && chmod 700 "$base") || rn_die "cannot create the scratch base ${base}"
  RN_SCRATCH="$(mktemp -d "${base}/rn.XXXXXXXX")" || rn_die "cannot create a private scratch directory"
  chmod 700 "$RN_SCRATCH"
  if [ -d /dev/shm ] && [ -w /dev/shm ]; then
    RN_SHM="$(mktemp -d /dev/shm/rn.XXXXXXXX)" || rn_die "cannot create a tmpfs directory"
    chmod 700 "$RN_SHM"
  else
    RN_SHM="${RN_SCRATCH}/shm"; mkdir -m 700 "$RN_SHM"
  fi
  RN_RESULTS="${RN_SCRATCH}/results.txt"; : >"$RN_RESULTS"
  RN_T0="$(rn_now)"
  trap 'exit 143' TERM
  trap 'exit 130' INT
  printf 'RUNNER|START|mode=%s|utc=%s|host=%s\n' "$RN_MODE" "$(date -u +%FT%TZ)" "$(hostname)"
}

rn_count() { grep -c "^$1\$" "$RN_RESULTS" 2>/dev/null || true; }

# rn_finish : summary line, exit status from the verdicts
rn_finish() {
  local f
  f="$(rn_count FAIL)"
  printf 'TIMING|%s_total_seconds|%s\n' "$RN_MODE" "$(rn_elapsed "$RN_T0")"
  printf 'SUMMARY|%s|pass=%s|fail=%s|finding=%s|blocked=%s|info=%s\n' "$RN_MODE" "$(rn_count PASS)" "$f" "$(rn_count FINDING)" "$(rn_count BLOCKED)" "$(rn_count INFO)"
  [ "${f:-0}" -eq 0 ] || exit 1
  exit 0
}

# ---------------------------------------------------------------- target / credentials
rn_is_loopback() { case "$1" in 127.0.0.1|localhost) return 0 ;; *) return 1 ;; esac; }

# rn_validate_target <host> <port> <what> : the staging RDS (or, for the local test harness only, a throwaway cluster on 5497-5499)
rn_validate_target() {
  local host="$1" port="$2" what="$3"
  [[ "$host" =~ ^[a-z0-9]([a-z0-9.-]{0,200})$ ]] || rn_die "${what}: malformed host"
  [[ "$port" =~ ^[0-9]{2,5}$ ]] || rn_die "${what}: malformed port"
  if rn_is_loopback "$host"; then
    [ "${RUNNER_ALLOW_LOCAL:-}" = "local-test-harness" ] || rn_die "${what}: a loopback target is only for the local test harness"
    case "$port" in 5497|5498|5499) ;; *) rn_die "${what}: a loopback target must be a throwaway drill port (5497-5499), never the shared 5432 cluster" ;; esac
    RN_LOCAL=1
  else
    [[ "$host" =~ ^[a-z0-9-]+\.[a-z0-9]+\.eu-central-1\.rds\.amazonaws\.com$ ]] || rn_die "${what}: the host must be an RDS instance endpoint in eu-central-1"
    [ "$port" = "5432" ] || rn_die "${what}: unexpected port"
    RN_LOCAL=0
  fi
}

# rn_pgpass_line host port user password : one libpq passfile line (':' and '\' escaped)
rn_pgpass_line() {
  local h="$1" p="$2" u="$3" w="$4"
  w="${w//\\/\\\\}"; w="${w//:/\\:}"
  printf '%s:%s:*:%s:%s\n' "$h" "$p" "$u" "$w"
}

# rn_load_credentials : the non-exported RN_S_MASTER / RN_S_OWNER / RN_S_RUNTIME (captured from DB_*_PASSWORD by entrypoint.sh) -> one 0600
# passfile in tmpfs. Registers the values for scrubbing.
rn_load_credentials() {
  : "${RDS_HOST:?RDS_HOST is required}" "${RDS_PORT:?RDS_PORT is required}" "${DB_NAME:?DB_NAME is required}" "${DB_MASTER_USER:?DB_MASTER_USER is required}"
  rn_validate_target "$RDS_HOST" "$RDS_PORT" "RDS_HOST"
  [[ "$DB_NAME" =~ ^[a-z][a-z0-9_]{0,62}$ ]] || rn_die "DB_NAME is malformed"
  [[ "$DB_MASTER_USER" =~ ^[a-z][a-z0-9_]{0,62}$ ]] || rn_die "DB_MASTER_USER is malformed"
  RN_HOST="$RDS_HOST"
  DB_OWNER_USER="${DB_OWNER_USER:-$DB_MASTER_USER}"
  [[ "$DB_OWNER_USER" =~ ^[a-z][a-z0-9_]{0,62}$ ]] || rn_die "DB_OWNER_USER is malformed"
  DB_RUNTIME_USER="${DB_RUNTIME_USER:-igaming_runtime}"
  RN_PGPASS="${RN_SHM}/db.pgpass"
  : >"$RN_PGPASS"; chmod 600 "$RN_PGPASS"
  if [ -n "${RN_S_MASTER:-}" ]; then
    rn_register_secret "$RN_S_MASTER"
    rn_pgpass_line "$RN_HOST" "$RDS_PORT" "$DB_MASTER_USER" "$RN_S_MASTER" >>"$RN_PGPASS"
    if [ -n "${RN_S_OWNER:-}" ]; then
      rn_register_secret "$RN_S_OWNER"
      rn_pgpass_line "$RN_HOST" "$RDS_PORT" "$DB_OWNER_USER" "$RN_S_OWNER" >>"$RN_PGPASS"
    elif [ "$DB_OWNER_USER" != "$DB_MASTER_USER" ]; then
      rn_die "DB_OWNER_USER differs from DB_MASTER_USER but DB_OWNER_PASSWORD is not set"
    fi
  fi
  if [ -n "${RN_S_RUNTIME:-}" ]; then
    rn_register_secret "$RN_S_RUNTIME"
    rn_pgpass_line "$RN_HOST" "$RDS_PORT" "$DB_RUNTIME_USER" "$RN_S_RUNTIME" >>"$RN_PGPASS"
  fi
  RN_S_MASTER=""; RN_S_OWNER=""; RN_S_RUNTIME=""
  [ -s "$RN_PGPASS" ] || rn_die "no database credential was injected (DB_MASTER_PASSWORD missing)"
}

# rn_fetch_ca : the RDS CA bundle for sslmode=verify-full (never `require`). Logs its SHA-256 and certificate count.
rn_fetch_ca() {
  if [ "$RN_LOCAL" = 1 ]; then RN_SSLMODE=prefer; RN_CA=""; rn_result ca_bundle INFO "local test harness: TLS not used"; return 0; fi
  local url="${RDS_CA_BUNDLE_URL:-https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem}" f="${RN_SCRATCH}/rds-ca-bundle.pem" n sha
  case "$url" in https://truststore.pki.rds.amazonaws.com/*) ;; *) rn_die "RDS_CA_BUNDLE_URL must be on https://truststore.pki.rds.amazonaws.com/" ;; esac
  if ! curl -fsS --proto '=https' --tlsv1.2 --max-time 60 --retry 3 -o "$f" "$url" 2>"${RN_SCRATCH}/ca.err"; then
    rn_stop "cannot download the RDS CA bundle (needed for sslmode=verify-full): $(rn_first_line "${RN_SCRATCH}/ca.err")"
  fi
  n="$(grep -c 'BEGIN CERTIFICATE' "$f" || true)"
  [ "${n:-0}" -ge 1 ] || rn_stop "the downloaded RDS CA bundle holds no certificate"
  sha="$(sha256sum <"$f" | cut -d' ' -f1)"
  chmod 600 "$f"
  RN_CA="$f"; RN_SSLMODE=verify-full
  rn_result ca_bundle PASS "downloaded from ${url}; sha256=${sha}; certificates=${n} (compare the hash with an independent download; connections use sslmode=verify-full)"
}

# rn_pg_env <who> : prints NAME=VALUE assignments for `env` (who = master|owner|runtime|loader|backup)
rn_pg_env() {
  local who="$1" user pf="$RN_PGPASS"
  case "$who" in
    master) user="$DB_MASTER_USER" ;;
    owner) user="$DB_OWNER_USER" ;;
    runtime) user="$DB_RUNTIME_USER" ;;
    loader) user="${RN_LOADER_USER:?}"; pf="${RN_LOADER_PGPASS:?}" ;;
    backup) user="$RN_BACKUP_USER"; pf="${RN_PGPASS_BACKUP:?}" ;;
    *) rn_die "internal: unknown connection role ${who}" ;;
  esac
  printf '%s\n' "PGHOST=${RN_HOST}" "PGPORT=${RDS_PORT}" "PGUSER=${user}" "PGPASSFILE=${pf}" "PGSSLMODE=${RN_SSLMODE}" "PGCONNECT_TIMEOUT=15" "PGAPPNAME=staging-runner-${RN_MODE}"
  [ -z "$RN_CA" ] || printf '%s\n' "PGSSLROOTCERT=${RN_CA}"
}

# rn_psql <who> <database> <psql args...> : psql -X -q -At -v ON_ERROR_STOP=1 as that role. No password in argv or env.
rn_psql() {
  local who="$1" db="$2"; shift 2
  local -a envs=()
  if [ "$who" = backup ]; then rn_backup_refresh || return 1; fi   # in this shell, so the 5-minute token cache holds
  mapfile -t envs < <(rn_pg_env "$who")
  env -u PGPASSWORD -u PGSERVICE -u PGOPTIONS "${envs[@]}" PGDATABASE="$db" psql -X -q -At -v ON_ERROR_STOP=1 "$@"
}

# ---------------------------------------------------------------- backup role (IAM database authentication)
# rn_backup_token_command : the command string handed to backup.sh as BACKUP_IAM_TOKEN_COMMAND (operator-trusted configuration, no secret in it)
rn_backup_token_command() {
  if [ "$RN_LOCAL" = 1 ]; then
    [ -n "${RUNNER_BACKUP_PASSWORD_FILE:-}" ] || rn_die "internal: local harness needs RUNNER_BACKUP_PASSWORD_FILE"
    printf 'cat %q' "$RUNNER_BACKUP_PASSWORD_FILE"
  else
    printf 'aws rds generate-db-auth-token --hostname %q --port %q --username %q --region %q' "$RN_HOST" "$RDS_PORT" "$RN_BACKUP_USER" "${AWS_REGION:-eu-central-1}"
  fi
}

# rn_backup_refresh : write a fresh passfile (valid ~15 minutes) for the backup role; at most every 5 minutes
rn_backup_refresh() {
  local now t cmd
  # pitr-verify: the disposable clone's backup role uses a short-lived password credential (a static passfile), not IAM
  if [ -n "${RN_BACKUP_STATIC_PASSFILE:-}" ]; then RN_PGPASS_BACKUP="$RN_BACKUP_STATIC_PASSFILE"; return 0; fi
  now="$(date +%s)"
  if [ -n "${RN_PGPASS_BACKUP:-}" ] && [ -s "$RN_PGPASS_BACKUP" ] && [ $((now - RN_BACKUP_TOKEN_AT)) -lt 300 ]; then return 0; fi
  RN_PGPASS_BACKUP="${RN_SHM}/backup.pgpass"
  cmd="$(rn_backup_token_command)"
  t="$(bash -c "$cmd" 2>"${RN_SCRATCH}/token.err")" || { rn_result backup_iam_token FAIL "cannot obtain the backup-role credential: $(rn_first_line "${RN_SCRATCH}/token.err")"; return 1; }
  [[ "$t" =~ ^[!-~]{8,4096}$ ]] || { rn_result backup_iam_token FAIL "the token command produced no usable single-line token"; return 1; }
  rn_register_secret "$t"
  (umask 077; rn_pgpass_line "$RN_HOST" "$RDS_PORT" "$RN_BACKUP_USER" "$t" >"${RN_PGPASS_BACKUP}.new") && mv -f "${RN_PGPASS_BACKUP}.new" "$RN_PGPASS_BACKUP"
  RN_BACKUP_TOKEN_AT="$now"
}

# rn_backup_token_fresh : force a new token file now (for scripts that take a passfile and run for a while, e.g. verify.sh)
rn_backup_token_fresh() { RN_BACKUP_TOKEN_AT=0; rn_backup_refresh; }

# ---------------------------------------------------------------- environment used by deploy/backup scripts
# rn_backup_env <database> : prints the BACKUP_* assignments (no password: IAM mode derives it per connection)
rn_backup_env() {
  local db="$1"
  printf '%s\n' "BACKUP_PGHOST=${RN_HOST}" "BACKUP_PGPORT=${RDS_PORT}" "BACKUP_PGDATABASE=${db}" "BACKUP_PGUSER=${RN_BACKUP_USER}"
  if [ "$RN_LOCAL" = 1 ]; then
    printf '%s\n' "BACKUP_PGSSLMODE=prefer"
  else
    printf '%s\n' "BACKUP_ALLOW_REMOTE_TARGET=${RN_HOST}" "BACKUP_PGSSLROOTCERT=${RN_CA}" "BACKUP_PGSSLMODE=verify-full"
  fi
  printf '%s\n' "BACKUP_TMPDIR=${RN_SCRATCH}/tmp" "BACKUP_IAM_TOKEN_COMMAND=$(rn_backup_token_command)"
}

# rn_backup_env_passfile <database> <user> <passfile> : the BACKUP_* assignments for a script that connects with a libpq passfile (restore.sh as master / loader)
rn_backup_env_passfile() {
  local db="$1" user="$2" pf="$3"
  printf '%s\n' "BACKUP_PGHOST=${RN_HOST}" "BACKUP_PGPORT=${RDS_PORT}" "BACKUP_PGDATABASE=${db}" "BACKUP_PGUSER=${user}" "BACKUP_PGPASSFILE=${pf}" "BACKUP_TMPDIR=${RN_SCRATCH}/tmp"
  if [ "$RN_LOCAL" = 1 ]; then
    printf '%s\n' "BACKUP_PGSSLMODE=prefer"
  else
    printf '%s\n' "BACKUP_ALLOW_REMOTE_TARGET=${RN_HOST}" "BACKUP_PGSSLROOTCERT=${RN_CA}" "BACKUP_PGSSLMODE=verify-full"
  fi
}

rn_ensure_tmp() { [ -d "${RN_SCRATCH}/tmp" ] || mkdir -m 700 "${RN_SCRATCH}/tmp"; }

# rn_run_logged <label> <logfile> <cmd...> : run, store stdout+stderr in a file (never streamed raw: a script may echo more than we want), return its status
rn_run_logged() { local label="$1" out="$2"; shift 2; "$@" >"$out" 2>&1; local rc=$?; printf '%s' "$rc" >"${out}.rc"; return "$rc"; }

# rn_emit_sql_output <file> : relay the RESULT|/SNAPSHOT|/GRANT| lines a SQL pack printed (scrubbed), counting the verdicts
rn_emit_sql_output() {
  local line st
  while IFS= read -r line; do
    case "$line" in
      RESULT\|*\|PASS\|*|RESULT\|*\|FAIL\|*|RESULT\|*\|INFO\|*|RESULT\|*\|FINDING\|*|RESULT\|*\|BLOCKED\|*)
        printf '%s\n' "$(rn_wire "$line")"
        st="${line#RESULT|*|}"; st="${st%%|*}"
        printf '%s\n' "$st" >>"$RN_RESULTS" ;;
      SNAPSHOT\|*|GRANT\|*) printf '%s\n' "$(rn_wire "$line")" ;;
    esac
  done <"$1"
}

# rn_migrate_url <who> : a password-free DATABASE_URL for cmd/migrate and cmd/seed-market (the passfile supplies the credential)
rn_migrate_url() {
  local user
  case "$1" in owner) user="$DB_OWNER_USER" ;; runtime) user="$DB_RUNTIME_USER" ;; *) rn_die "internal: rn_migrate_url $1" ;; esac
  local u="postgres://${user}@${RN_HOST}:${RDS_PORT}/${DB_NAME}?sslmode=${RN_SSLMODE}"
  [ -z "$RN_CA" ] || u="${u}&sslrootcert=${RN_CA}"
  printf '%s' "$u"
}

# rn_show_script_output <file> <prefix> : print the lines of a deploy/backup script's output that are safe and useful (TIMING|, CHK|, VERIFY RESULT, backup:/restore: messages), scrubbed
rn_show_script_output() {
  local f="$1" line
  while IFS= read -r line; do
    case "$line" in
      TIMING\|*) printf '%s\n' "$(rn_wire "$line")" ;;
      CHK\|*|"VERIFY RESULT"*|backup:*|restore:*) printf 'script|%s\n' "$(rn_oneline "$line")" ;;
    esac
  done <"$f"
}

# rn_script_run <label> <logfile> <env-array-name> <script> [args...] : run a deploy/backup script with the given BACKUP_*/RESTORE_* environment
rn_script_run() {
  local label="$1" log="$2" arrname="$3" script="$4"; shift 4
  local -n _envs="$arrname"
  local t0 rc
  t0="$(rn_now)"
  set +e
  env "${_envs[@]}" bash "${RN_BACKUP_DIR}/${script}" "$@" >"$log" 2>&1
  rc=$?
  set -e
  printf 'TIMING|drill_%s_seconds|%s\n' "$label" "$(rn_elapsed "$t0")"
  return "$rc"
}

# rn_relay_chk <log> <prefix> : every CHK|name|PASS|detail line of verify.sh as a RESULT (INFO when the caller says the comparison is informational)
rn_relay_chk() {
  local f="$1" prefix="$2" line name st detail
  while IFS= read -r line; do
    case "$line" in
      CHK\|*)
        IFS='|' read -r _ name st detail <<<"$line"
        case "$st" in PASS|FAIL) ;; *) st=FAIL ;; esac
        if [ -n "${RN_CHK_SKIP_RE:-}" ] && [[ "$name" =~ $RN_CHK_SKIP_RE ]]; then continue; fi
        [ "${RN_CHK_INFO_ONLY:-0}" = 0 ] || st=INFO
        rn_result "${prefix}_${name}" "$st" "$detail" ;;
    esac
  done <"$f"
}

#!/usr/bin/env bash
# Shared local-only database guard and credential handling for deploy/drill/backup-restore-drill.sh and
# deploy/loadtest/local-smoke.sh. SOURCED, not executed. The caller defines `die` first and requires
# TEST_ADMIN_DATABASE_URL and TEST_DATABASE_URL to be set. Afterwards: $WORK (0700 temp dir), $PGPASSFILE
# (0600, the only place the credentials live: never in process arguments), url_* helpers, and
# $ADMIN_URL (credential-free). Nothing here creates or alters a role or password.

# ---- environment hygiene and the local-host guard ----------------------------------------------
# libpq can be redirected by environment variables and by URL query parameters (host=, hostaddr=,
# service=, passfile=, ...), so a hostname check on the URL alone is bypassable. Defence in depth:
# (1) clear every redirecting environment variable, (2) refuse URLs that carry redirecting query keys
# or several hosts, (3) after connecting, ask the SERVER where it is and refuse anything but loopback
# or a unix socket (inet_server_addr() IS NULL).
unset PGHOST PGHOSTADDR PGPORT PGSERVICE PGSERVICEFILE PGSYSCONFDIR PGOPTIONS PGTARGETSESSIONATTRS PGDATABASE PGUSER PGPASSWORD
reject_redirecting_url() { # name url
  local name="$1" u="$2" q="" key rest authority hostpart
  case "$u" in postgres://*|postgresql://*) ;; *) die "$name must be a postgres:// URL" ;; esac
  rest="${u#*://}"; authority="${rest%%/*}"; hostpart="${authority##*@}"
  case "$hostpart" in *,*) die "$name: multiple hosts are refused" ;; esac
  case "$u" in *\?*) q="${u#*\?}" ;; esac
  local IFS='&'
  for key in $q; do
    key="${key%%=*}"
    case "$(printf '%s' "$key" | tr 'A-Z' 'a-z')" in
      host|hostaddr|service|servicefile|passfile|port|options|target_session_attrs|load_balance_hosts|sslrootcert|sslcert|sslkey|krbsrvname|gsslib|channel_binding)
        die "$name: URL query key '$key' is refused (it can redirect the connection); only sslmode is accepted" ;;
    esac
  done
}
url_host() { # postgres://user:pass@host:port/db?x -> host (IPv6 literals in brackets supported)
  local rest="${1#*://}" authority hostpart
  authority="${rest%%/*}"; hostpart="${authority##*@}"
  case "$hostpart" in
    \[*) hostpart="${hostpart#\[}"; printf '%s' "${hostpart%%\]*}" ;;
    *) printf '%s' "${hostpart%%:*}" ;;
  esac
}
url_port() {
  local rest="${1#*://}" authority hostpart
  authority="${rest%%/*}"; hostpart="${authority##*@}"
  case "$hostpart" in
    \[*\]:*) printf '%s' "${hostpart##*\]:}" ;;
    \[*) printf '5432' ;;
    *:*) printf '%s' "${hostpart##*:}" ;;
    *) printf '5432' ;;
  esac
}
url_user() { local rest="${1#*://}"; local authority="${rest%%/*}"; case "$authority" in *@*) local ui="${authority%@*}"; printf '%s' "${ui%%:*}" ;; esac; }
url_secret() { # the credential part of the URL, percent-decoded (never printed, never put in argv)
  local rest="${1#*://}"; local authority="${rest%%/*}" ui
  case "$authority" in *@*) ui="${authority%@*}" ;; *) return 0 ;; esac
  case "$ui" in *:*) local enc="${ui#*:}"; printf '%b' "${enc//%/\\x}" ;; esac
}
url_db() { local rest="${1#*://}" path; path="${rest#*/}"; printf '%s' "${path%%\?*}"; }
url_with_db() { # base-url new-db -> same authority/query, different database name, CREDENTIAL REMOVED
  local base="$1" db="$2" rest query="" authority hostpart user
  rest="${base#*://}"; authority="${rest%%/*}"; hostpart="${authority##*@}"; user="$(url_user "$base")"
  case "$base" in *\?*) query="?${base#*\?}" ;; esac
  if [ -n "$user" ]; then printf '%s://%s@%s/%s%s' "${base%%://*}" "$user" "$hostpart" "$db" "$query"
  else printf '%s://%s/%s%s' "${base%%://*}" "$hostpart" "$db" "$query"; fi
}
for pair in "TEST_ADMIN_DATABASE_URL:$TEST_ADMIN_DATABASE_URL" "TEST_DATABASE_URL:$TEST_DATABASE_URL"; do
  reject_redirecting_url "${pair%%:*}" "${pair#*:}"
  case "$(url_host "${pair#*:}")" in
    127.0.0.1|localhost|::1) ;;
    *) die "refusing a non-local database host (this drill is local-only; AWS is not authorised)" ;;
  esac
done

# Work directory first (0700) so credentials can live in a 0600 PGPASSFILE instead of process arguments.
umask 077
WORK="$(mktemp -d "${DRILL_WORKDIR:-${TMPDIR:-/tmp}}/${WORK_PREFIX:-drill-bk}.XXXXXX")"
chmod 700 "$WORK"
trap 'rm -rf "$WORK"' EXIT   # replaced by the full cleanup trap below once databases can exist
PGPASSFILE="${WORK}/pgpass"
export PGPASSFILE
: >"$PGPASSFILE"; chmod 600 "$PGPASSFILE"
add_pass() { # url -> one pgpass line for its host/port/user (any database)
  local u="$1" sec
  sec="$(url_secret "$u")"
  [ -n "$sec" ] || return 0
  sec="${sec//\\/\\\\}"; sec="${sec//:/\\:}"
  printf '%s:%s:*:%s:%s\n' "$(url_host "$u")" "$(url_port "$u")" "$(url_user "$u")" "$sec" >>"$PGPASSFILE"
}
add_pass "$TEST_ADMIN_DATABASE_URL"
add_pass "$TEST_DATABASE_URL"


ADMIN_URL="$(url_with_db "$TEST_ADMIN_DATABASE_URL" "$(url_db "$TEST_ADMIN_DATABASE_URL")")"
adm() { psql -X -q -At -v ON_ERROR_STOP=1 "$ADMIN_URL" "$@"; }

# Server-side proof that the connection really reached this machine (see the guard comment above).
LOCAL_OK="$(adm -c "SELECT (inet_server_addr() IS NULL OR inet_server_addr() << '127.0.0.0/8'::inet OR inet_server_addr() = '::1'::inet)::text")" \
  || die "cannot connect with TEST_ADMIN_DATABASE_URL"
[ "$LOCAL_OK" = "true" ] || die "the database server is not on this machine (inet_server_addr is not loopback or a unix socket): refusing"

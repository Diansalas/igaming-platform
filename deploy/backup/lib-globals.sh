#!/usr/bin/env bash
# Allow-list validation of the role statements restore.sh replays from a backup's globals.sql (--apply-globals).
# SOURCED by restore.sh. The checksum manifest is unsigned, so a party who can replace the backup directory could also
# replace globals.sql; replaying it blindly as a role-creating restore user would be a privilege-escalation path
# (e.g. `ALTER ROLE igaming WITH SUPERUSER`). Only the exact, expected shapes pass; anything else aborts the restore
# before any role or database is created.
#
#   bk_validate_role_stmts <role> <file>   returns 0 if every non-empty line of <file> is an allowed statement for <role>
#
# Allowed, for role r in {igaming, igaming_runtime, igaming_backup}:
#   CREATE ROLE r;
#   ALTER ROLE r WITH <attrs>;   attrs only from: NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION (all four REQUIRED),
#                                INHERIT NOINHERIT LOGIN NOLOGIN, CONNECTION LIMIT n, VALID UNTIL '<timestamp>', and
#                                BYPASSRLS if and only if r = igaming_backup (every other role must say NOBYPASSRLS)
#   ALTER ROLE r SET <p> TO '<v>';   p in the session-default allow-list below, v a plain token

# shellcheck shell=bash

BK_ROLE_SET_PARAMS="default_transaction_read_only statement_timeout idle_in_transaction_session_timeout lock_timeout log_statement"

bk_validate_role_stmts() {
  local r="$1" f="$2" stmt rest p tok re_chars re_set
  while IFS= read -r stmt; do
    [ -n "$stmt" ] || continue
    if [ "$stmt" = "CREATE ROLE ${r};" ]; then continue; fi
    case "$stmt" in
      "ALTER ROLE ${r} WITH "*";")
        rest="${stmt#"ALTER ROLE ${r} WITH "}"; rest="${rest%;}"
        # character allow-list first (no quotes except around the VALID UNTIL timestamp, no parentheses, no semicolons)
        re_chars="^[A-Za-z0-9 ':.+-]+\$"
        [[ "$rest" =~ $re_chars ]] || { echo "role ${r}: unexpected characters in: ${stmt}" >&2; return 1; }
        # attribute tokens
        local words=" ${rest} " need
        for need in NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION; do
          [[ "$words" == *" ${need} "* ]] || { echo "role ${r}: ${need} is required in: ${stmt}" >&2; return 1; }
        done
        if [ "$r" = igaming_backup ]; then
          [[ "$words" == *" BYPASSRLS "* ]] || { echo "role ${r}: BYPASSRLS expected for the backup role: ${stmt}" >&2; return 1; }
        else
          [[ "$words" == *" NOBYPASSRLS "* && "$words" != *" BYPASSRLS "* ]] || { echo "role ${r}: NOBYPASSRLS required: ${stmt}" >&2; return 1; }
        fi
        # nothing outside the known grammar: strip the known pieces, nothing may remain
        local residue="$words"
        for tok in NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION BYPASSRLS NOBYPASSRLS INHERIT NOINHERIT LOGIN NOLOGIN; do residue="${residue// ${tok} / }"; done
        residue="$(printf '%s' "$residue" | sed -E "s/ CONNECTION LIMIT -?[0-9]+ / /; s/ VALID UNTIL '[0-9][0-9: .+-]*' / /")"
        residue="${residue// /}"
        [ -z "$residue" ] || { echo "role ${r}: unrecognised attribute text '${residue}' in: ${stmt}" >&2; return 1; }
        ;;
      "ALTER ROLE ${r} SET "*" TO '"*"';")
        p="${stmt#"ALTER ROLE ${r} SET "}"; p="${p%% TO *}"
        [[ " ${BK_ROLE_SET_PARAMS} " == *" ${p} "* ]] || { echo "role ${r}: parameter '${p}' is not in the allow-list: ${stmt}" >&2; return 1; }
        re_set="^ALTER ROLE [a-z_]+ SET [a-z_]+ TO '[A-Za-z0-9._-]+';\$"
        [[ "$stmt" =~ $re_set ]] || { echo "role ${r}: unexpected value in: ${stmt}" >&2; return 1; }
        ;;
      *) echo "role ${r}: statement not allowed: ${stmt}" >&2; return 1 ;;
    esac
  done <"$f"
}

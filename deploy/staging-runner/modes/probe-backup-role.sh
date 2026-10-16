#!/usr/bin/env bash
# Mode probe-backup-role (STAGING-PREP-3, plan 5.2 / 5.3): the denied-probe pack of igaming_backup ALONE, re-runnable at any time after
# the role exists (also against a freshly restored copy of the primary). SOURCED.
#
# Parameters: EXPECT_HARDENING  applied (default) | refused | unknown   whether harden-backup-surface.sql took effect; with refused, an
#             abuse primitive that is not denied is a recorded FINDING (residual, ADR 0122 section 5b) instead of a FAIL.
# Connects as igaming_backup with an IAM authentication token minted by the task role (rds-db:connect on this instance only), over
# sslmode=verify-full with the RDS CA bundle. 1 attribute check + 15 write/escalation probes + 9 abuse-primitive probes + 3 actor-proof key probes (F1), each in its own
# connection inside BEGIN READ WRITE; ...; ROLLBACK. See lib-probes.sh for the verdict rules.

# shellcheck shell=bash
# shellcheck source=deploy/staging-runner/lib-probes.sh
. "${RN_DIR}/lib-probes.sh"

mode_probe_backup_role() {
  local hard
  hard="$(rn_param EXPECT_HARDENING '^(applied|refused|unknown)$' applied)"
  rn_need_tools psql
  rn_load_credentials
  rn_fetch_ca
  rn_backup_role_attributes
  rn_probe_pack "$hard"
}

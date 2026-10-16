#!/usr/bin/env bash
# Mode seed-world (STAGING-PREP-3 round 2, finding SA-1 of docs/plans/staging-acceptance-suite-record.md): create the SYNTHETIC world the
# staging acceptance suite (deploy/aws/acceptance) needs, through cmd/seed-staging-world, which uses the platform's own services:
#   * the synthetic ACTIVE tenant and brand (owner-provisioning path, ADR 0112, migration 0128) with operating-market rows (EnsureMarket),
#     and optionally a SECOND active tenant/brand (cross-tenant isolation checks);
#   * tenant staff for the first tenant: tenant_admin, finance x2, compliance, risk_manager, each with a bootstrap MFA enrolment token.
# Generated one-time passwords and the bootstrap tokens are written ONLY to the Secrets Manager secret igaming-staging/seed-world-passwords by this
# task's own task role (Get/PutSecretValue on that one ARN); they are never printed or logged. SOURCED. Runs in the staging-runner-seed-world task
# definition (master password + staff MFA key families + that task role).
#
# Parameters (non-secret; the first six and the three mandatory ones are required):
#   TENANT_SLUG BRAND_SLUG   slugs of the synthetic tenant and its brand;  COUNTRY  ISO-3166-1 alpha-2 (= MOCK_GEOLOCATION_COUNTRY when set)
#   SECOND_TENANT_SLUG SECOND_BRAND_SLUG   optional pair: a second ACTIVE tenant
#   AUTH_REF OPERATOR REASON   mandatory: authorisation reference, who, why (recorded in the audit rows)
#   ACTOR_ID   optional: staff id of the platform admin recorded as the actor (default: the sole active platform admin; it must be ACTIVE)
#   DRY_RUN    optional 0|1: run the read-only plan only (gates, what would be created); nothing is written anywhere, nothing is uploaded
# Gates (cmd/seed-staging-world, before anything is read or written): APP_ENV development or staging AND the database_environment anchor staging /
# development / test (no anchor row = production = refused: record it first, mode record-anchor), mandatory operator and reason, an ACTIVE
# platform_admin actor.
#
# TWO PHASES so a credential can never be lost (plan 6.5): (1) PLAN (read-only) generates a password for each missing account; the runner stores
# it in the secret BEFORE (2) APPLY creates anything; the bootstrap tokens (known only after the accounts exist) are merged into the secret
# afterwards. If the last upload fails the passwords are already stored; only the tokens are lost (reissue with seed-admin -issue-bootstrap).
# Idempotent: existing tenants, brands, market rows and accounts are left alone and their entries in the secret are kept.

# shellcheck shell=bash

RN_SW_SECRET_ID="igaming-staging/seed-world-passwords"

# rn_sw_get <outfile> : the current secret value (JSON) or {} when the secret has no value yet. Returns 1 on any other failure.
rn_sw_get() {
  local out="$1"
  if [ "$RN_LOCAL" = 1 ]; then
    [ -n "${SEED_WORLD_LOCAL_STORE:-}" ] || rn_die "internal: the local harness needs SEED_WORLD_LOCAL_STORE"
    if [ -s "$SEED_WORLD_LOCAL_STORE" ]; then cp "$SEED_WORLD_LOCAL_STORE" "$out"; else printf '{}' >"$out"; fi
    chmod 600 "$out"
    return 0
  fi
  : >"$out"; chmod 600 "$out"
  if aws secretsmanager get-secret-value --region "${AWS_REGION:-eu-central-1}" --secret-id "$RN_SW_SECRET_ID" --query SecretString --output text >"$out" 2>"${RN_SCRATCH}/sw-get.err"; then
    [ -s "$out" ] || printf '{}' >"$out"
    [ "$(cat "$out")" != None ] || printf '{}' >"$out"
    return 0
  fi
  if grep -qi "can.t find the specified secret value" "${RN_SCRATCH}/sw-get.err"; then printf '{}' >"$out"; return 0; fi
  return 1
}

# rn_sw_put <file> : store the JSON as the new secret value (the file is passed as file://, never on a command line)
rn_sw_put() {
  local f="$1"
  if [ "$RN_LOCAL" = 1 ]; then
    cp "$f" "$SEED_WORLD_LOCAL_STORE" 2>"${RN_SCRATCH}/sw-put.err" || return 1
    chmod 600 "$SEED_WORLD_LOCAL_STORE" || return 1
    return 0
  fi
  aws secretsmanager put-secret-value --region "${AWS_REGION:-eu-central-1}" --secret-id "$RN_SW_SECRET_ID" --secret-string "file://${f}" >/dev/null 2>"${RN_SCRATCH}/sw-put.err"
}

# rn_sw_merge <base> <new> <out> : account entries of <new> replace those of <base> wholesale (a stale bootstrap token must not survive a new password)
rn_sw_merge() {
  jq -s '{tenant_slug: (.[1].tenant_slug // .[0].tenant_slug), brand_slug: (.[1].brand_slug // .[0].brand_slug), country: (.[1].country // .[0].country),
          actors: ((.[0].actors // {}) + (.[1].actors // {}))}' "$1" "$2" >"$3"
  chmod 600 "$3"
}

# rn_sw_run <phase> <credentials-in or ""> <credentials-out> : the Go command in a subshell with a clean environment; only the apply phase gets the MFA keys
rn_sw_run() {
  local phase="$1" cin="$2" cout="$3"
  local -a args=(-phase "$phase" -tenant-slug "$SW_TENANT" -brand-slug "$SW_BRAND" -country "$SW_COUNTRY" -authorization-reference "$SW_AUTHREF" -operator "$SW_OPERATOR" -reason "$SW_REASON" -credentials-out "$cout")
  [ -z "$cin" ] || args+=(-credentials-in "$cin")
  [ -z "$SW_TENANT2" ] || args+=(-second-tenant-slug "$SW_TENANT2" -second-brand-slug "$SW_BRAND2")
  [ -z "$SW_ACTOR" ] || args+=(-actor-id "$SW_ACTOR")
  (
    # The subshell is a fork of this shell: its environment is the boot.sh allow-list (no secret: the secrets live in non-exported RN_S_* variables).
    # export is a builtin, so a key never appears in any argv.
    export DATABASE_URL PGPASSFILE="$RN_PGPASS"
    DATABASE_URL="$(rn_migrate_url owner)"
    if [ "$phase" = apply ]; then
      export STAFF_MFA_FACTOR_KEYS="$RN_S_MFA_FACTOR_KEYS" STAFF_MFA_MAC_KEYS="$RN_S_MFA_MAC_KEYS"
    fi
    exec "${RN_BIN}/seed-staging-world" "${args[@]}"
  ) >"${RN_SCRATCH}/sw-${phase}.out" 2>"${RN_SCRATCH}/sw-${phase}.err"
}

rn_sw_relay() { # <phase> : the status lines only (SEEDWORLD|key|value); the credentials file is never read here
  local line
  while IFS= read -r line; do
    case "$line" in SEEDWORLD\|*) printf '%s\n' "$(rn_wire "$line")" ;; esac
  done <"${RN_SCRATCH}/sw-${1}.out"
}

mode_seed_world() {
  rn_need_tools psql jq cp
  SW_TENANT="$(rn_param TENANT_SLUG '^[a-z0-9][a-z0-9-]{0,62}$')"
  SW_BRAND="$(rn_param BRAND_SLUG '^[a-z0-9][a-z0-9-]{0,62}$')"
  SW_TENANT2="$(rn_param SECOND_TENANT_SLUG '^([a-z0-9][a-z0-9-]{0,62})?$' "")"
  SW_BRAND2="$(rn_param SECOND_BRAND_SLUG '^([a-z0-9][a-z0-9-]{0,62})?$' "")"
  SW_COUNTRY="$(rn_param COUNTRY '^[A-Z]{2}$')"
  SW_AUTHREF="$(rn_param AUTH_REF "$RN_RE_AUTHREF")"
  SW_OPERATOR="$(rn_param OPERATOR "$RN_RE_OPERATOR")"
  SW_REASON="$(rn_param REASON "$RN_RE_REASON")"
  SW_ACTOR="$(rn_param ACTOR_ID '^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})?$' "")"
  local dry
  dry="$(rn_param DRY_RUN '^[01]$' 0)"
  { [ -z "$SW_TENANT2" ] && [ -z "$SW_BRAND2" ]; } || { [ -n "$SW_TENANT2" ] && [ -n "$SW_BRAND2" ]; } || rn_die "SECOND_TENANT_SLUG and SECOND_BRAND_SLUG go together"
  [ -x "${RN_BIN}/seed-staging-world" ] || rn_die "the seed-staging-world binary is not in the image"
  rn_load_credentials
  if [ "$RN_LOCAL" = 0 ]; then
    rn_need_tools aws
    [ -z "${SEED_WORLD_LOCAL_STORE:-}" ] || rn_die "SEED_WORLD_LOCAL_STORE is only for the local test harness"
  fi
  if [ "$dry" = 0 ]; then
    [ -n "${RN_S_MFA_FACTOR_KEYS:-}" ] && [ -n "${RN_S_MFA_MAC_KEYS:-}" ] || rn_die "the staff MFA key families are not injected: this mode runs only in the staging-runner-seed-world task definition"
    rn_register_secret "$RN_S_MFA_FACTOR_KEYS"; rn_register_secret "$RN_S_MFA_MAC_KEYS"
  fi
  rn_fetch_ca

  local existing="${RN_SHM}/sw-existing.json" plan="${RN_SHM}/sw-plan.json" stored="${RN_SHM}/sw-stored.json" applied="${RN_SHM}/sw-applied.json" final="${RN_SHM}/sw-final.json"
  local n

  # ---- 0. the current secret value (so a re-run never overwrites an account's stored credentials)
  if [ "$dry" = 0 ]; then
    if ! rn_sw_get "$existing"; then
      rn_stop "cannot read the secret ${RN_SW_SECRET_ID} with the task role: $(rn_first_line "${RN_SCRATCH}/sw-get.err") (Terraform not applied yet, or the seed-world task role / boundary is missing); nothing was written"
    fi
    if ! jq -e 'type == "object"' "$existing" >/dev/null 2>&1; then rn_stop "the secret ${RN_SW_SECRET_ID} does not hold a JSON object; nothing was written"; fi
    if jq -e --arg t "$SW_TENANT" '(.tenant_slug // "") != "" and .tenant_slug != $t and ((.actors // {}) | length) > 0' "$existing" >/dev/null 2>&1; then
      rn_stop "the secret ${RN_SW_SECRET_ID} already holds the accounts of tenant $(jq -r '.tenant_slug' "$existing" | cut -c1-63): refusing to mix two worlds in one secret; nothing was written"
    fi
  else
    printf '{}' >"$existing"
  fi

  # ---- 1. PLAN (read-only): gates and which accounts are missing
  if ! rn_sw_run plan "" "$plan"; then
    rn_sw_relay plan
    rn_result seed_world_plan FAIL "the plan phase failed: $(rn_first_line "${RN_SCRATCH}/sw-plan.err")"
    return 0
  fi
  rn_sw_relay plan
  n="$(jq -r '(.actors // {}) | length' "$plan")"
  rn_result seed_world_plan PASS "gates passed (APP_ENV and the environment anchor); ${n} staff account(s) to create"
  if [ "$dry" = 1 ]; then
    rn_result seed_world PASS "dry run: nothing written, nothing uploaded"
    return 0
  fi

  # ---- 2. store the planned passwords BEFORE any account exists
  rn_sw_merge "$existing" "$plan" "$stored"
  if [ "$n" -gt 0 ]; then
    if ! rn_sw_put "$stored"; then
      rn_stop "cannot store the planned credentials in ${RN_SW_SECRET_ID}: $(rn_first_line "${RN_SCRATCH}/sw-put.err") (the task role needs secretsmanager:PutSecretValue on it and the permissions boundary must allow it); NOTHING was created"
    fi
    rn_result seed_world_credentials_planned PASS "${n} generated one-time password(s) stored in ${RN_SW_SECRET_ID} before any account is created (values not printed)"
  fi

  # ---- 3. APPLY
  local apply_rc=0
  rn_sw_run apply "$plan" "$applied" || apply_rc=$?
  rn_sw_relay apply

  # ---- 4. store the bootstrap tokens (and whatever was created before a failure)
  if [ -s "$applied" ] && jq -e '(.actors // {}) | length > 0' "$applied" >/dev/null 2>&1; then
    rn_sw_merge "$stored" "$applied" "$final"
    if rn_sw_put "$final"; then
      rn_result seed_world_credentials_stored PASS "$(jq -r '(.actors // {}) | length' "$applied") account(s) with bootstrap enrolment tokens merged into ${RN_SW_SECRET_ID} (values not printed)"
    else
      rn_result seed_world_credentials_stored FAIL "cannot store the bootstrap tokens: $(rn_first_line "${RN_SCRATCH}/sw-put.err"). The passwords are already in ${RN_SW_SECRET_ID}; reissue the lost tokens with seed-admin -issue-bootstrap"
    fi
  fi
  if [ "$apply_rc" -ne 0 ]; then
    rn_result seed_world FAIL "the apply phase failed (exit ${apply_rc}): $(rn_first_line "${RN_SCRATCH}/sw-apply.err")"
    return 0
  fi
  rn_result seed_world PASS "synthetic tenant ${SW_TENANT}/${SW_BRAND}${SW_TENANT2:+ and ${SW_TENANT2}/${SW_BRAND2}} active with operating-market rows; staff accounts present; credentials only in ${RN_SW_SECRET_ID}"
}

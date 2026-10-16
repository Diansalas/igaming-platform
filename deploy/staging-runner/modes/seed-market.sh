#!/usr/bin/env bash
# Mode seed-market (STAGING-PREP-3, owner action C of aws-staging-deploy-plan.md): write the operating-market rows (licence-country
# ceiling version + tenant opt-in policy version) for a SYNTHETIC tenant / brand / country through cmd/seed-market, which calls
# operatingmarket.CreateLicenceCountryCeilingVersion and CreateOperatingCountryPolicyVersion (the reviewed library paths; the audit rows
# are written by the library in the same transactions). SOURCED.
#
# Parameters (non-secret container-environment overrides; all required unless marked):
#   TENANT_SLUG   slug of the synthetic tenant (it must already exist and carry a licence)
#   BRAND_SLUG    slug of that tenant's synthetic brand
#   COUNTRY       ISO-3166-1 alpha-2; must equal MOCK_GEOLOCATION_COUNTRY (terraform variable mock_geolocation_country) when that is set
#   AUTH_REF      the authorisation reference (a ticket id) recorded on both rows (mandatory: the library refuses an enabling write without one)
#   OPERATOR      who runs it (recorded); REASON  why (recorded as the audit reason code)
#   ACTOR_ID      optional: staff id of the platform admin recorded as the actor (default: the sole active platform admin)
#   DRY_RUN       optional 0|1 (default 0)
# MKT-DUAL-1 (four-eyes on market enablement) is still OPEN: two humans review these parameters by process and record both in the ticket.
# The tool is idempotent (an identical open version is left alone), refuses to re-enable a closed country, and refuses APP_ENV=production.

# shellcheck shell=bash

mode_seed_market() {
  local tenant brand country authref operator reason actor dry out rc
  tenant="$(rn_param TENANT_SLUG '^[a-z0-9][a-z0-9-]{0,62}$')"
  brand="$(rn_param BRAND_SLUG '^[a-z0-9][a-z0-9-]{0,62}$')"
  country="$(rn_param COUNTRY '^[A-Z]{2}$')"
  authref="$(rn_param AUTH_REF "$RN_RE_AUTHREF")"
  operator="$(rn_param OPERATOR "$RN_RE_OPERATOR")"
  reason="$(rn_param REASON "$RN_RE_REASON")"
  actor="$(rn_param ACTOR_ID '^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})?$' "")"
  dry="$(rn_param DRY_RUN '^[01]$' 0)"
  [ -x "${RN_BIN}/seed-market" ] || rn_die "the seed-market binary is not in the image"
  rn_load_credentials
  rn_fetch_ca

  local -a args=(-tenant-slug "$tenant" -brand-slug "$brand" -country "$country" -authorization-reference "$authref" -operator "$operator" -reason "$reason")
  [ -z "$actor" ] || args+=(-actor-id "$actor")
  [ "$dry" = 0 ] || args+=(-dry-run)
  out="${RN_SCRATCH}/seed-market.out"
  set +e
  env -i PATH="$PATH" HOME="${HOME:-/tmp}" APP_ENV="$APP_ENV" DATABASE_URL="$(rn_migrate_url owner)" PGPASSFILE="$RN_PGPASS" \
    ${MOCK_GEOLOCATION_COUNTRY:+MOCK_GEOLOCATION_COUNTRY="$MOCK_GEOLOCATION_COUNTRY"} \
    "${RN_BIN}/seed-market" "${args[@]}" >"$out" 2>"${RN_SCRATCH}/seed-market.err"
  rc=$?
  set -e
  local line
  while IFS= read -r line; do
    case "$line" in SEEDMARKET\|*) printf '%s\n' "$(rn_wire "$line")" ;; esac
  done <"$out"
  if [ "$rc" -eq 0 ]; then
    if [ "$dry" = 1 ]; then rn_result seed_market PASS "dry run: nothing written ($(grep -c . "$out") lines)"
    else rn_result seed_market PASS "ceiling and tenant policy present for ${tenant}/${brand} in ${country} (authorisation reference ${authref}); registration, deposit, casino and sportsbook wagering resolve as permitted"; fi
  else
    rn_result seed_market FAIL "seed-market exit ${rc}: $(rn_first_line "${RN_SCRATCH}/seed-market.err")"
  fi
}

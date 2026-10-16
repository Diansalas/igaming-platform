#!/usr/bin/env bash
# Offline test (no AWS, no Terraform, no Docker) of `deploy.sh runner`'s parameter handling (STAGING-PREP-3): only the allowed, plain,
# non-secret parameters of a mode reach the container override; anything that could carry a credential, an unknown name, a bad value or a
# bad mode is refused, and the override always carries the mode as the ONE command argument. Sources deploy.sh (which defines its functions
# and returns when sourced). Run by run-static-checks.sh.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/aws/scripts/deploy.sh
. "${HERE}/../scripts/deploy.sh"
set +e   # the assertions below check exit statuses themselves

fails=0
ok() { echo "ok   $*"; }
bad() { echo "FAIL $*" >&2; fails=$((fails + 1)); }

# expect_refused <description> <mode> [params...]  (die() exits the subshell with status 1)
expect_refused() {
  local d="$1"; shift
  if ( runner_overrides "$@" >/dev/null 2>&1 ); then bad "${d}: was accepted"; else ok "${d}: refused"; fi
}

out="$(runner_overrides verify-db EXPECT_BACKUP_ROLE=absent GRANT_LISTING=1)"
[[ "$(jq -c '.containerOverrides[0].command' <<<"${out}")" == '["verify-db"]' ]] && ok "the mode is the single command argument" || bad "command is not [mode]"
[[ "$(jq -r '.containerOverrides[0].name' <<<"${out}")" == "staging-runner" ]] && ok "targets the staging-runner container" || bad "wrong container name"
[[ "$(jq -r '.containerOverrides[0].environment | map(.name) | join(",")' <<<"${out}")" == "EXPECT_BACKUP_ROLE,GRANT_LISTING" ]] && ok "allowed parameters pass through" || bad "parameters lost"

out="$(runner_overrides seed-actor-proof)"
[[ "$(jq -c '.containerOverrides[0].environment' <<<"${out}")" == "[]" ]] && ok "seed-actor-proof takes no parameters (the key comes from Secrets Manager)" || bad "seed-actor-proof environment not empty"
expect_refused "seed-actor-proof with a parameter" seed-actor-proof ACTOR_PROOF_KEYS=x
expect_refused "a credential-looking name" seed-market DB_MASTER_PASSWORD=x
expect_refused "a token-looking name" verify-db API_TOKEN=x
expect_refused "an unknown name for the mode" verify-db TENANT_SLUG=demo
expect_refused "a parameter of another mode" backup-restore-drill PITR_ENDPOINT=x
expect_refused "a lower-case name" verify-db expect_backup_role=absent
expect_refused "a value with a backtick" seed-market TENANT_SLUG='`id`' BRAND_SLUG=b COUNTRY=SV AUTH_REF=t OPERATOR=o REASON=r
expect_refused "a value with a pipe" seed-market TENANT_SLUG='a|b' BRAND_SLUG=b COUNTRY=SV AUTH_REF=t OPERATOR=o REASON=r
expect_refused "a value with a quote" seed-market REASON="it's"
expect_refused "a value with a newline" seed-market REASON=$'a\nb'
expect_refused "a value with a dollar sign" seed-market REASON='$(id)'
expect_refused "an unknown mode" no-such-mode
expect_refused "a missing equals sign" verify-db GRANT_LISTING
expect_refused "an oversized value" pitr-verify "EXPECTED_DIGESTS=$(printf 'A%.0s' $(seq 1 1501))"

out="$(runner_overrides pitr-verify "PITR_ENDPOINT=igaming-staging-db-pitr-abc1234.cabc123.eu-central-1.rds.amazonaws.com" "EXPECTED_DIGESTS=TABLE=0123456789abcdef;ROLE=0123456789abcdef" EXPECT_ANCHOR=unrecorded)"
[[ "$(jq -r '.containerOverrides[0].environment[1].value' <<<"${out}")" == "TABLE=0123456789abcdef;ROLE=0123456789abcdef" ]] && ok "digests with = and ; survive intact" || bad "digest value altered"

out="$(runner_overrides seed-market TENANT_SLUG=demo-casino BRAND_SLUG=demo-brand COUNTRY=SV "AUTH_REF=TICKET-42" OPERATOR=ops-alice "REASON=staging bootstrap")"
[[ "$(jq -r '.containerOverrides[0].environment[] | select(.name=="REASON") | .value' <<<"${out}")" == "staging bootstrap" ]] && ok "a value with a space survives" || bad "space lost"

# STAGING-PREP-3 round 2: the writing modes need who / why / which authorisation; the new modes accept only their own names
expect_refused "seed-market without OPERATOR" seed-market TENANT_SLUG=a BRAND_SLUG=b COUNTRY=SV AUTH_REF=T-1 "REASON=x y"
expect_refused "seed-world without REASON" seed-world TENANT_SLUG=a BRAND_SLUG=b COUNTRY=SV AUTH_REF=T-1 OPERATOR=ops
expect_refused "seed-world without AUTH_REF" seed-world TENANT_SLUG=a BRAND_SLUG=b COUNTRY=SV OPERATOR=ops "REASON=x y"
expect_refused "record-anchor without OPERATOR" record-anchor ANCHOR_VALUE=staging AUTH_REF=T-1 REASON=x
expect_refused "record-anchor with a parameter of another mode" record-anchor ANCHOR_VALUE=staging AUTH_REF=T-1 OPERATOR=o REASON=r TENANT_SLUG=a
expect_refused "cleanup with a parameter of another mode" cleanup DRY_RUN=1
expect_refused "seed-world with a credential-looking name" seed-world TENANT_SLUG=a BRAND_SLUG=b COUNTRY=SV AUTH_REF=T-1 OPERATOR=o REASON=r PASSWORD_FILE=x
out="$(runner_overrides seed-world TENANT_SLUG=acc-one BRAND_SLUG=acc-brand SECOND_TENANT_SLUG=acc-two SECOND_BRAND_SLUG=acc-two-brand COUNTRY=SV AUTH_REF=TICKET-43 OPERATOR=ops-alice "REASON=staging acceptance world")"
[[ "$(jq -c '.containerOverrides[0].command' <<<"${out}")" == '["seed-world"]' ]] && ok "seed-world is accepted with its parameters" || bad "seed-world rejected"
out="$(runner_overrides record-anchor ANCHOR_VALUE=staging AUTH_REF=TICKET-44 OPERATOR=ops-alice "REASON=plan 6.2 anchor")"
[[ "$(jq -r '.containerOverrides[0].environment[] | select(.name=="REASON") | .value' <<<"${out}")" == "plan 6.2 anchor" ]] && ok "record-anchor is accepted (a reason with spaces and a dot)" || bad "record-anchor rejected"
out="$(runner_overrides verify-db REQUIRE_ANCHOR=1)"
[[ "$(jq -r '.containerOverrides[0].environment[0].name' <<<"${out}")" == "REQUIRE_ANCHOR" ]] && ok "verify-db accepts REQUIRE_ANCHOR" || bad "REQUIRE_ANCHOR rejected"
out="$(runner_overrides cleanup)"
[[ "$(jq -c '.containerOverrides[0].command' <<<"${out}")" == '["cleanup"]' ]] && ok "cleanup takes no required parameter" || bad "cleanup rejected"

if [[ ${fails} -ne 0 ]]; then echo "${fails} runner override test(s) failed" >&2; exit 1; fi
echo "runner override tests passed"

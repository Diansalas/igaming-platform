#!/usr/bin/env bash
# Offline test (no AWS) of deploy/aws/acceptance/fetch-seed-world.sh: modes, content and the refusals.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/../acceptance/fetch-seed-world.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
fails=0
ok() { echo "ok   $*"; }
bad() { echo "FAIL $*" >&2; fails=$((fails + 1)); }

cat >"${T}/in.json" <<'EOF'
{"tenant_slug":"acc-one","brand_slug":"acc-one-brand","country":"SV","actors":{
 "FINANCE1":{"email":"finance1@acc-one.acceptance.invalid","password":"Sw1-abcdefghijklmnopqrstuv","bootstrap_token":"tok-1234567890"},
 "COMPLIANCE":{"email":"compliance@acc-one.acceptance.invalid","password":"Sw1-zzzzzzzzzzzzzzzzzzzzzz"}}}
EOF

out="$(SEED_WORLD_JSON_FILE="${T}/in.json" bash "$SCRIPT" "${T}/out" 2>"${T}/err")" && ok "runs" || bad "failed: $(cat "${T}/err")"
[ "$(stat -c %a "${T}/out")" = 700 ] && ok "output directory is 0700" || bad "directory mode"
for f in FINANCE1.pw FINANCE1.token COMPLIANCE.pw suite.env; do
  [ "$(stat -c %a "${T}/out/${f}")" = 600 ] && ok "${f} is 0600" || bad "${f} mode"
done
[ ! -e "${T}/out/COMPLIANCE.token" ] && ok "no token file for an account without a token" || bad "empty token file kept"
[ "$(cat "${T}/out/FINANCE1.pw")" = "Sw1-abcdefghijklmnopqrstuv" ] && ok "password file content (no trailing newline)" || bad "password content"
grep -q 'export FINANCE1_PASSWORD_FILE=' "${T}/out/suite.env" && grep -q 'export TENANT=acc-one' "${T}/out/suite.env" && ok "suite.env names the files and the slugs" || bad "suite.env"
grep -q 'Sw1-\|tok-12' "${T}/out/suite.env" && bad "suite.env contains a secret" || ok "suite.env holds no secret"
case "$out" in *Sw1-*|*tok-12*) bad "output printed a secret" ;; *) ok "nothing secret on stdout" ;; esac
grep -q 'Sw1-\|tok-12' "${T}/err" && bad "stderr printed a secret" || ok "nothing secret on stderr"
grep -q 'TENANT_ADMIN is not in the secret' "${T}/err" && ok "a missing account is noted" || bad "missing account not noted"
printf 'existing-totp-secret' >"${T}/out/FINANCE1.totp"
SEED_WORLD_JSON_FILE="${T}/in.json" bash "$SCRIPT" "${T}/out" >/dev/null 2>&1
[ "$(cat "${T}/out/FINANCE1.totp")" = existing-totp-secret ] && ok "an existing TOTP secret file is never touched" || bad "totp file overwritten"
if bash "$SCRIPT" relative/dir >/dev/null 2>&1; then bad "relative path accepted"; else ok "relative output path refused"; fi
if bash "$SCRIPT" "${HERE}/../../../creds" >/dev/null 2>&1; then bad "a path inside the repository was accepted"; else ok "a path inside the repository is refused"; fi
printf '{"nope":1}' >"${T}/bad.json"
if SEED_WORLD_JSON_FILE="${T}/bad.json" bash "$SCRIPT" "${T}/out2" >/dev/null 2>&1; then bad "a malformed document was accepted"; else ok "a malformed document is refused"; fi

if [ "$fails" -ne 0 ]; then echo "${fails} fetch-seed-world test(s) failed" >&2; exit 1; fi
echo "fetch-seed-world tests passed"

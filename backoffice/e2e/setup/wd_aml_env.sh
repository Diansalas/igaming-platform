# lic22: extra API environment for the withdrawal-restriction / AML-screening specs (09, 10). Sourced by run.sh with `set -a`.
# Synthetic, run-time only (never committed): MOCK components on (non-production APP_ENV), a MOCK geolocation country that
# matches the seeded world's operating-market rows, and the two payout-instrument key families the withdrawal path needs.
# Everything here is additive: specs 01-06 do not use any of it.
_b64() { node -e 'process.stdout.write(require("crypto").randomBytes(32).toString("base64"))'; }
export TEST_SUPPORT_ENDPOINTS_ENABLED=true
export MOCK_GEOLOCATION_COUNTRY=MT
export PAYOUT_INSTRUMENT_KEYS="e2epm1:$(_b64)" PAYOUT_INSTRUMENT_ACTIVE_KID=e2epm1
export PAYOUT_INSTRUMENT_FP_KEYS="e2epf1:$(_b64)" PAYOUT_INSTRUMENT_FP_ACTIVE_KID=e2epf1
# the table-owner URL is already exported by run.sh; the RUNTIME-role URL is needed by spec 10 (below-the-application-layer attempts)
export E2E_RUNTIME_DATABASE_URL="${RUNTIME_URL:-$E2E_RUNTIME_DATABASE_URL}"
unset -f _b64

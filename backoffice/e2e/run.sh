#!/usr/bin/env bash
# Real-stack Back Office end-to-end run (lic13). See e2e/README.md.
#
# Required env (an ALREADY BUILT, FRESH, PRIVATE database; this script creates no roles/passwords/databases):
#   E2E_OWNER_DATABASE_URL    table-owner URL of the private DB   (default: $TEST_DATABASE_URL)
#   E2E_RUNTIME_DATABASE_URL  igaming_runtime URL, same DB        (default: $TEST_RUNTIME_DATABASE_URL)
# Optional: E2E_STACK_ONLY=1 (start the stack and wait, for debugging), E2E_RESULTS_DIR, E2E_FRONTEND_NODE_MODULES, E2E_PLAYWRIGHT_DIR, E2E_SPEC (playwright file filter), E2E_KEEP_RUN_DIR=1
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BO=$(cd "$HERE/.." && pwd)
REPO=$(cd "$BO/.." && pwd)
OWNER_URL=${E2E_OWNER_DATABASE_URL:-${TEST_DATABASE_URL:-}}
RUNTIME_URL=${E2E_RUNTIME_DATABASE_URL:-${TEST_RUNTIME_DATABASE_URL:-}}
[ -n "$OWNER_URL" ] && [ -n "$RUNTIME_URL" ] || { echo "e2e: set E2E_OWNER_DATABASE_URL and E2E_RUNTIME_DATABASE_URL (private DB)" >&2; exit 2; }

dbname() { local s=${1#*://*/}; echo "${s%%\?*}"; }
DB=$(dbname "$OWNER_URL")
[ "$DB" = "$(dbname "$RUNTIME_URL")" ] || { echo "e2e: owner and runtime URLs name different databases" >&2; exit 2; }
case "$DB" in
  igaming_platform_ci_local|igaming_platform_dev|igaming_platform_ci|*prod*|*staging*|"")
    echo "e2e: refusing database '$DB' (shared or non-private name)" >&2; exit 2;;
esac

RESULTS=${E2E_RESULTS_DIR:-$HERE/results}
mkdir -p "$RESULTS"
RUN_DIR=$(mktemp -d "${TMPDIR:-/tmp}/bo-e2e.XXXXXX"); chmod 700 "$RUN_DIR"
PIDS=(); MADE_NM=0; MADE_PWLINK=0
cleanup() {
  local rc=$?
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && wait "$p" 2>/dev/null || true; done
  [ "$MADE_NM" = 1 ] && rm -f "$BO/node_modules"
  [ "$MADE_PWLINK" = 1 ] && rm -f "$HERE/node_modules"
  if [ "${E2E_KEEP_RUN_DIR:-0}" != 1 ]; then rm -rf "$RUN_DIR"; else echo "e2e: run dir kept (contains secrets): $RUN_DIR"; fi
  exit $rc
}
trap cleanup EXIT INT TERM

freeport() { node -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})'; }
API_PORT=$(freeport); WEB_PORT=$(freeport)
export E2E_API_URL="http://127.0.0.1:$API_PORT" E2E_BASE_URL="http://127.0.0.1:$WEB_PORT"
export E2E_RUN_DIR="$RUN_DIR" E2E_WORLD_FILE="$RUN_DIR/world.json" E2E_OWNER_DATABASE_URL="$OWNER_URL" E2E_RESULTS_DIR="$RESULTS"

# frontend deps: reuse the main checkout's node_modules via symlink (disk)
if [ ! -e "$BO/node_modules" ]; then
  ln -s "${E2E_FRONTEND_NODE_MODULES:-/home/user/igaming-platform/backoffice/node_modules}" "$BO/node_modules"; MADE_NM=1
fi
PW_DIR=${E2E_PLAYWRIGHT_DIR:-$(npm root -g)/playwright}
[ -f "$PW_DIR/cli.js" ] || { echo "e2e: playwright not found at $PW_DIR" >&2; exit 2; }
if [ ! -e "$HERE/node_modules" ]; then ln -s "$(dirname "$PW_DIR")" "$HERE/node_modules"; MADE_PWLINK=1; fi
export PLAYWRIGHT_BROWSERS_PATH=${PLAYWRIGHT_BROWSERS_PATH:-/opt/pw-browsers}

echo "e2e: generating run-time secrets"
node "$HERE/setup/genkeys.mjs" > "$RUN_DIR/secrets.env"; chmod 600 "$RUN_DIR/secrets.env"
set -a; . "$RUN_DIR/secrets.env"; set +a

echo "e2e: building Go binaries"
mkdir -p "$RUN_DIR/bin"
( cd "$REPO" && for c in platform-api seed-admin seed-staging-world; do go build -o "$RUN_DIR/bin/$c" "./cmd/$c"; done )
# lic21 (additive): the test-only below-the-application-layer probe (runtime-role connection, real kyc/actorproof code).
( cd "$REPO" && go build -o "$RUN_DIR/bin/e2e-dbprobe" ./backoffice/e2e/tools/dbprobe )
export E2E_RUNTIME_DATABASE_URL="$RUNTIME_URL"

echo "e2e: building Back Office"
( cd "$BO" && ./node_modules/.bin/tsc -b && ./node_modules/.bin/vite build --outDir "$RUN_DIR/dist" --emptyOutDir >/dev/null )

echo "e2e: seeding"
node "$HERE/setup/seed.mjs" "$RUN_DIR/bin" "$E2E_WORLD_FILE"

echo "e2e: starting platform-api on $E2E_API_URL"
( exec env APP_ENV=development HTTP_ADDR="127.0.0.1:$API_PORT" DATABASE_URL="$RUNTIME_URL" AUTH_RATE_LIMIT_PER_MINUTE=1000 TEST_SUPPORT_ENDPOINTS_ENABLED=true MOCK_GEOLOCATION_COUNTRY=MT STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS="${E2E_STEPUP_SECONDS:-60}" \
    OTEL_EXPORTER=stdout "$RUN_DIR/bin/platform-api" >"$RUN_DIR/api.log" 2>&1 ) &
PIDS+=($!)
for i in $(seq 1 60); do
  curl -fsS "$E2E_API_URL/readyz" >/dev/null 2>&1 && break
  kill -0 "${PIDS[0]}" 2>/dev/null || { echo "e2e: platform-api exited:" >&2; tail -20 "$RUN_DIR/api.log" >&2; exit 1; }
  sleep 1
done
curl -fsS "$E2E_API_URL/readyz" >/dev/null || { echo "e2e: platform-api not ready" >&2; tail -20 "$RUN_DIR/api.log" >&2; exit 1; }

echo "e2e: serving Back Office on $E2E_BASE_URL (API proxied)"
( cd "$BO" && VITE_API_PROXY_TARGET="$E2E_API_URL" exec node ./node_modules/.bin/vite preview --outDir "$RUN_DIR/dist" --host 127.0.0.1 --port "$WEB_PORT" --strictPort >"$RUN_DIR/web.log" 2>&1 ) &
PIDS+=($!)
for i in $(seq 1 30); do curl -fsS "$E2E_BASE_URL/" >/dev/null 2>&1 && break; sleep 1; done

echo "e2e: enrolling staff factors over HTTP"
node "$HERE/setup/enrol.mjs" "$E2E_WORLD_FILE"

if [ "${E2E_STACK_ONLY:-0}" = 1 ]; then
  echo "e2e: stack up (E2E_STACK_ONLY=1). world file: $E2E_WORLD_FILE  api: $E2E_API_URL  web: $E2E_BASE_URL"
  echo "e2e: stop by killing this script's pid ($$)"
  while true; do sleep 5; done
fi
echo "e2e: running Playwright"
set +e
( cd "$HERE" && node "$PW_DIR/cli.js" test -c playwright.config.ts ${E2E_SPEC:+"$E2E_SPEC"} )
RC=$?
set -e
node "$HERE/setup/summarise.mjs" "$RESULTS/playwright.json" "$RESULTS" "$RC" || true
echo "e2e: summary in $RESULTS/summary.md and $RESULTS/summary.json (exit $RC)"
exit $RC

#!/usr/bin/env bash
# Runtime smoke test for deploy/docker/frontend.Dockerfile (b2c and
# backoffice). Builds the image, starts it exactly as ECS does (the image's
# own USER and CMD, no overrides), and asserts that nginx starts and keeps
# running as the non-root "nginx" user and serves the SPA on port 8080.
#
# Regression guard for the Stage 9.4 staging failure where both frontends
# exited with `open() "/run/nginx.pid" failed (13: Permission denied)`.
#
# Needs Docker. Creates no cloud resources. Run from anywhere in the repo:
#   deploy/docker/tests/frontend-image-smoke.sh b2c
#   deploy/docker/tests/frontend-image-smoke.sh backoffice
# SMOKE_IMAGE=<ref> skips the build and tests an already-built image.
set -euo pipefail

APP_DIR="${1:-}"
[[ "${APP_DIR}" == "b2c" || "${APP_DIR}" == "backoffice" ]] || { echo "usage: $0 <b2c|backoffice>" >&2; exit 2; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
TAG="${SMOKE_IMAGE:-igaming-frontend-smoke:${APP_DIR}}"
NAME="igaming-frontend-smoke-${APP_DIR}-$$"
PORT="${SMOKE_PORT:-18080}"

fail() { echo "FAIL (${APP_DIR}): $*" >&2; docker logs "${NAME}" >&2 2>/dev/null || true; exit 1; }
cleanup() { docker rm -f "${NAME}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

if [[ -z "${SMOKE_IMAGE:-}" ]]; then
  echo "== build ${APP_DIR} =="
  # VITE_BRAND_SLUG only matters in the browser (b2c's brand.ts); any value
  # lets the bundle build. Harmless for backoffice, which never reads it.
  docker build -f "${REPO_ROOT}/deploy/docker/frontend.Dockerfile" \
    --build-arg "APP_DIR=${APP_DIR}" \
    --build-arg "VITE_API_BASE_URL=http://api.invalid" \
    --build-arg "VITE_BRAND_SLUG=demo-casino" \
    -t "${TAG}" "${REPO_ROOT}"
fi

echo "== static config checks =="
user="$(docker image inspect --format '{{.Config.User}}' "${TAG}")"
[[ -n "${user}" && "${user}" != "root" && "${user}" != "0" ]] || fail "image USER is '${user}', must be a non-root user"
docker run --rm "${TAG}" sh -c 'grep -qx "pid /tmp/nginx.pid;" /etc/nginx/nginx.conf' \
  || fail "nginx.conf pid directive is not /tmp/nginx.pid"
docker run --rm "${TAG}" nginx -t || fail "nginx -t failed as the non-root user"

echo "== start (image defaults: USER ${user}, CMD) =="
docker run -d --name "${NAME}" -p "127.0.0.1:${PORT}:8080" "${TAG}" >/dev/null

code=""
for _ in $(seq 1 30); do
  [[ "$(docker inspect --format '{{.State.Running}}' "${NAME}")" == "true" ]] \
    || fail "container exited (code $(docker inspect --format '{{.State.ExitCode}}' "${NAME}"))"
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/" || true)"
  [[ "${code}" == "200" ]] && break
  sleep 1
done
[[ "${code}" == "200" ]] || fail "GET / returned '${code}', expected 200"

deep="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/some/client/route")"
[[ "${deep}" == "200" ]] || fail "SPA fallback GET /some/client/route returned '${deep}', expected 200"

uid="$(docker exec "${NAME}" id -u)"
[[ "${uid}" != "0" ]] || fail "container runs as uid 0"
master_uid="$(docker exec "${NAME}" sh -c 'awk "/^Uid:/{print \$2}" /proc/$(cat /tmp/nginx.pid)/status')" \
  || fail "no nginx master pid in /tmp/nginx.pid"
[[ "${master_uid}" != "0" ]] || fail "nginx master runs as uid 0"

sleep 2
[[ "$(docker inspect --format '{{.State.Running}}' "${NAME}")" == "true" ]] || fail "container stopped after serving"
[[ "$(docker inspect --format '{{.RestartCount}}' "${NAME}")" == "0" ]] || fail "container restarted"

echo "PASS (${APP_DIR}): nginx running as uid ${uid} (master uid ${master_uid}), pid /tmp/nginx.pid, HTTP 200 on 8080 (/ and SPA fallback)"

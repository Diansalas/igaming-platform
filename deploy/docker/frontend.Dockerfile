# Generic, parameterized image for BOTH Vite/React SPAs in this repo
# (b2c/ and backoffice/) - they have identical build tooling (see both
# package.json's "build": "tsc -b && vite build" script and matching
# devDependency versions), so one Dockerfile driven by a build ARG avoids
# maintaining two near-duplicate files that would silently drift apart.
#
# Mechanism: a single ARG (APP_DIR) names which top-level app directory to
# build, and the build is run with the REPO ROOT as its context (not
# b2c/ or backoffice/ individually) so this one Dockerfile can reach
# either directory. This was chosen over Docker's `--build-context` named
# contexts because it needs no BuildKit-specific named-context wiring at
# the call site - it's a plain `-f` + `--build-arg` invocation that works
# the same way in a CI runner or a developer's shell without assuming a
# particular BuildKit frontend version is available. See
# deploy/docker/README.md for the exact build commands.
#
# VITE_* build ARGs below are Vite/React's build-time-only configuration
# (b2c/src/vite-env.d.ts, backoffice/src/vite-env.d.ts): Vite inlines
# `import.meta.env.VITE_*` references into the static JS bundle at
# `vite build` time by reading them out of `process.env` (see
# https://vite.dev/guide/env-and-mode - loadEnv() merges already-exported
# process.env values, which is exactly what setting them as ENV inside
# the builder stage below does, ahead of `npm run build`). There is no
# server process for a static SPA to read a runtime environment variable
# from later, so these MUST be supplied at `docker build` time via
# --build-arg, never expected to work as a container-runtime ENV on the
# final nginx image - the final stage below does not receive them at all.
# Confirmed by grep across both apps' src/ trees that the only
# `import.meta.env` reads are VITE_API_BASE_URL (both apps) and cosmetic
# VITE_BRAND_* values (b2c only, see b2c/src/config/brand.ts) - nothing
# secret-shaped is ever read this way, so baking these into a built,
# publicly-served static bundle is the intended and only mechanism, not
# an oversight.

# --- Builder ---
# Pinned to Node 22 to match this repo's own CI (.github/workflows/ci.yml
# "Frontend build & test" job's actions/setup-node "node-version: 22"),
# rather than guessing at an LTS - there is no .nvmrc/engines field in
# either package.json to defer to instead, so CI's own pinned version is
# the most authoritative source of truth available for "what Node version
# these apps are actually developed and tested against."
FROM node:22-alpine AS builder

ARG APP_DIR

# Build-time-only Vite config. Empty-string defaults are safe for
# backoffice (its own vite-env.d.ts declares no VITE_BRAND_* fields at
# all, so an unreferenced one is simply never read) and for b2c's purely
# cosmetic fields (DISPLAY_NAME/PRIMARY_COLOR/*_HOVER/DEFAULT_ASSET each
# fall back to a hardcoded default - b2c/src/config/brand.ts).
# VITE_BRAND_SLUG for b2c is the one exception that is NOT safe to leave
# empty in a real deploy: brand.ts's resolveBrandSlug() throws at
# app-startup (in the browser, not at `docker build` time) in a
# production build (`import.meta.env.PROD`) if it was never set, exactly
# to stop a real brand silently registering/logging in visitors against
# the wrong tenant - see brand.ts's own doc comment. Always pass a real
# VITE_BRAND_SLUG when building the b2c image for anything beyond a local
# smoke test.
ARG VITE_API_BASE_URL=""
ARG VITE_BRAND_SLUG=""
ARG VITE_BRAND_DISPLAY_NAME=""
ARG VITE_BRAND_PRIMARY_COLOR=""
ARG VITE_BRAND_PRIMARY_COLOR_HOVER=""
ARG VITE_BRAND_DEFAULT_ASSET=""

WORKDIR /src

# Copy only the manifest files first so `npm ci` is cached independently
# of application source changes.
COPY ${APP_DIR}/package.json ${APP_DIR}/package-lock.json ./
RUN npm ci

COPY ${APP_DIR}/ ./

# Exported as real environment variables for this RUN step only - this
# builder stage's layers are discarded after COPY --from below pulls out
# just the built dist/ directory, so none of this reaches the final image
# even as build history (unlike a runtime ENV, which would persist into
# `docker inspect` on the final image if set there instead).
RUN VITE_API_BASE_URL="$VITE_API_BASE_URL" \
    VITE_BRAND_SLUG="$VITE_BRAND_SLUG" \
    VITE_BRAND_DISPLAY_NAME="$VITE_BRAND_DISPLAY_NAME" \
    VITE_BRAND_PRIMARY_COLOR="$VITE_BRAND_PRIMARY_COLOR" \
    VITE_BRAND_PRIMARY_COLOR_HOVER="$VITE_BRAND_PRIMARY_COLOR_HOVER" \
    VITE_BRAND_DEFAULT_ASSET="$VITE_BRAND_DEFAULT_ASSET" \
    npm run build

# --- Final ---
# nginx:1.27-alpine (unlike the Go builder, this base isn't pinned by any
# manifest in this repo - 1.27 is nginx's current stable line at the time
# of writing; bump deliberately, not silently, via a floating tag).
FROM nginx:1.27-alpine

COPY --from=builder /src/dist /usr/share/nginx/html
COPY deploy/docker/nginx-spa.conf /etc/nginx/conf.d/default.conf

# Run as the image's existing unprivileged "nginx" user, not root -
# ECS/Fargate and most container platforms default to allowing a
# non-root process, and there is no reason a static file server needs
# root once the config below removes its only two reasons to want it
# (binding <1024, and the "user" directive's setuid from master to
# worker - both irrelevant here since we bind 8080 and run everything,
# master included, as "nginx" already). /tmp is used for the pid file
# instead of the base image's root-owned default because a non-root
# process cannot create files there, whereas /tmp is universally
# world-writable in the base image. The default moved from
# /var/run/nginx.pid to /run/nginx.pid in newer nginx images (1.27.5 uses
# /run), and a sed that silently matched nothing made both frontends exit
# at startup on ECS with `open() "/run/nginx.pid" failed (13: Permission
# denied)` - so the rewrite accepts either path and the build FAILS if
# the pid directive is not exactly /tmp/nginx.pid afterwards.
# Regression check: deploy/docker/tests/frontend-image-smoke.sh.
RUN sed -i 's#^pid[[:space:]]\+\(/var\)\?/run/nginx\.pid;#pid /tmp/nginx.pid;#' /etc/nginx/nginx.conf \
    && grep -qx 'pid /tmp/nginx.pid;' /etc/nginx/nginx.conf \
    && chown -R nginx:nginx /usr/share/nginx/html /var/cache/nginx /etc/nginx/conf.d
USER nginx

EXPOSE 8080

CMD ["nginx", "-g", "daemon off;"]

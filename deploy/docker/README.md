# `deploy/docker/` — Stage 9.3 staging image build

Three Dockerfiles, all consumed as one-off `docker build` invocations (no
docker-compose file for AWS in scope for this stage — see
`docs/architecture/38-deployment-architecture.md`; the only compose file
in this repo, `deploy/docker-compose.dev.yml`, remains dev-only Postgres).

**Not verified end-to-end.** This sandbox has no Docker daemon (`docker
info` fails with `dial unix /var/run/docker.sock: ... no such file or
directory`) — none of the three `docker build` commands below has
actually been run to completion here. What WAS done instead:

- Every file/binary/script path referenced in each Dockerfile was checked
  against the real repository (`cmd/platform-api`, `cmd/migrate`,
  `migrations/`, `go.mod`'s exact Go version, both apps' `package.json`
  scripts and lockfiles, `b2c`/`backoffice`'s `vite-env.d.ts` and
  `src/config/brand.ts`).
- `hadolint` (fetched as a standalone static binary — no daemon needed to
  run it) was run against both Dockerfiles. Remaining findings, both
  accepted deliberately rather than fixed:
  - `DL3018` (pin `apk add` package versions) on both `apk add` lines in
    `platform-api.Dockerfile` — not pinned, because Alpine's own package
    index versions rotate out of its repo entirely on a schedule this
    Dockerfile doesn't control, and unlike the Go/Node/nginx base image
    tags above it, there is no version of `ca-certificates`/`git` this
    repo's own manifests declare a dependency on to pin against. Revisit
    if reproducibility across rebuilds becomes a real problem in
    practice.
  - `DL3066` (info: non-numeric `USER`) on the named `igaming`/`nginx`
    users in both final stages — accepted; a name-based `USER` is the
    normal, readable convention and every orchestrator this document
    anticipates (ECS/Fargate) resolves it via the image's own
    `/etc/passwd`, not the host's.
- Before that fix, `hadolint` also flagged `DL3059` (consecutive `RUN`
  instructions) on `platform-api.Dockerfile`'s two `go build` lines —
  fixed by consolidating them into one `RUN`.
- **Not checked at all, and only verifiable with a real daemon:** that
  the images actually build to completion (module/package downloads
  succeeding, `tsc -b` type-checking cleanly inside the container, the
  final `nginx` non-root permission fix — the `pid` directive rewrite and
  `chown` of `/var/cache/nginx`/`/etc/nginx/conf.d` — actually being
  sufficient for `nginx -g 'daemon off;'` to start cleanly as a non-root
  user), that the built `b2c`/`backoffice` bundles serve correctly and
  the SPA-fallback `try_files` rule behaves as expected, and that
  `platform-api`/`migrate` run correctly inside the final Alpine image
  (e.g. that no code path unexpectedly needs a package only present in
  the Alpine `glibc`-family — CGO is disabled, so this is not expected,
  but "not expected" is not the same as "confirmed"). Whoever runs the
  first real build (CI runner or a developer machine with Docker) should
  do so before trusting any of this beyond static review.

## 1. `platform-api` (Go binary + migration tool, one image)

Build context is the **repo root** (needs `go.mod`, `go.sum`, `cmd/`,
`internal/`, `migrations/`).

```sh
docker build \
  -f deploy/docker/platform-api.Dockerfile \
  -t igaming-platform-api:staging \
  .
```

No `--build-arg`s — this image takes zero build-time configuration by
design; every value `platform-api`/`migrate` need is a runtime env var
supplied by the orchestrator (`DATABASE_URL`, `JWT_SIGNING_SECRET`,
`APP_ENV`, etc. — see `docs/runbooks/production-configuration-
checklist.md`), never baked into the image.

Two container commands from this one image, run as two separate
deploy-pipeline steps (never let `platform-api` invoke `migrate` itself —
see the Dockerfile's own top comment and `docs/architecture/
38-deployment-architecture.md` §2/§3):

```sh
# 1. Migration step - run to completion BEFORE any new replica of the
#    long-running service receives traffic. Requires the migration-owner
#    role's DATABASE_URL (never the runtime role's).
docker run --rm -e DATABASE_URL="$MIGRATION_OWNER_DATABASE_URL" \
  igaming-platform-api:staging /app/migrate up

# 2. Long-running service - the image's default CMD. Requires the
#    non-owning igaming_runtime role's DATABASE_URL in any environment
#    where the role split has been provisioned.
docker run -p 8080:8080 \
  -e DATABASE_URL="$RUNTIME_DATABASE_URL" \
  -e JWT_SIGNING_SECRET="$JWT_SIGNING_SECRET" \
  -e APP_ENV=staging \
  igaming-platform-api:staging
```

**Stage 9.3 addendum:** this image also bundles a `psql` client
(`postgresql16-client`) beyond what running `migrate`/`platform-api`
alone would need, because the AWS ECS deployment (`deploy/aws/`) reuses
this same image for two further one-off task definitions: `role-init`
(`psql` running `deploy/aws/sql/init-runtime-role.rds.sql` to provision
the `igaming_runtime` role on RDS) and the migration task's own chained
`schema_migrations` write-revoke statement, run immediately after
`/app/migrate up` in the same container invocation. See
`docs/runbooks/stage-9-3-staging-deployment-runbook.md` for the full
sequence and why the revoke has to be chained rather than a separate,
skippable step.

## 2. `b2c` (Vite/React SPA)

Build context is the **repo root** (the shared `frontend.Dockerfile`
needs to reach into `b2c/` via `ARG APP_DIR`; see the Dockerfile's own
comment on why a build ARG was chosen over `--build-context`).
`VITE_BRAND_SLUG` is not cosmetic — see `b2c/src/config/brand.ts`; a
production build with it unset throws in the browser at startup rather
than silently defaulting.

```sh
docker build \
  -f deploy/docker/frontend.Dockerfile \
  --build-arg APP_DIR=b2c \
  --build-arg VITE_API_BASE_URL=https://api-staging.example.com \
  --build-arg VITE_BRAND_SLUG=demo-casino \
  --build-arg VITE_BRAND_DISPLAY_NAME="Demo Casino" \
  --build-arg VITE_BRAND_PRIMARY_COLOR="#2563eb" \
  --build-arg VITE_BRAND_PRIMARY_COLOR_HOVER="#1d4ed8" \
  --build-arg VITE_BRAND_DEFAULT_ASSET=USD \
  -t igaming-b2c:staging \
  .
```

## 3. `backoffice` (Vite/React SPA)

Same Dockerfile, `APP_DIR=backoffice`. `backoffice/src/vite-env.d.ts`
declares only `VITE_API_BASE_URL` — none of the `VITE_BRAND_*` args apply
here and are simply omitted.

```sh
docker build \
  -f deploy/docker/frontend.Dockerfile \
  --build-arg APP_DIR=backoffice \
  --build-arg VITE_API_BASE_URL=https://api-staging.example.com \
  -t igaming-backoffice:staging \
  .
```

Both frontend images serve on port `8080` (non-root `nginx`, matching
`platform-api`'s own `EXPOSE 8080` so every image in this deploy answers
the same question — "what port does this task listen on?" — the same
way):

```sh
docker run -p 8081:8080 igaming-b2c:staging
docker run -p 8082:8080 igaming-backoffice:staging
```

## Files in this directory

| File | Purpose |
|---|---|
| `platform-api.Dockerfile` | Multi-stage build of `cmd/platform-api` and `cmd/migrate` into one Alpine-based image. |
| `frontend.Dockerfile` | Generic multi-stage build for either `b2c` or `backoffice`, parameterized by `ARG APP_DIR`, served by non-root `nginx:1.27-alpine`. |
| `nginx-spa.conf` | Shared SPA-fallback nginx server block (`try_files $uri /index.html;`), COPYed into both frontend images. |
| `README.md` | This file. |

Repo-root `.dockerignore` (new, alongside these) keeps `.git`,
`node_modules`, `dist`, and any local `.env*` out of every build context
— see its own comment for why that last one matters even though `.env*`
is already gitignored (a locally-present, gitignored file can still be
picked up by a build context unless explicitly excluded).

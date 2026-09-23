# Multi-stage build for platform-api. Also bundles cmd/migrate and a
# Postgres client, because the SAME image backs THREE distinct ECS task
# definitions for the Stage 9.3 staging deployment (see
# deploy/aws/modules/ecs):
#   1. the long-running platform-api service (default CMD)
#   2. the one-off "migrate" task: `/app/migrate up`, immediately followed
#      (same task, same container command, same master/"igaming"
#      credential) by the authoritative `REVOKE INSERT, UPDATE, DELETE ON
#      schema_migrations FROM igaming_runtime` — see
#      docs/runbooks/stage-9-3-staging-deployment-runbook.md for why this
#      has to be chained rather than a separate step someone could forget.
#      `psql` is required in this image for that chained statement, beyond
#      what running `/app/migrate` alone would need.
#   3. the one-off "role-init" task: `psql` running
#      deploy/aws/sql/init-runtime-role.rds.sql
#
# No `--build-arg`s — this image takes zero build-time configuration by
# design; every value platform-api/migrate need is a runtime env var
# supplied by the orchestrator (DATABASE_URL, JWT_SIGNING_SECRET, APP_ENV,
# etc. — see docs/runbooks/production-configuration-checklist.md), never
# baked into the image.
#
# ENTRYPOINT OVERRIDE CONVENTION: this image sets only a CMD (the
# default, `/app/platform-api`), never a fixed ENTRYPOINT, so an ECS task
# definition's `command` override (or a plain `docker run image <cmd>`)
# fully replaces it — see deploy/aws/modules/ecs/main.tf's migrate/
# role-init task definitions.
#
# Build context is the repo root (needs go.mod/go.sum, cmd/, internal/,
# migrations/):
#   docker build -f deploy/docker/platform-api.Dockerfile -t <tag> .
#
# Two container commands from this one image, run as separate
# deploy-pipeline steps (never let platform-api invoke migrate itself —
# see docs/architecture/38-deployment-architecture.md §2/§3):
#
#   # migration step - run to completion BEFORE any new replica of the
#   # long-running service receives traffic, using the migration-owner
#   # role's DATABASE_URL (never the runtime role's).
#   docker run --rm -e DATABASE_URL="$MIGRATION_OWNER_DATABASE_URL" \
#     <tag> /app/migrate up
#
#   # long-running service - the image's default CMD, using the
#   # non-owning igaming_runtime role's DATABASE_URL.
#   docker run -p 8080:8080 \
#     -e DATABASE_URL="$RUNTIME_DATABASE_URL" \
#     -e JWT_SIGNING_SECRET="$JWT_SIGNING_SECRET" \
#     -e APP_ENV=staging \
#     <tag>

FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/platform-api ./cmd/platform-api \
    && CGO_ENABLED=0 GOOS=linux go build -o /out/migrate ./cmd/migrate \
    && CGO_ENABLED=0 GOOS=linux go build -o /out/seed-admin ./cmd/seed-admin

FROM alpine:3.20

# ca-certificates: outbound TLS (OTLP exporter, future provider calls).
# postgresql16-client: psql, needed by the migrate task's chained revoke
# statement and by the role-init task (see file header).
# wget: used by the ECS container healthCheck commands.
RUN apk add --no-cache ca-certificates postgresql16-client wget

WORKDIR /app

COPY --from=builder /out/platform-api /app/platform-api
COPY --from=builder /out/migrate /app/migrate
# Stage 9.4: the one-off seed-admin ECS task (first platform_admin for the
# Back Office) runs this binary — see deploy/aws/modules/ecs.
COPY --from=builder /out/seed-admin /app/seed-admin
COPY migrations /app/migrations
COPY deploy/aws/sql/init-runtime-role.rds.sql /app/sql/init-runtime-role.rds.sql

RUN adduser -D -u 10001 igaming
USER igaming

EXPOSE 8080

CMD ["/app/platform-api"]

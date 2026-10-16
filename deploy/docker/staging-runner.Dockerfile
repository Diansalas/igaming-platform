# The staging runner image (STAGING-PREP-3, closes gap G1 of docs/runbooks/aws-staging-deploy-plan.md).
#
# A ONE-OFF in-VPC operations image, never a service. The staging RDS instance is private, so every database
# operation that is not migrate / role-init / seed-admin (the platform-api image) runs as an ECS one-off task of THIS image:
#   verify-db, seed-actor-proof, seed-market, backup-restore-drill, pitr-verify, probe-backup-role
# selected by ONE command argument (the mode); parameters are non-secret container-environment overrides
# (deploy/aws/scripts/deploy.sh runner <mode> [KEY=VALUE...]). See docs/plans/staging-prep-3-record.md.
#
# Contents (the plan's G1 list): bash, postgresql 16 client (psql, pg_dump, pg_restore, pg_dumpall), OpenSSL 3 (Ed25519 signing),
# age, jq, the AWS CLI (IAM database-authentication tokens), curl (the RDS CA bundle), the deploy/backup scripts, the SQL verification
# pack, and the Go helpers: migrate (verify), restorecheck, scramhash (client-side SCRAM verifiers: no clear password in any
# statement), seed-market. NO signing key, NO age key, NO credential is baked in: the backup drill generates ephemeral keys inside
# the running task and nothing leaves it.
#
# Immutable tag = the full git commit SHA (deploy.sh build_and_push), like every other image. Build context is the repo root:
#   docker build -f deploy/docker/staging-runner.Dockerfile -t <tag> .
# The default command is `usage` (prints the modes and does nothing else).

FROM golang:1.26.9-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/migrate ./cmd/migrate \
    && CGO_ENABLED=0 GOOS=linux go build -o /out/seed-market ./cmd/seed-market \
    && CGO_ENABLED=0 GOOS=linux go build -o /out/restorecheck ./deploy/backup/restorecheck \
    && CGO_ENABLED=0 GOOS=linux go build -o /out/scramhash ./deploy/backup/scramhash

FROM alpine:3.20

# GNU coreutils/grep/sed/gawk/findutils/procps: the deploy/backup scripts use GNU options (stat -c, date -d, sed -i, ps --ppid, ...) that
# busybox does not provide. openssl >= 3.0 (Alpine 3.20 ships 3.3). postgresql16-client brings psql 16 (\bind) and the 16 dump tools.
RUN apk add --no-cache bash ca-certificates curl jq coreutils grep sed gawk findutils diffutils openssl age postgresql16-client aws-cli \
    && (apk add --no-cache procps-ng || apk add --no-cache procps)

RUN adduser -D -u 10001 -h /home/runner runner \
    && install -d -m 0700 -o runner -g runner /home/runner/scratch \
    && install -d /opt/igaming/bin /opt/igaming/deploy/backup /opt/igaming/deploy/drill /opt/igaming/deploy/staging-runner/modes /opt/igaming/deploy/staging-runner/sql /opt/igaming/migrations

COPY --from=builder /out/ /opt/igaming/bin/
COPY migrations /opt/igaming/migrations

# The reviewed backup tooling, exactly as in the repository (paths relative to its own location: REPO_ROOT = /opt/igaming).
COPY deploy/backup/backup.sh deploy/backup/restore.sh deploy/backup/verify.sh \
     deploy/backup/lib-common.sh deploy/backup/lib-sign.sh deploy/backup/lib-globals.sh \
     deploy/backup/baseline.sql deploy/backup/init-backup-role.sql deploy/backup/harden-backup-surface.sql deploy/backup/post-restore-grants.sql \
     /opt/igaming/deploy/backup/
COPY deploy/drill/verify.sql /opt/igaming/deploy/drill/verify.sql

COPY deploy/staging-runner/entrypoint.sh deploy/staging-runner/lib.sh deploy/staging-runner/lib-probes.sh /opt/igaming/deploy/staging-runner/
COPY deploy/staging-runner/modes/ /opt/igaming/deploy/staging-runner/modes/
COPY deploy/staging-runner/sql/ /opt/igaming/deploy/staging-runner/sql/

RUN chmod 0555 /opt/igaming/deploy/staging-runner/entrypoint.sh /opt/igaming/deploy/backup/*.sh /opt/igaming/bin/* \
    && chmod -R a-w /opt/igaming

ENV RN_ROOT=/opt/igaming
USER 10001
WORKDIR /home/runner

ENTRYPOINT ["/opt/igaming/deploy/staging-runner/entrypoint.sh"]
CMD ["usage"]

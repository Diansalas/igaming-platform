# Runbooks

Operational incident runbooks (failover, reconciliation-drift
investigation, provider outage handling) start once there is a running
system with real operational risk - realistically Stage 3 (wallet/ledger/
payments) onward. This directory carries only local development setup
until then.

## Local development setup (Stage 1)

Prerequisites: Go (see `go.mod` for version), a local PostgreSQL 16
instance (native install or via `deploy/docker-compose.dev.yml` if Docker
is available), `golangci-lint`.

```sh
# 1. Start Postgres (native service) ...
make dev-db-up
# ... or, where Docker is available:
#   docker compose -f deploy/docker-compose.dev.yml up -d

# 2. Create the dev role/database once. The role MUST be created without
#    SUPERUSER and without BYPASSRLS - either attribute silently disables
#    row-level security regardless of policy correctness (see
#    docs/decisions/0002-multi-tenancy-isolation-strategy.md and
#    internal/db/db.go's verifyNotPrivileged, which refuses to connect as
#    such a role at all). See deploy/init-app-role.sql for the same setup
#    via docker-compose.
#   sudo -u postgres psql -c "CREATE ROLE igaming LOGIN PASSWORD 'igaming_dev_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;"
#   sudo -u postgres psql -c "CREATE DATABASE igaming_platform_dev OWNER igaming;"

# 3. Apply migrations
make migrate-up

# 4. Configure environment
cp .env.example .env   # then `set -a; source .env; set +a` or export manually

# 5. Run the service
make run

# 6. In another shell, exercise it
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

## Verifying the foundation

```sh
make ci                 # fmt-check, vet, lint, build, unit tests
make test-integration    # RLS tenant-isolation + HTTP integration tests,
                          # against the running local database
```

See `docs/architecture/00-system-overview.md` for what "the foundation"
currently covers, and `docs/active-stage.md` for what's deferred to later
stages.

.PHONY: build run test test-integration test-integration-runtime-role lint fmt fmt-check vet migrate-up migrate-down migrate-status dev-db-up dev-db-down dev-db-init-roles dev-db-init-test-admin ci

GO ?= go
DATABASE_URL ?= postgres://igaming:igaming_dev_password@127.0.0.1:5432/igaming_platform_dev?sslmode=disable
DEV_DB ?= igaming_platform_dev
# igaming_runtime is PLAT-ROLESPLIT-1's non-owning runtime role (see
# deploy/init-app-role.sql and docs/security/runtime-role-separation.md).
# Not used by DATABASE_URL/TEST_DATABASE_URL above (see dev-db-init-roles
# below for why) - only by test-integration-runtime-role's dedicated
# regression test.
RUNTIME_DATABASE_URL ?= postgres://igaming_runtime:igaming_runtime_dev_password@127.0.0.1:5432/igaming_platform_dev?sslmode=disable
# igaming_test_admin (Stage 10 W0) is a DEV/CI-only role used solely by
# internal/testsupport/scratchdb to create/drop scratch databases for the
# migration and RLS tests; see dev-db-init-test-admin below and
# docs/testing/testing-strategy.md "Scratch databases". Without it those
# tests skip.
TEST_ADMIN_DATABASE_URL ?= postgres://igaming_test_admin:igaming_test_admin_dev_password@127.0.0.1:5432/igaming_platform_dev?sslmode=disable

build:
	$(GO) build ./...

# APP_ENV defaults to development for `make run` (gate 10.3-W1 code review
# #8): the synthetic-component startup guard (MOCK-ADAPTER-PROD-1) treats a
# MISSING APP_ENV as production and refuses to start with the mock
# adapters, so the local run target sets it explicitly. Override with e.g.
# `make run APP_ENV=staging`; never use this target for production.
APP_ENV ?= development

run:
	APP_ENV=$(APP_ENV) DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/platform-api

test:
	$(GO) test ./...

test-integration:
	TEST_DATABASE_URL=$(DATABASE_URL) TEST_ADMIN_DATABASE_URL=$(TEST_ADMIN_DATABASE_URL) $(GO) test -tags=integration -v ./...

# test-integration-runtime-role additionally sets TEST_RUNTIME_DATABASE_URL
# so internal/db/runtime_role_separation_test.go's adversarial-probe suite
# actually runs (it t.Skip()s cleanly without this var, exactly like every
# other integration test skips on a missing TEST_DATABASE_URL). Requires
# `make dev-db-init-roles` to have been run first so igaming_runtime
# exists. Kept as a separate target rather than folded into
# test-integration so a contributor who hasn't provisioned the runtime
# role yet still gets a clean, fast `make test-integration`.
test-integration-runtime-role:
	TEST_DATABASE_URL=$(DATABASE_URL) TEST_RUNTIME_DATABASE_URL=$(RUNTIME_DATABASE_URL) $(GO) test -tags=integration -v ./internal/db/...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needs to be run on:"; gofmt -l .; exit 1)

vet:
	$(GO) vet ./...

migrate-up:
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/migrate up

migrate-down:
	APP_ENV=development DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/migrate -steps=$(steps) down

migrate-status:
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/migrate status

# dev-db-up/down manage the local Postgres cluster used for development
# and manual testing. CI instead uses a Postgres service container - see
# .github/workflows/ci.yml.
dev-db-up:
	service postgresql start

dev-db-down:
	service postgresql stop

# dev-db-init-roles provisions deploy/init-app-role.sql's roles
# ("igaming", the migration-owner, and "igaming_runtime", the
# PLAT-ROLESPLIT-1 non-owning runtime role) against this environment's
# native Postgres cluster - the "actual script that creates the igaming
# role" for this sandboxed dev environment (docker-compose.dev.yml uses
# init-app-role.sql directly via docker-entrypoint-initdb.d instead; this
# target keeps the two paths running identical SQL). Requires the
# "igaming_platform_dev" database to already exist and requires
# passwordless-via-sudo access to the "postgres" OS/cluster superuser
# (the standard arrangement for a native apt-installed Postgres). Safe to
# re-run: every statement in init-app-role.sql is idempotent.
dev-db-init-roles:
	sudo -u postgres psql -d $(DEV_DB) -v ON_ERROR_STOP=1 -f deploy/init-app-role.sql

# dev-db-init-test-admin provisions the DEV-only igaming_test_admin role
# (deploy/init-test-admin-role.dev.sql) that scratch-database integration
# tests use to CREATE/DROP throwaway databases. Never run in staging or
# production. Requires dev-db-init-roles first (igaming must exist).
dev-db-init-test-admin:
	sudo -u postgres psql -d $(DEV_DB) -v ON_ERROR_STOP=1 -f deploy/init-test-admin-role.dev.sql

# ci runs the same checks CI runs, so failures are caught locally first.
ci: fmt-check vet lint build test
	@echo "Run 'make test-integration' separately against a running dev database."

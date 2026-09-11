.PHONY: build run test test-integration lint fmt fmt-check vet migrate-up migrate-down migrate-status dev-db-up dev-db-down ci

GO ?= go
DATABASE_URL ?= postgres://igaming:igaming_dev_password@127.0.0.1:5432/igaming_platform_dev?sslmode=disable

build:
	$(GO) build ./...

run:
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/platform-api

test:
	$(GO) test ./...

test-integration:
	TEST_DATABASE_URL=$(DATABASE_URL) $(GO) test -tags=integration -v ./...

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
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/migrate -steps=$(steps) down

migrate-status:
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/migrate status

# dev-db-up/down manage the local Postgres cluster used for development
# and manual testing. CI instead uses a Postgres service container - see
# .github/workflows/ci.yml.
dev-db-up:
	service postgresql start

dev-db-down:
	service postgresql stop

# ci runs the same checks CI runs, so failures are caught locally first.
ci: fmt-check vet lint build test
	@echo "Run 'make test-integration' separately against a running dev database."

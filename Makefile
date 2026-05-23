.PHONY: build run-api run-worker run-kchat-bridge test test-integration compose-up compose-down lint migrate proto-lint proto-gen proto-breaking proto-format

# The superuser connection runs migrations and test setup.
# The application connection uses the kapp_app role so local runs exercise
# the same RLS policies the production cell enforces. kapp_app is created
# by migrations/000001_initial_schema.sql. kapp_admin (BYPASSRLS) is
# created by migrations/000002_admin_role.sql and is used only for
# cross-tenant control-plane reads.
DB_URL       ?= $(shell echo "postgres://kapp:$${POSTGRES_PASSWORD:-kapp_dev}@localhost:5432/kapp?sslmode=disable")
APP_DB_URL   ?= postgres://kapp_app:kapp_app_dev@localhost:5432/kapp?sslmode=disable
ADMIN_DB_URL ?= postgres://kapp_admin:kapp_admin_dev@localhost:5432/kapp?sslmode=disable

build:
	go build -o bin/api ./services/api
	go build -o bin/worker ./services/worker
	go build -o bin/kchat-bridge ./services/kchat-bridge
	go build -o bin/kapp-backup ./services/kapp-backup

migrate:
	@set -e; for f in migrations/*.sql; do \
		echo "Running $$f..."; \
		psql "$(DB_URL)" -f "$$f"; \
	done

run-api: build
	DB_URL="$(APP_DB_URL)" ADMIN_DB_URL="$(ADMIN_DB_URL)" ./bin/api

run-worker: build
	DB_URL="$(APP_DB_URL)" ADMIN_DB_URL="$(ADMIN_DB_URL)" ./bin/worker

run-kchat-bridge: build
	DB_URL="$(APP_DB_URL)" ADMIN_DB_URL="$(ADMIN_DB_URL)" ./bin/kchat-bridge

test:
	go test ./...

# Phase A+B integration tests: require the docker-compose DB to be up and
# migrated. Skipped in plain `make test` / CI because they open a real
# Postgres connection. KAPP_TEST_DB_URL uses the app role so the test
# suite exercises RLS the same way production does.
test-integration:
	KAPP_TEST_DB_URL="$(APP_DB_URL)" \
	KAPP_TEST_ADMIN_DB_URL="$(ADMIN_DB_URL)" \
	go test -tags=integration -v ./internal/integrationtest/...

compose-up:
	docker compose up -d

compose-down:
	docker compose down

lint:
	golangci-lint run ./...

# Protobuf / gRPC targets (Pillar A4 / A5).
#
# `proto-lint` enforces buf's STANDARD ruleset on proto/. Wired into
# CI so a malformed schema is caught before code review.
proto-lint:
	buf lint

# `proto-gen` regenerates everything under gen/go/. Generated code
# is NOT checked in (gen/ is gitignored); CI runs `make proto-gen`
# before every Go build/test step. Run this once after a fresh
# clone so `go build ./...` works locally. The two `go install`
# guards are idempotent — Go caches the plugins under $GOPATH/bin
# so subsequent runs are no-ops.
proto-gen:
	@command -v protoc-gen-go >/dev/null 2>&1 || go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	buf generate

# `proto-breaking` rejects backwards-incompatible field/service
# changes against origin/main. The Rust SDK depends on stable
# field numbers; this is the load-bearing guarantee.
proto-breaking:
	buf breaking --against '.git#branch=main,subdir=proto' proto

# `proto-format` is buf's canonical formatter. Use before
# committing proto edits to avoid review-time formatting churn.
proto-format:
	buf format -w

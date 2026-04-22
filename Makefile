.PHONY: build run-api run-worker run-kchat-bridge test test-integration compose-up compose-down lint migrate

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

migrate:
	psql "$(DB_URL)" -f migrations/000001_initial_schema.sql
	psql "$(DB_URL)" -f migrations/000002_admin_role.sql
	psql "$(DB_URL)" -f migrations/000003_forms.sql
	psql "$(DB_URL)" -f migrations/000004_finance_extensions.sql

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

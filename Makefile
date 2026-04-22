.PHONY: build run-api run-worker test test-integration compose-up compose-down lint migrate

# DB_URL defaults to the local docker-compose connection. Override via env
# in any non-local environment.
DB_URL ?= $(shell echo "postgres://kapp:$${POSTGRES_PASSWORD:-kapp_dev}@localhost:5432/kapp?sslmode=disable")

build:
	go build -o bin/api ./services/api
	go build -o bin/worker ./services/worker
	go build -o bin/kchat-bridge ./services/kchat-bridge

migrate:
	psql "$(DB_URL)" -f migrations/000001_initial_schema.sql

run-api: build
	./bin/api

run-worker: build
	./bin/worker

test:
	go test ./...

# Phase A integration tests: require the docker-compose DB to be up and
# migrated. Skipped in plain `make test` / CI because they open a real
# Postgres connection.
test-integration:
	KAPP_TEST_DB_URL="$(DB_URL)" go test -tags=integration -v ./internal/integrationtest/...

compose-up:
	docker compose up -d

compose-down:
	docker compose down

lint:
	golangci-lint run ./...

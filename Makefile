.PHONY: build run-api run-worker test compose-up compose-down lint migrate

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

compose-up:
	docker compose up -d

compose-down:
	docker compose down

lint:
	golangci-lint run ./...

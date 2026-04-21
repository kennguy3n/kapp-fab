.PHONY: build run-api run-worker test compose-up compose-down lint

build:
	go build -o bin/api ./services/api
	go build -o bin/worker ./services/worker

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

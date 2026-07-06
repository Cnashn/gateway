.PHONY: build run test test-unit lint docker-up docker-down

build:
	go build -o bin/gateway ./cmd/gateway

run:
	go run ./cmd/gateway

test:
	go test ./...

test-unit:
	go test -short ./...

lint:
	go vet ./...

docker-up:
	docker compose -f deploy/docker-compose.yml up --build -d

docker-down:
	docker compose -f deploy/docker-compose.yml down

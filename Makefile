.PHONY: build run test test-unit lint docker-up docker-down demo-breaker loadtest

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

loadtest:
	@echo "=== scenario 1: steady load under the rate limit ==="
	k6 run deploy/loadtest/steady.js
	@echo "=== scenario 2: burst over the rate limit ==="
	k6 run deploy/loadtest/burst.js
	@echo "=== scenario 3: upstream down, breaker short-circuits ==="
	docker compose -f deploy/docker-compose.yml stop orders
	@sleep 3
	k6 run deploy/loadtest/breaker.js
	docker compose -f deploy/docker-compose.yml start orders

demo-breaker:
	docker compose -f deploy/docker-compose.yml stop orders
	@echo "orders upstream killed; watch the breaker open (502s then 503 short-circuits):"
	@for i in $$(seq 1 14); do curl -s -o /dev/null -w "%{http_code} " http://localhost:8080/api/orders/; sleep 0.3; done; echo
	@echo "short-circuited response:"
	@curl -s -i http://localhost:8080/api/orders/ | grep -E "HTTP|Retry-After|error"
	docker compose -f deploy/docker-compose.yml start orders

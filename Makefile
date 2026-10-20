TEST_DATABASE_URL ?= postgres://quickbytes:quickbytes@localhost:5432/quickbytes_test?sslmode=disable
# Database 1 of the dev Redis, so tests don't mix with the app's cache in database 0.
TEST_REDIS_URL ?= redis://localhost:6379/1

.PHONY: up down db run test lint fmt bench-near-me swagger-ui

up: ## Start everything (database + API) in Docker
	docker compose up --build -d

down: ## Stop everything (data is kept in a Docker volume)
	docker compose down

db: ## Start only the database and Redis
	docker compose up -d --wait postgres redis

run: db ## Run the API on your machine against the Docker database
	set -a && . ./.env && set +a && go run ./cmd/api

test: db ## Run all tests, including the ones that need the database
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_REDIS_URL="$(TEST_REDIS_URL)" go test -race -count=1 ./...

lint: ## Check formatting and common mistakes
	@test -z "$$(gofmt -l .)" || (echo "Not gofmt-formatted:"; gofmt -l .; exit 1)
	go vet ./...

fmt:
	gofmt -w .

bench-near-me: db ## Time the near-me query on 100k generated restaurants, without and with the index
	docker compose exec -T postgres psql -q -U quickbytes -d quickbytes -f - < scripts/bench-near-me.sql

swagger-ui: ## Download the pinned Swagger UI release (checksum-verified) into api/swaggerui
	./scripts/update-swagger-ui.sh

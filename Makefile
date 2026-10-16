TEST_DATABASE_URL ?= postgres://quickbytes:quickbytes@localhost:5432/quickbytes_test?sslmode=disable

.PHONY: up down db run test lint fmt

up: ## Start everything (database + API) in Docker
	docker compose up --build -d

down: ## Stop everything (data is kept in a Docker volume)
	docker compose down

db: ## Start only the database
	docker compose up -d --wait postgres

run: db ## Run the API on your machine against the Docker database
	set -a && . ./.env && set +a && go run ./cmd/api

test: db ## Run all tests, including the ones that need the database
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race ./...

lint: ## Check formatting and common mistakes
	@test -z "$$(gofmt -l .)" || (echo "Not gofmt-formatted:"; gofmt -l .; exit 1)
	go vet ./...

fmt:
	gofmt -w .

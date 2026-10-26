TEST_DATABASE_URL ?= postgres://quickbytes:quickbytes@localhost:5432/quickbytes_test?sslmode=disable
# The mock payment provider's own test database, next to the API's.
TEST_PAYPROVIDER_DATABASE_URL ?= postgres://quickbytes:quickbytes@localhost:5432/payprovider_test?sslmode=disable
# Database 1 of the dev Redis, so tests don't mix with the app's cache in database 0.
TEST_REDIS_URL ?= redis://localhost:6379/1
# The dev database and cache (the same ones `make up` uses), reached from this machine.
DEV_DATABASE_URL ?= postgres://quickbytes:quickbytes@localhost:5432/quickbytes?sslmode=disable
DEV_REDIS_URL ?= redis://localhost:6379/0
# Password of every demo account. Public on purpose: the seeder only writes to a
# local database, so these accounts never exist anywhere real.
SEED_PASSWORD ?= quickbytes-demo

.PHONY: up down db run test lint fmt seed seed-reset bench-near-me load-menu swagger-ui

up: db ## Start everything (database + API) in Docker
	docker compose up --build -d

down: ## Stop everything (data is kept in a Docker volume)
	docker compose down

db: ## Start only the database and Redis, and create any database that's missing
	docker compose up -d --wait postgres redis
	docker compose exec -T postgres psql -q -v ON_ERROR_STOP=1 -U quickbytes -d quickbytes -f - < scripts/init-databases.sql

run: db ## Run the API on your machine against the Docker database
	set -a && . ./.env && set +a && go run ./cmd/api

test: db ## Run all tests, including the ones that need the database
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_PAYPROVIDER_DATABASE_URL="$(TEST_PAYPROVIDER_DATABASE_URL)" \
		TEST_REDIS_URL="$(TEST_REDIS_URL)" go test -race -count=1 ./...

lint: ## Check formatting and common mistakes
	@test -z "$$(gofmt -l .)" || (echo "Not gofmt-formatted:"; gofmt -l .; exit 1)
	go vet ./...

fmt:
	gofmt -w .

seed: db ## Fill the dev database with Istanbul demo data (running it again adds only what's missing)
	DATABASE_URL="$(DEV_DATABASE_URL)" REDIS_URL="$(DEV_REDIS_URL)" SEED_PASSWORD="$(SEED_PASSWORD)" go run ./cmd/seed

seed-reset: db ## Delete only the demo data (accounts on @demo.quickbytes.test and what they own), then seed it fresh
	DATABASE_URL="$(DEV_DATABASE_URL)" REDIS_URL="$(DEV_REDIS_URL)" SEED_PASSWORD="$(SEED_PASSWORD)" go run ./cmd/seed -reset

bench-near-me: db ## Time the near-me query on 100k generated restaurants, without and with the index
	docker compose exec -T postgres psql -q -U quickbytes -d quickbytes -f - < scripts/bench-near-me.sql

load-menu: ## Load-test the public menu (50 users, 30 s each): no cache, cache, then 304s via ETag
	API_REDIS_URL= docker compose up --build -d --wait api
	docker compose run --rm k6 run -q -e MODE=nocache /scripts/load-menu.js
	docker compose up -d --wait api
	docker compose run --rm k6 run -q -e MODE=cache /scripts/load-menu.js
	docker compose run --rm k6 run -q -e MODE=etag /scripts/load-menu.js

swagger-ui: ## Download the pinned Swagger UI release (checksum-verified) into api/swaggerui
	./scripts/update-swagger-ui.sh

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

.PHONY: up down db topics events console run test lint fmt seed seed-reset bench-near-me load-menu load-checkout swagger-ui

up: db topics ## Start everything (database, Redpanda, API, payment provider) in Docker
	docker compose up --build -d

down: ## Stop everything, the console too (data is kept in Docker volumes)
	docker compose --profile console down

db: ## Start only the database and Redis, and create any database that's missing
	docker compose up -d --wait postgres redis
	docker compose exec -T postgres psql -q -v ON_ERROR_STOP=1 -U quickbytes -d quickbytes -f - < scripts/init-databases.sql

topics: ## Start Redpanda and create its topics if they're missing
	docker compose up -d --wait redpanda
	@# Topics are created on purpose, here, so a typo in a topic name fails
	@# loudly instead of quietly making a new, empty topic.
	docker compose exec -T redpanda rpk cluster config set auto_create_topics_enabled false
	docker compose exec -T redpanda rpk topic create orders --partitions 3 --replicas 1 --if-not-exists

events: topics ## Watch order events live, from the first one (Ctrl+C to stop)
	docker compose exec redpanda rpk topic consume orders --offset start \
		--format 'partition %p  offset %o  key %k  %h{%k=%v }\n%v\n\n'

console: topics ## Open Redpanda Console (browse topics and messages) at http://localhost:8088
	docker compose --profile console up -d console
	@echo "Redpanda Console: http://localhost:8088"

run: db topics ## Run the API on your machine against the Docker database
	set -a && . ./.env && set +a && go run ./cmd/api

test: db ## Run all tests, including the ones that need the database
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_PAYPROVIDER_DATABASE_URL="$(TEST_PAYPROVIDER_DATABASE_URL)" \
		TEST_REDIS_URL="$(TEST_REDIS_URL)" go test -race -count=1 ./...

lint: ## Check formatting and common mistakes
	@test -z "$$(gofmt -l .)" || (echo "Not gofmt-formatted:"; gofmt -l .; exit 1)
	go vet ./...

fmt:
	gofmt -w .

seed: db ## Fill the dev database with Istanbul demo data and a few orders (running it again adds only what's missing)
	@# The demo orders are placed through the running API, so start it first.
	docker compose up -d --wait api payprovider
	DATABASE_URL="$(DEV_DATABASE_URL)" REDIS_URL="$(DEV_REDIS_URL)" SEED_PASSWORD="$(SEED_PASSWORD)" go run ./cmd/seed

seed-reset: db ## Delete only the demo data (accounts on @demo.quickbytes.test and what they own), then seed it fresh
	@# The demo orders are placed through the running API, so start it first.
	docker compose up -d --wait api payprovider
	DATABASE_URL="$(DEV_DATABASE_URL)" REDIS_URL="$(DEV_REDIS_URL)" SEED_PASSWORD="$(SEED_PASSWORD)" go run ./cmd/seed -reset

bench-near-me: db ## Time the near-me query on 100k generated restaurants, without and with the index
	docker compose exec -T postgres psql -q -U quickbytes -d quickbytes -f - < scripts/bench-near-me.sql

load-menu: ## Load-test the public menu (50 users, 30 s each): no cache, cache, then 304s via ETag
	API_REDIS_URL= docker compose up --build -d --wait api
	docker compose run --rm k6 run -q -e MODE=nocache /scripts/load-menu.js
	docker compose up -d --wait api
	docker compose run --rm k6 run -q -e MODE=cache /scripts/load-menu.js
	docker compose run --rm k6 run -q -e MODE=etag /scripts/load-menu.js

load-checkout: ## Retry checkouts hard (7 customers, 10 tries per key, every test card), then count double charges at the provider
	docker compose up -d --wait api payprovider
	docker compose run --rm -e PROVIDER_URL=http://payprovider:8090 -e KEYS k6 run -q /scripts/load-checkout.js
	@echo "Orders with more than one payment at the provider (ever, want 0):"
	@docker compose exec -T postgres psql -qtA -U quickbytes -d payprovider -c \
		"SELECT count(*) FROM (SELECT reference FROM payments WHERE reference IS NOT NULL GROUP BY reference HAVING count(*) > 1) d"
	@echo "Outbox lag, written to published, last 10 minutes (p50 / p95 / max, ms):"
	@docker compose exec -T postgres psql -qtA -U quickbytes -d quickbytes -c \
		"SELECT round(percentile_cont(0.5) WITHIN GROUP (ORDER BY l)) || ' / ' || round(percentile_cont(0.95) WITHIN GROUP (ORDER BY l)) || ' / ' || round(max(l)) || '  (' || count(*) || ' events)' FROM (SELECT extract(epoch FROM published_at - created_at) * 1000 AS l FROM outbox WHERE published_at IS NOT NULL AND created_at > now() - interval '10 minutes') x"

swagger-ui: ## Download the pinned Swagger UI release (checksum-verified) into api/swaggerui
	./scripts/update-swagger-ui.sh

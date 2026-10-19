# Local development. Variables come from .env (copy .env.example first).
-include .env
export

.PHONY: up down ps

up: ## start postgres, redis, minio (all on 127.0.0.1), then create the buckets
	docker compose up -d --wait postgres redis
	docker compose up -d minio
	docker compose run --rm minio-init

down:
	docker compose down

ps:
	docker compose ps

.PHONY: migrate test test-integration

migrate: ## apply pending migrations (the API also does this at startup)
	go run ./cmd/ingest migrate

test:
	go test ./...

test-integration: ## needs `make up`. -p 1: packages share goalstream_test and the test bucket, so they must not run in parallel
	go test -tags integration -race -count=1 -p 1 ./...

.PHONY: fixtures
fixtures: ## generate synthetic test media into testdata/ (never real footage)
	scripts/fixtures.sh

.PHONY: ingest
ingest: ## cut clips from $(DATA_DIR)/matches into MinIO + Postgres, then write tags_todo.csv
	go run ./cmd/ingest run

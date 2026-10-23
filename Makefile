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

.PHONY: api fe
api: ## run the API (migrates first)
	go run ./cmd/api

fe: ## frontend dev server on :5173, proxies /api to the API (run `npm ci` in web/ once)
	cd web && npm run dev

.PHONY: ingest ingest-tags reembed
ingest: ## cut clips from $(DATA_DIR)/matches into MinIO + Postgres, then write tags_todo.csv
	go run ./cmd/ingest run

ingest-tags: ## apply the hand-filled $(TAGS_TODO_PATH) (tags separated by |), then rebuild embeddings
	go run ./cmd/ingest tags

reembed: ## rebuild every clip embedding from its columns + vocab
	go run ./cmd/ingest reembed

.PHONY: latency
latency: ## p50/p95 of GET /next over 1000 warm requests (needs `make api` in AUTH_MODE=dev)
	@INVITE_CODE=$$(echo "$(INVITE_CODES)" | cut -d, -f1 | cut -d: -f2-) scripts/next_latency.sh

.PHONY: admin
admin: ## grant is_admin: make admin EMAIL=me@x.com   (REVOKE=1 to demote)
	go run ./cmd/ingest admin -email "$(EMAIL)" $(if $(REVOKE),-revoke)

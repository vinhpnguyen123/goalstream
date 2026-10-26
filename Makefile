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

.PHONY: api fe worker
api: ## run the API (migrates first)
	go run ./cmd/api

worker: ## run one transcode job: make worker JOB=<job_id>  (the queue loop arrives in M6)
	@test -n "$(JOB)" || { echo "JOB=<job_id> required"; exit 1; }
	go run ./cmd/worker -once $(JOB)

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

# --- production (M4.5): cmd/ingest run from this laptop against R2 + Render Postgres -------
# Every target reads ONLY .env.prod (gitignored) via scripts/prod.sh. Render's Postgres admits
# external connections only from the allow-listed IP: add yours before, remove it after.
.PHONY: prod-migrate prod-run prod-tags prod-reembed prod-dump prod-restore
prod-migrate prod-run prod-tags prod-reembed: prod-%:
	scripts/prod.sh go run ./cmd/ingest $*

# pg_dump/pg_restore from the pg16 image: matches Render's server (a newer local pg_dump writes
# settings PG16 rejects). Dumps hold password and token hashes — backups/ is gitignored.
PG16 = docker run --rm -e DATABASE_URL -v "$(CURDIR)/backups:/backups" pgvector/pgvector:pg16

prod-dump: ## dump production into backups/ — run before the free DB expires (30 days)
	@mkdir -p backups
	scripts/prod.sh sh -c '$(PG16) pg_dump --format=custom --no-owner --no-acl --file=/backups/prod-$$(date +%Y%m%d-%H%M%S).dump "$$DATABASE_URL"'

prod-restore: ## restore a dump into the (new) production DB: make prod-restore DUMP=backups/prod-….dump
	@test -f "$(DUMP)" || { echo "DUMP=backups/<file>.dump required"; exit 1; }
	scripts/prod.sh sh -c '$(PG16) pg_restore --clean --if-exists --no-owner --no-acl --exit-on-error --dbname="$$DATABASE_URL" /backups/$(notdir $(DUMP))'

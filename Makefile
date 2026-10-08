.PHONY: build test test-real-bucket test-postgres lint fmt web web-test python-test python-lint

build:
	go build ./...

test:
	go test -race -count=1 ./...

# The tests that need a real S3 bucket, such as Wasabi. Their settings come
# from a file outside the repo (override the path with REAL_BUCKET_ENV=...),
# one KEY="value" per line:
#   ENGRAM_TEST_S3_ENDPOINT, _REGION, _BUCKET, _ACCESS_KEY, _SECRET_KEY
# The tests skip when the endpoint is unset, so check it here to avoid a
# green run that tested nothing.
REAL_BUCKET_ENV ?= $(HOME)/.config/engram-garden/real-bucket.env

test-real-bucket:
	@test -f "$(REAL_BUCKET_ENV)" || { echo "$(REAL_BUCKET_ENV) not found"; exit 1; }
	@set -a; . "$(REAL_BUCKET_ENV)"; set +a; \
	test -n "$$ENGRAM_TEST_S3_ENDPOINT" || { echo "ENGRAM_TEST_S3_ENDPOINT is empty in $(REAL_BUCKET_ENV)"; exit 1; }; \
	go test -v -count=1 -run TestRealBucket ./internal/blob/ ./internal/spacestore/

# The tests against a real Postgres: the control-plane store's own tests,
# then every suite that builds an appview, with its store in Postgres
# instead of memory. By default this starts a throwaway Postgres in Docker;
# set ENGRAM_TEST_POSTGRES_URL (a postgres:// URL) to use a database you
# already have. Each test works in its own schema and drops it afterwards.
# The store's own tests skip when the URL is unset, so this target sets it,
# to avoid a green run that tested nothing.
POSTGRES_IMAGE ?= postgres:17-alpine
POSTGRES_TESTS = go test -race -v -count=1 -run Postgres ./internal/control/ && \
	go test -race -count=1 ./internal/appview/ ./internal/web/ ./internal/mcpserver/ ./cmd/engram/

test-postgres:
	@if [ -n "$$ENGRAM_TEST_POSTGRES_URL" ]; then \
		$(POSTGRES_TESTS); \
	else \
		name=engram-test-postgres-$$$$; \
		docker run -d --rm --name $$name -e POSTGRES_PASSWORD=test -p 127.0.0.1::5432 $(POSTGRES_IMAGE) -c max_connections=500 >/dev/null || exit 1; \
		trap 'docker stop $$name >/dev/null' EXIT; \
		port=$$(docker port $$name 5432/tcp | head -n1 | sed 's/.*://'); \
		for i in $$(seq 60); do docker exec $$name pg_isready -h 127.0.0.1 -U postgres -q && break; sleep 1; done; \
		docker exec $$name pg_isready -h 127.0.0.1 -U postgres -q || { echo "postgres didn't start"; exit 1; }; \
		export ENGRAM_TEST_POSTGRES_URL="postgres://postgres:test@127.0.0.1:$$port/postgres"; \
		$(POSTGRES_TESTS); \
	fi

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run make fmt" && exit 1)

fmt:
	go fmt ./...

# The web app's frontend, built into internal/web/dist/app for engram-web
# to embed.
web:
	cd web && pnpm install --frozen-lockfile && pnpm build

web-test:
	cd web && pnpm test

# The Python client (python/). Needs uv. The Go side of its interoperability
# fixtures runs with `make test`; regenerate go.json after changing internal/lex
# with ENGRAM_WRITE_PY_FIXTURES=1 go test ./internal/lex/, then python.json with
# `cd python && uv run python tests/make_fixtures.py`.
python-test:
	cd python && uv sync --extra dev && uv run pytest -q

python-lint:
	cd python && uv sync --extra dev && uv run ruff check . && uv run ruff format --check .

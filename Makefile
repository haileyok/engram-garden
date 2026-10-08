.PHONY: build test test-real-bucket lint fmt web web-test

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

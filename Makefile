.PHONY: build test lint fmt web web-test

build:
	go build ./...

test:
	go test -race -count=1 ./...

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

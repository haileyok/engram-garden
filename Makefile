.PHONY: build test lint fmt

build:
	go build ./...

test:
	go test -race -count=1 ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run make fmt" && exit 1)

fmt:
	go fmt ./...

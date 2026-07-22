.PHONY: tidy fmt test test-race vet build run-api run-worker

tidy:
	go mod tidy

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

build:
	go build ./cmd/api ./cmd/worker

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

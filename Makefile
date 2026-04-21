.PHONY: build run test lint tidy clean

BIN := bin/group-limit-bot
VERSION ?= dev-$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X github.com/herbertgao/group-limit-bot/internal/version.Version=$(VERSION)

build:
	go build -trimpath -ldflags='$(LDFLAGS)' -o $(BIN) ./cmd/bot

run:
	go run ./cmd/bot --config ./config.yaml

test:
	go test ./... -race -count=1

lint:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/ coverage.out

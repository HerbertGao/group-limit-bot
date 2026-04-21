.PHONY: build run test lint tidy clean

BIN := bin/group-limit-bot

build:
	go build -trimpath -ldflags='-s -w' -o $(BIN) ./cmd/bot

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

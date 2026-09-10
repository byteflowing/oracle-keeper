.PHONY: all build build-dev run once test lint vet fmt tidy docker-build

# Published binary name.
BIN_NAME := oracle-keeper
CMD_DIR  := cmd/keeper

all: build

## build: Build the oracle-keeper binary
build:
	go build -trimpath -ldflags="-s -w" -o bin/$(BIN_NAME) ./$(CMD_DIR)/

## build-dev: Build without stripping (for debugging)
build-dev:
	go build -o bin/$(BIN_NAME) ./$(CMD_DIR)/

## run: Run the daemon locally (auto-copies config.example.yaml -> config.yaml if missing)
run:
	@test -f config.yaml || cp config.example.yaml config.yaml
	go run ./$(CMD_DIR)/

## once: Run a single keep-alive cycle immediately, then exit (smoke test)
once:
	@test -f config.yaml || cp config.example.yaml config.yaml
	go run ./$(CMD_DIR)/ once

## test: Run tests with race detector
test:
	go test -race -coverprofile=coverage.out ./...

## lint: Run golangci-lint
lint:
	golangci-lint run

## vet: Run go vet
vet:
	go vet ./...

## fmt: Format all Go sources
fmt:
	gofmt -s -w $(shell find . -name '*.go' -not -path './bin/*')

## tidy: Sync go.mod/go.sum
tidy:
	go mod tidy

## docker-build: Build the runtime image with the CI image name (arm64 A1 boxes
## build natively; other arches are cross-compiled by the Dockerfile)
docker-build:
	docker build -f Dockerfile -t ghcr.io/servekit/oracle-keeper:latest ..

GO        ?= go
BIN       := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X github.com/AkibUllah/go-api-gateway/internal/app.Version=$(VERSION)
CONFIG    ?= configs/gateway.yaml

.PHONY: help build test race cover bench vet lint fmt check run demo token compose-up compose-down docker clean

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-13s %s\n", $$1, $$2}'

build: ## build all binaries into ./bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/ ./cmd/...

test: ## run unit and integration tests
	$(GO) test ./...

race: ## run tests with the race detector
	$(GO) test -race -count=1 ./...

cover: ## race tests with a coverage report
	$(GO) test -race -count=1 -coverprofile=coverage.out -coverpkg=./internal/... ./...
	$(GO) tool cover -func=coverage.out | tail -1

bench: ## run benchmarks
	$(GO) test -run='^$$' -bench=. -benchmem ./...

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint (v2)
	golangci-lint run ./...

fmt: ## format code
	golangci-lint fmt ./...

check: build ## validate the config file
	$(BIN)/gateway -config $(CONFIG) -check

run: build ## run the gateway (needs upstreams; see `make demo`)
	$(BIN)/gateway -config $(CONFIG)

demo: build ## gateway + in-process demo upstreams on :8080 / admin :9090
	$(BIN)/gateway-demo -config $(CONFIG)

token: ## mint a demo customer JWT
	@$(GO) run ./cmd/devtoken

compose-up: ## gateway, 3 upstreams and Redis in docker
	docker compose up -d --build

compose-down:
	docker compose down -v

docker: ## build the container image
	docker build --build-arg VERSION=$(VERSION) -t go-api-gateway:$(VERSION) .

clean:
	rm -rf $(BIN) coverage.out

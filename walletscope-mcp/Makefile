BIN      := bin/walletscope
VERSION  ?= 0.1.0
LDFLAGS  := -s -w -X github.com/amnariaz007/walletscope-mcp/internal/mcpserver.Version=$(VERSION)
IMAGE    ?= walletscope-mcp:local

.PHONY: all tidy fmt vet test test-short race build run ui run-http smoke docker docker-smoke demo clean

all: fmt vet test build

tidy:
	go mod tidy

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./...

test-short:
	go test -short ./...

race:
	go test -race ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/walletscope
	go build -o bin/mcpcall ./scripts/mcpcall

run: build
	./$(BIN)

ui: build
	./$(BIN) ui

run-http: build
	WALLETSCOPE_TRANSPORT=http ./$(BIN)

# Live smoke test against the public Blockscout and OFAC endpoints (network required).
smoke: build
	./scripts/smoke.sh

demo: build
	./scripts/demo.sh

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-smoke: docker
	./scripts/docker-smoke.sh $(IMAGE)

clean:
	rm -rf bin dist

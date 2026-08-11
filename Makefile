# kelson build tooling (issue #18).

GO      ?= go
GOLANGCI?= golangci-lint
BIN     ?= $(CURDIR)/bin

.PHONY: all build test lint fmt clean install

all: lint test build

build:
	$(GO) build ./...

test:
	$(GO) test -race ./...

lint:
	$(GOLANGCI) run ./...

fmt:
	gofmt -l -w .

install: # install golangci-lint if missing
	command -v $(GOLANGCI) >/dev/null 2>&1 || $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

clean:
	$(GO) clean ./...

.PHONY: build run test clean install release

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
EXE := $(shell go env GOEXE)
BIN := bin/cfgate-cc$(EXE)
GOBIN := $(shell go env GOBIN)
GOPATH := $(shell go env GOPATH)
INSTALL_DIR := $(if $(GOBIN),$(GOBIN),$(GOPATH)/bin)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/cfgate-cc

run:
	go run ./cmd/cfgate-cc

test:
	go test ./...

clean:
	rm -rf bin

install: build
	mkdir -p "$(INSTALL_DIR)"
	install -m 0755 "$(BIN)" "$(INSTALL_DIR)/cfgate-cc$(EXE)"

release:
	@[ -n "$(TAG)" ] || (echo "Usage: make release TAG=v0.1.0" && exit 1)
	./scripts/release.sh "$(TAG)"

.PHONY: build run test vet

VERSION ?= $(shell git describe --tags --always --dirty)

build:
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o weknora-mcp .

run: build
	./weknora-mcp

test:
	go vet ./... && go test ./...

vet:
	go vet ./...

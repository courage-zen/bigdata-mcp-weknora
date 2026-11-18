.PHONY: build run test vet

build:
	go build -trimpath -ldflags='-s -w' -o weknora-mcp .

run: build
	./weknora-mcp

test:
	go vet ./... && go test ./...

vet:
	go vet ./...

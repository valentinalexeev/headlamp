.PHONY: build test vet

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/node-control ./cmd/node-control

test:
	go test ./...

vet:
	go vet ./...

.PHONY: build test vet demo

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/node-control ./cmd/node-control

test:
	go test ./...

vet:
	go vet ./...

# Serve the console on 127.0.0.1:8099 with invented data (SCENARIO=attention|clear|minimal|partial).
SCENARIO ?= attention
demo:
	go run ./cmd/console-demo -scenario $(SCENARIO)

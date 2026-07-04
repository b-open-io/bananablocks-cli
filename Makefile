VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-s -w -X github.com/b-open-io/bananablocks-cli/cmd.Version=$(VERSION)"

.PHONY: build test lint install clean

build:
	go build $(LDFLAGS) -o bin/bb .

test:
	go test ./...

lint:
	golangci-lint run

install:
	go install $(LDFLAGS) .

clean:
	rm -rf bin

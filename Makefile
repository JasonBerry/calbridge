VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build vet test race lint check bin clean

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run

check: build vet test lint

bin:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/calbridge ./cmd/calbridge

clean:
	rm -rf bin

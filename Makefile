VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w \
  -X github.com/skkap/yad/internal/buildinfo.Version=$(VERSION) \
  -X github.com/skkap/yad/internal/buildinfo.Commit=$(COMMIT)

.PHONY: build test lint check install dist clean

build:
	go build -ldflags '$(LDFLAGS)' -o bin/yad ./cmd/yad

test:
	go test ./...

lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

check: lint test build

install: build
	install -m 0755 bin/yad $(HOME)/.local/bin/yad

# The fleet is Linux; the laptop is macOS. Both are built from the laptop.
dist:
	GOOS=linux  GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o dist/yad-linux-amd64  ./cmd/yad
	GOOS=linux  GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o dist/yad-linux-arm64  ./cmd/yad
	GOOS=darwin GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o dist/yad-darwin-arm64 ./cmd/yad

clean:
	rm -rf bin dist

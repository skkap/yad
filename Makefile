VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w \
  -X github.com/skkap/yad/internal/buildinfo.Version=$(VERSION) \
  -X github.com/skkap/yad/internal/buildinfo.Commit=$(COMMIT)

# Pure Go everywhere: modernc SQLite needs no cgo, so one laptop builds every
# target without a cross toolchain.
export CGO_ENABLED := 0

# Files `make generate` owns. check-generated fails when any of them differ from
# what the code produces, including when one is new and uncommitted.
GENERATED := internal/store/db internal/hub/store/db protocol/v1/openapi.yaml protocol/hubapi/openapi.yaml

TARGETS := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64

.PHONY: fmt lint test build generate check-generated cross check install dist clean smoke

fmt:
	gofmt -w .

lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go tool staticcheck ./...

# -race needs cgo, so the tests are the one place it is switched back on.
test:
	CGO_ENABLED=1 go test -race ./...

build:
	go build -ldflags '$(LDFLAGS)' -o bin/yad ./cmd/yad

generate:
	cd internal/store && sqlc generate
	cd internal/hub/store && sqlc generate
	go run ./internal/hub/cmd/openapigen protocol/v1/openapi.yaml protocol/hubapi/openapi.yaml

check-generated: generate
	@git diff --exit-code -- $(GENERATED) || { echo "generated files are stale — run 'make generate' and commit"; exit 1; }
	@untracked=$$(git ls-files --others --exclude-standard -- $(GENERATED)); \
	  if [ -n "$$untracked" ]; then echo "generated files not committed:"; echo "$$untracked"; exit 1; fi

# Compiles every release target without writing binaries: a build that only
# works on the laptop is a broken build.
cross:
	@for t in $(TARGETS); do \
	  echo "go build $$t"; \
	  GOOS=$${t%/*} GOARCH=$${t#*/} go build -o /dev/null ./... || exit 1; \
	done

check: lint test build check-generated cross

# One real Claude run through yad hub (scripts/smoke.sh). It spends a few cents
# of the logged-in account, so it is run by hand and never by CI or make check.
smoke: build
	./scripts/smoke.sh

install: build
	install -m 0755 bin/yad $(HOME)/.local/bin/yad

dist:
	@for t in $(TARGETS); do \
	  GOOS=$${t%/*} GOARCH=$${t#*/} go build -ldflags '$(LDFLAGS)' -o dist/yad-$${t%/*}-$${t#*/} ./cmd/yad || exit 1; \
	done

clean:
	rm -rf bin dist

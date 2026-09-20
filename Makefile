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

.PHONY: fmt lint test build generate check-generated cross check ci install dist clean smoke smoke-codex

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

# Compiles every release target without writing binaries, and vets it — vet
# type-checks the test files too, which a build never sees: a build or a test
# that only works on the laptop is a broken one. A darwin-only symbol such as
# syscall.Getsid passes every host check on a Mac and fails CI on Linux.
cross:
	@for t in $(TARGETS); do \
	  echo "go build + vet $$t"; \
	  GOOS=$${t%/*} GOARCH=$${t#*/} go build -o /dev/null ./... || exit 1; \
	  GOOS=$${t%/*} GOARCH=$${t#*/} go vet ./... || exit 1; \
	done

check: lint test build check-generated cross

# What CI runs, and only that. CI's one advantage over a laptop here is that it
# is a clean Linux machine, so it runs the things whose answer can differ there
# — vet, a build, and the suite — and nothing whose answer cannot.
#
# gofmt, staticcheck, check-generated and cross give the same answer on any
# machine, so running them twice buys nothing; `make check` is where they live
# and CHECKS.md makes passing it the bar before a pull request.
#
# vet is here despite being machine-independent because it type-checks test
# files, which a build never does — twice this has caught a merge that was
# textually clean and did not compile.
#
# -race is deliberately absent: it needs cgo, roughly triples the suite, and
# `make check` runs it on every change before the branch is pushed.
ci:
	go vet ./...
	go build -ldflags '$(LDFLAGS)' -o /dev/null ./cmd/yad
	go test ./...

# One real run through yad hub (scripts/smoke.sh), with Claude or with Codex.
# Each spends a few cents of the logged-in account, so they are run by hand and
# never by CI or make check.
smoke: build
	./scripts/smoke.sh claude

smoke-codex: build
	./scripts/smoke.sh codex

install: build
	install -m 0755 bin/yad $(HOME)/.local/bin/yad

dist:
	@for t in $(TARGETS); do \
	  GOOS=$${t%/*} GOARCH=$${t#*/} go build -ldflags '$(LDFLAGS)' -o dist/yad-$${t%/*}-$${t#*/} ./cmd/yad || exit 1; \
	done

clean:
	rm -rf bin dist

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

.PHONY: fmt lint test build generate check-generated check-breaking cross check ci install dist clean smoke smoke-codex

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

# Compares both committed OpenAPI documents against themselves at the last
# release tag and fails on a change that breaks a generated client. Inert, and
# says so, until the owner pushes the first v[0-9]* tag. scripts/breaking.sh
# holds the reasoning; it runs after check-generated so the documents it reads
# are known to match the Go types. Its one known false alarm — a grown enum,
# which §2 Versioning makes safe here — is DEV-87, named in the script too.
check-breaking:
	@./scripts/breaking.sh

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

check: lint test build check-generated check-breaking cross

# What CI runs, and only that. CI's one advantage over a laptop here is that it
# is a clean Linux machine, so it runs the things whose answer can differ there
# — vet, a build, and the suite — plus two deliberate exceptions named below.
#
# gofmt, staticcheck, check-generated and cross give the same answer on any
# machine, so running them twice buys nothing; `make check` is where they live
# and CHECKS.md makes passing it the bar before a pull request.
#
# vet is the first exception: machine-independent, but it type-checks test
# files, which a build never does — twice this has caught a merge that was
# textually clean and did not compile.
#
# check-breaking is the second, and it is an exception to the *principle* and
# not just to the list. oasdiff answers the same on any machine, so the rule
# above says it belongs in `make check` alone. It is here as well because of
# who it protects: every other check in this file protects the person running
# it, and a broken one costs that person an hour. This one protects every hub
# already generated from a released document — software on machines nobody
# here can reach, whose author finds out at runtime. CHECKS.md is the bar, and
# CHECKS.md cannot bind a contributor who did not read it; the cost of running
# it twice is seconds, and the cost of the one time it is skipped is somebody
# else's outage. It needs tags to have a baseline, so ci.yml checks out with
# fetch-depth: 0 — without that it would find no tag and pass for ever.
#
# -race is deliberately absent: it needs cgo, roughly triples the suite, and
# `make check` runs it on every change before the branch is pushed.
ci:
	go vet ./...
	go build -ldflags '$(LDFLAGS)' -o /dev/null ./cmd/yad
	go test ./...
	$(MAKE) check-breaking

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

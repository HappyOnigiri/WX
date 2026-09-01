GO ?= go
INSTALL_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildMeta=dev
CI_JOBS ?= $(shell sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)
CI_MAKEFLAGS := -j$(CI_JOBS) --keep-going $(if $(filter output-sync,$(.FEATURES)),--output-sync=target)
GOLANGCI_VERSION ?= v2.11.0
DEADCODE_VERSION ?= v0.49.0
COVERAGE_MIN ?= 85
CORE_COVERAGE_MIN ?= 90

.PHONY: build install fmt fmt-check vet lint deadcode mod-tidy-check test test-race test-race-coverage coverage-check build-darwin reproducible-build smoke ci ci-checks hooks-install hook-pre-commit hook-pre-push clean

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/wx ./cmd/wx

install: build
	install -d "$(INSTALL_DIR)"
	install -m 0755 bin/wx "$(INSTALL_DIR)/wx"

fmt:
	gofmt -w cmd internal migrations

fmt-check:
	@test -z "$$(gofmt -l cmd internal migrations)" || { gofmt -l cmd internal migrations; echo "run make fmt"; exit 1; }

vet:
	$(GO) vet ./...

lint:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run ./...

deadcode:
	@output="$$($(GO) run golang.org/x/tools/cmd/deadcode@$(DEADCODE_VERSION) -test ./...)"; \
	if [ -n "$$output" ]; then printf '%s\n' "$$output"; exit 1; fi

mod-tidy-check:
	$(GO) mod tidy -diff

test:
	$(GO) test -shuffle=on -count=1 ./...

test-race:
	$(GO) test -race -shuffle=on -count=1 ./...

test-race-coverage:
	@mkdir -p coverage
	$(GO) test -race -shuffle=on -count=1 -covermode=atomic -coverpkg=./... -coverprofile=coverage/all.out ./...

coverage-check: test-race-coverage
	$(GO) run ./tools/checkcoverage -profile coverage/all.out -overall $(COVERAGE_MIN) -core $(CORE_COVERAGE_MIN)

build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o bin/wx-darwin-arm64 ./cmd/wx
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -o bin/wx-darwin-amd64 ./cmd/wx

reproducible-build:
	@first="$$(mktemp -d)"; second="$$(mktemp -d)"; \
	trap 'rm -rf "$$first" "$$second"' EXIT; \
	TZ=UTC LC_ALL=C CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$first/wx" ./cmd/wx; \
	TZ=UTC LC_ALL=C CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$second/wx" ./cmd/wx; \
	cmp "$$first/wx" "$$second/wx"; \
	$(GO) version -m "$$first/wx"

smoke: build
	./bin/wx --help >/dev/null
	./bin/wx --version | grep -q '^wx version '

ci:
	$(MAKE) $(CI_MAKEFLAGS) ci-checks

ci-checks: fmt-check lint deadcode mod-tidy-check coverage-check build-darwin reproducible-build smoke

hooks-install:
	git config --local core.hooksPath .githooks

hook-pre-commit: fmt-check vet test

hook-pre-push: fmt-check vet test

clean:
	$(GO) clean -testcache

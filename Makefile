GO ?= go
INSTALL_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildMeta=dev
CI_JOBS ?= $(shell sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)
CI_MAKEFLAGS := -j$(CI_JOBS) --keep-going $(if $(filter output-sync,$(.FEATURES)),--output-sync=target)

.PHONY: build install fmt fmt-check vet mod-tidy-check test test-race build-darwin ci ci-checks hooks-install hook-pre-commit hook-pre-push clean

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

mod-tidy-check:
	$(GO) mod tidy -diff

test:
	$(GO) test -shuffle=on -count=1 ./...

test-race:
	$(GO) test -race -shuffle=on -count=1 ./...

build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o bin/wx-darwin-arm64 ./cmd/wx
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -o bin/wx-darwin-amd64 ./cmd/wx

ci:
	$(MAKE) $(CI_MAKEFLAGS) ci-checks

ci-checks: fmt-check vet mod-tidy-check test-race build-darwin

hooks-install:
	git config --local core.hooksPath .githooks

hook-pre-commit: fmt-check vet test

hook-pre-push: fmt-check vet test

clean:
	$(GO) clean -testcache

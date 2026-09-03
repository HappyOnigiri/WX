GO ?= go
INSTALL_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildMeta=dev
CI_JOBS ?= $(shell sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)
CI_MAKEFLAGS := -j$(CI_JOBS) --keep-going $(if $(filter output-sync,$(.FEATURES)),--output-sync=target)
TOOLS_DIR := $(CURDIR)/.tools
TOOLS_BIN := $(TOOLS_DIR)/bin
NPM_BIN := $(TOOLS_DIR)/npm/node_modules/.bin
GOLANGCI_VERSION ?= v2.13.2
DEADCODE_VERSION ?= v0.49.0
GOFUMPT_VERSION ?= v0.11.0
GCI_VERSION ?= v0.14.0
ACTIONLINT_VERSION ?= v1.7.12
GOVULNCHECK_VERSION ?= v1.7.0
OSV_SCANNER_VERSION ?= v2.3.8
GOSEC_VERSION ?= v2.29.0
GITLEAKS_VERSION ?= v8.30.1
GO_LICENSES_VERSION ?= v2.0.1
CYCLONEDX_VERSION ?= v1.12.0
GREMLINS_VERSION ?= v0.6.0
MARKDOWNLINT_VERSION ?= 0.23.2
ZIZMOR_VERSION ?= 1.30.0
SHELLCHECK_VERSION ?= 0.11.0
COVERAGE_MIN ?= 85
CORE_COVERAGE_MIN ?= 90
CHANGED_COVERAGE_MIN ?= 90
COVERAGE_BASE ?= origin/main
COVERAGE_EXCLUSIONS ?= coverage-exclusions.txt
MUTATION_BASE ?= origin/main
MUTATION_MIN ?= 80
MUTATION_CORE_PACKAGES := ./internal/config ./internal/state ./internal/pool ./internal/gitx ./internal/workspace ./internal/archive ./internal/rpc
LICENSE_ALLOWLIST := Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT,MPL-2.0,Unicode-3.0,Unlicense
# wx intentionally executes Git, launchctl, and user-configured prepare commands
# against ownership-validated paths. These generic rules cannot model those
# trust boundaries; this exclusion list is used only by the explicitly invoked
# `gosec` target, which is paused out of default setup, CI, and hooks.
GOSEC_EXCLUDES := G104,G115,G202,G204,G302,G304,G306

.PHONY: setup setup-go-tools setup-external-tools setup-security-tools setup-sbom-tools setup-mutation-tools setup-markdownlint setup-zizmor check-shellcheck build install fmt fmt-check vet lint deadcode mod-tidy-check generated-check docs-check workflow-check workflow-lint workflow-security-audit shell-check test test-race test-race-coverage coverage-check changed-coverage-check portable-test integration-state integration-git integration-daemon integration-launchd concurrency-test build-darwin reproducible-build smoke govulncheck dependency-check gosec license-check secret-check sbom mutation-check mutation-full-check mutation-run security-local ci ci-checks hooks-install hooks-test hook-pre-commit hook-pre-push nightly-race fuzz fault-check crash-check soak-check resource-leak-check benchmark-check clean

# hooks-install is a dependency (not just documented separately) so that the
# README's `make setup && make ci` sequence succeeds on a brand-new clone or
# worktree: `ci-checks` runs hooks-test, which hard-fails unless
# core.hooksPath is already set to .githooks.
setup: setup-go-tools setup-external-tools hooks-install

setup-go-tools:
	mkdir -p "$(TOOLS_BIN)"
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install golang.org/x/tools/cmd/deadcode@$(DEADCODE_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/daixiang0/gci@$(GCI_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

setup-external-tools: setup-markdownlint check-shellcheck

# These setup targets are intentionally not dependencies of `setup`, CI, or
# hooks. Invoke the corresponding check explicitly only after the user grants
# permission to opt back into the paused security, SBOM, or mutation tooling.
setup-security-tools:
	mkdir -p "$(TOOLS_BIN)"
	GOBIN="$(TOOLS_BIN)" $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/google/osv-scanner/v2/cmd/osv-scanner@$(OSV_SCANNER_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)

setup-sbom-tools:
	mkdir -p "$(TOOLS_BIN)"
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_VERSION)

setup-mutation-tools:
	mkdir -p "$(TOOLS_BIN)"
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)

setup-markdownlint:
	command -v npm >/dev/null
	npm install --silent --no-audit --no-fund --prefix "$(TOOLS_DIR)/npm" markdownlint-cli2@$(MARKDOWNLINT_VERSION)

setup-zizmor:
	mkdir -p "$(TOOLS_BIN)"
	@if command -v uv >/dev/null; then \
	  UV_TOOL_BIN_DIR="$(TOOLS_BIN)" uv tool install --force zizmor==$(ZIZMOR_VERSION); \
	elif command -v pipx >/dev/null; then \
	  PIPX_BIN_DIR="$(TOOLS_BIN)" pipx install --force zizmor==$(ZIZMOR_VERSION); \
	else \
	  echo "uv or pipx is required to install zizmor"; exit 1; \
	fi

check-shellcheck:
	command -v shellcheck >/dev/null
	shellcheck --version | grep -q 'version: $(SHELLCHECK_VERSION)'

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/wx ./cmd/wx

install: build
	install -d "$(INSTALL_DIR)"
	install -m 0755 bin/wx "$(INSTALL_DIR)/wx"

fmt:
	"$(TOOLS_BIN)/gofumpt" -w cmd internal migrations tools
	find cmd internal tools -type f -name '*.go' -print0 | xargs -0 "$(TOOLS_BIN)/gci" write -s standard -s default -s "prefix(github.com/HappyOnigiri/WX)"

fmt-check:
	@test -x "$(TOOLS_BIN)/gofumpt" -a -x "$(TOOLS_BIN)/gci" || { echo "pinned formatters are missing; run make setup"; exit 1; }
	@test -z "$$($(TOOLS_BIN)/gofumpt -l cmd internal migrations tools)" || { $(TOOLS_BIN)/gofumpt -l cmd internal migrations tools; echo "run make fmt"; exit 1; }
	find cmd internal tools -type f -name '*.go' -print0 | xargs -0 "$(TOOLS_BIN)/gci" diff -s standard -s default -s "prefix(github.com/HappyOnigiri/WX)"

vet:
	$(GO) vet ./...

lint:
	@test -x "$(TOOLS_BIN)/golangci-lint" || { echo "pinned linter is missing; run make setup"; exit 1; }
	"$(TOOLS_BIN)/golangci-lint" run ./...

deadcode:
	@test -x "$(TOOLS_BIN)/deadcode" || { echo "pinned deadcode checker is missing; run make setup"; exit 1; }
	@output="$$($(TOOLS_BIN)/deadcode -test ./...)"; \
	if [ -n "$$output" ]; then printf '%s\n' "$$output"; exit 1; fi

mod-tidy-check:
	$(GO) mod tidy -diff
	$(GO) mod verify

# No package in this repository currently declares a //go:generate directive:
# help text, config schema, SQLite migrations, the LaunchAgent plist, and
# shell completion are all hand-maintained. `go generate ./...` therefore has
# nothing to run today, so this check is green by construction and does not
# yet catch generated-artifact drift; it is kept as a required CI check
# (rather than removed) so that adding a real //go:generate directive to any
# of those sources starts enforcing it immediately, with no further wiring.
#
# The comparison itself runs entirely inside two temporary directories rather
# than the working tree: `before` is an untouched `git archive HEAD` copy,
# `after` is a duplicate of it with `go generate ./...` run inside. Neither
# copy is the real checkout, so this never leaves the working tree dirty
# (unlike running `go generate` in place and `git diff --exit-code`-ing it),
# and the diff is limited to exactly what generation changed rather than
# also picking up unrelated working-tree state (.tools, coverage output, a
# developer's own uncommitted edits).
generated-check:
	@before="$$(mktemp -d)"; after="$$(mktemp -d)"; \
	trap 'rm -rf "$$before" "$$after"' EXIT; \
	git archive --format=tar HEAD | (cd "$$before" && tar -x); \
	cp -a "$$before/." "$$after/"; \
	(cd "$$after" && $(GO) generate ./...); \
	if ! diff -rq "$$before" "$$after"; then \
		echo "generated artifacts (help text, config schema, SQLite migrations, LaunchAgent plist, shell completion) are stale relative to their generators; run 'go generate ./...' and commit the result" >&2; \
		exit 1; \
	fi

docs-check:
	@test -x "$(NPM_BIN)/markdownlint-cli2" || { echo "pinned markdownlint is missing; run make setup"; exit 1; }
	"$(NPM_BIN)/markdownlint-cli2" README.md '**/*.md' '#.tools/**' '#tmp/**'

workflow-check: workflow-lint

workflow-lint:
	@test -x "$(TOOLS_BIN)/actionlint" || { echo "pinned actionlint is missing; run make setup"; exit 1; }
	"$(TOOLS_BIN)/actionlint"

workflow-security-audit: setup-zizmor
	@test -x "$(TOOLS_BIN)/zizmor" || { echo "pinned zizmor is missing; run make setup-zizmor"; exit 1; }
	"$(TOOLS_BIN)/zizmor" --pedantic --min-severity medium .github

shell-check:
	@shellcheck --version | grep -q 'version: $(SHELLCHECK_VERSION)' || { echo "ShellCheck $(SHELLCHECK_VERSION) is required; run make setup"; exit 1; }
	shellcheck .githooks/pre-commit .githooks/pre-push scripts/*.sh

test:
	$(GO) test -shuffle=on -count=1 ./...

test-race:
	$(GO) test -race -shuffle=on -count=1 ./...

test-race-coverage:
	@mkdir -p coverage
	$(GO) test -race -shuffle=on -count=1 -covermode=atomic -coverpkg=./... -coverprofile=coverage/all.out ./...

coverage-check: test-race-coverage
	$(GO) run ./tools/checkcoverage -profile coverage/all.out -exclusions $(COVERAGE_EXCLUSIONS) -overall $(COVERAGE_MIN) -core $(CORE_COVERAGE_MIN)

changed-coverage-check: coverage-check
	$(GO) run ./tools/checkchangedcoverage -profile coverage/all.out -exclusions $(COVERAGE_EXCLUSIONS) -base $(COVERAGE_BASE) -minimum $(CHANGED_COVERAGE_MIN)

portable-test:
	$(GO) test -shuffle=on -count=1 ./...

integration-state:
	$(GO) test -race -shuffle=on -count=1 ./internal/state

integration-git:
	$(GO) test -race -shuffle=on -count=1 ./internal/discovery ./internal/gitx ./internal/pool ./internal/workspace ./internal/archive

integration-daemon:
	$(GO) test -race -shuffle=on -count=1 ./internal/daemon ./internal/rpc ./internal/cli ./internal/agent

integration-launchd:
	$(GO) test -race -shuffle=on -count=1 ./internal/launchd ./cmd/wx

concurrency-test:
	$(GO) test -race -shuffle=on -count=10 -timeout=15m ./internal/state ./internal/daemon -run 'Lease|Concurrent|Crash|Archive|Remove|Worker'

build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o bin/wx-darwin-arm64 ./cmd/wx
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -o bin/wx-darwin-amd64 ./cmd/wx

reproducible-build:
	@scratch="$$(mktemp -d)"; trap 'rm -rf "$$scratch"' EXIT; \
	for arch in arm64 amd64; do \
	  mkdir -p "$$scratch/first" "$$scratch/second"; \
	  TZ=UTC LC_ALL=C CGO_ENABLED=0 GOOS=darwin GOARCH="$$arch" $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$scratch/first/wx-$$arch" ./cmd/wx; \
	  TZ=UTC LC_ALL=C CGO_ENABLED=0 GOOS=darwin GOARCH="$$arch" $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$scratch/second/wx-$$arch" ./cmd/wx; \
	  cmp "$$scratch/first/wx-$$arch" "$$scratch/second/wx-$$arch"; \
	  $(GO) version -m "$$scratch/first/wx-$$arch"; \
	done

smoke: build
	./bin/wx --help >/dev/null
	./bin/wx --version | grep -q '^wx version '
	$(GO) test ./internal/rpc -run TestClientServerRoundTripWithoutParentDeadline -count=1
	@destination="$$(mktemp -d)"; $(MAKE) install INSTALL_DIR="$$destination"; "$$destination/wx" --version >/dev/null

govulncheck: setup-security-tools
	@test -x "$(TOOLS_BIN)/govulncheck" || { echo "pinned govulncheck is missing; run make setup-security-tools"; exit 1; }
	"$(TOOLS_BIN)/govulncheck" ./...

dependency-check: setup-security-tools
	@test -x "$(TOOLS_BIN)/osv-scanner" || { echo "pinned OSV scanner is missing; run make setup-security-tools"; exit 1; }
	"$(TOOLS_BIN)/osv-scanner" scan source --lockfile=go.mod --all-vulns .

gosec: setup-security-tools
	@test -x "$(TOOLS_BIN)/gosec" || { echo "pinned gosec is missing; run make setup-security-tools"; exit 1; }
	"$(TOOLS_BIN)/gosec" -quiet -exclude="$(GOSEC_EXCLUDES)" -nosec-require-justification -nosec-require-rules ./...

license-check: setup-security-tools
	@test -x "$(TOOLS_BIN)/go-licenses" || { echo "pinned license checker is missing; run make setup-security-tools"; exit 1; }
	@# go-licenses v2 misclassifies Go 1.26 standard packages as modules;
	@# ignore the explicit `go list std` set while still resolving their dependencies.
	@$(GO) list std | sed 's/^/--ignore=/' | xargs "$(TOOLS_BIN)/go-licenses" check ./... \
	  --ignore=github.com/HappyOnigiri/WX --allowed_licenses="$(LICENSE_ALLOWLIST)"

secret-check: setup-security-tools
	@test -x "$(TOOLS_BIN)/gitleaks" || { echo "pinned secret scanner is missing; run make setup-security-tools"; exit 1; }
	"$(TOOLS_BIN)/gitleaks" git --redact --no-banner .
	"$(TOOLS_BIN)/gitleaks" dir --redact --no-banner .

sbom: setup-sbom-tools
	@test -x "$(TOOLS_BIN)/cyclonedx-gomod" || { echo "pinned SBOM generator is missing; run make setup-sbom-tools"; exit 1; }
	mkdir -p artifacts
	"$(TOOLS_BIN)/cyclonedx-gomod" mod -json -output artifacts/sbom.json

mutation-check: setup-mutation-tools
	@test -x "$(TOOLS_BIN)/gremlins" || { echo "pinned mutation tool is missing; run make setup-mutation-tools"; exit 1; }
	@packages="$$(git diff --name-only "$(MUTATION_BASE)...HEAD" -- 'internal/**/*.go' | awk -F/ 'NF >= 3 && $$2 ~ /^(config|state|pool|gitx|workspace|archive|rpc)$$/ { print "./" $$1 "/" $$2 }' | sort -u | tr '\n' ' ')"; \
	if [ -z "$$packages" ]; then echo "no changed core packages"; exit 0; fi; \
	$(MAKE) mutation-run MUTATION_PACKAGES="$$packages"

mutation-full-check: setup-mutation-tools
	@$(MAKE) mutation-run MUTATION_PACKAGES="$(MUTATION_CORE_PACKAGES)"

mutation-run: setup-mutation-tools
	@test -x "$(TOOLS_BIN)/gremlins" || { echo "pinned mutation tool is missing; run make setup-mutation-tools"; exit 1; }
	@set -eu; scratch="$$(mktemp -d)"; trap 'rm -rf "$$scratch"' EXIT; worktree="$$scratch/repository"; \
	git clone --quiet --no-local . "$$worktree"; \
	for package in $(MUTATION_PACKAGES); do \
	  name="$$(printf '%s' "$$package" | tr '/.' '__')"; result="$$scratch/mutation-$$name.json"; \
	  echo "mutation testing $$package"; \
	  (cd "$$worktree" && $(GO) clean -testcache && "$(TOOLS_BIN)/gremlins" unleash "$$package" --integration --timeout-coefficient 10 --threshold-efficacy "$(MUTATION_MIN)" --output "$$result"); \
	  scripts/check-mutation-survivors.sh "$$package" "$$result" "$(MUTATION_MIN)"; \
	done

# Security, SBOM, and mutation targets are manual opt-in checks. Keep them out
# of CI and hooks until the user explicitly authorizes re-enabling automation.
security-local: setup-security-tools govulncheck dependency-check gosec license-check secret-check

ci:
	$(MAKE) $(CI_MAKEFLAGS) ci-checks

ci-checks: fmt-check lint deadcode mod-tidy-check generated-check docs-check workflow-check shell-check changed-coverage-check portable-test integration-state integration-git integration-daemon integration-launchd concurrency-test build-darwin reproducible-build smoke hooks-test

hooks-install:
	git config --local core.hooksPath .githooks

hooks-test:
	scripts/test-hooks.sh

hook-pre-commit:
	scripts/hook-check.sh pre-commit

hook-pre-push:
	scripts/hook-check.sh pre-push

# -timeout is explicit here (rather than Go's default 10m/package) because
# -count=10 over the full module makes internal/daemon alone run well past 10
# minutes; measured ~14-18 minutes standalone, so 45m leaves headroom while
# still comfortably fitting the 90-minute nightly job budget.
nightly-race:
	$(GO) test -race -shuffle=on -count=10 -timeout=45m ./...

fuzz:
	$(GO) test -run=^$$ -fuzz=. -fuzztime=60s ./internal/config ./internal/rpc ./internal/agent ./internal/domain ./internal/archive

fault-check:
	$(GO) test -race ./internal/state ./internal/daemon -run 'Fault|FailsClosed|Damaged|Corrupt|Rollback' -count=5 -timeout=3m

crash-check:
	$(GO) test -race ./internal/daemon -run 'Crash|Reconcile|Recovery' -count=10 -timeout=10m

soak-check:
	WX_SOAK_SESSIONS=300 $(GO) test -race ./internal/daemon -run TestSessionLifecycleSoak -count=1 -timeout=15m

resource-leak-check:
	$(GO) test -race ./internal/daemon ./internal/rpc -run 'Leak|Close|Stops' -count=20 -timeout=5m

benchmark-check:
	$(GO) test -run=^$$ -bench=. -benchmem ./internal/daemon ./internal/archive

clean:
	$(GO) clean -testcache

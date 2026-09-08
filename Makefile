GO ?= go
INSTALL_DIR ?= $(HOME)/.local/bin
RELEASE_DIR ?= artifacts/release
# バージョンの真実源はリリースタグ（vX.Y.Z）である。
# 他のタグを起点に選ばないよう--matchで絞り、タグを取得していないcheckoutではコミットへ退避する。
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/HappyOnigiri/WX/internal/version.Version=$(VERSION) -X github.com/HappyOnigiri/WX/internal/version.BuildMeta=dev
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
MARKDOWNLINT_VERSION ?= 0.23.2
ZIZMOR_VERSION ?= 1.30.0
SHELLCHECK_VERSION ?= 0.11.0
COVERAGE_MIN ?= 80
CORE_COVERAGE_MIN ?= 85
COVERAGE_EXCLUSIONS ?= coverage-exclusions.txt
RACE_TEST_ARGS := -race -shuffle=on -count=1
RACE_DAEMON_PACKAGE := ./internal/daemon
# darwin専用テストは internal/workspace にしかない。テスト名の一覧を二重管理せずパッケージ単位で実行する。
DARWIN_TEST_PACKAGE := ./internal/workspace
LICENSE_ALLOWLIST := Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT,MPL-2.0,Unicode-3.0,Unlicense
# 所有権を検証したパスでGit・launchctl・prepareコマンドを実行する。
# 汎用ルールではこの信頼境界を表せないため、明示実行するgosecだけで除外する。
GOSEC_EXCLUDES := G104,G115,G202,G204,G302,G304,G306

.PHONY: setup setup-go-tools setup-external-tools setup-security-tools setup-sbom-tools setup-markdownlint setup-zizmor check-shellcheck build install fmt fmt-check vet lint deadcode mod-tidy-check generated-check docs-check comments-check tests-check lines-check testlayout-check fuzz-check gitexec-check migrations-check workflow-check workflow-lint workflow-security-audit shell-check static-check test test-race test-race-daemon test-race-rest ci-test-race test-coverage test-race-coverage coverage-check portable-test test-focus test-darwin check-fast concurrency-test build-darwin reproducible-build version-check smoke govulncheck dependency-check gosec license-check secret-check sbom security-local ci ci-checks hook-pre-commit nightly-race fuzz fault-check crash-check soak-check resource-leak-check clean

setup: setup-go-tools setup-external-tools

setup-go-tools:
	mkdir -p "$(TOOLS_BIN)"
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install golang.org/x/tools/cmd/deadcode@$(DEADCODE_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/daixiang0/gci@$(GCI_VERSION)
	GOBIN="$(TOOLS_BIN)" $(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

setup-external-tools: setup-markdownlint check-shellcheck

# セキュリティ・SBOMの再開はユーザーの明示許可後に手動実行する。
# setup・CI・hookの依存には含めない。
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

# リリース版は明示したタグでのみ生成し、開発用 build/install の -dev を維持する。
.PHONY: release release-check
release: export GO := $(GO)
release: export RELEASE_VERSION := $(RELEASE_VERSION)
release: export RELEASE_DIR := $(RELEASE_DIR)
release:
	bash scripts/build-release.sh

# Linux では対象 OS/arch とチェックサム、macOS arm64 では埋め込み版の実行結果も検査する。
release-check:
	@set -eu; directory="$$(mktemp -d)"; trap 'rm -rf "$$directory"' EXIT; \
	$(MAKE) release RELEASE_VERSION=v0.0.0 RELEASE_DIR="$$directory"; \
	$(GO) version -m "$$directory/wx-darwin-arm64" > "$$directory/build-info"; \
	grep -F 'CGO_ENABLED=0' "$$directory/build-info"; \
	grep -F 'GOOS=darwin' "$$directory/build-info"; \
	grep -F 'GOARCH=arm64' "$$directory/build-info"; \
	(cd "$$directory" && shasum -a 256 -c checksums.txt); \
	test -s "$$directory/install.sh"; \
	test -s "$$directory/uninstall.sh"; \
	if [ "$$(uname -sm)" = 'Darwin arm64' ]; then \
	  test "$$("$$directory/wx-darwin-arm64" --version)" = 'wx version v0.0.0'; \
	fi

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

# 現在は全て手書きで、go:generateがないため生成物の差分検出は行われず、ci-checksからは外してある。
# go:generateを追加したらci-checksの行に1行戻す。
# 生成前の作業ツリーを一時indexへ保存し、生成後に同じindexを更新して比較する。
# 実index・利用者の差分・生成前からあるuntrackedは変更せず、生成が加えた差分だけを検出する。
generated-check:
	@set -eu; \
	index="$$(git rev-parse --git-path index)"; \
	before="$$(mktemp)"; after="$$(mktemp)"; temporary_index="$$(mktemp)"; \
	trap 'rm -f "$$before" "$$after" "$$temporary_index"' EXIT; \
	cp "$$index" "$$temporary_index"; \
	GIT_INDEX_FILE="$$temporary_index" git add -A; \
	GIT_INDEX_FILE="$$temporary_index" git ls-files --stage -z > "$$before"; \
	$(GO) generate ./...; \
	GIT_INDEX_FILE="$$temporary_index" git add -A; \
	GIT_INDEX_FILE="$$temporary_index" git ls-files --stage -z > "$$after"; \
	if ! cmp -s "$$before" "$$after"; then \
		echo "go generate changed the working tree; generated artifacts are stale" >&2; \
		exit 1; \
	fi

# エージェント用worktreeやnpmの生成物はroot直下に限らず現れるため、深さに依存しないパターンで除外する。
# *.local.mdは各利用者のローカル指示で追跡しないため、リポジトリの書式規約の対象から外す。
docs-check:
	@test -x "$(NPM_BIN)/markdownlint-cli2" || { echo "pinned markdownlint is missing; run make setup"; exit 1; }
	command -v node >/dev/null
	node tools/markdownlint/selftest.mjs "$(TOOLS_DIR)/npm/node_modules/markdownlint-cli2/export-markdownlint.mjs"
	"$(NPM_BIN)/markdownlint-cli2" README.md '**/*.md' '#.tools/**' '#tmp/**' '#.claude/**' '#**/node_modules/**' '#**/*.local.md'

comments-check:
	$(GO) run ./tools/checkcomments

# テストコードの規約は、AGENTS.mdでの指示ではなくこの検査で強制する。
tests-check:
	$(GO) run ./tools/checktests

# Go実装ファイルの行数上限。警告は報告のみで、上限超過だけがci-checksを落とす。
lines-check:
	$(GO) run ./tools/checklines

# 実装ファイルとテストファイルの対応。backlogは警告のみで、未登録の欠落だけがci-checksを落とす。
testlayout-check:
	$(GO) run ./tools/checktestlayout

# go test -fuzz は一致するターゲットが無くても成功するため、設定だけが残った空回りをこの検査で落とす。
fuzz-check:
	$(GO) run ./tools/checkfuzz

# Gitはinternal/gitx経由という不変条件のうち、本番コードからの直接起動だけを構文で検出する。
gitexec-check:
	$(GO) run ./tools/checkgitexec

# migrationの適用版はSQL名の辞書順の位置で決まり、SchemaVersionは手動で揃える。
# 番号の欠番・重複と定数の据え置きはDBを開くまで表面化しないため、この検査で落とす。
migrations-check:
	$(GO) run ./tools/checkmigrations

workflow-check: workflow-lint

workflow-lint:
	@test -x "$(TOOLS_BIN)/actionlint" || { echo "pinned actionlint is missing; run make setup"; exit 1; }
	"$(TOOLS_BIN)/actionlint"

workflow-security-audit: setup-zizmor
	@test -x "$(TOOLS_BIN)/zizmor" || { echo "pinned zizmor is missing; run make setup-zizmor"; exit 1; }
	"$(TOOLS_BIN)/zizmor" --pedantic --min-severity medium .github

shell-check:
	@shellcheck --version | grep -q 'version: $(SHELLCHECK_VERSION)' || { echo "ShellCheck $(SHELLCHECK_VERSION) is required; run make setup"; exit 1; }
	shellcheck scripts/*.sh

test:
	$(GO) test -shuffle=on -count=1 ./...

test-race:
	$(GO) test $(RACE_TEST_ARGS) ./...

# race検査はCIの少コアランナーでCPU律速になり、単独で最長のinternal/daemonがジョブの下限を作る。
# daemonと残りを別ジョブへ分けるため、対象パッケージだけが違う2つのtargetを用意する。
test-race-daemon:
	$(GO) test $(RACE_TEST_ARGS) $(RACE_DAEMON_PACKAGE)

test-race-rest:
	$(GO) test $(RACE_TEST_ARGS) $$($(GO) list ./... | grep -v '/internal/daemon$$')

# ci-checksは-jで並列に走るため、2つのテストスイートが同時に実行されると資源が枯渇して失敗する。
# coverage計測の完了を待たせ、同時実行だけを防ぐ。
ci-test-race: coverage-check
	$(GO) test $(RACE_TEST_ARGS) ./...

# race検査とcoverage計測はCIで別ジョブへ分けるため、race抜きの計測を通常経路にする。
# test-race-coverageは両者を同時に走らせた場合の比較・診断用に残す。
test-coverage:
	@mkdir -p coverage
	$(GO) test -shuffle=on -count=1 -covermode=atomic -coverpkg=./... -coverprofile=coverage/all.out ./...

test-race-coverage:
	@mkdir -p coverage
	$(GO) test -race -shuffle=on -count=1 -covermode=atomic -coverpkg=./... -coverprofile=coverage/all.out ./...

coverage-check: test-coverage
	$(GO) run ./tools/checkcoverage -profile coverage/all.out -exclusions $(COVERAGE_EXCLUSIONS) -overall $(COVERAGE_MIN) -core $(CORE_COVERAGE_MIN)

portable-test:
	$(GO) test -shuffle=on -count=1 ./...

# 編集直後に対象を絞って確認する入口。PKG・RUN・VERBOSEはrecipeのshellへ埋め込まず環境経由で渡す。
# 差分からの自動選択はせず、対象の指定はユーザーに委ねる。
export PKG
export RUN
export VERBOSE

test-focus:
	GO="$(GO)" scripts/test-focus.sh

# darwin専用実装を実機で確認する入口。build-darwinはビルドだけで、これらのテストは走らない。
# 実行環境とTMPDIRのfilesystemを表示し、非Darwin・クロス設定・非APFSはテスト前に失敗させる。
test-darwin:
	GO="$(GO)" PKG=$(DARWIN_TEST_PACKAGE) VERBOSE=1 scripts/test-darwin.sh

# Go編集時の構造検査だけを集めた短い入口。共有契約や書式・lintは含めず、最終判定はmake ciとする。
check-fast: comments-check tests-check lines-check testlayout-check migrations-check

concurrency-test:
	$(GO) test -race -shuffle=on -count=10 -timeout=15m ./internal/state ./internal/daemon -run 'Lease|Concurrent|Crash|Archive|Remove|Worker'

# amd64は配布・実行対象にしないため落とした。arm64のCGO_ENABLED=0ビルドは
# 通常のmake buildと異なる唯一のci-checks構成要素であり退行検出の実体なので残す。
build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/wx-darwin-arm64 ./cmd/wx

reproducible-build:
	@scratch="$$(mktemp -d)"; trap 'rm -rf "$$scratch"' EXIT; \
	for arch in arm64 amd64; do \
	  mkdir -p "$$scratch/first" "$$scratch/second"; \
	  TZ=UTC LC_ALL=C CGO_ENABLED=0 GOOS=darwin GOARCH="$$arch" $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$scratch/first/wx-$$arch" ./cmd/wx; \
	  TZ=UTC LC_ALL=C CGO_ENABLED=0 GOOS=darwin GOARCH="$$arch" $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$scratch/second/wx-$$arch" ./cmd/wx; \
	  cmp "$$scratch/first/wx-$$arch" "$$scratch/second/wx-$$arch"; \
	  $(GO) version -m "$$scratch/first/wx-$$arch"; \
	done

# internal/rpcの単体テストは同じmake ciのcoverage-check/ci-test-raceが
# ./...として実行済みなのでここでは走らせない。
# --versionの表示がldflagsの埋め込みまで通っていることを確かめる。
# 接頭辞だけの確認では、-Xのパスが変わってVersionが"undefined"のままでも気付けない。
version-check: build
	@actual="$$(./bin/wx --version)"; expected="wx version $(VERSION)-dev"; \
	test "$$actual" = "$$expected" || { echo "wx --version is '$$actual', want '$$expected'"; exit 1; }

smoke: build
	./bin/wx --help >/dev/null
	./bin/wx --version | grep -q '^wx version '
	@destination="$$(mktemp -d)"; $(MAKE) install INSTALL_DIR="$$destination"; \
	"$$destination/wx" --help >/dev/null; "$$destination/wx" --version | grep -q '^wx version '

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
	@# go-licenses v2 misclassifies Go 1.27 standard packages as modules;
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

# セキュリティ・SBOMは手動opt-inとし、自動化の再開にはユーザーの明示許可が必要。
security-local: setup-security-tools govulncheck dependency-check gosec license-check secret-check

ci:
	$(MAKE) $(CI_MAKEFLAGS) ci-checks

# 静的検査の単一の入口。ローカルのci-checksとGitHub Actionsのstaticジョブはこのtargetだけを呼ぶ。
# 検査一覧を両者へ複製すると片方だけ更新され、CIで適用漏れが起きるため、追加する検査はここへ繋ぐ。
static-check: fmt-check lint deadcode mod-tidy-check docs-check comments-check tests-check lines-check testlayout-check fuzz-check gitexec-check migrations-check workflow-check shell-check

ci-checks: static-check coverage-check ci-test-race build-darwin version-check smoke release-check

# hook本体は共通Gitディレクトリのhooks直下に置き、user側のdispatcherを維持する。
# 以下はそのhookが呼び出す契約である。pre-pushのhookは持たず、コミット時のこの検査だけをゲートとする。
hook-pre-commit:
	scripts/hook-check.sh

# 全体を10回実行するとdaemonだけで約14〜18分かかるため、45分の上限を明示する。
# nightlyジョブの90分枠内に収めつつ余裕を持たせる。
nightly-race:
	$(GO) test -race -shuffle=on -count=10 -timeout=45m ./...

fuzz:
	$(GO) test -run=^$$ -fuzz=. -fuzztime=60s ./internal/config ./internal/rpc ./internal/agent ./internal/domain ./internal/archive ./internal/pool

fault-check:
	$(GO) test -race ./internal/state ./internal/daemon -run 'Fault|FailsClosed|Damaged|Corrupt|Rollback' -count=5 -timeout=3m

crash-check:
	$(GO) test -race ./internal/daemon -run 'Crash|Reconcile|Recovery' -count=10 -timeout=10m

soak-check:
	WX_SOAK_SESSIONS=300 $(GO) test -race ./internal/daemon -run TestSessionLifecycleSoak -count=1 -timeout=15m

resource-leak-check:
	$(GO) test -race ./internal/daemon ./internal/rpc -run 'Leak|Close|Stops' -count=20 -timeout=5m

clean:
	$(GO) clean -testcache

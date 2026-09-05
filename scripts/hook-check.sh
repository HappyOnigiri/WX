#!/bin/sh
set -eu

# この script を呼ぶ pre-commit・pre-push hook は固定の時間予算を設けない。
# user-level core.hooksPath dispatcher を維持するため hook 本体は追跡せず、軽量な検査だけを行う。
# 全テストと golangci-lint は CI の責務なのでここでは実行しない。
mode=${1:-}
case "$mode" in
  pre-commit|pre-push) ;;
  *) echo "usage: hook-check.sh <pre-commit|pre-push>" >&2; exit 2 ;;
esac

root=$(git rev-parse --show-toplevel)
cd "$root"
# hook はこの repository 用の GIT_INDEX_FILE などを継承する。
# テストは独立 repository を作るため、相対 path を child Git へ渡すと worktree の見え方が壊れる。
local_git_env=$(git rev-parse --local-env-vars)
for variable in $local_git_env; do
  unset "$variable"
done
tools="$root/.tools/bin"

require_tool() {
  if [ ! -x "$1" ]; then
    echo "required pinned tool is missing: $1; run make setup" >&2
    exit 1
  fi
}

require_tool "$tools/gofumpt"
require_tool "$tools/gci"

# `make fmt-check` と同じ書式検査。変更 path に関係なく module 全体を検査し、書式崩れを通さない。
if [ -n "$("$tools/gofumpt" -l cmd internal migrations tools)" ]; then
  "$tools/gofumpt" -l cmd internal migrations tools
  echo "run make fmt" >&2
  exit 1
fi
find cmd internal tools -type f -name '*.go' -print0 \
  | xargs -0 "$tools/gci" diff -s standard -s default -s "prefix(github.com/HappyOnigiri/WX)"

if [ "$mode" = pre-push ]; then
  go vet ./...
  mkdir -p bin
  go build -trimpath -o bin/wx ./cmd/wx
fi

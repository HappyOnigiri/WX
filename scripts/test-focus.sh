#!/bin/sh
set -eu

# make test-focus から export された PKG・RUN・VERBOSE・GO だけを読み、対象を絞った go test を1回実行する。
# 値を shell のソースへ埋め込まず、引用した引数として渡す。-short や -race は暗黙に足さない。
go_command=${GO:-go}
package=${PKG:-}
run=${RUN:-}
verbose=${VERBOSE:-}

usage() {
  cat >&2 <<'USAGE'
usage: make test-focus PKG=<package> [RUN=<regexp>] [VERBOSE=1]
  PKG は単一の相対パッケージパス（例: ./internal/daemon）。モジュール全体は ./... と明示したときだけ許可する。
  RUN は go test -run へそのまま渡す正規表現。省略すると PKG のテストすべてを実行する。
  VERBOSE を空でない値にすると go test -v を足し、個別のRUN/PASS/SKIPとその理由を残す。
example: make test-focus PKG=./internal/daemon RUN=TestLeaseArchiveAndRestorePreservesGitState
USAGE
  exit 2
}

if [ -z "$package" ]; then
  echo "test-focus: PKG is required" >&2
  usage
fi

# 複数指定・絶対パス・部分的なワイルドカードは、意図より広い実行を招くため受け付けない。
case "$package" in
  ./...) ;;
  *[[:space:]]*) echo "test-focus: PKG takes a single package path, got: $package" >&2; usage ;;
  *...*) echo "test-focus: wildcards are allowed only as the explicit whole-module ./..., got: $package" >&2; usage ;;
  ./?*) ;;
  *) echo "test-focus: PKG must be a relative package path starting with ./, got: $package" >&2; usage ;;
esac

set -- "$go_command" test -count=1 -shuffle=on
if [ -n "$verbose" ]; then
  set -- "$@" -v
fi
if [ -n "$run" ]; then
  set -- "$@" -run "$run"
fi
set -- "$@" "$package"

# 表示は実際の引数から組み立て、0件実行が go の出力で分かるよう標準出力は抑制しない。
echo "test-focus: $*" >&2

status=0
"$@" || status=$?

echo "test-focus: partial verification only; the final gate is make ci" >&2
exit "$status"

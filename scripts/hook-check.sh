#!/bin/sh
set -eu

# pre-commit hook の実体。固定の時間予算は設けず `make ci` をそのまま実行する。
# 検査の一部だけを hook に写すと ci との差分が育ち、通ったつもりの変更が CI で落ちるため。
# user-level core.hooksPath dispatcher を維持するため hook 本体は追跡せず、この script を契約とする。
# 同じ内容を再びコミットするときは直前の成功をキャッシュから再利用する。FORCE_CI=1 で無視できる。

root=$(git rev-parse --show-toplevel)
# hook は相対 path の GIT_DIR を継承し得るので、cd する前に絶対 path へ解決する。
git_dir=$(git rev-parse --absolute-git-dir)
cd "$root"

# hook はこの repository 用の GIT_INDEX_FILE などを継承する。
# テストは独立 repository を作るため、相対 path を child Git へ渡すと worktree の見え方が壊れる。
local_git_env=$(git rev-parse --local-env-vars)
for variable in $local_git_env; do
  unset "$variable"
done

# キャッシュキーは HEAD ではなく作業ツリーの内容から作る。
# コミット対象に入らない変更も `make ci` の結果を変えるため、tree hash に含める。
# 一時 index を使い、hook が触ってはならない本物の index を保つ。
cache_file="$git_dir/hook-make-ci-success"
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/wx-hook-ci.XXXXXX")
tree_hash=$(
  GIT_INDEX_FILE="$temp_dir/index" git read-tree HEAD &&
    GIT_INDEX_FILE="$temp_dir/index" git add -A -- . &&
    GIT_INDEX_FILE="$temp_dir/index" git write-tree
) || tree_hash=
rm -rf "$temp_dir"

if [ -z "$tree_hash" ]; then
  echo "warning: make ci のキャッシュキーを作れなかったので検査を実行する" >&2
else
  cache_key="v1:$tree_hash"
  if [ "${FORCE_CI:-0}" != "1" ] && [ -f "$cache_file" ] &&
    [ "$(cat "$cache_file")" = "$cache_key" ]; then
    echo "skipping make ci: 前回成功したときから作業ツリーが変わっていない"
    exit 0
  fi
fi

echo "running make ci..."
if ! make ci; then
  echo "error: make ci が失敗したのでコミットを中断する" >&2
  exit 1
fi

if [ -n "$tree_hash" ] && ! printf '%s\n' "$cache_key" >"$cache_file"; then
  echo "warning: make ci の成功をキャッシュできなかった" >&2
fi

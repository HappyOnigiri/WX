#!/bin/sh
set -eu

# scripts/hooks/ の内容をこのリポジトリのhooksディレクトリへ複製する。
# 宛先は共通Gitディレクトリ（--git-common-dir）で、worktreeから実行しても
# 全worktreeが共有する1か所へ入る。
# core.hooksPathは設定しない。userレベルのdispatcherを覆い隠さないためである。
root=$(git rev-parse --show-toplevel)
hooks_dir=$(git rev-parse --path-format=absolute --git-common-dir)/hooks

mkdir -p "$hooks_dir"

for source in "$root"/scripts/hooks/*; do
  name=$(basename "$source")
  destination="$hooks_dir/$name"
  if cmp -s "$source" "$destination"; then
    echo "unchanged: $destination"
    continue
  fi
  cp "$source" "$destination"
  chmod 755 "$destination"
  echo "installed: $destination"
done

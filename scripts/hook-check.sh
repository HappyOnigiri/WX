#!/bin/sh
set -eu

# pre-commit hook の差分選択を tools/hookcheck へ委譲する。
# root・git directory・使用中のindexはGit環境を解除する前に解決する。
mode=""
if [ "$#" -gt 0 ]; then
  if [ "$1" = "--plan" ] && [ "$#" -eq 1 ]; then
    mode="--plan"
  else
    echo "usage: scripts/hook-check.sh [--plan]" >&2
    exit 2
  fi
fi

root=$(git rev-parse --show-toplevel)
git_dir=$(git rev-parse --path-format=absolute --git-dir)
index=$(git rev-parse --path-format=absolute --git-path index)
force_ci=${FORCE_CI:-0}

cd "$root"
local_git_env=$(git rev-parse --local-env-vars)
for variable in $local_git_env; do
  unset "$variable"
done

if [ "$force_ci" = "1" ] && [ -z "$mode" ]; then
  exec make ci
fi

if [ -n "$mode" ]; then
  exec go run ./tools/hookcheck --root "$root" --git-dir "$git_dir" --index "$index" "$mode"
fi
exec go run ./tools/hookcheck --root "$root" --git-dir "$git_dir" --index "$index"

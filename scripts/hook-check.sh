#!/bin/sh
set -eu

# The hooks that invoke this script (the pre-commit and pre-push under
# $(git rev-parse --git-common-dir)/hooks, via `make hook-pre-commit` and
# `make hook-pre-push`) do not enforce a fixed time budget. They are not
# tracked here, so that a user-level core.hooksPath dispatcher stays in
# effect. Keep this script's own checks fast as a
# guideline (aim to stay well under 30 seconds), but do not narrow what it
# checks in order to chase a budget. The full test suite and golangci-lint are
# deliberately not run here: they are CI's job, not the hook's.
mode=${1:-}
case "$mode" in
  pre-commit|pre-push) ;;
  *) echo "usage: hook-check.sh <pre-commit|pre-push>" >&2; exit 2 ;;
esac

root=$(git rev-parse --show-toplevel)
cd "$root"
# Hooks inherit GIT_INDEX_FILE and related variables for this repository. Tests
# create independent repositories, so carrying those relative paths into child
# Git processes corrupts their worktree view.
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

# fmt-check, matching `make fmt-check`: every commit and push runs this over
# the whole module regardless of which paths changed, so formatting drift
# never slips past the hook.
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

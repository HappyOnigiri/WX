#!/bin/sh
set -eu

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
npm_tools="$root/.tools/npm/node_modules/.bin"

require_tool() {
  if [ ! -x "$1" ]; then
    echo "required pinned tool is missing: $1; run make setup" >&2
    exit 1
  fi
}

require_tool "$tools/gofumpt"
require_tool "$tools/gci"

if [ "$mode" = pre-commit ]; then
  paths=$(git diff --cached --name-only --diff-filter=ACMR)
else
  base=origin/main
  while read -r _local_ref _local_sha _remote_ref remote_sha; do
    case "$remote_sha" in
      ""|0000000000000000000000000000000000000000) ;;
      *) base=$remote_sha; break ;;
    esac
  done
  if ! git rev-parse --verify "$base^{commit}" >/dev/null 2>&1; then
    base=HEAD~1
  fi
  paths=$(git diff --name-only --diff-filter=ACMR "$base"...HEAD)
fi

if [ -z "$paths" ]; then
  exit 0
fi

go_changed=false
module_changed=false
docs_changed=false
workflow_changed=false
shell_changed=false

for path in $paths; do
  case "$path" in
    *.go)
      go_changed=true
      if [ -f "$path" ]; then
        if [ -n "$("$tools/gofumpt" -l "$path")" ]; then
          echo "$path is not gofumpt-formatted; run make fmt" >&2
          exit 1
        fi
        # pre-push receives ref updates on stdin. gci treats any piped stdin as
        # another Go source, so isolate it after the ref update has been read.
        "$tools/gci" diff -s standard -s default -s "prefix(github.com/HappyOnigiri/WX)" "$path" </dev/null
      fi
      ;;
    go.mod|go.sum) module_changed=true ;;
    *.md) docs_changed=true ;;
    .github/workflows/*|.github/actions/*) workflow_changed=true ;;
    *.sh|.githooks/*) shell_changed=true ;;
  esac
done

if [ "$module_changed" = true ]; then
  go mod tidy -diff
  go mod verify
fi
if [ "$docs_changed" = true ]; then
  require_tool "$npm_tools/markdownlint-cli2"
  "$npm_tools/markdownlint-cli2" README.md '**/*.md' '#.tools/**'
fi
if [ "$workflow_changed" = true ]; then
  require_tool "$tools/actionlint"
  require_tool "$tools/zizmor"
  "$tools/actionlint"
  "$tools/zizmor" --pedantic --min-severity medium .github
fi
if [ "$shell_changed" = true ]; then
  command -v shellcheck >/dev/null 2>&1 || { echo "ShellCheck is missing; run make setup" >&2; exit 1; }
  shellcheck .githooks/pre-commit .githooks/pre-push scripts/*.sh
fi
if [ "$go_changed" = true ]; then
  if [ "$mode" = pre-push ]; then
    require_tool "$tools/golangci-lint"
    go test -short -shuffle=on -count=1 ./... &
    test_pid=$!
    "$tools/golangci-lint" run ./... &
    lint_pid=$!
    status=0
    wait "$test_pid" || status=$?
    wait "$lint_pid" || status=$?
    exit "$status"
  fi
  go test -short -shuffle=on -count=1 ./...
fi

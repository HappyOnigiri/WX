#!/bin/sh
set -eu

package=${1:?package is required}
result=${2:?Gremlins result JSON is required}

case "$package" in
  ./internal/domain | ./internal/workspace | ./internal/archive | ./internal/daemon)
    if grep -q '"status":"LIVED"' "$result"; then
      printf 'surviving ownership/path mutation in %s\n' "$package" >&2
      exit 1
    fi
    ;;
esac

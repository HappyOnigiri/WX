#!/bin/sh
set -eu

package=${1:?package is required}
result=${2:?Gremlins result JSON is required}
minimum=${3:?minimum efficacy is required}

if [ ! -f "$result" ]; then
  printf 'mutation result is missing for %s\n' "$package" >&2
  exit 1
fi

efficacy=$(sed -n 's/.*"test_efficacy":\([0-9.]*\).*/\1/p' "$result")
if [ -z "$efficacy" ] || ! awk -v actual="$efficacy" -v required="$minimum" 'BEGIN { exit !(actual >= required) }'; then
  printf 'mutation efficacy for %s is %s%%; required %s%%\n' "$package" "${efficacy:-unknown}" "$minimum" >&2
  exit 1
fi

case "$package" in
  ./internal/state | ./internal/workspace | ./internal/archive)
    if grep -q '"status":"LIVED"' "$result"; then
      printf 'surviving ownership/path mutation in %s\n' "$package" >&2
      exit 1
    fi
    ;;
esac

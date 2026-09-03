#!/bin/sh
set -eu

root=$(git rev-parse --show-toplevel)

for hook in "$root/.githooks/pre-commit" "$root/.githooks/pre-push"; do
  if [ ! -x "$hook" ]; then
    echo "$hook is not executable" >&2
    exit 1
  fi
done

configured=$(git config --local --get core.hooksPath || true)
if [ "$configured" != ".githooks" ]; then
  echo "core.hooksPath is not .githooks; run make hooks-install" >&2
  exit 1
fi

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin"
log="$scratch/make.log"

# Stand in for `make` so we can assert which target each hook drives, without
# paying for a real hook-check.sh run (that is exercised by hooks-install plus
# an actual commit/push, not by this script).
stub_make="$scratch/bin/make"
cat >"$stub_make" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$WX_TEST_MAKE_LOG"
exit "${WX_TEST_MAKE_EXIT:-0}"
EOF
chmod 700 "$stub_make"

check_hook_drives_target() {
  hook_name=$1
  expected_target=$2

  : >"$log"
  if ! WX_TEST_MAKE_LOG="$log" WX_TEST_MAKE_EXIT=0 PATH="$scratch/bin:$PATH" "$root/.githooks/$hook_name" </dev/null; then
    echo "$hook_name exited nonzero even though its check succeeded" >&2
    exit 1
  fi
  invocation=$(cat "$log")
  if [ "$invocation" != "-C $root $expected_target" ]; then
    echo "$hook_name invoked make with unexpected arguments: $invocation" >&2
    exit 1
  fi

  : >"$log"
  if WX_TEST_MAKE_LOG="$log" WX_TEST_MAKE_EXIT=1 PATH="$scratch/bin:$PATH" "$root/.githooks/$hook_name" </dev/null; then
    echo "$hook_name exited zero even though its check failed" >&2
    exit 1
  fi
}

# Each hook must dispatch to its matching hook-check.sh target (regression
# guard for accidental target swaps) and must propagate that target's exit
# status, since that propagation is what actually blocks a bad commit/push.
check_hook_drives_target pre-commit hook-pre-commit
check_hook_drives_target pre-push hook-pre-push

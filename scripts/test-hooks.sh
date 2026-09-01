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
child="$scratch/child.sh"
pid_file="$scratch/pid"
cat >"$child" <<'EOF'
#!/bin/sh
sleep 60 &
echo $! >"$WX_HOOK_PID_FILE"
wait
EOF
chmod 700 "$child"
if WX_HOOK_PID_FILE="$pid_file" "$root/scripts/run-with-timeout.sh" 1 "$child"; then
  echo "timeout helper accepted an over-budget hook" >&2
  exit 1
fi
if [ -s "$pid_file" ]; then
  child_pid=$(sed -n '1p' "$pid_file")
  attempts=0
  while kill -0 "$child_pid" 2>/dev/null && [ "$attempts" -lt 20 ]; do
    attempts=$((attempts + 1))
    sleep 0.1
  done
  if kill -0 "$child_pid" 2>/dev/null; then
    echo "timeout helper left a child process running" >&2
    exit 1
  fi
fi

#!/bin/bash
set -euo pipefail

fail() {
  echo "wx uninstall: $*" >&2
  exit 1
}

# uninstall.sh は削除の順序だけを持ち、削除そのものは wx の既存コマンドへ委ねる。
# wx clear は daemon 越しに動き、wx setup --remove は LaunchAgent の解除で daemon を落とす。
# この順序を崩すと worktree と git worktree 登録がソースリポジトリに残る。
# curl | bash の途中切断では削除を始めないよう、全体を読み込んでから呼ぶ。
main() {
  [ "$(uname -s)" = Darwin ] || fail "wx only runs on macOS"
  [[ "${HOME:-}" = /* ]] || fail "HOME must be an absolute path"

  local assume_yes=false
  case "${1:-}" in
    -y | --yes) assume_yes=true ;;
    '') ;;
    *) fail "unknown option $1; the only option is --yes" ;;
  esac

  local installed="$HOME/.local/bin/wx" wx
  wx=$(command -v wx || true)
  [ -n "$wx" ] || wx="$installed"
  [ -x "$wx" ] || fail "wx was not found on PATH or at $installed; delete what is left by hand"

  local scratch
  scratch=$(mktemp -d "${TMPDIR:-/tmp}/wx-uninstall.XXXXXX")
  trap 'rm -rf "$scratch"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # 破棄の対象は先に見せる。dry-run は daemon 越しなので、応答しないときは worktree の掃除を飛ばす。
  local can_reach_daemon=true
  if ! "$wx" clear --all --discard --dry-run; then
    can_reach_daemon=false
    echo 'wx uninstall: the daemon did not answer, so wx cannot delete its worktrees now.' >&2
    echo 'wx uninstall: start it with wx daemon start and rerun this script to clean them up.' >&2
  fi

  if [ "$assume_yes" != true ]; then
    local answer=''
    echo ''
    echo 'This deletes the worktrees listed above without saving unfinished work, then'
    echo 'removes the wx hook entries, LaunchAgent, configuration, and binary.'
    printf 'Continue? [y/N] '
    # curl | bash では stdin がパイプに置き換わるため、制御端末から読む。
    [ -r /dev/tty ] || fail "no terminal to confirm on; rerun with --yes to skip the question"
    read -r answer < /dev/tty || answer=''
    case "$answer" in
      y | Y | yes | YES) ;;
      *) fail "cancelled; nothing was changed" ;;
    esac
  fi

  if [ "$can_reach_daemon" = true ]; then
    "$wx" clear --all --discard ||
      fail "wx clear did not finish; nothing else was removed, so rerun this script"
    # LaunchAgent 無しで起動された daemon もここで終わらせる。bootout は登録済みの service しか止められない。
    "$wx" daemon stop || echo 'wx uninstall: the daemon did not stop cleanly; continuing' >&2
  fi

  # 消さなかった path は wx が leftover 行で報告する。config.yaml が消えた後は worktree root を引けないため、ここで受け取る。
  local removal_failed=false
  "$wx" setup --remove > "$scratch/removal" || removal_failed=true
  cat "$scratch/removal"

  local leftovers=() tag rest
  while IFS=' ' read -r tag rest; do
    if [ "$tag" = leftover ]; then
      leftovers+=("$rest")
    fi
  done < "$scratch/removal"

  # 開発ビルドを消さないよう、削除するのは install.sh が置く path だけにする。
  if [ -e "$installed" ]; then
    rm -f "$installed"
    echo "Removed $installed"
  fi
  if [ "$wx" != "$installed" ] && [ -e "$wx" ]; then
    echo "wx uninstall: $wx was not installed by install.sh and was left in place; delete it yourself"
  fi

  echo ''
  echo 'Remove the wx PATH line from your shell configuration (for example, ~/.zshrc):'
  # 利用者が探す行をそのまま見せる。
  # shellcheck disable=SC2016
  echo '  export PATH="$HOME/.local/bin:$PATH"'
  if [ ${#leftovers[@]} -gt 0 ]; then
    local path
    echo ''
    echo 'wx kept these because they hold saved work and records. Delete them once you no longer need them:'
    for path in "${leftovers[@]}"; do
      printf '  rm -rf %q\n' "$path"
    done
  fi
  echo ''
  echo 'Repositories you ran wx from may still hold recovery refs for the snapshots wx took.'
  echo 'To list and then delete them, run these from inside such a repository:'
  echo '  git for-each-ref --format="%(refname)" refs/wx/recovery'
  echo '  git for-each-ref --format="delete %(refname)" refs/wx/recovery | git update-ref --stdin'

  [ "$removal_failed" != true ] || fail "some configuration could not be removed; see the errors above"
}

main "$@"

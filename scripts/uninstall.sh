#!/bin/bash
set -euo pipefail

language=en
msg() {
  local key=${1:-}
  case "$language:$key" in
    ja:daemon_unavailable) printf '%s' 'wx uninstall: daemon が応答しないため、worktree を今は削除できません。' ;;
    en:daemon_unavailable) printf '%s' 'wx uninstall: the daemon did not answer, so wx cannot delete its worktrees now.' ;;
    ja:daemon_hint) printf '%s' 'wx uninstall: wx daemon start を実行してから、この script を再実行してください。' ;;
    en:daemon_hint) printf '%s' 'wx uninstall: start it with wx daemon start and rerun this script to clean them up.' ;;
    ja:daemon_stop_failed) printf '%s' 'wx uninstall: daemon を正常に停止できませんでしたが、処理を続けます。' ;;
    en:daemon_stop_failed) printf '%s' 'wx uninstall: the daemon did not stop cleanly; continuing' ;;
    ja:confirm) printf '%s' 'Continue? / 続行しますか？ [y/N] ' ;;
    en:confirm) printf '%s' 'Continue? [y/N] ' ;;
    ja:summary) printf '%s' 'これは未保存の作業を保存せずに上記 worktree から削除し、wx の hook、LaunchAgent、設定、binary を削除します。' ;;
    en:summary) printf '%s' 'This deletes the worktrees listed above without saving unfinished work, then removes the wx hook entries, LaunchAgent, configuration, and binary.' ;;
    ja:path_hint) printf '%s' 'shell 設定（例: ~/.zshrc）から wx の PATH 行を削除してください:' ;;
    en:path_hint) printf '%s' 'Remove the wx PATH line from your shell configuration (for example, ~/.zshrc):' ;;
    ja:leftovers) printf '%s' '保存済みの作業と記録を含むため、wx は次を残しました。不要になったら削除してください:' ;;
    en:leftovers) printf '%s' 'wx kept these because they hold saved work and records. Delete them once you no longer need them:' ;;
    ja:refs) printf '%s' 'wx が保存した snapshot の復旧 ref が repository に残っている場合があります。' ;;
    en:refs) printf '%s' 'Repositories you ran wx from may still hold recovery refs for the snapshots wx took.' ;;
    ja:refs_hint) printf '%s' '一覧表示と削除は、対象 repository 内で次を実行してください:' ;;
    en:refs_hint) printf '%s' 'To list and then delete them, run these from inside such a repository:' ;;
    ja:removed) printf 'wx を %s から削除しました' "$2" ;;
    en:removed) printf 'Removed %s' "$2" ;;
    ja:external_left) printf 'wx uninstall: install.sh が置いたものではない %s は残しました。手動で削除してください' "$2" ;;
    en:external_left) printf 'wx uninstall: %s was not installed by install.sh and was left in place; delete it yourself' "$2" ;;
    ja:no_terminal) printf '%s' '確認用の端末がありません。質問を省くには --yes で再実行してください' ;;
    en:no_terminal) printf '%s' 'no terminal to confirm on; rerun with --yes to skip the question' ;;
    ja:cancelled) printf '%s' 'キャンセルしました。何も変更していません' ;;
    en:cancelled) printf '%s' 'cancelled; nothing was changed' ;;
    ja:clear_failed) printf '%s' 'wx clear が完了しませんでした。ほかは削除していないため、この script を再実行してください' ;;
    en:clear_failed) printf '%s' 'wx clear did not finish; nothing else was removed, so rerun this script' ;;
    ja:remove_failed) printf '%s' '一部の設定を削除できませんでした。上のエラーを確認してください' ;;
    en:remove_failed) printf '%s' 'some configuration could not be removed; see the errors above' ;;
    *) printf '%s' "$key" ;;
  esac
}

fail() {
  local detail="$*"
  case "$detail" in
    'no terminal to confirm on; rerun with --yes to skip the question') detail=$(msg no_terminal) ;;
    'cancelled; nothing was changed') detail=$(msg cancelled) ;;
    'wx clear did not finish; nothing else was removed, so rerun this script') detail=$(msg clear_failed) ;;
    'some configuration could not be removed; see the errors above') detail=$(msg remove_failed) ;;
  esac
  echo "wx uninstall: $detail" >&2
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

  # 設定削除の前に wx の実効言語を保存する。旧 binary や壊れた設定では YAML の
  # 明示値を補助的に読み、どちらも読めない場合は英語へ戻す。
  local saved_language=''
  saved_language=$("$wx" config language 2>/dev/null || true)
  if [ "$saved_language" != en ] && [ "$saved_language" != ja ]; then
    local config_path="$HOME/.config/wx/config.yaml"
    if [ -r "$config_path" ]; then
      saved_language=$(awk '$1 == "language:" {print $2; exit}' "$config_path" 2>/dev/null || true)
    fi
  fi
  case "$saved_language" in
    en|ja) language="$saved_language" ;;
    *) language=en ;;
  esac

  local scratch
  scratch=$(mktemp -d "${TMPDIR:-/tmp}/wx-uninstall.XXXXXX")
  trap 'rm -rf "$scratch"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # 破棄の対象は先に見せる。dry-run は daemon 越しなので、応答しないときは worktree の掃除を飛ばす。
  local can_reach_daemon=true
  if ! "$wx" clear --all --discard --dry-run; then
    can_reach_daemon=false
    msg daemon_unavailable >&2
    printf '\n' >&2
    msg daemon_hint >&2
  fi

  if [ "$assume_yes" != true ]; then
    local answer=''
    echo ''
    msg summary
    printf '\n'
    msg confirm
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
    "$wx" daemon stop || msg daemon_stop_failed >&2
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
    msg removed "$installed"
  fi
  if [ "$wx" != "$installed" ] && [ -e "$wx" ]; then
    msg external_left "$wx"
  fi

  echo ''
  msg path_hint
  # 利用者が探す行をそのまま見せる。
  # shellcheck disable=SC2016
  echo '  export PATH="$HOME/.local/bin:$PATH"'
  if [ ${#leftovers[@]} -gt 0 ]; then
    local path
    echo ''
    msg leftovers
    for path in "${leftovers[@]}"; do
      printf '  rm -rf %q\n' "$path"
    done
  fi
  echo ''
  msg refs
  msg refs_hint
  echo '  git for-each-ref --format="%(refname)" refs/wx/recovery'
  echo '  git for-each-ref --format="delete %(refname)" refs/wx/recovery | git update-ref --stdin'

  [ "$removal_failed" != true ] || fail "some configuration could not be removed; see the errors above"
}

main "$@"

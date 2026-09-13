#!/bin/bash
set -euo pipefail

# Release CI がこの値を埋め込み、インストーラーとバイナリを同じタグへ固定する。
release_version='@WX_RELEASE_VERSION@'

# 初回の shell 出力はバイナリを取得する前でも読めるよう、macOS 標準の
# 小さな keyed catalog だけを使う。未知の文は英語へ戻す。
language=en
msg() {
  local key=${1:-}
  case "$language:$key" in
    ja:choose_language) printf '%s' 'Display language / 表示言語 [English/日本語] (既定: English): ' ;;
    en:choose_language) printf '%s' 'Display language [English/Japanese] (default: English): ' ;;
    ja:downloading) printf 'wx %s をダウンロード中...\n' "$2" ;;
    en:downloading) printf 'Downloading wx %s...\n' "$2" ;;
    ja:installed) printf 'wx %s を %s にインストールしました\n' "$2" "$3" ;;
    en:installed) printf 'Installed wx %s to %s\n' "$2" "$3" ;;
    ja:finish) printf '%s\n' 'この端末で wx を使うには:' ;;
    en:finish) printf '%s\n' 'To use wx in this terminal, run:' ;;
    ja:path_hint) printf '%s\n' '新しい terminal では shell 設定（例: ~/.zshrc）にもこの行を追加してください。' ;;
    en:path_hint) printf '%s\n' 'Add that line to your shell configuration (for example, ~/.zshrc) for new terminals.' ;;
    ja:setup_hint) printf '%s\n' 'wx setup を実行すると agent hook を登録し、残りの設定を確認します。' ;;
    en:setup_hint) printf '%s\n' 'To finish the setup, run wx setup. It registers the agent hooks and reviews the rest.' ;;
    ja:run_hint) printf '%s\n' 'その後、repository で wx claude または wx codex を実行してください。' ;;
    en:run_hint) printf '%s\n' 'Then run wx claude or wx codex from your repository.' ;;
    *) printf '%s' "$key" ;;
  esac
}

fail() {
  echo "wx install: $*" >&2
  exit 1
}

# curl | bash の途中切断では配置処理を始めないよう、全体を読み込んでから呼ぶ。
main() {
  [[ "$release_version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
    fail "use install.sh from a GitHub Release"
  [ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] ||
    fail "macOS arm64 is required; use a native Apple Silicon terminal"
  [[ "${HOME:-}" = /* ]] || fail "HOME must be an absolute path"
  for tool in curl shasum mktemp install mv plutil launchctl id git; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
  done
  git --version >/dev/null 2>&1 || fail "install Git before installing wx"

  local install_dir="$HOME/.local/bin" destination="$HOME/.local/bin/wx"
  local asset=wx-darwin-arm64
  local base_url="https://github.com/HappyOnigiri/WX/releases/download/$release_version"
  local checksum checksum_name actual registered_binary='' needs_install=true
  local plist="$HOME/Library/LaunchAgents/com.user.wx.plist"
  [ ! -d "$destination" ] || fail "$destination is a directory"

  # 既存 binary の設定を最優先し、update で言語を再質問しない。取得できない
  # 古い binary は新規扱いとして TTY で確認し、非対話なら英語を採用する。
  local existing_language=''
  if [ -x "$destination" ]; then
    existing_language=$("$destination" config language 2>/dev/null || true)
    case "$existing_language" in
      en|ja) language="$existing_language" ;;
      *)
        existing_language=''
        # 旧 binary が config language をまだ提供しなくても、既存の global
        # 設定を読めれば update で質問を出さず、その値を新 binary へ引き継ぐ。
        local config_path="$HOME/.config/wx/config.yaml"
        if [ -r "$config_path" ]; then
          existing_language=$(awk '$1 == "language:" {print $2; exit}' "$config_path" 2>/dev/null || true)
          case "$existing_language" in
            en|ja) language="$existing_language" ;;
            *) existing_language='' ;;
          esac
        fi
        ;;
    esac
  fi
  if [ -z "$existing_language" ] && [ -r /dev/tty ] && exec 3<>/dev/tty; then
    if [ -t 3 ]; then
      answer=''
      printf '%s' "$(msg choose_language)" >&3
      IFS= read -r answer <&3 || answer=''
      case "$answer" in
        ja|JA|j|J|日本語) language=ja ;;
        en|EN|e|E|English|'') language=en ;;
        *) language=en ;;
      esac
      printf '\n' >&3
    fi
    exec 3>&-
  fi

  scratch=$(mktemp -d "${TMPDIR:-/tmp}/wx-install.XXXXXX")
  staged=''
  trap 'rm -rf "$scratch"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  msg downloading "$release_version"
  curl --fail --silent --show-error --location "$base_url/$asset" --output "$scratch/$asset" ||
    fail "download failed; the installed binary was not changed"
  curl --fail --silent --show-error --location "$base_url/checksums.txt" --output "$scratch/checksums.txt" ||
    fail "checksum download failed; the installed binary was not changed"
  read -r checksum checksum_name < "$scratch/checksums.txt" || fail "invalid checksums.txt"
  [[ "$checksum" =~ ^[0-9a-fA-F]{64}$ ]] && [ "$checksum_name" = "$asset" ] ||
    fail "invalid checksum entry for $asset"
  # 検証する名前を固定し、checksums.txt の別行や相対パスを shasum に渡さない。
  (cd "$scratch" && printf '%s  %s\n' "$checksum" "$asset" | shasum -a 256 -c -) ||
    fail "checksum verification failed; the installed binary was not changed"
  chmod 0755 "$scratch/$asset"
  actual=$("$scratch/$asset" --version) || fail "the downloaded binary could not run"
  [ "$actual" = "wx version $release_version" ] || fail "unexpected binary version: $actual"

  if [ -f "$plist" ]; then
    registered_binary=$(plutil -extract ProgramArguments.0 raw -o - "$plist" 2>/dev/null) || registered_binary=''
  fi
  if [ "$registered_binary" = "$destination" ] && launchctl print "gui/$(id -u)/com.user.wx" >/dev/null 2>&1; then
    needs_install=false
  fi
  # daemon install は bootout するため、初回・移行時も既存 daemon の正常停止を先に確認する。
  if [ "$needs_install" = true ]; then
    "$scratch/$asset" daemon stop || fail "daemon stop failed; the installed binary was not changed; retry after wx daemon stop succeeds"
  fi

  install -d "$install_dir"
  staged=$(mktemp "$install_dir/.wx-install.XXXXXX")
  install -m 0755 "$scratch/$asset" "$staged"
  mv -f "$staged" "$destination"
  staged=''
  msg installed "$release_version" "$destination"

  # 検証済み binary の config command で選択値を atomic save する。開発用の
  # fixture binary などが command を持たない場合だけ互換のため続行する。
  PATH="$install_dir:$PATH" "$destination" config language "$language" >/dev/null 2>&1 || true

  # ResolveBinary は PATH 上の wx を優先するため、別のインストール先を登録させない。
  if [ "$needs_install" = true ]; then
    if ! PATH="$install_dir:$PATH" "$destination" daemon install; then
      echo 'Add ~/.local/bin to PATH, then run wx daemon install.' >&2
      fail "binary installed, but LaunchAgent registration failed"
    fi
    if ! PATH="$install_dir:$PATH" "$destination" daemon start; then
      echo 'Add ~/.local/bin to PATH, then run wx daemon start.' >&2
      fail "binary installed, but daemon startup failed"
    fi
  elif ! PATH="$install_dir:$PATH" "$destination" daemon restart; then
    echo 'Add ~/.local/bin to PATH, then run wx daemon restart.' >&2
    fail "binary installed, but daemon restart did not complete"
  fi

  # 新規か更新かの判定は wx 側の状態に寄せる。--update は対応が要る項目だけを提示し、
  # 何もなければ無出力で 0 を返す。set -euo pipefail で install 全体を落とさないよう終了コードは吸収する。
  PATH="$install_dir:$PATH" "$destination" setup --update || true

  msg finish
  # 利用者が実行するコマンドを展開せず表示する。
  # shellcheck disable=SC2016
  echo '  export PATH="$HOME/.local/bin:$PATH"'
  msg path_hint
  msg setup_hint
  msg run_hint
}

main "$@"

#!/bin/bash
set -euo pipefail

# Release CI がこの値を埋め込み、インストーラーとバイナリを同じタグへ固定する。
release_version='@WX_RELEASE_VERSION@'

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

  scratch=$(mktemp -d "${TMPDIR:-/tmp}/wx-install.XXXXXX")
  staged=''
  trap 'rm -rf "$scratch"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  echo "Downloading wx $release_version..."
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
  echo "Installed wx $release_version to $destination"

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

  echo 'To use wx in this terminal, run:'
  # 利用者が実行するコマンドを展開せず表示する。
  # shellcheck disable=SC2016
  echo '  export PATH="$HOME/.local/bin:$PATH"'
  echo 'Add that line to your shell configuration (for example, ~/.zshrc) for new terminals.'
  echo 'To finish the setup, run wx setup. It registers the agent hooks and reviews the rest.'
  echo 'Then run wx claude or wx codex from your repository.'
}

main "$@"
